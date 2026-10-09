package scripts

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrationOverlapRunsDistinctBinaries(t *testing.T) {
	directory := t.TempDir()
	sink := filepath.Join(directory, "invocations")
	priorBinary := fakeOverlapBinary(t, directory, "prior", "prior-build", sink)
	nextBinary := fakeOverlapBinary(t, directory, "next", "next-build", sink)
	priorCheck := fakeOverlapCheck(t, directory, "prior-check", sink)
	nextCheck := fakeOverlapCheck(t, directory, "next-check", sink)
	httpProbe := fakeFamilyHTTPProbe(t, directory, sink)
	priorAdmin := fakeOverlapAdmin(t, directory, "prior-admin", "prior-admin-build", sink, true)
	priorManifest := filepath.Join(directory, "prior.json")
	nextManifest := filepath.Join(directory, "next.json")
	writeFakeCompatibilityManifest(t, priorManifest, 10, 11)
	writeFakeCompatibilityManifest(t, nextManifest, 11, 11)

	run := func(extraEnv []string, args ...string) (string, error) {
		command := exec.Command("bash", append([]string{"check-migration-overlap.sh"}, args...)...)
		command.Env = append(os.Environ(), append([]string{
			"BT_OVERLAP_TEST_MODE=1",
			"BT_OVERLAP_PRIOR_BINARY=" + priorBinary,
			"BT_OVERLAP_NEXT_BINARY=" + nextBinary,
			"BT_OVERLAP_PRIOR_CHECK_BINARY=" + priorCheck,
			"BT_OVERLAP_NEXT_CHECK_BINARY=" + nextCheck,
			"BT_OVERLAP_HTTP_PROBE_BINARY=" + httpProbe,
			"BT_OVERLAP_PRIOR_ADMIN_BINARY=" + priorAdmin,
			"BT_OVERLAP_INVOCATION_SINK=" + sink,
			"BT_OVERLAP_PRIOR_MANIFEST=" + priorManifest,
			"BT_OVERLAP_NEXT_MANIFEST=" + nextManifest,
		}, extraEnv...)...)
		output, err := command.CombinedOutput()
		return string(output), err
	}
	validArgs := []string{"--base-ref", "HEAD~2", "--prior-ref", "HEAD~1", "--feature-ref", "HEAD", "--next-version", "11", "--prior-min", "10", "--prior-max", "11", "--next-min", "11", "--next-max", "11"}
	if output, err := run(nil, validArgs...); err != nil {
		t.Fatalf("valid overlap harness failed: %v: %s", err, output)
	}
	invocations, err := os.ReadFile(sink)
	if err != nil {
		t.Fatal(err)
	}
	if string(invocations) != "prior-build\nnext-build\nprior-admin-build\nprior-admin-migrate\nmode prepared-immutable\nprior-check prior_at_n_minus_1 10\nprior-check seed-session\nfamily-http prior_at_n_minus_1 POST /v1/family 201 prior-check\nfamily-http prior_at_n_minus_1 GET /v1/family 200 prior-check\nprior-check seed-session\nfamily-http prior_at_n_minus_1 invitation-create-read-caregiver-denial prior-check\nprior-check prior_at_n 11\nfamily-http prior_at_n GET /v1/family 200 prior-check\nfamily-http prior_at_n invitation-create-read-caregiver-denial prior-check\nnext-check current_at_n 11\nfamily-http current_at_n GET /v1/family 200 next-check\nfamily-http current_at_n invitation-create-read-caregiver-denial next-check\nfamily-http current_at_n timer-start-stop next-check\nscenario prior_at_n_minus_1 version 10\nscenario prior_at_n version 11\nscenario current_at_n version 11\ninit prior_at_n_minus_1\nmigrate prior_at_n\nreconcile current_at_n\ninit_count 1\n" {
		t.Fatalf("distinct binaries were not both invoked: %q", invocations)
	}
	t.Run("schema twelve exercises timer behavior in every overlap phase", func(t *testing.T) {
		priorTwelve := filepath.Join(directory, "prior-twelve.json")
		nextTwelve := filepath.Join(directory, "next-twelve.json")
		writeFakeCompatibilityManifest(t, priorTwelve, 11, 12)
		writeFakeCompatibilityManifest(t, nextTwelve, 12, 12)
		args := []string{"--base-ref", "HEAD~2", "--prior-ref", "HEAD~1", "--feature-ref", "HEAD", "--next-version", "12", "--prior-min", "11", "--prior-max", "12", "--next-min", "12", "--next-max", "12"}
		if output, err := run([]string{"BT_OVERLAP_PRIOR_MANIFEST=" + priorTwelve, "BT_OVERLAP_NEXT_MANIFEST=" + nextTwelve}, args...); err != nil {
			t.Fatalf("schema-twelve overlap harness failed: %v: %s", err, output)
		}
		invocations, err := os.ReadFile(sink)
		if err != nil {
			t.Fatal(err)
		}
		for _, expected := range []string{
			"prior-check seed-session account-id-file\n",
			"family-http prior_at_n_minus_1 timer-start-stop prior-check\n",
			"family-http prior_at_n timer-start-stop prior-check\n",
			"family-http current_at_n timer-start-stop next-check\n",
			"prior-check assert-import-unused 11\n",
			"prior-check assert-import-unused 12\n",
			"family-http current_at_n import-stage-resume-match-conflict-records next-check\n",
		} {
			if strings.Count(string(invocations), expected) != 1 {
				t.Fatalf("schema-twelve overlap omitted exact predecessor timer proof %q: %q", expected, invocations)
			}
		}
	})
	t.Run("schema thirteen runs predecessor import while delivery stays unused", func(t *testing.T) {
		priorThirteen := filepath.Join(directory, "prior-thirteen.json")
		nextThirteen := filepath.Join(directory, "next-thirteen.json")
		writeFakeCompatibilityManifest(t, priorThirteen, 12, 13)
		writeFakeCompatibilityManifest(t, nextThirteen, 13, 13)
		args := []string{"--base-ref", "HEAD~2", "--prior-ref", "HEAD~1", "--feature-ref", "HEAD", "--next-version", "13", "--prior-min", "12", "--prior-max", "13", "--next-min", "13", "--next-max", "13"}
		if output, err := run([]string{"BT_OVERLAP_PRIOR_MANIFEST=" + priorThirteen, "BT_OVERLAP_NEXT_MANIFEST=" + nextThirteen}, args...); err != nil {
			t.Fatalf("schema-thirteen overlap harness failed: %v: %s", err, output)
		}
		invocations, err := os.ReadFile(sink)
		if err != nil {
			t.Fatal(err)
		}
		for _, expected := range []string{
			"next-check assert-import-preserved 13 prior_at_n_minus_1\n",
			"next-check prior_at_n 13\n",
			"family-http prior_at_n_minus_1 import-stage-resume-match-conflict-records prior-check\n",
			"family-http prior_at_n import-stage-resume-match-conflict-records prior-check\n",
			"prior-check assert-delivery-unused 12\n",
			"prior-check assert-delivery-unused 13\n",
			"family-http current_at_n import-stage-resume-match-conflict-records next-check\n",
		} {
			if strings.Count(string(invocations), expected) != 1 {
				t.Fatalf("schema-thirteen overlap omitted %q: %q", expected, invocations)
			}
		}
		if strings.Contains(string(invocations), "assert-import-unused") {
			t.Fatalf("schema-thirteen predecessor did not exercise import: %q", invocations)
		}
		if strings.Contains(string(invocations), "prior-check prior_at_n 13\n") {
			t.Fatalf("immutable checker global empty-table assertion ran after durable import writes: %q", invocations)
		}
	})
	t.Run("account artifact flag starts at schema twelve", func(t *testing.T) {
		for _, version := range []int{9, 10, 11, 12} {
			t.Run(fmt.Sprintf("schema_%d", version), func(t *testing.T) {
				prior := filepath.Join(directory, fmt.Sprintf("prior-account-flag-%d.json", version))
				next := filepath.Join(directory, fmt.Sprintf("next-account-flag-%d.json", version))
				writeFakeCompatibilityManifest(t, prior, version-1, version)
				writeFakeCompatibilityManifest(t, next, version, version)
				args := []string{"--base-ref", "HEAD~2", "--prior-ref", "HEAD~1", "--feature-ref", "HEAD",
					"--next-version", fmt.Sprint(version), "--prior-min", fmt.Sprint(version - 1),
					"--prior-max", fmt.Sprint(version), "--next-min", fmt.Sprint(version), "--next-max", fmt.Sprint(version)}
				if output, err := run([]string{"BT_OVERLAP_PRIOR_MANIFEST=" + prior, "BT_OVERLAP_NEXT_MANIFEST=" + next}, args...); err != nil {
					t.Fatalf("schema-%d overlap harness failed: %v: %s", version, err, output)
				}
				invocations, err := os.ReadFile(sink)
				if err != nil {
					t.Fatal(err)
				}
				count := strings.Count(string(invocations), "prior-check seed-session account-id-file\n")
				if version < 12 && count != 0 {
					t.Fatalf("schema-%d passed unsupported account artifact flag: %q", version, invocations)
				}
				if version == 12 && count != 1 {
					t.Fatalf("schema-12 did not pass the account artifact flag exactly once: %q", invocations)
				}
			})
		}
	})
	t.Run("schema nine keeps invitation probes disabled", func(t *testing.T) {
		priorNine := filepath.Join(directory, "prior-nine.json")
		nextNine := filepath.Join(directory, "next-nine.json")
		writeFakeCompatibilityManifest(t, priorNine, 8, 9)
		writeFakeCompatibilityManifest(t, nextNine, 9, 9)
		args := []string{"--base-ref", "HEAD~2", "--prior-ref", "HEAD~1", "--feature-ref", "HEAD", "--next-version", "9", "--prior-min", "8", "--prior-max", "9", "--next-min", "9", "--next-max", "9"}
		if output, err := run([]string{"BT_OVERLAP_PRIOR_MANIFEST=" + priorNine, "BT_OVERLAP_NEXT_MANIFEST=" + nextNine}, args...); err != nil {
			t.Fatalf("schema-nine overlap harness failed: %v: %s", err, output)
		}
		invocations, err := os.ReadFile(sink)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(invocations), "seed-session\n") != 1 || strings.Contains(string(invocations), "invitation-create-read-caregiver-denial") {
			t.Fatalf("schema-nine test mode diverged from real probe guards: %q", invocations)
		}
	})
	t.Run("legacy Task 2 synthesis initializes current exactly once", func(t *testing.T) {
		legacyPrior := filepath.Join(directory, "legacy-prior.json")
		legacyNext := filepath.Join(directory, "legacy-next.json")
		writeOverlapFile(t, legacyPrior, `{"min":1,"max":3,"known":[{"version":1,"name":"000001_a.sql","sha256":"1111111111111111111111111111111111111111111111111111111111111111"},{"version":2,"name":"000002_b.sql","sha256":"2222222222222222222222222222222222222222222222222222222222222222"},{"version":3,"name":"000003_c.sql","sha256":"3333333333333333333333333333333333333333333333333333333333333333"}]}`)
		writeOverlapFile(t, legacyNext, `{"min":4,"max":4,"known":[{"version":1,"name":"000001_a.sql","sha256":"1111111111111111111111111111111111111111111111111111111111111111"},{"version":2,"name":"000002_b.sql","sha256":"2222222222222222222222222222222222222222222222222222222222222222"},{"version":3,"name":"000003_c.sql","sha256":"3333333333333333333333333333333333333333333333333333333333333333"},{"version":4,"name":"000004_d.sql","sha256":"4444444444444444444444444444444444444444444444444444444444444444"}]}`)
		legacyArgs := []string{"--base-ref", "HEAD~2", "--prior-ref", "HEAD~1", "--feature-ref", "HEAD", "--legacy-task2-bootstrap", "--next-version", "4", "--prior-min", "1", "--prior-max", "4", "--next-min", "4", "--next-max", "4"}
		if output, err := run([]string{"BT_OVERLAP_PRIOR_MANIFEST=" + legacyPrior, "BT_OVERLAP_NEXT_MANIFEST=" + legacyNext}, legacyArgs...); err != nil {
			t.Fatalf("legacy plan failed: %v: %s", err, output)
		}
		planned, err := os.ReadFile(sink)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(planned), "mode legacy-synthesized\n") || !strings.Contains(string(planned), "init current_at_n\nmigrate prior_at_n\nreconcile current_at_n\ninit_count 1\n") || strings.Contains(string(planned), "init prior_at_n_minus_1\n") {
			t.Fatalf("legacy plan=%q", planned)
		}
	})
	t.Run("predecessor admin must apply migration N", func(t *testing.T) {
		failingAdmin := fakeOverlapAdmin(t, directory, "failing-prior-admin", "failing-prior-admin-build", sink, false)
		if output, err := run([]string{"BT_OVERLAP_PRIOR_ADMIN_BINARY=" + failingAdmin}, validArgs...); err == nil {
			t.Fatalf("overlap accepted predecessor admin that cannot migrate N: output=%q", output)
		}
		invocations, readErr := os.ReadFile(sink)
		if readErr != nil || !strings.Contains(string(invocations), "failing-prior-admin-migrate\n") {
			t.Fatalf("predecessor migration was not attempted: invocations=%q err=%v", invocations, readErr)
		}
	})

	t.Run("reused binary path", func(t *testing.T) {
		if output, err := run([]string{"BT_OVERLAP_NEXT_BINARY=" + priorBinary}, validArgs...); err == nil || !strings.Contains(output, "distinct") {
			t.Fatalf("reused path err=%v output=%q", err, output)
		}
	})
	t.Run("reused overlap checker path", func(t *testing.T) {
		if output, err := run([]string{"BT_OVERLAP_NEXT_CHECK_BINARY=" + priorCheck}, validArgs...); err == nil || !strings.Contains(output, "distinct") {
			t.Fatalf("reused checker path err=%v output=%q", err, output)
		}
	})
	t.Run("wrong bounds", func(t *testing.T) {
		args := append([]string(nil), validArgs...)
		args[len(args)-1] = "5"
		if output, err := run(nil, args...); err == nil || !strings.Contains(output, "bounds") {
			t.Fatalf("wrong bounds err=%v output=%q", err, output)
		}
	})
	t.Run("altered historical checksum", func(t *testing.T) {
		changed := filepath.Join(directory, "changed.json")
		writeOverlapFile(t, changed, `{"min":1,"max":2,"known":[{"version":1,"name":"000001_a.sql","sha256":"9999999999999999999999999999999999999999999999999999999999999999"},{"version":2,"name":"000002_b.sql","sha256":"2222222222222222222222222222222222222222222222222222222222222222"}]}`)
		if output, err := run([]string{"BT_OVERLAP_NEXT_MANIFEST=" + changed}, validArgs...); err == nil || (!strings.Contains(output, "historical migration") && !strings.Contains(output, "same migration history")) {
			t.Fatalf("changed checksum err=%v output=%q", err, output)
		}
	})
	t.Run("arithmetically valid flags reject current manifest bounds", func(t *testing.T) {
		changed := filepath.Join(directory, "wrong-next-bounds.json")
		writeFakeCompatibilityManifest(t, changed, 10, 11)
		if output, err := run([]string{"BT_OVERLAP_NEXT_MANIFEST=" + changed}, validArgs...); err == nil || !strings.Contains(output, "bounds") {
			t.Fatalf("mismatching manifest bounds err=%v output=%q", err, output)
		}
	})
	t.Run("missing prior ref", func(t *testing.T) {
		args := append([]string(nil), validArgs...)
		args[3] = "refs/heads/definitely-missing"
		if output, err := run(nil, args...); err == nil || !strings.Contains(output, "prior ref") {
			t.Fatalf("missing ref err=%v output=%q", err, output)
		}
	})
	t.Run("reversed ancestry", func(t *testing.T) {
		args := append([]string(nil), validArgs...)
		args[1] = "HEAD~1"
		args[3] = "HEAD~2"
		if output, err := run(nil, args...); err == nil || !strings.Contains(output, "not ordered") {
			t.Fatalf("reversed ancestry err=%v output=%q", err, output)
		}
	})
	t.Run("equal refs", func(t *testing.T) {
		args := append([]string(nil), validArgs...)
		args[1] = "HEAD~1"
		args[3] = "HEAD~1"
		if output, err := run(nil, args...); err == nil || !strings.Contains(output, "three distinct") {
			t.Fatalf("equal refs err=%v output=%q", err, output)
		}
	})
}

