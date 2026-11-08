package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/limits"
)

type accountGateFunc func(context.Context) error

func (function accountGateFunc) Check(ctx context.Context) error { return function(ctx) }

type challengeServiceFunc func(context.Context) (identity.Challenge, error)

func (function challengeServiceFunc) Challenge(ctx context.Context) (identity.Challenge, error) {
	return function(ctx)
}

type limiterFunc func(context.Context, string, string, limits.Policy) (limits.Decision, error)

func (function limiterFunc) Take(ctx context.Context, scope, key string, policy limits.Policy) (limits.Decision, error) {
	return function(ctx, scope, key, policy)
}

type limiterStub struct {
	take     func(context.Context, string, string, limits.Policy) (limits.Decision, error)
	takeMany func(context.Context, []limits.Charge) (limits.Decision, error)
	// peek, when set, answers Peek; unset, Peek admits.
	peek func(context.Context, []limits.Charge) (limits.Decision, error)
}

func (stub limiterStub) Take(ctx context.Context, scope, key string, policy limits.Policy) (limits.Decision, error) {
	return stub.take(ctx, scope, key, policy)
}

func (stub limiterStub) TakeMany(ctx context.Context, charges []limits.Charge) (limits.Decision, error) {
	return stub.takeMany(ctx, charges)
}

func (stub limiterStub) Peek(ctx context.Context, charges []limits.Charge) (limits.Decision, error) {
	if stub.peek == nil {
		return limits.Decision{Allowed: true}, nil
	}
	return stub.peek(ctx, charges)
}

type sessionServiceStub struct {
	exchange       func(context.Context, identity.ExchangeRequest) (auth.SessionBundle, error)
	refresh        func(context.Context, string) (auth.SessionBundle, error)
	refreshSubject func(context.Context, string) (auth.RefreshSubject, error)
	authenticate   func(context.Context, string) (access.Principal, error)
	logout         func(context.Context, access.Principal, string) (authority.Result, error)
	accessDigests  func(string) ([]authority.KeyedDigest, error)
	refreshDigests func(string) ([]authority.KeyedDigest, error)
}

func (stub sessionServiceStub) Exchange(ctx context.Context, input identity.ExchangeRequest) (auth.SessionBundle, error) {
	return stub.exchange(ctx, input)
}
func (stub sessionServiceStub) Refresh(ctx context.Context, token string) (auth.SessionBundle, error) {
	return stub.refresh(ctx, token)
}
func (stub sessionServiceStub) ResolveRefreshSubject(ctx context.Context, token string) (auth.RefreshSubject, error) {
	return stub.refreshSubject(ctx, token)
}
func (stub sessionServiceStub) Authenticate(ctx context.Context, token string) (access.Principal, error) {
	return stub.authenticate(ctx, token)
}
func (stub sessionServiceStub) Logout(ctx context.Context, principal access.Principal, commandID string) (authority.Result, error) {
	return stub.logout(ctx, principal, commandID)
}

// AccessDigests is a stand-in digest unless the stub sets its own: the
// sync stubs authorize with a fixed principal (stubAuthorize), not by the
// digest.
func (stub sessionServiceStub) AccessDigests(token string) ([]authority.KeyedDigest, error) {
	if stub.accessDigests != nil {
		return stub.accessDigests(token)
	}
	return []authority.KeyedDigest{{KeyID: "stub"}}, nil
}

// RefreshDigests is one stand-in digest of the token (its SHA-256 under key
// "stub") unless the stub sets its own; an empty token has none.
func (stub sessionServiceStub) RefreshDigests(token string) ([]authority.KeyedDigest, error) {
	if stub.refreshDigests != nil {
		return stub.refreshDigests(token)
	}
	if token == "" {
		return nil, auth.ErrSessionRevoked
	}
	return []authority.KeyedDigest{{KeyID: "stub", Sum: sha256.Sum256([]byte(token))}}, nil
}

type notificationServiceFunc func(context.Context, string) error

