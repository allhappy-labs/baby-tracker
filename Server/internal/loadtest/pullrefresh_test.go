package loadtest

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/protocol"
)

// sessionBoundSync makes the fake's pulls behave as the server's do: a page
// token is bound to the session that started the pull (sync/paging.go
// validatePageBinding answers invalid_page_token to any other), and a
// refresh revokes the parent session's access (session_revoked). page1
// is called while the first page is held, before it is answered.
func sessionBoundSync(t *testing.T, f *fakeV1, d *device, page1 func()) {
	t.Helper()
	var mu sync.Mutex
	revoked := map[string]bool{}
	tokenBearer := map[string]string{}
	f.hook("POST /v1/auth/refresh-recoverable", func(w http.ResponseWriter, _ *http.Request, body []byte) bool {
		var request protocol.RecoverableRefreshRequest
		_ = json.Unmarshal(body, &request)
		mu.Lock()
		revoked[d.access()] = true // the parent's access: the device still holds it until the answer
		mu.Unlock()
		f.refresh(w, body)
		return true
	})
	f.hook("POST /v1/sync", func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		var request protocol.SyncRequest
		_ = json.Unmarshal(body, &request)
		mu.Lock()
		gone := revoked[bearer]
		mu.Unlock()
		if gone {
			writeErrorResponse(w, http.StatusUnauthorized, "session_revoked")
			return true
		}
		var next *string
		if request.PageToken == nil {
			if page1 != nil {
				page1()
			}
			token := "page-2-" + newUUID()
			mu.Lock()
			tokenBearer[token] = bearer
			mu.Unlock()
			next = &token
		} else {
			mu.Lock()
			bound := tokenBearer[*request.PageToken]
			mu.Unlock()
			if bound != bearer {
				writeErrorResponse(w, http.StatusBadRequest, "invalid_page_token")
				return true
			}
		}
		writeJSONResponse(w, http.StatusOK, protocol.SyncResponse{ServerInstanceID: f.instance, RecoveryEpoch: 1, Outcomes: []protocol.Outcome{},
			Changes: []protocol.JournalChange{}, HighWater: protocol.Cursor{ServerInstanceID: f.instance, RecoveryEpoch: 1, FamilyID: d.FamilyID, Sequence: 2},
			NextPageToken: next})
		return true
	})
}

// A device's refresh that falls inside its own pull waits for the pull to
// finish, as the client does: the pull's later pages keep the session they
// started with, so none is refused invalid_page_token or session_revoked.
func TestRefreshWaitsForTheDevicesPullToFinish(t *testing.T) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	d := g.devices[0]
	inPage1 := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	sessionBoundSync(t, f, d, func() {
		once.Do(func() {
			close(inPage1)
			<-release
		})
	})
	pulled := make(chan bool, 1)
	go func() { pulled <- g.pullPages(t.Context(), d, time.Now(), 0, RouteSyncPull) }()
	<-inPage1
	refreshed := make(chan bool, 1)
	go func() { refreshed <- g.refresh(t.Context(), d, time.Now()) }()
	time.Sleep(50 * time.Millisecond) // the refresh, unserialised, commits now
	close(release)
	if !<-pulled {
		t.Fatalf("the pull failed: %v", g.outcomes(RouteSyncPull))
	}
	if !<-refreshed {
		t.Fatal("the refresh failed")
	}
	if got := g.outcomes(RouteSyncPull); got[OutcomeOK] != 2 || len(got) != 1 {
		t.Fatalf("sync_pull outcomes %v, want 2 ok", got)
	}
	syncs, refreshes := f.all("/v1/sync"), f.all("/v1/auth/refresh-recoverable")
	if len(refreshes) != 1 || refreshes[0].At.Before(syncs[len(syncs)-1].At) {
		t.Fatal("the refresh reached the server before the pull's last page")
	}
}

// A pull that arrives during its device's refresh waits for the new tokens
// and pulls with them.
func TestPullWaitsForTheDevicesRefresh(t *testing.T) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	d := g.devices[0]
	sessionBoundSync(t, f, d, nil)
	inRefresh := make(chan struct{})
	release := make(chan struct{})
	refreshHook := f.hooks["POST /v1/auth/refresh-recoverable"]
	f.hook("POST /v1/auth/refresh-recoverable", func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		close(inRefresh)
		<-release
		return refreshHook(w, r, body)
	})
	refreshed := make(chan bool, 1)
	go func() { refreshed <- g.refresh(t.Context(), d, time.Now()) }()
	<-inRefresh
	old := d.access()
	pulled := make(chan bool, 1)
	go func() { pulled <- g.pullPages(t.Context(), d, time.Now(), 0, RouteSyncPull) }()
	time.Sleep(50 * time.Millisecond) // the pull, unserialised, sends the old access now
	close(release)
	if !<-refreshed || !<-pulled {
		t.Fatalf("refresh or pull failed: %v", g.outcomes(RouteSyncPull))
	}
	if got := g.outcomes(RouteSyncPull); got[OutcomeOK] != 2 || len(got) != 1 {
		t.Fatalf("sync_pull outcomes %v, want 2 ok", got)
	}
	for _, r := range f.all("/v1/sync") {
		if r.Header.Get("Authorization") == "Bearer "+old {
			t.Fatal("the pull sent the parent session's access")
		}
	}
}

// A snapshot's pages are bound to the session too.
func TestRefreshWaitsForTheDevicesSnapshotPages(t *testing.T) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	d := g.devices[0]
	sessionBoundSync(t, f, d, nil)
	inPage1 := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	tokens := map[string]string{}
	var mu sync.Mutex
	f.hook("GET /v1/sync/snapshot", func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		var next *string
		if token := r.URL.Query().Get("page_token"); token == "" {
			once.Do(func() {
				close(inPage1)
				<-release
			})
			id := "snap-2-" + newUUID()
			mu.Lock()
			tokens[id] = bearer
			mu.Unlock()
			next = &id
		} else {
			mu.Lock()
			bound := tokens[token]
			mu.Unlock()
			if bound != bearer {
				writeErrorResponse(w, http.StatusBadRequest, "invalid_page_token")
				return true
			}
		}
		writeJSONResponse(w, http.StatusOK, protocol.SnapshotResponse{ServerInstanceID: f.instance, RecoveryEpoch: 1, SnapshotID: newUUID(),
			HighWater: protocol.Cursor{ServerInstanceID: f.instance, RecoveryEpoch: 1, FamilyID: d.FamilyID, Sequence: 1},
			Entities:  []protocol.CanonicalEntity{}, NextPageToken: next})
		return true
	})
	done := make(chan bool, 1)
	go func() { done <- g.snapshotAndPull(t.Context(), d, time.Now(), RouteSyncPull) }()
	<-inPage1
	refreshed := make(chan bool, 1)
	go func() { refreshed <- g.refresh(t.Context(), d, time.Now()) }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if !<-done || !<-refreshed {
		t.Fatalf("snapshot %v pull %v", g.outcomes(RouteSnapshot), g.outcomes(RouteSyncPull))
	}
	if got := g.outcomes(RouteSnapshot); got[OutcomeOK] != 2 || len(got) != 1 {
		t.Fatalf("snapshot_page outcomes %v, want 2 ok", got)
	}
}
