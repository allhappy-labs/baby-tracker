package realtime

import (
	"context"
	"errors"
	"go/build"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/protocol"
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

// pingHeldConnection holds its first ping, as a ping waiting for its pong
// does, until release closes or the ping's context ends. A cancelled
// context aborts it the way coder/websocket aborts a write whose context
// ends (setupWriteTimeout closes the transport), so the close frame that
// follows never reaches the client: aborted records that.
type pingHeldConnection struct {
	testConnection
	pinging chan struct{}
	release chan struct{}
	once    sync.Once
}

func newPingHeldConnection() *pingHeldConnection {
	return &pingHeldConnection{pinging: make(chan struct{}), release: make(chan struct{})}
}

func (connection *pingHeldConnection) Ping(ctx context.Context) error {
	first := false
	connection.once.Do(func() { first = true; close(connection.pinging) })
	if !first {
		return nil
	}
	select {
	case <-connection.release:
		return nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.Canceled) {
			connection.mu.Lock()
			connection.aborted = true
			connection.mu.Unlock()
		}
		return ctx.Err()
	}
}

// A ping in flight when another path ends the lease neither answers for it
// nor aborts the socket under it: the invalidation's 1008 is the only close
// and reaches the client. The v5 and v6 runs' lost sockets saw 1013 "slow
// consumer", or no frame at all, when the sweep's retirement met a ping. A
// ping cut off by the socket's lifetime closes 1000, not 1013.
func TestAPingInFlightLeavesTheCloseToWhatEndedTheLease(t *testing.T) {
	t.Run("invalidated", func(t *testing.T) {
		hub, _ := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits()})
		hub.pingInterval = 5 * time.Millisecond
		connection := newPingHeldConnection()
		reservation, _ := hub.Reserve(t.Context(), principal(testDevice), grant())
		cleanup, err := reservation.Bind(connection)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		<-connection.pinging
		hub.Invalidate(t.Context(), authority.SecurityInvalidation{FamilyID: testFamily, Revision: 6})
		waitCloses(t, &connection.testConnection, ClosePolicyViolation)
		close(connection.release)
		time.Sleep(50 * time.Millisecond)
		connection.mu.Lock()
		defer connection.mu.Unlock()
		if len(connection.closes) != 1 || connection.aborted {
			t.Fatalf("closes %v, aborted %v; want only %d, not aborted", connection.closes, connection.aborted, ClosePolicyViolation)
		}
	})
	t.Run("lifetime", func(t *testing.T) {
		now := time.Now().UTC()
		hub, _ := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits(), Now: func() time.Time { return now }})
		hub.pingInterval = 5 * time.Millisecond
		hub.maximumLife = 50 * time.Millisecond
		hub.writeTimeout = 100 * time.Millisecond
		p := principal(testDevice)
		p.AccessExpiresAt = now.Add(time.Hour)
		connection := newPingHeldConnection()
		reservation, _ := hub.Reserve(t.Context(), p, grant())
		cleanup, err := reservation.Bind(connection)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		waitCloses(t, &connection.testConnection, CloseNormal)
		connection.mu.Lock()
		defer connection.mu.Unlock()
		if len(connection.closes) != 1 || connection.aborted {
			t.Fatalf("closes %v, aborted %v; want only %d, not aborted", connection.closes, connection.aborted, CloseNormal)
		}
	})
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
	// block, when set, holds every CheckMany until it is closed or the
	// call's context ends (the recheck's deadline).
	block chan struct{}
	// late, when set, answers after this long whatever the context says:
	// an authority that ignores the deadline.
	late time.Duration
}

