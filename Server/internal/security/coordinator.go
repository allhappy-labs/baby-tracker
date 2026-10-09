package security

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/diagnostics"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	ledger                SecurityLedger
	heads                 LedgerHeadStore
	projection            Projection
	fence                 Fence
	now                   func() time.Time
	runtime               *pgxpool.Pool
	decide                Decision
	epoch                 func(context.Context) (uint64, error)
	binding               func(context.Context) (AuthorityBinding, error)
	recoveryKeyReferences func(context.Context, AuthorityView) (RecoveryKeyReferencesResult, error)
	cacheMu               sync.Mutex
	authorityProof        *validatedAuthority
	cacheBlocked          bool
	authorityFloor        Head
	familyPolicy          FamilyPolicy
	mu                    sync.RWMutex
	quarantine            bool
	snapshotID            atomic.Uint64
	invalidationMu        sync.RWMutex
	invalidationID        atomic.Uint64
	invalidations         map[uint64]chan SecurityInvalidation
}

func (service *Service) WithAuthoritySnapshot(ctx context.Context, run func(AuthoritySnapshot) error) error {
	start := time.Now()
	callerCtx := ctx
	var releaseSnapshot func()
	// A caller already inside our fence or a denied decision callback cannot
	// safely invoke an external recorder until its outer operation releases.
	nestedLease := service.hasFenceMarker(ctx) || service.callbackContextDenied(ctx)
	defer func() {
		elapsed := time.Since(start)
		if releaseSnapshot != nil {
			releaseSnapshot()
		}
		// The outer guarded operation owns this lease and records its boundary.
		if !nestedLease {
			diagnostics.Record(callerCtx, nil, diagnostics.Authority, elapsed)
		}
	}()
	if err := service.rejectCallbackEntry(ctx); err != nil {
		return err
	}
	if service == nil || service.fence == nil || run == nil {
		return errorCode("service_unavailable")
	}
	if service.hasFenceMarker(ctx) {
		snapshot, err := service.authoritySnapshotLocked(ctx)
		if err != nil {
			return err
		}
		return run(snapshot)
	}
	release, err := service.fence.Read(ctx)
	if err != nil {
		return errorCode("security_busy")
	}
	ctx, lease := service.withFenceLease(ctx)
	releaseSnapshot = func() {
		lease.invalidate()
		release()
	}
	snapshot, err := service.authoritySnapshotLocked(ctx)
	if err != nil {
		return err
	}
	return run(snapshot)
}

func (service *Service) ExecutePrepared(ctx context.Context, prepare PrepareCommand) (CommandResult, error) {
	return service.executePrepared(ctx, nil, prepare)
}

