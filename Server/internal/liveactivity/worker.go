package liveactivity

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// main's worker (main:Server/internal/liveactivity/worker.go) on the
// branch: the queue and every maintenance call run on the maintenance pool,
// each statement inside one of its worker slots; the snapshot is the
// authority's (authority.WithLiveActivitySnapshot) on the runtime pool;
// committed family changes arrive through Dirty (the post-commit change
// feed) and are flushed before every claim; the registration sweep, the
// work sweep and retention run once a minute.

// codedError is an authority error code (main's security.IsCodedError):
// the authority, not one job's data, refused or failed.
func codedError(err error) bool {
	var code authority.Code
	return errors.As(err, &code)
}

const WorkerBatchLimit = 32
const WorkerPollInterval = time.Second
const WorkerSendTimeout = 5 * time.Second

// maximumTrackedFailures bounds the per-job backoff memory; past it the
// counts restart, which only shortens one backoff.
const maximumTrackedFailures = 4096

// ErrDependencyUnavailable marks a failure of the worker's own dependencies
// (application readiness, database, credentials), as opposed to one job's
// data. Such failures stop the worker for its supervisor.
var ErrDependencyUnavailable = errors.New("live_activity_dependency_unavailable")

// IsInfrastructureError reports whether err means the worker itself cannot
// work — cancellation, its database or authority dependencies, a schema or
// privilege mismatch — rather than that one job failed. A lost database race
// (IsTransientConflict) is retried promptly; anything else is a per-job
// failure: that job is parked and the worker carries on.
func IsInfrastructureError(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrDependencyUnavailable) || codedError(err) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if len(pgErr.Code) < 2 {
			return true
		}
		switch pgErr.Code[:2] {
		// Connection, authorization, database, schema/privilege, resources,
		// operator intervention, system and internal errors.
		case "08", "28", "3D", "42", "53", "57", "58", "XX":
			return true
		}
		return false
	}
	var connect *pgconn.ConnectError
	var network net.Error
	return errors.As(err, &connect) || errors.As(err, &network) || pgconn.Timeout(err) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// IsTransientConflict reports a serialization failure or deadlock (SQLSTATE
// class 40): a race the statement lost once and wins on a retry, so neither
// the worker nor the job is at fault. Such a job is retried after the shortest
// backoff with no failure counted against it, never parked as dispatch_failed.
func IsTransientConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && len(pgErr.Code) >= 2 && pgErr.Code[:2] == "40"
}

// Fixed sentinels of this package and the database package: lowercase words
// only, so nothing but the name itself can match.
var safeSentinel = regexp.MustCompile(`^(live_activity|database)_[a-z_]{1,48}$`)

// SafeErrorCode names an error for logs without its message: messages may
// carry data, codes never do.
func SafeErrorCode(err error) string {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, ErrDependencyUnavailable):
		return "dependency_unavailable"
	case errors.Is(err, ErrStaleClaim):
		return "stale_claim"
	case errors.Is(err, authority.ErrLiveActivityAuthorizationChanged):
		return "authorization_changed"
	case codedError(err):
		return authority.CodeOf(err)
	case errors.As(err, &pgErr):
		return "sqlstate_" + pgErr.Code
	case errors.Is(err, pgx.ErrNoRows):
		return "no_rows"
	case safeSentinel.MatchString(err.Error()):
		return err.Error()
	}
	var connect *pgconn.ConnectError
	var network net.Error
	if errors.As(err, &connect) || errors.As(err, &network) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "connection"
	}
	return "internal"
}

// PushExpiration bounds how long APNs may store an update or end without its
// own deadline. It is measured from the request's presentation timestamp, which
// the reconciler sets to the send decision's current second.
const PushExpiration = 8 * time.Hour

var processSends = make(chan struct{}, 2)

// SnapshotGuard is the authority's Live Activity snapshot
// (authority.Service.WithLiveActivitySnapshot).
type SnapshotGuard interface {
	WithLiveActivitySnapshot(context.Context, authority.LiveActivitySubject, func(pgx.Tx, access.Grant) error) error
}

// ErrWorkerUnavailable is Ready's answer while the worker has not completed
// a full pass since it (re)started, or has stopped.
var ErrWorkerUnavailable = errors.New("live_activity_worker_unavailable")

// maximumSweepPages bounds one minute's registration sweep: each page
// revokes up to SweepRegistrationsLimit registrations, so this many pages
// cover any realistic node; the rest wait for the next minute.
const maximumSweepPages = 100

