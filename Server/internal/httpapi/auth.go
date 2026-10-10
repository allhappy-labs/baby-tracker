package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"babytracker/server/internal/auth"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
)

const authBodyBytes int64 = 32 << 10
const appleNotificationBodyBytes int64 = 64 << 10

func challengeHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		requestID := requestIDFromContext(request.Context())
		if authDependenciesError(request, options) != nil {
			writeError(w, http.StatusServiceUnavailable, "not_ready", "Service is not ready", requestID, true)
			return
		}
		if options.ClientIPResolver == nil || options.Limiter == nil || options.ChallengeService == nil {
			writeError(w, http.StatusServiceUnavailable, "not_ready", "Service is not ready", requestID, true)
			return
		}
		address, err := options.ClientIPResolver.Resolve(request.RemoteAddr, request.Header)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request", requestID, false)
			return
		}
		decision, err := options.Limiter.Take(request.Context(), limits.ScopeAuthChallengeIP, address.String(), options.ChallengePolicy)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "rate_limit_unavailable", "Rate limit service is unavailable", requestID, true)
			return
		}
		if !decision.Allowed {
			retryAfter := int((decision.RetryAfter + time.Second - 1) / time.Second)
			if retryAfter < 1 {
				retryAfter = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			writeRetryError(w, http.StatusTooManyRequests, "rate_limited", "Rate limit exceeded", requestID, true, retryAfter)
			return
		}
		var input protocol.ChallengeRequest
		if err := protocol.DecodeRequest(request.Body, authBodyBytes, &input); err != nil {
			writeError(w, http.StatusBadRequest, protocol.ErrorCodeOf(err), "Invalid request", requestID, false)
			return
		}
		if err := protocol.ValidateCapabilities(input.Capabilities, protocol.IdentityCapabilities()); err != nil {
			writeError(w, http.StatusBadRequest, protocol.ErrorCodeOf(err), "Unsupported capability", requestID, false)
			return
		}
		challenge, err := options.ChallengeService.Challenge(request.Context())
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "identity_unavailable", "Identity service is unavailable", requestID, true)
			return
		}
		writeJSON(w, http.StatusOK, protocol.ChallengeResponse{
			ProtocolVersion:  "v1",
			Challenge:        challenge.Challenge,
			ServerInstanceID: challenge.ServerInstanceID,
			CanonicalOrigin:  challenge.CanonicalOrigin,
			ExpiresAt:        challenge.ExpiresAt.UTC().Format(time.RFC3339Nano),
		})
	}
}

func appleExchangeHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if err := authReadyError(request, options); err != nil {
			writeAuthFault(w, request, authReadinessCode(err), err)
			return
		}
		if _, ok := takeIPLimit(w, request, options, limits.ScopeAuthExchangeIP, options.ExchangePolicy); !ok {
			return
		}
		var input protocol.AppleExchangeRequest
		if err := protocol.DecodeRequest(request.Body, authBodyBytes, &input); err != nil {
			writeProtocolError(w, request, err)
			return
		}
		if err := protocol.ValidateCapabilities(input.Capabilities, protocol.IdentityCapabilities()); err != nil {
			writeProtocolError(w, request, err)
			return
		}
		if options.AppleAuthorizationMode == "identity_token_only" && input.AuthorizationCode != "" {
			writeAuthCode(w, request, "hosted_only_field")
			return
		}
		if options.AppleAuthorizationMode == "code_exchange_required" && input.AuthorizationCode == "" {
			writeAuthCode(w, request, "invalid_request")
			return
		}
		if input.DeviceContinuity != nil && (input.DeviceContinuity.DeviceID == "" || len(input.DeviceContinuity.Credential) != 43) {
			writeAuthCode(w, request, "device_continuity_invalid")
			return
		}
		var profile *identity.ProfileInput
		if input.Profile != nil {
			profile = &identity.ProfileInput{DisplayName: input.Profile.DisplayName, Confirmed: input.Profile.Confirmed}
		}
		var continuity *identity.DeviceContinuityClaim
		if input.DeviceContinuity != nil {
			continuity = &identity.DeviceContinuityClaim{DeviceID: input.DeviceContinuity.DeviceID, Credential: input.DeviceContinuity.Credential}
		}
		bundle, err := options.SessionService.Exchange(request.Context(), identity.ExchangeRequest{
			CommandID: input.CommandID, Challenge: input.Challenge, IdentityToken: input.IdentityToken,
			AuthorizationCode: input.AuthorizationCode, InstallationID: input.InstallationID, Profile: profile, DeviceContinuity: continuity,
		})
		if err != nil {
			writeAuthError(w, request, err)
			return
		}
		writeJSON(w, http.StatusOK, sessionResponse(bundle))
	}
}

func refreshHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if err := authReadyError(request, options); err != nil {
			writeAuthFault(w, request, authReadinessCode(err), err)
			return
		}
		if _, ok := takeIPLimit(w, request, options, limits.ScopeAuthRefreshIP, options.RefreshIPPolicy); !ok {
			return
		}
		var input protocol.RefreshRequest
		if err := protocol.DecodeRequest(request.Body, authBodyBytes, &input); err != nil {
			writeProtocolError(w, request, err)
			return
		}
		if err := protocol.ValidateCapabilities(input.Capabilities, protocol.IdentityCapabilities()); err != nil {
			writeProtocolError(w, request, err)
			return
		}
		subject, err := options.SessionService.ResolveRefreshSubject(request.Context(), input.RefreshToken)
		if err != nil {
			writeAuthError(w, request, err)
			return
		}
		decision, err := options.Limiter.TakeMany(request.Context(), []limits.Charge{
			{Scope: limits.ScopeAuthRefreshAccount, Key: subject.AccountID, Cost: 1, Policy: options.RefreshAccountPolicy},
			{Scope: limits.ScopeAuthRefreshDevice, Key: subject.DeviceID, Cost: 1, Policy: options.RefreshDevicePolicy},
		})
		if err != nil {
			writeAuthFault(w, request, "service_unavailable", err)
			return
		}
		if !decision.Allowed {
			writeRateLimited(w, request, decision)
			return
		}
		bundle, err := options.SessionService.Refresh(request.Context(), input.RefreshToken)
		if err != nil {
			writeAuthError(w, request, err)
			return
		}
		writeJSON(w, http.StatusOK, sessionResponse(bundle))
	}
}

func recoverableRefreshHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !options.RecoveryEnabled || options.RecoveryService == nil || options.RecoveryReadiness == nil {
			writeAuthCode(w, request, "service_unavailable")
			return
		}
		if err := options.RecoveryReadiness.Check(request.Context()); err != nil {
			writeAuthFault(w, request, "service_unavailable", err)
			return
		}
		if err := authDependenciesError(request, options); err != nil {
			writeAuthFault(w, request, authReadinessCode(err), err)
			return
		}
		if _, ok := takeIPLimit(w, request, options, limits.ScopeAuthRefreshIP, options.RefreshIPPolicy); !ok {
			return
		}
		var wire protocol.RecoverableRefreshRequest
		if err := protocol.DecodeRequest(request.Body, authBodyBytes, &wire); err != nil {
			writeProtocolError(w, request, err)
			return
		}
		if err := protocol.ValidateRefreshRecoveryRequest(wire); err != nil {
			writeProtocolError(w, request, err)
			return
		}
		input := auth.RecoverableRefreshInput{RefreshToken: wire.RefreshToken, AttemptID: wire.RefreshAttemptID, DeviceContinuity: identity.DeviceContinuityClaim{DeviceID: wire.DeviceContinuity.DeviceID, Credential: wire.DeviceContinuity.Credential}}
		subject, err := options.RecoveryService.ResolveRecoverableRefreshSubject(request.Context(), input)
		if err != nil {
			writeAuthError(w, request, err)
			return
		}
		decision, err := options.Limiter.TakeMany(request.Context(), []limits.Charge{
			{Scope: limits.ScopeAuthRefreshAccount, Key: subject.AccountID, Cost: 1, Policy: options.RefreshAccountPolicy},
			{Scope: limits.ScopeAuthRefreshDevice, Key: subject.DeviceID, Cost: 1, Policy: options.RefreshDevicePolicy},
		})
		if err != nil {
			writeAuthFault(w, request, "service_unavailable", err)
			return
		}
		if !decision.Allowed {
			writeRateLimited(w, request, decision)
			return
		}
		bundle, err := options.RecoveryService.RefreshRecoverable(request.Context(), input)
		if err != nil {
			writeAuthError(w, request, err)
			return
		}
		writeJSON(w, http.StatusOK, protocol.RecoverableRefreshResponse{SessionResponse: sessionResponse(bundle.SessionBundle), RefreshAttemptID: bundle.RefreshAttemptID, ServerInstanceID: options.InstanceID})
	}
}

func refreshAckHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !options.RecoveryEnabled || options.RecoveryService == nil || options.SessionService == nil || options.RecoveryReadiness == nil {
			writeAuthCode(w, request, "service_unavailable")
			return
		}
		if err := options.RecoveryReadiness.Check(request.Context()); err != nil {
			writeAuthFault(w, request, "service_unavailable", err)
			return
		}
		if err := authDependenciesError(request, options); err != nil {
			writeAuthFault(w, request, authReadinessCode(err), err)
			return
		}
		var wire protocol.RefreshAckRequest
		if err := protocol.DecodeRequest(request.Body, authBodyBytes, &wire); err != nil {
			writeProtocolError(w, request, err)
			return
		}
		if err := protocol.ValidateRefreshAckRequest(wire); err != nil {
			writeProtocolError(w, request, err)
			return
		}
		token, err := auth.BearerToken(request)
		if err != nil {
			writeAuthError(w, request, err)
			return
		}
		principal, err := options.SessionService.Authenticate(request.Context(), token)
		if err != nil {
			writeAuthError(w, request, err)
			return
		}
		if err := options.RecoveryService.AcknowledgeRefresh(request.Context(), principal, wire.RefreshAttemptID, identity.DeviceContinuityClaim{DeviceID: wire.DeviceContinuity.DeviceID, Credential: wire.DeviceContinuity.Credential}); err != nil {
			writeAuthError(w, request, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func logoutHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if err := authReadyError(request, options); err != nil {
			writeAuthFault(w, request, authReadinessCode(err), err)
			return
		}
		var input protocol.LogoutRequest
		if err := protocol.DecodeRequest(request.Body, authBodyBytes, &input); err != nil {
			writeProtocolError(w, request, err)
			return
		}
		if err := protocol.ValidateCapabilities(input.Capabilities, protocol.IdentityCapabilities()); err != nil {
			writeProtocolError(w, request, err)
			return
		}
		token, err := auth.BearerToken(request)
		if err != nil {
			writeAuthError(w, request, err)
			return
		}
		principal, err := options.SessionService.Authenticate(request.Context(), token)
		if err != nil {
			writeAuthError(w, request, err)
			return
		}
		if _, err := options.SessionService.Logout(request.Context(), principal, input.CommandID); err != nil {
			writeAuthError(w, request, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func appleNotificationHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if err := authDependenciesError(request, options); err != nil {
			writeAuthFault(w, request, authReadinessCode(err), err)
			return
		}
		if options.AppleNotifications == nil {
			writeAuthCode(w, request, "service_unavailable")
			return
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, appleNotificationBodyBytes+1))
		if err != nil || len(body) == 0 || int64(len(body)) > appleNotificationBodyBytes {
			writeAuthCode(w, request, "invalid_apple_proof")
			return
		}
		if err := options.AppleNotifications.Handle(request.Context(), string(body)); err != nil {
			writeAuthError(w, request, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func authReadyError(request *http.Request, options Options) error {
	if err := authDependenciesError(request, options); err != nil {
		return err
	}
	if options.SessionService == nil {
		return auth.ErrUnavailable
	}
	return nil
}

func authDependenciesError(request *http.Request, options Options) error {
	if options.Readiness != nil {
		return options.Readiness.Check(request.Context())
	}
	if options.Gate == nil {
		return auth.ErrUnavailable
	}
	return options.Gate.Check(request.Context())
}

// authReadinessCode is the stable code a readiness or gate failure is
// reported as: one of the three refusal codes when the error is (or wraps) it,
// service_unavailable otherwise. The error's own text never reaches the
// client.
func authReadinessCode(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	var code authority.Code
	if errors.As(err, &code) {
		message = string(code)
	}
	switch message {
	case "authorization_quarantine", "hosted_adapter_unavailable", "credential_key_unavailable":
		return message
	default:
		return "service_unavailable"
	}
}

func takeIPLimit(w http.ResponseWriter, request *http.Request, options Options, scope string, policy limits.Policy) (string, bool) {
	if options.ClientIPResolver == nil || options.Limiter == nil {
		writeAuthCode(w, request, "service_unavailable")
		return "", false
	}
	address, err := options.ClientIPResolver.Resolve(request.RemoteAddr, request.Header)
	if err != nil {
		writeAuthCode(w, request, "invalid_request")
		return "", false
	}
	decision, err := options.Limiter.Take(request.Context(), scope, address.String(), policy)
	if err != nil {
		writeAuthFault(w, request, "service_unavailable", err)
		return "", false
	}
	if !decision.Allowed {
		writeRateLimited(w, request, decision)
		return "", false
	}
	return address.String(), true
}

func writeRateLimited(w http.ResponseWriter, request *http.Request, decision limits.Decision) {
	retryAfter := int((decision.RetryAfter + time.Second - 1) / time.Second)
	if retryAfter < 1 {
		retryAfter = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	writeRetryError(w, http.StatusTooManyRequests, "rate_limited", "Rate limit exceeded", requestIDFromContext(request.Context()), true, retryAfter)
}

func sessionResponse(bundle auth.SessionBundle) protocol.SessionResponse {
	response := protocol.SessionResponse{
		ProtocolVersion: "v1", AccessToken: bundle.AccessToken, RefreshToken: bundle.RefreshToken,
		ExpiresAt: bundle.ExpiresAt.UTC().Format(time.RFC3339Nano), ReauthenticateAt: bundle.ReauthenticateAt.UTC().Format(time.RFC3339Nano),
		RecoveryEpoch: bundle.RecoveryEpoch, DeviceID: bundle.DeviceID, DeviceCredential: bundle.DeviceCredential,
		Account: protocol.SessionAccount{ID: bundle.Account.ID, DisplayName: bundle.Account.DisplayName, ProfileRequired: bundle.Account.ProfileRequired},
	}
	if bundle.FamilySlot != nil {
		response.FamilySlot = &protocol.SessionFamilySlot{FamilyID: bundle.FamilySlot.FamilyID, State: bundle.FamilySlot.State}
	}
	return response
}

func writeProtocolError(w http.ResponseWriter, request *http.Request, err error) {
	code := protocol.ErrorCodeOf(err)
	if code == "" {
		code = "invalid_request"
	}
	writeAuthCode(w, request, code)
}

func writeAuthError(w http.ResponseWriter, request *http.Request, err error) {
	writeAuthFault(w, request, auth.Code(err), err)
}

func writeAuthCode(w http.ResponseWriter, request *http.Request, code string) {
	writeAuthFault(w, request, code, nil)
}

// writeAuthFault writes the refusal for code and, for a 5xx, logs it once
// with cause (logServerFault) through the logger the middleware put on the
// request.
func writeAuthFault(w http.ResponseWriter, request *http.Request, code string, cause error) {
	status, retryable, message := http.StatusServiceUnavailable, true, "Service is unavailable"
	switch code {
	case "invalid_request", "hosted_only_field", "display_name_required", "device_continuity_invalid":
		status, retryable, message = http.StatusBadRequest, false, "Invalid request"
	case "unsupported_protocol":
		status, retryable, message = http.StatusBadRequest, false, "Unsupported protocol"
	case "unsupported_capability":
		status, retryable, message = http.StatusConflict, false, "Unsupported capability"
	case "invalid_or_expired_challenge", "invalid_apple_proof", "invalid_apple_authorization", "access_expired", "session_revoked", "refresh_reused", "reauthentication_required", "epoch_changed", "refresh_receipt_unavailable":
		status, retryable, message = http.StatusUnauthorized, false, "Authentication failed"
		w.Header().Set("WWW-Authenticate", "Bearer")
	case "command_id_reused", "credential_conflict", "refresh_attempt_conflict":
		status, retryable, message = http.StatusConflict, false, "Request conflicts with existing state"
	case "rate_limited":
		status, retryable, message = http.StatusTooManyRequests, true, "Rate limit exceeded"
		w.Header().Set("Retry-After", "1")
		writeRetryError(w, status, code, message, requestIDFromContext(request.Context()), retryable, 1)
		return
	case "security_busy", "credential_rotation_busy":
		w.Header().Set("Retry-After", "1")
	case "authorization_quarantine", "apple_unavailable", "hosted_adapter_unavailable", "credential_key_unavailable", "service_unavailable":
	default:
		code = "service_unavailable"
	}
	logServerFault(requestLogger(request, nil), request, status, code, cause)
	writeError(w, status, code, message, requestIDFromContext(request.Context()), retryable)
}
