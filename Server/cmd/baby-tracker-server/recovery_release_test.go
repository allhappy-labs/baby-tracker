//go:build !identitytest

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReleaseRecoveryFaultSurfaceExcluded(t *testing.T) {
	t.Setenv("BT_IDENTITYTEST_RECOVERY_FAULT", "after_authority_commit")
	reached := 0
	h := wrapIdentityTestHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached++; w.WriteHeader(418) }), "synthetic")
	for _, path := range []string{"/__identitytest/recovery-fault", "/__identitytest/proof", "/v1/auth/refresh-recoverable"} {
		r := httptest.NewRequest("POST", path, nil)
		r.Header.Set("X-Identitytest-Recovery-Fault", "after_authority_commit")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 418 {
			t.Fatalf("release intercepts fault surface: %s", path)
		}
	}
	if reached != 3 {
		t.Fatal("release wrapper registered a route")
	}
}
