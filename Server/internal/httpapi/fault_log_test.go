package httpapi

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/family"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5/pgconn"
)

// terminated is the Postgres failure the fault tests inject: its line must
// name it by SQLSTATE.
func terminated() error {
	return &pgconn.PgError{Code: "57P01", Message: "terminating connection due to administrator command"}
}

// logLines is every JSON line of logged; errorLines those at ERROR.
func logLines(t *testing.T, logged string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(logged), "\n") {
		if raw == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("log line %q: %v", raw, err)
		}
		lines = append(lines, line)
	}
	return lines
}

func errorLines(t *testing.T, logged string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range logLines(t, logged) {
		if line["level"] == "ERROR" {
			out = append(out, line)
		}
	}
	return out
}

// oneFaultLine is the one ERROR line of logged, which must be the fault
// line for route and code with a cause containing cause.
func oneFaultLine(t *testing.T, logged, route, code, cause string) {
	t.Helper()
	lines := errorLines(t, logged)
	if len(lines) != 1 {
		t.Fatalf("%d ERROR lines, want 1: %s", len(lines), logged)
	}
	line := lines[0]
	got, _ := line["cause"].(string)
	if line["msg"] != "request failed" || line["route"] != route || line["code"] != code || !strings.Contains(got, cause) {
		t.Fatalf("fault line=%v, want route %s code %s cause containing %q", line, route, code, cause)
	}
}

// loggedRow is one request on a logging stack and its expected answer.
type loggedRow struct {
	name           string
	on             *stack
	method, target string
	body           []byte
	headers        map[string]string
	status         int
	code           string // the error code ("" for a success)
	// cause is what a 5xx's fault line's cause must contain.
	cause string
	// secrets are values the log must not carry besides the harness's
	// credentials, digests and the body (harnessSecrets).
	secrets []string
}

// checkLoggedRows sends each row to its stack's unlogged twin and then to
// the logging handler: the responses are the same byte for byte (request
// IDs blanked), a 5xx leaves exactly one ERROR line, the fault line with
// the row's code and cause, a lower status none, and the log carries no
// credential, digest or body.
func checkLoggedRows(t *testing.T, rows []loggedRow) {
	t.Helper()
	faults := 0
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			baseline := serveRaw(row.on.unlogged, row.method, row.target, row.body, row.headers)
			row.on.logs.Reset()
			rec := row.on.raw(row.method, row.target, row.body, row.headers)
			if rec.Code != row.status || (row.code != "" && errorCode(t, rec) != row.code) {
				t.Fatalf("status %d, want %d %s: %s", rec.Code, row.status, row.code, rec.Body.String())
			}
			wantRaw, _ := json.Marshal(observe(baseline))
			gotRaw, _ := json.Marshal(observe(rec))
			if !bytes.Equal(wantRaw, gotRaw) {
				t.Fatalf("logging changed the response:\n got %s\nwant %s", gotRaw, wantRaw)
			}
			logged := row.on.logs.String()
			lines := errorLines(t, logged)
			if rec.Code >= http.StatusInternalServerError {
				faults++
				if len(lines) != 1 {
					t.Fatalf("a %d left %d ERROR lines: %s", rec.Code, len(lines), logged)
				}
				line := lines[0]
				cause, _ := line["cause"].(string)
				if line["msg"] != "request failed" || line["code"] != row.code || line["status"] != float64(rec.Code) ||
					line["route"] == "" || line["route"] == "unmatched" || line["request_id"] != rec.Header().Get("X-Request-ID") ||
					row.cause == "" || !strings.Contains(cause, row.cause) {
					t.Fatalf("fault line=%v, want code %s and a cause containing %q", line, row.code, row.cause)
				}
			} else if len(lines) != 0 {
				t.Fatalf("a %d left ERROR lines: %v", rec.Code, lines)
			}
			for _, secret := range append(harnessSecrets(row.on.h, row.headers, row.body), row.secrets...) {
				if len(secret) >= 4 && strings.Contains(logged, secret) {
					t.Fatalf("the log carries a credential, digest or body: %q in %s", secret, logged)
				}
			}
		})
	}
	if faults == 0 {
		t.Fatal("no 5xx row ran")
	}
}

