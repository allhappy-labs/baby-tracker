package caretest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/care"
	"babytracker/server/internal/family"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

// Babies the batch builders log against.
const (
	BabyA = "11000000-0000-4000-8000-0000000000a1"
	BabyB = "11000000-0000-4000-8000-0000000000b1"
)

const (
	zoneID     = "Europe/Zurich"
	clientTime = "2026-09-20T15:00:00Z"
)

// Kind digits lead the ids the builders derive, so ids of different kinds
// never collide.
var kindDigit = map[string]int{"feed": 2, "sleep": 3, "diaper": 4, "supplement": 6, "measurement": 7, "immunization": 9}

// canonical is m as the server receives it: encoded and decoded through
// protocol.DecodeMutation.
func canonical(m protocol.Mutation) protocol.Mutation {
	raw, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	decoded, err := protocol.DecodeMutation(raw)
	if err != nil {
		panic(fmt.Sprintf("caretest: %s %s does not decode: %v\n%s", m.EntityKind, m.Operation, err, raw))
	}
	return decoded
}

// Baby is the create of a baby born 2026-01-12.
func Baby(id string) protocol.Mutation {
	return canonical(protocol.Mutation{MutationID: "0b" + id[2:], EntityID: id, EntityKind: "baby", Operation: "create",
		PayloadVersion: protocol.CurrentPayloadVersion("baby"), ClientMutationTime: clientTime,
		Payload: protocol.BabyPayload{Name: "Baby " + id[len(id)-2:], BirthDate: "2026-01-12"}})
}

// RecordID is the id of the n-th record of kind for baby.
func RecordID(kind, baby string, n int) string {
	return fmt.Sprintf("%d%07d-0000-4000-8000-%s", kindDigit[kind], n, baby[24:])
}

// Record is the create of the n-th record of kind (feed, sleep, diaper,
// supplement, measurement or immunization) for baby, with a fixed valid payload.
func Record(kind, baby string, n int) protocol.Mutation {
	id := RecordID(kind, baby, n)
	var payload protocol.Payload
	switch kind {
	case "feed":
		contents := "formula"
		payload = protocol.FeedPayload{BabyID: baby, Kind: "bottle", StartedAt: "2026-09-20T12:00:00Z", TimeZoneID: zoneID,
			Components: []protocol.FeedComponent{{ID: "3" + id[1:], Kind: "bottle", VolumeML: protocol.MustParseDecimal("60"), Contents: &contents}}}
	case "sleep":
		end := "2026-09-20T14:00:00Z"
		payload = protocol.SleepPayload{BabyID: baby, StartedAt: "2026-09-20T13:00:00Z", EndedAt: &end, TimeZoneID: zoneID}
	case "diaper":
		payload = protocol.DiaperPayload{BabyID: baby, StartedAt: "2026-09-20T14:10:00Z", TimeZoneID: zoneID, Contents: "wet"}
	case "supplement":
		payload = protocol.SupplementPayload{BabyID: baby, StartedAt: "2026-09-20T14:10:00Z", TimeZoneID: zoneID, Name: "Vitamin D",
			LoggedAt: "2026-09-20T15:00:00Z"}
	case "measurement":
		position := "lying"
		payload = protocol.MeasurementPayload{BabyID: baby, MeasuredAt: "2026-09-20T14:10:00Z", LoggedAt: "2026-09-20T15:00:00Z",
			TimeZoneID: zoneID, WeightKG: protocol.MustParseDecimal("7.125"), LengthCM: protocol.MustParseDecimal("65.5"), LengthPosition: &position}
	case "immunization":
		dose := 1 + n%10
		payload = protocol.ImmunizationPayload{BabyID: baby, GivenAt: "2026-09-20T09:30:00Z", TimeZoneID: zoneID, Vaccine: "Prevenar 13", Dose: &dose}
	default:
		panic("caretest: record kind " + kind)
	}
	return canonical(protocol.Mutation{MutationID: fmt.Sprintf("0%d%06d-0000-4000-8000-%s", kindDigit[kind], n, baby[24:]), EntityID: id,
		EntityKind: kind, Operation: "create", PayloadVersion: protocol.CurrentPayloadVersion(kind), ClientMutationTime: clientTime,
		Payload: payload})
}

