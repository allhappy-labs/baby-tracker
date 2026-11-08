package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/account"
	"babytracker/server/internal/admission"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/family"
	"babytracker/server/internal/identity"
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

// faultLevel is the level and message of a 5xx's one fault line: ERROR
// "request failed", except a lost race's WARN "request lost a race".
func faultLevel(code string) (level, msg string) {
	if _, lost := lostRaceClass(code); lost {
		return "WARN", "request lost a race"
	}
	return "ERROR", "request failed"
}

// linesAt are logged's lines at level with message msg.
func linesAt(t *testing.T, logged, level, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range logLines(t, logged) {
		if line["level"] == level && line["msg"] == msg {
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
// IDs blanked), a 5xx leaves exactly one fault line with the row's code
// and cause (ERROR, or WARN and no ERROR line for a lost race: faultLevel),
// a lower status no ERROR line, and the log carries no credential, digest
// or body.
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
				level, msg := faultLevel(row.code)
				if level != "ERROR" && len(lines) != 0 {
					t.Fatalf("a %s left ERROR lines: %v", row.code, lines)
				}
				lines = linesAt(t, logged, level, msg)
				if len(lines) != 1 {
					t.Fatalf("a %d left %d %s lines: %s", rec.Code, len(lines), level, logged)
				}
				line := lines[0]
				cause, _ := line["cause"].(string)
				if line["code"] != row.code || line["status"] != float64(rec.Code) ||
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
	// The fixture's 4-digit token would match a random hex request ID in the
	// log about once in a few thousand runs; a full-length token cannot.
	registration.PushToStartToken = strings.Repeat("a1b2c3d4", 8)
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

// errKeyring is a failure deriving an access token's digests. Its text is
// free (a path), so a fault line names it by type, never by text.
var errKeyring = errors.New("keyring k1 unreadable at /run/secrets/token-key")

// errNotReady is a readiness failure whose text carries a connection string.
var errNotReady = errors.New("ping postgres://runtime:hunter2@db/app failed")

// deadlineExceededByServer is a request context the server's own deadline
// ended (a handler's timeout on the request): the client is still there.
func deadlineExceededByServer() context.Context {
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
	cancel()
	return ctx
}

// cancelledByPeer is a request context net/http cancelled because the
// client closed the connection.
func cancelledByPeer() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// faultBearer is the bearer every fault row sends: it must never be logged.
var faultBearer = strings.Repeat("Zq9_", 10) + "Zq9"

// faultDigest is the access digest the fault rows' session service derives
// from faultBearer: it must never be logged, in any encoding.
var faultDigest = authority.KeyedDigest{KeyID: "stub", Sum: sha256.Sum256([]byte("fault-row-access-digest"))}

// The care data careSyncBody carries: a baby's name and a sleep's note,
// and their IDs. None may ever be logged.
const (
	careBabyName  = "Zelphine Quarrington"
	careSleepNote = "Pumpkin woke at the lighthouse"
	careBabyID    = "10000000-0000-4000-8000-0000000000b1"
	careSleepID   = "10000000-0000-4000-8000-0000000000c1"
)

var careSecrets = []string{"Zelphine", "Quarrington", "Pumpkin", "lighthouse", careBabyID, careSleepID}

// careSyncBody is a sync request that creates a named baby and a sleep
// with a note.
func careSyncBody() string {
	note, babyID := careSleepNote, careBabyID
	request := protocol.SyncRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityRecordsV1}},
		Mutations: []protocol.Mutation{
			{MutationID: "01000000-0000-4000-8000-0000000000b1", EntityID: careBabyID, EntityKind: "baby", Operation: "create", PayloadVersion: 1,
				ClientMutationTime: "2026-09-20T12:00:00Z", Payload: protocol.BabyPayload{Name: careBabyName, BirthDate: "2026-01-01"}},
			{MutationID: "01000000-0000-4000-8000-0000000000c1", EntityID: careSleepID, EntityKind: "sleep", Operation: "create", PayloadVersion: 2,
				ClientMutationTime: "2026-09-20T12:00:00Z", Payload: protocol.SleepPayload{BabyID: babyID, StartedAt: "2026-09-20T10:00:00Z",
					TimeZoneID: "Europe/Kyiv", Note: &note}},
		}}
	body, err := json.Marshal(request)
	if err != nil {
		panic(err)
	}
	return string(body)
}

