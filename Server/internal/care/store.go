package care

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

// The store reads and writes care_entities, care_receipts and the family
// row's sequence and usage counters. Every function runs inside the caller's
// transaction, the request transaction the authority guard opened with the
// tenant context set; the store never begins, commits or rolls back one.
// Statements are prepared and cached per connection (pgx's default mode),
// and the fixed sequences of a sync batch are pipelined: Fetch is one round
// trip, and QueueWrites (with QueueChanges) joins the caller's one batch.

var (
	// ErrNotFound is a row or family the tenant cannot see.
	ErrNotFound = errors.New("record_not_found")
	// ErrQuotaExceeded refuses a charge past the family's storage quota.
	ErrQuotaExceeded = errors.New("family_record_quota_exceeded")
	// ErrConflict is a write batch the stored state no longer admits: a
	// stored row already at that version or a newer one, or of another kind,
	// a family sequence that moved since it was read (neither happens under
	// the family lock), or a receipt not inserted (one exists already, or the
	// device is not the account's active device). The caller rolls the
	// transaction back.
	ErrConflict = errors.New("care: write conflict")
)

// FamilyState is the family row's sequence high water (next_sequence, the
// last sequence a change took) and care usage counters.
type FamilyState struct{ Sequence, RecordCount, ChargedBytes, EntityCount, WireBytes uint64 }

const (
	familyColumns = `next_sequence, care_record_count, care_charged_bytes, care_entity_count, care_wire_bytes`
	sqlLockFamily = `SELECT ` + familyColumns + ` FROM families WHERE id=$1 FOR NO KEY UPDATE`
	sqlReadFamily = `SELECT ` + familyColumns + ` FROM families WHERE id=$1`
)

func scanFamily(row pgx.Row) (FamilyState, error) {
	var s FamilyState
	err := row.Scan(&s.Sequence, &s.RecordCount, &s.ChargedBytes, &s.EntityCount, &s.WireBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return FamilyState{}, ErrNotFound
	}
	return s, err
}

// LockFamily locks the family row FOR NO KEY UPDATE (the write path's one
// family lock, the legacy LockFamilyForUpdate's semantics) and returns its
// state. A family the tenant cannot see is ErrNotFound.
func LockFamily(ctx context.Context, tx pgx.Tx, familyID string) (FamilyState, error) {
	return scanFamily(tx.QueryRow(ctx, sqlLockFamily, familyID))
}

// ReadFamilyState reads the family row without a lock.
func ReadFamilyState(ctx context.Context, tx pgx.Tx, familyID string) (FamilyState, error) {
	return scanFamily(tx.QueryRow(ctx, sqlReadFamily, familyID))
}

// QueueFamilyState queues ReadFamilyState onto b; read returns it after
// everything queued before it was read.
func QueueFamilyState(b *pgx.Batch, familyID string) (read func(pgx.BatchResults) (FamilyState, error)) {
	b.Queue(sqlReadFamily, familyID)
	return func(results pgx.BatchResults) (FamilyState, error) { return scanFamily(results.QueryRow()) }
}

// rowColumns is a care_entities row as scanRow reads it; the last column is
// the wire or, where only its size is needed, NULL.
const rowColumns = `family_id::text, id::text, kind, baby_id::text, entity_version, changed_seq, creator_account_id::text,
	actor_account_id::text, device_id::text, operation::text, accepted_at, tombstoned_at, started_at, ended_at, session_id::text,
	timer_kind, completed_record_id::text, sort_order, label, name_hash, breast_seconds, bottle_volume_ml::text, bottle_observations,
	bottle_missing, shortest_part_seconds, created_device_id::text, created_seq, feed_kind, octet_length(wire)`

const (
	withWire    = rowColumns + `, wire`
	withoutWire = rowColumns + `, NULL::bytea`
)

// writtenColumns are the columns a write sets, in the order the upsert's
// arrays (upsertArgs) give them.
const writtenColumns = `family_id, id, kind, baby_id, entity_version, changed_seq, creator_account_id, actor_account_id, device_id, operation,
	accepted_at, tombstoned_at, started_at, ended_at, session_id, timer_kind, completed_record_id, sort_order, label, name_hash, breast_seconds,
	bottle_volume_ml, bottle_observations, bottle_missing, shortest_part_seconds, wire, created_device_id, created_seq, feed_kind`

