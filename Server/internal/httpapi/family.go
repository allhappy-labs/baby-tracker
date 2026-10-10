package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/family"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
)

const familyBodyBytes int64 = 64 << 10

const (
	defaultFamilyActorPageSize = 50
	maxFamilyActorPageSize     = 100
)

var (
	familyActorUUIDPattern    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	familyActorDecimalPattern = regexp.MustCompile(`^[1-9][0-9]*$`)
)

func createFamilyHandler(options Options) http.HandlerFunc {
	return familyMutation(options, "", "", func(w http.ResponseWriter, r *http.Request, p access.Principal) {
		var input protocol.CreateFamilyRequest
		if !decodeFamilyRequest(w, r, &input) {
			return
		}
		view, err := options.FamilyService.Create(r.Context(), p, input.CommandID, input.DisplayName)
		if err != nil {
			writeFamilyError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, familyResponse(view))
	})
}

func decodeRecoveryRequest(w http.ResponseWriter, r *http.Request, target *protocol.ConfirmedCommandRequest) bool {
	if err := protocol.DecodeRequest(r.Body, familyBodyBytes, target); err != nil {
		writeFamilyCode(w, r, protocol.ErrorCodeOf(err))
		return false
	}
	if err := protocol.ValidateRecoveryRequestCapabilities(target); err != nil {
		writeFamilyCode(w, r, protocol.ErrorCodeOf(err))
		return false
	}
	return true
}

func currentFamilyHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, ok := authenticateFamilyRequest(w, r, options)
		if !ok {
			return
		}
		view, err := options.FamilyService.Current(r.Context(), p)
		if err != nil {
			writeFamilyError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, familyResponse(view))
	}
}

func repairFamilyProfileHandler(options Options) http.HandlerFunc {
	return familyMutation(options, "", "", func(w http.ResponseWriter, r *http.Request, p access.Principal) {
		var input protocol.RepairFamilyProfileRequest
		if !decodeFamilyRequest(w, r, &input) {
			return
		}
		view, err := options.FamilyService.RepairProfile(r.Context(), p, input.CommandID, input.DisplayName, input.Confirmed)
		if err != nil {
			writeFamilyError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, familyResponse(view))
	})
}

func familyActorsHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, ok := authenticateFamilyRequest(w, r, options)
		if !ok {
			return
		}
		after, highWater, limit, err := familyActorPageQuery(r.URL.RawQuery)
		if err != nil {
			writeFamilyCode(w, r, "invalid_request")
			return
		}
		page, err := options.FamilyService.ListActors(r.Context(), p, after, highWater, limit)
		if err != nil {
			writeFamilyError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, familyActorPageResponse(page))
	}
}

func familyActorPageQuery(rawQuery string) (string, string, int, error) {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", "", 0, err
	}
	for key, entries := range values {
		if len(entries) != 1 || (key != "after" && key != "high_water" && key != "limit") {
			return "", "", 0, family.ErrInvalidRequest
		}
	}
	afterEntries, afterPresent := values["after"]
	highWaterEntries, highWaterPresent := values["high_water"]
	if afterPresent != highWaterPresent {
		return "", "", 0, family.ErrInvalidRequest
	}
	after, highWater := "", ""
	if afterPresent {
		after, highWater = afterEntries[0], highWaterEntries[0]
		if !familyActorUUIDPattern.MatchString(after) || after == "00000000-0000-0000-0000-000000000000" || !familyActorDecimalPattern.MatchString(highWater) {
			return "", "", 0, family.ErrInvalidRequest
		}
		parsed, parseErr := strconv.ParseUint(highWater, 10, 64)
		if parseErr != nil || parsed > protocol.MaxSafeInteger {
			return "", "", 0, family.ErrInvalidRequest
		}
	}
	limit := defaultFamilyActorPageSize
	if entries, present := values["limit"]; present {
		if !familyActorDecimalPattern.MatchString(entries[0]) {
			return "", "", 0, family.ErrInvalidRequest
		}
		parsed, parseErr := strconv.Atoi(entries[0])
		if parseErr != nil || parsed > maxFamilyActorPageSize {
			return "", "", 0, family.ErrInvalidRequest
		}
		limit = parsed
	}
	return after, highWater, limit, nil
}

