package delivery

import (
	"context"
	"errors"
	"time"

	"babytracker/server/internal/diagnostics"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/realtime"
)

type HintPublisher interface {
	Publish(context.Context, realtime.Hint)
}

// ChangeRecorder records a committed advance of a family's sequence in
// memory (wake.Tracker), for the wake worker's next tick.
type ChangeRecorder interface {
	Changed(familyID string, sequence uint64)
}

// changes fans one committed advance out to several recorders.
type changes []ChangeRecorder

func (c changes) Changed(familyID string, sequence uint64) {
	for _, recorder := range c {
		recorder.Changed(familyID, sequence)
	}
}

// Changes is one recorder that records each committed advance in every
// non-nil recorder, in order (the wake tracker, then the Live Activity
// worker's Dirty). It is nil when no recorder is non-nil. A typed nil
// pointer is not nil here: callers pass only the recorders they composed.
func Changes(recorders ...ChangeRecorder) ChangeRecorder {
	var out changes
	for _, recorder := range recorders {
		if recorder != nil {
			out = append(out, recorder)
		}
	}
	switch len(out) {
	case 0:
		return nil
	case 1:
		return out[0]
	}
	return out
}

// Observer announces a family's new high water after the write transaction
// committed: to the in-process hub, and to the wake worker's tracker. The
// write transaction schedules nothing: no wake row and no NOTIFY. Wakes come
// from the wake worker comparing each device's acknowledged cursor with the
// family sequence.
type Observer struct {
	publisher HintPublisher
	changes   ChangeRecorder
}

// NewObserver publishes to publisher and records each change in changes
// (which may be nil).
func NewObserver(publisher HintPublisher, changes ChangeRecorder) *Observer {
	return &Observer{publisher: publisher, changes: changes}
}

// Publish is post-commit best effort; the caller ignores its error because
// the committed receipts are already replayable.
func (observer *Observer) Publish(ctx context.Context, familyID string, cursor protocol.Cursor) error {
	start := time.Now()
	defer func() { diagnostics.Record(ctx, nil, diagnostics.Observer, time.Since(start)) }()
	if observer == nil || observer.publisher == nil || familyID != cursor.FamilyID {
		return errors.New("invalid commit publish")
	}
	observer.publisher.Publish(ctx, realtime.Hint{FamilyID: familyID, Cursor: cursor})
	if observer.changes != nil {
		observer.changes.Changed(familyID, cursor.Sequence)
	}
	return nil
}
