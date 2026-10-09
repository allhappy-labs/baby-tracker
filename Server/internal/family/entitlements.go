package family

import (
	"context"
	"time"

	"babytracker/server/internal/security"
)

type EntitlementPolicy interface {
	Check(context.Context, string, string, time.Time) error
}

type SelfHostedEntitlementPolicy struct{}

func (SelfHostedEntitlementPolicy) Check(_ context.Context, _ string, key string, _ time.Time) error {
	if key != "family_sync" {
		return ErrEntitlementRequired
	}
	return nil
}

type HostedEntitlementPolicy struct{ Security *security.Service }

func (policy HostedEntitlementPolicy) Check(ctx context.Context, familyID, key string, now time.Time) error {
	if policy.Security == nil || key != "family_sync" {
		return ErrEntitlementRequired
	}
	var entitlement security.EntitlementView
	var ok bool
	err := policy.Security.WithAuthorityView(ctx, func(view security.AuthorityView) error {
		entitlement, ok = view.Entitlement(familyID, key)
		return nil
	})
	if err != nil {
		return familyError(err)
	}
	if !ok || entitlement.Status != "active" {
		return ErrEntitlementRequired
	}
	if entitlement.ExpiresAt != "" {
		expiresAt, err := time.Parse(time.RFC3339Nano, entitlement.ExpiresAt)
		if err != nil {
			return ErrUnavailable
		}
		if !now.UTC().Before(expiresAt) {
			return ErrEntitlementRequired
		}
	}
	return nil
}

type hostedBootstrapPolicy interface{ hostedBootstrap() bool }

func (HostedEntitlementPolicy) hostedBootstrap() bool { return true }
