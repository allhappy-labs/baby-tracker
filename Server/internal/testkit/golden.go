package testkit

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
)

// The golden wire corpus: one fixed scenario every storage implementation
// must answer with byte-identical sync, MCP and export output. Dynamic
// identifiers are rewritten to the fixed UUIDs below (same byte length), and
// page tokens and history cursors to "{{token}}".
const (
	GoldenFamilyID         = "f0000000-0000-4000-8000-0000000000f1"
	GoldenOwnerAccount     = "a0000000-0000-4000-8000-0000000000a1"
	GoldenOwnerDevice      = "d0000000-0000-4000-8000-0000000000d1"
	GoldenCaregiverAccount = "a0000000-0000-4000-8000-0000000000a2"
	GoldenCaregiverDevice  = "d0000000-0000-4000-8000-0000000000d2"
	GoldenSnapshotID       = "5a000000-0000-4000-8000-000000000001"
	GoldenToken            = "{{token}}"

	// GoldenBabyA is baby.json's baby; GoldenBabyB is created and then
	// deleted (with its children) by the scenario.
	GoldenBabyA = "10000000-0000-4000-8000-000000000001"
	GoldenBabyB = "10000000-0000-4000-8000-0000000000b2"
	// GoldenImportID is step 11's preserve_local_ids import.
	GoldenImportID = "0c000000-0000-4000-8000-000000000001"
)

// GoldenStep is one ApplyAndPull call of the scenario. Resolve, when set, is
// an import resolve the subject sends just before the step's mutations.
type GoldenStep struct {
	Name, Subject string // Subject is "owner" or "caregiver"
	Mutations     []protocol.Mutation
	Resolve       *protocol.ImportResolveRequest
}

// GoldenRun is what RunGolden recorded.
type GoldenRun struct {
	Outcomes  [][]json.RawMessage // per step, normalized outcome objects as encoded on the wire
	Imports   []json.RawMessage   // normalized import resolve responses
	HighWater []uint64            // families.next_sequence after each step
}

func goldenMutationID(step, index int) string {
	return fmt.Sprintf("0a000000-0000-4000-8000-%012d", step*100+index)
}

func goldenString(value string) *string { return &value }

