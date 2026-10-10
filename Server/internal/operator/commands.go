package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/protocol"
)

const confirmationLifetime = 5 * time.Minute

var (
	canonicalUUID    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	policyIDPattern  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,127}$`)
	evidencePattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)
	confirmationHash = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type codedError string

func (err codedError) Error() string { return string(err) }

var (
	ErrInvalidRequest         = codedError("invalid_request")
	ErrOperatorPolicyRequired = codedError("operator_policy_required")
	ErrInvalidConfirmation    = codedError("invalid_confirmation")
	ErrCommandIDReused        = codedError("command_id_reused")
	ErrUnavailable            = codedError("service_unavailable")
)

func Code(err error) string {
	if err == nil {
		return ""
	}
	var coded codedError
	if errors.As(err, &coded) {
		return string(coded)
	}
	return authority.CodeOf(err)
}

type RecoveryProposal struct {
	CommandID         string `json:"command_id"`
	FamilyID          string `json:"family_id"`
	OldOwnerID        string `json:"old_owner_id"`
	NewOwnerID        string `json:"new_owner_id"`
	PolicyID          string `json:"policy_id"`
	EvidenceReference string `json:"evidence_reference"`
	ReasonCode        string `json:"reason_code"`
	ExpectedRevision  uint64 `json:"expected_revision"`
}

type EntitlementProposal struct {
	CommandID         string    `json:"command_id"`
	FamilyID          string    `json:"family_id"`
	Operation         string    `json:"operation"`
	Key               string    `json:"key"`
	Source            string    `json:"source"`
	ExpiresAt         time.Time `json:"expires_at"`
	PolicyID          string    `json:"policy_id"`
	EvidenceReference string    `json:"evidence_reference"`
	ReasonCode        string    `json:"reason_code"`
	ExpectedRevision  uint64    `json:"expected_revision"`
}

type Confirmation struct {
	Digest    string    `json:"digest"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Audit struct {
	OperatorID, PolicyID, EvidenceDigest, ReasonCode string
	ExpectedRevision                                 uint64
}

type ReceiptKey struct {
	Operation, OperatorID, CommandID, ProposalDigest string
}

type RecoveryCommand struct {
	Proposal              RecoveryProposal
	Audit                 Audit
	Receipt               ReceiptKey
	ConfirmationExpiresAt time.Time
}

type EntitlementCommand struct {
	Proposal              EntitlementProposal
	Audit                 Audit
	Receipt               ReceiptKey
	ConfirmationExpiresAt time.Time
}

// Authority is the lifecycle integration seam. Replay is the preliminary
// durable-receipt probe that lets an exact retry succeed before the service
// considers confirmation expiry. Each mutation method must repeat that receipt
// lookup and recheck every proposal/audit precondition under one exclusive
// authority fence. An exact receipt wins even after expiry; otherwise the
// implementation must compare its injected trusted clock with the authenticated
// ConfirmationExpiresAt before creating a new commit while still holding the
// fence. The service's earlier check cannot authorize work queued across expiry.
type Authority interface {
	PreflightRecovery(context.Context, RecoveryCommand) error
	PreflightEntitlement(context.Context, EntitlementCommand) error
	ReplayRecovery(context.Context, RecoveryCommand) (authority.Result, bool, error)
	ReplayEntitlement(context.Context, EntitlementCommand) (authority.Result, bool, error)
	RecoverOwner(context.Context, RecoveryCommand) (authority.Result, error)
	GrantEntitlement(context.Context, EntitlementCommand) (authority.Result, error)
	SuspendEntitlement(context.Context, EntitlementCommand) (authority.Result, error)
}

type Options struct {
	Authority                     Authority
	IntegrationAuthority          IntegrationAuthority
	OperatorID, OwnershipPolicyID string
	EntitlementPolicyID           string
	IntegrationPolicyID           string
	ConfirmationKey               [32]byte
	Now                           func() time.Time
}

