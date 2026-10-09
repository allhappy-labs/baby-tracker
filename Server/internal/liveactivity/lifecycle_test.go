package liveactivity

import (
	"testing"
	"time"
)

func lifecycleCard(kind, timer, session string) *DesiredCard {
	d := &DesiredCard{Scope: ActivityScope{ServerInstanceID: "11111111-1111-4111-8111-111111111111", RecoveryEpoch: 1, FamilyID: "22222222-2222-4222-8222-222222222222", BabyID: "33333333-3333-4333-8333-333333333333", KindRaw: kind}, MayStart: true, Phase: "running", Sequence: 10, CanWrite: true, Content: ContentState{StartedAt: 100, ActivityKind: "sleep", Phase: "running", Parts: []PartLabel{}}}
	if timer != "" {
		d.TimerID = &timer
	}
	if session != "" {
		d.SessionID = &session
	}
	return d
}

func TestLiveActivitySameSecondOrdering(t *testing.T) {
	now := time.Unix(1800000000, 100)
	d := lifecycleCard("sleep", "44444444-4444-4444-8444-444444444444", "")
	first := Reconcile(TargetState{}, d, now)
	if first.Event != "start" || first.State.GenerationID == "" {
		t.Fatalf("first=%+v", first)
	}
	state := first.State
	state.Status = "active"
	state.ActivityID = "activity"
	state.TokenVersion = 1
	state.PendingEvent = ""
	d.Content.StartedAt++
	next := Reconcile(state, d, now.Add(time.Millisecond))
	if next.Event != "wait" || !next.Deadline.Equal(time.Unix(now.Unix()+1, 0)) || next.State.LastTimestamp != now.Unix() {
		t.Fatalf("same second=%+v", next)
	}
	d.Content.StartedAt++ // deadline must project latest, not replay an old payload
	next = Reconcile(next.State, d, next.Deadline)
	if next.Event != "update" || next.State.LastTimestamp != now.Unix()+1 || next.State.LastDigest == first.State.LastDigest {
		t.Fatalf("later=%+v", next)
	}
}

func TestLiveActivityAmbiguousStart(t *testing.T) {
	now := time.Unix(1800000000, 0)
	d := lifecycleCard("sleep", "44444444-4444-4444-8444-444444444444", "")
	first := Reconcile(TargetState{}, d, now)
	if first.State.Status != "startUnconfirmed" {
		t.Fatal("start must be durable-unconfirmed before I/O")
	}
	next := Reconcile(first.State, d, now.Add(time.Minute))
	if next.Event == "start" {
		t.Fatal("ambiguous or crashed start was blindly repeated")
	}
	state := next.State
	state.ActivityID = "late-activity"
	state.TokenVersion = 2
	next = Reconcile(state, d, now.Add(time.Minute))
	if next.Event != "update" || next.State.GenerationID != first.State.GenerationID {
		t.Fatalf("late token did not attach: %+v", next)
	}
}

func TestLiveActivityGenerationAndDismissal(t *testing.T) {
	now := time.Unix(1800000000, 0)
	d := lifecycleCard("feed", "44444444-4444-4444-8444-444444444444", "55555555-5555-4555-8555-555555555555")
	s := Reconcile(TargetState{}, d, now).State
	s.ActivityID = "card"
	s.TokenVersion = 1
	s.Status = "active"
	s.PendingEvent = ""
	d.TimerID = nil
	d.MayStart = false
	d.Phase = "feedBreak"
	d.Content.Phase = "feedBreak"
	expiry := now.Add(10 * time.Minute)
	d.ExpiresAt = &expiry
	br := Reconcile(s, d, now.Add(time.Second))
	if br.Event != "update" || br.State.GenerationID != s.GenerationID || !br.Deadline.Equal(expiry) {
		t.Fatalf("break=%+v", br)
	}
	newTimer := "66666666-6666-4666-8666-666666666666"
	d.TimerID = &newTimer
	d.MayStart = true
	d.Phase = "running"
	d.Content.Phase = "running"
	continued := Reconcile(br.State, d, now.Add(2*time.Second))
	if continued.State.GenerationID != s.GenerationID {
		t.Fatal("feed continuation replaced generation")
	}
	dismissed := continued.State
	dismissed.DismissedGeneration = s.GenerationID
	dismissed.ActivityID = ""
	if got := Reconcile(dismissed, d, now.Add(3*time.Second)); got.Event == "start" {
		t.Fatal("dismissed generation restarted")
	}
	ended := Reconcile(continued.State, nil, now.Add(3*time.Second))
	if ended.Event != "end" {
		t.Fatalf("end=%+v", ended)
	}
	ended.State.ActivityID = ""
	ended.State.PendingEvent = ""
	if got := Reconcile(ended.State, d, now.Add(4*time.Second)); got.Event != "start" || got.State.GenerationID == s.GenerationID {
		t.Fatal("closed lifecycle reused generation")
	}
}