type Worker struct {
	// Queue is on the maintenance pool.
	Queue *Queue
	Guard SnapshotGuard
	// Transport is the APNs transport (APNSTransport over the one client).
	Transport Transport
	// Dirty is the post-commit change feed's record of committed family
	// advances, flushed before every claim. Nil: only the sweep dirties.
	Dirty *Dirty
	// Slots are the maintenance pool's worker slots: every queue statement
	// runs inside one, so the readiness audit keeps its connection and the
	// other maintenance workers (wake, export, purge) take theirs in turn.
	// Nil runs without slots (tests).
	Slots *database.WorkerSlots
	// RequireEntitlement is hosted mode's family_sync rule for the
	// registration sweep.
	RequireEntitlement bool
	Now                func() time.Time
	Jitter             func(time.Duration) time.Duration
	// Logger receives bounded, token-free lines: one per send APNs did not
	// accept, one per parked job and one per minute's sweep. Nil logs to
	// slog.Default().
	Logger      *slog.Logger
	maintenance sync.Mutex
	nextSweep   time.Time
	failureMu   sync.Mutex
	failures    map[string]int
	// ticker supplies Run's poll and maintenance ticks; tests substitute one
	// they control. Nil uses time.NewTicker.
	ticker func(time.Duration) (<-chan time.Time, func())
	// running is set while Run's runners run; passed once one of them has
	// completed a full pass (sweep, dirty flush, claims) since.
	running, passed atomic.Bool
	// retryWait is Run's wait before it resumes; nil waits on a timer.
	retryWait func(context.Context, time.Duration) error
	// claims times the claim calls between two sweeps, for the sweep's log
	// line.
	claims claimTimes
}

// claimTimes are the claim calls since the last sweep: how many, how many
// claimed a job, and their latency (the worker slot wait and the
// statement), summed and at most.
type claimTimes struct {
	mu             sync.Mutex
	calls, claimed int
	total, longest time.Duration
}

func (c *claimTimes) observe(took time.Duration, claimed int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.claimed += claimed
	c.total += took
	c.longest = max(c.longest, took)
}

// take returns the log attributes of the calls so far and starts again.
func (c *claimTimes) take() []any {
	c.mu.Lock()
	defer c.mu.Unlock()
	mean := time.Duration(0)
	if c.calls > 0 {
		mean = c.total / time.Duration(c.calls)
	}
	attrs := []any{"claim_calls", c.calls, "claimed", c.claimed, "claim_mean_us", mean.Microseconds(), "claim_max_us", c.longest.Microseconds()}
	c.calls, c.claimed, c.total, c.longest = 0, 0, 0, 0
	return attrs
}

func NewWorker(q *Queue, g SnapshotGuard, t Transport, d *Dirty, slots *database.WorkerSlots) *Worker {
	return &Worker{Queue: q, Guard: g, Transport: t, Dirty: d, Slots: slots, Now: time.Now, Jitter: func(d time.Duration) time.Duration { return time.Duration(float64(d) * (0.8 + rand.Float64()*0.4)) }}
}

func (w *Worker) tick(d time.Duration) (<-chan time.Time, func()) {
	if w.ticker != nil {
		return w.ticker(d)
	}
	ticker := time.NewTicker(d)
	return ticker.C, ticker.Stop
}

func (w *Worker) logger() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}

// inSlot runs one queue statement inside a maintenance worker slot. The
// worker never holds a slot across a snapshot, a send or another slot.
func (w *Worker) inSlot(ctx context.Context, work func(context.Context) error) error {
	if w.Slots == nil {
		return work(ctx)
	}
	return w.Slots.Do(ctx, work)
}

// Ready is nil after the first full pass (sweep, dirty flush, claim) since
// Run last (re)started its runners, until Run stops them. The HTTP layer
// advertises live-activity-push-v1 and accepts registrations only then.
func (w *Worker) Ready(context.Context) error {
	if w == nil || !w.running.Load() || !w.passed.Load() {
		return ErrWorkerUnavailable
	}
	return nil
}

// Run supervises the worker until ctx ends, and then returns ctx's error.
// It starts the runners (run) once ready (the node's cached readiness;
// nil: always) answers nil. When they stop — the node became unready, or
// an infrastructure failure (IsInfrastructureError) — it logs the code,
// waits (2 s, doubling to a minute) and starts them again once ready.
func (w *Worker) Run(ctx context.Context, ready func(context.Context) error) error {
	delay := 2 * time.Second
	for {
		if ready != nil {
			if err := ready(ctx); err != nil {
				if waitErr := w.wait(ctx, WorkerPollInterval); waitErr != nil {
					return waitErr
				}
				continue
			}
		}
		started := time.Now()
		stopped := w.run(ctx, ready)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(started) > time.Minute {
			delay = 2 * time.Second
		}
		// A code only: an error's message may carry data.
		w.logger().Error("live activity worker stopped", "code", SafeErrorCode(stopped), "retry_in", delay.String())
		if err := w.wait(ctx, delay); err != nil {
			return err
		}
		delay = min(delay*2, time.Minute)
	}
}

