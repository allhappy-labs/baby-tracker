package testpg

import (
	"testing"
	"time"
)

func TestAuditMonthlyLineDoesNotChangeWithTheCalendar(t *testing.T) {
	const complete = "audit_monthly|public|security_audit_events|first=expected|through_now_plus_92d=complete"
	at := func(value string) time.Time {
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name     string
		now      time.Time
		suffixes []string
		want     string
	}{
		{"before 2027: nothing yet", at("2026-08-01T00:00:00Z"), nil, complete},
		{"before 2027: the first month", at("2026-10-09T12:00:00Z"), []string{"2027_01"}, complete},
		{"before 2027: the horizon reaches February", at("2026-11-15T00:00:00Z"), []string{"2027_01", "2027_02"}, complete},
		// From February 2027 a fresh install starts at the current month.
		{"after 2027-02: starts at the current month", at("2027-03-15T00:00:00Z"), []string{"2027_03", "2027_04", "2027_05", "2027_06"}, complete},
		{"after 2027-02: a year later", at("2028-02-01T00:00:00Z"), []string{"2028_02", "2028_03", "2028_04", "2028_05"}, complete},
		// The month is the UTC month, whatever zone now is expressed in.
		{"a UTC month boundary in another zone", at("2027-04-01T02:00:00Z").In(newYork), []string{"2027_04", "2027_05", "2027_06", "2027_07"}, complete},
		{"an extra leading month", at("2027-03-15T00:00:00Z"), []string{"2027_02", "2027_03", "2027_04", "2027_05", "2027_06"},
			"audit_monthly|public|security_audit_events|first=2027_02|through_now_plus_92d=missing=;beyond=2027_02"},
		{"a missing first month", at("2027-03-15T00:00:00Z"), []string{"2027_04", "2027_05", "2027_06"},
			"audit_monthly|public|security_audit_events|first=2027_04|through_now_plus_92d=missing=2027_03;beyond="},
		{"a gap", at("2027-03-15T00:00:00Z"), []string{"2027_03", "2027_05", "2027_06"},
			"audit_monthly|public|security_audit_events|first=expected|through_now_plus_92d=missing=2027_04;beyond="},
		{"a month past the horizon", at("2026-10-09T12:00:00Z"), []string{"2027_01", "2027_02"},
			"audit_monthly|public|security_audit_events|first=expected|through_now_plus_92d=missing=;beyond=2027_02"},
		{"none where some are due", at("2026-10-09T12:00:00Z"), nil,
			"audit_monthly|public|security_audit_events|first=none|through_now_plus_92d=missing=2027_01;beyond="},
	} {
		if got := auditMonthlyLine(c.suffixes, c.now); got != c.want {
			t.Errorf("%s: auditMonthlyLine = %q, want %q", c.name, got, c.want)
		}
	}
}
