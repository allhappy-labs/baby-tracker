package loadtest

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"babytracker/server/internal/protocol"
)

func TestExpiredAccessIsRefreshedNeverRetriedBlind(t *testing.T) {
	// The fake answers 401 access_expired to a sync. The generator records one
	// unexpected outcome ("access_expired": the schedule should have refreshed
	// first), refreshes through refresh-recoverable and ack, and does not
	// resend the failed sync.
	f := newFakeV1(t)
	f.hook("POST /v1/sync", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		writeErrorResponse(w, http.StatusUnauthorized, "access_expired")
		return true
	})
	g := testGenerator(t, f, 1)
	d := g.devices[0]
	g.pullPages(t.Context(), d, time.Now(), 0, RouteSyncPull)
	if got := g.outcomes(RouteSyncPull); got[OutcomeAccessExpired] != 1 || len(got) != 1 {
		t.Fatalf("pull outcomes %v, want one access_expired", got)
	}
	if f.count("POST /v1/sync") != 1 {
		t.Fatalf("the failed sync was sent %d times", f.count("POST /v1/sync"))
	}
	if f.count("POST /v1/auth/refresh-recoverable") != 1 || f.count("POST /v1/auth/refresh-ack") != 1 {
		t.Fatalf("refresh %d, ack %d", f.count("POST /v1/auth/refresh-recoverable"), f.count("POST /v1/auth/refresh-ack"))
	}
	if d.access() != "access-child-1" {
		t.Fatalf("access %q: not the refreshed token", d.access())
	}
	v := Evaluate(g.summary(g.phases.measure), SpecThresholds())
	if !slices.ContainsFunc(v.Reasons, func(r string) bool { return strings.HasPrefix(r, "unexpected ") }) {
		t.Fatalf("access_expired is not unexpected: %v", v.Reasons)
	}
}

func TestRecoverableRefreshRetriesALostAnswerWithTheSameAttempt(t *testing.T) {
	// The fake commits the refresh but drops the first response (closes the
	// connection). The device keeps PendingAttemptID, retries with the same
	// attempt ID and the old parent, receives the committed child and acks it.
	// The fleet then holds the child's tokens and no PendingAttemptID.
	f := newFakeV1(t)
	var dropped atomic.Bool
	f.hook("POST /v1/auth/refresh-recoverable", func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if dropped.Swap(true) {
			return false
		}
		f.refresh(httptestDiscard{}, body) // commit the child
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
		return true
	})
	g := testGenerator(t, f, 1)
	d := g.devices[0]
	parent := d.RefreshToken
	if !g.refresh(t.Context(), d, time.Now()) {
		t.Fatal("refresh failed")
	}
	sent := f.all("/v1/auth/refresh-recoverable")
	if len(sent) != 2 {
		t.Fatalf("%d refresh requests, want 2", len(sent))
	}
	var first, second protocol.RecoverableRefreshRequest
	_ = json.Unmarshal(sent[0].Body, &first)
	_ = json.Unmarshal(sent[1].Body, &second)
	if first.RefreshAttemptID == "" || second.RefreshAttemptID != first.RefreshAttemptID || second.RefreshToken != parent {
		t.Fatalf("retry attempt %q parent %q; first %q", second.RefreshAttemptID, second.RefreshToken, first.RefreshAttemptID)
	}
	var ack protocol.RefreshAckRequest
	acks := f.all("/v1/auth/refresh-ack")
	if len(acks) != 1 || json.Unmarshal(acks[0].Body, &ack) != nil || ack.RefreshAttemptID != first.RefreshAttemptID {
		t.Fatalf("acks %d attempt %q", len(acks), ack.RefreshAttemptID)
	}
	if got := g.outcomes(RouteRefresh); got[OutcomeTransport] != 1 || got[OutcomeOK] != 1 {
		t.Fatalf("refresh outcomes %v", got)
	}
	if err := g.checkpoint(); err != nil {
		t.Fatal(err)
	}
	_, states, err := ReadFleet(g.cfg.Fleet)
	if err != nil {
		t.Fatal(err)
	}
	if s := states[0]; s.AccessToken != "access-child-1" || s.RefreshToken != f.children[first.RefreshAttemptID].refresh || s.PendingAttemptID != "" {
		t.Fatalf("fleet holds access %q refresh %q pending %q", s.AccessToken, s.RefreshToken, s.PendingAttemptID)
	}
}