func (w *Worker) wait(ctx context.Context, d time.Duration) error {
	if w.retryWait != nil {
		return w.retryWait(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// run keeps two bounded runners, so a slow send does not stop the
// one-second scheduling poll. Each runner checks readiness, then drains due
// work — up to WorkerBatchLimit claims per pass — before it waits for the
// next tick. A separate minute tick sweeps authority even at full capacity.
// It returns the first runner's failure, after joining them all.
func (w *Worker) run(ctx context.Context, ready func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w.maintenance.Lock()
	w.nextSweep = time.Time{} // every (re)start sweeps at once
	w.maintenance.Unlock()
	w.running.Store(true)
	defer func() {
		w.running.Store(false)
		w.passed.Store(false)
	}()
	failures := make(chan error, 3)
	var runners sync.WaitGroup
	for i := 0; i < 2; i++ {
		runners.Add(1)
		go func() {
			defer runners.Done()
			ticks, stop := w.tick(WorkerPollInterval)
			defer stop()
			for {
				if ready != nil {
					if err := ready(ctx); err != nil {
						failures <- errors.Join(ErrDependencyUnavailable, err)
						return
					}
				}
				if err := w.RunOnce(ctx, WorkerBatchLimit); err != nil {
					failures <- err
					return
				}
				w.passed.Store(true)
				select {
				case <-ctx.Done():
					failures <- ctx.Err()
					return
				case <-ticks:
				}
			}
		}()
	}
	runners.Add(1)
	go func() {
		defer runners.Done()
		ticks, stop := w.tick(time.Minute)
		defer stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticks:
				if err := w.maintain(ctx); err != nil {
					failures <- err
					return
				}
			}
		}
	}()
	err := <-failures
	cancel()
	runners.Wait()
	return err
}

// maintain, once a minute: the registration sweep (repeated while a page
// comes back full), the work sweep (main's claim ran it; page after page
// until one comes back short), then retention. A completed sweep logs one
// line at Info, numbers only: its duration, the registration pages and
// revocations, the work-sweep pages, the rows retention removed, and the
// claim calls since the last sweep with their mean and longest latency.
func (w *Worker) maintain(ctx context.Context) error {
	w.maintenance.Lock()
	defer w.maintenance.Unlock()
	if !w.Now().Before(w.nextSweep) {
		started := time.Now()
		pages, revokedTotal := 0, 0
		for page := 0; page < maximumSweepPages; page++ {
			var revoked int
			if err := w.inSlot(ctx, func(ctx context.Context) (err error) {
				revoked, err = w.Queue.SweepRegistrations(ctx, w.RequireEntitlement)
				return err
			}); err != nil {
				return err
			}
			pages, revokedTotal = pages+1, revokedTotal+revoked
			if revoked < SweepRegistrationsLimit {
				break
			}
		}
		// The work sweep commits page by page, each in its own slot, so no
		// family's dirty row stays locked for the whole pass.
		workPages := 0
		for after := ""; ; {
			if err := w.inSlot(ctx, func(ctx context.Context) (err error) {
				after, err = w.Queue.SweepWorkPage(ctx, after)
				return err
			}); err != nil {
				return err
			}
			workPages++
			if after == "" {
				break
			}
		}
		var retained int64
		if err := w.inSlot(ctx, func(ctx context.Context) (err error) { retained, err = w.Queue.Retain(ctx); return err }); err != nil {
			return err
		}
		w.nextSweep = w.Now().Add(time.Minute)
		attrs := append([]any{"duration_ms", time.Since(started).Milliseconds(), "pages", pages, "revoked", revokedTotal, "work_pages", workPages, "retained", retained},
			w.claims.take()...)
		w.logger().Info("live activity sweep", attrs...)
	}
	return nil
}

// flush dirties the families Dirty holds (Queue.FlushDirty, in batches of
// MaximumDirtyBatch). A batch that fails is put back with every later one,
// newer sequences recorded meanwhile winning, and the error returned.
func (w *Worker) flush(ctx context.Context) error {
	families, sequences := w.Dirty.take()
	for start := 0; start < len(families); start += MaximumDirtyBatch {
		end := min(start+MaximumDirtyBatch, len(families))
		if err := w.inSlot(ctx, func(ctx context.Context) error {
			_, err := w.Queue.FlushDirty(ctx, families[start:end], sequences[start:end])
			return err
		}); err != nil {
			w.Dirty.restore(families[start:], sequences[start:])
			return err
		}
	}
	return nil
}

func (w *Worker) RunOnce(ctx context.Context, limit int) error {
	if limit <= 0 || limit > WorkerBatchLimit {
		limit = WorkerBatchLimit
	}
	// Acquire capacity before claiming, so no queued tail can exhaust its lease.
	for i := 0; i < limit; i++ {
		if err := w.maintain(ctx); err != nil {
			return err
		}
		// Every claim follows a flush of the committed changes recorded so far.
		if err := w.flush(ctx); err != nil {
			return err
		}
		select {
		case processSends <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		var claims []Claim
		claimStarted := time.Now()
		err := w.inSlot(ctx, func(ctx context.Context) (err error) {
			claims, err = w.Queue.Claim(ctx, w.Now(), 1)
			return err
		})
		if err == nil {
			w.claims.observe(time.Since(claimStarted), len(claims))
		}
		if err != nil {
			<-processSends
			return err
		}
		if len(claims) == 0 {
			<-processSends
			return nil
		}
		err = w.dispatch(ctx, claims[0])
		if errors.Is(err, ErrStaleClaim) || errors.Is(err, authority.ErrLiveActivityAuthorizationChanged) {
			// Dispatch has returned, so no send for this lease is in flight. A
			// release that fails leaves the lease to expire; it is logged by code
			// only, and the worker carries on with the rest of the queue.
			if released := w.releaseStaleLease(ctx, claims[0]); released != nil {
				w.logger().Warn("live activity stale lease not released", "kind", claims[0].Kind, "code", SafeErrorCode(released))
			}
		}
		<-processSends
		switch {
		case err == nil:
			w.forgetFailures(claims[0].JobID)
		case errors.Is(err, ErrStaleClaim) || errors.Is(err, authority.ErrLiveActivityAuthorizationChanged):
		case IsInfrastructureError(ctx, err):
			return err
		case IsTransientConflict(err):
			if err = w.deferTransient(ctx, claims[0], err); err != nil {
				return err
			}
		default:
			if err = w.park(ctx, claims[0], err); err != nil {
				return err
			}
		}
	}
	return nil
}

// The queue calls dispatch makes, each inside a worker slot.
func (w *Worker) loadTarget(ctx context.Context, c Claim) (state TargetState, err error) {
	err = w.inSlot(ctx, func(ctx context.Context) error { state, err = w.Queue.LoadTarget(ctx, c); return err })
	return state, err
}

func (w *Worker) saveTarget(ctx context.Context, c Claim, s TargetState, finish bool, deadline time.Time, invalidate *TokenFence) error {
	return w.inSlot(ctx, func(ctx context.Context) error { return w.Queue.SaveTarget(ctx, c, s, finish, deadline, invalidate) })
}

func (w *Worker) saveOutcome(ctx context.Context, c Claim, s TargetState, deadline time.Time, invalidate *TokenFence, outcome *PushOutcome) error {
	return w.inSlot(ctx, func(ctx context.Context) error { return w.Queue.SaveOutcome(ctx, c, s, deadline, invalidate, outcome) })
}

// deferTransient releases a job that lost a database race: the shortest
// backoff the first time, further along the ordinary progression (capped at
// 60 s) for a job that keeps losing, and never dispatch_failed. A delivery
// forgets the losses, as it does a parked job's failures.
func (w *Worker) deferTransient(ctx context.Context, c Claim, cause error) error {
	attempt := w.countFailure(c.JobID)
	delay := retryDelay(attempt, w.Jitter)
	w.logger().Info("live activity job deferred", "kind", c.Kind, "code", SafeErrorCode(cause), "attempt", attempt, "retry_in", delay.Round(time.Second).String())
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), WorkerSendTimeout)
	defer cancel()
	err := w.inSlot(cleanup, func(ctx context.Context) error {
		return w.Queue.Retry(ctx, c, w.Now().Add(delay), "transient_conflict")
	})
	if errors.Is(err, ErrStaleClaim) {
		return w.releaseStaleLease(ctx, c)
	}
	return err
}