// failingLimiter admits every IP charge and fails every verified charge
// with terminated.
func failingLimiter() limits.Limiter {
	return limiterStub{
		take: func(context.Context, string, string, limits.Policy) (limits.Decision, error) {
			return limits.Decision{Allowed: true}, nil
		},
		takeMany: func(context.Context, []limits.Charge) (limits.Decision, error) {
			return limits.Decision{}, fmt.Errorf("limiter: %w", terminated())
		},
	}
}

// chainFailed is a fresh family whose audit chain failed verification:
// family policy answers a retryable 503 (scope_failed).
func chainFailed(t *testing.T, h *testkit.Harness) *testkit.Harness {
	t.Helper()
	h.AuthorityExec(t, `UPDATE families SET chain_failed=true WHERE id=$1`, h.FamilyID)
	return h
}

// TestSyncRefusalsLogLikeTheyAnswer reruns the sync and snapshot refusal
// table (TestSyncErrorPrecedenceIsUnchanged), a snapshot sample and the
// limiter and body-gate failures on logging stacks (checkLoggedRows).
func TestSyncRefusalsLogLikeTheyAnswer(t *testing.T) {
	var rows []loggedRow
	for _, c := range syncPrecedenceRows(t) {
		cause := ""
		switch {
		case strings.HasPrefix(c.name, "node not ready"):
			cause = "*errors.errorString"
		case strings.HasPrefix(c.name, "family chain failed"):
			cause = "scope_failed"
		}
		rows = append(rows, loggedRow{name: c.name, on: c.on, method: c.method, target: c.target, body: c.body, headers: c.headers,
			status: c.status, code: c.code, cause: cause})
	}
	h := testkit.New(t)
	s := newStack(t, h, limits.Policy{})
	notReady := newStackWith(t, h, stackConfig{readiness: stackNotReady{}})
	limiterDown := newStackWith(t, h, stackConfig{options: func(o *Options) { o.Limiter = failingLimiter() }})
	gateDown := newStackWith(t, h, stackConfig{options: func(o *Options) {
		o.Limiter = limiterStub{peek: func(context.Context, []limits.Charge) (limits.Decision, error) {
			return limits.Decision{}, fmt.Errorf("peek: %w", terminated())
		}}
	}})
	failed := newStack(t, chainFailed(t, testkit.New(t)), limits.Policy{})
	pull, err := json.Marshal(h.SyncRequest(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	const snapshot = "/v1/sync/snapshot"
	rows = append(rows,
		loggedRow{name: "snapshot", on: s, method: http.MethodGet, target: snapshot, headers: s.headers("owner", snapshot), status: 200},
		loggedRow{name: "snapshot, node not ready", on: notReady, method: http.MethodGet, target: snapshot, headers: notReady.headers("owner", snapshot),
			status: 503, code: "service_unavailable", cause: "*errors.errorString"},
		loggedRow{name: "snapshot, limiter down", on: limiterDown, method: http.MethodGet, target: snapshot, headers: limiterDown.headers("owner", snapshot),
			status: 503, code: "service_unavailable", cause: "service_unavailable; sqlstate 57P01"},
		loggedRow{name: "snapshot, family chain failed", on: failed, method: http.MethodGet, target: snapshot, headers: failed.headers("owner", snapshot),
			status: 503, code: "service_unavailable", cause: "scope_failed"},
		loggedRow{name: "sync, limiter down", on: limiterDown, method: http.MethodPost, target: "/v1/sync", body: pull,
			headers: limiterDown.headers("owner", "/v1/sync"), status: 503, code: "service_unavailable", cause: "service_unavailable; sqlstate 57P01"},
		loggedRow{name: "sync, body gate limiter down", on: gateDown, method: http.MethodPost, target: "/v1/sync", body: pull,
			headers: gateDown.headers("owner", "/v1/sync"), status: 503, code: "service_unavailable", cause: "sqlstate 57P01"},
	)
	checkLoggedRows(t, rows)
}

// TestPushTokenRefusalsLogLikeTheyAnswer is the push-token route's
// refusals on logging stacks (checkLoggedRows); the token is never logged.
func TestPushTokenRefusalsLogLikeTheyAnswer(t *testing.T) {
	h := testkit.New(t)
	s := newPushStack(t, h, pushConfig{})
	notReady := newPushStack(t, h, pushConfig{readiness: stackNotReady{}})
	limiterDown := newPushStack(t, h, pushConfig{limiter: failingLimiter()})
	failed := newPushStack(t, chainFailed(t, testkit.New(t)), pushConfig{})
	token := bytes.Repeat([]byte{0x5d}, 32)
	tokenHex := hex.EncodeToString(token)
	headers := func(on *stack, subject string) map[string]string { return on.pushHeaders(subject) }
	row := func(name string, on *stack, method string, body []byte, hdrs map[string]string, status int, code, cause string) loggedRow {
		return loggedRow{name: name, on: on, method: method, target: pushTokenTarget, body: body, headers: hdrs, status: status, code: code,
			cause: cause, secrets: []string{tokenHex, strings.ToUpper(tokenHex)}}
	}
	checkLoggedRows(t, []loggedRow{
		row("register", s, http.MethodPut, pushBody(token, "sandbox"), headers(s, "owner"), 204, "", ""),
		row("no credential", s, http.MethodPut, pushBody(token, "sandbox"), withHeader(headers(s, "owner"), "Authorization", ""), 401, "access_expired", ""),
		row("malformed bearer", s, http.MethodPut, pushBody(token, "sandbox"), withHeader(headers(s, "owner"), "Authorization", "Bearer not-a-token"), 401, "session_revoked", ""),
		row("short token", s, http.MethodPut, pushBody(token[:15], "sandbox"), headers(s, "owner"), 400, "invalid_push_token", ""),
		row("unsupported protocol", s, http.MethodPut, pushBody(token, "sandbox"), withHeader(headers(s, "owner"), "X-Baby-Tracker-Protocol", "v2"), 400, "unsupported_protocol", ""),
		row("node not ready", notReady, http.MethodPut, pushBody(token, "sandbox"), headers(notReady, "owner"), 503, "service_unavailable", "*errors.errorString"),
		row("limiter down", limiterDown, http.MethodPut, pushBody(token, "sandbox"), headers(limiterDown, "owner"), 503, "service_unavailable", "service_unavailable; sqlstate 57P01"),
		row("limiter down, delete", limiterDown, http.MethodDelete, nil, headers(limiterDown, "owner"), 503, "service_unavailable", "service_unavailable; sqlstate 57P01"),
		row("family chain failed", failed, http.MethodPut, pushBody(token, "sandbox"), headers(failed, "owner"), 503, "service_unavailable", "scope_failed"),
	})
}

// TestLiveActivityRefusalsLogLikeTheyAnswer is the Live Activity routes'
// refusals on logging stacks (checkLoggedRows); no token is ever logged.
func TestLiveActivityRefusalsLogLikeTheyAnswer(t *testing.T) {
	h := liveActivityHarness(t)
	on := newStackWith(t, h, stackConfig{liveActivity: true})
	worker := newStackWith(t, h, stackConfig{liveActivity: true, options: func(o *Options) {
		o.LiveActivityReady = func(context.Context) error { return fmt.Errorf("worker: %w", terminated()) }
	}})
	limiterDown := newStackWith(t, h, stackConfig{liveActivity: true, options: func(o *Options) { o.Limiter = failingLimiter() }})
	failedHarness := chainFailed(t, liveActivityHarness(t))
	failed := newStackWith(t, failedHarness, stackConfig{liveActivity: true})
	registration := deviceRegistration(t, h)
	body := encoded(t, registration)
	if rec := on.raw(http.MethodPut, "/v1/live-activities/device", body, liveActivityRequestHeaders(h.AccessToken("owner"))); rec.Code != http.StatusOK {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	changed := registration
	changed.FeedBreakMinutes = 60
	const device = "/v1/live-activities/device"
	unregister := device + "/" + testInstallation
	headers := func(on *stack, subject string) map[string]string {
		return liveActivityRequestHeaders(on.h.AccessToken(subject))
	}
	row := func(name string, on *stack, method, target string, body []byte, hdrs map[string]string, status int, code, cause string) loggedRow {
		return loggedRow{name: name, on: on, method: method, target: target, body: body, headers: hdrs, status: status, code: code, cause: cause,
			secrets: []string{registration.PushToStartToken}}
	}
	checkLoggedRows(t, []loggedRow{
		row("no credential", on, http.MethodPut, device, body, withHeader(headers(on, "owner"), "Authorization", ""), 401, "access_expired", ""),
		row("unsupported protocol", on, http.MethodPut, device, body, withHeader(headers(on, "owner"), "X-Baby-Tracker-Protocol", "v2"), 400, "unsupported_protocol", ""),
		row("body over the limit", on, http.MethodPut, device, []byte(strings.Repeat("x", 16385)), headers(on, "owner"), 413, "body_too_large", ""),
		row("foreign device", on, http.MethodPut, device, body, headers(on, "caregiver"), 403, "live_activity_registration_denied", ""),
		row("changed retry at the same version", on, http.MethodPut, device, encoded(t, changed), headers(on, "owner"), 409, "live_activity_version_conflict", ""),
		row("unregister, bad version", on, http.MethodDelete, unregister+"?version=0", nil, headers(on, "owner"), 400, "invalid_request", ""),
		row("worker not ready", worker, http.MethodPut, device, body, headers(worker, "owner"), 503, "service_unavailable", "service_unavailable"),
		row("limiter down", limiterDown, http.MethodPut, device, body, headers(limiterDown, "owner"), 503, "service_unavailable", "service_unavailable; sqlstate 57P01"),
		row("limiter down, unregister", limiterDown, http.MethodDelete, unregister+"?version=9", nil, headers(limiterDown, "owner"), 503, "service_unavailable", "service_unavailable; sqlstate 57P01"),
		row("family chain failed", failed, http.MethodPut, device, body, headers(failed, "owner"), 503, "service_unavailable", "scope_failed"),
		row("family chain failed, unregister", failed, http.MethodDelete, unregister+"?version=9", nil, headers(failed, "owner"), 503, "service_unavailable", "scope_failed"),
	})
}

// A family route's 5xx leaves the one fault line with its cause: the
// service's error, a limiter's; a refusal leaves none.
func TestFamilyFaultsLogOnceWithCause(t *testing.T) {
	p := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001", Scope: "family", SessionScope: "family", SessionFamilyID: "30000000-0000-4000-8000-000000000001"}
	var logs bytes.Buffer
	current := func(context.Context, access.Principal) (family.FamilyView, error) {
		return family.FamilyView{}, fmt.Errorf("%w: %w", family.ErrUnavailable, terminated())
	}
	options := familyHandlerOptions(t, p, familyServiceStub{current: current}, nil)
	options.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	send := func(options Options, method, target, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer access")
		rec := httptest.NewRecorder()
		New(options).ServeHTTP(rec, req)
		return rec
	}
	if rec := send(options, http.MethodGet, "/v1/family", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("service failure: %d %s", rec.Code, rec.Body.String())
	}
	oneFaultLine(t, logs.String(), "/v1/family", "service_unavailable", "service_unavailable; sqlstate 57P01")

	logs.Reset()
	limited := options
	limited.Limiter = limiterStub{take: func(context.Context, string, string, limits.Policy) (limits.Decision, error) {
		return limits.Decision{}, fmt.Errorf("limiter: %w", terminated())
	}}
	if rec := send(limited, http.MethodPost, "/v1/family/invitations", familyBody("10000000-0000-4000-8000-000000000001", `"role":"caregiver"`)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("IP limiter failure: %d %s", rec.Code, rec.Body.String())
	}
	oneFaultLine(t, logs.String(), "/v1/family/invitations", "service_unavailable", "sqlstate 57P01")

	logs.Reset()
	limited.Limiter = failingLimiter()
	if rec := send(limited, http.MethodPost, "/v1/family/invitations", familyBody("10000000-0000-4000-8000-000000000001", `"role":"caregiver"`)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("account limiter failure: %d %s", rec.Code, rec.Body.String())
	}
	oneFaultLine(t, logs.String(), "/v1/family/invitations", "service_unavailable", "sqlstate 57P01")

	logs.Reset()
	refused := options
	refused.FamilyService = familyServiceStub{current: func(context.Context, access.Principal) (family.FamilyView, error) {
		return family.FamilyView{}, family.ErrOwnerRequired
	}}
	if rec := send(refused, http.MethodGet, "/v1/family", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("refusal: %d %s", rec.Code, rec.Body.String())
	}
	if lines := errorLines(t, logs.String()); len(lines) != 0 {
		t.Fatalf("a 403 was logged as a fault: %v", lines)
	}
}

// The body gate's limiter failure (Peek, before the body is read) keeps
// its cause.
func TestBodyGateLimiterFailureLogsItsCause(t *testing.T) {
	p := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001", Scope: "family", SessionScope: "family", SessionFamilyID: "30000000-0000-4000-8000-000000000001"}
	options := familyHandlerOptions(t, p, familyServiceStub{}, nil)
	effective := protocol.DefaultLimits()
	options.SyncLimits = &effective
	var logs bytes.Buffer
	options.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	options.SyncService = syncServiceStub{principal: p, apply: func(context.Context, access.Principal, protocol.SyncRequest) (protocol.SyncResponse, error) {
		t.Fatal("service called after a body-gate failure")
		return protocol.SyncResponse{}, nil
	}}
	options.Limiter = limiterStub{peek: func(context.Context, []limits.Charge) (limits.Decision, error) {
		return limits.Decision{}, fmt.Errorf("peek: %w", terminated())
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/sync", strings.NewReader(`{"protocol_version":"v1","capabilities":["records-v1"],"cursor":null,"mutations":[],"page_token":null}`))
	req.Header.Set("Authorization", "Bearer access")
	req.Header.Set("X-Baby-Tracker-Protocol", "v1")
	req.Header.Set("X-Baby-Tracker-Capabilities", "records-v1")
	rec := httptest.NewRecorder()
	New(options).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	oneFaultLine(t, logs.String(), "/v1/sync", "service_unavailable", "sqlstate 57P01")
}

// liveActivityUnregistrationFunc is an unregistration service.
type liveActivityUnregistrationFunc func(context.Context, authority.Request) error

func (f liveActivityUnregistrationFunc) UnregisterDevice(ctx context.Context, req authority.Request, _ string, _ uint64) error {
	return f(ctx, req)
}

// A Live Activity 5xx keeps its cause: the service's error (the family
// path of writeLiveActivityError) and a limiter's (the routes' Charge).
func TestLiveActivityFaultsLogTheirCause(t *testing.T) {
	p := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001", Scope: "family", SessionScope: "family", SessionFamilyID: "30000000-0000-4000-8000-000000000001"}
	const route, target = "/v1/live-activities/device/{installation_id}", "/v1/live-activities/device/" + testInstallation + "?version=1"
	for _, c := range []struct {
		name    string
		service liveActivityUnregistrationFunc
		limiter limits.Limiter
		cause   string
	}{
		{"service", func(context.Context, authority.Request) error {
			return fmt.Errorf("unregister: %w", fmt.Errorf("%w: %w", authority.ErrUnavailable, terminated()))
		}, nil, "service_unavailable; sqlstate 57P01"},
		{"limiter", func(_ context.Context, req authority.Request) error {
			_, _, err := stubAuthorize(req, p, access.Grant{AccountID: p.AccountID, FamilyID: p.SessionFamilyID, Role: "owner"})
			return err
		}, failingLimiter(), "service_unavailable; sqlstate 57P01"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logs bytes.Buffer
			options := familyHandlerOptions(t, p, familyServiceStub{}, nil)
			options.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			options.LiveActivityUnregistration = c.service
			if c.limiter != nil {
				options.Limiter = c.limiter
			}
			rec := serveRaw(New(options), http.MethodDelete, target, nil, liveActivityRequestHeaders("access"))
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			oneFaultLine(t, logs.String(), route, "service_unavailable", c.cause)
		})
	}
}

// The export routes' Admit charges the limiter; its failure keeps its
// cause.
func TestExportAdmitLimiterFailureLogsItsCause(t *testing.T) {
	p := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001", Scope: "family", SessionScope: "family", SessionFamilyID: "30000000-0000-4000-8000-000000000001"}
	options := familyHandlerOptions(t, p, familyServiceStub{}, nil)
	options.ExportService = &exportServiceStub{}
	options.Limiter = failingLimiter()
	var logs bytes.Buffer
	options.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	rec := serveRaw(New(options), http.MethodGet, "/v1/export/30000000-0000-4000-8000-00000000e001", nil, map[string]string{
		"Authorization": "Bearer access", "X-Baby-Tracker-Protocol": "v1", "X-Baby-Tracker-Capabilities": "export-v1"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	oneFaultLine(t, logs.String(), "/v1/export/{id}", "service_unavailable", "service_unavailable; sqlstate 57P01")
}