// The statements, each with the index it reads through (migration 000030
// section 3; TestCareQueriesUseOnePartitionAndIndexes pins them). Kind codes
// are literals: 1 baby, 5 timer.
const (
	// care_receipts_pkey.
	sqlFetchReceipts = `SELECT mutation_id::text, family_id = $2, semantic_hash, outcome::text FROM care_receipts
		WHERE device_id = $1 AND mutation_id = ANY($3::uuid[])`
	// care_entities_pkey, for the ids and for the completions of the stopped
	// timers (an InitPlan, so both stay index conditions).
	sqlFetchRows = `SELECT ` + withWire + ` FROM care_entities WHERE family_id = $1 AND id = ANY($2::uuid[] || ARRAY(
		SELECT completed_record_id FROM care_entities WHERE family_id = $1 AND id = ANY($3::uuid[]) AND completed_record_id IS NOT NULL))`
	// care_entities_session_idx.
	sqlFetchSessionMembers = `SELECT ` + withoutWire + ` FROM care_entities
		WHERE family_id = $1 AND tombstoned_at IS NULL AND session_id IS NOT NULL AND session_id = ANY($2::uuid[])`
	// care_entities_one_live_timer.
	sqlFetchLiveTimers = `SELECT ` + withWire + ` FROM care_entities
		WHERE family_id = $1 AND kind = 5 AND tombstoned_at IS NULL AND baby_id = ANY($2::uuid[])`
	// care_entities_history_idx.
	sqlFetchCascadeTargets = `SELECT ` + withoutWire + ` FROM care_entities
		WHERE family_id = $1 AND tombstoned_at IS NULL AND baby_id = ANY($2::uuid[])`
	// care_entities_baby_match_idx.
	sqlFetchLiveBabies = `SELECT count(*) FROM care_entities WHERE family_id = $1 AND kind = 1 AND tombstoned_at IS NULL`
	// care_entities_one_open_pump. Kind code literal: 10 pump.
	sqlFetchOpenPump = `SELECT ` + withWire + ` FROM care_entities
		WHERE family_id = $1 AND kind = 10 AND tombstoned_at IS NULL AND ended_at IS NULL`

	// care_entities_pkey (conflict target); the WHERE keeps a stored row from
	// moving back in version or changing kind. The origin (created_device_id,
	// created_seq) is inserted and never in the SET list: the runtime holds no
	// UPDATE on it, so a later version never moves it. A feed's feed_kind is
	// set like its other typed columns: an edit may change a feed's kind.
	sqlUpsert = `INSERT INTO care_entities (` + writtenColumns + `)
	SELECT $1, v.* FROM unnest($2::uuid[], $3::smallint[], $4::uuid[], $5::bigint[], $6::bigint[], $7::uuid[], $8::uuid[], $9::uuid[],
		$10::text[]::app.care_operation[], $11::timestamptz[], $12::timestamptz[], $13::timestamptz[], $14::timestamptz[], $15::uuid[],
		$16::smallint[], $17::uuid[], $18::integer[], $19::text[], $20::bytea[], $21::integer[], $22::text[]::numeric[], $23::smallint[],
		$24::smallint[], $25::integer[], $26::bytea[], $27::uuid[], $28::bigint[], $29::smallint[]) AS v
	ON CONFLICT (family_id, id) DO UPDATE SET baby_id = EXCLUDED.baby_id, entity_version = EXCLUDED.entity_version,
		changed_seq = EXCLUDED.changed_seq, actor_account_id = EXCLUDED.actor_account_id, device_id = EXCLUDED.device_id,
		operation = EXCLUDED.operation, accepted_at = EXCLUDED.accepted_at, tombstoned_at = EXCLUDED.tombstoned_at,
		started_at = EXCLUDED.started_at, ended_at = EXCLUDED.ended_at, session_id = EXCLUDED.session_id,
		completed_record_id = EXCLUDED.completed_record_id, sort_order = EXCLUDED.sort_order, label = EXCLUDED.label,
		name_hash = EXCLUDED.name_hash, breast_seconds = EXCLUDED.breast_seconds, bottle_volume_ml = EXCLUDED.bottle_volume_ml,
		bottle_observations = EXCLUDED.bottle_observations, bottle_missing = EXCLUDED.bottle_missing,
		shortest_part_seconds = EXCLUDED.shortest_part_seconds, wire = EXCLUDED.wire, feed_kind = EXCLUDED.feed_kind
	WHERE care_entities.entity_version < EXCLUDED.entity_version AND care_entities.kind = EXCLUDED.kind`
	// The device must be the receipt account's and active, as the legacy
	// app.save_mutation_receipt required; a receipt that is not inserted
	// fails the rows-affected check. devices_pkey.
	sqlInsertReceipts = `INSERT INTO care_receipts (device_id, mutation_id, family_id, account_id, semantic_hash, outcome, accepted_at)
	SELECT v.d, v.m, $1, v.a, v.h, v.o::jsonb, v.t
	FROM unnest($2::uuid[], $3::uuid[], $4::uuid[], $5::bytea[], $6::text[], $7::timestamptz[]) AS v(d, m, a, h, o, t)
	WHERE EXISTS (SELECT FROM devices d WHERE d.id = v.d AND d.account_id = v.a AND d.status = 'active')`
	// families_pkey; the row is already locked by Fetch.
	sqlAdvanceFamily = `UPDATE families SET next_sequence = $2, care_record_count = care_record_count + $3,
		care_charged_bytes = care_charged_bytes + $4, care_entity_count = care_entity_count + $5, care_wire_bytes = care_wire_bytes + $6
	WHERE id = $1 AND next_sequence = $7`

	changeColumns = `changed_seq, kind, id::text, entity_version, actor_account_id::text, device_id::text, operation::text, accepted_at, wire`
	// care_entities_seq_key.
	sqlChanges = `SELECT ` + changeColumns + ` FROM care_entities
		WHERE family_id = $1 AND changed_seq > $2 AND changed_seq <= $3 ORDER BY changed_seq LIMIT $4`
	// families_pkey (inside app.family_high_water, 000037), then
	// care_entities_seq_key bounded by the high water the same statement
	// read: a STABLE function runs in its caller's snapshot. The function
	// keeps families out of this statement's plan, so a VACUUM or ANALYZE
	// of families does not make every backend plan the pull again.
	sqlReadPull = `WITH hw AS (SELECT next_sequence FROM app.family_high_water($1) AS f(next_sequence))
	SELECT hw.next_sequence, c.changed_seq, c.kind, c.id::text, c.entity_version, c.actor_account_id::text, c.device_id::text,
		c.operation::text, c.accepted_at, c.wire
	FROM hw LEFT JOIN LATERAL (
		SELECT changed_seq, kind, id, entity_version, actor_account_id, device_id, operation, accepted_at, wire FROM care_entities
		WHERE family_id = $1 AND changed_seq > $2 AND changed_seq <= hw.next_sequence ORDER BY changed_seq LIMIT $3) c ON true`
	// care_entities_snapshot_idx; tombstones included. The ORDER BY names
	// the table's columns: an unqualified id would be the id::text output
	// column, a sort by text (a whole kind group read and sorted, and under
	// a collation that does not sort hex like bytes, pages out of the uuid
	// order the (kind, id) > key continuation resumes in).
	sqlSnapshotPage = `SELECT kind, id::text, wire FROM care_entities
		WHERE family_id = $1 AND (kind, id) > ($2::smallint, $3::uuid) ORDER BY care_entities.kind, care_entities.id LIMIT $4`
	// care_entities_snapshot_idx; tombstones included; ordered by the uuid
	// as sqlSnapshotPage.
	sqlStreamPage = `SELECT ` + withWire + ` FROM care_entities
		WHERE family_id = $1 AND (kind, id) > ($2::smallint, $3::uuid) ORDER BY care_entities.kind, care_entities.id LIMIT $4`
	// care_entities_pkey.
	sqlGet     = `SELECT ` + withWire + ` FROM care_entities WHERE family_id = $1 AND id = $2`
	sqlGetMany = `SELECT ` + withWire + ` FROM care_entities WHERE family_id = $1 AND id = ANY($2::uuid[])`
	// care_entities_baby_match_idx; ordered by the uuid as sqlSnapshotPage.
	sqlMatchBabies = `SELECT ` + withWire + ` FROM care_entities
		WHERE family_id = $1 AND kind = 1 AND tombstoned_at IS NULL AND name_hash = $2 ORDER BY care_entities.id`
	// families_pkey; a release (bytes <= 0) is never refused.
	sqlChargeImport = `UPDATE families SET care_charged_bytes = care_charged_bytes + $2::bigint
	WHERE id = $1 AND ($2::bigint <= 0 OR care_charged_bytes + $2::bigint <= $3::bigint) RETURNING care_charged_bytes`
)

