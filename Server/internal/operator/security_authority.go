package operator

import (
	"context"
	"errors"

	"babytracker/server/internal/authority"
)

// operatorSecurity is the authority's operator surface; *authority.Service
// implements it. Each Execute repeats the receipt lookup, then checks the
// confirmation deadline, ExpectedRevision (the family's audit seq) and every
// live precondition under the command's own row locks, in one transaction.
type operatorSecurity interface {
	PreflightOperatorRecoverOwner(context.Context, authority.OperatorRecoverOwner) error
	PreflightOperatorEntitlement(context.Context, authority.OperatorEntitlement) error
	ReplayOperatorRecoverOwner(context.Context, authority.OperatorRecoverOwner) (authority.Result, bool, error)
	ReplayOperatorEntitlement(context.Context, authority.OperatorEntitlement) (authority.Result, bool, error)
	ExecuteOperatorRecoverOwner(context.Context, authority.OperatorRecoverOwner) (authority.Result, error)
	ExecuteOperatorEntitlement(context.Context, authority.OperatorEntitlement) (authority.Result, error)
	PreflightOperatorIntegration(context.Context, authority.OperatorIntegration) error
	ReplayOperatorIntegration(context.Context, authority.OperatorIntegration) (authority.Result, bool, error)
	ExecuteOperatorIntegration(context.Context, authority.OperatorIntegration) (authority.Result, error)
	ListFamilyIntegrations(context.Context, string) ([]authority.IntegrationView, error)
	FamilyRevision(context.Context, string) (uint64, error)
}

var _ operatorSecurity = (*authority.Service)(nil)

// securityAuthorityGate is the node's readiness check: fresh operator work
// (preflight, commit, listing and revision reads) runs only on a ready node.
// The receipt probe is not gated, so an exact retry of a committed command
// still returns its stored result.
type securityAuthorityGate interface {
	Check(context.Context) error
}

// SecurityAuthority is the only production bridge from confirmed operator
// commands to the authority. It passes typed data directly; it does not
// mint an access.Principal or expose a generic authority callback.
type SecurityAuthority struct {
	security operatorSecurity
	gate     securityAuthorityGate
}

func NewSecurityAuthority(backend operatorSecurity, gate securityAuthorityGate) (*SecurityAuthority, error) {
	if backend == nil || gate == nil {
		return nil, errors.New("operator authority and readiness gate are required")
	}
	return &SecurityAuthority{security: backend, gate: gate}, nil
}

func (adapter *SecurityAuthority) check(ctx context.Context) error {
	if adapter.gate == nil {
		return nil
	}
	return adapter.gate.Check(ctx)
}

// FamilyRevision is the family's current audit seq: the value an operator
// passes as --expected-revision for a command on that family. A command
// prepared at an older revision is stale_security_revision.
func (adapter *SecurityAuthority) FamilyRevision(ctx context.Context, familyID string) (uint64, error) {
	if !validUUID(familyID) {
		return 0, ErrInvalidRequest
	}
	if err := adapter.check(ctx); err != nil {
		return 0, err
	}
	return adapter.security.FamilyRevision(ctx, familyID)
}

func (adapter *SecurityAuthority) PreflightRecovery(ctx context.Context, command RecoveryCommand) error {
	if err := adapter.check(ctx); err != nil {
		return err
	}
	return adapter.security.PreflightOperatorRecoverOwner(ctx, recoverySecurityInput(command))
}

func (adapter *SecurityAuthority) PreflightEntitlement(ctx context.Context, command EntitlementCommand) error {
	if err := adapter.check(ctx); err != nil {
		return err
	}
	return adapter.security.PreflightOperatorEntitlement(ctx, entitlementSecurityInput(command))
}

func (adapter *SecurityAuthority) ReplayRecovery(ctx context.Context, command RecoveryCommand) (authority.Result, bool, error) {
	return adapter.security.ReplayOperatorRecoverOwner(ctx, recoverySecurityInput(command))
}

func (adapter *SecurityAuthority) ReplayEntitlement(ctx context.Context, command EntitlementCommand) (authority.Result, bool, error) {
	return adapter.security.ReplayOperatorEntitlement(ctx, entitlementSecurityInput(command))
}

func (adapter *SecurityAuthority) RecoverOwner(ctx context.Context, command RecoveryCommand) (authority.Result, error) {
	if err := adapter.check(ctx); err != nil {
		return authority.Result{}, err
	}
	return adapter.security.ExecuteOperatorRecoverOwner(ctx, recoverySecurityInput(command))
}

