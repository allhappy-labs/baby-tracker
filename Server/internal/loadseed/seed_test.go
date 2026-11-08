//go:build identitytest

package loadseed

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/config"
	"babytracker/server/internal/database"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/loadtest"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testWriter sends a logger's lines to the test log.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// seedConfig is a hosted load node's configuration with fresh peppers.
func seedConfig(t *testing.T) config.Config {
	t.Helper()
	origin, _ := url.Parse("https://sync.load.test")
	cfg := config.Config{Mode: config.ModeHosted, PublicOrigin: origin, ServerInstanceID: newUUID(), RecoveryEpoch: 1,
		AppleAudience: "com.example.babytracker.load"}
	cfg.Limits = config.LimitsConfig{Policies: limits.DefaultPolicies(), MaxCaregivers: 10, MaxInvitations: 10, InvitationTTL: 7 * 24 * time.Hour}
	cfg.Sync.Limits = protocol.DefaultLimits()
	for _, ring := range []struct {
		ring *config.DigestKeyringConfig
		id   string
	}{{&cfg.Auth.IdentityKeys, "identity.v1"}, {&cfg.Auth.SessionTokenKeys, "session.v1"}, {&cfg.Auth.DeviceContinuityKeys, "device.v1"},
		{&cfg.Auth.AppleNotificationKeys, "notification.v1"}, {&cfg.Auth.InvitationKeys, "invitation.v1"}, {&cfg.Auth.PageTokenKeys, "page.v1"}} {
		ring.ring.Current.ID = ring.id
		if _, err := rand.Read(ring.ring.Current.Secret[:]); err != nil {
			t.Fatal(err)
		}
	}
	for _, pepper := range []*[32]byte{&cfg.Auth.ActorIDPepper, &cfg.Auth.RateLimitPepper} {
		if _, err := rand.Read(pepper[:]); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

// superuserOn is a superuser pool on pool's database (the test-db DSN's
// role), closed at cleanup.
func superuserOn(t *testing.T, pool *pgxpool.Pool, maxConns int32) *pgxpool.Pool {
	t.Helper()
	var database string
	if err := pool.QueryRow(t.Context(), `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(testpg.RequireDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + database
	cfg, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = maxConns
	superuser, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(superuser.Close)
	return superuser
}

// newSeedEnvironment is a disposable database at the current schema with
// server_state initialized, its runtime and superuser pools, and a hosted
// configuration with fresh peppers.
func newSeedEnvironment(t *testing.T) Environment {
	t.Helper()
	fixture := testpg.New(t)
	cfg := seedConfig(t)
	if _, err := authority.InitServerState(t.Context(), fixture.Owner, cfg.ServerInstanceID, cfg.Auth.ActorIDPepper); err != nil {
		t.Fatal(err)
	}
	env := Environment{Config: cfg, Runtime: fixture.Runtime, Superuser: superuserOn(t, fixture.Owner, 24),
		Logger: slog.New(slog.NewTextHandler(testWriter{t}, nil))}
	markStack(t, env.Superuser, StackMarker)
	return env
}

// markStack sets the database's comment, as `load-node.sh up` marks a load
// stack (nil: clears it).
func markStack(t *testing.T, superuser *pgxpool.Pool, comment any) {
	t.Helper()
	var database string
	if err := superuser.QueryRow(t.Context(), `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	literal := "NULL"
	if text, ok := comment.(string); ok {
		literal = "'" + strings.ReplaceAll(text, "'", "''") + "'"
	}
	if _, err := superuser.Exec(t.Context(), `COMMENT ON DATABASE `+pgx.Identifier{database}.Sanitize()+` IS `+literal); err != nil {
		t.Fatal(err)
	}
}

// The seeder writes nothing to a database that is not marked as a load
// stack, or through a superuser pool too small for the care stage.
func TestSeedRefusesAnUnmarkedDatabaseAndASmallPool(t *testing.T) {
	env := newSeedEnvironment(t)
	spec := Small(1, 1, time.Now().UTC().Truncate(time.Second))
	for _, comment := range []any{nil, "baby-tracker-production", StackMarker + " "} {
		markStack(t, env.Superuser, comment)
		_, _, _, err := Seed(t.Context(), env, spec)
		if err == nil || !strings.Contains(err.Error(), "not marked as a disposable load stack") {
			t.Fatalf("comment %v: seeded, or refused for another reason: %v", comment, err)
		}
	}
	markStack(t, env.Superuser, StackMarker)
	small := env
	small.Superuser = superuserOn(t, env.Runtime, MinSuperuserConns-1)
	if _, _, _, err := Seed(t.Context(), small, spec); err == nil || !strings.Contains(err.Error(), "superuser pool allows") {
		t.Fatalf("seeded through a pool too small for the care stage: %v", err)
	}
	var families, accounts int
	if err := env.Superuser.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM families), (SELECT count(*) FROM accounts)`).Scan(&families, &accounts); err != nil {
		t.Fatal(err)
	}
	if families != 0 || accounts != 0 {
		t.Fatalf("a refused seed wrote %d families and %d accounts", families, accounts)
	}
}

// assertNothingWritten checks that the pool's database holds no family and
// no account.
func assertNothingWritten(t *testing.T, superuser *pgxpool.Pool) {
	t.Helper()
	var families, accounts int
	if err := superuser.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM families), (SELECT count(*) FROM accounts)`).Scan(&families, &accounts); err != nil {
		t.Fatal(err)
	}
	if families != 0 || accounts != 0 {
		t.Fatalf("a refused seed wrote %d families and %d accounts", families, accounts)
	}
}

// A seed while another holds the seed lock is refused with nothing
// written; the lock is free again once that seed ends.
func TestSeedRefusesWhileAnotherSeedRuns(t *testing.T) {
	env := newSeedEnvironment(t)
	other, err := env.Superuser.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	if _, err := other.Exec(t.Context(), `SELECT pg_advisory_lock($1)`, seedLockKey); err != nil {
		t.Fatal(err)
	}
	spec := Small(1, 1, time.Now().UTC().Truncate(time.Second))
	if _, _, _, err := Seed(t.Context(), env, spec); err == nil || !strings.Contains(err.Error(), "another seed is running") {
		t.Fatalf("seeded while another seed held the lock: %v", err)
	}
	assertNothingWritten(t, env.Superuser)
	if _, err := other.Exec(t.Context(), `SELECT pg_advisory_unlock($1)`, seedLockKey); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockSeed(t.Context(), env)
	if err != nil {
		t.Fatalf("the seed lock is not free once the other seed ended: %v", err)
	}
	unlock()
	// unlock releases the lock: it can be taken again.
	unlock, err = lockSeed(t.Context(), env)
	if err != nil {
		t.Fatalf("an unlocked seed lock was not released: %v", err)
	}
	unlock()
}

// A superuser pool on another database than the runtime pool's is refused
// with nothing written to either: unmarked, and marked but different.
func TestSeedRefusesASuperuserPoolOnAnotherDatabase(t *testing.T) {
	env := newSeedEnvironment(t)
	elsewhere := superuserOn(t, testpg.New(t).Owner, 24)
	mixed := env
	mixed.Superuser = elsewhere
	spec := Small(1, 1, time.Now().UTC().Truncate(time.Second))
	if _, _, _, err := Seed(t.Context(), mixed, spec); err == nil || !strings.Contains(err.Error(), "not marked as a disposable load stack") {
		t.Fatalf("seeded through a superuser pool on an unmarked database: %v", err)
	}
	markStack(t, elsewhere, StackMarker)
	if _, _, _, err := Seed(t.Context(), mixed, spec); err == nil || !strings.Contains(err.Error(), "different databases") {
		t.Fatalf("seeded through pools on two databases: %v", err)
	}
	assertNothingWritten(t, env.Superuser)
	assertNothingWritten(t, elsewhere)
}

// A family whose history cannot be encoded fails the care stage with no
// care row or receipt committed, and the baby key restored.
func TestFailedEncodeCommitsNoCare(t *testing.T) {
	env := newSeedEnvironment(t)
	keyBefore := careKeyState(t, env.Superuser)
	schemaBefore := schemaState(t, env.Superuser)
	spec := Small(2, 1, time.Now().UTC().Truncate(time.Second))
	spec.Workers = 1
	s, err := compose(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	families, err := s.provisionAll(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	families[1].Devices = nil // no writer: encoding family 1 fails after family 0 was sent
	if _, err := loadCare(t.Context(), env, spec, families, s.server); err == nil || !strings.Contains(err.Error(), "writer") {
		t.Fatalf("the care stage did not fail on the unencodable family: %v", err)
	}
	var rows, receipts, keys int
	if err := env.Superuser.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM care_entities), (SELECT count(*) FROM care_receipts),
		(SELECT count(*) FROM pg_constraint WHERE conname = $1 AND conrelid = 'public.care_entities'::regclass)`, careFKName).Scan(&rows, &receipts, &keys); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || receipts != 0 || keys != 1 {
		t.Fatalf("a failed care stage left %d rows, %d receipts and %d baby keys", rows, receipts, keys)
	}
	if after := careKeyState(t, env.Superuser); after != keyBefore || !strings.HasSuffix(after, "valid=true") {
		t.Fatalf("the baby key is %q after the failed stage, was %q", after, keyBefore)
	}
	assertSameSchema(t, "after a failed care stage", schemaBefore, schemaState(t, env.Superuser))
}

// careKeyState is the baby key's definition and validity.
func careKeyState(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(t.Context(), `SELECT pg_get_constraintdef(oid) || ' valid=' || convalidated FROM pg_constraint
		WHERE conname = $1 AND conrelid = 'public.care_entities'::regclass`, careFKName).Scan(&state); err != nil {
		t.Fatalf("the baby key: %v", err)
	}
	return state
}

// The re-added key is a validated key under the migration's own definition:
// a care stage restores it (valid, same definition), and adding it back
// still refuses a baby that does not exist, under planner settings that do
// not outlive the transaction.
func TestCareKeyIsReaddedValidatedAndStillRefusesADanglingBaby(t *testing.T) {
	env := newSeedEnvironment(t)
	keyBefore := careKeyState(t, env.Superuser)
	spec := Small(2, 1, time.Now().UTC().Truncate(time.Second))
	s, err := compose(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	families, err := s.provisionAll(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	cares, err := loadCare(t.Context(), env, spec, families, s.server)
	if err != nil {
		t.Fatal(err)
	}
	if err := cares.waitBabyKey(); err != nil {
		t.Fatal(err)
	}
	if after := careKeyState(t, env.Superuser); after != keyBefore || !strings.HasSuffix(after, "valid=true") {
		t.Fatalf("the care stage left the baby key %q, was %q", after, keyBefore)
	}

	var definition, family, entity, baby string
	if err := env.Superuser.QueryRow(t.Context(), `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conname = $1 AND conrelid = 'public.care_entities'::regclass`, careFKName).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	if err := env.Superuser.QueryRow(t.Context(), `SELECT family_id::text, id::text, baby_id::text FROM care_entities WHERE baby_id IS NOT NULL LIMIT 1`).
		Scan(&family, &entity, &baby); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := env.Superuser.Exec(t.Context(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`ALTER TABLE public.care_entities DROP CONSTRAINT ` + careFKName)
	exec(`UPDATE care_entities SET baby_id = gen_random_uuid() WHERE family_id = $1 AND id = $2`, family, entity)
	err = addCareKey(t.Context(), env.Superuser, definition)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("adding the key over a dangling baby: %v, want a foreign key violation", err)
	}
	var keys int
	if err := env.Superuser.QueryRow(t.Context(), `SELECT count(*) FROM pg_constraint WHERE conname = $1 AND conrelid = 'public.care_entities'::regclass`, careFKName).Scan(&keys); err != nil || keys != 0 {
		t.Fatalf("a refused key left %d constraints: %v", keys, err)
	}
	exec(`UPDATE care_entities SET baby_id = $3 WHERE family_id = $1 AND id = $2`, family, entity, baby)
	if err := addCareKey(t.Context(), env.Superuser, definition); err != nil {
		t.Fatal(err)
	}
	if after := careKeyState(t, env.Superuser); after != keyBefore {
		t.Fatalf("the key is %q, was %q", after, keyBefore)
	}
	// The planner settings were the transaction's: no pooled session keeps them.
	conns := make([]*pgxpool.Conn, 0, 4)
	for range 4 {
		conn, err := env.Superuser.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
		var hash string
		if err := conn.QueryRow(t.Context(), `SHOW enable_hashjoin`).Scan(&hash); err != nil || hash != "on" {
			t.Fatalf("enable_hashjoin = %q, %v", hash, err)
		}
	}
	for _, conn := range conns {
		conn.Release()
	}
}

func TestSeedSmallFamiliesEndToEnd(t *testing.T) {
	env := newSeedEnvironment(t)
	schemaBefore := schemaState(t, env.Superuser)
	spec := Small(20, 1, time.Now().UTC().Truncate(time.Second))
	spec.MCPShare = 0.3 // R10k's 2% would grant none of 20 families
	report, header, fleet, err := Seed(t.Context(), env, spec)
	if err != nil {
		t.Fatal(err)
	}
	// The indexes and keys the stages deferred are back as the migrations made them.
	assertSameSchema(t, "after the seed", schemaBefore, schemaState(t, env.Superuser))
	assertVacuumedAndAnalyzed(t, env.Superuser)
	if report.Families != 20 || len(fleet) != 80 || header.Devices != 80 || header.Families != 20 {
		t.Fatalf("%+v", report)
	}
	for _, phase := range []string{PhaseAuthority, PhaseCare, PhaseSide, PhaseBacklog, PhaseVerify} {
		if _, ok := report.Phases[phase]; !ok {
			t.Fatalf("no %s phase in %v", phase, report.Phases)
		}
		t.Logf("phase %-9s %s", phase, report.Phases[phase].Round(time.Millisecond))
	}
	t.Logf("report %+v", report)
	if err := header.CheckFresh(time.Now(), 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if want, _ := spec.SHA256(); header.SeedSpecSHA256 != want || header.ServerInstanceID != env.Config.ServerInstanceID || header.RecoveryEpoch != 1 {
		t.Fatalf("header %+v", header)
	}
	if report.ConsumedSessions != spec.BacklogRows(80) || report.CareEntities == 0 || report.Receipts == 0 || report.Accounts < 40 ||
		report.LiveActivityRegistrations < 80 || report.PushTokens < 40 || report.DatabaseBytes == 0 {
		t.Fatalf("report %+v", report)
	}
	assertFleetShape(t, fleet)
	assertTables(t, env, report)

	s, err := compose(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	// Every device authenticates through the product's request guard.
	for i := range fleet[:8] {
		mustAuthenticate(t, s, &fleet[i])
	}
	mustCheckIntegrations(t, env, s, fleet, report.MCPGrants)
	// The audit chains verify, the counters hold and a pull from zero
	// returns every row of the family.
	mustVerifyChains(t, env)
	for _, i := range sample(20, 20) {
		if err := verifyCounters(t.Context(), env, fleet[i*DevicesPerFamily].FamilyID); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range distinctFamilies(fleet)[:3] {
		mustPullEverything(t, env, s, fleet, f)
	}
	// The history is the plan's: family 0's rows are planFamily's entities.
	assertPlannedEntities(t, env, spec, fleet[0].FamilyID)

	// A device whose access token is about to expire refreshes before its
	// request, and the fleet then holds its new tokens.
	d := fleet[5]
	d.AccessExpiresAt = time.Now()
	if err := verifyPull(t.Context(), env, s, &d); err != nil {
		t.Fatal(err)
	}
	if d.AccessToken == fleet[5].AccessToken || d.RefreshToken == fleet[5].RefreshToken {
		t.Fatal("an expiring device was not refreshed")
	}

	// The fleet round-trips through a private file.
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "fleet.jsonl")
	if err := loadtest.WriteFleet(path, header, fleet); err != nil {
		t.Fatal(err)
	}
	if _, read, err := loadtest.ReadFleet(path); err != nil || len(read) != len(fleet) || read[79].RefreshToken != fleet[79].RefreshToken {
		t.Fatalf("fleet read back: %v", err)
	}

	// A seeded stack is not seeded again.
	if _, _, _, err := Seed(t.Context(), env, Small(1, 2, spec.AsOf)); err == nil || !strings.Contains(err.Error(), "already holds families") {
		t.Fatalf("a seeded database was seeded again: %v", err)
	}
}

// TestSeedScale seeds BT_LOAD_SEED_FAMILIES families of the R10k profile
// (opt-in: a timing probe, never the full 10,000) and logs each stage's
// time and its linear projection to 10,000 families.
func TestSeedScale(t *testing.T) {
	families, err := strconv.Atoi(os.Getenv("BT_LOAD_SEED_FAMILIES"))
	if err != nil || families < 1 {
		t.Skip("BT_LOAD_SEED_FAMILIES is not set")
	}
	if families > 1000 {
		t.Fatal("the scale probe seeds at most 1,000 families; 10,000 is the load node's (Task 13)")
	}
	env := newSeedEnvironment(t)
	// The pools the command opens (OpenPool), sized as it sizes them.
	runtimePool, err := OpenPool(t.Context(), env.Runtime.Config().ConnString(), 16+8, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtimePool.Close)
	superuserPool, err := OpenPool(t.Context(), env.Superuser.Config().ConnString(), 32, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(superuserPool.Close)
	env.Runtime, env.Superuser = runtimePool, superuserPool
	spec := R10k(1, time.Now().UTC().Truncate(time.Second))
	spec.Families = families
	if path := os.Getenv("BT_LOAD_SEED_CPUPROFILE"); path != "" {
		profile, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := pprof.StartCPUProfile(profile); err != nil {
			t.Fatal(err)
		}
		defer func() { pprof.StopCPUProfile(); _ = profile.Close() }()
	}
	started := time.Now()
	report, _, fleet, err := Seed(t.Context(), env, spec)
	if err != nil {
		t.Fatal(err)
	}
	total := time.Since(started)
	scale := 10000 / float64(families)
	for _, phase := range []string{PhaseAuthority, PhaseCare, PhaseSide, PhaseBacklog, PhaseVerify} {
		d := report.Phases[phase]
		t.Logf("phase %-9s %8s  (x%.0f: %s)", phase, d.Round(time.Millisecond), scale, time.Duration(float64(d)*scale).Round(time.Second))
	}
	t.Logf("total %s; %d families, %d devices, %d care entities (%.0f per family), %d receipts, %d consumed sessions, %d push tokens, %d Live Activity registrations, %d MCP grants, %d MB",
		total.Round(time.Millisecond), report.Families, len(fleet), report.CareEntities, float64(report.CareEntities)/float64(families), report.Receipts,
		report.ConsumedSessions, report.PushTokens, report.LiveActivityRegistrations, report.MCPGrants, report.DatabaseBytes>>20)
}

// assertVacuumedAndAnalyzed checks what verify's vacuum leaves: every
// table of the product's schemas vacuumed (at least 90% of its pages
// all-visible: the pages the last refreshes wrote may not be, after a
// database-wide VACUUM as after this one) and analyzed, partitions
// included, and every partitioned table analyzed as a whole.
func assertVacuumedAndAnalyzed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT format('%s kind=%s pages=%s all_visible=%s vacuumed=%s analyzed=%s', c.relname, c.relkind,
			c.relpages, c.relallvisible, s.last_vacuum IS NOT NULL, s.last_analyze IS NOT NULL)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace LEFT JOIN pg_stat_all_tables s ON s.relid = c.oid
		WHERE n.nspname IN ('public', 'app') AND c.relkind IN ('r', 'p') AND c.relpersistence = 'p'
		  AND (s.last_analyze IS NULL OR (c.relkind = 'r' AND (s.last_vacuum IS NULL OR c.relallvisible < 0.9 * c.relpages)))
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	missed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(missed) > 0 {
		t.Fatalf("%d relations not vacuumed and analyzed by the seed:\n%s", len(missed), strings.Join(missed, "\n"))
	}
}

// assertFleetShape checks every device's state: four devices per family
// (owner 2, caregiver 2 or caregiver and viewer), credentials, a unique
// client address in 100.64.0.0/10, babies, a cursor and a Live Activity
// token.
func assertFleetShape(t *testing.T, fleet []loadtest.DeviceState) {
	t.Helper()
	ips, devices := map[string]bool{}, map[string]bool{}
	viewers, mcp := 0, 0
	for i, d := range fleet {
		if d.FamilyID == "" || d.AccountID == "" || d.DeviceID == "" || d.AccessToken == "" || d.RefreshToken == "" || d.DeviceCredential == "" ||
			len(d.BabyIDs) == 0 || d.LiveActivityToken == "" || d.AccessExpiresAt.IsZero() || !d.ReauthenticateAt.After(time.Now().Add(29*24*time.Hour)) {
			t.Fatalf("device %d: %+v", i, redacted(d))
		}
		if d.FamilyID != fleet[i-i%DevicesPerFamily].FamilyID {
			t.Fatalf("device %d is not in its family's block", i)
		}
		if ips[d.ClientIP] || devices[d.DeviceID] || !strings.HasPrefix(d.ClientIP, "100.64.") {
			t.Fatalf("device %d address %s or ID repeats", i, d.ClientIP)
		}
		ips[d.ClientIP], devices[d.DeviceID] = true, true
		want := []string{"owner", "owner", "caregiver", "caregiver"}[i%DevicesPerFamily]
		if i%DevicesPerFamily == 3 && d.Role == "viewer" {
			viewers++
		} else if d.Role != want {
			t.Fatalf("device %d is a %s, want %s", i, d.Role, want)
		}
		if d.MCPCredential != "" {
			mcp++
		}
	}
	t.Logf("%d viewer families, %d MCP grants", viewers, mcp)
}

func redacted(d loadtest.DeviceState) loadtest.DeviceState {
	d.AccessToken, d.RefreshToken, d.DeviceCredential, d.MCPCredential = "[redacted]", "[redacted]", "[redacted]", ""
	return d
}

// assertTables checks the side tables against the report.
func assertTables(t *testing.T, env Environment, report Report) {
	t.Helper()
	var cursors, pushTokens, registrations, entitlements, consumed, grants, receipts, care int
	if err := env.Superuser.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM device_sync_cursors), (SELECT count(*) FROM device_push_tokens), (SELECT count(*) FROM live_activity_devices),
		(SELECT count(*) FROM family_entitlements WHERE status = 'active'), (SELECT count(*) FROM sessions WHERE status = 'consumed' AND child_id IS NOT NULL),
		(SELECT count(*) FROM mcp_integrations WHERE status = 'active'), (SELECT count(*) FROM care_receipts), (SELECT count(*) FROM care_entities)`).
		Scan(&cursors, &pushTokens, &registrations, &entitlements, &consumed, &grants, &receipts, &care); err != nil {
		t.Fatal(err)
	}
	if cursors != report.Devices || pushTokens != report.PushTokens || registrations != report.Devices || entitlements != report.Families ||
		consumed < report.ConsumedSessions || grants != report.MCPGrants || receipts != report.Receipts || care != report.CareEntities {
		t.Fatalf("cursors %d, push tokens %d, registrations %d, entitlements %d, consumed %d, grants %d, receipts %d, care %d; report %+v",
			cursors, pushTokens, registrations, entitlements, consumed, grants, receipts, care, report)
	}
	var fk string
	if err := env.Superuser.QueryRow(t.Context(), `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conname = $1 AND conrelid = 'public.care_entities'::regclass AND convalidated`, careFKName).Scan(&fk); err != nil {
		t.Fatalf("the baby key is not back: %v", err)
	}
}

// mustAuthenticate authenticates d through the request seam and checks the
// session's family and device.
func mustAuthenticate(t *testing.T, s *services, d *loadtest.DeviceState) {
	t.Helper()
	req, err := s.request(d)
	if err != nil {
		t.Fatal(err)
	}
	err = s.authority.WithRequestTx(t.Context(), req, "read", func(_ pgx.Tx, p access.Principal, g access.Grant) error {
		if g.FamilyID != d.FamilyID || p.DeviceID != d.DeviceID || p.AccountID != d.AccountID || g.Role != d.Role || p.Scope != "family" {
			t.Errorf("device %s authenticated as %+v %+v", d.DeviceID, p, g)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("device %s: %v", d.DeviceID, err)
	}
}

// mustCheckIntegrations checks every MCP credential in the fleet through
// the integration reader the MCP route authenticates with.
func mustCheckIntegrations(t *testing.T, env Environment, s *services, fleet []loadtest.DeviceState, grants int) {
	t.Helper()
	pool, err := database.OpenReadOnlyWithMax(t.Context(), env.Runtime.Config().ConnString(), 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	reader, err := authority.NewIntegrationReader(s.authority, pool)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, d := range fleet {
		if d.MCPCredential == "" {
			continue
		}
		identity, err := reader.CheckIntegration(t.Context(), d.MCPCredential)
		if err != nil || identity.FamilyID != d.FamilyID || d.Role != "owner" {
			t.Fatalf("device %s integration: %+v %v", d.DeviceID, identity, err)
		}
		checked++
	}
	if checked == 0 || checked != grants {
		t.Fatalf("%d integration credentials in the fleet, %d granted", checked, grants)
	}
}

func mustVerifyChains(t *testing.T, env Environment) {
	t.Helper()
	verifier, err := authority.NewAuditVerifier(env.Superuser, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	report, err := verifier.VerifyAll(t.Context())
	if err != nil || len(report.Failed) > 0 || report.Scopes == 0 {
		t.Fatalf("audit chains: %+v %v", report, err)
	}
}

func distinctFamilies(fleet []loadtest.DeviceState) []string {
	var out []string
	for _, d := range fleet {
		if !slices.Contains(out, d.FamilyID) {
			out = append(out, d.FamilyID)
		}
	}
	return out
}

// mustPullEverything pulls familyID from cursor 0 as each of its devices
// and checks the entities pulled are exactly the family's rows.
func mustPullEverything(t *testing.T, env Environment, s *services, fleet []loadtest.DeviceState, familyID string) {
	t.Helper()
	rows, err := env.Superuser.Query(t.Context(), `SELECT id::text FROM care_entities WHERE family_id = $1 ORDER BY id`, familyID)
	if err != nil {
		t.Fatal(err)
	}
	want, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	for i := range fleet {
		d := &fleet[i]
		if d.FamilyID != familyID {
			continue
		}
		p, err := s.principal(t.Context(), d)
		if err != nil {
			t.Fatal(err)
		}
		request := protocol.SyncRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityRecordsV1}},
			Mutations: []protocol.Mutation{}}
		var got []string
		for {
			response, err := s.sync.ApplyAndPull(t.Context(), p, request)
			if err != nil {
				t.Fatal(err)
			}
			page, err := decodePage(response)
			if err != nil {
				t.Fatal(err)
			}
			for _, change := range page.Changes {
				if change.SnapshotOrTombstone.Meta.FamilyID != familyID {
					t.Fatalf("pulled %s of family %s", change.EntityID, change.SnapshotOrTombstone.Meta.FamilyID)
				}
				got = append(got, change.EntityID)
			}
			if page.NextPageToken == nil {
				break
			}
			request.PageToken = page.NextPageToken
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Fatalf("device %s pulled %d entities, the family holds %d", d.DeviceID, len(got), len(want))
		}
	}
}

// assertPlannedEntities checks that family 0 holds exactly planFamily's
// entities.
func assertPlannedEntities(t *testing.T, env Environment, spec Spec, familyID string) {
	t.Helper()
	plan := planFamily(spec, 0)
	want := map[string]bool{}
	for _, c := range plan.Changes {
		want[c.Mutation.EntityID] = true
	}
	rows, err := env.Superuser.Query(t.Context(), `SELECT id::text FROM care_entities WHERE family_id = $1`, familyID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) || len(got) != plan.Entities() {
		t.Fatalf("family 0 holds %d rows, its plan %d entities (%d)", len(got), len(want), plan.Entities())
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("family 0 holds %s, which its plan does not", id)
		}
	}
}
