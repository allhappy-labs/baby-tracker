package protocol

import (
	"bytes"
	"encoding/json"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Mutation struct {
	MutationID            string  `json:"mutation_id"`
	EntityID              string  `json:"entity_id"`
	EntityKind            string  `json:"entity_kind"`
	Operation             string  `json:"operation"`
	PayloadVersion        uint32  `json:"payload_version"`
	ObservedEntityVersion uint64  `json:"observed_entity_version"`
	ClientMutationTime    string  `json:"client_mutation_time"`
	ImportID              *string `json:"import_id"`
	Payload               Payload `json:"payload"`
}

func (mutation *Mutation) UnmarshalJSON(raw []byte) error {
	decoded, err := DecodeMutation(raw)
	if err != nil {
		return err
	}
	*mutation = decoded
	return nil
}

var canonicalIntegerPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]*)$`)

func DecodeMutation(raw []byte) (Mutation, error) {
	if len(raw) == 0 || !utf8.Valid(raw) || !json.Valid(raw) || !validJSONStringEscapes(raw) {
		return Mutation{}, errInvalidRequest
	}
	if err := rejectDuplicateRequestKeys(raw); err != nil {
		return Mutation{}, errInvalidRequest
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return Mutation{}, errInvalidRequest
	}
	allowed := map[string]bool{
		"mutation_id": true, "entity_id": true, "entity_kind": true, "operation": true,
		"payload_version": true, "observed_entity_version": true, "client_mutation_time": true,
		"import_id": true, "payload": true,
	}
	for key := range object {
		if !allowed[key] {
			return Mutation{}, errInvalidRequest
		}
	}
	for _, key := range []string{"mutation_id", "entity_id", "entity_kind", "operation", "payload_version", "observed_entity_version", "client_mutation_time", "payload"} {
		if value, ok := object[key]; !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return Mutation{}, errInvalidRequest
		}
	}
	mutation := Mutation{}
	if decodeJSONString(object["mutation_id"], &mutation.MutationID) != nil || validateUUID("mutation_id", mutation.MutationID) != nil ||
		decodeJSONString(object["entity_id"], &mutation.EntityID) != nil || validateUUID("entity_id", mutation.EntityID) != nil ||
		decodeJSONString(object["entity_kind"], &mutation.EntityKind) != nil ||
		decodeJSONString(object["operation"], &mutation.Operation) != nil ||
		decodeJSONString(object["client_mutation_time"], &mutation.ClientMutationTime) != nil {
		return Mutation{}, errInvalidRequest
	}
	payloadVersion, err := parseExactUint(object["payload_version"], 32)
	if err != nil {
		return Mutation{}, errInvalidRequest
	}
	observed, err := parseExactUint(object["observed_entity_version"], 64)
	if err != nil || observed > MaxSafeInteger {
		return Mutation{}, errInvalidRequest
	}
	mutation.PayloadVersion = uint32(payloadVersion)
	mutation.ObservedEntityVersion = observed
	mutation.ClientMutationTime = normalizeTimestampForHash(mutation.ClientMutationTime)
	if importRaw, ok := object["import_id"]; ok && !bytes.Equal(bytes.TrimSpace(importRaw), []byte("null")) {
		var importID string
		if json.Unmarshal(importRaw, &importID) != nil || validateUUID("import_id", importID) != nil {
			return Mutation{}, errInvalidRequest
		}
		mutation.ImportID = &importID
	}
	if !AcceptedPayloadVersion(mutation.EntityKind, mutation.Operation, mutation.PayloadVersion) {
		return Mutation{}, errInvalidRequest
	}
	payload, err := decodeMutationPayload(mutation.EntityKind, mutation.Operation, object["payload"])
	if err != nil {
		return Mutation{}, errInvalidRequest
	}
	mutation.Payload = payload
	return mutation, nil
}

// CurrentPayloadVersion is the payload version of an entity kind's current
// wire shape: feed 4, timer 5, sleep 2, every other kind 1.
func CurrentPayloadVersion(kind string) uint32 {
	switch kind {
	case "feed":
		return 4
	case "timer":
		return 5
	case "sleep":
		return 2
	}
	return 1
}

// AcceptedPayloadVersion is the inbound version rule. A create, an update and
// every timer command must declare the kind's current version. A delete names
// only its baby (or nothing, for a baby), so it has no versioned shape and is
// accepted at any version from 1 to the current one, permanently: build
// 2961ca1 declares feed deletes at 2 and sleep deletes at 1.
func AcceptedPayloadVersion(kind, operation string, version uint32) bool {
	current := CurrentPayloadVersion(kind)
	if operation == "delete" {
		return version >= 1 && version <= current
	}
	return version == current
}

// decodeMutationPayload reads a payload in the one shape its kind and
// operation have; DecodeMutation has already held the declared version to the
// inbound rule.
func decodeMutationPayload(kind, operation string, raw []byte) (Payload, error) {
	var target Payload
	switch {
	case operation == "delete" && (kind == "baby" || kind == "feed" || kind == "sleep" || kind == "diaper" || kind == "supplement" || kind == "growth_profile" || kind == "measurement"):
		target = DeletePayload{}
	case (operation == "create" || operation == "update") && kind == "baby":
		target = BabyPayload{}
	case (operation == "create" || operation == "update") && kind == "feed":
		target = FeedPayload{}
	case (operation == "create" || operation == "update") && kind == "sleep":
		target = SleepPayload{}
	case (operation == "create" || operation == "update") && kind == "growth_profile":
		target = GrowthProfilePayload{}
	case (operation == "create" || operation == "update") && kind == "measurement":
		target = MeasurementPayload{}
	case (operation == "create" || operation == "update") && kind == "supplement":
		target = SupplementPayload{}
	case (operation == "create" || operation == "update") && kind == "diaper":
		target = DiaperPayload{}
	case operation == "timer_start" && kind == "timer":
		target = TimerStartPayload{}
	case operation == "timer_finish_bottle" && kind == "timer":
		target = TimerBottleFinishPayload{}
	case operation == "timer_stop" && kind == "timer":
		target = TimerStopPayload{}
	case operation == "update" && kind == "timer":
		target = TimerEditPayload{}
	default:
		return nil, ErrInvalidMutation
	}
	if operation == "delete" {
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil || object == nil {
			return nil, ErrInvalidMutation
		}
		if kind == "baby" {
			if len(object) != 0 {
				return nil, ErrInvalidMutation
			}
		} else {
			babyID, exists := object["baby_id"]
			if !exists || bytes.Equal(bytes.TrimSpace(babyID), []byte("null")) {
				return nil, ErrInvalidMutation
			}
		}
	}
	return decodePayloadIntoValue(raw, target)
}

// decodePayloadIntoValue decodes an exact JSON object into the target's
// current shape.
func decodePayloadIntoValue(raw []byte, target Payload) (Payload, error) {
	valueType := reflect.TypeOf(target)
	if err := validateExactJSONObject(raw, valueType); err != nil {
		return nil, err
	}
	pointer := reflect.New(valueType)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(pointer.Interface()); err != nil {
		return nil, err
	}
	value := pointer.Elem().Interface().(Payload)
	if err := validateRequiredPayloadShape(value, raw); err != nil {
		return nil, err
	}
	return normalizeDecodedPayload(value), nil
}

func validateRequiredPayloadShape(payload Payload, raw []byte) error {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return errInvalidRequest
	}
	var required []string
	switch payload.(type) {
	case BabyPayload:
		required = []string{"name", "birth_date"}
	case FeedPayload:
		required = []string{"baby_id", "kind", "started_at", "time_zone_id", "components"}
		if err := requireSessionKey(object, "feeding_session_id"); err != nil {
			return err
		}
	case SleepPayload:
		required = []string{"baby_id", "started_at", "time_zone_id"}
		if err := requireSessionKey(object, "sleep_session_id"); err != nil {
			return err
		}
	case GrowthProfilePayload:
		required = []string{"baby_id", "reference_sex"}
		if _, present := object["expected_due_date"]; !present {
			return errInvalidRequest
		}
	case MeasurementPayload:
		required = []string{"baby_id", "measured_at", "logged_at", "time_zone_id"}
		for _, key := range []string{"weight_kg", "length_cm", "head_circumference_cm", "length_position"} {
			if _, present := object[key]; !present {
				return errInvalidRequest
			}
		}
	case SupplementPayload:
		required = []string{"baby_id", "started_at", "time_zone_id", "name", "logged_at"}
	case DiaperPayload:
		required = []string{"baby_id", "started_at", "time_zone_id", "contents"}
	case TimerStartPayload:
		required = []string{"baby_id", "timer_kind", "started_at", "time_zone_id", "completed_components"}
		if err := requireSessionKey(object, "session_id"); err != nil {
			return err
		}
	case TimerBottleFinishPayload:
		required = []string{"baby_id", "finished_at", "component_id", "volume_ml", "contents"}
	case TimerStopPayload:
		required = []string{"baby_id", "completed_record_id", "stopped_at"}
	case TimerEditPayload:
		required = []string{"baby_id", "components"}
		if err := requireSessionKey(object, "session_id"); err != nil {
			return err
		}
	case DeletePayload:
		return nil
	}
	for _, key := range required {
		value, exists := object[key]
		if !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errInvalidRequest
		}
	}
	if _, ok := payload.(BabyPayload); ok {
		if value, exists := object["sort_order"]; exists && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errInvalidRequest
		}
	}
	for _, key := range []string{"components", "completed_components"} {
		value, exists := object[key]
		if !exists {
			continue
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || validateComponents(value) != nil {
			return errInvalidRequest
		}
	}
	return nil
}

// requireSessionKey enforces a session-link key: always present, null or a
// valid UUID.
func requireSessionKey(object map[string]json.RawMessage, key string) error {
	value, present := object[key]
	if !present {
		return errInvalidRequest
	}
	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return nil
	}
	var sessionID string
	if json.Unmarshal(value, &sessionID) != nil || !ValidUUID(sessionID) {
		return errInvalidRequest
	}
	return nil
}

func validateComponents(raw []byte) error {
	var values []map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil || values == nil {
		return errInvalidRequest
	}
	for _, value := range values {
		var kind string
		if decodeJSONString(value["kind"], &kind) != nil {
			return errInvalidRequest
		}
		allowed := map[string]bool{"id": true, "kind": true}
		required := []string{"id", "kind"}
		switch kind {
		case "breast":
			allowed["side"], allowed["seconds"], allowed["started_at"], allowed["ended_at"] = true, true, true, true
			required = append(required, "side", "seconds")
		case "bottle":
			allowed["volume_ml"], allowed["contents"], allowed["seconds"] = true, true, true
			allowed["started_at"], allowed["ended_at"] = true, true
			required = append(required, "volume_ml", "contents")
		case "solids":
			allowed["foods"], allowed["amount"] = true, true
			required = append(required, "foods", "amount")
		default:
			return errInvalidRequest
		}
		for key := range value {
			if !allowed[key] {
				return errInvalidRequest
			}
		}
		for _, key := range required {
			field, ok := value[key]
			if !ok || bytes.Equal(bytes.TrimSpace(field), []byte("null")) {
				return errInvalidRequest
			}
		}
	}
	return nil
}

func normalizeDecodedPayload(payload Payload) Payload {
	normalizeString := func(value string) string {
		normalized, err := NormalizeText(value)
		if err != nil {
			return value
		}
		return normalized
	}
	normalizeOptionalString := func(value *string) *string {
		if value == nil {
			return nil
		}
		normalized := normalizeString(*value)
		return &normalized
	}
	normalizeOptionalTimestamp := func(value *string) *string {
		if value == nil {
			return nil
		}
		normalized := normalizeTimestampForHash(*value)
		return &normalized
	}
	switch value := payload.(type) {
	case BabyPayload:
		if normalized, err := NormalizeDisplayName(value.Name); err == nil {
			value.Name = normalized
		} else {
			value.Name = normalizeString(value.Name)
		}
		value.BirthDate = normalizeString(value.BirthDate)
		return value
	case FeedPayload:
		value.StartedAt = normalizeTimestampForHash(value.StartedAt)
		value.EndedAt = normalizeOptionalTimestamp(value.EndedAt)
		value.TimeZoneID = normalizeString(value.TimeZoneID)
		value.Note = normalizeOptionalString(value.Note)
		for index := range value.Components {
			value.Components[index].StartedAt = normalizeOptionalTimestamp(value.Components[index].StartedAt)
			value.Components[index].EndedAt = normalizeOptionalTimestamp(value.Components[index].EndedAt)
			value.Components[index].Contents = normalizeOptionalString(value.Components[index].Contents)
			value.Components[index].Amount = normalizeOptionalString(value.Components[index].Amount)
			for foodIndex := range value.Components[index].Foods {
				value.Components[index].Foods[foodIndex] = normalizeString(value.Components[index].Foods[foodIndex])
			}
		}
		return value
	case SleepPayload:
		value.StartedAt = normalizeTimestampForHash(value.StartedAt)
		value.EndedAt = normalizeOptionalTimestamp(value.EndedAt)
		value.TimeZoneID = normalizeString(value.TimeZoneID)
		value.Note = normalizeOptionalString(value.Note)
		value.KindOverride = normalizeOptionalString(value.KindOverride)
		return value
	case MeasurementPayload:
		value.MeasuredAt = normalizeTimestampForHash(value.MeasuredAt)
		value.LoggedAt = normalizeTimestampForHash(value.LoggedAt)
		value.TimeZoneID = normalizeString(value.TimeZoneID)
		return value
	case SupplementPayload:
		value.LoggedAt = normalizeTimestampForHash(value.LoggedAt)
		value.StartedAt = normalizeTimestampForHash(value.StartedAt)
		value.TimeZoneID = normalizeString(value.TimeZoneID)
		value.Name = strings.TrimSpace(normalizeString(value.Name))
		value.Note = normalizeOptionalString(value.Note)
		return value
	case DiaperPayload:
		value.StartedAt = normalizeTimestampForHash(value.StartedAt)
		value.EndedAt = normalizeOptionalTimestamp(value.EndedAt)
		value.TimeZoneID = normalizeString(value.TimeZoneID)
		value.Note = normalizeOptionalString(value.Note)
		value.Consistency = normalizeOptionalString(value.Consistency)
		value.Color = normalizeOptionalString(value.Color)
		return value
	case TimerStartPayload:
		value.StartedAt = normalizeTimestampForHash(value.StartedAt)
		value.TimeZoneID = normalizeString(value.TimeZoneID)
		value.CurrentSide = normalizeOptionalString(value.CurrentSide)
		value.CurrentSideStartedAt = normalizeOptionalTimestamp(value.CurrentSideStartedAt)
		for index := range value.CompletedComponents {
			value.CompletedComponents[index].Contents = normalizeOptionalString(value.CompletedComponents[index].Contents)
			value.CompletedComponents[index].Amount = normalizeOptionalString(value.CompletedComponents[index].Amount)
			for foodIndex := range value.CompletedComponents[index].Foods {
				value.CompletedComponents[index].Foods[foodIndex] = normalizeString(value.CompletedComponents[index].Foods[foodIndex])
			}
		}
		return value
	case TimerStopPayload:
		value.StoppedAt = normalizeTimestampForHash(value.StoppedAt)
		value.Note = normalizeOptionalString(value.Note)
		value.SleepKindOverride = normalizeOptionalString(value.SleepKindOverride)
		return value
	case TimerEditPayload:
		value.StartedAt = normalizeOptionalTimestamp(value.StartedAt)
		for index := range value.Components {
			value.Components[index].Contents = normalizeOptionalString(value.Components[index].Contents)
			value.Components[index].Amount = normalizeOptionalString(value.Components[index].Amount)
			for foodIndex := range value.Components[index].Foods {
				value.Components[index].Foods[foodIndex] = normalizeString(value.Components[index].Foods[foodIndex])
			}
		}
		return value
	default:
		return payload
	}
}

func decodeJSONString(raw []byte, target *string) error {
	if len(raw) == 0 || target == nil {
		return errInvalidRequest
	}
	return json.Unmarshal(raw, target)
}

func parseExactUint(raw []byte, bits int) (uint64, error) {
	text := string(bytes.TrimSpace(raw))
	if !canonicalIntegerPattern.MatchString(text) {
		return 0, errInvalidRequest
	}
	value, err := strconv.ParseUint(text, 10, bits)
	if err != nil {
		return 0, errInvalidRequest
	}
	return value, nil
}
