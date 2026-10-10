package limits

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"go/build"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func memoryLimiter(t *testing.T, now *time.Time) *Service {
	t.Helper()
	return newLimiter(newMemoryStore(0, 0), testPepper(), func() time.Time { return *now })
}

func TestMemoryEnforcesCapAcrossOneHundredCallers(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	limiter := memoryLimiter(t, &now)
	policy := Policy{Count: 25, Window: time.Minute}
	var allowed, failures atomic.Int32
	var wait sync.WaitGroup
	for range 100 {
		wait.Go(func() {
			decision, err := limiter.Take(context.Background(), ScopeAuthChallengeIP, "192.0.2.99", policy)
			if err != nil {
				failures.Add(1)
				return
			}
			if decision.Allowed {
				allowed.Add(1)
			}
		})
	}
	wait.Wait()
	if failures.Load() != 0 || allowed.Load() != 25 {
		t.Fatalf("allowed=%d failures=%d", allowed.Load(), failures.Load())
	}
}

func TestMemoryWeightedChargesAreAllOrNone(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	limiter := memoryLimiter(t, &now)
	policy := Policy{Count: 3, Window: time.Minute}
	a := Charge{Scope: ScopeAuthRefreshAccount, Key: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Cost: 2, Policy: policy}
	b := Charge{Scope: ScopeAuthRefreshDevice, Key: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Cost: 2, Policy: policy}
	if d, err := limiter.TakeMany(t.Context(), []Charge{a}); err != nil || !d.Allowed || d.Remaining != 1 {
		t.Fatalf("seed %+v %v", d, err)
	}
	if d, err := limiter.TakeMany(t.Context(), []Charge{a, b}); err != nil || d.Allowed || d.RetryAfter != 56*time.Second {
		t.Fatalf("combined %+v %v, want denied with Retry-After 56s (window end 12:04:00)", d, err)
	}
	if d, err := limiter.TakeMany(t.Context(), []Charge{b}); err != nil || !d.Allowed || d.Remaining != 1 {
		t.Fatalf("b was charged by the refused set: %+v %v", d, err)
	}
}

