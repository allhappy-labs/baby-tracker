package account

import (
	"context"
	"errors"
	"regexp"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
)

// Service deletes accounts through the authority.
type Service struct {
	Authority *authority.Service
}

type codedError string

func (err codedError) Error() string { return string(err) }

var (
	ErrInvalidRequest            = codedError("invalid_request")
	ErrConfirmationRequired      = codedError("confirmation_required")
	ErrOwnershipTransferRequired = codedError("ownership_transfer_required")
	ErrCommandIDReused           = codedError("command_id_reused")
	ErrStaleLease                = codedError("stale_lease")
	ErrUnavailable               = codedError("service_unavailable")
)

// AppleAuthorizationRevocationManual is the Apple authorization revocation
// every deletion reports: the server keeps no Apple refresh token (hosted
// sign-in exchanges the authorization code and discards the token), so the
// person revokes the app's Sign in with Apple authorization themselves.
// Hosted revocation returns with a credential vault (Phase 6).
const AppleAuthorizationRevocationManual = "manual_required"

// DeletionResult is a committed (or exactly retried) account deletion.
type DeletionResult struct {
	authority.Result
	// AppleAuthorizationRevocation is AppleAuthorizationRevocationManual.
	AppleAuthorizationRevocation string
}

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func validUUID(value string) bool {
	return canonicalUUID.MatchString(value) && value != "00000000-0000-0000-0000-000000000000"
}

// Code is err's stable code: the account service's own code, else the
// authority's (authority codes are the legacy strings).
func Code(err error) string {
	if err == nil {
		return ""
	}
	var own codedError
	if errors.As(err, &own) {
		return string(own)
	}
	return authority.CodeOf(err)
}

// Delete deletes the caller's account and identity binding in one authority
// command. The command names the account's identity key, read first; a key
// that changed in between refuses the command (session_revoked). An exact
// retry returns the stored result.
func (service *Service) Delete(ctx context.Context, principal access.Principal, commandID string, confirmed bool) (DeletionResult, error) {
	if !confirmed {
		return DeletionResult{}, ErrConfirmationRequired
	}
	if service == nil || service.Authority == nil || !validUUID(commandID) {
		return DeletionResult{}, ErrInvalidRequest
	}
	if !validUUID(principal.AccountID) {
		return DeletionResult{}, authority.ErrSessionRevoked
	}
	key, found, err := service.Authority.AccountIdentityKey(ctx, principal.AccountID)
	if err != nil {
		return DeletionResult{}, err
	}
	if !found {
		return DeletionResult{}, authority.ErrMembershipRevoked
	}
	result, err := service.Authority.DeleteAccount(ctx, authority.DeleteAccount{CommandID: commandID, AccountID: principal.AccountID,
		IdentityKey: key, Principal: principal})
	switch authority.CodeOf(err) {
	case "":
		return DeletionResult{Result: result, AppleAuthorizationRevocation: AppleAuthorizationRevocationManual}, nil
	case "ownership_transfer_required":
		return DeletionResult{}, ErrOwnershipTransferRequired
	case "command_id_reused":
		return DeletionResult{}, ErrCommandIDReused
	default:
		return DeletionResult{}, err
	}
}
