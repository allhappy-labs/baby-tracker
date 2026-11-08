package loadtest

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runConfig is a run against f over a fresh fake fleet.
func runConfig(t *testing.T, f *fakeV1, families int, m Model) RunConfig {
	t.Helper()
	dir, _, _ := fakeFleet(t, families)
	return RunConfig{Base: f.URL, Host: "sync.load.test", Fleet: filepath.Join(dir, "fleet.jsonl"), Ledger: filepath.Join(dir, "ledger.jsonl"),
		Tracked: filepath.Join(dir, "tracked.jsonl"), Out: filepath.Join(dir, "result.json"), Scenario: "soak", Load: 1, Seed: 11,
		TrackedDevices: 4, WithPump: true, model: &m,
		// The generator's own lag gate is loosened for -race runs on a busy
		// machine; nothing here measures it.
		Thresholds: Thresholds{SyncP95: 300 * time.Millisecond, RefreshP95: 300 * time.Millisecond, MaxSchedulerLagP99: time.Second,
			MaxUnexpectedRatio: 0.001}}
}

func readResult(t *testing.T, path string) RunResult {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r RunResult
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSoakSchedulesTheModelRates(t *testing.T) {
	// Against a fake that answers instantly: 40 devices, a model whose rates
	// 40 devices carry (each class within the server's per-device limits),
	// 20 s measure. Launched counts per class are within ±10% of rate ×
	// duration (or 4 standard deviations of the Poisson count, for the small
	// classes; refresh timers are slotted, so ±10% holds for them exactly), and window-start is written when warm-up ends.
	f := newFakeV1(t)
	// Snapshots at 0.5 a second: each account holds one for 15 minutes, so
	// the lead-in's and the window's (about 11) leave most of the fleet's
	// accounts idle for a snapshot's eight picks.
	m := Model{RefreshPerSecond: 1.6, PullPerSecond: 30, WritePerSecond: 10, SnapshotPerSecond: 0.5, MCPPerSecond: 1,
		PushTokenPerSecond: 5, LiveActivityPerSecond: 5, LiveSockets: 10}
	cfg := runConfig(t, f, 10, m)
	cfg.Warmup, cfg.Measure = 4*time.Second, 20*time.Second
	started := time.Now()
	summary, err := Run(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := readResult(t, cfg.Out)
	measure := cfg.Measure.Seconds()
	for class, rate := range map[string]float64{"pull": m.PullPerSecond, "write": m.WritePerSecond, "snapshot": m.SnapshotPerSecond,
		"mcp": m.MCPPerSecond, "push_token": m.PushTokenPerSecond, "live_activity": m.LiveActivityPerSecond, "refresh": m.RefreshPerSecond} {
		want := rate * measure
		tolerance := math.Max(0.10*want, 4*math.Sqrt(want))
		if class == "refresh" {
			// Not Poisson: every device has one timer slot per interval.
			tolerance = math.Max(0.10*want, 1)
		}
		if got := float64(r.Launched[class]); math.Abs(got-want) > tolerance {
			t.Errorf("%s launched %v, want %v ± %.0f", class, got, want, tolerance)
		}
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(cfg.Out), "window-start"))
	if err != nil {
		t.Fatalf("window-start: %v", err)
	}
	windowStart, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(raw)))
	if err != nil || windowStart.Sub(started.Add(cfg.Warmup)).Abs() > 100*time.Millisecond || !windowStart.Equal(r.WindowStart) {
		t.Fatalf("window-start %v (%v), want warm-up's end %v", windowStart, err, started.Add(cfg.Warmup))
	}
	// The one refresh schedule starts with the warm-up (not at window-start):
	// its first 4 s of timers (6.4 at the model's rate) are the warm-up's,
	// filed apart from the measurement's.
	if want := m.RefreshPerSecond * cfg.Warmup.Seconds(); math.Abs(float64(r.WarmupRefreshLaunched)-want) > 2 || r.WarmupRoutes[RouteRefresh].Latency.Total < 3 {
		t.Errorf("warm-up refreshes launched %d (filed %d), want %.1f ± 2", r.WarmupRefreshLaunched, r.WarmupRoutes[RouteRefresh].Latency.Total, want)
	}
	// The lead-in: the window's classes run over warm-up's last half (here
	// 2 s), filed as warm-up's and not launched (warm-up itself writes
	// nothing).
	lead := min(leadInMax, cfg.Warmup/2).Seconds()
	if got, want := float64(r.WarmupRoutes[RouteSyncWrite].Latency.Total), m.WritePerSecond*lead; math.Abs(got-want) > 4*math.Sqrt(want) {
		t.Errorf("warm-up writes %v, want the lead-in's %v ± %.0f", got, want, 4*math.Sqrt(want))
	}
	// Warm-up requests are kept apart from the measurement.
	if r.WarmupRoutes[RouteSyncPull].Latency.Total < 40 {
		t.Errorf("warm-up pulled %d times, want every device at least once", r.WarmupRoutes[RouteSyncPull].Latency.Total)
	}
	for route, rs := range summary.Routes {
		for outcome, n := range rs.Outcomes {
			if outcome != OutcomeOK {
				t.Errorf("%s: %d %s", route, n, outcome)
			}
		}
	}
	if r.LiveDevices != 10 || r.TrackedDevices != 4 || summary.UnexpectedLiveLosses != 0 {
		t.Errorf("live %d tracked %d losses %d", r.LiveDevices, r.TrackedDevices, summary.UnexpectedLiveLosses)
	}
	// The sockets actually held, and the refresh rate actually offered (the
	// token's lifetime does not bound a 25 s period).
	if r.LiveOpenMin < 1 || r.LiveOpenMean > float64(r.LiveDevices) || r.LiveOpenMean < 0.9*float64(r.LiveDevices) {
		t.Errorf("open sockets min %d mean %.1f of %d", r.LiveOpenMin, r.LiveOpenMean, r.LiveDevices)
	}
	if math.Abs(r.RefreshEffectivePerSecond-m.RefreshPerSecond) > 1e-9 || r.LaunchedOps == 0 || r.SkippedOps != 0 {
		t.Errorf("refresh effective %v, launched %d, skipped %d (%v)", r.RefreshEffectivePerSecond, r.LaunchedOps, r.SkippedOps, r.Skipped)
	}
	if !r.GeneratorVerdict.Pass {
		t.Errorf("generator verdict: %v", r.GeneratorVerdict.Reasons)
	}
}

