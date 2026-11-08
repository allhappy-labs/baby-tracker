package loadtest

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"runtime/trace"
	"sync"
	"time"
)

// The generator's own health: its release lag per class, its Go runtime's
// scheduling latency, GC pauses and CPU over the measurement (always, in
// result.json), and, with --profile-dir, its profiles.

// LagSummary is one class's release lag over the measurement.
type LagSummary struct {
	Arrivals uint64  `json:"arrivals"`
	P50Ms    float64 `json:"p50_ms"`
	P99Ms    float64 `json:"p99_ms"`
	MaxMs    float64 `json:"max_ms"`
}

func lagSummary(h *Histogram) LagSummary {
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	return LagSummary{Arrivals: h.Total, P50Ms: ms(h.Quantile(0.5)), P99Ms: ms(h.Quantile(0.99)), MaxMs: ms(h.Quantile(1))}
}

// classLags are the release lags of the measurement's arrivals by class
// (the scheduler-lag gate merges them: phases.health).
type classLags struct {
	mu sync.Mutex
	m  map[string]*classLag
}

type classLag struct {
	mu sync.Mutex
	h  Histogram
}

func (c *classLags) of(class string) *classLag {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*classLag{}
	}
	if c.m[class] == nil {
		c.m[class] = &classLag{}
	}
	return c.m[class]
}

// SessionWaitSummary is one class's waits for its device's session over
// the measurement (device.holdSession): the holds, how many waited at
// least a millisecond (a refresh, or for a refresh its device's requests,
// in flight), and the wait's quantiles.
type SessionWaitSummary struct {
	Holds  uint64  `json:"holds"`
	Waited uint64  `json:"waited"`
	P50Ms  float64 `json:"p50_ms"`
	P99Ms  float64 `json:"p99_ms"`
	MaxMs  float64 `json:"max_ms"`
}

// sessionWaits are the measurement's session waits by class.
type sessionWaits struct {
	lags   classLags
	mu     sync.Mutex
	waited map[string]uint64
}

func (s *sessionWaits) note(class string, wait time.Duration) {
	l := s.lags.of(class)
	l.mu.Lock()
	l.h.Record(wait)
	l.mu.Unlock()
	if wait >= time.Millisecond {
		s.mu.Lock()
		if s.waited == nil {
			s.waited = map[string]uint64{}
		}
		s.waited[class]++
		s.mu.Unlock()
	}
}

func (s *sessionWaits) summary() map[string]SessionWaitSummary {
	lags := s.lags.summary()
	if lags == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]SessionWaitSummary, len(lags))
	for class, l := range lags {
		out[class] = SessionWaitSummary{Holds: l.Arrivals, Waited: s.waited[class], P50Ms: l.P50Ms, P99Ms: l.P99Ms, MaxMs: l.MaxMs}
	}
	return out
}

// noteSessionWait files a hold of class's device session that waited
// wait, when it falls in the measurement.
func (g *generator) noteSessionWait(class string, wait time.Duration) {
	if g.phases.inMeasure(time.Now()) {
		g.sessionWaits.note(class, wait)
	}
}

func (c *classLags) summary() map[string]LagSummary {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) == 0 {
		return nil
	}
	out := make(map[string]LagSummary, len(c.m))
	for class, l := range c.m {
		l.mu.Lock()
		out[class] = lagSummary(&l.h)
		l.mu.Unlock()
	}
	return out
}

// observeClass is a dispatcher's Observe for class: inner (the phases'),
// then the class's lag when the arrival falls in the measurement, and the
// profile's lag trigger.
func (g *generator) observeClass(class string, inner func(time.Time, time.Duration, bool)) func(time.Time, time.Duration, bool) {
	l := g.lags.of(class)
	return func(intended time.Time, lag time.Duration, dropped bool) {
		inner(intended, lag, dropped)
		if !g.phases.inMeasure(intended) {
			return
		}
		l.mu.Lock()
		l.h.Record(lag)
		l.mu.Unlock()
		if g.profile != nil {
			g.profile.lagged(lag)
		}
	}
}

