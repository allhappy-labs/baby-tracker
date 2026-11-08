package care_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/care"
	"babytracker/server/internal/protocol"
)

const (
	familyID = "f0000000-0000-4000-8000-0000000000f1"
	babyA    = "10000000-0000-4000-8000-000000000001"
	accountA = "a0000000-0000-4000-8000-0000000000a1"
	deviceA  = "d0000000-0000-4000-8000-0000000000d1"
)

var corpusAccepted = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

type corpusChange struct {
	Cursor              protocol.Cursor `json:"cursor"`
	EntityKind          string          `json:"entity_kind"`
	EntityID            string          `json:"entity_id"`
	EntityVersion       uint64          `json:"entity_version"`
	ActorAccountID      string          `json:"actor_account_id"`
	DeviceID            string          `json:"device_id"`
	Operation           string          `json:"operation"`
	AcceptedAt          string          `json:"accepted_at"`
	SnapshotOrTombstone json.RawMessage `json:"snapshot_or_tombstone"`
}

type corpus struct {
	FullPull struct {
		Changes []json.RawMessage `json:"changes"`
	} `json:"full_pull"`
	MidPull struct {
		Changes []json.RawMessage `json:"changes"`
	} `json:"mid_pull"`
	Snapshot struct {
		Entities []json.RawMessage `json:"entities"`
	} `json:"snapshot"`
}

// loadCorpus reads Task 1's legacy sync corpus. Its values are indented by
// the golden writer; compacting them restores the served bytes, escapes
// included (the writer never re-escapes).
func loadCorpus(t *testing.T) corpus {
	t.Helper()
	raw, err := os.ReadFile("../sync/testdata/golden/legacy-sync.json")
	if err != nil {
		t.Fatal(err)
	}
	var c corpus
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	for _, values := range [][]json.RawMessage{c.FullPull.Changes, c.MidPull.Changes, c.Snapshot.Entities} {
		for index, value := range values {
			values[index] = compact(t, value)
		}
	}
	if len(c.FullPull.Changes) != 37 || len(c.Snapshot.Entities) != 23 {
		t.Fatalf("corpus has %d changes and %d entities", len(c.FullPull.Changes), len(c.Snapshot.Entities))
	}
	return c
}

func compact(t *testing.T, value []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, value); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func decodeEntity(t *testing.T, raw []byte) protocol.CanonicalEntity {
	t.Helper()
	var entity protocol.CanonicalEntity
	if err := json.Unmarshal(raw, &entity); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return entity
}

func change(operation string) care.Change {
	return care.Change{Seq: 41, ActorAccountID: accountA, DeviceID: deviceA, Operation: operation, AcceptedAt: corpusAccepted}
}

func at(value string) *time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		panic(err)
	}
	return &parsed
}

func ptr[T any](value T) *T { return &value }

// typed is the comparable projection of a row's typed and derived columns.
type typed struct {
	BabyID, SessionID, TimerKind, Label *string
	StartedAt, EndedAt                  *time.Time
	SortOrder                           *int32
	NameHash                            []byte
	BreastSeconds                       int32
	BottleVolumeML                      string
	BottleObservations, BottleMissing   int16
	ShortestPartSeconds                 *int32
}

func project(row care.Row) typed {
	volume := "null"
	if row.Derived.BottleVolumeML != nil {
		volume = row.Derived.BottleVolumeML.String()
	}
	return typed{BabyID: row.BabyID, SessionID: row.SessionID, TimerKind: row.TimerKind, Label: row.Label, StartedAt: row.StartedAt,
		EndedAt: row.EndedAt, SortOrder: row.SortOrder, NameHash: row.NameHash, BreastSeconds: row.Derived.BreastSeconds,
		BottleVolumeML: volume, BottleObservations: row.Derived.BottleObservations, BottleMissing: row.Derived.BottleMissing,
		ShortestPartSeconds: row.Derived.ShortestPartSeconds}
}

func equalTyped(a, b typed) bool {
	sameString := func(x, y *string) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	sameTime := func(x, y *time.Time) bool { return (x == nil) == (y == nil) && (x == nil || x.Equal(*y)) }
	sameInt := func(x, y *int32) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return sameString(a.BabyID, b.BabyID) && sameString(a.SessionID, b.SessionID) && sameString(a.TimerKind, b.TimerKind) &&
		sameString(a.Label, b.Label) && sameTime(a.StartedAt, b.StartedAt) && sameTime(a.EndedAt, b.EndedAt) &&
		sameInt(a.SortOrder, b.SortOrder) && bytes.Equal(a.NameHash, b.NameHash) && a.BreastSeconds == b.BreastSeconds &&
		a.BottleVolumeML == b.BottleVolumeML && a.BottleObservations == b.BottleObservations && a.BottleMissing == b.BottleMissing &&
		sameInt(a.ShortestPartSeconds, b.ShortestPartSeconds)
}

