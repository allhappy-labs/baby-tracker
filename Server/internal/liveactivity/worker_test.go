package liveactivity

import (
	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/records"
	"babytracker/server/internal/security"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testkit"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type transportFunc func(context.Context, PushRequest) (PushResult, error)

func (f transportFunc) Send(c context.Context, r PushRequest) (PushResult, error) { return f(c, r) }

func TestLiveActivityNoNetworkUnderFence(t *testing.T) {
	h := testkit.New(t)
	baby := seedBaby(t, h)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	guard, err := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	sent := []PushRequest{}
	transport := transportFunc(func(ctx context.Context, r PushRequest) (PushResult, error) {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 5*time.Second {
			t.Error("missing five-second bound")
		}
		// Real authority mutation would block if the worker retained its snapshot fence.
		_, err := h.Family.ChangeRole(ctx, h.Owner, h.CommandID(), h.Caregiver.AccountID, "viewer")
		if err != nil {
			return PushResult{}, err
		}
		mu.Lock()
		sent = append(sent, r)
		mu.Unlock()
		return PushResult{Classification: Accepted}, nil
	})
	w := NewWorker(NewQueue(h.LiveActivity), guard, transport)
	if err = w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0].Event != "start" || sent[0].Content.Remote == nil {
		t.Fatalf("sends=%+v", sent)
	}
	// Reconstruct worker after the accepted start. Missing registration is not absence.
	w = NewWorker(NewQueue(h.LiveActivity), guard, transport)
	h.RenameBaby(t, baby, "new name")
	if err = w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 {
		t.Fatal("restart blindly repeated unconfirmed start")
	}
}

func TestLiveActivityForegroundAbsenceRetry(t *testing.T) {
	h := testkit.New(t)
	baby := seedBaby(t, h)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	req := deviceRequest()
	if _, err := service.RegisterDevice(t.Context(), h.Caregiver, req); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	guard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	calls := 0
	w := NewWorker(NewQueue(h.LiveActivity), guard, transportFunc(func(context.Context, PushRequest) (PushResult, error) {
		calls++
		return PushResult{Classification: Ambiguous}, errors.New("response lost")
	}))
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	receipt, err := service.RegisterDevice(t.Context(), h.Caregiver, req)
	if err != nil || len(receipt.UnconfirmedStarts) != 1 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	start := receipt.UnconfirmedStarts[0]
	req.ForegroundInventory = &protocol.LiveActivityForegroundInventory{InventoryVersion: 1, AbsentGenerations: []protocol.LiveActivityAbsentGeneration{{GenerationID: start.GenerationID, ObservedAttemptID: start.AttemptID}}}
	if _, err = service.RegisterDevice(t.Context(), h.Caregiver, req); err != nil {
		t.Fatal(err)
	}
	// Real seconds, not a fabricated APNs timestamp. Worker projection re-runs.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	if err = w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("absence-authorized sends=%d", calls)
	}
	h.RenameBaby(t, baby, "after absence retry")
	if scalar(t, h, `SELECT count(*) FROM live_activity_delivery WHERE kind='feed' AND dirty_version>completed_version`) != 1 {
		t.Fatal("absence proof swallowed next committed care mutation")
	}
	if err = w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}

	if _, err = service.RegisterDevice(t.Context(), h.Caregiver, req); err != nil {
		t.Fatalf("identical inventory retry=%v", err)
	}
	req.ForegroundInventory.InventoryVersion = 2
	if _, err = service.RegisterDevice(t.Context(), h.Caregiver, req); err != nil {
		t.Fatal(err)
	}
	if err = w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("stale attempt authorized another start")
	}
	req.ForegroundInventory.AbsentGenerations = []protocol.LiveActivityAbsentGeneration{}
	if _, err = service.RegisterDevice(t.Context(), h.Caregiver, req); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("changed equal inventory accepted: %v", err)
	}
}

