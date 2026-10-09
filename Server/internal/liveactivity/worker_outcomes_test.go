package liveactivity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/security"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func nextWorkerSecond() { time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second))) }

// sleepTimerMutation starts a sleep at `hour`:00 on the fixture day, or stops
// it an hour later.
func sleepTimerMutation(t *testing.T, h *testkit.Harness, timer, operation, baby string, hour int) protocol.Mutation {
	t.Helper()
	m := testkit.MustMutation(t, "timer-sleep.json")
	m.MutationID, m.EntityID = h.CommandID(), timer
	if operation == "timer_stop" {
		m.Operation = "timer_stop"
		m.Payload = protocol.TimerStopPayload{BabyID: baby, CompletedRecordID: "8" + timer[1:], StoppedAt: fmt.Sprintf("2026-09-20T%02d:00:00Z", hour+1)}
		return m
	}
	p := m.Payload.(protocol.TimerStartPayload)
	p.StartedAt = fmt.Sprintf("2026-09-20T%02d:00:00Z", hour)
	m.ClientMutationTime = p.StartedAt
	m.Payload = p
	return m
}

func applyMutation(t *testing.T, h *testkit.Harness, p access.Principal, m protocol.Mutation) {
	t.Helper()
	if out, err := h.Service.Apply(t.Context(), p, h.FamilyID, m); err != nil || out.Status != "applied" {
		t.Fatalf("%s %s=%+v err=%v", m.Operation, m.EntityID, out, err)
	}
}

// The phone that starts a timer requests its own card at once and registers
// its update token bound to that timer (no generation: the server has pushed
// nothing to it). Start and registration can both land before the worker sees
// either, while the target is terminal from the previous lifecycle — or from no
// lifecycle at all. The card must be adopted on that same pass, not at the next
// family change, and never answered with a push-to-start.
func TestLiveActivityOwnTimerCardAdoptedWhenClaimedWithItsStart(t *testing.T) {
	h := testkit.New(t)
	baby := seedBaby(t, h)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	if _, err := service.RegisterDevice(t.Context(), h.Owner, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	guard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	var sent []PushRequest
	w := NewWorker(NewQueue(h.LiveActivity), guard, transportFunc(func(_ context.Context, r PushRequest) (PushResult, error) {
		sent = append(sent, r)
		return PushResult{HTTPStatus: 200, Classification: Accepted}, nil
	}))
	// Nothing runs yet: the pass leaves both targets terminal with no generation.
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	ownCard := func(id, token, timer string) {
		t.Helper()
		req := activityRequest(h, baby, id)
		req.Scope.KindRaw, req.Token, req.TimerID = "sleep", token, &timer
		if _, err := service.RegisterActivity(t.Context(), h.Owner, req); err != nil {
			t.Fatal(err)
		}
	}
	first := "52000000-0000-4000-8000-000000000001"
	applyMutation(t, h, h.Owner, sleepTimerMutation(t, h, first, "timer_start", baby, 16))
	ownCard("own-sleep-1", "ab01", first)
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0].Event != "update" || sent[0].Token != "ab01" || sent[0].Content.Remote.TimerID == nil || *sent[0].Content.Remote.TimerID != first {
		t.Fatalf("own card claimed with its start: sends=%s", describePushes(sent))
	}
	firstGeneration := sent[0].Content.Remote.GenerationID
	nextWorkerSecond()
	applyMutation(t, h, h.Owner, sleepTimerMutation(t, h, first, "timer_stop", baby, 16))
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[1].Event != "end" || sent[1].Token != "ab01" {
		t.Fatalf("own card end: sends=%s", describePushes(sent))
	}
	// The target is now terminal with the first lifecycle's generation.
	second := "52000000-0000-4000-8000-000000000002"
	applyMutation(t, h, h.Owner, sleepTimerMutation(t, h, second, "timer_start", baby, 18))
	ownCard("own-sleep-2", "ab02", second)
	nextWorkerSecond()
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 3 || sent[2].Event != "update" || sent[2].Token != "ab02" || sent[2].Content.Remote.GenerationID == firstGeneration {
		t.Fatalf("own card claimed after a previous lifecycle: sends=%s", describePushes(sent))
	}
	for _, s := range sent {
		if s.Event == "start" {
			t.Fatalf("the originating phone was pushed a start: %s", describePushes(sent))
		}
	}
}

