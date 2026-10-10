package mcpapi

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/admission"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/familyread"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/requestlog"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newClockedEnv(t *testing.T, configure func(*Options)) (*env, *testClock) {
	t.Helper()
	return newClockedEnvServer(t, configure, nil)
}

func newClockedEnvServer(t *testing.T, configure func(*Options), adjust func(*http.Server)) (*env, *testClock) {
	t.Helper()
	clock := &testClock{now: testNow}
	e := newEnvServer(t, func(o *Options) {
		o.Now = clock.Now
		if configure != nil {
			configure(o)
		}
	}, adjust)
	return e, clock
}

func holdAllMCPSlots(t *testing.T, e *env) func() {
	t.Helper()
	var held []func()
	for range 4 {
		release, ok := e.controller.TryAcquire(admission.MCP)
		if !ok {
			t.Fatal("setup: MCP slot unavailable")
		}
		held = append(held, release)
	}
	return func() {
		for _, release := range held {
			release()
		}
	}
}

// A credential that was authenticated and refused by its quota is refused
// again, with the same safe contract, before global admission and before
// authentication, until the limiter window it was given ends.
func TestRepeatQuotaRefusalIsRefusedBeforeAdmissionAndAuthentication(t *testing.T) {
	e, clock := newClockedEnv(t, nil)
	e.quota.decision = limits.Decision{Allowed: false, RetryAfter: 30 * time.Second}
	first := e.post(t, listTools)
	if first.status != 429 || errorCode(t, first) != "rate_limited" || first.header.Get("Retry-After") != "30" {
		t.Fatalf("authenticated refusal: %s", first)
	}
	if e.auth.calls.Load() != 1 || e.quota.calls.Load() != 1 {
		t.Fatalf("auth=%d quota=%d", e.auth.calls.Load(), e.quota.calls.Load())
	}
	// Every global MCP slot is busy: a cached refusal still needs none.
	releaseSlots := holdAllMCPSlots(t, e)
	repeat := e.post(t, toolCall(5, "list_babies", `{}`))
	if repeat.status != 429 || string(repeat.body) != string(first.body) || repeat.header.Get("Retry-After") != "30" ||
		repeat.header.Get("Content-Type") != first.header.Get("Content-Type") {
		t.Fatalf("cached refusal differs from the authenticated one:\n%s\n%s", first, repeat)
	}
	requireNoStore(t, repeat)
	if e.auth.calls.Load() != 1 || e.quota.calls.Load() != 1 || e.reader.calls.Load() != 0 {
		t.Fatalf("cached refusal reached auth=%d quota=%d reader=%d", e.auth.calls.Load(), e.quota.calls.Load(), e.reader.calls.Load())
	}
	clock.advance(12500 * time.Millisecond)
	if later := e.post(t, listTools); later.status != 429 || later.header.Get("Retry-After") != "18" || e.auth.calls.Load() != 1 {
		t.Fatalf("remaining window: %s auth=%d", later, e.auth.calls.Load())
	}
	releaseSlots()
	// When the window the limiter gave has passed, the full check runs again.
	clock.advance(17500 * time.Millisecond)
	e.quota.decision = limits.Decision{}
	if after := e.post(t, toolCall(6, "list_babies", `{}`)); after.status != 200 || e.auth.calls.Load() != 2 || e.quota.calls.Load() != 2 {
		t.Fatalf("after window: %s auth=%d quota=%d", after, e.auth.calls.Load(), e.quota.calls.Load())
	}
	e.assertIdle(t)
}

// Only the exact presented credential string is remembered: other tokens,
// including ones naming the same grant, still get the full check.
func TestRefusalCacheIsKeyedByTheExactCredentialNotTheGrant(t *testing.T) {
	e, _ := newClockedEnv(t, nil)
	e.quota.decision = limits.Decision{Allowed: false, RetryAfter: 30 * time.Second}
	if first := e.post(t, listTools); first.status != 429 {
		t.Fatalf("setup: %s", first)
	}
	sameGrant := "btmcp1." + testGrantID + ".BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	for _, token := range []string{sameGrant, testCredential + "A", strings.ToLower(testCredential)} {
		headers := legacyHeaders()
		headers["Authorization"] = "Bearer " + token
		before := e.auth.calls.Load()
		if response := e.do(t, http.MethodPost, listTools, headers); response.status != 401 || e.auth.calls.Load() != before+1 {
			t.Fatalf("%q was not fully checked: %s", token, response)
		}
	}
	if e.handler.refusals.len() != 1 {
		t.Fatalf("unauthenticated tokens created entries: %d", e.handler.refusals.len())
	}
}

