package loadtest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEveryRequestCarriesTheDevicesForwardedAddressAndHost(t *testing.T) {
	seen := newFakeV1(t)
	c := newClient(seen.URL, "sync.load.test")
	d := &device{DeviceState: DeviceState{ClientIP: "100.64.0.7", AccessToken: "a"}}
	c.pull(t.Context(), d, nil)
	r := seen.last()
	if r.Header.Get("X-Forwarded-For") != "100.64.0.7" || r.Host != "sync.load.test" ||
		r.Header.Get("X-Baby-Tracker-Protocol") != "v1" || r.Header.Get("Authorization") != "Bearer a" ||
		r.Header.Get("X-Baby-Tracker-Capabilities") != "records-v1" {
		t.Fatalf("headers %v host %q", r.Header, r.Host)
	}
}

// Each route names exactly the capabilities protocol.RequiredCapabilities
// gives its surface, in the header and the body alike.
func TestEachRouteSendsItsCapabilities(t *testing.T) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 2)
	d := g.writers[0]
	g.opPushToken(t.Context(), time.Now())
	g.opLiveActivity(t.Context(), time.Now())
	g.snapshotAndPull(t.Context(), d, time.Now(), RouteSyncPull)
	g.refresh(t.Context(), d, time.Now())
	want := map[string]string{
		"/v1/devices/push-token":       "push-token-v1",
		"/v1/live-activities/device":   "live-activity-push-v1",
		"/v1/sync/snapshot":            "records-v1,snapshot-v1",
		"/v1/auth/refresh-recoverable": "identity-v1,refresh-recovery-v1",
		"/v1/auth/refresh-ack":         "identity-v1,refresh-recovery-v1",
		"/v1/sync":                     "records-v1",
	}
	for path, capabilities := range want {
		requests := f.all(path)
		if len(requests) == 0 {
			t.Fatalf("no request to %s", path)
		}
		for _, r := range requests {
			if got := r.Header.Get("X-Baby-Tracker-Capabilities"); got != capabilities {
				t.Errorf("%s capabilities %q, want %q", path, got, capabilities)
			}
		}
	}
	if ack := f.all("/v1/auth/refresh-ack")[0]; ack.Header.Get("Authorization") != "Bearer access-child-1" {
		t.Errorf("the ack is not sent with the child's access token: %q", ack.Header.Get("Authorization"))
	}
}

func TestOutcomesAreAFixedSetOfClasses(t *testing.T) {
	cases := []struct {
		status int
		code   string
		want   string
	}{
		{503, "server_busy", OutcomeServerBusy},
		{503, "not_ready", OutcomeNotReady},
		{503, "security_busy", OutcomeSecurityBusy},
		{503, "credential_rotation_busy", OutcomeCredentialRotationBusy},
		{503, "timeout", OutcomeServerTimeout}, // MCP's own deadline refusal
		{503, "service_unavailable", OutcomeHTTP5xx},
		{500, "internal_error", OutcomeHTTP5xx},
		{429, "rate_limited", OutcomeRateLimited},
		{401, "access_expired", OutcomeAccessExpired},
		{401, "session_revoked", "http_4xx_session_revoked"},
		{409, "refresh_attempt_conflict", "http_4xx_refresh_attempt_conflict"},
		{400, "something_new_" + "x", "http_4xx_other"},
		{404, "", "http_4xx_other"},
	}
	for _, tc := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if tc.code == "" {
				w.WriteHeader(tc.status)
				return
			}
			w.Header().Set("Retry-After", "1")
			writeErrorResponse(w, tc.status, tc.code)
		}))
		res := newClient(server.URL, "sync.load.test").do(t.Context(), request{method: http.MethodGet, path: "/"}, nil)
		server.Close()
		if res.Outcome != tc.want {
			t.Errorf("%d %q: outcome %q, want %q", tc.status, tc.code, res.Outcome, tc.want)
		}
		if tc.code != "" && res.RetryAfter != time.Second {
			t.Errorf("%d %q: Retry-After %v", tc.status, tc.code, res.RetryAfter)
		}
	}

	// A lost answer is transport; the request deadline is timeout; an
	// answer that does not decode is protocol.
	lost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	}))
	defer lost.Close()
	if res := newClient(lost.URL, "h").do(t.Context(), request{method: http.MethodGet, path: "/"}, nil); res.Outcome != OutcomeTransport {
		t.Errorf("lost answer: %q", res.Outcome)
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer slow.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if res := newClient(slow.URL, "h").do(ctx, request{method: http.MethodGet, path: "/"}, nil); res.Outcome != OutcomeTimeout {
		t.Errorf("deadline: %q", res.Outcome)
	}
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{")) }))
	defer garbage.Close()
	var out map[string]any
	if res := newClient(garbage.URL, "h").do(t.Context(), request{method: http.MethodGet, path: "/"}, &out); res.Outcome != OutcomeProtocol {
		t.Errorf("garbage: %q", res.Outcome)
	}
}

func TestTransportIsHTTP1WithALargeIdlePool(t *testing.T) {
	tr := newClient("http://x", "h").http.Transport.(*http.Transport)
	if tr.ForceAttemptHTTP2 || tr.MaxIdleConns != 4096 || tr.MaxIdleConnsPerHost != 4096 || tr.IdleConnTimeout != 90*time.Second {
		t.Fatalf("transport %+v", tr)
	}
	if c := newClient("http://x", "h"); c.http.Timeout != 10*time.Second {
		t.Fatalf("timeout %v", c.http.Timeout)
	}
}
