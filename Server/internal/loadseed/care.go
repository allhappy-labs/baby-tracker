//go:build identitytest

package loadseed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"babytracker/server/internal/care"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// writer is a family member who writes care: its account and its devices
// (the owner first, then the caregiver).
type writer struct {
	AccountID string
	DeviceIDs []string
}

// seededReceipt is a care_receipts row with its outcome as the write path
// encodes it (the bytes it charges).
type seededReceipt struct {
	care.ReceiptRow
	outcome []byte
}

// encodedFamily is a family's care as the write path would have left it:
// the final row of every entity, the receipts kept, and the family's
// sequence and four counters.
type encodedFamily struct {
	Rows     []care.Row
	Receipts []seededReceipt
	State    care.FamilyState
}

// validateEvery: encodeFamily validates the mutations of every
// validateEvery-th family (care.ValidateMutation loads each time zone from
// disk per call, three quarters of the encoding's CPU); the generator's
// paths are the same for every family, and TestPlannedMutationsAreValid
// validates whole R10k families.
const validateEvery = 16

// encodeFamily applies plan's changes as the write path applies them, one
// mutation per batch in acceptance order, without a database: each change
// has its canonical entity built as the
// record rules build it (internal/care/records.go applyRecord, timers.go
// startTimer) and encoded by care.Encode over the entity's previous row;
// the counters move by care.CounterDelta.Replace (store.go). Sampled
// families' mutations are validated first (care.ValidateMutation). A change
// accepted at or after receiptsSince (zero: every change) gets the receipt
// the write path writes (plan.go addReceipt), charged to the family.
func encodeFamily(plan FamilyPlan, familyID string, writers []writer, server protocol.ServerInfo, receiptsSince time.Time) (encodedFamily, error) {
	limits := protocol.DefaultLimits()
	validate := plan.Index%validateEvery == 0
	current := make(map[string]care.Row, plan.entities)
	entities := make(map[string]protocol.CanonicalEntity, plan.entities)
	order := make([]string, 0, plan.entities)
	var out encodedFamily
	var delta care.CounterDelta
	var seq uint64
	for _, c := range plan.Changes {
		m := c.Mutation
		if validate {
			if err := care.ValidateMutation(m, limits); err != nil {
				return encodedFamily{}, fmt.Errorf("loadseed: family %d %s %s %s: %w", plan.Index, m.EntityKind, m.Operation, m.EntityID, err)
			}
		}
		if c.Actor < 0 || c.Actor >= len(writers) || len(writers[c.Actor].DeviceIDs) == 0 {
			return encodedFamily{}, fmt.Errorf("loadseed: family %d change by writer %d of %d", plan.Index, c.Actor, len(writers))
		}
		w := writers[c.Actor]
		device := w.DeviceIDs[c.Slot%len(w.DeviceIDs)]
		client, err := protocol.NormalizeTimestamp(m.ClientMutationTime)
		if err != nil {
			return encodedFamily{}, err
		}
		accepted, err := protocol.NormalizeTimestamp(c.AcceptedAt.UTC().Format(time.RFC3339Nano))
		if err != nil {
			return encodedFamily{}, err
		}
		meta := protocol.EntityMeta{ID: m.EntityID, FamilyID: familyID, EntityVersion: 1, CreatorAccountID: w.AccountID,
			LastEditorAccountID: w.AccountID, ClientMutationTime: client, AcceptedAt: accepted}
		var previous *care.Row
		stored, found := current[m.EntityID]
		switch m.Operation {
		case "create", "timer_start":
			if found {
				return encodedFamily{}, fmt.Errorf("loadseed: family %d creates %s twice", plan.Index, m.EntityID)
			}
		case "update", "delete":
			existing := entities[m.EntityID]
			if !found || existing.Meta.TombstonedAt != nil || m.ObservedEntityVersion != existing.Meta.EntityVersion {
				return encodedFamily{}, fmt.Errorf("loadseed: family %d %s of %s at version %d", plan.Index, m.Operation, m.EntityID, m.ObservedEntityVersion)
			}
			previous = &stored
			meta.EntityVersion = existing.Meta.EntityVersion + 1
			meta.CreatorAccountID = existing.Meta.CreatorAccountID
		default:
			return encodedFamily{}, fmt.Errorf("loadseed: family %d plans operation %s", plan.Index, m.Operation)
		}
		entity := protocol.CanonicalEntity{EntityKind: m.EntityKind, Meta: meta, Payload: m.Payload}
		if m.Operation == "delete" {
			tombstoned := accepted
			entity.Meta.TombstonedAt, entity.Payload = &tombstoned, nil
		}
		seq++
		row, err := care.Encode(entity, previous, care.Change{Seq: seq, ActorAccountID: w.AccountID, DeviceID: device, Operation: m.Operation,
			AcceptedAt: c.AcceptedAt})
		if err != nil {
			return encodedFamily{}, fmt.Errorf("loadseed: family %d: %w", plan.Index, err)
		}
		row.FamilyID = familyID
		delta = delta.Replace(previous, row)
		if !found {
			order = append(order, row.ID)
		}
		current[row.ID], entities[row.ID] = row, entity
		if !c.AcceptedAt.Before(receiptsSince) {
			cursor := protocol.Cursor{ServerInstanceID: server.InstanceID, RecoveryEpoch: server.RecoveryEpoch, FamilyID: familyID, Sequence: seq}
			outcome := protocol.Outcome{MutationID: m.MutationID, Status: "applied", CanonicalEntity: &entity,
				RelatedEntities: []protocol.CanonicalEntity{}, Cursor: &cursor}
			hash, err := protocol.CanonicalMutationHash(m)
			if err != nil {
				return encodedFamily{}, err
			}
			raw, err := json.Marshal(outcome)
			if err != nil {
				return encodedFamily{}, err
			}
			delta.ChargedBytes += care.ReceiptChargedBytes(raw)
			out.Receipts = append(out.Receipts, seededReceipt{ReceiptRow: care.ReceiptRow{DeviceID: device, MutationID: m.MutationID,
				AccountID: w.AccountID, Hash: hash, Outcome: outcome, AcceptedAt: c.AcceptedAt}, outcome: raw})
		}
	}
	out.Rows = make([]care.Row, len(order))
	for i, id := range order {
		out.Rows[i] = current[id]
	}
	if delta.Records < 0 || delta.ChargedBytes < 0 || delta.Entities < 0 || delta.WireBytes < 0 {
		return encodedFamily{}, fmt.Errorf("loadseed: family %d counters went negative: %+v", plan.Index, delta)
	}
	out.State = care.FamilyState{Sequence: seq, RecordCount: uint64(delta.Records), ChargedBytes: uint64(delta.ChargedBytes),
		EntityCount: uint64(delta.Entities), WireBytes: uint64(delta.WireBytes)}
	return out, nil
}

