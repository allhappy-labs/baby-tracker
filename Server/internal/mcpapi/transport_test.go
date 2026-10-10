package mcpapi

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/admission"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/familyread"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
)

const listTools = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

func TestNewHandlerRequiresEveryDependencyAndAPrivateOrigin(t *testing.T) {
	controller, _ := admission.New(8, 4)
	full := Options{Reader: &fakeReader{}, Authenticator: &fakeAuth{}, Admission: controller, Gate: testGate(), Quota: &fakeQuota{}, CanonicalOrigin: "https://mcp.example.ts.net"}
	if _, err := NewHandler(full); err != nil {
		t.Fatal(err)
	}
	broken := map[string]func(*Options){
		"reader":        func(o *Options) { o.Reader = nil },
		"authenticator": func(o *Options) { o.Authenticator = nil },
		"admission":     func(o *Options) { o.Admission = nil },
		"gate":          func(o *Options) { o.Gate = nil },
		"quota":         func(o *Options) { o.Quota = nil },
		"origin":        func(o *Options) { o.CanonicalOrigin = "" },
		"origin path":   func(o *Options) { o.CanonicalOrigin = "https://mcp.example.ts.net/mcp" },
		"origin scheme": func(o *Options) { o.CanonicalOrigin = "ftp://mcp.example.ts.net" },
		"origin user":   func(o *Options) { o.CanonicalOrigin = "https://u@mcp.example.ts.net" },
		"long timeout":  func(o *Options) { o.Timeout = 6 * time.Second },
		"neg timeout":   func(o *Options) { o.Timeout = -time.Second },
	}
	for name, mutate := range broken {
		options := full
		mutate(&options)
		if _, err := NewHandler(options); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestAuthenticationPrecedesEveryProtocolOperation(t *testing.T) {
	e := newEnv(t, nil)
	malformed := map[string]string{
		"missing":       "",
		"basic":         "Basic " + testCredential,
		"no token":      "Bearer ",
		"two tokens":    "Bearer " + testCredential + " " + testCredential,
		"folded":        "Bearer " + testCredential + ", Bearer " + testCredential,
		"tab":           "Bearer\t" + testCredential,
		"phone token":   "Bearer " + phoneToken,
		"tampered":      "Bearer " + testCredential[:len(testCredential)-1] + "B",
		"oversized":     "Bearer " + strings.Repeat("A", 4096),
		"double spaced": "Bearer  " + testCredential,
	}
	bodies := map[string]string{
		"initialize": `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`,
		"list":       listTools,
		"call":       toolCall(1, "list_babies", `{}`),
		"notify":     `{"jsonrpc":"2.0","method":"notifications/initialized"}`,
	}
	for authName, authorization := range malformed {
		for bodyName, body := range bodies {
			headers := legacyHeaders()
			headers["Authorization"] = authorization
			response := e.do(t, http.MethodPost, body, headers)
			if response.status != http.StatusUnauthorized || !strings.HasPrefix(response.header.Get("WWW-Authenticate"), "Bearer") ||
				errorCode(t, response) != "unauthorized" {
				t.Fatalf("%s/%s: %s", authName, bodyName, response)
			}
			requireNoStore(t, response)
			if strings.Contains(string(response.body), testCredential[20:]) || strings.Contains(string(response.body), "resource_metadata") {
				t.Fatalf("%s/%s leaked credential or OAuth metadata: %s", authName, bodyName, response.body)
			}
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		headers := legacyHeaders()
		delete(headers, "Authorization")
		if response := e.do(t, method, "", headers); response.status != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s: %s", method, response)
		}
	}
	// Duplicate Authorization headers are rejected even when both are valid.
	request, _ := http.NewRequest(http.MethodPost, e.url, strings.NewReader(listTools))
	for name, value := range legacyHeaders() {
		request.Header.Set(name, value)
	}
	request.Header.Add("Authorization", "Bearer "+testCredential)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("duplicate Authorization=%d", response.StatusCode)
	}
	if e.reader.calls.Load() != 0 || e.quota.calls.Load() != 0 {
		t.Fatalf("unauthenticated work: reader=%d quota=%d", e.reader.calls.Load(), e.quota.calls.Load())
	}
	// A lowercase scheme is still the Bearer scheme.
	headers := legacyHeaders()
	headers["Authorization"] = "bearer " + testCredential
	if response := e.post(t, listTools); response.status != http.StatusOK {
		t.Fatalf("valid bearer: %s", response)
	}
	if response := e.do(t, http.MethodPost, listTools, headers); response.status != http.StatusOK {
		t.Fatalf("lowercase bearer: %s", response)
	}
	e.assertIdle(t)
}

func TestAuthenticatorFailuresMapToSafeStatuses(t *testing.T) {
	cases := map[string]struct {
		err    error
		status int
		code   string
	}{
		"unauthorized": {authority.ErrIntegrationUnauthorized, 401, "unauthorized"},
		"unavailable":  {authority.ErrUnavailable, 503, "unavailable"},
		"driver":       {errors.New(`ERROR: relation "integration_grants" SELECT password=hunter2 (SQLSTATE 42P01)`), 503, "unavailable"},
		"deadline":     {context.DeadlineExceeded, 503, "timeout"},
	}
	for name, test := range cases {
		e := newEnv(t, nil)
		e.auth.err = test.err
		response := e.post(t, listTools)
		if response.status != test.status || errorCode(t, response) != test.code {
			t.Fatalf("%s: %s", name, response)
		}
		requireNoStore(t, response)
		if strings.Contains(string(response.body), "SQLSTATE") || strings.Contains(string(response.body), "hunter2") {
			t.Fatalf("%s leaked: %s", name, response.body)
		}
		if test.status == 503 && response.header.Get("Retry-After") == "" {
			t.Fatalf("%s: missing Retry-After", name)
		}
		e.assertIdle(t)
	}
}

func TestOriginAndHostMustMatchTheCanonicalPrivateOrigin(t *testing.T) {
	e := newEnv(t, nil)
	cases := map[string]struct {
		origin, host string
		status       int
	}{
		"missing origin":      {"", "", 200},
		"canonical origin":    {e.origin, "", 200},
		"foreign origin":      {"https://evil.example", "", 403},
		"null origin":         {"null", "", 403},
		"other port":          {"http://127.0.0.1:1", "", 403},
		"https variant":       {strings.Replace(e.origin, "http://", "https://", 1), "", 403},
		"origin with path":    {e.origin + "/x", "", 403},
		"foreign host":        {"", "evil.example", 403},
		"host without port":   {"", "127.0.0.1", 403},
		"canonical host":      {"", strings.TrimPrefix(e.origin, "http://"), 200},
		"forwarded host only": {"", "evil.example:80", 403},
	}
	for name, test := range cases {
		request, _ := http.NewRequest(http.MethodPost, e.url, strings.NewReader(listTools))
		for header, value := range legacyHeaders() {
			request.Header.Set(header, value)
		}
		if test.origin != "" {
			request.Header.Set("Origin", test.origin)
		}
		if test.host != "" {
			request.Host = test.host
		}
		request.Header.Set("X-Forwarded-Host", strings.TrimPrefix(e.origin, "http://"))
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != test.status || response.Header.Get("Access-Control-Allow-Origin") != "" || response.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d %v %s", name, response.StatusCode, response.Header, body)
		}
	}
	request, _ := http.NewRequest(http.MethodPost, e.url, strings.NewReader(listTools))
	for header, value := range legacyHeaders() {
		request.Header.Set(header, value)
	}
	request.Header.Add("Origin", e.origin)
	request.Header.Add("Origin", e.origin)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatalf("duplicate Origin=%d", response.StatusCode)
	}
	accepted := 0
	for _, test := range cases {
		if test.status == 200 {
			accepted++
		}
	}
	if int(e.auth.calls.Load()) != accepted {
		t.Fatalf("origin/host rejection must precede authentication: auth calls=%d want %d", e.auth.calls.Load(), accepted)
	}
}

