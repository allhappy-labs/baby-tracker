package httpapi

import (
	"bytes"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/family"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/liveactivity"
	"babytracker/server/internal/protocol"
)

// Registration (PUT) requires the composed service and a running worker.
// Unregistering is the privacy-protecting direction: it needs only the
// repository path, and works while push is disabled or the worker is stopped.
func liveActivityPrincipal(w http.ResponseWriter, r *http.Request, o Options, available bool, accepted []string) (access.Principal, bool) {
	w.Header().Set("Cache-Control", "no-store")
	p, ok := authenticateSyncFamilyRequest(w, r, o)
	if !ok {
		return p, false
	}
	if p.Scope != "family" || p.SessionScope != "family" || !protocol.ValidUUID(p.SessionFamilyID) {
		writeSyncCode(w, r, o, "access_expired")
		return p, false
	}
	if !available || o.Limiter == nil {
		writeSyncCode(w, r, o, "service_unavailable")
		return p, false
	}
	if _, ok = validateHeadersAgainst(w, r, protocol.SurfaceLiveActivity, o, accepted); !ok {
		return p, false
	}
	decision, err := o.Limiter.TakeMany(r.Context(), []limits.Charge{
		{Scope: limits.ScopeLiveActivityAccount, Key: p.AccountID, Cost: 1, Policy: limits.Policy{Count: 120, Window: time.Minute}},
		{Scope: limits.ScopeLiveActivityDevice, Key: p.DeviceID, Cost: 1, Policy: limits.Policy{Count: 30, Window: time.Minute}},
	})
	if err != nil {
		writeSyncFault(w, r, o, "service_unavailable", err)
		return p, false
	}
	if !decision.Allowed {
		writeSyncRateLimited(w, r, o, decision)
		return p, false
	}
	return p, true
}
func liveActivityBody(w http.ResponseWriter, r *http.Request, o Options, target interface{ Validate() error }) bool {
	if r.URL.RawQuery != "" {
		writeSyncCode(w, r, o, "invalid_request")
		return false
	}
	body, oversized, err := boundedBody(r, 16<<10)
	if oversized {
		writeSyncCode(w, r, o, "body_too_large")
		return false
	}
	if err != nil {
		writeSyncCode(w, r, o, "invalid_request")
		return false
	}
	if err = protocol.DecodeRequest(bytes.NewReader(body), 16<<10, target); err != nil {
		writeSyncCode(w, r, o, protocol.ErrorCodeOf(err))
		return false
	}
	if err = target.Validate(); err != nil {
		writeSyncCode(w, r, o, protocol.ErrorCodeOf(err))
		return false
	}
	return true
}
func liveActivityRegistrationAvailable(r *http.Request, o Options) bool {
	return o.LiveActivityService != nil && o.LiveActivityReadiness != nil && o.LiveActivityReadiness.Check(r.Context()) == nil
}

// Unregistering belongs to live-activity-push-v1 but needs none of it on the
// server, so the route accepts that capability header whether or not push is
// advertised; the phone withdrawing from push is exactly the phone whose
// server no longer offers it.
func liveActivityUnregistrationCapabilities(o Options) []string {
	accepted := advertisedCapabilities(o)
	if slices.Contains(accepted, protocol.CapabilityLiveActivityPushV1) {
		return accepted
	}
	return append(accepted, protocol.CapabilityLiveActivityPushV1)
}
func liveActivityDeviceHandler(o Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := liveActivityPrincipal(w, r, o, liveActivityRegistrationAvailable(r, o), advertisedCapabilities(o))
		if !ok {
			return
		}
		var input protocol.LiveActivityDeviceRegistration
		if !liveActivityBody(w, r, o, &input) {
			return
		}
		receipt, err := o.LiveActivityService.RegisterDevice(r.Context(), p, input)
		if err != nil {
			writeLiveActivityError(w, r, o, err)
			return
		}
		writeJSON(w, http.StatusOK, receipt)
	}
}
func liveActivityTokenHandler(o Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := liveActivityPrincipal(w, r, o, liveActivityRegistrationAvailable(r, o), advertisedCapabilities(o))
		if !ok {
			return
		}
		var input protocol.LiveActivityTokenRegistration
		if !liveActivityBody(w, r, o, &input) {
			return
		}
		receipt, err := o.LiveActivityService.RegisterActivity(r.Context(), p, input)
		if err != nil {
			writeLiveActivityError(w, r, o, err)
			return
		}
		writeJSON(w, http.StatusOK, receipt)
	}
}
func liveActivityDeleteHandler(o Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := liveActivityPrincipal(w, r, o, o.LiveActivityUnregistration != nil, liveActivityUnregistrationCapabilities(o))
		if !ok {
			return
		}
		query, err := url.ParseQuery(r.URL.RawQuery)
		raw := query.Get("version")
		version, versionErr := strconv.ParseUint(raw, 10, 64)
		if err != nil || len(query) != 1 || len(query["version"]) != 1 || versionErr != nil || version == 0 || version > protocol.MaxSafeInteger || strconv.FormatUint(version, 10) != raw || !protocol.ValidUUID(r.PathValue("installation_id")) {
			writeSyncCode(w, r, o, "invalid_request")
			return
		}
		if err = o.LiveActivityUnregistration.UnregisterDevice(r.Context(), p, r.PathValue("installation_id"), version); err != nil {
			writeLiveActivityError(w, r, o, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
func writeLiveActivityError(w http.ResponseWriter, r *http.Request, o Options, err error) {
	if errors.Is(err, liveactivity.ErrVersionConflict) || errors.Is(err, liveactivity.ErrRegistrationDenied) {
		status, code := http.StatusConflict, "live_activity_version_conflict"
		if errors.Is(err, liveactivity.ErrRegistrationDenied) {
			status, code = http.StatusForbidden, "live_activity_registration_denied"
		}
		response := newErrorResponse(code, "Live Activity registration was refused", requestIDFromContext(r.Context()), false)
		response.ServerInstanceID, response.RecoveryEpoch = o.InstanceID, o.RecoveryEpoch
		writeJSON(w, status, response)
		return
	}
	if code := protocol.ErrorCodeOf(err); code == "invalid_request" || code == "unsupported_capability" || code == "unsupported_protocol" {
		writeSyncCode(w, r, o, code)
		return
	}
	writeSyncFamilyFault(w, r, o, family.Code(err), err)
}