// careColumns are the care_entities columns COPY writes (the upsert's
// written columns, store.go writtenColumns).
var careColumns = []string{"family_id", "id", "kind", "baby_id", "entity_version", "changed_seq", "creator_account_id", "actor_account_id",
	"device_id", "operation", "accepted_at", "tombstoned_at", "started_at", "ended_at", "session_id", "timer_kind", "completed_record_id",
	"sort_order", "label", "name_hash", "breast_seconds", "bottle_volume_ml", "bottle_observations", "bottle_missing",
	"shortest_part_seconds", "wire", "created_device_id", "created_seq", "feed_kind"}

// careValues is r as COPY writes it, coded as the upsert codes it
// (store.go upsertArgs and columnCodes).
func careValues(r care.Row) ([]any, error) {
	kind, ok := care.KindCode(r.Kind)
	if !ok || len(r.Wire) == 0 {
		return nil, fmt.Errorf("loadseed: row %s kind %q", r.ID, r.Kind)
	}
	var timerKind, feedKind *int16
	if r.TimerKind != nil {
		code, ok := care.TimerKindCode(*r.TimerKind)
		if !ok {
			return nil, fmt.Errorf("loadseed: row %s timer kind %q", r.ID, *r.TimerKind)
		}
		timerKind = &code
	}
	if r.FeedKind != nil {
		code, ok := care.FeedKindCode(*r.FeedKind)
		if !ok {
			return nil, fmt.Errorf("loadseed: row %s feed kind %q", r.ID, *r.FeedKind)
		}
		feedKind = &code
	}
	var volume pgtype.Numeric
	if r.Derived.BottleVolumeML != nil {
		if err := volume.Scan(r.Derived.BottleVolumeML.String()); err != nil {
			return nil, err
		}
	}
	var createdDevice *string
	var createdSeq *int64
	if r.CreatedDeviceID != "" {
		device, seq := r.CreatedDeviceID, int64(r.CreatedSeq)
		createdDevice, createdSeq = &device, &seq
	}
	return []any{r.FamilyID, r.ID, kind, r.BabyID, int64(r.EntityVersion), int64(r.ChangedSeq), r.CreatorAccountID, r.ActorAccountID,
		r.DeviceID, r.Operation, r.AcceptedAt, r.TombstonedAt, r.StartedAt, r.EndedAt, r.SessionID, timerKind, r.CompletedRecordID,
		r.SortOrder, r.Label, r.NameHash, r.Derived.BreastSeconds, volume, r.Derived.BottleObservations, r.Derived.BottleMissing,
		r.Derived.ShortestPartSeconds, r.Wire, createdDevice, createdSeq, feedKind}, nil
}

