package familyread

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
)

type stubGuard struct{ calls int }

func (g *stubGuard) WithIntegrationRead(context.Context, string, func(context.Context, pgx.Tx, access.Grant) error) error {
	g.calls++
	return nil
}

func TestHistoryRejectsInvalidQueriesBeforeReading(t *testing.T) {
	start := day0
	valid := HistoryQuery{BabyID: fixtureBab, Start: start, End: start.Add(24 * time.Hour)}
	with := func(change func(*HistoryQuery)) HistoryQuery {
		q := valid
		change(&q)
		return q
	}
	for name, test := range map[string]struct {
		query HistoryQuery
		want  error
	}{
		"zero start":     {with(func(q *HistoryQuery) { q.Start = time.Time{} }), ErrInvalidRange},
		"zero end":       {with(func(q *HistoryQuery) { q.End = time.Time{} }), ErrInvalidRange},
		"empty range":    {with(func(q *HistoryQuery) { q.End = q.Start }), ErrInvalidRange},
		"reversed range": {with(func(q *HistoryQuery) { q.End = q.Start.Add(-time.Hour) }), ErrInvalidRange},
		"before 1900": {with(func(q *HistoryQuery) {
			q.Start = time.Date(1899, 12, 31, 0, 0, 0, 0, time.UTC)
			q.End = q.Start.Add(time.Hour)
		}), ErrInvalidRange},
		"after 2200": {with(func(q *HistoryQuery) {
			q.Start = time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
			q.End = q.Start.Add(time.Hour)
		}), ErrInvalidRange},
		"744h plus a nanosecond": {with(func(q *HistoryQuery) { q.End = q.Start.Add(MaxRangeElapsed + time.Nanosecond) }), ErrRangeTooLong},
		"32 days":                {with(func(q *HistoryQuery) { q.End = q.Start.Add(32 * 24 * time.Hour) }), ErrRangeTooLong},
		"negative limit":         {with(func(q *HistoryQuery) { q.Limit = -1 }), ErrInvalidLimit},
		"limit above max":        {with(func(q *HistoryQuery) { q.Limit = MaxHistoryLimit + 1 }), ErrInvalidLimit},
		"timer kind":             {with(func(q *HistoryQuery) { q.Kinds = []string{"timer"} }), ErrInvalidKind},
		"baby kind":              {with(func(q *HistoryQuery) { q.Kinds = []string{"feed", "baby"} }), ErrInvalidKind},
		"growth profile kind":    {with(func(q *HistoryQuery) { q.Kinds = []string{"growth_profile"} }), ErrInvalidKind},
		"case-folded kind":       {with(func(q *HistoryQuery) { q.Kinds = []string{"Feed"} }), ErrInvalidKind},
		"malformed baby":         {with(func(q *HistoryQuery) { q.BabyID = "not-a-uuid" }), ErrInvalidBabyID},
		"uppercase baby":         {with(func(q *HistoryQuery) { q.BabyID = strings.ToUpper("abcdef00-0000-4000-8000-000000000001") }), ErrInvalidBabyID},
		"empty baby":             {with(func(q *HistoryQuery) { q.BabyID = "" }), ErrInvalidBabyID},
	} {
		t.Run(name, func(t *testing.T) {
			guard := &stubGuard{}
			service := New(guard, fixedKey(1), func() time.Time { return day0 })
			result, err := service.History(t.Context(), "credential", test.query)
			if !errors.Is(err, test.want) || Code(err) != test.want.Error() || guard.calls != 0 || !reflect.DeepEqual(result, HistoryResult{}) {
				t.Fatalf("err=%v code=%q calls=%d result=%+v, want %v before any read", err, Code(err), guard.calls, result, test.want)
			}
			if _, err := service.HistoryWithBudget(t.Context(), "credential", test.query, func(HistoryResult) (bool, error) { return true, nil }); !errors.Is(err, test.want) || guard.calls != 0 {
				t.Fatalf("budgeted err=%v calls=%d", err, guard.calls)
			}
		})
	}
	for name, query := range map[string]HistoryQuery{
		"exactly 744 hours": with(func(q *HistoryQuery) { q.End = q.Start.Add(MaxRangeElapsed) }),
		"limit 100":         with(func(q *HistoryQuery) { q.Limit = MaxHistoryLimit }),
		"duplicate kinds":   with(func(q *HistoryQuery) { q.Kinds = []string{"sleep", "feed", "sleep"} }),
		"non-UTC instants": with(func(q *HistoryQuery) {
			zone := time.FixedZone("x", 3600)
			q.Start, q.End = q.Start.In(zone), q.End.In(zone)
		}),
	} {
		t.Run(name, func(t *testing.T) {
			guard := &stubGuard{}
			if _, err := New(guard, fixedKey(1), func() time.Time { return day0 }).History(t.Context(), "credential", query); err != nil || guard.calls != 1 {
				t.Fatalf("err=%v calls=%d, want one guarded read", err, guard.calls)
			}
		})
	}
	for name, service := range map[string]*Service{
		"nil guard": New(nil, fixedKey(1), time.Now),
		"nil clock": New(&stubGuard{}, fixedKey(1), nil),
		"zero key":  New(&stubGuard{}, [32]byte{}, time.Now),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := service.History(t.Context(), "credential", valid); !errors.Is(err, authority.ErrUnavailable) {
				t.Fatalf("history err=%v", err)
			}
			if _, err := service.ListBabies(t.Context(), "credential"); !errors.Is(err, authority.ErrUnavailable) {
				t.Fatalf("babies err=%v", err)
			}
		})
	}
}

