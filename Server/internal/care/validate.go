package care

import (
	"strings"
	"time"
	_ "time/tzdata"
	"unicode"
	"unicode/utf8"

	"babytracker/server/internal/protocol"
)

// ValidateMutation is the first-line semantic check of a mutation: ids,
// payload versions, the payload shape of the kind and operation, value
// bounds, timestamps and text. Its error maps to a rejection code
// (rejectionCode).
func ValidateMutation(mutation protocol.Mutation, limits protocol.Limits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	if !protocol.ValidUUID(mutation.MutationID) || !protocol.ValidUUID(mutation.EntityID) ||
		!protocol.AcceptedPayloadVersion(mutation.EntityKind, mutation.Operation, mutation.PayloadVersion) ||
		mutation.ObservedEntityVersion > protocol.MaxSafeInteger {
		return protocol.ErrInvalidMutation
	}
	if mutation.ImportID != nil && !protocol.ValidUUID(*mutation.ImportID) {
		return protocol.ErrInvalidMutation
	}
	if _, err := protocol.NormalizeTimestamp(mutation.ClientMutationTime); err != nil {
		return protocol.ErrInvalidTimestamp
	}
	switch payload := mutation.Payload.(type) {
	case protocol.BabyPayload:
		if mutation.EntityKind != "baby" || (mutation.Operation != "create" && mutation.Operation != "update") {
			return protocol.ErrInvalidMutation
		}
		return validateBaby(payload, limits)
	case protocol.FeedPayload:
		if mutation.EntityKind != "feed" || (mutation.Operation != "create" && mutation.Operation != "update") {
			return protocol.ErrInvalidMutation
		}
		if payload.FeedingSessionID != nil && !protocol.ValidUUID(*payload.FeedingSessionID) {
			return protocol.ErrInvalidMutation
		}
		return validateFeed(payload, limits)
	case protocol.SleepPayload:
		if mutation.EntityKind != "sleep" || (mutation.Operation != "create" && mutation.Operation != "update") {
			return protocol.ErrInvalidMutation
		}
		return validateSleep(payload, limits)
	case protocol.GrowthProfilePayload:
		if mutation.EntityKind != "growth_profile" || (mutation.Operation != "create" && mutation.Operation != "update") || !protocol.ValidUUID(payload.BabyID) || mutation.EntityID != protocol.GrowthProfileID(payload.BabyID) || !oneOf(payload.ReferenceSex, "girls", "boys") {
			return protocol.ErrInvalidMutation
		}
		if payload.ExpectedDueDate != nil {
			return validateBirthDate(*payload.ExpectedDueDate)
		}
		return nil
	case protocol.MeasurementPayload:
		if mutation.EntityKind != "measurement" || (mutation.Operation != "create" && mutation.Operation != "update") || !protocol.ValidUUID(payload.BabyID) || !validTimeZone(payload.TimeZoneID) {
			return protocol.ErrInvalidMutation
		}
		if err := validateTimestamp(payload.MeasuredAt); err != nil {
			return err
		}
		if err := validateTimestamp(payload.LoggedAt); err != nil {
			return err
		}
		if payload.WeightKG == nil && payload.LengthCM == nil && payload.HeadCircumferenceCM == nil {
			return protocol.ErrInvalidMutation
		}
		for _, value := range []*protocol.Decimal{payload.WeightKG, payload.LengthCM, payload.HeadCircumferenceCM} {
			if value != nil && value.CmpInteger(0) <= 0 {
				return protocol.ErrInvalidDecimal
			}
		}
		if (payload.LengthCM == nil) != (payload.LengthPosition == nil) || (payload.LengthPosition != nil && !oneOf(*payload.LengthPosition, "lying", "standing")) {
			return protocol.ErrInvalidMutation
		}
		return nil
	case protocol.SupplementPayload:
		if err := validateTimestamp(payload.LoggedAt); err != nil {
			return err
		}
		if mutation.EntityKind != "supplement" || (mutation.Operation != "create" && mutation.Operation != "update") {
			return protocol.ErrInvalidMutation
		}
		if !protocol.ValidUUID(payload.BabyID) || !validTimeZone(payload.TimeZoneID) {
			return protocol.ErrInvalidMutation
		}
		if err := validateTimestamp(payload.StartedAt); err != nil {
			return err
		}
		if strings.TrimSpace(payload.Name) == "" {
			return protocol.ErrInvalidText
		}
		if err := validateOptionalText(&payload.Name, 256, 64, false); err != nil {
			return err
		}
		return validateOptionalText(payload.Note, limits.MaxNoteBytes, 0, true)
	case protocol.PumpPayload:
		if mutation.EntityKind != "pump" || (mutation.Operation != "create" && mutation.Operation != "update") {
			return protocol.ErrInvalidMutation
		}
		if !validTimeZone(payload.TimeZoneID) || !oneOf(payload.Side, "left", "right", "both") {
			return protocol.ErrInvalidMutation
		}
		if err := validatePumpTimes(payload.StartedAt, payload.EndedAt); err != nil {
			return err
		}
		if payload.VolumeML != nil && (payload.VolumeML.CmpInteger(0) < 0 || payload.VolumeML.CmpInteger(1000) > 0 || payload.VolumeML.Scale() > 2) {
			return protocol.ErrInvalidDecimal
		}
		return validateOptionalText(payload.Note, limits.MaxNoteBytes, 0, true)
	case protocol.ImmunizationPayload:
		if mutation.EntityKind != "immunization" || (mutation.Operation != "create" && mutation.Operation != "update") {
			return protocol.ErrInvalidMutation
		}
		if !protocol.ValidUUID(payload.BabyID) || !validTimeZone(payload.TimeZoneID) {
			return protocol.ErrInvalidMutation
		}
		if err := validateTimestamp(payload.GivenAt); err != nil {
			return err
		}
		if strings.TrimSpace(payload.Vaccine) == "" {
			return protocol.ErrInvalidText
		}
		if err := validateOptionalText(&payload.Vaccine, 256, 64, false); err != nil {
			return err
		}
		if payload.Dose != nil && (*payload.Dose < 1 || *payload.Dose > 10) {
			return protocol.ErrInvalidMutation
		}
		return validateOptionalText(payload.Note, limits.MaxNoteBytes, 0, true)
	case protocol.DiaperPayload:
		if mutation.EntityKind != "diaper" || (mutation.Operation != "create" && mutation.Operation != "update") {
			return protocol.ErrInvalidMutation
		}
		return validateDiaper(payload, limits)
	case protocol.TimerStartPayload:
		if mutation.EntityKind != "timer" || mutation.Operation != "timer_start" {
			return protocol.ErrInvalidMutation
		}
		return validateTimerStart(payload, limits)
	case protocol.TimerBottleFinishPayload:
		if mutation.EntityKind != "timer" || mutation.Operation != "timer_finish_bottle" || !protocol.ValidUUID(payload.BabyID) || !protocol.ValidUUID(payload.ComponentID) || payload.VolumeML == nil || payload.Contents == nil {
			return protocol.ErrInvalidMutation
		}
		if err := validateTimestamp(payload.FinishedAt); err != nil {
			return err
		}
		return validateBottle(payload.VolumeML, payload.Contents)
	case protocol.TimerStopPayload:
		if mutation.EntityKind != "timer" || mutation.Operation != "timer_stop" || !protocol.ValidUUID(payload.BabyID) ||
			!protocol.ValidUUID(payload.CompletedRecordID) {
			return protocol.ErrInvalidMutation
		}
		if payload.ComponentID != "" && !protocol.ValidUUID(payload.ComponentID) {
			return protocol.ErrInvalidMutation
		}
		if err := validateTimestamp(payload.StoppedAt); err != nil {
			return err
		}
		if err := validateOptionalText(payload.Note, limits.MaxNoteBytes, 0, true); err != nil {
			return err
		}
		if payload.SleepKindOverride != nil && !oneOf(*payload.SleepKindOverride, "nap", "night") {
			return protocol.ErrInvalidMutation
		}
		return nil
	case protocol.TimerEditPayload:
		if mutation.EntityKind != "timer" || mutation.Operation != "update" || !protocol.ValidUUID(payload.BabyID) {
			return protocol.ErrInvalidMutation
		}
		if payload.StartedAt != nil {
			if err := validateTimestamp(*payload.StartedAt); err != nil {
				return err
			}
		}
		if err := validateComponents(payload.Components, limits); err != nil {
			return err
		}
		// session_id is always a real field of the patch (null included), so a
		// session-only edit is content on its own.
		return nil
	case protocol.DeletePayload:
		if mutation.Operation != "delete" || !oneOf(mutation.EntityKind, "baby", "feed", "sleep", "diaper", "supplement", "growth_profile", "measurement", "immunization", "pump") {
			return protocol.ErrInvalidMutation
		}
		if mutation.EntityKind == "baby" || mutation.EntityKind == "pump" {
			if payload.BabyID != nil {
				return protocol.ErrInvalidMutation
			}
			return nil
		}
		if payload.BabyID == nil || !protocol.ValidUUID(*payload.BabyID) {
			return protocol.ErrInvalidMutation
		}
		return nil
	default:
		return protocol.ErrInvalidMutation
	}
}

