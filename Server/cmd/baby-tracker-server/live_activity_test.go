package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/apns"
	"babytracker/server/internal/config"
	"babytracker/server/internal/database"
	"babytracker/server/internal/delivery"
	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/liveactivity"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"babytracker/server/internal/testpg"
	"babytracker/server/internal/wake"
	"github.com/jackc/pgx/v5/pgxpool"
)

// liveActivityCredentials is a throwaway P-256 key: no Apple credential is
// read.
func liveActivityCredentials(t *testing.T) config.APNSCredentials {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	path := filepath.Join(t.TempDir(), "apns.p8")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.APNSCredentials{TeamID: "TEAM123456", KeyID: "KEY1234567", KeyFile: path, BundleID: "com.example.babytracker"}
}

// unreachableAPNs builds the one APNs client against hosts nothing answers.
func unreachableAPNs(creds config.APNSCredentials) (*apns.Client, error) {
	return apns.NewWithTransport(creds, http.DefaultTransport, "https://sandbox.invalid", "https://production.invalid", nil)
}

func serve(options httpapi.Options, method, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	httpapi.New(options).ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader("{}")))
	return recorder
}

// advertised is /v1/server's capabilities with the node ready.
func advertised(t *testing.T, options httpapi.Options) []string {
	t.Helper()
	options.ServerState = nil
	options.Readiness = readinessFunc(func(context.Context) error { return nil })
	recorder := serve(options, http.MethodGet, "/v1/server")
	var info protocol.ServerInfo
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &info) != nil {
		t.Fatalf("/v1/server %d %s", recorder.Code, recorder.Body)
	}
	return info.Capabilities
}

