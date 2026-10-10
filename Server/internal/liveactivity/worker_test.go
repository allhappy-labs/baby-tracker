package liveactivity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/care"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/database"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// main's worker tests (main:Server/internal/liveactivity/worker_test.go) on
// the branch:
//   - the harness is testkit's with the owner and caregiver signed in at
//     the database clock (harness), and its sync service publishes each
//     committed write's high water to the worker's Dirty, as the
//     post-commit change feed does in production (main's journal trigger);
//   - registrations go through Service on the authority request seam, with
//     the member's access token (registrar);
//   - the guard is the authority (h.Authority), the queue is on the
//     maintenance pool, and the worker runs in its worker slots;
//   - where main asserted a journal-dirtied row before any worker pass, or
//     raced a commit against a held lease, the test flushes Dirty itself
//     (flushDirtyNow), as the worker's next poll would;
//   - main's SQL edits of feed_entries are one more committed version of
//     the feed (retimeFeed).
// Assertions on pushes, decisions, priorities, lifecycles, adoption and
// outcomes are main's.

type transportFunc func(context.Context, PushRequest) (PushResult, error)

func (f transportFunc) Send(c context.Context, r PushRequest) (PushResult, error) { return f(c, r) }

// dirtyPublisher is the post-commit change feed, minus the live hub: each
// committed write's high water is recorded in the worker's Dirty.
type dirtyPublisher struct{ dirty *Dirty }

func (p dirtyPublisher) Publish(_ context.Context, familyID string, cursor protocol.Cursor) error {
	p.dirty.Changed(familyID, cursor.Sequence)
	return nil
}

// registrar is main's Service calls over a member's access token.
type registrar struct {
	h       *testkit.Harness
	service *Service
}

func (r registrar) request(t *testing.T, token string) authority.Request {
	t.Helper()
	digests, err := r.h.Auth.AccessDigests(token)
	if err != nil {
		t.Fatal(err)
	}
	return authority.Request{Digests: digests}
}

func (r registrar) device(t *testing.T, ctx context.Context, token string, body protocol.LiveActivityDeviceRegistration) (protocol.LiveActivityRegistrationReceipt, error) {
	t.Helper()
	return r.service.RegisterDevice(ctx, r.request(t, token), body)
}

func (r registrar) activity(t *testing.T, ctx context.Context, token string, body protocol.LiveActivityTokenRegistration) (protocol.LiveActivityRegistrationReceipt, error) {
	t.Helper()
	return r.service.RegisterActivity(ctx, r.request(t, token), body)
}

// workerHarness is the worker tests' harness: the family's committed writes
// reach dirty, and reg registers as main's Service did.
func workerHarness(t *testing.T) (*testkit.Harness, *Dirty, registrar) {
	t.Helper()
	h := harness(t)
	dirty := NewDirty()
	h.SetPublisher(t, dirtyPublisher{dirty})
	return h, dirty, registrar{h: h, service: NewService(h.Authority, topic)}
}

// workerBaby is main's seedBaby: the fixture baby the timer fixtures name.
func workerBaby(t *testing.T, h *testkit.Harness) string {
	t.Helper()
	m := testkit.MustMutation(t, "baby.json")
	if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m); err != nil || out.Status != "applied" {
		t.Fatalf("baby=%+v err=%v", out, err)
	}
	return m.EntityID
}

// newTestWorker is NewWorker on the maintenance pool, in its worker slots.
func newTestWorker(t *testing.T, h *testkit.Harness, dirty *Dirty, guard SnapshotGuard, transport Transport) *Worker {
	t.Helper()
	slots, err := database.NewWorkerSlots(h.Maintenance)
	if err != nil {
		t.Fatal(err)
	}
	w := NewWorker(NewQueue(h.Maintenance), guard, transport, dirty, slots)
	w.Now = databaseClock(h)
	return w
}

// databaseClock is the host clock, moved forward onto the database's when
// the database's runs ahead. The worker claims due jobs at its own clock
// (Queue.Claim(now)), but a job's not_before is stamped by the database's
// now() when the family is dirtied; a host clock behind the database's
// (a container VM clock that drifted ahead) reads a job dirtied a moment
// ago as not yet due, and a pass claims nothing. It is never moved back:
// tests that sleep to the next wall second (nextWorkerSecond) need the
// worker's second to have advanced too.
func databaseClock(h *testkit.Harness) func() time.Time {
	ahead := max(h.DatabaseNow().Sub(time.Now()), 0)
	return func() time.Time { return time.Now().Add(ahead) }
}

// flushDirtyNow is the worker's flush of dirty, now: where main's journal
// trigger had dirtied the family inside the write.
func flushDirtyNow(t *testing.T, h *testkit.Harness, dirty *Dirty) {
	t.Helper()
	w := &Worker{Queue: NewQueue(h.Maintenance), Dirty: dirty}
	if err := w.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// forceSweep makes the worker's next pass run its minute maintenance.
func forceSweep(w *Worker) {
	w.maintenance.Lock()
	w.nextSweep = time.Time{}
	w.maintenance.Unlock()
}

// tenantCount is a count over a forced-RLS Live Activity table under
// account's tenant context in the harness family.
func tenantCount(t *testing.T, h *testkit.Harness, accountID, query string, args ...any) int64 {
	t.Helper()
	tx, err := h.Migrator.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountID, FamilyID: h.FamilyID}); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := tx.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// tenantExec is a statement on a forced-RLS Live Activity table under
// account's tenant context in the harness family, committed.
func tenantExec(t *testing.T, h *testkit.Harness, accountID, statement string, args ...any) {
	t.Helper()
	tx, err := h.Migrator.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountID, FamilyID: h.FamilyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), statement, args...); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func workerTokenCount(t *testing.T, h *testkit.Harness, query string) int64 {
	t.Helper()
	return tenantCount(t, h, h.Caregiver.AccountID, query)
}

// retimeFeed is one more committed version of feed id with its start and
// end moved (main edited feed_entries in SQL), recorded as the change feed
// records a write.
func retimeFeed(t *testing.T, h *testkit.Harness, dirty *Dirty, id string, started, ended time.Time) {
	t.Helper()
	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	row, err := care.Get(t.Context(), tx, h.FamilyID, id)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(t.Context())
	entity, err := row.Entity()
	if err != nil {
		t.Fatal(err)
	}
	feed := entity.Payload.(protocol.FeedPayload)
	end := ended.UTC().Format(metaLayout)
	feed.StartedAt, feed.EndedAt = started.UTC().Format(metaLayout), &end
	s := newSeeder(t, h)
	s.write(t, s.update(t, row, feed))
	dirty.Changed(h.FamilyID, h.FamilySequence(t))
}

func TestLiveActivityNoNetworkUnderFence(t *testing.T) {
	h, dirty, reg := workerHarness(t)
	baby := workerBaby(t, h)
	if _, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), deviceRequest()); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	var mu sync.Mutex
	sent := []PushRequest{}
	transport := transportFunc(func(ctx context.Context, r PushRequest) (PushResult, error) {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 5*time.Second {
			t.Error("missing five-second bound")
		}
		// Real authority mutation would block if the worker retained its snapshot fence.
		_, err := h.Family.ChangeRole(ctx, h.Owner, h.CommandID(), h.Caregiver.AccountID, "viewer")
		if err != nil {
			return PushResult{}, err
		}
		mu.Lock()
		sent = append(sent, r)
		mu.Unlock()
		return PushResult{Classification: Accepted}, nil
	})
	w := newTestWorker(t, h, dirty, h.Authority, transport)
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0].Event != "start" || sent[0].Content.Remote == nil {
		t.Fatalf("sends=%+v", sent)
	}
	// Reconstruct worker after the accepted start. Missing registration is not absence.
	w = newTestWorker(t, h, dirty, h.Authority, transport)
	h.RenameBaby(t, baby, "new name")
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 {
		t.Fatal("restart blindly repeated unconfirmed start")
	}
}

func TestLiveActivityForegroundAbsenceRetry(t *testing.T) {
	h, dirty, reg := workerHarness(t)
	baby := workerBaby(t, h)
	req := deviceRequest()
	if _, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), req); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	calls := 0
	w := newTestWorker(t, h, dirty, h.Authority, transportFunc(func(context.Context, PushRequest) (PushResult, error) {
		calls++
		return PushResult{Classification: Ambiguous}, errors.New("response lost")
	}))
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	receipt, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), req)
	if err != nil || len(receipt.UnconfirmedStarts) != 1 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	start := receipt.UnconfirmedStarts[0]
	req.ForegroundInventory = &protocol.LiveActivityForegroundInventory{InventoryVersion: 1, AbsentGenerations: []protocol.LiveActivityAbsentGeneration{{GenerationID: start.GenerationID, ObservedAttemptID: start.AttemptID}}}
	if _, err = reg.device(t, t.Context(), h.AccessToken("caregiver"), req); err != nil {
		t.Fatal(err)
	}
	// Real seconds, not a fabricated APNs timestamp. Worker projection re-runs.
	nextWorkerSecond()
	if err = w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("absence-authorized sends=%d", calls)
	}
	h.RenameBaby(t, baby, "after absence retry")
	flushDirtyNow(t, h, dirty)
	if scalar(t, h, `SELECT count(*) FROM live_activity_delivery WHERE kind='feed' AND dirty_version>completed_version`) != 1 {
		t.Fatal("absence proof swallowed next committed care mutation")
	}
	if err = w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}

	if _, err = reg.device(t, t.Context(), h.AccessToken("caregiver"), req); err != nil {
		t.Fatalf("identical inventory retry=%v", err)
	}
	req.ForegroundInventory.InventoryVersion = 2
	if _, err = reg.device(t, t.Context(), h.AccessToken("caregiver"), req); err != nil {
		t.Fatal(err)
	}
	if err = w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("stale attempt authorized another start")
	}
	req.ForegroundInventory.AbsentGenerations = []protocol.LiveActivityAbsentGeneration{}
	if _, err = reg.device(t, t.Context(), h.AccessToken("caregiver"), req); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("changed equal inventory accepted: %v", err)
	}
}

