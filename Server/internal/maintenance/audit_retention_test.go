package maintenance_test

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/maintenance"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// retentionClock is the sweep's app clock in these tests: 13 months back from
// it is 2025-09-16 12:00 UTC, before the database's own clock minus 13 months
// whenever the tests run, so the app clock decides what expires.
var retentionClock = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func TestAuditRetentionKeepsThirteenMonths(t *testing.T) {
	if maintenance.AuditRetentionMonths != 13 || maintenance.AuditRetentionInterval != time.Hour {
		t.Fatalf("retention %d months every %v, want 13 months (owner decision D1) hourly", maintenance.AuditRetentionMonths, maintenance.AuditRetentionInterval)
	}
	if (maintenance.AuditRetentionSweep{}).Name() != "audit-retention" {
		t.Fatal("sweep name")
	}
}

// Only partitions whose whole range ended 13 months before the sweep's clock
// are dropped, oldest first: 2025-01 and 2025-08 (ended by 2025-09-01) go;
// 2025-09, which began before the cutoff but ends after it, the 2026
// partition, the monthly partitions ahead and the default partition stay.
// Each chain with rows in a dropped partition gets the seq and hash of its
// last dropped row as its retention anchor; an anchor already further along
// is never moved back.
func TestRetentionDropsOnlyExpiredPartitions(t *testing.T) {
	h := testkit.New(t)
	for _, month := range []string{"2025-01", "2025-08", "2025-09"} {
		createMonth(t, h, month)
	}
	before := auditPartitions(t, h)
	a, b, c, d, e, g := newChain(t, h, "account", 4), newChain(t, h, "family", 2), newChain(t, h, "account", 1), newChain(t, h, "account", 9),
		newChain(t, h, "account", 2), newChain(t, h, "account", 2)
	for _, row := range []struct {
		scope, id string
		seq       int
		at        string
	}{
		{"account", a, 1, "2025-01-10 12:00"}, {"account", a, 2, "2025-08-10 12:00"}, {"account", a, 3, "2025-09-10 12:00"}, {"account", a, 4, "2026-02-10 12:00"},
		{"family", b, 1, "2025-01-10 12:00"}, {"family", b, 2, "2025-01-11 12:00"},
		{"account", c, 1, "2025-09-10 12:00"},
		{"account", d, 3, "2025-01-12 12:00"},
		// e's first row is a legacy one in the default partition (no 2024
		// partition), below the row that anchors it.
		{"account", e, 1, "2024-06-01 12:00"}, {"account", e, 2, "2025-01-12 12:00"},
		// g's two rows straddle a month boundary out of order (clock skew):
		// seq 1 just after it, in the kept 2025-09, seq 2 just before it.
		{"account", g, 1, "2025-09-01 00:00:01"}, {"account", g, 2, "2025-08-31 23:59:59"},
	} {
		auditRow(t, h, row.scope, row.id, row.seq, row.at)
	}
	// d's anchor is already past what the drop holds of it.
	h.AuthorityExec(t, `INSERT INTO audit_retention_anchors(scope,scope_id,anchor_seq,anchor_hash,anchored_at,dropped_before) VALUES ('account',$1,9,sha256('d9'::bytea),now(),'2024-12-01 00:00+00')`, d)

	log := &records{}
	sweep := maintenance.AuditRetentionSweep{Pool: h.Maintenance, Logger: slog.New(log)}
	n, err := sweep.Run(t.Context(), retentionClock)
	if err != nil || n != 2 {
		t.Fatalf("dropped %d (%v), want 2025-01 and 2025-08", n, err)
	}
	after := auditPartitions(t, h)
	var gone []string
	for _, name := range before {
		if !slices.Contains(after, name) {
			gone = append(gone, name)
		}
	}
	if !slices.Equal(gone, []string{"security_audit_events_2025_01", "security_audit_events_2025_08"}) || len(after) != len(before)-2 {
		t.Fatalf("dropped %v (left %v), want exactly 2025-01 and 2025-08", gone, after)
	}
	for _, kept := range []string{"security_audit_events_2025_09", "security_audit_events_2026", "security_audit_events_default", "security_audit_events_2027_01"} {
		if !slices.Contains(after, kept) {
			t.Fatalf("%s was dropped (left %v)", kept, after)
		}
	}
	// The detached partitions are gone, not left behind as tables.
	var leftovers int
	if err := h.Migrator.QueryRow(t.Context(), `SELECT count(*) FROM pg_class WHERE relname IN ('security_audit_events_2025_01','security_audit_events_2025_08')`).Scan(&leftovers); err != nil || leftovers != 0 {
		t.Fatalf("%d dropped partitions remain as tables (%v)", leftovers, err)
	}
	if n := authorityCount(t, h, `SELECT count(*) FROM audit_retention_anchors`); n != 5 {
		t.Fatalf("%d anchors, want a's, b's, d's, e's and g's (none for c)", n)
	}
	// Each anchor records the end of the partition its row was dropped with
	// (d's, already further along, is untouched).
	for _, want := range []struct {
		id, scope     string
		seq           int
		hash, dropped string
	}{{a, "account", 2, a + "2", "2025-09-01"}, {b, "family", 2, b + "2", "2025-02-01"}, {d, "account", 9, "d9", "2024-12-01"},
		{e, "account", 2, e + "2", "2025-02-01"}, {g, "account", 2, g + "2", "2025-09-01"}} {
		if authorityCount(t, h, `SELECT count(*) FROM audit_retention_anchors WHERE scope_id=$1 AND scope=$2::text::app.audit_scope AND anchor_seq=$3
			AND anchor_hash=sha256($4::text::bytea) AND dropped_before=($5::text||' 00:00+00')::timestamptz`, want.id, want.scope, want.seq, want.hash, want.dropped) != 1 {
			t.Fatalf("no anchor of %s at %d with its last dropped row's hash and partition end %s", want.id, want.seq, want.dropped)
		}
	}
	if n := authorityCount(t, h, `SELECT count(*) FROM security_audit_events WHERE scope_id=ANY($1::uuid[])`, []string{a, b, c, d, e, g}); n != 5 {
		t.Fatalf("%d seeded rows survive, want a's 2025-09 and 2026 rows, c's, e's legacy row and g's first", n)
	}
	infos := log.at(slog.LevelInfo)
	if len(infos) != 2 || attr(infos[0], "partition") != "security_audit_events_2025_01" || attr(infos[1], "partition") != "security_audit_events_2025_08" {
		t.Fatalf("logged %v, want each dropped partition, oldest first", infos)
	}
	if n, err := sweep.Run(t.Context(), retentionClock); err != nil || n != 0 {
		t.Fatalf("the next run dropped %d (%v), want 0", n, err)
	}
}

