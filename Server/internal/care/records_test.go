package care_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"babytracker/server/internal/care"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

// legacyCase is one differential case: setup applied as the owner, then
// probe decided as one batch.
type legacyCase struct {
	name   string
	limits func(*protocol.Limits)
	setup  []protocol.Mutation
	probe  []protocol.Mutation
	code   string // the code the probe's last outcome must carry, when set
	status string // the status the probe's last outcome must carry, when set
	fatal  string // the refusal that must stop the probe, when set
}

// legacyDecision is what the legacy storage decided for a case's probe,
// recorded by this harness while the legacy implementation still ran (Phase 2
// Task 9, at 50c87d3d, just before the legacy care storage was removed): the
// pre-switch sync.Service.Apply, one guarded transaction per mutation over the
// per-kind tables, the mutation journal and its receipts. Outcomes and pulled
// changes are the encoded bytes after testkit Normalize; Pull is the latest
// journal entry per entity of the full legacy pull (testkit.LatestPerEntity).
type legacyDecision struct {
	Outcomes  []string `json:"outcomes"`
	Fatal     string   `json:"fatal,omitempty"`
	HighWater uint64   `json:"high_water"`
	Pull      []string `json:"pull,omitempty"`
}

// runLegacyCases runs every case of the calling test against the legacy
// decisions recorded in testdata/legacy-rules/<test>.json. The file is the
// frozen oracle: nothing writes it, and the cases must be exactly its
// entries.
func runLegacyCases(t *testing.T, cases []legacyCase) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "legacy-rules", t.Name()+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var decisions map[string]legacyDecision
	if err := json.Unmarshal(raw, &decisions); err != nil {
		t.Fatal(err)
	}
	if len(cases) != len(decisions) {
		t.Fatalf("%d cases, %d recorded legacy decisions", len(cases), len(decisions))
	}
	for _, c := range cases {
		decision, ok := decisions[strings.ReplaceAll(c.name, " ", "_")]
		if !ok {
			t.Fatalf("case %q has no recorded legacy decision", c.name)
		}
		t.Run(c.name, func(t *testing.T) { runLegacyCase(t, c, decision) })
	}
}

// runLegacyCase applies setup through care, one committed batch per
// mutation as the legacy service committed one transaction per mutation,
// decides probe with ApplyBatch (rolled back), and requires the recorded
// legacy decision: the same outcomes (encoded), the same refusal, the same
// high water and the same pull (the latest legacy entry per entity).
func runLegacyCase(t *testing.T, c legacyCase, want legacyDecision) {
	h := testkit.NewAtFixtureClock(t) // the legacy decisions were recorded at it
	if c.limits != nil {
		c.limits(&h.Limits)
		h.RebuildSyncService(t, h.Limits)
	}
	for i, m := range c.setup {
		result := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{m})
		if result.Fatal != nil || len(result.Outcomes) != 1 || result.Outcomes[0].Status != "applied" {
			t.Fatalf("setup %d (%s %s): %+v %v", i, m.EntityKind, m.Operation, result.Outcomes, result.Fatal)
		}
	}

	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	result, err := care.ApplyBatch(t.Context(), tx, caretest.Command(t, h, h.Owner, c.probe), &care.PullRequest{After: 0, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}

	fatal := ""
	if result.Fatal != nil {
		fatal = result.Fatal.Error()
	}
	if fatal != want.Fatal || len(result.Outcomes) != len(want.Outcomes) {
		t.Fatalf("batch refused %q after %d outcomes; legacy %q after %d", fatal, len(result.Outcomes), want.Fatal, len(want.Outcomes))
	}
	for i := range want.Outcomes {
		if got := string(h.Normalize(mustJSON(t, result.Outcomes[i]))); got != want.Outcomes[i] {
			t.Fatalf("outcome %d (%s %s)\n got %s\nwant %s", i, c.probe[i].EntityKind, c.probe[i].Operation, got, want.Outcomes[i])
		}
	}
	if c.fatal != "" && fatal != c.fatal {
		t.Fatalf("the case is not exercised: refusal %q, want %s", fatal, c.fatal)
	}
	if c.code != "" {
		if last := result.Outcomes[len(result.Outcomes)-1]; caretest.Code(last) != c.code {
			t.Fatalf("the case is not exercised: last outcome %+v code %q, want %q", last, caretest.Code(last), c.code)
		}
	}
	if c.status != "" {
		if last := result.Outcomes[len(result.Outcomes)-1]; last.Status != c.status {
			t.Fatalf("the case is not exercised: last outcome %+v, want status %q", last, c.status)
		}
	}
	if result.HighWater != want.HighWater {
		t.Fatalf("high water %d, legacy %d", result.HighWater, want.HighWater)
	}
	if result.Fatal != nil {
		return
	}
	if len(result.Changes) != len(want.Pull) {
		t.Fatalf("%d changes, legacy latest %d", len(result.Changes), len(want.Pull))
	}
	rows := map[string]care.Row{}
	for i, change := range result.Changes {
		encoded, err := change.Encode(h.Info, h.FamilyID)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(h.Normalize(encoded)); got != want.Pull[i] {
			t.Fatalf("change %d\n got %s\nwant %s", i, got, want.Pull[i])
		}
		rows[change.ID] = care.Row{ID: change.ID, Wire: change.Wire}
	}
	wantWireRoundTrips(t, rows)
}

