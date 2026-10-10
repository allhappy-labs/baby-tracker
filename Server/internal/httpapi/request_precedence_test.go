package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	familyexport "babytracker/server/internal/export"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/realtime"
	"babytracker/server/internal/testkit"
)

// precedenceGolden is every refusal of the import, export and live routes
// as a shipped client sees it (status, the headers it reads, the body with
// its request ID blanked), recorded against the handlers that
// authenticated, read the family and charged in separate steps
// (BT_UPDATE_PRECEDENCE_GOLDEN=1 rewrites it). The one-statement handlers
// must answer every row byte for byte the same.
const precedenceGolden = "testdata/request_precedence.json"

// routeStack is the import, export and live handlers over a harness, with
// in-memory limits whose spent policies a row may choose.
type routeStack struct {
	h       *testkit.Harness
	handler http.Handler
	// wire counts the authority's connection (nil when the handlers
	// authorize on the harness pool).
	wire *testkit.Wire
	// exports is the export service's worker.
	exports *familyexport.Worker
	// logs is the handlers' log (Options.Logger).
	logs *syncBuffer
}

// routeConfig varies a stack: one-request policies for the import pull
// device, the export account, the live device and the live IP (zero
// means the default), and readiness.
type routeConfig struct {
	importPull, exportAccount, liveDevice, liveIP limits.Policy
	readiness                                     Readiness
	// exportOff is a node with export disabled (no export service);
	// importOff one with no import service.
	exportOff, importOff bool
	// limiterFails is a limiter whose verified charges (TakeMany) fail;
	// the IP charge (Take) still admits.
	limiterFails bool
	// trustedProxy trusts the test client's address (192.0.2.0/24) as a
	// proxy, so its X-Forwarded-For header is read.
	trustedProxy bool
}