// scanRow reads one row selected with withWire or withoutWire.
func scanRow(row pgx.Row) (Row, error) {
	var r Row
	var kind int16
	var timerKind *int16
	var volume *string
	var wireBytes int32
	var createdDevice *string
	var createdSeq *int64
	var feedKind *int16
	if err := row.Scan(&r.FamilyID, &r.ID, &kind, &r.BabyID, &r.EntityVersion, &r.ChangedSeq, &r.CreatorAccountID, &r.ActorAccountID,
		&r.DeviceID, &r.Operation, &r.AcceptedAt, &r.TombstonedAt, &r.StartedAt, &r.EndedAt, &r.SessionID, &timerKind,
		&r.CompletedRecordID, &r.SortOrder, &r.Label, &r.NameHash, &r.Derived.BreastSeconds, &volume, &r.Derived.BottleObservations,
		&r.Derived.BottleMissing, &r.Derived.ShortestPartSeconds, &createdDevice, &createdSeq, &feedKind, &wireBytes, &r.Wire); err != nil {
		return Row{}, err
	}
	if createdDevice != nil && createdSeq != nil {
		r.CreatedDeviceID, r.CreatedSeq = *createdDevice, uint64(*createdSeq)
	}
	var ok bool
	if r.Kind, ok = KindName(kind); !ok {
		return Row{}, fmt.Errorf("care: row %s has kind code %d", r.ID, kind)
	}
	if timerKind != nil {
		name, ok := TimerKindName(*timerKind)
		if !ok {
			return Row{}, fmt.Errorf("care: row %s has timer kind code %d", r.ID, *timerKind)
		}
		r.TimerKind = &name
	}
	if feedKind != nil {
		name, ok := FeedKindName(*feedKind)
		if !ok {
			return Row{}, fmt.Errorf("care: row %s has feed kind code %d", r.ID, *feedKind)
		}
		r.FeedKind = &name
	}
	if volume != nil {
		parsed, err := protocol.ParseDecimal(*volume)
		if err != nil {
			return Row{}, fmt.Errorf("care: row %s bottle volume %q: %w", r.ID, *volume, err)
		}
		r.Derived.BottleVolumeML = parsed
	}
	r.AcceptedAt = r.AcceptedAt.UTC()
	for _, at := range []**time.Time{&r.TombstonedAt, &r.StartedAt, &r.EndedAt} {
		if *at != nil {
			utc := (*at).UTC()
			*at = &utc
		}
	}
	r.WireBytes = int(wireBytes)
	return r, nil
}