// Whatever clock the sweep is given, nothing holding rows from the last 13
// months by the database's clock is dropped: with an app clock three years
// ahead, an expired 2025 partition goes, but the partition holding the
// database's now, the one after it and every later one stay. A cutoff that
// is not a finite time is refused.
func TestRetentionNeverDropsTheCurrentOrNextPartition(t *testing.T) {
	h := testkit.New(t)
	createMonth(t, h, "2025-01")
	now := h.DatabaseNow()
	bounds := auditPartitionBounds(t, h)
	var current, next string
	for name, b := range bounds {
		if !b[0].After(now) && now.Before(b[1]) {
			current = name
		}
	}
	for name, b := range bounds {
		if current != "" && b[0].Equal(bounds[current][1]) {
			next = name
		}
	}
	if current == "" || next == "" {
		t.Fatalf("no current or next partition in %v", bounds)
	}
	recent := 0
	for _, b := range bounds {
		if b[1].After(now.AddDate(0, -13, 0)) {
			recent++
		}
	}

	sweep := maintenance.AuditRetentionSweep{Pool: h.Maintenance, Logger: slog.New(&records{})}
	n, err := sweep.Run(t.Context(), now.AddDate(3, 0, 0))
	if err != nil || n != 1 {
		t.Fatalf("an app clock three years ahead dropped %d (%v), want only 2025-01", n, err)
	}
	after := auditPartitionBounds(t, h)
	if _, ok := after[current]; !ok {
		t.Fatalf("the current partition %s was dropped", current)
	}
	if _, ok := after[next]; !ok {
		t.Fatalf("the next partition %s was dropped", next)
	}
	if _, ok := after["security_audit_events_2025_01"]; ok || len(after) != recent || len(after) != len(bounds)-1 {
		t.Fatalf("partitions after %v, want every one of the last 13 months (%d) and not 2025-01", after, recent)
	}
	if !slices.Contains(auditPartitions(t, h), "security_audit_events_default") {
		t.Fatal("the default partition was dropped")
	}
	// The harness's own audit rows (this month's) are all still there.
	if n := authorityCount(t, h, `SELECT count(*) FROM security_audit_events`); n == 0 {
		t.Fatal("the harness's audit rows are gone")
	}
	for _, cutoff := range []string{"infinity", "-infinity"} {
		var pgErr *pgconn.PgError
		_, err := h.Maintenance.Exec(t.Context(), `SELECT app.drop_expired_audit_partition($1::timestamptz)`, cutoff)
		if !errors.As(err, &pgErr) || pgErr.Code != "22023" {
			t.Fatalf("cutoff %s: %v, want 22023", cutoff, err)
		}
	}
}

