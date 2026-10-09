package realtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/security"
)

const (
	testAccount = "10000000-0000-4000-8000-000000000001"
	testDevice  = "40000000-0000-4000-8000-000000000001"
	testSession = "60000000-0000-4000-8000-000000000001"
	testFamily  = "b0000000-0000-4000-8000-000000000001"
	testServer  = "50000000-0000-4000-8000-000000000001"
)

type testConnection struct {
	mu         sync.Mutex
	writes     []Hint
	closes     []int
	block      <-chan struct{}
	closeBlock <-chan struct{}
	pings      int
	aborted    bool
}

type firstWriteGateConnection struct {
	mu      sync.Mutex
	writes  []Hint
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (connection *firstWriteGateConnection) WriteHint(ctx context.Context, hint Hint) error {
	blocked := false
	connection.once.Do(func() {
		blocked = true
		close(connection.started)
	})
	if blocked {
		select {
		case <-connection.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	connection.mu.Lock()
	connection.writes = append(connection.writes, hint)
	connection.mu.Unlock()
	return nil
}
func (*firstWriteGateConnection) Ping(context.Context) error { return nil }
func (*firstWriteGateConnection) Close(int, string) error    { return nil }
func (*firstWriteGateConnection) Abort() error               { return nil }

func TestBlockedWriteRejectsLowerAndDuplicatePublishedCursors(t *testing.T) {
	hub, err := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	hub.writeTimeout = time.Second
	connection := &firstWriteGateConnection{started: make(chan struct{}), release: make(chan struct{})}
	reservation, err := hub.Reserve(t.Context(), principal(testDevice), grant())
	if err != nil {
		t.Fatal(err)
	}
	cleanup, err := reservation.Bind(connection)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	cursor := protocol.Cursor{ServerInstanceID: testServer, RecoveryEpoch: 1, FamilyID: testFamily, Sequence: 3}
	hub.Publish(t.Context(), Hint{FamilyID: testFamily, Cursor: cursor})
	select {
	case <-connection.started:
	case <-time.After(time.Second):
		t.Fatal("first write did not start")
	}
	hub.Publish(t.Context(), Hint{FamilyID: testFamily, Cursor: cursor})
	cursor.Sequence = 2
	hub.Publish(t.Context(), Hint{FamilyID: testFamily, Cursor: cursor})
	close(connection.release)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		connection.mu.Lock()
		written := len(connection.writes)
		connection.mu.Unlock()
		if written >= 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if len(connection.writes) != 1 || connection.writes[0].Cursor.Sequence != 3 {
		t.Fatalf("writes=%+v", connection.writes)
	}
}

func (connection *testConnection) WriteHint(ctx context.Context, hint Hint) error {
	if connection.block != nil {
		select {
		case <-connection.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	connection.mu.Lock()
	connection.writes = append(connection.writes, hint)
	connection.mu.Unlock()
	return nil
}
func (connection *testConnection) Ping(context.Context) error {
	connection.mu.Lock()
	connection.pings++
	connection.mu.Unlock()
	return nil
}
func (connection *testConnection) Close(code int, _ string) error {
	if connection.closeBlock != nil {
		<-connection.closeBlock
	}
	connection.mu.Lock()
	connection.closes = append(connection.closes, code)
	connection.mu.Unlock()
	return nil
}

func TestSocketBoundsCoalescingReorderAndSlowConsumerClose(t *testing.T) {
	hub, err := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	hub.writeTimeout = 20 * time.Millisecond
	blocked := make(chan struct{})
	connection := &testConnection{block: blocked}
	reservation, _ := hub.Reserve(t.Context(), principal(testDevice), grant())
	cleanup, err := reservation.Bind(connection)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	cursor := protocol.Cursor{ServerInstanceID: testServer, RecoveryEpoch: 1, FamilyID: testFamily, Sequence: 1}
	hub.Publish(t.Context(), Hint{FamilyID: testFamily, Cursor: cursor})
	cursor.Sequence = 3
	hub.Publish(t.Context(), Hint{FamilyID: testFamily, Cursor: cursor})
	cursor.Sequence = 2
	hub.Publish(t.Context(), Hint{FamilyID: testFamily, Cursor: cursor})
	waitCloses(t, connection, CloseTryAgainLater)
}

func TestCursorDomainChangeClosesSocket(t *testing.T) {
	hub, _ := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits()})
	connection := &testConnection{}
	reservation, _ := hub.Reserve(t.Context(), principal(testDevice), grant())
	cleanup, _ := reservation.Bind(connection)
	defer cleanup()
	cursor := protocol.Cursor{ServerInstanceID: testServer, RecoveryEpoch: 1, FamilyID: testFamily, Sequence: 1}
	hub.Publish(t.Context(), Hint{FamilyID: testFamily, Cursor: cursor})
	cursor.RecoveryEpoch = 2
	hub.Publish(t.Context(), Hint{FamilyID: testFamily, Cursor: cursor})
	waitCloses(t, connection, ClosePolicyViolation)
}

func TestPingDeadlineAndGracefulThenForcedShutdown(t *testing.T) {
	t.Run("ping and deadline", func(t *testing.T) {
		now := time.Now().UTC()
		hub, _ := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits(), Now: func() time.Time { return now }})
		hub.pingInterval = 5 * time.Millisecond
		hub.maximumLife = 20 * time.Millisecond
		p := principal(testDevice)
		p.AccessExpiresAt = now.Add(time.Hour)
		connection := &testConnection{}
		reservation, _ := hub.Reserve(t.Context(), p, grant())
		cleanup, _ := reservation.Bind(connection)
		defer cleanup()
		waitCloses(t, connection, CloseNormal)
		connection.mu.Lock()
		defer connection.mu.Unlock()
		if connection.pings == 0 {
			t.Fatal("socket lifetime sent no ping")
		}
	})
	t.Run("forced", func(t *testing.T) {
		hub, _ := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits()})
		blocked := make(chan struct{})
		connection := &testConnection{closeBlock: blocked}
		reservation, _ := hub.Reserve(t.Context(), principal(testDevice), grant())
		cleanup, _ := reservation.Bind(connection)
		defer cleanup()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		if err := hub.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown err=%v", err)
		}
		connection.mu.Lock()
		aborted := connection.aborted
		connection.mu.Unlock()
		if !aborted {
			t.Fatal("forced shutdown did not abort connection")
		}
		close(blocked)
	})
}