func TestLiveActivityDurableIntentSurvivesExpiredClaim(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	q := NewQueue(h.LiveActivity)
	claims, err := q.Claim(t.Context(), time.Now(), 2)
	if err != nil {
		t.Fatal(err)
	}
	var claim Claim
	for _, c := range claims {
		if c.Kind == "feed" {
			claim = c
		}
	}
	state := TargetState{GenerationID: installation, Status: "startUnconfirmed", AttemptID: otherInstallation}
	if err = q.SaveTarget(t.Context(), claim, state, false, time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = h.Migrator.Exec(t.Context(), `UPDATE live_activity_delivery SET lease_deadline=clock_timestamp()-interval '1 second' WHERE job_id=$1`, claim.JobID); err != nil {
		t.Fatal(err)
	}
	if err = q.SaveTarget(t.Context(), claim, TargetState{}, true, time.Time{}, nil); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("expired ack=%v", err)
	}
	fresh, err := q.Claim(t.Context(), time.Now(), 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range fresh {
		if c.JobID == claim.JobID {
			got, e := q.LoadTarget(t.Context(), c)
			if e != nil || got.Status != "startUnconfirmed" {
				t.Fatalf("lost intent %+v %v", got, e)
			}
			return
		}
	}
	t.Fatal("expired job not reclaimed")
}

func activityRequest(h *testkit.Harness, baby, id string) protocol.LiveActivityTokenRegistration {
	timer := testkit.TimerA
	return protocol.LiveActivityTokenRegistration{RequestMeta: deviceRequest().RequestMeta, InstallationID: installation, ActivityID: id, Token: "ccdd", TokenVersion: 1, Scope: protocol.LiveActivityScope{ServerInstanceID: h.Info.InstanceID, RecoveryEpoch: h.Info.RecoveryEpoch, FamilyID: h.FamilyID, BabyID: baby, KindRaw: "feed"}, TimerID: &timer, State: "active"}
}

func TestLiveActivityTokenRotationDuringSendAndDismissal(t *testing.T) {
	h := testkit.New(t)
	baby := seedBaby(t, h)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	req := activityRequest(h, baby, "local-card")
	first, err := service.RegisterActivity(t.Context(), h.Caregiver, req)
	if err != nil {
		t.Fatal(err)
	}
	guard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	calls := 0
	w := NewWorker(NewQueue(h.LiveActivity), guard, transportFunc(func(ctx context.Context, r PushRequest) (PushResult, error) {
		calls++
		if r.Content.Remote.GenerationID != first.Generations[0].GenerationID {
			t.Error("local generation changed")
		}
		if calls == 1 {
			req.TokenVersion = 2
			req.Token = "eeff"
			if _, err := service.RegisterActivity(ctx, h.Caregiver, req); err != nil {
				t.Errorf("registration during send: %v", err)
				return PushResult{}, err
			}
			return PushResult{Classification: InvalidToken}, nil
		}
		if r.Token != "eeff" {
			t.Error("retry did not refresh token")
		}
		return PushResult{Classification: Accepted}, nil
	}))
	if err = w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if workerTokenCount(t, h, `SELECT count(*) FROM live_activity_tokens WHERE token='eeff' AND token_version=2 AND state='active'`) != 1 {
		t.Fatalf("stale response invalidated rotated token; calls=%d", calls)
	}
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	if err = w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	req.TokenVersion = 3
	req.State = "dismissed"
	if _, err = service.RegisterActivity(t.Context(), h.Caregiver, req); err != nil {
		t.Fatal(err)
	}
	before := calls
	if err = w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	h.RenameBaby(t, baby, "new name")
	if err = w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if calls != before {
		t.Fatal("dismissed generation sent again")
	}
}

func TestLiveActivityLateTokenCanonicalReceiptAndPermissionSweep(t *testing.T) {
	h := testkit.New(t)
	baby := seedBaby(t, h)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	guard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	var sent []PushRequest
	w := NewWorker(NewQueue(h.LiveActivity), guard, transportFunc(func(_ context.Context, r PushRequest) (PushResult, error) {
		sent = append(sent, r)
		return PushResult{Classification: Accepted}, nil
	}))
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	req := activityRequest(h, baby, "late-card")
	req.KnownGenerationID = &sent[0].Content.Remote.GenerationID
	receipt, err := service.RegisterActivity(t.Context(), h.Caregiver, req)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Generations[0].GenerationID != *req.KnownGenerationID {
		t.Fatal("late registration got a different canonical generation")
	}
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	if err = w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[1].Event != "update" {
		t.Fatalf("late attachment sends=%+v", sent)
	}
	if _, err = h.Family.ChangeRole(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID, "viewer"); err != nil {
		t.Fatal(err)
	}
	// Simulate the required 60-second authority poll, with no care mutation.
	if _, err = h.Migrator.Exec(t.Context(), `UPDATE live_activity_family_dirty SET authority_checked_at=clock_timestamp()-interval '61 seconds'`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	if err = w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 3 || sent[2].Content.Remote.CanWrite {
		t.Fatalf("permission sweep sends=%+v", sent)
	}
}

func TestLiveActivityConservativeDuplicateEnd(t *testing.T) {
	h := testkit.New(t)
	baby := seedBaby(t, h)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	guard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	var sent []PushRequest
	w := NewWorker(NewQueue(h.LiveActivity), guard, transportFunc(func(_ context.Context, r PushRequest) (PushResult, error) {
		sent = append(sent, r)
		return PushResult{Classification: Accepted}, nil
	}))
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	generation := sent[0].Content.Remote.GenerationID
	for i, id := range []string{"a-card", "b-card"} {
		req := activityRequest(h, baby, id)
		if i == 1 {
			req.Token = "eeff"
		}
		req.KnownGenerationID = &generation
		if _, err := service.RegisterActivity(t.Context(), h.Caregiver, req); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[1].Event != "end" || sent[1].Token != "eeff" {
		t.Fatalf("duplicate sends=%+v", sent)
	}
	if workerTokenCount(t, h, `SELECT count(*) FROM live_activity_tokens WHERE activity_id='a-card' AND state='active'`) != 1 || workerTokenCount(t, h, `SELECT count(*) FROM live_activity_tokens WHERE activity_id='b-card' AND state='ended'`) != 1 {
		t.Fatal("duplicate cleanup retired wrong card")
	}
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 3 || sent[2].Event != "update" || sent[2].Token != "ccdd" {
		t.Fatal("duplicate end suppressed surviving card")
	}
}

func workerTokenCount(t *testing.T, h *testkit.Harness, query string) int64 {
	t.Helper()
	var n int64
	err := h.Security.WithAuthorizedTransaction(t.Context(), h.Caregiver, h.FamilyID, "read", func(tx pgx.Tx, _ access.Grant) error { return tx.QueryRow(t.Context(), query).Scan(&n) })
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLiveActivityCrashAfterSendBeforeAck(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	guard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	w := NewWorker(NewQueue(h.LiveActivity), guard, transportFunc(func(context.Context, PushRequest) (PushResult, error) {
		calls++
		cancel()
		return PushResult{Classification: Accepted}, nil
	}))
	if err := w.RunOnce(ctx, 32); err == nil {
		t.Fatal("simulated dead process acknowledged")
	}
	if _, err := h.Migrator.Exec(t.Context(), `UPDATE live_activity_delivery SET lease_deadline=clock_timestamp()-interval '1 second' WHERE lease_id IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	w = NewWorker(NewQueue(h.LiveActivity), guard, transportFunc(func(context.Context, PushRequest) (PushResult, error) {
		calls++
		return PushResult{Classification: Accepted}, nil
	}))
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("crash recovery resent start")
	}
}

func TestLiveActivityProcessWideSendBound(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	h.StartFeed(t, h.Owner, testkit.TimerA)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	for i := 1; i <= 3; i++ {
		p, err := h.NewDevice("caregiver")
		if err != nil {
			t.Fatal(err)
		}
		r := deviceRequest()
		r.InstallationID = fmt.Sprintf("70000000-0000-4000-8000-%012d", i)
		r.PushToStartToken = fmt.Sprintf("%04x", i)
		if _, err = service.RegisterDevice(t.Context(), p, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.Migrator.Exec(t.Context(), `UPDATE live_activity_delivery SET completed_version=dirty_version WHERE kind='sleep'`); err != nil {
		t.Fatal(err)
	}
	guard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	entered := make(chan PushRequest, 3)
	release := make(chan struct{})
	done := make(chan error, 3)
	transport := transportFunc(func(ctx context.Context, r PushRequest) (PushResult, error) {
		entered <- r
		select {
		case <-release:
			return PushResult{Classification: Accepted}, nil
		case <-ctx.Done():
			return PushResult{Classification: Ambiguous}, ctx.Err()
		}
	})
	for i := 0; i < 3; i++ {
		go func() { done <- NewWorker(NewQueue(h.LiveActivity), guard, transport).RunOnce(t.Context(), 32) }()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatal("two sends did not enter")
		}
	}
	select {
	case <-entered:
		close(release)
		t.Fatal("more than two sends in flight")
	case <-time.After(100 * time.Millisecond):
	}
	if n := scalar(t, h, `SELECT count(*) FROM live_activity_delivery WHERE lease_id IS NOT NULL`); n != 2 {
		t.Errorf("claimed tail while send capacity busy: %d", n)
	}
	close(release)
	for i := 0; i < 3; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestLiveActivityLateOldGenerationRetired(t *testing.T) {
	h := testkit.New(t)
	baby := seedBaby(t, h)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	guard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	var sent []PushRequest
	w := NewWorker(NewQueue(h.LiveActivity), guard, transportFunc(func(_ context.Context, r PushRequest) (PushResult, error) {
		sent = append(sent, r)
		return PushResult{Classification: Accepted}, nil
	}))
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	old := sent[0].Content.Remote.GenerationID
	workerStopFeed(t, h, baby, testkit.TimerA)
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	nextTimer := "99999999-9999-4999-8999-999999999999"
	if out := h.StartFeed(t, h.Owner, nextTimer); out.Status != "applied" {
		t.Fatalf("replacement: %+v", out)
	}
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	currentStartAttempt := sent[len(sent)-1].APNSID
	current := sent[len(sent)-1].Content.Remote.GenerationID
	if current == old {
		t.Fatal("closed generation reused")
	}
	req := activityRequest(h, baby, "delayed-old")
	req.KnownGenerationID = &old
	receipt, err := service.RegisterActivity(t.Context(), h.Caregiver, req)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Generations[0].GenerationID == current {
		t.Fatal("old token relabeled new")
	}
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	if err = w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	last := sent[len(sent)-1]
	if last.Event != "end" || last.Content.Remote.GenerationID != old {
		t.Fatalf("late old card not ended: %+v", last)
	}
	deviceReceipt, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(deviceReceipt.UnconfirmedStarts) != 1 || deviceReceipt.UnconfirmedStarts[0].AttemptID != currentStartAttempt {
		t.Fatal("old-card cleanup replaced the current start proof nonce")
	}

}

func workerStopFeed(t *testing.T, h *testkit.Harness, baby, timer string) {
	t.Helper()
	m := testkit.MustMutation(t, "timer-feed.json")
	m.MutationID = h.CommandID()
	m.EntityID = timer
	m.Operation = "timer_stop"
	m.Payload = protocol.TimerStopPayload{BabyID: baby, CompletedRecordID: "88888888-8888-4888-8888-888888888888", StoppedAt: "2026-09-20T15:10:00Z", ComponentID: "77777777-7777-4777-8777-777777777777"}
	out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil || out.Status != "applied" {
		t.Fatalf("stop=%+v err=%v", out, err)
	}
}

func TestLiveActivityStartTokenFailureDoesNotRevokeUpdateTokens(t *testing.T) {
	h := testkit.New(t)
	baby := seedBaby(t, h)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	req := activityRequest(h, baby, "independent-sleep")
	req.Scope.KindRaw = "sleep"
	req.TimerID = nil
	if _, err := service.RegisterActivity(t.Context(), h.Caregiver, req); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	guard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	w := NewWorker(NewQueue(h.LiveActivity), guard, transportFunc(func(context.Context, PushRequest) (PushResult, error) {
		return PushResult{Classification: InvalidToken}, nil
	}))
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if workerTokenCount(t, h, `SELECT count(*) FROM live_activity_devices WHERE push_token IS NULL AND NOT revoked`) != 1 {
		t.Fatal("failed start did not retire only start token")
	}
	if workerTokenCount(t, h, `SELECT count(*) FROM live_activity_tokens WHERE activity_id='independent-sleep' AND token='ccdd' AND state='active'`) != 1 {
		t.Fatal("failed start retired independent update token")
	}
}

func TestLiveActivityBreakExpiryWithoutMutation(t *testing.T) {
	h := testkit.New(t)
	baby := seedBaby(t, h)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	req := activityRequest(h, baby, "break-card")
	if _, err := service.RegisterActivity(t.Context(), h.Caregiver, req); err != nil {
		t.Fatal(err)
	}
	guard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	var sent []PushRequest
	w := NewWorker(NewQueue(h.LiveActivity), guard, transportFunc(func(_ context.Context, r PushRequest) (PushResult, error) {
		sent = append(sent, r)
		return PushResult{Classification: Accepted}, nil
	}))
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	workerStopFeed(t, h, baby, testkit.TimerA)
	// Arrange a one-second remaining break; the history reader must also run
	// through the actual read-only guard, then schedule its independent deadline.
	h.ExecFamily(t, `UPDATE feed_entries SET started_at=clock_timestamp()-interval '40 minutes',ended_at=clock_timestamp()-interval '30 minutes'+interval '2 seconds' WHERE id='88888888-8888-4888-8888-888888888888'`)
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[1].Event != "update" || sent[1].Content.Phase != "feedBreak" {
		t.Fatalf("break presentation=%+v", sent)
	}
	time.Sleep(2200 * time.Millisecond)
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 3 || sent[2].Event != "end" {
		t.Fatalf("expiry without mutation=%+v", sent)
	}
}

func TestLiveActivityFailedUpdateDoesNotRestart(t *testing.T) {
	h := testkit.New(t)
	baby := seedBaby(t, h)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	req := activityRequest(h, baby, "failed-card")
	if _, err := service.RegisterActivity(t.Context(), h.Caregiver, req); err != nil {
		t.Fatal(err)
	}
	guard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	calls := 0
	w := NewWorker(NewQueue(h.LiveActivity), guard, transportFunc(func(context.Context, PushRequest) (PushResult, error) {
		calls++
		return PushResult{Classification: InvalidToken}, nil
	}))
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("failed update restarted same generation")
	}
	if workerTokenCount(t, h, `SELECT count(*) FROM live_activity_devices WHERE push_token='aabb' AND NOT revoked`) != 1 {
		t.Fatal("failed update invalidated start token")
	}
}

func TestLiveActivityRetentionRemovesDeadTokensAndHistory(t *testing.T) {
	h := testkit.New(t)
	baby := seedBaby(t, h)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	req := activityRequest(h, baby, "dismissed")
	req.State = "dismissed"
	if _, err := service.RegisterActivity(t.Context(), h.Caregiver, req); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Migrator.Exec(t.Context(), `INSERT INTO live_activity_history(job_id,event,recorded_at) VALUES($1,'start',clock_timestamp()-interval '8 days'),($1,'update',clock_timestamp()-interval '6 days')`, installation); err != nil {
		t.Fatal(err)
	}
	if _, err := NewQueue(h.LiveActivity).Retain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if workerTokenCount(t, h, `SELECT count(*) FROM live_activity_tokens WHERE activity_id='dismissed' AND state='dismissed' AND token_version=1 AND token IS NULL`) != 1 {
		t.Fatal("dead token retained or dismissal fence lost")
	}
	if scalar(t, h, `SELECT count(*) FROM live_activity_history`) != 1 {
		t.Fatal("history retention boundary wrong")
	}
	if _, err := service.RegisterActivity(t.Context(), h.Caregiver, req); err != nil {
		t.Fatalf("identical terminal retry after cleanup=%v", err)
	}
}

func TestLiveActivityAckAndJournalLockOrder(t *testing.T) {
	h := testkit.New(t)
	baby := seedBaby(t, h)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	req := activityRequest(h, baby, "lock-card")
	if _, err := service.RegisterActivity(t.Context(), h.Caregiver, req); err != nil {
		t.Fatal(err)
	}
	claims, err := NewQueue(h.LiveActivity).Claim(t.Context(), time.Now(), 2)
	if err != nil {
		t.Fatal(err)
	}
	var claim Claim
	for _, c := range claims {
		if c.Kind == "feed" {
			claim = c
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	care, err := h.Migrator.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer care.Rollback(context.Background())
	if err = tenant.SetLocal(ctx, care, tenant.Context{AccountID: h.Owner.AccountID, FamilyID: h.FamilyID}); err != nil {
		t.Fatal(err)
	}
	var carePID int
	if err = care.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&carePID); err != nil {
		t.Fatal(err)
	}
	if _, err = care.Exec(ctx, `SELECT 1 FROM live_activity_family_dirty WHERE family_id=$1 FOR UPDATE`, h.FamilyID); err != nil {
		t.Fatal(err)
	}
	ack, err := h.LiveActivity.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ack.Rollback(context.Background())
	var ackPID int
	if err = ack.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&ackPID); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	raw, _ := json.Marshal(claim)
	go func() {
		var result []byte
		e := ack.QueryRow(ctx, `SELECT app.live_activity_target($1,'{}',true,NULL,'{"activity_id":"lock-card","version":1}',NULL)`, raw).Scan(&result)
		if e == nil {
			e = ack.Commit(ctx)
		}
		finished <- e
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var blocked bool
		if err = h.Migrator.QueryRow(ctx, `SELECT $1::integer=ANY(pg_blocking_pids($2::integer))`, carePID, ackPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ack never waited on family dirty lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err = care.Exec(ctx, `INSERT INTO mutation_journal(family_id,sequence,entity_kind,entity_id,entity_version,actor_account_id,device_id,operation,canonical_entity,accepted_at) VALUES($1,100,'baby',$2,100,$3,$4,'update','{}',now())`, h.FamilyID, baby, h.Owner.AccountID, h.Owner.DeviceID); err != nil {
		t.Fatal(err)
	}
	if err = care.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-finished; err != nil {
		t.Fatalf("ack/journal deadlock: %v", err)
	}
	if workerTokenCount(t, h, `SELECT count(*) FROM live_activity_tokens WHERE activity_id='lock-card' AND state='active'`) != 1 {
		t.Fatal("stale ack retired token after newer journal")
	}
}

func TestLiveActivityRolledBackCareDoesNotSend(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	rollback := errors.New("rollback care")
	err := h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "mutate_record", func(tx pgx.Tx, g access.Grant) error {
		mutation := testkit.MustMutation(t, "timer-feed.json")
		mutation.MutationID = h.CommandID()
		out, e := records.NewRepository().Apply(t.Context(), tx, records.ApplyCommand{Grant: g, DeviceID: h.Owner.DeviceID, Mutation: mutation, AcceptedAt: h.Now, ServerInstanceID: h.Info.InstanceID, RecoveryEpoch: h.Info.RecoveryEpoch, Limits: h.Limits})
		if e != nil {
			return e
		}
		if out.Status != "applied" {
			t.Errorf("rollback setup rejected: %+v", out)
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	guard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	w := NewWorker(NewQueue(h.LiveActivity), guard, transportFunc(func(context.Context, PushRequest) (PushResult, error) {
		t.Error("rolled-back care sent a card")
		return PushResult{Classification: Accepted}, nil
	}))
	if err = w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
}

func TestLiveActivityRunPollsWithTwoSendSlots(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	h.StartFeed(t, h.Owner, testkit.TimerA)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	for i := 1; i <= 2; i++ {
		p, err := h.NewDevice("caregiver")
		if err != nil {
			t.Fatal(err)
		}
		r := deviceRequest()
		r.InstallationID = fmt.Sprintf("70000000-0000-4000-8000-%012d", i)
		r.PushToStartToken = fmt.Sprintf("%04x", i)
		if _, err = service.RegisterDevice(t.Context(), p, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.Migrator.Exec(t.Context(), `UPDATE live_activity_delivery SET completed_version=dirty_version WHERE kind='sleep'`); err != nil {
		t.Fatal(err)
	}
	guard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	entered := make(chan struct{}, 2)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w := NewWorker(NewQueue(h.LiveActivity), guard, transportFunc(func(ctx context.Context, _ PushRequest) (PushResult, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return PushResult{Classification: Ambiguous}, ctx.Err()
	}))
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			cancel()
			<-done
			t.Fatal("poll blocked behind first send")
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown=%v", err)
	}
}

// Decorates the real guard at its public boundary; hooks run outside its fence.
type interleavedSnapshotGuard struct {
	SnapshotGuard
	beforeSecond func()
	feedReads    int
}

func (g *interleavedSnapshotGuard) WithLiveActivitySnapshot(ctx context.Context, s security.LiveActivitySubject, run func(pgx.Tx, access.Grant) error) error {
	if s.Kind == "feed" {
		g.feedReads++
		if g.feedReads == 2 && g.beforeSecond != nil {
			g.beforeSecond()
		}
	}
	return g.SnapshotGuard.WithLiveActivitySnapshot(ctx, s, run)
}

func TestLiveActivityKnownUnsentStartRecovers(t *testing.T) {
	for _, interleaving := range []string{"care_mutation", "viewer_demotion", "registration_rotation", "insufficient_lease", "revocation"} {
		t.Run(interleaving, func(t *testing.T) {
			h := testkit.New(t)
			baby := seedBaby(t, h)
			service := NewService(h.Security, familyResolver{h.Family}, topic)
			if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
				t.Fatal(err)
			}
			h.StartFeed(t, h.Owner, testkit.TimerA)
			realGuard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
			guard := &interleavedSnapshotGuard{SnapshotGuard: realGuard}
			var sent []PushRequest
			q := NewQueue(h.LiveActivity)
			w := NewWorker(q, guard, transportFunc(func(_ context.Context, r PushRequest) (PushResult, error) {
				sent = append(sent, r)
				return PushResult{Classification: Accepted}, nil
			}))
			claims, err := q.Claim(t.Context(), time.Now(), 2)
			if err != nil {
				t.Fatal(err)
			}
			var claim Claim
			for _, c := range claims {
				if c.Kind == "feed" {
					claim = c
				} else if err = q.Complete(t.Context(), c); err != nil {
					t.Fatal(err)
				}
			}
			guard.beforeSecond = func() {
				switch interleaving {
				case "care_mutation":
					h.RenameBaby(t, baby, "after intent")
				case "viewer_demotion":
					if _, e := h.Family.ChangeRole(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID, "viewer"); e != nil {
						t.Fatal(e)
					}
				case "registration_rotation":
					r := deviceRequest()
					r.RegistrationVersion = 2
					r.PushToStartToken = "eeff"
					if _, e := service.RegisterDevice(t.Context(), h.Caregiver, r); e != nil {
						t.Fatal(e)
					}
				case "insufficient_lease":
					w.Now = func() time.Time { return claim.LeaseDeadline.Add(-4 * time.Second) }
				case "revocation":
					if _, e := h.Auth.Logout(t.Context(), h.Caregiver, h.CommandID()); e != nil {
						t.Fatal(e)
					}
				}
			}
			err = w.dispatch(t.Context(), claim)
			if err != nil && !errors.Is(err, ErrStaleClaim) && !errors.Is(err, security.ErrLiveActivityAuthorizationChanged) {
				t.Fatal(err)
			}
			if len(sent) != 0 {
				t.Fatal("pre-send interleaving reached transport")
			}
			var generation, status string
			if err = h.Migrator.QueryRow(t.Context(), `SELECT target_state->>'generation_id',target_state->>'status' FROM live_activity_delivery WHERE job_id=$1`, claim.JobID).Scan(&generation, &status); err != nil {
				t.Fatal(err)
			}
			if generation == "" || status != "pending" {
				t.Fatalf("known-unsent start remained %q (generation %s)", status, generation)
			}
			if _, err = h.Migrator.Exec(t.Context(), `UPDATE live_activity_delivery SET lease_deadline=clock_timestamp()-interval '1 second' WHERE lease_id IS NOT NULL`); err != nil {
				t.Fatal(err)
			}
			w.Now = time.Now
			w.Guard = realGuard
			time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
			if err = w.RunOnce(t.Context(), 32); err != nil {
				t.Fatal(err)
			}
			if interleaving == "revocation" {
				if len(sent) != 0 {
					t.Fatal("abandonment reauthorized revoked device")
				}
				return
			}
			if len(sent) != 1 || sent[0].Event != "start" || sent[0].Content.Remote.GenerationID != generation {
				t.Fatalf("known-unsent recovery=%+v", sent)
			}
			if interleaving == "viewer_demotion" && sent[0].Content.Remote.CanWrite {
				t.Fatal("retry used stale write eligibility")
			}
			if interleaving == "care_mutation" && sent[0].Attributes.BabyName != "after intent" {
				t.Fatal("retry replayed stale presentation")
			}
			if interleaving == "registration_rotation" && sent[0].Token != "eeff" {
				t.Fatal("retry used old registration token")
			}
		})
	}
}

func TestLiveActivityProvisionalDismissalBeforeFirstWorker(t *testing.T) {
	for _, status := range []string{"dismissed", "ended"} {
		for _, activeFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/active_first_%t", status, activeFirst), func(t *testing.T) {
				h := testkit.New(t)
				baby := seedBaby(t, h)
				service := NewService(h.Security, familyResolver{h.Family}, topic)
				if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
					t.Fatal(err)
				}
				h.StartFeed(t, h.Owner, testkit.TimerA)
				req := activityRequest(h, baby, "provisional-card")
				if activeFirst {
					if _, err := service.RegisterActivity(t.Context(), h.Caregiver, req); err != nil {
						t.Fatal(err)
					}
					req.TokenVersion++
				}
				req.State = status
				receipt, err := service.RegisterActivity(t.Context(), h.Caregiver, req)
				if err != nil {
					t.Fatal(err)
				}
				guard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
				calls := 0
				w := NewWorker(NewQueue(h.LiveActivity), guard, transportFunc(func(context.Context, PushRequest) (PushResult, error) {
					calls++
					return PushResult{Classification: Accepted}, nil
				}))
				if err = w.RunOnce(t.Context(), 32); err != nil {
					t.Fatal(err)
				}
				h.RenameBaby(t, baby, "after dismissal")
				if err = w.RunOnce(t.Context(), 32); err != nil {
					t.Fatal(err)
				}
				if calls != 0 {
					t.Fatal("provisional terminal card was replaced by server start")
				}
				var generation, dismissed string
				if err = h.Migrator.QueryRow(t.Context(), `SELECT target_state->>'generation_id',target_state->>'dismissed_generation' FROM live_activity_delivery WHERE kind='feed'`).Scan(&generation, &dismissed); err != nil {
					t.Fatal(err)
				}
				if generation != receipt.Generations[0].GenerationID || dismissed != generation {
					t.Fatal("provisional lifecycle dismissal fence lost")
				}
			})
		}
	}
}

func TestLiveActivityAbandonStartFences(t *testing.T) {
	for _, interleaving := range []string{"newer_dirty", "reclaimed_lease", "newer_attempt", "wrong_generation", "wrong_registration_version", "expired_original_lease", "absence_retry"} {
		t.Run(interleaving, func(t *testing.T) {
			h := testkit.New(t)
			baby := seedBaby(t, h)
			service := NewService(h.Security, familyResolver{h.Family}, topic)
			if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
				t.Fatal(err)
			}
			h.StartFeed(t, h.Owner, testkit.TimerA)
			q := NewQueue(h.LiveActivity)
			claims, err := q.Claim(t.Context(), time.Now(), 2)
			if err != nil {
				t.Fatal(err)
			}
			var claim Claim
			for _, c := range claims {
				if c.Kind == "feed" {
					claim = c
				} else if err = q.Complete(t.Context(), c); err != nil {
					t.Fatal(err)
				}
			}
			desired := lifecycleCard("feed", testkit.TimerA, "")
			previous := TargetState{}
			if interleaving == "absence_retry" {
				previous = Reconcile(previous, desired, time.Now().Add(-time.Minute)).State
				previous.AbsenceRetryAllowed = true
			}
			intent := Reconcile(previous, desired, time.Now()).State
			if err = q.SaveTarget(t.Context(), claim, intent, false, time.Time{}, nil); err != nil {
				t.Fatal(err)
			}
			attempted, captured := intent, claim
			reject := false
			switch interleaving {
			case "newer_dirty":
				h.RenameBaby(t, baby, "newer work")
			case "reclaimed_lease":
				reclaimed, e := q.Claim(t.Context(), claim.LeaseDeadline.Add(time.Second), 2)
				if e != nil || len(reclaimed) == 0 {
					t.Fatalf("reclaim=%v %v", reclaimed, e)
				}
				reject = true
			case "newer_attempt":
				intent.AttemptID = deterministicID("newer attempt")
				intent.StartAttemptID = intent.AttemptID
				if err = q.SaveTarget(t.Context(), claim, intent, false, time.Time{}, nil); err != nil {
					t.Fatal(err)
				}
				reject = true
			case "wrong_generation":
				attempted.GenerationID = deterministicID("wrong generation")
				reject = true
			case "wrong_registration_version":
				captured.RegistrationVersion++
				reject = true
			case "expired_original_lease":
				// Preserve the exact persisted lease while advancing its observed expiry.
				// Abandonment does not authorize I/O, so an unreclaimed expired lease is safe.
				claim.LeaseDeadline = time.Now().Add(-time.Second).Truncate(time.Microsecond)
				captured = claim
				if _, err = h.Migrator.Exec(t.Context(), `UPDATE live_activity_delivery SET lease_deadline=$1 WHERE job_id=$2`, claim.LeaseDeadline, claim.JobID); err != nil {
					t.Fatal(err)
				}
			}
			var dirty, completed uint64
			var before []byte
			if err = h.Migrator.QueryRow(t.Context(), `SELECT dirty_version,completed_version,target_state FROM live_activity_delivery WHERE job_id=$1`, claim.JobID).Scan(&dirty, &completed, &before); err != nil {
				t.Fatal(err)
			}
			err = q.AbandonStart(t.Context(), captured, attempted, previous)
			if reject && !errors.Is(err, ErrStaleClaim) {
				t.Fatalf("stale abandonment=%v", err)
			}
			if !reject && err != nil {
				t.Fatal(err)
			}
			var after []byte
			var afterDirty, afterCompleted uint64
			if err = h.Migrator.QueryRow(t.Context(), `SELECT dirty_version,completed_version,target_state FROM live_activity_delivery WHERE job_id=$1`, claim.JobID).Scan(&afterDirty, &afterCompleted, &after); err != nil {
				t.Fatal(err)
			}
			if afterDirty != dirty || afterCompleted != completed || dirty <= completed {
				t.Fatal("abandonment consumed pending/newer dirty work")
			}
			if reject {
				if string(before) != string(after) {
					t.Fatal("stale abandonment replaced a newer intent")
				}
				return
			}
			var state TargetState
			if err = json.Unmarshal(after, &state); err != nil {
				t.Fatal(err)
			}
			if state.Status != "pending" || state.GenerationID != intent.GenerationID || state.PendingEvent != "" || state.StartAttemptID != "" {
				t.Fatalf("abandoned=%+v", state)
			}
			if state.AbsenceRetryAllowed != previous.AbsenceRetryAllowed || state.AbsenceRetryUsed != previous.AbsenceRetryUsed {
				t.Fatal("known-unsent attempt consumed absence authorization")
			}
			next := Reconcile(state, desired, time.Unix(state.LastTimestamp+1, 0))
			if next.Event != "start" || next.State.AttemptID == intent.AttemptID {
				t.Fatal("new attempt reused abandoned nonce or failed to retry")
			}
		})
	}
}

// Every request shape the worker builds must survive the production transport's
// own local validation and encoding. A recording Transport sees requests before
// that gate, which is how updates and ends without an expiration went unsent.
func TestLiveActivityWorkerRequestsPassAPNsValidation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	claim := Claim{JobID: installation, LeaseID: installation, RegistrationID: installation, RegistrationVersion: 1, AccountID: installation, DeviceID: installation, FamilyID: "22222222-2222-4222-8222-222222222222", BabyID: "33333333-3333-4333-8333-333333333333", Kind: "feed", InstanceID: "11111111-1111-4111-8111-111111111111", RecoveryEpoch: 1, DirtyVersion: 1}
	feed := lifecycleCard("feed", testkit.TimerA, "")
	feed.BabyName = "Mira"
	feed.Content.ActivityKind = "breastLeft"
	edited := *feed
	edited.Content.StartedAt = 50
	breakCard := lifecycleCard("feed", "", "88888888-8888-4888-8888-888888888888")
	breakCard.BabyName = "Mira"
	breakCard.Phase = "feedBreak"
	breakCard.MayStart = false
	breakCard.Content = ContentState{StartedAt: 100, ActivityKind: "breastLeft", Phase: "feedBreak", Parts: []PartLabel{{Side: ptr("left"), Seconds: 600}}}
	breakEnds := now.Add(25 * time.Minute)
	breakCard.StaleAt, breakCard.ExpiresAt = &breakEnds, &breakEnds
	expired := *breakCard
	past := now.Add(-time.Second)
	expired.ExpiresAt = &past
	sleep := lifecycleCard("sleep", testkit.TimerB, "")
	sleep.BabyName = "Mira"
	nextFeed := lifecycleCard("feed", "99999999-9999-4999-8999-999999999999", "")
	nextFeed.BabyName = "Mira"
	nextFeed.Content.ActivityKind = "breastRight"
	active := func(desired *DesiredCard) TargetState {
		s := Reconcile(TargetState{}, desired, now.Add(-time.Minute)).State
		s.Status, s.PendingEvent, s.ActivityID, s.TokenVersion, s.Attempts = "active", "", "card", 1, 0
		return s
	}
	running := active(feed)
	// The snapshot links a stopped timer to its completed record's session.
	linked := running
	linked.SessionID = breakCard.SessionID
	brk := Reconcile(linked, breakCard, now.Add(-30*time.Second)).State
	brk.PendingEvent, brk.Attempts = "", 0
	duplicate := Reconcile(running, feed, now).State
	duplicate.PendingEvent, duplicate.Attempts, duplicate.LastTimestamp = "duplicateEnd", 1, now.Unix()
	duplicate.PresentationVersion++
	duplicate.AttemptID = deterministicID(duplicate.GenerationID + "/duplicate/b-card")
	cases := []struct {
		name, event, kind string
		previous          TargetState
		desired           *DesiredCard
		decided           *Decision
		wantExpiry        time.Time
	}{
		{name: "running feed push-to-start", event: "start", kind: "feed", desired: feed},
		{name: "running feed update", event: "update", kind: "feed", previous: running, desired: &edited, wantExpiry: now.Add(8 * time.Hour)},
		{name: "feed break update", event: "update", kind: "feed", previous: linked, desired: breakCard, wantExpiry: breakEnds},
		{name: "break expiry end", event: "end", kind: "feed", previous: brk, desired: &expired, wantExpiry: now.Add(8 * time.Hour)},
		{name: "sleep stop end", event: "end", kind: "sleep", previous: active(sleep), wantExpiry: now.Add(8 * time.Hour)},
		{name: "replaced lifecycle end", event: "end", kind: "feed", previous: running, desired: nextFeed, wantExpiry: now.Add(8 * time.Hour)},
		{name: "duplicate end", event: "end", kind: "feed", decided: &Decision{"end", duplicate, now.Add(time.Second)}, desired: feed, wantExpiry: now.Add(8 * time.Hour)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := claim
			c.Kind = tc.kind
			d := Reconcile(tc.previous, tc.desired, now)
			if tc.decided != nil {
				d = *tc.decided
			}
			if d.Event != tc.event {
				t.Fatalf("decision=%s want %s", d.Event, tc.event)
			}
			token := "ccdd"
			if d.Event == "start" {
				token = "aabb"
			}
			desired := tc.desired
			if desired != nil && desired.ExpiresAt != nil && !now.Before(*desired.ExpiresAt) {
				desired = nil
			}
			req := makeRequest(c, d, dispatchSnapshot{desired: desired, environment: "sandbox", token: token})
			if err := validatePushRequest(req); err != nil {
				t.Fatalf("worker-built %s rejected by APNs validation: expiration=%v", req.Event, req.Expiration)
			}
			if _, err := encodePushPayload(req); err != nil {
				t.Fatalf("encode: %v", err)
			}
			if !req.Expiration.Equal(tc.wantExpiry) {
				t.Fatalf("expiration=%v want %v", req.Expiration, tc.wantExpiry)
			}
		})
	}
}

// recordingAPNs is the production APNSTransport against a local HTTP/2 TLS
// endpoint, so the worker's requests cross the real validation, encoding and
// header path before anything is recorded.
type recordingAPNs struct {
	*APNSTransport
	mu       sync.Mutex
	results  []string
	arrivals []string
}

func newRecordingAPNs(t *testing.T) *recordingAPNs {
	t.Helper()
	r := &recordingAPNs{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var payload struct {
			APS map[string]any `json:"aps"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Error(err)
		}
		event, _ := payload.APS["event"].(string)
		r.mu.Lock()
		r.arrivals = append(r.arrivals, event+" expiration="+req.Header.Get("apns-expiration"))
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	r.APNSTransport = newAPNSTransport("TEAM123456", "KEY1234567", "com.example.baby", testAPNSKey(t), server.Client(), server.URL, server.URL)
	return r
}

func (r *recordingAPNs) Send(ctx context.Context, req PushRequest) (PushResult, error) {
	result, err := r.APNSTransport.Send(ctx, req)
	r.mu.Lock()
	r.results = append(r.results, fmt.Sprintf("%s %s/%s %s err=%v", req.Event, req.Attributes.KindRaw, req.Content.Phase, result.Classification, err))
	r.mu.Unlock()
	return result, err
}

func (r *recordingAPNs) snapshot() ([]string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.results...), append([]string(nil), r.arrivals...)
}

func TestLiveActivityWorkerDeliversEveryEventThroughAPNsTransport(t *testing.T) {
	h := testkit.New(t)
	baby := seedBaby(t, h)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	guard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	apns := newRecordingAPNs(t)
	w := NewWorker(NewQueue(h.LiveActivity), guard, apns)
	nextSecond := func() { time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second))) }
	step := func(want int, label string) {
		t.Helper()
		nextSecond()
		if err := w.RunOnce(t.Context(), 32); err != nil {
			t.Fatal(err)
		}
		results, arrivals := apns.snapshot()
		if len(results) != want || len(arrivals) != want {
			t.Fatalf("%s: transport results=%v arrivals=%v", label, results, arrivals)
		}
		for _, result := range results {
			if !strings.Contains(result, " accepted err=<nil>") {
				t.Fatalf("%s: worker-built request not accepted by APNs transport: %v", label, results)
			}
		}
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	step(1, "running feed push-to-start")
	results, _ := apns.snapshot()
	if !strings.HasPrefix(results[0], "start feed/running") {
		t.Fatalf("start=%v", results)
	}
	var generation string
	if err := h.Migrator.QueryRow(t.Context(), `SELECT target_state->>'generation_id' FROM live_activity_delivery WHERE kind='feed'`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"a-card", "b-card"} {
		req := activityRequest(h, baby, id)
		if i == 1 {
			req.Token = "eeff"
		}
		req.KnownGenerationID = &generation
		if _, err := service.RegisterActivity(t.Context(), h.Caregiver, req); err != nil {
			t.Fatal(err)
		}
	}
	step(2, "duplicate end")
	step(3, "running feed confirmation update")
	workerStopFeed(t, h, baby, testkit.TimerA)
	h.ExecFamily(t, `UPDATE feed_entries SET started_at=clock_timestamp()-interval '40 minutes',ended_at=clock_timestamp()-interval '30 minutes'+interval '3 seconds' WHERE id='88888888-8888-4888-8888-888888888888'`)
	step(4, "feed break update")
	time.Sleep(3 * time.Second)
	step(5, "break expiry end")
	sleep := testkit.MustMutation(t, "timer-sleep.json")
	sleep.MutationID = h.CommandID()
	sleep.EntityID = testkit.TimerB
	if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, sleep); err != nil || out.Status != "applied" {
		t.Fatalf("sleep start=%+v err=%v", out, err)
	}
	step(6, "sleep push-to-start")
	if err := h.Migrator.QueryRow(t.Context(), `SELECT target_state->>'generation_id' FROM live_activity_delivery WHERE kind='sleep'`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	sleepCard := activityRequest(h, baby, "sleep-card")
	sleepCard.Scope.KindRaw = "sleep"
	timer := testkit.TimerB
	sleepCard.TimerID = &timer
	sleepCard.KnownGenerationID = &generation
	if _, err := service.RegisterActivity(t.Context(), h.Caregiver, sleepCard); err != nil {
		t.Fatal(err)
	}
	step(7, "sleep confirmation update")
	stop := testkit.MustMutation(t, "timer-sleep.json")
	stop.MutationID, stop.EntityID, stop.Operation = h.CommandID(), testkit.TimerB, "timer_stop"
	stop.Payload = protocol.TimerStopPayload{BabyID: baby, CompletedRecordID: "86666666-6666-4666-8666-666666666666", StoppedAt: "2026-09-20T17:00:00Z"}
	if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, stop); err != nil || out.Status != "applied" {
		t.Fatalf("sleep stop=%+v err=%v", out, err)
	}
	step(8, "sleep stop end")
	results, arrivals := apns.snapshot()
	want := []string{"start feed/running", "end feed/", "update feed/running", "update feed/feedBreak", "end feed/", "start sleep/running", "update sleep/running", "end sleep/"}
	for i, prefix := range want {
		if !strings.HasPrefix(results[i], prefix) {
			t.Fatalf("event %d=%q want %q; all=%v", i, results[i], prefix, results)
		}
		expiry := strings.TrimPrefix(arrivals[i][strings.Index(arrivals[i], "expiration="):], "expiration=")
		if (strings.HasPrefix(prefix, "start") && expiry != "0") || (!strings.HasPrefix(prefix, "start") && (expiry == "0" || expiry == "")) {
			t.Fatalf("APNs expiration for %q=%q", prefix, expiry)
		}
	}
}