// Seeds 101 entries with simultaneous timestamps across and within kinds and
// pages through them with the default and maximum limits.
func TestHistoryPagesInStableOrderWithDefaultAndMaximumLimits(t *testing.T) {
	f := newFixture(t)
	type key struct {
		start    time.Time
		kind, id string
	}
	var seeded []key
	for i := 0; i < 101; i++ {
		start := day0.Add(time.Duration(i/4) * 7 * time.Minute)
		kind := HistoryKinds()[(i/2)%len(historyKinds)]
		// IDs descend as i grows, so insertion order never decides ties.
		id := fmt.Sprintf("a0000000-0000-4000-8000-%012d", 1000-i)
		f.record(t, kind, id, start)
		seeded = append(seeded, key{start, kind, id})
	}
	sort.Slice(seeded, func(i, j int) bool {
		a, b := seeded[i], seeded[j]
		if !a.start.Equal(b.start) {
			return a.start.Before(b.start)
		}
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		return a.id < b.id
	})
	want := make([]string, len(seeded))
	for i, k := range seeded {
		want[i] = k.kind + "/" + k.id
	}
	revision := f.h.FamilySequence(t)
	query := HistoryQuery{BabyID: f.babyID, Start: day0.Add(-time.Hour), End: day0.Add(3 * 24 * time.Hour)}

	readAll := func(limit int, sizes []int) []string {
		t.Helper()
		var got []string
		q := query
		q.Limit = limit
		for page := 0; ; page++ {
			result := f.history(t, q)
			if result.Revision != revision || !result.AsOf.Equal(f.now) {
				t.Fatalf("page %d revision=%d asOf=%s, want %d %s", page, result.Revision, result.AsOf, revision, f.now)
			}
			if page >= len(sizes) || len(result.Entries) != sizes[page] {
				t.Fatalf("limit %d page %d has %d entries, want sizes %v", limit, page, len(result.Entries), sizes)
			}
			for _, entry := range result.Entries {
				if entry.Entity.EntityKind != entry.Kind || entry.Entity.Meta.ID != entry.ID || entry.Entity.Meta.TombstonedAt != nil || entry.Entity.Payload == nil {
					t.Fatalf("entry %s/%s carries entity %+v", entry.Kind, entry.ID, entry.Entity)
				}
			}
			got = append(got, entryKeys(result.Entries)...)
			if result.NextCursor == "" {
				if page != len(sizes)-1 {
					t.Fatalf("limit %d stopped after page %d", limit, page)
				}
				return got
			}
			q.Cursor = result.NextCursor
		}
	}
	if got := readAll(0, []int{50, 50, 1}); !reflect.DeepEqual(got, want) {
		t.Fatalf("default pages:\n got %v\nwant %v", got, want)
	}
	if got := readAll(MaxHistoryLimit, []int{100, 1}); !reflect.DeepEqual(got, want) {
		t.Fatalf("maximum pages:\n got %v\nwant %v", got, want)
	}
	// Equal timestamps: start ties order by kind, then same-kind ties by ID.
	if want[0] != "diaper/a0000000-0000-4000-8000-000000000999" || want[1] != "diaper/a0000000-0000-4000-8000-000000001000" || want[2] != "feed/a0000000-0000-4000-8000-000000000997" {
		t.Fatalf("fixture tie order %v", want[:4])
	}
}

