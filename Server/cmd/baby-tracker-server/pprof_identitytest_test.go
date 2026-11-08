//go:build identitytest

package main

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/database"
	"babytracker/server/internal/httpapi"
)

// pprofTestHandler is the API handler as main serves it (the identitytest
// wrapper around httpapi), composed without a database.
func pprofTestHandler(t *testing.T) http.Handler {
	t.Helper()
	cfg := mcpUnitConfig(t)
	pool, _ := database.OpenWithMax(t.Context(), "postgres://user@127.0.0.1:1/test", 4)
	t.Cleanup(pool.Close)
	authorityService, familyService, readiness := testComposition(t, cfg, pool)
	options, err := applicationOptions(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), readiness, authorityService, familyService, pool, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return wrapIdentityTestHandler(httpapi.New(options), cfg.AppleAudience)
}

func TestPprofListensOnlyOnItsOwnAddress(t *testing.T) {
	t.Setenv("BT_IDENTITYTEST_PPROF_ADDRESS", "127.0.0.1:0")
	stop := startProfiling(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer stop()
	address := profilingAddress()
	if address == "" {
		t.Fatal("no profiling listener")
	}
	for _, path := range []string{"/debug/pprof/heap?debug=1", "/debug/pprof/mutex?debug=1", "/debug/pprof/block?debug=1", "/debug/pprof/goroutine?debug=1"} {
		response, err := http.Get("http://" + address + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", path, response.StatusCode)
		}
	}
	// The API handler never serves pprof (net/http/pprof registers on
	// http.DefaultServeMux, which the server never serves).
	api := httptest.NewServer(pprofTestHandler(t))
	defer api.Close()
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/heap", "/debug/pprof/cmdline", "/debug/pprof/profile?seconds=1"} {
		response, err := http.Get(api.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("API handler answered %s with %d", path, response.StatusCode)
		}
	}
	stop()
	if profilingAddress() != "" {
		t.Fatal("the listener outlived stop")
	}
	if connection, err := net.Dial("tcp", address); err == nil {
		connection.Close()
		t.Fatalf("still listening on %s after stop", address)
	}
}

// blockForPprofTest blocks on ready for about 20 ms; its frame names the
// block sample TestPprofSetsTheLockProfileRates looks for.
//
//go:noinline
func blockForPprofTest(ready <-chan struct{}) { <-ready }

// startProfiling turns lock profiling on (without it /debug/pprof/mutex and
// /block answer 200 with empty profiles) and stop turns it off. The mutex
// fraction is read back; the block rate has no getter, so a deliberate
// 20 ms block, twice the 10 ms rate and so always recorded, must appear in
// the block profile.
func TestPprofSetsTheLockProfileRates(t *testing.T) {
	if runtime.SetMutexProfileFraction(-1) != 0 {
		t.Fatal("mutex profiling already on before startProfiling")
	}
	t.Setenv("BT_IDENTITYTEST_PPROF_ADDRESS", "127.0.0.1:0")
	stop := startProfiling(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer stop()
	if got := runtime.SetMutexProfileFraction(-1); got != 100 {
		t.Fatalf("mutex profile fraction %d, want 100", got)
	}
	// The goroutine signals just before it blocks, so the block lasts at
	// least the sleep, well over the rate, however late it was scheduled.
	started, ready, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() { defer close(done); close(started); blockForPprofTest(ready) }()
	<-started
	time.Sleep(20 * time.Millisecond)
	close(ready)
	<-done
	response, err := http.Get("http://" + profilingAddress() + "/debug/pprof/block?debug=1")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "blockForPprofTest") {
		t.Fatalf("the 20 ms block is not in the block profile:\n%s", body)
	}
	stop()
	if got := runtime.SetMutexProfileFraction(-1); got != 0 {
		t.Fatalf("mutex profile fraction %d after stop, want 0", got)
	}
}

// Without the variable there is no listener.
func TestPprofIsOffWithoutItsVariable(t *testing.T) {
	t.Setenv("BT_IDENTITYTEST_PPROF_ADDRESS", "")
	stop := startProfiling(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer stop()
	if profilingAddress() != "" {
		t.Fatal("profiling listens without BT_IDENTITYTEST_PPROF_ADDRESS")
	}
}

func addrs(values ...string) func() ([]net.Addr, error) {
	return func() ([]net.Addr, error) {
		var result []net.Addr
		for _, value := range values {
			_, network, err := net.ParseCIDR(value)
			if err != nil {
				panic(err)
			}
			network.IP = net.ParseIP(strings.Split(value, "/")[0])
			result = append(result, network)
		}
		return result, nil
	}
}

// The listener never binds an address other machines can reach: an
// explicit host must be loopback or localhost; a wildcard is accepted only
// when every interface address is loopback, private or link-local (a
// container on its bridge networks); anything else, and any host name but
// localhost, is refused.
func TestPprofNeverListensOnAPublicAddress(t *testing.T) {
	container := addrs("127.0.0.1/8", "::1/128", "172.18.0.5/16", "172.31.250.10/24", "fe80::1/64")
	public := addrs("127.0.0.1/8", "172.18.0.5/16", "203.0.113.7/24")
	publicV6 := addrs("127.0.0.1/8", "2001:db8::7/64")
	for _, c := range []struct {
		address    string
		interfaces func() ([]net.Addr, error)
		allowed    bool
	}{
		{"127.0.0.1:6060", public, true},
		{"127.0.0.2:6060", public, true},
		{"[::1]:6060", public, true},
		{"[::ffff:127.0.0.1]:6060", public, true},
		{"localhost:6060", public, true},
		// An explicit host other than loopback is refused, private or not: a
		// LAN, tailnet (ULA) or link-local address is reachable from other
		// machines.
		{"10.1.2.3:6060", container, false},
		{"172.31.250.10:6060", container, false},
		{"192.0.2.238:6060", container, false},
		{"[fd7a:115c:a1e0::1]:6060", container, false},
		{"[fe80::1]:6060", container, false},
		{"[fe80::1%en0]:6060", container, false},
		{"169.254.10.1:6060", container, false},
		{":6060", container, true},
		{"0.0.0.0:6060", container, true},
		{"[::]:6060", container, true},
		{":6060", public, false},
		{"0.0.0.0:6060", public, false},
		{"[::]:6060", publicV6, false},
		{":6060", func() ([]net.Addr, error) { return nil, errors.New("no interfaces") }, false},
		{"203.0.113.7:6060", container, false},
		{"8.8.8.8:6060", container, false},
		{"[2001:db8::7]:6060", container, false},
		{"pprof.example.com:6060", container, false},
		{"6060", container, false},
	} {
		err := checkProfilingAddress(c.address, c.interfaces)
		if (err == nil) != c.allowed {
			t.Errorf("%q: err=%v, want allowed=%v", c.address, err, c.allowed)
		}
	}
	// startProfiling refuses before it listens, and says so.
	var out bytes.Buffer
	t.Setenv("BT_IDENTITYTEST_PPROF_ADDRESS", "203.0.113.7:0")
	stop := startProfiling(slog.New(slog.NewTextHandler(&out, nil)))
	defer stop()
	if profilingAddress() != "" || !strings.Contains(out.String(), "pprof address refused") {
		t.Fatalf("a public address was not refused: %q", out.String())
	}
}
