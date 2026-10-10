// Package admission bounds finite in-flight work without a waiting queue.
package admission

import (
	"errors"
	"sync"
)

type Class uint8

const (
	Sync Class = iota // All finite non-MCP workload requests, including upgrade setup.
	MCP
	DefaultTotal = 32
)

type Controller struct {
	mu                sync.Mutex
	total, mcpMax     int
	active, mcpActive int
}

// New reserves at least one slot for Sync. A zero MCP maximum disables MCP.
// This bounds requests, not database connections; MCP also needs its own pool.
func New(total, mcpMax int) (*Controller, error) {
	if total <= 0 || mcpMax < 0 || mcpMax >= total {
		return nil, errors.New("admission requires positive total and MCP maximum below total")
	}
	return &Controller{total: total, mcpMax: mcpMax}, nil
}

// TryAcquire rejects immediately when full. The caller must retain its token
// until work has stopped, even if its context is canceled. Release is idempotent.
func (c *Controller) TryAcquire(class Class) (release func(), ok bool) {
	c.mu.Lock()
	if (class != Sync && class != MCP) || c.active >= c.total || (class == MCP && c.mcpActive >= c.mcpMax) {
		c.mu.Unlock()
		return nil, false
	}
	c.active++
	if class == MCP {
		c.mcpActive++
	}
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.active--
			if class == MCP {
				c.mcpActive--
			}
		})
	}, true
}
