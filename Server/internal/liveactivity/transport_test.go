package liveactivity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/apns"
	"babytracker/server/internal/config"
)

// testBundleID is the throwaway bundle every test APNs client signs for.
const testBundleID = "com.example.baby"

// testAPNSCredentials is a throwaway P-256 key in a temp dir: no Apple
// credential is ever read.
func testAPNSCredentials(t *testing.T) config.APNSCredentials {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "apns.p8")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.APNSCredentials{TeamID: "TEAM123456", KeyID: "KEY1234567", KeyFile: path, BundleID: testBundleID}
}

// testAPNSServer is a local TLS HTTP/2 server standing in for Apple.
func testAPNSServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

// testAPNSTransport is the production APNSTransport over the branch's one
// APNs client, pointed at server for both environments.
func testAPNSTransport(t *testing.T, server *httptest.Server, now func() time.Time) APNSTransport {
	t.Helper()
	client, err := apns.NewWithTransport(testAPNSCredentials(t), server.Client().Transport, server.URL, server.URL, now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return APNSTransport{Client: client}
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

type seenPush struct {
	path    string
	headers http.Header
	body    []byte
}

type pushLog struct {
	mu   sync.Mutex
	seen []seenPush
}

func (l *pushLog) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	l.mu.Lock()
	l.seen = append(l.seen, seenPush{path: r.URL.Path, headers: r.Header.Clone(), body: body})
	l.mu.Unlock()
}

func (l *pushLog) all() []seenPush {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]seenPush(nil), l.seen...)
}

// A start goes out as main's push-to-start: liveactivity push type, the
// bundle's Live Activity topic, priority 10, expiration 0, the claim's
// apns-id, and main's payload, which the phone decodes with ContentState.
func TestTransportSendsAStartAsMainsPushToStart(t *testing.T) {
	log := &pushLog{}
	server := testAPNSServer(t, func(w http.ResponseWriter, r *http.Request) { log.record(r); w.WriteHeader(http.StatusOK) })
	transport := testAPNSTransport(t, server, nil)
	request := testPushRequest("start", "sandbox")
	result, err := transport.Send(t.Context(), request)
	if err != nil || result.Classification != Accepted || result.HTTPStatus != 200 {
		t.Fatalf("start: %+v %v", result, err)
	}
	seen := log.all()
	if len(seen) != 1 {
		t.Fatalf("requests %d", len(seen))
	}
	h := seen[0].headers
	if seen[0].path != "/3/device/"+request.Token || h.Get("apns-push-type") != "liveactivity" || h.Get("apns-topic") != testBundleID+".push-type.liveactivity" ||
		h.Get("apns-priority") != "10" || h.Get("apns-expiration") != "0" || h.Get("apns-id") != request.APNSID || !strings.HasPrefix(h.Get("authorization"), "bearer ") {
		t.Fatalf("start headers %v path %s", h, seen[0].path)
	}
	want, err := encodePushPayload(request)
	if err != nil || string(seen[0].body) != string(want) {
		t.Fatalf("payload %s, want main's %s (%v)", seen[0].body, want, err)
	}
	var payload struct {
		APS struct {
			Event          string             `json:"event"`
			Timestamp      int64              `json:"timestamp"`
			ContentState   ContentState       `json:"content-state"`
			AttributesType string             `json:"attributes-type"`
			Attributes     ActivityAttributes `json:"attributes"`
			InputPushToken int                `json:"input-push-token"`
		} `json:"aps"`
	}
	if err := json.Unmarshal(seen[0].body, &payload); err != nil {
		t.Fatal(err)
	}
	aps := payload.APS
	if aps.Event != "start" || aps.Timestamp != request.Timestamp.Unix() || aps.AttributesType != "TimerActivityAttributes" || aps.InputPushToken != 1 ||
		aps.Attributes != request.Attributes || aps.ContentState.Remote == nil || *aps.ContentState.Remote != *request.Content.Remote || aps.ContentState.ActivityKind != "breastLeft" {
		t.Fatalf("decoded start %+v", aps)
	}
}

// An update goes out at the priority the reconciler decided (5 for a
// cosmetic change, 10 otherwise) with its expiration, rounded up to the
// second; an end always at 10.
func TestTransportSendsUpdatesAtTheirPriorityWithAnExpiration(t *testing.T) {
	log := &pushLog{}
	server := testAPNSServer(t, func(w http.ResponseWriter, r *http.Request) { log.record(r); w.WriteHeader(http.StatusOK) })
	transport := testAPNSTransport(t, server, nil)
	for _, tc := range []struct {
		event    string
		priority int
	}{{"update", 5}, {"update", 10}, {"end", 10}} {
		request := testPushRequest(tc.event, "production")
		request.Priority = tc.priority
		request.Expiration = time.Unix(1_790_000_060, 500)
		if result, err := transport.Send(t.Context(), request); err != nil || result.Classification != Accepted {
			t.Fatalf("%s: %+v %v", tc.event, result, err)
		}
	}
	seen := log.all()
	for i, want := range []string{"5", "10", "10"} {
		if seen[i].headers.Get("apns-priority") != want || seen[i].headers.Get("apns-expiration") != "1790000061" {
			t.Fatalf("push %d: priority %s expiration %s", i, seen[i].headers.Get("apns-priority"), seen[i].headers.Get("apns-expiration"))
		}
	}
	if !strings.Contains(string(seen[2].body), `"dismissal-date":1789999999`) {
		t.Fatalf("end payload %s", seen[2].body)
	}
}

