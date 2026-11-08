package admission

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// At most total requests wait: one more finds the queue full and is
// refused at once, unqueued, so a stall cannot pile up waiters (each may
// hold a buffered body of up to 1 MiB) without bound.
func TestAcquireCapsTheWaiters(t *testing.T) {
	c, held := full(t, 3, 0, MaxWait)
	results := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() {
			ticket, err := c.Acquire(context.Background(), Sync)
			if err == nil {
				ticket.Release()
			}
			results <- err
		}()
	}
	waitFor(t, func() bool { return c.queued() == 3 })
	start := time.Now()
	ticket, err := c.Acquire(context.Background(), Sync)
	if elapsed := time.Since(start); !errors.Is(err, ErrBusy) || ticket.Queued || ticket.Release != nil || elapsed > 50*time.Millisecond {
		t.Fatalf("beyond the cap: ticket %+v err %v after %v", ticket, err, elapsed)
	}
	for _, release := range held {
		release()
	}
	for i := 0; i < 3; i++ {
		if err := <-results; err != nil {
			t.Fatalf("a queued waiter: %v", err)
		}
	}
	if c.queued() != 0 {
		t.Fatal("waiters left")
	}
}

// Waiters that time out, are canceled and are granted at the same moment
// leave every slot accounted for: after the storm the controller admits
// exactly total and holds no waiter.
func TestAcquireRacesLeakNoSlot(t *testing.T) {
	for round := 0; round < 20; round++ {
		c, err := NewWaiting(4, 1, 2*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			class := Sync
			if i%5 == 0 {
				class = MCP
			}
			wg.Go(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i%3)*time.Millisecond)
				defer cancel()
				ticket, err := c.Acquire(ctx, class)
				if err != nil {
					return
				}
				time.Sleep(time.Duration(i%2) * time.Millisecond)
				ticket.Release()
			})
		}
		wg.Wait()
		if c.queued() != 0 {
			t.Fatalf("round %d: %d waiters left", round, c.queued())
		}
		for i := 0; i < 4; i++ {
			if _, ok := c.TryAcquire(Sync); !ok {
				t.Fatalf("round %d: slot %d leaked", round, i)
			}
		}
		if _, ok := c.TryAcquire(Sync); ok {
			t.Fatalf("round %d: capacity inflated", round)
		}
	}
}

// The no-wait path allocates only TryAcquire's release, and a refusal
// allocates nothing.
func TestAcquireWithoutWaitAllocatesAsTryAcquire(t *testing.T) {
	c, _ := New(1, 0)
	try := testing.AllocsPerRun(1000, func() {
		release, _ := c.TryAcquire(Sync)
		release()
	})
	acquire := testing.AllocsPerRun(1000, func() {
		ticket, _ := c.Acquire(context.Background(), Sync)
		ticket.Release()
	})
	held, _ := c.TryAcquire(Sync)
	refused := testing.AllocsPerRun(1000, func() { _, _ = c.Acquire(context.Background(), Sync) })
	held()
	if acquire != try || refused != 0 {
		t.Fatalf("allocations: TryAcquire %v, Acquire %v, refused %v", try, acquire, refused)
	}
}