func (service *Service) executePrepared(ctx context.Context, operatorGuard OperatorAuthorityGuard, prepare PrepareCommand) (CommandResult, error) {
	if err := service.rejectCallbackEntry(ctx); err != nil {
		return CommandResult{}, err
	}
	if service == nil || service.ledger == nil || service.heads == nil || service.projection == nil || service.fence == nil || service.epoch == nil || prepare == nil {
		return CommandResult{}, errorCode("service_unavailable")
	}
	callbackCtx := service.withCallbackDenial(ctx)
	release, err := service.fence.Write(ctx)
	if err != nil {
		return CommandResult{}, errorCode("security_busy")
	}
	defer release()
	ctx, lease := service.withFenceLease(ctx)
	defer lease.invalidate()
	head, state, err := service.loadState(ctx)
	if err != nil {
		return CommandResult{}, err
	}
	if operatorGuard != nil {
		if err := operatorGuard.ValidateOperatorAuthorityUnderFence(ctx, head); err != nil {
			service.setQuarantine(true)
			return CommandResult{}, errorCode("service_unavailable")
		}
	}
	if err := service.applyProjection(ctx, head); err != nil {
		service.setQuarantine(true)
		return CommandResult{}, errorCode("service_unavailable")
	}
	service.setQuarantine(false)
	epoch, err := service.epoch(ctx)
	if err != nil || epoch == 0 || epoch > maxWireInteger {
		return CommandResult{}, errorCode("service_unavailable")
	}
	if service.binding == nil {
		lease.material = &validatedAuthority{state: state, head: head, binding: AuthorityBinding{Epoch: epoch, TrustedHead: Head{Generation: 1}}, keys: state.ReferencedCredentialKeyIDs()}
	}
	owner, err := service.loadValidatedAuthority(ctx)
	if err != nil {
		return CommandResult{}, err
	}
	if owner.binding.Epoch != epoch {
		return CommandResult{}, ErrUnavailable
	}
	snapshot := newLeasedAuthoritySnapshot(owner, lease, service.snapshotID.Add(1))
	prepared, err := prepare(callbackCtx, snapshot)
	if err != nil {
		return CommandResult{}, err
	}
	if !snapshot.validPrepared(prepared) {
		return CommandResult{}, errorCode("invalid_command")
	}
	event, result, err := state.decidePrepared(prepared)
	if err != nil {
		return CommandResult{}, err
	}
	if event.Revision == 0 {
		return result, nil
	}
	encoded, err := EncodeEvent(event)
	if err != nil {
		return CommandResult{}, err
	}
	candidate := Candidate{CommandID: prepared.command.ID, Prior: head, EventJSON: encoded, EventHash: sha256.Sum256(encoded)}
	if err := service.ledger.AppendCandidate(ctx, candidate); err != nil {
		return CommandResult{}, err
	}
	if err := recoveryFault(ctx, "before_authority_commit"); err != nil {
		return CommandResult{}, err
	}
	next := Head{Generation: event.Generation, Revision: event.Revision, EventHash: candidate.EventHash}
	swapped, err := service.heads.CompareAndSwap(ctx, head, next)
	if err != nil {
		return CommandResult{}, err
	}
	if !swapped {
		return CommandResult{}, errorCode("security_busy")
	}
	service.recordAuthorityCommit(next)
	if err := recoveryFault(ctx, "after_authority_commit"); err != nil {
		return CommandResult{}, err
	}
	service.publishInvalidations(event)
	if err := service.projection.Apply(ctx, candidate); err != nil {
		service.setQuarantine(true)
		return CommandResult{}, errorCode("service_unavailable")
	}
	projectionHead, err := service.projection.Head(ctx)
	if err != nil || projectionHead != next {
		service.setQuarantine(true)
		return CommandResult{}, errorCode("service_unavailable")
	}
	if err := service.publishAuthoritySuccessor(ctx, owner, candidate, next); err != nil {
		service.setQuarantine(true)
		return CommandResult{}, err
	}
	service.setQuarantine(false)
	service.publishInvalidations(event)
	if err := recoveryFault(ctx, "after_projection"); err != nil {
		return CommandResult{}, err
	}
	return result, nil
}

func (service *Service) authoritySnapshotLocked(ctx context.Context) (AuthoritySnapshot, error) {
	owner, err := service.loadValidatedAuthority(ctx)
	if err != nil {
		return AuthoritySnapshot{}, err
	}
	return newLeasedAuthoritySnapshot(owner, service.currentFenceLease(ctx), service.snapshotID.Add(1)), nil
}

type Options struct {
	RecoveryKeyReferences        func(context.Context, AuthorityView) (RecoveryKeyReferencesResult, error)
	Ledger                       SecurityLedger
	Heads                        LedgerHeadStore
	Projection                   Projection
	Fence                        Fence
	Now                          func() time.Time
	Runtime                      *pgxpool.Pool
	Decide                       Decision
	Epoch                        func(context.Context) (uint64, error)
	Binding                      func(context.Context) (AuthorityBinding, error)
	MaxCaregivers                int
	MaxInvitations               int
	RequireFamilySyncEntitlement bool
}

func New(options Options) (*Service, error) {
	if options.Ledger == nil || options.Heads == nil || options.Projection == nil || options.Fence == nil || options.Now == nil ||
		options.Runtime == nil || options.Decide == nil || options.Epoch == nil {
		return nil, errors.New("security service requires ledger, heads, projection, fence, now, runtime, decision, and epoch")
	}
	maxCaregivers, maxInvitations := options.MaxCaregivers, options.MaxInvitations
	if maxCaregivers == 0 {
		maxCaregivers = 10
	}
	if maxInvitations == 0 {
		maxInvitations = 10
	}
	if maxCaregivers < 1 || maxInvitations < 1 {
		return nil, errors.New("security service family limits must be positive")
	}
	return &Service{ledger: options.Ledger, heads: options.Heads, projection: options.Projection, fence: options.Fence, now: options.Now,
		runtime: options.Runtime, decide: options.Decide, epoch: options.Epoch, binding: options.Binding, recoveryKeyReferences: options.RecoveryKeyReferences,
		invalidations: make(map[uint64]chan SecurityInvalidation),
		familyPolicy: FamilyPolicy{MaxCaregivers: maxCaregivers, MaxInvitations: maxInvitations,
			RequireFamilySyncEntitlement: options.RequireFamilySyncEntitlement}}, nil
}