func TestMemoryDenialRetryAfterIsTheLatestBlockingWindowEnd(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	limiter := memoryLimiter(t, &now)
	minute := Charge{Scope: ScopeSyncPullDevice, Key: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", Cost: 1, Policy: Policy{Count: 1, Window: time.Minute}}
	hour := Charge{Scope: ScopeInvitationRedeemAccount, Key: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", Cost: 1, Policy: Policy{Count: 1, Window: time.Hour}}
	if d, _ := limiter.TakeMany(t.Context(), []Charge{minute, hour}); !d.Allowed {
		t.Fatal("first set refused")
	}
	if d, _ := limiter.TakeMany(t.Context(), []Charge{minute, hour}); d.Allowed || d.RetryAfter != 56*time.Minute+56*time.Second {
		t.Fatalf("retry %v, want the hour window's end", d.RetryAfter)
	}
	now = now.Add(57 * time.Second) // next minute window
	if d, _ := limiter.TakeMany(t.Context(), []Charge{minute}); !d.Allowed {
		t.Fatal("next minute window still refused")
	}
}

// Replaces the four "UseIsolatedLegacyStorageNamespaces" tests: there is no
// storage remap left, so each logical scope and each key is its own bucket.
func TestMemoryScopesAndKeysAreIsolated(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	limiter := memoryLimiter(t, &now)
	key := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	one := Policy{Count: 1, Window: time.Minute}
	scopes := []string{ScopeSyncMutationAccount, ScopeAuthRefreshAccount, ScopeLiveAttemptAccount, ScopeExportAccount, ScopeMCPAccount, ScopeFamilyStructuralAccount}
	for _, scope := range scopes {
		if d, err := limiter.Take(t.Context(), scope, key, one); err != nil || !d.Allowed {
			t.Fatalf("%s shares a bucket with another scope: %+v %v", scope, d, err)
		}
	}
	if d, _ := limiter.Take(t.Context(), ScopeSyncMutationAccount, "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", one); !d.Allowed {
		t.Fatal("another key shares the bucket")
	}
	// The live and export IP scopes share a raw key with the legacy auth IP
	// scope; the logical scope keeps their quotas apart.
	for _, scope := range []string{ScopeLiveAttemptIP, ScopeExportIP, ScopeAuthChallengeIP} {
		if d, err := limiter.Take(t.Context(), scope, "192.0.2.99", one); err != nil || !d.Allowed {
			t.Fatalf("%s shares an IP bucket with another scope: %+v %v", scope, d, err)
		}
	}
	if d, _ := limiter.Take(t.Context(), ScopeAuthChallengeIP, "192.0.2.99", one); d.Allowed {
		t.Fatal("a scope's own bucket was not charged")
	}
}

func TestMemoryOversizedWeightedChargeIsDeniedWithoutPartialQuota(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	limiter := memoryLimiter(t, &now)
	small := Charge{Scope: ScopeSyncMutationDevice, Key: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Cost: 1, Policy: Policy{Count: 10, Window: time.Minute}}
	huge := Charge{Scope: ScopeSyncMutationFamily, Key: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Cost: 11, Policy: Policy{Count: 10, Window: time.Minute}}
	if d, _ := limiter.TakeMany(t.Context(), []Charge{small, huge}); d.Allowed {
		t.Fatal("oversized charge admitted")
	}
	if d, _ := limiter.TakeMany(t.Context(), []Charge{small}); d.Remaining != 9 {
		t.Fatalf("refused set charged small: remaining %d", d.Remaining)
	}
}

func TestMemoryExpiredBucketsAreSwept(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	store := newMemoryStore(0, 0)
	limiter := newLimiter(store, testPepper(), func() time.Time { return now })
	for i := range 10000 {
		if _, err := limiter.Take(t.Context(), ScopeLiveAttemptIP, fmt.Sprintf("198.51.%d.%d", i/256, i%256), Policy{Count: 60, Window: time.Minute}); err != nil {
			t.Fatal(err)
		}
	}
	if store.len() != 10000 {
		t.Fatalf("live buckets %d", store.len())
	}
	now = now.Add(2 * time.Minute)
	for i := range 4096 { // 4,096 new keys touch, and so sweep, every one of the 64 shards
		_, _ = limiter.Take(t.Context(), ScopeLiveAttemptIP, fmt.Sprintf("203.0.%d.%d", i/256, i%256), Policy{Count: 60, Window: time.Minute})
	}
	if store.len() != 4096 {
		t.Fatalf("%d buckets live, want only the 4,096 of the current window", store.len())
	}
}

func TestMemoryVerifiedPoolRefusesNewKeysAtCapacity(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	store := newMemoryStore(memoryShards, 0) // one verified bucket per shard
	limiter := newLimiter(store, testPepper(), func() time.Time { return now })
	var full int
	for i := range 2000 {
		_, err := limiter.Take(t.Context(), ScopeSyncPullDevice, testUUID(i), Policy{Count: 120, Window: time.Minute})
		if err != nil {
			if ErrorCodeOf(err) != CodeLimitUnavailable || !IsRetryable(err) {
				t.Fatalf("a full limiter refused with %v", err)
			}
			full++
		}
	}
	if full != 2000-memoryShards || store.len() != memoryShards {
		t.Fatalf("refused=%d live=%d: the verified pool grew past its bound", full, store.len())
	}
	if got := limiter.Stats().VerifiedFull; got != uint64(full) {
		t.Fatalf("VerifiedFull=%d want %d", got, full)
	}
}

// The request path's limiter has no database dependency at all.
func TestLimitsImportNoDatabaseDriver(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imported := range pkg.Imports {
		if strings.Contains(imported, "jackc/pgx") || strings.HasPrefix(imported, "babytracker/server/internal/database") {
			t.Fatalf("limits imports %s", imported)
		}
	}
}

// Store-level tests below drive memoryStore.take with prepared charges whose
// digests the test chooses, as the retired SQL tests drove
// app.take_rate_limits (000005) with raw key digests.

func storeDigest(shard byte, label string) [32]byte {
	digest := sha256.Sum256([]byte(label))
	digest[0] = shard
	return digest
}

func storeCharge(scope string, digest [32]byte, cost, limit int, start, end time.Time) preparedCharge {
	return preparedCharge{scope: scope, digest: digest, cost: cost, limit: limit, windowStart: start, windowEnd: end}
}

func (s *memoryStore) countOf(charge preparedCharge) int {
	shard := &s.shards[shardOf(charge.digest)]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	return shard.pools[poolOf[charge.scope]][bucketKey{scope: charge.scope, digest: charge.digest, windowStart: charge.windowStart.UnixMicro()}].count
}

func (s *memoryStore) scopeLen(scope string) int {
	total := 0
	for i := range s.shards {
		s.shards[i].mu.Lock()
		for key := range s.shards[i].pools[poolOf[scope]] {
			if key.scope == scope {
				total++
			}
		}
		s.shards[i].mu.Unlock()
	}
	return total
}

func testUUID(i int) string { return fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i) }

func assertStoreDecision(t *testing.T, store *memoryStore, charges []preparedCharge, now time.Time, allowed bool, remaining int, retry time.Duration) {
	t.Helper()
	decision, err := store.take(t.Context(), charges, now)
	if err != nil || decision.Allowed != allowed || decision.Remaining != remaining || decision.RetryAfter != retry {
		t.Fatalf("decision=%+v err=%v want allowed=%v remaining=%d retry=%v", decision, err, allowed, remaining, retry)
	}
}

// Port of database.TestTakeRateLimitsIsWeightedAtomicAndCappedAcross100Callers.
func TestMemoryIsWeightedAtomicAndCappedAcross100Callers(t *testing.T) {
	store := newMemoryStore(0, 0)
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	windowEnd := now.Add(10 * time.Minute)
	concurrent := storeCharge(ScopeAuthChallengeIP, sha256.Sum256([]byte("concurrent-rate-key")), 2, 50, now, windowEnd)

	start := make(chan struct{})
	var allowed, unexpected atomic.Int32
	var wait sync.WaitGroup
	for range 100 {
		wait.Go(func() {
			<-start
			decision, err := store.take(context.Background(), []preparedCharge{concurrent}, now)
			switch {
			case err != nil:
				unexpected.Add(1)
			case decision.Allowed:
				allowed.Add(1)
			case decision.Remaining != 0 || decision.RetryAfter != 600*time.Second:
				unexpected.Add(1)
			}
		})
	}
	close(start)
	wait.Wait()
	if allowed.Load() != 25 || unexpected.Load() != 0 {
		t.Fatalf("allowed=%d unexpected=%d", allowed.Load(), unexpected.Load())
	}
	if count := store.countOf(concurrent); count != 50 {
		t.Fatalf("stored weighted count=%d want=50", count)
	}

	// Charge A would succeed and charge B is already full. The denial must
	// not leak a partial admission of A.
	var keyA, keyB [32]byte
	for i := range keyB {
		keyB[i] = 0xff
	}
	assertStoreDecision(t, store, []preparedCharge{storeCharge(ScopeAuthRefreshAccount, keyB, 1, 1, now, windowEnd)}, now, true, 0, 0)
	multi := []preparedCharge{
		storeCharge(ScopeAuthRefreshAccount, keyA, 3, 10, now, windowEnd),
		storeCharge(ScopeAuthRefreshAccount, keyB, 1, 1, now, windowEnd),
	}
	assertStoreDecision(t, store, multi, now, false, 0, 600*time.Second)
	if leaked := store.countOf(multi[0]); leaked != 0 {
		t.Fatalf("denied multi-charge leaked %d into the first bucket", leaked)
	}

	// Retry-After is derived from the latest blocking charge, not merely the
	// first canonical key encountered.
	var lateA, lateB [32]byte
	lateA[0], lateB[0] = 0x11, 0x22
	early := storeCharge(ScopeAuthRefreshDevice, lateA, 1, 1, now, now.Add(5*time.Minute))
	late := storeCharge(ScopeAuthRefreshDevice, lateB, 1, 1, now, windowEnd)
	assertStoreDecision(t, store, []preparedCharge{early}, now, true, 0, 0)
	assertStoreDecision(t, store, []preparedCharge{late}, now, true, 0, 0)
	assertStoreDecision(t, store, []preparedCharge{early, late}, now, false, 0, 600*time.Second)
}

// 000005 keys a bucket by (scope, digest, window start) and refuses a charge
// whose window end disagrees with the stored bucket's.
func TestMemoryWindowEndMismatchBlocksLikeTheLegacyBucket(t *testing.T) {
	store := newMemoryStore(0, 0)
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	start := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	digest := storeDigest(7, "mismatch")
	assertStoreDecision(t, store, []preparedCharge{storeCharge(ScopeAuthRefreshDevice, digest, 1, 10, start, start.Add(5*time.Minute))}, now, true, 9, 0)
	longer := storeCharge(ScopeAuthRefreshDevice, digest, 1, 10, start, start.Add(10*time.Minute))
	assertStoreDecision(t, store, []preparedCharge{longer}, now, false, 0, 6*time.Minute+56*time.Second)
	if count := store.countOf(longer); count != 1 {
		t.Fatalf("mismatched window charged the bucket: count=%d", count)
	}
}

// The legacy repository answered a charge whose cost exceeds its limit before
// touching storage: Retry-After is the latest oversized window end, whatever
// else blocks.
func TestMemoryOversizedDenialReportsTheOversizedWindowEnd(t *testing.T) {
	store := newMemoryStore(0, 0)
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	minuteStart, hourStart := now.Truncate(time.Minute), now.Truncate(time.Hour)
	full := storeCharge(ScopeInvitationRedeemAccount, storeDigest(1, "full"), 1, 1, hourStart, hourStart.Add(time.Hour))
	assertStoreDecision(t, store, []preparedCharge{full}, now, true, 0, 0)
	oversized := storeCharge(ScopeSyncMutationDevice, storeDigest(2, "oversized"), 11, 10, minuteStart, minuteStart.Add(time.Minute))
	assertStoreDecision(t, store, []preparedCharge{full, oversized}, now, false, 0, 56*time.Second)
}

// A refusal is a refusal even when the limiter is full: capacity is judged
// only for a set that would otherwise be admitted.
func TestMemoryDenialWinsOverCapacity(t *testing.T) {
	store := newMemoryStore(memoryShards, 0) // one verified bucket per shard
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	start := now.Truncate(time.Minute)
	end := start.Add(time.Minute)
	blocked := storeCharge(ScopeAuthRefreshAccount, storeDigest(9, "blocked"), 1, 1, start, end)
	assertStoreDecision(t, store, []preparedCharge{blocked}, now, true, 0, 0)
	assertStoreDecision(t, store, []preparedCharge{storeCharge(ScopeAuthRefreshDevice, storeDigest(3, "occupant"), 1, 5, start, end)}, now, true, 4, 0)
	newcomer := storeCharge(ScopeAuthRefreshDevice, storeDigest(3, "newcomer"), 1, 5, start, end)
	assertStoreDecision(t, store, []preparedCharge{newcomer, blocked}, now, false, 0, 56*time.Second)
	if _, err := store.take(t.Context(), []preparedCharge{newcomer}, now); err != errLimiterFull {
		t.Fatalf("a new key in a full shard: err=%v", err)
	}
}

// Two new buckets bound for one shard in one take count against the bound
// together.
func TestMemoryCapacityCountsEveryNewBucketOfOneTake(t *testing.T) {
	store := newMemoryStore(memoryShards, 0) // one verified bucket per shard
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	start := now.Truncate(time.Minute)
	pair := []preparedCharge{
		storeCharge(ScopeAuthRefreshAccount, storeDigest(4, "first"), 1, 5, start, start.Add(time.Minute)),
		storeCharge(ScopeAuthRefreshDevice, storeDigest(4, "second"), 1, 5, start, start.Add(time.Minute)),
	}
	if _, err := store.take(t.Context(), pair, now); err != errLimiterFull {
		t.Fatalf("two new buckets overfilled a one-bucket shard: err=%v", err)
	}
	if store.len() != 0 {
		t.Fatalf("a refused take stored %d buckets", store.len())
	}
}

// 000005 keys a bucket by (scope, key digest, window start), and its
// Retry-After rounds the remaining window up to whole seconds. A new window
// starts fresh even when the shard has not yet swept the last one.
func TestMemoryBucketIsKeyedByScopeDigestAndWindowStart(t *testing.T) {
	store := newMemoryStore(0, 0)
	start := time.Date(2026, 9, 20, 12, 3, 0, 0, time.UTC)
	now := start.Add(4*time.Second + 250*time.Millisecond)
	digest := storeDigest(5, "keyed")
	first := storeCharge(ScopeAuthRefreshAccount, digest, 1, 1, start, start.Add(time.Minute))
	assertStoreDecision(t, store, []preparedCharge{first}, now, true, 0, 0)
	// The same digest under another scope is another bucket.
	assertStoreDecision(t, store, []preparedCharge{storeCharge(ScopeAuthRefreshDevice, digest, 1, 1, start, start.Add(time.Minute))}, now, true, 0, 0)
	assertStoreDecision(t, store, []preparedCharge{first}, now, false, 0, 56*time.Second) // ceil(55.75s)

	// Another key in the same shard sweeps it half a second before the window ends.
	late := start.Add(59*time.Second + 500*time.Millisecond)
	assertStoreDecision(t, store, []preparedCharge{storeCharge(ScopeAuthRefreshAccount, storeDigest(5, "neighbour"), 1, 5, start, start.Add(time.Minute))}, late, true, 4, 0)
	next := start.Add(time.Minute)
	second := storeCharge(ScopeAuthRefreshAccount, digest, 1, 1, next, next.Add(time.Minute))
	assertStoreDecision(t, store, []preparedCharge{second}, next, true, 0, 0)
	assertStoreDecision(t, store, []preparedCharge{second}, next, false, 0, time.Minute)
}

// Fix round 1, I1: a flood of new addresses on one IP scope evicts within
// that scope's own sub-cap. Verified-key requests and other IP scopes are
// still admitted, and the flooded scope still limits a repeat offender.
func TestMemoryIPFloodNeverStarvesOtherUsers(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	store := newMemoryStore(0, memoryShards*4) // four buckets per shard for each IP scope
	limiter := newLimiter(store, testPepper(), func() time.Time { return now })
	challenge := Policy{Count: 3, Window: 10 * time.Minute}
	const offender = "192.0.2.200"
	for range 3 {
		if d, err := limiter.Take(t.Context(), ScopeAuthChallengeIP, offender, challenge); err != nil || !d.Allowed {
			t.Fatalf("offender seed %+v %v", d, err)
		}
	}
	for i := range 5000 {
		if d, err := limiter.Take(t.Context(), ScopeAuthChallengeIP, fmt.Sprintf("10.%d.%d.%d", i/65536, i/256%256, i%256), challenge); err != nil || !d.Allowed {
			t.Fatalf("flood key %d refused: %+v %v", i, d, err)
		}
	}
	if live := store.scopeLen(ScopeAuthChallengeIP); live > memoryShards*4 {
		t.Fatalf("flooded scope holds %d buckets, sub-cap %d", live, memoryShards*4)
	}
	if evicted := limiter.Stats().IPEvictions[ScopeAuthChallengeIP]; evicted < 5000-memoryShards*4 {
		t.Fatalf("evictions=%d", evicted)
	}
	if d, err := limiter.Take(t.Context(), ScopeAuthChallengeIP, offender, challenge); err != nil || d.Allowed || d.RetryAfter != 6*time.Minute+56*time.Second {
		t.Fatalf("repeat offender after the flood: %+v %v", d, err)
	}
	pull := Policy{Count: 120, Window: time.Minute}
	for i := range 1000 {
		d, err := limiter.TakeMany(t.Context(), []Charge{
			{Scope: ScopeSyncPullDevice, Key: testUUID(i), Cost: 1, Policy: pull},
			{Scope: ScopeSyncPullAccount, Key: testUUID(100000 + i), Cost: 1, Policy: Policy{Count: 240, Window: time.Minute}},
		})
		if err != nil || !d.Allowed {
			t.Fatalf("sync pull %d refused during an IP flood: %+v %v", i, d, err)
		}
	}
	for _, scope := range []string{ScopeAuthExchangeIP, ScopeAuthRefreshIP, ScopeLiveAttemptIP, ScopeInvitationRedeemIP} {
		policy := DefaultPolicies()[scope]
		if d, err := limiter.Take(t.Context(), scope, "198.51.100.7", policy); err != nil || !d.Allowed {
			t.Fatalf("%s refused during a challenge flood: %+v %v", scope, d, err)
		}
	}
	if s := limiter.Stats(); s.VerifiedFull != 0 || s.IPEvictions[ScopeAuthExchangeIP] != 0 {
		t.Fatalf("stats=%+v", s)
	}
}

// Eviction takes the bucket whose window ends soonest; among equal ends, the
// one with the smallest count, so a repeat offender outlives a flood.
func TestMemoryIPEvictionTakesTheSoonestEndingThenSmallestBucket(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	start := now.Truncate(time.Minute)
	store := newMemoryStore(0, memoryShards*2) // two buckets per shard for each IP scope
	soon := storeCharge(ScopeAuthChallengeIP, storeDigest(3, "soon"), 5, 9, start, start.Add(time.Minute))
	later := storeCharge(ScopeAuthChallengeIP, storeDigest(3, "later"), 1, 9, start, start.Add(10*time.Minute))
	assertStoreDecision(t, store, []preparedCharge{soon}, now, true, 4, 0)
	assertStoreDecision(t, store, []preparedCharge{later}, now, true, 8, 0)
	newcomer := storeCharge(ScopeAuthChallengeIP, storeDigest(3, "newcomer"), 1, 9, start, start.Add(10*time.Minute))
	assertStoreDecision(t, store, []preparedCharge{newcomer}, now, true, 8, 0)
	if store.countOf(soon) != 0 || store.countOf(later) != 1 || store.countOf(newcomer) != 1 {
		t.Fatalf("soon=%d later=%d newcomer=%d: want the soonest-ending bucket evicted", store.countOf(soon), store.countOf(later), store.countOf(newcomer))
	}
	heavy := storeCharge(ScopeAuthChallengeIP, storeDigest(3, "later"), 4, 9, start, start.Add(10*time.Minute))
	assertStoreDecision(t, store, []preparedCharge{heavy}, now, true, 4, 0) // "later" now holds 5
	third := storeCharge(ScopeAuthChallengeIP, storeDigest(3, "third"), 1, 9, start, start.Add(10*time.Minute))
	assertStoreDecision(t, store, []preparedCharge{third}, now, true, 8, 0)
	if store.countOf(later) != 5 || store.countOf(newcomer) != 0 {
		t.Fatalf("later=%d newcomer=%d: want the smaller of two equal-end buckets evicted", store.countOf(later), store.countOf(newcomer))
	}
	if got := store.stats().IPEvictions[ScopeAuthChallengeIP]; got != 2 {
		t.Fatalf("evictions=%d want 2", got)
	}
}

// Fix round 1, M2: a full pool first drops its ended windows, ignoring the
// once-a-second throttle, before it refuses (verified) or evicts (IP).
func TestMemoryFullPoolsSweepBeforeRefusingOrEvicting(t *testing.T) {
	start := time.Date(2026, 9, 20, 12, 3, 0, 0, time.UTC)
	late := start.Add(59*time.Second + 500*time.Millisecond)
	next := start.Add(time.Minute + 200*time.Millisecond)
	nextStart := start.Add(time.Minute)
	for _, scope := range []string{ScopeSyncPullDevice, ScopeLiveAttemptIP} {
		store := newMemoryStore(memoryShards, memoryShards) // one bucket per shard in every pool
		old := storeCharge(scope, storeDigest(6, "old"), 1, 5, start, start.Add(time.Minute))
		assertStoreDecision(t, store, []preparedCharge{old}, late, true, 4, 0) // sweeps shard 6 at :59.5
		fresh := storeCharge(scope, storeDigest(6, "fresh"), 1, 5, nextStart, nextStart.Add(time.Minute))
		assertStoreDecision(t, store, []preparedCharge{fresh}, next, true, 4, 0)
		if s := store.stats(); s.VerifiedFull != 0 || s.IPEvictions[scope] != 0 {
			t.Fatalf("%s: an ended window forced %+v instead of a sweep", scope, s)
		}
	}
}

// Fix round 1, I2: the sweep drops only ended windows. A key at its limit is
// still refused after a neighbour's sweep inside its window.
func TestMemorySweepNeverDeletesALiveBucket(t *testing.T) {
	store := newMemoryStore(0, 0)
	start := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	full := storeCharge(ScopeAuthRefreshAccount, storeDigest(8, "full"), 2, 2, start, start.Add(10*time.Minute))
	assertStoreDecision(t, store, []preparedCharge{full}, start.Add(time.Second), true, 0, 0)
	for i, offset := range []time.Duration{3 * time.Second, time.Minute, 9*time.Minute + 59*time.Second} {
		neighbour := storeCharge(ScopeAuthRefreshDevice, storeDigest(8, fmt.Sprint("neighbour", i)), 1, 5, start, start.Add(10*time.Minute))
		assertStoreDecision(t, store, []preparedCharge{neighbour}, start.Add(offset), true, 4, 0)
		assertStoreDecision(t, store, []preparedCharge{storeCharge(ScopeAuthRefreshAccount, full.digest, 1, 2, start, start.Add(10*time.Minute))}, start.Add(offset), false, 0, 10*time.Minute-offset)
	}
}

// Fix round 1, I2: overlapping multi-shard takes, with charges in every order,
// neither race, overcharge nor deadlock.
func TestMemoryOverlappingMultiShardTakesStayAtomic(t *testing.T) {
	store := newMemoryStore(0, 0)
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	start := now.Truncate(10 * time.Minute)
	const keys, limit = 8, 60
	charges := make([]preparedCharge, keys)
	for i := range charges {
		charges[i] = storeCharge(ScopeAuthRefreshAccount, storeDigest(byte(i*7), fmt.Sprint("overlap", i)), 1, limit, start, start.Add(10*time.Minute))
	}
	var admitted [keys]atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wait sync.WaitGroup
		for worker := range 64 {
			wait.Go(func() {
				for round := range 200 {
					a, b, c := (worker+round)%keys, (worker+2*round+1)%keys, (3*worker+round+5)%keys
					if a == b || b == c || a == c {
						continue
					}
					set := []preparedCharge{charges[a], charges[b], charges[c]}
					if worker%2 == 1 {
						set[0], set[2] = set[2], set[0]
					}
					decision, err := store.take(context.Background(), set, now)
					if err != nil {
						t.Error(err)
						return
					}
					if decision.Allowed {
						admitted[a].Add(1)
						admitted[b].Add(1)
						admitted[c].Add(1)
					}
				}
			})
		}
		wait.Wait()
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("overlapping multi-shard takes deadlocked")
	}
	for i, charge := range charges {
		if count := store.countOf(charge); count > limit || count != int(admitted[i].Load()) {
			t.Fatalf("key %d count=%d admitted=%d limit=%d", i, count, admitted[i].Load(), limit)
		}
	}
}

