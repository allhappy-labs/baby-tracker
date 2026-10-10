package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/config"
	"babytracker/server/internal/database"
	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/migrate"
	"babytracker/server/internal/readiness"
	"babytracker/server/internal/testkit"
	"babytracker/server/internal/testpg"
	"babytracker/server/migrations"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func boundReadinessConfig() config.Config {
	cfg := testApplicationConfig(config.ModeSelfHosted)
	cfg.Admission = config.AdmissionConfig{}
	return cfg
}

// testClock is a monitor clock a test advances past the audit interval.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock { return &testClock{now: time.Now()} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// Test-only readiness options.
func withClock(now func() time.Time) readinessOption {
	return func(r *databaseReadiness) { r.now = now }
}

func withSchema(compatibility migrate.Compatibility, runner *migrate.Runner) readinessOption {
	return func(r *databaseReadiness) { r.compatibility, r.runner = compatibility, runner }
}

func withLogger(logger *slog.Logger) readinessOption {
	return func(r *databaseReadiness) { r.logger = logger }
}

func withLogin(login string) readinessOption {
	return func(r *databaseReadiness) { r.maintenanceLogin = login }
}

func withBudget(budget config.DatabasePools, pools []database.NamedPoolBudget) readinessOption {
	return func(r *databaseReadiness) { r.budget, r.pools = budget, pools }
}

func withReset(pools ...*pgxpool.Pool) readinessOption {
	return func(r *databaseReadiness) { r.reset = pools }
}

func withInventoryRead(cfg config.Config, read func(context.Context) (authority.KeyInventory, error)) readinessOption {
	return func(r *databaseReadiness) { r.inventory = newKeyInventory(cfg.Auth, read) }
}

