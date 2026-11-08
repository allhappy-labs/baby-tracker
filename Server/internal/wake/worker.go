package wake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"babytracker/server/internal/apns"
	"babytracker/server/internal/database"
	"babytracker/server/internal/diagnostics"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultInterval           = 2 * time.Second
	defaultMinWakeInterval    = 60 * time.Second
	defaultMaxWakesPerTick    = 500
	defaultMaxFamiliesPerTick = 1000
	// tickTimeout bounds one Run tick: the slot wait, the transaction and
	// the start of the sends (each send in flight then finishes within the
	// send budget).
	tickTimeout = 15 * time.Second
	// defaultSendBudget bounds a tick's pushes once started.
	defaultSendBudget = 10 * time.Second
	// seenAtStep: seen_at (indexed for the 35-day device sweep) advances at
	// most once a day, so the flush's updates stay heap-only.
	seenAtStep = "1 day"
)

// Wake is one family's wake round: the devices behind its high water.
type Wake struct {
	FamilyID  string
	DeviceIDs []string
	Sequence  uint64
}

// Sender delivers one tick's wakes, after the tick's transaction committed.
type Sender interface {
	Wake(ctx context.Context, wakes []Wake) error
}

// Stats is what one tick did: Flushed device rows written (inserted, or a
// cursor or the daily seen_at advanced), Woken devices re-announced through
// the hub, Pushed APNs sends made (each after its reservation committed; a
// send the client refused before writing is not one) and PushFailed the
// pushes that failed: those Apple did not accept, and those never sent.
type Stats struct{ Flushed, Woken, Pushed, PushFailed int }

// Totals are the worker's counts since it was built, for the resource
// sampler (and TicksSucceeded for the identitytest wake route): Ticks every
// Tick that ran (an idle one and a failed one included); TicksSucceeded
// those whose work returned no error, the only ones that prove they scanned
// and sent what was committed before they began; Flushed,
// Woken, Pushed and PushFailed the sums of the ticks' Stats; NotSent the
// pushes the client never put on the wire and Ambiguous those whose answer
// was lost (both within PushFailed); SettlesFailed the failed attempts to
// apply a kept settle, one per settle per attempt: a settle whose own
// statements failed, or every kept settle when the attempt found no worker
// slot (each stays kept, quarantined, while the others apply). Settles left
// untried because the context ended after an earlier failure are not
// counted. SettlesPending alone is not a sum: it is the device rows of the
// settles kept now.
//
// For capacity runs: TickDurationNS is the duration of each tick that had
// work (drained progress or families, or a backstop page; an idle tick is
// left out), from its start to its return, the sends and their settle
// included, as a capacity histogram (diagnostics.Histogram.Observe; its
// Count is those ticks); Families sums the families those ticks drained,
// and FamiliesPeak is the most one tick drained.
type Totals struct {
	Ticks, TicksSucceeded, Flushed, Woken, Pushed, PushFailed, NotSent, Ambiguous, SettlesPending, SettlesFailed uint64

	Families, FamiliesPeak uint64
	TickDurationNS         diagnostics.Histogram
}

// totalCounters back Totals under their own mutex (never w.mu, so Totals
// never waits on a running tick). A tick's counts are added together, once
// it returned, so a copy never shows NotSent or Ambiguous ahead of
// PushFailed.
type totalCounters struct {
	mu     sync.Mutex
	totals Totals
}

// addTick adds one tick's counts, all at once; succeeded reports that its
// work returned no error, and took is how long it ran.
func (c *totalCounters) addTick(stats Stats, counts tickCounts, succeeded bool, took time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.totals.Ticks++
	if succeeded {
		c.totals.TicksSucceeded++
	}
	c.totals.Flushed += uint64(stats.Flushed)
	c.totals.Woken += uint64(stats.Woken)
	c.totals.Pushed += uint64(stats.Pushed)
	c.totals.PushFailed += uint64(stats.PushFailed)
	c.totals.NotSent += uint64(counts.notSent)
	c.totals.Ambiguous += uint64(counts.ambiguous)
	if counts.worked {
		c.totals.TickDurationNS.Observe(took)
		c.totals.Families += uint64(counts.families)
		c.totals.FamiliesPeak = max(c.totals.FamiliesPeak, uint64(counts.families))
	}
}

// settled records the settle rows kept now and the settles that just
// failed to apply (each kept for a later attempt).
func (c *totalCounters) settled(pendingRows, failed int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.totals.SettlesPending = uint64(pendingRows)
	c.totals.SettlesFailed += uint64(failed)
}

// tickCounts are what one tick did beyond its Stats: the pushes the client
// never put on the wire and those whose answer was lost (both within its
// Stats.PushFailed), whether it had work, and the families it drained.
type tickCounts struct {
	notSent, ambiguous int
	worked             bool
	families           int
}

// Totals copies the worker's counts (zero for a nil worker).
func (w *Worker) Totals() Totals {
	if w == nil {
		return Totals{}
	}
	w.totals.mu.Lock()
	defer w.totals.mu.Unlock()
	return w.totals.totals
}