// Feed is the n-th bottle feed of baby.
func Feed(baby string, n int) protocol.Mutation { return Record("feed", baby, n) }

// Update is m sent again as an update of version observed under mutation
// id mutationID, with payload edited by edit (nil: unchanged).
func Update(m protocol.Mutation, mutationID string, observed uint64, edit func(protocol.Payload) protocol.Payload) protocol.Mutation {
	m.MutationID, m.Operation, m.ObservedEntityVersion = mutationID, "update", observed
	return Edit(m, edit)
}

// Edit is m with its payload edited by edit (nil: unchanged).
func Edit(m protocol.Mutation, edit func(protocol.Payload) protocol.Payload) protocol.Mutation {
	if edit != nil {
		m.Payload = edit(m.Payload)
	}
	return canonical(m)
}

// Delete is the delete of the record m created, observed at version
// observed, under mutation id mutationID.
func Delete(m protocol.Mutation, mutationID string, observed uint64) protocol.Mutation {
	var baby *string
	if id := BabyOf(m); id != "" {
		baby = &id
	}
	return canonical(protocol.Mutation{MutationID: mutationID, EntityID: m.EntityID, EntityKind: m.EntityKind, Operation: "delete",
		PayloadVersion: 1, ObservedEntityVersion: observed, ClientMutationTime: clientTime, Payload: protocol.DeletePayload{BabyID: baby}})
}

// BabyOf is the baby a record mutation's payload names.
func BabyOf(m protocol.Mutation) string {
	switch p := m.Payload.(type) {
	case protocol.FeedPayload:
		return p.BabyID
	case protocol.SleepPayload:
		return p.BabyID
	case protocol.DiaperPayload:
		return p.BabyID
	case protocol.SupplementPayload:
		return p.BabyID
	case protocol.MeasurementPayload:
		return p.BabyID
	case protocol.ImmunizationPayload:
		return p.BabyID
	case protocol.GrowthProfilePayload:
		return p.BabyID
	case protocol.DeletePayload:
		if p.BabyID != nil {
			return *p.BabyID
		}
	}
	return ""
}

// WithFeedingSession is a feed payload edit joining session.
func WithFeedingSession(session string) func(protocol.Payload) protocol.Payload {
	return func(p protocol.Payload) protocol.Payload {
		feed := p.(protocol.FeedPayload)
		feed.FeedingSessionID = &session
		return feed
	}
}

// WithNote is a payload edit setting the note of a feed, sleep, diaper or
// supplement.
func WithNote(note string) func(protocol.Payload) protocol.Payload {
	return func(p protocol.Payload) protocol.Payload {
		switch value := p.(type) {
		case protocol.FeedPayload:
			value.Note = &note
			return value
		case protocol.SleepPayload:
			value.Note = &note
			return value
		case protocol.DiaperPayload:
			value.Note = &note
			return value
		case protocol.SupplementPayload:
			value.Note = &note
			return value
		}
		panic(fmt.Sprintf("caretest: no note on %T", p))
	}
}

// MixedBatch is size mutations for baby: feeds (every other one in the
// first feed's session), sleeps, diapers, supplements and measurements in
// turn, ending, from two mutations on, with an update of the first feed.
func MixedBatch(baby string, size int) []protocol.Mutation {
	kinds := []string{"feed", "sleep", "diaper", "supplement", "measurement"}
	batch := make([]protocol.Mutation, 0, size)
	creates := size
	if size > 1 {
		creates = size - 1
	}
	var first protocol.Mutation
	for i := 0; i < creates; i++ {
		kind := kinds[i%len(kinds)]
		m := Record(kind, baby, 1000+i)
		if kind == "feed" {
			if i == 0 {
				first = m
			} else if (i/len(kinds))%2 == 1 {
				m = Edit(m, WithFeedingSession(first.EntityID))
			}
		}
		batch = append(batch, m)
	}
	if size > 1 {
		batch = append(batch, Update(first, "0e"+first.MutationID[2:], 1, WithNote("edited")))
	}
	return batch
}