func scanRows(rows pgx.Rows) ([]Row, error) {
	defer rows.Close()
	var out []Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Refs is everything a sync batch references, for Fetch.
type Refs struct {
	// IDs are rows to load whole (mutation targets, payload babies,
	// completion records, session ids that may name a row).
	IDs []string
	// StoppedTimerIDs are timers whose completion record (a stopped timer
	// tombstone's completed_record_id) is loaded whole with the rows.
	StoppedTimerIDs []string
	// SessionIDs select the live members of those sessions (without wire).
	SessionIDs []string
	// TimerBabyIDs select those babies' live timers.
	TimerBabyIDs []string
	// CascadeBabyIDs select every live row of those babies (without wire).
	CascadeBabyIDs []string
	// MutationIDs are looked up in DeviceID's receipts.
	MutationIDs []string
	DeviceID    string
	// CountLiveBabies counts the family's live babies.
	CountLiveBabies bool
	// OpenPump loads the family's running pump, if it has one.
	OpenPump bool
}

// StoredReceipt is a receipt of the device for one mutation id. SameFamily
// is false for the account's receipt from another family.
type StoredReceipt struct {
	SameFamily bool
	Hash       [32]byte
	Outcome    protocol.Outcome
}

// Fetched is what Fetch read: the locked family's state, the rows by id,
// session members and cascade targets (WireBytes set, Wire nil), live
// timers, the running pump, the live baby count and the receipts by
// mutation id.
type Fetched struct {
	Family                                     FamilyState
	Rows                                       map[string]Row
	SessionMembers, LiveTimers, CascadeTargets []Row
	OpenPumps                                  []Row
	LiveBabies                                 int
	Receipts                                   map[string]StoredReceipt
}

// Fetch locks the family row and reads everything refs names in one
// pipelined round trip: the lock first, then the receipts, the rows (with
// the stopped timers' completions), session members, live timers, cascade
// targets, the running pump and the live baby count, each only when refs
// asks for it.
func Fetch(ctx context.Context, tx pgx.Tx, familyID string, refs Refs) (Fetched, error) {
	fetched := Fetched{Rows: map[string]Row{}, Receipts: map[string]StoredReceipt{}}
	batch := &pgx.Batch{}
	var reads []func(pgx.BatchResults) error
	queue := func(sql string, read func(pgx.BatchResults) error, args ...any) {
		batch.Queue(sql, args...)
		reads = append(reads, read)
	}
	rowsInto := func(into *[]Row) func(pgx.BatchResults) error {
		return func(results pgx.BatchResults) error {
			rows, err := results.Query()
			if err != nil {
				return err
			}
			*into, err = scanRows(rows)
			return err
		}
	}
	queue(sqlLockFamily, func(results pgx.BatchResults) (err error) {
		fetched.Family, err = scanFamily(results.QueryRow())
		return err
	}, familyID)
	if len(refs.MutationIDs) > 0 {
		queue(sqlFetchReceipts, func(results pgx.BatchResults) error {
			rows, err := results.Query()
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id, outcome string
				var hash []byte
				var receipt StoredReceipt
				if err := rows.Scan(&id, &receipt.SameFamily, &hash, &outcome); err != nil {
					return err
				}
				if len(hash) != len(receipt.Hash) {
					return fmt.Errorf("care: receipt %s hash of %d bytes", id, len(hash))
				}
				copy(receipt.Hash[:], hash)
				if err := json.Unmarshal([]byte(outcome), &receipt.Outcome); err != nil {
					return fmt.Errorf("care: receipt %s outcome: %w", id, err)
				}
				fetched.Receipts[id] = receipt
			}
			return rows.Err()
		}, refs.DeviceID, familyID, refs.MutationIDs)
	}
	if len(refs.IDs) > 0 || len(refs.StoppedTimerIDs) > 0 {
		var rows []Row
		read := rowsInto(&rows)
		queue(sqlFetchRows, func(results pgx.BatchResults) error {
			if err := read(results); err != nil {
				return err
			}
			for _, r := range rows {
				fetched.Rows[r.ID] = r
			}
			return nil
		}, familyID, nonNil(refs.IDs), nonNil(refs.StoppedTimerIDs))
	}
	if len(refs.SessionIDs) > 0 {
		queue(sqlFetchSessionMembers, rowsInto(&fetched.SessionMembers), familyID, refs.SessionIDs)
	}
	if len(refs.TimerBabyIDs) > 0 {
		queue(sqlFetchLiveTimers, rowsInto(&fetched.LiveTimers), familyID, refs.TimerBabyIDs)
	}
	if len(refs.CascadeBabyIDs) > 0 {
		queue(sqlFetchCascadeTargets, rowsInto(&fetched.CascadeTargets), familyID, refs.CascadeBabyIDs)
	}
	if refs.OpenPump {
		queue(sqlFetchOpenPump, rowsInto(&fetched.OpenPumps), familyID)
	}
	if refs.CountLiveBabies {
		queue(sqlFetchLiveBabies, func(results pgx.BatchResults) error {
			return results.QueryRow().Scan(&fetched.LiveBabies)
		}, familyID)
	}
	results := tx.SendBatch(ctx, batch)
	for _, read := range reads {
		if err := read(results); err != nil {
			_ = results.Close()
			return Fetched{}, err
		}
	}
	if err := results.Close(); err != nil {
		return Fetched{}, err
	}
	return fetched, nil
}

