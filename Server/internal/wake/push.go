package wake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"babytracker/server/internal/apns"
	"github.com/jackc/pgx/v5"
)

// Pusher sends one APNs background wake. It is *apns.Client.
type Pusher interface {
	Wake(ctx context.Context, env apns.Environment, token []byte) (apns.Result, error)
}

const (
	// APNsInterval: at most one background push per device every 20
	// minutes, three an hour (APNs ruling).
	APNsInterval = 20 * time.Minute
	// MaxPushesPerTick bounds the pushes one tick sends.
	MaxPushesPerTick = 100
	// pushConcurrency bounds the pushes in flight at once.
	pushConcurrency = 8
	// maxPushTargets is app.push_targets' bound on one lookup.
	maxPushTargets = 1000

	// The pause after Apple's answers (Task 15 contract).
	minBackoff         = 2 * time.Second // the first server error (5xx, a lost answer, a send refused before writing)
	maxBackoff         = 2 * time.Minute // doubling stops here
	minRetryAfterPause = time.Minute     // a 429 pauses at least this long
	maxRetryAfterPause = time.Hour       // and at most this long, whatever Retry-After says
	providerTokenPause = time.Minute     // 403 ExpiredProviderToken / InvalidProviderToken: the client re-signs
	configurationPause = 5 * time.Minute // any other refusal of this deployment's configuration
	settleTimeout      = 2 * time.Second // the post-send statement when the tick's context already ended
)

// pushTarget is a device's registered token, as app.push_targets returned it.
type pushTarget struct {
	env   apns.Environment
	token []byte
}

// answer is how the worker acts on one Wake result.
type answer int

const (
	answerAccepted      answer = iota // reset the backoff
	answerInvalidToken                // forget the token if it is still the one sent
	answerBackoff                     // a server error or an unknown outcome: back off 2 s doubling to 2 min
	answerRetryAfter                  // 429: pause until Retry-After, between a minute and an hour
	answerProviderToken               // 403 provider token refused: pause a minute
	answerConfiguration               // other configuration refusal: pause five minutes
)

// classify maps a Wake result in the contract's order: a send that never
// left and an unknown outcome first (their Class says nothing about Apple's
// verdict), then Apple's answers.
func classify(result apns.Result, err error) answer {
	switch {
	case errors.Is(err, apns.ErrNotSent):
		return answerBackoff
	case err != nil || result.Class == apns.Ambiguous:
		return answerBackoff
	case result.Class == apns.Accepted:
		return answerAccepted
	case result.Class == apns.InvalidToken:
		return answerInvalidToken
	case result.Status == 429:
		return answerRetryAfter
	case result.Class == apns.Retryable:
		return answerBackoff
	case result.Class == apns.ConfigurationFailure && result.Status == 403 &&
		(result.Reason == "ExpiredProviderToken" || result.Reason == "InvalidProviderToken"):
		return answerProviderToken
	case result.Class == apns.ConfigurationFailure:
		return answerConfiguration
	default:
		return answerBackoff
	}
}

// pauses reports whether an answer stops further sends.
func (a answer) pauses() bool { return a != answerAccepted && a != answerInvalidToken }

// undelivered reports a Wake result that proves Apple did not deliver the
// push: a send the client refused before writing, a 429, a server error, or
// a refusal of this deployment's configuration. Its sequence is owed again
// once the device's push window ends. A lost answer (Ambiguous, any error)
// may have been delivered: it is the one at-most-once loss.
func undelivered(result apns.Result, err error) bool {
	switch {
	case errors.Is(err, apns.ErrNotSent):
		return true
	case err != nil || result.Class == apns.Ambiguous:
		return false
	default:
		return result.Class == apns.Retryable || result.Class == apns.ConfigurationFailure
	}
}

