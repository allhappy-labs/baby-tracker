package security

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type postgresProjector struct {
	pool *pgxpool.Pool
}

func NewProjector(pool *pgxpool.Pool) Projection {
	return &postgresProjector{pool: pool}
}

func (projector *postgresProjector) Head(ctx context.Context) (Head, error) {
	if projector == nil || projector.pool == nil {
		return Head{}, errors.New("projector database pool is required")
	}
	var head Head
	var hash []byte
	if err := projector.pool.QueryRow(ctx, `SELECT generation, revision, event_hash FROM security_projection_head WHERE singleton`).Scan(&head.Generation, &head.Revision, &hash); err != nil {
		return Head{}, fmt.Errorf("read security projection head: %w", err)
	}
	if len(hash) != len(head.EventHash) {
		return Head{}, ErrLedgerCorrupt
	}
	copy(head.EventHash[:], hash)
	return head, nil
}

func (projector *postgresProjector) Apply(ctx context.Context, candidate Candidate) error {
	if projector == nil || projector.pool == nil {
		return errors.New("projector database pool is required")
	}
	if err := validateCandidate(candidate.Prior, candidate); err != nil {
		return err
	}
	var event Event
	if err := decodeEventStrict(candidate.EventJSON, &event); err != nil {
		return ErrLedgerCorrupt
	}
	previousHash, err := hex.DecodeString(event.PreviousHash)
	if err != nil {
		return ErrLedgerCorrupt
	}
	commandHash, err := hex.DecodeString(event.CommandHash)
	if err != nil {
		return ErrLedgerCorrupt
	}
	result, err := json.Marshal(CommandResult{CommandID: event.CommandID, Outcome: "committed", Code: "ok", Revision: event.Revision})
	if err != nil {
		return err
	}
	if _, err := projector.pool.Exec(ctx, `SELECT app.project_security_event($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9::jsonb,$10)`,
		event.Generation, event.Revision, previousHash, candidate.EventHash[:], event.ActorAccountID, event.CommandID,
		commandHash, string(result), string(candidate.EventJSON), candidate.EventJSON); err != nil {
		return fmt.Errorf("project security event: %w", err)
	}
	return nil
}
