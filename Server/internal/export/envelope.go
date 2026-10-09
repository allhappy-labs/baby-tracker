package export

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const EnvelopeChunkBytes = 65_536
const MaxArchiveCiphertextBytes = 53_477_376
const maxEnvelopeHeaderBytes = 4096

var ErrEnvelopeIntegrity = errors.New("export_integrity")

type HeaderCore struct {
	Version            string `json:"version"`
	ExportID           string `json:"export_id"`
	InstanceID         string `json:"instance_id"`
	FamilyID           string `json:"family_id"`
	RequesterAccountID string `json:"requester_account_id"`
	CreatedAt          string `json:"created_at"`
	ExpiresAt          string `json:"expires_at"`
	KeyID              string `json:"key_id"`
	NoncePrefix        string `json:"nonce_prefix"`
	PlaintextBytes     uint64 `json:"plaintext_bytes"`
	PlaintextSHA256    string `json:"plaintext_sha256"`
	ChunkCount         uint64 `json:"chunk_count"`
}
type EnvelopeHeader struct {
	Core       HeaderCore `json:"core"`
	WrapNonce  string     `json:"wrap_nonce"`
	WrappedKey string     `json:"wrapped_key"`
}
type Chunk struct {
	Index           uint64
	PlaintextLength uint32
	Final           bool
	Ciphertext      []byte
}
type ChunkSink interface {
	Put(context.Context, Chunk) error
}
type ChunkSource interface {
	Each(context.Context, func(Chunk) error) error
}

func exportAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("%w: key length", ErrEnvelopeIntegrity)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func randomBytes(n int) ([]byte, error) { b := make([]byte, n); _, err := rand.Read(b); return b, err }
func erase(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
func chunkNonce(prefix []byte, index uint64) []byte {
	n := make([]byte, 12)
	copy(n, prefix)
	binary.BigEndian.PutUint64(n[4:], index)
	return n
}
func chunkAAD(id []byte, index uint64, length uint32, final bool) []byte {
	aad := append([]byte("baby-tracker-export-chunk-v1\x00"), id...)
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], index)
	aad = append(aad, number[:]...)
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], length)
	aad = append(aad, size[:]...)
	if final {
		return append(aad, 1)
	}
	return append(aad, 0)
}
func exportIDBytes(value string) ([]byte, error) {
	raw, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	if err != nil || len(raw) != 16 {
		return nil, fmt.Errorf("%w: export id", ErrEnvelopeIntegrity)
	}
	return raw, nil
}
func coreAAD(core HeaderCore) ([]byte, error) {
	raw, err := json.Marshal(core)
	if err != nil {
		return nil, err
	}
	return append([]byte("baby-tracker-export-key-v1\x00"), raw...), nil
}

// SealArchive encrypts one attempt. A fresh random data key and nonce prefix
// are generated for every call; the caller must discard staged chunks on error.
func SealArchive(ctx context.Context, core HeaderCore, kek []byte, plaintext io.Reader, sink ChunkSink) (EnvelopeHeader, error) {
	var empty EnvelopeHeader
	if _, err := exportIDBytes(core.ExportID); err != nil {
		return empty, err
	}
	wrapAEAD, err := exportAEAD(kek)
	if err != nil {
		return empty, err
	}
	dataKey, err := randomBytes(32)
	if err != nil {
		return empty, err
	}
	defer erase(dataKey)
	dataAEAD, err := exportAEAD(dataKey)
	if err != nil {
		return empty, err
	}
	prefix, err := randomBytes(4)
	if err != nil {
		return empty, err
	}
	id, _ := exportIDBytes(core.ExportID)
	core.Version = "v1"
	core.NoncePrefix = base64.RawURLEncoding.EncodeToString(prefix)
	digest := sha256.New()
	buf := make([]byte, EnvelopeChunkBytes)
	var index, total, ciphertextTotal uint64
	for {
		if err := ctx.Err(); err != nil {
			return empty, err
		}
		n, readErr := io.ReadFull(plaintext, buf)
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return empty, readErr
		}
		if total+uint64(n) > MaxArchivePlaintextBytes {
			return empty, ErrArchiveTooLarge
		}
		final := n < EnvelopeChunkBytes
		digest.Write(buf[:n])
		total += uint64(n)
		ciphertext := dataAEAD.Seal(nil, chunkNonce(prefix, index), buf[:n], chunkAAD(id, index, uint32(n), final))
		ciphertextTotal += uint64(len(ciphertext))
		if ciphertextTotal > MaxArchiveCiphertextBytes {
			return empty, ErrArchiveTooLarge
		}
		if err := sink.Put(ctx, Chunk{Index: index, PlaintextLength: uint32(n), Final: final, Ciphertext: ciphertext}); err != nil {
			return empty, err
		}
		index++
		if final {
			break
		}
	}
	core.PlaintextBytes = total
	core.PlaintextSHA256 = hex.EncodeToString(digest.Sum(nil))
	core.ChunkCount = index
	aad, err := coreAAD(core)
	if err != nil {
		return empty, err
	}
	wrapNonce, err := randomBytes(12)
	if err != nil {
		return empty, err
	}
	wrapped := wrapAEAD.Seal(nil, wrapNonce, dataKey, aad)
	header := EnvelopeHeader{Core: core, WrapNonce: base64.RawURLEncoding.EncodeToString(wrapNonce), WrappedKey: base64.RawURLEncoding.EncodeToString(wrapped)}
	encoded, err := json.Marshal(header)
	if err != nil {
		return empty, err
	}
	if len(encoded) > maxEnvelopeHeaderBytes {
		return empty, ErrEnvelopeIntegrity
	}
	return header, nil
}