func TestKindCodesMatchSchema(t *testing.T) {
	// The baseline states every code; 000033 restated the two care-kind
	// comments when it added immunization, 000034 when it added pump. The
	// latest comment on a column wins.
	var raw []byte
	for _, name := range []string{"000032_baseline.sql", "000033_immunizations.sql", "000034_pumping.sql"} {
		part, err := os.ReadFile("../../migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, part...)
	}
	comment := func(column, label string) map[int16]string {
		all := regexp.MustCompile(`COMMENT ON COLUMN public\.`+regexp.QuoteMeta(column)+` IS '`+label+`: ([^']*)'`).FindAllSubmatch(raw, -1)
		if all == nil {
			t.Fatalf("no comment on %s", column)
		}
		match := all[len(all)-1]
		codes := map[int16]string{}
		for _, pair := range strings.Split(string(match[1]), ", ") {
			code, name, _ := strings.Cut(pair, " ")
			value, err := strconv.ParseInt(code, 10, 16)
			if err != nil {
				t.Fatalf("%s: %q", column, pair)
			}
			codes[int16(value)] = name
		}
		return codes
	}
	kinds := comment("care_entities.kind", "care kind")
	if resume := comment("sync_page_bindings.resume_kind", "care kind"); !maps.Equal(resume, kinds) {
		t.Fatalf("resume_kind comment %v differs from kind comment %v", resume, kinds)
	}
	if len(kinds) != 10 {
		t.Fatalf("schema names %d care kinds", len(kinds))
	}
	for code, name := range kinds {
		if got, ok := care.KindCode(name); !ok || got != code {
			t.Errorf("KindCode(%q) = %d %v, schema %d", name, got, ok, code)
		}
		if got, ok := care.KindName(code); !ok || got != name {
			t.Errorf("KindName(%d) = %q %v, schema %q", code, got, ok, name)
		}
	}
	constants := map[int16]string{care.KindBaby: "baby", care.KindFeed: "feed", care.KindSleep: "sleep", care.KindDiaper: "diaper",
		care.KindTimer: "timer", care.KindSupplement: "supplement", care.KindGrowthProfile: "growth_profile", care.KindMeasurement: "measurement", care.KindImmunization: "immunization", care.KindPump: "pump"}
	if len(constants) != 10 {
		t.Fatalf("care kind constants collide: %v", constants)
	}
	for code, name := range constants {
		if kinds[code] != name {
			t.Errorf("constant %s = %d, schema names %d %q", name, code, code, kinds[code])
		}
	}
	timerKinds := comment("care_entities.timer_kind", "timer kind")
	if len(timerKinds) != 2 || timerKinds[care.TimerKindFeed] != "feed" || timerKinds[care.TimerKindSleep] != "sleep" {
		t.Fatalf("timer kind constants feed=%d sleep=%d, schema %v", care.TimerKindFeed, care.TimerKindSleep, timerKinds)
	}
	for code, name := range timerKinds {
		if got, ok := care.TimerKindCode(name); !ok || got != code {
			t.Errorf("TimerKindCode(%q) = %d %v, schema %d", name, got, ok, code)
		}
		if got, ok := care.TimerKindName(code); !ok || got != name {
			t.Errorf("TimerKindName(%d) = %q %v, schema %q", code, got, ok, name)
		}
	}
	for _, unknown := range []string{"", "Baby", "active_timer", "feed_component"} {
		if _, ok := care.KindCode(unknown); ok {
			t.Errorf("KindCode(%q) accepted", unknown)
		}
	}
	for _, code := range []int16{0, 11, -1} {
		if _, ok := care.KindName(code); ok {
			t.Errorf("KindName(%d) accepted", code)
		}
	}
}

func TestEncodeWireIsCanonicalMarshal(t *testing.T) {
	c := loadCorpus(t)
	nameHash := func(name, birth string) []byte {
		hash, err := care.BabyNameHash(name, birth)
		if err != nil {
			t.Fatal(err)
		}
		return hash[:]
	}
	baby := ptr(babyA)
	// The typed and derived columns each live corpus entity must get.
	want := map[string]typed{
		babyA:                                  {SortOrder: ptr(int32(0)), NameHash: nameHash("Élie", "2026-01-12"), BottleVolumeML: "null"},
		"20000000-0000-4000-8000-000000000001": {BabyID: baby, StartedAt: at("2026-09-20T12:00:00Z"), BottleVolumeML: "1", BottleObservations: 1},
		"20000000-0000-4000-8000-000000000002": {BabyID: baby, StartedAt: at("2026-09-20T12:00:00Z"), EndedAt: at("2026-09-20T12:10:00Z"),
			BreastSeconds: 600, BottleVolumeML: "null", ShortestPartSeconds: ptr(int32(300))},
		"20000000-0000-4000-8000-000000000003": {BabyID: baby, StartedAt: at("2026-09-20T13:00:00Z"), BottleVolumeML: "null"},
		"20000000-0000-4000-8000-000000000004": {BabyID: baby, StartedAt: at("2026-09-20T13:00:00Z"), EndedAt: at("2026-09-20T13:10:00Z"),
			BreastSeconds: 300, BottleVolumeML: "60", BottleObservations: 1, ShortestPartSeconds: ptr(int32(300))},
		"20000000-0000-4000-8000-000000000060": {BabyID: baby, StartedAt: at("2026-09-20T14:58:00Z"), EndedAt: at("2026-09-20T15:10:00Z"),
			BottleVolumeML: "95", BottleObservations: 1, ShortestPartSeconds: ptr(int32(300))},
		"20000000-0000-4000-8000-0000000000c1": {BabyID: baby, StartedAt: at("2026-09-20T18:00:00Z"), EndedAt: at("2026-09-20T18:10:00Z"),
			BreastSeconds: 600, BottleVolumeML: "null", ShortestPartSeconds: ptr(int32(300))},
		"30000000-0000-4000-8000-000000000001": {BabyID: baby, StartedAt: at("2026-09-20T13:00:00Z"), EndedAt: at("2026-09-20T14:00:00Z"), BottleVolumeML: "null"},
		"30000000-0000-4000-8000-000000000070": {BabyID: baby, StartedAt: at("2026-09-20T16:00:00Z"), EndedAt: at("2026-09-20T16:45:00Z"), BottleVolumeML: "null"},
		"50000000-0000-4000-8000-000000000003": {BabyID: baby, StartedAt: at("2026-09-20T19:00:00Z"), TimerKind: ptr("feed"),
			BottleVolumeML: "null", ShortestPartSeconds: ptr(int32(120))},
		"50000000-0000-4000-8000-000000000004": {BabyID: baby, StartedAt: at("2026-09-20T20:00:00Z"), TimerKind: ptr("sleep"), BottleVolumeML: "null"},
		"60000000-0000-4000-8000-000000000001": {BabyID: baby, StartedAt: at("2026-09-20T14:10:00Z"), Label: ptr("Vitamin D"), BottleVolumeML: "null"},
		"2742cf11-d3ad-51bb-bbfa-24147f4b0aae": {BabyID: baby, BottleVolumeML: "null"},
		"70000000-0000-4000-8000-000000000001": {BabyID: baby, StartedAt: at("2026-09-20T14:10:00Z"), BottleVolumeML: "null"},
	}
	// A tombstone keeps whatever typed columns its previous row had and
	// drops every derived value.
	previous := care.Row{BabyID: ptr("10000000-0000-4000-8000-0000000000b2"), StartedAt: at("2026-09-20T17:00:00Z"),
		EndedAt: at("2026-09-20T17:30:00Z"), SessionID: ptr("90000000-0000-4000-8000-000000000001"), TimerKind: ptr("feed"),
		SortOrder: ptr(int32(4)), Label: ptr("kept"), NameHash: bytes.Repeat([]byte{7}, 32),
		Derived: care.Derived{BreastSeconds: 60, BottleVolumeML: protocol.MustParseDecimal("5"), BottleObservations: 1, ShortestPartSeconds: ptr(int32(60))}}
	keptTyped := project(previous)
	keptTyped.BreastSeconds, keptTyped.BottleVolumeML, keptTyped.BottleObservations, keptTyped.ShortestPartSeconds = 0, "null", 0, nil

	kinds, live, tombstones := map[string]bool{}, 0, 0
	for _, raw := range c.Snapshot.Entities {
		entity := decodeEntity(t, raw)
		kinds[entity.EntityKind] = true
		var prior *care.Row
		operation := "update"
		if entity.Meta.TombstonedAt != nil {
			prior, operation = &previous, "delete"
			if _, err := care.Encode(entity, nil, change(operation)); err == nil {
				t.Fatalf("%s: tombstone without its previous row encoded", entity.Meta.ID)
			}
		}
		row, err := care.Encode(entity, prior, change(operation))
		if err != nil {
			t.Fatalf("encode %s: %v", entity.Meta.ID, err)
		}
		wire, err := json.Marshal(entity)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(row.Wire, wire) || row.WireBytes != len(wire) {
			t.Fatalf("%s: wire %s (%d bytes), want json.Marshal %s", entity.Meta.ID, row.Wire, row.WireBytes, wire)
		}
		decoded, err := row.Entity()
		if err != nil {
			t.Fatalf("%s: Entity: %v", entity.Meta.ID, err)
		}
		if again, err := json.Marshal(decoded); err != nil || !bytes.Equal(again, row.Wire) {
			t.Fatalf("%s: Entity re-encodes to %s, want %s (%v)", entity.Meta.ID, again, row.Wire, err)
		}
		if row.FamilyID != familyID || row.ID != entity.Meta.ID || row.Kind != entity.EntityKind || row.EntityVersion != entity.Meta.EntityVersion ||
			row.CreatorAccountID != entity.Meta.CreatorAccountID || row.ChangedSeq != 41 || row.ActorAccountID != accountA ||
			row.DeviceID != deviceA || row.Operation != operation || !row.AcceptedAt.Equal(corpusAccepted) || row.CompletedRecordID != nil {
			t.Fatalf("%s: envelope columns %+v", entity.Meta.ID, row)
		}
		if entity.Meta.TombstonedAt != nil {
			tombstones++
			if row.Live() || row.TombstonedAt == nil || !row.TombstonedAt.Equal(corpusAccepted) || !equalTyped(project(row), keptTyped) {
				t.Fatalf("%s: tombstone columns %+v, want %+v", entity.Meta.ID, project(row), keptTyped)
			}
			continue
		}
		live++
		expected, ok := want[entity.Meta.ID]
		if !ok {
			t.Fatalf("no expectation for live %s %s", entity.EntityKind, entity.Meta.ID)
		}
		if !row.Live() || row.TombstonedAt != nil || !equalTyped(project(row), expected) {
			t.Fatalf("%s %s: typed columns\n got %+v\nwant %+v", entity.EntityKind, entity.Meta.ID, project(row), expected)
		}
	}
	if len(kinds) != 8 || live != len(want) || tombstones == 0 {
		t.Fatalf("covered kinds %v, %d live (want %d), %d tombstones", kinds, live, len(want), tombstones)
	}
}

// Every entity the legacy server served, encoded once, is the same bytes:
// snapshot entities and change entities. Each change envelope built around
// the stored wire (protocol.EncodeChange) is the served change byte for byte.
func TestEncodeReproducesLegacyCorpus(t *testing.T) {
	c := loadCorpus(t)
	previous := &care.Row{BabyID: ptr(babyA)}
	check := func(where string, raw []byte, operation string) care.Row {
		t.Helper()
		row, err := care.Encode(decodeEntity(t, raw), previous, change(operation))
		if err != nil {
			t.Fatalf("%s: %v", where, err)
		}
		if !bytes.Equal(row.Wire, raw) {
			t.Fatalf("%s:\n got %s\nwant %s", where, row.Wire, raw)
		}
		return row
	}
	for index, raw := range c.Snapshot.Entities {
		check("snapshot entity "+strconv.Itoa(index), raw, "update")
	}
	for name, changes := range map[string][]json.RawMessage{"full_pull": c.FullPull.Changes, "mid_pull": c.MidPull.Changes} {
		for index, raw := range changes {
			var envelope corpusChange
			if err := json.Unmarshal(raw, &envelope); err != nil {
				t.Fatal(err)
			}
			where := name + " change " + strconv.Itoa(index)
			row := check(where, envelope.SnapshotOrTombstone, envelope.Operation)
			served, err := protocol.EncodeChange(envelope.Cursor, envelope.EntityKind, envelope.EntityID, envelope.EntityVersion,
				envelope.ActorAccountID, envelope.DeviceID, envelope.Operation, envelope.AcceptedAt, row.Wire)
			if err != nil || !bytes.Equal(served, raw) {
				t.Fatalf("%s envelope (%v):\n got %s\nwant %s", where, err, served, raw)
			}
		}
	}
}

// Review Focus 3: text with HTML-significant characters, line and paragraph
// separators, quotes, backslashes, emoji and accents is stored as
// json.Marshal writes it and served verbatim inside the HTTP encoder's
// output; the bytes equal today's decode-then-encode.
func TestWireServedVerbatimEscapesLikeEncoder(t *testing.T) {
	// Raw JSON as a client could send it: `e\u0301` is a decomposed é, which
	// decoding normalises to NFC (U+00E9).
	const hostile = `<b>&amp;</b> \u2028 line \u2029 para \"quoted\" back\\slash 🍼😴💊 Zoe\u0301 ✅ </script>`
	sources := []string{
		`{"entity_kind":"baby","meta":{"id":"10000000-0000-4000-8000-0000000000e1","family_id":"` + familyID + `","entity_version":1,"creator_account_id":"` + accountA + `","last_editor_account_id":"` + accountA + `","client_mutation_time":"2026-09-20T12:00:00.000000Z","accepted_at":"2026-09-21T12:00:00.000000Z","tombstoned_at":null},"payload":{"name":"Zoe\u0301 <i>&</i> \u2028🍼","birth_date":"2026-01-12","sort_order":3}}`,
		`{"entity_kind":"feed","meta":{"id":"20000000-0000-4000-8000-0000000000e1","family_id":"` + familyID + `","entity_version":2,"creator_account_id":"` + accountA + `","last_editor_account_id":"` + accountA + `","client_mutation_time":"2026-09-20T12:00:00.000000Z","accepted_at":"2026-09-21T12:00:00.000000Z","tombstoned_at":null},"payload":{"baby_id":"` + babyA + `","kind":"solids","started_at":"2026-09-20T12:00:00.000000Z","ended_at":null,"time_zone_id":"Europe/Zurich","note":"` + hostile + `","components":[{"id":"31000000-0000-4000-8000-0000000000e1","kind":"solids","foods":["pomme <crue> & poire","cre\u0300me \u2029"],"amount":"some"}],"feeding_session_id":null}}`,
		`{"entity_kind":"sleep","meta":{"id":"30000000-0000-4000-8000-0000000000e1","family_id":"` + familyID + `","entity_version":1,"creator_account_id":"` + accountA + `","last_editor_account_id":"` + accountA + `","client_mutation_time":"2026-09-20T12:00:00.000000Z","accepted_at":"2026-09-21T12:00:00.000000Z","tombstoned_at":null},"payload":{"baby_id":"` + babyA + `","started_at":"2026-09-20T12:00:00.000000Z","ended_at":"2026-09-20T13:00:00.000000Z","time_zone_id":"Europe/Zurich","note":"` + hostile + `","kind_override":"nap","sleep_session_id":null}}`,
		`{"entity_kind":"supplement","meta":{"id":"60000000-0000-4000-8000-0000000000e1","family_id":"` + familyID + `","entity_version":1,"creator_account_id":"` + accountA + `","last_editor_account_id":"` + accountA + `","client_mutation_time":"2026-09-20T12:00:00.000000Z","accepted_at":"2026-09-21T12:00:00.000000Z","tombstoned_at":null},"payload":{"baby_id":"` + babyA + `","started_at":"2026-09-20T12:00:00.000000Z","time_zone_id":"Europe/Zurich","name":"Fer <&> \u2028 💊","note":"` + hostile + `","logged_at":"2026-09-20T12:00:00.000000Z"}}`,
		`{"entity_kind":"diaper","meta":{"id":"40000000-0000-4000-8000-0000000000e1","family_id":"` + familyID + `","entity_version":1,"creator_account_id":"` + accountA + `","last_editor_account_id":"` + accountA + `","client_mutation_time":"2026-09-20T12:00:00.000000Z","accepted_at":"2026-09-21T12:00:00.000000Z","tombstoned_at":null},"payload":{"baby_id":"` + babyA + `","started_at":"2026-09-20T12:00:00.000000Z","ended_at":null,"time_zone_id":"Europe/Zurich","note":"` + hostile + `","contents":"wet","consistency":null,"color":null}}`,
	}
	server := protocol.ServerInfo{InstanceID: "50000000-0000-4000-8000-000000000001", RecoveryEpoch: 1}
	cursor := func(sequence uint64) protocol.Cursor {
		return protocol.Cursor{ServerInstanceID: server.InstanceID, RecoveryEpoch: 1, FamilyID: familyID, Sequence: sequence}
	}
	var decoded []protocol.CanonicalEntity
	var changes []protocol.JournalChange
	var encodedChanges, encodedEntities []json.RawMessage
	for index, source := range sources {
		entity := decodeEntity(t, []byte(source))
		row, err := care.Encode(entity, nil, change("create"))
		if err != nil {
			t.Fatalf("encode %s: %v", entity.EntityKind, err)
		}
		// Stored bytes carry no raw HTML-significant character or raw
		// separator, and hold the NFC é.
		for _, raw := range []string{"<", ">", "&", "\u2028", "\u2029"} {
			if bytes.Contains(row.Wire, []byte(raw)) {
				t.Fatalf("%s: stored wire holds a raw %q: %s", entity.EntityKind, raw, row.Wire)
			}
		}
		if !bytes.Contains(row.Wire, []byte("Zo\u00e9")) || bytes.Contains(row.Wire, []byte("e\u0301")) {
			t.Fatalf("%s: wire is not NFC: %s", entity.EntityKind, row.Wire)
		}
		// The stored wire is a fixed point of encoding/json's compaction
		// with HTML escaping, which is why the direct encoders
		// (protocol.AppendChange, SyncResponse.AppendJSON) append it
		// verbatim.
		if again, err := json.Marshal(json.RawMessage(row.Wire)); err != nil || !bytes.Equal(again, row.Wire) {
			t.Fatalf("%s: compaction changes the stored wire: %s (%v)", entity.EntityKind, again, err)
		}
		sequence := uint64(index + 1)
		journal := protocol.JournalChange{Cursor: cursor(sequence), EntityKind: entity.EntityKind, EntityID: entity.Meta.ID,
			EntityVersion: entity.Meta.EntityVersion, ActorAccountID: accountA, DeviceID: deviceA, Operation: "create",
			AcceptedAt: entity.Meta.AcceptedAt, SnapshotOrTombstone: entity}
		served, err := protocol.EncodeChange(journal.Cursor, journal.EntityKind, journal.EntityID, journal.EntityVersion,
			journal.ActorAccountID, journal.DeviceID, journal.Operation, journal.AcceptedAt, row.Wire)
		if err != nil {
			t.Fatal(err)
		}
		// Today the stored entity is decoded and encoded again.
		today := decodeEntity(t, row.Wire)
		journal.SnapshotOrTombstone = today
		decoded, changes = append(decoded, today), append(changes, journal)
		encodedChanges, encodedEntities = append(encodedChanges, served), append(encodedEntities, row.Wire)
	}
	encode := func(value any) []byte {
		var buffer bytes.Buffer
		if err := json.NewEncoder(&buffer).Encode(value); err != nil {
			t.Fatal(err)
		}
		return buffer.Bytes()
	}
	token := "next"
	highWater := cursor(uint64(len(sources)))
	verbatim := encode(protocol.SyncResponse{ServerInstanceID: server.InstanceID, RecoveryEpoch: 1, Outcomes: []protocol.Outcome{},
		EncodedChanges: encodedChanges, HighWater: highWater, NextPageToken: &token})
	todayBytes := encode(protocol.SyncResponse{ServerInstanceID: server.InstanceID, RecoveryEpoch: 1, Outcomes: []protocol.Outcome{},
		Changes: changes, HighWater: highWater, NextPageToken: &token})
	if !bytes.Equal(verbatim, todayBytes) {
		t.Fatalf("sync page served verbatim differs from today's encoding:\n got %s\nwant %s", verbatim, todayBytes)
	}
	// The HTTP layer's direct encoder writes the same bytes.
	direct, err := protocol.SyncResponse{ServerInstanceID: server.InstanceID, RecoveryEpoch: 1, Outcomes: []protocol.Outcome{},
		EncodedChanges: encodedChanges, HighWater: highWater, NextPageToken: &token}.AppendJSON(nil)
	if err != nil || !bytes.Equal(append(direct, '\n'), todayBytes) {
		t.Fatalf("sync page through AppendJSON differs from today's encoding (%v):\n got %s\nwant %s", err, direct, todayBytes)
	}
	for _, raw := range []string{"<", ">", "&", "\u2028", "\u2029"} {
		if bytes.Contains(verbatim, []byte(raw)) {
			t.Fatalf("served page holds a raw %q", raw)
		}
	}
	snapshotID := "80000000-0000-4000-8000-000000000001"
	verbatim = encode(protocol.SnapshotResponse{ServerInstanceID: server.InstanceID, RecoveryEpoch: 1, SnapshotID: snapshotID,
		HighWater: highWater, EncodedEntities: encodedEntities})
	todayBytes = encode(protocol.SnapshotResponse{ServerInstanceID: server.InstanceID, RecoveryEpoch: 1, SnapshotID: snapshotID,
		HighWater: highWater, Entities: decoded})
	if !bytes.Equal(verbatim, todayBytes) {
		t.Fatalf("snapshot page served verbatim differs from today's encoding:\n got %s\nwant %s", verbatim, todayBytes)
	}
	// A client decodes the verbatim page back to the same entities.
	var page protocol.SnapshotResponse
	if err := json.Unmarshal(verbatim, &page); err != nil || len(page.Entities) != len(decoded) {
		t.Fatalf("decode verbatim snapshot page: %d entities, %v", len(page.Entities), err)
	}
	for index := range decoded {
		again, _ := json.Marshal(page.Entities[index])
		if !bytes.Equal(again, encodedEntities[index]) {
			t.Fatalf("entity %d decodes to %s, stored %s", index, again, encodedEntities[index])
		}
	}
}

func TestDeriveFeed(t *testing.T) {
	seconds := func(value uint32) *uint32 { return &value }
	breast := func(value uint32) protocol.FeedComponent {
		return protocol.FeedComponent{Kind: "breast", Side: ptr("left"), Seconds: seconds(value)}
	}
	bottle := func(volume string, timed uint32) protocol.FeedComponent {
		component := protocol.FeedComponent{Kind: "bottle", Contents: ptr("formula")}
		if volume != "" {
			component.VolumeML = protocol.MustParseDecimal(volume)
		}
		if timed > 0 {
			component.Seconds = seconds(timed)
		}
		return component
	}
	solids := protocol.FeedComponent{Kind: "solids", Foods: []string{"pear"}, Amount: ptr("some")}
	for _, test := range []struct {
		name       string
		components []protocol.FeedComponent
		want       typed
	}{
		{"breast 600+300", []protocol.FeedComponent{breast(600), breast(300)}, typed{BreastSeconds: 900, BottleVolumeML: "null", ShortestPartSeconds: ptr(int32(300))}},
		{"bottles 120.5 and 89.5", []protocol.FeedComponent{bottle("120.5", 0), bottle("89.5", 0)}, typed{BottleVolumeML: "210", BottleObservations: 2}},
		{"bottle with no volume", []protocol.FeedComponent{bottle("12.25", 0), bottle("", 0)}, typed{BottleVolumeML: "12.25", BottleObservations: 1, BottleMissing: 1}},
		{"only missing volumes", []protocol.FeedComponent{bottle("", 0)}, typed{BottleVolumeML: "null", BottleMissing: 1}},
		{"zero volume is observed", []protocol.FeedComponent{bottle("0", 0)}, typed{BottleVolumeML: "0", BottleObservations: 1}},
		{"solids only", []protocol.FeedComponent{solids}, typed{BottleVolumeML: "null"}},
		{"none", nil, typed{BottleVolumeML: "null"}},
		{"shortest timed part", []protocol.FeedComponent{breast(400), bottle("30", 45), solids, breast(90)},
			typed{BreastSeconds: 490, BottleVolumeML: "30", BottleObservations: 1, ShortestPartSeconds: ptr(int32(45))}},
	} {
		got := care.DeriveFeed(test.components)
		row := care.Row{Derived: got}
		if !equalTyped(project(row), test.want) {
			t.Errorf("%s: %+v, want %+v", test.name, project(row), test.want)
		}
	}
}

func TestBabyNameHash(t *testing.T) {
	a, err := care.BabyNameHash("Élie", "2026-01-12")
	if err != nil {
		t.Fatal(err)
	}
	b, err := care.BabyNameHash(" e\u0301lie\u2028", "2026-01-12")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("equal normalized names hash differently")
	}
	other, err := care.BabyNameHash("Élie", "2026-01-13")
	if err != nil {
		t.Fatal(err)
	}
	if other == a {
		t.Fatal("a different birth date gives the same hash")
	}
	if len(a) != 32 {
		t.Fatalf("hash has %d bytes", len(a))
	}
	// The input layout is fixed: domain, normalization version, NUL,
	// normalized name, NUL, birth date.
	want := sha256.Sum256([]byte("baby-tracker-baby-match-v1\x00" + protocol.NormalizationVersion + "\x00élie\x002026-01-12"))
	if a != want {
		t.Fatalf("hash %x, want %x", a, want)
	}
	if _, err := care.BabyNameHash("  ", "2026-01-12"); !errors.Is(err, protocol.ErrInvalidText) {
		t.Fatalf("blank name: %v", err)
	}
}

