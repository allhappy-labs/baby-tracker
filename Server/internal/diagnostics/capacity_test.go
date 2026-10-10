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
	c.ObserveAdmission(AdmissionClass("credential-secret"), AdmissionAccepted)
	c.ObserveAdmission(SyncAdmission, AdmissionOutcome(255))
	s := c.Snapshot()
	if len(s.Admission) != 2 || s.Admission[SyncAdmission] != (AdmissionCounts{Accepted: 1, Rejected: 1, Active: 1}) || s.Admission[MCPAdmission] != (AdmissionCounts{Accepted: 1, Released: 1}) {
		t.Fatalf("metrics=%+v", s.Admission)
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
