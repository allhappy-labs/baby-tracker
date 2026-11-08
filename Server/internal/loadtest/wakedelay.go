package loadtest

import (
	"sync"
	"time"
)

// The hub-wake delay (Task 18 Step 1): for every live device of a family
// another device wrote to, the time from that write's acknowledgement to
// the live hint that covers it (the hint's cursor reaches the write's
// sequence). The server publishes the hint once the write commits, so the
// delay is the hub's fan-out plus the generator's own read loop. It is
// measured from the writer reading its answer, not from the commit: it
// under-states by the answer's way back, and a hint read before the writer
// read its answer counts as 0 (hint_before_ack).
//
// Each (device, write) pair is counted once, whichever arrives second: the
// hint (the write is pending at the device) or the acknowledgement (the
// device's last hint already covers it). Writes acknowledged before the
// device's socket opened are not its to wait for. Only writes acknowledged
// inside the measurement are counted. A pair whose hint has not come
// wakeMissingAfter after the acknowledgement is counted missing (a hub
// that drops hints must not look fast); one whose socket closed first is
// counted apart; one still waiting when the run ends, younger than that,
// is reported as pending.

// wakeMissingAfter is how long a live device may wait for a write's hint
// before the pair counts as missing.
const wakeMissingAfter = 30 * time.Second

// wakeAckKeep bounds a family's kept acknowledgements whatever their age
// (the hot family writes 16 a second: 480 in wakeMissingAfter).
const wakeAckKeep = 4096

// wakeAck is one acknowledged write: the family sequence its outcome
// reached, when the writer read the answer, the writer, and the live
// sockets still waiting for its hint.
type wakeAck struct {
	seq     uint64
	at      time.Time
	writer  *device
	pending map[*device]bool
}

// hintMark is a live socket's hint progress: the highest sequence a hint
// covered and when it arrived, and when the socket opened.
type hintMark struct {
	conn   *liveConn
	seq    uint64
	at     time.Time
	opened time.Time
}

// familyWake is a family's recent acknowledgements and its live devices'
// hint marks, under familyState.mu.
type familyWake struct {
	acks  []*wakeAck
	marks map[*device]*hintMark
}

// hubWake is the run's hub-wake delay.
type hubWake struct {
	mu        sync.Mutex
	hist      Histogram
	beforeAck uint64
	pairs     uint64
	missing   uint64 // pairs whose hint did not come within wakeMissingAfter
	closed    uint64 // pairs whose socket closed before their hint
	pending   uint64 // pairs still waiting at the end, younger than wakeMissingAfter
	evicted   uint64 // waiting pairs pushed out by wakeAckKeep (not judged)
}

// HubWakeDelay is result.json's hub_wake_delay.
type HubWakeDelay struct {
	Pairs               uint64    `json:"pairs"`
	HintBeforeAck       uint64    `json:"hint_before_ack"`
	Missing             uint64    `json:"missing"`
	MissingAfterSeconds float64   `json:"missing_after_seconds"`
	SocketClosed        uint64    `json:"socket_closed_before_hint"`
	PendingAtEnd        uint64    `json:"pending_at_end"`
	Evicted             uint64    `json:"evicted_pending"`
	P50Ms               float64   `json:"p50_ms"`
	P95Ms               float64   `json:"p95_ms"`
	P99Ms               float64   `json:"p99_ms"`
	MaxMs               float64   `json:"max_ms"`
	Histogram           Histogram `json:"histogram"`
	Note                string    `json:"note"`
}

func (w *hubWake) record(delay time.Duration) {
	w.mu.Lock()
	w.pairs++
	if delay < 0 {
		w.beforeAck++
		delay = 0
	}
	w.hist.Record(delay)
	w.mu.Unlock()
}

func (w *hubWake) add(counter *uint64, n int) {
	if n == 0 {
		return
	}
	w.mu.Lock()
	*counter += uint64(n)
	w.mu.Unlock()
}

func (w *hubWake) result() *HubWakeDelay {
	w.mu.Lock()
	defer w.mu.Unlock()
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	return &HubWakeDelay{Pairs: w.pairs, HintBeforeAck: w.beforeAck, Missing: w.missing, MissingAfterSeconds: wakeMissingAfter.Seconds(),
		SocketClosed: w.closed, PendingAtEnd: w.pending, Evicted: w.evicted, P50Ms: ms(w.hist.Quantile(0.50)),
		P95Ms: ms(w.hist.Quantile(0.95)), P99Ms: ms(w.hist.Quantile(0.99)), MaxMs: float64(w.hist.MaxMicros) / 1000, Histogram: w.hist,
		Note: "hint arrival at each live device minus the acknowledgement of a write another device of its family made inside the measurement " +
			"(sync_write and hot-family writes), measured from the writer reading its answer, not from the commit (it under-states by the " +
			"answer's way back); a hint read before the writer read its answer counts as 0 (hint_before_ack); a hint that has not come " +
			"missing_after_seconds after the acknowledgement is missing, and is not in the percentiles"}
}

