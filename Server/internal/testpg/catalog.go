package testpg

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Catalog is a normalized, sorted description of the public and app schemas:
//   - the database's owner and ACL;
//   - schema owners and ACLs;
//   - relations: kind, reloptions, ACL, row security, forced row security, partition bounds;
//   - columns: type, not null, default, identity;
//   - constraints (pg_get_constraintdef) and indexes (pg_get_indexdef);
//   - policies: command, roles, USING, WITH CHECK;
//   - triggers: pg_get_triggerdef, tgenabled;
//   - functions: regprocedure, security definer, proconfig, ACL, md5 of the source;
//   - types and enum labels;
//   - the schema_privilege_manifest rows of the current version, and schema_compatibility.current_version.
//
// Monthly audit partitions (security_audit_events_YYYY_MM) are collapsed into
// one line naming the first month and the count through now()+92 days, so
// the snapshot does not depend on the day it runs (auditMonthlyLine). The
// rows of schema_migrations are not part of it (the squash changes the
// history, not the catalog); its shape and grants are. Login role names that
// differ between fixtures are replaced through roles.
//
// Besides the list above it carries column ACLs, sequences, views, internal
// (foreign key) trigger states, default ACLs, rules and extensions in the two
// schemas: each of them changes what a principal may do.
//
// The snapshot is read with TimeZone UTC (SET LOCAL in a transaction or
// savepoint of its own, rolled back after), so timestamptz constants in
// partition bounds and CHECKs render the same whatever the session's zone.
type Catalog struct{ Lines []string }

// Querier is what SnapshotCatalog reads through: a pool or a transaction (to
// see a state that is never committed). Begin opens the snapshot's own
// transaction, or a savepoint inside the caller's.
type Querier interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// catalogSchemas are the schemas a snapshot describes.
const catalogSchemas = `('public', 'app')`

// aclItems renders acl (defaulting to acldefault(kind, owner) when kind is
// not empty) as a text[] of grantee=privilege[*]/grantor, PUBLIC for grantee 0.
// aclexplode refuses an empty array, and returns nothing for NULL.
func aclItems(acl, kind, owner string) string {
	source := acl
	if kind != "" {
		source = fmt.Sprintf(`coalesce(%s, acldefault(%s::"char", %s))`, acl, kind, owner)
	}
	source = fmt.Sprintf(`nullif(%s, '{}'::aclitem[])`, source)
	return `ARRAY(SELECT format('%s=%s%s/%s', CASE WHEN acl_item.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(acl_item.grantee) END,
		acl_item.privilege_type, CASE WHEN acl_item.is_grantable THEN '*' ELSE '' END, pg_get_userbyid(acl_item.grantor)) FROM aclexplode(` + source + `) acl_item)`
}