// pauseState is the worker's APNs pause: no push is sent before until. It is
// worker state: a restart clears it, and Apple's answer to the next send
// restores it.
type pauseState struct {
	until   time.Time
	backoff time.Duration // the last server-error pause (0: none since the last acceptance)
}

func (p pauseState) paused(now time.Time) bool { return now.Before(p.until) }

// tickAnswers is what one tick's sends were answered.
type tickAnswers struct {
	accepted, backoff, retryAfter, provider, configuration bool
	retryUntil                                             time.Time // the latest Retry-After among 429s (zero: none)
}

func (t *tickAnswers) add(a answer, result apns.Result) {
	switch a {
	case answerAccepted:
		t.accepted = true
	case answerBackoff:
		t.backoff = true
	case answerRetryAfter:
		t.retryAfter = true
		if result.RetryAfter.After(t.retryUntil) {
			t.retryUntil = result.RetryAfter
		}
	case answerProviderToken:
		t.provider = true
	case answerConfiguration:
		t.configuration = true
	}
}

// after is the pause once a tick's answers arrived at now. Server errors
// step the backoff once a tick (2 s, doubling, at most 2 min); a tick with
// an acceptance and no server error resets it. A 429 pauses until
// max(Retry-After, now+1 min), at most now+1 h; a refused provider token a
// minute; any other configuration refusal five minutes. The longest stands.
func (p pauseState) after(answers tickAnswers, now time.Time) pauseState {
	next := p
	extend := func(until time.Time) {
		if until.After(next.until) {
			next.until = until
		}
	}
	switch {
	case answers.backoff:
		next.backoff = min(max(2*p.backoff, minBackoff), maxBackoff)
		extend(now.Add(next.backoff))
	case answers.accepted:
		next.backoff = 0
	}
	if answers.retryAfter {
		until := now.Add(minRetryAfterPause)
		if answers.retryUntil.After(until) {
			until = answers.retryUntil
		}
		if limit := now.Add(maxRetryAfterPause); until.After(limit) {
			until = limit
		}
		extend(until)
	}
	if answers.provider {
		extend(now.Add(providerTokenPause))
	}
	if answers.configuration {
		extend(now.Add(configurationPause))
	}
	return next
}

// pushJob is one reserved push: the device, the sequence its reservation
// marked, the reservation it replaced (restored if the push is never sent)
// and its target. shared are the other due rows registered with the same
// target (one phone on several device rows): reserved with it, and woken by
// its one send.
type pushJob struct {
	family, device string
	sequence       uint64
	previous       uint64
	previousAt     *time.Time
	target         pushTarget
	shared         []pushJob
}

// rowsOf is every device row the jobs reserved: each job and its shared rows.
func rowsOf(jobs []pushJob) []pushJob {
	var rows []pushJob
	for _, job := range jobs {
		rows = append(rows, job)
		rows = append(rows, job.shared...)
	}
	return rows
}

// loggedAnswer is what a log record may carry of an answer: its class,
// status and reason, never a token or the client's error text.
type loggedAnswer struct {
	class  string
	status int
	reason string
}

func loggedOf(result apns.Result, err error) *loggedAnswer {
	class := string(result.Class)
	switch {
	case errors.Is(err, apns.ErrNotSent):
		class = "notSent"
	case err != nil:
		class = string(apns.Ambiguous)
	}
	return &loggedAnswer{class: class, status: result.Status, reason: result.Reason}
}

// pushReport is what the tick's sends did.
type pushReport struct {
	// sent counts the sends made (a send the client refused before writing
	// is not one); failed counts those Apple did not accept and those never
	// sent.
	sent, failed int
	// notSentCount counts the sends the client refused before writing;
	// ambiguous the sends whose answer was lost (both within failed).
	notSentCount, ambiguous int
	answers                 tickAnswers
	// The first answer of each logged kind.
	notSent, backoff, retry, provider, configuration *loggedAnswer
	forget                                           []pushJob // refused tokens
	undelivered                                      []pushJob // proven not delivered: their sequence is owed again
	unsent                                           []pushJob // never started: their reservations are released
}

