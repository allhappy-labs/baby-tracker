package scripts

import (
	"archive/tar"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const rehearsalScript = "rehearse-live-activity-upgrade.sh"

func writeRehearsalStub(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}

func runRehearsal(t *testing.T, env []string, args ...string) (int, string) {
	t.Helper()
	command := exec.Command("bash", append([]string{"./" + rehearsalScript}, args...)...)
	command.Dir = "."
	command.Env = append(os.Environ(), env...)
	output, err := command.CombinedOutput()
	status := 0
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("%s: %v: %s", rehearsalScript, err, output)
		}
		status = exit.ExitCode()
	}
	return status, string(output)
}

// Argument validation runs before any docker, git or go invocation, so a
// misuse costs nothing and cannot touch the engine.
func TestRehearsalRefusesMisuseBeforeTouchingDocker(t *testing.T) {
	directory := t.TempDir()
	stubDocker := filepath.Join(directory, "docker")
	writeRehearsalStub(t, stubDocker, "#!/bin/sh\necho 'docker must not run during argument validation' >&2\nexit 97\n")
	env := []string{"PATH=" + directory + string(os.PathListSeparator) + os.Getenv("PATH")}
	missing := filepath.Join(directory, "missing.dump")
	present := filepath.Join(directory, "present.dump")
	if err := os.WriteFile(present, []byte("PGDMP"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"no mode", nil, "usage:"},
		{"both modes", []string{"--synthetic", "--dump", present}, "either --dump or --synthetic"},
		{"missing dump", []string{"--dump", missing, "--ledger-tar", present, "--actor-id-pepper", present}, "not a readable file"},
		{"dump without ledger", []string{"--dump", present, "--actor-id-pepper", present}, "--ledger-tar"},
		{"dump without pepper", []string{"--dump", present, "--ledger-tar", present}, "--actor-id-pepper"},
		{"synthetic with ledger", []string{"--synthetic", "--ledger-tar", present}, "only with --dump"},
		{"synthetic with old image", []string{"--synthetic", "--old-image", "x:y"}, "only with --dump"},
		{"unknown flag", []string{"--synthetic", "--bogus"}, "usage:"},
		{"old image without tree on dump", []string{"--dump", present, "--ledger-tar", present, "--actor-id-pepper", present, "--old-tree", filepath.Join(directory, "absent")}, "old tree"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			status, output := runRehearsal(t, env, tt.args...)
			if status != 2 {
				t.Fatalf("status=%d, want 2; output=%s", status, output)
			}
			if !strings.Contains(output, tt.want) {
				t.Fatalf("output=%q lacks %q", output, tt.want)
			}
			if strings.Contains(output, "docker must not run") {
				t.Fatalf("argument validation invoked docker: %s", output)
			}
		})
	}
}

