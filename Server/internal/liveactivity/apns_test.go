package liveactivity

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testAPNSKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func testPushRequest(event, environment string) PushRequest {
	scope := ActivityScope{ServerInstanceID: "123e4567-e89b-12d3-a456-426614174000", RecoveryEpoch: 1, FamilyID: "123e4567-e89b-12d3-a456-426614174000", BabyID: "123e4567-e89b-12d3-a456-426614174000", KindRaw: "feed"}
	kind := UpdateToken
	if event == "start" {
		kind = PushToStartToken
	}
	return PushRequest{
		Environment: environment, Token: strings.Repeat("a", 64), APNSID: "123e4567-e89b-12d3-a456-426614174000",
		TokenKind: kind,
		Event:     event, Timestamp: time.Unix(1_790_000_000, 0), Expiration: time.Unix(1_790_000_060, 0),
		Priority: 10, Attributes: ActivityAttributes{BabyID: "123e4567-e89b-12d3-a456-426614174000", BabyName: "Mira", KindRaw: "feed", Scope: scope},
		Content: ContentState{StartedAt: 800_000_000, SideLabel: "L", ActivityKind: "breastLeft", Phase: "running", Parts: []PartLabel{}, Remote: &Revision{Scope: scope, GenerationID: "123e4567-e89b-12d3-a456-426614174001", PresentationVersion: 1, PresentationTimestamp: 1}},
	}
}