func TestBindRejectsReservationAfterAbsoluteDeadline(t *testing.T) {
	now := time.Now().UTC()
	hub, err := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	hub.maximumLife = 15 * time.Minute
	reservation, err := hub.Reserve(t.Context(), principal(testDevice), grant())
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(15*time.Minute + time.Nanosecond)
	if _, err = reservation.Bind(&testConnection{}); !errors.Is(err, ErrReservationReleased) {
		t.Fatalf("expired bind err=%v", err)
	}
	if _, err = hub.Reserve(t.Context(), principal(testDevice), grant()); err != nil {
		t.Fatalf("expired bind leaked capacity: %v", err)
	}
}

func TestAccessExpiryCancelsBlockedWriteAtAbsoluteDeadline(t *testing.T) {
	now := time.Now().UTC()
	hub, err := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	hub.writeTimeout = time.Second
	p := principal(testDevice)
	p.AccessExpiresAt = now.Add(30 * time.Millisecond)
	blocked := make(chan struct{})
	connection := &testConnection{block: blocked}
	reservation, err := hub.Reserve(t.Context(), p, grant())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reservation.Bind(connection); err != nil {
		t.Fatal(err)
	}
	hub.Publish(t.Context(), Hint{FamilyID: testFamily, Cursor: protocol.Cursor{ServerInstanceID: testServer, RecoveryEpoch: 1, FamilyID: testFamily, Sequence: 1}})
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(hub.RegisteredLeases()) == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("access expiry did not cancel blocked write")
}

