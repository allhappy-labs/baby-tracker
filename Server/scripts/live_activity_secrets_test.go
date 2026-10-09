package scripts

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const liveActivitySecretsScript = "provision-live-activity-secrets.sh"

// A deployment root as provision-unraid-secrets.sh leaves it: deployment.env
// naming the root itself, and a 0700 secrets directory.
func liveActivityDeployRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "baby-tracker-sync")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deployment.env"), []byte("BT_DEPLOY_ROOT="+root+"\nBT_SECRETS_DIR="+root+"/secrets\nBT_MODE=self_hosted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeTestP256Key(t *testing.T, path string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func runLiveActivitySecrets(t *testing.T, root string, args ...string) (int, string) {
	t.Helper()
	command := exec.Command("bash", append([]string{"./" + liveActivitySecretsScript}, args...)...)
	command.Dir = "."
	command.Env = append(os.Environ(), "BT_DEPLOY_ROOT="+root)
	if os.Geteuid() != 0 {
		command.Env = append(command.Env, "BT_ALLOW_UNOWNED_SECRETS=1")
	}
	output, err := command.CombinedOutput()
	status := 0
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("%s: %v: %s", liveActivitySecretsScript, err, output)
		}
		status = exit.ExitCode()
	}
	return status, string(output)
}

func secretMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("%s is not a regular file: %v", path, info.Mode())
	}
	return info.Mode().Perm()
}

var hexPassword = regexp.MustCompile(`^[0-9a-f]{64}\n$`)

