package httpapi

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/admission"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
)

// databaseLikeSync is a stub sync service that, like the authority's request
// transaction, fails without a round trip once the request's context has
// ended (pgxpool's acquire returns the context's error, which the authority
// reports as service_unavailable).
type databaseLikeSync struct{ SyncService }

func (s databaseLikeSync) ServeSync(ctx context.Context, req authority.Request, r protocol.SyncRequest) (protocol.SyncResponse, error) {
	if err := ctx.Err(); err != nil {
		return protocol.SyncResponse{}, fmt.Errorf("authority: begin request: %w", errors.Join(authority.ErrUnavailable, err))
	}
	return s.SyncService.ServeSync(ctx, req, r)
}

// stubSyncOptions is a sync route over stubs (no database) that authorizes
// every request as one family owner and serves it with apply; a request
// whose context has ended fails as it would on the database.
func stubSyncOptions(t *testing.T, apply func(context.Context, access.Principal, protocol.SyncRequest) (protocol.SyncResponse, error)) Options {
	t.Helper()
	p := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001", Scope: "family",
		SessionScope: "family", SessionFamilyID: "30000000-0000-4000-8000-000000000001"}
	options := familyHandlerOptions(t, p, familyServiceStub{}, nil)
	effective := protocol.DefaultLimits()
	options.SyncLimits = &effective
	options.SyncService = databaseLikeSync{syncServiceStub{principal: p, apply: apply}}
	policies := limits.DefaultPolicies()
	options.SyncPullDevicePolicy, options.SyncPullAccountPolicy = policies[limits.ScopeSyncPullDevice], policies[limits.ScopeSyncPullAccount]
	return options
}

const stubSyncBody = `{"protocol_version":"v1","capabilities":["records-v1"],"cursor":null,"mutations":[],"page_token":null}`

func stubSync(t *testing.T, client *http.Client, url string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url+"/v1/sync", strings.NewReader(stubSyncBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer access")
	request.Header.Set("X-Baby-Tracker-Protocol", "v1")
	request.Header.Set("X-Baby-Tracker-Capabilities", "records-v1")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body)
}

// A body that stalls is cut at the body-read deadline, well before the
// server's ReadTimeout. net/http then cancels the request, so it never
// reaches its credential check and answers 503 service_unavailable (as a
// stalled body did under Phase 2's ReadTimeout); its body-read slot is then
// free.
func TestBodyReadDeadlineEndsASlowBodyAndFreesItsSlot(t *testing.T) {
	options := stubSyncOptions(t, func(context.Context, access.Principal, protocol.SyncRequest) (protocol.SyncResponse, error) {
		return protocol.SyncResponse{}, nil
	})
	options.BodyReads, _ = admission.NewPerKey(1, 1)
	options.BodyReadTimeout = 200 * time.Millisecond
	server := httptest.NewUnstartedServer(New(options))
	server.Config.ReadTimeout = 10 * time.Second
	server.Start()
	defer server.Close()

	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	started := time.Now()
	fmt.Fprint(conn, "POST /v1/sync HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer access\r\nX-Baby-Tracker-Protocol: v1\r\n"+
		"X-Baby-Tracker-Capabilities: records-v1\r\nContent-Length: 1000\r\n\r\n{")
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer to a stalled body within 3 s: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), `"code":"service_unavailable"`) {
		t.Fatalf("stalled body: %d %s", response.StatusCode, body)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("stalled body answered after %v", elapsed)
	}
	if status, body := stubSync(t, server.Client(), server.URL); status != http.StatusOK {
		t.Fatalf("the slot was not freed: %d %s", status, body)
	}
}

// The body-read deadline ends with the body: a request whose work outlasts
// it is not cancelled by the connection's idle read timing out.
func TestBodyReadDeadlineDoesNotOutliveTheBody(t *testing.T) {
	options := stubSyncOptions(t, func(ctx context.Context, _ access.Principal, _ protocol.SyncRequest) (protocol.SyncResponse, error) {
		select {
		case <-time.After(600 * time.Millisecond):
			return protocol.SyncResponse{}, nil
		case <-ctx.Done():
			return protocol.SyncResponse{}, ctx.Err()
		}
	})
	options.BodyReadTimeout = 200 * time.Millisecond
	server := httptest.NewUnstartedServer(New(options))
	server.Config.ReadTimeout = 10 * time.Second
	server.Start()
	defer server.Close()
	if status, body := stubSync(t, server.Client(), server.URL); status != http.StatusOK {
		t.Fatalf("a request outlasting the body-read deadline: %d %s", status, body)
	}
}

