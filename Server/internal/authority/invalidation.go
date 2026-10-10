package authority

import (
	"context"
	"sync"
	"sync/atomic"
)

// Security invalidations tell live-connection holders (the realtime hub) that
// a committed command reduced someone's authority, so they can close the
// affected connections at once instead of at their next sweep. They are a
// prompt, never authorization: every transaction re-reads the session and
// family rows (security/invalidation.go semantics).
//
// An invalidation is published only after its command's transaction
// committed, never for a rolled-back command or an exact retry, and only by
// commands that can reduce a live grant's authority:
//
//   - Logout: one per revoked session of the device;
//   - MarkIdentityUnavailable and DeleteAccount: the account;
//   - Refresh: one per session a reuse revoked (a rotation publishes
//     nothing: legacy's session_rotated item named the new child, which no
//     connection holds yet, and would only take queue slots from real
//     revocations);
//   - LeaveFamily, RemoveMember, ChangeRole, TransferOwner,
//     ScheduleDeletion, operator owner recovery: each affected account in
//     the family;
//   - StartFamilyPurge, CompleteFamilyPurge, operator entitlement
//     suspension: the family;
//   - operator integration revocation: the grant's account in the family.

const (
	maxInvalidationSubscribers = 64
	invalidationQueueSize      = 32
)

// SecurityInvalidation names what a committed command invalidated. Empty
// fields match everything. Revision is the audit seq the command left on its
// primary scope: the family's when FamilyID is set (comparable with a family
// grant's revision), the account's otherwise (comparable with a session's
// issued seq).
type SecurityInvalidation struct {
	AccountID string
	FamilyID  string
	SessionID string
	Revision  uint64
}

// InvalidationSink receives invalidations on its own goroutine, in commit
// order per subscription.
type InvalidationSink interface {
	Invalidate(context.Context, SecurityInvalidation)
}

// InvalidationOverflowSink is an InvalidationSink that must not lose a
// revocation silently: when an invalidation does not fit its bounded queue,
// InvalidationsDropped is called on the delivery goroutine (once per burst of
// drops, coalesced) after every drop it covers, so the sink can re-check all
// of its grants against the committed rows. Every dropped item's command had
// committed before it was published, so a re-check that starts after the
// call sees it.
type InvalidationOverflowSink interface {
	InvalidationSink
	InvalidationsDropped(context.Context)
}

// subscription is one subscriber's bounded queue and its coalesced overflow
// signal.
type subscription struct {
	queue   chan SecurityInvalidation
	dropped chan struct{}
}

// invalidations is the Service's subscriber set.
type invalidations struct {
	mu     sync.RWMutex
	queues map[uint64]*subscription
	nextID atomic.Uint64
}

// Subscribe delivers every later invalidation to sink until the returned
// function is called (idempotent). Each subscriber has a bounded queue; an
// invalidation that does not fit is dropped for that subscriber. A sink that
// implements InvalidationOverflowSink is then told (InvalidationsDropped);
// any other sink relies on its own periodic re-check. At most 64 subscribers
// are kept; beyond that Subscribe is a no-op. A panicking sink loses only the
// call that panicked.
func (s *Service) Subscribe(sink InvalidationSink) (unsubscribe func()) {
	if s == nil || sink == nil {
		return func() {}
	}
	set := &s.invalidations
	set.mu.Lock()
	if set.queues == nil {
		set.queues = make(map[uint64]*subscription)
	}
	if len(set.queues) >= maxInvalidationSubscribers {
		set.mu.Unlock()
		return func() {}
	}
	id := set.nextID.Add(1)
	sub := &subscription{queue: make(chan SecurityInvalidation, invalidationQueueSize), dropped: make(chan struct{}, 1)}
	set.queues[id] = sub
	set.mu.Unlock()
	overflow, _ := sink.(InvalidationOverflowSink)
	go func() {
		for {
			select {
			case item, ok := <-sub.queue:
				if !ok {
					return
				}
				deliver(func() { sink.Invalidate(context.Background(), item) })
			case <-sub.dropped:
				if overflow != nil {
					deliver(func() { overflow.InvalidationsDropped(context.Background()) })
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			set.mu.Lock()
			if current, ok := set.queues[id]; ok {
				delete(set.queues, id)
				close(current.queue)
			}
			set.mu.Unlock()
		})
	}
}

// deliver runs one sink call, containing its panic.
func deliver(call func()) {
	defer func() { _ = recover() }()
	call()
}

// publish queues items, deduplicated, to every subscriber. Callers publish
// after their transaction committed. Items with a zero revision or naming
// nothing are dropped.
func (s *Service) publish(items ...SecurityInvalidation) {
	if s == nil || len(items) == 0 {
		return
	}
	seen := make(map[SecurityInvalidation]struct{}, len(items))
	unique := items[:0:0]
	for _, item := range items {
		if item.Revision == 0 || (item.AccountID == "" && item.FamilyID == "" && item.SessionID == "") {
			continue
		}
		if _, dup := seen[item]; dup {
			continue
		}
		seen[item] = struct{}{}
		unique = append(unique, item)
	}
	if len(unique) == 0 {
		return
	}
	set := &s.invalidations
	set.mu.RLock()
	defer set.mu.RUnlock()
	for _, sub := range set.queues {
		for _, item := range unique {
			select {
			case sub.queue <- item:
			default:
				select {
				case sub.dropped <- struct{}{}:
				default:
				}
			}
		}
	}
}

// invalidationSubscriberCount is the number of live subscriptions.
func (s *Service) invalidationSubscriberCount() int {
	s.invalidations.mu.RLock()
	defer s.invalidations.mu.RUnlock()
	return len(s.invalidations.queues)
}