func TestLiveActivityDurableIntentSurvivesExpiredClaim(t *testing.T) {
	h, _, reg := workerHarness(t)
	workerBaby(t, h)
	if _, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), deviceRequest()); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	q := NewQueue(h.Maintenance)
	claims, err := q.Claim(t.Context(), h.DatabaseNow(), 2)
	if err != nil {
		t.Fatal(err)
	}
	var claim Claim
	for _, c := range claims {
		if c.Kind == "feed" {
			claim = c
		}
	}
	state := TargetState{GenerationID: installation, Status: "startUnconfirmed", AttemptID: otherInstallation}
	if err = q.SaveTarget(t.Context(), claim, state, false, time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = h.Migrator.Exec(t.Context(), `UPDATE live_activity_delivery SET lease_deadline=clock_timestamp()-interval '1 second' WHERE job_id=$1`, claim.JobID); err != nil {
		t.Fatal(err)
	}
	if err = q.SaveTarget(t.Context(), claim, TargetState{}, true, time.Time{}, nil); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("expired ack=%v", err)
	}
	fresh, err := q.Claim(t.Context(), h.DatabaseNow(), 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range fresh {
		if c.JobID == claim.JobID {
			got, e := q.LoadTarget(t.Context(), c)
			if e != nil || got.Status != "startUnconfirmed" {
				t.Fatalf("lost intent %+v %v", got, e)
			}
			return
		}
	}
	t.Fatal("expired job not reclaimed")
}

func activityRequest(h *testkit.Harness, baby, id string) protocol.LiveActivityTokenRegistration {
	timer := testkit.TimerA
	return protocol.LiveActivityTokenRegistration{RequestMeta: deviceRequest().RequestMeta, InstallationID: installation, ActivityID: id, Token: "ccdd", TokenVersion: 1, Scope: protocol.LiveActivityScope{ServerInstanceID: h.Info.InstanceID, RecoveryEpoch: h.Info.RecoveryEpoch, FamilyID: h.FamilyID, BabyID: baby, KindRaw: "feed"}, TimerID: &timer, State: "active"}
}

func TestLiveActivityTokenRotationDuringSendAndDismissal(t *testing.T) {
	h, dirty, reg := workerHarness(t)
	baby := workerBaby(t, h)
	if _, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), deviceRequest()); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	req := activityRequest(h, baby, "local-card")
	first, err := reg.activity(t, t.Context(), h.AccessToken("caregiver"), req)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	w := newTestWorker(t, h, dirty, h.Authority, transportFunc(func(ctx context.Context, r PushRequest) (PushResult, error) {
		calls++
		if r.Content.Remote.GenerationID != first.Generations[0].GenerationID {
			t.Error("local generation changed")
		}
		if calls == 1 {
			req.TokenVersion = 2
			req.Token = "eeff"
			if _, err := reg.activity(t, ctx, h.AccessToken("caregiver"), req); err != nil {
				t.Errorf("registration during send: %v", err)
				return PushResult{}, err
			}
			return PushResult{Classification: InvalidToken}, nil
		}
		if r.Token != "eeff" {
			t.Error("retry did not refresh token")
		}
		return PushResult{Classification: Accepted}, nil
	}))
	if err = w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if workerTokenCount(t, h, `SELECT count(*) FROM live_activity_tokens WHERE token='eeff' AND token_version=2 AND state='active'`) != 1 {
		t.Fatalf("stale response invalidated rotated token; calls=%d", calls)
	}
	nextWorkerSecond()
	if err = w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	req.TokenVersion = 3
	req.State = "dismissed"
	if _, err = reg.activity(t, t.Context(), h.AccessToken("caregiver"), req); err != nil {
		t.Fatal(err)
	}
	before := calls
	if err = w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	h.RenameBaby(t, baby, "new name")
	if err = w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if calls != before {
		t.Fatal("dismissed generation sent again")
	}
}

func TestLiveActivityLateTokenCanonicalReceiptAndPermissionSweep(t *testing.T) {
	h, dirty, reg := workerHarness(t)
	baby := workerBaby(t, h)
	if _, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), deviceRequest()); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	var sent []PushRequest
	w := newTestWorker(t, h, dirty, h.Authority, transportFunc(func(_ context.Context, r PushRequest) (PushResult, error) {
		sent = append(sent, r)
		return PushResult{Classification: Accepted}, nil
	}))
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	req := activityRequest(h, baby, "late-card")
	req.KnownGenerationID = &sent[0].Content.Remote.GenerationID
	receipt, err := reg.activity(t, t.Context(), h.AccessToken("caregiver"), req)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Generations[0].GenerationID != *req.KnownGenerationID {
		t.Fatal("late registration got a different canonical generation")
	}
	nextWorkerSecond()
	if err = w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[1].Event != "update" {
		t.Fatalf("late attachment sends=%+v", sent)
	}
	if _, err = h.Family.ChangeRole(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID, "viewer"); err != nil {
		t.Fatal(err)
	}
	// Simulate the required 60-second authority poll, with no care mutation:
	// the minute maintenance's work sweep.
	if _, err = h.Migrator.Exec(t.Context(), `UPDATE live_activity_family_dirty SET authority_checked_at=clock_timestamp()-interval '61 seconds'`); err != nil {
		t.Fatal(err)
	}
	forceSweep(w)
	nextWorkerSecond()
	if err = w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 3 || sent[2].Content.Remote.CanWrite {
		t.Fatalf("permission sweep sends=%+v", sent)
	}
}

func TestLiveActivityConservativeDuplicateEnd(t *testing.T) {
	h, dirty, reg := workerHarness(t)
	baby := workerBaby(t, h)
	if _, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), deviceRequest()); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	var sent []PushRequest
	w := newTestWorker(t, h, dirty, h.Authority, transportFunc(func(_ context.Context, r PushRequest) (PushResult, error) {
		sent = append(sent, r)
		return PushResult{Classification: Accepted}, nil
	}))
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	generation := sent[0].Content.Remote.GenerationID
	for i, id := range []string{"a-card", "b-card"} {
		req := activityRequest(h, baby, id)
		if i == 1 {
			req.Token = "eeff"
		}
		req.KnownGenerationID = &generation
		if _, err := reg.activity(t, t.Context(), h.AccessToken("caregiver"), req); err != nil {
			t.Fatal(err)
		}
	}
	nextWorkerSecond()
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[1].Event != "end" || sent[1].Token != "eeff" {
		t.Fatalf("duplicate sends=%+v", sent)
	}
	if workerTokenCount(t, h, `SELECT count(*) FROM live_activity_tokens WHERE activity_id='a-card' AND state='active'`) != 1 || workerTokenCount(t, h, `SELECT count(*) FROM live_activity_tokens WHERE activity_id='b-card' AND state='ended'`) != 1 {
		t.Fatal("duplicate cleanup retired wrong card")
	}
	nextWorkerSecond()
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 3 || sent[2].Event != "update" || sent[2].Token != "ccdd" {
		t.Fatal("duplicate end suppressed surviving card")
	}
}

