package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type lifecycleWorkerFunc func(context.Context) error

func (function lifecycleWorkerFunc) RunOnce(ctx context.Context) error { return function(ctx) }

func TestAppleRevocationWorkerWaitsForReadinessAndStopsNewClaims(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var ready atomic.Bool
	var calls atomic.Int64
	checked := make(chan struct{})
	var checkedOnce sync.Once
	done := make(chan struct{})
	go func() {
		runLifecycleWorker(ctx, readinessFunc(func(context.Context) error {
			checkedOnce.Do(func() { close(checked) })
			if ready.Load() {
				return nil
			}
			return errors.New("not ready")
		}), lifecycleWorkerFunc(func(context.Context) error {
			calls.Add(1)
			return nil
		}), time.Millisecond, 50*time.Millisecond, nil)
		close(done)
	}()
	<-checked
	if calls.Load() != 0 {
		t.Fatal("worker claimed before readiness")
	}
	ready.Store(true)
	deadline := time.After(time.Second)
	for calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("ready worker never claimed")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker loop did not stop")
	}
	stoppedAt := calls.Load()
	if calls.Load() != stoppedAt {
		t.Fatal("worker claimed after shutdown")
	}
}

func TestAppleRevocationShutdownLetsCurrentLeaseBoundedIterationFinish(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var sawCanceled atomic.Bool
	go func() {
		runLifecycleWorker(ctx, readinessFunc(func(context.Context) error { return nil }), lifecycleWorkerFunc(func(iteration context.Context) error {
			close(started)
			select {
			case <-iteration.Done():
				sawCanceled.Store(true)
				return iteration.Err()
			case <-release:
				return nil
			}
		}), time.Hour, 100*time.Millisecond, nil)
		close(done)
	}()
	<-started
	cancel()
	select {
	case <-done:
		t.Fatal("shutdown returned before the current iteration")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not complete after iteration")
	}
	if sawCanceled.Load() {
		t.Fatal("process cancellation propagated into the active lease-bounded iteration")
	}
}

func TestAppleRevocationIterationCannotOutliveLease(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		runLifecycleWorker(ctx, readinessFunc(func(context.Context) error { return nil }), lifecycleWorkerFunc(func(iteration context.Context) error {
			<-iteration.Done()
			cancel()
			return iteration.Err()
		}), time.Hour, 10*time.Millisecond, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("lease deadline did not bound iteration")
	}
}

func TestAppleRevocationCancellationDuringReadinessStartsNoDetachedClaim(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var calls atomic.Int64
	done := make(chan struct{})
	go func() {
		runLifecycleWorker(ctx, readinessFunc(func(context.Context) error {
			cancel()
			return nil
		}), lifecycleWorkerFunc(func(context.Context) error {
			calls.Add(1)
			return nil
		}), time.Hour, time.Second, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after readiness cancellation")
	}
	if calls.Load() != 0 {
		t.Fatalf("detached claims=%d", calls.Load())
	}
}
