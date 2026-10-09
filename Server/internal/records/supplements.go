package records

import (
	"context"
	"errors"

	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

func (r *Repository) GetSupplement(ctx context.Context, tx pgx.Tx, familyID, id string) (protocol.SupplementPayload, protocol.EntityMeta, error) {
	var p protocol.SupplementPayload
	var m protocol.EntityMeta
	var tomb, note *string
	err := tx.QueryRow(ctx, `SELECT id::text,family_id::text,entity_version,creator_account_id::text,last_editor_account_id::text,
	to_char(client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),to_char(accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
	CASE WHEN tombstoned_at IS NULL THEN NULL ELSE to_char(tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
	baby_id::text,to_char(started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),time_zone_id,note,name,to_char(logged_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"')
	FROM supplement_entries WHERE family_id=$1 AND id=$2`, familyID, id).Scan(&m.ID, &m.FamilyID, &m.EntityVersion, &m.CreatorAccountID, &m.LastEditorAccountID, &m.ClientMutationTime, &m.AcceptedAt, &tomb, &p.BabyID, &p.StartedAt, &p.TimeZoneID, &note, &p.Name, &p.LoggedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, m, ErrNotFound
	}
	m.TombstonedAt = tomb
	p.Note = note
	return p, m, err
}
func (r *Repository) PutSupplement(ctx context.Context, tx pgx.Tx, m protocol.EntityMeta, p protocol.SupplementPayload) (protocol.CanonicalEntity, error) {
	e := protocol.CanonicalEntity{EntityKind: "supplement", Meta: m, Payload: p}
	b, err := EntityStorageBytes(e)
	if err != nil {
		return e, err
	}
	return r.putSupplement(ctx, tx, m, p, b)
}
func (r *Repository) putSupplement(ctx context.Context, tx pgx.Tx, m protocol.EntityMeta, p protocol.SupplementPayload, b int64) (protocol.CanonicalEntity, error) {
	result, err := tx.Exec(ctx, `INSERT INTO supplement_entries(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,started_at,time_zone_id,note,name,logged_at)
	VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,0,$10,$11,$12,$13,$14,$15) ON CONFLICT(family_id,id) DO UPDATE SET baby_id=EXCLUDED.baby_id,entity_version=EXCLUDED.entity_version,last_editor_account_id=EXCLUDED.last_editor_account_id,client_mutation_time=EXCLUDED.client_mutation_time,accepted_at=EXCLUDED.accepted_at,tombstoned_at=EXCLUDED.tombstoned_at,last_sequence=0,storage_bytes=EXCLUDED.storage_bytes,started_at=EXCLUDED.started_at,time_zone_id=EXCLUDED.time_zone_id,note=EXCLUDED.note,name=EXCLUDED.name WHERE supplement_entries.logged_at=EXCLUDED.logged_at`, m.FamilyID, m.ID, p.BabyID, m.EntityVersion, m.CreatorAccountID, m.LastEditorAccountID, m.ClientMutationTime, m.AcceptedAt, m.TombstonedAt, b, p.StartedAt, p.TimeZoneID, p.Note, p.Name, p.LoggedAt)
	if err == nil && result.RowsAffected() != 1 {
		err = protocol.ErrInvalidMutation
	}
	return protocol.CanonicalEntity{EntityKind: "supplement", Meta: m, Payload: p}, err
}
