package database_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"babytracker/server/internal/testpg"
)

// 000037 (Task 21 C1) makes the three row-security context helpers
// inlinable: no SET clause, every name in their bodies schema-qualified.
// These tests pin that they answer exactly as before, that the planner
// inlines them, and that the caller's search_path still cannot reach them.

var contextHelpers = []string{"app.authority_context()", "app.current_account_id()", "app.current_family_id()"}

// contextShape is one setting of the three context variables; nil leaves
// the variable unset in a fresh session (current_setting(…, true) is NULL).
type contextShape struct{ family, account, authority *string }

func (s contextShape) String() string {
	show := func(v *string) string {
		if v == nil {
			return "unset"
		}
		return fmt.Sprintf("%q", *v)
	}
	return fmt.Sprintf("family=%s account=%s authority=%s", show(s.family), show(s.account), show(s.authority))
}

func contextShapes(ids authorityFixture) []contextShape {
	value := func(v string) *string { return &v }
	families := []*string{nil, value(""), value(ids.familyA), value(strings.ToUpper(ids.familyB)), value("not-a-uuid")}
	accounts := []*string{nil, value(""), value(ids.accountA), value("not-a-uuid")}
	authorities := []*string{nil, value(""), value("1"), value("0"), value("true")}
	var shapes []contextShape
	for _, family := range families {
		for _, account := range accounts {
			for _, authority := range authorities {
				shapes = append(shapes, contextShape{family, account, authority})
			}
		}
	}
	return shapes
}

// contextProbes read the helpers directly and through the policies built
// from them (each statement alone, so one's error does not hide another's).
var contextProbes = []string{
	`SELECT app.authority_context()::text`,
	`SELECT app.current_family_id()::text`,
	`SELECT app.current_account_id()::text`,
	`SELECT string_agg(id::text, ',' ORDER BY id) FROM families`,
	`SELECT string_agg(id::text, ',' ORDER BY id) FROM accounts`,
	`SELECT string_agg(family_id::text || '/' || account_id::text, ',' ORDER BY family_id) FROM memberships`,
	`SELECT string_agg(family_id::text, ',' ORDER BY family_id) FROM family_slots`,
	`SELECT string_agg(id::text, ',' ORDER BY id) FROM devices`,
	`SELECT string_agg(id::text, ',' ORDER BY id) FROM invitations`,
	`SELECT count(*)::text FROM sessions`,
}

// contextAnswers runs every probe under every shape as the runtime, each
// shape in a fresh session, and returns the answers (a value, NULL, or the
// SQLSTATE of the error).
func contextAnswers(t *testing.T, f *testpg.Fixture, shapes []contextShape) map[string][]string {
	t.Helper()
	ctx := t.Context()
	answers := map[string][]string{}
	for _, shape := range shapes {
		conn, err := pgx.ConnectConfig(ctx, f.Runtime.Config().ConnConfig)
		if err != nil {
			t.Fatal(err)
		}
		for name, v := range map[string]*string{"app.family_id": shape.family, "app.account_id": shape.account, "app.authority": shape.authority} {
			if v != nil {
				if _, err := conn.Exec(ctx, `SELECT set_config($1, $2, false)`, name, *v); err != nil {
					t.Fatal(err)
				}
			}
		}
		var got []string
		for _, probe := range contextProbes {
			var answer *string
			err := conn.QueryRow(ctx, probe).Scan(&answer)
			var pgErr *pgconn.PgError
			switch {
			case errors.As(err, &pgErr):
				got = append(got, "error "+pgErr.Code)
			case err != nil:
				t.Fatalf("%s %s: %v", shape, probe, err)
			case answer == nil:
				got = append(got, "NULL")
			default:
				got = append(got, *answer)
			}
		}
		_ = conn.Close(ctx)
		answers[shape.String()] = got
	}
	return answers
}

type helperCatalog struct {
	owner, acl, config, language string
	volatility                   string
	definer, strict, leakproof   bool
	body                         string
}

