package admission

import (
	"sync"
	"testing"
)

func TestPerKeyBoundsEachKeyAndTheTotal(t *testing.T) {
	g, err := NewPerKey(3, 2)
	if err != nil {
		t.Fatal(err)
	}
	a1, ok := g.TryAcquire("a")
	if !ok {
		t.Fatal("first holder refused")
	}
	a2, ok := g.TryAcquire("a")
	if !ok {
		t.Fatal("second holder of a key refused below the per-key maximum")
	}
	if _, ok := g.TryAcquire("a"); ok {
		t.Fatal("a key exceeded its per-key maximum")
	}
	b1, ok := g.TryAcquire("b")
	if !ok {
		t.Fatal("another key refused while the total had room")
	}
	if _, ok := g.TryAcquire("c"); ok {
		t.Fatal("exceeded the total")
	}
	a1()
	a1() // idempotent: must not free a second slot
	if holders, keys := g.Len(); holders != 2 || keys != 2 {
		t.Fatalf("after one release: %d holders, %d keys, want 2 and 2", holders, keys)
	}
	c1, ok := g.TryAcquire("c")
	if !ok {
		t.Fatal("a released slot was not reusable")
	}
	if _, ok := g.TryAcquire("a"); ok {
		t.Fatal("a duplicate release freed a slot")
	}
	for _, release := range []func(){a2, b1, c1} {
		release()
	}
	if holders, keys := g.Len(); holders != 0 || keys != 0 {
		t.Fatalf("all released: %d holders, %d keys, want none", holders, keys)
	}
}

func TestPerKeyRefusesEmptyKeyAndInvalidBounds(t *testing.T) {
	for _, bounds := range [][2]int{{0, 1}, {1, 0}, {-1, 1}, {1, 2}} {
		if _, err := NewPerKey(bounds[0], bounds[1]); err == nil {
			t.Fatalf("accepted %v", bounds)
		}
	}
	g, _ := NewPerKey(1, 1)
	if _, ok := g.TryAcquire(""); ok {
		t.Fatal("empty key admitted")
	}
	var none *PerKey
	if _, ok := none.TryAcquire("a"); ok {
		t.Fatal("nil gate admitted")
	}
}

func TestPerKeyConcurrentHoldersStayBounded(t *testing.T) {
	g, _ := NewPerKey(8, 3)
	start := make(chan struct{})
	var mu sync.Mutex
	perKey := map[string]int{}
	peak, total := map[string]int{}, 0
	maxTotal := 0
	var wg sync.WaitGroup
	for i := range 64 {
		key := string(rune('a' + i%4))
		wg.Go(func() {
			<-start
			release, ok := g.TryAcquire(key)
			if !ok {
				return
			}
			mu.Lock()
			perKey[key]++
			total++
			peak[key] = max(peak[key], perKey[key])
			maxTotal = max(maxTotal, total)
			mu.Unlock()
			mu.Lock()
			perKey[key]--
			total--
			mu.Unlock()
			release()
		})
	}
	close(start)
	wg.Wait()
	for key, n := range peak {
		if n > 3 {
			t.Fatalf("key %s peaked at %d holders", key, n)
		}
	}
	if maxTotal > 8 {
		t.Fatalf("total peaked at %d", maxTotal)
	}
	if holders, keys := g.Len(); holders != 0 || keys != 0 {
		t.Fatalf("leaked %d holders, %d keys", holders, keys)
	}
}
