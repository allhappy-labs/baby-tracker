package apns_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"babytracker/server/internal/apns"
	"babytracker/server/internal/config"
	"babytracker/server/internal/protocol"
)

type seen struct {
	proto   int
	host    string
	path    string
	headers http.Header
	body    []byte
}

type fakeServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []seen
	conns    atomic.Int32
}

func (f *fakeServer) seen() []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seen(nil), f.requests...)
}

// fakeAPNs is a local TLS HTTP/2 server standing in for Apple; nothing in
// these tests can reach Apple.
func fakeAPNs(t *testing.T, answer func(w http.ResponseWriter, r *http.Request)) *fakeServer {
	t.Helper()
	fake := &fakeServer{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fake.mu.Lock()
		fake.requests = append(fake.requests, seen{proto: r.ProtoMajor, host: r.Host, path: r.URL.Path, headers: r.Header.Clone(), body: body})
		fake.mu.Unlock()
		answer(w, r)
	}))
	server.EnableHTTP2 = true
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			fake.conns.Add(1)
		}
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	fake.Server = server
	return fake
}

// testCredentials generates a throwaway P-256 key in a temp dir: no Apple
// credential is ever read.
func testCredentials(t *testing.T) (config.APNSCredentials, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	path := filepath.Join(t.TempDir(), "apns.p8")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.APNSCredentials{TeamID: "TEAM123456", KeyID: "KEY1234567", KeyFile: path, BundleID: "com.example.babytracker"}, key
}

type providerClaims struct {
	header, claims string
	valid          bool
}

func decodeProviderToken(t *testing.T, authorization string, key *ecdsa.PrivateKey) providerClaims {
	t.Helper()
	jwt, ok := strings.CutPrefix(authorization, "bearer ")
	parts := strings.Split(jwt, ".")
	if !ok || len(parts) != 3 {
		t.Fatalf("authorization %q is not a bearer JWT", authorization)
	}
	header, _ := base64.RawURLEncoding.DecodeString(parts[0])
	claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	valid := len(signature) == 64 && ecdsa.Verify(&key.PublicKey, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:]))
	return providerClaims{header: string(header), claims: string(claims), valid: valid}
}

