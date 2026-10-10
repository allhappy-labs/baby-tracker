package operator_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	core "babytracker/server/internal/authority"
	"babytracker/server/internal/operator"
)

const (
	testGrantID    = "60000000-0000-4000-8000-000000000001"
	testInstanceID = "50000000-0000-4000-8000-000000000001"
	testPolicyMCP  = "integration-policy-test-v1"
)

type fakeIntegrationAuthority struct {
	preflights, executes []operator.IntegrationCommand
	replays              int
	receipts             map[string]fakeReceipt
	views                []core.IntegrationView
	listed               []string
	preflightErr         error
}

func (authority *fakeIntegrationAuthority) PreflightIntegration(_ context.Context, command operator.IntegrationCommand) error {
	authority.preflights = append(authority.preflights, command)
	return authority.preflightErr
}

func (authority *fakeIntegrationAuthority) ReplayIntegration(_ context.Context, command operator.IntegrationCommand) (core.Result, bool, error) {
	authority.replays++
	receipt, ok := authority.receipts[command.Receipt.CommandID]
	if !ok {
		return core.Result{}, false, nil
	}
	if receipt.key != command.Receipt {
		return core.Result{}, false, operator.ErrCommandIDReused
	}
	return receipt.result, true, nil
}

func (authority *fakeIntegrationAuthority) ExecuteIntegration(_ context.Context, command operator.IntegrationCommand) (core.Result, error) {
	authority.executes = append(authority.executes, command)
	result := core.Result{CommandID: command.Proposal.CommandID, Outcome: "committed", Code: "ok", Revision: command.Proposal.ExpectedRevision + 1}
	authority.receipts[command.Proposal.CommandID] = fakeReceipt{key: command.Receipt, result: result}
	return result, nil
}

func (authority *fakeIntegrationAuthority) ListIntegrations(_ context.Context, familyID string) ([]core.IntegrationView, error) {
	authority.listed = append(authority.listed, familyID)
	return append([]core.IntegrationView(nil), authority.views...), nil
}