// A refresh whose every answer is lost keeps its attempt in the fleet, so
// the next run retries it with the same attempt and parent.
func TestAnUnansweredRefreshKeepsItsAttemptInTheFleet(t *testing.T) {
	f := newFakeV1(t)
	f.hook("POST /v1/auth/refresh-recoverable", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		writeErrorResponse(w, http.StatusServiceUnavailable, "server_busy")
		return true
	})
	g := testGenerator(t, f, 1)
	d := g.devices[0]
	if g.refresh(t.Context(), d, time.Now()) {
		t.Fatal("refresh succeeded against a busy server")
	}
	if n := f.count("POST /v1/auth/refresh-recoverable"); n != 1+refreshRetries {
		t.Fatalf("%d attempts", n)
	}
	if d.state().PendingAttemptID == "" || d.state().RefreshToken != "r"+strings.Repeat("0", 42) {
		t.Fatalf("state %+v", d.state())
	}
}

type httptestDiscard struct{}

func (httptestDiscard) Header() http.Header         { return http.Header{} }
func (httptestDiscard) Write(b []byte) (int, error) { return len(b), nil }
func (httptestDiscard) WriteHeader(int)             {}

func TestPullFollowsPagesAndRejectsAnotherFamilysEntity(t *testing.T) {
	// The fake returns two pages; the second contains an entity whose
	// meta.family_id is another family. The pull records outcome
	// "cross_family", and both pages are timed in sync_pull: for a tracked
	// device (the full decode) and an untracked one (pullPage) alike.
	t.Run("tracked", func(t *testing.T) { testPullFollowsPagesAndRejectsAnotherFamilysEntity(t, true) })
	t.Run("untracked", func(t *testing.T) { testPullFollowsPagesAndRejectsAnotherFamilysEntity(t, false) })
}

func testPullFollowsPagesAndRejectsAnotherFamilysEntity(t *testing.T, tracked bool) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	d := g.devices[0]
	if tracked {
		d.tracked = newTrackedState()
	}
	other := "f9999999-0000-4000-8000-000000000000"
	token := "page-2"
	var pages atomic.Int32
	f.hook("POST /v1/sync", func(w http.ResponseWriter, _ *http.Request, body []byte) bool {
		var request protocol.SyncRequest
		_ = json.Unmarshal(body, &request)
		page := pages.Add(1)
		family := d.FamilyID
		next := &token
		if page == 2 {
			if request.PageToken == nil || *request.PageToken != token || request.Cursor != nil {
				t.Errorf("page 2 request: token %v cursor %v", request.PageToken, request.Cursor)
			}
			family, next = other, nil
		}
		entity := fakeOutcome(f.instance, family, mustMutation(newUUID(), "diaper", "create", 0, time.Now(),
			map[string]any{"baby_id": d.BabyIDs[0], "started_at": stamp(time.Now()), "ended_at": nil, "time_zone_id": loadTimeZone,
				"note": nil, "contents": "mixed", "consistency": "soft", "color": "mustard"}), uint64(page)).CanonicalEntity
		change := protocol.JournalChange{Cursor: g.cursor(family, uint64(page)), EntityKind: "diaper", EntityID: entity.Meta.ID,
			EntityVersion: 1, ActorAccountID: d.AccountID, DeviceID: d.DeviceID, Operation: "create", AcceptedAt: entity.Meta.AcceptedAt,
			SnapshotOrTombstone: *entity}
		writeJSONResponse(w, http.StatusOK, protocol.SyncResponse{ServerInstanceID: f.instance, RecoveryEpoch: 1, Outcomes: []protocol.Outcome{},
			Changes: []protocol.JournalChange{change}, HighWater: g.cursor(d.FamilyID, 2), NextPageToken: next})
		return true
	})
	if g.pullPages(t.Context(), d, time.Now(), 0, RouteSyncPull) {
		t.Fatal("a pull with another family's entity completed")
	}
	routes := g.phases.measure.Snapshot()
	if got := routes[RouteSyncPull]; got.Outcomes[OutcomeOK] != 1 || got.Outcomes[OutcomeCrossFamily] != 1 || got.Latency.Total != 2 {
		t.Fatalf("sync_pull %v, %d timed", got.Outcomes, got.Latency.Total)
	}
	if d.state().Cursor != 0 {
		t.Fatalf("the cursor advanced to %d on an incomplete pull", d.state().Cursor)
	}
	if tracked && len(d.tracked.entities) != 1 {
		t.Fatalf("tracked map holds %d entities: another family's entity was applied", len(d.tracked.entities))
	}
	// A clean two-page pull advances the cursor from its final page only.
	pages.Store(0)
	other = d.FamilyID
	if !g.pullPages(t.Context(), d, time.Now(), 0, RouteSyncPull) || d.state().Cursor != 2 {
		t.Fatalf("cursor %d after a complete pull", d.state().Cursor)
	}
}

