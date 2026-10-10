package authority

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// actorKeyCommitmentDomain separates the actor-ID key commitment from every
// other HMAC of the actor-ID key (recovery/actor_key.go:27).
const actorKeyCommitmentDomain = "baby-tracker-actor-id-commitment-v1"

// Server identity codes. Readiness reports them; `baby-tracker-admin
// authority` prints them.
var (
	// ErrServerStateMissing is a database without a server_state row:
	// `baby-tracker-admin authority init` has not run.
	ErrServerStateMissing error = Code("server_state_missing")
	// ErrServerIdentityMismatch is a server_state row whose instance ID or
	// recovery epoch is not the configured one (another node's database, or
	// an epoch rotated without restarting with the new BT_RECOVERY_EPOCH).
	ErrServerIdentityMismatch error = Code("server_identity_mismatch")
	// ErrActorIDKeyMismatch is a configured actor-ID key whose commitment is
	// not the one server_state holds (a replaced or lost key would re-key
	// every pseudonymous actor ID).
	ErrActorIDKeyMismatch error = Code("actor_id_key_mismatch")
)

// ActorKeyCommitment binds the permanent actor-ID key to one server
// instance without revealing it: HMAC-SHA256 keyed by the actor-ID key over
// the length-prefixed (4-byte big-endian) domain and instance ID. It is the
// legacy recovery sidecar commitment (recovery/actor_key.go:46-61), so a
// key that verified there verifies here.
func ActorKeyCommitment(key [32]byte, instanceID string) [32]byte {
	mac := hmac.New(sha256.New, key[:])
	for _, field := range []string{actorKeyCommitmentDomain, instanceID} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(field)))
		_, _ = mac.Write(length[:])
		_, _ = mac.Write([]byte(field))
	}
	var commitment [32]byte
	copy(commitment[:], mac.Sum(nil))
	return commitment
}

// ServerState is the server_state row: the node's instance ID, recovery
// epoch and actor-ID key commitment (nil until `authority init` binds it).
type ServerState struct {
	InstanceID         string
	RecoveryEpoch      uint64
	ActorKeyCommitment []byte
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

const serverStateSQL = `SELECT instance_id::text, recovery_epoch, actor_key_commitment FROM public.server_state WHERE singleton`

// LoadServerState reads the server_state row. A missing row is
// ErrServerStateMissing; any other failure wraps ErrUnavailable.
func LoadServerState(ctx context.Context, q rowQuerier) (ServerState, error) {
	var state ServerState
	var epoch int64
	err := q.QueryRow(ctx, serverStateSQL).Scan(&state.InstanceID, &epoch, &state.ActorKeyCommitment)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ServerState{}, ErrServerStateMissing
	case err != nil:
		return ServerState{}, fmt.Errorf("authority: server state: %w", errors.Join(err, ErrUnavailable))
	case epoch < 1:
		return ServerState{}, fmt.Errorf("authority: server state: recovery epoch %d: %w", epoch, ErrServerIdentityMismatch)
	}
	state.RecoveryEpoch = uint64(epoch)
	return state, nil
}

// Check reports whether the row is the configured node: the same instance
// ID and recovery epoch, and an actor-ID key commitment equal to the
// commitment of actorKey. An unbound commitment is a mismatch.
func (state ServerState) Check(instanceID string, epoch uint64, actorKey [32]byte) error {
	if state.InstanceID != instanceID || state.RecoveryEpoch != epoch {
		return ErrServerIdentityMismatch
	}
	want := ActorKeyCommitment(actorKey, instanceID)
	if !hmac.Equal(state.ActorKeyCommitment, want[:]) {
		return ErrActorIDKeyMismatch
	}
	return nil
}

// InitResult is what InitServerState did.
type InitResult string

const (
	// InitCreated inserted the row.
	InitCreated InitResult = "initialized"
	// InitBound set the commitment of a row that had the same instance and
	// epoch but no commitment.
	InitBound InitResult = "bound"
	// InitUnchanged found the identical row (an idempotent retry).
	InitUnchanged InitResult = "unchanged"
)

