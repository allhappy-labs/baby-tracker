package hosted

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	"babytracker/server/internal/security"
)

const (
	headEnvelopeVersion = "security-head-v1"
	headAADDomain       = "baby-tracker-security-head-aad-v1"
	maxHeadPlaintext    = 1024
	maxHeadCiphertext   = 4096
)

type Heads struct {
	heads      AtomicHeads
	cipher     EnvelopeCipher
	instanceID string
	keyID      string
}

type headEnvelope struct {
	Version    string `json:"version"`
	Generation uint64 `json:"generation"`
	Revision   uint64 `json:"revision"`
	EventHash  string `json:"event_hash"`
	InstanceID string `json:"instance_id"`
}

func NewHeads(heads AtomicHeads, cipher EnvelopeCipher, instanceID, keyID string) (security.LedgerHeadStore, error) {
	if heads == nil || cipher == nil || !canonicalInstanceID.MatchString(instanceID) || !validBoundedIdentifier(keyID) {
		return nil, errors.New("invalid hosted head configuration")
	}
	return &Heads{heads: heads, cipher: cipher, instanceID: instanceID, keyID: keyID}, nil
}

func (heads *Heads) Load(ctx context.Context) (security.Head, error) {
	value, err := heads.heads.Load(ctx)
	if contextErr := contextError(ctx, err); contextErr != nil {
		return security.Head{}, contextErr
	}
	if errors.Is(err, ErrHeadNotFound) {
		return security.Head{}, security.ErrTrustedHeadRequired
	}
	if err != nil {
		return security.Head{}, safeUnavailable("head")
	}
	return heads.open(ctx, value)
}

func (heads *Heads) CompareAndSwap(ctx context.Context, expected, next security.Head) (bool, error) {
	if err := validateHead(expected); err != nil || next.Generation != 1 || next.Generation != expected.Generation ||
		next.Revision != expected.Revision+1 || next.EventHash == ([32]byte{}) {
		return false, security.ErrLedgerCorrupt
	}
	currentValue, err := heads.heads.Load(ctx)
	if contextErr := contextError(ctx, err); contextErr != nil {
		return false, contextErr
	}
	if errors.Is(err, ErrHeadNotFound) {
		return false, security.ErrTrustedHeadRequired
	}
	if err != nil {
		return false, safeUnavailable("head")
	}
	current, err := heads.open(ctx, currentValue)
	if err != nil {
		return false, err
	}
	if current != expected {
		return false, nil
	}
	plaintext, err := encodeHead(next, heads.instanceID)
	if err != nil {
		return false, security.ErrLedgerCorrupt
	}
	defer zeroBytes(plaintext)
	sealed, err := heads.cipher.Seal(ctx, heads.keyID, plaintext, headAAD(heads.instanceID, heads.keyID))
	ciphertext := append([]byte(nil), sealed...)
	if contextErr := contextError(ctx, err); contextErr != nil {
		return false, contextErr
	}
	if err != nil || len(ciphertext) == 0 || len(ciphertext) > maxHeadCiphertext {
		return false, safeUnavailable("head")
	}
	updated := EncryptedHeadValue{KeyID: heads.keyID, Ciphertext: append([]byte(nil), ciphertext...)}
	swapped, casErr := heads.heads.CompareAndSwap(ctx, currentValue.Version, updated)
	if casErr == nil {
		return swapped, nil
	}
	if contextErr := contextError(ctx, casErr); contextErr != nil && !errors.Is(casErr, context.DeadlineExceeded) {
		return false, contextErr
	}

	// A provider may time out after a successful atomic write. Reconcile the
	// authenticated head before deciding whether the exact update committed.
	if err := ctx.Err(); err != nil {
		return false, err
	}
	reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reconciliationTimeout)
	defer cancel()
	observedValue, loadErr := heads.heads.Load(reconcileCtx)
	if loadErr != nil {
		return false, safeUnavailable("head")
	}
	observed, openErr := heads.open(reconcileCtx, observedValue)
	if openErr != nil {
		return false, openErr
	}
	if observed == next {
		return true, nil
	}
	if observed != expected {
		return false, nil
	}
	return false, safeUnavailable("head")
}

func (heads *Heads) open(ctx context.Context, value EncryptedHeadValue) (security.Head, error) {
	if value.Version == "" || value.KeyID != heads.keyID || len(value.Ciphertext) == 0 || len(value.Ciphertext) > maxHeadCiphertext {
		return security.Head{}, security.ErrLedgerCorrupt
	}
	plaintext, err := heads.cipher.Open(ctx, value.KeyID, value.Ciphertext, headAAD(heads.instanceID, value.KeyID))
	defer zeroBytes(plaintext)
	if contextErr := contextError(ctx, err); contextErr != nil {
		return security.Head{}, contextErr
	}
	if err != nil || len(plaintext) == 0 || len(plaintext) > maxHeadPlaintext {
		return security.Head{}, security.ErrLedgerCorrupt
	}
	return decodeHead(plaintext, heads.instanceID)
}

func encodeHead(head security.Head, instanceID string) ([]byte, error) {
	if err := validateHead(head); err != nil || !canonicalInstanceID.MatchString(instanceID) {
		return nil, security.ErrLedgerCorrupt
	}
	envelope := headEnvelope{
		Version: headEnvelopeVersion, Generation: head.Generation, Revision: head.Revision,
		EventHash: hex.EncodeToString(head.EventHash[:]), InstanceID: instanceID,
	}
	return json.Marshal(envelope)
}

func decodeHead(data []byte, instanceID string) (security.Head, error) {
	if len(data) == 0 || len(data) > maxHeadPlaintext {
		return security.Head{}, security.ErrLedgerCorrupt
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var envelope headEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return security.Head{}, security.ErrLedgerCorrupt
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return security.Head{}, security.ErrLedgerCorrupt
	}
	canonical, err := json.Marshal(envelope)
	if err != nil || !bytes.Equal(canonical, data) || envelope.Version != headEnvelopeVersion || envelope.InstanceID != instanceID {
		return security.Head{}, security.ErrLedgerCorrupt
	}
	eventHash, ok := decodeHash(envelope.EventHash)
	if !ok {
		return security.Head{}, security.ErrLedgerCorrupt
	}
	head := security.Head{Generation: envelope.Generation, Revision: envelope.Revision, EventHash: eventHash}
	if err := validateHead(head); err != nil {
		return security.Head{}, err
	}
	return head, nil
}

func validateHead(head security.Head) error {
	if head.Generation != 1 || head.Revision > 9007199254740991 || (head.Revision == 0) != (head.EventHash == [32]byte{}) {
		return security.ErrLedgerCorrupt
	}
	return nil
}

func headAAD(instanceID, keyID string) []byte {
	return frameFields([]byte(headAADDomain), []byte(instanceID), []byte(keyID))
}