// A snapshot page holding another family's entity is a cross_family
// snapshot_page and ends the snapshot, for a tracked device (the full
// decode) and an untracked one (snapshotPage) alike; a clean snapshot pages
// to its end and pulls from its high water.
func TestSnapshotRejectsAnotherFamilysEntity(t *testing.T) {
	for _, tracked := range []bool{true, false} {
		t.Run(map[bool]string{true: "tracked", false: "untracked"}[tracked], func(t *testing.T) {
			f := newFakeV1(t)
			g := testGenerator(t, f, 1)
			d := g.devices[0]
			if tracked {
				d.tracked = newTrackedState()
			}
			var family atomic.Value
			family.Store("f9999999-0000-4000-8000-000000000000")
			f.hook("GET /v1/sync/snapshot", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
				entity := fakeOutcome(f.instance, family.Load().(string), mustMutation(newUUID(), "diaper", "create", 0, time.Now(),
					map[string]any{"baby_id": d.BabyIDs[0], "started_at": stamp(time.Now()), "ended_at": nil, "time_zone_id": loadTimeZone,
						"note": nil, "contents": "mixed", "consistency": "soft", "color": "mustard"}), 1).CanonicalEntity
				writeJSONResponse(w, http.StatusOK, protocol.SnapshotResponse{ServerInstanceID: f.instance, RecoveryEpoch: 1, SnapshotID: newUUID(),
					HighWater: g.cursor(d.FamilyID, 1), Entities: []protocol.CanonicalEntity{*entity}})
				return true
			})
			if g.snapshotAndPull(t.Context(), d, time.Now(), RouteSyncPull) {
				t.Fatal("a snapshot with another family's entity completed")
			}
			if got := g.outcomes(RouteSnapshot); got[OutcomeCrossFamily] != 1 || len(got) != 1 {
				t.Fatalf("snapshot_page %v", got)
			}
			if tracked && len(d.tracked.entities) != 0 {
				t.Fatalf("tracked map holds %d entities: another family's entity was applied", len(d.tracked.entities))
			}
			family.Store(d.FamilyID)
			if !g.snapshotAndPull(t.Context(), d, time.Now(), RouteSyncPull) {
				t.Fatalf("a clean snapshot did not complete: %v %v", g.outcomes(RouteSnapshot), g.outcomes(RouteSyncPull))
			}
			if tracked && len(d.tracked.entities) != 1 {
				t.Fatalf("tracked map holds %d entities after a clean snapshot", len(d.tracked.entities))
			}
		})
	}
}

func TestPullRewindsTheCursorByTheDrawnLag(t *testing.T) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	d := g.devices[0]
	d.Cursor = 40
	for _, tc := range []struct{ rewind, want uint64 }{{0, 40}, {10, 30}, {50, 0}, {rewindToZero, 0}} {
		g.pullPages(t.Context(), d, time.Now(), tc.rewind, RouteSyncPull)
		var request protocol.SyncRequest
		_ = json.Unmarshal(f.last().Body, &request)
		if request.Cursor == nil || request.Cursor.Sequence != tc.want || request.Cursor.FamilyID != d.FamilyID {
			t.Fatalf("rewind %d: cursor %+v, want %d", tc.rewind, request.Cursor, tc.want)
		}
		d.Cursor = 40
	}
	mix := map[bool]int{}
	for range 10000 {
		mix[g.drawLag() == 0]++
	}
	if at := float64(mix[true]) / 10000; at < 0.78 || at > 0.82 {
		t.Fatalf("%.3f of pulls at head, want 0.80", at)
	}
}

