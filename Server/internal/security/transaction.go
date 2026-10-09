package security

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/diagnostics"
	"babytracker/server/internal/tenant"
	"github.com/jackc/pgx/v5"
)

type fenceContextKey struct{}
type callbackContextKey struct{}
type decisionReadContextKey struct{}

type fenceLease struct {
	service         *Service
	active          atomic.Bool
	mu              sync.Mutex
	authority       *validatedAuthority
	material        *validatedAuthority
	credentialOwner *validatedAuthority
	credentialKeys  CredentialKeyInventory
	credentialErr   error
}

type decisionReadLease struct {
	service  *Service
	internal context.Context
	mu       sync.Mutex
	cond     *sync.Cond
	active   bool
	borrows  uint64
}

func (service *Service) withFenceLease(ctx context.Context) (context.Context, *fenceLease) {
	lease := &fenceLease{service: service}
	lease.active.Store(true)
	return context.WithValue(ctx, fenceContextKey{}, lease), lease
}

func (service *Service) hasFenceMarker(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	lease, ok := ctx.Value(fenceContextKey{}).(*fenceLease)
	return ok && lease.service == service && lease.active.Load()
}

func (service *Service) currentFenceLease(ctx context.Context) *fenceLease {
	if ctx == nil {
		return nil
	}
	lease, _ := ctx.Value(fenceContextKey{}).(*fenceLease)
	if lease == nil || lease.service != service || !lease.active.Load() {
		return nil
	}
	return lease
}
func (service *Service) hasExpiredFenceLease(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	lease, _ := ctx.Value(fenceContextKey{}).(*fenceLease)
	return lease != nil && lease.service == service && !lease.active.Load()
}

func (lease *fenceLease) invalidate() {
	if lease != nil {
		// Finish in-progress validation/publication before its filesystem fence is released.
		lease.mu.Lock()
		lease.active.Store(false)
		lease.mu.Unlock()
	}
}

func (service *Service) withCallbackDenial(ctx context.Context) context.Context {
	if service.callbackContextDenied(ctx) {
		return ctx
	}
	return context.WithValue(ctx, callbackContextKey{}, service)
}

func (service *Service) callbackContextDenied(ctx context.Context) bool {
	return service != nil && ctx != nil && ctx.Value(callbackContextKey{}) == service
}

func (service *Service) rejectCallbackEntry(ctx context.Context) error {
	if service.callbackContextDenied(ctx) || service.hasExpiredFenceLease(ctx) {
		return errorCode("security_busy")
	}
	return nil
}

func newDecisionReadLease(service *Service, internal context.Context) *decisionReadLease {
	lease := &decisionReadLease{service: service, internal: internal, active: true}
	lease.cond = sync.NewCond(&lease.mu)
	return lease
}

func (lease *decisionReadLease) borrow(service *Service) (context.Context, func(), bool) {
	if lease == nil || service == nil {
		return nil, nil, false
	}
	lease.mu.Lock()
	if !lease.active || lease.service != service {
		lease.mu.Unlock()
		return nil, nil, false
	}
	lease.borrows++
	internal := lease.internal
	lease.mu.Unlock()
	return internal, lease.releaseBorrow, true
}

func (lease *decisionReadLease) releaseBorrow() {
	lease.mu.Lock()
	if lease.borrows > 0 {
		lease.borrows--
	}
	if !lease.active && lease.borrows == 0 {
		lease.cond.Broadcast()
	}
	lease.mu.Unlock()
}

func (lease *decisionReadLease) invalidateAndWait() {
	lease.mu.Lock()
	lease.active = false
	for lease.borrows != 0 {
		lease.cond.Wait()
	}
	lease.internal = nil
	lease.mu.Unlock()
}

func (service *Service) borrowDecisionRead(ctx context.Context) (context.Context, func(), bool) {
	if service == nil || ctx == nil {
		return nil, nil, false
	}
	lease, _ := ctx.Value(decisionReadContextKey{}).(*decisionReadLease)
	return lease.borrow(service)
}

func (service *Service) decideWithAuthorityRead(callbackCtx, internalCtx context.Context, principal access.Principal, familyID, action string) (access.Grant, error) {
	lease := newDecisionReadLease(service, internalCtx)
	decisionCtx := context.WithValue(service.withCallbackDenial(callbackCtx), decisionReadContextKey{}, lease)
	defer lease.invalidateAndWait()
	return service.decide(decisionCtx, principal, familyID, action)
}

