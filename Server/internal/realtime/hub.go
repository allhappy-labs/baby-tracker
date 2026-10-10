package realtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/protocol"
)

const (
	CloseNormal          = 1000
	CloseGoingAway       = 1001
	ClosePolicyViolation = 1008
	CloseTryAgainLater   = 1013
)

var (
	ErrCapacity            = errors.New("live socket capacity exceeded")
	ErrReservationReleased = errors.New("live socket reservation released")
	ErrHubClosed           = errors.New("live hub closed")
	// ErrAuthorizationChanged refuses a reservation, or its Bind, because an
	// invalidation the hub processed after the handshake's authorization
	// read names its grant. It is an ErrReservationReleased.
	ErrAuthorizationChanged = fmt.Errorf("%w: authorization changed", ErrReservationReleased)
)

const (
	defaultCoalesceWindow = 50 * time.Millisecond
	defaultSweepTimeout   = 5 * time.Second
	// invalidationLogSize bounds the invalidations a reservation is checked
	// against (ReserveAfter). A handshake's authorization read and its
	// reservation are microseconds to milliseconds apart; a mark older than
	// this many invalidations is refused rather than checked.
	invalidationLogSize = 256
)

type Hint struct {
	FamilyID string          `json:"family_id"`
	Cursor   protocol.Cursor `json:"cursor"`
}

type Connection interface {
	WriteHint(context.Context, Hint) error
	Ping(context.Context) error
	Close(code int, reason string) error
}

type aborter interface{ Abort() error }

// AuthorizationLease is a live connection's authorization: the principal it
// authenticated as and the grant it was admitted with.
type AuthorizationLease = authority.LiveLease

type LiveAuthorizer interface {
	CheckMany(context.Context, []AuthorizationLease) ([]bool, error)
}

// invalidationSource is authority.Service.Subscribe. The hub implements
// authority.InvalidationOverflowSink, so a revocation that does not fit its
// queue triggers a full re-check instead of being lost.
type invalidationSource interface {
	Subscribe(authority.InvalidationSink) func()
}

type HubOptions struct {
	Limits        protocol.Limits
	Authorizer    LiveAuthorizer
	Invalidations invalidationSource
	Now           func() time.Time
	// CoalesceWindow: 0 means 50ms. Each family gets at most one delivery
	// per window, carrying the newest cursor published to it in the window.
	CoalesceWindow time.Duration
	// SweepTimeout: 0 means 5s. A re-check of the leases (periodic, or after
	// dropped invalidations) that has not answered by then closes every
	// lease it covers.
	SweepTimeout time.Duration
}

// InvalidationMark is a position in the invalidations a hub has processed.
// The live handshake takes one before its authorization read and reserves
// with ReserveAfter, so a revocation that commits after the read and reaches
// the hub before the reservation is not missed.
type InvalidationMark uint64

// loggedInvalidation is one processed invalidation; all marks dropped
// invalidations, which may have named anyone.
type loggedInvalidation struct {
	item authority.SecurityInvalidation
	all  bool
}

type Hub struct {
	mu            sync.Mutex
	limits        protocol.Limits
	authorizer    LiveAuthorizer
	ctx           context.Context
	cancel        context.CancelFunc
	unsubscribe   func()
	closed        bool
	nextID        atomic.Uint64
	reservations  map[uint64]*Reservation
	leases        map[uint64]*lease
	byFamily      map[string]map[*lease]struct{}
	deviceCount   map[string]int
	accountCount  map[string]int
	writeTimeout  time.Duration
	pingInterval  time.Duration
	sweepInterval time.Duration
	sweepTimeout  time.Duration
	maximumLife   time.Duration
	shutdownGrace time.Duration
	now           func() time.Time
	// recheck asks the sweep goroutine for a full re-check (dropped
	// invalidations); capacity 1, so requests during a running re-check
	// coalesce into one more.
	recheck chan struct{}
	// The invalidation log, under mu: invalidationSeq counts every processed
	// invalidation; the last invalidationLogSize are kept by seq modulo size.
	invalidationSeq uint64
	invalidationLog [invalidationLogSize]loggedInvalidation
	// Coalescing, under pendingMu (never held with mu): the newest hint per
	// family since the last flush, and whether a flush is armed.
	pendingMu     sync.Mutex
	pending       map[string]Hint
	coalesce      time.Duration
	flushTimer    *time.Timer
	armed         bool
	pendingClosed bool
}

