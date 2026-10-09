package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/recovery"
	"babytracker/server/internal/security"
	"babytracker/server/internal/security/localstore"
)

func TestCapabilitiesIsNonMutatingAndMachineReadable(t *testing.T) {
	var output bytes.Buffer
	if err := run(t.Context(), []string{"capabilities"}, func(string) string {
		t.Fatal("capability probe read configuration")
		return ""
	}, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "security-authority-v1\n" {
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

func TestOperatorCompositionRejectsHostedModeBeforeOpeningLocalAuthority(t *testing.T) {
	fixture := loadOperatorCLIFixture(t)
	directory := t.TempDir()
	policy := writeAdminSecret(t, directory, "ownership-policy", "owner-policy-test-v1")
	key := writeAdminSecret(t, directory, "confirmation-key", strings.Repeat("11", 32))
	values := map[string]string{
		"BT_MODE":                              "hosted",
		"BT_OPERATOR_ID":                       fixture.OperatorID,
		"BT_OPERATOR_OWNERSHIP_POLICY_ID_FILE": policy,
		"BT_OPERATOR_CONFIRMATION_KEY_FILE":    key,
		// Deliberately unusable local paths: hosted mode must fail before any
		// local authority store can be opened.
		"BT_SECURITY_LEDGER_DIR": filepath.Join(directory, "missing-ledger"),
		"BT_LEDGER_HEAD_DIR":     filepath.Join(directory, "missing-head"),
	}
	_, _, err := loadOperatorCommands(t.Context(), func(name string) string { return values[name] }, fixture.Examples[0].PrepareArgs)
	if err == nil || err.Error() != "hosted_adapter_unavailable" {
		t.Fatalf("hosted operator composition error=%v", err)
	}
}

func TestOperatorCommandsRequireRecoveryReconcileBeforeActivation(t *testing.T) {
	fixture := loadOperatorCLIFixture(t)
	service, _ := newOperatorCLIService(t, fixture)
	reconcileFailure := errors.New("recovery reconcile failed")
	gate := &fakeOperatorCommandGate{reconcileErr: reconcileFailure}
	if commands, err := activateOperatorCommands(t.Context(), gate, service); !errors.Is(err, reconcileFailure) || commands != nil || gate.reconciles != 1 {
		t.Fatalf("commands=%v reconciles=%d err=%v", commands, gate.reconciles, err)
	}

	gate = &fakeOperatorCommandGate{}
	commands, err := activateOperatorCommands(t.Context(), gate, service)
	if err != nil || commands == nil || gate.reconciles != 1 {
		t.Fatalf("commands=%v reconciles=%d err=%v", commands, gate.reconciles, err)
	}
	if commands != service {
		t.Fatal("activation replaced the typed operator service")
	}
}

type fakeOperatorCommandGate struct {
	reconciles, checks int
	reconcileErr       error
	checkErr           error
}

func (gate *fakeOperatorCommandGate) Reconcile(context.Context) error {
	gate.reconciles++
	return gate.reconcileErr
}

func (gate *fakeOperatorCommandGate) Check(context.Context) error {
	gate.checks++
	return gate.checkErr
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

func TestSecurityInitAndEpochRotation(t *testing.T) {
	ledger, head := t.TempDir(), t.TempDir()
	values := actorKeyAdminEnvironment(t, ledger, head)
	getenv := func(name string) string { return values[name] }
	const instance = "123e4567-e89b-42d3-a456-426614174000"
	var output bytes.Buffer
	if err := run(t.Context(), []string{"security", "init", "--instance-id", instance, "--epoch", "1", "--confirm-instance", instance}, getenv, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), instance) || !strings.Contains(output.String(), "epoch=1") {
		t.Fatalf("unsafe or incomplete init output: %q", output.String())
	}
	store, durable, err := openStores(getenv)
	if err != nil {
		t.Fatal(err)
	}
	actorKey, err := recovery.LoadActorIDPepperFile(values["BT_ACTOR_ID_PEPPER_FILE"])
	if err != nil {
		t.Fatal(err)
	}
	if err := durable.ValidateActorIDKey(t.Context(), instance, actorKey); err != nil {
		t.Fatalf("security init did not persist actor commitment: %v", err)
	}
	_ = store
	output.Reset()
	if err := run(t.Context(), []string{"recovery", "rotate-epoch", "--expected-epoch", "1", "--expected-revision", "0", "--reason-code", "database_restore", "--confirm-instance", instance}, getenv, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "epoch=2") || !strings.Contains(output.String(), "result=rotated") {
		t.Fatalf("rotation output: %q", output.String())
	}
	if err := run(t.Context(), []string{"recovery", "rotate-epoch", "--expected-epoch", "1", "--expected-revision", "0", "--reason-code", "database_restore", "--confirm-instance", instance}, getenv, &bytes.Buffer{}); err == nil {
		t.Fatal("stale epoch rotation was accepted")
	}
}

func TestSecurityInitPersistsActorCommitment(t *testing.T) {
	ledger, head := t.TempDir(), t.TempDir()
	values := actorKeyAdminEnvironment(t, ledger, head)
	getenv := func(name string) string { return values[name] }
	const instance = "123e4567-e89b-42d3-a456-426614174000"
	if err := run(t.Context(), []string{"security", "init", "--instance-id", instance, "--epoch", "1", "--confirm-instance", instance}, getenv, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	_, durable, err := openStores(getenv)
	if err != nil {
		t.Fatal(err)
	}
	key, err := recovery.LoadActorIDPepperFile(values["BT_ACTOR_ID_PEPPER_FILE"])
	if err != nil {
		t.Fatal(err)
	}
	if err := durable.ValidateActorIDKey(t.Context(), instance, key); err != nil {
		t.Fatal(err)
	}
}

func TestSecurityVerifyRequiresOriginalActorKeyAndSidecar(t *testing.T) {
	ledger, head := t.TempDir(), t.TempDir()
	values := actorKeyAdminEnvironment(t, ledger, head)
	getenv := func(name string) string { return values[name] }
	const instance = "123e4567-e89b-42d3-a456-426614174000"
	if err := run(t.Context(), []string{"security", "init", "--instance-id", instance, "--epoch", "1", "--confirm-instance", instance}, getenv, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"security", "verify"}, getenv, &bytes.Buffer{}); err != nil {
		t.Fatalf("valid actor key failed verification: %v", err)
	}

	sidecarPath := filepath.Join(head, "actor-id-key.json")
	originalSidecar, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	requireActorKeyFailure := func(name string) {
		t.Helper()
		if err := run(t.Context(), []string{"security", "verify"}, getenv, &bytes.Buffer{}); !errors.Is(err, recovery.ErrActorIDKeyMismatch) {
			t.Fatalf("%s verification error=%v", name, err)
		}
	}

	if err := os.WriteFile(sidecarPath, []byte(`{"version":"v1","instance_id":"123e4567-e89b-42d3-a456-426614174000","commitment":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	requireActorKeyFailure("replaced sidecar")
	if err := os.WriteFile(sidecarPath, originalSidecar, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sidecarPath); err != nil {
		t.Fatal(err)
	}
	requireActorKeyFailure("missing sidecar")
	if err := os.WriteFile(sidecarPath, originalSidecar, 0o600); err != nil {
		t.Fatal(err)
	}

	originalPepper := values["BT_ACTOR_ID_PEPPER_FILE"]
	values["BT_ACTOR_ID_PEPPER_FILE"] = filepath.Join(t.TempDir(), "missing-pepper")
	requireActorKeyFailure("missing pepper")
	replacementPepper := filepath.Join(t.TempDir(), "actor-id-pepper")
	if err := os.WriteFile(replacementPepper, []byte(strings.Repeat("f", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	values["BT_ACTOR_ID_PEPPER_FILE"] = replacementPepper
	requireActorKeyFailure("replaced pepper")
	values["BT_ACTOR_ID_PEPPER_FILE"] = originalPepper
	if err := run(t.Context(), []string{"security", "verify"}, getenv, &bytes.Buffer{}); err != nil {
		t.Fatalf("restored actor key failed verification: %v", err)
	}
}

func TestSecurityVerifyPreservesNonChainErrors(t *testing.T) {
	initialize := func(t *testing.T) (map[string]string, func(string) string, string) {
		t.Helper()
		ledger, head := t.TempDir(), t.TempDir()
		values := actorKeyAdminEnvironment(t, ledger, head)
		getenv := func(name string) string { return values[name] }
		const instance = "123e4567-e89b-42d3-a456-426614174000"
		if err := run(t.Context(), []string{"security", "init", "--instance-id", instance, "--epoch", "1", "--confirm-instance", instance}, getenv, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		return values, getenv, head
	}

	t.Run("canceled context", func(t *testing.T) {
		_, getenv, _ := initialize(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := run(ctx, []string{"security", "verify"}, getenv, &bytes.Buffer{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("missing recovery state", func(t *testing.T) {
		_, getenv, head := initialize(t)
		if err := os.Remove(filepath.Join(head, "identity.json")); err != nil {
			t.Fatal(err)
		}
		if err := run(t.Context(), []string{"security", "verify"}, getenv, &bytes.Buffer{}); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("corrupt recovery state", func(t *testing.T) {
		_, getenv, head := initialize(t)
		if err := os.WriteFile(filepath.Join(head, "identity.json"), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := run(t.Context(), []string{"security", "verify"}, getenv, &bytes.Buffer{}); err == nil || err.Error() != "recovery_state_corrupt" {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestSecurityInitRejectsMalformedIdentityWithoutArtifactsAndCanRetry(t *testing.T) {
	ledger, head := t.TempDir(), t.TempDir()
	values := actorKeyAdminEnvironment(t, ledger, head)
	getenv := func(name string) string { return values[name] }
	if err := run(t.Context(), []string{"security", "init", "--instance-id", "not-a-uuid", "--epoch", "1", "--confirm-instance", "not-a-uuid"}, getenv, &bytes.Buffer{}); err == nil {
		t.Fatal("malformed recovery identity initialized")
	}
	for _, path := range []string{filepath.Join(head, "head.json"), filepath.Join(head, "identity.json"), filepath.Join(head, "actor-id-key.json")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("failed initialization left artifact %s: %v", path, err)
		}
	}
	if matches, err := filepath.Glob(filepath.Join(ledger, "generation-*")); err != nil || len(matches) != 0 {
		t.Fatalf("failed initialization left ledger artifacts=%v err=%v", matches, err)
	}
	const instance = "123e4567-e89b-42d3-a456-426614174000"
	if err := run(t.Context(), []string{"security", "init", "--instance-id", instance, "--epoch", "1", "--confirm-instance", instance}, getenv, &bytes.Buffer{}); err != nil {
		t.Fatalf("corrected retry failed: %v", err)
	}
}

func TestSecurityInitRetriesAfterPartialGenesisWrite(t *testing.T) {
	ledger, head := t.TempDir(), t.TempDir()
	store, err := localstore.Open(ledger, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InitializeEmpty(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(head, "identity.json")); !os.IsNotExist(err) {
		t.Fatalf("partial fixture unexpectedly has identity: %v", err)
	}
	values := actorKeyAdminEnvironment(t, ledger, head)
	const instance = "123e4567-e89b-42d3-a456-426614174000"
	if err := run(t.Context(), []string{"security", "init", "--instance-id", instance, "--epoch", "1", "--confirm-instance", instance}, func(key string) string { return values[key] }, &bytes.Buffer{}); err != nil {
		t.Fatalf("partial-genesis retry failed: %v", err)
	}
	durable, err := recovery.NewStore(head, store.Fence)
	if err != nil {
		t.Fatal(err)
	}
	state, err := durable.Load(t.Context())
	if err != nil || state.InstanceID != instance || state.Epoch != 1 || state.TrustedHead != (security.Head{Generation: 1}) {
		t.Fatalf("recovered state=%+v err=%v", state, err)
	}
}

func TestSecurityInitRequiresActorPepperWithoutCreatingArtifacts(t *testing.T) {
	ledger, head := t.TempDir(), t.TempDir()
	values := map[string]string{"BT_SECURITY_LEDGER_DIR": ledger, "BT_LEDGER_HEAD_DIR": head}
	const instance = "123e4567-e89b-42d3-a456-426614174000"
	if err := run(t.Context(), []string{"security", "init", "--instance-id", instance, "--epoch", "1", "--confirm-instance", instance}, func(key string) string { return values[key] }, &bytes.Buffer{}); err == nil {
		t.Fatal("security init accepted a missing actor pepper")
	}
	for _, path := range []string{filepath.Join(head, "head.json"), filepath.Join(head, "identity.json"), filepath.Join(head, "actor-id-key.json")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("missing-secret init left artifact %s: %v", path, err)
		}
	}
}

func TestSecurityBindActorKeyOnlyOnEmptyOlderAuthority(t *testing.T) {
	ledger, head := t.TempDir(), t.TempDir()
	stores, err := localstore.Open(ledger, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.InitializeEmpty(t.Context()); err != nil {
		t.Fatal(err)
	}
	const instance = "123e4567-e89b-42d3-a456-426614174000"
	durable, err := recovery.NewVerifiedStore(head, stores.Fence, stores.Ledger, stores.Heads)
	if err != nil {
		t.Fatal(err)
	}
	if err := durable.Initialize(t.Context(), recovery.State{InstanceID: instance, Epoch: 1, TrustedHead: security.Head{Generation: 1}}); err != nil {
		t.Fatal(err)
	}
	values := actorKeyAdminEnvironment(t, ledger, head)
	getenv := func(key string) string { return values[key] }
	var output bytes.Buffer
	if err := run(t.Context(), []string{"security", "init", "--instance-id", instance, "--epoch", "1", "--confirm-instance", instance}, getenv, &bytes.Buffer{}); err == nil {
		t.Fatal("security init implicitly bound an accepted older authority")
	}
	if _, err := os.Stat(filepath.Join(head, "actor-id-key.json")); !os.IsNotExist(err) {
		t.Fatalf("security init created sidecar for older authority: %v", err)
	}
	if err := run(t.Context(), []string{"security", "bind-actor-key", "--instance-id", instance, "--confirm-instance", instance}, getenv, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "result=bound") {
		t.Fatalf("bind output=%q", output.String())
	}
	actorKey, err := recovery.LoadActorIDPepperFile(values["BT_ACTOR_ID_PEPPER_FILE"])
	if err != nil {
		t.Fatal(err)
	}
	if err := durable.ValidateActorIDKey(t.Context(), instance, actorKey); err != nil {
		t.Fatal(err)
	}
	if state, err := durable.Load(t.Context()); err != nil || state.InstanceID != instance || state.Epoch != 1 {
		t.Fatalf("older identity changed: state=%+v err=%v", state, err)
	}
}

func TestOlderBinaryIgnoresSidecarButCannotRewriteIt(t *testing.T) {
	ledger, head := t.TempDir(), t.TempDir()
	values := actorKeyAdminEnvironment(t, ledger, head)
	getenv := func(key string) string { return values[key] }
	const instance = "123e4567-e89b-42d3-a456-426614174000"
	arguments := []string{"security", "init", "--instance-id", instance, "--epoch", "1", "--confirm-instance", instance}
	if err := run(t.Context(), arguments, getenv, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	_, durable, err := openStores(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := durable.Load(t.Context()); err != nil {
		t.Fatalf("legacy identity reader rejected sidecar: %v", err)
	}
	replacement := filepath.Join(t.TempDir(), "replacement")
	if err := os.WriteFile(replacement, []byte(strings.Repeat("f", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacementKey, err := recovery.LoadActorIDPepperFile(replacement)
	if err != nil {
		t.Fatal(err)
	}
	if err := durable.ValidateActorIDKey(t.Context(), instance, replacementKey); err == nil || !strings.Contains(err.Error(), "actor_id_key_mismatch") {
		t.Fatalf("replacement init error=%v", err)
	}
}

func actorKeyAdminEnvironment(t *testing.T, ledger, head string) map[string]string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "actor-id-pepper")
	if err := os.WriteFile(path, []byte("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"BT_SECURITY_LEDGER_DIR":  ledger,
		"BT_LEDGER_HEAD_DIR":      head,
		"BT_ACTOR_ID_PEPPER_FILE": path,
	}
}

func TestEpochRotationRejectsUnsafeIntegerFlagsBeforeOpeningStores(t *testing.T) {
	const instance = "123e4567-e89b-42d3-a456-426614174000"
	for _, arguments := range [][]string{
		{"recovery", "rotate-epoch", "--expected-epoch", "9007199254740992", "--expected-revision", "0", "--reason-code", "database_restore", "--confirm-instance", instance},
		{"recovery", "rotate-epoch", "--expected-epoch", "1", "--expected-revision", "9007199254740992", "--reason-code", "database_restore", "--confirm-instance", instance},
	} {
		if err := run(t.Context(), arguments, func(string) string { t.Fatal("unsafe flags reached durable stores"); return "" }, &bytes.Buffer{}); err == nil {
			t.Fatalf("unsafe integer flags accepted: %v", arguments)
		}
	}
}

func TestSecurityVerifyWalksCommittedDescendant(t *testing.T) {
	ledger, head := t.TempDir(), t.TempDir()
	values := actorKeyAdminEnvironment(t, ledger, head)
	getenv := func(name string) string { return values[name] }
	store, err := localstore.Open(ledger, head)
	if err != nil {
		t.Fatal(err)
	}
	durable, err := recovery.NewVerifiedStore(head, store.Fence, store.Ledger, store.Heads)
	if err != nil {
		t.Fatal(err)
	}
	instance := "123e4567-e89b-42d3-a456-426614174000"
	actorKey, err := recovery.LoadActorIDPepperFile(values["BT_ACTOR_ID_PEPPER_FILE"])
	if err != nil {
		t.Fatal(err)
	}
	if err := durable.InitializeAuthorityWithActorIDKey(t.Context(), store, recovery.State{InstanceID: instance, Epoch: 1, TrustedHead: security.Head{Generation: 1}}, actorKey); err != nil {
		t.Fatal(err)
	}
	event := security.Event{Version: "v1", Generation: 1, Revision: 1, PreviousHash: strings.Repeat("0", 64),
		CommandID: "30000000-0000-4000-8000-000000000001", CommandHash: strings.Repeat("a", 64),
		ActorAccountID: "10000000-0000-4000-8000-000000000001", Kind: "account_registered", At: "2026-09-20T12:00:00Z",
		Effects: []security.Effect{{Kind: "account_registered", AccountID: "10000000-0000-4000-8000-000000000001", IdentityKey: "opaque", Status: "active"}}}
	encoded, err := security.EncodeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	candidate := security.Candidate{CommandID: event.CommandID, Prior: security.Head{Generation: 1}, EventJSON: encoded, EventHash: sha256.Sum256(encoded)}
	if err := store.Ledger.AppendCandidate(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	live := security.Head{Generation: 1, Revision: 1, EventHash: candidate.EventHash}
	if swapped, err := store.Heads.CompareAndSwap(t.Context(), candidate.Prior, live); err != nil || !swapped {
		t.Fatalf("commit descendant swapped=%v err=%v", swapped, err)
	}
	var output bytes.Buffer
	if err := run(t.Context(), []string{"security", "verify"}, getenv, &output); err != nil {
		t.Fatalf("valid descendant rejected: %v", err)
	}
	if !strings.Contains(output.String(), "revision=1 result=verified") {
		t.Fatalf("verification output=%q", output.String())
	}
	output.Reset()
	if err := run(t.Context(), []string{"recovery", "rotate-epoch", "--expected-epoch", "1", "--expected-revision", "1", "--reason-code", "database_restore", "--confirm-instance", instance}, getenv, &output); err != nil {
		t.Fatalf("verified descendant rotation failed: %v", err)
	}
	rotated, err := durable.Load(t.Context())
	if err != nil || rotated.Epoch != 2 || rotated.TrustedHead != live {
		t.Fatalf("rotated manifest=%+v err=%v", rotated, err)
	}

	files, err := filepath.Glob(filepath.Join(ledger, "generation-*", "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("ledger files=%v err=%v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(data, []byte("opaque"), []byte("broken"), 1)
	if err := os.WriteFile(files[0], changed, 0o600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := run(t.Context(), []string{"security", "verify"}, getenv, &output); err == nil || err.Error() != "trusted recovery manifest mismatch" || output.Len() != 0 {
		t.Fatalf("corrupt committed event verified: err=%v output=%q hash=%s", err, output.String(), hex.EncodeToString(candidate.EventHash[:]))
	}
	if err := run(t.Context(), []string{"recovery", "rotate-epoch", "--expected-epoch", "2", "--expected-revision", "1", "--reason-code", "database_restore", "--confirm-instance", instance}, getenv, &bytes.Buffer{}); err == nil {
		t.Fatal("corrupt committed descendant rotated recovery epoch")
	}
	unchanged, err := durable.Load(t.Context())
	if err != nil || unchanged.Epoch != 2 || unchanged.TrustedHead != live {
		t.Fatalf("corrupt rotation changed manifest=%+v err=%v", unchanged, err)
	}
	if err := os.WriteFile(files[0], data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"security", "verify"}, getenv, &bytes.Buffer{}); err == nil {
		t.Fatal("missing/gapped committed event verified")
	}
	if err := os.WriteFile(files[0], data, 0o600); err != nil {
		t.Fatal(err)
	}
	headPath := filepath.Join(head, "head.json")
	headData, err := os.ReadFile(headPath)
	if err != nil {
		t.Fatal(err)
	}
	generationMismatch := []byte(`{"generation":2,"revision":1,"event_hash":"` + hex.EncodeToString(candidate.EventHash[:]) + `"}` + "\n")
	if err := os.WriteFile(headPath, generationMismatch, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"security", "verify"}, getenv, &bytes.Buffer{}); err == nil {
		t.Fatal("generation-mismatched chain verified")
	}
	if err := os.WriteFile(headPath, headData, 0o600); err != nil {
		t.Fatal(err)
	}
	identityPath := filepath.Join(head, "identity.json")
	identityData, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(identityPath); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"security", "verify"}, getenv, &bytes.Buffer{}); err == nil {
		t.Fatal("missing trusted manifest verified")
	}
	if err := os.WriteFile(identityPath, identityData, 0o600); err != nil {
		t.Fatal(err)
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