// GeneratorRuntime is the generator process's Go runtime over the
// measurement (runtime/metrics, window-start to the measurement's end):
// whether its own scheduling, GC or CPU could explain a release lag.
type GeneratorRuntime struct {
	GOMAXPROCS int     `json:"gomaxprocs"`
	NumCPU     int     `json:"num_cpu"`
	GOGC       string  `json:"gogc"`       // the environment's ("" is the default, 100)
	GOMEMLIMIT string  `json:"gomemlimit"` // the environment's ("" is none)
	Seconds    float64 `json:"seconds"`
	// UserCores and SystemCores are the process's CPU (getrusage), in
	// cores (CPU seconds per second); 0 where the OS does not say.
	UserCores   float64 `json:"user_cores"`
	SystemCores float64 `json:"system_cores"`
	GCCycles    uint64  `json:"gc_cycles"`
	// GCPause* are the stop-the-world pauses; SchedLatency* the time
	// goroutines spent runnable before they ran (the dispatchers' included).
	GCPauseP99Ms      float64 `json:"gc_pause_p99_ms"`
	GCPauseMaxMs      float64 `json:"gc_pause_max_ms"`
	SchedLatencyP99Ms float64 `json:"sched_latency_p99_ms"`
	SchedLatencyMaxMs float64 `json:"sched_latency_max_ms"`
	// SchedLatencyOver10Ms is the share of goroutine wakeups that waited
	// more than 10 ms to run (the runtime's bucket straddling 10 ms counted
	// by the part of it above: histShareAbove).
	SchedLatencyOver10Ms float64 `json:"sched_latency_over_10ms_share"`
	HeapObjectsBytes     uint64  `json:"heap_objects_bytes"` // at the measurement's end
	Goroutines           uint64  `json:"goroutines"`         // at the measurement's end
}

var runtimeMetricNames = []string{
	"/gc/cycles/total:gc-cycles",
	"/sched/pauses/total/gc:seconds", "/sched/latencies:seconds", "/memory/classes/heap/objects:bytes",
	"/sched/goroutines:goroutines",
}

// runtimeSample is the runtime's metrics and the process's CPU at one
// moment.
type runtimeSample struct {
	at           time.Time
	samples      []metrics.Sample
	user, system time.Duration
	cpu          bool // user and system are known
}

func sampleRuntime() runtimeSample {
	s := runtimeSample{at: time.Now(), samples: make([]metrics.Sample, len(runtimeMetricNames))}
	s.user, s.system, s.cpu = processCPU()
	for i, name := range runtimeMetricNames {
		s.samples[i].Name = name
	}
	metrics.Read(s.samples)
	return s
}

func (s runtimeSample) value(name string) metrics.Value {
	for _, sample := range s.samples {
		if sample.Name == name {
			return sample.Value
		}
	}
	return metrics.Value{}
}

func (s runtimeSample) uint(name string) uint64 {
	if v := s.value(name); v.Kind() == metrics.KindUint64 {
		return v.Uint64()
	}
	return 0
}

func (s runtimeSample) hist(name string) *metrics.Float64Histogram {
	if v := s.value(name); v.Kind() == metrics.KindFloat64Histogram {
		return v.Float64Histogram()
	}
	return nil
}

// histDelta is to's counts less from's (the same buckets: the runtime's
// are fixed for the process's life).
func histDelta(from, to *metrics.Float64Histogram) *metrics.Float64Histogram {
	if to == nil {
		return nil
	}
	out := &metrics.Float64Histogram{Buckets: to.Buckets, Counts: append([]uint64(nil), to.Counts...)}
	if from != nil && len(from.Counts) == len(out.Counts) {
		for i := range out.Counts {
			out.Counts[i] -= from.Counts[i]
		}
	}
	return out
}

// histQuantile is the q-quantile's bucket upper bound in milliseconds (its
// lower bound for the last, unbounded bucket).
func histQuantile(h *metrics.Float64Histogram, q float64) float64 {
	if h == nil {
		return 0
	}
	var total uint64
	for _, c := range h.Counts {
		total += c
	}
	if total == 0 {
		return 0
	}
	rank := max(uint64(math.Ceil(q*float64(total))), 1)
	var seen uint64
	for i, c := range h.Counts {
		seen += c
		if seen >= rank {
			bound := h.Buckets[i+1]
			if math.IsInf(bound, 1) {
				bound = h.Buckets[i]
			}
			return bound * 1000
		}
	}
	return 0
}

// histShareAbove is the share of samples above over (seconds): every
// bucket that starts at or above it, and of the bucket that straddles it the
// part above over, as if its samples were spread evenly across it (all of
// it when the bucket is unbounded).
func histShareAbove(h *metrics.Float64Histogram, over float64) float64 {
	if h == nil {
		return 0
	}
	var total uint64
	var above float64
	for i, c := range h.Counts {
		total += c
		lo, hi := h.Buckets[i], h.Buckets[i+1]
		switch {
		case lo >= over:
			above += float64(c)
		case hi > over && math.IsInf(hi, 1):
			above += float64(c)
		case hi > over:
			above += float64(c) * (hi - over) / (hi - lo)
		}
	}
	if total == 0 {
		return 0
	}
	return above / float64(total)
}