func TestHistoryContinuationRejectsAnyFamilyChange(t *testing.T) {
	f := newFixture(t)
	otherBaby := f.createBaby(t, "10000000-0000-4000-8000-000000000002", "Other", 1)
	var diapers []string
	for i := 0; i < 6; i++ {
		diapers = append(diapers, f.diaper(t, f.id(0xd0000000), day0.Add(time.Duration(i)*time.Hour), nil))
	}
	query := HistoryQuery{BabyID: f.babyID, Start: day0, End: day0.Add(24 * time.Hour), Limit: 2}
	firstPage := func(t *testing.T) HistoryResult {
		page := f.history(t, query)
		if len(page.Entries) != 2 || page.NextCursor == "" || page.Revision != f.h.FamilySequence(t) {
			t.Fatalf("first page %+v", page)
		}
		return page
	}
	for _, test := range []struct {
		name   string
		change func(t *testing.T)
	}{
		{"edit", func(t *testing.T) {
			m := testkit.MustMutation(t, "diaper.json")
			m.EntityID, m.Operation, m.ObservedEntityVersion = diapers[3], "update", 1
			p := m.Payload.(protocol.DiaperPayload)
			p.BabyID, p.StartedAt, p.Note = f.babyID, ts(day0.Add(3*time.Hour)), ptr("changed")
			m.Payload = p
			f.apply(t, m)
		}},
		{"delete", func(t *testing.T) { f.remove(t, "diaper", diapers[4], 1) }},
		{"insert", func(t *testing.T) { f.diaper(t, f.id(0xd0000000), day0.Add(90*time.Minute), nil) }},
		{"unrelated baby", func(t *testing.T) {
			m := testkit.MustMutation(t, "baby.json")
			m.EntityID, m.Operation, m.ObservedEntityVersion = otherBaby, "update", 1
			p := m.Payload.(protocol.BabyPayload)
			p.Name, p.SortOrder = "Renamed", 1
			m.Payload = p
			f.apply(t, m)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			page := firstPage(t)
			test.change(t)
			next := query
			next.Cursor = page.NextCursor
			result, err := f.service.History(t.Context(), f.credential, next)
			if !errors.Is(err, ErrHistoryChanged) || Code(err) != "history_changed" || !reflect.DeepEqual(result, HistoryResult{}) {
				t.Fatalf("continuation after %s: err=%v result=%+v", test.name, err, result)
			}
			restarted := firstPage(t)
			if restarted.Revision == page.Revision {
				t.Fatalf("restart kept revision %d", page.Revision)
			}
			next.Cursor = restarted.NextCursor
			if continued := f.history(t, next); continued.Revision != restarted.Revision || len(continued.Entries) != 2 {
				t.Fatalf("unchanged continuation %+v", continued)
			}
		})
	}
}