// GoldenSteps is the fixed scenario: every entity kind, updates, deletes and
// tombstones, both timer kinds with their completions and two timers left
// running, refusals, a receipt replay, a baby cascade, and an import, with
// notes and names that exercise
// JSON escaping (<, >, &, U+2028, U+2029, non-ASCII and emoji). Mutation IDs,
// client times and payloads are fixed; accepted times come from h.Now, and
// the family's and its audit's times from the harness's creation, so the
// harness runs at FixtureClock (NewAtFixtureClock), the instant the corpora
// were recorded at.
func GoldenSteps(t *testing.T, h *Harness) []GoldenStep {
	t.Helper()
	if !h.Now.Equal(FixtureClock) {
		t.Fatalf("the golden scenario runs at FixtureClock (testkit.NewAtFixtureClock), not %s", h.Now.Format(time.RFC3339))
	}
	at := func(m protocol.Mutation, step, index int) protocol.Mutation {
		m.MutationID = goldenMutationID(step, index)
		return m
	}

	// Step 1: baby A from the fixture and baby B built here.
	babyA := at(MustMutation(t, "baby.json"), 1, 1)
	babyB := protocol.Mutation{MutationID: goldenMutationID(1, 2), EntityID: GoldenBabyB, EntityKind: "baby", Operation: "create",
		PayloadVersion: 1, ClientMutationTime: "2026-09-20T12:35:00.000000Z",
		Payload: protocol.BabyPayload{Name: "Zoë <b>&</b>", BirthDate: "2026-02-01", SortOrder: 1}}

	// Step 2: the four feed shapes. The combination fixture is an update of
	// an unknown entity, so it is replayed here as that entity's create.
	breast := at(MustMutation(t, "feed-breast.json"), 2, 1)
	bottle := at(MustMutation(t, "feed-bottle.json"), 2, 2)
	combination := at(MustMutation(t, "feed-combination.json"), 2, 3)
	combination.Operation, combination.ObservedEntityVersion = "create", 0
	solids := at(MustMutation(t, "feed-solids.json"), 2, 4)
	solidsPayload := solids.Payload.(protocol.FeedPayload)
	solidsPayload.Note = goldenString("Lunch 🍌 & yogurt")
	solids.Payload = solidsPayload

	// Step 3: the caregiver edits the bottle feed; the note holds U+2028
	// and HTML-significant characters.
	bottleEdit := bottle
	bottleEdit.MutationID, bottleEdit.Operation, bottleEdit.ObservedEntityVersion = goldenMutationID(3, 1), "update", 1
	bottleEdit.ClientMutationTime = "2026-09-20T12:40:00.000000Z"
	bottleEditPayload := bottle.Payload.(protocol.FeedPayload)
	bottleEditPayload.Note = goldenString("line sep <script>&")
	bottleEdit.Payload = bottleEditPayload

	// Step 4: the other record kinds.
	sleep := at(MustMutation(t, "sleep.json"), 4, 1)
	diaper := at(MustMutation(t, "diaper.json"), 4, 2)
	supplement := at(MustMutation(t, "supplement.json"), 4, 3)
	growth := at(MustMutation(t, "growth-profile.json"), 4, 4)
	measurement := at(MustMutation(t, "measurement.json"), 4, 5)

	// Step 5: a supplement update keeping logged_at (note with U+2029 and
	// emoji) and a diaper delete.
	supplementEdit := supplement
	supplementEdit.MutationID, supplementEdit.Operation, supplementEdit.ObservedEntityVersion = goldenMutationID(5, 1), "update", 1
	supplementEdit.ClientMutationTime = "2026-09-20T15:05:00.000000Z"
	supplementEditPayload := supplement.Payload.(protocol.SupplementPayload)
	supplementEditPayload.Note = goldenString("dose given 💊 > half")
	supplementEdit.Payload = supplementEditPayload
	babyAID := GoldenBabyA
	diaperDelete := protocol.Mutation{MutationID: goldenMutationID(5, 2), EntityID: diaper.EntityID, EntityKind: "diaper", Operation: "delete",
		PayloadVersion: 1, ObservedEntityVersion: 1, ClientMutationTime: "2026-09-20T15:06:00.000000Z", Payload: protocol.DeletePayload{BabyID: &babyAID}}

	// Step 6: a timed bottle: start, finish the bottle, edit the timer
	// (earlier start, corrected volume), stop into a completion feed.
	feedTimer := at(MustMutation(t, "timer-feed.json"), 6, 1)
	feedTimerStart := feedTimer.Payload.(protocol.TimerStartPayload)
	feedTimerStart.CurrentSide, feedTimerStart.CurrentSideStartedAt = nil, nil
	feedTimerStart.CurrentBottleStartedAt = goldenString(feedTimerStart.StartedAt)
	feedTimer.Payload = feedTimerStart
	timedBottleID := "31000000-0000-4000-8000-000000000060"
	finish := protocol.Mutation{MutationID: goldenMutationID(6, 2), EntityID: feedTimer.EntityID, EntityKind: "timer", Operation: "timer_finish_bottle",
		PayloadVersion: 5, ObservedEntityVersion: 1, ClientMutationTime: "2026-09-20T15:05:00.000000Z",
		Payload: protocol.TimerBottleFinishPayload{BabyID: GoldenBabyA, FinishedAt: "2026-09-20T15:05:00.000000Z", ComponentID: timedBottleID,
			VolumeML: protocol.MustParseDecimal("90"), Contents: goldenString("formula")}}
	seconds := uint32(300)
	edit := protocol.Mutation{MutationID: goldenMutationID(6, 3), EntityID: feedTimer.EntityID, EntityKind: "timer", Operation: "update",
		PayloadVersion: 5, ObservedEntityVersion: 2, ClientMutationTime: "2026-09-20T15:07:00.000000Z",
		Payload: protocol.TimerEditPayload{BabyID: GoldenBabyA, StartedAt: goldenString("2026-09-20T14:58:00.000000Z"),
			Components: []protocol.FeedComponent{{ID: timedBottleID, Kind: "bottle", Seconds: &seconds, VolumeML: protocol.MustParseDecimal("95"),
				Contents: goldenString("breast_milk"), StartedAt: goldenString("2026-09-20T15:00:00.000000Z"), EndedAt: goldenString("2026-09-20T15:05:00.000000Z")}}}}
	stop := protocol.Mutation{MutationID: goldenMutationID(6, 4), EntityID: feedTimer.EntityID, EntityKind: "timer", Operation: "timer_stop",
		PayloadVersion: 5, ObservedEntityVersion: 3, ClientMutationTime: "2026-09-20T15:10:00.000000Z",
		Payload: protocol.TimerStopPayload{BabyID: GoldenBabyA, CompletedRecordID: "20000000-0000-4000-8000-000000000060", StoppedAt: "2026-09-20T15:10:00.000000Z"}}

	// Step 7: the caregiver's sleep timer, stopped with a note and a kind.
	sleepTimer := at(MustMutation(t, "timer-sleep.json"), 7, 1)
	sleepStop := protocol.Mutation{MutationID: goldenMutationID(7, 2), EntityID: sleepTimer.EntityID, EntityKind: "timer", Operation: "timer_stop",
		PayloadVersion: 5, ObservedEntityVersion: 1, ClientMutationTime: "2026-09-20T16:45:00.000000Z",
		Payload: protocol.TimerStopPayload{BabyID: GoldenBabyA, CompletedRecordID: "30000000-0000-4000-8000-000000000070", StoppedAt: "2026-09-20T16:45:00.000000Z",
			Note: goldenString("Woke up happy 😴 \"quiet\""), SleepKindOverride: goldenString("nap")}}

	// Step 8: refusals and a stale write.
	orphan := breast
	orphan.MutationID, orphan.EntityID = goldenMutationID(8, 1), "20000000-0000-4000-8000-0000000000e1"
	orphanPayload := breast.Payload.(protocol.FeedPayload)
	orphanPayload.BabyID = "10000000-0000-4000-8000-0000000000ff"
	orphanPayload.Components = []protocol.FeedComponent{{ID: "31000000-0000-4000-8000-0000000000e1", Kind: "breast", Side: goldenString("left"), Seconds: &seconds}}
	orphan.Payload = orphanPayload
	duplicate := breast
	duplicate.MutationID = goldenMutationID(8, 2)
	stale := bottle
	stale.MutationID, stale.Operation, stale.ObservedEntityVersion = goldenMutationID(8, 3), "update", 1
	stale.ClientMutationTime = "2026-09-20T12:45:00.000000Z"
	stalePayload := bottle.Payload.(protocol.FeedPayload)
	stalePayload.Note = goldenString("owner's stale edit")
	stale.Payload = stalePayload

	// Step 10: baby B's children, then baby B's delete cascades over them.
	babyBID := GoldenBabyB
	feedB := bottle
	feedB.MutationID, feedB.EntityID = goldenMutationID(10, 1), "20000000-0000-4000-8000-0000000000b2"
	feedBPayload := bottle.Payload.(protocol.FeedPayload)
	feedBPayload.BabyID, feedBPayload.StartedAt = GoldenBabyB, "2026-09-20T17:00:00.000000Z"
	feedBPayload.Components = []protocol.FeedComponent{{ID: "31000000-0000-4000-8000-0000000000b2", Kind: "bottle", VolumeML: protocol.MustParseDecimal("120"), Contents: goldenString("formula")}}
	feedB.Payload = feedBPayload
	sleepB := sleep
	sleepB.MutationID, sleepB.EntityID = goldenMutationID(10, 2), "30000000-0000-4000-8000-0000000000b2"
	sleepBPayload := sleep.Payload.(protocol.SleepPayload)
	sleepBPayload.BabyID = GoldenBabyB
	sleepB.Payload = sleepBPayload
	timerB := MustMutation(t, "timer-feed.json")
	timerB.MutationID, timerB.EntityID = goldenMutationID(10, 3), "50000000-0000-4000-8000-0000000000b2"
	timerBPayload := timerB.Payload.(protocol.TimerStartPayload)
	timerBPayload.BabyID = GoldenBabyB
	timerB.Payload = timerBPayload
	growthB := growth
	growthB.MutationID, growthB.EntityID = goldenMutationID(10, 4), protocol.GrowthProfileID(GoldenBabyB)
	growthB.Payload = protocol.GrowthProfilePayload{BabyID: GoldenBabyB, ReferenceSex: "boys", ExpectedDueDate: goldenString("2026-02-08")}
	measurementB := measurement
	measurementB.MutationID, measurementB.EntityID = goldenMutationID(10, 5), "70000000-0000-4000-8000-0000000000b2"
	measurementBPayload := measurement.Payload.(protocol.MeasurementPayload)
	measurementBPayload.BabyID = GoldenBabyB
	measurementB.Payload = measurementBPayload
	deleteB := protocol.Mutation{MutationID: goldenMutationID(10, 6), EntityID: babyBID, EntityKind: "baby", Operation: "delete",
		PayloadVersion: 1, ObservedEntityVersion: 1, ClientMutationTime: "2026-09-20T17:30:00.000000Z", Payload: protocol.DeletePayload{}}

	// Step 11: a preserve_local_ids import of baby A with one new record and
	// one equal to step 2's solids feed.
	importID := GoldenImportID
	imported := breast
	imported.MutationID, imported.EntityID, imported.ImportID = goldenMutationID(11, 1), "20000000-0000-4000-8000-0000000000c1", &importID
	importedPayload := breast.Payload.(protocol.FeedPayload)
	importedPayload.StartedAt, importedPayload.EndedAt = "2026-09-20T18:00:00.000000Z", goldenString("2026-09-20T18:10:00.000000Z")
	importedPayload.Note = goldenString("imported ✅")
	importedPayload.Components = []protocol.FeedComponent{
		{ID: "31000000-0000-4000-8000-0000000000c1", Kind: "breast", Side: goldenString("right"), Seconds: &seconds},
		{ID: "31000000-0000-4000-8000-0000000000c2", Kind: "breast", Side: goldenString("left"), Seconds: &seconds}}
	imported.Payload = importedPayload
	equal := solids
	equal.MutationID, equal.ImportID = goldenMutationID(11, 2), &importID
	resolve := protocol.ImportResolveRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1",
		Capabilities: []string{protocol.CapabilityRecordsV1, protocol.CapabilityImportResolverV1}},
		ImportID: GoldenImportID, ImportMode: "preserve_local_ids", NormalizationVersion: "nfc-trim-fold-nfc-v1",
		Babies:  []protocol.ImportBaby{{LocalBabyID: GoldenBabyA, Name: "Élie", BirthDate: "2026-01-12", SortOrder: 0}},
		Records: []protocol.RecordManifest{goldenManifest(t, imported), goldenManifest(t, equal)}, TotalRecordCount: 2, ChunkIndex: 0, Finalize: true}

	// Steps 12 and 13: timers left running for every later read. The feed
	// timer is mid-switch: a finished left side and a finished bottle part,
	// with the right side running.
	runningFeed := MustMutation(t, "timer-feed.json")
	runningFeed.MutationID, runningFeed.EntityID = goldenMutationID(12, 1), "50000000-0000-4000-8000-000000000003"
	runningFeed.ClientMutationTime = "2026-09-20T19:07:00.000000Z"
	runningFeedPayload := runningFeed.Payload.(protocol.TimerStartPayload)
	bottleSeconds := uint32(120)
	runningFeedPayload.StartedAt = "2026-09-20T19:00:00.000000Z"
	runningFeedPayload.CurrentSide, runningFeedPayload.CurrentSideStartedAt = goldenString("right"), goldenString("2026-09-20T19:07:00.000000Z")
	runningFeedPayload.CompletedComponents = []protocol.FeedComponent{
		{ID: "31000000-0000-4000-8000-0000000000d1", Kind: "breast", Side: goldenString("left"), Seconds: &seconds,
			StartedAt: goldenString("2026-09-20T19:00:00.000000Z"), EndedAt: goldenString("2026-09-20T19:05:00.000000Z")},
		{ID: "31000000-0000-4000-8000-0000000000d2", Kind: "bottle", Seconds: &bottleSeconds, VolumeML: protocol.MustParseDecimal("40"),
			Contents: goldenString("breast_milk"), StartedAt: goldenString("2026-09-20T19:05:00.000000Z"), EndedAt: goldenString("2026-09-20T19:07:00.000000Z")}}
	runningFeed.Payload = runningFeedPayload
	runningSleep := MustMutation(t, "timer-sleep.json")
	runningSleep.MutationID, runningSleep.EntityID = goldenMutationID(13, 1), "50000000-0000-4000-8000-000000000004"
	runningSleep.ClientMutationTime = "2026-09-20T20:00:00.000000Z"
	runningSleepPayload := runningSleep.Payload.(protocol.TimerStartPayload)
	runningSleepPayload.StartedAt = "2026-09-20T20:00:00.000000Z"
	runningSleep.Payload = runningSleepPayload

	return []GoldenStep{
		{Name: "01 owner creates babies", Subject: "owner", Mutations: []protocol.Mutation{babyA, babyB}},
		{Name: "02 owner logs feeds", Subject: "owner", Mutations: []protocol.Mutation{breast, bottle, combination, solids}},
		{Name: "03 caregiver edits the bottle feed", Subject: "caregiver", Mutations: []protocol.Mutation{bottleEdit}},
		{Name: "04 owner logs the other kinds", Subject: "owner", Mutations: []protocol.Mutation{sleep, diaper, supplement, growth, measurement}},
		{Name: "05 caregiver edits a supplement and deletes a diaper", Subject: "caregiver", Mutations: []protocol.Mutation{supplementEdit, diaperDelete}},
		{Name: "06 owner runs a timed bottle", Subject: "owner", Mutations: []protocol.Mutation{feedTimer, finish, edit, stop}},
		{Name: "07 caregiver runs a sleep timer", Subject: "caregiver", Mutations: []protocol.Mutation{sleepTimer, sleepStop}},
		{Name: "08 owner refusals and a stale write", Subject: "owner", Mutations: []protocol.Mutation{orphan, duplicate, stale}},
		{Name: "09 owner replays step 2", Subject: "owner", Mutations: []protocol.Mutation{breast}},
		{Name: "10 owner fills and deletes baby B", Subject: "owner", Mutations: []protocol.Mutation{feedB, sleepB, timerB, growthB, measurementB, deleteB}},
		{Name: "11 owner imports", Subject: "owner", Mutations: []protocol.Mutation{imported, equal}, Resolve: &resolve},
		{Name: "12 owner leaves a feed timer running", Subject: "owner", Mutations: []protocol.Mutation{runningFeed}},
		{Name: "13 caregiver leaves a sleep timer running", Subject: "caregiver", Mutations: []protocol.Mutation{runningSleep}},
	}
}

