package liveactivity

import (
	"context"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

// Service registers devices and cards, each call one request transaction on
// the authority's request seam. The request's Admit has already refused a
// principal that is not a family session (the handler's), and family policy
// (action "read") has produced the grant from the session row.
type Service struct {
	guard      authority.RequestGuard
	repository *Repository
}

// NewService registers under topic, the app's bundle ID +
// ".push-type.liveactivity" (main's composition).
func NewService(guard authority.RequestGuard, topic string) *Service {
	return &Service{guard: guard, repository: NewRepository(topic)}
}

func (s *Service) transact(ctx context.Context, req authority.Request, run func(pgx.Tx, access.Principal, access.Grant) error) error {
	if s == nil || s.guard == nil {
		return ErrRegistrationDenied
	}
	// Registration needs reading authority only, including viewers. It conveys no mutation authority.
	return s.guard.WithRequestTx(ctx, req, "read", func(tx pgx.Tx, p access.Principal, g access.Grant) error {
		if p.SessionFamilyID != g.FamilyID {
			return ErrRegistrationDenied
		}
		return run(tx, p, g)
	})
}

// RegisterDevice registers the session's device (Repository.RegisterDevice,
// which validates body inside the transaction: the request's held refusals,
// in Admit, come first).
func (s *Service) RegisterDevice(ctx context.Context, req authority.Request, body protocol.LiveActivityDeviceRegistration) (result protocol.LiveActivityRegistrationReceipt, err error) {
	err = s.transact(ctx, req, func(tx pgx.Tx, p access.Principal, g access.Grant) error {
		result, err = s.repository.RegisterDevice(ctx, tx, p, g, body)
		return err
	})
	return
}

// RegisterActivity registers a card's update token (Repository.RegisterActivity,
// which validates body inside the transaction).
func (s *Service) RegisterActivity(ctx context.Context, req authority.Request, body protocol.LiveActivityTokenRegistration) (result protocol.LiveActivityRegistrationReceipt, err error) {
	err = s.transact(ctx, req, func(tx pgx.Tx, p access.Principal, g access.Grant) error {
		result, err = s.repository.RegisterActivity(ctx, tx, p, g, body)
		return err
	})
	return
}
func (s *Service) UnregisterDevice(ctx context.Context, req authority.Request, installation string, version uint64) error {
	return s.transact(ctx, req, func(tx pgx.Tx, p access.Principal, _ access.Grant) error {
		return s.repository.UnregisterDevice(ctx, tx, p, installation, version)
	})
}

// Unregistration is the removal path on its own. It needs no worker, APNs key,
// topic or push configuration, so a server whose push is disabled or whose
// worker is stopped still honours a phone's DELETE — with the authentication,
// authorization and version idempotency of Service.UnregisterDevice. It can
// register nothing: its repository has no topic.
type Unregistration struct{ service *Service }

func NewUnregistration(guard authority.RequestGuard) *Unregistration {
	return &Unregistration{service: &Service{guard: guard, repository: NewRepository("")}}
}

func (u *Unregistration) UnregisterDevice(ctx context.Context, req authority.Request, installation string, version uint64) error {
	if u == nil {
		return ErrRegistrationDenied
	}
	return u.service.UnregisterDevice(ctx, req, installation, version)
}