func TestStatelessTransportSurface(t *testing.T) {
	e := newEnv(t, nil)
	get := e.do(t, http.MethodGet, "", legacyHeaders())
	if get.status != http.StatusMethodNotAllowed || get.header.Get("Allow") != "POST" {
		t.Fatalf("authenticated GET: %s", get)
	}
	requireNoStore(t, get)
	deleteHeaders := legacyHeaders()
	deleteHeaders["Mcp-Session-Id"] = "client-chosen"
	if response := e.do(t, http.MethodDelete, "", deleteHeaders); response.status != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE: %s", response)
	}
	headers := legacyHeaders()
	headers["Mcp-Session-Id"] = "client-chosen"
	list := e.do(t, http.MethodPost, listTools, headers)
	if list.status != 200 || !strings.HasPrefix(list.header.Get("Content-Type"), "application/json") ||
		!strings.Contains(string(list.body), `"ttlMs":0,"cacheScope":"private"`) {
		t.Fatalf("list: %.300s", list)
	}
	requireNoStore(t, list)
	notification := e.post(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if notification.status != http.StatusAccepted || len(notification.body) != 0 {
		t.Fatalf("notification: %s", notification)
	}
	requireNoStore(t, notification)
	for name, body := range map[string]string{
		"malformed": `{"jsonrpc":"2.0","id":1,"method":`,
		"batch":     `[` + listTools + `,` + toolCall(2, "list_babies", `{}`) + `]`,
		"empty":     ``,
	} {
		response := e.post(t, body)
		if response.status != http.StatusBadRequest {
			t.Fatalf("%s: %s", name, response)
		}
		requireNoStore(t, response)
	}
	if e.reader.calls.Load() != 0 {
		t.Fatalf("batch reached the reader %d times", e.reader.calls.Load())
	}
	// Supported versions only; unsupported legacy header versions are HTTP 400.
	for _, version := range []string{"2025-06-18", "2025-03-26", "2024-11-05", "garbage"} {
		headers := legacyHeaders()
		headers["Mcp-Protocol-Version"] = version
		response := e.do(t, http.MethodPost, listTools, headers)
		if response.status != http.StatusBadRequest {
			t.Fatalf("version %s: %s", version, response)
		}
		requireNoStore(t, response)
	}
	// After initialization a request must carry the negotiated version header.
	headers = legacyHeaders()
	delete(headers, "Mcp-Protocol-Version")
	if response := e.do(t, http.MethodPost, toolCall(3, "list_babies", `{}`), headers); response.status != http.StatusBadRequest {
		t.Fatalf("headerless call: %s", response)
	}
	e.assertIdle(t)
}

