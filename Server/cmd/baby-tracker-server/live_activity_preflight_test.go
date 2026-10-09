package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/config"
	"babytracker/server/internal/liveactivity"
	"babytracker/server/internal/testkit"
)

// scriptedPreflightTransport stands in for Apple: the HTTP/2 classification
// itself is proven in the liveactivity package against httptest servers.
type scriptedPreflightTransport struct {
	mu      sync.Mutex
	answers map[string]liveactivity.PreflightResult
	err     error
	calls   []string
	closed  bool
}

func (s *scriptedPreflightTransport) Preflight(_ context.Context, environment string) (liveactivity.PreflightResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, environment)
	if s.err != nil {
		return liveactivity.PreflightResult{Environment: environment, Outcome: liveactivity.PreflightUnknown}, s.err
	}
	result := s.answers[environment]
	result.Environment = environment
	return result, nil
}

func (s *scriptedPreflightTransport) CloseIdleConnections() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

type preflightFixture struct {
	env       map[string]string
	keyPEM    string
	transport *scriptedPreflightTransport
	built     int
	output    bytes.Buffer
	command   *liveActivityPreflight
}

func newPreflightFixture(t *testing.T) *preflightFixture {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	keyPath := filepath.Join(t.TempDir(), "apns_key")
	if err := os.WriteFile(keyPath, []byte(keyPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &preflightFixture{
		env:       map[string]string{"BT_APNS_TEAM_ID": "TEAM123456", "BT_APNS_KEY_ID": "KEY1234567", "BT_APNS_KEY_FILE": keyPath, "BT_APNS_BUNDLE_ID": "com.example.baby"},
		keyPEM:    keyPEM,
		transport: &scriptedPreflightTransport{answers: map[string]liveactivity.PreflightResult{}},
	}
	f.answer("sandbox", liveactivity.PreflightPass, 400, "BadDeviceToken")
	f.answer("production", liveactivity.PreflightPass, 400, "BadDeviceToken")
	f.command = &liveActivityPreflight{
		getenv: func(name string) string { return f.env[name] },
		out:    &f.output,
		transport: func(cfg config.LiveActivityConfig) (preflightTransport, error) {
			f.built++
			if cfg.TeamID != "TEAM123456" || cfg.KeyID != "KEY1234567" || cfg.BundleID != "com.example.baby" || cfg.KeyFile != keyPath {
				t.Fatalf("transport built from other credentials: %+v", cfg)
			}
			return f.transport, nil
		},
	}
	return f
}

func (f *preflightFixture) answer(environment string, outcome liveactivity.PreflightOutcome, status int, reason string) {
	f.transport.mu.Lock()
	defer f.transport.mu.Unlock()
	f.transport.answers[environment] = liveactivity.PreflightResult{Outcome: outcome, HTTPStatus: status, Reason: reason, RetryAfter: time.Now().Add(30 * time.Second)}
}

func (f *preflightFixture) run(t *testing.T, args ...string) (int, string) {
	t.Helper()
	f.output.Reset()
	code := f.command.run(t.Context(), args)
	out := f.output.String()
	// Never the key, never a bearer token, never a database URL.
	for _, forbidden := range []string{strings.Split(f.keyPEM, "\n")[1], "bearer ", "postgres://", "secret"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("preflight output leaks %q:\n%s", forbidden, out)
		}
	}
	return code, out
}

func requireLines(t *testing.T, out string, lines ...string) {
	t.Helper()
	for _, line := range lines {
		if !strings.Contains(out, line) {
			t.Fatalf("output lacks %q:\n%s", line, out)
		}
	}
}

func TestLiveActivityPreflightPassesWithAcceptedCredentialsAndTopic(t *testing.T) {
	f := newPreflightFixture(t)
	code, out := f.run(t)
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	requireLines(t, out,
		"[PASS] key file:", "mode 0600", "P-256",
		"[PASS] identifiers: team TEAM123456, key ID KEY1234567, bundle com.example.baby",
		"[PASS] topic: com.example.baby.push-type.liveactivity",
		"[PASS] apple sandbox: 400 BadDeviceToken - key, team ID and key ID accepted; topic not contradicted (a dummy token cannot prove it)",
		"[SKIP] apple production: not selected",
		"[SKIP] worker database: BT_LIVE_ACTIVITY_DATABASE_URL_FILE not set",
		"RESULT: PASS")
	if calls := f.transport.calls; len(calls) != 1 || calls[0] != "sandbox" {
		t.Fatalf("requests: %v (one per selected environment)", calls)
	}
	if !f.transport.closed || f.built != 1 {
		t.Fatalf("transport closed=%v built=%d", f.transport.closed, f.built)
	}
	code, out = f.run(t, "--environment", "both")
	if code != 0 || len(f.transport.calls) != 3 || f.transport.calls[1] != "sandbox" || f.transport.calls[2] != "production" {
		t.Fatalf("both: exit=%d calls=%v\n%s", code, f.transport.calls, out)
	}
	requireLines(t, out, "[PASS] apple sandbox: 400 BadDeviceToken", "[PASS] apple production: 400 BadDeviceToken", "RESULT: PASS")
	code, out = f.run(t, "--environment", "production")
	if code != 0 || f.transport.calls[len(f.transport.calls)-1] != "production" {
		t.Fatalf("production: exit=%d calls=%v\n%s", code, f.transport.calls, out)
	}
	requireLines(t, out, "[SKIP] apple sandbox: not selected", "[PASS] apple production: 400 BadDeviceToken")
}

func TestLiveActivityPreflightReportsEachAppleVerdict(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome liveactivity.PreflightOutcome
		status  int
		reason  string
		line    string
	}{
		{"credentials", liveactivity.PreflightCredentials, 403, "InvalidProviderToken", "[FAIL] apple sandbox: 403 InvalidProviderToken - key, team ID or key ID rejected"},
		{"expired", liveactivity.PreflightCredentials, 403, "ExpiredProviderToken", "[FAIL] apple sandbox: 403 ExpiredProviderToken - key, team ID or key ID rejected"},
		{"topic", liveactivity.PreflightTopic, 400, "TopicDisallowed", "[FAIL] apple sandbox: 400 TopicDisallowed - bundle ID or topic rejected for this key"},
		{"retry", liveactivity.PreflightRetryLater, 429, "TooManyProviderTokenUpdates", "[RETRY] apple sandbox: 429 TooManyProviderTokenUpdates - rate limited, retry later"},
		{"connectivity", liveactivity.PreflightConnectivity, 0, "", "[FAIL] apple sandbox: no answer - connectivity (DNS, TCP, TLS or timeout)"},
		{"unknown", liveactivity.PreflightUnknown, 500, "InternalServerError", "[UNKNOWN] apple sandbox: 500 InternalServerError - not a recognised verdict"},
		{"unknown_no_reason", liveactivity.PreflightUnknown, 200, "", "[UNKNOWN] apple sandbox: 200 - not a recognised verdict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPreflightFixture(t)
			f.answer("sandbox", tc.outcome, tc.status, tc.reason)
			code, out := f.run(t)
			if code != 1 {
				t.Fatalf("exit=%d\n%s", code, out)
			}
			requireLines(t, out, tc.line, "RESULT: FAIL")
		})
	}
	f := newPreflightFixture(t)
	f.transport.err = errors.New("APNs provider signing key is invalid")
	code, out := f.run(t)
	if code != 1 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	requireLines(t, out, "[FAIL] apple sandbox: request not sent - APNs provider signing key is invalid", "RESULT: FAIL")
}

