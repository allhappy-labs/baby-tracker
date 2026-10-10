package operator_test

import (
	"context"
	"errors"
	"testing"
	"time"

	core "babytracker/server/internal/authority"
	"babytracker/server/internal/operator"
)

const (
	testOperatorID = "90000000-0000-4000-8000-000000000001"
	testFamilyID   = "20000000-0000-4000-8000-000000000001"
	testOldOwnerID = "10000000-0000-4000-8000-000000000001"
	testNewOwnerID = "10000000-0000-4000-8000-000000000002"
)

var testNow = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

type fakeAuthority struct {
	recoveryChecks, entitlementChecks int
	recoveryCommands                  []operator.RecoveryCommand
	entitlementCommands               []operator.EntitlementCommand
	receipts                          map[string]fakeReceipt
}

type fakeReceipt struct {
	key    operator.ReceiptKey
	result core.Result
}

type expiringAuthority struct {
	fakeAuthority
	entered chan struct{}
	release chan struct{}
	seen    chan time.Time
	now     func() time.Time
}

func (authority *expiringAuthority) RecoverOwner(ctx context.Context, command operator.RecoveryCommand) (core.Result, error) {
	authority.seen <- command.ConfirmationExpiresAt
	close(authority.entered)
	select {
	case <-authority.release:
	case <-ctx.Done():
		return core.Result{}, ctx.Err()
	}
	// This comparison represents the lifecycle adapter's trusted clock check
	// while it owns the exclusive authority fence.
	if !authority.now().Before(command.ConfirmationExpiresAt) {
		return core.Result{}, operator.ErrInvalidConfirmation
	}
	return authority.fakeAuthority.RecoverOwner(ctx, command)
}

func (authority *fakeAuthority) PreflightRecovery(context.Context, operator.RecoveryCommand) error {
	authority.recoveryChecks++
	return nil
}

func (authority *fakeAuthority) PreflightEntitlement(context.Context, operator.EntitlementCommand) error {
	authority.entitlementChecks++
	return nil
}

func (authority *fakeAuthority) ReplayRecovery(_ context.Context, command operator.RecoveryCommand) (core.Result, bool, error) {
	return authority.replay(command.Receipt)
}

func (authority *fakeAuthority) ReplayEntitlement(_ context.Context, command operator.EntitlementCommand) (core.Result, bool, error) {
	return authority.replay(command.Receipt)
}

func (authority *fakeAuthority) replay(key operator.ReceiptKey) (core.Result, bool, error) {
	receipt, ok := authority.receipts[key.CommandID]
	if !ok {
		return core.Result{}, false, nil
	}
	if receipt.key != key {
		return core.Result{}, false, operator.ErrCommandIDReused
	}
	return receipt.result, true, nil
}

func (authority *fakeAuthority) RecoverOwner(_ context.Context, command operator.RecoveryCommand) (core.Result, error) {
	authority.recoveryCommands = append(authority.recoveryCommands, command)
	result := core.Result{CommandID: command.Proposal.CommandID, Outcome: "committed", Code: "ok", Revision: command.Proposal.ExpectedRevision + 1}
	authority.receipts[command.Proposal.CommandID] = fakeReceipt{key: command.Receipt, result: result}
	return result, nil
}

func (authority *fakeAuthority) GrantEntitlement(_ context.Context, command operator.EntitlementCommand) (core.Result, error) {
	return authority.commitEntitlement(command)
}

func (authority *fakeAuthority) SuspendEntitlement(_ context.Context, command operator.EntitlementCommand) (core.Result, error) {
	return authority.commitEntitlement(command)
}

func (authority *fakeAuthority) commitEntitlement(command operator.EntitlementCommand) (core.Result, error) {
	authority.entitlementCommands = append(authority.entitlementCommands, command)
	result := core.Result{CommandID: command.Proposal.CommandID, Outcome: "committed", Code: "ok", Revision: command.Proposal.ExpectedRevision + 1}
	authority.receipts[command.Proposal.CommandID] = fakeReceipt{key: command.Receipt, result: result}
	return result, nil
}

