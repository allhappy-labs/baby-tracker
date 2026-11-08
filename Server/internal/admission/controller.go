// Package admission bounds finite in-flight work. By default there is no
// waiting queue; NewWaiting opts into a bounded wait for a slot. Auth, the
// requests that come before a caller is identified, has its own lane
// alongside the shared total, and never waits.
package admission

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"
)

type Class uint8

const (
	Sync Class = iota // All finite non-MCP, non-Auth workload requests, including upgrade setup.
	MCP
	// Auth is a request not yet tied to a known caller: sign-in, and a
	// refresh whose token this node has not issued. Its lane is bounded
	// apart from total, so it neither takes Sync's slots nor is refused
	// for want of them.
	Auth
	DefaultTotal = 32
	// DefaultAuth is the Auth lane's default bound (New's and
	// NewWaiting's lane).
	DefaultAuth = 8
)

// MaxWait bounds NewWaiting's wait: a slot not free within it is a stall to
// shed, not one to ride out.
const MaxWait = time.Second

// ErrBusy is Acquire's refusal: no slot was free and the queue was full, or
// none came free within the wait.
var ErrBusy = errors.New("admission: no slot free")

type Controller struct {
	mu                sync.Mutex
	total, mcpMax     int
	active, mcpActive int
	// authMax bounds the Auth lane; authActive is not counted in active.
	authMax, authActive int
	wait                time.Duration
	// waiters are Acquire's requests waiting for a slot (*waiter), first come
	// first; at most total of them (each may hold a buffered request body).
	waiters list.List
}

// waiter is one waiting request: its place in waiters, removed in O(1).
// granted is set, ready closed and the place removed, under mu, when a
// release hands it a slot.
type waiter struct {
	class   Class
	granted bool
	ready   chan struct{}
	place   *list.Element
}

// New reserves at least one slot for Sync. A zero MCP maximum disables MCP.
// This bounds requests, not database connections; MCP also needs its own pool.
// Its Acquire never waits. Its Auth lane is DefaultAuth.
func New(total, mcpMax int) (*Controller, error) {
	return NewWaiting(total, mcpMax, 0)
}

// NewWaiting is New whose Acquire waits up to wait (0 through MaxWait) for
// a slot when none is free, at most total requests at a time. 0 is New: no
// waiting queue.
func NewWaiting(total, mcpMax int, wait time.Duration) (*Controller, error) {
	return NewLanes(total, mcpMax, DefaultAuth, wait)
}

// NewLanes is NewWaiting with an Auth lane of auth slots (at least one)
// alongside total: Auth requests are not counted in total, nor Sync and MCP
// requests in the lane. The lane never waits; wait is Sync's and MCP's.
func NewLanes(total, mcpMax, auth int, wait time.Duration) (*Controller, error) {
	if total <= 0 || mcpMax < 0 || mcpMax >= total {
		return nil, errors.New("admission requires positive total and MCP maximum below total")
	}
	if auth <= 0 {
		return nil, errors.New("admission requires a positive auth lane")
	}
	if wait < 0 || wait > MaxWait {
		return nil, errors.New("admission wait must be from 0 through 1s")
	}
	return &Controller{total: total, mcpMax: mcpMax, authMax: auth, wait: wait}, nil
}

// TryAcquire rejects immediately when full. The caller must retain its token
// until work has stopped, even if its context is canceled. Release is idempotent.
func (c *Controller) TryAcquire(class Class) (release func(), ok bool) {
	c.mu.Lock()
	if !c.admits(class) {
		c.mu.Unlock()
		return nil, false
	}
	c.take(class)
	c.mu.Unlock()
	return c.releaser(class), true
}

// Ticket is Acquire's outcome: Release (nil unless admitted, idempotent, with
// TryAcquire's contract), and whether the request found no slot and waited,
// and for how long.
type Ticket struct {
	Release func()
	Queued  bool
	Waited  time.Duration
}

// Acquire is TryAcquire that, with a wait (NewWaiting), waits up to it for a
// slot when none is free: a release hands its slot to the first waiter it
// admits, so no later request takes it first. It refuses with ErrBusy at
// once when total requests already wait, with ErrBusy when the wait ends
// without a slot, and with ctx's error when ctx ends first.
// Without a wait it is TryAcquire exactly: ctx is not read. Auth never
// waits: a full lane refuses it with ErrBusy at once, and it never takes a
// place in the queue, so an Auth flood cannot fill the queue Sync waits in.
func (c *Controller) Acquire(ctx context.Context, class Class) (Ticket, error) {
	c.mu.Lock()
	if c.admits(class) {
		c.take(class)
		c.mu.Unlock()
		return Ticket{Release: c.releaser(class)}, nil
	}
	if c.wait <= 0 || (class != Sync && class != MCP) || (class == MCP && c.mcpMax == 0) || c.waiters.Len() >= c.total {
		c.mu.Unlock()
		return Ticket{}, ErrBusy
	}
	w := &waiter{class: class, ready: make(chan struct{})}
	w.place = c.waiters.PushBack(w)
	c.mu.Unlock()

	start := time.Now()
	timer := time.NewTimer(c.wait)
	defer timer.Stop()
	var err error
	select {
	case <-w.ready:
	case <-timer.C:
		err = ErrBusy
	case <-ctx.Done():
		err = ctx.Err()
	}
	ticket := Ticket{Queued: true, Waited: time.Since(start)}
	if err != nil {
		c.mu.Lock()
		granted := w.granted
		if !granted {
			c.waiters.Remove(w.place)
		}
		c.mu.Unlock()
		if !granted {
			return ticket, err
		}
		// A release handed it the slot as the wait ended: it is admitted.
	}
	ticket.Release = c.releaser(class)
	return ticket, nil
}

// admits reports whether class may take a slot now. The caller holds mu.
func (c *Controller) admits(class Class) bool {
	if class == Auth {
		return c.authActive < c.authMax
	}
	return (class == Sync || class == MCP) && c.active < c.total && (class != MCP || c.mcpActive < c.mcpMax)
}

// take counts a slot of class taken. The caller holds mu.
func (c *Controller) take(class Class) {
	if class == Auth {
		c.authActive++
		return
	}
	c.active++
	if class == MCP {
		c.mcpActive++
	}
}

// releaser is the idempotent release of one slot of class: it hands the slot
// on to the first waiters it admits, in order. An Auth slot frees only its
// lane, which has no waiters.
func (c *Controller) releaser(class Class) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if class == Auth {
				c.authActive--
				return
			}
			c.active--
			if class == MCP {
				c.mcpActive--
			}
			for place := c.waiters.Front(); place != nil && c.active < c.total; {
				next := place.Next()
				if w := place.Value.(*waiter); c.admits(w.class) {
					c.take(w.class)
					w.granted = true
					close(w.ready)
					c.waiters.Remove(place)
				}
				place = next
			}
		})
	}
}
