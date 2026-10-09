package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/protocol"
)

type readinessFunc func(context.Context) error

func (f readinessFunc) Check(ctx context.Context) error { return f(ctx) }

type serverStateFunc func(context.Context) (string, uint64, error)

func (f serverStateFunc) ServerState(ctx context.Context) (string, uint64, error) { return f(ctx) }

func newHandler(t *testing.T, readiness Readiness, logOutput io.Writer) http.Handler {
	t.Helper()
	return New(Options{
		Logger:                 slog.New(slog.NewJSONHandler(logOutput, nil)),
		Readiness:              readiness,
		InstanceID:             "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d",
		RecoveryEpoch:          7,
		CanonicalOrigin:        "https://sync.example.test",
		AppleAuthorizationMode: "identity_token_only",
		ServerState: serverStateFunc(func(context.Context) (string, uint64, error) {
			return "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", 7, nil
		}),
	})
}

func TestServerStateIsReadForEveryRequest(t *testing.T) {
	epoch := uint64(4)
	handler := New(Options{CanonicalOrigin: "https://sync.example.test", AppleAuthorizationMode: "identity_token_only", ServerState: serverStateFunc(func(context.Context) (string, uint64, error) {
		return "123e4567-e89b-42d3-a456-426614174000", epoch, nil
	})})
	for _, want := range []uint64{4, 5} {
		epoch = want
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/server", nil))
		var body struct {
			Epoch uint64 `json:"recovery_epoch"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Epoch != want {
			t.Fatalf("server state captured startup epoch: got %d want %d", body.Epoch, want)
		}
	}
}

func TestAuthRouteSetIsModeSpecific(t *testing.T) {
	all := Routes()
	hosted := RoutesForAuthorizationMode("code_exchange_required")
	if !reflect.DeepEqual(hosted, all) {
		t.Fatalf("hosted routes=%v want all=%v", hosted, all)
	}
	selfHosted := RoutesForAuthorizationMode("identity_token_only")
	wantSelfHosted := make([]Route, 0, len(all)-1)
	for _, route := range all {
		if route.Pattern != "/v1/auth/apple/notifications" {
			wantSelfHosted = append(wantSelfHosted, route)
		}
	}
	if !reflect.DeepEqual(selfHosted, wantSelfHosted) {
		t.Fatalf("self-host routes=%v want=%v", selfHosted, wantSelfHosted)
	}
}

func TestServerMetadataRejectsUnsafeIntegerEpoch(t *testing.T) {
	handler := New(Options{CanonicalOrigin: "https://sync.example.test", AppleAuthorizationMode: "identity_token_only", ServerState: serverStateFunc(func(context.Context) (string, uint64, error) {
		return "123e4567-e89b-42d3-a456-426614174000", 9007199254740992, nil
	})})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/server", nil))
	if recorder.Code != http.StatusServiceUnavailable || strings.Contains(recorder.Body.String(), "9007199254740992") {
		t.Fatalf("unsafe epoch was published: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestServerMetadataPublishesEffectiveSyncLimitsWhenConfigured(t *testing.T) {
	limits := protocol.DefaultLimits()
	handler := New(Options{
		CanonicalOrigin: "https://sync.example.test", AppleAuthorizationMode: "identity_token_only", SyncLimits: &limits,
		SourceVersion: "abc123", SourceURL: "https://git.example.test/baby-tracker",
		ServerState: serverStateFunc(func(context.Context) (string, uint64, error) {
			return "123e4567-e89b-42d3-a456-426614174000", 1, nil
		}),
	})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/server", nil))
	var response protocol.ServerInfo
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Limits == nil || response.Limits.SyncBatchSize != 100 {
		t.Fatalf("server limits = %+v", response.Limits)
	}
	if response.SourceVersion != "abc123" || response.SourceURL != "https://git.example.test/baby-tracker" ||
		response.MaxItemsPerPage != limits.SyncPageSize || response.MaxEncodedBytes != limits.SyncPageBytes {
		t.Fatalf("server deployment metadata = %+v", response)
	}
}

func TestOperationalEndpoints(t *testing.T) {
	tests := []struct {
		name      string
		readiness error
		path      string
		status    int
		want      string
	}{
		{name: "liveness does not depend on readiness", readiness: errors.New("database unavailable"), path: "/healthz", status: http.StatusOK, want: `{"status":"ok"}`},
		{name: "ready when checker succeeds", path: "/readyz", status: http.StatusOK, want: `{"status":"ok"}`},
		{name: "not ready has stable code", readiness: errors.New("database unavailable"), path: "/readyz", status: http.StatusServiceUnavailable, want: `{"error":{"code":"not_ready","message":"Service is not ready","request_id":"","retryable":true}}`},
		{name: "server metadata is the entire public surface", path: "/v1/server", status: http.StatusOK, want: `{"api_versions":["v1"],"capabilities":["identity-v1","family-lifecycle-v1","recovery-v1","records-v1","snapshot-v1","timer-commands-v1","import-resolver-v1","live-hints-v1"],"canonical_origin":"https://sync.example.test","instance_id":"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","recovery_epoch":7,"apple_authorization_mode":"identity_token_only"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			h := newHandler(t, readinessFunc(func(context.Context) error { return tt.readiness }), &logs)
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d", rec.Code, tt.status)
			}
			var got any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			var want any
			if err := json.Unmarshal([]byte(tt.want), &want); err != nil {
				t.Fatalf("decode expected response: %v", err)
			}
			if tt.path == "/readyz" && tt.readiness != nil {
				got.(map[string]any)["error"].(map[string]any)["request_id"] = ""
			}
			if !sameJSON(got, want) {
				t.Fatalf("body = %s, want %s", rec.Body.Bytes(), tt.want)
			}
		})
	}
}