// A phone switching sides from a first part (no session yet) sends the stop and
// the next part, in that record's session, in one sync. That is a continuation:
// the card keeps its generation and receives an update, never end + start.
func TestLiveActivityFirstPartSideSwitchContinuesGeneration(t *testing.T) {
	const record = "88888888-8888-4888-8888-888888888888"
	const next = "57000000-0000-4000-8000-000000000001"
	for _, tc := range []struct {
		name    string
		session *string
	}{{"same_session_continuation", ptr(record)}, {"new_feed_without_session", nil}, {"new_feed_other_session", ptr("58000000-0000-4000-8000-000000000001")}} {
		t.Run(tc.name, func(t *testing.T) {
			h := testkit.New(t)
			baby := seedBaby(t, h)
			service := NewService(h.Security, familyResolver{h.Family}, topic)
			if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
				t.Fatal(err)
			}
			h.StartFeed(t, h.Owner, testkit.TimerA)
			guard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
			var sent []PushRequest
			w := NewWorker(NewQueue(h.LiveActivity), guard, transportFunc(func(_ context.Context, r PushRequest) (PushResult, error) {
				sent = append(sent, r)
				return PushResult{Classification: Accepted}, nil
			}))
			run := func() {
				t.Helper()
				time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
				if err := w.RunOnce(t.Context(), 32); err != nil {
					t.Fatal(err)
				}
			}
			run()
			if len(sent) != 1 || sent[0].Event != "start" {
				t.Fatalf("first part start=%+v", sent)
			}
			generation := sent[0].Content.Remote.GenerationID
			card := activityRequest(h, baby, "b-card")
			card.KnownGenerationID = &generation
			if _, err := service.RegisterActivity(t.Context(), h.Caregiver, card); err != nil {
				t.Fatal(err)
			}
			run()
			if len(sent) != 2 || sent[1].Event != "update" {
				t.Fatalf("confirmation=%+v", sent)
			}
			// One sync: the left part stops into a record, the right part starts.
			workerStopFeed(t, h, baby, testkit.TimerA)
			right := testkit.MustMutation(t, "timer-feed.json")
			p := right.Payload.(protocol.TimerStartPayload)
			p.StartedAt, p.CurrentSide, p.CurrentSideStartedAt, p.SessionID = "2026-09-20T15:10:00Z", ptr("right"), ptr("2026-09-20T15:10:00Z"), tc.session
			right.MutationID, right.EntityID, right.Payload = h.CommandID(), next, p
			if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, right); err != nil || out.Status != "applied" {
				t.Fatalf("right part=%+v err=%v", out, err)
			}
			run()
			run()
			switched := sent[2:]
			if tc.session != nil && *tc.session == record {
				if len(switched) != 1 || switched[0].Event != "update" {
					t.Fatalf("side switch replaced the card: %s", describePushes(switched))
				}
				remote := switched[0].Content.Remote
				if remote.GenerationID != generation || !referenceEqual(remote.TimerID, ptr(next)) || !referenceEqual(remote.SessionID, ptr(record)) || switched[0].Content.ActivityKind != "breastRight" || switched[0].Token != "ccdd" {
					t.Fatalf("continuation update=%+v remote=%+v", switched[0], remote)
				}
				return
			}
			if len(switched) != 2 || switched[0].Event != "end" || switched[0].Content.Remote.GenerationID != generation || switched[1].Event != "start" || switched[1].Content.Remote.GenerationID == generation {
				t.Fatalf("new feed did not get a new generation: %s", describePushes(switched))
			}
		})
	}
}

