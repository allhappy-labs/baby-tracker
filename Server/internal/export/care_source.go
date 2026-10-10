package export

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"babytracker/server/internal/care"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

// careEntities reads care_entities (migration 000030): every row with
// care.Stream, and each entity's latest change with care.StreamChanges, so
// the journal section is the latest-state change list a pull from cursor 0
// returns, one change per entity at its changed_seq.
//
// Both read in pages of several statements. Under READ COMMITTED a write
// committed between two pages could move an entity past the pages already
// read or past the journal's high water, dropping or duplicating it, so
// they require the archive's transaction to be one snapshot (REPEATABLE
// READ or SERIALIZABLE, as Worker.readArchive opens it). A family lock is
// not an option there: the archive transaction is read-only, and it would
// stall every sync write for the length of an export.
type careEntities struct{}

var _ careSource = careEntities{}

func (careEntities) entities(ctx context.Context, tx pgx.Tx, familyID string, visit func(protocol.CanonicalEntity) error) error {
	if err := requireSnapshot(ctx, tx); err != nil {
		return err
	}
	return care.Stream(ctx, tx, familyID, func(r care.Row) error {
		entity, err := r.Entity()
		if err != nil {
			return fmt.Errorf("export: %s %s: %w", r.Kind, r.ID, err)
		}
		// Running timers as the legacy archive rendered them from the
		// timer tables (every timestamp to whole microseconds).
		if entity, err = care.NormalizeTimerEntity(entity); err != nil {
			return fmt.Errorf("export: %s %s: %w", r.Kind, r.ID, err)
		}
		return visit(entity)
	})
}

func (careEntities) journal(ctx context.Context, tx pgx.Tx, familyID string, through uint64, identity protocol.ServerInfo,
	visit func(protocol.JournalChange) error) error {
	if err := requireSnapshot(ctx, tx); err != nil {
		return err
	}
	return care.StreamChanges(ctx, tx, familyID, through, func(c care.ChangeRow) error {
		j := protocol.JournalChange{
			Cursor:     protocol.Cursor{ServerInstanceID: identity.InstanceID, RecoveryEpoch: identity.RecoveryEpoch, FamilyID: familyID, Sequence: c.Seq},
			EntityKind: c.Kind, EntityID: c.ID, EntityVersion: c.Version, ActorAccountID: c.ActorAccountID, DeviceID: c.DeviceID,
			Operation: c.Operation, AcceptedAt: c.AcceptedAt.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000Z"),
		}
		if err := json.Unmarshal(c.Wire, &j.SnapshotOrTombstone); err != nil {
			return fmt.Errorf("export: change %d: %w", c.Seq, err)
		}
		return visit(j)
	})
}

// requireSnapshot refuses a transaction whose statements do not share one
// snapshot.
func requireSnapshot(ctx context.Context, tx pgx.Tx) error {
	var isolation string
	if err := tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&isolation); err != nil {
		return err
	}
	if isolation != "repeatable read" && isolation != "serializable" {
		return fmt.Errorf("export: the care archive needs one snapshot, the transaction is %s: %w", isolation, ErrUnavailable)
	}
	return nil
}