func (function notificationServiceFunc) Handle(ctx context.Context, body string) error {
	return function(ctx, body)
}

func (function limiterFunc) TakeMany(context.Context, []limits.Charge) (limits.Decision, error) {
	return limits.Decision{Allowed: true}, nil
}

func (function limiterFunc) Peek(context.Context, []limits.Charge) (limits.Decision, error) {
	return limits.Decision{Allowed: true}, nil
}

func authHandler(t *testing.T, gate AccountGate, limiter limits.Limiter, service ChallengeService, logs io.Writer) http.Handler {
	t.Helper()
	resolver, err := limits.NewClientIPResolver([]netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")})
	if err != nil {
		t.Fatal(err)
	}
	return New(Options{
		Logger:                 slog.New(slog.NewJSONHandler(logs, nil)),
		Gate:                   gate,
		Limiter:                limiter,
		ClientIPResolver:       resolver,
		ChallengeService:       service,
		ChallengePolicy:        limits.Policy{Count: 30, Window: 10 * time.Minute},
		CanonicalOrigin:        "https://sync.example.test",
		AppleAuthorizationMode: "identity_token_only",
		InstanceID:             "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d",
		RecoveryEpoch:          1,
	})
}

func sessionHandlerOptions(t *testing.T, service SessionService, mode string) Options {
	t.Helper()
	resolver, err := limits.NewClientIPResolver([]netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")})
	if err != nil {
		t.Fatal(err)
	}
	policies := limits.DefaultPolicies()
	return Options{
		Gate: accountGateFunc(func(context.Context) error { return nil }), SessionService: service,
		AppleAuthorizationMode: mode, ClientIPResolver: resolver,
		Limiter: limiterFunc(func(context.Context, string, string, limits.Policy) (limits.Decision, error) {
			return limits.Decision{Allowed: true}, nil
		}),
		ExchangePolicy: policies[limits.ScopeAuthExchangeIP], RefreshIPPolicy: policies[limits.ScopeAuthRefreshIP],
		RefreshAccountPolicy: policies[limits.ScopeAuthRefreshAccount], RefreshDevicePolicy: policies[limits.ScopeAuthRefreshDevice],
	}
}

func TestAuthChallengeRouteIsBoundedGatedRateLimitedAndNoStore(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	var limitedKey string
	handler := authHandler(t,
		accountGateFunc(func(context.Context) error { return nil }),
		limiterFunc(func(_ context.Context, scope, key string, policy limits.Policy) (limits.Decision, error) {
			if scope != limits.ScopeAuthChallengeIP || policy.Count != 30 || policy.Window != 10*time.Minute {
				t.Fatalf("limit scope=%q policy=%+v", scope, policy)
			}
			limitedKey = key
			return limits.Decision{Allowed: true, Remaining: 29}, nil
		}),
		challengeServiceFunc(func(context.Context) (identity.Challenge, error) {
			return identity.Challenge{Challenge: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8", ServerInstanceID: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", CanonicalOrigin: "https://sync.example.test", ExpiresAt: now.Add(5 * time.Minute)}, nil
		}), io.Discard)

	request := httptest.NewRequest(http.MethodPost, "/v1/auth/challenge", strings.NewReader(`{"protocol_version":"v1","capabilities":["identity-v1"]}`))
	request.RemoteAddr = "127.0.0.1:43210"
	request.Header.Set("X-Forwarded-For", "203.0.113.8")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" || limitedKey != "203.0.113.8" {
		t.Fatalf("status=%d cache=%q key=%q body=%s", recorder.Code, recorder.Header().Get("Cache-Control"), limitedKey, recorder.Body.String())
	}
	var body struct {
		ProtocolVersion string `json:"protocol_version"`
		Challenge       string `json:"challenge"`
		ExpiresAt       string `json:"expires_at"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.ProtocolVersion != "v1" || body.Challenge == "" || body.ExpiresAt != "2026-09-20T12:05:00Z" {
		t.Fatalf("response=%+v err=%v", body, err)
	}

	oversized := `{"protocol_version":"v1","capabilities":[],"padding":"` + strings.Repeat("x", int(authBodyBytes)) + `"}`
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/auth/challenge", strings.NewReader(oversized)))
	if recorder.Code != http.StatusBadRequest || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("oversized status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestAuthChallengeFailsClosedAndReturnsRetryAfter(t *testing.T) {
	baseService := challengeServiceFunc(func(context.Context) (identity.Challenge, error) {
		return identity.Challenge{}, errors.New("must not be called")
	})
	tests := []struct {
		name      string
		gate      AccountGate
		limiter   limits.Limiter
		status    int
		code      string
		retryable bool
		retry     string
	}{
		{name: "gate", gate: accountGateFunc(func(context.Context) error { return errors.New("recovering") }), limiter: limiterFunc(func(context.Context, string, string, limits.Policy) (limits.Decision, error) {
			t.Fatal("limiter called")
			return limits.Decision{}, nil
		}), status: http.StatusServiceUnavailable, code: "not_ready", retryable: true},
		{name: "limiter dependency", gate: accountGateFunc(func(context.Context) error { return nil }), limiter: limiterFunc(func(context.Context, string, string, limits.Policy) (limits.Decision, error) {
			return limits.Decision{}, errors.New("database secret")
		}), status: http.StatusServiceUnavailable, code: "rate_limit_unavailable", retryable: true},
		{name: "denied", gate: accountGateFunc(func(context.Context) error { return nil }), limiter: limiterFunc(func(context.Context, string, string, limits.Policy) (limits.Decision, error) {
			return limits.Decision{Allowed: false, RetryAfter: 61 * time.Second}, nil
		}), status: http.StatusTooManyRequests, code: "rate_limited", retryable: true, retry: "61"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			handler := authHandler(t, test.gate, test.limiter, baseService, &logs)
			request := httptest.NewRequest(http.MethodPost, "/v1/auth/challenge", strings.NewReader(`{"protocol_version":"v1","capabilities":[]}`))
			request.RemoteAddr = "127.0.0.1:1234"
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != test.status || recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("Retry-After") != test.retry {
				t.Fatalf("status=%d cache=%q retry=%q body=%s", recorder.Code, recorder.Header().Get("Cache-Control"), recorder.Header().Get("Retry-After"), recorder.Body.String())
			}
			var body struct {
				Error struct {
					Code              string `json:"code"`
					Retryable         bool   `json:"retryable"`
					RetryAfterSeconds *int   `json:"retry_after_seconds"`
				} `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.Error.Code != test.code || body.Error.Retryable != test.retryable {
				t.Fatalf("body=%s err=%v", recorder.Body.String(), err)
			}
			if test.retry != "" && (body.Error.RetryAfterSeconds == nil || *body.Error.RetryAfterSeconds != 61) {
				t.Fatalf("retry extension=%v", body.Error.RetryAfterSeconds)
			}
		})
	}
}

