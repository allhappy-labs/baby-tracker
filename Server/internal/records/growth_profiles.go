package records

import (
	"context"
	"errors"

	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

func (r *Repository) GetGrowthProfile(ctx context.Context, tx pgx.Tx, familyID, id string) (protocol.GrowthProfilePayload, protocol.EntityMeta, error) {
	var p protocol.GrowthProfilePayload
	var m protocol.EntityMeta
	var tomb *string
	err := tx.QueryRow(ctx, `SELECT id::text,family_id::text,entity_version,creator_account_id::text,last_editor_account_id::text,
	to_char(client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),to_char(accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
	CASE WHEN tombstoned_at IS NULL THEN NULL ELSE to_char(tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
	baby_id::text,reference_sex,expected_due_date::text
	FROM growth_profiles WHERE family_id=$1 AND id=$2`, familyID, id).Scan(&m.ID, &m.FamilyID, &m.EntityVersion, &m.CreatorAccountID, &m.LastEditorAccountID, &m.ClientMutationTime, &m.AcceptedAt, &tomb, &p.BabyID, &p.ReferenceSex, &p.ExpectedDueDate)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, m, ErrNotFound
	}
	m.TombstonedAt = tomb

	return p, m, err
}
func (r *Repository) PutGrowthProfile(ctx context.Context, tx pgx.Tx, m protocol.EntityMeta, p protocol.GrowthProfilePayload) (protocol.CanonicalEntity, error) {
	e := protocol.CanonicalEntity{EntityKind: "growth_profile", Meta: m, Payload: p}
	b, err := EntityStorageBytes(e)
	if err != nil {
		return e, err
	}
	return r.putGrowthProfile(ctx, tx, m, p, b)
}
func (r *Repository) putGrowthProfile(ctx context.Context, tx pgx.Tx, m protocol.EntityMeta, p protocol.GrowthProfilePayload, b int64) (protocol.CanonicalEntity, error) {
	result, err := tx.Exec(ctx, `INSERT INTO growth_profiles(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,reference_sex,expected_due_date)
	VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,0,$10,$11,$12) ON CONFLICT(family_id,id) DO UPDATE SET baby_id=EXCLUDED.baby_id,entity_version=EXCLUDED.entity_version,last_editor_account_id=EXCLUDED.last_editor_account_id,client_mutation_time=EXCLUDED.client_mutation_time,accepted_at=EXCLUDED.accepted_at,tombstoned_at=EXCLUDED.tombstoned_at,last_sequence=0,storage_bytes=EXCLUDED.storage_bytes,reference_sex=EXCLUDED.reference_sex,expected_due_date=EXCLUDED.expected_due_date`, m.FamilyID, m.ID, p.BabyID, m.EntityVersion, m.CreatorAccountID, m.LastEditorAccountID, m.ClientMutationTime, m.AcceptedAt, m.TombstonedAt, b, p.ReferenceSex, p.ExpectedDueDate)
	if err == nil && result.RowsAffected() != 1 {
		err = protocol.ErrInvalidMutation
	}
	return protocol.CanonicalEntity{EntityKind: "growth_profile", Meta: m, Payload: p}, err
}
