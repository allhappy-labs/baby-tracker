package mcpapi

import (
	"crypto/sha256"
	"math"
	"sync"
	"time"
)

const (
	// maxRefusalEntries bounds the repeat-refusal cache. Only credentials
	// that passed CheckIntegration are ever inserted, so the bound is never
	// reached by unauthenticated input; it guards against many valid grants.
	maxRefusalEntries = 1024
	// concurrencyRefusalTTL caps a concurrency entry even if its holder never
	// finishes; it equals the Retry-After the refusal advertises.
	concurrencyRefusalTTL = time.Second
)

type refusalKind uint8

const (
	refusedQuota refusalKind = iota + 1
	refusedConcurrency
)

type refusal struct {
	kind    refusalKind
	until   time.Time
	grantID string // concurrency only: the holder Held confirms
}

// refusalCache remembers recent authenticated over-limit refusals by the
// SHA-256 digest of the exact presented credential string, so a repeat
// request from that credential is refused before global admission and
// before CheckIntegration. It is keyed by the credential, not the grant: a
// peer that only knows a grant ID cannot hit an entry. An entry only ever
// refuses, is inserted only after the full check authenticated the
// credential and the gate or quota refused it, and lasts no longer than that
// refusal: a quota entry until the limiter's retry time, a concurrency entry
// only while the credential's in-flight request still holds the gate (and at
// most concurrencyRefusalTTL). A miss always runs the full check.
type refusalCache struct {
	mu      sync.Mutex
	max     int
	now     func() time.Time
	held    func(grantID string) bool // nil: concurrency refusals are not cached
	entries map[[sha256.Size]byte]refusal
}

func newRefusalCache(max int, now func() time.Time, held func(string) bool) *refusalCache {
	return &refusalCache{max: max, now: now, held: held, entries: make(map[[sha256.Size]byte]refusal)}
}

func credentialDigest(credential string) [sha256.Size]byte {
	return sha256.Sum256([]byte(credential))
}

// lookup returns the refusal to repeat for digest, or false to run the full
// check. Expired or no-longer-held entries are removed.
func (c *refusalCache) lookup(digest [sha256.Size]byte) (failure, bool) {
	c.mu.Lock()
	entry, ok := c.entries[digest]
	now := c.now()
	if ok && !now.Before(entry.until) {
		delete(c.entries, digest)
		ok = false
	}
	c.mu.Unlock()
	if !ok {
		return failure{}, false
	}
	switch entry.kind {
	case refusedQuota:
		return rateLimited(int(max(1, math.Ceil(entry.until.Sub(now).Seconds())))), true
	case refusedConcurrency:
		// Held is consulted outside the cache lock; the gate has its own.
		if c.held != nil && c.held(entry.grantID) {
			return failConcurrency, true
		}
		c.mu.Lock()
		if current, still := c.entries[digest]; still && current == entry {
			delete(c.entries, digest)
		}
		c.mu.Unlock()
	}
	return failure{}, false
}

// refuseQuota records an authenticated quota refusal until retryAfter from now.
func (c *refusalCache) refuseQuota(digest [sha256.Size]byte, retryAfter time.Duration) {
	if retryAfter <= 0 {
		return
	}
	c.insert(digest, refusal{kind: refusedQuota, until: c.now().Add(retryAfter)})
}

// refuseConcurrency records an authenticated concurrency refusal of grantID.
func (c *refusalCache) refuseConcurrency(digest [sha256.Size]byte, grantID string) {
	if c.held == nil || grantID == "" {
		return
	}
	c.insert(digest, refusal{kind: refusedConcurrency, until: c.now().Add(concurrencyRefusalTTL), grantID: grantID})
}

func (c *refusalCache) insert(digest [sha256.Size]byte, entry refusal) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[digest]; !exists && len(c.entries) >= c.max {
		now := c.now()
		for key, existing := range c.entries {
			if !now.Before(existing.until) {
				delete(c.entries, key)
			}
		}
		if len(c.entries) >= c.max {
			// Evict the entry that would end soonest: dropping an entry only
			// means its next request runs the full check.
			var soonest [sha256.Size]byte
			first := true
			for key, existing := range c.entries {
				if first || existing.until.Before(c.entries[soonest].until) {
					soonest, first = key, false
				}
			}
			delete(c.entries, soonest)
		}
	}
	c.entries[digest] = entry
}

func (c *refusalCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
