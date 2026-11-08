package diagnostics

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Dynamic labels or retained samples would leak identifiers and grow without bound.
func TestCapacityBoundedMillionObservations(t *testing.T) {
	var c CapacityCollector
	for i := 0; i < 1_000_000; i++ {
		c.Observe(CacheWait, 500*time.Microsecond)
		c.Observe(Phase("family-secret"), time.Second)
	}
	s := c.Snapshot()
	if s.Phases[CacheWait].Count != 1_000_000 || s.Phases[CacheWait].Buckets[1] != 1_000_000 {
		t.Fatalf("wrong histogram: %+v", s)
	}
	raw, _ := json.Marshal(s)
	if len(raw) > 20000 || strings.Contains(string(raw), "family-secret") {
		t.Fatal("unbounded labels or samples")
	}
	if n := testing.AllocsPerRun(1000, func() { c.Observe(IntegrityVerify, time.Millisecond) }); n != 0 {
		t.Fatalf("allocations per observation: %v", n)
	}
	c.Observe(IntegrityVerify, time.Minute)
	s = c.Snapshot()
	if s.Phases[IntegrityVerify].Count == 0 || s.Phases[CacheWait].Count != 1_000_000 {
		t.Fatal("cache and integrity conflated")
	}
}

func TestCapacityBytesDeferredAndTransferredOnceWithoutTiming(t *testing.T) {
	var c CapacityCollector
	ctx := WithRecorder(context.Background(), c.Recorder())
	CaptureIntegrityBytes(ctx, 25)
	CaptureIntegrityBytes(ctx, 17)
	if c.Snapshot().IntegrityBytes != 0 {
		t.Fatal("bytes published before flush")
	}
	Flush(ctx)
	Flush(ctx)
	CaptureIntegrityBytes(ctx, 100)
	Flush(ctx)
	if c.Snapshot().IntegrityBytes != 42 {
		t.Fatalf("bytes=%d", c.Snapshot().IntegrityBytes)
	}
	if len(c.Snapshot().Phases) != 0 {
		t.Fatal("byte summary invented timing observations")
	}
}

func TestCounterDeltaUnknownResetAndZero(t *testing.T) {
	for _, tt := range []struct {
		before, after Counter
		status        string
		value         uint64
	}{
		{Counter{}, Counter{Available: true, Value: 0}, "unavailable", 0},
		{Counter{Available: true, Value: 9}, Counter{Available: true, Value: 2}, "reset", 0},
		{Counter{Available: true, Value: 2}, Counter{Available: true, Value: 2}, "available", 0},
		{Counter{Available: true, Value: 2}, Counter{Available: true, Value: 9}, "available", 7},
	} {
		got := CounterDelta(tt.before, tt.after)
		if got.Status != tt.status || got.Value != tt.value {
			t.Fatalf("delta=%+v", got)
		}
	}
}

func TestAdmissionMetricsFixedDomainsAndOccupancy(t *testing.T) {
	var c CapacityCollector
	c.ObserveAdmission(SyncAdmission, AdmissionAccepted)
	c.ObserveAdmission(SyncAdmission, AdmissionRejected)
	c.ObserveAdmission(MCPAdmission, AdmissionAccepted)
	c.ObserveAdmission(MCPAdmission, AdmissionReleased)
	c.ObserveAdmission(AuthAdmission, AdmissionAccepted)
	c.ObserveAdmission(AuthAdmission, AdmissionRejected)
	c.ObserveAdmission(AuthAdmission, AdmissionRejected)
	c.ObserveAdmission(AdmissionClass("credential-secret"), AdmissionAccepted)
	c.ObserveAdmission(SyncAdmission, AdmissionOutcome(255))
	s := c.Snapshot()
	if len(s.Admission) != 3 || s.Admission[SyncAdmission] != (AdmissionCounts{Accepted: 1, Rejected: 1, Active: 1}) || s.Admission[MCPAdmission] != (AdmissionCounts{Accepted: 1, Released: 1}) ||
		s.Admission[AuthAdmission] != (AdmissionCounts{Accepted: 1, Rejected: 2, Active: 1}) {
		t.Fatalf("metrics=%+v", s.Admission)
	}
	if AuthAdmission != "auth" {
		t.Fatalf("auth class %q", AuthAdmission)
	}
	raw, _ := json.Marshal(s)
	if strings.Contains(string(raw), "secret") {
		t.Fatal("dynamic label leaked")
	}
	if n := testing.AllocsPerRun(1000, func() {
		c.ObserveAdmission(SyncAdmission, AdmissionAccepted)
		c.ObserveAdmission(SyncAdmission, AdmissionReleased)
	}); n != 0 {
		t.Fatalf("allocations=%v", n)
	}
}