func validateBaby(payload protocol.BabyPayload, limits protocol.Limits) error {
	name, err := protocol.NormalizeDisplayName(payload.Name)
	if err != nil || name != payload.Name || utf8.RuneCountInString(name) > limits.MaxNameScalars || len(name) > limits.MaxNameBytes || hasControl(name, false) {
		return protocol.ErrInvalidText
	}
	if payload.SortOrder > uint32(limits.MaxSortOrder) {
		return protocol.ErrInvalidMutation
	}
	if payload.Icon != nil && !oneOf(*payload.Icon, "boy.1", "boy.2", "boy.3", "boy.4", "girl.1", "girl.2", "girl.3", "girl.4") {
		return protocol.ErrInvalidMutation
	}
	return validateBirthDate(payload.BirthDate)
}

func validateFeed(payload protocol.FeedPayload, limits protocol.Limits) error {
	if !protocol.ValidUUID(payload.BabyID) || !oneOf(payload.Kind, "breast", "bottle", "solids") ||
		!validTimeZone(payload.TimeZoneID) {
		return protocol.ErrInvalidMutation
	}
	if err := validateTimeRange(payload.StartedAt, payload.EndedAt); err != nil {
		return err
	}
	if err := validateOptionalText(payload.Note, limits.MaxNoteBytes, 0, true); err != nil {
		return err
	}
	if len(payload.Components) == 0 {
		return protocol.ErrInvalidCollection
	}
	if err := validateComponents(payload.Components, limits); err != nil {
		return err
	}
	if breastPartCount(payload.Components) > limits.MaxSideIntervals {
		return protocol.ErrInvalidCollection
	}
	parentStart, _ := time.Parse(time.RFC3339Nano, payload.StartedAt)
	var parentEnd time.Time
	if payload.EndedAt != nil {
		parentEnd, _ = time.Parse(time.RFC3339Nano, *payload.EndedAt)
	}
	for _, component := range payload.Components {
		for _, bound := range []*string{component.StartedAt, component.EndedAt} {
			if bound == nil {
				continue
			}
			at, _ := time.Parse(time.RFC3339Nano, *bound)
			if at.Before(parentStart) || (!parentEnd.IsZero() && at.After(parentEnd)) {
				return protocol.ErrInvalidMutation
			}
		}
	}
	return nil
}