// Behind a reader of the audit history the drop gives up within its 1s lock
// timeout (55P03), leaving the partition and no anchor, and the next run,
// once the reader is gone, drops it.
func TestAuditRetentionGivesUpBehindALockAndRetries(t *testing.T) {
	h := testkit.New(t)
	createMonth(t, h, "2025-01")
	account := newChain(t, h, "account", 1)
	auditRow(t, h, "account", account, 1, "2025-01-10 12:00")
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
	sweep := maintenance.AuditRetentionSweep{Pool: h.Maintenance, Logger: slog.New(&records{})}
	start := time.Now()
	_, err = sweep.Run(t.Context(), retentionClock)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("behind a reader: %v, want 55P03", err)
	}
	if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
		t.Fatalf("the drop waited %v behind a reader", elapsed)
	}
	if !slices.Contains(auditPartitions(t, h), "security_audit_events_2025_01") || authorityCount(t, h, `SELECT count(*) FROM audit_retention_anchors`) != 0 {
		t.Fatal("a drop that gave up left a change behind")
	}
	if err := reader.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n, err := sweep.Run(t.Context(), retentionClock); err != nil || n != 1 {
		t.Fatalf("the retry dropped %d (%v)", n, err)
	}
	if authorityCount(t, h, `SELECT count(*) FROM audit_retention_anchors WHERE scope_id=$1 AND anchor_seq=1 AND anchor_hash=sha256(($1::text||'1')::bytea)`, account) != 1 {
		t.Fatal("the retry recorded no anchor")
	}
}

// Two sweeps at once drop each expired partition once and neither fails:
// the drop takes the partition roll's advisory lock, so the second waits for
// the first and then finds the next partition (or none). Both partitions'
// rows anchor their chain.
func TestAuditRetentionSweepsRunConcurrently(t *testing.T) {
	h := testkit.New(t)
	account := newChain(t, h, "account", 2)
	for i, month := range []string{"2025-01", "2025-02"} {
		createMonth(t, h, month)
		auditRow(t, h, "account", account, i+1, month+"-10 12:00")
	}
	sweep := maintenance.AuditRetentionSweep{Pool: h.Maintenance, Logger: slog.New(&records{})}
	var wg sync.WaitGroup
	dropped := make([]int, 2)
	errs := make([]error, 2)
	start := make(chan struct{})
	for i := range 2 {
		wg.Go(func() {
			<-start
			dropped[i], errs[i] = sweep.Run(t.Context(), retentionClock)
		})
	}
	close(start)
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || dropped[0]+dropped[1] != 2 {
		t.Fatalf("concurrent sweeps dropped %v (%v), want the two months between them", dropped, errs)
	}
	if authorityCount(t, h, `SELECT count(*) FROM audit_retention_anchors WHERE scope_id=$1 AND anchor_seq=2 AND anchor_hash=sha256(($1::text||'2')::bytea)`, account) != 1 {
		t.Fatal("the chain is not anchored at its last dropped row")
	}
}

