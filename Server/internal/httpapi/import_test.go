package httpapi

import (
	"babytracker/server/internal/access"
	"babytracker/server/internal/family"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type importServiceStub struct{ calls int }

func (s *importServiceStub) Resolve(context.Context, access.Principal, protocol.ImportResolveRequest) (protocol.ImportResolveResponse, error) {
	s.calls++
	return protocol.ImportResolveResponse{}, nil
}
func (s *importServiceStub) StatusPage(context.Context, access.Principal, string, *string) (protocol.ImportResolveResponse, error) {
	s.calls++
	return protocol.ImportResolveResponse{}, nil
}
func (s *importServiceStub) ResolveConflict(context.Context, access.Principal, string, protocol.ImportConflictRequest) (protocol.ImportResolveResponse, error) {
	s.calls++
	return protocol.ImportResolveResponse{}, nil
}
func TestImportHTTPStrictNegotiationAndStaleSessionBeforeRate(t *testing.T) {
	for _, tc := range []struct {
		name, path, caps string
		stale            bool
		status           int
	}{
		{"valid", "/v1/family/import/10000000-0000-4000-8000-000000000001", "records-v1,import-resolver-v1", false, 200},
		{"extra query", "/v1/family/import/10000000-0000-4000-8000-000000000001?extra=1", "records-v1,import-resolver-v1", false, 400},
		{"invalid escape", "/v1/family/import/10000000-0000-4000-8000-000000000001?page_token=%zz", "records-v1,import-resolver-v1", false, 400},
		{"missing capability", "/v1/family/import/10000000-0000-4000-8000-000000000001", "import-resolver-v1", false, 409},
		{"stale session", "/v1/family/import/10000000-0000-4000-8000-000000000001", "records-v1,import-resolver-v1", true, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001", Scope: "family", SessionScope: "family", SessionFamilyID: "30000000-0000-4000-8000-000000000001"}
			calls := 0
			opts := familyHandlerOptions(t, p, familyServiceStub{current: func(context.Context, access.Principal) (family.FamilyView, error) {
				id := p.SessionFamilyID
				if tc.stale {
					id = "30000000-0000-4000-8000-000000000002"
				}
				return family.FamilyView{ID: id}, nil
			}}, func([]limits.Charge) { calls++ })
			l := protocol.DefaultLimits()
			opts.SyncLimits = &l
			stub := &importServiceStub{}
			opts.ImportService = stub
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Authorization", "Bearer access")
			req.Header.Set("X-Baby-Tracker-Protocol", "v1")
			req.Header.Set("X-Baby-Tracker-Capabilities", tc.caps)
			w := httptest.NewRecorder()
			New(opts).ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if tc.status != 200 && (calls != 0 || stub.calls != 0) {
				t.Fatal("invalid request charged or called importer")
			}
			if !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
				t.Fatal("cacheable import")
			}
		})
	}
}