func goldenManifest(t *testing.T, m protocol.Mutation) protocol.RecordManifest {
	t.Helper()
	hash, err := protocol.CanonicalContentHash(m.EntityKind, m.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.RecordManifest{RecordID: m.EntityID, EntityKind: m.EntityKind, LocalBabyID: GoldenBabyA, ContentSHA256: hex.EncodeToString(hash[:])}
}

func (h *Harness) goldenPrincipal(t *testing.T, subject string) access.Principal {
	t.Helper()
	switch subject {
	case "owner":
		return h.Owner
	case "caregiver":
		return h.Caregiver
	}
	t.Fatalf("golden subject %q", subject)
	return access.Principal{}
}

// RunGolden applies GoldenSteps, one ApplyAndPull per step from the
// subject's last seen high water, and records each step's normalized
// outcomes, the import resolve responses and the family high water.
func (h *Harness) RunGolden(t *testing.T) GoldenRun {
	t.Helper()
	var run GoldenRun
	var highWater uint64
	for _, step := range GoldenSteps(t, h) {
		principal := h.goldenPrincipal(t, step.Subject)
		if step.Resolve != nil {
			response, err := h.Importer.Resolve(t.Context(), principal, *step.Resolve)
			if err != nil {
				t.Fatalf("%s: resolve: %v", step.Name, err)
			}
			run.Imports = append(run.Imports, h.normalizedJSON(t, response))
		}
		capabilities := []string{protocol.CapabilityRecordsV1}
		for _, m := range step.Mutations {
			if m.EntityKind == "timer" {
				capabilities = append(capabilities, protocol.CapabilityTimerCommandsV1)
				break
			}
		}
		var cursor *protocol.Cursor
		if highWater > 0 {
			cursor = h.Cursor(highWater)
		}
		request := protocol.SyncRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: capabilities},
			Cursor: cursor, Mutations: step.Mutations}
		response, err := h.Service.ApplyAndPull(t.Context(), principal, request)
		if err != nil {
			t.Fatalf("%s: ApplyAndPull: %v", step.Name, err)
		}
		if len(response.Outcomes) != len(step.Mutations) {
			t.Fatalf("%s: %d outcomes for %d mutations", step.Name, len(response.Outcomes), len(step.Mutations))
		}
		outcomes := make([]json.RawMessage, 0, len(response.Outcomes))
		for _, outcome := range response.Outcomes {
			outcomes = append(outcomes, h.normalizedJSON(t, outcome))
		}
		run.Outcomes = append(run.Outcomes, outcomes)
		highWater = h.FamilySequence(t)
		run.HighWater = append(run.HighWater, highWater)
	}
	return run
}

