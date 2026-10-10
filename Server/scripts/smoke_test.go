package scripts

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	// PostgreSQL is the only security authority: no ledger directories, no
	// projector credential, no state volume.
	for _, serviceName := range []string{"app", "migrate"} {
		service := compose.Services[serviceName]
		for _, retired := range []string{"BT_SECURITY_LEDGER_DIR", "BT_LEDGER_HEAD_DIR", "BT_PROJECTOR_DATABASE_URL_FILE", "BT_DB_PROJECTOR_MAX_CONNS"} {
			if _, ok := service.Environment[retired]; ok {
				t.Errorf("%s still sets retired %s", serviceName, retired)
			}
		}
		if containsString(service.Secrets, "projector_database_url") {
			t.Errorf("%s still mounts the projector credential", serviceName)
		}
		for _, volume := range service.Volumes {
			if strings.Contains(volume, "ledger") || strings.Contains(volume, "/var/lib/babytracker") {
				t.Errorf("%s still mounts security state %q", serviceName, volume)
			}
		}
	}
	// Nor any credential of the projector or Apple-revoker roles the baseline
	// dropped (Phase 4 Task 27), on any service.
	for _, retired := range retiredRoleSecrets {
		if _, ok := compose.Secrets[retired]; ok {
			t.Errorf("compose still declares the retired %s", retired)
		}
		for serviceName, service := range compose.Services {
			if containsString(service.Secrets, retired) {
				t.Errorf("%s still mounts the retired %s", serviceName, retired)
			}
		}
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
  if [[ $* == *'authority verify'* ]]; then
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
grep -Fx 'run --rm --no-deps migrate authority verify' "$calls" >/dev/null
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

// Phase 3 Task 19: the smoke runs with push off (no BT_APNS_* in the smoke
// or its compose file, and no push-token-v1 in the server-info fixture it
// compares /v1/server against), and pins that the push-token route is then
// not registered: an unauthenticated PUT is 404 (with push on it would be
// 401). The check sits after the challenge check.
func TestSmokePinsThePushTokenRouteAbsentWithPushOff(t *testing.T) {
	script, err := os.ReadFile("smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	const pin = `[[ $(curl --silent --max-time 5 -o /dev/null -w '%{http_code}' -X PUT "$base/v1/devices/push-token") == 404 ]]`
	if n := strings.Count(text, pin); n != 1 {
		t.Fatalf("smoke.sh holds %d copies of the push-off route check, want 1", n)
	}
	challenge := strings.Index(text, `"$base/v1/auth/challenge")`)
	if challenge < 0 || strings.Index(text, pin) < challenge {
		t.Fatal("the push-off route check must follow the challenge check")
	}
	compose, err := os.ReadFile("../compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile("../api/fixtures/server-info.json")
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"smoke.sh": strings.ReplaceAll(text, "(no BT_APNS_*)", ""), "compose.yaml": string(compose)} {
		if strings.Contains(body, "BT_APNS_") {
			t.Errorf("%s configures APNs; the smoke must run with push off", name)
		}
	}
	if strings.Contains(string(fixture), "push-token-v1") {
		t.Error("server-info.json advertises push-token-v1; the push-off smoke compares /v1/server against it")
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
		`jq -r '.known[0].version' Server/internal/migrate/compatibility.json`,
		`jq -r '.known[0].name' Server/internal/migrate/compatibility.json`,
		`$baseline_name =~ ^[0-9]{6}_baseline\.sql$`,
		"SELECT count(*)=current_version-$baseline_version+1 AND min(version)=$baseline_version AND max(version)=current_version AND bool_or(version=$baseline_version AND name='$baseline_name')",
		"schema_version=$expected_schema_version",
		"baseline schema $expected_schema_version",
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
		"min(version)=1",
		"contiguous schema",
	} {
		if strings.Contains(text, stale) {
			t.Errorf("smoke retains brittle schema assertion %q", stale)
		}
	}
}

// retiredRoles and retiredRoleSecrets are the projector and Apple-revoker
// roles, logins and credentials the baseline dropped (Phase 4 Task 27).
var (
	retiredRoles       = []string{"baby_tracker_projector", "baby_tracker_projector_app", "baby_tracker_apple_revoker", "baby_tracker_apple_revoker_app"}
	retiredRoleSecrets = []string{"projector_password", "projector_database_url", "apple_revocation_password"}
)

// The deployment creates exactly five roles: the smoke and both principal
// probe modes check the exact set, and no deployment script names a retired
// role or credential.
func TestDeploymentProvesTheRetiredRolesAbsent(t *testing.T) {
	const exact = `string_agg(rolname, ',' ORDER BY rolname) FROM pg_roles WHERE rolname LIKE 'baby\_tracker\_%'`
	const roles = "baby_tracker_app,baby_tracker_export_app,baby_tracker_maintenance,baby_tracker_migrator,baby_tracker_runtime"
	for name, want := range map[string]string{
		"smoke.sh":            exact + `") == ` + roles + " ]]",
		"check-principals.sh": exact + `")" = ` + roles + "\n",
	} {
		encoded, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), want) {
			t.Errorf("%s does not prove the exact role set %q", name, want)
		}
	}
	for _, name := range []string{"smoke.sh", "check-principals.sh", "provision-unraid-secrets.sh", "test-db.sh", "lib/disposable-sync.sh",
		"refresh-recovery-phases.sh", "sync-performance.sh", "../deploy/postgres/init-roles.sql", "../deploy/postgres/init-users.sh", "../compose.yaml"} {
		encoded, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, retired := range append(append([]string(nil), retiredRoles...), retiredRoleSecrets...) {
			if strings.Contains(string(encoded), retired) {
				t.Errorf("%s still names the retired %s", name, retired)
			}
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

// Schema 30: both the principal probe (in each mode) and the smoke assert
// the care grants, and neither expects a legacy care table's grants; the
// smoke asserts the wake queue and its scheduler are gone.
func TestPrincipalProbeAndSmokeAssertTheCareGrants(t *testing.T) {
	principal, err := os.ReadFile("check-principals.sh")
	if err != nil {
		t.Fatal(err)
	}
	smoke, err := os.ReadFile("smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(principal), "unnest(ARRAY['public.care_entities','public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions'])"); n != 8 {
		t.Fatalf("principal probe asserts the care tables %d times, want the runtime's table and column checks and the other principals' table and column checks in both modes", n)
	}
	// has_table_privilege sees only table-level grants: a column-level
	// UPDATE on a receipt, binding or session table, any column grant to
	// PUBLIC, or an UPDATE on a care_entities
	// column outside the named list must fail both probes. (The maintenance
	// group's schema-31 grants: TestPrincipalProbeAndSmokeAssertTheMaintenanceGrants.)
	// Each check is a live (uncommented) line, once per principal-probe mode.
	allowlist := "baby_id,entity_version,changed_seq,actor_account_id,device_id,operation,accepted_at,tombstoned_at,started_at,ended_at,session_id," +
		"completed_record_id,sort_order,label,name_hash,breast_seconds,bottle_volume_ml,bottle_observations,bottle_missing,shortest_part_seconds,wire,feed_kind"
	others := regexp.QuoteMeta("unnest(ARRAY['public']) r")
	for _, check := range []struct {
		script, text string
		pattern      string
		want         int
	}{
		{"check-principals.sh", string(principal), `(?m)^\s+test "\$\(sql "SELECT NOT bool_or\(has_any_column_privilege\('baby_tracker_runtime',t,'UPDATE'\)\) FROM unnest\(ARRAY\['public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions'\]\) t"\)" = t$`, 2},
		{"check-principals.sh", string(principal), `(?m)^\s+test "\$\(sql "SELECT NOT bool_or\(has_any_column_privilege\('baby_tracker_runtime',t,'REFERENCES'\)\) FROM unnest\(ARRAY\['public.care_entities',`, 2},
		{"check-principals.sh", string(principal), `(?m)^\s+test "\$\(sql "SELECT string_agg\(attname, ',' ORDER BY attnum\) FROM pg_attribute WHERE attrelid='public.care_entities'::regclass AND attnum > 0 AND NOT attisdropped AND has_column_privilege\('baby_tracker_runtime',attrelid,attnum,'UPDATE'\)"\)" = ` + regexp.QuoteMeta(allowlist) + `$`, 2},
		{"check-principals.sh", string(principal), `(?m)^\s+test "\$\(sql "SELECT NOT bool_or\(has_table_privilege\(r,t,p\)\) FROM ` + others + ` CROSS JOIN unnest\(ARRAY\['public.care_entities',`, 2},
		{"check-principals.sh", string(principal), `(?m)^\s+test "\$\(sql "SELECT NOT bool_or\(has_any_column_privilege\(r,t,p\)\) FROM ` + others + ` CROSS JOIN unnest\(ARRAY\['public.care_entities',.*CROSS JOIN unnest\(ARRAY\['SELECT','INSERT','UPDATE','REFERENCES'\]\) p"\)" = t$`, 2},
		{"smoke.sh", string(smoke), `(?m)^\s+\[\[ \$\(docker exec "\$db" psql -XAt -U baby_tracker_app -d babytracker -c "SELECT NOT bool_or\(has_any_column_privilege\(current_user,t,'UPDATE'\)\) FROM unnest\(ARRAY\['care_receipts','sync_page_bindings','sync_snapshot_sessions'\]\) t"\) == t \]\]$`, 1},
		{"smoke.sh", string(smoke), `(?m)^\s+\[\[ \$\(docker exec "\$db" psql -XAt -U baby_tracker_app -d babytracker -c "SELECT string_agg\(attname, ',' ORDER BY attnum\) FROM pg_attribute WHERE attrelid='care_entities'::regclass .*has_column_privilege\(current_user,attrelid,attnum,'UPDATE'\)"\) == ` + regexp.QuoteMeta(allowlist) + ` \]\]$`, 1},
		{"smoke.sh", string(smoke), `(?m)^\s+\[\[ \$\(docker exec "\$db" psql -XAt -U postgres -d babytracker -c "SELECT NOT bool_or\(has_table_privilege\(r,t,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER'\) OR has_any_column_privilege\(r,t,'SELECT,INSERT,UPDATE,REFERENCES'\)\) FROM unnest\(ARRAY\['public'\]\) r CROSS JOIN unnest\(ARRAY\['care_entities',`, 1},
	} {
		if n := len(regexp.MustCompile(check.pattern).FindAllString(check.text, -1)); n != check.want {
			t.Fatalf("%s has %d live lines matching %s, want %d", check.script, n, check.pattern, check.want)
		}
	}
	// has_table_privilege with a privilege list is true when any one is held,
	// so each required grant is checked on its own: a lost INSERT must fail.
	if n := strings.Count(string(principal), "has_table_privilege('baby_tracker_runtime',t,'SELECT') AND has_table_privilege('baby_tracker_runtime',t,'INSERT') AND NOT"); n != 2 {
		t.Fatalf("principal probe checks the runtime's SELECT and INSERT separately %d times, want once per mode", n)
	}
	if strings.Contains(string(principal), "'SELECT,INSERT') AND NOT has_table_privilege('baby_tracker_runtime',t,'UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')) FROM unnest(ARRAY['public.care_entities'") {
		t.Fatal("principal probe checks the care tables' SELECT and INSERT as one either-or privilege list")
	}
	for _, required := range []string{"expected_schema_version >= 30", "unnest(ARRAY['care_entities','care_receipts','sync_page_bindings','sync_snapshot_sessions'])",
		"has_table_privilege(current_user,t,'SELECT') AND has_table_privilege(current_user,t,'INSERT') AND NOT",
		"to_regclass('public.delivery_pending') IS NULL", "p.proname='schedule_family_wakes')"} {
		if !strings.Contains(string(smoke), required) {
			t.Fatalf("smoke omits %q", required)
		}
	}
	for _, retired := range []string{"'public.babies'", "'public.mutation_journal'", "'public.sync_snapshot_entities'", "'public.active_timers'", "'babies'", "'mutation_journal'", "'family_record_usage'"} {
		if strings.Contains(string(principal), retired) || strings.Contains(string(smoke), retired) {
			t.Fatalf("a schema-30 principal check still expects the legacy %s", retired)
		}
	}
}

// Schema 31: both principal-probe modes and the smoke assert the maintenance
// group's retention, purge, wake and audit-partition grants (each required
// privilege on its own; the exact counter, care-key and import-key column
// lists, so no care payload; nothing on the push-token table; the four
// definers maintenance-only), and that the database rate limits are gone. No schema-30 check still lists the maintenance group among the
// principals with no care access.
func TestPrincipalProbeAndSmokeAssertTheMaintenanceGrants(t *testing.T) {
	principal, err := os.ReadFile("check-principals.sh")
	if err != nil {
		t.Fatal(err)
	}
	smoke, err := os.ReadFile("smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		script, text, required string
		want                   int
	}{
		{"check-principals.sh", string(principal), `if [ "$(sql "SELECT current_version >= 31 FROM schema_compatibility WHERE singleton")" = t ]; then`, 2},
		{"check-principals.sh", string(principal), "has_table_privilege('baby_tracker_maintenance',t,'DELETE') AND NOT has_table_privilege('baby_tracker_maintenance',t,'INSERT') AND NOT has_table_privilege('baby_tracker_maintenance',t,'UPDATE') AND NOT has_table_privilege('baby_tracker_maintenance',t,'TRUNCATE')) FROM unnest(ARRAY['public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions'])", 2},
		// Care rows and import staging: no DELETE of the maintenance group's
		// own, only app.purge_family_step (Task 11, Task 2 re-review m1).
		{"check-principals.sh", string(principal), "SELECT NOT has_table_privilege('baby_tracker_maintenance','public.care_entities','INSERT,UPDATE,DELETE,TRUNCATE') AND NOT has_table_privilege('baby_tracker_maintenance','public.import_sessions','INSERT,UPDATE,DELETE,TRUNCATE')", 2},
		{"check-principals.sh", string(principal), "has_table_privilege('baby_tracker_maintenance',t,'DELETE') AND NOT has_table_privilege('baby_tracker_maintenance',t,'INSERT') AND NOT has_table_privilege('baby_tracker_maintenance',t,'UPDATE') AND NOT has_table_privilege('baby_tracker_maintenance',t,'TRUNCATE')) FROM unnest(ARRAY['public.care_entities'", 0},
		{"check-principals.sh", string(principal), "SELECT NOT has_table_privilege('baby_tracker_maintenance','public.care_entities','SELECT') AND NOT has_table_privilege('baby_tracker_maintenance','public.import_sessions','SELECT')", 2},
		{"check-principals.sh", string(principal), "attrelid='public.care_entities'::regclass AND attnum > 0 AND NOT attisdropped AND has_column_privilege('baby_tracker_maintenance',attrelid,attnum,'SELECT')\")\" = family_id,id,kind\n", 2},
		{"check-principals.sh", string(principal), "attrelid='public.import_sessions'::regclass AND attnum > 0 AND NOT attisdropped AND has_column_privilege('baby_tracker_maintenance',attrelid,attnum,'SELECT')\")\" = family_id,import_id\n", 2},
		{"check-principals.sh", string(principal), "has_table_privilege('baby_tracker_maintenance',t,'SELECT') AND has_table_privilege('baby_tracker_maintenance',t,'DELETE')", 0},
		{"check-principals.sh", string(principal), "has_column_privilege('baby_tracker_maintenance',attrelid,attnum,'UPDATE')\")\" = care_record_count,care_charged_bytes,care_entity_count,care_wire_bytes\n", 2},
		{"check-principals.sh", string(principal), "has_column_privilege('baby_tracker_runtime',attrelid,attnum,'UPDATE')\")\" = environment,token,registered_at\n", 2},
		{"check-principals.sh", string(principal), "SELECT NOT has_table_privilege('baby_tracker_maintenance','public.device_push_tokens','SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') AND NOT has_any_column_privilege('baby_tracker_maintenance','public.device_push_tokens','SELECT,INSERT,UPDATE,REFERENCES')", 2},
		{"check-principals.sh", string(principal), "FROM unnest(ARRAY['app.push_targets(uuid[],uuid[])','app.prune_push_tokens(integer)','app.forget_push_token(uuid,bytea)','app.ensure_audit_partitions(timestamp with time zone)','app.purge_family_step(uuid,integer)']) f", 2},
		{"check-principals.sh", string(principal), "to_regclass('public.rate_limit_buckets') IS NULL", 2},
		{"check-principals.sh", string(principal), "'baby_tracker_maintenance']) r CROSS JOIN unnest(ARRAY['public.care_entities'", 0},
		{"smoke.sh", string(smoke), "if (( expected_schema_version >= 31 )); then", 1},
		{"smoke.sh", string(smoke), "has_table_privilege('baby_tracker_maintenance',t,'DELETE') AND NOT has_table_privilege('baby_tracker_maintenance',t,'INSERT,UPDATE,TRUNCATE')", 1},
		{"smoke.sh", string(smoke), "has_column_privilege('baby_tracker_maintenance',attrelid,attnum,'SELECT')\") == family_id,id,kind ]]", 1},
		{"smoke.sh", string(smoke), "has_column_privilege('baby_tracker_maintenance',attrelid,attnum,'UPDATE')\") == care_record_count,care_charged_bytes,care_entity_count,care_wire_bytes ]]", 1},
		{"smoke.sh", string(smoke), "SELECT NOT has_table_privilege('baby_tracker_maintenance','device_push_tokens','SELECT,INSERT,UPDATE,DELETE,TRUNCATE') AND NOT has_any_column_privilege('baby_tracker_maintenance','device_push_tokens','SELECT,INSERT,UPDATE,REFERENCES')", 1},
		{"smoke.sh", string(smoke), "app.forget_push_token(uuid,bytea)", 1},
		{"smoke.sh", string(smoke), "'app.ensure_audit_partitions(timestamp with time zone)','app.purge_family_step(uuid,integer)']) f", 1},
		{"smoke.sh", string(smoke), "FROM unnest(ARRAY['care_receipts','sync_page_bindings','sync_snapshot_sessions']) t\") == t ]]", 2},
		{"smoke.sh", string(smoke), "SELECT NOT bool_or(has_table_privilege('baby_tracker_maintenance',t,'INSERT,UPDATE,DELETE,TRUNCATE') OR has_any_column_privilege('baby_tracker_maintenance',t,'INSERT,UPDATE')) FROM unnest(ARRAY['care_entities','import_sessions']) t", 1},
		{"smoke.sh", string(smoke), "'DELETE') AND NOT has_table_privilege('baby_tracker_maintenance',t,'INSERT,UPDATE,TRUNCATE') AND NOT has_any_column_privilege('baby_tracker_maintenance',t,'INSERT,UPDATE')) FROM unnest(ARRAY['care_entities'", 0},
		{"smoke.sh", string(smoke), "to_regclass('public.rate_limit_buckets') IS NULL AND to_regprocedure('app.take_rate_limits(jsonb,timestamp with time zone)') IS NULL", 1},
		{"smoke.sh", string(smoke), "'baby_tracker_maintenance']) r CROSS JOIN unnest(ARRAY['care_entities'", 0},
	} {
		if n := strings.Count(check.text, check.required); n != check.want {
			t.Fatalf("%s holds %d copies of %q, want %d", check.script, n, check.required, check.want)
		}
	}
}

// Schema 32: both principal-probe modes and the smoke assert the Live Activity
// grants: the runtime's SELECT, INSERT and UPDATE on the two tenant tables
// (each on its own, never DELETE) and its five registration definers; the
// maintenance group's nine worker definers and nothing on any Live Activity
// table; the scheduling tables definer-only.
func TestPrincipalProbeAndSmokeAssertTheLiveActivityGrants(t *testing.T) {
	principal, err := os.ReadFile("check-principals.sh")
	if err != nil {
		t.Fatal(err)
	}
	smoke, err := os.ReadFile("smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	const runtimeDefiners = "'app.fence_live_activity_installation(uuid,bigint,uuid,uuid,uuid,uuid,bigint,timestamp with time zone,boolean)','app.live_activity_session_binding(uuid,uuid,uuid,bigint)','app.live_activity_claim_live(jsonb)','app.live_activity_registration_state(uuid,jsonb)','app.live_activity_authorized(uuid,bigint,boolean)']) f"
	const workerDefiners = "'app.claim_live_activity_work(timestamp with time zone,integer)','app.finish_live_activity_work(jsonb,timestamp with time zone,text)','app.retain_live_activity_work()','app.live_activity_target(jsonb,jsonb,boolean,timestamp with time zone,jsonb,jsonb)','app.abandon_live_activity_start(jsonb,text,text,boolean,boolean)','app.release_stale_live_activity_lease(jsonb)','app.dirty_live_activity_families(uuid[],bigint[])','app.sweep_live_activity_work()','app.sweep_live_activity_registrations(boolean,integer)']) f"
	for _, check := range []struct {
		script, text, required string
		want                   int
	}{
		{"check-principals.sh", string(principal), `if [ "$(sql "SELECT current_version >= 32 FROM schema_compatibility WHERE singleton")" = t ]; then`, 2},
		{"check-principals.sh", string(principal), "has_table_privilege('baby_tracker_runtime',t,'SELECT') AND has_table_privilege('baby_tracker_runtime',t,'INSERT') AND has_table_privilege('baby_tracker_runtime',t,'UPDATE') AND NOT has_table_privilege('baby_tracker_runtime',t,'DELETE') AND NOT has_table_privilege('baby_tracker_runtime',t,'TRUNCATE,REFERENCES,TRIGGER')) FROM unnest(ARRAY['public.live_activity_devices','public.live_activity_tokens'])", 2},
		{"check-principals.sh", string(principal), "unnest(ARRAY['public','baby_tracker_maintenance']) r CROSS JOIN unnest(ARRAY['public.live_activity_devices','public.live_activity_tokens']) t", 2},
		{"check-principals.sh", string(principal), "unnest(ARRAY['public','baby_tracker_runtime','baby_tracker_maintenance']) r CROSS JOIN unnest(ARRAY['public.live_activity_token_owners','public.live_activity_registration_index','public.live_activity_family_dirty','public.live_activity_delivery','public.live_activity_installations','public.live_activity_history']) t", 2},
		{"check-principals.sh", string(principal), "has_function_privilege('baby_tracker_runtime',f,'EXECUTE') AND NOT has_function_privilege('public',f,'EXECUTE') AND NOT has_function_privilege('baby_tracker_maintenance',f,'EXECUTE')) FROM unnest(ARRAY[" + runtimeDefiners, 2},
		{"check-principals.sh", string(principal), "has_function_privilege('baby_tracker_maintenance',f,'EXECUTE') AND NOT has_function_privilege('public',f,'EXECUTE') AND NOT has_function_privilege('baby_tracker_runtime',f,'EXECUTE')) FROM unnest(ARRAY[" + workerDefiners, 2},
		{"check-principals.sh", string(principal), "'app.live_activity_lineage_live(uuid,uuid,uuid,bigint,timestamp with time zone)','app.dirty_live_activity_family(uuid,bigint)','app.retire_stale_live_activity_registrations()','app.live_activity_token_ownership()','app.live_activity_registration_dirty()','app.live_activity_token_fingerprint()']) f", 2},
		{"check-principals.sh", string(principal), "to_regprocedure('app.live_activity_journal_dirty()') IS NULL AND to_regprocedure('app.live_activity_registration_subjects(uuid,integer)') IS NULL", 2},
		{"check-principals.sh", string(principal), "baby_tracker_live_activity", 0},
		{"smoke.sh", string(smoke), "if (( expected_schema_version >= 32 )); then", 1},
		{"smoke.sh", string(smoke), "has_table_privilege(current_user,t,'SELECT') AND has_table_privilege(current_user,t,'INSERT') AND has_table_privilege(current_user,t,'UPDATE') AND NOT has_table_privilege(current_user,t,'DELETE') AND NOT has_table_privilege(current_user,t,'TRUNCATE')) FROM unnest(ARRAY['live_activity_devices','live_activity_tokens'])", 1},
		{"smoke.sh", string(smoke), "unnest(ARRAY['public','baby_tracker_maintenance']) r CROSS JOIN unnest(ARRAY['live_activity_devices','live_activity_tokens','live_activity_token_owners','live_activity_registration_index','live_activity_family_dirty','live_activity_delivery','live_activity_installations','live_activity_history']) t", 1},
		{"smoke.sh", string(smoke), "has_table_privilege('baby_tracker_runtime',t,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') OR has_any_column_privilege('baby_tracker_runtime',t,'SELECT,INSERT,UPDATE,REFERENCES')) FROM unnest(ARRAY['live_activity_token_owners','live_activity_registration_index','live_activity_family_dirty','live_activity_delivery','live_activity_installations','live_activity_history']) t", 1},
		{"smoke.sh", string(smoke), "has_function_privilege('baby_tracker_runtime',f,'EXECUTE') AND NOT has_function_privilege('public',f,'EXECUTE') AND NOT has_function_privilege('baby_tracker_maintenance',f,'EXECUTE')) FROM unnest(ARRAY[" + runtimeDefiners, 1},
		{"smoke.sh", string(smoke), "has_function_privilege('baby_tracker_maintenance',f,'EXECUTE') AND NOT has_function_privilege('public',f,'EXECUTE') AND NOT has_function_privilege('baby_tracker_runtime',f,'EXECUTE')) FROM unnest(ARRAY[" + workerDefiners, 1},
		{"smoke.sh", string(smoke), "baby_tracker_live_activity", 0},
	} {
		if n := strings.Count(check.text, check.required); n != check.want {
			t.Fatalf("%s holds %d copies of %q, want %d", check.script, n, check.required, check.want)
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
		"authority init", "authority verify", "authority rotate-epoch", "server_state", "actor_id_key_mismatch",
		"BT_SECURITY_LEDGER_DIR", "BT_PROJECTOR_DATABASE_URL_FILE", "manual_required",
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
	for _, retired := range append(append([]string(nil), retiredRoles...), retiredRoleSecrets...) {
		if strings.Contains(text, retired) {
			t.Errorf("README still documents the retired %s", retired)
		}
	}
	if strings.Contains(text, "Recovery fencing and the authoritative ledger/head protocol remain later work") {
		t.Fatal("README still claims implemented recovery fencing is future work")
	}
	for _, retired := range []string{"security init", "security reconcile", "security verify", "recovery rotate-epoch"} {
		if strings.Contains(text, retired) {
			t.Errorf("README still documents the retired %q", retired)
		}
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