func waitCloses(t *testing.T, connection *testConnection, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		connection.mu.Lock()
		for _, code := range connection.closes {
			if code == want {
				connection.mu.Unlock()
				return
			}
		}
		connection.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("close code %d not observed", want)
}
func (connection *testConnection) Abort() error {
	connection.mu.Lock()
	connection.aborted = true
	connection.mu.Unlock()
	return nil
}

type batchAuthorizer struct {
	mu       sync.Mutex
	calls    int
	batchLen int
	allow    bool
	err      error
}

func (authorizer *batchAuthorizer) CheckMany(_ context.Context, leases []AuthorizationLease) ([]bool, error) {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	authorizer.calls++
	authorizer.batchLen = len(leases)
	if authorizer.err != nil {
		return nil, authorizer.err
	}
	allowed := make([]bool, len(leases))
	for index := range allowed {
		allowed[index] = authorizer.allow
	}
	return allowed, nil
}

func TestReservationRaceAdmitsOnlyOnePerDevice(t *testing.T) {
	limits := protocol.DefaultLimits()
	limits.SocketDeviceLimit = 1
	hub, _ := NewHub(t.Context(), HubOptions{Limits: limits})
	start := make(chan struct{})
	release := make(chan struct{})
	var successes atomic.Int32
	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			reservation, err := hub.Reserve(t.Context(), principal(testDevice), grant())
			if err == nil {
				successes.Add(1)
				<-release
				reservation.Release()
			}
		}()
	}
	close(start)
	for successes.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond)
	close(release)
	wait.Wait()
	if successes.Load() != 1 {
		t.Fatalf("reservation race successes=%d", successes.Load())
	}
	// Releasing a winner can allow later racing calls. Capacity must still be
	// bounded at every instant and be fully reusable after the race.
	if _, err := hub.Reserve(t.Context(), principal(testDevice), grant()); err != nil {
		t.Fatalf("reservation race leaked capacity: %v", err)
	}
}

func TestQuarantineAuthorizerFailureClosesSockets(t *testing.T) {
	authorizer := &batchAuthorizer{err: errors.New("authorization_quarantine")}
	hub, _ := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits(), Authorizer: authorizer})
	connection := &testConnection{}
	reservation, _ := hub.Reserve(t.Context(), principal(testDevice), grant())
	cleanup, _ := reservation.Bind(connection)
	defer cleanup()
	if err := hub.sweep(t.Context()); err == nil {
		t.Fatal("authorization failure accepted")
	}
	waitCloses(t, connection, ClosePolicyViolation)
}

func TestInvalidationDetachesAllPeersBeforeConcurrentClose(t *testing.T) {
	hub, _ := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits()})
	blockedClose := make(chan struct{})
	defer close(blockedClose)
	stalled := &testConnection{closeBlock: blockedClose}
	fast := &testConnection{}
	for index, connection := range []Connection{stalled, fast} {
		device := testDevice[:24] + format12(index+1)
		session := testSession[:24] + format12(index+1)
		p := principal(device)
		p.SessionID = session
		reservation, err := hub.Reserve(t.Context(), p, grant())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = reservation.Bind(connection); err != nil {
			t.Fatal(err)
		}
	}
	returned := make(chan struct{})
	go func() {
		hub.Invalidate(t.Context(), security.SecurityInvalidation{FamilyID: testFamily, Revision: 6})
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("invalidation blocked on stalled peer")
	}
	if leases := hub.RegisteredLeases(); len(leases) != 0 {
		t.Fatalf("leases remained after invalidation: %d", len(leases))
	}
	waitCloses(t, fast, ClosePolicyViolation)
	hub.Publish(t.Context(), Hint{FamilyID: testFamily, Cursor: protocol.Cursor{ServerInstanceID: testServer, RecoveryEpoch: 1, FamilyID: testFamily, Sequence: 1}})
	stalled.mu.Lock()
	writes := len(stalled.writes)
	stalled.mu.Unlock()
	if writes != 0 {
		t.Fatalf("invalidated peer received %d hints", writes)
	}
}

