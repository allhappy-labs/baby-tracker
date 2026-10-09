package security

import (
	"context"
	"sync"
)

const (
	maxInvalidationSubscribers = 64
	invalidationQueueSize      = 32
)

type SecurityInvalidation struct {
	AccountID string
	FamilyID  string
	SessionID string
	Revision  uint64
}

type InvalidationSink interface {
	Invalidate(context.Context, SecurityInvalidation)
}

func (service *Service) Subscribe(sink InvalidationSink) (unsubscribe func()) {
	if service == nil || sink == nil {
		return func() {}
	}
	service.invalidationMu.Lock()
	if service.invalidations == nil {
		service.invalidations = make(map[uint64]chan SecurityInvalidation)
	}
	if len(service.invalidations) >= maxInvalidationSubscribers {
		service.invalidationMu.Unlock()
		return func() {}
	}
	id := service.invalidationID.Add(1)
	queue := make(chan SecurityInvalidation, invalidationQueueSize)
	service.invalidations[id] = queue
	service.invalidationMu.Unlock()
	go func() {
		defer func() { _ = recover() }()
		for item := range queue {
			sink.Invalidate(context.Background(), item)
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			service.invalidationMu.Lock()
			if current, ok := service.invalidations[id]; ok {
				delete(service.invalidations, id)
				close(current)
			}
			service.invalidationMu.Unlock()
		})
	}
}

func (service *Service) publishInvalidations(event Event) {
	if service == nil || event.Revision == 0 {
		return
	}
	items := invalidationsForEvent(event)
	if len(items) == 0 {
		return
	}
	service.invalidationMu.RLock()
	defer service.invalidationMu.RUnlock()
	for _, queue := range service.invalidations {
		for _, item := range items {
			select {
			case queue <- item:
			default:
			}
		}
	}
}

func invalidationsForEvent(event Event) []SecurityInvalidation {
	seen := make(map[SecurityInvalidation]struct{})
	items := make([]SecurityInvalidation, 0, len(event.Effects))
	for _, effect := range event.Effects {
		if effect.AccountID == "" && effect.FamilyID == "" && effect.SessionID == "" {
			continue
		}
		item := SecurityInvalidation{AccountID: effect.AccountID, FamilyID: effect.FamilyID, SessionID: effect.SessionID, Revision: event.Revision}
		if _, exists := seen[item]; exists {
			continue
		}
		seen[item] = struct{}{}
		items = append(items, item)
	}
	return items
}

func (service *Service) invalidationSubscriberCount() int {
	if service == nil {
		return 0
	}
	service.invalidationMu.RLock()
	defer service.invalidationMu.RUnlock()
	return len(service.invalidations)
}
