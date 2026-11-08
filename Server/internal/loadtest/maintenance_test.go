package loadtest

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/protocol"
)

// fakeMaintenanceDB is the stack's superuser side of the hot purge: once
// the recovery window is advanced, the purge worker starts and completes
// on the next two status reads.
type fakeMaintenanceDB struct {
	mu     sync.Mutex
	due    []string
	reads  int
	status string
}

func (f *fakeMaintenanceDB) purgeDueNow(_ context.Context, familyID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.due = append(f.due, familyID)
	f.status = "deletion_pending"
	return nil
}

func (f *fakeMaintenanceDB) familyStatus(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	switch {
	case f.status == "deletion_pending":
		f.status = "purge_started"
	case f.status == "purge_started":
		f.status = "purge_completed"
	}
	return f.status, nil
}

func (f *fakeMaintenanceDB) careRows(context.Context, string) (int64, int64, error) {
	return 1100, 400, nil
}

// Task 16 Step 4: --maintenance hot runs, inside a soak, one export (an
// owner of another family, downloaded whole) and one family purge: the
// family with the most history (no MCP grant) has no part in the
// background, its owner schedules the deletion through the API, the
// recovery window is advanced and the purge is followed to its end. The
// cell records each step; the fleet drops the family.
func TestHotMaintenancePurgesOneFamilyAndExportsAnother(t *testing.T) {
	f := newFakeV1(t)
	exported := strings.Repeat("x", 300<<10)
	f.hook("POST /v1/export", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		writeJSONResponse(w, http.StatusOK, protocol.ExportStatus{ProtocolVersion: "v1", ExportID: "e1", State: "ready"})
		return true
	})
	f.hook("GET /v1/export/", func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if strings.HasSuffix(r.URL.Path, "/download") {
			_, _ = w.Write([]byte(exported))
			return true
		}
		writeJSONResponse(w, http.StatusOK, protocol.ExportStatus{ProtocolVersion: "v1", ExportID: "e1", State: "ready"})
		return true
	})
	f.hook("POST /v1/family/delete", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		writeJSONResponse(w, http.StatusOK, map[string]any{"protocol_version": "v1", "state": "deletion_pending"})
		return true
	})
	cfg := runConfig(t, f, 4, Model{RefreshPerSecond: 0.4, PullPerSecond: 10, WritePerSecond: 4, SnapshotPerSecond: 0.5, PushTokenPerSecond: 1,
		LiveActivityPerSecond: 1, LiveSockets: 6})
	// Family 4 (odd: no MCP credential) has the most history.
	header, states, err := ReadFleet(cfg.Fleet)
	if err != nil {
		t.Fatal(err)
	}
	purged := states[12].FamilyID
	for i := range states {
		states[i].Cursor = uint64(10 + i%4)
		if states[i].FamilyID == purged {
			states[i].Cursor = 500
		}
	}
	if err := WriteFleet(cfg.Fleet, header, states); err != nil {
		t.Fatal(err)
	}
	db := &fakeMaintenanceDB{}
	cfg.Maintenance, cfg.maintenanceDB = MaintenanceHot, db
	cfg.Warmup, cfg.Measure, cfg.MaintenanceAt = 300*time.Millisecond, 5*time.Second, time.Second
	if _, err := Run(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	r := readResult(t, cfg.Out)
	cell, ok := r.Cells["maintenance"].(map[string]any)
	if !ok {
		t.Fatalf("cells %v", r.Cells)
	}
	purge, _ := cell["purge"].(map[string]any)
	export, _ := cell["export"].(map[string]any)
	if purge["family_id"] != purged || purge["delete_outcome"] != OutcomeOK || purge["status"] != "purge_completed" || purge["due_at"] == nil ||
		purge["completed_seen_at"] == nil || purge["care_entities"] != float64(1100) {
		t.Fatalf("purge %v", purge)
	}
	if export["ok"] != true || export["bytes"] != float64(len(exported)) || export["family_id"] == purged {
		t.Fatalf("export %v", export)
	}
	if len(db.due) != 1 || db.due[0] != purged {
		t.Fatalf("recovery window advanced for %v", db.due)
	}
	// The purge family sent nothing but its deletion.
	f.mu.Lock()
	requests := append([]recorded(nil), f.requests...)
	f.mu.Unlock()
	for _, req := range requests {
		v, _ := bearerFamilies.Load(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
		if v == purged && req.Path != "/v1/family/delete" {
			t.Errorf("the purge family sent %s %s", req.Method, req.Path)
		}
	}
	if f.count("POST /v1/family/delete") != 1 || r.Routes[RouteFamilyDelete].Outcomes[OutcomeOK] != 1 {
		t.Fatalf("deletes %d, route %v", f.count("POST /v1/family/delete"), r.Routes[RouteFamilyDelete])
	}
	if r.Devices != 12 || r.LiveDevices != 6 {
		t.Fatalf("devices %d live %d: the purge family is not the background's", r.Devices, r.LiveDevices)
	}
	// The fleet no longer holds the purged family.
	after, devices, err := ReadFleet(cfg.Fleet)
	if err != nil {
		t.Fatal(err)
	}
	if after.Families != 3 || len(devices) != 12 {
		t.Fatalf("fleet after the purge: %d families, %d devices", after.Families, len(devices))
	}
	for _, d := range devices {
		if d.FamilyID == purged {
			t.Fatal("the fleet still holds the purged family")
		}
	}
	if !r.GeneratorVerdict.Pass {
		t.Fatalf("generator verdict %v", r.GeneratorVerdict.Reasons)
	}
}

// --maintenance hot runs only in a soak and needs the stack's superuser.
func TestHotMaintenanceIsRefusedOutsideASoakOrWithoutTheSuperuser(t *testing.T) {
	base := RunConfig{Base: "b", Host: "h", Fleet: "f", Ledger: "l", Tracked: "t", Out: "o", Load: 1, Measure: time.Minute, Scenario: "soak",
		Maintenance: MaintenanceHot}
	base.defaults()
	if err := base.validate(); err == nil || !strings.Contains(err.Error(), "superuser") {
		t.Fatalf("without a superuser: %v", err)
	}
	withDB := base
	withDB.maintenanceDB = &fakeMaintenanceDB{}
	if err := withDB.validate(); err != nil {
		t.Fatalf("a soak with the database: %v", err)
	}
	for _, change := range []func(*RunConfig){
		func(c *RunConfig) { c.Scenario, c.StormAt = "storm", time.Second },
		func(c *RunConfig) { c.ModelOverride = ModelLiveOnly },
		func(c *RunConfig) { c.MaintenanceAt = 2 * time.Minute },
		func(c *RunConfig) { c.Maintenance = "cold" },
	} {
		c := withDB
		change(&c)
		if err := c.validate(); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}