// OpenArchive verifies framing, every GCM tag, total length, and SHA-256.
// For an HTTP response, call it once with io.Discard before streaming bytes.
func OpenArchive(ctx context.Context, header EnvelopeHeader, kek []byte, src ChunkSource, out io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(header)
	if err != nil {
		return err
	}
	if len(encoded) > maxEnvelopeHeaderBytes {
		return ErrEnvelopeIntegrity
	}
	core := header.Core
	if core.Version != "v1" || core.ChunkCount == 0 || core.PlaintextBytes > MaxArchivePlaintextBytes {
		return ErrEnvelopeIntegrity
	}
	id, err := exportIDBytes(core.ExportID)
	if err != nil {
		return err
	}
	prefix, err := base64.RawURLEncoding.DecodeString(core.NoncePrefix)
	if err != nil || len(prefix) != 4 {
		return ErrEnvelopeIntegrity
	}
	nonce, err := base64.RawURLEncoding.DecodeString(header.WrapNonce)
	if err != nil || len(nonce) != 12 {
		return ErrEnvelopeIntegrity
	}
	wrapped, err := base64.RawURLEncoding.DecodeString(header.WrappedKey)
	if err != nil || len(wrapped) != 48 {
		return ErrEnvelopeIntegrity
	}
	wrapAEAD, err := exportAEAD(kek)
	if err != nil {
		return err
	}
	aad, err := coreAAD(core)
	if err != nil {
		return err
	}
	dataKey, err := wrapAEAD.Open(nil, nonce, wrapped, aad)
	if err != nil {
		return ErrEnvelopeIntegrity
	}
	defer erase(dataKey)
	dataAEAD, err := exportAEAD(dataKey)
	if err != nil {
		return err
	}
	digest := sha256.New()
	var index, total uint64
	seenFinal := false
	err = src.Each(ctx, func(frame Chunk) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if seenFinal || frame.Index != index || index >= core.ChunkCount {
			return ErrEnvelopeIntegrity
		}
		if frame.PlaintextLength > EnvelopeChunkBytes || len(frame.Ciphertext) != int(frame.PlaintextLength)+16 {
			return ErrEnvelopeIntegrity
		}
		if frame.Final != (index == core.ChunkCount-1) {
			return ErrEnvelopeIntegrity
		}
		if !frame.Final && frame.PlaintextLength != EnvelopeChunkBytes {
			return ErrEnvelopeIntegrity
		}
		if frame.Final && frame.PlaintextLength == EnvelopeChunkBytes {
			return ErrEnvelopeIntegrity
		}
		plaintext, err := dataAEAD.Open(nil, chunkNonce(prefix, index), frame.Ciphertext, chunkAAD(id, index, frame.PlaintextLength, frame.Final))
		if err != nil {
			return ErrEnvelopeIntegrity
		}
		if total+uint64(len(plaintext)) > core.PlaintextBytes {
			return ErrEnvelopeIntegrity
		}
		digest.Write(plaintext)
		total += uint64(len(plaintext))
		n, err := out.Write(plaintext)
		if err != nil {
			return err
		}
		if n != len(plaintext) {
			return io.ErrShortWrite
		}
		index++
		seenFinal = frame.Final
		return nil
	})
	if err != nil {
		return err
	}
	if !seenFinal || index != core.ChunkCount || total != core.PlaintextBytes || hex.EncodeToString(digest.Sum(nil)) != core.PlaintextSHA256 {
		return ErrEnvelopeIntegrity
	}
	return nil
}