// park backs one deterministically failing job off, with its reason recorded
// on the job, so the rest of the queue keeps delivering. Dispatch has returned:
// no send for this lease is in flight, and any persisted start intent keeps its
// no-blind-resend rule when the job is claimed again.
func (w *Worker) park(ctx context.Context, c Claim, cause error) error {
	attempt := w.countFailure(c.JobID)
	delay := retryDelay(attempt, w.Jitter)
	w.logger().Warn("live activity job parked", "kind", c.Kind, "code", SafeErrorCode(cause), "attempt", attempt, "retry_in", delay.Round(time.Second).String())
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), WorkerSendTimeout)
	defer cancel()
	err := w.inSlot(cleanup, func(ctx context.Context) error { return w.Queue.Retry(ctx, c, w.Now().Add(delay), "dispatch_failed") })
	if errors.Is(err, ErrStaleClaim) {
		// Newer work arrived meanwhile: release the lease so it is claimed again.
		return w.releaseStaleLease(ctx, c)
	}
	return err
}

// countFailure records one more failed dispatch of job and returns how many
// in a row it has had, bounded by maximumTrackedFailures.
func (w *Worker) countFailure(job string) int {
	w.failureMu.Lock()
	defer w.failureMu.Unlock()
	if w.failures == nil || len(w.failures) >= maximumTrackedFailures {
		w.failures = make(map[string]int)
	}
	w.failures[job]++
	return w.failures[job]
}

func (w *Worker) forgetFailures(job string) {
	w.failureMu.Lock()
	delete(w.failures, job)
	w.failureMu.Unlock()
}

type dispatchSnapshot struct {
	desired            *DesiredCard
	state              TargetState
	environment, token string
	tokenVersion       uint64
	duplicate          *duplicateToken
}

type duplicateToken struct {
	ID, Token, GenerationID string
	Version                 uint64
	TimerID, SessionID      *string
}

