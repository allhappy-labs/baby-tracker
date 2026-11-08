package loadtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RunConfig is one load run (`baby-tracker-load run`).
type RunConfig struct {
	Base, Host                  string // http://172.31.250.10:8080, sync.load.test
	Fleet, Ledger, Tracked, Out string // private fleet, ledger and tracked files; the public result.json
	// WindowStart is the file written when warm-up ends. Empty: window-start
	// beside Out (the runner's $RUN/window-start).
	WindowStart string

	Scenario string
	Load     float64
	Steps    []float64 // ramp only

	Warmup, Measure, StepDuration time.Duration
	Seed                          uint64
	TrackedDevices                int // 200

	// Offsets from window-start.
	StormAt, RestartAt, RestartGrace time.Duration

	WithPump   bool // kind 10 writes; the server does not advertise kinds, so the flag decides
	Thresholds Thresholds

	// ModelOverride reshapes the model: "" (the model as it is) or
	// ModelLiveOnly (every class but the live sockets at 0).
	ModelOverride string

	// Maintenance is "" or MaintenanceHot (soak only): one family purge
	// and one export from window-start + MaintenanceAt (default a third of
	// the measurement). Superuser is the disposable stack's superuser pool
	// the purge's recovery window is advanced through (the caller checks
	// the stack's marker); maintenanceDB replaces it in tests.
	Maintenance   string
	MaintenanceAt time.Duration

	// Flood is the flood scenario's attacks: FloodIP, FloodBearer or
	// FloodBoth ("" is both), so each one's cost can be measured alone.
	Flood         string
	Superuser     *pgxpool.Pool
	maintenanceDB maintenanceDB

	// ProfileDir, when set, is where the generator writes its own CPU,
	// heap, allocation, contention and goroutine profiles and its lag
	// traces (genprofile.go); a relative directory is beside Out (the
	// runner's --profile-dir profiles: $RUN/profiles).
	ProfileDir string

	// model replaces Model10k (tests). Nil: Model10k sized to the fleet
	// (Families/10,000) and scaled by the load.
	model *Model
}

// Run-wide constants (the plan's values).
const (
	maxInFlight        = 20000
	drainLimit         = 15 * time.Second
	checkpointInterval = 60 * time.Second
	lineageAge         = 30 * 24 * time.Hour // hosted sessions
	modelFamilies      = 10000
	stormWindow        = 2 * time.Minute // the storm's declared window: the iOS backoff ceiling (60 s) twice
)

// ModelLiveOnly is the --model-override that holds only the model's live
// sockets (Task 14's per-connection memory cell): no pulls, writes,
// snapshots, MCP, push-token or Live Activity calls, no refresh schedule and
// no tracked devices. A live device refreshes only when its access lapses
// before it dials (as the iOS client refreshes before it connects), so the
// sockets outlive the 10-minute token. The verdict marks the gates of the
// zeroed classes that measured nothing as not applicable (evaluate).
const ModelLiveOnly = "live-only"

// liveOnlyScenarios are the scenarios a live-only run may take: the ones
// that add nothing but sockets to the background.
var liveOnlyScenarios = map[string]bool{"soak": true, "ramp": true, "storm": true, "restart": true}

// RunResult is result.json: the run's summary (the fields Evaluate reads,
// inline, so the audit can decode the file as a RunSummary, fold its
// AuditResult in and evaluate again) plus everything a report needs.
type RunResult struct {
	RunSummary

	Scenario       string                  `json:"scenario"`
	Load           float64                 `json:"load"`
	Model          Model                   `json:"model"`
	Thresholds     ThresholdsJSON          `json:"thresholds"`
	Seed           uint64                  `json:"seed"`
	Devices        int                     `json:"devices"`
	Families       int                     `json:"families"`
	LiveDevices    int                     `json:"live_devices"`
	TrackedDevices int                     `json:"tracked_devices"`
	StartedAt      time.Time               `json:"started_at"`
	WindowStart    time.Time               `json:"window_start"`
	MeasureEnd     time.Time               `json:"measure_end"`
	EndedAt        time.Time               `json:"ended_at"`
	Launched       map[string]uint64       `json:"launched"`
	Skipped        map[string]uint64       `json:"skipped"`
	WarmupRoutes   map[string]RouteSummary `json:"warmup_routes"`
	DrainRoutes    map[string]RouteSummary `json:"drain_routes"`
	AttackRoutes   map[string]RouteSummary `json:"attack_routes,omitempty"`
	WarmupLiveLoss int                     `json:"warmup_live_losses"`
	// LiveLossDetails is the measurement's first maxLiveLossDetails
	// unexpected live losses, each as the device saw it.
	LiveLossDetails []LiveLoss `json:"live_loss_details,omitempty"`
	PlannedRenewals int64      `json:"planned_live_renewals"`
	LedgerEntries   int64      `json:"ledger_entries"`

	// WarmupRefreshLaunched is the refresh timers the schedule launched
	// before window-start; Launched["refresh"] counts the measurement's.
	WarmupRefreshLaunched uint64 `json:"warmup_refresh_launched"`

	// LiveOpenMin and LiveOpenMean are the sockets actually open over the
	// measurement (sampled every second), against LiveDevices designated.
	LiveOpenMin  int     `json:"live_open_min"`
	LiveOpenMean float64 `json:"live_open_mean"`
	// RefreshEffectivePerSecond is the refresh rate the schedule offered at
	// --load (the first step's, for a ramp): the model's, unless the access
	// token's lifetime bounds the period (RefreshPeriodSeconds).
	RefreshEffectivePerSecond float64 `json:"refresh_effective_per_second"`
	RefreshPeriodSeconds      float64 `json:"refresh_period_seconds"`
	// HubWakeDelay is the time from a write's acknowledgement to the live
	// hint at its family's other live devices (Task 18).
	HubWakeDelay *HubWakeDelay `json:"hub_wake_delay,omitempty"`
	// Cells are the scenario cells' own figures (hot-family, import).
	Cells   map[string]any `json:"cells,omitempty"`
	Steps   []WindowResult `json:"steps,omitempty"`
	Windows []WindowResult `json:"windows,omitempty"`
	Storm   *StormResult   `json:"storm,omitempty"`
	// SchedulerLagByClass is the measurement's release lag per class (the
	// scheduler-lag gate merges every class); GeneratorRuntime the
	// generator process over the measurement (CPU, GC, Go scheduling
	// latency); GeneratorProfile what --profile-dir wrote.
	SchedulerLagByClass map[string]LagSummary `json:"scheduler_lag_by_class,omitempty"`
	// SessionWaitByClass is the measurement's waits for a device's session
	// (its refresh serialised with its own requests) per class.
	SessionWaitByClass map[string]SessionWaitSummary `json:"session_wait_by_class,omitempty"`
	GeneratorRuntime   *GeneratorRuntime             `json:"generator_runtime,omitempty"`
	GeneratorProfile   *GeneratorProfile             `json:"generator_profile,omitempty"`
	// GeneratorVerdict judges what the generator can see (EvaluateWindow):
	// latency, unexpected outcomes, its own health and live losses. The run
	// verdict (Evaluate) needs the audit (Task 11), which folds into this
	// file afterwards.
	GeneratorVerdict Verdict  `json:"generator_verdict"`
	Notes            []string `json:"notes"`
}

