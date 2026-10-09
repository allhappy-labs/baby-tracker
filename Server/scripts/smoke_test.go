package scripts

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestComposeWiresActorIDPepperAndInvitationKeyring(t *testing.T) {
	encoded, err := os.ReadFile("../compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var compose struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
			Secrets     []string          `yaml:"secrets"`
			Volumes     []string          `yaml:"volumes"`
		} `yaml:"services"`
		Secrets map[string]struct {
			File string `yaml:"file"`
		} `yaml:"secrets"`
	}
	if err := yaml.Unmarshal(encoded, &compose); err != nil {
		t.Fatal(err)
	}
	for _, serviceName := range []string{"app", "migrate"} {
		service, ok := compose.Services[serviceName]
		if !ok {
			t.Fatalf("compose service %q is missing", serviceName)
		}
		if service.Environment["BT_ACTOR_ID_PEPPER_FILE"] != "/run/secrets/actor_id_pepper" {
			t.Fatalf("%s actor-ID input=%q", serviceName, service.Environment["BT_ACTOR_ID_PEPPER_FILE"])
		}
		if !containsString(service.Secrets, "actor_id_pepper") {
			t.Fatalf("%s does not mount actor_id_pepper: %v", serviceName, service.Secrets)
		}
	}
	if got := compose.Secrets["actor_id_pepper"].File; got != "${BT_SECRETS_DIR:?set BT_SECRETS_DIR}/actor_id_pepper" {
		t.Fatalf("actor_id_pepper source=%q", got)
	}
	if got := compose.Secrets["invitation_pepper"].File; got != "${BT_SECRETS_DIR:?set BT_SECRETS_DIR}/invitation_pepper" {
		t.Fatalf("invitation_pepper source=%q", got)
	}
	if got := compose.Secrets["page_token_key"].File; got != "${BT_SECRETS_DIR:?set BT_SECRETS_DIR}/page_token_key" {
		t.Fatalf("page_token_key source=%q", got)
	}

	app := compose.Services["app"]
	for variable, want := range map[string]string{
		"BT_IDENTITY_KEY_ID":                      "${BT_IDENTITY_KEY_ID:-identity.v1}",
		"BT_IDENTITY_PEPPER_FILE":                 "/run/secrets/identity_pepper",
		"BT_SESSION_TOKEN_KEY_ID":                 "${BT_SESSION_TOKEN_KEY_ID:-session.v1}",
		"BT_SESSION_TOKEN_PEPPER_FILE":            "/run/secrets/session_token_pepper",
		"BT_DEVICE_CONTINUITY_KEY_ID":             "${BT_DEVICE_CONTINUITY_KEY_ID:-device.v1}",
		"BT_DEVICE_CONTINUITY_PEPPER_FILE":        "/run/secrets/device_continuity_pepper",
		"BT_APPLE_NOTIFICATION_KEY_ID":            "${BT_APPLE_NOTIFICATION_KEY_ID:-notification.v1}",
		"BT_APPLE_NOTIFICATION_PEPPER_FILE":       "/run/secrets/apple_notification_pepper",
		"BT_INVITATION_KEY_ID":                    "${BT_INVITATION_KEY_ID:-invitation.v1}",
		"BT_INVITATION_PEPPER_FILE":               "/run/secrets/invitation_pepper",
		"BT_PAGE_TOKEN_KEY_ID":                    "${BT_PAGE_TOKEN_KEY_ID:-page.v1}",
		"BT_PAGE_TOKEN_KEY_FILE":                  "/run/secrets/page_token_key",
		"BT_MAX_CAREGIVERS":                       "${BT_MAX_CAREGIVERS:-10}",
		"BT_MAX_INVITATIONS":                      "${BT_MAX_INVITATIONS:-10}",
		"BT_INVITATION_TTL":                       "${BT_INVITATION_TTL:-168h}",
		"BT_MAX_CANONICAL_RECORDS_PER_FAMILY":     "${BT_MAX_CANONICAL_RECORDS_PER_FAMILY:-100000}",
		"BT_MAX_CHARGED_STORAGE_BYTES_PER_FAMILY": "${BT_MAX_CHARGED_STORAGE_BYTES_PER_FAMILY:-268435456}",
	} {
		if got := app.Environment[variable]; got != want {
			t.Errorf("%s=%q, want %q", variable, got, want)
		}
	}
	for _, secret := range []string{"identity_pepper", "session_token_pepper", "device_continuity_pepper", "apple_notification_pepper", "invitation_pepper", "page_token_key", "actor_id_pepper", "rate_limit_pepper"} {
		if !containsString(app.Secrets, secret) {
			t.Errorf("app does not mount %s", secret)
		}
	}
	for _, variable := range []string{"BT_IDENTITY_PREVIOUS_KEYS_FILE", "BT_SESSION_TOKEN_PREVIOUS_KEYS_FILE", "BT_DEVICE_CONTINUITY_PREVIOUS_KEYS_FILE", "BT_APPLE_NOTIFICATION_PREVIOUS_KEYS_FILE", "BT_INVITATION_PREVIOUS_KEYS_FILE"} {
		if _, ok := app.Environment[variable]; !ok {
			t.Errorf("app does not forward %s", variable)
		}
	}
	if !containsString(app.Volumes, "${BT_KEYRING_DIR:-./deploy/keyrings}:/run/keyrings:ro") {
		t.Fatalf("app does not mount retained keyring directory: %v", app.Volumes)
	}
}