func TestWritesLedgerEveryAppliedOutcome(t *testing.T) {
	// A write of 2 mutations answered "applied" with canonical entities appends
	// 2 ledger entries, with WireSHA256 = sha256(json.Marshal(entity)); a
	// "rejected" outcome is unexpected and is not ledgered.
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	d := g.writers[0]
	var answered []protocol.CanonicalEntity
	reject := false
	f.hook("POST /v1/sync", func(w http.ResponseWriter, _ *http.Request, body []byte) bool {
		var request struct {
			Mutations []json.RawMessage `json:"mutations"`
		}
		_ = json.Unmarshal(body, &request)
		var outcomes []protocol.Outcome
		for i, raw := range request.Mutations {
			m, err := protocol.DecodeMutation(raw)
			if err != nil {
				t.Errorf("invalid mutation: %v", err)
			}
			o := fakeOutcome(f.instance, d.FamilyID, m, uint64(i+1))
			if reject && i == 1 {
				code := "invalid_timestamp"
				o = protocol.Outcome{MutationID: m.MutationID, Status: "rejected", Code: &code, RelatedEntities: []protocol.CanonicalEntity{}}
			} else {
				answered = append(answered, *o.CanonicalEntity)
			}
			outcomes = append(outcomes, o)
		}
		writeJSONResponse(w, http.StatusOK, protocol.SyncResponse{ServerInstanceID: f.instance, RecoveryEpoch: 1, Outcomes: outcomes,
			Changes: []protocol.JournalChange{}, HighWater: g.cursor(d.FamilyID, 2)})
		return true
	})
	g.write(t.Context(), d, time.Now(), RouteSyncWrite, 2)
	reject = true
	g.write(t.Context(), d, time.Now(), RouteSyncWrite, 2)
	if err := g.ledger.Close(); err != nil {
		t.Fatal(err)
	}
	ledger, err := ReadLedger(g.cfg.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	if len(answered) != 3 || len(ledger) != 3 || g.ledgered.Load() != 3 {
		t.Fatalf("answered %d, ledgered %d (%d lines)", len(answered), len(ledger), g.ledgered.Load())
	}
	byID := map[string]LedgerEntry{}
	for _, e := range ledger {
		byID[e.EntityID] = e
	}
	for _, entity := range answered {
		raw, _ := json.Marshal(entity)
		want, _ := entityWireSHA256(entity)
		e := byID[entity.Meta.ID]
		if e.WireSHA256 != want || e.FamilyID != d.FamilyID || e.Kind != entity.EntityKind || e.Version != entity.Meta.EntityVersion || len(raw) == 0 {
			t.Fatalf("ledger entry %+v for %s", e, raw)
		}
	}
	if got := g.outcomes(RouteSyncWrite); got[OutcomeOK] != 1 || got[OutcomeRejected] != 1 {
		t.Fatalf("write outcomes %v", got)
	}
	if d.state().Cursor != 2 {
		t.Fatalf("cursor %d", d.state().Cursor)
	}
}

func TestFailedWriteIsRetriedOnceWithTheSameMutations(t *testing.T) {
	// The server applies the first request but its answer is lost: the
	// retry, a second later, carries the same body and is answered
	// already_applied; each entity is ledgered once.
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	d := g.writers[0]
	var lost atomic.Bool
	f.hook("POST /v1/sync", func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if lost.Swap(true) {
			return false
		}
		f.sync(httptestDiscard{}, body)
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
		return true
	})
	started := time.Now()
	g.write(t.Context(), d, started, RouteSyncWrite, 3)
	sent := f.all("/v1/sync")
	if len(sent) != 2 || string(sent[0].Body) != string(sent[1].Body) {
		t.Fatalf("%d requests; bodies equal: %v", len(sent), len(sent) == 2 && string(sent[0].Body) == string(sent[1].Body))
	}
	if time.Since(started) < time.Second {
		t.Fatal("retried without waiting a second")
	}
	if got := g.outcomes(RouteSyncWrite); got[OutcomeTransport] != 1 || got[OutcomeOK] != 1 {
		t.Fatalf("outcomes %v", got)
	}
	if g.ledgered.Load() != 3 {
		t.Fatalf("ledgered %d, want 3 (once each)", g.ledgered.Load())
	}
	if err := g.ledger.Close(); err != nil {
		t.Fatal(err)
	}
	// The file holds one acknowledgement per entity: the retry's replay is
	// not a second write (Task 11's acknowledged_writes).
	if entries, writes, err := ReadLedgerCounted(g.cfg.Ledger); err != nil || len(entries) != 3 || writes != 3 {
		t.Fatalf("ledger: %d entities, %d writes, %v", len(entries), writes, err)
	}
	g.ledger, _ = CreateLedger(g.cfg.Ledger + ".2")
	// A second failure is not retried again.
	f.hook("POST /v1/sync", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		writeErrorResponse(w, http.StatusInternalServerError, "internal_error")
		return true
	})
	g.write(t.Context(), d, time.Now(), RouteSyncWrite, 1)
	if n := len(f.all("/v1/sync")); n != 4 {
		t.Fatalf("%d requests after a failing write and its one retry", n)
	}
}