type Service struct {
	authority                     Authority
	integrationAuthority          IntegrationAuthority
	operatorID, ownershipPolicyID string
	entitlementPolicyID           string
	integrationPolicyID           string
	confirmationKey               [32]byte
	now                           func() time.Time
}

func New(options Options) (*Service, error) {
	if options.Now == nil {
		return nil, errors.New("operator service requires a clock")
	}
	enabled := options.OwnershipPolicyID != "" || options.EntitlementPolicyID != ""
	if enabled && (options.Authority == nil || !validUUID(options.OperatorID) || options.ConfirmationKey == ([32]byte{})) {
		return nil, errors.New("enabled operator service requires authority, operator ID, and confirmation key")
	}
	if options.IntegrationPolicyID != "" && (options.IntegrationAuthority == nil || !validUUID(options.OperatorID) || options.ConfirmationKey == ([32]byte{})) {
		return nil, errors.New("enabled integration commands require integration authority, operator ID, and confirmation key")
	}
	if options.OwnershipPolicyID != "" && !policyIDPattern.MatchString(options.OwnershipPolicyID) {
		return nil, errors.New("ownership policy ID is invalid")
	}
	if options.EntitlementPolicyID != "" && !policyIDPattern.MatchString(options.EntitlementPolicyID) {
		return nil, errors.New("entitlement policy ID is invalid")
	}
	if options.IntegrationPolicyID != "" && !policyIDPattern.MatchString(options.IntegrationPolicyID) {
		return nil, errors.New("integration policy ID is invalid")
	}
	return &Service{authority: options.Authority, integrationAuthority: options.IntegrationAuthority, operatorID: options.OperatorID,
		ownershipPolicyID: options.OwnershipPolicyID, entitlementPolicyID: options.EntitlementPolicyID, integrationPolicyID: options.IntegrationPolicyID,
		confirmationKey: options.ConfirmationKey, now: options.Now}, nil
}

func (service *Service) PrepareRecovery(ctx context.Context, proposal RecoveryProposal) (Confirmation, error) {
	if service == nil || service.ownershipPolicyID == "" {
		return Confirmation{}, ErrOperatorPolicyRequired
	}
	if err := validateRecoveryProposal(proposal); err != nil {
		return Confirmation{}, err
	}
	if proposal.PolicyID != service.ownershipPolicyID {
		return Confirmation{}, ErrOperatorPolicyRequired
	}
	if service.authority == nil {
		return Confirmation{}, ErrUnavailable
	}
	_, command, err := service.recoveryCommand(proposal, Confirmation{})
	if err != nil {
		return Confirmation{}, err
	}
	if err := service.authority.PreflightRecovery(ctx, command); err != nil {
		return Confirmation{}, err
	}
	return service.prepare("ownership-recover", proposal)
}

func (service *Service) RecoverOwner(ctx context.Context, proposal RecoveryProposal, confirmation Confirmation) (authority.Result, error) {
	if service == nil || service.ownershipPolicyID == "" {
		return authority.Result{}, ErrOperatorPolicyRequired
	}
	if err := validateRecoveryProposal(proposal); err != nil {
		return authority.Result{}, err
	}
	if proposal.PolicyID != service.ownershipPolicyID {
		return authority.Result{}, ErrOperatorPolicyRequired
	}
	canonical, command, err := service.recoveryCommand(proposal, confirmation)
	if err != nil {
		return authority.Result{}, err
	}
	if result, replayed, err := service.authority.ReplayRecovery(ctx, command); err != nil || replayed {
		return result, err
	}
	if err := service.verifyConfirmation("ownership-recover", canonical, confirmation); err != nil {
		return authority.Result{}, err
	}
	return service.authority.RecoverOwner(ctx, command)
}

