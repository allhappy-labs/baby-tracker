package operator_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/access"
	core "babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"babytracker/server/internal/operator"
)

type readyGate struct{}

func (readyGate) Check(context.Context) error { return nil }

// ownedFamily is a family created through the authority by a signed-in owner.
func ownedFamily(t *testing.T, f *authoritytest.Fixture) (owner, family string) {
	t.Helper()
	owner = f.Account(t)
	device := f.Device(t, owner)
	session, _, _ := f.Session(t, owner, device, "account", "", "")
	principal := access.Principal{AccountID: owner, DeviceID: device, SessionID: session, RecoveryEpoch: authoritytest.RecoveryEpoch,
		SecurityRevision: 1, Scope: "account", SessionScope: "account"}
	family = authoritytest.NewID(t)
	if _, err := f.Service.CreateFamily(t.Context(), core.CreateFamily{CommandID: authoritytest.NewID(t), OwnerAccountID: owner, FamilyID: family,
		DisplayName: "The Smiths", Principal: principal}); err != nil {
		t.Fatal(err)
	}
	return owner, family
}

func realOperator(t *testing.T, f *authoritytest.Fixture) (*operator.Service, *operator.SecurityAuthority) {
	t.Helper()
	adapter, err := operator.NewSecurityAuthority(f.Service, readyGate{})
	if err != nil {
		t.Fatal(err)
	}
	var key [32]byte
	for index := range key {
		key[index] = 0x42
	}
	service, err := operator.New(operator.Options{Authority: adapter, IntegrationAuthority: adapter, OperatorID: testOperatorID,
		EntitlementPolicyID: "entitlement-policy-test-v1", IntegrationPolicyID: testPolicyMCP, ConfirmationKey: key,
		Now: func() time.Time { return f.Now }})
	if err != nil {
		t.Fatal(err)
	}
	return service, adapter
}

// The operator's expected revision is the family's audit seq, read through
// the adapter: a proposal at the current revision prepares and commits and
// advances it by one; a proposal at any other revision is
// stale_security_revision, at preparation and at commit; an exact retry
// returns the committed receipt.
func TestOperatorCommandsUseTheFamilyAuditSeqAsExpectedRevision(t *testing.T) {
	f := authoritytest.New(t)
	_, family := ownedFamily(t, f)
	service, adapter := realOperator(t, f)
	revision, err := adapter.FamilyRevision(t.Context(), family)
	if err != nil || revision != 1 {
		t.Fatalf("revision=%d err=%v", revision, err)
	}
	proposal := operator.EntitlementProposal{CommandID: authoritytest.NewID(t), FamilyID: family, Operation: "grant", Key: "family_sync",
		Source: "hosted_beta", ExpiresAt: time.Date(2027, 10, 4, 12, 0, 0, 0, time.UTC), PolicyID: "entitlement-policy-test-v1",
		EvidenceReference: "entitlement_case_0001", ReasonCode: "beta_extension", ExpectedRevision: revision + 1}
	if _, err := service.PrepareEntitlement(t.Context(), proposal); operator.Code(err) != "stale_security_revision" {
		t.Fatalf("stale prepare code=%q err=%v", operator.Code(err), err)
	}
	proposal.ExpectedRevision = revision
	confirmation, err := service.PrepareEntitlement(t.Context(), proposal)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.GrantEntitlement(t.Context(), proposal, confirmation)
	if err != nil || result.Revision != revision+1 || result.CommandID != proposal.CommandID {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if replay, err := service.GrantEntitlement(t.Context(), proposal, confirmation); err != nil || replay != result {
		t.Fatalf("replay=%+v err=%v want %+v", replay, err, result)
	}
	if now, err := adapter.FamilyRevision(t.Context(), family); err != nil || now != revision+1 {
		t.Fatalf("revision after commit=%d err=%v", now, err)
	}
	// A later proposal prepared at the old revision is stale.
	stale := proposal
	stale.CommandID, stale.ExpiresAt = authoritytest.NewID(t), stale.ExpiresAt.Add(24*time.Hour)
	if _, err := service.PrepareEntitlement(t.Context(), stale); operator.Code(err) != "stale_security_revision" {
		t.Fatalf("stale after commit code=%q err=%v", operator.Code(err), err)
	}
}

// An integration grant issued through the operator service and the real
// authority commits at the family's revision, and listing returns it
// without its verifier.
func TestOperatorIntegrationIssuesThroughTheAuthority(t *testing.T) {
	f := authoritytest.New(t)
	owner, family := ownedFamily(t, f)
	service, adapter := realOperator(t, f)
	revision, err := adapter.FamilyRevision(t.Context(), family)
	if err != nil {
		t.Fatal(err)
	}
	proposal := operator.IntegrationProposal{CommandID: authoritytest.NewID(t), Operation: "grant", GrantID: authoritytest.NewID(t),
		FamilyID: family, AccountID: owner, Name: "claude-desktop-laptop", Scope: "family:read", Verifier: strings.Repeat("ab", 32),
		ServerInstanceID: f.InstanceID, RecoveryEpoch: authoritytest.RecoveryEpoch, ValiditySeconds: 7776000, PolicyID: testPolicyMCP,
		EvidenceReference: "mcp_issue_0001", ReasonCode: "integration_issued", ExpectedRevision: revision}
	confirmation, err := service.PrepareIntegration(t.Context(), proposal)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.IssueIntegration(t.Context(), proposal, confirmation)
	if err != nil || result.Revision != revision+1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	views, err := service.ListIntegrations(t.Context(), family)
	if err != nil || len(views) != 1 || views[0].ID != proposal.GrantID || views[0].Status != "active" || views[0].Verifier != "" ||
		!views[0].ExpiresAt.Equal(views[0].CreatedAt.Add(7776000*time.Second)) {
		t.Fatalf("views=%+v err=%v", views, err)
	}
}