// ThresholdsJSON is Thresholds in thresholds.json's units.
type ThresholdsJSON struct {
	SyncP95Ms            float64 `json:"sync_p95_ms"`
	RefreshP95Ms         float64 `json:"refresh_p95_ms"`
	MaxSchedulerLagP99Ms float64 `json:"max_scheduler_lag_p99_ms"`
	MaxUnexpectedRatio   float64 `json:"max_unexpected_ratio"`
}

func thresholdsJSON(t Thresholds) ThresholdsJSON {
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	return ThresholdsJSON{SyncP95Ms: ms(t.SyncP95), RefreshP95Ms: ms(t.RefreshP95), MaxSchedulerLagP99Ms: ms(t.MaxSchedulerLagP99),
		MaxUnexpectedRatio: t.MaxUnexpectedRatio}
}

// WindowResult is one step (ramp) or window (storm, restart).
type WindowResult struct {
	Name string  `json:"name"`
	Load float64 `json:"load"`
	// RefreshEffectivePerSecond is a ramp step's refresh schedule rate.
	RefreshEffectivePerSecond float64 `json:"refresh_effective_per_second,omitempty"`
	// LiveSockets is the model's sockets at the window's load; LiveOpenMin
	// and LiveOpenMean the sockets actually open inside it (sampled every
	// second): a live-only ramp's per-level socket counts.
	LiveSockets  int     `json:"live_sockets"`
	LiveOpenMin  int     `json:"live_open_min"`
	LiveOpenMean float64 `json:"live_open_mean"`
	// Live-only: the sockets from SettledFrom on, once the window's ramp-in
	// (its new sockets' 30 s, or a quarter of the step) is over; the
	// per-level count for Δheap/Δconnections. LiveOpenMin and
	// LiveOpenMean include the ramp-in.
	SettledFrom         time.Time  `json:"settled_from,omitzero"`
	LiveOpenMinSettled  int        `json:"live_open_min_settled,omitempty"`
	LiveOpenMeanSettled float64    `json:"live_open_mean_settled,omitempty"`
	From                time.Time  `json:"from"`
	To                  time.Time  `json:"to"`
	Summary             RunSummary `json:"summary"`
	Verdict             Verdict    `json:"verdict"`
}

// Run drives the fleet through cfg's scenario and writes result.json, the
// ledger, tracked.jsonl and the checkpointed fleet. A run that completes
// returns nil, PASS or not: the verdict is in the result.
func Run(ctx context.Context, cfg RunConfig) (RunSummary, error) {
	result, err := runScenario(ctx, cfg)
	return result.RunSummary, err
}

// generator is one run's state.
type generator struct {
	cfg     RunConfig
	header  FleetHeader
	devices []*device
	writers []*device // owners and caregivers
	mcp     []*device // devices holding an MCP credential
	// pullers and bgWriters are the background pull (and snapshot) and
	// write classes' devices: every device and writer, less any a scenario
	// keeps for itself (hot-family).
	pullers, bgWriters []*device
	accounts           map[string]*accountState
	families           map[string]*familyState
	c                  *client
	wsHTTP             *http.Client
	rng                *lockedRand
	ledger             *LedgerWriter
	phases             *phases
	work               context.Context // every operation's context: ends after the drain

	countMu    sync.Mutex
	launched   map[string]uint64
	skipped    map[string]uint64
	liveLosses atomic.Int64
	lossMu     sync.Mutex
	lossDetail []LiveLoss
	warmLosses atomic.Int64
	renewals   atomic.Int64
	ledgered   atomic.Int64
	attack     *Recorder
	liveWG     sync.WaitGroup
	bg         sync.WaitGroup // every operation no dispatcher owns
	ledgerErrs atomic.Int64
	liveOrder  []*device
	ttlNanos   atomic.Int64
	withPump   bool
	modelBase  Model
	notesMu    sync.Mutex
	notes      []string
	cells      map[string]any
	refreshMu  sync.Mutex
	refreshes  []refreshRate // the refresh schedule's rate, then each retime's
	schedule   *refreshSchedule
	excluded   map[*device]bool // devices the scenario keeps out of the background (hot-family)
	hot        *hotFamily
	closeMu    sync.Mutex
	closing    bool // the final waits have begun: no operation starts
	liveOpen   liveSamples
	wake       hubWake // the hub-wake delay (wakedelay.go)
	// maintenance is the hot maintenance cell's plan (maintenance.go).
	maintenance *hotMaintenance

	// refreshLaunched and refreshWarm count the schedule's launches by
	// intended time: the measurement's, and the warm-up's.
	refreshLaunched, refreshWarm atomic.Uint64

	lags classLags // the measurement's release lag per class
	// sessionWaits are the measurement's waits for a device's session.
	sessionWaits sessionWaits
	profile      *generatorProfile // --profile-dir; nil without
}

type refreshRate struct {
	load          float64
	perSecond     float64
	periodSeconds float64
}

// liveSamples are the open-socket counts sampled over the measurement
// (or a window).
type liveSamples struct {
	mu    sync.Mutex
	min   int
	sum   int64
	count int64
}

// add files one sample.
func (l *liveSamples) add(open int) {
	l.mu.Lock()
	if l.count == 0 || open < l.min {
		l.min = open
	}
	l.sum += int64(open)
	l.count++
	l.mu.Unlock()
}

// read is the samples' minimum and mean (0, 0 without a sample).
func (l *liveSamples) read() (int, float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.count == 0 {
		return 0, 0
	}
	return l.min, float64(l.sum) / float64(l.count)
}

// startClosing marks the final waits: goBG and new readers refuse.
func (g *generator) startClosing() {
	g.closeMu.Lock()
	g.closing = true
	g.closeMu.Unlock()
}

