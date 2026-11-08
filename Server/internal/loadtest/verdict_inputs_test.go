package loadtest

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/protocol"
)

func unexpectedReason(v Verdict) bool {
	return slices.ContainsFunc(v.Reasons, func(r string) bool { return strings.HasPrefix(r, "unexpected ") })
}

// I2: a lost race no retry follows is lost_race_exhausted, unexpected: a
// server that answers security_busy forever cannot pass.
func TestExhaustedLostRaceIsUnexpected(t *testing.T) {
	f := newFakeV1(t)
	busy := func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		w.Header().Set("Retry-After", "1")
		writeErrorResponse(w, http.StatusServiceUnavailable, "security_busy")
		return true
	}
	f.hook("POST /v1/sync", busy)
	f.hook("PUT /v1/devices/push-token", busy)
	g := testGenerator(t, f, 1)
	if g.pullPages(t.Context(), g.devices[0], time.Now(), 0, RouteSyncPull) {
		t.Fatal("a pull completed against a server that is always busy")
	}
	if got := g.outcomes(RouteSyncPull); got[OutcomeSecurityBusy] != refreshRetries || got[OutcomeLostRaceExhausted] != 1 {
		t.Fatalf("pull outcomes %v", got)
	}
	g.opPushToken(t.Context(), time.Now())
	if got := g.outcomes(RoutePushToken); got[OutcomeLostRaceExhausted] != 1 {
		t.Fatalf("push-token outcomes %v", got)
	}
	g.write(t.Context(), g.writers[0], time.Now(), RouteSyncWrite, 1)
	if got := g.outcomes(RouteSyncWrite); got[OutcomeLostRaceExhausted] != 1 || got[OutcomeSecurityBusy] != refreshRetries {
		t.Fatalf("write outcomes %v", got)
	}
	if v := EvaluateWindow(g.summary(g.phases.measure), SpecThresholds()); !unexpectedReason(v) {
		t.Fatalf("exhausted lost races passed: %v", v.Reasons)
	}
}

// M2: a replay answers as the original did. A retried timer start that was
// first superseded by the baby's live timer adopts that timer and ledgers
// nothing.
func TestReplayedSupersededTimerStartAdoptsTheLiveTimer(t *testing.T) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	d := g.writers[0]
	var p *plannedMutation
	for p == nil || p.m.Operation != "timer_start" {
		p = g.planTimer(d)
	}
	live := p.m
	live.EntityID = "e0000000-0000-4000-8000-000000000002"
	o := fakeOutcome(f.instance, d.FamilyID, live, 1)
	original := "superseded"
	o.MutationID, o.Status, o.OriginalStatus = p.m.MutationID, "already_applied", &original
	if out, _ := g.applyOutcomes(d, []*plannedMutation{p}, protocol.SyncResponse{Outcomes: []protocol.Outcome{o}}); out != OutcomeOK {
		t.Fatalf("outcome %q", out)
	}
	if g.ledgered.Load() != 0 {
		t.Fatal("a superseded start was ledgered")
	}
	adopted := false
	for _, ref := range g.families[d.FamilyID].timers {
		adopted = adopted || ref.id == live.EntityID
	}
	if !adopted {
		t.Fatal("the live timer was not adopted")
	}
	// A replay of an applied mutation is applied (and ledgered).
	q := g.planMutation(d)
	for q.m.EntityKind == "timer" || q.m.EntityKind == "growth_profile" {
		q = g.planMutation(d)
	}
	o = fakeOutcome(f.instance, d.FamilyID, q.m, 2)
	applied := "applied"
	o.Status, o.OriginalStatus = "already_applied", &applied
	if out, _ := g.applyOutcomes(d, []*plannedMutation{q}, protocol.SyncResponse{Outcomes: []protocol.Outcome{o}}); out != OutcomeOK || g.ledgered.Load() != 1 {
		t.Fatalf("applied replay: %q, ledgered %d", out, g.ledgered.Load())
	}
}