var receiptColumns = []string{"device_id", "mutation_id", "family_id", "account_id", "semantic_hash", "outcome", "accepted_at"}

func receiptValues(familyID string, r seededReceipt) []any {
	return []any{r.DeviceID, r.MutationID, familyID, r.AccountID, r.Hash[:], json.RawMessage(r.outcome), r.AcceptedAt}
}

// registerTypes loads the enum types COPY encodes into conn's type map.
func registerTypes(ctx context.Context, conn *pgx.Conn, names ...string) error {
	for _, name := range names {
		if _, ok := conn.TypeMap().TypeForName(name); ok {
			continue
		}
		t, err := conn.LoadType(ctx, name)
		if err != nil {
			return fmt.Errorf("loadseed: load type %s: %w", name, err)
		}
		conn.TypeMap().RegisterType(t)
	}
	return nil
}

// copyCareRows writes rows into table (care_entities or one of its
// partitions) with COPY, on one connection of pool (the superuser's: COPY
// bypasses the tenant policies only for a role that bypasses row
// security).
func copyCareRows(ctx context.Context, pool *pgxpool.Pool, table string, rows []care.Row) (int64, error) {
	next := 0
	return copyFrom(ctx, pool, table, careColumns, pgx.CopyFromFunc(func() ([]any, error) {
		if next == len(rows) {
			return nil, nil
		}
		next++
		return careValues(rows[next-1])
	}))
}

// copyFrom runs one COPY into table on a connection of pool.
func copyFrom(ctx context.Context, pool *pgxpool.Pool, table string, columns []string, source pgx.CopyFromSource) (int64, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	if err := registerTypes(ctx, conn.Conn(), "app.care_operation", "app.session_scope", "app.member_role", "app.session_status"); err != nil {
		return 0, err
	}
	n, err := conn.Conn().CopyFrom(ctx, pgx.Identifier{table}, columns, source)
	if err != nil {
		return n, fmt.Errorf("loadseed: copy into %s: %w", table, err)
	}
	return n, nil
}

// channelSource is a COPY source fed batch by batch from a channel until
// it closes (or ctx ends).
type channelSource[T any] struct {
	ctx     context.Context
	batches <-chan []T
	batch   []T
	at      int
	values  func(T) ([]any, error)
	current []any
	err     error
}

func (s *channelSource[T]) Next() bool {
	for s.at >= len(s.batch) {
		select {
		case <-s.ctx.Done():
			s.err = s.ctx.Err()
			return false
		case batch, ok := <-s.batches:
			if !ok {
				// A channel closed because the stage failed ends the COPY
				// with that error (it rolls back), never as a clean EOF.
				s.err = s.ctx.Err()
				return false
			}
			s.batch, s.at = batch, 0
		}
	}
	s.current, s.err = s.values(s.batch[s.at])
	s.at++
	return s.err == nil
}

func (s *channelSource[T]) Values() ([]any, error) { return s.current, s.err }
func (s *channelSource[T]) Err() error             { return s.err }

// careLoad is the care stage's result per family, and the baby key's
// validation, which goes on after the stage (see loadCare).
type careLoad struct {
	states   []care.FamilyState
	entities int
	receipts int
	plans    []familyCare
	babyKey  <-chan error
}

// waitBabyKey waits until the baby key is back and returns its error (nil
// once it has been waited for, or when no validation was started).
func (c *careLoad) waitBabyKey() error {
	if c.babyKey == nil {
		return nil
	}
	err := <-c.babyKey
	c.babyKey = nil
	return err
}