func TestLostRaceIsRetriedPerRetryAfterAndExpected(t *testing.T) {
	f := newFakeV1(t)
	var raced atomic.Bool
	f.hook("POST /v1/sync", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		if raced.Swap(true) {
			return false
		}
		w.Header().Set("Retry-After", "1")
		writeErrorResponse(w, http.StatusServiceUnavailable, "security_busy")
		return true
	})
	g := testGenerator(t, f, 1)
	if !g.pullPages(t.Context(), g.devices[0], time.Now(), 0, RouteSyncPull) {
		t.Fatal("the pull did not finish after its lost race")
	}
	if got := g.outcomes(RouteSyncPull); got[OutcomeSecurityBusy] != 1 || got[OutcomeOK] != 1 {
		t.Fatalf("outcomes %v", got)
	}
	s := g.summary(g.phases.measure)
	v := EvaluateWindow(s, SpecThresholds())
	if slices.ContainsFunc(v.Reasons, func(r string) bool { return strings.HasPrefix(r, "unexpected ") }) {
		t.Fatalf("a lost race counted as unexpected: %v", v.Reasons)
	}
}

// Every kind and operation the write mix draws is a mutation the protocol
// decodes, and survives the wire (encode, decode) unchanged.
func TestEveryPlannedMutationDecodes(t *testing.T) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 2)
	d := g.writers[0]
	kinds := map[string]bool{}
	for range 400 {
		g.write(t.Context(), d, time.Now(), RouteSyncWrite, 3)
	}
	for _, r := range f.all("/v1/sync") {
		var request protocol.SyncRequest
		if err := json.Unmarshal(r.Body, &request); err != nil {
			t.Fatalf("request: %v", err)
		}
		for _, m := range request.Mutations {
			kinds[m.EntityKind+" "+m.Operation] = true
			raw, _ := json.Marshal(m)
			again, err := protocol.DecodeMutation(raw)
			if err != nil {
				t.Fatalf("%s %s does not round-trip: %v", m.EntityKind, m.Operation, err)
			}
			if a, _ := json.Marshal(again); string(a) != string(raw) {
				t.Fatalf("%s %s changes on the wire:\n%s\n%s", m.EntityKind, m.Operation, raw, a)
			}
		}
	}
	for _, want := range []string{"feed create", "feed update", "feed delete", "diaper create", "sleep create", "supplement create",
		"measurement create", "immunization create", "pump create", "timer timer_start", "timer timer_stop", "growth_profile update"} {
		if !kinds[want] {
			t.Errorf("never wrote %s (wrote %v)", want, kinds)
		}
	}
	if got := g.outcomes(RouteSyncWrite); got[OutcomeOK] == 0 || len(got) != 1 {
		t.Fatalf("outcomes %v", got)
	}
}

func TestWritesComeOnlyFromOwnersAndCaregivers(t *testing.T) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 4)
	for _, d := range g.writers {
		if d.Role == "viewer" {
			t.Fatalf("viewer %s writes", d.DeviceID)
		}
	}
	if len(g.writers) != 14 {
		t.Fatalf("%d writers of 16 devices, two viewers", len(g.writers))
	}
}

