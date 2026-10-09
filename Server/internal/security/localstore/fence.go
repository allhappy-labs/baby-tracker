package localstore

import (
	"context"
	"sync"
	"syscall"
	"time"

	"babytracker/server/internal/diagnostics"
)

type FileFence struct {
	path string
	mu   sync.RWMutex
}

func (fence *FileFence) Read(ctx context.Context) (func(), error) {
	start := diagnostics.Start(ctx)
	defer func() { diagnostics.CaptureSince(ctx, diagnostics.SecurityFence, start) }()
	for !fence.mu.TryRLock() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	file, err := acquireFileLock(ctx, fence.path, syscall.LOCK_SH)
	if err != nil {
		fence.mu.RUnlock()
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { releaseFileLock(file); fence.mu.RUnlock() }) }, nil
}

func (fence *FileFence) Write(ctx context.Context) (func(), error) {
	start := diagnostics.Start(ctx)
	defer func() { diagnostics.CaptureSince(ctx, diagnostics.SecurityFence, start) }()
	for !fence.mu.TryLock() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	file, err := acquireFileLock(ctx, fence.path, syscall.LOCK_EX)
	if err != nil {
		fence.mu.Unlock()
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { releaseFileLock(file); fence.mu.Unlock() }) }, nil
}
