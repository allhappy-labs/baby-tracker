package care

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"babytracker/server/internal/protocol"
)

// Care kind codes (care_entities.kind, sync_page_bindings.resume_kind) and
// timer kind codes (care_entities.timer_kind), fixed by migration 000030
// section 1. The care kinds follow the legacy snapshot order, so ORDER BY
// kind, id pages a snapshot in the order the staged snapshot used.
const (
	KindBaby          int16 = 1
	KindFeed          int16 = 2
	KindSleep         int16 = 3
	KindDiaper        int16 = 4
	KindTimer         int16 = 5
	KindSupplement    int16 = 6
	KindGrowthProfile int16 = 7
	KindMeasurement   int16 = 8

	TimerKindFeed  int16 = 1
	TimerKindSleep int16 = 2
)

// Feed kind codes (care_entities.feed_kind), fixed by migration 000032
// section 1: a feed's payload kind, on which Live Activity's latest-milk and
// latest-breast reads filter.
const (
	FeedKindBreast int16 = 1
	FeedKindBottle int16 = 2
	FeedKindSolids int16 = 3
)

var (
	kindNames      = [...]string{KindBaby: "baby", KindFeed: "feed", KindSleep: "sleep", KindDiaper: "diaper", KindTimer: "timer", KindSupplement: "supplement", KindGrowthProfile: "growth_profile", KindMeasurement: "measurement"}
	timerKindNames = [...]string{TimerKindFeed: "feed", TimerKindSleep: "sleep"}
	feedKindNames  = [...]string{FeedKindBreast: "breast", FeedKindBottle: "bottle", FeedKindSolids: "solids"}
)

// KindCode is the schema code of a wire entity kind.
func KindCode(kind string) (int16, bool) { return codeOf(kindNames[:], kind) }

// KindName is the wire entity kind of a schema code.
func KindName(code int16) (string, bool) { return nameOf(kindNames[:], code) }

// TimerKindCode is the schema code of a wire timer kind.
func TimerKindCode(kind string) (int16, bool) { return codeOf(timerKindNames[:], kind) }

// TimerKindName is the wire timer kind of a schema code.
func TimerKindName(code int16) (string, bool) { return nameOf(timerKindNames[:], code) }

// FeedKindCode is the schema code of a wire feed kind.
func FeedKindCode(kind string) (int16, bool) { return codeOf(feedKindNames[:], kind) }

// FeedKindName is the wire feed kind of a schema code.
func FeedKindName(code int16) (string, bool) { return nameOf(feedKindNames[:], code) }

func codeOf(names []string, name string) (int16, bool) {
	for code := 1; code < len(names); code++ {
		if name == names[code] {
			return int16(code), true
		}
	}
	return 0, false
}

func nameOf(names []string, code int16) (string, bool) {
	if code < 1 || int(code) >= len(names) {
		return "", false
	}
	return names[code], true
}

const (
	// RowOverheadBytes is charged for every row, live or tombstone.
	RowOverheadBytes = 256
	// LiveReserveBytes is charged on top while the row is live.
	LiveReserveBytes = 4096
	// ReceiptOverheadBytes is charged for every care_receipts row on top of
	// its outcome (ReceiptChargedBytes): the per-row overhead an entity row
	// carries (RowOverheadBytes), as the legacy ReceiptStorageBytes charged
	// a receipt.
	ReceiptOverheadBytes = RowOverheadBytes
	// MaxWireBytes is the care_entities.wire bound (octet_length <= 1 MiB).
	MaxWireBytes = 1 << 20
)

// ErrWireTooLarge refuses an entity whose wire exceeds MaxWireBytes before
// it reaches the database; it is an invalid mutation.
var ErrWireTooLarge = fmt.Errorf("%w: canonical entity exceeds %d bytes", protocol.ErrInvalidMutation, MaxWireBytes)