// newRouteStack composes the routes as production does, with the authority
// (the routes' authorizer and the import and export services' guard) on a
// one-connection counted pool.
func newRouteStack(t *testing.T, h *testkit.Harness, config routeConfig) *routeStack {
	t.Helper()
	pool, wire := testkit.CountedPool(t, h.Runtime)
	guard := h.AuthorityOn(t, pool)
	policies := limits.DefaultPolicies()
	pick := func(value limits.Policy, scope string) limits.Policy {
		if value == (limits.Policy{}) {
			return policies[scope]
		}
		return value
	}
	if config.readiness == nil {
		config.readiness = stackReady{}
	}
	limiter, err := limits.New(limits.Options{Pepper: [32]byte{7}, Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	hub, err := realtime.NewHub(t.Context(), realtime.HubOptions{Limits: h.Limits, Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	var proxies []netip.Prefix
	if config.trustedProxy {
		proxies = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	}
	resolver, err := limits.NewClientIPResolver(proxies)
	if err != nil {
		t.Fatal(err)
	}
	var routeLimiter limits.Limiter = limiter
	if config.limiterFails {
		routeLimiter = limiterStub{
			take: func(context.Context, string, string, limits.Policy) (limits.Decision, error) {
				return limits.Decision{Allowed: true}, nil
			},
			takeMany: func(context.Context, []limits.Charge) (limits.Decision, error) {
				return limits.Decision{}, errors.New("limiter unavailable")
			},
		}
	}
	var key [32]byte
	for i := range key {
		key[i] = 0x5a
	}
	exports, worker, err := familyexport.New(familyexport.Options{Authority: guard, Identity: h.Info,
		Runtime: h.Runtime, WorkerPool: h.Maintenance, Keys: map[string][32]byte{"test.v1": key}, ActiveKeyID: "test.v1",
		Now: h.DatabaseNow})
	if err != nil {
		t.Fatal(err)
	}
	var exportService ExportService = exports
	if config.exportOff {
		exportService = nil
	}
	var importService ImportService = h.ImporterOn(guard)
	if config.importOff {
		importService = nil
	}
	logs := &syncBuffer{}
	options := Options{Logger: slog.New(slog.NewJSONHandler(logs, nil)), Readiness: config.readiness, InstanceID: h.Info.InstanceID, RecoveryEpoch: h.Info.RecoveryEpoch,
		CanonicalOrigin: h.Info.CanonicalOrigin, SyncLimits: &h.Limits, SessionService: h.Auth,
		ImportService: importService, ExportService: exportService, RequestAuthorizer: guard, LiveHub: hub,
		Limiter: routeLimiter, ClientIPResolver: resolver,
		SyncMutationDevicePolicy: policies[limits.ScopeSyncMutationDevice], SyncMutationAccountPolicy: policies[limits.ScopeSyncMutationAccount],
		SyncMutationFamilyPolicy: policies[limits.ScopeSyncMutationFamily], SyncPullDevicePolicy: pick(config.importPull, limits.ScopeSyncPullDevice),
		SyncPullAccountPolicy: policies[limits.ScopeSyncPullAccount],
		ExportIPPolicy:        policies[limits.ScopeExportIP], ExportAccountPolicy: pick(config.exportAccount, limits.ScopeExportAccount),
		ExportDevicePolicy:  policies[limits.ScopeExportDevice],
		LiveAttemptIPPolicy: pick(config.liveIP, limits.ScopeLiveAttemptIP), LiveAttemptDevicePolicy: pick(config.liveDevice, limits.ScopeLiveAttemptDevice),
		LiveAttemptAccountPolicy: policies[limits.ScopeLiveAttemptAccount], LiveAttemptFamilyPolicy: policies[limits.ScopeLiveAttemptFamily]}
	return &routeStack{h: h, handler: New(options), wire: wire, exports: worker, logs: logs}
}

// routeHeaders is a route's protocol headers with subject's bearer token.
func (s *routeStack) routeHeaders(subject, target string) map[string]string {
	capabilities := "records-v1,import-resolver-v1"
	switch {
	case strings.HasPrefix(target, "/v1/export"):
		capabilities = "export-v1"
	case strings.HasPrefix(target, "/v1/live"):
		capabilities = "records-v1,live-hints-v1"
	}
	return map[string]string{"Authorization": "Bearer " + s.h.AccessToken(subject), "X-Baby-Tracker-Protocol": "v1",
		"X-Baby-Tracker-Capabilities": capabilities}
}

func (s *routeStack) do(method, target string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	if body == nil {
		req = httptest.NewRequest(method, target, http.NoBody)
	}
	for name, value := range headers {
		if value != "" {
			req.Header.Set(name, value)
		}
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

func (s *routeStack) send(t *testing.T, subject, method, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	return s.do(method, target, body, s.routeHeaders(subject, target))
}

func withHeader(headers map[string]string, name, value string) map[string]string {
	out := map[string]string{}
	for k, v := range headers {
		out[k] = v
	}
	out[name] = value
	return out
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

var requestIDField = regexp.MustCompile(`"request_id":"[^"]*"`)

// observed is what the golden records of one response.
type observed struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body,omitempty"`
}

func observe(rec *httptest.ResponseRecorder) observed {
	out := observed{Status: rec.Code, Headers: map[string]string{}}
	for _, name := range []string{"Cache-Control", "Content-Type", "Retry-After", "WWW-Authenticate"} {
		if value := rec.Header().Get(name); value != "" {
			out.Headers[name] = value
		}
	}
	if rec.Code >= 400 {
		out.Body = requestIDField.ReplaceAllString(rec.Body.String(), `"request_id":"-"`)
	}
	return out
}

type precedenceRow struct {
	name    string
	on      *routeStack
	method  string
	target  string
	body    []byte
	headers map[string]string
	status  int
	code    string
}

// TestImportExportLiveErrorPrecedenceIsUnchanged pins the legacy refusal
// order of the import, export and live routes on the wire:
//   - import: readiness, the credential, the session's scope, the headers,
//     the path, the query or body, family policy (the legacy family read),
//     the rate limit, then the import service (whose role and import checks
//     follow the charge);
//   - export: readiness, the credential, the session's scope, the headers,
//     the rate limit, the query or body and the request's own fields, then
//     family policy (the legacy service's family read), then the owner
//     check and the export's state;
//   - live: readiness, the IP limit, the credential, the session's scope,
//     the headers, family policy, the device, account and family limits,
//     then the socket checks.
//
// A family that left active refuses through family policy; one whose audit
// chain failed answers 503. Each also has a stack whose route limit was
// spent before the family changed, so the rows show where the charge sits.
func TestImportExportLiveErrorPrecedenceIsUnchanged(t *testing.T) {
	rows := importExportLiveRows(t)
	got := map[string]observed{}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			rec := row.on.do(row.method, row.target, row.body, row.headers)
			var response struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || rec.Code != row.status || response.Error.Code != row.code {
				t.Errorf("status %d code %q, want %d %q: %s", rec.Code, response.Error.Code, row.status, row.code, rec.Body.String())
			}
			got[row.name] = observe(rec)
		})
	}
	path := filepath.FromSlash(precedenceGolden)
	if os.Getenv("BT_UPDATE_PRECEDENCE_GOLDEN") == "1" {
		raw, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want := precedenceGoldenRows(t)
	if len(want) != len(rows) {
		t.Errorf("golden has %d rows, the test %d", len(want), len(rows))
	}
	for _, row := range rows {
		w, ok := want[row.name]
		g := got[row.name]
		if !ok {
			t.Errorf("%s: not in the golden", row.name)
			continue
		}
		wantRaw, _ := json.Marshal(w)
		gotRaw, _ := json.Marshal(g)
		if !bytes.Equal(wantRaw, gotRaw) {
			t.Errorf("%s:\n got %s\nwant %s", row.name, gotRaw, wantRaw)
		}
	}
}

// precedenceGoldenRows is the recorded golden, by row name.
func precedenceGoldenRows(t *testing.T) map[string]observed {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(precedenceGolden))
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]observed
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	return want
}

// TestFaultLoggingNeverChangesTheResponse reruns the import, export and
// live precedence table (TestImportExportLiveErrorPrecedenceIsUnchanged)
// on stacks that log: every response is the golden's byte for byte, every
// 5xx leaves exactly one ERROR line (the fault line, with the row's route
// and code) and every 4xx none, and no line carries a bearer, refresh or
// device credential, an access token's digest, or the request body.
func TestFaultLoggingNeverChangesTheResponse(t *testing.T) {
	rows := importExportLiveRows(t)
	want := precedenceGoldenRows(t)
	if len(want) != len(rows) {
		t.Fatalf("golden has %d rows, the test %d", len(want), len(rows))
	}
	faults := 0
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			row.on.logs.Reset()
			rec := row.on.do(row.method, row.target, row.body, row.headers)
			wantRaw, _ := json.Marshal(want[row.name])
			gotRaw, _ := json.Marshal(observe(rec))
			if !bytes.Equal(wantRaw, gotRaw) {
				t.Errorf("response changed:\n got %s\nwant %s", gotRaw, wantRaw)
			}
			logged := row.on.logs.String()
			var errorLines []map[string]any
			for _, raw := range strings.Split(strings.TrimSpace(logged), "\n") {
				if raw == "" {
					continue
				}
				var line map[string]any
				if err := json.Unmarshal([]byte(raw), &line); err != nil {
					t.Fatalf("log line %q: %v", raw, err)
				}
				if line["level"] == "ERROR" {
					errorLines = append(errorLines, line)
				}
			}
			if rec.Code >= http.StatusInternalServerError {
				faults++
				if len(errorLines) != 1 {
					t.Fatalf("a %d left %d ERROR lines: %s", rec.Code, len(errorLines), logged)
				}
				line := errorLines[0]
				if line["msg"] != "request failed" || line["code"] != row.code || line["status"] != float64(rec.Code) ||
					line["route"] == "" || line["route"] == "unmatched" || line["request_id"] != rec.Header().Get("X-Request-ID") {
					t.Fatalf("fault line=%v", line)
				}
			} else if len(errorLines) != 0 {
				t.Fatalf("a %d left ERROR lines: %v", rec.Code, errorLines)
			}
			for _, secret := range loggedSecrets(t, row) {
				if strings.Contains(logged, secret) {
					t.Fatalf("the log carries a credential, digest or body: %q in %s", secret, logged)
				}
			}
		})
	}
	if faults == 0 {
		t.Fatal("no 5xx row ran")
	}
}