// addReader counts a live reader, unless the run is closing.
func (g *generator) addReader() bool {
	g.closeMu.Lock()
	defer g.closeMu.Unlock()
	if g.closing {
		return false
	}
	g.liveWG.Add(1)
	return true
}

// noteRefreshRate records the refresh schedule's rate at load.
func (g *generator) noteRefreshRate(load, perSecond float64, period time.Duration) {
	g.refreshMu.Lock()
	g.refreshes = append(g.refreshes, refreshRate{load: load, perSecond: perSecond, periodSeconds: period.Seconds()})
	g.refreshMu.Unlock()
}

func (g *generator) note(format string, args ...any) {
	g.notesMu.Lock()
	g.notes = append(g.notes, fmt.Sprintf(format, args...))
	g.notesMu.Unlock()
}

func (g *generator) cell(name string, value any) {
	g.notesMu.Lock()
	if g.cells == nil {
		g.cells = map[string]any{}
	}
	g.cells[name] = value
	g.notesMu.Unlock()
}

// goBG runs f as a background operation the end of the run waits for.
// Once the final waits have begun (closing) nothing starts: a reader that
// sees a last hint, or a dial loop that ends, adds nothing to a WaitGroup
// being waited on.
func (g *generator) goBG(f func()) {
	g.closeMu.Lock()
	defer g.closeMu.Unlock()
	if g.closing {
		return
	}
	g.bg.Add(1)
	go func() {
		defer g.bg.Done()
		f()
	}()
}

// accountState holds an account's snapshot sessions (each active for the
// server's snapshot TTL, finished or not).
type accountState struct {
	mu        sync.Mutex
	snapshots []time.Time // expiries
}

const snapshotTTL = 15 * time.Minute

// tryStartSnapshot takes a snapshot slot when the account has none active.
func (a *accountState) tryStartSnapshot(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	live := a.snapshots[:0]
	for _, expiry := range a.snapshots {
		if expiry.After(now) {
			live = append(live, expiry)
		}
	}
	a.snapshots = live
	if len(live) > 0 {
		return false
	}
	a.snapshots = append(a.snapshots, now.Add(snapshotTTL))
	return true
}

// phases routes every recorded request by its intended start: warm-up,
// the measurement window, or the drain after it; inside the window, also
// into every declared window (ramp steps, storm and restart windows).
type phases struct {
	windowStart time.Time
	measureEnd  atomic.Int64 // UnixNano; 0 until known
	warm        *Recorder
	measure     *Recorder
	drain       *Recorder
	health      health // the measurement's generator health, by intended start

	mu      sync.RWMutex
	windows []*windowRecorder

	restartFrom, restartTo time.Time // zero: no restart window
	stormFrom, stormTo     time.Time // zero: no storm window
}

// windowRecorder is one declared window: its requests (by intended
// start) and its own generator health: the lag and drops of the arrivals
// intended in it and the live sockets lost in it.
type windowRecorder struct {
	name     string
	load     float64
	from, to time.Time
	rec      *Recorder
	health   health
	liveOpen liveSamples // the open sockets sampled inside the window
	// settled is liveOpen from settleAt on: a live-only ramp step's sockets
	// once its ramp-in (liveOnlyRampIn) is over.
	settled  liveSamples
	settleAt time.Time
}

// health is a span's generator health, every figure by intended start:
// the lag and drops of its arrivals, the operations launched and those
// skipped, and the sockets lost in it.
type health struct {
	mu       sync.Mutex
	lag      Histogram
	dropped  uint64
	launched uint64
	skipped  uint64
	losses   int
}

func (h *health) observe(lag time.Duration, dropped bool) {
	h.mu.Lock()
	h.lag.Record(lag)
	if dropped {
		h.dropped++
	} else {
		h.launched++
	}
	h.mu.Unlock()
}

func (h *health) skip() {
	h.mu.Lock()
	h.skipped++
	h.mu.Unlock()
}

func (h *health) lose() {
	h.mu.Lock()
	h.losses++
	h.mu.Unlock()
}

// into sets s's generator-health fields from h.
func (h *health) into(s *RunSummary) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s.SchedulerLag, s.Dropped, s.LaunchedOps, s.SkippedOps, s.UnexpectedLiveLosses = h.lag, h.dropped, h.launched, h.skipped, h.losses
}

func (w *windowRecorder) holds(t time.Time) bool { return !t.Before(w.from) && t.Before(w.to) }

// each runs f on the measurement's health and every window's holding t,
// when t falls in the measurement.
func (p *phases) each(t time.Time, f func(*health)) {
	if !p.inMeasure(t) {
		return
	}
	f(&p.health)
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, w := range p.windows {
		if w.holds(t) {
			f(&w.health)
		}
	}
}

// observeRefresh is the refresh schedule's Observe: phases.observe, and the
// launch counted by its intended time, so Launched["refresh"] is the
// measurement's, as for every other class.
func (g *generator) observeRefresh(intended time.Time, lag time.Duration, dropped bool) {
	g.phases.observe(intended, lag, dropped)
	switch {
	case dropped:
	case g.phases.inMeasure(intended):
		g.refreshLaunched.Add(1)
	case intended.Before(g.phases.windowStart):
		g.refreshWarm.Add(1)
	}
}

// observe files one released arrival's lag, and its launch or drop, by its
// intended start.
func (p *phases) observe(intended time.Time, lag time.Duration, dropped bool) {
	p.each(intended, func(h *health) { h.observe(lag, dropped) })
}

// skipped files an operation launched at intended that the generator
// could not carry out.
func (p *phases) skipped(intended time.Time) { p.each(intended, (*health).skip) }

// lose files an unexpected live loss at t.
func (p *phases) lose(t time.Time) { p.each(t, (*health).lose) }

// declared reports whether t falls in a declared window (storm or
// restart): a socket lost there is not an unexpected loss.
func (p *phases) declared(t time.Time) bool {
	in := func(from, to time.Time) bool { return !from.IsZero() && !t.Before(from) && !t.After(to) }
	return in(p.restartFrom, p.restartTo) || in(p.stormFrom, p.stormTo)
}

func newPhases(windowStart time.Time) *phases {
	return &phases{windowStart: windowStart, warm: NewRecorder(), measure: NewRecorder(), drain: NewRecorder()}
}

func (p *phases) setMeasureEnd(t time.Time) { p.measureEnd.Store(t.UnixNano()) }

