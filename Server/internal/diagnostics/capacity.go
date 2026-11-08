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
	// AuthAdmission is the auth lane: the requests not yet tied to a known
	// caller (admission.Auth).
	AuthAdmission AdmissionClass = "auth"
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

// AdmissionCounts are a class's admission outcomes since start. Rejected
// counts the 503 server_busy refusals: no slot (and, with a wait, none
// freed within it, or the queue full). Waited counts the requests that
// found no slot and waited for one (BT_ADMISSION_WAIT), WaitNS their total
// wait. A waiter ends Accepted, Rejected, or Canceled: its request ended
// while it waited (its client gone, answered service_unavailable as
// client_gone), which is neither a refusal nor an admission.
type AdmissionCounts struct {
	Accepted uint64 `json:"accepted"`
	Rejected uint64 `json:"rejected"`
	Released uint64 `json:"released"`
	Active   uint64 `json:"active"`
	Waited   uint64 `json:"waited"`
	WaitNS   uint64 `json:"wait_ns"`
	Canceled uint64 `json:"canceled"`
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
	admission      [3]AdmissionCounts
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
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values[index].Observe(elapsed)
}

// Observe adds one duration: negative counts as 0 and anything above a
// minute as a minute. The bucket bounds are the sample's boundaries_ns. The
// caller serialises access.
func (h *Histogram) Observe(elapsed time.Duration) {
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
	index, ok := admissionIndex(class)
	if !ok {
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

// ObserveAdmissionWait records one request of class that found no slot and
// waited (a negative wait counts as 0), canceled when its request ended
// while it waited. Its admission or refusal is ObserveAdmission's.
func (c *CapacityCollector) ObserveAdmissionWait(class AdmissionClass, waited time.Duration, canceled bool) {
	index, ok := admissionIndex(class)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	count := &c.admission[index]
	count.Waited++
	count.WaitNS += uint64(max(waited, 0))
	if canceled {
		count.Canceled++
	}
}

// admissionIndex is class's place in CapacityCollector.admission, and
// whether class is one of the fixed domains.
func admissionIndex(class AdmissionClass) (int, bool) {
	switch class {
	case SyncAdmission:
		return 0, true
	case MCPAdmission:
		return 1, true
	case AuthAdmission:
		return 2, true
	}
	return 0, false
}

func (c *CapacityCollector) Snapshot() CapacitySnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := CapacitySnapshot{Admission: map[AdmissionClass]AdmissionCounts{SyncAdmission: c.admission[0], MCPAdmission: c.admission[1], AuthAdmission: c.admission[2]}, Phases: make(map[Phase]Histogram), IntegrityBytes: c.integrityBytes, BoundariesNS: boundaries}
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
	// The process's own state, each nil when its source is not composed.
	// Every field is a count or a size under a fixed name; the limiter's
	// map is keyed by its fixed IP scope names, never by a key.
	Runtime *RuntimeSample `json:"runtime,omitempty"`
	Limiter *LimiterSample `json:"limiter,omitempty"`
	Live    *LiveSample    `json:"live,omitempty"`
	Wake    *WakeSample    `json:"wake,omitempty"`
}

// RuntimeSample is the Go runtime's heap and collector state and its limits.
type RuntimeSample struct {
	HeapInuse    uint64 `json:"heap_inuse_bytes"`
	HeapObjects  uint64 `json:"heap_objects"`
	NumGC        uint32 `json:"num_gc"`
	PauseTotalNS uint64 `json:"gc_pause_total_ns"`
	GOMEMLIMIT   int64  `json:"gomemlimit"`
	GOMAXPROCS   int    `json:"gomaxprocs"`
}

// LimiterSample is the in-memory rate limiter's capacity events: takes
// refused because the verified-key pool was full, and live buckets evicted
// per IP scope (fixed scope names).
type LimiterSample struct {
	VerifiedFull uint64            `json:"verified_full"`
	IPEvictions  map[string]uint64 `json:"ip_evictions"`
}

// LiveSample is the live hub's occupancy and its authorization re-checks
// since start: SweepDurationNS times each sweep's CheckMany (buckets as
// boundaries_ns), and SweepClosed counts the sockets closed because a sweep
// failed, answered late or answered wrongly ("authorization unavailable"),
// not those it found denied.
type LiveSample struct {
	Leases          int       `json:"leases"`
	Reservations    int       `json:"reservations"`
	SweepDurationNS Histogram `json:"sweep_duration_ns"`
	SweepClosed     uint64    `json:"sweep_closed"`
}

// WakeSample is the wake tracker's held entries and drops, and the wake
// worker's totals since start (SettlesPending: the settle rows kept now;
// TickDurationNS: the ticks that had work, in the capacity buckets; Families
// the families they drained, FamiliesPeak the most in one tick).
type WakeSample struct {
	TrackerEntries int       `json:"tracker_entries"`
	TrackerDropped uint64    `json:"tracker_dropped"`
	Ticks          uint64    `json:"ticks"`
	Flushed        uint64    `json:"flushed"`
	Woken          uint64    `json:"woken"`
	Pushed         uint64    `json:"pushed"`
	PushFailed     uint64    `json:"push_failed"`
	NotSent        uint64    `json:"not_sent"`
	Ambiguous      uint64    `json:"ambiguous"`
	SettlesPending uint64    `json:"settles_pending"`
	SettlesFailed  uint64    `json:"settles_failed"`
	Families       uint64    `json:"families"`
	FamiliesPeak   uint64    `json:"families_peak"`
	TickDurationNS Histogram `json:"tick_duration_ns"`
}