// familyCare is what later stages read of a family's plan: its babies and
// running timers.
type familyCare struct {
	Babies []BabyPlan
}

// careFKName is the self-referencing key the load drops and re-adds.
const careFKName = "care_entities_baby_fk"

// careKeySettings are the planner settings the baby key's validation runs
// under (SET LOCAL, in the transaction that adds the key). PostgreSQL
// validates a foreign key with one anti-join per referencing partition:
// care_entities references itself, so each of the 16 partitions' rows are
// checked against the whole table. Left to itself the planner hashes the
// referenced table for every partition, 16 scans of all the rows (820 s at
// 10,000 families, 11.5 M rows). Without hash and merge joins it probes the
// referenced partition's primary key once per row instead, and the hash
// partitioning prunes the other 15 (the family is in the join clause), so
// the cost is linear in the rows.
var careKeySettings = []string{"enable_hashjoin = off", "enable_mergejoin = off"}

// careKey is the baby key as the care stage defers it.
func careKey(definition string) deferredConstraint {
	return deferredConstraint{table: "public.care_entities", name: careFKName, definition: definition, settings: careKeySettings}
}

// addCareKey adds the baby key back under its recorded definition, validated
// (a plain ADD CONSTRAINT: the key is never NOT VALID, and a dangling baby
// fails it). The settings last only for its transaction.
func addCareKey(ctx context.Context, pool *pgxpool.Pool, definition string) error {
	return addConstraint(ctx, pool, careKey(definition))
}

// careDeferral is what the care stage keeps out of its COPY: the secondary
// indexes of care_entities (its partitioned primary key and sequence key
// stay: the baby key validates against the first) and of care_receipts,
// the receipts' primary key (random device and mutation IDs) and family
// key, and the two keys that reference care_entities, the baby key and
// import_baby_mappings' shared baby (TRUNCATE, which COPY FREEZE needs,
// refuses a referenced partition; import_baby_mappings is empty on a fresh
// stack).
var careDeferral = deferral{
	secondaryIndexesOf: []string{"public.care_entities", "public.care_receipts"},
	constraints: []deferredConstraint{{table: "public.care_entities", name: careFKName, settings: careKeySettings},
		{table: "public.import_baby_mappings", name: "import_mappings_shared_baby_fk"},
		{table: "public.care_receipts", name: "care_receipts_pkey"}, {table: "public.care_receipts", name: "care_receipts_family_id_fkey"}},
}

// restoreWorkers is how many indexes a restore builds at once (the load
// node's database has 4 CPUs).
const restoreWorkers = 4