func (service *Service) PrepareEntitlement(ctx context.Context, proposal EntitlementProposal) (Confirmation, error) {
	if service == nil || service.entitlementPolicyID == "" {
		return Confirmation{}, ErrOperatorPolicyRequired
	}
	if err := validateEntitlementProposal(proposal); err != nil {
		return Confirmation{}, err
	}
	if proposal.PolicyID != service.entitlementPolicyID {
		return Confirmation{}, ErrOperatorPolicyRequired
	}
	if service.authority == nil {
		return Confirmation{}, ErrUnavailable
	}
	_, command, err := service.entitlementCommand(proposal, Confirmation{})
	if err != nil {
		return Confirmation{}, err
	}
	if err := service.authority.PreflightEntitlement(ctx, command); err != nil {
		return Confirmation{}, err
	}
	return service.prepare("entitlement-"+proposal.Operation, proposal)
}

func (service *Service) GrantEntitlement(ctx context.Context, proposal EntitlementProposal, confirmation Confirmation) (authority.Result, error) {
	if proposal.Operation != "grant" {
		return authority.Result{}, ErrInvalidRequest
	}
	return service.commitEntitlement(ctx, proposal, confirmation)
}

func (service *Service) SuspendEntitlement(ctx context.Context, proposal EntitlementProposal, confirmation Confirmation) (authority.Result, error) {
	if proposal.Operation != "suspend" {
		return authority.Result{}, ErrInvalidRequest
	}
	return service.commitEntitlement(ctx, proposal, confirmation)
}

func (service *Service) commitEntitlement(ctx context.Context, proposal EntitlementProposal, confirmation Confirmation) (authority.Result, error) {
	if service == nil || service.entitlementPolicyID == "" {
		return authority.Result{}, ErrOperatorPolicyRequired
	}
	if err := validateEntitlementProposal(proposal); err != nil {
		return authority.Result{}, err
	}
	if proposal.PolicyID != service.entitlementPolicyID {
		return authority.Result{}, ErrOperatorPolicyRequired
	}
	operation := "entitlement-" + proposal.Operation
	canonical, command, err := service.entitlementCommand(proposal, confirmation)
	if err != nil {
		return authority.Result{}, err
	}
	if result, replayed, err := service.authority.ReplayEntitlement(ctx, command); err != nil || replayed {
		return result, err
	}
	if err := service.verifyConfirmation(operation, canonical, confirmation); err != nil {
		return authority.Result{}, err
	}
	if proposal.Operation == "grant" {
		return service.authority.GrantEntitlement(ctx, command)
	}
	return service.authority.SuspendEntitlement(ctx, command)
}

func (service *Service) recoveryCommand(proposal RecoveryProposal, confirmation Confirmation) ([]byte, RecoveryCommand, error) {
	canonical, receipt, err := service.commitCoordinates("ownership-recover", proposal.CommandID, proposal)
	if err != nil {
		return nil, RecoveryCommand{}, err
	}
	return canonical, RecoveryCommand{Proposal: proposal, Audit: service.audit(proposal.PolicyID, proposal.EvidenceReference, proposal.ReasonCode, proposal.ExpectedRevision),
		Receipt: receipt, ConfirmationExpiresAt: confirmation.ExpiresAt}, nil
}

func (service *Service) entitlementCommand(proposal EntitlementProposal, confirmation Confirmation) ([]byte, EntitlementCommand, error) {
	canonical, receipt, err := service.commitCoordinates("entitlement-"+proposal.Operation, proposal.CommandID, proposal)
	if err != nil {
		return nil, EntitlementCommand{}, err
	}
	return canonical, EntitlementCommand{Proposal: proposal, Audit: service.audit(proposal.PolicyID, proposal.EvidenceReference, proposal.ReasonCode, proposal.ExpectedRevision),
		Receipt: receipt, ConfirmationExpiresAt: confirmation.ExpiresAt}, nil
}

func (service *Service) prepare(operation string, proposal any) (Confirmation, error) {
	canonical, err := json.Marshal(proposal)
	if err != nil {
		return Confirmation{}, ErrInvalidRequest
	}
	expires := service.now().UTC().Truncate(time.Second).Add(confirmationLifetime)
	digest, err := confirmationDigest(service.confirmationKey[:], operation, service.operatorID, canonical, expires)
	if err != nil {
		return Confirmation{}, err
	}
	return Confirmation{Digest: digest, ExpiresAt: expires}, nil
}