// The anchor is taken only from rows that hold together (review I1). The
// runtime may insert audit rows: a backdated copy of a kept row (a seq the
// chain already has, its hash copied) placed in an expiring partition would
// move the anchor past rows nobody verified, so the drop refuses it, changing
// nothing, and logs the chain at error level; so it does for a gap, a row
// that does not link, a failed chain, a head behind the anchor and rows of
// no account or family. Once the chain is settled the drop goes ahead.
func TestRetentionRefusesToAnchorOverAChainThatDoesNotHoldTogether(t *testing.T) {
	cases := []struct {
		name, reason string
		seed         func(t *testing.T, h *testkit.Harness, id string)
		settle       string // undoes the damage ($1 the chain; statements split on ";\n")
	}{
		{"backdated duplicate", "seq=3 reason=duplicate seq", func(t *testing.T, h *testkit.Harness, id string) {
			h.AuthorityExec(t, `INSERT INTO security_audit_events(scope,scope_id,seq,prev_hash,hash,kind,command_id,actor_account_id,at,payload)
				SELECT scope,scope_id,seq,prev_hash,hash,kind,command_id,actor_account_id,timestamptz '2025-01-20 12:00+00',payload
				  FROM security_audit_events WHERE scope='account' AND scope_id=$1 AND seq=3`, id)
		}, `DELETE FROM security_audit_events WHERE scope='account' AND scope_id=$1 AND seq=3 AND at < '2026-01-01'`},
		// The check reads the chain wherever its rows are: a copy of the
		// expiring seq 2 is refused in the default partition (no 2024
		// partition) as in a kept one.
		{"duplicate in the default partition", "seq=2 reason=duplicate seq", func(t *testing.T, h *testkit.Harness, id string) {
			h.AuthorityExec(t, `INSERT INTO security_audit_events(scope,scope_id,seq,prev_hash,hash,kind,command_id,actor_account_id,at,payload)
				SELECT scope,scope_id,seq,prev_hash,hash,kind,command_id,actor_account_id,timestamptz '2024-06-01 12:00+00',payload
				  FROM security_audit_events WHERE scope='account' AND scope_id=$1 AND seq=2`, id)
		}, `DELETE FROM security_audit_events WHERE scope='account' AND scope_id=$1 AND seq=2 AND at < '2025-01-01'`},
		{"duplicate in a kept partition", "seq=2 reason=duplicate seq", func(t *testing.T, h *testkit.Harness, id string) {
			h.AuthorityExec(t, `INSERT INTO security_audit_events(scope,scope_id,seq,prev_hash,hash,kind,command_id,actor_account_id,at,payload)
				SELECT scope,scope_id,seq,prev_hash,hash,kind,command_id,actor_account_id,timestamptz '2026-03-01 12:00+00',payload
				  FROM security_audit_events WHERE scope='account' AND scope_id=$1 AND seq=2`, id)
		}, `DELETE FROM security_audit_events WHERE scope='account' AND scope_id=$1 AND seq=2 AND at >= '2026-01-01'`},
		// Seq 2 moved out of the way and seq 3 relinked over the hole, so
		// the prev_hash links hold and only the seqs show the gap.
		{"gap", "seq=3 reason=missing row", func(t *testing.T, h *testkit.Harness, id string) {
			h.AuthorityExec(t, `UPDATE security_audit_events SET seq=7 WHERE scope='account' AND scope_id=$1 AND seq=2`, id)
			h.AuthorityExec(t, `UPDATE security_audit_events SET prev_hash=sha256(($1::text||'1')::bytea) WHERE scope='account' AND scope_id=$1 AND seq=3`, id)
		}, `UPDATE security_audit_events SET seq=2 WHERE scope='account' AND scope_id=$1 AND seq=7;
			UPDATE security_audit_events SET prev_hash=sha256(($1::text||'2')::bytea) WHERE scope='account' AND scope_id=$1 AND seq=3`},
		{"prev_hash", "seq=2 reason=prev_hash does not link", func(t *testing.T, h *testkit.Harness, id string) {
			h.AuthorityExec(t, `UPDATE security_audit_events SET prev_hash=sha256('forged'::bytea) WHERE scope='account' AND scope_id=$1 AND seq=2`, id)
		}, `UPDATE security_audit_events SET prev_hash=sha256(($1::text||'1')::bytea) WHERE scope='account' AND scope_id=$1 AND seq=2`},
		{"failed chain", "seq=2 reason=chain_failed", func(t *testing.T, h *testkit.Harness, id string) {
			h.AuthorityExec(t, `UPDATE accounts SET chain_failed=true WHERE id=$1`, id)
		}, `UPDATE accounts SET chain_failed=false WHERE id=$1`},
		{"head behind", "seq=2 reason=head behind the anchor", func(t *testing.T, h *testkit.Harness, id string) {
			h.AuthorityExec(t, `UPDATE accounts SET audit_seq=1 WHERE id=$1`, id)
		}, `UPDATE accounts SET audit_seq=4 WHERE id=$1`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := testkit.New(t)
			createMonth(t, h, "2025-01")
			id := newChain(t, h, "account", 4)
			// Seqs 1 and 2 expire with 2025-01; 3 and 4 are kept.
			for seq, at := range []string{"2025-01-10 12:00", "2025-01-11 12:00", "2026-02-10 12:00", "2026-02-11 12:00"} {
				auditRow(t, h, "account", id, seq+1, at)
			}
			c.seed(t, h, id)
			log := &records{}
			sweep := maintenance.AuditRetentionSweep{Pool: h.Maintenance, Logger: slog.New(log)}
			n, err := sweep.Run(t.Context(), retentionClock)
			var pgErr *pgconn.PgError
			if n != 0 || !errors.As(err, &pgErr) || pgErr.Code != "XX001" {
				t.Fatalf("dropped %d (%v), want the drop refused (XX001)", n, err)
			}
			if !slices.Contains(auditPartitions(t, h), "security_audit_events_2025_01") || authorityCount(t, h, `SELECT count(*) FROM audit_retention_anchors`) != 0 {
				t.Fatal("a refused drop left a change behind")
			}
			alerts := log.at(slog.LevelError)
			if len(alerts) != 1 || !strings.Contains(attr(alerts[0], "detail"), "id="+id) || !strings.Contains(attr(alerts[0], "detail"), c.reason) ||
				!strings.Contains(attr(alerts[0], "detail"), "partition=security_audit_events_2025_01") {
				t.Fatalf("logged %v, want one error naming the partition, the chain and %q", alerts, c.reason)
			}
			for _, statement := range strings.Split(c.settle, ";\n") {
				h.AuthorityExec(t, statement, id)
			}
			if n, err := sweep.Run(t.Context(), retentionClock); err != nil || n != 1 {
				t.Fatalf("once settled: dropped %d (%v)", n, err)
			}
			if authorityCount(t, h, `SELECT count(*) FROM audit_retention_anchors WHERE scope_id=$1 AND anchor_seq=2`, id) != 1 {
				t.Fatal("the settled chain is not anchored at its last dropped row")
			}
		})
	}
	// Rows of no account or family: nothing to verify them against.
	t.Run("no scope row", func(t *testing.T) {
		h := testkit.New(t)
		createMonth(t, h, "2025-01")
		orphan := newUUID(t, h)
		auditRow(t, h, "account", orphan, 1, "2025-01-10 12:00")
		_, err := maintenance.AuditRetentionSweep{Pool: h.Maintenance}.Run(t.Context(), retentionClock)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "XX001" || !strings.Contains(pgErr.Detail, "reason=no account or family row") {
			t.Fatalf("an orphan chain: %v, want the drop refused", err)
		}
	})
}