// Worker flushes the tracker's progress and wakes devices behind their
// family: through the live hub when the device holds a connection, through
// an APNs background push otherwise. One tick is one maintenance
// transaction:
//
//  1. drain the tracker: every recorded device and up to MaxFamiliesPerTick
//     changed (or deferred) families;
//  2. one pipelined batch: lock and statement timeouts, the flush (progress
//     of families that are not active is dropped), the lag scan over the
//     drained families, the deferral scan, and while the startup backstop
//     is unfinished one keyset page of device_sync_cursors. Each row is
//     hub-due (behind its woken_sequence, outside the 60 s window) and/or
//     push-due (behind its pushed_sequence, outside the 20-minute APNs
//     window; only with a Pusher);
//  3. for rows push-due without a connection, while sends are not paused,
//     app.push_targets (the registered tokens of devices still holding a
//     family session there);
//  4. decide (the hub, the pushes, the marks) and one mark of woken_* and
//     the push reservation pushed_*, then COMMIT;
//  5. after the commit, outside the slot: one Sender.Wake with one Wake per
//     family, then the pushes, at most 8 at a time, under SendBudget (the
//     tick's deadline stops new pushes, never one in flight).
//
// A failed tick restores what it drained. A device is woken at most once
// per MinWakeInterval and once per sequence, and pushed at most once per
// APNsInterval and once per sequence: the marks commit before the sends, so
// a crash between the two loses that wake, never repeats it. A push never
// started (the tick was paused by an earlier answer, or its context ended)
// gives its reservation back. A push Apple did not deliver (not sent, 429,
// a server error, a configuration refusal) gives its sequence back and
// keeps its time, so it is pushed again when its window ends; only a lost
// answer (Ambiguous) keeps both, the one at-most-once loss. These post-send
// settles (and the tokens Apple refused) are kept until they apply: a
// settle that cannot run in its tick (no worker slot before the tick's
// deadline, a lost connection) is applied by the next tick, before its
// scans, or when Run stops; each settle applies on its own, so one that
// cannot apply never holds back the others; each statement is guarded by
// the reservation it gives back, so a replay changes nothing twice. The kept settles are
// bounded (maxSettleRows; the oldest dropped, logged); a crash loses them,
// like a crash between a mark and its send. One phone registered on several
// device rows gets one push for all of them.
//
// Locks: the flush and the mark take row locks on device_sync_cursors only
// (only this worker writes it); new rows' foreign-key check takes KEY SHARE
// on their family rows, which never conflicts with a request's FOR NO KEY
// UPDATE. The worker locks no family, account or session row.
type Worker struct {
	Pool *pgxpool.Pool // maintenance
	// Slots are the maintenance pool's worker slots: a tick that has work
	// holds one for its transaction. nil takes none (tests).
	Slots   *database.WorkerSlots
	Tracker *Tracker
	Sender  Sender
	// Pusher sends APNs background wakes (nil: hub wakes only, a self-hoster
	// without APNs).
	Pusher Pusher
	// Connected reports a device with a live connection (the hub's
	// DeviceConnected): it is woken through the hub, never pushed. nil
	// treats every device as connected (hub only); a Pusher needs it.
	Connected          func(deviceID string) bool
	Logger             *slog.Logger
	Now                func() time.Time
	Interval           time.Duration // 0 means 2s
	MinWakeInterval    time.Duration // 0 means 60s
	MaxWakesPerTick    int           // 0 means 500 (at most 1000)
	MaxFamiliesPerTick int           // 0 means 1000
	// SendBudget bounds a tick's APNs sends, apart from the tick's own
	// context: the tick's deadline (or Run's stop) stops new pushes from
	// starting but never cancels one in flight, whose answer would then be
	// lost (Ambiguous) though Apple had it (C25). 0 means 10s.
	SendBudget time.Duration

	mu sync.Mutex
	// The startup backstop: device_sync_cursors in key order, one page a
	// tick, from the first tick until a page comes back short. It wakes
	// devices left behind across a restart (no Changed names their family).
	backstopDone                   bool
	backstopFamily, backstopDevice string
	// pause holds pushes back after Apple's answers.
	pause pauseState
	// settles are the post-send settles not applied yet (at most
	// maxSettleRows device rows), oldest first.
	settles    []pendingSettle
	settleRows int
	settleSeq  uint64 // the last kept settle's id
	totals     totalCounters
}

