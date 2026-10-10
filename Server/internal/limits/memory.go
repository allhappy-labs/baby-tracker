package limits

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Pool sizes, from the load model (about 70 refreshes/s, 500-700 pulls/s,
// under 10 care writes/s, 4,000 live connections), which keeps about 200k
// verified-key buckets live. A bucket costs 192 B of heap, map overhead
// included (BenchmarkMemoryBucketFootprint, go1.26 arm64), so both pools full
// hold at most 1,048,576 buckets, about 192 MiB.
const (
	// DefaultMaxVerifiedEntries bounds the buckets keyed by server-verified
	// IDs: 2.6 times the load model, 96 MiB when full. A full pool refuses
	// new buckets (rate_limit_unavailable); only real growth fills it.
	DefaultMaxVerifiedEntries = 1 << 19
	// DefaultMaxIPEntriesPerScope bounds each IP-keyed scope on its own. The
	// busiest, auth-refresh-ip, needs at most 70/s x 600 s = 42k addresses a
	// window. The eight IP scopes hold at most 524,288 buckets, 96 MiB.
	// A full IP scope evicts within itself and never refuses.
	DefaultMaxIPEntriesPerScope = 1 << 16
)

const (
	memoryShards = 64
	// verifiedPool is pool 0; each IP scope has its own pool after it.
	verifiedPool = 0
	// fullLogEvery is the most often a pool logs that it is full or evicting.
	fullLogEvery = time.Minute
)

// poolOf maps every scope to its pool: verified scopes share pool 0, and
// each IP scope, in name order, has a pool of its own.
var poolOf, poolScopes = func() (map[string]int, []string) {
	pools := map[string]int{}
	scopes := []string{""}
	var ipScopes []string
	for scope, class := range scopeKeyClasses {
		switch class {
		case keyVerified:
			pools[scope] = verifiedPool
		case keyIP:
			ipScopes = append(ipScopes, scope)
		}
	}
	sort.Strings(ipScopes)
	for _, scope := range ipScopes {
		pools[scope] = len(scopes)
		scopes = append(scopes, scope)
	}
	return pools, scopes
}()

type preparedCharge struct {
	scope       string
	digest      [32]byte
	cost, limit int
	windowStart time.Time
	windowEnd   time.Time
}

type rateStore interface {
	take(context.Context, []preparedCharge, time.Time) (Decision, error)
	// peek is take's decision with nothing recorded.
	peek(context.Context, []preparedCharge, time.Time) (Decision, error)
}

// Stats counts the store's capacity events since it started.
type Stats struct {
	// VerifiedFull counts takes refused because the verified-key pool was full.
	VerifiedFull uint64
	// IPEvictions counts, per IP scope, live buckets evicted to admit a new key.
	IPEvictions map[string]uint64
}

type bucketKey struct {
	scope       string
	digest      [32]byte
	windowStart int64 // UnixMicro
}

type bucket struct {
	count     int
	windowEnd time.Time
}

type memoryShard struct {
	mu      sync.Mutex
	pools   []map[bucketKey]bucket // indexed by poolOf
	sweptAt time.Time
}

// memoryStore is app.take_rate_limits (000005) in process memory: fixed
// windows keyed by (scope, key digest, window start), all-or-none across one
// request's charges, and a denial that reports the latest blocking window end.
// Shards are locked in index order, so concurrent multi-charge takes cannot
// deadlock and no lock is global. A shard drops its ended windows at most once
// a second, when it is next used, and at once when a pool is full.
//
// Buckets are split by key class, so a flood of new client addresses cannot
// deny anyone else. The verified-key pool refuses new buckets when full. Each
// IP scope's pool, when full, evicts its own bucket that ends soonest (the
// smallest count among equal ends, so a repeat offender outlives a flood).
// An evicted address loses its count, which the restart ruling accepts: the
// IP scopes guard 32-byte random secrets.
type memoryStore struct {
	shards    [memoryShards]memoryShard
	caps      []int // per shard, indexed by pool
	logger    *slog.Logger
	full      atomic.Uint64
	evictions []atomic.Uint64 // indexed by pool
	loggedAt  []atomic.Int64  // UnixNano of each pool's last log line
}

var errLimiterFull = errors.New("rate limiter full")

func newMemoryStore(maxVerified, maxIPPerScope int) *memoryStore {
	if maxVerified <= 0 {
		maxVerified = DefaultMaxVerifiedEntries
	}
	if maxIPPerScope <= 0 {
		maxIPPerScope = DefaultMaxIPEntriesPerScope
	}
	store := &memoryStore{
		caps:      make([]int, len(poolScopes)),
		evictions: make([]atomic.Uint64, len(poolScopes)),
		loggedAt:  make([]atomic.Int64, len(poolScopes)),
	}
	store.caps[verifiedPool] = max(1, maxVerified/memoryShards)
	for pool := verifiedPool + 1; pool < len(poolScopes); pool++ {
		store.caps[pool] = max(1, maxIPPerScope/memoryShards)
	}
	for i := range store.shards {
		store.shards[i].pools = make([]map[bucketKey]bucket, len(poolScopes))
		for pool := range store.shards[i].pools {
			store.shards[i].pools[pool] = make(map[bucketKey]bucket)
		}
	}
	return store
}

func shardOf(digest [32]byte) int { return int(digest[0]) % memoryShards }

// retryAfter is 000005's greatest(1, ceil(extract(epoch FROM end - now))).
func retryAfter(end, now time.Time) time.Duration {
	return max(time.Second, time.Duration(math.Ceil(end.Sub(now).Seconds()))*time.Second)
}

// takeEvents records what a take did to a full pool, for logging once the
// shard locks are released.
type takeEvents struct {
	full    bool
	evicted []int // pools, one entry per eviction
}

func (s *memoryStore) take(_ context.Context, charges []preparedCharge, now time.Time) (Decision, error) {
	var events takeEvents
	decision, err := s.takeLocked(charges, now, &events, true)
	s.report(events, now)
	return decision, err
}

// peek is the decision take would return, with no bucket created, charged
// or evicted (a shard may still drop its ended windows).
func (s *memoryStore) peek(_ context.Context, charges []preparedCharge, now time.Time) (Decision, error) {
	return s.takeLocked(charges, now, &takeEvents{}, false)
}

func (s *memoryStore) takeLocked(charges []preparedCharge, now time.Time, events *takeEvents, record bool) (Decision, error) {
	if len(charges) == 0 || len(charges) > maximumCharges {
		return Decision{}, errors.New("invalid rate-limit charges")
	}
	pools := make([]int, len(charges))
	// A charge that exceeds its own limit can never be admitted. As the legacy
	// repository did before touching storage, the set is refused with the
	// latest such window end.
	var oversizedEnd time.Time
	for i, c := range charges {
		pool, classified := poolOf[c.scope]
		if !classified {
			return Decision{}, errors.New("unclassified rate-limit scope")
		}
		pools[i] = pool
		if !c.windowEnd.After(now) || c.windowStart.After(now) {
			return Decision{}, errors.New("invalid rate-limit window")
		}
		if c.cost > c.limit && c.windowEnd.After(oversizedEnd) {
			oversizedEnd = c.windowEnd
		}
	}
	if !oversizedEnd.IsZero() {
		return Decision{RetryAfter: retryAfter(oversizedEnd, now)}, nil
	}

	var touched [memoryShards]bool
	for _, c := range charges {
		touched[shardOf(c.digest)] = true
	}
	for i := range s.shards {
		if touched[i] {
			s.shards[i].mu.Lock()
			defer s.shards[i].mu.Unlock()
		}
	}

	keys := make([]bucketKey, len(charges))
	live := make([]bool, len(charges))
	decision := Decision{Allowed: true, Remaining: math.MaxInt}
	var blockingEnd time.Time
	for i, c := range charges {
		shard := &s.shards[shardOf(c.digest)]
		shard.sweep(now)
		keys[i] = bucketKey{scope: c.scope, digest: c.digest, windowStart: c.windowStart.UnixMicro()}
		var current bucket
		current, live[i] = shard.pools[pools[i]][keys[i]]
		if (live[i] && !current.windowEnd.Equal(c.windowEnd)) || current.count+c.cost > c.limit {
			if c.windowEnd.After(blockingEnd) {
				blockingEnd = c.windowEnd
			}
			continue
		}
		decision.Remaining = min(decision.Remaining, c.limit-current.count-c.cost)
	}
	if !blockingEnd.IsZero() {
		return Decision{RetryAfter: retryAfter(blockingEnd, now)}, nil
	}
	if !record {
		return decision, nil
	}

	// The set is admissible. Make room for its new buckets: the verified pool
	// refuses when still full after a sweep; an IP pool evicts.
	type place struct{ shard, pool, count int }
	var places [maximumCharges]place
	used := 0
	for i, c := range charges {
		if live[i] {
			continue
		}
		at := place{shard: shardOf(c.digest), pool: pools[i], count: 1}
		found := false
		for j := range used {
			if places[j].shard == at.shard && places[j].pool == at.pool {
				places[j].count++
				found = true
			}
		}
		if !found {
			places[used] = at
			used++
		}
	}
	for _, at := range places[:used] {
		if at.pool == verifiedPool && !s.shards[at.shard].room(at.pool, at.count, s.caps[at.pool], now) {
			events.full = true
			return Decision{}, errLimiterFull
		}
	}
	for _, at := range places[:used] {
		if at.pool != verifiedPool && !s.shards[at.shard].room(at.pool, at.count, s.caps[at.pool], now) {
			for range s.shards[at.shard].evict(at.pool, at.count, s.caps[at.pool], keys) {
				events.evicted = append(events.evicted, at.pool)
			}
		}
	}
	for i, c := range charges {
		buckets := s.shards[shardOf(c.digest)].pools[pools[i]]
		buckets[keys[i]] = bucket{count: buckets[keys[i]].count + c.cost, windowEnd: c.windowEnd}
	}
	return decision, nil
}

// room reports whether a pool can take count new buckets, first dropping its
// ended windows if it could not.
func (shard *memoryShard) room(pool, count, capacity int, now time.Time) bool {
	if len(shard.pools[pool])+count <= capacity {
		return true
	}
	sweepPool(shard.pools[pool], now)
	return len(shard.pools[pool])+count <= capacity
}

// evict drops the pool's buckets that end soonest, the smallest count first
// among equal ends, until count new buckets fit, and reports how many it
// dropped. It never drops a bucket of the current take; if only those remain,
// the pool goes over its bound by at most maximumCharges.
func (shard *memoryShard) evict(pool, count, capacity int, protected []bucketKey) int {
	buckets := shard.pools[pool]
	dropped := 0
	for len(buckets)+count > capacity {
		var victim bucketKey
		var chosen bucket
		found := false
		for key, candidate := range buckets {
			if isProtected(key, protected) {
				continue
			}
			if !found || candidate.windowEnd.Before(chosen.windowEnd) ||
				(candidate.windowEnd.Equal(chosen.windowEnd) && candidate.count < chosen.count) {
				victim, chosen, found = key, candidate, true
			}
		}
		if !found {
			break
		}
		delete(buckets, victim)
		dropped++
	}
	return dropped
}

func isProtected(key bucketKey, protected []bucketKey) bool {
	for _, own := range protected {
		if key == own {
			return true
		}
	}
	return false
}

func (shard *memoryShard) sweep(now time.Time) {
	if now.Sub(shard.sweptAt) < time.Second {
		return
	}
	shard.sweptAt = now
	for _, buckets := range shard.pools {
		sweepPool(buckets, now)
	}
}

func sweepPool(buckets map[bucketKey]bucket, now time.Time) {
	for key, b := range buckets {
		if !now.Before(b.windowEnd) {
			delete(buckets, key)
		}
	}
}

// report counts a take's capacity events and logs each pool's at most once
// per fullLogEvery. It runs with no shard lock held.
func (s *memoryStore) report(events takeEvents, now time.Time) {
	if events.full {
		total := s.full.Add(1)
		if s.shouldLog(verifiedPool, now) {
			s.logger.Warn("rate limiter verified-key pool full; refusing new buckets",
				"refusals", total, "capacity", s.caps[verifiedPool]*memoryShards)
		}
	}
	for _, pool := range events.evicted {
		total := s.evictions[pool].Add(1)
		if s.shouldLog(pool, now) {
			s.logger.Warn("rate limiter evicted IP buckets; scope at its sub-cap",
				"scope", poolScopes[pool], "evictions", total, "capacity", s.caps[pool]*memoryShards)
		}
	}
}

func (s *memoryStore) shouldLog(pool int, now time.Time) bool {
	if s.logger == nil {
		return false
	}
	last := s.loggedAt[pool].Load()
	if last != 0 && now.UnixNano()-last < int64(fullLogEvery) {
		return false
	}
	return s.loggedAt[pool].CompareAndSwap(last, now.UnixNano())
}

func (s *memoryStore) stats() Stats {
	result := Stats{VerifiedFull: s.full.Load(), IPEvictions: make(map[string]uint64, len(poolScopes)-1)}
	for pool := verifiedPool + 1; pool < len(poolScopes); pool++ {
		result.IPEvictions[poolScopes[pool]] = s.evictions[pool].Load()
	}
	return result
}

func (s *memoryStore) len() int {
	total := 0
	for i := range s.shards {
		s.shards[i].mu.Lock()
		for _, buckets := range s.shards[i].pools {
			total += len(buckets)
		}
		s.shards[i].mu.Unlock()
	}
	return total
}
