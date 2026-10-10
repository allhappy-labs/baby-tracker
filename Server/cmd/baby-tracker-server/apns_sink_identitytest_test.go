//go:build identitytest

package main

import (
	"bytes"
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
	"testing"

	"babytracker/server/internal/apns"
	"babytracker/server/internal/config"
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
	} `json:"requests"`
}

func readIdentityTestAPNS(t *testing.T, handler http.Handler) recordedAPNS {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/__identitytest/apns", nil))
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