func liveFeed(note string) protocol.CanonicalEntity {
	return protocol.CanonicalEntity{EntityKind: "feed", Meta: protocol.EntityMeta{ID: "20000000-0000-4000-8000-0000000000f1", FamilyID: familyID,
		EntityVersion: 1, CreatorAccountID: accountA, LastEditorAccountID: accountA, ClientMutationTime: "2026-09-20T12:00:00.000000Z",
		AcceptedAt: "2026-09-21T12:00:00.000000Z"},
		Payload: protocol.FeedPayload{BabyID: babyA, Kind: "breast", StartedAt: "2026-09-20T12:00:00.000000Z", TimeZoneID: "UTC", Note: &note,
			Components: []protocol.FeedComponent{{ID: "31000000-0000-4000-8000-0000000000f1", Kind: "breast", Side: ptr("left"), Seconds: ptr(uint32(60))}}}}
}

func TestEncodeRejectsWireAboveTheSchemaBound(t *testing.T) {
	small, err := care.Encode(liveFeed("x"), nil, change("create"))
	if err != nil {
		t.Fatal(err)
	}
	overhead := small.WireBytes - 1
	exact, err := care.Encode(liveFeed(strings.Repeat("x", care.MaxWireBytes-overhead)), nil, change("create"))
	if err != nil || exact.WireBytes != care.MaxWireBytes {
		t.Fatalf("a wire of exactly %d bytes: %d %v", care.MaxWireBytes, exact.WireBytes, err)
	}
	// One escaped byte more: "<" is written as six bytes.
	_, err = care.Encode(liveFeed(strings.Repeat("x", care.MaxWireBytes-overhead-5)+"<"), nil, change("create"))
	if !errors.Is(err, care.ErrWireTooLarge) || !errors.Is(err, protocol.ErrInvalidMutation) {
		t.Fatalf("oversize wire: %v", err)
	}
}