// loggedSecrets is what row's log must never carry: every credential of
// its stack's subjects (access, refresh, device), every access token's
// digests under the configured keys (hex, base64), and the request body.
func loggedSecrets(t *testing.T, row precedenceRow) []string {
	t.Helper()
	secrets := harnessSecrets(row.on.h, row.headers, row.body)
	if len(row.body) != 0 {
		secrets = append(secrets, "Mira")
	}
	return secrets
}

// harnessSecrets is what a request's log must never carry: every
// credential of the harness's subjects (access, refresh, device) and the
// request's own bearer, every access token's digests under the configured
// keys (hex, base64), and the request body (raw and JSON-escaped). Values
// shorter than four bytes are left out.
func harnessSecrets(h *testkit.Harness, headers map[string]string, body []byte) []string {
	var secrets []string
	tokens := []string{strings.TrimPrefix(headers["Authorization"], "Bearer ")}
	for _, subject := range []string{"owner", "caregiver", "viewer", "removed", "outsider"} {
		tokens = append(tokens, h.AccessToken(subject))
		secrets = append(secrets, h.RefreshToken(subject), h.DeviceCredential(subject))
	}
	for _, token := range tokens {
		secrets = append(secrets, token)
		digests, err := h.Auth.AccessDigests(token)
		if err != nil {
			continue
		}
		for _, digest := range digests {
			secrets = append(secrets, hex.EncodeToString(digest.Sum[:]), base64.StdEncoding.EncodeToString(digest.Sum[:]),
				base64.RawURLEncoding.EncodeToString(digest.Sum[:]))
		}
	}
	if len(body) != 0 {
		escaped, _ := json.Marshal(string(body))
		secrets = append(secrets, string(body), strings.Trim(string(escaped), `"`))
	}
	out := secrets[:0]
	for _, secret := range secrets {
		if len(secret) >= 4 && secret != "Bearer" {
			out = append(out, secret)
		}
	}
	return out
}

