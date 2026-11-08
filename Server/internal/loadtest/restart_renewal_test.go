package loadtest

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/protocol"
)

// The graceful-restart cell (Phase 5 v2 runs/5): a device's refresh failed
// in the restart window (every retry met the stopped server), so its
// attempt stayed pending; its parent access then lapsed, and its next
// request went with it: 401 access_expired outside the window. iOS does
// not do that: FamilySyncEngine.launch runs prepareSession before any
// work, and FamilySessionRenewer.prepare re-sends a pending recovery
// checkpoint's refresh (or renews an expired token) first; work whose
// renewal fails is not sent. A live reconnect is itself a sync
// (liveConnected), so it renews the same way.
func TestARefreshLostInARestartIsRetriedBeforeTheNextRequest(t *testing.T) {
	f := newFakeV1(t)
	var down atomic.Bool
	down.Store(true)
	var mu sync.Mutex
	var attempts []string
	f.hook("POST /v1/auth/refresh-recoverable", func(w http.ResponseWriter, _ *http.Request, body []byte) bool {
		var request protocol.RecoverableRefreshRequest
		_ = json.Unmarshal(body, &request)
		mu.Lock()
		attempts = append(attempts, request.RefreshAttemptID)
		mu.Unlock()
		if down.Load() {
			writeErrorResponse(w, http.StatusServiceUnavailable, "not_ready")
			return true
		}
		return false
	})
	g := testGenerator(t, f, 1)
	d := g.devices[0]
	g.pullers = []*device{d}
	parent := d.access()

	// The restart: the scheduled refresh fails on every try.
	if g.refresh(t.Context(), d, time.Now()) {
		t.Fatal("the refresh succeeded against a stopped server")
	}
	pending := d.state().PendingAttemptID
	if pending == "" {
		t.Fatal("the failed refresh left no pending attempt")
	}

	// The server is back; later the parent's access lapses.
	down.Store(false)
	f.lapsed.Store(parent, true)
	d.mu.Lock()
	d.AccessExpiresAt = time.Now().Add(-time.Second)
	d.mu.Unlock()

	g.opPull(t.Context(), time.Now())

	if got := g.outcomes(RouteSyncPull)[OutcomeAccessExpired]; got != 0 {
		t.Fatalf("the pull met access_expired %d times", got)
	}
	if got := g.outcomes(RouteSyncPull)[OutcomeOK]; got == 0 {
		t.Fatalf("the pull did not succeed: %v", g.outcomes(RouteSyncPull))
	}
	mu.Lock()
	last := attempts[len(attempts)-1]
	mu.Unlock()
	if last != pending {
		t.Fatalf("the renewal used attempt %q, not the pending %q", last, pending)
	}
	for _, r := range f.all("/v1/sync") {
		if r.Header.Get("Authorization") == "Bearer "+parent {
			t.Fatal("a pull went with the lapsed parent's access")
		}
	}
}

// Every request class a device sends with its access renews a pending
// attempt (or a lapsed access) first, as iOS's prepareSession does, and
// so never meets access_expired for it.
func TestEveryRequestRenewsAPendingAttemptFirst(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lapse bool // the parent's access has lapsed too (else only the attempt is pending)
		op    func(g *generator, d *device)
		path  string
	}{
		{"pull", true, func(g *generator, d *device) { g.opPull(t.Context(), time.Now()) }, "/v1/sync"},
		{"pull, access still valid", false, func(g *generator, d *device) { g.opPull(t.Context(), time.Now()) }, "/v1/sync"},
		{"write", true, func(g *generator, d *device) { g.write(t.Context(), d, time.Now(), RouteSyncWrite, 1) }, "/v1/sync"},
		{"snapshot", true, func(g *generator, d *device) { g.snapshotAndPull(t.Context(), d, time.Now(), RouteSyncPull) }, "/v1/sync/snapshot"},
		{"push token", true, func(g *generator, d *device) { g.opPushToken(t.Context(), time.Now()) }, "/v1/devices/push-token"},
		{"live activity", true, func(g *generator, d *device) { g.opLiveActivity(t.Context(), time.Now()) }, "/v1/live-activities/device"},
		{"live dial", true, func(g *generator, d *device) { g.connectLive(t.Context(), d, 0, 0) }, "/v1/live"},
		{"live dial, access still valid", false, func(g *generator, d *device) { g.connectLive(t.Context(), d, 0, 0) }, "/v1/live"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeV1(t)
			g := testGenerator(t, f, 1)
			d := g.writers[0]
			g.devices, g.writers, g.bgWriters, g.pullers = []*device{d}, []*device{d}, []*device{d}, []*device{d}
			parent := d.access()
			d.mu.Lock()
			d.PendingAttemptID = newUUID() // a refresh lost in the restart window
			if tc.lapse {
				d.AccessExpiresAt = time.Now().Add(-time.Second)
			}
			d.mu.Unlock()
			if tc.lapse {
				f.lapsed.Store(parent, true)
			}
			tc.op(g, d)
			if f.count("POST /v1/auth/refresh-recoverable") != 1 {
				t.Fatalf("%d refreshes", f.count("POST /v1/auth/refresh-recoverable"))
			}
			sent := f.all(tc.path)
			if len(sent) == 0 {
				t.Fatalf("no %s was sent", tc.path)
			}
			for _, r := range sent {
				if r.Header.Get("Authorization") == "Bearer "+parent {
					t.Fatalf("%s went with the parent's access", tc.path)
				}
			}
			for route, counts := range g.phases.measure.Snapshot() {
				if counts.Outcomes[OutcomeAccessExpired] != 0 {
					t.Fatalf("%s met access_expired", route)
				}
			}
			if d.state().PendingAttemptID != "" {
				t.Fatal("the attempt is still pending")
			}
		})
	}
}

// A request whose renewal fails is not sent (iOS: prepareSession throws
// and the work does not run); it is counted skipped.
func TestARequestWhoseRenewalFailsIsNotSent(t *testing.T) {
	f := newFakeV1(t)
	f.hook("POST /v1/auth/refresh-recoverable", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		w.Header().Set("Retry-After", "1")
		writeErrorResponse(w, http.StatusServiceUnavailable, "not_ready")
		return true
	})
	g := testGenerator(t, f, 1)
	d := g.devices[0]
	g.pullers = []*device{d}
	d.mu.Lock()
	d.PendingAttemptID = newUUID()
	d.mu.Unlock()
	g.opPull(t.Context(), time.Now())
	if n := f.count("POST /v1/sync"); n != 0 {
		t.Fatalf("%d pulls sent without a renewed session", n)
	}
	if g.skipped[skipAccessNotRenewed] != 1 {
		t.Fatalf("skipped %v", g.skipped)
	}
	if !strings.Contains(strings.Join(mapKeys(g.outcomes(RouteRefresh)), ","), OutcomeNotReady) {
		t.Fatalf("the renewal was not recorded: %v", g.outcomes(RouteRefresh))
	}
}

func mapKeys(m map[string]uint64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
