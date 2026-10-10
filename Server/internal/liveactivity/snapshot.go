package liveactivity

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/care"
	"babytracker/server/internal/protocol"
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

// main's reads (main:Server/internal/liveactivity/snapshot.go) on
// care_entities. Each names the index it reads; kind codes are care.Kind*
// (1 baby, 2 feed, 3 sleep, 5 timer) and session_id is a feed's
// feeding_session_id, a sleep's sleep_session_id or a timer's session_id.
const (
	cursorSQL = `SELECT s.instance_id::text,s.recovery_epoch,f.id::text,f.next_sequence FROM server_state s CROSS JOIN families f WHERE f.id=$1`
	// The baby, deleted or not (primary key).
	babySQL = `SELECT wire,tombstoned_at IS NOT NULL FROM care_entities WHERE family_id=$1 AND id=$2 AND kind=1`
	// The baby's running timers, at most one per timer kind
	// (care_entities_one_live_timer).
	timersSQL = `SELECT wire FROM care_entities WHERE family_id=$1 AND baby_id=$2 AND kind=5 AND tombstoned_at IS NULL ORDER BY timer_kind`
	// latestMilkSQL is main's: the newest undeleted feed that is not solids
	// by end (or start), effective session and id, one row of
	// care_entities_latest_milk_idx (feed_kind 3 is solids).
	latestMilkSQL = `SELECT wire FROM care_entities WHERE family_id=$1 AND baby_id=$2 AND kind=2 AND tombstoned_at IS NULL AND feed_kind<>3 ORDER BY COALESCE(ended_at,started_at) DESC,COALESCE(session_id,id) DESC,id DESC LIMIT 1`
	// latestBreastSQL is main's: the most recently started undeleted breast
	// feed (feed_kind 1), whatever its parts, one row of
	// care_entities_latest_breast_idx.
	latestBreastSQL = `SELECT wire FROM care_entities WHERE family_id=$1 AND baby_id=$2 AND kind=2 AND tombstoned_at IS NULL AND feed_kind=1 ORDER BY started_at DESC,id DESC LIMIT 1`
	// sessionFeedsSQL is every undeleted feed of one feeding session, by link
	// or as its root (care_entities_session_idx and the primary key), newest
	// first: main's sessionMembersSQL order, whose filters (not solids, a
	// breast or bottle part) and count (sessionCountSQL) the reader applies
	// to the wire.
	sessionFeedsSQL = `SELECT wire FROM care_entities WHERE family_id=$1 AND baby_id=$2 AND kind=2 AND tombstoned_at IS NULL AND (session_id=$3 OR (session_id IS NULL AND id=$3)) ORDER BY started_at DESC,id DESC`
)

// The whole-second time asleep in the completed, undeleted parts of one
// sleep session: BabyStore.earlierAsleepSeconds(of:) as one aggregate on
// care_entities_session_idx (and the primary key for the root). Each part is
// truncated to whole seconds before the sum, as Swift's Int(ended - started)
// is, so awake gaps between parts never count. A sum has no rows to cap, so
// no sleep is too long for it; earlierAsleepSeconds below is the same rule in
// Go, for the shared fixture.
const earlierAsleepSQL = `SELECT coalesce(sum(floor(extract(epoch FROM ended_at-started_at))),0)::bigint FROM care_entities WHERE family_id=$1 AND baby_id=$2 AND kind=3 AND tombstoned_at IS NULL AND ended_at IS NOT NULL AND (session_id=$3 OR (session_id IS NULL AND id=$3))`

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

// Read requires the read-only repeatable-read transaction with the family's
// tenant context that authority.WithLiveActivitySnapshot supplies; it
// neither creates nor retains that transaction. Every read is of this one
// snapshot, so the cursor, the timers and the feeds agree.
func (SnapshotReader) Read(ctx context.Context, tx pgx.Tx, grant access.Grant, babyID string) (CareSnapshot, error) {
	s := CareSnapshot{Grant: grant}
	err := tx.QueryRow(ctx, cursorSQL, grant.FamilyID).Scan(&s.Cursor.ServerInstanceID, &s.Cursor.RecoveryEpoch, &s.Cursor.FamilyID, &s.Cursor.Sequence)
	if err != nil {
		return s, err
	}
	var wire []byte
	var deleted bool
	err = tx.QueryRow(ctx, babySQL, grant.FamilyID, babyID).Scan(&wire, &deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	// A deleted baby keeps its tombstone, as main's did: no cards.
	if s.Baby, err = entity(wire); err != nil || deleted {
		return s, err
	}
	rows, err := tx.Query(ctx, timersSQL, grant.FamilyID, babyID)
	if err != nil {
		return s, err
	}
	wires, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return s, err
	}
	for _, wire := range wires {
		timer, e := entity(wire)
		if e == nil {
			// main's GetTimerSnapshot(…).Canonical(): the running timer as
			// the timer tables rendered it.
			timer, e = care.NormalizeTimerEntity(timer)
		}
		if e != nil {
			return s, e
		}
		s.Timers = append(s.Timers, timer)
		// A running sleep's card counts the sleep it continues; one session,
		// summed in this same transaction so the parts and the timer agree.
		p, ok := timer.Payload.(protocol.TimerStartPayload)
		if !ok || p.TimerKind != "sleep" || p.SessionID == nil {
			continue
		}
		if e := tx.QueryRow(ctx, earlierAsleepSQL, grant.FamilyID, babyID, *p.SessionID).Scan(&s.EarlierAsleepSeconds); e != nil {
			return s, e
		}
	}
	if len(s.Timers) > 0 {
		return s, nil
	}
	if s.LastMilk, err = latestFeed(ctx, tx, latestMilkSQL, grant.FamilyID, babyID); err != nil || s.LastMilk == nil {
		return s, err
	}
	if err = readSession(ctx, tx, &s, grant.FamilyID, babyID, effectiveSession(*s.LastMilk)); err != nil {
		return s, err
	}
	s.LastBreast, err = latestFeed(ctx, tx, latestBreastSQL, grant.FamilyID, babyID)
	return s, err
}