const (
	sqlTimeouts = `SELECT set_config('lock_timeout', '2s', true), set_config('statement_timeout', '10s', true)`
	// sqlFlush upserts drained progress (in key order); a family that is
	// not active drops its devices' progress. A row is rewritten only when
	// a cursor advanced or seen_at is a day old.
	sqlFlush = `INSERT INTO device_sync_cursors AS d (family_id, device_id, acked_sequence, delivered_sequence, seen_at)
SELECT v.f, v.d, v.a, v.dl, v.s
FROM unnest($1::uuid[], $2::uuid[], $3::bigint[], $4::bigint[], $5::timestamptz[]) WITH ORDINALITY AS v(f, d, a, dl, s, n)
JOIN families fam ON fam.id = v.f AND fam.status = 'active'
ORDER BY v.n
ON CONFLICT (family_id, device_id) DO UPDATE SET
  acked_sequence = greatest(d.acked_sequence, EXCLUDED.acked_sequence),
  delivered_sequence = greatest(d.delivered_sequence, EXCLUDED.delivered_sequence),
  seen_at = CASE WHEN EXCLUDED.seen_at >= d.seen_at + interval '` + seenAtStep + `' THEN EXCLUDED.seen_at ELSE d.seen_at END
WHERE EXCLUDED.acked_sequence > d.acked_sequence OR EXCLUDED.delivered_sequence > d.delivered_sequence
  OR EXCLUDED.seen_at >= d.seen_at + interval '` + seenAtStep + `'`
	// hubBehind and pushBehind: the device is behind its family's high
	// water, beyond the sequence it was last woken (re-announced) for, or
	// last pushed for. $pushing is false without a Pusher, so no row is
	// push-due then.
	hubBehind  = `f.next_sequence > greatest(d.acked_sequence, d.delivered_sequence, d.woken_sequence)`
	pushBehind = `f.next_sequence > greatest(d.acked_sequence, d.delivered_sequence, d.pushed_sequence)`
	// sqlLagScan: the drained families' devices that are hub-due (behind
	// beyond woken_sequence, outside the 60 s window $2) or push-due (behind
	// beyond pushed_sequence, outside the APNs window $3, $4 pushing), with
	// their push reservation (device_sync_cursors_pkey, families_pkey).
	sqlLagScan = `SELECT family_id::text, device_id::text, next_sequence, hub_due, push_due, pushed_sequence, pushed_at, woken_at FROM (
  SELECT d.family_id, d.device_id, f.next_sequence, d.pushed_sequence, d.pushed_at, d.woken_at,
    ` + hubBehind + ` AND (d.woken_at IS NULL OR d.woken_at <= $2) AS hub_due,
    $4::boolean AND ` + pushBehind + ` AND (d.pushed_at IS NULL OR d.pushed_at <= $3) AS push_due
  FROM device_sync_cursors d JOIN families f ON f.id = d.family_id
  WHERE d.family_id = ANY($1::uuid[]) AND f.status = 'active') s
WHERE hub_due OR push_due
ORDER BY family_id, device_id LIMIT $5`
	// sqlDeferrals: per drained family, the earliest wake of a device that
	// is behind but inside its hub window, and the earliest push of a device
	// that is behind but inside its APNs window; the family is scanned again
	// when the first of those windows ends (a trailing change is announced,
	// and pushed, without a further write).
	sqlDeferrals = `SELECT d.family_id::text,
  min(d.woken_at) FILTER (WHERE ` + hubBehind + ` AND d.woken_at > $2),
  min(d.pushed_at) FILTER (WHERE $4::boolean AND ` + pushBehind + ` AND d.pushed_at > $3)
FROM device_sync_cursors d JOIN families f ON f.id = d.family_id
WHERE d.family_id = ANY($1::uuid[]) AND f.status = 'active'
  AND ((` + hubBehind + ` AND d.woken_at > $2) OR ($4::boolean AND ` + pushBehind + ` AND d.pushed_at > $3))
GROUP BY d.family_id`
	// sqlBackstop: one keyset page of every device row (any family), each
	// with whether it is behind for the hub and for a push ($4 pushing),
	// when it was last woken and pushed, and its push reservation.
	sqlBackstop = `SELECT d.family_id::text, d.device_id::text, coalesce(f.next_sequence, 0),
  coalesce(f.status = 'active' AND ` + hubBehind + `, false), d.woken_at,
  coalesce($4::boolean AND f.status = 'active' AND ` + pushBehind + `, false), d.pushed_sequence, d.pushed_at
FROM (SELECT family_id, device_id, acked_sequence, delivered_sequence, woken_sequence, woken_at, pushed_sequence, pushed_at
  FROM device_sync_cursors WHERE (family_id, device_id) > ($1::uuid, $2::uuid) ORDER BY family_id, device_id LIMIT $3) d
LEFT JOIN families f ON f.id = d.family_id
ORDER BY d.family_id, d.device_id`
	// sqlPushTargets: the registered tokens of the push candidates whose
	// device still holds a family session in that family (a definer: the
	// maintenance role holds no privilege on device_push_tokens).
	sqlPushTargets = `SELECT family_id::text, device_id::text, environment::text, token FROM app.push_targets($1::uuid[], $2::uuid[])`
	// sqlMark marks, in one statement, the woken devices (woken_*) and the
	// push reservations (pushed_*), each at the tick's time and sequence.
	sqlMark = `UPDATE device_sync_cursors d SET
  woken_sequence = CASE WHEN v.wake THEN greatest(d.woken_sequence, v.seq) ELSE d.woken_sequence END,
  woken_at = CASE WHEN v.wake THEN $1 ELSE d.woken_at END,
  pushed_sequence = CASE WHEN v.push THEN greatest(d.pushed_sequence, v.seq) ELSE d.pushed_sequence END,
  pushed_at = CASE WHEN v.push THEN $1 ELSE d.pushed_at END
FROM unnest($2::uuid[], $3::uuid[], $4::bigint[], $5::boolean[], $6::boolean[]) AS v(f, dv, seq, wake, push)
WHERE d.family_id = v.f AND d.device_id = v.dv`
	zeroUUID = "00000000-0000-0000-0000-000000000000"
)

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Worker) minWakeInterval() time.Duration {
	if w.MinWakeInterval > 0 {
		return w.MinWakeInterval
	}
	return defaultMinWakeInterval
}

