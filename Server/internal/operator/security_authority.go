package operator

import (
	"context"
	"errors"

	"babytracker/server/internal/security"
)

type operatorSecurity interface {
	PreflightOperatorRecovery(context.Context, security.OperatorRecoveryInput) error
	PreflightOperatorEntitlement(context.Context, security.OperatorEntitlementInput) error
	ReplayOperatorRecovery(context.Context, security.OperatorRecoveryInput, security.OperatorAuthorityGuard) (security.CommandResult, bool, error)
	ReplayOperatorEntitlement(context.Context, security.OperatorEntitlementInput, security.OperatorAuthorityGuard) (security.CommandResult, bool, error)
	ExecuteOperatorRecovery(context.Context, security.OperatorRecoveryInput, security.OperatorAuthorityGuard) (security.CommandResult, error)
	ExecuteOperatorEntitlement(context.Context, security.OperatorEntitlementInput, security.OperatorAuthorityGuard) (security.CommandResult, error)
}

type securityAuthorityGate interface {
	Check(context.Context) error
	ValidateOperatorAuthorityUnderFence(context.Context, security.Head) error
}

// SecurityAuthority is the only production bridge from confirmed operator
// commands to security authority. It passes typed data directly; it does not
// mint an access.Principal or expose a generic authority callback.
type SecurityAuthority struct {
	security operatorSecurity
	gate     securityAuthorityGate
}

func NewSecurityAuthority(authority operatorSecurity, gate securityAuthorityGate) (*SecurityAuthority, error) {
	if authority == nil || gate == nil {
		return nil, errors.New("operator security authority and recovery gate are required")
	}
	return &SecurityAuthority{security: authority, gate: gate}, nil
}

func (authority *SecurityAuthority) check(ctx context.Context) error {
	if authority.gate == nil {
		return nil
	}
	return authority.gate.Check(ctx)
}

func (authority *SecurityAuthority) PreflightRecovery(ctx context.Context, command RecoveryCommand) error {
	if err := authority.check(ctx); err != nil {
		return err
	}
	return authority.security.PreflightOperatorRecovery(ctx, recoverySecurityInput(command))
}

func (authority *SecurityAuthority) PreflightEntitlement(ctx context.Context, command EntitlementCommand) error {
	if err := authority.check(ctx); err != nil {
		return err
	}
	return authority.security.PreflightOperatorEntitlement(ctx, entitlementSecurityInput(command))
}

func (authority *SecurityAuthority) ReplayRecovery(ctx context.Context, command RecoveryCommand) (security.CommandResult, bool, error) {
	return authority.security.ReplayOperatorRecovery(ctx, recoverySecurityInput(command), authority.gate)
}

func (authority *SecurityAuthority) ReplayEntitlement(ctx context.Context, command EntitlementCommand) (security.CommandResult, bool, error) {
	return authority.security.ReplayOperatorEntitlement(ctx, entitlementSecurityInput(command), authority.gate)
}

func (authority *SecurityAuthority) RecoverOwner(ctx context.Context, command RecoveryCommand) (security.CommandResult, error) {
	if err := authority.check(ctx); err != nil {
		return security.CommandResult{}, err
	}
	return authority.security.ExecuteOperatorRecovery(ctx, recoverySecurityInput(command), authority.gate)
}

func (authority *SecurityAuthority) GrantEntitlement(ctx context.Context, command EntitlementCommand) (security.CommandResult, error) {
	if command.Proposal.Operation != "grant" {
		return security.CommandResult{}, ErrInvalidRequest
	}
	if err := authority.check(ctx); err != nil {
		return security.CommandResult{}, err
	}
	return authority.security.ExecuteOperatorEntitlement(ctx, entitlementSecurityInput(command), authority.gate)
}

func (authority *SecurityAuthority) SuspendEntitlement(ctx context.Context, command EntitlementCommand) (security.CommandResult, error) {
	if command.Proposal.Operation != "suspend" {
		return security.CommandResult{}, ErrInvalidRequest
	}
	if err := authority.check(ctx); err != nil {
		return security.CommandResult{}, err
	}
	return authority.security.ExecuteOperatorEntitlement(ctx, entitlementSecurityInput(command), authority.gate)
}

func recoverySecurityInput(command RecoveryCommand) security.OperatorRecoveryInput {
	return security.OperatorRecoveryInput{
		CommandID: command.Proposal.CommandID, OperatorID: command.Audit.OperatorID, FamilyID: command.Proposal.FamilyID,
		OldOwnerAccountID: command.Proposal.OldOwnerID, NewOwnerAccountID: command.Proposal.NewOwnerID,
		PolicyID: command.Audit.PolicyID, EvidenceDigest: command.Audit.EvidenceDigest, ReasonCode: command.Audit.ReasonCode,
		ExpectedRevision: command.Audit.ExpectedRevision, ConfirmationExpiresAt: command.ConfirmationExpiresAt,
	}
}

func entitlementSecurityInput(command EntitlementCommand) security.OperatorEntitlementInput {
	return security.OperatorEntitlementInput{
		CommandID: command.Proposal.CommandID, OperatorID: command.Audit.OperatorID, FamilyID: command.Proposal.FamilyID,
		Operation: command.Proposal.Operation, Key: command.Proposal.Key, Source: command.Proposal.Source, ExpiresAt: command.Proposal.ExpiresAt,
		PolicyID: command.Audit.PolicyID, EvidenceDigest: command.Audit.EvidenceDigest, ReasonCode: command.Audit.ReasonCode,
		ExpectedRevision: command.Audit.ExpectedRevision, ConfirmationExpiresAt: command.ConfirmationExpiresAt,
	}
}

var _ Authority = (*SecurityAuthority)(nil)