func describePushes(sent []PushRequest) string {
	out := make([]string, 0, len(sent))
	for _, s := range sent {
		out = append(out, fmt.Sprintf("{%s %s token=%s kind=%s gen=%s}", s.Event, s.Attributes.KindRaw, s.Token, s.Content.ActivityKind, s.Content.Remote.GenerationID))
	}
	return strings.Join(out, " ")
}

// A family re-dirty while another runner holds a job's lease makes that lease's
// acknowledgement stale. The newer work must be reclaimed within a few one-second
// polls, not after the 30-second lease, with both send bounds intact.
func TestLiveActivityStaleLeaseRedirtyDeliversPromptly(t *testing.T) {
	h := testkit.New(t)
	baby := seedBaby(t, h)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	h.StartFeed(t, h.Owner, testkit.TimerA)
	second, err := h.NewDevice("caregiver")
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range []access.Principal{h.Caregiver, second} {
		device := deviceRequest()
		card := activityRequest(h, baby, fmt.Sprintf("card-%d", i))
		if i == 1 {
			device.InstallationID, device.PushToStartToken = otherInstallation, "bbcc"
			card.InstallationID, card.Token = otherInstallation, "ddee"
		}
		if _, err = service.RegisterDevice(t.Context(), p, device); err != nil {
			t.Fatal(err)
		}
		if _, err = service.RegisterActivity(t.Context(), p, card); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = h.Migrator.Exec(t.Context(), `UPDATE live_activity_delivery SET completed_version=dirty_version WHERE kind='sleep'`); err != nil {
		t.Fatal(err)
	}
	guard, _ := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	var mu sync.Mutex
	inflight, perTarget := 0, map[string]int{}
	maxInflight, maxPerTarget := 0, 0
	renamed := map[string]time.Time{}
	entered := make(chan string, 2)
	release := make(chan struct{})
	blocked := 0
	transport := transportFunc(func(ctx context.Context, r PushRequest) (PushResult, error) {
		mu.Lock()
		inflight++
		perTarget[r.Token]++
		maxInflight = max(maxInflight, inflight)
		maxPerTarget = max(maxPerTarget, perTarget[r.Token])
		hold := blocked < 2
		if hold {
			blocked++
		}
		if r.Attributes.BabyName == "renamed during send" {
			if _, seen := renamed[r.Token]; !seen {
				renamed[r.Token] = time.Now()
			}
		}
		mu.Unlock()
		defer func() {
			mu.Lock()
			inflight--
			perTarget[r.Token]--
			mu.Unlock()
		}()
		if hold {
			entered <- r.Token
			select {
			case <-release:
			case <-ctx.Done():
				return PushResult{Classification: Ambiguous}, ctx.Err()
			}
		}
		return PushResult{Classification: Accepted}, nil
	})
	w := NewWorker(NewQueue(h.LiveActivity), guard, transport)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("both leased sends did not enter")
		}
	}
	// Both jobs are leased with a send in flight; this commit re-dirties them.
	h.RenameBaby(t, baby, "renamed during send")
	released := time.Now()
	close(release)
	deadline := time.Now().Add(12 * time.Second)
	for {
		mu.Lock()
		got := len(renamed)
		mu.Unlock()
		if got == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("re-dirtied leased jobs not redelivered within a few polls (lease is 30s): delivered=%v", renamed)
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	for token, at := range renamed {
		if at.Sub(released) > 5*time.Second {
			t.Fatalf("%s redelivered after %v", token, at.Sub(released))
		}
	}
	if maxInflight > 2 || maxPerTarget > 1 {
		t.Fatalf("send bounds: process=%d per-target=%d", maxInflight, maxPerTarget)
	}
}

