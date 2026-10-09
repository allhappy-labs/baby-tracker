package records

import (
	"context"
	"errors"

	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

func (r *Repository) GetMeasurement(ctx context.Context, tx pgx.Tx, familyID, id string) (protocol.MeasurementPayload, protocol.EntityMeta, error) {
	var p protocol.MeasurementPayload
	var m protocol.EntityMeta
	var tomb *string
	var weight, length, head *string
	err := tx.QueryRow(ctx, `SELECT id::text,family_id::text,entity_version,creator_account_id::text,last_editor_account_id::text,
	to_char(client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),to_char(accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
	CASE WHEN tombstoned_at IS NULL THEN NULL ELSE to_char(tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
	baby_id::text,to_char(measured_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),time_zone_id,to_char(logged_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),weight_kg::text,length_cm::text,head_circumference_cm::text,length_position
	FROM measurement_entries WHERE family_id=$1 AND id=$2`, familyID, id).Scan(&m.ID, &m.FamilyID, &m.EntityVersion, &m.CreatorAccountID, &m.LastEditorAccountID, &m.ClientMutationTime, &m.AcceptedAt, &tomb, &p.BabyID, &p.MeasuredAt, &p.TimeZoneID, &p.LoggedAt, &weight, &length, &head, &p.LengthPosition)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, m, ErrNotFound
	}
	m.TombstonedAt = tomb
	if err != nil {
		return p, m, err
	}
	for _, v := range []struct {
		raw    *string
		target **protocol.Decimal
	}{{weight, &p.WeightKG}, {length, &p.LengthCM}, {head, &p.HeadCircumferenceCM}} {
		if v.raw != nil {
			*v.target, err = protocol.ParseDecimal(*v.raw)
			if err != nil {
				return p, m, err
			}
		}
	}
	return p, m, err
}
func (r *Repository) PutMeasurement(ctx context.Context, tx pgx.Tx, m protocol.EntityMeta, p protocol.MeasurementPayload) (protocol.CanonicalEntity, error) {
	e := protocol.CanonicalEntity{EntityKind: "measurement", Meta: m, Payload: p}
	b, err := EntityStorageBytes(e)
	if err != nil {
		return e, err
	}
	return r.putMeasurement(ctx, tx, m, p, b)
}
func (r *Repository) putMeasurement(ctx context.Context, tx pgx.Tx, m protocol.EntityMeta, p protocol.MeasurementPayload, b int64) (protocol.CanonicalEntity, error) {
	result, err := tx.Exec(ctx, `INSERT INTO measurement_entries(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,measured_at,time_zone_id,logged_at,weight_kg,length_cm,head_circumference_cm,length_position)
	VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,0,$10,$11,$12,$13,$14,$15,$16,$17) ON CONFLICT(family_id,id) DO UPDATE SET baby_id=EXCLUDED.baby_id,entity_version=EXCLUDED.entity_version,last_editor_account_id=EXCLUDED.last_editor_account_id,client_mutation_time=EXCLUDED.client_mutation_time,accepted_at=EXCLUDED.accepted_at,tombstoned_at=EXCLUDED.tombstoned_at,last_sequence=0,storage_bytes=EXCLUDED.storage_bytes,measured_at=EXCLUDED.measured_at,time_zone_id=EXCLUDED.time_zone_id,weight_kg=EXCLUDED.weight_kg,length_cm=EXCLUDED.length_cm,head_circumference_cm=EXCLUDED.head_circumference_cm,length_position=EXCLUDED.length_position WHERE measurement_entries.logged_at=EXCLUDED.logged_at`, m.FamilyID, m.ID, p.BabyID, m.EntityVersion, m.CreatorAccountID, m.LastEditorAccountID, m.ClientMutationTime, m.AcceptedAt, m.TombstonedAt, b, p.MeasuredAt, p.TimeZoneID, p.LoggedAt, decimalSQL(p.WeightKG), decimalSQL(p.LengthCM), decimalSQL(p.HeadCircumferenceCM), p.LengthPosition)
	if err == nil && result.RowsAffected() != 1 {
		err = protocol.ErrInvalidMutation
	}
	return protocol.CanonicalEntity{EntityKind: "measurement", Meta: m, Payload: p}, err
}

func decimalSQL(value *protocol.Decimal) any {
	if value == nil {
		return nil
	}
	return value.String()
}
