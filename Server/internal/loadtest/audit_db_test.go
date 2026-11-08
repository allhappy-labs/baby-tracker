package loadtest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5/pgxpool"
)

// auditFixtures are the creates applyAndLedger writes, in order: one entity
// of every record kind and a timer, each naming the fixtures' baby.
var auditFixtures = []string{"feed-bottle.json", "diaper.json", "sleep.json", "supplement.json", "measurement.json",
	"feed-breast.json", "growth-profile.json", "immunization.json", "pump.json", "timer-sleep.json"}

// fixtureBaby is the baby every fixture names.
const fixtureBaby = "10000000-0000-4000-8000-000000000001"

// applyOne applies m through h.Service as the owner and returns its outcome
// as the generator reads it: the sync response encoded as the HTTP handler
// writes it, then decoded.
func applyOne(t *testing.T, h *testkit.Harness, m protocol.Mutation) protocol.Outcome {
	t.Helper()
	request := h.SyncRequest(nil, nil)
	if m.EntityKind == "timer" { // the timer surface; any other write is refused under it
		request.Capabilities = append(request.Capabilities, protocol.CapabilityTimerCommandsV1)
	}
	request.Mutations = []protocol.Mutation{m}
	response, err := h.Service.ApplyAndPull(t.Context(), h.Owner, request)
	if err != nil {
		t.Fatalf("%s %s: %v", m.Operation, m.EntityKind, err)
	}
	decoded := testkit.Decoded(t, response)
	if len(decoded.Outcomes) != 1 || decoded.Outcomes[0].Status != "applied" || decoded.Outcomes[0].CanonicalEntity == nil {
		codes := []string{}
		for _, o := range decoded.Outcomes {
			if o.Code != nil {
				codes = append(codes, o.Status+"/"+*o.Code)
			}
		}
		t.Fatalf("%s %s: outcomes %+v (%v)", m.Operation, m.EntityKind, decoded.Outcomes, codes)
	}
	return decoded.Outcomes[0]
}

