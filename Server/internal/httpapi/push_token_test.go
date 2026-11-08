package httpapi

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/pushtoken"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
)

const pushTokenTarget = "/v1/devices/push-token"

// pushConfig varies a push-token stack.
type pushConfig struct {
	// off is a server without push: no PushTokens.
	off bool
	// device and account replace the default push-token policies.
	device, account limits.Policy
	readiness       Readiness
	authority       func(*authority.Options)
	limiter         limits.Limiter
	logs            *syncBuffer
}

// syncBuffer is a log sink safe for the handler's goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// newPushStack is the production push-token handlers over a harness: the
// service's authority on a one-connection counted pool, the in-memory
// limiter on the harness clock.
func newPushStack(t *testing.T, h *testkit.Harness, config pushConfig) *stack {
	t.Helper()
	pool, wire := testkit.CountedPool(t, h.Runtime)
	guard := h.AuthorityOn(t, pool)
	if config.authority != nil {
		guard = h.AuthorityOnWith(t, pool, config.authority)
	}
	policies := limits.DefaultPolicies()
	if config.device == (limits.Policy{}) {
		config.device = policies[limits.ScopePushTokenDevice]
	}
	if config.account == (limits.Policy{}) {
		config.account = policies[limits.ScopePushTokenAccount]
	}
	if config.readiness == nil {
		config.readiness = stackReady{}
	}
	if config.limiter == nil {
		limiter, err := limits.New(limits.Options{Pepper: [32]byte{11}, Now: func() time.Time { return h.Now }})
		if err != nil {
			t.Fatal(err)
		}
		config.limiter = limiter
	}
	var tokens PushTokenService
	if !config.off {
		tokens = pushtoken.New(guard, func() time.Time { return h.Now })
	}
	var logger *slog.Logger
	if config.logs != nil {
		logger = slog.New(slog.NewJSONHandler(config.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	handler, unlogged, logs := newLoggedStackHandlers(Options{Readiness: config.readiness, Logger: logger, InstanceID: h.Info.InstanceID,
		RecoveryEpoch: h.Info.RecoveryEpoch, CanonicalOrigin: h.Info.CanonicalOrigin, SyncLimits: &h.Limits, SessionService: h.Auth,
		AppleAuthorizationMode: "identity_token_only", PushTokens: tokens, BearerAuthenticator: guard, Limiter: config.limiter,
		PushTokenDevicePolicy: config.device, PushTokenAccountPolicy: config.account})
	if config.logs != nil {
		logs = config.logs
	}
	return &stack{h: h, wire: wire, handler: handler, unlogged: unlogged, logs: logs}
}

// pushHeaders is the route's protocol headers with subject's bearer token.
func (s *stack) pushHeaders(subject string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + s.h.AccessToken(subject), "X-Baby-Tracker-Protocol": "v1",
		"X-Baby-Tracker-Capabilities": protocol.CapabilityPushTokenV1}
}

// pushBody is a PUT body for token (raw bytes, sent as lowercase hex).
func pushBody(token []byte, environment string) []byte {
	return []byte(`{"protocol_version":"v1","capabilities":["push-token-v1"],"token":"` + hex.EncodeToString(token) +
		`","environment":"` + environment + `"}`)
}

func (s *stack) putToken(subject string, token []byte, environment string) *httptest.ResponseRecorder {
	return s.raw(http.MethodPut, pushTokenTarget, pushBody(token, environment), s.pushHeaders(subject))
}

func (s *stack) deleteToken(subject string) *httptest.ResponseRecorder {
	return s.raw(http.MethodDelete, pushTokenTarget, nil, s.pushHeaders(subject))
}

// pushRow is a device's stored token, read as the table owner under the
// authority flag (maintenance holds no privilege on device_push_tokens).
type pushRow struct {
	found              bool
	token              []byte
	environment, owner string
}

func storedPushToken(t *testing.T, h *testkit.Harness, deviceID string) pushRow {
	t.Helper()
	var row pushRow
	err := pgx.BeginFunc(t.Context(), h.Migrator, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `SELECT set_config('app.authority','1',true)`); err != nil {
			return err
		}
		err := tx.QueryRow(t.Context(), `SELECT token, environment::text, account_id::text FROM device_push_tokens WHERE device_id=$1`, deviceID).
			Scan(&row.token, &row.environment, &row.owner)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		row.found = err == nil
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var response protocol.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("%d %q: %v", rec.Code, rec.Body.String(), err)
	}
	return string(response.Error.Code)
}

func serverCapabilities(t *testing.T, handler http.Handler) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/server", nil))
	var info protocol.ServerInfo
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &info) != nil {
		t.Fatalf("/v1/server: %d %s", rec.Code, rec.Body.String())
	}
	return info.Capabilities
}

