//go:build identitytest

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/apns"
	"babytracker/server/internal/config"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/realtime"
	"babytracker/server/internal/testpg"
	"babytracker/server/internal/wake"
	"github.com/jackc/pgx/v5/pgxpool"
)

// identityTestAPNSConfig is a throwaway key and made-up identifiers: nothing
// here is an Apple credential.
func identityTestAPNSConfig(t *testing.T) config.APNSCredentials {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	path := filepath.Join(t.TempDir(), "apns.p8")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.APNSCredentials{TeamID: "E2ETEAM001", KeyID: "E2EKEY0001", KeyFile: path, BundleID: "com.example.babytracker.protocol"}
}

type recordedAPNS struct {
	Requests []struct {
		Sequence            int               `json:"sequence"`
		Method              string            `json:"method"`
		Host                string            `json:"host"`
		Path                string            `json:"path"`
		Token               string            `json:"token"`
		Headers             map[string]string `json:"headers"`
		ProviderTokenValid  bool              `json:"provider_token_valid"`
		ProviderTokenKeyID  string            `json:"provider_token_key_id"`
		ProviderTokenIssuer string            `json:"provider_token_issuer"`
		Payload             json.RawMessage   `json:"payload"`
		Acknowledged        bool              `json:"acknowledged"`
		ReceivedAt          string            `json:"received_at"`
	} `json:"requests"`
	Total        int `json:"total"`
	InFlightPeak int `json:"in_flight_peak"`
}

func readIdentityTestAPNS(t *testing.T, handler http.Handler) recordedAPNS {
	t.Helper()
	return readIdentityTestAPNSPath(t, handler, "/__identitytest/apns")
}

