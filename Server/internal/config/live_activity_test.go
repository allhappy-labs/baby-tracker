package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLiveActivityConfig(t *testing.T) {
	if cfg, err := LoadLiveActivity(mapGetter(nil)); err != nil || cfg.Enabled {
		t.Fatalf("absent optional config: %+v %v", cfg, err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalECPrivateKey(key)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "apns-key")
	dbPath := filepath.Join(dir, "worker-db")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath, []byte("postgres://worker:secret@localhost/baby\n"), 0600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"BT_LIVE_ACTIVITY_PUSH_ENABLED": "true", "BT_APNS_TEAM_ID": "TEAM123456", "BT_APNS_KEY_ID": "KEY1234567", "BT_APNS_KEY_FILE": keyPath, "BT_APNS_BUNDLE_ID": "com.example.baby", "BT_LIVE_ACTIVITY_DATABASE_URL_FILE": dbPath}
	cfg, err := LoadLiveActivity(mapGetter(values))
	if err != nil || !cfg.Enabled || cfg.WorkerDatabaseURL != "postgres://worker:secret@localhost/baby" {
		t.Fatalf("enabled config: %+v %v", cfg, err)
	}
	for name, value := range map[string]string{"BT_APNS_TEAM_ID": "bad", "BT_APNS_KEY_ID": "bad", "BT_APNS_BUNDLE_ID": "bad bundle", "BT_APNS_KEY_FILE": "/missing", "BT_LIVE_ACTIVITY_DATABASE_URL_FILE": "/missing"} {
		copy := map[string]string{}
		for k, v := range values {
			copy[k] = v
		}
		copy[name] = value
		_, err := LoadLiveActivity(mapGetter(copy))
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), dir) {
			t.Fatalf("%s error: %v", name, err)
		}
	}
	values["BT_LIVE_ACTIVITY_PUSH_ENABLED"] = "invalid"
	if _, err := LoadLiveActivity(mapGetter(values)); err == nil {
		t.Fatal("malformed enable flag")
	}
	values["BT_LIVE_ACTIVITY_PUSH_ENABLED"] = "false"
	if cfg, err := LoadLiveActivity(mapGetter(values)); err != nil || cfg.Enabled {
		t.Fatalf("disabled must remain optional: %+v %v", cfg, err)
	}
}

func TestLiveActivityConfigDistinctWorker(t *testing.T) {
	values := validHostedEnvironment(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalECPrivateKey(key)
	keyPath := filepath.Join(t.TempDir(), "key")
	dbPath := filepath.Join(t.TempDir(), "db")
	os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0600)
	os.WriteFile(dbPath, []byte(values["BT_DATABASE_URL"]), 0600)
	values["BT_LIVE_ACTIVITY_PUSH_ENABLED"] = "true"
	values["BT_APNS_TEAM_ID"] = "TEAM123456"
	values["BT_APNS_KEY_ID"] = "KEY1234567"
	values["BT_APNS_BUNDLE_ID"] = "com.example.baby"
	values["BT_APNS_KEY_FILE"] = keyPath
	values["BT_LIVE_ACTIVITY_DATABASE_URL_FILE"] = dbPath
	if _, err := Load(mapGetter(values)); err == nil {
		t.Fatal("worker shared runtime credentials")
	}
}

func TestLiveActivityConfigRejectsUnsafeWorkerFile(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	secret := "postgres://worker:private-password@localhost/baby"
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, []byte(secret+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"BT_LIVE_ACTIVITY_PUSH_ENABLED": "true", "BT_APNS_TEAM_ID": "TEAM123456", "BT_APNS_KEY_ID": "KEY1234567", "BT_APNS_KEY_FILE": keyPath, "BT_APNS_BUNDLE_ID": "com.example.baby"}
	check := func(t *testing.T, path string) {
		t.Helper()
		values["BT_LIVE_ACTIVITY_DATABASE_URL_FILE"] = path
		result := make(chan error, 1)
		go func() { _, err := LoadLiveActivity(mapGetter(values)); result <- err }()
		select {
		case err := <-result:
			if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), path) {
				t.Fatalf("unsafe worker file error: %v", err)
			}
		case <-time.After(300 * time.Millisecond):
			t.Fatal("unsafe worker file blocked config startup")
		}
	}
	t.Run("permissive mode", func(t *testing.T) {
		if err := os.Chmod(regular, 0644); err != nil {
			t.Fatal(err)
		}
		check(t, regular)
		if err := os.Chmod(regular, 0600); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		link := filepath.Join(dir, "link")
		if err := os.Symlink(regular, link); err != nil {
			t.Fatal(err)
		}
		check(t, link)
	})
	t.Run("FIFO", func(t *testing.T) {
		fifo := filepath.Join(dir, "fifo")
		if err := syscall.Mkfifo(fifo, 0600); err != nil {
			t.Fatal(err)
		}
		check(t, fifo)
	})
}

// The preflight command validates the Apple identifiers and the mounted key
// exactly as an enabled startup does, before any worker URL exists, and reads
// the worker URL file through the same checks when one is configured.
func TestLiveActivityAPNSCredentialsLoadWithoutWorkerURL(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "apns-key")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"BT_APNS_TEAM_ID": "TEAM123456", "BT_APNS_KEY_ID": "KEY1234567", "BT_APNS_KEY_FILE": keyPath, "BT_APNS_BUNDLE_ID": "com.example.baby"}
	cfg, err := LoadAPNSCredentials(mapGetter(values))
	if err != nil || cfg.TeamID != "TEAM123456" || cfg.KeyID != "KEY1234567" || cfg.KeyFile != keyPath || cfg.BundleID != "com.example.baby" || cfg.WorkerDatabaseURL != "" {
		t.Fatalf("credentials: %+v %v", cfg, err)
	}
	for name, value := range map[string]string{"BT_APNS_TEAM_ID": "", "BT_APNS_KEY_ID": "short", "BT_APNS_BUNDLE_ID": "bad bundle", "BT_APNS_KEY_FILE": filepath.Join(dir, "missing")} {
		copy := map[string]string{}
		for k, v := range values {
			copy[k] = v
		}
		copy[name] = value
		_, err := LoadAPNSCredentials(mapGetter(copy))
		if err == nil || !strings.Contains(err.Error(), name) || strings.Contains(err.Error(), dir) {
			t.Fatalf("%s error: %v", name, err)
		}
	}
	if err := os.Chmod(keyPath, 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAPNSCredentials(mapGetter(values)); err == nil {
		t.Fatal("a group-readable key must be rejected like at startup")
	}
	dbPath := filepath.Join(dir, "worker-db")
	if err := os.WriteFile(dbPath, []byte("postgres://worker:secret@localhost/baby\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if url, err := ReadLiveActivityDatabaseURL(dbPath); err != nil || url != "postgres://worker:secret@localhost/baby" {
		t.Fatalf("worker url: %q %v", url, err)
	}
	if _, err := ReadLiveActivityDatabaseURL(""); err == nil || !strings.Contains(err.Error(), "BT_LIVE_ACTIVITY_DATABASE_URL_FILE") {
		t.Fatalf("empty path: %v", err)
	}
	if err := os.Chmod(dbPath, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLiveActivityDatabaseURL(dbPath); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("world-readable worker url: %v", err)
	}
}
