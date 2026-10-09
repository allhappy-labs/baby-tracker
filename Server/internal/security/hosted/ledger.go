package hosted

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"babytracker/server/internal/security"
)

const (
	ledgerEnvelopeVersion = "security-ledger-envelope-v1"
	ledgerAADDomain       = "baby-tracker-security-ledger-aad-v1"
	maxLedgerPlaintext    = 68 << 10
	maxLedgerCiphertext   = 128 << 10
	replayIndexEntryBytes = 48
	maxMaterializedEvents = 4096
	reconciliationTimeout = 5 * time.Second
)

var canonicalInstanceID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type Ledger struct {
	objects         ImmutableObjects
	cipher          EnvelopeCipher
	scratch         EncryptedReplayScratch
	instanceID      string
	keyID           string
	maxScratchBytes int64
}

type ledgerEnvelope struct {
	Version   string          `json:"version"`
	CommandID string          `json:"command_id"`
	Prior     ledgerHead      `json:"prior"`
	EventJSON json.RawMessage `json:"event"`
	EventHash string          `json:"event_hash"`
}

type ledgerHead struct {
	Generation uint64 `json:"generation"`
	Revision   uint64 `json:"revision"`
	EventHash  string `json:"event_hash"`
}

func NewLedger(objects ImmutableObjects, cipher EnvelopeCipher, scratch EncryptedReplayScratch, instanceID, keyID string, maxScratchBytes int64) (security.SecurityLedger, error) {
	if objects == nil || cipher == nil || scratch == nil || !canonicalInstanceID.MatchString(instanceID) ||
		!validBoundedIdentifier(keyID) || maxScratchBytes < replayIndexEntryBytes || maxScratchBytes > 1<<30 {
		return nil, errors.New("invalid hosted ledger configuration")
	}
	return &Ledger{objects: objects, cipher: cipher, scratch: scratch, instanceID: instanceID, keyID: keyID, maxScratchBytes: maxScratchBytes}, nil
}

func (ledger *Ledger) AppendCandidate(ctx context.Context, candidate security.Candidate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateLedgerCandidate(candidate); err != nil {
		return security.ErrLedgerCorrupt
	}
	key := ledgerObjectKey(candidate.Prior.Generation, candidate.Prior.Revision+1, candidate.EventHash)
	existing, readErr := ledger.objects.Read(ctx, key)
	switch {
	case readErr == nil:
		got, decodeErr := ledger.openObject(ctx, key, candidate.Prior.Generation, candidate.Prior.Revision+1, candidate.EventHash, existing)
		if contextErr := contextError(ctx, decodeErr); contextErr != nil {
			return contextErr
		}
		if decodeErr != nil || !equalCandidates(got, candidate) {
			return security.ErrLedgerCorrupt
		}
		return nil
	case errors.Is(readErr, ErrObjectNotFound):
		// The immutable slot is free; continue to conditional create.
	case contextError(ctx, readErr) != nil:
		return contextError(ctx, readErr)
	default:
		return safeUnavailable("ledger")
	}

	plaintext, err := encodeLedgerCandidate(candidate)
	if err != nil {
		return security.ErrLedgerCorrupt
	}
	defer zeroBytes(plaintext)
	aad := ledgerAAD(ledger.instanceID, ledger.keyID, candidate.Prior.Generation, candidate.Prior.Revision+1, candidate.EventHash)
	sealed, err := ledger.cipher.Seal(ctx, ledger.keyID, plaintext, aad)
	ciphertext := append([]byte(nil), sealed...)
	if contextErr := contextError(ctx, err); contextErr != nil {
		return contextErr
	}
	if err != nil || len(ciphertext) == 0 || len(ciphertext) > maxLedgerCiphertext {
		return safeUnavailable("ledger")
	}
	created := ObjectVersion{Key: key, KeyID: ledger.keyID, Ciphertext: append([]byte(nil), ciphertext...), SHA256: sha256.Sum256(ciphertext)}
	createErr := ledger.objects.Create(ctx, created)
	if createErr == nil {
		return nil
	} else if contextErr := contextError(ctx, createErr); contextErr != nil && !errors.Is(createErr, context.DeadlineExceeded) {
		return contextErr
	}

	// A definite create conflict can be a concurrent idempotent append whose
	// randomized AEAD bytes differ. Accept only its authenticated canonical
	// candidate. Ambiguous failures reconcile only this attempt's exact bytes.
	if err := ctx.Err(); err != nil {
		return err
	}
	reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reconciliationTimeout)
	defer cancel()
	stored, readErr := ledger.objects.Read(reconcileCtx, key)
	if readErr == nil {
		if errors.Is(createErr, ErrObjectExists) {
			got, decodeErr := ledger.openObject(reconcileCtx, key, candidate.Prior.Generation, candidate.Prior.Revision+1, candidate.EventHash, stored)
			if decodeErr == nil && equalCandidates(got, candidate) {
				return nil
			}
			return security.ErrLedgerCorrupt
		}
		if exactObject(stored, created) {
			return nil
		}
		return security.ErrLedgerCorrupt
	}
	return safeUnavailable("ledger")
}

