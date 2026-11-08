package loadtest

import (
	"sync"
	"sync/atomic"
	"time"
)

// Route classes. Every request the generator sends is recorded under one.
const (
	RouteSyncPull     = "sync_pull"
	RouteSyncWrite    = "sync_write"
	RouteSyncHintPull = "sync_hint_pull"
	RouteRefresh      = "refresh_recoverable"
	RouteRefreshAck   = "refresh_ack"
	RouteSnapshot     = "snapshot_page"
	RouteLive         = "live_handshake"
	RouteMCP          = "mcp_call"
	RoutePushToken    = "push_token"
	RouteLiveActivity = "live_activity"
)

// SyncRoutes are every POST /v1/sync class; the sync gate merges them.
var SyncRoutes = []string{RouteSyncPull, RouteSyncWrite, RouteSyncHintPull}

// OutcomeOK is the only expected outcome; every other outcome string is
// unexpected and counts against the run.
const OutcomeOK = "ok"

// RouteSummary is one route class's latencies and outcome counts, and, for
// a write route, why its rejected_mutation requests' mutations were
// refused: "<status>:<code>" per mutation (rejectionReason).
type RouteSummary struct {
	Latency    Histogram         `json:"latency"`
	Outcomes   map[string]uint64 `json:"outcomes"`
	Rejections map[string]uint64 `json:"rejections,omitempty"`
}

const recorderShards = 64

type recorderShard struct {
	mu     sync.Mutex
	routes map[string]*RouteSummary
}

// Recorder collects per-route latencies from many goroutines. Writers spread
// over 64 independently locked shards; Snapshot merges them.
type Recorder struct {
	shards [recorderShards]recorderShard
	next   atomic.Uint64
	now    func() time.Time // nil: time.Now
}

func NewRecorder() *Recorder {
	r := &Recorder{}
	for i := range r.shards {
		r.shards[i].routes = map[string]*RouteSummary{}
	}
	return r
}

// Done records one finished request: its latency from the intended start
// (not from when it was actually sent, which a stalled server delays) and
// its outcome, and the reasons of its refused mutations.
func (r *Recorder) Done(route string, intended time.Time, outcome string, rejections ...string) {
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	latency := now().Sub(intended)
	s := &r.shards[(fnv64a(route)^r.next.Add(1))%recorderShards]
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.routes[route]
	if rs == nil {
		rs = &RouteSummary{Outcomes: map[string]uint64{}}
		s.routes[route] = rs
	}
	rs.Latency.Record(latency)
	rs.Outcomes[outcome]++
	for _, reason := range rejections {
		if rs.Rejections == nil {
			rs.Rejections = map[string]uint64{}
		}
		rs.Rejections[reason]++
	}
}

// Snapshot returns a merged copy of every route; later Done calls do not
// change it.
func (r *Recorder) Snapshot() map[string]RouteSummary {
	out := map[string]RouteSummary{}
	for i := range r.shards {
		s := &r.shards[i]
		s.mu.Lock()
		for route, rs := range s.routes {
			merged, ok := out[route]
			if !ok {
				merged = RouteSummary{Outcomes: map[string]uint64{}}
			}
			merged.Latency.Merge(&rs.Latency)
			for o, n := range rs.Outcomes {
				merged.Outcomes[o] += n
			}
			for reason, n := range rs.Rejections {
				if merged.Rejections == nil {
					merged.Rejections = map[string]uint64{}
				}
				merged.Rejections[reason] += n
			}
			out[route] = merged
		}
		s.mu.Unlock()
	}
	return out
}

// fnv64a is FNV-1a over the route name, without the hasher allocation.
func fnv64a(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}
