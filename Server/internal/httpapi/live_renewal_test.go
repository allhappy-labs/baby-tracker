package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"babytracker/server/internal/limits"
	"babytracker/server/internal/realtime"
	"babytracker/server/internal/testkit"
	"github.com/coder/websocket"
)

// A device's live slot is held until the server has read its old socket's
// close and the handler has released the lease: a redial that arrives
// first, however soon after the client's close, is refused 429 with
// Retry-After 5 (realtime.ErrCapacity) and never replaces the socket still
// bound. This is the load generator's live_handshake rate_limited: it
// closes its old socket on a token renewal and redials at once, so now and
// then its new upgrade beats the old socket's release (on the load stack
// every such 429 is followed within about a millisecond by the old
// socket's 101 completing).
func TestLiveSameDeviceRedialBeforeTheOldSocketIsReleasedIsRateLimited(t *testing.T) {
	h := testkit.New(t)
	if h.Limits.SocketDeviceLimit != 1 {
		t.Fatalf("socket device limit %d, want the default 1", h.Limits.SocketDeviceLimit)
	}
	hub, err := realtime.NewHub(t.Context(), realtime.HubOptions{Limits: h.Limits, Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	resolver, _ := limits.NewClientIPResolver(nil)
	policies := limits.DefaultPolicies()
	server := httptest.NewServer(New(Options{
		Readiness: readinessFunc(func(context.Context) error { return nil }), InstanceID: h.Info.InstanceID,
		RecoveryEpoch: h.Info.RecoveryEpoch, CanonicalOrigin: h.Info.CanonicalOrigin, SessionService: h.Auth,
		RequestAuthorizer: h.Authority, LiveHub: hub, Limiter: liveAllowLimiter(), ClientIPResolver: resolver,
		LiveAttemptIPPolicy: policies[limits.ScopeLiveAttemptIP], LiveAttemptDevicePolicy: policies[limits.ScopeLiveAttemptDevice],
		LiveAttemptAccountPolicy: policies[limits.ScopeLiveAttemptAccount], LiveAttemptFamilyPolicy: policies[limits.ScopeLiveAttemptFamily],
	}))
	defer server.Close()
	header := http.Header{"Authorization": []string{"Bearer " + h.AccessToken("owner")}, "X-Baby-Tracker-Protocol": []string{"v1"},
		"X-Baby-Tracker-Capabilities": []string{"records-v1,live-hints-v1"}}
	dial := func() (*websocket.Conn, *http.Response, error) {
		return websocket.Dial(t.Context(), server.URL+"/v1/live", &websocket.DialOptions{HTTPHeader: header, Subprotocols: []string{realtime.Subprotocol}})
	}
	old, _, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	defer old.CloseNow()
	readDone := make(chan error, 1)
	go func() { _, _, err := old.Read(t.Context()); readDone <- err }()

	// The old socket's close has not reached the server: the redial is
	// refused, not admitted in its place.
	next, response, err := dial()
	if err == nil {
		next.CloseNow()
		t.Fatal("a second socket of the device was admitted")
	}
	if response == nil || response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") != "5" {
		t.Fatalf("redial response=%v retry-after=%q err=%v", response, responseHeader(response, "Retry-After"), err)
	}
	select {
	case err := <-readDone:
		t.Fatalf("the refused redial ended the bound socket: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// Once the server has read the close and released the lease, the
	// device's next dial upgrades.
	if err := old.Close(websocket.StatusNormalClosure, "token renewed"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		next, response, err = dial()
		if err == nil {
			next.CloseNow()
			break
		}
		if response == nil || response.StatusCode != http.StatusTooManyRequests || time.Now().After(deadline) {
			t.Fatalf("redial after the close: response=%v err=%v", response, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