func TestRequestBodyIsBoundedAt32KiB(t *testing.T) {
	e := newEnv(t, nil)
	prefix := `{"jsonrpc":"2.0","id":"`
	suffix := `","method":"tools/list"}`
	exact := prefix + strings.Repeat("a", MaxRequestBytes-len(prefix)-len(suffix)) + suffix
	if len(exact) != MaxRequestBytes {
		t.Fatal(len(exact))
	}
	if response := e.post(t, exact); response.status != 200 {
		t.Fatalf("exactly 32 KiB: %d %.200s", response.status, response.body)
	}
	over := prefix + strings.Repeat("a", MaxRequestBytes-len(prefix)-len(suffix)+1) + suffix
	response := e.post(t, over)
	if response.status != http.StatusRequestEntityTooLarge || errorCode(t, response) != "request_too_large" {
		t.Fatalf("32 KiB + 1: %s", response)
	}
	requireNoStore(t, response)
	// Oversized tool arguments are refused before any read.
	args := `{"baby_id":"` + strings.Repeat("x", MaxRequestBytes) + `"}`
	if response := e.post(t, toolCall(1, "get_summary", args)); response.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized arguments: %d", response.status)
	}
	// Chunked bodies are counted the same way.
	request, _ := http.NewRequest(http.MethodPost, e.url, io.MultiReader(strings.NewReader(over)))
	for name, value := range legacyHeaders() {
		request.Header.Set(name, value)
	}
	request.ContentLength = -1
	chunked, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	chunked.Body.Close()
	if chunked.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked oversized=%d", chunked.StatusCode)
	}
	if e.reader.calls.Load() != 0 {
		t.Fatalf("oversized bodies reached the reader")
	}
	e.assertIdle(t)
}