func (r *pushReport) record(job pushJob, result apns.Result, err error) answer {
	a := classify(result, err)
	notSent := errors.Is(err, apns.ErrNotSent)
	if notSent {
		r.notSentCount++
	} else {
		r.sent++
		if err != nil || result.Class == apns.Ambiguous {
			r.ambiguous++
		}
	}
	if a != answerAccepted {
		r.failed++
	}
	r.answers.add(a, result)
	first := func(slot **loggedAnswer) {
		if *slot == nil {
			*slot = loggedOf(result, err)
		}
	}
	switch {
	case a == answerInvalidToken:
		r.forget = append(r.forget, job)
	case notSent:
		first(&r.notSent)
	case a == answerBackoff:
		first(&r.backoff)
	case a == answerRetryAfter:
		first(&r.retry)
	case a == answerProviderToken:
		first(&r.provider)
	case a == answerConfiguration:
		first(&r.configuration)
	}
	if undelivered(result, err) {
		r.undelivered = append(r.undelivered, job)
	}
	return a
}

// sendPushes sends the tick's reserved pushes, each under the client's own
// bound: the first alone (it opens, or proves, the one HTTP/2 connection the
// rest share, and an answer that pauses sends stops the tick before any
// other leaves), then the rest at most pushConcurrency at once. Each push
// runs under sendCtx (the send budget, detached from the tick), so the
// tick's end never cancels one in flight. Once an answer pauses sends, or
// the tick's context or the budget ends, no further push is started: those
// are reported unsent.
func (w *Worker) sendPushes(ctx, sendCtx context.Context, jobs []pushJob) pushReport {
	var (
		mu     sync.Mutex
		report pushReport
		stop   bool
		wg     sync.WaitGroup
	)
	if len(jobs) == 0 {
		return report
	}
	ended := func() bool { return ctx.Err() != nil || sendCtx.Err() != nil }
	if ended() {
		report.unsent = jobs
		return report
	}
	first, err := w.Pusher.Wake(sendCtx, jobs[0].target.env, jobs[0].target.token)
	stop = report.record(jobs[0], first, err).pauses()
	slots := make(chan struct{}, pushConcurrency)
	for i := 1; i < len(jobs); i++ {
		job := jobs[i]
		acquired := false
		select {
		case slots <- struct{}{}:
			acquired = true
		case <-ctx.Done():
		case <-sendCtx.Done():
		}
		mu.Lock()
		halted := stop || ended()
		if halted {
			report.unsent = append(report.unsent, jobs[i:]...)
		}
		mu.Unlock()
		if halted {
			if acquired {
				<-slots
			}
			break
		}
		wg.Go(func() {
			defer func() { <-slots }()
			result, err := w.Pusher.Wake(sendCtx, job.target.env, job.target.token)
			mu.Lock()
			defer mu.Unlock()
			if report.record(job, result, err).pauses() {
				stop = true
			}
		})
	}
	wg.Wait()
	return report
}

// applyAnswers moves the pause by the tick's answers and logs, once a tick,
// each kind of refusal.
func (w *Worker) applyAnswers(report pushReport, now time.Time) {
	w.pause = w.pause.after(report.answers, now)
	logger := w.Logger
	if logger == nil {
		return
	}
	attrs := func(logged *loggedAnswer) []any {
		return []any{slog.String("class", logged.class), slog.Int("status", logged.status), slog.String("reason", logged.reason),
			slog.Time("paused_until", w.pause.until)}
	}
	if report.notSent != nil {
		logger.Error("apns wake not sent", attrs(report.notSent)...)
	}
	if report.configuration != nil {
		logger.Error("apns configuration refused", attrs(report.configuration)...)
	}
	if report.provider != nil {
		logger.Warn("apns provider token refused", attrs(report.provider)...)
	}
	if report.retry != nil {
		logger.Warn("apns wakes rate limited", attrs(report.retry)...)
	}
	if report.backoff != nil {
		logger.Warn("apns wakes backing off", attrs(report.backoff)...)
	}
}

