package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"babytracker/server/internal/testpg"
)

func TestCapabilitiesIsNonMutatingAndMachineReadable(t *testing.T) {
	var output bytes.Buffer
	if err := run(t.Context(), []string{"capabilities"}, func(string) string {
		t.Fatal("capability probe read configuration")
		return ""
	}, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "postgres-authority-v1\n" {
		t.Fatalf("capabilities=%q", output.String())
	}
}

func TestRunRoutesOperatorCommandsThroughInjectedComposition(t *testing.T) {
	fixture := loadOperatorCLIFixture(t)
	service, _ := newOperatorCLIService(t, fixture)
	loaded, cleaned := 0, 0
	loader := func(context.Context, func(string) string, []string) (OperatorCommands, func(), error) {
		loaded++
		return service, func() { cleaned++ }, nil
	}
	var output bytes.Buffer
	if err := runWithOperatorLoader(t.Context(), fixture.Examples[0].PrepareArgs, func(string) string { return "" }, &output, loader); err != nil {
		t.Fatal(err)
	}
	if loaded != 1 || cleaned != 1 || !strings.Contains(output.String(), fixture.Examples[0].ExpectedDigest) {
		t.Fatalf("loaded=%d cleaned=%d output=%q", loaded, cleaned, output.String())
	}
}

func TestOperatorCommandWithoutConfiguredPolicyFailsBeforeInfrastructure(t *testing.T) {
	fixture := loadOperatorCLIFixture(t)
	err := run(t.Context(), fixture.Examples[0].PrepareArgs, func(string) string { return "" }, &bytes.Buffer{})
	if err == nil || err.Error() != "operator_policy_required" {
		t.Fatalf("error=%v", err)
	}
}

func TestOperatorCompositionRejectsDirectSecretWithoutDisclosure(t *testing.T) {
	fixture := loadOperatorCLIFixture(t)
	const secret = "do-not-disclose-operator-secret"
	values := map[string]string{"BT_OPERATOR_CONFIRMATION_KEY": secret}
	err := run(t.Context(), fixture.Examples[0].PrepareArgs, func(name string) string { return values[name] }, &bytes.Buffer{})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe error=%v", err)
	}
}

func TestOperatorCompositionRejectsHostedModeBeforeOpeningTheDatabase(t *testing.T) {
	fixture := loadOperatorCLIFixture(t)
	directory := t.TempDir()
	policy := writeAdminSecret(t, directory, "ownership-policy", "owner-policy-test-v1")
	key := writeAdminSecret(t, directory, "confirmation-key", strings.Repeat("11", 32))
	values := map[string]string{
		"BT_MODE":                              "hosted",
		"BT_OPERATOR_ID":                       fixture.OperatorID,
		"BT_OPERATOR_OWNERSHIP_POLICY_ID_FILE": policy,
		"BT_OPERATOR_CONFIRMATION_KEY_FILE":    key,
		// Deliberately unusable: hosted mode must fail before any database
		// is opened.
		"BT_DATABASE_URL": "postgres://unused@127.0.0.1:1/unused",
	}
	_, _, err := loadOperatorCommands(t.Context(), func(name string) string { return values[name] }, fixture.Examples[0].PrepareArgs)
	if err == nil || err.Error() != "hosted_adapter_unavailable" {
		t.Fatalf("hosted operator composition error=%v", err)
	}
}

