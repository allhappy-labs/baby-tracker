package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/diagnostics"
	"babytracker/server/internal/family"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
	familysync "babytracker/server/internal/sync"
)

func syncHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(diagnostics.WithRecorder(r.Context(), options.SyncDiagnosticRecorder))
		defer diagnostics.Flush(r.Context())
		w.Header().Set("Cache-Control", "no-store")
		authorityStart := time.Now()
		principal, ok := authenticateSyncFamilyRequest(w, r, options)
		diagnostics.Record(r.Context(), nil, diagnostics.Authority, time.Since(authorityStart))
		if !ok {
			return
		}
		if principal.Scope != "family" {
			writeSyncCode(w, r, options, "family_scope_required")
			return
		}
		if principal.SessionScope != "family" || !protocol.ValidUUID(principal.SessionFamilyID) {
			writeSyncCode(w, r, options, "access_expired")
			return
		}
		if options.SyncService == nil || options.SyncLimits == nil || options.Limiter == nil {
			writeSyncCode(w, r, options, "service_unavailable")
			return
		}
		headerCapabilities, ok := parseSyncHeaders(w, r, options)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		body, oversized, err := boundedBody(r, int64(options.SyncLimits.SyncBodyBytes))
		if oversized {
			writeSyncCode(w, r, options, "body_too_large")
			return
		}
		if err != nil {
			writeSyncCode(w, r, options, "invalid_request")
			return
		}
		var input protocol.SyncRequest
		if err := protocol.DecodeRequest(bytes.NewReader(body), int64(options.SyncLimits.SyncBodyBytes), &input); err != nil {
			writeSyncCode(w, r, options, protocol.ErrorCodeOf(err))
			return
		}
		surface := protocol.SurfaceSync
		for _, mutation := range input.Mutations {
			if mutation.EntityKind == "timer" {
				surface = protocol.SurfaceSyncTimer
				break
			}
		}
		if err := protocol.ValidateRouteCapabilities(surface, headerCapabilities, input.Capabilities, advertisedCapabilities(options)); err != nil {
			writeSyncCode(w, r, options, protocol.ErrorCodeOf(err))
			return
		}
		if input.Mutations == nil || input.PageToken != nil && (input.Cursor != nil || len(input.Mutations) != 0) {
			writeSyncCode(w, r, options, "invalid_request")
			return
		}
		if len(input.Mutations) > options.SyncLimits.SyncBatchSize {
			writeSyncCode(w, r, options, "batch_limit")
			return
		}
		authorityStart = time.Now()
		familyView, err := options.FamilyService.Current(r.Context(), principal)
		diagnostics.Record(r.Context(), nil, diagnostics.Authority, time.Since(authorityStart))
		if err != nil {
			writeSyncFamilyError(w, r, options, err)
			return
		}
		if principal.SessionFamilyID != familyView.ID {
			writeSyncCode(w, r, options, "access_expired")
			return
		}
		charges := []limits.Charge{
			{Scope: limits.ScopeSyncPullDevice, Key: principal.DeviceID, Cost: 1, Policy: options.SyncPullDevicePolicy},
			{Scope: limits.ScopeSyncPullAccount, Key: principal.AccountID, Cost: 1, Policy: options.SyncPullAccountPolicy},
		}
		if len(input.Mutations) != 0 {
			charges = append(charges,
				limits.Charge{Scope: limits.ScopeSyncMutationDevice, Key: principal.DeviceID, Cost: len(input.Mutations), Policy: options.SyncMutationDevicePolicy},
				limits.Charge{Scope: limits.ScopeSyncMutationAccount, Key: principal.AccountID, Cost: len(input.Mutations), Policy: options.SyncMutationAccountPolicy},
				limits.Charge{Scope: limits.ScopeSyncMutationFamily, Key: familyView.ID, Cost: len(input.Mutations), Policy: options.SyncMutationFamilyPolicy})
		}
		decision, err := options.Limiter.TakeMany(r.Context(), charges)
		if err != nil {
			writeSyncFault(w, r, options, "service_unavailable", err)
			return
		}
		if !decision.Allowed {
			writeSyncRateLimited(w, r, options, decision)
			return
		}
		databaseStart := time.Now()
		response, err := options.SyncService.ApplyAndPull(r.Context(), principal, input)
		diagnostics.Record(r.Context(), nil, diagnostics.Database, time.Since(databaseStart))
		if err != nil {
			writeSyncError(w, r, options, err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	}
}

func snapshotHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(diagnostics.WithRecorder(r.Context(), options.SyncDiagnosticRecorder))
		defer diagnostics.Flush(r.Context())
		w.Header().Set("Cache-Control", "no-store")
		authorityStart := time.Now()
		principal, ok := authenticateSyncFamilyRequest(w, r, options)
		diagnostics.Record(r.Context(), nil, diagnostics.Authority, time.Since(authorityStart))
		if !ok {
			return
		}
		if principal.Scope != "family" {
			writeSyncCode(w, r, options, "family_scope_required")
			return
		}
		if principal.SessionScope != "family" || !protocol.ValidUUID(principal.SessionFamilyID) {
			writeSyncCode(w, r, options, "access_expired")
			return
		}
		if options.SyncService == nil || options.SyncLimits == nil || options.Limiter == nil {
			writeSyncCode(w, r, options, "service_unavailable")
			return
		}
		if _, ok := validateSyncHeaders(w, r, protocol.SurfaceSnapshot, options); !ok {
			return
		}
		token, err := snapshotPageToken(r.URL.Query())
		if err != nil {
			writeSyncCode(w, r, options, "invalid_request")
			return
		}
		authorityStart = time.Now()
		familyView, err := options.FamilyService.Current(r.Context(), principal)
		diagnostics.Record(r.Context(), nil, diagnostics.Authority, time.Since(authorityStart))
		if err != nil {
			writeSyncFamilyError(w, r, options, err)
			return
		}
		if principal.SessionFamilyID != familyView.ID {
			writeSyncCode(w, r, options, "access_expired")
			return
		}
		decision, err := options.Limiter.TakeMany(r.Context(), []limits.Charge{
			{Scope: limits.ScopeSyncPullDevice, Key: principal.DeviceID, Cost: 1, Policy: options.SyncPullDevicePolicy},
			{Scope: limits.ScopeSyncPullAccount, Key: principal.AccountID, Cost: 1, Policy: options.SyncPullAccountPolicy},
		})
		if err != nil {
			writeSyncFault(w, r, options, "service_unavailable", err)
			return
		}
		if !decision.Allowed {
			writeSyncRateLimited(w, r, options, decision)
			return
		}
		databaseStart := time.Now()
		response, err := options.SyncService.FullSnapshot(r.Context(), principal, protocol.SnapshotRequest{PageToken: token})
		diagnostics.Record(r.Context(), nil, diagnostics.Database, time.Since(databaseStart))
		if err != nil {
			writeSyncError(w, r, options, err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	}
}

func boundedBody(r *http.Request, maximum int64) ([]byte, bool, error) {
	if maximum < 1 {
		return nil, false, errors.New("invalid body limit")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maximum+1))
	if err != nil {
		var tooLarge *http.MaxBytesError
		return nil, errors.As(err, &tooLarge), err
	}
	if int64(len(body)) > maximum {
		return nil, true, nil
	}
	return body, false, nil
}

func validateSyncHeaders(w http.ResponseWriter, r *http.Request, surface protocol.Surface, options Options) ([]string, bool) {
	return validateHeadersAgainst(w, r, surface, options, advertisedCapabilities(options))
}

// validateHeadersAgainst validates the protocol and capability headers against
// an explicit accepted set. Routes accept what the server advertises; the one
// exception is Live Activity unregistration, which accepts its capability
// whether or not push is advertised.
func validateHeadersAgainst(w http.ResponseWriter, r *http.Request, surface protocol.Surface, options Options, accepted []string) ([]string, bool) {
	capabilities, ok := parseHeadersAgainst(w, r, options, accepted)
	if !ok {
		return nil, false
	}
	if err := protocol.ValidateRouteCapabilities(surface, capabilities, nil, accepted); err != nil {
		writeSyncCode(w, r, options, protocol.ErrorCodeOf(err))
		return nil, false
	}
	return capabilities, true
}

func parseSyncHeaders(w http.ResponseWriter, r *http.Request, options Options) ([]string, bool) {
	return parseHeadersAgainst(w, r, options, advertisedCapabilities(options))
}