func TestPushTokenRouteContract(t *testing.T) {
	h := testkit.New(t)
	logs := &syncBuffer{}
	s := newPushStack(t, h, pushConfig{logs: logs})
	token := bytes.Repeat([]byte{0xa7, 0x3e}, 16)
	tokenHex := hex.EncodeToString(token)
	var bodies []string
	keep := func(rec *httptest.ResponseRecorder) *httptest.ResponseRecorder {
		bodies = append(bodies, rec.Body.String()+fmt.Sprint(rec.Header())) // the body and every header
		return rec
	}

	if !slices.Contains(serverCapabilities(t, s.handler), protocol.CapabilityPushTokenV1) {
		t.Fatal("a server with push does not advertise push-token-v1")
	}
	rec := keep(s.putToken("owner", token, "sandbox"))
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("PUT: %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	if row := storedPushToken(t, h, h.Owner.DeviceID); !row.found || !bytes.Equal(row.token, token) || row.environment != "sandbox" || row.owner != h.Owner.AccountID {
		t.Fatalf("stored %+v", row)
	}
	if rec := keep(s.putToken("owner", token, "sandbox")); rec.Code != http.StatusNoContent {
		t.Fatalf("the same PUT again: %d %s", rec.Code, rec.Body.String())
	}
	replacement := bytes.Repeat([]byte{0x5c}, 32)
	if rec := keep(s.putToken("owner", replacement, "production")); rec.Code != http.StatusNoContent {
		t.Fatalf("a replacement: %d %s", rec.Code, rec.Body.String())
	}
	if row := storedPushToken(t, h, h.Owner.DeviceID); !bytes.Equal(row.token, replacement) || row.environment != "production" {
		t.Fatalf("replaced %+v", row)
	}
	rec = keep(s.deleteToken("owner"))
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("DELETE: %d %q", rec.Code, rec.Body.String())
	}
	if row := storedPushToken(t, h, h.Owner.DeviceID); row.found {
		t.Fatalf("deleted token kept: %+v", row)
	}
	if rec := keep(s.deleteToken("owner")); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE again: %d %s", rec.Code, rec.Body.String())
	}

	// Refusals.
	oversized := append(pushBody(token, "sandbox"), bytes.Repeat([]byte(" "), 5<<10)...)
	if rec := keep(s.raw(http.MethodPut, pushTokenTarget, oversized, s.pushHeaders("caregiver"))); rec.Code != http.StatusRequestEntityTooLarge || errorCode(t, rec) != "body_too_large" {
		t.Fatalf("5 KiB body: %d %s", rec.Code, rec.Body.String())
	}
	wrong := withHeader(s.pushHeaders("caregiver"), "X-Baby-Tracker-Capabilities", "records-v1")
	if rec := keep(s.raw(http.MethodPut, pushTokenTarget, pushBody(token, "sandbox"), wrong)); rec.Code != http.StatusConflict || errorCode(t, rec) != "unsupported_capability" {
		t.Fatalf("records-v1: %d %s", rec.Code, rec.Body.String())
	}
	upper := []byte(strings.Replace(string(pushBody(token, "sandbox")), tokenHex, strings.ToUpper(tokenHex), 1))
	rec = keep(s.raw(http.MethodPut, pushTokenTarget, upper, s.pushHeaders("caregiver")))
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_push_token" || rec.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("malformed token: %d %s", rec.Code, rec.Body.String())
	}
	if row := storedPushToken(t, h, h.Caregiver.DeviceID); row.found {
		t.Fatalf("a refused PUT stored %+v", row)
	}
	removed := h.AddMember(t, "removed", "caregiver")
	if rec := s.putToken("removed", token, "sandbox"); rec.Code != http.StatusNoContent {
		t.Fatalf("member's PUT: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), removed.AccountID); err != nil {
		t.Fatal(err)
	}
	rec = keep(s.putToken("removed", token, "sandbox"))
	if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "session_revoked" || rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("revoked token: %d %s", rec.Code, rec.Body.String())
	}
	if rec := keep(s.deleteToken("removed")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked DELETE: %d %s", rec.Code, rec.Body.String())
	}

	// The device budget: 10 an hour, the 11th refused with Retry-After.
	limiterNow := h.Now
	hourly, err := limits.New(limits.Options{Pepper: [32]byte{13}, Now: func() time.Time { return limiterNow }})
	if err != nil {
		t.Fatal(err)
	}
	limited := newPushStack(t, h, pushConfig{logs: logs, limiter: hourly})
	for i := range 10 {
		if rec := keep(limited.putToken("caregiver", token, "sandbox")); rec.Code != http.StatusNoContent {
			t.Fatalf("PUT %d: %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
	rec = keep(limited.putToken("caregiver", token, "sandbox"))
	if rec.Code != http.StatusTooManyRequests || errorCode(t, rec) != "rate_limited" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("11th PUT: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	if rec := keep(limited.deleteToken("caregiver")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a DELETE past the device budget: %d %s", rec.Code, rec.Body.String())
	}
	if rec := keep(limited.putToken("owner", token, "sandbox")); rec.Code != http.StatusNoContent {
		t.Fatalf("another account's device was limited: %d %s", rec.Code, rec.Body.String())
	}
	_, tablet, err := h.NewDeviceSession("caregiver")
	if err != nil {
		t.Fatal(err)
	}
	tabletHeaders := withHeader(limited.pushHeaders("caregiver"), "Authorization", "Bearer "+tablet)
	if rec := keep(limited.raw(http.MethodPut, pushTokenTarget, pushBody(token, "sandbox"), tabletHeaders)); rec.Code != http.StatusNoContent {
		t.Fatalf("the account's other device was limited: %d %s", rec.Code, rec.Body.String())
	}
	limiterNow = limiterNow.Add(time.Hour)
	if rec := keep(limited.putToken("caregiver", token, "sandbox")); rec.Code != http.StatusNoContent {
		t.Fatalf("the next hour: %d %s", rec.Code, rec.Body.String())
	}

	// A server without push: the route is 404 and the capability is not
	// advertised.
	off := newPushStack(t, h, pushConfig{off: true, logs: logs})
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		rec := keep(off.raw(method, pushTokenTarget, pushBody(token, "sandbox"), off.pushHeaders("owner")))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s without push: %d %s", method, rec.Code, rec.Body.String())
		}
	}
	if slices.Contains(serverCapabilities(t, off.handler), protocol.CapabilityPushTokenV1) {
		t.Fatal("a server without push advertises push-token-v1")
	}

	// The token never leaves in a response or a log line.
	logged := logs.String()
	if logged == "" {
		t.Fatal("no log output captured")
	}
	for _, needle := range []string{tokenHex, strings.ToUpper(tokenHex), hex.EncodeToString(replacement)} {
		if strings.Contains(logged, needle) {
			t.Fatalf("the token was logged: %s", logged)
		}
		for _, body := range bodies {
			if strings.Contains(body, needle) {
				t.Fatalf("the token was echoed: %s", body)
			}
		}
	}
}