func withBaby(m protocol.Mutation, baby string) protocol.Mutation {
	return caretest.Edit(m, func(p protocol.Payload) protocol.Payload {
		switch value := p.(type) {
		case protocol.FeedPayload:
			value.BabyID = baby
			return value
		case protocol.SleepPayload:
			value.BabyID = baby
			return value
		}
		panic(fmt.Sprintf("withBaby %T", p))
	})
}

func renamed(m protocol.Mutation, mutationID, entityID string) protocol.Mutation {
	m.MutationID, m.EntityID = mutationID, entityID
	return m
}

func mid(n int) string { return fmt.Sprintf("0a%06d-0000-4000-8000-000000000001", n) }

// The record rules decide every case as the legacy Repository.Apply decided
// it: each case's probe batch gives the legacy outcomes byte for byte, the
// same refusal, the same sequences and the same pulled state.
func TestRecordRulesMatchLegacy(t *testing.T) {
	a, b := caretest.BabyA, caretest.BabyB
	babyA, babyB := caretest.Baby(a), caretest.Baby(b)
	feed := caretest.Feed(a, 1)
	sleep := caretest.Record("sleep", a, 1)
	supplement := caretest.Record("supplement", a, 1)
	measurement := caretest.Record("measurement", a, 1)
	growth := func() protocol.Mutation {
		raw := fmt.Sprintf(`{"mutation_id":"%s","entity_id":"%s","entity_kind":"growth_profile","operation":"create","payload_version":1,
			"observed_entity_version":0,"client_mutation_time":"2026-09-20T14:10:00Z","import_id":null,
			"payload":{"baby_id":"%s","reference_sex":"girls","expected_due_date":null}}`, mid(91), protocol.GrowthProfileID(a), a)
		m, err := protocol.DecodeMutation([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}()
	sessionTimer := func(session, kind string) protocol.Mutation {
		fixture := "timer-feed.json"
		if kind == "sleep" {
			fixture = "timer-sleep.json"
		}
		m := testkit.MustMutation(t, fixture)
		m.MutationID, m.EntityID, m.PayloadVersion = mid(80), "50000000-0000-4000-8000-0000000000a1", 5
		p := m.Payload.(protocol.TimerStartPayload)
		p.BabyID, p.SessionID = a, &session
		m.Payload = p
		return m
	}
	session := "aaaaaaaa-0000-4000-8000-000000000001"
	inSession := func(m protocol.Mutation, s string) protocol.Mutation {
		return caretest.Edit(m, func(p protocol.Payload) protocol.Payload {
			switch value := p.(type) {
			case protocol.FeedPayload:
				value.FeedingSessionID = &s
				return value
			case protocol.SleepPayload:
				value.SleepSessionID = &s
				return value
			}
			panic("withSession")
		})
	}
	setLogged := func(at string) func(protocol.Payload) protocol.Payload {
		return func(p protocol.Payload) protocol.Payload {
			switch value := p.(type) {
			case protocol.SupplementPayload:
				value.LoggedAt = at
				return value
			case protocol.MeasurementPayload:
				value.LoggedAt = at
				return value
			}
			panic("setLogged")
		}
	}
	measuredAt := func(at string) protocol.Mutation {
		return caretest.Edit(caretest.Record("measurement", a, 2), func(p protocol.Payload) protocol.Payload {
			m := p.(protocol.MeasurementPayload)
			m.MeasuredAt = at
			return m
		})
	}
	escapes := strings.Repeat("<", 100)
	foods := make([]string, 50)
	for i := range foods {
		foods[i] = escapes
	}
	amount := "some"
	solids := caretest.Edit(feed, func(protocol.Payload) protocol.Payload {
		note := strings.Repeat("&", 4096)
		var parts []protocol.FeedComponent
		for i := range 3 {
			parts = append(parts, protocol.FeedComponent{ID: fmt.Sprintf("3a%06d-0000-4000-8000-000000000001", i), Kind: "solids", Foods: foods, Amount: &amount})
		}
		return protocol.FeedPayload{BabyID: a, Kind: "solids", StartedAt: "2026-09-20T12:00:00Z", TimeZoneID: "Europe/Zurich", Note: &note,
			Components: parts}
	})
	hundredFeeds := []protocol.Mutation{babyA}
	for n := 1; n < 100; n++ {
		hundredFeeds = append(hundredFeeds, caretest.Feed(a, n))
	}
	badZone := caretest.Edit(feed, func(p protocol.Payload) protocol.Payload {
		f := p.(protocol.FeedPayload)
		f.TimeZoneID = "Local"
		return f
	})

	cases := []legacyCase{
		{name: "every kind created", setup: []protocol.Mutation{babyA},
			probe: []protocol.Mutation{feed, sleep, caretest.Record("diaper", a, 1), supplement, measurement, growth}},
		{name: "update then delete in one batch", setup: []protocol.Mutation{babyA, feed},
			probe: []protocol.Mutation{caretest.Update(feed, mid(1), 1, caretest.WithNote("edited")), caretest.Delete(feed, mid(2), 2)}},
		{name: "stale update overwrites a newer version", setup: []protocol.Mutation{babyA, feed, caretest.Update(feed, mid(1), 1, caretest.WithNote("v2"))},
			probe: []protocol.Mutation{caretest.Update(feed, mid(2), 1, caretest.WithNote("stale"))}},
		{name: "baby update", setup: []protocol.Mutation{babyA},
			probe: []protocol.Mutation{caretest.Update(babyA, mid(1), 1, func(p protocol.Payload) protocol.Payload {
				baby := p.(protocol.BabyPayload)
				baby.Name = "Renamed <&>"
				return baby
			})}},
		{name: "entity exists", setup: []protocol.Mutation{babyA, feed}, probe: []protocol.Mutation{renamed(feed, mid(1), feed.EntityID)}, code: "entity_exists"},
		{name: "kind collision on create", setup: []protocol.Mutation{babyA, sleep},
			probe: []protocol.Mutation{renamed(feed, mid(1), sleep.EntityID)}, code: "entity_kind_collision"},
		{name: "kind collision on update", setup: []protocol.Mutation{babyA, sleep},
			probe: []protocol.Mutation{caretest.Update(renamed(feed, mid(1), sleep.EntityID), mid(1), 1, nil)}, code: "entity_kind_collision"},
		{name: "entity not found", setup: []protocol.Mutation{babyA},
			probe: []protocol.Mutation{caretest.Update(feed, mid(1), 1, nil), caretest.Delete(feed, mid(2), 1)}, code: "entity_not_found"},
		{name: "parent mismatch on a live record", setup: []protocol.Mutation{babyA, babyB, feed},
			probe: []protocol.Mutation{withBaby(caretest.Update(feed, mid(1), 1, nil), b)}, code: "record_parent_mismatch"},
		{name: "parent mismatch on a tombstone", setup: []protocol.Mutation{babyA, babyB, feed, caretest.Delete(feed, mid(1), 1)},
			probe: []protocol.Mutation{withBaby(caretest.Update(feed, mid(2), 2, nil), b)}, code: "record_parent_mismatch"},
		{name: "superseded by a delete", setup: []protocol.Mutation{babyA, feed, caretest.Delete(feed, mid(1), 1)},
			probe: []protocol.Mutation{caretest.Update(feed, mid(2), 1, nil), caretest.Delete(feed, mid(3), 2)}, code: "entity_deleted"},
		{name: "baby not found", setup: []protocol.Mutation{babyA, feed},
			probe: []protocol.Mutation{renamed(caretest.Feed(b, 2), mid(1), caretest.RecordID("feed", b, 2)),
				withBaby(renamed(caretest.Feed(a, 3), mid(2), caretest.RecordID("feed", a, 3)), feed.EntityID)}, code: "baby_not_found"},
		{name: "baby deleted", setup: []protocol.Mutation{babyA, babyB, deleteBaby(b)}, probe: []protocol.Mutation{caretest.Feed(b, 1)}, code: "baby_deleted"},
		{name: "measurement before birth", setup: []protocol.Mutation{babyA}, probe: []protocol.Mutation{measuredAt("2026-01-11T22:00:00Z")},
			code: "invalid_timestamp"},
		{name: "measurement on the birth date in its zone", setup: []protocol.Mutation{babyA}, probe: []protocol.Mutation{measuredAt("2026-01-11T23:30:00Z")}},
		{name: "measurement in the future", setup: []protocol.Mutation{babyA}, probe: []protocol.Mutation{measuredAt("2026-09-21T12:00:01Z")},
			code: "invalid_timestamp"},
		{name: "logged_at changed", setup: []protocol.Mutation{babyA, supplement, measurement},
			probe: []protocol.Mutation{caretest.Update(supplement, mid(1), 1, setLogged("2026-09-20T16:00:00Z")),
				caretest.Update(measurement, mid(2), 1, setLogged("2026-09-20T16:00:00Z"))}, code: "record_logged_at_mismatch"},
		{name: "logged_at kept", setup: []protocol.Mutation{babyA, supplement, measurement},
			probe: []protocol.Mutation{caretest.Update(supplement, mid(1), 1, caretest.WithNote("kept")), caretest.Update(measurement, mid(2), 1, nil)}},
		{name: "session of another baby, committed", setup: []protocol.Mutation{babyA, babyB, feed},
			probe: []protocol.Mutation{inSession(caretest.Feed(b, 2), feed.EntityID)}, code: "record_parent_mismatch"},
		{name: "session of the same baby", setup: []protocol.Mutation{babyA, feed, inSession(caretest.Feed(a, 2), feed.EntityID)},
			probe: []protocol.Mutation{inSession(caretest.Feed(a, 3), feed.EntityID)}},
		{name: "session of another baby or kind, earlier in the batch", setup: []protocol.Mutation{babyA, babyB},
			probe: []protocol.Mutation{inSession(caretest.Feed(b, 1), session), inSession(caretest.Feed(a, 2), session)}, code: "record_parent_mismatch"},
		{name: "sleep joining a feed session", setup: []protocol.Mutation{babyA, inSession(feed, session)},
			probe: []protocol.Mutation{inSession(sleep, session)}, code: "record_parent_mismatch"},
		{name: "session left earlier in the batch", setup: []protocol.Mutation{babyA, babyB, inSession(caretest.Feed(b, 1), session)},
			probe: []protocol.Mutation{caretest.Update(caretest.Feed(b, 1), mid(1), 1, nil), inSession(caretest.Feed(a, 2), session)}},
		{name: "session of a live timer of the other kind", setup: []protocol.Mutation{babyA, sessionTimer(session, "sleep")},
			probe: []protocol.Mutation{inSession(feed, session)}, code: "record_parent_mismatch"},
		{name: "session of a live timer of the same kind", setup: []protocol.Mutation{babyA, sessionTimer(session, "feed")},
			probe: []protocol.Mutation{inSession(feed, session)}},
		{name: "canonical mutation too large", limits: func(l *protocol.Limits) { l.SyncMutationBytes = 4096 }, setup: []protocol.Mutation{babyA},
			probe: []protocol.Mutation{caretest.Edit(feed, caretest.WithNote(strings.Repeat("n", 4096)))}, code: "mutation_too_large"},
		{name: "entity too large for the minimum page", setup: []protocol.Mutation{babyA}, probe: []protocol.Mutation{solids}, code: "mutation_too_large"},
		{name: "invalid mutation", setup: []protocol.Mutation{babyA}, probe: []protocol.Mutation{badZone}, code: "invalid_mutation"},
		{name: "live baby quota", limits: func(l *protocol.Limits) { l.MaxLiveBabies = 1 }, setup: []protocol.Mutation{babyA},
			probe: []protocol.Mutation{caretest.Feed(a, 1), babyB}},
		{name: "record quota", limits: func(l *protocol.Limits) { l.MaxCanonicalRecordsPerFamily = 100 }, setup: hundredFeeds,
			probe: []protocol.Mutation{caretest.Update(caretest.Feed(a, 1), mid(1), 1, nil), caretest.Feed(a, 100), caretest.Feed(a, 101)}},
		{name: "a delete under a full record quota", limits: func(l *protocol.Limits) { l.MaxCanonicalRecordsPerFamily = 100 },
			setup: append(append([]protocol.Mutation{}, hundredFeeds...), caretest.Feed(a, 100)),
			probe: []protocol.Mutation{caretest.Delete(caretest.Feed(a, 1), mid(1), 1), caretest.Feed(a, 101), caretest.Feed(a, 102)}},
	}
	runLegacyCases(t, cases)
}

// Ported from records/components_test.go at 342973dc: feed parts are stored and pulled
// back in order with their v4 fields; an older client's update is refused
// before it reaches the stored feed; a session link to another baby's feed
// is refused.
func TestComponentsRoundTripThroughTheBatch(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA), caretest.Baby(caretest.BabyB))
	left, right, start, end := "left", "right", "2026-09-24T10:00:00.000000Z", "2026-09-24T10:02:00.000000Z"
	seconds := uint32(120)
	milk, formula := "breast_milk", "formula"
	link := "40000000-0000-4000-8000-000000000801"
	create := caretest.Edit(caretest.Feed(caretest.BabyA, 1), func(protocol.Payload) protocol.Payload {
		return protocol.FeedPayload{BabyID: caretest.BabyA, Kind: "breast", StartedAt: start, EndedAt: &end, TimeZoneID: "UTC", FeedingSessionID: &link,
			Components: []protocol.FeedComponent{
				{ID: "30000000-0000-4000-8000-000000000001", Kind: "breast", Side: &left, Seconds: &seconds, StartedAt: &start, EndedAt: &end},
				{ID: "30000000-0000-4000-8000-000000000002", Kind: "breast", Side: &right, Seconds: &seconds},
				{ID: "30000000-0000-4000-8000-000000000003", Kind: "bottle", VolumeML: protocol.MustParseDecimal("50"), Contents: &milk},
				{ID: "30000000-0000-4000-8000-000000000004", Kind: "bottle", VolumeML: protocol.MustParseDecimal("30"), Contents: &formula},
			}}
	})
	outcome := caretest.Seed(t, h, create).Outcomes[0]
	parts := outcome.CanonicalEntity.Payload.(protocol.FeedPayload).Components
	if len(parts) != 4 || *parts[2].Contents != milk || *parts[3].Contents != formula {
		t.Fatalf("canonical parts %+v", parts)
	}
	row := caretest.Rows(t, h)[create.EntityID]
	stored, err := row.Entity()
	if err != nil {
		t.Fatal(err)
	}
	feed := stored.Payload.(protocol.FeedPayload)
	if feed.FeedingSessionID == nil || *feed.FeedingSessionID != link || len(feed.Components) != 4 || feed.Components[0].StartedAt == nil ||
		*feed.Components[0].StartedAt != start || feed.Components[3].ID != "30000000-0000-4000-8000-000000000004" {
		t.Fatalf("stored feed %+v", feed)
	}
	if row.Derived.BreastSeconds != 240 || row.Derived.BottleVolumeML.String() != "80" || row.Derived.BottleObservations != 2 {
		t.Fatalf("derived %+v", row.Derived)
	}

	// A payload-version-3 update (no v4 fields) is refused.
	older := caretest.Update(create, mid(2), 1, nil)
	older.PayloadVersion = 3
	result := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{older})
	if o := result.Outcomes[0]; o.Status != "rejected" || caretest.Code(o) != "invalid_mutation" {
		t.Fatalf("older update %+v %s", o, caretest.Code(o))
	}
	if after := caretest.Rows(t, h)[create.EntityID]; after.EntityVersion != 1 || string(after.Wire) != string(row.Wire) {
		t.Fatal("refused update changed the feed")
	}

	// Baby B's feed may not join baby A's feed as its session.
	other := inSessionOf(caretest.Feed(caretest.BabyB, 2), create.EntityID)
	result = caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{other})
	if o := result.Outcomes[0]; caretest.Code(o) != "record_parent_mismatch" {
		t.Fatalf("cross-baby link %+v", o)
	}
}

