package admission

import (
	"fmt"
	"sync"
	"testing"
)

func TestKeyedAdmitsOneHolderPerKeyAndEvictsOnRelease(t *testing.T) {
	if _, err := NewKeyed(0); err == nil {
		t.Fatal("zero-capacity registry accepted")
	}
	g, err := NewKeyed(2)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := g.TryAcquire(""); ok {
		t.Fatal("empty key admitted")
	}
	releaseA, ok := g.TryAcquire("a")
	if !ok {
		t.Fatal("first holder refused")
	}
	if _, ok := g.TryAcquire("a"); ok {
		t.Fatal("second concurrent holder of one key admitted")
	}
	releaseB, ok := g.TryAcquire("b")
	if !ok || g.Len() != 2 {
		t.Fatalf("distinct key refused ok=%v len=%d", ok, g.Len())
	}
	// The registry is bounded: a third distinct key is refused while full.
	if _, ok := g.TryAcquire("c"); ok || g.Len() != 2 {
		t.Fatalf("registry grew past its bound: len=%d", g.Len())
	}
	releaseA()
	releaseA() // idempotent: must not evict a later holder of the same key
	if g.Len() != 1 {
		t.Fatalf("released key retained: len=%d", g.Len())
	}
	again, ok := g.TryAcquire("a")
	if !ok {
		t.Fatal("released key not reusable")
	}
	releaseA()
	if _, ok := g.TryAcquire("a"); ok {
		t.Fatal("stale release evicted the current holder")
	}
	again()
	releaseB()
	if g.Len() != 0 {
		t.Fatalf("idle entries retained: len=%d", g.Len())
	}
}

func TestKeyedRegistryStaysBoundedUnderConcurrentChurn(t *testing.T) {
	g, err := NewKeyed(4)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	maxLen := 0
	for worker := 0; worker < 32; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				release, ok := g.TryAcquire(fmt.Sprintf("grant-%d-%d", worker, i))
				mu.Lock()
				maxLen = max(maxLen, g.Len())
				mu.Unlock()
				if ok {
					release()
				}
			}
		}(worker)
	}
	wg.Wait()
	if maxLen > 4 || g.Len() != 0 {
		t.Fatalf("registry max=%d final=%d", maxLen, g.Len())
	}
}

// Held lets a caller confirm that a refusal it already observed for a key is
// still current, without acquiring or creating an entry.
func TestKeyedHeldReportsOnlyCurrentHolders(t *testing.T) {
	g, err := NewKeyed(2)
	if err != nil {
		t.Fatal(err)
	}
	if g.Held("a") || g.Held("") || g.Len() != 0 {
		t.Fatal("idle key reported held or created an entry")
	}
	release, _ := g.TryAcquire("a")
	if !g.Held("a") || g.Held("b") {
		t.Fatal("holder not reported, or another key reported")
	}
	release()
	if g.Held("a") || g.Len() != 0 {
		t.Fatal("released key still reported held")
	}
	var nilGate *Keyed
	if nilGate.Held("a") {
		t.Fatal("nil registry reported a holder")
	}
}
