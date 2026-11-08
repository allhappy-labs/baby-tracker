package httpapi

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/care"
	"babytracker/server/internal/protocol"
)

// importHandler serves the import routes. Before any database work it reads
// readiness (cached), the bearer token's digests, and the headers, path,
// query and body (resolve and resolve-conflict read it before taking their
// admission slot, admitAfterBody), validated in full; the first refusal is
// held. Then one pooled statement (RequestAuthorizer.AuthorizeRequest)
// authenticates, returns the held refusal after the session checks (Admit,
// as the sync routes do), applies family policy where the legacy family read
// stood, and charges the in-memory limits (Charge). The import service then runs its
// one guarded transaction, which re-reads the session.
func importHandler(options Options, operation string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		digests, ok := syncCredential(w, r, options)
		if !ok {
			return
		}
		if options.SyncLimits == nil || options.Limiter == nil || options.RequestAuthorizer == nil {
			writeSyncCode(w, r, options, "service_unavailable")
			return
		}
		noteCredential := func(context.Context, error) {}
		if operation != "status" {
			admitted, ok := admitAfterBody(w, r, options, digests, int64(options.SyncLimits.SyncBodyBytes), writeRequestError)
			if !ok {
				return
			}
			defer admitted.release()
			noteCredential = admitted.noteCredential
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(options.SyncLimits.SyncTransactionSeconds)*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		id := r.PathValue("import_id")
		var input protocol.ImportResolveRequest
		var choice protocol.ImportConflictRequest
		var token *string
		refusal := "service_unavailable" // no import service: refused after the session checks, as before
		if options.ImportService != nil {
			input, choice, token, refusal = readImportRequest(r, options, operation, id)
		}
		mutations := 1
		if operation == "status" {
			mutations = 0
		}
		charge := chargeSync(ctx, options, mutations)
		p, _, e := options.RequestAuthorizer.AuthorizeRequest(ctx, authority.Request{Digests: digests, Admit: admitSync(refusal),
			Charge: func(p access.Principal, g access.Grant) error {
				if g.FamilyID != p.SessionFamilyID {
					// The legacy family read's check, before the charge; the
					// grant is the session's family for every session Admit
					// lets through.
					return syncRefusal("access_expired")
				}
				return charge(p, g)
			}}, "read")
		noteCredential(ctx, e)
		if e != nil {
			writeRequestError(w, r, options, e)
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

// readImportRequest reads and validates an import request's headers, path,
// query and body in the legacy order, returning the first refusal's code
// instead of writing it ("" when the request is acceptable): the protocol
// and capability headers, the import ID (status and conflict), then the
// status query, or the empty query, the bounded body, its decode and the
// route capabilities.
func readImportRequest(r *http.Request, options Options, operation, id string) (protocol.ImportResolveRequest, protocol.ImportConflictRequest, *string, string) {
	var input protocol.ImportResolveRequest
	var choice protocol.ImportConflictRequest
	caps, refusal := syncHeaders(r, protocol.SurfaceImport, options)
	if refusal != "" {
		return input, choice, nil, refusal
	}
	if operation != "resolve" && !protocol.ValidUUID(id) {
		return input, choice, nil, "invalid_request"
	}
	if operation == "status" {
		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			return input, choice, nil, "invalid_request"
		}
		token, err := snapshotPageToken(values)
		if err != nil {
			return input, choice, nil, "invalid_request"
		}
		return input, choice, token, ""
	}
	if r.URL.RawQuery != "" {
		return input, choice, nil, "invalid_request"
	}
	body, oversized, err := boundedBody(r, int64(options.SyncLimits.SyncBodyBytes))
	if oversized {
		return input, choice, nil, "body_too_large"
	}
	if err != nil {
		return input, choice, nil, "invalid_request"
	}
	var bodyCaps []string
	if operation == "resolve" {
		err = protocol.DecodeRequest(bytes.NewReader(body), int64(options.SyncLimits.SyncBodyBytes), &input)
		bodyCaps = input.Capabilities
	} else {
		err = protocol.DecodeRequest(bytes.NewReader(body), int64(options.SyncLimits.SyncBodyBytes), &choice)
		bodyCaps = choice.Capabilities
	}
	if err != nil {
		return input, choice, nil, protocol.ErrorCodeOf(err)
	}
	if err = protocol.ValidateRouteCapabilities(protocol.SurfaceImport, caps, bodyCaps, advertisedCapabilities(options)); err != nil {
		return input, choice, nil, protocol.ErrorCodeOf(err)
	}
	return input, choice, nil, ""
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
		if errors.Is(err, care.ErrQuotaExceeded) {
			value = "family_storage_limit"
		} else {
			writeSyncError(w, r, options, err)
			return
		}
	}
	response := newErrorResponse(value, "Import request could not be completed", requestIDFromContext(r.Context()), false)
	response.ServerInstanceID, response.RecoveryEpoch = options.InstanceID, options.RecoveryEpoch
	writeJSON(w, status, response)
}