// newChain inserts an active account (scope "account") or a family and its
// owner account (scope "family") whose audit head is at head, its hash the
// one auditRow gives that seq, and returns the scope's ID.
func newChain(t *testing.T, h *testkit.Harness, scope string, head int) string {
	t.Helper()
	id := newUUID(t, h)
	owner := id
	if scope == "family" {
		owner = newUUID(t, h)
	}
	h.AuthorityExec(t, `INSERT INTO accounts(id,display_name,status,created_at,updated_at) VALUES ($1,'Retention','active',now(),now())`, owner)
	table := "accounts"
	if scope == "family" {
		table = "families"
		h.AuthorityExec(t, `INSERT INTO families(id,display_name,status,owner_account_id,created_at,updated_at) VALUES ($1,'Retention','active',$2,now(),now())`, id, owner)
	}
	h.AuthorityExec(t, `UPDATE `+table+` SET audit_seq=$2::bigint, audit_hash=sha256(($1::uuid::text||$2::bigint::text)::bytea) WHERE id=$1`, id, head)
	return id
}

// auditRow inserts seq of the chain at at (UTC): its hash is sha256(id||seq)
// and its prev_hash the previous seq's (zeros for seq 1), so a chain seeded
// seq by seq links.
func auditRow(t *testing.T, h *testkit.Harness, scope, id string, seq int, at string) {
	t.Helper()
	h.AuthorityExec(t, `INSERT INTO security_audit_events(scope,scope_id,seq,prev_hash,hash,kind,command_id,actor_account_id,at,payload)
		VALUES ($1::text::app.audit_scope,$2::uuid,$3::bigint,
			CASE WHEN $3::bigint = 1 THEN '\x0000000000000000000000000000000000000000000000000000000000000000'::bytea ELSE sha256(($2::uuid::text||($3::bigint-1)::text)::bytea) END,
			sha256(($2::uuid::text||$3::bigint::text)::bytea),'session_granted',gen_random_uuid(),gen_random_uuid(),($4::text||'+00')::timestamptz,'{}')`,
		scope, id, seq, at)
}