func (service *Service) commitCoordinates(operation, commandID string, proposal any) ([]byte, ReceiptKey, error) {
	if service.authority == nil {
		return nil, ReceiptKey{}, ErrUnavailable
	}
	return service.receiptCoordinates(operation, commandID, proposal)
}

// receiptCoordinates derives the canonical proposal and its durable receipt
// key without assuming which authority seam will execute it.
func (service *Service) receiptCoordinates(operation, commandID string, proposal any) ([]byte, ReceiptKey, error) {
	canonical, err := json.Marshal(proposal)
	if err != nil {
		return nil, ReceiptKey{}, ErrInvalidRequest
	}
	hash := sha256.New()
	hash.Write([]byte("baby-tracker-operator-command-v1\x00" + operation + "\x00" + service.operatorID + "\x00"))
	hash.Write(canonical)
	receipt := ReceiptKey{Operation: operation, OperatorID: service.operatorID, CommandID: commandID, ProposalDigest: hex.EncodeToString(hash.Sum(nil))}
	return canonical, receipt, nil
}

func (service *Service) verifyConfirmation(operation string, canonical []byte, confirmation Confirmation) error {
	now := service.now().UTC()
	if confirmation.ExpiresAt.Location() != time.UTC || confirmation.ExpiresAt.Nanosecond() != 0 ||
		!now.Before(confirmation.ExpiresAt) || confirmation.ExpiresAt.After(now.Add(confirmationLifetime)) || !confirmationHash.MatchString(confirmation.Digest) {
		return ErrInvalidConfirmation
	}
	expected, err := confirmationDigest(service.confirmationKey[:], operation, service.operatorID, canonical, confirmation.ExpiresAt)
	if err != nil || !equalHexDigest(expected, confirmation.Digest) {
		return ErrInvalidConfirmation
	}
	return nil
}

func (service *Service) audit(policyID, evidenceReference, reason string, expectedRevision uint64) Audit {
	return Audit{OperatorID: service.operatorID, PolicyID: policyID, EvidenceDigest: evidenceDigest(service.confirmationKey[:], evidenceReference),
		ReasonCode: reason, ExpectedRevision: expectedRevision}
}

func validateRecoveryProposal(proposal RecoveryProposal) error {
	if !validUUID(proposal.CommandID) || !validUUID(proposal.FamilyID) || !validUUID(proposal.OldOwnerID) || !validUUID(proposal.NewOwnerID) ||
		proposal.OldOwnerID == proposal.NewOwnerID || !policyIDPattern.MatchString(proposal.PolicyID) || !evidencePattern.MatchString(proposal.EvidenceReference) ||
		proposal.ReasonCode != "owner_identity_unavailable" || proposal.ExpectedRevision == 0 || proposal.ExpectedRevision > protocol.MaxSafeInteger {
		return ErrInvalidRequest
	}
	return nil
}

func validateEntitlementProposal(proposal EntitlementProposal) error {
	expectedReason := "beta_extension"
	if proposal.Operation == "suspend" {
		expectedReason = "abuse_suspension"
	} else if proposal.Operation != "grant" {
		return ErrInvalidRequest
	}
	if !validUUID(proposal.CommandID) || !validUUID(proposal.FamilyID) || proposal.Key != "family_sync" || proposal.Source != "hosted_beta" ||
		proposal.ExpiresAt.IsZero() || proposal.ExpiresAt.Location() != time.UTC || proposal.ExpiresAt.Nanosecond() != 0 ||
		!policyIDPattern.MatchString(proposal.PolicyID) || !evidencePattern.MatchString(proposal.EvidenceReference) || proposal.ReasonCode != expectedReason ||
		proposal.ExpectedRevision == 0 || proposal.ExpectedRevision > protocol.MaxSafeInteger {
		return ErrInvalidRequest
	}
	return nil
}

func validUUID(value string) bool {
	return canonicalUUID.MatchString(value) && value != "00000000-0000-0000-0000-000000000000"
}
