package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/apns"
	"babytracker/server/internal/config"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/realtime"
)

// The wake worker as main composes it pushes through APNs exactly when
// APNs wakes are enabled (never a typed-nil Pusher), and splits devices by
// the live hub's DeviceConnected.
func TestWakeWorkerPushesOnlyWhenAPNsWakesAreEnabled(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	path := filepath.Join(t.TempDir(), "apns.p8")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	credentials := config.APNSCredentials{TeamID: "TEAM123456", KeyID: "KEY1234567", KeyFile: path, BundleID: "com.example.babytracker"}
	client, err := apns.NewWithTransport(credentials, http.DefaultTransport, "https://sandbox.invalid", "https://production.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := realtime.NewHub(t.Context(), realtime.HubOptions{Limits: protocol.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	enabled, disabled := config.Config{}, config.Config{}
	enabled.APNSWakes = config.APNSWakesConfig{Enabled: true, Credentials: credentials}
	for _, test := range []struct {
		name   string
		cfg    config.Config
		client *apns.Client
		pushes bool
	}{
		{"enabled", enabled, client, true},
		{"disabled", disabled, nil, false},
		{"disabled with a client", disabled, client, false},
		{"enabled without a client", enabled, nil, false},
	} {
		worker := newWakeWorker(test.cfg, hub, nil, nil, nil, test.client, nil)
		switch {
		case test.pushes && worker.Pusher != client:
			t.Fatalf("%s: Pusher %v, want the APNs client", test.name, worker.Pusher)
		case !test.pushes && worker.Pusher != nil:
			t.Fatalf("%s: Pusher %#v, want none", test.name, worker.Pusher)
		}
		if worker.Connected == nil {
			t.Fatalf("%s: no Connected", test.name)
		}
	}
	// Connected is the live hub's DeviceConnected.
	const device, other = "40000000-0000-4000-8000-0000000000e1", "40000000-0000-4000-8000-0000000000e2"
	worker := newWakeWorker(enabled, hub, nil, nil, nil, client, nil)
	if worker.Connected(device) {
		t.Fatal("a device without a connection is connected")
	}
	principal := access.Principal{AccountID: "10000000-0000-4000-8000-0000000000e1", DeviceID: device, SessionID: "60000000-0000-4000-8000-0000000000e1",
		RecoveryEpoch: 1, SecurityRevision: 1, Scope: "family", SessionScope: "family", SessionFamilyID: "b0000000-0000-4000-8000-0000000000e1",
		AccessExpiresAt: time.Now().Add(time.Hour)}
	reservation, err := hub.ReserveAfter(t.Context(), hub.InvalidationMark(), principal, access.Grant{AccountID: principal.AccountID, FamilyID: principal.SessionFamilyID, Role: "owner", Revision: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !worker.Connected(device) || worker.Connected(other) {
		t.Fatal("Connected does not read the live hub")
	}
	reservation.Release()
	if worker.Connected(device) {
		t.Fatal("a released connection is still connected")
	}
}

// One APNs client serves wakes and Live Activities: it is built exactly once
// when either is enabled, from the shared BT_APNS_* credentials, and not at
// all when neither is.
func TestOneAPNsClientForWakesAndLiveActivities(t *testing.T) {
	credentials := config.APNSCredentials{TeamID: "TEAM123456", KeyID: "KEY1234567", KeyFile: "/unused", BundleID: "com.example.babytracker"}
	built := &apns.Client{}
	for _, test := range []struct {
		name        string
		wakes, live bool
		builds      int
	}{
		{"neither", false, false, 0},
		{"wakes", true, false, 1},
		{"live activities", false, true, 1},
		{"both", true, true, 1},
	} {
		cfg := config.Config{}
		if test.wakes {
			cfg.APNSWakes = config.APNSWakesConfig{Enabled: true, Credentials: credentials}
		}
		if test.live {
			cfg.LiveActivity = config.LiveActivityConfig{Enabled: true, Credentials: credentials}
		}
		var calls []config.APNSCredentials
		client, err := apnsClientFor(cfg, func(creds config.APNSCredentials) (*apns.Client, error) {
			calls = append(calls, creds)
			return built, nil
		})
		if err != nil || len(calls) != test.builds {
			t.Fatalf("%s: %d builds %v, want %d", test.name, len(calls), err, test.builds)
		}
		if (test.builds == 0) != (client == nil) || (client != nil && client != built) || (len(calls) == 1 && calls[0] != credentials) {
			t.Fatalf("%s: client %p calls %+v", test.name, client, calls)
		}
	}
	// A composition whose two switches disagree on the credentials is refused.
	cfg := config.Config{APNSWakes: config.APNSWakesConfig{Enabled: true, Credentials: credentials}}
	other := credentials
	other.KeyID = "KEY7654321"
	cfg.LiveActivity = config.LiveActivityConfig{Enabled: true, Credentials: other}
	if client, err := apnsClientFor(cfg, func(config.APNSCredentials) (*apns.Client, error) { return built, nil }); err == nil || client != nil {
		t.Fatalf("differing credentials: %v %v", client, err)
	}
}
