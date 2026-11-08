package protocol

import (
	"crypto/sha256"
	"fmt"
)

var (
	mutationHashDomain = []byte("baby-tracker-mutation-v1\x00")
	contentHashDomain  = []byte("baby-tracker-content-v1\x00")
)

func CanonicalMutationHash(mutation Mutation) ([32]byte, error) {
	canonical, err := canonicalMutationBytes(mutation)
	if err != nil {
		return [32]byte{}, err
	}
	input := make([]byte, 0, len(mutationHashDomain)+len(canonical))
	input = append(input, mutationHashDomain...)
	input = append(input, canonical...)
	return sha256.Sum256(input), nil
}

// CanonicalMutationBytes returns the exact normalized bytes used by the v1
// receipt hash and by the per-item sync admission limit.
func CanonicalMutationBytes(mutation Mutation) ([]byte, error) {
	canonical, err := canonicalMutationBytes(mutation)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), canonical...), nil
}

func CanonicalContentHash(kind string, payload Payload) ([32]byte, error) {
	canonicalPayload, err := canonicalPayloadValue(payload, false)
	if err != nil {
		return [32]byte{}, err
	}
	canonical, err := marshalCanonicalJSON(map[string]any{
		"entity_kind": kind, "payload": canonicalPayload, "payload_version": uint32(1),
	})
	if err != nil {
		return [32]byte{}, err
	}
	input := make([]byte, 0, len(contentHashDomain)+len(canonical))
	input = append(input, contentHashDomain...)
	input = append(input, canonical...)
	return sha256.Sum256(input), nil
}

func canonicalMutationBytes(mutation Mutation) ([]byte, error) {
	payload, err := canonicalPayloadValue(mutation.Payload, true)
	if err != nil {
		return nil, err
	}
	return marshalCanonicalJSON(map[string]any{
		"client_mutation_time":    normalizeTimestampForHash(mutation.ClientMutationTime),
		"entity_id":               mutation.EntityID,
		"entity_kind":             mutation.EntityKind,
		"import_id":               nullableString(mutation.ImportID),
		"observed_entity_version": mutation.ObservedEntityVersion,
		"operation":               mutation.Operation,
		"payload":                 payload,
		"payload_version":         mutation.PayloadVersion,
	})
}

