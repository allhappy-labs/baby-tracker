package records

import (
	"context"
	"errors"

	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

func (r *Repository) GetFeed(ctx context.Context, tx pgx.Tx, familyID, id string) (protocol.FeedPayload, protocol.EntityMeta, error) {
	var p protocol.FeedPayload
	var m protocol.EntityMeta
	var tomb, ended, note *string
	err := tx.QueryRow(ctx, `SELECT id::text,family_id::text,entity_version,creator_account_id::text,last_editor_account_id::text,
		to_char(client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),to_char(accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
		CASE WHEN tombstoned_at IS NULL THEN NULL ELSE to_char(tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
		baby_id::text,kind,to_char(started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
		CASE WHEN ended_at IS NULL THEN NULL ELSE to_char(ended_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
		time_zone_id,note,feeding_session_id::text FROM feed_entries WHERE family_id=$1 AND id=$2`, familyID, id).Scan(
		&m.ID, &m.FamilyID, &m.EntityVersion, &m.CreatorAccountID, &m.LastEditorAccountID, &m.ClientMutationTime, &m.AcceptedAt, &tomb,
		&p.BabyID, &p.Kind, &p.StartedAt, &ended, &p.TimeZoneID, &note, &p.FeedingSessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, m, ErrNotFound
	}
	if err != nil {
		return p, m, err
	}
	m.TombstonedAt = tomb
	p.EndedAt = ended
	p.Note = note
	components, err := tx.Query(ctx, `SELECT component_id::text,kind,side,seconds,volume_ml::text,contents,foods,amount,
		CASE WHEN started_at IS NULL THEN NULL ELSE to_char(started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
		CASE WHEN ended_at IS NULL THEN NULL ELSE to_char(ended_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END
		FROM feed_components WHERE family_id=$1 AND feed_id=$2 ORDER BY position`, familyID, id)
	if err != nil {
		return p, m, err
	}
	p.Components = []protocol.FeedComponent{}
	for components.Next() {
		var component protocol.FeedComponent
		var volume *string
		if err = components.Scan(&component.ID, &component.Kind, &component.Side, &component.Seconds, &volume, &component.Contents, &component.Foods, &component.Amount, &component.StartedAt, &component.EndedAt); err != nil {
			components.Close()
			return p, m, err
		}
		if volume != nil {
			component.VolumeML, err = protocol.ParseDecimal(*volume)
			if err != nil {
				components.Close()
				return p, m, err
			}
		}
		p.Components = append(p.Components, component)
	}
	err = components.Err()
	components.Close()
	return p, m, err
}

func (r *Repository) PutFeed(ctx context.Context, tx pgx.Tx, m protocol.EntityMeta, p protocol.FeedPayload) (protocol.CanonicalEntity, error) {
	e := protocol.CanonicalEntity{EntityKind: "feed", Meta: m, Payload: p}
	b, err := EntityStorageBytes(e)
	if err != nil {
		return e, err
	}
	return r.putFeed(ctx, tx, m, p, b)
}

// putFeed writes a feed and replaces its parts. component_version is pinned
// to the current version by the schema, so it is neither written nor read.
func (r *Repository) putFeed(ctx context.Context, tx pgx.Tx, m protocol.EntityMeta, p protocol.FeedPayload, b int64) (protocol.CanonicalEntity, error) {
	_, err := tx.Exec(ctx, `INSERT INTO feed_entries(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,kind,started_at,ended_at,time_zone_id,note,feeding_session_id)
	VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,0,$10,$11,$12,$13,$14,$15,$16)
	ON CONFLICT(family_id,id) DO UPDATE SET baby_id=EXCLUDED.baby_id,entity_version=EXCLUDED.entity_version,last_editor_account_id=EXCLUDED.last_editor_account_id,client_mutation_time=EXCLUDED.client_mutation_time,accepted_at=EXCLUDED.accepted_at,tombstoned_at=EXCLUDED.tombstoned_at,last_sequence=0,storage_bytes=EXCLUDED.storage_bytes,kind=EXCLUDED.kind,started_at=EXCLUDED.started_at,ended_at=EXCLUDED.ended_at,time_zone_id=EXCLUDED.time_zone_id,note=EXCLUDED.note,feeding_session_id=EXCLUDED.feeding_session_id`, m.FamilyID, m.ID, p.BabyID, m.EntityVersion, m.CreatorAccountID, m.LastEditorAccountID, m.ClientMutationTime, m.AcceptedAt, m.TombstonedAt, b, p.Kind, p.StartedAt, p.EndedAt, p.TimeZoneID, p.Note, p.FeedingSessionID)
	if err != nil {
		return protocol.CanonicalEntity{}, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM feed_components WHERE family_id=$1 AND feed_id=$2`, m.FamilyID, m.ID); err != nil {
		return protocol.CanonicalEntity{}, err
	}
	for i, component := range p.Components {
		var volume any
		if component.VolumeML != nil {
			volume = component.VolumeML.String()
		}
		if _, err = tx.Exec(ctx, `INSERT INTO feed_components(family_id,feed_id,component_id,position,kind,side,seconds,volume_ml,contents,foods,amount,started_at,ended_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, m.FamilyID, m.ID, component.ID, i, component.Kind, component.Side, component.Seconds, volume, component.Contents, component.Foods, component.Amount, component.StartedAt, component.EndedAt); err != nil {
			return protocol.CanonicalEntity{}, err
		}
	}
	return protocol.CanonicalEntity{EntityKind: "feed", Meta: m, Payload: p}, nil
}