func integrationTestService(t *testing.T, authority operator.IntegrationAuthority, now *time.Time) *operator.Service {
	t.Helper()
	var key [32]byte
	for index := range key {
		key[index] = 0x42
	}
	service, err := operator.New(operator.Options{IntegrationAuthority: authority, OperatorID: testOperatorID, IntegrationPolicyID: testPolicyMCP,
		ConfirmationKey: key, Now: func() time.Time { return *now }})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func integrationGrantProposal() operator.IntegrationProposal {
	return operator.IntegrationProposal{CommandID: "30000000-0000-4000-8000-000000000064", Operation: "grant", GrantID: testGrantID,
		FamilyID: testFamilyID, AccountID: testOldOwnerID, Name: "claude-desktop-laptop", Scope: "family:read",
		Verifier: strings.Repeat("ab", 32), ServerInstanceID: testInstanceID, RecoveryEpoch: 1, ValiditySeconds: 7776000,
		PolicyID: testPolicyMCP, EvidenceReference: "mcp_issue_0001", ReasonCode: "integration_issued", ExpectedRevision: 42}
}

func integrationRevokeProposal() operator.IntegrationProposal {
	return operator.IntegrationProposal{CommandID: "30000000-0000-4000-8000-000000000065", Operation: "revoke", GrantID: testGrantID,
		FamilyID: testFamilyID, AccountID: testOldOwnerID, ServerInstanceID: testInstanceID, RecoveryEpoch: 1,
		PolicyID: testPolicyMCP, EvidenceReference: "mcp_revoke_0001", ReasonCode: "integration_revoked", ExpectedRevision: 43}
}

func TestIntegrationCommandsRequireConfiguredMatchingPolicy(t *testing.T) {
	now := testNow
	authority := &fakeIntegrationAuthority{receipts: map[string]fakeReceipt{}}
	disabled, err := operator.New(operator.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	otherFamilies := testService(t, &fakeAuthority{receipts: map[string]fakeReceipt{}}, &now)
	for name, service := range map[string]*operator.Service{"disabled": disabled, "other families only": otherFamilies} {
		t.Run(name, func(t *testing.T) {
			if _, err := service.PrepareIntegration(t.Context(), integrationGrantProposal()); !errors.Is(err, operator.ErrOperatorPolicyRequired) {
				t.Fatalf("prepare err=%v", err)
			}
			if _, err := service.IssueIntegration(t.Context(), integrationGrantProposal(), operator.Confirmation{}); !errors.Is(err, operator.ErrOperatorPolicyRequired) {
				t.Fatalf("issue err=%v", err)
			}
			if _, err := service.RevokeIntegration(t.Context(), integrationRevokeProposal(), operator.Confirmation{}); !errors.Is(err, operator.ErrOperatorPolicyRequired) {
				t.Fatalf("revoke err=%v", err)
			}
			if _, err := service.ListIntegrations(t.Context(), testFamilyID); !errors.Is(err, operator.ErrOperatorPolicyRequired) {
				t.Fatalf("list err=%v", err)
			}
		})
	}
	service := integrationTestService(t, authority, &now)
	mismatched := integrationGrantProposal()
	mismatched.PolicyID = "owner-policy-test-v1"
	if _, err := service.PrepareIntegration(t.Context(), mismatched); !errors.Is(err, operator.ErrOperatorPolicyRequired) {
		t.Fatalf("mismatched policy err=%v", err)
	}
	if _, err := service.IssueIntegration(t.Context(), mismatched, operator.Confirmation{}); !errors.Is(err, operator.ErrOperatorPolicyRequired) {
		t.Fatalf("mismatched issue err=%v", err)
	}
	if len(authority.preflights) != 0 || authority.replays != 0 || len(authority.executes) != 0 || len(authority.listed) != 0 {
		t.Fatalf("policy denial reached authority: %+v", authority)
	}
	var key [32]byte
	key[0] = 1
	if _, err := operator.New(operator.Options{OperatorID: testOperatorID, IntegrationPolicyID: testPolicyMCP, ConfirmationKey: key,
		Now: func() time.Time { return now }}); err == nil {
		t.Fatal("integration policy without integration authority was accepted")
	}
	if _, err := operator.New(operator.Options{IntegrationAuthority: authority, OperatorID: testOperatorID, IntegrationPolicyID: "bad policy",
		ConfirmationKey: key, Now: func() time.Time { return now }}); err == nil {
		t.Fatal("invalid integration policy ID was accepted")
	}
}

func TestPrepareIntegrationPreflightsExactAuditedProposal(t *testing.T) {
	now := testNow.Add(750 * time.Millisecond)
	authority := &fakeIntegrationAuthority{receipts: map[string]fakeReceipt{}}
	service := integrationTestService(t, authority, &now)
	for _, proposal := range []operator.IntegrationProposal{integrationGrantProposal(), integrationRevokeProposal()} {
		confirmation, err := service.PrepareIntegration(t.Context(), proposal)
		if err != nil || !confirmation.ExpiresAt.Equal(testNow.Add(5*time.Minute)) || len(confirmation.Digest) != 64 {
			t.Fatalf("%s confirmation=%+v err=%v", proposal.Operation, confirmation, err)
		}
		command := authority.preflights[len(authority.preflights)-1]
		if command.Proposal != proposal || command.Audit.OperatorID != testOperatorID || command.Audit.PolicyID != testPolicyMCP ||
			command.Audit.ReasonCode != proposal.ReasonCode || command.Audit.ExpectedRevision != proposal.ExpectedRevision ||
			len(command.Audit.EvidenceDigest) != 64 || strings.Contains(command.Audit.EvidenceDigest, proposal.EvidenceReference) ||
			command.Receipt.Operation != "integration-"+proposal.Operation || command.Receipt.CommandID != proposal.CommandID ||
			!command.ConfirmationExpiresAt.IsZero() {
			t.Fatalf("preflight command=%+v", command)
		}
	}
	authority.preflightErr = errors.New("owner_required")
	if _, err := service.PrepareIntegration(t.Context(), integrationGrantProposal()); err == nil || err.Error() != "owner_required" {
		t.Fatalf("preflight error was not returned: %v", err)
	}
}

func TestIntegrationProposalValidationRejectsBeforeAuthority(t *testing.T) {
	now := testNow
	authority := &fakeIntegrationAuthority{receipts: map[string]fakeReceipt{}}
	service := integrationTestService(t, authority, &now)
	grant := func(mutate func(*operator.IntegrationProposal)) operator.IntegrationProposal {
		proposal := integrationGrantProposal()
		mutate(&proposal)
		return proposal
	}
	revoke := func(mutate func(*operator.IntegrationProposal)) operator.IntegrationProposal {
		proposal := integrationRevokeProposal()
		mutate(&proposal)
		return proposal
	}
	for name, proposal := range map[string]operator.IntegrationProposal{
		"unknown operation":   grant(func(p *operator.IntegrationProposal) { p.Operation = "issue" }),
		"command id":          grant(func(p *operator.IntegrationProposal) { p.CommandID = "not-a-uuid" }),
		"grant id":            grant(func(p *operator.IntegrationProposal) { p.GrantID = strings.ToUpper(p.GrantID) + "A" }),
		"zero grant id":       grant(func(p *operator.IntegrationProposal) { p.GrantID = "00000000-0000-0000-0000-000000000000" }),
		"family id":           grant(func(p *operator.IntegrationProposal) { p.FamilyID = "" }),
		"account id":          grant(func(p *operator.IntegrationProposal) { p.AccountID = "x" }),
		"instance id":         grant(func(p *operator.IntegrationProposal) { p.ServerInstanceID = "" }),
		"empty label":         grant(func(p *operator.IntegrationProposal) { p.Name = "" }),
		"padded label":        grant(func(p *operator.IntegrationProposal) { p.Name = " laptop" }),
		"control label":       grant(func(p *operator.IntegrationProposal) { p.Name = "lap\ttop" }),
		"long label":          grant(func(p *operator.IntegrationProposal) { p.Name = strings.Repeat("a", 65) }),
		"scope":               grant(func(p *operator.IntegrationProposal) { p.Scope = "family:write" }),
		"validity":            grant(func(p *operator.IntegrationProposal) { p.ValiditySeconds = 7776001 }),
		"uppercase verifier":  grant(func(p *operator.IntegrationProposal) { p.Verifier = strings.Repeat("AB", 32) }),
		"short verifier":      grant(func(p *operator.IntegrationProposal) { p.Verifier = strings.Repeat("ab", 31) }),
		"zero epoch":          grant(func(p *operator.IntegrationProposal) { p.RecoveryEpoch = 0 }),
		"unsafe epoch":        grant(func(p *operator.IntegrationProposal) { p.RecoveryEpoch = 1 << 53 }),
		"zero revision":       grant(func(p *operator.IntegrationProposal) { p.ExpectedRevision = 0 }),
		"grant reason":        grant(func(p *operator.IntegrationProposal) { p.ReasonCode = "integration_revoked" }),
		"evidence":            grant(func(p *operator.IntegrationProposal) { p.EvidenceReference = "short" }),
		"policy syntax":       grant(func(p *operator.IntegrationProposal) { p.PolicyID = "bad policy" }),
		"revoke reason":       revoke(func(p *operator.IntegrationProposal) { p.ReasonCode = "integration_issued" }),
		"revoke label":        revoke(func(p *operator.IntegrationProposal) { p.Name = "laptop" }),
		"revoke scope":        revoke(func(p *operator.IntegrationProposal) { p.Scope = "family:read" }),
		"revoke verifier":     revoke(func(p *operator.IntegrationProposal) { p.Verifier = strings.Repeat("ab", 32) }),
		"revoke validity":     revoke(func(p *operator.IntegrationProposal) { p.ValiditySeconds = 7776000 }),
		"revoke zero epoch":   revoke(func(p *operator.IntegrationProposal) { p.RecoveryEpoch = 0 }),
		"revoke zero grantid": revoke(func(p *operator.IntegrationProposal) { p.GrantID = "" }),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := service.PrepareIntegration(t.Context(), proposal); !errors.Is(err, operator.ErrInvalidRequest) {
				t.Fatalf("prepare err=%v", err)
			}
			if _, err := service.IssueIntegration(t.Context(), proposal, operator.Confirmation{}); !errors.Is(err, operator.ErrInvalidRequest) {
				t.Fatalf("issue err=%v", err)
			}
			if _, err := service.RevokeIntegration(t.Context(), proposal, operator.Confirmation{}); !errors.Is(err, operator.ErrInvalidRequest) {
				t.Fatalf("revoke err=%v", err)
			}
		})
	}
	if _, err := service.IssueIntegration(t.Context(), integrationRevokeProposal(), operator.Confirmation{}); !errors.Is(err, operator.ErrInvalidRequest) {
		t.Fatalf("issue accepted a revoke proposal: %v", err)
	}
	if _, err := service.RevokeIntegration(t.Context(), integrationGrantProposal(), operator.Confirmation{}); !errors.Is(err, operator.ErrInvalidRequest) {
		t.Fatalf("revoke accepted a grant proposal: %v", err)
	}
	if len(authority.preflights) != 0 || authority.replays != 0 || len(authority.executes) != 0 {
		t.Fatalf("invalid proposal reached authority: %+v", authority)
	}
}