// Fix round 1, M3: the mismatch blocks in both directions, and the oversized
// Retry-After is the latest of several oversized ends.
func TestMemoryLegacyBranchesHoldInBothDirections(t *testing.T) {
	store := newMemoryStore(0, 0)
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	start := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	digest := storeDigest(7, "shorter")
	assertStoreDecision(t, store, []preparedCharge{storeCharge(ScopeAuthRefreshDevice, digest, 1, 10, start, start.Add(10*time.Minute))}, now, true, 9, 0)
	assertStoreDecision(t, store, []preparedCharge{storeCharge(ScopeAuthRefreshDevice, digest, 1, 10, start, start.Add(5*time.Minute))}, now, false, 0, time.Minute+56*time.Second)

	minuteStart, hourStart := now.Truncate(time.Minute), now.Truncate(time.Hour)
	assertStoreDecision(t, store, []preparedCharge{
		storeCharge(ScopeSyncMutationDevice, storeDigest(1, "minute"), 11, 10, minuteStart, minuteStart.Add(time.Minute)),
		storeCharge(ScopeInvitationRedeemAccount, storeDigest(2, "hour"), 21, 20, hourStart, hourStart.Add(time.Hour)),
	}, now, false, 0, 56*time.Minute+56*time.Second)
}

// Fix round 1, I1: each pool logs its full or evicting state at most once a
// minute, and counts every event.
func TestMemoryPoolsLogAtMostOncePerMinute(t *testing.T) {
	var buffer lockedLog
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	store := newMemoryStore(memoryShards, memoryShards)
	store.logger = slog.New(slog.NewTextHandler(&buffer, nil))
	limiter := newLimiter(store, testPepper(), func() time.Time { return now })
	flood := func(round int) {
		for i := range 500 {
			_, _ = limiter.Take(t.Context(), ScopeAuthExchangeIP, fmt.Sprintf("10.%d.%d.%d", round, i/256, i%256), Policy{Count: 30, Window: 10 * time.Minute})
			_, _ = limiter.Take(t.Context(), ScopeSyncPullDevice, testUUID(round*1000+i), Policy{Count: 120, Window: time.Minute})
		}
	}
	flood(1)
	now = now.Add(30 * time.Second)
	flood(2)
	if evict, full := buffer.count("rate limiter evicted"), buffer.count("rate limiter verified-key pool full"); evict != 1 || full != 1 {
		t.Fatalf("within a minute: evict logs=%d full logs=%d\n%s", evict, full, buffer.String())
	}
	if !strings.Contains(buffer.String(), "scope="+ScopeAuthExchangeIP) {
		t.Fatalf("eviction log names no scope:\n%s", buffer.String())
	}
	now = now.Add(31 * time.Second)
	flood(3)
	if evict, full := buffer.count("rate limiter evicted"), buffer.count("rate limiter verified-key pool full"); evict != 2 || full != 2 {
		t.Fatalf("after a minute: evict logs=%d full logs=%d\n%s", evict, full, buffer.String())
	}
	if s := limiter.Stats(); s.VerifiedFull == 0 || s.IPEvictions[ScopeAuthExchangeIP] == 0 {
		t.Fatalf("stats=%+v", s)
	}
}

