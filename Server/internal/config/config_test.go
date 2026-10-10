package config

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Catches ignored file credentials, unbounded reads, ambiguous sources, and
// accidental secret/path disclosure through configuration errors.
func TestLoadDatabaseURLFile(t *testing.T) {
	const secret = "postgres://runtime:sensitive@db/babytracker"
	for _, tc := range []struct{ name, content, want, wantError string }{
		{"plain", secret, secret, ""},
		{"LF", secret + "\n", secret, ""},
		{"CRLF", secret + "\r\n", secret, ""},
		{"limit", strings.Repeat("x", 16384), strings.Repeat("x", 16384), ""},
		{"empty", "", "", "BT_DATABASE_URL_FILE must contain one nonempty line of at most 16 KiB"},
		{"only newline", "\n", "", "BT_DATABASE_URL_FILE must contain one nonempty line of at most 16 KiB"},
		{"oversized", strings.Repeat("x", 16385), "", "BT_DATABASE_URL_FILE must contain one nonempty line of at most 16 KiB"},
		{"two newlines", secret + "\n\n", "", "BT_DATABASE_URL_FILE must contain one nonempty line of at most 16 KiB"},
		{"interior newline", secret + "\nextra", "", "BT_DATABASE_URL_FILE must contain one nonempty line of at most 16 KiB"},
		{"bare CR", secret + "\r", "", "BT_DATABASE_URL_FILE must contain one nonempty line of at most 16 KiB"},
		{"NUL", secret + "\x00", "", "BT_DATABASE_URL_FILE must contain one nonempty line of at most 16 KiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "private-secret-path")
			if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			values := validHostedEnvironment(t)
			delete(values, "BT_DATABASE_URL")
			values["BT_DATABASE_URL_FILE"] = path
			cfg, err := Load(mapGetter(values))
			if tc.wantError != "" {
				if err == nil || err.Error() != tc.wantError {
					t.Fatalf("unexpected error: %v", err)
				}
			} else if err != nil || cfg.DatabaseURL != tc.want {
				t.Fatal("file credential not loaded correctly", err)
			}
		})
	}
	for _, tc := range []struct {
		name      string
		both      bool
		directory bool
		want      string
	}{
		{"missing", false, false, "BT_DATABASE_URL_FILE could not be read"},
		{"directory", false, true, "BT_DATABASE_URL_FILE could not be read"},
		{"both", true, false, "BT_DATABASE_URL and BT_DATABASE_URL_FILE are mutually exclusive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := validHostedEnvironment(t)
			if !tc.both {
				delete(values, "BT_DATABASE_URL")
			}
			path := t.TempDir()
			if !tc.directory {
				path = filepath.Join(path, "private-missing")
			}
			values["BT_DATABASE_URL_FILE"] = path
			_, err := Load(mapGetter(values))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func mapGetter(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func validHostedEnvironment(t *testing.T) map[string]string {
	t.Helper()
	directory := t.TempDir()
	identityPepper := filepath.Join(directory, "identity-pepper")
	sessionPepper := filepath.Join(directory, "session-pepper")
	devicePepper := filepath.Join(directory, "device-pepper")
	notificationPepper := filepath.Join(directory, "notification-pepper")
	invitationPepper := filepath.Join(directory, "invitation-pepper")
	pageTokenPepper := filepath.Join(directory, "page-token-pepper")
	actorPepper := filepath.Join(directory, "actor-pepper")
	ratePepper := filepath.Join(directory, "rate-pepper")
	for index, path := range []string{identityPepper, sessionPepper, devicePepper, notificationPepper, invitationPepper, pageTokenPepper, actorPepper, ratePepper} {
		if err := os.WriteFile(path, []byte(strings.Repeat(string("12345678"[index]), 64)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	maintenanceURL := filepath.Join(directory, "maintenance-database-url")
	if err := os.WriteFile(maintenanceURL, []byte("postgres://maintenance@sync.example.test/babytracker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE": maintenanceURL,
		"BT_MODE":                           "hosted",
		"BT_DATABASE_URL":                   "postgres://sync.example.test/babytracker",
		"BT_PUBLIC_ORIGIN":                  "https://sync.example.test",
		"BT_SERVER_INSTANCE_ID":             "123e4567-e89b-12d3-a456-426614174000",
		"BT_RECOVERY_EPOCH":                 "7",
		"BT_APPLE_AUDIENCE":                 "com.example.babytracker",
		"BT_SESSION_SIGNING_KEY_FILE":       sessionPepper,
		"BT_IDENTITY_KEY_ID":                "identity.v1",
		"BT_IDENTITY_PEPPER_FILE":           identityPepper,
		"BT_SESSION_TOKEN_KEY_ID":           "session.v1",
		"BT_DEVICE_CONTINUITY_KEY_ID":       "device.v1",
		"BT_DEVICE_CONTINUITY_PEPPER_FILE":  devicePepper,
		"BT_APPLE_NOTIFICATION_KEY_ID":      "notification.v1",
		"BT_APPLE_NOTIFICATION_PEPPER_FILE": notificationPepper,
		"BT_INVITATION_KEY_ID":              "invitation.v1",
		"BT_INVITATION_PEPPER_FILE":         invitationPepper,
		"BT_PAGE_TOKEN_KEY_ID":              "page.v1",
		"BT_PAGE_TOKEN_KEY_FILE":            pageTokenPepper,
		"BT_ACTOR_ID_PEPPER_FILE":           actorPepper,
		"BT_RATE_LIMIT_PEPPER_FILE":         ratePepper,
		"BT_TRUSTED_PROXY_CIDRS":            "10.0.0.0/8,192.168.0.0/16",
	}
}

// writeCredentialFile writes a one-line database URL file and returns its path.
func writeCredentialFile(t *testing.T, url string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "database-url")
	if err := os.WriteFile(path, []byte(url+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadNeedsTheRuntimeAndMaintenanceCredentials(t *testing.T) {
	for _, mode := range []string{"hosted", "self_hosted", "development"} {
		t.Run(mode, func(t *testing.T) {
			values := validHostedEnvironment(t)
			values["BT_MODE"] = mode
			cfg, err := Load(mapGetter(values))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.DatabaseURL != values["BT_DATABASE_URL"] {
				t.Fatal("runtime database credential was not loaded")
			}
			if !cfg.RecoveryCleanup.Enabled || cfg.RecoveryCleanup.WorkerDatabaseURL != "postgres://maintenance@sync.example.test/babytracker" {
				t.Fatalf("maintenance database credential was not loaded: %+v", cfg.RecoveryCleanup)
			}
		})
	}
	values := validHostedEnvironment(t)
	values["BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE"] = writeCredentialFile(t, values["BT_DATABASE_URL"])
	if _, err := Load(mapGetter(values)); err == nil || !strings.Contains(err.Error(), "recovery maintenance credential must differ") {
		t.Fatalf("recovery maintenance sharing the runtime credential: %v", err)
	}
}

// Decision D3: retention, the family purge and wakes run as the
// maintenance principal, so every mode refuses to start without its
// credential, naming both settings that supply it. The export credential
// alone supplies it.
func TestMaintenanceCredentialIsRequired(t *testing.T) {
	for _, mode := range []string{"hosted", "self_hosted", "development"} {
		t.Run(mode, func(t *testing.T) {
			values := validHostedEnvironment(t)
			values["BT_MODE"] = mode
			delete(values, "BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE")
			_, err := Load(mapGetter(values))
			if err == nil {
				t.Fatal("configuration without a maintenance credential accepted")
			}
			for _, name := range []string{"BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE", "BT_EXPORT_DATABASE_URL_FILE", "maintenance principal"} {
				if !strings.Contains(err.Error(), name) {
					t.Fatalf("refusal does not name %s: %v", name, err)
				}
			}
		})
	}
	values := validHostedEnvironment(t)
	delete(values, "BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE")
	keys := filepath.Join(t.TempDir(), "export-keys.json")
	if err := os.WriteFile(keys, []byte(`{"export.v1":"`+strings.Repeat("ab", 32)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	values["BT_EXPORT_DATABASE_URL_FILE"] = writeCredentialFile(t, "postgres://export@sync.example.test/babytracker")
	values["BT_EXPORT_KEYS_FILE"], values["BT_EXPORT_KEY_ID"] = keys, "export.v1"
	cfg, err := Load(mapGetter(values))
	if err != nil {
		t.Fatalf("export credential alone refused: %v", err)
	}
	if !cfg.RecoveryCleanup.Enabled || cfg.RecoveryCleanup.WorkerDatabaseURL != "postgres://export@sync.example.test/babytracker" {
		t.Fatalf("export credential did not supply maintenance: %+v", cfg.RecoveryCleanup)
	}
}

func TestLoadRejectsUnsafeProductionConfiguration(t *testing.T) {
	base := validHostedEnvironment(t)
	tests := map[string]func(map[string]string){
		"missing database":              func(v map[string]string) { delete(v, "BT_DATABASE_URL") },
		"http public origin":            func(v map[string]string) { v["BT_PUBLIC_ORIGIN"] = "http://sync.example.test" },
		"origin path":                   func(v map[string]string) { v["BT_PUBLIC_ORIGIN"] = "https://sync.example.test/api" },
		"empty origin query":            func(v map[string]string) { v["BT_PUBLIC_ORIGIN"] = "https://sync.example.test?" },
		"origin credentials":            func(v map[string]string) { v["BT_PUBLIC_ORIGIN"] = "https://user:pass@sync.example.test" },
		"invalid instance id":           func(v map[string]string) { v["BT_SERVER_INSTANCE_ID"] = "shared" },
		"zero recovery epoch":           func(v map[string]string) { v["BT_RECOVERY_EPOCH"] = "0" },
		"unsafe integer recovery epoch": func(v map[string]string) { v["BT_RECOVERY_EPOCH"] = "9007199254740992" },
		"retired ledger directory":      func(v map[string]string) { v["BT_SECURITY_LEDGER_DIR"] = "/var/lib/babytracker/security-ledger" },
		"retired projector credential":  func(v map[string]string) { v["BT_PROJECTOR_DATABASE_URL_FILE"] = "/run/secrets/projector_database_url" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			values := maps.Clone(base)
			mutate(values)
			if _, err := Load(mapGetter(values)); err == nil {
				t.Fatal("Load succeeded")
			}
		})
	}
}

func TestLoadCanonicalizesHostedConfiguration(t *testing.T) {
	for _, origin := range []string{"https://SYNC.EXAMPLE.TEST:443/", "https://SYNC.EXAMPLE.TEST:0443/"} {
		t.Run(origin, func(t *testing.T) {
			values := validHostedEnvironment(t)
			values["BT_PUBLIC_ORIGIN"] = origin
			cfg, err := Load(mapGetter(values))
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.PublicOrigin.String(); got != "https://sync.example.test" {
				t.Fatalf("origin = %q", got)
			}
			if cfg.ListenAddress != ":8080" || cfg.RecoveryEpoch != 7 {
				t.Fatalf("cfg = %#v", cfg)
			}
		})
	}
}

func TestLoadPreservesPublicOriginPort(t *testing.T) {
	for _, origin := range []string{"http://localhost:8080", "https://sync.example.test:8443", "https://sync.example.test:08443"} {
		t.Run(origin, func(t *testing.T) {
			values := validSelfHostedEnvironment(t)
			values["BT_MODE"] = "development"
			values["BT_PUBLIC_ORIGIN"] = origin
			cfg, err := Load(mapGetter(values))
			if err != nil {
				t.Fatal(err)
			}
			want := strings.Replace(origin, ":08443", ":8443", 1)
			if cfg.PublicOrigin.String() != want {
				t.Fatalf("origin = %q; want %q", cfg.PublicOrigin, want)
			}
		})
	}
}
