package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProtocolE2EUsesIdentityTestTag(t *testing.T) {
	directory := t.TempDir()
	arguments := filepath.Join(directory, "arguments")
	environment := filepath.Join(directory, "environment")
	stub := filepath.Join(directory, "go")
	writeExecutable(t, stub, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$BT_CAPTURE_ARGUMENTS\"\nprintf '%s\\n' \"${BABY_TRACKER_REQUIRE_DATABASE:-}\" >> \"$BT_CAPTURE_ENVIRONMENT\"\n")
	command := exec.CommandContext(t.Context(), "bash", "protocol-e2e.sh")
	command.Env = append(os.Environ(), "BT_PROTOCOL_E2E_STUB_MODE=1", "BABY_TRACKER_TEST_DATABASE_URL=postgres://disposable.invalid/test",
		"BT_PROTOCOL_E2E_GO="+stub, "BT_CAPTURE_ARGUMENTS="+arguments, "BT_CAPTURE_ENVIRONMENT="+environment)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("protocol e2e stub: %v: %s", err, output)
	}
	args, _ := os.ReadFile(arguments)
	for _, required := range []string{"test -race -count=1 ./internal/realtime", "test -tags identitytest -race -count=1 ./tests/protocol/..."} {
		if !strings.Contains(string(args), required) {
			t.Fatalf("arguments=%q missing %q", args, required)
		}
	}
	required, _ := os.ReadFile(environment)
	if got := strings.Fields(string(required)); len(got) != 2 || got[0] != "1" || got[1] != "1" {
		t.Fatalf("BABY_TRACKER_REQUIRE_DATABASE=%q", required)
	}
}

func TestProtocolE2EPropagatesTestFailure(t *testing.T) {
	directory := t.TempDir()
	stub := filepath.Join(directory, "go")
	writeExecutable(t, stub, "#!/bin/sh\nexit 23\n")
	command := exec.CommandContext(t.Context(), "bash", "protocol-e2e.sh")
	command.Env = append(os.Environ(), "BT_PROTOCOL_E2E_STUB_MODE=1", "BABY_TRACKER_TEST_DATABASE_URL=postgres://disposable.invalid/test", "BT_PROTOCOL_E2E_GO="+stub)
	if err := command.Run(); err == nil {
		t.Fatal("protocol e2e swallowed test failure")
	}
}

func TestProtocolE2ERefreshesEphemeralPortAfterRestart(t *testing.T) {
	script, err := os.ReadFile("protocol-e2e.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	refresh := strings.Index(text, `restart_binding=$(compose port app 8080)`)
	rebase := strings.Index(text, `base="http://$restart_binding"`)
	verify := strings.Index(text, `--phase verify`)
	if refresh < 0 || rebase < refresh || verify < rebase {
		t.Fatalf("restart binding is not refreshed before verification")
	}
}

func TestProtocolE2EAssertsExactPersistedCompletedFeed(t *testing.T) {
	script, err := os.ReadFile("protocol-e2e.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, required := range []string{
		"completed_feed=", "21000000-0000-4000-8000-000000000201",
		"11000000-0000-4000-8000-000000000101", "feed_components",
		"1:true:1:21100000-0000-4000-8000-000000000201,breast,left,600,true,true",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("external protocol gate omits exact completed-feed assertion %q", required)
		}
	}
}

