package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/testkit"
)

type recoveryServiceStub struct {
	resolve func(context.Context, auth.RecoverableRefreshInput) (auth.RefreshSubject, error)
	refresh func(context.Context, auth.RecoverableRefreshInput) (auth.RecoveredSessionBundle, error)
	ack     func(context.Context, access.Principal, string, identity.DeviceContinuityClaim) error
}

func (s recoveryServiceStub) ResolveRecoverableRefreshSubject(ctx context.Context, i auth.RecoverableRefreshInput) (auth.RefreshSubject, error) {
	return s.resolve(ctx, i)
}
func (s recoveryServiceStub) RefreshRecoverable(ctx context.Context, i auth.RecoverableRefreshInput) (auth.RecoveredSessionBundle, error) {
	return s.refresh(ctx, i)
}
func (s recoveryServiceStub) AcknowledgeRefresh(ctx context.Context, p access.Principal, a string, c identity.DeviceContinuityClaim) error {
	return s.ack(ctx, p, a, c)
}

const recoveryBody = `{"protocol_version":"v1","capabilities":["identity-v1","refresh-recovery-v1"],"refresh_token":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","refresh_attempt_id":"11111111-1111-4111-8111-111111111111","device_continuity":{"device_id":"22222222-2222-4222-8222-222222222222","credential":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"}}`

func TestRecoveryHTTPStrictContract(t *testing.T) {
	rotations := 0
	options := sessionHandlerOptions(t, nil, "identity_token_only")
	options.RecoveryService = recoveryServiceStub{resolve: func(context.Context, auth.RecoverableRefreshInput) (auth.RefreshSubject, error) {
		t.Fatal("resolver called")
		return auth.RefreshSubject{}, nil
	}, refresh: func(context.Context, auth.RecoverableRefreshInput) (auth.RecoveredSessionBundle, error) {
		rotations++
		return auth.RecoveredSessionBundle{}, nil
	}}
	options.RecoveryEnabled = true
	options.RecoveryReadiness = accountGateFunc(func(context.Context) error { return nil })
	h := New(options)
	for _, body := range []string{
		strings.Replace(recoveryBody, `"v1"`, `"v2"`, 1),
		strings.Replace(recoveryBody, `"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"`, `"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","extra":1`, 1),
		strings.Replace(recoveryBody, `"11111111-1111-4111-8111-111111111111"`, `"11111111-1111-4111-8111-11111111111A"`, 1),
		strings.Replace(recoveryBody, `,"refresh-recovery-v1"`, ``, 1),
		strings.Replace(recoveryBody, `"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"`, `"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","unknown":1`, 1),
		recoveryBody + strings.Repeat(" ", int(authBodyBytes)),
	} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/auth/refresh-recoverable", strings.NewReader(body)))
		if r.Code < 400 || rotations != 0 {
			t.Fatalf("status=%d rotations=%d body=%s", r.Code, rotations, r.Body.String())
		}
	}
}

func TestRecoveryHTTPQuotaBeforeRotation(t *testing.T) {
	rotations := 0
	options := sessionHandlerOptions(t, nil, "identity_token_only")
	options.RecoveryEnabled = true
	options.RecoveryReadiness = accountGateFunc(func(context.Context) error { return nil })
	options.RecoveryService = recoveryServiceStub{resolve: func(context.Context, auth.RecoverableRefreshInput) (auth.RefreshSubject, error) {
		return auth.RefreshSubject{AccountID: "account", DeviceID: "device"}, nil
	}, refresh: func(context.Context, auth.RecoverableRefreshInput) (auth.RecoveredSessionBundle, error) {
		rotations++
		return auth.RecoveredSessionBundle{}, nil
	}}
	options.Limiter = limiterStub{take: func(context.Context, string, string, limits.Policy) (limits.Decision, error) {
		return limits.Decision{Allowed: true}, nil
	}, takeMany: func(_ context.Context, charges []limits.Charge) (limits.Decision, error) {
		if len(charges) != 2 || charges[0].Key != "account" || charges[1].Key != "device" {
			t.Fatalf("charges=%+v", charges)
		}
		return limits.Decision{Allowed: false}, nil
	}}
	r := httptest.NewRecorder()
	New(options).ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/auth/refresh-recoverable", strings.NewReader(recoveryBody)))
	if r.Code != 429 || rotations != 0 {
		t.Fatalf("status=%d rotations=%d body=%s", r.Code, rotations, r.Body.String())
	}
}