func inSessionOf(m protocol.Mutation, session string) protocol.Mutation {
	return caretest.Edit(m, caretest.WithFeedingSession(session))
}

// Ported from records/sleep_sessions_test.go at 342973dc (the record cases; the timer
// cases follow the timer planner): a session id owned by a live sleep of
// one baby refuses another baby's sleep and a feed of the same baby, and
// admits the same baby's sleep; a live feed timer's session refuses another
// baby's feed and admits its own baby's.
func TestSleepSessionOwnership(t *testing.T) {
	h := testkit.New(t)
	owner := caretest.Owner(h)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA), caretest.Baby(caretest.BabyB))
	session := "aaaaaaaa-0000-4000-8000-000000000004"
	sleepIn := func(m protocol.Mutation) protocol.Mutation {
		return caretest.Edit(m, func(p protocol.Payload) protocol.Payload {
			s := p.(protocol.SleepPayload)
			s.SleepSessionID = &session
			return s
		})
	}
	caretest.Seed(t, h, sleepIn(caretest.Record("sleep", caretest.BabyA, 1)))
	result := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{
		sleepIn(caretest.Record("sleep", caretest.BabyB, 2)), sleepIn(caretest.Record("sleep", caretest.BabyA, 3)),
		inSessionOf(caretest.Feed(caretest.BabyA, 4), session)})
	if o := result.Outcomes; caretest.Code(o[0]) != "record_parent_mismatch" || o[1].Status != "applied" || caretest.Code(o[2]) != "record_parent_mismatch" {
		t.Fatalf("outcomes %+v", o)
	}
	// A live sleep timer (written as a row) owns its session against feeds.
	rows := caretest.Rows(t, h)
	baby := rows[caretest.BabyA]
	timerSession := "aaaaaaaa-0000-4000-8000-000000000005"
	timer := owner.Row(t, "timer", "50000000-0000-4000-8000-0000000000a5", &baby, familyStateOf(t, h).Sequence+1)
	timer = owner.Session(t, timer, timerSession, timer.ChangedSeq+1)
	timer.ChangedSeq--
	if err := caretest.Write(t, h, timer); err != nil {
		t.Fatal(err)
	}
	result = caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{inSessionOf(caretest.Feed(caretest.BabyB, 5), timerSession),
		inSessionOf(caretest.Feed(caretest.BabyA, 6), timerSession)})
	if o := result.Outcomes; caretest.Code(o[0]) != "record_parent_mismatch" || o[1].Status != "applied" {
		t.Fatalf("feeds joining a feed timer's session: %+v", o)
	}
}

