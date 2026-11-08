package database_test

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"babytracker/server/internal/testpg"
	"babytracker/server/migrations"
)

// 000037 (Task 21 R1) makes app.request_authority read sessions once: the
// join runs by the digests (or the session id) itself and counts its own
// rows, instead of a count probe by the digests and the join again by the
// digest the probe found. These tests pin that every answer is 36's and
// that each call reads sessions once.

// requestAuthorityBody is the CREATE OR REPLACE of app.request_authority in
// a migration, to put one schema's body back at another.
func requestAuthorityBody(t *testing.T, migration string) string {
	t.Helper()
	raw, err := fs.ReadFile(migrations.Files, migration)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	start := strings.Index(text, "CREATE OR REPLACE FUNCTION app.request_authority(")
	end := strings.Index(text[max(start, 0):], "\nEND $$;")
	if start < 0 || end < 0 {
		t.Fatalf("%s has no app.request_authority body", migration)
	}
	return text[start : start+end+len("\nEND $$;")]
}

// installRequestAuthority replaces app.request_authority's body as its owner
// (CREATE OR REPLACE keeps the owner, grants and manifest).
func installRequestAuthority(t *testing.T, f *testpg.Fixture, body string) {
	t.Helper()
	if _, err := f.Owner.Exec(t.Context(), body); err != nil {
		t.Fatal(err)
	}
}

// requestAuthorityCatalog is what a body swap must leave alone: owner,
// grants, the definer, its pinned search_path, language and volatility.
func requestAuthorityCatalog(t *testing.T, f *testpg.Fixture) string {
	t.Helper()
	var catalog string
	if err := f.Owner.QueryRow(t.Context(), `SELECT concat_ws(' | ', pg_get_userbyid(p.proowner), p.proacl::text, p.prosecdef::text,
			array_to_string(p.proconfig, ','), l.lanname, p.provolatile::text, p.proparallel::text, pg_get_function_result(p.oid))
		FROM pg_proc p JOIN pg_language l ON l.oid = p.prolang WHERE p.oid = 'app.request_authority(bytea[],uuid,boolean,bigint)'::regprocedure`).
		Scan(&catalog); err != nil {
		t.Fatal(err)
	}
	return catalog
}