// generatorRuntime is the runtime over [from, to].
func generatorRuntime(from, to runtimeSample) *GeneratorRuntime {
	seconds := to.at.Sub(from.at).Seconds()
	if seconds <= 0 {
		return nil
	}
	pauses := histDelta(from.hist("/sched/pauses/total/gc:seconds"), to.hist("/sched/pauses/total/gc:seconds"))
	latencies := histDelta(from.hist("/sched/latencies:seconds"), to.hist("/sched/latencies:seconds"))
	r := &GeneratorRuntime{
		GOMAXPROCS: runtime.GOMAXPROCS(0), NumCPU: runtime.NumCPU(), GOGC: os.Getenv("GOGC"), GOMEMLIMIT: os.Getenv("GOMEMLIMIT"),
		Seconds:      seconds,
		GCCycles:     to.uint("/gc/cycles/total:gc-cycles") - from.uint("/gc/cycles/total:gc-cycles"),
		GCPauseP99Ms: histQuantile(pauses, 0.99), GCPauseMaxMs: histQuantile(pauses, 1),
		SchedLatencyP99Ms: histQuantile(latencies, 0.99), SchedLatencyMaxMs: histQuantile(latencies, 1),
		SchedLatencyOver10Ms: histShareAbove(latencies, 0.010),
		HeapObjectsBytes:     to.uint("/memory/classes/heap/objects:bytes"), Goroutines: to.uint("/sched/goroutines:goroutines"),
	}
	if from.cpu && to.cpu {
		r.UserCores, r.SystemCores = (to.user-from.user).Seconds()/seconds, (to.system-from.system).Seconds()/seconds
	}
	return r
}

// Profile files the generator writes into --profile-dir. The runner's own
// app profiles there are named after their label (at-12m-cpu.pb.gz).
const (
	profileCPU       = "generator-cpu.pb.gz"
	profileHeap      = "generator-heap.pb.gz"
	profileAllocs    = "generator-allocs.pb.gz"
	profileMutex     = "generator-mutex.pb.gz"
	profileBlock     = "generator-block.pb.gz"
	profileGoroutine = "generator-goroutine.txt"
	profileLagTraces = 3 // generator-lag-trace-N.out
)

// lagTraceThreshold is the release lag that snapshots the execution trace:
// the scheduler-lag gate's p99 bound.
const lagTraceThreshold = 20 * time.Millisecond

// lagTraceSpacing keeps the snapshots apart: each holds about the last
// lagTraceWindow.
const (
	lagTraceSpacing = 30 * time.Second
	lagTraceWindow  = 5 * time.Second
)

// lagTraceName is the nth lag trace's file.
func lagTraceName(n int) string { return fmt.Sprintf("generator-lag-trace-%d.out", n) }

// generatorProfile is --profile-dir: a CPU profile of the whole run; at its
// end the heap, allocations, mutex and block contention and goroutines;
// and an execution-trace flight recorder whose window is written (up to
// profileLagTraces times) when a measured arrival is released more than
// lagTraceThreshold late, so a lag can be read goroutine by goroutine.
type generatorProfile struct {
	dir    string
	cpu    *os.File
	fr     *trace.FlightRecorder
	lag    chan time.Duration
	done   chan struct{}
	exited chan struct{}

	mu     sync.Mutex
	traces []LagTrace
	errs   []error
}

// LagTrace is one flight-recorder snapshot: when, the lag that triggered
// it and its file.
type LagTrace struct {
	At    time.Time `json:"at"`
	LagMs float64   `json:"lag_ms"`
	File  string    `json:"file"`
	Error string    `json:"error,omitempty"`
}

// profileDir is --profile-dir resolved: a relative directory is beside
// out (the runner passes "profiles": $RUN/profiles).
func profileDir(dir, out string) string {
	if dir == "" || filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(filepath.Dir(out), dir)
}

