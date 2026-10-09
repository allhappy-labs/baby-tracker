package records

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/database"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Migration 27 is seeded at schema 26 with the stored shape classes production
// holds (feeds v2/v3/v4, sleeps v1, timers v2/v3/v4 stopped, a live v5 timer,
// tombstones, legacy timer operations, receipts) and applied the way
// production's `compose run migrate` applies it: by the table-owning role that
// has no BYPASSRLS, against tables that FORCE row-level security.

const (
	fcAccount = "fc000000-0000-4000-8000-000000000001"
	fcFamily  = "fc000000-0000-4000-8000-000000000002"
	fcDevice  = "fc000000-0000-4000-8000-000000000003"
	fcSession = "fc000000-0000-4000-8000-000000000004"
	fcBaby    = "fc000000-0000-4000-8000-000000000010"

	fcFeedV2      = "fc000000-0000-4000-8000-000000000020"
	fcFeedV3      = "fc000000-0000-4000-8000-000000000021"
	fcFeedSolids  = "fc000000-0000-4000-8000-000000000022"
	fcFeedV4      = "fc000000-0000-4000-8000-000000000023"
	fcFeedDeleted = "fc000000-0000-4000-8000-000000000024"
	fcSleepV1     = "fc000000-0000-4000-8000-000000000030"
	fcSleepGone   = "fc000000-0000-4000-8000-000000000031"
	fcSleepV2     = "fc000000-0000-4000-8000-000000000032"
	fcDiaper      = "fc000000-0000-4000-8000-000000000040"
	fcTimerV2     = "fc000000-0000-4000-8000-000000000050"
	fcTimerV3     = "fc000000-0000-4000-8000-000000000051"
	fcTimerV4     = "fc000000-0000-4000-8000-000000000052"
	fcTimerV5     = "fc000000-0000-4000-8000-000000000053"

	fcClientTime = "2026-09-20T12:00:00.000000Z"
	fcAccepted   = "2026-09-20T12:00:01.000000Z"
	fcInstance   = "fc000000-0000-4000-8000-0000000000ff"
)

// fcBracketTables are the tables 000027 reads or writes, so the tables whose
// FORCE ROW LEVEL SECURITY it lifts for its own transaction.
var fcBracketTables = []string{
	"families", "feed_entries", "feed_components", "sleep_entries", "active_timers", "timer_components",
	"mutation_journal", "mutation_receipts", "sync_page_sessions", "sync_snapshots", "sync_snapshot_entities",
	"sync_snapshot_admissions", "import_sessions",
}

// fcDroppedTables are the legacy side and food tables 000027 drops.
var fcDroppedTables = []string{"feed_side_intervals", "feed_foods", "timer_side_intervals"}

var fcStagingTables = []string{"sync_page_sessions", "sync_snapshot_entities", "sync_snapshot_admissions", "sync_snapshots"}

// Payloads exactly as the schema-26 server stored them, one per shape class.
var (
	fcBabyPayload = `{"name":"Ada","birth_date":"2026-08-01","sort_order":0}`
	// Feed v2: components with ids, no session key.
	fcFeedV2Payload = `{"baby_id":"` + fcBaby + `","kind":"breast","started_at":"2026-09-20T08:00:00.000000Z",
		"ended_at":"2026-09-20T08:20:00.000000Z","time_zone_id":"Europe/Zurich","note":null,
		"components":[{"id":"fc000000-0000-4000-8000-0000000000c1","kind":"breast","side":"left","seconds":600},
			{"id":"fc000000-0000-4000-8000-0000000000c2","kind":"breast","side":"right","seconds":540}]}`
	// Feed v3: a bottle that carries its measured seconds.
	fcFeedV3CreatePayload = `{"baby_id":"` + fcBaby + `","kind":"bottle","started_at":"2026-09-20T11:00:00.000000Z",
		"ended_at":"2026-09-20T11:10:00.000000Z","time_zone_id":"Europe/Zurich","note":null,
		"components":[{"id":"fc000000-0000-4000-8000-0000000000c3","kind":"bottle","volume_ml":120,"contents":"formula","seconds":600}]}`
	fcFeedV3UpdatePayload = `{"baby_id":"` + fcBaby + `","kind":"bottle","started_at":"2026-09-20T11:00:00.000000Z",
		"ended_at":"2026-09-20T11:10:00.000000Z","time_zone_id":"Europe/Zurich","note":"warm",
		"components":[{"id":"fc000000-0000-4000-8000-0000000000c3","kind":"bottle","volume_ml":125.5,"contents":"formula","seconds":600}]}`
	fcFeedSolidsPayload = `{"baby_id":"` + fcBaby + `","kind":"solids","started_at":"2026-09-20T13:00:00.000000Z",
		"ended_at":null,"time_zone_id":"Europe/Zurich","note":null,
		"components":[{"id":"fc000000-0000-4000-8000-0000000000c4","kind":"solids","foods":["banana","oat"],"amount":"some"}]}`
	// Feed v4 is already current; its session link and step times stay as they are.
	fcFeedV4Payload = `{"baby_id":"` + fcBaby + `","kind":"breast","started_at":"2026-09-20T08:25:00.000000Z",
		"ended_at":"2026-09-20T08:35:00.000000Z","time_zone_id":"Europe/Zurich","note":null,
		"components":[{"id":"fc000000-0000-4000-8000-0000000000c5","kind":"breast","side":"left","seconds":600,
			"started_at":"2026-09-20T08:25:00.000000Z","ended_at":"2026-09-20T08:35:00.000000Z"}],
		"feeding_session_id":"` + fcFeedV2 + `"}`
	fcFeedDeletedPayload = `{"baby_id":"` + fcBaby + `","kind":"breast","started_at":"2026-09-19T08:00:00.000000Z",
		"ended_at":"2026-09-19T08:10:00.000000Z","time_zone_id":"Europe/Zurich","note":null,
		"components":[{"id":"fc000000-0000-4000-8000-0000000000c6","kind":"breast","side":"left","seconds":600}]}`
	// Sleep v1: no session key.
	fcSleepCreatePayload = `{"baby_id":"` + fcBaby + `","started_at":"2026-09-20T14:00:00.000000Z",
		"ended_at":"2026-09-20T15:00:00.000000Z","time_zone_id":"Europe/Zurich","note":null,"kind_override":null}`
	fcSleepUpdatePayload = `{"baby_id":"` + fcBaby + `","started_at":"2026-09-20T14:00:00.000000Z",
		"ended_at":"2026-09-20T15:00:00.000000Z","time_zone_id":"Europe/Zurich","note":"edited","kind_override":"nap"}`
	fcSleepGonePayload = `{"baby_id":"` + fcBaby + `","started_at":"2026-09-19T14:00:00.000000Z",
		"ended_at":"2026-09-19T15:00:00.000000Z","time_zone_id":"Europe/Zurich","note":null,"kind_override":null}`
	fcSleepV2Payload = `{"baby_id":"` + fcBaby + `","started_at":"2026-09-20T15:10:00.000000Z",
		"ended_at":"2026-09-20T16:00:00.000000Z","time_zone_id":"Europe/Zurich","note":null,"kind_override":null,
		"sleep_session_id":"` + fcSleepV1 + `"}`
	// A diaper's top-level `contents` is not a scalar feed key.
	fcDiaperPayload = `{"baby_id":"` + fcBaby + `","started_at":"2026-09-20T09:00:00.000000Z","ended_at":null,
		"time_zone_id":"Europe/Zurich","note":null,"contents":"wet","consistency":null,"color":null}`
	// Timer v2: completed_components, no bottle phase, no session key.
	fcTimerV2StartPayload = `{"baby_id":"` + fcBaby + `","timer_kind":"feed","started_at":"2026-09-19T08:00:00.000000Z",
		"time_zone_id":"Europe/Zurich","current_side":"left","current_side_started_at":"2026-09-19T08:00:00.000000Z",
		"completed_components":[]}`
	fcTimerV2SwitchPayload = `{"baby_id":"` + fcBaby + `","timer_kind":"feed","started_at":"2026-09-19T08:00:00.000000Z",
		"time_zone_id":"Europe/Zurich","current_side":"right","current_side_started_at":"2026-09-19T08:05:00.000000Z",
		"completed_components":[{"id":"fc000000-0000-4000-8000-0000000000d1","kind":"breast","side":"left","seconds":300}]}`
	// Timer v3: the bottle phase key.
	fcTimerV3StartPayload = `{"baby_id":"` + fcBaby + `","timer_kind":"feed","started_at":"2026-09-19T10:00:00.000000Z",
		"time_zone_id":"Europe/Zurich","current_side":"left","current_side_started_at":"2026-09-19T10:00:00.000000Z",
		"current_bottle_started_at":null,"completed_components":[]}`
	fcTimerV3BottlePayload = `{"baby_id":"` + fcBaby + `","timer_kind":"feed","started_at":"2026-09-19T10:00:00.000000Z",
		"time_zone_id":"Europe/Zurich","current_side":null,"current_side_started_at":null,
		"current_bottle_started_at":"2026-09-19T10:05:00.000000Z",
		"completed_components":[{"id":"fc000000-0000-4000-8000-0000000000d2","kind":"breast","side":"left","seconds":300}]}`
	// Timer v4: step times on the finished parts.
	fcTimerV4StartPayload = `{"baby_id":"` + fcBaby + `","timer_kind":"feed","started_at":"2026-09-19T12:00:00.000000Z",
		"time_zone_id":"Europe/Zurich","current_side":"left","current_side_started_at":"2026-09-19T12:00:00.000000Z",
		"current_bottle_started_at":null,"completed_components":[]}`
	fcTimerV4EditPayload = `{"baby_id":"` + fcBaby + `","timer_kind":"feed","started_at":"2026-09-19T12:00:00.000000Z",
		"time_zone_id":"Europe/Zurich","current_side":"right","current_side_started_at":"2026-09-19T12:05:00.000000Z",
		"current_bottle_started_at":null,
		"completed_components":[{"id":"fc000000-0000-4000-8000-0000000000d3","kind":"breast","side":"left","seconds":300,
			"started_at":"2026-09-19T12:00:00.000000Z","ended_at":"2026-09-19T12:05:00.000000Z"}]}`
	// Timer v5 is already current.
	fcTimerV5Payload = `{"baby_id":"` + fcBaby + `","timer_kind":"sleep","started_at":"2026-09-20T20:00:00.000000Z",
		"time_zone_id":"Europe/Zurich","current_side":null,"current_side_started_at":null,
		"current_bottle_started_at":null,"completed_components":[],"session_id":null}`
)