// A cached refusal is still a refusal after revocation, and only for the
// rest of the window the limiter gave; it never admits anything. Once the
// window ends, revocation is reported as 401.
func TestRevocationIsReportedOnceTheCachedRefusalEnds(t *testing.T) {
	e, clock := newClockedEnv(t, nil)
	e.quota.decision = limits.Decision{Allowed: false, RetryAfter: 2 * time.Second}
	if first := e.post(t, listTools); first.status != 429 {
		t.Fatalf("setup: %s", first)
	}
	e.auth.err = authority.ErrIntegrationUnauthorized
	if cached := e.post(t, listTools); cached.status != 429 || errorCode(t, cached) != "rate_limited" || e.reader.calls.Load() != 0 {
		t.Fatalf("cached: %s", cached)
	}
	clock.advance(2 * time.Second)
	revoked := e.post(t, listTools)
	if revoked.status != 401 || revoked.header.Get("WWW-Authenticate") == "" {
		t.Fatalf("revoked after window: %s", revoked)
	}
	if e.handler.refusals.len() != 0 {
		t.Fatalf("expired entry retained: %d", e.handler.refusals.len())
	}
}

// A concurrency refusal is cached only while the credential's in-flight
// request still holds the gate: the next request after it finishes is
// admitted at once, with no backoff.
func TestRepeatConcurrencyRefusalLastsOnlyWhileTheCredentialIsInFlight(t *testing.T) {
	e, _ := newClockedEnv(t, nil)
	entered, unblock := make(chan struct{}), make(chan struct{})
	var once, unblockOnce sync.Once
	release := func() { unblockOnce.Do(func() { close(unblock) }) }
	t.Cleanup(release) // registered after the server's Close, so it runs first
	e.reader.babies = func(ctx context.Context) (familyread.BabiesResult, error) {
		once.Do(func() {
			close(entered)
			<-unblock
		})
		return familyread.BabiesResult{Revision: 1}, nil
	}
	done := make(chan rawResponse, 1)
	go func() { done <- e.post(t, toolCall(1, "list_babies", `{}`)) }()
	<-entered
	gated := e.post(t, listTools)
	if gated.status != 429 || errorCode(t, gated) != "concurrency_limited" || gated.header.Get("Retry-After") != "1" || e.auth.calls.Load() != 2 {
		t.Fatalf("authenticated concurrency refusal: %s auth=%d", gated, e.auth.calls.Load())
	}
	repeat := e.post(t, listTools)
	if repeat.status != 429 || string(repeat.body) != string(gated.body) || repeat.header.Get("Retry-After") != "1" || e.auth.calls.Load() != 2 {
		t.Fatalf("cached concurrency refusal: %s auth=%d", repeat, e.auth.calls.Load())
	}
	requireNoStore(t, repeat)
	if e.quota.calls.Load() != 1 {
		t.Fatalf("concurrency refusals charged quota: %d", e.quota.calls.Load())
	}
	release()
	if first := <-done; first.status != 200 {
		t.Fatalf("in-flight call: %s", first)
	}
	if next := e.post(t, toolCall(2, "list_babies", `{}`)); next.status != 200 || e.auth.calls.Load() != 3 {
		t.Fatalf("after the holder finished: %s auth=%d", next, e.auth.calls.Load())
	}
	e.assertIdle(t)
}

// A gate without Held cannot confirm the holder, so concurrency refusals are
// not cached at all (quota refusals still are).
type plainGate struct{ inner *admission.Keyed }

func (g plainGate) TryAcquire(key string) (func(), bool) { return g.inner.TryAcquire(key) }

func TestConcurrencyRefusalIsNotCachedWithoutAHolderCheck(t *testing.T) {
	inner := testGate()
	e, _ := newClockedEnv(t, func(o *Options) { o.Gate = plainGate{inner} })
	release, _ := inner.TryAcquire(testGrantID)
	for i := range 2 {
		if gated := e.post(t, listTools); gated.status != 429 || e.auth.calls.Load() != int32(i+1) {
			t.Fatalf("refusal %d: %s auth=%d", i, gated, e.auth.calls.Load())
		}
	}
	release()
}

// The cache only remembers authenticated refusals, never grows past its
// bound and never turns a hit into anything but a refusal.
func TestRefusalCacheIsBoundedAndRefusalOnly(t *testing.T) {
	clock := &testClock{now: testNow}
	held := map[string]bool{}
	cache := newRefusalCache(3, clock.Now, func(grant string) bool { return held[grant] })
	digest := func(i int) [32]byte { return sha256.Sum256([]byte(fmt.Sprintf("credential-%d", i))) }
	for i := range 5 {
		cache.refuseQuota(digest(i), time.Duration(10+i)*time.Second)
		if cache.len() > 3 {
			t.Fatalf("grew to %d", cache.len())
		}
	}
	// The soonest-ending entries were evicted; the latest remain refusals.
	for i := range 5 {
		f, hit := cache.lookup(digest(i))
		if want := i >= 2; hit != want || (hit && (f.status != http.StatusTooManyRequests || f.code != "rate_limited")) {
			t.Fatalf("entry %d hit=%v failure=%+v", i, hit, f)
		}
	}
	// Expired entries are purged before anything is evicted.
	clock.advance(13 * time.Second)
	cache.refuseConcurrency(digest(9), "grant-9")
	if cache.len() != 2 {
		t.Fatalf("expired entries kept: %d", cache.len())
	}
	if _, hit := cache.lookup(digest(9)); hit {
		t.Fatal("concurrency entry hit without a current holder")
	}
	held["grant-9"] = true
	cache.refuseConcurrency(digest(9), "grant-9")
	if f, hit := cache.lookup(digest(9)); !hit || f.code != "concurrency_limited" || f.retryAfter != 1 {
		t.Fatalf("held concurrency entry: hit=%v %+v", hit, f)
	}
	// Bounded even for a holder that never finishes.
	clock.advance(time.Second)
	if _, hit := cache.lookup(digest(9)); hit {
		t.Fatal("concurrency entry outlived its bound")
	}
	if _, hit := cache.lookup(sha256.Sum256(nil)); hit {
		t.Fatal("unknown digest hit")
	}
}

