package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/apns"
	"babytracker/server/internal/config"
)

type preflightSeen struct {
	path    string
	headers http.Header
}

// preflightAPNs is a local TLS HTTP/2 server standing in for Apple: answer
// handles every request, and the paths and headers are recorded.
func preflightAPNs(t *testing.T, answer http.HandlerFunc) (*httptest.Server, func() []preflightSeen) {
	t.Helper()
	var mu sync.Mutex
	var seen []preflightSeen
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		seen = append(seen, preflightSeen{path: r.URL.Path, headers: r.Header.Clone()})
		mu.Unlock()
		answer(w, r)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, func() []preflightSeen {
		mu.Lock()
		defer mu.Unlock()
		return append([]preflightSeen(nil), seen...)
	}
}

func preflightAnswer(status int, reason string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"reason":%q}`, reason)
	}
}

// preflightEnvironment is a throwaway key and made-up identifiers.
func preflightEnvironment(t *testing.T) map[string]string {
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
	return map[string]string{"BT_APNS_TEAM_ID": "TEAM123456", "BT_APNS_KEY_ID": "KEY1234567", "BT_APNS_KEY_FILE": path, "BT_APNS_BUNDLE_ID": "com.example.babytracker"}
}

// connectTo is a connect function over the test server's trusted transport
// (or rt), recording each connection's credentials.
func connectTo(server *httptest.Server, rt http.RoundTripper, connected *[]config.APNSCredentials) func(config.APNSCredentials) (apnsPreflighter, error) {
	if rt == nil {
		rt = server.Client().Transport
	}
	return func(creds config.APNSCredentials) (apnsPreflighter, error) {
		*connected = append(*connected, creds)
		client, err := apns.NewWithTransport(creds, rt, server.URL, server.URL, time.Now)
		if err != nil {
			return nil, err
		}
		return client, nil
	}
}

func TestAPNsPreflightPrintsOneLineAndPassesOnlyOnBadDeviceToken(t *testing.T) {
	for _, c := range []struct {
		arguments []string
		answer    http.HandlerFunc
		line      string
		pushType  string
		topic     string
		success   bool
	}{
		{[]string{"--environment", "sandbox"}, preflightAnswer(400, "BadDeviceToken"),
			"apns_preflight environment=sandbox push_type=background outcome=pass status=400 reason=BadDeviceToken\n", "background", "com.example.babytracker", true},
		{[]string{"--environment", "production", "--push-type", "liveactivity"}, preflightAnswer(400, "BadDeviceToken"),
			"apns_preflight environment=production push_type=liveactivity outcome=pass status=400 reason=BadDeviceToken\n", "liveactivity", "com.example.babytracker.push-type.liveactivity", true},
		{[]string{"--push-type", "background", "--environment", "sandbox"}, preflightAnswer(403, "InvalidProviderToken"),
			"apns_preflight environment=sandbox push_type=background outcome=bad_key status=403 reason=InvalidProviderToken\n", "background", "com.example.babytracker", false},
		{[]string{"--environment", "sandbox", "--push-type", "liveactivity"}, preflightAnswer(400, "TopicDisallowed"),
			"apns_preflight environment=sandbox push_type=liveactivity outcome=topic status=400 reason=TopicDisallowed\n", "liveactivity", "com.example.babytracker.push-type.liveactivity", false},
		{[]string{"--environment", "sandbox"}, preflightAnswer(500, "InternalServerError"),
			"apns_preflight environment=sandbox push_type=background outcome=unexpected status=500 reason=InternalServerError\n", "background", "com.example.babytracker", false},
		// The answer is lost after the request was taken: unreachable, with the coarse cause.
		{[]string{"--environment", "sandbox", "--push-type", "liveactivity"}, func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) },
			"apns_preflight environment=sandbox push_type=liveactivity outcome=unreachable status=0 reason=connection\n", "liveactivity", "com.example.babytracker.push-type.liveactivity", false},
	} {
		server, seen := preflightAPNs(t, c.answer)
		values := preflightEnvironment(t)
		var connected []config.APNSCredentials
		var out bytes.Buffer
		err := runAPNsPreflight(t.Context(), c.arguments, func(name string) string { return values[name] }, connectTo(server, nil, &connected), &out)
		if (err == nil) != c.success || out.String() != c.line {
			t.Fatalf("%q: %q %v, want %q", c.arguments, out.String(), err, c.line)
		}
		if len(connected) != 1 || connected[0].KeyFile != values["BT_APNS_KEY_FILE"] {
			t.Fatalf("connected %+v", connected)
		}
		requests := seen()
		if len(requests) != 1 || requests[0].path != "/3/device/"+strings.Repeat("0", 64) ||
			requests[0].headers.Get("apns-push-type") != c.pushType || requests[0].headers.Get("apns-topic") != c.topic {
			t.Fatalf("%q: requests %+v", c.arguments, requests)
		}
		text := out.String()
		if err != nil {
			text += err.Error()
		}
		for _, secret := range []string{"KEY1234567", "TEAM123456", strings.Repeat("0", 64), values["BT_APNS_KEY_FILE"], "bearer", server.URL} {
			if strings.Contains(text, secret) {
				t.Fatalf("%q: the output %q names %q", c.arguments, text, secret)
			}
		}
	}
}

// A preflight that never reached Apple prints its unreachable line with the
// coarse cause.
func TestAPNsPreflightReportsAnUnreachableApple(t *testing.T) {
	server, seen := preflightAPNs(t, preflightAnswer(400, "BadDeviceToken"))
	values := preflightEnvironment(t)
	var connected []config.APNSCredentials
	var out bytes.Buffer
	// The default transport does not trust the test server's certificate.
	err := runAPNsPreflight(t.Context(), []string{"--environment", "production"}, func(name string) string { return values[name] },
		connectTo(server, &http.Transport{ForceAttemptHTTP2: true}, &connected), &out)
	if err == nil || out.String() != "apns_preflight environment=production push_type=background outcome=unreachable status=0 reason=tls\n" || len(seen()) != 0 {
		t.Fatalf("%q %v, %d requests", out.String(), err, len(seen()))
	}
}

// failingPreflighter is a client whose preflight request cannot be built.
type failingPreflighter struct{ closed bool }

func (f *failingPreflighter) Preflight(context.Context, apns.Environment, string) (apns.PreflightResult, error) {
	return apns.PreflightResult{}, errors.Join(apns.ErrNotSent, errors.New("signing failed for SENTINEL-KEY-c0ffee"))
}
func (f *failingPreflighter) CloseIdleConnections() { f.closed = true }

// A request that could not be built prints nothing and fails with a fixed
// message, never the underlying error's text.
func TestAPNsPreflightReportsARequestThatCouldNotBeBuilt(t *testing.T) {
	values := preflightEnvironment(t)
	failing := &failingPreflighter{}
	var out bytes.Buffer
	err := runAPNsPreflight(t.Context(), []string{"--environment", "sandbox", "--push-type", "liveactivity"}, func(name string) string { return values[name] },
		func(config.APNSCredentials) (apnsPreflighter, error) { return failing, nil }, &out)
	if err == nil || err.Error() != "the preflight request could not be built" || out.Len() != 0 || !failing.closed {
		t.Fatalf("%q %v closed=%v", out.String(), err, failing.closed)
	}
}

func TestAPNsPreflightRefusesBadInvocations(t *testing.T) {
	values := preflightEnvironment(t)
	getenv := func(name string) string { return values[name] }
	never := func(config.APNSCredentials) (apnsPreflighter, error) {
		t.Fatal("Apple was contacted")
		return nil, nil
	}
	for _, arguments := range [][]string{nil, {"--environment"}, {"--environment", "staging"}, {"--environment", "both"},
		{"--environment", "sandbox", "extra"}, {"--skip-apple"}, {"--environment", "sandbox", "--push-type", "alert"},
		{"--environment", "sandbox", "--push-type", ""}, {"--environment", "sandbox", "--push-type"}} {
		var out bytes.Buffer
		if err := runAPNsPreflight(t.Context(), arguments, getenv, never, &out); !errors.Is(err, errAPNsPreflightUsage) || out.Len() != 0 {
			t.Fatalf("%q: %q %v, want a usage error", arguments, out.String(), err)
		}
	}
	delete(values, "BT_APNS_KEY_FILE")
	var out bytes.Buffer
	if err := runAPNsPreflight(t.Context(), []string{"--environment", "production"}, getenv, never, &out); err == nil ||
		!strings.Contains(err.Error(), "BT_APNS_KEY_FILE") || out.Len() != 0 {
		t.Fatalf("no key file: %q %v", out.String(), err)
	}
}
