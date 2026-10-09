package config

import (
	"maps"
	"strings"
	"testing"

	"babytracker/server/internal/security/hosted"
)

func TestHostedCannotFallBackToFilesystem(t *testing.T) {
	values := validSelfHostedEnvironment(t)
	values["BT_MODE"] = "hosted"
	values["BT_APPLE_REVOCATION_DATABASE_URL"] = "postgres://apple-revoker@sync.example.test/babytracker"
	values["BT_APPLE_REVOCATION_WORKER_ID"] = "hosted-worker-1"

	_, err := Load(mapGetter(values))
	if err == nil || !strings.Contains(err.Error(), "hosted mode does not accept filesystem security stores") {
		t.Fatalf("hosted filesystem fallback error=%v", err)
	}
}

func TestHostedSecurityRequiresExplicitProviderPolicy(t *testing.T) {
	values := validHostedEnvironment(t)
	cfg, err := Load(mapGetter(values))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Security.Backend != SecurityBackendHosted || cfg.Security.Hosted.ProviderID != "fixture-provider" ||
		cfg.Security.Hosted.Region != "eu-test-1" || cfg.Security.Hosted.ReplayScratchMaxBytes != 64<<20 {
		t.Fatalf("hosted security config=%+v", cfg.Security)
	}
	observed := hosted.Properties{
		Provider: "fixture-provider", Region: "eu-test-1",
		LedgerFailureDomain: "ledger-domain", HeadFailureDomain: "head-domain",
		LedgerKeyID: "ledger-key-v1", HeadKeyID: "head-key-v1",
		Encrypted: true, ImmutableLedger: true, ReplicatedHead: true,
		ConditionalCreate: true, LinearizableCAS: true,
	}
	if err := cfg.Security.Hosted.ValidateObserved(observed); err != nil {
		t.Fatalf("matching observed properties rejected: %v", err)
	}
	observed.HeadFailureDomain = "unapproved-head-domain"
	if err := cfg.Security.Hosted.ValidateObserved(observed); err == nil {
		t.Fatal("changed provider property accepted")
	}
}

func TestHostedSecurityRejectsMissingOrAmbiguousPolicy(t *testing.T) {
	tests := map[string]func(map[string]string){
		"missing provider":  func(values map[string]string) { delete(values, "BT_HOSTED_PROVIDER_ID") },
		"unapproved region": func(values map[string]string) { values["BT_HOSTED_REGION"] = "eu-test-3" },
		"duplicate approved region": func(values map[string]string) {
			values["BT_HOSTED_APPROVED_REGIONS"] = "eu-test-1,eu-test-1"
		},
		"same failure domain": func(values map[string]string) {
			values["BT_HOSTED_HEAD_FAILURE_DOMAIN"] = values["BT_HOSTED_LEDGER_FAILURE_DOMAIN"]
		},
		"same authority key": func(values map[string]string) {
			values["BT_HOSTED_HEAD_KEY_ID"] = values["BT_HOSTED_LEDGER_KEY_ID"]
		},
		"vault reuses authority key": func(values map[string]string) {
			values["BT_HOSTED_VAULT_KEY_ID"] = values["BT_HOSTED_LEDGER_KEY_ID"]
		},
		"relative scratch path": func(values map[string]string) { values["BT_SECURITY_REPLAY_SCRATCH_PATH"] = "scratch" },
		"unbounded scratch":     func(values map[string]string) { values["BT_SECURITY_REPLAY_SCRATCH_MAX_BYTES"] = "0" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			values := maps.Clone(validHostedEnvironment(t))
			mutate(values)
			if _, err := Load(mapGetter(values)); err == nil {
				t.Fatal("unsafe hosted property policy accepted")
			}
		})
	}
}

func TestSelfHostedSecurityConfigurationIsUnchanged(t *testing.T) {
	values := validSelfHostedEnvironment(t)
	cfg, err := Load(mapGetter(values))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Security.Backend != SecurityBackendFilesystem || cfg.Security.LedgerDir == "" || cfg.Security.HeadDir == "" ||
		cfg.SecurityLedgerDir != cfg.Security.LedgerDir || cfg.LedgerHeadDir != cfg.Security.HeadDir {
		t.Fatalf("self-hosted security config=%+v", cfg.Security)
	}
}

func validSelfHostedEnvironment(t *testing.T) map[string]string {
	t.Helper()
	values := validHostedEnvironment(t)
	values["BT_MODE"] = "self_hosted"
	values["BT_SECURITY_LEDGER_DIR"] = "/var/lib/babytracker/security-ledger"
	values["BT_LEDGER_HEAD_DIR"] = "/var/lib/babytracker/ledger-head"
	for _, name := range hostedSecuritySettingNames {
		delete(values, name)
	}
	delete(values, "BT_APPLE_REVOCATION_DATABASE_URL_FILE")
	delete(values, "BT_APPLE_REVOCATION_DATABASE_URL")
	delete(values, "BT_APPLE_REVOCATION_WORKER_ID")
	return values
}
