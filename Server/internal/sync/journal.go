package sync

import (
	"context"
	"encoding/json"

	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

// ReadJournal is the read-only journal seam used by the bounded paging service
// in P2.3. This slice deliberately does not define paging policy.
func ReadJournal(ctx context.Context, tx pgx.Tx, familyID, instanceID string, epoch, after, highWater uint64, limit int) ([]protocol.JournalChange, error) {
	rows, err := tx.Query(ctx, `SELECT sequence,entity_kind,entity_id::text,entity_version,actor_account_id::text,device_id::text,
		operation,to_char(accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),canonical_entity
		FROM mutation_journal WHERE family_id=$1 AND sequence>$2 AND sequence<=$3 ORDER BY sequence LIMIT $4`, familyID, after, highWater, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	changes := []protocol.JournalChange{}
	for rows.Next() {
		var c protocol.JournalChange
		var raw []byte
		var sequence uint64
		if err := rows.Scan(&sequence, &c.EntityKind, &c.EntityID, &c.EntityVersion, &c.ActorAccountID, &c.DeviceID, &c.Operation, &c.AcceptedAt, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &c.SnapshotOrTombstone); err != nil {
			return nil, err
		}
		c.Cursor = protocol.Cursor{ServerInstanceID: instanceID, RecoveryEpoch: epoch, FamilyID: familyID, Sequence: sequence}
		changes = append(changes, c)
	}
	return changes, rows.Err()
}