// composeReadiness composes the server readiness on pool (its readiness
// pool) with maintenance as its one maintenance pool, expecting
// maintenance's own login, and the key inventory reading an authority over
// inventoryPool. Nothing is probed.
func composeReadiness(t *testing.T, cfg config.Config, pool, inventoryPool, maintenance *pgxpool.Pool, options ...readinessOption) *databaseReadiness {
	t.Helper()
	var login string
	if err := maintenance.QueryRow(t.Context(), "SELECT current_user").Scan(&login); err != nil {
		t.Fatal(err)
	}
	backgroundAuthority, err := authority.New(authorityOptions(cfg, inventoryPool))
	if err != nil {
		t.Fatal(err)
	}
	r, err := newDatabaseReadiness(cfg, readinessPools{Readiness: pool, Maintenance: []*pgxpool.Pool{maintenance},
		MaintenanceLogin: login}, backgroundAuthority, nil, options...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// readinessOver is composeReadiness with the inventory on pool too.
func readinessOver(t *testing.T, cfg config.Config, pool, maintenance *pgxpool.Pool, options ...readinessOption) *databaseReadiness {
	t.Helper()
	return composeReadiness(t, cfg, pool, pool, maintenance, options...)
}

// boundReadiness runs `authority init` (as the table owner) for cfg on f
// once and returns the server readiness over f's runtime and maintenance
// logins (or the given pools), on a clock the test advances, with the key
// inventory computed and the first refresh (fast probe and audit) passed.
func boundReadiness(t *testing.T, f *testpg.Fixture, cfg config.Config, options ...readinessOption) (*databaseReadiness, *testClock) {
	t.Helper()
	return boundReadinessOver(t, f, cfg, f.Runtime, f.Runtime, f.Maintenance, options...)
}

func boundReadinessOver(t *testing.T, f *testpg.Fixture, cfg config.Config, pool, inventoryPool, maintenance *pgxpool.Pool,
	options ...readinessOption) (*databaseReadiness, *testClock) {
	t.Helper()
	if _, err := authority.InitServerState(t.Context(), f.Owner, cfg.ServerInstanceID, cfg.Auth.ActorIDPepper); err != nil {
		t.Fatal(err)
	}
	clock := newTestClock()
	r := composeReadiness(t, cfg, pool, inventoryPool, maintenance, append([]readinessOption{withClock(clock.Now)}, options...)...)
	if err := r.inventory.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := r.Refresh(t.Context()); err != nil {
		t.Fatalf("bound node not ready: %v", err)
	}
	return r, clock
}

// refreshed runs one refresh and returns the cached outcome requests see.
func refreshed(t *testing.T, r *databaseReadiness) error {
	t.Helper()
	refreshErr := r.Refresh(t.Context())
	checkErr := r.Check(t.Context())
	if !errors.Is(checkErr, refreshErr) && !errors.Is(refreshErr, checkErr) {
		t.Fatalf("cached %v differs from the refresh's %v", checkErr, refreshErr)
	}
	return checkErr
}

// Readiness holds the node to server_state: the row must exist, carry the
// configured instance and epoch, and commit to the configured actor-ID
// key. Each mismatch fails closed with its own code, logged once.
func TestReadinessRequiresServerStateIdentityAndActorKey(t *testing.T) {
	f := testpg.New(t)
	cfg := boundReadinessConfig()
	unbound := readinessOver(t, cfg, f.Runtime, f.Maintenance)
	if err := unbound.inventory.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := unbound.Check(t.Context()); !errors.Is(err, readiness.ErrNotReady) {
		t.Fatalf("before the first refresh: %v", err)
	}
	if err := refreshed(t, unbound); !errors.Is(err, authority.ErrServerStateMissing) {
		t.Fatalf("uninitialized: %v", err)
	}
	if _, _, err := unbound.ServerState(t.Context()); err == nil {
		t.Fatal("/v1/server identity published before init")
	}
	var logs bytes.Buffer
	ready, _ := boundReadiness(t, f, cfg, withLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	if instance, epoch, err := ready.ServerState(t.Context()); err != nil || instance != cfg.ServerInstanceID || epoch != 1 {
		t.Fatalf("server state=%s/%d %v", instance, epoch, err)
	}
	// A monitor that stopped refreshing (a hung probe) serves neither
	// readiness nor the published identity once its state is stale.
	// The bound covers a whole refresh at its full budgets, with headroom.
	if readinessStaleAfter < readinessInterval+fastProbeTimeout+auditTimeout+readinessInterval {
		t.Fatalf("staleness bound %v does not cover a refresh's budgets", readinessStaleAfter)
	}
	stalled, clock := boundReadiness(t, f, cfg)
	clock.Advance(readinessStaleAfter)
	if _, _, err := stalled.ServerState(t.Context()); err != nil {
		t.Fatalf("identity within the staleness bound: %v", err)
	}
	if err := stalled.Check(t.Context()); err != nil {
		t.Fatalf("readiness within the staleness bound: %v", err)
	}
	clock.Advance(time.Millisecond)
	if err := stalled.Check(t.Context()); !errors.Is(err, readiness.ErrStale) {
		t.Fatalf("stale readiness: %v", err)
	}
	if _, _, err := stalled.ServerState(t.Context()); !errors.Is(err, readiness.ErrStale) {
		t.Fatalf("stale identity published: %v", err)
	}
	for name, mutate := range map[string]func(*config.Config){
		"other instance": func(c *config.Config) { c.ServerInstanceID = "223e4567-e89b-42d3-a456-426614174000" },
		"other epoch":    func(c *config.Config) { c.RecoveryEpoch = 2 },
		"other actor key": func(c *config.Config) {
			c.Auth.ActorIDPepper[1] ^= 0xff
		},
	} {
		t.Run(name, func(t *testing.T) {
			other := cfg
			mutate(&other)
			changed := readinessOver(t, other, f.Runtime, f.Maintenance)
			err := refreshed(t, changed)
			want := authority.ErrServerIdentityMismatch
			if name == "other actor key" {
				want = authority.ErrActorIDKeyMismatch
			}
			if !errors.Is(err, want) {
				t.Fatalf("err=%v want %v", err, want)
			}
			// /v1/server refuses another node's identity, as before; a
			// replaced actor key is readiness's refusal alone.
			if _, _, err := changed.ServerState(t.Context()); (err != nil) != (name != "other actor key") {
				t.Fatalf("server state: %v", err)
			}
		})
	}
	// A tampered commitment fails closed at the next fast probe (no audit
	// needed) and is logged once, not per refresh or per request.
	if _, err := f.Owner.Exec(t.Context(), `UPDATE server_state SET actor_key_commitment=$1 WHERE singleton`, bytes.Repeat([]byte{1}, 32)); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := refreshed(t, ready); !errors.Is(err, authority.ErrActorIDKeyMismatch) {
			t.Fatalf("tampered commitment: %v", err)
		}
		for range 10 {
			if err := ready.Check(t.Context()); !errors.Is(err, authority.ErrActorIDKeyMismatch) {
				t.Fatalf("tampered commitment: %v", err)
			}
		}
	}
	if strings.Count(logs.String(), "actor_id_key_mismatch") != 1 {
		t.Fatalf("mismatch alert logged %d times: %s", strings.Count(logs.String(), "actor_id_key_mismatch"), logs.String())
	}
	if _, err := f.Owner.Exec(t.Context(), `UPDATE server_state SET actor_key_commitment=$1 WHERE singleton`, func() []byte {
		c := authority.ActorKeyCommitment(cfg.Auth.ActorIDPepper, cfg.ServerInstanceID)
		return c[:]
	}()); err != nil {
		t.Fatal(err)
	}
	if err := refreshed(t, ready); err != nil {
		t.Fatalf("restored commitment: %v", err)
	}
	if strings.Count(logs.String(), "node ready") != 2 {
		t.Fatalf("readiness and its recovery not logged once each: %s", logs.String())
	}
}

// A runtime or maintenance login holding a privilege beyond the manifest
// fails readiness closed at the next audit (startup, a schema change, a
// failed audit, or every five minutes; not every fast probe), and readiness
// recovers at the next refresh once the drift is revoked.
func TestReadinessFailsClosedOnRoleDriftAndRecovers(t *testing.T) {
	f := testpg.New(t)
	cfg := boundReadinessConfig()
	for _, drift := range []struct{ name, grant, revoke, want string }{
		{"runtime", `GRANT REFERENCES (id) ON public.families TO baby_tracker_runtime`, `REVOKE REFERENCES (id) ON public.families FROM baby_tracker_runtime`,
			"runtime database_role_separation_invalid"},
		{"maintenance", `GRANT SELECT ON public.accounts TO baby_tracker_maintenance`, `REVOKE SELECT ON public.accounts FROM baby_tracker_maintenance`,
			"maintenance database_role_separation_invalid"},
		// A revoked privilege surfaces as a refused query (42501), not as a
		// row comparison: it is a verdict too, never "unavailable".
		{"runtime privilege revoked", `REVOKE SELECT ON public.schema_migrations FROM baby_tracker_runtime`,
			`GRANT SELECT ON public.schema_migrations TO baby_tracker_runtime`, "schema check refused (SQLSTATE 42501)"},
	} {
		t.Run(drift.name, func(t *testing.T) {
			ready, clock := boundReadiness(t, f, cfg)
			if _, err := f.Owner.Exec(t.Context(), drift.grant); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = f.Owner.Exec(context.Background(), drift.revoke) })
			clock.Advance(2 * time.Second)
			if err := refreshed(t, ready); err != nil {
				t.Fatalf("a fast probe ran the role audit: %v", err)
			}
			clock.Advance(authorityLoopInterval)
			// The last good verdict is 5 minutes old, inside the grace: a
			// drift read as inconclusive would be served as ready here.
			err := refreshed(t, ready)
			if !errors.Is(err, database.ErrUnavailable) || errors.Is(err, readiness.ErrInconclusive) || !strings.Contains(err.Error(), drift.want) {
				t.Fatalf("%s drift at the periodic audit: %v", drift.name, err)
			}
			clock.Advance(2 * time.Second)
			if err := refreshed(t, ready); err == nil {
				t.Fatal("drift served as ready on the next tick")
			}
			if _, err := f.Owner.Exec(t.Context(), drift.revoke); err != nil {
				t.Fatal(err)
			}
			clock.Advance(2 * time.Second)
			if err := refreshed(t, ready); err != nil {
				t.Fatalf("revoked drift still refused: %v", err)
			}
		})
	}
	// A maintenance login that is not the configured one is refused, as a
	// verdict (not an inconclusive audit).
	other := readinessOver(t, cfg, f.Runtime, f.Maintenance, withLogin("baby_tracker_export_app"))
	if err := other.inventory.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := refreshed(t, other); !errors.Is(err, database.ErrUnavailable) || errors.Is(err, readiness.ErrInconclusive) ||
		!strings.Contains(err.Error(), "isolated maintenance principal") {
		t.Fatalf("unexpected maintenance login: %v", err)
	}
	// The runtime credential as the maintenance pool is refused.
	runtimeAsMaintenance := readinessOver(t, cfg, f.Runtime, f.Runtime)
	if err := runtimeAsMaintenance.inventory.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := refreshed(t, runtimeAsMaintenance); !errors.Is(err, database.ErrUnavailable) || errors.Is(err, readiness.ErrInconclusive) {
		t.Fatalf("runtime credential as maintenance: %v", err)
	}
}

