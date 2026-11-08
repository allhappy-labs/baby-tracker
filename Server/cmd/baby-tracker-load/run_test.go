//go:build identitytest

package main

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"babytracker/server/internal/loadtest"
)

// The runner's own invocation (Server/scripts/load-node.sh cmd_run), with
// every scenario option it passes through.
func TestRunFlagsAcceptTheRunnersInvocation(t *testing.T) {
	thresholds := filepath.Join(t.TempDir(), "thresholds.json")
	if err := os.WriteFile(thresholds, []byte(`{"sync_p95_ms": 250, "refresh_p95_ms": 300, "max_scheduler_lag_p99_ms": 20, "max_unexpected_ratio": 0.001}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseRunFlags([]string{"--base", "http://172.31.250.10:8080", "--host", "sync.load.test",
		"--fleet", "/run/load/private/fleet.jsonl", "--ledger", "/run/load/private/ledger.jsonl", "--tracked", "/run/load/private/tracked.jsonl",
		"--out", "/run/load/result.json", "--scenario", "ramp", "--load", "2", "--warmup", "08s", "--measure", "30m", "--seed", "09",
		"--thresholds", thresholds, "--steps", "0.5,1,1.5,2", "--step-duration", "5m", "--restart-grace", "90s"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Base != "http://172.31.250.10:8080" || cfg.Host != "sync.load.test" || cfg.Scenario != "ramp" || cfg.Load != 2 ||
		cfg.Warmup != 8*time.Second || cfg.Measure != 30*time.Minute || cfg.Seed != 9 || cfg.StepDuration != 5*time.Minute ||
		cfg.RestartGrace != 90*time.Second || !slices.Equal(cfg.Steps, []float64{0.5, 1, 1.5, 2}) || cfg.Thresholds.SyncP95 != 250*time.Millisecond ||
		cfg.TrackedDevices != 200 || !cfg.WithPump || cfg.Out != "/run/load/result.json" {
		t.Fatalf("config %+v", cfg)
	}
	storm, err := parseRunFlags([]string{"--base", "b", "--host", "h", "--fleet", "f", "--ledger", "l", "--tracked", "t", "--out", "o",
		"--scenario", "storm", "--load", "1", "--warmup", "1m", "--measure", "20m", "--storm-at", "10m", "--with-pump=false"})
	if err != nil || storm.StormAt != 10*time.Minute || storm.WithPump {
		t.Fatalf("storm %+v %v", storm, err)
	}
	// Task 14's live-only ramp: 0 → 4,000 → 8,000 → 12,000 sockets at 10k.
	live, err := parseRunFlags([]string{"--base", "b", "--host", "h", "--fleet", "f", "--ledger", "l", "--tracked", "t", "--out", "o",
		"--scenario", "ramp", "--load", "1", "--warmup", "2m", "--measure", "9m", "--steps", "1,2,3", "--step-duration", "3m",
		"--model-override", "live-only"})
	if err != nil || live.ModelOverride != loadtest.ModelLiveOnly || !slices.Equal(live.Steps, []float64{1, 2, 3}) {
		t.Fatalf("live-only %+v %v", live, err)
	}
	// Task 16 Step 4's hot maintenance, as the runner passes it.
	hot, superuser, err := parseRunFlagsWithSuperuser([]string{"--base", "b", "--host", "h", "--fleet", "f", "--ledger", "l", "--tracked", "t",
		"--out", "o", "--scenario", "soak", "--load", "2", "--warmup", "5m", "--measure", "15m", "--maintenance", "hot", "--maintenance-at", "4m",
		"--superuser-url-file", "/run/load/private/secrets/superuser_database_url"})
	if err != nil || hot.Maintenance != loadtest.MaintenanceHot || hot.MaintenanceAt != 4*time.Minute ||
		superuser != "/run/load/private/secrets/superuser_database_url" {
		t.Fatalf("hot maintenance %+v %q %v", hot, superuser, err)
	}
	// The generator's own profile, as the runner passes it through
	// (load-node.sh run ... --profile-dir profiles: no slash passes its
	// option filter, so the directory is relative to --out's).
	profiled, err := parseRunFlags([]string{"--base", "b", "--host", "h", "--fleet", "f", "--ledger", "l", "--tracked", "t",
		"--out", "/run/load/result.json", "--scenario", "soak", "--load", "2", "--warmup", "5m", "--measure", "30m", "--profile-dir", "profiles"})
	if err != nil || profiled.ProfileDir != "profiles" {
		t.Fatalf("profile %+v %v", profiled, err)
	}
}

func TestRunFlagsRefuseAnIncompleteOrLooseRun(t *testing.T) {
	base := []string{"--base", "b", "--host", "h", "--fleet", "f", "--ledger", "l", "--tracked", "t", "--out", "o", "--warmup", "1m"}
	loose := filepath.Join(t.TempDir(), "loose.json")
	_ = os.WriteFile(loose, []byte(`{"sync_p95_ms": 900, "refresh_p95_ms": 300, "max_scheduler_lag_p99_ms": 20, "max_unexpected_ratio": 0.001}`), 0o644)
	for name, extra := range map[string][]string{
		"no load":            {"--measure", "5m"},
		"no measure":         {"--load", "2"},
		"unknown scenario":   {"--load", "2", "--measure", "5m", "--scenario", "chaos"},
		"ramp without steps": {"--load", "2", "--measure", "5m", "--scenario", "ramp"},
		"bad step":           {"--load", "2", "--measure", "5m", "--scenario", "ramp", "--steps", "1,-2"},
		"falling step":       {"--load", "2", "--measure", "5m", "--scenario", "ramp", "--steps", "1,2,1.5"},
		"storm outside":      {"--load", "2", "--measure", "5m", "--scenario", "storm", "--storm-at", "6m"},
		"restart without at": {"--load", "2", "--measure", "5m", "--scenario", "restart"},
		"bad duration":       {"--load", "2", "--measure", "5 minutes"},
		"looser than spec":   {"--load", "2", "--measure", "5m", "--thresholds", loose},
		"stray argument":     {"--load", "2", "--measure", "5m", "extra"},
		"unknown override":   {"--load", "2", "--measure", "5m", "--model-override", "pull-only"},
		"live-only writes":   {"--load", "2", "--measure", "5m", "--scenario", "hot-family", "--model-override", "live-only"},
		"hot without db":     {"--load", "2", "--measure", "5m", "--maintenance", "hot"},
		"hot in a storm":     {"--load", "2", "--measure", "5m", "--scenario", "storm", "--storm-at", "1m", "--maintenance", "hot", "--superuser-url-file", "s"},
		"hot after measure":  {"--load", "2", "--measure", "5m", "--maintenance", "hot", "--maintenance-at", "5m", "--superuser-url-file", "s"},
		"cold maintenance":   {"--load", "2", "--measure", "5m", "--maintenance", "cold", "--superuser-url-file", "s"},
		"superuser alone":    {"--load", "2", "--measure", "5m", "--superuser-url-file", "s"},
	} {
		if _, err := parseRunFlags(append(slices.Clone(base), extra...)); !errors.Is(err, errUsage) {
			t.Errorf("%s: %v", name, err)
		}
	}
	missing := []string{"--base", "b", "--load", "1", "--measure", "1m"}
	if _, err := parseRunFlags(missing); !errors.Is(err, errUsage) {
		t.Errorf("missing paths: %v", err)
	}
}

// A run that cannot start (a stale fleet here) is the generator failing:
// an error, so the program exits non-zero.
func TestRunCommandFailsWhenTheGeneratorCannot(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fleet := filepath.Join(dir, "fleet.jsonl")
	h := loadtest.FleetHeader{Version: loadtest.FleetVersion, AsOf: time.Now().Add(-40 * 24 * time.Hour), Families: 1, Devices: 1}
	if err := loadtest.WriteFleet(fleet, h, []loadtest.DeviceState{{FamilyID: "f", DeviceID: "d"}}); err != nil {
		t.Fatal(err)
	}
	err := runCommand(t.Context(), []string{"--base", "http://127.0.0.1:1", "--host", "h", "--fleet", fleet, "--ledger", filepath.Join(dir, "l"),
		"--tracked", filepath.Join(dir, "t"), "--out", filepath.Join(dir, "result.json"), "--load", "1", "--warmup", "0s", "--measure", "1m"},
		discardLogger())
	if err == nil || errors.Is(err, errUsage) {
		t.Fatalf("stale fleet: %v", err)
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
