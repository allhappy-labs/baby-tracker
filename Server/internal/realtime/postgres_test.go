package realtime

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

func TestDecodeNotificationRejectsMalformedDuplicateOversizedForeignAndWrongEpoch(t *testing.T) {
	valid := Hint{FamilyID: testFamily, Cursor: protocol.Cursor{ServerInstanceID: testServer, RecoveryEpoch: 1, FamilyID: testFamily, Sequence: 7}}
	raw, _ := json.Marshal(valid)
	registered := map[string]struct{}{testFamily: {}}
	if got, err := DecodeNotification(raw, testServer, 1, registered); err != nil || got != valid {
		t.Fatalf("valid=%+v err=%v", got, err)
	}
	duplicate := []byte(strings.Replace(string(raw), `"family_id":"`+testFamily+`"`, `"family_id":"`+testFamily+`","family_id":"`+testFamily+`"`, 1))
	foreign := strings.Replace(string(raw), testFamily, "b0000000-0000-4000-8000-000000000099", 1)
	wrongEpoch := strings.Replace(string(raw), `"recovery_epoch":1`, `"recovery_epoch":2`, 1)
	for name, payload := range map[string][]byte{
		"malformed": []byte(`{"family_id":`), "duplicate": duplicate,
		"oversized": []byte(strings.Repeat("x", 1025)), "foreign": []byte(foreign), "wrong epoch": []byte(wrongEpoch),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeNotification(payload, testServer, 1, registered); err == nil {
				t.Fatal("invalid notification accepted")
			}
		})
	}
}

type resyncerStub struct {
	mu     sync.Mutex
	calls  int
	cursor protocol.Cursor
}

func (resyncer *resyncerStub) Current(_ context.Context, lease AuthorizationLease) (protocol.Cursor, error) {
	resyncer.mu.Lock()
	defer resyncer.mu.Unlock()
	resyncer.calls++
	cursor := resyncer.cursor
	cursor.FamilyID = lease.Grant.FamilyID
	return cursor, nil
}