func TestImportProbeArgumentsRemainCompatibleWithSchemaTwelveChecker(t *testing.T) {
	source, err := os.ReadFile("check-migration-overlap.sh")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(source), "probe_import_feature_http() {")
	end := strings.Index(string(source), "run_gate() {")
	if start < 0 || end <= start {
		t.Fatal("could not locate actual import HTTP probe function")
	}

	directory := t.TempDir()
	sink := filepath.Join(directory, "import-invocations")
	priorChecker := fakeStrictImportOverlapCheck(t, directory, "prior-check", sink)
	nextChecker := fakeStrictImportOverlapCheck(t, directory, "next-overlap-check", sink)
	command := exec.Command("bash", "-c", `set -euo pipefail
workspace=$1
family_curl_config=/tmp/owner.curl
caregiver_family_curl_config=/tmp/caregiver.curl
`+string(source[start:end])+`
next_version=12
probe_import_feature_http "$2" 12345 current_at_n
next_version=13
printf '{}\n' > "$workspace/sync-overlap-cursor.json"
probe_import_feature_http "$2" 12345 prior_at_n_minus_1
probe_import_feature_http "$2" 12345 prior_at_n
probe_import_feature_http "$2" 12345 current_at_n
`, "import-argument-regression", directory, priorChecker)
	if output, runErr := command.CombinedOutput(); runErr != nil {
		t.Fatalf("strict archived checker compatibility failed: %v: %s", runErr, output)
	}
	invocations, err := os.ReadFile(sink)
	if err != nil {
		t.Fatal(err)
	}
	want := "prior-check schema12\n" +
		"prior-check schema13 prior_at_n_minus_1 " + filepath.Join(directory, "predecessor-import-untrusted-high-water.json") + "\n" +
		"next-overlap-check drain " + filepath.Join(directory, "predecessor-import-start-cursor.json") + "\n" +
		"next-overlap-check schema13 prior_at_n " + filepath.Join(directory, "sync-overlap-cursor.json") + "\n" +
		"next-overlap-check schema13 current_at_n " + filepath.Join(directory, "sync-overlap-cursor.json") + "\n"
	if string(invocations) != want {
		t.Fatalf("import probe arguments=%q want=%q", invocations, want)
	}
	if priorChecker == nextChecker {
		t.Fatal("schema-thirteen import regression did not use distinct checker identities")
	}
}

