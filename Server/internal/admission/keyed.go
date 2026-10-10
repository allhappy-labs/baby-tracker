package admission

import (
	"errors"
	"sync"
)

// Keyed admits at most one in-flight holder per key, without a waiting queue,
// and holds at most maxKeys entries. An entry exists only while its holder
// runs: release evicts it, so idle keys cost nothing and the registry cannot
// grow with the number of keys ever seen. Callers key it only with
// authenticated identities (MCP grant IDs), never with request input.
type Keyed struct {
	mu      sync.Mutex
	maxKeys int
	next    uint64
	active  map[string]uint64
}

func NewKeyed(maxKeys int) (*Keyed, error) {
	if maxKeys <= 0 {
		return nil, errors.New("keyed admission requires a positive capacity")
	}
	return &Keyed{maxKeys: maxKeys, active: make(map[string]uint64, maxKeys)}, nil
}

// TryAcquire refuses immediately when key is held or the registry is full.
// Release is idempotent and never evicts a later holder of the same key.
func (g *Keyed) TryAcquire(key string) (release func(), ok bool) {
	if g == nil || key == "" {
		return nil, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, busy := g.active[key]; busy || len(g.active) >= g.maxKeys {
		return nil, false
	}
	g.next++
	token := g.next
	g.active[key] = token
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.active[key] == token {
				delete(g.active, key)
			}
		})
	}, true
}

// Len reports the current number of held keys.
func (g *Keyed) Len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.active)
}

// Held reports whether key currently has an in-flight holder. It never
// creates an entry.
func (g *Keyed) Held(key string) bool {
	if g == nil || key == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	_, held := g.active[key]
	return held
}