func parseHeadersAgainst(w http.ResponseWriter, r *http.Request, options Options, accepted []string) ([]string, bool) {
	if r.Header.Get("X-Baby-Tracker-Protocol") != "v1" {
		writeSyncCode(w, r, options, "unsupported_protocol")
		return nil, false
	}
	values := r.Header.Values("X-Baby-Tracker-Capabilities")
	if len(values) != 1 || values[0] == "" || strings.ContainsAny(values[0], " \t\r\n") {
		writeSyncCode(w, r, options, "unsupported_capability")
		return nil, false
	}
	capabilities := strings.Split(values[0], ",")
	if err := protocol.ValidateCapabilities(capabilities, accepted); err != nil {
		writeSyncCode(w, r, options, protocol.ErrorCodeOf(err))
		return nil, false
	}
	return capabilities, true
}

func snapshotPageToken(values url.Values) (*string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	items, ok := values["page_token"]
	if !ok || len(values) != 1 || len(items) != 1 || items[0] == "" || len(items[0]) > 512 {
		return nil, errors.New("invalid query")
	}
	return &items[0], nil
}

func writeSyncError(w http.ResponseWriter, r *http.Request, options Options, err error) {
	switch {
	case errors.Is(err, familysync.ErrInvalidPageToken):
		writeSyncCode(w, r, options, "invalid_page_token")
	case errors.Is(err, familysync.ErrPageExpired):
		writeSyncCode(w, r, options, "page_expired")
	case errors.Is(err, familysync.ErrEpochChanged):
		writeSyncCode(w, r, options, "epoch_changed")
	case errors.Is(err, familysync.ErrSnapshotRequired):
		writeSyncCode(w, r, options, "snapshot_required")
	case errors.Is(err, familysync.ErrBatchLimit):
		writeSyncCode(w, r, options, "batch_limit")
	case errors.Is(err, familysync.ErrSnapshotLimit), errors.Is(err, familysync.ErrPageLimit):
		writeSyncCode(w, r, options, "snapshot_limit")
	case errors.Is(err, familysync.ErrInvalidSync):
		writeSyncCode(w, r, options, "invalid_request")
	case errors.Is(err, familysync.ErrMutationIDReused):
		writeSyncCode(w, r, options, "mutation_id_reused")
	case errors.Is(err, familysync.ErrFamilyScopeRequired):
		writeSyncCode(w, r, options, "family_scope_required")
	case errors.Is(err, familysync.ErrSessionRefreshRequired):
		writeSyncCode(w, r, options, "access_expired")
	case errors.Is(err, familysync.ErrReadOnlyCaregiver):
		writeSyncCode(w, r, options, "read_only_caregiver")
	default:
		// Anything unrecognised here is an internal fault — a stored entity
		// that no longer decodes, a journal read that failed — and it leaves
		// as a retryable 503. The writer logs it with its cause, so it is not
		// also a silent one: the v2 timer decode fault was invisible until a
		// client happened to hit it.
		writeSyncFamilyError(w, r, options, err)
	}
}

func writeSyncCode(w http.ResponseWriter, r *http.Request, options Options, code string) {
	writeSyncFault(w, r, options, code, nil)
}

// writeSyncFault writes the protocol-level refusal for code and, for a 5xx,
// logs it once with cause (logServerFault).
func writeSyncFault(w http.ResponseWriter, r *http.Request, options Options, code string, cause error) {
	status, retryable, message := http.StatusServiceUnavailable, true, "Service is unavailable"
	switch code {
	case "invalid_request":
		status, retryable, message = http.StatusBadRequest, false, "Invalid request"
	case "unsupported_protocol":
		status, retryable, message = http.StatusBadRequest, false, "Unsupported protocol"
	case "batch_limit":
		status, retryable, message = http.StatusBadRequest, false, "Mutation batch limit exceeded"
	case "invalid_page_token":
		status, retryable, message = http.StatusBadRequest, false, "Invalid page token"
	case "unsupported_capability":
		status, retryable, message = http.StatusConflict, false, "Unsupported capability"
	case "body_too_large":
		status, retryable, message = http.StatusRequestEntityTooLarge, false, "Request body is too large"
	case "read_only_caregiver":
		// Not retryable: the role must change before this can ever succeed,
		// so a client that keeps retrying is burning battery for nothing.
		status, retryable, message = http.StatusForbidden, false, "This caregiver has view-only access"
	case "snapshot_required":
		status, retryable, message = http.StatusConflict, false, "Snapshot required"
	case "snapshot_limit":
		status, retryable, message = http.StatusConflict, false, "Snapshot limit exceeded"
	case "mutation_id_reused":
		status, retryable, message = http.StatusConflict, false, "Mutation ID was reused"
	case "family_scope_required":
		status, retryable, message = http.StatusForbidden, false, "Family access is required"
	case "page_expired":
		status, retryable, message = http.StatusGone, false, "Page expired"
	case "epoch_changed":
		status, retryable, message = http.StatusUnauthorized, false, "Authentication failed"
		w.Header().Set("WWW-Authenticate", "Bearer")
	case "access_expired", "reauthentication_required":
		status, retryable, message = http.StatusUnauthorized, false, "Authentication failed"
		w.Header().Set("WWW-Authenticate", "Bearer")
	case "service_unavailable":
	default:
		code = "service_unavailable"
	}
	response := newErrorResponse(code, message, requestIDFromContext(r.Context()), retryable)
	response.ServerInstanceID, response.RecoveryEpoch = options.InstanceID, options.RecoveryEpoch
	logServerFault(requestLogger(r, options.Logger), r, status, code, cause)
	writeJSON(w, status, response)
}