func readHelperCatalog(t *testing.T, f *testpg.Fixture) map[string]helperCatalog {
	t.Helper()
	out := map[string]helperCatalog{}
	for _, signature := range contextHelpers {
		var c helperCatalog
		if err := f.Owner.QueryRow(t.Context(), `SELECT pg_get_userbyid(p.proowner), coalesce(p.proacl::text, ''), coalesce(array_to_string(p.proconfig, ','), ''),
				l.lanname, p.provolatile::text, p.prosecdef, p.proisstrict, p.proleakproof, p.prosrc
			FROM pg_proc p JOIN pg_language l ON l.oid=p.prolang WHERE p.oid=$1::regprocedure`, signature).
			Scan(&c.owner, &c.acl, &c.config, &c.language, &c.volatility, &c.definer, &c.strict, &c.leakproof, &c.body); err != nil {
			t.Fatal(err)
		}
		out[signature] = c
	}
	return out
}

// Every RLS-relevant setting shape (each variable unset, empty, valid, an
// uppercase uuid, invalid uuid text; authority unset, empty, 1, 0, true)
// gets the same answer from the helpers and from the policies built on them
// at 37 as at 36, errors included. The helpers keep their owner, grants,
// language and volatility; only the SET clause and the body change.
func TestContextHelpersAnswerAt37AsAt36(t *testing.T) {
	f := testpg.NewThrough(t, 36)
	ids := seedAuthorityFamilies(t, f)
	shapes := contextShapes(ids)
	catalog36 := readHelperCatalog(t, f)
	at36 := contextAnswers(t, f, shapes)
	if err := f.MigrateThrough(t.Context(), 37); err != nil {
		t.Fatal(err)
	}
	catalog37 := readHelperCatalog(t, f)
	at37 := contextAnswers(t, f, shapes)
	for _, shape := range shapes {
		key := shape.String()
		if strings.Join(at37[key], " | ") != strings.Join(at36[key], " | ") {
			t.Errorf("%s:\n at 36 %v\n at 37 %v", key, at36[key], at37[key])
		}
	}

	// The answers are the rule, not only the same: a few shapes spelled out.
	want := map[string][]string{
		// No context: nothing.
		contextShape{}.String(): {"false", "NULL", "NULL", "NULL", "NULL", "NULL", "NULL", "NULL", "NULL", "0"},
		// Empty settings are no context.
		contextShape{ptr(""), ptr(""), ptr("")}.String(): {"false", "NULL", "NULL", "NULL", "NULL", "NULL", "NULL", "NULL", "NULL", "0"},
		// The authority sees every row; only '1' is the authority.
		contextShape{nil, nil, ptr("1")}.String(): {"true", "NULL", "NULL", ids.familyA + "," + ids.familyB, ids.accountA + "," + ids.accountB,
			ids.familyA + "/" + ids.accountA + "," + ids.familyB + "/" + ids.accountB, ids.familyA + "," + ids.familyB, ids.deviceA,
			"64000000-0000-4000-8000-000000000001,64000000-0000-4000-8000-000000000002", "0"},
		contextShape{nil, nil, ptr("true")}.String(): {"false", "NULL", "NULL", "NULL", "NULL", "NULL", "NULL", "NULL", "NULL", "0"},
		// A tenant sees its own rows.
		contextShape{ptr(ids.familyA), ptr(ids.accountA), nil}.String(): {"false", ids.familyA, ids.accountA, ids.familyA, ids.accountA,
			ids.familyA + "/" + ids.accountA, ids.familyA, ids.deviceA, "64000000-0000-4000-8000-000000000001", "0"},
		// A uuid in capitals is the same uuid.
		contextShape{ptr(strings.ToUpper(ids.familyB)), nil, nil}.String(): {"false", ids.familyB, "NULL", ids.familyB, "NULL",
			ids.familyB + "/" + ids.accountB, "NULL", "NULL", "64000000-0000-4000-8000-000000000002", "0"},
		// Text that is no uuid fails as a uuid input wherever a policy reads
		// the family, even beside the authority; tables whose policies read
		// only the account and the authority answer.
		contextShape{ptr("not-a-uuid"), ptr(ids.accountA), ptr("1")}.String(): {"true", "error 22P02", ids.accountA, "error 22P02",
			ids.accountA + "," + ids.accountB, "error 22P02", ids.familyA + "," + ids.familyB, ids.deviceA, "error 22P02", "0"},
	}
	for key, answer := range want {
		if got, ok := at37[key]; !ok || strings.Join(got, " | ") != strings.Join(answer, " | ") {
			t.Errorf("%s: %v, want %v", key, got, answer)
		}
	}

	for _, signature := range contextHelpers {
		before, after := catalog36[signature], catalog37[signature]
		if before.config != "search_path=pg_catalog" || after.config != "" {
			t.Errorf("%s config %q -> %q, want the search_path clause dropped", signature, before.config, after.config)
		}
		if after.owner != before.owner || after.acl != before.acl || after.language != "sql" || after.volatility != "s" ||
			after.definer || after.strict != before.strict || after.leakproof != before.leakproof || after.language != before.language {
			t.Errorf("%s: %+v -> %+v, want the same owner, grants, language and volatility", signature, before, after)
		}
		assertQualifiedBody(t, signature, after.body)
	}
}

