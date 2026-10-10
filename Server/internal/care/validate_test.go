package care

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"babytracker/server/internal/protocol"
)

func TestDomainBounds(t *testing.T) {
	limits := protocol.DefaultLimits()
	valid := mustMutation(t, "feed-bottle.json")
	if err := ValidateMutation(valid, limits); err != nil {
		t.Fatalf("valid bottle rejected: %v", err)
	}

	invalidDate := mustMutation(t, "baby.json")
	baby := invalidDate.Payload.(protocol.BabyPayload)
	baby.BirthDate = "2026-02-29"
	invalidDate.Payload = baby
	if err := ValidateMutation(invalidDate, limits); !errors.Is(err, protocol.ErrInvalidTimestamp) {
		t.Fatalf("invalid leap date error = %v", err)
	}

	oversizedNote := mustMutation(t, "sleep.json")
	note := strings.Repeat("x", limits.MaxNoteBytes+1)
	sleep := oversizedNote.Payload.(protocol.SleepPayload)
	sleep.Note = &note
	oversizedNote.Payload = sleep
	if err := ValidateMutation(oversizedNote, limits); !errors.Is(err, protocol.ErrInvalidText) {
		t.Fatalf("oversized note error = %v", err)
	}
}

func TestDomainAllowsCombinationFeedAndZeroVolume(t *testing.T) {
	combination := mustMutation(t, "feed-combination.json")
	if err := ValidateMutation(combination, protocol.DefaultLimits()); err != nil {
		t.Fatalf("combination feed rejected: %v", err)
	}
	zero := mustMutation(t, "feed-bottle.json")
	feed := zero.Payload.(protocol.FeedPayload)
	feed.Components[0].VolumeML = protocol.MustParseDecimal("0")
	zero.Payload = feed
	if err := ValidateMutation(zero, protocol.DefaultLimits()); err != nil {
		t.Fatalf("zero volume rejected: %v", err)
	}
}

func TestV4RejectsReversedAndDurationMismatchedStepBounds(t *testing.T) {
	start, end := "2026-09-28T12:00:00.000000Z", "2026-09-28T12:02:00.000000Z"
	side, seconds := "left", uint32(120)
	feed := protocol.FeedPayload{BabyID: "10000000-0000-4000-8000-000000000001", Kind: "breast", StartedAt: start, EndedAt: &end, TimeZoneID: "UTC",
		Components: []protocol.FeedComponent{{ID: "30000000-0000-4000-8000-000000000001", Kind: "breast", Side: &side, Seconds: &seconds, StartedAt: &start, EndedAt: &end}}}
	mutation := protocol.Mutation{MutationID: "01000000-0000-4000-8000-000000000001", EntityID: "20000000-0000-4000-8000-000000000001", EntityKind: "feed", Operation: "create", PayloadVersion: 4, ClientMutationTime: end, Payload: feed}
	if err := ValidateMutation(mutation, protocol.DefaultLimits()); err != nil {
		t.Fatalf("valid timed step: %v", err)
	}
	feed.Components[0].StartedAt, feed.Components[0].EndedAt = &end, &start
	mutation.Payload = feed
	if err := ValidateMutation(mutation, protocol.DefaultLimits()); err == nil {
		t.Fatal("reversed step accepted")
	}
	feed.Components[0].StartedAt, feed.Components[0].EndedAt = &start, &end
	short := uint32(119)
	feed.Components[0].Seconds = &short
	mutation.Payload = feed
	if err := ValidateMutation(mutation, protocol.DefaultLimits()); err == nil {
		t.Fatal("duration mismatch accepted")
	}
}

func TestV4RejectsOverlappingStepBounds(t *testing.T) {
	start, firstEnd, secondStart, end := "2026-09-28T12:00:00.000000Z", "2026-09-28T12:02:00.000000Z", "2026-09-28T12:01:00.000000Z", "2026-09-28T12:03:00.000000Z"
	left, right, seconds := "left", "right", uint32(120)
	feed := protocol.FeedPayload{BabyID: "10000000-0000-4000-8000-000000000001", Kind: "breast", StartedAt: start, EndedAt: &end, TimeZoneID: "UTC",
		Components: []protocol.FeedComponent{
			{ID: "30000000-0000-4000-8000-000000000001", Kind: "breast", Side: &left, Seconds: &seconds, StartedAt: &start, EndedAt: &firstEnd},
			{ID: "30000000-0000-4000-8000-000000000002", Kind: "breast", Side: &right, Seconds: &seconds, StartedAt: &secondStart, EndedAt: &end},
		}}
	mutation := protocol.Mutation{MutationID: "01000000-0000-4000-8000-000000000001", EntityID: "20000000-0000-4000-8000-000000000001", EntityKind: "feed", Operation: "create", PayloadVersion: 4, ClientMutationTime: end, Payload: feed}
	if err := ValidateMutation(mutation, protocol.DefaultLimits()); err == nil {
		t.Fatal("overlapping steps accepted")
	}
}

