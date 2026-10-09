package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/family"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/security"
	"github.com/jackc/pgx/v5/pgconn"
)

// Every 5xx the shared writers send leaves exactly one line: route, code, a
// bounded cause that names the failure without repeating provider or client
// text, and whether the client was still there. A request the client had
// already abandoned is not a server fault and is not logged as one.
func TestServerFaultsLogOnceWithCauseAndClientGone(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	options := Options{Logger: logger, InstanceID: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", RecoveryEpoch: 1}
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
	request := func(ctx context.Context) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/sync", nil).WithContext(ctx)
		r.Pattern = "POST /v1/sync"
		return r
	}

	// A genuine failure, with its Postgres cause, at ERROR.
	cause := fmt.Errorf("%w: %w", security.ErrUnavailable, &pgconn.PgError{Code: "57P01", Message: "terminating connection due to administrator command"})
	recorder := httptest.NewRecorder()
	writeSyncFamilyError(recorder, request(context.Background()), options, cause)
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), `"code":"service_unavailable"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	line := oneLine(t)
	if line["level"] != "ERROR" || line["msg"] != "request failed" || line["route"] != "/v1/sync" || line["code"] != "service_unavailable" || line["client_gone"] != false {
		t.Fatalf("fault line=%v", line)
	}
	if c, _ := line["cause"].(string); !strings.Contains(c, "57P01") || !strings.Contains(c, "terminating connection") {
		t.Fatalf("cause lost: %v", line)
	}

	// The same failure after the client left: the response goes nowhere, and
	// the line says so below ERROR.
	logs.Reset()
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	recorder = httptest.NewRecorder()
	writeSyncFamilyError(recorder, request(gone), options, fmt.Errorf("%w: %w", security.ErrUnavailable, context.Canceled))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), `"code":"service_unavailable"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	line = oneLine(t)
	if line["level"] != "INFO" || line["msg"] != "request abandoned by client" || line["client_gone"] != true || line["code"] != "service_unavailable" {
		t.Fatalf("abandoned line=%v", line)
	}
	if c, _ := line["cause"].(string); !strings.Contains(c, "context canceled") {
		t.Fatalf("cause lost: %v", line)
	}

	// A client's own refusal is not a fault.
	logs.Reset()
	writeSyncFamilyError(httptest.NewRecorder(), request(context.Background()), options, family.ErrMembershipRevoked)
	if logs.Len() != 0 {
		t.Fatalf("a 403 was logged as a fault: %s", logs.String())
	}

	// Writers without options find the logger the middleware put on the
	// request; text of unknown origin is named by type, never repeated.
	logs.Reset()
	r := httptest.NewRequest(http.MethodPost, "/v1/auth/refresh", nil).WithContext(contextWithLogger(context.Background(), logger))
	r.Pattern = "POST /v1/auth/refresh"
	recorder = httptest.NewRecorder()
	writeAuthError(recorder, r, fmt.Errorf("%w: %w", auth.ErrUnavailable, errors.New("provider-private-text")))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", recorder.Code)
	}
	line = oneLine(t)
	if line["level"] != "ERROR" || line["route"] != "/v1/auth/refresh" || strings.Contains(logs.String(), "provider-private-text") {
		t.Fatalf("auth fault line=%v", line)
	}
	if c, _ := line["cause"].(string); !strings.Contains(c, "errors.errorString") {
		t.Fatalf("cause not named by type: %v", line)
	}
}