// The rehearsal is the runbook's R1 (and R2's database half) run against a
// disposable copy. Every gated step, in the runbook's own words, must be in
// the script, and nothing may name production or print a secret.
func TestRehearsalFollowsTheR1Procedure(t *testing.T) {
	script, err := os.ReadFile(rehearsalScript)
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, required := range []string{
		// Inputs: the cutover dump format and the old tree the runbook names.
		"pg_dump -Fc -U postgres -d babytracker",
		"pg_restore -U postgres -d babytracker --single-transaction --exit-on-error",
		"8032e7972cbc107a0576e4e104c781de81ea7ba5",
		`'{"min":27,"max":27}'`, `'{"min":29,"max":29}'`,
		// R1-6 step 2: recreate db on the new init scripts, rerun init-users, exact roles.
		"up -d --no-deps --force-recreate --wait db",
		"grep -q baby_tracker_live_activity /docker-entrypoint-initdb.d/10-init-roles.sql",
		"exec -T -u postgres db /docker-entrypoint-initdb.d/20-init-users.sh",
		"SELECT rolname, rolcanlogin, rolpassword IS NULL FROM pg_authid WHERE rolname IN ('baby_tracker_live_activity','baby_tracker_live_activity_app') ORDER BY rolname",
		"baby_tracker_live_activity f t;baby_tracker_live_activity_app t t;",
		// R1-6 step 3: migrate exactly as production, then the exact schema checks.
		"run --rm --no-deps migrate",
		"SELECT current_version FROM schema_compatibility WHERE singleton",
		"SELECT count(*)=current_version AND min(version)=1 AND max(version)=current_version FROM schema_migrations CROSS JOIN schema_compatibility WHERE singleton GROUP BY current_version",
		"SELECT sha256 FROM schema_migrations WHERE version = 28",
		`jq -r '.known[] | select(.version == 28) | .sha256'`,
		// Exact role and privilege inventories, reusing the tree's verifiers.
		"BT_PRINCIPAL_NEGATIVE_PROBES=0", "check-principals.sh",
		"effective privileges do not match",
		// Old image refusal and new image readiness with push off.
		"database_newer_than_binary: version 28",
		"index(\"live-activity-push-v1\") == null",
		"/readyz", "/v1/server",
		// R2's database half: password mounted, init-users rerun, preflight without Apple.
		"live_activity_password", "live_activity_database_url",
		"live-activity-preflight --environment sandbox --skip-apple",
		"[PASS] worker database",
		// Row counts and content digests before, after and at the end.
		"md5(coalesce(string_agg(t::text",
		// Cleanup proof.
		"down --volumes --remove-orphans",
		"CLEANUP: removed",
		"REHEARSAL: PASS",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("rehearsal omits %q", required)
		}
	}
	for _, forbidden := range []string{
		"baby-tracker-sync-db-1", "baby-tracker-sync-app-1", "--project-name baby-tracker-sync",
		"push.apple.com", "ssh ", "docker push",
		`echo "$bootstrap_password`, `echo "$migrator_password`, `echo "$live_activity_password`,
		"cat \"$secrets/", "BT_LIVE_ACTIVITY_PUSH_ENABLED: \"true\"",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("rehearsal contains %q", forbidden)
		}
	}
	recreate := strings.Index(text, "compose_new up -d --no-deps --force-recreate --wait db")
	migrate := strings.Index(text, "compose_new run --rm --no-deps migrate")
	refusal := strings.Index(text, "compose_old run --rm --no-deps migrate")
	start := strings.Index(text, "compose_new up -d --no-deps --force-recreate --wait --wait-timeout 120 app")
	if recreate < 0 || migrate < recreate {
		t.Error("db is not recreated on the new init scripts before migrate; R1-6 recreates it first")
	}
	if refusal < migrate {
		t.Error("the old image's refusal is checked before migrate ran")
	}
	if start < refusal {
		t.Error("the new app starts before the old image's refusal is proven")
	}
	seed, err := os.ReadFile("rehearse-live-activity-upgrade-seed.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"feed_entries", "feed_components", "feeding_session_id", "sleep_entries", "sleep_session_id", "diaper_entries", "active_timers"} {
		if !strings.Contains(string(seed), required) {
			t.Errorf("synthetic seed omits %q", required)
		}
	}
}