func nonNil(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

// ReceiptRow is a receipt to write.
type ReceiptRow struct {
	DeviceID, MutationID, AccountID string
	Hash                            [32]byte
	Outcome                         protocol.Outcome
	AcceptedAt                      time.Time
}

// CounterDelta is the change a write batch makes to the family's usage
// counters.
type CounterDelta struct{ Records, ChargedBytes, Entities, WireBytes int64 }

// Replace is d plus next replacing previous (nil: next is a new row).
func (d CounterDelta) Replace(previous *Row, next Row) CounterDelta {
	d.Records += countsAsRecord(next)
	d.ChargedBytes += next.ChargedBytes()
	d.WireBytes += int64(next.WireBytes)
	if previous == nil {
		d.Entities++
		return d
	}
	d.Records -= countsAsRecord(*previous)
	d.ChargedBytes -= previous.ChargedBytes()
	d.WireBytes -= int64(previous.WireBytes)
	return d
}

func countsAsRecord(r Row) int64 {
	if r.CountsAsRecord() {
		return 1
	}
	return 0
}

// Writes is one sync batch's writes: the final row of every entity it
// changed (one row per id), its receipts, and the family's new sequence and
// counter delta over Base, the state Fetch locked.
type Writes struct {
	FamilyID string
	Base     FamilyState
	Sequence uint64
	Rows     []Row
	Receipts []ReceiptRow
	Delta    CounterDelta
}

// QueueWrites queues a write batch onto b: the multi-row upsert, the bulk
// receipt insert and the family row's sequence and counters, each only when
// there is something to write. check reads their results, in queue order,
// before anything queued after them; it fails with ErrConflict when a row
// was not written (a stored row at that version or newer, or of another
// kind), a receipt was not written (a receipt of that mutation exists, or
// the device is not the account's active device), or the family sequence is
// no longer Base.Sequence. After a failed check the caller rolls back.
//
// The write set itself is checked first, and a malformed one queues
// nothing: every row takes a distinct sequence in (Base.Sequence,
// Sequence], one row per entity, one receipt per (device, mutation).
func QueueWrites(b *pgx.Batch, w Writes) (check func(pgx.BatchResults) error) {
	return queueWrites(b, w, true)
}

// checkWriteSet refuses a write set no planner may produce: a row whose
// sequence is outside the range the batch allocated (a pull past the high
// water would never see it, or a stale sequence would hide it from cursors
// already past it), two rows at one sequence or of one entity, or two
// receipts of one mutation.
func checkWriteSet(w Writes) error {
	sequences := make(map[uint64]string, len(w.Rows))
	ids := make(map[string]bool, len(w.Rows))
	for _, r := range w.Rows {
		if r.ChangedSeq <= w.Base.Sequence || r.ChangedSeq > w.Sequence {
			return fmt.Errorf("care: row %s at sequence %d, outside the batch's (%d, %d]", r.ID, r.ChangedSeq, w.Base.Sequence, w.Sequence)
		}
		if other, ok := sequences[r.ChangedSeq]; ok {
			return fmt.Errorf("care: rows %s and %s at sequence %d twice", other, r.ID, r.ChangedSeq)
		}
		if ids[r.ID] {
			return fmt.Errorf("care: row %s written twice in one batch", r.ID)
		}
		sequences[r.ChangedSeq], ids[r.ID] = r.ID, true
	}
	receipts := make(map[[2]string]bool, len(w.Receipts))
	for _, r := range w.Receipts {
		key := [2]string{r.DeviceID, r.MutationID}
		if receipts[key] {
			return fmt.Errorf("care: receipt of mutation %s written twice in one batch", r.MutationID)
		}
		receipts[key] = true
	}
	return nil
}

func queueWrites(b *pgx.Batch, w Writes, guarded bool) (check func(pgx.BatchResults) error) {
	fail := func(err error) func(pgx.BatchResults) error { return func(pgx.BatchResults) error { return err } }
	if w.Sequence < w.Base.Sequence {
		return fail(fmt.Errorf("%w: sequence %d below the family's %d", ErrConflict, w.Sequence, w.Base.Sequence))
	}
	if guarded {
		if err := checkWriteSet(w); err != nil {
			return fail(err)
		}
	}
	var steps []func(pgx.BatchResults) error
	affects := func(n int64, what string) func(pgx.BatchResults) error {
		return func(results pgx.BatchResults) error {
			tag, err := results.Exec()
			if err != nil {
				return err
			}
			if tag.RowsAffected() != n {
				return fmt.Errorf("%w: %s: %d of %d written", ErrConflict, what, tag.RowsAffected(), n)
			}
			return nil
		}
	}
	// Arguments first, so a failure queues nothing.
	var rowArgs, receiptRowArgs []any
	var err error
	if len(w.Rows) > 0 {
		if rowArgs, err = upsertArgs(w.FamilyID, w.Rows); err != nil {
			return fail(err)
		}
	}
	if len(w.Receipts) > 0 {
		if receiptRowArgs, err = receiptArgs(w.FamilyID, w.Receipts); err != nil {
			return fail(err)
		}
	}
	if rowArgs != nil {
		b.Queue(sqlUpsert, rowArgs...)
		steps = append(steps, affects(int64(len(w.Rows)), "rows (a stored row has this version or a newer one, or another kind)"))
	}
	if receiptRowArgs != nil {
		b.Queue(sqlInsertReceipts, receiptRowArgs...)
		steps = append(steps, affects(int64(len(w.Receipts)), "receipts (one exists, or the device is not the account's active device)"))
	}
	if w.Sequence != w.Base.Sequence || w.Delta != (CounterDelta{}) {
		b.Queue(sqlAdvanceFamily, w.FamilyID, int64(w.Sequence), w.Delta.Records, w.Delta.ChargedBytes, w.Delta.Entities, w.Delta.WireBytes,
			int64(w.Base.Sequence))
		steps = append(steps, affects(1, fmt.Sprintf("family at sequence %d", w.Base.Sequence)))
	}
	return func(results pgx.BatchResults) error {
		for _, step := range steps {
			if err := step(results); err != nil {
				return err
			}
		}
		return nil
	}
}

func upsertArgs(familyID string, rows []Row) ([]any, error) {
	n := len(rows)
	var (
		ids, creators, actors, devices, operations   = make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
		kinds, observations, missing                 = make([]int16, n), make([]int16, n), make([]int16, n)
		babies, sessions, completed, labels, volumes = make([]*string, n), make([]*string, n), make([]*string, n), make([]*string, n), make([]*string, n)
		versions, seqs                               = make([]int64, n), make([]int64, n)
		accepted                                     = make([]time.Time, n)
		tombstoned, started, ended                   = make([]*time.Time, n), make([]*time.Time, n), make([]*time.Time, n)
		timerKinds, feedKinds                        = make([]*int16, n), make([]*int16, n)
		sortOrders, shortest                         = make([]*int32, n), make([]*int32, n)
		breast                                       = make([]int32, n)
		nameHashes, wires                            = make([][]byte, n), make([][]byte, n)
		createdDevices                               = make([]*string, n)
		createdSeqs                                  = make([]*int64, n)
	)
	for i, r := range rows {
		if r.FamilyID != familyID {
			return nil, fmt.Errorf("care: row %s of family %s written to %s", r.ID, r.FamilyID, familyID)
		}
		c, err := columnCodes(r)
		if err != nil {
			return nil, err
		}
		timerKinds[i], feedKinds[i], volumes[i], createdDevices[i], createdSeqs[i] = c.timerKind, c.feedKind, c.volume, c.createdDevice, c.createdSeq
		ids[i], kinds[i], babies[i], versions[i], seqs[i] = r.ID, c.kind, r.BabyID, int64(r.EntityVersion), int64(r.ChangedSeq)
		creators[i], actors[i], devices[i], operations[i] = r.CreatorAccountID, r.ActorAccountID, r.DeviceID, r.Operation
		accepted[i], tombstoned[i], started[i], ended[i] = r.AcceptedAt, r.TombstonedAt, r.StartedAt, r.EndedAt
		sessions[i], completed[i], sortOrders[i], labels[i], nameHashes[i] = r.SessionID, r.CompletedRecordID, r.SortOrder, r.Label, r.NameHash
		breast[i], observations[i], missing[i], shortest[i] = r.Derived.BreastSeconds, r.Derived.BottleObservations, r.Derived.BottleMissing,
			r.Derived.ShortestPartSeconds
		wires[i] = r.Wire
	}
	return []any{familyID, ids, kinds, babies, versions, seqs, creators, actors, devices, operations, accepted, tombstoned, started, ended,
		sessions, timerKinds, completed, sortOrders, labels, nameHashes, breast, volumes, observations, missing, shortest, wires, createdDevices,
		createdSeqs, feedKinds}, nil
}

// codes is a row's coded and optional columns as the upsert writes them: the
// kind codes, the bottle volume as decimal text and the origin, NULL ("" and
// 0) or both set.
type codes struct {
	kind                  int16
	timerKind, feedKind   *int16
	volume, createdDevice *string
	createdSeq            *int64
}

// columnCodes checks r for writing (a wire, known kind codes, an origin
// either whole or absent) and codes its columns.
func columnCodes(r Row) (codes, error) {
	var c codes
	if len(r.Wire) == 0 {
		return c, fmt.Errorf("care: row %s written without its wire", r.ID)
	}
	var ok bool
	if c.kind, ok = KindCode(r.Kind); !ok {
		return c, fmt.Errorf("care: row %s kind %q", r.ID, r.Kind)
	}
	if r.TimerKind != nil {
		code, ok := TimerKindCode(*r.TimerKind)
		if !ok {
			return c, fmt.Errorf("care: row %s timer kind %q", r.ID, *r.TimerKind)
		}
		c.timerKind = &code
	}
	if r.FeedKind != nil {
		code, ok := FeedKindCode(*r.FeedKind)
		if !ok {
			return c, fmt.Errorf("care: row %s feed kind %q", r.ID, *r.FeedKind)
		}
		c.feedKind = &code
	}
	if r.Derived.BottleVolumeML != nil {
		volume := r.Derived.BottleVolumeML.String()
		c.volume = &volume
	}
	if (r.CreatedDeviceID == "") != (r.CreatedSeq == 0) {
		return c, fmt.Errorf("care: row %s origin %q at sequence %d is half set", r.ID, r.CreatedDeviceID, r.CreatedSeq)
	}
	if r.CreatedDeviceID != "" { // none ("" and 0) is NULL
		device, seq := r.CreatedDeviceID, int64(r.CreatedSeq)
		c.createdDevice, c.createdSeq = &device, &seq
	}
	return c, nil
}

func receiptArgs(familyID string, receipts []ReceiptRow) ([]any, error) {
	n := len(receipts)
	devices, mutations, accounts, outcomes := make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	hashes, accepted := make([][]byte, n), make([]time.Time, n)
	for i, r := range receipts {
		outcome, err := json.Marshal(r.Outcome)
		if err != nil {
			return nil, fmt.Errorf("care: receipt %s outcome: %w", r.MutationID, err)
		}
		devices[i], mutations[i], accounts[i], outcomes[i], accepted[i] = r.DeviceID, r.MutationID, r.AccountID, string(outcome), r.AcceptedAt
		hashes[i] = append([]byte(nil), r.Hash[:]...)
	}
	return []any{familyID, devices, mutations, accounts, hashes, outcomes, accepted}, nil
}

// ChangeRow is a row as an incremental pull serves it: the change envelope
// columns and the stored wire.
type ChangeRow struct {
	Seq                                 uint64
	Kind, ID                            string
	Version                             uint64
	ActorAccountID, DeviceID, Operation string
	AcceptedAt                          time.Time
	Wire                                []byte
}

// Encode is the served change: the envelope around the stored wire,
// verbatim, byte-identical to the legacy journal change.
func (c ChangeRow) Encode(server protocol.ServerInfo, familyID string) (json.RawMessage, error) {
	cursor := protocol.Cursor{ServerInstanceID: server.InstanceID, RecoveryEpoch: server.RecoveryEpoch, FamilyID: familyID, Sequence: c.Seq}
	return protocol.EncodeChange(cursor, c.Kind, c.ID, c.Version, c.ActorAccountID, c.DeviceID, c.Operation,
		c.AcceptedAt.UTC().Format("2006-01-02T15:04:05.000000Z"), c.Wire)
}

// scanChange reads changeColumns into c; seq is nil for ReadPull's empty
// lateral row.
func scanChange(row pgx.Rows, dest ...any) (ChangeRow, bool, error) {
	var seq, version *uint64
	var kind *int16
	var id, actor, device, operation *string
	var accepted *time.Time
	var c ChangeRow
	if err := row.Scan(append(dest, &seq, &kind, &id, &version, &actor, &device, &operation, &accepted, &c.Wire)...); err != nil {
		return ChangeRow{}, false, err
	}
	if seq == nil {
		return ChangeRow{}, false, nil
	}
	name, ok := KindName(*kind)
	if !ok {
		return ChangeRow{}, false, fmt.Errorf("care: change %d has kind code %d", *seq, *kind)
	}
	c.Seq, c.Kind, c.ID, c.Version, c.ActorAccountID, c.DeviceID, c.Operation, c.AcceptedAt = *seq, name, *id, *version, *actor, *device,
		*operation, accepted.UTC()
	return c, true, nil
}

func scanChanges(rows pgx.Rows) ([]ChangeRow, error) {
	defer rows.Close()
	changes := []ChangeRow{}
	for rows.Next() {
		c, _, err := scanChange(rows)
		if err != nil {
			return nil, err
		}
		changes = append(changes, c)
	}
	return changes, rows.Err()
}

// QueueChanges queues the read of the changes in (after, through], in
// sequence order, at most limit; read returns them after everything queued
// before it was read.
func QueueChanges(b *pgx.Batch, familyID string, after, through uint64, limit int) (read func(pgx.BatchResults) ([]ChangeRow, error)) {
	b.Queue(sqlChanges, familyID, int64(after), int64(through), limit)
	return func(results pgx.BatchResults) ([]ChangeRow, error) {
		rows, err := results.Query()
		if err != nil {
			return nil, err
		}
		return scanChanges(rows)
	}
}

// ReadPull reads the family's high water and the changes in (after, high
// water], at most limit, from one statement's snapshot, so every change it
// returns is at or below the high water and none at or below it is missed.
// It takes no lock. A family the tenant cannot see is ErrNotFound.
func ReadPull(ctx context.Context, tx pgx.Tx, familyID string, after uint64, limit int) (highWater uint64, changes []ChangeRow, err error) {
	rows, err := tx.Query(ctx, sqlReadPull, familyID, int64(after), limit)
	if err != nil {
		return 0, nil, err
	}
	return scanPull(rows)
}

// QueuePull queues ReadPull's statement on b (the pull sent with the
// commit, authority.CommitPipeline); read returns what ReadPull returns,
// from that statement's results, after everything queued before it was
// read.
func QueuePull(b *pgx.Batch, familyID string, after uint64, limit int) (read func(pgx.BatchResults) (uint64, []ChangeRow, error)) {
	b.Queue(sqlReadPull, familyID, int64(after), limit)
	return func(results pgx.BatchResults) (uint64, []ChangeRow, error) {
		rows, err := results.Query()
		if err != nil {
			return 0, nil, err
		}
		return scanPull(rows)
	}
}

// scanPull reads sqlReadPull's rows and closes them.
func scanPull(rows pgx.Rows) (highWater uint64, changes []ChangeRow, err error) {
	defer rows.Close()
	found := false
	changes = []ChangeRow{}
	for rows.Next() {
		found = true
		c, ok, err := scanChange(rows, &highWater)
		if err != nil {
			return 0, nil, err
		}
		if ok {
			changes = append(changes, c)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	if !found {
		return 0, nil, ErrNotFound
	}
	return highWater, changes, nil
}

// SnapshotKey is a snapshot position: the care kind and id of the last row
// served.
type SnapshotKey struct{ Kind, ID string }

// FirstSnapshotKey sorts before every row (ids are never the zero uuid).
var FirstSnapshotKey = SnapshotKey{Kind: "baby", ID: "00000000-0000-0000-0000-000000000000"}

// SnapshotRow is a snapshot entity: its key and stored wire.
type SnapshotRow struct {
	Key  SnapshotKey
	Wire []byte
}

// QueueSnapshotPage queues the read of the rows after the key, in (kind
// code, id) order, at most limit. Every row is served, tombstones included,
// as the legacy snapshot served them; there is no lock and no copy, and a
// row changed during paging also arrives in the next pull.
func QueueSnapshotPage(b *pgx.Batch, familyID string, after SnapshotKey, limit int) (read func(pgx.BatchResults) ([]SnapshotRow, error)) {
	kind, ok := KindCode(after.Kind)
	if !ok {
		return func(pgx.BatchResults) ([]SnapshotRow, error) {
			return nil, fmt.Errorf("care: snapshot key kind %q", after.Kind)
		}
	}
	b.Queue(sqlSnapshotPage, familyID, kind, after.ID, limit)
	return func(results pgx.BatchResults) ([]SnapshotRow, error) {
		rows, err := results.Query()
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		page := []SnapshotRow{}
		for rows.Next() {
			var r SnapshotRow
			var code int16
			if err := rows.Scan(&code, &r.Key.ID, &r.Wire); err != nil {
				return nil, err
			}
			if r.Key.Kind, ok = KindName(code); !ok {
				return nil, fmt.Errorf("care: row %s has kind code %d", r.Key.ID, code)
			}
			page = append(page, r)
		}
		return page, rows.Err()
	}
}

// Get reads one row. A row the tenant cannot see is ErrNotFound.
func Get(ctx context.Context, tx pgx.Tx, familyID, id string) (Row, error) {
	r, err := scanRow(tx.QueryRow(ctx, sqlGet, familyID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Row{}, ErrNotFound
	}
	return r, err
}

// GetMany reads the rows of ids that exist, by id.
func GetMany(ctx context.Context, tx pgx.Tx, familyID string, ids []string) (map[string]Row, error) {
	out := map[string]Row{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, sqlGetMany, familyID, ids)
	if err != nil {
		return nil, err
	}
	found, err := scanRows(rows)
	for _, r := range found {
		out[r.ID] = r
	}
	return out, err
}

// streamPageSize is the rows Stream and StreamChanges read per statement.
var streamPageSize = 1000

// Stream visits every row of the family, tombstones included, in (kind
// code, id) order. It reads in pages and visits a page after reading it, so
// visit may use tx.
func Stream(ctx context.Context, tx pgx.Tx, familyID string, visit func(Row) error) error {
	after := FirstSnapshotKey
	for {
		kind, _ := KindCode(after.Kind)
		rows, err := tx.Query(ctx, sqlStreamPage, familyID, kind, after.ID, streamPageSize)
		if err != nil {
			return err
		}
		page, err := scanRows(rows)
		if err != nil {
			return err
		}
		for _, r := range page {
			if err := visit(r); err != nil {
				return err
			}
		}
		if len(page) < streamPageSize {
			return nil
		}
		last := page[len(page)-1]
		after = SnapshotKey{Kind: last.Kind, ID: last.ID}
	}
}

// StreamChanges visits every change at or below through in sequence order
// (each entity's latest change), in pages; visit may use tx.
func StreamChanges(ctx context.Context, tx pgx.Tx, familyID string, through uint64, visit func(ChangeRow) error) error {
	after := uint64(0)
	for {
		rows, err := tx.Query(ctx, sqlChanges, familyID, int64(after), int64(through), streamPageSize)
		if err != nil {
			return err
		}
		page, err := scanChanges(rows)
		if err != nil {
			return err
		}
		for _, c := range page {
			if err := visit(c); err != nil {
				return err
			}
		}
		if len(page) < streamPageSize {
			return nil
		}
		after = page[len(page)-1].Seq
	}
}

// MatchBabies reads the family's live babies whose name hash
// (BabyNameHash of name and birth date) is hash, in id order.
func MatchBabies(ctx context.Context, tx pgx.Tx, familyID string, hash [32]byte) ([]Row, error) {
	rows, err := tx.Query(ctx, sqlMatchBabies, familyID, hash[:])
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
}

// ChargeImport adds bytes (retained import staging; negative releases it)
// to the family's charged bytes. A charge that would pass
// limits.MaxChargedStorageBytesPerFamily is ErrQuotaExceeded and changes
// nothing; a release is never refused. The caller holds the family lock.
func ChargeImport(ctx context.Context, tx pgx.Tx, familyID string, bytes int64, limits protocol.Limits) error {
	var charged int64
	err := tx.QueryRow(ctx, sqlChargeImport, familyID, bytes, limits.MaxChargedStorageBytesPerFamily).Scan(&charged)
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err := ReadFamilyState(ctx, tx, familyID); err != nil {
		return err
	}
	return ErrQuotaExceeded
}
