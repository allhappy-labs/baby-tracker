package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const operatorID = "90000000-0000-4000-8000-000000000001"

func TestLoadOperatorConfiguration(t *testing.T) {
	directory := t.TempDir()
	ownership := writeOperatorFile(t, directory, "ownership-policy", "owner-policy-test-v1\n", 0o600)
	entitlement := writeOperatorFile(t, directory, "entitlement-policy", "entitlement-policy-test-v1", 0o600)
	key := writeOperatorFile(t, directory, "confirmation-key", strings.Repeat("42", 32)+"\n", 0o600)
	values := map[string]string{
		"BT_OPERATOR_ID":                         operatorID,
		"BT_OPERATOR_OWNERSHIP_POLICY_ID_FILE":   ownership,
		"BT_OPERATOR_ENTITLEMENT_POLICY_ID_FILE": entitlement,
		"BT_OPERATOR_CONFIRMATION_KEY_FILE":      key,
	}
	configuration, err := LoadOperator(mapGetter(values))
	if err != nil {
		t.Fatal(err)
	}
	if configuration.OperatorID != operatorID || configuration.OwnershipPolicyID != "owner-policy-test-v1" ||
		configuration.EntitlementPolicyID != "entitlement-policy-test-v1" || configuration.ConfirmationKey[0] != 0x42 || configuration.ConfirmationKey[31] != 0x42 {
		t.Fatalf("configuration=%+v", configuration)
	}

	configuration, err = LoadOperator(mapGetter(map[string]string{}))
	if err != nil || configuration != (OperatorConfig{}) {
		t.Fatalf("disabled configuration=%+v err=%v", configuration, err)
	}
}

func TestOperatorSecretFilesRequire0600RegularNonsymlink(t *testing.T) {
	for _, mode := range []os.FileMode{0o400, 0o640, 0o644} {
		t.Run(mode.String(), func(t *testing.T) {
			directory := t.TempDir()
			policy := writeOperatorFile(t, directory, "policy", "owner-policy-test-v1", mode)
			key := writeOperatorFile(t, directory, "key", strings.Repeat("42", 32), 0o600)
			_, err := LoadOperator(mapGetter(operatorEnvironment(policy, "", key)))
			if err == nil || strings.Contains(err.Error(), policy) {
				t.Fatalf("unsafe mode error=%v", err)
			}
		})
	}
	for name, special := range map[string]os.FileMode{
		"setuid": os.ModeSetuid,
		"setgid": os.ModeSetgid,
		"sticky": os.ModeSticky,
	} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			policy := writeOperatorFile(t, directory, "policy", "owner-policy-test-v1", 0o600|special)
			info, err := os.Stat(policy)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode()&special == 0 {
				t.Skip("filesystem did not preserve requested special mode bit")
			}
			key := writeOperatorFile(t, directory, "key", strings.Repeat("42", 32), 0o600)
			if _, err := LoadOperator(mapGetter(operatorEnvironment(policy, "", key))); err == nil {
				t.Fatalf("mode %v was accepted", info.Mode())
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		directory := t.TempDir()
		target := writeOperatorFile(t, directory, "target", "owner-policy-test-v1", 0o600)
		link := filepath.Join(directory, "policy")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		key := writeOperatorFile(t, directory, "key", strings.Repeat("42", 32), 0o600)
		_, err := LoadOperator(mapGetter(operatorEnvironment(link, "", key)))
		if err == nil || strings.Contains(err.Error(), link) {
			t.Fatalf("symlink error=%v", err)
		}
	})
	t.Run("directory", func(t *testing.T) {
		directory := t.TempDir()
		key := writeOperatorFile(t, directory, "key", strings.Repeat("42", 32), 0o600)
		_, err := LoadOperator(mapGetter(operatorEnvironment(directory, "", key)))
		if err == nil {
			t.Fatal("directory was accepted")
		}
	})
}

func TestLoadOperatorRejectsMalformedOrPartialConfiguration(t *testing.T) {
	directory := t.TempDir()
	validPolicy := writeOperatorFile(t, directory, "valid-policy", "owner-policy-test-v1", 0o600)
	validKey := writeOperatorFile(t, directory, "valid-key", strings.Repeat("42", 32), 0o600)
	tests := map[string]map[string]string{
		"missing operator": operatorEnvironment(validPolicy, "", validKey),
		"missing key": {
			"BT_OPERATOR_ID": operatorID, "BT_OPERATOR_OWNERSHIP_POLICY_ID_FILE": validPolicy,
		},
		"key without policy": {
			"BT_OPERATOR_ID": operatorID, "BT_OPERATOR_CONFIRMATION_KEY_FILE": validKey,
		},
		"direct policy":             {"BT_OPERATOR_OWNERSHIP_POLICY_ID": "owner-policy-test-v1"},
		"direct entitlement policy": {"BT_OPERATOR_ENTITLEMENT_POLICY_ID": "entitlement-policy-test-v1"},
		"direct key":                {"BT_OPERATOR_CONFIRMATION_KEY": strings.Repeat("42", 32)},
	}
	delete(tests["missing operator"], "BT_OPERATOR_ID")
	for name, values := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadOperator(mapGetter(values)); err == nil {
				t.Fatal("unsafe operator configuration was accepted")
			}
		})
	}

	for name, content := range map[string]string{
		"uppercase key": strings.Repeat("42", 31) + "4A",
		"short key":     strings.Repeat("42", 31),
		"two lines":     strings.Repeat("42", 32) + "\n\n",
		"bad policy":    "owner policy",
	} {
		t.Run(name, func(t *testing.T) {
			path := writeOperatorFile(t, t.TempDir(), "input", content, 0o600)
			values := operatorEnvironment(validPolicy, "", validKey)
			if name == "bad policy" {
				values["BT_OPERATOR_OWNERSHIP_POLICY_ID_FILE"] = path
			} else {
				values["BT_OPERATOR_CONFIRMATION_KEY_FILE"] = path
			}
			if _, err := LoadOperator(mapGetter(values)); err == nil {
				t.Fatal("malformed operator configuration was accepted")
			}
		})
	}
}

func operatorEnvironment(ownership, entitlement, key string) map[string]string {
	values := map[string]string{"BT_OPERATOR_ID": operatorID, "BT_OPERATOR_CONFIRMATION_KEY_FILE": key}
	if ownership != "" {
		values["BT_OPERATOR_OWNERSHIP_POLICY_ID_FILE"] = ownership
	}
	if entitlement != "" {
		values["BT_OPERATOR_ENTITLEMENT_POLICY_ID_FILE"] = entitlement
	}
	return values
}

func writeOperatorFile(t *testing.T, directory, name, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}