func TestSmokeRejectsActorIDPepperReplacement(t *testing.T) {
	directory := t.TempDir()
	actorPath := filepath.Join(directory, "actor_id_pepper")
	if err := os.WriteFile(actorPath, []byte(strings.Repeat("01", 32)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	harness := `set -euo pipefail
export BT_SMOKE_LIBRARY_ONLY=1
export BT_SECRETS_DIR=$TEST_SECRET_DIR
export BT_SERVER_INSTANCE_ID=123e4567-e89b-12d3-a456-426614174000
source ./smoke.sh
calls=$TEST_SECRET_DIR/calls
compose() {
  printf '%s\n' "$*" >> "$calls"
  if [[ $* == 'port app 8080' ]]; then
    echo 127.0.0.1:31999
    return 0
  fi
  if [[ $* == *'security verify'* ]]; then
    echo actor_id_key_mismatch
    return 1
  fi
}

request() {
  if cmp -s "$BT_SECRETS_DIR/actor_id_pepper" "$BT_SECRETS_DIR/actor_id_pepper.original"; then
    state=restored
    code=200
  else
    state=replacement
    code=503
  fi
  marker=$BT_SECRETS_DIR/request-$state
  if [[ ! -e $marker ]]; then
    : > "$marker"
    return 7
  fi
  printf %s "$code"
}
smoke_rejects_actor_id_pepper_replacement
cmp -s "$BT_SECRETS_DIR/actor_id_pepper" "$BT_SECRETS_DIR/actor_id_pepper.original"
grep -Fx 'up -d --force-recreate app' "$calls" >/dev/null
grep -Fx 'run --rm --no-deps migrate security verify' "$calls" >/dev/null
[[ $(grep -Fc 'up -d --force-recreate app' "$calls") == 3 ]]
[[ $(grep -Fc 'port app 8080' "$calls") == 3 ]]
[[ $base == http://127.0.0.1:31999 ]]
`
	command := exec.Command("bash", "-c", harness)
	command.Dir = "."
	command.Env = append(os.Environ(), "TEST_SECRET_DIR="+directory)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("actor-pepper smoke contract failed: %v: %s", err, output)
	}
}

func TestReadinessPollingUsesShortPerRequestDeadline(t *testing.T) {
	script, err := os.ReadFile("smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, required := range []string{"request /readyz 0.2", `--connect-timeout "$timeout"`, `--max-time "$timeout"`} {
		if !strings.Contains(text, required) {
			t.Fatalf("smoke readiness deadline omitted %q", required)
		}
	}
}

func TestSmokeDerivesSchemaInventoryFromCompatibilityManifest(t *testing.T) {
	script, err := os.ReadFile("smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, required := range []string{
		`jq -r '.max' Server/internal/migrate/compatibility.json`,
		"SELECT current_version FROM schema_compatibility WHERE singleton",
		"SELECT count(*)=current_version AND min(version)=1 AND max(version)=current_version",
		"schema_version=$expected_schema_version",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("smoke omits compatibility-derived schema assertion %q", required)
		}
	}
	for _, stale := range []string{
		`tableowner = 'baby_tracker_migrator'") == 28`,
		`SELECT count(*) FROM schema_migrations') == 6`,
		"schema_version=6",
		"exact v6",
	} {
		if strings.Contains(text, stale) {
			t.Errorf("smoke retains brittle schema assertion %q", stale)
		}
	}
}