func TestHistoryCursorIsAuthenticatedAndBound(t *testing.T) {
	f := newFixture(t)
	secondBaby := f.createBaby(t, "10000000-0000-4000-8000-000000000002", "Second", 1)
	for i := 0; i < 3; i++ {
		f.diaper(t, f.id(0xd0000000), day0.Add(time.Duration(i)*time.Hour), nil)
	}
	query := HistoryQuery{BabyID: f.babyID, Start: day0, End: day0.Add(24 * time.Hour), Kinds: []string{"diaper", "feed"}, Limit: 1}
	page := f.history(t, query)
	if page.NextCursor == "" {
		t.Fatal("no cursor")
	}
	continuation := query
	continuation.Cursor = page.NextCursor
	expect := func(t *testing.T, service *Service, credential string, q HistoryQuery, want error) {
		t.Helper()
		result, err := service.History(t.Context(), credential, q)
		if !errors.Is(err, want) || !reflect.DeepEqual(result, HistoryResult{}) {
			t.Fatalf("err=%v result=%+v, want %v", err, result, want)
		}
	}
	with := func(change func(*HistoryQuery)) HistoryQuery {
		q := continuation
		change(&q)
		return q
	}
	t.Run("other key", func(t *testing.T) {
		expect(t, New(f.guard, fixedKey(0x43), func() time.Time { return f.now }), f.credential, continuation, ErrInvalidCursor)
	})
	t.Run("tampered", func(t *testing.T) {
		raw := []byte(page.NextCursor)
		for _, index := range []int{0, len(raw) / 2, len(raw) - 1} {
			changed := append([]byte(nil), raw...)
			if changed[index] == 'A' {
				changed[index] = 'B'
			} else {
				changed[index] = 'A'
			}
			expect(t, f.service, f.credential, with(func(q *HistoryQuery) { q.Cursor = string(changed) }), ErrInvalidCursor)
		}
		for _, garbage := range []string{"x", ".", "a.b", strings.Repeat("A", 4096), page.NextCursor + "."} {
			expect(t, f.service, f.credential, with(func(q *HistoryQuery) { q.Cursor = garbage }), ErrInvalidCursor)
		}
	})
	t.Run("other baby", func(t *testing.T) {
		expect(t, f.service, f.credential, with(func(q *HistoryQuery) { q.BabyID = secondBaby }), ErrInvalidCursor)
	})
	t.Run("other range", func(t *testing.T) {
		expect(t, f.service, f.credential, with(func(q *HistoryQuery) { q.Start = q.Start.Add(time.Microsecond) }), ErrInvalidCursor)
		expect(t, f.service, f.credential, with(func(q *HistoryQuery) { q.End = q.End.Add(-time.Microsecond) }), ErrInvalidCursor)
	})
	t.Run("other kinds", func(t *testing.T) {
		expect(t, f.service, f.credential, with(func(q *HistoryQuery) { q.Kinds = []string{"diaper"} }), ErrInvalidCursor)
		expect(t, f.service, f.credential, with(func(q *HistoryQuery) { q.Kinds = nil }), ErrInvalidCursor)
	})
	t.Run("other grant", func(t *testing.T) {
		expect(t, f.service, f.issue(t, grantB), continuation, ErrInvalidCursor)
	})
	t.Run("expiry", func(t *testing.T) {
		issued := f.now
		defer func() { f.now = issued }()
		f.now = issued.Add(CursorTTL)
		expect(t, f.service, f.credential, continuation, ErrCursorExpired)
		f.now = issued.Add(CursorTTL - time.Microsecond)
		if result := f.history(t, continuation); len(result.Entries) != 1 {
			t.Fatalf("cursor before expiry: %+v", result)
		}
	})
	t.Run("same filters in another order and another limit", func(t *testing.T) {
		q := with(func(q *HistoryQuery) { q.Kinds = []string{"feed", "diaper", "feed"}; q.Limit = 5 })
		if result := f.history(t, q); len(result.Entries) != 2 || result.NextCursor != "" {
			t.Fatalf("equivalent continuation %+v", result)
		}
	})
}

func TestHistoryRequiresLiveBabyInAuthorizedFamily(t *testing.T) {
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
	query := HistoryQuery{Start: day0, End: day0.Add(time.Hour)}
	for name, babyID := range map[string]string{"missing": "10000000-0000-4000-8000-0000000000aa", "foreign family": foreign.EntityID, "deleted": deleted} {
		t.Run(name, func(t *testing.T) {
			q := query
			q.BabyID = babyID
			if result, err := f.service.History(t.Context(), f.credential, q); !errors.Is(err, ErrBabyNotFound) || !reflect.DeepEqual(result, HistoryResult{}) {
				t.Fatalf("err=%v result=%+v", err, result)
			}
		})
	}
	t.Run("unauthorized credential", func(t *testing.T) {
		q := query
		q.BabyID = f.babyID
		bad := f.credential[:len(f.credential)-1] + "A"
		if bad == f.credential {
			bad = f.credential[:len(f.credential)-1] + "B"
		}
		if _, err := f.service.History(t.Context(), bad, q); !errors.Is(err, authority.ErrIntegrationUnauthorized) {
			t.Fatalf("history err=%v", err)
		}
		if _, err := f.service.ListBabies(t.Context(), f.h.AccessToken("owner")); !errors.Is(err, authority.ErrIntegrationUnauthorized) {
			t.Fatalf("babies err=%v", err)
		}
	})
}

