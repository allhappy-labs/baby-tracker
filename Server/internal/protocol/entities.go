package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
)

type Payload interface{ protocolPayload() }

type BabyPayload struct {
	Name      string  `json:"name"`
	BirthDate string  `json:"birth_date"`
	SortOrder uint32  `json:"sort_order"`
	Icon      *string `json:"icon,omitempty"`
}

func (BabyPayload) protocolPayload() {}

type FeedComponent struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Side      *string  `json:"side,omitempty"`
	Seconds   *uint32  `json:"seconds,omitempty"`
	VolumeML  *Decimal `json:"volume_ml,omitempty"`
	Contents  *string  `json:"contents,omitempty"`
	Foods     []string `json:"foods,omitempty"`
	Amount    *string  `json:"amount,omitempty"`
	StartedAt *string  `json:"started_at,omitempty"`
	EndedAt   *string  `json:"ended_at,omitempty"`
}

// MarshalJSON writes the fields a part's kind has. A solids part always
// carries its foods list, `[]` when empty: the stored decoder and the clients
// require the key, and `omitempty` would drop an empty list. A breast or
// bottle part never carries one.
func (component FeedComponent) MarshalJSON() ([]byte, error) {
	type wireComponent struct {
		ID        string    `json:"id"`
		Kind      string    `json:"kind"`
		Side      *string   `json:"side,omitempty"`
		Seconds   *uint32   `json:"seconds,omitempty"`
		VolumeML  *Decimal  `json:"volume_ml,omitempty"`
		Contents  *string   `json:"contents,omitempty"`
		Foods     *[]string `json:"foods,omitempty"`
		Amount    *string   `json:"amount,omitempty"`
		StartedAt *string   `json:"started_at,omitempty"`
		EndedAt   *string   `json:"ended_at,omitempty"`
	}
	wire := wireComponent{ID: component.ID, Kind: component.Kind, Side: component.Side, Seconds: component.Seconds,
		VolumeML: component.VolumeML, Contents: component.Contents, Amount: component.Amount,
		StartedAt: component.StartedAt, EndedAt: component.EndedAt}
	if component.Kind == "solids" {
		foods := component.Foods
		if foods == nil {
			foods = []string{}
		}
		wire.Foods = &foods
	}
	return json.Marshal(wire)
}

// FeedPayload is a feed in its one wire shape (payload version 4): parts as
// `components` and the session link as `feeding_session_id`, written even
// when null.
type FeedPayload struct {
	BabyID           string          `json:"baby_id"`
	Kind             string          `json:"kind"`
	StartedAt        string          `json:"started_at"`
	EndedAt          *string         `json:"ended_at"`
	TimeZoneID       string          `json:"time_zone_id"`
	Note             *string         `json:"note"`
	Components       []FeedComponent `json:"components"`
	FeedingSessionID *string         `json:"feeding_session_id"`
}

func (payload FeedPayload) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"baby_id": payload.BabyID, "kind": payload.Kind, "started_at": payload.StartedAt,
		"ended_at": payload.EndedAt, "time_zone_id": payload.TimeZoneID, "note": payload.Note,
		"components": payload.Components, "feeding_session_id": payload.FeedingSessionID,
	})
}

func (FeedPayload) protocolPayload() {}

// SleepPayload is a sleep in its one wire shape (payload version 2), with
// `sleep_session_id` written even when null.
type SleepPayload struct {
	BabyID         string  `json:"baby_id"`
	StartedAt      string  `json:"started_at"`
	EndedAt        *string `json:"ended_at"`
	TimeZoneID     string  `json:"time_zone_id"`
	Note           *string `json:"note"`
	KindOverride   *string `json:"kind_override"`
	SleepSessionID *string `json:"sleep_session_id"`
}

func (SleepPayload) protocolPayload() {}

type GrowthProfilePayload struct {
	BabyID          string  `json:"baby_id"`
	ReferenceSex    string  `json:"reference_sex"`
	ExpectedDueDate *string `json:"expected_due_date"`
}

func (GrowthProfilePayload) protocolPayload() {}