// catalogQueries each return (kind, schema, name, attributes, set label, set):
// the set (an ACL or a role list) is sorted after its role names are mapped.
var catalogQueries = []string{
	// The database's own owner and ACL (CONNECT, TEMPORARY, CREATE): a
	// migration may revoke from PUBLIC there (000002 revokes TEMPORARY), and a
	// schema-only dump does not carry it.
	`SELECT 'database', '', '', 'owner=' || pg_get_userbyid(d.datdba), 'acl', ` + aclItems("d.datacl", "'d'", "d.datdba") + `
	FROM pg_database d WHERE d.datname = current_database()`,

	// Schemas.
	`SELECT 'schema', n.nspname::text, '', 'owner=' || pg_get_userbyid(n.nspowner), 'acl', ` + aclItems("n.nspacl", "'n'", "n.nspowner") + `
	FROM pg_namespace n WHERE n.nspname IN ` + catalogSchemas,

	// Relations (tables, partitioned tables, views, sequences, composite types).
	`SELECT 'relation', n.nspname::text, c.relname::text,
		concat_ws('|', 'kind=' || c.relkind::text, 'owner=' || pg_get_userbyid(c.relowner), 'persistence=' || c.relpersistence::text,
			'options=' || coalesce((SELECT string_agg(o, ',' ORDER BY o COLLATE "C") FROM unnest(c.reloptions) o), ''),
			'rls=' || c.relrowsecurity, 'force_rls=' || c.relforcerowsecurity,
			'inherits=' || coalesce((SELECT string_agg(pn.nspname || '.' || p.relname, ',' ORDER BY pn.nspname COLLATE "C", p.relname COLLATE "C")
				FROM pg_inherits i JOIN pg_class p ON p.oid = i.inhparent JOIN pg_namespace pn ON pn.oid = p.relnamespace WHERE i.inhrelid = c.oid), ''),
			'bound=' || coalesce(pg_get_expr(c.relpartbound, c.oid), ''),
			'partition_key=' || coalesce(CASE WHEN c.relkind = 'p' THEN pg_get_partkeydef(c.oid) END, ''),
			'view=' || coalesce(CASE WHEN c.relkind IN ('v', 'm') THEN pg_get_viewdef(c.oid) END, ''),
			'sequence=' || coalesce((SELECT concat_ws(',', format_type(s.seqtypid, NULL), s.seqstart, s.seqincrement, s.seqmin, s.seqmax, s.seqcache, s.seqcycle)
				FROM pg_sequence s WHERE s.seqrelid = c.oid), '')),
		'acl', ` + aclItems("c.relacl", `CASE WHEN c.relkind = 'S' THEN 's' ELSE 'r' END`, "c.relowner") + `
	FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE n.nspname IN ` + catalogSchemas + ` AND c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f', 'c')`,

	// Columns, by their position among the live ones (a dropped column leaves
	// an attnum gap that a dump-built baseline does not have).
	`SELECT 'column', n.nspname::text, c.relname || '.' || a.attname,
		concat_ws('|', 'position=' || row_number() OVER (PARTITION BY c.oid ORDER BY a.attnum), 'type=' || format_type(a.atttypid, a.atttypmod),
			'not_null=' || a.attnotnull, 'default=' || coalesce(pg_get_expr(d.adbin, d.adrelid), ''),
			'identity=' || a.attidentity::text, 'generated=' || a.attgenerated::text,
			'collation=' || coalesce(CASE WHEN a.attcollation <> t.typcollation THEN a.attcollation::regcollation::text END, '')),
		'acl', ` + aclItems("a.attacl", "", "") + `
	FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
	JOIN pg_type t ON t.oid = a.atttypid LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
	WHERE n.nspname IN ` + catalogSchemas + ` AND c.relkind IN ('r', 'p', 'v', 'm', 'f', 'c') AND a.attnum > 0 AND NOT a.attisdropped`,

	// Table and domain constraints.
	`SELECT 'constraint', n.nspname::text, coalesce(c.relname, t.typname, '') || '.' || co.conname,
		concat_ws('|', 'type=' || co.contype::text, 'def=' || pg_get_constraintdef(co.oid), 'deferrable=' || co.condeferrable,
			'deferred=' || co.condeferred, 'validated=' || co.convalidated), '', '{}'::text[]
	FROM pg_constraint co JOIN pg_namespace n ON n.oid = co.connamespace
	LEFT JOIN pg_class c ON c.oid = co.conrelid LEFT JOIN pg_type t ON t.oid = co.contypid
	WHERE n.nspname IN ` + catalogSchemas,

	// Indexes.
	`SELECT 'index', n.nspname::text, c.relname::text,
		concat_ws('|', 'def=' || pg_get_indexdef(i.indexrelid), 'valid=' || i.indisvalid, 'clustered=' || i.indisclustered,
			'replica_identity=' || i.indisreplident), '', '{}'::text[]
	FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE n.nspname IN ` + catalogSchemas,

	// Row-level security policies.
	`SELECT 'policy', n.nspname::text, c.relname || '.' || p.polname,
		concat_ws('|', 'command=' || p.polcmd::text, 'permissive=' || p.polpermissive,
			'using=' || coalesce(pg_get_expr(p.polqual, p.polrelid), ''), 'with_check=' || coalesce(pg_get_expr(p.polwithcheck, p.polrelid), '')),
		'roles', ARRAY(SELECT CASE WHEN r = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(r) END FROM unnest(p.polroles) r)
	FROM pg_policy p JOIN pg_class c ON c.oid = p.polrelid JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE n.nspname IN ` + catalogSchemas,

	// User triggers.
	`SELECT 'trigger', n.nspname::text, c.relname || '.' || tg.tgname,
		concat_ws('|', 'def=' || pg_get_triggerdef(tg.oid), 'enabled=' || tg.tgenabled::text), '', '{}'::text[]
	FROM pg_trigger tg JOIN pg_class c ON c.oid = tg.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE n.nspname IN ` + catalogSchemas + ` AND NOT tg.tgisinternal`,

	// Internal (foreign key) triggers carry OIDs in their names: one line per
	// table naming each by its constraint, function, type and enabled state,
	// so DISABLE TRIGGER ALL shows.
	`SELECT 'internal_triggers', n.nspname::text, c.relname::text, '', 'triggers',
		array_agg(format('%s/%s/%s/%s', coalesce(co.conname, '-'), tg.tgfoid::regproc, tg.tgtype, tg.tgenabled))
	FROM pg_trigger tg JOIN pg_class c ON c.oid = tg.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
	LEFT JOIN pg_constraint co ON co.oid = tg.tgconstraint
	WHERE n.nspname IN ` + catalogSchemas + ` AND tg.tgisinternal
	GROUP BY n.nspname, c.relname`,

	// Functions and procedures.
	`SELECT 'function', n.nspname::text, p.oid::regprocedure::text,
		concat_ws('|', 'kind=' || p.prokind::text, 'arguments=' || pg_get_function_arguments(p.oid), 'returns=' || coalesce(pg_get_function_result(p.oid), ''),
			'language=' || l.lanname, 'owner=' || pg_get_userbyid(p.proowner), 'security_definer=' || p.prosecdef,
			'volatility=' || p.provolatile::text, 'strict=' || p.proisstrict, 'leakproof=' || p.proleakproof, 'parallel=' || p.proparallel::text,
			'config=' || coalesce(array_to_string(ARRAY(SELECT x FROM unnest(p.proconfig) x ORDER BY x COLLATE "C"), ','), ''),
			'source_md5=' || md5(p.prosrc)),
		'acl', ` + aclItems("p.proacl", "'f'", "p.proowner") + `
	FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace JOIN pg_language l ON l.oid = p.prolang
	WHERE n.nspname IN ` + catalogSchemas,

	// Enums, domains, ranges and standalone composite types (not the row
	// types of tables, nor implicit arrays).
	`SELECT 'type', n.nspname::text, t.typname::text,
		concat_ws('|', 'type=' || t.typtype::text, 'owner=' || pg_get_userbyid(t.typowner),
			'labels=' || coalesce((SELECT string_agg(e.enumlabel, ',' ORDER BY e.enumsortorder) FROM pg_enum e WHERE e.enumtypid = t.oid), ''),
			'base=' || CASE WHEN t.typtype = 'd' THEN format_type(t.typbasetype, t.typtypmod) ELSE '' END,
			'not_null=' || t.typnotnull, 'default=' || coalesce(t.typdefault, ''),
			'range_subtype=' || coalesce((SELECT format_type(r.rngsubtype, NULL) FROM pg_range r WHERE r.rngtypid = t.oid), '')),
		'acl', ` + aclItems("t.typacl", "'T'", "t.typowner") + `
	FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
	WHERE n.nspname IN ` + catalogSchemas + `
	  AND (t.typtype IN ('e', 'd', 'r', 'm') OR (t.typtype = 'c' AND EXISTS (SELECT FROM pg_class c WHERE c.oid = t.typrelid AND c.relkind = 'c')))`,

	// Default privileges set for a schema.
	`SELECT 'default_acl', n.nspname::text, pg_get_userbyid(d.defaclrole) || '.' || d.defaclobjtype::text, '', 'acl', ` + aclItems("d.defaclacl", "", "") + `
	FROM pg_default_acl d JOIN pg_namespace n ON n.oid = d.defaclnamespace
	WHERE n.nspname IN ` + catalogSchemas,

	// Rewrite rules other than views' own.
	`SELECT 'rule', r.schemaname::text, r.tablename || '.' || r.rulename, 'def=' || r.definition, '', '{}'::text[]
	FROM pg_rules r WHERE r.schemaname IN ` + catalogSchemas,

	// Extensions installed into either schema.
	`SELECT 'extension', n.nspname::text, e.extname::text, 'version=' || e.extversion || '|owner=' || pg_get_userbyid(e.extowner), '', '{}'::text[]
	FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace
	WHERE n.nspname IN ` + catalogSchemas,
}

