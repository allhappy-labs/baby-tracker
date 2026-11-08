package loadtest

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/protocol"
)

// I6: the hot family writes at 80% of its writers' sync budgets (the
// tighter of 120/min per device and 240/min per account, per account,
// summed), spread so that no device or account exceeds its own limit.
func TestHotFamilyStaysWithinTheSyncLimits(t *testing.T) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 4)
	h := planHotFamily(g)
	// The family has an owner with two devices (240/min, the account limit
	// binds) and two caregivers with one device each (120/min each): 480/min.
	if want := 0.8 * 480 / 60; math.Abs(h.rate-want) > 1e-9 || len(h.accounts) != 3 {
		t.Fatalf("rate %v (want %v), accounts %d, binding %v", h.rate, want, len(h.accounts), h.binding)
	}
	const picks = 200000
	perDevice, perAccount := map[string]int{}, map[string]int{}
	for range picks {
		d := h.pick(g.rng)
		if d.FamilyID != h.family || d.Role == "viewer" {
			t.Fatalf("picked %s (%s) outside the hot family's writers", d.DeviceID, d.Role)
		}
		perDevice[d.DeviceID]++
		perAccount[d.AccountID]++
	}
	perMinute := func(n int) float64 { return float64(n) / picks * h.rate * 60 }
	for id, n := range perDevice {
		if got := perMinute(n); got > 120*1.02 {
			t.Errorf("device %s writes %.0f/min, over 120", id, got)
		}
	}
	for id, n := range perAccount {
		if got := perMinute(n); got > 240*1.02 {
			t.Errorf("account %s writes %.0f/min, over 240", id, got)
		}
	}

	// A short run reports the cell: its target, offered and achieved rates
	// and limits. R2: the cell's budget is the family's alone. Its devices
	// hold no live socket (whose hint pulls would draw on the same budgets)
	// and take no background pull, snapshot or write: during the cell their
	// only sync traffic is the cell's writes. Every other device is live.
	cfg := runConfig(t, f, 4, Model{RefreshPerSecond: 0.5, PullPerSecond: 10, WritePerSecond: 2, SnapshotPerSecond: 1, LiveSockets: 12})
	cfg.Scenario, cfg.Warmup, cfg.Measure = "hot-family", 200*time.Millisecond, 3*time.Second
	if _, err := Run(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	r := readResult(t, cfg.Out)
	cell, ok := r.Cells["hot_family"].(map[string]any)
	if !ok || cell["target_requests_per_second"] != h.rate || cell["binding_limits"] == nil || cell["excluded"] == nil {
		t.Fatalf("hot_family cell %v", r.Cells)
	}
	achieved, offered := cell["achieved_requests_per_second"].(float64), cell["offered_requests_per_second"].(float64)
	if achieved <= 0 || achieved > offered {
		t.Fatalf("achieved %v of offered %v", achieved, offered)
	}
	if got := r.Routes[RouteSyncWrite].Outcomes; got[OutcomeRateLimited] != 0 {
		t.Fatalf("hot family writes were rate limited: %v", got)
	}
	hot := cell["family_id"].(string)
	familyOf := func(req recorded) string {
		v, _ := bearerFamilies.Load(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
		s, _ := v.(string)
		return s
	}
	hotWrites, otherSockets := 0, 0
	for _, req := range f.all("/v1/live") {
		if familyOf(req) == hot {
			t.Fatal("a hot-family device dialled a live socket")
		}
		otherSockets++
	}
	if r.LiveDevices != 12 || otherSockets < 12 {
		t.Fatalf("live devices %d, dials %d: every other device should be live", r.LiveDevices, otherSockets)
	}
	inCell := func(req recorded) bool { return !req.At.Before(r.WindowStart) && req.At.Before(r.MeasureEnd) }
	for _, req := range f.all("/v1/sync/snapshot") {
		if inCell(req) && familyOf(req) == hot {
			t.Fatal("a hot-family device took a background snapshot")
		}
	}
	for _, req := range f.all("/v1/sync") {
		if !inCell(req) || familyOf(req) != hot {
			continue
		}
		var body protocol.SyncRequest
		if err := json.Unmarshal(req.Body, &body); err != nil || len(body.Mutations) != 1 {
			t.Fatalf("a hot-family device sent a sync other than the cell's one-mutation write (%d mutations)", len(body.Mutations))
		}
		hotWrites++
	}
	if hotWrites == 0 {
		t.Fatal("the cell wrote nothing")
	}
}

// m2: an import's lost race is retried per Retry-After, as every route's.
func TestImportRetriesALostRace(t *testing.T) {
	f := newFakeV1(t)
	var raced atomic.Bool
	f.hook("POST /v1/family/import/resolve", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		if !raced.Swap(true) {
			w.Header().Set("Retry-After", "1")
			writeErrorResponse(w, http.StatusServiceUnavailable, "security_busy")
			return true
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{"import_id": newUUID(), "resolver_revision": 1, "mappings": []any{},
			"conflicts": []any{}, "mapping_complete": true, "manifest_complete": true})
		return true
	})
	g := testGenerator(t, f, 1)
	if !g.importRecords(t.Context(), g.devices[0], time.Now(), &importStats{}) {
		t.Fatalf("a lost race ended the import: %v", g.outcomes(RouteImportResolve))
	}
	if got := g.outcomes(RouteImportResolve); got[OutcomeSecurityBusy] != 1 || got[OutcomeOK] != 2 {
		t.Fatalf("resolve outcomes %v", got)
	}
}

// I6: an import waits out a 429 per Retry-After and resumes the same
// batch; the pause is an expected outcome and is reported.
func TestImportWaitsOutA429AndResumes(t *testing.T) {
	f := newFakeV1(t)
	var writes atomic.Int32
	f.hook("POST /v1/sync", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		if writes.Add(1) == 13 {
			w.Header().Set("Retry-After", "1")
			writeErrorResponse(w, http.StatusTooManyRequests, "rate_limited")
			return true
		}
		return false
	})
	f.hook("POST /v1/family/import/resolve", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		writeJSONResponse(w, http.StatusOK, map[string]any{"import_id": newUUID(), "resolver_revision": 1, "mappings": []any{},
			"conflicts": []any{}, "mapping_complete": true, "manifest_complete": true})
		return true
	})
	g := testGenerator(t, f, 1)
	stats := &importStats{}
	started := time.Now()
	if !g.importRecords(t.Context(), g.devices[0], time.Now(), stats) {
		t.Fatalf("the import did not complete: %v", g.outcomes(RouteImportWrite))
	}
	if time.Since(started) < time.Second || stats.pauses.Load() != 1 || stats.pausedNanos.Load() != int64(time.Second) {
		t.Fatalf("pauses %d (%v) after %v", stats.pauses.Load(), time.Duration(stats.pausedNanos.Load()), time.Since(started))
	}
	if got := g.outcomes(RouteImportWrite); got[OutcomeImportPaused] != 1 || got[OutcomeOK] != importRecords/importBatch {
		t.Fatalf("import writes %v", got)
	}
	if g.ledgered.Load() != importRecords {
		t.Fatalf("ledgered %d", g.ledgered.Load())
	}
	if v := EvaluateWindow(RunSummary{Routes: g.phases.measure.Snapshot()}, SpecThresholds()); unexpectedReason(v) {
		t.Fatalf("the pause counted as unexpected: %v", v.Reasons)
	}
}
