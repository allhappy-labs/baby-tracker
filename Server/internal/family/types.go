package family

import (
	"errors"
	"fmt"
	"time"

	"babytracker/server/internal/security"
)

type FamilyView struct {
	ID, DisplayName, Status string
	ProfileRequired         bool
	Members                 []MemberView
}

type MemberView struct{ AccountID, DisplayName, Role, Status string }

type ActorLabelView struct {
	AccountID, DisplayName string
	Former                 bool
}

type ActorLabelPage struct {
	Items                 []ActorLabelView
	HighWater, NextCursor string
}

type FamilySlotView struct{ FamilyID, State string }

type AccountView struct {
	ID, DisplayName, Status string
	ProfileRequired         bool
	FamilySlot              *FamilySlotView
}

type InvitationView struct {
	ID, FamilyID, Role, Status string
	ExpiresAt, CreatedAt       time.Time
}

type InvitationSecret struct {
	Invitation        InvitationView
	Token, Link       string
	SecretUnavailable bool
}

type InvitationPreview struct {
	FamilyDisplayName string
	ExpiresAt         time.Time
}

type codedError string

func (err codedError) Error() string { return string(err) }

var (
	ErrInvalidRequest            = codedError("invalid_request")
	ErrFamilySlotOccupied        = codedError("family_slot_occupied")
	ErrOwnerRequired             = codedError("owner_required")
	ErrReadOnlyCaregiver         = codedError("read_only_caregiver")
	ErrMembershipRevoked         = codedError("membership_revoked")
	ErrFamilyUnavailable         = codedError("family_unavailable")
	ErrInvitationInvalid         = codedError("invitation_invalid")
	ErrInvitationExpired         = codedError("invitation_expired")
	ErrInvitationConsumed        = codedError("invitation_consumed")
	ErrInvitationRevoked         = codedError("invitation_revoked")
	ErrCaregiverLimit            = codedError("caregiver_limit")
	ErrInvitationLimit           = codedError("invitation_limit")
	ErrConfirmationRequired      = codedError("confirmation_required")
	ErrProfileNotRequired        = codedError("profile_not_required")
	ErrEntitlementRequired       = codedError("entitlement_required")
	ErrSecretUnavailable         = codedError("secret_unavailable")
	ErrUnavailable               = codedError("service_unavailable")
	ErrCommandIDReused           = codedError("command_id_reused")
	ErrAuthorizationQuarantine   = codedError("authorization_quarantine")
	ErrHostedAdapterUnavailable  = codedError("hosted_adapter_unavailable")
	ErrCredentialKeyUnavailable  = codedError("credential_key_unavailable")
	ErrRecoveryOnly              = codedError("recovery_only")
	ErrRecoveryExpired           = codedError("recovery_expired")
	ErrRecoveryForfeited         = codedError("recovery_forfeited")
	ErrOwnershipTransferRequired = codedError("ownership_transfer_required")
)

func Code(err error) string {
	if err == nil {
		return ""
	}
	var coded codedError
	if errors.As(err, &coded) {
		return string(coded)
	}
	if code := security.Code(err); code != "service_unavailable" {
		return code
	}
	return "service_unavailable"
}

func familyError(err error) error {
	var own codedError
	if errors.As(err, &own) {
		return err
	}
	switch security.Code(err) {
	case "family_slot_occupied":
		return ErrFamilySlotOccupied
	// A command the server itself judged malformed. Left unmapped it fell
	// through to `service_unavailable`, which is both wrong and retryable:
	// the client was told to keep resending a request that can never be
	// accepted.
	case "invalid_command":
		return ErrInvalidRequest
	case "owner_required":
		return ErrOwnerRequired
	case "read_only_caregiver":
		return ErrReadOnlyCaregiver
	case "membership_revoked":
		return ErrMembershipRevoked
	case "family_unavailable":
		return ErrFamilyUnavailable
	case "invitation_invalid":
		return ErrInvitationInvalid
	case "invitation_expired":
		return ErrInvitationExpired
	case "invitation_consumed":
		return ErrInvitationConsumed
	case "invitation_revoked":
		return ErrInvitationRevoked
	case "caregiver_limit":
		return ErrCaregiverLimit
	case "invitation_limit":
		return ErrInvitationLimit
	case "confirmation_required":
		return ErrConfirmationRequired
	case "entitlement_required":
		return ErrEntitlementRequired
	case "command_id_reused":
		return ErrCommandIDReused
	case "epoch_changed", "session_revoked", "reauthentication_required", "security_busy":
		return err
	case "authorization_quarantine":
		return ErrAuthorizationQuarantine
	case "hosted_adapter_unavailable":
		return ErrHostedAdapterUnavailable
	case "credential_key_unavailable":
		return ErrCredentialKeyUnavailable
	case "recovery_only":
		return ErrRecoveryOnly
	case "recovery_expired":
		return ErrRecoveryExpired
	case "recovery_forfeited":
		return ErrRecoveryForfeited
	case "ownership_transfer_required":
		return ErrOwnershipTransferRequired
	default:
		// The cause travels with the code: the writer logs it, the client
		// still sees only service_unavailable.
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
}
