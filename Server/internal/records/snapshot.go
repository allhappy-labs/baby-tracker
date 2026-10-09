package records

import (
	"context"
	"encoding/json"

	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

// StreamCanonical reads typed source-of-truth tables in deterministic kind/ID
// order and invokes visit once per row. PostgreSQL streams the union; callers
// need not retain the family snapshot in process memory.
func (r *Repository) StreamCanonical(ctx context.Context, tx pgx.Tx, familyID string, visit func(protocol.CanonicalEntity) error) error {
	rows, err := tx.Query(ctx, `SELECT canonical_entity FROM (`+snapshotEntitiesUnion+`) typed ORDER BY kind_order,id`, familyID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		var entity protocol.CanonicalEntity
		if err := json.Unmarshal(raw, &entity); err != nil {
			return err
		}
		if err := visit(entity); err != nil {
			return err
		}
	}
	return rows.Err()
}

// SnapshotBatch returns one bounded deterministic slice so the caller can
// close the read portal before inserting staged rows on the same transaction.
func (r *Repository) SnapshotBatch(ctx context.Context, tx pgx.Tx, familyID string, afterKind int, afterID string, limit int) ([]protocol.CanonicalEntity, error) {
	rows, err := tx.Query(ctx, `SELECT canonical_entity FROM (`+snapshotEntitiesUnion+`) typed
 WHERE (kind_order,id) > ($2,$3::uuid) ORDER BY kind_order,id LIMIT $4`, familyID, afterKind, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]protocol.CanonicalEntity, 0, limit)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var entity protocol.CanonicalEntity
		if err := json.Unmarshal(raw, &entity); err != nil {
			return nil, err
		}
		result = append(result, entity)
	}
	return result, rows.Err()
}