func TestLiveActivityCrashAfterSendBeforeAck(t *testing.T) {
	h, dirty, reg := workerHarness(t)
	workerBaby(t, h)
	if _, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), deviceRequest()); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	w := newTestWorker(t, h, dirty, h.Authority, transportFunc(func(context.Context, PushRequest) (PushResult, error) {
		calls++
		cancel()
		return PushResult{Classification: Accepted}, nil
	}))
	if err := w.RunOnce(ctx, 32); err == nil {
		t.Fatal("simulated dead process acknowledged")
	}
	if _, err := h.Migrator.Exec(t.Context(), `UPDATE live_activity_delivery SET lease_deadline=clock_timestamp()-interval '1 second' WHERE lease_id IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	w = newTestWorker(t, h, dirty, h.Authority, transportFunc(func(context.Context, PushRequest) (PushResult, error) {
		calls++
		return PushResult{Classification: Accepted}, nil
	}))
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("crash recovery resent start")
	}
}

// registerCaregiverDevices registers count more caregiver devices and
// completes their sleep jobs (main's helper).
func registerCaregiverDevices(t *testing.T, h *testkit.Harness, reg registrar, count int) {
	t.Helper()
	for i := 1; i <= count; i++ {
		_, token, err := h.NewDeviceSession("caregiver")
		if err != nil {
			t.Fatal(err)
		}
		r := deviceRequest()
		r.InstallationID = fmt.Sprintf("70000000-0000-4000-8000-%012d", i)
		r.PushToStartToken = fmt.Sprintf("%04x", i)
		if _, err = reg.device(t, t.Context(), token, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.Migrator.Exec(t.Context(), `UPDATE live_activity_delivery SET completed_version=dirty_version WHERE kind='sleep'`); err != nil {
		t.Fatal(err)
	}
}

func TestLiveActivityProcessWideSendBound(t *testing.T) {
	h, dirty, reg := workerHarness(t)
	workerBaby(t, h)
	h.StartFeed(t, h.Owner, testkit.TimerA)
	flushDirtyNow(t, h, dirty) // main's trigger: no registration yet
	registerCaregiverDevices(t, h, reg, 3)
	entered := make(chan PushRequest, 3)
	release := make(chan struct{})
	done := make(chan error, 3)
	transport := transportFunc(func(ctx context.Context, r PushRequest) (PushResult, error) {
		entered <- r
		select {
		case <-release:
			return PushResult{Classification: Accepted}, nil
		case <-ctx.Done():
			return PushResult{Classification: Ambiguous}, ctx.Err()
		}
	})
	for i := 0; i < 3; i++ {
		w := newTestWorker(t, h, dirty, h.Authority, transport)
		go func() { done <- w.RunOnce(t.Context(), 32) }()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatal("two sends did not enter")
		}
	}
	select {
	case <-entered:
		close(release)
		t.Fatal("more than two sends in flight")
	case <-time.After(100 * time.Millisecond):
	}
	if n := scalar(t, h, `SELECT count(*) FROM live_activity_delivery WHERE lease_id IS NOT NULL`); n != 2 {
		t.Errorf("claimed tail while send capacity busy: %d", n)
	}
	close(release)
	for i := 0; i < 3; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestLiveActivityLateOldGenerationRetired(t *testing.T) {
	h, dirty, reg := workerHarness(t)
	baby := workerBaby(t, h)
	if _, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), deviceRequest()); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	var sent []PushRequest
	w := newTestWorker(t, h, dirty, h.Authority, transportFunc(func(_ context.Context, r PushRequest) (PushResult, error) {
		sent = append(sent, r)
		return PushResult{Classification: Accepted}, nil
	}))
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	old := sent[0].Content.Remote.GenerationID
	workerStopFeed(t, h, baby, testkit.TimerA)
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	nextTimer := "99999999-9999-4999-8999-999999999999"
	if out := h.StartFeed(t, h.Owner, nextTimer); out.Status != "applied" {
		t.Fatalf("replacement: %+v", out)
	}
	nextWorkerSecond()
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	currentStartAttempt := sent[len(sent)-1].APNSID
	current := sent[len(sent)-1].Content.Remote.GenerationID
	if current == old {
		t.Fatal("closed generation reused")
	}
	req := activityRequest(h, baby, "delayed-old")
	req.KnownGenerationID = &old
	receipt, err := reg.activity(t, t.Context(), h.AccessToken("caregiver"), req)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Generations[0].GenerationID == current {
		t.Fatal("old token relabeled new")
	}
	nextWorkerSecond()
	if err = w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	last := sent[len(sent)-1]
	if last.Event != "end" || last.Content.Remote.GenerationID != old {
		t.Fatalf("late old card not ended: %+v", last)
	}
	deviceReceipt, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), deviceRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(deviceReceipt.UnconfirmedStarts) != 1 || deviceReceipt.UnconfirmedStarts[0].AttemptID != currentStartAttempt {
		t.Fatal("old-card cleanup replaced the current start proof nonce")
	}
}

const stoppedFeedRecord = "88888888-8888-4888-8888-888888888888"

func workerStopFeed(t *testing.T, h *testkit.Harness, baby, timer string) {
	t.Helper()
	m := testkit.MustMutation(t, "timer-feed.json")
	m.MutationID = h.CommandID()
	m.EntityID = timer
	m.Operation = "timer_stop"
	m.Payload = protocol.TimerStopPayload{BabyID: baby, CompletedRecordID: stoppedFeedRecord, StoppedAt: "2026-09-20T15:10:00Z", ComponentID: "77777777-7777-4777-8777-777777777777"}
	out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil || out.Status != "applied" {
		t.Fatalf("stop=%+v err=%v", out, err)
	}
}

func TestLiveActivityStartTokenFailureDoesNotRevokeUpdateTokens(t *testing.T) {
	h, dirty, reg := workerHarness(t)
	baby := workerBaby(t, h)
	if _, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), deviceRequest()); err != nil {
		t.Fatal(err)
	}
	req := activityRequest(h, baby, "independent-sleep")
	req.Scope.KindRaw = "sleep"
	req.TimerID = nil
	if _, err := reg.activity(t, t.Context(), h.AccessToken("caregiver"), req); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	w := newTestWorker(t, h, dirty, h.Authority, transportFunc(func(context.Context, PushRequest) (PushResult, error) {
		return PushResult{Classification: InvalidToken}, nil
	}))
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if workerTokenCount(t, h, `SELECT count(*) FROM live_activity_devices WHERE push_token IS NULL AND NOT revoked`) != 1 {
		t.Fatal("failed start did not retire only start token")
	}
	if workerTokenCount(t, h, `SELECT count(*) FROM live_activity_tokens WHERE activity_id='independent-sleep' AND token='ccdd' AND state='active'`) != 1 {
		t.Fatal("failed start retired independent update token")
	}
}

func TestLiveActivityBreakExpiryWithoutMutation(t *testing.T) {
	h, dirty, reg := workerHarness(t)
	baby := workerBaby(t, h)
	if _, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), deviceRequest()); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	req := activityRequest(h, baby, "break-card")
	if _, err := reg.activity(t, t.Context(), h.AccessToken("caregiver"), req); err != nil {
		t.Fatal(err)
	}
	var sent []PushRequest
	w := newTestWorker(t, h, dirty, h.Authority, transportFunc(func(_ context.Context, r PushRequest) (PushResult, error) {
		sent = append(sent, r)
		return PushResult{Classification: Accepted}, nil
	}))
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	workerStopFeed(t, h, baby, testkit.TimerA)
	// Arrange a one-second remaining break; the history reader must also run
	// through the actual read-only guard, then schedule its independent deadline.
	now := h.DatabaseNow()
	retimeFeed(t, h, dirty, stoppedFeedRecord, now.Add(-40*time.Minute), now.Add(-30*time.Minute+2*time.Second))
	nextWorkerSecond()
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[1].Event != "update" || sent[1].Content.Phase != "feedBreak" {
		t.Fatalf("break presentation=%+v", sent)
	}
	time.Sleep(2200 * time.Millisecond)
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 3 || sent[2].Event != "end" {
		t.Fatalf("expiry without mutation=%+v", sent)
	}
}

func TestLiveActivityFailedUpdateDoesNotRestart(t *testing.T) {
	h, dirty, reg := workerHarness(t)
	baby := workerBaby(t, h)
	if _, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), deviceRequest()); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	req := activityRequest(h, baby, "failed-card")
	if _, err := reg.activity(t, t.Context(), h.AccessToken("caregiver"), req); err != nil {
		t.Fatal(err)
	}
	calls := 0
	w := newTestWorker(t, h, dirty, h.Authority, transportFunc(func(context.Context, PushRequest) (PushResult, error) {
		calls++
		return PushResult{Classification: InvalidToken}, nil
	}))
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	nextWorkerSecond()
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("failed update restarted same generation")
	}
	if workerTokenCount(t, h, `SELECT count(*) FROM live_activity_devices WHERE push_token='aabb' AND NOT revoked`) != 1 {
		t.Fatal("failed update invalidated start token")
	}
}

func TestLiveActivityRetentionRemovesDeadTokensAndHistory(t *testing.T) {
	h, _, reg := workerHarness(t)
	baby := workerBaby(t, h)
	if _, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), deviceRequest()); err != nil {
		t.Fatal(err)
	}
	req := activityRequest(h, baby, "dismissed")
	req.State = "dismissed"
	if _, err := reg.activity(t, t.Context(), h.AccessToken("caregiver"), req); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Migrator.Exec(t.Context(), `INSERT INTO live_activity_history(job_id,event,recorded_at) VALUES($1,'start',clock_timestamp()-interval '8 days'),($1,'update',clock_timestamp()-interval '6 days')`, installation); err != nil {
		t.Fatal(err)
	}
	if _, err := NewQueue(h.Maintenance).Retain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if workerTokenCount(t, h, `SELECT count(*) FROM live_activity_tokens WHERE activity_id='dismissed' AND state='dismissed' AND token_version=1 AND token IS NULL`) != 1 {
		t.Fatal("dead token retained or dismissal fence lost")
	}
	if scalar(t, h, `SELECT count(*) FROM live_activity_history`) != 1 {
		t.Fatal("history retention boundary wrong")
	}
	if _, err := reg.activity(t, t.Context(), h.AccessToken("caregiver"), req); err != nil {
		t.Fatalf("identical terminal retry after cleanup=%v", err)
	}
}

// main's journal trigger took the family dirty lock inside the care write;
// on the branch the change feed's flush (app.dirty_live_activity_families)
// takes it. An acknowledgement that waits on it must not deadlock, and a
// stale one must not retire a token after the newer work.
func TestLiveActivityAckAndJournalLockOrder(t *testing.T) {
	h, _, reg := workerHarness(t)
	baby := workerBaby(t, h)
	if _, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), deviceRequest()); err != nil {
		t.Fatal(err)
	}
	req := activityRequest(h, baby, "lock-card")
	if _, err := reg.activity(t, t.Context(), h.AccessToken("caregiver"), req); err != nil {
		t.Fatal(err)
	}
	claims, err := NewQueue(h.Maintenance).Claim(t.Context(), time.Now(), 2)
	if err != nil {
		t.Fatal(err)
	}
	var claim Claim
	for _, c := range claims {
		if c.Kind == "feed" {
			claim = c
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	flush, err := h.Migrator.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer flush.Rollback(context.Background())
	var flushPID int
	if err = flush.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&flushPID); err != nil {
		t.Fatal(err)
	}
	if _, err = flush.Exec(ctx, `SELECT 1 FROM live_activity_family_dirty WHERE family_id=$1 FOR UPDATE`, h.FamilyID); err != nil {
		t.Fatal(err)
	}
	ack, err := h.Maintenance.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ack.Rollback(context.Background())
	var ackPID int
	if err = ack.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&ackPID); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	raw, _ := json.Marshal(claim)
	go func() {
		var result []byte
		e := ack.QueryRow(ctx, `SELECT app.live_activity_target($1,'{}',true,NULL,'{"activity_id":"lock-card","version":1}',NULL)`, raw).Scan(&result)
		if e == nil {
			e = ack.Commit(ctx)
		}
		finished <- e
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var blocked bool
		if err = h.Migrator.QueryRow(ctx, `SELECT $1::integer=ANY(pg_blocking_pids($2::integer))`, flushPID, ackPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ack never waited on family dirty lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err = flush.Exec(ctx, `SELECT app.dirty_live_activity_families($1::uuid[],$2::bigint[])`, []string{h.FamilyID}, []int64{100}); err != nil {
		t.Fatal(err)
	}
	if err = flush.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-finished; err != nil {
		t.Fatalf("ack/flush deadlock: %v", err)
	}
	if workerTokenCount(t, h, `SELECT count(*) FROM live_activity_tokens WHERE activity_id='lock-card' AND state='active'`) != 1 {
		t.Fatal("stale ack retired token after newer work")
	}
}

func TestLiveActivityRolledBackCareDoesNotSend(t *testing.T) {
	h, dirty, reg := workerHarness(t)
	baby := workerBaby(t, h)
	if _, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), deviceRequest()); err != nil {
		t.Fatal(err)
	}
	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	babyRow, err := care.Get(t.Context(), tx, h.FamilyID, baby)
	if err != nil {
		t.Fatal(err)
	}
	timer := caretest.Owner(h).Row(t, "timer", testkit.TimerA, &babyRow, h.FamilySequence(t)+1)
	if err := caretest.Owner(h).Queue(t, tx, nil, timer); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	w := newTestWorker(t, h, dirty, h.Authority, transportFunc(func(context.Context, PushRequest) (PushResult, error) {
		t.Error("rolled-back care sent a card")
		return PushResult{Classification: Accepted}, nil
	}))
	if err = w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
}

func TestLiveActivityRunPollsWithTwoSendSlots(t *testing.T) {
	h, dirty, reg := workerHarness(t)
	workerBaby(t, h)
	h.StartFeed(t, h.Owner, testkit.TimerA)
	flushDirtyNow(t, h, dirty)
	registerCaregiverDevices(t, h, reg, 2)
	entered := make(chan struct{}, 2)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w := newTestWorker(t, h, dirty, h.Authority, transportFunc(func(ctx context.Context, _ PushRequest) (PushResult, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return PushResult{Classification: Ambiguous}, ctx.Err()
	}))
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, nil) }()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			cancel()
			<-done
			t.Fatal("poll blocked behind first send")
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown=%v", err)
	}
}

// Decorates the real guard at its public boundary; hooks run outside its fence.
type interleavedSnapshotGuard struct {
	SnapshotGuard
	beforeSecond func()
	feedReads    int
}

func (g *interleavedSnapshotGuard) WithLiveActivitySnapshot(ctx context.Context, s authority.LiveActivitySubject, run func(pgx.Tx, access.Grant) error) error {
	if s.Kind == "feed" {
		g.feedReads++
		if g.feedReads == 2 && g.beforeSecond != nil {
			g.beforeSecond()
		}
	}
	return g.SnapshotGuard.WithLiveActivitySnapshot(ctx, s, run)
}

func TestLiveActivityKnownUnsentStartRecovers(t *testing.T) {
	for _, interleaving := range []string{"care_mutation", "viewer_demotion", "registration_rotation", "insufficient_lease", "revocation"} {
		t.Run(interleaving, func(t *testing.T) {
			h, dirty, reg := workerHarness(t)
			baby := workerBaby(t, h)
			if _, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), deviceRequest()); err != nil {
				t.Fatal(err)
			}
			h.StartFeed(t, h.Owner, testkit.TimerA)
			guard := &interleavedSnapshotGuard{SnapshotGuard: h.Authority}
			var sent []PushRequest
			w := newTestWorker(t, h, dirty, guard, transportFunc(func(_ context.Context, r PushRequest) (PushResult, error) {
				sent = append(sent, r)
				return PushResult{Classification: Accepted}, nil
			}))
			q := w.Queue
			claims, err := q.Claim(t.Context(), h.DatabaseNow(), 2)
			if err != nil {
				t.Fatal(err)
			}
			var claim Claim
			for _, c := range claims {
				if c.Kind == "feed" {
					claim = c
				} else if err = q.Complete(t.Context(), c); err != nil {
					t.Fatal(err)
				}
			}
			guard.beforeSecond = func() {
				switch interleaving {
				case "care_mutation":
					h.RenameBaby(t, baby, "after intent")
					flushDirtyNow(t, h, dirty)
				case "viewer_demotion":
					if _, e := h.Family.ChangeRole(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID, "viewer"); e != nil {
						t.Fatal(e)
					}
				case "registration_rotation":
					r := deviceRequest()
					r.RegistrationVersion = 2
					r.PushToStartToken = "eeff"
					if _, e := reg.device(t, t.Context(), h.AccessToken("caregiver"), r); e != nil {
						t.Fatal(e)
					}
				case "insufficient_lease":
					w.Now = func() time.Time { return claim.LeaseDeadline.Add(-4 * time.Second) }
				case "revocation":
					if _, e := h.Auth.Logout(t.Context(), h.Caregiver, h.CommandID()); e != nil {
						t.Fatal(e)
					}
				}
			}
			err = w.dispatch(t.Context(), claim)
			if err != nil && !errors.Is(err, ErrStaleClaim) && !errors.Is(err, authority.ErrLiveActivityAuthorizationChanged) {
				t.Fatal(err)
			}
			if len(sent) != 0 {
				t.Fatal("pre-send interleaving reached transport")
			}
			var generation, status string
			if err = h.Migrator.QueryRow(t.Context(), `SELECT target_state->>'generation_id',target_state->>'status' FROM live_activity_delivery WHERE job_id=$1`, claim.JobID).Scan(&generation, &status); err != nil {
				t.Fatal(err)
			}
			if generation == "" || status != "pending" {
				t.Fatalf("known-unsent start remained %q (generation %s)", status, generation)
			}
			if _, err = h.Migrator.Exec(t.Context(), `UPDATE live_activity_delivery SET lease_deadline=clock_timestamp()-interval '1 second' WHERE lease_id IS NOT NULL`); err != nil {
				t.Fatal(err)
			}
			w.Now = databaseClock(h)
			w.Guard = h.Authority
			nextWorkerSecond()
			if err = w.RunOnce(t.Context(), 32); err != nil {
				t.Fatal(err)
			}
			if interleaving == "revocation" {
				if len(sent) != 0 {
					t.Fatal("abandonment reauthorized revoked device")
				}
				return
			}
			if len(sent) != 1 || sent[0].Event != "start" || sent[0].Content.Remote.GenerationID != generation {
				t.Fatalf("known-unsent recovery=%+v", sent)
			}
			if interleaving == "viewer_demotion" && sent[0].Content.Remote.CanWrite {
				t.Fatal("retry used stale write eligibility")
			}
			if interleaving == "care_mutation" && sent[0].Attributes.BabyName != "after intent" {
				t.Fatal("retry replayed stale presentation")
			}
			if interleaving == "registration_rotation" && sent[0].Token != "eeff" {
				t.Fatal("retry used old registration token")
			}
		})
	}
}

func TestLiveActivityProvisionalDismissalBeforeFirstWorker(t *testing.T) {
	for _, status := range []string{"dismissed", "ended"} {
		for _, activeFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/active_first_%t", status, activeFirst), func(t *testing.T) {
				h, dirty, reg := workerHarness(t)
				baby := workerBaby(t, h)
				if _, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), deviceRequest()); err != nil {
					t.Fatal(err)
				}
				h.StartFeed(t, h.Owner, testkit.TimerA)
				req := activityRequest(h, baby, "provisional-card")
				if activeFirst {
					if _, err := reg.activity(t, t.Context(), h.AccessToken("caregiver"), req); err != nil {
						t.Fatal(err)
					}
					req.TokenVersion++
				}
				req.State = status
				receipt, err := reg.activity(t, t.Context(), h.AccessToken("caregiver"), req)
				if err != nil {
					t.Fatal(err)
				}
				calls := 0
				w := newTestWorker(t, h, dirty, h.Authority, transportFunc(func(context.Context, PushRequest) (PushResult, error) {
					calls++
					return PushResult{Classification: Accepted}, nil
				}))
				if err = w.RunOnce(t.Context(), 32); err != nil {
					t.Fatal(err)
				}
				h.RenameBaby(t, baby, "after dismissal")
				if err = w.RunOnce(t.Context(), 32); err != nil {
					t.Fatal(err)
				}
				if calls != 0 {
					t.Fatal("provisional terminal card was replaced by server start")
				}
				var generation, dismissed string
				if err = h.Migrator.QueryRow(t.Context(), `SELECT target_state->>'generation_id',target_state->>'dismissed_generation' FROM live_activity_delivery WHERE kind='feed'`).Scan(&generation, &dismissed); err != nil {
					t.Fatal(err)
				}
				if generation != receipt.Generations[0].GenerationID || dismissed != generation {
					t.Fatal("provisional lifecycle dismissal fence lost")
				}
			})
		}
	}
}

func TestLiveActivityAbandonStartFences(t *testing.T) {
	for _, interleaving := range []string{"newer_dirty", "reclaimed_lease", "newer_attempt", "wrong_generation", "wrong_registration_version", "expired_original_lease", "absence_retry"} {
		t.Run(interleaving, func(t *testing.T) {
			h, dirty, reg := workerHarness(t)
			baby := workerBaby(t, h)
			if _, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), deviceRequest()); err != nil {
				t.Fatal(err)
			}
			h.StartFeed(t, h.Owner, testkit.TimerA)
			q := NewQueue(h.Maintenance)
			claims, err := q.Claim(t.Context(), h.DatabaseNow(), 2)
			if err != nil {
				t.Fatal(err)
			}
			var claim Claim
			for _, c := range claims {
				if c.Kind == "feed" {
					claim = c
				} else if err = q.Complete(t.Context(), c); err != nil {
					t.Fatal(err)
				}
			}
			desired := lifecycleCard("feed", testkit.TimerA, "")
			previous := TargetState{}
			if interleaving == "absence_retry" {
				previous = Reconcile(previous, desired, time.Now().Add(-time.Minute)).State
				previous.AbsenceRetryAllowed = true
			}
			intent := Reconcile(previous, desired, time.Now()).State
			if err = q.SaveTarget(t.Context(), claim, intent, false, time.Time{}, nil); err != nil {
				t.Fatal(err)
			}
			attempted, captured := intent, claim
			reject := false
			switch interleaving {
			case "newer_dirty":
				h.RenameBaby(t, baby, "newer work")
				flushDirtyNow(t, h, dirty)
			case "reclaimed_lease":
				reclaimed, e := q.Claim(t.Context(), claim.LeaseDeadline.Add(time.Second), 2)
				if e != nil || len(reclaimed) == 0 {
					t.Fatalf("reclaim=%v %v", reclaimed, e)
				}
				reject = true
			case "newer_attempt":
				intent.AttemptID = deterministicID("newer attempt")
				intent.StartAttemptID = intent.AttemptID
				if err = q.SaveTarget(t.Context(), claim, intent, false, time.Time{}, nil); err != nil {
					t.Fatal(err)
				}
				reject = true
			case "wrong_generation":
				attempted.GenerationID = deterministicID("wrong generation")
				reject = true
			case "wrong_registration_version":
				captured.RegistrationVersion++
				reject = true
			case "expired_original_lease":
				// Preserve the exact persisted lease while advancing its observed expiry.
				// Abandonment does not authorize I/O, so an unreclaimed expired lease is safe.
				claim.LeaseDeadline = time.Now().Add(-time.Second).Truncate(time.Microsecond)
				captured = claim
				if _, err = h.Migrator.Exec(t.Context(), `UPDATE live_activity_delivery SET lease_deadline=$1 WHERE job_id=$2`, claim.LeaseDeadline, claim.JobID); err != nil {
					t.Fatal(err)
				}
			}
			var dirtyVersion, completed uint64
			var before []byte
			if err = h.Migrator.QueryRow(t.Context(), `SELECT dirty_version,completed_version,target_state FROM live_activity_delivery WHERE job_id=$1`, claim.JobID).Scan(&dirtyVersion, &completed, &before); err != nil {
				t.Fatal(err)
			}
			err = q.AbandonStart(t.Context(), captured, attempted, previous)
			if reject && !errors.Is(err, ErrStaleClaim) {
				t.Fatalf("stale abandonment=%v", err)
			}
			if !reject && err != nil {
				t.Fatal(err)
			}
			var after []byte
			var afterDirty, afterCompleted uint64
			if err = h.Migrator.QueryRow(t.Context(), `SELECT dirty_version,completed_version,target_state FROM live_activity_delivery WHERE job_id=$1`, claim.JobID).Scan(&afterDirty, &afterCompleted, &after); err != nil {
				t.Fatal(err)
			}
			if afterDirty != dirtyVersion || afterCompleted != completed || dirtyVersion <= completed {
				t.Fatal("abandonment consumed pending/newer dirty work")
			}
			if reject {
				if string(before) != string(after) {
					t.Fatal("stale abandonment replaced a newer intent")
				}
				return
			}
			var state TargetState
			if err = json.Unmarshal(after, &state); err != nil {
				t.Fatal(err)
			}
			if state.Status != "pending" || state.GenerationID != intent.GenerationID || state.PendingEvent != "" || state.StartAttemptID != "" {
				t.Fatalf("abandoned=%+v", state)
			}
			if state.AbsenceRetryAllowed != previous.AbsenceRetryAllowed || state.AbsenceRetryUsed != previous.AbsenceRetryUsed {
				t.Fatal("known-unsent attempt consumed absence authorization")
			}
			next := Reconcile(state, desired, time.Unix(state.LastTimestamp+1, 0))
			if next.Event != "start" || next.State.AttemptID == intent.AttemptID {
				t.Fatal("new attempt reused abandoned nonce or failed to retry")
			}
		})
	}
}

// Every request shape the worker builds must survive the production transport's
// own local validation and encoding. A recording Transport sees requests before
// that gate, which is how updates and ends without an expiration went unsent.
func TestLiveActivityWorkerRequestsPassAPNsValidation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	claim := Claim{JobID: installation, LeaseID: installation, RegistrationID: installation, RegistrationVersion: 1, AccountID: installation, DeviceID: installation, FamilyID: "22222222-2222-4222-8222-222222222222", BabyID: "33333333-3333-4333-8333-333333333333", Kind: "feed", InstanceID: "11111111-1111-4111-8111-111111111111", RecoveryEpoch: 1, DirtyVersion: 1}
	feed := lifecycleCard("feed", testkit.TimerA, "")
	feed.BabyName = "Mira"
	feed.Content.ActivityKind = "breastLeft"
	edited := *feed
	edited.Content.StartedAt = 50
	breakCard := lifecycleCard("feed", "", stoppedFeedRecord)
	breakCard.BabyName = "Mira"
	breakCard.Phase = "feedBreak"
	breakCard.MayStart = false
	breakCard.Content = ContentState{StartedAt: 100, ActivityKind: "breastLeft", Phase: "feedBreak", Parts: []PartLabel{{Side: ptr("left"), Seconds: 600}}}
	breakEnds := now.Add(25 * time.Minute)
	breakCard.StaleAt, breakCard.ExpiresAt = &breakEnds, &breakEnds
	expired := *breakCard
	past := now.Add(-time.Second)
	expired.ExpiresAt = &past
	sleep := lifecycleCard("sleep", testkit.TimerB, "")
	sleep.BabyName = "Mira"
	nextFeed := lifecycleCard("feed", "99999999-9999-4999-8999-999999999999", "")
	nextFeed.BabyName = "Mira"
	nextFeed.Content.ActivityKind = "breastRight"
	active := func(desired *DesiredCard) TargetState {
		s := Reconcile(TargetState{}, desired, now.Add(-time.Minute)).State
		s.Status, s.PendingEvent, s.ActivityID, s.TokenVersion, s.Attempts = "active", "", "card", 1, 0
		return s
	}
	running := active(feed)
	// The snapshot links a stopped timer to its completed record's session.
	linked := running
	linked.SessionID = breakCard.SessionID
	brk := Reconcile(linked, breakCard, now.Add(-30*time.Second)).State
	brk.PendingEvent, brk.Attempts = "", 0
	duplicate := Reconcile(running, feed, now).State
	duplicate.PendingEvent, duplicate.Attempts, duplicate.LastTimestamp = "duplicateEnd", 1, now.Unix()
	duplicate.PresentationVersion++
	duplicate.AttemptID = deterministicID(duplicate.GenerationID + "/duplicate/b-card")
	cases := []struct {
		name, event, kind string
		previous          TargetState
		desired           *DesiredCard
		decided           *Decision
		wantExpiry        time.Time
	}{
		{name: "running feed push-to-start", event: "start", kind: "feed", desired: feed},
		{name: "running feed update", event: "update", kind: "feed", previous: running, desired: &edited, wantExpiry: now.Add(8 * time.Hour)},
		{name: "feed break update", event: "update", kind: "feed", previous: linked, desired: breakCard, wantExpiry: breakEnds},
		{name: "break expiry end", event: "end", kind: "feed", previous: brk, desired: &expired, wantExpiry: now.Add(8 * time.Hour)},
		{name: "sleep stop end", event: "end", kind: "sleep", previous: active(sleep), wantExpiry: now.Add(8 * time.Hour)},
		{name: "replaced lifecycle end", event: "end", kind: "feed", previous: running, desired: nextFeed, wantExpiry: now.Add(8 * time.Hour)},
		{name: "duplicate end", event: "end", kind: "feed", decided: &Decision{"end", duplicate, now.Add(time.Second)}, desired: feed, wantExpiry: now.Add(8 * time.Hour)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := claim
			c.Kind = tc.kind
			d := Reconcile(tc.previous, tc.desired, now)
			if tc.decided != nil {
				d = *tc.decided
			}
			if d.Event != tc.event {
				t.Fatalf("decision=%s want %s", d.Event, tc.event)
			}
			token := "ccdd"
			if d.Event == "start" {
				token = "aabb"
			}
			desired := tc.desired
			if desired != nil && desired.ExpiresAt != nil && !now.Before(*desired.ExpiresAt) {
				desired = nil
			}
			req := makeRequest(c, d, dispatchSnapshot{desired: desired, environment: "sandbox", token: token})
			if err := validatePushRequest(req); err != nil {
				t.Fatalf("worker-built %s rejected by APNs validation: expiration=%v", req.Event, req.Expiration)
			}
			if _, err := encodePushPayload(req); err != nil {
				t.Fatalf("encode: %v", err)
			}
			if !req.Expiration.Equal(tc.wantExpiry) {
				t.Fatalf("expiration=%v want %v", req.Expiration, tc.wantExpiry)
			}
		})
	}
}

// recordingAPNs is the production APNSTransport against a local HTTP/2 TLS
// endpoint, so the worker's requests cross the real validation, encoding and
// header path (the branch's one APNs client) before anything is recorded.
type recordingAPNs struct {
	APNSTransport
	mu       sync.Mutex
	results  []string
	arrivals []string
}

func newRecordingAPNs(t *testing.T) *recordingAPNs {
	t.Helper()
	r := &recordingAPNs{}
	server := testAPNSServer(t, func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var payload struct {
			APS map[string]any `json:"aps"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Error(err)
		}
		event, _ := payload.APS["event"].(string)
		r.mu.Lock()
		r.arrivals = append(r.arrivals, event+" expiration="+req.Header.Get("apns-expiration"))
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	r.APNSTransport = testAPNSTransport(t, server, nil)
	return r
}