// loadCare plans and encodes every family's history (spec.Workers at a
// time) and loads it with COPY as the superuser: careDeferral dropped, one
// COPY FREEZE per hash partition (copyFrozen) fed as families are encoded,
// receipts in a few more, then careDeferral restored from its recorded
// definitions (indexes built from the rows, the keys added validated, see
// careKeySettings), and the families' sequences and counters set in one
// UPDATE.
//
// The baby key is added last and in the background: its validation reads
// every care row on one of the database's CPUs and blocks only writes to
// care_entities, which no later stage makes, so it runs while the side
// stage (reads of care through the restored indexes) does. The caller
// waits for it with waitBabyKey before anything vacuums or verifies care.
func loadCare(ctx context.Context, env Environment, spec Spec, families []*familyState, server protocol.ServerInfo) (careLoad, error) {
	started := time.Now()
	seedCtx := ctx
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	result := careLoad{states: make([]care.FamilyState, len(families)), plans: make([]familyCare, len(families))}
	partitions, err := familyPartitions(ctx, env.Superuser, families)
	if err != nil {
		return result, err
	}
	deferred, err := recordDeferral(ctx, env.Superuser, careDeferral)
	if err != nil {
		return result, err
	}
	if err := deferred.drop(ctx, env.Superuser); err != nil {
		return result, err
	}
	restored := false
	defer func() {
		if !restored { // a failed load leaves the schema whole
			env.logger().Warn("loadseed: the care stage failed: restoring the care indexes and keys (minutes at 10,000 families); do not kill the seed")
			if err := deferred.restore(context.WithoutCancel(ctx), env.Superuser, restoreWorkers); err != nil {
				env.logger().Error("loadseed: restoring the care schema failed; recreate the stack", "error", err)
			}
		}
	}()

	rowChannels := make([]chan []care.Row, partitionCount)
	var copies sync.WaitGroup
	var copied [partitionCount]int64
	var copiedReceipts [receiptCopies]int64
	fail := func(err error) { cancel(err) }
	for p := range partitionCount {
		rowChannels[p] = make(chan []care.Row, 8)
		copies.Add(1)
		go func() {
			defer copies.Done()
			source := &channelSource[care.Row]{ctx: ctx, batches: rowChannels[p], values: careValues}
			n, err := copyFrozen(ctx, env.Superuser, fmt.Sprintf("care_entities_p%02d", p), careColumns, source)
			if err == nil {
				err = source.err
			}
			if err != nil {
				fail(err)
				drain(rowChannels[p])
			}
			copied[p] = n
		}()
	}
	receiptBatches := make(chan []receiptRowOf, 64)
	for c := range receiptCopies {
		copies.Add(1)
		go func() {
			defer copies.Done()
			source := &channelSource[receiptRowOf]{ctx: ctx, batches: receiptBatches, values: func(r receiptRowOf) ([]any, error) {
				return receiptValues(r.familyID, r.receipt), nil
			}}
			n, err := copyFrom(ctx, env.Superuser, "care_receipts", receiptColumns, source)
			if err == nil {
				err = source.err
			}
			if err != nil {
				fail(err)
				drain(receiptBatches)
			}
			copiedReceipts[c] = n
		}()
	}

	err = forEach(ctx, spec.Workers, len(families), func(i int) error {
		f := families[i]
		plan := planFamily(spec, i)
		encoded, err := encodeFamily(plan, f.FamilyID, f.writers(), server, spec.AsOf.Add(-receiptRetention))
		if err != nil {
			return err
		}
		result.states[i] = encoded.State
		result.plans[i] = familyCare{Babies: plan.Babies}
		select {
		case rowChannels[partitions[i]] <- encoded.Rows:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
		if len(encoded.Receipts) > 0 {
			batch := make([]receiptRowOf, len(encoded.Receipts))
			for j, r := range encoded.Receipts {
				batch[j] = receiptRowOf{familyID: f.FamilyID, receipt: r}
			}
			select {
			case receiptBatches <- batch:
			case <-ctx.Done():
				return context.Cause(ctx)
			}
		}
		return nil
	})
	if err != nil {
		// Cancel before closing the channels, so every COPY ends with the
		// error and rolls back: a failed encode commits no partial care.
		cancel(err)
	}
	for p := range partitionCount {
		close(rowChannels[p])
	}
	close(receiptBatches)
	copies.Wait()
	if err != nil {
		return result, err
	}
	if cause := context.Cause(ctx); cause != nil {
		return result, cause
	}
	for _, n := range copied {
		result.entities += int(n)
	}
	for _, n := range copiedReceipts {
		result.receipts += int(n)
	}
	logger := env.logger()
	logger.Info("loadseed: care copied", "entities", result.entities, "receipts", result.receipts, "seconds", time.Since(started).Seconds())
	step := time.Now()
	rest, babyKey := deferred.split(careFKName)
	if err := rest.restore(ctx, env.Superuser, restoreWorkers); err != nil {
		return result, err
	}
	logger.Info("loadseed: care indexes and keys restored", "seconds", time.Since(step).Seconds())
	step = time.Now()
	if err := setFamilyCounters(ctx, env.Superuser, families, result.states); err != nil {
		return result, err
	}
	logger.Info("loadseed: family counters set", "seconds", time.Since(step).Seconds())
	restored = true
	done := make(chan error, 1)
	result.babyKey = done
	go func() {
		step := time.Now()
		err := babyKey.restore(seedCtx, env.Superuser, restoreWorkers)
		if err != nil {
			err = fmt.Errorf("loadseed: re-add %s: %w", careFKName, err)
			if seedCtx.Err() != nil { // cancelled: the key still comes back
				logger.Warn("loadseed: the seed was cancelled: re-adding the care baby key (minutes at 10,000 families); do not kill the seed")
				err = errors.Join(err, babyKey.restore(context.WithoutCancel(seedCtx), env.Superuser, restoreWorkers))
			}
		}
		if err != nil {
			logger.Error("loadseed: the care baby key could not be re-added; recreate the stack", "seconds", time.Since(step).Seconds(), "error", err)
		} else {
			logger.Info("loadseed: care baby key re-added", "seconds", time.Since(step).Seconds())
		}
		done <- err
	}()
	return result, nil
}

// receiptRetention is how long the receipt sweep keeps a receipt
// (maintenance.ReceiptSweep: 30 days after its acceptance).
const receiptRetention = 30 * 24 * time.Hour

// partitionCount is care_entities' hash partitions (care_entities_p00..p15).
const partitionCount = 16

// receiptCopies is how many COPY streams write care_receipts.
const receiptCopies = 4

type receiptRowOf struct {
	familyID string
	receipt  seededReceipt
}

func drain[T any](c <-chan T) {
	go func() {
		for range c {
		}
	}()
}

// familyPartitions is each family's care_entities hash partition, as
// PostgreSQL routes it (satisfies_hash_partition over the 16 remainders).
func familyPartitions(ctx context.Context, pool *pgxpool.Pool, families []*familyState) ([]int, error) {
	ids := make([]string, len(families))
	for i, f := range families {
		ids[i] = f.FamilyID
	}
	rows, err := pool.Query(ctx, `SELECT f.ord::int, r FROM unnest($1::uuid[]) WITH ORDINALITY AS f(id, ord), generate_series(0, $2::int - 1) AS r
		WHERE satisfies_hash_partition('public.care_entities'::regclass, $2::int, r, f.id)`, ids, partitionCount)
	if err != nil {
		return nil, fmt.Errorf("loadseed: family partitions: %w", err)
	}
	out := make([]int, len(families))
	seen := make([]bool, len(families))
	var ord, remainder int
	if _, err := pgx.ForEachRow(rows, []any{&ord, &remainder}, func() error {
		if ord < 1 || ord > len(families) || seen[ord-1] {
			return fmt.Errorf("loadseed: family %d routed twice", ord)
		}
		seen[ord-1], out[ord-1] = true, remainder
		return nil
	}); err != nil {
		return nil, fmt.Errorf("loadseed: family partitions: %w", err)
	}
	for i, ok := range seen {
		if !ok {
			return nil, fmt.Errorf("loadseed: family %s has no partition", ids[i])
		}
	}
	return out, nil
}

// setFamilyCounters sets every family's sequence and four care counters in
// one statement, refusing any family that already holds changes.
func setFamilyCounters(ctx context.Context, pool *pgxpool.Pool, families []*familyState, states []care.FamilyState) error {
	n := len(families)
	ids := make([]string, n)
	seqs, records, charged, entities, wire := make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n)
	for i, f := range families {
		s := states[i]
		ids[i] = f.FamilyID
		seqs[i], records[i], charged[i], entities[i], wire[i] = int64(s.Sequence), int64(s.RecordCount), int64(s.ChargedBytes), int64(s.EntityCount), int64(s.WireBytes)
	}
	tag, err := pool.Exec(ctx, `UPDATE families f SET next_sequence = u.seq, care_record_count = u.records, care_charged_bytes = u.charged,
		care_entity_count = u.entities, care_wire_bytes = u.wire
		FROM unnest($1::uuid[], $2::bigint[], $3::bigint[], $4::bigint[], $5::bigint[], $6::bigint[]) AS u(id, seq, records, charged, entities, wire)
		WHERE f.id = u.id AND f.next_sequence = 0 AND f.care_entity_count = 0 AND f.care_charged_bytes = 0`, ids, seqs, records, charged, entities, wire)
	if err != nil {
		return fmt.Errorf("loadseed: set family counters: %w", err)
	}
	if tag.RowsAffected() != int64(n) {
		return fmt.Errorf("loadseed: set the counters of %d of %d families (the others already hold changes)", tag.RowsAffected(), n)
	}
	return nil
}

// forEach runs work(i) for i in [0, n) on workers goroutines, stopping at
// the first error.
func forEach(ctx context.Context, workers, n int, work func(i int) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	next := make(chan int)
	var wg sync.WaitGroup
	for range min(workers, max(n, 1)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				if err := work(i); err != nil {
					cancel(err)
					return
				}
			}
		}()
	}
feed:
	for i := range n {
		select {
		case next <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(next)
	wg.Wait()
	return context.Cause(ctx)
}