func ptr(v string) *string { return &v }

// assertQualifiedBody fails unless every name in body is schema-qualified:
// an inlined body is parsed in the caller's search_path.
func assertQualifiedBody(t *testing.T, signature, body string) {
	t.Helper()
	unqualified := regexp.MustCompile(`(?i)(^|[^.\w])(current_setting|nullif|uuid|text|boolean)\b`)
	if m := unqualified.FindString(body); m != "" {
		t.Errorf("%s body names %q without its schema: %s", signature, m, body)
	}
	if strings.Contains(strings.ReplaceAll(body, "OPERATOR(pg_catalog.=)", ""), "=") {
		t.Errorf("%s body has an operator without its schema: %s", signature, body)
	}
}

// The planner inlines the helpers: under the runtime and under the
// definers' owner, no statement over a policy table calls them (no function
// call per row or per call site); the policies read the settings directly.
func TestContextHelpersAreInlinedIntoThePolicies(t *testing.T) {
	f := testpg.New(t)
	ids := seedAuthorityFamilies(t, f)
	ctx := t.Context()
	called := regexp.MustCompile(`app\.(authority_context|current_family_id|current_account_id)\(`)
	explain := func(tx pgx.Tx, statement string, args ...any) string {
		t.Helper()
		rows, err := tx.Query(ctx, `EXPLAIN (VERBOSE, COSTS OFF) `+statement, args...)
		if err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(lines, "\n")
	}

	// Every table a policy guards that the runtime reads, read whole, in a
	// tenant context and under the authority.
	rows, err := f.Owner.Query(ctx, `SELECT DISTINCT format('%I.%I', n.nspname, c.relname) FROM pg_policy p JOIN pg_class c ON c.oid=p.polrelid
		JOIN pg_namespace n ON n.oid=c.relnamespace WHERE has_table_privilege('baby_tracker_runtime', c.oid, 'SELECT') ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(tables) < 20 {
		t.Fatalf("policy tables the runtime reads: %v (%v)", tables, err)
	}
	for _, authority := range []string{"", "1"} {
		tx := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
		if _, err := tx.Exec(ctx, `SELECT set_config('app.authority', $1, true)`, authority); err != nil {
			t.Fatal(err)
		}
		for _, table := range tables {
			if plan := explain(tx, `SELECT * FROM `+table); called.MatchString(plan) {
				t.Errorf("runtime (authority %q) reading %s calls a context helper:\n%s", authority, table, plan)
			}
		}
		_ = tx.Rollback(ctx)
	}

	// The request path's shapes, as the runtime: the family's pull keys
	// care_entities by the family and prunes to its partition, the context
	// check a one-time filter on the setting itself; families by id.
	tx := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
	pull := explain(tx, `SELECT changed_seq FROM care_entities WHERE family_id=$1 AND changed_seq > 0 ORDER BY changed_seq LIMIT 10`, ids.familyA)
	if !strings.Contains(pull, "One-Time Filter") || !strings.Contains(pull, "current_setting('app.family_id'::text, true)") ||
		strings.Count(pull, "Index Only Scan using care_entities_p") != 1 && strings.Count(pull, "Index Scan using care_entities_p") != 1 {
		t.Errorf("pull plan:\n%s", pull)
	}
	if family := explain(tx, `SELECT id FROM families WHERE id=$1`, ids.familyA); !strings.Contains(family, "families_pkey") ||
		!strings.Contains(family, "current_setting('app.authority'::text, true)") {
		t.Errorf("family plan:\n%s", family)
	}
	_ = tx.Rollback(ctx)

	// The definers' owner (request_authority runs as it) inlines them too.
	owner, err := f.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Rollback(context.Background())
	if _, err := owner.Exec(ctx, `SELECT set_config('app.authority', '1', true)`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{`SELECT * FROM public.sessions WHERE access_digest = '\x00'::bytea`, `SELECT * FROM public.families`, `SELECT * FROM public.accounts`} {
		if plan := explain(owner, statement); called.MatchString(plan) {
			t.Errorf("owner: %s calls a context helper:\n%s", statement, plan)
		}
	}
}

// The SET search_path clause kept a schema earlier in the caller's
// search_path from shadowing a name in the helpers. Inlined, they are
// parsed in the caller's search_path, so their names are qualified: a
// schema ahead of pg_catalog with its own current_setting and its own text
// `=` reaches the caller's unqualified calls but not the helpers, and the
// runtime still reads only its own family's rows.
func TestContextHelpersIgnoreTheCallersSearchPath(t *testing.T) {
	f := testpg.New(t)
	ids := seedAuthorityFamilies(t, f)
	ctx := t.Context()
	su := testpg.Superuser(t, f)
	for _, statement := range []string{
		`CREATE SCHEMA evil`,
		fmt.Sprintf(`CREATE FUNCTION evil.current_setting(text, boolean) RETURNS text LANGUAGE sql STABLE AS
			$$ SELECT CASE $1 WHEN 'app.family_id' THEN '%s' WHEN 'app.account_id' THEN '%s' ELSE '1' END $$`, ids.familyB, ids.accountB),
		`CREATE FUNCTION evil.always(text, text) RETURNS boolean LANGUAGE sql IMMUTABLE AS $$ SELECT true $$`,
		`CREATE OPERATOR evil.= (LEFTARG = text, RIGHTARG = text, FUNCTION = evil.always)`,
		`GRANT USAGE ON SCHEMA evil TO baby_tracker_runtime`,
	} {
		if _, err := su.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	tx := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
	if _, err := tx.Exec(ctx, `SET LOCAL search_path = evil, pg_catalog, public`); err != nil {
		t.Fatal(err)
	}
	read := func(query string) string {
		t.Helper()
		var got *string
		if err := tx.QueryRow(ctx, query).Scan(&got); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if got == nil {
			return "NULL"
		}
		return *got
	}
	// The caller's own unqualified names do reach the shadowing schema.
	if got := read(`SELECT current_setting('app.family_id', true)`); got != ids.familyB {
		t.Fatalf("unqualified current_setting = %s, want the shadow's %s", got, ids.familyB)
	}
	if got := read(`SELECT ('a'::text = 'b'::text)::text`); got != "true" {
		t.Fatalf("unqualified text = gave %s, want the shadow's true", got)
	}
	// The helpers and the policies do not.
	for query, want := range map[string]string{
		`SELECT app.current_family_id()::text`:                             ids.familyA,
		`SELECT app.current_account_id()::text`:                            ids.accountA,
		`SELECT app.authority_context()::text`:                             "false",
		`SELECT string_agg(id::text, ',' ORDER BY id) FROM families`:       ids.familyA,
		`SELECT string_agg(id::text, ',' ORDER BY id) FROM accounts`:       ids.accountA,
		`SELECT string_agg(id::text, ',' ORDER BY id) FROM invitations`:    "64000000-0000-4000-8000-000000000001",
		`SELECT string_agg(family_id::text, ',') FROM family_entitlements`: ids.familyA,
	} {
		if got := read(query); got != want {
			t.Errorf("%s under the shadowing search_path = %s, want %s", query, got, want)
		}
	}
}
