package loadtest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/protocol"
)

// The generator's per-request overhead, measured against an in-process
// server that answers from bytes built in advance (its own cost is a copy),
// so what these benchmarks see is the generator's: encoding the request,
// reading and decoding the answer, the family check, the tracked map, the
// recorder, the dispatcher. Run with -benchmem; `go test -run '^$' -bench .
// ./internal/loadtest`.

// benchHistory is the changes a benchmark family holds: about the R10k
// seed's 1,100 care entities per family, plus updates.
const benchHistory = 1200

// pageServer answers POST /v1/sync from each family's changes, encoded in
// advance one by one: a page is the changes after the request's cursor (or
// its page token's resume point), at most 200, assembled by copying.
type pageServer struct {
	*httptest.Server
	instance string
	changes  map[string][][]byte // family -> encoded changes, sequence i+1 at i
	pageSize int
}

func newPageServer(tb testing.TB, g *generator, history int) *pageServer {
	tb.Helper()
	s := &pageServer{instance: g.header.ServerInstanceID, changes: map[string][][]byte{}, pageSize: 200}
	for _, d := range g.devices {
		if _, ok := s.changes[d.FamilyID]; ok {
			continue
		}
		s.changes[d.FamilyID] = benchChanges(tb, g, d, history)
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	tb.Cleanup(s.Close)
	return s
}

// benchChanges is history changes of d's family as the server encodes them:
// a care entity each (the write mix's kinds and payloads), every tenth a
// tombstone.
func benchChanges(tb testing.TB, g *generator, d *device, history int) [][]byte {
	tb.Helper()
	out := make([][]byte, 0, history)
	for seq := 1; seq <= history; seq++ {
		p := g.planMutation(d)
		if p == nil {
			tb.Fatal("no mutation to plan")
		}
		o := fakeOutcome(g.header.ServerInstanceID, d.FamilyID, p.m, uint64(seq))
		entity := *o.CanonicalEntity
		operation := p.m.Operation
		if seq%10 == 0 {
			accepted := entity.Meta.AcceptedAt
			entity.Payload, entity.Meta.TombstonedAt, operation = nil, &accepted, "delete"
		}
		change := protocol.JournalChange{Cursor: *o.Cursor, EntityKind: entity.EntityKind, EntityID: entity.Meta.ID,
			EntityVersion: entity.Meta.EntityVersion, ActorAccountID: entity.Meta.LastEditorAccountID, DeviceID: d.DeviceID,
			Operation: operation, AcceptedAt: entity.Meta.AcceptedAt, SnapshotOrTombstone: entity}
		raw, err := json.Marshal(change)
		if err != nil {
			tb.Fatal(err)
		}
		out = append(out, raw)
	}
	return out
}

// pageBuffers keep the page server's own allocations out of the
// benchmarks' B/op.
var pageBuffers = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func (s *pageServer) serve(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Cursor    *protocol.Cursor `json:"cursor"`
		PageToken *string          `json:"page_token"`
	}
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &request); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var family string
	var from int
	switch {
	case request.PageToken != nil:
		family, from = (*request.PageToken)[:36], 0
		from, _ = strconv.Atoi((*request.PageToken)[37:])
	case request.Cursor != nil:
		family, from = request.Cursor.FamilyID, int(request.Cursor.Sequence)
	}
	changes := s.changes[family]
	from = min(from, len(changes))
	to := min(from+s.pageSize, len(changes))
	b := pageBuffers.Get().(*bytes.Buffer)
	defer pageBuffers.Put(b)
	b.Reset()
	fmt.Fprintf(b, `{"server_instance_id":%q,"recovery_epoch":1,"outcomes":[],"changes":[`, s.instance)
	for i, raw := range changes[from:to] {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(raw)
	}
	fmt.Fprintf(b, `],"high_water":{"server_instance_id":%q,"recovery_epoch":1,"family_id":%q,"sequence":%d},"next_page_token":`,
		s.instance, family, len(changes))
	if to < len(changes) {
		fmt.Fprintf(b, `"%s:%d"}`, family, to)
	} else {
		b.WriteString(`null}`)
	}
	b.WriteByte('\n')
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(b.Len()))
	_, _ = w.Write(b.Bytes())
}