// The phone ends one sleep and starts the next, and registers the next one's
// card, before the worker sees any of it. The new card names the new timer; it
// is not the previous lifecycle's card, whatever its activity ID. Taken for
// one, it would be picked for the previous lifecycle's end, the send would be
// deferred when the re-read disagreed, and neither card would move on that
// pass. Instead the previous card is ended and the new one adopted promptly.
func TestLiveActivityOwnNextCardIsNotEndedWithThePreviousLifecycle(t *testing.T) {
	h := testkit.New(t)
	baby := seedBaby(t, h)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	if _, err := service.RegisterDevice(t.Context(), h.Owner, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	guard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	var sent []PushRequest
	w := NewWorker(NewQueue(h.LiveActivity), guard, transportFunc(func(_ context.Context, r PushRequest) (PushResult, error) {
		sent = append(sent, r)
		return PushResult{HTTPStatus: 200, Classification: Accepted}, nil
	}))
	ownCard := func(id, token, timer string) {
		t.Helper()
		req := activityRequest(h, baby, id)
		req.Scope.KindRaw, req.Token, req.TimerID = "sleep", token, &timer
		if _, err := service.RegisterActivity(t.Context(), h.Owner, req); err != nil {
			t.Fatal(err)
		}
	}
	first := "52000000-0000-4000-8000-000000000011"
	applyMutation(t, h, h.Owner, sleepTimerMutation(t, h, first, "timer_start", baby, 16))
	ownCard("own-sleep-z", "ac01", first)
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0].Event != "update" || sent[0].Token != "ac01" {
		t.Fatalf("first card: %s", describePushes(sent))
	}
	second := "52000000-0000-4000-8000-000000000012"
	applyMutation(t, h, h.Owner, sleepTimerMutation(t, h, first, "timer_stop", baby, 16))
	applyMutation(t, h, h.Owner, sleepTimerMutation(t, h, second, "timer_start", baby, 18))
	// Sorts before the previous card: the smallest ID among cards the worker
	// took for the previous lifecycle would receive its end.
	ownCard("own-sleep-a", "ac02", second)
	for i := 0; i < 2; i++ {
		nextWorkerSecond()
		if err := w.RunOnce(t.Context(), 32); err != nil {
			t.Fatal(err)
		}
	}
	if len(sent) != 3 || sent[1].Event != "end" || sent[1].Token != "ac01" || sent[2].Event != "update" || sent[2].Token != "ac02" {
		t.Fatalf("previous end and next adoption: %s", describePushes(sent))
	}
	if sent[2].Content.Remote.GenerationID == sent[1].Content.Remote.GenerationID {
		t.Fatalf("next card kept the previous generation: %s", describePushes(sent))
	}
}

// After the server ends a duplicate, the phone reports that losing card as
// ended or dismissed at a newer token version. The server's own retirement
// must survive that report, or the shared lifecycle reads as dismissed and the
// surviving card is never updated again.
func TestLiveActivityServerRetiredDuplicateReportKeepsTheSurvivor(t *testing.T) {
	for _, reported := range []string{"ended", "dismissed"} {
		t.Run(reported, func(t *testing.T) {
			h := testkit.New(t)
			baby := seedBaby(t, h)
			service := NewService(h.Security, familyResolver{h.Family}, topic)
			if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
				t.Fatal(err)
			}
			h.StartFeed(t, h.Owner, testkit.TimerA)
			guard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
			var sent []PushRequest
			w := NewWorker(NewQueue(h.LiveActivity), guard, transportFunc(func(_ context.Context, r PushRequest) (PushResult, error) {
				sent = append(sent, r)
				return PushResult{HTTPStatus: 200, Classification: Accepted}, nil
			}))
			if err := w.RunOnce(t.Context(), 32); err != nil {
				t.Fatal(err)
			}
			generation := sent[0].Content.Remote.GenerationID
			loser := activityRequest(h, baby, "b-card")
			loser.Token = "eeff"
			loser.KnownGenerationID = &generation
			for _, req := range []protocol.LiveActivityTokenRegistration{activityRequest(h, baby, "a-card"), loser} {
				req.KnownGenerationID = &generation
				if _, err := service.RegisterActivity(t.Context(), h.Caregiver, req); err != nil {
					t.Fatal(err)
				}
			}
			nextWorkerSecond()
			if err := w.RunOnce(t.Context(), 32); err != nil {
				t.Fatal(err)
			}
			if len(sent) != 2 || sent[1].Event != "end" || sent[1].Token != "eeff" {
				t.Fatalf("duplicate end: %s", describePushes(sent))
			}
			// ActivityKit ends the loser; the phone reports it.
			loser.TokenVersion, loser.State = 2, reported
			if _, err := service.RegisterActivity(t.Context(), h.Caregiver, loser); err != nil {
				t.Fatal(err)
			}
			if workerTokenCount(t, h, `SELECT count(*) FROM live_activity_tokens WHERE activity_id='b-card' AND server_retired`) != 1 {
				t.Fatal("the phone's report reset the server's retirement")
			}
			nextWorkerSecond()
			if err := w.RunOnce(t.Context(), 32); err != nil {
				t.Fatal(err)
			}
			h.RenameBaby(t, baby, "after the duplicate")
			nextWorkerSecond()
			if err := w.RunOnce(t.Context(), 32); err != nil {
				t.Fatal(err)
			}
			updates := 0
			for _, s := range sent[2:] {
				if s.Event == "update" && s.Token == "ccdd" {
					updates++
				}
				if s.Token == "eeff" {
					t.Fatalf("retired duplicate pushed again: %s", describePushes(sent))
				}
			}
			if updates != 2 {
				t.Fatalf("surviving card frozen after the loser was reported %s: %s", reported, describePushes(sent))
			}
		})
	}
}

