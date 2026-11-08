package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
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

	"babytracker/server/internal/admission"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/liveactivity"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/pushtoken"
	"babytracker/server/internal/testkit"
)

// The routes that read a request body before their request transaction
// (I1). Since Phase 5 Task 19 a bearer they have not seen authenticate is
// checked before that read.
var unauthenticatedBodyRoutes = []struct{ method, target, capabilities string }{
	{http.MethodPost, "/v1/sync", "records-v1"},
	{http.MethodPost, "/v1/family/import/resolve", "records-v1,import-resolver-v1"},
	{http.MethodPost, "/v1/family/import/10000000-0000-4000-8000-000000000001/resolve-conflict", "records-v1,import-resolver-v1"},
	{http.MethodPut, pushTokenTarget, protocol.CapabilityPushTokenV1},
	{http.MethodDelete, pushTokenTarget, protocol.CapabilityPushTokenV1},
	{http.MethodPut, liveActivityDeviceRoute, protocol.CapabilityLiveActivityPushV1},
	{http.MethodPut, liveActivityActivityRoute, protocol.CapabilityLiveActivityPushV1},
}

// abuseStack is the production handlers of the sync, import and push-token
// routes over a harness, with the authority on a one-connection counted
// pool, the in-memory limiter, and the client address read from
// X-Forwarded-For sent from loopback (a trusted proxy), so one test client
// can speak as several addresses.
func abuseStack(t *testing.T, h *testkit.Harness, gate *admission.Controller) *stack {
	t.Helper()
	pool, wire := testkit.CountedPool(t, h.Runtime)
	guard := h.AuthorityOn(t, pool)
	limiter, err := limits.New(limits.Options{Pepper: [32]byte{13}, Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := limits.NewClientIPResolver([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("192.0.2.0/24")})
	if err != nil {
		t.Fatal(err)
	}
	policies := limits.DefaultPolicies()
	return &stack{h: h, wire: wire, handler: New(Options{Admission: gate, Readiness: stackReady{}, InstanceID: h.Info.InstanceID,
		RecoveryEpoch: h.Info.RecoveryEpoch, CanonicalOrigin: h.Info.CanonicalOrigin, SyncLimits: &h.Limits, SessionService: h.Auth,
		AppleAuthorizationMode: "identity_token_only", SyncService: testkit.SyncServiceOn(t, h, guard), ImportService: h.ImporterOn(guard),
		RequestAuthorizer: guard, BearerAuthenticator: guard, PushTokens: pushtoken.New(guard, func() time.Time { return h.Now }), Limiter: limiter, ClientIPResolver: resolver,
		SyncMutationDevicePolicy: policies[limits.ScopeSyncMutationDevice], SyncMutationAccountPolicy: policies[limits.ScopeSyncMutationAccount],
		SyncMutationFamilyPolicy: policies[limits.ScopeSyncMutationFamily], SyncPullDevicePolicy: policies[limits.ScopeSyncPullDevice],
		SyncPullAccountPolicy: policies[limits.ScopeSyncPullAccount], PushTokenDevicePolicy: policies[limits.ScopePushTokenDevice],
		PushTokenAccountPolicy: policies[limits.ScopePushTokenAccount], LiveActivityService: liveactivity.NewService(guard, liveActivityTopic),
		LiveActivityReady: func(context.Context) error { return nil }})}
}

// fakeBearer is a well-formed access token no session holds: 43 base64url
// characters, as validBearerSecret accepts.
func fakeBearer(t *testing.T) string {
	t.Helper()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(secret)
}

// I1: slow request bodies sent with made-up bearers from one address hold
// no admission slot, so the node keeps serving /v1/server and a real
// client's sync from another address.
func TestSlowFakeBearerBodiesCannotStarveAdmission(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	gate, err := admission.New(2, 0)
	if err != nil {
		t.Fatal(err)
	}
	s := abuseStack(t, h, gate)
	server := httptest.NewUnstartedServer(s.handler)
	server.Config.ReadHeaderTimeout = 5 * time.Second
	server.Config.ReadTimeout = 15 * time.Second
	server.Config.WriteTimeout = 30 * time.Second
	server.Start()
	defer server.Close()

	// Sixteen connections, eight times the admission total, each sending
	// its headers and one byte of a 100,000-byte body, then stalling.
	const attacker = "203.0.113.7"
	for i := range 16 {
		route := unauthenticatedBodyRoutes[i%len(unauthenticatedBodyRoutes)]
		conn, err := net.Dial("tcp", server.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer %s\r\nX-Baby-Tracker-Protocol: v1\r\n"+
			"X-Baby-Tracker-Capabilities: %s\r\nX-Forwarded-For: %s\r\nContent-Type: application/json\r\nContent-Length: 100000\r\n\r\n{",
			route.method, route.target, fakeBearer(t), route.capabilities, attacker)
	}
	time.Sleep(300 * time.Millisecond) // every stalled request is in its body read

	const client = "198.51.100.9"
	request, err := http.NewRequest(http.MethodGet, server.URL+"/v1/server", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Forwarded-For", client)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("/v1/server under slow fake-bearer bodies: %d %s", response.StatusCode, body)
	}

	raw, err := json.Marshal(h.SyncRequest(h.Cursor(h.FamilySequence(t)), nil))
	if err != nil {
		t.Fatal(err)
	}
	request, err = http.NewRequest(http.MethodPost, server.URL+"/v1/sync", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range s.headers("owner", "/v1/sync") {
		request.Header.Set(name, value)
	}
	request.Header.Set("X-Forwarded-For", client)
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("a real client's sync under slow fake-bearer bodies: %d %s", response.StatusCode, body)
	}
}

// countingBody is a request body that counts the bytes the server read.
type countingBody struct {
	reader io.Reader
	read   *atomic.Int64
}

func (b countingBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read.Add(int64(n))
	return n, err
}

// I1, then Phase 5 Task 19: a made-up bearer is refused 401 by the one
// credential check made before its body is read, so no byte of a body is
// read, and spends a unit of its address's failed-bearer budget; once the
// budget is spent the address is refused 429 before that check, with no
// statement run, on every route that reads a body before the credential.
func TestFakeBearerBodiesAreBoundedByTheFailedBearerIPLimit(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	s := abuseStack(t, h, nil)
	budget := limits.DefaultPolicies()[limits.ScopeBearerFailureIP].Count

	// A sync body of exactly SyncBodyBytes whose page token is one long
	// string, so the strict decode scans all of it.
	prefix, suffix := `{"protocol_version":"v1","capabilities":["records-v1"],"mutations":[],"page_token":"`, `"}`
	full := []byte(prefix + strings.Repeat("a", h.Limits.SyncBodyBytes-len(prefix)-len(suffix)) + suffix)
	send := func(address, target, method, bearer, capabilities string, body []byte) (*httptest.ResponseRecorder, int64, int) {
		var read atomic.Int64
		req := httptest.NewRequest(method, target, countingBody{bytes.NewReader(body), &read})
		req.RemoteAddr = address + ":4567"
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("X-Baby-Tracker-Protocol", "v1")
		req.Header.Set("X-Baby-Tracker-Capabilities", capabilities)
		rec := httptest.NewRecorder()
		s.wire.Reset()
		s.handler.ServeHTTP(rec, req)
		return rec, read.Load(), s.wire.Trips()
	}

	const attacker = "203.0.113.9"
	started := time.Now()
	read := int64(0)
	for i := range budget + 20 {
		rec, n, trips := send(attacker, "/v1/sync", http.MethodPost, fakeBearer(t), "records-v1", full)
		read += n
		if i < budget {
			if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "session_revoked" || rec.Header().Get("Connection") != "close" {
				t.Fatalf("fake bearer %d within the budget: %d %s", i, rec.Code, rec.Body.String())
			}
			if n != 0 || i > 0 && trips != 1 { // the first also prepares the statement

				t.Fatalf("fake bearer %d within the budget: read %d body bytes and ran %d round trips, want 0 and the one check", i, n, trips)
			}
			continue
		}
		if rec.Code != http.StatusTooManyRequests || errorCode(t, rec) != "rate_limited" || rec.Header().Get("Retry-After") == "" ||
			rec.Header().Get("Connection") != "close" || !strings.Contains(rec.Body.String(), `"server_instance_id"`) {
			t.Fatalf("fake bearer %d past the budget of %d: %d %s", i, budget, rec.Code, rec.Body.String())
		}
		if n != 0 || trips != 0 {
			t.Fatalf("fake bearer %d past the budget: read %d body bytes and ran %d round trips before refusing", i, n, trips)
		}
	}
	if read != 0 {
		t.Fatalf("read %d body bytes for %d fake bearers, want none", read, budget+20)
	}
	t.Logf("%d fake-bearer requests of %d bytes from one address: no body read, %v in all", budget+20, len(full), time.Since(started))

	// The refused address is refused on every body route before its body.
	for _, route := range unauthenticatedBodyRoutes {
		rec, n, trips := send(attacker, route.target, route.method, fakeBearer(t), route.capabilities, []byte(`{}`))
		if rec.Code != http.StatusTooManyRequests || n != 0 || trips != 0 {
			t.Fatalf("%s %s from the refused address: %d, read %d, %d round trips", route.method, route.target, rec.Code, n, trips)
		}
	}

	// Every body route counts its made-up bearers against the same budget.
	const mixed = "203.0.113.30"
	for i := range budget {
		route := unauthenticatedBodyRoutes[i%len(unauthenticatedBodyRoutes)]
		if rec, _, _ := send(mixed, route.target, route.method, fakeBearer(t), route.capabilities, []byte(`{}`)); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s fake bearer %d: %d %s", route.method, route.target, i, rec.Code, rec.Body.String())
		}
	}
	if rec, n, _ := send(mixed, "/v1/sync", http.MethodPost, fakeBearer(t), "records-v1", full); rec.Code != http.StatusTooManyRequests || n != 0 {
		t.Fatalf("after a budget spent across the routes: %d, read %d", rec.Code, n)
	}

	// Another address keeps its own budget, and a real client there syncs.
	const neighbour = "203.0.113.10"
	if rec, n, _ := send(neighbour, "/v1/sync", http.MethodPost, fakeBearer(t), "records-v1", full); rec.Code != http.StatusUnauthorized || n != 0 {
		t.Fatalf("another address's fake bearer: %d, read %d", rec.Code, n)
	}
	raw, err := json.Marshal(h.SyncRequest(h.Cursor(h.FamilySequence(t)), nil))
	if err != nil {
		t.Fatal(err)
	}
	if rec, _, _ := send(neighbour, "/v1/sync", http.MethodPost, h.AccessToken("owner"), "records-v1", raw); rec.Code != http.StatusOK {
		t.Fatalf("a real client at another address: %d %s", rec.Code, rec.Body.String())
	}
}

// Only a bearer that matches no session counts against its address: a real
// client's refusals (a malformed body, a held header refusal) and its
// successful requests never do, however many it sends.
func TestOnlyUnknownBearersCountAgainstTheAddress(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	s := abuseStack(t, h, nil)
	budget := limits.DefaultPolicies()[limits.ScopeBearerFailureIP].Count
	raw, err := json.Marshal(h.SyncRequest(h.Cursor(h.FamilySequence(t)), nil))
	if err != nil {
		t.Fatal(err)
	}
	send := func(bearer, protocolVersion string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/sync", bytes.NewReader(body))
		req.RemoteAddr = "203.0.113.20:1"
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("X-Baby-Tracker-Protocol", protocolVersion)
		req.Header.Set("X-Baby-Tracker-Capabilities", "records-v1")
		rec := httptest.NewRecorder()
		s.handler.ServeHTTP(rec, req)
		return rec
	}
	owner := h.AccessToken("owner")
	for i := range budget + 5 {
		if rec := send(owner, "v1", []byte(`{`)); rec.Code != http.StatusBadRequest {
			t.Fatalf("malformed body %d: %d %s", i, rec.Code, rec.Body.String())
		}
		if rec := send(owner, "v2", raw); rec.Code != http.StatusBadRequest || errorCode(t, rec) != "unsupported_protocol" {
			t.Fatalf("held header refusal %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	// One short of the budget in made-up bearers, then the real client is
	// still served; the next made-up bearer spends the budget.
	for i := range budget - 1 {
		if rec := send(fakeBearer(t), "v1", raw); rec.Code != http.StatusUnauthorized {
			t.Fatalf("fake bearer %d: %d", i, rec.Code)
		}
	}
	if rec := send(owner, "v1", raw); rec.Code != http.StatusOK {
		t.Fatalf("the real client after its refusals: %d %s", rec.Code, rec.Body.String())
	}
	if rec := send(fakeBearer(t), "v1", raw); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the budget's last fake bearer: %d", rec.Code)
	}
	if rec := send(owner, "v1", raw); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the address past its budget: %d %s", rec.Code, rec.Body.String())
	}
}

// Phase 5 Task 19: a bearer this node has not seen authenticate is checked
// (one pooled statement, under an admission slot) before its body is read;
// one that passed is remembered, so its next requests skip the check and
// cost what they cost before. A remembered bearer whose session was since
// revoked is refused by its request transaction, after its body, as before,
// and forgotten: its next request is refused by the check, its body unread.
func TestOnlyUnseenBearersAreCheckedBeforeTheirBody(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	s := abuseStack(t, h, nil)
	raw, err := json.Marshal(h.SyncRequest(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	send := func(subject string) (*httptest.ResponseRecorder, int64, int) {
		var read atomic.Int64
		req := httptest.NewRequest(http.MethodPost, "/v1/sync", countingBody{bytes.NewReader(raw), &read})
		req.RemoteAddr = "203.0.113.40:1"
		for name, value := range s.headers(subject, "/v1/sync") {
			req.Header.Set(name, value)
		}
		rec := httptest.NewRecorder()
		s.wire.Reset()
		s.handler.ServeHTTP(rec, req)
		return rec, read.Load(), s.wire.Trips()
	}
	if warm, _, _ := send("owner"); warm.Code != http.StatusOK { // prepares the statements
		t.Fatalf("warm-up: %d %s", warm.Code, warm.Body.String())
	}
	first, n, firstTrips := send("caregiver")
	if first.Code != http.StatusOK || n != int64(len(raw)) {
		t.Fatalf("a real client's first sync: %d, read %d: %s", first.Code, n, first.Body.String())
	}
	second, _, secondTrips := send("caregiver")
	if second.Code != http.StatusOK || firstTrips != secondTrips+1 {
		t.Fatalf("a real client's syncs: %d, %d then %d round trips, want the first one more (the check)", second.Code, firstTrips, secondTrips)
	}
	if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID); err != nil {
		t.Fatal(err)
	}
	revoked, n, _ := send("caregiver")
	if revoked.Code != http.StatusUnauthorized || errorCode(t, revoked) != "session_revoked" || n != int64(len(raw)) {
		t.Fatalf("a remembered bearer since revoked: %d, read %d: %s", revoked.Code, n, revoked.Body.String())
	}
	forgotten, n, trips := send("caregiver")
	if forgotten.Code != http.StatusUnauthorized || errorCode(t, forgotten) != "session_revoked" || n != 0 || trips != 1 {
		t.Fatalf("the revoked bearer again: %d, read %d, %d round trips: %s", forgotten.Code, n, trips, forgotten.Body.String())
	}
}

// Phase 5 Task 19: a made-up bearer is refused 401 by the credential check
// before the body-read bound and the body read, with the connection
// closed: its body stalling, it answers at once, where before the read ran
// into its deadline and answered 503 service_unavailable; from an address
// holding all its body reads, it answers 401, where before it answered 503
// server_busy. A known bearer there still meets the bound (503
// server_busy), as before.
func TestFakeBearersAreRefusedBeforeTheBodyReadBounds(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	s := abuseStack(t, h, nil)
	server := httptest.NewUnstartedServer(s.handler)
	server.Config.ReadTimeout = 15 * time.Second
	server.Start()
	t.Cleanup(server.Close) // after the stalled connections close (cleanups run last first)
	owner := h.AccessToken("owner")
	// stall sends a sync request's headers and the first byte of its body
	// from the loopback address and returns the open connection.
	stall := func(bearer string) net.Conn {
		conn, err := net.Dial("tcp", server.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		fmt.Fprintf(conn, "POST /v1/sync HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer %s\r\nX-Baby-Tracker-Protocol: v1\r\n"+
			"X-Baby-Tracker-Capabilities: records-v1\r\nContent-Length: 100000\r\n\r\n{", bearer)
		return conn
	}
	answer := func(conn net.Conn, label string) (*http.Response, string) {
		if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("%s: no answer within 3 s: %v", label, err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		return response, string(body)
	}

	started := time.Now()
	response, body := answer(stall(fakeBearer(t)), "a stalled fake-bearer body")
	if response.StatusCode != http.StatusUnauthorized || !strings.Contains(body, `"code":"session_revoked"`) || !response.Close {
		t.Fatalf("a stalled fake-bearer body: %d (close %v) %s", response.StatusCode, response.Close, body)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("answered after %v, want well before the 5 s body-read deadline", elapsed)
	}

	// The owner's bearer becomes known, then holds every body read of the
	// address.
	raw, err := json.Marshal(h.SyncRequest(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/sync", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range s.headers("owner", "/v1/sync") {
		request.Header.Set(name, value)
	}
	if response, err := server.Client().Do(request); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("the owner's sync: %v %v", response, err)
	} else {
		response.Body.Close()
	}
	for range defaultBodyReadsPerClient {
		stall(owner)
	}
	time.Sleep(200 * time.Millisecond) // every stalled request is in its body read
	response, body = answer(stall(fakeBearer(t)), "a fake bearer from a full address")
	if response.StatusCode != http.StatusUnauthorized || !strings.Contains(body, `"code":"session_revoked"`) {
		t.Fatalf("a fake bearer from an address holding its body reads: %d %s", response.StatusCode, body)
	}
	response, body = answer(stall(owner), "a known bearer from a full address")
	if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, `"code":"server_busy"`) {
		t.Fatalf("a known bearer from an address holding its body reads: %d %s", response.StatusCode, body)
	}
}
