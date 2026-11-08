package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestProtocolE2EUsesIdentityTestTag(t *testing.T) {
	directory := t.TempDir()
	arguments := filepath.Join(directory, "arguments")
	environment := filepath.Join(directory, "environment")
	stub := filepath.Join(directory, "go")
	writeExecutable(t, stub, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$BT_CAPTURE_ARGUMENTS\"\nprintf '%s\\n' \"${BABY_TRACKER_REQUIRE_DATABASE:-}\" >> \"$BT_CAPTURE_ENVIRONMENT\"\n")
	command := exec.CommandContext(t.Context(), "bash", "protocol-e2e.sh")
	command.Env = append(os.Environ(), "BT_PROTOCOL_E2E_STUB_MODE=1", "BABY_TRACKER_TEST_DATABASE_URL=postgres://disposable.invalid/test",
		"BT_PROTOCOL_E2E_GO="+stub, "BT_CAPTURE_ARGUMENTS="+arguments, "BT_CAPTURE_ENVIRONMENT="+environment)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("protocol e2e stub: %v: %s", err, output)
	}
	args, _ := os.ReadFile(arguments)
	for _, required := range []string{"test -race -count=1 ./internal/realtime", "test -tags identitytest -race -count=1 ./tests/protocol/..."} {
		if !strings.Contains(string(args), required) {
			t.Fatalf("arguments=%q missing %q", args, required)
		}
	}
	required, _ := os.ReadFile(environment)
	if got := strings.Fields(string(required)); len(got) != 2 || got[0] != "1" || got[1] != "1" {
		t.Fatalf("BABY_TRACKER_REQUIRE_DATABASE=%q", required)
	}
}

func TestProtocolE2EPropagatesTestFailure(t *testing.T) {
	directory := t.TempDir()
	stub := filepath.Join(directory, "go")
	writeExecutable(t, stub, "#!/bin/sh\nexit 23\n")
	command := exec.CommandContext(t.Context(), "bash", "protocol-e2e.sh")
	command.Env = append(os.Environ(), "BT_PROTOCOL_E2E_STUB_MODE=1", "BABY_TRACKER_TEST_DATABASE_URL=postgres://disposable.invalid/test", "BT_PROTOCOL_E2E_GO="+stub)
	if err := command.Run(); err == nil {
		t.Fatal("protocol e2e swallowed test failure")
	}
}

func TestProtocolE2ERefreshesEphemeralPortAfterRestart(t *testing.T) {
	script, err := os.ReadFile("protocol-e2e.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	refresh := strings.Index(text, `restart_binding=$(compose port app 8080)`)
	rebase := strings.Index(text, `base="http://$restart_binding"`)
	verify := strings.Index(text, `--phase verify`)
	if refresh < 0 || rebase < refresh || verify < rebase {
		t.Fatalf("restart binding is not refreshed before verification")
	}
}

func TestProtocolE2EAssertsExactPersistedCompletedFeed(t *testing.T) {
	script, err := os.ReadFile("protocol-e2e.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, required := range []string{
		"completed_feed=", "21000000-0000-4000-8000-000000000201",
		"11000000-0000-4000-8000-000000000101", "FROM care_entities", "breast_seconds=600", "'payload'->'components'",
		"1:true:1:21100000-0000-4000-8000-000000000201,breast,left,600,true,true",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("external protocol gate omits exact completed-feed assertion %q", required)
		}
	}
}

// Schema 30 keeps care data only in care_entities and schedules no durable
// wake: the gate reads the completed feed and the owner-reinstall ledger from
// care_entities and asserts the wake queue relation is gone.
func TestProtocolE2EReadsCareEntitiesAndNoWakeQueue(t *testing.T) {
	script, err := os.ReadFile("protocol-e2e.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	if !strings.Contains(text, `SELECT to_regclass('public.delivery_pending') IS NULL`) {
		t.Fatal("protocol e2e does not assert that the wake queue relation is gone")
	}
	for _, ledger := range []string{"kind=1 AND", "kind=2 AND", "kind=3 AND", "kind=4 AND", "kind=5 AND", "kind=6 AND", "kind=7 AND", "kind=8 AND", "kind=9 AND", "kind=10 AND"} {
		if !strings.Contains(text, ledger) {
			t.Fatalf("owner-reinstall ledger omits care kind %q", ledger)
		}
	}
	for _, retired := range []string{"FROM feed_entries", "FROM feed_components", "FROM babies", "FROM active_timers", "FROM delivery_pending"} {
		if strings.Contains(text, retired) {
			t.Fatalf("protocol e2e still reads the legacy care storage: %q", retired)
		}
	}
}