// rawExchange writes a request on a raw TCP connection and reads one response.
func rawExchange(t *testing.T, e *env, wait time.Duration, write func(net.Conn)) (int, string, time.Duration) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(e.origin, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	started := time.Now()
	write(conn)
	_ = conn.SetReadDeadline(time.Now().Add(wait))
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0, err.Error(), time.Since(started)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	return response.StatusCode, response.Header.Get("Cache-Control") + " " + string(body), time.Since(started)
}

func rawHead(host string, extra string) string {
	return "POST /mcp HTTP/1.1\r\nHost: " + host + "\r\nContent-Type: application/json\r\nAccept: application/json, text/event-stream\r\n" +
		"Authorization: Bearer " + testCredential + "\r\nMcp-Protocol-Version: " + ProtocolLegacy + "\r\n" + extra
}

func TestSlowBodiesAreInterruptedByARealReadDeadline(t *testing.T) {
	const budget = 600 * time.Millisecond
	e := newEnv(t, func(o *Options) { o.Timeout = budget })
	host := strings.TrimPrefix(e.origin, "http://")
	cases := map[string]func(net.Conn){
		"content-length stall": func(c net.Conn) {
			fmt.Fprintf(c, "%sContent-Length: 100\r\n\r\n{\"jsonrpc\":", rawHead(host, ""))
		},
		"chunked stall": func(c net.Conn) {
			fmt.Fprintf(c, "%sTransfer-Encoding: chunked\r\n\r\n5\r\n{\"jso\r\n", rawHead(host, ""))
		},
		"trickle": func(c net.Conn) {
			fmt.Fprintf(c, "%sContent-Length: 40\r\n\r\n", rawHead(host, ""))
			for range 40 {
				if _, err := c.Write([]byte(" ")); err != nil {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
		},
	}
	for name, write := range cases {
		status, body, elapsed := rawExchange(t, e, 3*time.Second, write)
		if elapsed > budget+400*time.Millisecond {
			t.Fatalf("%s: response after %v exceeds %v budget", name, elapsed, budget)
		}
		if status != http.StatusRequestTimeout || !strings.HasPrefix(body, "no-store ") || !strings.Contains(body, "request_timeout") {
			t.Fatalf("%s: status=%d body=%q after %v", name, status, body, elapsed)
		}
	}
	if e.reader.calls.Load() != 0 {
		t.Fatal("stalled bodies reached the reader")
	}
	e.assertIdle(t)
}

func TestDefaultBudgetIsFiveSecondsFromHandlerEntry(t *testing.T) {
	if testing.Short() {
		t.Skip("five-second budget")
	}
	// Slow authentication consumes the same budget as the slow body: there
	// is no fresh budget after authentication.
	e := newEnv(t, nil)
	e.auth.after = func(context.Context) { time.Sleep(3 * time.Second) }
	host := strings.TrimPrefix(e.origin, "http://")
	status, body, elapsed := rawExchange(t, e, 7*time.Second, func(c net.Conn) {
		fmt.Fprintf(c, "%sContent-Length: 100\r\n\r\n{", rawHead(host, ""))
	})
	if status != http.StatusRequestTimeout || elapsed < 4*time.Second || elapsed > 5*time.Second+300*time.Millisecond {
		t.Fatalf("status=%d body=%q elapsed=%v", status, body, elapsed)
	}
	e.assertIdle(t)
}

func TestUnsupportedDeadlineControlFailsClosed(t *testing.T) {
	controller, _ := admission.New(8, 4)
	reader, auth := &fakeReader{}, &fakeAuth{}
	handler, err := NewHandler(Options{Reader: reader, Authenticator: auth, Admission: controller, Gate: testGate(), Quota: &fakeQuota{}, CanonicalOrigin: "http://example.test"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://example.test/mcp", strings.NewReader(listTools))
	for name, value := range legacyHeaders() {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Cache-Control") != "no-store" || auth.calls.Load() != 0 || reader.calls.Load() != 0 {
		t.Fatalf("recorder without deadlines: %d %v %s auth=%d", recorder.Code, recorder.Header(), recorder.Body, auth.calls.Load())
	}
}

func TestKeepAliveConnectionIsReusableAfterSuccessAndTimeout(t *testing.T) {
	const budget = 500 * time.Millisecond
	e := newEnv(t, func(o *Options) { o.Timeout = budget })
	var slow atomic.Bool
	e.reader.babies = func(ctx context.Context) (familyread.BabiesResult, error) {
		if slow.Load() {
			<-ctx.Done()
			return familyread.BabiesResult{}, ctx.Err()
		}
		return familyread.BabiesResult{Revision: 1, Babies: []protocol.CanonicalEntity{}}, nil
	}
	transport := &http.Transport{MaxIdleConnsPerHost: 1}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	send := func() (int, bool) {
		reused := false
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}
		request, _ := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), http.MethodPost, e.url, strings.NewReader(toolCall(1, "list_babies", `{}`)))
		for name, value := range legacyHeaders() {
			request.Header.Set(name, value)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return response.StatusCode, reused
	}
	if status, _ := send(); status != 200 {
		t.Fatalf("first=%d", status)
	}
	time.Sleep(budget + 200*time.Millisecond) // the previous route deadline must not poison the idle connection
	if status, reused := send(); status != 200 || !reused {
		t.Fatalf("second after success: status=%d reused=%v", status, reused)
	}
	// A different route on the same connection (no server WriteTimeout to
	// re-arm deadlines) must not inherit the expired MCP write deadline.
	time.Sleep(budget + 200*time.Millisecond)
	reused := false
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}
	other, _ := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), http.MethodGet, e.server.URL+"/other", nil)
	response, err := client.Do(other)
	if err != nil {
		t.Fatalf("next route on the reused connection: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || string(body) != "other" || !reused {
		t.Fatalf("next route: %d %q reused=%v", response.StatusCode, body, reused)
	}
	slow.Store(true)
	started := time.Now()
	if status, _ := send(); status != http.StatusServiceUnavailable || time.Since(started) > budget+400*time.Millisecond {
		t.Fatalf("timed-out call status=%d after %v", status, time.Since(started))
	}
	slow.Store(false)
	if status, _ := send(); status != 200 {
		t.Fatalf("after timeout=%d", status)
	}
	e.assertIdle(t)
}