// Refusals keep the keep-alive connection: no Connection: close, and the
// next request on the same connection is served.
func TestRefusalsKeepTheKeepAliveConnection(t *testing.T) {
	var conns atomic.Int32
	e, clock := newClockedEnvServer(t, nil, func(server *http.Server) {
		server.ConnState = func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				conns.Add(1)
			}
		}
	})
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 1}, Timeout: 5 * time.Second}
	send := func(body string) (int, string, bool, http.Header) {
		reused := false
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}
		request, _ := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), http.MethodPost, e.url, strings.NewReader(body))
		for name, value := range legacyHeaders() {
			request.Header.Set(name, value)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		return response.StatusCode, string(data), reused, response.Header
	}
	if status, _, _, _ := send(listTools); status != 200 {
		t.Fatalf("first=%d", status)
	}
	releaseSlots := holdAllMCPSlots(t, e)
	if status, body, reused, header := send(strings.Repeat(" ", 16<<10) + listTools); status != 503 || !strings.Contains(body, "server_busy") || !reused || header.Get("Connection") != "" {
		t.Fatalf("busy: %d reused=%v header=%v", status, reused, header)
	}
	releaseSlots()
	e.quota.decision = limits.Decision{Allowed: false, RetryAfter: 30 * time.Second}
	for _, name := range []string{"authenticated", "cached"} {
		if status, body, reused, header := send(strings.Repeat(" ", 16<<10) + listTools); status != 429 || !strings.Contains(body, "rate_limited") || !reused || header.Get("Connection") != "" {
			t.Fatalf("%s quota refusal: %d reused=%v header=%v", name, status, reused, header)
		}
	}
	clock.advance(30 * time.Second)
	e.quota.decision = limits.Decision{}
	if status, _, reused, _ := send(listTools); status != 200 || !reused || conns.Load() != 1 {
		t.Fatalf("after refusals: %d reused=%v conns=%d", status, reused, conns.Load())
	}
}

// Repeat refusals and pre-authentication busy refusals are marked for
// aggregated logging; authenticated outcomes are not.
func TestRepeatRefusalsAreMarkedForAggregatedLogging(t *testing.T) {
	var marked, unmarked atomic.Int32
	e, _ := newClockedEnvServer(t, nil, func(server *http.Server) {
		inner := server.Handler
		server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, aggregated := requestlog.WithMark(r.Context())
			inner.ServeHTTP(w, r.WithContext(ctx))
			if aggregated() {
				marked.Add(1)
			} else {
				unmarked.Add(1)
			}
		})
	})
	e.post(t, listTools) // admitted
	e.quota.decision = limits.Decision{Allowed: false, RetryAfter: 30 * time.Second}
	e.post(t, listTools) // authenticated refusal
	if marked.Load() != 0 || unmarked.Load() != 2 {
		t.Fatalf("authenticated outcomes marked=%d unmarked=%d", marked.Load(), unmarked.Load())
	}
	e.post(t, listTools) // cached refusal
	e.quota.decision = limits.Decision{}
	headers := legacyHeaders()
	headers["Authorization"] = "Bearer " + strings.Replace(testCredential, "AAAA", "BBBB", 1)
	releaseSlots := holdAllMCPSlots(t, e)
	e.do(t, http.MethodPost, listTools, headers) // server_busy before authentication
	releaseSlots()
	if marked.Load() != 2 || unmarked.Load() != 2 {
		t.Fatalf("repeat refusals marked=%d unmarked=%d", marked.Load(), unmarked.Load())
	}
}

// The refusal cache never holds an entry for an unauthenticated failure.
func TestFailuresOtherThanOverLimitAreNeverCached(t *testing.T) {
	e, _ := newClockedEnv(t, nil)
	e.quota.err = errors.New("unavailable")
	e.post(t, listTools)
	e.quota.err = nil
	e.auth.err = errors.New("authority unavailable")
	e.post(t, listTools)
	e.auth.err = nil
	headers := legacyHeaders()
	headers["Authorization"] = "Bearer " + phoneToken
	e.do(t, http.MethodPost, listTools, headers)
	if e.handler.refusals.len() != 0 {
		t.Fatalf("non-refusal cached: %d", e.handler.refusals.len())
	}
	if response := e.post(t, listTools); response.status != 200 {
		t.Fatalf("valid request after failures: %s", response)
	}
}
