package operator

import (
	"context"
	"regexp"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/protocol"
)

const (
	integrationGrantReason  = "integration_issued"
	integrationRevokeReason = "integration_revoked"
)

var integrationVerifierPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// IntegrationProposal is the confirmed operator form of one private MCP
// credential issuance ("grant") or revocation ("revoke"). It mirrors the
// authority's OperatorIntegration plus the existing operator
// audit fields. Issuance carries only the credential verifier, never the
// secret, and the fixed ValiditySeconds rather than an absolute expiry: the
// committed event time becomes the issuance time. Revocation names the stored
// grant identity and bindings and carries no label, scope, verifier or
// validity. Name is an operator-chosen client label persisted in the
// append-only authority log, so it must never contain a person's name.
type IntegrationProposal struct {
	CommandID         string `json:"command_id"`
	Operation         string `json:"operation"`
	GrantID           string `json:"grant_id"`
	FamilyID          string `json:"family_id"`
	AccountID         string `json:"account_id"`
	Name              string `json:"name,omitempty"`
	Scope             string `json:"scope,omitempty"`
	Verifier          string `json:"verifier,omitempty"`
	ServerInstanceID  string `json:"server_instance_id"`
	RecoveryEpoch     uint64 `json:"recovery_epoch"`
	ValiditySeconds   uint64 `json:"validity_seconds,omitempty"`
	PolicyID          string `json:"policy_id"`
	EvidenceReference string `json:"evidence_reference"`
	ReasonCode        string `json:"reason_code"`
	ExpectedRevision  uint64 `json:"expected_revision"`
}

type IntegrationCommand struct {
	Proposal              IntegrationProposal
	Audit                 Audit
	Receipt               ReceiptKey
	ConfirmationExpiresAt time.Time
}

// IntegrationAuthority is the integration-credential seam, kept separate from
// Authority so enabling one command family never implies another. As with
// Authority, ReplayIntegration is the preliminary durable-receipt probe and
// ExecuteIntegration must repeat the receipt lookup, then compare its trusted
// clock with ConfirmationExpiresAt and recheck every live precondition under
// one exclusive authority fence. ListIntegrations reads stored metadata only.
type IntegrationAuthority interface {
	PreflightIntegration(context.Context, IntegrationCommand) error
	ReplayIntegration(context.Context, IntegrationCommand) (authority.Result, bool, error)
	ExecuteIntegration(context.Context, IntegrationCommand) (authority.Result, error)
	ListIntegrations(context.Context, string) ([]authority.IntegrationView, error)
}

// PrepareIntegration validates and preflights one issuance or revocation and
// returns a short-lived confirmation over the complete canonical proposal. It
// writes nothing and fixes no issuance time.
func (service *Service) PrepareIntegration(ctx context.Context, proposal IntegrationProposal) (Confirmation, error) {
	if err := service.checkIntegrationProposal(proposal); err != nil {
		return Confirmation{}, err
	}
	_, command, err := service.integrationCommand(proposal, Confirmation{})
	if err != nil {
		return Confirmation{}, err
	}
	if err := service.integrationAuthority.PreflightIntegration(ctx, command); err != nil {
		return Confirmation{}, err
	}
	return service.prepare(integrationOperation(proposal), proposal)
}

// IssueIntegration commits a confirmed issuance, or returns the original
// receipt of an exact retry even after its confirmation or grant has expired.
func (service *Service) IssueIntegration(ctx context.Context, proposal IntegrationProposal, confirmation Confirmation) (authority.Result, error) {
	if service != nil && service.integrationPolicyID != "" && proposal.Operation != "grant" {
		return authority.Result{}, ErrInvalidRequest
	}
	return service.commitIntegration(ctx, proposal, confirmation)
}

// RevokeIntegration commits a confirmed revocation, or returns the original
// receipt of an exact retry.
func (service *Service) RevokeIntegration(ctx context.Context, proposal IntegrationProposal, confirmation Confirmation) (authority.Result, error) {
	if service != nil && service.integrationPolicyID != "" && proposal.Operation != "revoke" {
		return authority.Result{}, ErrInvalidRequest
	}
	return service.commitIntegration(ctx, proposal, confirmation)
}

