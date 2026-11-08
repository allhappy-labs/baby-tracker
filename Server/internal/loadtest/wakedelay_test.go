package loadtest

import (
	"testing"
	"time"
)

// The hub-wake delay pairs each acknowledged write with the hint that covers
// it at each other live device of its family, once, whichever comes second:
// the writer's own hint, a write acknowledged before the socket opened and
// a write outside the measurement are not counted; a hint read before the
// writer read its answer counts as 0 (hint_before_ack).
func TestHubWakeDelayPairsEachWriteWithTheHintThatCoversIt(t *testing.T) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	writer, reader, late := g.devices[0], g.devices[1], g.devices[2]
	t0 := time.Now()
	g.phases = newPhases(t0.Add(-time.Hour))
	readerConn, lateConn, writerConn := &liveConn{}, &liveConn{}, &liveConn{}
	g.noteSocket(reader, readerConn, t0)
	g.noteSocket(writer, writerConn, t0)

	g.noteAck(writer, 3, t0.Add(10*time.Millisecond))
	g.noteAck(writer, 4, t0.Add(20*time.Millisecond))
	g.noteHint(writer, 4, t0.Add(70*time.Millisecond))  // its own writes: not counted
	g.noteHint(reader, 4, t0.Add(120*time.Millisecond)) // covers 3 (110 ms) and 4 (100 ms)
	g.noteHint(reader, 4, t0.Add(500*time.Millisecond)) // nothing new
	g.noteSocket(late, lateConn, t0.Add(200*time.Millisecond))
	g.noteHint(late, 4, t0.Add(210*time.Millisecond)) // acknowledged before its socket opened

	// A hint read before the writer read its answer: counted at the answer, as 0.
	g.noteHint(reader, 5, t0.Add(600*time.Millisecond))
	g.noteAck(writer, 5, t0.Add(650*time.Millisecond))
	g.noteHint(reader, 6, t0.Add(900*time.Millisecond)) // 5 is already counted
	// Another device's write reaches the late socket.
	g.noteAck(reader, 6, t0.Add(700*time.Millisecond))
	g.noteHint(late, 6, t0.Add(1000*time.Millisecond))

	r := g.wake.result()
	// reader: 3 (110), 4 (100), 5 (before ack); writer's socket: 6 by the
	// reader (no hint yet); late: 5 (350) and 6 (300).
	if r.Pairs != 5 || r.HintBeforeAck != 1 {
		t.Fatalf("pairs %d, before ack %d", r.Pairs, r.HintBeforeAck)
	}
	if r.MaxMs < 349 || r.MaxMs > 351 {
		t.Fatalf("max %v ms, want 350 (the late socket's)", r.MaxMs)
	}
	// The reader's socket closes; a hint on it no longer counts, and a
	// write outside the measurement counts nowhere.
	g.noteAck(writer, 7, t0.Add(1020*time.Millisecond))
	g.forgetSocket(reader, readerConn)
	g.noteHint(reader, 7, t0.Add(1040*time.Millisecond))
	g.noteHint(late, 7, t0.Add(1045*time.Millisecond)) // the late socket's: counted (25 ms)
	g.phases.setMeasureEnd(t0.Add(1050 * time.Millisecond))
	g.noteAck(writer, 8, t0.Add(1200*time.Millisecond))
	g.noteHint(late, 8, t0.Add(1300*time.Millisecond))
	if r := g.wake.result(); r.Pairs != 6 || r.SocketClosed != 1 {
		t.Fatalf("pairs %d, closed %d after the close and the measurement's end", r.Pairs, r.SocketClosed)
	}
	// The writer's own socket never got the hint for the reader's write 6:
	// 40 s on it is missing, not fast.
	g.finishWake(t0.Add(40 * time.Second))
	if r := g.wake.result(); r.Missing != 1 || r.Pairs != 6 || r.PendingAtEnd != 0 {
		t.Fatalf("missing %d, pairs %d, pending %d", r.Missing, r.Pairs, r.PendingAtEnd)
	}
	// A renewal's new socket keeps its mark when the old one's reader ends.
	newConn := &liveConn{}
	g.noteSocket(late, newConn, t0.Add(1400*time.Millisecond))
	g.forgetSocket(late, lateConn)
	if g.families[late.FamilyID].wake.marks[late] == nil {
		t.Fatal("the old socket's close dropped the new socket's mark")
	}
}

