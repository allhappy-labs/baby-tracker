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
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/config"
	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/realtime"
	"babytracker/server/internal/records"
	familysync "babytracker/server/internal/sync"
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
			h := newReadinessFixture(t)
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
			if err := verifyExportPool(t.Context(), h.db.Maintenance, role); err != nil {
				t.Fatal(err)
			}
			if err := verifyExportPool(t.Context(), h.db.Runtime, role); err == nil {
				t.Fatal("runtime accepted as maintenance")
			}
			if err := verifyExportPool(t.Context(), h.db.Projector, role); err == nil {
				t.Fatal("projector accepted as maintenance")
			}
			gate, authority, families, err := runtimeServices(cfg, h.db.Runtime, h.db.Projector)
			if err != nil {
				t.Fatal(err)
			}
			if err = gate.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
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
			options, err := applicationOptions(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), gate, authority, families, h.db.Runtime)
			if err != nil {
				t.Fatal(err)
			}
			if options.Limiter == nil || options.LiveHub == nil || options.SyncMutationDevicePolicy != limits.DefaultPolicies()[limits.ScopeSyncMutationDevice] || options.SyncPullDevicePolicy != limits.DefaultPolicies()[limits.ScopeSyncPullDevice] {
				t.Fatal("default limiter or live hub absent")
			}
			if cfg.RecoveryCleanup.Enabled {
				options.RecoveryReadiness = recoveryFunctionReadiness{pool: h.db.Maintenance}
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
				options.SyncService = familysync.NewServiceWithPaging(authority, records.NewRepository(), info, cfg.Sync.Limits, time.Now, syncFamilyResolver{service: families}, nil, composeDigestKeyring(cfg.Auth.PageTokenKeys))
			}
			server.Config.Handler = wrapIdentityTestHandler(httpapi.New(options), cfg.AppleAudience)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			listener, err := realtime.StartPostgresListener(ctx, realtime.PostgresOptions{Pool: h.db.Runtime, Hub: options.LiveHub, InstanceID: cfg.ServerInstanceID, RecoveryEpoch: cfg.RecoveryEpoch, Resyncer: syncCursorResyncer{service: options.SyncService}})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			defer options.LiveHub.Shutdown(context.Background())
			go reconcileLoop(ctx, gate)
			// Run the actual worker through the real gate and maintenance pool. The
			// wrapper observes successful SQL only; it does not replace its behavior.
			calls := make(chan struct{}, 16)
			workerDone := make(chan struct{})
			worker := recoveryCleanup{pool: h.db.Maintenance, now: time.Now}
			go func() {
				defer close(workerDone)
				runRecoveryCleanup(ctx, gate, recoveryCleanupFunc(func(c context.Context) error {
					err := worker.RunOnce(c)
					if err == nil {
						select {
						case calls <- struct{}{}:
						default:
						}
					}
					return err
				}), nil, 20*time.Millisecond)
			}()
			defer func() { cancel(); <-workerDone }()
			select {
			case <-calls:
			case <-time.After(3 * time.Second):
				t.Fatal("cleanup worker not admitted")
			}
			// Conditional recovery readiness controls real capability and route gates.
			assertRecovery := func(enabled bool) {
				t.Helper()
				rr := httptest.NewRecorder()
				httpapi.New(options).ServeHTTP(rr, httptest.NewRequest("GET", "/v1/server", nil))
				if rr.Code != 200 || strings.Contains(rr.Body.String(), `"refresh-recovery-v1"`) != enabled {
					t.Fatal("recovery capability gate mismatch")
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
			if _, err := h.db.Owner.Exec(t.Context(), "REVOKE EXECUTE ON FUNCTION app.cleanup_refresh_recovery_receipts(timestamptz) FROM baby_tracker_maintenance"); err != nil {
				t.Fatal(err)
			}
			assertRecovery(false)
			if _, err := h.db.Owner.Exec(t.Context(), "GRANT EXECUTE ON FUNCTION app.cleanup_refresh_recovery_receipts(timestamptz) TO baby_tracker_maintenance"); err != nil {
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
			// Prove listener delivery independently of in-process observer publication.
			httpClient := proxy.Client()
			ws, _, err := websocket.Dial(t.Context(), origin+"/v1/live", &websocket.DialOptions{HTTPClient: httpClient, HTTPHeader: http.Header{"Authorization": []string{"Bearer " + saved.Caregiver.AccessToken}, "X-Baby-Tracker-Protocol": []string{"v1"}, "X-Baby-Tracker-Capabilities": []string{"records-v1,live-hints-v1"}}, Subprotocols: []string{realtime.Subprotocol}})
			if err != nil {
				t.Fatal("listener proof socket failed")
			}
			defer ws.CloseNow()
			hint := realtime.Hint{FamilyID: saved.Cursor.FamilyID, Cursor: saved.Cursor}
			hint.Cursor.Sequence++
			payload, _ := json.Marshal(hint)
			if _, err := h.db.Owner.Exec(t.Context(), "SELECT pg_notify('family_cursor_v1',$1)", string(payload)); err != nil {
				t.Fatal(err)
			}
			readCtx, readCancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer readCancel()
			for {
				_, raw, err := ws.Read(readCtx)
				if err != nil {
					t.Fatal("listener notification missing")
				}
				var got realtime.Hint
				if json.Unmarshal(raw, &got) == nil && got.Cursor == hint.Cursor {
					break
				}
			}
			var pending int
			if err := h.db.Owner.QueryRow(t.Context(), "SELECT count(*) FROM delivery_pending").Scan(&pending); err != nil || pending != 0 {
				t.Fatal("selfhosted default delivery policy changed")
			}
			// Corrupt the owned trusted head: cleanup must stop at Gate.Check even
			// though the maintenance function itself remains callable.
			if err := os.WriteFile(filepath.Join(cfg.LedgerHeadDir, "head.json"), []byte("{}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			time.Sleep(60 * time.Millisecond)
			for len(calls) > 0 {
				<-calls
			}
			select {
			case <-calls:
				t.Fatal("cleanup bypassed recovery gate")
			case <-time.After(80 * time.Millisecond):
			}
		})
	}
	t.Log("performance composition proof complete")
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
