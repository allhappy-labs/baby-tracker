package familyread

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestDecimal6EncodesExactStablePrecision(t *testing.T) {
	for value, want := range map[Decimal6]string{
		0: "0", 1: "0.000001", 10: "0.00001", 1_500_000: "1.5", 82_800_000_000: "82800",
		350_500_000: "350.5", 123_456_789: "123.456789", -1_500_000: "-1.5",
	} {
		raw, err := json.Marshal(value)
		if err != nil || string(raw) != want {
			t.Errorf("%d encodes %s (%v), want %s", int64(value), raw, err, want)
		}
	}
}

func TestSummaryRejectsInvalidQueriesBeforeReading(t *testing.T) {
	start := day0
	valid := SummaryQuery{BabyID: fixtureBab, Start: start, End: start.Add(24 * time.Hour), Timezone: "Europe/Zurich"}
	with := func(change func(*SummaryQuery)) SummaryQuery {
		q := valid
		change(&q)
		return q
	}
	for name, test := range map[string]struct {
		query SummaryQuery
		want  error
	}{
		"zero start":             {with(func(q *SummaryQuery) { q.Start = time.Time{} }), ErrInvalidRange},
		"empty range":            {with(func(q *SummaryQuery) { q.End = q.Start }), ErrInvalidRange},
		"reversed range":         {with(func(q *SummaryQuery) { q.End = q.Start.Add(-time.Hour) }), ErrInvalidRange},
		"744h plus a nanosecond": {with(func(q *SummaryQuery) { q.End = q.Start.Add(MaxRangeElapsed + time.Nanosecond) }), ErrRangeTooLong},
		"32 days":                {with(func(q *SummaryQuery) { q.End = q.Start.Add(32 * 24 * time.Hour) }), ErrRangeTooLong},
		"malformed baby":         {with(func(q *SummaryQuery) { q.BabyID = "not-a-uuid" }), ErrInvalidBabyID},
		"empty timezone":         {with(func(q *SummaryQuery) { q.Timezone = "" }), ErrInvalidTimezone},
		"Local timezone":         {with(func(q *SummaryQuery) { q.Timezone = "Local" }), ErrInvalidTimezone},
		"unknown zone":           {with(func(q *SummaryQuery) { q.Timezone = "Mars/Olympus_Mons" }), ErrInvalidTimezone},
		"leading space":          {with(func(q *SummaryQuery) { q.Timezone = " Europe/Zurich" }), ErrInvalidTimezone},
		"trailing slash":         {with(func(q *SummaryQuery) { q.Timezone = "Europe/Zurich/" }), ErrInvalidTimezone},
		"double slash":           {with(func(q *SummaryQuery) { q.Timezone = "Europe//Zurich" }), ErrInvalidTimezone},
		"dot segment":            {with(func(q *SummaryQuery) { q.Timezone = "./UTC" }), ErrInvalidTimezone},
		"parent segment":         {with(func(q *SummaryQuery) { q.Timezone = "Europe/../UTC" }), ErrInvalidTimezone},
		"absolute path":          {with(func(q *SummaryQuery) { q.Timezone = "/usr/share/zoneinfo/UTC" }), ErrInvalidTimezone},
		"numeric offset":         {with(func(q *SummaryQuery) { q.Timezone = "+02:00" }), ErrInvalidTimezone},
		"overlong":               {with(func(q *SummaryQuery) { q.Timezone = strings.Repeat("A", 256) }), ErrInvalidTimezone},
	} {
		t.Run(name, func(t *testing.T) {
			guard := &stubGuard{}
			service := New(guard, fixedKey(1), func() time.Time { return day0 })
			result, err := service.Summary(t.Context(), "credential", test.query)
			if !errors.Is(err, test.want) || Code(err) != test.want.Error() || guard.calls != 0 || !reflect.DeepEqual(result, SummaryResult{}) {
				t.Fatalf("err=%v code=%q calls=%d result=%+v, want %v before any read", err, Code(err), guard.calls, result, test.want)
			}
		})
	}
	for name, query := range map[string]SummaryQuery{
		"exactly 744 hours":  with(func(q *SummaryQuery) { q.End = q.Start.Add(MaxRangeElapsed) }),
		"UTC":                with(func(q *SummaryQuery) { q.Timezone = "UTC" }),
		"Etc offset zone":    with(func(q *SummaryQuery) { q.Timezone = "Etc/GMT-14" }),
		"three-segment zone": with(func(q *SummaryQuery) { q.Timezone = "America/Argentina/Buenos_Aires" }),
		"non-UTC query instant": with(func(q *SummaryQuery) {
			q.Start, q.End = q.Start.In(time.FixedZone("x", 3600)), q.End.In(time.FixedZone("y", -7200))
		}),
	} {
		t.Run(name, func(t *testing.T) {
			guard := &stubGuard{}
			if _, err := New(guard, fixedKey(1), func() time.Time { return day0 }).Summary(t.Context(), "credential", query); err != nil || guard.calls != 1 {
				t.Fatalf("err=%v calls=%d, want one guarded read", err, guard.calls)
			}
		})
	}
	for name, service := range map[string]*Service{
		"nil guard": New(nil, fixedKey(1), time.Now),
		"nil clock": New(&stubGuard{}, fixedKey(1), nil),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := service.Summary(t.Context(), "credential", valid); !errors.Is(err, authority.ErrUnavailable) {
				t.Fatalf("summary err=%v", err)
			}
		})
	}
}