func (h *Harness) normalizedJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return h.Normalize(raw)
}

var (
	goldenTokenPattern    = regexp.MustCompile(`"(next_page_token|page_token|next_cursor|next_conflict_page_token)":"[^"]*"`)
	goldenSnapshotPattern = regexp.MustCompile(`"snapshot_id":"[0-9a-f-]{36}"`)
)

// Normalize rewrites the harness's dynamic identifiers in encoded JSON to
// the fixed golden UUIDs (same length, still valid, so normalized entities
// decode), snapshot IDs to GoldenSnapshotID, and page tokens and history
// cursors to "{{token}}". It never re-encodes, so every other byte, escapes
// included, is kept.
func (h *Harness) Normalize(raw []byte) []byte {
	out := bytes.Clone(raw)
	for _, pair := range [][2]string{
		{h.FamilyID, GoldenFamilyID},
		{h.Owner.AccountID, GoldenOwnerAccount}, {h.Owner.DeviceID, GoldenOwnerDevice},
		{h.Caregiver.AccountID, GoldenCaregiverAccount}, {h.Caregiver.DeviceID, GoldenCaregiverDevice},
	} {
		if pair[0] == "" || len(pair[0]) != len(pair[1]) {
			h.t.Fatalf("golden normalization of %q to %q", pair[0], pair[1])
		}
		out = bytes.ReplaceAll(out, []byte(pair[0]), []byte(pair[1]))
	}
	out = goldenSnapshotPattern.ReplaceAll(out, []byte(`"snapshot_id":"`+GoldenSnapshotID+`"`))
	return goldenTokenPattern.ReplaceAll(out, []byte(`"$1":"`+GoldenToken+`"`))
}

