package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/care"
	"babytracker/server/internal/diagnostics"
	"babytracker/server/internal/family"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
	familysync "babytracker/server/internal/sync"
)

// syncHandler serves POST /v1/sync as one authenticated transaction
// (SyncService.ServeSync). Everything that needs no database is done before
// it opens: readiness (cached), the bearer token's digests, and the headers
// and the body, read (before the route's admission slot, admitAfterBody) and
// validated in full, so no transaction waits on the network. A header or
// body refusal is held and returned by Admit, after authentication, so a bad
// credential still answers 401 before any 400, 409 or 413, as the legacy
// handler's order had it. The rate limit is charged
// by Charge, after family policy and before any care statement.
func syncHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(diagnostics.WithRecorder(r.Context(), options.SyncDiagnosticRecorder))
		defer diagnostics.Flush(r.Context())
		w.Header().Set("Cache-Control", "no-store")
		digests, ok := syncCredential(w, r, options)
		if !ok {
			return
		}
		if options.SyncService == nil || options.SyncLimits == nil || options.Limiter == nil {
			writeSyncCode(w, r, options, "service_unavailable")
			return
		}
		admitted, ok := admitAfterBody(w, r, options, digests, int64(options.SyncLimits.SyncBodyBytes), writeRequestError)
		if !ok {
			return
		}
		defer admitted.release()
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		input, refusal := readSyncRequest(r, options)
		response, err := options.SyncService.ServeSync(ctx, authority.Request{Digests: digests, Admit: admitSync(refusal),
			Charge: chargeSync(ctx, options, len(input.Mutations))}, input)
		admitted.noteCredential(ctx, err)
		if err != nil {
			writeRequestError(w, r, options, err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	}
}

// snapshotHandler serves GET /v1/sync/snapshot as one authenticated
// transaction (SyncService.ServeSnapshot), with the header and query
// refusals held as syncHandler holds its own.
func snapshotHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(diagnostics.WithRecorder(r.Context(), options.SyncDiagnosticRecorder))
		defer diagnostics.Flush(r.Context())
		w.Header().Set("Cache-Control", "no-store")
		digests, ok := syncCredential(w, r, options)
		if !ok {
			return
		}
		if options.SyncService == nil || options.SyncLimits == nil || options.Limiter == nil {
			writeSyncCode(w, r, options, "service_unavailable")
			return
		}
		_, refusal := syncHeaders(r, protocol.SurfaceSnapshot, options)
		token, err := snapshotPageToken(r.URL.Query())
		if refusal == "" && err != nil {
			refusal = "invalid_request"
		}
		response, err := options.SyncService.ServeSnapshot(r.Context(), authority.Request{Digests: digests, Admit: admitSync(refusal),
			Charge: chargeSync(r.Context(), options, 0)}, protocol.SnapshotRequest{PageToken: token})
		if err != nil {
			writeRequestError(w, r, options, err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	}
}

// syncCredential is the sync routes' work before any database: readiness
// (the cached state) and the bearer access token's digests. A refusal is
// written here, with the legacy codes and in the legacy order: not ready,
// no Authorization header (access_expired), a malformed one or a malformed
// token (session_revoked).
func syncCredential(w http.ResponseWriter, r *http.Request, options Options) ([]authority.KeyedDigest, bool) {
	if err := authReadyError(r, options); err != nil {
		writeSyncFamilyFault(w, r, options, authReadinessCode(err), err)
		return nil, false
	}
	if len(r.Header.Values("Authorization")) == 0 {
		writeSyncFamilyCode(w, r, options, "access_expired")
		return nil, false
	}
	token, err := auth.BearerToken(r)
	if err != nil {
		writeSyncFamilyCode(w, r, options, auth.Code(err))
		return nil, false
	}
	digests, err := options.SessionService.AccessDigests(token)
	if err != nil {
		// A refusal (session_revoked) logs nothing; any other failure is a
		// 5xx whose fault line carries err.
		writeSyncFamilyFault(w, r, options, auth.Code(err), err)
		return nil, false
	}
	return digests, true
}

// readSyncRequest reads and validates a sync request's headers and body in
// the legacy order: the protocol and capability headers, the bounded body
// (body_too_large), the decode, the route capabilities, the request shape
// and the batch limit. It returns the first refusal's code instead of
// writing it ("" when the request is acceptable).
func readSyncRequest(r *http.Request, options Options) (protocol.SyncRequest, string) {
	var input protocol.SyncRequest
	headerCapabilities, refusal := parseSyncHeaderValues(r, options)
	if refusal != "" {
		return input, refusal
	}
	body, oversized, err := boundedBody(r, int64(options.SyncLimits.SyncBodyBytes))
	if oversized {
		return input, "body_too_large"
	}
	if err != nil {
		return input, "invalid_request"
	}
	if err := protocol.DecodeRequest(bytes.NewReader(body), int64(options.SyncLimits.SyncBodyBytes), &input); err != nil {
		return protocol.SyncRequest{}, protocol.ErrorCodeOf(err)
	}
	surface := protocol.SurfaceSync
	for _, mutation := range input.Mutations {
		if mutation.EntityKind == "timer" {
			surface = protocol.SurfaceSyncTimer
			break
		}
	}
	if err := protocol.ValidateRouteCapabilities(surface, headerCapabilities, input.Capabilities, advertisedCapabilities(options)); err != nil {
		return input, protocol.ErrorCodeOf(err)
	}
	if input.Mutations == nil || input.PageToken != nil && (input.Cursor != nil || len(input.Mutations) != 0) {
		return input, "invalid_request"
	}
	if len(input.Mutations) > options.SyncLimits.SyncBatchSize {
		return input, "batch_limit"
	}
	return input, ""
}