type MeasurementPayload struct {
	BabyID              string   `json:"baby_id"`
	MeasuredAt          string   `json:"measured_at"`
	LoggedAt            string   `json:"logged_at"`
	TimeZoneID          string   `json:"time_zone_id"`
	WeightKG            *Decimal `json:"weight_kg"`
	LengthCM            *Decimal `json:"length_cm"`
	HeadCircumferenceCM *Decimal `json:"head_circumference_cm"`
	LengthPosition      *string  `json:"length_position"`
}

func (MeasurementPayload) protocolPayload() {}

type SupplementPayload struct {
	BabyID     string  `json:"baby_id"`
	StartedAt  string  `json:"started_at"`
	TimeZoneID string  `json:"time_zone_id"`
	Name       string  `json:"name"`
	Note       *string `json:"note"`
	LoggedAt   string  `json:"logged_at"`
}

func (SupplementPayload) protocolPayload() {}

type DiaperPayload struct {
	BabyID      string  `json:"baby_id"`
	StartedAt   string  `json:"started_at"`
	EndedAt     *string `json:"ended_at"`
	TimeZoneID  string  `json:"time_zone_id"`
	Note        *string `json:"note"`
	Contents    string  `json:"contents"`
	Consistency *string `json:"consistency"`
	Color       *string `json:"color"`
}

func (DiaperPayload) protocolPayload() {}

// TimerStartPayload is a running timer in its one wire shape (payload
// version 5): finished parts as `completed_components`, with
// `current_bottle_started_at` and `session_id` written even when null.
type TimerStartPayload struct {
	BabyID                 string          `json:"baby_id"`
	TimerKind              string          `json:"timer_kind"`
	StartedAt              string          `json:"started_at"`
	TimeZoneID             string          `json:"time_zone_id"`
	CurrentSide            *string         `json:"current_side"`
	CurrentSideStartedAt   *string         `json:"current_side_started_at"`
	CurrentBottleStartedAt *string         `json:"current_bottle_started_at"`
	CompletedComponents    []FeedComponent `json:"completed_components"`
	SessionID              *string         `json:"session_id"`
}

func (payload TimerStartPayload) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"baby_id": payload.BabyID, "timer_kind": payload.TimerKind, "started_at": payload.StartedAt,
		"time_zone_id": payload.TimeZoneID, "current_side": payload.CurrentSide,
		"current_side_started_at": payload.CurrentSideStartedAt, "current_bottle_started_at": payload.CurrentBottleStartedAt,
		"completed_components": payload.CompletedComponents, "session_id": payload.SessionID,
	})
}

func (TimerStartPayload) protocolPayload() {}

type TimerBottleFinishPayload struct {
	BabyID      string   `json:"baby_id"`
	FinishedAt  string   `json:"finished_at"`
	ComponentID string   `json:"component_id"`
	VolumeML    *Decimal `json:"volume_ml"`
	Contents    *string  `json:"contents"`
}

func (TimerBottleFinishPayload) protocolPayload() {}

type TimerStopPayload struct {
	BabyID                     string  `json:"baby_id"`
	CompletedRecordID          string  `json:"completed_record_id"`
	StoppedAt                  string  `json:"stopped_at"`
	ComponentID                string  `json:"component_id,omitempty"`
	CleanupShortBreastSegments bool    `json:"cleanup_short_breast_segments,omitempty"`
	Note                       *string `json:"note"`
	SleepKindOverride          *string `json:"sleep_kind_override"`
}

func (TimerStopPayload) protocolPayload() {}

// TimerEditPayload is the patch an `update` on a running timer carries. It is
// a patch, not a replacement: a null `started_at` leaves the start alone, and
// `components` names only the finished parts that changed, matched by id. A
// full replacement would erase a side switch or a bottle the other phone
// committed a moment earlier.
type TimerEditPayload struct {
	BabyID     string          `json:"baby_id"`
	StartedAt  *string         `json:"started_at"`
	Components []FeedComponent `json:"components"`
	SessionID  *string         `json:"session_id"`
}

