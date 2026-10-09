package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/config"
	"babytracker/server/internal/database"
	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/liveactivity"
	"babytracker/server/internal/migrate"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/recovery"
	"babytracker/server/internal/security"
	"babytracker/server/internal/security/localstore"
	"babytracker/server/internal/testkit"
	"babytracker/server/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type liveActivityTransportFunc func(context.Context, liveactivity.PushRequest) (liveactivity.PushResult, error)

func (f liveActivityTransportFunc) Send(ctx context.Context, req liveactivity.PushRequest) (liveactivity.PushResult, error) {
	return f(ctx, req)
}

func TestLiveActivityRuntimeRejectsDifferentDatabaseWithValidWorkerRole(t *testing.T) {
	h := testkit.New(t)
	name := fmt.Sprintf("push_wrong_database_%d", time.Now().UnixNano())
	if _, err := h.Admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" OWNER "+pgx.Identifier{h.Migrator.Config().ConnConfig.User}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := h.Admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	open := func(original *pgxpool.Pool) *pgxpool.Pool {
		cfg := original.Config().Copy()
		cfg.ConnConfig.Database = name
		p, err := pgxpool.NewWithConfig(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		return p
	}
	owner, worker := open(h.Migrator), open(h.LiveActivity)
	if err := migrate.New(migrations.Files).Apply(t.Context(), owner); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyLiveActivityRole(t.Context(), worker, 29); err != nil {
		t.Fatal("same-version standalone worker inventory must be valid", err)
	}
	var observed string
	if err := worker.QueryRow(t.Context(), "SELECT current_database()").Scan(&observed); err != nil || observed != name {
		t.Fatalf("wrong-database fixture=%q err=%v", observed, err)
	}
	workerURL, err := url.Parse(worker.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	workerURL.Path = "/" + name
	logs := &supervisionLog{}
	options := httpapi.Options{InstanceID: h.Info.InstanceID, RecoveryEpoch: 1, FamilyService: h.Family, Readiness: readinessFunc(func(context.Context) error { return nil }), Logger: slog.New(slog.NewTextHandler(logs, nil))}
	cfg := config.Config{LiveActivity: config.LiveActivityConfig{Enabled: true, BundleID: "com.example.babytracker", WorkerDatabaseURL: workerURL.String()}}
	// The worker never activates against another database: the process keeps
	// running with push unavailable and supervision retrying, never sending.
	shutdown, err := configureLiveActivityWorkerWithWait(t.Context(), cfg, h.Security, h.Runtime, &options, newDatabasePoolBudget(h.Runtime), liveActivityTransportFunc(func(context.Context, liveactivity.PushRequest) (liveactivity.PushResult, error) {
		t.Fatal("wrong database sent push")
		return liveactivity.PushResult{}, nil
	}), func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() })
	if err != nil {
		t.Fatalf("wrong database exited the process: %v", err)
	}
	t.Cleanup(shutdown)
	assertLiveActivityDeferred(t, options, logs, "database_role_separation_invalid")
}

// assertLiveActivityDeferred: activation was deferred to supervision with the
// given code; push is unavailable and nothing is advertised meanwhile.
func assertLiveActivityDeferred(t *testing.T, options httpapi.Options, logs *supervisionLog, code string) {
	t.Helper()
	state, ok := options.LiveActivityReadiness.(*liveActivityRuntime)
	if !ok || state.running.Load() || state.Check(t.Context()) == nil {
		t.Fatalf("deferred activation reported a running worker: %+v", options.LiveActivityReadiness)
	}
	assertLiveActivityCapability(t, options, false)
	out := logs.String()
	if !strings.Contains(out, `msg="live activity worker activation deferred"`) || !strings.Contains(out, "code="+code) {
		t.Fatalf("deferred activation log=%s", out)
	}
}

func TestLiveActivityRuntimeRequiresRecoveryReadiness(t *testing.T) {
	h := testkit.New(t)
	root := t.TempDir()
	stores, err := localstore.Open(filepath.Join(root, "ledger"), filepath.Join(root, "head"))
	if err != nil {
		t.Fatal(err)
	}
	durable, err := recovery.NewVerifiedStore(filepath.Join(root, "head"), stores.Fence, stores.Ledger, stores.Heads)
	if err != nil {
		t.Fatal(err)
	}
	var originalKey, badKey [32]byte
	originalKey[0] = 1
	badKey[0] = 2
	if err = durable.InitializeAuthorityWithActorIDKey(t.Context(), stores, recovery.State{InstanceID: h.Info.InstanceID, Epoch: 1, TrustedHead: security.Head{Generation: 1}}, originalKey); err != nil {
		t.Fatal(err)
	}
	mirror, err := recovery.NewPostgresMirror(h.Runtime, h.Migrator)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := recovery.NewGate(recovery.Options{Store: durable, Security: h.Security, Schema: readinessFunc(func(context.Context) error { return nil }), Mirror: mirror, ExpectedInstanceID: h.Info.InstanceID, ExpectedEpoch: 1, ActorIDKey: badKey})
	if err != nil {
		t.Fatal(err)
	}
	if err = gate.Check(t.Context()); !errors.Is(err, recovery.ErrQuarantine) {
		t.Fatalf("actor-key mismatch not quarantined: %v", err)
	}
	cfg := config.Config{LiveActivity: config.LiveActivityConfig{Enabled: true, BundleID: "com.example.babytracker", WorkerDatabaseURL: h.LiveActivity.Config().ConnString()}}
	// No readiness dependency at all is a composition fault and fails fast.
	options := httpapi.Options{FamilyService: h.Family}
	shutdown, err := configureLiveActivityWorker(t.Context(), cfg, h.Security, h.Runtime, &options, newDatabasePoolBudget(h.Runtime), liveActivityTransportFunc(func(context.Context, liveactivity.PushRequest) (liveactivity.PushResult, error) {
		t.Fatal("missing readiness sent push")
		return liveactivity.PushResult{}, nil
	}))
	if shutdown != nil {
		shutdown()
	}
	if err == nil || options.LiveActivityService != nil {
		t.Fatalf("missing readiness activation err=%v", err)
	}
	// A quarantined recovery gate at boot defers activation: the process stays
	// up with push unavailable, nothing is sent, and registration is refused.
	logs := &supervisionLog{}
	options = httpapi.Options{InstanceID: h.Info.InstanceID, RecoveryEpoch: 1, FamilyService: h.Family, Readiness: gate, Logger: slog.New(slog.NewTextHandler(logs, nil))}
	var sends atomic.Int32
	shutdown, err = configureLiveActivityWorkerWithWait(t.Context(), cfg, h.Security, h.Runtime, &options, newDatabasePoolBudget(h.Runtime), liveActivityTransportFunc(func(context.Context, liveactivity.PushRequest) (liveactivity.PushResult, error) {
		sends.Add(1)
		return liveactivity.PushResult{Classification: liveactivity.Accepted}, nil
	}), func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() })
	if err != nil {
		t.Fatalf("quarantined boot exited the process: %v", err)
	}
	t.Cleanup(shutdown)
	assertLiveActivityDeferred(t, options, logs, "dependency_unavailable")
	req := protocol.LiveActivityDeviceRegistration{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityLiveActivityPushV1}}, InstallationID: "50000000-0000-4000-8000-000000000900", RegistrationVersion: 1, APNsEnvironment: "sandbox", PayloadVersion: 1, PushToStartToken: "aabb", FeedBreakMinutes: 30}
	body, _ := json.Marshal(req)
	options.SessionService = h.Auth
	r := httptest.NewRequest(http.MethodPut, "/v1/live-activities/device", bytes.NewReader(body))
	r.Header.Set("X-Baby-Tracker-Protocol", "v1")
	r.Header.Set("X-Baby-Tracker-Capabilities", protocol.CapabilityLiveActivityPushV1)
	r.Header.Set("Authorization", "Bearer "+h.AccessToken("caregiver"))
	w := httptest.NewRecorder()
	httpapi.New(options).ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || sends.Load() != 0 {
		t.Fatalf("quarantined boot registration=%d sends=%d body=%s", w.Code, sends.Load(), w.Body.String())
	}
}