func (r *recordingAPNs) Send(ctx context.Context, req PushRequest) (PushResult, error) {
	result, err := r.APNSTransport.Send(ctx, req)
	r.mu.Lock()
	r.results = append(r.results, fmt.Sprintf("%s %s/%s %s err=%v", req.Event, req.Attributes.KindRaw, req.Content.Phase, result.Classification, err))
	r.mu.Unlock()
	return result, err
}

func (r *recordingAPNs) snapshot() ([]string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.results...), append([]string(nil), r.arrivals...)
}

func TestLiveActivityWorkerDeliversEveryEventThroughAPNsTransport(t *testing.T) {
	h, dirty, reg := workerHarness(t)
	baby := workerBaby(t, h)
	if _, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), deviceRequest()); err != nil {
		t.Fatal(err)
	}
	apns := newRecordingAPNs(t)
	w := newTestWorker(t, h, dirty, h.Authority, apns)
	step := func(want int, label string) {
		t.Helper()
		nextWorkerSecond()
		if err := w.RunOnce(t.Context(), 32); err != nil {
			t.Fatal(err)
		}
		results, arrivals := apns.snapshot()
		if len(results) != want || len(arrivals) != want {
			t.Fatalf("%s: transport results=%v arrivals=%v", label, results, arrivals)
		}
		for _, result := range results {
			if !strings.Contains(result, " accepted err=<nil>") {
				t.Fatalf("%s: worker-built request not accepted by APNs transport: %v", label, results)
			}
		}
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	step(1, "running feed push-to-start")
	results, _ := apns.snapshot()
	if !strings.HasPrefix(results[0], "start feed/running") {
		t.Fatalf("start=%v", results)
	}
	var generation string
	if err := h.Migrator.QueryRow(t.Context(), `SELECT target_state->>'generation_id' FROM live_activity_delivery WHERE kind='feed'`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"a-card", "b-card"} {
		req := activityRequest(h, baby, id)
		if i == 1 {
			req.Token = "eeff"
		}
		req.KnownGenerationID = &generation
		if _, err := reg.activity(t, t.Context(), h.AccessToken("caregiver"), req); err != nil {
			t.Fatal(err)
		}
	}
	step(2, "duplicate end")
	step(3, "running feed confirmation update")
	workerStopFeed(t, h, baby, testkit.TimerA)
	now := h.DatabaseNow()
	retimeFeed(t, h, dirty, stoppedFeedRecord, now.Add(-40*time.Minute), now.Add(-30*time.Minute+3*time.Second))
	step(4, "feed break update")
	time.Sleep(3 * time.Second)
	step(5, "break expiry end")
	sleep := testkit.MustMutation(t, "timer-sleep.json")
	sleep.MutationID = h.CommandID()
	sleep.EntityID = testkit.TimerB
	if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, sleep); err != nil || out.Status != "applied" {
		t.Fatalf("sleep start=%+v err=%v", out, err)
	}
	step(6, "sleep push-to-start")
	if err := h.Migrator.QueryRow(t.Context(), `SELECT target_state->>'generation_id' FROM live_activity_delivery WHERE kind='sleep'`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	sleepCard := activityRequest(h, baby, "sleep-card")
	sleepCard.Scope.KindRaw = "sleep"
	timer := testkit.TimerB
	sleepCard.TimerID = &timer
	sleepCard.KnownGenerationID = &generation
	if _, err := reg.activity(t, t.Context(), h.AccessToken("caregiver"), sleepCard); err != nil {
		t.Fatal(err)
	}
	step(7, "sleep confirmation update")
	stop := testkit.MustMutation(t, "timer-sleep.json")
	stop.MutationID, stop.EntityID, stop.Operation = h.CommandID(), testkit.TimerB, "timer_stop"
	stop.Payload = protocol.TimerStopPayload{BabyID: baby, CompletedRecordID: "86666666-6666-4666-8666-666666666666", StoppedAt: "2026-09-20T17:00:00Z"}
	if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, stop); err != nil || out.Status != "applied" {
		t.Fatalf("sleep stop=%+v err=%v", out, err)
	}
	step(8, "sleep stop end")
	results, arrivals := apns.snapshot()
	want := []string{"start feed/running", "end feed/", "update feed/running", "update feed/feedBreak", "end feed/", "start sleep/running", "update sleep/running", "end sleep/"}
	for i, prefix := range want {
		if !strings.HasPrefix(results[i], prefix) {
			t.Fatalf("event %d=%q want %q; all=%v", i, results[i], prefix, results)
		}
		expiry := strings.TrimPrefix(arrivals[i][strings.Index(arrivals[i], "expiration="):], "expiration=")
		if (strings.HasPrefix(prefix, "start") && expiry != "0") || (!strings.HasPrefix(prefix, "start") && (expiry == "0" || expiry == "")) {
			t.Fatalf("APNs expiration for %q=%q", prefix, expiry)
		}
	}
}

