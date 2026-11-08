package loadtest

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

// Every request a device sends with its access token waits for the
// device's refresh in flight and goes with the child's access: a refresh
// revokes the parent session, so a request sent after it began with the
// parent's access would be refused session_revoked. Writes, push-token and
// Live Activity registrations, the live handshake, an export and an import
// all hold the device's session around their bearer requests.
func TestBearerRequestsWaitForTheDevicesRefresh(t *testing.T) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	d := g.writers[0]
	g.devices, g.writers = []*device{d}, []*device{d}
	inRefresh, release := make(chan struct{}), make(chan struct{})
	f.hook("POST /v1/auth/refresh-recoverable", func(w http.ResponseWriter, _ *http.Request, body []byte) bool {
		close(inRefresh)
		<-release
		f.refresh(w, body)
		return true
	})
	f.hook("POST /v1/family/import/resolve", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		writeJSONResponse(w, http.StatusOK, map[string]any{"import_id": newUUID(), "resolver_revision": 1, "mappings": []any{},
			"conflicts": []any{}, "mapping_complete": true, "manifest_complete": true})
		return true
	})
	old := d.access()
	refreshed := make(chan bool, 1)
	go func() { refreshed <- g.refresh(t.Context(), d, time.Now()) }()
	<-inRefresh
	var wg sync.WaitGroup
	for _, op := range []func(){
		func() { g.write(t.Context(), d, time.Now(), RouteSyncWrite, 2) },
		func() { g.opPushToken(t.Context(), time.Now()) },
		func() { g.opLiveActivity(t.Context(), time.Now()) },
		func() { g.dial(t.Context(), d, time.Now()) },
		func() { g.slowExport(t.Context(), d, time.Now(), 0) },
		func() { g.importRecords(t.Context(), d, time.Now(), &importStats{}) },
	} {
		wg.Add(1)
		go func() { defer wg.Done(); op() }()
	}
	time.Sleep(300 * time.Millisecond) // unserialised, every op sends the parent's access now
	close(release)
	if !<-refreshed {
		t.Fatal("the refresh failed")
	}
	wg.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	var began time.Time
	paths := map[string]bool{}
	for _, r := range f.requests {
		if r.Path == "/v1/auth/refresh-recoverable" {
			began = r.At
		}
	}
	for _, r := range f.requests {
		if r.Path == "/v1/auth/refresh-recoverable" || r.At.Before(began) {
			continue
		}
		paths[r.Method+" "+r.Path] = true
		if r.Header.Get("Authorization") == "Bearer "+old {
			t.Errorf("%s %s went with the parent's access after the refresh began", r.Method, r.Path)
		}
	}
	for _, want := range []string{"POST /v1/sync", "PUT /v1/devices/push-token", "PUT /v1/live-activities/device", "GET /v1/live",
		"POST /v1/export", "POST /v1/family/import/resolve"} {
		if !paths[want] {
			t.Errorf("no %s after the refresh began: %v", want, paths)
		}
	}
}