// Enabled-mode boot while application readiness is not yet ready: the process
// starts with push unavailable (as a disabled server would) and supervision
// activates it once readiness passes, without a restart.
func TestLiveActivityRuntimeDefersActivationUntilReady(t *testing.T) {
	h, cfg, options := liveActivitySupervisionFixture(t)
	var unready atomic.Bool
	unready.Store(true)
	options.Readiness = readinessFunc(func(context.Context) error {
		if unready.Load() {
			return recovery.ErrQuarantine
		}
		return nil
	})
	logs := &supervisionLog{}
	options.Logger = slog.New(slog.NewTextHandler(logs, nil))
	waits := make(chan time.Duration, 8)
	permit := make(chan struct{})
	wait := func(ctx context.Context, d time.Duration) error {
		waits <- d
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-permit:
			return nil
		}
	}
	sent := make(chan liveactivity.PushRequest, 8)
	shutdown, err := configureLiveActivityWorkerWithWait(t.Context(), cfg, h.Security, h.Runtime, &options, newDatabasePoolBudget(h.Runtime), liveActivityTransportFunc(func(_ context.Context, r liveactivity.PushRequest) (liveactivity.PushResult, error) {
		sent <- r
		return liveactivity.PushResult{Classification: liveactivity.Accepted}, nil
	}), wait)
	if err != nil {
		t.Fatalf("unready boot exited the process: %v", err)
	}
	t.Cleanup(shutdown)
	assertLiveActivityDeferred(t, options, logs, "dependency_unavailable")
	// Supervision backs off exactly as after a stop, and does not activate
	// while readiness still fails.
	for i, seconds := range []int{2, 4} {
		select {
		case d := <-waits:
			if d != time.Duration(seconds)*time.Second {
				t.Fatalf("backoff%d=%v want %ds", i, d, seconds)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("backoff%d absent", i)
		}
		if options.LiveActivityReadiness.(*liveActivityRuntime).running.Load() {
			t.Fatal("unready application activated push")
		}
		permit <- struct{}{}
	}
	if out := h.StartFeed(t, h.Owner, testkit.TimerA); out.Status != "applied" {
		t.Fatalf("queue timer=%s", out.Status)
	}
	select {
	case <-waits:
	case <-time.After(3 * time.Second):
		t.Fatal("third backoff absent")
	}
	unready.Store(false)
	permit <- struct{}{}
	select {
	case request := <-sent:
		if request.Event != "start" {
			t.Fatalf("activated event=%s", request.Event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("push did not activate once readiness passed")
	}
	state := options.LiveActivityReadiness.(*liveActivityRuntime)
	deadline := time.Now().Add(time.Second)
	for !state.running.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	assertLiveActivityCapability(t, options, true)
	out := logs.String()
	if strings.Count(out, `msg="live activity worker resume failed"`) != 2 || !strings.Contains(out, `msg="live activity worker resumed"`) || strings.Contains(out, recovery.ErrQuarantine.Error()) {
		t.Fatalf("deferred activation log=%s", out)
	}
}

func TestLiveActivityRuntimeLaterRecoveryFailurePreventsSend(t *testing.T) {
	h := testkit.New(t)
	head, err := h.Security.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Migrator.Exec(t.Context(), `SELECT app.update_server_state($1,$2,$3,$4,$5)`, h.Info.InstanceID, h.Info.RecoveryEpoch, head.Generation, head.Revision, head.EventHash[:]); err != nil {
		t.Fatal(err)
	}
	m := testkit.MustMutation(t, "baby.json")
	if out, e := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m); e != nil || out.Status != "applied" {
		t.Fatalf("seed baby: %v %s", e, out.Status)
	}
	var failed atomic.Bool
	var sends atomic.Int32
	options := httpapi.Options{FamilyService: h.Family, Readiness: readinessFunc(func(context.Context) error {
		if failed.Load() {
			return recovery.ErrQuarantine
		}
		return nil
	})}
	cfg := config.Config{LiveActivity: config.LiveActivityConfig{Enabled: true, BundleID: "com.example.babytracker", WorkerDatabaseURL: h.LiveActivity.Config().ConnString()}}
	shutdown, err := configureLiveActivityWorker(t.Context(), cfg, h.Security, h.Runtime, &options, newDatabasePoolBudget(h.Runtime), liveActivityTransportFunc(func(context.Context, liveactivity.PushRequest) (liveactivity.PushResult, error) {
		sends.Add(1)
		return liveactivity.PushResult{Classification: liveactivity.Accepted}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(shutdown)
	failed.Store(true)
	req := protocol.LiveActivityDeviceRegistration{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityLiveActivityPushV1}}, InstallationID: "50000000-0000-4000-8000-000000000900", RegistrationVersion: 1, APNsEnvironment: "sandbox", PayloadVersion: 1, PushToStartToken: "aabb", FeedBreakMinutes: 30}
	if _, err = options.LiveActivityService.RegisterDevice(t.Context(), h.Caregiver, req); err != nil {
		t.Fatal(err)
	}
	if out := h.StartFeed(t, h.Owner, testkit.TimerA); out.Status != "applied" {
		t.Fatalf("start=%s", out.Status)
	}
	state := options.LiveActivityReadiness.(*liveActivityRuntime)
	deadline := time.Now().Add(3 * time.Second)
	for state.running.Load() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if state.running.Load() || sends.Load() != 0 {
		t.Fatalf("readiness failure allowed worker: running=%t sends=%d", state.running.Load(), sends.Load())
	}
}

func liveActivitySupervisionFixture(t *testing.T) (*testkit.Harness, config.Config, httpapi.Options) {
	t.Helper()
	h := testkit.New(t)
	head, err := h.Security.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Migrator.Exec(t.Context(), `SELECT app.update_server_state($1,$2,$3,$4,$5)`, h.Info.InstanceID, h.Info.RecoveryEpoch, head.Generation, head.Revision, head.EventHash[:]); err != nil {
		t.Fatal(err)
	}
	baby := testkit.MustMutation(t, "baby.json")
	if out, e := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, baby); e != nil || out.Status != "applied" {
		t.Fatalf("seed: %v %s", e, out.Status)
	}
	req := protocol.LiveActivityDeviceRegistration{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityLiveActivityPushV1}}, InstallationID: "50000000-0000-4000-8000-000000000900", RegistrationVersion: 1, APNsEnvironment: "sandbox", PayloadVersion: 1, PushToStartToken: "aabb", FeedBreakMinutes: 30}
	if _, err = liveactivity.NewService(h.Security, syncFamilyResolver{h.Family}, "com.example.babytracker.push-type.liveactivity").RegisterDevice(t.Context(), h.Caregiver, req); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Mode: config.ModeSelfHosted, LiveActivity: config.LiveActivityConfig{Enabled: true, BundleID: "com.example.babytracker", WorkerDatabaseURL: h.LiveActivity.Config().ConnString()}}
	options := httpapi.Options{InstanceID: h.Info.InstanceID, RecoveryEpoch: 1, FamilyService: h.Family, Readiness: readinessFunc(func(context.Context) error { return nil })}
	return h, cfg, options
}