// faultRow is one request (repeated) on a stubbed stack, and what its log
// must carry.
type faultRow struct {
	name string
	// route is the target: "" is GET /v1/family, whose service answers
	// cause, or (cause nil) a 503 wrapping the request context's error.
	route string
	cause error
	// ctx is the request's context (nil: alive).
	ctx              context.Context
	admissionFull    bool
	admissionWait    bool  // the full controller waits admission.MaxWait for a slot
	notReady         bool  // readiness fails with errNotReady
	accessDigestsErr error // the session service's AccessDigests error
	accountErr       error // the account service's Delete error
	challengeErr     error // the challenge service's error
	limiterTakeErr   error // the limiter's Take error
	serverStateErr   error // the server state's error
	// syncErr fails the write of careSyncBody (route /v1/sync).
	syncErr   error
	repeat    int // requests sent (0: 1)
	wantLines int // log lines carrying wantField (0: 1)
	wantField string
	wantLevel string // the level of those lines ("": any)
	forbid    []string
}

func (row faultRow) options(t *testing.T, logger *slog.Logger) Options {
	t.Helper()
	p := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001", Scope: "family", SessionScope: "family", SessionFamilyID: "30000000-0000-4000-8000-000000000001"}
	current := func(ctx context.Context, _ access.Principal) (family.FamilyView, error) {
		if row.cause != nil {
			return family.FamilyView{}, row.cause
		}
		return family.FamilyView{}, fmt.Errorf("%w: %w", family.ErrUnavailable, ctx.Err())
	}
	options := familyHandlerOptions(t, p, familyServiceStub{current: current}, nil)
	options.Logger = logger
	options.SessionService = sessionServiceStub{
		authenticate: func(context.Context, string) (access.Principal, error) { return p, nil },
		accessDigests: func(string) ([]authority.KeyedDigest, error) {
			if row.accessDigestsErr != nil {
				return nil, row.accessDigestsErr
			}
			return []authority.KeyedDigest{faultDigest}, nil
		},
	}
	syncLimits := protocol.DefaultLimits()
	options.SyncLimits = &syncLimits
	options.SyncService = syncServiceStub{principal: p, apply: func(context.Context, access.Principal, protocol.SyncRequest) (protocol.SyncResponse, error) {
		if row.syncErr == nil {
			t.Fatal("a sync write without syncErr")
		}
		return protocol.SyncResponse{}, row.syncErr
	}}
	if row.serverStateErr != nil {
		options.ServerState = serverStateFunc(func(context.Context) (string, uint64, error) { return "", 0, row.serverStateErr })
	}
	options.Readiness = readinessFunc(func(context.Context) error {
		if row.notReady {
			return errNotReady
		}
		return nil
	})
	options.AccountService = accountLifecycleStub{delete: func(context.Context, access.Principal, string, bool) (account.DeletionResult, error) {
		return account.DeletionResult{}, row.accountErr
	}}
	options.ChallengeService = challengeServiceFunc(func(context.Context) (identity.Challenge, error) {
		return identity.Challenge{}, row.challengeErr
	})
	if row.limiterTakeErr != nil {
		limiter := options.Limiter.(limiterStub)
		limiter.take = func(context.Context, string, string, limits.Policy) (limits.Decision, error) {
			return limits.Decision{}, row.limiterTakeErr
		}
		options.Limiter = limiter
	}
	if row.admissionFull {
		wait := time.Duration(0)
		if row.admissionWait {
			wait = admission.MaxWait
		}
		controller, _ := admission.NewWaiting(1, 0, wait)
		release, _ := controller.TryAcquire(admission.Sync)
		t.Cleanup(release)
		options.Admission = controller
	}
	return options
}