func createInvitationHandler(options Options) http.HandlerFunc {
	return familyMutation(options, limits.ScopeInvitationCreateAccount, limits.ScopeInvitationCreateIP, func(w http.ResponseWriter, r *http.Request, p access.Principal) {
		var input protocol.InviteRequest
		if !decodeFamilyRequest(w, r, &input) {
			return
		}
		secret, err := options.FamilyService.Invite(r.Context(), p, input.CommandID, input.Role)
		if err != nil {
			writeFamilyError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, invitationResponse(secret.Invitation, secret.Token, secret.Link, secret.SecretUnavailable))
	})
}

func pendingInvitationsHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, ok := authenticateFamilyRequest(w, r, options)
		if !ok {
			return
		}
		views, err := options.FamilyService.PendingInvitations(r.Context(), p)
		if err != nil {
			writeFamilyError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, pendingInvitationsResponse(views))
	}
}

func invitationHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, ok := authenticateFamilyRequest(w, r, options)
		if !ok {
			return
		}
		view, err := options.FamilyService.Invitation(r.Context(), p, r.PathValue("id"))
		if err != nil {
			writeFamilyError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, invitationResponse(view, "", "", true))
	}
}

func revokeInvitationHandler(options Options) http.HandlerFunc {
	return familyMutation(options, "", "", func(w http.ResponseWriter, r *http.Request, p access.Principal) {
		var input protocol.CommandRequest
		if !decodeFamilyRequest(w, r, &input) {
			return
		}
		result, err := options.FamilyService.RevokeInvitation(r.Context(), p, input.CommandID, r.PathValue("id"))
		if err != nil {
			writeFamilyError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, commandResponse(result))
	})
}

func previewInvitationHandler(options Options) http.HandlerFunc {
	return familyOperation(options, false, limits.ScopeInvitationRedeemAccount, limits.ScopeInvitationRedeemIP, func(w http.ResponseWriter, r *http.Request, p access.Principal) {
		var input protocol.PreviewInvitationRequest
		if !decodeFamilyRequest(w, r, &input) {
			return
		}
		view, err := options.FamilyService.Preview(r.Context(), p, input.Token)
		if err != nil {
			writeFamilyError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, protocol.InvitationPreviewResponse{ProtocolVersion: "v1", FamilyDisplayName: view.FamilyDisplayName, ExpiresAt: timestamp(view.ExpiresAt)})
	})
}

func redeemInvitationHandler(options Options) http.HandlerFunc {
	return familyMutation(options, limits.ScopeInvitationRedeemAccount, limits.ScopeInvitationRedeemIP, func(w http.ResponseWriter, r *http.Request, p access.Principal) {
		var input protocol.RedeemInvitationRequest
		if !decodeFamilyRequest(w, r, &input) {
			return
		}
		view, err := options.FamilyService.Redeem(r.Context(), p, input.CommandID, input.Token, input.Confirmed)
		if err != nil {
			writeFamilyError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, familyResponse(view))
	})
}

func removeMemberHandler(options Options) http.HandlerFunc {
	return familyMutation(options, "", "", func(w http.ResponseWriter, r *http.Request, p access.Principal) {
		var input protocol.CommandRequest
		if !decodeFamilyRequest(w, r, &input) {
			return
		}
		result, err := options.FamilyService.Remove(r.Context(), p, input.CommandID, r.PathValue("account_id"))
		if err != nil {
			writeFamilyError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, commandResponse(result))
	})
}

func memberRoleHandler(options Options) http.HandlerFunc {
	return familyMutation(options, "", "", func(w http.ResponseWriter, r *http.Request, p access.Principal) {
		var input protocol.MemberRoleRequest
		if !decodeFamilyRequest(w, r, &input) {
			return
		}
		result, err := options.FamilyService.ChangeRole(r.Context(), p, input.CommandID, r.PathValue("account_id"), input.Role)
		if err != nil {
			writeFamilyError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, commandResponse(result))
	})
}