func TestLiveActivityRuntimeSupervisionRestoresQueuedDelivery(t *testing.T) {
	h, cfg, options := liveActivitySupervisionFixture(t)
	waits := make(chan time.Duration, 8)
	permit := make(chan struct{})
	sent := make(chan liveactivity.PushRequest, 8)
	wait := func(ctx context.Context, d time.Duration) error {
		waits <- d
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-permit:
			return nil
		}
	}
	shutdown, err := configureLiveActivityWorkerWithWait(t.Context(), cfg, h.Security, h.Runtime, &options, newDatabasePoolBudget(h.Runtime), liveActivityTransportFunc(func(_ context.Context, r liveactivity.PushRequest) (liveactivity.PushResult, error) {
		sent <- r
		return liveactivity.PushResult{Classification: liveactivity.Accepted}, nil
	}), wait)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(shutdown)
	// Let the fixture's two no-card jobs settle before injecting an outage.
	// Cancelling a claimed job correctly retains its 30-second durable lease;
	// that separate recovery boundary must not race this role-restoration check.
	deadline := time.Now().Add(3 * time.Second)
	var jobs, pending int
	for time.Now().Before(deadline) {
		if err = h.Migrator.QueryRow(t.Context(), `SELECT count(*),count(*) FILTER (WHERE lease_id IS NOT NULL OR dirty_version>completed_version) FROM live_activity_delivery WHERE device_id=$1`, h.Caregiver.DeviceID).Scan(&jobs, &pending); err != nil {
			t.Fatal(err)
		}
		if jobs == 2 && pending == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if jobs != 2 || pending != 0 {
		t.Fatalf("initial no-card jobs=%d pending=%d", jobs, pending)
	}
	if _, err = h.Migrator.Exec(t.Context(), `REVOKE EXECUTE ON FUNCTION app.claim_live_activity_work(timestamptz,integer) FROM baby_tracker_live_activity`); err != nil {
		t.Fatal(err)
	}
	restore := func() {
		if _, e := h.Migrator.Exec(context.Background(), `GRANT EXECUTE ON FUNCTION app.claim_live_activity_work(timestamptz,integer) TO baby_tracker_live_activity`); e != nil {
			t.Error(e)
		}
	}
	t.Cleanup(restore)
	select {
	case d := <-waits:
		if d != 2*time.Second {
			t.Fatalf("first backoff=%v", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("failed worker was not supervised")
	}
	assertLiveActivityCapability(t, options, false)
	if out := h.StartFeed(t, h.Owner, testkit.TimerA); out.Status != "applied" {
		t.Fatalf("queue timer=%s", out.Status)
	}
	restore()
	permit <- struct{}{}
	select {
	case request := <-sent:
		if request.Event != "start" {
			t.Fatalf("recovered event=%s", request.Event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued delivery did not resume")
	}
	state := options.LiveActivityReadiness.(*liveActivityRuntime)
	deadline = time.Now().Add(time.Second)
	for !state.running.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	assertLiveActivityCapability(t, options, true)
	shutdown()
	if err = state.pool.Ping(t.Context()); err == nil {
		t.Fatal("shutdown retained worker pool")
	}
}

func TestLiveActivityRuntimeSupervisionBackoffCancellation(t *testing.T) {
	h, cfg, options := liveActivitySupervisionFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	entered := make(chan time.Duration, 1)
	wait := func(ctx context.Context, d time.Duration) error { entered <- d; <-ctx.Done(); return ctx.Err() }
	shutdown, err := configureLiveActivityWorkerWithWait(ctx, cfg, h.Security, h.Runtime, &options, newDatabasePoolBudget(h.Runtime), liveActivityTransportFunc(func(context.Context, liveactivity.PushRequest) (liveactivity.PushResult, error) {
		t.Fatal("empty care sent push")
		return liveactivity.PushResult{}, nil
	}), wait)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(shutdown)
	if _, err = h.Migrator.Exec(t.Context(), `REVOKE EXECUTE ON FUNCTION app.claim_live_activity_work(timestamptz,integer) FROM baby_tracker_live_activity`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, e := h.Migrator.Exec(context.Background(), `GRANT EXECUTE ON FUNCTION app.claim_live_activity_work(timestamptz,integer) TO baby_tracker_live_activity`); e != nil {
			t.Error(e)
		}
	})
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("backoff did not start")
	}
	cancel()
	done := make(chan struct{})
	go func() { shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel and join backoff")
	}
	state := options.LiveActivityReadiness.(*liveActivityRuntime)
	if state.running.Load() {
		t.Fatal("cancelled worker ready")
	}
	if err = state.pool.Ping(t.Context()); err == nil {
		t.Fatal("cancelled worker pool open")
	}
}

func TestLiveActivityRuntimeSupervisionPersistentReadinessBacksOff(t *testing.T) {
	h, cfg, options := liveActivitySupervisionFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	var failed atomic.Bool
	var sends atomic.Int32
	options.Readiness = readinessFunc(func(context.Context) error {
		if failed.Load() {
			return recovery.ErrQuarantine
		}
		return nil
	})
	logs := &supervisionLog{}
	options.Logger = slog.New(slog.NewTextHandler(logs, nil))
	waits := make(chan time.Duration, 8)
	permit := make(chan struct{})
	wait := func(ctx context.Context, d time.Duration) error {
		waits <- d
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-permit:
			return nil
		}
	}
	shutdown, err := configureLiveActivityWorkerWithWait(ctx, cfg, h.Security, h.Runtime, &options, newDatabasePoolBudget(h.Runtime), liveActivityTransportFunc(func(context.Context, liveactivity.PushRequest) (liveactivity.PushResult, error) {
		sends.Add(1)
		return liveactivity.PushResult{Classification: liveactivity.Accepted}, nil
	}), wait)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(shutdown)
	failed.Store(true)
	if out := h.StartFeed(t, h.Owner, testkit.TimerA); out.Status != "applied" {
		t.Fatalf("queue=%s", out.Status)
	}
	for i, seconds := range []int{2, 4, 8, 16, 32, 60, 60} {
		select {
		case d := <-waits:
			if d != time.Duration(seconds)*time.Second {
				t.Fatalf("backoff%d=%v want%ds", i, d, seconds)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("backoff%d absent", i)
		}
		if options.LiveActivityReadiness.(*liveActivityRuntime).running.Load() || sends.Load() != 0 {
			t.Fatal("invalid readiness resumed delivery")
		}
		if i < 6 {
			permit <- struct{}{}
		}
	}
	cancel()
	shutdown()
	if sends.Load() != 0 {
		t.Fatal("invalid readiness sent push")
	}
	// The stop and every failed resume are logged with a code, never the
	// underlying message.
	out := logs.String()
	if !strings.Contains(out, `msg="live activity worker stopped"`) || strings.Count(out, `msg="live activity worker resume failed"`) < 6 ||
		strings.Count(out, "code=dependency_unavailable") < 7 || strings.Contains(out, recovery.ErrQuarantine.Error()) {
		t.Fatalf("supervision log=%s", out)
	}
}

type supervisionLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *supervisionLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *supervisionLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// Request-path readiness must stay off the 2-connection worker pool and the
// runtime pool: /v1/server is unauthenticated, and a burst of checks must not
// starve worker claims. Full local revalidation is startup/supervision only.
func TestLiveActivityRuntimeRequestChecksStayOffWorkerPool(t *testing.T) {
	h := testkit.New(t)
	open := func(original *pgxpool.Pool) *pgxpool.Pool {
		p, err := pgxpool.NewWithConfig(t.Context(), original.Config().Copy())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		return p
	}
	workerPool, runtimePool := open(h.LiveActivity), open(h.Runtime)
	compatibility, err := migrate.EmbeddedCompatibility()
	if err != nil {
		t.Fatal(err)
	}
	var applicationChecks atomic.Int32
	application := readinessFunc(func(context.Context) error { applicationChecks.Add(1); return nil })
	state := &liveActivityRuntime{pool: workerPool, runtime: runtimePool, runner: migrate.New(migrations.Files), compatibility: compatibility, budget: newDatabasePoolBudget(runtimePool), readiness: application}
	// The supervision-only revalidation is valid for these pools, so any request
	// path that still ran it would succeed and be observed only by pool use.
	if err = state.checkLocal(t.Context()); err != nil {
		t.Fatal(err)
	}
	workerAcquires, runtimeAcquires := workerPool.Stat().AcquireCount(), runtimePool.Stat().AcquireCount()
	state.running.Store(true)
	options := httpapi.Options{InstanceID: h.Info.InstanceID, RecoveryEpoch: 1, FamilyService: h.Family, Readiness: application,
		LiveActivityService: liveactivity.NewService(h.Security, syncFamilyResolver{h.Family}, "com.example.babytracker.push-type.liveactivity"), LiveActivityReadiness: state,
		Capabilities: append(protocol.ServerCapabilities(), protocol.CapabilityLiveActivityPushV1)}
	const requests = 20
	for i := 0; i < requests; i++ {
		assertLiveActivityCapability(t, options, true)
	}
	if err = state.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	state.running.Store(false)
	assertLiveActivityCapability(t, options, false)
	if err = state.Check(t.Context()); err == nil {
		t.Fatal("stopped worker reported ready")
	}
	if w, r := workerPool.Stat().AcquireCount()-workerAcquires, runtimePool.Stat().AcquireCount()-runtimeAcquires; w != 0 || r != 0 {
		t.Fatalf("request-path readiness acquired worker=%d runtime=%d connections", w, r)
	}
	// One application readiness check per /v1/server, none from state.Check.
	if got := applicationChecks.Load(); got > requests+1 {
		t.Fatalf("application readiness checks=%d for %d server requests", got, requests+1)
	}
}

// APNs latency must not consume the local-validation budget or fail activation.
// Slow local readiness (three guard checks before the send) plus a send that
// hangs until its own timeout exceeds a single shared 10-second startup budget.
func TestLiveActivityRuntimeStartupSurvivesHangingAPNs(t *testing.T) {
	h, cfg, options := liveActivitySupervisionFixture(t)
	var slow atomic.Bool
	slow.Store(true)
	options.Readiness = readinessFunc(func(ctx context.Context) error {
		if !slow.Load() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
			return nil
		}
	})
	if out := h.StartFeed(t, h.Owner, testkit.TimerA); out.Status != "applied" {
		t.Fatalf("queue timer=%s", out.Status)
	}
	// The startup pass claims one job ordered by not_before; put the feed first so
	// it, rather than the registration's sibling no-card job, reaches transport.
	tag, err := h.Migrator.Exec(t.Context(), `UPDATE live_activity_delivery SET not_before=now()-interval '1 hour' WHERE device_id=$1 AND kind='feed'`, h.Caregiver.DeviceID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("prioritize feed rows=%d err=%v", tag.RowsAffected(), err)
	}
	var sends atomic.Int32
	transport := liveActivityTransportFunc(func(ctx context.Context, r liveactivity.PushRequest) (liveactivity.PushResult, error) {
		if sends.Add(1) == 1 {
			<-ctx.Done()
			return liveactivity.PushResult{}, ctx.Err()
		}
		return liveactivity.PushResult{Classification: liveactivity.Accepted}, nil
	})
	started := time.Now()
	shutdown, err := configureLiveActivityWorkerWithWait(t.Context(), cfg, h.Security, h.Runtime, &options, newDatabasePoolBudget(h.Runtime), transport, func(ctx context.Context, _ time.Duration) error {
		<-ctx.Done()
		return ctx.Err()
	})
	slow.Store(false)
	if shutdown != nil {
		t.Cleanup(shutdown)
	}
	if err != nil {
		t.Fatalf("hanging APNs failed activation after %v: %v", time.Since(started).Round(time.Millisecond), err)
	}
	if sends.Load() == 0 {
		t.Fatal("startup pass did not reach transport")
	}
	assertLiveActivityCapability(t, options, true)
	// The ambiguous start outcome was durably recorded by the startup pass.
	var pending bool
	if err = h.Migrator.QueryRow(t.Context(), `SELECT dirty_version>completed_version FROM live_activity_delivery WHERE device_id=$1 AND kind='feed'`, h.Caregiver.DeviceID).Scan(&pending); err != nil || pending {
		t.Fatalf("ambiguous start not recorded pending=%t err=%v", pending, err)
	}
}

func TestLiveActivityRuntimeDisabledDoesNotOpenPool(t *testing.T) {
	options := httpapi.Options{InstanceID: "50000000-0000-4000-8000-000000000001", RecoveryEpoch: 1, Capabilities: []string{protocol.CapabilityLiveActivityPushV1}}
	shutdown, err := configureLiveActivities(t.Context(), config.Config{LiveActivity: config.LiveActivityConfig{WorkerDatabaseURL: "invalid"}}, nil, nil, &options, databasePoolBudget{})
	if err != nil {
		t.Fatal(err)
	}
	shutdown()
	if options.LiveActivityService != nil || options.LiveActivityReadiness != nil {
		t.Fatal("disabled configuration constructed push services")
	}
	assertLiveActivityCapability(t, options, false)
}

func TestLiveActivityPoolBudgetDeduplicatesSharedPools(t *testing.T) {
	open := func(max string) *pgxpool.Pool {
		p, e := pgxpool.New(t.Context(), "postgres://unused@127.0.0.1/db?pool_max_conns="+max)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(p.Close)
		return p
	}
	runtime, projector, export := open("5"), open("4"), open("3")
	budget := newDatabasePoolBudget(runtime, projector, export, export, nil)
	if got := budget.maximum(); got != 12 {
		t.Fatalf("unique maximum=%d want12", got)
	}
	if err := budget.validate(14, 0, 0); err != nil {
		t.Fatal(err)
	}
	for _, reserved := range [][2]int64{{1, 0}, {0, 1}, {1, 1}} {
		if err := budget.validate(14, reserved[0], reserved[1]); err == nil {
			t.Fatal("worker +2 or reserved connections omitted")
		}
	}
}

// Malformed configuration or composition — an unreadable key, a worker URL
// that does not parse, a missing dependency — fails startup. A database that
// is not ready for push — schema still below 28, wrong worker privileges, a
// connection budget shortfall — defers activation to supervision instead:
// the process serves, push stays unavailable and unadvertised, nothing is sent.
func TestLiveActivityRuntimeConfigurationFailsFastDatabaseChecksDefer(t *testing.T) {
	for _, kind := range []string{"key", "worker_url", "family_service", "readiness", "schema", "role", "budget"} {
		t.Run(kind, func(t *testing.T) {
			h := testkit.New(t)
			if kind == "schema" {
				h = testkit.NewThrough(t, 27)
			}
			cfg := config.Config{Mode: config.ModeSelfHosted, LiveActivity: config.LiveActivityConfig{Enabled: true, BundleID: "com.example.babytracker", WorkerDatabaseURL: h.LiveActivity.Config().ConnString(), KeyFile: t.TempDir() + "/missing"}}
			logs := &supervisionLog{}
			options := httpapi.Options{InstanceID: "50000000-0000-4000-8000-000000000001", RecoveryEpoch: 1, FamilyService: h.Family, Readiness: readinessFunc(func(context.Context) error { return nil }), Logger: slog.New(slog.NewTextHandler(logs, nil))}
			budget := newDatabasePoolBudget(h.Runtime)
			switch kind {
			case "worker_url":
				cfg.LiveActivity.WorkerDatabaseURL = "not a url"
			case "family_service":
				options.FamilyService = nil
			case "readiness":
				options.Readiness = nil
			case "role":
				cfg.LiveActivity.WorkerDatabaseURL = h.Runtime.Config().ConnString()
			case "budget":
				p, e := pgxpool.New(t.Context(), "postgres://unused@127.0.0.1/db?pool_max_conns=1000")
				if e != nil {
					t.Fatal(e)
				}
				t.Cleanup(p.Close)
				budget = newDatabasePoolBudget(h.Runtime, p)
			}
			var shutdown func()
			var err error
			transport := liveActivityTransportFunc(func(context.Context, liveactivity.PushRequest) (liveactivity.PushResult, error) {
				t.Fatal("unready startup sent APNs")
				return liveactivity.PushResult{}, nil
			})
			if kind == "key" {
				shutdown, err = configureLiveActivities(t.Context(), cfg, h.Security, h.Runtime, &options, budget)
			} else {
				shutdown, err = configureLiveActivityWorkerWithWait(t.Context(), cfg, h.Security, h.Runtime, &options, budget, transport, func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() })
			}
			if shutdown != nil {
				t.Cleanup(shutdown)
			}
			if failFast := map[string]string{"key": "BT_APNS_KEY_FILE", "worker_url": database.ErrOpen.Error(), "family_service": "live_activity_family_service_required", "readiness": "live_activity_application_readiness_required"}[kind]; failFast != "" {
				if err == nil || !strings.Contains(err.Error(), failFast) {
					t.Fatalf("%s startup failure=%v want %s", kind, err, failFast)
				}
				if options.LiveActivityService != nil || options.LiveActivityReadiness != nil {
					t.Fatal("failed startup exposed service")
				}
				assertLiveActivityCapability(t, options, false)
				return
			}
			if err != nil {
				t.Fatalf("%s exited the process instead of deferring: %v", kind, err)
			}
			assertLiveActivityDeferred(t, options, logs, map[string]string{"schema": "live_activity_schema_unavailable", "role": "database_role_separation_invalid", "budget": "live_activity_database_budget_exceeded"}[kind])
			shutdown()
			if e := options.LiveActivityReadiness.(*liveActivityRuntime).pool.Ping(t.Context()); e == nil {
				t.Fatal("shutdown of a deferred worker kept its pool")
			}
		})
	}
}