// maxWakes bounds a tick's due rows; at most maxPushTargets, so one
// app.push_targets call can look up every candidate.
func (w *Worker) maxWakes() int {
	if w.MaxWakesPerTick > 0 {
		return min(w.MaxWakesPerTick, maxPushTargets)
	}
	return defaultMaxWakesPerTick
}

// connectedOnce reads Connected at most once per device for one tick (nil
// Connected: every device is reached through the hub).
func (w *Worker) connectedOnce() func(string) bool {
	if w.Connected == nil {
		return func(string) bool { return true }
	}
	seen := map[string]bool{}
	return func(device string) bool {
		live, known := seen[device]
		if !known {
			live = w.Connected(device)
			seen[device] = live
		}
		return live
	}
}

func (w *Worker) maxFamilies() int {
	if w.MaxFamiliesPerTick > 0 {
		return w.MaxFamiliesPerTick
	}
	return defaultMaxFamiliesPerTick
}

func (w *Worker) sendBudget() time.Duration {
	if w.SendBudget > 0 {
		return w.SendBudget
	}
	return defaultSendBudget
}

func (w *Worker) interval() time.Duration {
	if w.Interval > 0 {
		return w.Interval
	}
	return defaultInterval
}

var errNotComposed = errors.New("wake worker needs a pool, a tracker and a sender (and Connected with a Pusher)")

// Tick runs one tick: at most 4 round trips (BEGIN, the flush and scans,
// the mark, COMMIT), 5 with push candidates (app.push_targets), none when
// there is nothing to do; then, after the commit, the sends and their
// settle (one more). Settles kept from an earlier tick cost one round trip
// each first.
func (w *Worker) Tick(ctx context.Context) (Stats, error) {
	if w == nil || w.Pool == nil || w.Tracker == nil || w.Sender == nil || (w.Pusher != nil && w.Connected == nil) {
		return Stats{}, errNotComposed
	}
	started := time.Now()
	var counts tickCounts
	stats, err := w.tick(ctx, &counts)
	w.totals.addTick(stats, counts, err == nil, time.Since(started))
	return stats, err
}

// tick is Tick's work; push receives what it did that Stats does not
// carry.
func (w *Worker) tick(ctx context.Context, push *tickCounts) (Stats, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	// Settles an earlier tick could not apply go first; their families are
	// scanned by this tick.
	applied, replayErr := w.applySettles(ctx)
	for _, settle := range applied {
		w.rescanSettled(settle)
	}
	work := w.Tracker.drain(now, w.maxFamilies())
	if len(work.devices) == 0 && len(work.families) == 0 && w.backstopDone {
		return Stats{}, replayErr
	}
	push.worked, push.families = true, len(work.families)
	paused := w.Pusher != nil && w.pause.paused(now)
	outcome, err := w.transact(ctx, now, work, paused)
	if err != nil {
		w.Tracker.restore(work)
		return Stats{}, errors.Join(replayErr, err)
	}
	// Committed: schedule what the tick left for later.
	w.Tracker.requeue(outcome.requeue)
	for family, at := range outcome.deferrals {
		w.Tracker.deferFamily(family, at)
	}
	w.hold(outcome.held, paused)
	if outcome.backstop != nil {
		w.backstopDone, w.backstopFamily, w.backstopDevice = outcome.backstop.done, outcome.backstop.family, outcome.backstop.device
	}
	stats := Stats{Flushed: outcome.flushed}
	for _, wake := range outcome.wakes {
		stats.Woken += len(wake.DeviceIDs)
	}
	var sendErr error
	if len(outcome.wakes) > 0 {
		if err := w.Sender.Wake(ctx, outcome.wakes); err != nil {
			sendErr = &SendError{Err: err}
		}
	}
	if len(outcome.pushes) == 0 {
		return stats, errors.Join(replayErr, sendErr)
	}
	// The sends' own context: the tick's end stops new pushes from starting
	// (sendPushes watches ctx) but never cancels one in flight.
	sendCtx, cancelSends := context.WithTimeout(context.WithoutCancel(ctx), w.sendBudget())
	report := w.sendPushes(ctx, sendCtx, outcome.pushes)
	cancelSends()
	stats.Pushed, stats.PushFailed = report.sent, report.failed
	push.notSent, push.ambiguous = report.notSentCount, report.ambiguous
	w.applyAnswers(report, w.now())
	// The settle is kept until it applies: now, or on a later tick (after
	// any earlier settle still kept, each in its own transaction).
	settle := pendingSettle{reservedAt: now, forget: rowsOf(report.forget), undelivered: rowsOf(report.undelivered), unsent: rowsOf(report.unsent)}
	own := w.keepSettle(settle)
	applied, settleErr := w.applySettles(ctx)
	for _, earlier := range applied {
		if earlier.id != own { // this tick's families are scheduled below
			w.rescanSettled(earlier)
		}
	}
	if settleErr == nil {
		replayErr = nil // whatever the replay left kept has applied since
	}
	if unsent := rowsOf(report.unsent); len(unsent) > 0 {
		families := make([]string, 0, len(unsent))
		for _, job := range unsent {
			families = append(families, job.family)
		}
		w.hold(families, w.pause.paused(w.now()))
	}
	// A push Apple did not deliver is owed again when its window ends: its
	// family is scanned again then (its reservation kept pushed_at).
	for _, job := range rowsOf(report.undelivered) {
		w.Tracker.deferFamily(job.family, now.Add(APNsInterval))
	}
	return stats, errors.Join(replayErr, sendErr, settleErr)
}