// A phone switching sides from a first part (no session yet) sends the stop and
// the next part, in that record's session, in one sync. That is a continuation:
// the card keeps its generation and receives an update, never end + start.
func TestLiveActivityFirstPartSideSwitchContinuesGeneration(t *testing.T) {
	const record = stoppedFeedRecord
	const next = "57000000-0000-4000-8000-000000000001"
	for _, tc := range []struct {
		name    string
		session *string
	}{{"same_session_continuation", ptr(record)}, {"new_feed_without_session", nil}, {"new_feed_other_session", ptr("58000000-0000-4000-8000-000000000001")}} {
		t.Run(tc.name, func(t *testing.T) {
			h, dirty, reg := workerHarness(t)
			baby := workerBaby(t, h)
			if _, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), deviceRequest()); err != nil {
				t.Fatal(err)
			}
			h.StartFeed(t, h.Owner, testkit.TimerA)
			var sent []PushRequest
			w := newTestWorker(t, h, dirty, h.Authority, transportFunc(func(_ context.Context, r PushRequest) (PushResult, error) {
				sent = append(sent, r)
				return PushResult{Classification: Accepted}, nil
			}))
			run := func() {
				t.Helper()
				nextWorkerSecond()
				if err := w.RunOnce(t.Context(), 32); err != nil {
					t.Fatal(err)
				}
			}
			run()
			if len(sent) != 1 || sent[0].Event != "start" {
				t.Fatalf("first part start=%+v", sent)
			}
			generation := sent[0].Content.Remote.GenerationID
			card := activityRequest(h, baby, "b-card")
			card.KnownGenerationID = &generation
			if _, err := reg.activity(t, t.Context(), h.AccessToken("caregiver"), card); err != nil {
				t.Fatal(err)
			}
			run()
			if len(sent) != 2 || sent[1].Event != "update" {
				t.Fatalf("confirmation=%+v", sent)
			}
			// One sync: the left part stops into a record, the right part starts.
			workerStopFeed(t, h, baby, testkit.TimerA)
			right := testkit.MustMutation(t, "timer-feed.json")
			p := right.Payload.(protocol.TimerStartPayload)
			p.StartedAt, p.CurrentSide, p.CurrentSideStartedAt, p.SessionID = "2026-09-20T15:10:00Z", ptr("right"), ptr("2026-09-20T15:10:00Z"), tc.session
			right.MutationID, right.EntityID, right.Payload = h.CommandID(), next, p
			if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, right); err != nil || out.Status != "applied" {
				t.Fatalf("right part=%+v err=%v", out, err)
			}
			run()
			run()
			switched := sent[2:]
			if tc.session != nil && *tc.session == record {
				if len(switched) != 1 || switched[0].Event != "update" {
					t.Fatalf("side switch replaced the card: %s", describePushes(switched))
				}
				remote := switched[0].Content.Remote
				if remote.GenerationID != generation || !referenceEqual(remote.TimerID, ptr(next)) || !referenceEqual(remote.SessionID, ptr(record)) || switched[0].Content.ActivityKind != "breastRight" || switched[0].Token != "ccdd" {
					t.Fatalf("continuation update=%+v remote=%+v", switched[0], remote)
				}
				return
			}
			if len(switched) != 2 || switched[0].Event != "end" || switched[0].Content.Remote.GenerationID != generation || switched[1].Event != "start" || switched[1].Content.Remote.GenerationID == generation {
				t.Fatalf("new feed did not get a new generation: %s", describePushes(switched))
			}
		})
	}
}