type Reservation struct {
	hub       *Hub
	id        uint64
	principal access.Principal
	grant     access.Grant
	accepted  time.Time
	mu        sync.Mutex
	state     uint8 // 0 reserved, 1 bound, 2 released
	// revoked, under hub.mu: an invalidation released this reservation.
	revoked bool
}

type lease struct {
	hub        *Hub
	id         uint64
	principal  access.Principal
	grant      access.Grant
	accepted   time.Time
	deadline   time.Time
	connection Connection
	ctx        context.Context
	cancel     context.CancelFunc
	cleanup    sync.Once
	mu         sync.Mutex
	active     bool
	pending    *Hint
	domain     *protocol.Cursor
	highest    *protocol.Cursor
	wake       chan struct{}
}

func NewHub(parent context.Context, options HubOptions) (*Hub, error) {
	if parent == nil {
		return nil, errors.New("live hub context is required")
	}
	if err := options.Limits.Validate(); err != nil {
		return nil, err
	}
	if options.CoalesceWindow < 0 || options.SweepTimeout < 0 {
		return nil, errors.New("live hub windows must not be negative")
	}
	if options.CoalesceWindow == 0 {
		options.CoalesceWindow = defaultCoalesceWindow
	}
	if options.SweepTimeout == 0 {
		options.SweepTimeout = defaultSweepTimeout
	}
	ctx, cancel := context.WithCancel(context.Background())
	hub := &Hub{
		limits: options.Limits, authorizer: options.Authorizer, ctx: ctx, cancel: cancel,
		reservations: make(map[uint64]*Reservation), leases: make(map[uint64]*lease), byFamily: make(map[string]map[*lease]struct{}),
		deviceCount: make(map[string]int), accountCount: make(map[string]int),
		writeTimeout: 5 * time.Second, pingInterval: 25 * time.Second, sweepInterval: 5 * time.Second, sweepTimeout: options.SweepTimeout,
		maximumLife: 15 * time.Minute, shutdownGrace: 5 * time.Second, now: time.Now,
		recheck: make(chan struct{}, 1), pending: make(map[string]Hint), coalesce: options.CoalesceWindow,
	}
	if options.Now != nil {
		hub.now = options.Now
	}
	if options.Invalidations != nil {
		hub.unsubscribe = options.Invalidations.Subscribe(hub)
	}
	go hub.runSweep()
	go func() {
		select {
		case <-parent.Done():
			shutdown, stop := context.WithTimeout(context.Background(), hub.shutdownGrace)
			defer stop()
			_ = hub.Shutdown(shutdown)
		case <-hub.ctx.Done():
		}
	}()
	return hub, nil
}

