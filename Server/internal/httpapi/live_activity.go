package httpapi

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/family"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/liveactivity"
	"babytracker/server/internal/protocol"
)

// The Live Activity registration routes (capability live-activity-push-v1).
const (
	liveActivityDeviceRoute         = "/v1/live-activities/device"
	liveActivityActivityRoute       = "/v1/live-activities/activity"
	liveActivityUnregistrationRoute = "/v1/live-activities/device/{installation_id}"
	// liveActivityBodyBytes is a registration body's bound (main's 16 KiB).
	liveActivityBodyBytes int64 = 16 << 10
)

// LiveActivityService registers devices and cards (liveactivity.Service),
// each call one guarded request transaction with the request's checks in the
// authority.Request. It is composed only while push is enabled, and its
// routes need the supervised worker to be ready (Options.LiveActivityReady).
type LiveActivityService interface {
	RegisterDevice(context.Context, authority.Request, protocol.LiveActivityDeviceRegistration) (protocol.LiveActivityRegistrationReceipt, error)
	RegisterActivity(context.Context, authority.Request, protocol.LiveActivityTokenRegistration) (protocol.LiveActivityRegistrationReceipt, error)
}

// LiveActivityUnregistration revokes an installation's registration
// (liveactivity.Unregistration). It is the repository-only path — no worker,
// APNs key or push configuration — so a phone can withdraw while push is
// disabled or the worker is stopped.
type LiveActivityUnregistration interface {
	UnregisterDevice(context.Context, authority.Request, string, uint64) error
}

// liveActivityConfigured is whether push is composed: the registration
// service and the worker's readiness.
func liveActivityConfigured(options Options) bool {
	return options.LiveActivityService != nil && options.LiveActivityReady != nil
}

// liveActivityRegistrationReady is main's rule for the PUT routes: the
// service is composed and the worker is ready.
func liveActivityRegistrationReady(ctx context.Context, options Options) bool {
	return liveActivityConfigured(options) && options.LiveActivityReady(ctx) == nil
}

// liveActivityUnregistrationCapabilities is the capabilities the DELETE
// route accepts. Unregistering belongs to live-activity-push-v1 but needs
// none of it on the server, so the route accepts that capability header
// whether or not push is advertised; the phone withdrawing from push is
// exactly the phone whose server no longer offers it.
func liveActivityUnregistrationCapabilities(options Options) []string {
	accepted := advertisedCapabilities(options)
	if slices.Contains(accepted, protocol.CapabilityLiveActivityPushV1) {
		return accepted
	}
	return append(accepted, protocol.CapabilityLiveActivityPushV1)
}

// liveActivityRegistrationHandler serves PUT /v1/live-activities/device
// (device) or /v1/live-activities/activity as one guarded request
// transaction, in syncHandler's shape: readiness and the bearer token's
// digests before any database work; the worker's readiness, the headers,
// the query (none) and the body, read (before the route's admission slot,
// admitAfterBody) and judged in full, any refusal held for Admit (after
// authentication, before family policy); the account and device charges in
// Charge (after family policy). Tokens are never echoed or logged.
func liveActivityRegistrationHandler(options Options, device bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		digests, ok := syncCredential(w, r, options)
		if !ok {
			return
		}
		if options.LiveActivityService == nil || options.Limiter == nil {
			writeSyncCode(w, r, options, "service_unavailable")
			return
		}
		admitted, ok := admitAfterBody(w, r, options, liveActivityBodyBytes)
		if !ok {
			return
		}
		defer admitted.release()
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		refusal := ""
		if !liveActivityRegistrationReady(ctx, options) {
			refusal = "service_unavailable"
		}
		request := func(held string) authority.Request {
			if refusal == "" {
				refusal = held
			}
			return authority.Request{Digests: digests, Admit: admitLiveActivity(refusal), Charge: chargeLiveActivity(ctx, options)}
		}
		var receipt protocol.LiveActivityRegistrationReceipt
		var err error
		if device {
			var input protocol.LiveActivityDeviceRegistration
			held := readLiveActivityBody(r, options, &input)
			receipt, err = options.LiveActivityService.RegisterDevice(ctx, request(held), input)
		} else {
			var input protocol.LiveActivityTokenRegistration
			held := readLiveActivityBody(r, options, &input)
			receipt, err = options.LiveActivityService.RegisterActivity(ctx, request(held), input)
		}
		admitted.noteCredential(ctx, err)
		if err != nil {
			writeLiveActivityError(w, r, options, err)
			return
		}
		writeJSON(w, http.StatusOK, receipt)
	}
}