func TestVolumeWireRoundingPrecedesSemanticBounds(t *testing.T) {
	mutation := mustMutation(t, "feed-bottle.json")
	feed := mutation.Payload.(protocol.FeedPayload)
	feed.Components[0].VolumeML = protocol.MustParseDecimal("2000.0000005")
	mutation.Payload = feed
	if err := ValidateMutation(mutation, protocol.DefaultLimits()); err != nil {
		t.Fatalf("round-to-maximum volume rejected: %v", err)
	}
	feed.Components[0].VolumeML = protocol.MustParseDecimal("2000.0000006")
	mutation.Payload = feed
	if err := ValidateMutation(mutation, protocol.DefaultLimits()); !errors.Is(err, protocol.ErrInvalidDecimal) {
		t.Fatalf("round-above-maximum volume error = %v", err)
	}
}

func TestDomainRejectsProcessLocalTimeZone(t *testing.T) {
	mutation := mustMutation(t, "sleep.json")
	payload := mutation.Payload.(protocol.SleepPayload)
	payload.TimeZoneID = "Local"
	mutation.Payload = payload
	if err := ValidateMutation(mutation, protocol.DefaultLimits()); err == nil {
		t.Fatal("process-local time zone accepted")
	}
}

func TestEveryFrozenMutationFixtureDecodesAndValidates(t *testing.T) {
	for _, name := range []string{"baby.json", "feed-breast.json", "feed-bottle.json", "feed-solids.json", "feed-combination.json", "sleep.json", "diaper.json", "supplement.json", "timer-feed.json", "timer-sleep.json"} {
		t.Run(name, func(t *testing.T) {
			mutation := mustMutation(t, name)
			if err := ValidateMutation(mutation, protocol.DefaultLimits()); err != nil {
				t.Fatalf("ValidateMutation: %v", err)
			}
		})
	}
	raw, err := os.ReadFile("../../api/fixtures/v1/mutations.json")
	if err != nil {
		t.Fatal(err)
	}
	var mutations map[string]json.RawMessage
	if err := json.Unmarshal(raw, &mutations); err != nil {
		t.Fatal(err)
	}
	for name, raw := range mutations {
		t.Run(name, func(t *testing.T) {
			mutation, err := protocol.DecodeMutation(raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateMutation(mutation, protocol.DefaultLimits()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func mustMutation(t *testing.T, fixture string) protocol.Mutation {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("../../api/fixtures/v1", fixture))
	if err != nil {
		t.Fatal(err)
	}
	mutation, err := protocol.DecodeMutation(raw)
	if err != nil {
		t.Fatal(err)
	}
	return mutation
}

func TestSupplementNameValidation(t *testing.T) {
	for _, tt := range []struct {
		name  string
		valid bool
	}{{"Vitamin D", true}, {"", false}, {"  ", false}, {strings.Repeat("x", 65), false}, {"D\x00", false}} {
		t.Run(tt.name, func(t *testing.T) {
			m := mustMutation(t, "supplement.json")
			p := m.Payload.(protocol.SupplementPayload)
			p.Name = tt.name
			m.Payload = p
			if err := ValidateMutation(m, protocol.DefaultLimits()); (err == nil) != tt.valid {
				t.Fatalf("name %q: %v", tt.name, err)
			}
		})
	}
}

func TestSupplementRequiresValidLoggedAt(t *testing.T) {
	for _, value := range []string{"", "not-a-date", "1899-12-31T23:59:59.000000Z"} {
		m := mustMutation(t, "supplement.json")
		p := m.Payload.(protocol.SupplementPayload)
		p.LoggedAt = value
		m.Payload = p
		if err := ValidateMutation(m, protocol.DefaultLimits()); err == nil {
			t.Fatalf("invalid logged_at %q accepted", value)
		}
	}
}

// A feed holds at most MaxSideIntervals breast parts, and the side still
// running at the stop becomes one more, so a start already holding
// MaxSideIntervals breast parts is refused.
func TestTimerStartBreastPartsFitTheSideIntervalLimit(t *testing.T) {
	limits := protocol.DefaultLimits()
	for count, wantErr := range map[int]bool{limits.MaxSideIntervals - 1: false, limits.MaxSideIntervals: true} {
		mutation := mustMutation(t, "timer-feed.json")
		timer := mutation.Payload.(protocol.TimerStartPayload)
		side, seconds := "left", uint32(1)
		timer.CompletedComponents = make([]protocol.FeedComponent, count)
		for index := range timer.CompletedComponents {
			timer.CompletedComponents[index] = protocol.FeedComponent{
				ID: fmt.Sprintf("30000000-0000-4000-8000-%012d", index+1), Kind: "breast", Side: &side, Seconds: &seconds}
		}
		mutation.Payload = timer
		raw, err := json.Marshal(mutation)
		if err != nil {
			t.Fatal(err)
		}
		if mutation, err = protocol.DecodeMutation(raw); err != nil {
			t.Fatalf("%d breast parts: decode: %v", count, err)
		}
		if err := ValidateMutation(mutation, limits); (err != nil) != wantErr {
			t.Fatalf("%d breast parts: err=%v, want error %v", count, err, wantErr)
		}
	}
}

// A feed holds at most MaxSideIntervals breast parts, so a timer's whole
// breast feed fits; more is a collection refusal, not a storage error.
func TestFeedBreastPartsFitTheSideIntervalLimit(t *testing.T) {
	limits := protocol.DefaultLimits()
	for count, want := range map[int]error{limits.MaxSideIntervals: nil, limits.MaxSideIntervals + 1: protocol.ErrInvalidCollection} {
		mutation := mustMutation(t, "feed-breast.json")
		feed := mutation.Payload.(protocol.FeedPayload)
		side, seconds := "left", uint32(1)
		feed.Components = make([]protocol.FeedComponent, count)
		for index := range feed.Components {
			feed.Components[index] = protocol.FeedComponent{
				ID: fmt.Sprintf("30000000-0000-4000-8000-%012d", index+1), Kind: "breast", Side: &side, Seconds: &seconds}
		}
		mutation.Payload = feed
		raw, err := json.Marshal(mutation)
		if err != nil {
			t.Fatal(err)
		}
		if mutation, err = protocol.DecodeMutation(raw); err != nil {
			t.Fatalf("%d breast parts: decode: %v", count, err)
		}
		if err := ValidateMutation(mutation, limits); !errors.Is(err, want) {
			t.Fatalf("%d breast parts: err=%v, want %v", count, err, want)
		}
	}
}

func TestGrowthProfileValidation(t *testing.T) {
	m := mustMutation(t, "growth-profile.json")
	if err := ValidateMutation(m, protocol.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	p := m.Payload.(protocol.GrowthProfilePayload)
	for _, sex := range []string{"", "female", "GIRLS"} {
		bad := p
		bad.ReferenceSex = sex
		m.Payload = bad
		if ValidateMutation(m, protocol.DefaultLimits()) == nil {
			t.Fatalf("sex %q accepted", sex)
		}
	}
	bad := p
	date := "2026-02-29"
	bad.ExpectedDueDate = &date
	m.Payload = bad
	if ValidateMutation(m, protocol.DefaultLimits()) == nil {
		t.Fatal("invalid date accepted")
	}
	m.Payload = p
	m.EntityID = "70000000-0000-4000-8000-000000000001"
	if ValidateMutation(m, protocol.DefaultLimits()) == nil {
		t.Fatal("noncanonical profile ID accepted")
	}
}

func TestMeasurementValidation(t *testing.T) {
	m := mustMutation(t, "measurement.json")
	p := m.Payload.(protocol.MeasurementPayload)
	if err := ValidateMutation(m, protocol.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	cases := []protocol.MeasurementPayload{}
	bad := p
	bad.WeightKG = nil
	bad.LengthCM = nil
	bad.HeadCircumferenceCM = nil
	bad.LengthPosition = nil
	cases = append(cases, bad)
	bad = p
	bad.WeightKG = protocol.MustParseDecimal("0")
	cases = append(cases, bad)
	bad = p
	bad.WeightKG = protocol.MustParseDecimal("-1")
	cases = append(cases, bad)
	bad = p
	bad.LengthPosition = nil
	cases = append(cases, bad)
	bad = p
	bad.LengthCM = nil
	cases = append(cases, bad)
	bad = p
	position := "sitting"
	bad.LengthPosition = &position
	cases = append(cases, bad)
	bad = p
	bad.MeasuredAt = "bad"
	cases = append(cases, bad)
	bad = p
	bad.LoggedAt = "bad"
	cases = append(cases, bad)
	bad = p
	bad.TimeZoneID = "Local"
	cases = append(cases, bad)
	for i, p := range cases {
		m.Payload = p
		if ValidateMutation(m, protocol.DefaultLimits()) == nil {
			t.Fatalf("invalid case %d accepted", i)
		}
	}
}

func TestBabyIconValidation(t *testing.T) {
	limits := protocol.DefaultLimits()
	for _, icon := range []string{"boy.1", "boy.4", "girl.1", "girl.4"} {
		m := mustMutation(t, "baby.json")
		p := m.Payload.(protocol.BabyPayload)
		value := icon
		p.Icon = &value
		m.Payload = p
		if err := ValidateMutation(m, limits); err != nil {
			t.Fatalf("icon %q rejected: %v", icon, err)
		}
	}
	for _, icon := range []string{"", "neutral", "boy.0", "boy.5", "girl", "BOY.1"} {
		m := mustMutation(t, "baby.json")
		p := m.Payload.(protocol.BabyPayload)
		value := icon
		p.Icon = &value
		m.Payload = p
		if err := ValidateMutation(m, limits); !errors.Is(err, protocol.ErrInvalidMutation) {
			t.Fatalf("icon %q error = %v", icon, err)
		}
	}
}