// A Postgres-backed limiter that fails is a 503 like any other: the single
// fault line must carry its cause, not a bare service_unavailable.
func TestSyncLimiterFailureLogsItsCause(t *testing.T) {
	p := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001", Scope: "family", SessionScope: "family", SessionFamilyID: "30000000-0000-4000-8000-000000000001"}
	options := familyHandlerOptions(t, p, familyServiceStub{current: func(context.Context, access.Principal) (family.FamilyView, error) {
		return family.FamilyView{ID: "30000000-0000-4000-8000-000000000001"}, nil
	}}, nil)
	effective := protocol.DefaultLimits()
	options.SyncLimits = &effective
	options.InstanceID, options.RecoveryEpoch = "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", 1
	var logs bytes.Buffer
	options.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	options.SyncService = syncServiceStub{apply: func(context.Context, access.Principal, protocol.SyncRequest) (protocol.SyncResponse, error) {
		t.Fatal("service called after a limiter failure")
		return protocol.SyncResponse{}, nil
	}}
	options.Limiter = limiterStub{takeMany: func(context.Context, []limits.Charge) (limits.Decision, error) {
		return limits.Decision{}, fmt.Errorf("limiter: %w", &pgconn.PgError{Code: "57P01", Message: "terminating connection due to administrator command"})
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/sync", strings.NewReader(`{"protocol_version":"v1","capabilities":["records-v1"],"cursor":null,"mutations":[],"page_token":null}`))
	req.Header.Set("Authorization", "Bearer access")
	req.Header.Set("X-Baby-Tracker-Protocol", "v1")
	req.Header.Set("X-Baby-Tracker-Capabilities", "records-v1")
	rec := httptest.NewRecorder()
	New(options).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"code":"service_unavailable"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var faults []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatal(err)
		}
		if line["msg"] != "request completed" {
			faults = append(faults, line)
		}
	}
	if len(faults) != 1 || faults[0]["level"] != "ERROR" || faults[0]["route"] != "/v1/sync" || faults[0]["code"] != "service_unavailable" {
		t.Fatalf("fault lines=%v", faults)
	}
	if c, _ := faults[0]["cause"].(string); !strings.Contains(c, "sqlstate 57P01") {
		t.Fatalf("limiter cause lost: %v", faults[0])
	}
}

// A data exception (SQLSTATE class 22) quotes the offending value, which may
// be the client's own input: only the code is logged for that class.
func TestDescribeCauseOmitsDataExceptionText(t *testing.T) {
	data := describeCause(fmt.Errorf("%w: %w", security.ErrUnavailable, &pgconn.PgError{Code: "22P02", Message: `invalid input syntax for type uuid: "client-value"`}))
	if data != "service_unavailable; sqlstate 22P02" {
		t.Fatalf("data exception described as %q", data)
	}
	if other := describeCause(&pgconn.PgError{Code: "57P01", Message: "terminating connection"}); other != "sqlstate 57P01 terminating connection" {
		t.Fatalf("other class described as %q", other)
	}
}

type syncServiceStub struct {
	apply    func(context.Context, access.Principal, protocol.SyncRequest) (protocol.SyncResponse, error)
	snapshot func(context.Context, access.Principal, protocol.SnapshotRequest) (protocol.SnapshotResponse, error)
}

func TestSnapshotHTTPUsesExactCapabilitiesQueryShapeAndTwoPullCharges(t *testing.T) {
	p := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001", Scope: "family", SessionScope: "family", SessionFamilyID: "30000000-0000-4000-8000-000000000001"}
	familyID := "30000000-0000-4000-8000-000000000001"
	var charged []limits.Charge
	options := familyHandlerOptions(t, p, familyServiceStub{current: func(context.Context, access.Principal) (family.FamilyView, error) {
		return family.FamilyView{ID: familyID}, nil
	}}, func(got []limits.Charge) { charged = append([]limits.Charge(nil), got...) })
	effective := protocol.DefaultLimits()
	options.SyncLimits = &effective
	options.SyncService = syncServiceStub{snapshot: func(_ context.Context, got access.Principal, request protocol.SnapshotRequest) (protocol.SnapshotResponse, error) {
		if got != p || request.PageToken == nil || *request.PageToken != "opaque" {
			t.Fatalf("principal=%+v request=%+v", got, request)
		}
		return protocol.SnapshotResponse{ServerInstanceID: "50000000-0000-4000-8000-000000000001", RecoveryEpoch: 1,
			SnapshotID: "60000000-0000-4000-8000-000000000001", HighWater: protocol.Cursor{ServerInstanceID: "50000000-0000-4000-8000-000000000001", RecoveryEpoch: 1, FamilyID: familyID}, Entities: []protocol.CanonicalEntity{}}, nil
	}}
	policies := limits.DefaultPolicies()
	options.SyncPullDevicePolicy, options.SyncPullAccountPolicy = policies[limits.ScopeSyncPullDevice], policies[limits.ScopeSyncPullAccount]
	req := httptest.NewRequest(http.MethodGet, "/v1/sync/snapshot?page_token=opaque", nil)
	req.Header.Set("Authorization", "Bearer access")
	req.Header.Set("X-Baby-Tracker-Protocol", "v1")
	req.Header.Set("X-Baby-Tracker-Capabilities", "records-v1,snapshot-v1")
	rec := httptest.NewRecorder()
	New(options).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || len(charged) != 2 || charged[0].Scope != limits.ScopeSyncPullDevice || charged[1].Scope != limits.ScopeSyncPullAccount {
		t.Fatalf("status=%d charges=%+v body=%s", rec.Code, charged, rec.Body.String())
	}
}

