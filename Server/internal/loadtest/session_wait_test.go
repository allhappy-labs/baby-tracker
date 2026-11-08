package loadtest

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// The time requests wait for their device's refresh is recorded per
// class (route): how many holds, how many waited at least a millisecond,
// and the wait's quantiles, so a report can tell the serialisation's cost.
func TestSessionWaitsAreRecordedPerClass(t *testing.T) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	d := g.writers[0]
	inRefresh, release := make(chan struct{}), make(chan struct{})
	f.hook("POST /v1/auth/refresh-recoverable", func(w http.ResponseWriter, _ *http.Request, body []byte) bool {
		close(inRefresh)
		<-release
		f.refresh(w, body)
		return true
	})
	if !g.pullPages(t.Context(), d, time.Now(), 0, RouteSyncPull) { // a hold that does not wait
		t.Fatal("pull failed")
	}
	refreshed := make(chan bool, 1)
	go func() { refreshed <- g.refresh(t.Context(), d, time.Now()) }()
	<-inRefresh
	done := make(chan struct{})
	go func() {
		g.pullPages(t.Context(), d, time.Now(), 0, RouteSyncPull)
		g.write(t.Context(), d, time.Now(), RouteSyncWrite, 1)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	close(release)
	<-refreshed
	<-done
	waits := g.sessionWaits.summary()
	pull, write := waits[RouteSyncPull], waits[RouteSyncWrite]
	if pull.Holds != 2 || pull.Waited != 1 || pull.MaxMs < 90 {
		t.Fatalf("sync_pull waits %+v", pull)
	}
	if write.Holds != 1 || write.Waited != 0 {
		t.Fatalf("sync_write waits %+v (the refresh was over)", write)
	}
	if _, ok := waits[RouteRefresh]; !ok {
		t.Fatalf("the refresh's own wait is not recorded: %v", waits)
	}
	raw, _ := json.Marshal(RunResult{SessionWaitByClass: waits})
	var decoded struct {
		Waits map[string]map[string]float64 `json:"session_wait_by_class"`
	}
	if json.Unmarshal(raw, &decoded) != nil || decoded.Waits[RouteSyncPull]["waited"] != 1 {
		t.Fatalf("result JSON %s", raw)
	}
}