// importExportLiveRows is the import, export and live refusal table over
// its stacks, with every budget a row depends on already spent.
func importExportLiveRows(t *testing.T) []precedenceRow {
	t.Helper()
	h := testkit.New(t)
	h.AddMember(t, "viewer", "viewer")
	removed := h.AddMember(t, "removed", "caregiver")
	if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), removed.AccountID); err != nil {
		t.Fatal(err)
	}
	const ownerImport, unknownImport = "8a000000-0000-4000-8000-000000000001", "8a000000-0000-4000-8000-0000000000ff"
	h.Import(t, h.Owner, ownerImport, "Mira", "2026-01-01")
	s := newRouteStack(t, h, routeConfig{})
	notReady := newRouteStack(t, h, routeConfig{readiness: stackNotReady{}})
	exportOff := newRouteStack(t, h, routeConfig{exportOff: true})
	importOff := newRouteStack(t, h, routeConfig{importOff: true})
	limiterDown := newRouteStack(t, h, routeConfig{limiterFails: true})
	proxied := newRouteStack(t, h, routeConfig{trustedProxy: true})
	// A hosted node whose family lost its family_sync entitlement.
	entitledHarness := testkit.NewHosted(t)
	entitledHarness.Import(t, entitledHarness.Owner, ownerImport, "Mira", "2026-01-01")
	entitledHarness.AuthorityExec(t, `DELETE FROM family_entitlements WHERE family_id=$1`, entitledHarness.FamilyID)
	unentitled := newRouteStack(t, entitledHarness, routeConfig{})
	// An owner whose family is deleted, signed in again: a family_recovery
	// session.
	recoveryHarness := testkit.New(t)
	if _, err := recoveryHarness.Family.ScheduleDeletion(t.Context(), recoveryHarness.Owner, recoveryHarness.CommandID(), true); err != nil {
		t.Fatal(err)
	}
	if p, err := recoveryHarness.Reauthenticate("owner"); err != nil || p.SessionScope != "family_recovery" {
		t.Fatalf("recovery session %+v: %v", p, err)
	}
	recovery := newRouteStack(t, recoveryHarness, routeConfig{})
	one := limits.Policy{Count: 1, Window: time.Minute}
	limited := newRouteStack(t, h, routeConfig{importPull: one, exportAccount: one, liveDevice: one})
	ipLimited := newRouteStack(t, h, routeConfig{liveIP: one})
	expiredHarness := testkit.New(t)
	expired := newRouteStack(t, expiredHarness, routeConfig{})
	expiredHarness.Now = expiredHarness.Now.Add(time.Hour)
	unrefreshed := newRouteStack(t, testkit.NewWithUnrefreshedCaregiver(t), routeConfig{})

	resolve := func(i int) []byte { return mustJSON(t, testkit.ImportRequest(i, "Mira", "2026-01-01")) }
	export := func(id string) []byte {
		return []byte(`{"protocol_version":"v1","capabilities":["export-v1"],"request_id":"` + id + `"}`)
	}
	const exportID = "30000000-0000-4000-8000-00000000e001"
	malformed := []byte(`{"protocol_version":`)
	const statusRoute, resolveRoute, exportRoute, liveRoute = "/v1/family/import/" + ownerImport, "/v1/family/import/resolve", "/v1/export", "/v1/live"
	unknownStatus := "/v1/family/import/" + unknownImport

	// The limited stack's budgets, spent by requests that pass the charge:
	// the viewer's import (refused after it, read_only_caregiver), the
	// caregiver's export (owner_required), the owner's live attempt (no
	// socket: invalid_request).
	for _, spend := range []struct {
		subject, method, target string
		body                    []byte
		status                  int
	}{
		{"viewer", http.MethodPost, resolveRoute, resolve(40), http.StatusForbidden},
		{"caregiver", http.MethodPost, exportRoute, export(exportID), http.StatusForbidden},
		{"owner", http.MethodGet, liveRoute, nil, http.StatusBadRequest},
	} {
		if rec := limited.send(t, spend.subject, spend.method, spend.target, spend.body); rec.Code != spend.status {
			t.Fatalf("spend %s %s: %d %s", spend.method, spend.target, rec.Code, rec.Body.String())
		}
	}

	// policyStacks is a family that leaves the active state (or whose audit
	// chain fails) after its stack's limits were spent by the owner.
	policyStacks := func(state string) (*routeStack, *routeStack) {
		ph := testkit.New(t)
		ph.Import(t, ph.Owner, ownerImport, "Mira", "2026-01-01")
		ps := newRouteStack(t, ph, routeConfig{})
		pl := newRouteStack(t, ph, routeConfig{importPull: one, exportAccount: one, liveDevice: one})
		for _, spend := range []struct {
			method, target string
			body           []byte
			status         int
		}{
			{http.MethodGet, "/v1/family/import/" + unknownImport, nil, http.StatusNotFound},
			{http.MethodGet, "/v1/export/" + exportID, nil, http.StatusNotFound},
			{http.MethodGet, liveRoute, nil, http.StatusBadRequest},
		} {
			if rec := pl.send(t, "owner", spend.method, spend.target, spend.body); rec.Code != spend.status {
				t.Fatalf("%s: spend %s: %d %s", state, spend.target, rec.Code, rec.Body.String())
			}
		}
		ph.AuthorityExec(t, `UPDATE families SET `+state+` WHERE id=$1`, ph.FamilyID)
		return ps, pl
	}
	pending, pendingLimited := policyStacks("status='deletion_pending'")
	failed, failedLimited := policyStacks("chain_failed=true")

	// The live IP limit is charged before the credential is read: an
	// attempt with none spends it.
	if rec := ipLimited.do(http.MethodGet, liveRoute, nil, nil); rec.Code == http.StatusTooManyRequests {
		t.Fatalf("first live IP attempt: %d %s", rec.Code, rec.Body.String())
	}

	rows := []precedenceRow{}
	add := func(name string, on *routeStack, method, target string, body []byte, headers map[string]string, status int, code string) {
		rows = append(rows, precedenceRow{name, on, method, target, body, headers, status, code})
	}
	// Import.
	add("import: node not ready, malformed body", notReady, http.MethodPost, resolveRoute, malformed, notReady.routeHeaders("owner", resolveRoute), 503, "service_unavailable")
	add("import: no credential, malformed body", s, http.MethodPost, resolveRoute, malformed, withHeader(s.routeHeaders("owner", resolveRoute), "Authorization", ""), 401, "access_expired")
	add("import: malformed bearer", s, http.MethodPost, resolveRoute, resolve(1), withHeader(s.routeHeaders("owner", resolveRoute), "Authorization", "Bearer not-a-token"), 401, "session_revoked")
	add("import: removed caregiver, malformed body", s, http.MethodPost, resolveRoute, malformed, s.routeHeaders("removed", resolveRoute), 401, "session_revoked")
	add("import: removed caregiver, bad import id", s, http.MethodGet, "/v1/family/import/not-a-uuid", nil, s.routeHeaders("removed", statusRoute), 401, "session_revoked")
	add("import: expired access token, malformed body", expired, http.MethodPost, resolveRoute, malformed, expired.routeHeaders("owner", resolveRoute), 401, "access_expired")
	add("import: unrefreshed account session, malformed body", unrefreshed, http.MethodPost, resolveRoute, malformed, unrefreshed.routeHeaders("caregiver", resolveRoute), 401, "access_expired")
	add("import: account with no family, malformed body", s, http.MethodPost, resolveRoute, malformed, s.routeHeaders("outsider", resolveRoute), 403, "family_scope_required")
	add("import: unsupported protocol header, malformed body", s, http.MethodPost, resolveRoute, malformed, withHeader(s.routeHeaders("owner", resolveRoute), "X-Baby-Tracker-Protocol", "v2"), 400, "unsupported_protocol")
	add("import: missing capability", s, http.MethodGet, statusRoute, nil, withHeader(s.routeHeaders("owner", statusRoute), "X-Baby-Tracker-Capabilities", "import-resolver-v1"), 409, "unsupported_capability")
	add("import: bad import id", s, http.MethodGet, "/v1/family/import/not-a-uuid", nil, s.routeHeaders("owner", statusRoute), 400, "invalid_request")
	add("import: bad status query", s, http.MethodGet, statusRoute+"?page_token=", nil, s.routeHeaders("owner", statusRoute), 400, "invalid_request")
	add("import: resolve with a query", s, http.MethodPost, resolveRoute+"?x=1", resolve(1), s.routeHeaders("owner", resolveRoute), 400, "invalid_request")
	add("import: body over the limit", s, http.MethodPost, resolveRoute, append(resolve(1), bytes.Repeat([]byte(" "), h.Limits.SyncBodyBytes)...), s.routeHeaders("owner", resolveRoute), 413, "body_too_large")
	add("import: malformed body", s, http.MethodPost, resolveRoute, malformed, s.routeHeaders("owner", resolveRoute), 400, "invalid_request")
	add("import: conflict with a malformed body", s, http.MethodPost, statusRoute+"/resolve-conflict", malformed, s.routeHeaders("owner", statusRoute), 400, "invalid_request")
	add("import: unknown import", s, http.MethodGet, unknownStatus, nil, s.routeHeaders("owner", statusRoute), 404, "import_not_found")
	add("import: another member's import", s, http.MethodGet, statusRoute, nil, s.routeHeaders("caregiver", statusRoute), 403, "import_access_denied")
	add("import: viewer's import", s, http.MethodPost, resolveRoute, resolve(2), s.routeHeaders("viewer", resolveRoute), 403, "read_only_caregiver")
	add("import: viewer's import, pull limit spent", limited, http.MethodPost, resolveRoute, resolve(3), limited.routeHeaders("viewer", resolveRoute), 429, "rate_limited")
	add("import: family not active, malformed body", pending, http.MethodPost, resolveRoute, malformed, pending.routeHeaders("owner", resolveRoute), 400, "invalid_request")
	add("import: family not active, bad status query", pending, http.MethodGet, statusRoute+"?page_token=", nil, pending.routeHeaders("owner", statusRoute), 400, "invalid_request")
	add("import: family not active", pending, http.MethodGet, statusRoute, nil, pending.routeHeaders("owner", statusRoute), 403, "membership_revoked")
	add("import: family not active, resolve", pending, http.MethodPost, resolveRoute, resolve(4), pending.routeHeaders("owner", resolveRoute), 403, "membership_revoked")
	add("import: family not active, pull limit spent", pendingLimited, http.MethodGet, statusRoute, nil, pendingLimited.routeHeaders("owner", statusRoute), 403, "membership_revoked")
	add("import: family chain failed, malformed body", failed, http.MethodPost, resolveRoute, malformed, failed.routeHeaders("owner", resolveRoute), 400, "invalid_request")
	add("import: family chain failed", failed, http.MethodGet, statusRoute, nil, failed.routeHeaders("owner", statusRoute), 503, "service_unavailable")
	add("import: family chain failed, pull limit spent", failedLimited, http.MethodGet, statusRoute, nil, failedLimited.routeHeaders("owner", statusRoute), 503, "service_unavailable")
	// Export.
	statusTarget, downloadTarget := "/v1/export/"+exportID, "/v1/export/"+exportID+"/download"
	add("export: export off, no credential", exportOff, http.MethodPost, exportRoute, export(exportID), withHeader(exportOff.routeHeaders("owner", exportRoute), "Authorization", ""), 401, "access_expired")
	add("export: export off, removed caregiver", exportOff, http.MethodPost, exportRoute, export(exportID), exportOff.routeHeaders("removed", exportRoute), 401, "session_revoked")
	add("export: export off, account with no family", exportOff, http.MethodGet, "/v1/export/"+exportID, nil, exportOff.routeHeaders("outsider", exportRoute), 401, "access_expired")
	add("export: export off, unsupported protocol header", exportOff, http.MethodPost, exportRoute, malformed, withHeader(exportOff.routeHeaders("owner", exportRoute), "X-Baby-Tracker-Protocol", "v2"), 503, "service_unavailable")
	add("export: node not ready", notReady, http.MethodPost, exportRoute, export(exportID), notReady.routeHeaders("owner", exportRoute), 503, "service_unavailable")
	add("export: no credential, malformed body", s, http.MethodPost, exportRoute, malformed, withHeader(s.routeHeaders("owner", exportRoute), "Authorization", ""), 401, "access_expired")
	add("export: malformed bearer", s, http.MethodPost, exportRoute, export(exportID), withHeader(s.routeHeaders("owner", exportRoute), "Authorization", "Bearer not-a-token"), 401, "session_revoked")
	add("export: removed caregiver, malformed body", s, http.MethodPost, exportRoute, malformed, s.routeHeaders("removed", exportRoute), 401, "session_revoked")
	add("export: expired access token", expired, http.MethodGet, statusTarget, nil, expired.routeHeaders("owner", exportRoute), 401, "access_expired")
	add("export: unrefreshed account session", unrefreshed, http.MethodGet, statusTarget, nil, unrefreshed.routeHeaders("caregiver", exportRoute), 401, "access_expired")
	add("export: account with no family", s, http.MethodGet, statusTarget, nil, s.routeHeaders("outsider", exportRoute), 401, "access_expired")
	add("export: unsupported protocol header, malformed body", s, http.MethodPost, exportRoute, malformed, withHeader(s.routeHeaders("owner", exportRoute), "X-Baby-Tracker-Protocol", "v2"), 400, "unsupported_protocol")
	add("export: wrong capability", s, http.MethodPost, exportRoute, export(exportID), withHeader(s.routeHeaders("owner", exportRoute), "X-Baby-Tracker-Capabilities", "records-v1"), 409, "unsupported_capability")
	add("export: create with a query", s, http.MethodPost, exportRoute+"?x=1", export(exportID), s.routeHeaders("owner", exportRoute), 400, "invalid_request")
	add("export: body over the limit", s, http.MethodPost, exportRoute, append(export(exportID), bytes.Repeat([]byte(" "), 4<<10)...), s.routeHeaders("owner", exportRoute), 413, "body_too_large")
	add("export: malformed body", s, http.MethodPost, exportRoute, malformed, s.routeHeaders("owner", exportRoute), 400, "invalid_request")
	add("export: bad request id", s, http.MethodPost, exportRoute, export("not-a-uuid"), s.routeHeaders("owner", exportRoute), 400, "invalid_request")
	add("export: bad export id", s, http.MethodGet, "/v1/export/not-a-uuid", nil, s.routeHeaders("owner", exportRoute), 400, "invalid_request")
	add("export: status with a query", s, http.MethodGet, statusTarget+"?x=1", nil, s.routeHeaders("owner", exportRoute), 400, "invalid_request")
	add("export: unknown export", s, http.MethodGet, statusTarget, nil, s.routeHeaders("owner", exportRoute), 404, "export_not_found")
	add("export: unknown download", s, http.MethodGet, downloadTarget, nil, s.routeHeaders("owner", exportRoute), 404, "export_not_found")
	add("export: caregiver's export", s, http.MethodPost, exportRoute, export(exportID), s.routeHeaders("caregiver", exportRoute), 403, "owner_required")
	add("export: caregiver's export, account limit spent", limited, http.MethodPost, exportRoute, export(exportID), limited.routeHeaders("caregiver", exportRoute), 429, "rate_limited")
	add("export: caregiver's export, account limit spent, malformed body", limited, http.MethodPost, exportRoute, malformed, limited.routeHeaders("caregiver", exportRoute), 429, "rate_limited")
	add("export: family not active, malformed body", pending, http.MethodPost, exportRoute, malformed, pending.routeHeaders("owner", exportRoute), 400, "invalid_request")
	add("export: family not active, bad request id", pending, http.MethodPost, exportRoute, export("not-a-uuid"), pending.routeHeaders("owner", exportRoute), 400, "invalid_request")
	add("export: family not active", pending, http.MethodPost, exportRoute, export(exportID), pending.routeHeaders("owner", exportRoute), 503, "service_unavailable")
	add("export: family not active, status", pending, http.MethodGet, statusTarget, nil, pending.routeHeaders("owner", exportRoute), 503, "service_unavailable")
	add("export: family not active, download", pending, http.MethodGet, downloadTarget, nil, pending.routeHeaders("owner", exportRoute), 503, "service_unavailable")
	add("export: family not active, caregiver", pending, http.MethodPost, exportRoute, export(exportID), pending.routeHeaders("caregiver", exportRoute), 503, "service_unavailable")
	add("export: family not active, account limit spent", pendingLimited, http.MethodGet, statusTarget, nil, pendingLimited.routeHeaders("owner", exportRoute), 429, "rate_limited")
	add("export: family chain failed", failed, http.MethodPost, exportRoute, export(exportID), failed.routeHeaders("owner", exportRoute), 503, "service_unavailable")
	add("export: family chain failed, account limit spent", failedLimited, http.MethodGet, statusTarget, nil, failedLimited.routeHeaders("owner", exportRoute), 429, "rate_limited")
	// Live.
	add("live: node not ready", notReady, http.MethodGet, liveRoute, nil, notReady.routeHeaders("owner", liveRoute), 503, "service_unavailable")
	add("live: IP limit spent", ipLimited, http.MethodGet, liveRoute, nil, ipLimited.routeHeaders("owner", liveRoute), 429, "rate_limited")
	add("live: no credential", s, http.MethodGet, liveRoute, nil, withHeader(s.routeHeaders("owner", liveRoute), "Authorization", ""), 503, "service_unavailable")
	add("live: malformed bearer", s, http.MethodGet, liveRoute, nil, withHeader(s.routeHeaders("owner", liveRoute), "Authorization", "Bearer not-a-token"), 401, "session_revoked")
	add("live: token in the query", s, http.MethodGet, liveRoute+"?access_token=x", nil, s.routeHeaders("owner", liveRoute), 503, "service_unavailable")
	add("live: removed caregiver, unsupported protocol header", s, http.MethodGet, liveRoute, nil, withHeader(s.routeHeaders("removed", liveRoute), "X-Baby-Tracker-Protocol", "v2"), 401, "session_revoked")
	add("live: expired access token", expired, http.MethodGet, liveRoute, nil, expired.routeHeaders("owner", liveRoute), 401, "access_expired")
	add("live: unrefreshed account session, unsupported protocol header", unrefreshed, http.MethodGet, liveRoute, nil, withHeader(unrefreshed.routeHeaders("caregiver", liveRoute), "X-Baby-Tracker-Protocol", "v2"), 401, "access_expired")
	add("live: account with no family", s, http.MethodGet, liveRoute, nil, s.routeHeaders("outsider", liveRoute), 401, "access_expired")
	add("live: unsupported protocol header", s, http.MethodGet, liveRoute, nil, withHeader(s.routeHeaders("owner", liveRoute), "X-Baby-Tracker-Protocol", "v2"), 400, "unsupported_protocol")
	add("live: missing capability", s, http.MethodGet, liveRoute, nil, withHeader(s.routeHeaders("owner", liveRoute), "X-Baby-Tracker-Capabilities", "records-v1"), 409, "unsupported_capability")
	add("live: viewer without a socket", s, http.MethodGet, liveRoute, nil, s.routeHeaders("viewer", liveRoute), 400, "invalid_request")
	add("live: device limit spent", limited, http.MethodGet, liveRoute, nil, limited.routeHeaders("owner", liveRoute), 429, "rate_limited")
	add("live: family not active, unsupported protocol header", pending, http.MethodGet, liveRoute, nil, withHeader(pending.routeHeaders("owner", liveRoute), "X-Baby-Tracker-Protocol", "v2"), 400, "unsupported_protocol")
	add("live: family not active", pending, http.MethodGet, liveRoute, nil, pending.routeHeaders("owner", liveRoute), 403, "membership_revoked")
	add("live: family not active, device limit spent", pendingLimited, http.MethodGet, liveRoute, nil, pendingLimited.routeHeaders("owner", liveRoute), 403, "membership_revoked")
	add("live: family chain failed", failed, http.MethodGet, liveRoute, nil, failed.routeHeaders("owner", liveRoute), 503, "service_unavailable")
	add("live: family chain failed, device limit spent", failedLimited, http.MethodGet, liveRoute, nil, failedLimited.routeHeaders("owner", liveRoute), 503, "service_unavailable")
	// Fix round 1 rows (also recorded at BASE).
	badForward := func(headers map[string]string) map[string]string {
		return withHeader(headers, "X-Forwarded-For", "not-an-address")
	}
	add("import: no import service, removed caregiver", importOff, http.MethodGet, statusRoute, nil, importOff.routeHeaders("removed", statusRoute), 401, "session_revoked")
	add("import: no import service, account with no family", importOff, http.MethodGet, statusRoute, nil, importOff.routeHeaders("outsider", statusRoute), 403, "family_scope_required")
	add("import: no import service, unsupported protocol header", importOff, http.MethodPost, resolveRoute, malformed, withHeader(importOff.routeHeaders("owner", resolveRoute), "X-Baby-Tracker-Protocol", "v2"), 503, "service_unavailable")
	add("import: limiter error", limiterDown, http.MethodGet, statusRoute, nil, limiterDown.routeHeaders("owner", statusRoute), 503, "service_unavailable")
	add("import: limiter error, malformed body", limiterDown, http.MethodPost, resolveRoute, malformed, limiterDown.routeHeaders("owner", resolveRoute), 400, "invalid_request")
	add("import: no entitlement", unentitled, http.MethodGet, statusRoute, nil, unentitled.routeHeaders("owner", statusRoute), 403, "entitlement_required")
	add("import: no entitlement, malformed body", unentitled, http.MethodPost, resolveRoute, malformed, unentitled.routeHeaders("owner", resolveRoute), 400, "invalid_request")
	add("import: family recovery session", recovery, http.MethodGet, statusRoute, nil, recovery.routeHeaders("owner", statusRoute), 403, "family_scope_required")
	add("import: family recovery session, malformed body", recovery, http.MethodPost, resolveRoute, malformed, recovery.routeHeaders("owner", resolveRoute), 403, "family_scope_required")
	add("export: limiter error", limiterDown, http.MethodGet, statusTarget, nil, limiterDown.routeHeaders("owner", exportRoute), 503, "service_unavailable")
	add("export: limiter error, malformed body", limiterDown, http.MethodPost, exportRoute, malformed, limiterDown.routeHeaders("owner", exportRoute), 503, "service_unavailable")
	add("export: limiter error, unsupported protocol header", limiterDown, http.MethodPost, exportRoute, malformed, withHeader(limiterDown.routeHeaders("owner", exportRoute), "X-Baby-Tracker-Protocol", "v2"), 400, "unsupported_protocol")
	add("export: forwarded address unreadable", proxied, http.MethodPost, exportRoute, malformed, badForward(proxied.routeHeaders("owner", exportRoute)), 400, "invalid_request")
	add("export: forwarded address unreadable, removed caregiver", proxied, http.MethodPost, exportRoute, export(exportID), badForward(proxied.routeHeaders("removed", exportRoute)), 401, "session_revoked")
	add("export: forwarded address unreadable, unsupported protocol header", proxied, http.MethodPost, exportRoute, export(exportID), badForward(withHeader(proxied.routeHeaders("owner", exportRoute), "X-Baby-Tracker-Protocol", "v2")), 400, "unsupported_protocol")
	add("export: no entitlement", unentitled, http.MethodPost, exportRoute, export(exportID), unentitled.routeHeaders("owner", exportRoute), 503, "service_unavailable")
	add("export: no entitlement, bad request id", unentitled, http.MethodPost, exportRoute, export("not-a-uuid"), unentitled.routeHeaders("owner", exportRoute), 400, "invalid_request")
	add("export: family recovery session", recovery, http.MethodGet, statusTarget, nil, recovery.routeHeaders("owner", exportRoute), 401, "access_expired")
	add("live: limiter error", limiterDown, http.MethodGet, liveRoute, nil, limiterDown.routeHeaders("owner", liveRoute), 503, "service_unavailable")
	add("live: limiter error, unsupported protocol header", limiterDown, http.MethodGet, liveRoute, nil, withHeader(limiterDown.routeHeaders("owner", liveRoute), "X-Baby-Tracker-Protocol", "v2"), 400, "unsupported_protocol")
	add("live: forwarded address unreadable, removed caregiver", proxied, http.MethodGet, liveRoute, nil, badForward(proxied.routeHeaders("removed", liveRoute)), 400, "invalid_request")
	add("live: no entitlement", unentitled, http.MethodGet, liveRoute, nil, unentitled.routeHeaders("owner", liveRoute), 403, "entitlement_required")
	add("live: no entitlement, unsupported protocol header", unentitled, http.MethodGet, liveRoute, nil, withHeader(unentitled.routeHeaders("owner", liveRoute), "X-Baby-Tracker-Protocol", "v2"), 400, "unsupported_protocol")
	add("live: family recovery session", recovery, http.MethodGet, liveRoute, nil, recovery.routeHeaders("owner", liveRoute), 401, "access_expired")
	return rows
}