func TestIntegrationCommitReplaysBeforeConfirmationAndBindsDeadline(t *testing.T) {
	now := testNow
	authority := &fakeIntegrationAuthority{receipts: map[string]fakeReceipt{}}
	service := integrationTestService(t, authority, &now)
	proposal := integrationGrantProposal()
	confirmation, err := service.PrepareIntegration(t.Context(), proposal)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*operator.IntegrationProposal){
		"verifier": func(p *operator.IntegrationProposal) { p.Verifier = strings.Repeat("cd", 32) },
		"label":    func(p *operator.IntegrationProposal) { p.Name = "other-laptop" },
		"grant":    func(p *operator.IntegrationProposal) { p.GrantID = "60000000-0000-4000-8000-000000000002" },
	} {
		changed := proposal
		mutate(&changed)
		if _, err := service.IssueIntegration(t.Context(), changed, confirmation); !errors.Is(err, operator.ErrInvalidConfirmation) {
			t.Fatalf("%s changed after confirmation err=%v", name, err)
		}
	}
	if _, err := service.RevokeIntegration(t.Context(), integrationRevokeProposal(), confirmation); !errors.Is(err, operator.ErrInvalidConfirmation) {
		t.Fatalf("grant confirmation authorized revoke: %v", err)
	}
	result, err := service.IssueIntegration(t.Context(), proposal, confirmation)
	if err != nil || result.Revision != 43 || len(authority.executes) != 1 || !authority.executes[0].ConfirmationExpiresAt.Equal(confirmation.ExpiresAt) {
		t.Fatalf("result=%+v executes=%+v err=%v", result, authority.executes, err)
	}
	now = now.Add(time.Hour)
	replayed, err := service.IssueIntegration(t.Context(), proposal, operator.Confirmation{})
	if err != nil || replayed != result || len(authority.executes) != 1 {
		t.Fatalf("lost-acknowledgement retry=%+v executes=%d err=%v", replayed, len(authority.executes), err)
	}
	changed := proposal
	changed.Name = "other-laptop"
	if _, err := service.IssueIntegration(t.Context(), changed, confirmation); !errors.Is(err, operator.ErrCommandIDReused) {
		t.Fatalf("changed retry err=%v", err)
	}
	fresh := integrationGrantProposal()
	fresh.CommandID = "30000000-0000-4000-8000-000000000066"
	if _, err := service.IssueIntegration(t.Context(), fresh, confirmation); !errors.Is(err, operator.ErrInvalidConfirmation) || len(authority.executes) != 1 {
		t.Fatalf("expired fresh commit err=%v executes=%d", err, len(authority.executes))
	}
}