// Ported from records/repository_test.go at 342973dc: the entity charge is the legacy
// entity storage bytes (plus the live reserve), deterministic and nonzero.
func TestRowChargeIsTheLegacyEntityCharge(t *testing.T) {
	entity := protocol.CanonicalEntity{EntityKind: "baby", Meta: protocol.EntityMeta{
		ID: "10000000-0000-4000-8000-000000000001", FamilyID: "20000000-0000-4000-8000-000000000001",
		EntityVersion: 1, CreatorAccountID: "30000000-0000-4000-8000-000000000001",
		LastEditorAccountID: "30000000-0000-4000-8000-000000000001",
		ClientMutationTime:  "2026-09-20T12:00:00.000000Z", AcceptedAt: "2026-09-20T12:00:01.000000Z",
	}, Payload: protocol.BabyPayload{Name: "Élie", BirthDate: "2026-01-12"}}
	legacy, err := legacyEntityCharge(entity)
	if err != nil || legacy <= 0 {
		t.Fatalf("legacy %d %v", legacy, err)
	}
	for range 2 {
		row, err := care.Encode(entity, nil, care.Change{Seq: 1, ActorAccountID: entity.Meta.CreatorAccountID,
			DeviceID: "40000000-0000-4000-8000-000000000001", Operation: "create"})
		if err != nil {
			t.Fatal(err)
		}
		if row.ChargedBytes() != legacy+legacyDeletionReserve {
			t.Fatalf("charged %d, legacy %d + reserve", row.ChargedBytes(), legacy)
		}
	}
}