func TestAuthErrorsNeverEchoSecrets(t *testing.T) {
	var logs bytes.Buffer
	secret := "distinctive-private-token-and-name"
	var limited bool
	handler := authHandler(t, accountGateFunc(func(context.Context) error { return nil }),
		limiterFunc(func(context.Context, string, string, limits.Policy) (limits.Decision, error) {
			limited = true
			return limits.Decision{Allowed: true}, nil
		}),
		challengeServiceFunc(func(context.Context) (identity.Challenge, error) { return identity.Challenge{}, errors.New(secret) }), &logs)
	request := httptest.NewRequest(http.MethodPost, "/v1/auth/challenge", strings.NewReader(`{"protocol_version":"v1","capabilities":[],"`+secret+`":true}`))
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if !limited {
		t.Fatal("malformed request bypassed the challenge IP limit")
	}
	if strings.Contains(recorder.Body.String(), secret) || strings.Contains(logs.String(), secret) {
		t.Fatalf("secret echoed: response=%s logs=%s", recorder.Body.String(), logs.String())
	}
}

func TestSessionHTTPRoutesEncodeBundleAndLogoutAuthenticatedDevice(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	bundle := auth.SessionBundle{
		AccessToken: "access", RefreshToken: "refresh", DeviceID: "40000000-0000-4000-8000-000000000001", DeviceCredential: strings.Repeat("a", 43),
		RecoveryEpoch: 7, ExpiresAt: now.Add(10 * time.Minute), ReauthenticateAt: now.Add(24 * time.Hour),
		Account:    auth.AccountView{ID: "30000000-0000-4000-8000-000000000001", DisplayName: "Parent", ProfileRequired: false},
		FamilySlot: &auth.FamilySlot{FamilyID: "50000000-0000-4000-8000-000000000001", State: "active"},
	}
	principal := access.Principal{AccountID: bundle.Account.ID, DeviceID: bundle.DeviceID, SessionID: "60000000-0000-4000-8000-000000000001", RecoveryEpoch: 7, SecurityRevision: 3, Scope: "account"}
	var gotExchange identity.ExchangeRequest
	var gotRefresh, gotBearer, gotLogout string
	service := sessionServiceStub{
		exchange: func(_ context.Context, request identity.ExchangeRequest) (auth.SessionBundle, error) {
			gotExchange = request
			return bundle, nil
		},
		refresh: func(_ context.Context, token string) (auth.SessionBundle, error) {
			gotRefresh = token
			continued := bundle
			continued.DeviceCredential = ""
			return continued, nil
		},
		refreshSubject: func(_ context.Context, token string) (auth.RefreshSubject, error) {
			return auth.RefreshSubject{AccountID: bundle.Account.ID, DeviceID: bundle.DeviceID}, nil
		},
		authenticate: func(_ context.Context, token string) (access.Principal, error) {
			gotBearer = token
			return principal, nil
		},
		logout: func(_ context.Context, got access.Principal, commandID string) (authority.Result, error) {
			if got != principal {
				t.Fatalf("principal=%+v", got)
			}
			gotLogout = commandID
			return authority.Result{CommandID: commandID, Revision: 4}, nil
		},
	}
	options := sessionHandlerOptions(t, service, "identity_token_only")
	options.CanonicalOrigin = "https://sync.example.test"
	handler := New(options)

	exchangeBody := `{"protocol_version":"v1","capabilities":["identity-v1"],"command_id":"30000000-0000-4000-8000-000000000002","challenge":"challenge","identity_token":"token","authorization_code":"","installation_id":"install","profile":{"display_name":"Parent","confirmed":true},"device_continuity":null}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/auth/apple", strings.NewReader(exchangeBody)))
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" || gotExchange.CommandID == "" {
		t.Fatalf("exchange status=%d cache=%q body=%s request=%+v", recorder.Code, recorder.Header().Get("Cache-Control"), recorder.Body.String(), gotExchange)
	}
	var response struct {
		ProtocolVersion  string `json:"protocol_version"`
		RecoveryEpoch    uint64 `json:"recovery_epoch"`
		DeviceCredential string `json:"device_credential"`
		Account          struct {
			DisplayName string `json:"display_name"`
		} `json:"account"`
		FamilySlot *struct {
			State string `json:"state"`
		} `json:"family_slot"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.ProtocolVersion != "v1" || response.RecoveryEpoch != 7 || response.DeviceCredential == "" || response.Account.DisplayName != "Parent" || response.FamilySlot == nil {
		t.Fatalf("response=%+v err=%v body=%s", response, err, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/auth/refresh", strings.NewReader(`{"protocol_version":"v1","capabilities":["identity-v1"],"refresh_token":"refresh"}`)))
	if recorder.Code != http.StatusOK || gotRefresh != "refresh" || strings.Contains(recorder.Body.String(), "device_credential") {
		t.Fatalf("refresh status=%d token=%q body=%s", recorder.Code, gotRefresh, recorder.Body.String())
	}

	logoutID := "30000000-0000-4000-8000-000000000003"
	request := httptest.NewRequest(http.MethodPost, "/v1/auth/logout", strings.NewReader(`{"protocol_version":"v1","capabilities":["identity-v1"],"command_id":"`+logoutID+`"}`))
	request.Header.Set("Authorization", "Bearer bearer-token")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent || recorder.Body.Len() != 0 || gotBearer != "bearer-token" || gotLogout != logoutID {
		t.Fatalf("logout status=%d bearer=%q command=%q body=%s", recorder.Code, gotBearer, gotLogout, recorder.Body.String())
	}
}

func TestSessionHTTPModeAndNotificationRouting(t *testing.T) {
	service := sessionServiceStub{exchange: func(context.Context, identity.ExchangeRequest) (auth.SessionBundle, error) {
		return auth.SessionBundle{}, errors.New("must not exchange")
	}}
	gate := accountGateFunc(func(context.Context) error { return nil })
	selfOptions := sessionHandlerOptions(t, service, "identity_token_only")
	selfOptions.Gate = gate
	selfHosted := New(selfOptions)
	withCode := `{"protocol_version":"v1","capabilities":["identity-v1"],"command_id":"30000000-0000-4000-8000-000000000002","challenge":"challenge","identity_token":"token","authorization_code":"secret-code","installation_id":"install","profile":null,"device_continuity":null}`
	recorder := httptest.NewRecorder()
	selfHosted.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/auth/apple", strings.NewReader(withCode)))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"code":"hosted_only_field"`) {
		t.Fatalf("self-host code status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	selfHosted.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/auth/apple/notifications", strings.NewReader(`{"payload":"signed"}`)))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("self-host notification status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	var notificationBody string
	hostedOptions := sessionHandlerOptions(t, service, "code_exchange_required")
	hostedOptions.Gate = gate
	hostedOptions.AppleNotifications = notificationServiceFunc(func(_ context.Context, body string) error { notificationBody = body; return nil })
	hosted := New(hostedOptions)
	recorder = httptest.NewRecorder()
	hosted.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/auth/apple/notifications", strings.NewReader(`{"payload":"signed"}`)))
	if recorder.Code != http.StatusNoContent || notificationBody != `{"payload":"signed"}` || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("hosted notification status=%d cache=%q body=%q got=%q", recorder.Code, recorder.Header().Get("Cache-Control"), recorder.Body.String(), notificationBody)
	}
}

func TestAuthQuarantineReturnsStableCodeWithoutCallingService(t *testing.T) {
	called := false
	service := sessionServiceStub{exchange: func(context.Context, identity.ExchangeRequest) (auth.SessionBundle, error) {
		called = true
		return auth.SessionBundle{}, nil
	}}
	options := sessionHandlerOptions(t, service, "identity_token_only")
	options.Gate = accountGateFunc(func(context.Context) error {
		return fmt.Errorf("private recovery detail: %w", authority.ErrAuthorizationQuarantine)
	})

	recorder := httptest.NewRecorder()
	New(options).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/auth/apple", strings.NewReader(`{}`)))

	if called {
		t.Fatal("session service called while authorization was quarantined")
	}
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(recorder.Body.String(), `"code":"authorization_quarantine"`) ||
		strings.Contains(recorder.Body.String(), "private recovery detail") {
		t.Fatalf("status=%d cache=%q body=%s", recorder.Code, recorder.Header().Get("Cache-Control"), recorder.Body.String())
	}
}

func TestAuthHTTPErrorMatrix(t *testing.T) {
	for _, test := range []struct {
		code       string
		status     int
		retryAfter string
	}{
		{code: "invalid_request", status: 400}, {code: "device_continuity_invalid", status: 400},
		{code: "unsupported_capability", status: 409},
		{code: "invalid_apple_proof", status: 401}, {code: "refresh_reused", status: 401},
		{code: "command_id_reused", status: 409}, {code: "credential_conflict", status: 409},
		{code: "rate_limited", status: 429, retryAfter: "1"},
		{code: "security_busy", status: 503, retryAfter: "1"}, {code: "credential_rotation_busy", status: 503, retryAfter: "1"},
		{code: "authorization_quarantine", status: 503}, {code: "hosted_adapter_unavailable", status: 503},
		{code: "credential_key_unavailable", status: 503}, {code: "service_unavailable", status: 503},
	} {
		t.Run(test.code, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/", nil)
			writeAuthCode(recorder, request, test.code)
			if recorder.Code != test.status || recorder.Header().Get("Retry-After") != test.retryAfter || recorder.Header().Get("Cache-Control") != "" {
				t.Fatalf("status=%d retry=%q body=%s", recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
			}
			if test.status == http.StatusUnauthorized && recorder.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatal("401 omitted bearer challenge")
			}
			if test.code == "rate_limited" && !strings.Contains(recorder.Body.String(), `"retry_after_seconds":1`) {
				t.Fatalf("429 omitted retry extension: %s", recorder.Body.String())
			}
		})
	}
}