// When the database goes away readiness fails closed within the fast probe's
// deadline, and when it comes back the next refresh is ready again.
func TestReadinessFailsClosedWhileTheDatabaseIsDownAndRecovers(t *testing.T) {
	f := testpg.New(t)
	cfg := boundReadinessConfig()
	proxy := newSwitchProxy(t, f.Runtime.Config().ConnConfig.Host, f.Runtime.Config().ConnConfig.Port)
	viaProxy := f.Runtime.Config().Copy()
	viaProxy.ConnConfig.Host, viaProxy.ConnConfig.Port = "127.0.0.1", proxy.port
	viaProxy.ConnConfig.ConnectTimeout = time.Second
	pool, err := pgxpool.NewWithConfig(t.Context(), viaProxy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ready, clock := boundReadinessOver(t, f, cfg, pool, f.Runtime, f.Maintenance)
	proxy.set(false)
	clock.Advance(2 * time.Second)
	started := time.Now()
	if err := refreshed(t, ready); !errors.Is(err, database.ErrUnavailable) {
		t.Fatalf("database down: %v", err)
	}
	if elapsed := time.Since(started); elapsed > fastProbeTimeout+time.Second {
		t.Fatalf("the fast probe outlived its deadline: %v", elapsed)
	}
	if _, _, err := ready.ServerState(t.Context()); !errors.Is(err, database.ErrUnavailable) {
		t.Fatalf("/v1/server identity published while the database is down: %v", err)
	}
	clock.Advance(2 * time.Second)
	if err := refreshed(t, ready); !errors.Is(err, database.ErrUnavailable) {
		t.Fatalf("database still down: %v", err)
	}
	proxy.set(true)
	clock.Advance(2 * time.Second)
	if err := refreshed(t, ready); err != nil {
		t.Fatalf("database back: %v", err)
	}
	if _, _, err := ready.ServerState(t.Context()); err != nil {
		t.Fatalf("/v1/server identity after recovery: %v", err)
	}
}

// Node readiness costs a request no round trip: once the monitor has
// refreshed, Check and ServerState (and /readyz and /v1/server through the
// composed router) touch neither the background pool nor the request pool;
// the monitor's own fast refresh is exactly one round trip, one statement.
func TestReadinessCheckCostsNoRoundTrip(t *testing.T) {
	f := testpg.New(t)
	cfg := boundReadinessConfig()
	cfg.Limits.InvitationTTL = time.Hour
	if _, err := authority.InitServerState(t.Context(), f.Owner, cfg.ServerInstanceID, cfg.Auth.ActorIDPepper); err != nil {
		t.Fatal(err)
	}
	background, wire := testkit.CountedPool(t, f.Runtime)
	requests, requestWire := testkit.CountedPool(t, f.Runtime)
	// background is the readiness pool here (and the inventory's).
	ready := readinessOver(t, cfg, background, f.Maintenance)
	if err := ready.inventory.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := ready.Refresh(t.Context()); err != nil {
		t.Fatalf("warm refresh: %v", err)
	}
	authorityService, familyService, err := runtimeServices(cfg, requests)
	if err != nil {
		t.Fatal(err)
	}
	options, err := applicationOptions(cfg, nil, ready, authorityService, familyService, requests, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = options.LiveHub.Shutdown(context.Background()) })
	handler := httpapi.New(options)
	wire.Reset()
	requestWire.Reset()
	for range 1000 {
		if err := ready.Check(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, _, err := ready.ServerState(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/readyz", "/v1/server"} {
		for range 50 {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("%s=%d %s", path, recorder.Code, recorder.Body)
			}
		}
	}
	if trips, requestTrips := wire.Trips(), requestWire.Trips(); trips != 0 || requestTrips != 0 {
		t.Fatalf("readiness reads cost %d background and %d request round trips: %q %q", trips, requestTrips, wire.Statements(), requestWire.Statements())
	}
	if err := ready.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if trips, statements := wire.Trips(), wire.Statements(); trips != 1 || len(statements) != 1 || statements[0] != fastProbeSQL {
		t.Fatalf("fast refresh: %d round trips, statements %q", trips, statements)
	}
	if requestWire.Trips() != 0 {
		t.Fatalf("the monitor used the request pool: %q", requestWire.Statements())
	}
}

// switchProxy forwards TCP to a PostgreSQL server while up; set(false) cuts
// every forwarded connection and refuses new ones until set(true).
type switchProxy struct {
	port   uint16
	mu     sync.Mutex
	up     bool
	conns  map[net.Conn]struct{}
	target string
}

func newSwitchProxy(t *testing.T, host string, port uint16) *switchProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &switchProxy{port: uint16(listener.Addr().(*net.TCPAddr).Port), up: true, conns: map[net.Conn]struct{}{},
		target: net.JoinHostPort(host, strconv.Itoa(int(port)))}
	t.Cleanup(func() { listener.Close(); p.set(false) })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go p.forward(conn)
		}
	}()
	return p
}