func TestAdmissionGateAndQuotaRefusalsUseSafeStatuses(t *testing.T) {
	e, clock := newClockedEnv(t, nil)
	var held []func()
	for range 4 {
		release, ok := e.controller.TryAcquire(admission.MCP)
		if !ok {
			t.Fatal("setup")
		}
		held = append(held, release)
	}
	busy := e.post(t, listTools)
	if busy.status != 503 || busy.header.Get("Retry-After") != "1" || errorCode(t, busy) != "server_busy" || e.auth.calls.Load() != 0 {
		t.Fatalf("saturated: %s auth=%d", busy, e.auth.calls.Load())
	}
	requireNoStore(t, busy)
	for _, release := range held {
		release()
	}
	// One in-flight request per credential, for every authenticated request
	// (not only tools/call): refused before the body is read and before any
	// quota is charged, so an oversized body still gets the gate's 429.
	release, _ := e.gate.TryAcquire(testGrantID)
	for name, body := range map[string]string{"call": toolCall(7, "list_babies", `{}`), "list": listTools,
		"notification": `{"jsonrpc":"2.0","method":"notifications/initialized"}`, "oversized": strings.Repeat(" ", MaxRequestBytes+1)} {
		gated := e.post(t, body)
		if gated.status != 429 || errorCode(t, gated) != "concurrency_limited" || gated.header.Get("Retry-After") != "1" {
			t.Fatalf("gated %s: %s", name, gated)
		}
		requireNoStore(t, gated)
	}
	if e.quota.calls.Load() != 0 {
		t.Fatalf("concurrency refusal charged quota %d times", e.quota.calls.Load())
	}
	release()
	e.quota.decision = limits.Decision{Allowed: false, RetryAfter: 1500 * time.Millisecond}
	for name, body := range map[string]string{"list": listTools, "oversized": strings.Repeat(" ", MaxRequestBytes+1)} {
		limited := e.post(t, body)
		if limited.status != 429 || errorCode(t, limited) != "rate_limited" || limited.header.Get("Retry-After") != "2" {
			t.Fatalf("quota %s: %s", name, limited)
		}
		requireNoStore(t, limited)
	}
	// The repeat was refused from the refusal cache without another charge;
	// once the limiter's retry time passes, the full check runs again.
	if e.quota.calls.Load() != 1 {
		t.Fatalf("repeat quota refusal charged again: %d", e.quota.calls.Load())
	}
	clock.advance(1500 * time.Millisecond)
	e.quota.decision = limits.Decision{}
	e.quota.err = errors.New("rate limit table SQLSTATE 57014")
	unavailable := e.post(t, listTools)
	if unavailable.status != 503 || errorCode(t, unavailable) != "unavailable" || strings.Contains(string(unavailable.body), "SQLSTATE") {
		t.Fatalf("quota error: %s", unavailable)
	}
	e.quota.err = nil
	// Notifications are authenticated, admitted, gated and charged.
	before := e.quota.calls.Load()
	if response := e.post(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); response.status != 202 || e.quota.calls.Load() != before+1 {
		t.Fatalf("notification not charged exactly once: %s calls=%d", response, e.quota.calls.Load()-before)
	}
	// Every authenticated request is charged exactly once.
	before = e.quota.calls.Load()
	if response := e.post(t, toolCall(9, "list_babies", `{}`)); response.status != 200 || e.quota.calls.Load() != before+1 {
		t.Fatalf("call charged %d times: %s", e.quota.calls.Load()-before, response)
	}
	if e.reader.calls.Load() != 1 {
		t.Fatalf("refused calls reached the reader: %d", e.reader.calls.Load())
	}
	e.assertIdle(t)
}

