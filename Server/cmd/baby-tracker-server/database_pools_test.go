package main

import (
	"context"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/config"
	"babytracker/server/internal/database"
	familyexport "babytracker/server/internal/export"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCompositionCountsActualPoolIdentity(t *testing.T) {
	open := func(max int32) *pgxpool.Pool {
		p, err := database.OpenWithMax(t.Context(), "postgres://user@127.0.0.1:1/test", max)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		return p
	}
	readinessPool, err := database.OpenReadiness(t.Context(), "postgres://user@127.0.0.1:1/test?pool_max_conns=16")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(readinessPool.Close)
	background, err := database.OpenBackground(t.Context(), "postgres://user@127.0.0.1:1/test?pool_max_conns=16")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(background.Close)
	runtime, maintenance, other := open(16), open(2), open(2)
	// The readiness and background pools count, one connection each:
	// runtime 16 + readiness 1 + background 1 + maintenance 2.
	shared := actualPoolBudgets(runtime, readinessPool, background, maintenance, maintenance)
	if len(shared) != 4 || shared[1] != (database.NamedPoolBudget{Identity: "readiness", Max: 1}) ||
		shared[2] != (database.NamedPoolBudget{Identity: "background", Max: 1}) ||
		database.ValidateBudget(shared, 22, 2) != nil || database.ValidateBudget(shared, 21, 2) == nil {
		t.Fatalf("shared counted twice or side pools not counted: %+v", shared)
	}
	distinct := actualPoolBudgets(runtime, readinessPool, background, maintenance, other)
	if len(distinct) != 5 || database.ValidateBudget(distinct, 22, 2) == nil || database.ValidateBudget(distinct, 24, 2) != nil {
		t.Fatalf("distinct maintenance merged: %+v", distinct)
	}
	if got := actualPoolBudgets(runtime, nil, nil, nil, nil); len(got) != 1 {
		t.Fatalf("absent pools counted: %+v", got)
	}
	got := serverPoolBudgets(runtime, nil, readinessPool, background, maintenance)
	if len(got) != 4 || got[0].Identity != "runtime" || got[1].Identity != "readiness" || got[2].Identity != "background" || got[3].Identity != "maintenance-1" {
		t.Fatalf("composition order: %+v", got)
	}
}

func TestReadinessRejectsDeclaredCeilingAboveActualAndPreservesRoleChecks(t *testing.T) {
	f := testpg.New(t)
	cfg := boundReadinessConfig()
	if _, err := authority.InitServerState(t.Context(), f.Owner, cfg.ServerInstanceID, cfg.Auth.ActorIDPepper); err != nil {
		t.Fatal(err)
	}
	var actual int32
	if err := f.Runtime.QueryRow(t.Context(), "SELECT current_setting('max_connections')::integer").Scan(&actual); err != nil {
		t.Fatal(err)
	}
	pools := actualPoolBudgets(f.Runtime, nil, nil, f.Maintenance)
	audit := func(pool *pgxpool.Pool, budget config.DatabasePools, budgets []database.NamedPoolBudget) error {
		t.Helper()
		r := readinessOver(t, cfg, pool, f.Maintenance, withBudget(budget, budgets))
		if err := r.inventory.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		return refreshed(t, r)
	}
	budget := config.DatabasePools{Runtime: 32, Maintenance: 4, ServerMax: actual + 1, Reserved: 8}
	if err := audit(f.Runtime, budget, pools); err == nil {
		t.Fatal("lower actual PostgreSQL ceiling accepted")
	}
	budget.ServerMax = actual
	if err := audit(f.Runtime, budget, pools); err != nil {
		t.Fatalf("valid ceiling not ready: %v", err)
	}
	// The budget never replaces the runtime role check: the owner login
	// holds privileges the runtime manifest does not grant.
	if err := audit(f.Owner, budget, pools); err == nil {
		t.Fatal("budget replaced the runtime role check")
	}
	if err := audit(f.Runtime, budget, []database.NamedPoolBudget{{Identity: "runtime", Max: actual}}); err == nil {
		t.Fatal("overcommitted effective budget accepted")
	}
}

// The export worker must progress on the minimum one-connection
// maintenance pool.
func TestMinimumMaintenanceMakesExportProgress(t *testing.T) {
	h := testkit.New(t)
	cfg := h.Maintenance.Config()
	cfg.MaxConns = 1
	cfg.MinConns = 0
	cfg.MinIdleConns = 0
	maintenance, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json")); err != nil {
		t.Fatal(err)
	}
	var key [32]byte
	key[0] = 1
	// Create stamps next_attempt_at with the service clock and the worker
	// claims WHERE next_attempt_at<=now(), the database clock; a container
	// clock trailing the host's left the job queued after RunOnce (the
	// intermittent "export did not progress: State:queued"). One clock.
	service, worker, err := familyexport.New(familyexport.Options{Authority: h.Authority, Identity: h.Info, Runtime: h.Runtime, WorkerPool: maintenance, Keys: map[string][32]byte{"test.v1": key}, ActiveKeyID: "test.v1", Now: h.DatabaseNow})
	if err != nil {
		t.Fatal(err)
	}
	created, _, err := service.Create(t.Context(), h.Owner, protocol.CreateExportRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityExportV1}}, RequestID: "30000000-0000-4000-8000-000000004001"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := worker.RunOnce(ctx, 1); err != nil {
		t.Fatalf("minimum maintenance worker: %v", err)
	}
	status, err := service.Status(ctx, h.Owner, created.ExportID)
	if err != nil || status.State != "ready" {
		t.Fatalf("export did not progress: %+v %v", status, err)
	}
	if maintenance.Config().MaxConns != 1 {
		t.Fatal("test did not use minimum maintenance pool")
	}
}