// Change is the change envelope a row records: the family sequence of its
// latest change, who made it, and the operation. CompletedRecordID is the
// completion record of a timer stop (a timer tombstone only).
//
// AcceptedAt is optional and only checked: the row's accepted_at (the served
// change's accepted_at) and a tombstone's tombstoned_at always come from the
// entity meta, as the legacy journal served them (records/journal.go at 342973dc). A
// non-zero AcceptedAt must equal Meta.AcceptedAt once normalized to whole
// microseconds (protocol.NormalizeTimestamp, half-even), or Encode refuses
// the change; a raw clock reading therefore never puts a column 1 µs off
// the wire.
type Change struct {
	Seq                                 uint64
	ActorAccountID, DeviceID, Operation string
	AcceptedAt                          time.Time
	CompletedRecordID                   *string
}

// Row is one care_entities row. Rows are values: a tombstone shares its
// previous row's pointer and slice fields (BabyID, StartedAt, NameHash, …),
// so never mutate a row through them; assign new values instead.
type Row struct {
	FamilyID, ID, Kind string
	BabyID             *string
	EntityVersion      uint64
	ChangedSeq         uint64

	CreatorAccountID, ActorAccountID, DeviceID, Operation string
	AcceptedAt                                            time.Time
	TombstonedAt                                          *time.Time

	// CreatedDeviceID and CreatedSeq are the entity's origin: the device
	// and family sequence of its first version, set by Encode and inserted
	// once (the upsert never updates them). Rows written before migration 32
	// have none: "" and 0, stored as NULL.
	CreatedDeviceID string
	CreatedSeq      uint64

	StartedAt, EndedAt                      *time.Time
	SessionID, TimerKind, CompletedRecordID *string
	// FeedKind is a feed's payload kind (breast, bottle or solids); nil for
	// every other kind, and for a feed tombstone written before migration 32.
	FeedKind  *string
	SortOrder *int32
	Label     *string
	NameHash  []byte

	Derived Derived
	Wire    []byte
	// WireBytes is len(Wire), or octet_length(wire) when Wire is not loaded.
	WireBytes int
}

// Encode turns a canonical entity and the change that produced it into its
// row: the wire is json.Marshal(entity), and the typed and derived columns
// come from the payload. A tombstone keeps the typed columns of previous (the
// entity's current row, required) with no derived values. AcceptedAt and
// TombstonedAt are the entity meta's accepted_at and tombstoned_at. The
// origin is c's device and sequence for a new entity (previous nil) and
// previous's otherwise, a tombstone's included.
func Encode(entity protocol.CanonicalEntity, previous *Row, c Change) (Row, error) {
	if _, ok := KindCode(entity.EntityKind); !ok {
		return Row{}, fmt.Errorf("%w: care kind %q", protocol.ErrUnsupportedResource, entity.EntityKind)
	}
	wire, err := json.Marshal(entity)
	if err != nil {
		return Row{}, err
	}
	if len(wire) > MaxWireBytes {
		return Row{}, fmt.Errorf("%w: %s %s is %d bytes", ErrWireTooLarge, entity.EntityKind, entity.Meta.ID, len(wire))
	}
	accepted, err := metaTime(entity.Meta.AcceptedAt)
	if err != nil {
		return Row{}, fmt.Errorf("care: %s accepted_at: %w", entity.Meta.ID, err)
	}
	if !c.AcceptedAt.IsZero() {
		normalized, err := protocol.NormalizeTimestamp(c.AcceptedAt.UTC().Format(time.RFC3339Nano))
		if err != nil || !accepted.Equal(mustParse(normalized)) {
			return Row{}, fmt.Errorf("care: change accepted at %s, entity %s meta accepted at %s", c.AcceptedAt.UTC().Format(time.RFC3339Nano),
				entity.Meta.ID, entity.Meta.AcceptedAt)
		}
	}
	r := Row{FamilyID: entity.Meta.FamilyID, ID: entity.Meta.ID, Kind: entity.EntityKind, EntityVersion: entity.Meta.EntityVersion,
		ChangedSeq: c.Seq, CreatorAccountID: entity.Meta.CreatorAccountID, ActorAccountID: c.ActorAccountID, DeviceID: c.DeviceID,
		Operation: c.Operation, AcceptedAt: accepted, CompletedRecordID: c.CompletedRecordID, Wire: wire, WireBytes: len(wire),
		CreatedDeviceID: c.DeviceID, CreatedSeq: c.Seq}
	if previous != nil {
		r.CreatedDeviceID, r.CreatedSeq = previous.CreatedDeviceID, previous.CreatedSeq
	}
	if entity.Meta.TombstonedAt != nil {
		if previous == nil {
			return Row{}, fmt.Errorf("care: tombstone of %s without its row", r.ID)
		}
		if c.CompletedRecordID != nil && r.Kind != "timer" {
			return Row{}, fmt.Errorf("care: completion record on a %s tombstone", r.Kind)
		}
		r.BabyID, r.StartedAt, r.EndedAt, r.SessionID, r.TimerKind = previous.BabyID, previous.StartedAt, previous.EndedAt, previous.SessionID, previous.TimerKind
		r.FeedKind = previous.FeedKind
		r.SortOrder, r.Label, r.NameHash = previous.SortOrder, previous.Label, previous.NameHash
		tombstoned, err := metaTime(*entity.Meta.TombstonedAt)
		if err != nil {
			return Row{}, fmt.Errorf("care: %s tombstoned_at: %w", r.ID, err)
		}
		r.TombstonedAt = &tombstoned
		return r, nil
	}
	if c.CompletedRecordID != nil {
		return Row{}, fmt.Errorf("care: completion record on live %s %s", r.Kind, r.ID)
	}
	if err := typedColumns(&r, entity.Payload); err != nil {
		return Row{}, fmt.Errorf("care: %s %s: %w", r.Kind, r.ID, err)
	}
	return r, nil
}

