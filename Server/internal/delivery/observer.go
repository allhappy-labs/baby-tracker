package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/diagnostics"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/realtime"
	"github.com/jackc/pgx/v5"
)

type HintPublisher interface {
	Publish(context.Context, realtime.Hint)
}

type Observer struct {
	queue     Queue
	publisher HintPublisher
}

func NewObserver(queue Queue, publisher HintPublisher) *Observer {
	return &Observer{queue: queue, publisher: publisher}
}

func (observer *Observer) Schedule(ctx context.Context, tx pgx.Tx, grant access.Grant, deviceID string, cursor protocol.Cursor) error {
	if observer == nil || observer.queue == nil || observer.publisher == nil || tx == nil || cursor.FamilyID != grant.FamilyID {
		return errors.New("invalid commit observer")
	}
	if err := observer.queue.ScheduleFamily(ctx, tx, grant, deviceID, cursor); err != nil {
		return err
	}
	payload, err := json.Marshal(realtime.Hint{FamilyID: grant.FamilyID, Cursor: cursor})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `SELECT pg_notify('family_cursor_v1',$1)`, string(payload))
	return err
}

func (observer *Observer) Publish(ctx context.Context, familyID string, cursor protocol.Cursor) error {
	start := time.Now()
	defer func() { diagnostics.Record(ctx, nil, diagnostics.Observer, time.Since(start)) }()
	if observer == nil || observer.publisher == nil || familyID != cursor.FamilyID {
		return errors.New("invalid commit publish")
	}
	observer.publisher.Publish(ctx, realtime.Hint{FamilyID: familyID, Cursor: cursor})
	return nil
}
