//go:build identitytest

package identity_test

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"babytracker/server/internal/config"
)

// The hosted security provider is retired with the file ledger: its policy
// settings fail closed in hosted mode (as in every mode) instead of
// configuring a provider, and the production server links no ledger,
// recovery store or hosted adapter.
func TestHostedProviderPolicyIsRetired(t *testing.T) {
	values := map[string]string{
		"BT_HOSTED_PROVIDER_ID":                "fixture-provider",
		"BT_HOSTED_REGION":                     "eu-test-1",
		"BT_HOSTED_APPROVED_REGIONS":           "eu-test-1,eu-test-2",
		"BT_HOSTED_LEDGER_FAILURE_DOMAIN":      "ledger-domain",
		"BT_HOSTED_HEAD_FAILURE_DOMAIN":        "head-domain",
		"BT_HOSTED_LEDGER_KEY_ID":              "ledger-key-v1",
		"BT_HOSTED_HEAD_KEY_ID":                "head-key-v1",
		"BT_HOSTED_VAULT_KEY_ID":               "vault-key-v1",
		"BT_SECURITY_REPLAY_SCRATCH_PATH":      "/var/lib/babytracker/replay-scratch",
		"BT_SECURITY_REPLAY_SCRATCH_MAX_BYTES": "67108864",
	}
	err := config.RejectRetiredSecuritySettings(func(name string) string { return values[name] })
	if err == nil {
		t.Fatal("hosted provider policy accepted")
	}
	for name := range values {
		if !strings.Contains(err.Error(), name+" is retired") {
			t.Errorf("%s not refused: %v", name, err)
		}
	}
	values["BT_MODE"] = "hosted"
	if _, err := config.Load(func(name string) string { return values[name] }); err == nil || !strings.Contains(err.Error(), "BT_HOSTED_PROVIDER_ID is retired") {
		t.Fatalf("hosted configuration with provider policy: %v", err)
	}
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	command := exec.CommandContext(t.Context(), "go", "list", "-deps", "./cmd/baby-tracker-server")
	command.Dir = filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go list production server: %v: %s", err, bytes.TrimSpace(output))
	}
	for _, dependency := range strings.Fields(string(output)) {
		switch dependency {
		// Task 14 deletes internal/security (and account's dead Apple
		// revocation files, its last production linker) along with
		// internal/recovery and the hosted credential vault.
		case "babytracker/server/internal/security", "babytracker/server/internal/recovery",
			"babytracker/server/internal/security/localstore", "babytracker/server/internal/security/hosted",
			"babytracker/server/internal/identity/hostedcredentials":
			t.Errorf("production server still links %s", dependency)
		}
	}
}

func TestProductionServerDependencyGraphExcludesTestIdentityIssuer(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	serverRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	command := exec.CommandContext(t.Context(), "go", "list", "-deps", "./cmd/baby-tracker-server")
	command.Dir = serverRoot
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go list production server: %v: %s", err, bytes.TrimSpace(output))
	}
	for _, dependency := range strings.Fields(string(output)) {
		if dependency == "babytracker/server/internal/testidentity" {
			t.Fatal("production server depends on compile-time test identity issuer")
		}
	}
}
