package liveactivity

import (
	"context"
	"errors"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/records"
	"github.com/jackc/pgx/v5"
)

// CareSnapshot contains canonical records, never a second wire model. Members
// are only the latest eight nonempty feeds in the latest effective session.
// TotalSessionParts preserves the exact omitted count across bounded reads.
// EarlierAsleepSeconds is the time asleep in the completed parts of the sleep
// a running sleep timer continues: the phone's card counts the whole sleep,
// so the pushed card needs the same sum (BabyStore.earlierAsleepSeconds(of:)).
// Zero for a feed timer or a sleep that continues none.
type CareSnapshot struct {
	Baby                 protocol.CanonicalEntity
	Timers               []protocol.CanonicalEntity
	LastMilk             *protocol.CanonicalEntity
	SessionMembers       []protocol.CanonicalEntity
	TotalSessionParts    uint64
	LastBreast           *protocol.CanonicalEntity
	EarlierAsleepSeconds int64
	Cursor               protocol.Cursor
	Grant                access.Grant
}
type SnapshotReader struct{}

// The whole-second time asleep in the completed, undeleted parts of one
// sleep session: BabyStore.earlierAsleepSeconds(of:) as one aggregate on the
// sleep_entries_session index. Each part is truncated to whole seconds before
// the sum, as Swift's Int(ended - started) is, so awake gaps between parts
// never count. A sum has no rows to cap, so no sleep is too long for it;
// earlierAsleepSeconds below is the same rule in Go, for the shared fixture.
const earlierAsleepSQL = `SELECT coalesce(sum(floor(extract(epoch FROM ended_at-started_at))),0)::bigint FROM sleep_entries WHERE family_id=$1 AND baby_id=$2 AND tombstoned_at IS NULL AND ended_at IS NOT NULL AND (sleep_session_id=$3 OR (sleep_session_id IS NULL AND id=$3))`

// earlierAsleepSeconds is earlierAsleepSQL stated over canonical rows: the
// same filters (this baby, undeleted, completed, this session by link or as
// its root) and the same per-part truncation to whole seconds. The shared
// projection fixture derives CareSnapshot.EarlierAsleepSeconds with it, so one
// fixture pins the rule on the phone, in this Go and in the SQL. Parts of
// another sleep or baby are skipped rather than trusted, as feed members are.
func earlierAsleepSeconds(timer protocol.TimerStartPayload, parts []protocol.CanonicalEntity) (int64, error) {
	if timer.TimerKind != "sleep" || timer.SessionID == nil {
		return 0, nil
	}
	var total int64
	for _, e := range parts {
		p, ok := e.Payload.(protocol.SleepPayload)
		if !ok {
			return 0, errors.New("invalid sleep snapshot")
		}
		if e.Meta.TombstonedAt != nil || p.EndedAt == nil || p.BabyID != timer.BabyID {
			continue
		}
		session := e.Meta.ID
		if p.SleepSessionID != nil {
			session = *p.SleepSessionID
		}
		if session != *timer.SessionID {
			continue
		}
		started, err := time.Parse(time.RFC3339Nano, p.StartedAt)
		if err != nil {
			return 0, err
		}
		ended, err := time.Parse(time.RFC3339Nano, *p.EndedAt)
		if err != nil {
			return 0, err
		}
		// Swift's Int(TimeInterval) truncates toward zero; so does this
		// division, and floor() in the SQL (a part never ends before it starts).
		total += int64(ended.Sub(started) / time.Second)
	}
	return total, nil
}