func (service *Service) Execute(ctx context.Context, principal access.Principal, command Command) (CommandResult, error) {
	if err := service.rejectCallbackEntry(ctx); err != nil {
		return CommandResult{}, err
	}
	if service == nil || service.ledger == nil || service.heads == nil || service.projection == nil || service.fence == nil || service.epoch == nil {
		return CommandResult{}, errorCode("service_unavailable")
	}
	release, err := service.fence.Write(ctx)
	if err != nil {
		return CommandResult{}, errorCode("security_busy")
	}
	defer release()
	ctx, lease := service.withFenceLease(ctx)
	defer lease.invalidate()

	for attempt := 0; attempt < 8; attempt++ {
		epoch, err := service.epoch(ctx)
		if err != nil {
			return CommandResult{}, errorCode("service_unavailable")
		}
		if principal.RecoveryEpoch == 0 || principal.RecoveryEpoch != epoch {
			return CommandResult{}, errorCode("epoch_changed")
		}
		head, state, err := service.loadStateAtEpoch(ctx, epoch)
		if err != nil {
			return CommandResult{}, err
		}
		if service.binding != nil && lease.material.binding.Epoch != epoch {
			return CommandResult{}, errorCode("epoch_changed")
		}
		if err := service.applyProjection(ctx, head); err != nil {
			service.setQuarantine(true)
			return CommandResult{}, errorCode("service_unavailable")
		}
		service.setQuarantine(false)
		owner := lease.material
		if owner == nil {
			owner = &validatedAuthority{state: state, head: head, binding: AuthorityBinding{Epoch: epoch, TrustedHead: Head{Generation: 1}}}
		}
		event, result, err := state.decide(principal, command, service.now(), service.familyPolicy)
		if err != nil {
			return CommandResult{}, err
		}
		if event.Revision == 0 {
			return result, nil
		}
		encoded, err := EncodeEvent(event)
		if err != nil {
			return CommandResult{}, err
		}
		candidate := Candidate{CommandID: command.ID, Prior: head, EventJSON: encoded, EventHash: sha256.Sum256(encoded)}
		if err := service.ledger.AppendCandidate(ctx, candidate); err != nil {
			return CommandResult{}, err
		}
		next := Head{Generation: event.Generation, Revision: event.Revision, EventHash: candidate.EventHash}
		swapped, err := service.heads.CompareAndSwap(ctx, head, next)
		if err != nil {
			return CommandResult{}, err
		}
		if !swapped {
			continue
		}
		service.recordAuthorityCommit(next)
		service.publishInvalidations(event)
		if err := service.projection.Apply(ctx, candidate); err != nil {
			service.setQuarantine(true)
			return CommandResult{}, errorCode("service_unavailable")
		}
		projectionHead, err := service.projection.Head(ctx)
		if err != nil || projectionHead != next {
			service.setQuarantine(true)
			return CommandResult{}, errorCode("service_unavailable")
		}
		if err := service.publishAuthoritySuccessor(ctx, owner, candidate, next); err != nil {
			service.setQuarantine(true)
			return CommandResult{}, err
		}
		service.setQuarantine(false)
		service.publishInvalidations(event)
		return result, nil
	}
	return CommandResult{}, errorCode("security_busy")
}

func (service *Service) Reconcile(ctx context.Context) error {
	if err := service.rejectCallbackEntry(ctx); err != nil {
		return err
	}
	_, err := service.ReconcileTrusted(ctx, Head{Generation: 1})
	return err
}

// ReconcileTrusted validates the entire chain and trusted ancestry under the
// exclusive authority fence before applying any projection side effect.
func (service *Service) ReconcileTrusted(ctx context.Context, trusted Head) (Head, error) {
	if err := service.rejectCallbackEntry(ctx); err != nil {
		return Head{}, err
	}
	if service == nil || service.ledger == nil || service.heads == nil || service.projection == nil || service.fence == nil {
		return Head{}, errorCode("service_unavailable")
	}
	release, err := service.fence.Write(ctx)
	if err != nil {
		return Head{}, errorCode("security_busy")
	}
	defer release()
	head, state, err := VerifyCommittedChain(ctx, service.ledger, service.heads, trusted)
	if err != nil {
		return Head{}, err
	}
	if err := service.applyProjection(ctx, head); err != nil {
		service.setQuarantine(true)
		return Head{}, errorCode("service_unavailable")
	}
	binding, err := service.authorityBinding(ctx)
	if err != nil {
		return Head{}, err
	}
	if _, _, err := VerifyCommittedChain(ctx, service.ledger, service.heads, binding.TrustedHead); err != nil {
		return Head{}, err
	}
	service.cacheMu.Lock()
	service.authorityProof = &validatedAuthority{state: state, head: head, binding: binding, keys: state.ReferencedCredentialKeyIDs()}
	service.cacheBlocked = false
	service.authorityFloor = head
	service.cacheMu.Unlock()
	service.setQuarantine(false)
	return head, nil
}

