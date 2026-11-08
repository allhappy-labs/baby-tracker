package loadtest

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestStormReconnectsWithTheIOSBackoff(t *testing.T) {
	// The reconnect delay for attempt n is uniform over 0..min(2^(n-1), 60) s
	// (SyncRetryPolicy(base: 1, ceiling: 60), FamilySyncWebSocket.swift:84).
	// The first attempt is drawn from 0..1 s.
	for attempt, want := range map[int]time.Duration{0: 0, 1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 6: 32 * time.Second,
		7: 60 * time.Second, 40: 60 * time.Second} {
		if got := iosBackoffWindow(attempt); got != want {
			t.Errorf("attempt %d window %v, want %v", attempt, got, want)
		}
	}
	rng := newLockedRand(1, 2)
	var sum time.Duration
	const n = 20000
	for range n {
		d := iosBackoff(1, rng)
		if d < 0 || d > time.Second {
			t.Fatalf("first attempt drew %v", d)
		}
		sum += d
	}
	if mean := sum / n; mean < 480*time.Millisecond || mean > 520*time.Millisecond {
		t.Fatalf("first-attempt mean %v, want uniform over 0..1 s (500 ms)", mean)
	}
	for range 1000 {
		if d := iosBackoff(9, rng); d > 60*time.Second {
			t.Fatalf("attempt 9 drew %v past the ceiling", d)
		}
	}

	// The storm itself: every socket closed client-side at once, each
	// redialled from its first attempt; 99% are back within about a second
	// and no close counts as a loss.
	f := newFakeV1(t)
	cfg := runConfig(t, f, 10, Model{RefreshPerSecond: 1.5, PullPerSecond: 10, LiveSockets: 20})
	cfg.Scenario, cfg.Warmup, cfg.Measure, cfg.StormAt = "storm", 500*time.Millisecond, 4*time.Second, time.Second
	summary, err := Run(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := readResult(t, cfg.Out)
	if r.Storm == nil || r.Storm.Sockets != 20 || r.Storm.Reconnected != 20 || r.Storm.SecondsTo99 < 0 || r.Storm.SecondsTo99 > 1.5 {
		t.Fatalf("storm %+v", r.Storm)
	}
	if summary.UnexpectedLiveLosses != 0 {
		t.Fatalf("storm closes counted as losses: %d", summary.UnexpectedLiveLosses)
	}
	names := []string{}
	for _, w := range r.Windows {
		names = append(names, w.Name)
	}
	if !slices.Equal(names, []string{"before_storm", "storm"}) {
		t.Fatalf("windows %v", names)
	}
	if storm := r.Windows[1].Summary.Routes[RouteLive]; storm.Outcomes[OutcomeOK] < 20 {
		t.Fatalf("storm window handshakes %v", storm.Outcomes)
	}
}

// Task 3 carry M4: a window (ramp step, storm or restart window) is judged
// by the window gates alone. The whole-run gates (the audit, the tracked
// devices, the host) would fail every window; a restart window's declared
// failures and lost races are not unexpected.
func TestEvaluateWindowJudgesAWindowWithoutTheRunGates(t *testing.T) {
	step := passing()
	step.Audit = AuditResult{} // no audit for a window
	if v := Evaluate(step, SpecThresholds()); v.Pass {
		t.Fatal("a summary without an audit passed the run verdict")
	}
	if v := EvaluateWindow(step, SpecThresholds()); !v.Pass {
		t.Fatalf("a fast clean step failed its window gates: %v", v.Reasons)
	}
	for _, gate := range []string{"acknowledged", "tracked_devices", "lost", "host"} {
		if _, ok := EvaluateWindow(step, SpecThresholds()).Gates[gate]; ok {
			t.Errorf("window verdict carries the run gate %s", gate)
		}
	}

	declared := passing()
	declared.Routes[RouteSyncPull].Outcomes[OutcomeRestartWindow] = 500
	declared.Routes[RouteSyncPull].Outcomes[OutcomeSecurityBusy] = 20
	declared.Routes[RouteRefresh].Outcomes[OutcomeCredentialRotationBusy] = 20
	if v := EvaluateWindow(declared, SpecThresholds()); !v.Pass {
		t.Fatalf("declared outcomes counted: %v", v.Reasons)
	}
	if v := Evaluate(declared, SpecThresholds()); !v.Pass {
		t.Fatalf("declared outcomes failed the run: %v", v.Reasons)
	}

	slow := passing()
	slow.Routes[RouteSyncPull] = RouteSummary{Latency: slowHistogram(400 * time.Millisecond), Outcomes: map[string]uint64{OutcomeOK: 1000}}
	if v := EvaluateWindow(slow, SpecThresholds()); v.Pass {
		t.Fatal("a slow step passed")
	}
	busy := passing()
	busy.Routes[RouteMCP] = RouteSummary{Latency: slowHistogram(40 * time.Millisecond), Outcomes: map[string]uint64{OutcomeOK: 10, OutcomeServerBusy: 3, OutcomeServerTimeout: 2}}
	v := EvaluateWindow(busy, SpecThresholds())
	if v.Pass || !strings.HasPrefix(v.Gates["saturation"], "5 ") {
		t.Fatalf("saturation (MCP server_busy and timeout) not counted: %v %q", v.Reasons, v.Gates["saturation"])
	}
	lost := passing()
	lost.UnexpectedLiveLosses = 1
	if EvaluateWindow(lost, SpecThresholds()).Pass {
		t.Fatal("a window with a live loss passed")
	}
}

func TestRampStopsAtTheFirstFailingStepPlusOne(t *testing.T) {
	f := newFakeV1(t)
	cfg := runConfig(t, f, 10, Model{RefreshPerSecond: 1.5, PullPerSecond: 20, LiveSockets: 4})
	cfg.Scenario, cfg.Steps, cfg.StepDuration = "ramp", []float64{1, 1, 1, 1, 1}, 2*time.Second
	cfg.Warmup, cfg.Measure = 500*time.Millisecond, 10*time.Second
	windowStart := time.Now().Add(cfg.Warmup)
	slowFrom, slowTo := windowStart.Add(2*time.Second), windowStart.Add(4*time.Second)
	f.hook("POST /v1/sync", func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if now := time.Now(); now.After(slowFrom.Add(50*time.Millisecond)) && now.Before(slowTo) {
			time.Sleep(400 * time.Millisecond)
		}
		return false
	})
	if _, err := Run(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	r := readResult(t, cfg.Out)
	if len(r.Steps) != 3 {
		t.Fatalf("%d steps ran, want 3 (step 2 fails, one more step)", len(r.Steps))
	}
	if !r.Steps[0].Verdict.Pass || r.Steps[1].Verdict.Pass {
		t.Fatalf("step verdicts: 1 %v %v; 2 %v %v", r.Steps[0].Verdict.Pass, r.Steps[0].Verdict.Reasons, r.Steps[1].Verdict.Pass, r.Steps[1].Verdict.Reasons)
	}
	if _, ok := r.Steps[0].Verdict.Gates["acknowledged"]; ok {
		t.Fatal("a step was judged by the audit gates")
	}
	if !r.MeasureEnd.Equal(r.Steps[2].To) {
		t.Fatalf("measurement ends %v, the third step %v", r.MeasureEnd, r.Steps[2].To)
	}
}

func TestRestartWindowClassesFailuresAndCountsNoLoss(t *testing.T) {
	f := newFakeV1(t)
	cfg := runConfig(t, f, 10, Model{RefreshPerSecond: 1.5, PullPerSecond: 20, LiveSockets: 8})
	cfg.Scenario, cfg.Warmup, cfg.Measure, cfg.RestartAt, cfg.RestartGrace = "restart", 500*time.Millisecond, 4*time.Second, time.Second, 1500*time.Millisecond
	restartFrom := time.Now().Add(cfg.Warmup + cfg.RestartAt)
	down := func() bool {
		now := time.Now()
		return now.After(restartFrom.Add(200*time.Millisecond)) && now.Before(restartFrom.Add(cfg.RestartGrace-300*time.Millisecond))
	}
	f.hook("POST /v1/sync", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		if down() {
			writeErrorResponse(w, http.StatusServiceUnavailable, "not_ready")
			return true
		}
		return false
	})
	var closed bool
	f.hook("GET /v1/live", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		if down() {
			writeErrorResponse(w, http.StatusServiceUnavailable, "not_ready")
			return true
		}
		return false
	})
	go func() {
		if sleepUntil(t.Context(), restartFrom.Add(300*time.Millisecond)) {
			closed = f.closeSocketsWith(1012, "service restart") > 0
		}
	}()
	summary, err := Run(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !closed {
		t.Fatal("no socket was open at the restart")
	}
	var declared uint64
	for route, rs := range summary.Routes {
		for outcome, n := range rs.Outcomes {
			switch outcome {
			case OutcomeOK:
			case OutcomeRestartWindow:
				declared += n
			default:
				t.Errorf("%s: %d %s outside the restart window", route, n, outcome)
			}
		}
	}
	if declared == 0 || summary.UnexpectedLiveLosses != 0 {
		t.Fatalf("restart_window %d, losses %d", declared, summary.UnexpectedLiveLosses)
	}
	r := readResult(t, cfg.Out)
	if len(r.Windows) != 3 || r.Windows[1].Name != "restart" || r.Windows[1].Summary.Routes[RouteSyncPull].Outcomes[OutcomeRestartWindow] == 0 {
		t.Fatalf("windows %+v", r.Windows)
	}
	if !r.GeneratorVerdict.Pass {
		t.Fatalf("restart run's generator verdict: %v", r.GeneratorVerdict.Reasons)
	}
}