func TestAuthErrorsNeverExposeProviderText(t *testing.T) {
	secret := "provider-private-invalid-grant-description"
	var logs bytes.Buffer
	service := sessionServiceStub{
		refreshSubject: func(context.Context, string) (auth.RefreshSubject, error) {
			return auth.RefreshSubject{AccountID: "30000000-0000-4000-8000-000000000001", DeviceID: "40000000-0000-4000-8000-000000000001"}, nil
		},
		refresh: func(context.Context, string) (auth.SessionBundle, error) {
			return auth.SessionBundle{}, errors.New(secret)
		},
	}
	options := sessionHandlerOptions(t, service, "identity_token_only")
	options.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	handler := New(options)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/auth/refresh", strings.NewReader(`{"protocol_version":"v1","capabilities":["identity-v1"],"refresh_token":"`+strings.Repeat("a", 43)+`"}`)))
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d cache=%q body=%s", recorder.Code, recorder.Header().Get("Cache-Control"), recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), secret) || strings.Contains(logs.String(), secret) {
		t.Fatalf("provider text leaked: body=%s logs=%s", recorder.Body.String(), logs.String())
	}
}

// A request that lost a race (security_busy: a twin registration, or a
// sign-in whose account was deleted under it) is a retryable 503 with
// Retry-After and a WARN line, never an ERROR line: it is not a server fault.
func TestLostRaceIsRetryableWithoutAnErrorLine(t *testing.T) {
	var logs bytes.Buffer
	service := sessionServiceStub{
		refreshSubject: func(context.Context, string) (auth.RefreshSubject, error) {
			return auth.RefreshSubject{AccountID: "30000000-0000-4000-8000-000000000001", DeviceID: "40000000-0000-4000-8000-000000000001"}, nil
		},
		refresh: func(context.Context, string) (auth.SessionBundle, error) {
			return auth.SessionBundle{}, fmt.Errorf("authority: lost: %w", authority.ErrSecurityBusy)
		},
	}
	options := sessionHandlerOptions(t, service, "identity_token_only")
	options.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	recorder := httptest.NewRecorder()
	New(options).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/auth/refresh",
		strings.NewReader(`{"protocol_version":"v1","capabilities":["identity-v1"],"refresh_token":"`+strings.Repeat("a", 43)+`"}`)))
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "1" ||
		!strings.Contains(recorder.Body.String(), `"code":"security_busy"`) || !strings.Contains(recorder.Body.String(), `"retryable":true`) {
		t.Fatalf("status=%d retry=%q body=%s", recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
	}
	if lines := errorLines(t, logs.String()); len(lines) != 0 {
		t.Fatalf("ERROR lines for a lost race: %v", lines)
	}
	warned := 0
	for _, line := range logLines(t, logs.String()) {
		if line["level"] == "WARN" && line["msg"] == "request lost a race" && line["code"] == "security_busy" {
			warned++
		}
	}
	if warned != 1 {
		t.Fatalf("lost-race WARN lines %d, want 1: %s", warned, logs.String())
	}
}

