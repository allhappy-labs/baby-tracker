package protocol_test

import (
	"testing"

	"babytracker/server/internal/protocol"
)

// The default is Phase 5 Task 14's rule: the hub's share of 85% of the
// homelab's 512 MiB container at 59.4 KiB a connection (heap and the stacks
// and socket buffers outside it, the owner's reading), rounded down to 100
// (1,700). The bound is 65,536 so a capacity ramp can pass 10,000.
func TestSocketProcessLimitDefaultAndBounds(t *testing.T) {
	const wantDefault = 1700
	if got := protocol.DefaultLimits().SocketProcessLimit; got != wantDefault {
		t.Fatalf("default %d, want %d", got, wantDefault)
	}
	if protocol.MaxSocketProcessLimit != 65536 {
		t.Fatalf("bound %d", protocol.MaxSocketProcessLimit)
	}
	for value, ok := range map[int]bool{10: true, 1000: true, 1700: true, 10000: true, 10001: true, 65536: true, 65537: false, 9: false, 0: false} {
		limits := protocol.DefaultLimits()
		limits.SocketProcessLimit = value
		if err := limits.Validate(); (err == nil) != ok {
			t.Errorf("socket_process_limit %d: err %v, want accepted=%v", value, err, ok)
		}
	}
}