// oneProbeLookups is seedAuthorityLookups plus the shapes the one-probe
// join could tell apart from the probe and the join: the two matching
// digests in the other order, one digest twice, a revoked session (by
// digest and by id), an account that is not active and one whose chain
// failed, a stale epoch by id, and every call with a digest of another
// session's refresh token (no match).
func oneProbeLookups(t *testing.T, f *testpg.Fixture) []authorityLookup {
	t.Helper()
	ctx := t.Context()
	lookups := seedAuthorityLookups(t, f)
	for _, statement := range []string{
		fmt.Sprintf(`UPDATE sessions SET status = 'revoked', revoked_at = now(), revoke_reason = 'device_revoked' WHERE id = '%s'`, fleetID(74, 14)),
		fmt.Sprintf(`UPDATE accounts SET status = 'identity_unavailable' WHERE id = '%s'`, fleetID(71, 15)),
		fmt.Sprintf(`UPDATE accounts SET chain_failed = true WHERE id = '%s'`, fleetID(71, 16)),
	} {
		if err := authorityExec(ctx, f.Owner, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	var refresh []byte
	if err := f.Owner.QueryRow(ctx, `SELECT sha256(convert_to('r1', 'UTF8'))`).Scan(&refresh); err != nil {
		t.Fatal(err)
	}
	digest := func(n int) []byte { return fleetDigest(t, f.Owner, n) }
	return append(lookups,
		authorityLookup{name: "two digests matching, the other order", digests: [][]byte{digest(12), digest(1)}, epoch: 1},
		authorityLookup{name: "one digest twice", digests: [][]byte{digest(1), digest(1)}, epoch: 1},
		authorityLookup{name: "revoked session", digests: [][]byte{digest(14)}, epoch: 1},
		authorityLookup{name: "revoked session by id", session: fleetID(74, 14), epoch: 1},
		authorityLookup{name: "account not active", digests: [][]byte{digest(15)}, epoch: 1},
		authorityLookup{name: "account chain failed", digests: [][]byte{digest(16)}, epoch: 1},
		authorityLookup{name: "account chain failed by id", session: fleetID(74, 16), epoch: 1},
		authorityLookup{name: "stale epoch by id", session: fleetID(74, 1), epoch: 2},
		authorityLookup{name: "a refresh digest", digests: [][]byte{refresh}, epoch: 1},
		authorityLookup{name: "eight digests, the last matching", digests: [][]byte{refresh, refresh, refresh, refresh, refresh, refresh, refresh, digest(17)}, epoch: 1},
	)
}

// lookupAnswers is every lookup, family on and off, under every context
// shape the caller may have set before the call (W2-E's 100 shapes over the
// fleet's first two families and first account), each shape in a fresh
// runtime session so that "unset" really is unset, each call in its own
// transaction. Keyed by shape, then call.
func lookupAnswers(t *testing.T, f *testpg.Fixture, lookups []authorityLookup, shapes []contextShape) map[string][]string {
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
		for _, l := range lookups {
			for _, family := range []bool{true, false} {
				got = append(got, fmt.Sprintf("%s, family %t: %s", l.name, family, requestAuthorityAnswer(t, conn, l.digests, l.session, family, l.epoch)))
			}
		}
		_ = conn.Close(ctx)
		answers[shape.String()] = got
	}
	return answers
}

// Every call — 28 lookup shapes, family on and off, under 100 caller
// context shapes — answers at 37 exactly as at 36: the returned row (the
// match count, every column, the row chosen when two digests match) and
// the context it leaves (account, family, the restored app.authority),
// errors included. The comparison runs twice: across the migration (C1's
// helpers and R1's body together, as deployed) and at 37 between 36's body
// and 37's (R1 alone). Owner, grants, the definer and its search_path are
// unchanged.
func TestRequestAuthorityOneProbeAnswersAsBefore(t *testing.T) {
	f := testpg.NewThrough(t, 36)
	seedFleet(t, f)
	lookups := oneProbeLookups(t, f)
	shapes := contextShapes(authorityFixture{accountA: fleetID(71, 1), familyA: fleetID(72, 1), familyB: fleetID(72, 2)})
	if len(shapes) != 100 || len(lookups) != 28 {
		t.Fatalf("%d shapes and %d lookups, want 100 and 28", len(shapes), len(lookups))
	}
	catalog36 := requestAuthorityCatalog(t, f)
	at36 := lookupAnswers(t, f, lookups, shapes)
	if err := f.MigrateThrough(t.Context(), 37); err != nil {
		t.Fatal(err)
	}
	// 37 adds only the plan cache setting to the definer's pinned settings.
	want37 := strings.Replace(catalog36, " | search_path=pg_catalog | ", " | search_path=pg_catalog,plan_cache_mode=force_generic_plan | ", 1)
	if got := requestAuthorityCatalog(t, f); want37 == catalog36 || got != want37 {
		t.Errorf("request_authority catalog at 36 %q, at 37 %q, want %q", catalog36, got, want37)
	}
	at37 := lookupAnswers(t, f, lookups, shapes)
	installRequestAuthority(t, f, requestAuthorityBody(t, "000036_request_authority_family_key.sql"))
	if got := requestAuthorityCatalog(t, f); got != catalog36 {
		t.Errorf("request_authority catalog with 36's body at 37 %q, want %q", got, catalog36)
	}
	old37 := lookupAnswers(t, f, lookups, shapes)
	compared, unmatchedInvalid := 0, map[string]int{}
	for _, shape := range shapes {
		key := shape.String()
		for i := range at36[key] {
			for _, before := range []struct{ label, answer string }{{"at 36", at36[key][i]}, {"36's body at 37", old37[key][i]}} {
				if at37[key][i] == before.answer {
					continue
				}
				if invalidUUIDContext(shape) && strings.Contains(before.answer, ": matches=0 ") && strings.HasSuffix(at37[key][i], ": error 22P02") {
					unmatchedInvalid[before.label]++
					continue
				}
				t.Errorf("%s, %s\n  %s: %s", key, at37[key][i], before.label, before.answer)
			}
			compared++
		}
	}
	if compared != 100*28*2 {
		t.Fatalf("compared %d calls, want %d", compared, 100*28*2)
	}
	t.Logf("calls under a context holding text that is no uuid, matching no session: %v", unmatchedInvalid)

	// The answers are what they claim, not all one refusal: with no context
	// set before the call, a few spelled out.
	none := at37[contextShape{}.String()]
	answer := func(name string, family bool) string {
		t.Helper()
		prefix := fmt.Sprintf("%s, family %t: ", name, family)
		for _, a := range none {
			if strings.HasPrefix(a, prefix) {
				return a
			}
		}
		t.Fatalf("no answer %q", prefix)
		return ""
	}
	lowest := fleetDigest(t, f.Owner, 1)
	if other := fleetDigest(t, f.Owner, 12); string(other) < string(lowest) {
		lowest = other
	}
	for _, c := range []struct {
		name   string
		family bool
		want   []string
	}{
		{"family session", true, []string{"matches=1 ", " family_id=" + fleetID(72, 1), "app.family_id=" + fleetID(72, 1)}},
		{"by session id", true, []string{"matches=1 ", "session_id=" + fleetID(74, 1), "app.family_id=" + fleetID(72, 1)}},
		{"two digests matching", true, []string{"matches=2 ", fmt.Sprintf("access_digest=%v ", lowest), "app.account_id= "}},
		{"two digests matching, the other order", true, []string{"matches=2 ", fmt.Sprintf("access_digest=%v ", lowest)}},
		{"one digest twice", true, []string{"matches=1 ", "app.family_id=" + fleetID(72, 1)}},
		{"nothing matching", true, []string{"matches=0 ", "session_id=<nil>", "app.account_id= "}},
		{"by unknown session id", false, []string{"matches=0 ", "session_id=<nil>"}},
		{"revoked session by id", true, []string{"matches=1 ", "session_status=revoked", "app.account_id= "}},
		{"account not active", true, []string{"matches=1 ", "account_status=identity_unavailable", "app.account_id= "}},
		{"account chain failed by id", true, []string{"matches=1 ", "account_chain_failed=true", "app.account_id= "}},
		{"eight digests, the last matching", true, []string{"matches=1 ", " family_id=" + fleetID(72, 17), "app.family_id=" + fleetID(72, 17)}},
		{"family session", false, []string{"matches=1 ", " family_id=<nil>", "app.account_id=" + fleetID(71, 1), "app.family_id= "}},
	} {
		got := answer(c.name, c.family)
		for _, want := range c.want {
			if !strings.Contains(got, want) {
				t.Errorf("%s, family %t lacks %q: %s", c.name, c.family, want, got)
			}
		}
	}
}

// invalidUUIDContext reports whether shape sets app.family_id or
// app.account_id to text that is no uuid, a context no request path sets
// (request_authority and the definers set uuids or the empty string).
func invalidUUIDContext(shape contextShape) bool {
	return shape.family != nil && *shape.family == "not-a-uuid" || shape.account != nil && *shape.account == "not-a-uuid"
}

// sessionsScans is the number of scans of sessions, its heap and every
// index, so far in tx.
func sessionsScans(t *testing.T, tx pgx.Tx) int64 {
	t.Helper()
	var n int64
	if err := tx.QueryRow(t.Context(), `SELECT pg_stat_get_xact_numscans('public.sessions'::regclass)
			+ coalesce((SELECT sum(pg_stat_get_xact_numscans(indexrelid)) FROM pg_index WHERE indrelid = 'public.sessions'::regclass), 0)`).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Each app.request_authority call scans sessions once — by digest and by
// session id, family on and off, over the first calls' custom plans and the
// cached plan after them — where 36's body scanned it twice (the probe and
// the join), which the same count shows when 36's body is put back.
func TestRequestAuthorityReadsSessionsOnce(t *testing.T) {
	f := testpg.New(t)
	seedFleet(t, f)
	ctx := t.Context()
	digest := fleetDigest(t, f.Owner, fleetFamilies/2)
	session := fleetID(74, fleetFamilies/2)
	scansPerCall := func() map[string][]int64 {
		t.Helper()
		conn, err := f.Runtime.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		out := map[string][]int64{}
		for _, lookup := range []struct {
			name      string
			digests   any
			sessionID any
			family    bool
		}{
			{"by digest with family", [][]byte{digest}, nil, true},
			{"by session id with family", nil, session, true},
			{"by digest without family", [][]byte{digest}, nil, false},
			{"by session id without family", nil, session, false},
		} {
			for range 10 {
				before := sessionsScans(t, tx)
				var matches int32
				if err := tx.QueryRow(ctx, `SELECT matches FROM app.request_authority($1,$2,$3,1)`, lookup.digests, lookup.sessionID, lookup.family).
					Scan(&matches); err != nil || matches != 1 {
					t.Fatalf("%s: matches=%d (%v)", lookup.name, matches, err)
				}
				out[lookup.name] = append(out[lookup.name], sessionsScans(t, tx)-before)
			}
		}
		return out
	}
	for name, scans := range scansPerCall() {
		for call, n := range scans {
			if n != 1 {
				t.Errorf("%s, call %d: sessions scanned %d times, want once (%v)", name, call+1, n, scans)
				break
			}
		}
	}
	installRequestAuthority(t, f, requestAuthorityBody(t, "000036_request_authority_family_key.sql"))
	for name, scans := range scansPerCall() {
		for call, n := range scans {
			if n != 2 {
				t.Errorf("36's body, %s, call %d: sessions scanned %d times, want the probe and the join (%v)", name, call+1, n, scans)
				break
			}
		}
	}
}