func TestFloodIsRecordedApartFromTheRun(t *testing.T) {
	defer func(n int) { floodBearerBodyBytes = n }(floodBearerBodyBytes)
	floodBearerBodyBytes = 4 << 10 // the fake keeps every body
	f := newFakeV1(t)
	flood := func(r *http.Request) bool { return strings.HasPrefix(r.Header.Get("X-Forwarded-For"), "198.1") }
	f.hook("POST /v1/sync", func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if flood(r) {
			writeErrorResponse(w, http.StatusUnauthorized, "session_revoked")
			return true
		}
		return false
	})
	f.hook("POST /v1/auth/refresh-recoverable", func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if flood(r) {
			writeErrorResponse(w, http.StatusUnauthorized, "session_revoked")
			return true
		}
		return false
	})
	cfg := runConfig(t, f, 10, Model{RefreshPerSecond: 1.5, PullPerSecond: 10})
	cfg.Scenario, cfg.Warmup, cfg.Measure = "flood", 200*time.Millisecond, 2*time.Second
	summary, err := Run(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := readResult(t, cfg.Out)
	if r.AttackRoutes[RouteFloodChallenge].Latency.Total < 1000 || r.AttackRoutes[RouteFloodRefresh].Outcomes["http_4xx_session_revoked"] < 1000 ||
		r.AttackRoutes[RouteFloodBadBearer].Outcomes["http_4xx_session_revoked"] < 500 {
		t.Fatalf("attack routes %v", r.AttackRoutes)
	}
	for _, route := range []string{RouteFloodChallenge, RouteFloodRefresh, RouteFloodBadBearer} {
		if _, ok := summary.Routes[route]; ok {
			t.Fatalf("%s requests are in the run's routes", route)
		}
	}
	if !r.GeneratorVerdict.Pass {
		t.Fatalf("the flood judged the run: %v", r.GeneratorVerdict.Reasons)
	}
	cell, ok := r.Cells["flood"].(map[string]any)
	if !ok || cell["ip_addresses"] != float64(floodIPAddresses) || cell["bearer_addresses"] != float64(floodBearerAddresses) ||
		cell["bearer_body_bytes"] != float64(floodBearerBodyBytes) ||
		fmt.Sprint(cell["evictions_expected"]) != "[auth-challenge-ip auth-refresh-ip]" {
		t.Fatalf("flood cell %v", r.Cells["flood"])
	}
}
