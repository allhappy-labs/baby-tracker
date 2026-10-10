package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeAPNSKey writes a throwaway PKCS#8 key (generated here, never an Apple
// credential) with mode, and returns its path.
func writeAPNSKey(t *testing.T, key any, mode os.FileMode) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "apns.p8")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func throwawayP256(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// recordingGetter answers from values and records every variable read.
func recordingGetter(values map[string]string, read *[]string) func(string) string {
	return func(name string) string {
		*read = append(*read, name)
		return values[name]
	}
}

func TestAPNSWakesAreOptionalAndStrict(t *testing.T) {
	keyFile := writeAPNSKey(t, throwawayP256(t), 0o600)
	complete := func() map[string]string {
		return map[string]string{
			"BT_APNS_WAKES_ENABLED": "true",
			"BT_APNS_TEAM_ID":       "TEAM123456",
			"BT_APNS_KEY_ID":        "KEY1234567",
			"BT_APNS_KEY_FILE":      keyFile,
			"BT_APNS_BUNDLE_ID":     "com.example.babytracker",
		}
	}

	// Off: unset or "false". Nothing but the switch is read, so a stale or
	// unreadable credential in a self-hoster's environment never matters.
	for _, enabled := range []string{"", "false"} {
		values := complete()
		values["BT_APNS_WAKES_ENABLED"] = enabled
		values["BT_APNS_KEY_FILE"] = filepath.Join(t.TempDir(), "missing.p8")
		var read []string
		got, err := LoadAPNSWakes(recordingGetter(values, &read))
		if err != nil || got.Enabled || got.Credentials != (APNSCredentials{}) {
			t.Fatalf("BT_APNS_WAKES_ENABLED=%q: %+v %v", enabled, got, err)
		}
		if len(read) != 1 || read[0] != "BT_APNS_WAKES_ENABLED" {
			t.Fatalf("BT_APNS_WAKES_ENABLED=%q read %v, want only the switch", enabled, read)
		}
	}

	got, err := LoadAPNSWakes(mapGetter(complete()))
	want := APNSCredentials{TeamID: "TEAM123456", KeyID: "KEY1234567", KeyFile: keyFile, BundleID: "com.example.babytracker"}
	if err != nil || !got.Enabled || got.Credentials != want {
		t.Fatalf("complete configuration: %+v %v", got, err)
	}

	symlink := filepath.Join(t.TempDir(), "linked.p8")
	if err := os.Symlink(keyFile, symlink); err != nil {
		t.Fatal(err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	twoKeys := filepath.Join(t.TempDir(), "two.p8")
	first, _ := os.ReadFile(keyFile)
	if err := os.WriteFile(twoKeys, append(first, first...), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, variable, value string
	}{
		{"a world-readable key", "BT_APNS_KEY_FILE", writeAPNSKey(t, throwawayP256(t), 0o644)},
		{"a group-readable key", "BT_APNS_KEY_FILE", writeAPNSKey(t, throwawayP256(t), 0o640)},
		{"a symlinked key", "BT_APNS_KEY_FILE", symlink},
		{"an RSA key", "BT_APNS_KEY_FILE", writeAPNSKey(t, rsaKey, 0o600)},
		{"a P-384 key", "BT_APNS_KEY_FILE", writeAPNSKey(t, p384, 0o600)},
		{"two PEM blocks", "BT_APNS_KEY_FILE", twoKeys},
		{"a missing key file", "BT_APNS_KEY_FILE", filepath.Join(t.TempDir(), "absent-key.p8")},
		{"no key file", "BT_APNS_KEY_FILE", ""},
		{"a nine-character team ID", "BT_APNS_TEAM_ID", "TEAM12345"},
		{"a lower-case team ID", "BT_APNS_TEAM_ID", "team123456"},
		{"a nine-character key ID", "BT_APNS_KEY_ID", "KEY123456"},
		{"no team ID", "BT_APNS_TEAM_ID", ""},
		{"no bundle ID", "BT_APNS_BUNDLE_ID", ""},
		{"a one-label bundle ID", "BT_APNS_BUNDLE_ID", "babytracker"},
	} {
		values := complete()
		values[c.variable] = c.value
		_, err := LoadAPNSWakes(mapGetter(values))
		if err == nil {
			t.Fatalf("%s was accepted", c.name)
		}
		if !strings.Contains(err.Error(), c.variable) {
			t.Fatalf("%s: %q does not name %s", c.name, err, c.variable)
		}
		if c.value != "" && strings.Contains(err.Error(), c.value) {
			t.Fatalf("%s: %q prints the value", c.name, err)
		}
		for _, secret := range []string{keyFile, "TEAM123456", "KEY1234567"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("%s: %q prints a configured value", c.name, err)
			}
		}
	}

	for _, enabled := range []string{"maybe", "TRUE", "1", " true"} {
		values := complete()
		values["BT_APNS_WAKES_ENABLED"] = enabled
		if _, err := LoadAPNSWakes(mapGetter(values)); err == nil || !strings.Contains(err.Error(), "BT_APNS_WAKES_ENABLED") {
			t.Fatalf("BT_APNS_WAKES_ENABLED=%q: %v", enabled, err)
		}
	}
}

func TestReadAPNSPrivateKeyAcceptsBothPEMForms(t *testing.T) {
	key := throwawayP256(t)
	pkcs8 := writeAPNSKey(t, key, 0o600)
	sec1DER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	sec1 := filepath.Join(t.TempDir(), "sec1.pem")
	if err := os.WriteFile(sec1, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1DER}), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{pkcs8, sec1} {
		got, err := ReadAPNSPrivateKey(path)
		if err != nil || !got.Equal(key) {
			t.Fatalf("%s: %v", filepath.Base(path), err)
		}
	}
}

func TestLoadCarriesTheAPNSWakesConfiguration(t *testing.T) {
	values := validHostedEnvironment(t)
	cfg, err := Load(mapGetter(values))
	if err != nil || cfg.APNSWakes.Enabled {
		t.Fatalf("APNs wakes default: %+v %v", cfg.APNSWakes, err)
	}
	keyFile := writeAPNSKey(t, throwawayP256(t), 0o600)
	values["BT_APNS_WAKES_ENABLED"] = "true"
	values["BT_APNS_TEAM_ID"] = "TEAM123456"
	values["BT_APNS_KEY_ID"] = "KEY1234567"
	values["BT_APNS_KEY_FILE"] = keyFile
	values["BT_APNS_BUNDLE_ID"] = "com.example.babytracker"
	cfg, err = Load(mapGetter(values))
	if err != nil || !cfg.APNSWakes.Enabled || cfg.APNSWakes.Credentials.KeyFile != keyFile {
		t.Fatalf("APNs wakes enabled: %+v %v", cfg.APNSWakes, err)
	}
	values["BT_APNS_WAKES_ENABLED"] = "maybe"
	if _, err := Load(mapGetter(values)); err == nil || !strings.Contains(err.Error(), "BT_APNS_WAKES_ENABLED") {
		t.Fatalf("an invalid APNs switch started: %v", err)
	}
}
