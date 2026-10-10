package admission

import (
	"sync"
	"testing"
)

func TestMCPFloodPreservesSyncSlots(t *testing.T) {
	c, err := New(32, 4)
	if err != nil {
		t.Fatal(err)
	}
	var releases []func()
	for i := 0; i < 4; i++ {
		release, ok := c.TryAcquire(MCP)
		if !ok {
			t.Fatal("MCP rejected within reservation")
		}
		releases = append(releases, release)
	}
	if _, ok := c.TryAcquire(MCP); ok {
		t.Fatal("fifth MCP admitted")
	}
	for i := 0; i < 28; i++ {
		release, ok := c.TryAcquire(Sync)
		if !ok {
			t.Fatal("sync reservation lost")
		}
		releases = append(releases, release)
	}
	for _, class := range []Class{Sync, MCP} {
		if _, ok := c.TryAcquire(class); ok {
			t.Fatal("exceeded total")
		}
	}
	var wg sync.WaitGroup
	for _, release := range releases {
		for i := 0; i < 8; i++ {
			wg.Go(release)
		}
	}
	wg.Wait()
	for i := 0; i < 32; i++ {
		release, ok := c.TryAcquire(Sync)
		if !ok {
			t.Fatal("release leaked")
		}
		defer release()
	}
	if _, ok := c.TryAcquire(Sync); ok {
		t.Fatal("duplicate release inflated capacity")
	}
}

func TestAdmissionDisabledMCPAndInvalidClass(t *testing.T) {
	c, _ := New(1, 0)
	for _, class := range []Class{MCP, Class(255)} {
		if _, ok := c.TryAcquire(class); ok {
			t.Fatal("disabled or invalid class admitted")
		}
	}
	release, ok := c.TryAcquire(Sync)
	if !ok {
		t.Fatal("sync disabled")
	}
	release()
}

func TestAdmissionInvalidBounds(t *testing.T) {
	for _, values := range [][2]int{{0, 0}, {-1, 0}, {1, -1}, {1, 1}, {32, 33}} {
		if _, err := New(values[0], values[1]); err == nil {
			t.Fatalf("accepted %v", values)
		}
	}
}

func TestAdmissionConcurrentTotal(t *testing.T) {
	c, _ := New(32, 4)
	start := make(chan struct{})
	held := make(chan Class, 128)
	drain := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 128; i++ {
		class := Sync
		if i%2 == 0 {
			class = MCP
		}
		wg.Go(func() {
			<-start
			release, ok := c.TryAcquire(class)
			if !ok {
				held <- Class(255)
				return
			}
			held <- class
			<-drain
			release()
		})
	}
	close(start)
	total, mcp := 0, 0
	for i := 0; i < 128; i++ {
		class := <-held
		if class != Class(255) {
			total++
		}
		if class == MCP {
			mcp++
		}
	}
	close(drain)
	wg.Wait()
	if total != 32 || mcp > 4 {
		t.Fatalf("total=%d mcp=%d", total, mcp)
	}
}
