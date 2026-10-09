package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/access"
	familyexport "babytracker/server/internal/export"
	"babytracker/server/internal/family"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5/pgconn"
)

type exportServiceStub struct {
	created int
}

func TestExportCapabilityRequiresConfiguredService(t *testing.T) {
	if slices.Contains(advertisedCapabilities(Options{}), protocol.CapabilityExportV1) {
		t.Fatal("disabled export was advertised")
	}
	if !slices.Contains(advertisedCapabilities(Options{ExportService: &exportServiceStub{}}), protocol.CapabilityExportV1) {
		t.Fatal("configured export was hidden")
	}
}

func (stub *exportServiceStub) Create(_ context.Context, _ access.Principal, request protocol.CreateExportRequest) (protocol.ExportStatus, bool, error) {
	stub.created++
	return protocol.ExportStatus{ProtocolVersion: "v1", ExportID: request.RequestID, State: "queued", CreatedAt: "2026-09-27T12:00:00.000000Z", ExpiresAt: "2026-09-28T12:00:00.000000Z"}, true, nil
}
func (*exportServiceStub) Status(context.Context, access.Principal, string) (protocol.ExportStatus, error) {
	return protocol.ExportStatus{}, nil
}
func (*exportServiceStub) Download(context.Context, access.Principal, string, io.Writer) error {
	return nil
}

func TestExportCreateRequiresExactCapabilityAndFamilySession(t *testing.T) {
	familyID := "30000000-0000-4000-8000-000000000001"
	p := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001",
		Scope: "family", SessionScope: "family", SessionFamilyID: familyID}
	service := &exportServiceStub{}
	charged := 0
	options := familyHandlerOptions(t, p, familyServiceStub{}, func(charges []limits.Charge) {
		charged++
		if len(charges) != 3 || charges[0].Scope != limits.ScopeExportAccount || charges[1].Scope != limits.ScopeExportDevice || charges[2].Scope != limits.ScopeExportIP {
			t.Fatalf("export charges=%+v", charges)
		}
	})
	policies := limits.DefaultPolicies()
	options.ExportIPPolicy = policies[limits.ScopeExportIP]
	options.ExportAccountPolicy = policies[limits.ScopeExportAccount]
	options.ExportDevicePolicy = policies[limits.ScopeExportDevice]
	options.ExportService = service
	options.InstanceID = "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"
	options.RecoveryEpoch = 7
	body := `{"protocol_version":"v1","capabilities":["export-v1"],"request_id":"10000000-0000-4000-8000-000000000001"}`
	for _, tc := range []struct {
		name, caps string
		want       int
	}{
		{"valid", "export-v1", http.StatusAccepted},
		{"missing", "", http.StatusConflict},
		{"wrong", "records-v1", http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/export", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer access")
			r.Header.Set("X-Baby-Tracker-Protocol", "v1")
			if tc.caps != "" {
				r.Header.Set("X-Baby-Tracker-Capabilities", tc.caps)
			}
			w := httptest.NewRecorder()
			New(options).ServeHTTP(w, r)
			if w.Code != tc.want || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d cache=%q body=%s", w.Code, w.Header().Get("Cache-Control"), w.Body.String())
			}
		})
	}
	if service.created != 1 {
		t.Fatalf("create calls=%d", service.created)
	}
	if charged != 1 {
		t.Fatalf("rate-limit charges=%d", charged)
	}
	p.Scope = "account"
	options = familyHandlerOptions(t, p, familyServiceStub{}, nil)
	options.ExportService = service
	r := httptest.NewRequest(http.MethodPost, "/v1/export", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer access")
	r.Header.Set("X-Baby-Tracker-Protocol", "v1")
	r.Header.Set("X-Baby-Tracker-Capabilities", "export-v1")
	w := httptest.NewRecorder()
	New(options).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || service.created != 1 {
		t.Fatalf("account session status=%d calls=%d", w.Code, service.created)
	}
}

