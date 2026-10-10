//go:build identitytest

package authority

import "context"

type refreshFaultKey struct{}

// WithRecoveryFault is linked only into synthetic identitytest binaries. fail
// is called at each refresh rotation boundary ("before_authority_commit",
// "after_authority_commit", "after_projection"); a non-nil error interrupts
// the rotation there. Before the commit nothing is written; after it the
// child is committed and the caller sees an error, as a lost reply.
// "after_projection" is kept for the legacy fault vocabulary: there is no
// projection, so it is a second after-commit point.
func WithRecoveryFault(ctx context.Context, fail func(string) error) context.Context {
	return context.WithValue(ctx, refreshFaultKey{}, fail)
}

func refreshFault(ctx context.Context, point string) error {
	if fail, ok := ctx.Value(refreshFaultKey{}).(func(string) error); ok {
		return fail(point)
	}
	return nil
}
