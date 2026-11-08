package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/admission"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/diagnostics"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
)

// laneRefreshBody is a legacy refresh request presenting token.
func laneRefreshBody(token string) string {
	return `{"protocol_version":"v1","capabilities":["identity-v1"],"refresh_token":"` + token + `"}`
}

// laneRecoverableBody is a recoverable refresh request presenting token.
func laneRecoverableBody(token string) string {
	return strings.Replace(recoveryBody, strings.Repeat("B", 43), token, 1)
}

// laneToken is a well-formed refresh secret of one repeated character.
func laneToken(c string) string { return strings.Repeat(c, 43) }

// isServerBusy reports whether r is admission's refusal: 503 server_busy
// with Retry-After: 1.
func isServerBusy(t *testing.T, r *httptest.ResponseRecorder) bool {
	t.Helper()
	if r.Code != http.StatusServiceUnavailable {
		return false
	}
	var body protocol.ErrorResponse
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatalf("%d %s: %v", r.Code, r.Body, err)
	}
	if body.Error.Code != "server_busy" {
		return false
	}
	if r.Header().Get("Retry-After") != "1" || !body.Error.Retryable || body.Error.RetryAfterSeconds == nil || *body.Error.RetryAfterSeconds != 1 {
		t.Fatalf("server_busy without Retry-After 1: %v %s", r.Header(), r.Body)
	}
	return true
}

// laneStack is the router over stub services whose refreshes succeed,
// each issuing the child token next() returns, with one Sync slot and one
// Auth slot.
type laneStack struct {
	c        *admission.Controller
	metrics  *diagnostics.CapacityCollector
	handler  http.Handler
	children []string
}

func newLaneStack(t *testing.T, mutate func(*Options)) *laneStack {
	t.Helper()
	s := &laneStack{metrics: &diagnostics.CapacityCollector{}}
	var err error
	if s.c, err = admission.NewLanes(1, 0, 1, 0); err != nil {
		t.Fatal(err)
	}
	child := func() string {
		token := laneToken(string(rune('a' + len(s.children))))
		s.children = append(s.children, token)
		return token
	}
	bundle := func() auth.SessionBundle {
		return auth.SessionBundle{AccessToken: laneToken("Z"), RefreshToken: child(), ExpiresAt: time.Now(), ReauthenticateAt: time.Now()}
	}
	options := sessionHandlerOptions(t, sessionServiceStub{
		refreshSubject: func(context.Context, string) (auth.RefreshSubject, error) {
			return auth.RefreshSubject{AccountID: "account", DeviceID: "device"}, nil
		},
		refresh: func(context.Context, string) (auth.SessionBundle, error) { return bundle(), nil },
		exchange: func(context.Context, identity.ExchangeRequest) (auth.SessionBundle, error) {
			return bundle(), nil
		},
	}, "identity_token_only")
	options.Admission, options.AdmissionMetrics = s.c, s.metrics
	options.Limiter = limiterStub{
		take: func(context.Context, string, string, limits.Policy) (limits.Decision, error) {
			return limits.Decision{Allowed: true}, nil
		},
		takeMany: func(context.Context, []limits.Charge) (limits.Decision, error) {
			return limits.Decision{Allowed: true}, nil
		},
	}
	options.RecoveryEnabled = true
	options.RecoveryReadiness = accountGateFunc(func(context.Context) error { return nil })
	options.RecoveryService = recoveryServiceStub{
		resolve: func(context.Context, auth.RecoverableRefreshInput) (auth.RefreshSubject, error) {
			return auth.RefreshSubject{AccountID: "account", DeviceID: "device"}, nil
		},
		refresh: func(_ context.Context, input auth.RecoverableRefreshInput) (auth.RecoveredSessionBundle, error) {
			return auth.RecoveredSessionBundle{SessionBundle: bundle(), RefreshAttemptID: input.AttemptID}, nil
		},
	}
	if mutate != nil {
		mutate(&options)
	}
	s.handler = New(options)
	return s
}

