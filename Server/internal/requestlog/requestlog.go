// Package requestlog carries one request-scoped logging decision from a
// handler back to the access-log middleware: a response marked with
// Aggregate is a repeat over-limit refusal whose completion is counted in a
// periodic summary instead of being logged on its own line.
package requestlog

import (
	"context"
	"sync/atomic"
)

type markKey struct{}

// WithMark returns a context in which handlers may mark the response, and a
// function reporting whether one did. The access-log middleware owns it.
func WithMark(ctx context.Context) (context.Context, func() bool) {
	mark := &atomic.Bool{}
	return context.WithValue(ctx, markKey{}, mark), mark.Load
}

// Aggregate marks the response as a repeat over-limit refusal. Without the
// middleware's mark it does nothing, and the request is logged as usual.
func Aggregate(ctx context.Context) {
	if mark, ok := ctx.Value(markKey{}).(*atomic.Bool); ok {
		mark.Store(true)
	}
}
