//go:build identitytest

package testkit

import (
	"errors"
	"sync"
)

type RecoveryFaultPoint string

const (
	BeforeAuthorityCommit RecoveryFaultPoint = "before_authority_commit"
	AfterAuthorityCommit  RecoveryFaultPoint = "after_authority_commit"
	AfterProjection       RecoveryFaultPoint = "after_projection"
)

// RecoveryFaults is owned by a synthetic composition. Each attempt/point fails
// exactly once; callers cannot reset it while the application is running.
type RecoveryFaults struct {
	mu    sync.Mutex
	fired map[string]bool
}

func (f *RecoveryFaults) FailOnce(point RecoveryFaultPoint, attemptID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if point != BeforeAuthorityCommit && point != AfterAuthorityCommit && point != AfterProjection {
		return nil
	}
	if f.fired == nil {
		f.fired = make(map[string]bool)
	}
	key := string(point) + ":" + attemptID
	if f.fired[key] {
		return nil
	}
	f.fired[key] = true
	return errors.New("identitytest recovery boundary interrupted")
}
