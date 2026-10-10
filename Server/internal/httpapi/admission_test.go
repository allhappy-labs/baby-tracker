package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/admission"
	"babytracker/server/internal/diagnostics"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/realtime"
	"babytracker/server/internal/testkit"
	"github.com/coder/websocket"
)

func TestAdmissionAllWorkloadRoutesBeforeDependencies(t *testing.T) {
	c, _ := admission.New(1, 0)
	release, _ := c.TryAcquire(admission.Sync)
	defer release()
	var metrics diagnostics.CapacityCollector
	handler := New(Options{Admission: c, AdmissionMetrics: &metrics, AppleAuthorizationMode: "code_exchange_required", PushTokens: pushServiceFunc(nil), SessionService: sessionServiceStub{}, Readiness: readinessFunc(func(context.Context) error { return nil }),
		LiveActivityService: liveActivityServiceStub{}, LiveActivityUnregistration: liveActivityServiceStub{}, LiveActivityReady: func(context.Context) error { return nil }})
	for _, route := range RoutesForAuthorizationMode("code_exchange_required") {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(route.Method, route.Pattern, nil))
		if route.Pattern == "/healthz" || route.Pattern == "/readyz" {
			if response.Code != 200 {
				t.Fatalf("%s=%d", route.Pattern, response.Code)
			}
			continue
		}
		if bodyFirstRoutes[route] {
			// Takes its slot after its body (TestBodyFirstRoutesTakeAdmissionAfterTheBody):
			// a request with no credential is refused without one.
			if response.Code != 401 || response.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("%s %s: %d %s", route.Method, route.Pattern, response.Code, response.Body)
			}
			continue
		}
		var body protocol.ErrorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if response.Code != 503 || response.Header().Get("Retry-After") != "1" || body.Error.Code != "server_busy" || !body.Error.Retryable || body.Error.RetryAfterSeconds == nil || *body.Error.RetryAfterSeconds != 1 || body.Error.RequestID == "" {
			t.Fatalf("%s: %d %s", route.Pattern, response.Code, response.Body)
		}
	}
	release()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/invite", nil))
	if response.Code == 503 {
		t.Fatal("admission did not recover")
	}
}

// The routes that read their body before their credential take their
// admission slot only once the body is read: with the node full, a
// well-formed request is read in full and then refused server_busy, and a
// request that never sends its body holds no slot.
func TestBodyFirstRoutesTakeAdmissionAfterTheBody(t *testing.T) {
	if len(bodyFirstRoutes) != len(unauthenticatedBodyRoutes) {
		t.Fatalf("%d body-first routes, %d exercised", len(bodyFirstRoutes), len(unauthenticatedBodyRoutes))
	}
	c, _ := admission.New(1, 0)
	release, _ := c.TryAcquire(admission.Sync)
	defer release()
	syncLimits := protocol.DefaultLimits()
	sessions := sessionServiceStub{}
	handler := New(Options{Admission: c, AppleAuthorizationMode: "identity_token_only", Readiness: readinessFunc(func(context.Context) error { return nil }),
		SyncLimits: &syncLimits, SessionService: sessions, SyncService: syncServiceStub{}, PushTokens: pushServiceFunc(nil),
		LiveActivityService: liveActivityServiceStub{}, LiveActivityReady: func(context.Context) error { return nil },
		RequestAuthorizer: requestAuthorizerStub{}, Limiter: limiterStub{}})
	for _, route := range unauthenticatedBodyRoutes {
		read := false
		body := readFunc(func(p []byte) (int, error) {
			read = true
			return 0, io.EOF
		})
		request := httptest.NewRequest(route.method, route.target, body)
		request.Header.Set("Authorization", "Bearer "+strings.Repeat("A", 43))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var refusal protocol.ErrorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &refusal); err != nil {
			t.Fatal(err)
		}
		if response.Code != 503 || refusal.Error.Code != "server_busy" || response.Header().Get("Retry-After") != "1" || !read {
			t.Fatalf("%s %s with the node full: %d %s, body read %v", route.method, route.target, response.Code, response.Body, read)
		}
		if !bodyFirstRoutes[Route{Method: route.method, Pattern: routePattern(route.target)}] {
			t.Fatalf("%s %s is not a body-first route", route.method, route.target)
		}
	}
}

// readFunc is an io.Reader from a function.
type readFunc func([]byte) (int, error)

func (f readFunc) Read(p []byte) (int, error) { return f(p) }