// A request that found no slot and waited (BT_ADMISSION_WAIT) is counted
// with its wait, and as canceled when its request ended while it waited
// (neither admitted nor refused); an admission or refusal after a wait is
// counted as any other. Fixed domains, no
// allocations.
func TestAdmissionWaitMetrics(t *testing.T) {
	var c CapacityCollector
	c.ObserveAdmissionWait(SyncAdmission, 30*time.Millisecond, false)
	c.ObserveAdmission(SyncAdmission, AdmissionAccepted)
	c.ObserveAdmissionWait(SyncAdmission, 250*time.Millisecond, false)
	c.ObserveAdmission(SyncAdmission, AdmissionRejected)
	c.ObserveAdmissionWait(SyncAdmission, -time.Millisecond, true)
	c.ObserveAdmissionWait(MCPAdmission, time.Millisecond, false)
	c.ObserveAdmissionWait(AuthAdmission, 2*time.Millisecond, false)
	c.ObserveAdmissionWait(AdmissionClass("credential-secret"), time.Second, true)
	s := c.Snapshot()
	if got := s.Admission[SyncAdmission]; got != (AdmissionCounts{Accepted: 1, Rejected: 1, Active: 1, Waited: 3, Canceled: 1,
		WaitNS: uint64(280 * time.Millisecond)}) {
		t.Fatalf("sync %+v", got)
	}
	if got := s.Admission[MCPAdmission]; got != (AdmissionCounts{Waited: 1, WaitNS: uint64(time.Millisecond)}) {
		t.Fatalf("mcp %+v", got)
	}
	if got := s.Admission[AuthAdmission]; got != (AdmissionCounts{Waited: 1, WaitNS: uint64(2 * time.Millisecond)}) {
		t.Fatalf("auth %+v", got)
	}
	raw, _ := json.Marshal(s.Admission[SyncAdmission])
	if string(raw) != `{"accepted":1,"rejected":1,"released":0,"active":1,"waited":3,"wait_ns":280000000,"canceled":1}` {
		t.Fatalf("json %s", raw)
	}
	if n := testing.AllocsPerRun(1000, func() { c.ObserveAdmissionWait(SyncAdmission, time.Millisecond, false) }); n != 0 {
		t.Fatalf("allocations=%v", n)
	}
}

// A standalone Histogram (the live sample's sweep durations) buckets by the
// same boundaries as the capacity phases, clamps to [0, 1 minute] and
// allocates nothing.
func TestHistogramObserveBucketsAndClamps(t *testing.T) {
	var h Histogram
	h.Observe(-time.Second)     // 0: bucket 0
	h.Observe(time.Second)      // bucket 8 (<= 1s)
	h.Observe(time.Second + 1)  // bucket 9 (<= 5s)
	h.Observe(31 * time.Second) // bucket 11 (above 30s)
	h.Observe(2 * time.Hour)    // clamped to a minute, bucket 11
	want := [12]uint64{0: 1, 8: 1, 9: 1, 11: 2}
	if h.Count != 5 || h.Buckets != want || h.TotalNS != uint64(time.Second+time.Second+1+31*time.Second+time.Minute) {
		t.Fatalf("histogram %+v", h)
	}
	if n := testing.AllocsPerRun(1000, func() { h.Observe(time.Millisecond) }); n != 0 {
		t.Fatalf("allocations per observation: %v", n)
	}
}