const (
	// sqlForgetTokens deletes each refused token only if it is still the
	// token that was sent (a re-registration in between keeps the new one).
	sqlForgetTokens = `SELECT app.forget_push_token(v.d, v.t) FROM unnest($1::uuid[], $2::bytea[]) AS v(d, t)`
	// sqlReleasePushes gives back the reservations of pushes never sent:
	// each row returns to the reservation it had, unless something reserved
	// it since (its sequence and time are no longer those its tick
	// reserved, seq at ra).
	sqlReleasePushes = `UPDATE device_sync_cursors d SET pushed_sequence = v.ps, pushed_at = v.pa
FROM unnest($1::uuid[], $2::uuid[], $3::bigint[], $4::timestamptz[], $5::bigint[], $6::timestamptz[]) AS v(f, dv, ps, pa, seq, ra)
WHERE d.family_id = v.f AND d.device_id = v.dv AND d.pushed_sequence = v.seq AND d.pushed_at = v.ra`
	// sqlOwePushes gives back the sequence of pushes Apple did not deliver
	// and keeps their time: the device is pushed again for that sequence
	// once its window ends (never sooner), unless something reserved it
	// since.
	sqlOwePushes = `UPDATE device_sync_cursors d SET pushed_sequence = v.ps
FROM unnest($1::uuid[], $2::uuid[], $3::bigint[], $4::bigint[], $5::timestamptz[]) AS v(f, dv, ps, seq, ra)
WHERE d.family_id = v.f AND d.device_id = v.dv AND d.pushed_sequence = v.seq AND d.pushed_at = v.ra`
)

// maxSettleRows bounds the device rows of the settles a worker keeps: at
// least four full ticks' worth (a tick reserves at most maxPushTargets
// rows), and far more in practice, since a settle names only the pushes
// that were refused, not delivered or never started.
const maxSettleRows = 4 * maxPushTargets

// pendingSettle is one tick's post-send work, kept until it applies: forget
// the tokens Apple refused, owe again the sequences of pushes Apple did not
// deliver, and release the reservations of pushes never sent (each a device
// row the tick reserved at reservedAt).
type pendingSettle struct {
	id                          uint64 // from keepSettle: 1, 2, ...
	reservedAt                  time.Time
	forget, undelivered, unsent []pushJob
}

func (s pendingSettle) rows() int { return len(s.forget) + len(s.undelivered) + len(s.unsent) }

// keepSettle keeps a tick's settle until it applies. Past maxSettleRows the
// oldest kept settles are dropped, logged at Warn: their reservations stand,
// so those devices miss that write's push (they are pushed for the family's
// next write). It returns the kept settle's id (0: an empty settle, not
// kept).
func (w *Worker) keepSettle(settle pendingSettle) uint64 {
	if settle.rows() == 0 {
		return 0
	}
	w.settleSeq++
	settle.id = w.settleSeq
	w.settles = append(w.settles, settle)
	w.settleRows += settle.rows()
	dropped := 0
	for w.settleRows > maxSettleRows && len(w.settles) > 1 {
		dropped += w.settles[0].rows()
		w.settleRows -= w.settles[0].rows()
		w.settles = w.settles[1:]
	}
	w.totals.settled(w.settleRows, 0)
	if dropped > 0 && w.Logger != nil {
		w.Logger.Warn("wake settles dropped: too many kept; their reservations stand", slog.Int("rows", dropped),
			slog.Int("kept_rows", w.settleRows), slog.Int("cap", maxSettleRows))
	}
	return settle.id
}

// settleColumns are the owe or release statement's arrays.
type settleColumns struct {
	families, devices   []string
	previous, sequences []int64
	previousAt          []*time.Time
	reservedAt          []time.Time
}