// The iOS gate proves the live socket through the TLS front, so the front
// must pass the upgrade on, and the script must insist the step ran.
func TestProtocolE2EProxiesWebSocketUpgradesAndRequiresTheLiveStep(t *testing.T) {
	script, err := os.ReadFile("protocol-e2e.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, required := range []string{
		"source Server/scripts/lib/disposable-sync.sh",
		`start_disposable_sync "${BT_RECOVERY_E2E_MODE:-hosted}" normal`,
		// The live step is 18; the gate requires the last step, which the
		// sequential test reaches only after the live step passed.
		"grep -q '^two-caregiver proof: step 24:'",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("protocol e2e omits %q", required)
		}
	}
	helper, err := os.ReadFile("lib/disposable-sync.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"map $http_upgrade $connection_upgrade {",
		"proxy_http_version 1.1;",
		"proxy_set_header Upgrade $http_upgrade;",
		"proxy_set_header Connection $connection_upgrade;",
	} {
		if !strings.Contains(string(helper), required) {
			t.Fatalf("disposable TLS setup omits %q", required)
		}
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryRunnerRequiresBothModesAndRealPhases(t *testing.T) {
	runner, err := os.ReadFile("refresh-recovery-e2e.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(runner), "for mode in hosted self_hosted") {
		t.Fatal("missing independent mode compositions")
	}
	phases, err := os.ReadFile("refresh-recovery-phases.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"--phase recovery-bootstrap", "--phase recovery-verify", "BABY_TRACKER_RECOVERY_PHASE=bootstrap", "BABY_TRACKER_RECOVERY_PHASE=verify", "refresh-recovery crash:", "refresh-recovery proof: completed", "identitytest recovery cleanup completed", "has_function_privilege", "refresh-recovery-v1"} {
		if !strings.Contains(string(phases), required) {
			t.Fatalf("recovery runner omits %q", required)
		}
	}
}
func TestRecoveryRunnerRejectsExternalDatabase(t *testing.T) {
	c := exec.CommandContext(t.Context(), "bash", "refresh-recovery-e2e.sh")
	c.Env = append(os.Environ(), "BABY_TRACKER_TEST_DATABASE_URL=postgres://external.invalid/do-not-use")
	if output, err := c.CombinedOutput(); err == nil || !strings.Contains(string(output), "refuses external database") {
		t.Fatalf("external DB not refused: %v %s", err, output)
	}
}

func TestRecoveryRunnerRejectsStubBeforeDelegation(t *testing.T) {
	for _, stubMode := range []string{"1", ""} {
		t.Run("stub="+stubMode, func(t *testing.T) {
			directory := t.TempDir()
			scripts := filepath.Join(directory, "Server", "scripts")
			if err := os.MkdirAll(scripts, 0o700); err != nil {
				t.Fatal(err)
			}
			runner, err := os.ReadFile("refresh-recovery-e2e.sh")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(scripts, "refresh-recovery-e2e.sh")
			writeExecutable(t, path, string(runner))
			calls := filepath.Join(directory, "calls")
			writeExecutable(t, filepath.Join(scripts, "protocol-e2e.sh"), "#!/bin/sh\nprintf '%s\\n' \"$BT_RECOVERY_E2E_MODE\" >> \"$BT_CAPTURE_ARGUMENTS\"\necho 'refresh-recovery proof: completed synthetic-delegation'\n")
			command := exec.CommandContext(t.Context(), "bash", path)
			command.Env = append(os.Environ(), "BT_PROTOCOL_E2E_STUB_MODE="+stubMode, "BABY_TRACKER_TEST_DATABASE_URL=", "BT_CAPTURE_ARGUMENTS="+calls)
			output, runErr := command.CombinedOutput()
			delegated, readErr := os.ReadFile(calls)
			if stubMode != "" {
				if runErr == nil || !strings.Contains(string(output), "recovery runner refuses stub mode") {
					t.Errorf("stub input not refused: err=%v output=%s", runErr, output)
				}
				if !os.IsNotExist(readErr) {
					t.Errorf("stub input invoked child: calls=%q err=%v", delegated, readErr)
				}
				if strings.Contains(string(output), "refresh-recovery mode") || strings.Contains(string(output), "refresh-recovery proof:") {
					t.Errorf("stub input emitted acceptance markers: %s", output)
				}
				return
			}
			if runErr != nil || readErr != nil || string(delegated) != "hosted\nself_hosted\n" {
				t.Fatalf("normal delegation changed: run=%v read=%v calls=%q output=%s", runErr, readErr, delegated, output)
			}
			for _, mode := range []string{"hosted", "self_hosted"} {
				if !strings.Contains(string(output), "refresh-recovery mode completed: "+mode) {
					t.Errorf("missing completed mode %s", mode)
				}
			}
		})
	}
}

// The ordinary gate runs the Live Activity push profile against an exact
// schema, through the identitytest recorder, and insists the phone test ran
// to its last step; the refresh-recovery modes keep push disabled.
func TestProtocolE2ERequiresTheLiveActivityPushProfile(t *testing.T) {
	script, err := os.ReadFile("protocol-e2e.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, required := range []string{
		`if [[ -z ${BT_RECOVERY_E2E_MODE:-} ]]; then`,
		"export BT_DISPOSABLE_LIVE_ACTIVITY=1",
		`BABY_TRACKER_E2E_APNS="${BT_DISPOSABLE_LIVE_ACTIVITY:-0}"`,
		"grep -q '^live-activity proof: step 13:'",
		"live_activity_devices", "a synthetic APNs token appeared in the server log",
		"-run 'IdentityTestAPNS' ./cmd/baby-tracker-server",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("protocol e2e omits %q", required)
		}
	}
	if strings.Index(text, "export BT_DISPOSABLE_LIVE_ACTIVITY=1") > strings.Index(text, "start_disposable_sync") {
		t.Fatal("push profile is chosen after the composition starts")
	}
	helper, err := os.ReadFile("lib/disposable-sync.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`if [[ $schema != 29 || $manifest != "29:29:000029_baby_icon.sql" || $privilege_manifest != 29 ]]; then`,
		`BT_IDENTITYTEST_APNS_SINK: "1"`, "openssl ecparam -name prime256v1",
		`[[ $live_activity_capability == false ]]`, `${BT_DISPOSABLE_PERFORMANCE:-0} != 1`,
	} {
		if !strings.Contains(string(helper), required) {
			t.Fatalf("disposable composition omits %q", required)
		}
	}
	for _, forbidden := range []string{"schema >=", "schema -ge", "push.apple.com", "BT_APNS_KEY_FILE: /run/secrets/apns_key.p8"} {
		if strings.Contains(string(helper), forbidden) {
			t.Fatalf("disposable composition contains %q", forbidden)
		}
	}
}