func TestTimerStartSupersededByALiveTimerAdoptsIt(t *testing.T) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	d := g.writers[0]
	existing := "e0000000-0000-4000-8000-000000000001"
	var p *plannedMutation
	for p == nil || p.m.Operation != "timer_start" {
		p = g.planTimer(d)
	}
	out, _ := g.applyOutcomes(d, []*plannedMutation{p}, protocol.SyncResponse{Outcomes: func() []protocol.Outcome {
		live := p.m
		live.EntityID = existing
		o := fakeOutcome(f.instance, d.FamilyID, live, 1)
		o.MutationID, o.Status = p.m.MutationID, "superseded"
		return []protocol.Outcome{o}
	}()})
	if out != OutcomeOK || g.ledgered.Load() != 0 {
		t.Fatalf("outcome %q, ledgered %d", out, g.ledgered.Load())
	}
	fam := g.families[d.FamilyID]
	adopted := false
	for _, ref := range fam.timers {
		adopted = adopted || ref.id == existing && !ref.busy && !ref.pending
	}
	if !adopted {
		t.Fatal("the live timer was not adopted")
	}
	// Its stop is a phone's: a feed's running side is named.
	for {
		p = g.planTimer(d)
		if p != nil && p.m.EntityID == existing {
			break
		}
		if p != nil {
			p.release()
		}
	}
	stop, ok := p.m.Payload.(protocol.TimerStopPayload)
	feed := false
	for key := range fam.timers {
		feed = feed || strings.HasSuffix(key, "/feed") && fam.timers[key].id == existing
	}
	if !ok || feed != (stop.ComponentID != "") || feed != stop.CleanupShortBreastSegments {
		t.Fatalf("stop of the adopted timer (feed %v): %+v", feed, p.m.Payload)
	}
}

func TestLiveSocketReconnectsOnLifetimeCloseWithoutCountingALoss(t *testing.T) {
	// The fake hub closes with 1001 (going away) and, separately, at token
	// expiry; the device reconnects and the loss counter stays 0. A close with
	// 1008 outside a declared window counts one unexpected loss.
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	d := g.devices[0]
	d.live.Store(true)
	if !g.connectLive(t.Context(), d, 0, 0).opened {
		t.Fatal("dial failed")
	}
	waitFor(t, func() bool { return f.openSockets() == 1 })
	for _, close := range []struct {
		code   websocket.StatusCode
		reason string
	}{{closeGoingAway, "server shutdown"}, {closeNormal, "maximum lifetime"}} {
		dials := f.count("GET /v1/live")
		f.closeSocketsWith(close.code, close.reason)
		waitFor(t, func() bool { return f.count("GET /v1/live") > dials && f.openSockets() == 1 })
		if g.liveLosses.Load() != 0 {
			t.Fatalf("close %d counted as a loss", close.code)
		}
	}
	dials := f.count("GET /v1/live")
	f.closeSocketsWith(closePolicyViolation, "authorization changed")
	waitFor(t, func() bool { return g.liveLosses.Load() == 1 })
	waitFor(t, func() bool { return f.count("GET /v1/live") > dials && f.openSockets() == 1 })
	if got := g.outcomes(RouteLive); got[OutcomeOK] != 4 {
		t.Fatalf("handshakes %v", got)
	}
	g.closeSockets()
	waitFor(t, func() bool { return f.openSockets() == 0 })

	// Inside a declared window (a restart), a 1008 is not a loss.
	g2 := testGenerator(t, f, 1)
	g2.phases.restartFrom, g2.phases.restartTo = time.Now().Add(-time.Minute), time.Now().Add(time.Minute)
	d2 := g2.devices[0]
	d2.live.Store(true)
	g2.connectLive(t.Context(), d2, 0, 0)
	waitFor(t, func() bool { return f.openSockets() == 1 })
	dials = f.count("GET /v1/live")
	f.closeSocketsWith(closePolicyViolation, "authorization changed")
	waitFor(t, func() bool { return f.count("GET /v1/live") > dials && f.openSockets() == 1 })
	if g2.liveLosses.Load() != 0 {
		t.Fatalf("a close in a declared window counted: %d", g2.liveLosses.Load())
	}
}

func TestHintSchedulesAnImmediatePullOfTheFamily(t *testing.T) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	d := g.devices[0]
	d.live.Store(true)
	g.connectLive(t.Context(), d, 0, 0)
	waitFor(t, func() bool { return f.openSockets() == 1 })
	f.sendHint(protocol.Hint{FamilyID: d.FamilyID, Cursor: g.cursor(d.FamilyID, 9)})
	waitFor(t, func() bool { return g.outcomes(RouteSyncHintPull)[OutcomeOK] == 1 })
	other := "f9999999-0000-4000-8000-000000000000"
	f.sendHint(protocol.Hint{FamilyID: other, Cursor: g.cursor(other, 9)})
	waitFor(t, func() bool { return g.outcomes(RouteSyncHintPull)[OutcomeCrossFamily] == 1 })
	if n := f.count("POST /v1/sync"); n != 1 {
		t.Fatalf("%d pulls: another family's hint was pulled", n)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