func TestSelfHostedSmokeCoversPasswordlessFourthPrincipal(t *testing.T) {
	script, err := os.ReadFile("smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, required := range []string{
		"baby_tracker_apple_revoker_app",
		"baby_tracker_apple_revoker",
		"rolpassword IS NULL",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("smoke omits self-hosted revoker contract %q", required)
		}
	}
}

func TestSmokeUsesPositivePrincipalProbeAndReadinessForDrift(t *testing.T) {
	script, err := os.ReadFile("smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, required := range []string{
		"BT_PRINCIPAL_NEGATIVE_PROBES=0", "CREATE ROLE smoke_rogue", "GRANT REFERENCES (id)",
		"wait_for_ready_status 503", "wait_for_ready_status 200",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("smoke omits crash-safe privilege drift proof %q", required)
		}
	}
	if strings.Contains(text, `if docker exec "$db" /docker-entrypoint-initdb.d/20-init-users.sh`) {
		t.Fatal("smoke still triggers the PostgreSQL SIGPIPE-prone failing bootstrap heredoc")
	}
	principalScript, err := os.ReadFile("check-principals.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"BT_PRINCIPAL_NEGATIVE_PROBES", "positive role and exact current privilege contract verified"} {
		if !strings.Contains(string(principalScript), required) {
			t.Errorf("principal probe omits safe-mode contract %q", required)
		}
	}
}