const snapshotEntitiesUnion = `
 SELECT 1 kind_order,b.id,
  jsonb_build_object('entity_kind','baby','meta',jsonb_build_object(
   'id',b.id::text,'family_id',b.family_id::text,'entity_version',b.entity_version,
   'creator_account_id',b.creator_account_id::text,'last_editor_account_id',b.last_editor_account_id::text,
   'client_mutation_time',to_char(b.client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'accepted_at',to_char(b.accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'tombstoned_at',CASE WHEN b.tombstoned_at IS NULL THEN NULL ELSE to_char(b.tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END),
   'payload',CASE WHEN b.tombstoned_at IS NOT NULL THEN NULL ELSE jsonb_strip_nulls(jsonb_build_object(
    'name',b.name,'birth_date',b.birth_date::text,'sort_order',b.sort_order,'icon',b.icon)) END) canonical_entity
 FROM babies b WHERE b.family_id=$1
 UNION ALL
 SELECT 2,f.id,
  jsonb_build_object('entity_kind','feed','meta',jsonb_build_object(
   'id',f.id::text,'family_id',f.family_id::text,'entity_version',f.entity_version,
   'creator_account_id',f.creator_account_id::text,'last_editor_account_id',f.last_editor_account_id::text,
   'client_mutation_time',to_char(f.client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'accepted_at',to_char(f.accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'tombstoned_at',CASE WHEN f.tombstoned_at IS NULL THEN NULL ELSE to_char(f.tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END),
   'payload',CASE WHEN f.tombstoned_at IS NOT NULL THEN NULL ELSE jsonb_build_object(
    'baby_id',f.baby_id::text,'kind',f.kind,
    'started_at',to_char(f.started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
    'ended_at',CASE WHEN f.ended_at IS NULL THEN NULL ELSE to_char(f.ended_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
    'time_zone_id',f.time_zone_id,'note',f.note) || jsonb_build_object(
    'components',COALESCE((SELECT jsonb_agg(jsonb_strip_nulls(jsonb_build_object(
      'id',c.component_id::text,'kind',c.kind,'side',c.side,'seconds',c.seconds,
      'volume_ml',c.volume_ml,'contents',c.contents,'foods',c.foods,'amount',c.amount,
      'started_at',CASE WHEN c.started_at IS NULL THEN NULL ELSE to_char(c.started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
      'ended_at',CASE WHEN c.ended_at IS NULL THEN NULL ELSE to_char(c.ended_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END)) ORDER BY c.position)
      FROM feed_components c WHERE c.family_id=f.family_id AND c.feed_id=f.id),'[]'::jsonb),
    'feeding_session_id',f.feeding_session_id::text) END) canonical_entity
 FROM feed_entries f WHERE f.family_id=$1
 UNION ALL
 SELECT 3,s.id,
  jsonb_build_object('entity_kind','sleep','meta',jsonb_build_object(
   'id',s.id::text,'family_id',s.family_id::text,'entity_version',s.entity_version,
   'creator_account_id',s.creator_account_id::text,'last_editor_account_id',s.last_editor_account_id::text,
   'client_mutation_time',to_char(s.client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'accepted_at',to_char(s.accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'tombstoned_at',CASE WHEN s.tombstoned_at IS NULL THEN NULL ELSE to_char(s.tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END),
   'payload',CASE WHEN s.tombstoned_at IS NOT NULL THEN NULL ELSE jsonb_build_object(
    'baby_id',s.baby_id::text,'started_at',to_char(s.started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
    'ended_at',CASE WHEN s.ended_at IS NULL THEN NULL ELSE to_char(s.ended_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
    'time_zone_id',s.time_zone_id,'note',s.note,'kind_override',s.kind_override,'sleep_session_id',s.sleep_session_id::text) END) canonical_entity
 FROM sleep_entries s WHERE s.family_id=$1
 UNION ALL
 SELECT 4,d.id,
  jsonb_build_object('entity_kind','diaper','meta',jsonb_build_object(
   'id',d.id::text,'family_id',d.family_id::text,'entity_version',d.entity_version,
   'creator_account_id',d.creator_account_id::text,'last_editor_account_id',d.last_editor_account_id::text,
   'client_mutation_time',to_char(d.client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'accepted_at',to_char(d.accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'tombstoned_at',CASE WHEN d.tombstoned_at IS NULL THEN NULL ELSE to_char(d.tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END),
   'payload',CASE WHEN d.tombstoned_at IS NOT NULL THEN NULL ELSE jsonb_build_object(
    'baby_id',d.baby_id::text,'started_at',to_char(d.started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
    'ended_at',CASE WHEN d.ended_at IS NULL THEN NULL ELSE to_char(d.ended_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
    'time_zone_id',d.time_zone_id,'note',d.note,'contents',d.contents,'consistency',d.consistency,'color',d.color) END) canonical_entity
 FROM diaper_entries d WHERE d.family_id=$1
 UNION ALL
 SELECT 6,d.id,
  jsonb_build_object('entity_kind','supplement','meta',jsonb_build_object(
   'id',d.id::text,'family_id',d.family_id::text,'entity_version',d.entity_version,
   'creator_account_id',d.creator_account_id::text,'last_editor_account_id',d.last_editor_account_id::text,
   'client_mutation_time',to_char(d.client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'accepted_at',to_char(d.accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'tombstoned_at',CASE WHEN d.tombstoned_at IS NULL THEN NULL ELSE to_char(d.tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END),
   'payload',CASE WHEN d.tombstoned_at IS NOT NULL THEN NULL ELSE jsonb_build_object(
    'baby_id',d.baby_id::text,'started_at',to_char(d.started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
    'time_zone_id',d.time_zone_id,'note',d.note,'name',d.name,'logged_at',to_char(d.logged_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"')) END) canonical_entity
 FROM supplement_entries d WHERE d.family_id=$1
 UNION ALL
 SELECT 5,t.id,
  jsonb_build_object('entity_kind','timer','meta',jsonb_build_object(
   'id',t.id::text,'family_id',t.family_id::text,'entity_version',t.entity_version,
   'creator_account_id',t.creator_account_id::text,'last_editor_account_id',t.last_editor_account_id::text,
   'client_mutation_time',to_char(t.client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'accepted_at',to_char(t.accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'tombstoned_at',CASE WHEN t.tombstoned_at IS NULL THEN NULL ELSE to_char(t.tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END),
   'payload',CASE WHEN t.tombstoned_at IS NOT NULL THEN NULL ELSE jsonb_build_object(
    'baby_id',t.baby_id::text,'timer_kind',t.timer_kind,
    'started_at',to_char(t.started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
    'time_zone_id',t.time_zone_id,
    'current_side',t.current_side,
    'current_side_started_at',CASE WHEN t.current_side_started_at IS NULL THEN NULL ELSE to_char(t.current_side_started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
    'current_bottle_started_at',CASE WHEN t.current_bottle_started_at IS NULL THEN NULL ELSE to_char(t.current_bottle_started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END) || jsonb_build_object(
    'completed_components',COALESCE((SELECT jsonb_agg(jsonb_strip_nulls(jsonb_build_object(
      'id',c.component_id::text,'kind',c.kind,'side',c.side,'seconds',c.seconds,
      'volume_ml',c.volume_ml,'contents',c.contents,'foods',c.foods,'amount',c.amount,
      'started_at',CASE WHEN c.started_at IS NULL THEN NULL ELSE to_char(c.started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
      'ended_at',CASE WHEN c.ended_at IS NULL THEN NULL ELSE to_char(c.ended_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END)) ORDER BY c.position)
      FROM timer_components c WHERE c.family_id=t.family_id AND c.timer_id=t.id),'[]'::jsonb),
    'session_id',t.session_id::text) END) canonical_entity
 FROM active_timers t WHERE t.family_id=$1
 UNION ALL
 SELECT 7,d.id,
  jsonb_build_object('entity_kind','growth_profile','meta',jsonb_build_object(
   'id',d.id::text,'family_id',d.family_id::text,'entity_version',d.entity_version,
   'creator_account_id',d.creator_account_id::text,'last_editor_account_id',d.last_editor_account_id::text,
   'client_mutation_time',to_char(d.client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'accepted_at',to_char(d.accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'tombstoned_at',CASE WHEN d.tombstoned_at IS NULL THEN NULL ELSE to_char(d.tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END),
   'payload',CASE WHEN d.tombstoned_at IS NOT NULL THEN NULL ELSE jsonb_build_object(
    'baby_id',d.baby_id::text,'reference_sex',d.reference_sex,'expected_due_date',d.expected_due_date::text) END) canonical_entity
 FROM growth_profiles d WHERE d.family_id=$1
 UNION ALL
 SELECT 8,d.id,
  jsonb_build_object('entity_kind','measurement','meta',jsonb_build_object(
   'id',d.id::text,'family_id',d.family_id::text,'entity_version',d.entity_version,
   'creator_account_id',d.creator_account_id::text,'last_editor_account_id',d.last_editor_account_id::text,
   'client_mutation_time',to_char(d.client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'accepted_at',to_char(d.accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'tombstoned_at',CASE WHEN d.tombstoned_at IS NULL THEN NULL ELSE to_char(d.tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END),
   'payload',CASE WHEN d.tombstoned_at IS NOT NULL THEN NULL ELSE jsonb_build_object(
    'baby_id',d.baby_id::text,'measured_at',to_char(d.measured_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),'logged_at',to_char(d.logged_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),'time_zone_id',d.time_zone_id,'weight_kg',d.weight_kg,'length_cm',d.length_cm,'head_circumference_cm',d.head_circumference_cm,'length_position',d.length_position) END) canonical_entity
 FROM measurement_entries d WHERE d.family_id=$1
`
