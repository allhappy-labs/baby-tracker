package protocol

import "testing"

func TestTimerStopAcceptsExplicitShortBreastCleanup(t *testing.T) {
	raw := mutationJSON("timer", "timer_stop", 5, `{"baby_id":"10000000-0000-4000-8000-000000000001",`+
		`"completed_record_id":"20000000-0000-4000-8000-000000000001",`+
		`"stopped_at":"2026-09-28T12:00:00.000000Z","component_id":"30000000-0000-4000-8000-000000000001",`+
		`"cleanup_short_breast_segments":true}`)
	mutation, err := DecodeMutation(raw)
	if err != nil {
		t.Fatalf("timer stop with cleanup opt-in rejected: %v", err)
	}
	if stop, ok := mutation.Payload.(TimerStopPayload); !ok || !stop.CleanupShortBreastSegments {
		t.Fatalf("payload=%+v", mutation.Payload)
	}
}
