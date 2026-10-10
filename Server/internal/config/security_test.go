package config

import (
	"strings"
	"testing"
)

// The file ledger, the projector, the hosted security provider and the hosted
// Apple revocation worker are gone: every one of their settings fails closed
// in every mode, naming the setting, instead of being silently ignored.
func TestRetiredSecuritySettingsFailClosedInEveryMode(t *testing.T) {
	for _, mode := range []string{"hosted", "self_hosted", "development"} {
		for _, name := range RetiredSecuritySettings {
			t.Run(mode+"/"+name, func(t *testing.T) {
				values := validHostedEnvironment(t)
				values["BT_MODE"] = mode
				values[name] = "/var/lib/babytracker/retired"
				_, err := Load(mapGetter(values))
				if err == nil || !strings.Contains(err.Error(), name+" is retired") {
					t.Fatalf("retired %s: err=%v", name, err)
				}
			})
		}
	}
	values := validHostedEnvironment(t)
	values["BT_SECURITY_LEDGER_DIR"] = "/a"
	values["BT_LEDGER_HEAD_DIR"] = "/b"
	err := RejectRetiredSecuritySettings(mapGetter(values))
	if err == nil || !strings.Contains(err.Error(), "BT_SECURITY_LEDGER_DIR") || !strings.Contains(err.Error(), "BT_LEDGER_HEAD_DIR") {
		t.Fatalf("every retired setting is named: %v", err)
	}
	if RejectRetiredSecuritySettings(nil) == nil {
		t.Fatal("nil getter accepted")
	}
	if err := RejectRetiredSecuritySettings(mapGetter(validSelfHostedEnvironment(t))); err != nil {
		t.Fatalf("current environment rejected: %v", err)
	}
}

func TestSelfHostedConfigurationNeedsNoSecurityStores(t *testing.T) {
	cfg, err := Load(mapGetter(validSelfHostedEnvironment(t)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModeSelfHosted || cfg.DatabaseURL == "" || cfg.ServerInstanceID == "" || cfg.RecoveryEpoch != 7 {
		t.Fatalf("self-hosted config=%+v", cfg)
	}
}

func validSelfHostedEnvironment(t *testing.T) map[string]string {
	t.Helper()
	values := validHostedEnvironment(t)
	values["BT_MODE"] = "self_hosted"
	return values
}