func TestTwoPostgresListenersReceiveNotifyAndReconnectResync(t *testing.T) {
	h := testkit.New(t)
	limits := protocol.DefaultLimits()
	p := principal(h.Owner.DeviceID)
	p.AccountID, p.SessionID, p.SessionFamilyID, p.AccessExpiresAt = h.Owner.AccountID, h.Owner.SessionID, h.FamilyID, h.Now.Add(time.Hour)
	g := grant()
	g.AccountID, g.FamilyID, g.Revision = h.Owner.AccountID, h.FamilyID, h.Owner.SecurityRevision
	hubs := make([]*Hub, 2)
	connections := make([]*testConnection, 2)
	resyncer := &resyncerStub{cursor: protocol.Cursor{ServerInstanceID: h.Info.InstanceID, RecoveryEpoch: h.Info.RecoveryEpoch, Sequence: 3}}
	listeners := make([]*PostgresListener, 2)
	for index := range hubs {
		connections[index] = &testConnection{}
		var err error
		hubs[index], err = NewHub(t.Context(), HubOptions{Limits: limits, Now: func() time.Time { return h.Now }})
		if err != nil {
			t.Fatal(err)
		}
		reservation, reserveErr := hubs[index].Reserve(t.Context(), p, g)
		if reserveErr != nil {
			t.Fatal(reserveErr)
		}
		cleanup, bindErr := reservation.Bind(connections[index])
		if bindErr != nil {
			t.Fatal(bindErr)
		}
		defer cleanup()
		listeners[index], err = StartPostgresListener(t.Context(), PostgresOptions{Pool: h.Runtime, Hub: hubs[index], InstanceID: h.Info.InstanceID, RecoveryEpoch: h.Info.RecoveryEpoch, Resyncer: resyncer})
		if err != nil {
			t.Fatal(err)
		}
		defer listeners[index].Close()
		ready, cancel := context.WithTimeout(t.Context(), time.Second)
		if err = listeners[index].WaitReady(ready); err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		waitWrites(t, connections[index], 1)
	}
	payload, _ := json.Marshal(Hint{FamilyID: h.FamilyID, Cursor: protocol.Cursor{ServerInstanceID: h.Info.InstanceID, RecoveryEpoch: h.Info.RecoveryEpoch, FamilyID: h.FamilyID, Sequence: 4}})
	if _, err := h.Runtime.Exec(t.Context(), `SELECT pg_notify('family_cursor_v1',$1)`, string(payload)); err != nil {
		t.Fatal(err)
	}
	for _, connection := range connections {
		waitWrites(t, connection, 2)
	}
	resyncer.mu.Lock()
	resyncer.cursor.Sequence = 5
	resyncer.mu.Unlock()
	if _, err := h.Admin.Exec(t.Context(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name='baby-tracker-live-listener'`); err != nil {
		t.Fatal(err)
	}
	for _, connection := range connections {
		waitWrites(t, connection, 3)
	}
	resyncer.mu.Lock()
	calls := resyncer.calls
	resyncer.mu.Unlock()
	if calls < 4 {
		t.Fatalf("resync calls=%d", calls)
	}
}

type boundedResyncer struct {
	current atomic.Int32
	maximum atomic.Int32
	calls   atomic.Int32
}

func (resyncer *boundedResyncer) Current(ctx context.Context, lease AuthorizationLease) (protocol.Cursor, error) {
	concurrent := resyncer.current.Add(1)
	defer resyncer.current.Add(-1)
	for {
		maximum := resyncer.maximum.Load()
		if concurrent <= maximum || resyncer.maximum.CompareAndSwap(maximum, concurrent) {
			break
		}
	}
	call := resyncer.calls.Add(1)
	if call == 1 {
		return protocol.Cursor{ServerInstanceID: testServer, RecoveryEpoch: 1, FamilyID: lease.Grant.FamilyID, Sequence: 7}, nil
	}
	<-ctx.Done()
	return protocol.Cursor{}, ctx.Err()
}

func TestReconnectResyncBoundsWorkersDeadlineAndPublishesPartialResult(t *testing.T) {
	limits := protocol.DefaultLimits()
	limits.SocketDeviceLimit, limits.SocketAccountLimit, limits.SocketProcessLimit = 5, 10, 10
	hub, err := NewHub(t.Context(), HubOptions{Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	connections := make([]*testConnection, 6)
	for index := range connections {
		connections[index] = &testConnection{}
		p, g := principal(testDevice[:24]+format12(index+1)), grant()
		p.AccountID = testAccount[:24] + format12(index+1)
		p.SessionID = testSession[:24] + format12(index+1)
		p.SessionFamilyID = testFamily[:24] + format12(index+1)
		g.AccountID, g.FamilyID = p.AccountID, p.SessionFamilyID
		reservation, reserveErr := hub.Reserve(t.Context(), p, g)
		if reserveErr != nil {
			t.Fatal(reserveErr)
		}
		if _, bindErr := reservation.Bind(connections[index]); bindErr != nil {
			t.Fatal(bindErr)
		}
	}
	resyncer := &boundedResyncer{}
	listener := &PostgresListener{ctx: t.Context(), options: PostgresOptions{Hub: hub, Resyncer: resyncer, ResyncTimeout: 50 * time.Millisecond}}
	started := time.Now()
	listener.resync()
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("resync exceeded bounded deadline: %s", elapsed)
	}
	if maximum := resyncer.maximum.Load(); maximum > 4 {
		t.Fatalf("resync workers=%d", maximum)
	}
	written := 0
	for index := range connections {
		connections[index].mu.Lock()
		written += len(connections[index].writes)
		connections[index].mu.Unlock()
	}
	if written != 1 || resyncer.calls.Load() < 4 {
		t.Fatalf("partial resync writes=%d calls=%d", written, resyncer.calls.Load())
	}
}

func waitWrites(t *testing.T, connection *testConnection, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		connection.mu.Lock()
		got := len(connection.writes)
		connection.mu.Unlock()
		if got >= count {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	connection.mu.Lock()
	defer connection.mu.Unlock()
	t.Fatalf("writes=%+v want at least %d", connection.writes, count)
}

func TestNotificationLookupRejectsForeignDomains(t *testing.T) {
	valid := Hint{FamilyID: testFamily, Cursor: protocol.Cursor{ServerInstanceID: testServer, RecoveryEpoch: 1, FamilyID: testFamily, Sequence: 7}}
	raw, _ := json.Marshal(valid)
	for name, payload := range map[string][]byte{
		"malformed": []byte(`{"family_id":`),
		"uuid":      []byte(strings.ReplaceAll(string(raw), testFamily, "bad")),
		"sequence":  []byte(strings.Replace(string(raw), `"sequence":7`, `"sequence":-1`, 1)),
		"instance":  []byte(strings.ReplaceAll(string(raw), testServer, testDevice)),
		"epoch":     []byte(strings.Replace(string(raw), `"recovery_epoch":1`, `"recovery_epoch":2`, 1)),
		"family":    []byte(strings.Replace(string(raw), testFamily, testDevice, 1)),
		"duplicate": []byte(strings.Replace(string(raw), `"family_id":`, `"family_id":"`+testFamily+`","family_id":`, 1)),
		"oversized": []byte(strings.Repeat("x", 1025)),
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			_, err := DecodeNotificationForFamily(payload, testServer, 1, func(string) bool { calls++; return true })
			if err == nil || calls != 0 {
				t.Fatalf("invalid notification err=%v registration calls=%d", err, calls)
			}
		})
	}
	hub, _ := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits()})
	if _, err := DecodeNotificationForFamily(raw, testServer, 1, hub.HasRegisteredFamily); err == nil {
		t.Fatal("unregistered hint accepted")
	}
	if len(hub.RegisteredLeases()) != 0 || hub.HasRegisteredFamily(testFamily) {
		t.Fatal("notification created registration")
	}
	calls := 0
	got, err := DecodeNotificationForFamily(raw, testServer, 1, func(family string) bool { calls++; return family == testFamily })
	if err != nil || got != valid || calls != 1 {
		t.Fatalf("valid=%+v err=%v calls=%d", got, err, calls)
	}
	if _, err := DecodeNotificationForFamily(raw, testServer, 1, nil); err == nil {
		t.Fatal("nil lookup accepted")
	}
}
