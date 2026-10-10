package capacity

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync/atomic"
	"testing"
	"time"
)

// Phones and the MCP load generator are different clients: MCP requests
// never borrow, evict or wait for a phone connection. Both stay pinned to
// the owned proxy and refuse redirects.
func TestPhonesAndMCPClientsUseSeparateTransports(t *testing.T) {
	var conns atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "{}")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	phones, mcp := targetClients(&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, server.Listener.Addr().String())
	if phones == mcp || phones.Transport == mcp.Transport || phones.Timeout != 10*time.Second || mcp.Timeout != 10*time.Second {
		t.Fatalf("clients share state or lost their bound: %+v %+v", phones, mcp)
	}
	get := func(client *http.Client, url string) (bool, error) {
		reused := false
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}
		request, _ := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), http.MethodGet, url, nil)
		response, err := client.Do(request)
		if err != nil {
			return reused, err
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return reused, errors.New(response.Status)
		}
		return reused, nil
	}
	if _, err := get(phones, "https://sync.example.test/v1/server"); err != nil || conns.Load() != 1 {
		t.Fatalf("phone request: %v conns=%d", err, conns.Load())
	}
	if reused, err := get(mcp, "https://sync.example.test/mcp"); err != nil || reused || conns.Load() != 2 {
		t.Fatalf("MCP request borrowed a phone connection: reused=%v err=%v conns=%d", reused, err, conns.Load())
	}
	if reused, err := get(phones, "https://sync.example.test/v1/server"); err != nil || !reused || conns.Load() != 2 {
		t.Fatalf("phone connection not kept: reused=%v err=%v conns=%d", reused, err, conns.Load())
	}
	for name, client := range map[string]*http.Client{"phones": phones, "mcp": mcp} {
		if _, err := get(client, "https://other.example.test/"); !errors.Is(err, ErrUnsafeTarget) {
			t.Fatalf("%s reached an unowned target: %v", name, err)
		}
		if _, err := get(client, "https://sync.example.test/redirect"); !errors.Is(err, ErrUnsafeTarget) {
			t.Fatalf("%s followed a redirect: %v", name, err)
		}
	}
}

type refusingTransport struct{}

func (refusingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("phone transport used for MCP")
}

// MCP calls and the route probe use the MCP client when the target has one.
func TestMCPTrafficUsesTheMCPClient(t *testing.T) {
	target := mcpTestTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[],"structuredContent":{}}}`)
	}))
	target.MCPHTTP, target.HTTP = target.HTTP, &http.Client{Transport: refusingTransport{}}
	if outcome, _ := mcpCall(t.Context(), target, mcpCredential{Credential: "btmcp1.a", BabyID: "baby"}, 0, time.Now()); outcome != mcpAdmitted {
		t.Fatalf("MCP call outcome=%s", outcome)
	}
	if status, err := mcpRouteStatus(t.Context(), target); err != nil || status != http.StatusUnauthorized {
		t.Fatalf("route probe status=%d err=%v", status, err)
	}
}
