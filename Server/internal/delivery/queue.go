package delivery

import (
	"context"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

type Queue interface {
	ScheduleFamily(context.Context, pgx.Tx, access.Grant, string, protocol.Cursor) error
}

type PendingWake struct {
	FamilyID   string
	DeviceID   string
	Cursor     protocol.Cursor
	NotBefore  time.Time
	Attempts   uint32
	Generation uint64
}

// WorkerRepository is the narrow P4.2 contract. Request-process credentials
// cannot construct an implementation in this slice.
type WorkerRepository interface {
	Claim(context.Context, time.Time, int) ([]PendingWake, error)
	Complete(context.Context, PendingWake) error
	Retry(context.Context, PendingWake, time.Time, string) error
}