func TestHistorySelectsDocumentedPredicates(t *testing.T) {
	f := newFixture(t)
	S := day0.Add(10 * time.Hour)
	E := S.Add(24 * time.Hour)
	us := time.Microsecond
	id := func(prefix uint32) string { return f.id(prefix) }
	in := map[string]string{} // label -> kind/id for included records
	add := func(label, kind, id string) { in[label] = kind + "/" + id }

	add("F1 start=S", "feed", f.feed(t, id(0xf0000000), S, ptr(S.Add(10*time.Minute)), nil))
	add("F2 start=E-1us", "feed", f.feed(t, id(0xf0000000), E.Add(-us), nil, nil))
	f.feed(t, id(0xf0000000), E, nil, nil)                                 // starts at E
	f.feed(t, id(0xf0000000), S.Add(-us), ptr(S.Add(30*time.Minute)), nil) // starts before S
	add("F5 null end", "feed", f.feed(t, id(0xf0000000), S.Add(time.Hour), nil, nil))

	add("S1 spans S", "sleep", f.sleep(t, id(0x50000000), S.Add(-2*time.Hour), ptr(S.Add(time.Hour)), nil))
	f.sleep(t, id(0x50000000), S.Add(-2*time.Hour), ptr(S), nil) // ends exactly at S
	add("S3 zero at S", "sleep", f.sleep(t, id(0x50000000), S, ptr(S), nil))
	f.sleep(t, id(0x50000000), S.Add(-us), ptr(S.Add(-us)), nil) // zero-length before S
	f.sleep(t, id(0x50000000), E, ptr(E), nil)                   // zero-length at E
	add("S6 unfinished in range", "sleep", f.sleep(t, id(0x50000000), S.Add(time.Hour), nil, nil))
	f.sleep(t, id(0x50000000), S.Add(-time.Hour), nil, nil) // unfinished before S
	add("S8 spans E", "sleep", f.sleep(t, id(0x50000000), E.Add(-time.Hour), ptr(E.Add(time.Hour)), nil))
	f.sleep(t, id(0x50000000), E, ptr(E.Add(time.Hour)), nil) // starts at E
	add("S10 seven-day sleep spanning S", "sleep", f.sleep(t, id(0x50000000), S.Add(-7*24*time.Hour+time.Hour), ptr(S.Add(time.Hour)), nil))

	add("D1 at S", "diaper", f.diaper(t, id(0xd0000000), S, nil))
	f.diaper(t, id(0xd0000000), E, nil)
	gone := f.diaper(t, id(0xd0000000), S.Add(3*time.Hour), nil)
	f.remove(t, "diaper", gone, 1)
	add("P1 at E-1us", "supplement", f.supplement(t, id(0xe0000000), E.Add(-us)))
	f.supplement(t, id(0xe0000000), S.Add(-us))
	add("M1 in range", "measurement", f.measurement(t, id(0x70000000), S.Add(2*time.Hour)))
	f.measurement(t, id(0x70000000), E)
	growth := testkit.MustMutation(t, "growth-profile.json")
	f.apply(t, growth)
	f.startTimer(t, "timer-feed.json", id(0x5f000000), S.Add(time.Hour), nil)
	f.startTimer(t, "timer-sleep.json", id(0x5e000000), S.Add(2*time.Hour), nil)

	result := f.history(t, HistoryQuery{BabyID: f.babyID, Start: S, End: E})
	got := map[string]HistoryEntry{}
	for _, entry := range result.Entries {
		got[entry.Kind+"/"+entry.ID] = entry
	}
	if len(got) != len(in) || len(result.Entries) != len(in) {
		t.Fatalf("selected %v\nwant %v", entryKeys(result.Entries), in)
	}
	for label, key := range in {
		if _, ok := got[key]; !ok {
			t.Errorf("%s (%s) missing", label, key)
		}
	}
	for i := 1; i < len(result.Entries); i++ {
		a, b := result.Entries[i-1], result.Entries[i]
		if a.Start.After(b.Start) || (a.Start.Equal(b.Start) && (a.Kind > b.Kind || (a.Kind == b.Kind && a.ID >= b.ID))) {
			t.Fatalf("order broken at %d: %v", i, entryKeys(result.Entries))
		}
	}
	for label, key := range in {
		entry := got[key]
		switch label {
		case "S6 unfinished in range":
			if !entry.Unfinished || entry.End != nil {
				t.Errorf("%s: unfinished=%t end=%v", label, entry.Unfinished, entry.End)
			}
		case "F5 null end":
			if entry.Unfinished || entry.End != nil || entry.Session == nil {
				t.Errorf("%s: logged feed without end must be a completed record: %+v", label, entry)
			}
		case "S1 spans S":
			if !entry.Start.Equal(S.Add(-2*time.Hour)) || entry.End == nil || !entry.End.Equal(S.Add(time.Hour)) || entry.Unfinished {
				t.Errorf("%s: raw interval not preserved: %+v", label, entry)
			}
		case "M1 in range":
			payload := entry.Entity.Payload.(protocol.MeasurementPayload)
			if payload.HeadCircumferenceCM != nil || payload.WeightKG == nil || entry.End != nil || entry.Session != nil {
				t.Errorf("%s: %+v", label, entry)
			}
		default:
			if entry.Unfinished {
				t.Errorf("%s flagged unfinished", label)
			}
		}
	}
	if len(result.ActiveTimers) != 2 || result.ActiveTimers[0].TimerKind != "feed" || result.ActiveTimers[1].TimerKind != "sleep" {
		t.Fatalf("active timers %+v", result.ActiveTimers)
	}
	for _, timer := range result.ActiveTimers {
		if timer.Entity.EntityKind != "timer" || timer.Entity.Meta.ID != timer.ID || timer.Entity.Payload == nil {
			t.Fatalf("timer entity %+v", timer)
		}
	}
	// Timers are current facts, independent of the requested range.
	past := f.history(t, HistoryQuery{BabyID: f.babyID, Start: day0.Add(-10 * 24 * time.Hour), End: day0.Add(-9 * 24 * time.Hour)})
	if len(past.Entries) != 0 || len(past.ActiveTimers) != 2 || !past.AsOf.Equal(f.now) {
		t.Fatalf("past range %+v", past)
	}
	only := f.history(t, HistoryQuery{BabyID: f.babyID, Start: S, End: E, Kinds: []string{"sleep", "measurement"}})
	for _, entry := range only.Entries {
		if entry.Kind != "sleep" && entry.Kind != "measurement" {
			t.Fatalf("kind filter leaked %s", entry.Kind)
		}
	}
	if len(only.Entries) != 6 {
		t.Fatalf("filtered entries %v", entryKeys(only.Entries))
	}
}

