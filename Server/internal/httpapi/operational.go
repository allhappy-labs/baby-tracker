package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"babytracker/server/internal/protocol"
)

// errInvalidRecoveryEpoch is the cause of a not_ready from a node whose
// recovery epoch is not yet valid.
var errInvalidRecoveryEpoch = errors.New("recovery_epoch_invalid")

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, protocol.StatusResponse{Status: "ok"})
}

func readyHandler(readiness Readiness) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		var err error = errNoReadiness
		if readiness != nil {
			err = readiness.Check(ctx)
		}
		if err != nil {
			writeNotReady(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, protocol.StatusResponse{Status: "ok"})
	}
}

// errNoReadiness is the cause of a not_ready from a node built without a
// readiness check.
var errNoReadiness = errors.New("readiness_unconfigured")

// writeNotReady is the not_ready refusal, with its spaced fault line
// (logRefusal) carrying cause.
func writeNotReady(w http.ResponseWriter, r *http.Request, cause error) {
	logRefusal(r, refusalNotReady, http.StatusServiceUnavailable, cause)
	writeError(w, http.StatusServiceUnavailable, "not_ready", "Service is not ready", requestIDFromContext(r.Context()), true)
}

func serverHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		instanceID, recoveryEpoch := options.InstanceID, options.RecoveryEpoch
		if options.ServerState != nil {
			var err error
			instanceID, recoveryEpoch, err = options.ServerState.ServerState(request.Context())
			if err != nil {
				writeNotReady(w, request, err)
				return
			}
		}
		if !protocol.ValidRecoveryEpoch(recoveryEpoch) {
			writeNotReady(w, request, errInvalidRecoveryEpoch)
			return
		}
		// refresh-recovery-v1 is advertised from configuration alone, never
		// withdrawn while the node is not ready: an iOS client awaiting a
		// recovery acknowledgement treats its absence as terminal and signs
		// the parent out, while the recovery routes already refuse with a
		// retryable 503 until the node is ready.
		capabilityOptions := options
		// live-activity-push-v1 is advertised only while registration is
		// served: the worker (its cheap local state first) and the node are
		// ready (main's rule).
		if liveActivityConfigured(options) && (options.LiveActivityReady(request.Context()) != nil || options.Readiness == nil || options.Readiness.Check(request.Context()) != nil) {
			capabilityOptions.LiveActivityService = nil
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