func (p *switchProxy) forward(conn net.Conn) {
	p.mu.Lock()
	if !p.up {
		p.mu.Unlock()
		conn.Close()
		return
	}
	upstream, err := net.DialTimeout("tcp", p.target, time.Second)
	if err != nil {
		p.mu.Unlock()
		conn.Close()
		return
	}
	p.conns[conn], p.conns[upstream] = struct{}{}, struct{}{}
	p.mu.Unlock()
	go func() { _, _ = io.Copy(upstream, conn); upstream.Close() }()
	_, _ = io.Copy(conn, upstream)
	conn.Close()
}

func (p *switchProxy) set(up bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.up = up
	if !up {
		for conn := range p.conns {
			conn.Close()
		}
		clear(p.conns)
	}
}

// A binary from before the squash (a chain 1–31, compatibility {min:31,max:31})
// refuses the baseline database: readiness 503, never a session against it.
func TestOldBinaryFailsClosedOnTheBaseline(t *testing.T) {
	f := testpg.New(t)
	cfg := boundReadinessConfig()
	ready, _ := boundReadiness(t, f, cfg)
	old := migrate.Compatibility{Min: 31, Max: 31}
	for version := int64(1); version <= 31; version++ {
		old.Known = append(old.Known, migrate.MigrationDigest{Version: version, Name: fmt.Sprintf("%06d_chain.sql", version), SHA256: strings.Repeat("c", 64)})
	}
	if err := migrate.ValidateCompatibility(old.Known, old); err != nil {
		t.Fatalf("the synthetic chain is not a valid old binary: %v", err)
	}
	if err := refreshed(t, ready); err != nil {
		t.Fatalf("current binary: %v", err)
	}
	oldBinary := readinessOver(t, cfg, f.Runtime, f.Maintenance, withSchema(old, migrate.New(migrations.Files)))
	if err := oldBinary.inventory.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := refreshed(t, oldBinary); err == nil {
		t.Fatal("a schema-31 binary served the baseline database")
	}
}