func TestCredentialRotationBusyReturns503WithRetryAfterOne(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeAuthCode(recorder, httptest.NewRequest(http.MethodPost, "/v1/auth/apple", nil), "credential_rotation_busy")
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "1" {
		t.Fatalf("status=%d retry=%q body=%s", recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
	}
}

func TestSessionRateLimitsRunBeforeProofAndRefreshMutation(t *testing.T) {
	accountID := "30000000-0000-4000-8000-000000000001"
	deviceID := "40000000-0000-4000-8000-000000000001"
	order := []string{}
	service := sessionServiceStub{
		exchange: func(context.Context, identity.ExchangeRequest) (auth.SessionBundle, error) {
			order = append(order, "exchange")
			return auth.SessionBundle{}, auth.ErrUnavailable
		},
		refreshSubject: func(context.Context, string) (auth.RefreshSubject, error) {
			order = append(order, "subject")
			return auth.RefreshSubject{AccountID: accountID, DeviceID: deviceID}, nil
		},
		refresh: func(context.Context, string) (auth.SessionBundle, error) {
			order = append(order, "refresh")
			return auth.SessionBundle{}, auth.ErrUnavailable
		},
	}
	options := sessionHandlerOptions(t, service, "identity_token_only")
	options.Limiter = limiterStub{
		take: func(_ context.Context, scope, _ string, policy limits.Policy) (limits.Decision, error) {
			order = append(order, scope)
			if policy.Count == 0 {
				t.Fatalf("zero policy for %s", scope)
			}
			return limits.Decision{Allowed: true}, nil
		},
		takeMany: func(_ context.Context, charges []limits.Charge) (limits.Decision, error) {
			order = append(order, "refresh-subject-limits")
			if len(charges) != 2 || charges[0].Scope != limits.ScopeAuthRefreshAccount || charges[0].Key != accountID ||
				charges[1].Scope != limits.ScopeAuthRefreshDevice || charges[1].Key != deviceID {
				t.Fatalf("charges=%+v", charges)
			}
			return limits.Decision{Allowed: true}, nil
		},
	}
	handler := New(options)
	exchangeBody := `{"protocol_version":"v1","capabilities":["identity-v1"],"command_id":"30000000-0000-4000-8000-000000000002","challenge":"challenge","identity_token":"token","installation_id":"install"}`
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/auth/apple", strings.NewReader(exchangeBody)))
	refreshBody := `{"protocol_version":"v1","capabilities":["identity-v1"],"refresh_token":"` + strings.Repeat("a", 43) + `"}`
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/auth/refresh", strings.NewReader(refreshBody)))
	want := []string{limits.ScopeAuthExchangeIP, "exchange", limits.ScopeAuthRefreshIP, "subject", "refresh-subject-limits", "refresh"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order=%v want=%v", order, want)
	}
}

func TestSessionRateLimitDenialReturnsMatchingRetryMetadata(t *testing.T) {
	called := false
	service := sessionServiceStub{exchange: func(context.Context, identity.ExchangeRequest) (auth.SessionBundle, error) {
		called = true
		return auth.SessionBundle{}, nil
	}}
	options := sessionHandlerOptions(t, service, "identity_token_only")
	options.Limiter = limiterStub{
		take: func(context.Context, string, string, limits.Policy) (limits.Decision, error) {
			return limits.Decision{Allowed: false, RetryAfter: 61 * time.Second}, nil
		},
		takeMany: func(context.Context, []limits.Charge) (limits.Decision, error) {
			t.Fatal("unexpected subject limits")
			return limits.Decision{}, nil
		},
	}
	handler := New(options)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/auth/apple", strings.NewReader(`{"not":"decoded"}`)))
	if called || recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "61" ||
		!strings.Contains(recorder.Body.String(), `"retry_after_seconds":61`) {
		t.Fatalf("called=%t status=%d retry=%q body=%s", called, recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
	}
}