func TestLiveActivitySecretsDryRunWritesNothingAndNamesOwners(t *testing.T) {
	root := liveActivityDeployRoot(t)
	keyPath := filepath.Join(t.TempDir(), "AuthKey_KEY1234567.p8")
	keyPEM := writeTestP256Key(t, keyPath)
	status, output := runLiveActivitySecrets(t, root, "--dry-run", "--apns-key", keyPath)
	if status != 0 {
		t.Fatalf("dry run exit=%d:\n%s", status, output)
	}
	entries, err := os.ReadDir(filepath.Join(root, "secrets"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("dry run wrote into secrets/: %v %v", entries, err)
	}
	for _, required := range []string{
		"would create " + root + "/secrets/live_activity_password (owner 70:70, mode 0600)",
		"would create " + root + "/secrets/live_activity_database_url (owner 65532:65532, mode 0600)",
		"would install " + root + "/secrets/apns_key.p8 (owner 65532:65532, mode 0600)",
		"DRY RUN",
	} {
		if !strings.Contains(output, required) {
			t.Errorf("dry run omits %q:\n%s", required, output)
		}
	}
	if strings.Contains(output, strings.Split(keyPEM, "\n")[1]) {
		t.Fatalf("dry run printed key material:\n%s", output)
	}
	// Without a key argument the dry run still describes the database pair.
	status, output = runLiveActivitySecrets(t, root, "--dry-run")
	if status != 0 || !strings.Contains(output, "apns_key.p8 not provided") {
		t.Fatalf("dry run without key exit=%d:\n%s", status, output)
	}
}

// A root given with a trailing slash — shell completion leaves one — is the
// same root: the self-naming proof compares it with deployment.env's exact
// line, and the paths it prints carry no double slash.
func TestLiveActivitySecretsAcceptsATrailingSlashOnTheRoot(t *testing.T) {
	root := liveActivityDeployRoot(t)
	status, output := runLiveActivitySecrets(t, root+"/", "--dry-run")
	if status != 0 {
		t.Fatalf("dry run with a trailing slash exit=%d:\n%s", status, output)
	}
	if !strings.Contains(output, "would create "+root+"/secrets/live_activity_password") || strings.Contains(output, root+"//") {
		t.Fatalf("paths are not the root's own:\n%s", output)
	}
}

func TestLiveActivitySecretsProvisionsIdempotentlyAndRefusesOverwrite(t *testing.T) {
	root := liveActivityDeployRoot(t)
	secrets := filepath.Join(root, "secrets")
	keyPath := filepath.Join(t.TempDir(), "AuthKey_KEY1234567.p8")
	keyPEM := writeTestP256Key(t, keyPath)

	status, output := runLiveActivitySecrets(t, root, "--apns-key", keyPath)
	if status != 0 {
		t.Fatalf("provision exit=%d:\n%s", status, output)
	}
	password, err := os.ReadFile(filepath.Join(secrets, "live_activity_password"))
	if err != nil || !hexPassword.Match(password) {
		t.Fatalf("password file: %q %v", password, err)
	}
	url, err := os.ReadFile(filepath.Join(secrets, "live_activity_database_url"))
	if err != nil || string(url) != "postgres://baby_tracker_live_activity_app:"+strings.TrimSuffix(string(password), "\n")+"@db:5432/babytracker?sslmode=disable\n" {
		t.Fatalf("database url: %q %v", url, err)
	}
	installed, err := os.ReadFile(filepath.Join(secrets, "apns_key.p8"))
	if err != nil || string(installed) != keyPEM {
		t.Fatalf("installed key differs: %v", err)
	}
	for _, name := range []string{"live_activity_password", "live_activity_database_url", "apns_key.p8"} {
		if mode := secretMode(t, filepath.Join(secrets, name)); mode != 0o600 {
			t.Fatalf("%s mode %o, want 0600", name, mode)
		}
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("the source key must stay where the operator put it (removal is the operator's step): %v", err)
	}
	if entries, _ := os.ReadDir(secrets); len(entries) != 3 {
		t.Fatalf("secrets/ holds more than the three files (temporary files left behind?): %v", entries)
	}
	secret := strings.TrimSuffix(string(password), "\n")
	for _, forbidden := range []string{secret, strings.Split(keyPEM, "\n")[1]} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("output leaks a secret value:\n%s", output)
		}
	}
	for _, required := range []string{"PUSH SECRETS: OK", "created " + secrets + "/live_activity_password", "installed " + secrets + "/apns_key.p8"} {
		if !strings.Contains(output, required) {
			t.Errorf("output omits %q:\n%s", required, output)
		}
	}
	if os.Geteuid() != 0 && !strings.Contains(output, "ownership not applied") {
		t.Errorf("a non-root run must say that ownership was not applied:\n%s", output)
	}

	// Idempotent: a rerun with the same inputs changes nothing.
	status, output = runLiveActivitySecrets(t, root, "--apns-key", keyPath)
	if status != 0 || !strings.Contains(output, "unchanged") {
		t.Fatalf("rerun exit=%d:\n%s", status, output)
	}
	again, _ := os.ReadFile(filepath.Join(secrets, "live_activity_password"))
	if string(again) != string(password) {
		t.Fatal("rerun rotated the password")
	}
	status, output = runLiveActivitySecrets(t, root)
	if status != 0 || !strings.Contains(output, "unchanged") {
		t.Fatalf("rerun without key exit=%d:\n%s", status, output)
	}

	// A different key is an overwrite: refused without --force, applied with it.
	otherPath := filepath.Join(t.TempDir(), "AuthKey_OTHER.p8")
	otherPEM := writeTestP256Key(t, otherPath)
	status, output = runLiveActivitySecrets(t, root, "--apns-key", otherPath)
	if status != 1 || !strings.Contains(output, "--force") {
		t.Fatalf("overwrite exit=%d:\n%s", status, output)
	}
	if kept, _ := os.ReadFile(filepath.Join(secrets, "apns_key.p8")); string(kept) != keyPEM {
		t.Fatal("refused overwrite still replaced the key")
	}
	status, output = runLiveActivitySecrets(t, root, "--apns-key", otherPath, "--force")
	if status != 0 {
		t.Fatalf("forced exit=%d:\n%s", status, output)
	}
	if replaced, _ := os.ReadFile(filepath.Join(secrets, "apns_key.p8")); string(replaced) != otherPEM {
		t.Fatal("--force did not install the new key")
	}
	rotated, _ := os.ReadFile(filepath.Join(secrets, "live_activity_password"))
	rotatedURL, _ := os.ReadFile(filepath.Join(secrets, "live_activity_database_url"))
	if string(rotated) == string(password) || !hexPassword.Match(rotated) || !strings.Contains(string(rotatedURL), strings.TrimSuffix(string(rotated), "\n")) {
		t.Fatalf("--force must rotate the password and its URL together: %q %q", rotated, rotatedURL)
	}
	if !strings.Contains(output, "20-init-users.sh") {
		t.Errorf("a rotated password must remind the operator to rerun the initializer:\n%s", output)
	}

	// A fresh root without a key provisions the database pair and leaves the key pending.
	fresh := liveActivityDeployRoot(t)
	status, output = runLiveActivitySecrets(t, fresh)
	if status != 0 || !strings.Contains(output, "apns_key.p8 not provided") {
		t.Fatalf("pair-only exit=%d:\n%s", status, output)
	}
	if _, err := os.Stat(filepath.Join(fresh, "secrets", "apns_key.p8")); !os.IsNotExist(err) {
		t.Fatal("a key appeared from nowhere")
	}
	secretMode(t, filepath.Join(fresh, "secrets", "live_activity_password"))
}