func canonicalPayloadValue(payload Payload, includeBabyID bool) (map[string]any, error) {
	normalizeString := func(value string) string {
		normalized, err := NormalizeText(value)
		if err != nil {
			return value
		}
		return normalized
	}
	// A timer edit's parts hash without bottle seconds or step bounds, as they
	// always have (canonical-vectors.json timer_edit_mutation); a feed's and a
	// running timer's parts hash with them.
	mapComponents := func(values []FeedComponent, timed bool) []any {
		result := make([]any, 0, len(values))
		for _, component := range values {
			item := map[string]any{"id": component.ID, "kind": component.Kind}
			switch component.Kind {
			case "breast":
				item["side"], item["seconds"] = nullableString(component.Side), component.Seconds
			case "bottle":
				item["volume_ml"], item["contents"] = nullableDecimal(component.VolumeML), nullableNormalizedString(component.Contents)
				if timed {
					item["seconds"] = component.Seconds
				}
			case "solids":
				foods := make([]any, 0, len(component.Foods))
				for _, food := range component.Foods {
					foods = append(foods, normalizeString(food))
				}
				item["foods"], item["amount"] = foods, nullableNormalizedString(component.Amount)
			}
			if timed && (component.Kind == "breast" || component.Kind == "bottle") {
				item["started_at"] = nullableNormalizedTimestamp(component.StartedAt)
				item["ended_at"] = nullableNormalizedTimestamp(component.EndedAt)
			}
			result = append(result, item)
		}
		return result
	}
	withBaby := func(result map[string]any, babyID string) map[string]any {
		if includeBabyID {
			result["baby_id"] = babyID
		}
		return result
	}
	switch value := payload.(type) {
	case BabyPayload:
		name, err := NormalizeDisplayName(value.Name)
		if err != nil {
			name = normalizeString(value.Name)
		}
		result := map[string]any{"birth_date": normalizeString(value.BirthDate), "name": name, "sort_order": value.SortOrder}
		// Only when set: a receipt stored before icons existed must hash
		// the same on retry.
		if value.Icon != nil {
			result["icon"] = *value.Icon
		}
		return result, nil
	case FeedPayload:
		return withBaby(map[string]any{
			"components": mapComponents(value.Components, true), "ended_at": nullableNormalizedTimestamp(value.EndedAt),
			"feeding_session_id": nullableString(value.FeedingSessionID), "kind": value.Kind,
			"note": nullableNormalizedString(value.Note), "started_at": normalizeTimestampForHash(value.StartedAt),
			"time_zone_id": normalizeString(value.TimeZoneID),
		}, value.BabyID), nil
	case SleepPayload:
		return withBaby(map[string]any{
			"ended_at": nullableNormalizedTimestamp(value.EndedAt), "kind_override": nullableNormalizedString(value.KindOverride),
			"note": nullableNormalizedString(value.Note), "sleep_session_id": nullableString(value.SleepSessionID),
			"started_at": normalizeTimestampForHash(value.StartedAt), "time_zone_id": normalizeString(value.TimeZoneID),
		}, value.BabyID), nil
	case GrowthProfilePayload:
		return withBaby(map[string]any{"reference_sex": value.ReferenceSex, "expected_due_date": nullableString(value.ExpectedDueDate)}, value.BabyID), nil
	case MeasurementPayload:
		return withBaby(map[string]any{"measured_at": normalizeTimestampForHash(value.MeasuredAt), "logged_at": normalizeTimestampForHash(value.LoggedAt), "time_zone_id": normalizeString(value.TimeZoneID), "weight_kg": nullableDecimal(value.WeightKG), "length_cm": nullableDecimal(value.LengthCM), "head_circumference_cm": nullableDecimal(value.HeadCircumferenceCM), "length_position": nullableString(value.LengthPosition)}, value.BabyID), nil
	case SupplementPayload:
		return withBaby(map[string]any{"logged_at": normalizeTimestampForHash(value.LoggedAt), "name": normalizeString(value.Name), "note": nullableNormalizedString(value.Note), "started_at": normalizeTimestampForHash(value.StartedAt), "time_zone_id": normalizeString(value.TimeZoneID)}, value.BabyID), nil
	case ImmunizationPayload:
		return withBaby(map[string]any{"dose": nullableInt(value.Dose), "given_at": normalizeTimestampForHash(value.GivenAt), "note": nullableNormalizedString(value.Note), "time_zone_id": normalizeString(value.TimeZoneID), "vaccine": normalizeString(value.Vaccine)}, value.BabyID), nil
	case PumpPayload:
		// Family-owned: no baby_id in the mutation hash or the content hash.
		return map[string]any{"ended_at": nullableNormalizedTimestamp(value.EndedAt), "note": nullableNormalizedString(value.Note),
			"side": normalizeString(value.Side), "started_at": normalizeTimestampForHash(value.StartedAt),
			"time_zone_id": normalizeString(value.TimeZoneID), "volume_ml": nullableDecimal(value.VolumeML)}, nil
	case DiaperPayload:
		return withBaby(map[string]any{
			"color": nullableNormalizedString(value.Color), "consistency": nullableNormalizedString(value.Consistency),
			"contents": value.Contents, "ended_at": nullableNormalizedTimestamp(value.EndedAt),
			"note": nullableNormalizedString(value.Note), "started_at": normalizeTimestampForHash(value.StartedAt),
			"time_zone_id": normalizeString(value.TimeZoneID),
		}, value.BabyID), nil
	case TimerStartPayload:
		return withBaby(map[string]any{
			"completed_components":      mapComponents(value.CompletedComponents, true),
			"current_bottle_started_at": nullableNormalizedTimestamp(value.CurrentBottleStartedAt),
			"current_side":              nullableNormalizedString(value.CurrentSide),
			"current_side_started_at":   nullableNormalizedTimestamp(value.CurrentSideStartedAt),
			"session_id":                nullableString(value.SessionID),
			"started_at":                normalizeTimestampForHash(value.StartedAt), "time_zone_id": normalizeString(value.TimeZoneID),
			"timer_kind": value.TimerKind,
		}, value.BabyID), nil
	case TimerBottleFinishPayload:
		return withBaby(map[string]any{"finished_at": normalizeTimestampForHash(value.FinishedAt), "component_id": value.ComponentID,
			"volume_ml": nullableDecimal(value.VolumeML), "contents": nullableNormalizedString(value.Contents)}, value.BabyID), nil
	case TimerStopPayload:
		result := map[string]any{
			"completed_record_id": value.CompletedRecordID, "note": nullableNormalizedString(value.Note),
			"sleep_kind_override": nullableNormalizedString(value.SleepKindOverride), "stopped_at": normalizeTimestampForHash(value.StoppedAt),
		}
		if value.ComponentID != "" {
			result["component_id"] = value.ComponentID
		}
		if value.CleanupShortBreastSegments {
			result["cleanup_short_breast_segments"] = true
		}
		return withBaby(result, value.BabyID), nil
	case TimerEditPayload:
		// A null start and an omitted one are the same patch, so both hash as null.
		return withBaby(map[string]any{
			"components": mapComponents(value.Components, false), "session_id": nullableString(value.SessionID),
			"started_at": nullableNormalizedTimestamp(value.StartedAt),
		}, value.BabyID), nil
	case DeletePayload:
		result := map[string]any{}
		if includeBabyID && value.BabyID != nil {
			result["baby_id"] = *value.BabyID
		}
		return result, nil
	default:
		return nil, fmt.Errorf("%w: payload %T", ErrInvalidMutation, payload)
	}
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableNormalizedString(value *string) any {
	if value == nil {
		return nil
	}
	normalized, err := NormalizeText(*value)
	if err != nil {
		return *value
	}
	return normalized
}

func nullableNormalizedTimestamp(value *string) any {
	if value == nil {
		return nil
	}
	return normalizeTimestampForHash(*value)
}

func nullableDecimal(value *Decimal) any {
	if value == nil {
		return nil
	}
	return *value
}