func (w *Worker) snapshot(ctx context.Context, c Claim, state TargetState) (dispatchSnapshot, error) {
	out := dispatchSnapshot{state: state}
	err := w.Guard.WithLiveActivitySnapshot(ctx, SubjectFromClaim(c), func(tx pgx.Tx, g access.Grant) error {
		var preference int
		var push *string
		var registeredSequence uint64
		if err := tx.QueryRow(ctx, `SELECT environment,push_token,feed_break_minutes,registered_sequence FROM live_activity_devices WHERE registration_id=$1`, c.RegistrationID).Scan(&out.environment, &push, &preference, &registeredSequence); err != nil {
			return err
		}
		if push != nil {
			out.token = *push
		}
		out.tokenVersion = c.RegistrationVersion
		care, err := (SnapshotReader{}).Read(ctx, tx, g, c.BabyID)
		if err != nil {
			return err
		}
		out.state.LastSequence = care.Cursor.Sequence
		cards, err := Project(care, DevicePreferences{preference}, w.Now())
		if err != nil {
			return err
		}
		for i := range cards {
			if cards[i].Scope.KindRaw == c.Kind {
				out.desired = &cards[i]
				break
			}
		}
		// A first feed part has no session UUID until its stop creates the root
		// feed. Its break, and a next part started in that record's session (a
		// side switch), continue the same lifecycle. Follow that committed
		// timer-to-record link, not time proximity or a client presentation claim.
		if c.Kind == "feed" && out.desired != nil && state.TimerID != nil && state.SessionID == nil && out.desired.SessionID != nil && !referenceEqual(state.TimerID, out.desired.TimerID) {
			completed, completedSession, err := ReadCompletedSession(ctx, tx, c.FamilyID, *state.TimerID)
			if err != nil {
				return err
			}
			if referenceEqual(completed, out.desired.SessionID) || referenceEqual(completedSession, out.desired.SessionID) {
				out.state.SessionID = out.desired.SessionID
			}
		}
		if out.desired != nil && out.desired.TimerID != nil {
			// The timer's first version's device and sequence (care.Encode's
			// origin): the phone that started it after it registered shows its
			// own local card and gets no push-to-start.
			origin, err := ReadTimerOrigin(ctx, tx, c.FamilyID, *out.desired.TimerID)
			if err != nil {
				return err
			}
			out.state.SuppressStart = origin.Sequence != 0 && origin.DeviceID == c.DeviceID && origin.Sequence > registeredSequence
		}
		// Byte order, whatever the database's collation: the phone keeps the
		// smallest activity ID by Swift's String comparison, so the first card
		// here is the one the phone itself keeps and any later one is the duplicate.
		rows, err := tx.Query(ctx, `SELECT activity_id,token,token_version,generation_id::text,known_generation_id::text,timer_id::text,session_id::text,state,server_retired FROM live_activity_tokens WHERE registration_id=$1 AND baby_id=$2 AND kind=$3 ORDER BY activity_id COLLATE "C"`, c.RegistrationID, c.BabyID, c.Kind)
		if err != nil {
			return err
		}
		tokens, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (tokenRow, error) {
			var r tokenRow
			err := row.Scan(&r.id, &r.token, &r.version, &r.generation, &r.known, &r.timer, &r.session, &r.status, &r.serverRetired)
			return r, err
		})
		if err != nil {
			return err
		}
		out.state.ActivityID = ""
		out.state.TokenVersion = 0
		out.state.KnownActivityIDs = nil
		basis := state
		if adopted := adoptOwnCard(state, out.desired, c.Kind, tokens); adopted != nil {
			// A new lifecycle whose card this phone registered before the worker
			// saw it: that card's generation is the lifecycle's, at once.
			basis = TargetState{GenerationID: adopted.generation, Kind: c.Kind, TimerID: adopted.timer, SessionID: adopted.session, Status: "pending", LastSequence: out.state.LastSequence, SuppressStart: out.state.SuppressStart}
			out.state = basis
		}
		// A card that only names the desired card's timer or session belongs to
		// this lifecycle only while the lifecycle is that one (the committed
		// continuation link included) or has no generation yet. Otherwise the
		// card is the next lifecycle's — the phone's own new card registered
		// before the previous card has ended — and is never picked for the
		// previous lifecycle's end; it is adopted once that one is terminal.
		// An ended or dismissed card joins by reference only while the lifecycle
		// has no generation (the phone dismissed its provisional card before the
		// first pass); once it has one, such a card is an older lifecycle's —
		// the swiped break card still naming a session resumed later — and must
		// not relabel itself through the shared session and dismiss the new one.
		referencesDesired := out.desired != nil && basis.Status != "terminal" && (basis.GenerationID == "" || sameLifecycle(out.state, out.desired))
		receiverEnded := false
		for _, r := range tokens {
			matches := r.claims(basis.GenerationID)
			if !matches && referencesDesired && (r.status == "active" || basis.GenerationID == "") {
				matches = r.refersTo(out.desired, c.Kind)
			}
			if !matches {
				if r.status == "active" && r.token != nil && r.known != nil && basis.GenerationID != "" && *r.known != basis.GenerationID {
					out.duplicate = &duplicateToken{ID: r.id, Token: *r.token, Version: r.version, GenerationID: *r.known, TimerID: r.timer, SessionID: r.session}
				}
				continue
			}
			// A matching local card establishes the lifecycle even when its first
			// registration already reports receiver dismissal or ending.
			if out.state.GenerationID == "" && !r.serverRetired {
				out.state.GenerationID = r.generation
				out.state.Kind = c.Kind
				out.state.TimerID = r.timer
				out.state.SessionID = r.session
				out.state.Status = "pending"
			}
			// Only the receiver's own ending is a dismissal. A card the server
			// itself retired (a duplicate end, an accepted end, a dead token)
			// stays retired whatever the phone later reports about it.
			if (r.status == "dismissed" || r.status == "ended") && !r.serverRetired {
				receiverEnded = true
			}
			if r.status != "active" || r.token == nil {
				continue
			}
			out.state.KnownActivityIDs = append(out.state.KnownActivityIDs, r.id)
			if out.state.ActivityID == "" {
				out.state.ActivityID = r.id
				out.state.TokenVersion = r.version
				out.token = *r.token
				out.tokenVersion = r.version
			} else if r.claims(out.state.GenerationID) {
				out.duplicate = &duplicateToken{ID: r.id, Token: *r.token, Version: r.version, GenerationID: out.state.GenerationID, TimerID: r.timer, SessionID: r.session}
			}
		}
		// The receiver dismissed the lifecycle only when none of its cards is
		// left. A phone that ends one of two cards itself (its provisional card
		// giving way to the pushed one, a card replaced after a relaunch) still
		// shows the other: that card keeps its updates and gets the end. Read as
		// a dismissal, its end was decided on every pass and abandoned before
		// the send, and no later lifecycle could start on the phone.
		switch {
		case out.state.ActivityID != "":
			if out.state.DismissedGeneration == out.state.GenerationID {
				out.state.DismissedGeneration = ""
			}
		case receiverEnded:
			out.state.DismissedGeneration = out.state.GenerationID
		}
		return nil
	})
	return out, err
}

