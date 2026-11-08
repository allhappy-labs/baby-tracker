package loadtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func passing() RunSummary {
	fast := func() Histogram {
		var h Histogram
		for i := 0; i < 1000; i++ {
			h.Record(40 * time.Millisecond)
		}
		return h
	}
	routes := map[string]RouteSummary{}
	for _, r := range []string{RouteSyncPull, RouteSyncWrite, RouteRefresh, RouteRefreshAck} {
		routes[r] = RouteSummary{Latency: fast(), Outcomes: map[string]uint64{OutcomeOK: 1000}}
	}
	var lag Histogram
	for i := 0; i < 1000; i++ {
		lag.Record(2 * time.Millisecond)
	}
	return RunSummary{Routes: routes, SchedulerLag: lag, Audit: AuditResult{Acknowledged: 500, TrackedDevices: 200}}
}

func expectReason(t *testing.T, s RunSummary, substr string) {
	t.Helper()
	v := Evaluate(s, SpecThresholds())
	if v.Pass {
		t.Fatalf("passed, want a failure naming %q", substr)
	}
	for _, r := range v.Reasons {
		if strings.Contains(r, substr) {
			return
		}
	}
	t.Fatalf("no reason contains %q: %q", substr, v.Reasons)
}

func slowHistogram(d time.Duration) Histogram {
	var h Histogram
	for i := 0; i < 1000; i++ {
		h.Record(d)
	}
	return h
}

func TestEvaluatePassesAndNamesEachBreach(t *testing.T) {
	if v := Evaluate(passing(), SpecThresholds()); !v.Pass {
		t.Fatalf("passing run failed: %v", v.Reasons)
	}
	slow := passing()
	h := Histogram{}
	for i := 0; i < 1000; i++ {
		h.Record(400 * time.Millisecond)
	}
	slow.Routes[RouteSyncWrite] = RouteSummary{Latency: h, Outcomes: map[string]uint64{OutcomeOK: 1000}}
	expectReason(t, slow, "sync p95")
	errs := passing()
	errs.Routes[RouteSyncPull].Outcomes["server_busy"] = 5 // 5 of 4005: > 0.1%
	expectReason(t, errs, "unexpected 0.12%")
	lost := passing()
	lost.Audit.Lost = 1
	expectReason(t, lost, "lost 1")
	cross := passing()
	cross.Audit.CrossFamily = 1
	expectReason(t, cross, "cross-family 1")
}

func TestEvaluateGatesEachRouteClass(t *testing.T) {
	hint := passing()
	hint.Routes[RouteSyncHintPull] = RouteSummary{Latency: slowHistogram(5 * time.Second), Outcomes: map[string]uint64{OutcomeOK: 1000}}
	expectReason(t, hint, "sync p95") // hint pulls are sync requests: a third of the merged sync samples are slow
	refresh := passing()
	refresh.Routes[RouteRefresh] = RouteSummary{Latency: slowHistogram(310 * time.Millisecond), Outcomes: map[string]uint64{OutcomeOK: 1000}}
	expectReason(t, refresh, "refresh_recoverable p95")
	ack := passing()
	ack.Routes[RouteRefreshAck] = RouteSummary{Latency: slowHistogram(310 * time.Millisecond), Outcomes: map[string]uint64{OutcomeOK: 1000}}
	expectReason(t, ack, "refresh_ack p95")
	// A slow route outside the gates is reported, not gated.
	snap := passing()
	snap.Routes[RouteSnapshot] = RouteSummary{Latency: slowHistogram(2 * time.Second), Outcomes: map[string]uint64{OutcomeOK: 10}}
	if v := Evaluate(snap, SpecThresholds()); !v.Pass {
		t.Fatalf("an ungated route failed the run: %v", v.Reasons)
	}
	// Unexpected outcomes count on every route, gated or not.
	live := passing()
	live.Routes[RouteLive] = RouteSummary{Latency: slowHistogram(time.Millisecond), Outcomes: map[string]uint64{"transport": 5}}
	expectReason(t, live, "unexpected")
}

func TestEvaluateUnexpectedRatioIsStrict(t *testing.T) {
	s := passing()
	s.Routes[RouteSyncPull] = RouteSummary{Latency: slowHistogram(time.Millisecond), Outcomes: map[string]uint64{OutcomeOK: 1000, "server_busy": 0}}
	// 4 of 4000 is exactly 0.1%; the spec says under 0.1%.
	s.Routes[RouteSyncPull].Outcomes[OutcomeOK] = 996
	s.Routes[RouteSyncPull].Outcomes["server_busy"] = 4
	expectReason(t, s, "unexpected 0.10%")
	s.Routes[RouteSyncPull].Outcomes[OutcomeOK] = 997
	s.Routes[RouteSyncPull].Outcomes["server_busy"] = 3
	if v := Evaluate(s, SpecThresholds()); !v.Pass {
		t.Fatalf("3 of 4000 failed: %v", v.Reasons)
	}
}