func (row faultRow) request() *http.Request {
	method, target, body := http.MethodGet, row.route, ""
	headers := map[string]string{}
	switch row.route {
	case "":
		target = "/v1/family"
	case "/v1/sync":
		method, body = http.MethodPost, `{"protocol_version":"v1","capabilities":["records-v1"],"cursor":null,"mutations":[],"page_token":null}`
		if row.syncErr != nil {
			body = careSyncBody()
		}
		headers["X-Baby-Tracker-Protocol"], headers["X-Baby-Tracker-Capabilities"] = "v1", "records-v1"
	case "/v1/account/delete":
		method, body = http.MethodPost, familyBody("10000000-0000-4000-8000-000000000009", `"confirmed":true`)
	case "/v1/auth/challenge":
		method, body = http.MethodPost, `{"protocol_version":"v1","capabilities":[]}`
	}
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+faultBearer)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	if row.ctx != nil {
		req = req.WithContext(row.ctx)
	}
	return req
}

// TestFaultLinesNameCausesClassesAndRefusals sends each row to a logging
// stack and to its unlogged twin: every response is the same byte for byte
// (request IDs blanked), the log carries the row's field on exactly the
// wanted number of lines, and never the bearer, the body or the forbidden
// text.
func TestFaultLinesNameCausesClassesAndRefusals(t *testing.T) {
	digest := "ab" + strings.Repeat("3f", 31)
	requestID := "c" + strings.Repeat("0e", 15) + "1"
	for _, row := range []faultRow{
		{name: "sentinel code is named", cause: errors.New("family_lock_timeout"), wantField: `"cause_code":"family_lock_timeout"`, wantLevel: "ERROR"},
		{name: "code with digits is named", cause: fmt.Errorf("verify: %w", errors.New("sha256_mismatch")), wantField: `"cause_code":"sha256_mismatch"`, forbid: []string{"verify"}},
		{name: "cause keeps digits out", cause: errors.New("sha256_mismatch"), wantField: `"cause":"*errors.errorString"`},
		{name: "a failed care write never logs its data", route: "/v1/sync", wantField: `"cause":"service_unavailable; sqlstate 57P01`, wantLevel: "ERROR",
			syncErr: fmt.Errorf("%w: apply sleep %s note %q for %s: %w", authority.ErrUnavailable, careSleepID, careSleepNote, careBabyName, terminated())},
		{name: "wrapped sentinel codes are all named", cause: fmt.Errorf("%w: %w", family.ErrUnavailable, errors.New("family_lock_timeout")),
			wantField: `"cause_code":"service_unavailable; family_lock_timeout"`},
		{name: "free text sentinel stays a type", cause: errors.New("could not read config at /x"), wantField: `"cause_type":"*errors.errorString"`,
			forbid: []string{"/x", "could not", "cause_code"}},
		{name: "a digest is never a code", cause: errors.New(digest), wantField: `"cause_type":"*errors.errorString"`, forbid: []string{digest, "cause_code"}},
		{name: "a request ID is never a code", cause: errors.New(requestID), wantField: `"cause_type":"*errors.errorString"`, forbid: []string{requestID, "cause_code"}},
		{name: "a Postgres cause has its type", cause: fmt.Errorf("%w: %w", family.ErrUnavailable, terminated()),
			wantField: `"cause_type":"family.codedError; *pgconn.PgError"`, forbid: []string{"cause_class"}},
		{name: "server deadline is timed_out", ctx: deadlineExceededByServer(), wantField: `"cause_class":"timed_out"`, wantLevel: "ERROR"},
		{name: "a deadline inside the handler is timed_out", cause: fmt.Errorf("%w: %w", family.ErrUnavailable, context.DeadlineExceeded),
			wantField: `"cause_class":"timed_out"`, wantLevel: "ERROR"},
		{name: "peer cancel is client_gone", ctx: cancelledByPeer(), wantField: `"cause_class":"client_gone"`, wantLevel: "INFO"},
		{name: "a lost race has a WARN line once a second and no ERROR line", cause: fmt.Errorf("receipt: %w", authority.ErrSecurityBusy),
			repeat: 20, wantLines: 1, wantField: `"msg":"request lost a race"`, wantLevel: "WARN", forbid: []string{`"level":"ERROR"`}},
		{name: "a lost race carries its cause fields", cause: fmt.Errorf("%w: %w", authority.ErrSecurityBusy, terminated()),
			wantField: `"cause":"security_busy; sqlstate 57P01`, wantLevel: "WARN", forbid: []string{`"level":"ERROR"`}},
		{name: "a lost race names its cause code and what it left out", cause: fmt.Errorf("receipt: %w", authority.ErrSecurityBusy),
			wantField: `"cause_code":"security_busy","suppressed":0`, wantLevel: "WARN"},
		{name: "server_busy has a fault line once a second", route: "/v1/sync", admissionFull: true, repeat: 50, wantLines: 1,
			wantField: `"refusal":"server_busy"`, wantLevel: "WARN"},
		{name: "server_busy before the body has a fault line once a second", admissionFull: true, repeat: 50, wantLines: 1,
			wantField: `"refusal":"server_busy"`, wantLevel: "WARN"},
		{name: "an admission waiter whose client left is client_gone, not server_busy", route: "/v1/sync", admissionFull: true, admissionWait: true,
			ctx: cancelledByPeer(), wantField: `"cause_class":"client_gone"`, wantLevel: "INFO", forbid: []string{"server_busy", `"level":"ERROR"`}},
		{name: "an admission waiter before the body whose client left is client_gone", admissionFull: true, admissionWait: true,
			ctx: cancelledByPeer(), wantField: `"cause_class":"client_gone"`, wantLevel: "INFO", forbid: []string{"server_busy", `"level":"ERROR"`}},
		{name: "not_ready has a fault line once a second", route: "/readyz", notReady: true, repeat: 50, wantLines: 1,
			wantField: `"refusal":"not_ready"`, wantLevel: "WARN", forbid: []string{"hunter2", "postgres://"}},
		{name: "not_ready names its cause by type", route: "/readyz", notReady: true, wantField: `"cause_type":"*errors.errorString"`},
		{name: "server info not_ready has a fault line once a second", route: "/v1/server", repeat: 20, wantLines: 1,
			wantField: `"refusal":"not_ready"`, wantLevel: "WARN"},
		{name: "server info names an invalid epoch", route: "/v1/server", wantField: `"cause_code":"recovery_epoch_invalid"`},
		{name: "server info state failure names its cause by type", route: "/v1/server", serverStateErr: errNotReady,
			wantField: `"cause_type":"*errors.errorString"`, forbid: []string{"hunter2", "postgres://"}},
		{name: "hosted auth not_ready has a fault line once a second", route: "/v1/auth/challenge", notReady: true, repeat: 20, wantLines: 1,
			wantField: `"refusal":"not_ready"`, wantLevel: "WARN", forbid: []string{"hunter2"}},
		{name: "hosted auth identity_unavailable has a fault line", route: "/v1/auth/challenge", challengeErr: fmt.Errorf("%w: %w", errors.New("apple keys"), terminated()),
			repeat: 20, wantLines: 1, wantField: `"refusal":"identity_unavailable"`, wantLevel: "WARN"},
		{name: "hosted auth rate_limit_unavailable has a fault line", route: "/v1/auth/challenge", limiterTakeErr: fmt.Errorf("limiter: %w", terminated()),
			repeat: 20, wantLines: 1, wantField: `"refusal":"rate_limit_unavailable"`, wantLevel: "WARN"},
		{name: "AccessDigests failure carries its cause", route: "/v1/sync", accessDigestsErr: fmt.Errorf("decode: %w", errKeyring),
			wantField: `"cause_type"`, wantLevel: "ERROR", forbid: []string{"/run/secrets", "keyring", "decode"}},
		{name: "live AccessDigests failure carries its cause", route: "/v1/live", accessDigestsErr: fmt.Errorf("decode: %w", errKeyring),
			wantField: `"cause_type":"*errors.errorString"`, wantLevel: "ERROR", forbid: []string{"/run/secrets", "keyring"}},
		{name: "account deletion failure carries its cause", route: "/v1/account/delete", accountErr: fmt.Errorf("%w: %w", authority.ErrUnavailable, terminated()),
			wantField: `"cause":"service_unavailable; sqlstate 57P01`, wantLevel: "ERROR"},
	} {
		t.Run(row.name, func(t *testing.T) {
			var logs bytes.Buffer
			logged := New(row.options(t, slog.New(slog.NewJSONHandler(&logs, nil))))
			unlogged := New(row.options(t, nil))
			for range max(row.repeat, 1) {
				want, got := httptest.NewRecorder(), httptest.NewRecorder()
				unlogged.ServeHTTP(want, row.request())
				logged.ServeHTTP(got, row.request())
				wantRaw, _ := json.Marshal(observe(want))
				gotRaw, _ := json.Marshal(observe(got))
				if !bytes.Equal(wantRaw, gotRaw) {
					t.Fatalf("logging changed the response:\n got %s\nwant %s", gotRaw, wantRaw)
				}
				if got.Code < http.StatusInternalServerError {
					t.Fatalf("status %d, want a 5xx: %s", got.Code, got.Body.String())
				}
			}
			matching := 0
			for _, raw := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				if !strings.Contains(raw, row.wantField) {
					continue
				}
				matching++
				if row.wantLevel != "" && !strings.Contains(raw, `"level":"`+row.wantLevel+`"`) {
					t.Fatalf("line %s, want level %s", raw, row.wantLevel)
				}
			}
			if wantLines := max(row.wantLines, 1); matching != wantLines {
				t.Fatalf("%d lines carry %s, want %d: %s", matching, row.wantField, wantLines, logs.String())
			}
			forbidden := append([]string{faultBearer, "10000000-0000-4000-8000-000000000009", `records-v1\",\"cursor`,
				hex.EncodeToString(faultDigest.Sum[:]), base64.StdEncoding.EncodeToString(faultDigest.Sum[:]),
				base64.RawURLEncoding.EncodeToString(faultDigest.Sum[:])}, careSecrets...)
			for _, secret := range append(forbidden, row.forbid...) {
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("the log carries %q: %s", secret, logs.String())
				}
			}
		})
	}
}

