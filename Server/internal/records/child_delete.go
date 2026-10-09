package records

import (
	"context"
	"errors"
	"fmt"

	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

const childDeleteBatchSize = 4096

type cascadeTarget struct {
	id       string
	version  uint64
	creator  string
	oldBytes int64
}

func (r *Repository) DeleteBaby(ctx context.Context, tx pgx.Tx, command ApplyCommand) (protocol.Outcome, error) {
	m := command.Mutation
	var lockedID string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM babies WHERE family_id=$1 AND id=$2 FOR UPDATE`, command.Grant.FamilyID, m.EntityID).Scan(&lockedID); errors.Is(err, pgx.ErrNoRows) {
		return rejection(m, "rejected", "entity_not_found", nil), nil
	} else if err != nil {
		return protocol.Outcome{}, err
	}
	baby, err := r.get(ctx, tx, "baby", command.Grant.FamilyID, m.EntityID)
	if err != nil {
		return protocol.Outcome{}, err
	}
	if baby.Meta.TombstonedAt != nil {
		return rejection(m, "superseded", "entity_deleted", &baby), nil
	}

	metaTemplate, err := timerMeta(command, 1, "")
	if err != nil {
		return protocol.Outcome{}, err
	}
	total, err := countCascadeTargets(ctx, tx, command.Grant.FamilyID, m.EntityID)
	if err != nil {
		return protocol.Outcome{}, err
	}
	oldRootBytes, err := storedEntityBytes(ctx, tx, command.Grant.FamilyID, baby.EntityKind, baby.Meta.ID)
	if err != nil {
		return protocol.Outcome{}, err
	}
	root, rootBytes, err := cascadeTombstone(command, metaTemplate, baby.EntityKind, cascadeTarget{
		id: baby.Meta.ID, version: baby.Meta.EntityVersion, creator: baby.Meta.CreatorAccountID, oldBytes: oldRootBytes,
	})
	if err != nil {
		return protocol.Outcome{}, err
	}
	if !entityFitsMinimumPage(command, root) {
		return rejection(m, "rejected", "mutation_too_large", nil), nil
	}
	byteDelta := rootBytes - oldRootBytes - deletionReserveBytes
	countDelta := int64(0)
	last, journalBytes, err := AppendJournalBatches(ctx, tx, command.Grant, command.DeviceID, m, total,
		command.ServerInstanceID, command.RecoveryEpoch, func(appendBatch JournalBatchAppender) error {
			if err := r.putTombstone(ctx, tx, root.Meta, root.EntityKind, rootBytes); err != nil {
				return err
			}
			if err := appendBatch([]protocol.CanonicalEntity{root}); err != nil {
				return err
			}
			for _, source := range []struct {
				kind, table string
			}{{"feed", "feed_entries"}, {"sleep", "sleep_entries"}, {"diaper", "diaper_entries"}, {"supplement", "supplement_entries"}, {"growth_profile", "growth_profiles"}, {"measurement", "measurement_entries"}, {"timer", "active_timers"}} {
				afterID := "00000000-0000-0000-0000-000000000000"
				for {
					targets, loadErr := loadCascadeTargets(ctx, tx, command.Grant.FamilyID, m.EntityID, source.table, afterID)
					if loadErr != nil {
						return loadErr
					}
					if len(targets) == 0 {
						break
					}
					tombstones := make([]protocol.CanonicalEntity, len(targets))
					storageBytes := make([]int64, len(targets))
					for index, target := range targets {
						tombstone, bytes, tombstoneErr := cascadeTombstone(command, metaTemplate, source.kind, target)
						if tombstoneErr != nil {
							return tombstoneErr
						}
						tombstones[index], storageBytes[index] = tombstone, bytes
						byteDelta += bytes - target.oldBytes - deletionReserveBytes
						if source.kind != "timer" {
							countDelta--
						}
					}
					if err := updateCascadeTargets(ctx, tx, command, source.kind, source.table, tombstones, storageBytes); err != nil {
						return err
					}
					if err := appendBatch(tombstones); err != nil {
						return err
					}
					afterID = targets[len(targets)-1].id
				}
			}
			return nil
		})
	if err != nil {
		return protocol.Outcome{}, err
	}
	byteDelta += journalBytes
	if err := chargeUsage(ctx, tx, command.Grant.FamilyID, countDelta, byteDelta, command.Limits, true); err != nil {
		return protocol.Outcome{}, err
	}
	return protocol.Outcome{MutationID: m.MutationID, Status: "applied", CanonicalEntity: &root,
		RelatedEntities: []protocol.CanonicalEntity{}, Cursor: &last}, nil
}

func countCascadeTargets(ctx context.Context, tx pgx.Tx, familyID, babyID string) (int, error) {
	var total int64
	err := tx.QueryRow(ctx, `SELECT 1+
		(SELECT count(*) FROM feed_entries WHERE family_id=$1 AND baby_id=$2 AND tombstoned_at IS NULL)+
		(SELECT count(*) FROM sleep_entries WHERE family_id=$1 AND baby_id=$2 AND tombstoned_at IS NULL)+
		(SELECT count(*) FROM supplement_entries WHERE family_id=$1 AND baby_id=$2 AND tombstoned_at IS NULL)+
 (SELECT count(*) FROM diaper_entries WHERE family_id=$1 AND baby_id=$2 AND tombstoned_at IS NULL)+
		(SELECT count(*) FROM growth_profiles WHERE family_id=$1 AND baby_id=$2 AND tombstoned_at IS NULL)+
		(SELECT count(*) FROM measurement_entries WHERE family_id=$1 AND baby_id=$2 AND tombstoned_at IS NULL)+
		(SELECT count(*) FROM active_timers WHERE family_id=$1 AND baby_id=$2 AND tombstoned_at IS NULL)`, familyID, babyID).Scan(&total)
	if err != nil {
		return 0, err
	}
	if total < 1 || total > int64(^uint(0)>>1) {
		return 0, fmt.Errorf("invalid child cascade size")
	}
	return int(total), nil
}

func loadCascadeTargets(ctx context.Context, tx pgx.Tx, familyID, babyID, table, afterID string) ([]cascadeTarget, error) {
	rows, err := tx.Query(ctx, `SELECT id::text,entity_version,creator_account_id::text,storage_bytes FROM `+table+`
		WHERE family_id=$1 AND baby_id=$2 AND tombstoned_at IS NULL AND id>$3 ORDER BY id LIMIT $4`,
		familyID, babyID, afterID, childDeleteBatchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := make([]cascadeTarget, 0, childDeleteBatchSize)
	for rows.Next() {
		var target cascadeTarget
		if err := rows.Scan(&target.id, &target.version, &target.creator, &target.oldBytes); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

func cascadeTombstone(command ApplyCommand, template protocol.EntityMeta, kind string, target cascadeTarget) (protocol.CanonicalEntity, int64, error) {
	meta := protocol.EntityMeta{ID: target.id, FamilyID: command.Grant.FamilyID, EntityVersion: target.version + 1,
		CreatorAccountID: target.creator, LastEditorAccountID: command.Grant.AccountID,
		ClientMutationTime: template.ClientMutationTime, AcceptedAt: template.AcceptedAt, TombstonedAt: &template.AcceptedAt}
	entity := protocol.CanonicalEntity{EntityKind: kind, Meta: meta, Payload: nil}
	bytes, err := EntityStorageBytes(entity)
	return entity, bytes, err
}

func updateCascadeTargets(ctx context.Context, tx pgx.Tx, command ApplyCommand, kind, table string,
	entities []protocol.CanonicalEntity, storageBytes []int64) error {
	ids := make([]string, len(entities))
	versions := make([]int64, len(entities))
	for index, entity := range entities {
		ids[index], versions[index] = entity.Meta.ID, int64(entity.Meta.EntityVersion)
	}
	// A tombstone keeps no parts, as putTombstone leaves it.
	switch kind {
	case "feed":
		if _, err := tx.Exec(ctx, `DELETE FROM feed_components WHERE family_id=$1 AND feed_id=ANY($2::uuid[])`, command.Grant.FamilyID, ids); err != nil {
			return err
		}
	case "timer":
		if _, err := tx.Exec(ctx, `DELETE FROM timer_components WHERE family_id=$1 AND timer_id=ANY($2::uuid[])`, command.Grant.FamilyID, ids); err != nil {
			return err
		}
	}
	result, err := tx.Exec(ctx, `WITH input AS (
		SELECT * FROM unnest($2::uuid[],$3::bigint[],$4::bigint[]) AS value(id,entity_version,storage_bytes)
	) UPDATE `+table+` target SET entity_version=input.entity_version,last_editor_account_id=$5,
		client_mutation_time=$6,accepted_at=$7,tombstoned_at=$7,last_sequence=0,storage_bytes=input.storage_bytes
		FROM input WHERE target.family_id=$1 AND target.id=input.id AND target.tombstoned_at IS NULL`,
		command.Grant.FamilyID, ids, versions, storageBytes, command.Grant.AccountID,
		entities[0].Meta.ClientMutationTime, entities[0].Meta.AcceptedAt)
	if err != nil {
		return err
	}
	if result.RowsAffected() != int64(len(entities)) {
		return fmt.Errorf("child cascade target changed")
	}
	return nil
}