func validateSleep(payload protocol.SleepPayload, limits protocol.Limits) error {
	if !protocol.ValidUUID(payload.BabyID) || !validTimeZone(payload.TimeZoneID) {
		return protocol.ErrInvalidMutation
	}
	if err := validateTimeRange(payload.StartedAt, payload.EndedAt); err != nil {
		return err
	}
	if err := validateOptionalText(payload.Note, limits.MaxNoteBytes, 0, true); err != nil {
		return err
	}
	if payload.KindOverride != nil && !oneOf(*payload.KindOverride, "nap", "night") {
		return protocol.ErrInvalidMutation
	}
	return nil
}

func validateDiaper(payload protocol.DiaperPayload, limits protocol.Limits) error {
	if !protocol.ValidUUID(payload.BabyID) || !validTimeZone(payload.TimeZoneID) ||
		!oneOf(payload.Contents, "wet", "dirty", "mixed", "dry") {
		return protocol.ErrInvalidMutation
	}
	if err := validateTimeRange(payload.StartedAt, payload.EndedAt); err != nil {
		return err
	}
	if err := validateOptionalText(payload.Note, limits.MaxNoteBytes, 0, true); err != nil {
		return err
	}
	if err := validateOptionalText(payload.Consistency, 256, 64, false); err != nil {
		return err
	}
	return validateOptionalText(payload.Color, 256, 64, false)
}

