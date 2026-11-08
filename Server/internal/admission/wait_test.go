package admission

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// full returns a controller of total slots, all held by Sync, and the
// releases of the held slots.
func full(t *testing.T, total, mcpMax int, wait time.Duration) (*Controller, []func()) {
	t.Helper()
	c, err := NewWaiting(total, mcpMax, wait)
	if err != nil {
		t.Fatal(err)
	}
	var held []func()
	for i := 0; i < total; i++ {
		release, ok := c.TryAcquire(Sync)
		if !ok {
			t.Fatal("slot refused below total")
		}
		held = append(held, release)
	}
	return c, held
}

func TestWaitingBounds(t *testing.T) {
	for _, wait := range []time.Duration{-time.Nanosecond, MaxWait + time.Nanosecond} {
		if _, err := NewWaiting(8, 0, wait); err == nil {
			t.Fatalf("accepted wait %v", wait)
		}
	}
	for _, wait := range []time.Duration{0, time.Millisecond, MaxWait} {
		if _, err := NewWaiting(8, 0, wait); err != nil {
			t.Fatalf("refused wait %v: %v", wait, err)
		}
	}
	if _, err := NewWaiting(1, 1, 0); err == nil {
		t.Fatal("the total and MCP bounds are New's")
	}
}

// With no wait, Acquire is TryAcquire: a full controller refuses at once,
// whatever the context, and nothing queues.
func TestAcquireWithoutWaitRefusesAtOnce(t *testing.T) {
	c, held := full(t, 2, 0, 0)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, ctx := range []context.Context{context.Background(), canceled} {
		start := time.Now()
		ticket, err := c.Acquire(ctx, Sync)
		if !errors.Is(err, ErrBusy) || ticket.Release != nil || ticket.Queued || ticket.Waited != 0 {
			t.Fatalf("ticket %+v err %v", ticket, err)
		}
		if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
			t.Fatalf("a refusal took %v", elapsed)
		}
	}
	if c.queued() != 0 {
		t.Fatalf("%d waiters", c.queued())
	}
	held[0]()
	ticket, err := c.Acquire(canceled, Sync)
	if err != nil || ticket.Release == nil || ticket.Queued {
		t.Fatalf("a free slot: ticket %+v err %v", ticket, err)
	}
	ticket.Release()
	held[1]()
}

// A free slot is taken without waiting, as TryAcquire takes it.
func TestAcquireFreeSlotDoesNotQueue(t *testing.T) {
	c, err := NewWaiting(1, 0, MaxWait)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := c.Acquire(context.Background(), Sync)
	if err != nil || ticket.Queued || ticket.Waited != 0 {
		t.Fatalf("ticket %+v err %v", ticket, err)
	}
	ticket.Release()
	ticket.Release()
	if release, ok := c.TryAcquire(Sync); !ok {
		t.Fatal("release leaked")
	} else {
		release()
	}
}

// A request that finds no slot waits; a release hands its slot to the first
// waiter, which is admitted, and to no one else.
func TestAcquireWaitsThenIsAdmitted(t *testing.T) {
	c, held := full(t, 2, 0, MaxWait)
	got := make(chan Ticket, 1)
	go func() {
		ticket, err := c.Acquire(context.Background(), Sync)
		if err != nil {
			t.Error(err)
		}
		got <- ticket
	}()
	waitFor(t, func() bool { return c.queued() == 1 })
	time.Sleep(20 * time.Millisecond)
	held[0]()
	ticket := <-got
	if ticket.Release == nil || !ticket.Queued || ticket.Waited < 20*time.Millisecond || ticket.Waited > MaxWait {
		t.Fatalf("ticket %+v", ticket)
	}
	// The slot went to the waiter: the controller is still full.
	if _, ok := c.TryAcquire(Sync); ok {
		t.Fatal("the released slot was free for another request")
	}
	ticket.Release()
	held[1]()
	for i := 0; i < 2; i++ {
		if _, ok := c.TryAcquire(Sync); !ok {
			t.Fatal("slots leaked")
		}
	}
}

