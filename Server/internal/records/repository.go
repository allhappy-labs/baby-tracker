package records

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/diagnostics"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

var (
	ErrNotFound           = errors.New("record_not_found")
	ErrQuotaExceeded      = errors.New("family_record_quota_exceeded")
	ErrFeatureUnavailable = errors.New("feature_unavailable")
)

const deletionReserveBytes int64 = 4096

type Guard interface {
	WithAuthorizedTransaction(context.Context, access.Principal, string, string, func(pgx.Tx, access.Grant) error) error
}

type ApplyCommand struct {
	Grant            access.Grant
	DeviceID         string
	Mutation         protocol.Mutation
	AcceptedAt       time.Time
	ServerInstanceID string
	RecoveryEpoch    uint64
	Limits           protocol.Limits
}

type Repository struct{}

func NewRepository() *Repository { return &Repository{} }

// ChargeImport accounts for retained staging, decisions, and ID markers under
// the same family lock and transaction as their writes.
func (r *Repository) ChargeImport(ctx context.Context, tx pgx.Tx, familyID string, bytes int64, limits protocol.Limits) error {
	return chargeUsage(ctx, tx, familyID, 0, bytes, limits, false)
}

func (r *Repository) GetCanonical(ctx context.Context, tx pgx.Tx, kind, familyID, id string) (protocol.CanonicalEntity, protocol.Cursor, error) {
	entity, err := r.get(ctx, tx, kind, familyID, id)
	if err != nil {
		return entity, protocol.Cursor{}, err
	}
	table, err := tableFor(kind)
	if err != nil {
		return entity, protocol.Cursor{}, err
	}
	var sequence uint64
	err = tx.QueryRow(ctx, `SELECT last_sequence FROM `+table+` WHERE family_id=$1 AND id=$2`, familyID, id).Scan(&sequence)
	return entity, protocol.Cursor{FamilyID: familyID, Sequence: sequence}, err
}

func EntityStorageBytes(entity protocol.CanonicalEntity) (int64, error) {
	raw, err := json.Marshal(entity)
	if err != nil {
		return 0, err
	}
	return int64(len(raw)) + 256, nil
}

func JournalStorageBytes(change protocol.JournalChange) (int64, error) {
	raw, err := json.Marshal(change)
	if err != nil {
		return 0, err
	}
	return int64(len(raw)) + 384, nil
}
func ReceiptStorageBytes(outcome protocol.Outcome) (int64, error) {
	raw, err := json.Marshal(outcome)
	if err != nil {
		return 0, err
	}
	return int64(len(raw)) + 256, nil
}
func DeletionReserveStorageBytes() int64 { return deletionReserveBytes }

func rejection(m protocol.Mutation, status, code string, entity *protocol.CanonicalEntity) protocol.Outcome {
	return protocol.Outcome{MutationID: m.MutationID, Status: status, CanonicalEntity: entity,
		RelatedEntities: []protocol.CanonicalEntity{}, Code: &code}
}