func readIdentityTestAPNSPath(t *testing.T, handler http.Handler, path string) recordedAPNS {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("GET /__identitytest/apns: %d %s", response.Code, response.Body)
	}
	var record recordedAPNS
	if err := json.Unmarshal(response.Body.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestIdentityTestAPNSRecorderKeepsTheProductionClient(t *testing.T) {
	t.Setenv(identityTestAPNSSinkEnv, "1")
	client, err := newAPNSClient(identityTestAPNSConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	handler := wrapIdentityTestHandler(http.NotFoundHandler(), "com.example.protocol")
	token := bytes.Repeat([]byte{0x5a}, 32)
	for _, env := range []apns.Environment{apns.Sandbox, apns.Production} {
		if result, err := client.Wake(t.Context(), env, token); err != nil || result.Class != apns.Accepted {
			t.Fatalf("%s: %+v %v", env, result, err)
		}
	}
	record := readIdentityTestAPNS(t, handler)
	if len(record.Requests) != 2 {
		t.Fatalf("%d recorded requests, want 2", len(record.Requests))
	}
	for index, host := range []string{"sandbox.apns.identitytest.invalid", "production.apns.identitytest.invalid"} {
		got := record.Requests[index]
		if got.Sequence != index+1 || got.Method != http.MethodPost || got.Host != host || got.Path != "/3/device/"+hex.EncodeToString(token) ||
			got.Token != hex.EncodeToString(token) || !got.Acknowledged {
			t.Fatalf("request %d: %+v", index, got)
		}
		if got.Headers["apns-push-type"] != "background" || got.Headers["apns-priority"] != "5" ||
			got.Headers["apns-topic"] != "com.example.babytracker.protocol" || got.Headers["content-type"] != "application/json" ||
			got.Headers["apns-id"] == "" || got.Headers["apns-expiration"] == "" {
			t.Fatalf("request %d headers %v", index, got.Headers)
		}
		if _, recorded := got.Headers["authorization"]; recorded {
			t.Fatal("the recorder keeps the provider token")
		}
		if !got.ProviderTokenValid || got.ProviderTokenKeyID != "E2EKEY0001" || got.ProviderTokenIssuer != "E2ETEAM001" {
			t.Fatalf("request %d provider token %v %q %q", index, got.ProviderTokenValid, got.ProviderTokenKeyID, got.ProviderTokenIssuer)
		}
		if string(got.Payload) != apns.WakePayload {
			t.Fatalf("request %d payload %s", index, got.Payload)
		}
	}

	// The harness can lose Apple's answer to one token: the request is
	// recorded, the client reports it as ambiguous.
	lose := httptest.NewRecorder()
	handler.ServeHTTP(lose, httptest.NewRequest(http.MethodPost, "/__identitytest/apns/lose-ack", strings.NewReader(`{"token":"`+hex.EncodeToString(token)+`"}`)))
	if lose.Code != http.StatusNoContent {
		t.Fatalf("lose-ack: %d", lose.Code)
	}
	// The recorder had the request (it reports the write): Ambiguous, never
	// "not sent".
	if result, err := client.Wake(t.Context(), apns.Sandbox, token); !errors.Is(err, apns.ErrOutcomeUnknown) || errors.Is(err, apns.ErrNotSent) || result.Class != apns.Ambiguous {
		t.Fatalf("a lost acknowledgment: %+v %v", result, err)
	}
	record = readIdentityTestAPNS(t, handler)
	if len(record.Requests) != 3 || record.Requests[2].Acknowledged {
		t.Fatalf("the lost answer was not recorded: %+v", record.Requests)
	}

	// A provider token signed by another key is recorded as invalid.
	other := identityTestAPNSConfig(t)
	other.KeyID = "E2EKEY0002"
	stranger, err := apns.NewWithTransport(other, identityTestAPNS.recorder, "https://sandbox.apns.identitytest.invalid", "https://production.apns.identitytest.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stranger.Wake(t.Context(), apns.Sandbox, token); err != nil {
		t.Fatal(err)
	}
	record = readIdentityTestAPNS(t, handler)
	if last := record.Requests[len(record.Requests)-1]; last.ProviderTokenValid || last.ProviderTokenKeyID != "E2EKEY0002" {
		t.Fatalf("a foreign provider token verified: %+v", last)
	}
}

// A Live Activity push goes through the same client and recorder, which
// keeps its push type, its derived topic and its payload.
func TestIdentityTestAPNSRecorderRecordsLiveActivityPushes(t *testing.T) {
	t.Setenv(identityTestAPNSSinkEnv, "1")
	client, err := newAPNSClient(identityTestAPNSConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	handler := wrapIdentityTestHandler(http.NotFoundHandler(), "com.example.protocol")
	token := bytes.Repeat([]byte{0x6b}, 100)
	payload := []byte(`{"aps":{"event":"update","timestamp":1}}`)
	if result, err := client.Send(t.Context(), apns.Request{Environment: apns.Production, Token: token, PushType: apns.PushLiveActivity, Priority: 10,
		Expiration: 0, APNsID: "6f9619ff-8b86-4011-b42d-00c04fc964ff", Payload: payload}); err != nil || result.Class != apns.Accepted {
		t.Fatalf("%+v %v", result, err)
	}
	record := readIdentityTestAPNS(t, handler)
	got := record.Requests[len(record.Requests)-1]
	if got.Token != hex.EncodeToString(token) || got.Host != "production.apns.identitytest.invalid" || string(got.Payload) != string(payload) {
		t.Fatalf("recorded %+v", got)
	}
	for name, want := range map[string]string{"apns-push-type": "liveactivity", "apns-topic": "com.example.babytracker.protocol.push-type.liveactivity",
		"apns-priority": "10", "apns-expiration": "0", "apns-id": "6f9619ff-8b86-4011-b42d-00c04fc964ff"} {
		if got.Headers[name] != want {
			t.Fatalf("%s = %q, want %q", name, got.Headers[name], want)
		}
	}
	if !got.ProviderTokenValid || got.ProviderTokenKeyID != "E2EKEY0001" {
		t.Fatalf("provider token %v %q", got.ProviderTokenValid, got.ProviderTokenKeyID)
	}
}

// An identitytest build pushes only to the recorder: with wakes or Live
// Activities enabled but without BT_IDENTITYTEST_APNS_SINK=1 it refuses to start rather than build a
// client that could dial Apple (a deliberate divergence from main). The
// recorder endpoint then answers 503.
func TestIdentityTestAPNSWithoutTheSinkRefusesToStart(t *testing.T) {
	identityTestAPNS.Lock()
	identityTestAPNS.recorder = nil
	identityTestAPNS.Unlock()
	for _, sink := range []string{"", "0", "true", "yes", " 1"} {
		t.Setenv(identityTestAPNSSinkEnv, sink)
		client, err := newAPNSClient(identityTestAPNSConfig(t))
		if err == nil || client != nil || !strings.Contains(err.Error(), identityTestAPNSSinkEnv) {
			t.Fatalf("sink %q: %v %v, want a refusal naming %s", sink, client, err, identityTestAPNSSinkEnv)
		}
	}
	response := httptest.NewRecorder()
	wrapIdentityTestHandler(http.NotFoundHandler(), "com.example.protocol").ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/__identitytest/apns", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("recorder endpoint without the sink: %d", response.Code)
	}
}

// BT_IDENTITYTEST_APNS_SINK_LATENCY_MS is the recorder's delay before it
// answers: whole milliseconds from 0 to 2,000, 0 when unset. Anything else
// refuses to start, naming the variable.
func TestIdentityTestAPNSSinkLatencyIsBoundedAtStartup(t *testing.T) {
	t.Setenv(identityTestAPNSSinkEnv, "1")
	for _, value := range []string{"-1", "2001", "abc", "1.5", " 5", "5ms", "+5", "99999999999999999999"} {
		t.Setenv(identityTestAPNSSinkLatencyEnv, value)
		client, err := newAPNSClient(identityTestAPNSConfig(t))
		if err == nil || client != nil || !strings.Contains(err.Error(), identityTestAPNSSinkLatencyEnv) {
			t.Fatalf("latency %q: %v %v, want a refusal naming %s", value, client, err, identityTestAPNSSinkLatencyEnv)
		}
	}
	for value, want := range map[string]time.Duration{"": 0, "0": 0, "100": 100 * time.Millisecond, "2000": 2 * time.Second} {
		t.Setenv(identityTestAPNSSinkLatencyEnv, value)
		client, err := newAPNSClient(identityTestAPNSConfig(t))
		if err != nil || client == nil {
			t.Fatalf("latency %q: %v", value, err)
		}
		identityTestAPNS.RLock()
		got := identityTestAPNS.recorder.latency
		identityTestAPNS.RUnlock()
		if got != want {
			t.Fatalf("latency %q: the recorder waits %v, want %v", value, got, want)
		}
	}
}

// The delay comes before the answer: a push waits for it, pushes made at
// once overlap in the recorder (which reports the most it held at once),
// and a push whose context ends inside the delay is Ambiguous (the recorder
// had the request) and recorded unacknowledged.
func TestIdentityTestAPNSSinkLatencyDelaysTheAnswer(t *testing.T) {
	t.Setenv(identityTestAPNSSinkEnv, "1")
	t.Setenv(identityTestAPNSSinkLatencyEnv, "300")
	client, err := newAPNSClient(identityTestAPNSConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	handler := wrapIdentityTestHandler(http.NotFoundHandler(), "com.example.protocol")
	token := bytes.Repeat([]byte{0x7c}, 32)
	started := time.Now()
	if result, err := client.Wake(t.Context(), apns.Sandbox, token); err != nil || result.Class != apns.Accepted {
		t.Fatalf("%+v %v", result, err)
	}
	if elapsed := time.Since(started); elapsed < 300*time.Millisecond {
		t.Fatalf("the answer came after %v, want the 300 ms delay", elapsed)
	}
	started = time.Now()
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if result, err := client.Wake(t.Context(), apns.Sandbox, token); err != nil || result.Class != apns.Accepted {
				t.Errorf("%+v %v", result, err)
			}
		})
	}
	wg.Wait()
	if elapsed := time.Since(started); elapsed >= 1500*time.Millisecond {
		t.Fatalf("five pushes at once took %v: the delay serialised them", elapsed)
	}
	record := readIdentityTestAPNS(t, handler)
	if record.InFlightPeak != 5 || record.Total != 6 {
		t.Fatalf("in_flight_peak %d total %d, want 5 and 6", record.InFlightPeak, record.Total)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started = time.Now()
	if result, err := client.Wake(ctx, apns.Sandbox, token); !errors.Is(err, apns.ErrOutcomeUnknown) || result.Class != apns.Ambiguous {
		t.Fatalf("a push whose context ended in the delay: %+v %v, want Ambiguous", result, err)
	}
	if elapsed := time.Since(started); elapsed >= 300*time.Millisecond {
		t.Fatalf("the cancelled push returned after %v, want at its context's end", elapsed)
	}
	record = readIdentityTestAPNS(t, handler)
	if last := record.Requests[len(record.Requests)-1]; last.Sequence != 7 || last.Acknowledged {
		t.Fatalf("the cancelled push was recorded %+v, want sequence 7 unacknowledged", last)
	}
}

// The recorder keeps the latest 4,096 requests, numbered from the first, and
// GET /__identitytest/apns?after=N returns only those numbered past N, so a
// sampler reads each request once however long the run.
func TestIdentityTestAPNSRecorderKeepsTheLatestRequests(t *testing.T) {
	t.Setenv(identityTestAPNSSinkEnv, "1")
	t.Setenv(identityTestAPNSSinkLatencyEnv, "")
	client, err := newAPNSClient(identityTestAPNSConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	handler := wrapIdentityTestHandler(http.NotFoundHandler(), "com.example.protocol")
	started := time.Now()
	for i := range 4100 {
		token := bytes.Repeat([]byte{byte(i)}, 32)
		if _, err := client.Wake(t.Context(), apns.Sandbox, token); err != nil {
			t.Fatal(err)
		}
	}
	record := readIdentityTestAPNS(t, handler)
	if len(record.Requests) != 4096 || record.Total != 4100 || record.InFlightPeak != 1 {
		t.Fatalf("%d requests, total %d, in_flight_peak %d; want the latest 4,096 of 4,100, one at a time", len(record.Requests), record.Total, record.InFlightPeak)
	}
	for i, got := range record.Requests {
		if got.Sequence != i+5 || got.Token != hex.EncodeToString(bytes.Repeat([]byte{byte(i + 4)}, 32)) {
			t.Fatalf("request %d: sequence %d token %s, want sequence %d", i, got.Sequence, got.Token, i+5)
		}
	}
	after := readIdentityTestAPNSPath(t, handler, "/__identitytest/apns?after=4098")
	if len(after.Requests) != 2 || after.Requests[0].Sequence != 4099 || after.Requests[1].Sequence != 4100 || after.Total != 4100 {
		t.Fatalf("after=4098: %+v", after)
	}
	if all := readIdentityTestAPNSPath(t, handler, "/__identitytest/apns?after=0"); len(all.Requests) != 4096 {
		t.Fatalf("after=0: %d requests", len(all.Requests))
	}
	// Every request carries when the recorder took it (the load runner's
	// duplicate check reads it): RFC 3339 with nanoseconds, UTC, in
	// sequence order, within the test's own time.
	var previous time.Time
	for _, got := range record.Requests {
		at, err := time.Parse(time.RFC3339Nano, got.ReceivedAt)
		if err != nil || at.Location() != time.UTC || at.Before(previous) || at.Before(started) || at.After(time.Now()) {
			t.Fatalf("request %d received_at %q (%v), previous %v", got.Sequence, got.ReceivedAt, err, previous)
		}
		previous = at
	}
	for _, bad := range []string{"x", "-1", "1.5", ""} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/__identitytest/apns?after="+bad, nil))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("after=%q: %d, want 400", bad, response.Code)
		}
	}
}

