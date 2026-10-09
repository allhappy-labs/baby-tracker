package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"babytracker/server/internal/auth"
	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/testpg"
)

func TestRecoveryMaintenanceFunctionReadiness(t *testing.T) {
	fixture := testpg.New(t)
	check := recoveryFunctionReadiness{pool: fixture.Maintenance}
	if err := check.Check(t.Context()); err != nil {
		t.Fatalf("installed maintenance function: %v", err)
	}
	options := httpapi.Options{
		InstanceID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", RecoveryEpoch: 7,
		CanonicalOrigin: "https://sync.example.test", AppleAuthorizationMode: "identity_token_only",
		Capabilities: []string{"identity-v1", "refresh-recovery-v1"}, RecoveryEnabled: true,
		RecoveryService: &auth.Service{}, RecoveryReadiness: check,
		Readiness: readinessFunc(func(context.Context) error { return nil }),
	}
	assertGate := func(want bool) {
		t.Helper()
		r := httptest.NewRecorder()
		httpapi.New(options).ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/v1/server", nil))
		if r.Code != 200 {
			t.Fatalf("server status=%d body=%s", r.Code, r.Body.String())
		}
		count := strings.Count(r.Body.String(), `"refresh-recovery-v1"`)
		if want && count != 1 || !want && count != 0 {
			t.Fatalf("capability count=%d want=%v body=%s", count, want, r.Body.String())
		}
		if !want {
			r = httptest.NewRecorder()
			httpapi.New(options).ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/auth/refresh-recoverable", strings.NewReader(`{}`)))
			if r.Code != 503 {
				t.Fatalf("recovery status=%d body=%s", r.Code, r.Body.String())
			}
		}
	}
	assertGate(true)
	const signature = "app.cleanup_refresh_recovery_receipts(timestamptz)"
	if _, err := fixture.Owner.Exec(t.Context(), "REVOKE EXECUTE ON FUNCTION "+signature+" FROM baby_tracker_maintenance"); err != nil {
		t.Fatal(err)
	}
	if err := check.Check(t.Context()); err == nil {
		t.Fatal("revoked maintenance EXECUTE accepted")
	}
	assertGate(false)
	if _, err := fixture.Owner.Exec(t.Context(), "GRANT EXECUTE ON FUNCTION "+signature+" TO baby_tracker_maintenance"); err != nil {
		t.Fatal(err)
	}
	if err := check.Check(t.Context()); err != nil {
		t.Fatalf("restored execute: %v", err)
	}
	if _, err := fixture.Owner.Exec(t.Context(), "REVOKE USAGE ON SCHEMA app FROM baby_tracker_maintenance"); err != nil {
		t.Fatal(err)
	}
	if err := check.Check(t.Context()); err == nil {
		t.Fatal("revoked schema USAGE accepted")
	}
	assertGate(false)
	if _, err := fixture.Owner.Exec(t.Context(), "GRANT USAGE ON SCHEMA app TO baby_tracker_maintenance"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Owner.Exec(t.Context(), "DROP FUNCTION "+signature); err != nil {
		t.Fatal(err)
	}
	if err := check.Check(t.Context()); err == nil {
		t.Fatal("missing maintenance function accepted")
	}
	assertGate(false)
}
