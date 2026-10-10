package operator_test

import (
	"context"
	"errors"
	"testing"
	"time"

	core "babytracker/server/internal/authority"
	"babytracker/server/internal/operator"
)

type fakeOperatorSecurity struct {
	recoveryPreflight, recoveryReplay, recoveryCommit    core.OperatorRecoverOwner
	entitlementPreflight, entitlementReplay              core.OperatorEntitlement
	entitlementCommits                                   []core.OperatorEntitlement
	result                                               core.Result
	recoveryPreflights, recoveryReplays, recoveryCommits int
	integrationCalls                                     []string
	integrationInputs                                    []core.OperatorIntegration
	integrationViews                                     []core.IntegrationView
	revisions                                            map[string]uint64
	revisionCalls                                        []string
}

func (backend *fakeOperatorSecurity) PreflightOperatorRecoverOwner(_ context.Context, input core.OperatorRecoverOwner) error {
	backend.recoveryPreflights++
	backend.recoveryPreflight = input
	return nil
}
func (backend *fakeOperatorSecurity) PreflightOperatorEntitlement(_ context.Context, input core.OperatorEntitlement) error {
	backend.entitlementPreflight = input
	return nil
}
func (backend *fakeOperatorSecurity) ReplayOperatorRecoverOwner(_ context.Context, input core.OperatorRecoverOwner) (core.Result, bool, error) {
	backend.recoveryReplays++
	backend.recoveryReplay = input
	return backend.result, false, nil
}
func (backend *fakeOperatorSecurity) ReplayOperatorEntitlement(_ context.Context, input core.OperatorEntitlement) (core.Result, bool, error) {
	backend.entitlementReplay = input
	return backend.result, false, nil
}
func (backend *fakeOperatorSecurity) ExecuteOperatorRecoverOwner(_ context.Context, input core.OperatorRecoverOwner) (core.Result, error) {
	backend.recoveryCommits++
	backend.recoveryCommit = input
	return backend.result, nil
}
func (backend *fakeOperatorSecurity) ExecuteOperatorEntitlement(_ context.Context, input core.OperatorEntitlement) (core.Result, error) {
	backend.entitlementCommits = append(backend.entitlementCommits, input)
	return backend.result, nil
}
func (backend *fakeOperatorSecurity) FamilyRevision(_ context.Context, familyID string) (uint64, error) {
	backend.revisionCalls = append(backend.revisionCalls, familyID)
	revision, ok := backend.revisions[familyID]
	if !ok {
		return 0, core.ErrFamilyUnavailable
	}
	return revision, nil
}

// fakeSecurityAuthorityGate is the node readiness check.
type fakeSecurityAuthorityGate struct {
	checks int
	err    error
}

func (gate *fakeSecurityAuthorityGate) Check(context.Context) error {
	gate.checks++
	return gate.err
}

func TestSecurityAuthorityMapsTypedRecoveryWithoutMintingPrincipal(t *testing.T) {
	backend := &fakeOperatorSecurity{result: core.Result{CommandID: "30000000-0000-4000-8000-000000000061", Outcome: "committed", Code: "ok", Revision: 43}}
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
	want := core.OperatorRecoverOwner{CommandID: command.Proposal.CommandID, OperatorID: command.Audit.OperatorID,
		FamilyID: command.Proposal.FamilyID, OldOwnerAccountID: command.Proposal.OldOwnerID, NewOwnerAccountID: command.Proposal.NewOwnerID,
		PolicyID: command.Audit.PolicyID, EvidenceDigest: command.Audit.EvidenceDigest, ReasonCode: command.Audit.ReasonCode,
		ExpectedRevision: command.Audit.ExpectedRevision, ConfirmationExpiresAt: command.ConfirmationExpiresAt}
	if backend.recoveryPreflight != want || backend.recoveryReplay != want || backend.recoveryCommit != want {
		t.Fatalf("preflight=%+v replay=%+v commit=%+v want=%+v", backend.recoveryPreflight, backend.recoveryReplay, backend.recoveryCommit, want)
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
		t.Fatal("nil readiness gate was accepted")
	}
}

func TestGatedSecurityAuthorityAllowsReceiptProbeButGatesPreflightAndCommit(t *testing.T) {
	backend := &fakeOperatorSecurity{}
	gateErr := errors.New("authority not ready")
	gate := &fakeSecurityAuthorityGate{err: gateErr}
	adapter, err := operator.NewSecurityAuthority(backend, gate)
	if err != nil {
		t.Fatal(err)
	}
	command := operator.RecoveryCommand{Proposal: recoveryProposal(), Audit: operator.Audit{OperatorID: testOperatorID,
		PolicyID: "owner-policy-test-v1", EvidenceDigest: "99beb25654c5a1b5e54f0560ad6cc46c59edcd1e1074b0c7562cd37ade9fd672",
		ReasonCode: "owner_identity_unavailable", ExpectedRevision: 42}, ConfirmationExpiresAt: testNow.Add(5 * time.Minute)}
	if _, _, err := adapter.ReplayRecovery(t.Context(), command); err != nil || gate.checks != 0 || backend.recoveryReplays != 1 {
		t.Fatalf("receipt probe checks=%d replays=%d err=%v", gate.checks, backend.recoveryReplays, err)
	}
	if err := adapter.PreflightRecovery(t.Context(), command); !errors.Is(err, gateErr) || gate.checks != 1 || backend.recoveryPreflights != 0 {
		t.Fatalf("preflight checks=%d calls=%d err=%v", gate.checks, backend.recoveryPreflights, err)
	}
	if _, err := adapter.RecoverOwner(t.Context(), command); !errors.Is(err, gateErr) || gate.checks != 2 || backend.recoveryCommits != 0 {
		t.Fatalf("commit checks=%d calls=%d err=%v", gate.checks, backend.recoveryCommits, err)
	}
}

// The operator reads the family's audit seq to fill --expected-revision:
// a ready node answers it from the authority, a node that is not ready
// refuses before reading, and a malformed family never reaches it.
func TestSecurityAuthorityReadsFamilyRevisionBehindReadiness(t *testing.T) {
	backend := &fakeOperatorSecurity{revisions: map[string]uint64{testFamilyID: 7}}
	gate := &fakeSecurityAuthorityGate{}
	adapter, err := operator.NewSecurityAuthority(backend, gate)
	if err != nil {
		t.Fatal(err)
	}
	if revision, err := adapter.FamilyRevision(t.Context(), testFamilyID); err != nil || revision != 7 || gate.checks != 1 {
		t.Fatalf("revision=%d checks=%d err=%v", revision, gate.checks, err)
	}
	if _, err := adapter.FamilyRevision(t.Context(), "60000000-0000-4000-8000-0000000000ff"); operator.Code(err) != "family_unavailable" {
		t.Fatalf("unknown family code=%q err=%v", operator.Code(err), err)
	}
	if _, err := adapter.FamilyRevision(t.Context(), "not-a-family"); !errors.Is(err, operator.ErrInvalidRequest) || len(backend.revisionCalls) != 2 {
		t.Fatalf("malformed family err=%v calls=%v", err, backend.revisionCalls)
	}
	gate.err = errors.New("authority not ready")
	if _, err := adapter.FamilyRevision(t.Context(), testFamilyID); !errors.Is(err, gate.err) || len(backend.revisionCalls) != 2 {
		t.Fatalf("unready err=%v calls=%v", err, backend.revisionCalls)
	}
}