// monthlyAuditPartition names a monthly audit partition.
var monthlyAuditPartition = regexp.MustCompile(`^security_audit_events_[0-9]{4}_(0[1-9]|1[0-2])$`)

// SnapshotCatalog reads q's catalog (see Catalog). roles maps login role names
// to the placeholders that make two fixtures comparable (Roles). It reads in a
// transaction of its own (a savepoint when q is a transaction) with TimeZone
// UTC, and rolls it back, so the caller's session is left as it was.
func SnapshotCatalog(t testing.TB, q Querier, roles map[string]string) Catalog {
	t.Helper()
	ctx := context.Background()
	replace := roleReplacer(roles)

	snapshot, err := q.Begin(ctx)
	if err != nil {
		t.Fatalf("snapshot catalog: begin: %v", err)
	}
	defer snapshot.Rollback(context.Background())
	if _, err := snapshot.Exec(ctx, `SET LOCAL TimeZone = 'UTC'`); err != nil {
		t.Fatalf("snapshot catalog: time zone: %v", err)
	}
	q = snapshot

	// Monthly audit partitions (attached ones; a detached month is an ordinary
	// table): each one's name and range literals, as pg_get_expr and
	// pg_get_constraintdef render them. The months are UTC months, as
	// app.ensure_audit_partitions builds them.
	type month struct{ name, suffix, from, to string }
	rows, err := q.Query(ctx, `SELECT c.relname::text, substr(c.relname, 23),
			quote_literal(b.month_start::text), quote_literal(((b.month_start AT TIME ZONE 'UTC' + interval '1 month') AT TIME ZONE 'UTC')::text)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		CROSS JOIN LATERAL (SELECT make_timestamptz(substr(c.relname, 23, 4)::int, substr(c.relname, 28, 2)::int, 1, 0, 0, 0, 'UTC') AS month_start) b
		WHERE n.nspname = 'public' AND c.relname ~ $1
		  AND EXISTS (SELECT FROM pg_inherits i WHERE i.inhrelid = c.oid AND i.inhparent = to_regclass('public.security_audit_events'))
		ORDER BY 1`, monthlyAuditPartition.String())
	if err != nil {
		t.Fatalf("snapshot catalog: monthly audit partitions: %v", err)
	}
	months, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (month, error) {
		var m month
		err := row.Scan(&m.name, &m.suffix, &m.from, &m.to)
		return m, err
	})
	if err != nil {
		t.Fatalf("snapshot catalog: monthly audit partitions: %v", err)
	}
	var now time.Time
	if err := q.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		t.Fatalf("snapshot catalog: now: %v", err)
	}

	var plain []string
	// A line naming a monthly partition, with the month's name and range
	// replaced, and the months it appeared for.
	monthly := map[string]map[string]bool{}
	for _, query := range catalogQueries {
		rows, err := q.Query(ctx, query)
		if err != nil {
			t.Fatalf("snapshot catalog: %v\n%s", err, query)
		}
		for rows.Next() {
			var kind, schema, name, attributes, label string
			var set []string
			if err := rows.Scan(&kind, &schema, &name, &attributes, &label, &set); err != nil {
				rows.Close()
				t.Fatalf("snapshot catalog: %v\n%s", err, query)
			}
			line := kind + "|" + schema + "|" + name + "|" + attributes
			if label != "" {
				mapped := make([]string, len(set))
				for i, item := range set {
					mapped[i] = replace.Replace(item)
				}
				sort.Strings(mapped)
				line += "|" + label + "=" + strings.Join(mapped, ",")
			}
			// One object, one line: view and policy bodies span several.
			line = strings.ReplaceAll(replace.Replace(line), "\n", `\n`)
			found := false
			for _, m := range months {
				if !strings.Contains(line, m.name) {
					continue
				}
				normalized := strings.NewReplacer(m.name, "security_audit_events_<month>", m.from, "'<from>'", m.to, "'<to>'").Replace(line)
				if monthly[normalized] == nil {
					monthly[normalized] = map[string]bool{}
				}
				monthly[normalized][m.suffix] = true
				found = true
				break
			}
			if !found {
				plain = append(plain, line)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("snapshot catalog: %v\n%s", err, query)
		}
	}
	// A line every month shares appears once; one only some months have names them.
	for line, seen := range monthly {
		if len(seen) == len(months) {
			plain = append(plain, line)
			continue
		}
		var only []string
		for suffix := range seen {
			only = append(only, suffix)
		}
		sort.Strings(only)
		plain = append(plain, line+"|only="+strings.Join(only, ","))
	}
	suffixes := make([]string, len(months))
	for i, m := range months {
		suffixes[i] = m.suffix
	}
	plain = append(plain, auditMonthlyLine(suffixes, now))

	plain = append(plain, manifestLines(t, q, replace)...)
	sort.Strings(plain)
	return Catalog{Lines: plain}
}