func TestEncodeRejectsShapesTheSchemaRefuses(t *testing.T) {
	feed := liveFeed("n")
	wrongPayload := feed
	wrongPayload.Payload = protocol.SleepPayload{BabyID: babyA, StartedAt: "2026-09-20T12:00:00.000000Z", TimeZoneID: "UTC"}
	noPayload := feed
	noPayload.Payload = nil
	unknown := feed
	unknown.EntityKind = "feed_component"
	badTime := feed
	badTime.Payload = protocol.FeedPayload{BabyID: babyA, Kind: "breast", StartedAt: "yesterday", TimeZoneID: "UTC"}
	completed := change("update")
	completed.CompletedRecordID = ptr("20000000-0000-4000-8000-000000000060")
	for name, test := range map[string]struct {
		entity protocol.CanonicalEntity
		change care.Change
	}{
		"payload of another kind":          {wrongPayload, change("update")},
		"live entity without payload":      {noPayload, change("update")},
		"unknown kind":                     {unknown, change("update")},
		"unparsable start":                 {badTime, change("update")},
		"completion on a live entity":      {feed, completed},
		"completion on a non-timer delete": {tombstone(feed), completed},
	} {
		if _, err := care.Encode(test.entity, &care.Row{BabyID: ptr(babyA)}, test.change); err == nil {
			t.Errorf("%s: encoded", name)
		}
	}
}