func TestEvaluateFailsSaturatedOrLaggingGenerator(t *testing.T) {
	s := passing()
	s.Dropped = 3
	expectReason(t, s, "generator saturated: 3 dropped arrivals")
	l := passing()
	l.SchedulerLag = Histogram{}
	for i := 0; i < 100; i++ {
		l.SchedulerLag.Record(50 * time.Millisecond)
	}
	expectReason(t, l, "scheduler lag p99")
}

func TestEvaluateFailsContaminatedRun(t *testing.T) {
	s := passing()
	s.Host = HostVerdict{Contaminated: true, Offenders: []string{"swift-frontend"}}
	expectReason(t, s, "host contaminated: swift-frontend")
}

func TestEvaluateFailsVacuousAudit(t *testing.T) {
	s := passing()
	s.Audit.Acknowledged = 0
	expectReason(t, s, "audit vacuous")
	d := passing()
	d.Audit.TrackedDevices = 0
	expectReason(t, d, "tracked devices vacuous")
}

func TestEvaluateFailsEveryAuditAndLiveLoss(t *testing.T) {
	div := passing()
	div.Audit.Diverged = 2
	expectReason(t, div, "diverged 2")
	tr := passing()
	tr.Audit.TrackedDiverged = 1
	expectReason(t, tr, "tracked devices diverged 1")
	live := passing()
	live.UnexpectedLiveLosses = 4
	expectReason(t, live, "unexpected live losses 4")
}

// A gate with nothing measured cannot pass.
func TestEvaluateFailsGatesWithNoSamples(t *testing.T) {
	none := passing()
	delete(none.Routes, RouteSyncPull)
	delete(none.Routes, RouteSyncWrite)
	expectReason(t, none, "sync p95: no samples")
	noAck := passing()
	delete(noAck.Routes, RouteRefreshAck)
	expectReason(t, noAck, "refresh_ack p95: no samples")
	noLag := passing()
	noLag.SchedulerLag = Histogram{}
	expectReason(t, noLag, "scheduler lag p99: no samples")
	expectReason(t, RunSummary{Audit: AuditResult{Acknowledged: 1, TrackedDevices: 1}}, "no requests")
}

// A latency beyond the histogram's ~134 s range is clamped, so a quantile
// could hide it: any overflow, gated route or not, fails the run.
func TestEvaluateFailsLatencyOverflow(t *testing.T) {
	overflowed := func() Histogram {
		h := slowHistogram(40 * time.Millisecond)
		h.Record(200 * time.Second) // one in 1001: p95 is still 40 ms
		return h
	}
	sync := passing()
	sync.Routes[RouteSyncPull] = RouteSummary{Latency: overflowed(), Outcomes: map[string]uint64{OutcomeOK: 1001}}
	expectReason(t, sync, "latency overflow")
	expectReason(t, sync, "sync_pull 1")
	snap := passing()
	snap.Routes[RouteSnapshot] = RouteSummary{Latency: overflowed(), Outcomes: map[string]uint64{OutcomeOK: 1001}}
	expectReason(t, snap, "snapshot_page 1")
	lag := passing()
	lag.SchedulerLag.Record(200 * time.Second) // one in 1001: p99 is still 2 ms
	expectReason(t, lag, "scheduler lag 1")
	if v := Evaluate(sync, SpecThresholds()); v.Gates["latency_overflow"] != "1 / 0" {
		t.Fatalf("latency_overflow gate %q", v.Gates["latency_overflow"])
	}
}

func TestEvaluateReportsEveryGate(t *testing.T) {
	v := Evaluate(passing(), SpecThresholds())
	for _, g := range []string{"latency_overflow", "sync_p95", "refresh_recoverable_p95", "refresh_ack_p95", "unexpected_ratio", "scheduler_lag_p99", "dropped_arrivals", "lost", "cross_family", "diverged", "tracked_diverged", "acknowledged", "tracked_devices", "unexpected_live_losses", "host"} {
		if !strings.Contains(v.Gates[g], " / ") {
			t.Errorf("gate %s = %q, want \"value / threshold\"", g, v.Gates[g])
		}
	}
	if v.Gates["sync_p95"] != "40ms / 300ms" {
		t.Errorf("sync_p95 gate %q", v.Gates["sync_p95"])
	}
}