func (p *phases) measureEndTime() time.Time {
	if n := p.measureEnd.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

func (p *phases) addWindow(name string, load float64, from, to time.Time) *windowRecorder {
	w := &windowRecorder{name: name, load: load, from: from, to: to, rec: NewRecorder(), settleAt: from}
	p.mu.Lock()
	p.windows = append(p.windows, w)
	p.mu.Unlock()
	return w
}

// inMeasure reports whether t falls in the measurement window.
func (p *phases) inMeasure(t time.Time) bool {
	end := p.measureEndTime()
	return !t.Before(p.windowStart) && (end.IsZero() || t.Before(end))
}

// inRestart reports whether a request intended at intended and finishing
// now overlaps the declared restart window.
func (p *phases) inRestart(intended, now time.Time) bool {
	return !p.restartFrom.IsZero() && !now.Before(p.restartFrom) && !intended.After(p.restartTo)
}

// record files one finished request.
func (g *generator) record(route string, intended time.Time, outcome string, rejections ...string) {
	p := g.phases
	if outcome != OutcomeOK && p.inRestart(intended, time.Now()) {
		outcome = OutcomeRestartWindow
	}
	// Reasons reconcile with the outcomes: only a request recorded
	// rejected_mutation files its refused mutations' reasons.
	if outcome != OutcomeRejected {
		rejections = nil
	}
	switch {
	case intended.Before(p.windowStart):
		p.warm.Done(route, intended, outcome, rejections...)
		return
	case !p.inMeasure(intended):
		p.drain.Done(route, intended, outcome, rejections...)
		return
	}
	p.measure.Done(route, intended, outcome, rejections...)
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, w := range p.windows {
		if w.holds(intended) {
			w.rec.Done(route, intended, outcome, rejections...)
		}
	}
}

// LiveLoss is one unexpected live loss: when, the close's code (-1: no
// close frame) and reason, the socket's age, the time since the device's
// last refresh (0: none) and whether a refresh was in flight.
type LiveLoss struct {
	At                  time.Time `json:"at"`
	Code                int       `json:"code"`
	Reason              string    `json:"reason"`
	UpSeconds           float64   `json:"up_seconds"`
	SinceRefreshSeconds float64   `json:"since_refresh_seconds"`
	Refreshing          bool      `json:"refreshing"`
}

// maxLiveLossDetails bounds RunResult.LiveLossDetails.
const maxLiveLossDetails = 64

func (g *generator) noteLiveLoss(l LiveLoss) {
	g.lossMu.Lock()
	if len(g.lossDetail) < maxLiveLossDetails {
		g.lossDetail = append(g.lossDetail, l)
	}
	g.lossMu.Unlock()
}

// sinceOrZero is now-t in seconds, or 0 for a zero t.
func sinceOrZero(now, t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return now.Sub(t).Seconds()
}

func (g *generator) count(m map[string]uint64, class string, n uint64) {
	g.countMu.Lock()
	m[class] += n
	g.countMu.Unlock()
}

// skip counts an operation intended at intended that the generator could
// not carry out: by class for the report, and (except a scenario's surplus
// arrivals) in the skipped gate of the spans holding intended.
func (g *generator) skip(class string, intended time.Time) {
	g.count(g.skipped, class, 1)
	if class != skipSurplus {
		g.phases.skipped(intended)
	}
}

// randomDevice is a uniformly drawn device of list.
func (g *generator) randomDevice(list []*device) *device {
	if len(list) == 0 {
		return nil
	}
	return list[g.rng.IntN(len(list))]
}

// modelAt is the offered load at load: Model10k sized to the fleet
// (Families/10,000 of it) and scaled by load.
func (g *generator) modelAt(load float64) Model { return g.modelBase.Scale(load) }

func newGenerator(cfg RunConfig, header FleetHeader, states []DeviceState) *generator {
	g := &generator{cfg: cfg, header: header, accounts: map[string]*accountState{}, families: map[string]*familyState{},
		c: newClient(cfg.Base, cfg.Host), rng: newLockedRand(cfg.Seed, 0x9e1),
		launched: map[string]uint64{}, skipped: map[string]uint64{}, attack: NewRecorder(), withPump: cfg.WithPump}
	g.wsHTTP = &http.Client{Transport: newTransport(), Timeout: requestTimeout}
	for i := range states {
		d := &device{DeviceState: states[i], index: i}
		g.devices = append(g.devices, d)
		if d.Role != "viewer" {
			g.writers = append(g.writers, d)
		}
		if d.MCPCredential != "" && len(d.BabyIDs) > 0 {
			g.mcp = append(g.mcp, d)
		}
		if g.accounts[d.AccountID] == nil {
			g.accounts[d.AccountID] = &accountState{}
		}
		if g.families[d.FamilyID] == nil {
			g.families[d.FamilyID] = newFamilyState()
		}
	}
	g.pullers, g.bgWriters = g.devices, g.writers
	g.modelBase = g.baseModel(0)
	return g
}

// baseModel is the model at 1x: Model10k sized to the fleet's families
// (Families/10,000 of it), less the seeded hot family (the hot-family
// cell's) and the parked ones (the hot maintenance's purge family); a
// test's own model as it is; live-only, its sockets alone.
func (g *generator) baseModel(parked int) Model {
	m := Model{}
	if g.cfg.model != nil {
		m = *g.cfg.model
	} else {
		families := g.header.Families - parked
		if g.header.HotFamilyID != "" {
			families--
		}
		m = Model10k().Scale(float64(families) / modelFamilies)
	}
	if g.liveOnly() {
		m = Model{LiveSockets: m.LiveSockets}
	}
	return m
}

// liveOnly reports a --model-override live-only run.
func (g *generator) liveOnly() bool { return g.cfg.ModelOverride == ModelLiveOnly }

// defaults fills unset options with the plan's values.
func (cfg *RunConfig) defaults() {
	if cfg.TrackedDevices == 0 {
		cfg.TrackedDevices = 200
	}
	if cfg.ModelOverride == ModelLiveOnly {
		// A tracked device pulls (its seed and its final pull): none here.
		cfg.TrackedDevices = 0
	}
	if cfg.Thresholds == (Thresholds{}) {
		cfg.Thresholds = SpecThresholds()
	}
	if cfg.WindowStart == "" && cfg.Out != "" {
		cfg.WindowStart = filepath.Join(filepath.Dir(cfg.Out), "window-start")
	}
	if cfg.StepDuration == 0 {
		cfg.StepDuration = 10 * time.Minute
	}
	if cfg.RestartGrace == 0 {
		cfg.RestartGrace = 60 * time.Second
	}
	if cfg.Maintenance == MaintenanceHot && cfg.MaintenanceAt == 0 {
		cfg.MaintenanceAt = cfg.Measure / 3
	}
}

func (cfg RunConfig) validate() error {
	switch {
	case cfg.Base == "" || cfg.Host == "":
		return errors.New("run: --base and --host are required")
	case cfg.Fleet == "" || cfg.Ledger == "" || cfg.Tracked == "" || cfg.Out == "":
		return errors.New("run: --fleet, --ledger, --tracked and --out are required")
	case cfg.Measure <= 0 || cfg.Warmup < 0:
		return errors.New("run: --measure must be positive and --warmup not negative")
	case !(cfg.Load > 0) || math.IsInf(cfg.Load, 0):
		return errors.New("run: --load must be a positive number")
	case cfg.TrackedDevices < 0:
		return errors.New("run: --tracked-devices must not be negative")
	}
	if cfg.Scenario == "ramp" {
		if err := checkRisingSteps(cfg.Steps); err != nil {
			return err
		}
	}
	if _, ok := scenarios[cfg.Scenario]; !ok {
		return fmt.Errorf("run: unknown scenario %q", cfg.Scenario)
	}
	switch cfg.Maintenance {
	case "":
	case MaintenanceHot:
		if cfg.Scenario != "soak" || cfg.ModelOverride != "" {
			return fmt.Errorf("run: --maintenance %s runs only in a soak with the full model", MaintenanceHot)
		}
		if cfg.Superuser == nil && cfg.maintenanceDB == nil {
			return fmt.Errorf("run: --maintenance %s needs the stack's superuser (--superuser-url-file)", MaintenanceHot)
		}
		if cfg.MaintenanceAt <= 0 || cfg.MaintenanceAt >= cfg.Measure {
			return fmt.Errorf("run: --maintenance-at must fall inside the measurement")
		}
	default:
		return fmt.Errorf("run: unknown --maintenance %q (want %s)", cfg.Maintenance, MaintenanceHot)
	}
	switch cfg.Flood {
	case "", FloodIP, FloodBearer, FloodBoth:
		if cfg.Flood != "" && cfg.Scenario != "flood" {
			return fmt.Errorf("run: --flood takes only the flood scenario, not %q", cfg.Scenario)
		}
	default:
		return fmt.Errorf("run: unknown --flood %q (want %s, %s or %s)", cfg.Flood, FloodIP, FloodBearer, FloodBoth)
	}
	switch cfg.ModelOverride {
	case "":
	case ModelLiveOnly:
		if !liveOnlyScenarios[cfg.Scenario] {
			return fmt.Errorf("run: --model-override %s takes only soak, ramp, storm or restart, not %q (its cell adds other classes)",
				ModelLiveOnly, cfg.Scenario)
		}
	default:
		return fmt.Errorf("run: unknown --model-override %q (want %s)", cfg.ModelOverride, ModelLiveOnly)
	}
	return nil
}

// checkRisingSteps refuses a ramp step lower than the one before: the
// refresh schedule's retime (setRate) only keeps every device inside its
// token's lifetime when the rate rises.
func checkRisingSteps(steps []float64) error {
	for i := 1; i < len(steps); i++ {
		if steps[i] < steps[i-1] {
			return fmt.Errorf("run: --steps must not fall: step %d (%g) is below step %d (%g)", i+1, steps[i], i, steps[i-1])
		}
	}
	return nil
}

// runScenario is Run, returning the whole result.
func runScenario(ctx context.Context, cfg RunConfig) (RunResult, error) {
	cfg.defaults()
	if err := cfg.validate(); err != nil {
		return RunResult{}, err
	}
	header, states, err := ReadFleet(cfg.Fleet)
	if err != nil {
		return RunResult{}, err
	}
	if err := header.CheckFresh(time.Now(), lineageAge); err != nil {
		return RunResult{}, err
	}
	if len(states) == 0 {
		return RunResult{}, errors.New("run: the fleet holds no device")
	}
	sc := scenarios[cfg.Scenario]
	var purgeFamily string
	var parked []DeviceState
	if cfg.Maintenance == MaintenanceHot {
		if purgeFamily, parked, states, err = splitMaintenance(header, states); err != nil {
			return RunResult{}, err
		}
	}
	g := newGenerator(cfg, header, states)
	if parked != nil {
		if err := g.planMaintenance(purgeFamily, parked); err != nil {
			return RunResult{}, err
		}
		g.modelBase = g.baseModel(1)
	}
	loads := sc.loads(cfg)
	if len(loads) == 0 {
		return RunResult{}, errors.New("run: the scenario has no load (ramp needs --steps)")
	}
	maxSockets := 0
	for _, load := range loads {
		m := g.modelAt(load)
		if err := m.Validate(len(g.devices), len(g.mcp)); err != nil {
			return RunResult{}, fmt.Errorf("run: the model at %.3gx does not fit the fleet: %w", load, err)
		}
		maxSockets = max(maxSockets, m.LiveSockets)
	}
	if sc.exclude != nil {
		sc.exclude(g)
	}
	g.liveOrder = designate(g.devices, g.excluded, cfg.Seed, maxSockets, g.modelAt(loads[0]).LiveSockets, cfg.TrackedDevices)
	ledger, err := CreateLedger(cfg.Ledger)
	if err != nil {
		return RunResult{}, fmt.Errorf("run: ledger: %w", err)
	}
	g.ledger = ledger
	ledgerClosed := false
	defer func() {
		if !ledgerClosed {
			_ = ledger.Close()
		}
	}()

	// The profile covers the whole run; a run that fails still ends it.
	var profile *generatorProfile
	if cfg.ProfileDir != "" {
		if profile, err = startGeneratorProfile(profileDir(cfg.ProfileDir, cfg.Out)); err != nil {
			return RunResult{}, err
		}
		g.profile = profile
		defer func() {
			if profile != nil {
				_, _ = profile.stop()
			}
		}()
	}

	work, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	g.work = work
	started := time.Now()
	g.phases = newPhases(started.Add(cfg.Warmup))
	sc.prepare(g)

	// One refresh schedule for the whole run, from the warm-up's start: no
	// reset at window-start, so the measurement opens on the schedule's
	// steady rate (a ramp retimes it per step).
	g.schedule = g.newRefreshSchedule(g.modelAt(loads[0]).RefreshPerSecond, started, g.phases.windowStart.Add(cfg.Measure), 0)
	g.schedule.Observe = g.observeClass("refresh", g.observeRefresh)
	scheduleDone := make(chan struct{})
	go func() {
		defer close(scheduleDone)
		g.schedule.run(g.work)
	}()

	// Warm-up: each device starts at U(0, Warmup).
	g.warmup(started)
	lead := g.leadIn(loads[0])
	if !sleepUntil(ctx, g.phases.windowStart) {
		return RunResult{}, ctx.Err()
	}
	if err := writeWindowStart(cfg.WindowStart, g.phases.windowStart); err != nil {
		return RunResult{}, err
	}
	runtimeFrom := sampleRuntime()
	stopCheckpoint := g.checkpointEvery(checkpointInterval)
	g.sampleLiveOpen()
	if g.maintenance != nil {
		at := g.phases.windowStart.Add(cfg.MaintenanceAt)
		g.goBG(func() { g.runHotMaintenance(at) })
	}

	measured := sc.measure(ctx, g)
	runtimeTo := sampleRuntime()
	measureEnd := g.phases.measureEndTime()
	g.schedule.setUntil(measureEnd)
	<-scheduleDone
	g.count(g.launched, "refresh", g.refreshLaunched.Load())
	measured.dispatchers = append(measured.dispatchers, &classDispatcher{class: "refresh", d: &g.schedule.Dispatcher})
	measured.dispatchers = append(measured.dispatchers, <-lead...)

	// Drain what is in flight for at most 15 s; the tracked devices pull to
	// head; then every operation ends, and the run waits for each to
	// return, so nothing appends to the ledger or replaces a token after
	// the ledger closes and the fleet's last checkpoint.
	waitAll(measured.dispatchers, drainLimit)
	stopCheckpoint()
	g.finalPulls(ctx)
	// The hub-wake pairs are settled before the run closes its own sockets.
	g.finishWake(time.Now())
	g.closeSockets()
	cancelWork()
	g.startClosing()
	waitAll(measured.dispatchers, 0)
	g.bg.Wait()
	g.liveWG.Wait()

	closeErr := ledger.Close()
	ledgerClosed = true
	result := g.result(measured, started, measureEnd)
	result.SchedulerLagByClass = g.lags.summary()
	result.SessionWaitByClass = g.sessionWaits.summary()
	result.GeneratorRuntime = generatorRuntime(runtimeFrom, runtimeTo)
	if profile != nil {
		// A profile that could not be written fails nothing: the run is the
		// measurement, the profile its diagnosis.
		result.GeneratorProfile = profile.result(profile.stop())
		profile = nil
		if result.GeneratorProfile.Error != "" {
			result.Notes = append(result.Notes, "the generator's profile is incomplete: "+result.GeneratorProfile.Error)
		}
	}
	err = errors.Join(closeErr, g.writeTracked(cfg.Tracked), g.checkpoint())
	if err != nil {
		return result, err
	}
	result.EndedAt = time.Now().UTC()
	if err := writeResult(cfg.Out, result); err != nil {
		return result, err
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, nil
}

// measurement is what a scenario's measure phase ran.
type measurement struct {
	dispatchers []*classDispatcher
	steps       []*windowRecorder
	windows     []*windowRecorder
	storm       *StormResult
}

// classDispatcher is one request class's open-loop dispatcher.
type classDispatcher struct {
	class string
	d     *Dispatcher
}

// waitAll waits for every launched operation, for at most limit (0: as
// long as they take).
func waitAll(dispatchers []*classDispatcher, limit time.Duration) {
	done := make(chan struct{})
	go func() {
		for _, cd := range dispatchers {
			cd.d.Wait()
		}
		close(done)
	}()
	if limit <= 0 {
		<-done
		return
	}
	select {
	case <-done:
	case <-time.After(limit):
	}
}

// classSeeds keep each class's arrivals independent of the others'.
var classSeeds = map[string]uint64{
	"pull": 0x51, "write": 0x52, "snapshot": 0x53, "mcp": 0x54, "push_token": 0x55, "live_activity": 0x56,
	"mass_snapshot": 0x57, "hot_family": 0x58, "flood_ip": 0x59, "flood_bearer": 0x5a,
}

// extraClass is a scenario's extra open-loop class. A zero length runs
// for the whole background.
type extraClass struct {
	class  string
	rate   float64
	length time.Duration
	op     func(context.Context, time.Time)
	report func() // after the measurement: the cell's figures (Cells)
	// attack keeps the class's release lag and drops out of the run's
	// gates (its own lag stays in scheduler_lag_by_class): a flood is a
	// deliberate overload of the generator's own network and CPU, which
	// would fail the scheduler-lag gate for the generator's sake, while the
	// legitimate classes' lag still judges whether their schedule was kept.
	attack bool
}

// skipSurplus is a scenario's arrival past its fixed set of work (the
// mass-snapshot devices are used up): not a shortfall of offered load.
const skipSurplus = "surplus_arrival"

// runBackground runs every class of the model at load from `from` for
// `length`, and the extra classes (the refresh schedule is the run's own, not
// the background's), and ensures the
// model's live sockets. It returns at from+length (once every schedule has
// ended, not their operations) with the dispatchers. Every released
// arrival's lag and drop is also filed in the windows holding it.
func (g *generator) runBackground(ctx context.Context, load float64, from time.Time, length time.Duration, salt uint64,
	extras []extraClass) []*classDispatcher {
	out := g.runClasses(load, from, length, salt, extras)
	for _, cd := range out {
		g.count(g.launched, cd.class, cd.d.Launched())
	}
	return out
}

// leadInMax bounds the lead-in (leadIn).
const leadInMax = 2 * time.Minute

// leadInSalt seeds the lead-in's arrivals apart from every window's.
const leadInSalt = 0xfff

// leadIn runs the first window's background classes at load over the last
// min(leadInMax, Warmup/2) of warm-up, so the measurement opens on their
// steady rate instead of a step from warm-up's one pull a device (the v4
// soak's first second went from about 350 to 1,700 requests a second). Its
// requests are warm-up's (phases go by intended start) and its launches are
// not counted. A live-only run has none: it sends no pulls, and its first
// step's sockets ramp in from window-start. The channel yields its
// dispatchers at window-start.
func (g *generator) leadIn(load float64) <-chan []*classDispatcher {
	out := make(chan []*classDispatcher, 1)
	lead := min(leadInMax, g.cfg.Warmup/2)
	if lead <= 0 || g.liveOnly() {
		out <- nil
		return out
	}
	from := g.phases.windowStart.Add(-lead)
	go func() {
		if !sleepUntil(g.work, from) {
			out <- nil
			return
		}
		out <- g.runClasses(load, from, lead, leadInSalt, nil)
	}()
	return out
}

// runClasses is runBackground without counting the launches.
func (g *generator) runClasses(load float64, from time.Time, length time.Duration, salt uint64,
	extras []extraClass) []*classDispatcher {
	m := g.modelAt(load)
	g.ensureLive(m.LiveSockets)
	var wg sync.WaitGroup
	var out []*classDispatcher
	start := func(class string, rate float64, until time.Duration, op func(context.Context, time.Time), attack ...bool) {
		if !(rate > 0) {
			return
		}
		gates := g.phases.observe
		if len(attack) > 0 && attack[0] {
			gates = func(time.Time, time.Duration, bool) {}
		}
		cd := &classDispatcher{class: class, d: &Dispatcher{Start: from, MaxInFlight: maxInFlight, Observe: g.observeClass(class, gates)}}
		out = append(out, cd)
		arrivals := NewArrivals(rate, g.cfg.Seed^classSeeds[class]<<8^salt<<20)
		wg.Add(1)
		go func() {
			defer wg.Done()
			cd.d.Run(g.work, arrivals, until, op)
		}()
	}
	start("pull", m.PullPerSecond, length, g.opPull)
	start("write", m.WritePerSecond, length, g.opWrite)
	start("snapshot", m.SnapshotPerSecond, length, g.opSnapshot)
	start("mcp", m.MCPPerSecond, length, g.opMCP)
	start("push_token", m.PushTokenPerSecond, length, g.opPushToken)
	start("live_activity", m.LiveActivityPerSecond, length, g.opLiveActivity)
	for _, e := range extras {
		until := length
		if e.length > 0 {
			until = min(e.length, length)
		}
		start(e.class, e.rate, until, e.op, e.attack)
	}
	wg.Wait()
	// A schedule ends at its last arrival; the window ends at from+length.
	sleepUntil(g.work, from.Add(length))
	return out
}

// warmup starts every device at U(0, Warmup) from started: a refresh when
// its access expires within 60 s (or a lost refresh answer is pending), a
// pull from its cursor (a tracked device first takes a complete seed), and
// its live socket if it holds one.
func (g *generator) warmup(started time.Time) {
	type start struct {
		at time.Time
		d  *device
	}
	starts := make([]start, 0, len(g.devices))
	for _, d := range g.devices {
		at := started.Add(g.rng.Duration(g.cfg.Warmup))
		if g.liveOnly() && !d.live.Load() {
			continue // live-only: a device without a socket sends nothing
		}
		starts = append(starts, start{at: at, d: d})
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].at.Before(starts[j].at) })
	g.goBG(func() {
		for _, s := range starts {
			if !sleepUntil(g.work, s.at) {
				return
			}
			g.goBG(func() { g.startDevice(g.work, s.d, s.at) })
		}
	})
}