// startGeneratorProfile starts the CPU profile, contention sampling and the
// flight recorder. It refuses a profile file that already exists: a
// directory of an earlier run is not overwritten.
func startGeneratorProfile(dir string) (*generatorProfile, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("run: --profile-dir: %w", err)
	}
	names := []string{profileCPU, profileHeap, profileAllocs, profileMutex, profileBlock, profileGoroutine}
	for n := 1; n <= profileLagTraces; n++ {
		names = append(names, lagTraceName(n))
	}
	for _, name := range names {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return nil, fmt.Errorf("run: --profile-dir: %s already exists", filepath.Join(dir, name))
		}
	}
	cpu, err := os.OpenFile(filepath.Join(dir, profileCPU), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("run: --profile-dir: %w", err)
	}
	if err := pprof.StartCPUProfile(cpu); err != nil {
		_ = cpu.Close()
		_ = os.Remove(cpu.Name()) // empty: it would refuse the next run
		return nil, fmt.Errorf("run: cpu profile: %w", err)
	}
	// Contention, sampled: 1 in 100 mutex events, and blocking of 1 ms or
	// more (the profile is opt-in; the cost stays small).
	runtime.SetMutexProfileFraction(100)
	runtime.SetBlockProfileRate(int(time.Millisecond))
	p := &generatorProfile{dir: dir, cpu: cpu, lag: make(chan time.Duration, 1), done: make(chan struct{}), exited: make(chan struct{})}
	p.fr = trace.NewFlightRecorder(trace.FlightRecorderConfig{MinAge: lagTraceWindow, MaxBytes: 64 << 20})
	if err := p.fr.Start(); err != nil {
		p.fr = nil
		p.errs = append(p.errs, fmt.Errorf("flight recorder: %w", err))
	}
	go p.watch()
	return p, nil
}

// lagged hands a measured arrival's lag to the watcher without waiting: the
// dispatcher never blocks on the profile.
func (p *generatorProfile) lagged(lag time.Duration) {
	if lag <= lagTraceThreshold {
		return
	}
	select {
	case p.lag <- lag:
	default:
	}
}

func (p *generatorProfile) watch() {
	defer close(p.exited)
	var last time.Time
	for {
		select {
		case <-p.done:
			return
		case lag := <-p.lag:
			p.mu.Lock()
			n := len(p.traces)
			p.mu.Unlock()
			if p.fr == nil || n >= profileLagTraces || !last.IsZero() && time.Since(last) < lagTraceSpacing {
				continue
			}
			last = time.Now()
			// Half a second more, so the snapshot holds the lag's aftermath.
			select {
			case <-p.done:
				return
			case <-time.After(500 * time.Millisecond):
			}
			p.snapshot(last, lag, n+1)
		}
	}
}

func (p *generatorProfile) snapshot(at time.Time, lag time.Duration, n int) {
	name := lagTraceName(n)
	t := LagTrace{At: at.UTC(), LagMs: float64(lag) / float64(time.Millisecond), File: name}
	f, err := os.OpenFile(filepath.Join(p.dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err == nil {
		_, err = p.fr.WriteTo(f)
		err = errors.Join(err, f.Close())
	}
	if err != nil {
		t.Error = err.Error()
	}
	p.mu.Lock()
	p.traces = append(p.traces, t)
	p.mu.Unlock()
}

// stop ends the CPU profile and the flight recorder and writes the
// end-of-run profiles. It returns the lag traces taken and every failure.
func (p *generatorProfile) stop() ([]LagTrace, error) {
	close(p.done)
	<-p.exited
	if p.fr != nil {
		p.fr.Stop()
	}
	pprof.StopCPUProfile()
	errs := append([]error{}, p.errs...)
	errs = append(errs, p.cpu.Close())
	for _, w := range []struct {
		name, profile string
		debug         int
	}{{profileHeap, "heap", 0}, {profileAllocs, "allocs", 0}, {profileMutex, "mutex", 0}, {profileBlock, "block", 0}, {profileGoroutine, "goroutine", 1}} {
		f, err := os.OpenFile(filepath.Join(p.dir, w.name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, pprof.Lookup(w.profile).WriteTo(f, w.debug), f.Close())
	}
	runtime.SetMutexProfileFraction(0)
	runtime.SetBlockProfileRate(0)
	p.mu.Lock()
	traces := append([]LagTrace(nil), p.traces...)
	p.mu.Unlock()
	return traces, errors.Join(errs...)
}

// GeneratorProfile is result.json's account of --profile-dir.
type GeneratorProfile struct {
	Dir       string     `json:"dir"`
	Files     []string   `json:"files"`
	LagTraces []LagTrace `json:"lag_traces"`
	Error     string     `json:"error,omitempty"`
}

func (p *generatorProfile) result(traces []LagTrace, err error) *GeneratorProfile {
	r := &GeneratorProfile{Dir: p.dir, Files: []string{profileCPU, profileHeap, profileAllocs, profileMutex, profileBlock, profileGoroutine},
		LagTraces: traces}
	if r.LagTraces == nil {
		r.LagTraces = []LagTrace{}
	}
	if err != nil {
		r.Error = err.Error()
	}
	return r
}