func TestListIntegrationsNeverDisclosesVerifier(t *testing.T) {
	now := testNow
	created := testNow.Truncate(time.Second)
	authority := &fakeIntegrationAuthority{receipts: map[string]fakeReceipt{}, views: []core.IntegrationView{{ID: testGrantID, AccountID: testOldOwnerID,
		FamilyID: testFamilyID, ServerInstanceID: testInstanceID, Name: "claude-desktop-laptop", Scope: "family:read",
		Verifier: strings.Repeat("ab", 32), Status: "active", RecoveryEpoch: 1, CreatedAt: created, ExpiresAt: created.Add(7776000 * time.Second)}}}
	service := integrationTestService(t, authority, &now)
	listed, err := service.ListIntegrations(t.Context(), testFamilyID)
	if err != nil || len(listed) != 1 || listed[0].Verifier != "" || listed[0].ID != testGrantID || listed[0].Name != "claude-desktop-laptop" ||
		!listed[0].ExpiresAt.Equal(created.Add(7776000*time.Second)) {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	if authority.listed[0] != testFamilyID || authority.views[0].Verifier == "" {
		t.Fatalf("listing families=%v or mutated authority view", authority.listed)
	}
	if _, err := service.ListIntegrations(t.Context(), "not-a-family"); !errors.Is(err, operator.ErrInvalidRequest) || len(authority.listed) != 1 {
		t.Fatalf("invalid family err=%v calls=%d", err, len(authority.listed))
	}
}

func (backend *fakeOperatorSecurity) PreflightOperatorIntegration(_ context.Context, input core.OperatorIntegration) error {
	backend.integrationCalls = append(backend.integrationCalls, "preflight")
	backend.integrationInputs = append(backend.integrationInputs, input)
	return nil
}

func (backend *fakeOperatorSecurity) ReplayOperatorIntegration(_ context.Context, input core.OperatorIntegration) (core.Result, bool, error) {
	backend.integrationCalls = append(backend.integrationCalls, "replay")
	backend.integrationInputs = append(backend.integrationInputs, input)
	return backend.result, false, nil
}

func (backend *fakeOperatorSecurity) ExecuteOperatorIntegration(_ context.Context, input core.OperatorIntegration) (core.Result, error) {
	backend.integrationCalls = append(backend.integrationCalls, "execute")
	backend.integrationInputs = append(backend.integrationInputs, input)
	return backend.result, nil
}

func (backend *fakeOperatorSecurity) ListFamilyIntegrations(_ context.Context, familyID string) ([]core.IntegrationView, error) {
	backend.integrationCalls = append(backend.integrationCalls, "list:"+familyID)
	return backend.integrationViews, nil
}

func TestSecurityAuthorityMapsIntegrationExactlyAndGatesFreshWork(t *testing.T) {
	backend := &fakeOperatorSecurity{result: core.Result{CommandID: "30000000-0000-4000-8000-000000000064", Outcome: "committed", Code: "ok", Revision: 43}}
	gate := &fakeSecurityAuthorityGate{}
	adapter, err := operator.NewSecurityAuthority(backend, gate)
	if err != nil {
		t.Fatal(err)
	}
	deadline := testNow.Add(5 * time.Minute)
	for _, proposal := range []operator.IntegrationProposal{integrationGrantProposal(), integrationRevokeProposal()} {
		backend.integrationInputs, backend.integrationCalls = nil, nil
		command := operator.IntegrationCommand{Proposal: proposal, Audit: operator.Audit{OperatorID: testOperatorID, PolicyID: proposal.PolicyID,
			EvidenceDigest: strings.Repeat("ef", 32), ReasonCode: proposal.ReasonCode, ExpectedRevision: proposal.ExpectedRevision},
			ConfirmationExpiresAt: deadline}
		if err := adapter.PreflightIntegration(t.Context(), command); err != nil {
			t.Fatal(err)
		}
		if _, _, err := adapter.ReplayIntegration(t.Context(), command); err != nil {
			t.Fatal(err)
		}
		if result, err := adapter.ExecuteIntegration(t.Context(), command); err != nil || result.Revision != 43 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		want := core.OperatorIntegration{CommandID: proposal.CommandID, OperatorID: testOperatorID, Operation: proposal.Operation,
			GrantID: proposal.GrantID, FamilyID: proposal.FamilyID, AccountID: proposal.AccountID, Name: proposal.Name, Scope: proposal.Scope,
			Verifier: proposal.Verifier, ServerInstanceID: proposal.ServerInstanceID, RecoveryEpoch: proposal.RecoveryEpoch,
			ValiditySeconds: proposal.ValiditySeconds, PolicyID: proposal.PolicyID, EvidenceDigest: strings.Repeat("ef", 32),
			ReasonCode: proposal.ReasonCode, ExpectedRevision: proposal.ExpectedRevision, ConfirmationExpiresAt: deadline}
		if len(backend.integrationInputs) != 3 {
			t.Fatalf("calls=%v", backend.integrationCalls)
		}
		for _, got := range backend.integrationInputs {
			if got != want {
				t.Fatalf("%s mapped=%+v\nwant=%+v", proposal.Operation, got, want)
			}
		}
	}
	backend.integrationViews = []core.IntegrationView{{ID: testGrantID}}
	if views, err := adapter.ListIntegrations(t.Context(), testFamilyID); err != nil || len(views) != 1 ||
		backend.integrationCalls[len(backend.integrationCalls)-1] != "list:"+testFamilyID {
		t.Fatalf("views=%+v calls=%v err=%v", views, backend.integrationCalls, err)
	}

	gateErr := errors.New("authority not ready")
	gate.err, gate.checks, backend.integrationCalls = gateErr, 0, nil
	command := operator.IntegrationCommand{Proposal: integrationGrantProposal(), ConfirmationExpiresAt: deadline}
	if _, _, err := adapter.ReplayIntegration(t.Context(), command); err != nil || gate.checks != 0 {
		t.Fatalf("receipt probe was gated: checks=%d err=%v", gate.checks, err)
	}
	if err := adapter.PreflightIntegration(t.Context(), command); !errors.Is(err, gateErr) {
		t.Fatalf("preflight err=%v", err)
	}
	if _, err := adapter.ExecuteIntegration(t.Context(), command); !errors.Is(err, gateErr) {
		t.Fatalf("execute err=%v", err)
	}
	if _, err := adapter.ListIntegrations(t.Context(), testFamilyID); !errors.Is(err, gateErr) {
		t.Fatalf("list err=%v", err)
	}
	if gate.checks != 3 || len(backend.integrationCalls) != 1 || backend.integrationCalls[0] != "replay" {
		t.Fatalf("gated calls reached security: checks=%d calls=%v", gate.checks, backend.integrationCalls)
	}
}