// rotate-epoch moves server_state to the next epoch: a node still
// configured for the old epoch fails closed until restarted with the new one.
func TestReadinessFailsClosedAfterEpochRotationUntilReconfigured(t *testing.T) {
	f := testpg.New(t)
	cfg := boundReadinessConfig()
	ready, _ := boundReadiness(t, f, cfg)
	if _, err := authority.RotateRecoveryEpoch(t.Context(), f.Owner, cfg.ServerInstanceID, 1); err != nil {
		t.Fatal(err)
	}
	// The fast probe reads server_state: no audit is needed to notice.
	if err := refreshed(t, ready); !errors.Is(err, authority.ErrServerIdentityMismatch) {
		t.Fatalf("stale epoch: %v", err)
	}
	if _, _, err := ready.ServerState(t.Context()); !errors.Is(err, authority.ErrServerIdentityMismatch) {
		t.Fatalf("stale epoch published: %v", err)
	}
	cfg.RecoveryEpoch = 2
	restarted := readinessOver(t, cfg, f.Runtime, f.Maintenance)
	if err := restarted.inventory.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := refreshed(t, restarted); err != nil {
		t.Fatalf("restarted with the rotated epoch: %v", err)
	}
}

// Retiring a session-token key: a session past its lifetime pins nothing
// even while its status stays active, so readiness is ready once the
// retired key is removed from the configuration; a live session under the
// same key keeps readiness closed with credential_key_unavailable.
func TestReadinessReleasesRetiredSessionKeyOfExpiredSessions(t *testing.T) {
	f := testpg.New(t)
	cfg := boundReadinessConfig()
	ready, clock := boundReadiness(t, f, cfg)
	now := time.Now().UTC().Truncate(time.Microsecond)
	const (
		account = "71000000-0000-4000-8000-000000000001"
		device  = "73000000-0000-4000-8000-000000000001"
	)
	seed := func(statement string, args ...any) {
		t.Helper()
		tx, err := f.Owner.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(t.Context(), `SELECT set_config('app.authority','1',true)`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), statement, args...); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	seed(`INSERT INTO accounts(id,display_name,status,created_at,updated_at) VALUES ($1,'Retired key','active',$2,$2)`, account, now)
	seed(`INSERT INTO devices(id,account_id,canonical_origin,continuity_key_id,continuity_digest,created_at,updated_at)
		VALUES ($1,$2,'https://tracker.example','device.v1',$3,$4,$4)`, device, account, bytes.Repeat([]byte{7}, 32), now)
	session := func(id string, digest byte, deadline time.Time) {
		t.Helper()
		seed(`INSERT INTO sessions(access_digest,id,account_id,device_id,lineage_root_id,lineage_started_at,scope,recovery_epoch,issued_seq,
				token_key_id,refresh_digest,access_expires_at,refresh_expires_at,deadline_at,status,created_at)
			VALUES ($1,$2,$3,$4,$2,$5,'account',$6,1,'session.retired',$7,$8,$8,$8,'active',$5)`,
			bytes.Repeat([]byte{digest}, 32), id, account, device, deadline.Add(-24*time.Hour), int64(cfg.RecoveryEpoch),
			bytes.Repeat([]byte{digest + 1}, 32), deadline)
	}
	// Abandoned: never refreshed, its lifetime over, status still active.
	session("74000000-0000-4000-8000-000000000001", 0x10, now.Add(-time.Minute))
	if err := ready.inventory.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(authorityLoopInterval) // the audit reads the inventory's verdict
	if err := refreshed(t, ready); err != nil {
		t.Fatalf("expired session pinned the retired key: %v", err)
	}
	// Live: it can still authenticate and refresh, so it pins the key.
	session("74000000-0000-4000-8000-000000000002", 0x20, now.Add(time.Hour))
	if err := ready.inventory.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(authorityLoopInterval)
	if err := refreshed(t, ready); !errors.Is(err, authority.ErrCredentialKeyUnavailable) || !strings.Contains(err.Error(), "session.retired") {
		t.Fatalf("live session under a retired key: %v", err)
	}
}

