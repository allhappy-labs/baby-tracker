//go:build identitytest

package security

import "context"

type recoveryFaultKey struct{}

// WithRecoveryFault is linked only into synthetic identitytest binaries.
func WithRecoveryFault(ctx context.Context, fail func(string) error) context.Context {
	return context.WithValue(ctx, recoveryFaultKey{}, fail)
}
func recoveryFault(ctx context.Context, point string) error {
	if fail, ok := ctx.Value(recoveryFaultKey{}).(func(string) error); ok {
		return fail(point)
	}
	return nil
}