func tombstone(entity protocol.CanonicalEntity) protocol.CanonicalEntity {
	entity.Payload = nil
	entity.Meta.TombstonedAt = ptr("2026-09-21T12:00:00.000000Z")
	entity.Meta.EntityVersion++
	return entity
}

func TestRowAccounting(t *testing.T) {
	timerEntity := protocol.CanonicalEntity{EntityKind: "timer", Meta: liveFeed("").Meta,
		Payload: protocol.TimerStartPayload{BabyID: babyA, TimerKind: "feed", StartedAt: "2026-09-20T12:00:00.000000Z", TimeZoneID: "UTC",
			CompletedComponents: []protocol.FeedComponent{}}}
	feed, err := care.Encode(liveFeed("n"), nil, change("create"))
	if err != nil {
		t.Fatal(err)
	}
	timer, err := care.Encode(timerEntity, nil, change("timer_start"))
	if err != nil {
		t.Fatal(err)
	}
	stop := change("timer_stop")
	stop.CompletedRecordID = ptr("20000000-0000-4000-8000-000000000060")
	stopped, err := care.Encode(tombstone(timerEntity), &timer, stop)
	if err != nil {
		t.Fatal(err)
	}
	if *stopped.CompletedRecordID != *stop.CompletedRecordID || *stopped.TimerKind != "feed" {
		t.Fatalf("stopped timer %+v", stopped)
	}
	deleted, err := care.Encode(tombstone(liveFeed("n")), &feed, change("delete"))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		row     care.Row
		charged int64
		counts  bool
	}{
		{"live feed", feed, int64(feed.WireBytes) + care.RowOverheadBytes + care.LiveReserveBytes, true},
		{"deleted feed", deleted, int64(deleted.WireBytes) + care.RowOverheadBytes, false},
		{"live timer", timer, int64(timer.WireBytes) + care.RowOverheadBytes + care.LiveReserveBytes, false},
		{"stopped timer", stopped, int64(stopped.WireBytes) + care.RowOverheadBytes, false},
	} {
		if got := test.row.ChargedBytes(); got != test.charged {
			t.Errorf("%s charged %d, want %d", test.name, got, test.charged)
		}
		if got := test.row.CountsAsRecord(); got != test.counts {
			t.Errorf("%s counts as record %v, want %v", test.name, got, test.counts)
		}
	}
	// A row read without its wire charges by WireBytes alone.
	unloaded := feed
	unloaded.Wire = nil
	if unloaded.ChargedBytes() != feed.ChargedBytes() {
		t.Fatal("charge depends on the loaded wire")
	}
	babyRow := care.Row{Kind: "baby", WireBytes: 100}
	if !babyRow.Live() || babyRow.CountsAsRecord() {
		t.Fatal("a live baby is live and not a record")
	}
}

