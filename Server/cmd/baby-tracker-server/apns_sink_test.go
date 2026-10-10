//go:build !identitytest

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"babytracker/server/internal/config"
)

// Production builds have one APNs client, Apple's, built from the shared
// BT_APNS_* credentials; whether it exists is apnsClientFor's decision.
func TestAPNSClientIsApplesInProductionBuilds(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	path := filepath.Join(t.TempDir(), "apns.p8")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BT_IDENTITYTEST_APNS_SINK", "") // irrelevant outside identitytest builds
	credentials := config.APNSCredentials{TeamID: "TEAM123456", KeyID: "KEY1234567", KeyFile: path, BundleID: "com.example.babytracker"}
	client, err := newAPNSClient(credentials)
	if err != nil || client == nil {
		t.Fatalf("%v %v", client, err)
	}
	client.CloseIdleConnections()
	missing := credentials
	missing.KeyFile = filepath.Join(t.TempDir(), "absent.p8")
	if client, err := newAPNSClient(missing); err == nil || client != nil {
		t.Fatalf("a missing key built a client: %v", err)
	}
}
