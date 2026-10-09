//go:build identitytest

package testkit

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestRecoveryFaultsOneShotAttemptAndPointBound(t *testing.T) {
	var f RecoveryFaults
	var count atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if f.FailOnce(AfterProjection, "attempt-a") != nil {
				count.Add(1)
			}
		}()
	}
	wg.Wait()
	if count.Load() != 1 {
		t.Fatalf("failures=%d", count.Load())
	}
	if f.FailOnce(AfterAuthorityCommit, "attempt-a") == nil || f.FailOnce(AfterProjection, "attempt-b") == nil {
		t.Fatal("one shot leaked across point/attempt")
	}
	if f.FailOnce(RecoveryFaultPoint("after_decode"), "attempt-c") != nil {
		t.Fatal("server claimed a client decode boundary")
	}
}