// The row's accepted_at and tombstoned_at are the entity meta's, whole
// microseconds, whatever sub-microsecond clock reading the change carries:
// the served change envelope (built from the row) equals the legacy one,
// whose accepted_at was the meta's.
func TestRowTimestampsComeFromTheMeta(t *testing.T) {
	const layout = "2006-01-02T15:04:05.000000Z"
	const meta = "2026-09-21T12:00:00.000001Z"
	want := time.Date(2026, 9, 21, 12, 0, 0, 1000, time.UTC)
	live := liveFeed("n")
	live.Meta.AcceptedAt = meta
	dead := tombstone(live)
	dead.Meta.AcceptedAt, dead.Meta.TombstonedAt = meta, ptr(meta)
	server := protocol.Cursor{ServerInstanceID: "50000000-0000-4000-8000-000000000001", RecoveryEpoch: 1, FamilyID: familyID, Sequence: 41}
	for _, test := range []struct {
		name  string
		clock time.Time
	}{
		{"no clock", time.Time{}},
		{"0.6 µs rounds half-even up", time.Date(2026, 9, 21, 12, 0, 0, 600, time.UTC)},
		{"1.4 µs rounds down", time.Date(2026, 9, 21, 12, 0, 0, 1400, time.UTC)},
		{"same instant in another zone", time.Date(2026, 9, 21, 14, 0, 0, 1000, time.FixedZone("CEST", 7200))},
	} {
		for _, entity := range []protocol.CanonicalEntity{live, dead} {
			c := change("update")
			if entity.Meta.TombstonedAt != nil {
				c.Operation = "delete"
			}
			c.AcceptedAt = test.clock
			row, err := care.Encode(entity, &care.Row{BabyID: ptr(babyA)}, c)
			if err != nil {
				t.Fatalf("%s: %v", test.name, err)
			}
			if !row.AcceptedAt.Equal(want) || row.AcceptedAt.Nanosecond() != 1000 || row.AcceptedAt.UTC().Format(layout) != meta {
				t.Fatalf("%s: accepted_at %s, want the meta's %s", test.name, row.AcceptedAt.Format(time.RFC3339Nano), meta)
			}
			if entity.Meta.TombstonedAt != nil && (row.TombstonedAt == nil || !row.TombstonedAt.Equal(want) || row.TombstonedAt.Nanosecond() != 1000) {
				t.Fatalf("%s: tombstoned_at %v, want the meta's %s", test.name, row.TombstonedAt, meta)
			}
			served, err := protocol.EncodeChange(server, row.Kind, row.ID, row.EntityVersion, row.ActorAccountID, row.DeviceID, row.Operation,
				row.AcceptedAt.UTC().Format(layout), row.Wire)
			if err != nil {
				t.Fatal(err)
			}
			legacy, err := json.Marshal(protocol.JournalChange{Cursor: server, EntityKind: entity.EntityKind, EntityID: entity.Meta.ID,
				EntityVersion: entity.Meta.EntityVersion, ActorAccountID: accountA, DeviceID: deviceA, Operation: c.Operation,
				AcceptedAt: entity.Meta.AcceptedAt, SnapshotOrTombstone: entity})
			if err != nil || !bytes.Equal(served, legacy) {
				t.Fatalf("%s: served %s\nlegacy %s (%v)", test.name, served, legacy, err)
			}
		}
	}
	// 1.5 µs and 0.5 µs round half-even to 2 µs and 0 µs: not the meta's 1 µs.
	for _, clock := range []time.Time{time.Date(2026, 9, 21, 12, 0, 0, 2000, time.UTC), time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 21, 12, 0, 0, 1500, time.UTC), time.Date(2026, 9, 21, 12, 0, 0, 500, time.UTC),
		time.Date(2026, 9, 21, 12, 0, 1, 1000, time.UTC)} {
		c := change("update")
		c.AcceptedAt = clock
		if _, err := care.Encode(live, nil, c); err == nil {
			t.Fatalf("a change accepted at %s encoded an entity accepted at %s", clock.Format(time.RFC3339Nano), meta)
		}
	}
	bad := live
	bad.Meta.AcceptedAt = "yesterday"
	if _, err := care.Encode(bad, nil, care.Change{Seq: 1, ActorAccountID: accountA, DeviceID: deviceA, Operation: "create"}); !errors.Is(err, protocol.ErrInvalidTimestamp) {
		t.Fatalf("unparsable meta accepted_at: %v", err)
	}
}

