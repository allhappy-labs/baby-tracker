package records

import (
	"context"
	"errors"

	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

func (r *Repository) GetBaby(ctx context.Context, tx pgx.Tx, familyID, id string) (protocol.BabyPayload, protocol.EntityMeta, error) {
	var payload protocol.BabyPayload
	var meta protocol.EntityMeta
	var tombstone *string
	err := tx.QueryRow(ctx, `SELECT id::text,family_id::text,entity_version,creator_account_id::text,last_editor_account_id::text,
		to_char(client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
		to_char(accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
		CASE WHEN tombstoned_at IS NULL THEN NULL ELSE to_char(tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
		name,birth_date::text,sort_order,icon FROM babies WHERE family_id=$1 AND id=$2`, familyID, id).Scan(
		&meta.ID, &meta.FamilyID, &meta.EntityVersion, &meta.CreatorAccountID, &meta.LastEditorAccountID, &meta.ClientMutationTime,
		&meta.AcceptedAt, &tombstone, &payload.Name, &payload.BirthDate, &payload.SortOrder, &payload.Icon)
	if errors.Is(err, pgx.ErrNoRows) {
		return payload, meta, ErrNotFound
	}
	meta.TombstonedAt = tombstone
	return payload, meta, err
}

func (r *Repository) PutBaby(ctx context.Context, tx pgx.Tx, meta protocol.EntityMeta, payload protocol.BabyPayload) (protocol.CanonicalEntity, error) {
	entity := protocol.CanonicalEntity{EntityKind: "baby", Meta: meta, Payload: payload}
	bytes, err := EntityStorageBytes(entity)
	if err != nil {
		return entity, err
	}
	return r.putBaby(ctx, tx, meta, payload, bytes)
}

func (r *Repository) putBaby(ctx context.Context, tx pgx.Tx, meta protocol.EntityMeta, payload protocol.BabyPayload, bytes int64) (protocol.CanonicalEntity, error) {
	normalized, err := protocol.NormalizeBabyName(payload.Name)
	if err != nil {
		return protocol.CanonicalEntity{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO babies(family_id,id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,
		accepted_at,tombstoned_at,last_sequence,storage_bytes,name,normalized_name,normalization_version,birth_date,sort_order,icon)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,0,$9,$10,$11,'nfc-trim-fold-nfc-v1',$12,$13,$14)
		ON CONFLICT(family_id,id) DO UPDATE SET entity_version=EXCLUDED.entity_version,last_editor_account_id=EXCLUDED.last_editor_account_id,
		client_mutation_time=EXCLUDED.client_mutation_time,accepted_at=EXCLUDED.accepted_at,tombstoned_at=EXCLUDED.tombstoned_at,
		last_sequence=0,storage_bytes=EXCLUDED.storage_bytes,name=EXCLUDED.name,normalized_name=EXCLUDED.normalized_name,
		birth_date=EXCLUDED.birth_date,sort_order=EXCLUDED.sort_order,icon=COALESCE(EXCLUDED.icon,babies.icon)`, meta.FamilyID, meta.ID, meta.EntityVersion, meta.CreatorAccountID,
		meta.LastEditorAccountID, meta.ClientMutationTime, meta.AcceptedAt, meta.TombstonedAt, bytes, payload.Name, normalized, payload.BirthDate, payload.SortOrder, payload.Icon)
	return protocol.CanonicalEntity{EntityKind: "baby", Meta: meta, Payload: payload}, err
}
