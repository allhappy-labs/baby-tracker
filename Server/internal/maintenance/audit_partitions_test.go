package maintenance_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/database"
	"babytracker/server/internal/maintenance"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestAuditPartitionSweepKeepsThreeMonthsAhead(t *testing.T) {
	if maintenance.AuditPartitionHorizon != 92*24*time.Hour {
		t.Fatalf("audit partition horizon %v, want three months (92 days)", maintenance.AuditPartitionHorizon)
	}
	h := testkit.New(t)
	// The migration rolled 92 days ahead; drop its furthest month (empty)
	// so the sweep has one to create.
	before := monthlyPartitions(t, h)
	if len(before) == 0 {
		t.Fatal("the migration created no monthly partition")
	}
	dropPartition(t, h, before[len(before)-1])
	sweep := maintenance.AuditPartitionSweep{Pool: h.Maintenance, Logger: slog.New(&records{})}
	now := h.DatabaseNow()
	if n, err := sweep.Run(t.Context(), now); err != nil || n != 1 {
		t.Fatalf("first run created %d (%v), want the dropped month", n, err)
	}
	have := monthlyPartitions(t, h)
	for _, month := range auditMonths(now, now.Add(maintenance.AuditPartitionHorizon)) {
		if !slices.Contains(have, month) {
			t.Fatalf("no partition %s within the 92-day horizon (have %v)", month, have)
		}
	}
	if n, err := sweep.Run(t.Context(), now); err != nil || n != 0 {
		t.Fatalf("second run created %d (%v), want 0", n, err)
	}
	// The harness's sign-ins, refreshes and family commands all landed in a
	// dated partition.
	if n := authorityCount(t, h, `SELECT count(*) FROM security_audit_events WHERE tableoid='public.security_audit_events_default'::regclass`); n != 0 {
		t.Fatalf("%d audit rows in the default partition", n)
	}
	if n := authorityCount(t, h, `SELECT count(*) FROM security_audit_events`); n == 0 {
		t.Fatal("the harness wrote no audit row: the default-partition check is vacuous")
	}
}

// The horizon is the sweep's clock plus 92 days (the audit rows' `at` is
// stamped by the app clock), not the database's.
func TestAuditPartitionSweepRollsToTheSweepClocksHorizon(t *testing.T) {
	h := testkit.New(t)
	now := h.DatabaseNow().Add(200 * 24 * time.Hour)
	sweep := maintenance.AuditPartitionSweep{Pool: h.Maintenance, Logger: slog.New(&records{})}
	if _, err := sweep.Run(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	have := monthlyPartitions(t, h)
	want := "security_audit_events_" + now.Add(maintenance.AuditPartitionHorizon).UTC().Format("2006_01")
	if !slices.Contains(have, want) {
		t.Fatalf("no partition %s for the sweep clock's horizon (have %v)", want, have)
	}
}

// Two sweeps at once create each missing month once and neither fails (the
// roller's advisory lock serializes them); the next run creates nothing.
func TestAuditPartitionSweepsRunConcurrently(t *testing.T) {
	h := testkit.New(t)
	now := h.DatabaseNow().Add(200 * 24 * time.Hour)
	before := monthlyPartitions(t, h)
	var missing int
	for _, month := range auditMonths(h.DatabaseNow(), now.Add(maintenance.AuditPartitionHorizon)) {
		if !slices.Contains(before, month) {
			missing++
		}
	}
	if missing == 0 {
		t.Fatal("nothing to create")
	}
	sweep := maintenance.AuditPartitionSweep{Pool: h.Maintenance, Logger: slog.New(&records{})}
	var wg sync.WaitGroup
	created := make([]int, 2)
	errs := make([]error, 2)
	start := make(chan struct{})
	for i := range 2 {
		wg.Go(func() {
			<-start
			created[i], errs[i] = sweep.Run(t.Context(), now)
		})
	}
	close(start)
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || created[0]+created[1] != missing {
		t.Fatalf("concurrent sweeps created %v (%v), want %d months between them", created, errs, missing)
	}
	if n, err := sweep.Run(t.Context(), now); err != nil || n != 0 {
		t.Fatalf("the run after created %d (%v)", n, err)
	}
}

// A month the default partition already holds rows of cannot be attached:
// the sweep still creates every other month, and logs the skipped one at
// error level.
func TestAuditPartitionSweepLogsASkippedMonth(t *testing.T) {
	h := testkit.New(t)
	now := h.DatabaseNow().Add(200 * 24 * time.Hour)
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	h.AuthorityExec(t, `INSERT INTO security_audit_events(scope,scope_id,seq,prev_hash,hash,kind,command_id,actor_account_id,at,payload)
		VALUES ('account',gen_random_uuid(),1,sha256('p'::bytea),sha256('h'::bytea),'session_rotated',gen_random_uuid(),gen_random_uuid(),$1,'{}')`, month.Add(time.Hour))
	log := &records{}
	sweep := maintenance.AuditPartitionSweep{Pool: h.Maintenance, Logger: slog.New(log)}
	skipped := "security_audit_events_" + month.Format("2006_01")
	n, err := sweep.Run(t.Context(), now)
	if err != nil || n == 0 {
		t.Fatalf("created %d (%v), want every month but the skipped one", n, err)
	}
	if have := monthlyPartitions(t, h); slices.Contains(have, skipped) {
		t.Fatalf("the refused month %s exists", skipped)
	}
	errorsLogged := log.at(slog.LevelError)
	if len(errorsLogged) != 1 || attr(errorsLogged[0], "partition") != skipped {
		t.Fatalf("logged %v at error, want one naming %s", errorsLogged, skipped)
	}
}

// Behind a reader of the whole audit history the roll gives up within its
// lock timeout (55P03) instead of queueing audit inserts behind itself, and
// the next run, once the reader is gone, creates the months.
func TestAuditPartitionSweepGivesUpBehindALockAndRetries(t *testing.T) {
	h := testkit.New(t)
	now := h.DatabaseNow().Add(200 * 24 * time.Hour)
	reader, err := h.Migrator.Begin(t.Context())
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
	sweep := maintenance.AuditPartitionSweep{Pool: h.Maintenance, Logger: slog.New(&records{})}
	start := time.Now()
	_, err = sweep.Run(t.Context(), now)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("behind a reader: %v, want 55P03", err)
	}
	if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
		t.Fatalf("the roll waited %v behind a reader", elapsed)
	}
	if err := reader.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n, err := sweep.Run(t.Context(), now); err != nil || n == 0 {
		t.Fatalf("the retry created %d (%v)", n, err)
	}
}

