//go:build !identitytest

package security

import "context"

// No runtime configuration or context value can enable faults in release.
func recoveryFault(context.Context, string) error { return nil }