func TestRunWritesPrivateOutputsAndCheckpointsTheFleet(t *testing.T) {
	f := newFakeV1(t)
	m := Model{RefreshPerSecond: 1.5, PullPerSecond: 20, WritePerSecond: 10, LiveSockets: 4}
	cfg := runConfig(t, f, 10, m)
	cfg.Warmup, cfg.Measure = 500*time.Millisecond, 3*time.Second
	_, before, _ := ReadFleet(cfg.Fleet)
	if _, err := Run(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{cfg.Ledger, cfg.Tracked, cfg.Fleet} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", filepath.Base(path), info.Mode(), err)
		}
	}
	ledger, err := ReadLedger(cfg.Ledger)
	if err != nil || len(ledger) == 0 {
		t.Fatalf("ledger: %d entries, %v", len(ledger), err)
	}
	tracked, err := ReadTracked(cfg.Tracked)
	if err != nil || len(tracked) != 4 {
		t.Fatalf("tracked: %d devices, %v", len(tracked), err)
	}
	// Once writes stopped, every tracked device pulled to its family's
	// head (the drain's pulls): the cursors in tracked.jsonl are post-pull.
	f.mu.Lock()
	for _, d := range tracked {
		if d.Cursor != f.seq[d.FamilyID] {
			t.Errorf("tracked device %s at cursor %d, its family's head is %d", d.DeviceID, d.Cursor, f.seq[d.FamilyID])
		}
	}
	f.mu.Unlock()
	if r := readResult(t, cfg.Out); r.DrainRoutes[RouteSyncPull].Outcomes[OutcomeOK] < 4 {
		t.Fatalf("final pulls %v (drain %v, measure end %v)", r.DrainRoutes[RouteSyncPull].Outcomes, sortedKeys(r.DrainRoutes), r.MeasureEnd)
	}
	_, after, err := ReadFleet(cfg.Fleet)
	if err != nil {
		t.Fatal(err)
	}
	rotated := 0
	for i := range after {
		if after[i].RefreshToken != before[i].RefreshToken {
			rotated++
		}
		if after[i].PendingAttemptID != "" {
			t.Fatalf("device %d left a pending attempt", i)
		}
	}
	if rotated == 0 {
		t.Fatal("the checkpoint holds no rotated token")
	}
	// result.json carries the run summary at its top level: the audit reads
	// it as a RunSummary.
	raw, _ := os.ReadFile(cfg.Out)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"routes", "scheduler_lag", "dropped", "audit", "host", "unexpected_live_losses", "generator_verdict"} {
		if _, ok := top[key]; !ok {
			t.Errorf("result.json has no %q", key)
		}
	}
	var summary RunSummary
	if err := json.Unmarshal(raw, &summary); err != nil || len(summary.Routes) == 0 {
		t.Fatalf("result.json as a RunSummary: %v", err)
	}
	if strings.Contains(string(raw), "access-") || strings.Contains(string(raw), before[0].RefreshToken) {
		t.Fatal("result.json holds a token")
	}
	// A second run into the same ledger is refused, not appended.
	if _, err := Run(t.Context(), cfg); err == nil {
		t.Fatal("a second run reused the ledger")
	}
}

