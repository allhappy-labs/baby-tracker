package admission

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The Auth lane is its own bound, alongside total: an Auth flood that fills
// it refuses no Sync or MCP request, and Sync and MCP filling total refuse
// no Auth request.
func TestAuthLaneIsIsolatedFromSyncAndMCP(t *testing.T) {
	c, err := NewLanes(4, 1, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	var auth []func()
	for range 2 {
		release, ok := c.TryAcquire(Auth)
		if !ok {
			t.Fatal("auth refused within its lane")
		}
		auth = append(auth, release)
	}
	if _, ok := c.TryAcquire(Auth); ok {
		t.Fatal("auth admitted beyond its lane")
	}
	var shared []func()
	mcp, ok := c.TryAcquire(MCP)
	if !ok {
		t.Fatal("a full auth lane refused MCP")
	}
	shared = append(shared, mcp)
	for range 3 {
		release, ok := c.TryAcquire(Sync)
		if !ok {
			t.Fatal("a full auth lane refused sync")
		}
		shared = append(shared, release)
	}
	if _, ok := c.TryAcquire(Sync); ok {
		t.Fatal("sync admitted beyond total")
	}
	// An auth release frees an auth slot, never a sync one.
	auth[0]()
	auth[0]()
	if _, ok := c.TryAcquire(Sync); ok {
		t.Fatal("an auth release freed a sync slot")
	}
	again, ok := c.TryAcquire(Auth)
	if !ok {
		t.Fatal("a full total refused auth")
	}
	if _, ok := c.TryAcquire(Auth); ok {
		t.Fatal("a duplicate auth release inflated the lane")
	}
	again()
	auth[1]()
	// A sync release frees a sync slot, never an auth one beyond the lane.
	for _, release := range shared {
		release()
	}
	for range 4 {
		release, ok := c.TryAcquire(Sync)
		if !ok {
			t.Fatal("sync release leaked")
		}
		defer release()
	}
	for range 2 {
		release, ok := c.TryAcquire(Auth)
		if !ok {
			t.Fatal("auth release leaked")
		}
		defer release()
	}
	if _, ok := c.TryAcquire(Auth); ok {
		t.Fatal("auth admitted beyond its lane")
	}
}

// The Auth lane never waits, even on a waiting controller: a full lane
// refuses at once, and an Auth request never takes a place in the waiting
// queue, so an Auth flood cannot fill the queue Sync waits in (a full queue
// refuses Sync at once).
func TestAuthLaneNeverWaits(t *testing.T) {
	c, err := NewLanes(1, 0, 1, MaxWait)
	if err != nil {
		t.Fatal(err)
	}
	syncHeld, ok := c.TryAcquire(Sync)
	if !ok {
		t.Fatal("sync refused")
	}
	ticket, err := c.Acquire(context.Background(), Auth)
	if err != nil || ticket.Release == nil || ticket.Queued {
		t.Fatalf("a full total held auth back: %+v %v", ticket, err)
	}
	start := time.Now()
	refused, err := c.Acquire(context.Background(), Auth)
	if !errors.Is(err, ErrBusy) || refused.Release != nil || refused.Queued || refused.Waited != 0 {
		t.Fatalf("a full auth lane: %+v %v", refused, err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("an auth refusal waited %v", elapsed)
	}
	if c.queued() != 0 {
		t.Fatalf("%d waiters after an auth refusal", c.queued())
	}
	// A sync waiter is not handed the slot an auth release frees.
	granted := make(chan Ticket, 1)
	go func() {
		ticket, _ := c.Acquire(context.Background(), Sync)
		granted <- ticket
	}()
	deadline := time.Now().Add(time.Second)
	for c.queued() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	ticket.Release()
	select {
	case got := <-granted:
		t.Fatalf("an auth release admitted a sync waiter: %+v", got)
	case <-time.After(20 * time.Millisecond):
	}
	syncHeld()
	got := <-granted
	if got.Release == nil || !got.Queued {
		t.Fatalf("the sync waiter: %+v", got)
	}
	got.Release()
}

func TestAuthLaneBounds(t *testing.T) {
	for _, auth := range []int{0, -1} {
		if _, err := NewLanes(8, 0, auth, 0); err == nil {
			t.Fatalf("accepted auth lane %d", auth)
		}
	}
	if _, err := NewLanes(1, 1, 1, 0); err == nil {
		t.Fatal("the total and MCP bounds are New's")
	}
	if _, err := NewLanes(8, 0, 1, MaxWait+time.Nanosecond); err == nil {
		t.Fatal("the wait bound is NewWaiting's")
	}
	// New and NewWaiting have the default lane.
	c, _ := New(1, 0)
	for range DefaultAuth {
		if _, ok := c.TryAcquire(Auth); !ok {
			t.Fatal("New's auth lane is below DefaultAuth")
		}
	}
	if _, ok := c.TryAcquire(Auth); ok {
		t.Fatal("New's auth lane is above DefaultAuth")
	}
}
