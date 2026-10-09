package records

import (
	"context"
	"errors"

	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

func (r *Repository) GetDiaper(ctx context.Context, tx pgx.Tx, familyID, id string) (protocol.DiaperPayload, protocol.EntityMeta, error) {
	var p protocol.DiaperPayload
	var m protocol.EntityMeta
	var tomb, ended, note, consistency, color *string
	err := tx.QueryRow(ctx, `SELECT id::text,family_id::text,entity_version,creator_account_id::text,last_editor_account_id::text,
	to_char(client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),to_char(accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
	CASE WHEN tombstoned_at IS NULL THEN NULL ELSE to_char(tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
	baby_id::text,to_char(started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),CASE WHEN ended_at IS NULL THEN NULL ELSE to_char(ended_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,time_zone_id,note,contents,consistency,color
	FROM diaper_entries WHERE family_id=$1 AND id=$2`, familyID, id).Scan(&m.ID, &m.FamilyID, &m.EntityVersion, &m.CreatorAccountID, &m.LastEditorAccountID, &m.ClientMutationTime, &m.AcceptedAt, &tomb, &p.BabyID, &p.StartedAt, &ended, &p.TimeZoneID, &note, &p.Contents, &consistency, &color)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, m, ErrNotFound
	}
	m.TombstonedAt = tomb
	p.EndedAt = ended
	p.Note = note
	p.Consistency = consistency
	p.Color = color
	return p, m, err
}
func (r *Repository) PutDiaper(ctx context.Context, tx pgx.Tx, m protocol.EntityMeta, p protocol.DiaperPayload) (protocol.CanonicalEntity, error) {
	e := protocol.CanonicalEntity{EntityKind: "diaper", Meta: m, Payload: p}
	b, err := EntityStorageBytes(e)
	if err != nil {
		return e, err
	}
	return r.putDiaper(ctx, tx, m, p, b)
}
func (r *Repository) putDiaper(ctx context.Context, tx pgx.Tx, m protocol.EntityMeta, p protocol.DiaperPayload, b int64) (protocol.CanonicalEntity, error) {
	_, err := tx.Exec(ctx, `INSERT INTO diaper_entries(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,started_at,ended_at,time_zone_id,note,contents,consistency,color)
	VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,0,$10,$11,$12,$13,$14,$15,$16,$17) ON CONFLICT(family_id,id) DO UPDATE SET baby_id=EXCLUDED.baby_id,entity_version=EXCLUDED.entity_version,last_editor_account_id=EXCLUDED.last_editor_account_id,client_mutation_time=EXCLUDED.client_mutation_time,accepted_at=EXCLUDED.accepted_at,tombstoned_at=EXCLUDED.tombstoned_at,last_sequence=0,storage_bytes=EXCLUDED.storage_bytes,started_at=EXCLUDED.started_at,ended_at=EXCLUDED.ended_at,time_zone_id=EXCLUDED.time_zone_id,note=EXCLUDED.note,contents=EXCLUDED.contents,consistency=EXCLUDED.consistency,color=EXCLUDED.color`, m.FamilyID, m.ID, p.BabyID, m.EntityVersion, m.CreatorAccountID, m.LastEditorAccountID, m.ClientMutationTime, m.AcceptedAt, m.TombstonedAt, b, p.StartedAt, p.EndedAt, p.TimeZoneID, p.Note, p.Contents, p.Consistency, p.Color)
	return protocol.CanonicalEntity{EntityKind: "diaper", Meta: m, Payload: p}, err
}