// metaTime is a meta timestamp as the instant its normalized (whole
// microsecond, UTC) form names, so the column equals the wire exactly.
func metaTime(value string) (time.Time, error) {
	normalized, err := protocol.NormalizeTimestamp(value)
	if err != nil {
		return time.Time{}, err
	}
	return mustParse(normalized), nil
}

// mustParse parses a protocol.NormalizeTimestamp result, which is always a
// valid RFC 3339 timestamp.
func mustParse(normalized string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, normalized)
	if err != nil {
		panic(err)
	}
	return parsed
}

var errPayloadKind = errors.New("payload does not match the entity kind")

// typedColumns fills a live row's typed and derived columns from its payload.
func typedColumns(r *Row, payload protocol.Payload) error {
	var err error
	timestamp := func(value string) *time.Time {
		parsed, parseErr := time.Parse(time.RFC3339Nano, value)
		if parseErr != nil && err == nil {
			err = fmt.Errorf("%w: %q", protocol.ErrInvalidTimestamp, value)
		}
		return &parsed
	}
	optionalTimestamp := func(value *string) *time.Time {
		if value == nil {
			return nil
		}
		return timestamp(*value)
	}
	baby := func(id string) *string { return &id }
	kind := ""
	switch value := payload.(type) {
	case protocol.BabyPayload:
		kind = "baby"
		if value.SortOrder > math.MaxInt32 {
			return fmt.Errorf("%w: sort order %d", protocol.ErrInvalidMutation, value.SortOrder)
		}
		hash, hashErr := BabyNameHash(value.Name, value.BirthDate)
		if hashErr != nil {
			return hashErr
		}
		sortOrder := int32(value.SortOrder)
		r.SortOrder, r.NameHash = &sortOrder, hash[:]
	case protocol.FeedPayload:
		kind = "feed"
		r.BabyID, r.StartedAt, r.EndedAt, r.SessionID = baby(value.BabyID), timestamp(value.StartedAt), optionalTimestamp(value.EndedAt), value.FeedingSessionID
		if _, ok := FeedKindCode(value.Kind); !ok {
			return fmt.Errorf("%w: feed kind %q", protocol.ErrInvalidMutation, value.Kind)
		}
		feedKind := value.Kind
		r.FeedKind = &feedKind
		r.Derived = DeriveFeed(value.Components)
	case protocol.SleepPayload:
		kind = "sleep"
		r.BabyID, r.StartedAt, r.EndedAt, r.SessionID = baby(value.BabyID), timestamp(value.StartedAt), optionalTimestamp(value.EndedAt), value.SleepSessionID
	case protocol.DiaperPayload:
		kind = "diaper"
		r.BabyID, r.StartedAt, r.EndedAt = baby(value.BabyID), timestamp(value.StartedAt), optionalTimestamp(value.EndedAt)
	case protocol.TimerStartPayload:
		kind = "timer"
		timerKind := value.TimerKind
		r.BabyID, r.StartedAt, r.SessionID, r.TimerKind = baby(value.BabyID), timestamp(value.StartedAt), value.SessionID, &timerKind
		r.Derived.ShortestPartSeconds = shortestPart(value.CompletedComponents)
	case protocol.SupplementPayload:
		kind = "supplement"
		label := value.Name
		r.BabyID, r.StartedAt, r.Label = baby(value.BabyID), timestamp(value.StartedAt), &label
	case protocol.GrowthProfilePayload:
		kind = "growth_profile"
		r.BabyID = baby(value.BabyID)
	case protocol.MeasurementPayload:
		kind = "measurement"
		r.BabyID, r.StartedAt = baby(value.BabyID), timestamp(value.MeasuredAt)
	case nil:
		return fmt.Errorf("%w: live entity without a payload", protocol.ErrInvalidMutation)
	default:
		return fmt.Errorf("%w: payload %T", protocol.ErrInvalidMutation, payload)
	}
	if kind != r.Kind {
		return fmt.Errorf("%w: %w (%s payload)", protocol.ErrInvalidMutation, errPayloadKind, kind)
	}
	return err
}

