package testpg_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCatalogSeesWhatMatters(t *testing.T) {
	f := testpg.New(t)
	testpg.ProductionShape(t, f)
	roles := testpg.Roles(t, f)
	base := testpg.SnapshotCatalog(t, f.Owner, roles)
	if len(base.Lines) == 0 {
		t.Fatal("an empty snapshot")
	}
	for _, want := range []string{"audit_monthly|", "database|||owner=<owner>|acl=", "schema|public|", "schema|app|", "manifest|", "compatibility|current_version="} {
		if !containsPrefix(base.Lines, want) {
			t.Fatalf("the snapshot has no %q line", want)
		}
	}
	for _, line := range base.Lines {
		for login := range roles {
			if strings.Contains(line, login) {
				t.Fatalf("a login role name survived normalization: %s", line)
			}
		}
	}
	// schema_migrations' rows are not part of the catalog; its shape and the
	// runtime's grant on it are.
	var migrationsTable bool
	for _, line := range base.Lines {
		if strings.HasPrefix(line, "relation|public|schema_migrations|") && strings.Contains(line, "baby_tracker_runtime=SELECT/<owner>") {
			migrationsTable = true
		}
	}
	if !migrationsTable || !containsPrefix(base.Lines, "column|public|schema_migrations.sha256|") || !containsPrefix(base.Lines, "index|public|schema_migrations_pkey|") {
		t.Fatal("the snapshot lacks schema_migrations' shape or the runtime's grant on it")
	}
	var runtimeLogin string
	for login, placeholder := range roles {
		if placeholder == "<runtime>" {
			runtimeLogin = login
		}
	}
	if runtimeLogin == "" {
		t.Fatalf("no runtime login in %v", roles)
	}

	// Each change must show as a diff line starting with want and, when
	// contains is set, holding it: the attribute that changed.
	type change struct{ sql, want, contains string }
	changes := []change{
		{`ALTER TABLE public.sessions NO FORCE ROW LEVEL SECURITY`, "relation|public|sessions|", "|force_rls=false|"},
		{`ALTER TABLE public.sessions DISABLE ROW LEVEL SECURITY`, "relation|public|sessions|", "|rls=false|"},
		{`ALTER POLICY authority_only ON public.sessions USING (true)`, "policy|public|sessions.authority_only|", "|using=true|"},
		{`ALTER POLICY authority_only ON public.sessions WITH CHECK (true)`, "policy|public|sessions.authority_only|", "|with_check=true"},
		// The same policy recreated RESTRICTIVE, every other attribute as it was.
		{`DO $$ DECLARE p record; BEGIN
			SELECT * INTO STRICT p FROM pg_policies WHERE schemaname = 'public' AND tablename = 'sessions' AND policyname = 'authority_only';
			EXECUTE format('DROP POLICY %I ON public.%I', p.policyname, p.tablename);
			EXECUTE format('CREATE POLICY %I ON public.%I AS RESTRICTIVE FOR %s TO %s %s %s', p.policyname, p.tablename, p.cmd,
				(SELECT string_agg(quote_ident(r), ', ') FROM unnest(p.roles) r),
				coalesce('USING (' || p.qual || ')', ''), coalesce('WITH CHECK (' || p.with_check || ')', ''));
		END $$`, "policy|public|sessions.authority_only|", "|permissive=false|"},
		// An index and a constraint redefined under the same name.
		{`DROP INDEX public.sessions_consumed_idx; CREATE INDEX sessions_consumed_idx ON public.sessions (consumed_at)`,
			"index|public|sessions_consumed_idx|", "def=CREATE INDEX sessions_consumed_idx ON public.sessions USING btree (consumed_at)|"},
		{`ALTER TABLE public.devices DROP CONSTRAINT devices_finite_times, ADD CONSTRAINT devices_finite_times CHECK (isfinite(created_at))`,
			"constraint|public|devices.devices_finite_times|", "def=CHECK (isfinite(created_at))|"},
		// A grant to the login role instead of its group shows as the
		// login's placeholder, not as the group.
		{`GRANT SELECT ON public.accounts TO ` + runtimeLogin, "relation|public|accounts|", "<runtime>=SELECT/<owner>"},
		{`REVOKE SELECT ON public.schema_migrations FROM baby_tracker_runtime`, "relation|public|schema_migrations|", ""},
		{`ALTER TABLE public.accounts DISABLE TRIGGER USER`, "trigger|public|accounts.", ""},
		{`GRANT CREATE ON SCHEMA public TO PUBLIC`, "schema|public|", ""},
		{`DO $$ BEGIN EXECUTE format('GRANT TEMPORARY ON DATABASE %I TO PUBLIC', current_database()); END $$`, "database|", "PUBLIC=TEMPORARY/<owner>"},
		{`REVOKE USAGE ON SCHEMA app FROM baby_tracker_runtime`, "schema|app|", ""},
		{`UPDATE schema_privilege_manifest SET privileges='SELECT'
			WHERE (schema_version, principal, object_kind, object_identity) = (SELECT schema_version, principal, object_kind, object_identity
				FROM schema_privilege_manifest WHERE schema_version=(SELECT current_version FROM schema_compatibility WHERE singleton) AND privileges<>'SELECT'
				ORDER BY principal, object_kind, object_identity LIMIT 1)`, "manifest|", ""},
		{`UPDATE schema_compatibility SET current_version=current_version+1`, "compatibility|current_version=", ""},
		{`DROP POLICY push_token_authority ON public.device_push_tokens`, "policy|public|device_push_tokens.push_token_authority|", ""},
		{`ALTER POLICY push_token_authority ON public.device_push_tokens TO PUBLIC`, "policy|public|device_push_tokens.push_token_authority|", ""},
		{`REVOKE UPDATE (care_record_count) ON public.families FROM baby_tracker_runtime`, "column|public|families.care_record_count|", ""},
		{`ALTER FUNCTION app.ensure_audit_partitions(timestamptz) RESET lock_timeout`, "function|app|app.ensure_audit_partitions(", ""},
		{`ALTER FUNCTION app.ensure_audit_partitions(timestamptz) SECURITY INVOKER`, "function|app|app.ensure_audit_partitions(", ""},
		{`GRANT EXECUTE ON FUNCTION app.ensure_audit_partitions(timestamptz) TO baby_tracker_runtime`, "function|app|app.ensure_audit_partitions(", ""},
		{`CREATE OR REPLACE FUNCTION app.current_account_id() RETURNS uuid LANGUAGE sql STABLE SET search_path = pg_catalog AS $$ SELECT NULL::uuid $$`, "function|app|app.current_account_id()|", ""},
		{`ALTER TABLE public.sessions RESET (autovacuum_vacuum_scale_factor)`, "relation|public|sessions|", ""},
		{`ALTER TABLE public.accounts ALTER COLUMN display_name DROP NOT NULL`, "column|public|accounts.display_name|", ""},
		{`CREATE INDEX catalog_test_index ON public.accounts (display_name)`, "index|public|catalog_test_index|", ""},
		{`ALTER TABLE public.accounts ADD CONSTRAINT catalog_test_check CHECK (display_name <> 'x')`, "constraint|public|accounts.catalog_test_check|", ""},
		{`ALTER TYPE app.audit_kind ADD VALUE 'catalog_test_value'`, "type|app|audit_kind|", ""},
	}
	// The newest monthly audit partition (the fixture creates them through
	// now() + 92 days): each change to one month alone must show.
	var month string
	if err := f.Owner.QueryRow(t.Context(), `SELECT max(relname) FROM pg_class
		WHERE relnamespace = 'public'::regnamespace AND relname ~ '^security_audit_events_[0-9]{4}_[0-9]{2}$'`).Scan(&month); err != nil {
		t.Fatal(err)
	}
	changes = append(changes,
		change{fmt.Sprintf(`ALTER TABLE public.security_audit_events DETACH PARTITION public.%s`, month), "audit_monthly|", ""},
		change{fmt.Sprintf(`ALTER TABLE public.%s DROP CONSTRAINT %s_at_range`, month, month), "constraint|public|security_audit_events_<month>.security_audit_events_<month>_at_range|", ""},
		change{fmt.Sprintf(`ALTER TABLE public.%s SET (autovacuum_vacuum_insert_scale_factor = 0.2)`, month), "relation|public|security_audit_events_<month>|", ""},
		change{fmt.Sprintf(`GRANT SELECT ON public.%s TO baby_tracker_runtime`, month), "relation|public|security_audit_events_<month>|", ""},
		// Only a superuser may disable the internal (foreign key) triggers.
		change{`ALTER TABLE public.memberships DISABLE TRIGGER ALL`, "internal_triggers|public|memberships|", ""},
	)
	superuser := testpg.Superuser(t, f)
	for _, c := range changes {
		t.Run(c.sql, func(t *testing.T) {
			pool := f.Owner
			if strings.HasSuffix(c.sql, "DISABLE TRIGGER ALL") {
				pool = superuser
			}
			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if _, err := tx.Exec(t.Context(), c.sql); err != nil {
				t.Fatalf("apply the change: %v", err)
			}
			diff := base.Diff(testpg.SnapshotCatalog(t, tx, roles))
			for _, line := range diff {
				if strings.HasPrefix(line[2:], c.want) && strings.Contains(line, c.contains) {
					return
				}
			}
			t.Fatalf("the snapshot did not see the change as a %q line holding %q; diff:\n%s", c.want, c.contains, strings.Join(diff, "\n"))
		})
	}

	if diff := base.Diff(testpg.SnapshotCatalog(t, f.Owner, roles)); len(diff) != 0 {
		t.Fatalf("a rolled-back change left a trace:\n%s", strings.Join(diff, "\n"))
	}
	other := testpg.New(t)
	testpg.ProductionShape(t, other)
	if diff := base.Diff(testpg.SnapshotCatalog(t, other.Owner, testpg.Roles(t, other))); len(diff) != 0 {
		t.Fatalf("two fresh installs differ:\n%s", strings.Join(diff, "\n"))
	}
}