// hold schedules families whose push the tick held back: at the pause's end
// while paused, else on the next tick.
func (w *Worker) hold(families []string, paused bool) {
	for _, family := range families {
		if paused {
			w.Tracker.deferFamily(family, w.pause.until)
		} else {
			w.Tracker.requeue([]familyChange{{FamilyID: family}})
		}
	}
}

// SendError is a Sender failure after the tick committed: the database work
// is done (and the wakes marked); the hints of that tick are lost.
type SendError struct{ Err error }

func (e *SendError) Error() string { return "wake: send: " + e.Err.Error() }
func (e *SendError) Unwrap() error { return e.Err }

type backstopPosition struct {
	done           bool
	family, device string
}

// tickOutcome is a committed tick's result.
type tickOutcome struct {
	flushed   int
	due       []lagRow // the scans' due rows, decided after the scans
	wakes     []Wake
	pushes    []pushJob
	held      []string // families whose push was held back
	requeue   []familyChange
	deferrals map[string]time.Time
	backstop  *backstopPosition
}

// lagRow is one due device: hub-due and/or push-due at the family's high
// water (sequence), with its current push reservation, when it was last
// woken and, once looked up, its registered target.
type lagRow struct {
	family, device  string
	sequence        uint64
	hubDue, pushDue bool
	pushedSequence  uint64
	pushedAt        *time.Time
	wokenAt         *time.Time
	target          *pushTarget
}