// fakeClock is a settable clock for the refusal fault lines.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

// A refusal class logs at most one line a second, counting the refusals it
// left out; each class keeps its own second.
func TestRefusalFaultLinesAreSpacedPerClass(t *testing.T) {
	// The production base carries the monotonic clock, so a wall-clock step
	// cannot move the spacing. (A Time whose wall clock steps back while its
	// monotonic reading moves on cannot be built, so the step itself is not
	// replayed here.)
	if base := newRefusalFaults(time.Now).start.String(); !strings.Contains(base, " m=") {
		t.Fatalf("the spacing base %s has no monotonic reading", base)
	}
	clock := &fakeClock{now: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	faults := newRefusalFaults(clock.Now)
	admit := func(class refusalClass, wantOK bool, wantSuppressed int64) {
		t.Helper()
		suppressed, ok := faults.admit(class)
		if ok != wantOK || suppressed != wantSuppressed {
			t.Fatalf("class %d at %s: ok=%v suppressed=%d, want %v %d", class, clock.now.Format(time.StampMilli), ok, suppressed, wantOK, wantSuppressed)
		}
	}
	admit(refusalServerBusy, true, 0)
	admit(refusalServerBusy, false, 0)
	admit(refusalNotReady, true, 0)
	clock.now = clock.now.Add(999 * time.Millisecond)
	admit(refusalServerBusy, false, 0)
	clock.now = clock.now.Add(time.Millisecond)
	admit(refusalServerBusy, true, 2)
	admit(refusalNotReady, true, 0)
	admit(refusalIdentityUnavailable, true, 0)
	admit(refusalRateLimitUnavailable, true, 0)

	// Concurrent refusals within one second leave exactly one line.
	clock.now = clock.now.Add(time.Hour)
	var wg sync.WaitGroup
	var lines atomic.Int64
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := faults.admit(refusalServerBusy); ok {
				lines.Add(1)
			}
		}()
	}
	wg.Wait()
	if lines.Load() != 1 {
		t.Fatalf("%d lines from one second's refusals, want 1", lines.Load())
	}
}