func TestWakeSendsAContentlessBackgroundPushOverOneHTTP2Connection(t *testing.T) {
	server := fakeAPNs(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	creds, key := testCredentials(t)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	client, err := apns.NewWithTransport(creds, server.Client().Transport, server.URL, server.URL, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	token := bytes.Repeat([]byte{0xab}, 32)
	for range 10 {
		if result, err := client.Wake(t.Context(), apns.Sandbox, token); err != nil || result.Class != apns.Accepted || result.Status != http.StatusOK {
			t.Fatalf("%+v %v", result, err)
		}
	}
	if server.conns.Load() != 1 {
		t.Fatalf("%d connections for 10 pushes, want 1 reused", server.conns.Load())
	}
	requests := server.seen()
	first := requests[0]
	if first.proto != 2 || first.path != "/3/device/"+hex.EncodeToString(token) || string(first.body) != apns.WakePayload {
		t.Fatalf("request %d %s %s", first.proto, first.path, first.body)
	}
	if apns.WakePayload != `{"aps":{"content-available":1}}` {
		t.Fatalf("payload %s", apns.WakePayload)
	}
	for name, want := range map[string]string{"apns-push-type": "background", "apns-priority": "5", "apns-topic": "com.example.babytracker",
		"apns-expiration": strconv.FormatInt(now.Add(time.Hour).Unix(), 10), "content-type": "application/json"} {
		if got := first.headers.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	ids := map[string]bool{}
	for _, r := range requests {
		id := r.headers.Get("apns-id")
		if !protocol.ValidUUID(id) || id[14] != '4' || ids[id] {
			t.Fatalf("apns-id %q is not a fresh lower-case version-4 UUID", id)
		}
		ids[id] = true
	}
	// The provider token: ES256, kid = key ID, iss = team, iat = now, verified
	// with the public key; one token for all ten.
	jwt := decodeProviderToken(t, first.headers.Get("authorization"), key)
	if jwt.header != `{"alg":"ES256","kid":"KEY1234567"}` || jwt.claims != fmt.Sprintf(`{"iat":%d,"iss":"TEAM123456"}`, now.Unix()) || !jwt.valid {
		t.Fatalf("provider token %+v", jwt)
	}
	for _, r := range requests {
		if r.headers.Get("authorization") != first.headers.Get("authorization") {
			t.Fatal("the provider token was re-signed within 40 minutes")
		}
	}
}

func TestWakeRoutesEachEnvironmentToItsOwnHost(t *testing.T) {
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	sandbox, production := fakeAPNs(t, ok), fakeAPNs(t, ok)
	creds, _ := testCredentials(t)
	// Each environment's test transport trusts its own server's certificate.
	transport := &hostRouter{routes: map[string]http.RoundTripper{
		strings.TrimPrefix(sandbox.URL, "https://"):    sandbox.Client().Transport,
		strings.TrimPrefix(production.URL, "https://"): production.Client().Transport,
	}}
	client, err := apns.NewWithTransport(creds, transport, sandbox.URL, production.URL, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	token := bytes.Repeat([]byte{0x01}, 32)
	for _, env := range []apns.Environment{apns.Sandbox, apns.Sandbox, apns.Production} {
		if result, err := client.Wake(t.Context(), env, token); err != nil || result.Class != apns.Accepted {
			t.Fatalf("%s: %+v %v", env, result, err)
		}
	}
	if len(sandbox.seen()) != 2 || len(production.seen()) != 1 {
		t.Fatalf("sandbox got %d, production got %d", len(sandbox.seen()), len(production.seen()))
	}
	if sandbox.conns.Load() != 1 || production.conns.Load() != 1 {
		t.Fatalf("connections: sandbox %d, production %d, want one each", sandbox.conns.Load(), production.conns.Load())
	}
}

type hostRouter struct{ routes map[string]http.RoundTripper }

func (r *hostRouter) RoundTrip(request *http.Request) (*http.Response, error) {
	route, ok := r.routes[request.URL.Host]
	if !ok {
		return nil, errors.New("unrouted host")
	}
	return route.RoundTrip(request)
}

func TestWakeClassifiesApplesAnswers(t *testing.T) {
	for _, c := range []struct {
		status     int
		reason     string
		retryAfter string
		class      apns.Class
		wantReason string
	}{
		{200, "", "", apns.Accepted, ""},
		{410, "Unregistered", "", apns.InvalidToken, "Unregistered"},
		{410, "", "", apns.InvalidToken, ""},
		{400, "BadDeviceToken", "", apns.InvalidToken, "BadDeviceToken"},
		{400, "DeviceTokenNotForTopic", "", apns.ConfigurationFailure, "DeviceTokenNotForTopic"},
		// A topic reason is the deployment's, whatever the status: never delete a token for it.
		{410, "DeviceTokenNotForTopic", "", apns.ConfigurationFailure, "DeviceTokenNotForTopic"},
		{400, "BadTopic", "", apns.ConfigurationFailure, "BadTopic"},
		{400, "TopicDisallowed", "", apns.ConfigurationFailure, "TopicDisallowed"},
		{400, "BadPriority", "", apns.ConfigurationFailure, "BadPriority"},
		{400, "an undocumented reason", "", apns.ConfigurationFailure, ""},
		{403, "ExpiredProviderToken", "", apns.ConfigurationFailure, "ExpiredProviderToken"},
		{403, "InvalidProviderToken", "", apns.ConfigurationFailure, "InvalidProviderToken"},
		{404, "BadPath", "", apns.ConfigurationFailure, "BadPath"},
		// Apple's current reasons beyond main's list; the status still decides the class.
		{410, "ExpiredToken", "", apns.InvalidToken, "ExpiredToken"},
		{403, "BadEnvironmentKeyIdInToken", "", apns.ConfigurationFailure, "BadEnvironmentKeyIdInToken"},
		{403, "UnrelatedKeyIdInToken", "", apns.ConfigurationFailure, "UnrelatedKeyIdInToken"},
		{400, "DuplicateHeaders", "", apns.ConfigurationFailure, "DuplicateHeaders"},
		{400, "InvalidPushType", "", apns.ConfigurationFailure, "InvalidPushType"},
		{400, "BadPushType", "", apns.ConfigurationFailure, ""}, // not one of Apple's reasons
		{413, "PayloadTooLarge", "", apns.ConfigurationFailure, "PayloadTooLarge"},
		{429, "TooManyRequests", "30", apns.Retryable, "TooManyRequests"},
		{429, "TooManyProviderTokenUpdates", "", apns.Retryable, "TooManyProviderTokenUpdates"},
		{500, "InternalServerError", "", apns.Retryable, "InternalServerError"},
		{503, "ServiceUnavailable", "", apns.Retryable, "ServiceUnavailable"},
	} {
		server := fakeAPNs(t, func(w http.ResponseWriter, _ *http.Request) {
			if c.retryAfter != "" {
				w.Header().Set("Retry-After", c.retryAfter)
			}
			w.WriteHeader(c.status)
			if c.reason != "" {
				fmt.Fprintf(w, `{"reason":%q}`, c.reason)
			}
		})
		creds, _ := testCredentials(t)
		now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
		client, err := apns.NewWithTransport(creds, server.Client().Transport, server.URL, server.URL, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		result, err := client.Wake(t.Context(), apns.Production, bytes.Repeat([]byte{1}, 32))
		if err != nil || result.Class != c.class || result.Status != c.status || result.Reason != c.wantReason {
			t.Fatalf("%d %s: %+v %v", c.status, c.reason, result, err)
		}
		if c.retryAfter != "" && !result.RetryAfter.Equal(now.Add(30*time.Second)) {
			t.Fatalf("Retry-After %v", result.RetryAfter)
		}
		if c.retryAfter == "" && !result.RetryAfter.IsZero() {
			t.Fatalf("%d %s: Retry-After %v without the header", c.status, c.reason, result.RetryAfter)
		}
	}
}

// Retry-After is either form, bounded to 24 hours either way: an answer
// outside the bound, in the past or unreadable carries no Retry-After (the
// caller's own minimum pause applies).
func TestWakeReadsRetryAfterAsAnHTTPDate(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		header string
		want   time.Time
	}{
		{now.Add(2 * time.Minute).Format(http.TimeFormat), now.Add(2 * time.Minute)},
		{now.Add(24 * time.Hour).Format(http.TimeFormat), now.Add(24 * time.Hour)},
		{now.Add(24*time.Hour + time.Second).Format(http.TimeFormat), time.Time{}},
		{"Fri, 01 Jan 2100 00:00:00 GMT", time.Time{}},
		{now.Add(-time.Minute).Format(http.TimeFormat), time.Time{}},
		{now.Format(http.TimeFormat), time.Time{}},
		{"86400", now.Add(24 * time.Hour)},
		{"86401", time.Time{}},
		{"-5", time.Time{}},
		{"soon", time.Time{}},
		{"", time.Time{}},
	} {
		server := fakeAPNs(t, func(w http.ResponseWriter, _ *http.Request) {
			if c.header != "" {
				w.Header().Set("Retry-After", c.header)
			}
			w.WriteHeader(http.StatusTooManyRequests)
		})
		creds, _ := testCredentials(t)
		client, err := apns.NewWithTransport(creds, server.Client().Transport, server.URL, server.URL, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		result, err := client.Wake(t.Context(), apns.Production, bytes.Repeat([]byte{1}, 32))
		if err != nil || result.Class != apns.Retryable || !result.RetryAfter.Equal(c.want) {
			t.Fatalf("Retry-After %q: %+v %v, want %v", c.header, result, err, c.want)
		}
	}
}

// Apple's 403 ExpiredProviderToken / InvalidProviderToken makes the next send
// re-sign, but never sooner than 20 minutes after the last signing
// (TooManyProviderTokenUpdates).
func TestWakeReSignsTheProviderTokenAfterAppleRefusesIt(t *testing.T) {
	for _, reason := range []string{"ExpiredProviderToken", "InvalidProviderToken"} {
		var refuse atomic.Bool
		server := fakeAPNs(t, func(w http.ResponseWriter, _ *http.Request) {
			if refuse.Load() {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprintf(w, `{"reason":%q}`, reason)
				return
			}
			w.WriteHeader(http.StatusOK)
		})
		creds, key := testCredentials(t)
		start := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
		var clock atomic.Int64
		clock.Store(start.UnixNano())
		client, err := apns.NewWithTransport(creds, server.Client().Transport, server.URL, server.URL, func() time.Time { return time.Unix(0, clock.Load()).UTC() })
		if err != nil {
			t.Fatal(err)
		}
		token := bytes.Repeat([]byte{2}, 32)
		wake := func(at time.Duration) string {
			t.Helper()
			clock.Store(start.Add(at).UnixNano())
			if _, err := client.Wake(t.Context(), apns.Sandbox, token); err != nil {
				t.Fatal(err)
			}
			requests := server.seen()
			return requests[len(requests)-1].headers.Get("authorization")
		}
		signed := wake(0)
		refuse.Store(true)
		if wake(10*time.Minute) != signed || wake(11*time.Minute) != signed {
			t.Fatalf("%s: re-signed inside Apple's 20-minute floor", reason)
		}
		refused := wake(21 * time.Minute) // refused with a 20-minute-old token: invalidated
		if refused != signed {
			t.Fatalf("%s: the refused request did not carry the cached token", reason)
		}
		refuse.Store(false)
		resigned := wake(22 * time.Minute)
		jwt := decodeProviderToken(t, resigned, key)
		if resigned == signed || !jwt.valid || jwt.claims != fmt.Sprintf(`{"iat":%d,"iss":"TEAM123456"}`, start.Add(22*time.Minute).Unix()) {
			t.Fatalf("%s: not re-signed after the refusal: %+v", reason, jwt)
		}
		if wake(23*time.Minute) != resigned {
			t.Fatalf("%s: an accepted token was re-signed", reason)
		}
	}
}

// Other refusals never touch the provider token.
func TestWakeKeepsTheProviderTokenOnOtherRefusals(t *testing.T) {
	var answer atomic.Int32
	answer.Store(http.StatusOK)
	server := fakeAPNs(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(answer.Load()))
		if answer.Load() != http.StatusOK {
			fmt.Fprint(w, `{"reason":"Forbidden"}`)
		}
	})
	creds, _ := testCredentials(t)
	start := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(start.UnixNano())
	client, err := apns.NewWithTransport(creds, server.Client().Transport, server.URL, server.URL, func() time.Time { return time.Unix(0, clock.Load()).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	token := bytes.Repeat([]byte{3}, 32)
	_, _ = client.Wake(t.Context(), apns.Sandbox, token)
	answer.Store(http.StatusForbidden)
	clock.Store(start.Add(25 * time.Minute).UnixNano())
	_, _ = client.Wake(t.Context(), apns.Sandbox, token)
	answer.Store(http.StatusOK)
	clock.Store(start.Add(26 * time.Minute).UnixNano())
	_, _ = client.Wake(t.Context(), apns.Sandbox, token)
	requests := server.seen()
	if requests[0].headers.Get("authorization") != requests[2].headers.Get("authorization") {
		t.Fatal("a 403 Forbidden re-signed the provider token")
	}
}

func TestWakeRefusesBeforeSending(t *testing.T) {
	server := fakeAPNs(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	creds, _ := testCredentials(t)
	client, err := apns.NewWithTransport(creds, server.Client().Transport, server.URL, server.URL, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		env   apns.Environment
		token []byte
	}{
		{"a 15-byte token", apns.Sandbox, bytes.Repeat([]byte{4}, 15)},
		{"a 257-byte token", apns.Production, bytes.Repeat([]byte{4}, 257)},
		{"no token", apns.Production, nil},
		{"environment staging", apns.Environment("staging"), bytes.Repeat([]byte{4}, 32)},
		{"no environment", apns.Environment(""), bytes.Repeat([]byte{4}, 32)},
	} {
		if _, err := client.Wake(t.Context(), c.env, c.token); !errors.Is(err, apns.ErrNotSent) {
			t.Fatalf("%s: %v, want ErrNotSent", c.name, err)
		}
	}
	if len(server.seen()) != 0 {
		t.Fatalf("%d requests reached the server for refused wakes", len(server.seen()))
	}
	for _, size := range []int{16, 256} {
		if result, err := client.Wake(t.Context(), apns.Sandbox, bytes.Repeat([]byte{5}, size)); err != nil || result.Class != apns.Accepted {
			t.Fatalf("a %d-byte token: %+v %v", size, result, err)
		}
	}

}

// A Wake whose answer is lost or unusable after its request may have reached
// Apple is Ambiguous, always with an error that wraps ErrOutcomeUnknown and a
// coarse cause, never the host, the path or the token.
func TestWakeReportsALostAnswerAsAmbiguousWithItsCause(t *testing.T) {
	creds, _ := testCredentials(t)
	token := bytes.Repeat([]byte{0xcd}, 32)
	arrived := make(chan struct{}, 1)
	reset := fakeAPNs(t, func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	silent := fakeAPNs(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	waiting := fakeAPNs(t, func(_ http.ResponseWriter, r *http.Request) { arrived <- struct{}{}; <-r.Context().Done() })
	redirect := fakeAPNs(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://elsewhere.example/")
		w.WriteHeader(http.StatusFound)
	})
	for _, c := range []struct {
		name     string
		server   *fakeServer
		cancel   bool
		cause    error
		status   int
		requests int
	}{
		{name: "a reset stream", server: reset, cause: apns.ErrCauseConnection, requests: 1},
		{name: "no answer within the bound", server: silent, cause: apns.ErrCauseTimeout, requests: 1},
		{name: "the caller gives up", server: waiting, cancel: true, cause: apns.ErrCauseCanceled, requests: 1},
		{name: "a 3xx answer", server: redirect, cause: apns.ErrCauseStatus, status: http.StatusFound, requests: 1},
	} {
		transport := c.server.Client().Transport
		client, err := apns.NewWithTransport(creds, transport, c.server.URL, c.server.URL, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		before := len(c.server.seen())
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		if c.cancel {
			go func() { <-arrived; cancel() }()
		}
		result, err := client.Wake(ctx, apns.Sandbox, token)
		cancel()
		if err == nil || errors.Is(err, apns.ErrNotSent) || !errors.Is(err, apns.ErrOutcomeUnknown) || result.Class != apns.Ambiguous || result.Status != c.status {
			t.Fatalf("%s: %+v %v, want Ambiguous with ErrOutcomeUnknown", c.name, result, err)
		}
		if !errors.Is(err, c.cause) {
			t.Fatalf("%s: %v, want cause %v", c.name, err, c.cause)
		}
		for _, secret := range []string{hex.EncodeToString(token), c.server.URL, strings.TrimPrefix(c.server.URL, "https://"), "127.0.0.1", "/3/device", "apple.com", "elsewhere"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("%s: the error %q names %q", c.name, err, secret)
			}
		}
		if got := len(c.server.seen()) - before; got != c.requests {
			t.Fatalf("%s: %d requests, want %d", c.name, got, c.requests)
		}
	}
}

// A Wake whose request never reached the wire (Apple's host did not resolve,
// the dial was refused, the TLS handshake failed or the connection closed
// during it, or the deadline or the caller ended the wait for a connection)
// was not sent: its error wraps ErrNotSent and a coarse cause, never
// ErrOutcomeUnknown, and never the host, the path or the token (final
// review R2-I2). Whether it was sent is decided by what the transport
// reports writing, not by the error's type: the same refused dial is
// Ambiguous once a request was written (see the lost-answer test).
func TestWakeReportsARequestThatNeverReachedTheWireAsNotSent(t *testing.T) {
	creds, _ := testCredentials(t)
	token := bytes.Repeat([]byte{0xce}, 32)
	silent := fakeAPNs(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	failingDial := func(err error) http.RoundTripper {
		return &http.Transport{ForceAttemptHTTP2: true, DialContext: func(context.Context, string, string) (net.Conn, error) { return nil, err }}
	}
	hangingDial := &http.Transport{ForceAttemptHTTP2: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	// A listener that accepts and closes at once: the TLS handshake meets EOF.
	closing, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closing.Close() })
	go func() {
		for {
			conn, err := closing.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	closingURL := "https://" + closing.Addr().String()
	for _, c := range []struct {
		name      string
		url       string
		transport http.RoundTripper
		cancel    bool
		cause     error
	}{
		{name: "an untrusted certificate", url: silent.URL, transport: &http.Transport{ForceAttemptHTTP2: true}, cause: apns.ErrCauseTLS},
		{name: "an unresolvable host", url: silent.URL, transport: failingDial(&net.DNSError{Err: "no such host", Name: "api.push.apple.com", IsNotFound: true}), cause: apns.ErrCauseDNS},
		{name: "a refused connection", url: silent.URL, transport: failingDial(&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}), cause: apns.ErrCauseConnection},
		{name: "a connection closed in the handshake", url: closingURL, transport: &http.Transport{ForceAttemptHTTP2: true}, cause: apns.ErrCauseConnection},
		{name: "no connection within the bound", url: silent.URL, transport: hangingDial, cause: apns.ErrCauseTimeout},
		{name: "the caller gives up before a connection", url: silent.URL, transport: hangingDial, cancel: true, cause: apns.ErrCauseCanceled},
	} {
		client, err := apns.NewWithTransport(creds, c.transport, c.url, c.url, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		before := len(silent.seen())
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		if c.cancel {
			time.AfterFunc(20*time.Millisecond, cancel)
		}
		result, err := client.Wake(ctx, apns.Sandbox, token)
		cancel()
		if !errors.Is(err, apns.ErrNotSent) || errors.Is(err, apns.ErrOutcomeUnknown) || result.Class == apns.Ambiguous || result.Class == apns.Accepted {
			t.Fatalf("%s: %+v %v, want ErrNotSent and not Ambiguous", c.name, result, err)
		}
		if !errors.Is(err, c.cause) {
			t.Fatalf("%s: %v, want cause %v", c.name, err, c.cause)
		}
		for _, secret := range []string{hex.EncodeToString(token), c.url, strings.TrimPrefix(c.url, "https://"), "127.0.0.1", "/3/device", "apple.com"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("%s: the error %q names %q", c.name, err, secret)
			}
		}
		if got := len(silent.seen()) - before; got != 0 {
			t.Fatalf("%s: %d requests reached the server", c.name, got)
		}
	}
}

// A request written on a connection that then fails is Ambiguous even
// though the error is a connection error: what reached the wire decides,
// not the error's type.
func TestWakeIsAmbiguousOnceTheRequestWasWritten(t *testing.T) {
	creds, _ := testCredentials(t)
	written := &writtenThenFailed{}
	client, err := apns.NewWithTransport(creds, written, "https://sandbox.invalid", "https://production.invalid", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Wake(t.Context(), apns.Sandbox, bytes.Repeat([]byte{0xcf}, 32))
	if !written.traced || errors.Is(err, apns.ErrNotSent) || !errors.Is(err, apns.ErrOutcomeUnknown) || !errors.Is(err, apns.ErrCauseConnection) || result.Class != apns.Ambiguous {
		t.Fatalf("traced %v: %+v %v, want Ambiguous with a connection cause", written.traced, result, err)
	}
}

// writtenThenFailed reports writing the request's headers (as a transport
// does), then fails as a refused dial would.
type writtenThenFailed struct{ traced bool }

func (w *writtenThenFailed) RoundTrip(request *http.Request) (*http.Response, error) {
	if trace := httptrace.ContextClientTrace(request.Context()); trace != nil && trace.WroteHeaders != nil {
		trace.WroteHeaders()
		w.traced = true
	}
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
}

func TestNewWithTransportRefusesAnUnusableConfiguration(t *testing.T) {
	creds, _ := testCredentials(t)
	server := fakeAPNs(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	if _, err := apns.NewWithTransport(creds, nil, server.URL, server.URL, time.Now); err == nil {
		t.Fatal("a nil transport was accepted")
	}
	broken := creds
	broken.KeyFile = filepath.Join(t.TempDir(), "absent.p8")
	if _, err := apns.NewWithTransport(broken, server.Client().Transport, server.URL, server.URL, time.Now); err == nil || strings.Contains(err.Error(), broken.KeyFile) {
		t.Fatalf("a missing key: %v", err)
	}
	if _, err := apns.New(broken); err == nil || strings.Contains(err.Error(), broken.KeyFile) {
		t.Fatalf("a missing key: %v", err)
	}
}
