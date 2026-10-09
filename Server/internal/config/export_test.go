package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadExportRequiresCompleteRestrictedConfiguration(t *testing.T) {
	if got, err := LoadExport(func(string) string { return "" }); err != nil || got.Enabled {
		t.Fatalf("absent export config = %+v, %v", got, err)
	}
	directory := t.TempDir()
	database := filepath.Join(directory, "database-url")
	keys := filepath.Join(directory, "keys")
	if err := os.WriteFile(database, []byte("postgres://export-worker@db/babytracker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keys, []byte(`{"key.v1":"`+strings.Repeat("12", 32)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"BT_EXPORT_DATABASE_URL_FILE": database, "BT_EXPORT_KEYS_FILE": keys, "BT_EXPORT_KEY_ID": "key.v1"}
	get := func(key string) string { return values[key] }
	got, err := LoadExport(get)
	if err != nil || !got.Enabled || got.ActiveKeyID != "key.v1" || got.WorkerDatabaseURL != "postgres://export-worker@db/babytracker" || len(got.Keys) != 1 {
		t.Fatalf("valid export config = %+v, %v", got, err)
	}
	delete(values, "BT_EXPORT_KEY_ID")
	if _, err := LoadExport(get); err == nil {
		t.Fatal("partial export config accepted")
	}
	values["BT_EXPORT_KEY_ID"] = "key.v1"
	if err := os.Chmod(keys, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadExport(get); err == nil {
		t.Fatal("world-readable export keys accepted")
	}
}

func TestLoadExportRejectsDuplicateAndUnknownKeys(t *testing.T) {
	directory := t.TempDir()
	database := filepath.Join(directory, "database-url")
	keys := filepath.Join(directory, "keys")
	if err := os.WriteFile(database, []byte("postgres://export-worker@db/babytracker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"BT_EXPORT_DATABASE_URL_FILE": database, "BT_EXPORT_KEYS_FILE": keys, "BT_EXPORT_KEY_ID": "key.v1"}
	get := func(key string) string { return values[key] }
	for _, content := range []string{
		`{"key.v1":"` + strings.Repeat("12", 32) + `","key.v1":"` + strings.Repeat("13", 32) + `"}`,
		`{"key.v1":"` + strings.Repeat("12", 32) + `","key.v2":"` + strings.Repeat("12", 32) + `"}`,
		`{"key.v2":"` + strings.Repeat("12", 32) + `"}`,
	} {
		if err := os.WriteFile(keys, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadExport(get); err == nil {
			t.Fatalf("invalid keyring accepted: %s", content)
		}
	}
}

func TestExportWorkerCredentialMustBeDistinct(t *testing.T) {
	values := validSelfHostedEnvironment(t)
	directory := t.TempDir()
	database := filepath.Join(directory, "export-database-url")
	keys := filepath.Join(directory, "export-keys")
	if err := os.WriteFile(keys, []byte(`{"key.v1":"`+strings.Repeat("12", 32)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	values["BT_EXPORT_DATABASE_URL_FILE"] = database
	values["BT_EXPORT_KEYS_FILE"] = keys
	values["BT_EXPORT_KEY_ID"] = "key.v1"
	if err := os.WriteFile(database, []byte(values["BT_DATABASE_URL"]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(mapGetter(values)); err == nil {
		t.Fatal("runtime database credential was accepted for export worker")
	}
	if err := os.WriteFile(database, []byte("postgres://export-worker@sync.example.test/babytracker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(mapGetter(values)); err != nil {
		t.Fatalf("distinct export credential rejected: %v", err)
	}
}