// A failing service is logged (a retryable 503), never with the token.
func TestPushTokenServiceFailureLogsNoToken(t *testing.T) {
	logs := &syncBuffer{}
	token := bytes.Repeat([]byte{0xd4}, 32)
	failing := pushServiceFunc(func(_ context.Context, _ authority.Request, environment string, got []byte) error {
		return errors.New("register " + environment + " " + hex.EncodeToString(got) + ": boom")
	})
	limiter, err := limits.New(limits.Options{Pepper: [32]byte{12}, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	digests := []authority.KeyedDigest{{KeyID: "k"}}
	handler := New(Options{Readiness: stackReady{}, Logger: slog.New(slog.NewJSONHandler(logs, nil)), PushTokens: failing, Limiter: limiter,
		SessionService: digestSessions{digests: digests}, AppleAuthorizationMode: "identity_token_only"})
	req := httptest.NewRequest(http.MethodPut, pushTokenTarget, bytes.NewReader(pushBody(token, "sandbox")))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Baby-Tracker-Protocol", "v1")
	req.Header.Set("X-Baby-Tracker-Capabilities", protocol.CapabilityPushTokenV1)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || errorCode(t, rec) != "service_unavailable" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	// The one fault line (logServerFault) names the cause by type, never
	// its text.
	if !strings.Contains(logs.String(), `"msg":"request failed"`) || !strings.Contains(logs.String(), `"route":"`+pushTokenRoute+`"`) ||
		!strings.Contains(logs.String(), `"cause":"*errors.errorString"`) || strings.Count(logs.String(), `"level":"ERROR"`) != 1 ||
		strings.Contains(logs.String(), hex.EncodeToString(token)) {
		t.Fatalf("logs: %s", logs.String())
	}
}

type pushServiceFunc func(context.Context, authority.Request, string, []byte) error

func (f pushServiceFunc) Register(ctx context.Context, req authority.Request, environment string, token []byte) error {
	return f(ctx, req, environment, token)
}

func (f pushServiceFunc) Unregister(ctx context.Context, req authority.Request) error {
	return f(ctx, req, "", nil)
}

// digestSessions answers AccessDigests only (the handler's one use before
// the service).
type digestSessions struct {
	SessionService
	digests []authority.KeyedDigest
}

func (s digestSessions) AccessDigests(string) ([]authority.KeyedDigest, error) { return s.digests, nil }

// pushPrecedenceGolden is every push-token refusal as a client sees it
// (status, the headers it reads, the body with its request ID blanked).
// The route is new, so its first recording is the reference
// (BT_UPDATE_PUSH_PRECEDENCE_GOLDEN=1 rewrites it).
const pushPrecedenceGolden = "testdata/push_token_precedence.json"

// TestPushTokenErrorPrecedence pins the route's refusal order, the sync
// seam's conventions:
//   - before any database work (0 round trips): readiness, no
//     Authorization header (access_expired), a malformed bearer
//     (session_revoked);
//   - for a bearer the stack has not seen authenticate, the credential
//     check before the body (Phase 5 Task 19; 1 round trip, the authority
//     statement, no transaction): authentication (session_revoked,
//     access_expired, epoch_changed, reauthentication_required), so these
//     never meet the body; a bearer that passes it goes on to the request
//     transaction (3 round trips in all);
//   - in the request transaction (2 round trips: BEGIN+authority, then one
//     ROLLBACK): authentication again, then Admit (a session
//     that is not a refreshed family session: family_scope_required or
//     access_expired; then the held header, query or body refusal), then
//     family policy, then the in-memory charge.
func TestPushTokenErrorPrecedence(t *testing.T) {
	h := testkit.New(t)
	removed := h.AddMember(t, "removed", "caregiver")
	if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), removed.AccountID); err != nil {
		t.Fatal(err)
	}
	s := newPushStack(t, h, pushConfig{})
	notReady := newPushStack(t, h, pushConfig{readiness: stackNotReady{}})
	expired := newPushStack(t, h, pushConfig{authority: func(o *authority.Options) { o.Now = func() time.Time { return h.Now.Add(time.Hour) } }})
	epoch := newPushStack(t, h, pushConfig{authority: func(o *authority.Options) { o.RecoveryEpoch++ }})
	deadline := newPushStack(t, h, pushConfig{authority: func(o *authority.Options) { o.Now = func() time.Time { return h.Now.Add(400 * 24 * time.Hour) } }})
	unrefreshed := newPushStack(t, testkit.NewWithUnrefreshedCaregiver(t), pushConfig{})
	limiterDown := newPushStack(t, h, pushConfig{limiter: limiterStub{
		take: func(context.Context, string, string, limits.Policy) (limits.Decision, error) {
			return limits.Decision{Allowed: true}, nil
		},
		takeMany: func(context.Context, []limits.Charge) (limits.Decision, error) {
			return limits.Decision{}, errors.New("limiter unavailable")
		},
	}})
	one := limits.Policy{Count: 1, Window: time.Hour}
	deviceLimited := newPushStack(t, h, pushConfig{device: one})
	accountLimited := newPushStack(t, h, pushConfig{account: one})
	valid := pushBody(bytes.Repeat([]byte{0x42}, 32), "sandbox")
	for _, spent := range []*stack{deviceLimited, accountLimited} {
		if rec := spent.putToken("owner", bytes.Repeat([]byte{0x42}, 32), "sandbox"); rec.Code != http.StatusNoContent {
			t.Fatalf("spend: %d %s", rec.Code, rec.Body.String())
		}
	}
	// A hosted family that lost its family_sync entitlement.
	hosted := testkit.NewHosted(t)
	hosted.AuthorityExec(t, `DELETE FROM family_entitlements WHERE family_id=$1`, hosted.FamilyID)
	unentitled := newPushStack(t, hosted, pushConfig{})
	// An owner whose family is deleted, signed in again: a family_recovery
	// session.
	recoveryHarness := testkit.New(t)
	if _, err := recoveryHarness.Family.ScheduleDeletion(t.Context(), recoveryHarness.Owner, recoveryHarness.CommandID(), true); err != nil {
		t.Fatal(err)
	}
	if p, err := recoveryHarness.Reauthenticate("owner"); err != nil || p.SessionScope != "family_recovery" {
		t.Fatalf("recovery session %+v: %v", p, err)
	}
	recovery := newPushStack(t, recoveryHarness, pushConfig{})
	// A family that left active, or whose audit chain failed, after the
	// owner spent the device budget on a second stack.
	policyStacks := func(state string) (*stack, *stack) {
		ph := testkit.New(t)
		ps := newPushStack(t, ph, pushConfig{})
		pl := newPushStack(t, ph, pushConfig{device: one})
		if rec := pl.putToken("owner", bytes.Repeat([]byte{0x42}, 32), "sandbox"); rec.Code != http.StatusNoContent {
			t.Fatalf("%s: spend: %d %s", state, rec.Code, rec.Body.String())
		}
		ph.AuthorityExec(t, `UPDATE families SET `+state+` WHERE id=$1`, ph.FamilyID)
		return ps, pl
	}
	pending, pendingLimited := policyStacks("status='deletion_pending'")
	failed, failedLimited := policyStacks("chain_failed=true")

	malformed := []byte(`{"protocol_version":`)
	badToken := pushBody(bytes.Repeat([]byte{0x42}, 15), "sandbox")
	staging := pushBody(bytes.Repeat([]byte{0x42}, 32), "staging")
	extraField := []byte(strings.Replace(string(valid), `"environment"`, `"device_id":"`+h.Caregiver.DeviceID+`","environment"`, 1))
	bodyCaps := []byte(strings.Replace(string(valid), `["push-token-v1"]`, `["records-v1"]`, 1))
	oversized := append(append([]byte(nil), valid...), bytes.Repeat([]byte(" "), protocol.PushTokenBodyBytes)...)
	type row struct {
		name    string
		on      *stack
		method  string
		target  string
		body    []byte
		headers map[string]string
		status  int
		code    string
		trips   int // exactly; see assertRefusalStatements
	}
	put, del := http.MethodPut, http.MethodDelete
	h0 := func(on *stack, subject string) map[string]string { return on.pushHeaders(subject) }
	rows := []row{
		{"node not ready, malformed body", notReady, put, pushTokenTarget, malformed, h0(notReady, "owner"), 503, "service_unavailable", 0},
		{"node not ready, delete", notReady, del, pushTokenTarget, nil, h0(notReady, "owner"), 503, "service_unavailable", 0},
		{"no credential, malformed body", s, put, pushTokenTarget, malformed, withHeader(h0(s, "owner"), "Authorization", ""), 401, "access_expired", 0},
		{"malformed bearer", s, put, pushTokenTarget, valid, withHeader(h0(s, "owner"), "Authorization", "Bearer not-a-token"), 401, "session_revoked", 0},
		{"removed caregiver, malformed body", s, put, pushTokenTarget, malformed, h0(s, "removed"), 401, "session_revoked", 1},
		{"removed caregiver, bad token", s, put, pushTokenTarget, badToken, h0(s, "removed"), 401, "session_revoked", 1},
		{"removed caregiver, delete", s, del, pushTokenTarget, nil, h0(s, "removed"), 401, "session_revoked", 1},
		{"expired access token, bad token", expired, put, pushTokenTarget, badToken, h0(expired, "owner"), 401, "access_expired", 1},
		{"another recovery epoch, bad token", epoch, put, pushTokenTarget, badToken, h0(epoch, "owner"), 401, "epoch_changed", 1},
		{"past the reauthentication deadline, bad token", deadline, put, pushTokenTarget, badToken, h0(deadline, "owner"), 401, "reauthentication_required", 1},
		{"unrefreshed account session, bad token", unrefreshed, put, pushTokenTarget, badToken, h0(unrefreshed, "caregiver"), 401, "access_expired", 3},
		{"account with no family, bad token", s, put, pushTokenTarget, badToken, h0(s, "outsider"), 403, "family_scope_required", 3},
		{"account with no family, delete", s, del, pushTokenTarget, nil, h0(s, "outsider"), 403, "family_scope_required", 2},
		{"family recovery session, bad token", recovery, put, pushTokenTarget, badToken, h0(recovery, "owner"), 403, "family_scope_required", 2},
		{"unsupported protocol header, malformed body", s, put, pushTokenTarget, malformed, withHeader(h0(s, "owner"), "X-Baby-Tracker-Protocol", "v2"), 400, "unsupported_protocol", 2},
		{"missing capability header", s, put, pushTokenTarget, valid, withHeader(h0(s, "owner"), "X-Baby-Tracker-Capabilities", ""), 409, "unsupported_capability", 2},
		{"records-v1 header", s, put, pushTokenTarget, valid, withHeader(h0(s, "owner"), "X-Baby-Tracker-Capabilities", "records-v1"), 409, "unsupported_capability", 2},
		{"excess capability header", s, put, pushTokenTarget, valid, withHeader(h0(s, "owner"), "X-Baby-Tracker-Capabilities", "push-token-v1,records-v1"), 409, "unsupported_capability", 2},
		{"records-v1 header, delete", s, del, pushTokenTarget, nil, withHeader(h0(s, "owner"), "X-Baby-Tracker-Capabilities", "records-v1"), 409, "unsupported_capability", 2},
		{"body capabilities differ from the header", s, put, pushTokenTarget, bodyCaps, h0(s, "owner"), 409, "unsupported_capability", 2},
		{"put with a query", s, put, pushTokenTarget + "?device_id=x", valid, h0(s, "owner"), 400, "invalid_request", 2},
		{"delete with a query", s, del, pushTokenTarget + "?device_id=x", nil, h0(s, "owner"), 400, "invalid_request", 2},
		{"put with an empty query", s, put, pushTokenTarget + "?", valid, h0(s, "owner"), 400, "invalid_request", 2},
		{"delete with an empty query", s, del, pushTokenTarget + "?", nil, h0(s, "owner"), 400, "invalid_request", 2},
		{"body over 4 KiB", s, put, pushTokenTarget, oversized, h0(s, "owner"), 413, "body_too_large", 2},
		{"malformed body", s, put, pushTokenTarget, malformed, h0(s, "owner"), 400, "invalid_request", 2},
		{"empty body", s, put, pushTokenTarget, nil, h0(s, "owner"), 400, "invalid_request", 2},
		{"a device id in the body", s, put, pushTokenTarget, extraField, h0(s, "owner"), 400, "invalid_request", 2},
		{"delete with a body", s, del, pushTokenTarget, valid, h0(s, "owner"), 400, "invalid_request", 2},
		{"15-byte token", s, put, pushTokenTarget, badToken, h0(s, "owner"), 400, "invalid_push_token", 2},
		{"environment staging", s, put, pushTokenTarget, staging, h0(s, "owner"), 400, "invalid_push_token", 2},
		// A held refusal ranks before family policy, and family policy
		// before the charge.
		{"family not active, bad token", pending, put, pushTokenTarget, badToken, h0(pending, "owner"), 400, "invalid_push_token", 2},
		{"family not active", pending, put, pushTokenTarget, valid, h0(pending, "owner"), 403, "membership_revoked", 2},
		{"family not active, delete", pending, del, pushTokenTarget, nil, h0(pending, "owner"), 403, "membership_revoked", 2},
		{"family not active, device limit spent", pendingLimited, put, pushTokenTarget, valid, h0(pendingLimited, "owner"), 403, "membership_revoked", 2},
		{"family chain failed, bad token", failed, put, pushTokenTarget, badToken, h0(failed, "owner"), 400, "invalid_push_token", 2},
		{"family chain failed", failed, put, pushTokenTarget, valid, h0(failed, "owner"), 503, "service_unavailable", 2},
		{"family chain failed, device limit spent", failedLimited, put, pushTokenTarget, valid, h0(failedLimited, "owner"), 503, "service_unavailable", 2},
		{"no entitlement", unentitled, put, pushTokenTarget, valid, h0(unentitled, "owner"), 403, "entitlement_required", 2},
		{"no entitlement, bad token", unentitled, put, pushTokenTarget, badToken, h0(unentitled, "owner"), 400, "invalid_push_token", 2},
		{"device limit spent", deviceLimited, put, pushTokenTarget, valid, h0(deviceLimited, "owner"), 429, "rate_limited", 2},
		{"device limit spent, delete", deviceLimited, del, pushTokenTarget, nil, h0(deviceLimited, "owner"), 429, "rate_limited", 2},
		{"device limit spent, bad token", deviceLimited, put, pushTokenTarget, badToken, h0(deviceLimited, "owner"), 400, "invalid_push_token", 2},
		{"account limit spent", accountLimited, put, pushTokenTarget, valid, h0(accountLimited, "owner"), 429, "rate_limited", 2},
		{"limiter error", limiterDown, put, pushTokenTarget, valid, h0(limiterDown, "owner"), 503, "service_unavailable", 2},
		{"limiter error, bad token", limiterDown, put, pushTokenTarget, badToken, h0(limiterDown, "owner"), 400, "invalid_push_token", 2},
	}
	for _, warm := range []*stack{s, expired, epoch, deadline, unrefreshed, recovery, pending, failed, unentitled, limiterDown, deviceLimited, accountLimited} {
		warm.deleteToken("owner") // prepares the authority statement
	}
	got := map[string]observed{}
	for _, c := range rows {
		t.Run(c.name, func(t *testing.T) {
			c.on.wire.Reset()
			rec := c.on.raw(c.method, c.target, c.body, c.headers)
			if rec.Code != c.status || errorCode(t, rec) != c.code {
				t.Fatalf("status %d code %q, want %d %q: %s", rec.Code, errorCode(t, rec), c.status, c.code, rec.Body.String())
			}
			if trips := c.on.wire.Trips(); trips != c.trips {
				t.Errorf("%d round trips, want %d: %q", trips, c.trips, c.on.wire.Statements())
			}
			c.on.assertRefusalStatements(t, c.name, c.trips)
			got[c.name] = observe(rec)
		})
	}
	path := filepath.FromSlash(pushPrecedenceGolden)
	if os.Getenv("BT_UPDATE_PUSH_PRECEDENCE_GOLDEN") == "1" {
		raw, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]observed
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(want) != len(rows) {
		t.Errorf("golden has %d rows, the test %d", len(want), len(rows))
	}
	for _, c := range rows {
		wantRaw, _ := json.Marshal(want[c.name])
		gotRaw, _ := json.Marshal(got[c.name])
		if !bytes.Equal(wantRaw, gotRaw) {
			t.Errorf("%s:\n got %s\nwant %s", c.name, gotRaw, wantRaw)
		}
	}
}