// With BT_LIVE_ACTIVITY_PUSH_ENABLED off nothing of push is composed: no
// worker, the PUT routes are not registered and the capability is never
// listed. A phone may still withdraw (DELETE is served), under the
// configured Live Activity rate-limit policies.
func TestLiveActivitySwitchOffComposesOnlyUnregistration(t *testing.T) {
	cfg := testApplicationConfig(config.ModeSelfHosted)
	cfg.Limits.Policies[limits.ScopeLiveActivityDevice] = limits.Policy{Count: 7, Window: time.Minute}
	pool := &pgxpool.Pool{}
	authorityService, familyService, readiness := testComposition(t, cfg, pool)
	if newLiveActivityDirty(cfg) != nil {
		t.Fatal("a change feed recorder with push off")
	}
	options, err := applicationOptions(cfg, nil, readiness, authorityService, familyService, pool, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := composeLiveActivity(cfg, &options, authorityService, nil, pool, nil, nil, nil)
	if err != nil || worker != nil {
		t.Fatalf("push off composed a worker: %v %v", worker, err)
	}
	if options.LiveActivityService != nil || options.LiveActivityReady != nil || options.LiveActivityUnregistration == nil {
		t.Fatalf("push off: service %v ready %v unregistration %v", options.LiveActivityService, options.LiveActivityReady != nil, options.LiveActivityUnregistration)
	}
	if slices.Contains(options.Capabilities, protocol.CapabilityLiveActivityPushV1) || slices.Contains(advertised(t, options), protocol.CapabilityLiveActivityPushV1) {
		t.Fatalf("push off advertises %s", protocol.CapabilityLiveActivityPushV1)
	}
	for _, path := range []string{"/v1/live-activities/device", "/v1/live-activities/activity"} {
		if code := serve(options, http.MethodPut, path).Code; code != http.StatusNotFound {
			t.Fatalf("push off: PUT %s %d", path, code)
		}
	}
	if code := serve(options, http.MethodDelete, "/v1/live-activities/device/70000000-0000-4000-8000-000000000001?registration_version=1").Code; code == http.StatusNotFound || code == http.StatusMethodNotAllowed {
		t.Fatalf("push off: DELETE %d", code)
	}
	if options.LiveActivityDevicePolicy.Count != 7 || options.LiveActivityAccountPolicy != cfg.Limits.Policies[limits.ScopeLiveActivityAccount] {
		t.Fatalf("live activity policies %+v %+v", options.LiveActivityDevicePolicy, options.LiveActivityAccountPolicy)
	}
	cfg.LiveActivity = config.LiveActivityConfig{Enabled: true, Credentials: liveActivityCredentials(t)}
	if _, err := applicationOptions(cfg, nil, readiness, authorityService, familyService, pool, nil, nil); err == nil {
		t.Fatal("push on without its change feed recorder accepted")
	}
	delete(cfg.Limits.Policies, limits.ScopeLiveActivityAccount)
	cfg.LiveActivity = config.LiveActivityConfig{}
	if _, err := applicationOptions(cfg, nil, readiness, authorityService, familyService, pool, nil, nil); err == nil {
		t.Fatal("composition accepted a missing live activity policy")
	}
}

// With the switch on, the worker runs on the maintenance pool in its worker
// slots and sends through the one APNs client the wake pusher uses (the same
// pointer); the change feed records into its Dirty; live-activity-push-v1 is
// advertised once the worker's first pass completed, and withdrawn when it
// stops.
func TestLiveActivitySwitchOnRunsTheWorkerOnTheMaintenancePool(t *testing.T) {
	db := testpg.New(t)
	creds := liveActivityCredentials(t)
	cfg := testApplicationConfig(config.ModeSelfHosted)
	cfg.LiveActivity = config.LiveActivityConfig{Enabled: true, Credentials: creds}
	cfg.APNSWakes = config.APNSWakesConfig{Enabled: true, Credentials: creds}
	client, err := apnsClientFor(cfg, unreachableAPNs)
	if err != nil || client == nil {
		t.Fatalf("apns client %v %v", client, err)
	}
	authorityService, familyService, readiness := testComposition(t, cfg, db.Runtime)
	dirty := newLiveActivityDirty(cfg)
	options, err := applicationOptions(cfg, nil, readiness, authorityService, familyService, db.Runtime, nil, dirty)
	if err != nil {
		t.Fatal(err)
	}
	counted, wire := testkit.CountedPool(t, db.Maintenance)
	slots, err := database.NewWorkerSlots(db.Maintenance)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := composeLiveActivity(cfg, &options, authorityService, dirty, counted, slots, nil, nil); err == nil {
		t.Fatal("push on without the APNs client composed")
	}
	worker, err := composeLiveActivity(cfg, &options, authorityService, dirty, counted, slots, client, nil)
	if err != nil || worker == nil {
		t.Fatalf("push on: %v %v", worker, err)
	}
	if transport, ok := worker.Transport.(liveactivity.APNSTransport); !ok || transport.Client != client {
		t.Fatalf("the worker's transport %#v is not the one APNs client", worker.Transport)
	}
	if wake := newWakeWorker(cfg, options.LiveHub, db.Maintenance, slots, nil, client, nil); wake.Pusher != client {
		t.Fatalf("the wake pusher %v is not the one APNs client", wake.Pusher)
	}
	if worker.Slots != slots || worker.Dirty != dirty || worker.Guard != authorityService || worker.RequireEntitlement {
		t.Fatalf("worker %+v", worker)
	}
	if _, ok := options.LiveActivityService.(*liveactivity.Service); !ok {
		t.Fatalf("push on: service %T", options.LiveActivityService)
	}
	if options.LiveActivityReady(t.Context()) == nil || slices.Contains(advertised(t, options), protocol.CapabilityLiveActivityPushV1) {
		t.Fatal("advertised before the worker's first pass")
	}
	// The change feed records each committed change in the tracker and in
	// Dirty (proveLiveActivityComposition proves it through a sync write).
	tracker := wake.NewTracker(0)
	changeFeed(tracker, dirty).Changed("f0000000-0000-4000-8000-0000000000f1", 3)
	if dirty.Len() != 1 {
		t.Fatalf("the change feed recorded %d families in the worker's Dirty", dirty.Len())
	}
	if changeFeed(nil, nil) != nil || changeFeed(tracker, nil) != delivery.ChangeRecorder(tracker) {
		t.Fatal("the change feed composes recorders that were not composed")
	}
	// Running, but its first pass waits for a maintenance slot (every slot
	// held): not ready, not advertised.
	var held []func()
	for range slots.Size() {
		release, err := slots.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, release)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx, func(context.Context) error { return nil }) }()
	time.Sleep(300 * time.Millisecond)
	if options.LiveActivityReady(t.Context()) == nil || slices.Contains(advertised(t, options), protocol.CapabilityLiveActivityPushV1) {
		t.Fatal("advertised while the worker's first pass waited for a slot")
	}
	for _, release := range held {
		release()
	}
	deadline := time.Now().Add(10 * time.Second)
	for options.LiveActivityReady(t.Context()) != nil {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("the worker never became ready: %v", <-done)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if wire.Trips() == 0 || dirty.Len() != 0 {
		t.Fatalf("the worker's pass did not run on the maintenance pool: %d round trips, %d dirty", wire.Trips(), dirty.Len())
	}
	if !slices.Contains(advertised(t, options), protocol.CapabilityLiveActivityPushV1) {
		t.Fatal("not advertised once the worker was ready")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("stop: %v", err)
	}
	if options.LiveActivityReady(t.Context()) == nil || slices.Contains(advertised(t, options), protocol.CapabilityLiveActivityPushV1) {
		t.Fatal("still advertised after the worker stopped")
	}
}

// main's worker credential is not ported: a leftover setting refuses to
// start with the maintenance-credential explanation.
func TestLiveActivityWorkerCredentialIsRefused(t *testing.T) {
	for _, name := range []string{"BT_LIVE_ACTIVITY_DATABASE_URL_FILE", "BT_LIVE_ACTIVITY_DATABASE_URL"} {
		t.Setenv(name, "/run/secrets/live_activity_database_url")
		_, err := loadServerConfig()
		if err == nil || !strings.Contains(err.Error(), name+" is not used: the Live Activity worker runs on the maintenance credential") {
			t.Fatalf("%s: %v", name, err)
		}
		t.Setenv(name, "")
	}
}
