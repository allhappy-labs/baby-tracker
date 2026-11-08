package httpapi

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// knownBearerPeriod is how long a generation of knownBearers lasts: the
	// default access-token lifetime, so a phone's token is checked before
	// its body about once per token.
	knownBearerPeriod = 10 * time.Minute
	// knownBearerLimit bounds one generation (two are kept): at 32 bytes a
	// key and Go's map overhead, under 4 MiB each when full. A bearer seen
	// once a generation is full is not remembered, and is checked before
	// its body on each request until the next generation.
	knownBearerLimit = 1 << 16
)

// knownBearers remembers the access tokens (by their current-key digest)
// whose credential passed the check a body-first route makes before reading
// its body (admitAfterBody), so a live session's later requests skip that
// statement. It authorizes nothing: every request is still authenticated
// by its own request transaction, and a remembered token that transaction
// refuses is forgotten. Only a token that authenticated is ever remembered,
// so a client without one cannot fill it. A token is remembered for at
// least one period and at most two (a generation is replaced each period).
//
// It counts the checks made and the bearers a full generation could not
// remember, and logs a full generation once a generation (Warn), so the
// cliff where every request of an overflowing bearer pays the check is
// visible.
//
// The same generational set, with its own period and log line, remembers
// the refresh tokens of live sessions this node knows (KnownRefreshes).
type knownBearers struct {
	mu     sync.Mutex
	now    func() time.Time
	logger *slog.Logger
	// name is the full line's message; period is a generation's length.
	name              string
	period            time.Duration
	limit             int
	started           time.Time
	current, previous map[[32]byte]struct{}
	// fullLogged is whether this generation's full line was logged.
	fullLogged bool
	checks     atomic.Uint64
	dropped    atomic.Uint64
}

// newKnownBearers is an empty knownBearers; logger may be nil (no log).
func newKnownBearers(limit int, now func() time.Time, logger *slog.Logger) *knownBearers {
	return newKnownSet("known bearers full", knownBearerPeriod, limit, now, logger)
}

// newKnownSet is an empty generational set of period, logging name when a
// generation is full; logger may be nil (no log).
func newKnownSet(name string, period time.Duration, limit int, now func() time.Time, logger *slog.Logger) *knownBearers {
	return &knownBearers{now: now, logger: logger, name: name, period: period, limit: limit, started: now(), current: map[[32]byte]struct{}{}}
}

// checked counts one credential check made before a body.
func (k *knownBearers) checked() {
	if k != nil {
		k.checks.Add(1)
	}
}

// rotate replaces the generations whose period has passed; k.mu is held.
func (k *knownBearers) rotate() {
	now := k.now()
	elapsed := now.Sub(k.started)
	if elapsed < k.period {
		return
	}
	k.previous = k.current
	if elapsed >= 2*k.period {
		k.previous = nil
	}
	k.current, k.started, k.fullLogged = map[[32]byte]struct{}{}, now, false
}

func (k *knownBearers) known(key [32]byte) bool {
	if k == nil {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rotate()
	if _, ok := k.current[key]; ok {
		return true
	}
	_, ok := k.previous[key]
	return ok
}

func (k *knownBearers) remember(key [32]byte) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rotate()
	if _, ok := k.current[key]; ok || len(k.current) < k.limit {
		k.current[key] = struct{}{}
		return
	}
	dropped := k.dropped.Add(1)
	if !k.fullLogged && k.logger != nil {
		k.fullLogged = true
		k.logger.Warn(k.name, slog.Int("limit", k.limit), slog.Uint64("checks", k.checks.Load()),
			slog.Uint64("dropped", dropped))
	}
}

// seed fills the previous generation with keys, up to the limit, and
// reports how many it holds: they are known until the current generation
// passes, and the current generation stays free for remember.
func (k *knownBearers) seed(keys [][32]byte) int {
	if k == nil {
		return 0
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rotate()
	if k.previous == nil {
		k.previous = make(map[[32]byte]struct{}, min(len(keys), k.limit))
	}
	for _, key := range keys {
		if len(k.previous) >= k.limit {
			break
		}
		k.previous[key] = struct{}{}
	}
	return len(k.previous)
}

func (k *knownBearers) forget(key [32]byte) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.current, key)
	delete(k.previous, key)
}
