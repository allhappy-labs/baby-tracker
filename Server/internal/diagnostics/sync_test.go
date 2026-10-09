package diagnostics

import (
	"context"
	"testing"
	"time"
)

func TestRecordOnlyAllowsBoundedSyncPhases(t *testing.T) {
	var got []Phase
	recorder := Recorder(func(_ context.Context, phase Phase, elapsed time.Duration) {
		if elapsed < 0 {
			t.Fatal("negative elapsed duration")
		}
		got = append(got, phase)
	})
	for _, phase := range []Phase{Authority, Database, Observer, Phase("patient_payload"), Phase("")} {
		Record(context.Background(), recorder, phase, time.Millisecond)
	}
	if len(got) != 3 || got[0] != Authority || got[1] != Database || got[2] != Observer {
		t.Fatalf("unexpected bounded phases: %v", got)
	}
}

func TestNilRecorderDoesNotRecord(t *testing.T) {
	Record(context.Background(), nil, Authority, time.Second)
}

func TestContextRecorderClampsElapsedTime(t *testing.T) {
	var got time.Duration
	ctx := WithRecorder(context.Background(), func(_ context.Context, phase Phase, elapsed time.Duration) {
		if phase != Database {
			t.Fatalf("phase = %q", phase)
		}
		got = elapsed
	})
	Record(ctx, nil, Database, 3*time.Hour)
	if got != time.Minute {
		t.Fatalf("elapsed = %s, want one minute", got)
	}
}

func TestCapturedMetricsDeferCallbacksAndCloseBeforeReentry(t *testing.T) {
	calls := 0
	var ctx context.Context
	ctx = WithRecorder(t.Context(), func(_ context.Context, phase Phase, elapsed time.Duration) {
		calls++
		if phase != SecurityFence || elapsed != 7*time.Millisecond {
			t.Fatalf("unexpected metric %s %v", phase, elapsed)
		}
		Capture(ctx, SecurityFence, time.Second)
		Flush(ctx)
	})
	Capture(ctx, SecurityFence, 3*time.Millisecond)
	Capture(ctx, SecurityFence, 4*time.Millisecond)
	Capture(ctx, Phase("secret"), time.Second)
	if calls != 0 {
		t.Fatal("callback ran in protected capture")
	}
	Flush(ctx)
	Flush(ctx)
	if calls != 1 {
		t.Fatalf("reentry/duplicate flush calls=%d", calls)
	}
	if !Start(t.Context()).IsZero() {
		t.Fatal("disabled diagnostic started clock")
	}
}