// oracleDays partitions [start,end) by stepping minute by minute (start and
// every zone transition lie on whole minutes): a new day begins at the first
// minute whose local date is later than every date seen so far.
func oracleDays(start, end time.Time, loc *time.Location) []SummaryDay {
	var days []SummaryDay
	latest := ""
	for at := start; at.Before(end); at = at.Add(time.Minute) {
		if date := at.In(loc).Format("2006-01-02"); date > latest {
			if len(days) > 0 {
				days[len(days)-1].End = at
			}
			days = append(days, SummaryDay{Date: date, Start: at})
			latest = date
		}
	}
	days[len(days)-1].End = end
	for i := range days {
		days[i].ElapsedSeconds = Decimal6(days[i].End.Sub(days[i].Start).Microseconds())
	}
	return days
}

func TestLocalDaysAdvanceThroughActualInstants(t *testing.T) {
	type window struct {
		zone       string
		start, end time.Time
	}
	at := func(zone string, y int, m time.Month, d, h, min int) time.Time {
		return time.Date(y, m, d, h, min, 0, 0, mustZone(t, zone)).UTC()
	}
	for name, w := range map[string]window{
		"Zurich spring forward":       {"Europe/Zurich", at("Europe/Zurich", 2026, 3, 28, 0, 0), at("Europe/Zurich", 2026, 3, 31, 0, 0)},
		"Zurich fall back":            {"Europe/Zurich", at("Europe/Zurich", 2025, 10, 25, 0, 0), at("Europe/Zurich", 2025, 10, 28, 0, 0)},
		"Zurich partial edges":        {"Europe/Zurich", at("Europe/Zurich", 2026, 3, 28, 13, 37), at("Europe/Zurich", 2026, 3, 30, 4, 1)},
		"Zurich full 744 hours":       {"Europe/Zurich", at("Europe/Zurich", 2026, 3, 15, 5, 0), at("Europe/Zurich", 2026, 3, 15, 5, 0).Add(MaxRangeElapsed)},
		"UTC":                         {"UTC", at("UTC", 2026, 9, 1, 0, 0), at("UTC", 2026, 9, 3, 12, 0)},
		"Beirut midnight gap":         {"Asia/Beirut", at("Asia/Beirut", 2026, 3, 27, 0, 0), at("Asia/Beirut", 2026, 3, 31, 0, 0)},
		"Sao Paulo midnight fallback": {"America/Sao_Paulo", time.Date(2018, 2, 16, 3, 0, 0, 0, time.UTC), time.Date(2018, 2, 20, 3, 0, 0, 0, time.UTC)},
		"Apia skipped date":           {"Pacific/Apia", time.Date(2011, 12, 28, 0, 0, 0, 0, time.UTC), time.Date(2012, 1, 2, 0, 0, 0, 0, time.UTC)},
		"Lord Howe half hour":         {"Australia/Lord_Howe", at("Australia/Lord_Howe", 2026, 4, 4, 0, 0), at("Australia/Lord_Howe", 2026, 4, 7, 0, 0)},
	} {
		t.Run(name, func(t *testing.T) {
			loc := mustZone(t, w.zone)
			got, err := localDays(w.start, w.end, loc)
			if err != nil {
				t.Fatal(err)
			}
			want := oracleDays(w.start, w.end, loc)
			if len(got) != len(want) {
				t.Fatalf("%d days, want %d: %+v", len(got), len(want), got)
			}
			for i := range want {
				if got[i].date != want[i].Date || !got[i].start.Equal(want[i].Start) || !got[i].end.Equal(want[i].End) {
					t.Fatalf("day %d = %s [%s,%s), want %s [%s,%s)", i, got[i].date, got[i].start, got[i].end, want[i].Date, want[i].Start, want[i].End)
				}
			}
		})
	}
	zurich := mustZone(t, "Europe/Zurich")
	for _, check := range []struct {
		start, end time.Time
		date       string
		hours      time.Duration
	}{
		{at("Europe/Zurich", 2026, 3, 28, 0, 0), at("Europe/Zurich", 2026, 3, 31, 0, 0), "2026-03-29", 23},
		{at("Europe/Zurich", 2025, 10, 25, 0, 0), at("Europe/Zurich", 2025, 10, 28, 0, 0), "2025-10-26", 25},
	} {
		days, _ := localDays(check.start, check.end, zurich)
		if len(days) != 3 || days[1].date != check.date || days[1].end.Sub(days[1].start) != check.hours*time.Hour {
			t.Fatalf("days %+v, want %s to last %dh", days, check.date, check.hours)
		}
	}
	// The full 744-hour range covers 32 local dates, not 31.
	if days, _ := localDays(at("Europe/Zurich", 2026, 3, 15, 5, 0), at("Europe/Zurich", 2026, 3, 15, 5, 0).Add(MaxRangeElapsed), zurich); len(days) != 32 {
		t.Fatalf("744h range has %d days", len(days))
	}
}

// careLog records every canonical write of a summary fixture, so the oracle
// can recompute the summary from inputs without the production SQL.
type careLog struct {
	feeds        []loggedFeed
	sleeps       []loggedSleep
	diapers      []loggedPoint
	supplements  []loggedPoint
	measurements []loggedPoint
}

type loggedFeed struct {
	id, baby    string
	start       time.Time
	session     *string
	bottleMicro []int64
	deleted     bool
}

type loggedSleep struct {
	id, baby string
	start    time.Time
	end      *time.Time
	session  *string
	deleted  bool
}

type loggedPoint struct {
	id, baby, name string
	at             time.Time
	deleted        bool
}

type summaryFixture struct {
	*fixture
	log  careLog
	baby string // the baby new records belong to
}

func newSummaryFixture(t *testing.T) *summaryFixture {
	f := newFixture(t)
	return &summaryFixture{fixture: f, baby: f.babyID}
}

func bottle(ml string) protocol.FeedComponent {
	return protocol.FeedComponent{Kind: "bottle", VolumeML: protocol.MustParseDecimal(ml), Contents: ptr("formula")}
}