func (r *Repository) Apply(ctx context.Context, tx pgx.Tx, command ApplyCommand) (protocol.Outcome, error) {
	m := command.Mutation
	canonicalMutation, err := protocol.CanonicalMutationBytes(m)
	if err != nil {
		return protocol.Outcome{}, err
	}
	if len(canonicalMutation) > command.Limits.SyncMutationBytes {
		return rejection(m, "rejected", "mutation_too_large", nil), nil
	}
	if err := ValidateMutation(m, command.Limits); err != nil {
		return rejection(m, "rejected", semanticRejectionCode(err), nil), nil
	}
	if m.EntityKind == "timer" {
		return r.ApplyTimer(ctx, tx, command)
	}
	if m.EntityKind == "baby" && m.Operation == "delete" {
		return r.DeleteBaby(ctx, tx, command)
	}
	kind, err := FindEntityKind(ctx, tx, command.Grant.FamilyID, m.EntityID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return protocol.Outcome{}, err
	}
	if m.Operation == "create" && err == nil {
		code := "entity_exists"
		if kind != m.EntityKind {
			code = "entity_kind_collision"
		}
		return rejection(m, "rejected", code, nil), nil
	}
	if m.Operation != "create" && errors.Is(err, ErrNotFound) {
		return rejection(m, "rejected", "entity_not_found", nil), nil
	}
	if m.Operation != "create" && kind != m.EntityKind {
		return rejection(m, "rejected", "entity_kind_collision", nil), nil
	}

	accepted, err := protocol.NormalizeTimestamp(command.AcceptedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return protocol.Outcome{}, err
	}
	client, err := protocol.NormalizeTimestamp(m.ClientMutationTime)
	if err != nil {
		return protocol.Outcome{}, err
	}
	meta := protocol.EntityMeta{ID: m.EntityID, FamilyID: command.Grant.FamilyID,
		EntityVersion: 1, CreatorAccountID: command.Grant.AccountID,
		LastEditorAccountID: command.Grant.AccountID, ClientMutationTime: client, AcceptedAt: accepted}
	var existing *protocol.CanonicalEntity
	if m.Operation != "create" {
		entity, getErr := r.get(ctx, tx, m.EntityKind, command.Grant.FamilyID, m.EntityID)
		if getErr != nil {
			return protocol.Outcome{}, getErr
		}
		existing = &entity
		meta.EntityVersion = entity.Meta.EntityVersion + 1
		meta.CreatorAccountID = entity.Meta.CreatorAccountID
	}

	if m.EntityKind != "baby" {
		babyID := mutationBabyID(m)
		if existing != nil {
			existingBabyID := mutationBabyIDFromEntity(*existing)
			if existing.Meta.TombstonedAt != nil {
				existingBabyID, err = storedBabyID(ctx, tx, command.Grant.FamilyID, m.EntityKind, m.EntityID)
				if err != nil {
					return protocol.Outcome{}, err
				}
			}
			if existingBabyID != babyID {
				return rejection(m, "rejected", "record_parent_mismatch", existing), nil
			}
		}
		baby, getErr := r.get(ctx, tx, "baby", command.Grant.FamilyID, babyID)
		if errors.Is(getErr, ErrNotFound) {
			return rejection(m, "rejected", "baby_not_found", nil), nil
		}
		if getErr != nil {
			return protocol.Outcome{}, getErr
		}
		if baby.Meta.TombstonedAt != nil {
			return rejection(m, "rejected", "baby_deleted", &baby), nil
		}
	} else if m.Operation == "create" {
		var live int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM babies WHERE family_id=$1 AND tombstoned_at IS NULL`, command.Grant.FamilyID).Scan(&live); err != nil {
			return protocol.Outcome{}, err
		}
		if live >= command.Limits.MaxLiveBabies {
			return protocol.Outcome{}, ErrQuotaExceeded
		}
	}
	if existing != nil && existing.Meta.TombstonedAt != nil {
		return rejection(m, "superseded", "entity_deleted", existing), nil
	}
	if m.EntityKind == "feed" && m.Operation != "delete" {
		feed := m.Payload.(protocol.FeedPayload)
		if feed.FeedingSessionID != nil {
			mismatch, err := sessionOwnerMismatch(ctx, tx, command.Grant.FamilyID, feed.BabyID, "feed", *feed.FeedingSessionID)
			if err != nil {
				return protocol.Outcome{}, err
			}
			if mismatch {
				return rejection(m, "rejected", "record_parent_mismatch", existing), nil
			}
		}
	}
	if m.EntityKind == "sleep" && m.Operation != "delete" {
		sleep := m.Payload.(protocol.SleepPayload)
		if sleep.SleepSessionID != nil {
			mismatch, err := sessionOwnerMismatch(ctx, tx, command.Grant.FamilyID, sleep.BabyID, "sleep", *sleep.SleepSessionID)
			if err != nil {
				return protocol.Outcome{}, err
			}
			if mismatch {
				return rejection(m, "rejected", "record_parent_mismatch", existing), nil
			}
		}
	}

	if m.EntityKind == "measurement" && m.Operation != "delete" {
		p := m.Payload.(protocol.MeasurementPayload)
		measured, _ := time.Parse(time.RFC3339Nano, p.MeasuredAt)
		baby, _, err := r.GetBaby(ctx, tx, command.Grant.FamilyID, p.BabyID)
		if err != nil {
			return protocol.Outcome{}, err
		}
		zone, _ := time.LoadLocation(p.TimeZoneID)
		if measured.After(command.AcceptedAt) || measured.In(zone).Format("2006-01-02") < baby.BirthDate {
			return rejection(m, "rejected", "invalid_timestamp", existing), nil
		}
		if existing != nil && existing.Payload.(protocol.MeasurementPayload).LoggedAt != p.LoggedAt {
			return rejection(m, "rejected", "record_logged_at_mismatch", existing), nil
		}
	}
	if existing != nil && m.EntityKind == "supplement" && m.Operation == "update" {
		old := existing.Payload.(protocol.SupplementPayload)
		next := m.Payload.(protocol.SupplementPayload)
		if old.LoggedAt != next.LoggedAt {
			return rejection(m, "rejected", "record_logged_at_mismatch", existing), nil
		}
	}

	payload := m.Payload
	// An update from a build that predates icons leaves `icon` out. The
	// stored icon is kept here, not only in SQL, so the journal entry other
	// phones pull and the response carry it too.
	if next, ok := payload.(protocol.BabyPayload); ok && next.Icon == nil && existing != nil {
		if old, ok := existing.Payload.(protocol.BabyPayload); ok && old.Icon != nil {
			next.Icon = old.Icon
			payload = next
		}
	}
	var entity protocol.CanonicalEntity
	if m.Operation == "delete" {
		meta.TombstonedAt = &accepted
		entity = protocol.CanonicalEntity{EntityKind: m.EntityKind, Meta: meta, Payload: nil}
	} else {
		entity = protocol.CanonicalEntity{EntityKind: m.EntityKind, Meta: meta, Payload: payload}
	}
	if !entityFitsMinimumPage(command, entity) {
		return rejection(m, "rejected", "mutation_too_large", existing), nil
	}
	bytes, err := EntityStorageBytes(entity)
	if err != nil {
		return protocol.Outcome{}, err
	}
	oldBytes := int64(0)
	if existing != nil {
		oldBytes, err = storedEntityBytes(ctx, tx, command.Grant.FamilyID, m.EntityKind, m.EntityID)
		if err != nil {
			return protocol.Outcome{}, err
		}
	}
	if err := r.put(ctx, tx, entity, bytes); err != nil {
		return protocol.Outcome{}, err
	}
	changes, err := AppendJournal(ctx, tx, command.Grant, command.DeviceID, m, []protocol.CanonicalEntity{entity}, command.ServerInstanceID, command.RecoveryEpoch)
	if err != nil {
		return protocol.Outcome{}, err
	}
	entityDelta := bytes - oldBytes
	countDelta, reserveDelta := int64(0), int64(0)
	if m.Operation == "create" {
		reserveDelta = deletionReserveBytes
	}
	if m.Operation == "delete" {
		reserveDelta = -deletionReserveBytes
	}
	if m.EntityKind != "baby" {
		if m.Operation == "create" {
			countDelta = 1
		}
		if m.Operation == "delete" {
			countDelta = -1
		}
	}
	journalBytes, err := JournalStorageBytes(changes[0])
	if err != nil {
		return protocol.Outcome{}, err
	}
	if err := chargeUsage(ctx, tx, command.Grant.FamilyID, countDelta, entityDelta+journalBytes+reserveDelta,
		command.Limits, m.Operation == "delete"); err != nil {
		return protocol.Outcome{}, err
	}
	last := changes[len(changes)-1].Cursor
	outcome := protocol.Outcome{MutationID: m.MutationID, Status: "applied", CanonicalEntity: &entity,
		RelatedEntities: []protocol.CanonicalEntity{}, Cursor: &last}
	if existing != nil && m.ObservedEntityVersion < existing.Meta.EntityVersion {
		outcome.OverwroteNewerVersion = true
	}
	return outcome, nil
}

func entityFitsMinimumPage(command ApplyCommand, entity protocol.CanonicalEntity) bool {
	maximumToken := strings.Repeat("x", protocol.MaximumPageTokenLength)
	highWater := protocol.Cursor{ServerInstanceID: command.ServerInstanceID, RecoveryEpoch: command.RecoveryEpoch,
		FamilyID: command.Grant.FamilyID, Sequence: protocol.MaxSafeInteger}
	change := protocol.JournalChange{Cursor: highWater, EntityKind: entity.EntityKind, EntityID: entity.Meta.ID,
		EntityVersion: entity.Meta.EntityVersion, ActorAccountID: command.Grant.AccountID, DeviceID: command.DeviceID,
		Operation: command.Mutation.Operation, AcceptedAt: entity.Meta.AcceptedAt, Optional: false, SnapshotOrTombstone: entity}
	server := protocol.ServerInfo{InstanceID: command.ServerInstanceID, RecoveryEpoch: command.RecoveryEpoch}
	if protocol.IncrementalPageEncodedBytes(server, command.Grant.FamilyID, protocol.MaxSafeInteger,
		[]protocol.JournalChange{change}, &maximumToken) > protocol.MinimumSyncPageBytes {
		return false
	}
	return protocol.SnapshotPageEncodedBytes(server, "ffffffff-ffff-4fff-bfff-ffffffffffff", highWater,
		[]protocol.CanonicalEntity{entity}, &maximumToken) <= protocol.MinimumSyncPageBytes
}

func mutationBabyID(m protocol.Mutation) string {
	switch p := m.Payload.(type) {
	case protocol.FeedPayload:
		return p.BabyID
	case protocol.SleepPayload:
		return p.BabyID
	case protocol.GrowthProfilePayload:
		return p.BabyID
	case protocol.MeasurementPayload:
		return p.BabyID
	case protocol.SupplementPayload:
		return p.BabyID
	case protocol.DiaperPayload:
		return p.BabyID
	case protocol.DeletePayload:
		if p.BabyID != nil {
			return *p.BabyID
		}
	}
	return ""
}

func mutationBabyIDFromEntity(entity protocol.CanonicalEntity) string {
	switch p := entity.Payload.(type) {
	case protocol.FeedPayload:
		return p.BabyID
	case protocol.SleepPayload:
		return p.BabyID
	case protocol.GrowthProfilePayload:
		return p.BabyID
	case protocol.MeasurementPayload:
		return p.BabyID
	case protocol.SupplementPayload:
		return p.BabyID
	case protocol.DiaperPayload:
		return p.BabyID
	}
	return ""
}

func FindEntityKind(ctx context.Context, tx pgx.Tx, familyID, entityID string) (string, error) {
	var kind string
	err := tx.QueryRow(ctx, `SELECT kind FROM (
		SELECT 'baby' kind FROM babies WHERE family_id=$1 AND id=$2 UNION ALL
		SELECT 'feed' FROM feed_entries WHERE family_id=$1 AND id=$2 UNION ALL
		SELECT 'sleep' FROM sleep_entries WHERE family_id=$1 AND id=$2 UNION ALL
		SELECT 'growth_profile' FROM growth_profiles WHERE family_id=$1 AND id=$2 UNION ALL
		SELECT 'measurement' FROM measurement_entries WHERE family_id=$1 AND id=$2 UNION ALL
		SELECT 'supplement' FROM supplement_entries WHERE family_id=$1 AND id=$2 UNION ALL
		SELECT 'diaper' FROM diaper_entries WHERE family_id=$1 AND id=$2 UNION ALL
		SELECT 'timer' FROM active_timers WHERE family_id=$1 AND id=$2
	) entities LIMIT 1`, familyID, entityID).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return kind, err
}

func storedEntityBytes(ctx context.Context, tx pgx.Tx, familyID, kind, entityID string) (int64, error) {
	table, err := tableFor(kind)
	if err != nil {
		return 0, err
	}
	var value int64
	err = tx.QueryRow(ctx, "SELECT storage_bytes FROM "+table+" WHERE family_id=$1 AND id=$2", familyID, entityID).Scan(&value)
	return value, err
}

func storedBabyID(ctx context.Context, tx pgx.Tx, familyID, kind, entityID string) (string, error) {
	table, err := tableFor(kind)
	if err != nil || kind == "baby" {
		return "", protocol.ErrUnsupportedResource
	}
	var value string
	err = tx.QueryRow(ctx, "SELECT baby_id::text FROM "+table+" WHERE family_id=$1 AND id=$2", familyID, entityID).Scan(&value)
	return value, err
}

func tableFor(kind string) (string, error) {
	switch kind {
	case "baby":
		return "babies", nil
	case "feed":
		return "feed_entries", nil
	case "sleep":
		return "sleep_entries", nil
	case "growth_profile":
		return "growth_profiles", nil
	case "measurement":
		return "measurement_entries", nil
	case "supplement":
		return "supplement_entries", nil
	case "diaper":
		return "diaper_entries", nil
	case "timer":
		return "active_timers", nil
	}
	return "", protocol.ErrUnsupportedResource
}

func chargeUsage(ctx context.Context, tx pgx.Tx, familyID string, countDelta, byteDelta int64, limits protocol.Limits, allowDelete bool) error {
	var count, bytes int64
	err := tx.QueryRow(ctx, `INSERT INTO family_record_usage(family_id,canonical_record_count,charged_storage_bytes,updated_at)
		VALUES($1,0,0,now()) ON CONFLICT(family_id) DO UPDATE SET updated_at=EXCLUDED.updated_at
		RETURNING canonical_record_count,charged_storage_bytes`, familyID).Scan(&count, &bytes)
	if err != nil {
		return err
	}
	nextCount, nextBytes := count+countDelta, bytes+byteDelta
	if nextCount < 0 || nextBytes < 0 {
		return fmt.Errorf("invalid family usage")
	}
	if !allowDelete && (nextCount > limits.MaxCanonicalRecordsPerFamily || nextBytes > limits.MaxChargedStorageBytesPerFamily) {
		return ErrQuotaExceeded
	}
	_, err = tx.Exec(ctx, `UPDATE family_record_usage SET canonical_record_count=$2,charged_storage_bytes=$3,updated_at=now() WHERE family_id=$1`, familyID, nextCount, nextBytes)
	return err
}

func (r *Repository) get(ctx context.Context, tx pgx.Tx, kind, familyID, id string) (protocol.CanonicalEntity, error) {
	switch kind {
	case "baby":
		p, m, e := r.GetBaby(ctx, tx, familyID, id)
		entity := protocol.CanonicalEntity{EntityKind: kind, Meta: m, Payload: p}
		if m.TombstonedAt != nil {
			entity.Payload = nil
		}
		return entity, e
	case "feed":
		p, m, e := r.GetFeed(ctx, tx, familyID, id)
		entity := protocol.CanonicalEntity{EntityKind: kind, Meta: m, Payload: p}
		if m.TombstonedAt != nil {
			entity.Payload = nil
		}
		return entity, e
	case "sleep":
		p, m, e := r.GetSleep(ctx, tx, familyID, id)
		entity := protocol.CanonicalEntity{EntityKind: kind, Meta: m, Payload: p}
		if m.TombstonedAt != nil {
			entity.Payload = nil
		}
		return entity, e
	case "growth_profile":
		p, m, e := r.GetGrowthProfile(ctx, tx, familyID, id)
		entity := protocol.CanonicalEntity{EntityKind: kind, Meta: m, Payload: p}
		if m.TombstonedAt != nil {
			entity.Payload = nil
		}
		return entity, e
	case "measurement":
		p, m, e := r.GetMeasurement(ctx, tx, familyID, id)
		entity := protocol.CanonicalEntity{EntityKind: kind, Meta: m, Payload: p}
		if m.TombstonedAt != nil {
			entity.Payload = nil
		}
		return entity, e
	case "supplement":
		p, m, e := r.GetSupplement(ctx, tx, familyID, id)
		entity := protocol.CanonicalEntity{EntityKind: kind, Meta: m, Payload: p}
		if m.TombstonedAt != nil {
			entity.Payload = nil
		}
		return entity, e
	case "diaper":
		p, m, e := r.GetDiaper(ctx, tx, familyID, id)
		entity := protocol.CanonicalEntity{EntityKind: kind, Meta: m, Payload: p}
		if m.TombstonedAt != nil {
			entity.Payload = nil
		}
		return entity, e
	case "timer":
		record, err := r.GetTimer(ctx, tx, familyID, id)
		return record.Canonical(), err
	default:
		return protocol.CanonicalEntity{}, protocol.ErrUnsupportedResource
	}
}

func (r *Repository) put(ctx context.Context, tx pgx.Tx, entity protocol.CanonicalEntity, bytes int64) error {
	switch payload := entity.Payload.(type) {
	case protocol.BabyPayload:
		_, err := r.putBaby(ctx, tx, entity.Meta, payload, bytes)
		return err
	case protocol.FeedPayload:
		_, err := r.putFeed(ctx, tx, entity.Meta, payload, bytes)
		return err
	case protocol.SleepPayload:
		_, err := r.putSleep(ctx, tx, entity.Meta, payload, bytes)
		return err
	case protocol.GrowthProfilePayload:
		_, err := r.putGrowthProfile(ctx, tx, entity.Meta, payload, bytes)
		return err
	case protocol.MeasurementPayload:
		_, err := r.putMeasurement(ctx, tx, entity.Meta, payload, bytes)
		return err
	case protocol.SupplementPayload:
		_, err := r.putSupplement(ctx, tx, entity.Meta, payload, bytes)
		return err
	case protocol.DiaperPayload:
		_, err := r.putDiaper(ctx, tx, entity.Meta, payload, bytes)
		return err
	case nil:
		return r.putTombstone(ctx, tx, entity.Meta, entity.EntityKind, bytes)
	default:
		return protocol.ErrUnsupportedResource
	}
}

func (r *Repository) putTombstone(ctx context.Context, tx pgx.Tx, meta protocol.EntityMeta, kind string, bytes int64) error {
	table, err := tableFor(kind)
	if err != nil {
		return err
	}
	if kind == "feed" {
		if _, err = tx.Exec(ctx, `DELETE FROM feed_components WHERE family_id=$1 AND feed_id=$2`, meta.FamilyID, meta.ID); err != nil {
			return err
		}
	}
	if kind == "timer" {
		if _, err = tx.Exec(ctx, `DELETE FROM timer_components WHERE family_id=$1 AND timer_id=$2`, meta.FamilyID, meta.ID); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE `+table+` SET entity_version=$3,last_editor_account_id=$4,client_mutation_time=$5,
		accepted_at=$6,tombstoned_at=$7,last_sequence=0,storage_bytes=$8 WHERE family_id=$1 AND id=$2`,
		meta.FamilyID, meta.ID, meta.EntityVersion, meta.LastEditorAccountID, meta.ClientMutationTime, meta.AcceptedAt, meta.TombstonedAt, bytes)
	return err
}

func LockFamilyForUpdate(ctx context.Context, tx pgx.Tx, familyID string) error {
	start := diagnostics.Start(ctx)
	defer func() { diagnostics.CaptureSince(ctx, diagnostics.FamilyFence, start) }()
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM families WHERE id=$1 FOR UPDATE`, familyID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (r *Repository) ChargeReceipt(ctx context.Context, tx pgx.Tx, familyID string, outcome protocol.Outcome, limits protocol.Limits, quotaExempt bool) error {
	bytes, err := ReceiptStorageBytes(outcome)
	if err != nil {
		return err
	}
	return chargeUsage(ctx, tx, familyID, 0, bytes, limits, quotaExempt)
}

func semanticRejectionCode(err error) string {
	switch {
	case errors.Is(err, protocol.ErrInvalidTimestamp):
		return "invalid_timestamp"
	case errors.Is(err, protocol.ErrInvalidDecimal):
		return "invalid_decimal"
	case errors.Is(err, protocol.ErrInvalidText):
		return "invalid_text"
	case errors.Is(err, protocol.ErrInvalidCollection):
		return "invalid_collection"
	case errors.Is(err, protocol.ErrUnsupportedResource):
		return "unsupported_resource"
	default:
		return "invalid_mutation"
	}
}