func (g *generator) startDevice(ctx context.Context, d *device, intended time.Time) {
	if g.liveOnly() {
		// No pull: only the socket (connectLive refreshes a lapsed token first).
		g.connectLive(ctx, d, 0, 0)
		return
	}
	if d.needsRefresh(time.Now()) && !g.refresh(ctx, d, intended) {
		// Its access is gone: the device sits out until its next timer
		// refreshes it. Counted, so a short offered load shows.
		g.skip("warmup_device_without_access", intended)
		return
	}
	if d.tracked != nil {
		g.seedTracked(ctx, d, time.Now())
	} else {
		g.pullPages(ctx, d, intended, 0, RouteSyncPull)
	}
	if d.live.Load() {
		g.connectLive(ctx, d, 0, 0)
	}
}

// finalPulls brings every tracked device to the family's head after the
// drain, so its map and cursor describe the state the audit reads. A device
// whose seed never completed pulls from 0, which returns every entity at
// its current version (tombstones included) and completes its seed.
func (g *generator) finalPulls(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 64)
	for _, d := range g.devices {
		if d.tracked == nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			if d.tracked.isSeeded() {
				g.pullPages(g.work, d, time.Now(), 0, RouteSyncPull)
				return
			}
			if g.pullFrom(g.work, d, 0, time.Now(), RouteSyncPull) {
				d.tracked.markSeeded()
			}
		}()
	}
	wg.Wait()
}