func breast() protocol.FeedComponent {
	return protocol.FeedComponent{Kind: "breast", Side: ptr("left"), Seconds: ptr(uint32(300))}
}

func solids() protocol.FeedComponent {
	return protocol.FeedComponent{Kind: "solids", Foods: []string{"banana"}, Amount: ptr("some")}
}

// microML converts a decimal millilitre literal (at most six fractional
// digits) to millionths with plain string arithmetic.
func microML(t *testing.T, ml string) int64 {
	t.Helper()
	whole, fraction, _ := strings.Cut(ml, ".")
	fraction += strings.Repeat("0", 6-len(fraction))
	var value int64
	for _, digit := range whole + fraction {
		if digit < '0' || digit > '9' {
			t.Fatalf("bad ml %q", ml)
		}
		value = value*10 + int64(digit-'0')
	}
	return value
}

func (s *summaryFixture) feed(t *testing.T, start time.Time, end *time.Time, session *string, parts ...protocol.FeedComponent) string {
	t.Helper()
	m := testkit.MustMutation(t, "feed-bottle.json")
	m.EntityID = s.id(0x20000000)
	p := m.Payload.(protocol.FeedPayload)
	p.BabyID, p.StartedAt, p.FeedingSessionID, p.Kind = s.baby, ts(start), session, parts[0].Kind
	p.EndedAt = nil
	if end != nil {
		p.EndedAt = ptr(ts(*end))
	}
	logged := loggedFeed{id: m.EntityID, baby: s.baby, start: start, session: session}
	p.Components = nil
	for _, part := range parts {
		part.ID = s.id(0x31000000)
		p.Components = append(p.Components, part)
		if part.Kind == "bottle" {
			logged.bottleMicro = append(logged.bottleMicro, microML(t, part.VolumeML.String()))
		}
	}
	m.Payload = p
	s.apply(t, m)
	s.log.feeds = append(s.log.feeds, logged)
	return m.EntityID
}

func (s *summaryFixture) sleepAt(t *testing.T, start time.Time, end *time.Time, session *string) string {
	t.Helper()
	m := testkit.MustMutation(t, "sleep.json")
	m.EntityID = s.id(0x30000000)
	p := m.Payload.(protocol.SleepPayload)
	p.BabyID, p.StartedAt, p.SleepSessionID, p.EndedAt = s.baby, ts(start), session, nil
	if end != nil {
		p.EndedAt = ptr(ts(*end))
	}
	m.Payload = p
	s.apply(t, m)
	s.log.sleeps = append(s.log.sleeps, loggedSleep{id: m.EntityID, baby: s.baby, start: start, end: end, session: session})
	return m.EntityID
}

func (s *summaryFixture) diaperAt(t *testing.T, at time.Time) string {
	t.Helper()
	m := testkit.MustMutation(t, "diaper.json")
	m.EntityID = s.id(0x40000000)
	p := m.Payload.(protocol.DiaperPayload)
	p.BabyID, p.StartedAt = s.baby, ts(at)
	m.Payload = p
	s.apply(t, m)
	s.log.diapers = append(s.log.diapers, loggedPoint{id: m.EntityID, baby: s.baby, at: at})
	return m.EntityID
}

func (s *summaryFixture) dose(t *testing.T, at time.Time, name string) string {
	t.Helper()
	m := testkit.MustMutation(t, "supplement.json")
	m.EntityID = s.id(0x60000000)
	p := m.Payload.(protocol.SupplementPayload)
	p.BabyID, p.StartedAt, p.LoggedAt, p.Name = s.baby, ts(at), ts(at.Add(time.Minute)), name
	m.Payload = p
	s.apply(t, m)
	s.log.supplements = append(s.log.supplements, loggedPoint{id: m.EntityID, baby: s.baby, at: at, name: name})
	return m.EntityID
}

func (s *summaryFixture) measure(t *testing.T, at time.Time, weight, length, head *string) string {
	t.Helper()
	m := testkit.MustMutation(t, "measurement.json")
	m.EntityID = s.id(0x70000000)
	p := m.Payload.(protocol.MeasurementPayload)
	p.BabyID, p.MeasuredAt, p.LoggedAt = s.baby, ts(at), ts(at.Add(time.Minute))
	p.WeightKG, p.LengthCM, p.HeadCircumferenceCM, p.LengthPosition = nil, nil, nil, nil
	if weight != nil {
		p.WeightKG = protocol.MustParseDecimal(*weight)
	}
	if length != nil {
		p.LengthCM, p.LengthPosition = protocol.MustParseDecimal(*length), ptr("lying")
	}
	if head != nil {
		p.HeadCircumferenceCM = protocol.MustParseDecimal(*head)
	}
	m.Payload = p
	s.apply(t, m)
	s.log.measurements = append(s.log.measurements, loggedPoint{id: m.EntityID, baby: s.baby, at: at})
	return m.EntityID
}

// drop deletes a record through the sync path and marks it deleted in the log.
func (s *summaryFixture) drop(t *testing.T, kind, id string) {
	t.Helper()
	s.remove(t, kind, id, 1)
	mark := func(points []loggedPoint) {
		for i := range points {
			if points[i].id == id {
				points[i].deleted = true
			}
		}
	}
	for i := range s.log.feeds {
		if s.log.feeds[i].id == id {
			s.log.feeds[i].deleted = true
		}
	}
	for i := range s.log.sleeps {
		if s.log.sleeps[i].id == id {
			s.log.sleeps[i].deleted = true
		}
	}
	mark(s.log.diapers)
	mark(s.log.supplements)
	mark(s.log.measurements)
}