func TestSyncRateDenialLeavesServiceUncalled(t *testing.T) {
	p := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001", Scope: "family", SessionScope: "family", SessionFamilyID: "30000000-0000-4000-8000-000000000001"}
	options := familyHandlerOptions(t, p, familyServiceStub{current: func(context.Context, access.Principal) (family.FamilyView, error) {
		return family.FamilyView{ID: "30000000-0000-4000-8000-000000000001"}, nil
	}}, nil)
	effective := protocol.DefaultLimits()
	options.SyncLimits = &effective
	options.InstanceID, options.RecoveryEpoch = "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", 1
	options.SyncService = syncServiceStub{apply: func(context.Context, access.Principal, protocol.SyncRequest) (protocol.SyncResponse, error) {
		t.Fatal("service called after rate denial")
		return protocol.SyncResponse{}, nil
	}}
	options.Limiter = limiterStub{takeMany: func(context.Context, []limits.Charge) (limits.Decision, error) {
		return limits.Decision{Allowed: false, RetryAfter: time.Second}, nil
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/sync", strings.NewReader(`{"protocol_version":"v1","capabilities":["records-v1"],"cursor":null,"mutations":[],"page_token":null}`))
	req.Header.Set("Authorization", "Bearer access")
	req.Header.Set("X-Baby-Tracker-Protocol", "v1")
	req.Header.Set("X-Baby-Tracker-Capabilities", "records-v1")
	rec := httptest.NewRecorder()
	New(options).ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "1" || !strings.Contains(rec.Body.String(), `"server_instance_id":"`+options.InstanceID+`"`) {
		t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
}

func TestSyncErrorHandlersMatchFrozenFixtures(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "fixtures", "v1", "errors.json"))
	if err != nil {
		t.Fatal(err)
	}
	fixtures := map[string]protocol.ErrorResponse{}
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	options := Options{InstanceID: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", RecoveryEpoch: 1}
	statuses := map[string]int{"unsupported_protocol": 400, "unsupported_capability": 409, "snapshot_required": 409,
		"invalid_page_token": 400, "page_expired": 410, "epoch_changed": 401, "snapshot_limit": 409,
		"body_too_large": 413, "batch_limit": 400, "mutation_id_reused": 409,
		"family_scope_required": 403, "access_expired": 401, "reauthentication_required": 401, "service_unavailable": 503}
	for code, status := range statuses {
		t.Run(code, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeSyncCode(rec, httptest.NewRequest(http.MethodGet, "/", nil), options, code)
			var got protocol.ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != status || !reflect.DeepEqual(got, fixtures[code]) {
				t.Fatalf("status=%d got=%+v want=%+v err=%v", rec.Code, got, fixtures[code], err)
			}
		})
	}
	t.Run("rate_limited", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeSyncRateLimited(rec, httptest.NewRequest(http.MethodGet, "/", nil), options, limits.Decision{RetryAfter: time.Second})
		var got protocol.ErrorResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 429 || !reflect.DeepEqual(got, fixtures["rate_limited"]) {
			t.Fatalf("status=%d got=%+v want=%+v err=%v", rec.Code, got, fixtures["rate_limited"], err)
		}
	})
}

func (s syncServiceStub) ApplyAndPull(ctx context.Context, p access.Principal, r protocol.SyncRequest) (protocol.SyncResponse, error) {
	return s.apply(ctx, p, r)
}
func (s syncServiceStub) FullSnapshot(ctx context.Context, p access.Principal, r protocol.SnapshotRequest) (protocol.SnapshotResponse, error) {
	return s.snapshot(ctx, p, r)
}