func TestCompatibilityPredecessorProductionGraphHasNoIdentityFeature(t *testing.T) {
	priorRef := os.Getenv("BT_MIGRATION_PRIOR_REF")
	if priorRef == "" {
		t.Skip("BT_MIGRATION_PRIOR_REF is not set; requires the reviewed compatibility predecessor")
	}
	workspace := t.TempDir()
	archive := exec.Command("git", "archive", "--format=tar", priorRef, "Server")
	archive.Dir = "../.."
	tar := exec.Command("tar", "-x", "-C", workspace)
	pipe, err := archive.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	tar.Stdin = pipe
	if err := tar.Start(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Run(); err != nil {
		t.Fatalf("archive compatibility predecessor: %v", err)
	}
	if err := tar.Wait(); err != nil {
		t.Fatalf("extract compatibility predecessor: %v", err)
	}
	if predecessorManifestMax(t, workspace) != 5 {
		t.Skip("identity dependency check applies only to the schema-five predecessor")
	}
	command := exec.Command("go", "list", "-deps", "./cmd/baby-tracker-server")
	command.Dir = filepath.Join(workspace, "Server")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("list predecessor dependency graph: %v: %s", err, output)
	}
	for _, forbidden := range []string{
		"babytracker/server/internal/identity",
		"babytracker/server/internal/limits",
		"babytracker/server/internal/auth",
	} {
		for _, dependency := range strings.Fields(string(output)) {
			if dependency == forbidden || strings.HasPrefix(dependency, forbidden+"/") {
				t.Fatalf("compatibility predecessor production graph contains schema-five feature dependency %q", dependency)
			}
		}
	}
}

