//go:build unix

package loadtest

import (
	"syscall"
	"time"
)

// processCPU is the process's user and system CPU time so far (getrusage).
func processCPU() (user, system time.Duration, ok bool) {
	var r syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &r) != nil {
		return 0, 0, false
	}
	return time.Duration(r.Utime.Nano()), time.Duration(r.Stime.Nano()), true
}