// Every mode the server starts in computes the key inventory: development
// applies the self-hosted session lifetime, as refresh does.
func TestKeyInventoryComputesInEveryMode(t *testing.T) {
	f := testpg.New(t)
	for _, mode := range []config.Mode{config.ModeDevelopment, config.ModeSelfHosted, config.ModeHosted} {
		cfg := boundReadinessConfig()
		cfg.Mode = mode
		readiness := readinessOver(t, cfg, f.Runtime, f.Maintenance)
		if err := readiness.inventory.Refresh(t.Context()); err != nil || readiness.inventory.Check() != nil {
			t.Fatalf("%s inventory: %v %v", mode, err, readiness.inventory.Check())
		}
	}
}

// Readiness owns its connection: with the background pool's only
// connection held (the verifier, the key inventory or a sweep running long),
// the fast probe and the periodic audit still run and the node stays ready.
func TestReadinessKeepsItsConnectionWhileTheBackgroundPoolIsHeld(t *testing.T) {
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
	if _, err := authority.InitServerState(t.Context(), f.Owner, cfg.ServerInstanceID, cfg.Auth.ActorIDPepper); err != nil {
		t.Fatal(err)
	}
	var login string
	if err := f.Maintenance.QueryRow(t.Context(), "SELECT current_user").Scan(&login); err != nil {
		t.Fatal(err)
	}
	if _, _, err := composeNodeReadiness(cfg, readinessPools{Readiness: background, Maintenance: []*pgxpool.Pool{f.Maintenance},
		MaintenanceLogin: login}, background, nil); err == nil {
		t.Fatal("readiness composed on the background pool")
	}
	clock := newTestClock()
	ready, verifier, err := composeNodeReadiness(cfg, readinessPools{Readiness: readinessPool, Maintenance: []*pgxpool.Pool{f.Maintenance},
		MaintenanceLogin: login}, background, nil, withClock(clock.Now))
	if err != nil || verifier == nil {
		t.Fatal(err)
	}
	if err := ready.inventory.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := refreshed(t, ready); err != nil {
		t.Fatalf("composed node not ready: %v", err)
	}
	held, err := background.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	if _, err := held.Exec(t.Context(), "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	for tick := range 3 {
		clock.Advance(authorityLoopInterval) // every tick audits
		started := time.Now()
		if err := refreshed(t, ready); err != nil {
			t.Fatalf("tick %d with the background pool held: %v", tick, err)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("tick %d waited %v for a connection", tick, elapsed)
		}
	}
}