// transact is the tick's transaction, inside a worker slot.
func (w *Worker) transact(ctx context.Context, now time.Time, work drained, paused bool) (outcome tickOutcome, err error) {
	if w.Slots != nil {
		release, err := w.Slots.Acquire(ctx)
		if err != nil {
			return tickOutcome{}, err
		}
		defer release()
	}
	limit := w.maxWakes()
	cut, pushCut, pushing := now.Add(-w.minWakeInterval()), now.Add(-APNsInterval), w.Pusher != nil
	tx, err := w.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return tickOutcome{}, fmt.Errorf("wake: begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(context.Background())
		}
	}()
	batch := &pgx.Batch{}
	batch.Queue(sqlTimeouts)
	if len(work.devices) > 0 {
		families, devices := make([]string, len(work.devices)), make([]string, len(work.devices))
		acked, delivered := make([]int64, len(work.devices)), make([]int64, len(work.devices))
		seen := make([]time.Time, len(work.devices))
		for i, d := range work.devices {
			families[i], devices[i], acked[i], delivered[i], seen[i] = d.FamilyID, d.DeviceID, int64(d.Acked), int64(d.Delivered), d.Seen
		}
		batch.Queue(sqlFlush, families, devices, acked, delivered, seen)
	}
	familyIDs := make([]string, len(work.families))
	for i, f := range work.families {
		familyIDs[i] = f.FamilyID
	}
	if len(familyIDs) > 0 {
		batch.Queue(sqlLagScan, familyIDs, cut, pushCut, pushing, limit)
		batch.Queue(sqlDeferrals, familyIDs, cut, pushCut, pushing)
	}
	if !w.backstopDone {
		from, fromDevice := w.backstopFamily, w.backstopDevice
		if from == "" {
			from, fromDevice = zeroUUID, zeroUUID
		}
		batch.Queue(sqlBackstop, from, fromDevice, limit, pushing)
	}
	results := tx.SendBatch(ctx, batch)
	outcome, err = w.readBatch(results, work, familyIDs, cut, pushCut, limit)
	if closeErr := results.Close(); err == nil && closeErr != nil {
		err = fmt.Errorf("wake: batch: %w", closeErr)
	}
	if err != nil {
		return tickOutcome{}, err
	}
	connected := w.connectedOnce()
	if lookups := pushLookups(outcome.due, connected, paused); len(lookups) > 0 {
		if err = readTargets(ctx, tx, outcome.due, lookups); err != nil {
			return tickOutcome{}, err
		}
	}
	d := decide(outcome.due, connected, MaxPushesPerTick, paused)
	for _, family := range d.recheck {
		outcome.deferUntil(family, now.Add(w.minWakeInterval()))
	}
	// A connected device woken within its hub window and still owed a push
	// keeps its recheck at woken_at+60 s, whatever rescanned the family
	// earlier (the tracker holds one deferral per family): if its connection
	// was dying it is pushed then. At or after that time this is false, so it
	// never repeats.
	for _, row := range outcome.due {
		if row.pushDue && !row.hubDue && row.wokenAt != nil && row.wokenAt.After(cut) && connected(row.device) {
			outcome.deferUntil(row.family, row.wokenAt.Add(w.minWakeInterval()))
		}
	}
	if len(d.marks) > 0 {
		n := len(d.marks)
		families, devices, sequences := make([]string, n), make([]string, n), make([]int64, n)
		wake, push := make([]bool, n), make([]bool, n)
		for i, mark := range d.marks {
			families[i], devices[i], sequences[i], wake[i], push[i] = mark.family, mark.device, int64(mark.sequence), mark.wake, mark.push
		}
		if _, err = tx.Exec(ctx, sqlMark, now, families, devices, sequences, wake, push); err != nil {
			return tickOutcome{}, fmt.Errorf("wake: mark: %w", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return tickOutcome{}, fmt.Errorf("wake: commit: %w", err)
	}
	outcome.wakes, outcome.pushes, outcome.held = wakesOf(d.hub), d.push, d.held
	return outcome, nil
}

// readTargets looks up the registered targets of rows[lookups] in one
// app.push_targets call and attaches them; a row without one (no token, or
// its device no longer holds a family session there) keeps a nil target.
func readTargets(ctx context.Context, tx pgx.Tx, rows []lagRow, lookups []int) error {
	families, devices := make([]string, len(lookups)), make([]string, len(lookups))
	index := make(map[deviceKey]int, len(lookups))
	for i, at := range lookups {
		families[i], devices[i] = rows[at].family, rows[at].device
		index[deviceKey{rows[at].family, rows[at].device}] = at
	}
	targets, err := tx.Query(ctx, sqlPushTargets, families, devices)
	if err != nil {
		return fmt.Errorf("wake: push targets: %w", err)
	}
	defer targets.Close()
	for targets.Next() {
		var family, device, env string
		var token []byte
		if err := targets.Scan(&family, &device, &env, &token); err != nil {
			return fmt.Errorf("wake: push targets: %w", err)
		}
		if at, ok := index[deviceKey{family, device}]; ok {
			rows[at].target = &pushTarget{env: apns.Environment(env), token: token}
		}
	}
	if err := targets.Err(); err != nil {
		return fmt.Errorf("wake: push targets: %w", err)
	}
	return nil
}

// readBatch reads the batch's results in queue order and gathers the
// tick's due rows: the lag scan's first, then the backstop page's while the
// per-tick budget lasts.
func (w *Worker) readBatch(results pgx.BatchResults, work drained, familyIDs []string, cut, pushCut time.Time, limit int) (tickOutcome, error) {
	outcome := tickOutcome{deferrals: map[string]time.Time{}}
	window := w.minWakeInterval()
	if _, err := results.Exec(); err != nil {
		return tickOutcome{}, fmt.Errorf("wake: timeouts: %w", err)
	}
	if len(work.devices) > 0 {
		tag, err := results.Exec()
		if err != nil {
			return tickOutcome{}, fmt.Errorf("wake: flush: %w", err)
		}
		outcome.flushed = int(tag.RowsAffected())
	}
	var due []lagRow
	taken := map[deviceKey]bool{}
	if len(familyIDs) > 0 {
		rows, err := results.Query()
		if err != nil {
			return tickOutcome{}, fmt.Errorf("wake: lag scan: %w", err)
		}
		for rows.Next() {
			var row lagRow
			var sequence, pushed int64
			if err := rows.Scan(&row.family, &row.device, &sequence, &row.hubDue, &row.pushDue, &pushed, &row.pushedAt, &row.wokenAt); err != nil {
				rows.Close()
				return tickOutcome{}, fmt.Errorf("wake: lag scan: %w", err)
			}
			row.sequence, row.pushedSequence = uint64(sequence), uint64(pushed)
			due = append(due, row)
			taken[deviceKey{row.family, row.device}] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return tickOutcome{}, fmt.Errorf("wake: lag scan: %w", err)
		}
		if len(due) == limit {
			// Truncated: families from the last one scanned on may have more
			// devices behind; scan them again next tick (the marked ones
			// are inside their window by then).
			last := due[len(due)-1].family
			for _, f := range work.families {
				if f.FamilyID >= last {
					outcome.requeue = append(outcome.requeue, f)
				}
			}
		}
		deferrals, err := results.Query()
		if err != nil {
			return tickOutcome{}, fmt.Errorf("wake: deferrals: %w", err)
		}
		for deferrals.Next() {
			var family string
			var woken, pushed *time.Time
			if err := deferrals.Scan(&family, &woken, &pushed); err != nil {
				deferrals.Close()
				return tickOutcome{}, fmt.Errorf("wake: deferrals: %w", err)
			}
			if woken != nil {
				outcome.deferUntil(family, woken.Add(window))
			}
			if pushed != nil {
				outcome.deferUntil(family, pushed.Add(APNsInterval))
			}
		}
		deferrals.Close()
		if err := deferrals.Err(); err != nil {
			return tickOutcome{}, fmt.Errorf("wake: deferrals: %w", err)
		}
	}
	if !w.backstopDone {
		page, err := results.Query()
		if err != nil {
			return tickOutcome{}, fmt.Errorf("wake: backstop: %w", err)
		}
		position := backstopPosition{family: w.backstopFamily, device: w.backstopDevice}
		read, consumed := 0, 0
		budget := limit - len(due)
		for page.Next() {
			var row lagRow
			var sequence, pushed int64
			var hubBehind, pushBehind bool
			var wokenAt *time.Time
			if err := page.Scan(&row.family, &row.device, &sequence, &hubBehind, &wokenAt, &pushBehind, &pushed, &row.pushedAt); err != nil {
				page.Close()
				return tickOutcome{}, fmt.Errorf("wake: backstop: %w", err)
			}
			read++
			if read != consumed+1 {
				continue // the budget ran out on an earlier row: the rest wait for the next page
			}
			row.sequence, row.pushedSequence, row.wokenAt = uint64(sequence), uint64(pushed), wokenAt
			key := deviceKey{row.family, row.device}
			hubWaits := hubBehind && wokenAt != nil && wokenAt.After(cut)
			pushWaits := pushBehind && row.pushedAt != nil && row.pushedAt.After(pushCut)
			row.hubDue, row.pushDue = hubBehind && !hubWaits, pushBehind && !pushWaits
			isDue := (row.hubDue || row.pushDue) && !taken[key]
			if isDue && budget == 0 {
				continue
			}
			if hubWaits {
				outcome.deferUntil(row.family, wokenAt.Add(window))
			}
			if pushWaits {
				outcome.deferUntil(row.family, row.pushedAt.Add(APNsInterval))
			}
			if isDue {
				due = append(due, row)
				taken[key] = true
				budget--
			}
			consumed++
			position.family, position.device = row.family, row.device
		}
		page.Close()
		if err := page.Err(); err != nil {
			return tickOutcome{}, fmt.Errorf("wake: backstop: %w", err)
		}
		position.done = read < limit && consumed == read
		outcome.backstop = &position
	}
	outcome.due = due
	return outcome, nil
}

// deferUntil keeps the earliest time a family is to be scanned again.
func (outcome *tickOutcome) deferUntil(family string, at time.Time) {
	if current, ok := outcome.deferrals[family]; !ok || at.Before(current) {
		outcome.deferrals[family] = at
	}
}

// wakesOf groups due devices into one Wake per family, in family order,
// devices in order, at the highest sequence read for the family.
func wakesOf(due []lagRow) []Wake {
	if len(due) == 0 {
		return nil
	}
	slices.SortFunc(due, func(a, b lagRow) int {
		if c := strings.Compare(a.family, b.family); c != 0 {
			return c
		}
		return strings.Compare(a.device, b.device)
	})
	var wakes []Wake
	for _, row := range due {
		if n := len(wakes); n > 0 && wakes[n-1].FamilyID == row.family {
			wakes[n-1].DeviceIDs = append(wakes[n-1].DeviceIDs, row.device)
			wakes[n-1].Sequence = max(wakes[n-1].Sequence, row.sequence)
			continue
		}
		wakes = append(wakes, Wake{FamilyID: row.family, DeviceIDs: []string{row.device}, Sequence: row.sequence})
	}
	return wakes
}

// applySettlesOnStop applies, once Run's context ended, the settles still
// kept, within settleTimeout (a graceful stop loses none while the database
// answers). What it cannot apply is logged at Warn and lost with the
// process: those reservations stand, as after a crash between a send and
// its settle.
func (w *Worker) applySettlesOnStop(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	rows := w.settleRows
	if _, err := w.applySettles(ctx); err != nil && w.Logger != nil {
		w.Logger.Warn("wake settles lost at stop; their reservations stand", slog.Int("rows", rows),
			slog.Bool("timeout", errors.Is(err, context.DeadlineExceeded)))
	}
}

// stopFlushTimeout bounds the flush when Run stops.
const stopFlushTimeout = 2 * time.Second

// FlushTracker writes the progress the tracker recorded since the last tick
// or flush: Tick's flush statement alone (no scan, no send), inside a
// worker slot, within 2 s of its own (detached from ctx's cancellation), so
// a graceful restart loses no device's progress (C42). Run calls it once
// its context ended, after the kept settles; the server calls it once more
// after the HTTP drain, for the progress requests recorded while they
// drained. It logs one line at Info with the devices drained and the rows
// written (nothing when there is nothing to write); what it cannot write is
// logged at Warn and lost with the process, as after a crash. Safe on a nil
// worker.
func (w *Worker) FlushTracker(ctx context.Context) {
	if w == nil || w.Pool == nil || w.Tracker == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	work := w.Tracker.drain(w.now(), 0)
	// The families are scanned by a later Run, if any: put them back.
	w.Tracker.requeue(work.families)
	if len(work.devices) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopFlushTimeout)
	defer cancel()
	flushed, err := w.flushOnly(ctx, work.devices)
	if err != nil {
		w.Tracker.restore(drained{devices: work.devices})
		if w.Logger != nil {
			w.Logger.Warn("wake progress lost at stop", slog.Int("devices", len(work.devices)),
				slog.Bool("timeout", errors.Is(err, context.DeadlineExceeded)))
		}
		return
	}
	if w.Logger != nil {
		w.Logger.Info("wake progress flushed at stop", slog.Int("devices", len(work.devices)), slog.Int("rows", flushed))
	}
}