type lockedLog struct {
	mu      sync.Mutex
	builder strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.builder.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.builder.String()
}

func (l *lockedLog) count(substring string) int { return strings.Count(l.String(), substring) }

// Measures the heap one bucket costs; the pool sizes in memory.go cite it.
func BenchmarkMemoryBucketFootprint(b *testing.B) {
	const buckets = 1 << 18
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	start := now.Truncate(time.Minute)
	for range b.N {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		store := newMemoryStore(buckets*2, 0)
		for i := range buckets {
			var digest [32]byte
			binary.BigEndian.PutUint64(digest[:8], uint64(i)*0x9e3779b97f4a7c15)
			binary.BigEndian.PutUint64(digest[8:], uint64(i))
			if _, err := store.take(context.Background(), []preparedCharge{storeCharge(ScopeSyncPullDevice, digest, 1, 120, start, start.Add(time.Minute))}, now); err != nil {
				b.Fatal(err)
			}
		}
		runtime.GC()
		runtime.ReadMemStats(&after)
		b.ReportMetric(float64(after.HeapAlloc-before.HeapAlloc)/buckets, "B/bucket")
		runtime.KeepAlive(store)
	}
}

// Eviction never drops a bucket the take itself charges: that would reset its
// count. Only such buckets left, the pool goes over its bound by at most
// maximumCharges.
func TestMemoryEvictionSparesTheTakesOwnBuckets(t *testing.T) {
	store := newMemoryStore(0, memoryShards) // one bucket per shard for each IP scope
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	start := now.Truncate(10 * time.Minute)
	own := storeCharge(ScopeAuthRefreshIP, storeDigest(3, "own"), 2, 3, start, start.Add(10*time.Minute))
	assertStoreDecision(t, store, []preparedCharge{own}, now, true, 1, 0)
	own.cost = 1
	other := storeCharge(ScopeAuthRefreshIP, storeDigest(3, "other"), 1, 3, start, start.Add(10*time.Minute))
	assertStoreDecision(t, store, []preparedCharge{own, other}, now, true, 0, 0)
	if store.countOf(own) != 3 || store.countOf(other) != 1 {
		t.Fatalf("own=%d other=%d: the take evicted its own bucket", store.countOf(own), store.countOf(other))
	}
}