const latestMilkSQL = `SELECT id::text FROM feed_entries WHERE family_id=$1 AND baby_id=$2 AND tombstoned_at IS NULL AND kind<>'solids' ORDER BY COALESCE(ended_at,started_at) DESC,COALESCE(feeding_session_id,id) DESC,feed_entries.id DESC LIMIT 1`
const latestBreastSQL = `SELECT id::text FROM feed_entries WHERE family_id=$1 AND baby_id=$2 AND tombstoned_at IS NULL AND kind='breast' ORDER BY started_at DESC,feed_entries.id DESC LIMIT 1`
const sessionMembersSQL = `SELECT f.id::text FROM feed_entries f WHERE f.family_id=$1 AND f.baby_id=$2 AND f.tombstoned_at IS NULL AND f.kind<>'solids' AND (f.feeding_session_id=$3 OR (f.feeding_session_id IS NULL AND f.id=$3)) AND EXISTS(SELECT 1 FROM feed_components c WHERE c.family_id=f.family_id AND c.feed_id=f.id AND c.kind IN ('breast','bottle')) ORDER BY f.started_at DESC,f.id DESC LIMIT 8`
const sessionCountSQL = `SELECT count(*) FROM feed_entries f JOIN feed_components c ON c.family_id=f.family_id AND c.feed_id=f.id WHERE f.family_id=$1 AND f.baby_id=$2 AND f.tombstoned_at IS NULL AND f.kind<>'solids' AND (f.feeding_session_id=$3 OR (f.feeding_session_id IS NULL AND f.id=$3)) AND c.kind IN ('breast','bottle')`

// Read requires the authorized repeatable-read transaction supplied by the
// LiveActivitySnapshotGuard; it neither creates nor retains that transaction.
func (SnapshotReader) Read(ctx context.Context, tx pgx.Tx, grant access.Grant, babyID string) (CareSnapshot, error) {
	s := CareSnapshot{Grant: grant}
	r := records.NewRepository()
	err := tx.QueryRow(ctx, `SELECT s.instance_id::text,s.recovery_epoch,f.id::text,f.next_sequence FROM server_state s CROSS JOIN families f WHERE f.id=$1`, grant.FamilyID).Scan(&s.Cursor.ServerInstanceID, &s.Cursor.RecoveryEpoch, &s.Cursor.FamilyID, &s.Cursor.Sequence)
	if err != nil {
		return s, err
	}
	s.Baby, _, err = r.GetCanonical(ctx, tx, "baby", grant.FamilyID, babyID)
	if errors.Is(err, records.ErrNotFound) {
		return s, nil
	}
	if err != nil || s.Baby.Meta.TombstonedAt != nil {
		return s, err
	}
	rows, err := tx.Query(ctx, `SELECT id::text FROM active_timers WHERE family_id=$1 AND baby_id=$2 AND tombstoned_at IS NULL ORDER BY timer_kind`, grant.FamilyID, babyID)
	if err != nil {
		return s, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return s, err
	}
	for _, id := range ids {
		timer, e := r.GetTimerSnapshot(ctx, tx, grant.FamilyID, id)
		if e != nil {
			return s, e
		}
		s.Timers = append(s.Timers, timer.Canonical())
		// A running sleep's card counts the sleep it continues; one session,
		// summed in this same transaction so the parts and the timer agree.
		if timer.Payload.TimerKind != "sleep" || timer.Payload.SessionID == nil {
			continue
		}
		if e := tx.QueryRow(ctx, earlierAsleepSQL, grant.FamilyID, babyID, *timer.Payload.SessionID).Scan(&s.EarlierAsleepSeconds); e != nil {
			return s, e
		}
	}
	if len(s.Timers) > 0 {
		return s, nil
	}
	getLatest := func(query string) (*protocol.CanonicalEntity, error) {
		var id string
		err := tx.QueryRow(ctx, query, grant.FamilyID, babyID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		e, _, err := r.GetCanonical(ctx, tx, "feed", grant.FamilyID, id)
		return &e, err
	}
	s.LastMilk, err = getLatest(latestMilkSQL)
	if err != nil || s.LastMilk == nil {
		return s, err
	}
	session := effectiveSession(*s.LastMilk)
	if err = tx.QueryRow(ctx, sessionCountSQL, grant.FamilyID, babyID, session).Scan(&s.TotalSessionParts); err != nil {
		return s, err
	}
	rows, err = tx.Query(ctx, sessionMembersSQL, grant.FamilyID, babyID, session)
	if err != nil {
		return s, err
	}
	ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return s, err
	}
	for _, id := range ids {
		entity, _, e := r.GetCanonical(ctx, tx, "feed", grant.FamilyID, id)
		if e != nil {
			return s, e
		}
		s.SessionMembers = append(s.SessionMembers, entity)
	}
	s.LastBreast, err = getLatest(latestBreastSQL)
	return s, err
}