func (s *laneStack) post(path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRecorder()
	s.handler.ServeHTTP(r, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return r
}

// hold takes class's only slot and returns its release.
func (s *laneStack) hold(t *testing.T, class admission.Class) func() {
	t.Helper()
	release, ok := s.c.TryAcquire(class)
	if !ok {
		t.Fatalf("class %d already full", class)
	}
	return release
}

// The routes that run before a caller is identified take the auth lane,
// not the shared total: with every Sync slot held they are still admitted,
// and with the lane full they alone are refused server_busy, while the
// Sync routes are admitted. The flood's two routes (challenge, and a
// refresh with a token this node never issued) are among them.
func TestAuthLaneSeparatesUnidentifiedRequestsFromSync(t *testing.T) {
	c, err := admission.NewLanes(1, 0, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	var metrics diagnostics.CapacityCollector
	options := Options{Admission: c, AdmissionMetrics: &metrics, AppleAuthorizationMode: "code_exchange_required", SessionService: sessionServiceStub{},
		Readiness: readinessFunc(func(context.Context) error { return nil })}
	handler := New(options)
	laneRequests := map[string]string{
		"/v1/auth/challenge":           `{}`,
		"/v1/auth/apple":               `{}`,
		"/v1/auth/apple/notifications": `{}`,
		"/v1/auth/refresh":             laneRefreshBody(laneToken("Q")),
		"/v1/auth/refresh-recoverable": laneRecoverableBody("garbage"),
	}
	if len(laneRequests) != len(authLaneRoutes)+len(refreshRoutes) {
		t.Fatalf("%d lane requests for %d lane routes", len(laneRequests), len(authLaneRoutes)+len(refreshRoutes))
	}
	for route := range authLaneRoutes {
		if _, ok := laneRequests[route.Pattern]; !ok || route.Method != http.MethodPost {
			t.Fatalf("lane route %v not exercised", route)
		}
	}
	for route := range refreshRoutes {
		if _, ok := laneRequests[route.Pattern]; !ok || route.Method != http.MethodPost {
			t.Fatalf("refresh route %v not exercised", route)
		}
	}
	serve := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, httptest.NewRequest(method, path, strings.NewReader(body)))
		return r
	}

	syncHeld, _ := c.TryAcquire(admission.Sync)
	for path, body := range laneRequests {
		if r := serve(http.MethodPost, path, body); isServerBusy(t, r) {
			t.Fatalf("a full total refused %s: %d %s", path, r.Code, r.Body)
		}
	}
	if r := serve(http.MethodGet, "/v1/server", ""); !isServerBusy(t, r) {
		t.Fatalf("/v1/server is not a sync route: %d %s", r.Code, r.Body)
	}
	syncHeld()

	authHeld, _ := c.TryAcquire(admission.Auth)
	for path, body := range laneRequests {
		if r := serve(http.MethodPost, path, body); !isServerBusy(t, r) {
			t.Fatalf("a full auth lane admitted %s: %d %s", path, r.Code, r.Body)
		}
	}
	for _, path := range []string{"/v1/server", "/invite", "/v1/account"} {
		if r := serve(http.MethodGet, path, ""); isServerBusy(t, r) {
			t.Fatalf("a full auth lane refused %s: %d %s", path, r.Code, r.Body)
		}
	}
	authHeld()

	counts := metrics.Snapshot().Admission
	lane := len(laneRequests)
	if got := counts[diagnostics.AuthAdmission]; got.Accepted != uint64(lane) || got.Released != uint64(lane) || got.Rejected != uint64(lane) || got.Active != 0 {
		t.Fatalf("auth lane metrics %+v", got)
	}
	if got := counts[diagnostics.SyncAdmission]; got.Accepted != 3 || got.Rejected != 1 {
		t.Fatalf("sync metrics %+v", got)
	}
}