// NormalizeTimerEntity renders a live timer as the legacy timer tables did
// (every timestamp to whole microseconds, no parts as []) and returns any
// other entity, and every timer tombstone, unchanged, even a tombstone that
// still carried a payload.
func TestNormalizeTimerEntity(t *testing.T) {
	bottle, partStart, partEnd := "2026-09-21T11:00:00.456Z", "2026-09-21T10:55:00.25Z", "2026-09-21T10:57:00.250Z"
	seconds := uint32(120)
	side := "left"
	running := protocol.TimerStartPayload{BabyID: babyA, TimerKind: "feed", StartedAt: "2026-09-21T10:55:00.123000Z", TimeZoneID: "Europe/Zurich",
		CurrentBottleStartedAt: &bottle, CompletedComponents: []protocol.FeedComponent{{ID: "31000000-0000-4000-8000-000000000001", Kind: "breast",
			Side: &side, Seconds: &seconds, StartedAt: &partStart, EndedAt: &partEnd}}}
	meta := protocol.EntityMeta{ID: "50000000-0000-4000-8000-000000000001", FamilyID: familyID, EntityVersion: 1, CreatorAccountID: accountA,
		LastEditorAccountID: accountA, ClientMutationTime: "2026-09-21T10:55:00.123000Z", AcceptedAt: "2026-09-21T12:00:00.000000Z"}
	encode := func(e protocol.CanonicalEntity) string {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	live, err := care.NormalizeTimerEntity(protocol.CanonicalEntity{EntityKind: "timer", Meta: meta, Payload: running})
	if err != nil {
		t.Fatal(err)
	}
	got := encode(live)
	for _, want := range []string{`"current_bottle_started_at":"2026-09-21T11:00:00.456000Z"`, `"started_at":"2026-09-21T10:55:00.250000Z"`,
		`"ended_at":"2026-09-21T10:57:00.250000Z"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("live timer lacks %s: %s", want, got)
		}
	}
	sleep := running
	sleep.TimerKind, sleep.CurrentBottleStartedAt, sleep.CompletedComponents = "sleep", nil, nil
	normalized, err := care.NormalizeTimerEntity(protocol.CanonicalEntity{EntityKind: "timer", Meta: meta, Payload: sleep})
	if err != nil || !strings.Contains(encode(normalized), `"completed_components":[]`) {
		t.Fatalf("sleep timer parts: %v %s", err, encode(normalized))
	}

	tombstoned := "2026-09-21T12:00:00.000000Z"
	stopped := meta
	stopped.TombstonedAt = &tombstoned
	for _, entity := range []protocol.CanonicalEntity{
		{EntityKind: "timer", Meta: stopped},
		{EntityKind: "timer", Meta: stopped, Payload: running}, // not a stored shape; still untouched
		{EntityKind: "feed", Meta: meta, Payload: protocol.FeedPayload{BabyID: babyA, Kind: "breast", StartedAt: "2026-09-21T10:00:00.25Z"}},
	} {
		out, err := care.NormalizeTimerEntity(entity)
		if err != nil || encode(out) != encode(entity) {
			t.Fatalf("%s changed: %v\n got %s\nwant %s", entity.EntityKind, err, encode(out), encode(entity))
		}
	}
}
