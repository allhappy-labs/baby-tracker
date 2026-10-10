package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLiveActivitySwitch(t *testing.T) {
	keyFile := writeAPNSKey(t, throwawayP256(t), 0o600)
	complete := func() map[string]string {
		return map[string]string{
			"BT_LIVE_ACTIVITY_PUSH_ENABLED": "true",
			"BT_APNS_TEAM_ID":               "TEAM123456",
			"BT_APNS_KEY_ID":                "KEY1234567",
			"BT_APNS_KEY_FILE":              keyFile,
			"BT_APNS_BUNDLE_ID":             "com.example.babytracker",
		}
	}

	// Off: unset or "false". No BT_APNS_* variable is read, so no key file is
	// opened (the configured one here does not exist).
	for _, enabled := range []string{"", "false"} {
		values := complete()
		values["BT_LIVE_ACTIVITY_PUSH_ENABLED"] = enabled
		values["BT_APNS_KEY_FILE"] = filepath.Join(t.TempDir(), "missing.p8")
		var read []string
		got, err := LoadLiveActivity(recordingGetter(values, &read))
		if err != nil || got.Enabled || got.Credentials != (APNSCredentials{}) {
			t.Fatalf("BT_LIVE_ACTIVITY_PUSH_ENABLED=%q: %+v %v", enabled, got, err)
		}
		for _, name := range read {
			if strings.HasPrefix(name, "BT_APNS_") {
				t.Fatalf("BT_LIVE_ACTIVITY_PUSH_ENABLED=%q read %s", enabled, name)
			}
		}
	}

	// On: the same credentials as LoadAPNSCredentials (and so as wakes).
	got, err := LoadLiveActivity(mapGetter(complete()))
	want, wantErr := LoadAPNSCredentials(mapGetter(complete()))
	if err != nil || wantErr != nil || !got.Enabled || got.Credentials != want {
		t.Fatalf("complete configuration: %+v %v (want %+v %v)", got, err, want, wantErr)
	}

	values := complete()
	delete(values, "BT_APNS_KEY_FILE")
	if _, err := LoadLiveActivity(mapGetter(values)); err == nil || !strings.Contains(err.Error(), "BT_APNS_KEY_FILE") {
		t.Fatalf("no key file: %v", err)
	}

	for _, enabled := range []string{"maybe", "TRUE", "1", " true"} {
		values := complete()
		values["BT_LIVE_ACTIVITY_PUSH_ENABLED"] = enabled
		if _, err := LoadLiveActivity(mapGetter(values)); err == nil || !strings.Contains(err.Error(), "BT_LIVE_ACTIVITY_PUSH_ENABLED") {
			t.Fatalf("BT_LIVE_ACTIVITY_PUSH_ENABLED=%q: %v", enabled, err)
		}
	}

	// main's worker credential is not ported: either form is refused, on or
	// off, with the explanation and never the value (a distinctive sentinel,
	// in both the URL and the file form).
	const sentinel = "postgres://live_activity_worker:SENTINEL-7b3e91c4d2a8f605@db.invalid:5432/baby_tracker?sslmode=verify-full"
	for _, variable := range []string{"BT_LIVE_ACTIVITY_DATABASE_URL_FILE", "BT_LIVE_ACTIVITY_DATABASE_URL"} {
		for _, enabled := range []string{"true", "false", ""} {
			values := complete()
			values["BT_LIVE_ACTIVITY_PUSH_ENABLED"] = enabled
			values[variable] = sentinel
			_, err := LoadLiveActivity(mapGetter(values))
			if err == nil || !strings.Contains(err.Error(), "the Live Activity worker runs on the maintenance credential (BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE); remove the setting") ||
				!strings.Contains(err.Error(), variable) || strings.Contains(err.Error(), "SENTINEL") || strings.Contains(err.Error(), "db.invalid") {
				t.Fatalf("%s with the switch %q: %v", variable, enabled, err)
			}
		}
	}
}

func TestLoadCarriesTheLiveActivityConfiguration(t *testing.T) {
	values := validHostedEnvironment(t)
	cfg, err := Load(mapGetter(values))
	if err != nil || cfg.LiveActivity.Enabled {
		t.Fatalf("Live Activity default: %+v %v", cfg.LiveActivity, err)
	}
	keyFile := writeAPNSKey(t, throwawayP256(t), 0o600)
	values["BT_LIVE_ACTIVITY_PUSH_ENABLED"] = "true"
	values["BT_APNS_TEAM_ID"] = "TEAM123456"
	values["BT_APNS_KEY_ID"] = "KEY1234567"
	values["BT_APNS_KEY_FILE"] = keyFile
	values["BT_APNS_BUNDLE_ID"] = "com.example.babytracker"
	cfg, err = Load(mapGetter(values))
	if err != nil || !cfg.LiveActivity.Enabled || cfg.LiveActivity.Credentials.KeyFile != keyFile || cfg.APNSWakes.Enabled {
		t.Fatalf("Live Activity enabled: %+v %+v %v", cfg.LiveActivity, cfg.APNSWakes, err)
	}
	values["BT_LIVE_ACTIVITY_DATABASE_URL_FILE"] = "/run/secrets/SENTINEL-7b3e91c4d2a8f605"
	if _, err := Load(mapGetter(values)); err == nil || !strings.Contains(err.Error(), "BT_LIVE_ACTIVITY_DATABASE_URL_FILE") || strings.Contains(err.Error(), "SENTINEL") {
		t.Fatalf("a Live Activity worker credential started: %v", err)
	}
}

// With both switches on, one credential error is reported once, not once a
// switch.
func TestLoadReportsASharedAPNSCredentialErrorOnce(t *testing.T) {
	values := validHostedEnvironment(t)
	values["BT_APNS_WAKES_ENABLED"] = "true"
	values["BT_LIVE_ACTIVITY_PUSH_ENABLED"] = "true"
	values["BT_APNS_TEAM_ID"] = "TEAM123456"
	values["BT_APNS_KEY_ID"] = "KEY1234567"
	values["BT_APNS_BUNDLE_ID"] = "com.example.babytracker"
	_, err := Load(mapGetter(values))
	if err == nil || strings.Count(err.Error(), "BT_APNS_KEY_FILE") != 1 {
		t.Fatalf("both switches without a key: %v, want BT_APNS_KEY_FILE named once", err)
	}
	// Errors of their own are each still reported.
	values["BT_APNS_WAKES_ENABLED"] = "maybe"
	_, err = Load(mapGetter(values))
	if err == nil || !strings.Contains(err.Error(), "BT_APNS_WAKES_ENABLED") || strings.Count(err.Error(), "BT_APNS_KEY_FILE") != 1 {
		t.Fatalf("an invalid wake switch and no key: %v", err)
	}
}