// flushOnly runs the timeouts and Tick's flush in one implicit transaction
// inside a worker slot, and returns the rows written.
func (w *Worker) flushOnly(ctx context.Context, devices []deviceProgress) (int, error) {
	if w.Slots != nil {
		release, err := w.Slots.Acquire(ctx)
		if err != nil {
			return 0, err
		}
		defer release()
	}
	families, ids := make([]string, len(devices)), make([]string, len(devices))
	acked, delivered := make([]int64, len(devices)), make([]int64, len(devices))
	seen := make([]time.Time, len(devices))
	for i, d := range devices {
		families[i], ids[i], acked[i], delivered[i], seen[i] = d.FamilyID, d.DeviceID, int64(d.Acked), int64(d.Delivered), d.Seen
	}
	batch := &pgx.Batch{}
	batch.Queue(sqlTimeouts)
	batch.Queue(sqlFlush, families, ids, acked, delivered, seen)
	results := w.Pool.SendBatch(ctx, batch)
	_, err := results.Exec()
	var tag pgconn.CommandTag
	if err == nil {
		tag, err = results.Exec()
	}
	if closeErr := results.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return 0, fmt.Errorf("wake: flush at stop: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// droppedLogInterval rate-limits the tracker-dropped warning.
const droppedLogInterval = time.Minute

// Run ticks every Interval while ready (the node's cached readiness) allows,
// until ctx ends. A failed tick is logged (a send failure apart from a
// database failure, which carries its SQLSTATE but never the database's
// text); its work waits in the tracker for the next one. Progress the
// tracker dropped (past its bound, or invalid) is logged at most once a
// minute. When ctx ends it applies the kept settles, then flushes the
// tracker's progress once more (each within 2 s), before it returns.
func (w *Worker) Run(ctx context.Context, ready func(context.Context) error) {
	ticker := time.NewTicker(w.interval())
	defer ticker.Stop()
	var reported uint64
	var reportedAt time.Time
	defer func() {
		w.applySettlesOnStop(ctx)
		w.FlushTracker(ctx)
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if w.Tracker != nil && w.Logger != nil {
			if dropped, now := w.Tracker.Dropped(), w.now(); dropped > reported && (reportedAt.IsZero() || now.Sub(reportedAt) >= droppedLogInterval) {
				w.Logger.Warn("wake tracker dropped progress", slog.Uint64("dropped_total", dropped), slog.Uint64("dropped_since_last_log", dropped-reported))
				reported, reportedAt = dropped, now
			}
		}
		if ready != nil && ready(ctx) != nil {
			continue
		}
		tickContext, cancel := context.WithTimeout(ctx, tickTimeout)
		_, err := w.Tick(tickContext)
		cancel()
		if err == nil || ctx.Err() != nil || w.Logger == nil {
			continue
		}
		var pgErr *pgconn.PgError
		sqlstate := ""
		if errors.As(err, &pgErr) {
			sqlstate = pgErr.Code
		}
		timeout := errors.Is(err, context.DeadlineExceeded)
		var sendErr *SendError
		if errors.As(err, &sendErr) {
			w.Logger.Warn("wake worker send failed", slog.String("sqlstate", sqlstate), slog.Bool("timeout", timeout))
			continue
		}
		// The database's own text may name hosts or users: log its class.
		w.Logger.Warn("wake worker tick failed", slog.String("sqlstate", sqlstate), slog.Bool("timeout", timeout))
	}
}