func TestSyncHTTPChargesFiveScopesOnceAndAcceptsConfiguredBodyAboveOneMiB(t *testing.T) {
	p := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001", SessionID: "22000000-0000-4000-8000-000000000001", RecoveryEpoch: 1, SecurityRevision: 3, Scope: "family", SessionScope: "family", SessionFamilyID: "30000000-0000-4000-8000-000000000001"}
	familyID := "30000000-0000-4000-8000-000000000001"
	mutation := protocol.Mutation{MutationID: "01000000-0000-4000-8000-000000000001", EntityID: "10000000-0000-4000-8000-000000000001", EntityKind: "baby", Operation: "create", PayloadVersion: 1, ClientMutationTime: "2026-09-20T12:00:00Z", Payload: protocol.BabyPayload{Name: "Baby", BirthDate: "2026-01-01", SortOrder: 0}}
	request := protocol.SyncRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityRecordsV1}}, Mutations: []protocol.Mutation{mutation}}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, []byte(strings.Repeat(" ", 1<<20))...)
	var charged []limits.Charge
	options := familyHandlerOptions(t, p, familyServiceStub{current: func(context.Context, access.Principal) (family.FamilyView, error) {
		return family.FamilyView{ID: familyID}, nil
	}}, func(got []limits.Charge) { charged = append([]limits.Charge(nil), got...) })
	effective := protocol.DefaultLimits()
	effective.SyncBodyBytes = 2 << 20
	options.SyncLimits = &effective
	options.SyncService = syncServiceStub{apply: func(_ context.Context, got access.Principal, input protocol.SyncRequest) (protocol.SyncResponse, error) {
		if got != p || len(input.Mutations) != 1 {
			t.Fatalf("principal=%+v request=%+v", got, input)
		}
		return protocol.SyncResponse{ServerInstanceID: "50000000-0000-4000-8000-000000000001", RecoveryEpoch: 1, Outcomes: []protocol.Outcome{}, Changes: []protocol.JournalChange{}, HighWater: protocol.Cursor{ServerInstanceID: "50000000-0000-4000-8000-000000000001", RecoveryEpoch: 1, FamilyID: familyID}}, nil
	}}
	policies := limits.DefaultPolicies()
	options.SyncMutationDevicePolicy, options.SyncMutationAccountPolicy, options.SyncMutationFamilyPolicy = policies[limits.ScopeSyncMutationDevice], policies[limits.ScopeSyncMutationAccount], policies[limits.ScopeSyncMutationFamily]
	options.SyncPullDevicePolicy, options.SyncPullAccountPolicy = policies[limits.ScopeSyncPullDevice], policies[limits.ScopeSyncPullAccount]
	req := httptest.NewRequest(http.MethodPost, "/v1/sync", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer access")
	req.Header.Set("X-Baby-Tracker-Protocol", "v1")
	req.Header.Set("X-Baby-Tracker-Capabilities", "records-v1")
	rec := httptest.NewRecorder()
	New(options).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(charged) != 5 {
		t.Fatalf("charges=%+v", charged)
	}
	for _, charge := range charged {
		if charge.Cost != 1 {
			t.Fatalf("charge=%+v", charge)
		}
	}
}

