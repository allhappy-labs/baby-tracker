package database

import (
	"context"
	"errors"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// WorkerSlots reserves one maintenance connection for the readiness audit.
// Every maintenance worker (the export worker; the care receipt and expiry
// sweeps; the family purge; the audit partition roll; the push-token sweep;
// the wake worker's tick and its post-send settle) runs each unit of work
// inside a slot (Do, or Acquire and release), and holds at most one
// connection of the pool while it does. There is one
// slot fewer than the pool has connections, so however busy the workers
// are, the readiness audit's role check finds a free connection and a busy
// pool never reads as role drift.
//
// One WorkerSlots belongs to one maintenance pool: workers sharing a pool
// share its WorkerSlots.
type WorkerSlots struct{ slots chan struct{} }

// ErrWorkerSlots is a maintenance pool too small to reserve the audit's
// connection beside one worker.
var ErrWorkerSlots = errors.New("maintenance pool needs at least 2 connections: one is reserved for the readiness audit")

// NewWorkerSlots returns the worker slots of pool: its maximum connections
// minus the one reserved for the readiness audit.
func NewWorkerSlots(pool *pgxpool.Pool) (*WorkerSlots, error) {
	if pool == nil || pool.Config().MaxConns < 2 {
		return nil, ErrWorkerSlots
	}
	return &WorkerSlots{slots: make(chan struct{}, pool.Config().MaxConns-1)}, nil
}

// Size is the number of workers that may hold a connection at once.
func (s *WorkerSlots) Size() int { return cap(s.slots) }

// Acquire waits for a free slot (or ctx) and returns its release, which is
// safe to call more than once.
func (s *WorkerSlots) Acquire(ctx context.Context) (release func(), err error) {
	// A select over a free slot and a done context picks either at random:
	// a cancelled context must never take a slot.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var once sync.Once
	return func() { once.Do(func() { <-s.slots }) }, nil
}

// Do runs work inside a slot and returns its error, or ctx's while waiting.
func (s *WorkerSlots) Do(ctx context.Context, work func(context.Context) error) error {
	release, err := s.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	return work(ctx)
}
