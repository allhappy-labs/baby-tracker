package liveactivity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/protocol"
	"babytracker/server/internal/security"
	"babytracker/server/internal/testkit"
)

// workerClock is the worker's wall clock under test control. Advancing it by a
// second between passes replaces sleeping until the next wall-clock second: the
// reconciler never sends twice within one APNs timestamp second, and retry
// deadlines are measured from this clock. It only ever moves forward from the
// real time it started at, so database deadlines written with clock_timestamp()
// (leases, dirty not_before) stay consistent with it.
type workerClock struct {
	mu  sync.Mutex
	now time.Time
}

func newWorkerClock() *workerClock { return &workerClock{now: time.Now()} }

func (c *workerClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *workerClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// A phone-reported ended or dismissed card from an older lifecycle that still
// names the desired feed session (the swiped, timer-less break card) is never
// adopted, whatever its position in the registration order: only a live card
// with a token can establish a new lifecycle's generation.
func TestLiveActivityAdoptOwnCardRequiresALiveToken(t *testing.T) {
	session := "88888888-8888-4888-8888-888888888888"
	third := "57000000-0000-4000-8000-000000000003"
	desired := lifecycleCard("feed", third, session)
	state := TargetState{GenerationID: "g2", Status: "terminal", Kind: "feed"}
	fresh := tokenRow{id: "c-part3", generation: "g3-local", status: "active", token: ptr("ab03"), timer: &third, session: &session}
	for _, status := range []string{"ended", "dismissed"} {
		stale := tokenRow{id: "a-break", generation: "g1", status: status, session: &session}
		for _, rows := range [][]tokenRow{{stale, fresh}, {fresh, stale}} {
			if adopted := adoptOwnCard(state, desired, "feed", rows); adopted == nil || adopted.id != "c-part3" {
				t.Fatalf("%s stale card adopted over the live one: %+v", status, adopted)
			}
		}
		if adopted := adoptOwnCard(state, desired, "feed", []tokenRow{stale}); adopted != nil {
			t.Fatalf("%s stale card adopted with no live card: %+v", status, adopted)
		}
	}
	tokenless := fresh
	tokenless.token = nil
	if adopted := adoptOwnCard(state, desired, "feed", []tokenRow{tokenless}); adopted != nil {
		t.Fatalf("active card without a token adopted: %+v", adopted)
	}
}

// The parent swipes the phone's own feed card away during a break. The same
// session is resumed twice more (two further parts, each ending the previous
// lifecycle). The swiped card still names that session and sorts before the
// phone's new card; it must not be adopted, or the new card is frozen until
// the next lifecycle.
func TestLiveActivitySwipedBreakCardIsNotAdoptedTwoLifecyclesLater(t *testing.T) {
	const record = "88888888-8888-4888-8888-888888888888"
	const second, third = "57000000-0000-4000-8000-000000000002", "57000000-0000-4000-8000-000000000003"
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
	clock := newWorkerClock()
	w.Now = clock.Now
	step := func() {
		t.Helper()
		clock.Advance(time.Second)
		if err := w.RunOnce(t.Context(), 32); err != nil {
			t.Fatal(err)
		}
	}
	ownCard := func(id string, version uint64, token string, timer, session *string, state string) {
		t.Helper()
		req := activityRequest(h, baby, id)
		req.TokenVersion, req.Token, req.TimerID, req.SessionID, req.State = version, token, timer, session, state
		if _, err := service.RegisterActivity(t.Context(), h.Owner, req); err != nil {
			t.Fatal(err)
		}
	}
	startPart := func(timer, side, at string) {
		t.Helper()
		m := testkit.MustMutation(t, "timer-feed.json")
		p := m.Payload.(protocol.TimerStartPayload)
		p.StartedAt, p.CurrentSide, p.CurrentSideStartedAt, p.SessionID = at, ptr(side), ptr(at), ptr(record)
		m.MutationID, m.EntityID, m.ClientMutationTime, m.Payload = h.CommandID(), timer, at, p
		applyMutation(t, h, h.Owner, m)
	}
	stopPart := func(timer, completed, at string) {
		t.Helper()
		m := testkit.MustMutation(t, "timer-feed.json")
		m.MutationID, m.EntityID, m.Operation = h.CommandID(), timer, "timer_stop"
		m.Payload = protocol.TimerStopPayload{BabyID: baby, CompletedRecordID: completed, StoppedAt: at, ComponentID: "4" + completed[1:]}
		applyMutation(t, h, h.Owner, m)
	}
	// Part one: the phone starts the feed and registers its own card bound to
	// the timer. Its own start is never pushed to it; the card is adopted.
	h.StartFeed(t, h.Owner, testkit.TimerA)
	ownCard("a-own", 1, "ab01", ptr(testkit.TimerA), nil, "active")
	step()
	if len(sent) != 1 || sent[0].Event != "update" || sent[0].Token != "ab01" {
		t.Fatalf("part one adoption: %s", describePushes(sent))
	}
	first := sent[0].Content.Remote.GenerationID
	// The part stops into a record; the break is live. The phone's card now
	// names the session, not the timer, and the parent swipes it away.
	workerStopFeed(t, h, baby, testkit.TimerA)
	h.ExecFamily(t, `UPDATE feed_entries SET started_at=clock_timestamp()-interval '20 minutes',ended_at=clock_timestamp()-interval '10 minutes' WHERE id=$1`, record)
	step()
	if len(sent) != 2 || sent[1].Event != "update" || sent[1].Content.Phase != "feedBreak" {
		t.Fatalf("break on the own card: %s", describePushes(sent))
	}
	ownCard("a-own", 2, "ab01", nil, ptr(record), "active")
	ownCard("a-own", 3, "ab01", nil, ptr(record), "ended")
	step()
	// The break's thirty minutes pass (the job is due again at its deadline):
	// the first lifecycle is over without a further send.
	clock.Advance(21 * time.Minute)
	step()
	if len(sent) != 2 {
		t.Fatalf("swiped card received sends: %s", describePushes(sent))
	}
	if n := scalar(t, h, `SELECT count(*) FROM live_activity_delivery WHERE kind='feed' AND target_state->>'status'='terminal'`); n != 1 {
		t.Fatalf("first lifecycle not terminal after the break expired: %d", n)
	}
	// Part two of the same session, with its own new card, then its stop.
	startPart(second, "right", "2026-09-20T15:20:00Z")
	ownCard("b-own", 1, "ab02", ptr(second), ptr(record), "active")
	step()
	if len(sent) != 3 || sent[2].Event != "update" || sent[2].Token != "ab02" || sent[2].Content.Remote.GenerationID == first {
		t.Fatalf("part two adoption: %s", describePushes(sent))
	}
	stopPart(second, "88888888-8888-4888-8888-888888888802", "2026-09-20T15:25:00Z")
	step()
	if len(sent) != 4 || sent[3].Event != "end" || sent[3].Token != "ab02" {
		t.Fatalf("part two end: %s", describePushes(sent))
	}
	// Part three: the swiped card from part one still names this session and
	// sorts first. The phone's new card is the one to adopt.
	startPart(third, "left", "2026-09-20T15:30:00Z")
	ownCard("c-own", 1, "ab03", ptr(third), ptr(record), "active")
	// Two passes on purpose: the first adopts the new card; the second must
	// change nothing (no extra send, no late adoption of the swiped row), which
	// the single count below asserts for both.
	step()
	step()
	if len(sent) != 5 || sent[4].Event != "update" || sent[4].Token != "ab03" || sent[4].Content.Remote.GenerationID == first {
		t.Fatalf("part three adopted the swiped card instead of the new one: %s", describePushes(sent))
	}
	for _, s := range sent {
		if s.Event == "start" {
			t.Fatalf("the originating phone was pushed a start: %s", describePushes(sent))
		}
	}
}

// Two cards of one phone register against the same generation. The server's
// survivor must be the one the phone itself keeps — the smallest activity ID
// by Swift's String comparison, byte order — so the server's duplicate end
// never targets the phone's winner. Mixed case is where a locale collation
// would disagree with byte order.
func TestLiveActivityDuplicateSurvivorFollowsByteOrder(t *testing.T) {
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
	clock := newWorkerClock()
	w.Now = clock.Now
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	generation := sent[0].Content.Remote.GenerationID
	// "B-card" < "a-card" in byte order; a case-folding locale orders them the
	// other way round.
	for id, token := range map[string]string{"a-card": "ccdd", "B-card": "eeff"} {
		req := activityRequest(h, baby, id)
		req.Token = token
		req.KnownGenerationID = &generation
		if _, err := service.RegisterActivity(t.Context(), h.Caregiver, req); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(time.Second)
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[1].Event != "end" || sent[1].Token != "ccdd" {
		t.Fatalf("duplicate end must target a-card (the byte-order loser): %s", describePushes(sent))
	}
	if workerTokenCount(t, h, `SELECT count(*) FROM live_activity_tokens WHERE activity_id='B-card' AND state='active'`) != 1 || workerTokenCount(t, h, `SELECT count(*) FROM live_activity_tokens WHERE activity_id='a-card' AND state='ended' AND server_retired`) != 1 {
		t.Fatal("survivor is not the byte-order winner")
	}
	clock.Advance(time.Second)
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 3 || sent[2].Event != "update" || sent[2].Token != "eeff" {
		t.Fatalf("survivor not updated: %s", describePushes(sent))
	}
}

// A stale lease the worker cannot release is logged with a safe code, and the
// worker carries on; nothing of the failure's message reaches the log.
func TestLiveActivityStaleLeaseReleaseFailureIsLogged(t *testing.T) {
	h := testkit.New(t)
	baby := seedBaby(t, h)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	realGuard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	guard := &interleavedSnapshotGuard{SnapshotGuard: realGuard}
	logs := captureDefaultLog(t)
	var sent []PushRequest
	w := NewWorker(NewQueue(h.LiveActivity), guard, transportFunc(func(_ context.Context, r PushRequest) (PushResult, error) {
		sent = append(sent, r)
		return PushResult{HTTPStatus: 200, Classification: Accepted}, nil
	}))
	clock := newWorkerClock()
	w.Now = clock.Now
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0].Event != "start" {
		t.Fatalf("start: %s", describePushes(sent))
	}
	req := activityRequest(h, baby, "card")
	req.KnownGenerationID = &sent[0].Content.Remote.GenerationID
	if _, err := service.RegisterActivity(t.Context(), h.Caregiver, req); err != nil {
		t.Fatal(err)
	}
	// The worker may not release leases any more: an infrastructure fault
	// confined to that one function.
	if _, err := h.Migrator.Exec(t.Context(), `REVOKE EXECUTE ON FUNCTION app.release_stale_live_activity_lease(jsonb) FROM baby_tracker_live_activity`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := h.Migrator.Exec(context.Background(), `GRANT EXECUTE ON FUNCTION app.release_stale_live_activity_lease(jsonb) TO baby_tracker_live_activity`); err != nil {
			t.Error(err)
		}
	})
	// Newer family work lands between the confirmation update's two snapshots,
	// so its acknowledgement is stale and the lease is due for release.
	guard.feedReads = 0
	guard.beforeSecond = func() { h.RenameBaby(t, baby, "renamed between snapshots") }
	clock.Advance(time.Second)
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatalf("a failed lease release stopped the worker: %v", err)
	}
	if len(sent) != 1 {
		t.Fatalf("stale snapshot reached transport: %s", describePushes(sent))
	}
	out := logs.String()
	if !strings.Contains(out, "live activity stale lease not released") || !strings.Contains(out, "code=sqlstate_42501") {
		t.Fatalf("release failure not logged with its code: %s", out)
	}
	if strings.Contains(out, "permission denied") || strings.Contains(out, h.FamilyID) || strings.Contains(out, "ccdd") {
		t.Fatalf("log carries a message or identifier: %s", out)
	}
}