// benchGenerator is testGenerator against base, with every device at its
// family's head (history).
func benchGenerator(tb testing.TB, families, history int) (*generator, *pageServer) {
	tb.Helper()
	dir, header, states := fakeFleet(tb, families)
	for i := range states {
		states[i].Cursor = uint64(history)
	}
	cfg := RunConfig{Base: "http://127.0.0.1:1", Host: "sync.load.test", Fleet: filepath.Join(dir, "fleet.jsonl"), Ledger: filepath.Join(dir, "ledger.jsonl"),
		Tracked: filepath.Join(dir, "tracked.jsonl"), Out: filepath.Join(dir, "result.json"), Scenario: "soak", Load: 1, Seed: 7,
		Measure: time.Minute, WithPump: true}
	cfg.defaults()
	g := newGenerator(cfg, header, states)
	ledger, err := CreateLedger(cfg.Ledger)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = ledger.Close() })
	g.ledger = ledger
	g.work = tb.Context()
	g.phases = newPhases(time.Now().Add(-time.Hour))
	s := newPageServer(tb, g, history)
	g.c = newClient(s.URL, cfg.Host)
	return g, s
}

// BenchmarkPullFrom is one pull of the cursor mix's shapes: at head (an
// empty page), 25 changes, 450 changes (3 pages) and the whole history
// (6 pages), by an untracked device and by a tracked one.
func BenchmarkPullFrom(b *testing.B) {
	for _, shape := range []struct {
		name   string
		rewind int
	}{{"at_head", 0}, {"small_25", 25}, {"large_450", 450}, {"far_1200", benchHistory}} {
		for _, tracked := range []bool{false, true} {
			name := shape.name + "/untracked"
			if tracked {
				name = shape.name + "/tracked"
			}
			b.Run(name, func(b *testing.B) {
				g, _ := benchGenerator(b, 1, benchHistory)
				d := g.devices[0]
				if tracked {
					d.tracked = newTrackedState()
				}
				from := uint64(benchHistory - shape.rewind)
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if !g.pullFrom(g.work, d, from, time.Now(), RouteSyncPull) {
						b.Fatalf("the pull failed: %v", g.outcomes(RouteSyncPull))
					}
				}
				if shape.rewind > 0 {
					b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(shape.rewind), "ns/change")
				}
			})
		}
	}
}

// BenchmarkDispatcherArrival is the dispatcher's own cost per arrival
// (virtual time, an operation that does nothing): the lag record, the
// Observe hook into the phases, the in-flight slot and the goroutine.
func BenchmarkDispatcherArrival(b *testing.B) {
	p := newPhases(time.Now().Add(-time.Hour))
	p.addWindow("w", 1, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	start := time.Now()
	clk := &fakeClock{now: start}
	d := &Dispatcher{Start: start, MaxInFlight: 1 << 40, Observe: p.observe, clock: clk}
	const rate = 1e6
	b.ReportAllocs()
	b.ResetTimer()
	d.Run(b.Context(), NewArrivals(rate, 1), time.Duration(float64(b.N)/rate*float64(time.Second)), func(context.Context, time.Time) {})
	d.Wait()
}

// BenchmarkCheckpoint is the minute's fleet checkpoint at the 10k node's
// size (40,000 devices): its CPU and garbage run beside the dispatchers.
func BenchmarkCheckpoint(b *testing.B) {
	dir, header, states := fakeFleet(b, 10000)
	g := newGenerator(RunConfig{Fleet: filepath.Join(dir, "fleet.jsonl"), Seed: 1}, header, states)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := g.checkpoint(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRecorderDone is one recorded request, from every core at once.
func BenchmarkRecorderDone(b *testing.B) {
	rec := NewRecorder()
	intended := time.Now()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rec.Done(RouteSyncPull, intended, OutcomeOK)
		}
	})
}