func TestMigrationOverlapUsesCurrentCredentialFiles(t *testing.T) {
	script, err := os.ReadFile("check-migration-overlap.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	if strings.Contains(text, "BT_SESSION_SIGNING_KEY_FILE") {
		t.Fatal("overlap harness still passes the removed session-signing alias")
	}
	for _, variable := range []string{
		"BT_SESSION_TOKEN_PEPPER_FILE",
		"BT_DEVICE_CONTINUITY_PEPPER_FILE",
		"BT_APPLE_NOTIFICATION_PEPPER_FILE",
		"BT_ACTOR_ID_PEPPER_FILE",
		"BT_PAGE_TOKEN_KEY_ID",
		"BT_PAGE_TOKEN_KEY_FILE",
	} {
		if !strings.Contains(text, variable) {
			t.Fatalf("overlap harness omits %s", variable)
		}
	}
	for _, required := range []string{
		"probe_feature_sync_http", "/v1/sync/snapshot?page_token=", "Overlap second before", "Overlap second after",
		"probe_timer_feature_sync_http", "timer-start-stop", "records-v1,timer-commands-v1",
		`probe_timer_feature_sync_http "$port" "$phase"`, `if [[ $next_version -ge 12`,
		"BT_SYNC_PAGE_SIZE=1", "sync-overlap-cursor.json", `--argjson cursor "$initial_cursor"`,
		"prior_at_n_minus_1) suffix=4", "prior_at_n) suffix=3; feature_curl_config=$caregiver_family_curl_config",
		`curl --config "$feature_curl_config" --config "$snapshot_continuation_config"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("overlap harness omits feature sync proof %q", required)
		}
	}
}

func TestMigrationOverlapFeaturePhaseUsesTaggedHTTPBootstrappedFamily(t *testing.T) {
	script, err := os.ReadFile("check-migration-overlap.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	featureBranch := `if [[ $next_version -ge 13 && "$pid_name" == next && "$phase" == current_at_n ]]; then`
	branch := strings.Index(text, featureBranch)
	if branch < 0 {
		t.Fatal("overlap harness lacks an isolated candidate feature phase")
	}
	remaining := text[branch:]
	end := strings.Index(remaining, "\n  fi")
	if end < 0 {
		t.Fatal("candidate feature phase is not bounded")
	}
	feature := remaining[:end]
	for _, required := range []string{
		`"$workspace/next-protocol-client"`, `--phase overlap`,
		`assert-delivery-feature`, `--protocol-state-file "$protocol_state"`,
	} {
		if !strings.Contains(feature, required) {
			t.Fatalf("candidate feature phase omits %q", required)
		}
	}
	for _, forbidden := range []string{
		`probe_family_http`, `probe_import_feature_http`, `--account-id-file`,
		`--family-id-file`, `--cursor-file`,
	} {
		if strings.Contains(feature, forbidden) {
			t.Fatalf("candidate feature phase reuses predecessor artifact %q", forbidden)
		}
	}
	client, err := os.ReadFile("../cmd/protocol-e2e-client/main.go")
	if err != nil {
		t.Fatal(err)
	}
	clientText := string(client)
	for _, required := range []string{
		`case "overlap":`, `c.bootstrap(ctx, *statePath, true)`,
		`"/__identitytest/proof"`, `"/v1/family"`, `"/v1/family/import/resolve"`,
	} {
		if !strings.Contains(clientText, required) {
			t.Fatalf("tagged overlap client omits supported HTTP flow %q", required)
		}
	}
	if strings.Contains(text, "INSERT INTO family_entitlements") || strings.Contains(text, "UPDATE family_entitlements") {
		t.Fatal("overlap harness synthesizes feature entitlement with SQL")
	}
}

func TestMigrationOverlapEnablesHostedTestAdapterOnlyForCandidateFeatureServer(t *testing.T) {
	script, err := os.ReadFile("check-migration-overlap.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	start := strings.Index(text, "run_gate() {")
	end := strings.Index(text[start:], "\nport_base=")
	if start < 0 || end < 0 {
		t.Fatal("overlap run_gate function not found")
	}
	runGate := text[start : start+end]
	for _, required := range []string{
		`"$pid_name:$phase" == next:current_at_n`,
		`BT_MODE=hosted`, `BT_IDENTITYTEST_HOSTED_ADAPTER=1`,
	} {
		if !strings.Contains(runGate, required) {
			t.Fatalf("candidate server composition omits %q", required)
		}
	}
	if !strings.Contains(text, `"BT_MODE=development"`) {
		t.Fatal("predecessor overlap mode is no longer development")
	}
}

func TestLifecycleCompatibilityPredecessorProductionGraphHasNoOperatorFeature(t *testing.T) {
	priorRef := os.Getenv("BT_MIGRATION_PRIOR_REF")
	if priorRef == "" {
		t.Skip("BT_MIGRATION_PRIOR_REF is not set; requires the reviewed compatibility predecessor")
	}
	workspace := t.TempDir()
	archive := exec.Command("git", "archive", "--format=tar", priorRef, "Server")
	archive.Dir = "../.."
	tar := exec.Command("tar", "-x", "-C", workspace)
	pipe, err := archive.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	tar.Stdin = pipe
	if err := tar.Start(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Run(); err != nil {
		t.Fatalf("archive compatibility predecessor: %v", err)
	}
	if err := tar.Wait(); err != nil {
		t.Fatalf("extract compatibility predecessor: %v", err)
	}
	if predecessorManifestMax(t, workspace) != 8 {
		t.Skip("lifecycle dependency check applies only to the schema-eight predecessor")
	}
	command := exec.Command("go", "list", "-deps", "./cmd/baby-tracker-server")
	command.Dir = filepath.Join(workspace, "Server")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("list predecessor dependency graph: %v: %s", err, output)
	}
	for _, dependency := range strings.Fields(string(output)) {
		if dependency == "babytracker/server/internal/operator" || strings.HasPrefix(dependency, "babytracker/server/internal/operator/") {
			t.Fatalf("compatibility predecessor production graph contains schema-eight operator dependency %q", dependency)
		}
	}
}

func TestRecordsCompatibilityPredecessorProductionGraphHasNoRecordOrSyncFeature(t *testing.T) {
	priorRef := os.Getenv("BT_MIGRATION_PRIOR_REF")
	if priorRef == "" {
		t.Skip("BT_MIGRATION_PRIOR_REF is not set; requires the reviewed compatibility predecessor")
	}
	workspace := t.TempDir()
	archive := exec.Command("git", "archive", "--format=tar", priorRef, "Server")
	archive.Dir = "../.."
	tar := exec.Command("tar", "-x", "-C", workspace)
	pipe, err := archive.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	tar.Stdin = pipe
	if err := tar.Start(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Run(); err != nil {
		t.Fatalf("archive compatibility predecessor: %v", err)
	}
	if err := tar.Wait(); err != nil {
		t.Fatalf("extract compatibility predecessor: %v", err)
	}
	if predecessorManifestMax(t, workspace) != 9 {
		t.Skip("records dependency check applies only to the schema-nine predecessor")
	}
	command := exec.Command("go", "list", "-deps", "./cmd/baby-tracker-server")
	command.Dir = filepath.Join(workspace, "Server")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("list predecessor dependency graph: %v: %s", err, output)
	}
	for _, forbidden := range []string{
		"babytracker/server/internal/records",
		"babytracker/server/internal/sync",
	} {
		for _, dependency := range strings.Fields(string(output)) {
			if dependency == forbidden || strings.HasPrefix(dependency, forbidden+"/") {
				t.Fatalf("compatibility predecessor production graph contains schema-nine feature dependency %q", dependency)
			}
		}
	}
}

func predecessorManifestMax(t *testing.T, workspace string) int64 {
	t.Helper()
	encoded, err := os.ReadFile(filepath.Join(workspace, "Server", "internal", "migrate", "compatibility.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Max int64 `json:"max"`
	}
	if err := json.Unmarshal(encoded, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest.Max
}

func fakeOverlapBinary(t *testing.T, directory, name, buildID, sink string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	script := "#!/bin/sh\n[ \"$1\" = build-id ] || exit 2\nprintf '%s\\n' '" + buildID + "' >> '" + sink + "'\nprintf '%s\\n' '" + buildID + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFakeCompatibilityManifest(t *testing.T, path string, minimum, maximum int) {
	t.Helper()
	type knownMigration struct {
		Version int    `json:"version"`
		Name    string `json:"name"`
		SHA256  string `json:"sha256"`
	}
	manifest := struct {
		Min   int              `json:"min"`
		Max   int              `json:"max"`
		Known []knownMigration `json:"known"`
	}{Min: minimum, Max: maximum}
	for version := 1; version <= maximum; version++ {
		manifest.Known = append(manifest.Known, knownMigration{Version: version,
			Name: fmt.Sprintf("%06d_fake.sql", version), SHA256: fmt.Sprintf("%064x", version)})
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeOverlapFile(t, path, string(encoded))
}

func fakeOverlapCheck(t *testing.T, directory, name, sink string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	script := "#!/bin/sh\nif [ \"$1\" = seed-session ]; then\n  [ \"$2\" = --curl-config ] || exit 2\n  if [ \"$3\" = /tmp/redacted-overlap-curl-config ]; then\n    if [ \"$#\" = 3 ]; then suffix=; elif [ \"$#\" = 5 ] && [ \"$4\" = --account-id-file ] && [ \"$5\" = /tmp/redacted-overlap-account-id ]; then suffix=' account-id-file'; else exit 2; fi\n  elif [ \"$3\" = /tmp/redacted-overlap-caregiver-curl-config ]; then\n    [ \"$#\" = 7 ] && [ \"$4\" = --identity-subject ] && [ \"$5\" = overlap-family-http-caregiver ] && [ \"$6\" = --refresh-request ] && [ \"$7\" = /tmp/redacted-overlap-caregiver-refresh ] || exit 2\n    suffix=\n  else\n    exit 2\n  fi\n  printf '%s seed-session%s\\n' '" + name + "' \"$suffix\" >> '" + sink + "'\n  exit 0\nfi\nif [ \"$1\" = assert-import-preserved ]; then\n  shift\n  while [ \"$#\" -gt 0 ]; do\n    case \"$1\" in --expected-version) version=$2;; --phase) phase=$2;; esac\n    shift 2\n  done\n  [ -n \"${version:-}\" ] && [ -n \"${phase:-}\" ] || exit 2\n  printf '%s assert-import-preserved %s %s\\n' '" + name + "' \"$version\" \"$phase\" >> '" + sink + "'\n  exit 0\nfi\nif [ \"$1\" = assert-import-unused ]; then\n  shift\n  while [ \"$#\" -gt 0 ]; do\n    if [ \"$1\" = --expected-version ]; then version=$2; shift 2; else shift 2; fi\n  done\n  [ -n \"${version:-}\" ] || exit 2\n  printf '%s assert-import-unused %s\\n' '" + name + "' \"$version\" >> '" + sink + "'\n  exit 0\nfi\n[ \"$1\" = --scenario ] || exit 2\n[ \"$3\" = --expected-version ] || exit 2\nprintf '%s %s %s\\n' '" + name + "' \"$2\" \"$4\" >> '" + sink + "'\n"
	script = strings.Replace(script, `if [ "$1" = assert-import-unused ]; then`, `if [ "$1" = assert-import-unused ] || [ "$1" = assert-delivery-unused ]; then
  command=$1`, 1)
	script = strings.Replace(script, `printf '%s assert-import-unused %s\n' '`, `printf '%s %s %s\n' '`, 1)
	script = strings.Replace(script, `' "$version" >> '`, `' "$command" "$version" >> '`, 1)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func fakeFamilyHTTPProbe(t *testing.T, directory, sink string) string {
	t.Helper()
	path := filepath.Join(directory, "family-http-probe")
	script := "#!/bin/sh\n[ \"$#\" = 3 ] || exit 2\nif [ \"$2\" = create-read ]; then\n  printf 'family-http %s POST /v1/family 201 %s\\n' \"$1\" \"$3\" >> '" + sink + "'\nelif [ \"$2\" = invitation-create-read-caregiver-denial ]; then\n  printf 'family-http %s invitation-create-read-caregiver-denial %s\\n' \"$1\" \"$3\" >> '" + sink + "'\n  exit 0\nelif [ \"$2\" = timer-start-stop ]; then\n  printf 'family-http %s timer-start-stop %s\\n' \"$1\" \"$3\" >> '" + sink + "'\n  exit 0\nelif [ \"$2\" = import-stage-resume-match-conflict-records ]; then\n  printf 'family-http %s import-stage-resume-match-conflict-records %s\\n' \"$1\" \"$3\" >> '" + sink + "'\n  exit 0\nelif [ \"$2\" != read ]; then\n  exit 2\nfi\nprintf 'family-http %s GET /v1/family 200 %s\\n' \"$1\" \"$3\" >> '" + sink + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func fakeStrictImportOverlapCheck(t *testing.T, directory, name, sink string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	script := `#!/bin/sh
set -eu
identity=$(basename "$0")
if [ "$1" = drain-sync-http ]; then
  [ "$2" = --origin ] && [ "$3" = http://127.0.0.1:12345 ]
  [ "$4" = --owner-curl-config ] && [ "$5" = /tmp/owner.curl ]
  [ "$6" = --cursor-file ]
  printf '%s drain %s\n' "$identity" "$7" >> "$BT_STRICT_IMPORT_SINK"
  exit 0
fi
[ "$1" = probe-import-http ]
[ "$2" = --origin ] && [ "$3" = http://127.0.0.1:12345 ]
[ "$4" = --owner-curl-config ] && [ "$5" = /tmp/owner.curl ]
[ "$6" = --caregiver-curl-config ] && [ "$7" = /tmp/caregiver.curl ]
if [ "$#" = 7 ]; then
  printf '%s schema12\n' "$identity" >> "$BT_STRICT_IMPORT_SINK"
elif [ "$#" = 11 ] && [ "$8" = --phase ] && { [ "$9" = prior_at_n_minus_1 ] || [ "$9" = prior_at_n ] || [ "$9" = current_at_n ]; } &&
     [ "${10}" = --cursor-file ]; then
  printf '%s schema13 %s %s\n' "$identity" "$9" "${11}" >> "$BT_STRICT_IMPORT_SINK"
else
  printf 'unexpected import arguments:' >&2
  printf ' <%s>' "$@" >&2
  printf '\n' >&2
  exit 2
fi
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BT_STRICT_IMPORT_SINK", sink)
	return path
}

func fakeOverlapAdmin(t *testing.T, directory, name, buildID, sink string, migrate bool) string {
	t.Helper()
	path := filepath.Join(directory, name)
	exit := "exit 23"
	if migrate {
		exit = "exit 0"
	}
	script := "#!/bin/sh\ncase \"$1\" in\nbuild-id) printf '%s\\n' '" + buildID + "' >> '" + sink + "'; printf '%s\\n' '" + buildID + "';;\nmigrate) printf '%s\\n' '" + name + "-migrate' >> '" + sink + "'; " + exit + ";;\n*) exit 2;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeOverlapFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