// InitServerState writes the server_state row for a fresh install: instance
// ID, recovery epoch 1 and the actor-ID key commitment. It runs as the
// migrator (the runtime role may only read server_state). A row with the
// same values is left alone; a row with any other instance ID, epoch or
// commitment is refused, so init can never re-key or re-identify a node.
func InitServerState(ctx context.Context, pool *pgxpool.Pool, instanceID string, actorKey [32]byte) (InitResult, error) {
	if pool == nil || !validID(instanceID) || actorKey == ([32]byte{}) {
		return "", ErrInvalidCommand
	}
	commitment := ActorKeyCommitment(actorKey, instanceID)
	var result InitResult
	err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `LOCK TABLE public.server_state IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		current, err := LoadServerState(ctx, tx)
		switch {
		case errors.Is(err, ErrServerStateMissing):
			if _, err := tx.Exec(ctx, `INSERT INTO public.server_state(singleton, instance_id, recovery_epoch, actor_key_commitment, updated_at)
				VALUES (true, $1, 1, $2, $3)`, instanceID, commitment[:], time.Now().UTC()); err != nil {
				return err
			}
			result = InitCreated
			return nil
		case err != nil:
			return err
		case current.InstanceID != instanceID || current.RecoveryEpoch != 1:
			return ErrServerIdentityMismatch
		case current.ActorKeyCommitment == nil:
			if _, err := tx.Exec(ctx, `UPDATE public.server_state SET actor_key_commitment=$1, updated_at=$2 WHERE singleton`,
				commitment[:], time.Now().UTC()); err != nil {
				return err
			}
			result = InitBound
			return nil
		case !hmac.Equal(current.ActorKeyCommitment, commitment[:]):
			return ErrActorIDKeyMismatch
		default:
			result = InitUnchanged
			return nil
		}
	})
	if err != nil {
		return "", serverStateError(err)
	}
	return result, nil
}

// RotateRecoveryEpoch moves server_state.recovery_epoch from expectedEpoch
// to expectedEpoch+1 on the node whose instance ID is confirmInstance (the
// operator's confirmation). It runs as the migrator. Every session carries
// the epoch it was issued under, so once the node restarts with the new
// BT_RECOVERY_EPOCH every earlier session fails epoch_changed; until then
// readiness refuses (server_identity_mismatch). A stale expectedEpoch or
// another instance is refused and nothing changes.
//
// The same transaction empties device_sync_cursors: every cursor recorded
// there names what was delivered under the old epoch (a restored database
// may be older than what the devices have seen), and the wake worker
// rebuilds the rows from the devices' next requests. The migrator owns the
// table, and TRUNCATE is not subject to its row security.
func RotateRecoveryEpoch(ctx context.Context, pool *pgxpool.Pool, confirmInstance string, expectedEpoch uint64) (ServerState, error) {
	if pool == nil || !validID(confirmInstance) || expectedEpoch < 1 || expectedEpoch >= maxWireInteger {
		return ServerState{}, ErrInvalidCommand
	}
	var rotated ServerState
	err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `LOCK TABLE public.server_state IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		current, err := LoadServerState(ctx, tx)
		if err != nil {
			return err
		}
		if current.InstanceID != confirmInstance || current.RecoveryEpoch != expectedEpoch {
			return ErrServerIdentityMismatch
		}
		if _, err := tx.Exec(ctx, `UPDATE public.server_state SET recovery_epoch=$1, updated_at=$2 WHERE singleton`,
			int64(expectedEpoch+1), time.Now().UTC()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `TRUNCATE public.device_sync_cursors`); err != nil {
			return err
		}
		rotated = current
		rotated.RecoveryEpoch = expectedEpoch + 1
		return nil
	})
	if err != nil {
		return ServerState{}, serverStateError(err)
	}
	return rotated, nil
}

func serverStateError(err error) error {
	var code Code
	if errors.As(err, &code) {
		return err
	}
	return fmt.Errorf("authority: server state: %w", errors.Join(err, ErrUnavailable))
}