// sampleLiveOpen counts the open sockets every second until the
// measurement ends.
func (g *generator) sampleLiveOpen() {
	g.goBG(func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-g.work.Done():
				return
			case now := <-ticker.C:
				if end := g.phases.measureEndTime(); !end.IsZero() && now.After(end) {
					return
				}
				open := 0
				for _, d := range g.devices {
					d.mu.Lock()
					if d.socket != nil {
						open++
					}
					d.mu.Unlock()
				}
				g.liveOpen.add(open)
				g.phases.mu.RLock()
				for _, w := range g.phases.windows {
					if w.holds(now) {
						w.liveOpen.add(open)
						if !now.Before(w.settleAt) {
							w.settled.add(open)
						}
					}
				}
				g.phases.mu.RUnlock()
			}
		}
	})
}

// checkpointEvery writes the fleet every interval until stopped.
func (g *generator) checkpointEvery(interval time.Duration) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-g.work.Done():
				return
			case <-ticker.C:
				_ = g.checkpointPaced()
			}
		}
	}()
	return func() { close(stop); <-done }
}

// checkpoint writes every device's state atomically, so the tokens the run
// rotated survive for the next run.
func (g *generator) checkpoint() error {
	header, states := g.checkpointStates()
	return WriteFleet(g.cfg.Fleet, header, states)
}