func authenticateSyncFamilyRequest(w http.ResponseWriter, r *http.Request, options Options) (access.Principal, bool) {
	if err := authReadyError(r, options); err != nil || options.FamilyService == nil {
		writeSyncFamilyFault(w, r, options, authReadinessCode(err), err)
		return access.Principal{}, false
	}
	if len(r.Header.Values("Authorization")) == 0 {
		writeSyncFamilyCode(w, r, options, "access_expired")
		return access.Principal{}, false
	}
	token, err := auth.BearerToken(r)
	if err != nil {
		writeSyncFamilyCode(w, r, options, auth.Code(err))
		return access.Principal{}, false
	}
	principal, err := options.SessionService.Authenticate(r.Context(), token)
	if err != nil {
		writeSyncFamilyCode(w, r, options, auth.Code(err))
		return access.Principal{}, false
	}
	return principal, true
}

func writeSyncFamilyError(w http.ResponseWriter, r *http.Request, options Options, err error) {
	writeSyncFamilyFault(w, r, options, family.Code(err), err)
}

func writeSyncFamilyCode(w http.ResponseWriter, r *http.Request, options Options, code string) {
	writeSyncFamilyFault(w, r, options, code, nil)
}

// writeSyncFamilyFault writes the family-level refusal for code and, for a
// 5xx, logs it once with cause (logServerFault).
func writeSyncFamilyFault(w http.ResponseWriter, r *http.Request, options Options, code string, cause error) {
	status, retryable, message := http.StatusServiceUnavailable, true, "Service is unavailable"
	switch code {
	case "access_expired", "session_revoked", "reauthentication_required", "epoch_changed":
		status, retryable, message = http.StatusUnauthorized, false, "Authentication failed"
		w.Header().Set("WWW-Authenticate", "Bearer")
	case "owner_required", "membership_revoked", "entitlement_required", "recovery_only":
		status, retryable, message = http.StatusForbidden, false, "Access denied"
	case "read_only_caregiver":
		// Same status and the same refusal to retry as the in-transaction
		// gate's version of this, and the same sentence, because to a client
		// they are the same event arriving from two different depths.
		status, retryable, message = http.StatusForbidden, false, "This caregiver has view-only access"
	case "family_unavailable":
		status, retryable, message = http.StatusNotFound, false, "Resource unavailable"
	case "authorization_quarantine", "security_busy", "service_unavailable", "hosted_adapter_unavailable", "credential_key_unavailable":
	default:
		code = "service_unavailable"
	}
	response := newErrorResponse(code, message, requestIDFromContext(r.Context()), retryable)
	response.ServerInstanceID, response.RecoveryEpoch = options.InstanceID, options.RecoveryEpoch
	logServerFault(requestLogger(r, options.Logger), r, status, code, cause)
	writeJSON(w, status, response)
}

func writeSyncRateLimited(w http.ResponseWriter, r *http.Request, options Options, decision limits.Decision) {
	retryAfter := int((decision.RetryAfter + time.Second - 1) / time.Second)
	if retryAfter < 1 {
		retryAfter = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	response := newErrorResponse("rate_limited", "Rate limit exceeded", requestIDFromContext(r.Context()), true)
	response.Error.RetryAfterSeconds = &retryAfter
	response.ServerInstanceID, response.RecoveryEpoch = options.InstanceID, options.RecoveryEpoch
	writeJSON(w, http.StatusTooManyRequests, response)
}