// A request validation or encoding refuses is known-unsent: no request
// reaches the server. Apple's own refusal, however it answered, is not.
func TestTransportRefusesAnInvalidRequestBeforeSending(t *testing.T) {
	var requests atomic.Int32
	server := testAPNSServer(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	transport := testAPNSTransport(t, server, nil)
	invalid := []PushRequest{testPushRequest("start", "bogus"), testPushRequest("start", "sandbox"), testPushRequest("update", "sandbox"), testPushRequest("start", "sandbox")}
	invalid[1].Attributes.BabyName = strings.Repeat("n", 4096)
	invalid[2].Priority = 7
	invalid[3].Token = "abc"
	for i, request := range invalid {
		result, err := transport.Send(t.Context(), request)
		if result.Classification != ConfigurationFailure || !errors.Is(err, ErrRequestNotSent) {
			t.Fatalf("invalid %d: %+v %v", i, result, err)
		}
	}
	if result, err := (APNSTransport{}).Send(t.Context(), testPushRequest("start", "sandbox")); !errors.Is(err, ErrRequestNotSent) || result.Classification != ConfigurationFailure {
		t.Fatalf("no client: %+v %v", result, err)
	}
	if requests.Load() != 0 {
		t.Fatalf("%d invalid requests reached the server", requests.Load())
	}
	// The client's own not-sent answer (no connection was made: nothing was
	// written) is known-unsent too.
	closed := testAPNSServer(t, func(http.ResponseWriter, *http.Request) {})
	unreachable := testAPNSTransport(t, closed, nil)
	closed.Close()
	if result, err := unreachable.Send(t.Context(), testPushRequest("start", "sandbox")); !errors.Is(err, ErrRequestNotSent) || !errors.Is(err, apns.ErrNotSent) || result.Classification != ConfigurationFailure {
		t.Fatalf("unreachable Apple: %+v %v", result, err)
	}
}

// Apple's answers keep main's classes: 410 Unregistered is a dead token;
// 403 InvalidProviderToken is the deployment's configuration (and the
// client re-signs no sooner than its 20-minute floor); 429 carries its
// Retry-After.
func TestTransportClassifiesApplesAnswers(t *testing.T) {
	var answer atomic.Value
	log := &pushLog{}
	server := testAPNSServer(t, func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		a := answer.Load().([3]string)
		if a[2] != "" {
			w.Header().Set("Retry-After", a[2])
		}
		var status int
		fmt.Sscan(a[0], &status)
		w.WriteHeader(status)
		if a[1] != "" {
			fmt.Fprintf(w, `{"reason":%q}`, a[1])
		}
	})
	start := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(start.UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	transport := testAPNSTransport(t, server, now)
	send := func(at time.Duration, status, reason, retryAfter string) (PushResult, string) {
		t.Helper()
		clock.Store(start.Add(at).UnixNano())
		answer.Store([3]string{status, reason, retryAfter})
		result, err := transport.Send(t.Context(), testPushRequest("update", "sandbox"))
		if err != nil {
			t.Fatalf("an answered push carries no error: %v", err)
		}
		seen := log.all()
		return result, seen[len(seen)-1].headers.Get("authorization")
	}
	if result, _ := send(0, "410", "Unregistered", ""); result.Classification != InvalidToken || result.HTTPStatus != 410 || result.Reason != "Unregistered" {
		t.Fatalf("410: %+v", result)
	}
	if result, _ := send(0, "400", "BadDeviceToken", ""); result.Classification != InvalidToken {
		t.Fatalf("BadDeviceToken: %+v", result)
	}
	if result, _ := send(0, "400", "DeviceTokenNotForTopic", ""); result.Classification != ConfigurationFailure {
		t.Fatalf("DeviceTokenNotForTopic: %+v", result)
	}
	_, signed := send(0, "200", "", "")
	result, auth := send(time.Minute, "403", "InvalidProviderToken", "")
	if result.Classification != ConfigurationFailure || result.HTTPStatus != 403 || result.Reason != "InvalidProviderToken" || auth != signed {
		t.Fatalf("403: %+v re-signed=%v", result, auth != signed)
	}
	if _, auth := send(10*time.Minute, "200", "", ""); auth != signed {
		t.Fatal("re-signed after a refusal inside the 20-minute floor")
	}
	if _, auth := send(21*time.Minute, "403", "InvalidProviderToken", ""); auth != signed {
		t.Fatal("the refused request did not carry the cached token")
	}
	if _, auth := send(22*time.Minute, "200", "", ""); auth == signed {
		t.Fatal("not re-signed after Apple refused a provider token older than the floor")
	}
	result, _ = send(22*time.Minute, "429", "TooManyRequests", "30")
	if result.Classification != Retryable || result.Reason != "TooManyRequests" || !result.RetryAfter.Equal(start.Add(22*time.Minute+30*time.Second)) {
		t.Fatalf("429: %+v", result)
	}
	if result, _ := send(22*time.Minute, "503", "ServiceUnavailable", ""); result.Classification != Retryable || !result.RetryAfter.IsZero() {
		t.Fatalf("503: %+v", result)
	}
	if result, _ := send(22*time.Minute, "400", "NotAnAppleReason", ""); result.Classification != ConfigurationFailure || result.Reason != "" {
		t.Fatalf("an undocumented reason is dropped: %+v", result)
	}
}