func transferOwnershipHandler(options Options) http.HandlerFunc {
	return familyMutation(options, "", "", func(w http.ResponseWriter, r *http.Request, p access.Principal) {
		var input protocol.TransferOwnershipRequest
		if !decodeFamilyRequest(w, r, &input) {
			return
		}
		result, err := options.FamilyService.Transfer(r.Context(), p, input.CommandID, input.NewOwnerAccountID)
		if err != nil {
			writeFamilyError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, commandResponse(result))
	})
}

func leaveFamilyHandler(options Options) http.HandlerFunc {
	return familyMutation(options, "", "", func(w http.ResponseWriter, r *http.Request, p access.Principal) {
		var input protocol.CommandRequest
		if !decodeFamilyRequest(w, r, &input) {
			return
		}
		result, err := options.FamilyService.Leave(r.Context(), p, input.CommandID)
		if err != nil {
			writeFamilyError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, commandResponse(result))
	})
}

func familyDeleteHandler(options Options) http.HandlerFunc {
	return familyMutation(options, "", "", func(w http.ResponseWriter, r *http.Request, p access.Principal) {
		var input protocol.ConfirmedCommandRequest
		if !decodeFamilyRequest(w, r, &input) {
			return
		}
		status, err := options.FamilyService.ScheduleDeletion(r.Context(), p, input.CommandID, input.Confirmed)
		if err != nil {
			writeFamilyError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, recoveryResponse(status))
	})
}

func familyRecoveryStatusHandler(options Options) http.HandlerFunc {
	return familyOperation(options, false, "", "", func(w http.ResponseWriter, r *http.Request, p access.Principal) {
		status, err := options.FamilyService.RecoveryStatus(r.Context(), p)
		if err != nil {
			writeFamilyError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, recoveryResponse(status))
	})
}

func familyRecoverHandler(options Options) http.HandlerFunc {
	return familyRecoveryMutation(options, func(ctx context.Context, p access.Principal, commandID string, confirmed bool) (authority.Result, error) {
		return options.FamilyService.Recover(ctx, p, commandID, confirmed)
	})
}

func familyForfeitHandler(options Options) http.HandlerFunc {
	return familyRecoveryMutation(options, func(ctx context.Context, p access.Principal, commandID string, confirmed bool) (authority.Result, error) {
		return options.FamilyService.Forfeit(ctx, p, commandID, confirmed)
	})
}

func familyRecoveryMutation(options Options, mutate func(context.Context, access.Principal, string, bool) (authority.Result, error)) http.HandlerFunc {
	return familyMutation(options, "", "", func(w http.ResponseWriter, r *http.Request, p access.Principal) {
		var input protocol.ConfirmedCommandRequest
		if !decodeRecoveryRequest(w, r, &input) {
			return
		}
		result, err := mutate(r.Context(), p, input.CommandID, input.Confirmed)
		if err != nil {
			writeFamilyError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, commandResponse(result))
	})
}

func familyMutation(options Options, accountScope, ipScope string, next func(http.ResponseWriter, *http.Request, access.Principal)) http.HandlerFunc {
	return familyOperation(options, true, accountScope, ipScope, next)
}

func familyOperation(options Options, structural bool, accountScope, ipScope string, next func(http.ResponseWriter, *http.Request, access.Principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, ok := authenticateFamilyRequest(w, r, options)
		if !ok {
			return
		}
		if !takeFamilyLimits(w, r, options, p, structural, accountScope, ipScope) {
			return
		}
		next(w, r, p)
	}
}

func authenticateFamilyRequest(w http.ResponseWriter, r *http.Request, options Options) (access.Principal, bool) {
	if err := authReadyError(r, options); err != nil || options.FamilyService == nil {
		writeFamilyFault(w, r, authReadinessCode(err), err)
		return access.Principal{}, false
	}
	values := r.Header.Values("Authorization")
	if len(values) == 0 {
		writeFamilyCode(w, r, "access_expired")
		return access.Principal{}, false
	}
	token, err := auth.BearerToken(r)
	if err != nil {
		writeFamilyCode(w, r, auth.Code(err))
		return access.Principal{}, false
	}
	p, err := options.SessionService.Authenticate(r.Context(), token)
	if err != nil {
		writeFamilyCode(w, r, auth.Code(err))
		return access.Principal{}, false
	}
	return p, true
}

func decodeFamilyRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	if err := protocol.DecodeRequest(r.Body, familyBodyBytes, target); err != nil {
		writeFamilyCode(w, r, protocol.ErrorCodeOf(err))
		return false
	}
	if err := protocol.ValidateFamilyRequestCapabilities(target); err != nil {
		writeFamilyCode(w, r, protocol.ErrorCodeOf(err))
		return false
	}
	return true
}

func takeFamilyLimits(w http.ResponseWriter, r *http.Request, options Options, p access.Principal, structural bool, accountScope, ipScope string) bool {
	if options.Limiter == nil {
		writeFamilyCode(w, r, "service_unavailable")
		return false
	}
	if ipScope != "" {
		if options.ClientIPResolver == nil {
			writeFamilyCode(w, r, "service_unavailable")
			return false
		}
		address, err := options.ClientIPResolver.Resolve(r.RemoteAddr, r.Header)
		if err != nil {
			writeFamilyCode(w, r, "invalid_request")
			return false
		}
		policy := options.InvitationCreateIPPolicy
		if ipScope == limits.ScopeInvitationRedeemIP {
			policy = options.InvitationRedeemIPPolicy
		}
		decision, err := options.Limiter.Take(r.Context(), ipScope, address.String(), policy)
		if err != nil {
			writeFamilyFault(w, r, "service_unavailable", err)
			return false
		}
		if !decision.Allowed {
			writeRateLimited(w, r, decision)
			return false
		}
	}
	charges := make([]limits.Charge, 0, 3)
	if structural {
		charges = append(charges,
			limits.Charge{Scope: limits.ScopeFamilyStructuralAccount, Key: p.AccountID, Cost: 1, Policy: options.FamilyStructuralAccountPolicy},
			limits.Charge{Scope: limits.ScopeFamilyStructuralDevice, Key: p.DeviceID, Cost: 1, Policy: options.FamilyStructuralDevicePolicy})
	}
	if accountScope != "" {
		policy := options.InvitationCreateAccountPolicy
		if accountScope == limits.ScopeInvitationRedeemAccount {
			policy = options.InvitationRedeemAccountPolicy
		}
		charges = append(charges, limits.Charge{Scope: accountScope, Key: p.AccountID, Cost: 1, Policy: policy})
	}
	decision, err := options.Limiter.TakeMany(r.Context(), charges)
	if err != nil {
		writeFamilyFault(w, r, "service_unavailable", err)
		return false
	}
	if !decision.Allowed {
		writeRateLimited(w, r, decision)
		return false
	}
	return true
}

func familyResponse(view family.FamilyView) protocol.FamilyResponse {
	members := make([]protocol.FamilyMemberResponse, 0, len(view.Members))
	for _, member := range view.Members {
		members = append(members, protocol.FamilyMemberResponse{AccountID: member.AccountID, DisplayName: member.DisplayName, Role: member.Role, Status: member.Status})
	}
	return protocol.FamilyResponse{ProtocolVersion: "v1", ID: view.ID, DisplayName: view.DisplayName, Status: view.Status, ProfileRequired: view.ProfileRequired, Members: members}
}

func familyActorPageResponse(page family.ActorLabelPage) protocol.FamilyActorPageResponse {
	actors := make([]protocol.FamilyActorResponse, 0, len(page.Items))
	for _, actor := range page.Items {
		actors = append(actors, protocol.FamilyActorResponse{AccountID: actor.AccountID, DisplayName: actor.DisplayName, Former: actor.Former})
	}
	return protocol.FamilyActorPageResponse{ProtocolVersion: "v1", Actors: actors, HighWater: page.HighWater, NextCursor: page.NextCursor}
}

