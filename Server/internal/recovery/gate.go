package recovery

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"babytracker/server/internal/security"
)

var ErrQuarantine = errors.New("authorization_quarantine")

type Authority interface {
	Check(context.Context) error
	Head(context.Context) (security.Head, error)
	IsDescendant(context.Context, security.Head) (bool, error)
	Reconcile(context.Context) error
	ReconcileTrusted(context.Context, security.Head) (security.Head, error)
}

type Schema interface {
	Check(context.Context) error
}

type Mirror interface {
	Load(context.Context) (State, bool, error)
	Update(context.Context, State) error
}

type Options struct {
	Store              *Store
	Security           Authority
	Schema             Schema
	Mirror             Mirror
	ExpectedInstanceID string
	ExpectedEpoch      uint64
	ActorIDKey         [32]byte
}

type Gate struct {
	store              *Store
	security           Authority
	schema             Schema
	mirror             Mirror
	expectedInstanceID string
	expectedEpoch      uint64
	actorIDKey         [32]byte
	mu                 sync.RWMutex
	quarantine         bool
}

func NewGate(options Options) (*Gate, error) {
	var zeroKey [32]byte
	if options.Store == nil || options.Security == nil || options.Schema == nil || options.Mirror == nil ||
		options.ExpectedInstanceID == "" || options.ExpectedEpoch == 0 || options.ActorIDKey == zeroKey {
		return nil, errors.New("recovery gate requires store, security, schema, mirror, expected identity, and actor ID key")
	}
	return &Gate{store: options.Store, security: options.Security, schema: options.Schema, mirror: options.Mirror,
		expectedInstanceID: options.ExpectedInstanceID, expectedEpoch: options.ExpectedEpoch, actorIDKey: options.ActorIDKey, quarantine: true}, nil
}

func (gate *Gate) Check(ctx context.Context) error {
	if gate == nil {
		return ErrQuarantine
	}
	state, head, err := gate.verifyPersistentIdentity(ctx)
	if err != nil {
		return quarantineError(err)
	}
	if err := gate.verifyMirror(ctx, state, head, false); err != nil {
		return quarantineError(err)
	}
	if err := gate.security.Check(ctx); err != nil {
		return quarantineError(err)
	}
	if err := gate.schema.Check(ctx); err != nil {
		return quarantineError(err)
	}
	gate.mu.RLock()
	defer gate.mu.RUnlock()
	if gate.quarantine {
		return ErrQuarantine
	}
	return nil
}

// ValidateOperatorAuthorityUnderFence revalidates recovery roots without
// reacquiring the shared security fence. The caller must already hold the
// authority write fence. Projection health is intentionally excluded so an
// exact committed operator receipt can repair mere projection lag.
func (gate *Gate) ValidateOperatorAuthorityUnderFence(ctx context.Context, authorityHead security.Head) error {
	if gate == nil || gate.store == nil || gate.security == nil || gate.schema == nil || gate.mirror == nil {
		return ErrQuarantine
	}
	gate.mu.RLock()
	quarantined := gate.quarantine
	gate.mu.RUnlock()
	if quarantined {
		return ErrQuarantine
	}
	state, err := gate.store.LoadUnderFence(ctx)
	if err != nil || state.InstanceID != gate.expectedInstanceID || state.Epoch != gate.expectedEpoch {
		if err == nil {
			err = errors.New("durable recovery identity does not match asserted environment")
		}
		return quarantineError(err)
	}
	if err := gate.store.validateActorIDKeyUnderFence(ctx, state.InstanceID, gate.actorIDKey); err != nil {
		return quarantineError(err)
	}
	if authorityHead != state.TrustedHead {
		descendant, err := gate.security.IsDescendant(ctx, state.TrustedHead)
		if err != nil {
			return quarantineError(err)
		}
		if !descendant {
			return quarantineError(errors.New("authority head is not a descendant of trusted recovery manifest"))
		}
	}
	if err := gate.verifyMirror(ctx, state, authorityHead, false); err != nil {
		return quarantineError(err)
	}
	if err := gate.schema.Check(ctx); err != nil {
		return quarantineError(err)
	}
	return nil
}

