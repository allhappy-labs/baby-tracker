package diagnostics

import (
	"context"
	"sync"
	"time"
)

// Phase is deliberately closed to operational timing categories.
// Never add request, account, family, mutation, or payload fields here.
type Phase string

const (
	Authority          Phase = "authority"
	Database           Phase = "database"
	Observer           Phase = "observer"
	SecurityFence      Phase = "security_fence"
	FamilyFence        Phase = "family_fence"
	TransactionAcquire Phase = "transaction_acquire"
	IntegrityInventory Phase = "integrity_inventory"
	IntegrityExtension Phase = "integrity_extension"
	IntegrityVerify    Phase = "integrity_verify"
)

type Recorder func(context.Context, Phase, time.Duration)

type recorderKey struct{}
type captureKey struct{}
type captured struct {
	mu     sync.Mutex
	closed bool
	values map[Phase]time.Duration
}

// Start is inert without opt-in. Capture only adds bounded scalars and never
// invokes observers, performs I/O, or reads authority state while fences are held.
func Start(ctx context.Context) time.Time {
	if ctx != nil {
		if c, _ := ctx.Value(captureKey{}).(*captured); c != nil {
			return time.Now()
		}
	}
	return time.Time{}
}
func CaptureSince(ctx context.Context, phase Phase, start time.Time) {
	if !start.IsZero() {
		Capture(ctx, phase, time.Since(start))
	}
}
func Capture(ctx context.Context, phase Phase, elapsed time.Duration) {
	switch phase {
	case SecurityFence, FamilyFence, TransactionAcquire, IntegrityInventory, IntegrityExtension, IntegrityVerify:
	default:
		return
	}
	if ctx == nil {
		return
	}
	c, _ := ctx.Value(captureKey{}).(*captured)
	if c == nil {
		return
	}
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed > time.Minute {
		elapsed = time.Minute
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.values[phase] += elapsed
		if c.values[phase] > time.Minute {
			c.values[phase] = time.Minute
		}
	}
}

// Flush belongs after the entire outer handler/lease has returned. Close and
// detach before callbacks so nested observers cannot replay or append metrics.
func Flush(ctx context.Context) {
	if ctx == nil {
		return
	}
	c, _ := ctx.Value(captureKey{}).(*captured)
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	values := c.values
	c.values = nil
	c.mu.Unlock()
	for _, phase := range []Phase{SecurityFence, FamilyFence, TransactionAcquire, IntegrityInventory, IntegrityExtension, IntegrityVerify} {
		if elapsed, ok := values[phase]; ok {
			Record(ctx, nil, phase, elapsed)
		}
	}
}

// WithRecorder is process-local and opt-in; nil keeps the ordinary path inert.
func WithRecorder(ctx context.Context, recorder Recorder) context.Context {
	if recorder == nil {
		return ctx
	}
	ctx = context.WithValue(ctx, recorderKey{}, recorder)
	return context.WithValue(ctx, captureKey{}, &captured{values: make(map[Phase]time.Duration)})
}

// Record refuses arbitrary phase strings and clamps elapsed time to a bounded
// value before it can reach an observer or production log.
func Record(ctx context.Context, recorder Recorder, phase Phase, elapsed time.Duration) {
	if recorder == nil && ctx != nil {
		recorder, _ = ctx.Value(recorderKey{}).(Recorder)
	}
	if recorder == nil {
		return
	}
	switch phase {
	case Authority, Database, Observer, SecurityFence, FamilyFence, TransactionAcquire, IntegrityInventory, IntegrityExtension, IntegrityVerify:
	default:
		return
	}
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed > time.Minute {
		elapsed = time.Minute
	}
	recorder(ctx, phase, elapsed)
}
