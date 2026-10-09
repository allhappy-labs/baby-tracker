package recovery

import (
	"context"
	"errors"
	"fmt"

	"babytracker/server/internal/security"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresMirror struct {
	reader *pgxpool.Pool
	writer *pgxpool.Pool
}

func NewPostgresMirror(runtime, projector *pgxpool.Pool) (*PostgresMirror, error) {
	if runtime == nil || projector == nil || runtime == projector {
		return nil, errors.New("distinct recovery mirror runtime reader and projector writer pools are required")
	}
	return &PostgresMirror{reader: runtime, writer: projector}, nil
}

func (mirror *PostgresMirror) Load(ctx context.Context) (State, bool, error) {
	if mirror == nil || mirror.reader == nil {
		return State{}, false, errors.New("recovery mirror runtime reader pool is required")
	}
	var instanceID string
	var epoch, generation, revision int64
	var hash []byte
	err := mirror.reader.QueryRow(ctx, `SELECT instance_id::text,recovery_epoch,generation,revision,event_hash
		FROM server_state WHERE singleton`).Scan(&instanceID, &epoch, &generation, &revision, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return State{}, false, nil
	}
	if err != nil || epoch < 0 || generation < 0 || revision < 0 || len(hash) != 32 {
		return State{}, false, errors.New("load server state mirror")
	}
	var eventHash [32]byte
	copy(eventHash[:], hash)
	state := State{InstanceID: instanceID, Epoch: uint64(epoch), TrustedHead: security.Head{
		Generation: uint64(generation), Revision: uint64(revision), EventHash: eventHash,
	}}
	if err := ValidateState(state); err != nil {
		return State{}, false, errors.New("load server state mirror")
	}
	return state, true, nil
}

func (mirror *PostgresMirror) Update(ctx context.Context, state State) error {
	if mirror == nil || mirror.writer == nil {
		return errors.New("recovery mirror projector writer pool is required")
	}
	if err := ValidateState(state); err != nil {
		return err
	}
	if _, err := mirror.writer.Exec(ctx, "SELECT app.update_server_state($1,$2,$3,$4,$5)", state.InstanceID, int64(state.Epoch),
		int64(state.TrustedHead.Generation), int64(state.TrustedHead.Revision), state.TrustedHead.EventHash[:]); err != nil {
		return fmt.Errorf("update server state mirror: %w", err)
	}
	return nil
}
