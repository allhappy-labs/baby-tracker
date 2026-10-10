package diagnostics

import (
	"context"
	"sync"
	"time"
)

// Fixed domains contain no account, device, family, credential or request labels.
type PoolRole string

const (
	RuntimePool           PoolRole = "runtime"
	ProjectorPool         PoolRole = "projector"
	ExportPool            PoolRole = "export"
	RecoveryPool          PoolRole = "recovery"
	SharedMaintenancePool PoolRole = "shared_maintenance"
	MCPPool               PoolRole = "mcp"
)

type AdmissionClass string

const (
	SyncAdmission AdmissionClass = "sync"
	MCPAdmission  AdmissionClass = "mcp"
)

var phases = [...]Phase{Authority, Database, Observer, SecurityFence, FamilyFence, TransactionAcquire, IntegrityInventory, IntegrityExtension, IntegrityVerify, CacheWait}
var boundaries = [...]time.Duration{100 * time.Microsecond, 500 * time.Microsecond, time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 500 * time.Millisecond, time.Second, 5 * time.Second, 30 * time.Second}

// Buckets are disjoint; the final bucket includes all values above 30 seconds.
type Histogram struct {
	Count   uint64     `json:"count"`
	TotalNS uint64     `json:"total_ns"`
	Buckets [12]uint64 `json:"buckets"`
}
type AdmissionOutcome uint8

const (
	AdmissionAccepted AdmissionOutcome = iota
	AdmissionRejected
	AdmissionReleased
)

type AdmissionCounts struct {
	Accepted uint64 `json:"accepted"`
	Rejected uint64 `json:"rejected"`
	Released uint64 `json:"released"`
	Active   uint64 `json:"active"`
}

type CapacitySnapshot struct {
	Admission      map[AdmissionClass]AdmissionCounts `json:"admission"`
	Phases         map[Phase]Histogram                `json:"phases"`
	IntegrityBytes uint64                             `json:"integrity_bytes"`
	BoundariesNS   [11]time.Duration                  `json:"boundaries_ns"`
}

// CapacityCollector retains only fixed arrays. Request observations allocate no
// samples, read no system state and invoke no external callbacks.
type CapacityCollector struct {
	admission      [2]AdmissionCounts
	mu             sync.Mutex
	values         [10]Histogram
	integrityBytes uint64
}

func (c *CapacityCollector) Observe(phase Phase, elapsed time.Duration) {
	index := -1
	for i, p := range phases {
		if p == phase {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed > time.Minute {
		elapsed = time.Minute
	}
	bucket := len(boundaries)
	for i, b := range boundaries {
		if elapsed <= b {
			bucket = i
			break
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	h := &c.values[index]
	h.Count++
	h.TotalNS += uint64(elapsed)
	h.Buckets[bucket]++
}
func (c *CapacityCollector) AddIntegrityBytes(n uint64) {
	c.mu.Lock()
	c.integrityBytes += n
	c.mu.Unlock()
}

// ObserveAdmission accepts only fixed class/outcome domains and keeps no samples.
func (c *CapacityCollector) ObserveAdmission(class AdmissionClass, outcome AdmissionOutcome) {
	var index int
	switch class {
	case SyncAdmission:
		index = 0
	case MCPAdmission:
		index = 1
	default:
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	count := &c.admission[index]
	switch outcome {
	case AdmissionAccepted:
		count.Accepted++
		count.Active++
	case AdmissionRejected:
		count.Rejected++
	case AdmissionReleased:
		if count.Active > 0 {
			count.Active--
			count.Released++
		}
	}
}

func (c *CapacityCollector) Snapshot() CapacitySnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := CapacitySnapshot{Admission: map[AdmissionClass]AdmissionCounts{SyncAdmission: c.admission[0], MCPAdmission: c.admission[1]}, Phases: make(map[Phase]Histogram), IntegrityBytes: c.integrityBytes, BoundariesNS: boundaries}
	for i, p := range phases {
		if c.values[i].Count > 0 {
			s.Phases[p] = c.values[i]
		}
	}
	return s
}

// The private byte-summary signal is emitted once by Flush, after outer leases
// return, even when no timing phases were captured. It is not a duration sample.
func (c *CapacityCollector) Recorder() Recorder {
	return func(ctx context.Context, p Phase, d time.Duration) {
		if p == integrityBytesSummary {
			n, _ := ctx.Value(integrityBytesKey{}).(uint64)
			c.AddIntegrityBytes(n)
			return
		}
		c.Observe(p, d)
	}
}

type Counter struct {
	Available bool   `json:"available"`
	Value     uint64 `json:"value"`
}
type Delta struct {
	Status string `json:"status"`
	Value  uint64 `json:"value"`
}

func CounterDelta(before, after Counter) Delta {
	if !before.Available || !after.Available {
		return Delta{Status: "unavailable"}
	}
	if after.Value < before.Value {
		return Delta{Status: "reset"}
	}
	return Delta{Status: "available", Value: after.Value - before.Value}
}

type PoolSample struct {
	AcquireCount         uint64 `json:"acquire_count"`
	AcquireNS            uint64 `json:"acquire_ns"`
	WaitNS               uint64 `json:"wait_ns"`
	EmptyAcquireCount    uint64 `json:"empty_acquire_count"`
	CanceledAcquireCount uint64 `json:"canceled_acquire_count"`
	TotalConnections     int32  `json:"total_connections"`
	AcquiredConnections  int32  `json:"acquired_connections"`
	MaxConnections       int32  `json:"max_connections"`
}
type ResourceSample struct {
	At         time.Time               `json:"at"`
	AllocTotal Counter                 `json:"alloc_total_bytes"`
	CPUTotal   Counter                 `json:"cpu_total_ns"`
	RSSPeak    Counter                 `json:"rss_peak_bytes"`
	Goroutines int                     `json:"goroutines"`
	Pools      map[PoolRole]PoolSample `json:"pools"`
	Capacity   CapacitySnapshot        `json:"capacity"`
	// MCPCapacity holds the private MCP guard's phase timings and integrity
	// bytes from its own collector (nil when MCP or diagnostics are off).
	// Capacity.Phases and IntegrityBytes therefore stay sync-only; MCP
	// admission counts remain in Capacity.Admission under the "mcp" class.
	MCPCapacity *CapacitySnapshot `json:"mcp_capacity,omitempty"`
}