func (authorizer *batchAuthorizer) CheckMany(ctx context.Context, leases []AuthorizationLease) ([]bool, error) {
	authorizer.mu.Lock()
	authorizer.calls++
	authorizer.batchLen = len(leases)
	block, late := authorizer.block, authorizer.late
	authorizer.mu.Unlock()
	time.Sleep(late)
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
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
		hub.Invalidate(t.Context(), authority.SecurityInvalidation{FamilyID: testFamily, Revision: 6})
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
	hub.Invalidate(t.Context(), authority.SecurityInvalidation{AccountID: testAccount, Revision: 5})
	hub.Invalidate(t.Context(), authority.SecurityInvalidation{AccountID: testAccount, FamilyID: "b0000000-0000-4000-8000-000000000099", Revision: 6})
	connection.mu.Lock()
	before := len(connection.closes)
	connection.mu.Unlock()
	if before != 0 {
		t.Fatal("non-newer or partially matching invalidation closed connection")
	}
	hub.Invalidate(t.Context(), authority.SecurityInvalidation{AccountID: testAccount, FamilyID: testFamily, SessionID: testSession, Revision: 6})
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
				hub.Invalidate(t.Context(), authority.SecurityInvalidation{FamilyID: testFamily, Revision: 6})
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

// boundLease reserves and binds one lease for principal p and grant g.
func boundLease(t *testing.T, hub *Hub, p access.Principal, g access.Grant) *testConnection {
	t.Helper()
	reservation, err := hub.Reserve(t.Context(), p, g)
	if err != nil {
		t.Fatal(err)
	}
	connection := &testConnection{}
	cleanup, err := reservation.Bind(connection)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	return connection
}

// leaseCount is the number of bound leases (RegisteredLeases is one per
// family).
func leaseCount(hub *Hub) int {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return len(hub.leases)
}

func closeCount(connection *testConnection) int {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return len(connection.closes)
}

// An invalidation's revision is the audit seq of its own scope: the family
// seq when it names a family, the account seq otherwise. A grant's revision
// is a family seq and a session's issued seq (Principal.SecurityRevision) is
// an account seq, so each class is compared with its own counterpart. A
// family seq far ahead of the account seq must not shield a lease from an
// account revocation, and an account seq must not close a lease admitted
// after it.
func TestInvalidationRevisionIsComparedWithinItsScope(t *testing.T) {
	const (
		accountSeq = 3  // the session was issued at account seq 3
		familySeq  = 10 // the grant was admitted at family seq 10
	)
	lease := func(t *testing.T, hub *Hub) *testConnection {
		p := principal(testDevice)
		p.SecurityRevision = accountSeq
		g := grant()
		g.Revision = familySeq
		return boundLease(t, hub, p, g)
	}
	for _, test := range []struct {
		name   string
		item   authority.SecurityInvalidation
		closes bool
	}{
		// Logout and refresh reuse: account seq, one session.
		{"logout_after_issue", authority.SecurityInvalidation{AccountID: testAccount, SessionID: testSession, Revision: accountSeq + 1}, true},
		{"logout_at_issue", authority.SecurityInvalidation{AccountID: testAccount, SessionID: testSession, Revision: accountSeq}, false},
		{"logout_other_session", authority.SecurityInvalidation{AccountID: testAccount, SessionID: testSession[:24] + "000000000099", Revision: accountSeq + 1}, false},
		// MarkIdentityUnavailable and DeleteAccount: account seq, the whole
		// account. Below the grant's family seq, yet newer than the session.
		{"account_after_issue", authority.SecurityInvalidation{AccountID: testAccount, Revision: accountSeq + 1}, true},
		{"account_before_issue", authority.SecurityInvalidation{AccountID: testAccount, Revision: accountSeq - 1}, false},
		{"other_account", authority.SecurityInvalidation{AccountID: testAccount[:24] + "000000000099", Revision: accountSeq + 1}, false},
		// Member commands: family seq for one account in the family.
		{"member_after_grant", authority.SecurityInvalidation{AccountID: testAccount, FamilyID: testFamily, Revision: familySeq + 1}, true},
		{"member_at_grant", authority.SecurityInvalidation{AccountID: testAccount, FamilyID: testFamily, Revision: familySeq}, false},
		// Purge and entitlement suspension: family seq, the whole family.
		{"family_after_grant", authority.SecurityInvalidation{FamilyID: testFamily, Revision: familySeq + 1}, true},
		{"family_before_grant", authority.SecurityInvalidation{FamilyID: testFamily, Revision: accountSeq + 1}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			hub, err := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits()})
			if err != nil {
				t.Fatal(err)
			}
			connection := lease(t, hub)
			hub.Invalidate(t.Context(), test.item)
			if test.closes {
				waitCloses(t, connection, ClosePolicyViolation)
				if n := leaseCount(hub); n != 0 {
					t.Fatalf("lease survived: %d", n)
				}
				return
			}
			time.Sleep(20 * time.Millisecond)
			if closeCount(connection) != 0 || leaseCount(hub) != 1 {
				t.Fatalf("lease closed by %+v", test.item)
			}
		})
	}
}

// Dropped invalidations name nobody the hub can see, so it re-checks every
// lease at once: a denied lease closes without waiting for the periodic
// sweep, an allowed one stays, and a hub that cannot re-check closes all.
func TestDroppedInvalidationsRecheckEveryLease(t *testing.T) {
	for _, test := range []struct {
		name       string
		authorizer *batchAuthorizer
		closes     bool
	}{
		{"denied", &batchAuthorizer{allow: false}, true},
		{"allowed", &batchAuthorizer{allow: true}, false},
		{"authorizer_failed", &batchAuthorizer{err: errors.New("database unavailable")}, true},
		{"no_authorizer", nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := HubOptions{Limits: protocol.DefaultLimits()}
			if test.authorizer != nil {
				options.Authorizer = test.authorizer
			}
			hub, err := NewHub(t.Context(), options)
			if err != nil {
				t.Fatal(err)
			}
			first := boundLease(t, hub, principal(testDevice), grant())
			other := principal(testDevice[:24] + "000000000002")
			other.SessionID = testSession[:24] + "000000000002"
			second := boundLease(t, hub, other, grant())
			hub.InvalidationsDropped(t.Context())
			if test.authorizer != nil {
				// The recheck runs on the sweep goroutine, not the caller's.
				calls, batch := waitAuthorizerCall(t, test.authorizer)
				if calls != 1 || batch != 2 {
					t.Fatalf("authorizer calls=%d batch=%d, want one batch of both leases", calls, batch)
				}
			}
			if test.closes {
				waitCloses(t, first, ClosePolicyViolation)
				waitCloses(t, second, ClosePolicyViolation)
				return
			}
			time.Sleep(20 * time.Millisecond)
			if closeCount(first)+closeCount(second) != 0 || leaseCount(hub) != 2 {
				t.Fatal("allowed leases closed")
			}
		})
	}
}

// waitAuthorizerCall waits for the first CheckMany and returns the call count
// and the last batch's size.
func waitAuthorizerCall(t *testing.T, authorizer *batchAuthorizer) (int, int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		authorizer.mu.Lock()
		calls, batch := authorizer.calls, authorizer.batchLen
		authorizer.mu.Unlock()
		if calls > 0 || time.Now().After(deadline) {
			// Let a wrongly repeated call show up before the count is read.
			time.Sleep(20 * time.Millisecond)
			authorizer.mu.Lock()
			defer authorizer.mu.Unlock()
			return authorizer.calls, authorizer.batchLen
		}
		_ = batch
		time.Sleep(time.Millisecond)
	}
}