// fcHistory is one seeded journal row: the canonical entity the server stored,
// and the null keys 000027 must add to its payload (none for current shapes,
// tombstones and kinds without versions).
type fcHistory struct {
	sequence  int64
	operation string
	kind, id  string
	version   int64
	payload   string // empty: a tombstone
	adds      []string
	wantOp    string
}

func fcEntity(kind, id string, version int64, payload string) string {
	tombstoned := "null"
	if payload == "" {
		tombstoned, payload = `"`+fcAccepted+`"`, "null"
	}
	return fmt.Sprintf(`{"entity_kind":%q,"meta":{"id":%q,"family_id":%q,"entity_version":%d,"creator_account_id":%q,`+
		`"last_editor_account_id":%q,"client_mutation_time":%q,"accepted_at":%q,"tombstoned_at":%s},"payload":%s}`,
		kind, id, fcFamily, version, fcAccount, fcAccount, fcClientTime, fcAccepted, tombstoned, payload)
}

func (history fcHistory) entity() string {
	return fcEntity(history.kind, history.id, history.version, history.payload)
}

// expected is the stored entity with the added keys set to null.
func (history fcHistory) expected(t *testing.T) string {
	t.Helper()
	return fcWithNullKeys(t, history.entity(), history.adds)
}

func fcWithNullKeys(t *testing.T, entity string, keys []string) string {
	t.Helper()
	if len(keys) == 0 {
		return entity
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(entity), &decoded); err != nil {
		t.Fatal(err)
	}
	payload := decoded["payload"].(map[string]any)
	for _, key := range keys {
		if _, exists := payload[key]; exists {
			t.Fatalf("seed already holds %q", key)
		}
		payload[key] = nil
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

var fcJournal = []fcHistory{
	{1, "create", "baby", fcBaby, 1, fcBabyPayload, nil, "create"},
	{2, "create", "feed", fcFeedV2, 1, fcFeedV2Payload, []string{"feeding_session_id"}, "create"},
	{3, "create", "feed", fcFeedV3, 1, fcFeedV3CreatePayload, []string{"feeding_session_id"}, "create"},
	{4, "update", "feed", fcFeedV3, 2, fcFeedV3UpdatePayload, []string{"feeding_session_id"}, "update"},
	{5, "create", "feed", fcFeedSolids, 1, fcFeedSolidsPayload, []string{"feeding_session_id"}, "create"},
	{6, "create", "feed", fcFeedV4, 1, fcFeedV4Payload, nil, "create"},
	{7, "create", "feed", fcFeedDeleted, 1, fcFeedDeletedPayload, []string{"feeding_session_id"}, "create"},
	{8, "delete", "feed", fcFeedDeleted, 2, "", nil, "delete"},
	{9, "create", "sleep", fcSleepV1, 1, fcSleepCreatePayload, []string{"sleep_session_id"}, "create"},
	{10, "update", "sleep", fcSleepV1, 2, fcSleepUpdatePayload, []string{"sleep_session_id"}, "update"},
	{11, "create", "sleep", fcSleepGone, 1, fcSleepGonePayload, []string{"sleep_session_id"}, "create"},
	{12, "delete", "sleep", fcSleepGone, 2, "", nil, "delete"},
	{13, "create", "sleep", fcSleepV2, 1, fcSleepV2Payload, nil, "create"},
	{14, "create", "diaper", fcDiaper, 1, fcDiaperPayload, nil, "create"},
	{15, "timer_start", "timer", fcTimerV2, 1, fcTimerV2StartPayload, []string{"current_bottle_started_at", "session_id"}, "timer_start"},
	{16, "timer_switch_side", "timer", fcTimerV2, 2, fcTimerV2SwitchPayload, []string{"current_bottle_started_at", "session_id"}, "update"},
	{17, "timer_stop", "timer", fcTimerV2, 3, "", nil, "timer_stop"},
	{18, "timer_start", "timer", fcTimerV3, 1, fcTimerV3StartPayload, []string{"session_id"}, "timer_start"},
	{19, "timer_start_bottle", "timer", fcTimerV3, 2, fcTimerV3BottlePayload, []string{"session_id"}, "update"},
	{20, "timer_stop", "timer", fcTimerV3, 3, "", nil, "timer_stop"},
	{21, "timer_start", "timer", fcTimerV4, 1, fcTimerV4StartPayload, []string{"session_id"}, "timer_start"},
	{22, "update", "timer", fcTimerV4, 2, fcTimerV4EditPayload, []string{"session_id"}, "update"},
	{23, "timer_stop", "timer", fcTimerV4, 3, "", nil, "timer_stop"},
	{24, "timer_start", "timer", fcTimerV5, 1, fcTimerV5Payload, nil, "timer_start"},
}

func fcHistoryAt(sequence int64) fcHistory {
	for _, history := range fcJournal {
		if history.sequence == sequence {
			return history
		}
	}
	panic(fmt.Sprintf("no seeded journal row %d", sequence))
}

// fcReceipt is one stored mutation outcome, referring to journal rows by
// sequence for its canonical and related entities (0: JSON null).
type fcReceipt struct {
	mutationID string
	status     string
	canonical  int64
	related    []int64
	nullList   bool
	cursor     int64
}

var fcReceipts = []fcReceipt{
	{"fc000000-0000-4000-8000-0000000000a1", "applied", 2, []int64{}, false, 2},
	{"fc000000-0000-4000-8000-0000000000a2", "applied", 4, []int64{}, false, 4},
	{"fc000000-0000-4000-8000-0000000000a3", "applied", 10, []int64{}, false, 10},
	{"fc000000-0000-4000-8000-0000000000a4", "applied", 12, []int64{}, false, 12},
	{"fc000000-0000-4000-8000-0000000000a5", "applied", 16, []int64{}, false, 16},
	{"fc000000-0000-4000-8000-0000000000a6", "applied", 19, []int64{}, false, 19},
	{"fc000000-0000-4000-8000-0000000000a7", "applied", 17, []int64{2}, false, 17},
	{"fc000000-0000-4000-8000-0000000000a8", "applied", 14, []int64{}, false, 14},
	{"fc000000-0000-4000-8000-0000000000a9", "rejected", 0, nil, true, 0},
}

func (receipt fcReceipt) outcome(t *testing.T, converted bool) string {
	t.Helper()
	entity := func(sequence int64) string {
		history := fcHistoryAt(sequence)
		if converted {
			return history.expected(t)
		}
		return history.entity()
	}
	canonical, cursor, code := "null", "null", "null"
	if receipt.canonical != 0 {
		canonical = entity(receipt.canonical)
	}
	related := "null"
	if !receipt.nullList {
		parts := make([]string, 0, len(receipt.related))
		for _, sequence := range receipt.related {
			parts = append(parts, entity(sequence))
		}
		related = "[" + strings.Join(parts, ",") + "]"
	}
	if receipt.cursor != 0 {
		cursor = fmt.Sprintf(`{"server_instance_id":%q,"recovery_epoch":1,"family_id":%q,"sequence":%d}`, fcInstance, fcFamily, receipt.cursor)
	}
	if receipt.status == "rejected" {
		code = `"invalid_request"`
	}
	return fmt.Sprintf(`{"mutation_id":%q,"status":%q,"original_status":null,"canonical_entity":%s,"related_entities":%s,`+
		`"cursor":%s,"code":%s,"overwrote_newer_version":false}`, receipt.mutationID, receipt.status, canonical, related, cursor, code)
}

func fcTenantTx(t *testing.T, fixture *testpg.Fixture) pgx.Tx {
	t.Helper()
	tx, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(t.Context()) })
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: fcAccount, FamilyID: fcFamily}); err != nil {
		t.Fatal(err)
	}
	return tx
}

func fcExec(t *testing.T, tx pgx.Tx, query string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(t.Context(), query, args...); err != nil {
		t.Fatalf("%v\n%s", err, query)
	}
}