func TestLiveActivityRuntimeSelfHostedOutageAndShutdown(t *testing.T) {
	h := testkit.New(t)
	head, err := h.Security.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Migrator.Exec(t.Context(), `SELECT app.update_server_state($1,$2,$3,$4,$5)`, h.Info.InstanceID, h.Info.RecoveryEpoch, head.Generation, head.Revision, head.EventHash[:]); err != nil {
		t.Fatal(err)
	}
	// Seed the canonical baby ID used by the timer fixture.
	m := testkit.MustMutation(t, "baby.json")
	m.MutationID = h.CommandID()
	if out, e := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m); e != nil || out.Status != "applied" {
		t.Fatalf("seed baby: %v %s", e, out.Status)
	}
	options := httpapi.Options{InstanceID: h.Info.InstanceID, RecoveryEpoch: 1, AppleAuthorizationMode: "identity_token_only", FamilyService: h.Family, Readiness: readinessFunc(func(context.Context) error { return nil })}
	cfg := config.Config{Mode: config.ModeSelfHosted, LiveActivity: config.LiveActivityConfig{Enabled: true, BundleID: "com.example.babytracker", WorkerDatabaseURL: h.LiveActivity.Config().ConnString() + "&pool_max_conns=99"}}
	var sends atomic.Int32
	transport := liveActivityTransportFunc(func(context.Context, liveactivity.PushRequest) (liveactivity.PushResult, error) {
		sends.Add(1)
		return liveactivity.PushResult{Classification: liveactivity.Retryable, HTTPStatus: 503}, errors.New("temporary APNs outage")
	})
	shutdown, err := configureLiveActivityWorker(t.Context(), cfg, h.Security, h.Runtime, &options, newDatabasePoolBudget(h.Runtime), transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(shutdown)
	readiness := options.LiveActivityReadiness.(*liveActivityRuntime)
	if got := readiness.pool.Config().MaxConns; got != 2 {
		t.Fatalf("worker maximum=%d", got)
	}
	for _, table := range []string{"babies", "active_timers", "live_activity_devices"} {
		if _, e := readiness.pool.Exec(t.Context(), "SELECT * FROM "+table); e == nil {
			t.Fatalf("worker can read %s", table)
		}
	}
	assertLiveActivityCapability(t, options, true)
	req := protocol.LiveActivityDeviceRegistration{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityLiveActivityPushV1}}, InstallationID: "50000000-0000-4000-8000-000000000900", RegistrationVersion: 1, APNsEnvironment: "sandbox", PayloadVersion: 1, PushToStartToken: "aabb", FeedBreakMinutes: 30}
	_, e := options.LiveActivityService.RegisterDevice(t.Context(), h.Caregiver, req)
	if e != nil {
		t.Fatal(e)
	}
	if out := h.StartFeed(t, h.Owner, testkit.TimerA); out.Status != "applied" {
		t.Fatalf("start=%s", out.Status)
	}
	deadline := time.Now().Add(5 * time.Second)
	for sends.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if sends.Load() == 0 {
		t.Fatal("worker did not send")
	}
	// Definitive 503 rejection is durably deferred, never hot-looped.
	var attempts int
	var retry *time.Time
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		e = h.Migrator.QueryRow(t.Context(), `SELECT (target_state->>'attempts')::integer,(target_state->>'retry_at')::timestamptz FROM live_activity_delivery WHERE device_id=$1 AND kind='feed'`, h.Caregiver.DeviceID).Scan(&attempts, &retry)
		if e == nil && retry != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e != nil || attempts != 1 || retry == nil || !retry.After(time.Now()) {
		t.Fatalf("backoff attempts=%d retry=%v err=%v", attempts, retry, e)
	}
	assertLiveActivityCapability(t, options, true)
	recorder := httptest.NewRecorder()
	httpapi.New(options).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != 200 {
		t.Fatalf("APNs outage broke sync readiness: %d", recorder.Code)
	}
	if _, e = h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(nil, nil)); e != nil {
		t.Fatal("APNs outage broke sync", e)
	}
	shutdown()
	shutdown()
	if e = readiness.Check(t.Context()); e == nil {
		t.Fatal("shutdown kept worker ready")
	}
	if e = readiness.pool.Ping(t.Context()); e == nil {
		t.Fatal("shutdown did not close pool")
	}
	assertLiveActivityCapability(t, options, false)
}

