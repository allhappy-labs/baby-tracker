package main

import (
	"babytracker/server/internal/diagnostics"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/realtime"
	"babytracker/server/internal/wake"
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5/pgxpool"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestResourceSamplerNeverSamplesRequestPhasesAndJoins(t *testing.T) {
	var reads atomic.Int32
	var c diagnostics.CapacityCollector
	sampler := resourceSampler{collector: &c, read: func() diagnostics.ResourceSample { reads.Add(1); return diagnostics.ResourceSample{} }, emit: func(diagnostics.ResourceSample) {}}
	ctx := diagnostics.WithRecorder(context.Background(), c.Recorder())
	for i := 0; i < 1000; i++ {
		diagnostics.Capture(ctx, diagnostics.SecurityFence, time.Millisecond)
		diagnostics.Record(ctx, nil, diagnostics.Database, time.Millisecond)
	}
	diagnostics.Flush(ctx)
	if reads.Load() != 0 {
		t.Fatal("request observed system state")
	}
	run, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); sampler.RunResourceSampler(run, 5*time.Millisecond) }()
	deadline := time.Now().Add(time.Second)
	for reads.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	n := reads.Load()
	if n < 2 {
		t.Fatal("sampler did not tick")
	}
	time.Sleep(10 * time.Millisecond)
	if reads.Load() != n {
		t.Fatal("sampler survived cancellation")
	}
}