func historyLines(t *testing.T, h *testkit.Harness) []string {
	t.Helper()
	rows, err := h.Migrator.Query(t.Context(), `SELECT event||' '||coalesce(result,'-')||' '||coalesce(http_status::text,'-')||' '||coalesce(reason,'-') FROM live_activity_history ORDER BY recorded_at,id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		out = append(out, line)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func captureDefaultLog(t *testing.T) *logCapture {
	t.Helper()
	capture := &logCapture{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(capture, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return capture
}

// Every attempt's APNs response class, status and allowlisted reason is kept
// with its history row — never a token or payload — and every result other
// than accepted leaves one bounded log line.
func TestLiveActivityHistoryRecordsResponseClasses(t *testing.T) {
	h := testkit.New(t)
	baby := seedBaby(t, h)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	logs := captureDefaultLog(t)
	guard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	results := []PushResult{
		{HTTPStatus: 200, Classification: Accepted},
		{HTTPStatus: 503, Reason: "ServiceUnavailable", Classification: Retryable},
		{HTTPStatus: 410, Reason: "Unregistered", Classification: InvalidToken},
	}
	var sent []PushRequest
	w := NewWorker(NewQueue(h.LiveActivity), guard, transportFunc(func(_ context.Context, r PushRequest) (PushResult, error) {
		sent = append(sent, r)
		result := results[0]
		results = results[1:]
		return result, nil
	}))
	// The shortest retry delay, so the retried attempt is due one clock second
	// later; the worker's clock is advanced, never slept on.
	w.Jitter = func(time.Duration) time.Duration { return time.Second }
	clock := newWorkerClock()
	w.Now = clock.Now
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	req := activityRequest(h, baby, "card")
	req.KnownGenerationID = &sent[0].Content.Remote.GenerationID
	if _, err := service.RegisterActivity(t.Context(), h.Caregiver, req); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 3 {
		t.Fatalf("sends=%s", describePushes(sent))
	}
	want := []string{"start accepted 200 -", "update retryable 503 ServiceUnavailable", "update invalidToken 410 Unregistered"}
	if got := historyLines(t, h); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("history=%q want %q", got, want)
	}
	if n := scalar(t, h, `SELECT count(*) FROM live_activity_delivery WHERE kind='feed' AND last_reason='invalidToken'`); n != 1 {
		t.Fatalf("delivery last_reason rows=%d", n)
	}
	out := logs.String()
	if strings.Count(out, "live activity push not accepted") != 2 || !strings.Contains(out, "class=retryable") || !strings.Contains(out, "status=410") || !strings.Contains(out, "reason=Unregistered") {
		t.Fatalf("logs=%s", out)
	}
	for _, secret := range []string{"ccdd", "aabb", req.Scope.BabyID, h.FamilyID} {
		if strings.Contains(out, secret) {
			t.Fatalf("log line carries a token or identifier %q: %s", secret, out)
		}
	}
}

// A wrong bundle ID (topic) is the operator's configuration, not a dead
// device: it must not wipe start or update tokens, or fence the generation.
func TestLiveActivityTopicMismatchKeepsTokens(t *testing.T) {
	h := testkit.New(t)
	baby := seedBaby(t, h)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	guard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	var mu sync.Mutex
	reasons := []string{"", "DeviceTokenNotForTopic"}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		reason := reasons[0]
		if len(reasons) > 1 {
			reasons = reasons[1:]
		}
		mu.Unlock()
		if reason == "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"reason":%q}`, reason)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	transport := newAPNSTransport("TEAM123456", "KEY1234567", "com.example.baby", testAPNSKey(t), server.Client(), server.URL, server.URL)
	w := NewWorker(NewQueue(h.LiveActivity), guard, transport)
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	var generation string
	if err := h.Migrator.QueryRow(t.Context(), `SELECT target_state->>'generation_id' FROM live_activity_delivery WHERE kind='feed'`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	req := activityRequest(h, baby, "card")
	req.KnownGenerationID = &generation
	if _, err := service.RegisterActivity(t.Context(), h.Caregiver, req); err != nil {
		t.Fatal(err)
	}
	nextWorkerSecond()
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if workerTokenCount(t, h, `SELECT count(*) FROM live_activity_tokens WHERE activity_id='card' AND state='active' AND token='ccdd' AND NOT server_retired`) != 1 {
		t.Fatal("a topic mismatch invalidated the update token")
	}
	if workerTokenCount(t, h, `SELECT count(*) FROM live_activity_devices WHERE push_token='aabb'`) != 1 {
		t.Fatal("a topic mismatch cleared the start token")
	}
	if n := scalar(t, h, `SELECT count(*) FROM live_activity_delivery WHERE kind='feed' AND coalesce(target_state->>'dismissed_generation','')=$1`, generation); n != 0 {
		t.Fatal("a topic mismatch fenced the generation")
	}
	if got := historyLines(t, h); len(got) != 2 || got[1] != "update configurationFailure 400 DeviceTokenNotForTopic" {
		t.Fatalf("history=%q", got)
	}
}

// failingGuard fails the snapshot for one registration only, the way corrupt
// stored data or a projection bug fails one job and no other.
type failingGuard struct {
	SnapshotGuard
	registration string
	err          error
}

func (g failingGuard) WithLiveActivitySnapshot(ctx context.Context, s security.LiveActivitySubject, run func(pgx.Tx, access.Grant) error) error {
	if s.RegistrationID == g.registration {
		return g.err
	}
	return g.SnapshotGuard.WithLiveActivitySnapshot(ctx, s, run)
}

func registerCaregiverDevices(t *testing.T, h *testkit.Harness, service *Service, count int) {
	t.Helper()
	for i := 1; i <= count; i++ {
		p, err := h.NewDevice("caregiver")
		if err != nil {
			t.Fatal(err)
		}
		r := deviceRequest()
		r.InstallationID = fmt.Sprintf("70000000-0000-4000-8000-%012d", i)
		r.PushToStartToken = fmt.Sprintf("%04x", i)
		if _, err = service.RegisterDevice(t.Context(), p, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.Migrator.Exec(t.Context(), `UPDATE live_activity_delivery SET completed_version=dirty_version WHERE kind='sleep'`); err != nil {
		t.Fatal(err)
	}
}

// One job that fails deterministically is parked with a recorded reason and
// backs off; it must not stop the worker, and every other job still delivers.
// Infrastructure failures still stop the worker for its supervisor.
func TestLiveActivityPerJobFailureIsParked(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	h.StartFeed(t, h.Owner, testkit.TimerA)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	registerCaregiverDevices(t, h, service, 2)
	var failing string
	if err := h.Migrator.QueryRow(t.Context(), `SELECT registration_id::text FROM live_activity_registration_index WHERE installation_id='70000000-0000-4000-8000-000000000001'`).Scan(&failing); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	var mu sync.Mutex
	var sent []PushRequest
	transport := transportFunc(func(_ context.Context, r PushRequest) (PushResult, error) {
		mu.Lock()
		sent = append(sent, r)
		mu.Unlock()
		return PushResult{HTTPStatus: 200, Classification: Accepted}, nil
	})
	logs := captureDefaultLog(t)
	w := NewWorker(NewQueue(h.LiveActivity), failingGuard{snapshot, failing, errors.New("invalid stored projection input")}, transport)
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatalf("a per-job failure stopped the worker: %v", err)
	}
	if len(sent) != 1 || sent[0].Token != "0002" || sent[0].Event != "start" {
		t.Fatalf("the healthy job did not deliver: %s", describePushes(sent))
	}
	if n := scalar(t, h, `SELECT count(*) FROM live_activity_delivery WHERE registration_id=$1 AND kind='feed' AND last_reason='dispatch_failed' AND not_before>clock_timestamp() AND lease_id IS NULL AND dirty_version>completed_version`, failing); n != 1 {
		t.Fatal("the failing job was not parked with its reason")
	}
	if out := logs.String(); !strings.Contains(out, "live activity job parked") || strings.Contains(out, "invalid stored projection input") {
		t.Fatalf("parked job log=%s", out)
	}
	// Parked, it is not claimed again at once.
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 {
		t.Fatalf("parked job retried without backoff: %s", describePushes(sent))
	}
	// The worker's database or readiness is gone: that still stops the worker.
	if _, err := h.Migrator.Exec(t.Context(), `UPDATE live_activity_delivery SET not_before=clock_timestamp() WHERE registration_id=$1`, failing); err != nil {
		t.Fatal(err)
	}
	w = NewWorker(NewQueue(h.LiveActivity), failingGuard{snapshot, failing, security.ErrUnavailable}, transport)
	if err := w.RunOnce(t.Context(), 32); !errors.Is(err, security.ErrUnavailable) {
		t.Fatalf("infrastructure failure=%v", err)
	}
}

// A serialization failure or deadlock (SQLSTATE class 40) is a race the
// statement loses once and wins on a retry: neither the worker's problem nor
// the job's. The job is retried after the shortest backoff with no failure
// counted against it, instead of being parked for up to a minute as
// dispatch_failed; every other job still delivers.
func TestLiveActivityTransientConflictIsRetriedPromptlyNotParked(t *testing.T) {
	for _, code := range []string{"40001", "40P01"} {
		if IsInfrastructureError(t.Context(), &pgconn.PgError{Code: code}) || !IsTransientConflict(&pgconn.PgError{Code: code}) {
			t.Fatalf("sqlstate %s misclassified", code)
		}
	}
	if IsTransientConflict(&pgconn.PgError{Code: "42P01"}) || IsTransientConflict(errors.New("x")) {
		t.Fatal("a non-conflict classified as transient")
	}
	h := testkit.New(t)
	seedBaby(t, h)
	h.StartFeed(t, h.Owner, testkit.TimerA)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	registerCaregiverDevices(t, h, service, 2)
	var racing string
	if err := h.Migrator.QueryRow(t.Context(), `SELECT registration_id::text FROM live_activity_registration_index WHERE installation_id='70000000-0000-4000-8000-000000000001'`).Scan(&racing); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	var mu sync.Mutex
	var sent []PushRequest
	transport := transportFunc(func(_ context.Context, r PushRequest) (PushResult, error) {
		mu.Lock()
		sent = append(sent, r)
		mu.Unlock()
		return PushResult{HTTPStatus: 200, Classification: Accepted}, nil
	})
	logs := captureDefaultLog(t)
	conflict := &pgconn.PgError{Code: "40001", Message: "could not serialize access due to concurrent update"}
	w := NewWorker(NewQueue(h.LiveActivity), failingGuard{snapshot, racing, conflict}, transport)
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatalf("a transient conflict stopped the worker: %v", err)
	}
	if len(sent) != 1 || sent[0].Token != "0002" || sent[0].Event != "start" {
		t.Fatalf("the healthy job did not deliver: %s", describePushes(sent))
	}
	// Released with the shortest backoff, not parked: no dispatch_failed, and
	// claimable again within seconds.
	if n := scalar(t, h, `SELECT count(*) FROM live_activity_delivery WHERE registration_id=$1 AND kind='feed' AND last_reason='transient_conflict' AND lease_id IS NULL AND not_before<=clock_timestamp()+interval '3 seconds' AND dirty_version>completed_version`, racing); n != 1 {
		t.Fatalf("the racing job was not deferred briefly: reason=%v", scalar(t, h, `SELECT count(*) FROM live_activity_delivery WHERE registration_id=$1 AND last_reason='dispatch_failed'`, racing))
	}
	if out := logs.String(); strings.Contains(out, "live activity job parked") || !strings.Contains(out, "live activity job deferred") || strings.Contains(out, "concurrent update") {
		t.Fatalf("deferred job log=%s", out)
	}
	// A job that keeps losing the race backs off further each time, on the
	// ordinary progression (2, 4, 8 ... 60 s), still as transient_conflict.
	if _, err := h.Migrator.Exec(t.Context(), `UPDATE live_activity_delivery SET not_before=clock_timestamp() WHERE registration_id=$1`, racing); err != nil {
		t.Fatal(err)
	}
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatalf("a repeated transient conflict stopped the worker: %v", err)
	}
	if n := scalar(t, h, `SELECT count(*) FROM live_activity_delivery WHERE registration_id=$1 AND kind='feed' AND last_reason='transient_conflict' AND lease_id IS NULL AND not_before>clock_timestamp()+interval '3 seconds' AND not_before<=clock_timestamp()+interval '5 seconds'`, racing); n != 1 {
		t.Fatal("a repeated conflict did not back off further")
	}
	// Once the race is over, the job delivers and the losses are forgotten.
	if _, err := h.Migrator.Exec(t.Context(), `UPDATE live_activity_delivery SET not_before=clock_timestamp() WHERE registration_id=$1`, racing); err != nil {
		t.Fatal(err)
	}
	w = NewWorker(NewQueue(h.LiveActivity), snapshot, transport)
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[1].Token != "0001" {
		t.Fatalf("the racing job did not deliver after the conflict: %s", describePushes(sent))
	}
}