func describePushes(sent []PushRequest) string {
	out := make([]string, 0, len(sent))
	for _, s := range sent {
		out = append(out, fmt.Sprintf("{%s %s token=%s kind=%s gen=%s}", s.Event, s.Attributes.KindRaw, s.Token, s.Content.ActivityKind, s.Content.Remote.GenerationID))
	}
	return strings.Join(out, " ")
}

// A family re-dirty while another runner holds a job's lease makes that lease's
// acknowledgement stale. The newer work must be reclaimed within a few one-second
// polls, not after the 30-second lease, with both send bounds intact.
func TestLiveActivityStaleLeaseRedirtyDeliversPromptly(t *testing.T) {
	h, dirty, reg := workerHarness(t)
	baby := workerBaby(t, h)
	h.StartFeed(t, h.Owner, testkit.TimerA)
	flushDirtyNow(t, h, dirty)
	_, second, err := h.NewDeviceSession("caregiver")
	if err != nil {
		t.Fatal(err)
	}
	for i, token := range []string{h.AccessToken("caregiver"), second} {
		device := deviceRequest()
		card := activityRequest(h, baby, fmt.Sprintf("card-%d", i))
		if i == 1 {
			device.InstallationID, device.PushToStartToken = otherInstallation, "bbcc"
			card.InstallationID, card.Token = otherInstallation, "ddee"
		}
		if _, err = reg.device(t, t.Context(), token, device); err != nil {
			t.Fatal(err)
		}
		if _, err = reg.activity(t, t.Context(), token, card); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = h.Migrator.Exec(t.Context(), `UPDATE live_activity_delivery SET completed_version=dirty_version WHERE kind='sleep'`); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	inflight, perTarget := 0, map[string]int{}
	maxInflight, maxPerTarget := 0, 0
	renamed := map[string]time.Time{}
	entered := make(chan string, 2)
	release := make(chan struct{})
	blocked := 0
	transport := transportFunc(func(ctx context.Context, r PushRequest) (PushResult, error) {
		mu.Lock()
		inflight++
		perTarget[r.Token]++
		maxInflight = max(maxInflight, inflight)
		maxPerTarget = max(maxPerTarget, perTarget[r.Token])
		hold := blocked < 2
		if hold {
			blocked++
		}
		if r.Attributes.BabyName == "renamed during send" {
			if _, seen := renamed[r.Token]; !seen {
				renamed[r.Token] = time.Now()
			}
		}
		mu.Unlock()
		defer func() {
			mu.Lock()
			inflight--
			perTarget[r.Token]--
			mu.Unlock()
		}()
		if hold {
			entered <- r.Token
			select {
			case <-release:
			case <-ctx.Done():
				return PushResult{Classification: Ambiguous}, ctx.Err()
			}
		}
		return PushResult{Classification: Accepted}, nil
	})
	w := newTestWorker(t, h, dirty, h.Authority, transport)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, nil) }()
	defer func() {
		cancel()
		<-done
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("both leased sends did not enter")
		}
	}
	// Both jobs are leased with a send in flight; this commit re-dirties them
	// once the worker's next poll flushes the change feed.
	h.RenameBaby(t, baby, "renamed during send")
	released := time.Now()
	close(release)
	deadline := time.Now().Add(12 * time.Second)
	for {
		mu.Lock()
		got := len(renamed)
		mu.Unlock()
		if got == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("re-dirtied leased jobs not redelivered within a few polls (lease is 30s): delivered=%v", renamed)
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	for token, at := range renamed {
		if at.Sub(released) > 5*time.Second {
			t.Fatalf("%s redelivered after %v", token, at.Sub(released))
		}
	}
	if maxInflight > 2 || maxPerTarget > 1 {
		t.Fatalf("send bounds: process=%d per-target=%d", maxInflight, maxPerTarget)
	}
}

// The release is an exact-lease CAS that only applies when newer dirty work has
// made the lease unacknowledgeable. It never edits lifecycle intent or completes
// work, so an attempt that entered transport stays start-unconfirmed.
func TestLiveActivityReleaseStaleLeaseFences(t *testing.T) {
	for _, interleaving := range []string{"newer_dirty", "current_dirty", "reclaimed_lease", "wrong_registration_version", "wrong_lease", "expired_original_lease"} {
		t.Run(interleaving, func(t *testing.T) {
			h, dirty, reg := workerHarness(t)
			baby := workerBaby(t, h)
			if _, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), deviceRequest()); err != nil {
				t.Fatal(err)
			}
			h.StartFeed(t, h.Owner, testkit.TimerA)
			q := NewQueue(h.Maintenance)
			claims, err := q.Claim(t.Context(), h.DatabaseNow(), 2)
			if err != nil {
				t.Fatal(err)
			}
			var claim Claim
			for _, c := range claims {
				if c.Kind == "feed" {
					claim = c
				} else if err = q.Complete(t.Context(), c); err != nil {
					t.Fatal(err)
				}
			}
			// Durable intent of a start that entered transport.
			intent := Reconcile(TargetState{}, lifecycleCard("feed", testkit.TimerA, ""), time.Now()).State
			if err = q.SaveTarget(t.Context(), claim, intent, false, time.Time{}, nil); err != nil {
				t.Fatal(err)
			}
			newerWork := func() {
				h.RenameBaby(t, baby, "newer work")
				flushDirtyNow(t, h, dirty)
			}
			captured := claim
			released := true
			switch interleaving {
			case "newer_dirty":
				newerWork()
			case "current_dirty":
				released = false
			case "reclaimed_lease":
				newerWork()
				reclaimed, e := q.Claim(t.Context(), claim.LeaseDeadline.Add(time.Second), 2)
				if e != nil || len(reclaimed) == 0 {
					t.Fatalf("reclaim=%v %v", reclaimed, e)
				}
				released = false
			case "wrong_registration_version":
				newerWork()
				captured.RegistrationVersion++
				released = false
			case "wrong_lease":
				newerWork()
				captured.LeaseID = otherInstallation
				released = false
			case "expired_original_lease":
				newerWork()
				claim.LeaseDeadline = time.Now().Add(-time.Second).Truncate(time.Microsecond)
				captured = claim
				if _, err = h.Migrator.Exec(t.Context(), `UPDATE live_activity_delivery SET lease_deadline=$1 WHERE job_id=$2`, claim.LeaseDeadline, claim.JobID); err != nil {
					t.Fatal(err)
				}
			}
			type row struct {
				dirty, completed uint64
				notBefore        time.Time
				lease            *string
				state            string
			}
			read := func() row {
				var r row
				if e := h.Migrator.QueryRow(t.Context(), `SELECT dirty_version,completed_version,not_before,lease_id::text,target_state::text FROM live_activity_delivery WHERE job_id=$1`, claim.JobID).Scan(&r.dirty, &r.completed, &r.notBefore, &r.lease, &r.state); e != nil {
					t.Fatal(e)
				}
				return r
			}
			before := read()
			raw, _ := json.Marshal(captured)
			if _, e := h.Runtime.Exec(t.Context(), `SELECT app.release_stale_live_activity_lease($1)`, raw); e == nil {
				t.Fatal("runtime role released a worker lease")
			}
			var accepted bool
			if err = h.Maintenance.QueryRow(t.Context(), `SELECT app.release_stale_live_activity_lease($1)`, raw).Scan(&accepted); err != nil {
				t.Fatal(err)
			}
			after := read()
			if accepted != released {
				t.Fatalf("release accepted=%v want %v", accepted, released)
			}
			if after.dirty != before.dirty || after.completed != before.completed || !after.notBefore.Equal(before.notBefore) || after.state != before.state {
				t.Fatalf("release changed work or intent: before=%+v after=%+v", before, after)
			}
			if !released {
				if !referenceEqual(after.lease, before.lease) {
					t.Fatal("rejected release changed the lease")
				}
				return
			}
			if after.lease != nil || after.dirty <= after.completed {
				t.Fatalf("stale lease not released or newer work consumed: %+v", after)
			}
			// The re-dirty also made the (completed) sleep job due again.
			fresh, err := q.Claim(t.Context(), h.DatabaseNow(), 2)
			var reclaimed *Claim
			for i := range fresh {
				if fresh[i].JobID == claim.JobID {
					reclaimed = &fresh[i]
				}
			}
			if err != nil || reclaimed == nil || reclaimed.LeaseID == claim.LeaseID {
				t.Fatalf("released job not reclaimable at once: %+v %v", fresh, err)
			}
			state, err := q.LoadTarget(t.Context(), *reclaimed)
			if err != nil || state.Status != "startUnconfirmed" || state.AttemptID != intent.AttemptID {
				t.Fatalf("ambiguous start lost across release: %+v %v", state, err)
			}
			if next := Reconcile(state, lifecycleCard("feed", testkit.TimerA, ""), time.Now().Add(time.Minute)); next.Event != "wait" {
				t.Fatalf("released ambiguous start was resent: %s", next.Event)
			}
		})
	}
}

