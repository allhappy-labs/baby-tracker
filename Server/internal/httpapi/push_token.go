package httpapi

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/family"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
)

// pushTokenRoute is the device push-token route (capability push-token-v1),
// registered only when Options.PushTokens is set.
const pushTokenRoute = "/v1/devices/push-token"

// pushTokenHandler serves PUT (register) or DELETE (unregister)
// /v1/devices/push-token as one guarded request transaction, in
// syncHandler's shape: readiness and the bearer token's digests before any
// database work; the headers, the query and the body read (before the
// route's admission slot, admitAfterBody) and judged in full and any
// refusal held for Admit (after authentication, before family
// policy); the in-memory device and account charges in Charge (after family
// policy). The device is always the authenticated session's own: no device
// ID is read from the request. The token is never echoed or logged.
func pushTokenHandler(options Options, register bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		digests, ok := syncCredential(w, r, options)
		if !ok {
			return
		}
		if options.PushTokens == nil || options.Limiter == nil {
			writeSyncCode(w, r, options, "service_unavailable")
			return
		}
		admitted, ok := admitAfterBody(w, r, options, protocol.PushTokenBodyBytes)
		if !ok {
			return
		}
		defer admitted.release()
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		var err error
		if register {
			input, token, refusal := readPushTokenRequest(r, options)
			err = options.PushTokens.Register(ctx, authority.Request{Digests: digests, Admit: admitSync(refusal),
				Charge: chargePushToken(ctx, options)}, input.Environment, token)
		} else {
			refusal := readPushTokenDelete(r, options)
			err = options.PushTokens.Unregister(ctx, authority.Request{Digests: digests, Admit: admitSync(refusal),
				Charge: chargePushToken(ctx, options)})
		}
		admitted.noteCredential(ctx, err)
		if err != nil {
			writePushTokenError(w, r, options, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// readPushTokenRequest reads and judges a PUT in order: the protocol and
// capability headers, the query (none), the bounded body (body_too_large),
// the strict decode, the route capabilities of the header and the body,
// then the token and environment (invalid_push_token). It returns the
// first refusal's code ("" when the request is acceptable) and, only then,
// the decoded token.
func readPushTokenRequest(r *http.Request, options Options) (protocol.PushTokenRequest, []byte, string) {
	var input protocol.PushTokenRequest
	headerCapabilities, refusal := parseSyncHeaderValues(r, options)
	if refusal != "" {
		return input, nil, refusal
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		return input, nil, "invalid_request"
	}
	body, oversized, err := boundedBody(r, protocol.PushTokenBodyBytes)
	if oversized {
		return input, nil, "body_too_large"
	}
	if err != nil {
		return input, nil, "invalid_request"
	}
	if err := protocol.DecodeRequest(bytes.NewReader(body), protocol.PushTokenBodyBytes, &input); err != nil {
		return protocol.PushTokenRequest{}, nil, protocol.ErrorCodeOf(err)
	}
	if err := protocol.ValidateRouteCapabilities(protocol.SurfacePushToken, headerCapabilities, input.Capabilities, advertisedCapabilities(options)); err != nil {
		return protocol.PushTokenRequest{}, nil, protocol.ErrorCodeOf(err)
	}
	token, err := input.Decode()
	if err != nil {
		return protocol.PushTokenRequest{}, nil, protocol.ErrorCodeOf(err)
	}
	return input, token, ""
}

// readPushTokenDelete judges a DELETE: the headers (the route
// capabilities), no query and no body.
func readPushTokenDelete(r *http.Request, options Options) string {
	if _, refusal := syncHeaders(r, protocol.SurfacePushToken, options); refusal != "" {
		return refusal
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		return "invalid_request"
	}
	body, oversized, err := boundedBody(r, protocol.PushTokenBodyBytes)
	if oversized {
		return "body_too_large"
	}
	if err != nil || len(body) != 0 {
		return "invalid_request"
	}
	return ""
}

// chargePushToken is the route's Charge: the authenticated device's and
// account's push-token budgets, in memory, all or none.
func chargePushToken(ctx context.Context, options Options) func(access.Principal, access.Grant) error {
	return func(p access.Principal, _ access.Grant) error {
		decision, err := options.Limiter.TakeMany(ctx, []limits.Charge{
			{Scope: limits.ScopePushTokenDevice, Key: p.DeviceID, Cost: 1, Policy: options.PushTokenDevicePolicy},
			{Scope: limits.ScopePushTokenAccount, Key: p.AccountID, Cost: 1, Policy: options.PushTokenAccountPolicy},
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

// writePushTokenError is writeRequestError with the route's own refusal
// log: an expected refusal is logged with its code; an unexpected (5xx)
// error leaves the one fault line (logServerFault), its cause described,
// never its text (which a service error might fill with request data).
func writePushTokenError(w http.ResponseWriter, r *http.Request, options Options, err error) {
	var refusal syncRefusal
	var limited rateLimited
	if errors.Is(err, protocol.ErrInvalidPushToken) {
		writeSyncCode(w, r, options, "invalid_push_token")
		return
	}
	if !errors.As(err, &refusal) && !errors.As(err, &limited) && options.Logger != nil {
		code := family.Code(err)
		requestID, route := slog.String("request_id", requestIDFromContext(r.Context())), slog.String("route", normalizedRoute(r.Pattern))
		_, status, _, _ := syncFamilyResponse(code)
		switch {
		case code == "access_expired":
			// The routine refresh signal, as on the sync routes.
			options.Logger.Debug("push token request refused", requestID, route, slog.String("code", code))
		case status != http.StatusServiceUnavailable:
			options.Logger.Info("push token request refused", requestID, route, slog.String("code", code))
		}
		writeSyncFamilyFault(w, r, options, code, err)
		return
	}
	writeRequestError(w, r, options, err)
}
