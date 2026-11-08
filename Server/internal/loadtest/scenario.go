package loadtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
)

// scenario is one `--scenario`: the loads it reaches, the windows it
// declares before warm-up, and its measurement.
type scenario struct {
	loads func(cfg RunConfig) []float64
	// exclude, when set, keeps a scenario's own devices out of the live
	// designation and the background classes (before designation).
	exclude func(g *generator)
	prepare func(g *generator)
	measure func(ctx context.Context, g *generator) measurement
}

var scenarios = map[string]scenario{
	"soak":          {loads: backgroundLoad, prepare: noWindows, measure: measureSoak},
	"ramp":          {loads: func(cfg RunConfig) []float64 { return cfg.Steps }, prepare: noWindows, measure: measureRamp},
	"storm":         {loads: backgroundLoad, prepare: prepareStorm, measure: measureStorm},
	"restart":       {loads: backgroundLoad, prepare: prepareRestart, measure: measureSoak},
	"mass-snapshot": {loads: backgroundLoad, prepare: noWindows, measure: withExtras(massSnapshotExtras)},
	"hot-family":    {loads: backgroundLoad, exclude: excludeHotFamily, prepare: noWindows, measure: withExtras(hotFamilyExtras)},
	"flood":         {loads: backgroundLoad, prepare: noWindows, measure: withExtras(floodExtras)},
	"import":        {loads: backgroundLoad, prepare: noWindows, measure: withExtras(importExtras)},
	"export":        {loads: backgroundLoad, prepare: noWindows, measure: withExtras(exportExtras)},
}

// backgroundLoad is --load. The plan's scenario table names the background
// each scenario is run at (1x for mass-snapshot, 2x for hot-family, flood,
// import and export); the runner passes it as --load.
func backgroundLoad(cfg RunConfig) []float64 { return []float64{cfg.Load} }

func noWindows(*generator) {}

// measureSoak runs the background model at --load for --measure.
func measureSoak(ctx context.Context, g *generator) measurement {
	return withExtras(nil)(ctx, g)
}

// withExtras runs the background plus a scenario's extra classes.
func withExtras(extras func(g *generator) []extraClass) func(context.Context, *generator) measurement {
	return func(ctx context.Context, g *generator) measurement {
		from := g.phases.windowStart
		g.phases.setMeasureEnd(from.Add(g.cfg.Measure))
		var classes []extraClass
		if extras != nil {
			classes = extras(g)
		}
		ds := g.runBackground(ctx, g.cfg.Load, from, g.cfg.Measure, 0, classes)
		for _, c := range classes {
			if c.report != nil {
				c.report()
			}
		}
		g.phases.mu.RLock()
		windows := slices.Clone(g.phases.windows)
		g.phases.mu.RUnlock()
		return measurement{dispatchers: ds, windows: windows}
	}
}

// --- restart ----------------------------------------------------------------------

// prepareRestart declares [RestartAt, RestartAt+RestartGrace] (from
// window-start): failures inside it are restart_window, socket losses are
// not counted, and the windows before, during and after are summarised.
func prepareRestart(g *generator) {
	p := g.phases
	from := p.windowStart.Add(g.cfg.RestartAt)
	to := from.Add(g.cfg.RestartGrace)
	p.restartFrom, p.restartTo = from, to
	end := p.windowStart.Add(g.cfg.Measure)
	p.addWindow("before_restart", g.cfg.Load, p.windowStart, from)
	p.addWindow("restart", g.cfg.Load, from, to)
	p.addWindow("after_restart", g.cfg.Load, to, end)
}

// --- storm ------------------------------------------------------------------------

// StormResult is the storm's reconnection: every live socket closed
// client-side at once at StormAt, each redialled by the iOS policy.
type StormResult struct {
	At               time.Time `json:"at"`
	Sockets          int       `json:"sockets"`
	Reconnected      int       `json:"reconnected"`
	SecondsTo99      float64   `json:"seconds_to_99_percent"` // -1: never reached
	SecondsToAll     float64   `json:"seconds_to_all"`        // -1: never reached
	FailedHandshakes uint64    `json:"failed_handshakes"`
}

