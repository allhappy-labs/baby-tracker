package main

import (
	"babytracker/server/internal/diagnostics"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/realtime"
	"babytracker/server/internal/wake"
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"
)

type resourceSampler struct {
	collector *diagnostics.CapacityCollector
	// mcp is the separate MCP phase collector; nil when MCP is disabled.
	mcp  *diagnostics.CapacityCollector
	read func() diagnostics.ResourceSample
	emit func(diagnostics.ResourceSample)
}

// RunResourceSampler owns system reads; requests only update bounded counters.
// Callers join this loop before closing the sampled pools.
func (s resourceSampler) RunResourceSampler(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	sample := func() {
		row := s.read()
		row.At = time.Now().UTC()
		row.Capacity = s.collector.Snapshot()
		if s.mcp != nil {
			snapshot := s.mcp.Snapshot()
			row.MCPCapacity = &snapshot
		}
		s.emit(row)
	}
	sample()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			sample()
			return
		case <-ticker.C:
			sample()
		}
	}
}

// poolRoles labels each actual pool once with a fixed role. The dedicated MCP
// pool is sampled under the fixed "mcp" role; labels never carry credentials,
// accounts or families.
func poolRoles(runtimePool, projector, export, recovery, mcp *pgxpool.Pool) map[diagnostics.PoolRole]*pgxpool.Pool {
	result := map[diagnostics.PoolRole]*pgxpool.Pool{}
	for _, pair := range []struct {
		role diagnostics.PoolRole
		pool *pgxpool.Pool
	}{{diagnostics.RuntimePool, runtimePool}, {diagnostics.ProjectorPool, projector}, {diagnostics.ExportPool, export}, {diagnostics.RecoveryPool, recovery}, {diagnostics.MCPPool, mcp}} {
		if pair.pool == nil {
			continue
		}
		role := pair.role
		if export != nil && export == recovery && (role == diagnostics.ExportPool || role == diagnostics.RecoveryPool) {
			role = diagnostics.SharedMaintenancePool
		}
		duplicate := false
		for _, pool := range result {
			if pool == pair.pool {
				duplicate = true
			}
		}
		if !duplicate {
			result[role] = pair.pool
		}
	}
	return result
}

// resourceSources are the in-process counters a sample reads, each nil when
// not composed (its field is then omitted). Every one returns counts under
// fixed names only.
type resourceSources struct {
	Limiter        func() limits.Stats
	Live           func() realtime.HubStats
	TrackerLen     func() int
	TrackerDropped func() uint64
	Wake           func() wake.Totals
}

func readResources(pools map[diagnostics.PoolRole]*pgxpool.Pool, sources resourceSources) diagnostics.ResourceSample {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	s := diagnostics.ResourceSample{AllocTotal: diagnostics.Counter{Available: true, Value: mem.TotalAlloc}, Goroutines: runtime.NumGoroutine(), Pools: map[diagnostics.PoolRole]diagnostics.PoolSample{}}
	s.Runtime = &diagnostics.RuntimeSample{HeapInuse: mem.HeapInuse, HeapObjects: mem.HeapObjects, NumGC: mem.NumGC, PauseTotalNS: mem.PauseTotalNs,
		GOMEMLIMIT: debug.SetMemoryLimit(-1), GOMAXPROCS: runtime.GOMAXPROCS(0)}
	if sources.Limiter != nil {
		stats := sources.Limiter()
		s.Limiter = &diagnostics.LimiterSample{VerifiedFull: stats.VerifiedFull, IPEvictions: stats.IPEvictions}
	}
	if sources.Live != nil {
		stats := sources.Live()
		s.Live = &diagnostics.LiveSample{Leases: stats.Leases, Reservations: stats.Reservations,
			SweepDurationNS: stats.SweepDuration, SweepClosed: stats.SweepClosed}
	}
	if sources.Wake != nil {
		totals := sources.Wake()
		s.Wake = &diagnostics.WakeSample{Ticks: totals.Ticks, Flushed: totals.Flushed, Woken: totals.Woken, Pushed: totals.Pushed,
			PushFailed: totals.PushFailed, NotSent: totals.NotSent, Ambiguous: totals.Ambiguous, SettlesPending: totals.SettlesPending,
			SettlesFailed: totals.SettlesFailed, Families: totals.Families, FamiliesPeak: totals.FamiliesPeak, TickDurationNS: totals.TickDurationNS}
		if sources.TrackerLen != nil {
			s.Wake.TrackerEntries = sources.TrackerLen()
		}
		if sources.TrackerDropped != nil {
			s.Wake.TrackerDropped = sources.TrackerDropped()
		}
	}
	var usage syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) == nil {
		rss := uint64(usage.Maxrss)
		if runtime.GOOS != "darwin" {
			rss *= 1024
		}
		s.RSSPeak = diagnostics.Counter{Available: true, Value: rss}
		s.CPUTotal = diagnostics.Counter{Available: true, Value: uint64(usage.Utime.Sec+usage.Stime.Sec)*1e9 + uint64(usage.Utime.Usec+usage.Stime.Usec)*1000}
	}
	for role, pool := range pools {
		p := pool.Stat()
		s.Pools[role] = diagnostics.PoolSample{AcquireCount: uint64(p.AcquireCount()), AcquireNS: uint64(p.AcquireDuration()), WaitNS: uint64(p.EmptyAcquireWaitTime()), EmptyAcquireCount: uint64(p.EmptyAcquireCount()), CanceledAcquireCount: uint64(p.CanceledAcquireCount()), TotalConnections: p.TotalConns(), AcquiredConnections: p.AcquiredConns(), MaxConnections: p.MaxConns()}
	}
	return s
}