// syncRefusal is a sync code held until after authentication (Admit) or
// raised by Charge; it is written with writeSyncFault, with the whole error
// as the fault line's cause.
type syncRefusal string

func (r syncRefusal) Error() string { return string(r) }

// limiterUnavailable is the refusal of a charge whose limiter failed: a
// service_unavailable refusal that keeps the limiter's error as its cause
// for the fault line (logServerFault).
func limiterUnavailable(err error) error {
	return fmt.Errorf("%w: %w", syncRefusal("service_unavailable"), err)
}

// rateLimited is Charge's denial; it is written with writeSyncRateLimited.
type rateLimited struct{ decision limits.Decision }

func (r rateLimited) Error() string { return "rate_limited" }

// admitSync is the sync routes' Admit: the legacy checks of the
// authenticated principal (a family member's refreshed family session),
// then the held header, query or body refusal. It refuses every session
// that is not a family session, so family policy never sees a recovery or
// account session (and never answers recovery_only here).
func admitSync(refusal string) func(access.Principal) error {
	return func(p access.Principal) error {
		switch {
		case p.Scope != "family":
			return syncRefusal("family_scope_required")
		case p.SessionScope != "family" || !protocol.ValidUUID(p.SessionFamilyID):
			return syncRefusal("access_expired")
		case refusal != "":
			return syncRefusal(refusal)
		}
		return nil
	}
}

// chargeSync is the sync routes' Charge: the in-memory rate limit, with the
// family from the grant. It runs inside the open transaction, holding a
// pooled connection, so it must stay in memory and never block.
func chargeSync(ctx context.Context, options Options, mutations int) func(access.Principal, access.Grant) error {
	return func(p access.Principal, g access.Grant) error {
		decision, err := options.Limiter.TakeMany(ctx, syncCharges(options, p, g.FamilyID, mutations))
		if err != nil {
			return limiterUnavailable(err)
		}
		if !decision.Allowed {
			return rateLimited{decision}
		}
		return nil
	}
}

// syncCharges is a sync request's charges: the pull device and account
// charges, plus, for mutations, the device, account and family mutation
// charges (a snapshot page is charged as a pull, with no mutations).
func syncCharges(options Options, p access.Principal, familyID string, mutations int) []limits.Charge {
	charges := []limits.Charge{
		{Scope: limits.ScopeSyncPullDevice, Key: p.DeviceID, Cost: 1, Policy: options.SyncPullDevicePolicy},
		{Scope: limits.ScopeSyncPullAccount, Key: p.AccountID, Cost: 1, Policy: options.SyncPullAccountPolicy},
	}
	if mutations != 0 {
		charges = append(charges,
			limits.Charge{Scope: limits.ScopeSyncMutationDevice, Key: p.DeviceID, Cost: mutations, Policy: options.SyncMutationDevicePolicy},
			limits.Charge{Scope: limits.ScopeSyncMutationAccount, Key: p.AccountID, Cost: mutations, Policy: options.SyncMutationAccountPolicy},
			limits.Charge{Scope: limits.ScopeSyncMutationFamily, Key: familyID, Cost: mutations, Policy: options.SyncMutationFamilyPolicy})
	}
	return charges
}

