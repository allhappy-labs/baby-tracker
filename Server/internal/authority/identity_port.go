package authority

import (
	"context"
	"errors"
	"fmt"
	"time"

	"babytracker/server/internal/identity"
)

// IdentityAuthority returns s as identity.Authority: the challenge,
// identity-resolution and Apple notification surface the identity package
// uses. (identity cannot import this package, which imports identity's key
// types.) Challenge errors become identity's codes; every other error is
// returned unchanged.
func (s *Service) IdentityAuthority() identity.Authority { return identityPort{s: s} }

type identityPort struct{ s *Service }

func (p identityPort) CreateChallenge(ctx context.Context, digest [32]byte, expiresAt, now time.Time) error {
	if err := p.s.CreateChallenge(ctx, digest, expiresAt, now); err != nil {
		return fmt.Errorf("%w: %w", identity.ErrUnavailable, err)
	}
	return nil
}

func (p identityPort) ConsumeChallenge(ctx context.Context, digest [32]byte, now time.Time) error {
	err := p.s.ConsumeChallenge(ctx, digest, now)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrInvalidOrExpiredChallenge):
		return identity.ErrInvalidOrExpiredChallenge
	default:
		return fmt.Errorf("%w: %w", identity.ErrUnavailable, err)
	}
}

func (p identityPort) ResolveIdentity(ctx context.Context, candidates []string) (identity.IdentityAuthority, error) {
	resolution, err := p.s.ResolveIdentity(ctx, candidates)
	if err != nil {
		return identity.IdentityAuthority{}, err
	}
	return identity.IdentityAuthority{Status: resolution.Status, AccountID: resolution.AccountID, IdentityKey: resolution.IdentityKey,
		LifecycleGeneration: resolution.LifecycleGeneration, NextActorGeneration: resolution.NextActorGeneration,
		ProfileRequired: resolution.ProfileRequired}, nil
}

func (p identityPort) ReserveAppleNotification(ctx context.Context, candidates []identity.KeyedDigest, commandID string, at time.Time) (string, bool, error) {
	return p.s.ReserveAppleNotification(ctx, candidates, commandID, at)
}

func (p identityPort) NotificationReceipt(ctx context.Context, accountID string, commandIDs []string, notificationDigest string) (bool, error) {
	return p.s.NotificationReceipt(ctx, accountID, commandIDs, notificationDigest)
}

func (p identityPort) MarkIdentityUnavailable(ctx context.Context, command identity.IdentityUnavailableCommand) error {
	_, err := p.s.MarkIdentityUnavailable(ctx, IdentityUnavailable{CommandID: command.CommandID, AccountID: command.AccountID,
		Candidates: command.Candidates, NotificationDigest: command.NotificationDigest, At: command.At})
	return err
}
