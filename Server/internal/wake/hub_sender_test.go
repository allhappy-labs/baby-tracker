package wake_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/realtime"
	"babytracker/server/internal/wake"
)

const (
	senderInstance = "50000000-0000-4000-8000-000000000001"
	senderFamily   = "b0000000-0000-4000-8000-000000000001"
)

type hintConnection struct {
	mu     sync.Mutex
	writes []realtime.Hint
}

func (c *hintConnection) WriteHint(_ context.Context, hint realtime.Hint) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = append(c.writes, hint)
	return nil
}
func (*hintConnection) Ping(context.Context) error { return nil }
func (*hintConnection) Close(int, string) error    { return nil }

func (c *hintConnection) sequences() []uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]uint64, 0, len(c.writes))
	for _, hint := range c.writes {
		result = append(result, hint.Cursor.Sequence)
	}
	return result
}

func waitSequences(t *testing.T, c *hintConnection, count int) []uint64 {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := c.sequences(); len(got) >= count {
			return got
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("hints %v, want at least %d", c.sequences(), count)
	return nil
}

func bind(t *testing.T, hub *realtime.Hub, account, device string) *hintConnection {
	t.Helper()
	p := access.Principal{AccountID: account, DeviceID: device, SessionID: "60000000-0000-4000-8000-" + device[24:], RecoveryEpoch: 1,
		SecurityRevision: 5, Scope: "family", SessionScope: "family", SessionFamilyID: senderFamily, AccessExpiresAt: time.Now().Add(time.Hour)}
	g := access.Grant{AccountID: account, FamilyID: senderFamily, Role: "caregiver", Revision: 5}
	reservation, err := hub.ReserveAfter(t.Context(), hub.InvalidationMark(), p, g)
	if err != nil {
		t.Fatal(err)
	}
	connection := &hintConnection{}
	release, err := reservation.Bind(connection)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return connection
}

// The Task 7 carry, resolved by a device-targeted re-announce: a connected
// device that was already sent the family's high water but has not pulled it
// is sent it again (the iOS client syncs on any hint its local cursor does
// not cover), and only the woken devices are written. One HubSender call
// tells the hub once per family.
func TestHubSenderWakesAConnectedDeviceThatAlreadySawTheCursor(t *testing.T) {
	hub, err := realtime.NewHub(t.Context(), realtime.HubOptions{Limits: protocol.DefaultLimits(), CoalesceWindow: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	stale := bind(t, hub, "10000000-0000-4000-8000-000000000001", "40000000-0000-4000-8000-000000000001")
	current := bind(t, hub, "10000000-0000-4000-8000-000000000002", "40000000-0000-4000-8000-000000000002")
	server := protocol.ServerInfo{InstanceID: senderInstance, RecoveryEpoch: 1}
	hub.Publish(t.Context(), realtime.Hint{FamilyID: senderFamily,
		Cursor: protocol.Cursor{ServerInstanceID: senderInstance, RecoveryEpoch: 1, FamilyID: senderFamily, Sequence: 12}})
	waitSequences(t, stale, 1)
	waitSequences(t, current, 1)
	sender := wake.HubSender{Hub: hub, Server: server}
	if err := sender.Wake(t.Context(), []wake.Wake{{FamilyID: senderFamily, DeviceIDs: []string{"40000000-0000-4000-8000-000000000001"}, Sequence: 12}}); err != nil {
		t.Fatal(err)
	}
	if got := waitSequences(t, stale, 2); len(got) != 2 || got[1] != 12 {
		t.Fatalf("the lagging connected device got %v, want 12 again", got)
	}
	time.Sleep(20 * time.Millisecond)
	if got := current.sequences(); len(got) != 1 {
		t.Fatalf("a device not named was written %v", got)
	}
	// A device without a connection is not an error: nothing reaches it
	// through the hub (Task 15 pushes it).
	if err := sender.Wake(t.Context(), []wake.Wake{{FamilyID: senderFamily, DeviceIDs: []string{"40000000-0000-4000-8000-0000000000ff"}, Sequence: 12}}); err != nil {
		t.Fatal(err)
	}
	if err := (wake.HubSender{}).Wake(t.Context(), []wake.Wake{{FamilyID: senderFamily, DeviceIDs: []string{"40000000-0000-4000-8000-000000000001"}, Sequence: 12}}); err == nil {
		t.Fatal("a sender without a hub accepted a wake")
	}
}