// A request still without a slot when its wait ends is refused busy.
func TestAcquireWaitsThenIsRefused(t *testing.T) {
	c, held := full(t, 1, 0, 30*time.Millisecond)
	start := time.Now()
	ticket, err := c.Acquire(context.Background(), Sync)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrBusy) || ticket.Release != nil || !ticket.Queued || ticket.Waited < 30*time.Millisecond {
		t.Fatalf("ticket %+v err %v", ticket, err)
	}
	if elapsed < 30*time.Millisecond || elapsed > time.Second {
		t.Fatalf("refused after %v", elapsed)
	}
	if c.queued() != 0 {
		t.Fatal("the refused request stayed queued")
	}
	// Its place is gone: the next release frees the slot.
	held[0]()
	if release, ok := c.TryAcquire(Sync); !ok {
		t.Fatal("the slot went to a request that had left")
	} else {
		release()
	}
}

// A request whose context ends while it waits leaves at once with the
// context's error, and its place does not take a later slot.
func TestAcquireWaitEndsWithTheRequest(t *testing.T) {
	c, held := full(t, 1, 0, MaxWait)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var ticket Ticket
	go func() {
		var err error
		ticket, err = c.Acquire(ctx, Sync)
		done <- err
	}()
	waitFor(t, func() bool { return c.queued() == 1 })
	start := time.Now()
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) || ticket.Release != nil || !ticket.Queued {
		t.Fatalf("ticket %+v err %v", ticket, err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("left %v after the cancel", elapsed)
	}
	if c.queued() != 0 {
		t.Fatal("the canceled request stayed queued")
	}
	held[0]()
	if release, ok := c.TryAcquire(Sync); !ok {
		t.Fatal("the slot went to a canceled request")
	} else {
		release()
	}
}

// Waiters are admitted first come, first served, and an MCP waiter does not
// hold back a Sync waiter behind it when only MCP is at its maximum.
func TestAcquireOrderAndClasses(t *testing.T) {
	c, err := NewWaiting(3, 1, MaxWait)
	if err != nil {
		t.Fatal(err)
	}
	mcp, ok := c.TryAcquire(MCP)
	if !ok {
		t.Fatal("mcp refused")
	}
	a, _ := c.TryAcquire(Sync)
	b, _ := c.TryAcquire(Sync)
	order := make(chan string, 3)
	finish := make(chan struct{})
	var wg sync.WaitGroup
	for _, w := range []struct {
		name  string
		class Class
	}{{"mcp", MCP}, {"sync1", Sync}, {"sync2", Sync}} {
		wg.Go(func() {
			ticket, err := c.Acquire(context.Background(), w.class)
			if err != nil {
				t.Errorf("%s: %v", w.name, err)
				return
			}
			order <- w.name
			<-finish
			ticket.Release()
		})
		queued := c.queued() + 1
		waitFor(t, func() bool { return c.queued() == queued })
	}
	// A Sync slot frees: the MCP waiter is first but MCP is at its maximum,
	// so the first Sync waiter takes it.
	a()
	if first := <-order; first != "sync1" {
		t.Fatalf("first admitted %s", first)
	}
	// The MCP slot frees: the MCP waiter takes it.
	mcp()
	if next := <-order; next != "mcp" {
		t.Fatalf("then %s", next)
	}
	b()
	if last := <-order; last != "sync2" {
		t.Fatalf("last %s", last)
	}
	close(finish)
	wg.Wait()
	if c.queued() != 0 {
		t.Fatal("waiters left")
	}
}

// Under contention every request is admitted or refused exactly once, the
// total is never exceeded and no slot leaks.
func TestAcquireConcurrentWaiters(t *testing.T) {
	c, err := NewWaiting(4, 1, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	inFlight, peak := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		class := Sync
		if i%4 == 0 {
			class = MCP
		}
		wg.Go(func() {
			ticket, err := c.Acquire(context.Background(), class)
			if err != nil {
				if !errors.Is(err, ErrBusy) {
					t.Error(err)
				}
				return
			}
			mu.Lock()
			inFlight++
			peak = max(peak, inFlight)
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
			ticket.Release()
		})
	}
	wg.Wait()
	if peak > 4 {
		t.Fatalf("peak %d in flight", peak)
	}
	for i := 0; i < 4; i++ {
		if _, ok := c.TryAcquire(Sync); !ok {
			t.Fatal("slots leaked")
		}
	}
}

// queued is the number of waiting requests.
func (c *Controller) queued() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waiters.Len()
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(time.Millisecond)
	}
}
