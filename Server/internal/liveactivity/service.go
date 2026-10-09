package liveactivity

import (
	"context"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

type Authority interface {
	WithAuthorizedTransaction(context.Context, access.Principal, string, string, func(pgx.Tx, access.Grant) error) error
}
type FamilyResolver interface {
	Current(context.Context, access.Principal) (string, error)
}
type Service struct {
	authority  Authority
	resolver   FamilyResolver
	repository *Repository
}

func NewService(authority Authority, resolver FamilyResolver, topic string) *Service {
	return &Service{authority: authority, resolver: resolver, repository: NewRepository(topic)}
}
func (s *Service) transact(ctx context.Context, p access.Principal, run func(pgx.Tx, access.Grant) error) error {
	if s == nil || s.authority == nil || s.resolver == nil || p.Scope != "family" || p.SessionScope != "family" || !protocol.ValidUUID(p.SessionFamilyID) {
		return ErrRegistrationDenied
	}
	family, err := s.resolver.Current(ctx, p)
	if err != nil {
		return err
	}
	if family != p.SessionFamilyID {
		return ErrRegistrationDenied
	}
	// Registration needs reading authority only, including viewers. It conveys no mutation authority.
	return s.authority.WithAuthorizedTransaction(ctx, p, family, "read", run)
}
func (s *Service) RegisterDevice(ctx context.Context, p access.Principal, req protocol.LiveActivityDeviceRegistration) (result protocol.LiveActivityRegistrationReceipt, err error) {
	if err = req.Validate(); err != nil {
		return
	}
	err = s.transact(ctx, p, func(tx pgx.Tx, g access.Grant) error {
		result, err = s.repository.RegisterDevice(ctx, tx, p, g, req)
		return err
	})
	return
}
func (s *Service) RegisterActivity(ctx context.Context, p access.Principal, req protocol.LiveActivityTokenRegistration) (result protocol.LiveActivityRegistrationReceipt, err error) {
	if err = req.Validate(); err != nil {
		return
	}
	err = s.transact(ctx, p, func(tx pgx.Tx, g access.Grant) error {
		result, err = s.repository.RegisterActivity(ctx, tx, p, g, req)
		return err
	})
	return
}
func (s *Service) UnregisterDevice(ctx context.Context, p access.Principal, installation string, version uint64) error {
	return s.transact(ctx, p, func(tx pgx.Tx, g access.Grant) error {
		return s.repository.UnregisterDevice(ctx, tx, p, installation, version)
	})
}

// Unregistration is the removal path on its own. It needs no worker, APNs key,
// topic or push configuration, so a server whose push is disabled or whose
// worker is stopped still honours a phone's DELETE — with the authentication,
// authorization and version idempotency of Service.UnregisterDevice. It can
// register nothing: its repository has no topic.
type Unregistration struct{ service *Service }

func NewUnregistration(authority Authority, resolver FamilyResolver) *Unregistration {
	return &Unregistration{service: &Service{authority: authority, resolver: resolver, repository: NewRepository("")}}
}

func (u *Unregistration) UnregisterDevice(ctx context.Context, p access.Principal, installation string, version uint64) error {
	if u == nil {
		return ErrRegistrationDenied
	}
	return u.service.UnregisterDevice(ctx, p, installation, version)
}
