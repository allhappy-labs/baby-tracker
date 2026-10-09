package sync

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

type Receipt struct {
	Hash    [32]byte
	Outcome protocol.Outcome
}

func readReceipt(ctx context.Context, tx pgx.Tx, deviceID, mutationID string) (Receipt, bool, error) {
	var receipt Receipt
	var hash []byte
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT semantic_hash,outcome FROM mutation_receipts WHERE device_id=$1 AND mutation_id=$2`, deviceID, mutationID).Scan(&hash, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return receipt, false, nil
	}
	if err != nil {
		return receipt, false, err
	}
	if len(hash) != 32 {
		return receipt, false, errors.New("invalid receipt hash")
	}
	copy(receipt.Hash[:], hash)
	if err = json.Unmarshal(raw, &receipt.Outcome); err != nil {
		return receipt, false, err
	}
	return receipt, true, nil
}

func saveReceipt(ctx context.Context, tx pgx.Tx, g access.Grant, deviceID string, m protocol.Mutation, hash [32]byte, outcome protocol.Outcome, acceptedAt time.Time) error {
	raw, err := json.Marshal(outcome)
	if err != nil {
		return err
	}
	var last any
	if outcome.Cursor != nil {
		last = int64(outcome.Cursor.Sequence)
	}
	var result string
	err = tx.QueryRow(ctx, `SELECT app.save_mutation_receipt($1,$2,$3,$4,$5,$6,$7,$8)`, g.AccountID, g.FamilyID, deviceID, m.MutationID, hash[:], raw, acceptedAt, last).Scan(&result)
	if err != nil {
		return err
	}
	if result != "inserted" {
		return errReceiptCollision
	}
	return nil
}
