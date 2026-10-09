package operator_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"babytracker/server/internal/operator"
	"babytracker/server/internal/security"
)

type fakeOperatorSecurity struct {
	recoveryPreflight, recoveryReplay, recoveryCommit    security.OperatorRecoveryInput
	entitlementPreflight, entitlementReplay              security.OperatorEntitlementInput
	entitlementCommits                                   []security.OperatorEntitlementInput
	result                                               security.CommandResult
	recoveryPreflights, recoveryReplays, recoveryCommits int
	recoveryReplayGuard, recoveryCommitGuard             security.OperatorAuthorityGuard
}

func (backend *fakeOperatorSecurity) PreflightOperatorRecovery(_ context.Context, input security.OperatorRecoveryInput) error {
	backend.recoveryPreflights++
	backend.recoveryPreflight = input
	return nil
}
func (backend *fakeOperatorSecurity) PreflightOperatorEntitlement(_ context.Context, input security.OperatorEntitlementInput) error {
	backend.entitlementPreflight = input
	return nil
}
func (backend *fakeOperatorSecurity) ReplayOperatorRecovery(_ context.Context, input security.OperatorRecoveryInput, guard security.OperatorAuthorityGuard) (security.CommandResult, bool, error) {
	backend.recoveryReplays++
	backend.recoveryReplay = input
	backend.recoveryReplayGuard = guard
	return backend.result, false, nil
}
func (backend *fakeOperatorSecurity) ReplayOperatorEntitlement(_ context.Context, input security.OperatorEntitlementInput, _ security.OperatorAuthorityGuard) (security.CommandResult, bool, error) {
	backend.entitlementReplay = input
	return backend.result, false, nil
}
func (backend *fakeOperatorSecurity) ExecuteOperatorRecovery(_ context.Context, input security.OperatorRecoveryInput, guard security.OperatorAuthorityGuard) (security.CommandResult, error) {
	backend.recoveryCommits++
	backend.recoveryCommit = input
	backend.recoveryCommitGuard = guard
	return backend.result, nil
}

type fakeSecurityAuthorityGate struct {
	checks int
	err    error
}

func (gate *fakeSecurityAuthorityGate) Check(context.Context) error {
	gate.checks++
	return gate.err
}
func (gate *fakeSecurityAuthorityGate) ValidateOperatorAuthorityUnderFence(context.Context, security.Head) error {
	return gate.err
}
func (backend *fakeOperatorSecurity) ExecuteOperatorEntitlement(_ context.Context, input security.OperatorEntitlementInput, _ security.OperatorAuthorityGuard) (security.CommandResult, error) {
	backend.entitlementCommits = append(backend.entitlementCommits, input)
	return backend.result, nil
}

