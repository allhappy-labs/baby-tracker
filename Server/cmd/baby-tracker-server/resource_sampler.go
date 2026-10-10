package main

import (
	"babytracker/server/internal/diagnostics"
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"runtime"
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
func readResources(pools map[diagnostics.PoolRole]*pgxpool.Pool) diagnostics.ResourceSample {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	s := diagnostics.ResourceSample{AllocTotal: diagnostics.Counter{Available: true, Value: mem.TotalAlloc}, Goroutines: runtime.NumGoroutine(), Pools: map[diagnostics.PoolRole]diagnostics.PoolSample{}}
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