func TestLiveActivityProviderJWT(t *testing.T) {
	key := testAPNSKey(t)
	clock := time.Unix(1_790_000_000, 0)
	provider := newProviderToken("TEAM123456", "KEY1234567", key, func() time.Time { return clock })
	first, err := provider.token()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(first, ".")
	if len(parts) != 3 {
		t.Fatal("invalid JWT compact form")
	}
	var header map[string]any
	var claims map[string]any
	for i, dest := range []any{&header, &claims} {
		decoded, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(decoded, dest); err != nil {
			t.Fatal(err)
		}
	}
	if header["alg"] != "ES256" || header["kid"] != "KEY1234567" || claims["iss"] != "TEAM123456" || claims["iat"] != float64(clock.Unix()) {
		t.Fatal("JWT headers/claims")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if len(sig) != 64 || !ecdsa.Verify(&key.PublicKey, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("invalid ES256 signature")
	}
	clock = clock.Add(39 * time.Minute)
	cached, _ := provider.token()
	if cached != first {
		t.Fatal("provider token refreshed too early")
	}
	clock = clock.Add(time.Minute)
	refreshed, _ := provider.token()
	if refreshed == first {
		t.Fatal("provider token did not refresh at 40 minutes")
	}
	der, _ := x509.MarshalECPrivateKey(key)
	if len(der) == 0 {
		t.Fatal("key fixture")
	}
}

func TestLiveActivityAPNsHTTP2(t *testing.T) {
	key := testAPNSKey(t)
	var mu sync.Mutex
	seen := []string{}
	handler := func(route string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			seen = append(seen, route+" "+r.Proto+" "+r.URL.Path)
			mu.Unlock()
			if r.ProtoMajor != 2 || r.Method != http.MethodPost || r.Header.Get("apns-topic") != "com.example.baby.push-type.liveactivity" || r.Header.Get("apns-push-type") != "liveactivity" || r.Header.Get("apns-id") == "" || !strings.HasPrefix(r.Header.Get("Authorization"), "bearer ") {
				t.Error("APNs HTTP/2 headers")
			}
			if len(body) > 4096 {
				t.Error("oversized payload sent")
			}
			var payload struct {
				APS map[string]any `json:"aps"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Error(err)
			}
			event, _ := payload.APS["event"].(string)
			if event == "start" {
				alert, _ := payload.APS["alert"].(map[string]any)
				body, _ := alert["body"].(map[string]any)
				title, _ := alert["title"].(map[string]any)
				if title["loc-key"] != "LIVE_ACTIVITY_FEED_START_TITLE" || body["loc-key"] != "LIVE_ACTIVITY_START_BODY" || alert["sound"] != nil || payload.APS["attributes-type"] != "TimerActivityAttributes" || payload.APS["input-push-token"] != float64(1) || r.Header.Get("apns-expiration") != "0" {
					t.Error("start contract")
				}
			} else if event == "update" {
				if payload.APS["alert"] != nil || r.Header.Get("apns-priority") != "5" || (route == "sandbox" && r.Header.Get("apns-expiration") != "1790000061") {
					t.Error("quiet cosmetic update")
				}
			} else if event == "end" && payload.APS["dismissal-date"].(float64) >= payload.APS["timestamp"].(float64) {
				t.Error("end dismissal")
			}
			if event == "end" {
				w.WriteHeader(http.StatusGone)
				fmt.Fprint(w, `{"reason":"Unregistered"}`)
				return
			}
			w.WriteHeader(http.StatusOK)
		}
	}
	server := httptest.NewUnstartedServer(handler("sandbox"))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	production := httptest.NewUnstartedServer(handler("production"))
	production.EnableHTTP2 = true
	production.StartTLS()
	defer production.Close()
	// One injection seam chooses either distinct endpoint, with no dynamic
	// destination field in PushRequest.
	client := server.Client()
	client.Transport = &routeTLSRoundTripper{sandbox: server.Client().Transport, production: production.Client().Transport, productionHost: strings.TrimPrefix(production.URL, "https://")}
	transport := newAPNSTransport("TEAM123456", "KEY1234567", "com.example.baby", key, client, server.URL, production.URL)
	for _, event := range []string{"start", "update", "end"} {
		req := testPushRequest(event, "sandbox")
		if event == "start" {
			req.Expiration = time.Time{}
		}
		if event != "start" {
			req.Token = strings.Repeat("c", 64)
		}
		if event == "update" {
			req.Priority = 5
			req.Expiration = time.Unix(1_790_000_060, 1)
		}
		result, err := transport.Send(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		want := Accepted
		if event == "end" {
			want = InvalidToken
		}
		if result.Classification != want {
			t.Fatalf("%s: %+v", event, result)
		}
	}
	productionRequest := testPushRequest("update", "production")
	productionRequest.Token = strings.Repeat("d", 64)
	productionRequest.Priority = 5
	if result, err := transport.Send(context.Background(), productionRequest); err != nil || result.Classification != Accepted {
		t.Fatalf("production: %+v %v", result, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 4 || !strings.HasPrefix(seen[0], "sandbox HTTP/2") || !strings.HasPrefix(seen[3], "production HTTP/2") || !strings.HasSuffix(seen[0], strings.Repeat("a", 64)) || !strings.HasSuffix(seen[1], strings.Repeat("c", 64)) || !strings.HasSuffix(seen[3], strings.Repeat("d", 64)) {
		t.Fatalf("requests: %v", seen)
	}
}

type routeTLSRoundTripper struct {
	sandbox, production http.RoundTripper
	productionHost      string
}

func (r *routeTLSRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == r.productionHost {
		return r.production.RoundTrip(req)
	}
	return r.sandbox.RoundTrip(req)
}

func TestLiveActivityAPNsClassificationAndTimeout(t *testing.T) {
	key := testAPNSKey(t)
	var status atomic.Int64
	status.Store(429)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status.Load() == -1 {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(int(status.Load()))
		fmt.Fprint(w, `{"reason":"TooManyRequests"}`)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	transport := newAPNSTransport("TEAM123456", "KEY1234567", "com.example.baby", key, server.Client(), server.URL, server.URL)
	req := testPushRequest("update", "sandbox")
	result, err := transport.Send(context.Background(), req)
	if err != nil || result.Classification != Retryable || result.RetryAfter.Before(time.Now()) || result.Reason != "TooManyRequests" {
		t.Fatalf("429 result %+v %v", result, err)
	}
	status.Store(503)
	result, err = transport.Send(context.Background(), req)
	if err != nil || result.Classification != Retryable {
		t.Fatalf("503 result %+v %v", result, err)
	}
	status.Store(-1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	result, err = transport.Send(ctx, req)
	if err == nil || result.Classification != Ambiguous || time.Since(started) > time.Second {
		t.Fatalf("timeout result %+v %v", result, err)
	}
}

// A token for another topic (a wrong bundle ID) or a topic this key may not
// use is the operator's configuration, not a dead device: every phone would
// otherwise lose its tokens to one misconfigured deployment.
func TestLiveActivityAPNsTopicFailuresAreConfiguration(t *testing.T) {
	key := testAPNSKey(t)
	var mu sync.Mutex
	status, reason := 0, ""
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		s, r := status, reason
		mu.Unlock()
		w.WriteHeader(s)
		fmt.Fprintf(w, `{"reason":%q}`, r)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	transport := newAPNSTransport("TEAM123456", "KEY1234567", "com.example.baby", key, server.Client(), server.URL, server.URL)
	for _, tc := range []struct {
		status int
		reason string
		want   PushClassification
	}{
		{400, "DeviceTokenNotForTopic", ConfigurationFailure},
		{400, "TopicDisallowed", ConfigurationFailure},
		{400, "BadTopic", ConfigurationFailure},
		{403, "InvalidProviderToken", ConfigurationFailure},
		{400, "BadDeviceToken", InvalidToken},
		{410, "Unregistered", InvalidToken},
	} {
		mu.Lock()
		status, reason = tc.status, tc.reason
		mu.Unlock()
		for _, event := range []string{"start", "update"} {
			result, err := transport.Send(context.Background(), testPushRequest(event, "sandbox"))
			if err != nil || result.Classification != tc.want || result.HTTPStatus != tc.status || result.Reason != tc.reason {
				t.Fatalf("%s %d %s: result=%+v err=%v want %s", event, tc.status, tc.reason, result, err, tc.want)
			}
		}
	}
}

func TestLiveActivityAPNsPayloadSizeAndStaleCeiling(t *testing.T) {
	req := testPushRequest("start", "sandbox")
	req.Attributes.BabyName = strings.Repeat("<", MaximumDisplayNameBytes)
	req.Content.Parts = make([]PartLabel, MaximumSummaryParts)
	for i := range req.Content.Parts {
		req.Content.Parts[i] = PartLabel{Side: ptr("left"), Seconds: 9007199254740991}
	}
	stale := time.Unix(1_790_000_001, 1)
	req.StaleAt = &stale
	encoded, err := encodePushPayload(req)
	if err != nil || len(encoded) > maximumAPNsPayloadBytes {
		t.Fatalf("start envelope bytes=%d err=%v", len(encoded), err)
	}
	var payload struct {
		APS map[string]any `json:"aps"`
	}
	if err = json.Unmarshal(encoded, &payload); err != nil || payload.APS["stale-date"] != float64(stale.Unix()+1) {
		t.Fatal("stale date rounded early", err)
	}
	if payload.APS["alert"] == nil || payload.APS["content-state"] == nil {
		t.Fatal("required start fields absent from size gate")
	}
	if _, ok := payload.APS["content-state"].(map[string]any)["remote"]; !ok {
		t.Fatal("worker revision absent from size gate")
	}
	req.Content.SideLabel = strings.Repeat("x", 4096)
	if _, err := encodePushPayload(req); err == nil {
		t.Fatal("oversized final payload accepted")
	}
}

func TestLiveActivityAPNsRejectsMismatchedStartIdentity(t *testing.T) {
	req := testPushRequest("start", "sandbox")
	req.Attributes.Scope.KindRaw = "sleep"
	if err := validatePushRequest(req); err == nil {
		t.Fatal("start attributes diverged from worker revision")
	}
}

func TestLiveActivityRedactedErrors(t *testing.T) {
	key := testAPNSKey(t)
	secret := strings.Repeat("b", 64)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprintf(w, `{"reason":%q}`, secret)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	transport := newAPNSTransport("TEAM123456", "KEY1234567", "com.example.baby", key, server.Client(), server.URL, server.URL)
	req := testPushRequest("start", "production")
	req.Token = secret
	result, err := transport.Send(context.Background(), req)
	if err != nil && strings.Contains(err.Error(), secret) {
		t.Fatal("secret in error")
	}
	if result.Reason == secret || result.Classification != ConfigurationFailure {
		t.Fatalf("unallowlisted reason leaked: %+v", result)
	}
	req.Content.SideLabel = strings.Repeat("x", 4096)
	_, err = transport.Send(context.Background(), req)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("oversize should fail without token leak")
	}
}