// One address holding its share of body reads is refused before its body
// is read, with the connection closed rather than drained; another address
// is not.
func TestBodyReadsAreCappedPerClientAddress(t *testing.T) {
	entered, finish := make(chan struct{}, 4), make(chan struct{})
	options := stubSyncOptions(t, func(context.Context, access.Principal, protocol.SyncRequest) (protocol.SyncResponse, error) {
		return protocol.SyncResponse{}, nil
	})
	options.BodyReads, _ = admission.NewPerKey(4, 1)
	handler := New(options)
	slow := func(remote string) *httptest.ResponseRecorder {
		read := false
		body := readFunc(func(p []byte) (int, error) {
			if !read {
				read = true
				entered <- struct{}{}
				<-finish
				return copy(p, stubSyncBody), nil
			}
			return 0, io.EOF
		})
		r := httptest.NewRequest(http.MethodPost, "/v1/sync", body)
		r.RemoteAddr = remote
		r.Header.Set("Authorization", "Bearer access")
		r.Header.Set("X-Baby-Tracker-Protocol", "v1")
		r.Header.Set("X-Baby-Tracker-Capabilities", "records-v1")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- slow("198.51.100.1:1") }()
	<-entered
	refusedRead := false
	r := httptest.NewRequest(http.MethodPost, "/v1/sync", readFunc(func([]byte) (int, error) { refusedRead = true; return 0, io.EOF }))
	r.RemoteAddr = "198.51.100.1:2"
	r.Header.Set("Authorization", "Bearer access")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	if rec.Code != http.StatusServiceUnavailable || errorCode(t, rec) != "server_busy" || rec.Header().Get("Connection") != "close" || refusedRead {
		t.Fatalf("second body read from one address: %d %s, Connection %q, body read %v", rec.Code, rec.Body.String(), rec.Header().Get("Connection"), refusedRead)
	}
	other := make(chan *httptest.ResponseRecorder, 1)
	go func() { other <- slow("198.51.100.2:1") }()
	<-entered
	close(finish)
	for _, rec := range []*httptest.ResponseRecorder{<-done, <-other} {
		if rec.Code != http.StatusOK {
			t.Fatalf("a body read within its address's share: %d %s", rec.Code, rec.Body.String())
		}
	}
}