func validateTimerStart(payload protocol.TimerStartPayload, limits protocol.Limits) error {
	if !protocol.ValidUUID(payload.BabyID) || !oneOf(payload.TimerKind, "feed", "sleep") || !validTimeZone(payload.TimeZoneID) {
		return protocol.ErrInvalidMutation
	}
	if err := validateTimestamp(payload.StartedAt); err != nil {
		return err
	}
	if err := validateComponents(payload.CompletedComponents, limits); err != nil {
		return err
	}
	if breastPartCount(payload.CompletedComponents) > limits.MaxSideIntervals {
		return protocol.ErrInvalidCollection
	}
	if payload.TimerKind == "sleep" {
		if len(payload.CompletedComponents) != 0 || payload.CurrentSide != nil || payload.CurrentSideStartedAt != nil || payload.CurrentBottleStartedAt != nil {
			return protocol.ErrInvalidMutation
		}
		return nil
	}
	// A running feed timer keeps one breast part free for the side it is
	// timing, so the feed its stop completes fits MaxSideIntervals.
	if (payload.CurrentSide == nil) != (payload.CurrentSideStartedAt == nil) ||
		breastPartCount(payload.CompletedComponents) >= limits.MaxSideIntervals {
		return protocol.ErrInvalidMutation
	}
	if payload.CurrentSide != nil {
		if !oneOf(*payload.CurrentSide, "left", "right") {
			return protocol.ErrInvalidMutation
		}
		if err := validateTimestamp(*payload.CurrentSideStartedAt); err != nil {
			return err
		}
		started, err := parseTimerTime(payload.StartedAt)
		if err != nil {
			return protocol.ErrInvalidTimestamp
		}
		current, err := parseTimerTime(*payload.CurrentSideStartedAt)
		if err != nil || current.Before(started) {
			return protocol.ErrInvalidTimestamp
		}
	}
	if payload.CurrentBottleStartedAt != nil {
		if payload.CurrentSide != nil {
			return protocol.ErrInvalidMutation
		}
		if err := validateTimestamp(*payload.CurrentBottleStartedAt); err != nil {
			return err
		}
		start, _ := parseTimerTime(payload.StartedAt)
		bottle, _ := parseTimerTime(*payload.CurrentBottleStartedAt)
		if bottle.Before(start) {
			return protocol.ErrInvalidTimestamp
		}
	}
	return nil
}

func validateComponents(components []protocol.FeedComponent, limits protocol.Limits) error {
	if len(components) > 512 {
		return protocol.ErrInvalidCollection
	}
	seen := map[string]bool{}
	totalSeconds := 0
	var lastKnownEnd time.Time
	for _, component := range components {
		if !protocol.ValidUUID(component.ID) || seen[component.ID] {
			return protocol.ErrInvalidMutation
		}
		seen[component.ID] = true
		if component.StartedAt != nil {
			if err := validateTimestamp(*component.StartedAt); err != nil {
				return err
			}
		}
		if component.EndedAt != nil {
			if err := validateTimestamp(*component.EndedAt); err != nil {
				return err
			}
		}
		if component.StartedAt != nil && component.EndedAt != nil {
			start, err := time.Parse(time.RFC3339Nano, *component.StartedAt)
			if err != nil {
				return protocol.ErrInvalidTimestamp
			}
			end, err := time.Parse(time.RFC3339Nano, *component.EndedAt)
			if err != nil {
				return protocol.ErrInvalidTimestamp
			}
			if !end.After(start) || component.Seconds == nil || uint32(end.Sub(start)/time.Second) != *component.Seconds {
				return protocol.ErrInvalidMutation
			}
			if !lastKnownEnd.IsZero() && start.Before(lastKnownEnd) {
				return protocol.ErrInvalidMutation
			}
			lastKnownEnd = end
		}
		switch component.Kind {
		case "breast":
			if component.Side == nil || component.Seconds == nil || !oneOf(*component.Side, "left", "right") || *component.Seconds == 0 || *component.Seconds > uint32(limits.MaxSideSeconds) || component.VolumeML != nil || component.Contents != nil || component.Foods != nil || component.Amount != nil {
				return protocol.ErrInvalidMutation
			}
			totalSeconds += int(*component.Seconds)
			if totalSeconds > limits.MaxSideTotalSeconds {
				return protocol.ErrInvalidMutation
			}
		case "bottle":
			if component.Side != nil || (component.Seconds != nil && (*component.Seconds == 0 || *component.Seconds > 604800)) || component.VolumeML == nil || component.Contents == nil || component.Foods != nil || component.Amount != nil {
				return protocol.ErrInvalidMutation
			}
			if err := validateBottle(component.VolumeML, component.Contents); err != nil {
				return err
			}
		case "solids":
			if component.Side != nil || component.Seconds != nil || component.StartedAt != nil || component.EndedAt != nil || component.VolumeML != nil || component.Contents != nil || component.Foods == nil || component.Amount == nil || !oneOf(*component.Amount, "taste", "some", "lots") || len(component.Foods) > limits.MaxFoods {
				return protocol.ErrInvalidMutation
			}
			for _, food := range component.Foods {
				if food == "" || utf8.RuneCountInString(food) > limits.MaxFoodScalars || len(food) > limits.MaxFoodBytes || hasControl(food, false) {
					return protocol.ErrInvalidText
				}
			}
		default:
			return protocol.ErrInvalidMutation
		}
	}
	return nil
}

