package authority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// CanonicalCommandHash identifies a command's content for exact-retry
// detection: sha256 of
//
//	{"version":"v2","kind":<kind>,"actor_account_id":<actor>,"payload":<canonical payload>}
//
// with the fields in that order, no insignificant whitespace, no HTML
// escaping, and the payload in canonical JSON (object keys sorted, integers
// only). The command ID is not part of the preimage: receipts are keyed by
// (actor, command ID) already.
func CanonicalCommandHash(actorID string, kind Kind, payload any) ([32]byte, error) {
	if !validID(actorID) || !kind.valid() {
		return [32]byte{}, ErrInvalidCommand
	}
	canonical, err := canonicalJSON(payload)
	if err != nil {
		return [32]byte{}, err
	}
	preimage := struct {
		Version        string          `json:"version"`
		Kind           Kind            `json:"kind"`
		ActorAccountID string          `json:"actor_account_id"`
		Payload        json.RawMessage `json:"payload"`
	}{"v2", kind, actorID, canonical}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(preimage); err != nil {
		return [32]byte{}, ErrInvalidCommand
	}
	return sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}

// lookupReceipt returns the stored result of (actorID, commandID). An exact
// retry (same hash) returns the stored result with found=true; a different
// command hash under the same ID is ErrCommandIDReused. Call it after the
// command's account locks, before any other check: the actor's account lock
// serializes concurrent copies of one command, so the loser sees the
// winner's committed receipt.
func lookupReceipt(ctx context.Context, tx pgx.Tx, actorID, commandID string, hash [32]byte) (Result, bool, error) {
	if !validID(actorID) || !validID(commandID) {
		return Result{}, false, ErrInvalidCommand
	}
	var stored, encoded []byte
	err := tx.QueryRow(ctx, `SELECT command_hash, result FROM command_receipts WHERE actor_account_id=$1 AND command_id=$2`, actorID, commandID).Scan(&stored, &encoded)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, fmt.Errorf("authority: lookup receipt: %w", err)
	}
	if !bytes.Equal(stored, hash[:]) {
		return Result{}, false, ErrCommandIDReused
	}
	result, err := decodeResult(encoded)
	if err != nil {
		return Result{}, false, err
	}
	return result, true, nil
}

// decodeResult decodes a receipt's stored result.
func decodeResult(encoded []byte) (Result, error) {
	var result Result
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return Result{}, fmt.Errorf("authority: decode receipt: %w", err)
	}
	return result, nil
}

// writeReceipt records a command's result under (actorID, commandID).
// familyIDs are the families the command affected (nil for none).
//
// Handlers lock the actor's account row, then lookupReceipt, then write: a
// concurrent copy of the same command waits on that lock and its lookup finds
// the committed receipt. If a receipt for (actorID, commandID) was committed
// anyway (a caller that skipped the actor lock), the write inserts nothing and
// fails deterministically: the same hash is ErrSecurityBusy (an identical
// command won the race; roll back, and the retry returns its stored result),
// a different hash is ErrCommandIDReused.
func writeReceipt(ctx context.Context, tx pgx.Tx, actorID, commandID string, hash [32]byte, kind Kind, result Result, familyIDs []string, at time.Time) error {
	if !validID(actorID) || !validID(commandID) || !kind.valid() || !validTime(at) || result.CommandID != commandID || result.Revision > maxWireInteger {
		return ErrInvalidCommand
	}
	families, err := sortedIDs(familyIDs)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return ErrInvalidCommand
	}
	tag, err := tx.Exec(ctx, `INSERT INTO command_receipts(actor_account_id,command_id,command_hash,kind,result,family_ids,created_at)
		VALUES ($1,$2,$3,$4::text::app.audit_kind,$5,$6::uuid[],$7) ON CONFLICT (actor_account_id,command_id) DO NOTHING`,
		actorID, commandID, hash[:], string(kind), encoded, families, at.UTC().Truncate(time.Microsecond))
	if err != nil {
		return fmt.Errorf("authority: write receipt: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	// ON CONFLICT waited for the conflicting transaction to commit, so this
	// read-committed statement sees its receipt.
	var stored []byte
	if err := tx.QueryRow(ctx, `SELECT command_hash FROM command_receipts WHERE actor_account_id=$1 AND command_id=$2`, actorID, commandID).Scan(&stored); err != nil {
		return fmt.Errorf("authority: write receipt: conflicting receipt unreadable: %w", errors.Join(err, ErrUnavailable))
	}
	if !bytes.Equal(stored, hash[:]) {
		return ErrCommandIDReused
	}
	return ErrSecurityBusy
}