func (c *settleColumns) add(job pushJob, reservedAt time.Time) {
	c.families, c.devices = append(c.families, job.family), append(c.devices, job.device)
	c.previous, c.sequences = append(c.previous, int64(job.previous)), append(c.sequences, int64(job.sequence))
	c.previousAt, c.reservedAt = append(c.previousAt, job.previousAt), append(c.reservedAt, reservedAt)
}

// rescanSettled scans again, on the next tick, the families of a settle
// applied after its own tick ended: a released push is sent then (or held
// to the pause's end), and an owed push gets its deferral to the end of its
// window from the deferral scan.
func (w *Worker) rescanSettled(settle pendingSettle) {
	var families []familyChange
	for _, job := range settle.undelivered {
		families = append(families, familyChange{FamilyID: job.family})
	}
	for _, job := range settle.unsent {
		families = append(families, familyChange{FamilyID: job.family})
	}
	w.Tracker.requeue(families)
}

// applySettles runs every kept settle, oldest first, each in its own short
// implicit transaction, all inside one worker slot. It returns the settles
// that committed (no longer kept), in order, and the failures of the
// others: each of those stays kept (quarantined) for the next tick or the
// worker's stop and counts once in Totals.SettlesFailed, so one settle that
// cannot apply never holds the others back (C35). Once ctx has ended after
// a failure the rest are kept untried, and not counted. Without a worker
// slot no settle can apply: each kept one counts as failed. Every statement is guarded by its row's reservation
// (pushed_sequence and pushed_at as its tick reserved them), and forgetting
// a token deletes it only while it is the token sent, so applying a settle
// again (after a commit whose answer was lost) changes nothing. If ctx
// already ended (shutdown), it gets a short bound of its own.
func (w *Worker) applySettles(ctx context.Context) ([]pendingSettle, error) {
	if len(w.settles) == 0 {
		return nil, nil
	}
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
		defer cancel()
	}
	if w.Slots != nil {
		release, err := w.Slots.Acquire(ctx)
		if err != nil {
			w.totals.settled(w.settleRows, len(w.settles))
			return nil, fmt.Errorf("wake: settle pushes: %w", err)
		}
		defer release()
	}
	var applied, kept []pendingSettle
	var failures []error
	keptRows := 0
	for _, settle := range w.settles {
		if len(failures) > 0 && ctx.Err() != nil {
			kept, keptRows = append(kept, settle), keptRows+settle.rows()
			continue
		}
		if err := w.applySettle(ctx, settle); err != nil {
			kept, keptRows = append(kept, settle), keptRows+settle.rows()
			failures = append(failures, err)
			continue
		}
		applied = append(applied, settle)
	}
	w.settles, w.settleRows = kept, keptRows
	w.totals.settled(keptRows, len(failures))
	return applied, errors.Join(failures...)
}

// applySettle runs one settle's statements in one implicit transaction.
func (w *Worker) applySettle(ctx context.Context, settle pendingSettle) error {
	var forgetDevices []string
	var forgetTokens [][]byte
	var owe, release settleColumns
	for _, job := range settle.forget {
		forgetDevices, forgetTokens = append(forgetDevices, job.device), append(forgetTokens, job.target.token)
	}
	for _, job := range settle.undelivered {
		owe.add(job, settle.reservedAt)
	}
	for _, job := range settle.unsent {
		release.add(job, settle.reservedAt)
	}
	batch := &pgx.Batch{}
	batch.Queue(sqlTimeouts)
	if len(forgetDevices) > 0 {
		batch.Queue(sqlForgetTokens, forgetDevices, forgetTokens)
	}
	if len(owe.devices) > 0 {
		batch.Queue(sqlOwePushes, owe.families, owe.devices, owe.previous, owe.sequences, owe.reservedAt)
	}
	if len(release.devices) > 0 {
		batch.Queue(sqlReleasePushes, release.families, release.devices, release.previous, release.previousAt, release.sequences, release.reservedAt)
	}
	if err := w.Pool.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("wake: settle pushes: %w", err)
	}
	return nil
}

