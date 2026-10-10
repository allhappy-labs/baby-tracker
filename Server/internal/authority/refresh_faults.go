//go:build !identitytest

package authority

import "context"

// No runtime configuration or context value can enable refresh faults in a
// release build (security/refresh_recovery_faults.go).
func refreshFault(context.Context, string) error { return nil }
