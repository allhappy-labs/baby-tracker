package liveactivity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type preflightCall struct {
	route, proto, path, topic, pushType, authorization, priority string
	payload                                                      map[string]any
}

// A scripted Apple: one HTTP/2 TLS endpoint per environment, answering with the
// status and reason the case names.
type preflightApple struct {
	mu     sync.Mutex
	status int
	reason string
	calls  []preflightCall
}

func (a *preflightApple) handler(route string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			APS map[string]any `json:"aps"`
		}
		_ = json.Unmarshal(body, &payload)
		a.mu.Lock()
		a.calls = append(a.calls, preflightCall{route: route, proto: r.Proto, path: r.URL.Path, topic: r.Header.Get("apns-topic"), pushType: r.Header.Get("apns-push-type"),
			authorization: r.Header.Get("Authorization"), priority: r.Header.Get("apns-priority"), payload: payload.APS})
		status, reason := a.status, a.reason
		a.mu.Unlock()
		if status == 0 {
			// Hang until the client gives up: no answer at all.
			<-r.Context().Done()
			return
		}
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "30")
		}
		w.WriteHeader(status)
		if reason != "" {
			fmt.Fprintf(w, `{"reason":%q}`, reason)
		}
	}
}

func (a *preflightApple) answer(status int, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.status, a.reason, a.calls = status, reason, nil
}

func (a *preflightApple) recorded() []preflightCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]preflightCall(nil), a.calls...)
}

func startPreflightApple(t *testing.T, apple *preflightApple, route string) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(apple.handler(route))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

// The preflight sends the production request shape (provider JWT, Live
// Activity topic and push type, HTTP/2) once to an all-zero dummy token and
// interprets Apple's answer as a verdict on the key, team, key ID and topic.
func TestLiveActivityPreflightClassifiesAppleAnswers(t *testing.T) {
	key := testAPNSKey(t)
	apple := &preflightApple{}
	sandbox := startPreflightApple(t, apple, "sandbox")
	production := startPreflightApple(t, apple, "production")
	client := sandbox.Client()
	client.Transport = &routeTLSRoundTripper{sandbox: sandbox.Client().Transport, production: production.Client().Transport, productionHost: strings.TrimPrefix(production.URL, "https://")}
	transport := newAPNSTransport("TEAM123456", "KEY1234567", "com.example.baby", key, client, sandbox.URL, production.URL)
	for _, tc := range []struct {
		status int
		reason string
		want   PreflightOutcome
	}{
		{400, "BadDeviceToken", PreflightPass},
		{403, "InvalidProviderToken", PreflightCredentials},
		{403, "ExpiredProviderToken", PreflightCredentials},
		{403, "Forbidden", PreflightCredentials},
		{403, "", PreflightCredentials},
		{400, "TopicDisallowed", PreflightTopic},
		{400, "BadTopic", PreflightTopic},
		{400, "DeviceTokenNotForTopic", PreflightTopic},
		{429, "TooManyRequests", PreflightRetryLater},
		// Apple's one-token-update-per-20-minutes throttle: the reason must
		// survive the allowlist so the operator can tell it from a plain 429.
		{429, "TooManyProviderTokenUpdates", PreflightRetryLater},
		{429, "", PreflightRetryLater},
		{200, "", PreflightUnknown},
		{400, "MissingTopic", PreflightUnknown},
		{410, "Unregistered", PreflightUnknown},
		{500, "InternalServerError", PreflightUnknown},
		{503, "ServiceUnavailable", PreflightUnknown},
	} {
		for _, environment := range []string{"sandbox", "production"} {
			apple.answer(tc.status, tc.reason)
			result, err := transport.Preflight(context.Background(), environment)
			if err != nil {
				t.Fatalf("%d %s %s: %v", tc.status, tc.reason, environment, err)
			}
			if result.Outcome != tc.want || result.HTTPStatus != tc.status || result.Reason != tc.reason || result.Environment != environment {
				t.Fatalf("%d %s %s: %+v want %s", tc.status, tc.reason, environment, result, tc.want)
			}
			if tc.status == 429 && !result.RetryAfter.After(time.Now()) {
				t.Fatalf("429 without retry-after: %+v", result)
			}
			calls := apple.recorded()
			if len(calls) != 1 {
				t.Fatalf("%d %s %s: %d requests, want exactly one", tc.status, tc.reason, environment, len(calls))
			}
			call := calls[0]
			if call.route != environment || call.proto != "HTTP/2.0" || call.path != "/3/device/"+strings.Repeat("0", 64) ||
				call.topic != "com.example.baby.push-type.liveactivity" || call.pushType != "liveactivity" || !strings.HasPrefix(call.authorization, "bearer ") || call.priority != "5" {
				t.Fatalf("%s request: %+v", environment, call)
			}
			if call.payload["event"] != "update" || call.payload["alert"] != nil || call.payload["attributes"] != nil {
				t.Fatalf("the preflight must be a quiet update, never a start: %+v", call.payload)
			}
		}
	}
	// A hanging Apple is a connectivity failure, not a verdict.
	apple.answer(0, "")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result, err := transport.Preflight(ctx, "sandbox")
	if err != nil || result.Outcome != PreflightConnectivity || result.HTTPStatus != 0 {
		t.Fatalf("hung apple: %+v %v", result, err)
	}
	// So is a closed endpoint (TLS or TCP failure).
	sandbox.Close()
	result, err = transport.Preflight(context.Background(), "sandbox")
	if err != nil || result.Outcome != PreflightConnectivity {
		t.Fatalf("closed apple: %+v %v", result, err)
	}
	if _, err := transport.Preflight(context.Background(), "staging"); err == nil {
		t.Fatal("unknown environment accepted")
	}
}

// A key the provider cannot sign with is reported before any request is
// written, without a secret in the error.
func TestLiveActivityPreflightReportsUnsignableKeyWithoutSending(t *testing.T) {
	apple := &preflightApple{}
	apple.answer(400, "BadDeviceToken")
	server := startPreflightApple(t, apple, "sandbox")
	transport := newAPNSTransport("TEAM123456", "KEY1234567", "com.example.baby", nil, server.Client(), server.URL, server.URL)
	result, err := transport.Preflight(context.Background(), "sandbox")
	if err == nil || result.Outcome != PreflightUnknown || len(apple.recorded()) != 0 {
		t.Fatalf("nil key: %+v %v calls=%d", result, err, len(apple.recorded()))
	}
	if strings.Contains(err.Error(), "TEAM123456") || strings.Contains(strings.ToLower(err.Error()), "bearer") {
		t.Fatalf("error carries identifiers or token material: %v", err)
	}
}