func validateBottle(volume *protocol.Decimal, contents *string) error {
	if volume != nil && (volume.CmpInteger(0) < 0 || volume.CmpInteger(2000) > 0) {
		return protocol.ErrInvalidDecimal
	}
	if contents != nil && !oneOf(*contents, "breast_milk", "formula", "mixed") {
		return protocol.ErrInvalidMutation
	}
	return nil
}

func validateBirthDate(value string) error {
	if len(value) != 10 || value[4] != '-' || value[7] != '-' {
		return protocol.ErrInvalidTimestamp
	}
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil || parsed.Year() < 1900 || parsed.Year() > 2100 || parsed.Format("2006-01-02") != value {
		return protocol.ErrInvalidTimestamp
	}
	return nil
}

func validateTimestamp(value string) error {
	_, err := protocol.NormalizeTimestamp(value)
	if err != nil {
		return protocol.ErrInvalidTimestamp
	}
	return nil
}

func validateTimeRange(start string, end *string) error {
	startNormalized, err := protocol.NormalizeTimestamp(start)
	if err != nil {
		return protocol.ErrInvalidTimestamp
	}
	if end == nil {
		return nil
	}
	endNormalized, err := protocol.NormalizeTimestamp(*end)
	if err != nil {
		return protocol.ErrInvalidTimestamp
	}
	startTime, _ := time.Parse("2006-01-02T15:04:05.000000Z", startNormalized)
	endTime, _ := time.Parse("2006-01-02T15:04:05.000000Z", endNormalized)
	if endTime.Before(startTime) || endTime.Sub(startTime) > 7*24*time.Hour {
		return protocol.ErrInvalidTimestamp
	}
	return nil
}

// validatePumpTimes: a pumping session's end, when it has one, is not before
// its start and at most six hours after it. Whether either is in the future
// needs the accept time, so the record rules check that (applyRecord).
func validatePumpTimes(start string, end *string) error {
	startNormalized, err := protocol.NormalizeTimestamp(start)
	if err != nil {
		return protocol.ErrInvalidTimestamp
	}
	if end == nil {
		return nil
	}
	endNormalized, err := protocol.NormalizeTimestamp(*end)
	if err != nil {
		return protocol.ErrInvalidTimestamp
	}
	startTime, _ := time.Parse("2006-01-02T15:04:05.000000Z", startNormalized)
	endTime, _ := time.Parse("2006-01-02T15:04:05.000000Z", endNormalized)
	if endTime.Before(startTime) || endTime.Sub(startTime) > 6*time.Hour {
		return protocol.ErrInvalidTimestamp
	}
	return nil
}

func validTimeZone(identifier string) bool {
	if identifier == "" || identifier == "Local" || strings.TrimSpace(identifier) != identifier {
		return false
	}
	_, err := time.LoadLocation(identifier)
	return err == nil
}

func validateOptionalText(value *string, maxBytes, maxScalars int, note bool) error {
	if value == nil {
		return nil
	}
	if !utf8.ValidString(*value) || len(*value) > maxBytes || maxScalars > 0 && utf8.RuneCountInString(*value) > maxScalars || hasControl(*value, note) {
		return protocol.ErrInvalidText
	}
	normalized, err := protocol.NormalizeText(*value)
	if err != nil || normalized != *value {
		return protocol.ErrInvalidText
	}
	return nil
}

func hasControl(value string, note bool) bool {
	for _, character := range value {
		if unicode.IsControl(character) && !(note && (character == '\t' || character == '\n')) {
			return true
		}
	}
	return false
}

func oneOf(value string, values ...string) bool {
	for _, candidate := range values {
		if value == candidate {
			return true
		}
	}
	return false
}