// AssertGolden compares got, encoded as indented JSON, with the golden file
// at path (relative to the test's package directory). BT_GOLDEN_UPDATE=1
// rewrites the file instead. The encoder does not HTML-escape, so the file
// holds the recorded wire bytes exactly as they were produced: escapes in
// it are the wire's own.
func AssertGolden(t *testing.T, path string, got any) {
	t.Helper()
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(got); err != nil {
		t.Fatalf("encode golden %s: %v", path, err)
	}
	encoded := buffer.Bytes()
	if os.Getenv("BT_GOLDEN_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Fatalf("golden file %s missing (set BT_GOLDEN_UPDATE=1)", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(want, encoded) {
		return
	}
	t.Fatalf("golden file %s differs: %s", path, goldenDifference(want, encoded))
}

// goldenDifference names the first JSON path whose value differs, or the
// first differing byte when the values are equal but the bytes are not.
func goldenDifference(want, got []byte) string {
	var wantValue, gotValue any
	if decodeGolden(want, &wantValue) == nil && decodeGolden(got, &gotValue) == nil {
		if path, ok := firstDifference("$", wantValue, gotValue); ok {
			return path
		}
	}
	index := 0
	for index < len(want) && index < len(got) && want[index] == got[index] {
		index++
	}
	excerpt := func(b []byte) string {
		end := min(len(b), index+60)
		return strings.ReplaceAll(string(b[max(0, index-20):end]), "\n", "⏎")
	}
	return fmt.Sprintf("byte %d: want %q got %q", index, excerpt(want), excerpt(got))
}

