package security

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"babytracker/server/internal/diagnostics"
)

// AuthorityBinding is loaded from persistent recovery identity under the fence.
// An absent Binding adapter deliberately disables cross-lease acceleration.
type AuthorityBinding struct {
	InstanceID  string
	Epoch       uint64
	TrustedHead Head
}

// validatedAuthority owns immutable maps and retained trusted inventory. Neither
// it nor its State is returned to callers. A lease authorizes access, not a cache.
type validatedAuthority struct {
	state            State
	head             Head
	binding          AuthorityBinding
	inventory        CommittedInventory
	keys             CredentialKeyInventory
	credentialMu     sync.Mutex
	credentialResult *RecoveryKeyReferencesResult
}

func validAuthorityBinding(b AuthorityBinding) bool {
	return validUUID(b.InstanceID) && b.Epoch > 0 && b.Epoch <= maxWireInteger && b.TrustedHead.Generation == 1 &&
		b.TrustedHead.Revision <= maxWireInteger && (b.TrustedHead.Revision == 0) == (b.TrustedHead.EventHash == [32]byte{})
}

func (service *Service) authorityBinding(ctx context.Context) (AuthorityBinding, error) {
	if service.binding != nil {
		b, err := service.binding(ctx)
		if err != nil {
			return AuthorityBinding{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
		if !validAuthorityBinding(b) {
			return AuthorityBinding{}, ErrUnavailable
		}
		return b, nil
	}
	epoch, err := service.epoch(ctx)
	if err != nil {
		return AuthorityBinding{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if epoch == 0 || epoch > maxWireInteger {
		return AuthorityBinding{}, ErrUnavailable
	}
	return AuthorityBinding{Epoch: epoch, TrustedHead: Head{Generation: 1}}, nil
}

// authorityMaterial verifies the durable proof without repairing projection.
// Successful material verification retains monotonic proof even if later
// projection/quarantine checks deny usable lease publication. cacheMu serializes
// proof retention with verification; the existing filesystem fence remains the
// authority serialization boundary across processes.
func (service *Service) authorityMaterial(ctx context.Context) (*validatedAuthority, error) {
	b, err := service.authorityBinding(ctx)
	if err != nil {
		return nil, err
	}
	service.cacheMu.Lock()
	defer service.cacheMu.Unlock()
	return service.authorityMaterialLocked(ctx, b)
}

func (service *Service) authorityMaterialLocked(ctx context.Context, b AuthorityBinding) (*validatedAuthority, error) {
	old := service.authorityProof
	if service.binding != nil && old != nil && (old.binding.InstanceID != b.InstanceID || old.binding.Epoch != b.Epoch) {
		service.cacheBlocked = true
	}
	if service.binding != nil && old != nil {
		prior := old.binding.TrustedHead
		if b.TrustedHead.Revision < prior.Revision || (b.TrustedHead.Revision == prior.Revision && b.TrustedHead != prior) {
			return nil, ErrTrustedHeadRequired
		}
	}
	if service.cacheBlocked {
		return nil, ErrUnavailable
	}
	head, err := service.heads.Load(ctx)
	if err != nil {
		return nil, err
	}
	if old != nil && (head.Generation != old.head.Generation || head.Revision < old.head.Revision || (head.Revision == old.head.Revision && head != old.head)) {
		return nil, ErrTrustedHeadRequired
	}
	floor := service.authorityFloor
	if floor.Generation != 0 && (head.Generation != floor.Generation || head.Revision < floor.Revision || (head.Revision == floor.Revision && head != floor)) {
		return nil, ErrTrustedHeadRequired
	}
	ledger, optimized := service.ledger.(IntegrityLedger)
	if !optimized || service.binding == nil {
		// Unsupported/unbound adapters retain complete strict chain/event validation.
		trusted := b.TrustedHead
		if old != nil && old.head.Revision > trusted.Revision {
			trusted = old.head
		}
		if floor.Revision > trusted.Revision {
			trusted = floor
		}
		integrityStart := diagnostics.Start(ctx)
		verified, state, err := VerifyCommittedChain(ctx, service.ledger, service.heads, trusted)
		diagnostics.CaptureSince(ctx, diagnostics.IntegrityInventory, integrityStart)
		if err != nil {
			return nil, err
		}
		if b.TrustedHead != trusted {
			integrityStart = diagnostics.Start(ctx)
			_, _, verifyErr := VerifyCommittedChain(ctx, service.ledger, service.heads, b.TrustedHead)
			diagnostics.CaptureSince(ctx, diagnostics.IntegrityInventory, integrityStart)
			if err := verifyErr; err != nil {
				return nil, err
			}
		}
		return service.retainAuthorityProofLocked(&validatedAuthority{state: state, head: verified, binding: b, keys: state.ReferencedCredentialKeyIDs()}), nil
	}
	var inv CommittedInventory
	var candidates []Candidate
	var state State
	if old == nil || old.inventory.Head != old.head {
		start := diagnostics.Start(ctx)
		inv, candidates, err = ledger.ReadValidatedInventory(ctx, head)
		diagnostics.CaptureSince(ctx, diagnostics.IntegrityInventory, start)
		state = NewState()
	} else if head == old.head {
		start := diagnostics.Start(ctx)
		err = ledger.VerifyInventory(ctx, old.inventory)
		diagnostics.CaptureSince(ctx, diagnostics.IntegrityVerify, start)
		inv = old.inventory
		state = old.state
	} else {
		start := diagnostics.Start(ctx)
		inv, candidates, err = ledger.ReadValidatedExtension(ctx, old.inventory, head)
		diagnostics.CaptureSince(ctx, diagnostics.IntegrityExtension, start)
		// A successor never changes a published owner's maps.
		if err == nil {
			state = old.state.Clone()
		}
	}
	if err != nil {
		return nil, err
	}
	for _, candidate := range candidates {
		if err := validateCandidate(state.Head(), candidate); err != nil {
			return nil, err
		}
		var event Event
		if err := decodeEventStrict(candidate.EventJSON, &event); err != nil {
			return nil, ErrLedgerCorrupt
		}
		if err := state.apply(event, candidate.EventHash); err != nil {
			return nil, err
		}
	}
	if state.Head() != head || inv.Head != head {
		return nil, ErrLedgerCorrupt
	}
	if !inventoryContains(inv, b.TrustedHead) || (old != nil && !inventoryContains(inv, old.head)) || (floor.Generation != 0 && !inventoryContains(inv, floor)) {
		return nil, ErrTrustedHeadRequired
	}
	if old != nil && old.inventory.Head == old.head && head == old.head && b == old.binding {
		return service.retainAuthorityProofLocked(old), nil
	}
	return service.retainAuthorityProofLocked(&validatedAuthority{state: state, head: head, binding: b, inventory: inv, keys: state.ReferencedCredentialKeyIDs()}), nil
}

// retainAuthorityProofLocked is reached only after complete durable chain,
// inventory, ancestry and binding validation. Existing high-water comparisons
// make this monotonic; ReconcileTrusted is the separate explicit reset path.
// Retaining proof never authorizes a view: loadValidatedAuthority must still
// verify projection/quarantine before assigning the lease's usable authority.
func (service *Service) retainAuthorityProofLocked(owner *validatedAuthority) *validatedAuthority {
	service.authorityProof = owner
	service.authorityFloor = owner.head
	return owner
}

func inventoryContains(inv CommittedInventory, head Head) bool {
	if head.Generation != inv.Head.Generation || head.Revision > inv.Head.Revision {
		return false
	}
	if head.Revision == 0 {
		return head.EventHash == [32]byte{}
	}
	if head.Revision == inv.Head.Revision {
		return head == inv.Head
	}
	if head.Revision > uint64(len(inv.Files)) {
		return false
	}
	file := inv.Files[head.Revision-1]
	return file.Revision == head.Revision && file.EventHash == head.EventHash
}

func (service *Service) loadValidatedAuthority(ctx context.Context) (*validatedAuthority, error) {
	lease := service.currentFenceLease(ctx)
	if lease == nil {
		return nil, ErrUnavailable
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if !lease.active.Load() {
		return nil, ErrUnavailable
	}
	if lease.authority != nil {
		return lease.authority, nil
	}
	// Binding is a composition callback: invoke it outside the cache mutex.
	var binding AuthorityBinding
	if lease.material == nil {
		var err error
		binding, err = service.authorityBinding(ctx)
		if err != nil {
			return nil, err
		}
	}
	// Serialize extension construction through publication so concurrent first
	// readers cannot each clone the same successor state.
	service.cacheMu.Lock()
	owner := lease.material
	var err error
	if owner == nil {
		owner, err = service.authorityMaterialLocked(ctx, binding)
	}
	service.cacheMu.Unlock()
	if err != nil {
		return nil, err
	}
	projected, err := service.projection.Head(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if projected != owner.head {
		return nil, ErrUnavailable
	}
	service.mu.RLock()
	quarantine := service.quarantine
	service.mu.RUnlock()
	if quarantine {
		return nil, ErrUnavailable
	}
	if err := service.loadCredentialInventory(ctx, owner, lease); err != nil {
		return nil, err
	}
	// Only this assignment authorizes reuse inside the active lease. Durable
	// proof was retained earlier, independently of these usability checks.
	lease.authority = owner
	return owner, nil
}

// Advance rollback evidence immediately after durable CAS, including projection
// failures. Task 4 publishes eager successor state; this invalidation floor
// ensures the older immutable owner can never authorize a rolled-back commit.
func (service *Service) recordAuthorityCommit(head Head) {
	service.cacheMu.Lock()
	service.authorityFloor = head
	service.cacheMu.Unlock()
}

// RecoveryKeyReferencesResult is provider-owned eligibility metadata. A zero
// deadline has no known time boundary; uncertainty must set Cacheable=false.
type RecoveryKeyReferencesResult struct {
	Inventory    CredentialKeyInventory
	RevalidateAt time.Time
	Cacheable    bool
}

// loadCredentialInventory runs after projection validation, with lease.mu held.
// The owner mutex coalesces concurrent first readers; no global cache mutex is
// held while calling the provider. The provider may only inspect the supplied
// view, never reenter authority or key inventory acquisition.
func (service *Service) loadCredentialInventory(ctx context.Context, owner *validatedAuthority, lease *fenceLease) error {
	if lease.credentialOwner == owner {
		return lease.credentialErr
	}
	lease.credentialOwner = owner
	lease.credentialKeys = CredentialKeyInventory{}
	lease.credentialErr = ErrUnavailable
	owner.credentialMu.Lock()
	defer owner.credentialMu.Unlock()
	result := owner.credentialResult
	if result == nil || !result.Cacheable || (!result.RevalidateAt.IsZero() && !service.now().Before(result.RevalidateAt)) {
		fresh := RecoveryKeyReferencesResult{Cacheable: true}
		if service.recoveryKeyReferences != nil {
			var err error
			fresh, err = service.recoveryKeyReferences(service.withCallbackDenial(ctx), AuthorityView{owner: owner, lease: lease, referencesUnavailable: true})
			if err != nil {
				return err
			}
		}
		fresh.Inventory = unionCredentialKeys(owner.keys, fresh.Inventory)
		result = &fresh
		owner.credentialResult = result
	}
	lease.credentialKeys = cloneCredentialKeys(result.Inventory)
	lease.credentialErr = nil
	return nil
}

func cloneCredentialKeys(k CredentialKeyInventory) CredentialKeyInventory {
	return CredentialKeyInventory{SessionToken: slices.Clone(k.SessionToken), Identity: slices.Clone(k.Identity), DeviceContinuity: slices.Clone(k.DeviceContinuity), Invitation: slices.Clone(k.Invitation)}
}
func unionCredentialKeys(a, b CredentialKeyInventory) CredentialKeyInventory {
	union := func(x, y []string) []string {
		z := append(slices.Clone(x), y...)
		slices.Sort(z)
		return slices.Compact(z)
	}
	return CredentialKeyInventory{SessionToken: union(a.SessionToken, b.SessionToken), Identity: union(a.Identity, b.Identity), DeviceContinuity: union(a.DeviceContinuity, b.DeviceContinuity), Invitation: union(a.Invitation, b.Invitation)}
}

func (service *Service) ReferencedCredentialKeyIDs(ctx context.Context) (CredentialKeyInventory, error) {
	var keys CredentialKeyInventory
	err := service.WithAuthorityView(ctx, func(view AuthorityView) error { keys = view.ReferencedCredentialKeyIDs(); return nil })
	return keys, err
}