func TestSecurityAuthorityMapsTypedRecoveryWithoutMintingPrincipal(t *testing.T) {
	backend := &fakeOperatorSecurity{result: security.CommandResult{CommandID: "30000000-0000-4000-8000-000000000061", Outcome: "committed", Code: "ok", Revision: 43}}
	adapter, err := operator.NewSecurityAuthority(backend, &fakeSecurityAuthorityGate{})
	if err != nil {
		t.Fatal(err)
	}
	command := operator.RecoveryCommand{Proposal: recoveryProposal(), Audit: operator.Audit{OperatorID: testOperatorID,
		PolicyID: "owner-policy-test-v1", EvidenceDigest: "99beb25654c5a1b5e54f0560ad6cc46c59edcd1e1074b0c7562cd37ade9fd672",
		ReasonCode: "owner_identity_unavailable", ExpectedRevision: 42}, ConfirmationExpiresAt: testNow.Add(5 * time.Minute)}
	if err := adapter.PreflightRecovery(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	if _, _, err := adapter.ReplayRecovery(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	result, err := adapter.RecoverOwner(t.Context(), command)
	if err != nil || result.Revision != 43 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	want := security.OperatorRecoveryInput{CommandID: command.Proposal.CommandID, OperatorID: command.Audit.OperatorID,
		FamilyID: command.Proposal.FamilyID, OldOwnerAccountID: command.Proposal.OldOwnerID, NewOwnerAccountID: command.Proposal.NewOwnerID,
		PolicyID: command.Audit.PolicyID, EvidenceDigest: command.Audit.EvidenceDigest, ReasonCode: command.Audit.ReasonCode,
		ExpectedRevision: command.Audit.ExpectedRevision, ConfirmationExpiresAt: command.ConfirmationExpiresAt}
	if backend.recoveryPreflight != want || backend.recoveryReplay != want || backend.recoveryCommit != want {
		t.Fatalf("preflight=%+v replay=%+v commit=%+v want=%+v", backend.recoveryPreflight, backend.recoveryReplay, backend.recoveryCommit, want)
	}
	if backend.recoveryReplayGuard == nil || backend.recoveryCommitGuard == nil {
		t.Fatal("recovery guard was not propagated to security authority")
	}
}

func TestSecurityAuthorityMapsGrantAndSuspendExactly(t *testing.T) {
	backend := &fakeOperatorSecurity{}
	adapter, err := operator.NewSecurityAuthority(backend, &fakeSecurityAuthorityGate{})
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"grant", "suspend"} {
		proposal := entitlementProposal(operation)
		command := operator.EntitlementCommand{Proposal: proposal, Audit: operator.Audit{OperatorID: testOperatorID,
			PolicyID: proposal.PolicyID, EvidenceDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			ReasonCode: proposal.ReasonCode, ExpectedRevision: proposal.ExpectedRevision}, ConfirmationExpiresAt: testNow.Add(5 * time.Minute)}
		if err := adapter.PreflightEntitlement(t.Context(), command); err != nil {
			t.Fatal(err)
		}
		if _, _, err := adapter.ReplayEntitlement(t.Context(), command); err != nil {
			t.Fatal(err)
		}
		if operation == "grant" {
			_, err = adapter.GrantEntitlement(t.Context(), command)
		} else {
			_, err = adapter.SuspendEntitlement(t.Context(), command)
		}
		if err != nil {
			t.Fatal(err)
		}
		got := backend.entitlementCommits[len(backend.entitlementCommits)-1]
		if got.Operation != operation || got.Source != "hosted_beta" || !got.ExpiresAt.Equal(proposal.ExpiresAt) || got.ExpectedRevision != 42 {
			t.Fatalf("mapped entitlement=%+v", got)
		}
	}
}

func TestSecurityAuthorityRejectsMissingBackend(t *testing.T) {
	if _, err := operator.NewSecurityAuthority(nil, &fakeSecurityAuthorityGate{}); err == nil {
		t.Fatal("nil security backend was accepted")
	}
	if _, err := operator.NewSecurityAuthority(&fakeOperatorSecurity{}, nil); err == nil {
		t.Fatal("nil recovery gate was accepted")
	}
}

func TestGatedSecurityAuthorityAllowsReceiptProbeButGatesPreflightAndCommit(t *testing.T) {
	backend := &fakeOperatorSecurity{}
	gateErr := errors.New("recovery gate quarantined")
	gate := &fakeSecurityAuthorityGate{err: gateErr}
	adapter, err := operator.NewSecurityAuthority(backend, gate)
	if err != nil {
		t.Fatal(err)
	}
	command := operator.RecoveryCommand{Proposal: recoveryProposal(), Audit: operator.Audit{OperatorID: testOperatorID,
		PolicyID: "owner-policy-test-v1", EvidenceDigest: "99beb25654c5a1b5e54f0560ad6cc46c59edcd1e1074b0c7562cd37ade9fd672",
		ReasonCode: "owner_identity_unavailable", ExpectedRevision: 42}, ConfirmationExpiresAt: testNow.Add(5 * time.Minute)}
	if _, _, err := adapter.ReplayRecovery(t.Context(), command); err != nil || gate.checks != 0 || backend.recoveryReplays != 1 || backend.recoveryReplayGuard != gate {
		t.Fatalf("receipt probe checks=%d replays=%d err=%v", gate.checks, backend.recoveryReplays, err)
	}
	if err := adapter.PreflightRecovery(t.Context(), command); !errors.Is(err, gateErr) || gate.checks != 1 || backend.recoveryPreflights != 0 {
		t.Fatalf("preflight checks=%d calls=%d err=%v", gate.checks, backend.recoveryPreflights, err)
	}
	if _, err := adapter.RecoverOwner(t.Context(), command); !errors.Is(err, gateErr) || gate.checks != 2 || backend.recoveryCommits != 0 {
		t.Fatalf("commit checks=%d calls=%d err=%v", gate.checks, backend.recoveryCommits, err)
	}
}
