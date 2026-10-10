package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/authority"
	familyexport "babytracker/server/internal/export"
	"babytracker/server/internal/family"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
)

// exportRefusal is an export route's own refusal, held until after
// authentication (Admit); it is written with writeExportCode.
type exportRefusal string

func (r exportRefusal) Error() string { return string(r) }

// exportPrincipal authorizes an export request in one pooled statement with
// no transaction (RequestAuthorizer.AuthorizeRequest), in the legacy order:
// readiness and the credential (before any database work), then, after
// authentication (Admit), the session's scope, the headers, the IP and the
// rate limit, and the route's own refusal (its query, body or request);
// then family policy, where the legacy service's family read stood. The
// export service runs its guarded transaction afterwards.
func exportPrincipal(w http.ResponseWriter, r *http.Request, options Options, refusal string) (access.Principal, bool) {
	w.Header().Set("Cache-Control", "no-store")
	digests, ok := syncCredential(w, r, options)
	if !ok {
		return access.Principal{}, false
	}
	if options.RequestAuthorizer == nil {
		writeExportCode(w, r, options, "service_unavailable")
		return access.Principal{}, false
	}
	_, headerRefusal := syncHeaders(r, protocol.SurfaceExport, options)
	admitted := false
	p, g, err := options.RequestAuthorizer.AuthorizeRequest(r.Context(), authority.Request{Digests: digests, Admit: func(p access.Principal) error {
		if err := admitExport(r, options, p, headerRefusal, refusal); err != nil {
			return err
		}
		admitted = true
		return nil
	}}, "read")
	if err != nil {
		writeExportRequestError(w, r, options, err, admitted)
		return access.Principal{}, false
	}
	if g.FamilyID != p.SessionFamilyID {
		// The legacy family read's check (owner_required); the grant is the
		// session's family for every session Admit lets through.
		writeExportError(w, r, options, familyexport.ErrOwnerRequired)
		return access.Principal{}, false
	}
	return p, true
}

// admitExport is the export routes' Admit: every check the legacy handler
// made between authentication and the service's family read. The rate limit
// is charged here, before the route's own refusal and before family policy,
// where the legacy handler charged it; it is in memory and never blocks.
func admitExport(r *http.Request, options Options, p access.Principal, headerRefusal, refusal string) error {
	if p.Scope != "family" || p.SessionScope != "family" || !protocol.ValidUUID(p.SessionFamilyID) {
		return exportRefusal("access_expired")
	}
	if options.ExportService == nil {
		// Export is off on this node (cfg.Export.Enabled): a signed-in
		// family member is told so after the credential, as before.
		return exportRefusal("service_unavailable")
	}
	if headerRefusal != "" {
		return syncRefusal(headerRefusal)
	}
	if options.Limiter == nil || options.ClientIPResolver == nil {
		return exportRefusal("service_unavailable")
	}
	address, err := options.ClientIPResolver.Resolve(r.RemoteAddr, r.Header)
	if err != nil {
		return exportRefusal("invalid_request")
	}
	charges := []limits.Charge{
		{Scope: limits.ScopeExportAccount, Key: p.AccountID, Cost: 1, Policy: options.ExportAccountPolicy},
		{Scope: limits.ScopeExportDevice, Key: p.DeviceID, Cost: 1, Policy: options.ExportDevicePolicy},
	}
	if r.Method == http.MethodPost {
		charges = append(charges, limits.Charge{Scope: limits.ScopeExportIP, Key: address.String(), Cost: 1, Policy: options.ExportIPPolicy})
	}
	decision, err := options.Limiter.TakeMany(r.Context(), charges)
	if err != nil {
		// The limiter's error stays the fault line's cause.
		return fmt.Errorf("%w: %w", exportRefusal("service_unavailable"), err)
	}
	if !decision.Allowed {
		return rateLimited{decision}
	}
	if refusal != "" {
		return exportRefusal(refusal)
	}
	return nil
}