// Policy is the authority's family policy as a batch policy: a viewer
// reaching a caregiver action is read_only_caregiver, any other refusal
// owner_required.
func Policy(role, action string) error {
	if family.Allowed(role, action) {
		return nil
	}
	if role == "viewer" && family.Allowed("caregiver", action) {
		return authority.ErrReadOnlyCaregiver
	}
	return authority.ErrOwnerRequired
}

// Role is p's membership role in the harness family.
func Role(t *testing.T, h *testkit.Harness, p access.Principal) string {
	t.Helper()
	tx := TenantTx(t, h.Runtime, p.AccountID, h.FamilyID)
	defer func() { _ = tx.Rollback(t.Context()) }()
	var role string
	if err := tx.QueryRow(t.Context(), `SELECT role::text FROM memberships WHERE family_id=$1 AND account_id=$2 AND status='active'`,
		h.FamilyID, p.AccountID).Scan(&role); err != nil {
		t.Fatalf("role of %s: %v", p.AccountID, err)
	}
	return role
}

// Command is batch sent by p into the harness family: p's grant and device,
// accepted at h.Now, under the harness limits and the family Policy.
func Command(t *testing.T, h *testkit.Harness, p access.Principal, batch []protocol.Mutation) care.BatchCommand {
	t.Helper()
	return care.BatchCommand{Grant: access.Grant{AccountID: p.AccountID, FamilyID: h.FamilyID, Role: Role(t, h, p)}, DeviceID: p.DeviceID,
		Mutations: batch, AcceptedAt: h.Now, Server: h.Info, Limits: h.Limits, Policy: Policy}
}

// ApplyCommitted applies batch as p in a tenant transaction and commits
// it; an infrastructure error fails the test.
func ApplyCommitted(t *testing.T, h *testkit.Harness, p access.Principal, batch []protocol.Mutation) care.BatchResult {
	t.Helper()
	return ApplyCommandCommitted(t, h, p, Command(t, h, p, batch))
}