func (s *summaryFixture) summary(t *testing.T, start, end time.Time, zone string) SummaryResult {
	t.Helper()
	calls := s.guard.calls
	result, err := s.service.Summary(t.Context(), s.credential, SummaryQuery{BabyID: s.babyID, Start: start, End: end, Timezone: zone})
	if err != nil {
		t.Fatalf("summary [%s,%s) %s: %v", start, end, zone, err)
	}
	if s.guard.calls != calls+1 {
		t.Fatalf("summary used %d guarded reads", s.guard.calls-calls)
	}
	return result
}

type oracleResult struct {
	totals       SummaryCounts
	days         []SummaryDay
	byName       []SupplementDoses
	measurements []string
}

// oracle recomputes a summary for babyID from the care log alone: day
// boundaries by minute stepping, sleep time by testing each minute against
// every completed sleep, and sessions by grouping all live members.
func oracle(log careLog, babyID string, start, end time.Time, loc *time.Location) oracleResult {
	days := oracleDays(start, end, loc)
	in := func(at time.Time) bool { return !at.Before(start) && at.Before(end) }
	dayOf := func(at time.Time) *SummaryCounts {
		for i := range days {
			if !at.Before(days[i].Start) && at.Before(days[i].End) {
				return &days[i].SummaryCounts
			}
		}
		panic("instant outside the range")
	}
	addVolume := func(counts *SummaryCounts, micro int64) {
		counts.Feed.BottleVolumeObservations++
		if counts.Feed.KnownBottleVolumeML == nil {
			counts.Feed.KnownBottleVolumeML = new(Decimal6)
		}
		*counts.Feed.KnownBottleVolumeML += Decimal6(micro)
	}
	key := func(session *string, id string) string {
		if session != nil {
			return *session
		}
		return id
	}
	feedFirst := map[string]time.Time{}
	for _, feed := range log.feeds {
		if feed.deleted || feed.baby != babyID {
			continue
		}
		k := key(feed.session, feed.id)
		if first, ok := feedFirst[k]; !ok || feed.start.Before(first) {
			feedFirst[k] = feed.start
		}
		if in(feed.start) {
			counts := dayOf(feed.start)
			counts.Feed.LoggedFeeds++
			for _, micro := range feed.bottleMicro {
				addVolume(counts, micro)
			}
		}
	}
	for _, first := range feedFirst {
		if in(first) {
			dayOf(first).Feed.Sessions++
		}
	}
	sleepFirst, sleepCompleted := map[string]time.Time{}, map[string]bool{}
	var completed []loggedSleep
	for _, sleep := range log.sleeps {
		if sleep.deleted || sleep.baby != babyID {
			continue
		}
		k := key(sleep.session, sleep.id)
		if first, ok := sleepFirst[k]; !ok || sleep.start.Before(first) {
			sleepFirst[k] = sleep.start
		}
		if sleep.end != nil {
			sleepCompleted[k] = true
			completed = append(completed, sleep)
		} else if in(sleep.start) {
			dayOf(sleep.start).Sleep.UnfinishedSleeps++
		}
	}
	for k, first := range sleepFirst {
		if in(first) && sleepCompleted[k] {
			dayOf(first).Sleep.Sessions++
		}
	}
	for minute := start; minute.Before(end); minute = minute.Add(time.Minute) {
		for _, sleep := range completed {
			if !minute.Before(sleep.start) && minute.Before(*sleep.end) {
				dayOf(minute).Sleep.CompletedSeconds += Decimal6(60_000_000)
				break
			}
		}
	}
	doses := map[string]int{}
	for _, point := range log.diapers {
		if !point.deleted && point.baby == babyID && in(point.at) {
			dayOf(point.at).Diapers++
		}
	}
	for _, point := range log.supplements {
		if !point.deleted && point.baby == babyID && in(point.at) {
			dayOf(point.at).SupplementDoses++
			doses[point.name]++
		}
	}
	var measured []loggedPoint
	for _, point := range log.measurements {
		if !point.deleted && point.baby == babyID && in(point.at) {
			dayOf(point.at).MeasurementObservations++
			measured = append(measured, point)
		}
	}
	result := oracleResult{days: days, byName: []SupplementDoses{}, measurements: []string{}}
	for _, day := range days {
		t := &result.totals
		t.Feed.Sessions += day.Feed.Sessions
		t.Feed.LoggedFeeds += day.Feed.LoggedFeeds
		t.Feed.BottlesMissingVolume += day.Feed.BottlesMissingVolume
		t.Feed.BottleVolumeObservations += day.Feed.BottleVolumeObservations
		if day.Feed.KnownBottleVolumeML != nil {
			if t.Feed.KnownBottleVolumeML == nil {
				t.Feed.KnownBottleVolumeML = new(Decimal6)
			}
			*t.Feed.KnownBottleVolumeML += *day.Feed.KnownBottleVolumeML
		}
		t.Sleep.Sessions += day.Sleep.Sessions
		t.Sleep.CompletedSeconds += day.Sleep.CompletedSeconds
		t.Sleep.UnfinishedSleeps += day.Sleep.UnfinishedSleeps
		t.Diapers += day.Diapers
		t.SupplementDoses += day.SupplementDoses
		t.MeasurementObservations += day.MeasurementObservations
	}
	for name, count := range doses {
		result.byName = append(result.byName, SupplementDoses{Name: name, Doses: count})
	}
	sort.Slice(result.byName, func(i, j int) bool { return result.byName[i].Name < result.byName[j].Name })
	sort.Slice(measured, func(i, j int) bool {
		if !measured[i].at.Equal(measured[j].at) {
			return measured[i].at.Before(measured[j].at)
		}
		return measured[i].id < measured[j].id
	})
	for _, point := range measured {
		result.measurements = append(result.measurements, point.id)
	}
	return result
}