func TestLiveActivityRuntimeInfrastructureFailureWithdrawsPushOnly(t *testing.T) {
	h := testkit.New(t)
	options := httpapi.Options{InstanceID: h.Info.InstanceID, RecoveryEpoch: 1, FamilyService: h.Family, Readiness: readinessFunc(func(context.Context) error { return nil })}
	cfg := config.Config{LiveActivity: config.LiveActivityConfig{Enabled: true, BundleID: "com.example.babytracker", WorkerDatabaseURL: h.LiveActivity.Config().ConnString()}}
	shutdown, err := configureLiveActivityWorker(t.Context(), cfg, h.Security, h.Runtime, &options, newDatabasePoolBudget(h.Runtime), liveActivityTransportFunc(func(context.Context, liveactivity.PushRequest) (liveactivity.PushResult, error) {
		t.Fatal("empty family sent push")
		return liveactivity.PushResult{}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(shutdown)
	state := options.LiveActivityReadiness.(*liveActivityRuntime)
	if _, err = h.Migrator.Exec(t.Context(), `REVOKE EXECUTE ON FUNCTION app.claim_live_activity_work(timestamptz,integer) FROM baby_tracker_live_activity`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, e := h.Migrator.Exec(context.Background(), `GRANT EXECUTE ON FUNCTION app.claim_live_activity_work(timestamptz,integer) TO baby_tracker_live_activity`); e != nil {
			t.Error(e)
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for state.running.Load() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if state.running.Load() {
		t.Fatal("failed worker did not exit and join")
	}
	assertLiveActivityCapability(t, options, false)
	r := httptest.NewRecorder()
	httpapi.New(options).ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if r.Code != 200 {
		t.Fatalf("worker failure blocked sync readiness %d", r.Code)
	}
	if _, err = h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(nil, nil)); err != nil {
		t.Fatal("worker failure blocked sync", err)
	}
}

func assertLiveActivityCapability(t *testing.T, options httpapi.Options, want bool) {
	t.Helper()
	r := httptest.NewRecorder()
	httpapi.New(options).ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/v1/server", nil))
	if r.Code != 200 {
		t.Fatalf("server status=%d body=%s", r.Code, r.Body.String())
	}
	var info protocol.ServerInfo
	if e := json.Unmarshal(r.Body.Bytes(), &info); e != nil {
		t.Fatal(e)
	}
	got := false
	for _, capability := range info.Capabilities {
		if capability == protocol.CapabilityLiveActivityPushV1 {
			got = true
		}
	}
	if got != want {
		t.Fatalf("push capability=%t want%t body=%s", got, want, strings.TrimSpace(r.Body.String()))
	}
}

func TestLiveActivityComposeOptionalSecrets(t *testing.T) {
	root := t.TempDir()
	for _, enabled := range []bool{false, true} {
		args := []string{"compose", "-f", "../../compose.yaml", "-f", "../../deploy/unraid/compose.yaml"}
		if enabled {
			args = append(args, "-f", "../../deploy/unraid/live-activity-push.yaml")
		}
		args = append(args, "config", "--format", "json")
		command := exec.Command("docker", args...)
		for _, e := range os.Environ() {
			if !strings.HasPrefix(e, "BT_") {
				command.Env = append(command.Env, e)
			}
		}
		command.Env = append(command.Env, "BT_DEPLOY_ROOT="+root, "BT_SECRETS_DIR="+root, "BT_SOURCE_SHA="+strings.Repeat("a", 40), "BT_SERVER_INSTANCE_ID=50000000-0000-4000-8000-000000000001", "BT_APPLE_AUDIENCE=com.example.babytracker")
		if enabled {
			command.Env = append(command.Env, "BT_APNS_TEAM_ID=ABCDEFGHIJ", "BT_APNS_KEY_ID=KLMNOPQRST", "BT_APNS_BUNDLE_ID=com.example.babytracker")
		}
		data, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("enabled=%t config: %v %s", enabled, err, data)
		}
		var model struct {
			Services map[string]struct {
				Environment map[string]string
				Ports       []any
				Secrets     []struct{ Source string }
			}
			Secrets map[string]any
		}
		if err = json.Unmarshal(data, &model); err != nil {
			t.Fatal(err)
		}
		app := model.Services["app"]
		if len(app.Ports) != 0 {
			t.Fatal("push composition publishes host ports")
		}
		_, keyExists := model.Secrets["apns_key"]
		_, workerExists := model.Secrets["live_activity_database_url"]
		if keyExists != enabled || workerExists != enabled {
			t.Fatalf("optional secrets enabled=%t key=%t worker=%t", enabled, keyExists, workerExists)
		}
		if enabled && app.Environment["BT_LIVE_ACTIVITY_PUSH_ENABLED"] != "true" {
			t.Fatal("overlay did not enable push")
		}
		if !enabled && app.Environment["BT_LIVE_ACTIVITY_PUSH_ENABLED"] != "" {
			t.Fatal("default enabled push")
		}
	}
}

// A shutdown signal while the boot activation attempt is in flight is a clean
// stop: main must not log it as a configuration failure or exit 1.
func TestLiveActivityBootActivationInterruptedByShutdownIsClean(t *testing.T) {
	h := testkit.New(t)
	cfg := config.Config{Mode: config.ModeSelfHosted, LiveActivity: config.LiveActivityConfig{Enabled: true, BundleID: "com.example.babytracker", WorkerDatabaseURL: h.LiveActivity.Config().ConnString()}}
	options := httpapi.Options{InstanceID: "50000000-0000-4000-8000-000000000001", RecoveryEpoch: 1, FamilyService: h.Family, Readiness: readinessFunc(func(context.Context) error { return nil })}
	transport := liveActivityTransportFunc(func(context.Context, liveactivity.PushRequest) (liveactivity.PushResult, error) {
		t.Fatal("interrupted boot sent APNs")
		return liveactivity.PushResult{}, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	shutdown, err := configureLiveActivityWorkerWithWait(ctx, cfg, h.Security, h.Runtime, &options, newDatabasePoolBudget(h.Runtime), transport, waitLiveActivityRetry)
	if shutdown != nil {
		t.Cleanup(shutdown)
	}
	if !errors.Is(err, context.Canceled) || !liveActivityShutdownRequested(ctx, err) {
		t.Fatalf("cancelled boot: err=%v shutdown=%v", err, liveActivityShutdownRequested(ctx, err))
	}
	if options.LiveActivityService != nil || options.LiveActivityReadiness != nil {
		t.Fatal("interrupted boot exposed the service")
	}
	// A real configuration failure under a live context stays an exit.
	cfg.LiveActivity.WorkerDatabaseURL = "not a url"
	_, err = configureLiveActivityWorkerWithWait(t.Context(), cfg, h.Security, h.Runtime, &options, newDatabasePoolBudget(h.Runtime), transport, waitLiveActivityRetry)
	if err == nil || liveActivityShutdownRequested(t.Context(), err) {
		t.Fatalf("configuration failure treated as shutdown: %v", err)
	}
	if liveActivityShutdownRequested(ctx, errors.New("live_activity_runtime_required")) {
		t.Fatal("an unrelated error under a cancelled context is not the shutdown itself")
	}
}