func TestLiveActivityPreflightFailsOnConfigurationWithoutContactingApple(t *testing.T) {
	f := newPreflightFixture(t)
	delete(f.env, "BT_APNS_TEAM_ID")
	code, out := f.run(t)
	if code != 1 || f.built != 0 {
		t.Fatalf("exit=%d built=%d\n%s", code, f.built, out)
	}
	requireLines(t, out, "[FAIL] credentials: BT_APNS_TEAM_ID must be a ten-character Apple team ID", "[SKIP] apple sandbox: credentials invalid", "RESULT: FAIL")

	// A transport that cannot be constructed is its own failure; the
	// per-environment lines must not blame the credentials.
	f = newPreflightFixture(t)
	f.command.transport = func(config.LiveActivityConfig) (preflightTransport, error) {
		return nil, errors.New("APNs transport is disabled")
	}
	code, out = f.run(t, "--environment", "both")
	if code != 1 {
		t.Fatalf("transport failure: exit=%d\n%s", code, out)
	}
	requireLines(t, out, "[FAIL] apple: transport unavailable - APNs transport is disabled", "[SKIP] apple sandbox: transport unavailable", "[SKIP] apple production: transport unavailable", "RESULT: FAIL")
	if strings.Contains(out, "credentials invalid") {
		t.Fatalf("transport failure blamed the credentials:\n%s", out)
	}

	f = newPreflightFixture(t)
	if err := os.Chmod(f.env["BT_APNS_KEY_FILE"], 0o640); err != nil {
		t.Fatal(err)
	}
	code, out = f.run(t)
	if code != 1 || f.built != 0 {
		t.Fatalf("group-readable key: exit=%d built=%d\n%s", code, f.built, out)
	}
	requireLines(t, out, "[FAIL] credentials: BT_APNS_KEY_FILE must be a regular nonsymlink P-256 PEM file with mode 0600")

	f = newPreflightFixture(t)
	f.env["BT_LIVE_ACTIVITY_DATABASE_URL"] = "postgres://worker:secret@db/babytracker"
	code, out = f.run(t)
	if code != 1 {
		t.Fatalf("inline url: exit=%d\n%s", code, out)
	}
	requireLines(t, out, "[FAIL] worker database: BT_LIVE_ACTIVITY_DATABASE_URL is forbidden; use BT_LIVE_ACTIVITY_DATABASE_URL_FILE", "[PASS] apple sandbox")

	f = newPreflightFixture(t)
	urlPath := filepath.Join(t.TempDir(), "worker_url")
	if err := os.WriteFile(urlPath, []byte("postgres://worker:secret@db/babytracker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Explicit: the disposable runner's umask 077 would otherwise make this 0600.
	if err := os.Chmod(urlPath, 0o644); err != nil {
		t.Fatal(err)
	}
	f.env["BT_LIVE_ACTIVITY_DATABASE_URL_FILE"] = urlPath
	code, out = f.run(t)
	if code != 1 {
		t.Fatalf("world-readable url file: exit=%d\n%s", code, out)
	}
	requireLines(t, out, "[FAIL] worker database: BT_LIVE_ACTIVITY_DATABASE_URL_FILE must be a regular nonsymlink file with mode 0600")

	// A configured but unreadable runtime URL file fails instead of silently
	// skipping the same-database proof; nothing is dialled first.
	f = newPreflightFixture(t)
	readable := filepath.Join(t.TempDir(), "worker_url")
	if err := os.WriteFile(readable, []byte("postgres://worker:secret@127.0.0.1:1/babytracker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.env["BT_LIVE_ACTIVITY_DATABASE_URL_FILE"] = readable
	f.env["BT_DATABASE_URL_FILE"] = filepath.Join(t.TempDir(), "missing")
	code, out = f.run(t)
	if code != 1 {
		t.Fatalf("unreadable runtime url file: exit=%d\n%s", code, out)
	}
	requireLines(t, out, "[FAIL] worker database: runtime BT_DATABASE_URL_FILE could not be read")

	f = newPreflightFixture(t)
	code, out = f.run(t, "--environment", "staging")
	if code != 2 || f.built != 0 {
		t.Fatalf("usage: exit=%d built=%d\n%s", code, f.built, out)
	}
	code, out = f.run(t, "--skip-apple")
	if code != 0 || f.built != 0 {
		t.Fatalf("skip apple: exit=%d built=%d\n%s", code, f.built, out)
	}
	requireLines(t, out, "[SKIP] apple sandbox: skipped by --skip-apple", "RESULT: PASS")

	// Before R2-6 the worker login has no password yet: the Apple checks can
	// run alone, and the database checks follow once the login exists.
	f = newPreflightFixture(t)
	f.env["BT_LIVE_ACTIVITY_DATABASE_URL_FILE"] = urlPath
	code, out = f.run(t, "--skip-database")
	if code != 0 || f.built != 1 {
		t.Fatalf("skip database: exit=%d built=%d\n%s", code, f.built, out)
	}
	requireLines(t, out, "[PASS] apple sandbox", "[SKIP] worker database: skipped by --skip-database", "RESULT: PASS")
}

// The worker-database checks are the production activation checks: schema
// exactly 29, the exact baby_tracker_live_activity inventory, and the shared
// snapshot that proves the worker and runtime use one database.
func TestLiveActivityPreflightVerifiesWorkerDatabase(t *testing.T) {
	h := testkit.New(t)
	workerURL := h.LiveActivity.Config().ConnString()
	password := h.LiveActivity.Config().ConnConfig.Password
	if password == "" {
		t.Fatal("fixture worker credential has no password to redact")
	}
	write := func(t *testing.T, url string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "live_activity_database_url")
		if err := os.WriteFile(path, []byte(url+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	run := func(t *testing.T, f *preflightFixture) (int, string) {
		t.Helper()
		code, out := f.run(t, "--skip-apple")
		if strings.Contains(out, password) {
			t.Fatalf("output leaks the worker password:\n%s", out)
		}
		return code, out
	}
	t.Run("pass", func(t *testing.T) {
		f := newPreflightFixture(t)
		f.env["BT_LIVE_ACTIVITY_DATABASE_URL_FILE"] = write(t, workerURL)
		f.env["BT_DATABASE_URL"] = h.Runtime.Config().ConnString()
		code, out := run(t, f)
		if code != 0 {
			t.Fatalf("exit=%d\n%s", code, out)
		}
		requireLines(t, out, "[PASS] worker database: schema 29, exact baby_tracker_live_activity inventory, same database as runtime", "RESULT: PASS")
	})
	t.Run("worker_only", func(t *testing.T) {
		f := newPreflightFixture(t)
		f.env["BT_LIVE_ACTIVITY_DATABASE_URL_FILE"] = write(t, workerURL)
		code, out := run(t, f)
		if code != 0 {
			t.Fatalf("exit=%d\n%s", code, out)
		}
		requireLines(t, out, "[PASS] worker database: schema 29, exact baby_tracker_live_activity inventory; same-database proof skipped (BT_DATABASE_URL or BT_DATABASE_URL_FILE not set)", "RESULT: PASS")
	})
	t.Run("wrong_role", func(t *testing.T) {
		f := newPreflightFixture(t)
		f.env["BT_LIVE_ACTIVITY_DATABASE_URL_FILE"] = write(t, h.Runtime.Config().ConnString())
		f.env["BT_DATABASE_URL"] = h.Runtime.Config().ConnString()
		code, out := run(t, f)
		if code != 1 {
			t.Fatalf("exit=%d\n%s", code, out)
		}
		requireLines(t, out, "[FAIL] worker database: database_role_separation_invalid", "RESULT: FAIL")
	})
	t.Run("wrong_password", func(t *testing.T) {
		f := newPreflightFixture(t)
		f.env["BT_LIVE_ACTIVITY_DATABASE_URL_FILE"] = write(t, strings.Replace(workerURL, password, "not-the-password", 1))
		f.env["BT_DATABASE_URL"] = h.Runtime.Config().ConnString()
		code, out := run(t, f)
		if code != 1 {
			t.Fatalf("exit=%d\n%s", code, out)
		}
		requireLines(t, out, "[FAIL] worker database: sqlstate_28P01", "RESULT: FAIL")
	})
	t.Run("schema_27", func(t *testing.T) {
		older := testkit.NewThrough(t, 27)
		f := newPreflightFixture(t)
		f.env["BT_LIVE_ACTIVITY_DATABASE_URL_FILE"] = write(t, older.LiveActivity.Config().ConnString())
		f.env["BT_DATABASE_URL"] = older.Runtime.Config().ConnString()
		code, out := run(t, f)
		if code != 1 {
			t.Fatalf("exit=%d\n%s", code, out)
		}
		requireLines(t, out, "[FAIL] worker database: live_activity_schema_unavailable", "RESULT: FAIL")
	})
}