// The keyed registry is keyed only by authenticated grant IDs: arbitrary
// invalid credentials never create an entry, and every path releases it.
func TestCredentialRegistryNeverGrowsWithInvalidTokens(t *testing.T) {
	e := newEnv(t, nil)
	var observed atomic.Int32
	e.auth.after = func(context.Context) { observed.Store(int32(max(int(observed.Load()), e.gate.Len()))) }
	for i := range 200 {
		token := fmt.Sprintf("btmcp1.%08x-0000-4000-8000-000000000000.%043d", i, i)
		request, _ := http.NewRequest(http.MethodPost, e.url, strings.NewReader(listTools))
		for name, value := range legacyHeaders() {
			request.Header.Set(name, value)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 401 || e.gate.Len() != 0 {
			t.Fatalf("invalid token %d: status=%d registry=%d", i, response.StatusCode, e.gate.Len())
		}
	}
	if response := e.post(t, listTools); response.status != 200 || e.gate.Len() != 0 || observed.Load() != 0 {
		t.Fatalf("valid request: %s registry=%d before-auth=%d", response, e.gate.Len(), observed.Load())
	}
}

func TestDisconnectCancelsTheReadAndReleasesSlotsForBothProtocols(t *testing.T) {
	for _, version := range []string{ProtocolLegacy, ProtocolCurrent} {
		t.Run(version, func(t *testing.T) {
			e := newEnv(t, nil)
			entered := make(chan struct{})
			finished := make(chan error, 1)
			e.reader.babies = func(ctx context.Context) (familyread.BabiesResult, error) {
				close(entered)
				// The fallback keeps a broken bridge from hanging the test.
				select {
				case <-ctx.Done():
				case <-time.After(3 * time.Second):
				}
				finished <- ctx.Err()
				return familyread.BabiesResult{}, ctx.Err()
			}
			body, headers := toolCall(1, "list_babies", `{}`), legacyHeaders()
			if version == ProtocolCurrent {
				body, headers = currentToolCall(1, "list_babies", `{}`), currentHeaders("tools/call", "list_babies")
			}
			ctx, cancel := context.WithCancel(context.Background())
			request, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.url, strings.NewReader(body))
			for name, value := range headers {
				request.Header.Set(name, value)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				response, err := (&http.Client{Transport: &http.Transport{}}).Do(request)
				if err == nil {
					response.Body.Close()
				}
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("read never started")
			}
			cancel()
			<-done
			select {
			case err := <-finished:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("read ended with %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("disconnect did not cancel the read")
			}
			e.assertIdle(t)
		})
	}
}

func TestDeadlineReachesToolReadsForBothProtocols(t *testing.T) {
	const budget = 500 * time.Millisecond
	for _, version := range []string{ProtocolLegacy, ProtocolCurrent} {
		t.Run(version, func(t *testing.T) {
			e := newEnv(t, func(o *Options) { o.Timeout = budget })
			deadlines := make(chan time.Time, 1)
			e.reader.babies = func(ctx context.Context) (familyread.BabiesResult, error) {
				deadline, _ := ctx.Deadline()
				deadlines <- deadline
				select {
				case <-ctx.Done():
				case <-time.After(3 * time.Second):
				}
				return familyread.BabiesResult{}, ctx.Err()
			}
			body, headers := toolCall(1, "list_babies", `{}`), legacyHeaders()
			if version == ProtocolCurrent {
				body, headers = currentToolCall(1, "list_babies", `{}`), currentHeaders("tools/call", "list_babies")
			}
			started := time.Now()
			response := e.do(t, http.MethodPost, body, headers)
			elapsed := time.Since(started)
			if response.status != http.StatusServiceUnavailable || errorCode(t, response) != "timeout" || elapsed > budget+300*time.Millisecond {
				t.Fatalf("%s after %v", response, elapsed)
			}
			requireNoStore(t, response)
			deadline := <-deadlines
			if deadline.IsZero() || deadline.After(started.Add(budget+200*time.Millisecond)) {
				t.Fatalf("tool deadline %v not within budget from %v", deadline, started)
			}
			e.assertIdle(t)
		})
	}
}

func TestPanickingToolReleasesSlotsWithSafeError(t *testing.T) {
	e := newEnv(t, nil)
	e.reader.babies = func(context.Context) (familyread.BabiesResult, error) { panic("pgx: password=hunter2") }
	response := e.post(t, toolCall(1, "list_babies", `{}`))
	if response.status != 503 || strings.Contains(string(response.body), "hunter2") {
		t.Fatalf("%s", response)
	}
	requireNoStore(t, response)
	e.assertIdle(t)
}

type panicQuota struct{}

func (panicQuota) Charge(context.Context, authority.IntegrationIdentity) (limits.Decision, error) {
	panic("limiter: password=hunter2")
}

type panicGate struct{}

func (g panicGate) TryAcquire(string) (func(), bool) { panic("registry: password=hunter2") }

type panicAuth struct{}

func (panicAuth) CheckIntegration(context.Context, string) (authority.IntegrationIdentity, error) {
	panic("authority: password=hunter2")
}

// A panic in authentication, the credential gate or the quota (outside the
// SDK middleware's recover) still yields the safe JSON-RPC 503 with no-store
// through the handler, and releases both the global and credential slots.
func TestPanickingAuthGateAndQuotaGetTheSafeNoStore503(t *testing.T) {
	for name, configure := range map[string]func(*Options){
		"quota": func(o *Options) { o.Quota = panicQuota{} },
		"gate":  func(o *Options) { o.Gate = panicGate{} },
		"auth":  func(o *Options) { o.Authenticator = panicAuth{} },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, configure)
			response := e.post(t, listTools)
			if response.status != 503 || errorCode(t, response) != "unavailable" || response.header.Get("Retry-After") != "1" ||
				strings.Contains(string(response.body), "hunter2") {
				t.Fatalf("%s", response)
			}
			requireNoStore(t, response)
			e.assertIdle(t)
			// The connection and handler stay usable afterwards.
			if again := e.post(t, listTools); again.status != 503 {
				t.Fatalf("second request: %s", again)
			}
		})
	}
}

