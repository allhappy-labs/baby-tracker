package records

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

var ErrIncompleteJournalReservation = errors.New("incomplete journal reservation")

func AppendJournal(ctx context.Context, tx pgx.Tx, grant access.Grant, deviceID string, mutation protocol.Mutation,
	entities []protocol.CanonicalEntity, instanceID string, epoch uint64) ([]protocol.JournalChange, error) {
	if len(entities) == 0 {
		return []protocol.JournalChange{}, nil
	}
	var last int64
	err := tx.QueryRow(ctx, `UPDATE families SET next_sequence=next_sequence+$2
		WHERE id=$1 AND next_sequence <= 9007199254740991-$2 RETURNING next_sequence`, grant.FamilyID, len(entities)).Scan(&last)
	if err != nil {
		return nil, err
	}
	first := last - int64(len(entities)) + 1
	changes := make([]protocol.JournalChange, 0, len(entities))
	for index, entity := range entities {
		sequence := uint64(first + int64(index))
		cursor := protocol.Cursor{ServerInstanceID: instanceID, RecoveryEpoch: epoch, FamilyID: grant.FamilyID, Sequence: sequence}
		change := protocol.JournalChange{Cursor: cursor, EntityKind: entity.EntityKind, EntityID: entity.Meta.ID,
			EntityVersion: entity.Meta.EntityVersion, ActorAccountID: grant.AccountID, DeviceID: deviceID,
			Operation: mutation.Operation, AcceptedAt: entity.Meta.AcceptedAt, Optional: false, SnapshotOrTombstone: entity}
		raw, marshalErr := json.Marshal(entity)
		if marshalErr != nil {
			return nil, marshalErr
		}
		if _, err := tx.Exec(ctx, `INSERT INTO mutation_journal(family_id,sequence,entity_kind,entity_id,entity_version,
			actor_account_id,device_id,operation,canonical_entity,accepted_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, grant.FamilyID, sequence, entity.EntityKind, entity.Meta.ID,
			entity.Meta.EntityVersion, grant.AccountID, deviceID, mutation.Operation, raw, entity.Meta.AcceptedAt); err != nil {
			return nil, err
		}
		table, tableErr := tableFor(entity.EntityKind)
		if tableErr != nil {
			return nil, tableErr
		}
		result, execErr := tx.Exec(ctx, `UPDATE `+table+` SET last_sequence=$3 WHERE family_id=$1 AND id=$2 AND entity_version=$4`,
			grant.FamilyID, entity.Meta.ID, sequence, entity.Meta.EntityVersion)
		if execErr != nil {
			return nil, execErr
		}
		if result.RowsAffected() != 1 {
			return nil, fmt.Errorf("journal target changed")
		}
		changes = append(changes, change)
	}
	return changes, nil
}

type journalReservation struct {
	grant      access.Grant
	deviceID   string
	mutation   protocol.Mutation
	instanceID string
	epoch      uint64
	next       uint64
	remaining  int
	last       protocol.Cursor
	bytes      int64
}

// JournalBatchAppender accepts one bounded, homogeneous entity-kind batch.
// Callers receive it only for the duration of AppendJournalBatches.
type JournalBatchAppender func([]protocol.CanonicalEntity) error

// AppendJournalBatches allocates one contiguous cursor range and asks produce
// to fill it in bounded batches in the caller's transaction. It cannot report
// success unless every reserved sequence was written, so a caller cannot
// accidentally commit a cursor range containing unseen changes.
func AppendJournalBatches(ctx context.Context, tx pgx.Tx, grant access.Grant, deviceID string, mutation protocol.Mutation,
	entityCount int, instanceID string, epoch uint64, produce func(JournalBatchAppender) error) (protocol.Cursor, int64, error) {
	reservation, err := reserveJournal(ctx, tx, grant, deviceID, mutation, entityCount, instanceID, epoch)
	if err != nil {
		return protocol.Cursor{}, 0, err
	}
	if err := produce(func(entities []protocol.CanonicalEntity) error {
		return reservation.append(ctx, tx, entities)
	}); err != nil {
		return protocol.Cursor{}, 0, err
	}
	return reservation.finish()
}

func reserveJournal(ctx context.Context, tx pgx.Tx, grant access.Grant, deviceID string, mutation protocol.Mutation,
	entityCount int, instanceID string, epoch uint64) (*journalReservation, error) {
	if entityCount < 1 {
		return nil, fmt.Errorf("journal reservation must contain entities")
	}
	var last int64
	if err := tx.QueryRow(ctx, `UPDATE families SET next_sequence=next_sequence+$2
		WHERE id=$1 AND next_sequence <= 9007199254740991-$2 RETURNING next_sequence`, grant.FamilyID, entityCount).Scan(&last); err != nil {
		return nil, err
	}
	first := last - int64(entityCount) + 1
	return &journalReservation{grant: grant, deviceID: deviceID, mutation: mutation, instanceID: instanceID,
		epoch: epoch, next: uint64(first), remaining: entityCount}, nil
}

// Append writes at most one bounded caller-owned batch without retaining it.
// All entities in a batch must have the same kind so the typed source table can
// be updated set-wise in the same statement as its journal insertion.
func (reservation *journalReservation) append(ctx context.Context, tx pgx.Tx, entities []protocol.CanonicalEntity) error {
	if reservation == nil || len(entities) == 0 || len(entities) > reservation.remaining {
		return fmt.Errorf("invalid journal reservation batch")
	}
	kind := entities[0].EntityKind
	table, err := tableFor(kind)
	if err != nil {
		return err
	}
	sequences := make([]int64, len(entities))
	ids := make([]string, len(entities))
	versions := make([]int64, len(entities))
	canonical := make([]string, len(entities))
	accepted := make([]string, len(entities))
	for index, entity := range entities {
		if entity.EntityKind != kind {
			return fmt.Errorf("journal reservation batch mixes entity kinds")
		}
		sequence := reservation.next + uint64(index)
		change := protocol.JournalChange{Cursor: protocol.Cursor{ServerInstanceID: reservation.instanceID,
			RecoveryEpoch: reservation.epoch, FamilyID: reservation.grant.FamilyID, Sequence: sequence},
			EntityKind: kind, EntityID: entity.Meta.ID, EntityVersion: entity.Meta.EntityVersion,
			ActorAccountID: reservation.grant.AccountID, DeviceID: reservation.deviceID,
			Operation: reservation.mutation.Operation, AcceptedAt: entity.Meta.AcceptedAt,
			Optional: false, SnapshotOrTombstone: entity}
		raw, marshalErr := json.Marshal(entity)
		if marshalErr != nil {
			return marshalErr
		}
		storageBytes, sizeErr := JournalStorageBytes(change)
		if sizeErr != nil {
			return sizeErr
		}
		reservation.bytes += storageBytes
		sequences[index], ids[index], versions[index] = int64(sequence), entity.Meta.ID, int64(entity.Meta.EntityVersion)
		canonical[index], accepted[index] = string(raw), entity.Meta.AcceptedAt
		reservation.last = change.Cursor
	}
	result, err := tx.Exec(ctx, `/* child-cascade-journal */
		WITH input AS (
			SELECT * FROM unnest($2::bigint[],$3::uuid[],$4::bigint[],$5::text[],$6::timestamptz[])
				AS value(sequence,entity_id,entity_version,canonical_entity,accepted_at)
		), inserted AS (
			INSERT INTO mutation_journal(family_id,sequence,entity_kind,entity_id,entity_version,
				actor_account_id,device_id,operation,canonical_entity,accepted_at)
			SELECT $1,value.sequence,$7,value.entity_id,value.entity_version,$8,$9,$10,value.canonical_entity::jsonb,value.accepted_at
			FROM input value ORDER BY value.sequence
			RETURNING entity_id,entity_version,sequence
		)
		UPDATE `+table+` target SET last_sequence=inserted.sequence FROM inserted
		WHERE target.family_id=$1 AND target.id=inserted.entity_id AND target.entity_version=inserted.entity_version`,
		reservation.grant.FamilyID, sequences, ids, versions, canonical, accepted, kind,
		reservation.grant.AccountID, reservation.deviceID, reservation.mutation.Operation)
	if err != nil {
		return err
	}
	if result.RowsAffected() != int64(len(entities)) {
		return fmt.Errorf("journal target changed")
	}
	reservation.next += uint64(len(entities))
	reservation.remaining -= len(entities)
	return nil
}

func (reservation *journalReservation) finish() (protocol.Cursor, int64, error) {
	if reservation == nil || reservation.remaining != 0 || reservation.last.Sequence == 0 {
		return protocol.Cursor{}, 0, ErrIncompleteJournalReservation
	}
	return reservation.last, reservation.bytes, nil
}
