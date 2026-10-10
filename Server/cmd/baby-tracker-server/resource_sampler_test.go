package main

import (
	"babytracker/server/internal/diagnostics"
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
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