// InvalidationMark is the hub's position in the invalidations it has
// processed. Take it before the authorization read that ReserveAfter admits.
func (hub *Hub) InvalidationMark() InvalidationMark {
	if hub == nil {
		return 0
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return InvalidationMark(hub.invalidationSeq)
}

// ReserveAfter holds capacity for a connection authorized as principal and
// grant by an authorization read made after mark was taken. It refuses
// (ErrAuthorizationChanged) when an invalidation processed since mark names
// the grant: the read may not have seen that revocation's commit. Once
// reserved, a later invalidation releases the reservation itself, so Bind
// refuses; together they close the window from the authorization read to
// Bind.
func (hub *Hub) ReserveAfter(_ context.Context, mark InvalidationMark, principal access.Principal, grant access.Grant) (*Reservation, error) {
	if hub == nil || !protocol.ValidUUID(principal.AccountID) || !protocol.ValidUUID(principal.DeviceID) || !protocol.ValidUUID(principal.SessionID) ||
		principal.RecoveryEpoch == 0 || principal.SecurityRevision == 0 || principal.Scope != "family" || principal.SessionScope != "family" ||
		principal.SessionFamilyID != grant.FamilyID || principal.AccountID != grant.AccountID || !protocol.ValidUUID(grant.FamilyID) || grant.Revision == 0 {
		return nil, errors.New("invalid live reservation")
	}
	now := hub.now().UTC()
	if principal.AccessExpiresAt.IsZero() || !principal.AccessExpiresAt.After(now) || principal.AccessExpiresAt.Year() > 9999 {
		return nil, errors.New("invalid live access deadline")
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.closed {
		return nil, ErrHubClosed
	}
	if hub.invalidatedSinceLocked(uint64(mark), principal, grant) {
		return nil, ErrAuthorizationChanged
	}
	if len(hub.reservations)+len(hub.leases) >= hub.limits.SocketProcessLimit ||
		hub.deviceCount[principal.DeviceID] >= hub.limits.SocketDeviceLimit || hub.accountCount[principal.AccountID] >= hub.limits.SocketAccountLimit {
		return nil, ErrCapacity
	}
	id := hub.nextID.Add(1)
	reservation := &Reservation{hub: hub, id: id, principal: principal, grant: grant, accepted: now}
	hub.reservations[id] = reservation
	hub.deviceCount[principal.DeviceID]++
	hub.accountCount[principal.AccountID]++
	return reservation, nil
}

func (reservation *Reservation) Bind(connection Connection) (func(), error) {
	if reservation == nil || connection == nil || reservation.hub == nil {
		return nil, ErrReservationReleased
	}
	reservation.mu.Lock()
	defer reservation.mu.Unlock()
	if reservation.state != 0 {
		return nil, ErrReservationReleased
	}
	hub := reservation.hub
	hub.mu.Lock()
	if hub.closed || hub.reservations[reservation.id] != reservation {
		revoked := reservation.revoked
		hub.mu.Unlock()
		reservation.state = 2
		if revoked {
			return nil, ErrAuthorizationChanged
		}
		return nil, ErrReservationReleased
	}
	deadline := reservation.accepted.Add(hub.maximumLife)
	if reservation.principal.AccessExpiresAt.Before(deadline) {
		deadline = reservation.principal.AccessExpiresAt
	}
	remaining := deadline.Sub(hub.now().UTC())
	if remaining <= 0 {
		delete(hub.reservations, reservation.id)
		hub.releaseCountsLocked(reservation.principal)
		hub.mu.Unlock()
		reservation.state = 2
		return nil, ErrReservationReleased
	}
	delete(hub.reservations, reservation.id)
	connectionCtx, cancel := context.WithTimeout(hub.ctx, remaining)
	item := &lease{hub: hub, id: reservation.id, principal: reservation.principal, grant: reservation.grant,
		accepted: reservation.accepted, deadline: deadline, connection: connection, ctx: connectionCtx, cancel: cancel,
		active: true, wake: make(chan struct{}, 1)}
	hub.leases[item.id] = item
	if hub.byFamily[item.grant.FamilyID] == nil {
		hub.byFamily[item.grant.FamilyID] = make(map[*lease]struct{})
	}
	hub.byFamily[item.grant.FamilyID][item] = struct{}{}
	hub.mu.Unlock()
	reservation.state = 1
	go item.run()
	return item.release, nil
}

func (reservation *Reservation) Release() {
	if reservation == nil || reservation.hub == nil {
		return
	}
	reservation.mu.Lock()
	defer reservation.mu.Unlock()
	if reservation.state != 0 {
		return
	}
	reservation.state = 2
	reservation.hub.mu.Lock()
	if reservation.hub.reservations[reservation.id] == reservation {
		delete(reservation.hub.reservations, reservation.id)
		reservation.hub.releaseCountsLocked(reservation.principal)
	}
	reservation.hub.mu.Unlock()
}

func (item *lease) release() {
	item.hub.mu.Lock()
	item.releaseLocked()
	item.hub.mu.Unlock()
}

func (item *lease) releaseLocked() {
	item.cleanup.Do(func() {
		item.mu.Lock()
		item.active = false
		item.pending = nil
		item.mu.Unlock()
		item.cancel()
		if item.hub.leases[item.id] == item {
			delete(item.hub.leases, item.id)
			family := item.hub.byFamily[item.grant.FamilyID]
			delete(family, item)
			if len(family) == 0 {
				delete(item.hub.byFamily, item.grant.FamilyID)
			}
			item.hub.releaseCountsLocked(item.principal)
		}
	})
}

func (hub *Hub) releaseCountsLocked(principal access.Principal) {
	hub.deviceCount[principal.DeviceID]--
	if hub.deviceCount[principal.DeviceID] <= 0 {
		delete(hub.deviceCount, principal.DeviceID)
	}
	hub.accountCount[principal.AccountID]--
	if hub.accountCount[principal.AccountID] <= 0 {
		delete(hub.accountCount, principal.AccountID)
	}
}

func (item *lease) run() {
	defer item.release()
	remaining := item.deadline.Sub(item.hub.now().UTC())
	if remaining <= 0 {
		item.release()
		_ = item.connection.Close(CloseNormal, "maximum lifetime")
		return
	}
	lifetime := time.NewTimer(remaining)
	defer lifetime.Stop()
	ping := time.NewTicker(item.hub.pingInterval)
	defer ping.Stop()
	for {
		select {
		case <-item.ctx.Done():
			if errors.Is(item.ctx.Err(), context.DeadlineExceeded) {
				item.release()
				_ = item.connection.Close(CloseNormal, "maximum lifetime")
			}
			return
		case <-lifetime.C:
			item.release()
			_ = item.connection.Close(CloseNormal, "maximum lifetime")
			return
		case <-ping.C:
			ctx, cancel := context.WithTimeout(item.ctx, item.hub.writeTimeout)
			err := item.connection.Ping(ctx)
			cancel()
			if err != nil {
				item.release()
				_ = item.connection.Close(CloseTryAgainLater, "slow consumer")
				return
			}
		case <-item.wake:
			item.mu.Lock()
			if !item.active {
				item.mu.Unlock()
				continue
			}
			hint := item.pending
			item.pending = nil
			item.mu.Unlock()
			if hint == nil {
				continue
			}
			ctx, cancel := context.WithTimeout(item.ctx, item.hub.writeTimeout)
			err := item.connection.WriteHint(ctx, *hint)
			cancel()
			if err != nil {
				item.release()
				_ = item.connection.Close(CloseTryAgainLater, "slow consumer")
				return
			}
		}
	}
}

// Publish announces a family's committed high water. Hints are coalesced
// per family: within one window only the newest cursor is kept (a higher
// sequence, or a different cursor domain, replaces the pending hint; an older
// domain's pending hint is delivered at once first, so a connection still
// sees the domain change), and each window ends in one delivery per family.
// A family with no bound connection keeps nothing: a connection bound later
// catches up with its own sync on connect.
func (hub *Hub) Publish(_ context.Context, hint Hint) {
	if hub == nil || !protocol.ValidUUID(hint.FamilyID) || hint.Cursor.Validate() != nil || hint.Cursor.FamilyID != hint.FamilyID {
		return
	}
	if !hub.HasRegisteredFamily(hint.FamilyID) {
		return
	}
	var superseded *Hint
	hub.pendingMu.Lock()
	if hub.pendingClosed {
		hub.pendingMu.Unlock()
		return
	}
	current, ok := hub.pending[hint.FamilyID]
	switch {
	case !ok:
		hub.pending[hint.FamilyID] = hint
	case !sameCursorDomain(current.Cursor, hint.Cursor):
		superseded = &current
		hub.pending[hint.FamilyID] = hint
	case hint.Cursor.Sequence > current.Cursor.Sequence:
		hub.pending[hint.FamilyID] = hint
	}
	if !hub.armed {
		hub.armed = true
		if hub.flushTimer == nil {
			hub.flushTimer = time.AfterFunc(hub.coalesce, hub.flush)
		} else {
			hub.flushTimer.Reset(hub.coalesce)
		}
	}
	hub.pendingMu.Unlock()
	if superseded != nil {
		hub.deliver(*superseded)
	}
}

// flush delivers every pending hint. A Publish after the swap arms the next
// window, so no hint waits for more than one window plus one flush.
func (hub *Hub) flush() {
	hub.pendingMu.Lock()
	batch := hub.pending
	hub.armed = false
	if len(batch) == 0 {
		// Nothing to swap out; the map stays the publishers' alone.
		hub.pendingMu.Unlock()
		return
	}
	hub.pending = make(map[string]Hint, len(batch))
	hub.pendingMu.Unlock()
	for _, hint := range batch {
		hub.deliver(hint)
	}
}

// deliver hands hint to every connection bound to its family. It never
// waits on a connection: each lease keeps one pending hint, the newest, and
// its own goroutine writes it.
func (hub *Hub) deliver(hint Hint) {
	for _, item := range hub.familyLeases(hint.FamilyID, nil) {
		hub.offer(item, hint, false)
	}
}

// Reannounce is the wake worker's path: it hands hint to the connections of
// deviceIDs bound to its family, at once (no coalescing window), even to a
// connection that was already sent that cursor, which Publish never repeats.
// A device behind its family has not pulled what it was told, and the
// client syncs on any hint its local cursor does not cover. A connection
// that was sent a higher cursor is re-sent that one. It returns the number
// of connections handed a hint; other devices and families get nothing.
func (hub *Hub) Reannounce(_ context.Context, hint Hint, deviceIDs []string) int {
	if hub == nil || len(deviceIDs) == 0 || !protocol.ValidUUID(hint.FamilyID) || hint.Cursor.Validate() != nil || hint.Cursor.FamilyID != hint.FamilyID {
		return 0
	}
	devices := make(map[string]struct{}, len(deviceIDs))
	for _, device := range deviceIDs {
		devices[device] = struct{}{}
	}
	reached := 0
	for _, item := range hub.familyLeases(hint.FamilyID, devices) {
		if hub.offer(item, hint, true) {
			reached++
		}
	}
	return reached
}

// familyLeases is the leases bound to familyID, of the given devices only
// when devices is not nil.
func (hub *Hub) familyLeases(familyID string, devices map[string]struct{}) []*lease {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	family := hub.byFamily[familyID]
	items := make([]*lease, 0, len(family))
	for item := range family {
		if devices != nil {
			if _, named := devices[item.principal.DeviceID]; !named {
				continue
			}
		}
		items = append(items, item)
	}
	return items
}

// offer hands hint to one lease and reports whether the lease now has a
// hint to write. A cursor of another domain closes the lease. Without force
// a cursor at or below the highest the lease was sent is dropped; with
// force the lease is re-sent the higher of the two.
func (hub *Hub) offer(item *lease, hint Hint, force bool) bool {
	item.mu.Lock()
	if !item.active {
		item.mu.Unlock()
		return false
	}
	if item.domain != nil && !sameCursorDomain(*item.domain, hint.Cursor) {
		item.mu.Unlock()
		item.release()
		hub.closeDetached([]*lease{item}, ClosePolicyViolation, "recovery epoch changed")
		return false
	}
	if item.domain == nil {
		domain := hint.Cursor
		item.domain = &domain
	}
	if item.highest != nil && hint.Cursor.Sequence <= item.highest.Sequence {
		if !force {
			item.mu.Unlock()
			return false
		}
		hint.Cursor = *item.highest
	}
	highest := hint.Cursor
	item.highest = &highest
	if item.pending == nil || sameCursorDomain(item.pending.Cursor, hint.Cursor) && hint.Cursor.Sequence > item.pending.Cursor.Sequence {
		copy := hint
		item.pending = &copy
	}
	select {
	case item.wake <- struct{}{}:
	default:
	}
	item.mu.Unlock()
	return true
}

func sameCursorDomain(left, right protocol.Cursor) bool {
	return left.ServerInstanceID == right.ServerInstanceID && left.RecoveryEpoch == right.RecoveryEpoch && left.FamilyID == right.FamilyID
}

// Invalidate closes the live connections a committed command's invalidation
// names, at once rather than at the next sweep. Empty fields match every
// lease. Revision tells a lease admitted after the command apart from one it
// invalidated, and is compared within its own scope:
//
//   - a family invalidation (FamilyID set) carries the family audit seq, and
//     a lease whose grant was issued at or after it is kept;
//   - an account invalidation (FamilyID empty: Logout, refresh reuse,
//     MarkIdentityUnavailable, DeleteAccount) carries the account audit seq,
//     which is not comparable with a grant's family seq; it is compared with
//     the session's issued seq (Principal.SecurityRevision, an account seq),
//     so a session issued at or after it is kept.
//
// It also releases every reservation it names (a later Bind refuses with
// ErrAuthorizationChanged), and is logged so a reservation made after it
// from an earlier authorization read is refused (ReserveAfter).
func (hub *Hub) Invalidate(_ context.Context, invalidation authority.SecurityInvalidation) {
	if hub == nil || invalidation.Revision == 0 {
		return
	}
	hub.mu.Lock()
	hub.logInvalidationLocked(loggedInvalidation{item: invalidation})
	items := make([]*lease, 0)
	for _, item := range hub.leases {
		if !invalidates(invalidation, item.principal, item.grant) {
			continue
		}
		items = append(items, item)
		item.releaseLocked()
	}
	for _, reservation := range hub.reservations {
		if invalidates(invalidation, reservation.principal, reservation.grant) {
			hub.revokeReservationLocked(reservation)
		}
	}
	hub.mu.Unlock()
	hub.closeDetached(items, ClosePolicyViolation, "authorization changed")
}

// invalidates reports whether invalidation names a connection authorized as
// principal and grant (Invalidate's rules: empty fields match, Revision is
// compared within its own scope).
func invalidates(invalidation authority.SecurityInvalidation, principal access.Principal, grant access.Grant) bool {
	issued := grant.Revision
	if invalidation.FamilyID == "" {
		issued = principal.SecurityRevision
	}
	return invalidation.Revision > issued &&
		(invalidation.AccountID == "" || invalidation.AccountID == principal.AccountID) &&
		(invalidation.FamilyID == "" || invalidation.FamilyID == grant.FamilyID) &&
		(invalidation.SessionID == "" || invalidation.SessionID == principal.SessionID)
}

func (hub *Hub) logInvalidationLocked(entry loggedInvalidation) {
	hub.invalidationSeq++
	hub.invalidationLog[hub.invalidationSeq%invalidationLogSize] = entry
}

// invalidatedSinceLocked reports whether an invalidation processed after
// mark names principal and grant. A mark older than the log fails closed.
func (hub *Hub) invalidatedSinceLocked(mark uint64, principal access.Principal, grant access.Grant) bool {
	if mark >= hub.invalidationSeq {
		return false
	}
	if hub.invalidationSeq-mark > invalidationLogSize {
		return true
	}
	for seq := mark + 1; seq <= hub.invalidationSeq; seq++ {
		entry := hub.invalidationLog[seq%invalidationLogSize]
		if entry.all || invalidates(entry.item, principal, grant) {
			return true
		}
	}
	return false
}

// revokeReservationLocked releases a reservation an invalidation named, and
// its capacity, so its Bind refuses.
func (hub *Hub) revokeReservationLocked(reservation *Reservation) {
	if hub.reservations[reservation.id] != reservation {
		return
	}
	delete(hub.reservations, reservation.id)
	hub.releaseCountsLocked(reservation.principal)
	reservation.revoked = true
}

// InvalidationsDropped is called, on the authority's delivery goroutine,
// when invalidations did not fit the hub's subscription queue. Which grants
// they named is unknown, so every reservation in flight is released and
// every earlier mark refused at once, and the bound leases are re-checked
// against the authority on the sweep goroutine (a check that fails or misses
// its deadline closes every lease; a hub with no authorizer cannot re-check,
// so it closes every lease). The call never waits for the re-check.
func (hub *Hub) InvalidationsDropped(context.Context) {
	if hub == nil {
		return
	}
	hub.mu.Lock()
	hub.logInvalidationLocked(loggedInvalidation{all: true})
	for _, reservation := range hub.reservations {
		hub.revokeReservationLocked(reservation)
	}
	hub.mu.Unlock()
	select {
	case hub.recheck <- struct{}{}:
	default:
	}
}

func (hub *Hub) closeDetached(items []*lease, code int, reason string) {
	for _, item := range items {
		item := item
		go func() {
			done := make(chan struct{})
			go func() {
				_ = item.connection.Close(code, reason)
				close(done)
			}()
			timer := time.NewTimer(hub.writeTimeout)
			defer timer.Stop()
			select {
			case <-done:
			case <-timer.C:
				if connection, ok := item.connection.(aborter); ok {
					_ = connection.Abort()
				}
			}
		}()
	}
}

// runSweep owns every re-check: the periodic sweep (with an authorizer) and
// the re-check after dropped invalidations. It runs apart from the
// invalidation and publish paths, so a slow authority delays neither.
func (hub *Hub) runSweep() {
	var tick <-chan time.Time
	if hub.authorizer != nil {
		ticker := time.NewTicker(hub.sweepInterval)
		defer ticker.Stop()
		tick = ticker.C
	}
	for {
		select {
		case <-hub.ctx.Done():
			return
		case <-tick:
			_ = hub.sweep(hub.ctx)
		case <-hub.recheck:
			if hub.authorizer == nil {
				hub.closeAll(ClosePolicyViolation, "authorization unavailable")
				continue
			}
			_ = hub.sweep(hub.ctx)
		}
	}
}

func (hub *Hub) closeAll(code int, reason string) {
	hub.mu.Lock()
	items := make([]*lease, 0, len(hub.leases))
	for _, item := range hub.leases {
		items = append(items, item)
	}
	hub.mu.Unlock()
	hub.detach(items)
	hub.closeDetached(items, code, reason)
}

func (hub *Hub) sweep(ctx context.Context) error {
	if hub == nil || hub.authorizer == nil {
		return nil
	}
	hub.mu.Lock()
	items := make([]*lease, 0, len(hub.leases))
	leasing := make([]AuthorizationLease, 0, len(hub.leases))
	for _, item := range hub.leases {
		items = append(items, item)
		leasing = append(leasing, AuthorizationLease{Principal: item.principal, Grant: item.grant})
	}
	hub.mu.Unlock()
	if len(leasing) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, hub.sweepTimeout)
	defer cancel()
	allowed, err := hub.authorizer.CheckMany(ctx, leasing)
	if err == nil && ctx.Err() != nil {
		// Answered, but after the deadline: fail closed as if it had not.
		err = ctx.Err()
	}
	if err != nil {
		hub.detach(items)
		hub.closeDetached(items, ClosePolicyViolation, "authorization unavailable")
		return err
	}
	if len(allowed) != len(items) {
		hub.detach(items)
		hub.closeDetached(items, ClosePolicyViolation, "authorization unavailable")
		return errors.New("live authorizer returned wrong result count")
	}
	denied := make([]*lease, 0)
	for index, ok := range allowed {
		if !ok {
			denied = append(denied, items[index])
		}
	}
	hub.detach(denied)
	hub.closeDetached(denied, ClosePolicyViolation, "authorization changed")
	return nil
}

func (hub *Hub) detach(items []*lease) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for _, item := range items {
		if hub.leases[item.id] == item {
			item.releaseLocked()
		}
	}
}