func TestSyncHTTPRejectsOversizeAndInvalidContinuationBeforeCharging(t *testing.T) {
	p := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001", Scope: "family", SessionScope: "family", SessionFamilyID: "30000000-0000-4000-8000-000000000001"}
	familyID := "30000000-0000-4000-8000-000000000001"
	charged := 0
	options := familyHandlerOptions(t, p, familyServiceStub{current: func(context.Context, access.Principal) (family.FamilyView, error) {
		return family.FamilyView{ID: familyID}, nil
	}}, func([]limits.Charge) { charged++ })
	effective := protocol.DefaultLimits()
	effective.SyncBodyBytes = 65536
	options.SyncLimits = &effective
	options.SyncService = syncServiceStub{apply: func(context.Context, access.Principal, protocol.SyncRequest) (protocol.SyncResponse, error) {
		t.Fatal("service called")
		return protocol.SyncResponse{}, nil
	}}
	for _, test := range []struct {
		name, body string
		want       int
	}{
		{"oversize", strings.Repeat(" ", 65537), http.StatusRequestEntityTooLarge},
		{"invalid continuation", `{"protocol_version":"v1","capabilities":["records-v1"],"cursor":null,"mutations":[{"mutation_id":"01000000-0000-4000-8000-000000000001","entity_id":"10000000-0000-4000-8000-000000000001","entity_kind":"baby","operation":"create","payload_version":1,"observed_entity_version":0,"client_mutation_time":"2026-09-20T12:00:00Z","import_id":null,"payload":{"name":"Baby","birth_date":"2026-01-01","sort_order":0}}],"page_token":"token"}`, http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/sync", strings.NewReader(test.body))
			req.Header.Set("Authorization", "Bearer access")
			req.Header.Set("X-Baby-Tracker-Protocol", "v1")
			req.Header.Set("X-Baby-Tracker-Capabilities", "records-v1")
			rec := httptest.NewRecorder()
			New(options).ServeHTTP(rec, req)
			if rec.Code != test.want {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
	if charged != 0 {
		t.Fatalf("invalid requests charged=%d", charged)
	}
}

func TestSyncHTTPCompatibilityStatusCodes(t *testing.T) {
	p := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001", Scope: "family", SessionScope: "family", SessionFamilyID: "30000000-0000-4000-8000-000000000001"}
	options := familyHandlerOptions(t, p, familyServiceStub{}, func([]limits.Charge) { t.Fatal("compatibility failure charged") })
	effective := protocol.DefaultLimits()
	options.SyncLimits = &effective
	options.SyncService = syncServiceStub{}
	for _, test := range []struct {
		name, protocol, capabilities string
		want                         int
	}{
		{"missing protocol", "", "records-v1", http.StatusBadRequest},
		{"unsupported protocol", "v2", "records-v1", http.StatusBadRequest},
		{"missing capability", "v1", "", http.StatusConflict},
		{"unsupported capability", "v1", "future-v1", http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/sync", strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer access")
			if test.protocol != "" {
				req.Header.Set("X-Baby-Tracker-Protocol", test.protocol)
			}
			if test.capabilities != "" {
				req.Header.Set("X-Baby-Tracker-Capabilities", test.capabilities)
			}
			rec := httptest.NewRecorder()
			New(options).ServeHTTP(rec, req)
			if rec.Code != test.want {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestSyncHTTPTimerCapabilityIsSelectedFromDecodedMutationsBeforeAdmission(t *testing.T) {
	p := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001", Scope: "family", SessionScope: "family", SessionFamilyID: "30000000-0000-4000-8000-000000000001"}
	options := familyHandlerOptions(t, p, familyServiceStub{current: func(context.Context, access.Principal) (family.FamilyView, error) {
		t.Fatal("capability failure reached family resolution")
		return family.FamilyView{}, nil
	}}, func([]limits.Charge) { t.Fatal("capability failure charged") })
	effective := protocol.DefaultLimits()
	options.SyncLimits, options.SyncService = &effective, syncServiceStub{}
	timer := `{"mutation_id":"01000000-0000-4000-8000-000000000008","entity_id":"50000000-0000-4000-8000-000000000001","entity_kind":"timer","operation":"timer_start","payload_version":5,"observed_entity_version":0,"client_mutation_time":"2026-09-20T15:00:00Z","import_id":null,"payload":{"baby_id":"10000000-0000-4000-8000-000000000001","timer_kind":"feed","started_at":"2026-09-20T15:00:00Z","time_zone_id":"Europe/Zurich","completed_components":[],"current_side":"left","current_side_started_at":"2026-09-20T15:00:00Z","session_id":null}}`
	for _, test := range []struct {
		name, capabilities, mutations string
	}{
		{"timer mutation lacks timer capability", "records-v1", `[` + timer + `]`},
		{"ordinary request overclaims timer capability", "records-v1,timer-commands-v1", `[]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := `{"protocol_version":"v1","capabilities":["` + strings.ReplaceAll(test.capabilities, ",", `","`) + `"],"cursor":null,"mutations":` + test.mutations + `,"page_token":null}`
			req := httptest.NewRequest(http.MethodPost, "/v1/sync", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer access")
			req.Header.Set("X-Baby-Tracker-Protocol", "v1")
			req.Header.Set("X-Baby-Tracker-Capabilities", test.capabilities)
			rec := httptest.NewRecorder()
			New(options).ServeHTTP(rec, req)
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"unsupported_capability"`) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestSyncHTTPClassifiesBodyNegotiationBeforeCharging(t *testing.T) {
	p := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001", Scope: "family", SessionScope: "family", SessionFamilyID: "30000000-0000-4000-8000-000000000001"}
	options := familyHandlerOptions(t, p, familyServiceStub{}, func([]limits.Charge) { t.Fatal("negotiation failure charged") })
	effective := protocol.DefaultLimits()
	options.SyncLimits = &effective
	options.SyncService = syncServiceStub{}
	for _, test := range []struct {
		name, body, code string
		want             int
	}{
		{"missing version", `{"capabilities":["records-v1"],"cursor":null,"mutations":[],"page_token":null}`, "unsupported_protocol", 400},
		{"mismatching version", `{"protocol_version":"v2","capabilities":["records-v1"],"cursor":null,"mutations":[],"page_token":null}`, "unsupported_protocol", 400},
		{"missing capabilities", `{"protocol_version":"v1","cursor":null,"mutations":[],"page_token":null}`, "unsupported_capability", 409},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/sync", strings.NewReader(test.body))
			req.Header.Set("Authorization", "Bearer access")
			req.Header.Set("X-Baby-Tracker-Protocol", "v1")
			req.Header.Set("X-Baby-Tracker-Capabilities", "records-v1")
			rec := httptest.NewRecorder()
			New(options).ServeHTTP(rec, req)
			if rec.Code != test.want || !strings.Contains(rec.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestSyncHTTPRejectsNonFamilyScopesBeforeAdmission(t *testing.T) {
	for _, route := range []struct {
		method, path, capabilities, body string
	}{
		{http.MethodPost, "/v1/sync", "records-v1", `{"protocol_version":"v1","capabilities":["records-v1"],"cursor":null,"mutations":[],"page_token":null}`},
		{http.MethodGet, "/v1/sync/snapshot", "records-v1,snapshot-v1", ""},
	} {
		for _, scope := range []string{"account", "family_recovery"} {
			t.Run(route.method+"_"+scope, func(t *testing.T) {
				p := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001", Scope: scope}
				options := familyHandlerOptions(t, p, familyServiceStub{}, func([]limits.Charge) { t.Fatal("non-family scope charged") })
				effective := protocol.DefaultLimits()
				options.SyncLimits = &effective
				options.SyncService = syncServiceStub{
					apply: func(context.Context, access.Principal, protocol.SyncRequest) (protocol.SyncResponse, error) {
						t.Fatal("sync service called")
						return protocol.SyncResponse{}, nil
					},
					snapshot: func(context.Context, access.Principal, protocol.SnapshotRequest) (protocol.SnapshotResponse, error) {
						t.Fatal("snapshot service called")
						return protocol.SnapshotResponse{}, nil
					},
				}
				options.InstanceID, options.RecoveryEpoch = "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", 1
				req := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
				req.Header.Set("Authorization", "Bearer access")
				req.Header.Set("X-Baby-Tracker-Protocol", "v1")
				req.Header.Set("X-Baby-Tracker-Capabilities", route.capabilities)
				rec := httptest.NewRecorder()
				New(options).ServeHTTP(rec, req)
				if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"code":"family_scope_required"`) ||
					!strings.Contains(rec.Body.String(), `"server_instance_id":"`+options.InstanceID+`"`) {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
			})
		}
	}
}

func TestSyncHTTPRequiresPersistedFamilySessionBeforeAdmission(t *testing.T) {
	for _, route := range []struct {
		method, path, capabilities, body string
	}{
		{http.MethodPost, "/v1/sync", "records-v1", `{"protocol_version":"v1","capabilities":["records-v1"],"cursor":null,"mutations":[],"page_token":null}`},
		{http.MethodGet, "/v1/sync/snapshot", "records-v1,snapshot-v1", ""},
	} {
		t.Run(route.method, func(t *testing.T) {
			p := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001", Scope: "family", SessionScope: "account"}
			options := familyHandlerOptions(t, p, familyServiceStub{}, func([]limits.Charge) { t.Fatal("unrefreshed family session charged") })
			effective := protocol.DefaultLimits()
			options.SyncLimits = &effective
			options.SyncService = syncServiceStub{
				apply: func(context.Context, access.Principal, protocol.SyncRequest) (protocol.SyncResponse, error) {
					t.Fatal("sync service called")
					return protocol.SyncResponse{}, nil
				},
				snapshot: func(context.Context, access.Principal, protocol.SnapshotRequest) (protocol.SnapshotResponse, error) {
					t.Fatal("snapshot service called")
					return protocol.SnapshotResponse{}, nil
				},
			}
			options.InstanceID, options.RecoveryEpoch = "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", 1
			req := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
			req.Header.Set("Authorization", "Bearer access")
			req.Header.Set("X-Baby-Tracker-Protocol", "v1")
			req.Header.Set("X-Baby-Tracker-Capabilities", route.capabilities)
			rec := httptest.NewRecorder()
			New(options).ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != "Bearer" ||
				!strings.Contains(rec.Body.String(), `"code":"access_expired"`) ||
				!strings.Contains(rec.Body.String(), `"server_instance_id":"`+options.InstanceID+`"`) {
				t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
			}
		})
	}
}