// Review Focus 5: Live Activity cards never reach the wrong phone, never
// duplicate and never stop. The harness below is main's worker over a
// recording transport, with the owner's and the caregiver's phones both
// registered for push-to-start.

// recordedPush is one push the recording transport saw, by the device its
// token belongs to.
type recordedPush struct {
	device, event, token string
	request              PushRequest
}

// recordingTransport records each push by device and answers with answer
// (nil: Accepted).
type recordingTransport struct {
	mu      sync.Mutex
	devices map[string]string // token -> device ID
	pushes  []recordedPush
	answer  func(PushRequest) PushResult
}

func (r *recordingTransport) Send(_ context.Context, req PushRequest) (PushResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pushes = append(r.pushes, recordedPush{device: r.devices[req.Token], event: req.Event, token: req.Token, request: req})
	if r.answer != nil {
		return r.answer(req), nil
	}
	return PushResult{HTTPStatus: 200, Classification: Accepted}, nil
}

func (r *recordingTransport) bind(token, device string) {
	r.mu.Lock()
	r.devices[token] = device
	r.mu.Unlock()
}

func (r *recordingTransport) all() []recordedPush {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedPush(nil), r.pushes...)
}

func (r *recordingTransport) reset() {
	r.mu.Lock()
	r.pushes = nil
	r.mu.Unlock()
}

func (r *recordingTransport) has(device, event string) bool {
	for _, p := range r.all() {
		if p.device == device && p.event == event {
			return true
		}
	}
	return false
}

func (r *recordingTransport) any(device string) bool { return len(r.events(device)) > 0 }

func (r *recordingTransport) events(device string) []string {
	var out []string
	for _, p := range r.all() {
		if p.device == device {
			out = append(out, p.event+":"+p.token)
		}
	}
	return out
}

// toToken is every push to token, in order.
func (r *recordingTransport) toToken(token string) []recordedPush {
	var out []recordedPush
	for _, p := range r.all() {
		if p.token == token {
			out = append(out, p)
		}
	}
	return out
}

// workerClocks maps a focus harness's worker to its clock (drain advances it).
var workerClocks sync.Map

const (
	ownerStartToken     = "bbcc"
	caregiverStartToken = "aabb"
	caregiverCardToken  = "ccdd"
	ownerCardToken      = "ab01"
)

// liveActivityHarness is a worker over a recording transport, on a clock
// drain advances, with the owner's and the caregiver's phones registered
// for push-to-start (the fixture baby seeded).
func liveActivityHarness(t *testing.T) (*testkit.Harness, *Worker, *recordingTransport) {
	t.Helper()
	h, dirty, reg := workerHarness(t)
	workerBaby(t, h)
	owner := deviceRequest()
	owner.InstallationID, owner.PushToStartToken = otherInstallation, ownerStartToken
	if _, err := reg.device(t, t.Context(), h.AccessToken("owner"), owner); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), deviceRequest()); err != nil {
		t.Fatal(err)
	}
	sent := &recordingTransport{devices: map[string]string{ownerStartToken: h.Owner.DeviceID, caregiverStartToken: h.Caregiver.DeviceID,
		caregiverCardToken: h.Caregiver.DeviceID}}
	w := newTestWorker(t, h, dirty, h.Authority, sent)
	clock := newWorkerClock(h)
	w.Now = clock.Now
	workerClocks.Store(w, clock)
	return h, w, sent
}

func clockOf(t *testing.T, w *Worker) *workerClock {
	t.Helper()
	clock, ok := workerClocks.Load(w)
	if !ok {
		t.Fatal("not a focus harness worker")
	}
	return clock.(*workerClock)
}

// settle records the family's latest change in Dirty (as the change feed
// does) and runs passes a second apart until no job is due, with no
// maintenance: within the minute, before any sweep.
func settle(t *testing.T, h *testkit.Harness, w *Worker) {
	t.Helper()
	w.Dirty.Changed(h.FamilyID, h.FamilySequence(t))
	clock := clockOf(t, w)
	for i := 0; i < 10; i++ {
		clock.Advance(time.Second)
		if err := w.RunOnce(t.Context(), WorkerBatchLimit); err != nil {
			t.Fatal(err)
		}
		if scalar(t, h, `SELECT count(*) FROM live_activity_delivery WHERE dirty_version>completed_version AND lease_id IS NULL AND not_before<=$1`, clock.Now().Add(2*time.Second)) == 0 {
			return
		}
	}
	t.Fatal("the worker did not become idle")
}

// drain settles, then runs the minute maintenance (the registration and
// work sweeps) and settles again.
func drain(t *testing.T, h *testkit.Harness, w *Worker) {
	t.Helper()
	settle(t, h, w)
	forceSweep(w)
	settle(t, h, w)
}

// registerCaregiverCard registers the caregiver's card update token for
// generation (the card the push-to-start created).
func registerCaregiverCard(t *testing.T, h *testkit.Harness, generation string) {
	t.Helper()
	card := activityRequest(h, testkit.MustMutation(t, "baby.json").EntityID, "card")
	card.KnownGenerationID = &generation
	reg := registrar{h: h, service: NewService(h.Authority, topic)}
	if _, err := reg.activity(t, t.Context(), h.AccessToken("caregiver"), card); err != nil {
		t.Fatal(err)
	}
}

// startedGeneration is the generation of the push-to-start sent to token.
func startedGeneration(t *testing.T, sent *recordingTransport, token string) string {
	t.Helper()
	for _, p := range sent.toToken(token) {
		if p.event == "start" {
			return p.request.Content.Remote.GenerationID
		}
	}
	t.Fatalf("no push-to-start to %s: %+v", token, sent.all())
	return ""
}

func TestRemovedMemberGetsNoLiveActivityPush(t *testing.T) { // Review Focus 5
	h, w, sent := liveActivityHarness(t)
	baby := testkit.MustMutation(t, "baby.json").EntityID
	// The owner starts a feed and registers its own card for it (adopted:
	// the starting phone gets no push-to-start); the caregiver is pushed a
	// start and registers the card it created.
	h.StartFeed(t, h.Owner, testkit.TimerA)
	ownerCard := activityRequest(h, baby, "own-card")
	ownerCard.InstallationID, ownerCard.Token = otherInstallation, ownerCardToken
	sent.bind(ownerCardToken, h.Owner.DeviceID)
	reg := registrar{h: h, service: NewService(h.Authority, topic)}
	if _, err := reg.activity(t, t.Context(), h.AccessToken("owner"), ownerCard); err != nil {
		t.Fatal(err)
	}
	drain(t, h, w)
	if !sent.has(h.Caregiver.DeviceID, "start") {
		t.Fatal("the caregiver's phone got no push-to-start")
	}
	registerCaregiverCard(t, h, startedGeneration(t, sent, caregiverStartToken))
	drain(t, h, w)
	if len(sent.toToken(caregiverCardToken)) != 1 || len(sent.toToken(ownerCardToken)) == 0 {
		t.Fatalf("before the removal: caregiver %v owner %v", sent.events(h.Caregiver.DeviceID), sent.events(h.Owner.DeviceID))
	}
	if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID); err != nil {
		t.Fatal(err)
	}
	sent.reset()
	// Within the minute, before any sweep revokes the registration, the
	// owner stops the feed (the caregiver's card would get an end) and
	// starts a sleep (the caregiver's phone would get a push-to-start):
	// only the snapshot's authorization keeps both from the removed member.
	workerStopFeed(t, h, baby, testkit.TimerA)
	applyMutation(t, h, h.Owner, sleepTimerMutation(t, h, testkit.TimerB, "timer_start", baby, 16))
	settle(t, h, w)
	if sent.any(h.Caregiver.DeviceID) {
		t.Fatalf("a removed member's phone was pushed: %v", sent.events(h.Caregiver.DeviceID))
	}
	if ends := sent.toToken(ownerCardToken); len(ends) != 1 || ends[0].event != "end" {
		t.Fatalf("the owner's card was not ended: %v", sent.events(h.Owner.DeviceID))
	}
	// The minute sweep then revokes the removed member's registration.
	drain(t, h, w)
	if sent.any(h.Caregiver.DeviceID) {
		t.Fatalf("a removed member's phone was pushed: %v", sent.events(h.Caregiver.DeviceID))
	}
	if n := tenantCount(t, h, h.Caregiver.AccountID, `SELECT count(*) FROM live_activity_devices WHERE device_id=$1 AND NOT revoked`, h.Caregiver.DeviceID); n != 0 {
		t.Fatal("the sweep left the removed member's registration live")
	}
	if n := tenantCount(t, h, h.Caregiver.AccountID, `SELECT count(*) FROM live_activity_devices WHERE device_id=$1 AND revoked`, h.Caregiver.DeviceID); n != 1 {
		t.Fatalf("the removed member's registration is gone instead of revoked: %d", n)
	}
	if n := scalar(t, h, `SELECT count(*) FROM live_activity_registration_index WHERE device_id=$1 AND NOT revoked`, h.Caregiver.DeviceID); n != 0 {
		t.Fatal("the registration index still schedules the removed member")
	}
	// The owner's phone keeps its registration.
	if n := tenantCount(t, h, h.Owner.AccountID, `SELECT count(*) FROM live_activity_devices WHERE device_id=$1 AND NOT revoked`, h.Owner.DeviceID); n != 1 {
		t.Fatal("the sweep revoked the owner's registration")
	}
}