// ApplyCommandCommitted is ApplyCommitted of a built command.
func ApplyCommandCommitted(t *testing.T, h *testkit.Harness, p access.Principal, cmd care.BatchCommand) care.BatchResult {
	t.Helper()
	tx := TenantTx(t, h.Runtime, p.AccountID, h.FamilyID)
	result, err := care.ApplyBatch(t.Context(), tx, cmd, nil)
	if err != nil {
		_ = tx.Rollback(t.Context())
		t.Fatalf("apply: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return result
}

// Seed applies mutations as the owner and requires every one applied.
func Seed(t *testing.T, h *testkit.Harness, mutations ...protocol.Mutation) care.BatchResult {
	t.Helper()
	result := ApplyCommitted(t, h, h.Owner, mutations)
	if result.Fatal != nil || len(result.Outcomes) != len(mutations) {
		t.Fatalf("seed: %d of %d outcomes, %v", len(result.Outcomes), len(mutations), result.Fatal)
	}
	for i, outcome := range result.Outcomes {
		if outcome.Status != "applied" {
			t.Fatalf("seed %s %s: %+v code %s", mutations[i].EntityKind, mutations[i].Operation, outcome, Code(outcome))
		}
	}
	return result
}

// Code is an outcome's code, or "" without one.
func Code(o protocol.Outcome) string {
	if o.Code == nil {
		return ""
	}
	return *o.Code
}

// StoredReceipt is the stored receipt outcome of (device, mutation), read
// in a tenant transaction of account; found is false without one.
func StoredReceipt(t *testing.T, h *testkit.Harness, p access.Principal, mutationID string) (outcome []byte, found bool) {
	t.Helper()
	tx := TenantTx(t, h.Runtime, p.AccountID, h.FamilyID)
	defer func() { _ = tx.Rollback(t.Context()) }()
	rows, err := tx.Query(t.Context(), `SELECT outcome::text FROM care_receipts WHERE device_id=$1 AND mutation_id=$2`, p.DeviceID, mutationID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		outcome, found = []byte(raw), true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return outcome, found
}

// DeleteBaby is the delete of baby id (observed version 1).
func DeleteBaby(id string) protocol.Mutation {
	raw := fmt.Sprintf(`{"mutation_id":"0d%s","entity_id":"%s","entity_kind":"baby","operation":"delete","payload_version":1,
		"observed_entity_version":1,"client_mutation_time":"2026-09-20T15:00:00Z","import_id":null,"payload":{}}`, id[2:], id)
	m, err := protocol.DecodeMutation([]byte(raw))
	if err != nil {
		panic(err)
	}
	return m
}

// GrowthProfile is the create of baby's growth profile.
func GrowthProfile(baby string) protocol.Mutation {
	raw := fmt.Sprintf(`{"mutation_id":"09000000-0000-4000-8000-%s","entity_id":"%s","entity_kind":"growth_profile","operation":"create",
		"payload_version":1,"observed_entity_version":0,"client_mutation_time":"2026-09-20T14:10:00Z","import_id":null,
		"payload":{"baby_id":"%s","reference_sex":"girls","expected_due_date":null}}`, baby[24:], protocol.GrowthProfileID(baby), baby)
	m, err := protocol.DecodeMutation([]byte(raw))
	if err != nil {
		panic(err)
	}
	return m
}

// Timers and completions the timer builders use.
const (
	TimerA      = "50000000-0000-4000-8000-0000000000a1"
	TimerB      = "50000000-0000-4000-8000-0000000000b1"
	CompletionA = "21000000-0000-4000-8000-0000000000a1"
)

// TimerAt is when the timer builders start their timers: an hour before
// testkit.FixtureClock (2026-09-21T12:00:00Z). A test whose timer rules
// read the service clock runs on testkit.NewAtFixtureClock.
const TimerAt = "2026-09-21T11:00:00.000000Z"

// TimerCommand is the n-th command on timer (mutation id
// 05nnnnnn-0000-4000-8000-<timer's last 12 digits>) at the timer payload
// version, as the server receives it.
func TimerCommand(timer string, n int, operation, clientTime string, observed uint64, payload protocol.Payload) protocol.Mutation {
	return canonical(protocol.Mutation{MutationID: fmt.Sprintf("05%06d-0000-4000-8000-%s", n, timer[24:]), EntityID: timer, EntityKind: "timer",
		Operation: operation, PayloadVersion: protocol.CurrentPayloadVersion("timer"), ObservedEntityVersion: observed,
		ClientMutationTime: clientTime, Payload: payload})
}

// StartTimer is the start of timer for baby at TimerAt: a feed timer with
// its left side running, or a sleep timer.
func StartTimer(baby, timer, kind string) protocol.Mutation {
	p := protocol.TimerStartPayload{BabyID: baby, TimerKind: kind, StartedAt: TimerAt, TimeZoneID: zoneID, CompletedComponents: []protocol.FeedComponent{}}
	if kind == "feed" {
		left, at := "left", TimerAt
		p.CurrentSide, p.CurrentSideStartedAt = &left, &at
	}
	return TimerCommand(timer, 1, "timer_start", TimerAt, 0, p)
}

// derivedID is a version 4 UUID derived from parts, the same for the same
// parts.
func derivedID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "/")))
	h := hex.EncodeToString(sum[:16])
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-8" + h[17:20] + "-" + h[20:32]
}