// The storage quota counts the entity charge, its live reserve and the
// mutation's receipt on the family counters: a create is admitted up to the
// limit exactly, refused one byte past it, and a delete is never refused.
func TestStorageQuotaChargesTheEntityAndItsReserve(t *testing.T) {
	h := testkit.New(t)
	feed := caretest.Feed(caretest.BabyA, 1)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA), feed)
	next := caretest.Feed(caretest.BabyA, 2)
	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	probe, err := care.ApplyBatch(t.Context(), tx, caretest.Command(t, h, h.Owner, []protocol.Mutation{next}), nil)
	if err != nil || probe.Fatal != nil {
		t.Fatal(err, probe.Fatal)
	}
	_ = tx.Rollback(t.Context())
	charge, err := legacyEntityCharge(*probe.Outcomes[0].CanonicalEntity)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := caretest.LegacyReceiptCharge(probe.Outcomes[0])
	if err != nil {
		t.Fatal(err)
	}
	charge += legacyDeletionReserve + receipt
	limit := h.Limits.MaxChargedStorageBytesPerFamily
	setCharged := func(charged int64) {
		tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
		if _, err := tx.Exec(t.Context(), `UPDATE families SET care_charged_bytes = $2 WHERE id = $1`, h.FamilyID, charged); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	apply := func(m protocol.Mutation) care.BatchResult {
		tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
		defer func() { _ = tx.Rollback(t.Context()) }()
		result, err := care.ApplyBatch(t.Context(), tx, caretest.Command(t, h, h.Owner, []protocol.Mutation{m}), nil)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	setCharged(limit - charge + 1)
	if r := apply(next); !errors.Is(r.Fatal, care.ErrQuotaExceeded) {
		t.Fatalf("one byte past the limit: %v", r.Fatal)
	}
	setCharged(limit - charge)
	if r := apply(next); r.Fatal != nil || r.Outcomes[0].Status != "applied" {
		t.Fatalf("exactly at the limit: %v", r.Fatal)
	}
	// Far over the limit (a lowered quota), as chargeUsage: the total is
	// checked, not the delta, so an unchanged update is refused, and a delete
	// is admitted although the family stays over the limit after it.
	setCharged(limit + 1<<20)
	if r := apply(caretest.Update(feed, mid(1), 1, nil)); !errors.Is(r.Fatal, care.ErrQuotaExceeded) {
		t.Fatalf("an update over the limit: %v", r.Fatal)
	}
	if r := apply(caretest.Delete(feed, mid(2), 1)); r.Fatal != nil || r.Outcomes[0].Status != "applied" {
		t.Fatalf("delete over the limit: %v", r.Fatal)
	}
	// A rejection writes no entity, but its receipt is charged and refused
	// over the limit, as the legacy ChargeReceipt refused it.
	if r := apply(caretest.Update(caretest.Feed(caretest.BabyA, 9), mid(3), 1, nil)); !errors.Is(r.Fatal, care.ErrQuotaExceeded) {
		t.Fatalf("a rejection over the limit: %v %+v", r.Fatal, r.Outcomes)
	}
}

// Receipts cannot grow a family past its storage quota (Ruling R1-I2):
// every update of one entity replaces the entity's charge but adds its
// receipt's, so repeated updates (and repeated refusals) end at the storage
// limit with the legacy refusal, never past it; the refused mutation's
// write is rolled back with it, and the counters stay the rows plus the
// receipts.
func TestReceiptsCannotExceedTheStorageQuota(t *testing.T) {
	for _, refusals := range []bool{false, true} {
		t.Run(map[bool]string{false: "updates", true: "rejections"}[refusals], func(t *testing.T) {
			h := testkit.New(t)
			feed := caretest.Feed(caretest.BabyA, 1)
			caretest.Seed(t, h, caretest.Baby(caretest.BabyA), feed)
			// 64 KiB of headroom under the limit, about a hundred receipts:
			// the family's charge is raised by offset for the loop and
			// lowered again before the counters are compared with the rows.
			offset := h.Limits.MaxChargedStorageBytesPerFamily - 64<<10 - int64(familyStateOf(t, h).ChargedBytes)
			addCharge := func(bytes int64) {
				tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
				if _, err := tx.Exec(t.Context(), `UPDATE families SET care_charged_bytes = care_charged_bytes + $2 WHERE id = $1`, h.FamilyID, bytes); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			addCharge(offset)
			for n := 1; ; n++ {
				if n > 1000 {
					t.Fatal("1000 receipts never reached the storage quota")
				}
				state := familyStateOf(t, h)
				m := caretest.Update(feed, mid(n), uint64(n), caretest.WithNote(strings.Repeat("n", 200)))
				if refusals {
					m = caretest.Update(caretest.Feed(caretest.BabyA, 2), mid(n), 1, nil) // entity_not_found
				}
				result := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{m})
				if result.Fatal != nil {
					if !errors.Is(result.Fatal, care.ErrQuotaExceeded) || result.Fatal.Error() != "family_record_quota_exceeded" || len(result.Outcomes) != 0 {
						t.Fatalf("update %d refused with %v after %d outcomes", n, result.Fatal, len(result.Outcomes))
					}
					if after := familyStateOf(t, h); after != state {
						t.Fatalf("the refused mutation changed the family: %+v -> %+v", state, after)
					}
					wantNoReceipt(t, h, h.Owner, m.MutationID)
					if n < 20 {
						t.Fatalf("refused after %d receipts of 64 KiB headroom", n)
					}
					break
				}
				if refusals && caretest.Code(result.Outcomes[0]) != "entity_not_found" || !refusals && result.Outcomes[0].Status != "applied" {
					t.Fatalf("update %d: %+v %s", n, result.Outcomes[0], caretest.Code(result.Outcomes[0]))
				}
				if charged := familyStateOf(t, h).ChargedBytes; charged <= state.ChargedBytes ||
					int64(charged) > h.Limits.MaxChargedStorageBytesPerFamily {
					t.Fatalf("update %d moved the charge %d -> %d (limit %d)", n, state.ChargedBytes, charged, h.Limits.MaxChargedStorageBytesPerFamily)
				}
			}
			addCharge(-offset)
			wantCountersMatchRows(t, h)
		})
	}
}

// A mutation whose receipt is over the storage quota is refused whole,
// after its rule already wrote the entity in the batch's working set: the
// batch commits the prefix only (the earlier update, its receipt, one
// sequence), and the refused update's row write is undone, as the legacy
// mutation transaction rolled back.
func TestReceiptOverTheQuotaRefusesItsMutationWhole(t *testing.T) {
	h := testkit.New(t)
	feed := caretest.Feed(caretest.BabyA, 1)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA), feed)
	first := caretest.Update(feed, mid(1), 1, caretest.WithNote("first"))
	second := caretest.Update(feed, mid(2), 2, caretest.WithNote("other"))
	base := familyStateOf(t, h)
	charged := func(batch ...protocol.Mutation) int64 {
		tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
		defer func() { _ = tx.Rollback(t.Context()) }()
		result, err := care.ApplyBatch(t.Context(), tx, caretest.Command(t, h, h.Owner, batch), nil)
		if err != nil || result.Fatal != nil || len(result.Outcomes) != len(batch) {
			t.Fatalf("probe: %v %v", err, result.Fatal)
		}
		after, err := care.ReadFamilyState(t.Context(), tx, h.FamilyID)
		if err != nil {
			t.Fatal(err)
		}
		return int64(after.ChargedBytes) - int64(base.ChargedBytes)
	}
	one, both := charged(first), charged(first, second)
	if both <= one {
		t.Fatalf("the second update charged nothing: %d then %d", one, both)
	}
	// The first update fits exactly; the second's receipt does not.
	offset := h.Limits.MaxChargedStorageBytesPerFamily - one - int64(base.ChargedBytes)
	h.ExecFamily(t, `UPDATE families SET care_charged_bytes = care_charged_bytes + $2 WHERE id = $1`, h.FamilyID, offset)
	result := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{first, second})
	if !errors.Is(result.Fatal, care.ErrQuotaExceeded) || len(result.Outcomes) != 1 || result.Outcomes[0].Status != "applied" {
		t.Fatalf("batch: %v %+v", result.Fatal, result.Outcomes)
	}
	state := familyStateOf(t, h)
	if state.Sequence != base.Sequence+1 || int64(state.ChargedBytes) != h.Limits.MaxChargedStorageBytesPerFamily {
		t.Fatalf("family after the refusal %+v (base %+v, limit %d)", state, base, h.Limits.MaxChargedStorageBytesPerFamily)
	}
	stored := caretest.Rows(t, h)[feed.EntityID]
	entity, err := stored.Entity()
	if err != nil {
		t.Fatal(err)
	}
	if stored.EntityVersion != 2 || stored.ChangedSeq != base.Sequence+1 || *entity.Payload.(protocol.FeedPayload).Note != "first" {
		t.Fatalf("stored feed %d at %d %+v, want the first update", stored.EntityVersion, stored.ChangedSeq, entity.Payload)
	}
	wantReceipt(t, h, h.Owner, result.Outcomes[0])
	wantNoReceipt(t, h, h.Owner, second.MutationID)
	h.ExecFamily(t, `UPDATE families SET care_charged_bytes = care_charged_bytes - $2 WHERE id = $1`, h.FamilyID, offset)
	wantCountersMatchRows(t, h)
}