// liveActivityUnregistrationHandler serves DELETE
// /v1/live-activities/device/{installation_id}?version=N as one guarded
// request transaction, with the headers, the query and the path held for
// Admit and the same charges as registration. It needs only
// Options.LiveActivityUnregistration.
func liveActivityUnregistrationHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		digests, ok := syncCredential(w, r, options)
		if !ok {
			return
		}
		if options.LiveActivityUnregistration == nil || options.Limiter == nil {
			writeSyncCode(w, r, options, "service_unavailable")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		installation := r.PathValue("installation_id")
		version, refusal := readLiveActivityUnregistration(r, options, installation)
		err := options.LiveActivityUnregistration.UnregisterDevice(ctx, authority.Request{Digests: digests, Admit: admitLiveActivity(refusal),
			Charge: chargeLiveActivity(ctx, options)}, installation, version)
		if err != nil {
			writeLiveActivityError(w, r, options, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// liveActivityHeaders reads the protocol and capability headers against
// accepted and checks the route's capability, returning the refusal's code
// ("" when the headers are acceptable).
func liveActivityHeaders(r *http.Request, accepted []string) string {
	if r.Header.Get("X-Baby-Tracker-Protocol") != "v1" {
		return "unsupported_protocol"
	}
	values := r.Header.Values("X-Baby-Tracker-Capabilities")
	if len(values) != 1 || values[0] == "" || strings.ContainsAny(values[0], " \t\r\n") {
		return "unsupported_capability"
	}
	capabilities := strings.Split(values[0], ",")
	if err := protocol.ValidateCapabilities(capabilities, accepted); err != nil {
		return protocol.ErrorCodeOf(err)
	}
	if err := protocol.ValidateRouteCapabilities(protocol.SurfaceLiveActivity, capabilities, nil, accepted); err != nil {
		return protocol.ErrorCodeOf(err)
	}
	return ""
}

// readLiveActivityBody reads and judges a PUT in main's order: the headers,
// the query (none), the bounded body (body_too_large), the strict decode and
// the registration's own validation. It returns the first refusal's code
// ("" when the request is acceptable).
func readLiveActivityBody(r *http.Request, options Options, target interface{ Validate() error }) string {
	if refusal := liveActivityHeaders(r, advertisedCapabilities(options)); refusal != "" {
		return refusal
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		return "invalid_request"
	}
	body, oversized, err := boundedBody(r, liveActivityBodyBytes)
	if oversized {
		return "body_too_large"
	}
	if err != nil {
		return "invalid_request"
	}
	if err := protocol.DecodeRequest(bytes.NewReader(body), liveActivityBodyBytes, target); err != nil {
		return protocol.ErrorCodeOf(err)
	}
	if err := target.Validate(); err != nil {
		return protocol.ErrorCodeOf(err)
	}
	return ""
}

// readLiveActivityUnregistration judges a DELETE: the headers (accepting
// live-activity-push-v1 whether or not it is advertised), then exactly one
// canonical version query parameter and a UUID installation.
func readLiveActivityUnregistration(r *http.Request, options Options, installation string) (uint64, string) {
	if refusal := liveActivityHeaders(r, liveActivityUnregistrationCapabilities(options)); refusal != "" {
		return 0, refusal
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	raw := query.Get("version")
	version, versionErr := strconv.ParseUint(raw, 10, 64)
	if err != nil || len(query) != 1 || len(query["version"]) != 1 || versionErr != nil || version == 0 || version > protocol.MaxSafeInteger ||
		strconv.FormatUint(version, 10) != raw || !protocol.ValidUUID(installation) {
		return 0, "invalid_request"
	}
	return version, ""
}

// admitLiveActivity is the routes' Admit: main's refusal of anything but a
// refreshed family session (access_expired), then the held refusal.
func admitLiveActivity(refusal string) func(access.Principal) error {
	return func(p access.Principal) error {
		if p.Scope != "family" || p.SessionScope != "family" || !protocol.ValidUUID(p.SessionFamilyID) {
			return syncRefusal("access_expired")
		}
		if refusal != "" {
			return syncRefusal(refusal)
		}
		return nil
	}
}

// chargeLiveActivity is the routes' Charge: the account's and the device's
// Live Activity budgets, in memory, all or none.
func chargeLiveActivity(ctx context.Context, options Options) func(access.Principal, access.Grant) error {
	return func(p access.Principal, _ access.Grant) error {
		decision, err := options.Limiter.TakeMany(ctx, []limits.Charge{
			{Scope: limits.ScopeLiveActivityAccount, Key: p.AccountID, Cost: 1, Policy: options.LiveActivityAccountPolicy},
			{Scope: limits.ScopeLiveActivityDevice, Key: p.DeviceID, Cost: 1, Policy: options.LiveActivityDevicePolicy},
		})
		if err != nil {
			return limiterUnavailable(err)
		}
		if !decision.Allowed {
			return rateLimited{decision}
		}
		return nil
	}
}

// writeLiveActivityError is main's mapping: a version conflict (409) and a
// refused registration (403) with their own codes, a protocol refusal with
// its code, anything else as writeRequestError writes it. An expected
// refusal is logged with its code; an unexpected (5xx) error leaves the one
// fault line (logServerFault), its cause described, never its text.
func writeLiveActivityError(w http.ResponseWriter, r *http.Request, options Options, err error) {
	if errors.Is(err, liveactivity.ErrVersionConflict) || errors.Is(err, liveactivity.ErrRegistrationDenied) {
		status, code := http.StatusConflict, "live_activity_version_conflict"
		if errors.Is(err, liveactivity.ErrRegistrationDenied) {
			status, code = http.StatusForbidden, "live_activity_registration_denied"
		}
		response := newErrorResponse(code, "Live Activity registration was refused", requestIDFromContext(r.Context()), false)
		response.ServerInstanceID, response.RecoveryEpoch = options.InstanceID, options.RecoveryEpoch
		writeJSON(w, status, response)
		return
	}
	if code := protocol.ErrorCodeOf(err); code == "invalid_request" || code == "unsupported_capability" || code == "unsupported_protocol" {
		writeSyncCode(w, r, options, code)
		return
	}
	var refusal syncRefusal
	var limited rateLimited
	if !errors.As(err, &refusal) && !errors.As(err, &limited) {
		code := family.Code(err)
		if options.Logger != nil {
			requestID, route := slog.String("request_id", requestIDFromContext(r.Context())), slog.String("route", normalizedRoute(r.Pattern))
			_, status, _, _ := syncFamilyResponse(code)
			switch {
			case code == "access_expired":
				options.Logger.Debug("live activity request refused", requestID, route, slog.String("code", code))
			case status != http.StatusServiceUnavailable:
				options.Logger.Info("live activity request refused", requestID, route, slog.String("code", code))
			}
		}
		writeSyncFamilyFault(w, r, options, code, err)
		return
	}
	writeRequestError(w, r, options, err)
}
