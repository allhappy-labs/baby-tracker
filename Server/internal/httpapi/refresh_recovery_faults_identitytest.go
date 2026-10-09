//go:build identitytest

package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"babytracker/server/internal/protocol"
	"babytracker/server/internal/security"
)

// RecoveryFaultHandler never bypasses the real route's readiness, strict
// decoder, proof validation, quotas, fences, authority CAS or projection.
func RecoveryFaultHandler(next http.Handler, fail func(string, string) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		point := r.Header.Get("X-Identitytest-Recovery-Fault")
		if r.Method != http.MethodPost || r.URL.Path != "/v1/auth/refresh-recoverable" || point == "" {
			next.ServeHTTP(w, r)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, authBodyBytes+1))
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var input protocol.RecoverableRefreshRequest
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err == nil && int64(len(raw)) <= authBodyBytes && decoder.Decode(&input) == nil && decoder.Decode(new(any)) == io.EOF && input.ProtocolVersion == "v1" && protocol.ValidateRefreshRecoveryRequest(input) == nil {
			r = r.WithContext(security.WithRecoveryFault(r.Context(), func(at string) error {
				if at != string(point) {
					return nil
				}
				return fail(point, input.RefreshAttemptID)
			}))
		}
		next.ServeHTTP(w, r)
	})
}