func decodeGolden(raw []byte, value *any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(value)
}

func firstDifference(path string, want, got any) (string, bool) {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s: want object got %v", path, describe(got)), true
		}
		keys := make([]string, 0, len(w)+len(g))
		for key := range w {
			keys = append(keys, key)
		}
		for key := range g {
			if _, seen := w[key]; !seen {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			wv, wok := w[key]
			gv, gok := g[key]
			if !wok || !gok {
				return fmt.Sprintf("%s.%s: present in want=%t got=%t", path, key, wok, gok), true
			}
			if p, differs := firstDifference(path+"."+key, wv, gv); differs {
				return p, true
			}
		}
		return "", false
	case []any:
		g, ok := got.([]any)
		if !ok {
			return fmt.Sprintf("%s: want array got %v", path, describe(got)), true
		}
		for i := 0; i < len(w) && i < len(g); i++ {
			if p, differs := firstDifference(fmt.Sprintf("%s[%d]", path, i), w[i], g[i]); differs {
				return p, true
			}
		}
		if len(w) != len(g) {
			return fmt.Sprintf("%s: want %d items got %d", path, len(w), len(g)), true
		}
		return "", false
	default:
		if fmt.Sprint(want) != fmt.Sprint(got) || fmt.Sprintf("%T", want) != fmt.Sprintf("%T", got) {
			return fmt.Sprintf("%s: want %v got %v", path, describe(want), describe(got)), true
		}
		return "", false
	}
}

