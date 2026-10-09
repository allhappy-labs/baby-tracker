package httpapi

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/records"
)

func importHandler(options Options, operation string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, ok := authenticateSyncFamilyRequest(w, r, options)
		if !ok {
			return
		}
		if p.Scope != "family" {
			writeSyncCode(w, r, options, "family_scope_required")
			return
		}
		if p.SessionScope != "family" || !protocol.ValidUUID(p.SessionFamilyID) {
			writeSyncCode(w, r, options, "access_expired")
			return
		}
		if options.ImportService == nil || options.SyncLimits == nil || options.Limiter == nil {
			writeSyncCode(w, r, options, "service_unavailable")
			return
		}
		caps, ok := validateSyncHeaders(w, r, protocol.SurfaceImport, options)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(options.SyncLimits.SyncTransactionSeconds)*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		var input protocol.ImportResolveRequest
		var choice protocol.ImportConflictRequest
		var token *string
		var e error
		id := r.PathValue("import_id")
		if operation != "resolve" && !protocol.ValidUUID(id) {
			writeSyncCode(w, r, options, "invalid_request")
			return
		}
		if operation == "status" {
			values, parseErr := url.ParseQuery(r.URL.RawQuery)
			if parseErr != nil {
				writeSyncCode(w, r, options, "invalid_request")
				return
			}
			token, e = snapshotPageToken(values)
			if e != nil {
				writeSyncCode(w, r, options, "invalid_request")
				return
			}
		} else {
			if r.URL.RawQuery != "" {
				writeSyncCode(w, r, options, "invalid_request")
				return
			}
			body, oversized, e := boundedBody(r, int64(options.SyncLimits.SyncBodyBytes))
			if oversized {
				writeSyncCode(w, r, options, "body_too_large")
				return
			}
			if e != nil {
				writeSyncCode(w, r, options, "invalid_request")
				return
			}
			var bodyCaps []string
			if operation == "resolve" {
				e = protocol.DecodeRequest(bytes.NewReader(body), int64(options.SyncLimits.SyncBodyBytes), &input)
				bodyCaps = input.Capabilities
			} else {
				e = protocol.DecodeRequest(bytes.NewReader(body), int64(options.SyncLimits.SyncBodyBytes), &choice)
				bodyCaps = choice.Capabilities
			}
			if e != nil {
				writeSyncCode(w, r, options, protocol.ErrorCodeOf(e))
				return
			}
			if e = protocol.ValidateRouteCapabilities(protocol.SurfaceImport, caps, bodyCaps, advertisedCapabilities(options)); e != nil {
				writeSyncCode(w, r, options, protocol.ErrorCodeOf(e))
				return
			}
		}
		view, e := options.FamilyService.Current(ctx, p)
		if e != nil {
			writeSyncFamilyError(w, r, options, e)
			return
		}
		if p.SessionFamilyID != view.ID {
			writeSyncCode(w, r, options, "access_expired")
			return
		}
		charges := []limits.Charge{{Scope: limits.ScopeSyncPullDevice, Key: p.DeviceID, Cost: 1, Policy: options.SyncPullDevicePolicy}, {Scope: limits.ScopeSyncPullAccount, Key: p.AccountID, Cost: 1, Policy: options.SyncPullAccountPolicy}}
		if operation != "status" {
			charges = append(charges, limits.Charge{Scope: limits.ScopeSyncMutationDevice, Key: p.DeviceID, Cost: 1, Policy: options.SyncMutationDevicePolicy}, limits.Charge{Scope: limits.ScopeSyncMutationAccount, Key: p.AccountID, Cost: 1, Policy: options.SyncMutationAccountPolicy}, limits.Charge{Scope: limits.ScopeSyncMutationFamily, Key: view.ID, Cost: 1, Policy: options.SyncMutationFamilyPolicy})
		}
		decision, e := options.Limiter.TakeMany(ctx, charges)
		if e != nil {
			writeSyncFault(w, r, options, "service_unavailable", e)
			return
		}
		if !decision.Allowed {
			writeSyncRateLimited(w, r, options, decision)
			return
		}
		var out protocol.ImportResolveResponse
		switch operation {
		case "resolve":
			out, e = options.ImportService.Resolve(ctx, p, input)
		case "conflict":
			out, e = options.ImportService.ResolveConflict(ctx, p, id, choice)
		default:
			out, e = options.ImportService.StatusPage(ctx, p, id, token)
		}
		if e != nil {
			writeImportError(w, r, options, e)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}
func writeImportError(w http.ResponseWriter, r *http.Request, options Options, err error) {
	value := err.Error()
	status := http.StatusConflict
	switch value {
	case "invalid_request", "invalid_page_token":
		writeSyncCode(w, r, options, value)
		return
	case "access_expired", "family_scope_required", "page_expired":
		writeSyncCode(w, r, options, value)
		return
	case "import_expired":
		status = http.StatusGone
	case "import_access_denied", "owner_required":
		status = http.StatusForbidden
	case "import_not_found":
		status = http.StatusNotFound
	case "import_history_limit", "import_unfinished_limit", "import_chunk_reused", "import_chunk_out_of_order", "import_manifest_changed", "import_manifest_incomplete", "import_resolution_stale", "import_target_already_assigned", "baby_uuid_collision", "command_id_reused", "import_page_limit":
	default:
		if errors.Is(err, records.ErrQuotaExceeded) {
			value = "family_storage_limit"
		} else {
			writeSyncFamilyError(w, r, options, err)
			return
		}
	}
	response := newErrorResponse(value, "Import request could not be completed", requestIDFromContext(r.Context()), false)
	response.ServerInstanceID, response.RecoveryEpoch = options.InstanceID, options.RecoveryEpoch
	writeJSON(w, status, response)
}
