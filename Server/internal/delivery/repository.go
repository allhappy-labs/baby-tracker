package delivery

import (
	"context"
	"errors"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

type Repository struct{ hosted bool }

func NewRepository(hosted bool) *Repository { return &Repository{hosted: hosted} }

func (repository *Repository) ScheduleFamily(ctx context.Context, tx pgx.Tx, grant access.Grant, actorDeviceID string, cursor protocol.Cursor) error {
	if repository == nil || tx == nil || !protocol.ValidUUID(grant.AccountID) || !protocol.ValidUUID(grant.FamilyID) ||
		!protocol.ValidUUID(actorDeviceID) || cursor.Validate() != nil || cursor.FamilyID != grant.FamilyID {
		return errors.New("invalid delivery schedule")
	}
	if !repository.hosted {
		return nil
	}
	_, err := tx.Exec(ctx, `SELECT app.schedule_family_wakes($1,$2,$3,$4,$5)`, grant.FamilyID, actorDeviceID,
		cursor.ServerInstanceID, cursor.RecoveryEpoch, cursor.Sequence)
	return err
}
