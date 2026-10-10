package scripts

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This exercises the init-only bootstrap in an isolated container. The
// secrets bind starts without the export (maintenance) credential, then the
// same bootstrap is rerun after that file appears. The bootstrap creates no
// projector or Apple-revoker role (Phase 4 Task 27). Nothing under the host's
// /run hierarchy is read or changed.
func TestBootstrapInstallsExportPasswordWhenSecretAppearsLater(t *testing.T) {
	if os.Getenv("BABY_TRACKER_REQUIRE_DATABASE") != "1" {
		t.Skip("set BABY_TRACKER_REQUIRE_DATABASE=1 to run container bootstrap integration")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Fatalf("docker is required for bootstrap integration: %v", err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	secretDir := t.TempDir()
	for name, value := range map[string]string{
		"migrator_password": "migrator-bootstrap-test",
		"runtime_password":  "runtime-bootstrap-test",
	} {
		if err := os.WriteFile(filepath.Join(secretDir, name), []byte(value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	container := fmt.Sprintf("baby-tracker-bootstrap-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		_ = exec.Command(docker, "rm", "--force", container).Run()
	})
	const image = "postgres:17.6-alpine@sha256:ef257d85f76e48da1c64832459b59fcaba1a4dac97bf5d7450c77753542eee94"
	run := exec.Command(docker, "run", "--detach", "--name", container,
		"--env", "POSTGRES_PASSWORD=bootstrap-superuser-test",
		"--env", "POSTGRES_DB=babytracker",
		"--mount", "type=bind,src="+filepath.Join(root, "deploy/postgres/init-roles.sql")+",dst=/docker-entrypoint-initdb.d/10-init-roles.sql,readonly",
		"--mount", "type=bind,src="+filepath.Join(root, "deploy/postgres/init-users.sh")+",dst=/docker-entrypoint-initdb.d/20-init-users.sh,readonly",
		"--mount", "type=bind,src="+secretDir+",dst=/run/secrets,readonly",
		image)
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("start disposable postgres: %v: %s", err, output)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		ready := exec.Command(docker, "exec", container, "pg_isready", "-U", "postgres", "-d", "babytracker")
		if ready.Run() == nil {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command(docker, "logs", container).CombinedOutput()
			t.Fatalf("disposable postgres did not become ready: %s", logs)
		}
		time.Sleep(100 * time.Millisecond)
	}
	for {
		logs, _ := exec.Command(docker, "logs", container).CombinedOutput()
		ready := exec.Command(docker, "exec", container, "pg_isready", "-U", "postgres", "-d", "babytracker").Run() == nil
		if ready && strings.Contains(string(logs), "PostgreSQL init process complete; ready for start up.") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("disposable postgres did not complete initialization: %s", logs)
		}
		time.Sleep(100 * time.Millisecond)
	}
	passwordIsNull := func() (string, error) {
		command := exec.Command(docker, "exec", container, "psql", "-XAt", "-U", "postgres", "-d", "babytracker", "-c",
			`SELECT rolpassword IS NULL FROM pg_authid WHERE rolname='baby_tracker_export_app'`)
		output, err := command.CombinedOutput()
		if err != nil {
			return string(output), fmt.Errorf("inspect export password: %w", err)
		}
		return strings.TrimSpace(string(output)), nil
	}
	var got string
	for {
		got, err = passwordIsNull()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command(docker, "logs", container).CombinedOutput()
			t.Fatalf("bootstrap did not finish: %v: %s\n%s", err, got, logs)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got != "t" {
		t.Fatalf("export password absent bootstrap=%q, want null", got)
	}
	retired := exec.Command(docker, "exec", container, "psql", "-XAt", "-U", "postgres", "-d", "babytracker", "-c",
		`SELECT count(*) FROM pg_roles WHERE rolname IN ('baby_tracker_projector','baby_tracker_projector_app','baby_tracker_apple_revoker','baby_tracker_apple_revoker_app')`)
	if output, err := retired.CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != "0" {
		t.Fatalf("the bootstrap created a retired role: %v: %s", err, output)
	}

	const laterPassword = "later-mounted-export-test"
	if err := os.WriteFile(filepath.Join(secretDir, "export_password"), []byte(laterPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rerun := exec.Command(docker, "exec", container, "/docker-entrypoint-initdb.d/20-init-users.sh")
	if output, err := rerun.CombinedOutput(); err != nil {
		t.Fatalf("rerun bootstrap with later secret: %v: %s", err, output)
	}
	got, err = passwordIsNull()
	if err != nil {
		t.Fatal(err)
	}
	if got != "f" {
		t.Fatalf("export password after rerun=%q, want installed", got)
	}
	authenticate := exec.Command(docker, "exec", "--env", "PGPASSWORD="+laterPassword, container,
		"psql", "-XAt", "-h", "127.0.0.1", "-U", "baby_tracker_export_app", "-d", "babytracker", "-c", "SELECT current_user")
	if output, err := authenticate.CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != "baby_tracker_export_app" {
		t.Fatalf("later export credential unusable: %v: %s", err, output)
	}
}