// The release is an exact-lease CAS that only applies when newer dirty work has
// made the lease unacknowledgeable. It never edits lifecycle intent or completes
// work, so an attempt that entered transport stays start-unconfirmed.
func TestLiveActivityReleaseStaleLeaseFences(t *testing.T) {
	for _, interleaving := range []string{"newer_dirty", "current_dirty", "reclaimed_lease", "wrong_registration_version", "wrong_lease", "expired_original_lease"} {
		t.Run(interleaving, func(t *testing.T) {
			h := testkit.New(t)
			baby := seedBaby(t, h)
			service := NewService(h.Security, familyResolver{h.Family}, topic)
			if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
				t.Fatal(err)
			}
			h.StartFeed(t, h.Owner, testkit.TimerA)
			q := NewQueue(h.LiveActivity)
			claims, err := q.Claim(t.Context(), time.Now(), 2)
			if err != nil {
				t.Fatal(err)
			}
			var claim Claim
			for _, c := range claims {
				if c.Kind == "feed" {
					claim = c
				} else if err = q.Complete(t.Context(), c); err != nil {
					t.Fatal(err)
				}
			}
			// Durable intent of a start that entered transport.
			intent := Reconcile(TargetState{}, lifecycleCard("feed", testkit.TimerA, ""), time.Now()).State
			if err = q.SaveTarget(t.Context(), claim, intent, false, time.Time{}, nil); err != nil {
				t.Fatal(err)
			}
			captured := claim
			released := true
			switch interleaving {
			case "newer_dirty":
				h.RenameBaby(t, baby, "newer work")
			case "current_dirty":
				released = false
			case "reclaimed_lease":
				h.RenameBaby(t, baby, "newer work")
				reclaimed, e := q.Claim(t.Context(), claim.LeaseDeadline.Add(time.Second), 2)
				if e != nil || len(reclaimed) == 0 {
					t.Fatalf("reclaim=%v %v", reclaimed, e)
				}
				released = false
			case "wrong_registration_version":
				h.RenameBaby(t, baby, "newer work")
				captured.RegistrationVersion++
				released = false
			case "wrong_lease":
				h.RenameBaby(t, baby, "newer work")
				captured.LeaseID = otherInstallation
				released = false
			case "expired_original_lease":
				h.RenameBaby(t, baby, "newer work")
				claim.LeaseDeadline = time.Now().Add(-time.Second).Truncate(time.Microsecond)
				captured = claim
				if _, err = h.Migrator.Exec(t.Context(), `UPDATE live_activity_delivery SET lease_deadline=$1 WHERE job_id=$2`, claim.LeaseDeadline, claim.JobID); err != nil {
					t.Fatal(err)
				}
			}
			type row struct {
				dirty, completed uint64
				notBefore        time.Time
				lease            *string
				state            string
			}
			read := func() row {
				var r row
				if e := h.Migrator.QueryRow(t.Context(), `SELECT dirty_version,completed_version,not_before,lease_id::text,target_state::text FROM live_activity_delivery WHERE job_id=$1`, claim.JobID).Scan(&r.dirty, &r.completed, &r.notBefore, &r.lease, &r.state); e != nil {
					t.Fatal(e)
				}
				return r
			}
			before := read()
			raw, _ := json.Marshal(captured)
			if _, e := h.Runtime.Exec(t.Context(), `SELECT app.release_stale_live_activity_lease($1)`, raw); e == nil {
				t.Fatal("runtime role released a worker lease")
			}
			var accepted bool
			if err = h.LiveActivity.QueryRow(t.Context(), `SELECT app.release_stale_live_activity_lease($1)`, raw).Scan(&accepted); err != nil {
				t.Fatal(err)
			}
			after := read()
			if accepted != released {
				t.Fatalf("release accepted=%v want %v", accepted, released)
			}
			if after.dirty != before.dirty || after.completed != before.completed || !after.notBefore.Equal(before.notBefore) || after.state != before.state {
				t.Fatalf("release changed work or intent: before=%+v after=%+v", before, after)
			}
			if !released {
				if !referenceEqual(after.lease, before.lease) {
					t.Fatal("rejected release changed the lease")
				}
				return
			}
			if after.lease != nil || after.dirty <= after.completed {
				t.Fatalf("stale lease not released or newer work consumed: %+v", after)
			}
			// The re-dirty also made the (completed) sleep job due again.
			fresh, err := q.Claim(t.Context(), time.Now(), 2)
			var reclaimed *Claim
			for i := range fresh {
				if fresh[i].JobID == claim.JobID {
					reclaimed = &fresh[i]
				}
			}
			if err != nil || reclaimed == nil || reclaimed.LeaseID == claim.LeaseID {
				t.Fatalf("released job not reclaimable at once: %+v %v", fresh, err)
			}
			state, err := q.LoadTarget(t.Context(), *reclaimed)
			if err != nil || state.Status != "startUnconfirmed" || state.AttemptID != intent.AttemptID {
				t.Fatalf("ambiguous start lost across release: %+v %v", state, err)
			}
			if next := Reconcile(state, lifecycleCard("feed", testkit.TimerA, ""), time.Now().Add(time.Minute)); next.Event != "wait" {
				t.Fatalf("released ambiguous start was resent: %s", next.Event)
			}
		})
	}
}