func TestCatalogIgnoresTheSessionTimeZone(t *testing.T) {
	f := testpg.New(t)
	testpg.ProductionShape(t, f)
	roles := testpg.Roles(t, f)
	base := testpg.SnapshotCatalog(t, f.Owner, roles)

	// A session zone with daylight saving renders timestamptz constants (the
	// 2026 audit partition's bounds, the monthly ones) differently from UTC.
	const zone = "America/New_York"
	var rendered, utc string
	if err := f.Owner.QueryRow(t.Context(), `SELECT pg_get_expr(relpartbound, oid) FROM pg_class WHERE oid = 'public.security_audit_events_2026'::regclass`).Scan(&utc); err != nil {
		t.Fatal(err)
	}

	// As PGTZ would set it: every connection of the pool starts in the zone.
	config := f.Owner.Config().Copy()
	config.ConnConfig.RuntimeParams["timezone"] = zone
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.QueryRow(t.Context(), `SELECT pg_get_expr(relpartbound, oid) FROM pg_class WHERE oid = 'public.security_audit_events_2026'::regclass`).Scan(&rendered); err != nil {
		t.Fatal(err)
	}
	if rendered == utc {
		t.Fatalf("the zone does not change the rendering, so this test proves nothing: %s", rendered)
	}
	if diff := base.Diff(testpg.SnapshotCatalog(t, pool, roles)); len(diff) != 0 {
		t.Fatalf("a %s pool snapshots differently:\n%s", zone, strings.Join(diff, "\n"))
	}

	// Inside a caller's transaction with the zone set: the snapshot equals
	// UTC's and leaves the caller's zone as it was.
	tx, err := f.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `SET LOCAL TimeZone = '`+zone+`'`); err != nil {
		t.Fatal(err)
	}
	if diff := base.Diff(testpg.SnapshotCatalog(t, tx, roles)); len(diff) != 0 {
		t.Fatalf("a %s transaction snapshots differently:\n%s", zone, strings.Join(diff, "\n"))
	}
	var after string
	if err := tx.QueryRow(t.Context(), `SELECT current_setting('TimeZone')`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != zone {
		t.Fatalf("the snapshot changed the caller's TimeZone to %s", after)
	}
}

func TestCatalogDiffNamesBothSides(t *testing.T) {
	a := testpg.Catalog{Lines: []string{"a", "b", "c"}}
	b := testpg.Catalog{Lines: []string{"b", "c", "d"}}
	got := strings.Join(a.Diff(b), "\n")
	if got != "- a\n+ d" {
		t.Fatalf("Diff = %q", got)
	}
	if diff := a.Diff(a); len(diff) != 0 {
		t.Fatalf("a catalog differs from itself: %v", diff)
	}
}

func containsPrefix(lines []string, prefix string) bool {
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}
