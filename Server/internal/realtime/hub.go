package realtime

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/security"
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

type AuthorizationLease struct {
	Principal access.Principal
	Grant     access.Grant
}

type LiveAuthorizer interface {
	CheckMany(context.Context, []AuthorizationLease) ([]bool, error)
}

type invalidationSource interface {
	Subscribe(security.InvalidationSink) func()
}

type HubOptions struct {
	Limits        protocol.Limits
	Authorizer    LiveAuthorizer
	Invalidations invalidationSource
	Now           func() time.Time
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
	maximumLife   time.Duration
	shutdownGrace time.Duration
	now           func() time.Time
}

type Reservation struct {
	hub       *Hub
	id        uint64
	principal access.Principal
	grant     access.Grant
	accepted  time.Time
	mu        sync.Mutex
	state     uint8 // 0 reserved, 1 bound, 2 released
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
	ctx, cancel := context.WithCancel(context.Background())
	hub := &Hub{
		limits: options.Limits, authorizer: options.Authorizer, ctx: ctx, cancel: cancel,
		reservations: make(map[uint64]*Reservation), leases: make(map[uint64]*lease), byFamily: make(map[string]map[*lease]struct{}),
		deviceCount: make(map[string]int), accountCount: make(map[string]int),
		writeTimeout: 5 * time.Second, pingInterval: 25 * time.Second, sweepInterval: 5 * time.Second,
		maximumLife: 15 * time.Minute, shutdownGrace: 5 * time.Second, now: time.Now,
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

func (hub *Hub) Reserve(_ context.Context, principal access.Principal, grant access.Grant) (*Reservation, error) {
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
		hub.mu.Unlock()
		reservation.state = 2
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

func (hub *Hub) Publish(_ context.Context, hint Hint) {
	if hub == nil || !protocol.ValidUUID(hint.FamilyID) || hint.Cursor.Validate() != nil || hint.Cursor.FamilyID != hint.FamilyID {
		return
	}
	hub.mu.Lock()
	family := hub.byFamily[hint.FamilyID]
	items := make([]*lease, 0, len(family))
	for item := range family {
		items = append(items, item)
	}
	hub.mu.Unlock()
	for _, item := range items {
		item.mu.Lock()
		if !item.active {
			item.mu.Unlock()
			continue
		}
		if item.domain != nil && !sameCursorDomain(*item.domain, hint.Cursor) {
			item.mu.Unlock()
			item.release()
			hub.closeDetached([]*lease{item}, ClosePolicyViolation, "recovery epoch changed")
			continue
		}
		if item.domain == nil {
			domain := hint.Cursor
			item.domain = &domain
		}
		if item.highest != nil && hint.Cursor.Sequence <= item.highest.Sequence {
			item.mu.Unlock()
			continue
		}
		highest := hint.Cursor
		item.highest = &highest
		if item.pending == nil || sameCursorDomain(item.pending.Cursor, hint.Cursor) && hint.Cursor.Sequence > item.pending.Cursor.Sequence {
			copy := hint
			item.pending = &copy
			select {
			case item.wake <- struct{}{}:
			default:
			}
		}
		item.mu.Unlock()
	}
}

func sameCursorDomain(left, right protocol.Cursor) bool {
	return left.ServerInstanceID == right.ServerInstanceID && left.RecoveryEpoch == right.RecoveryEpoch && left.FamilyID == right.FamilyID
}

func (hub *Hub) Invalidate(_ context.Context, invalidation security.SecurityInvalidation) {
	if hub == nil || invalidation.Revision == 0 {
		return
	}
	hub.mu.Lock()
	items := make([]*lease, 0)
	for _, item := range hub.leases {
		if invalidation.Revision <= item.grant.Revision ||
			invalidation.AccountID != "" && invalidation.AccountID != item.principal.AccountID ||
			invalidation.FamilyID != "" && invalidation.FamilyID != item.grant.FamilyID ||
			invalidation.SessionID != "" && invalidation.SessionID != item.principal.SessionID {
			continue
		}
		items = append(items, item)
		item.releaseLocked()
	}
	hub.mu.Unlock()
	hub.closeDetached(items, ClosePolicyViolation, "authorization changed")
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

func (hub *Hub) runSweep() {
	if hub.authorizer == nil {
		return
	}
	ticker := time.NewTicker(hub.sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-hub.ctx.Done():
			return
		case <-ticker.C:
			_ = hub.sweep(hub.ctx)
		}
	}
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
	allowed, err := hub.authorizer.CheckMany(ctx, leasing)
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

func (hub *Hub) RegisteredFamilies() []string {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	result := make([]string, 0, len(hub.byFamily))
	for familyID := range hub.byFamily {
		result = append(result, familyID)
	}
	return result
}

func (hub *Hub) RegisteredLeases() []AuthorizationLease {
	if hub == nil {
		return nil
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	byFamily := make(map[string]AuthorizationLease)
	for _, item := range hub.leases {
		if _, exists := byFamily[item.grant.FamilyID]; !exists {
			byFamily[item.grant.FamilyID] = AuthorizationLease{Principal: item.principal, Grant: item.grant}
		}
	}
	result := make([]AuthorizationLease, 0, len(byFamily))
	for _, item := range byFamily {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Grant.FamilyID < result[j].Grant.FamilyID })
	return result
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

// HasRegisteredFamily checks bound leases only; notifications cannot create registrations.
func (hub *Hub) HasRegisteredFamily(familyID string) bool {
	if hub == nil {
		return false
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return len(hub.byFamily[familyID]) != 0
}
