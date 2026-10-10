//go:build identitytest

package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/apns"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/capacity"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/config"
	"babytracker/server/internal/database"
	"babytracker/server/internal/delivery"
	"babytracker/server/internal/family"
	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/migrate"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/realtime"
	familysync "babytracker/server/internal/sync"
	"babytracker/server/internal/testpg"
	"babytracker/server/internal/wake"
	"babytracker/server/migrations"
	"github.com/coder/websocket"
)

func init() { performanceComposition = runPerformanceComposition }
func runPerformanceComposition(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "client")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-tags", "identitytest", "-o", binary, "../sync-performance-client")
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("client build: %v %s", err, raw)
	}
	for _, disabled := range []bool{false, true} {
		name := "full"
		if disabled {
			name = "observer_disabled"
		}
		t.Run(name, func(t *testing.T) {
			h := newPerformanceFixture(t)
			cfg := h.cfg
			cfg.AppleAudience = "com.example.performance"
			cfg.TrustedProxyCIDRs = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
			cfg.Limits.Policies = limits.DefaultPolicies()
			cfg.Sync.Limits = protocol.DefaultLimits()
			rings := []*config.DigestKeyringConfig{&cfg.Auth.IdentityKeys, &cfg.Auth.SessionTokenKeys, &cfg.Auth.DeviceContinuityKeys, &cfg.Auth.AppleNotificationKeys, &cfg.Auth.InvitationKeys, &cfg.Auth.PageTokenKeys}
			for i, ring := range rings {
				*ring = config.DigestKeyringConfig{Current: config.DigestKeyConfig{ID: "performance.v1", Secret: [32]byte{byte(i + 1)}}}
			}
			cfg.Auth.RateLimitPepper[0] = 19
			cfg.RecoveryCleanup.Enabled = true
			var role string
			if err := h.db.Maintenance.QueryRow(t.Context(), "SELECT current_user").Scan(&role); err != nil {
				t.Fatal(err)
			}
			if err := verifyMaintenancePool(t.Context(), h.db.Maintenance, role); err != nil {
				t.Fatal(err)
			}
			if err := verifyMaintenancePool(t.Context(), h.db.Runtime, role); err == nil {
				t.Fatal("runtime accepted as maintenance")
			}
			authorityService, families, err := runtimeServices(cfg, h.db.Runtime)
			if err != nil {
				t.Fatal(err)
			}
			readiness := readinessOver(t, cfg, h.db.Runtime, h.db.Maintenance)
			if err := readiness.inventory.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			_ = readiness.Refresh(t.Context())
			server := httptest.NewUnstartedServer(nil)
			server.Config.ConnContext = realtime.TransportContext
			server.Start()
			defer server.Close()
			target, _ := url.Parse(server.URL)
			reverse := httputil.NewSingleHostReverseProxy(target)
			originalDirector := reverse.Director
			reverse.Director = func(r *http.Request) { originalDirector(r); r.Header.Set("X-Forwarded-For", "192.0.2.1") }
			proxy := httptest.NewUnstartedServer(reverse)
			cert, certPath := performanceCertificate(t)
			proxy.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
			proxy.StartTLS()
			defer proxy.Close()
			origin := strings.Replace(proxy.URL, "127.0.0.1", "localhost", 1)
			cfg.PublicOrigin, _ = url.Parse(origin)
			tracker := wake.NewTracker(0)
			options, err := applicationOptions(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), readiness, authorityService, families, h.db.Runtime, tracker, nil)
			if err != nil {
				t.Fatal(err)
			}
			if options.Limiter == nil || options.LiveHub == nil || options.SyncMutationDevicePolicy != limits.DefaultPolicies()[limits.ScopeSyncMutationDevice] || options.SyncPullDevicePolicy != limits.DefaultPolicies()[limits.ScopeSyncPullDevice] {
				t.Fatal("default limiter or live hub absent")
			}
			if err := options.Readiness.Check(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := options.RecoveryReadiness.Check(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !options.RecoveryEnabled || options.RecoveryService == nil {
				t.Fatal("recovery composition missing")
			}
			if disabled {
				info := protocol.ServerInfo{InstanceID: cfg.ServerInstanceID, RecoveryEpoch: cfg.RecoveryEpoch, CanonicalOrigin: origin, APIVersions: []string{"v1"}, Capabilities: options.Capabilities, Limits: options.SyncLimits, AppleAuthorizationMode: options.AppleAuthorizationMode}
				options.SyncService = familysync.New(familysync.Options{Guard: authorityService, Server: info, Limits: cfg.Sync.Limits, Now: time.Now,
					PageKeys: composeDigestKeyring(cfg.Auth.PageTokenKeys), Allowed: family.Allowed})
			}
			server.Config.Handler = wrapIdentityTestHandler(httpapi.New(options), cfg.AppleAudience)
			defer options.LiveHub.Shutdown(context.Background())
			// Conditional recovery readiness controls real capability and route gates.
			assertRecovery := func(enabled bool) {
				t.Helper()
				// The monitor's next tick: requests read its cached outcome.
				_ = readiness.Refresh(t.Context())
				rr := httptest.NewRecorder()
				httpapi.New(options).ServeHTTP(rr, httptest.NewRequest("GET", "/v1/server", nil))
				// Advertised from configuration whether or not the node is
				// ready; the route itself is gated.
				if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"refresh-recovery-v1"`) {
					t.Fatal("recovery capability withdrawn")
				}
				rr = httptest.NewRecorder()
				httpapi.New(options).ServeHTTP(rr, httptest.NewRequest("POST", "/v1/auth/refresh-recoverable", strings.NewReader(`{}`)))
				want := 400
				if !enabled {
					want = 503
				}
				if rr.Code != want {
					t.Fatalf("recovery admission=%d want=%d", rr.Code, want)
				}
			}
			assertRecovery(true)
			// A node that is not ready (here: an actor-ID key commitment that
			// is not the configured key's) keeps advertising the capability
			// but refuses the route with a retryable 503; restoring it
			// re-admits the route.
			var commitment []byte
			if err := h.db.Owner.QueryRow(t.Context(), "SELECT actor_key_commitment FROM server_state WHERE singleton").Scan(&commitment); err != nil {
				t.Fatal(err)
			}
			if _, err := h.db.Owner.Exec(t.Context(), "UPDATE server_state SET actor_key_commitment=$1 WHERE singleton", make([]byte, 32)); err != nil {
				t.Fatal(err)
			}
			assertRecovery(false)
			if _, err := h.db.Owner.Exec(t.Context(), "UPDATE server_state SET actor_key_commitment=$1 WHERE singleton", commitment); err != nil {
				t.Fatal(err)
			}
			assertRecovery(true)
			state := filepath.Join(t.TempDir(), "state.json")
			clientCtx, stop := context.WithTimeout(t.Context(), 45*time.Second)
			defer stop()
			command := exec.CommandContext(clientCtx, binary, "--base", origin, "--cert", certPath, "--proof-only", "--state", state)
			output, clientErr := command.CombinedOutput()
			if disabled {
				if clientErr == nil || !strings.Contains(string(output), "hint_gate_failed") {
					t.Fatalf("observer-negative did not fail hint gate: %v %s", clientErr, output)
				}
				return
			}
			if clientErr != nil || !strings.Contains(string(output), `"two_authenticated_sockets":true`) || !strings.Contains(string(output), `"hint_gate":true`) {
				t.Fatalf("full composition proof failed: %v %s", clientErr, output)
			}
			var saved struct {
				Caregiver protocol.SessionResponse
				Cursor    protocol.Cursor
			}
			raw, err := os.ReadFile(state)
			if err != nil {
				t.Fatal(err)
			}
			if json.Unmarshal(raw, &saved) != nil {
				t.Fatal("invalid private fixture state")
			}
			// Prove the composed hub delivers a published high water to a new
			// socket through the post-commit observer, independently of a write.
			httpClient := proxy.Client()
			ws, _, err := websocket.Dial(t.Context(), origin+"/v1/live", &websocket.DialOptions{HTTPClient: httpClient, HTTPHeader: http.Header{"Authorization": []string{"Bearer " + saved.Caregiver.AccessToken}, "X-Baby-Tracker-Protocol": []string{"v1"}, "X-Baby-Tracker-Capabilities": []string{"records-v1,live-hints-v1"}}, Subprotocols: []string{realtime.Subprotocol}})
			if err != nil {
				t.Fatal("hub proof socket failed")
			}
			defer ws.CloseNow()
			hint := realtime.Hint{FamilyID: saved.Cursor.FamilyID, Cursor: saved.Cursor}
			hint.Cursor.Sequence++
			// The hub publishes only to bound connections, and the socket
			// binds just after its upgrade answered: publish until it reads.
			readCtx, readCancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer readCancel()
			observer := delivery.NewObserver(options.LiveHub, nil)
			go func() {
				for readCtx.Err() == nil {
					_ = observer.Publish(readCtx, hint.FamilyID, hint.Cursor)
					time.Sleep(10 * time.Millisecond)
				}
			}()
			for {
				_, raw, err := ws.Read(readCtx)
				if err != nil {
					t.Fatal("published hint missing")
				}
				var got realtime.Hint
				if json.Unmarshal(raw, &got) == nil && got.Cursor == hint.Cursor {
					break
				}
			}
			readCancel()
			proveWakeComposition(t, cfg, h.db, options.LiveHub, tracker, httpClient, origin, saved.Caregiver, saved.Cursor, ws)
			proveLiveActivityComposition(t, cfg, h.db, readiness, authorityService, families, saved.Caregiver, saved.Cursor.FamilyID)
		})
	}
	t.Log("performance composition proof complete")
}

// proveWakeComposition proves the wake worker as main composes it: a sync
// request through the composed HTTP stack records the device's progress
// (applicationOptions' Progress), newWakeWorker flushes it on the
// maintenance pool, and a device left behind is re-announced the family's
// high water through the composed live hub on its open socket, although that
// socket was already sent it.
func proveWakeComposition(t *testing.T, cfg config.Config, db *testpg.Fixture, hub *realtime.Hub, tracker *wake.Tracker,
	client *http.Client, origin string, caregiver protocol.SessionResponse, cursor protocol.Cursor, ws *websocket.Conn) {
	t.Helper()
	slots, err := database.NewWorkerSlots(db.Maintenance)
	if err != nil {
		t.Fatal(err)
	}
	worker := newWakeWorker(cfg, hub, db.Maintenance, slots, tracker, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	body, err := json.Marshal(protocol.SyncRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityRecordsV1}},
		Cursor: &cursor, Mutations: []protocol.Mutation{}})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, origin+"/v1/sync", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header = http.Header{"Authorization": []string{"Bearer " + caregiver.AccessToken}, "X-Baby-Tracker-Protocol": []string{"v1"},
		"X-Baby-Tracker-Capabilities": []string{"records-v1"}, "Content-Type": []string{"application/json"}}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("caregiver pull: %d", response.StatusCode)
	}
	if _, err := worker.Tick(t.Context()); err != nil {
		t.Fatalf("wake tick: %v", err)
	}
	var high, progressed int64
	if err := db.Maintenance.QueryRow(t.Context(), `SELECT f.next_sequence, greatest(d.acked_sequence, d.delivered_sequence)
		FROM device_sync_cursors d JOIN families f ON f.id = d.family_id WHERE d.family_id = $1 AND d.device_id = $2`,
		cursor.FamilyID, caregiver.DeviceID).Scan(&high, &progressed); err != nil {
		t.Fatalf("the caregiver's progress was not flushed: %v", err)
	}
	if high == 0 || progressed != high {
		t.Fatalf("flushed progress %d, want the family high water %d", progressed, high)
	}
	// The caregiver's device falls behind (as if its pull had failed) and the
	// family is announced: the worker re-announces through the hub.
	if _, err := db.Maintenance.Exec(t.Context(), `UPDATE device_sync_cursors SET acked_sequence = 0, delivered_sequence = 0
		WHERE family_id = $1 AND device_id = $2`, cursor.FamilyID, caregiver.DeviceID); err != nil {
		t.Fatal(err)
	}
	tracker.Changed(cursor.FamilyID, uint64(high))
	stats, err := worker.Tick(t.Context())
	if err != nil || stats.Woken == 0 {
		t.Fatalf("wake tick: %+v %v", stats, err)
	}
	readCtx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, raw, err := ws.Read(readCtx)
	if err != nil {
		t.Fatalf("no re-announce reached the caregiver's socket: %v", err)
	}
	var got realtime.Hint
	if json.Unmarshal(raw, &got) != nil || got.FamilyID != cursor.FamilyID || got.Cursor.Sequence < uint64(high) {
		t.Fatalf("re-announced %s, want the family's high water %d", raw, high)
	}
	// The composed observer records each committed change for the worker. A
	// batch with a stale cursor commits its write and is then refused, so it
	// reports no progress: only the observer's Changed can make the worker
	// scan the family and wake the device (its window cleared here).
	if _, err := db.Maintenance.Exec(t.Context(), `UPDATE device_sync_cursors SET woken_sequence = 0, woken_at = NULL
		WHERE family_id = $1 AND device_id = $2`, cursor.FamilyID, caregiver.DeviceID); err != nil {
		t.Fatal(err)
	}
	stale := cursor
	stale.Sequence = uint64(high) + 1000
	body, err = json.Marshal(protocol.SyncRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityRecordsV1}},
		Cursor: &stale, Mutations: []protocol.Mutation{caretest.Baby("11000000-0000-4000-8000-0000000000c1")}})
	if err != nil {
		t.Fatal(err)
	}
	request, err = http.NewRequestWithContext(t.Context(), http.MethodPost, origin+"/v1/sync", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header = http.Header{"Authorization": []string{"Bearer " + caregiver.AccessToken}, "X-Baby-Tracker-Protocol": []string{"v1"},
		"X-Baby-Tracker-Capabilities": []string{"records-v1"}, "Content-Type": []string{"application/json"}}
	if response, err = client.Do(request); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	var after int64
	if err := db.Maintenance.QueryRow(t.Context(), `SELECT next_sequence FROM families WHERE id = $1`, cursor.FamilyID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode == http.StatusOK || after != high+1 {
		t.Fatalf("stale-cursor batch: status %d, family at %d (was %d); want a committed write refused after the commit", response.StatusCode, after, high)
	}
	if stats, err = worker.Tick(t.Context()); err != nil || stats.Woken == 0 {
		t.Fatalf("the observer's change woke nobody: %+v %v", stats, err)
	}
}

type performanceFixture struct {
	db  *testpg.Fixture
	cfg config.Config
}

// newPerformanceFixture is a migrated database bound (authority init) to a
// self-hosted configuration.
func newPerformanceFixture(t *testing.T) performanceFixture {
	t.Helper()
	db := testpg.New(t)
	origin, _ := url.Parse("https://sync.example.test")
	cfg := config.Config{Mode: config.ModeSelfHosted, PublicOrigin: origin, ServerInstanceID: "123e4567-e89b-42d3-a456-426614174000", RecoveryEpoch: 1}
	cfg.Limits.MaxCaregivers = 10
	cfg.Limits.MaxInvitations = 10
	cfg.Limits.InvitationTTL = 24 * time.Hour
	cfg.Auth.ActorIDPepper[0] = 1
	if _, err := authority.InitServerState(t.Context(), db.Owner, cfg.ServerInstanceID, cfg.Auth.ActorIDPepper); err != nil {
		t.Fatal(err)
	}
	return performanceFixture{db: db, cfg: cfg}
}

func performanceCertificate(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tls.crt")
	if err = os.WriteFile(path, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	return cert, path
}

// Sealed capacity fixtures must boot through the actual production composition
// with RLS, distinct roles, default quotas and normal application readiness.
func TestCapacityFixtureActualApplicationComposition(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "fixture")
	manifest, err := capacity.BuildFixture(t.Context(), capacity.FixtureSpec{Families: 2, DevicesPerFamily: 2, AuthorityEvents: 12, CareEntriesPerBaby: 3}, directory)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := capacity.RestoreFixture(t.Context(), directory, filepath.Join(root, "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restored.Close(); err != nil {
			t.Error(err)
		}
	})
	if restored.Manifest != manifest || len(manifest.SourceSHA) != 40 || len(manifest.InputSHA256) != 64 || len(restored.SourceInputSHA256) != 64 {
		t.Fatal("source or input identity missing")
	}
	cfg := restored.Config
	authorityService, families, err := runtimeServices(cfg, restored.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	readiness := readinessOver(t, cfg, restored.Runtime, restored.Maintenance)
	if err := readiness.inventory.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := readiness.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	options, err := applicationOptions(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), readiness, authorityService, families, restored.Runtime, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { options.LiveHub.Shutdown(context.Background()) })
	if err := options.Readiness.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	if options.Limiter == nil || options.SyncMutationDevicePolicy != limits.DefaultPolicies()[limits.ScopeSyncMutationDevice] || options.SyncPullDevicePolicy != limits.DefaultPolicies()[limits.ScopeSyncPullDevice] {
		t.Fatal("normal measured quotas absent")
	}
	compatibility, err := migrate.EmbeddedCompatibility()
	if err != nil {
		t.Fatal(err)
	}
	version, err := migrate.New(migrations.Files).CompatibleVersion(t.Context(), restored.Runtime, compatibility)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyRuntimeRole(t.Context(), restored.Runtime, version); err != nil {
		t.Fatal(err)
	}
	var leaked int
	if err := restored.Runtime.QueryRow(t.Context(), "SELECT count(*) FROM care_entities").Scan(&leaked); err != nil || leaked != 0 {
		t.Fatal("unscoped runtime bypassed RLS")
	}
	for _, device := range restored.Devices {
		principal, err := options.SessionService.Authenticate(t.Context(), device.Session.AccessToken)
		if err != nil {
			t.Fatal("application rejected fixture credential")
		}
		view, err := families.Current(t.Context(), principal)
		if err != nil || view.ID != device.FamilyID {
			t.Fatal("application rejected fixture family")
		}
	}
	// A request is granted its session's family only: device 0 is never
	// granted device 2's family.
	digests, err := options.SessionService.AccessDigests(restored.Devices[0].Session.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Devices[0].FamilyID == restored.Devices[2].FamilyID {
		t.Fatal("fixture devices 0 and 2 share a family")
	}
	if _, grant, err := options.RequestAuthorizer.AuthorizeRequest(t.Context(), authority.Request{Digests: digests}, "read"); err != nil ||
		grant.FamilyID != restored.Devices[0].FamilyID {
		t.Fatalf("device 0 granted family %s (err %v), want its own", grant.FamilyID, err)
	}
}

// proveLiveActivityComposition proves Live Activity push as main composes
// it, with BT_LIVE_ACTIVITY_PUSH_ENABLED on over the same database and
// node readiness: applicationOptions with the worker's Dirty on the change
// feed, composeLiveActivity's worker on the maintenance pool in its worker
// slots sending through the one APNs client (here a local HTTP/2 recorder),
// and Run gated by the node's readiness. live-activity-push-v1 is advertised
// only once the worker is ready; a phone that registers through the composed
// HTTP stack gets the push-to-start for a timer it started before it
// registered; and a sync write reaches the worker through the change feed
// (its flush, not the minute sweep, moves the family's dirty sequence).
func proveLiveActivityComposition(t *testing.T, cfg config.Config, db *testpg.Fixture, readiness *databaseReadiness, authorityService *authority.Service,
	families *family.Service, caregiver protocol.SessionResponse, familyID string) {
	t.Helper()
	type push struct {
		path    string
		headers http.Header
		body    string
	}
	pushes := make(chan push, 16)
	apple := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		pushes <- push{path: r.URL.Path, headers: r.Header.Clone(), body: string(body)}
		w.WriteHeader(http.StatusOK)
	}))
	apple.EnableHTTP2 = true
	apple.StartTLS()
	defer apple.Close()
	cfg.LiveActivity = config.LiveActivityConfig{Enabled: true, Credentials: liveActivityCredentials(t)}
	client, err := apnsClientFor(cfg, func(creds config.APNSCredentials) (*apns.Client, error) {
		return apns.NewWithTransport(creds, apple.Client().Transport, apple.URL, apple.URL, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dirty := newLiveActivityDirty(cfg)
	options, err := applicationOptions(cfg, logger, readiness, authorityService, families, db.Runtime, nil, dirty)
	if err != nil {
		t.Fatal(err)
	}
	defer options.LiveHub.Shutdown(context.Background())
	slots, err := database.NewWorkerSlots(db.Maintenance)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := composeLiveActivity(cfg, &options, authorityService, dirty, db.Maintenance, slots, client, logger)
	if err != nil || worker == nil {
		t.Fatalf("compose live activity: %v", err)
	}
	server := httptest.NewServer(httpapi.New(options))
	defer server.Close()
	advertised := func() bool {
		t.Helper()
		response, err := http.Get(server.URL + "/v1/server")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var info protocol.ServerInfo
		if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&info) != nil {
			t.Fatalf("/v1/server %d", response.StatusCode)
		}
		return slices.Contains(info.Capabilities, protocol.CapabilityLiveActivityPushV1)
	}
	if advertised() {
		t.Fatal("live-activity-push-v1 advertised before the worker ran")
	}
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan error, 1)
	go func() { stopped <- worker.Run(ctx, options.Readiness.Check) }()
	defer func() {
		cancel()
		if err := <-stopped; !errors.Is(err, context.Canceled) {
			t.Errorf("live activity worker stop: %v", err)
		}
	}()
	for deadline := time.Now().Add(10 * time.Second); options.LiveActivityReady(t.Context()) != nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the live activity worker never became ready")
		}
	}
	if !advertised() {
		t.Fatal("live-activity-push-v1 not advertised once the worker was ready")
	}
	send := func(method, path, capabilities string, body any) int {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, strings.NewReader(string(raw)))
		if err != nil {
			t.Fatal(err)
		}
		request.Header = http.Header{"Authorization": []string{"Bearer " + caregiver.AccessToken}, "X-Baby-Tracker-Protocol": []string{"v1"},
			"X-Baby-Tracker-Capabilities": []string{capabilities}, "Content-Type": []string{"application/json"}}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			answer, _ := io.ReadAll(io.LimitReader(response.Body, 512))
			t.Logf("%s %s: %d %s", method, path, response.StatusCode, answer)
		}
		return response.StatusCode
	}
	highWater := func() int64 {
		t.Helper()
		var high int64
		if err := db.Maintenance.QueryRow(t.Context(), `SELECT next_sequence FROM families WHERE id=$1`, familyID).Scan(&high); err != nil {
			t.Fatal(err)
		}
		return high
	}
	// sync is one batch through the composed stack, with exactly the
	// capabilities its surface requires (timer commands need theirs).
	sync := func(capabilities []string, mutations ...protocol.Mutation) {
		t.Helper()
		cursor := protocol.Cursor{ServerInstanceID: cfg.ServerInstanceID, RecoveryEpoch: cfg.RecoveryEpoch, FamilyID: familyID, Sequence: uint64(highWater())}
		if code := send(http.MethodPost, "/v1/sync", strings.Join(capabilities, ","), protocol.SyncRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1",
			Capabilities: capabilities}, Cursor: &cursor, Mutations: mutations}); code != http.StatusOK {
			t.Fatalf("caregiver sync of %d mutations: %d", len(mutations), code)
		}
	}
	// The caregiver's phone starts a feed, then registers for Live
	// Activities: its timer predates the registration, so it is pushed a
	// start (a timer started after it registered would show its own card).
	const baby, timer = "11000000-0000-4000-8000-0000000000d1", "51000000-0000-4000-8000-0000000000d1"
	sync([]string{protocol.CapabilityRecordsV1, protocol.CapabilityTimerCommandsV1}, caretest.Baby(baby), caretest.StartTimer(baby, timer, "feed"))
	registration := protocol.LiveActivityDeviceRegistration{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityLiveActivityPushV1}},
		InstallationID: "70000000-0000-4000-8000-0000000000d1", RegistrationVersion: 1, APNsEnvironment: "sandbox", PayloadVersion: 1, PushToStartToken: "aabbccdd", FeedBreakMinutes: 30}
	if code := send(http.MethodPut, "/v1/live-activities/device", protocol.CapabilityLiveActivityPushV1, registration); code != http.StatusOK {
		t.Fatalf("live activity registration: %d", code)
	}
	select {
	case got := <-pushes:
		if got.path != "/3/device/aabbccdd" || got.headers.Get("apns-push-type") != "liveactivity" ||
			got.headers.Get("apns-topic") != cfg.LiveActivity.Credentials.BundleID+".push-type.liveactivity" || !strings.Contains(got.body, `"event":"start"`) {
			t.Fatalf("push %s %v %s", got.path, got.headers, got.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no push-to-start reached APNs")
	}
	// A later write reaches the worker through the change feed: its flush
	// moves the family's dirty sequence to the write's high water within a
	// poll, a minute before the next sweep could.
	sync([]string{protocol.CapabilityRecordsV1}, caretest.Baby("11000000-0000-4000-8000-0000000000d2"))
	high := highWater()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		var flushed int64
		if err := db.Owner.QueryRow(t.Context(), `SELECT journal_sequence FROM live_activity_family_dirty WHERE family_id=$1`, familyID).Scan(&flushed); err != nil {
			t.Fatal(err)
		}
		if flushed >= high {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the change feed did not reach the worker: dirty sequence %d, family at %d", flushed, high)
		}
	}
}