func jsonText(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// assertMatchesOracle compares the service's summary with the independently
// computed one, through the JSON encoding the transport will expose.
func assertMatchesOracle(t *testing.T, got SummaryResult, want oracleResult) {
	t.Helper()
	if g, w := jsonText(t, got.Days), jsonText(t, want.days); g != w {
		t.Errorf("days differ from the oracle:\n got %s\nwant %s", g, w)
	}
	if g, w := jsonText(t, got.Totals), jsonText(t, want.totals); g != w {
		t.Errorf("totals differ from the oracle:\n got %s\nwant %s", g, w)
	}
	if g, w := jsonText(t, got.SupplementsByName), jsonText(t, want.byName); g != w {
		t.Errorf("supplements differ: got %s want %s", g, w)
	}
	ids := []string{}
	for _, observation := range got.Measurements {
		ids = append(ids, observation.ID)
	}
	if !reflect.DeepEqual(ids, want.measurements) {
		t.Errorf("measurements %v, want %v", ids, want.measurements)
	}
	var elapsed Decimal6
	for _, day := range got.Days {
		elapsed += day.ElapsedSeconds
	}
	if elapsed != got.ElapsedSeconds || got.ElapsedSeconds != Decimal6(got.End.Sub(got.Start).Microseconds()) {
		t.Errorf("elapsed %s, days sum %s, range %s", got.ElapsedSeconds, elapsed, got.End.Sub(got.Start))
	}
}

func dec(value Decimal6) *Decimal6 { return &value }

const hour = Decimal6(3_600_000_000)

func TestSummaryCountsSessionsVolumesAndSleepByLocalDay(t *testing.T) {
	s := newSummaryFixture(t)
	loc := mustZone(t, "Europe/Zurich")
	lz := func(day, h, m int) time.Time { return time.Date(2026, 9, day, h, m, 0, 0, loc).UTC() }
	S, E := lz(1, 3, 0), lz(4, 12, 0)
	opaque := s.id(0x5e000000)

	// Feeds. A two-segment feed counts as one session; its two parts are two
	// logged feeds and both bottle parts are summed once each.
	a := s.feed(t, lz(1, 8, 0), ptr(lz(1, 8, 15)), nil, bottle("120"))
	s.feed(t, lz(1, 8, 30), ptr(lz(1, 8, 40)), &a, bottle("30"), breast())
	// An opaque session crossing midnight counts on its first part's day; the
	// instantaneous 0 ml bottle is a known zero on the next day.
	s.feed(t, lz(1, 23, 50), ptr(lz(2, 0, 10)), &opaque, breast())
	s.feed(t, lz(2, 0, 20), nil, &opaque, bottle("0"))
	// A session that began before S is not counted, but its in-range part is
	// a logged feed and its volume counts.
	early := s.feed(t, lz(1, 2, 0), ptr(lz(1, 2, 20)), nil, bottle("10"))
	s.feed(t, lz(1, 3, 30), ptr(lz(1, 3, 45)), &early, bottle("50.5"))
	// Deleting a session's first part moves its start to the next live part.
	gone := s.feed(t, lz(3, 10, 0), ptr(lz(3, 10, 10)), nil, bottle("80"))
	s.feed(t, lz(3, 10, 30), ptr(lz(3, 10, 40)), &gone, bottle("90"))
	s.drop(t, "feed", gone)
	// Equal timestamps in one session count once.
	h1 := s.feed(t, lz(3, 9, 0), ptr(lz(3, 9, 5)), nil, breast())
	s.feed(t, lz(3, 9, 0), ptr(lz(3, 9, 10)), &h1, bottle("60"))
	s.feed(t, lz(3, 12, 0), nil, nil, solids())
	s.drop(t, "feed", s.feed(t, lz(3, 13, 0), ptr(lz(3, 13, 5)), nil, bottle("999")))
	s.feed(t, E, nil, nil, bottle("40"))
	s.feed(t, E.Add(-time.Minute), nil, nil, solids())

	// Sleeps.
	s.sleepAt(t, S.Add(-2*time.Hour), ptr(S.Add(time.Hour)), nil) // clipped at S, session began before S
	joined := s.sleepAt(t, lz(1, 13, 0), ptr(lz(1, 14, 0)), nil)
	s.sleepAt(t, lz(1, 13, 30), ptr(lz(1, 15, 0)), &joined)         // joined part overlaps: union is 2h
	s.sleepAt(t, lz(1, 22, 0), ptr(lz(2, 6, 0)), nil)               // split at midnight: 2h + 6h
	overlapping := s.sleepAt(t, lz(2, 5, 0), ptr(lz(2, 7, 0)), nil) // overlaps the night: adds 1h
	s.sleepAt(t, lz(2, 12, 0), ptr(lz(2, 12, 0)), nil)              // zero-length: a session, no time
	open := s.sleepAt(t, lz(3, 20, 0), nil, nil)                    // open sibling sets the session start
	s.sleepAt(t, lz(3, 20, 30), ptr(lz(3, 21, 0)), &open)
	s.sleepAt(t, lz(3, 1, 0), nil, nil) // unfinished alone: no session, no time
	s.drop(t, "sleep", s.sleepAt(t, lz(2, 14, 0), ptr(lz(2, 16, 0)), nil))
	s.sleepAt(t, E.Add(-30*time.Minute), ptr(E.Add(2*time.Hour)), nil) // clipped at E

	// Point kinds.
	s.diaperAt(t, lz(1, 9, 0))
	s.diaperAt(t, lz(2, 9, 0))
	s.drop(t, "diaper", s.diaperAt(t, lz(2, 10, 0)))
	s.diaperAt(t, lz(4, 11, 59))
	s.diaperAt(t, S.Add(-time.Minute))
	for _, day := range []int{1, 2, 3} {
		s.dose(t, lz(day, 9, 0), "Vitamin D")
	}
	s.dose(t, lz(2, 12, 0), "Iron")
	s.drop(t, "supplement", s.dose(t, lz(3, 12, 0), "Iron"))
	weightOnly := s.measure(t, lz(1, 10, 0), ptr("4.25"), nil, nil)
	lengthAndHead := s.measure(t, lz(3, 10, 0), nil, ptr("55.5"), ptr("38"))
	s.drop(t, "measurement", s.measure(t, lz(2, 10, 0), ptr("4.3"), nil, nil))

	// A twin's records in the same family never count.
	s.baby = s.createBaby(t, "10000000-0000-4000-8000-0000000000b2", "Twin", 1)
	s.feed(t, lz(2, 9, 0), nil, nil, bottle("500"))
	s.sleepAt(t, lz(2, 14, 0), ptr(lz(2, 18, 0)), nil)
	s.diaperAt(t, lz(2, 9, 0))
	s.baby = s.babyID

	// Running timers linked to counted sessions, started before S: if they
	// moved a session's earliest start, those sessions would not count. The
	// feed timer holds a finished 70 ml bottle and a running bottle of
	// unknown volume.
	feedTimer := testkit.MustMutation(t, "timer-feed.json")
	feedTimer.EntityID = s.id(0x50000000)
	tp := feedTimer.Payload.(protocol.TimerStartPayload)
	tp.BabyID, tp.StartedAt, tp.SessionID = s.babyID, ts(lz(1, 2, 30)), &opaque
	tp.CurrentSide, tp.CurrentSideStartedAt, tp.CurrentBottleStartedAt = nil, nil, ptr(ts(lz(1, 2, 40)))
	tp.CompletedComponents = []protocol.FeedComponent{bottle("70")}
	tp.CompletedComponents[0].ID = s.id(0x31000000)
	feedTimer.Payload = tp
	s.apply(t, feedTimer)
	s.startTimer(t, "timer-sleep.json", s.id(0x50000000), lz(1, 1, 0), &overlapping)

	got := s.summary(t, S, E, "Europe/Zurich")
	assertMatchesOracle(t, got, oracle(s.log, s.babyID, S, E, loc))

	// Hand-checked facts.
	if got.BabyID != s.babyID || got.Timezone != "Europe/Zurich" || !got.Start.Equal(S) || !got.End.Equal(E) ||
		got.Revision != s.h.FamilySequence(t) || !got.AsOf.Equal(s.now) || got.ElapsedSeconds != 81*hour {
		t.Fatalf("header %+v", got)
	}
	dates := []string{}
	for _, day := range got.Days {
		dates = append(dates, day.Date)
	}
	if !reflect.DeepEqual(dates, []string{"2026-09-01", "2026-09-02", "2026-09-03", "2026-09-04"}) ||
		got.Days[0].ElapsedSeconds != 21*hour || got.Days[3].ElapsedSeconds != 12*hour {
		t.Fatalf("days %+v", got.Days)
	}
	wantFeeds := []FeedTotals{
		{Sessions: 2, LoggedFeeds: 4, BottleVolumeObservations: 3, KnownBottleVolumeML: dec(200_500_000)},
		{Sessions: 0, LoggedFeeds: 1, BottleVolumeObservations: 1, KnownBottleVolumeML: dec(0)},
		{Sessions: 3, LoggedFeeds: 4, BottleVolumeObservations: 2, KnownBottleVolumeML: dec(150_000_000)},
		{Sessions: 1, LoggedFeeds: 1},
	}
	wantSleeps := []SleepTotals{
		{Sessions: 2, CompletedSeconds: 5 * hour},
		{Sessions: 2, CompletedSeconds: 7 * hour},
		{Sessions: 1, CompletedSeconds: hour / 2, UnfinishedSleeps: 2},
		{Sessions: 1, CompletedSeconds: hour / 2},
	}
	for i, day := range got.Days {
		if jsonText(t, day.Feed) != jsonText(t, wantFeeds[i]) || day.Sleep != wantSleeps[i] {
			t.Errorf("%s feed %s sleep %+v, want %s %+v", day.Date, jsonText(t, day.Feed), day.Sleep, jsonText(t, wantFeeds[i]), wantSleeps[i])
		}
	}
	if text := jsonText(t, got.Days[1].Feed); !strings.Contains(text, `"known_bottle_volume_ml":0,`) {
		t.Errorf("known zero volume encodes as %s", text)
	}
	if text := jsonText(t, got.Days[3].Feed); !strings.Contains(text, `"known_bottle_volume_ml":null`) {
		t.Errorf("no bottle observations encodes as %s", text)
	}
	totals := got.Totals
	if totals.Feed.Sessions != 6 || totals.Feed.LoggedFeeds != 10 || totals.Feed.BottleVolumeObservations != 6 ||
		totals.Feed.KnownBottleVolumeML == nil || *totals.Feed.KnownBottleVolumeML != 350_500_000 || totals.Feed.BottlesMissingVolume != 0 ||
		totals.Sleep != (SleepTotals{Sessions: 6, CompletedSeconds: 13 * hour, UnfinishedSleeps: 2}) ||
		totals.Diapers != 3 || totals.SupplementDoses != 4 || totals.MeasurementObservations != 2 {
		t.Fatalf("totals %s", jsonText(t, totals))
	}
	if !reflect.DeepEqual(got.SupplementsByName, []SupplementDoses{{"Iron", 1}, {"Vitamin D", 3}}) {
		t.Fatalf("supplements %+v", got.SupplementsByName)
	}
	measurements := jsonText(t, got.Measurements)
	want := `[{"id":"` + weightOnly + `","measured_at":"2026-09-01T08:00:00Z","day":"2026-09-01","weight_kg":4.25},` +
		`{"id":"` + lengthAndHead + `","measured_at":"2026-09-03T08:00:00Z","day":"2026-09-03","length_cm":55.5,"length_position":"lying","head_circumference_cm":38}]`
	if measurements != want {
		t.Fatalf("measurements\n got %s\nwant %s", measurements, want)
	}
	// Running timers are current facts only: present, unchanged, and outside
	// every total (the 70 ml finished timer bottle is not in the volume).
	if len(got.ActiveTimers) != 2 || got.ActiveTimers[0].TimerKind != "feed" || got.ActiveTimers[1].TimerKind != "sleep" {
		t.Fatalf("timers %+v", got.ActiveTimers)
	}
	running := got.ActiveTimers[0].Entity.Payload.(protocol.TimerStartPayload)
	if running.CurrentBottleStartedAt == nil || len(running.CompletedComponents) != 1 {
		t.Fatalf("running feed timer %+v", running)
	}
}

func TestSummaryUsesActualElapsedSecondsAcrossZurichDST(t *testing.T) {
	s := newSummaryFixture(t)
	loc := mustZone(t, "Europe/Zurich")
	lz := func(y int, mo time.Month, d, h, m int) time.Time { return time.Date(y, mo, d, h, m, 0, 0, loc).UTC() }

	// Spring forward: 2026-03-29 lasts 23 hours. One sleep covers the whole
	// day, so that day's sleep equals its actual elapsed 82800 seconds.
	spring := []time.Time{lz(2026, 3, 28, 0, 0), lz(2026, 3, 31, 0, 0)}
	s.sleepAt(t, lz(2026, 3, 28, 22, 0), ptr(lz(2026, 3, 30, 1, 0)), nil)
	s.sleepAt(t, lz(2026, 3, 29, 1, 30), ptr(lz(2026, 3, 29, 3, 30)), nil) // inside the night, across the gap
	s.feed(t, lz(2026, 3, 29, 3, 0), nil, nil, bottle("100"))              // 03:00 CEST, right after the gap
	s.diaperAt(t, lz(2026, 3, 29, 1, 59))

	// Fall back: 2025-10-26 lasts 25 hours; 02:30 happens twice.
	autumn := []time.Time{lz(2025, 10, 25, 0, 0), lz(2025, 10, 28, 0, 0)}
	twice := time.Date(2025, 10, 26, 0, 30, 0, 0, time.UTC) // 02:30 CEST
	s.feed(t, twice, nil, nil, bottle("80"))
	s.feed(t, twice.Add(time.Hour), nil, nil, bottle("90")) // 02:30 CET
	s.sleepAt(t, lz(2025, 10, 26, 1, 0), ptr(lz(2025, 10, 26, 4, 0)), nil)
	s.sleepAt(t, lz(2025, 10, 26, 22, 0), ptr(lz(2025, 10, 27, 6, 0)), nil)

	for name, window := range map[string][]time.Time{"spring": spring, "autumn": autumn} {
		t.Run(name, func(t *testing.T) {
			got := s.summary(t, window[0], window[1], "Europe/Zurich")
			assertMatchesOracle(t, got, oracle(s.log, s.babyID, window[0], window[1], loc))
			if len(got.Days) != 3 {
				t.Fatalf("days %+v", got.Days)
			}
			middle := got.Days[1]
			switch name {
			case "spring":
				if middle.Date != "2026-03-29" || middle.ElapsedSeconds != 23*hour || middle.Sleep.CompletedSeconds != 23*hour ||
					got.Days[0].Sleep.CompletedSeconds != 2*hour || got.Days[2].Sleep.CompletedSeconds != hour ||
					middle.Feed.LoggedFeeds != 1 || middle.Diapers != 1 || got.Totals.Sleep.Sessions != 2 {
					t.Fatalf("spring %s", jsonText(t, got.Days))
				}
			case "autumn":
				// 01:00 CEST to 04:00 CET is three wall-clock hours but four
				// elapsed hours.
				if middle.Date != "2025-10-26" || middle.ElapsedSeconds != 25*hour || middle.Sleep.CompletedSeconds != 6*hour ||
					got.Days[2].Sleep.CompletedSeconds != 6*hour || middle.Feed.LoggedFeeds != 2 || middle.Feed.Sessions != 2 ||
					*middle.Feed.KnownBottleVolumeML != 170_000_000 {
					t.Fatalf("autumn %s", jsonText(t, got.Days))
				}
			}
		})
	}
	// The same instants in UTC fall on different days.
	utc := s.summary(t, autumn[0], autumn[1], "UTC")
	assertMatchesOracle(t, utc, oracle(s.log, s.babyID, autumn[0], autumn[1], time.UTC))
	if utc.Timezone != "UTC" || len(utc.Days) != 4 || utc.Days[0].Date != "2025-10-24" || utc.Days[0].ElapsedSeconds != 2*hour {
		t.Fatalf("UTC days %s", jsonText(t, utc.Days))
	}
}

func TestSummaryHonoursExactMicrosecondRangeEndpoints(t *testing.T) {
	s := newSummaryFixture(t)
	us := time.Microsecond
	S := day0.Add(10*time.Hour + 500*us)
	E := S.Add(6 * time.Hour)
	for _, at := range []time.Time{S.Add(-us), S, E.Add(-us), E} {
		s.feed(t, at, nil, nil, bottle("1"))
		s.diaperAt(t, at)
		s.dose(t, at, "Vitamin D")
		s.measure(t, at, ptr("5"), nil, nil)
	}
	s.sleepAt(t, S.Add(-time.Hour), ptr(S), nil)         // ends exactly at S: no time, began before S
	s.sleepAt(t, S, ptr(S), nil)                         // zero-length at S: a session
	s.sleepAt(t, E.Add(-us), ptr(E.Add(time.Hour)), nil) // one microsecond inside the range
	s.sleepAt(t, S.Add(-us), nil, nil)                   // unfinished before S
	s.sleepAt(t, S, nil, nil)                            // unfinished at S
	s.sleepAt(t, E, ptr(E.Add(time.Hour)), nil)          // starts at E

	// Nanosecond bounds round up to the microsecond grid.
	result, err := s.service.Summary(t.Context(), s.credential, SummaryQuery{BabyID: s.babyID, Start: S.Add(-999), End: E.Add(-999), Timezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Start.Equal(S) || !result.End.Equal(E) || len(result.Days) != 1 || result.ElapsedSeconds != 6*hour {
		t.Fatalf("range [%s,%s) days %d elapsed %s", result.Start, result.End, len(result.Days), result.ElapsedSeconds)
	}
	totals := result.Totals
	if totals.Feed.LoggedFeeds != 2 || totals.Feed.Sessions != 2 || *totals.Feed.KnownBottleVolumeML != 2_000_000 ||
		totals.Diapers != 2 || totals.SupplementDoses != 2 || totals.MeasurementObservations != 2 || len(result.Measurements) != 2 ||
		totals.Sleep != (SleepTotals{Sessions: 2, CompletedSeconds: 1, UnfinishedSleeps: 1}) {
		t.Fatalf("totals %s", jsonText(t, totals))
	}
	if text := jsonText(t, totals.Sleep); !strings.Contains(text, `"completed_seconds":0.000001`) {
		t.Fatalf("sleep encodes as %s", text)
	}
}

func TestSummaryRequiresLiveBabyAndHonoursDeadline(t *testing.T) {
	f := newFixture(t)
	other, err := f.h.Family.Create(t.Context(), f.h.Outsider, f.h.CommandID(), "Other family")
	if err != nil {
		t.Fatal(err)
	}
	foreign := testkit.MustMutation(t, "baby.json")
	foreign.EntityID, foreign.MutationID = "10000000-0000-4000-8000-0000000000f0", f.h.CommandID()
	if created, err := f.h.Service.Apply(t.Context(), f.h.Refresh("outsider"), other.ID, foreign); err != nil || created.Status != "applied" {
		t.Fatalf("foreign baby %+v %v", created, err)
	}
	deleted := f.createBaby(t, "10000000-0000-4000-8000-0000000000de", "Deleted", 2)
	f.remove(t, "baby", deleted, 1)
	query := SummaryQuery{BabyID: f.babyID, Start: day0, End: day0.Add(time.Hour), Timezone: "Europe/Zurich"}
	for name, babyID := range map[string]string{"missing": "10000000-0000-4000-8000-0000000000aa", "foreign family": foreign.EntityID, "deleted child": deleted} {
		t.Run(name, func(t *testing.T) {
			q := query
			q.BabyID = babyID
			if result, err := f.service.Summary(t.Context(), f.credential, q); !errors.Is(err, ErrBabyNotFound) || !reflect.DeepEqual(result, SummaryResult{}) {
				t.Fatalf("err=%v result=%+v", err, result)
			}
		})
	}
	t.Run("unauthorized credential", func(t *testing.T) {
		bad := f.credential[:len(f.credential)-1] + "A"
		if bad == f.credential {
			bad = f.credential[:len(f.credential)-1] + "B"
		}
		for _, credential := range []string{bad, f.h.AccessToken("owner")} {
			if _, err := f.service.Summary(t.Context(), credential, query); !errors.Is(err, authority.ErrIntegrationUnauthorized) {
				t.Fatalf("summary err=%v", err)
			}
		}
	})
	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if result, err := f.service.Summary(ctx, f.credential, query); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(result, SummaryResult{}) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("internal deadline", func(t *testing.T) {
		f.service.summaryTimeout = time.Nanosecond
		defer func() { f.service.summaryTimeout = SummaryTimeout }()
		if result, err := f.service.Summary(t.Context(), f.credential, query); !errors.Is(err, context.DeadlineExceeded) || !reflect.DeepEqual(result, SummaryResult{}) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("empty range of a live baby", func(t *testing.T) {
		result, err := f.service.Summary(t.Context(), f.credential, query)
		if err != nil {
			t.Fatal(err)
		}
		if result.Days == nil || result.SupplementsByName == nil || result.Measurements == nil || result.ActiveTimers == nil ||
			result.Totals.Feed.KnownBottleVolumeML != nil || len(result.Days) != 1 {
			t.Fatalf("empty result %s", jsonText(t, result))
		}
	})
}

func TestSummaryFailsWholeInsteadOfTruncating(t *testing.T) {
	s := newSummaryFixture(t)
	S := day0
	E := S.Add(24 * time.Hour)
	s.measure(t, S.Add(time.Hour), ptr("4"), nil, nil)
	s.measure(t, S.Add(2*time.Hour), ptr("4.1"), nil, nil)
	q := SummaryQuery{BabyID: s.babyID, Start: S, End: E, Timezone: "UTC"}
	full := s.summary(t, S, E, "UTC")
	if len(full.Measurements) != 2 {
		t.Fatalf("measurements %+v", full.Measurements)
	}
	s.service.maxMeasurements = 1
	if result, err := s.service.Summary(t.Context(), s.credential, q); !errors.Is(err, ErrResultTooLarge) || !reflect.DeepEqual(result, SummaryResult{}) {
		t.Fatalf("measurement cap err=%v", err)
	}
	s.service.maxMeasurements = MaxSummaryMeasurements
	s.service.maxResultBytes = encodedSize(t, full) - 1
	if result, err := s.service.Summary(t.Context(), s.credential, q); !errors.Is(err, ErrResultTooLarge) || !reflect.DeepEqual(result, SummaryResult{}) {
		t.Fatalf("byte budget err=%v", err)
	}
	s.service.maxResultBytes = encodedSize(t, full)
	if _, err := s.service.Summary(t.Context(), s.credential, q); err != nil {
		t.Fatalf("exact budget err=%v", err)
	}
}
