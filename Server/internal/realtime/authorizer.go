package realtime

import (
	"context"
	"errors"
	"time"

	"babytracker/server/internal/security"
)

type AuthorityAuthorizer struct {
	authority          *security.Service
	requireEntitlement bool
	now                func() time.Time
}

func NewAuthorityAuthorizer(authority *security.Service, requireEntitlement bool, now func() time.Time) (*AuthorityAuthorizer, error) {
	if authority == nil || now == nil {
		return nil, errors.New("live authority and clock are required")
	}
	return &AuthorityAuthorizer{authority: authority, requireEntitlement: requireEntitlement, now: now}, nil
}

func (authorizer *AuthorityAuthorizer) CheckMany(ctx context.Context, leases []AuthorizationLease) ([]bool, error) {
	if authorizer == nil || authorizer.authority == nil || authorizer.now == nil {
		return nil, errors.New("live authorizer unavailable")
	}
	allowed := make([]bool, len(leases))
	now := authorizer.now().UTC()
	err := authorizer.authority.WithAuthoritySnapshot(ctx, func(snapshot security.AuthoritySnapshot) error {
		for index, lease := range leases {
			principal, grant := lease.Principal, lease.Grant
			if principal.AccountID != grant.AccountID || principal.SessionFamilyID != grant.FamilyID || principal.Scope != "family" ||
				principal.SessionScope != "family" || principal.AccessExpiresAt.IsZero() || !now.Before(principal.AccessExpiresAt) ||
				principal.RecoveryEpoch != snapshot.RecoveryEpoch || grant.Revision == 0 || grant.Revision > snapshot.Head.Revision {
				continue
			}
			account, accountOK := snapshot.View.Account(principal.AccountID)
			device, deviceOK := snapshot.View.Device(principal.DeviceID)
			session, sessionOK := snapshot.View.Session(principal.SessionID)
			slot, slotOK := snapshot.View.FamilySlot(principal.AccountID)
			family, familyOK := snapshot.View.Family(grant.FamilyID)
			membership, memberOK := snapshot.View.Membership(grant.FamilyID, principal.AccountID)
			if !accountOK || account.Status != "active" || !deviceOK || device.Status != "active" || device.AccountID != principal.AccountID ||
				!sessionOK || session.Status != "active" || session.AccountID != principal.AccountID || session.DeviceID != principal.DeviceID ||
				session.RecoveryEpoch != principal.RecoveryEpoch || session.SecurityRevision != principal.SecurityRevision ||
				!slotOK || slot.FamilyID != grant.FamilyID || slot.State != "active" || !familyOK || family.Status != "active" ||
				!memberOK || membership.Status != "active" || membership.Role != grant.Role {
				continue
			}
			if authorizer.requireEntitlement {
				entitlement, ok := snapshot.View.Entitlement(grant.FamilyID, "family_sync")
				if !ok || entitlement.Status != "active" || entitlementExpired(entitlement.ExpiresAt, now) {
					continue
				}
			}
			allowed[index] = true
		}
		return nil
	})
	return allowed, err
}

func entitlementExpired(raw string, now time.Time) bool {
	if raw == "" {
		return false
	}
	deadline, err := time.Parse(time.RFC3339Nano, raw)
	return err != nil || !now.Before(deadline)
}