func writes(connection *testConnection) int {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return len(connection.writes)
}

func (connection *testConnection) snapshotWrites() []Hint {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return append([]Hint(nil), connection.writes...)
}

func waitWrites(t *testing.T, connection *testConnection, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if writes(connection) >= count {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("writes=%+v want at least %d", connection.snapshotWrites(), count)
}

func hintAt(family string, seq uint64) Hint {
	return Hint{FamilyID: family, Cursor: protocol.Cursor{ServerInstanceID: testServer, RecoveryEpoch: 1, FamilyID: family, Sequence: seq}}
}

// familyLease binds one lease in its own family (account, device, session
// and family all numbered by index).
func familyLease(t *testing.T, hub *Hub, index int) (*testConnection, string) {
	t.Helper()
	p, g := principal(testDevice[:24]+format12(index)), grant()
	p.AccountID = testAccount[:24] + format12(index)
	p.SessionID = testSession[:24] + format12(index)
	p.SessionFamilyID = testFamily[:24] + format12(index)
	g.AccountID, g.FamilyID = p.AccountID, p.SessionFamilyID
	return boundLease(t, hub, p, g), g.FamilyID
}

func TestPublishCoalescesPerFamily(t *testing.T) {
	hub, err := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits(), CoalesceWindow: time.Hour}) // only FlushNow delivers
	if err != nil {
		t.Fatal(err)
	}
	connection := boundLease(t, hub, principal(testDevice), grant())
	for seq := uint64(1); seq <= 100; seq++ {
		hub.Publish(t.Context(), Hint{FamilyID: testFamily, Cursor: protocol.Cursor{ServerInstanceID: testServer, RecoveryEpoch: 1, FamilyID: testFamily, Sequence: seq}})
	}
	time.Sleep(20 * time.Millisecond)
	if n := writes(connection); n != 0 {
		t.Fatalf("%d hints written inside the window", n)
	}
	hub.FlushNow()
	deadline := time.Now().Add(time.Second)
	for writes(connection) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if got := connection.snapshotWrites(); len(got) != 1 || got[0].Cursor.Sequence != 100 {
		t.Fatalf("hints %+v, want one carrying sequence 100", got)
	}
}

func TestPublishDeliversWithinTheWindow(t *testing.T) {
	hub, err := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits()}) // 50ms
	if err != nil {
		t.Fatal(err)
	}
	connection := boundLease(t, hub, principal(testDevice), grant())
	start := time.Now()
	for seq := uint64(1); seq <= 10; seq++ {
		hub.Publish(t.Context(), Hint{FamilyID: testFamily, Cursor: protocol.Cursor{ServerInstanceID: testServer, RecoveryEpoch: 1, FamilyID: testFamily, Sequence: seq}})
	}
	for writes(connection) == 0 && time.Since(start) < time.Second {
		time.Sleep(time.Millisecond)
	}
	elapsed := time.Since(start)
	time.Sleep(20 * time.Millisecond)
	if got := connection.snapshotWrites(); elapsed > 250*time.Millisecond || len(got) != 1 || got[0].Cursor.Sequence != 10 {
		t.Fatalf("delivered after %v: %+v", elapsed, got)
	}
	// The next window delivers the next change: coalescing never holds a
	// hint back for longer than one window.
	hub.Publish(t.Context(), hintAt(testFamily, 11))
	waitWrites(t, connection, 2)
	if got := connection.snapshotWrites(); got[1].Cursor.Sequence != 11 {
		t.Fatalf("second window %+v", got)
	}
}