// M3: skipped operations past 1% of those launched fail the verdict.
func TestSkippedOperationsAreGated(t *testing.T) {
	s := passing()
	s.LaunchedOps, s.SkippedOps = 1000, 9
	if v := EvaluateWindow(s, SpecThresholds()); !v.Pass {
		t.Fatalf("0.9%% skipped failed: %v", v.Reasons)
	}
	s.SkippedOps = 10
	v := EvaluateWindow(s, SpecThresholds())
	if v.Pass || !slices.ContainsFunc(v.Reasons, func(r string) bool { return strings.Contains(r, "skipped 10 of 1000") }) {
		t.Fatalf("1%% skipped: %v", v.Reasons)
	}
}

// M3: a device whose warm-up refresh fails sits out, and it is counted.
func TestWarmupWithoutAccessIsCounted(t *testing.T) {
	f := newFakeV1(t)
	f.hook("POST /v1/auth/refresh-recoverable", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		writeErrorResponse(w, http.StatusUnauthorized, "session_revoked")
		return true
	})
	g := testGenerator(t, f, 1)
	d := g.devices[0]
	d.AccessExpiresAt = time.Now()
	g.startDevice(t.Context(), d, time.Now())
	if g.skipped["warmup_device_without_access"] != 1 || f.count("POST /v1/sync") != 0 {
		t.Fatalf("skipped %v, syncs %d", g.skipped, f.count("POST /v1/sync"))
	}
}

// M4: an acknowledged write the ledger cannot record fails the verdict.
func TestLedgerErrorsFailTheVerdict(t *testing.T) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	_ = g.ledger.Close()
	g.write(t.Context(), g.writers[0], time.Now(), RouteSyncWrite, 1)
	if g.ledgerErrs.Load() == 0 {
		t.Fatal("an append after close was not counted")
	}
	v := EvaluateWindow(g.summary(g.phases.measure), SpecThresholds())
	if v.Pass || !slices.ContainsFunc(v.Reasons, func(r string) bool { return strings.HasPrefix(r, "ledger:") }) {
		t.Fatalf("verdict %v", v.Reasons)
	}
}

// I4: a tracked device whose warm-up seed failed holds no complete map: the
// final pull takes it from 0, completing its seed, and only a seeded
// device goes into tracked.jsonl.
func TestUnseededTrackedDevicePullsFromZeroAtTheEnd(t *testing.T) {
	f := newFakeV1(t)
	f.hook("GET /v1/sync/snapshot", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		writeErrorResponse(w, http.StatusServiceUnavailable, "server_busy")
		return true
	})
	g := testGenerator(t, f, 2)
	seeded, unseeded := g.devices[0], g.devices[4]
	for _, d := range []*device{seeded, unseeded} {
		d.tracked = newTrackedState()
		d.Cursor = 7
	}
	g.seedTracked(t.Context(), unseeded, time.Now())
	if unseeded.tracked.isSeeded() {
		t.Fatal("a failed snapshot seeded the map")
	}
	g.accounts[seeded.AccountID].snapshots = []time.Time{time.Now().Add(time.Hour)} // its slot is taken: a pull from 0
	g.seedTracked(t.Context(), seeded, time.Now())
	if !seeded.tracked.isSeeded() {
		t.Fatal("a pull from 0 did not seed the map")
	}
	pullsBefore := len(f.all("/v1/sync"))
	g.finalPulls(t.Context())
	var fromZero bool
	for _, r := range f.all("/v1/sync")[pullsBefore:] {
		var request protocol.SyncRequest
		_ = json.Unmarshal(r.Body, &request)
		if request.Cursor != nil && request.Cursor.FamilyID == unseeded.FamilyID && request.Cursor.Sequence == 0 {
			fromZero = true
		}
	}
	if !fromZero || !unseeded.tracked.isSeeded() {
		t.Fatalf("the unseeded device's final pull was not from 0 (seeded now: %v)", unseeded.tracked.isSeeded())
	}

	// A device that never seeds stays out of tracked.jsonl.
	never := g.devices[1]
	never.tracked = newTrackedState()
	if err := g.writeTracked(g.cfg.Tracked); err != nil {
		t.Fatal(err)
	}
	tracked, err := ReadTracked(g.cfg.Tracked)
	if err != nil || len(tracked) != 2 {
		t.Fatalf("tracked.jsonl holds %d devices (%v), want the 2 seeded", len(tracked), err)
	}
	for _, d := range tracked {
		if d.DeviceID == never.DeviceID {
			t.Fatal("an unseeded device was written")
		}
	}
}