func (service *Service) Check(ctx context.Context) error {
	if err := service.rejectCallbackEntry(ctx); err != nil {
		return err
	}
	if service == nil || service.fence == nil {
		return errorCode("service_unavailable")
	}
	if service.hasFenceMarker(ctx) {
		return service.checkLocked(ctx)
	}
	release, err := service.fence.Read(ctx)
	if err != nil {
		return errorCode("security_busy")
	}
	defer release()
	ctx, lease := service.withFenceLease(ctx)
	defer lease.invalidate()
	return service.checkLocked(ctx)
}

func (service *Service) checkLocked(ctx context.Context) error {
	_, err := service.loadValidatedAuthority(ctx)
	return err
}

// ReadState returns a detached full copy for explicit diagnostic compatibility.
// Production authorization and bounded reads use WithAuthorityView.
func (service *Service) ReadState(ctx context.Context) (State, error) {
	var state State
	err := service.WithAuthorityView(ctx, func(view AuthorityView) error { state = view.owner.state.Clone(); return nil })
	return state, err
}

// Head is strict metadata inspection for recovery preflight. It deliberately
// does not expose authorization state, publish a cache, or reset its proof;
// recovery must be able to inspect the chain before repairing projection.
func (service *Service) Head(ctx context.Context) (Head, error) {
	if err := service.rejectCallbackEntry(ctx); err != nil {
		return Head{}, err
	}
	if service == nil || service.fence == nil {
		return Head{}, ErrUnavailable
	}
	if !service.hasFenceMarker(ctx) {
		release, err := service.fence.Read(ctx)
		if err != nil {
			return Head{}, errorCode("security_busy")
		}
		defer release()
	}
	head, _, err := VerifyCommittedChain(ctx, service.ledger, service.heads, Head{Generation: 1})
	return head, err
}

func (service *Service) IsDescendant(ctx context.Context, trusted Head) (bool, error) {
	if err := service.rejectCallbackEntry(ctx); err != nil {
		return false, err
	}
	if service == nil || service.fence == nil || trusted.Generation != 1 {
		return false, nil
	}
	if !service.hasFenceMarker(ctx) {
		release, err := service.fence.Read(ctx)
		if err != nil {
			return false, errorCode("security_busy")
		}
		defer release()
		var lease *fenceLease
		ctx, lease = service.withFenceLease(ctx)
		defer lease.invalidate()
	}
	_, _, err := VerifyCommittedChain(ctx, service.ledger, service.heads, trusted)
	if errors.Is(err, ErrTrustedHeadRequired) {
		return false, nil
	}
	return err == nil, err
}

func (service *Service) loadState(ctx context.Context) (Head, State, error) {
	return service.loadStateAtEpoch(ctx, 0)
}

// Execute already checked this epoch under its exclusive fence. Reuse that
// value for the unbound strict adapter so it cannot change between the check
// and material construction; each CAS retry performs its own fresh check.
func (service *Service) loadStateAtEpoch(ctx context.Context, epoch uint64) (Head, State, error) {
	if !service.hasFenceMarker(ctx) {
		return VerifyCommittedChain(ctx, service.ledger, service.heads, Head{Generation: 1})
	}
	var owner *validatedAuthority
	var err error
	if service.binding == nil && epoch != 0 {
		service.cacheMu.Lock()
		owner, err = service.authorityMaterialLocked(ctx, AuthorityBinding{Epoch: epoch, TrustedHead: Head{Generation: 1}})
		service.cacheMu.Unlock()
	} else {
		owner, err = service.authorityMaterial(ctx)
	}
	if err != nil {
		return Head{}, State{}, err
	}
	lease := service.currentFenceLease(ctx)
	lease.mu.Lock()
	lease.material = owner
	lease.mu.Unlock()
	return owner.head, owner.state, nil
}