func TestLiveActivityOriginAndCoalescing(t *testing.T) {
	now := time.Unix(1800000000, 0)
	d := lifecycleCard("sleep", "44444444-4444-4444-8444-444444444444", "")
	s := TargetState{SuppressStart: true}
	wait := Reconcile(s, d, now)
	if wait.Event != "wait" {
		t.Fatalf("origin=%+v", wait)
	}
	wait.State.ActivityID = "local"
	wait.State.TokenVersion = 1
	if got := Reconcile(wait.State, d, now.Add(time.Second)); got.Event != "update" {
		t.Fatal("local registration never receives changes")
	}
	if got := Reconcile(TargetState{}, nil, now); got.Event != "none" {
		t.Fatal("coalesced start-stop sent historical start")
	}
}

func TestLiveActivityRetryPolicyAndTerminalSupersession(t *testing.T) {
	for i, want := range []time.Duration{2, 4, 8, 16, 32, 60, 60} {
		if got := retryDelay(i+1, func(d time.Duration) time.Duration { return d }); got != want*time.Second {
			t.Fatalf("attempt%d=%v", i, got)
		}
	}
	if retryDelay(6, func(d time.Duration) time.Duration { return d * 2 }) != 60*time.Second {
		t.Fatal("jitter escaped cap")
	}
	now := time.Unix(1800000000, 0)
	d := lifecycleCard("sleep", "44444444-4444-4444-8444-444444444444", "")
	s := Reconcile(TargetState{}, d, now).State
	s.ActivityID = "card"
	s.Status = "active"
	s.PendingEvent = "update"
	retry := now.Add(time.Minute)
	s.RetryAt = &retry
	end := Reconcile(s, nil, now.Add(time.Second))
	if end.Event != "end" {
		t.Fatal("stale update retry survived a newer end")
	}
	end.State.RetryAt = &retry
	if got := Reconcile(end.State, nil, now.Add(2*time.Second)); got.Event != "wait" || !got.Deadline.Equal(retry) {
		t.Fatal("end ignored rejection backoff")
	}
}

func TestLiveActivityAbsenceProofWaitDoesNotConsumeAttempt(t *testing.T) {
	now := time.Unix(1800000000, 0)
	d := lifecycleCard("sleep", "44444444-4444-4444-8444-444444444444", "")
	first := Reconcile(TargetState{}, d, now)
	s := first.State
	s.AbsenceRetryAllowed = true
	wait := Reconcile(s, d, now.Add(time.Millisecond))
	if wait.Event != "wait" || !wait.State.AbsenceRetryAllowed || wait.State.AbsenceRetryUsed {
		t.Fatal("same-second wait consumed absence proof")
	}
	retry := Reconcile(wait.State, d, now.Add(time.Second))
	if retry.Event != "start" || !retry.State.AbsenceRetryUsed || retry.State.AbsenceRetryAllowed {
		t.Fatal("proof did not authorize exactly one deferred attempt")
	}
}

func TestLiveActivityReplacementWaitsForOldEnd(t *testing.T) {
	now := time.Unix(1800000000, 0)
	d := lifecycleCard("sleep", "44444444-4444-4444-8444-444444444444", "")
	s := Reconcile(TargetState{}, d, now).State
	s.ActivityID = "old"
	s.Status = "active"
	s.Attempts = 0
	ended := Reconcile(s, nil, now.Add(time.Second))
	next := lifecycleCard("sleep", "55555555-5555-4555-8555-555555555555", "")
	recovery := Reconcile(ended.State, next, now.Add(2*time.Second))
	if recovery.Event != "end" || recovery.State.GenerationID != s.GenerationID || recovery.State.Attempts != 2 {
		t.Fatal("replacement repurposed a card whose end was unacknowledged")
	}
}
