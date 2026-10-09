//go:build identitytest

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/config"
	"babytracker/server/internal/liveactivity"
)

func identityTestAPNSConfig(t *testing.T) config.LiveActivityConfig {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "apns_key.p8")
	if err = os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.LiveActivityConfig{Enabled: true, TeamID: "E2ETEAM001", KeyID: "E2EKEY0001", KeyFile: path, BundleID: "com.example.babytracker.protocol"}
}

func identityTestAPNSRequest(event, token string) liveactivity.PushRequest {
	scope := liveactivity.ActivityScope{ServerInstanceID: "70000000-0000-4000-8000-000000000001", RecoveryEpoch: 1, FamilyID: "70000000-0000-4000-8000-000000000002", BabyID: "70000000-0000-4000-8000-000000000003", KindRaw: "feed"}
	kind := liveactivity.UpdateToken
	if event == "start" {
		kind = liveactivity.PushToStartToken
	}
	return liveactivity.PushRequest{Environment: "sandbox", Token: token, TokenKind: kind, APNSID: "70000000-0000-4000-8000-000000000004", Event: event,
		Timestamp: time.Unix(1_790_000_000, 0), Expiration: time.Unix(1_790_000_060, 0), Priority: 10,
		Attributes: liveactivity.ActivityAttributes{BabyID: scope.BabyID, BabyName: "Mira", KindRaw: "feed", Scope: scope},
		Content: liveactivity.ContentState{StartedAt: 800_000_000, ActivityKind: "breastLeft", Phase: "running", Parts: []liveactivity.PartLabel{},
			Remote: &liveactivity.Revision{Scope: scope, GenerationID: "70000000-0000-4000-8000-000000000005", PresentationVersion: 1, PresentationTimestamp: 1}}}
}

// The harness's Apple keeps the production transport and is reachable only in
// identitytest builds with BT_IDENTITYTEST_APNS_SINK=1.
func TestIdentityTestAPNSRecorderKeepsTheProductionTransport(t *testing.T) {
	cfg := identityTestAPNSConfig(t)
	t.Setenv(identityTestAPNSSinkEnv, "")
	plain, err := newLiveActivityTransport(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plain.(*liveactivity.APNSTransport); !ok {
		t.Fatalf("without the sink variable the transport is %T", plain)
	}
	t.Setenv(identityTestAPNSSinkEnv, "1")
	transport, err := newLiveActivityTransport(cfg)
	if err != nil {
		t.Fatal(err)
	}
	handler := wrapIdentityTestHandler(http.NotFoundHandler(), "com.example.babytracker.protocol")
	const token = "0a0b0c0d0e0f1011"
	result, err := transport.Send(t.Context(), identityTestAPNSRequest("start", token))
	if err != nil || result.Classification != liveactivity.Accepted {
		t.Fatalf("recorded send=%+v err=%v", result, err)
	}
	lose := httptest.NewRecorder()
	handler.ServeHTTP(lose, httptest.NewRequest(http.MethodPost, "/__identitytest/apns/lose-ack", strings.NewReader(`{"token":"`+token+`"}`)))
	if lose.Code != http.StatusNoContent {
		t.Fatalf("lose-ack status=%d", lose.Code)
	}
	result, err = transport.Send(t.Context(), identityTestAPNSRequest("update", token))
	if err == nil || result.Classification != liveactivity.Ambiguous {
		t.Fatalf("lost acknowledgment=%+v err=%v", result, err)
	}
	listed := httptest.NewRecorder()
	handler.ServeHTTP(listed, httptest.NewRequest(http.MethodGet, "/__identitytest/apns", nil))
	var body struct {
		Requests []identityTestAPNSRecord `json:"requests"`
	}
	if err = json.Unmarshal(listed.Body.Bytes(), &body); err != nil || len(body.Requests) != 2 {
		t.Fatalf("records=%s err=%v", listed.Body.String(), err)
	}
	first, second := body.Requests[0], body.Requests[1]
	if first.Host != "sandbox.apns.identitytest.invalid" || first.Path != "/3/device/"+token || first.Token != token || !first.Acknowledged ||
		!first.ProviderTokenValid || first.ProviderTokenKeyID != "E2EKEY0001" || first.ProviderTokenIssuer != "E2ETEAM001" ||
		first.Headers["apns-push-type"] != "liveactivity" || first.Headers["apns-topic"] != "com.example.babytracker.protocol.push-type.liveactivity" ||
		first.Headers["apns-expiration"] != "0" || first.Headers["apns-priority"] != "10" {
		t.Fatalf("start record=%+v", first)
	}
	var payload struct {
		APS struct {
			Event        string          `json:"event"`
			ContentState json.RawMessage `json:"content-state"`
			Attributes   json.RawMessage `json:"attributes"`
		} `json:"aps"`
	}
	if err = json.Unmarshal(first.Payload, &payload); err != nil || payload.APS.Event != "start" || len(payload.APS.ContentState) == 0 || len(payload.APS.Attributes) == 0 {
		t.Fatalf("start payload=%s err=%v", first.Payload, err)
	}
	if second.Acknowledged || second.Sequence != 2 {
		t.Fatalf("lost record=%+v", second)
	}
	bad := httptest.NewRecorder()
	handler.ServeHTTP(bad, httptest.NewRequest(http.MethodPost, "/__identitytest/apns/lose-ack", strings.NewReader(`{"token":"NOT-HEX"}`)))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid lose-ack status=%d", bad.Code)
	}
}
