package loadtest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"slices"
	"strings"
	"time"
)

// Thresholds are the SLO gates. SpecThresholds are the spec's values;
// thresholds.json holds the gates a run is judged by (tightened by Task 25
// after the first PASS, never looser than the spec).
type Thresholds struct {
	SyncP95            time.Duration
	RefreshP95         time.Duration
	MaxSchedulerLagP99 time.Duration
	// MaxUnexpectedRatio is exclusive: the spec's "under 0.1%" fails at
	// exactly 0.1%.
	MaxUnexpectedRatio float64
}

func SpecThresholds() Thresholds {
	return Thresholds{
		SyncP95:            300 * time.Millisecond,
		RefreshP95:         300 * time.Millisecond,
		MaxSchedulerLagP99: 20 * time.Millisecond,
		MaxUnexpectedRatio: 0.001,
	}
}

type thresholdsFile struct {
	SyncP95Ms            *float64 `json:"sync_p95_ms"`
	RefreshP95Ms         *float64 `json:"refresh_p95_ms"`
	MaxSchedulerLagP99Ms *float64 `json:"max_scheduler_lag_p99_ms"`
	MaxUnexpectedRatio   *float64 `json:"max_unexpected_ratio"`
}

// LoadThresholds reads a thresholds file. Every field is required and
// positive, unknown fields are rejected (a misspelt gate must not silently
// fall back), and no value may be looser than SpecThresholds: Task 25 may
// tighten the spec, never relax it.
func LoadThresholds(path string) (Thresholds, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Thresholds{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var f thresholdsFile
	if err := dec.Decode(&f); err != nil {
		return Thresholds{}, fmt.Errorf("thresholds %s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Thresholds{}, fmt.Errorf("thresholds %s: trailing data", path)
	}
	fields := []struct {
		name string
		v    *float64
	}{
		{"sync_p95_ms", f.SyncP95Ms}, {"refresh_p95_ms", f.RefreshP95Ms},
		{"max_scheduler_lag_p99_ms", f.MaxSchedulerLagP99Ms}, {"max_unexpected_ratio", f.MaxUnexpectedRatio},
	}
	for _, fl := range fields {
		if fl.v == nil {
			return Thresholds{}, fmt.Errorf("thresholds %s: %s is missing", path, fl.name)
		}
		if !(*fl.v > 0) || math.IsInf(*fl.v, 0) {
			return Thresholds{}, fmt.Errorf("thresholds %s: %s = %v, want a positive number", path, fl.name, *fl.v)
		}
	}
	if *f.MaxUnexpectedRatio > 1 {
		return Thresholds{}, fmt.Errorf("thresholds %s: max_unexpected_ratio %v is above 1", path, *f.MaxUnexpectedRatio)
	}
	ms := func(v float64) time.Duration { return time.Duration(math.Round(v * float64(time.Millisecond))) }
	t := Thresholds{
		SyncP95:            ms(*f.SyncP95Ms),
		RefreshP95:         ms(*f.RefreshP95Ms),
		MaxSchedulerLagP99: ms(*f.MaxSchedulerLagP99Ms),
		MaxUnexpectedRatio: *f.MaxUnexpectedRatio,
	}
	if looser := looserThanSpec(t); looser != "" {
		return Thresholds{}, fmt.Errorf("thresholds %s: looser than the spec: %s", path, looser)
	}
	return t, nil
}

// looserThanSpec names every gate in t that is looser than SpecThresholds,
// or returns "".
func looserThanSpec(t Thresholds) string {
	spec := SpecThresholds()
	var looser []string
	if t.SyncP95 > spec.SyncP95 {
		looser = append(looser, fmt.Sprintf("sync_p95_ms %v > %v", t.SyncP95, spec.SyncP95))
	}
	if t.RefreshP95 > spec.RefreshP95 {
		looser = append(looser, fmt.Sprintf("refresh_p95_ms %v > %v", t.RefreshP95, spec.RefreshP95))
	}
	if t.MaxSchedulerLagP99 > spec.MaxSchedulerLagP99 {
		looser = append(looser, fmt.Sprintf("max_scheduler_lag_p99_ms %v > %v", t.MaxSchedulerLagP99, spec.MaxSchedulerLagP99))
	}
	if t.MaxUnexpectedRatio > spec.MaxUnexpectedRatio {
		looser = append(looser, fmt.Sprintf("max_unexpected_ratio %v > %v", t.MaxUnexpectedRatio, spec.MaxUnexpectedRatio))
	}
	return strings.Join(looser, ", ")
}

// RunSummary is everything a verdict is computed from.
type RunSummary struct {
	Routes               map[string]RouteSummary `json:"routes"`
	SchedulerLag         Histogram               `json:"scheduler_lag"`
	Dropped              uint64                  `json:"dropped"`
	Audit                AuditResult             `json:"audit"`
	Host                 HostVerdict             `json:"host"`
	UnexpectedLiveLosses int                     `json:"unexpected_live_losses"`
	// LaunchedOps and SkippedOps are the generator's own operations and the
	// ones it launched but could not carry out (Task 10): too many skips
	// mean the offered load fell short of the model.
	LaunchedOps uint64 `json:"launched_ops"`
	SkippedOps  uint64 `json:"skipped_ops"`
	// LedgerErrors counts acknowledged writes the ledger could not record:
	// the audit cannot see them, so any fails the run.
	LedgerErrors int `json:"ledger_errors"`
	// TrackedUnseeded counts tracked devices whose map never took a
	// complete seed, even from the final pull from 0: they are left out of
	// tracked.jsonl, so the audit cannot check them, and any fails the run.
	TrackedUnseeded int `json:"tracked_unseeded"`
	// ModelOverride is the run's --model-override (ModelLiveOnly): the
	// gates of the classes it holds at 0 are not applicable when they
	// measured nothing.
	ModelOverride string `json:"model_override,omitempty"`
	// OpenSocketsMean is the live sockets open over the span, sampled every
	// second (a live-only run's measure: none open fails it).
	OpenSocketsMean float64 `json:"open_sockets_mean,omitempty"`
}

// AuditResult is the post-run comparison of the write ledger and the tracked
// devices against the database (Task 11).
type AuditResult struct {
	// Acknowledged is the distinct entities the ledger holds, each checked
	// at its latest acknowledged version.
	Acknowledged int `json:"acknowledged_entities"`
	// AcknowledgedWrites is every acknowledgement the ledger file holds
	// (ReadLedgerCounted), retries included; 0 when the caller did not read
	// a file.
	AcknowledgedWrites int `json:"acknowledged_writes"`
	Lost               int `json:"lost"`
	CrossFamily        int `json:"cross_family"`
	Diverged           int `json:"diverged"`
	TrackedDevices     int `json:"tracked_devices"`
	TrackedDiverged    int `json:"tracked_diverged"`
	// TrackedSkipped is the tracked devices' entities held at an older
	// version than a row that changed after the device's cursor: not
	// comparable, only counted.
	TrackedSkipped int `json:"tracked_skipped"`
}

// HostVerdict is the machine-state check over the run (Task 1).
type HostVerdict struct {
	Contaminated bool     `json:"contaminated"`
	Offenders    []string `json:"offenders"`
	MaxLoad1     float64  `json:"max_load1"`
}

// Verdict is PASS or FAIL with every reason, plus each gate as
// "value / threshold" for the report.
type Verdict struct {
	Pass    bool              `json:"pass"`
	Reasons []string          `json:"reasons"`
	Gates   map[string]string `json:"gates"`
	// ModelOverride names a run judged on part of the model (live-only):
	// its PASS is that cell's, never the spec's 2x PASS.
	ModelOverride string `json:"model_override,omitempty"`
}

// expectedOutcome reports a non-ok outcome that is not an error and does
// not count against the unexpected ratio:
//   - restart_window: a failure inside a declared restart window (Task 23
//     judges the restart by its own windows);
//   - security_busy and credential_rotation_busy: a lost race the server
//     answers 503 with Retry-After 1 (Task 5), recorded only when a retry
//     follows it (the retry is a request of its own). A lost race that is
//     never resolved ends as lost_race_exhausted, which is unexpected;
//   - import_paused: a 429 the import cell waited out per Retry-After
//     before resuming, as the iOS importer does;
//   - import_leftover: a new import's chunk 0 refused because the owner's
//     account holds an unfinished import the fleet cannot resume; the cell
//     moves to the next owner (at most once per account per staging TTL).
//
// Every other outcome but ok is unexpected, server_busy included: a
// request the server shed at the measured load is an error the client saw
// (it is reported as saturation, apart from http_5xx).
//
// The unexpected ratio's denominator is every recorded request, expected
// outcomes included: an expected outcome is a request the server answered.
func expectedOutcome(outcome string) bool {
	switch outcome {
	case OutcomeRestartWindow, OutcomeSecurityBusy, OutcomeCredentialRotationBusy, OutcomeImportPaused, OutcomeImportLeftover:
		return true
	}
	return false
}

// saturationOutcome reports the refusals of a saturated server (httpapi's
// and MCP's admission and deadline 503s). They are unexpected; the
// "saturation" gate line counts them so a report can tell shedding from
// failures.
func saturationOutcome(outcome string) bool {
	return outcome == OutcomeServerBusy || outcome == OutcomeServerTimeout
}

// maxSkippedShare is the share of launched operations the generator may
// skip (no idle account for a snapshot, a refresh already in flight, a
// device whose warm-up failed) before the offered load is no longer the
// model's.
const maxSkippedShare = 0.01

// Evaluate applies the spec's PASS criteria. Any reason fails the run; a
// gate that measured nothing fails rather than passes.
//
// Latency and lag gate values are histogram quantiles reported as the upper
// bound of the bucket holding them: at most 1/32 (+3.2%) above the true
// value, never below it. A true p95 just under a threshold (for example
// 295–300 ms against 300 ms) can read as up to ~303 ms and fail; the error is
// always on the strict side. Samples above the histogram's range (about
// 134 s) are clamped, so any overflow fails the run outright.
func Evaluate(s RunSummary, t Thresholds) Verdict { return evaluate(s, t, true) }

// EvaluateWindow judges one window of a run (a ramp step, a storm or
// restart window) or a run before its audit: the latency, unexpected-ratio,
// generator-health and live-loss gates only. The whole-run gates (the
// audit's acknowledged writes, tracked devices, lost, cross-family and
// diverged changes, and the host) belong to the run, not to a window: a
// window passing EvaluateWindow says nothing about them.
func EvaluateWindow(s RunSummary, t Thresholds) Verdict { return evaluate(s, t, false) }

func evaluate(s RunSummary, t Thresholds, whole bool) Verdict {
	v := Verdict{Reasons: []string{}, Gates: map[string]string{}}
	fail := func(format string, args ...any) { v.Reasons = append(v.Reasons, fmt.Sprintf(format, args...)) }

	// A live-only run holds every class but the sockets at 0: a gate that
	// measured nothing there is not applicable, not failed. A gate that did
	// measure something (a lapsed token's refresh) is judged as usual.
	liveOnly := s.ModelOverride == ModelLiveOnly
	const notApplicable = "n/a (live-only: the class is held at 0)"
	if liveOnly {
		v.ModelOverride = ModelLiveOnly
		v.Gates["model_override"] = "live-only: the sockets alone; not the spec's PASS"
		v.Gates["open_sockets"] = fmt.Sprintf("%.1f mean / >0", s.OpenSocketsMean)
		if !(s.OpenSocketsMean > 0) {
			fail("live-only: no live socket was open")
		}
	}
	latencyGate := func(name string, h Histogram, limit time.Duration) {
		gate := strings.ReplaceAll(name, " ", "_")
		if h.Total == 0 && liveOnly {
			v.Gates[gate] = notApplicable
			return
		}
		if h.Total == 0 {
			v.Gates[gate] = "no samples / " + limit.String()
			fail("%s: no samples", name)
			return
		}
		got := h.Quantile(0.95)
		v.Gates[gate] = got.String() + " / " + limit.String()
		if got > limit {
			fail("%s %v > %v (%d requests)", name, got, limit, h.Total)
		}
	}
	var sync Histogram
	for _, r := range SyncRoutes {
		if rs, ok := s.Routes[r]; ok {
			sync.Merge(&rs.Latency)
		}
	}
	latencyGate("sync p95", sync, t.SyncP95)
	latencyGate(RouteRefresh+" p95", s.Routes[RouteRefresh].Latency, t.RefreshP95)
	latencyGate(RouteRefreshAck+" p95", s.Routes[RouteRefreshAck].Latency, t.RefreshP95)

	// Overflowed samples are clamped to the histogram's top, so a quantile
	// could under-state them: any overflow, on any route, fails the run.
	var overflow uint64
	var overflowed []string
	for _, route := range slices.Sorted(maps.Keys(s.Routes)) {
		if n := s.Routes[route].Latency.Overflow; n > 0 {
			overflow += n
			overflowed = append(overflowed, fmt.Sprintf("%s %d", route, n))
		}
	}
	if n := s.SchedulerLag.Overflow; n > 0 {
		overflow += n
		overflowed = append(overflowed, fmt.Sprintf("scheduler lag %d", n))
	}
	v.Gates["latency_overflow"] = fmt.Sprintf("%d / 0", overflow)
	if overflow > 0 {
		fail("latency overflow: samples above the histogram's %v range: %s", time.Duration(histMaxMicros)*time.Microsecond, strings.Join(overflowed, ", "))
	}

	var all, unexpected, expected, saturated uint64
	for _, rs := range s.Routes {
		for outcome, n := range rs.Outcomes {
			all += n
			switch {
			case outcome == OutcomeOK:
			case expectedOutcome(outcome):
				expected += n
			default:
				unexpected += n
				if saturationOutcome(outcome) {
					saturated += n
				}
			}
		}
	}
	v.Gates["expected_outcomes"] = fmt.Sprintf("%d (restart window, lost races) / not counted", expected)
	v.Gates["saturation"] = fmt.Sprintf("%d (server_busy, server_timeout) / counted as unexpected", saturated)
	limitPct := fmt.Sprintf("<%.2f%%", t.MaxUnexpectedRatio*100)
	if all == 0 && liveOnly {
		// A live-only step whose sockets all opened before it sends nothing.
		v.Gates["unexpected_ratio"] = "n/a (live-only: no request in the span, the sockets were already open)"
	} else if all == 0 {
		v.Gates["unexpected_ratio"] = "no requests / " + limitPct
		fail("no requests recorded")
	} else {
		ratio := float64(unexpected) / float64(all)
		v.Gates["unexpected_ratio"] = fmt.Sprintf("%.4f%% (%d/%d) / %s", ratio*100, unexpected, all, limitPct)
		if ratio >= t.MaxUnexpectedRatio {
			fail("unexpected %.2f%% (%d of %d requests), want under %.2f%%", ratio*100, unexpected, all, t.MaxUnexpectedRatio*100)
		}
	}

	if s.SchedulerLag.Total == 0 && liveOnly {
		v.Gates["scheduler_lag_p99"] = notApplicable
	} else if s.SchedulerLag.Total == 0 {
		v.Gates["scheduler_lag_p99"] = "no samples / " + t.MaxSchedulerLagP99.String()
		fail("scheduler lag p99: no samples")
	} else {
		lag := s.SchedulerLag.Quantile(0.99)
		v.Gates["scheduler_lag_p99"] = lag.String() + " / " + t.MaxSchedulerLagP99.String()
		if lag > t.MaxSchedulerLagP99 {
			fail("scheduler lag p99 %v > %v: the generator, not the server, is the bottleneck", lag, t.MaxSchedulerLagP99)
		}
	}
	v.Gates["dropped_arrivals"] = fmt.Sprintf("%d / 0", s.Dropped)
	if s.Dropped > 0 {
		fail("generator saturated: %d dropped arrivals", s.Dropped)
	}

	if s.LaunchedOps > 0 {
		share := float64(s.SkippedOps) / float64(s.LaunchedOps)
		v.Gates["skipped_ops"] = fmt.Sprintf("%.4f%% (%d/%d) / <%.2f%%", share*100, s.SkippedOps, s.LaunchedOps, maxSkippedShare*100)
		if share >= maxSkippedShare {
			fail("generator skipped %d of %d operations (%.2f%%): the offered load fell short of the model", s.SkippedOps, s.LaunchedOps, share*100)
		}
	}
	v.Gates["ledger_errors"] = fmt.Sprintf("%d / 0", s.LedgerErrors)
	if s.LedgerErrors > 0 {
		fail("ledger: %d acknowledged writes could not be recorded", s.LedgerErrors)
	}
	v.Gates["tracked_unseeded"] = fmt.Sprintf("%d / 0", s.TrackedUnseeded)
	if s.TrackedUnseeded > 0 {
		fail("tracked devices unseeded %d: their maps never completed, so the audit cannot check them", s.TrackedUnseeded)
	}
	v.Gates["unexpected_live_losses"] = fmt.Sprintf("%d / 0", s.UnexpectedLiveLosses)
	if s.UnexpectedLiveLosses != 0 {
		fail("unexpected live losses %d", s.UnexpectedLiveLosses)
	}
	if !whole {
		v.Pass = len(v.Reasons) == 0
		return v
	}

	a := s.Audit
	v.Gates["acknowledged"] = fmt.Sprintf("%d entities (%d writes) / >0", a.Acknowledged, a.AcknowledgedWrites)
	switch {
	case a.Acknowledged == 0 && liveOnly:
		v.Gates["acknowledged"] = notApplicable
	case a.Acknowledged == 0:
		fail("audit vacuous: no acknowledged writes")
	}
	v.Gates["tracked_devices"] = fmt.Sprintf("%d / >0", a.TrackedDevices)
	switch {
	case a.TrackedDevices == 0 && liveOnly:
		v.Gates["tracked_devices"] = notApplicable
	case a.TrackedDevices == 0:
		fail("tracked devices vacuous: no device was tracked")
	}
	for _, c := range []struct {
		gate, label string
		n           int
	}{
		{"lost", "lost", a.Lost},
		{"cross_family", "cross-family", a.CrossFamily},
		{"diverged", "diverged", a.Diverged},
		{"tracked_diverged", "tracked devices diverged", a.TrackedDiverged},
	} {
		v.Gates[c.gate] = fmt.Sprintf("%d / 0", c.n)
		if c.n != 0 {
			fail("%s %d", c.label, c.n)
		}
	}

	v.Gates["host"] = fmt.Sprintf("contaminated=%v (max load1 %.2f) / contaminated=false", s.Host.Contaminated, s.Host.MaxLoad1)
	if s.Host.Contaminated {
		if len(s.Host.Offenders) == 0 {
			fail("host contaminated")
		} else {
			fail("host contaminated: %s", strings.Join(s.Host.Offenders, ", "))
		}
	}

	v.Pass = len(v.Reasons) == 0
	return v
}