// Export 503s go through the shared fault line like every other writer's.
func TestExportFaultsLogOnceWithCause(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	options := Options{Logger: logger, InstanceID: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", RecoveryEpoch: 1}
	request := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/family/export", nil).WithContext(contextWithLogger(context.Background(), logger))
		r.Pattern = "POST /v1/family/export"
		return r
	}
	oneLine := func(t *testing.T) map[string]any {
		t.Helper()
		lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
		if len(lines) != 1 || lines[0] == "" {
			t.Fatalf("expected one log line, got %q", logs.String())
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(lines[0]), &line); err != nil {
			t.Fatal(err)
		}
		return line
	}
	rec := httptest.NewRecorder()
	writeExportError(rec, request(), options, fmt.Errorf("%w: %w", familyexport.ErrUnavailable, &pgconn.PgError{Code: "57P01", Message: "terminating connection"}))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	line := oneLine(t)
	if c, _ := line["cause"].(string); line["level"] != "ERROR" || line["msg"] != "request failed" || line["route"] != "/v1/family/export" || line["code"] != "service_unavailable" || !strings.Contains(c, "sqlstate 57P01") {
		t.Fatalf("export fault line=%v", line)
	}
	// A 503 from configuration (no export service, no limiter) has no cause
	// but still leaves its line.
	logs.Reset()
	writeExportCode(httptest.NewRecorder(), request(), options, "service_unavailable")
	if line = oneLine(t); line["level"] != "ERROR" || line["code"] != "service_unavailable" {
		t.Fatalf("configuration fault line=%v", line)
	}
	logs.Reset()
	writeExportError(httptest.NewRecorder(), request(), options, familyexport.ErrNotFound)
	if logs.Len() != 0 {
		t.Fatalf("a 404 was logged as a fault: %s", logs.String())
	}
}

func TestExportOwnerDenialMapsToForbidden(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/export/10000000-0000-4000-8000-000000000001", nil)
	writeExportError(w, r, Options{}, family.ErrOwnerRequired)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"code":"owner_required"`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

type exportFamilyResolver struct{ service *family.Service }

func (resolver exportFamilyResolver) Current(ctx context.Context, principal access.Principal) (string, error) {
	view, err := resolver.service.Current(ctx, principal)
	return view.ID, err
}

func TestExportRoutesCreateAndDownloadRealArchive(t *testing.T) {
	h := testkit.New(t)
	head, err := h.Security.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Migrator.Exec(t.Context(), `SELECT app.update_server_state($1,$2,$3,$4,$5)`,
		h.Info.InstanceID, h.Info.RecoveryEpoch, head.Generation, head.Revision, head.EventHash[:]); err != nil {
		t.Fatal(err)
	}
	var key [32]byte
	for index := range key {
		key[index] = 0x5a
	}
	service, worker, err := familyexport.New(familyexport.Options{Security: h.Security, Resolver: exportFamilyResolver{h.Family},
		Identity: h.Info, Runtime: h.Runtime, WorkerPool: h.Maintenance,
		Keys: map[string][32]byte{"test.v1": key}, ActiveKeyID: "test.v1", Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	options := familyHandlerOptions(t, h.Owner, h.Family, nil)
	options.ExportService = service
	options.InstanceID = h.Info.InstanceID
	options.RecoveryEpoch = h.Info.RecoveryEpoch
	policies := limits.DefaultPolicies()
	options.ExportIPPolicy = policies[limits.ScopeExportIP]
	options.ExportAccountPolicy = policies[limits.ScopeExportAccount]
	options.ExportDevicePolicy = policies[limits.ScopeExportDevice]
	router := New(options)
	request := func(method, path string, body io.Reader) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, body)
		r.Header.Set("Authorization", "Bearer access")
		r.Header.Set("X-Baby-Tracker-Protocol", "v1")
		r.Header.Set("X-Baby-Tracker-Capabilities", "export-v1")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	created := request(http.MethodPost, "/v1/export", strings.NewReader(`{"protocol_version":"v1","capabilities":["export-v1"],"request_id":"10000000-0000-4000-8000-000000000001"}`))
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var status protocol.ExportStatus
	if err := json.Unmarshal(created.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	var ready *httptest.ResponseRecorder
	var readyStatus protocol.ExportStatus
	for attempt := 0; attempt < 20; attempt++ {
		if err := worker.RunOnce(t.Context(), 1); err != nil {
			t.Fatal(err)
		}
		ready = request(http.MethodGet, "/v1/export/"+status.ExportID, nil)
		if ready.Code != http.StatusOK {
			t.Fatalf("ready status=%d body=%s", ready.Code, ready.Body.String())
		}
		if err := json.Unmarshal(ready.Body.Bytes(), &readyStatus); err != nil {
			t.Fatal(err)
		}
		if readyStatus.State == "ready" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if readyStatus.State != "ready" {
		t.Fatalf("export remained %s: %s", readyStatus.State, ready.Body.String())
	}
	download := request(http.MethodGet, "/v1/export/"+status.ExportID+"/download", nil)
	if download.Code != http.StatusOK || download.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("download status=%d type=%q body=%s", download.Code, download.Header().Get("Content-Type"), download.Body.String())
	}
	var archive familyexport.ArchiveV1
	if err := json.NewDecoder(bytes.NewReader(download.Body.Bytes())).Decode(&archive); err != nil {
		t.Fatal(err)
	}
	if archive.FamilyID != h.FamilyID {
		t.Fatalf("exported family=%s want=%s", archive.FamilyID, h.FamilyID)
	}
}