// expire settles fam's acknowledgements older than wakeMissingAfter at now
// (their waiting sockets count missing) and keeps the rest; fam.mu held.
func (g *generator) expire(fam *familyState, now time.Time) {
	keep := fam.wake.acks[:0]
	for _, a := range fam.wake.acks {
		if now.Sub(a.at) >= wakeMissingAfter {
			g.wake.add(&g.wake.missing, len(a.pending))
			continue
		}
		keep = append(keep, a)
	}
	clear(fam.wake.acks[len(keep):])
	fam.wake.acks = keep
}

// noteSocket starts d's hint mark when its socket lc opens: the writes its
// family acknowledged before are not its to wait for.
func (g *generator) noteSocket(d *device, lc *liveConn, opened time.Time) {
	fam := g.families[d.FamilyID]
	if fam == nil {
		return
	}
	fam.mu.Lock()
	defer fam.mu.Unlock()
	mark := &hintMark{conn: lc, opened: opened}
	for _, a := range fam.wake.acks {
		mark.seq = max(mark.seq, a.seq)
	}
	if fam.wake.marks == nil {
		fam.wake.marks = map[*device]*hintMark{}
	}
	fam.wake.marks[d] = mark
}

// forgetSocket ends d's hint mark when its socket lc closes (a socket that
// replaced lc keeps its own): the writes it still waited for are counted
// apart, not as missing.
func (g *generator) forgetSocket(d *device, lc *liveConn) {
	fam := g.families[d.FamilyID]
	if fam == nil {
		return
	}
	fam.mu.Lock()
	defer fam.mu.Unlock()
	mark := fam.wake.marks[d]
	if mark == nil || mark.conn != lc {
		return
	}
	delete(fam.wake.marks, d)
	closed := 0
	for _, a := range fam.wake.acks {
		if a.pending[d] {
			delete(a.pending, d)
			closed++
		}
	}
	g.wake.add(&g.wake.closed, closed)
}

// noteAck records writer's acknowledged write at seq: every live device of
// the family whose last hint already covered it is counted now; the others
// wait for their hint.
func (g *generator) noteAck(writer *device, seq uint64, at time.Time) {
	if seq == 0 || !g.phases.inMeasure(at) {
		return
	}
	fam := g.families[writer.FamilyID]
	if fam == nil {
		return
	}
	fam.mu.Lock()
	defer fam.mu.Unlock()
	g.expire(fam, at)
	a := &wakeAck{seq: seq, at: at, writer: writer, pending: map[*device]bool{}}
	for d, mark := range fam.wake.marks {
		if d == writer || at.Before(mark.opened) {
			continue
		}
		if mark.seq >= seq {
			g.wake.record(mark.at.Sub(at))
		} else {
			a.pending[d] = true
		}
	}
	if len(fam.wake.acks) == wakeAckKeep {
		g.wake.add(&g.wake.evicted, len(fam.wake.acks[0].pending))
		fam.wake.acks[0] = nil
		fam.wake.acks = fam.wake.acks[1:]
	}
	fam.wake.acks = append(fam.wake.acks, a)
}

// noteHint records a hint at d reaching seq: every acknowledged write d
// waited for, up to seq, is counted.
func (g *generator) noteHint(d *device, seq uint64, at time.Time) {
	fam := g.families[d.FamilyID]
	if fam == nil {
		return
	}
	fam.mu.Lock()
	defer fam.mu.Unlock()
	mark := fam.wake.marks[d]
	if mark == nil || seq <= mark.seq {
		return
	}
	// A pair older than wakeMissingAfter is missing, whether or not another
	// event of the family expired it first.
	g.expire(fam, at)
	for _, a := range fam.wake.acks {
		if a.seq <= seq && a.pending[d] {
			delete(a.pending, d)
			g.wake.record(at.Sub(a.at))
		}
	}
	mark.seq, mark.at = seq, at
}

// finishWake settles every family's waiting pairs at the run's end: older
// than wakeMissingAfter, missing; younger, pending.
func (g *generator) finishWake(now time.Time) {
	for _, fam := range g.families {
		fam.mu.Lock()
		g.expire(fam, now)
		for _, a := range fam.wake.acks {
			g.wake.add(&g.wake.pending, len(a.pending))
			clear(a.pending)
		}
		fam.mu.Unlock()
	}
}