// A request the transport rejects before writing it never reached Apple. Such
// an error wraps ErrRequestNotSent; one Apple answered, however it answered,
// does not.
func TestLiveActivityAPNsPreflightRejectionIsNotSent(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"reason":"InvalidProviderToken"}`)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	transport := newAPNSTransport("TEAM123456", "KEY1234567", "com.example.baby", testAPNSKey(t), server.Client(), server.URL, server.URL)
	invalid := testPushRequest("start", "bogus")
	result, err := transport.Send(t.Context(), invalid)
	if result.Classification != ConfigurationFailure || !errors.Is(err, ErrRequestNotSent) || requests.Load() != 0 {
		t.Fatalf("preflight rejection result=%+v err=%v requests=%d", result, err, requests.Load())
	}
	oversized := testPushRequest("start", "sandbox")
	oversized.Attributes.BabyName = strings.Repeat("n", 4096)
	result, err = transport.Send(t.Context(), oversized)
	if result.Classification != ConfigurationFailure || !errors.Is(err, ErrRequestNotSent) || requests.Load() != 0 {
		t.Fatalf("payload rejection result=%+v err=%v requests=%d", result, err, requests.Load())
	}
	result, err = transport.Send(t.Context(), testPushRequest("start", "sandbox"))
	if err != nil || result.Classification != ConfigurationFailure || result.HTTPStatus != 403 || requests.Load() != 1 {
		t.Fatalf("Apple's 403 result=%+v err=%v requests=%d", result, err, requests.Load())
	}
}

// A start rejected before any network I/O is known-unsent: the intent is
// released for a later attempt (with backoff) instead of staying
// start-unconfirmed, and the outcome is still recorded. A start that may have
// reached Apple keeps the ambiguity rules: no blind resend.
func TestLiveActivityUnsentStartIsReleasedForRetry(t *testing.T) {
	for _, reached := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejected_before_io", true: "answered_by_apple"}[reached], func(t *testing.T) {
			h := testkit.New(t)
			seedBaby(t, h)
			service := NewService(h.Security, familyResolver{h.Family}, topic)
			if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
				t.Fatal(err)
			}
			h.StartFeed(t, h.Owner, testkit.TimerA)
			guard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
			logs := captureDefaultLog(t)
			var sent []PushRequest
			w := NewWorker(NewQueue(h.LiveActivity), guard, transportFunc(func(_ context.Context, r PushRequest) (PushResult, error) {
				sent = append(sent, r)
				if len(sent) > 1 {
					return PushResult{HTTPStatus: 200, Classification: Accepted}, nil
				}
				if reached {
					return PushResult{HTTPStatus: 403, Reason: "InvalidProviderToken", Classification: ConfigurationFailure}, nil
				}
				return PushResult{Classification: ConfigurationFailure}, errors.Join(ErrRequestNotSent, errors.New("invalid live activity push request"))
			}))
			clock := newWorkerClock()
			w.Now = clock.Now
			w.Jitter = func(d time.Duration) time.Duration { return d }
			if err := w.RunOnce(t.Context(), 32); err != nil {
				t.Fatal(err)
			}
			if len(sent) != 1 || sent[0].Event != "start" {
				t.Fatalf("first attempt: %s", describePushes(sent))
			}
			var status, pending, startAttempt, lastReason string
			var retryAt *time.Time
			read := func() {
				t.Helper()
				if err := h.Migrator.QueryRow(t.Context(), `SELECT coalesce(target_state->>'status',''),coalesce(target_state->>'pending_event',''),coalesce(target_state->>'start_attempt_id',''),(target_state->>'retry_at')::timestamptz,coalesce(last_reason,'') FROM live_activity_delivery WHERE kind='feed'`).Scan(&status, &pending, &startAttempt, &retryAt, &lastReason); err != nil {
					t.Fatal(err)
				}
			}
			read()
			if reached {
				if status != "startUnconfirmed" || startAttempt != sent[0].APNSID {
					t.Fatalf("answered start lost its ambiguity: status=%s start_attempt=%s", status, startAttempt)
				}
				clock.Advance(2 * time.Minute)
				if err := w.RunOnce(t.Context(), 32); err != nil {
					t.Fatal(err)
				}
				if len(sent) != 1 {
					t.Fatalf("ambiguous start was resent blind: %s", describePushes(sent))
				}
				if got := historyLines(t, h); len(got) != 1 || got[0] != "start configurationFailure 403 InvalidProviderToken" {
					t.Fatalf("history=%q", got)
				}
				return
			}
			if status != "pending" || pending != "" || startAttempt != "" || retryAt == nil || !retryAt.After(clock.Now()) || lastReason != "configurationFailure" {
				t.Fatalf("unsent start not released: status=%s pending=%q start_attempt=%q retry_at=%v last_reason=%s", status, pending, startAttempt, retryAt, lastReason)
			}
			if got := historyLines(t, h); len(got) != 1 || got[0] != "start configurationFailure - -" {
				t.Fatalf("history=%q", got)
			}
			if out := logs.String(); !strings.Contains(out, "live activity push not accepted") || !strings.Contains(out, "class=configurationFailure") || strings.Contains(out, "invalid live activity push request") {
				t.Fatalf("logs=%s", out)
			}
			// Not before its backoff.
			clock.Advance(time.Second)
			if err := w.RunOnce(t.Context(), 32); err != nil {
				t.Fatal(err)
			}
			if len(sent) != 1 {
				t.Fatalf("released start retried before its backoff: %s", describePushes(sent))
			}
			// At the backoff, a fresh attempt with its own nonce.
			clock.Advance(2 * time.Second)
			if err := w.RunOnce(t.Context(), 32); err != nil {
				t.Fatal(err)
			}
			if len(sent) != 2 || sent[1].Event != "start" || sent[1].APNSID == sent[0].APNSID || sent[1].Content.Remote.GenerationID != sent[0].Content.Remote.GenerationID {
				t.Fatalf("released start not retried as a new attempt: %s", describePushes(sent))
			}
			read()
			if status != "startUnconfirmed" || startAttempt != sent[1].APNSID {
				t.Fatalf("retried start: status=%s start_attempt=%s", status, startAttempt)
			}
			if got := historyLines(t, h); len(got) != 2 || got[1] != "start accepted 200 -" {
				t.Fatalf("history=%q", got)
			}
		})
	}
}