// requireSwiftProofLine requires protocol-e2e.sh to fail unless the Swift
// suite's log holds a line starting with proof: a live (uncommented) grep
// of $swift_log that exits 1 when the line is missing, placed after the
// live line that runs the Swift suite, so it checks that run's log. A
// textual match alone would accept a commented-out or misplaced grep.
func requireSwiftProofLine(t *testing.T, script, proof string) {
	t.Helper()
	swift := regexp.MustCompile(`(?m)^\s*swift test --filter FamilySyncClientE2ETests\) 2>&1 \| tee "\$swift_log"$`).FindStringIndex(script)
	if swift == nil {
		t.Fatal("protocol e2e does not run the Swift suite into $swift_log")
	}
	grep := regexp.MustCompile(`(?m)^grep -q '\^`+regexp.QuoteMeta(proof)+`' "\$swift_log" \|\| \{ echo "[^"]+" >&2; exit 1; \}$`).FindAllStringIndex(script, -1)
	if len(grep) != 1 {
		t.Fatalf("protocol e2e has %d live greps requiring %q in the Swift log, want 1", len(grep), proof)
	}
	if grep[0][0] < swift[1] {
		t.Fatalf("protocol e2e greps for %q before the Swift suite runs", proof)
	}
}

// The wake worker's flush is proven on the real server: after the verify
// phase, the script waits up to 10 s for a device_sync_cursors row with a
// cursor for each phone device, fails without both, and prints the proof.
func TestProtocolE2ERequiresTheWakeProof(t *testing.T) {
	script, err := os.ReadFile("protocol-e2e.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	verify := strings.Index(text, "--phase verify")
	query := strings.Index(text, "FROM device_sync_cursors WHERE family_id='$family_id' AND device_id IN ('$owner_device','$caregiver_device') AND greatest(acked_sequence, delivered_sequence) > 0")
	wait := strings.Index(text, "for _ in $(seq 1 100); do\n  wake_rows=")
	refuse := strings.Index(text, `[[ $wake_rows == 2 ]] || { echo "wake worker did not record both phone devices within 10 s`)
	proof := regexp.MustCompile(`(?m)^echo "wake proof: `).FindStringIndex(text)
	if verify < 0 || query < verify || wait < verify || wait > query || refuse < query || proof == nil || proof[0] < refuse {
		t.Fatalf("protocol e2e lacks the wake proof after the verify phase (verify %d, wait %d, query %d, refusal %d, proof %v)",
			verify, wait, query, refuse, proof)
	}
}

// Snapshot pages serve running timers verbatim since Phase 2; the iOS gate
// must insist the client applied such a snapshot, not just that the suite
// passed (it passes early when it cannot see the server). The two-caregiver
// and owner-reinstall tests' last steps are required the same way.
func TestProtocolE2ERequiresTheRunningTimerSnapshotStep(t *testing.T) {
	script, err := os.ReadFile("protocol-e2e.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, proof := range []string{"running-timer snapshot proof: ", "two-caregiver proof: step 24:", "immunization proof: step 6:", "pumping proof: step 6:", "owner-reinstall proof: step 6:"} {
		requireSwiftProofLine(t, string(script), proof)
	}
}

// liveLine returns the offset of the one uncommented line of script that
// matches pattern (anchored on its own line), failing unless there is exactly
// one, so a commented-out or duplicated line cannot satisfy a pin.
func liveLine(t *testing.T, script, what, pattern string) int {
	t.Helper()
	found := regexp.MustCompile(`(?m)^`+pattern+`$`).FindAllStringIndex(script, -1)
	if len(found) != 1 {
		t.Fatalf("protocol e2e has %d live lines for %s, want 1", len(found), what)
	}
	return found[0][0]
}

// A backgrounded caregiver is woken by an APNs push and pulls (Phase 3 Task
// 18). Both modes compose the push profile before the composition starts; the
// Swift suite is told to run the push test; its last line is required, as is
// the recorder's own account read back from the server, filtered to the token
// the test names on its proof line: exactly one push to that sandbox token
// (other tests' tokens are not counted), with the background headers and a
// content-available-only payload, and the token absent from the server log.
func TestProtocolE2ERequiresTheBackgroundPushProof(t *testing.T) {
	raw, err := os.ReadFile("protocol-e2e.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	requireSwiftProofLine(t, script, "background-push proof: ")

	profile := liveLine(t, script, "the push profile", `export BT_DISPOSABLE_PUSH=1`)
	liveActivity := liveLine(t, script, "the Live Activity profile", `export BT_DISPOSABLE_LIVE_ACTIVITY=1`)
	source := liveLine(t, script, "the disposable composition", `source Server/scripts/lib/disposable-sync.sh`)
	start := liveLine(t, script, "the composition start", `start_disposable_sync "\$\{BT_RECOVERY_E2E_MODE:-hosted\}" normal`)
	if profile > source || liveActivity > source || source > start {
		t.Fatal("protocol e2e chooses the push or Live Activity profile after the composition starts")
	}
	// main ran its Live Activity profile only in the ordinary gate; the port
	// runs it in both modes, as the push profile is.
	if regexp.MustCompile(`(?m)^\s*if .*BT_RECOVERY_E2E_MODE.*\n(\s*export BT_DISPOSABLE_\w+=1\n)*\s*export BT_DISPOSABLE_(PUSH|LIVE_ACTIVITY)`).MatchString(script) {
		t.Fatal("the push and Live Activity profiles must run in both modes")
	}

	suite := liveLine(t, script, "the Swift suite's working directory", `\(cd Packages/BabyTracker && \\`)
	apns := liveLine(t, script, "the push test switch", `  BABY_TRACKER_E2E_APNS=1 \\`)
	swift := liveLine(t, script, "the Swift suite", `  swift test --filter FamilySyncClientE2ETests\) 2>&1 \| tee "\$swift_log"`)
	if suite > apns || apns > swift {
		t.Fatal("protocol e2e does not pass BABY_TRACKER_E2E_APNS=1 to the Swift suite")
	}

	// Only this test's token is counted: it is read from the test's proof
	// line, must be a whole token, and filters the recorder before the
	// projection, so another test's token cannot change the count.
	token := liveLine(t, script, "the token read",
		`push_token=\$\(grep '\^background-push proof: ' "\$swift_log" \| tail -n 1 \| sed -nE 's/\^background-push proof: token=\(\[0-9a-f\]\{64\}\) \.\*\$/\\1/p'\)`)
	whole := liveLine(t, script, "the token shape check",
		`\[\[ \$push_token =~ \^\[0-9a-f\]\{64\}\$ \]\] \|\| \{ echo "[^"]+" >&2; exit 1; \}`)
	records := liveLine(t, script, "the recorder read filtered to the test's token",
		`push_records=\$\(curl --silent --fail --max-time 5 "\$base/__identitytest/apns" \| jq -c --arg token "\$push_token" `+
			`'\[\.requests\[\] \| select\(\.token == \$token\) \| \{[^']+\}\]'\)`)
	if token < swift || whole < token || records < whole {
		t.Fatal("protocol e2e reads the recorder before it knows the test's token")
	}
	exact := liveLine(t, script, "the recorder assertion",
		`\[\[ \$push_records == "\$expected_push" \]\] \|\| \{ printf '[^']+' "\$push_records" >&2; exit 1; \}`)
	expected := liveLine(t, script, "the expected push", `expected_push='\[\{"host":"sandbox\.apns\.identitytest\.invalid","path":"/3/device/`+
		`808182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9f","push_type":"background","priority":"5",`+
		`"topic":"com\.example\.babytracker\.protocol","provider_token_valid":true,"acknowledged":true,"payload":\{"aps":\{"content-available":1\}\}\}\]'`)
	appLog := liveLine(t, script, "the server-log read", `push_app_log=\$\(compose logs --no-color app 2>&1\)`)
	leak := liveLine(t, script, "the server-log token check",
		`\[\[ -n \$push_app_log && \$push_app_log != \*"\$push_token"\* \]\] \|\| \{ echo "[^"]+" >&2; exit 1; \}`)
	proof := liveLine(t, script, "the recorder proof", `echo "background-push recorder proof: .+"`)
	if records < swift || expected < swift || exact < records || exact < expected || appLog < swift || leak < appLog || proof < exact || proof < leak {
		t.Fatal("protocol e2e checks the push recorder before the Swift suite runs, or prints its proof first")
	}
}

// The app's Live Activity cards end to end (Phase 4 Task 25; main's
// protocol-e2e.sh lines 143-169 re-pointed at 000032's tables). The Swift
// test's last step is required; the phones' ledger line is recomputed from
// Postgres for the family the test printed (registrations = non-revoked
// devices, sandbox = sandbox devices, cards = card tokens); the worker must
// have had at least one push accepted for that family; no token the test
// names may appear in the server's log; the summary is printed last.
func TestProtocolE2ERequiresTheLiveActivityProof(t *testing.T) {
	raw, err := os.ReadFile("protocol-e2e.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	requireSwiftProofLine(t, script, "live-activity proof: step 13:")
	swift := liveLine(t, script, "the Swift suite", `  swift test --filter FamilySyncClientE2ETests\) 2>&1 \| tee "\$swift_log"`)

	ledger := liveLine(t, script, "the phones' ledger line", `la_ledger=\$\(grep '\^live-activity ledger: ' "\$swift_log" \| tail -n 1\)`)
	family := liveLine(t, script, "the ledger's family",
		`la_family=\$\(sed -E 's/\^live-activity ledger: family=\(\[0-9a-f-\]\{36\}\) \.\*\$/\\1/' <<< "\$la_ledger"\)`)
	shape := liveLine(t, script, "the family's shape check",
		`\[\[ \$la_family =~ \^\[0-9a-f\]\{8\}-\[0-9a-f\]\{4\}-\[0-9a-f\]\{4\}-\[0-9a-f\]\{4\}-\[0-9a-f\]\{12\}\$ \]\] \|\| \{ echo "[^"]+" >&2; exit 1; \}`)
	phone := liveLine(t, script, "the phones' counts", `la_phone=\$\{la_ledger#live-activity ledger: family=\$la_family \}`)
	query := liveLine(t, script, "the Postgres ledger",
		`la_server=\$\(docker exec "\$db" psql -XAt -v ON_ERROR_STOP=1 -U postgres -d babytracker -c "SELECT`)
	for _, part := range []string{
		`  'registrations='\|\|\(SELECT count\(\*\) FROM live_activity_devices WHERE family_id='\$la_family' AND NOT revoked\)`,
		`  \|\|' sandbox='\|\|\(SELECT count\(\*\) FROM live_activity_devices WHERE family_id='\$la_family' AND environment='sandbox'\)`,
		`  \|\|' cards='\|\|\(SELECT count\(\*\) FROM live_activity_tokens WHERE family_id='\$la_family'\)`,
		`  \|\|' delivered='\|\|\(SELECT count\(\*\) FROM live_activity_history h JOIN live_activity_delivery d USING \(job_id\)` +
			` WHERE d\.family_id='\$la_family' AND h\.result='accepted'\)"\)`,
	} {
		if at := liveLine(t, script, "the Postgres ledger's "+part[:20], part); at < query {
			t.Fatalf("the Postgres ledger's %q is not part of its query", part)
		}
	}
	compare := liveLine(t, script, "the ledger comparison", `if \[\[ \$la_phone != "\$\{la_server%% delivered=\*\}" \]\]; then`)
	delivered := liveLine(t, script, "the delivered count", `la_delivered=\$\{la_server##\* delivered=\}`)
	atLeastOne := liveLine(t, script, "at least one accepted push",
		`\[\[ \$la_delivered =~ \^\[0-9\]\+\$ && \$la_delivered -ge 1 \]\] \|\| \{ echo "[^"]+" >&2; exit 1; \}`)
	tokens := liveLine(t, script, "the tokens line",
		`la_tokens=\$\(grep '\^live-activity tokens: ' "\$swift_log" \| tail -n 1 \| sed 's/\^live-activity tokens: //'\)`)
	appLog := liveLine(t, script, "the server log", `compose logs --no-color app > "\$la_app_log" 2>&1`)
	each := liveLine(t, script, "every token", `for token in \$la_tokens; do`)
	whole := liveLine(t, script, "a whole token", `  \[\[ \$token =~ \^\[0-9a-f\]\{32,\}\$ \]\] \|\| \{ echo "[^"]+" >&2; exit 1; \}`)
	leak := liveLine(t, script, "the leak check", `  if grep -qF "\$token" "\$la_app_log"; then`)
	summary := liveLine(t, script, "the summary", `echo "live-activity proof: .+"`)
	order := []int{swift, ledger, family, shape, phone, query, compare, delivered, atLeastOne, tokens, appLog, each, whole, leak, summary}
	for i := 1; i < len(order); i++ {
		if order[i] < order[i-1] {
			t.Fatalf("the Live Activity proof's step %d runs before step %d (offsets %v)", i, i-1, order)
		}
	}
	// The profile is on in both modes, so the proof is unconditional (main
	// guarded it, and the Swift switch, on BT_DISPOSABLE_LIVE_ACTIVITY).
	if strings.Contains(script, "BT_DISPOSABLE_LIVE_ACTIVITY:-") {
		t.Fatal("protocol e2e makes the Live Activity proof or the Swift push switch conditional")
	}
}

// The push profile composes the production APNs client against the
// identitytest recorder with a throwaway key and invented identifiers: no
// Apple credential, no Apple host. It is off unless asked for, and always
// off for performance runs; a composition's capability list must match it.
func TestDisposablePushProfileIsThrowawayAndOptional(t *testing.T) {
	raw, err := os.ReadFile("lib/disposable-sync.sh")
	if err != nil {
		t.Fatal(err)
	}
	helper := string(raw)
	gate := liveLine(t, helper, "the push profile gate",
		`if \[\[ \$\{BT_DISPOSABLE_PUSH:-0\} == 1 && \$\{BT_DISPOSABLE_PERFORMANCE:-0\} != 1 \]\]; then`)
	key := liveLine(t, helper, "the throwaway key",
		`  openssl ecparam -name prime256v1 -genkey -noout 2>/dev/null \| openssl pkcs8 -topk8 -nocrypt -out "\$BT_SECRETS_DIR/apns_key\.p8"`)
	mode := liveLine(t, helper, "the key's mode", `  chmod 0600 "\$BT_SECRETS_DIR/apns_key\.p8"`)
	if gate > key || key > mode {
		t.Fatal("the throwaway key is made outside the push profile")
	}
	for _, setting := range []string{
		`BT_APNS_WAKES_ENABLED: "true"`, `BT_APNS_TEAM_ID: E2ETEAM001`, `BT_APNS_KEY_ID: E2EKEY0001`,
		`BT_APNS_BUNDLE_ID: com.example.babytracker.protocol`, `BT_APNS_KEY_FILE: /run/secrets/apns_key`, `BT_IDENTITYTEST_APNS_SINK: "1"`,
	} {
		if at := liveLine(t, helper, setting, `    '      `+regexp.QuoteMeta(setting)+`' \\`); at < gate {
			t.Fatalf("%s is set outside the push profile", setting)
		}
	}
	liveLine(t, helper, "the push override in compose",
		`    docker compose --project-name "\$project" -f Server/compose\.yaml -f "\$override" \$\{push_override:\+-f "\$push_override"\} "\$@"`)
	check := liveLine(t, helper, "the capability read",
		`push_capability=\$\(curl --silent --fail --max-time 2 "\$base/v1/server" \| jq -r '\.capabilities \| index\("push-token-v1"\) != null'\)`)
	on := liveLine(t, helper, "push on advertised", `  \[\[ \$push_capability == true \]\] \|\| \{ echo "[^"]+" >&2; exit 1; \}`)
	off := liveLine(t, helper, "push off not advertised", `  \[\[ \$push_capability == false \]\] \|\| \{ echo "[^"]+" >&2; exit 1; \}`)
	if check < gate || on < check || off < check {
		t.Fatal("the composition does not check push-token-v1 against its profile after it starts")
	}

	// The Live Activity profile is the push profile plus
	// BT_LIVE_ACTIVITY_PUSH_ENABLED (an explicit false otherwise); asked for
	// without the push profile it is refused, not silently dropped; the
	// composition advertises live-activity-push-v1 exactly when it is on,
	// waiting for the worker's first pass; and it creates no worker login
	// or secret (main's disposable-sync.sh lines 225-261 are not ported).
	refuse := liveLine(t, helper, "the Live Activity refusal without push",
		`if \[\[ \$\{BT_DISPOSABLE_LIVE_ACTIVITY:-0\} == 1 && \$\{BT_DISPOSABLE_PUSH:-0\} != 1 \]\]; then`)
	off0 := liveLine(t, helper, "Live Activity off by default", `live_activity_push=false`)
	on1 := liveLine(t, helper, "Live Activity on with its profile",
		`  \[\[ \$\{BT_DISPOSABLE_LIVE_ACTIVITY:-0\} != 1 \]\] \|\| live_activity_push=true`)
	setting := liveLine(t, helper, "the Live Activity setting",
		`    "      BT_LIVE_ACTIVITY_PUSH_ENABLED: \\"\$live_activity_push\\"" \\`)
	if refuse > off0 || off0 > gate || on1 < gate || setting < on1 || strings.Count(helper, "live_activity_push=true") != 1 {
		t.Fatal("the Live Activity setting is decided outside the push profile")
	}
	poll := liveLine(t, helper, "the Live Activity capability read",
		`  live_activity_capability=\$\(curl --silent --fail --max-time 2 "\$base/v1/server" \| jq -r '\.capabilities \| index\("live-activity-push-v1"\) != null'\)`)
	settled := liveLine(t, helper, "the capability wait", `  \[\[ \$live_activity_capability != "\$live_activity_push" \]\] \|\| break`)
	laOn := liveLine(t, helper, "Live Activity on advertised", `  \[\[ \$live_activity_capability == true \]\] \|\| \{ echo "[^"]+" >&2; exit 1; \}`)
	laOff := liveLine(t, helper, "Live Activity off not advertised", `  \[\[ \$live_activity_capability == false \]\] \|\| \{ echo "[^"]+" >&2; exit 1; \}`)
	if poll < setting || settled < poll || laOn < settled || laOff < settled {
		t.Fatal("the composition does not check live-activity-push-v1 against its profile after it starts")
	}
	for _, retired := range []string{"live_activity_password", "live_activity_database_url", "BT_LIVE_ACTIVITY_DATABASE_URL_FILE", "baby_tracker_live_activity"} {
		if strings.Contains(helper, retired) {
			t.Fatalf("disposable composition still creates main's Live Activity worker credential: %q", retired)
		}
	}
	for _, forbidden := range []string{"push.apple.com", "AuthKey_", "BEGIN PRIVATE KEY", "BEGIN EC PRIVATE KEY"} {
		if strings.Contains(helper, forbidden) {
			t.Fatalf("disposable composition contains %q", forbidden)
		}
	}
}

// The iOS gate proves the live socket through the TLS front, so the front
// must pass the upgrade on, and the script must insist the step ran.
func TestProtocolE2EProxiesWebSocketUpgradesAndRequiresTheLiveStep(t *testing.T) {
	script, err := os.ReadFile("protocol-e2e.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, required := range []string{
		"source Server/scripts/lib/disposable-sync.sh",
		`start_disposable_sync "${BT_RECOVERY_E2E_MODE:-hosted}" normal`,
		"grep -q '^two-caregiver proof: step 24:'",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("protocol e2e omits %q", required)
		}
	}
	helper, err := os.ReadFile("lib/disposable-sync.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"map $http_upgrade $connection_upgrade {",
		"proxy_http_version 1.1;",
		"proxy_set_header Upgrade $http_upgrade;",
		"proxy_set_header Connection $connection_upgrade;",
	} {
		if !strings.Contains(string(helper), required) {
			t.Fatalf("disposable TLS setup omits %q", required)
		}
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryRunnerRequiresBothModesAndRealPhases(t *testing.T) {
	runner, err := os.ReadFile("refresh-recovery-e2e.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(runner), "for mode in hosted self_hosted") {
		t.Fatal("missing independent mode compositions")
	}
	phases, err := os.ReadFile("refresh-recovery-phases.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"--phase recovery-bootstrap", "--phase recovery-verify", "--phase recovery-reuse", "BABY_TRACKER_RECOVERY_PHASE=bootstrap", "BABY_TRACKER_RECOVERY_PHASE=verify", "refresh-recovery crash:", "refresh-recovery proof: completed", "FROM refresh_receipts", "revoked/refresh_reused", "has_table_privilege", "refresh-recovery-v1"} {
		if !strings.Contains(string(phases), required) {
			t.Fatalf("recovery runner omits %q", required)
		}
	}
	// Schema 29 has no recovery cleanup worker or legacy receipt table to wait on.
	for _, retired := range []string{"identitytest recovery cleanup completed", "FROM refresh_recovery_receipts", "event_data"} {
		if strings.Contains(string(phases), retired) {
			t.Fatalf("recovery runner still expects the retired %q", retired)
		}
	}
}
func TestRecoveryRunnerRejectsExternalDatabase(t *testing.T) {
	c := exec.CommandContext(t.Context(), "bash", "refresh-recovery-e2e.sh")
	c.Env = append(os.Environ(), "BABY_TRACKER_TEST_DATABASE_URL=postgres://external.invalid/do-not-use")
	if output, err := c.CombinedOutput(); err == nil || !strings.Contains(string(output), "refuses external database") {
		t.Fatalf("external DB not refused: %v %s", err, output)
	}
}

func TestRecoveryRunnerRejectsStubBeforeDelegation(t *testing.T) {
	for _, stubMode := range []string{"1", ""} {
		t.Run("stub="+stubMode, func(t *testing.T) {
			directory := t.TempDir()
			scripts := filepath.Join(directory, "Server", "scripts")
			if err := os.MkdirAll(scripts, 0o700); err != nil {
				t.Fatal(err)
			}
			runner, err := os.ReadFile("refresh-recovery-e2e.sh")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(scripts, "refresh-recovery-e2e.sh")
			writeExecutable(t, path, string(runner))
			calls := filepath.Join(directory, "calls")
			writeExecutable(t, filepath.Join(scripts, "protocol-e2e.sh"), "#!/bin/sh\nprintf '%s\\n' \"$BT_RECOVERY_E2E_MODE\" >> \"$BT_CAPTURE_ARGUMENTS\"\necho 'refresh-recovery proof: completed synthetic-delegation'\n")
			command := exec.CommandContext(t.Context(), "bash", path)
			command.Env = append(os.Environ(), "BT_PROTOCOL_E2E_STUB_MODE="+stubMode, "BABY_TRACKER_TEST_DATABASE_URL=", "BT_CAPTURE_ARGUMENTS="+calls)
			output, runErr := command.CombinedOutput()
			delegated, readErr := os.ReadFile(calls)
			if stubMode != "" {
				if runErr == nil || !strings.Contains(string(output), "recovery runner refuses stub mode") {
					t.Errorf("stub input not refused: err=%v output=%s", runErr, output)
				}
				if !os.IsNotExist(readErr) {
					t.Errorf("stub input invoked child: calls=%q err=%v", delegated, readErr)
				}
				if strings.Contains(string(output), "refresh-recovery mode") || strings.Contains(string(output), "refresh-recovery proof:") {
					t.Errorf("stub input emitted acceptance markers: %s", output)
				}
				return
			}
			if runErr != nil || readErr != nil || string(delegated) != "hosted\nself_hosted\n" {
				t.Fatalf("normal delegation changed: run=%v read=%v calls=%q output=%s", runErr, readErr, delegated, output)
			}
			for _, mode := range []string{"hosted", "self_hosted"} {
				if !strings.Contains(string(output), "refresh-recovery mode completed: "+mode) {
					t.Errorf("missing completed mode %s", mode)
				}
			}
		})
	}
}