// ListIntegrations returns one family's stored grant metadata. The verifier
// is removed: listing output is operator metadata, never credential material.
func (service *Service) ListIntegrations(ctx context.Context, familyID string) ([]authority.IntegrationView, error) {
	if service == nil || service.integrationPolicyID == "" {
		return nil, ErrOperatorPolicyRequired
	}
	if !validUUID(familyID) {
		return nil, ErrInvalidRequest
	}
	if service.integrationAuthority == nil {
		return nil, ErrUnavailable
	}
	views, err := service.integrationAuthority.ListIntegrations(ctx, familyID)
	if err != nil {
		return nil, err
	}
	listed := make([]authority.IntegrationView, 0, len(views))
	for _, view := range views {
		view.Verifier = ""
		listed = append(listed, view)
	}
	return listed, nil
}

func (service *Service) commitIntegration(ctx context.Context, proposal IntegrationProposal, confirmation Confirmation) (authority.Result, error) {
	if err := service.checkIntegrationProposal(proposal); err != nil {
		return authority.Result{}, err
	}
	canonical, command, err := service.integrationCommand(proposal, confirmation)
	if err != nil {
		return authority.Result{}, err
	}
	if result, replayed, err := service.integrationAuthority.ReplayIntegration(ctx, command); err != nil || replayed {
		return result, err
	}
	if err := service.verifyConfirmation(integrationOperation(proposal), canonical, confirmation); err != nil {
		return authority.Result{}, err
	}
	return service.integrationAuthority.ExecuteIntegration(ctx, command)
}

func (service *Service) checkIntegrationProposal(proposal IntegrationProposal) error {
	if service == nil || service.integrationPolicyID == "" {
		return ErrOperatorPolicyRequired
	}
	if err := validateIntegrationProposal(proposal); err != nil {
		return err
	}
	if proposal.PolicyID != service.integrationPolicyID {
		return ErrOperatorPolicyRequired
	}
	if service.integrationAuthority == nil {
		return ErrUnavailable
	}
	return nil
}

func (service *Service) integrationCommand(proposal IntegrationProposal, confirmation Confirmation) ([]byte, IntegrationCommand, error) {
	canonical, receipt, err := service.receiptCoordinates(integrationOperation(proposal), proposal.CommandID, proposal)
	if err != nil {
		return nil, IntegrationCommand{}, err
	}
	return canonical, IntegrationCommand{Proposal: proposal, Audit: service.audit(proposal.PolicyID, proposal.EvidenceReference, proposal.ReasonCode, proposal.ExpectedRevision),
		Receipt: receipt, ConfirmationExpiresAt: confirmation.ExpiresAt}, nil
}

func integrationOperation(proposal IntegrationProposal) string {
	return "integration-" + proposal.Operation
}

func validateIntegrationProposal(proposal IntegrationProposal) error {
	if !validUUID(proposal.CommandID) || !validUUID(proposal.GrantID) || !validUUID(proposal.FamilyID) || !validUUID(proposal.AccountID) ||
		!validUUID(proposal.ServerInstanceID) || proposal.RecoveryEpoch == 0 || proposal.RecoveryEpoch > protocol.MaxSafeInteger ||
		!policyIDPattern.MatchString(proposal.PolicyID) || !evidencePattern.MatchString(proposal.EvidenceReference) ||
		proposal.ExpectedRevision == 0 || proposal.ExpectedRevision > protocol.MaxSafeInteger {
		return ErrInvalidRequest
	}
	switch proposal.Operation {
	case "grant":
		if proposal.ReasonCode != integrationGrantReason || !authority.ValidIntegrationName(proposal.Name) ||
			proposal.Scope != authority.IntegrationScopeFamilyRead || !integrationVerifierPattern.MatchString(proposal.Verifier) ||
			proposal.ValiditySeconds != authority.IntegrationValiditySeconds {
			return ErrInvalidRequest
		}
	case "revoke":
		if proposal.ReasonCode != integrationRevokeReason || proposal.Name != "" || proposal.Scope != "" || proposal.Verifier != "" || proposal.ValiditySeconds != 0 {
			return ErrInvalidRequest
		}
	default:
		return ErrInvalidRequest
	}
	return nil
}