func TestLiveActivitySecretsRefusesUnsafeInputs(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "AuthKey_KEY1234567.p8")
	writeTestP256Key(t, keyPath)
	refuse := func(t *testing.T, root, want string, args ...string) {
		t.Helper()
		status, output := runLiveActivitySecrets(t, root, args...)
		if status != 1 || !strings.Contains(output, want) {
			t.Fatalf("exit=%d want refusal %q:\n%s", status, want, output)
		}
		if entries, _ := os.ReadDir(filepath.Join(root, "secrets")); len(entries) != 0 {
			t.Fatalf("refusal left files behind: %v", entries)
		}
	}
	t.Run("foreign_root", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.MkdirAll(filepath.Join(root, "secrets"), 0o700); err != nil {
			t.Fatal(err)
		}
		refuse(t, root, "deployment.env", "--apns-key", keyPath)
	})
	t.Run("root_named_for_another_deployment", func(t *testing.T) {
		root := liveActivityDeployRoot(t)
		if err := os.WriteFile(filepath.Join(root, "deployment.env"), []byte("BT_DEPLOY_ROOT=/mnt/user/appdata/other\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		refuse(t, root, "deployment.env", "--apns-key", keyPath)
	})
	t.Run("symlinked_root", func(t *testing.T) {
		root := liveActivityDeployRoot(t)
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(root, link); err != nil {
			t.Fatal(err)
		}
		refuse(t, link, "symlink", "--apns-key", keyPath)
	})
	t.Run("missing_key", func(t *testing.T) {
		refuse(t, liveActivityDeployRoot(t), "regular", "--apns-key", filepath.Join(t.TempDir(), "absent.p8"))
	})
	t.Run("symlinked_key", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "key-link.p8")
		if err := os.Symlink(keyPath, link); err != nil {
			t.Fatal(err)
		}
		refuse(t, liveActivityDeployRoot(t), "regular", "--apns-key", link)
	})
	t.Run("not_p256", func(t *testing.T) {
		_, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(private)
		if err != nil {
			t.Fatal(err)
		}
		other := filepath.Join(t.TempDir(), "ed25519.p8")
		if err := os.WriteFile(other, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		refuse(t, liveActivityDeployRoot(t), "P-256", "--apns-key", other)
	})
	t.Run("not_a_key", func(t *testing.T) {
		text := filepath.Join(t.TempDir(), "notes.p8")
		if err := os.WriteFile(text, []byte("-----BEGIN PRIVATE KEY-----\nnot base64\n-----END PRIVATE KEY-----\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		refuse(t, liveActivityDeployRoot(t), "P-256", "--apns-key", text)
	})
	t.Run("malformed_existing_password", func(t *testing.T) {
		root := liveActivityDeployRoot(t)
		if err := os.WriteFile(filepath.Join(root, "secrets", "live_activity_password"), []byte("short\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "secrets", "live_activity_database_url"), []byte("postgres://baby_tracker_live_activity_app:short@db:5432/babytracker?sslmode=disable\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		status, output := runLiveActivitySecrets(t, root)
		if status != 1 || !strings.Contains(output, "malformed") {
			t.Fatalf("exit=%d:\n%s", status, output)
		}
	})
	t.Run("half_provisioned_pair", func(t *testing.T) {
		root := liveActivityDeployRoot(t)
		if err := os.WriteFile(filepath.Join(root, "secrets", "live_activity_password"), []byte(strings.Repeat("ab", 32)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		status, output := runLiveActivitySecrets(t, root)
		if status != 1 || !strings.Contains(output, "--force") {
			t.Fatalf("exit=%d:\n%s", status, output)
		}
	})
	t.Run("unknown_flag", func(t *testing.T) {
		status, output := runLiveActivitySecrets(t, liveActivityDeployRoot(t), "--apns-key")
		if status != 2 || !strings.Contains(output, "usage") {
			t.Fatalf("exit=%d:\n%s", status, output)
		}
	})
	if os.Geteuid() != 0 {
		t.Run("requires_root_unless_overridden", func(t *testing.T) {
			root := liveActivityDeployRoot(t)
			command := exec.Command("bash", "./"+liveActivitySecretsScript, "--apns-key", keyPath)
			command.Dir = "."
			command.Env = append(os.Environ(), "BT_DEPLOY_ROOT="+root, "BT_ALLOW_UNOWNED_SECRETS=")
			output, err := command.CombinedOutput()
			if err == nil || !strings.Contains(string(output), "requires root") {
				t.Fatalf("non-root run accepted: %v:\n%s", err, output)
			}
		})
	}
}

func TestLiveActivitySecretsScriptFollowsProvisionerContract(t *testing.T) {
	encoded, err := os.ReadFile(liveActivitySecretsScript)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, required := range []string{
		"set -euo pipefail", "umask 077", "BT_DEPLOY_ROOT", "baby-tracker-sync",
		"openssl rand -hex 32", "baby_tracker_live_activity_app", "@db:5432/babytracker?sslmode=disable",
		"openssl pkey", "prime256v1",
		// The root proof is a fixed-string whole-line match, the key inspection
		// pipes only the public key, temporary files are cleaned on exit, and
		// no tool is required that the script does not call.
		`grep -qxF "BT_DEPLOY_ROOT=$deploy_root"`, `openssl pkey -in "$apns_key_source" -pubout`, `openssl pkey -pubin -noout -text`,
		"trap cleanup EXIT", "for tool in openssl mktemp; do", "self-naming",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("%s omits %q", liveActivitySecretsScript, required)
		}
	}
	for _, forbidden := range []string{"echo \"$password", "echo $password", "cat \"$secrets/live_activity_password\"", "rm -rf", "docker",
		`openssl pkey -in "$apns_key_source" -noout -text`, "for tool in openssl install mktemp"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("%s contains %q", liveActivitySecretsScript, forbidden)
		}
	}
}
