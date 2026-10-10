package admission

import (
	"errors"
	"sync"
)

// PerKey admits at most perKey in-flight holders per key and total holders
// overall, without a waiting queue. It bounds work done for clients that have
// not authenticated yet (the body read before a sync, import or push-token
// request's credential is checked), keyed by client address: one address can
// hold at most perKey slots, so it cannot take the rest from everyone else.
// An entry exists only while a holder of its key runs, so the registry holds
// at most total keys and never grows with the number of keys ever seen.
type PerKey struct {
	mu            sync.Mutex
	total, perKey int
	active        int
	held          map[string]int
}

// NewPerKey bounds holders to perKey per key and total overall.
func NewPerKey(total, perKey int) (*PerKey, error) {
	if total <= 0 || perKey <= 0 || perKey > total {
		return nil, errors.New("per-key admission requires a positive total and a per-key maximum from 1 through the total")
	}
	return &PerKey{total: total, perKey: perKey, held: make(map[string]int)}, nil
}

// TryAcquire refuses immediately when key already has perKey holders or
// total holders are in flight. Release is idempotent.
func (g *PerKey) TryAcquire(key string) (release func(), ok bool) {
	if g == nil || key == "" {
		return nil, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active >= g.total || g.held[key] >= g.perKey {
		return nil, false
	}
	g.active++
	g.held[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.active--
			if g.held[key]--; g.held[key] == 0 {
				delete(g.held, key)
			}
		})
	}, true
}

// Len reports the holders in flight and the keys they hold.
func (g *PerKey) Len() (holders, keys int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active, len(g.held)
}
