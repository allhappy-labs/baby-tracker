//go:build identitytest

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/config"
	"babytracker/server/internal/loadseed"
	"babytracker/server/internal/loadtest"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5"
)

// writeSecrets writes a load stack's secrets directory as
// disposable-sync.sh does: 64-hex peppers and the database URLs.
func writeSecrets(t *testing.T, runtimeURL, maintenanceURL, superuserURL string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "secrets")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"identity_pepper", "session_token_pepper", "device_continuity_pepper", "apple_notification_pepper",
		"invitation_pepper", "page_token_key", "actor_id_pepper", "rate_limit_pepper"} {
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(hex.EncodeToString(secret[:])+"\n"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range map[string]string{"runtime_database_url": runtimeURL, "maintenance_database_url": maintenanceURL,
		"superuser_database_url": superuserURL} {
		if value == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func environment(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

const instance = "70000000-0000-4000-8000-0000000a0d01"

// stackEnvironment is a load stack's environment (deploy/load): its
// instance, .test origin and com.example audience, with extra on top.
func stackEnvironment(extra map[string]string) func(string) string {
	values := map[string]string{"BT_SERVER_INSTANCE_ID": instance, "BT_PUBLIC_ORIGIN": "https://sync.load.test",
		"BT_APPLE_AUDIENCE": "com.example.babytracker.load"}
	for name, value := range extra {
		values[name] = value
	}
	return environment(values)
}

func TestConfigReadsTheStacksSecretsAsTheServerDoes(t *testing.T) {
	secrets := writeSecrets(t, "postgres://app:a@db:5432/babytracker", "postgres://export:b@db:5432/babytracker", "")
	cfg, err := loadConfig(secrets, stackEnvironment(map[string]string{"BT_APNS_TEAM_ID": "ignored", "BT_MCP_ENABLED": "true"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != config.ModeHosted || cfg.PublicOrigin.String() != "https://sync.load.test" || cfg.ServerInstanceID != instance ||
		cfg.Auth.SessionTokenKeys.Current.ID != "session.v1" || cfg.Auth.PageTokenKeys.Current.ID != "page.v1" || cfg.MCP.Enabled ||
		cfg.LiveActivity.Credentials.BundleID != loadseed.DefaultBundleID {
		t.Fatalf("config %+v", cfg)
	}
	raw, err := os.ReadFile(filepath.Join(secrets, "session_token_pepper"))
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(cfg.Auth.SessionTokenKeys.Current.Secret[:]) != strings.TrimSpace(string(raw)) {
		t.Fatal("the session token key is not the secret file's")
	}
	// The server's own environment (Server/compose.yaml) loads the same keys.
	server, err := config.Load(environment(map[string]string{"BT_MODE": "self_hosted", "BT_SERVER_INSTANCE_ID": instance, "BT_RECOVERY_EPOCH": "1",
		"BT_APPLE_AUDIENCE": "com.example.babytracker.load", "BT_PUBLIC_ORIGIN": "https://sync.load.test", "BT_TRUSTED_PROXY_CIDRS": "172.31.250.100/32",
		"BT_DATABASE_URL_FILE": filepath.Join(secrets, "runtime_database_url"), "BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE": filepath.Join(secrets, "maintenance_database_url"),
		"BT_IDENTITY_KEY_ID": "identity.v1", "BT_IDENTITY_PEPPER_FILE": filepath.Join(secrets, "identity_pepper"),
		"BT_SESSION_TOKEN_KEY_ID": "session.v1", "BT_SESSION_TOKEN_PEPPER_FILE": filepath.Join(secrets, "session_token_pepper"),
		"BT_DEVICE_CONTINUITY_KEY_ID": "device.v1", "BT_DEVICE_CONTINUITY_PEPPER_FILE": filepath.Join(secrets, "device_continuity_pepper"),
		"BT_APPLE_NOTIFICATION_KEY_ID": "notification.v1", "BT_APPLE_NOTIFICATION_PEPPER_FILE": filepath.Join(secrets, "apple_notification_pepper"),
		"BT_INVITATION_KEY_ID": "invitation.v1", "BT_INVITATION_PEPPER_FILE": filepath.Join(secrets, "invitation_pepper"),
		"BT_PAGE_TOKEN_KEY_ID": "page.v1", "BT_PAGE_TOKEN_KEY_FILE": filepath.Join(secrets, "page_token_key"),
		"BT_ACTOR_ID_PEPPER_FILE": filepath.Join(secrets, "actor_id_pepper"), "BT_RATE_LIMIT_PEPPER_FILE": filepath.Join(secrets, "rate_limit_pepper")}))
	if err != nil {
		t.Fatal(err)
	}
	server.Mode = config.ModeHosted
	if !sameAuth(server.Auth, cfg.Auth) {
		t.Fatal("the seeder's keys differ from the server's")
	}
}

func sameAuth(a, b config.AuthConfig) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

func TestSeedRefusesAStackThatIsNotDisposable(t *testing.T) {
	secrets := writeSecrets(t, "postgres://app:a@db:5432/babytracker", "postgres://export:b@db:5432/babytracker", "")
	if _, err := loadConfig(secrets, stackEnvironment(nil)); err != nil {
		t.Fatalf("refused the load stack's own environment: %v", err)
	}
	// Nothing set is refused: the guard checks what the stack names, never
	// a default of the seeder's.
	if _, err := loadConfig(secrets, environment(map[string]string{})); err == nil || !strings.Contains(err.Error(), "is not set") {
		t.Fatalf("seeded with no environment: %v", err)
	}
	for _, unset := range []string{"BT_PUBLIC_ORIGIN", "BT_APPLE_AUDIENCE", "BT_SERVER_INSTANCE_ID"} {
		if _, err := loadConfig(secrets, stackEnvironment(map[string]string{unset: ""})); err == nil || !strings.Contains(err.Error(), unset) {
			t.Fatalf("seeded with %s unset: %v", unset, err)
		}
	}
	if _, err := loadConfig(secrets, stackEnvironment(map[string]string{"BT_PUBLIC_ORIGIN": "https://sync.example.com"})); err == nil {
		t.Fatal("seeded a non-.test origin")
	}
	if _, err := loadConfig(secrets, stackEnvironment(map[string]string{"BT_APPLE_AUDIENCE": "com.example.babytracker"})); err == nil {
		t.Fatal("seeded a real Apple audience")
	}
	if err := os.Chmod(secrets, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(secrets, stackEnvironment(nil)); err == nil {
		t.Fatal("read secrets from a shared directory")
	}
}

func TestSeedFlags(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	_, spec, err := parseSeedFlags([]string{"--secrets", "/s", "--families", "100", "--seed", "3", "--history", "small", "--backlog-load", "1",
		"--fleet", "/p/fleet.jsonl", "--report", "/r.json", "--as-of", "2026-10-20T11:00:00Z"}, now)
	if err != nil || spec.Families != 100 || spec.Seed != 3 || spec.BacklogLoad != 1 || spec.Workers != 16 || len(spec.History.DaysWeights) != 1 ||
		!spec.AsOf.Equal(now.Add(-time.Hour)) {
		t.Fatalf("%+v %v", spec, err)
	}
	if _, spec, err = parseSeedFlags([]string{"--secrets", "/s", "--families", "1", "--fleet", "/p/f", "--report", "/r", "--as-of", "2026-10-19T12:01:00Z"}, now); err != nil {
		t.Fatalf("refused an as-of within a day: %v", err)
	}
	_, spec, err = parseSeedFlags([]string{"--secrets", "/s", "--families", "10000", "--fleet", "/p/f", "--report", "/r"}, now)
	if err != nil || spec.Families != 10000 || len(spec.History.DaysWeights) != 5 || !spec.AsOf.Equal(now) || spec.HotFamily {
		t.Fatalf("%+v %v", spec, err)
	}
	_, spec, err = parseSeedFlags([]string{"--secrets", "/s", "--families", "10000", "--fleet", "/p/f", "--report", "/r", "--hot-family"}, now)
	if err != nil || !spec.HotFamily || spec.SeededFamilies() != 10001 {
		t.Fatalf("--hot-family: %+v %v", spec, err)
	}
	for _, args := range [][]string{
		{"--families", "1", "--fleet", "/p/f", "--report", "/r"},
		{"--secrets", "/s", "--families", "0", "--fleet", "/p/f", "--report", "/r"},
		{"--secrets", "/s", "--families", "1", "--fleet", "/p/f", "--report", "/r", "--history", "huge"},
		{"--secrets", "/s", "--families", "1", "--fleet", "/p/f", "--report", "/r", "--as-of", "2027-01-01T00:00:00Z"},
		{"--secrets", "/s", "--families", "1", "--fleet", "/p/f", "--report", "/r", "--as-of", "2026-10-19T11:59:00Z"},
		{"--secrets", "/s", "--families", "1", "--fleet", "/p/f", "--report", "/r", "--workers", "0"},
	} {
		if _, _, err := parseSeedFlags(args, now); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

// The command end to end on a disposable database: an unmarked database is
// refused with nothing written; once marked as a load stack, the fleet is
// private and the report holds no credential.
func TestSeedCommandWritesAPrivateFleetAndAPublicReport(t *testing.T) {
	fixture := testpg.New(t)
	var database string
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	base, err := url.Parse(testpg.RequireDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	base.Path = "/" + database
	superuser := base.String()
	runtimeURL, maintenanceURL := fixture.Runtime.Config().ConnString(), fixture.Maintenance.Config().ConnString()
	secrets := writeSecrets(t, runtimeURL, maintenanceURL, superuser)
	env := stackEnvironment(nil)
	cfg, err := loadConfig(secrets, env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.InitServerState(t.Context(), fixture.Owner, instance, cfg.Auth.ActorIDPepper); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	fleetPath, reportPath := filepath.Join(private, "fleet.jsonl"), filepath.Join(t.TempDir(), "seed-report.json")
	args := []string{"--secrets", secrets, "--families", "3", "--seed", "5", "--history", "small", "--backlog-load", "1",
		"--workers", "2", "--fleet", fleetPath, "--report", reportPath}
	err = seedCommand(t.Context(), args, env, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "not marked as a disposable load stack") {
		t.Fatalf("seeded an unmarked database: %v", err)
	}
	if strings.Contains(err.Error(), "disposable-test-only") {
		t.Fatal("the refusal prints a database password")
	}
	var families int
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT count(*) FROM accounts`).Scan(&families); err != nil || families != 0 {
		t.Fatalf("a refused seed wrote %d accounts (%v)", families, err)
	}
	if _, err := os.Stat(fleetPath); !os.IsNotExist(err) {
		t.Fatal("a refused seed wrote a fleet")
	}
	// load-node.sh up marks the stack after migrating.
	if _, err := fixture.Admin.Exec(t.Context(), `COMMENT ON DATABASE `+pgx.Identifier{database}.Sanitize()+` IS '`+loadseed.StackMarker+`'`); err != nil {
		t.Fatal(err)
	}
	if err := seedCommand(t.Context(), args, env, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	header, fleet, err := loadtest.ReadFleet(fleetPath)
	if err != nil || header.Families != 3 || len(fleet) != 12 || header.ServerInstanceID != instance {
		t.Fatalf("fleet %+v (%d devices): %v", header, len(fleet), err)
	}
	report, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var decoded seedReport
	if err := json.Unmarshal(report, &decoded); err != nil || decoded.Families != 3 || decoded.SeedSpecSHA256 != header.SeedSpecSHA256 ||
		len(decoded.PhaseSeconds) != 5 {
		t.Fatalf("report %s: %v", report, err)
	}
	for _, d := range fleet {
		for _, secret := range []string{d.AccessToken, d.RefreshToken, d.DeviceCredential} {
			if strings.Contains(string(report), secret) {
				t.Fatal("the report holds a credential")
			}
		}
	}
	if strings.Contains(string(report), "disposable-test-only") {
		t.Fatal("the report holds a database password")
	}
}
