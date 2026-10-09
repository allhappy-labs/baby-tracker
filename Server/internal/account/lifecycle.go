package account

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"

	"babytracker/server/internal/access"
	"babytracker/server/internal/security"
)

type Service struct {
	Security *security.Service
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

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func validUUID(value string) bool {
	return canonicalUUID.MatchString(value) && value != "00000000-0000-0000-0000-000000000000"
}

func Code(err error) string {
	if err == nil {
		return ""
	}
	var own codedError
	if errors.As(err, &own) {
		return string(own)
	}
	return security.Code(err)
}

// Delete commits local account and identity removal. Provider credentials are
// deliberately outside this request-path service and are handled by the
// separately leased revocation worker.
func (service *Service) Delete(ctx context.Context, principal access.Principal, commandID string, confirmed bool) (security.CommandResult, error) {
	if !confirmed {
		return security.CommandResult{}, ErrConfirmationRequired
	}
	if service == nil || service.Security == nil || !validUUID(commandID) {
		return security.CommandResult{}, ErrInvalidRequest
	}
	var account security.AccountView
	var ok bool
	var receipt security.ReceiptView
	var receiptExists bool
	err := service.Security.WithAuthorityView(ctx, func(view security.AuthorityView) error {
		account, ok = view.Account(principal.AccountID)
		receipt, receiptExists = view.Receipt(principal.AccountID, commandID)
		return nil
	})
	if err != nil {
		return security.CommandResult{}, err
	}
	if !ok {
		return security.CommandResult{}, security.ErrMembershipRevoked
	}
	payload := security.AccountDeletedPayload{Version: "v1", AccountID: principal.AccountID, IdentityKey: account.IdentityKey}
	raw, _ := json.Marshal(payload)
	command := security.Command{ID: commandID, Kind: "account_deleted", AffectedAccountIDs: []string{principal.AccountID}, Payload: raw}
	if receiptExists {
		if receipt.Kind != command.Kind {
			return security.CommandResult{}, ErrCommandIDReused
		}
		hash, hashErr := security.CanonicalCommandHash(principal, command)
		if hashErr != nil || hash != receipt.CommandHash {
			return security.CommandResult{}, ErrCommandIDReused
		}
		return receipt.Result, nil
	}
	result, err := service.Security.Execute(ctx, principal, command)
	if security.Code(err) == "ownership_transfer_required" {
		return security.CommandResult{}, ErrOwnershipTransferRequired
	}
	return result, err
}