func TestHistorySessionMetadataUsesEarliestLiveMember(t *testing.T) {
	f := newFixture(t)
	S := day0.Add(10 * time.Hour)
	E := S.Add(12 * time.Hour)
	a1 := f.feed(t, f.id(0xf1000000), S.Add(-time.Hour), ptr(S.Add(-50*time.Minute)), nil)
	a2 := f.feed(t, f.id(0xf2000000), S.Add(time.Hour), ptr(S.Add(70*time.Minute)), &a1)
	a3 := f.feed(t, f.id(0xf3000000), S.Add(2*time.Hour), nil, nil)
	opaque := "0e000000-0000-4000-8000-000000000001"
	b1 := f.feed(t, f.id(0xf4000000), S.Add(3*time.Hour), nil, &opaque)
	b2 := f.feed(t, f.id(0xf5000000), S.Add(4*time.Hour), nil, &opaque)
	tie := "0e000000-0000-4000-8000-000000000002"
	c1 := f.feed(t, f.id(0xf6000000), S.Add(5*time.Hour), nil, &tie)
	c2 := f.feed(t, f.id(0xf7000000), S.Add(5*time.Hour), nil, &tie)
	open := f.sleep(t, f.id(0x51000000), S.Add(-30*time.Minute), nil, nil)
	joined := f.sleep(t, f.id(0x52000000), S.Add(6*time.Hour), ptr(S.Add(7*time.Hour)), &open)
	// A running timer in the same session never sets the earliest start.
	f.startTimer(t, "timer-feed.json", f.id(0x5f000000), S.Add(-2*time.Hour), &a1)

	sessions := func() map[string]Session {
		result := f.history(t, HistoryQuery{BabyID: f.babyID, Start: S, End: E})
		found := map[string]Session{}
		for _, entry := range result.Entries {
			if entry.Session == nil {
				t.Fatalf("%s/%s has no session", entry.Kind, entry.ID)
			}
			found[entry.ID] = *entry.Session
		}
		return found
	}
	want := map[string]Session{
		a2: {a1, S.Add(-time.Hour)}, a3: {a3, S.Add(2 * time.Hour)},
		b1: {opaque, S.Add(3 * time.Hour)}, b2: {opaque, S.Add(3 * time.Hour)},
		c1: {tie, S.Add(5 * time.Hour)}, c2: {tie, S.Add(5 * time.Hour)},
		joined: {open, S.Add(-30 * time.Minute)},
	}
	if got := sessions(); !sessionsEqual(got, want) {
		t.Fatalf("sessions\n got %+v\nwant %+v", got, want)
	}
	f.remove(t, "feed", a1, 1)
	want[a2] = Session{a1, S.Add(time.Hour)}
	if got := sessions(); !sessionsEqual(got, want) {
		t.Fatalf("after deleting the first member\n got %+v\nwant %+v", got, want)
	}
}