// A slow hint is kept for wakeMissingAfter whatever the family's write
// rate (the hot family's 16 a second would have pushed it out of a short
// list); a hint younger than that at the run's end is pending, not missing.
func TestHubWakeDelayKeepsSlowHintsAndCountsPending(t *testing.T) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	writer, reader := g.devices[0], g.devices[1]
	t0 := time.Now()
	g.phases = newPhases(t0.Add(-time.Hour))
	g.noteSocket(reader, &liveConn{}, t0)
	g.noteAck(writer, 1, t0)
	for i := range 400 { // 16 a second for 24 s
		g.noteAck(writer, uint64(2+i), t0.Add(time.Duration(i)*60*time.Millisecond))
	}
	g.noteHint(reader, 400, t0.Add(25*time.Second))
	for i := range 100 { // and on to 31 s, unhinted
		g.noteAck(writer, uint64(402+i), t0.Add(25*time.Second+time.Duration(i)*60*time.Millisecond))
	}
	r := g.wake.result()
	if r.Pairs != 400 || r.Missing != 0 || r.Evicted != 0 || r.MaxMs < 24999 || r.MaxMs > 25001 {
		t.Fatalf("pairs %d (want 400, the first 25 s later), missing %d, evicted %d, max %v ms", r.Pairs, r.Missing, r.Evicted, r.MaxMs)
	}
	// At the end (t0 + 40 s) writes 401..501 are 9 to 17 s old: pending.
	g.finishWake(t0.Add(40 * time.Second))
	if r := g.wake.result(); r.PendingAtEnd+r.Missing != 101 || r.Missing != 0 {
		t.Fatalf("pending %d, missing %d, want 101 pending", r.PendingAtEnd, r.Missing)
	}
}

// A hint 35 s after its write is missing, not a 35 s delay, even when
// nothing else of the family ran in between.
func TestHubWakeDelayCountsALateHintMissing(t *testing.T) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	writer, reader := g.devices[0], g.devices[1]
	t0 := time.Now()
	g.phases = newPhases(t0.Add(-time.Hour))
	g.noteSocket(reader, &liveConn{}, t0)
	g.noteAck(writer, 1, t0.Add(time.Second))
	g.noteHint(reader, 1, t0.Add(36*time.Second))
	if r := g.wake.result(); r.Pairs != 0 || r.Missing != 1 {
		t.Fatalf("a 35 s hint: pairs %d, missing %d, max %v ms", r.Pairs, r.Missing, r.MaxMs)
	}
}

// End to end against a fake hub that sends each write's hint 150 ms after
// answering it: result.json's hub_wake_delay holds the pairs, at about
// 150 ms.
func TestRunReportsTheHubWakeDelay(t *testing.T) {
	f := newFakeV1(t)
	f.hintDelay = 150 * time.Millisecond
	cfg := runConfig(t, f, 4, Model{RefreshPerSecond: 0.5, PullPerSecond: 2, WritePerSecond: 8, LiveSockets: 16})
	cfg.Warmup, cfg.Measure = 500*time.Millisecond, 3*time.Second
	if _, err := Run(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	r := readResult(t, cfg.Out)
	w := r.HubWakeDelay
	if w == nil || w.Pairs < 10 || w.P50Ms < 150 || w.P50Ms > 600 || w.Histogram.Total != w.Pairs {
		t.Fatalf("hub_wake_delay %+v", w)
	}
}