func TestSweepDetachesDeniedPeersWithoutWaitingForStalledClose(t *testing.T) {
	authorizer := &batchAuthorizer{allow: false}
	hub, _ := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits(), Authorizer: authorizer})
	blockedClose := make(chan struct{})
	defer close(blockedClose)
	stalled := &testConnection{closeBlock: blockedClose}
	fast := &testConnection{}
	for index, connection := range []Connection{stalled, fast} {
		device := testDevice[:24] + format12(index+10)
		session := testSession[:24] + format12(index+10)
		p := principal(device)
		p.SessionID = session
		reservation, _ := hub.Reserve(t.Context(), p, grant())
		_, _ = reservation.Bind(connection)
	}
	completed := make(chan error, 1)
	go func() { completed <- hub.sweep(t.Context()) }()
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("sweep blocked on stalled peer")
	}
	if leases := hub.RegisteredLeases(); len(leases) != 0 {
		t.Fatalf("leases remained after denied sweep: %d", len(leases))
	}
	waitCloses(t, fast, ClosePolicyViolation)
}

func principal(device string) access.Principal {
	return access.Principal{AccountID: testAccount, DeviceID: device, SessionID: testSession, RecoveryEpoch: 1,
		SecurityRevision: 5, Scope: "family", SessionScope: "family", SessionFamilyID: testFamily,
		AccessExpiresAt: time.Now().Add(time.Hour)}
}
func grant() access.Grant {
	return access.Grant{AccountID: testAccount, FamilyID: testFamily, Role: "owner", Revision: 5}
}