// BenchmarkLedgerAppend is one acknowledged write ledgered, from every core
// at once.
func BenchmarkLedgerAppend(b *testing.B) {
	ledger, err := CreateLedger(filepath.Join(b.TempDir(), "ledger.jsonl"))
	if err != nil {
		b.Fatal(err)
	}
	defer ledger.Close()
	entry := LedgerEntry{FamilyID: "f0000001-0000-4000-8000-000000000000", EntityID: "e0000001-0000-4000-8000-000000000000",
		Kind: "feed", Version: 3, WireSHA256: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := ledger.Append(entry); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkSchedulerLag runs the pull class open loop on the wall clock for
// a few seconds (the cursor mix, against the page server) and reports the
// dispatcher's release lag, the process CPU per second and the GC cycles:
// the generator's own health under its pull work, with every core
// (procs=all) and squeezed onto two (procs=2), where per-request CPU shows
// as lag. The rate is the model's pull rate at 2x (1,200/s) and twice that.
// One iteration is one run: use -benchtime 1x.
func BenchmarkSchedulerLag(b *testing.B) {
	for _, procs := range []int{0, 2} {
		for _, rate := range []float64{1200, 2400} {
			name := fmt.Sprintf("procs=%s/rate=%g", map[bool]string{true: "all", false: strconv.Itoa(procs)}[procs == 0], rate)
			b.Run(name, func(b *testing.B) {
				if procs > 0 {
					defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(procs))
				}
				g, _ := benchGenerator(b, 50, benchHistory)
				for i, d := range g.devices {
					if i%25 == 0 {
						d.tracked = newTrackedState() // 8 of 200, about the run's 200 of 40,000 (x4)
					}
				}
				const length = 5 * time.Second
				var lag Histogram
				var cpu time.Duration
				var gcs uint32
				for b.Loop() {
					var msBefore, msAfter runtime.MemStats
					runtime.GC()
					runtime.ReadMemStats(&msBefore)
					userBefore, systemBefore, _ := processCPU()
					d := &Dispatcher{Start: time.Now().Add(10 * time.Millisecond), MaxInFlight: maxInFlight}
					d.Run(g.work, NewArrivals(rate, 0x51), length, g.opPull)
					d.Wait()
					userAfter, systemAfter, _ := processCPU()
					runtime.ReadMemStats(&msAfter)
					l := d.LagHistogram()
					lag.Merge(&l)
					cpu += userAfter - userBefore + systemAfter - systemBefore
					gcs += msAfter.NumGC - msBefore.NumGC
				}
				seconds := length.Seconds() * float64(b.N)
				b.ReportMetric(float64(lag.Quantile(0.5))/1e6, "lag-p50-ms")
				b.ReportMetric(float64(lag.Quantile(0.99))/1e6, "lag-p99-ms")
				b.ReportMetric(float64(lag.Quantile(1))/1e6, "lag-max-ms")
				b.ReportMetric(cpu.Seconds()/seconds*100, "cpu-%")
				b.ReportMetric(float64(gcs)/seconds, "gc/s")
				if bad := badOutcomes(g); bad != "" {
					b.Logf("pull outcomes other than ok: %s", bad)
				}
			})
		}
	}
}

// badOutcomes is the pull outcomes other than ok ("" when none).
func badOutcomes(g *generator) string {
	var out []string
	for outcome, n := range g.outcomes(RouteSyncPull) {
		if outcome != OutcomeOK && n > 0 {
			out = append(out, fmt.Sprintf("%s=%d", outcome, n))
		}
	}
	return strings.Join(out, " ")
}