// entity decodes a stored wire.
func entity(wire []byte) (protocol.CanonicalEntity, error) {
	var e protocol.CanonicalEntity
	err := json.Unmarshal(wire, &e)
	return e, err
}

// feedOf is the feed payload of a stored feed.
func feedOf(e protocol.CanonicalEntity) (protocol.FeedPayload, error) {
	p, ok := e.Payload.(protocol.FeedPayload)
	if !ok {
		return p, errors.New("invalid feed snapshot")
	}
	return p, nil
}

// latestFeed is the one feed query reads, or nil.
func latestFeed(ctx context.Context, tx pgx.Tx, query, familyID, babyID string) (*protocol.CanonicalEntity, error) {
	var wire []byte
	err := tx.QueryRow(ctx, query, familyID, babyID).Scan(&wire)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e, err := entity(wire)
	if err != nil {
		return nil, err
	}
	if _, err := feedOf(e); err != nil {
		return nil, err
	}
	return &e, nil
}

// readSession is main's sessionMembersSQL and sessionCountSQL over one read
// of the session's feeds: the members are the feeds that are not solids and
// have a breast or bottle part, newest first, of which the snapshot keeps
// MaximumSummaryParts (main's LIMIT 8); TotalSessionParts counts the breast
// and bottle parts of every member.
func readSession(ctx context.Context, tx pgx.Tx, s *CareSnapshot, familyID, babyID, session string) error {
	rows, err := tx.Query(ctx, sessionFeedsSQL, familyID, babyID, session)
	if err != nil {
		return err
	}
	var wire []byte
	_, err = pgx.ForEachRow(rows, []any{&wire}, func() error {
		e, err := entity(wire)
		if err != nil {
			return err
		}
		p, err := feedOf(e)
		if err != nil || p.Kind == "solids" {
			return err
		}
		var parts uint64
		for _, c := range p.Components {
			if c.Kind == "breast" || c.Kind == "bottle" {
				parts++
			}
		}
		if parts == 0 {
			return nil
		}
		s.TotalSessionParts += parts
		if len(s.SessionMembers) < MaximumSummaryParts {
			s.SessionMembers = append(s.SessionMembers, e)
		}
		return nil
	})
	return err
}

// ReadCompletedSession is a stopped timer's link to the feed its stop
// created (main's worker: active_timers joined to feed_entries): the
// completion record and, while that feed is undeleted, its effective
// session. Both are nil when the timer is not a stopped timer of the family
// (still running, never stopped, or unknown); session is nil when the
// completion is not an undeleted feed.
func ReadCompletedSession(ctx context.Context, tx pgx.Tx, familyID, timerID string) (completed, session *string, err error) {
	err = tx.QueryRow(ctx, `SELECT t.completed_record_id::text,COALESCE(f.session_id,f.id)::text FROM care_entities t
 LEFT JOIN care_entities f ON f.family_id=t.family_id AND f.id=t.completed_record_id AND f.kind=2 AND f.tombstoned_at IS NULL
 WHERE t.family_id=$1 AND t.id=$2 AND t.kind=5 AND t.tombstoned_at IS NOT NULL`, familyID, timerID).Scan(&completed, &session)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	return completed, session, err
}

// TimerOrigin is the device and family sequence of a timer's start (main's
// first timer_start journal row), for SuppressStart: the phone that started
// a timer gets no push-to-start for it.
type TimerOrigin struct {
	DeviceID string
	Sequence uint64 // 0 when unknown (an entity written before 000032)
}

// ReadTimerOrigin is the origin of timerID (care_entities.created_device_id
// and created_seq, written once by care.Encode). An unknown timer, and one
// written before migration 32, has the zero origin.
func ReadTimerOrigin(ctx context.Context, tx pgx.Tx, familyID, timerID string) (TimerOrigin, error) {
	var o TimerOrigin
	err := tx.QueryRow(ctx, `SELECT coalesce(created_device_id::text,''),coalesce(created_seq,0) FROM care_entities WHERE family_id=$1 AND id=$2 AND kind=5`, familyID, timerID).Scan(&o.DeviceID, &o.Sequence)
	if errors.Is(err, pgx.ErrNoRows) {
		return TimerOrigin{}, nil
	}
	return o, err
}
