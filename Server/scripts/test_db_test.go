package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDatabaseRunnerRejectsExternalDatabaseURL(t *testing.T) {
	command := exec.Command("bash", "./test-db.sh")
	command.Dir = "."
	command.Env = append(os.Environ(),
		"BABY_TRACKER_TEST_DATABASE_URL=postgres://production.example/babytracker",
		"BT_TEST_DB_LIBRARY_ONLY=0",
	)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("test-db accepted an externally supplied database reset target")
	}
	if !strings.Contains(string(output), "refusing external BABY_TRACKER_TEST_DATABASE_URL") {
		t.Fatalf("unexpected refusal: %v: %s", err, output)
	}
}

func TestDatabaseRunnerOwnsDisposableComposeProjectAndRequiredTestMode(t *testing.T) {
	script, err := os.ReadFile("test-db.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, required := range []string{
		`project="baby-tracker-test-`,
		`mktemp -d`,
		`umask 077`,
		`chmod 0600`,
		`docker compose --project-name "$project"`,
		`compose down --volumes --remove-orphans`,
		`BABY_TRACKER_REQUIRE_DATABASE=1`,
		`BABY_TRACKER_TEST_DATABASE_URL=`,
		`go test -p=1 -race -count=1`,
		`BT_TEST_RUN`,
		`case ${BT_TEST_RACE:-1} in`,
		`0) go_test=(go test -p=1 -count=1) ;;`,
		`go_test+=(-timeout "$BT_TEST_TIMEOUT")`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("test-db omits disposable integration-test contract %q", required)
		}
	}
	for _, forbidden := range []string{
		`docker volume prune`,
		`DROP DATABASE babytracker`,
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("test-db contains unsafe broad reset operation %q", forbidden)
		}
	}
}

func TestDatabaseRunnerProvesEveryRequiredRoleExistsInPostgresCatalog(t *testing.T) {
	script, err := os.ReadFile("test-db.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	if !strings.Contains(text, "bool_and(pg_roles.oid IS NOT NULL)") {
		t.Fatal("test-db role proof does not check that every pg_roles join matched")
	}
	if strings.Contains(text, "bool_and(rolname IS NOT NULL)") {
		t.Fatal("test-db role proof checks the non-null expected input instead of the joined pg_roles row")
	}
}

func TestDatabaseRunnerUsesCleanPinnedPostgresWithoutProductionAppMemberships(t *testing.T) {
	script, err := os.ReadFile("test-db.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, required := range []string{
		"postgres:17.6-alpine@sha256:ef257d85f76e48da1c64832459b59fcaba1a4dac97bf5d7450c77753542eee94",
		"NOT EXISTS",
		"baby_tracker_app",
		"pg_auth_members",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("clean test database contract omits %q", required)
		}
	}
	for _, forbidden := range []string{"Server/compose.yaml", "init-users.sh"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("test-db imports production bootstrap through %q", forbidden)
		}
	}
	membershipProof := strings.Index(text, "pg_auth_members")
	goTest := strings.Index(text, "go_test=(go test")
	if membershipProof < 0 || goTest < membershipProof {
		t.Fatalf("test-db must prove production app memberships absent before isolated tests: proof=%d go=%d", membershipProof, goTest)
	}
}

func TestMakeServerTestDelegatesToDisposableDatabaseRunner(t *testing.T) {
	makefile, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(makefile)
	start := strings.Index(text, "server-test:")
	if start < 0 {
		t.Fatal("server-test target is missing")
	}
	target := text[start:]
	if next := strings.Index(target, "\nserver-build:"); next >= 0 {
		target = target[:next]
	}
	if !strings.Contains(target, "Server/scripts/test-db.sh") {
		t.Fatalf("server-test does not delegate to test-db.sh: %s", target)
	}
	if strings.Contains(target, "go test") {
		t.Fatalf("server-test bypasses its disposable database runner: %s", target)
	}
}
