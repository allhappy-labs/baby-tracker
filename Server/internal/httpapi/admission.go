package httpapi

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"babytracker/server/internal/admission"
	"babytracker/server/internal/diagnostics"
)

type admissionReleaseKey struct{}

// WithAdmission wraps one route before readiness, authentication and body work
// (a route that reads its body before its credential takes its slot itself,
// after the body: admitAfterBody).
// A route that runs before a caller is identified takes class Auth, the
// lane alongside the shared total (authLaneRoutes; the refresh routes choose
// per request, refreshAdmission).
// Register a future MCP route with class MCP using the same Controller as Sync;
// do not additionally wrap that route with Sync admission. No context watcher
// releases tokens: cancellation does not prove the handler has stopped working.
func WithAdmission(controller *admission.Controller, class admission.Class, metrics *diagnostics.CapacityCollector, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		finish, ok := acquireAdmission(w, r, controller, class, metrics)
		if !ok {
			return
		}
		defer finish()
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), admissionReleaseKey{}, finish)))
	})
}

// acquireAdmission takes one slot of class, or writes 503 server_busy and
// reports false. A controller with a wait (BT_ADMISSION_WAIT) lets a
// request that finds no slot wait up to it, until its context ends; the
// wait is counted, and a request still without a slot is refused as one
// refused at once. A request whose context ends while it waits is not a
// refusal: it is answered as any request whose client has gone (a retryable
// 503 service_unavailable whose fault line has cause_class client_gone, at
// INFO; timed_out for a deadline) and counted canceled, not rejected. The
// returned release is idempotent.
func acquireAdmission(w http.ResponseWriter, r *http.Request, controller *admission.Controller, class admission.Class,
	metrics *diagnostics.CapacityCollector) (func(), bool) {
	metricClass := diagnostics.SyncAdmission
	switch class {
	case admission.MCP:
		metricClass = diagnostics.MCPAdmission
	case admission.Auth:
		metricClass = diagnostics.AuthAdmission
	}
	ticket, err := controller.Acquire(r.Context(), class)
	ended := err != nil && !errors.Is(err, admission.ErrBusy)
	if ticket.Queued && metrics != nil {
		metrics.ObserveAdmissionWait(metricClass, ticket.Waited, ended)
	}
	release := ticket.Release
	if ended {
		logServerFault(requestLogger(r, nil), r, http.StatusServiceUnavailable, "service_unavailable", err)
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "Service is unavailable", requestIDFromContext(r.Context()), true)
		return nil, false
	}
	if err != nil {
		if metrics != nil {
			metrics.ObserveAdmission(metricClass, diagnostics.AdmissionRejected)
		}
		writeServerBusy(w, r)
		return nil, false
	}
	if metrics != nil {
		metrics.ObserveAdmission(metricClass, diagnostics.AdmissionAccepted)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if metrics != nil {
				metrics.ObserveAdmission(metricClass, diagnostics.AdmissionReleased)
			}
			release()
		})
	}, true
}

// writeServerBusy is the refusal of a request the node has no slot for. Its
// fault line (logRefusal) is spaced: a refusal flood leaves one a second.
func writeServerBusy(w http.ResponseWriter, r *http.Request) {
	logRefusal(r, refusalServerBusy, http.StatusServiceUnavailable, nil)
	w.Header().Set("Retry-After", "1")
	w.Header().Set("Cache-Control", "no-store")
	writeRetryError(w, http.StatusServiceUnavailable, "server_busy", "Server is busy", requestIDFromContext(r.Context()), true, 1)
}

// Call only after realtime.Accept actually succeeds. Status 101 alone does not
// establish a successful hijack. Hub reservations retain socket lifetime limits.
func releaseUpgradeAdmission(ctx context.Context) {
	if release, ok := ctx.Value(admissionReleaseKey{}).(func()); ok {
		release()
	}
}