func TestStartingPhoneGetsNoPushToStart(t *testing.T) { // Review Focus 5
	h, w, sent := liveActivityHarness(t)
	h.StartFeed(t, h.Owner, testkit.TimerA) // the owner's phone shows its own local card
	drain(t, h, w)
	if sent.has(h.Owner.DeviceID, "start") || !sent.has(h.Caregiver.DeviceID, "start") {
		t.Fatalf("starts owner=%v caregiver=%v", sent.events(h.Owner.DeviceID), sent.events(h.Caregiver.DeviceID))
	}
	// Nor at a later write: the start stays suppressed for the phone that
	// started the timer, and the caregiver's start is not repeated.
	h.RenameBaby(t, testkit.MustMutation(t, "baby.json").EntityID, "renamed")
	drain(t, h, w)
	if sent.any(h.Owner.DeviceID) || len(sent.toToken(caregiverStartToken)) != 1 {
		t.Fatalf("after a write: owner=%v caregiver=%v", sent.events(h.Owner.DeviceID), sent.events(h.Caregiver.DeviceID))
	}
}

func TestStoppedTimerEndsItsCardOnce(t *testing.T) { // Review Focus 5
	h, w, sent := liveActivityHarness(t)
	h.StartFeed(t, h.Owner, testkit.TimerA)
	drain(t, h, w)
	registerCaregiverCard(t, h, startedGeneration(t, sent, caregiverStartToken))
	drain(t, h, w)
	if pushes := sent.toToken(caregiverCardToken); len(pushes) != 1 || pushes[0].event != "update" {
		t.Fatalf("the pushed card was not confirmed: %v", sent.events(h.Caregiver.DeviceID))
	}
	sent.reset()
	workerStopFeed(t, h, testkit.MustMutation(t, "baby.json").EntityID, testkit.TimerA) // checked: applied
	for i := 0; i < 6; i++ {
		drain(t, h, w)
	}
	pushes := sent.toToken(caregiverCardToken)
	if len(pushes) != 1 || pushes[0].event != "end" {
		t.Fatalf("the stopped timer's card got %v, want exactly one end", sent.events(h.Caregiver.DeviceID))
	}
	payload, err := encodePushPayload(pushes[0].request)
	if err != nil || !strings.Contains(string(payload), `"dismissal-date":`) {
		t.Fatalf("the end carries no dismissal date: %s %v", payload, err)
	}
	if sent.any(h.Owner.DeviceID) || len(sent.all()) != 1 {
		t.Fatalf("other pushes after the stop: %+v", sent.all())
	}
}

func TestInvalidCardTokenIsClearedOnce(t *testing.T) { // Review Focus 5
	h, w, sent := liveActivityHarness(t)
	baby := testkit.MustMutation(t, "baby.json").EntityID
	h.StartFeed(t, h.Owner, testkit.TimerA)
	drain(t, h, w)
	sent.answer = func(r PushRequest) PushResult {
		if r.Token == caregiverCardToken {
			return PushResult{HTTPStatus: 410, Reason: "Unregistered", Classification: InvalidToken}
		}
		return PushResult{HTTPStatus: 200, Classification: Accepted}
	}
	registerCaregiverCard(t, h, startedGeneration(t, sent, caregiverStartToken))
	drain(t, h, w)
	if pushes := sent.toToken(caregiverCardToken); len(pushes) != 1 {
		t.Fatalf("pushes to the card: %v", sent.events(h.Caregiver.DeviceID))
	}
	if n := workerTokenCount(t, h, `SELECT count(*) FROM live_activity_tokens WHERE activity_id='card' AND token IS NULL AND state='ended' AND server_retired`); n != 1 {
		t.Fatal("the 410 token was not cleared and retired")
	}
	// A later write sends nothing to it.
	h.RenameBaby(t, baby, "after the dead token")
	drain(t, h, w)
	if pushes := sent.toToken(caregiverCardToken); len(pushes) != 1 {
		t.Fatalf("the dead token was retried: %v", sent.events(h.Caregiver.DeviceID))
	}
	// A 429 answer holds that card's send until its Retry-After (within the
	// worker's one-minute cap): the next lifecycle's push-to-start.
	clock := clockOf(t, w)
	var mu sync.Mutex
	limited := true
	var retryAfter time.Time
	sent.answer = func(r PushRequest) PushResult {
		mu.Lock()
		defer mu.Unlock()
		if limited && r.Token == caregiverStartToken {
			limited = false
			retryAfter = clock.Now().Add(30 * time.Second)
			return PushResult{HTTPStatus: 429, Reason: "TooManyRequests", RetryAfter: retryAfter, Classification: Retryable}
		}
		return PushResult{HTTPStatus: 200, Classification: Accepted}
	}
	sent.reset()
	applyMutation(t, h, h.Owner, sleepTimerMutation(t, h, testkit.TimerB, "timer_start", baby, 16))
	drain(t, h, w)
	if starts := sent.toToken(caregiverStartToken); len(starts) != 1 || starts[0].event != "start" || starts[0].request.Attributes.KindRaw != "sleep" {
		t.Fatalf("the sleep's push-to-start: %v", sent.events(h.Caregiver.DeviceID))
	}
	for clock.Now().Add(time.Second).Before(retryAfter) {
		clock.Advance(5 * time.Second)
		if !clock.Now().Before(retryAfter) {
			break
		}
		if err := w.RunOnce(t.Context(), WorkerBatchLimit); err != nil {
			t.Fatal(err)
		}
		if n := len(sent.toToken(caregiverStartToken)); n != 1 {
			t.Fatalf("sent again %v before Retry-After", clock.Now().Sub(retryAfter))
		}
	}
	clock.Advance(retryAfter.Sub(clock.Now()) + time.Second)
	if err := w.RunOnce(t.Context(), WorkerBatchLimit); err != nil {
		t.Fatal(err)
	}
	if starts := sent.toToken(caregiverStartToken); len(starts) != 2 || starts[1].event != "start" || starts[1].request.APNSID == starts[0].request.APNSID {
		t.Fatalf("not retried after Retry-After: %v", sent.events(h.Caregiver.DeviceID))
	}
	if got := historyLines(t, h); !strings.Contains(strings.Join(got, "|"), "start retryable 429 TooManyRequests") {
		t.Fatalf("history=%q", got)
	}
}

// A flush that fails puts the families back into Dirty for the next poll; a
// newer sequence recorded meanwhile wins. Changed ignores an invalid family
// and keeps each family's newest sequence.
func TestLiveActivityFailedFlushKeepsTheFamilies(t *testing.T) {
	const family, other = "f0000000-0000-4000-8000-0000000000f1", "f0000000-0000-4000-8000-0000000000f2"
	dirty := NewDirty()
	dirty.Changed(family, 5)
	dirty.Changed(family, 3)
	dirty.Changed(other, 2)
	dirty.Changed("not-a-family", 9)
	if dirty.Len() != 2 {
		t.Fatalf("dirty holds %d families", dirty.Len())
	}
	pool, err := pgxpool.New(t.Context(), "postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	w := &Worker{Queue: NewQueue(pool), Dirty: dirty}
	if err := w.flush(t.Context()); err == nil {
		t.Fatal("a flush over a closed pool succeeded")
	}
	dirty.Changed(other, 7)
	families, sequences := dirty.take()
	if strings.Join(families, ",") != family+","+other || sequences[0] != 5 || sequences[1] != 7 {
		t.Fatalf("after a failed flush: %v %v", families, sequences)
	}
	dirty.restore(families, []uint64{4, 6})
	if families, sequences = dirty.take(); sequences[0] != 4 || sequences[1] != 6 {
		t.Fatalf("restore into an empty Dirty: %v %v", families, sequences)
	}
	dirty.Changed(family, 9)
	dirty.restore([]string{family}, []uint64{4})
	if _, sequences = dirty.take(); len(sequences) != 1 || sequences[0] != 9 {
		t.Fatalf("an older restored sequence won over a newer change: %v", sequences)
	}
}

// Every queue statement runs inside a maintenance worker slot: with every
// slot held (the wake worker, export and purge at work), the worker claims
// nothing until one is free, and then delivers.
func TestLiveActivityWorkerWaitsForAMaintenanceSlot(t *testing.T) {
	h, dirty, reg := workerHarness(t)
	workerBaby(t, h)
	if _, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), deviceRequest()); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	var sent []PushRequest
	w := newTestWorker(t, h, dirty, h.Authority, transportFunc(func(_ context.Context, r PushRequest) (PushResult, error) {
		sent = append(sent, r)
		return PushResult{HTTPStatus: 200, Classification: Accepted}, nil
	}))
	var held []func()
	for range w.Slots.Size() {
		release, err := w.Slots.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, release)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if err := w.RunOnce(ctx, WorkerBatchLimit); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the worker ran without a slot: %v", err)
	}
	if n := scalar(t, h, `SELECT count(*) FROM live_activity_delivery WHERE lease_id IS NOT NULL OR completed_version>0`); n != 0 || len(sent) != 0 {
		t.Fatalf("claimed %d jobs, sent %d without a slot", n, len(sent))
	}
	for _, release := range held {
		release()
	}
	if err := w.RunOnce(t.Context(), WorkerBatchLimit); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0].Event != "start" {
		t.Fatalf("after the slots were freed: %s", describePushes(sent))
	}
}