// writeRequestError writes a one-transaction request's error: a held or
// Charge refusal with writeSyncFault (the error, which may wrap a limiter's,
// as its cause), a rate-limit denial with writeSyncRateLimited, anything
// else (an authentication or family-policy refusal from the authority, or
// the service's own) with writeSyncError, whose fallback maps authority
// codes to their legacy statuses.
func writeRequestError(w http.ResponseWriter, r *http.Request, options Options, err error) {
	var refusal syncRefusal
	var limited rateLimited
	switch {
	case errors.As(err, &refusal):
		writeSyncFault(w, r, options, string(refusal), err)
	case errors.As(err, &limited):
		writeSyncRateLimited(w, r, options, limited.decision)
	default:
		writeSyncError(w, r, options, err)
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

// syncHeaders reads the protocol and capability headers and checks the
// route's capabilities, returning the refusal's code ("" when the headers
// are acceptable).
func syncHeaders(r *http.Request, surface protocol.Surface, options Options) ([]string, string) {
	capabilities, refusal := parseSyncHeaderValues(r, options)
	if refusal != "" {
		return nil, refusal
	}
	if err := protocol.ValidateRouteCapabilities(surface, capabilities, nil, advertisedCapabilities(options)); err != nil {
		return nil, protocol.ErrorCodeOf(err)
	}
	return capabilities, ""
}

// parseSyncHeaderValues reads the protocol and capability headers, returning
// the refusal's code ("" when they are acceptable) instead of writing it.
func parseSyncHeaderValues(r *http.Request, options Options) ([]string, string) {
	if r.Header.Get("X-Baby-Tracker-Protocol") != "v1" {
		return nil, "unsupported_protocol"
	}
	values := r.Header.Values("X-Baby-Tracker-Capabilities")
	if len(values) != 1 || values[0] == "" || strings.ContainsAny(values[0], " \t\r\n") {
		return nil, "unsupported_capability"
	}
	capabilities := strings.Split(values[0], ",")
	if err := protocol.ValidateCapabilities(capabilities, advertisedCapabilities(options)); err != nil {
		return nil, protocol.ErrorCodeOf(err)
	}
	return capabilities, ""
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
		code := family.Code(err)
		_, status, _, _ := syncFamilyResponse(code)
		if options.Logger != nil {
			requestID, route := slog.String("request_id", requestIDFromContext(r.Context())), slog.String("route", normalizedRoute(r.Pattern))
			switch {
			case code == "access_expired":
				// The routine refresh signal: every device meets one per
				// access-token lifetime and refreshes, so it is not news.
				options.Logger.Debug("sync request refused", requestID, route, slog.String("code", code))
			case status != http.StatusServiceUnavailable:
				// An expected refusal (owner_required, membership_revoked,
				// read_only_caregiver, an expired session): the client is
				// told not to retry, and a removed caregiver's phone keeps
				// asking, so it is not an error.
				options.Logger.Info("sync request refused", requestID, route, slog.String("code", code))
			case errors.Is(err, care.ErrQuotaExceeded):
				// A family at its storage or record quota: refused (a
				// retryable 503, as the legacy route left it), worth an
				// operator's notice, not a fault.
				options.Logger.Warn("sync request refused", requestID, route, slog.String("code", care.ErrQuotaExceeded.Error()))
			}
		}
		if status == http.StatusServiceUnavailable && errors.Is(err, care.ErrQuotaExceeded) {
			// Its line is the Warn above: no fault line.
			writeSyncFamilyResponse(w, r, options, code)
			return
		}
		// Anything unrecognised here is an internal fault — a stored entity
		// that no longer decodes, a care read that failed — and it leaves as
		// a retryable 503. The writer logs it with its cause, so it is not
		// also a silent one: the v2 timer decode fault was invisible until a
		// client happened to hit it.
		writeSyncFamilyFault(w, r, options, code, err)
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
	case "invalid_push_token":
		status, retryable, message = http.StatusBadRequest, false, "Invalid push token"
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

func writeSyncFamilyError(w http.ResponseWriter, r *http.Request, options Options, err error) {
	writeSyncFamilyFault(w, r, options, family.Code(err), err)
}

func writeSyncFamilyCode(w http.ResponseWriter, r *http.Request, options Options, code string) {
	writeSyncFamilyFault(w, r, options, code, nil)
}

// writeSyncFamilyFault writes the family-level refusal for code and, for a
// 5xx, logs it once with cause (logServerFault).
func writeSyncFamilyFault(w http.ResponseWriter, r *http.Request, options Options, code string, cause error) {
	written, status, _, _ := syncFamilyResponse(code)
	logServerFault(requestLogger(r, options.Logger), r, status, written, cause)
	writeSyncFamilyResponse(w, r, options, code)
}

// writeSyncFamilyResponse writes the family-level refusal for code, and
// logs nothing.
func writeSyncFamilyResponse(w http.ResponseWriter, r *http.Request, options Options, code string) {
	code, status, retryable, message := syncFamilyResponse(code)
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	response := newErrorResponse(code, message, requestIDFromContext(r.Context()), retryable)
	response.ServerInstanceID, response.RecoveryEpoch = options.InstanceID, options.RecoveryEpoch
	writeJSON(w, status, response)
}

// syncFamilyResponse is the error a family or authority code is written
// as: an expected refusal's own status, or for any other code a retryable
// 503 (service_unavailable unless the code is one of its named causes).
func syncFamilyResponse(code string) (string, int, bool, string) {
	status, retryable, message := http.StatusServiceUnavailable, true, "Service is unavailable"
	switch code {
	case "access_expired", "session_revoked", "reauthentication_required", "epoch_changed":
		status, retryable, message = http.StatusUnauthorized, false, "Authentication failed"
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
	return code, status, retryable, message
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