func TestHTTP2StreamStallIsInterruptedByTheReadDeadline(t *testing.T) {
	const budget = 600 * time.Millisecond
	controller, _ := admission.New(8, 4)
	reader := &fakeReader{}
	server := httptest.NewUnstartedServer(nil)
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	handler, err := NewHandler(Options{Reader: reader, Authenticator: &fakeAuth{}, Admission: controller, Gate: testGate(), Quota: &fakeQuota{},
		CanonicalOrigin: server.URL, Timeout: budget})
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = handler
	pipeReader, pipeWriter := io.Pipe()
	defer pipeWriter.Close()
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/mcp", pipeReader)
	for name, value := range legacyHeaders() {
		request.Header.Set(name, value)
	}
	go func() { _, _ = pipeWriter.Write([]byte(`{"jsonrpc":"2.0",`)) }() // then stall
	started := time.Now()
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.ProtoMajor != 2 || response.StatusCode != http.StatusRequestTimeout || time.Since(started) > budget+400*time.Millisecond ||
		response.Header.Get("Cache-Control") != "no-store" || reader.calls.Load() != 0 {
		t.Fatalf("h2 stall: proto=%d status=%d after %v: %s", response.ProtoMajor, response.StatusCode, time.Since(started), body)
	}
	// The same HTTP/2 connection still serves a complete request.
	request, _ = http.NewRequest(http.MethodPost, server.URL+"/mcp", strings.NewReader(listTools))
	for name, value := range legacyHeaders() {
		request.Header.Set(name, value)
	}
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("h2 follow-up=%d", response.StatusCode)
	}
}