func testService(t *testing.T, authority operator.Authority, now *time.Time) *operator.Service {
	t.Helper()
	var key [32]byte
	for index := range key {
		key[index] = 0x42
	}
	service, err := operator.New(operator.Options{
		Authority: authority, OperatorID: testOperatorID, OwnershipPolicyID: "owner-policy-test-v1",
		EntitlementPolicyID: "entitlement-policy-test-v1", ConfirmationKey: key, Now: func() time.Time { return *now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func recoveryProposal() operator.RecoveryProposal {
	return operator.RecoveryProposal{CommandID: "30000000-0000-4000-8000-000000000061", FamilyID: testFamilyID,
		OldOwnerID: testOldOwnerID, NewOwnerID: testNewOwnerID, PolicyID: "owner-policy-test-v1",
		EvidenceReference: "case_test_0001", ReasonCode: "owner_identity_unavailable", ExpectedRevision: 42}
}

func entitlementProposal(operation string) operator.EntitlementProposal {
	commandID, evidence, reason := "30000000-0000-4000-8000-000000000062", "entitlement_test_0001", "beta_extension"
	if operation == "suspend" {
		commandID, evidence, reason = "30000000-0000-4000-8000-000000000063", "entitlement_test_0002", "abuse_suspension"
	}
	return operator.EntitlementProposal{CommandID: commandID, FamilyID: testFamilyID, Operation: operation, Key: "family_sync",
		Source: "hosted_beta", ExpiresAt: time.Date(2027, 9, 20, 12, 0, 0, 0, time.UTC), PolicyID: "entitlement-policy-test-v1",
		EvidenceReference: evidence, ReasonCode: reason, ExpectedRevision: 42}
}

func TestOperatorConfirmationVectors(t *testing.T) {
	authority := &fakeAuthority{receipts: make(map[string]fakeReceipt)}
	now := testNow
	service := testService(t, authority, &now)
	tests := []struct {
		name, expected string
		prepare        func() (operator.Confirmation, error)
	}{
		{"ownership-recover", "0e9030b6119c2f6834643fc0c17f5681d1ea044ab618e841d2c652b6f02ae37a", func() (operator.Confirmation, error) {
			return service.PrepareRecovery(t.Context(), recoveryProposal())
		}},
		{"entitlement-grant", "35c9bfa1a62100f8cc06d8d04f74feafd46cde8b944b40826e2e1d5b402f3e82", func() (operator.Confirmation, error) {
			return service.PrepareEntitlement(t.Context(), entitlementProposal("grant"))
		}},
		{"entitlement-suspend", "5ff8150ff438cc3a471ccba9b6bb23529de13080e19d97339518c9b519065dcc", func() (operator.Confirmation, error) {
			return service.PrepareEntitlement(t.Context(), entitlementProposal("suspend"))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			confirmation, err := test.prepare()
			if err != nil || confirmation.Digest != test.expected || !confirmation.ExpiresAt.Equal(testNow.Add(5*time.Minute)) {
				t.Fatalf("confirmation=%+v err=%v", confirmation, err)
			}
		})
	}
}

func TestOperatorConfirmationBindsOperatorIDOperationProposalAndExpiry(t *testing.T) {
	authority := &fakeAuthority{receipts: make(map[string]fakeReceipt)}
	now := testNow
	service := testService(t, authority, &now)
	proposal := recoveryProposal()
	confirmation, err := service.PrepareRecovery(t.Context(), proposal)
	if err != nil {
		t.Fatal(err)
	}
	mutated := proposal
	mutated.NewOwnerID = "10000000-0000-4000-8000-000000000003"
	if _, err := service.RecoverOwner(t.Context(), mutated, confirmation); !errors.Is(err, operator.ErrInvalidConfirmation) {
		t.Fatalf("modified proposal err=%v", err)
	}
	confirmation.ExpiresAt = confirmation.ExpiresAt.Add(-time.Second)
	if _, err := service.RecoverOwner(t.Context(), proposal, confirmation); !errors.Is(err, operator.ErrInvalidConfirmation) {
		t.Fatalf("modified expiry err=%v", err)
	}
}

func TestOperatorConfirmationRejectsWrongConfiguredOperator(t *testing.T) {
	authority := &fakeAuthority{receipts: make(map[string]fakeReceipt)}
	now := testNow
	service := testService(t, authority, &now)
	proposal := recoveryProposal()
	confirmation, err := service.PrepareRecovery(t.Context(), proposal)
	if err != nil {
		t.Fatal(err)
	}
	var key [32]byte
	for index := range key {
		key[index] = 0x42
	}
	other, err := operator.New(operator.Options{Authority: authority, OperatorID: "90000000-0000-4000-8000-000000000002",
		OwnershipPolicyID: "owner-policy-test-v1", EntitlementPolicyID: "entitlement-policy-test-v1",
		ConfirmationKey: key, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.RecoverOwner(t.Context(), proposal, confirmation); !errors.Is(err, operator.ErrInvalidConfirmation) {
		t.Fatalf("wrong operator err=%v", err)
	}
}

func TestOperatorEvidenceDigestKnownAnswer(t *testing.T) {
	authority := &fakeAuthority{receipts: make(map[string]fakeReceipt)}
	now := testNow
	service := testService(t, authority, &now)
	for _, test := range []struct {
		operation, expected string
	}{
		{"grant", "ae11cb7ca4a4db1a8cc773a778603f2b71018f37dd547c0f4a05d43bd8aabd8e"},
		{"suspend", "883948f1dd98b409a5ba5288fbdd59737744b84ba4047f2cdb1fa4b8770166d0"},
	} {
		proposal := entitlementProposal(test.operation)
		confirmation, err := service.PrepareEntitlement(t.Context(), proposal)
		if err != nil {
			t.Fatal(err)
		}
		if test.operation == "grant" {
			_, err = service.GrantEntitlement(t.Context(), proposal, confirmation)
		} else {
			_, err = service.SuspendEntitlement(t.Context(), proposal, confirmation)
		}
		if err != nil {
			t.Fatal(err)
		}
		command := authority.entitlementCommands[len(authority.entitlementCommands)-1]
		if command.Audit.EvidenceDigest != test.expected {
			t.Fatalf("%s evidence digest=%q", test.operation, command.Audit.EvidenceDigest)
		}
	}
}

func TestOperatorCommitsAuditedCommandsAndReplaysBeforeExpiryCheck(t *testing.T) {
	authority := &fakeAuthority{receipts: make(map[string]fakeReceipt)}
	now := testNow
	service := testService(t, authority, &now)
	proposal := recoveryProposal()
	confirmation, err := service.PrepareRecovery(t.Context(), proposal)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.RecoverOwner(t.Context(), proposal, confirmation)
	if err != nil || result.Revision != 43 || len(authority.recoveryCommands) != 1 {
		t.Fatalf("result=%+v commands=%d err=%v", result, len(authority.recoveryCommands), err)
	}
	command := authority.recoveryCommands[0]
	if command.Audit.OperatorID != testOperatorID || command.Audit.PolicyID != proposal.PolicyID || command.Audit.ExpectedRevision != 42 ||
		command.Audit.EvidenceDigest != "99beb25654c5a1b5e54f0560ad6cc46c59edcd1e1074b0c7562cd37ade9fd672" {
		t.Fatalf("audit=%+v", command.Audit)
	}
	now = now.Add(10 * time.Minute)
	replayed, err := service.RecoverOwner(t.Context(), proposal, confirmation)
	if err != nil || replayed != result || len(authority.recoveryCommands) != 1 {
		t.Fatalf("expired replay=%+v commands=%d err=%v", replayed, len(authority.recoveryCommands), err)
	}
	changed := proposal
	changed.FamilyID = "20000000-0000-4000-8000-000000000002"
	if _, err := service.RecoverOwner(t.Context(), changed, confirmation); !errors.Is(err, operator.ErrCommandIDReused) {
		t.Fatalf("changed command ID replay err=%v", err)
	}
}

func TestQueuedOperatorCommitCannotCrossConfirmationExpiryFence(t *testing.T) {
	now := testNow
	authority := &expiringAuthority{
		fakeAuthority: fakeAuthority{receipts: make(map[string]fakeReceipt)},
		entered:       make(chan struct{}),
		release:       make(chan struct{}),
		seen:          make(chan time.Time, 1),
		now:           func() time.Time { return now },
	}
	service := testService(t, authority, &now)
	proposal := recoveryProposal()
	confirmation, err := service.PrepareRecovery(t.Context(), proposal)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, commitErr := service.RecoverOwner(t.Context(), proposal, confirmation)
		result <- commitErr
	}()
	<-authority.entered
	if seen := <-authority.seen; !seen.Equal(confirmation.ExpiresAt) {
		t.Fatalf("authority received expiry %s; want %s", seen, confirmation.ExpiresAt)
	}
	now = confirmation.ExpiresAt
	close(authority.release)
	if err := <-result; !errors.Is(err, operator.ErrInvalidConfirmation) {
		t.Fatalf("queued commit err=%v", err)
	}
	if len(authority.recoveryCommands) != 0 || len(authority.receipts) != 0 {
		t.Fatalf("expired queued commit mutated authority: commands=%d receipts=%d", len(authority.recoveryCommands), len(authority.receipts))
	}
}

func TestOperatorEntitlementPreservesHostedBetaSource(t *testing.T) {
	authority := &fakeAuthority{receipts: make(map[string]fakeReceipt)}
	now := testNow
	service := testService(t, authority, &now)
	proposal := entitlementProposal("suspend")
	confirmation, err := service.PrepareEntitlement(t.Context(), proposal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SuspendEntitlement(t.Context(), proposal, confirmation); err != nil {
		t.Fatal(err)
	}
	if len(authority.entitlementCommands) != 1 || authority.entitlementCommands[0].Proposal.Source != "hosted_beta" {
		t.Fatalf("commands=%+v", authority.entitlementCommands)
	}
	if !authority.entitlementCommands[0].ConfirmationExpiresAt.Equal(confirmation.ExpiresAt) {
		t.Fatalf("authenticated expiry was not carried to authority: command=%+v confirmation=%+v", authority.entitlementCommands[0], confirmation)
	}
	changed := proposal
	changed.Source = "operator"
	if _, err := service.PrepareEntitlement(t.Context(), changed); !errors.Is(err, operator.ErrInvalidRequest) {
		t.Fatalf("changed source err=%v", err)
	}
}

func TestOperatorRejectsZeroExpectedRevisionAndMissingPolicy(t *testing.T) {
	now := testNow
	authority := &fakeAuthority{receipts: make(map[string]fakeReceipt)}
	service := testService(t, authority, &now)
	proposal := recoveryProposal()
	proposal.ExpectedRevision = 0
	if _, err := service.PrepareRecovery(t.Context(), proposal); !errors.Is(err, operator.ErrInvalidRequest) {
		t.Fatalf("zero revision err=%v", err)
	}
	disabled, err := operator.New(operator.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := disabled.PrepareRecovery(t.Context(), recoveryProposal()); !errors.Is(err, operator.ErrOperatorPolicyRequired) {
		t.Fatalf("missing policy err=%v", err)
	}
}