func sessionsEqual(got, want map[string]Session) bool {
	if len(got) != len(want) {
		return false
	}
	for id, session := range want {
		if other, ok := got[id]; !ok || other.ID != session.ID || !other.EarliestMemberStart.Equal(session.EarliestMemberStart) {
			return false
		}
	}
	return true
}

func TestHistoryResultBudgetNeverOmitsData(t *testing.T) {
	f := newFixture(t)
	note := strings.Repeat("n", 1000)
	var ids []string
	for i := 0; i < 5; i++ {
		text := note
		if i == 2 {
			text = strings.Repeat("N", 4000) // the oversized third entry
		}
		ids = append(ids, f.diaper(t, f.id(0xd0000000), day0.Add(time.Duration(i)*time.Hour), &text))
	}
	f.startTimer(t, "timer-feed.json", f.id(0x5f000000), day0, nil)
	query := HistoryQuery{BabyID: f.babyID, Start: day0, End: day0.Add(24 * time.Hour)}
	full := f.history(t, query)
	if len(full.Entries) != 5 || full.NextCursor != "" || len(full.ActiveTimers) != 1 {
		t.Fatalf("full %+v", full)
	}
	size := func(result HistoryResult) int { return encodedSize(t, result) }
	limited := func(limit int) HistoryResult {
		q := query
		q.Limit = limit
		return f.history(t, q)
	}
	// Exactly two small entries and their real cursor fit; the big third
	// entry, alone or after them, does not.
	limit := size(limited(2))

	t.Run("body-independent bound", func(t *testing.T) {
		f.service.maxResultBytes = size(limited(1)) - 1
		defer func() { f.service.maxResultBytes = MaxResultBytes }()
		if result, err := f.service.History(t.Context(), f.credential, query); !errors.Is(err, ErrResultTooLarge) || !reflect.DeepEqual(result, HistoryResult{}) {
			t.Fatalf("first entity over the bound: err=%v result=%+v", err, result)
		}
	})
	t.Run("fitting keeps order, cursor and revision", func(t *testing.T) {
		f.guard.calls = 0
		calls, sawTimers := 0, true
		fits := func(candidate HistoryResult) (bool, error) {
			calls++
			sawTimers = sawTimers && len(candidate.ActiveTimers) == 1
			return size(candidate) <= limit, nil
		}
		first, err := f.service.HistoryWithBudget(t.Context(), f.credential, query, fits)
		if err != nil || len(first.Entries) != 2 || first.NextCursor == "" || first.Revision != full.Revision || calls == 0 || !sawTimers || f.guard.calls != 1 {
			t.Fatalf("fitted page entries=%v cursor=%q err=%v calls=%d timers=%t guarded=%d", entryKeys(first.Entries), first.NextCursor, err, calls, sawTimers, f.guard.calls)
		}
		if size(first) > limit {
			t.Fatalf("returned page does not fit: %d > %d", size(first), limit)
		}
		if !reflect.DeepEqual(entryKeys(first.Entries), entryKeys(full.Entries[:2])) {
			t.Fatalf("fitted page %v", entryKeys(first.Entries))
		}
		next := query
		next.Cursor = first.NextCursor
		// The oversized third entry is never skipped to reach the later ones.
		if result, err := f.service.HistoryWithBudget(t.Context(), f.credential, next, fits); !errors.Is(err, ErrResultTooLarge) || !reflect.DeepEqual(result, HistoryResult{}) {
			t.Fatalf("oversized entity after cursor: err=%v result=%v", err, entryKeys(result.Entries))
		}
		// A roomier budget continues from the same cursor without loss.
		rest, err := f.service.HistoryWithBudget(t.Context(), f.credential, next, func(candidate HistoryResult) (bool, error) {
			return size(candidate) <= 3*limit, nil
		})
		if err != nil || rest.Revision != first.Revision || !reflect.DeepEqual(append(entryKeys(first.Entries), entryKeys(rest.Entries)...), entryKeys(full.Entries)) || rest.NextCursor != "" {
			t.Fatalf("continued %v cursor=%q err=%v", entryKeys(rest.Entries), rest.NextCursor, err)
		}
	})
	t.Run("fixed fields that cannot fit", func(t *testing.T) {
		never := func(HistoryResult) (bool, error) { return false, nil }
		if _, err := f.service.HistoryWithBudget(t.Context(), f.credential, query, never); !errors.Is(err, ErrResultTooLarge) {
			t.Fatalf("err=%v", err)
		}
		empty := HistoryQuery{BabyID: f.babyID, Start: day0.Add(-48 * time.Hour), End: day0.Add(-24 * time.Hour)}
		if _, err := f.service.HistoryWithBudget(t.Context(), f.credential, empty, never); !errors.Is(err, ErrResultTooLarge) {
			t.Fatalf("timers alone: err=%v", err)
		}
		if result, err := f.service.HistoryWithBudget(t.Context(), f.credential, empty, func(HistoryResult) (bool, error) { return true, nil }); err != nil || len(result.Entries) != 0 || len(result.ActiveTimers) != 1 {
			t.Fatalf("empty page err=%v result=%+v", err, result)
		}
	})
	t.Run("predicate failure", func(t *testing.T) {
		failure := errors.New("sizing failed")
		if _, err := f.service.HistoryWithBudget(t.Context(), f.credential, query, func(HistoryResult) (bool, error) { return false, failure }); !errors.Is(err, failure) {
			t.Fatalf("err=%v", err)
		}
		if _, err := f.service.HistoryWithBudget(t.Context(), f.credential, query, nil); !errors.Is(err, authority.ErrUnavailable) {
			t.Fatalf("nil predicate err=%v", err)
		}
	})
}