func TestStaleFleetIsRefusedBeforeAnyRequest(t *testing.T) {
	f := newFakeV1(t)
	cfg := runConfig(t, f, 1, Model{PullPerSecond: 1})
	h, states, _ := ReadFleet(cfg.Fleet)
	h.AsOf = time.Now().Add(-29*24*time.Hour - time.Minute)
	if err := WriteFleet(cfg.Fleet, h, states); err != nil {
		t.Fatal(err)
	}
	cfg.Measure = time.Second
	if _, err := Run(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "seed again") {
		t.Fatalf("stale fleet: %v", err)
	}
	if len(f.all("/v1/sync")) != 0 {
		t.Fatal("requests were sent")
	}
	if _, err := os.Stat(cfg.Ledger); !os.IsNotExist(err) {
		t.Fatal("a ledger was created")
	}
}

func TestModelThatDoesNotFitTheFleetIsRefused(t *testing.T) {
	f := newFakeV1(t)
	cfg := runConfig(t, f, 1, Model{PullPerSecond: 100})
	cfg.Measure = time.Second
	if _, err := Run(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "pulls per device") {
		t.Fatalf("err %v", err)
	}
}

// Without a test model, the offered load is Model10k sized to the fleet.
func TestModelIsSizedToTheFleet(t *testing.T) {
	g := newGenerator(RunConfig{}, FleetHeader{Families: 2500}, nil)
	m := g.modelAt(2)
	if math.Abs(m.PullPerSecond-300) > 1e-9 || math.Abs(m.RefreshPerSecond-35) > 1e-9 || m.LiveSockets != 2000 {
		t.Fatalf("model %+v", m)
	}
}

// A ramp's steps only rise: setRate's retime would stretch a pending timer
// past the token's lifetime when a step falls.
func TestRampRefusesAFallingStep(t *testing.T) {
	f := newFakeV1(t)
	cfg := runConfig(t, f, 1, Model{PullPerSecond: 1})
	cfg.Scenario, cfg.Measure = "ramp", time.Second
	for _, steps := range [][]float64{{2, 1}, {1, 2, 1.5}} {
		cfg.Steps = steps
		if _, err := Run(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "must not fall") {
			t.Errorf("steps %v: %v", steps, err)
		}
	}
	if len(f.all("/v1/sync")) != 0 {
		t.Fatal("requests were sent")
	}
}
