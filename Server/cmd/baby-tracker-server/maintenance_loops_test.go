package main

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/database"
	"babytracker/server/internal/maintenance"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The maintenance loops as main composes them: retention prunes through
// the background authority (the one the key inventory reads, on the
// one-connection background pool, never a request connection) half an
// interval out of phase, and the audit partition roll runs hourly on the
// maintenance pool inside its worker slots; both behind the node readiness.
func TestMaintenanceLoopsComposition(t *testing.T) {
	f := testpg.New(t)
	cfg := boundReadinessConfig()
	runtimeDSN := f.Runtime.Config().ConnString()
	readinessPool, err := database.OpenReadiness(t.Context(), runtimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(readinessPool.Close)
	background, err := database.OpenBackground(t.Context(), runtimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(background.Close)
	var login string
	if err := f.Maintenance.QueryRow(t.Context(), "SELECT current_user").Scan(&login); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.InitServerState(t.Context(), f.Owner, cfg.ServerInstanceID, cfg.Auth.ActorIDPepper); err != nil {
		t.Fatal(err)
	}
	ready, _, err := composeNodeReadiness(cfg, readinessPools{Readiness: readinessPool, Maintenance: []*pgxpool.Pool{f.Maintenance},
		MaintenanceLogin: login}, background, nil)
	if err != nil {
		t.Fatal(err)
	}
	slots, err := database.NewWorkerSlots(f.Maintenance)
	if err != nil {
		t.Fatal(err)
	}
	retention, roll, pushTokens := maintenanceLoops(ready, f.Maintenance, slots, slog.New(slog.DiscardHandler))

	if maintenance.RetentionInterval != 10*time.Second || maintenance.AuditPartitionInterval != time.Hour {
		t.Fatalf("cadences %v and %v, want retention every 10 s and the audit roll hourly", maintenance.RetentionInterval, maintenance.AuditPartitionInterval)
	}
	if retention.Interval != maintenance.RetentionInterval || retention.Offset != maintenance.RetentionInterval/2 || len(retention.Sweeps) != 5 {
		t.Fatalf("retention loop %+v", retention)
	}
	sessions, ok := retention.Sweeps[0].(*maintenance.SessionSweep)
	if !ok || ready.background == nil || sessions.Authority != ready.background || sessions.Logger == nil {
		t.Fatalf("retention sweeps %+v, want SessionSweep over the background authority", retention.Sweeps)
	}
	// Care retention: the receipt sweep and the three expiry sweeps on the
	// maintenance pool inside its worker slots, at the production batch size.
	receipts, ok := retention.Sweeps[1].(*maintenance.ReceiptSweep)
	if !ok || receipts.Pool != f.Maintenance || receipts.Slots != slots || receipts.Batch != 0 || receipts.Logger == nil {
		t.Fatalf("retention sweep %+v, want the receipt sweep on the maintenance pool inside its slots", retention.Sweeps[1])
	}
	for i, table := range []string{"sync_page_bindings", "sync_snapshot_sessions", "device_sync_cursors"} {
		expiry, ok := retention.Sweeps[2+i].(*maintenance.ExpirySweep)
		if !ok || expiry.Table != table || expiry.Pool != f.Maintenance || expiry.Slots != slots || expiry.Logger == nil {
			t.Fatalf("retention sweep %d %+v, want the %s expiry sweep on the maintenance pool inside its slots", 2+i, retention.Sweeps[2+i], table)
		}
	}
	if roll.Interval != maintenance.AuditPartitionInterval || roll.Offset != 0 || roll.FailureRetry != maintenance.DefaultFailureRetry || len(roll.Sweeps) != 1 {
		t.Fatalf("audit partition loop %+v", roll)
	}
	partitions, ok := roll.Sweeps[0].(maintenance.AuditPartitionSweep)
	if !ok || partitions.Pool != f.Maintenance || partitions.Slots != slots {
		t.Fatalf("audit partition sweeps %+v, want the maintenance pool inside its slots", roll.Sweeps)
	}
	// Push tokens without a family session: their own hourly loop (the
	// pre-serving audit roll stays roll-only, and neither one's failure
	// re-runs the other), first a few minutes after startup.
	if pushTokens.Name != "push-tokens" || pushTokens.Interval != maintenance.PushTokenInterval || pushTokens.Offset != pushTokenLoopOffset ||
		pushTokenLoopOffset <= 0 || pushTokens.FailureRetry != maintenance.DefaultFailureRetry || len(pushTokens.Sweeps) != 1 {
		t.Fatalf("push-token loop %+v", pushTokens)
	}
	tokens, ok := pushTokens.Sweeps[0].(*maintenance.PushTokenSweep)
	if !ok || tokens.Pool != f.Maintenance || tokens.Slots != slots || tokens.Batch != 0 || tokens.Logger == nil {
		t.Fatalf("push-token sweeps %+v, want the push-token sweep on the maintenance pool inside its slots", pushTokens.Sweeps)
	}
	// The loops read the node's cached readiness: refused before the
	// monitor's first refresh, admitted after it.
	if retention.Ready == nil || roll.Ready == nil || pushTokens.Ready == nil ||
		retention.Ready(t.Context()) == nil || roll.Ready(t.Context()) == nil || pushTokens.Ready(t.Context()) == nil {
		t.Fatal("the loops run before the node is ready")
	}
	if err := ready.inventory.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := refreshed(t, ready); err != nil {
		t.Fatalf("composed node not ready: %v", err)
	}
	if err := retention.Ready(t.Context()); err != nil {
		t.Fatalf("retention gate on a ready node: %v", err)
	}
	if err := roll.Ready(t.Context()); err != nil {
		t.Fatalf("audit roll gate on a ready node: %v", err)
	}
	if err := pushTokens.Ready(t.Context()); err != nil {
		t.Fatalf("push-token gate on a ready node: %v", err)
	}

	// One retention pass prunes through the background pool: an expired
	// sign-in challenge goes.
	seed, err := f.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Rollback(context.Background())
	if _, err := seed.Exec(t.Context(), `SELECT set_config('app.authority','1',true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(t.Context(), `INSERT INTO auth_challenges(token_digest, expires_at, created_at)
		VALUES (sha256('expired'::bytea), now() - interval '1 hour', now() - interval '65 minutes')`); err != nil {
		t.Fatal(err)
	}
	if err := seed.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if counts, err := retention.Pass(t.Context()); err != nil || len(counts) != 5 || counts["sessions"] != 1 {
		t.Fatalf("retention pass %v (%v), want every sweep run and the expired challenge pruned", counts, err)
	}
	if counts, err := roll.Pass(t.Context()); err != nil || counts["audit-partitions"] != 0 {
		t.Fatalf("audit roll %v (%v) after the migration's 92 days", counts, err)
	}
}

// A node that starts ready rolls the audit partitions once before serving,
// and the loop's first pass then waits a whole interval (no second roll at
// startup). A node not ready yet does not roll (liveness never waits on an
// unreachable database), nor does a roll that fails (55P03 behind a reader):
// either way the loop's first pass comes at once.
func TestAuditRollBeforeServing(t *testing.T) {
	f := testpg.New(t)
	cfg := boundReadinessConfig()
	runtimeDSN := f.Runtime.Config().ConnString()
	readinessPool, err := database.OpenReadiness(t.Context(), runtimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(readinessPool.Close)
	background, err := database.OpenBackground(t.Context(), runtimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(background.Close)
	var login string
	if err := f.Maintenance.QueryRow(t.Context(), "SELECT current_user").Scan(&login); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.InitServerState(t.Context(), f.Owner, cfg.ServerInstanceID, cfg.Auth.ActorIDPepper); err != nil {
		t.Fatal(err)
	}
	ready, _, err := composeNodeReadiness(cfg, readinessPools{Readiness: readinessPool, Maintenance: []*pgxpool.Pool{f.Maintenance},
		MaintenanceLogin: login}, background, nil)
	if err != nil {
		t.Fatal(err)
	}
	slots, err := database.NewWorkerSlots(f.Maintenance)
	if err != nil {
		t.Fatal(err)
	}
	_, roll, _ := maintenanceLoops(ready, f.Maintenance, slots, slog.New(slog.DiscardHandler))
	months := func() []string {
		rows, err := f.Owner.Query(t.Context(), `SELECT c.relname::text FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid
			WHERE i.inhparent='public.security_audit_events'::regclass AND c.relname ~ '^security_audit_events_[0-9]{4}_[0-9]{2}$' ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return names
	}
	present := months()
	if len(present) == 0 {
		t.Fatal("the migration created no monthly partition")
	}
	last := pgx.Identifier{present[len(present)-1]}.Sanitize()
	if _, err := f.Owner.Exec(t.Context(), `ALTER TABLE public.security_audit_events DETACH PARTITION public.`+last); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Owner.Exec(t.Context(), `DROP TABLE public.`+last); err != nil {
		t.Fatal(err)
	}

	// Not ready yet: no roll, and the loop starts at once.
	if got := rollBeforeServing(t.Context(), roll); got.Offset != 0 || len(months()) != len(present)-1 {
		t.Fatalf("not ready: offset %v, %d months (want 0 and no roll)", got.Offset, len(months()))
	}
	if err := ready.inventory.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := refreshed(t, ready); err != nil {
		t.Fatalf("composed node not ready: %v", err)
	}
	// Ready, but the roll gives up behind a reader of the audit history.
	reader, err := f.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback(context.Background())
	if _, err := reader.Exec(t.Context(), `SELECT set_config('app.authority','1',true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Exec(t.Context(), `SELECT count(*) FROM security_audit_events`); err != nil {
		t.Fatal(err)
	}
	if got := rollBeforeServing(t.Context(), roll); got.Offset != 0 || len(months()) != len(present)-1 {
		t.Fatalf("a failed roll: offset %v, %d months (want 0 and no partition)", got.Offset, len(months()))
	}
	if err := reader.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Ready: the roll runs before serving and the loop waits an interval.
	got := rollBeforeServing(t.Context(), roll)
	if got.Offset != maintenance.AuditPartitionInterval || len(months()) != len(present) {
		t.Fatalf("ready: offset %v, %d months (want an interval and the month rolled back)", got.Offset, len(months()))
	}
	// main's startMaintenance rolls before it returns and starts the audit
	// loop an interval later.
	if _, err := f.Owner.Exec(t.Context(), `ALTER TABLE public.security_audit_events DETACH PARTITION public.`+last); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Owner.Exec(t.Context(), `DROP TABLE public.`+last); err != nil {
		t.Fatal(err)
	}
	requestAuthority, err := authority.New(authorityOptions(cfg, f.Runtime))
	if err != nil {
		t.Fatal(err)
	}
	started, stop := startMaintenance(t.Context(), ready, requestAuthority, f.Maintenance, slots, slog.New(slog.DiscardHandler))
	rolled := len(months())
	stop()
	if len(started) != 5 || started[0].Name != "retention" || started[1].Name != "audit-partitions" ||
		started[1].Offset != maintenance.AuditPartitionInterval || started[2].Name != "push-tokens" ||
		started[2].Offset != pushTokenLoopOffset || started[3].Name != "family-purge" || started[4].Name != "audit-retention" || rolled != len(present) {
		t.Fatalf("startMaintenance started %d loops (audit offset %v), %d months on return; want retention, the audit loop an interval out "+
			"(the month rolled), the push-token loop at its offset, the family purge and the audit retention", len(started), started[min(1, len(started)-1)].Offset, rolled)
	}
	if purger, ok := started[3].Sweeps[0].(*maintenance.Purger); !ok || purger.Authority != requestAuthority {
		t.Fatalf("startMaintenance's purge loop %+v, want the purge over the request authority", started[3])
	}
	// Audit retention (owner decision D1): its own hourly loop, well after
	// startup, on the maintenance pool inside its worker slots, behind
	// readiness; the pre-serving roll stays roll-only.
	expiry := started[4]
	sweep, ok := expiry.Sweeps[0].(maintenance.AuditRetentionSweep)
	if len(expiry.Sweeps) != 1 || !ok || sweep.Pool != f.Maintenance || sweep.Slots != slots || sweep.Logger == nil ||
		expiry.Interval != maintenance.AuditRetentionInterval || expiry.Offset != auditRetentionLoopOffset || auditRetentionLoopOffset <= pushTokenLoopOffset ||
		expiry.FailureRetry != maintenance.AuditRetentionInterval || expiry.Timeout != maintenance.AuditRetentionSweepTimeout || expiry.Ready == nil || expiry.Now == nil {
		t.Fatalf("audit retention loop %+v, want the retention sweep hourly (a failure retried an hour later) on the maintenance pool inside its slots, after the push-token loop", expiry)
	}
	if len(started[1].Sweeps) != 1 {
		t.Fatalf("the audit roll loop runs %d sweeps, want the roll alone", len(started[1].Sweeps))
	}
}

// The purge loop as main composes it: every 30 s, a retention interval out
// of phase, over the request authority (its invalidations reach the live
// hub), never the background one, and the maintenance pool inside its
// worker slots, behind the node's readiness. Two composed passes purge a
// due deletion-pending family through the real maintenance login (the
// purge step's grant included).
func TestPurgeLoopComposition(t *testing.T) {
	f := testpg.New(t)
	cfg := boundReadinessConfig()
	runtimeDSN := f.Runtime.Config().ConnString()
	readinessPool, err := database.OpenReadiness(t.Context(), runtimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(readinessPool.Close)
	background, err := database.OpenBackground(t.Context(), runtimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(background.Close)
	var login string
	if err := f.Maintenance.QueryRow(t.Context(), "SELECT current_user").Scan(&login); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.InitServerState(t.Context(), f.Owner, cfg.ServerInstanceID, cfg.Auth.ActorIDPepper); err != nil {
		t.Fatal(err)
	}
	ready, _, err := composeNodeReadiness(cfg, readinessPools{Readiness: readinessPool, Maintenance: []*pgxpool.Pool{f.Maintenance},
		MaintenanceLogin: login}, background, nil)
	if err != nil {
		t.Fatal(err)
	}
	slots, err := database.NewWorkerSlots(f.Maintenance)
	if err != nil {
		t.Fatal(err)
	}
	requestAuthority, err := authority.New(authorityOptions(cfg, f.Runtime))
	if err != nil {
		t.Fatal(err)
	}
	loop := purgeLoop(ready, requestAuthority, f.Maintenance, slots, slog.New(slog.DiscardHandler))
	if maintenance.PurgeInterval != 30*time.Second || loop.Name != "family-purge" || loop.Interval != maintenance.PurgeInterval ||
		loop.Offset != maintenance.RetentionInterval || loop.Ready == nil || loop.Now == nil || len(loop.Sweeps) != 1 {
		t.Fatalf("purge loop %+v, want family-purge every 30 s, 10 s out of phase, behind readiness", loop)
	}
	purger, ok := loop.Sweeps[0].(*maintenance.Purger)
	if !ok || purger.Authority != requestAuthority || purger.Authority == ready.background || purger.Pool != f.Maintenance || purger.Slots != slots ||
		purger.Batch != 0 || purger.Logger == nil {
		t.Fatalf("purge sweep %+v, want the purge over the request authority on the maintenance pool inside its slots", loop.Sweeps[0])
	}
	if err := loop.Ready(t.Context()); err == nil {
		t.Fatal("the purge loop runs before the node is ready")
	}
	if err := ready.inventory.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := refreshed(t, ready); err != nil {
		t.Fatalf("composed node not ready: %v", err)
	}

	// A deletion-pending family due a day ago.
	seed, err := f.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Rollback(context.Background())
	const owner, family = "c0000000-0000-4000-8000-000000000001", "f0000000-0000-4000-8000-000000000001"
	for _, statement := range []string{
		`SELECT set_config('app.authority','1',true), set_config('app.family_id','` + family + `',true)`,
		`INSERT INTO accounts(id,display_name,status,created_at,updated_at) VALUES ('` + owner + `','Owner','active',now(),now())`,
		`INSERT INTO families(id,display_name,status,owner_account_id,purge_after,created_at,updated_at)
			VALUES ('` + family + `','Gone','deletion_pending','` + owner + `',now() - interval '1 day',now(),now())`,
	} {
		if _, err := seed.Exec(t.Context(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := seed.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	status := func() string {
		var s string
		if err := f.Maintenance.QueryRow(t.Context(), `SELECT status::text FROM families WHERE id=$1`, family).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if _, err := loop.Pass(t.Context()); err != nil || status() != "purge_started" {
		t.Fatalf("first pass: %v, status %s; want the purge started", err, status())
	}
	if _, err := loop.Pass(t.Context()); err != nil || status() != "purge_completed" {
		t.Fatalf("second pass: %v, status %s; want the purge completed", err, status())
	}
}