// tokenRow is one registered card as the snapshot reads it. Its token never
// leaves the dispatch that is about to send to it.
type tokenRow struct {
	id, generation, status       string
	token, known, timer, session *string
	version                      uint64
	serverRetired                bool
}

// claims: the card is registered against this generation.
func (r tokenRow) claims(generation string) bool {
	return generation != "" && (r.generation == generation || (r.known != nil && *r.known == generation))
}

// refersTo: the card names the desired card's timer, or, timer-less, its feed
// session.
func (r tokenRow) refersTo(desired *DesiredCard, kind string) bool {
	return referenceEqual(r.timer, desired.TimerID) || (r.timer == nil && kind == "feed" && referenceEqual(r.session, desired.SessionID))
}

// adoptOwnCard finds the card a phone requested for a new lifecycle when the
// target has no live one: the phone that started a timer registers its own
// card bound to it, often before the worker has seen the start. Nothing is
// adopted while an earlier lifecycle's card still awaits its end, nor from a
// card the server retired. Only an active card with a token is a candidate: a
// phone-reported ended or dismissed card that still names the desired timer or
// session is an older lifecycle's (a swiped break card, the same feed session
// resumed later) and would otherwise freeze the phone's new card.
func adoptOwnCard(state TargetState, desired *DesiredCard, kind string, tokens []tokenRow) *tokenRow {
	if desired == nil || state.Status != "terminal" {
		return nil
	}
	for _, r := range tokens {
		if r.claims(state.GenerationID) && r.status == "active" && r.token != nil {
			return nil
		}
	}
	for i := range tokens {
		r := &tokens[i]
		if !r.serverRetired && r.status == "active" && r.token != nil && !r.claims(state.GenerationID) && r.refersTo(desired, kind) {
			return r
		}
	}
	return nil
}

