package realtime

import (
	"context"
	"errors"

	"babytracker/server/internal/authority"
)

// AuthorityAuthorizer re-checks live leases against the PostgreSQL authority
// (authority.Service.CheckMany): one read-only transaction and one statement
// for the whole batch. The family_sync entitlement is the authority's own
// rule (node flag or family flag), so there is nothing to configure here.
type AuthorityAuthorizer struct {
	authority *authority.Service
}

func NewAuthorityAuthorizer(a *authority.Service) (*AuthorityAuthorizer, error) {
	if a == nil {
		return nil, errors.New("live authority is required")
	}
	return &AuthorityAuthorizer{authority: a}, nil
}

func (authorizer *AuthorityAuthorizer) CheckMany(ctx context.Context, leases []AuthorizationLease) ([]bool, error) {
	if authorizer == nil || authorizer.authority == nil {
		return nil, errors.New("live authorizer unavailable")
	}
	return authorizer.authority.CheckMany(ctx, leases)
}