type readinessStub struct {
	calls atomic.Int32
	err   atomic.Pointer[error]
}

func (r *readinessStub) Check(context.Context) error {
	r.calls.Add(1)
	if err := r.err.Load(); err != nil {
		return *err
	}
	return nil
}

// A node that is not ready refuses every MCP request with the safe no-store
// JSON-RPC 503, before taking an admission slot or checking the credential;
// a ready node serves normally.
func TestNotReadyNodeRefusesBeforeAdmissionAndAuthentication(t *testing.T) {
	ready := &readinessStub{}
	e := newEnv(t, func(options *Options) { options.Readiness = ready })
	post := func() (int, http.Header, string) {
		t.Helper()
		request, _ := http.NewRequest(http.MethodPost, e.url, strings.NewReader(listTools))
		for header, value := range legacyHeaders() {
			request.Header.Set(header, value)
		}
		request.Header.Set("Authorization", "Bearer "+testCredential)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return response.StatusCode, response.Header, string(body)
	}
	if status, _, body := post(); status != http.StatusOK {
		t.Fatalf("ready node: %d %s", status, body)
	}
	notReady := errors.New("server_identity_mismatch")
	ready.err.Store(&notReady)
	authBefore := e.auth.calls.Load()
	held := make([]func(), 0, 4)
	for range 4 {
		release, ok := e.controller.TryAcquire(admission.MCP)
		if !ok {
			t.Fatal("setup: MCP admission unavailable")
		}
		held = append(held, release)
	}
	// Every MCP slot is held: a refusal that reached admission would be
	// server_busy; the readiness refusal comes first.
	status, header, body := post()
	for _, release := range held {
		release()
	}
	if status != http.StatusServiceUnavailable || header.Get("Cache-Control") != "no-store" ||
		!strings.Contains(body, `"code":"unavailable"`) || !strings.Contains(body, `"jsonrpc":"2.0"`) || strings.Contains(body, "server_identity_mismatch") {
		t.Fatalf("not-ready node: %d %v %s", status, header, body)
	}
	if e.auth.calls.Load() != authBefore {
		t.Fatal("not-ready node checked the credential")
	}
	ready.err.Store(nil)
	if status, _, body := post(); status != http.StatusOK {
		t.Fatalf("recovered node: %d %s", status, body)
	}
	e.assertIdle(t)
}