// checkpointPace is the wait after each 1 MiB a minute checkpoint syncs
// (pacedFile): 40,000 devices take about 3 s.
const checkpointPace = 100 * time.Millisecond

// checkpointPaced is checkpoint for the minute checkpoints: paced, so the
// measurement never sees the generator's own write burst.
func (g *generator) checkpointPaced() error {
	header, states := g.checkpointStates()
	return writeFleet(g.cfg.Fleet, header, states, checkpointPace)
}

// writeTracked writes tracked.jsonl (WriteTracked: 0600, never over an
// existing file): every seeded tracked device's map at its cursor. A
// device whose seed never completed holds no complete map, so it is left
// out (and counted in result.json) rather than reported as diverged.
func (g *generator) writeTracked(path string) error {
	var devices []TrackedDevice
	for _, d := range g.devices {
		t := d.tracked
		if t == nil || !t.isSeeded() {
			continue
		}
		t.mu.Lock()
		devices = append(devices, TrackedDevice{FamilyID: d.FamilyID, DeviceID: d.DeviceID, Cursor: t.cursor, Entities: maps.Clone(t.entities)})
		t.mu.Unlock()
	}
	if err := WriteTracked(path, devices); err != nil {
		return fmt.Errorf("run: tracked: %w", err)
	}
	return nil
}