func TestRecoveryHTTPIPQuotaRefusesBeforeSubjectResolution(t *testing.T) {
	options := sessionHandlerOptions(t, nil, "identity_token_only")
	options.RecoveryEnabled = true
	options.RecoveryReadiness = accountGateFunc(func(context.Context) error { return nil })
	options.RecoveryService = recoveryServiceStub{resolve: func(context.Context, auth.RecoverableRefreshInput) (auth.RefreshSubject, error) {
		t.Fatal("subject resolved after IP denial")
		return auth.RefreshSubject{}, nil
	}}
	options.Limiter = limiterStub{take: func(_ context.Context, scope, key string, _ limits.Policy) (limits.Decision, error) {
		if scope != limits.ScopeAuthRefreshIP {
			t.Fatalf("scope=%s", scope)
		}
		return limits.Decision{Allowed: false}, nil
	}, takeMany: func(context.Context, []limits.Charge) (limits.Decision, error) {
		t.Fatal("account/device charged after IP denial")
		return limits.Decision{}, nil
	}}
	r := httptest.NewRecorder()
	New(options).ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/auth/refresh-recoverable", strings.NewReader(recoveryBody)))
	if r.Code != 429 {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
}

func TestRecoveryHTTPConsumedParentQuotaDenialPreservesChild(t *testing.T) {
	h := testkit.New(t)
	const attempt = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	input := auth.RecoverableRefreshInput{RefreshToken: h.RefreshToken("owner"), AttemptID: attempt, DeviceContinuity: identity.DeviceContinuityClaim{DeviceID: h.Owner.DeviceID, Credential: h.DeviceCredential("owner")}}
	first, err := h.Auth.RefreshRecoverable(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	options := sessionHandlerOptions(t, h.Auth, "identity_token_only")
	options.RecoveryEnabled = true
	options.RecoveryReadiness = accountGateFunc(func(context.Context) error { return nil })
	options.RecoveryService = h.Auth
	options.Limiter = limiterStub{take: func(context.Context, string, string, limits.Policy) (limits.Decision, error) {
		return limits.Decision{Allowed: true}, nil
	}, takeMany: func(_ context.Context, charges []limits.Charge) (limits.Decision, error) {
		if len(charges) != 2 || charges[0].Key != h.Owner.AccountID || charges[1].Key != h.Owner.DeviceID {
			t.Fatalf("charges=%+v", charges)
		}
		return limits.Decision{Allowed: false}, nil
	}}
	body, err := json.Marshal(map[string]any{"protocol_version": "v1", "capabilities": []string{"identity-v1", "refresh-recovery-v1"}, "refresh_token": input.RefreshToken, "refresh_attempt_id": attempt, "device_continuity": map[string]string{"device_id": input.DeviceContinuity.DeviceID, "credential": input.DeviceContinuity.Credential}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRecorder()
	New(options).ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/auth/refresh-recoverable", strings.NewReader(string(body))))
	if r.Code != 429 {
		t.Fatalf("status=%d", r.Code)
	}
	ackBody, err := json.Marshal(map[string]any{"protocol_version": "v1", "capabilities": []string{"identity-v1", "refresh-recovery-v1"}, "refresh_attempt_id": attempt, "device_continuity": map[string]string{"device_id": input.DeviceContinuity.DeviceID, "credential": strings.Repeat("D", 43)}})
	if err != nil {
		t.Fatal(err)
	}
	r = httptest.NewRecorder()
	ackRequest := httptest.NewRequest(http.MethodPost, "/v1/auth/refresh-ack", strings.NewReader(string(ackBody)))
	ackRequest.Header.Set("Authorization", "Bearer "+first.AccessToken)
	New(options).ServeHTTP(r, ackRequest)
	if r.Code != 400 || !strings.Contains(r.Body.String(), `"device_continuity_invalid"`) {
		t.Fatalf("wrong proof status=%d body=%s", r.Code, r.Body.String())
	}
	again, err := h.Auth.RefreshRecoverable(t.Context(), input)
	if err != nil || again.AccessToken != first.AccessToken || again.RefreshToken != first.RefreshToken {
		t.Fatalf("consumed child changed after quota refusal: %v", err)
	}
}

func TestRecoveryCapabilityReadiness(t *testing.T) {
	options := sessionHandlerOptions(t, nil, "identity_token_only")
	options.InstanceID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	options.RecoveryEpoch = 7
	options.CanonicalOrigin = "https://sync.example.test"
	options.RecoveryEnabled = true
	options.RecoveryReadiness = accountGateFunc(func(context.Context) error { return nil })
	options.RecoveryService = recoveryServiceStub{}
	for _, ready := range []bool{false, true} {
		options.Readiness = accountGateFunc(func(context.Context) error {
			if ready {
				return nil
			}
			return auth.ErrUnavailable
		})
		r := httptest.NewRecorder()
		New(options).ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/v1/server", nil))
		if r.Code != 200 {
			t.Fatalf("server status=%d", r.Code)
		}
		var body struct {
			Capabilities []string `json:"capabilities"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, capability := range body.Capabilities {
			if capability == "refresh-recovery-v1" {
				found = true
			}
		}
		// Advertised from configuration alone: a not-ready node keeps it (an
		// iOS client awaiting a recovery acknowledgement treats its absence
		// as terminal and signs the parent out); the route itself refuses
		// with a retryable 503 until the node is ready.
		if !found {
			t.Fatalf("ready=%v capabilities=%v", ready, body.Capabilities)
		}
		r = httptest.NewRecorder()
		New(options).ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/auth/refresh-recoverable", strings.NewReader(recoveryBody)))
		var refusal struct {
			Error struct {
				Code      string `json:"code"`
				Retryable bool   `json:"retryable"`
			} `json:"error"`
		}
		_ = json.Unmarshal(r.Body.Bytes(), &refusal)
		if !ready && (r.Code != http.StatusServiceUnavailable || refusal.Error.Code != "service_unavailable" || !refusal.Error.Retryable) {
			t.Fatalf("recovery while not ready: %d %s", r.Code, r.Body.String())
		}
		if ready && r.Code == http.StatusServiceUnavailable {
			t.Fatalf("recovery while ready: %d %s", r.Code, r.Body.String())
		}
	}
	options.RecoveryEnabled = false
	options.Readiness = accountGateFunc(func(context.Context) error { return nil })
	r := httptest.NewRecorder()
	New(options).ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/auth/refresh-recoverable", strings.NewReader(recoveryBody)))
	if r.Code != 503 {
		t.Fatalf("disabled status=%d", r.Code)
	}
}

func TestRecoveryCapabilitySuppliedListCannotBypassGate(t *testing.T) {
	for _, test := range []struct {
		name                    string
		enabled, service, ready bool
	}{
		{"disabled", false, true, true}, {"service absent", true, false, true}, {"unready", true, true, false}, {"ready", true, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := sessionHandlerOptions(t, nil, "identity_token_only")
			options.InstanceID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
			options.RecoveryEpoch = 7
			options.CanonicalOrigin = "https://sync.example.test"
			options.Capabilities = []string{"identity-v1", "refresh-recovery-v1", "refresh-recovery-v1"}
			options.RecoveryEnabled = test.enabled
			options.RecoveryReadiness = accountGateFunc(func(context.Context) error { return nil })
			if test.service {
				options.RecoveryService = recoveryServiceStub{}
			}
			options.Readiness = accountGateFunc(func(context.Context) error {
				if test.ready {
					return nil
				}
				return auth.ErrUnavailable
			})
			r := httptest.NewRecorder()
			New(options).ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/v1/server", nil))
			if r.Code != 200 {
				t.Fatalf("status=%d", r.Code)
			}
			var body struct {
				Capabilities []string `json:"capabilities"`
			}
			if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, capability := range body.Capabilities {
				if capability == "refresh-recovery-v1" {
					count++
				}
			}
			// The supplied list never adds it; configuration does, once,
			// whether or not the node is ready now.
			want := 0
			if test.enabled && test.service {
				want = 1
			}
			if count != want {
				t.Fatalf("count=%d want=%d capabilities=%v", count, want, body.Capabilities)
			}
		})
	}
}

func TestRecoveryMaintenanceReadinessDoesNotBlockLegacyRefresh(t *testing.T) {
	options := sessionHandlerOptions(t, sessionServiceStub{}, "identity_token_only")
	options.InstanceID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	options.RecoveryEpoch = 7
	options.CanonicalOrigin = "https://sync.example.test"
	options.RecoveryEnabled = true
	options.RecoveryService = recoveryServiceStub{}
	options.Readiness = accountGateFunc(func(context.Context) error { return nil })
	for _, gate := range []Readiness{nil, accountGateFunc(func(context.Context) error { return auth.ErrUnavailable })} {
		options.RecoveryReadiness = gate
		r := httptest.NewRecorder()
		New(options).ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/v1/server", nil))
		// Advertised from configuration: a recovery readiness is composed or
		// not, whatever it answers now.
		if r.Code != 200 || strings.Contains(r.Body.String(), "refresh-recovery-v1") != (gate != nil) {
			t.Fatalf("server status=%d body=%s", r.Code, r.Body.String())
		}
		r = httptest.NewRecorder()
		New(options).ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/auth/refresh-recoverable", strings.NewReader(recoveryBody)))
		if r.Code != 503 {
			t.Fatalf("recovery status=%d", r.Code)
		}
		r = httptest.NewRecorder()
		New(options).ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/auth/refresh", strings.NewReader(`{"protocol_version":"v1","capabilities":["identity-v1"],"refresh_token":"x","extra":1}`)))
		if r.Code != 400 {
			t.Fatalf("legacy status=%d body=%s", r.Code, r.Body.String())
		}
	}
}

func TestRecoveryAckHTTP(t *testing.T) {
	acknowledged := 0
	service := sessionServiceStub{authenticate: func(_ context.Context, token string) (access.Principal, error) {
		if token != "active" {
			t.Fatalf("token=%q", token)
		}
		return access.Principal{AccountID: "account", DeviceID: "device"}, nil
	}}
	options := sessionHandlerOptions(t, service, "identity_token_only")
	options.RecoveryEnabled = true
	options.RecoveryReadiness = accountGateFunc(func(context.Context) error { return nil })
	options.RecoveryService = recoveryServiceStub{ack: func(_ context.Context, p access.Principal, attempt string, claim identity.DeviceContinuityClaim) error {
		acknowledged++
		if p.AccountID != "account" || attempt != "11111111-1111-4111-8111-111111111111" || claim.DeviceID != "22222222-2222-4222-8222-222222222222" {
			t.Fatalf("ack=%+v %s %+v", p, attempt, claim)
		}
		return nil
	}}
	body := strings.Replace(recoveryBody, `"refresh_token":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",`, "", 1)
	for _, token := range []string{"", "active"} {
		r := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/auth/refresh-ack", strings.NewReader(body))
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		New(options).ServeHTTP(r, request)
		if token == "" && (r.Code != 400 || acknowledged != 0) {
			t.Fatalf("missing bearer status=%d ack=%d", r.Code, acknowledged)
		}
		if token != "" && (r.Code != 204 || r.Body.Len() != 0 || acknowledged != 1) {
			t.Fatalf("ack status=%d body=%s count=%d", r.Code, r.Body.String(), acknowledged)
		}
	}
}

func TestRecoveryAckRejectsInvalidAndExpiredBearerBeforeProof(t *testing.T) {
	for _, test := range []struct {
		name      string
		authError error
		status    int
		code      string
	}{
		{"revoked", auth.ErrSessionRevoked, 401, "session_revoked"},
		{"expired", auth.ErrAccessExpired, 401, "access_expired"},
	} {
		t.Run(test.name, func(t *testing.T) {
			proofCalls := 0
			service := sessionServiceStub{authenticate: func(context.Context, string) (access.Principal, error) { return access.Principal{}, test.authError }}
			options := sessionHandlerOptions(t, service, "identity_token_only")
			options.RecoveryEnabled = true
			options.RecoveryReadiness = accountGateFunc(func(context.Context) error { return nil })
			options.RecoveryService = recoveryServiceStub{ack: func(context.Context, access.Principal, string, identity.DeviceContinuityClaim) error {
				proofCalls++
				return nil
			}}
			body := strings.Replace(recoveryBody, `"refresh_token":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",`, "", 1)
			r := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/auth/refresh-ack", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer authority")
			New(options).ServeHTTP(r, request)
			if r.Code != test.status || !strings.Contains(r.Body.String(), `"code":"`+test.code+`"`) || proofCalls != 0 {
				t.Fatalf("status=%d calls=%d body=%s", r.Code, proofCalls, r.Body.String())
			}
		})
	}
}

func TestRecoveryHTTPSuccessEchoAndSanitizedErrors(t *testing.T) {
	options := sessionHandlerOptions(t, nil, "identity_token_only")
	options.RecoveryEnabled = true
	options.RecoveryReadiness = accountGateFunc(func(context.Context) error { return nil })
	options.InstanceID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	options.RecoveryService = recoveryServiceStub{
		resolve: func(context.Context, auth.RecoverableRefreshInput) (auth.RefreshSubject, error) {
			return auth.RefreshSubject{AccountID: "account", DeviceID: "device"}, nil
		},
		refresh: func(_ context.Context, input auth.RecoverableRefreshInput) (auth.RecoveredSessionBundle, error) {
			return auth.RecoveredSessionBundle{SessionBundle: auth.SessionBundle{AccessToken: "access-child", RefreshToken: "refresh-child", DeviceID: input.DeviceContinuity.DeviceID, RecoveryEpoch: 7, ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), ReauthenticateAt: time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC), Account: auth.AccountView{ID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc"}}, RefreshAttemptID: input.AttemptID}, nil
		},
	}
	r := httptest.NewRecorder()
	New(options).ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/auth/refresh-recoverable", strings.NewReader(recoveryBody)))
	if r.Code != 200 {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
	var response struct {
		Attempt  string `json:"refresh_attempt_id"`
		Instance string `json:"server_instance_id"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil || response.Attempt != "11111111-1111-4111-8111-111111111111" || response.Instance != options.InstanceID {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	for _, value := range []struct {
		err    error
		status int
		code   string
	}{
		{auth.ErrRefreshAttemptConflict, 409, "refresh_attempt_conflict"},
		{auth.ErrRefreshReceiptUnavailable, 401, "refresh_receipt_unavailable"},
	} {
		options.RecoveryService = recoveryServiceStub{resolve: func(context.Context, auth.RecoverableRefreshInput) (auth.RefreshSubject, error) {
			return auth.RefreshSubject{AccountID: "account", DeviceID: "device"}, nil
		}, refresh: func(context.Context, auth.RecoverableRefreshInput) (auth.RecoveredSessionBundle, error) {
			return auth.RecoveredSessionBundle{}, value.err
		}}
		r = httptest.NewRecorder()
		New(options).ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/auth/refresh-recoverable", strings.NewReader(recoveryBody)))
		if r.Code != value.status || !strings.Contains(r.Body.String(), `"code":"`+value.code+`"`) || strings.Contains(r.Body.String(), "BBBBBBBB") || strings.Contains(r.Body.String(), "CCCCCCCC") {
			t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
		}
	}
}