func (ledger *Ledger) ReadCommitted(ctx context.Context, head security.Head) ([]security.Candidate, error) {
	if head.Revision > maxMaterializedEvents || head.Revision > uint64(ledger.maxScratchBytes/replayIndexEntryBytes) {
		return nil, ErrScratchFull
	}
	result := make([]security.Candidate, 0, head.Revision)
	err := ledger.WalkCommitted(ctx, head, func(candidate security.Candidate) error {
		result = append(result, candidate)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (ledger *Ledger) WalkCommitted(ctx context.Context, head security.Head, visit func(security.Candidate) error) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if visit == nil || head.Generation != 1 || (head.Revision == 0) != (head.EventHash == [32]byte{}) {
		return security.ErrLedgerCorrupt
	}
	if head.Revision > uint64(ledger.maxScratchBytes/replayIndexEntryBytes) {
		return ErrScratchFull
	}
	if head.Revision == 0 {
		return nil
	}
	request := ReplayScratchRequest{
		Namespace: fmt.Sprintf("security-ledger-replay/%s/g%020d/%x", ledger.instanceID, head.Generation, head.EventHash),
		MaxBytes:  ledger.maxScratchBytes,
	}
	session, err := ledger.scratch.Begin(ctx, request)
	if session != nil {
		defer func() {
			if closeErr := session.Close(); closeErr != nil {
				resultErr = safeUnavailable("replay scratch")
			}
		}()
	}
	if contextErr := contextError(ctx, err); contextErr != nil {
		return contextErr
	}
	if err != nil || session == nil {
		if errors.Is(err, ErrScratchFull) {
			return ErrScratchFull
		}
		return safeUnavailable("replay scratch")
	}

	requiredHash := head.EventHash
	for revision := head.Revision; revision >= 1; revision-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := ledgerObjectKey(head.Generation, revision, requiredHash)
		object, err := ledger.objects.Read(ctx, key)
		if contextErr := contextError(ctx, err); contextErr != nil {
			return contextErr
		}
		if errors.Is(err, ErrObjectNotFound) {
			return security.ErrLedgerCorrupt
		}
		if err != nil {
			return safeUnavailable("ledger")
		}
		candidate, err := ledger.openObject(ctx, key, head.Generation, revision, requiredHash, object)
		if contextErr := contextError(ctx, err); contextErr != nil {
			return contextErr
		}
		if err != nil || candidate.Prior.Generation != head.Generation || candidate.Prior.Revision+1 != revision {
			return security.ErrLedgerCorrupt
		}
		if err := session.Append(ctx, ReplayIndexEntry{Generation: head.Generation, Revision: revision, EventHash: requiredHash}); err != nil {
			if contextErr := contextError(ctx, err); contextErr != nil {
				return contextErr
			}
			if errors.Is(err, ErrScratchFull) {
				return ErrScratchFull
			}
			return safeUnavailable("replay scratch")
		}
		requiredHash = candidate.Prior.EventHash
		if revision == 1 {
			break
		}
	}
	if requiredHash != ([32]byte{}) {
		return security.ErrLedgerCorrupt
	}

	prior := security.Head{Generation: head.Generation}
	var visitorErr error
	err = session.WalkReverse(ctx, func(entry ReplayIndexEntry) error {
		if entry.Generation != head.Generation || entry.Revision != prior.Revision+1 {
			return security.ErrLedgerCorrupt
		}
		key := ledgerObjectKey(entry.Generation, entry.Revision, entry.EventHash)
		object, err := ledger.objects.Read(ctx, key)
		if contextErr := contextError(ctx, err); contextErr != nil {
			return contextErr
		}
		if errors.Is(err, ErrObjectNotFound) {
			return security.ErrLedgerCorrupt
		}
		if err != nil {
			return safeUnavailable("ledger")
		}
		candidate, err := ledger.openObject(ctx, key, entry.Generation, entry.Revision, entry.EventHash, object)
		if contextErr := contextError(ctx, err); contextErr != nil {
			return contextErr
		}
		if err != nil || candidate.Prior != prior {
			return security.ErrLedgerCorrupt
		}
		if err := visit(candidate); err != nil {
			visitorErr = err
			return err
		}
		prior = security.Head{Generation: entry.Generation, Revision: entry.Revision, EventHash: entry.EventHash}
		return nil
	})
	if contextErr := contextError(ctx, err); contextErr != nil {
		return contextErr
	}
	if err != nil {
		if visitorErr != nil && errors.Is(err, visitorErr) {
			return visitorErr
		}
		if errors.Is(err, security.ErrLedgerCorrupt) || errors.Is(err, ErrScratchFull) {
			return err
		}
		return safeUnavailable("replay scratch")
	}
	if prior != head {
		return security.ErrLedgerCorrupt
	}
	return nil
}

func (ledger *Ledger) openObject(ctx context.Context, expectedKey string, generation, revision uint64, eventHash [32]byte, object ObjectVersion) (security.Candidate, error) {
	if object.Key != expectedKey || object.Version == "" || object.KeyID != ledger.keyID || len(object.Ciphertext) == 0 ||
		len(object.Ciphertext) > maxLedgerCiphertext || sha256.Sum256(object.Ciphertext) != object.SHA256 {
		return security.Candidate{}, security.ErrLedgerCorrupt
	}
	plaintext, err := ledger.cipher.Open(ctx, object.KeyID, object.Ciphertext, ledgerAAD(ledger.instanceID, object.KeyID, generation, revision, eventHash))
	defer zeroBytes(plaintext)
	if contextErr := contextError(ctx, err); contextErr != nil {
		return security.Candidate{}, contextErr
	}
	if err != nil || len(plaintext) == 0 || len(plaintext) > maxLedgerPlaintext {
		return security.Candidate{}, security.ErrLedgerCorrupt
	}
	candidate, err := decodeLedgerCandidate(plaintext)
	if err != nil || candidate.Prior.Generation != generation || candidate.Prior.Revision+1 != revision || candidate.EventHash != eventHash {
		return security.Candidate{}, security.ErrLedgerCorrupt
	}
	return candidate, nil
}

func encodeLedgerCandidate(candidate security.Candidate) ([]byte, error) {
	if err := validateLedgerCandidate(candidate); err != nil {
		return nil, err
	}
	envelope := ledgerEnvelope{
		Version: ledgerEnvelopeVersion, CommandID: candidate.CommandID,
		Prior:     ledgerHead{Generation: candidate.Prior.Generation, Revision: candidate.Prior.Revision, EventHash: hex.EncodeToString(candidate.Prior.EventHash[:])},
		EventJSON: json.RawMessage(candidate.EventJSON), EventHash: hex.EncodeToString(candidate.EventHash[:]),
	}
	encoded, err := json.Marshal(envelope)
	if err != nil || len(encoded) > maxLedgerPlaintext {
		return nil, security.ErrLedgerCorrupt
	}
	return encoded, nil
}

func decodeLedgerCandidate(data []byte) (security.Candidate, error) {
	if len(data) == 0 || len(data) > maxLedgerPlaintext {
		return security.Candidate{}, security.ErrLedgerCorrupt
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var envelope ledgerEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return security.Candidate{}, security.ErrLedgerCorrupt
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return security.Candidate{}, security.ErrLedgerCorrupt
	}
	canonical, err := json.Marshal(envelope)
	if err != nil || !bytes.Equal(canonical, data) || envelope.Version != ledgerEnvelopeVersion {
		return security.Candidate{}, security.ErrLedgerCorrupt
	}
	priorHash, ok := decodeHash(envelope.Prior.EventHash)
	if !ok {
		return security.Candidate{}, security.ErrLedgerCorrupt
	}
	eventHash, ok := decodeHash(envelope.EventHash)
	if !ok {
		return security.Candidate{}, security.ErrLedgerCorrupt
	}
	candidate := security.Candidate{
		CommandID: envelope.CommandID,
		Prior:     security.Head{Generation: envelope.Prior.Generation, Revision: envelope.Prior.Revision, EventHash: priorHash},
		EventJSON: append([]byte(nil), envelope.EventJSON...), EventHash: eventHash,
	}
	if err := validateLedgerCandidate(candidate); err != nil {
		return security.Candidate{}, err
	}
	return candidate, nil
}

func validateLedgerCandidate(candidate security.Candidate) error {
	if candidate.Prior.Generation != 1 || candidate.Prior.Revision >= 9007199254740991 || candidate.CommandID == "" ||
		len(candidate.EventJSON) == 0 || sha256.Sum256(candidate.EventJSON) != candidate.EventHash {
		return security.ErrLedgerCorrupt
	}
	var event security.Event
	decoder := json.NewDecoder(bytes.NewReader(candidate.EventJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&event); err != nil {
		return security.ErrLedgerCorrupt
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return security.ErrLedgerCorrupt
	}
	canonical, err := security.EncodeEvent(event)
	if err != nil || !bytes.Equal(canonical, candidate.EventJSON) || event.Generation != candidate.Prior.Generation ||
		event.Revision != candidate.Prior.Revision+1 || event.CommandID != candidate.CommandID ||
		event.PreviousHash != hex.EncodeToString(candidate.Prior.EventHash[:]) {
		return security.ErrLedgerCorrupt
	}
	return nil
}

func ledgerObjectKey(generation, revision uint64, eventHash [32]byte) string {
	return fmt.Sprintf("security-ledger/v1/g%020d/r%020d/%x", generation, revision, eventHash)
}

func ledgerAAD(instanceID, keyID string, generation, revision uint64, eventHash [32]byte) []byte {
	return frameFields([]byte(ledgerAADDomain), []byte(instanceID), []byte(keyID), uint64Bytes(generation), uint64Bytes(revision), eventHash[:])
}

func frameFields(fields ...[]byte) []byte {
	total := 0
	for _, field := range fields {
		total += 4 + len(field)
	}
	result := make([]byte, 0, total)
	var length [4]byte
	for _, field := range fields {
		binary.BigEndian.PutUint32(length[:], uint32(len(field)))
		result = append(result, length[:]...)
		result = append(result, field...)
	}
	return result
}

func uint64Bytes(value uint64) []byte {
	result := make([]byte, 8)
	binary.BigEndian.PutUint64(result, value)
	return result
}

func decodeHash(value string) ([32]byte, bool) {
	var result [32]byte
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != len(result) || hex.EncodeToString(decoded) != value {
		return result, false
	}
	copy(result[:], decoded)
	return result, true
}

func exactObject(stored, expected ObjectVersion) bool {
	return stored.Version != "" && stored.Key == expected.Key && stored.KeyID == expected.KeyID &&
		stored.SHA256 == expected.SHA256 && bytes.Equal(stored.Ciphertext, expected.Ciphertext)
}

func equalCandidates(left, right security.Candidate) bool {
	return left.CommandID == right.CommandID && left.Prior == right.Prior && left.EventHash == right.EventHash && bytes.Equal(left.EventJSON, right.EventJSON)
}

func contextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
