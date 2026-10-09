package httpapi

import (
	"context"
	"net/http"
	"time"

	"babytracker/server/internal/protocol"
)

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, protocol.StatusResponse{Status: "ok"})
}

func readyHandler(readiness Readiness) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if readiness == nil || readiness.Check(ctx) != nil {
			writeError(w, http.StatusServiceUnavailable, "not_ready", "Service is not ready", requestIDFromContext(r.Context()), true)
			return
		}
		writeJSON(w, http.StatusOK, protocol.StatusResponse{Status: "ok"})
	}
}

func serverHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		instanceID, recoveryEpoch := options.InstanceID, options.RecoveryEpoch
		if options.ServerState != nil {
			var err error
			instanceID, recoveryEpoch, err = options.ServerState.ServerState(request.Context())
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, "not_ready", "Service is not ready", requestIDFromContext(request.Context()), true)
				return
			}
		}
		if !protocol.ValidRecoveryEpoch(recoveryEpoch) {
			writeError(w, http.StatusServiceUnavailable, "not_ready", "Service is not ready", requestIDFromContext(request.Context()), true)
			return
		}
		// Application readiness is evaluated at most once per request.
		checked, ready := false, false
		applicationReady := func() bool {
			if !checked {
				checked = true
				ready = options.Readiness != nil && options.Readiness.Check(request.Context()) == nil
			}
			return ready
		}
		capabilityOptions := options
		// LiveActivityReadiness is the cheap worker-local state; check it first.
		if options.LiveActivityService != nil && (options.LiveActivityReadiness == nil || options.LiveActivityReadiness.Check(request.Context()) != nil || !applicationReady()) {
			capabilityOptions.LiveActivityService = nil
		}
		if options.RecoveryEnabled && (!applicationReady() || options.RecoveryReadiness == nil || options.RecoveryReadiness.Check(request.Context()) != nil) {
			capabilityOptions.RecoveryEnabled = false
		}
		writeJSON(w, http.StatusOK, protocol.ServerInfo{
			InstanceID:             instanceID,
			RecoveryEpoch:          recoveryEpoch,
			CanonicalOrigin:        options.CanonicalOrigin,
			APIVersions:            []string{"v1"},
			Capabilities:           advertisedCapabilities(capabilityOptions),
			AppleAuthorizationMode: options.AppleAuthorizationMode,
			Limits:                 options.SyncLimits,
			SourceVersion:          options.SourceVersion,
			SourceURL:              options.SourceURL,
			MaxItemsPerPage:        syncPageSize(options.SyncLimits),
			MaxEncodedBytes:        syncPageBytes(options.SyncLimits),
		})
	}
}

func syncPageSize(limits *protocol.Limits) int {
	if limits == nil {
		return 0
	}
	return limits.SyncPageSize
}

func syncPageBytes(limits *protocol.Limits) int {
	if limits == nil {
		return 0
	}
	return limits.SyncPageBytes
}

func writeRetryError(w http.ResponseWriter, status int, code, message, requestID string, retryable bool, retryAfterSeconds int) {
	response := newErrorResponse(code, message, requestID, retryable)
	response.Error.RetryAfterSeconds = &retryAfterSeconds
	writeJSON(w, status, response)
}

func newErrorResponse(code, message, requestID string, retryable bool) protocol.ErrorResponse {
	return protocol.ErrorResponse{Error: protocol.ErrorDetail{
		Code:      protocol.ErrorCode(code),
		Message:   message,
		RequestID: requestID,
		Retryable: retryable,
	}}
}

func requestIDFromContext(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDContextKey{}).(string)
	return requestID
}