func (adapter *SecurityAuthority) GrantEntitlement(ctx context.Context, command EntitlementCommand) (authority.Result, error) {
	if command.Proposal.Operation != "grant" {
		return authority.Result{}, ErrInvalidRequest
	}
	if err := adapter.check(ctx); err != nil {
		return authority.Result{}, err
	}
	return adapter.security.ExecuteOperatorEntitlement(ctx, entitlementSecurityInput(command))
}

func (adapter *SecurityAuthority) SuspendEntitlement(ctx context.Context, command EntitlementCommand) (authority.Result, error) {
	if command.Proposal.Operation != "suspend" {
		return authority.Result{}, ErrInvalidRequest
	}
	if err := adapter.check(ctx); err != nil {
		return authority.Result{}, err
	}
	return adapter.security.ExecuteOperatorEntitlement(ctx, entitlementSecurityInput(command))
}

func (adapter *SecurityAuthority) PreflightIntegration(ctx context.Context, command IntegrationCommand) error {
	if err := adapter.check(ctx); err != nil {
		return err
	}
	return adapter.security.PreflightOperatorIntegration(ctx, integrationSecurityInput(command))
}

func (adapter *SecurityAuthority) ReplayIntegration(ctx context.Context, command IntegrationCommand) (authority.Result, bool, error) {
	return adapter.security.ReplayOperatorIntegration(ctx, integrationSecurityInput(command))
}

func (adapter *SecurityAuthority) ExecuteIntegration(ctx context.Context, command IntegrationCommand) (authority.Result, error) {
	if err := adapter.check(ctx); err != nil {
		return authority.Result{}, err
	}
	return adapter.security.ExecuteOperatorIntegration(ctx, integrationSecurityInput(command))
}

func (adapter *SecurityAuthority) ListIntegrations(ctx context.Context, familyID string) ([]authority.IntegrationView, error) {
	if err := adapter.check(ctx); err != nil {
		return nil, err
	}
	return adapter.security.ListFamilyIntegrations(ctx, familyID)
}

func recoverySecurityInput(command RecoveryCommand) authority.OperatorRecoverOwner {
	return authority.OperatorRecoverOwner{
		CommandID: command.Proposal.CommandID, OperatorID: command.Audit.OperatorID, FamilyID: command.Proposal.FamilyID,
		OldOwnerAccountID: command.Proposal.OldOwnerID, NewOwnerAccountID: command.Proposal.NewOwnerID,
		PolicyID: command.Audit.PolicyID, EvidenceDigest: command.Audit.EvidenceDigest, ReasonCode: command.Audit.ReasonCode,
		ExpectedRevision: command.Audit.ExpectedRevision, ConfirmationExpiresAt: command.ConfirmationExpiresAt,
	}
}

func entitlementSecurityInput(command EntitlementCommand) authority.OperatorEntitlement {
	return authority.OperatorEntitlement{
		CommandID: command.Proposal.CommandID, OperatorID: command.Audit.OperatorID, FamilyID: command.Proposal.FamilyID,
		Operation: command.Proposal.Operation, Key: command.Proposal.Key, Source: command.Proposal.Source, ExpiresAt: command.Proposal.ExpiresAt,
		PolicyID: command.Audit.PolicyID, EvidenceDigest: command.Audit.EvidenceDigest, ReasonCode: command.Audit.ReasonCode,
		ExpectedRevision: command.Audit.ExpectedRevision, ConfirmationExpiresAt: command.ConfirmationExpiresAt,
	}
}

func integrationSecurityInput(command IntegrationCommand) authority.OperatorIntegration {
	return authority.OperatorIntegration{
		CommandID: command.Proposal.CommandID, OperatorID: command.Audit.OperatorID, Operation: command.Proposal.Operation,
		GrantID: command.Proposal.GrantID, FamilyID: command.Proposal.FamilyID, AccountID: command.Proposal.AccountID,
		Name: command.Proposal.Name, Scope: command.Proposal.Scope, Verifier: command.Proposal.Verifier,
		ServerInstanceID: command.Proposal.ServerInstanceID, RecoveryEpoch: command.Proposal.RecoveryEpoch,
		ValiditySeconds: command.Proposal.ValiditySeconds, PolicyID: command.Audit.PolicyID, EvidenceDigest: command.Audit.EvidenceDigest,
		ReasonCode: command.Audit.ReasonCode, ExpectedRevision: command.Audit.ExpectedRevision, ConfirmationExpiresAt: command.ConfirmationExpiresAt,
	}
}

var (
	_ Authority            = (*SecurityAuthority)(nil)
	_ IntegrationAuthority = (*SecurityAuthority)(nil)
)