// markRow is one device's mark: woken_* (wake) and the push reservation
// pushed_* (push), both at the tick's time and the row's sequence.
type markRow struct {
	family, device string
	sequence       uint64
	wake, push     bool
}

// decision is how one tick splits its due rows.
type decision struct {
	hub   []lagRow  // connected and hub-due: re-announced through the hub
	push  []pushJob // pushed after the commit (reserved by the mark)
	marks []markRow
	held  []string // families with a push held back (over the budget or paused): scanned again
	// recheck are families with a device re-announced through the hub while
	// a push was also owed to it: scanned again once its hub window ends, so
	// a device whose connection was closing (it never pulled) is pushed then.
	recheck []string
}

// decide splits a tick's due rows (connected is read once per device):
//
//   - connected: the hub reaches it. If hub-due, re-announce and mark woken,
//     and if also push-due recheck the family (the connection may have been
//     closing); if only push-due, nothing (a push would duplicate the hub,
//     and the push window stays open for when it disconnects);
//   - not connected, push-due, and its target already pushed this tick for
//     another row (one phone on several device rows): mark pushed and woken
//     with that send, the phone's one push;
//   - not connected, push-due, a registered target, not paused and budget
//     left: push, and mark pushed (the reservation) and woken;
//   - not connected, push-due and held back (paused, or over the budget):
//     mark woken if hub-due, never pushed, and hold the family so it is
//     scanned again;
//   - otherwise (no target, or not push-due): mark woken if hub-due.
//
// A tick with no Pusher never has a push-due row (the scans do not compute
// it), so it is Task 8's hub split.
func decide(rows []lagRow, connected func(device string) bool, pushBudget int, paused bool) decision {
	var d decision
	held, recheck := map[string]bool{}, map[string]bool{}
	sends := map[string]int{} // a target's environment and token -> its job in d.push
	for _, row := range rows {
		mark := markRow{family: row.family, device: row.device, sequence: row.sequence, wake: row.hubDue}
		var job pushJob
		var target string
		send, sending := 0, false
		if row.target != nil {
			job = pushJob{family: row.family, device: row.device, sequence: row.sequence,
				previous: row.pushedSequence, previousAt: row.pushedAt, target: *row.target}
			target = string(row.target.env) + "/" + string(row.target.token)
			send, sending = sends[target]
		}
		switch {
		case connected(row.device):
			if row.hubDue {
				d.hub = append(d.hub, row)
				if row.pushDue {
					recheck[row.family] = true
				}
			}
		case row.pushDue && paused:
			held[row.family] = true
		case row.pushDue && sending:
			mark.wake, mark.push = true, true
			d.push[send].shared = append(d.push[send].shared, job)
		case row.pushDue && row.target != nil && pushBudget == 0:
			held[row.family] = true
		case row.pushDue && row.target != nil:
			pushBudget--
			mark.wake, mark.push = true, true
			sends[target] = len(d.push)
			d.push = append(d.push, job)
		}
		if mark.wake || mark.push {
			d.marks = append(d.marks, mark)
		}
	}
	d.held, d.recheck = sortedKeys(held), sortedKeys(recheck)
	return d
}

func sortedKeys(set map[string]bool) []string {
	var keys []string
	for key := range set {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// pushLookups are the rows whose target the tick looks up: not connected
// and push-due, while sends are not paused.
func pushLookups(rows []lagRow, connected func(device string) bool, paused bool) []int {
	if paused {
		return nil
	}
	var lookups []int
	for i, row := range rows {
		if row.pushDue && !connected(row.device) {
			lookups = append(lookups, i)
		}
	}
	return lookups
}
