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
	"babytracker/server/internal/security/hosted"
)

func TestHostedProviderPolicyIsModeBound(t *testing.T) {
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
	getenv := func(name string) string { return values[name] }
	configured, err := config.LoadSecurity(getenv, config.ModeHosted)
	if err != nil {
		t.Fatal(err)
	}
	properties := hosted.Properties{
		Provider: "fixture-provider", Region: "eu-test-1",
		LedgerFailureDomain: "ledger-domain", HeadFailureDomain: "head-domain",
		LedgerKeyID: "ledger-key-v1", HeadKeyID: "head-key-v1",
		Encrypted: true, ImmutableLedger: true, ReplicatedHead: true,
		ConditionalCreate: true, LinearizableCAS: true,
	}
	if configured.Backend != config.SecurityBackendHosted {
		t.Fatalf("backend=%q", configured.Backend)
	}
	if err := configured.Hosted.ValidateObserved(properties); err != nil {
		t.Fatalf("matching provider assertions rejected: %v", err)
	}
	properties.Region = "eu-test-2"
	if err := configured.Hosted.ValidateObserved(properties); err == nil {
		t.Fatal("configured region drift accepted")
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