// wireSHA256 is the ledger's hash: sha256 of json.Marshal of the decoded
// canonical entity, as the generator computes it.
func wireSHA256(t *testing.T, entity protocol.CanonicalEntity) string {
	t.Helper()
	raw, err := json.Marshal(entity)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ledgerEntryOf is the ledger entry the generator appends for an applied
// outcome.
func ledgerEntryOf(t *testing.T, familyID string, o protocol.Outcome) LedgerEntry {
	t.Helper()
	e := o.CanonicalEntity
	return LedgerEntry{FamilyID: familyID, EntityID: e.Meta.ID, Kind: e.EntityKind, Version: e.Meta.EntityVersion,
		Deleted: e.Meta.TombstonedAt != nil, WireSHA256: wireSHA256(t, *e)}
}

// createBaby creates the fixtures' baby and returns its ledger entry.
func createBaby(t *testing.T, h *testkit.Harness) LedgerEntry {
	t.Helper()
	m := testkit.MustMutation(t, "baby.json")
	m.MutationID = h.CommandID()
	return ledgerEntryOf(t, h.FamilyID, applyOne(t, h, m))
}

// applyAndLedger creates the fixtures' baby (set-up, not ledgered: the
// records reference it), then applies the first n auditFixtures through
// h.Service and returns the ledger entries of their outcomes.
func applyAndLedger(t *testing.T, h *testkit.Harness, n int) []LedgerEntry {
	t.Helper()
	if n > len(auditFixtures) {
		t.Fatalf("applyAndLedger: %d writes, %d fixtures", n, len(auditFixtures))
	}
	createBaby(t, h)
	ledger := make([]LedgerEntry, 0, n)
	for _, fixture := range auditFixtures[:n] {
		m := testkit.MustMutation(t, fixture)
		m.MutationID = h.CommandID()
		ledger = append(ledger, ledgerEntryOf(t, h.FamilyID, applyOne(t, h, m)))
	}
	return ledger
}

// superuser is a pool on the harness's database as the bootstrap
// superuser, as the audit connects on the load stack (h.Admin is on the
// server's base database, not the harness's).
func superuser(t *testing.T, h *testkit.Harness) *pgxpool.Pool {
	t.Helper()
	var database string
	if err := h.Migrator.QueryRow(t.Context(), `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(testpg.RequireDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + database
	pool, err := pgxpool.New(t.Context(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// otherFamilyID creates a family for the harness's Outsider: a second
// family in the same database.
func otherFamilyID(t *testing.T, h *testkit.Harness) string {
	t.Helper()
	created, err := h.Family.Create(t.Context(), h.Outsider, h.CommandID(), "Other")
	if err != nil {
		t.Fatal(err)
	}
	return created.ID
}

// deleteFeed deletes the feed-bottle fixture's record at version 1, as
// mutations.json's delete_record does.
func deleteFeed(t *testing.T, h *testkit.Harness) protocol.Mutation {
	t.Helper()
	m := testkit.MustMutation(t, "feed-bottle.json")
	baby := m.Payload.(protocol.FeedPayload).BabyID
	m.MutationID, m.Operation, m.ObservedEntityVersion = h.CommandID(), "delete", 1
	m.Payload = protocol.DeletePayload{BabyID: &baby}
	return m
}

// renameBaby updates the fixtures' baby from version 1 to name.
func renameBaby(t *testing.T, h *testkit.Harness, name string) protocol.Mutation {
	t.Helper()
	m := testkit.MustMutation(t, "baby.json")
	m.MutationID, m.Operation, m.ObservedEntityVersion = h.CommandID(), "update", 1
	payload := m.Payload.(protocol.BabyPayload)
	payload.Name = name
	m.Payload = payload
	return m
}

func TestAuditDetectsLostCrossFamilyAndDiverged(t *testing.T) {
	h := testkit.New(t)
	db := superuser(t, h)
	ledger := applyAndLedger(t, h, 6) // 6 real writes through h.Service; entries from their outcomes
	if r, err := Audit(t.Context(), db, ledger, nil); err != nil || r.Lost+r.CrossFamily+r.Diverged != 0 || r.Acknowledged != 6 {
		t.Fatalf("clean audit: %+v %v", r, err)
	}
	if _, err := db.Exec(t.Context(), `DELETE FROM care_entities WHERE family_id=$1 AND id=$2`, ledger[0].FamilyID, ledger[0].EntityID); err != nil {
		t.Fatal(err)
	}
	moved := ledger[1]
	moved.FamilyID = otherFamilyID(t, h) // the row exists, under the true family
	changed := ledger[2]
	changed.WireSHA256 = strings.Repeat("0", 64) // same version, different wire
	r, findings, err := AuditWithFindings(t.Context(), db, []LedgerEntry{ledger[0], moved, changed, ledger[3]}, nil)
	if err != nil || r.Lost != 1 || r.CrossFamily != 1 || r.Diverged != 1 || r.Acknowledged != 4 {
		t.Fatalf("got %+v %v; want 1 lost, 1 cross-family, 1 diverged", r, err)
	}
	classes := map[string]string{}
	for _, f := range findings {
		classes[f.EntityID] = f.Class
	}
	if classes[ledger[0].EntityID] != "lost" || classes[moved.EntityID] != "cross_family" || classes[changed.EntityID] != "diverged" || len(findings) != 3 {
		t.Fatalf("findings %+v", findings)
	}
}

// An older stored version is a loss; a newer one (a write the ledger does
// not hold) is not; and an entity whose older row stays in its family while
// another family also files its id is cross-family.
func TestAuditComparesVersions(t *testing.T) {
	h := testkit.New(t)
	db := superuser(t, h)
	ledger := applyAndLedger(t, h, 3)
	ahead := ledger[1]
	ahead.Version++ // the database is behind the acknowledgement
	behind := ledger[2]
	behind.Version = 0 // the database is ahead: a later write
	behind.WireSHA256 = strings.Repeat("f", 64)
	r, err := Audit(t.Context(), db, []LedgerEntry{ledger[0], ahead, behind}, nil)
	if err != nil || r.Lost != 1 || r.CrossFamily != 0 || r.Diverged != 0 {
		t.Fatalf("got %+v %v; want 1 lost", r, err)
	}
	// The baby's row filed under another family as well.
	other := otherFamilyID(t, h)
	if _, err := db.Exec(t.Context(), `INSERT INTO care_entities SELECT $2::uuid, id, kind, baby_id, entity_version, changed_seq, creator_account_id,
		actor_account_id, device_id, operation, accepted_at, tombstoned_at, started_at, ended_at, session_id, timer_kind,
		completed_record_id, sort_order, label, name_hash, breast_seconds, bottle_volume_ml, bottle_observations, bottle_missing,
		shortest_part_seconds, wire, created_device_id, created_seq, feed_kind
		FROM care_entities WHERE family_id = $1 AND id = $3`, h.FamilyID, other, fixtureBaby); err != nil {
		t.Fatal(err)
	}
	stale := LedgerEntry{FamilyID: h.FamilyID, EntityID: fixtureBaby, Kind: "baby", Version: 2, WireSHA256: strings.Repeat("a", 64)}
	r, findings, err := AuditWithFindings(t.Context(), db, []LedgerEntry{stale}, nil)
	if err != nil || r.Lost != 0 || r.CrossFamily != 1 || len(findings) != 1 || !strings.Contains(findings[0].Detail, other) {
		t.Fatalf("got %+v %+v %v; want 1 cross-family naming %s", r, findings, err, other)
	}
}

// The audit reads the ledger in batches of auditBatch: a ledger longer than
// one batch is checked whole, and suspects from every batch are classified.
func TestAuditCoversEveryBatch(t *testing.T) {
	h := testkit.New(t)
	db := superuser(t, h)
	ledger := applyAndLedger(t, h, 2)
	// One suspect in the first batch, one in the second: both are
	// classified once the batches end.
	first, second := ledger[1], ledger[1]
	first.EntityID = "20000000-0000-4000-8000-0000000000fe"
	second.EntityID = "20000000-0000-4000-8000-0000000000ff"
	long := []LedgerEntry{first}
	for len(long) < auditBatch {
		long = append(long, ledger[0])
	}
	long = append(long, ledger[1], second)
	r, findings, err := AuditWithFindings(t.Context(), db, long, nil)
	if err != nil || r.Acknowledged != auditBatch+2 || r.Lost != 2 || len(findings) != 2 {
		t.Fatalf("got %+v %+v %v; want %d acknowledged, 2 lost (one per batch)", r, findings, err, auditBatch+2)
	}
}

func TestLedgerHashEqualsStoredWire(t *testing.T) {
	// For each applied outcome, sha256(json.Marshal(outcome.CanonicalEntity))
	// equals sha256(care_entities.wire): the round trip the audit relies on.
	h := testkit.New(t)
	db := superuser(t, h)
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	w, err := CreateLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	check := func(entries ...LedgerEntry) {
		t.Helper()
		for _, e := range entries {
			if err := w.Append(e); err != nil {
				t.Fatal(err)
			}
			var stored string
			if err := db.QueryRow(t.Context(), `SELECT encode(sha256(wire), 'hex') FROM care_entities WHERE family_id=$1 AND id=$2 AND entity_version=$3`,
				e.FamilyID, e.EntityID, int64(e.Version)).Scan(&stored); err != nil {
				t.Fatalf("%s %s v%d: %v", e.Kind, e.EntityID, e.Version, err)
			}
			if stored != e.WireSHA256 {
				t.Errorf("%s %s v%d: stored sha256 %s, ledger %s", e.Kind, e.EntityID, e.Version, stored, e.WireSHA256)
			}
		}
	}
	check(applyAndLedger(t, h, len(auditFixtures))...)
	// An update whose name json.Marshal escapes, then a delete: the
	// ledger's latest entry for the record is the tombstone, at the
	// tombstone's version.
	check(ledgerEntryOf(t, h.FamilyID, applyOne(t, h, renameBaby(t, h, "Renamed <&> é"))))
	tombstone := ledgerEntryOf(t, h.FamilyID, applyOne(t, h, deleteFeed(t, h)))
	if !tombstone.Deleted || tombstone.Version != 2 {
		t.Fatalf("delete outcome %+v", tombstone)
	}
	check(tombstone)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	ledger, err := ReadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Audit(t.Context(), db, ledger, nil)
	if err != nil || r.Acknowledged != len(auditFixtures)+1 || r.Lost+r.CrossFamily+r.Diverged != 0 {
		t.Fatalf("audit of the read-back ledger: %+v %v", r, err)
	}
}

// pullAll pulls the owner's family from cursor 0 to the end as a device
// does and returns its entity map and final cursor.
func pullAll(t *testing.T, h *testkit.Harness) (map[string]TrackedEntity, uint64) {
	t.Helper()
	held := map[string]TrackedEntity{}
	var token *string
	for page := 0; page < 100; page++ {
		response := h.Pull(t, h.Owner, h.Cursor(0), token)
		for _, c := range response.Changes {
			e := c.SnapshotOrTombstone
			held[e.Meta.ID] = TrackedEntity{Version: e.Meta.EntityVersion, WireSHA256: wireSHA256(t, e), Tombstone: e.Meta.TombstonedAt != nil}
		}
		if response.NextPageToken == nil {
			return held, response.HighWater.Sequence
		}
		token = response.NextPageToken
	}
	t.Fatal("pull did not end")
	return nil, 0
}

func TestTrackedDevicesConvergeOrAreCounted(t *testing.T) {
	// A tracked device whose map matches every row with changed_seq <= cursor
	// is converged; changing one entity's version in its map counts
	// TrackedDiverged = 1.
	h := testkit.New(t)
	db := superuser(t, h)
	ledger := applyAndLedger(t, h, 6)
	applyOne(t, h, deleteFeed(t, h))
	feed, diaper := ledger[0].EntityID, ledger[1].EntityID
	held, cursor := pullAll(t, h)
	if len(held) != 7 || !held[feed].Tombstone {
		t.Fatalf("pulled %d entities (%+v)", len(held), held)
	}
	device := func(change func(map[string]TrackedEntity)) TrackedDevice {
		entities := map[string]TrackedEntity{}
		for id, e := range held {
			entities[id] = e
		}
		if change != nil {
			change(entities)
		}
		return TrackedDevice{FamilyID: h.FamilyID, DeviceID: h.Owner.DeviceID, Cursor: cursor, Entities: entities}
	}
	converged := device(nil)
	if r, err := Audit(t.Context(), db, nil, []TrackedDevice{converged}); err != nil || r.TrackedDevices != 1 || r.TrackedDiverged != 0 {
		t.Fatalf("converged device: %+v %v", r, err)
	}
	if r, err := Audit(t.Context(), db, nil, []TrackedDevice{device(func(m map[string]TrackedEntity) { e := m[diaper]; e.Version++; m[diaper] = e })}); err != nil || r.TrackedDiverged != 1 {
		t.Fatalf("one entity's version changed: %+v %v; want TrackedDiverged 1", r, err)
	}
	// A write after the device's cursor: its older copy is not compared,
	// only counted as skipped.
	renamed := applyOne(t, h, renameBaby(t, h, "Later")).CanonicalEntity
	if r, err := Audit(t.Context(), db, nil, []TrackedDevice{converged}); err != nil || r.TrackedDiverged != 0 || r.TrackedSkipped != 1 {
		t.Fatalf("a device behind a later write: %+v %v; want 1 skipped, 0 diverged", r, err)
	}
	// The device that made that write holds its acknowledged outcome
	// (version 2) although its last pull ended before it: verified, neither
	// skipped nor diverged.
	ownWrite := device(func(m map[string]TrackedEntity) {
		m[fixtureBaby] = TrackedEntity{Version: renamed.Meta.EntityVersion, WireSHA256: wireSHA256(t, *renamed)}
	})
	if r, err := Audit(t.Context(), db, nil, []TrackedDevice{ownWrite}); err != nil || r.TrackedDiverged != 0 || r.TrackedSkipped != 0 || renamed.Meta.EntityVersion != 2 {
		t.Fatalf("a device holding its own write after its last pull: %+v %v; want 0 diverged, 0 skipped", r, err)
	}
	for name, d := range map[string]TrackedDevice{
		"older version": device(func(m map[string]TrackedEntity) { e := m[diaper]; e.Version--; m[diaper] = e }),
		"another wire": device(func(m map[string]TrackedEntity) {
			e := m[diaper]
			e.WireSHA256 = strings.Repeat("0", 64)
			m[diaper] = e
		}),
		"no tombstone":     device(func(m map[string]TrackedEntity) { e := m[feed]; e.Tombstone = false; m[feed] = e }),
		"missing row":      device(func(m map[string]TrackedEntity) { delete(m, diaper) }),
		"missed tombstone": device(func(m map[string]TrackedEntity) { delete(m, feed) }),
		// The baby's row (version 2) changed after the cursor: at that
		// version the device must hold its content; above it, it holds a
		// version the server never kept.
		"a later row's version with another wire": device(func(m map[string]TrackedEntity) { e := m[fixtureBaby]; e.Version = 2; m[fixtureBaby] = e }),
		"above a later row's version": device(func(m map[string]TrackedEntity) {
			m[fixtureBaby] = TrackedEntity{Version: 3, WireSHA256: wireSHA256(t, *renamed)}
		}),
		"unknown entity": device(func(m map[string]TrackedEntity) {
			m["40000000-0000-4000-8000-0000000000ff"] = TrackedEntity{Version: 1, WireSHA256: strings.Repeat("1", 64)}
		}),
	} {
		r, findings, err := AuditWithFindings(t.Context(), db, nil, []TrackedDevice{converged, d})
		if err != nil || r.TrackedDevices != 2 || r.TrackedDiverged != 1 || len(findings) != 1 || findings[0].Class != "tracked_diverged" {
			t.Errorf("%s: %+v %+v %v; want 1 of 2 devices diverged", name, r, findings, err)
		}
	}
	// A device of another family holding this family's baby has diverged.
	other := TrackedDevice{FamilyID: otherFamilyID(t, h), DeviceID: h.Outsider.DeviceID, Cursor: 100,
		Entities: map[string]TrackedEntity{fixtureBaby: held[fixtureBaby]}}
	if r, err := Audit(t.Context(), db, nil, []TrackedDevice{other}); err != nil || r.TrackedDiverged != 1 {
		t.Fatalf("a device holding another family's entity: %+v %v", r, err)
	}
}

func TestEmptyLedgerIsVacuous(t *testing.T) {
	h := testkit.New(t)
	r, _ := Audit(t.Context(), superuser(t, h), nil, nil)
	if v := Evaluate(RunSummary{Audit: r}, SpecThresholds()); v.Pass {
		t.Fatal("an empty ledger passed")
	}
	// Vacuous even when every other gate passes.
	s := passing()
	s.Audit = r
	v := Evaluate(s, SpecThresholds())
	if v.Pass || !strings.Contains(strings.Join(v.Reasons, "; "), "audit vacuous") {
		t.Fatalf("verdict %+v", v)
	}
}

func TestTrackedFileRoundTrips(t *testing.T) {
	const (
		family = "f0000000-0000-4000-8000-000000000001"
		entity = "e0000000-0000-4000-8000-000000000001"
	)
	path := filepath.Join(t.TempDir(), "tracked.jsonl")
	devices := []TrackedDevice{
		{FamilyID: family, DeviceID: "d0000000-0000-4000-8000-000000000001", Cursor: 7,
			Entities: map[string]TrackedEntity{entity: {Version: 2, WireSHA256: "ab", Tombstone: true}}},
		{FamilyID: family, DeviceID: "d0000000-0000-4000-8000-000000000002", Cursor: 0, Entities: map[string]TrackedEntity{}},
	}
	if err := WriteTracked(path, devices); err != nil {
		t.Fatal(err)
	}
	if err := WriteTracked(path, devices); err == nil {
		t.Fatal("WriteTracked replaced an existing file")
	}
	got, err := ReadTracked(path)
	if err != nil || len(got) != 2 || got[0].Entities[entity] != devices[0].Entities[entity] || got[1].DeviceID != devices[1].DeviceID {
		t.Fatalf("read back %+v %v", got, err)
	}
	dup := filepath.Join(t.TempDir(), "dup.jsonl")
	if err := WriteTracked(dup, []TrackedDevice{devices[0], devices[0]}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTracked(dup); err == nil || !strings.Contains(err.Error(), "listed twice") {
		t.Fatalf("a device listed twice: %v", err)
	}
	empty := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := WriteTracked(empty, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadTracked(empty); err != nil || len(got) != 0 {
		t.Fatalf("empty file: %v %v", got, err)
	}
	// A malformed id is refused with its line, before any query uses it.
	for name, bad := range map[string]TrackedDevice{
		"entity": {FamilyID: family, DeviceID: devices[1].DeviceID, Entities: map[string]TrackedEntity{"e1": {Version: 1}}},
		"device": {FamilyID: family, DeviceID: "d2", Entities: map[string]TrackedEntity{}},
		"family": {FamilyID: "f0000000-0000-4000-8000-00000000000g", DeviceID: devices[1].DeviceID},
	} {
		path := filepath.Join(t.TempDir(), name+".jsonl")
		if err := WriteTracked(path, []TrackedDevice{devices[0], bad}); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadTracked(path); err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "UUID") {
			t.Errorf("a malformed %s id: %v", name, err)
		}
	}
}