// apns-preflight in an identitytest build goes to the recorder through
// newAPNSClient, and a Live Activity preflight reaches it with its push type,
// derived topic, priority and main's quiet update payload.
func TestIdentityTestAPNSRecorderSeesTheLiveActivityPreflight(t *testing.T) {
	t.Setenv(identityTestAPNSSinkEnv, "1")
	creds := identityTestAPNSConfig(t)
	values := map[string]string{"BT_APNS_TEAM_ID": creds.TeamID, "BT_APNS_KEY_ID": creds.KeyID, "BT_APNS_KEY_FILE": creds.KeyFile, "BT_APNS_BUNDLE_ID": creds.BundleID}
	handler := wrapIdentityTestHandler(http.NotFoundHandler(), "com.example.protocol")
	for _, pushType := range []string{"liveactivity", "background"} {
		var out bytes.Buffer
		// The recorder accepts every push: a 200 is not Apple's answer to a dummy token.
		err := runAPNsPreflight(t.Context(), []string{"--environment", "sandbox", "--push-type", pushType}, func(name string) string { return values[name] }, connectAPNsPreflight, &out)
		if err == nil || out.String() != "apns_preflight environment=sandbox push_type="+pushType+" outcome=unexpected status=200 reason=\n" {
			t.Fatalf("%s: %q %v", pushType, out.String(), err)
		}
		record := readIdentityTestAPNS(t, handler)
		got := record.Requests[len(record.Requests)-1]
		topic, priority := creds.BundleID, "5"
		if pushType == "liveactivity" {
			topic, priority = creds.BundleID+".push-type.liveactivity", "10"
		}
		if got.Token != strings.Repeat("0", 64) || got.Host != "sandbox.apns.identitytest.invalid" || got.Headers["apns-push-type"] != pushType ||
			got.Headers["apns-topic"] != topic || got.Headers["apns-priority"] != priority || !got.ProviderTokenValid {
			t.Fatalf("%s: recorded %+v", pushType, got)
		}
		var payload struct {
			APS map[string]json.RawMessage `json:"aps"`
		}
		if err := json.Unmarshal(got.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if pushType == "liveactivity" && (string(payload.APS["event"]) != `"update"` || payload.APS["content-state"] == nil || payload.APS["alert"] != nil) {
			t.Fatalf("Live Activity preflight payload %s", got.Payload)
		}
		if pushType == "background" && string(got.Payload) != apns.WakePayload {
			t.Fatalf("background preflight payload %s", got.Payload)
		}
	}
}

// wakeTickRoute isolates GET /__identitytest/wake's worker for one test and
// returns its count reader.
func wakeTickRoute(t *testing.T) (http.Handler, func() uint64) {
	t.Helper()
	identityTestWake.Lock()
	previous := identityTestWake.worker
	identityTestWake.worker = nil
	identityTestWake.Unlock()
	t.Cleanup(func() { identityTestWakeWorker(previous) })
	handler := wrapIdentityTestHandler(http.NotFoundHandler(), "com.example.protocol")
	return handler, func() uint64 {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/__identitytest/wake", nil))
		if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("GET /__identitytest/wake: %d %v %s", response.Code, response.Header(), response.Body)
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || len(body) != 1 {
			t.Fatalf("wake body %s: %v", response.Body, err)
		}
		var count uint64
		if err := json.Unmarshal(body["ticks"], &count); err != nil {
			t.Fatalf("ticks %s: %v", body["ticks"], err)
		}
		return count
	}
}

// GET /__identitytest/wake reports the composed wake worker's successful
// ticks (Totals().TicksSucceeded), so the E2E harness waits for whole ticks
// rather than sleeping past the interval: 503 before newWakeWorker composed
// a worker, then 0 however many ticks fail. Here the pool points at a
// closed port, so every tick fails at BEGIN: it ran (Ticks grows), but it
// scanned and sent nothing, so the route does not count it.
func TestWakeTickRouteReportsTheWorkersTicks(t *testing.T) {
	handler, ticks := wakeTickRoute(t)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/__identitytest/wake", nil))
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("wake route before a worker: %d %v", response.Code, response.Header())
	}
	pool, err := pgxpool.New(t.Context(), "postgres://identitytest@127.0.0.1:1/none?sslmode=disable&connect_timeout=5")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	hub, err := realtime.NewHub(t.Context(), realtime.HubOptions{Limits: protocol.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	worker := newWakeWorker(config.Config{}, hub, pool, nil, wake.NewTracker(0), nil, nil)
	if got := ticks(); got != 0 {
		t.Fatalf("a new worker reports %d ticks", got)
	}
	for ran := uint64(1); ran <= 3; ran++ {
		if _, err := worker.Tick(t.Context()); err == nil {
			t.Fatal("a tick against a closed port succeeded")
		}
		if got, totals := ticks(), worker.Totals(); got != 0 || totals.Ticks != ran {
			t.Fatalf("after failed tick %d the route reports %d (worker %+v)", ran, got, totals)
		}
	}
}

// With a database, each successful tick of the newest composed worker
// counts once.
func TestWakeTickRouteCountsSuccessfulTicksOfTheNewestWorker(t *testing.T) {
	db := testpg.New(t)
	_, ticks := wakeTickRoute(t)
	hub, err := realtime.NewHub(t.Context(), realtime.HubOptions{Limits: protocol.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	first := newWakeWorker(config.Config{}, hub, db.Maintenance, nil, wake.NewTracker(0), nil, nil)
	for want := uint64(1); want <= 3; want++ {
		if _, err := first.Tick(t.Context()); err != nil {
			t.Fatalf("tick %d: %v", want, err)
		}
		if got := ticks(); got != want || got != first.Totals().TicksSucceeded {
			t.Fatalf("after tick %d the route reports %d (worker %+v)", want, got, first.Totals())
		}
	}
	// The route follows the worker newWakeWorker composed last.
	second := newWakeWorker(config.Config{}, hub, db.Maintenance, nil, wake.NewTracker(0), nil, nil)
	if got := ticks(); got != 0 {
		t.Fatalf("the route reports %d ticks for a new worker", got)
	}
	if _, err := second.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := ticks(); got != 1 {
		t.Fatalf("after the new worker's tick the route reports %d", got)
	}
}