// VerifyCommittedChain streams and validates the complete committed chain and
// requires trusted to be either the live head or an exact ancestor.
func VerifyCommittedChain(ctx context.Context, ledger SecurityLedger, heads LedgerHeadStore, trusted Head) (Head, State, error) {
	if ledger == nil || heads == nil || trusted.Generation != 1 ||
		(trusted.Revision == 0) != (trusted.EventHash == [32]byte{}) {
		return Head{}, State{}, ErrTrustedHeadRequired
	}
	head, err := heads.Load(ctx)
	if err != nil {
		return Head{}, State{}, err
	}
	if head.Generation != 1 {
		return Head{}, State{}, ErrLedgerCorrupt
	}
	state := NewState()
	prior := Head{Generation: 1}
	foundTrusted := trusted == prior
	err = ledger.WalkCommitted(ctx, head, func(candidate Candidate) error {
		if err := validateCandidate(prior, candidate); err != nil {
			return err
		}
		var event Event
		if err := decodeEventStrict(candidate.EventJSON, &event); err != nil {
			return ErrLedgerCorrupt
		}
		if err := state.apply(event, candidate.EventHash); err != nil {
			return err
		}
		prior = Head{Generation: event.Generation, Revision: event.Revision, EventHash: candidate.EventHash}
		if prior.Revision == trusted.Revision {
			foundTrusted = prior == trusted
		}
		return nil
	})
	if err != nil {
		return Head{}, State{}, err
	}
	if state.Head() != head {
		return Head{}, State{}, ErrLedgerCorrupt
	}
	if !foundTrusted {
		return Head{}, State{}, ErrTrustedHeadRequired
	}
	return head, state, nil
}

func (service *Service) applyProjection(ctx context.Context, head Head) error {
	projected, err := service.projection.Head(ctx)
	if err != nil {
		return err
	}
	if projected.Generation != head.Generation || projected.Revision > head.Revision {
		return ErrLedgerCorrupt
	}
	if projected.Revision == head.Revision {
		if projected.EventHash != head.EventHash {
			return ErrLedgerCorrupt
		}
		return nil
	}
	var prior Head = Head{Generation: 1}
	err = service.ledger.WalkCommitted(ctx, head, func(candidate Candidate) error {
		if candidate.Prior != prior {
			return ErrLedgerCorrupt
		}
		if candidate.Prior.Revision >= projected.Revision {
			if candidate.Prior.Revision == projected.Revision && candidate.Prior.EventHash != projected.EventHash {
				return ErrLedgerCorrupt
			}
			if err := service.projection.Apply(ctx, candidate); err != nil {
				return err
			}
		}
		prior = Head{Generation: head.Generation, Revision: candidate.Prior.Revision + 1, EventHash: candidate.EventHash}
		return nil
	})
	if err != nil {
		return err
	}
	projected, err = service.projection.Head(ctx)
	if err != nil {
		return err
	}
	if projected != head {
		return errors.New("projection head mismatch")
	}
	return nil
}

func (service *Service) setQuarantine(value bool) {
	service.mu.Lock()
	service.quarantine = value
	service.mu.Unlock()
}

// publishAuthoritySuccessor runs only after durable CAS and projection success,
// while the writer still owns the exclusive fence. It never mutates a reader.
func (service *Service) publishAuthoritySuccessor(ctx context.Context, prior *validatedAuthority, candidate Candidate, next Head) error {
	if prior == nil || prior.head != candidate.Prior || validateCandidate(prior.head, candidate) != nil {
		return ErrLedgerCorrupt
	}
	var event Event
	if decodeEventStrict(candidate.EventJSON, &event) != nil {
		return ErrLedgerCorrupt
	}
	if next != (Head{Generation: event.Generation, Revision: event.Revision, EventHash: candidate.EventHash}) {
		return ErrLedgerCorrupt
	}
	var inventory CommittedInventory
	if ledger, ok := service.ledger.(IntegrityLedger); ok && service.binding != nil {
		var candidates []Candidate
		var err error
		if prior.inventory.Head == prior.head {
			inventory, candidates, err = ledger.ReadValidatedExtension(ctx, prior.inventory, next)
			if err == nil && (len(candidates) != 1 || candidates[0].EventHash != candidate.EventHash) {
				err = ErrLedgerCorrupt
			}
		} else {
			inventory, _, err = ledger.ReadValidatedInventory(ctx, next)
		}
		if err != nil {
			return err
		}
		if inventory.Head != next {
			return ErrLedgerCorrupt
		}
	}
	state := prior.state.Clone()
	if err := state.apply(event, candidate.EventHash); err != nil {
		return err
	}
	owner := &validatedAuthority{state: state, head: next, binding: prior.binding, inventory: inventory, keys: state.ReferencedCredentialKeyIDs()}
	service.cacheMu.Lock()
	service.retainAuthorityProofLocked(owner)
	service.cacheMu.Unlock()
	// Existing capabilities keep their immutable prior owner until expiry; only
	// subsequent reads under this lease see the successfully published successor.
	if lease := service.currentFenceLease(ctx); lease != nil {
		lease.mu.Lock()
		lease.material = owner
		lease.authority = nil
		err := service.loadCredentialInventory(ctx, owner, lease)
		if err == nil {
			lease.authority = owner
		}
		lease.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}
