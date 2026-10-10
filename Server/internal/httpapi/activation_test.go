package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"babytracker/server/internal/access"
	"babytracker/server/internal/testkit"
)

// A session issued before its account held a family (sign-in, then create
// or join) authenticates with the derived scope family over an account-scope
// session row. Its first sync and import must answer 401 access_expired, the
// one code the iOS client renews on, never 403 family_scope_required: the
// refresh is what persists the family scope (Task 11 ruling, legacy
// Principal.Scope derivation).
func TestFirstActivationAfterCreateOrJoinAsksForRefresh(t *testing.T) {
	for _, test := range []struct {
		name, subject string
		harness       func(*testing.T) *testkit.Harness
		principal     func(*testkit.Harness) access.Principal
	}{
		{"owner_create", "owner", testkit.NewWithUnrefreshedOwner, func(h *testkit.Harness) access.Principal { return h.Owner }},
		{"caregiver_join", "caregiver", testkit.NewWithUnrefreshedCaregiver, func(h *testkit.Harness) access.Principal { return h.Caregiver }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := test.harness(t)
			if p := test.principal(h); p.Scope != "family" || p.SessionScope != "account" || p.SessionFamilyID != "" {
				t.Fatalf("unrefreshed principal scope=%q session scope=%q session family=%q", p.Scope, p.SessionScope, p.SessionFamilyID)
			}
			options := Options{Readiness: readinessFunc(func(context.Context) error { return nil }), InstanceID: h.Info.InstanceID,
				RecoveryEpoch: h.Info.RecoveryEpoch, CanonicalOrigin: h.Info.CanonicalOrigin, SyncLimits: &h.Limits,
				SessionService: h.Auth, FamilyService: h.Family, SyncService: h.Service, ImportService: h.Importer, RequestAuthorizer: h.Authority,
				Limiter: liveAllowLimiter()}
			handler := New(options)
			for _, route := range []struct{ method, path, capabilities, body string }{
				{http.MethodPost, "/v1/sync", "records-v1", `{"protocol_version":"v1","capabilities":["records-v1"],"cursor":null,"mutations":[]}`},
				{http.MethodGet, "/v1/sync/snapshot", "records-v1,snapshot-v1", ""},
				{http.MethodPost, "/v1/family/import/resolve", "records-v1,import-resolver-v1", `{}`},
			} {
				request := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
				request.Header.Set("Authorization", "Bearer "+h.AccessToken(test.subject))
				request.Header.Set("X-Baby-Tracker-Protocol", "v1")
				request.Header.Set("X-Baby-Tracker-Capabilities", route.capabilities)
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, request)
				if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), `"code":"access_expired"`) {
					t.Fatalf("%s %s status=%d body=%s", route.method, route.path, recorder.Code, recorder.Body.String())
				}
			}
			// The refresh persists the family scope; the same routes then serve.
			refreshed := h.Refresh(test.subject)
			if refreshed.SessionScope != "family" || refreshed.SessionFamilyID != h.FamilyID {
				t.Fatalf("refreshed principal=%+v", refreshed)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/sync", strings.NewReader(`{"protocol_version":"v1","capabilities":["records-v1"],"cursor":null,"mutations":[]}`))
			request.Header.Set("Authorization", "Bearer "+h.AccessToken(test.subject))
			request.Header.Set("X-Baby-Tracker-Protocol", "v1")
			request.Header.Set("X-Baby-Tracker-Capabilities", "records-v1")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("after refresh status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}
