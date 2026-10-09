package delivery

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/realtime"
	"babytracker/server/internal/records"
	familysync "babytracker/server/internal/sync"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type hintPublisher struct{ hints []realtime.Hint }

func (publisher *hintPublisher) Publish(_ context.Context, hint realtime.Hint) {
	publisher.hints = append(publisher.hints, hint)
}

type observerResolver struct{ familyID string }

func (resolver observerResolver) Current(context.Context, access.Principal) (string, error) {
	return resolver.familyID, nil
}

func TestObserverSchedulesNotifiesInTransactionAndPublishesAfterCommit(t *testing.T) {
	h := testkit.NewHosted(t)
	listener, err := h.Runtime.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Release()
	if _, err = listener.Exec(t.Context(), `LISTEN family_cursor_v1`); err != nil {
		t.Fatal(err)
	}
	publisher := &hintPublisher{}
	observer := NewObserver(NewRepository(true), publisher)
	service := familysync.NewServiceWithResolver(h.Security, records.NewRepository(), h.Info, h.Limits, func() time.Time { return h.Now }, observerResolver{h.FamilyID}, observer)
	outcome, err := service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json"))
	if err != nil || outcome.Cursor == nil {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	wait, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	notification, err := listener.Conn().WaitForNotification(wait)
	if err != nil {
		t.Fatal(err)
	}
	hint, err := realtime.DecodeNotification([]byte(notification.Payload), h.Info.InstanceID, h.Info.RecoveryEpoch, map[string]struct{}{h.FamilyID: {}})
	if err != nil || hint.Cursor != *outcome.Cursor {
		t.Fatalf("notification=%q hint=%+v err=%v", notification.Payload, hint, err)
	}
	if len(publisher.hints) != 1 || publisher.hints[0].Cursor != *outcome.Cursor {
		t.Fatalf("published=%+v", publisher.hints)
	}
	pending := h.PendingWakes(t)
	if len(pending) != 1 || pending[0].DeviceID != h.Caregiver.DeviceID || pending[0].Cursor != *outcome.Cursor {
		t.Fatalf("pending=%+v", pending)
	}
}

type rollbackObserver struct{ inner *Observer }

func (observer rollbackObserver) Schedule(ctx context.Context, tx pgx.Tx, grant access.Grant, deviceID string, cursor protocol.Cursor) error {
	if err := observer.inner.Schedule(ctx, tx, grant, deviceID, cursor); err != nil {
		return err
	}
	return errors.New("rollback after schedule")
}
func (observer rollbackObserver) Publish(context.Context, string, protocol.Cursor) error { return nil }

func TestObserverRollbackLeavesNoWakeOrNotification(t *testing.T) {
	h := testkit.NewHosted(t)
	listener, err := h.Runtime.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Release()
	if _, err = listener.Exec(t.Context(), `LISTEN family_cursor_v1`); err != nil {
		t.Fatal(err)
	}
	observer := rollbackObserver{inner: NewObserver(NewRepository(true), &hintPublisher{})}
	service := familysync.NewServiceWithResolver(h.Security, records.NewRepository(), h.Info, h.Limits, func() time.Time { return h.Now }, observerResolver{h.FamilyID}, observer)
	if _, err = service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json")); err == nil {
		t.Fatal("rollback observer accepted mutation")
	}
	if pending := h.PendingWakes(t); len(pending) != 0 {
		t.Fatalf("pending=%+v", pending)
	}
	wait, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if notification, waitErr := listener.Conn().WaitForNotification(wait); waitErr == nil || notification != nil {
		t.Fatalf("rollback delivered notification=%+v err=%v", notification, waitErr)
	}
}

func TestHostedQueueRequiresProjectedFamilyEntitlement(t *testing.T) {
	h := testkit.New(t)
	publisher := &hintPublisher{}
	observer := NewObserver(NewRepository(true), publisher)
	service := familysync.NewServiceWithResolver(h.Security, records.NewRepository(), h.Info, h.Limits, func() time.Time { return h.Now }, observerResolver{h.FamilyID}, observer)
	_, err := service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json"))
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) || databaseError.Code != "42501" {
		t.Fatalf("error=%T %v want schedule authorization denial", err, err)
	}
	if len(publisher.hints) != 0 {
		t.Fatalf("post-commit publish ran after schedule denial: %+v", publisher.hints)
	}
}

func TestCommitBeforePublishSurvivesRestart(t *testing.T) {
	h := testkit.NewHosted(t)
	h.SuppressHintPublisher(t)
	applied, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json"))
	if err != nil || applied.Status != "applied" || applied.Cursor == nil {
		t.Fatalf("apply=%+v err=%v", applied, err)
	}
	h.RestartServices(t)
	page := h.Pull(t, h.Caregiver, nil, nil)
	if len(page.Changes) != 1 || page.HighWater != *applied.Cursor {
		t.Fatalf("page=%+v", page)
	}
	pending := h.PendingWakes(t)
	if len(pending) != 1 || pending[0].DeviceID != h.Caregiver.DeviceID || pending[0].Cursor != *applied.Cursor {
		t.Fatalf("pending=%+v", pending)
	}
}

func TestWakeCoalescesLatestGeneration(t *testing.T) {
	h := testkit.NewHosted(t)
	var last *protocol.Cursor
	for index, fixture := range []string{"baby.json", "feed-bottle.json", "sleep.json"} {
		mutation := testkit.MustMutation(t, fixture)
		mutation.MutationID = h.CommandID()
		if index > 0 {
			mutation.EntityID = mutation.EntityID[:24] + fmt.Sprintf("%012d", index+1)
		}
		outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, mutation)
		if err != nil || outcome.Cursor == nil {
			t.Fatalf("apply %d=%+v err=%v", index, outcome, err)
		}
		last = outcome.Cursor
	}
	pending := h.PendingWakes(t)
	if len(pending) != 1 || pending[0].Cursor != *last || pending[0].Generation != 3 {
		t.Fatalf("pending=%+v last=%+v", pending, last)
	}
}
