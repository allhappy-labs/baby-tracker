package records

import (
	"babytracker/server/internal/protocol"
	"testing"
)

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