// Execute the rehearsal's actual EXIT handler with docker and compose
// replaced. A leftover container, volume, network, image or temporary
// directory must fail the run; a failed rehearsal keeps its own status.
func TestRehearsalCleanupProvesRemovalAndPreservesFailure(t *testing.T) {
	source, err := os.ReadFile(rehearsalScript)
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
		name          string
		leftover      string // resource kind that survives removal
		imageRemains  bool
		initial, want int
	}{
		{"everything removed", "", false, 0, 0},
		{"container remains", "container", false, 0, 1},
		{"volume remains", "volume", false, 0, 1},
		{"network remains", "network", false, 0, 1},
		{"image remains", "", true, 0, 1},
		{"failure preserved", "", false, 7, 7},
		{"failure preserved over leftovers", "volume", true, 7, 7},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			work := filepath.Join(root, "bt-rehearse.test0001")
			if err := os.MkdirAll(filepath.Join(work, "secrets"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(work, "secrets", "bootstrap_password"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			harness := `set -euo pipefail
project=rehearse-cleanup-test
work=$TEST_WORK
work_root=$TEST_WORK_ROOT
owned_images=(rehearse-cleanup-test-old:rehearsal rehearse-cleanup-test-new:rehearsal)
compose_down() { return 0; }
docker() {
  case "$*" in
    'ps -aq --filter label=com.docker.compose.project=rehearse-cleanup-test') [[ $TEST_LEFTOVER == container ]] && echo c0ffee; return 0 ;;
    'volume ls -q --filter label=com.docker.compose.project=rehearse-cleanup-test') [[ $TEST_LEFTOVER == volume ]] && echo rehearse-cleanup-test_database; return 0 ;;
    'network ls -q --filter label=com.docker.compose.project=rehearse-cleanup-test') [[ $TEST_LEFTOVER == network ]] && echo n3t; return 0 ;;
    'image ls -q --filter reference=rehearse-cleanup-test-old:rehearsal'|'image ls -q --filter reference=rehearse-cleanup-test-new:rehearsal') [[ $TEST_IMAGE_REMAINS == true ]] && echo sha256:1mage; return 0 ;;
    'rm -f '*|'volume rm '*|'network rm '*|'image rm '*) return 0 ;;
    *) echo "unexpected docker arguments: $*" >&2; return 99 ;;
  esac
}
cleanup() {` + body + "trap cleanup EXIT\nexit " + fmt.Sprint(tt.initial) + "\n"
			command := exec.Command("bash", "-c", harness)
			command.Env = append(os.Environ(), "TEST_WORK="+work, "TEST_WORK_ROOT="+root, "TEST_LEFTOVER="+tt.leftover,
				fmt.Sprintf("TEST_IMAGE_REMAINS=%t", tt.imageRemains))
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
			if _, err := os.Stat(work); !os.IsNotExist(err) {
				t.Fatalf("temporary directory remains: %v", err)
			}
			if tt.leftover == "" && !tt.imageRemains && !strings.Contains(string(output), "CLEANUP: removed") {
				t.Fatalf("clean run printed no cleanup proof: %s", output)
			}
			if (tt.leftover != "" || tt.imageRemains) && !strings.Contains(string(output), "CLEANUP: FAIL") {
				t.Fatalf("leftovers not reported: %s", output)
			}
		})
	}
}

// R1 must rehearse on the real backup before the outage, with the exact
// invocation, and stop on a failed rehearsal.
func TestRunbookR1RequiresTheRehearsal(t *testing.T) {
	runbook, err := os.ReadFile("../../docs/runbooks/family-live-activity-push.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(runbook)
	for _, required := range []string{
		"### R1-3b Rehearse the upgrade on the backup (STOP gate)",
		"rehearse-live-activity-upgrade.sh\" --dump", "--ledger-tar", "--actor-id-pepper", "--work-dir \"$BT_DEPLOY_ROOT\"",
		"--old-tree \"$src\"", "--old-image \"baby-tracker-server:$old_sha\"", "--new-tree \"$staged\"",
		"REHEARSAL: PASS", "STOP: rehearsal",
		"--synthetic",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("runbook omits %q", required)
		}
	}
	for _, stale := range []string{
		"**No rehearsal tool for this migration.**",
		"**No 27 → 28 dry-run tool:**",
	} {
		if strings.Contains(text, stale) {
			t.Errorf("runbook still claims %q", stale)
		}
	}
	rehearsal := strings.Index(text, "### R1-3b Rehearse")
	rollbackSet := strings.Index(text, "### R1-4 Rollback set")
	if rehearsal < 0 || rollbackSet < rehearsal {
		t.Error("the rehearsal is not gated before R1-4 changes production configuration")
	}
}

// The final comparison may excuse only what migration 28 writes and the one
// table the gate rewrites on reconcile (server_state, the recovery mirror);
// anything else changing is a care or security change and must fail.
func TestRehearsalFinalAllowlistIsExact(t *testing.T) {
	script, err := os.ReadFile(rehearsalScript)
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	const allowlist = `operational="public.schema_migrations public.schema_compatibility public.schema_privilege_manifest public.server_state"`
	if !strings.Contains(text, allowlist) {
		t.Fatalf("final allowlist is not exactly the migration tables plus server_state")
	}
	for _, excused := range []string{"security_projection_head", "delivery_pending", "rate_limit_buckets"} {
		if strings.Contains(text, excused) {
			t.Errorf("final comparison still excuses %q", excused)
		}
	}
	if !strings.Contains(text, "mirror.Update") {
		t.Error("the server_state exception does not name its cause (the gate's mirror.Update on reconcile)")
	}
}