func prepareStorm(g *generator) {
	p := g.phases
	from := p.windowStart.Add(g.cfg.StormAt)
	to := from.Add(stormWindow)
	p.stormFrom, p.stormTo = from, to
	end := p.windowStart.Add(g.cfg.Measure)
	p.addWindow("before_storm", g.cfg.Load, p.windowStart, from)
	stormEnd := to
	if end.Before(stormEnd) {
		stormEnd = end
	}
	p.addWindow("storm", g.cfg.Load, from, stormEnd)
	if to.Before(end) {
		p.addWindow("after_storm", g.cfg.Load, to, end)
	}
}

func measureStorm(ctx context.Context, g *generator) measurement {
	done := make(chan measurement, 1)
	go func() { done <- measureSoak(ctx, g) }()
	var storm *StormResult
	if sleepUntil(g.work, g.phases.stormFrom) {
		// Reconnection is followed until the measurement ends: a socket
		// still down then counts as not reconnected.
		ctx, cancel := context.WithDeadline(g.work, g.phases.windowStart.Add(g.cfg.Measure))
		storm = g.runStorm(ctx)
		cancel()
	}
	m := <-done
	m.storm = storm
	return m
}

// runStorm closes every live socket client-side at once (1001), then
// redials each by SyncRetryPolicy(base 1, ceiling 60) with full jitter,
// from its first attempt (U(0, 1 s)), and reports the time until 99% and
// all of them were connected again. A device whose own other dial (a
// renewal after a server close) got there first counts as reconnected when that dial
// opens. Failed handshakes are the storm redials' own.
func (g *generator) runStorm(ctx context.Context) *StormResult {
	at := time.Now()
	var live []*device
	for _, d := range g.devices {
		if d.live.Load() {
			live = append(live, d)
		}
	}
	closed := g.retireSockets(live, closeGoingAway, "storm")
	result := &StormResult{At: at.UTC(), Sockets: len(closed), SecondsTo99: -1, SecondsToAll: -1}
	var mu sync.Mutex
	var times []time.Duration
	var failed atomic.Uint64
	var wg sync.WaitGroup
	for _, d := range closed {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dialed := g.connectLive(ctx, d, 1, iosBackoff(1, g.rng))
			failed.Add(uint64(dialed.failures))
			opened := dialed.opened
			if dialed.busy {
				opened = waitForSocket(ctx, d)
			}
			if opened {
				mu.Lock()
				times = append(times, time.Since(at))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	result.FailedHandshakes = failed.Load()
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	result.Reconnected = len(times)
	if n := len(closed); n > 0 {
		if need := (99*n + 99) / 100; len(times) >= need {
			result.SecondsTo99 = times[need-1].Seconds()
		}
		if len(times) == n {
			result.SecondsToAll = times[n-1].Seconds()
		}
	}
	return result
}

// waitForSocket waits until d holds a socket, or ctx ends.
func waitForSocket(ctx context.Context, d *device) bool {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		d.mu.Lock()
		open := d.socket != nil
		d.mu.Unlock()
		if open {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

// --- ramp -------------------------------------------------------------------------

// measureRamp runs each step's load for StepDuration (within --measure),
// evaluating each step with the window gates (EvaluateWindow) once the next
// step has run: the ramp stops at the first failing step plus one step.
func measureRamp(ctx context.Context, g *generator) measurement {
	var m measurement
	from := g.phases.windowStart
	steps := g.cfg.Steps
	maxSteps := int(g.cfg.Measure / g.cfg.StepDuration)
	if maxSteps < len(steps) {
		steps = steps[:max(maxSteps, 1)]
	}
	g.phases.setMeasureEnd(from.Add(time.Duration(len(steps)) * g.cfg.StepDuration))
	for i, load := range steps {
		if ctx.Err() != nil {
			break
		}
		start := from.Add(time.Duration(i) * g.cfg.StepDuration)
		w := g.phases.addWindow(fmt.Sprintf("step_%d_%gx", i+1, load), load, start, start.Add(g.cfg.StepDuration))
		if i > 0 && g.liveOnly() {
			w.settleAt = start.Add(g.liveRampIn())
		}
		if i > 0 {
			// The run's one refresh schedule moves to the step's rate.
			g.schedule.setRate(start, g.modelAt(load).RefreshPerSecond)
		}
		ds := g.runBackground(ctx, load, start, g.cfg.StepDuration, uint64(i+1), nil)
		m.dispatchers = append(m.dispatchers, ds...)
		m.steps = append(m.steps, w)
		if i >= 1 && !EvaluateWindow(g.windowSummary(m.steps[i-1]), g.cfg.Thresholds).Pass {
			g.phases.setMeasureEnd(start.Add(g.cfg.StepDuration))
			g.note("ramp stopped after step %d: step %d failed its window gates", i+1, i)
			break
		}
	}
	return m
}

// --- mass-snapshot, hot-family, flood -----------------------------------------------

// massSnapshotExtras: 10% of the devices (one per account) start
// snapshots, then pull, over the first 5 minutes.
func massSnapshotExtras(g *generator) []extraClass {
	seen := map[string]bool{}
	var picked []*device
	order := newLockedRand(g.cfg.Seed, 0x5a5).rngPerm(len(g.devices))
	want := len(g.devices) / 10
	for _, i := range order {
		d := g.devices[i]
		if len(picked) >= want {
			break
		}
		if !seen[d.AccountID] {
			seen[d.AccountID] = true
			picked = append(picked, d)
		}
	}
	window := min(5*time.Minute, g.cfg.Measure)
	var next atomic.Int64
	return []extraClass{{class: "mass_snapshot", rate: float64(len(picked)) / window.Seconds(), length: window,
		op: func(ctx context.Context, intended time.Time) {
			i := int(next.Add(1)) - 1
			if i >= len(picked) {
				g.skip(skipSurplus, intended)
				return
			}
			d := picked[i]
			g.accounts[d.AccountID].tryStartSnapshot(time.Now())
			g.snapshotAndPull(ctx, d, intended, RouteSyncPull)
		}}}
}

// hotFamilyShare is the share of the product's sync limits the hot family
// writes at: its writers' per-device (120/min) and per-account (240/min)
// sync budgets, the tighter of the two for each account, summed. Every POST
// /v1/sync is charged to both, writes included, so the plan's 50/s on 3–4
// devices would measure the limiter (Task 10 review I6; the controller's
// ruling: the cell measures family-lock contention at the highest rate
// real clients can reach; the plan's 10-device, 300/min question goes to
// Task 20).
const hotFamilyShare = 0.8

// hotFamilyMutationsPerSecond is the plan's hot-family write rate (Task 20
// Step 2: 50 writes a second), carried by the seeded hot family
// (`seed --hot-family`, 5 accounts × 2 devices). Its sync budget at
// hotFamilyShare is 16 requests a second (10 devices at 120/min, 5
// accounts at 240/min). The cell drives that whole budget, one family-lock
// acquisition a request, and carries the mutations several to a request,
// as a phone batches its pending writes: 50/16 = 3.125 a request on
// average (3, or 4 one request in 8), so about 50 mutations a second.
const hotFamilyMutationsPerSecond = 50

// hotFamily is the hot family cell: its writers by account and the rate
// the limits allow.
type hotFamily struct {
	family     string
	seeded     bool        // the fleet's own hot family (FleetHeader.HotFamilyID)
	accounts   [][]*device // writers grouped by account
	weights    []float64   // each account's share of the rate
	budget     float64     // requests per second at hotFamilyShare of the sync limits
	rate       float64     // requests per second
	perRequest float64     // mutations per request, on average
	binding    []string    // each account's binding limit
	// next is each account's round-robin over its devices: the limiter
	// counts fixed minute windows, so a random choice would push a device
	// over its 120/min now and then; a rotation keeps each at its share.
	next []*atomic.Uint64
}

// planHotFamily picks the fleet's seeded hot family, else the family with
// the most writers (by the seed), and sizes its request rate to
// hotFamilyShare of its writers' sync budgets. The seeded family sends
// hotFamilyMutationsPerSecond over that whole budget; another family one
// mutation a request.
func planHotFamily(g *generator) hotFamily {
	byFamily := map[string][]*device{}
	for _, d := range g.writers {
		byFamily[d.FamilyID] = append(byFamily[d.FamilyID], d)
	}
	families := make([]string, 0, len(byFamily))
	for f := range byFamily {
		families = append(families, f)
	}
	slices.Sort(families)
	best := ""
	for _, i := range newLockedRand(g.cfg.Seed, 0x40f).rngPerm(len(families)) {
		if f := families[i]; best == "" || len(byFamily[f]) > len(byFamily[best]) {
			best = f
		}
	}
	seeded := false
	if hot := g.header.HotFamilyID; hot != "" {
		if len(byFamily[hot]) > 0 {
			best, seeded = hot, true
		} else {
			g.note("the fleet names hot family %s, but it holds no writer: the family with the most writers stands in", hot)
		}
	}
	limits := protocol.DefaultLimits()
	byAccount := map[string][]*device{}
	var order []string
	for _, d := range byFamily[best] {
		if byAccount[d.AccountID] == nil {
			order = append(order, d.AccountID)
		}
		byAccount[d.AccountID] = append(byAccount[d.AccountID], d)
	}
	h := hotFamily{family: best, seeded: seeded}
	var total float64
	for _, account := range order {
		devices := byAccount[account]
		perMinute := float64(limits.SyncPullDevicePerMinute * len(devices))
		binding := fmt.Sprintf("sync-pull-device %d/min × %d devices", limits.SyncPullDevicePerMinute, len(devices))
		if accountMax := float64(limits.SyncPullAccountPerMinute); accountMax < perMinute {
			perMinute, binding = accountMax, fmt.Sprintf("sync-pull-account %d/min", limits.SyncPullAccountPerMinute)
		}
		h.accounts = append(h.accounts, devices)
		h.next = append(h.next, &atomic.Uint64{})
		h.weights = append(h.weights, perMinute)
		h.binding = append(h.binding, binding)
		total += perMinute
	}
	for i := range h.weights {
		h.weights[i] /= total
	}
	h.budget = hotFamilyShare * total / 60
	h.rate, h.perRequest = h.budget, 1
	if seeded && h.budget > 0 {
		h.perRequest = min(max(hotFamilyMutationsPerSecond/h.budget, 1), float64(limits.SyncBatchSize))
	}
	return h
}

// mutations draws one request's mutations: perRequest on average, the
// whole part and one more with the fraction's probability.
func (h hotFamily) mutations(rng *lockedRand) int {
	n := int(h.perRequest)
	if rng.Float64() < h.perRequest-float64(n) {
		n++
	}
	return n
}

// pick draws a writer: an account by its share, then its devices in turn.
func (h hotFamily) pick(rng *lockedRand) *device {
	x := rng.Float64()
	for i, w := range h.weights {
		if x < w || i == len(h.weights)-1 {
			devices := h.accounts[i]
			return devices[(h.next[i].Add(1)-1)%uint64(len(devices))]
		}
		x -= w
	}
	return nil
}

// hotFamilyExtras: the family with the most writers writes at 80% of its
// writers' sync budgets, spread so no device or account exceeds its own.
//
// The cell's sync budget is the family's alone: its devices hold no live
// socket (whose hint pulls, one per write per socket, would draw on the
// same per-device and per-account pull budgets) and take no background
// pull, snapshot or write (excludeHotFamily).
func hotFamilyExtras(g *generator) []extraClass {
	h := *g.hot
	writers := 0
	for _, a := range h.accounts {
		writers += len(a)
	}
	var written, mutated atomic.Int64
	return []extraClass{{class: "hot_family", rate: h.rate,
		op: func(ctx context.Context, intended time.Time) {
			n := h.mutations(g.rng)
			if g.write(ctx, h.pick(g.rng), intended, RouteSyncWrite, n) == OutcomeOK {
				written.Add(1)
				mutated.Add(int64(n))
			}
		},
		report: func() {
			g.countMu.Lock()
			launched := g.launched["hot_family"]
			g.countMu.Unlock()
			measure := g.cfg.Measure.Seconds()
			cell := map[string]any{"family_id": h.family, "seeded": h.seeded, "writers": writers, "accounts": len(h.accounts),
				"target_requests_per_second": h.rate, "offered_requests_per_second": float64(launched) / measure,
				"achieved_requests_per_second": float64(written.Load()) / measure, "mutations_per_request": h.perRequest,
				"target_mutations_per_second":   h.rate * h.perRequest,
				"achieved_mutations_per_second": float64(mutated.Load()) / measure,
				"request_budget_per_second":     h.budget, "share_of_limits": hotFamilyShare, "binding_limits": h.binding,
				"excluded": "the family's devices hold no live socket and take no background pull, snapshot or write: the cell's writes are their only sync traffic"}
			if h.seeded {
				cell["note"] = fmt.Sprintf("the seeded hot family drives its whole request budget (%.3g requests a second, %.0f%% of its sync-pull limits) "+
					"with the plan's %d mutations a second, %.3g to a request on average: 50 requests a second would need 300 syncs a minute per "+
					"device, past the product's limits (120/min a device, 240/min an account)", h.rate, hotFamilyShare*100,
					hotFamilyMutationsPerSecond, h.perRequest)
			} else {
				cell["note"] = "no seeded hot family (seed --hot-family): the family with the most writers writes one mutation a request at the limits' share"
			}
			g.cell("hot_family", cell)
		}}}
}

// excludeHotFamily picks the hot family and keeps every one of its devices
// out of the live designation and the background pull, snapshot and write
// classes.
func excludeHotFamily(g *generator) {
	h := planHotFamily(g)
	g.hot = &h
	g.excluded = map[*device]bool{}
	for _, d := range g.devices {
		if d.FamilyID == h.family {
			g.excluded[d] = true
		}
	}
	keep := func(list []*device) []*device {
		var out []*device
		for _, d := range list {
			if !g.excluded[d] {
				out = append(out, d)
			}
		}
		return out
	}
	g.pullers, g.bgWriters = keep(g.devices), keep(g.writers)
}

// The flood cells (Task 19), on top of the run's traffic. Their requests
// are attacks, recorded apart from the run's routes (result.json
// attack_routes): they are refused by design and judge nothing.
//
//   - IP flood: floodIPAddresses distinct forwarded addresses, each sending
//     one POST /v1/auth/challenge and one refresh with a garbage token every
//     floodIPCycle (about 1,638 requests a second), so the two IP scope
//     pools these routes charge (floodEvictionScopes) fill and evict. A
//     pool holds limits.DefaultMaxIPEntriesPerScope buckets in 64 shards,
//     and a bucket lives for its policy's window (10 minutes), so the
//     addresses are half again the pool, enough for every shard to
//     overflow, and each returns well within a window. The other IP scopes
//     (exchange, live attempts, export, bearer failures) see none of it.
//   - Bad bearers: floodBearerAddresses other addresses, each sending
//     floodBearerBodyBytes POST /v1/sync bodies with a well-formed unknown
//     bearer at the bearer-failure-ip limit (60 per 10 minutes: 500 a
//     second). A well-formed bearer passes the format check, so the server
//     reads the body before it finds no session, and charges the address's
//     failed-bearer budget; past the budget it refuses before the body.
//
// Both use 198.18.0.0/15 (the benchmarking range, 131,072 addresses): the
// IP flood from 198.18.0.0 up, the bad bearers right after it.
const (
	floodIPAddresses     = limits.DefaultMaxIPEntriesPerScope * 3 / 2
	floodIPCycle         = 2 * time.Minute
	floodBearerAddresses = 5000
	floodIPRate          = floodIPAddresses * 2 / 120.0
	floodBearerRate      = floodBearerAddresses * 60 / 600.0
	RouteFloodChallenge  = "flood_ip_challenge"
	RouteFloodRefresh    = "flood_ip_refresh"
	RouteFloodBadBearer  = "flood_bad_bearer"
)

// floodBearerBodyBytes is a bad-bearer request's body: the sync body limit
// (BT_SYNC_BODY_BYTES' default). A variable only so the run test can send
// smaller bodies through a fake that keeps every one.
var floodBearerBodyBytes = 1 << 20

// floodBase is 198.18.0.0, the first address of 198.18.0.0/15.
const floodBase = 198<<24 | 18<<16

func floodAddressAt(n uint32) string {
	return fmt.Sprintf("%d.%d.%d.%d", n>>24, n>>16&0xff, n>>8&0xff, n&0xff)
}

// floodIPAddress is the IP flood's address i (mod floodIPAddresses).
func floodIPAddress(i int) string {
	return floodAddressAt(floodBase + uint32(i%floodIPAddresses))
}

// floodBearerAddress is the bad bearers' address i (mod
// floodBearerAddresses), after the IP flood's.
func floodBearerAddress(i int) string {
	return floodAddressAt(floodBase + floodIPAddresses + uint32(i%floodBearerAddresses))
}

// floodEvictionScopes are the IP scopes the IP flood must make evict: the
// runner checks their ip_evictions over the measurement (cells.flood).
var floodEvictionScopes = []string{limits.ScopeAuthChallengeIP, limits.ScopeAuthRefreshIP}

// floodBearerBody is a valid sync request padded with JSON whitespace to
// floodBearerBodyBytes.
func floodBearerBody() []byte {
	raw, _ := json.Marshal(syncBody(capabilitiesSync, nil, nil, nil))
	body := bytes.Repeat([]byte{' '}, max(floodBearerBodyBytes, len(raw)))
	copy(body, raw)
	return body
}

// Flood modes (--flood).
const (
	FloodIP     = "ip"
	FloodBearer = "bearer"
	FloodBoth   = "both"
)

// floodRouteFigures is one attack route's offered and achieved load over
// the measurement, and the request bytes it sent.
type floodRouteFigures struct {
	OfferedPerSecond  float64 `json:"offered_per_second"`
	AchievedPerSecond float64 `json:"achieved_per_second"`
	Sent              uint64  `json:"sent"`
	BytesSent         int64   `json:"bytes_sent"`
}

type floodCounter struct {
	sent  atomic.Uint64
	bytes atomic.Int64
}

func (c *floodCounter) add(bytes int) {
	c.sent.Add(1)
	c.bytes.Add(int64(bytes))
}

// floodExtras: the IP flood and the bad bearers (--flood: either alone or
// both). Each arrival takes the next address in turn, so over floodIPCycle
// (the IP flood) or ten minutes (the bad bearers) every address sends its
// share. The cell records each route's offered and achieved rates and
// bytes, and the generator's own CPU over the measurement: the bad
// bearers alone offer about 500 MiB/s, whose cost lands on the generator
// and the kernel, not only on the server.
func floodExtras(g *generator) []extraClass {
	mode := g.cfg.Flood
	if mode == "" {
		mode = FloodBoth
	}
	var ipNext, bearerNext atomic.Int64
	var challenges, refreshes, bearers floodCounter
	body := floodBearerBody()
	challengeBody, _ := json.Marshal(protocol.ChallengeRequest{
		RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: protocol.IdentityCapabilities()}})
	ip := func(ctx context.Context, intended time.Time) {
		n := int(ipNext.Add(1) - 1)
		d := &device{DeviceState: DeviceState{ClientIP: floodIPAddress(n / 2)}}
		if n%2 == 0 {
			challenges.add(len(challengeBody))
			res := g.c.do(ctx, request{method: http.MethodPost, path: "/v1/auth/challenge", device: d,
				capabilities: protocol.IdentityCapabilities(), body: challengeBody}, nil)
			g.attack.Done(RouteFloodChallenge, intended, res.Outcome)
			return
		}
		refresh, _ := json.Marshal(protocol.RecoverableRefreshRequest{
			RequestMeta:  protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: protocol.RefreshRecoveryCapabilities()},
			RefreshToken: "garbage-" + randomHex(16), RefreshAttemptID: newUUID(),
			DeviceContinuity: protocol.DeviceContinuityClaim{DeviceID: newUUID(), Credential: "garbage-" + randomHex(16)}})
		refreshes.add(len(refresh))
		res := g.c.do(ctx, request{method: http.MethodPost, path: "/v1/auth/refresh-recoverable", device: d,
			capabilities: protocol.RefreshRecoveryCapabilities(), body: refresh}, nil)
		g.attack.Done(RouteFloodRefresh, intended, res.Outcome)
	}
	bearer := func(ctx context.Context, intended time.Time) {
		d := &device{DeviceState: DeviceState{ClientIP: floodBearerAddress(int(bearerNext.Add(1) - 1))}}
		bearers.add(len(body))
		res := g.c.do(ctx, request{method: http.MethodPost, path: "/v1/sync", device: d, bearer: wellFormedBearer(),
			capabilities: capabilitiesSync, body: body}, nil)
		g.attack.Done(RouteFloodBadBearer, intended, res.Outcome)
	}
	userFrom, systemFrom, cpuOK := processCPU()
	report := func() {
		seconds := g.cfg.Measure.Seconds()
		figures := func(offered float64, c *floodCounter) floodRouteFigures {
			return floodRouteFigures{OfferedPerSecond: offered, AchievedPerSecond: float64(c.sent.Load()) / seconds, Sent: c.sent.Load(),
				BytesSent: c.bytes.Load()}
		}
		routes := map[string]floodRouteFigures{}
		expected := []string{}
		if mode != FloodBearer {
			routes[RouteFloodChallenge] = figures(floodIPRate/2, &challenges)
			routes[RouteFloodRefresh] = figures(floodIPRate/2, &refreshes)
			expected = floodEvictionScopes
		}
		if mode != FloodIP {
			routes[RouteFloodBadBearer] = figures(floodBearerRate, &bearers)
		}
		cell := map[string]any{"mode": mode, "routes": routes, "ip_addresses": floodIPAddresses, "ip_rate": floodIPRate,
			"ip_cycle_seconds": floodIPCycle.Seconds(), "evictions_expected": expected,
			"bearer_addresses": floodBearerAddresses, "bearer_rate": floodBearerRate, "bearer_body_bytes": floodBearerBodyBytes}
		if user, system, ok := processCPU(); ok && cpuOK {
			cell["generator_user_cpu_seconds"] = (user - userFrom).Seconds()
			cell["generator_system_cpu_seconds"] = (system - systemFrom).Seconds()
		}
		g.cell("flood", cell)
	}
	var classes []extraClass
	if mode != FloodBearer {
		classes = append(classes, extraClass{class: "flood_ip", rate: floodIPRate, op: ip, attack: true})
	}
	if mode != FloodIP {
		classes = append(classes, extraClass{class: "flood_bearer", rate: floodBearerRate, op: bearer, attack: true})
	}
	classes[0].report = report
	return classes
}

// wellFormedBearer is a random bearer of the server's access-token format
// (raw base64url of 32 bytes) that names no session.
func wellFormedBearer() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// --- export -----------------------------------------------------------------------

// Export scenario shape (the plan's 5 slow downloads at 100 KiB/s).
const (
	exportCount        = 5
	exportReadRate     = 100 << 10 // bytes per second
	exportPollInterval = 2 * time.Second
	exportReadyLimit   = 5 * time.Minute
	// Routes of the export cell.
	RouteExportCreate   = "export_create"
	RouteExportStatus   = "export_status"
	RouteExportDownload = "export_download"
)

// exportExtras: 5 owners of distinct families request an export, poll it
// until ready and download it at 100 KiB/s, started over the first fifth
// of the measurement.
func exportExtras(g *generator) []extraClass {
	owners := distinctOwners(g, exportCount, 0xe4)
	window := g.cfg.Measure / 5
	var next atomic.Int64
	return []extraClass{{class: "export", rate: float64(len(owners)) / window.Seconds(), length: window,
		op: func(ctx context.Context, intended time.Time) {
			i := int(next.Add(1)) - 1
			if i >= len(owners) {
				return
			}
			g.slowExport(ctx, owners[i], intended, exportReadRate)
		}}}
}

// exportResult is one export's end: downloaded whole, its bytes, and when
// it was ready.
type exportResult struct {
	ok    bool
	bytes int64
	ready time.Time
}

// slowExport creates d's export, polls it ready and downloads it at rate
// bytes a second (0: as fast as it comes).
func (g *generator) slowExport(ctx context.Context, d *device, intended time.Time, rate int) (out exportResult) {
	body := protocol.CreateExportRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: capabilitiesExport},
		RequestID: newUUID()}
	var status protocol.ExportStatus
	res := g.sendWithRaces(ctx, RouteExportCreate, intended, request{method: http.MethodPost, path: "/v1/export", device: d,
		session: d, capabilities: capabilitiesExport, body: body}, &status)
	if !res.ok() {
		return out
	}
	deadline := time.Now().Add(exportReadyLimit)
	for status.State != "ready" {
		if status.State == "failed" || status.State == "expired" || time.Now().After(deadline) {
			g.record(RouteExportStatus, time.Now(), OutcomeRejected)
			return out
		}
		if !sleepFor(ctx, exportPollInterval) {
			return out
		}
		res = g.sendWithRaces(ctx, RouteExportStatus, time.Now(), request{method: http.MethodGet, path: "/v1/export/" + status.ExportID,
			device: d, session: d, capabilities: capabilitiesExport}, &status)
		if !res.ok() {
			return out
		}
	}
	out.ready = time.Now()
	started := time.Now()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, g.c.base+"/v1/export/"+status.ExportID+"/download", nil)
	if err != nil {
		return out
	}
	// The session is held until the answer's headers: the download is
	// authorised then, and streams on after it.
	release, waited := d.holdSession()
	g.noteSessionWait(RouteExportDownload, waited)
	started = started.Add(waited)
	g.c.decorate(request, requestOf(d, capabilitiesExport))
	// A slow reader outlives the 10 s request deadline by design: only the
	// run's context bounds it.
	slow := &http.Client{Transport: g.c.http.Transport}
	response, err := slow.Do(request)
	release()
	if err != nil {
		g.record(RouteExportDownload, started, transportOutcome(ctx, err))
		return out
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		g.record(RouteExportDownload, started, readResponse(ctx, response, nil).Outcome)
		return out
	}
	outcome := OutcomeOK
	size := 64 << 10
	if rate > 0 {
		size = rate / 10
	}
	chunk := make([]byte, size)
	for {
		n, err := io.ReadFull(response.Body, chunk)
		out.bytes += int64(n)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			outcome = transportOutcome(ctx, err)
			break
		}
		if rate > 0 && !sleepFor(ctx, 100*time.Millisecond) {
			outcome = OutcomeTimeout
			break
		}
	}
	g.record(RouteExportDownload, started, outcome)
	out.ok = outcome == OutcomeOK
	return out
}

func requestOf(d *device, capabilities []string) request {
	return request{device: d, bearer: d.access(), capabilities: capabilities}
}

// rngPerm is a permutation of n by the source.
func (r *lockedRand) rngPerm(n int) []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rng.Perm(n)
}