// writeExportRequestError writes an export authorization's refusal as the
// legacy handler wrote it: its own refusals with writeExportFault, a header
// refusal with writeSyncFault, a rate-limit denial with writeSyncRateLimited,
// an authentication refusal (before Admit) as the sync routes write it, and
// a family-policy refusal (after Admit) as the service's family read was
// written (writeExportError). The error is each 5xx's logged cause.
func writeExportRequestError(w http.ResponseWriter, r *http.Request, options Options, err error, admitted bool) {
	var own exportRefusal
	var refusal syncRefusal
	var limited rateLimited
	switch {
	case errors.As(err, &own):
		writeExportFault(w, r, options, string(own), err)
	case errors.As(err, &refusal):
		writeSyncFault(w, r, options, string(refusal), err)
	case errors.As(err, &limited):
		writeSyncRateLimited(w, r, options, limited.decision)
	case admitted:
		writeExportError(w, r, options, err)
	default:
		writeSyncFamilyFault(w, r, options, auth.Code(err), err)
	}
}

// exportCreateRefusal is a create request's refusal ("" when acceptable):
// its query, its bounded body, the decode, the route capabilities and the
// request's own fields, in the legacy order.
func exportCreateRefusal(r *http.Request, options Options) (protocol.CreateExportRequest, string) {
	var input protocol.CreateExportRequest
	if r.URL.RawQuery != "" {
		return input, "invalid_request"
	}
	body, oversized, err := boundedBody(r, 4<<10)
	if oversized {
		return input, "body_too_large"
	}
	if err != nil {
		return input, "invalid_request"
	}
	if err := protocol.DecodeRequest(bytes.NewReader(body), 4<<10, &input); err != nil {
		return input, protocol.ErrorCodeOf(err)
	}
	if err := protocol.ValidateRouteCapabilities(protocol.SurfaceExport, input.Capabilities, input.Capabilities, advertisedCapabilities(options)); err != nil {
		return input, protocol.ErrorCodeOf(err)
	}
	if !familyexport.ValidCreateRequest(input) {
		return input, "invalid_request"
	}
	return input, ""
}

// exportIDRefusal is a status or download request's refusal ("" when
// acceptable): a query, or an export ID that is not a UUID.
func exportIDRefusal(r *http.Request) (string, string) {
	id := r.PathValue("id")
	if r.URL.RawQuery != "" || !protocol.ValidUUID(id) {
		return id, "invalid_request"
	}
	return id, ""
}

func exportCreateHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		input, refusal := exportCreateRefusal(r, options)
		p, ok := exportPrincipal(w, r, options, refusal)
		if !ok {
			return
		}
		status, created, err := options.ExportService.Create(r.Context(), p, input)
		if err != nil {
			writeExportError(w, r, options, err)
			return
		}
		if created {
			writeJSON(w, http.StatusAccepted, status)
		} else {
			writeJSON(w, http.StatusOK, status)
		}
	}
}

func exportStatusHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, refusal := exportIDRefusal(r)
		p, ok := exportPrincipal(w, r, options, refusal)
		if !ok {
			return
		}
		status, err := options.ExportService.Status(r.Context(), p, id)
		if err != nil {
			writeExportError(w, r, options, err)
			return
		}
		writeJSON(w, http.StatusOK, status)
	}
}

type exportDownloadWriter struct {
	w       http.ResponseWriter
	started bool
}

func (writer *exportDownloadWriter) Write(data []byte) (int, error) {
	if !writer.started {
		writer.w.WriteHeader(http.StatusOK)
		writer.started = true
	}
	return writer.w.Write(data)
}

func exportDownloadHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, refusal := exportIDRefusal(r)
		p, ok := exportPrincipal(w, r, options, refusal)
		if !ok {
			return
		}
		status, err := options.ExportService.Status(r.Context(), p, id)
		if err != nil {
			writeExportError(w, r, options, err)
			return
		}
		if status.State == "expired" {
			writeExportCode(w, r, options, "export_expired")
			return
		}
		if status.State != "ready" || status.PlaintextBytes == nil {
			writeExportCode(w, r, options, "export_not_ready")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="baby-tracker-family-v1.json"`)
		w.Header().Set("Content-Length", strconv.FormatUint(*status.PlaintextBytes, 10))
		writer := &exportDownloadWriter{w: w}
		deadline := time.Now().Add(30 * time.Second)
		if expires, err := time.Parse("2006-01-02T15:04:05.000000Z", status.ExpiresAt); err == nil && expires.Before(deadline) {
			deadline = expires
		}
		ctx, cancel := context.WithDeadline(r.Context(), deadline)
		defer cancel()
		if err := options.ExportService.Download(ctx, p, id, writer); err != nil {
			if !writer.started {
				w.Header().Del("Content-Disposition")
				w.Header().Del("Content-Length")
				writeExportError(w, r, options, err)
			}
			return
		}
		if !writer.started {
			w.WriteHeader(http.StatusOK)
		}
	}
}

func writeExportError(w http.ResponseWriter, r *http.Request, options Options, err error) {
	code := "service_unavailable"
	switch {
	case errors.Is(err, familyexport.ErrInvalidRequest):
		code = "invalid_request"
	case errors.Is(err, familyexport.ErrOwnerRequired), family.Code(err) == "owner_required":
		code = "owner_required"
	case errors.Is(err, familyexport.ErrNotFound):
		code = "export_not_found"
	case errors.Is(err, familyexport.ErrInProgress):
		code = "export_in_progress"
	case errors.Is(err, familyexport.ErrRateLimited):
		code = "export_rate_limited"
	case errors.Is(err, familyexport.ErrNotReady):
		code = "export_not_ready"
	case errors.Is(err, familyexport.ErrExpired):
		code = "export_expired"
	case errors.Is(err, familyexport.ErrIntegrity):
		code = "export_integrity"
	case errors.Is(err, familyexport.ErrTooLarge):
		code = "export_too_large"
	case errors.Is(err, familyexport.ErrAuthorizationChanged):
		code = "export_authorization_changed"
	}
	writeExportFault(w, r, options, code, err)
}

func writeExportCode(w http.ResponseWriter, r *http.Request, options Options, code string) {
	writeExportFault(w, r, options, code, nil)
}

// writeExportFault writes the export refusal for code and, for a 5xx, logs it
// once with cause (logServerFault).
func writeExportFault(w http.ResponseWriter, r *http.Request, options Options, code string, cause error) {
	status, retryable, message := http.StatusServiceUnavailable, true, "Service is unavailable"
	switch code {
	case "invalid_request", "unsupported_protocol":
		status, retryable, message = http.StatusBadRequest, false, "Invalid export request"
	case "body_too_large":
		status, retryable, message = http.StatusRequestEntityTooLarge, false, "Export request is too large"
	case "unsupported_capability", "export_in_progress", "export_not_ready", "export_authorization_changed", "export_too_large":
		status, retryable, message = http.StatusConflict, false, "Export is unavailable"
	case "access_expired":
		status, retryable, message = http.StatusUnauthorized, false, "Authentication failed"
		w.Header().Set("WWW-Authenticate", "Bearer")
	case "owner_required":
		status, retryable, message = http.StatusForbidden, false, "Owner access is required"
	case "export_not_found":
		status, retryable, message = http.StatusNotFound, false, "Export was not found"
	case "export_expired":
		status, retryable, message = http.StatusGone, false, "Export has expired"
	case "export_rate_limited":
		status, retryable, message = http.StatusTooManyRequests, true, "Export rate limit exceeded"
		w.Header().Set("Retry-After", strconv.Itoa(int((6*time.Hour)/time.Second)))
	case "export_integrity", "service_unavailable":
	default:
		code = "service_unavailable"
	}
	response := newErrorResponse(code, message, requestIDFromContext(r.Context()), retryable)
	response.ServerInstanceID, response.RecoveryEpoch = options.InstanceID, options.RecoveryEpoch
	logServerFault(requestLogger(r, options.Logger), r, status, code, cause)
	writeJSON(w, status, response)
}

var _ io.Writer = (*exportDownloadWriter)(nil)