// Every exported BT_* and COMPOSE_* variable is dropped before compose runs,
// not a fixed list: the runbook's exported BT_IMAGE_NAME hazard generalised.
func TestRehearsalIsolatesEveryComposeVariable(t *testing.T) {
	source, err := os.ReadFile(rehearsalScript)
	if err != nil {
		t.Fatal(err)
	}
	_, body, found := strings.Cut(string(source), "isolate_compose_environment() {")
	if !found {
		t.Fatal("isolate_compose_environment entry point missing")
	}
	body, _, found = strings.Cut(body, "\n}\n")
	if !found {
		t.Fatal("isolate_compose_environment has no end")
	}
	if !strings.Contains(body, "compgen -v BT_") || !strings.Contains(body, "compgen -v COMPOSE_") {
		t.Fatalf("isolation enumerates a fixed list instead of every BT_*/COMPOSE_* variable: %s", body)
	}
	harness := `set -euo pipefail
export BT_IMAGE_NAME=leak BT_SOURCE_SHA=leak BT_WHATEVER_NEW=leak COMPOSE_FILE=leak COMPOSE_PROJECT_NAME=leak KEEP_ME=kept
isolate_compose_environment() {` + body + `
}
isolate_compose_environment
[[ -z ${BT_IMAGE_NAME:-}${BT_SOURCE_SHA:-}${BT_WHATEVER_NEW:-}${COMPOSE_FILE:-}${COMPOSE_PROJECT_NAME:-} ]]
[[ $(compgen -v BT_ | wc -l) -eq 0 && $(compgen -v COMPOSE_ | wc -l) -eq 0 ]]
[[ $KEEP_ME == kept ]]
`
	if output, err := exec.Command("bash", "-c", harness).CombinedOutput(); err != nil {
		t.Fatalf("isolation harness: %v: %s", err, output)
	}
}

// --work-dir is made absolute and physical as soon as it is validated: the
// run directory under it is bind-mounted into containers and matched again on
// cleanup, and neither may depend on the caller's working directory or on a
// symlinked path (Unraid's /mnt/user is one).
func TestRehearsalCanonicalisesTheWorkDirectory(t *testing.T) {
	source, err := os.ReadFile(rehearsalScript)
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	const validation = `--work-dir is not a writable directory`
	const canonical = `work_root=$(cd "$work_root" && pwd -P)`
	const first_use = `mktemp -d "$work_root/bt-rehearse.XXXXXXXX"`
	validated, canonicalised, used := strings.Index(text, validation), strings.Index(text, canonical), strings.Index(text, first_use)
	if canonicalised < 0 {
		t.Fatalf("the script never canonicalises --work-dir (%s)", canonical)
	}
	if validated < 0 || used < 0 || !(validated < canonicalised && canonicalised < used) {
		t.Fatalf("canonicalisation must follow the writable check and precede the first use: validation@%d canonical@%d use@%d", validated, canonicalised, used)
	}
	// The same statement, run for real: a relative path through a symlink
	// comes out absolute and physical.
	directory := t.TempDir()
	physical, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(physical, filepath.Join(directory, "link")); err != nil {
		t.Fatal(err)
	}
	harness := `set -euo pipefail
cd "$1"
work_root=link
` + canonical + `
printf '%s' "$work_root"
`
	output, err := exec.Command("bash", "-c", harness, "harness", directory).CombinedOutput()
	if err != nil || string(output) != physical {
		t.Fatalf("canonicalised %q, want %q (%v)", output, physical, err)
	}
}

// A Linux host must be root: the bind-mounted ledger and the secret files are
// chowned to the containers' UIDs, which nothing else can do. The refusal
// happens during validation, before docker runs.
func TestRehearsalRefusesLinuxWithoutRoot(t *testing.T) {
	directory := t.TempDir()
	writeRehearsalStub(t, filepath.Join(directory, "docker"), "#!/bin/sh\necho 'docker must not run during argument validation' >&2\nexit 97\n")
	writeRehearsalStub(t, filepath.Join(directory, "uname"), "#!/bin/sh\necho Linux\n")
	writeRehearsalStub(t, filepath.Join(directory, "id"), "#!/bin/sh\necho 1000\n")
	env := []string{"PATH=" + directory + string(os.PathListSeparator) + os.Getenv("PATH")}
	status, output := runRehearsal(t, env, "--synthetic")
	if status != 2 || !strings.Contains(output, "as root") || strings.Contains(output, "docker must not run") {
		t.Fatalf("status=%d output=%s", status, output)
	}
}