// probeCharge applies batch as the owner in a transaction it rolls back,
// requires every mutation to apply, and returns the charge the batch added
// to the family and its outcomes.
func probeCharge(t *testing.T, h *testkit.Harness, batch ...protocol.Mutation) (int64, []protocol.Outcome) {
	t.Helper()
	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	defer func() { _ = tx.Rollback(t.Context()) }()
	before, err := care.ReadFamilyState(t.Context(), tx, h.FamilyID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := care.ApplyBatch(t.Context(), tx, caretest.Command(t, h, h.Owner, batch), nil)
	if err != nil || result.Fatal != nil || len(result.Outcomes) != len(batch) {
		t.Fatalf("probe: %v %v", err, result.Fatal)
	}
	for i, o := range result.Outcomes {
		if o.Status != "applied" {
			t.Fatalf("probe %s %s: %+v %s", batch[i].EntityKind, batch[i].Operation, o, caretest.Code(o))
		}
	}
	after, err := care.ReadFamilyState(t.Context(), tx, h.FamilyID)
	if err != nil {
		t.Fatal(err)
	}
	return int64(after.ChargedBytes) - int64(before.ChargedBytes), result.Outcomes
}

// receiptOneByteOver commits batch with the family's charge offset so the
// whole batch would exceed the storage quota by exactly one byte, removes
// the offset again, and returns the result: the last mutation's rule is
// admitted and only its receipt is refused.
func receiptOneByteOver(t *testing.T, h *testkit.Harness, batch ...protocol.Mutation) care.BatchResult {
	t.Helper()
	all, outcomes := probeCharge(t, h, batch...)
	last, err := caretest.LegacyReceiptCharge(outcomes[len(outcomes)-1])
	if err != nil {
		t.Fatal(err)
	}
	if last < 2 {
		t.Fatalf("the last receipt charges %d bytes", last)
	}
	base := int64(familyStateOf(t, h).ChargedBytes)
	offset := h.Limits.MaxChargedStorageBytesPerFamily - (base + all - 1)
	h.ExecFamily(t, `UPDATE families SET care_charged_bytes = care_charged_bytes + $2 WHERE id = $1`, h.FamilyID, offset)
	result := caretest.ApplyCommitted(t, h, h.Owner, batch)
	h.ExecFamily(t, `UPDATE families SET care_charged_bytes = care_charged_bytes - $2 WHERE id = $1`, h.FamilyID, offset)
	return result
}

// A timer stop writes two rows, the timer's tombstone and a new completion
// record. When its receipt lands one byte over the storage quota, after a
// feed create in the same batch, the stop is refused whole: the batch
// commits only the create, the timer is still running, the completion
// does not exist, and the family counters (charge, record count, entity
// count, wire bytes) are the rows' and receipts' again, so the undo log
// restored the counter delta of both writes.
func TestTimerStopReceiptOverTheQuotaUndoesBothWrites(t *testing.T) {
	h := testkit.New(t)
	sleepTimer, completion := "50000000-0000-4000-8000-0000000000c1", "31000000-0000-4000-8000-0000000000c1"
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA), caretest.StartTimer(caretest.BabyA, sleepTimer, "sleep"))
	create := caretest.Feed(caretest.BabyA, 1)
	stop := caretest.Stop(caretest.BabyA, sleepTimer, completion)
	base := familyStateOf(t, h)
	result := receiptOneByteOver(t, h, create, stop)
	if !errors.Is(result.Fatal, care.ErrQuotaExceeded) || len(result.Outcomes) != 1 || result.Outcomes[0].Status != "applied" ||
		result.HighWater != base.Sequence+1 {
		t.Fatalf("batch: %v, %d outcomes, high water %d (base %d)", result.Fatal, len(result.Outcomes), result.HighWater, base.Sequence)
	}
	rows := caretest.Rows(t, h)
	if _, ok := rows[completion]; ok {
		t.Fatal("the refused stop's completion was written")
	}
	if timer := rows[sleepTimer]; !timer.Live() || timer.EntityVersion != 1 {
		t.Fatalf("timer after the refused stop %+v", timer)
	}
	if _, ok := rows[create.EntityID]; !ok {
		t.Fatal("the admitted create was not written")
	}
	wantNoReceipt(t, h, h.Owner, stop.MutationID)
	wantCountersMatchRows(t, h)
}