// Entity decodes the stored wire.
func (r Row) Entity() (protocol.CanonicalEntity, error) {
	var entity protocol.CanonicalEntity
	if len(r.Wire) == 0 {
		return entity, fmt.Errorf("care: row %s has no wire loaded", r.ID)
	}
	err := json.Unmarshal(r.Wire, &entity)
	return entity, err
}

// Live reports whether the row is not a tombstone.
func (r Row) Live() bool { return r.TombstonedAt == nil }

// ReceiptChargedBytes is a care_receipts row's storage charge: its outcome
// as written (the JSON encoding of the outcome, the bytes the receipt insert
// stores) and ReceiptOverheadBytes. It is charged to
// families.care_charged_bytes when the receipt is written. RELEASE:
// maintenance.ReceiptSweep deletes a receipt 30 days after its acceptance
// and, in the transaction that deletes it, subtracts this same charge
// (StoredReceiptCharge, recomputed from the stored row) from its family's
// care_charged_bytes; caretest.AssertCounters proves the counters exact
// after every batch.
func ReceiptChargedBytes(outcomeJSON []byte) int64 {
	return int64(len(outcomeJSON)) + ReceiptOverheadBytes
}

// StoredReceiptCharge is the charge a stored receipt's outcome was written
// with: the stored outcome (care_receipts.outcome, whose jsonb text orders
// and spaces keys its own way) decoded and encoded as the write path encoded
// it (plan.go addReceipt), plus the overhead. Decoding and encoding again
// reproduces the written bytes: the outcome is a fixed struct whose
// canonical entity payloads are themselves encoded by the same rules.
func StoredReceiptCharge(outcomeJSON []byte) (int64, error) {
	var outcome protocol.Outcome
	if err := json.Unmarshal(outcomeJSON, &outcome); err != nil {
		return 0, fmt.Errorf("care: stored receipt outcome: %w", err)
	}
	raw, err := json.Marshal(outcome)
	if err != nil {
		return 0, fmt.Errorf("care: stored receipt outcome: %w", err)
	}
	return ReceiptChargedBytes(raw), nil
}

// ChargedBytes is the row's storage charge: its wire and the row overhead,
// plus the live reserve while it is live.
func (r Row) ChargedBytes() int64 {
	charged := int64(r.WireBytes) + RowOverheadBytes
	if r.Live() {
		charged += LiveReserveBytes
	}
	return charged
}

// CountsAsRecord reports whether the row counts toward the family's record
// quota: live, and neither a baby nor a timer.
func (r Row) CountsAsRecord() bool {
	return r.Live() && r.Kind != "baby" && r.Kind != "timer"
}