// writeLedgerTar builds a tar whose listing is far longer than a pipe buffer:
// the named top-level directories (in order), each with `files` entries.
func writeLedgerTar(t *testing.T, path string, files int, directories ...string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	writer := tar.NewWriter(file)
	for _, directory := range directories {
		if err := writer.WriteHeader(&tar.Header{Name: directory + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < files; i++ {
			name := fmt.Sprintf("%s/segment-%06d-0123456789abcdef0123456789abcdef.jsonl", directory, i)
			if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: 0}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

// The ledger tar check must read tar's whole listing. `tar -tf | grep -q`
// lets grep exit at the first match; on a listing longer than the pipe buffer
// tar then dies of SIGPIPE (141) and pipefail reports a false "ledger tar has
// no security-ledger/" STOP, which is what the production tar (hundreds of
// entries) hit on Unraid (GNU tar 1.35) while the small synthetic ledger passed.
//
// bsdtar (macOS) swallows the broken pipe, so the harness lists through a tar
// with GNU tar's SIGPIPE behaviour: the platform tar piped through cat, which
// dies of SIGPIPE (141) when its reader leaves early.
func TestRehearsalLedgerTarCheckReadsTheWholeListing(t *testing.T) {
	realTar, err := exec.LookPath("tar")
	if err != nil {
		t.Skip("no tar on PATH")
	}
	stubs := t.TempDir()
	writeRehearsalStub(t, filepath.Join(stubs, "tar"), "#!/bin/bash\nset -o pipefail\n"+realTar+" \"$@\" | cat\n")
	path := "PATH=" + stubs + string(os.PathListSeparator) + os.Getenv("PATH")
	source, err := os.ReadFile(rehearsalScript)
	if err != nil {
		t.Fatal(err)
	}
	_, body, found := strings.Cut(string(source), "check_ledger_tar() {")
	if !found {
		t.Fatal("check_ledger_tar entry point missing")
	}
	body, _, found = strings.Cut(body, "\n}\n")
	if !found {
		t.Fatal("check_ledger_tar has no end")
	}
	if !strings.Contains(string(source), "\ncheck_ledger_tar \"$ledger_tar\"\n") {
		t.Fatal("the restore phase does not call check_ledger_tar on --ledger-tar")
	}
	harness := `set -euo pipefail
fail() { echo "STOP: $*" >&2; exit 1; }
check_ledger_tar() {` + body + `
}
check_ledger_tar "$1"
echo "LEDGER TAR: OK"
`
	const entries = 6000 // ~400 KiB of listing, several pipe buffers
	for _, tt := range []struct {
		name        string
		directories []string
		want        int
		message     string
	}{
		{"both directories, long listing", []string{"security-ledger", "ledger-head"}, 0, "LEDGER TAR: OK"},
		{"head first, long listing", []string{"ledger-head", "security-ledger"}, 0, "LEDGER TAR: OK"},
		{"no security-ledger", []string{"ledger-head", "something-else"}, 1, "STOP: ledger tar has no security-ledger/"},
		{"no ledger-head", []string{"security-ledger", "something-else"}, 1, "STOP: ledger tar has no ledger-head/"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ledger := filepath.Join(t.TempDir(), "ledger.tar")
			writeLedgerTar(t, ledger, entries, tt.directories...)
			command := exec.Command("bash", "-c", harness, "harness", ledger)
			command.Env = append(os.Environ(), path)
			output, err := command.CombinedOutput()
			status := 0
			if err != nil {
				exit, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatal(err)
				}
				status = exit.ExitCode()
			}
			if status != tt.want || !strings.Contains(string(output), tt.message) {
				t.Fatalf("status=%d, want %d; output=%s", status, tt.want, output)
			}
		})
	}
	// The unreadable case is a STOP too, not a silent exit from set -e.
	missing := exec.Command("bash", "-c", harness, "harness", filepath.Join(t.TempDir(), "missing.tar"))
	missing.Env = append(os.Environ(), path)
	status := 0
	output, err := missing.CombinedOutput()
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		status = exit.ExitCode()
	}
	if status != 1 || !strings.Contains(string(output), "STOP: ") {
		t.Fatalf("missing tar: status=%d output=%s", status, output)
	}
}
