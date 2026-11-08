package config

import (
	"testing"
	"time"

	"babytracker/server/internal/protocol"
)

func TestCanonicalSyncLimitsLoadDefaults(t *testing.T) {
	got, err := LoadSync(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if got.Limits != protocol.DefaultLimits() {
		t.Fatalf("limits = %+v, want %+v", got.Limits, protocol.DefaultLimits())
	}
	if got.Limits.MaxCanonicalRecordsPerFamily != 100000 || got.Limits.MaxChargedStorageBytesPerFamily != 256<<20 {
		t.Fatalf("durable limits = %+v", got.Limits)
	}
	if got.Limits.SnapshotMaxEntities != 250000 {
		t.Fatalf("snapshot entity limit = %d", got.Limits.SnapshotMaxEntities)
	}
	if got.Limits.ImportMaxSessionsPerFamily != 1000 || got.Limits.ImportMaxSessionsPerAccount != 100 {
		t.Fatalf("import retained-history limits = %+v", got.Limits)
	}
	if got.Limits.SocketAttemptIPPerMinute != 60 || got.Limits.SocketAttemptDevicePerMinute != 12 ||
		got.Limits.SocketAttemptAccountPerMinute != 36 || got.Limits.SocketAttemptFamilyPerMinute != 120 {
		t.Fatalf("socket attempt defaults = %+v", got.Limits)
	}
}

func TestRejectSyncLimitsOutsideHardRanges(t *testing.T) {
	for name, value := range map[string]string{
		"BT_SYNC_BATCH_SIZE":                      "0",
		"BT_SYNC_BODY_BYTES":                      "10485761",
		"BT_IMPORT_MAX_RECORDS":                   "99",
		"BT_IMPORT_MAX_BABIES":                    "501",
		"BT_IMPORT_MAX_SESSIONS_PER_FAMILY":       "9",
		"BT_IMPORT_MAX_SESSIONS_PER_ACCOUNT":      "1001",
		"BT_SYNC_PAGE_SIZE":                       "1001",
		"BT_SOCKET_PROCESS_LIMIT":                 "9",
		"BT_MAX_CANONICAL_RECORDS_PER_FAMILY":     "99",
		"BT_MAX_CHARGED_STORAGE_BYTES_PER_FAMILY": "16777215",
		"BT_SNAPSHOT_MAX_ENTITIES":                "999",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadSync(func(key string) string {
				if key == name {
					return value
				}
				return ""
			})
			if err == nil {
				t.Fatalf("accepted %s=%s", name, value)
			}
		})
	}
}

func TestSocketProcessLimitBounds(t *testing.T) {
	for raw, want := range map[string]int{"": 1700, "10": 10, "10000": 10000, "10001": 10001, "65536": 65536} {
		cfg, err := loadSyncForTest(map[string]string{"BT_SOCKET_PROCESS_LIMIT": raw})
		if err != nil || cfg.Limits.SocketProcessLimit != want {
			t.Fatalf("%q: %d %v, want %d", raw, cfg.Limits.SocketProcessLimit, err, want)
		}
	}
	for _, bad := range []string{"9", "65537", "100000", "-1", "1e4"} {
		if _, err := loadSyncForTest(map[string]string{"BT_SOCKET_PROCESS_LIMIT": bad}); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestLoadIntegratesEffectiveSyncLimits(t *testing.T) {
	values := validHostedEnvironment(t)
	values["BT_SYNC_BATCH_SIZE"] = "17"
	values["BT_RATE_LIMIT_SYNC_MUTATION_DEVICE_COUNT"] = "23"
	values["BT_RATE_LIMIT_SYNC_PULL_ACCOUNT_COUNT"] = "19"
	cfg, err := Load(mapGetter(values))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sync.Limits.SyncBatchSize != 17 || cfg.Sync.Limits.SyncBodyBytes != 1<<20 ||
		cfg.Sync.Limits.SyncMutationDevicePerMinute != 23 || cfg.Sync.Limits.SyncPullAccountPerMinute != 19 {
		t.Fatalf("integrated sync limits = %+v", cfg.Sync.Limits)
	}
}

func loadSyncForTest(values map[string]string) (SyncConfig, error) {
	return LoadSync(func(key string) string { return values[key] })
}

func TestBodyReadTimeoutIsConfigurable(t *testing.T) {
	for raw, want := range map[string]time.Duration{"": 5 * time.Second, "12s": 12 * time.Second, "1s": time.Second, "60s": time.Minute, "1m": time.Minute} {
		cfg, err := loadSyncForTest(map[string]string{"BT_BODY_READ_TIMEOUT": raw})
		if err != nil || cfg.BodyReadTimeout != want {
			t.Fatalf("%q: %v %v", raw, cfg.BodyReadTimeout, err)
		}
	}
	for _, bad := range []string{"0s", "61s", "999ms", "5", "-1s", " 5s", "abc", "1m1s"} {
		if _, err := loadSyncForTest(map[string]string{"BT_BODY_READ_TIMEOUT": bad}); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestLoadCarriesBodyReadTimeout(t *testing.T) {
	values := validHostedEnvironment(t)
	values["BT_BODY_READ_TIMEOUT"] = "9s"
	cfg, err := Load(mapGetter(values))
	if err != nil || cfg.Sync.BodyReadTimeout != 9*time.Second {
		t.Fatalf("%v %v", cfg.Sync.BodyReadTimeout, err)
	}
	delete(values, "BT_BODY_READ_TIMEOUT")
	if cfg, err = Load(mapGetter(values)); err != nil || cfg.Sync.BodyReadTimeout != DefaultBodyReadTimeout {
		t.Fatalf("default %v %v", cfg.Sync.BodyReadTimeout, err)
	}
}