func (gate *Gate) Reconcile(ctx context.Context) error {
	gate.setQuarantine(true)
	state, err := gate.store.Load(ctx)
	if err != nil {
		return quarantineError(err)
	}
	if state.InstanceID != gate.expectedInstanceID || state.Epoch != gate.expectedEpoch {
		return quarantineError(errors.New("durable recovery identity does not match asserted environment"))
	}
	if err := gate.store.ValidateActorIDKey(ctx, state.InstanceID, gate.actorIDKey); err != nil {
		return quarantineError(err)
	}
	preReconcileHead, err := gate.security.Head(ctx)
	if err != nil {
		return quarantineError(err)
	}
	if err := gate.verifyMirror(ctx, state, preReconcileHead, true); err != nil {
		return quarantineError(err)
	}
	currentHead, err := gate.security.ReconcileTrusted(ctx, state.TrustedHead)
	if err != nil {
		return quarantineError(err)
	}
	mirrored := state
	mirrored.TrustedHead = currentHead
	if err := gate.mirror.Update(ctx, mirrored); err != nil {
		return quarantineError(err)
	}
	persisted, exists, err := gate.mirror.Load(ctx)
	if err != nil || !exists || persisted != mirrored {
		if err == nil {
			err = errors.New("database recovery mirror did not persist exact authority head")
		}
		return quarantineError(err)
	}
	if err := gate.schema.Check(ctx); err != nil {
		return quarantineError(err)
	}
	gate.setQuarantine(false)
	return nil
}

func (gate *Gate) State(ctx context.Context) (State, error) {
	state, err := gate.store.Load(ctx)
	if err != nil {
		return State{}, quarantineError(err)
	}
	return state, nil
}

func (gate *Gate) ServerState(ctx context.Context) (string, uint64, error) {
	state, err := gate.State(ctx)
	return state.InstanceID, state.Epoch, err
}

func (gate *Gate) verifyPersistentIdentity(ctx context.Context) (State, security.Head, error) {
	state, err := gate.store.Load(ctx)
	if err != nil {
		return State{}, security.Head{}, err
	}
	if state.InstanceID != gate.expectedInstanceID || state.Epoch != gate.expectedEpoch {
		return State{}, security.Head{}, errors.New("durable recovery identity does not match asserted environment")
	}
	if err := gate.store.ValidateActorIDKey(ctx, state.InstanceID, gate.actorIDKey); err != nil {
		return State{}, security.Head{}, err
	}
	head, err := gate.security.Head(ctx)
	if err != nil {
		return State{}, security.Head{}, err
	}
	if head != state.TrustedHead {
		descendant, err := gate.security.IsDescendant(ctx, state.TrustedHead)
		if err != nil {
			return State{}, security.Head{}, err
		}
		if !descendant {
			return State{}, security.Head{}, errors.New("authority head is not a descendant of trusted recovery manifest")
		}
	}
	return state, head, nil
}

func (gate *Gate) verifyMirror(ctx context.Context, durable State, authorityHead security.Head, allowMissing bool) error {
	mirrored, exists, err := gate.mirror.Load(ctx)
	if err != nil {
		return err
	}
	if !exists {
		if allowMissing {
			return nil
		}
		return errors.New("database recovery mirror is missing")
	}
	if mirrored.InstanceID != durable.InstanceID || mirrored.Epoch > durable.Epoch {
		return errors.New("database recovery identity is ahead or mismatched")
	}
	if mirrored.Epoch < durable.Epoch {
		return nil
	}
	if mirrored.TrustedHead == authorityHead {
		return nil
	}
	if mirrored.TrustedHead.Revision >= authorityHead.Revision {
		return errors.New("database recovery head conflicts with authority")
	}
	descendant, err := gate.security.IsDescendant(ctx, mirrored.TrustedHead)
	if err != nil {
		return err
	}
	if !descendant {
		return errors.New("database recovery head is not an authority ancestor")
	}
	return nil
}

func (gate *Gate) setQuarantine(value bool) {
	gate.mu.Lock()
	gate.quarantine = value
	gate.mu.Unlock()
}

func quarantineError(err error) error {
	return fmt.Errorf("%w: %w", ErrQuarantine, err)
}
