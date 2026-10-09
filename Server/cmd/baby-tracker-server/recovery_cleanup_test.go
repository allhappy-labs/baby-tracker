package main

import (
	"babytracker/server/internal/testpg"
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"testing"
	"time"
)

type recoveryCleanupFunc func(context.Context) error

func (f recoveryCleanupFunc) RunOnce(ctx context.Context) error { return f(ctx) }
func TestRecoveryCleanupCadenceCancellationAndError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := make(chan time.Time, 4)
	done := make(chan struct{})
	worker := recoveryCleanupFunc(func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("iteration lacks timeout")
		}
		calls <- time.Now()
		return errors.New("synthetic transient")
	})
	go func() {
		runRecoveryCleanup(ctx, readinessFunc(func(context.Context) error { return nil }), worker, nil, 20*time.Millisecond)
		close(done)
	}()
	var first, second time.Time
	select {
	case first = <-calls:
	case <-time.After(time.Second):
		t.Fatal("initial cleanup missing")
	}
	select {
	case second = <-calls:
	case <-time.After(time.Second):
		t.Fatal("error stopped worker")
	}
	if second.Sub(first) < 15*time.Millisecond {
		t.Fatal("cleanup did not honor cadence")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup ignored cancellation")
	}
}
func TestRecoveryCleanupIsolatedPrincipal(t *testing.T) {
	fixture := testpg.New(t)
	var role string
	if err := fixture.Maintenance.QueryRow(t.Context(), "SELECT current_user").Scan(&role); err != nil {
		t.Fatal(err)
	}
	if err := verifyExportPool(t.Context(), fixture.Maintenance, role); err != nil {
		t.Fatal(err)
	}
	for _, pool := range []*pgxpool.Pool{fixture.Runtime, fixture.Projector, fixture.Owner} {
		if err := verifyExportPool(t.Context(), pool, role); err == nil {
			t.Fatal("nonmaintenance pool accepted")
		}
	}
	runner := recoveryCleanup{pool: fixture.Maintenance, now: time.Now}
	if err := runner.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
}