// MarshalJSON always writes `components` as an array: the wire requires it,
// and an edit built in Go with only a new start leaves the slice nil.
func (payload TimerEditPayload) MarshalJSON() ([]byte, error) {
	components := payload.Components
	if components == nil {
		components = []FeedComponent{}
	}
	return json.Marshal(map[string]any{"baby_id": payload.BabyID, "started_at": payload.StartedAt,
		"components": components, "session_id": payload.SessionID})
}

func (TimerEditPayload) protocolPayload() {}

type DeletePayload struct {
	BabyID *string `json:"baby_id,omitempty"`
}

func (DeletePayload) protocolPayload() {}

type EntityMeta struct {
	ID                  string  `json:"id"`
	FamilyID            string  `json:"family_id"`
	EntityVersion       uint64  `json:"entity_version"`
	CreatorAccountID    string  `json:"creator_account_id"`
	LastEditorAccountID string  `json:"last_editor_account_id"`
	ClientMutationTime  string  `json:"client_mutation_time"`
	AcceptedAt          string  `json:"accepted_at"`
	TombstonedAt        *string `json:"tombstoned_at"`
}

func (meta *EntityMeta) UnmarshalJSON(raw []byte) error {
	type wireEntityMeta EntityMeta
	var wire wireEntityMeta
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	if wire.EntityVersion < 1 || wire.EntityVersion > MaxSafeInteger {
		return errInvalidRequest
	}
	*meta = EntityMeta(wire)
	return nil
}

type CanonicalEntity struct {
	EntityKind string     `json:"entity_kind"`
	Meta       EntityMeta `json:"meta"`
	Payload    Payload    `json:"payload"`
}

func (entity *CanonicalEntity) UnmarshalJSON(raw []byte) error {
	var envelope struct {
		EntityKind string          `json:"entity_kind"`
		Meta       json.RawMessage `json:"meta"`
		Payload    json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	if !knownEntityKind(envelope.EntityKind) {
		return fmt.Errorf("%w: %s", ErrUnsupportedResource, envelope.EntityKind)
	}
	if len(envelope.Meta) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Meta), []byte("null")) {
		return errInvalidRequest
	}
	var meta EntityMeta
	if err := json.Unmarshal(envelope.Meta, &meta); err != nil {
		return err
	}
	entity.EntityKind, entity.Meta = envelope.EntityKind, meta
	if bytes.Equal(bytes.TrimSpace(envelope.Payload), []byte("null")) {
		if meta.TombstonedAt == nil {
			return errInvalidRequest
		}
		entity.Payload = nil
		return nil
	}
	payload, err := decodeEntityPayload(envelope.EntityKind, envelope.Payload)
	if err != nil {
		return err
	}
	entity.Payload = payload
	return nil
}

func knownEntityKind(kind string) bool {
	switch kind {
	case "baby", "feed", "sleep", "diaper", "supplement", "growth_profile", "measurement", "timer":
		return true
	default:
		return false
	}
}

// decodeEntityPayload reads an entity's payload (a journal row, a receipt, a
// snapshot row) by its kind alone. Every stored payload is in its kind's
// current shape — migration 000027 rewrote the older ones — so it is held to
// the current required keys and part rules. Unlike an inbound payload, an
// entity tolerates additive keys a newer server may write.
func decodeEntityPayload(kind string, raw []byte) (Payload, error) {
	var target Payload
	switch kind {
	case "baby":
		target = BabyPayload{}
	case "feed":
		target = FeedPayload{}
	case "sleep":
		target = SleepPayload{}
	case "growth_profile":
		target = GrowthProfilePayload{}
	case "measurement":
		target = MeasurementPayload{}
	case "supplement":
		target = SupplementPayload{}
	case "diaper":
		target = DiaperPayload{}
	case "timer":
		target = TimerStartPayload{}
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedResource, kind)
	}
	pointer := reflect.New(reflect.TypeOf(target))
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(pointer.Interface()); err != nil {
		return nil, err
	}
	value := pointer.Elem().Interface().(Payload)
	if err := validateRequiredPayloadShape(value, raw); err != nil {
		return nil, err
	}
	return normalizeDecodedPayload(value), nil
}