func TestListBabiesReturnsLiveBabiesInSortOrder(t *testing.T) {
	f := newFixture(t)
	late := f.createBaby(t, "10000000-0000-4000-8000-000000000003", "Late", 5)
	tied := f.createBaby(t, "10000000-0000-4000-8000-000000000002", "Tied", 0)
	gone := f.createBaby(t, "10000000-0000-4000-8000-000000000004", "Gone", 1)
	f.remove(t, "baby", gone, 1)
	result, err := f.service.ListBabies(t.Context(), f.credential)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, baby := range result.Babies {
		if baby.EntityKind != "baby" || baby.Meta.TombstonedAt != nil || baby.Payload == nil {
			t.Fatalf("baby entity %+v", baby)
		}
		ids = append(ids, baby.Meta.ID)
	}
	if want := []string{f.babyID, tied, late}; !reflect.DeepEqual(ids, want) || result.Revision != f.h.FamilySequence(t) {
		t.Fatalf("babies %v revision=%d, want %v %d", ids, result.Revision, want, f.h.FamilySequence(t))
	}
	if name := result.Babies[0].Payload.(protocol.BabyPayload).Name; name != "Élie" {
		t.Fatalf("name %q", name)
	}
	f.service.maxResultBytes = 64
	defer func() { f.service.maxResultBytes = MaxResultBytes }()
	if result, err := f.service.ListBabies(t.Context(), f.credential); !errors.Is(err, ErrResultTooLarge) || !reflect.DeepEqual(result, BabiesResult{}) {
		t.Fatalf("oversized babies err=%v result=%+v", err, result)
	}
}