// The lost-hint property: however publishes interleave with flushes, every
// family's connections end on that family's highest published cursor, and
// never receive one lower than a cursor they already had.
func TestCoalescingNeverLosesAFamilysLastHint(t *testing.T) {
	limits := protocol.DefaultLimits()
	hub, err := NewHub(t.Context(), HubOptions{Limits: limits, CoalesceWindow: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	const families, publishers, perPublisher = 8, 4, 300
	connections := make([]*testConnection, families)
	ids := make([]string, families)
	for index := range families {
		connections[index], ids[index] = familyLease(t, hub, index+1)
	}
	var wait sync.WaitGroup
	for publisher := range publishers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for step := range perPublisher {
				family := ids[(publisher+step)%families]
				// Interleaved, out of order across publishers.
				hub.Publish(t.Context(), hintAt(family, uint64(step*publishers+publisher+1)))
				if step%50 == 0 {
					hub.FlushNow()
				}
			}
		}()
	}
	wait.Wait()
	highest := make(map[string]uint64)
	for publisher := range publishers {
		for step := range perPublisher {
			family := ids[(publisher+step)%families]
			highest[family] = max(highest[family], uint64(step*publishers+publisher+1))
		}
	}
	for index, connection := range connections {
		want := highest[ids[index]]
		deadline := time.Now().Add(2 * time.Second)
		for {
			got := connection.snapshotWrites()
			if len(got) > 0 && got[len(got)-1].Cursor.Sequence == want {
				for i := 1; i < len(got); i++ {
					if got[i].Cursor.Sequence <= got[i-1].Cursor.Sequence {
						t.Fatalf("family %d hints went backwards: %+v", index, got)
					}
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("family %d ended on %+v, want sequence %d", index, got, want)
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// A connection whose writes stall holds one pending hint, however many are
// published to it, and still ends on the newest.
func TestAStalledConnectionHoldsOneHintAndEndsOnTheNewest(t *testing.T) {
	hub, err := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits(), CoalesceWindow: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	hub.writeTimeout = 5 * time.Second
	connection := &firstWriteGateConnection{started: make(chan struct{}), release: make(chan struct{})}
	reservation, err := hub.Reserve(t.Context(), principal(testDevice), grant())
	if err != nil {
		t.Fatal(err)
	}
	cleanup, err := reservation.Bind(connection)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	hub.Publish(t.Context(), hintAt(testFamily, 1))
	hub.FlushNow()
	<-connection.started
	for seq := uint64(2); seq <= 1000; seq++ {
		hub.Publish(t.Context(), hintAt(testFamily, seq))
		hub.FlushNow()
	}
	hub.mu.Lock()
	var item *lease
	for _, candidate := range hub.leases {
		item = candidate
	}
	hub.mu.Unlock()
	item.mu.Lock()
	pending := item.pending
	item.mu.Unlock()
	if pending == nil || pending.Cursor.Sequence != 1000 {
		t.Fatalf("pending %+v, want the one newest hint", pending)
	}
	close(connection.release)
	deadline := time.Now().Add(time.Second)
	for {
		connection.mu.Lock()
		got := append([]Hint(nil), connection.writes...)
		connection.mu.Unlock()
		if len(got) == 2 {
			if got[0].Cursor.Sequence != 1 || got[1].Cursor.Sequence != 1000 {
				t.Fatalf("writes %+v", got)
			}
			return
		}
		if len(got) > 2 || time.Now().After(deadline) {
			t.Fatalf("writes %+v, want the first and the newest", got)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDroppedInvalidationsRecheckOffTheDeliveryGoroutineWithADeadline(t *testing.T) {
	blocked := &batchAuthorizer{block: make(chan struct{})} // CheckMany waits for block or ctx
	hub, err := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits(), Authorizer: blocked, SweepTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	first := boundLease(t, hub, principal(testDevice), grant())
	start := time.Now()
	hub.InvalidationsDropped(t.Context())
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("InvalidationsDropped blocked the delivery goroutine for %v", elapsed)
	}
	waitAuthorizerCall(t, blocked)
	if closeCount(first) != 0 {
		t.Fatal("closed before the recheck's deadline")
	}
	waitCloses(t, first, ClosePolicyViolation) // the recheck timed out: fail closed
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("closed after %v, before the 100ms deadline", elapsed)
	}
	if n := leaseCount(hub); n != 0 {
		t.Fatalf("%d leases survived a recheck that never answered", n)
	}
}

// While a recheck waits on the authority, hints still reach every
// connection: the recheck never holds the hub, the publisher or the
// invalidation queue.
func TestARecheckInFlightNeverBlocksHintDelivery(t *testing.T) {
	blocked := &batchAuthorizer{allow: true, block: make(chan struct{})}
	hub, err := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits(), Authorizer: blocked, SweepTimeout: time.Hour, CoalesceWindow: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	first, family := familyLease(t, hub, 1)
	second, other := familyLease(t, hub, 2)
	hub.InvalidationsDropped(t.Context())
	waitAuthorizerCall(t, blocked)
	invalidated := make(chan struct{})
	go func() {
		// The next invalidation is not held behind the recheck either.
		hub.Invalidate(t.Context(), authority.SecurityInvalidation{AccountID: testAccount[:24] + format12(99), Revision: 99})
		close(invalidated)
	}()
	select {
	case <-invalidated:
	case <-time.After(time.Second):
		t.Fatal("an invalidation waited for the recheck")
	}
	hub.Publish(t.Context(), hintAt(family, 7))
	hub.Publish(t.Context(), hintAt(other, 8))
	waitWrites(t, first, 1)
	waitWrites(t, second, 1)
	close(blocked.block)
	time.Sleep(20 * time.Millisecond)
	if closeCount(first)+closeCount(second) != 0 {
		t.Fatal("allowed leases closed")
	}
}

func TestInvalidationReachesAReservationBeforeBind(t *testing.T) {
	other := testAccount[:24] + format12(99)
	for _, test := range []struct {
		name    string
		item    authority.SecurityInvalidation
		revoked bool
	}{
		{"logout", authority.SecurityInvalidation{SessionID: testSession, AccountID: testAccount, Revision: 99}, true},
		{"member_removed", authority.SecurityInvalidation{AccountID: testAccount, FamilyID: testFamily, Revision: 99}, true},
		{"family_purged", authority.SecurityInvalidation{FamilyID: testFamily, Revision: 99}, true},
		{"account_deleted", authority.SecurityInvalidation{AccountID: testAccount, Revision: 99}, true},
		{"other_account", authority.SecurityInvalidation{AccountID: other, Revision: 99}, false},
		{"other_session", authority.SecurityInvalidation{SessionID: testSession[:24] + format12(99), AccountID: testAccount, Revision: 99}, false},
		{"older_than_the_grant", authority.SecurityInvalidation{AccountID: testAccount, FamilyID: testFamily, Revision: 5}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			limits := protocol.DefaultLimits()
			limits.SocketDeviceLimit = 1
			hub, err := NewHub(t.Context(), HubOptions{Limits: limits})
			if err != nil {
				t.Fatal(err)
			}
			reservation, err := hub.Reserve(t.Context(), principal(testDevice), grant())
			if err != nil {
				t.Fatal(err)
			}
			hub.Invalidate(t.Context(), test.item)
			release, err := reservation.Bind(&testConnection{})
			if !test.revoked {
				if err != nil {
					t.Fatalf("an unrelated invalidation released the reservation: %v", err)
				}
				release()
				return
			}
			if !errors.Is(err, ErrReservationReleased) || !errors.Is(err, ErrAuthorizationChanged) {
				t.Fatalf("a revoked session's reservation bound: %v", err)
			}
			// Its device slot was released with it.
			again, err := hub.Reserve(t.Context(), principal(testDevice), grant())
			if err != nil {
				t.Fatalf("revoked reservation kept its capacity: %v", err)
			}
			again.Release()
			reservation.Release()
			hub.mu.Lock()
			counts := len(hub.deviceCount) + len(hub.accountCount) + len(hub.reservations)
			hub.mu.Unlock()
			if counts != 0 {
				t.Fatal("capacity counts leaked")
			}
		})
	}
}

// The other half of the Authorize→Bind window: an invalidation the hub
// processed after the handler took its mark, and before it reserved, names a
// grant the authorization read could not yet see revoked. ReserveAfter
// refuses it.
func TestReserveAfterRefusesAnInvalidationSinceTheMark(t *testing.T) {
	hub, err := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	mark := hub.InvalidationMark()
	hub.Invalidate(t.Context(), authority.SecurityInvalidation{AccountID: testAccount[:24] + format12(99), Revision: 99})
	reservation, err := hub.ReserveAfter(t.Context(), mark, principal(testDevice), grant())
	if err != nil {
		t.Fatalf("an unrelated invalidation refused the reservation: %v", err)
	}
	reservation.Release()
	hub.Invalidate(t.Context(), authority.SecurityInvalidation{AccountID: testAccount, FamilyID: testFamily, Revision: 99})
	if _, err = hub.ReserveAfter(t.Context(), mark, principal(testDevice), grant()); !errors.Is(err, ErrAuthorizationChanged) {
		t.Fatalf("member removed since the mark: %v", err)
	}
	// An authorization read after the invalidation (a later mark) is
	// admitted, and so is a grant issued after it.
	later := hub.InvalidationMark()
	if reservation, err = hub.ReserveAfter(t.Context(), later, principal(testDevice), grant()); err != nil {
		t.Fatalf("a mark after the invalidation refused: %v", err)
	}
	reservation.Release()
	newer := grant()
	newer.Revision = 99
	if reservation, err = hub.ReserveAfter(t.Context(), mark, principal(testDevice), newer); err != nil {
		t.Fatalf("a grant issued at the invalidation's revision refused: %v", err)
	}
	reservation.Release()
	// More invalidations than the hub remembers since the mark: fail closed.
	for index := range invalidationLogSize + 1 {
		hub.Invalidate(t.Context(), authority.SecurityInvalidation{AccountID: testAccount[:24] + format12(1000+index), Revision: 99})
	}
	if _, err = hub.ReserveAfter(t.Context(), later, principal(testDevice), grant()); !errors.Is(err, ErrAuthorizationChanged) {
		t.Fatalf("a mark older than the remembered invalidations: %v", err)
	}
	if reservation, err = hub.ReserveAfter(t.Context(), hub.InvalidationMark(), principal(testDevice), grant()); err != nil {
		t.Fatal(err)
	}
	reservation.Release()
}

// Dropped invalidations name nobody, so every reservation in flight is
// released, and every mark taken before the drop is refused.
func TestDroppedInvalidationsReleaseReservations(t *testing.T) {
	hub, err := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits(), Authorizer: &batchAuthorizer{allow: true}})
	if err != nil {
		t.Fatal(err)
	}
	mark := hub.InvalidationMark()
	reservation, err := hub.Reserve(t.Context(), principal(testDevice), grant())
	if err != nil {
		t.Fatal(err)
	}
	hub.InvalidationsDropped(t.Context())
	if _, err = reservation.Bind(&testConnection{}); !errors.Is(err, ErrAuthorizationChanged) {
		t.Fatalf("a reservation in flight across dropped invalidations bound: %v", err)
	}
	if _, err = hub.ReserveAfter(t.Context(), mark, principal(testDevice), grant()); !errors.Is(err, ErrAuthorizationChanged) {
		t.Fatalf("a mark from before the drop: %v", err)
	}
}

// Bounded goroutines: every connection's goroutines end when it closes, by
// any path, and the hub's own end at shutdown.
func TestGoroutinesReturnToBaselineAfterConnectionsClose(t *testing.T) {
	baseline := settledGoroutines()
	limits := protocol.DefaultLimits()
	limits.SocketProcessLimit = 1000
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	authorizer := &batchAuthorizer{allow: false}
	hub, err := NewHub(ctx, HubOptions{Limits: limits, Authorizer: authorizer, CoalesceWindow: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	hub.writeTimeout = 20 * time.Millisecond
	withHub := runtime.NumGoroutine()
	const count = 60
	releases := make([]func(), 0, count)
	for index := range count {
		p, g := principal(testDevice[:24]+format12(index+1)), grant()
		p.AccountID = testAccount[:24] + format12(index+1)
		p.SessionID = testSession[:24] + format12(index+1)
		p.SessionFamilyID = testFamily[:24] + format12(index%6+1)
		g.AccountID, g.FamilyID = p.AccountID, p.SessionFamilyID
		reservation, err := hub.Reserve(t.Context(), p, g)
		if err != nil {
			t.Fatal(err)
		}
		connection := &testConnection{}
		if index%5 == 4 {
			connection.block = make(chan struct{}) // a slow consumer: its write times out
		}
		release, err := reservation.Bind(connection)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	for seq := uint64(1); seq <= 20; seq++ {
		for family := range 6 {
			hub.Publish(t.Context(), hintAt(testFamily[:24]+format12(family+1), seq))
		}
	}
	time.Sleep(50 * time.Millisecond)
	// Close the rest by every path: release, an invalidation, a denied sweep.
	for index, release := range releases {
		if index%3 == 0 {
			release()
		}
	}
	hub.Invalidate(t.Context(), authority.SecurityInvalidation{FamilyID: testFamily[:24] + format12(2), Revision: 99})
	if err := hub.sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := leaseCount(hub); n != 0 {
		t.Fatalf("%d leases left", n)
	}
	waitGoroutines(t, withHub)
	if err := hub.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitGoroutines(t, baseline)
}

// settledGoroutines waits for goroutines left by earlier tests (hubs
// shutting down behind their test's context) to finish, so they cannot hide
// a leak.
func settledGoroutines() int {
	previous := runtime.NumGoroutine()
	for range 100 {
		time.Sleep(10 * time.Millisecond)
		current := runtime.NumGoroutine()
		if current == previous {
			return current
		}
		previous = current
	}
	return previous
}

func waitGoroutines(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > want {
		if time.Now().After(deadline) {
			buffer := make([]byte, 1<<20)
			t.Fatalf("goroutines %d, want at most %d:\n%s", runtime.NumGoroutine(), want, buffer[:runtime.Stack(buffer, true)])
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// LISTEN/NOTIFY is removed: the hub's package reaches no database.
func TestRealtimeImportsNoDatabaseDriver(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imported := range pkg.Imports {
		if strings.Contains(imported, "jackc/pgx") {
			t.Fatalf("realtime imports %s", imported)
		}
	}
}

// An answer that arrives after the deadline is not trusted: the leases it
// covers close as if it had never come.
func TestARecheckAnsweredAfterItsDeadlineFailsClosed(t *testing.T) {
	late := &batchAuthorizer{allow: true, late: 150 * time.Millisecond}
	hub, err := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits(), Authorizer: late, SweepTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	connection := boundLease(t, hub, principal(testDevice), grant())
	if err := hub.sweep(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("late answer accepted: %v", err)
	}
	waitCloses(t, connection, ClosePolicyViolation)
}

// The hub keeps no hint for a family it holds no connection for, so the
// pending set is bounded by the connections, not by the families writing.
func TestPublishKeepsNothingForFamiliesWithoutConnections(t *testing.T) {
	hub, err := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits(), CoalesceWindow: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	connection, family := familyLease(t, hub, 1)
	for index := range 1000 {
		hub.Publish(t.Context(), hintAt(testFamily[:24]+format12(index+2), 1))
	}
	hub.Publish(t.Context(), hintAt(family, 1))
	hub.pendingMu.Lock()
	pending := len(hub.pending)
	hub.pendingMu.Unlock()
	if pending != 1 {
		t.Fatalf("%d families pending, want only the connected one", pending)
	}
	hub.FlushNow()
	waitWrites(t, connection, 1)
}

// firstCheckBlocks holds its first CheckMany until release, then allows it;
// every later check denies.
type firstCheckBlocks struct {
	mu      sync.Mutex
	calls   int
	release chan struct{}
}

func (authorizer *firstCheckBlocks) CheckMany(ctx context.Context, leases []AuthorizationLease) ([]bool, error) {
	authorizer.mu.Lock()
	authorizer.calls++
	first := authorizer.calls == 1
	authorizer.mu.Unlock()
	allowed := make([]bool, len(leases))
	if !first {
		return allowed, nil
	}
	select {
	case <-authorizer.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	for index := range allowed {
		allowed[index] = true
	}
	return allowed, nil
}

func (authorizer *firstCheckBlocks) callCount() int {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	return authorizer.calls
}

// Invalidations dropped while a recheck is running schedule one more: the
// running check's snapshot cannot cover a lease bound after it started, so a
// drop then must not be lost (not even until the periodic sweep).
func TestDroppedInvalidationsDuringARecheckScheduleAnother(t *testing.T) {
	authorizer := &firstCheckBlocks{release: make(chan struct{})}
	hub, err := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits(), Authorizer: authorizer, SweepTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := familyLease(t, hub, 1)
	hub.InvalidationsDropped(t.Context())
	deadline := time.Now().Add(time.Second)
	for authorizer.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if authorizer.callCount() != 1 {
		t.Fatal("the first recheck never started")
	}
	second, _ := familyLease(t, hub, 2)
	hub.InvalidationsDropped(t.Context())
	close(authorizer.release)
	// Only the follow-up check (which denies) can see the second lease.
	waitCloses(t, second, ClosePolicyViolation)
	waitCloses(t, first, ClosePolicyViolation)
	if n := authorizer.callCount(); n != 2 {
		t.Fatalf("%d checks, want the running one and one follow-up", n)
	}
}

// Reannounce is the wake worker's path (Task 7 carry): it reaches the named
// devices' connections even when they were already sent the cursor, which
// Publish never repeats. The iOS client acts on a repeated cursor whenever
// its own cursor is behind it (FamilySyncEngine.liveHintArrived), so a
// connected device that saw N and failed to pull is woken. Other devices of
// the family, and other families, get nothing; a connection that was sent a
// higher cursor is re-sent that one, never a lower one.
func TestReannounceReachesTheNamedDevicesEvenAfterTheySawTheCursor(t *testing.T) {
	limits := protocol.DefaultLimits()
	limits.SocketAccountLimit = 10
	hub, err := NewHub(t.Context(), HubOptions{Limits: limits, CoalesceWindow: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	const (
		lagging = "40000000-0000-4000-8000-0000000000a1"
		caught  = "40000000-0000-4000-8000-0000000000a2"
		ahead   = "40000000-0000-4000-8000-0000000000a3"
		other   = "b0000000-0000-4000-8000-0000000000ff"
	)
	stale := boundLease(t, hub, principal(lagging), grant())
	upToDate := boundLease(t, hub, principal(caught), grant())
	higher := boundLease(t, hub, principal(ahead), grant())
	otherPrincipal := principal("40000000-0000-4000-8000-0000000000a4")
	otherPrincipal.SessionFamilyID = other
	otherGrant := grant()
	otherGrant.FamilyID = other
	elsewhere := boundLease(t, hub, otherPrincipal, otherGrant)
	hub.Publish(t.Context(), hintAt(testFamily, 7))
	for _, connection := range []*testConnection{stale, upToDate, higher} {
		waitWrites(t, connection, 1)
	}
	// Publish never repeats a cursor a connection was sent.
	hub.Publish(t.Context(), hintAt(testFamily, 7))
	hub.FlushNow()
	time.Sleep(20 * time.Millisecond)
	if writes(stale) != 1 {
		t.Fatalf("Publish repeated the cursor: %+v", stale.snapshotWrites())
	}
	if reached := hub.Reannounce(t.Context(), hintAt(testFamily, 7), []string{lagging, "40000000-0000-4000-8000-0000000000ff"}); reached != 1 {
		t.Fatalf("reannounce reached %d connections, want the lagging device's one", reached)
	}
	waitWrites(t, stale, 2)
	if got := stale.snapshotWrites(); got[1].Cursor.Sequence != 7 {
		t.Fatalf("reannounced %+v", got)
	}
	// The connection that was sent 9 is re-sent 9 when asked for 8.
	hub.Publish(t.Context(), hintAt(testFamily, 9))
	waitWrites(t, higher, 2)
	waitWrites(t, stale, 3)
	if reached := hub.Reannounce(t.Context(), hintAt(testFamily, 8), []string{ahead}); reached != 1 {
		t.Fatalf("reannounce reached %d, want 1", reached)
	}
	waitWrites(t, higher, 3)
	if got := higher.snapshotWrites(); got[2].Cursor.Sequence != 9 {
		t.Fatalf("a connection that saw 9 was re-sent %+v", got)
	}
	time.Sleep(20 * time.Millisecond)
	if writes(upToDate) != 2 || writes(stale) != 3 || writes(elsewhere) != 0 {
		t.Fatalf("unnamed connections were written: caught %+v, lagging %+v, other family %+v",
			upToDate.snapshotWrites(), stale.snapshotWrites(), elsewhere.snapshotWrites())
	}
	// An invalid hint, or no device, reaches nothing.
	if hub.Reannounce(t.Context(), Hint{FamilyID: testFamily}, []string{lagging}) != 0 || hub.Reannounce(t.Context(), hintAt(testFamily, 9), nil) != 0 {
		t.Fatal("an invalid reannounce reached a connection")
	}
	var nilHub *Hub
	if nilHub.Reannounce(t.Context(), hintAt(testFamily, 9), []string{lagging}) != 0 {
		t.Fatal("a nil hub reached a connection")
	}
}

// DeviceConnected is the wake worker's split between a hub re-announce and
// an APNs push: a device holding a reserved or bound connection is reached
// through the hub. It is false before any reservation, true while one is
// reserved or bound, and false again once the connection closes or a
// revocation closes it.
func TestDeviceConnected(t *testing.T) {
	limits := protocol.DefaultLimits()
	limits.SocketAccountLimit = 10
	hub, err := NewHub(t.Context(), HubOptions{Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	const other = "40000000-0000-4000-8000-0000000000c2"
	if hub.DeviceConnected(testDevice) || hub.DeviceConnected(other) {
		t.Fatal("a device is connected before any reservation")
	}
	reservation, err := hub.Reserve(t.Context(), principal(testDevice), grant())
	if err != nil {
		t.Fatal(err)
	}
	if !hub.DeviceConnected(testDevice) || hub.DeviceConnected(other) {
		t.Fatal("a reserved connection does not count as connected (or another device does)")
	}
	release, err := reservation.Bind(&testConnection{})
	if err != nil {
		t.Fatal(err)
	}
	if !hub.DeviceConnected(testDevice) {
		t.Fatal("a bound connection does not count as connected")
	}
	release() // the connection closed
	if hub.DeviceConnected(testDevice) {
		t.Fatal("a closed connection still counts as connected")
	}
	// A revocation closes the device's connection.
	revoked := boundLease(t, hub, principal(testDevice), grant())
	if !hub.DeviceConnected(testDevice) {
		t.Fatal("a second connection does not count")
	}
	hub.Invalidate(t.Context(), authority.SecurityInvalidation{SessionID: testSession, AccountID: testAccount, Revision: 99})
	waitCloses(t, revoked, ClosePolicyViolation)
	if hub.DeviceConnected(testDevice) {
		t.Fatal("a revoked connection still counts as connected")
	}
	// A reservation released before it bound no longer counts either.
	pending, err := hub.Reserve(t.Context(), principal(other), grant())
	if err != nil {
		t.Fatal(err)
	}
	pending.Release()
	if hub.DeviceConnected(other) {
		t.Fatal("a released reservation still counts as connected")
	}
	var nilHub *Hub
	if nilHub.DeviceConnected(testDevice) {
		t.Fatal("a nil hub reports a connection")
	}
}

// Stats counts what the hub holds now: a bound reservation becomes a lease,
// and a released one counts as neither. It carries no identifiers.
func TestHubStatsCountsLeasesAndReservations(t *testing.T) {
	hub, err := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	if got := hub.Stats(); got != (HubStats{}) {
		t.Fatalf("empty hub stats %+v", got)
	}
	first, err := hub.Reserve(t.Context(), principal(testDevice), grant())
	if err != nil {
		t.Fatal(err)
	}
	second, err := hub.Reserve(t.Context(), principal("40000000-0000-4000-8000-000000000002"), grant())
	if err != nil {
		t.Fatal(err)
	}
	if got := hub.Stats(); got != (HubStats{Reservations: 2}) {
		t.Fatalf("two reserved: %+v", got)
	}
	cleanup, err := first.Bind(&testConnection{})
	if err != nil {
		t.Fatal(err)
	}
	if got := hub.Stats(); got != (HubStats{Leases: 1, Reservations: 1}) {
		t.Fatalf("one bound: %+v, want {Leases:1 Reservations:1}", got)
	}
	second.Release()
	cleanup()
	if got := hub.Stats(); got != (HubStats{}) {
		t.Fatalf("all released: %+v", got)
	}
	var none *Hub
	if got := none.Stats(); got != (HubStats{}) {
		t.Fatalf("nil hub stats %+v", got)
	}
}

// Each sweep's CheckMany is timed into the hub's stats; a sweep with no
// leases asks nothing and records nothing, and a sweep that answers (even
// with a denial) closes nothing as unavailable.
func TestSweepDurationIsObserved(t *testing.T) {
	authorizer := &batchAuthorizer{allow: true, late: 20 * time.Millisecond}
	hub, err := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits(), Authorizer: authorizer})
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := hub.Stats().SweepDuration; got.Count != 0 {
		t.Fatalf("an empty sweep was observed: %+v", got)
	}
	familyLease(t, hub, 1)
	familyLease(t, hub, 2)
	if err := hub.sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	authorizer.mu.Lock()
	authorizer.allow, authorizer.late = false, 0
	authorizer.mu.Unlock()
	if err := hub.sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	stats := hub.Stats()
	var bucketed, fast uint64
	for index, n := range stats.SweepDuration.Buckets {
		bucketed += n
		if index <= 4 { // up to 10 ms (bounds as boundaries_ns)
			fast += n
		}
	}
	if stats.SweepDuration.Count != 2 || bucketed != 2 || stats.SweepDuration.TotalNS < uint64(20*time.Millisecond) {
		t.Fatalf("sweep durations %+v", stats.SweepDuration)
	}
	if fast == 2 {
		t.Fatalf("the 20 ms sweep was not bucketed by its duration: %+v", stats.SweepDuration)
	}
	if stats.SweepClosed != 0 || stats.Leases != 0 {
		t.Fatalf("denied sweep: closed %d (want 0: a denial is not unavailability), leases %d", stats.SweepClosed, stats.Leases)
	}
}

// A sweep that cannot answer (an error, a late answer, a wrong count) closes
// every lease it covered as "authorization unavailable" and counts each one
// once; the re-check after dropped invalidations without an authorizer
// counts its closes too.
func TestUnavailableSweepCountsItsCloses(t *testing.T) {
	for name, authorizer := range map[string]LiveAuthorizer{
		"error":       &batchAuthorizer{err: errors.New("authorization_quarantine")},
		"late":        &batchAuthorizer{allow: true, late: 100 * time.Millisecond},
		"wrong count": wrongCountAuthorizer{},
	} {
		t.Run(name, func(t *testing.T) {
			hub, err := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits(), Authorizer: authorizer, SweepTimeout: 30 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			connections := []*testConnection{}
			for index := 1; index <= 3; index++ {
				connection, _ := familyLease(t, hub, index)
				connections = append(connections, connection)
			}
			if err := hub.sweep(t.Context()); err == nil {
				t.Fatal("an unavailable sweep succeeded")
			}
			for _, connection := range connections {
				waitCloses(t, connection, ClosePolicyViolation)
			}
			if stats := hub.Stats(); stats.SweepClosed != 3 || stats.SweepDuration.Count != 1 || stats.Leases != 0 {
				t.Fatalf("stats %+v", stats)
			}
			familyLease(t, hub, 4)
			if err := hub.sweep(t.Context()); err == nil {
				t.Fatal("an unavailable sweep succeeded")
			}
			if stats := hub.Stats(); stats.SweepClosed != 4 || stats.SweepDuration.Count != 2 {
				t.Fatalf("stats after a one-lease failure %+v", stats)
			}
		})
	}
	t.Run("dropped invalidations without an authorizer", func(t *testing.T) {
		hub, err := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits()})
		if err != nil {
			t.Fatal(err)
		}
		first, _ := familyLease(t, hub, 1)
		second, _ := familyLease(t, hub, 2)
		hub.InvalidationsDropped(t.Context())
		waitCloses(t, first, ClosePolicyViolation)
		waitCloses(t, second, ClosePolicyViolation)
		deadline := time.Now().Add(time.Second)
		for hub.Stats().SweepClosed != 2 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if stats := hub.Stats(); stats.SweepClosed != 2 || stats.SweepDuration.Count != 0 {
			t.Fatalf("stats %+v", stats)
		}
	})
}

// wrongCountAuthorizer answers one result fewer than it was asked.
type wrongCountAuthorizer struct{}

func (wrongCountAuthorizer) CheckMany(_ context.Context, leases []AuthorizationLease) ([]bool, error) {
	allowed := make([]bool, len(leases)-1)
	for index := range allowed {
		allowed[index] = true
	}
	return allowed, nil
}