func TestReservationBoundsAndIdempotentRelease(t *testing.T) {
	limits := protocol.DefaultLimits()
	limits.SocketDeviceLimit, limits.SocketAccountLimit, limits.SocketProcessLimit = 1, 3, 10
	hub, err := NewHub(t.Context(), HubOptions{Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	first, err := hub.Reserve(t.Context(), principal(testDevice), grant())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = hub.Reserve(t.Context(), principal(testDevice), grant()); !errors.Is(err, ErrCapacity) {
		t.Fatalf("second reservation err=%v", err)
	}
	first.Release()
	first.Release()
	second, err := hub.Reserve(t.Context(), principal(testDevice), grant())
	if err != nil {
		t.Fatal(err)
	}
	cleanup, err := second.Bind(&testConnection{})
	if err != nil {
		t.Fatal(err)
	}
	second.Release()
	cleanup()
	cleanup()
	if _, err = hub.Reserve(t.Context(), principal(testDevice), grant()); err != nil {
		t.Fatalf("capacity leaked after cleanup: %v", err)
	}
}

func TestInvalidationSelectorsAreConjunctiveAndNewer(t *testing.T) {
	hub, err := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	reservation, _ := hub.Reserve(t.Context(), principal(testDevice), grant())
	connection := &testConnection{}
	cleanup, err := reservation.Bind(connection)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	hub.Invalidate(t.Context(), security.SecurityInvalidation{AccountID: testAccount, Revision: 5})
	hub.Invalidate(t.Context(), security.SecurityInvalidation{AccountID: testAccount, FamilyID: "b0000000-0000-4000-8000-000000000099", Revision: 6})
	connection.mu.Lock()
	before := len(connection.closes)
	connection.mu.Unlock()
	if before != 0 {
		t.Fatal("non-newer or partially matching invalidation closed connection")
	}
	hub.Invalidate(t.Context(), security.SecurityInvalidation{AccountID: testAccount, FamilyID: testFamily, SessionID: testSession, Revision: 6})
	waitCloses(t, connection, ClosePolicyViolation)
}

func TestSweepBatchesOneThousandLeasesInOneAuthorizationCall(t *testing.T) {
	limits := protocol.DefaultLimits()
	limits.SocketDeviceLimit, limits.SocketAccountLimit, limits.SocketProcessLimit = 5, 10, 1000
	authorizer := &batchAuthorizer{allow: true}
	hub, err := NewHub(t.Context(), HubOptions{Limits: limits, Authorizer: authorizer})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 1000; index++ {
		account := testAccount[:24] + format12(index/10+1)
		device := testDevice[:24] + format12(index+1)
		session := testSession[:24] + format12(index+1)
		family := testFamily[:24] + format12(index/10+1)
		p := principal(device)
		p.AccountID, p.SessionID, p.SessionFamilyID = account, session, family
		g := grant()
		g.AccountID, g.FamilyID = account, family
		reservation, reserveErr := hub.Reserve(t.Context(), p, g)
		if reserveErr != nil {
			t.Fatalf("reserve %d: %v", index, reserveErr)
		}
		if _, bindErr := reservation.Bind(&testConnection{}); bindErr != nil {
			t.Fatalf("bind %d: %v", index, bindErr)
		}
	}
	if err := hub.sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	if authorizer.calls != 1 || authorizer.batchLen != 1000 {
		t.Fatalf("calls=%d batch=%d", authorizer.calls, authorizer.batchLen)
	}
}

func format12(value int) string {
	const digits = "0123456789abcdef"
	result := make([]byte, 12)
	for index := len(result) - 1; index >= 0; index-- {
		result[index] = digits[value&15]
		value >>= 4
	}
	return string(result)
}

// Check the routing index and authoritative registry under the same lock.
func assertFamilyIndex(t *testing.T, hub *Hub, want int) {
	t.Helper()
	hub.mu.Lock()
	defer hub.mu.Unlock()
	count := 0
	for family, members := range hub.byFamily {
		if len(members) == 0 {
			t.Fatal("empty family bucket retained")
		}
		for item := range members {
			if hub.leases[item.id] != item || item.grant.FamilyID != family {
				t.Fatal("index member absent from authoritative registry or wrong family")
			}
			count++
		}
	}
	if count != want || len(hub.leases) != want {
		t.Fatalf("index=%d registry=%d want=%d", count, len(hub.leases), want)
	}
}

func TestFamilyIndexLifecycleAndConcurrentPublish(t *testing.T) {
	for _, mode := range []string{"release", "expiry", "revocation", "shutdown", "sweep"} {
		t.Run(mode, func(t *testing.T) {
			options := HubOptions{Limits: protocol.DefaultLimits()}
			if mode == "sweep" {
				options.Authorizer = &batchAuthorizer{allow: false}
			}
			hub, err := NewHub(t.Context(), options)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "expiry" {
				hub.maximumLife = 30 * time.Millisecond
			}
			reservation, err := hub.Reserve(t.Context(), principal(testDevice), grant())
			if err != nil {
				t.Fatal(err)
			}
			assertFamilyIndex(t, hub, 0)
			if hub.HasRegisteredFamily(testFamily) {
				t.Fatal("unbound reservation is routable")
			}
			connection := &testConnection{}
			release, err := reservation.Bind(connection)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			assertFamilyIndex(t, hub, 1)
			if !hub.HasRegisteredFamily(testFamily) {
				t.Fatal("bound family absent")
			}
			var wait sync.WaitGroup
			wait.Add(1)
			go func() {
				defer wait.Done()
				for i := uint64(1); i <= 100; i++ {
					hub.Publish(t.Context(), Hint{FamilyID: testFamily, Cursor: protocol.Cursor{ServerInstanceID: testServer, RecoveryEpoch: 1, FamilyID: testFamily, Sequence: i}})
				}
			}()
			switch mode {
			case "release":
				release()
				release()
			case "expiry":
				waitCloses(t, connection, CloseNormal)
			case "revocation":
				hub.Invalidate(t.Context(), security.SecurityInvalidation{FamilyID: testFamily, Revision: 6})
				waitCloses(t, connection, ClosePolicyViolation)
			case "shutdown":
				if err := hub.Shutdown(t.Context()); err != nil {
					t.Fatal(err)
				}
			case "sweep":
				if err := hub.sweep(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			wait.Wait()
			assertFamilyIndex(t, hub, 0)
			if hub.HasRegisteredFamily(testFamily) {
				t.Fatal("released family retained")
			}
			hub.mu.Lock()
			counts := len(hub.deviceCount) + len(hub.accountCount)
			hub.mu.Unlock()
			if counts != 0 {
				t.Fatal("capacity counts leaked")
			}
		})
	}
}

func TestFamilyIndexRetainsOtherLeaseAndReleasedReservationIsUnroutable(t *testing.T) {
	hub, err := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	releases := make([]func(), 0, 2)
	for i := 1; i <= 2; i++ {
		reservation, err := hub.Reserve(t.Context(), principal(testDevice[:24]+format12(i)), grant())
		if err != nil {
			t.Fatal(err)
		}
		release, err := reservation.Bind(&testConnection{})
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		releases = append(releases, release)
	}
	assertFamilyIndex(t, hub, 2)
	releases[0]()
	assertFamilyIndex(t, hub, 1)
	if !hub.HasRegisteredFamily(testFamily) {
		t.Fatal("release removed another lease's family")
	}
	releases[1]()
	reservation, err := hub.Reserve(t.Context(), principal(testDevice), grant())
	if err != nil {
		t.Fatal(err)
	}
	reservation.Release()
	assertFamilyIndex(t, hub, 0)
	if hub.HasRegisteredFamily(testFamily) {
		t.Fatal("released reservation registered")
	}
}

func TestIndexedPublishTargetsOnlyFamily(t *testing.T) {
	hub, err := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	slow := &firstWriteGateConnection{started: make(chan struct{}), release: make(chan struct{})}
	reservation, err := hub.Reserve(t.Context(), principal(testDevice), grant())
	if err != nil {
		t.Fatal(err)
	}
	release, err := reservation.Bind(slow)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	p, g := principal(testDevice[:24]+format12(2)), grant()
	p.AccountID = testAccount[:24] + format12(2)
	p.SessionFamilyID = testFamily[:24] + format12(2)
	g.AccountID, g.FamilyID = p.AccountID, p.SessionFamilyID
	fast := &testConnection{}
	reservation, err = hub.Reserve(t.Context(), p, g)
	if err != nil {
		t.Fatal(err)
	}
	releaseFast, err := reservation.Bind(fast)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFast()
	hint := Hint{FamilyID: testFamily, Cursor: protocol.Cursor{ServerInstanceID: testServer, RecoveryEpoch: 1, FamilyID: testFamily, Sequence: 1}}
	hub.Publish(t.Context(), hint)
	select {
	case <-slow.started:
	case <-time.After(time.Second):
		t.Fatal("write did not start")
	}
	var unblock sync.Once
	defer unblock.Do(func() { close(slow.release) })
	for _, seq := range []uint64{3, 2, 3} {
		hint.Cursor.Sequence = seq
		hub.Publish(t.Context(), hint)
	}
	// A blocked write must not retain the hub mutex or impede another family.
	completed := make(chan struct{})
	go func() {
		hub.RegisteredLeases()
		hint.FamilyID = g.FamilyID
		hint.Cursor.FamilyID = g.FamilyID
		hub.Publish(t.Context(), hint)
		close(completed)
	}()
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("blocked writer held hub mutex")
	}
	waitWrites(t, fast, 1)
	fast.mu.Lock()
	for _, got := range fast.writes {
		if got.FamilyID != g.FamilyID {
			t.Error("cross-family write")
		}
	}
	fast.mu.Unlock()
	assertFamilyIndex(t, hub, 2)
	unblock.Do(func() { close(slow.release) })
	deadline := time.Now().Add(time.Second)
	for {
		slow.mu.Lock()
		count := len(slow.writes)
		if count >= 2 {
			if count != 2 || slow.writes[0].Cursor.Sequence != 1 || slow.writes[1].Cursor.Sequence != 3 {
				t.Errorf("coalesced writes=%+v", slow.writes)
			}
			slow.mu.Unlock()
			break
		}
		slow.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("coalesced high-water hint not delivered")
		}
		time.Sleep(time.Millisecond)
	}
}
