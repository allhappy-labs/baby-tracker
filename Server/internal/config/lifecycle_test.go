package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadLifecycleHostedDefaultsAndOverrides(t *testing.T) {
	directory := t.TempDir()
	database := filepath.Join(directory, "revoker-url")
	if err := os.WriteFile(database, []byte("postgres://revoker:secret@db/babytracker?sslmode=require\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := map[string]string{
		"BT_APPLE_REVOCATION_DATABASE_URL_FILE": database,
		"BT_APPLE_REVOCATION_WORKER_ID":         "hosted-worker:one",
	}
	configuration, err := LoadLifecycle(mapGetter(base), ModeHosted)
	if err != nil {
		t.Fatal(err)
	}
	if !configuration.Enabled || configuration.DatabaseURL != "postgres://revoker:secret@db/babytracker?sslmode=require" ||
		configuration.WorkerID != "hosted-worker:one" || configuration.PollInterval != 5*time.Second ||
		configuration.LeaseTTL != 30*time.Second || configuration.OrphanGrace != time.Hour {
		t.Fatalf("configuration=%+v", configuration)
	}
	overrides := mapsClone(base)
	overrides["BT_APPLE_REVOCATION_POLL_INTERVAL"] = "1m"
	overrides["BT_APPLE_REVOCATION_LEASE_TTL"] = "2m"
	overrides["BT_CREDENTIAL_ORPHAN_GRACE"] = "12m"
	configuration, err = LoadLifecycle(mapGetter(overrides), ModeHosted)
	if err != nil || configuration.PollInterval != time.Minute || configuration.LeaseTTL != 2*time.Minute || configuration.OrphanGrace != 12*time.Minute {
		t.Fatalf("overrides=%+v err=%v", configuration, err)
	}
}

func TestLoadLifecycleRejectsUnsafeOrMisappliedConfiguration(t *testing.T) {
	directory := t.TempDir()
	database := filepath.Join(directory, "revoker-url")
	if err := os.WriteFile(database, []byte("postgres://revoker:test@db/babytracker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	valid := map[string]string{"BT_APPLE_REVOCATION_DATABASE_URL_FILE": database, "BT_APPLE_REVOCATION_WORKER_ID": "worker-1"}
	tests := map[string]map[string]string{
		"missing database":   {"BT_APPLE_REVOCATION_WORKER_ID": "worker-1"},
		"direct database":    {"BT_APPLE_REVOCATION_DATABASE_URL": "postgres://secret", "BT_APPLE_REVOCATION_WORKER_ID": "worker-1"},
		"missing worker":     {"BT_APPLE_REVOCATION_DATABASE_URL_FILE": database},
		"unsafe worker":      {"BT_APPLE_REVOCATION_DATABASE_URL_FILE": database, "BT_APPLE_REVOCATION_WORKER_ID": "worker secret"},
		"short poll":         withLifecycleValue(valid, "BT_APPLE_REVOCATION_POLL_INTERVAL", "999ms"),
		"long poll":          withLifecycleValue(valid, "BT_APPLE_REVOCATION_POLL_INTERVAL", "5m1s"),
		"short lease":        withLifecycleValue(valid, "BT_APPLE_REVOCATION_LEASE_TTL", "9s"),
		"long lease":         withLifecycleValue(valid, "BT_APPLE_REVOCATION_LEASE_TTL", "5m1s"),
		"short orphan grace": withLifecycleValue(valid, "BT_CREDENTIAL_ORPHAN_GRACE", "10m59s"),
	}
	for name, values := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := LoadLifecycle(mapGetter(values), ModeHosted)
			if err == nil || strings.Contains(err.Error(), "secret") && name != "direct database" {
				t.Fatalf("error=%v", err)
			}
		})
	}
	for _, mode := range []Mode{ModeDevelopment, ModeSelfHosted} {
		if got, err := LoadLifecycle(mapGetter(map[string]string{}), mode); err != nil || got != (LifecycleConfig{}) {
			t.Fatalf("mode=%s disabled=%+v err=%v", mode, got, err)
		}
		if _, err := LoadLifecycle(mapGetter(map[string]string{"BT_APPLE_REVOCATION_WORKER_ID": "stale-worker"}), mode); err == nil {
			t.Fatalf("mode=%s accepted hosted setting", mode)
		}
	}
}

func withLifecycleValue(base map[string]string, name, value string) map[string]string {
	result := mapsClone(base)
	result[name] = value
	return result
}

func mapsClone(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}