// A refresh presenting a token this node issued (a refresh's or a
// sign-in's child) is tied to a known session and takes a Sync slot, so a
// full auth lane does not refuse it; the parent it replaced is forgotten.
// A token the node did not issue takes the lane.
func TestRefreshWithAnIssuedTokenTakesASyncSlot(t *testing.T) {
	for _, path := range []string{"/v1/auth/refresh", "/v1/auth/refresh-recoverable"} {
		t.Run(path, func(t *testing.T) {
			s := newLaneStack(t, nil)
			body := laneRefreshBody
			if path == "/v1/auth/refresh-recoverable" {
				body = laneRecoverableBody
			}
			// An unknown token, with the lane free: admitted in the lane.
			syncHeld := s.hold(t, admission.Sync)
			if r := s.post(path, body(laneToken("P"))); r.Code != http.StatusOK {
				t.Fatalf("an unknown token with the lane free: %d %s", r.Code, r.Body)
			}
			syncHeld()
			child := s.children[0]

			authHeld := s.hold(t, admission.Auth)
			if r := s.post(path, body(laneToken("U"))); !isServerBusy(t, r) {
				t.Fatalf("an unknown token took a sync slot: %d %s", r.Code, r.Body)
			}
			if r := s.post(path, body(child)); r.Code != http.StatusOK {
				t.Fatalf("an issued token with the lane full: %d %s", r.Code, r.Body)
			}
			grandchild := s.children[1]
			if r := s.post(path, body(child)); !isServerBusy(t, r) {
				t.Fatalf("the replaced parent kept a sync slot: %d %s", r.Code, r.Body)
			}
			if r := s.post(path, body(grandchild)); r.Code != http.StatusOK {
				t.Fatalf("the child's own child with the lane full: %d %s", r.Code, r.Body)
			}
			authHeld()

			// In Sync an issued token meets Sync's bound.
			syncHeld = s.hold(t, admission.Sync)
			if r := s.post(path, body(s.children[2])); !isServerBusy(t, r) {
				t.Fatalf("an issued token bypassed a full total: %d %s", r.Code, r.Body)
			}
			syncHeld()
			if got := s.metrics.Snapshot().Admission[diagnostics.SyncAdmission]; got.Accepted != 2 || got.Rejected != 1 {
				t.Fatalf("sync metrics %+v", got)
			}
			if got := s.metrics.Snapshot().Admission[diagnostics.AuthAdmission]; got.Accepted != 1 || got.Rejected != 2 {
				t.Fatalf("auth metrics %+v", got)
			}
		})
	}
}

// A sign-in's refresh token is known: its first refresh takes a Sync slot.
func TestSignInIssuesAKnownRefreshToken(t *testing.T) {
	s := newLaneStack(t, nil)
	exchange := `{"protocol_version":"v1","capabilities":["identity-v1"],"command_id":"11111111-1111-4111-8111-111111111111","challenge":"challenge","identity_token":"token","installation_id":"33333333-3333-4333-8333-333333333333"}`
	if r := s.post("/v1/auth/apple", exchange); r.Code != http.StatusOK {
		t.Fatalf("exchange: %d %s", r.Code, r.Body)
	}
	defer s.hold(t, admission.Auth)()
	if r := s.post("/v1/auth/refresh", laneRefreshBody(s.children[0])); r.Code != http.StatusOK {
		t.Fatalf("a sign-in's token with the lane full: %d %s", r.Code, r.Body)
	}
}

// The refresh token is read before admission under its own bound on reads
// in flight, apart from the body-first routes'. With that bound full the
// body is not read before admission and the request takes the lane, even
// with an issued token; the handler reads the body as before.
func TestRefreshPreReadBoundFullTakesTheLane(t *testing.T) {
	var gate *admission.PerKey
	s := newLaneStack(t, func(options *Options) {
		gate, _ = admission.NewPerKey(1, 1)
		options.AuthBodyReads = gate
	})
	if r := s.post("/v1/auth/refresh", laneRefreshBody(laneToken("P"))); r.Code != http.StatusOK {
		t.Fatalf("first refresh: %d %s", r.Code, r.Body)
	}
	child := s.children[0]
	readHeld, ok := gate.TryAcquire("192.0.2.1")
	if !ok {
		t.Fatal("pre-read bound not free")
	}
	defer s.hold(t, admission.Auth)()
	read := false
	body := readFunc(func([]byte) (int, error) { read = true; return 0, io.EOF })
	r := httptest.NewRecorder()
	s.handler.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/auth/refresh", body))
	if !isServerBusy(t, r) || read {
		t.Fatalf("a full pre-read bound: %d %s, body read %v", r.Code, r.Body, read)
	}
	// The same issued token, read once the bound frees, takes a Sync slot.
	readHeld()
	if r := s.post("/v1/auth/refresh", laneRefreshBody(child)); r.Code != http.StatusOK {
		t.Fatalf("an issued token with the lane full: %d %s", r.Code, r.Body)
	}
}

// The body read before admission is the body the handler judges: an
// oversized or malformed refresh is refused as before, never server_busy.
func TestRefreshPreReadReplaysTheBody(t *testing.T) {
	s := newLaneStack(t, nil)
	for _, body := range []string{
		laneRefreshBody(laneToken("P")) + strings.Repeat(" ", int(authBodyBytes)),
		`{"protocol_version":"v1"`,
		`not json`,
	} {
		if r := s.post("/v1/auth/refresh", body); r.Code != http.StatusBadRequest && r.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("%.40q: %d %s", body, r.Code, r.Body)
		}
	}
	if len(s.children) != 0 {
		t.Fatal("a refused body rotated")
	}
}
