package security

import (
	"babytracker/server/internal/diagnostics"
	"context"
	"time"
)

// AuthorityView is an expiring read capability. Every selected result is a
// detached value; the private owner never escapes as a writable State.
type AuthorityView struct {
	owner                 *validatedAuthority
	lease                 *fenceLease
	snapshotToken         *snapshotCapability
	referencesUnavailable bool
}

func (v AuthorityView) valid() bool { return v.owner != nil && v.lease != nil && v.lease.active.Load() }
func (v AuthorityView) Head() Head {
	if !v.valid() {
		return Head{}
	}
	return v.owner.head
}
func (v AuthorityView) RecoveryEpoch() uint64 {
	if !v.valid() {
		return 0
	}
	return v.owner.binding.Epoch
}
func (v AuthorityView) Account(id string) (AccountView, bool) {
	if !v.valid() {
		return AccountView{}, false
	}
	return v.owner.state.Account(id)
}
func (v AuthorityView) Device(id string) (DeviceView, bool) {
	if !v.valid() {
		return DeviceView{}, false
	}
	return v.owner.state.Device(id)
}
func (v AuthorityView) Session(id string) (SessionView, bool) {
	if !v.valid() {
		return SessionView{}, false
	}
	return v.owner.state.Session(id)
}
func (v AuthorityView) Family(id string) (FamilyView, bool) {
	if !v.valid() {
		return FamilyView{}, false
	}
	return v.owner.state.Family(id)
}
func (v AuthorityView) FamilySlot(id string) (FamilySlotView, bool) {
	if !v.valid() {
		return FamilySlotView{}, false
	}
	return v.owner.state.FamilySlot(id)
}
func (v AuthorityView) Membership(familyID, accountID string) (MembershipView, bool) {
	if !v.valid() {
		return MembershipView{}, false
	}
	return v.owner.state.Membership(familyID, accountID)
}
func (v AuthorityView) Invitation(id string) (InvitationView, bool) {
	if !v.valid() {
		return InvitationView{}, false
	}
	return v.owner.state.Invitation(id)
}
func (v AuthorityView) Entitlement(familyID, key string) (EntitlementView, bool) {
	if !v.valid() {
		return EntitlementView{}, false
	}
	return v.owner.state.Entitlement(familyID, key)
}
func (v AuthorityView) RecoveryClaim(familyID, accountID string) (RecoveryClaimView, bool) {
	if !v.valid() {
		return RecoveryClaimView{}, false
	}
	return v.owner.state.RecoveryClaim(familyID, accountID)
}
func (v AuthorityView) Receipt(accountID, commandID string) (ReceiptView, bool) {
	if !v.valid() {
		return ReceiptView{}, false
	}
	return v.owner.state.Receipt(accountID, commandID)
}
func (v AuthorityView) Memberships(familyID string) []MembershipView {
	if !v.valid() {
		return nil
	}
	return v.owner.state.Memberships(familyID)
}
func (v AuthorityView) ResolveIdentityAuthority(candidates []string) (IdentityAuthorityResolution, error) {
	if !v.valid() {
		return IdentityAuthorityResolution{}, ErrUnavailable
	}
	return v.owner.state.ResolveIdentityAuthority(candidates)
}
func (v AuthorityView) ReferencedCredentialKeyIDs() CredentialKeyInventory {
	if !v.valid() || v.referencesUnavailable {
		return CredentialKeyInventory{}
	}
	v.lease.mu.Lock()
	defer v.lease.mu.Unlock()
	if !v.valid() {
		return CredentialKeyInventory{}
	}
	if v.lease.credentialOwner == v.owner {
		if v.lease.credentialErr != nil {
			return CredentialKeyInventory{}
		}
		return cloneCredentialKeys(v.lease.credentialKeys)
	}
	// A capability retained across a local write still names its prior owner.
	// Its recovery union must not fall back to authority-only keys.
	v.owner.credentialMu.Lock()
	defer v.owner.credentialMu.Unlock()
	if v.owner.credentialResult != nil {
		return cloneCredentialKeys(v.owner.credentialResult.Inventory)
	}
	return cloneCredentialKeys(v.owner.keys)
}

func (service *Service) WithAuthorityView(ctx context.Context, run func(AuthorityView) error) error {
	start := time.Now()
	callerCtx := ctx
	nested := service.hasFenceMarker(ctx) || service.callbackContextDenied(ctx) || service.hasExpiredFenceLease(ctx)
	var release func()
	defer func() {
		elapsed := time.Since(start)
		if release != nil {
			release()
		}
		if !nested {
			diagnostics.Record(callerCtx, nil, diagnostics.Authority, elapsed)
		}
	}()
	if run == nil || service == nil || service.fence == nil {
		return ErrUnavailable
	}
	if internal, done, ok := service.borrowDecisionRead(ctx); ok {
		defer done()
		ctx = internal
	} else if err := service.rejectCallbackEntry(ctx); err != nil {
		return err
	}
	if service.hasExpiredFenceLease(ctx) {
		return ErrUnavailable
	}
	lease := service.currentFenceLease(ctx)
	if lease == nil {
		unlock, err := service.fence.Read(ctx)
		if err != nil {
			return errorCode("security_busy")
		}
		ctx, lease = service.withFenceLease(ctx)
		release = func() { lease.invalidate(); unlock() }
	}
	owner, err := service.loadValidatedAuthority(ctx)
	if err != nil {
		return err
	}
	return run(AuthorityView{owner: owner, lease: lease})
}

func (v AuthorityView) MembershipGeneration(familyID, accountID string) uint64 {
	if !v.valid() {
		return 0
	}
	return v.owner.state.MembershipGeneration(familyID, accountID)
}