func invitationResponse(view family.InvitationView, token, link string, secretUnavailable bool) protocol.InvitationResponse {
	return protocol.InvitationResponse{ProtocolVersion: "v1", ID: view.ID, FamilyID: view.FamilyID, Role: view.Role, Status: view.Status,
		ExpiresAt: timestamp(view.ExpiresAt), CreatedAt: timestamp(view.CreatedAt), Token: token, Link: link, SecretUnavailable: secretUnavailable}
}

func pendingInvitationsResponse(views []family.InvitationView) protocol.PendingInvitationsResponse {
	invitations := make([]protocol.InvitationSummary, 0, len(views))
	for _, view := range views {
		invitations = append(invitations, protocol.InvitationSummary{ID: view.ID, FamilyID: view.FamilyID, Role: view.Role, Status: view.Status,
			ExpiresAt: timestamp(view.ExpiresAt), CreatedAt: timestamp(view.CreatedAt)})
	}
	return protocol.PendingInvitationsResponse{Invitations: invitations}
}

func commandResponse(result authority.Result) protocol.CommandResponse {
	return protocol.CommandResponse{ProtocolVersion: "v1", CommandID: result.CommandID, Outcome: result.Outcome, Code: result.Code, Revision: result.Revision}
}

func recoveryResponse(status family.RecoveryStatus) protocol.FamilyRecoveryResponse {
	return protocol.FamilyRecoveryResponse{
		ProtocolVersion: "v1",
		FamilyID:        status.FamilyID,
		State:           status.State,
		PurgeAfter:      timestamp(status.PurgeAfter),
		CanRecover:      status.CanRecover,
	}
}

func timestamp(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func writeFamilyError(w http.ResponseWriter, r *http.Request, err error) {
	writeFamilyFault(w, r, family.Code(err), err)
}

func writeFamilyCode(w http.ResponseWriter, r *http.Request, code string) {
	writeFamilyFault(w, r, code, nil)
}

// writeFamilyFault writes the refusal for code and, for a 5xx, logs it once
// with cause (logServerFault) through the logger the middleware put on the
// request.
func writeFamilyFault(w http.ResponseWriter, r *http.Request, code string, cause error) {
	status, retryable, message := http.StatusServiceUnavailable, true, "Service is unavailable"
	switch code {
	case "invalid_request", "confirmation_required", "display_name_required", "invalid_display_name":
		status, retryable, message = http.StatusBadRequest, false, "Invalid request"
	case "unsupported_protocol":
		status, retryable, message = http.StatusBadRequest, false, "Unsupported protocol"
	case "unsupported_capability":
		status, retryable, message = http.StatusConflict, false, "Unsupported capability"
	case "access_expired", "session_revoked", "reauthentication_required", "epoch_changed":
		status, retryable, message = http.StatusUnauthorized, false, "Authentication failed"
		w.Header().Set("WWW-Authenticate", "Bearer")
	case "owner_required", "membership_revoked", "entitlement_required", "recovery_only", "owner_identity_unavailable", "operator_policy_required":
		status, retryable, message = http.StatusForbidden, false, "Access denied"
	case "family_unavailable", "invitation_invalid":
		status, retryable, message = http.StatusNotFound, false, "Resource unavailable"
	case "family_slot_occupied", "caregiver_limit", "invitation_limit", "command_id_reused", "profile_not_required", "ownership_transfer_required", "stale_security_revision", "invalid_confirmation":
		status, retryable, message = http.StatusConflict, false, "Request conflicts with existing family state"
	case "invitation_expired", "invitation_consumed", "invitation_revoked", "recovery_expired", "recovery_forfeited":
		status, retryable, message = http.StatusGone, false, "Resource is no longer available"
	case "rate_limited":
		status, retryable, message = http.StatusTooManyRequests, true, "Rate limit exceeded"
	case "security_busy":
		w.Header().Set("Retry-After", "1")
	case "service_unavailable", "authorization_quarantine", "hosted_adapter_unavailable", "credential_key_unavailable":
	default:
		code = "service_unavailable"
	}
	logServerFault(requestLogger(r, nil), r, status, code, cause)
	writeError(w, status, code, message, requestIDFromContext(r.Context()), retryable)
}