// A client address is counted as itself (IPv4) or its /64 (IPv6); a
// forwarded chain that does not resolve is counted by the peer.
func TestBodyClientAddress(t *testing.T) {
	resolver, err := limits.NewClientIPResolver([]netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")})
	if err != nil {
		t.Fatal(err)
	}
	options := Options{ClientIPResolver: resolver}
	for _, c := range []struct{ remote, forwarded, want string }{
		{"203.0.113.5:1234", "", "203.0.113.5"},
		{"[2001:db8:1:2::9]:1234", "", "2001:db8:1:2::"},
		{"192.0.2.1:1234", "198.51.100.7", "198.51.100.7"},
		{"192.0.2.1:1234", "2001:db8:5:6:7::1", "2001:db8:5:6::"},
		{"192.0.2.1:1234", "not-an-address", "192.0.2.1"},
		{"203.0.113.5:1234", "198.51.100.7", "203.0.113.5"}, // an untrusted peer's header is ignored
		{"[::ffff:203.0.113.6]:1", "", "203.0.113.6"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/v1/sync", nil)
		r.RemoteAddr = c.remote
		if c.forwarded != "" {
			r.Header.Set("X-Forwarded-For", c.forwarded)
		}
		if got := bodyClientAddress(r, options); got != c.want {
			t.Fatalf("%s via %q: %s, want %s", c.remote, c.forwarded, got, c.want)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/sync", nil)
	r.RemoteAddr = "203.0.113.8:9"
	if got := bodyClientAddress(r, Options{}); got != "203.0.113.8" {
		t.Fatalf("no resolver: %s", got)
	}
}

// An address's body-read slot is held only for the read: while its first
// request is served, its next request's body is read too.
func TestBodyReadSlotIsFreedOnceTheBodyIsRead(t *testing.T) {
	serving, finish := make(chan struct{}, 2), make(chan struct{})
	options := stubSyncOptions(t, func(context.Context, access.Principal, protocol.SyncRequest) (protocol.SyncResponse, error) {
		serving <- struct{}{}
		<-finish
		return protocol.SyncResponse{}, nil
	})
	options.BodyReads, _ = admission.NewPerKey(1, 1)
	handler := New(options)
	send := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/sync", strings.NewReader(stubSyncBody))
		r.RemoteAddr = "198.51.100.3:1"
		r.Header.Set("Authorization", "Bearer access")
		r.Header.Set("X-Baby-Tracker-Protocol", "v1")
		r.Header.Set("X-Baby-Tracker-Capabilities", "records-v1")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec
	}
	results := make(chan *httptest.ResponseRecorder, 2)
	go func() { results <- send() }()
	<-serving
	go func() { results <- send() }()
	select {
	case <-serving:
	case rec := <-results:
		t.Fatalf("a second request while the first is served: %d %s", rec.Code, rec.Body.String())
	case <-time.After(2 * time.Second):
		t.Fatal("the second request never reached the service")
	}
	close(finish)
	for range 2 {
		if rec := <-results; rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	}
}

// stallBody sends a sync request's headers and the first byte of a
// 1,000-byte body over a fresh connection, then waits for the answer.
func stallBody(t *testing.T, address string) (int, string) {
	t.Helper()
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "POST /v1/sync HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer access\r\nX-Baby-Tracker-Protocol: v1\r\n"+
		"X-Baby-Tracker-Capabilities: records-v1\r\nContent-Length: 1000\r\n\r\n{")
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer to a stalled body within 3 s: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body)
}

// I1 (re-review): a body cut short spends a unit of its address's
// failed-bearer budget, so an address cannot stall bodies forever: once the
// budget is spent, its next request is refused 429 before its body is read.
// Another address is unaffected.
func TestStalledBodiesSpendTheFailedBearerBudget(t *testing.T) {
	options := stubSyncOptions(t, func(context.Context, access.Principal, protocol.SyncRequest) (protocol.SyncResponse, error) {
		return protocol.SyncResponse{}, nil
	})
	limiter, err := limits.New(limits.Options{Pepper: [32]byte{17}, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	options.Limiter = limiter
	options.BearerFailureIPPolicy = limits.Policy{Count: 3, Window: 10 * time.Minute}
	options.BodyReadTimeout = 100 * time.Millisecond
	handler := New(options)
	server := httptest.NewUnstartedServer(handler)
	server.Config.ReadTimeout = 10 * time.Second
	server.Start()
	defer server.Close()

	for i := range options.BearerFailureIPPolicy.Count {
		if status, body := stallBody(t, server.Listener.Addr().String()); status != http.StatusServiceUnavailable ||
			!strings.Contains(body, `"code":"service_unavailable"`) {
			t.Fatalf("stalled body %d within the budget: %d %s", i, status, body)
		}
	}
	if status, body := stallBody(t, server.Listener.Addr().String()); status != http.StatusTooManyRequests ||
		!strings.Contains(body, `"code":"rate_limited"`) {
		t.Fatalf("the stalled body past the budget: %d %s", status, body)
	}
	send := func(remote string) (*httptest.ResponseRecorder, int64) {
		var read atomic.Int64
		r := httptest.NewRequest(http.MethodPost, "/v1/sync", countingBody{strings.NewReader(stubSyncBody), &read})
		r.RemoteAddr = remote
		r.Header.Set("Authorization", "Bearer access")
		r.Header.Set("X-Baby-Tracker-Protocol", "v1")
		r.Header.Set("X-Baby-Tracker-Capabilities", "records-v1")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec, read.Load()
	}
	if rec, n := send("127.0.0.1:4567"); rec.Code != http.StatusTooManyRequests || errorCode(t, rec) != "rate_limited" || n != 0 {
		t.Fatalf("the address's next request: %d %s, read %d body bytes", rec.Code, rec.Body.String(), n)
	}
	if rec, _ := send("198.51.100.9:4567"); rec.Code != http.StatusOK {
		t.Fatalf("another address: %d %s", rec.Code, rec.Body.String())
	}
}

// A complete body that exceeds the route's bound is not charged: it meets
// its credential check as before (and, with a session, its 413).
func TestOversizedBodiesDoNotSpendTheFailedBearerBudget(t *testing.T) {
	options := stubSyncOptions(t, func(context.Context, access.Principal, protocol.SyncRequest) (protocol.SyncResponse, error) {
		return protocol.SyncResponse{}, nil
	})
	limiter, err := limits.New(limits.Options{Pepper: [32]byte{19}, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	options.Limiter = limiter
	options.BearerFailureIPPolicy = limits.Policy{Count: 1, Window: 10 * time.Minute}
	handler := New(options)
	oversized := strings.Repeat(" ", options.SyncLimits.SyncBodyBytes+10)
	// /v1/sync reads past its bound to its 4 MiB transport limit (a complete
	// read); the import route meets its 1 MiB transport limit (a
	// MaxBytesError). Neither is charged.
	for _, route := range []struct{ target, capabilities string }{
		{"/v1/sync", "records-v1"}, {"/v1/family/import/resolve", "records-v1,import-resolver-v1"},
	} {
		for i := range 3 {
			r := httptest.NewRequest(http.MethodPost, route.target, strings.NewReader(oversized))
			r.Header.Set("Authorization", "Bearer access")
			r.Header.Set("X-Baby-Tracker-Protocol", "v1")
			r.Header.Set("X-Baby-Tracker-Capabilities", route.capabilities)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			if rec.Code == http.StatusTooManyRequests || route.target == "/v1/sync" && rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("%s oversized body %d: %d %s", route.target, i, rec.Code, rec.Body.String())
			}
		}
	}
}