// firstAuditMonth is the first monthly audit partition app.ensure_audit_partitions
// creates: 2027-01, or the current UTC month once that is later.
const firstAuditMonth = "2027_01"

// auditMonthlyLine collapses the monthly partitions (YYYY_MM suffixes, sorted)
// into one line that does not change with the calendar. A fresh install at now
// has exactly the months from max(2027_01, now's UTC month) through the UTC
// month of now + 92 days (app.ensure_audit_partitions). The line says
// first=expected and through_now_plus_92d=complete for exactly that set; only
// a different first month, a gap or an extra month is spelled out.
func auditMonthlyLine(suffixes []string, now time.Time) string {
	const prefix = "audit_monthly|public|security_audit_events|"
	utc := now.UTC()
	expectedFirst := max(firstAuditMonth, utc.Format("2006_01"))
	horizon := utc.Add(92 * 24 * time.Hour).Format("2006_01")
	want := map[string]bool{}
	for month := expectedFirst; month <= horizon; month = nextMonth(month) {
		want[month] = true
	}
	have := map[string]bool{}
	var extra []string
	for _, suffix := range suffixes {
		have[suffix] = true
		if !want[suffix] {
			extra = append(extra, suffix)
		}
	}
	var missing []string
	for month := range want {
		if !have[month] {
			missing = append(missing, month)
		}
	}
	sort.Strings(missing)
	first := "none"
	if len(suffixes) > 0 {
		first = suffixes[0]
	}
	if (len(suffixes) == 0 && len(want) == 0) || (len(suffixes) > 0 && suffixes[0] == expectedFirst) {
		first = "expected"
	}
	state := "complete"
	if len(missing) > 0 || len(extra) > 0 {
		state = "missing=" + strings.Join(missing, ",") + ";beyond=" + strings.Join(extra, ",")
	}
	return prefix + "first=" + first + "|through_now_plus_92d=" + state
}

