package httpapi

import (
	"context"
	"net/http"
	"sync"

	"babytracker/server/internal/admission"
	"babytracker/server/internal/diagnostics"
)

type admissionReleaseKey struct{}

// WithAdmission wraps one route before readiness, authentication and body work
// (a route that reads its body before its credential takes its slot itself,
// after the body: admitAfterBody).
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
// reports false. The returned release is idempotent.
func acquireAdmission(w http.ResponseWriter, r *http.Request, controller *admission.Controller, class admission.Class,
	metrics *diagnostics.CapacityCollector) (func(), bool) {
	metricClass := diagnostics.SyncAdmission
	if class == admission.MCP {
		metricClass = diagnostics.MCPAdmission
	}
	release, ok := controller.TryAcquire(class)
	if !ok {
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

// writeServerBusy is the refusal of a request the node has no slot for.
func writeServerBusy(w http.ResponseWriter, r *http.Request) {
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