// routePattern is the registered pattern of a body route's target.
func routePattern(target string) string {
	if strings.HasPrefix(target, "/v1/family/import/") && strings.HasSuffix(target, "/resolve-conflict") {
		return "/v1/family/import/{import_id}/resolve-conflict"
	}
	return target
}

func TestAdmissionCancellationHoldsUntilHandlerCompletes(t *testing.T) {
	c, _ := admission.New(1, 0)
	entered := make(chan struct{})
	canceled := make(chan struct{})
	finish := make(chan struct{})
	done := make(chan struct{})
	handler := WithAdmission(c, admission.Sync, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(canceled)
		<-finish
	}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		defer close(done)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/work", nil).WithContext(ctx))
	}()
	<-entered
	cancel()
	<-canceled
	if release, ok := c.TryAcquire(admission.Sync); ok {
		release()
		t.Error("cancellation freed token while work continued")
	}
	close(finish)
	<-done
	release, ok := c.TryAcquire(admission.Sync)
	if !ok {
		t.Fatal("completed cancellation leaked")
	}
	release()
}

func TestAdmissionPanicAndQuotaRelease(t *testing.T) {
	c, _ := admission.New(1, 0)
	for _, panicHandler := range []bool{true, false} {
		handler := withMiddleware(slog.New(slog.NewTextHandler(io.Discard, nil)), WithAdmission(c, admission.Sync, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if panicHandler {
				panic("controlled")
			}
			w.Header().Set("Retry-After", "60")
			writeRetryError(w, 429, "rate_limited", "Quota reached", requestIDFromContext(r.Context()), true, 60)
		})))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "/work", nil))
		want := 429
		if panicHandler {
			want = 500
		}
		if response.Code != want {
			t.Fatalf("status=%d", response.Code)
		}
		if !panicHandler && response.Header().Get("Retry-After") != "60" {
			t.Fatal("quota retry overwritten")
		}
		release, ok := c.TryAcquire(admission.Sync)
		if !ok {
			t.Fatal("handler leaked token")
		}
		release()
	}
}

func TestAdmissionMCPRouteAcquiresOnce(t *testing.T) {
	c, _ := admission.New(2, 1)
	var metrics diagnostics.CapacityCollector
	handler := WithAdmission(c, admission.MCP, &metrics, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if release, ok := c.TryAcquire(admission.MCP); ok {
			release()
			t.Error("MCP maximum not held")
		}
		release, ok := c.TryAcquire(admission.Sync)
		if !ok {
			t.Error("MCP consumed two slots")
		} else {
			release()
		}
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/mcp", nil))
	s := metrics.Snapshot().Admission[diagnostics.MCPAdmission]
	if s.Accepted != 1 || s.Active != 0 || s.Released != 1 {
		t.Fatalf("metrics=%+v", s)
	}
}

// A context timer cannot unblock a body read. Exercise a real transport deadline.
func TestAdmissionSlowBodyTimeoutAndRecovery(t *testing.T) {
	c, _ := admission.New(1, 0)
	entered := make(chan struct{}, 1)
	done := make(chan error, 1)
	handler := WithAdmission(c, admission.Sync, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		_, err := io.Copy(io.Discard, r.Body)
		done <- err
	}))
	server := httptest.NewUnstartedServer(handler)
	server.Config.ReadHeaderTimeout = 100 * time.Millisecond
	server.Config.ReadTimeout = 250 * time.Millisecond
	server.Config.WriteTimeout = time.Second
	server.Config.IdleTimeout = time.Second
	server.Config.MaxHeaderBytes = 1 << 20
	server.Start()
	defer server.Close()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprintf(conn, "POST /slow HTTP/1.1\r\nHost: test\r\nContent-Length: 100\r\n\r\nx")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("body reader did not enter")
	}
	response, err := server.Client().Get(server.URL + "/overload")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 503 {
		t.Fatalf("slow body not bounded: %d", response.StatusCode)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("slow body had no read timeout")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("transport did not unblock read")
	}
	response, err = http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	response, err = server.Client().Post(server.URL+"/recovery", "text/plain", strings.NewReader("done"))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("no recovery: %d", response.StatusCode)
	}
}

