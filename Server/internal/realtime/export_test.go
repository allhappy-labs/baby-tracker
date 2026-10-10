package realtime

import (
	"context"
	"sort"

	"babytracker/server/internal/access"
)

// Reserve is ReserveAfter at the hub's current mark, for tests that hold a
// principal and grant no authorization read produced. A production caller
// has read its authorization before reserving, so it takes InvalidationMark
// before that read and passes it to ReserveAfter.
func (hub *Hub) Reserve(ctx context.Context, principal access.Principal, grant access.Grant) (*Reservation, error) {
	return hub.ReserveAfter(ctx, hub.InvalidationMark(), principal, grant)
}

// FlushNow delivers every pending family hint at once, without waiting for
// the coalescing window.
func (hub *Hub) FlushNow() {
	hub.pendingMu.Lock()
	if hub.flushTimer != nil {
		hub.flushTimer.Stop()
	}
	hub.pendingMu.Unlock()
	hub.flush()
}

// RegisteredLeases is one bound lease per family.
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