// The roll takes a maintenance worker slot for its round trip: with every
// slot held it waits (and gives up with its context) without touching the
// pool, and it runs once a slot frees.
func TestAuditPartitionSweepRunsInsideAWorkerSlot(t *testing.T) {
	h := testkit.New(t)
	pool, err := database.OpenWithMax(t.Context(), h.Maintenance.Config().ConnString(), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	slots, err := database.NewWorkerSlots(pool)
	if err != nil {
		t.Fatal(err)
	}
	release, err := slots.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sweep := maintenance.AuditPartitionSweep{Pool: pool, Slots: slots, Logger: slog.New(&records{})}
	now := h.DatabaseNow().Add(200 * 24 * time.Hour)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := sweep.Run(ctx, now); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("with every slot held: %v, want to wait for one", err)
	}
	if acquired := pool.Stat().AcquireCount(); acquired != 0 {
		t.Fatalf("the roll took %d pool connections outside a slot", acquired)
	}
	release()
	if n, err := sweep.Run(t.Context(), now); err != nil || n == 0 {
		t.Fatalf("with a free slot: created %d (%v)", n, err)
	}
}

// auditMonths is app.ensure_audit_partitions' month walk: every month from
// the later of 2027-01 and the database's current month whose first instant
// is at or before through.
func auditMonths(now, through time.Time) []string {
	start := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	if floor := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC); start.Before(floor) {
		start = floor
	}
	var months []string
	for month := start; !month.After(through); month = month.AddDate(0, 1, 0) {
		months = append(months, "security_audit_events_"+month.Format("2006_01"))
	}
	return months
}

func monthlyPartitions(t *testing.T, h *testkit.Harness) []string {
	t.Helper()
	rows, err := h.Migrator.Query(t.Context(), `SELECT c.relname::text FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid
		WHERE i.inhparent='public.security_audit_events'::regclass AND c.relname ~ '^security_audit_events_[0-9]{4}_[0-9]{2}$' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	months, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return months
}

func dropPartition(t *testing.T, h *testkit.Harness, name string) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.Migrator.Exec(ctx, `ALTER TABLE public.security_audit_events DETACH PARTITION public.`+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Migrator.Exec(ctx, `DROP TABLE public.`+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
}