// nextMonth is the YYYY_MM after month.
func nextMonth(month string) string {
	var year, m int
	if _, err := fmt.Sscanf(month, "%d_%d", &year, &m); err != nil {
		return "9999_99"
	}
	if m == 12 {
		return fmt.Sprintf("%04d_%02d", year+1, 1)
	}
	return fmt.Sprintf("%04d_%02d", year, m+1)
}

// manifestLines are the current version's privilege manifest rows and the
// compatibility version, or one line saying the tables are absent.
func manifestLines(t testing.TB, q Querier, replace *strings.Replacer) []string {
	t.Helper()
	ctx := context.Background()
	var present bool
	if err := q.QueryRow(ctx, `SELECT to_regclass('public.schema_privilege_manifest') IS NOT NULL
		AND to_regclass('public.schema_compatibility') IS NOT NULL`).Scan(&present); err != nil {
		t.Fatalf("snapshot catalog: manifest: %v", err)
	}
	if !present {
		return []string{"compatibility|absent"}
	}
	var current int64
	if err := q.QueryRow(ctx, `SELECT current_version FROM public.schema_compatibility WHERE singleton`).Scan(&current); err != nil {
		t.Fatalf("snapshot catalog: compatibility: %v", err)
	}
	lines := []string{fmt.Sprintf("compatibility|current_version=%d", current)}
	rows, err := q.Query(ctx, `SELECT format('manifest|%s|%s|%s|privileges=%s', principal, object_kind, object_identity, privileges)
		FROM public.schema_privilege_manifest WHERE schema_version = $1`, current)
	if err != nil {
		t.Fatalf("snapshot catalog: manifest: %v", err)
	}
	manifest, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("snapshot catalog: manifest: %v", err)
	}
	for _, line := range manifest {
		lines = append(lines, replace.Replace(line))
	}
	return lines
}