// A create and an update of the same entity in one batch, the update's
// receipt one byte over the storage quota: the update (its note grows the
// stored wire) is refused whole, the entity stays at the create's version,
// and the counters match the rows, so the undo log restored the update's
// counter delta over the create it replaced.
func TestChainedUpdateReceiptOverTheQuotaUndoesItsWrite(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	create := caretest.Feed(caretest.BabyA, 1)
	update := caretest.Update(create, mid(1), 1, caretest.WithNote(strings.Repeat("n", 300)))
	base := familyStateOf(t, h)
	result := receiptOneByteOver(t, h, create, update)
	if !errors.Is(result.Fatal, care.ErrQuotaExceeded) || len(result.Outcomes) != 1 || result.Outcomes[0].Status != "applied" ||
		result.HighWater != base.Sequence+1 {
		t.Fatalf("batch: %v, %d outcomes, high water %d (base %d)", result.Fatal, len(result.Outcomes), result.HighWater, base.Sequence)
	}
	if stored := caretest.Rows(t, h)[create.EntityID]; stored.EntityVersion != 1 || stored.ChangedSeq != base.Sequence+1 {
		t.Fatalf("stored feed %+v, want the create", stored)
	}
	wantNoReceipt(t, h, h.Owner, update.MutationID)
	wantCountersMatchRows(t, h)
}
