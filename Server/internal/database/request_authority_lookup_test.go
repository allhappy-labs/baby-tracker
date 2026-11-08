package database_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"babytracker/server/internal/database"
	"babytracker/server/internal/testpg"
)

// fleetFamilies is the seeded fleet: large enough that a request reading the
// families table end to end is unmistakable (thousands of rows a call), small
// enough to seed in a second.
const fleetFamilies = 3000

// fleetID is the deterministic id of the fleet's n-th row of a kind: 71
// accounts, 72 families, 73 devices, 74 sessions.
func fleetID(kind, n int) string {
	return fmt.Sprintf("%d000000-0000-4000-8000-%012d", kind, n)
}

// fleetDigest is the access digest of the fleet's n-th session, as SQL
// computes it: sha256('s' || n).
func fleetDigest(t *testing.T, pool *pgxpool.Pool, n int) []byte {
	t.Helper()
	var digest []byte
	if err := pool.QueryRow(t.Context(), `SELECT sha256(convert_to('s' || $1::int, 'UTF8'))`, n).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	return digest
}

// seedFleet seeds fleetFamilies families, each with an owner account, its
// membership and active slot, a device, the family_sync entitlement and one
// active family session, then analyzes the tables as production's autovacuum
// would. The row ids and digests are fleetID and fleetDigest of 1..n.
func seedFleet(t *testing.T, f *testpg.Fixture) {
	t.Helper()
	ctx := t.Context()
	tx, err := f.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	id := func(kind int) string {
		return fmt.Sprintf(`format('%d000000-0000-4000-8000-%%s', lpad(g::text, 12, '0'))::uuid`, kind)
	}
	for _, statement := range []string{
		`SELECT set_config('app.authority','1',true)`,
		`INSERT INTO accounts(id,display_name,status) SELECT ` + id(71) + `,'Account ' || g,'active' FROM generate_series(1,$1::int) g`,
		`INSERT INTO families(id,display_name,status,owner_account_id) SELECT ` + id(72) + `,'Family ' || g,'active',` + id(71) + ` FROM generate_series(1,$1::int) g`,
		`INSERT INTO memberships(family_id,account_id,role,status) SELECT ` + id(72) + `,` + id(71) + `,'owner','active' FROM generate_series(1,$1::int) g`,
		`INSERT INTO family_slots(account_id,family_id,state) SELECT ` + id(71) + `,` + id(72) + `,'active' FROM generate_series(1,$1::int) g`,
		`INSERT INTO devices(id,account_id,canonical_origin,continuity_key_id,continuity_digest,created_at,updated_at)
			SELECT ` + id(73) + `,` + id(71) + `,'https://tracker.example','continuity.v1',sha256(convert_to('d' || g, 'UTF8')),now(),now() FROM generate_series(1,$1::int) g`,
		`INSERT INTO family_entitlements(family_id,entitlement_key,source,status,created_at,updated_at)
			SELECT ` + id(72) + `,'family_sync','hosted_beta','active',now(),now() FROM generate_series(1,$1::int) g`,
		`INSERT INTO sessions(access_digest,id,account_id,device_id,lineage_root_id,lineage_started_at,scope,family_id,role,recovery_epoch,issued_seq,token_key_id,
			refresh_digest,access_expires_at,refresh_expires_at,deadline_at,status,created_at)
			SELECT sha256(convert_to('s' || g, 'UTF8')),` + id(74) + `,` + id(71) + `,` + id(73) + `,` + id(74) + `,now(),'family',` + id(72) + `,'owner',1,1,'token.v1',
			sha256(convert_to('r' || g, 'UTF8')),now() + interval '15 minutes',now() + interval '30 days',now() + interval '90 days','active',now()
			FROM generate_series(1,$1::int) g`,
	} {
		var args []any
		if strings.Contains(statement, "$1") {
			args = []any{fleetFamilies}
		}
		if _, err := tx.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Owner.Exec(ctx, `ANALYZE accounts, families, memberships, family_slots, devices, family_entitlements, sessions`); err != nil {
		t.Fatal(err)
	}
}

// relationReads is the number of rows each public table, its indexes
// included, has returned so far in the transaction: heap tuples for its
// sequential scans plus index entries for its index scans.
func relationReads(t *testing.T, tx pgx.Tx) map[string]int64 {
	t.Helper()
	rows, err := tx.Query(t.Context(), `SELECT coalesce(i.indrelid, c.oid)::regclass::text, sum(pg_stat_get_xact_tuples_returned(c.oid))::bigint
		FROM pg_class c LEFT JOIN pg_index i ON i.indexrelid = c.oid
		WHERE c.relnamespace = 'public'::regnamespace AND c.relkind IN ('r','p','i')
		GROUP BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	reads := map[string]int64{}
	for rows.Next() {
		var relation string
		var n int64
		if err := rows.Scan(&relation, &n); err != nil {
			t.Fatal(err)
		}
		reads[relation] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return reads
}

// Every request authenticates through app.request_authority, so its cost
// must not grow with the fleet. At 10,000 families the Phase 5 load showed
// it reading the whole families table per call: under the definer's forced
// row security the planner would not use coalesce(session family, slot
// family) as an index key past the families policy, scanned families_pkey
// end to end with the policy as a filter, and joined by filter (5,929 rows
// and 7.6 ms a call; 84% of database time). Here, with 3,000 families, each
// lookup — by digest and by session id, family on and off, the first calls
// and the cached generic plan after them — reads at most a handful of rows
// of any table, families included.
func TestRequestAuthorityReadsAFewRowsWhateverTheFleet(t *testing.T) {
	f := testpg.New(t)
	seedFleet(t, f)
	ctx := t.Context()
	digest := fleetDigest(t, f.Owner, fleetFamilies/2)
	session := fleetID(74, fleetFamilies/2)
	family := fleetID(72, fleetFamilies/2)

	conn, err := f.Runtime.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	// Ten calls of each shape: plpgsql plans the first five with the
	// parameters' values and may switch to a generic plan after them. A call
	// that plans reads a few more index entries than it returns (the planner
	// probes the ends of join keys' indexes), so the bound is a handful, not
	// one; the regression it catches reads half the fleet.
	const callsEach, maxReads = 10, 8
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
		for call := 1; call <= callsEach; call++ {
			before := relationReads(t, tx)
			var matches int32
			var got *string
			if err := tx.QueryRow(ctx, `SELECT matches, family_id::text FROM app.request_authority($1,$2,$3,1)`,
				lookup.digests, lookup.sessionID, lookup.family).Scan(&matches, &got); err != nil {
				t.Fatalf("%s: %v", lookup.name, err)
			}
			if matches != 1 || (lookup.family && (got == nil || *got != family)) || (!lookup.family && got != nil) {
				t.Fatalf("%s: matches=%d family=%v", lookup.name, matches, got)
			}
			after := relationReads(t, tx)
			var over []string
			for relation, n := range after {
				if read := n - before[relation]; read > maxReads {
					over = append(over, fmt.Sprintf("%s=%d", relation, read))
				}
			}
			if len(over) > 0 {
				sort.Strings(over)
				t.Fatalf("%s, call %d, %d families: rows read %s; want at most %d of any table", lookup.name, call, fleetFamilies,
					strings.Join(over, " "), maxReads)
			}
		}
	}

	// The per-call time on this database, for the record (not asserted).
	const timed = 200
	start := time.Now()
	for range timed {
		if _, err := tx.Exec(ctx, `SELECT * FROM app.request_authority($1,NULL,true,1)`, [][]byte{digest}); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("request_authority by digest with family: %.3f ms a call over %d calls, %d families",
		float64(time.Since(start).Microseconds())/1000/timed, timed, fleetFamilies)
}

// txStarter is what requestAuthorityAnswer opens its transaction on: a pool,
// or one connection whose session settings the call should see.
type txStarter interface {
	Begin(context.Context) (pgx.Tx, error)
}

// requestAuthorityAnswer is one app.request_authority call as a request sees
// it: the returned row, then the context it left (account, family and the
// restored app.authority), in one string; a call that fails is its SQLSTATE.
func requestAuthorityAnswer(t *testing.T, on txStarter, digests [][]byte, sessionID any, family bool, epoch int64) string {
	t.Helper()
	ctx := t.Context()
	tx, err := on.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var digestArg any
	if digests != nil {
		digestArg = digests
	}
	failed := func(err error) string {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatal(err)
		}
		return "error " + pgErr.Code
	}
	rows, err := tx.Query(ctx, `SELECT * FROM app.request_authority($1,$2,$3,$4)`, digestArg, sessionID, family, epoch)
	if err != nil {
		return failed(err)
	}
	var answer []string
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			t.Fatal(err)
		}
		for i, field := range rows.FieldDescriptions() {
			if id, ok := values[i].([16]byte); ok {
				values[i] = fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16])
			}
			answer = append(answer, fmt.Sprintf("%s=%v", field.Name, values[i]))
		}
	}
	if err := rows.Err(); err != nil {
		return failed(err)
	}
	var account, familyContext, authority *string
	if err := tx.QueryRow(ctx, `SELECT current_setting('app.account_id',true), current_setting('app.family_id',true), current_setting('app.authority',true)`).
		Scan(&account, &familyContext, &authority); err != nil {
		t.Fatal(err)
	}
	show := func(s *string) string {
		if s == nil {
			return "<unset>"
		}
		return *s
	}
	answer = append(answer, "app.account_id="+show(account), "app.family_id="+show(familyContext), "app.authority="+show(authority))
	return strings.Join(answer, " ")
}

// authorityLookup is one app.request_authority call of the lookup shapes:
// by digests (digests set) or by session id (session set), at an epoch.
type authorityLookup struct {
	name    string
	digests [][]byte
	session any
	epoch   int64
}

// seedAuthorityLookups adds to a seeded fleet every shape the lookup
// distinguishes (a session's own family or its account's slot family, no
// slot, a family that is gone, not active, without the entitlement, a
// recovery session and slot, a removed membership, two digests matching, a
// stale epoch, nothing matching; by digest and by session id) and returns
// the eighteen calls that reach them.
func seedAuthorityLookups(t *testing.T, f *testpg.Fixture) []authorityLookup {
	t.Helper()
	ctx := t.Context()
	extra := func(n int, kind string) []byte {
		t.Helper()
		var digest []byte
		if err := f.Owner.QueryRow(ctx, `SELECT sha256(convert_to($1 || $2::int, 'UTF8'))`, kind, n).Scan(&digest); err != nil {
			t.Fatal(err)
		}
		return digest
	}
	outsider := fleetFamilies + 1
	for _, statement := range []string{
		// An account with a device and no slot.
		fmt.Sprintf(`INSERT INTO accounts(id,display_name,status) VALUES ('%s','Outsider','active')`, fleetID(71, outsider)),
		fmt.Sprintf(`INSERT INTO devices(id,account_id,canonical_origin,continuity_key_id,continuity_digest,created_at,updated_at)
			VALUES ('%s','%s','https://tracker.example','continuity.v1',sha256('o'::bytea),now(),now())`, fleetID(73, outsider), fleetID(71, outsider)),
	} {
		if err := authorityExec(ctx, f.Owner, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	var lookups []authorityLookup
	byDigest := func(name string, digests ...[]byte) {
		lookups = append(lookups, authorityLookup{name: name, digests: digests, epoch: 1})
	}
	insertSession := func(kind string, n int, scope string, family any, role any) []byte {
		t.Helper()
		if err := authorityExec(ctx, f.Owner, `INSERT INTO sessions(access_digest,id,account_id,device_id,lineage_root_id,lineage_started_at,scope,family_id,role,
			recovery_epoch,issued_seq,token_key_id,refresh_digest,access_expires_at,refresh_expires_at,deadline_at,status,created_at)
			SELECT d,id,$3,$4,id,now(),$5::text::app.session_scope,$6,$7::text::app.member_role,1,1,'token.v1',sha256(d),
			  now() + interval '15 minutes',now() + interval '30 days',now() + interval '90 days','active',now()
			FROM (SELECT sha256(convert_to($1 || $2::int,'UTF8')) AS d, gen_random_uuid() AS id) s`,
			kind, n, fleetID(71, n), fleetID(73, n), scope, family, role); err != nil {
			t.Fatalf("session %s%d: %v", kind, n, err)
		}
		return extra(n, kind)
	}

	byDigest("family session", fleetDigest(t, f.Owner, 1))
	byDigest("account session, slot family", insertSession("a", 2, "account", nil, nil))
	byDigest("account session, no slot", insertSession("a", outsider, "account", nil, nil))
	byDigest("family session of another family than the slot's", insertSession("x", 3, "family", fleetID(72, 5), "caregiver"))
	byDigest("family session of a family that is gone", insertSession("x", 4, "family", "79000000-0000-4000-8000-000000000004", "owner"))
	byDigest("family without the entitlement", fleetDigest(t, f.Owner, 6))
	byDigest("account session, family without the entitlement", insertSession("a", 6, "account", nil, nil))
	byDigest("family not active", fleetDigest(t, f.Owner, 7))
	byDigest("recovery session", insertSession("x", 9, "family_recovery", fleetID(72, 9), "owner"))
	byDigest("recovery slot", insertSession("a", 10, "account", nil, nil))
	byDigest("removed membership", fleetDigest(t, f.Owner, 11))
	byDigest("entitlement with an expiry", fleetDigest(t, f.Owner, 13))
	byDigest("two digests matching", fleetDigest(t, f.Owner, 1), fleetDigest(t, f.Owner, 12))
	byDigest("one of two digests matching", extra(1, "none"), fleetDigest(t, f.Owner, 12))
	byDigest("nothing matching", extra(2, "none"))
	lookups = append(lookups,
		authorityLookup{name: "stale epoch", digests: [][]byte{fleetDigest(t, f.Owner, 1)}, epoch: 2},
		authorityLookup{name: "by session id", session: fleetID(74, 1), epoch: 1},
		authorityLookup{name: "by unknown session id", session: "79000000-0000-4000-8000-000000000001", epoch: 1})
	for _, statement := range []string{
		fmt.Sprintf(`DELETE FROM family_entitlements WHERE family_id = '%s'`, fleetID(72, 6)),
		fmt.Sprintf(`UPDATE families SET status = 'deletion_pending' WHERE id = '%s'`, fleetID(72, 7)),
		fmt.Sprintf(`UPDATE family_slots SET state = 'family_recovery' WHERE account_id = '%s'`, fleetID(71, 10)),
		fmt.Sprintf(`UPDATE memberships SET role = 'caregiver', status = 'removed' WHERE account_id = '%s'`, fleetID(71, 11)),
		fmt.Sprintf(`UPDATE family_entitlements SET expires_at = '2027-01-01T00:00:00Z' WHERE family_id = '%s'`, fleetID(72, 13)),
	} {
		if err := authorityExec(ctx, f.Owner, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	return lookups
}

// 000036 rewrites only how app.request_authority keys the family; every
// answer is the same as 35's. A schema-35 fleet with every shape the lookup
// distinguishes (a session's own family or its account's slot family, no
// slot, a family that is gone, not active, without the entitlement, a
// recovery session and slot, a removed membership, two digests matching, a
// stale epoch, nothing matching; by digest and by session id; family on and
// off) answers each call identically before and after the migration, and the
// migration leaves the privilege manifest as it was.
func TestRequestAuthorityFamilyKeyMigrationAnswersAsBefore(t *testing.T) {
	f := testpg.NewThrough(t, 35)
	seedFleet(t, f)
	ctx := t.Context()
	lookups := seedAuthorityLookups(t, f)
	answers := func() []string {
		t.Helper()
		var out []string
		for _, l := range lookups {
			for _, family := range []bool{true, false} {
				out = append(out, fmt.Sprintf("%s, family %t: %s", l.name, family, requestAuthorityAnswer(t, f.Runtime, l.digests, l.session, family, l.epoch)))
			}
		}
		return out
	}
	before := answers()
	if err := f.MigrateThrough(ctx, 36); err != nil {
		t.Fatalf("000036 over a schema-35 database: %v", err)
	}
	var version int64
	var introducedBy string
	if err := f.Owner.QueryRow(ctx, `SELECT current_version, introduced_by FROM schema_compatibility WHERE singleton`).Scan(&version, &introducedBy); err != nil ||
		version != 36 || introducedBy != "000036_request_authority_family_key.sql" {
		t.Fatalf("schema %d by %q (%v)", version, introducedBy, err)
	}
	after := answers()
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("answer changed:\n  35: %s\n  36: %s", before[i], after[i])
		}
	}
	// The answers are the shapes they claim to be, not all one refusal.
	for i, want := range map[int]string{
		0: "family_id=" + fleetID(72, 1), 2: "family_id=" + fleetID(72, 2), 6: "family_id=" + fleetID(72, 5),
		3: "app.family_id= ", 4: "family_status=<nil>",
	} {
		if !strings.Contains(after[i], want) {
			t.Errorf("answer %d lacks %q: %s", i, want, after[i])
		}
	}
	var missing, extraRows int
	if err := f.Owner.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM (SELECT principal,object_kind,object_identity,privileges FROM schema_privilege_manifest WHERE schema_version=35
			  EXCEPT SELECT principal,object_kind,object_identity,privileges FROM schema_privilege_manifest WHERE schema_version=36) m),
			(SELECT count(*) FROM (SELECT principal,object_kind,object_identity,privileges FROM schema_privilege_manifest WHERE schema_version=36
			  EXCEPT SELECT principal,object_kind,object_identity,privileges FROM schema_privilege_manifest WHERE schema_version=35) e)`).
		Scan(&missing, &extraRows); err != nil || missing != 0 || extraRows != 0 {
		t.Fatalf("privilege manifest at 36 differs from 35: %d missing, %d extra (%v)", missing, extraRows, err)
	}
	if err := database.VerifyRuntimeRole(ctx, f.Runtime, 36); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyMaintenanceRole(ctx, f.Runtime, f.Maintenance, 36); err != nil {
		t.Fatal(err)
	}
}