func (w *Worker) dispatch(ctx context.Context, c Claim) error {
	state, err := w.loadTarget(ctx, c)
	if err != nil {
		return err
	}
	snapshot, err := w.snapshot(ctx, c, state)
	if err != nil {
		return err
	}
	decision := Reconcile(snapshot.state, snapshot.desired, w.Now())
	duplicate := snapshot.duplicate
	if duplicate != nil && decision.State.GenerationID != "" && decision.State.DismissedGeneration != decision.State.GenerationID && decision.State.Status != "terminal" {
		if snapshot.state.PendingEvent == "duplicateEnd" && snapshot.state.RetryAt != nil && w.Now().Before(*snapshot.state.RetryAt) {
			decision = Decision{"wait", snapshot.state, *snapshot.state.RetryAt}
		} else if w.Now().Unix() <= snapshot.state.LastTimestamp {
			decision = Decision{"wait", snapshot.state, time.Unix(snapshot.state.LastTimestamp+1, 0)}
		} else {
			decision = Decision{"end", snapshot.state, w.Now().Add(time.Second)}
			decision.State.LastTimestamp = w.Now().Unix()
			decision.State.PresentationVersion++
			decision.State.PendingEvent = "duplicateEnd"
			decision.State.Attempts++
			decision.State.RetryAt = nil
			decision.State.AttemptID = deterministicID(decision.State.GenerationID + "/duplicate/" + duplicate.ID + "/" + w.Now().String())
		}
	}
	if decision.Event == "none" || decision.Event == "wait" || snapshot.token == "" {
		return w.saveTarget(ctx, c, decision.State, true, decision.Deadline, nil)
	}
	// Write intent before I/O. A crash now leaves starts unconfirmed, even if the
	// connection later dies after Apple accepted them.
	if err = w.saveTarget(ctx, c, decision.State, false, time.Time{}, nil); err != nil {
		return err
	}
	latest, err := w.snapshot(ctx, c, decision.State)
	if err != nil {
		return errors.Join(err, w.abandonUnsentStart(ctx, c, decision, snapshot.state))
	}
	if desiredDigest(latest.desired) != desiredDigest(snapshot.desired) || latest.tokenVersion != snapshot.tokenVersion || latest.token != snapshot.token || latest.state.DismissedGeneration == decision.State.GenerationID {
		if decision.Event == "start" {
			return w.abandonUnsentStart(ctx, c, decision, snapshot.state)
		}
		return w.saveTarget(ctx, c, decision.State, true, w.Now().Add(time.Second), nil)
	}
	if !w.Now().Add(WorkerSendTimeout).Before(c.LeaseDeadline) {
		return errors.Join(ErrStaleClaim, w.abandonUnsentStart(ctx, c, decision, snapshot.state))
	}
	if decision.State.PendingEvent == "duplicateEnd" {
		if latest.duplicate == nil || latest.duplicate.ID != duplicate.ID || latest.duplicate.Token != duplicate.Token || latest.duplicate.Version != duplicate.Version || latest.duplicate.GenerationID != duplicate.GenerationID {
			return w.saveTarget(ctx, c, decision.State, true, w.Now().Add(time.Second), nil)
		}
		snapshot.token = duplicate.Token
	}
	request := makeRequest(c, decision, snapshot)
	if decision.State.PendingEvent == "duplicateEnd" {
		request.Content.Remote.GenerationID = duplicate.GenerationID
		request.Content.Remote.TimerID = duplicate.TimerID
		request.Content.Remote.SessionID = duplicate.SessionID
	}
	sendCtx, cancel := context.WithTimeout(ctx, WorkerSendTimeout)
	// A transport error carries no data the worker keeps: only the class does,
	// and whether the request was known not to have been written at all.
	result, sendErr := w.Transport.Send(sendCtx, request)
	cancel()
	notSent := errors.Is(sendErr, ErrRequestNotSent)
	if result.Classification == "" {
		// An error with no class, or no class at all, is an unknown outcome.
		result.Classification = Ambiguous
	}
	outcome := outcomeOf(result)
	if result.Classification != Accepted {
		status := 0
		if outcome.HTTPStatus != nil {
			status = *outcome.HTTPStatus
		}
		w.logger().Warn("live activity push not accepted", "event", decision.State.PendingEvent, "kind", c.Kind, "class", string(result.Classification), "status", status, "reason", outcome.Reason, "attempt", decision.State.Attempts, "sent", !notSent)
	}
	s := decision.State
	deadline := decision.Deadline
	var invalidate *TokenFence
	if notSent && result.Classification != Accepted && decision.Event == "start" {
		// Rejected before any byte reached Apple: this start is known-unsent.
		// Release the intent — status pending, no attempt nonce, the absence
		// authorization as it was — so the next attempt is a fresh start after
		// backoff, instead of a start-unconfirmed waiting for a phone's proof.
		// Only a transport that proves pre-I/O failure gets here; anything that
		// may have reached Apple keeps the ambiguity rules below.
		s.Status = "pending"
		s.PendingEvent, s.AttemptID, s.StartAttemptID = "", "", ""
		if snapshot.state.GenerationID == s.GenerationID {
			s.AbsenceRetryAllowed, s.AbsenceRetryUsed = snapshot.state.AbsenceRetryAllowed, snapshot.state.AbsenceRetryUsed
		}
		retry := w.Now().Add(retryDelay(s.Attempts, w.Jitter))
		s.RetryAt = &retry
		err = w.saveOutcome(ctx, c, s, retry, nil, outcome)
		if errors.Is(err, ErrStaleClaim) {
			// Newer work took the lease's acknowledgement: release the intent
			// through the lease-exact CAS instead, so no unsent start lingers.
			return errors.Join(err, w.abandonUnsentStart(ctx, c, decision, snapshot.state))
		}
		return err
	}
	switch result.Classification {
	case Accepted:
		s.PendingEvent = ""
		s.Attempts = 0
		if decision.State.PendingEvent == "duplicateEnd" {
			invalidate = &TokenFence{duplicate.ID, duplicate.Version}
		} else if decision.Event == "end" {
			invalidate = &TokenFence{s.ActivityID, s.TokenVersion}
			s.ActivityID = ""
		}
	case InvalidToken:
		version := snapshot.tokenVersion
		activity := s.ActivityID
		if decision.Event == "start" {
			activity = ""
		}
		invalidate = &TokenFence{activity, version}
		if decision.Event != "start" && decision.State.PendingEvent != "duplicateEnd" {
			s.DismissedGeneration = s.GenerationID
		}
		if decision.State.PendingEvent == "duplicateEnd" {
			invalidate = &TokenFence{duplicate.ID, duplicate.Version}
		}
		s.PendingEvent = ""
		s.ActivityID = ""
		s.TokenVersion = 0
	case Retryable:
		if decision.Event == "start" {
			s.Status = "pending"
		}
		retry := w.Now().Add(retryDelay(s.Attempts, w.Jitter))
		if result.RetryAfter.After(retry) {
			retry = result.RetryAfter
		}
		if maximum := w.Now().Add(time.Minute); retry.After(maximum) {
			retry = maximum
		}
		s.RetryAt = &retry
		deadline = retry
	default:
		if decision.Event != "start" {
			retry := w.Now().Add(retryDelay(s.Attempts, w.Jitter))
			s.RetryAt = &retry
			deadline = retry
		} else {
			s.PendingEvent = ""
		}
	}
	if s.ExpiresAt != nil && w.Now().Before(*s.ExpiresAt) && (deadline.IsZero() || s.ExpiresAt.Before(deadline)) {
		deadline = *s.ExpiresAt
	}
	return w.saveOutcome(ctx, c, s, deadline, invalidate, outcome)
}