// roleReplacer maps each login role name to its placeholder, longest names
// first so no name replaces a prefix of another.
func roleReplacer(roles map[string]string) *strings.Replacer {
	names := make([]string, 0, len(roles))
	for name := range roles {
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		if len(names[i]) != len(names[j]) {
			return len(names[i]) > len(names[j])
		}
		return names[i] < names[j]
	})
	pairs := make([]string, 0, 2*len(names))
	for _, name := range names {
		pairs = append(pairs, name, roles[name])
	}
	return strings.NewReplacer(pairs...)
}

// Roles maps f's per-test login roles to "<owner>", "<runtime>", "<maintenance>".
func Roles(t testing.TB, f *Fixture) map[string]string {
	t.Helper()
	roles := map[string]string{}
	for _, pool := range []struct {
		pool        *pgxpool.Pool
		placeholder string
	}{{f.Owner, "<owner>"}, {f.Runtime, "<runtime>"}, {f.Maintenance, "<maintenance>"}} {
		if pool.pool == nil {
			continue
		}
		var name string
		if err := pool.pool.QueryRow(context.Background(), `SELECT current_user`).Scan(&name); err != nil {
			t.Fatalf("read %s login role: %v", pool.placeholder, err)
		}
		roles[name] = pool.placeholder
	}
	return roles
}

// Diff lists the lines only in c ("- …") or only in other ("+ …"), in line order.
func (c Catalog) Diff(other Catalog) []string {
	left := append([]string(nil), c.Lines...)
	right := append([]string(nil), other.Lines...)
	sort.Strings(left)
	sort.Strings(right)
	var diff []string
	i, j := 0, 0
	for i < len(left) || j < len(right) {
		switch {
		case j == len(right) || (i < len(left) && left[i] < right[j]):
			diff = append(diff, "- "+left[i])
			i++
		case i == len(left) || right[j] < left[i]:
			diff = append(diff, "+ "+right[j])
			j++
		default:
			i++
			j++
		}
	}
	return diff
}

// ProductionShape makes f's public schema look like a deployed one
// (deploy/postgres/init-users.sh:193): owned by the migrating login, no CREATE for PUBLIC.
// It runs as the cluster's superuser on a connection it closes at once.
func ProductionShape(t testing.TB, f *Fixture) {
	t.Helper()
	ctx := context.Background()
	var owner string
	if err := f.Owner.QueryRow(ctx, `SELECT current_user`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.ConnectConfig(ctx, superuserConfig(t, f).ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `ALTER SCHEMA public OWNER TO `+pgx.Identifier{owner}.Sanitize()+`; REVOKE CREATE ON SCHEMA public FROM PUBLIC`); err != nil {
		t.Fatalf("production shape: %v", err)
	}
}

// Superuser is a superuser pool on f's database (the test-db.sh DSN with f's database name), closed at cleanup.
func Superuser(t testing.TB, f *Fixture) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.NewWithConfig(context.Background(), superuserConfig(t, f))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func superuserConfig(t testing.TB, f *Fixture) *pgxpool.Config {
	t.Helper()
	var database string
	if err := f.Owner.QueryRow(context.Background(), `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(RequireDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + database
	config, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 2
	return config
}