// Stop is the stop of timer at 11:10 into completion, under a mutation id
// derived from the timer and the completion; a running side becomes the
// part 3c…<completion>.
func Stop(baby, timer, completion string) protocol.Mutation {
	return canonical(protocol.Mutation{MutationID: derivedID("stop", timer, completion), EntityID: timer, EntityKind: "timer", Operation: "timer_stop",
		PayloadVersion: protocol.CurrentPayloadVersion("timer"), ClientMutationTime: "2026-09-21T11:10:00.000000Z",
		Payload: protocol.TimerStopPayload{BabyID: baby, CompletedRecordID: completion, StoppedAt: "2026-09-21T11:10:00.000000Z",
			ComponentID: "3c" + completion[2:]}})
}

// FeedTimerLifecycle is a timed bottle on one feed timer: the start with
// the bottle running (11:00), the bottle finished at 11:05 (90 ml of
// formula), an edit of that bottle (95 ml of breast milk) and the stop at
// 11:10 into completion.
func FeedTimerLifecycle(baby, timer, completion string) (start, finish, edit, stop protocol.Mutation) {
	bottleAt, finishedAt := TimerAt, "2026-09-21T11:05:00.000000Z"
	start = TimerCommand(timer, 1, "timer_start", TimerAt, 0, protocol.TimerStartPayload{BabyID: baby, TimerKind: "feed", StartedAt: TimerAt,
		TimeZoneID: zoneID, CurrentBottleStartedAt: &bottleAt, CompletedComponents: []protocol.FeedComponent{}})
	part := "36" + timer[2:]
	formula, milk := "formula", "breast_milk"
	finish = TimerCommand(timer, 2, "timer_finish_bottle", finishedAt, 1, protocol.TimerBottleFinishPayload{BabyID: baby, FinishedAt: finishedAt,
		ComponentID: part, VolumeML: protocol.MustParseDecimal("90"), Contents: &formula})
	seconds := uint32(300)
	edit = TimerCommand(timer, 3, "update", "2026-09-21T11:07:00.000000Z", 2, protocol.TimerEditPayload{BabyID: baby,
		Components: []protocol.FeedComponent{{ID: part, Kind: "bottle", Seconds: &seconds, VolumeML: protocol.MustParseDecimal("95"), Contents: &milk,
			StartedAt: &bottleAt, EndedAt: &finishedAt}}})
	stop = Stop(baby, timer, completion)
	stop.MutationID, stop.ObservedEntityVersion = fmt.Sprintf("05%06d-0000-4000-8000-%s", 4, timer[24:]), 3
	return start, finish, edit, stop
}

// Babies of MillisecondFamily.
const (
	BabyC = "0b000000-0000-4000-8000-0000000000c3"
	BabyD = "0d000000-0000-4000-8000-0000000000d4"
)