// The sample carries the runtime, the limiter's capacity events, the live
// hub's occupancy and the wake worker's totals: counts under fixed labels
// only (the limiter's IP scope names), never a key, token or identifier.
func TestResourceSampleCarriesLimiterLiveAndWake(t *testing.T) {
	limiter, err := limits.New(limits.Options{Pepper: [32]byte{1}, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	service := limiter.(*limits.Service)
	var sweeps diagnostics.Histogram
	sweeps.Observe(2 * time.Second)
	sources := resourceSources{
		Limiter: service.Stats,
		Live: func() realtime.HubStats {
			return realtime.HubStats{Leases: 3, Reservations: 1, SweepDuration: sweeps, SweepClosed: 5}
		},
		TrackerLen:     func() int { return 7 },
		TrackerDropped: func() uint64 { return 2 },
		Wake: func() wake.Totals {
			return wake.Totals{Ticks: 10, Flushed: 9, Woken: 8, Pushed: 7, PushFailed: 6, NotSent: 5, Ambiguous: 4, SettlesPending: 3, SettlesFailed: 1,
				Families: 12, FamiliesPeak: 5, TickDurationNS: diagnostics.Histogram{Count: 2, TotalNS: 30e6, Buckets: [12]uint64{4: 1, 5: 1}}}
		},
	}
	sample := readResources(nil, sources)
	if sample.Runtime == nil || sample.Runtime.GOMAXPROCS != runtime.GOMAXPROCS(0) || sample.Runtime.GOMEMLIMIT != debug.SetMemoryLimit(-1) ||
		sample.Runtime.HeapInuse == 0 || sample.Runtime.HeapObjects == 0 {
		t.Fatalf("runtime %+v", sample.Runtime)
	}
	if sample.Limiter == nil || len(sample.Limiter.IPEvictions) == 0 {
		t.Fatalf("limiter %+v", sample.Limiter)
	}
	policies := limits.DefaultPolicies()
	for scope := range sample.Limiter.IPEvictions {
		if _, fixed := policies[scope]; !fixed {
			t.Fatalf("limiter label %q is not a fixed scope", scope)
		}
	}
	if sample.Live == nil || *sample.Live != (diagnostics.LiveSample{Leases: 3, Reservations: 1, SweepDurationNS: sweeps, SweepClosed: 5}) {
		t.Fatalf("live %+v", sample.Live)
	}
	want := diagnostics.WakeSample{TrackerEntries: 7, TrackerDropped: 2, Ticks: 10, Flushed: 9, Woken: 8, Pushed: 7, PushFailed: 6,
		NotSent: 5, Ambiguous: 4, SettlesPending: 3, SettlesFailed: 1, Families: 12, FamiliesPeak: 5,
		TickDurationNS: diagnostics.Histogram{Count: 2, TotalNS: 30e6, Buckets: [12]uint64{4: 1, 5: 1}}}
	if sample.Wake == nil || *sample.Wake != want {
		t.Fatalf("wake %+v, want %+v", sample.Wake, want)
	}
	encoded, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"runtime":{"heap_inuse_bytes":`, `"gomaxprocs":`, `"limiter":{"verified_full":0,"ip_evictions":{`,
		`"live":{"leases":3,"reservations":1,"sweep_duration_ns":{"count":1,"total_ns":2000000000,"buckets":[0,0,0,0,0,0,0,0,0,1,0,0]},"sweep_closed":5}`, `"wake":{"tracker_entries":7,"tracker_dropped":2,"ticks":10,`,
		`"families":12,"families_peak":5,"tick_duration_ns":{"count":2,"total_ns":30000000,"buckets":[0,0,0,0,1,1,0,0,0,0,0,0]}`} {
		if !strings.Contains(string(encoded), key) {
			t.Fatalf("encoded sample lacks %s: %s", key, encoded)
		}
	}
	// Sources not composed (diagnostics on, wakes off in a unit test) are
	// omitted, not zero.
	bare := readResources(nil, resourceSources{})
	if bare.Runtime == nil || bare.Limiter != nil || bare.Live != nil || bare.Wake != nil {
		t.Fatalf("bare sample %+v", bare)
	}
	encoded, _ = json.Marshal(bare)
	for _, key := range []string{`"limiter"`, `"live"`, `"wake"`} {
		if strings.Contains(string(encoded), key) {
			t.Fatalf("bare sample encodes %s: %s", key, encoded)
		}
	}
}

func TestResourceSamplerDeduplicatesOnlySharedPoolInstances(t *testing.T) {
	a, b, c, d := new(pgxpool.Pool), new(pgxpool.Pool), new(pgxpool.Pool), new(pgxpool.Pool)
	separate := poolRoles(a, b, c, d, nil)
	if len(separate) != 4 || separate[diagnostics.ExportPool] != c || separate[diagnostics.RecoveryPool] != d {
		t.Fatal("distinct pools omitted")
	}
	shared := poolRoles(a, b, c, c, nil)
	if len(shared) != 3 || shared[diagnostics.SharedMaintenancePool] != c {
		t.Fatal("shared maintenance double counted")
	}
	if len(poolRoles(a, b, nil, nil, nil)) != 2 {
		t.Fatal("nil pool sampled")
	}
}

// MCP guard phases are sampled from their own collector, so sync phase
// histograms stay sync-only and MCP work is never mixed into them.
func TestResourceSamplerEmitsMCPPhasesSeparately(t *testing.T) {
	var syncCollector, mcpCollector diagnostics.CapacityCollector
	ctx := diagnostics.WithRecorder(context.Background(), mcpCollector.Recorder())
	diagnostics.Record(ctx, nil, diagnostics.Authority, time.Millisecond)
	diagnostics.Flush(ctx)
	var rows []diagnostics.ResourceSample
	sampler := resourceSampler{collector: &syncCollector, mcp: &mcpCollector, read: func() diagnostics.ResourceSample { return diagnostics.ResourceSample{} }, emit: func(row diagnostics.ResourceSample) { rows = append(rows, row) }}
	run, cancel := context.WithCancel(context.Background())
	cancel()
	sampler.RunResourceSampler(run, time.Hour)
	if len(rows) == 0 || rows[0].MCPCapacity == nil || rows[0].MCPCapacity.Phases[diagnostics.Authority].Count != 1 || len(rows[0].Capacity.Phases) != 0 {
		t.Fatalf("rows=%+v", rows)
	}
	sampler.mcp = nil
	rows = nil
	sampler.RunResourceSampler(run, time.Hour)
	if len(rows) == 0 || rows[0].MCPCapacity != nil {
		t.Fatal("disabled MCP emitted an MCP snapshot")
	}
}
