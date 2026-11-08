package loadtest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Task 14 Step 1's cell: --model-override live-only holds every class but
// the live sockets at 0, and a ramp grows the sockets per step (0 → 4,000 →
// 8,000 → 12,000 at 10k families: steps 1, 2, 3). Every device's access has
// lapsed (a fleet from an earlier run): each live device refreshes before it
// dials, never answering a handshake with access_expired. Nothing else is
// sent, nothing is tracked, and the gates of the zeroed classes are not
// applicable, in the generator's verdict and in the run's after the audit.
func TestLiveOnlyRampHoldsOnlyTheSockets(t *testing.T) {
	f := newFakeV1(t)
	cfg := runConfig(t, f, 10, Model{RefreshPerSecond: 1.5, PullPerSecond: 20, WritePerSecond: 5, SnapshotPerSecond: 1, MCPPerSecond: 1,
		PushTokenPerSecond: 1, LiveActivityPerSecond: 1, LiveSockets: 4})
	header, states, err := ReadFleet(cfg.Fleet)
	if err != nil {
		t.Fatal(err)
	}
	for i := range states {
		states[i].AccessExpiresAt = time.Now().Add(-time.Hour)
	}
	if err := WriteFleet(cfg.Fleet, header, states); err != nil {
		t.Fatal(err)
	}
	cfg.ModelOverride = ModelLiveOnly
	cfg.Scenario, cfg.Steps, cfg.StepDuration = "ramp", []float64{1, 2}, 2*time.Second
	cfg.Warmup, cfg.Measure = 500*time.Millisecond, 4*time.Second
	summary, err := Run(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"POST /v1/sync", "GET /v1/sync/snapshot", "POST /mcp", "PUT /v1/devices/push-token", "PUT /v1/live-activities/device"} {
		if n := f.count(route); n != 0 {
			t.Errorf("%s sent %d times in a live-only run", route, n)
		}
	}
	// The second step's 8 sockets, each refreshed once before its first dial.
	if n := f.count("GET /v1/live"); n < 8 {
		t.Errorf("%d handshakes, want the 8 sockets of step 2", n)
	}
	if n := f.count("POST /v1/auth/refresh-recoverable"); n != 8 {
		t.Errorf("%d refreshes, want one per live device whose access lapsed (8)", n)
	}
	for route, rs := range summary.Routes {
		for outcome, n := range rs.Outcomes {
			if outcome != OutcomeOK {
				t.Errorf("%s: %d %s", route, n, outcome)
			}
		}
	}
	r := readResult(t, cfg.Out)
	if r.ModelOverride != ModelLiveOnly || r.Model.PullPerSecond != 0 || r.Model.RefreshPerSecond != 0 || r.Model.LiveSockets != 4 {
		t.Fatalf("model %+v (override %q)", r.Model, r.ModelOverride)
	}
	if r.TrackedDevices != 0 {
		t.Fatalf("tracked %d", r.TrackedDevices)
	}
	for class, n := range r.Launched {
		if n != 0 {
			t.Errorf("class %s launched %d", class, n)
		}
	}
	if len(r.Steps) != 2 || r.Steps[0].LiveSockets != 4 || r.Steps[1].LiveSockets != 8 || r.Steps[1].LiveOpenMean <= r.Steps[0].LiveOpenMean ||
		r.Steps[1].LiveOpenMin < 4 {
		t.Fatalf("steps %+v", r.Steps)
	}
	// Step 2's settled figures start after its ramp-in (a quarter of its
	// 2 s, at least 1 s) and see all 8 sockets.
	if s := r.Steps[1]; !s.SettledFrom.Equal(s.From.Add(time.Second)) || s.LiveOpenMinSettled != 8 || s.LiveOpenMeanSettled != 8 ||
		r.ModelOverride != ModelLiveOnly || r.GeneratorVerdict.ModelOverride != ModelLiveOnly || !(r.OpenSocketsMean > 0) {
		t.Fatalf("step 2 settled from %v (step from %v): min %d mean %v; verdict %q, open %v", s.SettledFrom, s.From, s.LiveOpenMinSettled,
			s.LiveOpenMeanSettled, r.GeneratorVerdict.ModelOverride, r.OpenSocketsMean)
	}
	if tracked, err := ReadTracked(cfg.Tracked); err != nil || len(tracked) != 0 {
		t.Fatalf("tracked.jsonl: %d devices, %v", len(tracked), err)
	}
	if !r.GeneratorVerdict.Pass || !strings.HasPrefix(r.GeneratorVerdict.Gates["sync_p95"], "n/a") ||
		!strings.HasPrefix(r.GeneratorVerdict.Gates["scheduler_lag_p99"], "n/a") {
		t.Fatalf("generator verdict %v %v", r.GeneratorVerdict.Reasons, r.GeneratorVerdict.Gates)
	}
	for _, step := range r.Steps {
		if !step.Verdict.Pass {
			t.Fatalf("step %s failed: %v", step.Name, step.Verdict.Reasons)
		}
	}
	// The run verdict after an audit of an empty ledger and no tracked device.
	if v := Evaluate(r.RunSummary, SpecThresholds()); !v.Pass || !strings.HasPrefix(v.Gates["acknowledged"], "n/a") ||
		!strings.HasPrefix(v.Gates["tracked_devices"], "n/a") {
		t.Fatalf("run verdict %v %v", v.Reasons, v.Gates)
	}
}