// MillisecondFamily is two more babies whose records and timers carry
// millisecond timestamps, as the phones send them, decoded as the server
// receives them, for the owner to apply in this order:
//   - baby C (sort order 2, its id before the golden babies'): a bottle feed
//     before local midnight and two linked to its session the next morning,
//     a timed breast feed, a sleep across local midnight and one linked to
//     it, an unfinished sleep, a diaper, supplements named "vitamin D",
//     "Zinc" and "Vitamin D", two measurements, a running feed timer with a
//     finished breast part and its bottle running, and a running sleep
//     timer whose id sorts before the feed timer's;
//   - baby D (sort order 1): a feed timer whose bottle is finished and which
//     keeps running;
//   - then the deletes of the bottle feeds' session root and of its earliest
//     linked member, so the session's earliest live member is the second
//     linked feed.
//
// The decoder normalizes record times, a timer's start and its running
// side's start, but keeps a running bottle's start and a timer part's
// bounds as sent (".250Z"); a bottle finish keeps its finish time as sent.
func MillisecondFamily() []protocol.Mutation {
	id := func(prefix string, n int) string { return fmt.Sprintf("%s000000-0000-4000-8000-%012d", prefix, n) }
	mutation := func(entity, kind, operation string, observed int, client, payload string) protocol.Mutation {
		// The mutation id keeps the entity id's leading digits: 0e2c0000-… for 2c000000-….
		raw := fmt.Sprintf(`{"mutation_id":"0e%s%s","entity_id":"%s","entity_kind":"%s","operation":"%s","payload_version":%d,
			"observed_entity_version":%d,"client_mutation_time":"%s","import_id":null,"payload":%s}`,
			entity[:6], entity[8:], entity, kind, operation, protocol.CurrentPayloadVersion(kind), observed, client, payload)
		m, err := protocol.DecodeMutation([]byte(raw))
		if err != nil {
			panic(fmt.Sprintf("caretest: %s %s %s: %v", kind, operation, entity, err))
		}
		return m
	}
	baby := func(entity, name string, sortOrder int) protocol.Mutation {
		return mutation(entity, "baby", "create", 0, "2026-09-19T08:00:00.001Z",
			fmt.Sprintf(`{"name":%q,"birth_date":"2026-03-03","sort_order":%d}`, name, sortOrder))
	}
	feed := func(n int, kind, start, end, session, components string) protocol.Mutation {
		return mutation(id("2c", n), "feed", "create", 0, start, fmt.Sprintf(`{"baby_id":"%s","kind":"%s","started_at":"%s","ended_at":%s,
			"time_zone_id":"Europe/Zurich","note":null,"components":%s,"feeding_session_id":%s}`, BabyC, kind, start, end, components, session))
	}
	bottle := func(n int, volume string) string {
		return fmt.Sprintf(`[{"id":"%s","kind":"bottle","volume_ml":%s,"contents":"formula","seconds":null,"started_at":null,"ended_at":null}]`,
			id("31", 900+n), volume)
	}
	sleep := func(n int, start, end, session string) protocol.Mutation {
		return mutation(id("3c", n), "sleep", "create", 0, start, fmt.Sprintf(`{"baby_id":"%s","started_at":"%s","ended_at":%s,
			"time_zone_id":"Europe/Zurich","note":null,"kind_override":null,"sleep_session_id":%s}`, BabyC, start, end, session))
	}
	supplement := func(n int, name, at string) protocol.Mutation {
		return mutation(id("6c", n), "supplement", "create", 0, at, fmt.Sprintf(`{"baby_id":"%s","started_at":"%s",
			"time_zone_id":"Europe/Zurich","note":null,"name":%q,"logged_at":"%s"}`, BabyC, at, name, at))
	}
	measurement := func(n int, at, values string) protocol.Mutation {
		return mutation(id("7c", n), "measurement", "create", 0, at, fmt.Sprintf(`{"baby_id":"%s","measured_at":"%s","logged_at":"%s",
			"time_zone_id":"Europe/Zurich",%s}`, BabyC, at, at, values))
	}
	quoted := func(value string) string { return `"` + value + `"` }
	timer := func(entity, babyID, kind, start, parts, bottleStart string) protocol.Mutation {
		return mutation(entity, "timer", "timer_start", 0, start, fmt.Sprintf(`{"baby_id":"%s","timer_kind":"%s","started_at":"%s",
			"time_zone_id":"Europe/Zurich","completed_components":%s,"current_side":null,"current_side_started_at":null,
			"current_bottle_started_at":%s,"session_id":null}`, babyID, kind, start, parts, bottleStart))
	}
	remove := func(entity string) protocol.Mutation {
		raw := fmt.Sprintf(`{"mutation_id":"0d%s%s","entity_id":"%s","entity_kind":"feed","operation":"delete","payload_version":1,
			"observed_entity_version":1,"client_mutation_time":"2026-09-21T11:40:00.500Z","import_id":null,"payload":{"baby_id":"%s"}}`,
			entity[:6], entity[8:], entity, BabyC)
		m, err := protocol.DecodeMutation([]byte(raw))
		if err != nil {
			panic(fmt.Sprintf("caretest: delete %s: %v", entity, err))
		}
		return m
	}
	finish := mutation(id("5d", 1), "timer", "timer_finish_bottle", 1, "2026-09-21T11:20:00.789Z", fmt.Sprintf(`{"baby_id":"%s",
		"finished_at":"2026-09-21T11:20:00.789Z","component_id":"%s","volume_ml":75,"contents":"breast_milk"}`, BabyD, id("3d", 2)))
	finish.MutationID = "0f" + finish.MutationID[2:]
	return []protocol.Mutation{
		baby(BabyC, "Chloé", 2),
		baby(BabyD, "Dan", 1),
		feed(1, "bottle", "2026-09-19T21:30:00.250Z", quoted("2026-09-19T21:45:00.250Z"), "null", bottle(1, "60")),
		feed(2, "bottle", "2026-09-20T06:00:00.125Z", "null", quoted(id("2c", 1)), bottle(2, "45.5")),
		feed(4, "bottle", "2026-09-20T07:30:00.875Z", "null", quoted(id("2c", 1)), bottle(4, "30")),
		feed(3, "breast", "2026-09-20T09:00:00.500Z", quoted("2026-09-20T09:12:00.500Z"), "null", fmt.Sprintf(
			`[{"id":"%s","kind":"breast","side":"left","seconds":300,"started_at":"2026-09-20T09:00:00.500Z","ended_at":"2026-09-20T09:05:00.500Z"},
			  {"id":"%s","kind":"breast","side":"right","seconds":420,"started_at":"2026-09-20T09:05:00.500Z","ended_at":"2026-09-20T09:12:00.500Z"}]`,
			id("31", 931), id("31", 932))),
		sleep(1, "2026-09-19T20:00:00.333Z", quoted("2026-09-20T05:00:00.777Z"), "null"),
		sleep(2, "2026-09-20T05:30:00.000Z", quoted("2026-09-20T06:00:00.001Z"), quoted(id("3c", 1))),
		sleep(3, "2026-09-20T22:00:00.900Z", "null", "null"),
		mutation(id("4c", 1), "diaper", "create", 0, "2026-09-20T07:00:00.010Z", fmt.Sprintf(`{"baby_id":"%s","started_at":"2026-09-20T07:00:00.010Z",
			"ended_at":null,"time_zone_id":"Europe/Zurich","note":"a <b> & c","contents":"wet","consistency":null,"color":null}`, BabyC)),
		supplement(1, "vitamin D", "2026-09-20T08:00:00.100Z"),
		supplement(2, "Zinc", "2026-09-20T09:00:00.200Z"),
		supplement(3, "Vitamin D", "2026-09-20T10:00:00.300Z"),
		measurement(1, "2026-09-20T12:00:00.456Z", `"weight_kg":7.1,"length_cm":null,"head_circumference_cm":null,"length_position":null`),
		measurement(2, "2026-09-21T10:00:00.000Z", `"weight_kg":null,"length_cm":66,"head_circumference_cm":41.25,"length_position":"lying"`),
		timer(id("5c", 2), BabyC, "feed", "2026-09-21T10:55:00.123Z", fmt.Sprintf(
			`[{"id":"%s","kind":"breast","side":"left","seconds":120,"started_at":"2026-09-21T10:55:00.250Z","ended_at":"2026-09-21T10:57:00.250Z"}]`,
			id("3c", 901)), quoted("2026-09-21T11:00:00.456Z")),
		timer(id("5c", 1), BabyC, "sleep", "2026-09-21T11:30:00.654Z", "[]", "null"),
		timer(id("5d", 1), BabyD, "feed", "2026-09-21T11:10:00.321Z", "[]", quoted("2026-09-21T11:10:00.321Z")),
		finish,
		remove(id("2c", 1)), // the session root
		remove(id("2c", 2)), // its earliest linked member
	}
}
