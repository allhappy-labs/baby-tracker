package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"babytracker/server/internal/access"
	familyexport "babytracker/server/internal/export"
	"babytracker/server/internal/family"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
)

func exportPrincipal(w http.ResponseWriter, r *http.Request, options Options) (access.Principal, bool) {
	w.Header().Set("Cache-Control", "no-store")
	p, ok := authenticateSyncFamilyRequest(w, r, options)
	if !ok {
		return access.Principal{}, false
	}
	if p.Scope != "family" || p.SessionScope != "family" || !protocol.ValidUUID(p.SessionFamilyID) {
		writeExportCode(w, r, options, "access_expired")
		return access.Principal{}, false
	}
	if options.ExportService == nil {
		writeExportCode(w, r, options, "service_unavailable")
		return access.Principal{}, false
	}
	if _, ok := validateSyncHeaders(w, r, protocol.SurfaceExport, options); !ok {
		return access.Principal{}, false
	}
	if options.Limiter == nil || options.ClientIPResolver == nil {
		writeExportCode(w, r, options, "service_unavailable")
		return access.Principal{}, false
	}
	address, err := options.ClientIPResolver.Resolve(r.RemoteAddr, r.Header)
	if err != nil {
		writeExportCode(w, r, options, "invalid_request")
		return access.Principal{}, false
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
		writeExportFault(w, r, options, "service_unavailable", err)
		return access.Principal{}, false
	}
	if !decision.Allowed {
		writeSyncRateLimited(w, r, options, decision)
		return access.Principal{}, false
	}
	return p, true
}

func exportCreateHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := exportPrincipal(w, r, options)
		if !ok {
			return
		}
		if r.URL.RawQuery != "" {
			writeExportCode(w, r, options, "invalid_request")
			return
		}
		body, oversized, err := boundedBody(r, 4<<10)
		if oversized {
			writeExportCode(w, r, options, "body_too_large")
			return
		}
		if err != nil {
			writeExportCode(w, r, options, "invalid_request")
			return
		}
		var input protocol.CreateExportRequest
		if err := protocol.DecodeRequest(bytes.NewReader(body), 4<<10, &input); err != nil {
			writeExportCode(w, r, options, protocol.ErrorCodeOf(err))
			return
		}
		if err := protocol.ValidateRouteCapabilities(protocol.SurfaceExport, input.Capabilities, input.Capabilities, advertisedCapabilities(options)); err != nil {
			writeExportCode(w, r, options, protocol.ErrorCodeOf(err))
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
		p, ok := exportPrincipal(w, r, options)
		if !ok {
			return
		}
		id := r.PathValue("id")
		if r.URL.RawQuery != "" || !protocol.ValidUUID(id) {
			writeExportCode(w, r, options, "invalid_request")
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
		p, ok := exportPrincipal(w, r, options)
		if !ok {
			return
		}
		id := r.PathValue("id")
		if r.URL.RawQuery != "" || !protocol.ValidUUID(id) {
			writeExportCode(w, r, options, "invalid_request")
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