func describe(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	if len(raw) > 200 {
		return string(raw[:200]) + "…"
	}
	return string(raw)
}

// LatestPerEntity keeps each entity's last change from a pull's changes,
// in sequence order: what a pull that serves only latest state returns.
func LatestPerEntity(t *testing.T, changes []json.RawMessage) []json.RawMessage {
	t.Helper()
	type keyed struct {
		EntityID string `json:"entity_id"`
		Cursor   struct {
			Sequence uint64 `json:"sequence"`
		} `json:"cursor"`
	}
	last := map[string]int{}
	decoded := make([]keyed, len(changes))
	for i, raw := range changes {
		if err := json.Unmarshal(raw, &decoded[i]); err != nil || decoded[i].EntityID == "" {
			t.Fatalf("change %d: %v", i, err)
		}
		if i > 0 && decoded[i].Cursor.Sequence <= decoded[i-1].Cursor.Sequence {
			t.Fatalf("change %d: sequence %d not after %d", i, decoded[i].Cursor.Sequence, decoded[i-1].Cursor.Sequence)
		}
		last[decoded[i].EntityID] = i
	}
	result := make([]json.RawMessage, 0, len(last))
	for i, raw := range changes {
		if last[decoded[i].EntityID] == i {
			result = append(result, raw)
		}
	}
	return result
}
