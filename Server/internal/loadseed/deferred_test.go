//go:build identitytest

package loadseed

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/loadtest"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// schemaState is the database's catalog (testpg.SnapshotCatalog: every
// relation, column, constraint and index definition, trigger, policy and
// ACL) and what that snapshot leaves out but a restore could get wrong:
// each index's flags and the partitioned index it is attached to, and each
// constraint's index, referenced table and parent constraint.
func schemaState(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	lines := testpg.SnapshotCatalog(t, pool, nil).Lines
	rows, err := pool.Query(t.Context(), `SELECT format('index|%s|%s|primary=%s|unique=%s|immediate=%s|valid=%s|ready=%s|live=%s|parent=%s',
			t.relname, c.relname, i.indisprimary, i.indisunique, i.indimmediate, i.indisvalid, i.indisready, i.indislive,
			coalesce((SELECT p.relname FROM pg_inherits h JOIN pg_class p ON p.oid = h.inhparent WHERE h.inhrelid = c.oid), ''))
		FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid JOIN pg_class t ON t.oid = i.indrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname IN ('public', 'app')
		UNION ALL
		SELECT format('constraint|%s|%s|index=%s|references=%s|parent=%s|parent_table=%s', r.relname, k.conname, coalesce(ix.relname, ''),
			coalesce(fr.relname, ''), coalesce(pk.conname, ''), coalesce(pr.relname, ''))
		FROM pg_constraint k JOIN pg_class r ON r.oid = k.conrelid JOIN pg_namespace n ON n.oid = r.relnamespace
		LEFT JOIN pg_class ix ON ix.oid = k.conindid LEFT JOIN pg_class fr ON fr.oid = k.confrelid
		LEFT JOIN pg_constraint pk ON pk.oid = k.conparentid LEFT JOIN pg_class pr ON pr.oid = pk.conrelid
		WHERE n.nspname IN ('public', 'app')`)
	if err != nil {
		t.Fatal(err)
	}
	extra, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	lines = append(lines, extra...)
	slices.Sort(lines)
	return lines
}

// assertSameSchema fails with the lines that differ.
func assertSameSchema(t *testing.T, label string, before, after []string) {
	t.Helper()
	if slices.Equal(before, after) {
		return
	}
	diff := testpg.Catalog{Lines: before}.Diff(testpg.Catalog{Lines: after})
	if len(diff) > 20 {
		diff = diff[:20]
	}
	t.Fatalf("%s: the schema differs from before (- before, + after):\n%s", label, strings.Join(diff, "\n"))
}

// Each stage's deferred indexes and keys come back exactly as the
// migrations made them, and the drop really removed them; a restore
// refuses rows its unique index or key refuses, and a second restore
// finishes what a failed one left.
func TestDeferredSchemaIsRestoredExactly(t *testing.T) {
	env := newSeedEnvironment(t)
	before := schemaState(t, env.Superuser)
	for _, d := range []struct {
		name     string
		deferral deferral
		gone     []string
	}{
		{"care", careDeferral, []string{"care_entities_history_idx", "care_entities_p07_family_id_kind_id_idx", "care_receipts_pkey",
			"care_receipts_retention_idx", careFKName, "care_receipts_family_id_fkey", "import_mappings_shared_baby_fk"}},
		{"sessions", sessionsDeferral, []string{"sessions_pkey", "sessions_id_key", "sessions_refresh_digest_key", "sessions_consumed_idx",
			"sessions_account_id_fkey", "sessions_device_id_account_id_fkey", "refresh_receipts_child_id_fkey"}},
	} {
		deferred, err := recordDeferral(t.Context(), env.Superuser, d.deferral)
		if err != nil {
			t.Fatal(err)
		}
		if err := deferred.drop(t.Context(), env.Superuser); err != nil {
			t.Fatal(err)
		}
		dropped := strings.Join(schemaState(t, env.Superuser), "\n")
		for _, name := range d.gone {
			if strings.Contains(dropped, "|"+name+"|") {
				t.Fatalf("%s: %s is still there after the drop", d.name, name)
			}
		}
		if err := deferred.restore(t.Context(), env.Superuser, restoreWorkers); err != nil {
			t.Fatal(err)
		}
		assertSameSchema(t, d.name, before, schemaState(t, env.Superuser))
	}

	// Two sessions with one access digest: the restored primary key refuses
	// them, and once one is gone a second restore completes the schema.
	s, err := compose(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.provisionAll(t.Context(), Small(1, 1, time.Now().UTC().Truncate(time.Second))); err != nil {
		t.Fatal(err)
	}
	deferred, err := recordDeferral(t.Context(), env.Superuser, sessionsDeferral)
	if err != nil {
		t.Fatal(err)
	}
	if err := deferred.drop(t.Context(), env.Superuser); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Superuser.Exec(t.Context(), `INSERT INTO sessions SELECT * FROM sessions ORDER BY created_at LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	err = deferred.restore(t.Context(), env.Superuser, restoreWorkers)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("restoring over a duplicate session: %v, want a unique violation", err)
	}
	if _, err := env.Superuser.Exec(t.Context(), `DELETE FROM sessions WHERE ctid = (SELECT max(ctid) FROM sessions)`); err != nil {
		t.Fatal(err)
	}
	if err := deferred.restore(t.Context(), env.Superuser, restoreWorkers); err != nil {
		t.Fatal(err)
	}
	assertSameSchema(t, "sessions after a failed restore", before, schemaState(t, env.Superuser))
}

// A backlog that fails while it copies leaves the sessions schema whole.
func TestFailedBacklogRestoresTheSessionsSchema(t *testing.T) {
	env := newSeedEnvironment(t)
	before := schemaState(t, env.Superuser)
	spec := Small(2, 1, time.Now().UTC().Truncate(time.Second))
	spec.BacklogLoad = 500 // 576,000 rows: long enough to cancel mid-COPY
	s, err := compose(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	families, err := s.provisionAll(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	var devices []*loadtest.DeviceState
	for _, f := range families {
		devices = append(devices, f.Devices...)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := loadBacklog(ctx, env, s, spec, devices)
		done <- err
	}()
	deadline := time.Now().Add(time.Minute)
	for {
		var copying bool
		if err := env.Superuser.QueryRow(t.Context(), `SELECT EXISTS (SELECT FROM pg_stat_progress_copy
			WHERE relid = 'public.sessions'::regclass AND tuples_processed > 0)`).Scan(&copying); err != nil {
			t.Fatal(err)
		}
		if copying {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the backlog never started copying")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a cancelled backlog succeeded")
	}
	var consumed int
	if err := env.Superuser.QueryRow(t.Context(), `SELECT count(*) FROM sessions WHERE status = 'consumed' AND child_id IS NOT NULL`).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if consumed >= spec.BacklogRows(len(devices)) {
		t.Fatalf("the cancelled backlog wrote all %d rows", consumed)
	}
	assertSameSchema(t, "after a failed backlog", before, schemaState(t, env.Superuser))
}