// summary is the measurement's routes and generator health (by intended
// start), the ledger's errors and the tracked devices without a seed.
func (g *generator) summary(rec *Recorder) RunSummary {
	s := RunSummary{Routes: rec.Snapshot(), LedgerErrors: int(g.ledgerErrs.Load()), ModelOverride: g.cfg.ModelOverride}
	if g.liveOnly() {
		_, s.OpenSocketsMean = g.liveOpen.read()
	}
	g.phases.health.into(&s)
	for _, d := range g.devices {
		if d.tracked != nil && !d.tracked.isSeeded() {
			s.TrackedUnseeded++
		}
	}
	return s
}

// windowSummary is a window's routes with its own generator health: the
// lag, drops, launches and skips of the operations intended in it and the
// sockets lost in it.
func (g *generator) windowSummary(w *windowRecorder) RunSummary {
	s := RunSummary{Routes: w.rec.Snapshot(), ModelOverride: g.cfg.ModelOverride}
	if g.liveOnly() {
		_, s.OpenSocketsMean = w.liveOpen.read()
	}
	w.health.into(&s)
	return s
}

func (g *generator) result(m measurement, started, measureEnd time.Time) RunResult {
	model := g.modelAt(g.cfg.Load)
	g.countMu.Lock()
	launched, skipped := maps.Clone(g.launched), maps.Clone(g.skipped)
	g.countMu.Unlock()
	g.notesMu.Lock()
	notes, cells := append([]string{}, g.notes...), maps.Clone(g.cells)
	g.notesMu.Unlock()
	r := RunResult{RunSummary: g.summary(g.phases.measure), Scenario: g.cfg.Scenario, Load: g.cfg.Load, Model: model,
		Thresholds: thresholdsJSON(g.cfg.Thresholds), Seed: g.cfg.Seed, Devices: len(g.devices), Families: g.header.Families,
		StartedAt: started.UTC(), WindowStart: g.phases.windowStart.UTC(), MeasureEnd: measureEnd.UTC(),
		Launched: launched, Skipped: skipped, WarmupRoutes: g.phases.warm.Snapshot(), DrainRoutes: g.phases.drain.Snapshot(),
		WarmupLiveLoss: int(g.warmLosses.Load()), WarmupRefreshLaunched: g.refreshWarm.Load(), PlannedRenewals: g.renewals.Load(), LedgerEntries: g.ledgered.Load(),
		Storm: m.storm, Notes: notes, Cells: cells}
	g.lossMu.Lock()
	r.LiveLossDetails = slices.Clone(g.lossDetail)
	g.lossMu.Unlock()
	for _, d := range g.devices {
		if d.live.Load() {
			r.LiveDevices++
		}
		if d.tracked != nil {
			r.TrackedDevices++
		}
	}
	r.LiveOpenMin, r.LiveOpenMean = g.liveOpen.read()
	if !g.liveOnly() {
		r.HubWakeDelay = g.wake.result()
	}
	g.refreshMu.Lock()
	if len(g.refreshes) > 0 {
		r.RefreshEffectivePerSecond, r.RefreshPeriodSeconds = g.refreshes[0].perSecond, g.refreshes[0].periodSeconds
	}
	refreshes := slices.Clone(g.refreshes)
	g.refreshMu.Unlock()
	if attack := g.attack.Snapshot(); len(attack) > 0 {
		r.AttackRoutes = attack
	}
	if g.liveOnly() {
		r.Notes = append(r.Notes, "model override live-only: every class but the live sockets at 0, no refresh schedule and no tracked devices; "+
			"a live device refreshes only when its access lapses before it dials; the gates of the zeroed classes that measured nothing are not applicable")
	}
	if r.TrackedUnseeded > 0 {
		r.Notes = append(r.Notes, fmt.Sprintf("%d tracked devices never completed their seed: left out of tracked.jsonl, and the run fails", r.TrackedUnseeded))
	}
	r.Notes = append(r.Notes,
		"latency is measured from each request's intended start (open loop); pages after a pull's first are measured from the previous page's completion",
		"audit fields are filled by `baby-tracker-load audit` (Task 11); generator_verdict judges only what the generator sees")
	window := func(w *windowRecorder) WindowResult {
		s := g.windowSummary(w)
		result := WindowResult{Name: w.name, Load: w.load, From: w.from.UTC(), To: w.to.UTC(), Summary: s, Verdict: EvaluateWindow(s, g.cfg.Thresholds),
			LiveSockets: g.modelAt(w.load).LiveSockets}
		result.LiveOpenMin, result.LiveOpenMean = w.liveOpen.read()
		if g.liveOnly() {
			result.SettledFrom = w.settleAt.UTC()
			result.LiveOpenMinSettled, result.LiveOpenMeanSettled = w.settled.read()
		}
		return result
	}
	for i, w := range m.steps {
		step := window(w)
		if i < len(refreshes) {
			step.RefreshEffectivePerSecond = refreshes[i].perSecond
		}
		r.Steps = append(r.Steps, step)
	}
	for _, w := range m.windows {
		r.Windows = append(r.Windows, window(w))
	}
	r.GeneratorVerdict = EvaluateWindow(r.RunSummary, g.cfg.Thresholds)
	return r
}

// writeWindowStart writes the measurement window's start (RFC 3339): the
// runner resets pg_stat_statements and times profiles from it.
func writeWindowStart(path string, at time.Time) error {
	if path == "" {
		return nil
	}
	return writeFileAtomic(path, []byte(at.UTC().Format(time.RFC3339Nano)+"\n"), 0o644)
}

func writeResult(path string, r RunResult) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(raw, '\n'), 0o644)
}

// writeFileAtomic writes a temporary file beside path, syncs it and renames
// it over path.
func writeFileAtomic(path string, raw []byte, mode os.FileMode) (resultErr error) {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil {
			_ = file.Close()
			_ = os.Remove(file.Name())
		}
	}()
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if _, err := file.Write(raw); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

// sleepUntil waits for t or ctx; it reports false when ctx ended first.
func sleepUntil(ctx context.Context, t time.Time) bool { return wallClock{}.SleepUntil(ctx, t) }

// sleepFor waits d or ctx.
func sleepFor(ctx context.Context, d time.Duration) bool { return sleepUntil(ctx, time.Now().Add(d)) }

// sortedKeys is m's keys in order (stable result files).
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