func fcSeedFamily(t *testing.T, fixture *testpg.Fixture) {
	t.Helper()
	tx := fcTenantTx(t, fixture)
	fcExec(t, tx, `INSERT INTO accounts(id,display_name) VALUES($1,'Format owner')`, fcAccount)
	fcExec(t, tx, `INSERT INTO families(id,display_name) VALUES($1,'Format family')`, fcFamily)
	fcExec(t, tx, `INSERT INTO memberships(family_id,account_id,role) VALUES($1,$2,'owner')`, fcFamily, fcAccount)
	fcExec(t, tx, `INSERT INTO mutation_journal(family_id,sequence,entity_kind,entity_id,entity_version,actor_account_id,device_id,operation,canonical_entity,accepted_at)
		VALUES($1,1,'baby',$2,1,$3,$4,'create',$5,$6)`, fcFamily, fcBaby, fcAccount, fcDevice, fcEntity("baby", fcBaby, 1, fcBabyPayload), fcAccepted)
	fcExec(t, tx, `INSERT INTO babies(family_id,id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,last_sequence,storage_bytes,name,normalized_name,normalization_version,birth_date,sort_order)
		VALUES($1,$2,1,$3,$3,$4,$5,1,1,'Ada','ada','nfc-trim-fold-nfc-v1','2026-08-01',0)`, fcFamily, fcBaby, fcAccount, fcClientTime, fcAccepted)
	fcExec(t, tx, `UPDATE families SET next_sequence=1 WHERE id=$1`, fcFamily)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// fcSeedHistory writes every journal row after the baby's, the records whose
// heads they are (columns and child rows as putFeed/putTimer left them), the
// receipts and the transient staging.
func fcSeedHistory(t *testing.T, fixture *testpg.Fixture) {
	t.Helper()
	tx := fcTenantTx(t, fixture)
	for _, history := range fcJournal[1:] {
		fcExec(t, tx, `INSERT INTO mutation_journal(family_id,sequence,entity_kind,entity_id,entity_version,actor_account_id,device_id,operation,canonical_entity,accepted_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, fcFamily, history.sequence, history.kind, history.id, history.version,
			fcAccount, fcDevice, history.operation, history.entity(), fcAccepted)
	}
	fcExec(t, tx, `UPDATE families SET next_sequence=$2 WHERE id=$1`, fcFamily, len(fcJournal))

	feed := func(id string, version, sequence int64, tombstoned bool, kind, started string, ended *string, note *string,
		volume *string, contents, amount *string, componentVersion int, session *string) {
		var tomb *string
		if tombstoned {
			value := fcAccepted
			tomb = &value
		}
		fcExec(t, tx, `INSERT INTO feed_entries(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,kind,started_at,ended_at,time_zone_id,note,volume_ml,contents,amount,component_version,feeding_session_id)
			VALUES($1,$2,$3,$4,$5,$5,$6,$7,$8,$9,200,$10,$11,$12,'Europe/Zurich',$13,$14::numeric,$15,$16,$17,$18)`,
			fcFamily, id, fcBaby, version, fcAccount, fcClientTime, fcAccepted, tomb, sequence, kind, started, ended, note,
			volume, contents, amount, componentVersion, session)
		// putFeed: scalar children, then the components replace whatever the
		// legacy projection triggers derived from them.
		fcExec(t, tx, `DELETE FROM feed_components WHERE family_id=$1 AND feed_id=$2`, fcFamily, id)
	}
	text := func(value string) *string { return &value }
	feed(fcFeedV2, 1, 2, false, "breast", "2026-09-20T08:00:00.000000Z", text("2026-09-20T08:20:00.000000Z"), nil, nil, nil, nil, 2, nil)
	fcExec(t, tx, `INSERT INTO feed_side_intervals(family_id,feed_id,position,side,seconds) VALUES($1,$2,0,'left',600),($1,$2,1,'right',540)`, fcFamily, fcFeedV2)
	fcExec(t, tx, `DELETE FROM feed_components WHERE family_id=$1 AND feed_id=$2`, fcFamily, fcFeedV2)
	fcExec(t, tx, `INSERT INTO feed_components(family_id,feed_id,component_id,position,kind,side,seconds) VALUES
		($1,$2,'fc000000-0000-4000-8000-0000000000c1',0,'breast','left',600),($1,$2,'fc000000-0000-4000-8000-0000000000c2',1,'breast','right',540)`, fcFamily, fcFeedV2)

	feed(fcFeedV3, 2, 4, false, "bottle", "2026-09-20T11:00:00.000000Z", text("2026-09-20T11:10:00.000000Z"), text("warm"), text("125.5"), text("formula"), nil, 3, nil)
	fcExec(t, tx, `DELETE FROM feed_components WHERE family_id=$1 AND feed_id=$2`, fcFamily, fcFeedV3)
	fcExec(t, tx, `INSERT INTO feed_components(family_id,feed_id,component_id,position,kind,seconds,volume_ml,contents) VALUES
		($1,$2,'fc000000-0000-4000-8000-0000000000c3',0,'bottle',600,125.5,'formula')`, fcFamily, fcFeedV3)

	feed(fcFeedSolids, 1, 5, false, "solids", "2026-09-20T13:00:00.000000Z", nil, nil, nil, nil, text("some"), 2, nil)
	fcExec(t, tx, `INSERT INTO feed_foods(family_id,feed_id,position,food) VALUES($1,$2,0,'banana'),($1,$2,1,'oat')`, fcFamily, fcFeedSolids)
	fcExec(t, tx, `DELETE FROM feed_components WHERE family_id=$1 AND feed_id=$2`, fcFamily, fcFeedSolids)
	fcExec(t, tx, `INSERT INTO feed_components(family_id,feed_id,component_id,position,kind,foods,amount) VALUES
		($1,$2,'fc000000-0000-4000-8000-0000000000c4',0,'solids',ARRAY['banana','oat'],'some')`, fcFamily, fcFeedSolids)

	feed(fcFeedV4, 1, 6, false, "breast", "2026-09-20T08:25:00.000000Z", text("2026-09-20T08:35:00.000000Z"), nil, nil, nil, nil, 4, text(fcFeedV2))
	fcExec(t, tx, `INSERT INTO feed_side_intervals(family_id,feed_id,position,side,seconds) VALUES($1,$2,0,'left',600)`, fcFamily, fcFeedV4)
	fcExec(t, tx, `DELETE FROM feed_components WHERE family_id=$1 AND feed_id=$2`, fcFamily, fcFeedV4)
	fcExec(t, tx, `INSERT INTO feed_components(family_id,feed_id,component_id,position,kind,side,seconds,started_at,ended_at) VALUES
		($1,$2,'fc000000-0000-4000-8000-0000000000c5',0,'breast','left',600,'2026-09-20T08:25:00Z','2026-09-20T08:35:00Z')`, fcFamily, fcFeedV4)

	feed(fcFeedDeleted, 2, 8, true, "breast", "2026-09-19T08:00:00.000000Z", text("2026-09-19T08:10:00.000000Z"), nil, nil, nil, nil, 2, nil)

	sleep := func(id string, version, sequence int64, tombstoned bool, started, ended string, note, override *string, payloadVersion int, session *string) {
		var tomb *string
		if tombstoned {
			value := fcAccepted
			tomb = &value
		}
		fcExec(t, tx, `INSERT INTO sleep_entries(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,started_at,ended_at,time_zone_id,note,kind_override,payload_version,sleep_session_id)
			VALUES($1,$2,$3,$4,$5,$5,$6,$7,$8,$9,150,$10,$11,'Europe/Zurich',$12,$13,$14,$15)`,
			fcFamily, id, fcBaby, version, fcAccount, fcClientTime, fcAccepted, tomb, sequence, started, ended, note, override, payloadVersion, session)
	}
	sleep(fcSleepV1, 2, 10, false, "2026-09-20T14:00:00Z", "2026-09-20T15:00:00Z", text("edited"), text("nap"), 1, nil)
	sleep(fcSleepGone, 2, 12, true, "2026-09-19T14:00:00Z", "2026-09-19T15:00:00Z", nil, nil, 1, nil)
	sleep(fcSleepV2, 1, 13, false, "2026-09-20T15:10:00Z", "2026-09-20T16:00:00Z", nil, nil, 2, text(fcSleepV1))

	fcExec(t, tx, `INSERT INTO diaper_entries(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,last_sequence,storage_bytes,started_at,time_zone_id,contents)
		VALUES($1,$2,$3,1,$4,$4,$5,$6,14,150,'2026-09-20T09:00:00Z','Europe/Zurich','wet')`, fcFamily, fcDiaper, fcBaby, fcAccount, fcClientTime, fcAccepted)

	// Stopped timers are tombstones: their component rows went with the stop.
	for _, timer := range []struct {
		id        string
		sequence  int64
		version   int
		completed string
	}{{fcTimerV2, 17, 2, fcFeedV2}, {fcTimerV3, 20, 3, fcFeedV3}, {fcTimerV4, 23, 4, fcFeedSolids}} {
		fcExec(t, tx, `INSERT INTO active_timers(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,timer_kind,started_at,time_zone_id,completed_record_id,component_version)
			VALUES($1,$2,$3,3,$4,$4,$5,$6,$6,$7,100,'feed','2026-09-19T08:00:00Z','Europe/Zurich',$8,$9)`,
			fcFamily, timer.id, fcBaby, fcAccount, fcClientTime, fcAccepted, timer.sequence, timer.completed, timer.version)
		fcExec(t, tx, `DELETE FROM timer_components WHERE family_id=$1 AND timer_id=$2`, fcFamily, timer.id)
	}
	fcExec(t, tx, `INSERT INTO active_timers(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,last_sequence,storage_bytes,timer_kind,started_at,time_zone_id,component_version)
		VALUES($1,$2,$3,1,$4,$4,$5,$6,24,100,'sleep','2026-09-20T20:00:00Z','Europe/Zurich',5)`,
		fcFamily, fcTimerV5, fcBaby, fcAccount, fcClientTime, fcAccepted)

	for _, receipt := range fcReceipts {
		var last *int64
		if receipt.cursor != 0 {
			last = &receipt.cursor
		}
		fcExec(t, tx, `INSERT INTO mutation_receipts(family_id,actor_account_id,device_id,mutation_id,semantic_hash,outcome,accepted_at,last_sequence)
			VALUES($1,$2,$3,$4,sha256(convert_to($8,'UTF8')),$5,$6,$7)`,
			fcFamily, fcAccount, fcDevice, receipt.mutationID, receipt.outcome(t, false), fcAccepted, last, receipt.mutationID)
	}

	// A snapshot staged before the cutover, in the old shapes, with its page binding.
	now := time.Now().UTC()
	fcExec(t, tx, `INSERT INTO devices(id,account_id,server_instance_id,canonical_origin,continuity_key_id,continuity_digest,created_at,updated_at)
		VALUES($1,$2,$3,'https://format.example','device.v1',decode(repeat('11',32),'hex'),$4,$4)`, fcDevice, fcAccount, fcInstance, now)
	fcExec(t, tx, `INSERT INTO sessions(session_id,account_id,device_id,lineage_root_session_id,server_instance_id,recovery_epoch,security_revision,scope,family_id,token_key_id,access_digest,refresh_digest,access_expires_at,refresh_expires_at,reauthenticate_at,absolute_expires_at,status,created_at,updated_at)
		VALUES($1,$2,$3,$1,$4,1,1,'family',$5,'session.v1',decode(repeat('22',32),'hex'),decode(repeat('33',32),'hex'),$6,$7,$7,$7,'active',$8,$8)`,
		fcSession, fcAccount, fcDevice, fcInstance, fcFamily, now.Add(time.Hour), now.Add(2*time.Hour), now)
	const snapshot = "fc000000-0000-4000-8000-0000000000e1"
	fcExec(t, tx, `INSERT INTO sync_snapshots(family_id,id,account_id,device_id,session_id,server_instance_id,recovery_epoch,security_revision,high_water_sequence,entity_count,encoded_bytes,captured_at,expires_at)
		VALUES($1,$2,$3,$4,$5,$6,1,1,24,1,512,$7,$8)`, fcFamily, snapshot, fcAccount, fcDevice, fcSession, fcInstance, now, now.Add(time.Hour))
	fcExec(t, tx, `INSERT INTO sync_snapshot_entities(family_id,snapshot_id,account_id,device_id,session_id,server_instance_id,recovery_epoch,security_revision,position,entity_kind,entity_id,entity_version,canonical_entity,encoded_bytes)
		VALUES($1,$2,$3,$4,$5,$6,1,1,0,'feed',$7,1,$8,512)`, fcFamily, snapshot, fcAccount, fcDevice, fcSession, fcInstance, fcFeedV2, fcHistoryAt(2).entity())
	fcExec(t, tx, `INSERT INTO sync_page_sessions(family_id,token_digest,token_key_id,account_id,device_id,session_id,server_instance_id,recovery_epoch,security_revision,kind,high_water_sequence,range_end_position,resume_position,page_end_position,page_item_count,page_encoded_bytes,snapshot_id,item_limit,byte_limit,created_at,expires_at)
		VALUES($1,decode(repeat('41',32),'hex'),'page.v1',$2,$3,$4,$5,1,1,'snapshot',24,1,0,1,1,512,$6,2,65536,$7,$8)`,
		fcFamily, fcAccount, fcDevice, fcSession, fcInstance, snapshot, now, now.Add(time.Hour))
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func fcCount(t *testing.T, tx pgx.Tx, query string, args ...any) int64 {
	t.Helper()
	var count int64
	if err := tx.QueryRow(t.Context(), query, args...).Scan(&count); err != nil {
		t.Fatalf("%v\n%s", err, query)
	}
	return count
}

// fcRecordHeads lists every record of the family, live or tombstoned, with
// the journal row its last_sequence names.
const fcRecordHeads = `
 SELECT 'baby' kind,id,entity_version,last_sequence FROM babies WHERE family_id=$1
 UNION ALL SELECT 'feed',id,entity_version,last_sequence FROM feed_entries WHERE family_id=$1
 UNION ALL SELECT 'sleep',id,entity_version,last_sequence FROM sleep_entries WHERE family_id=$1
 UNION ALL SELECT 'diaper',id,entity_version,last_sequence FROM diaper_entries WHERE family_id=$1
 UNION ALL SELECT 'supplement',id,entity_version,last_sequence FROM supplement_entries WHERE family_id=$1
 UNION ALL SELECT 'timer',id,entity_version,last_sequence FROM active_timers WHERE family_id=$1
 UNION ALL SELECT 'growth_profile',id,entity_version,last_sequence FROM growth_profiles WHERE family_id=$1
 UNION ALL SELECT 'measurement',id,entity_version,last_sequence FROM measurement_entries WHERE family_id=$1`

// fcSchema27SnapshotUnion is the current snapshot SQL as the schema-27 server
// had it: before 000029 gave babies an icon column. The format-cleanup tests
// stop at schema 27.
var fcSchema27SnapshotUnion = fcWithoutBabyIcon(snapshotEntitiesUnion)

func fcWithoutBabyIcon(union string) string {
	const icon = ",'icon',b.icon"
	if strings.Count(union, icon) != 1 {
		panic("snapshot union no longer reads the baby icon as " + icon)
	}
	return strings.Replace(union, icon, "", 1)
}

// fcSchema26SnapshotUnion is the snapshot SQL of the schema-26 server
// (records/snapshot.go before 000027), frozen: one branch per stored version,
// reading the scalar columns and the side and food tables 000027 drops. The
// seeds use it to write each journal head exactly as that server built it.
const fcSchema26SnapshotUnion = `
 SELECT 1 kind_order,b.id,
  jsonb_build_object('entity_kind','baby','meta',jsonb_build_object(
   'id',b.id::text,'family_id',b.family_id::text,'entity_version',b.entity_version,
   'creator_account_id',b.creator_account_id::text,'last_editor_account_id',b.last_editor_account_id::text,
   'client_mutation_time',to_char(b.client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'accepted_at',to_char(b.accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'tombstoned_at',CASE WHEN b.tombstoned_at IS NULL THEN NULL ELSE to_char(b.tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END),
   'payload',CASE WHEN b.tombstoned_at IS NOT NULL THEN NULL ELSE jsonb_build_object(
    'name',b.name,'birth_date',b.birth_date::text,'sort_order',b.sort_order) END) canonical_entity
 FROM babies b WHERE b.family_id=$1
 UNION ALL
 SELECT 2,f.id,
  jsonb_build_object('entity_kind','feed','meta',jsonb_build_object(
   'id',f.id::text,'family_id',f.family_id::text,'entity_version',f.entity_version,
   'creator_account_id',f.creator_account_id::text,'last_editor_account_id',f.last_editor_account_id::text,
   'client_mutation_time',to_char(f.client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'accepted_at',to_char(f.accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'tombstoned_at',CASE WHEN f.tombstoned_at IS NULL THEN NULL ELSE to_char(f.tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END),
   'payload',CASE WHEN f.tombstoned_at IS NOT NULL THEN NULL ELSE jsonb_build_object(
    'baby_id',f.baby_id::text,'kind',f.kind,
    'started_at',to_char(f.started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
    'ended_at',CASE WHEN f.ended_at IS NULL THEN NULL ELSE to_char(f.ended_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
    'time_zone_id',f.time_zone_id,'note',f.note) || CASE WHEN f.component_version>=2 THEN jsonb_build_object(
    'components',COALESCE((SELECT jsonb_agg(jsonb_strip_nulls(jsonb_build_object(
      'id',c.component_id::text,'kind',c.kind,'side',c.side,'seconds',c.seconds,
      'volume_ml',c.volume_ml,'contents',c.contents,'foods',c.foods,'amount',c.amount,
      'started_at',CASE WHEN c.started_at IS NULL THEN NULL ELSE to_char(c.started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
      'ended_at',CASE WHEN c.ended_at IS NULL THEN NULL ELSE to_char(c.ended_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END)) ORDER BY c.position)
      FROM feed_components c WHERE c.family_id=f.family_id AND c.feed_id=f.id),'[]'::jsonb)) ELSE jsonb_build_object(
    'sides',COALESCE((SELECT jsonb_agg(jsonb_build_object('side',s.side,'seconds',s.seconds) ORDER BY s.position)
      FROM feed_side_intervals s WHERE s.family_id=f.family_id AND s.feed_id=f.id),'[]'::jsonb),
    'volume_ml',f.volume_ml,'contents',f.contents,
    'foods',COALESCE((SELECT jsonb_agg(v.food ORDER BY v.position) FROM feed_foods v
      WHERE v.family_id=f.family_id AND v.feed_id=f.id),'[]'::jsonb),'amount',f.amount) END
      || CASE WHEN f.component_version>=4 THEN jsonb_build_object('feeding_session_id',f.feeding_session_id::text) ELSE '{}'::jsonb END END) canonical_entity
 FROM feed_entries f WHERE f.family_id=$1
 UNION ALL
 SELECT 3,s.id,
  jsonb_build_object('entity_kind','sleep','meta',jsonb_build_object(
   'id',s.id::text,'family_id',s.family_id::text,'entity_version',s.entity_version,
   'creator_account_id',s.creator_account_id::text,'last_editor_account_id',s.last_editor_account_id::text,
   'client_mutation_time',to_char(s.client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'accepted_at',to_char(s.accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'tombstoned_at',CASE WHEN s.tombstoned_at IS NULL THEN NULL ELSE to_char(s.tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END),
   'payload',CASE WHEN s.tombstoned_at IS NOT NULL THEN NULL ELSE jsonb_build_object(
    'baby_id',s.baby_id::text,'started_at',to_char(s.started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
    'ended_at',CASE WHEN s.ended_at IS NULL THEN NULL ELSE to_char(s.ended_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
    'time_zone_id',s.time_zone_id,'note',s.note,'kind_override',s.kind_override) || CASE WHEN s.payload_version>=2 THEN jsonb_build_object('sleep_session_id',s.sleep_session_id::text) ELSE '{}'::jsonb END END) canonical_entity
 FROM sleep_entries s WHERE s.family_id=$1
 UNION ALL
 SELECT 4,d.id,
  jsonb_build_object('entity_kind','diaper','meta',jsonb_build_object(
   'id',d.id::text,'family_id',d.family_id::text,'entity_version',d.entity_version,
   'creator_account_id',d.creator_account_id::text,'last_editor_account_id',d.last_editor_account_id::text,
   'client_mutation_time',to_char(d.client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'accepted_at',to_char(d.accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'tombstoned_at',CASE WHEN d.tombstoned_at IS NULL THEN NULL ELSE to_char(d.tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END),
   'payload',CASE WHEN d.tombstoned_at IS NOT NULL THEN NULL ELSE jsonb_build_object(
    'baby_id',d.baby_id::text,'started_at',to_char(d.started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
    'ended_at',CASE WHEN d.ended_at IS NULL THEN NULL ELSE to_char(d.ended_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
    'time_zone_id',d.time_zone_id,'note',d.note,'contents',d.contents,'consistency',d.consistency,'color',d.color) END) canonical_entity
 FROM diaper_entries d WHERE d.family_id=$1
 UNION ALL
 SELECT 6,d.id,
  jsonb_build_object('entity_kind','supplement','meta',jsonb_build_object(
   'id',d.id::text,'family_id',d.family_id::text,'entity_version',d.entity_version,
   'creator_account_id',d.creator_account_id::text,'last_editor_account_id',d.last_editor_account_id::text,
   'client_mutation_time',to_char(d.client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'accepted_at',to_char(d.accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'tombstoned_at',CASE WHEN d.tombstoned_at IS NULL THEN NULL ELSE to_char(d.tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END),
   'payload',CASE WHEN d.tombstoned_at IS NOT NULL THEN NULL ELSE jsonb_build_object(
    'baby_id',d.baby_id::text,'started_at',to_char(d.started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
    'time_zone_id',d.time_zone_id,'note',d.note,'name',d.name,'logged_at',to_char(d.logged_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"')) END) canonical_entity
 FROM supplement_entries d WHERE d.family_id=$1
 UNION ALL
 SELECT 5,t.id,
  jsonb_build_object('entity_kind','timer','meta',jsonb_build_object(
   'id',t.id::text,'family_id',t.family_id::text,'entity_version',t.entity_version,
   'creator_account_id',t.creator_account_id::text,'last_editor_account_id',t.last_editor_account_id::text,
   'client_mutation_time',to_char(t.client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'accepted_at',to_char(t.accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'tombstoned_at',CASE WHEN t.tombstoned_at IS NULL THEN NULL ELSE to_char(t.tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END),
   'payload',CASE WHEN t.tombstoned_at IS NOT NULL THEN NULL ELSE jsonb_build_object(
    'baby_id',t.baby_id::text,'timer_kind',t.timer_kind,
    'started_at',to_char(t.started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
    'time_zone_id',t.time_zone_id,
    'current_side',t.current_side,
    'current_side_started_at',CASE WHEN t.current_side_started_at IS NULL THEN NULL ELSE to_char(t.current_side_started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END) || CASE WHEN t.component_version>=3 THEN jsonb_build_object('current_bottle_started_at',CASE WHEN t.current_bottle_started_at IS NULL THEN NULL ELSE to_char(t.current_bottle_started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END) ELSE '{}'::jsonb END || CASE WHEN t.component_version>=2 THEN jsonb_build_object(
    'completed_components',COALESCE((SELECT jsonb_agg(jsonb_strip_nulls(jsonb_build_object(
      'id',c.component_id::text,'kind',c.kind,'side',c.side,'seconds',c.seconds,
      'volume_ml',c.volume_ml,'contents',c.contents,'foods',c.foods,'amount',c.amount,
      'started_at',CASE WHEN c.started_at IS NULL THEN NULL ELSE to_char(c.started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
      'ended_at',CASE WHEN c.ended_at IS NULL THEN NULL ELSE to_char(c.ended_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END)) ORDER BY c.position)
      FROM timer_components c WHERE c.family_id=t.family_id AND c.timer_id=t.id),'[]'::jsonb)) ELSE jsonb_build_object(
    'completed_sides',COALESCE((SELECT jsonb_agg(jsonb_build_object('side',i.side,'seconds',i.seconds) ORDER BY i.position)
      FROM timer_side_intervals i WHERE i.family_id=t.family_id AND i.timer_id=t.id),'[]'::jsonb),
    'pending_volume_ml',t.pending_volume_ml,'pending_contents',t.pending_contents) END
    || CASE WHEN t.component_version>=5 THEN jsonb_build_object('session_id',t.session_id::text) ELSE '{}'::jsonb END END) canonical_entity
 FROM active_timers t WHERE t.family_id=$1
 UNION ALL
 SELECT 7,d.id,
  jsonb_build_object('entity_kind','growth_profile','meta',jsonb_build_object(
   'id',d.id::text,'family_id',d.family_id::text,'entity_version',d.entity_version,
   'creator_account_id',d.creator_account_id::text,'last_editor_account_id',d.last_editor_account_id::text,
   'client_mutation_time',to_char(d.client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'accepted_at',to_char(d.accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'tombstoned_at',CASE WHEN d.tombstoned_at IS NULL THEN NULL ELSE to_char(d.tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END),
   'payload',CASE WHEN d.tombstoned_at IS NOT NULL THEN NULL ELSE jsonb_build_object(
    'baby_id',d.baby_id::text,'reference_sex',d.reference_sex,'expected_due_date',d.expected_due_date::text) END) canonical_entity
 FROM growth_profiles d WHERE d.family_id=$1
 UNION ALL
 SELECT 8,d.id,
  jsonb_build_object('entity_kind','measurement','meta',jsonb_build_object(
   'id',d.id::text,'family_id',d.family_id::text,'entity_version',d.entity_version,
   'creator_account_id',d.creator_account_id::text,'last_editor_account_id',d.last_editor_account_id::text,
   'client_mutation_time',to_char(d.client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'accepted_at',to_char(d.accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'tombstoned_at',CASE WHEN d.tombstoned_at IS NULL THEN NULL ELSE to_char(d.tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END),
   'payload',CASE WHEN d.tombstoned_at IS NOT NULL THEN NULL ELSE jsonb_build_object(
    'baby_id',d.baby_id::text,'measured_at',to_char(d.measured_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),'logged_at',to_char(d.logged_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),'time_zone_id',d.time_zone_id,'weight_kg',d.weight_kg,'length_cm',d.length_cm,'head_circumference_cm',d.head_circumference_cm,'length_position',d.length_position) END) canonical_entity
 FROM measurement_entries d WHERE d.family_id=$1
`

// fcAssertHeadsMatchSnapshot compares the journal head of every record in the
// family with what the given snapshot SQL builds from the typed tables for
// the same record, and returns how many records it compared: before 000027
// against the schema-26 server's SQL, after it against the current one.
func fcAssertHeadsMatchSnapshot(t *testing.T, fixture *testpg.Fixture, union string) int {
	t.Helper()
	tx := fcTenantTx(t, fixture)
	// Release the read before anything migrates: an open transaction would
	// hold the locks 000027's ALTER TABLE statements wait for.
	defer func() { _ = tx.Rollback(t.Context()) }()
	rows, err := tx.Query(t.Context(), `SELECT head.kind,head.id::text,head.last_sequence,
			coalesce(typed.canonical_entity=j.canonical_entity,false),typed.canonical_entity::text,coalesce(j.canonical_entity::text,'(no journal row)')
		FROM (`+fcRecordHeads+`) head
		JOIN (`+union+`) typed ON typed.id=head.id AND typed.canonical_entity->>'entity_kind'=head.kind
		LEFT JOIN mutation_journal j ON j.family_id=$1 AND j.sequence=head.last_sequence
			AND j.entity_kind=head.kind AND j.entity_id=head.id AND j.entity_version=head.entity_version
		ORDER BY head.kind,head.id`, fcFamily)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	compared := 0
	for rows.Next() {
		var kind, id, snapshot, journal string
		var sequence int64
		var matches bool
		if err := rows.Scan(&kind, &id, &sequence, &matches, &snapshot, &journal); err != nil {
			t.Fatal(err)
		}
		compared++
		if !matches {
			t.Errorf("%s %s journal head %d differs from its snapshot\nsnapshot=%s\njournal= %s", kind, id, sequence, snapshot, journal)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var records int
	if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM (`+fcRecordHeads+`) head`, fcFamily).Scan(&records); err != nil {
		t.Fatal(err)
	}
	if compared == 0 || compared != records {
		t.Errorf("compared %d journal heads of %d records", compared, records)
	}
	return compared
}

func TestFormatCleanupMigrationConvertsStoredRecordsAndHistory(t *testing.T) {
	fixture := testpg.NewThrough(t, 26)
	fcSeedFamily(t, fixture)
	fcSeedHistory(t, fixture)

	// The migration runs as the production-equivalent migrator: it owns the
	// tables, cannot bypass RLS, and with no tenant context sees no family rows.
	var super, bypass bool
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&super, &bypass); err != nil || super || bypass {
		t.Fatalf("migrator superuser=%t bypassrls=%t err=%v", super, bypass, err)
	}
	for _, table := range fcBracketTables {
		var owned, forced bool
		if err := fixture.Owner.QueryRow(t.Context(), `SELECT relowner=(SELECT oid FROM pg_roles WHERE rolname=current_user),relforcerowsecurity
			FROM pg_class WHERE oid=('public.'||$1)::regclass`, table).Scan(&owned, &forced); err != nil || !owned || !forced {
			t.Fatalf("%s owned=%t forced=%t err=%v", table, owned, forced, err)
		}
	}
	var unscoped int64
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM feed_entries)+(SELECT count(*) FROM mutation_journal)`).Scan(&unscoped); err != nil || unscoped != 0 {
		t.Fatalf("migrator without tenant context sees %d rows err=%v", unscoped, err)
	}
	// The seeds are what the schema-26 server built: each live head equals
	// that server's snapshot of the record.
	fcAssertHeadsMatchSnapshot(t, fixture, fcSchema26SnapshotUnion)
	seeded := fcTenantTx(t, fixture)
	for _, table := range fcStagingTables {
		if got := fcCount(t, seeded, `SELECT count(*) FROM `+table); got != 1 {
			t.Fatalf("seeded %s rows=%d", table, got)
		}
	}
	if got := fcCount(t, seeded, `SELECT count(*) FROM mutation_journal WHERE operation IN ('timer_switch_side','timer_start_bottle')`); got != 2 {
		t.Fatalf("seeded legacy operations=%d", got)
	}
	if err := seeded.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := fixture.MigrateThrough(t.Context(), 27); err != nil {
		t.Fatal(err)
	}

	tx := fcTenantTx(t, fixture)
	if got := fcCount(t, tx, `SELECT current_version FROM schema_compatibility WHERE singleton`); got != 27 {
		t.Fatalf("schema=%d", got)
	}

	t.Run("record versions", func(t *testing.T) {
		for _, want := range []struct {
			table, column string
			version       int64
			rows          int64
		}{{"feed_entries", "component_version", 4, 5}, {"sleep_entries", "payload_version", 2, 3}, {"active_timers", "component_version", 5, 4}} {
			query := fmt.Sprintf(`SELECT count(*) FILTER (WHERE %s=%d)*1000+count(*) FROM %s WHERE family_id=$1`, want.column, want.version, want.table)
			if got := fcCount(t, tx, query, fcFamily); got != want.rows*1000+want.rows {
				t.Errorf("%s: %d of %d rows at %s=%d", want.table, got/1000, got%1000, want.column, want.version)
			}
		}
		// Every converted live record is published once more, in (kind, id)
		// order after the old high water; current-format, tombstoned and
		// other-kind records are not.
		fcAssertAppended(t, tx, int64(len(fcJournal)), []fcAppended{
			{"feed", fcFeedV2, 25, 2, fcAccount, fcDevice},
			{"feed", fcFeedV3, 26, 3, fcAccount, fcDevice},
			{"feed", fcFeedSolids, 27, 2, fcAccount, fcDevice},
			{"sleep", fcSleepV1, 28, 3, fcAccount, fcDevice},
		})
		for _, unchanged := range []struct {
			table, id         string
			version, sequence int64
		}{{"babies", fcBaby, 1, 1}, {"feed_entries", fcFeedV4, 1, 6}, {"feed_entries", fcFeedDeleted, 2, 8},
			{"sleep_entries", fcSleepGone, 2, 12}, {"sleep_entries", fcSleepV2, 1, 13}, {"diaper_entries", fcDiaper, 1, 14},
			{"active_timers", fcTimerV2, 3, 17}, {"active_timers", fcTimerV3, 3, 20}, {"active_timers", fcTimerV4, 3, 23},
			{"active_timers", fcTimerV5, 1, 24}} {
			if got := fcCount(t, tx, `SELECT entity_version*1000+last_sequence FROM `+unchanged.table+` WHERE family_id=$1 AND id=$2`,
				fcFamily, unchanged.id); got != unchanged.version*1000+unchanged.sequence {
				t.Errorf("%s %s version/head=%d, want unchanged %d/%d", unchanged.table, unchanged.id, got, unchanged.version, unchanged.sequence)
			}
		}
		if got := fcCount(t, tx, `SELECT count(*) FROM (
			SELECT id,entity_version,last_sequence FROM feed_entries UNION ALL SELECT id,entity_version,last_sequence FROM sleep_entries
			UNION ALL SELECT id,entity_version,last_sequence FROM active_timers) r
			WHERE (id,entity_version,last_sequence) NOT IN (SELECT entity_id,entity_version,sequence FROM mutation_journal)`); got != 0 {
			t.Errorf("%d records lost their journal head", got)
		}
		// The converted v2 feed and v1 sleep are the roots a current-format
		// part already continued, so they keep no link and stay roots.
		if got := fcCount(t, tx, `SELECT count(*) FROM feed_entries WHERE feeding_session_id IS NOT NULL AND id<>$1`, fcFeedV4); got != 0 {
			t.Errorf("%d feeds gained a session link", got)
		}
		if got := fcCount(t, tx, `SELECT count(*) FROM sleep_entries WHERE sleep_session_id IS NOT NULL AND id<>$1`, fcSleepV2); got != 0 {
			t.Errorf("%d sleeps gained a session link", got)
		}
		if got := fcCount(t, tx, `SELECT count(*) FROM active_timers WHERE session_id IS NOT NULL`); got != 0 {
			t.Errorf("%d timers gained a session link", got)
		}
		if got := fcCount(t, tx, `SELECT count(*) FROM feed_components WHERE component_id::text LIKE 'fc000000-0000-4000-8000-0000000000c_'`); got != 5 {
			t.Errorf("feed components=%d", got)
		}
	})

	t.Run("journal rows are the current shape", func(t *testing.T) {
		for _, history := range fcJournal {
			var raw string
			var matches bool
			var operation string
			if err := tx.QueryRow(t.Context(), `SELECT canonical_entity::text,canonical_entity=$3::jsonb,operation FROM mutation_journal WHERE family_id=$1 AND sequence=$2`,
				fcFamily, history.sequence, history.expected(t)).Scan(&raw, &matches, &operation); err != nil {
				t.Fatal(err)
			}
			if !matches {
				t.Errorf("journal %d = %s\nwant %s", history.sequence, raw, history.expected(t))
			}
			if operation != history.wantOp {
				t.Errorf("journal %d operation=%s want %s", history.sequence, operation, history.wantOp)
			}
			fcAssertCurrentEntity(t, tx, fmt.Sprintf("journal %d", history.sequence), raw)
		}
		if got := fcCount(t, tx, `SELECT count(*) FROM mutation_journal WHERE operation NOT IN ('create','update','delete','timer_start','timer_finish_bottle','timer_stop')`); got != 0 {
			t.Errorf("%d legacy operations left", got)
		}
	})

	t.Run("receipt outcomes are the current shape", func(t *testing.T) {
		for _, receipt := range fcReceipts {
			var raw string
			var matches bool
			if err := tx.QueryRow(t.Context(), `SELECT outcome::text,outcome=$3::jsonb FROM mutation_receipts WHERE device_id=$1 AND mutation_id=$2`,
				fcDevice, receipt.mutationID, receipt.outcome(t, true)).Scan(&raw, &matches); err != nil {
				t.Fatal(err)
			}
			if !matches {
				t.Errorf("receipt %s = %s\nwant %s", receipt.mutationID, raw, receipt.outcome(t, true))
			}
			var outcome protocol.Outcome
			if err := json.Unmarshal([]byte(raw), &outcome); err != nil {
				t.Fatalf("receipt %s does not decode: %v", receipt.mutationID, err)
			}
			var parts struct {
				Canonical json.RawMessage   `json:"canonical_entity"`
				Related   []json.RawMessage `json:"related_entities"`
			}
			if err := json.Unmarshal([]byte(raw), &parts); err != nil {
				t.Fatal(err)
			}
			if string(parts.Canonical) != "null" {
				fcAssertCurrentEntity(t, tx, "receipt "+receipt.mutationID, string(parts.Canonical))
			}
			for index, related := range parts.Related {
				fcAssertCurrentEntity(t, tx, fmt.Sprintf("receipt %s related %d", receipt.mutationID, index), string(related))
			}
		}
	})

	t.Run("journal heads equal the snapshot of their records", func(t *testing.T) {
		fcAssertHeadsMatchSnapshot(t, fixture, fcSchema27SnapshotUnion)
	})

	t.Run("transient staging is cleared", func(t *testing.T) {
		for _, table := range fcStagingTables {
			if got := fcCount(t, tx, `SELECT count(*) FROM `+table); got != 0 {
				t.Errorf("%s rows=%d", table, got)
			}
		}
	})

	t.Run("legacy operations can no longer be journaled", func(t *testing.T) {
		_, err := tx.Exec(t.Context(), `SAVEPOINT legacy_operation`)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(t.Context(), `INSERT INTO mutation_journal(family_id,sequence,entity_kind,entity_id,entity_version,actor_account_id,device_id,operation,canonical_entity,accepted_at)
			VALUES($1,99,'timer',$2,4,$3,$4,'timer_switch_side',$5,$6)`, fcFamily, fcTimerV5, fcAccount, fcDevice, fcEntity("timer", fcTimerV5, 4, fcTimerV5Payload), fcAccepted)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.ConstraintName != "mutation_journal_operation_check" {
			t.Fatalf("legacy operation insert err=%v", err)
		}
		if _, err := tx.Exec(t.Context(), `ROLLBACK TO SAVEPOINT legacy_operation`); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("legacy projection is gone and the bracket is closed", func(t *testing.T) {
		if got := fcCount(t, tx, `SELECT count(*) FROM pg_trigger WHERE tgname IN ('feed_components_from_parent','feed_components_from_sides',
			'feed_components_from_foods','timer_components_from_parent','timer_components_from_sides')`); got != 0 {
			t.Errorf("%d legacy triggers left", got)
		}
		if got := fcCount(t, tx, `SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
			WHERE (n.nspname='app' AND p.proname IN ('project_legacy_feed_components','project_legacy_feed_parent_components',
				'project_legacy_timer_components','project_legacy_timer_parent_components','rebuild_feed_components',
				'rebuild_timer_components','legacy_component_id','sha1'))
			   OR p.proname LIKE 'format\_cleanup\_%'`); got != 0 {
			t.Errorf("%d legacy or migration-temporary functions left", got)
		}
		for _, table := range fcBracketTables {
			var forced bool
			if err := tx.QueryRow(t.Context(), `SELECT relforcerowsecurity FROM pg_class WHERE oid=('public.'||$1)::regclass`, table).Scan(&forced); err != nil || !forced {
				t.Errorf("%s FORCE ROW LEVEL SECURITY=%t err=%v", table, forced, err)
			}
		}
		if err := fixture.Owner.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM feed_entries)+(SELECT count(*) FROM mutation_journal)`).Scan(&unscoped); err != nil || unscoped != 0 {
			t.Errorf("migrator without tenant context sees %d rows after the migration err=%v", unscoped, err)
		}
		// Manifest 27 is manifest 26 less the grants on the dropped tables.
		dropped := []string{"public.feed_side_intervals", "public.feed_foods", "public.timer_side_intervals"}
		if got := fcCount(t, tx, `SELECT count(*) FROM schema_privilege_manifest WHERE schema_version=26
			AND object_kind='table' AND object_identity=ANY($1)`, dropped); got != 3 {
			t.Errorf("manifest 26 holds %d grants on the dropped tables, want 3", got)
		}
		if got := fcCount(t, tx, `SELECT count(*) FROM (
			(SELECT principal,object_kind,object_identity,privileges FROM schema_privilege_manifest WHERE schema_version=26
			  AND NOT (object_kind='table' AND object_identity=ANY($1))
			 EXCEPT ALL SELECT principal,object_kind,object_identity,privileges FROM schema_privilege_manifest WHERE schema_version=27)
			UNION ALL
			(SELECT principal,object_kind,object_identity,privileges FROM schema_privilege_manifest WHERE schema_version=27
			 EXCEPT ALL SELECT principal,object_kind,object_identity,privileges FROM schema_privilege_manifest WHERE schema_version=26
			  AND NOT (object_kind='table' AND object_identity=ANY($1)))) differ`, dropped); got != 0 {
			t.Errorf("privilege manifest 27 differs from 26 less the dropped tables by %d rows", got)
		}
		if err := database.VerifyRoleSeparation(t.Context(), fixture.Runtime, fixture.Projector, 27); err != nil {
			t.Errorf("schema-27 privilege snapshot rejected: %v", err)
		}
	})

	t.Run("legacy storage is dropped and each version is pinned", func(t *testing.T) {
		for _, table := range fcDroppedTables {
			if got := fcCount(t, tx, `SELECT count(*) FROM pg_class WHERE oid=to_regclass('public.'||$1)`, table); got != 0 {
				t.Errorf("table %s still exists", table)
			}
		}
		if got := fcCount(t, tx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='public'
			AND ((table_name='feed_entries' AND column_name IN ('volume_ml','contents','amount'))
			  OR (table_name='active_timers' AND column_name IN ('pending_volume_ml','pending_contents')))`); got != 0 {
			t.Errorf("%d scalar columns left", got)
		}
		if got := fcCount(t, tx, `SELECT count(*) FROM pg_constraint
			WHERE conname IN ('active_timers_session_requires_v5','sleep_session_requires_v2')`); got != 0 {
			t.Errorf("%d version guards on session links left", got)
		}
		for _, pin := range []struct{ table, column, check, value, other string }{
			{"feed_entries", "component_version", "feed_entries_component_version_check", "4", "3"},
			{"active_timers", "component_version", "active_timers_component_version_check", "5", "4"},
			{"sleep_entries", "payload_version", "sleep_entries_payload_version_check", "2", "1"},
		} {
			var definition, defaultValue string
			if err := tx.QueryRow(t.Context(), `SELECT pg_get_constraintdef(c.oid),pg_get_expr(d.adbin,d.adrelid)
				FROM pg_constraint c JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attname=$2
				JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
				WHERE c.conrelid=('public.'||$1)::regclass AND c.conname=$3`, pin.table, pin.column, pin.check).Scan(&definition, &defaultValue); err != nil {
				t.Fatalf("%s: %v", pin.check, err)
			}
			if definition != "CHECK (("+pin.column+" = "+pin.value+"))" || defaultValue != pin.value {
				t.Errorf("%s.%s check=%q default=%q, want = %s", pin.table, pin.column, definition, defaultValue, pin.value)
			}
			if _, err := tx.Exec(t.Context(), `SAVEPOINT pinned_version`); err != nil {
				t.Fatal(err)
			}
			_, err := tx.Exec(t.Context(), `UPDATE `+pin.table+` SET `+pin.column+`=`+pin.other+` WHERE family_id=$1`, fcFamily)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.ConstraintName != pin.check {
				t.Errorf("%s.%s=%s err=%v, want %s", pin.table, pin.column, pin.other, err, pin.check)
			}
			if _, err := tx.Exec(t.Context(), `ROLLBACK TO SAVEPOINT pinned_version`); err != nil {
				t.Fatal(err)
			}
		}
		var stateShape string
		if err := tx.QueryRow(t.Context(), `SELECT pg_get_constraintdef(oid) FROM pg_constraint
			WHERE conrelid='public.active_timers'::regclass AND conname='active_timers_state_shape'`).Scan(&stateShape); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"timer_kind", "current_side", "current_bottle_started_at", "completed_record_id"} {
			if !strings.Contains(stateShape, key) {
				t.Errorf("active_timers_state_shape lost %s: %s", key, stateShape)
			}
		}
		if strings.Contains(stateShape, "pending") || strings.Contains(stateShape, "component_version") {
			t.Errorf("active_timers_state_shape still names legacy columns: %s", stateShape)
		}
	})
}

// fcAssertCurrentEntity decodes a stored canonical entity with the server's
// decoders and checks that it is the current shape: the stored-entity decoder
// reads it, the inbound decoder accepts its
// payload as a current-version create (timer_start for a timer), and
// marshaling the decoded entity reproduces the stored document.
func fcAssertCurrentEntity(t *testing.T, tx pgx.Tx, label, raw string) {
	t.Helper()
	var entity protocol.CanonicalEntity
	if err := json.Unmarshal([]byte(raw), &entity); err != nil {
		t.Errorf("%s does not decode: %v", label, err)
		return
	}
	var parts struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal([]byte(raw), &parts); err != nil {
		t.Fatal(err)
	}
	if entity.Payload != nil {
		operation := "create"
		if entity.EntityKind == "timer" {
			operation = "timer_start"
		}
		mutation := fmt.Sprintf(`{"mutation_id":"fc000000-0000-4000-8000-0000000000b1","entity_id":%q,"entity_kind":%q,"operation":%q,`+
			`"payload_version":%d,"observed_entity_version":0,"client_mutation_time":%q,"payload":%s}`,
			entity.Meta.ID, entity.EntityKind, operation, protocol.CurrentPayloadVersion(entity.EntityKind), fcClientTime, parts.Payload)
		if _, err := protocol.DecodeMutation([]byte(mutation)); err != nil {
			t.Errorf("%s payload is not a current-version %s: %v\n%s", label, entity.EntityKind, err, parts.Payload)
		}
	}
	marshaled, err := json.Marshal(entity)
	if err != nil {
		t.Fatal(err)
	}
	var same bool
	if err := tx.QueryRow(t.Context(), `SELECT $1::jsonb=$2::jsonb`, raw, string(marshaled)).Scan(&same); err != nil {
		t.Fatal(err)
	}
	if !same {
		t.Errorf("%s is not what the server marshals for it\nstored=   %s\nmarshaled=%s", label, raw, marshaled)
	}
}

// Each precondition refuses the whole migration; the database stays at 26 and
// the migration succeeds once the violation is gone.
func TestFormatCleanupMigrationRefusesUnconvertibleData(t *testing.T) {
	fixture := testpg.NewThrough(t, 26)
	fcSeedFamily(t, fixture)

	const (
		violatingFeed  = "fc000000-0000-4000-8000-000000000070"
		violatingTimer = "fc000000-0000-4000-8000-000000000071"
		importID       = "fc000000-0000-4000-8000-000000000072"
		receiptID      = "fc000000-0000-4000-8000-000000000073"
	)
	scalarFeed := `{"baby_id":"` + fcBaby + `","kind":"breast","started_at":"2026-09-20T08:00:00.000000Z","ended_at":null,
		"time_zone_id":"Europe/Zurich","note":null,"sides":[{"side":"left","seconds":60}],"volume_ml":null,"contents":null,"foods":[],"amount":null}`
	completedSides := `{"baby_id":"` + fcBaby + `","timer_kind":"feed","started_at":"2026-09-20T08:00:00.000000Z",
		"time_zone_id":"Europe/Zurich","current_side":"left","current_side_started_at":"2026-09-20T08:00:00.000000Z",
		"completed_sides":[],"pending_volume_ml":null,"pending_contents":null}`
	journalHead := func(kind, id, payload string) string {
		return fmt.Sprintf(`INSERT INTO mutation_journal(family_id,sequence,entity_kind,entity_id,entity_version,actor_account_id,device_id,operation,canonical_entity,accepted_at)
			VALUES('%s',2,'%s','%s',1,'%s','%s','create','%s','%s')`, fcFamily, kind, id, fcAccount, fcDevice, fcEntity(kind, id, 1, payload), fcAccepted)
	}
	insertFeed := func(componentVersion int, tombstoned bool) string {
		tomb := "NULL"
		if tombstoned {
			tomb = "'" + fcAccepted + "'"
		}
		return journalHead("feed", violatingFeed, fcFeedV2Payload) + ";" + fmt.Sprintf(`INSERT INTO feed_entries(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,kind,started_at,time_zone_id,component_version)
			VALUES('%s','%s','%s',1,'%s','%s','%s','%s',%s,2,100,'breast','2026-09-20T08:00:00Z','Europe/Zurich',%d)`,
			fcFamily, violatingFeed, fcBaby, fcAccount, fcAccount, fcClientTime, fcAccepted, tomb, componentVersion)
	}
	insertTimer := func(componentVersion int, tombstoned bool) string {
		tomb := "NULL"
		if tombstoned {
			tomb = "'" + fcAccepted + "'"
		}
		return journalHead("timer", violatingTimer, fcTimerV4StartPayload) + ";" + fmt.Sprintf(`INSERT INTO active_timers(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,timer_kind,started_at,time_zone_id,current_side,current_side_started_at,component_version)
			VALUES('%s','%s','%s',1,'%s','%s','%s','%s',%s,2,100,'feed','2026-09-20T08:00:00Z','Europe/Zurich','left','2026-09-20T08:00:00Z',%d)`,
			fcFamily, violatingTimer, fcBaby, fcAccount, fcAccount, fcClientTime, fcAccepted, tomb, componentVersion)
	}
	receipt := func(outcome string) string {
		return fmt.Sprintf(`INSERT INTO mutation_receipts(family_id,actor_account_id,device_id,mutation_id,semantic_hash,outcome,accepted_at)
			VALUES('%s','%s','%s','%s',sha256('x'::bytea),'%s','%s')`, fcFamily, fcAccount, fcDevice, receiptID, outcome, fcAccepted)
	}
	cleanup := fmt.Sprintf(`DELETE FROM feed_entries WHERE id='%[1]s'; DELETE FROM active_timers WHERE id='%[2]s';
		DELETE FROM mutation_journal WHERE sequence>1; DELETE FROM mutation_receipts WHERE family_id='%[3]s';
		DELETE FROM import_sessions WHERE family_id='%[3]s'`, violatingFeed, violatingTimer, fcFamily)
	importSession := func(mappingComplete bool, expiresIn string) string {
		finalized, mapped := "NULL", "NULL"
		if mappingComplete {
			finalized, mapped = "now()-interval '30 minutes'", "now()-interval '20 minutes'"
		}
		return fmt.Sprintf(`INSERT INTO import_sessions(family_id,import_id,initiating_account_id,initiating_device_id,import_mode,normalization_version,total_record_count,manifest_complete,finalized_at,mapping_complete,mapping_completed_at,created_at,expires_at,storage_bytes)
			VALUES('%s','%s','%s','%s','preserve_local_ids','nfc-trim-fold-nfc-v1',0,%t,%s,%t,%s,now()-interval '2 hours',now()+interval '%s',1)`,
			fcFamily, importID, fcAccount, fcDevice, mappingComplete, finalized, mappingComplete, mapped, expiresIn)
	}

	for _, test := range []struct {
		name, setup, refusal string
	}{
		{"unfinished import", importSession(false, "1 hour"), "1 unfinished import"},
		{"v1 feed", insertFeed(1, false), "1 feed(s) at component_version 1"},
		{"tombstoned v1 feed", insertFeed(1, true), "1 feed(s) at component_version 1"},
		{"tombstoned v1 timer", insertTimer(1, true), "1 timer(s) at component_version 1"},
		{"running v4 timer", insertTimer(4, false), "1 running timer(s) below component_version 5"},
		{"live feed without components", insertFeed(2, false) + `; DELETE FROM feed_components WHERE feed_id='` + violatingFeed + `'`, "1 live feed(s) without components"},
		{"scalar feed in the journal", journalHead("feed", violatingFeed, scalarFeed), "1 journal row(s) hold a scalar feed"},
		{"completed_sides timer in the journal", journalHead("timer", violatingTimer, completedSides), "1 journal row(s) hold a scalar feed or completed_sides timer"},
		{"scalar feed related to a receipt", receipt(`{"mutation_id":"` + receiptID + `","status":"applied","original_status":null,"canonical_entity":null,"related_entities":[` +
			fcEntity("feed", violatingFeed, 1, scalarFeed) + `],"cursor":null,"code":null,"overwrote_newer_version":false}`), "1 receipt(s) hold"},
		{"completed_sides timer in a receipt", receipt(`{"mutation_id":"` + receiptID + `","status":"applied","original_status":null,"canonical_entity":` +
			fcEntity("timer", violatingTimer, 1, completedSides) + `,"related_entities":[],"cursor":null,"code":null,"overwrote_newer_version":false}`), "1 receipt(s) hold"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx := fcTenantTx(t, fixture)
			fcExec(t, tx, test.setup)
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			defer func() {
				cleanupTx := fcTenantTx(t, fixture)
				fcExec(t, cleanupTx, cleanup)
				if err := cleanupTx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
			}()
			err := fixture.MigrateThrough(t.Context(), 27)
			if err == nil || !strings.Contains(err.Error(), "format_cleanup_precondition: "+test.refusal) {
				t.Fatalf("migration err=%v, want refusal %q", err, test.refusal)
			}
			var version int64
			if err := fixture.Owner.QueryRow(t.Context(), `SELECT current_version FROM schema_compatibility WHERE singleton`).Scan(&version); err != nil || version != 26 {
				t.Fatalf("schema after refusal=%d err=%v", version, err)
			}
		})
	}

	// Finished and expired imports do not block; neither does a stopped v4
	// timer or a v2 feed with components.
	tx := fcTenantTx(t, fixture)
	fcExec(t, tx, importSession(true, "1 hour"))
	fcExec(t, tx, insertTimer(4, true))
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	expired := fcTenantTx(t, fixture)
	fcExec(t, expired, strings.Replace(strings.Replace(importSession(false, "1 hour"), importID, "fc000000-0000-4000-8000-000000000074", 1),
		"now()+interval '1 hour'", "now()-interval '1 hour'", 1))
	if err := expired.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := fixture.MigrateThrough(t.Context(), 27); err != nil {
		t.Fatalf("clean data refused: %v", err)
	}
}