// Two runners must drain a backlog in one pass, not take one job per runner
// per one-second poll. The runners' tickers are replaced by ones that never
// fire, so every send observed can only come from the first pass.
func TestLiveActivityRunDrainsABacklogWithinOnePoll(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	h.StartFeed(t, h.Owner, testkit.TimerA)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	const backlog = 4
	registerCaregiverDevices(t, h, service, backlog)
	guard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	arrived := make(chan struct{}, backlog)
	w := NewWorker(NewQueue(h.LiveActivity), guard, transportFunc(func(context.Context, PushRequest) (PushResult, error) {
		arrived <- struct{}{}
		return PushResult{HTTPStatus: 200, Classification: Accepted}, nil
	}))
	var ticks atomic.Int32
	w.ticker = func(time.Duration) (<-chan time.Time, func()) {
		ticks.Add(1)
		return make(chan time.Time), func() {}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	for i := 0; i < backlog; i++ {
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d queued starts were sent in the first pass", i, backlog)
		}
	}
	// Two runner polls and the maintenance tick, none of which ever fired.
	if got := ticks.Load(); got != 3 {
		t.Fatalf("tickers=%d", got)
	}
}

// Start, end and every control-changing update go at priority 10; a cosmetic
// edit in the same phase goes at 5, on the wire.
func TestLiveActivityPriorityFollowsControlChanges(t *testing.T) {
	h := testkit.New(t)
	baby := seedBaby(t, h)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var wire []string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		wire = append(wire, req.Header.Get("apns-priority"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	transport := newAPNSTransport("TEAM123456", "KEY1234567", "com.example.baby", testAPNSKey(t), server.Client(), server.URL, server.URL)
	var events []string
	guard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	w := NewWorker(NewQueue(h.LiveActivity), guard, transportFunc(func(ctx context.Context, r PushRequest) (PushResult, error) {
		events = append(events, r.Event+"/"+r.Content.Phase)
		return transport.Send(ctx, r)
	}))
	step := func() {
		t.Helper()
		nextWorkerSecond()
		if err := w.RunOnce(t.Context(), 32); err != nil {
			t.Fatal(err)
		}
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	step()
	var generation string
	if err := h.Migrator.QueryRow(t.Context(), `SELECT target_state->>'generation_id' FROM live_activity_delivery WHERE kind='feed'`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	req := activityRequest(h, baby, "card")
	req.KnownGenerationID = &generation
	if _, err := service.RegisterActivity(t.Context(), h.Caregiver, req); err != nil {
		t.Fatal(err)
	}
	step() // confirmation of the pushed card
	h.RenameBaby(t, baby, "renamed")
	step() // cosmetic: same phase and kind
	workerStopFeed(t, h, baby, testkit.TimerA)
	h.ExecFamily(t, `UPDATE feed_entries SET started_at=clock_timestamp()-interval '20 minutes',ended_at=clock_timestamp()-interval '10 minutes' WHERE id='88888888-8888-4888-8888-888888888888'`)
	step() // running -> feed break
	mu.Lock()
	got := strings.Join(wire, ",")
	mu.Unlock()
	if want := "10,10,5,10"; got != want {
		t.Fatalf("apns-priority=%s want %s for %v", got, want, events)
	}
}
