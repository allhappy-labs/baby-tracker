package authority

import "errors"

// Code is a stable error code. The strings are identical to the legacy
// security, family and auth codes so callers and clients see the same codes.
type Code string

func (c Code) Error() string { return string(c) }

// CodeOf returns err's code: "" for nil, "service_unavailable" for any error
// that does not wrap a Code.
func CodeOf(err error) string {
	if err == nil {
		return ""
	}
	var code Code
	if errors.As(err, &code) {
		return string(code)
	}
	return "service_unavailable"
}

// Codes of security/types.go.
var (
	ErrLedgerCorrupt       error = Code("ledger_corrupt")
	ErrTrustedHeadRequired error = Code("trusted_head_required")
	ErrUnsupportedEvent    error = Code("unsupported_security_event")
	ErrMembershipRevoked   error = Code("membership_revoked")
	ErrUnavailable         error = Code("service_unavailable")
)

// Codes of family/types.go.
var (
	ErrInvalidRequest            error = Code("invalid_request")
	ErrFamilySlotOccupied        error = Code("family_slot_occupied")
	ErrOwnerRequired             error = Code("owner_required")
	ErrReadOnlyCaregiver         error = Code("read_only_caregiver")
	ErrFamilyUnavailable         error = Code("family_unavailable")
	ErrInvitationInvalid         error = Code("invitation_invalid")
	ErrInvitationExpired         error = Code("invitation_expired")
	ErrInvitationConsumed        error = Code("invitation_consumed")
	ErrInvitationRevoked         error = Code("invitation_revoked")
	ErrCaregiverLimit            error = Code("caregiver_limit")
	ErrInvitationLimit           error = Code("invitation_limit")
	ErrConfirmationRequired      error = Code("confirmation_required")
	ErrProfileNotRequired        error = Code("profile_not_required")
	ErrEntitlementRequired       error = Code("entitlement_required")
	ErrSecretUnavailable         error = Code("secret_unavailable")
	ErrCommandIDReused           error = Code("command_id_reused")
	ErrAuthorizationQuarantine   error = Code("authorization_quarantine")
	ErrHostedAdapterUnavailable  error = Code("hosted_adapter_unavailable")
	ErrCredentialKeyUnavailable  error = Code("credential_key_unavailable")
	ErrRecoveryOnly              error = Code("recovery_only")
	ErrRecoveryExpired           error = Code("recovery_expired")
	ErrRecoveryForfeited         error = Code("recovery_forfeited")
	ErrOwnershipTransferRequired error = Code("ownership_transfer_required")
)

// Codes of auth/refresh.go.
var (
	ErrAccessExpired            error = Code("access_expired")
	ErrSessionRevoked           error = Code("session_revoked")
	ErrRefreshReused            error = Code("refresh_reused")
	ErrReauthenticationRequired error = Code("reauthentication_required")
	ErrEpochChanged             error = Code("epoch_changed")
)

// Codes of auth/refresh_recovery.go, auth/types.go and identity/types.go.
var (
	// ErrRefreshAttemptConflict is a consumed parent replayed under another
	// recoverable attempt; the lineage is revoked.
	ErrRefreshAttemptConflict error = Code("refresh_attempt_conflict")
	// ErrRefreshReceiptUnavailable is an exact recovery whose receipt or
	// child is gone; nothing is revoked.
	ErrRefreshReceiptUnavailable error = Code("refresh_receipt_unavailable")
	// ErrDeviceContinuityInvalid is a device proof that does not match the
	// device binding; it never triggers a revocation.
	ErrDeviceContinuityInvalid   error = Code("device_continuity_invalid")
	ErrInvalidOrExpiredChallenge error = Code("invalid_or_expired_challenge")
)

// Codes the legacy security state machine returns, plus scope_failed.
var (
	// ErrInvalidCommand is a command the server judged malformed.
	ErrInvalidCommand error = Code("invalid_command")
	// ErrScopeFailed is a family or account whose audit chain failed
	// verification; its requests fail closed with 503.
	ErrScopeFailed error = Code("scope_failed")
	// ErrSecurityBusy is a lost race against an identical concurrent command
	// whose receipt committed first (writeReceipt); a retry returns that
	// command's stored result.
	ErrSecurityBusy               error = Code("security_busy")
	ErrInvalidConfirmation        error = Code("invalid_confirmation")
	ErrStaleSecurityRevision      error = Code("stale_security_revision")
	ErrOwnerIdentityUnavailable   error = Code("owner_identity_unavailable")
	ErrIntegrationVerifierReused  error = Code("integration_verifier_reused")
	ErrIntegrationInvalid         error = Code("integration_invalid")
	ErrIntegrationRevoked         error = Code("integration_revoked")
	ErrCredentialConflict         error = Code("credential_conflict")
	ErrExportAuthorizationChanged error = Code("export_authorization_changed")
)