func (service *Service) WithAuthorizedTransaction(ctx context.Context, principal access.Principal, familyID, action string, run func(pgx.Tx, access.Grant) error) error {
	if err := service.rejectCallbackEntry(ctx); err != nil {
		return err
	}
	if service == nil || service.decide == nil || run == nil {
		return ErrUnavailable
	}
	callbackCtx := ctx
	ctx, head, release, err := service.beginGuardedTransaction(ctx, principal)
	if err != nil {
		return err
	}
	databaseStart := time.Now()
	defer func() {
		elapsed := time.Since(databaseStart)
		release()
		diagnostics.Record(callbackCtx, nil, diagnostics.Database, elapsed)
	}()
	grant, err := service.decideWithAuthorityRead(callbackCtx, ctx, principal, familyID, action)
	if err != nil {
		return err
	}
	if grant.AccountID != principal.AccountID || grant.FamilyID != familyID || grant.Revision != head.Revision {
		return ErrMembershipRevoked
	}
	acquireStart := diagnostics.Start(ctx)
	tx, err := service.runtime.BeginTx(ctx, pgx.TxOptions{})
	diagnostics.CaptureSince(ctx, diagnostics.TransactionAcquire, acquireStart)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer tx.Rollback(ctx)
	if err := tenant.SetLocal(ctx, tx, tenant.Context{AccountID: grant.AccountID, FamilyID: grant.FamilyID}); err != nil {
		return err
	}
	if err := run(tx, grant); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return nil
}

func (service *Service) WithAccountTransaction(ctx context.Context, principal access.Principal, run func(pgx.Tx) error) error {
	if err := service.rejectCallbackEntry(ctx); err != nil {
		return err
	}
	if service == nil || run == nil {
		return ErrUnavailable
	}
	ctx, _, release, err := service.beginGuardedTransaction(ctx, principal)
	if err != nil {
		return err
	}
	defer release()
	if principal.Scope != "account" && principal.Scope != "family" {
		return errorCode("session_revoked")
	}
	acquireStart := diagnostics.Start(ctx)
	tx, err := service.runtime.BeginTx(ctx, pgx.TxOptions{})
	diagnostics.CaptureSince(ctx, diagnostics.TransactionAcquire, acquireStart)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer tx.Rollback(ctx)
	if err := tenant.SetAccountLocal(ctx, tx, principal.AccountID); err != nil {
		return err
	}
	if err := run(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return nil
}

func (service *Service) beginGuardedTransaction(ctx context.Context, principal access.Principal) (context.Context, Head, func(), error) {
	start := time.Now()
	callerCtx := ctx
	recordFailure := func() { diagnostics.Record(callerCtx, nil, diagnostics.Authority, time.Since(start)) }
	if service == nil || service.fence == nil || service.runtime == nil || service.epoch == nil || service.now == nil {
		recordFailure()
		return ctx, Head{}, nil, ErrUnavailable
	}
	release, err := service.fence.Read(ctx)
	if err != nil {
		recordFailure()
		return ctx, Head{}, nil, errorCode("security_busy")
	}
	ctx, lease := service.withFenceLease(ctx)
	var authorityElapsed time.Duration
	releaseAll := func() {
		if authorityElapsed == 0 {
			authorityElapsed = time.Since(start)
		}
		lease.invalidate()
		release()
		diagnostics.Record(callerCtx, nil, diagnostics.Authority, authorityElapsed)
	}
	fail := func(err error) (context.Context, Head, func(), error) {
		releaseAll()
		return ctx, Head{}, nil, err
	}
	owner, err := service.loadValidatedAuthority(ctx)
	if err != nil {
		return fail(err)
	}
	head, state, epoch := owner.head, owner.state, owner.binding.Epoch
	if principal.RecoveryEpoch == 0 || principal.RecoveryEpoch != epoch {
		return fail(errorCode("epoch_changed"))
	}
	if err := validateSessionPrincipal(state, head, epoch, principal, service.now()); err != nil {
		return fail(err)
	}
	authorityElapsed = time.Since(start)
	return ctx, head, releaseAll, nil
}

func validateSessionPrincipal(state State, head Head, epoch uint64, principal access.Principal, now time.Time) error {
	if principal.AccountID == "" || principal.DeviceID == "" || principal.SessionID == "" ||
		principal.RecoveryEpoch == 0 || principal.RecoveryEpoch != epoch || principal.SecurityRevision == 0 || principal.SecurityRevision > head.Revision {
		return errorCode("session_revoked")
	}
	account, accountOK := state.Account(principal.AccountID)
	device, deviceOK := state.Device(principal.DeviceID)
	session, sessionOK := state.Session(principal.SessionID)
	if !accountOK || account.Status != "active" || !deviceOK || device.Status != "active" || device.AccountID != principal.AccountID ||
		!sessionOK || session.Status != "active" || session.AccountID != principal.AccountID || session.DeviceID != principal.DeviceID ||
		session.RecoveryEpoch != epoch || session.SecurityRevision != principal.SecurityRevision {
		return errorCode("session_revoked")
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, session.ExpiresAt)
	if err != nil {
		return ErrLedgerCorrupt
	}
	if !now.UTC().Before(expiresAt) {
		return errorCode("reauthentication_required")
	}
	return nil
}