// A busy maintenance pool is not role drift. Workers run inside
// WorkerSlots, which keep one connection free for the audit: however many
// workers wait, the audit verifies the login even when the last good
// verdict is past the grace. A pool held outside the slots (no verdict
// possible) keeps the last good verdict for 10 minutes, logged, then fails
// closed; it recovers as soon as a connection is free.
func TestBusyMaintenancePoolIsNotDrift(t *testing.T) {
	f := testpg.New(t)
	cfg := boundReadinessConfig()
	maintenance, err := database.OpenWithMax(t.Context(), f.Maintenance.Config().ConnString(), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(maintenance.Close)
	slots, err := database.NewWorkerSlots(maintenance)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	ready, clock := boundReadinessOver(t, f, cfg, f.Runtime, f.Runtime, maintenance, withLogger(slog.New(slog.NewTextHandler(&logs, nil))))

	// Three workers saturate the slots, each holding a connection while in.
	// Every held connection is released on any exit, or the pool's cleanup
	// Close would wait for it forever.
	release := make(chan struct{})
	var releaseOnce sync.Once
	var workers sync.WaitGroup
	defer func() { releaseOnce.Do(func() { close(release) }); workers.Wait() }()
	holding := make(chan struct{}, 3)
	for range 3 {
		workers.Go(func() {
			_ = slots.Do(t.Context(), func(ctx context.Context) error {
				conn, err := maintenance.Acquire(ctx)
				if err != nil {
					return err
				}
				defer conn.Release()
				holding <- struct{}{}
				<-release
				return nil
			})
		})
	}
	<-holding
	clock.Advance(11 * time.Minute) // past the grace: no last verdict to lean on
	if err := refreshed(t, ready); err != nil {
		t.Fatalf("audit with every worker slot busy: %v", err)
	}
	releaseOnce.Do(func() { close(release) })
	workers.Wait()

	// Held outside the slots: inconclusive, served from the last verdict.
	a, err := maintenance.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, err := maintenance.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Release()
	clock.Advance(authorityLoopInterval)
	if err := refreshed(t, ready); err != nil {
		t.Fatalf("inconclusive audit within the grace: %v", err)
	}
	if !strings.Contains(logs.String(), "inconclusive") || !strings.Contains(logs.String(), "maintenance database unavailable") ||
		strings.Contains(logs.String(), "isolated maintenance principal") {
		t.Fatalf("busy pool not logged as inconclusive: %s", logs.String())
	}
	clock.Advance(6 * time.Minute) // the last good verdict is 11 minutes old
	err = refreshed(t, ready)
	if !errors.Is(err, readiness.ErrInconclusive) || !strings.Contains(err.Error(), "maintenance database unavailable") {
		t.Fatalf("inconclusive audit past the grace: %v", err)
	}
	a.Release()
	b.Release()
	clock.Advance(2 * time.Second)
	if err := refreshed(t, ready); err != nil {
		t.Fatalf("maintenance pool free again: %v", err)
	}
}

// When the fast probe recovers from an outage, the request (and the other
// reset) pools drop their pooled connections, so no request meets a
// connection the outage terminated (57P01): ShouldPing skips connections
// idle under a minute.
func TestFastProbeRecoveryResetsPooledConnections(t *testing.T) {
	f := testpg.New(t)
	cfg := boundReadinessConfig()
	requests, err := database.OpenWithMax(t.Context(), f.Runtime.Config().ConnString(), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(requests.Close)
	var pid int32
	if err := requests.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	proxy := newSwitchProxy(t, f.Runtime.Config().ConnConfig.Host, f.Runtime.Config().ConnConfig.Port)
	viaProxy := f.Runtime.Config().Copy()
	viaProxy.ConnConfig.Host, viaProxy.ConnConfig.Port = "127.0.0.1", proxy.port
	viaProxy.ConnConfig.ConnectTimeout = time.Second
	pool, err := pgxpool.NewWithConfig(t.Context(), viaProxy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ready, clock := boundReadinessOver(t, f, cfg, pool, f.Runtime, f.Maintenance, withReset(requests))
	// The outage: the probe fails, and the request pool's idle connection
	// is terminated meanwhile.
	proxy.set(false)
	clock.Advance(2 * time.Second)
	if err := refreshed(t, ready); err == nil {
		t.Fatal("ready while the database is down")
	}
	if _, err := f.Admin.Exec(t.Context(), "SELECT pg_terminate_backend($1)", pid); err != nil {
		t.Fatal(err)
	}
	proxy.set(true)
	clock.Advance(2 * time.Second)
	if err := refreshed(t, ready); err != nil {
		t.Fatalf("database back: %v", err)
	}
	if _, err := requests.Exec(t.Context(), "SELECT 1"); err != nil {
		t.Fatalf("first request after recovery: %v", err)
	}
}

// The audit never computes the key inventory itself (the authority loop is
// its only computation, single flight): while it is pending the audit
// refuses and nudges the loop.
func TestAuditNudgesButNeverComputesTheInventory(t *testing.T) {
	f := testpg.New(t)
	cfg := boundReadinessConfig()
	if _, err := authority.InitServerState(t.Context(), f.Owner, cfg.ServerInstanceID, cfg.Auth.ActorIDPepper); err != nil {
		t.Fatal(err)
	}
	reads := atomic.Int32{}
	ready := readinessOver(t, cfg, f.Runtime, f.Maintenance, withInventoryRead(cfg, func(context.Context) (authority.KeyInventory, error) {
		reads.Add(1)
		return authority.KeyInventory{}, nil
	}))
	for range 3 {
		if err := refreshed(t, ready); !errors.Is(err, errKeyInventoryPending) {
			t.Fatalf("pending inventory: %v", err)
		}
	}
	if reads.Load() != 0 {
		t.Fatalf("the audit computed the inventory %d times", reads.Load())
	}
	select {
	case <-ready.inventory.nudge:
	default:
		t.Fatal("the audit did not nudge the inventory loop")
	}
	if err := ready.inventory.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := refreshed(t, ready); err != nil {
		t.Fatalf("computed inventory: %v", err)
	}
}

// Pooled connections are reset only after the fast probe failed on a lost
// connection: a probe that timed out (a slow or locked database) or a
// refusal of this node's identity leaves the pools' connections alone.
func TestFastProbeRecoveryKeepsPoolsAfterATimeoutOrARefusal(t *testing.T) {
	f := testpg.New(t)
	cfg := boundReadinessConfig()
	requests, err := database.OpenWithMax(t.Context(), f.Runtime.Config().ConnString(), 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(requests.Close)
	backend := func() int32 {
		t.Helper()
		var pid int32
		if err := requests.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&pid); err != nil {
			t.Fatal(err)
		}
		return pid
	}
	pid := backend()
	ready, clock := boundReadiness(t, f, cfg, withReset(requests))
	// A probe stuck behind a lock times out.
	tx, err := f.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), "LOCK schema_compatibility IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Second)
	if err := refreshed(t, ready); !errors.Is(err, database.ErrUnavailable) {
		t.Fatalf("locked probe: %v", err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Second)
	if err := refreshed(t, ready); err != nil {
		t.Fatalf("after the lock: %v", err)
	}
	if got := backend(); got != pid {
		t.Fatalf("a probe timeout reset the request pool: backend %d became %d", pid, got)
	}
	// An identity refusal (a tampered actor-key commitment), then restored.
	var commitment []byte
	if err := f.Owner.QueryRow(t.Context(), "SELECT actor_key_commitment FROM server_state WHERE singleton").Scan(&commitment); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Owner.Exec(t.Context(), "UPDATE server_state SET actor_key_commitment=$1 WHERE singleton", make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Second)
	if err := refreshed(t, ready); !errors.Is(err, authority.ErrActorIDKeyMismatch) {
		t.Fatalf("tampered commitment: %v", err)
	}
	if _, err := f.Owner.Exec(t.Context(), "UPDATE server_state SET actor_key_commitment=$1 WHERE singleton", commitment); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Second)
	if err := refreshed(t, ready); err != nil {
		t.Fatalf("restored commitment: %v", err)
	}
	if got := backend(); got != pid {
		t.Fatalf("an identity refusal reset the request pool: backend %d became %d", pid, got)
	}
}

// An audit step that could not reach the database reaches no verdict; any
// refusal the server answered with is a verdict naming its SQLSTATE only;
// a schema validation failure is schema_incompatible.
func TestAuditStepClassifiesTheCause(t *testing.T) {
	refused := &pgconn.PgError{Code: "42501", Message: "permission denied for table schema_migrations"}
	for _, c := range []struct {
		name         string
		err          error
		inconclusive bool
		text         string
	}{
		{"deadline", fmt.Errorf("read applied migrations: %w", context.DeadlineExceeded), true, "schema check unavailable"},
		{"admin shutdown", fmt.Errorf("acquire compatibility connection: %w", &pgconn.PgError{Code: "57P01"}), true, "schema check unavailable"},
		{"lock timeout", fmt.Errorf("check migration table: %w", &pgconn.PgError{Code: "55P03"}), true, "schema check unavailable"},
		{"revoked privilege", fmt.Errorf("read applied migrations: %w", refused), false, "schema check refused (SQLSTATE 42501)"},
		{"validation", errors.New("migration_compatibility: unknown or changed migration 31"), false, "schema_incompatible"},
	} {
		got := schemaCheck(c.err)
		if errors.Is(got, readiness.ErrInconclusive) != c.inconclusive || !errors.Is(got, database.ErrUnavailable) ||
			!strings.Contains(got.Error(), c.text) || strings.Contains(got.Error(), "permission denied") {
			t.Fatalf("%s: %v", c.name, got)
		}
	}
	if got := auditStep("runtime role check", database.Unavailable(refused)); errors.Is(got, readiness.ErrInconclusive) ||
		!strings.Contains(got.Error(), "runtime role check refused (SQLSTATE 42501)") {
		t.Fatalf("carried refusal: %v", got)
	}
	if got := auditStep("runtime role check", database.Unavailable(context.DeadlineExceeded)); !errors.Is(got, readiness.ErrInconclusive) {
		t.Fatalf("carried deadline: %v", got)
	}
}

// An audit that times out behind a lock (here on schema_migrations) reached
// no verdict: the node keeps the last good verdict within the grace, then
// fails closed, and recovers once the lock is gone.
func TestAuditTimedOutBehindALockIsInconclusive(t *testing.T) {
	f := testpg.New(t)
	cfg := boundReadinessConfig()
	ready, clock := boundReadiness(t, f, cfg)
	tx, err := f.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), "LOCK schema_migrations IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	clock.Advance(authorityLoopInterval)
	if err := refreshed(t, ready); err != nil {
		t.Fatalf("locked audit within the grace: %v", err)
	}
	clock.Advance(6 * time.Minute)
	if err := refreshed(t, ready); !errors.Is(err, readiness.ErrInconclusive) || !strings.Contains(err.Error(), "schema check unavailable") {
		t.Fatalf("locked audit past the grace: %v", err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Second)
	if err := refreshed(t, ready); err != nil {
		t.Fatalf("lock released: %v", err)
	}
}