func admissionLiveOptions(t *testing.T, c *admission.Controller) (Options, http.Header) {
	t.Helper()
	h := testkit.New(t)
	hub, err := realtime.NewHub(t.Context(), realtime.HubOptions{Limits: h.Limits, Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := limits.NewClientIPResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	policies := limits.DefaultPolicies()
	options := Options{Admission: c, Readiness: readinessFunc(func(context.Context) error { return nil }), InstanceID: h.Info.InstanceID, RecoveryEpoch: h.Info.RecoveryEpoch, CanonicalOrigin: h.Info.CanonicalOrigin,
		SessionService: h.Auth, RequestAuthorizer: h.Authority, LiveHub: hub, Limiter: liveAllowLimiter(), ClientIPResolver: resolver,
		LiveAttemptIPPolicy: policies[limits.ScopeLiveAttemptIP], LiveAttemptDevicePolicy: policies[limits.ScopeLiveAttemptDevice], LiveAttemptAccountPolicy: policies[limits.ScopeLiveAttemptAccount], LiveAttemptFamilyPolicy: policies[limits.ScopeLiveAttemptFamily]}
	header := http.Header{"Authorization": []string{"Bearer " + h.AccessToken("owner")}, "X-Baby-Tracker-Protocol": []string{"v1"}, "X-Baby-Tracker-Capabilities": []string{"records-v1,live-hints-v1"}}
	return options, header
}

func TestAdmissionSuccessfulLiveUpgradeReleasesWhileSocketPersists(t *testing.T) {
	c, _ := admission.New(1, 0)
	options, header := admissionLiveOptions(t, c)
	server := httptest.NewServer(New(options))
	defer server.Close()
	conn, _, err := websocket.Dial(t.Context(), server.URL+"/v1/live", &websocket.DialOptions{HTTPHeader: header, Subprotocols: []string{realtime.Subprotocol}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	// Round-trip a hint to prove Accept and Bind finished without closing the socket.
	familyID := optionsFamilyID(t, options, header)
	// Dial may return just before the server binds its Hub reservation.
	deadline := time.Now().Add(2 * time.Second)
	for !options.LiveHub.HasRegisteredFamily(familyID) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !options.LiveHub.HasRegisteredFamily(familyID) {
		t.Fatal("successful upgrade did not bind Hub reservation")
	}
	options.LiveHub.Publish(t.Context(), realtime.Hint{FamilyID: familyID, Cursor: protocol.Cursor{ServerInstanceID: options.InstanceID, RecoveryEpoch: options.RecoveryEpoch, FamilyID: familyID, Sequence: 9}})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Get(server.URL + "/v1/server")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("live socket holds finite token: %d", response.StatusCode)
	}
	second, denial, err := websocket.Dial(ctx, server.URL+"/v1/live", &websocket.DialOptions{HTTPHeader: header, Subprotocols: []string{realtime.Subprotocol}})
	if second != nil {
		second.CloseNow()
	}
	if err == nil || denial == nil || denial.StatusCode != 429 {
		t.Fatalf("Hub lifetime limit lost: response=%v err=%v", denial, err)
	}
}

func optionsFamilyID(t *testing.T, options Options, header http.Header) string {
	t.Helper()
	principal, err := options.SessionService.Authenticate(t.Context(), strings.TrimPrefix(header.Get("Authorization"), "Bearer "))
	if err != nil {
		t.Fatal(err)
	}
	return principal.SessionFamilyID
}

type failingAdmissionHijacker struct {
	*httptest.ResponseRecorder
	entered, finish chan struct{}
}

func (w *failingAdmissionHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	close(w.entered)
	<-w.finish
	return nil, nil, errors.New("controlled hijack failure")
}

func TestAdmissionFailedUpgradeRetainsUntilAcceptReturns(t *testing.T) {
	c, _ := admission.New(1, 0)
	options, header := admissionLiveOptions(t, c)
	request := httptest.NewRequest("GET", "http://example.test/v1/live", nil)
	request.Header = header
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	request.Header.Set("Sec-WebSocket-Protocol", realtime.Subprotocol)
	w := &failingAdmissionHijacker{httptest.NewRecorder(), make(chan struct{}), make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); New(options).ServeHTTP(w, request) }()
	select {
	case <-w.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("did not reach hijack")
	}
	if w.Code != 101 {
		t.Errorf("expected pre-hijack status101, got%d", w.Code)
	}
	if release, ok := c.TryAcquire(admission.Sync); ok {
		release()
		t.Error("status101 released admission before successful Accept")
	}
	close(w.finish)
	<-done
	release, ok := c.TryAcquire(admission.Sync)
	if !ok {
		t.Fatal("failed upgrade leaked token")
	}
	release()
	// The failed Accept must release the Hub reservation as well.
	server := httptest.NewServer(New(options))
	defer server.Close()
	conn, _, err := websocket.Dial(t.Context(), server.URL+"/v1/live", &websocket.DialOptions{HTTPHeader: header, Subprotocols: []string{realtime.Subprotocol}})
	if err != nil {
		t.Fatal(err)
	}
	conn.CloseNow()
}