func TestAccessLogAllowsOnlyOperationalMetadata(t *testing.T) {
	var logs bytes.Buffer
	h := newHandler(t, readinessFunc(func(context.Context) error { return nil }), &logs)
	body := `{"baby_name":"Ari"}`
	req := httptest.NewRequest(http.MethodGet, "/v1/server", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("Cookie", "session=top-secret")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	logLine := logs.String()
	for _, forbidden := range []string{"secret-token", "top-secret", "Ari", "baby_name", "Authorization", "Cookie"} {
		if strings.Contains(logLine, forbidden) {
			t.Errorf("access log leaked %q: %s", forbidden, logLine)
		}
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(logLine), &entry); err != nil {
		t.Fatalf("decode access log: %v", err)
	}
	for _, field := range []string{"request_id", "method", "route", "status", "duration"} {
		if _, ok := entry[field]; !ok {
			t.Errorf("access log omitted %q: %s", field, logLine)
		}
	}
	if entry["method"] != http.MethodGet || entry["route"] != "/v1/server" || entry["status"] != float64(http.StatusOK) {
		t.Errorf("unexpected access metadata: %s", logLine)
	}
}

func TestRequestIDIsValidatedOrGenerated(t *testing.T) {
	h := newHandler(t, readinessFunc(func(context.Context) error { return nil }), io.Discard)
	valid := "0123456789abcdef0123456789abcdef"
	for _, supplied := range []string{valid, "not-a-valid-request-id"} {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set("X-Request-ID", supplied)
		rec := httptest.NewRecorder()

		h.ServeHTTP(rec, req)

		got := rec.Header().Get("X-Request-ID")
		if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(got) {
			t.Errorf("request ID = %q, want 32 lowercase hex characters", got)
		}
		if supplied == valid && got != supplied {
			t.Errorf("valid request ID = %q, want it preserved", got)
		}
		if supplied != valid && got == supplied {
			t.Errorf("invalid request ID %q was accepted", supplied)
		}
	}
}

func TestPanicUsesErrorEnvelope(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	h := withMiddleware(logger, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("sensitive panic detail")
	}))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	var body struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Error.Code != "internal_error" || body.Error.Message != "Internal server error" || !body.Error.Retryable || body.Error.RequestID == "" {
		t.Fatalf("panic response = %s", rec.Body.Bytes())
	}
	if strings.Contains(rec.Body.String(), "sensitive panic detail") || strings.Contains(logs.String(), "sensitive panic detail") {
		t.Fatal("panic detail leaked")
	}
	// One ERROR line names the recovered value by type only.
	var panics []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatal(err)
		}
		if line["msg"] == "request panicked" {
			panics = append(panics, line)
		}
	}
	if len(panics) != 1 || panics[0]["level"] != "ERROR" || panics[0]["route"] != "unmatched" || panics[0]["panic_type"] != "string" || panics[0]["request_id"] == "" {
		t.Fatalf("panic lines=%v", panics)
	}
}

func TestRunShutsDownOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	server := &http.Server{Addr: "127.0.0.1:0", Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}
	shutdown := make(chan struct{})
	server.RegisterOnShutdown(func() { close(shutdown) })
	result := make(chan error, 1)
	go func() { result <- Run(ctx, server) }()

	cancel()

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
	select {
	case <-shutdown:
	case <-time.After(time.Second):
		t.Fatal("Run returned without shutting down the HTTP server")
	}
}

func sameJSON(got, want any) bool {
	gotBytes, _ := json.Marshal(got)
	wantBytes, _ := json.Marshal(want)
	return bytes.Equal(gotBytes, wantBytes)
}