func TestPrincipalNegativeProbesDoNotWriteIntentionalErrorsToServerLog(t *testing.T) {
	script, err := os.ReadFile("check-principals.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	if !strings.Contains(text, "log_min_messages=panic") {
		t.Fatal("principal negative probes must suppress their intentional ERROR messages from the PostgreSQL server log")
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// Execute the smoke script's actual EXIT handler with only Docker replaced.
// A failed removal, retained exact tag, or failed resource cleanup must never
// turn a successful smoke run green; an existing work failure keeps its status.
func TestSmokeCleanupChecksImageAndPreservesFailure(t *testing.T) {
	source, err := os.ReadFile("smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	_, body, found := strings.Cut(string(source), "cleanup() {")
	if !found {
		t.Fatal("cleanup entry point missing")
	}
	body, _, found = strings.Cut(body, "trap cleanup EXIT")
	if !found {
		t.Fatal("cleanup exit trap missing")
	}
	for _, tt := range []struct {
		name, state, removal string
		initial, want        int
		composeFails         bool
	}{
		{"removed", "present", "success", 0, 0, false},
		{"never created", "absent", "fail", 0, 0, false},
		{"removal fails", "present", "fail", 0, 1, false},
		{"tag remains", "present", "retain", 0, 1, false},
		{"preserve work failure on image error", "present", "fail", 7, 7, false},
		{"preserve work failure on compose error", "absent", "fail", 7, 7, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			secretDir := filepath.Join(t.TempDir(), "secrets")
			if err := os.Mkdir(secretDir, 0700); err != nil {
				t.Fatal(err)
			}
			harness := `set -euo pipefail
project=cleanup-test-exact
BT_IMAGE_NAME=cleanup-test-exact:local
image_state=$TEST_IMAGE_STATE
compose() { [[ $TEST_COMPOSE_FAILS == false ]]; }
docker() {
  case "$*" in
    'ps -aq --filter label=com.docker.compose.project=cleanup-test-exact'|'volume ls -q --filter label=com.docker.compose.project=cleanup-test-exact') return 0 ;;
    'image ls --quiet --filter reference=cleanup-test-exact:local')
      if [[ $image_state == present ]]; then echo sha256:exact-test-image; fi ;;
    'image rm cleanup-test-exact:local')
      [[ $image_state == present ]] || return 1
      [[ $TEST_REMOVAL != fail ]] || return 1
      if [[ $TEST_REMOVAL == success ]]; then image_state=absent; fi ;;
    *) echo "unexpected docker arguments: $*" >&2; return 99 ;;
  esac
}
cleanup() {` + body + "trap cleanup EXIT\nexit " + fmt.Sprint(tt.initial) + "\n"
			command := exec.Command("bash", "-c", harness)
			command.Env = append(os.Environ(), "BT_SECRETS_DIR="+secretDir, "TEST_IMAGE_STATE="+tt.state,
				"TEST_REMOVAL="+tt.removal, fmt.Sprintf("TEST_COMPOSE_FAILS=%t", tt.composeFails))
			output, err := command.CombinedOutput()
			status := 0
			if err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					status = exit.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if status != tt.want {
				t.Fatalf("cleanup status=%d, want %d; output=%s", status, tt.want, output)
			}
			if _, err := os.Stat(secretDir); !os.IsNotExist(err) {
				t.Fatalf("temporary secret directory remains: %v", err)
			}
		})
	}
}

func TestREADMETracksComposeRecoveryAndRoleContract(t *testing.T) {
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(readme)
	for _, required := range []string{
		"projector_password", "projector_database_url", "baby_tracker_projector_app",
		"baby_tracker_apple_revoker", "baby_tracker_apple_revoker_app", "passwordless",
		"security init", "security reconcile", "BT_DATABASE_URL_FILE=/run/secrets/runtime_database_url",
		"/readyz", "503", "200", "baby_tracker_migrator", "baby_tracker_app",
		"identity_pepper", "invitation_pepper", "BT_INVITATION_PREVIOUS_KEYS_FILE",
		"BT_MAX_CAREGIVERS", "BT_MAX_INVITATIONS", "BT_INVITATION_TTL",
		"rate_limit_pepper", "POST /v1/auth/challenge", "identity-v1", "family-lifecycle-v1", "recovery-v1",
		"POST /v1/account/delete", "POST /v1/family/delete", "GET /v1/family/recovery",
		"POST /v1/family/recover", "POST /v1/family/recovery/forfeit",
		"BT_APPLE_REVOCATION_DATABASE_URL_FILE", "BT_APPLE_REVOCATION_WORKER_ID",
		"BT_APPLE_REVOCATION_POLL_INTERVAL", "BT_APPLE_REVOCATION_LEASE_TTL", "BT_CREDENTIAL_ORPHAN_GRACE",
		"hosted_adapter_unavailable", "hosted_credential_rotation_unverified",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("README missing current deployment contract %q", required)
		}
	}
	if strings.Contains(text, "Recovery fencing and the authoritative ledger/head protocol remain later work") {
		t.Fatal("README still claims implemented recovery fencing is future work")
	}
}

func TestBootstrapUsesVersionedExactPrivilegeSnapshots(t *testing.T) {
	bootstrap, err := os.ReadFile("../deploy/postgres/init-users.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(bootstrap)
	for _, required := range []string{
		"schema_privilege_manifest", "schema_version=compatible_version", "observed_privileges",
		"'column'", "has_column_privilege", "manifest_absent", "effective privileges do not match",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("bootstrap missing versioned privilege contract %q", required)
		}
	}
	if !strings.Contains(text, "manifest_absent AND NOT") {
		t.Fatal("pre-v4 bootstrap no longer fails closed on group ACL drift")
	}
}