// valueErrorAllowed are the known safe errors.New arguments that are not a
// string literal, by file (relative to Server/): an argument that is one of
// them, or a concatenation of them and string literals, passes. Each is
// fixed text of the server's own, never a client's value.
var valueErrorAllowed = map[string][]string{
	// importer's code(value): every caller passes a string literal
	// (checked by TestErrorsAreNeverBuiltFromValues).
	"internal/importer/resolver.go": {"value"},
	// The load generator's own failure codes (capacity.FailureCode).
	"internal/capacity/workload.go": {"r.FailureCode"},
	// Usage and configuration text constants.
	"cmd/baby-tracker-admin/authority_cli.go":           {"authorityUsage"},
	"cmd/baby-tracker-admin/mcp_cli.go":                 {"mcpUsage"},
	"cmd/baby-tracker-admin/operator_cli.go":            {"mcpLabelRule"},
	"cmd/baby-tracker-server/apns_preflight.go":         {"apnsPreflightUsage"},
	"cmd/baby-tracker-server/apns_sink_identitytest.go": {"identityTestAPNSSinkEnv"},
}

// allowedErrorText reports whether expr, an errors.New argument in rel, is
// string literals and rel's valueErrorAllowed entries joined with +,
// marking each entry it uses.
func allowedErrorText(rel string, expr ast.Expr, used map[string]bool) bool {
	switch e := expr.(type) {
	case *ast.BasicLit:
		return e.Kind == token.STRING
	case *ast.ParenExpr:
		return allowedErrorText(rel, e.X, used)
	case *ast.BinaryExpr:
		return e.Op == token.ADD && allowedErrorText(rel, e.X, used) && allowedErrorText(rel, e.Y, used)
	}
	text := types.ExprString(expr)
	for _, allowed := range valueErrorAllowed[rel] {
		if text == allowed {
			used[rel+" "+text] = true
			return true
		}
	}
	return false
}

