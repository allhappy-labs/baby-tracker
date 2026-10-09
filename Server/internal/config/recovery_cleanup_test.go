package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveryCleanupConfiguration(t *testing.T) {
	get := func(string) string { return "" }
	cfg, err := LoadRecoveryCleanup(get, ExportConfig{})
	if err != nil || cfg.Enabled {
		t.Fatalf("disabled: %+v %v", cfg, err)
	}
	cfg, err = LoadRecoveryCleanup(get, ExportConfig{Enabled: true, WorkerDatabaseURL: "postgres://maintenance"})
	if err != nil || !cfg.Enabled || cfg.WorkerDatabaseURL != "postgres://maintenance" {
		t.Fatalf("export reuse: %+v %v", cfg, err)
	}
	path := filepath.Join(t.TempDir(), "database-url")
	if err = os.WriteFile(path, []byte("postgres://baby_tracker_export_app:password@localhost/babytracker"), 0600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE": path}
	get = func(key string) string { return values[key] }
	cfg, err = LoadRecoveryCleanup(get, ExportConfig{})
	if err != nil || !cfg.Enabled {
		t.Fatalf("fallback: %+v %v", cfg, err)
	}
	values["BT_RECOVERY_MAINTENANCE_DATABASE_URL"] = "postgres://secret"
	if _, err = LoadRecoveryCleanup(get, ExportConfig{}); err == nil {
		t.Fatal("inline credential accepted")
	}
}