// createMonth attaches a monthly audit partition ("2025-01") as the table
// owner, as app.ensure_audit_partitions would have when it was current.
func createMonth(t *testing.T, h *testkit.Harness, month string) {
	t.Helper()
	start, err := time.Parse("2006-01", month)
	if err != nil {
		t.Fatal(err)
	}
	name := pgx.Identifier{"public", "security_audit_events_" + start.Format("2006_01")}.Sanitize()
	if _, err := h.Migrator.Exec(t.Context(), `CREATE TABLE `+name+` PARTITION OF public.security_audit_events FOR VALUES FROM ('`+
		start.Format(time.RFC3339)+`') TO ('`+start.AddDate(0, 1, 0).Format(time.RFC3339)+`')`); err != nil {
		t.Fatal(err)
	}
}

func newUUID(t *testing.T, h *testkit.Harness) string {
	t.Helper()
	var id string
	if err := h.Migrator.QueryRow(t.Context(), `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// auditPartitions names every partition of the audit table.
func auditPartitions(t *testing.T, h *testkit.Harness) []string {
	t.Helper()
	rows, err := h.Migrator.Query(t.Context(), `SELECT c.relname::text FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid
		WHERE i.inhparent='public.security_audit_events'::regclass ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return names
}

var rangeBound = regexp.MustCompile(`^FOR VALUES FROM \('([^']+)'\) TO \('([^']+)'\)$`)

// auditPartitionBounds maps each ranged audit partition (not the default) to
// its [from, to) bounds.
func auditPartitionBounds(t *testing.T, h *testkit.Harness) map[string][2]time.Time {
	t.Helper()
	tx, err := h.Migrator.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `SET LOCAL TimeZone = 'UTC'`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(t.Context(), `SELECT c.relname::text, pg_get_expr(c.relpartbound, c.oid) FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid
		WHERE i.inhparent='public.security_audit_events'::regclass`)
	if err != nil {
		t.Fatal(err)
	}
	type bound struct{ name, expr string }
	all, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (bound, error) {
		var b bound
		return b, row.Scan(&b.name, &b.expr)
	})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][2]time.Time{}
	for _, b := range all {
		m := rangeBound.FindStringSubmatch(b.expr)
		if m == nil {
			continue // the default partition
		}
		var from, to time.Time
		for i, dst := range []*time.Time{&from, &to} {
			parsed, err := time.Parse("2006-01-02 15:04:05-07", m[i+1])
			if err != nil {
				t.Fatalf("%s bound %q: %v", b.name, m[i+1], err)
			}
			*dst = parsed
		}
		out[b.name] = [2]time.Time{from, to}
	}
	return out
}