// bareVerb is a format that is one verb of a value and nothing else.
var bareVerb = regexp.MustCompile(`^%[-+# 0-9.]*[sqvx]$`)

// TestErrorsAreNeverBuiltFromValues scans every non-test Go file under
// internal/ and cmd/: an error's whole message must never be a value (a
// fault line repeats code-shaped messages as cause_code, trusting their
// shape). errors.New takes a string literal (or a valueErrorAllowed
// exception); fmt.Errorf takes a literal format that is not a bare verb
// ("%w" alone only wraps, and is fine). Every allowlist entry must still
// be needed.
func TestErrorsAreNeverBuiltFromValues(t *testing.T) {
	root := filepath.Join("..", "..")
	used := map[string]bool{}
	var problems []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() && entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			files := token.NewFileSet()
			file, err := parser.ParseFile(files, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				where := fmt.Sprintf("%s:%d", rel, files.Position(call.Pos()).Line)
				first := call.Args[0]
				literal, isLiteral := first.(*ast.BasicLit)
				isLiteral = isLiteral && literal.Kind == token.STRING
				switch callee := types.ExprString(call.Fun); {
				case callee == "errors.New" && !isLiteral:
					if !allowedErrorText(rel, first, used) {
						problems = append(problems, where+": errors.New("+types.ExprString(first)+")")
					}
				case callee == "fmt.Errorf" && !isLiteral:
					problems = append(problems, where+": fmt.Errorf with a format that is not a literal")
				case callee == "fmt.Errorf":
					if format, err := strconv.Unquote(literal.Value); err != nil || bareVerb.MatchString(format) {
						problems = append(problems, where+": fmt.Errorf("+literal.Value+", …) is a bare value")
					}
				case callee == "code" && strings.HasPrefix(rel, "internal/importer/") && !isLiteral:
					problems = append(problems, where+": importer code("+types.ExprString(first)+")")
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for rel, entries := range valueErrorAllowed {
		for _, entry := range entries {
			if !used[rel+" "+entry] {
				problems = append(problems, rel+": stale allowlist entry "+entry)
			}
		}
	}
	if len(problems) != 0 {
		t.Fatalf("errors built from values:\n%s", strings.Join(problems, "\n"))
	}
}