// Only pre-transport branches may abandon an intent. Once Send is entered,
// cancellation and network errors must retain start-unconfirmed semantics.
func (w *Worker) abandonUnsentStart(ctx context.Context, c Claim, d Decision, previous TargetState) error {
	if d.Event != "start" {
		return nil
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), WorkerSendTimeout)
	defer cancel()
	err := w.inSlot(cleanup, func(ctx context.Context) error { return w.Queue.AbandonStart(ctx, c, d.State, previous) })
	if errors.Is(err, ErrStaleClaim) {
		// A reclaimed lease or a revoked/deleted target belongs to its new owner.
		return nil
	}
	return err
}

// A stale claim caused by newer dirty work is released at once, so the job is
// reclaimable at the next one-second poll instead of after its 30-second lease.
// Other stale causes (reclaimed lease, revocation) are left to their owner.
func (w *Worker) releaseStaleLease(ctx context.Context, c Claim) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), WorkerSendTimeout)
	defer cancel()
	if err := w.inSlot(cleanup, func(ctx context.Context) error { return w.Queue.ReleaseStale(ctx, c) }); err != nil && !errors.Is(err, ErrStaleClaim) {
		return err
	}
	return nil
}

func makeRequest(c Claim, d Decision, s dispatchSnapshot) PushRequest {
	state := d.State
	scope := ActivityScope{c.InstanceID, c.RecoveryEpoch, c.FamilyID, c.BabyID, c.Kind}
	content := ContentState{Phase: "running", ActivityKind: "paused", Parts: []PartLabel{}}
	name := ""
	var stale *time.Time
	expires := time.Time{}
	canWrite := false
	if s.desired != nil {
		content = s.desired.Content
		name = s.desired.BabyName
		stale = s.desired.StaleAt
		canWrite = s.desired.CanWrite
		if s.desired.ExpiresAt != nil {
			expires = *s.desired.ExpiresAt
		}
	}
	content.Remote = &Revision{Scope: scope, GenerationID: state.GenerationID, TimerID: state.TimerID, SessionID: state.SessionID, FamilySequence: state.LastSequence, PresentationVersion: state.PresentationVersion, PresentationTimestamp: uint64(state.LastTimestamp), CanWrite: canWrite}
	timestamp := time.Unix(state.LastTimestamp, 0)
	kind := UpdateToken
	switch {
	case d.Event == "start":
		kind = PushToStartToken
		expires = time.Time{}
	case d.Event == "end" || expires.IsZero():
		// APNs requires a positive expiration for every update and end. A late
		// copy is harmless: ActivityKit drops content older than the card's
		// timestamp, and an end targets one specific activity token.
		expires = timestamp.Add(PushExpiration)
	}
	priority := PriorityImmediate
	if d.Event == "update" && state.Priority == PriorityCosmetic {
		priority = PriorityCosmetic
	}
	return PushRequest{Environment: s.environment, Token: s.token, TokenKind: kind, APNSID: state.AttemptID, Event: d.Event, Timestamp: timestamp, Expiration: expires, Priority: priority, Attributes: ActivityAttributes{c.BabyID, name, c.Kind, scope}, Content: content, StaleAt: stale}
}