func writeAdminSecret(t *testing.T, directory, name, value string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// adminEnvironment is the migrate service's environment: the migrator
// credential and the actor-ID key file.
func adminEnvironment(t *testing.T, databaseURL string, actorKey byte) map[string]string {
	t.Helper()
	directory := t.TempDir()
	pepper := writeAdminSecret(t, directory, "actor-id-pepper", strings.Repeat(fmt.Sprintf("%02x", actorKey), 32))
	return map[string]string{"BT_DATABASE_URL": databaseURL, "BT_ACTOR_ID_PEPPER_FILE": pepper, "BT_MODE": "self_hosted"}
}

func runAdmin(t *testing.T, values map[string]string, arguments ...string) (string, error) {
	t.Helper()
	var output bytes.Buffer
	err := run(t.Context(), arguments, func(name string) string { return values[name] }, &output)
	return output.String(), err
}

func actorKeyOf(value byte) [32]byte {
	var key [32]byte
	for index := range key {
		key[index] = value
	}
	return key
}

func TestAuthorityInitBindsServerStateOnce(t *testing.T) {
	f := testpg.New(t)
	const instance = "123e4567-e89b-42d3-a456-426614174000"
	values := adminEnvironment(t, f.Owner.Config().ConnString(), 0x05)
	for name, arguments := range map[string][]string{
		"epoch 2":              {"authority", "init", "--instance-id", instance, "--epoch", "2", "--confirm-instance", instance},
		"unconfirmed":          {"authority", "init", "--instance-id", instance, "--epoch", "1", "--confirm-instance", "223e4567-e89b-42d3-a456-426614174000"},
		"uppercase instance":   {"authority", "init", "--instance-id", strings.ToUpper(instance), "--epoch", "1", "--confirm-instance", strings.ToUpper(instance)},
		"nil instance":         {"authority", "init", "--instance-id", "00000000-0000-0000-0000-000000000000", "--epoch", "1", "--confirm-instance", "00000000-0000-0000-0000-000000000000"},
		"extra argument":       {"authority", "init", "--instance-id", instance, "--epoch", "1", "--confirm-instance", instance, "extra"},
		"unknown subcommand":   {"authority", "bind-actor-key"},
		"retired security":     {"security", "init", "--instance-id", instance, "--epoch", "1", "--confirm-instance", instance},
		"retired reconcile":    {"security", "reconcile"},
		"retired epoch rotate": {"recovery", "rotate-epoch", "--expected-epoch", "1", "--reason-code", "database_restore", "--confirm-instance", instance},
	} {
		if _, err := runAdmin(t, values, arguments...); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	var rows int
	if err := f.Owner.QueryRow(t.Context(), "SELECT count(*) FROM server_state").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("refused init wrote server_state: rows=%d err=%v", rows, err)
	}
	init := []string{"authority", "init", "--instance-id", instance, "--epoch", "1", "--confirm-instance", instance}
	if output, err := runAdmin(t, values, init...); err != nil || output != "instance_id="+instance+" epoch=1 result=initialized\n" {
		t.Fatalf("init output=%q err=%v", output, err)
	}
	if output, err := runAdmin(t, values, init...); err != nil || output != "instance_id="+instance+" epoch=1 result=unchanged\n" {
		t.Fatalf("idempotent init output=%q err=%v", output, err)
	}
	other := "223e4567-e89b-42d3-a456-426614174000"
	if _, err := runAdmin(t, values, "authority", "init", "--instance-id", other, "--epoch", "1", "--confirm-instance", other); err == nil ||
		err.Error() != "server_identity_mismatch" {
		t.Fatalf("re-identifying init err=%v", err)
	}
	if _, err := runAdmin(t, adminEnvironment(t, f.Owner.Config().ConnString(), 0x06), init...); err == nil || err.Error() != "actor_id_key_mismatch" {
		t.Fatalf("re-keying init err=%v", err)
	}
	state, err := authority.LoadServerState(t.Context(), f.Runtime)
	if err != nil || state.Check(instance, 1, actorKeyOf(0x05)) != nil {
		t.Fatalf("server_state=%+v err=%v", state, err)
	}
	// The runtime login can read server_state but never write it.
	if _, err := f.Runtime.Exec(t.Context(), "UPDATE server_state SET recovery_epoch=2"); err == nil {
		t.Fatal("runtime rewrote server_state")
	}
	// No pepper, no init.
	delete(values, "BT_ACTOR_ID_PEPPER_FILE")
	if _, err := runAdmin(t, values, init...); err == nil {
		t.Fatal("init without the actor-ID key accepted")
	}
}

// A server_state row a fixture (or an older install) wrote without a
// commitment is bound by init, once.
func TestAuthorityInitBindsCommitmentOfMatchingRow(t *testing.T) {
	f := authoritytest.New(t)
	values := adminEnvironment(t, f.DB.Owner.Config().ConnString(), 0x07)
	init := []string{"authority", "init", "--instance-id", f.InstanceID, "--epoch", "1", "--confirm-instance", f.InstanceID}
	if output, err := runAdmin(t, values, init...); err != nil || !strings.HasSuffix(output, "result=bound\n") {
		t.Fatalf("bind output=%q err=%v", output, err)
	}
	if output, err := runAdmin(t, values, init...); err != nil || !strings.HasSuffix(output, "result=unchanged\n") {
		t.Fatalf("rebind output=%q err=%v", output, err)
	}
}

func TestAuthorityVerifyChecksActorKeyAndEveryChain(t *testing.T) {
	f := authoritytest.New(t)
	values := adminEnvironment(t, f.DB.Owner.Config().ConnString(), 0x05)
	// The fixture's server_state row has no commitment yet: unbound.
	if _, err := runAdmin(t, values, "authority", "verify"); err == nil || err.Error() != "actor_id_key_mismatch" {
		t.Fatalf("verify before init: %v", err)
	}
	if _, err := runAdmin(t, values, "authority", "init", "--instance-id", f.InstanceID, "--epoch", "1", "--confirm-instance", f.InstanceID); err != nil {
		t.Fatal(err)
	}
	// Two accounts with real audit rows (one each, from a family command).
	owner := f.Account(t)
	device := f.Device(t, owner)
	session, _, _ := f.Session(t, owner, device, "account", "", "")
	principal := access.Principal{AccountID: owner, DeviceID: device, SessionID: session, RecoveryEpoch: authoritytest.RecoveryEpoch, SecurityRevision: 1, Scope: "account", SessionScope: "account"}
	created, err := f.Service.CreateFamily(t.Context(), authority.CreateFamily{CommandID: authoritytest.NewID(t), OwnerAccountID: owner, Principal: principal, FamilyID: authoritytest.NewID(t), DisplayName: "Family"})
	if err != nil {
		t.Fatal(err)
	}
	output, err := runAdmin(t, values, "authority", "verify")
	if err != nil || !strings.HasPrefix(output, "instance_id="+f.InstanceID+" epoch=1 ") || !strings.HasSuffix(output, " failed=0 result=verified\n") {
		t.Fatalf("verify output=%q err=%v", output, err)
	}
	// A replaced actor-ID key is refused before any chain is read.
	if _, err := runAdmin(t, adminEnvironment(t, f.DB.Owner.Config().ConnString(), 0x09), "authority", "verify"); err == nil || err.Error() != "actor_id_key_mismatch" {
		t.Fatalf("replaced pepper verify err=%v", err)
	}
	// A configured instance or epoch other than server_state's is refused.
	values["BT_RECOVERY_EPOCH"] = "2"
	if _, err := runAdmin(t, values, "authority", "verify"); err == nil || err.Error() != "server_identity_mismatch" {
		t.Fatalf("other epoch verify err=%v", err)
	}
	delete(values, "BT_RECOVERY_EPOCH")
	// A tampered audit row fails its own scope closed and is named.
	f.Exec(t, `UPDATE security_audit_events SET payload = payload || '{"tampered":true}'::jsonb WHERE scope='family' AND scope_id=$1`, created.FamilyID)
	output, err = runAdmin(t, values, "authority", "verify")
	if err == nil || !strings.Contains(err.Error(), "1 scopes failed closed") ||
		!strings.Contains(output, "failed scope=family id="+created.FamilyID+"\n") || !strings.HasSuffix(output, " failed=1 result=failed\n") {
		t.Fatalf("tampered verify output=%q err=%v", output, err)
	}
	if failed := f.Exec(t, `UPDATE families SET updated_at=updated_at WHERE id=$1 AND chain_failed`, created.FamilyID); failed != 1 {
		t.Fatal("tampered family not failed closed")
	}
	if _, err := runAdmin(t, values, "authority", "verify", "--all"); err == nil {
		t.Fatal("verify accepted a flag")
	}
}

// Epoch rotation after a database restore is monotonic, confirmed and
// fenced by the expected epoch; every session issued before it then fails
// epoch_changed.
func TestAuthorityRotateEpochIsMonotonicAndInvalidatesSessions(t *testing.T) {
	f := authoritytest.New(t)
	values := adminEnvironment(t, f.DB.Owner.Config().ConnString(), 0x05)
	if _, err := runAdmin(t, values, "authority", "init", "--instance-id", f.InstanceID, "--epoch", "1", "--confirm-instance", f.InstanceID); err != nil {
		t.Fatal(err)
	}
	account := f.Account(t)
	device := f.Device(t, account)
	_, access1, _ := f.Session(t, account, device, "account", "", "")
	rotate := func(expected string, extra ...string) (string, error) {
		arguments := append([]string{"authority", "rotate-epoch", "--expected-epoch", expected, "--reason-code", "database_restore", "--confirm-instance", f.InstanceID}, extra...)
		return runAdmin(t, values, arguments...)
	}
	for name, arguments := range map[string][]string{
		"wrong reason":     {"authority", "rotate-epoch", "--expected-epoch", "1", "--reason-code", "operator_choice", "--confirm-instance", f.InstanceID},
		"unsafe epoch":     {"authority", "rotate-epoch", "--expected-epoch", "9007199254740991", "--reason-code", "database_restore", "--confirm-instance", f.InstanceID},
		"zero epoch":       {"authority", "rotate-epoch", "--expected-epoch", "0", "--reason-code", "database_restore", "--confirm-instance", f.InstanceID},
		"missing instance": {"authority", "rotate-epoch", "--expected-epoch", "1", "--reason-code", "database_restore"},
		"retired revision": {"authority", "rotate-epoch", "--expected-epoch", "1", "--expected-revision", "0", "--reason-code", "database_restore", "--confirm-instance", f.InstanceID},
		"another instance": {"authority", "rotate-epoch", "--expected-epoch", "1", "--reason-code", "database_restore", "--confirm-instance", "223e4567-e89b-42d3-a456-426614174000"},
	} {
		if _, err := runAdmin(t, values, arguments...); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if output, err := rotate("1"); err != nil || output != "instance_id="+f.InstanceID+" epoch=2 result=rotated\n" {
		t.Fatalf("rotate output=%q err=%v", output, err)
	}
	if _, err := rotate("1"); err == nil || err.Error() != "server_identity_mismatch" {
		t.Fatalf("stale rotation err=%v", err)
	}
	if output, err := rotate("2"); err != nil || !strings.Contains(output, " epoch=3 ") {
		t.Fatalf("second rotation output=%q err=%v", output, err)
	}
	state, err := authority.LoadServerState(t.Context(), f.Pool)
	if err != nil || state.RecoveryEpoch != 3 || state.Check(f.InstanceID, 3, actorKeyOf(0x05)) != nil {
		t.Fatalf("rotated state=%+v err=%v", state, err)
	}
	// The node restarted with the rotated epoch refuses the old session.
	rotated, err := authority.New(authority.Options{Pool: f.Pool, Now: func() time.Time { return f.Now }, InstanceID: f.InstanceID, RecoveryEpoch: 3,
		TokenKeys: f.TokenKeys, Allowed: authoritytest.Allowed})
	if err != nil {
		t.Fatal(err)
	}
	digest := authoritytest.TokenDigest(f.TokenKeys.Current, "access", access1)
	if _, err := rotated.Authenticate(t.Context(), []authority.KeyedDigest{{KeyID: f.TokenKeys.Current.ID, Sum: digest}}); authority.CodeOf(err) != "epoch_changed" {
		t.Fatalf("pre-rotation session after rotation: %v", err)
	}
}

func TestAuthorityFamilyRevisionRunsBehindTheReadinessGate(t *testing.T) {
	f := authoritytest.New(t)
	if _, err := runAdmin(t, adminEnvironment(t, f.DB.Owner.Config().ConnString(), 0x05),
		"authority", "init", "--instance-id", f.InstanceID, "--epoch", "1", "--confirm-instance", f.InstanceID); err != nil {
		t.Fatal(err)
	}
	owner := f.Account(t)
	family := f.Family(t, owner)
	f.Exec(t, `UPDATE families SET audit_seq=41 WHERE id=$1`, family)
	values := operatorRuntimeEnvironment(t, f.Pool.Config().ConnString(), f.InstanceID, 0x05)
	output, err := runAdmin(t, values, "authority", "family-revision", "--family-id", family)
	if err != nil || output != "family_id="+family+" revision=41\n" {
		t.Fatalf("family revision output=%q err=%v", output, err)
	}
	if _, err := runAdmin(t, values, "authority", "family-revision"); err == nil {
		t.Fatal("family revision without --family-id accepted")
	}
	// A node that is not ready (another actor-ID key) answers nothing.
	if _, err := runAdmin(t, operatorRuntimeEnvironment(t, f.Pool.Config().ConnString(), f.InstanceID, 0x09),
		"authority", "family-revision", "--family-id", family); err == nil || err.Error() != "actor_id_key_mismatch" {
		t.Fatalf("unready family revision err=%v", err)
	}
	if _, err := runAdmin(t, values, "authority", "family-revision", "--family-id", authoritytest.NewID(t)); err == nil {
		t.Fatal("unknown family revision answered")
	}
}

// operatorRuntimeEnvironment is the server's runtime configuration in
// development mode (direct key values), as operator commands load it.
func operatorRuntimeEnvironment(t *testing.T, runtimeURL, instance string, actorKey byte) map[string]string {
	t.Helper()
	directory := t.TempDir()
	return map[string]string{
		"BT_MODE": "development", "BT_DATABASE_URL": runtimeURL, "BT_PUBLIC_ORIGIN": "https://sync.example.test",
		"BT_SERVER_INSTANCE_ID": instance, "BT_RECOVERY_EPOCH": "1", "BT_APPLE_AUDIENCE": "test", "BT_TRUSTED_PROXY_CIDRS": "127.0.0.1/32",
		"BT_SESSION_SIGNING_KEY_FILE": writeAdminSecret(t, directory, "session", strings.Repeat("02", 32)),
		"BT_IDENTITY_KEY_ID":          "identity.v1", "BT_IDENTITY_PEPPER": strings.Repeat("01", 32),
		"BT_SESSION_TOKEN_KEY_ID":     "session.v1",
		"BT_DEVICE_CONTINUITY_KEY_ID": "device.v1", "BT_DEVICE_CONTINUITY_PEPPER": strings.Repeat("03", 32),
		"BT_APPLE_NOTIFICATION_KEY_ID": "notification.v1", "BT_APPLE_NOTIFICATION_PEPPER": strings.Repeat("04", 32),
		"BT_INVITATION_KEY_ID": "invitation.v1", "BT_INVITATION_PEPPER": strings.Repeat("07", 32),
		"BT_PAGE_TOKEN_KEY_ID": "page.v1", "BT_PAGE_TOKEN_KEY_FILE": writeAdminSecret(t, directory, "page", strings.Repeat("08", 32)),
		"BT_ACTOR_ID_PEPPER": strings.Repeat(fmt.Sprintf("%02x", actorKey), 32), "BT_RATE_LIMIT_PEPPER": strings.Repeat("06", 32),
		// The server's configuration always names the maintenance
		// credential (decision D3); the operator runtime never opens it.
		"BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE": writeAdminSecret(t, directory, "maintenance-database-url", "postgres://maintenance@127.0.0.1:1/unused"),
	}
}

func TestAdminProcess(t *testing.T) {
	if os.Getenv("BT_TEST_ADMIN_PROCESS") != "1" {
		return
	}
	os.Args = []string{os.Args[0], "migrate"}
	main()
	os.Exit(0)
}

func TestAdminUnavailableDatabaseFailsPromptly(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAdminProcess$")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "BT_") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "BT_TEST_ADMIN_PROCESS=1", "BT_DATABASE_URL=postgres://test:private-test-password@"+listener.Addr().String()+"/test?sslmode=disable")
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("admin hung on an unavailable database instead of failing promptly")
	}
	if err == nil {
		t.Fatal("admin accepted unavailable database")
	}
	if string(output) != "open database: database_unavailable\n" {
		t.Fatalf("unexpected/unsafe admin error: %s", output)
	}
}

// migrate and the authority commands refuse an environment written for the
// file ledger, naming each retired setting, before opening any database.
func TestMigrateAndAuthorityCommandsRejectRetiredSettings(t *testing.T) {
	const instance = "123e4567-e89b-42d3-a456-426614174000"
	for _, retired := range []string{"BT_SECURITY_LEDGER_DIR", "BT_LEDGER_HEAD_DIR", "BT_PROJECTOR_DATABASE_URL_FILE", "BT_HOSTED_PROVIDER_ID"} {
		values := adminEnvironment(t, "postgres://unused@127.0.0.1:1/unused", 0x05)
		values[retired] = "/var/lib/babytracker/retired"
		for _, arguments := range [][]string{
			{"migrate"},
			{"authority", "init", "--instance-id", instance, "--epoch", "1", "--confirm-instance", instance},
			{"authority", "verify"},
			{"authority", "rotate-epoch", "--expected-epoch", "1", "--reason-code", "database_restore", "--confirm-instance", instance},
			{"authority", "family-revision", "--family-id", instance},
		} {
			_, err := runAdmin(t, values, arguments...)
			if err == nil || !strings.Contains(err.Error(), retired+" is retired: PostgreSQL is the only security authority") {
				t.Fatalf("%v with %s: %v", arguments, retired, err)
			}
		}
	}
}
