//go:build !unix

package loadtest

import "time"

// processCPU is unknown here (the generator runs on Linux).
func processCPU() (user, system time.Duration, ok bool) { return 0, 0, false }