func (hub *Hub) Context() context.Context {
	if hub == nil {
		return context.Background()
	}
	return hub.ctx
}

func (hub *Hub) Shutdown(ctx context.Context) error {
	if hub == nil {
		return nil
	}
	// No hint is kept or delivered once shutdown starts.
	hub.pendingMu.Lock()
	hub.pendingClosed = true
	hub.pending = nil
	if hub.flushTimer != nil {
		hub.flushTimer.Stop()
	}
	hub.pendingMu.Unlock()
	hub.mu.Lock()
	if hub.closed {
		hub.mu.Unlock()
		return nil
	}
	hub.closed = true
	reservations := make([]*Reservation, 0, len(hub.reservations))
	for _, reservation := range hub.reservations {
		reservations = append(reservations, reservation)
	}
	items := make([]*lease, 0, len(hub.leases))
	for _, item := range hub.leases {
		items = append(items, item)
		item.releaseLocked()
	}
	for _, reservation := range reservations {
		if hub.reservations[reservation.id] == reservation {
			delete(hub.reservations, reservation.id)
			hub.releaseCountsLocked(reservation.principal)
		}
	}
	hub.mu.Unlock()
	defer hub.cancel()
	if hub.unsubscribe != nil {
		hub.unsubscribe()
	}
	for _, reservation := range reservations {
		reservation.mu.Lock()
		reservation.state = 2
		reservation.mu.Unlock()
	}
	done := make(chan struct{})
	go func() {
		var wait sync.WaitGroup
		for _, item := range items {
			wait.Add(1)
			go func(item *lease) {
				defer wait.Done()
				_ = item.connection.Close(CloseGoingAway, "server shutdown")
			}(item)
		}
		wait.Wait()
		close(done)
	}()
	grace := hub.shutdownGrace
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if half := remaining / 2; half < grace {
			grace = half
		}
	}
	if grace < 0 {
		grace = 0
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}
	var aborts sync.WaitGroup
	for _, item := range items {
		if connection, ok := item.connection.(aborter); ok {
			aborts.Add(1)
			go func() {
				defer aborts.Done()
				_ = connection.Abort()
			}()
		}
	}
	aborted := make(chan struct{})
	go func() {
		aborts.Wait()
		close(aborted)
	}()
	select {
	case <-done:
		return nil
	case <-aborted:
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// DeviceConnected reports whether deviceID holds a bound lease or a
// reservation here: the wake worker reaches such a device through
// Reannounce and never pushes it. A reservation counts because its
// connection binds within the handshake and syncs on connect.
func (hub *Hub) DeviceConnected(deviceID string) bool {
	if hub == nil {
		return false
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return hub.deviceCount[deviceID] > 0
}

// HasRegisteredFamily checks bound leases only; notifications cannot create registrations.
func (hub *Hub) HasRegisteredFamily(familyID string) bool {
	if hub == nil {
		return false
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return len(hub.byFamily[familyID]) != 0
}