// Not applicable is live-only's alone: a full run that measured no sync, no
// write and no tracked device still fails, and a live-only run is judged on
// every gate that did measure something (a slow refresh fails it).
func TestOnlyALiveOnlyRunMarksEmptyGatesNotApplicable(t *testing.T) {
	empty := RunSummary{Routes: map[string]RouteSummary{RouteLive: {Latency: slowHistogram(5 * time.Millisecond), Outcomes: map[string]uint64{OutcomeOK: 100}}}}
	v := Evaluate(empty, SpecThresholds())
	for _, reason := range []string{"sync p95: no samples", "scheduler lag p99: no samples", "audit vacuous", "tracked devices vacuous"} {
		if !strings.Contains(strings.Join(v.Reasons, "; "), reason) {
			t.Errorf("a full run without %q passed it: %v", reason, v.Reasons)
		}
	}
	empty.ModelOverride, empty.OpenSocketsMean = ModelLiveOnly, 4000
	v = Evaluate(empty, SpecThresholds())
	if !v.Pass || v.ModelOverride != ModelLiveOnly || !strings.Contains(v.Gates["model_override"], "not the spec's PASS") {
		t.Fatalf("live-only: %v %q %v", v.Reasons, v.ModelOverride, v.Gates)
	}
	// verdict.json names the mode, so a live-only PASS is never cited as the spec's.
	if raw, _ := json.Marshal(v); !strings.Contains(string(raw), `"model_override":"live-only"`) {
		t.Fatalf("verdict JSON %s", raw)
	}
	// A live-only run that held no socket measured nothing: it fails.
	none := empty
	none.OpenSocketsMean = 0
	if v := Evaluate(none, SpecThresholds()); v.Pass || !strings.Contains(strings.Join(v.Reasons, "; "), "no live socket was open") {
		t.Fatalf("a live-only run without sockets: %v", v.Reasons)
	}
	if v := Evaluate(RunSummary{}, SpecThresholds()); v.ModelOverride != "" {
		t.Fatalf("a full verdict names a mode: %q", v.ModelOverride)
	}
	// A live-only step whose sockets opened before it sends nothing at all.
	quiet := RunSummary{Routes: map[string]RouteSummary{}, ModelOverride: ModelLiveOnly, OpenSocketsMean: 8}
	if v := EvaluateWindow(quiet, SpecThresholds()); !v.Pass {
		t.Fatalf("a quiet live-only step: %v", v.Reasons)
	}
	quiet.ModelOverride = ""
	if v := EvaluateWindow(quiet, SpecThresholds()); v.Pass || !strings.Contains(strings.Join(v.Reasons, "; "), "no requests recorded") {
		t.Fatalf("a quiet full step passed: %v", v.Reasons)
	}
	slow := empty
	slow.Routes = map[string]RouteSummary{RouteLive: empty.Routes[RouteLive],
		RouteRefresh: {Latency: slowHistogram(400 * time.Millisecond), Outcomes: map[string]uint64{OutcomeOK: 100}}}
	if v := Evaluate(slow, SpecThresholds()); v.Pass || !strings.Contains(strings.Join(v.Reasons, "; "), RouteRefresh+" p95") {
		t.Fatalf("a live-only run's slow refreshes passed: %v", v.Reasons)
	}
	if err := (RunConfig{Base: "b", Host: "h", Fleet: "f", Ledger: "l", Tracked: "t", Out: "o", Load: 1, Measure: time.Second,
		Scenario: "hot-family", ModelOverride: ModelLiveOnly}).validate(); err == nil {
		t.Fatal("live-only took a scenario that writes")
	}
}