// thresholds.json is Task 25's tightening from the PASS at the final code
// (the v10 2x soak at 0482fe8a, wait 250ms: sync p95 8.063 ms,
// refresh_recoverable p95 12.799 ms, unexpected 0.0000%): p95s at
// min(300 ms, 1.5 × the soak's) rounded up to 10 ms, the unexpected ratio
// at 0.05% (the owner's loosening of the rule's 0.01% floor, 2026-10-27:
// it covers every measured 2x PASS, 0.0001-0.0423%), and the scheduler lag
// where the spec has it. Every value is at or under the
// spec's, and LoadThresholds reads the file.
func TestThresholdsAreTighterThanTheSpec(t *testing.T) {
	got, err := LoadThresholds("thresholds.json")
	if err != nil {
		t.Fatal(err)
	}
	spec := SpecThresholds()
	if got.SyncP95 <= 0 || got.SyncP95 > spec.SyncP95 || got.RefreshP95 <= 0 || got.RefreshP95 > spec.RefreshP95 ||
		got.MaxSchedulerLagP99 <= 0 || got.MaxSchedulerLagP99 > spec.MaxSchedulerLagP99 ||
		got.MaxUnexpectedRatio <= 0 || got.MaxUnexpectedRatio > spec.MaxUnexpectedRatio {
		t.Fatalf("thresholds.json %+v is looser than the spec %+v", got, spec)
	}
	want := Thresholds{SyncP95: 20 * time.Millisecond, RefreshP95: 20 * time.Millisecond, MaxSchedulerLagP99: 20 * time.Millisecond,
		MaxUnexpectedRatio: 0.0005}
	if got != want {
		t.Fatalf("thresholds.json %+v, want the Task 25 tightening %+v", got, want)
	}
}

func TestLoadThresholdsAcceptsTighterValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thresholds.json")
	body := `{"sync_p95_ms": 120, "refresh_p95_ms": 90.5, "max_scheduler_lag_p99_ms": 20, "max_unexpected_ratio": 0.0001}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadThresholds(path)
	want := Thresholds{SyncP95: 120 * time.Millisecond, RefreshP95: 90500 * time.Microsecond, MaxSchedulerLagP99: 20 * time.Millisecond, MaxUnexpectedRatio: 0.0001}
	if err != nil || got != want {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
}

func TestLoadThresholdsRejectsBadFiles(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"missing field": `{"sync_p95_ms": 300, "refresh_p95_ms": 300, "max_scheduler_lag_p99_ms": 20}`,
		"unknown field": `{"sync_p95_ms": 300, "refresh_p95_ms": 300, "max_scheduler_lag_p99_ms": 20, "max_unexpected_ratio": 0.001, "sync_p59_ms": 1}`,
		"zero":          `{"sync_p95_ms": 0, "refresh_p95_ms": 300, "max_scheduler_lag_p99_ms": 20, "max_unexpected_ratio": 0.001}`,
		"ratio above 1": `{"sync_p95_ms": 300, "refresh_p95_ms": 300, "max_scheduler_lag_p99_ms": 20, "max_unexpected_ratio": 2}`,
		"trailing data": `{"sync_p95_ms": 300, "refresh_p95_ms": 300, "max_scheduler_lag_p99_ms": 20, "max_unexpected_ratio": 0.001} {}`,
		// Looser than the spec, one gate at a time.
		"sync looser":    `{"sync_p95_ms": 300.5, "refresh_p95_ms": 300, "max_scheduler_lag_p99_ms": 20, "max_unexpected_ratio": 0.001}`,
		"refresh looser": `{"sync_p95_ms": 300, "refresh_p95_ms": 400, "max_scheduler_lag_p99_ms": 20, "max_unexpected_ratio": 0.001}`,
		"lag looser":     `{"sync_p95_ms": 300, "refresh_p95_ms": 300, "max_scheduler_lag_p99_ms": 21, "max_unexpected_ratio": 0.001}`,
		"ratio looser":   `{"sync_p95_ms": 300, "refresh_p95_ms": 300, "max_scheduler_lag_p99_ms": 20, "max_unexpected_ratio": 0.0011}`,
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadThresholds(path); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := LoadThresholds(filepath.Join(dir, "absent.json")); err == nil {
		t.Error("a missing file was accepted")
	}
}
