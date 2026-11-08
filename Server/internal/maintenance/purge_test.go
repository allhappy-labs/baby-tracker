package maintenance_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/database"
	"babytracker/server/internal/maintenance"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"babytracker/server/internal/wake"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The purge's cadence and bounds, pinned by value.
func TestPurgeBoundsArePinned(t *testing.T) {
	if maintenance.PurgeInterval != 30*time.Second {
		t.Errorf("PurgeInterval = %v, want 30s", maintenance.PurgeInterval)
	}
	if maintenance.PurgeBatch != 5000 || maintenance.PurgePassCap != 20000 {
		t.Errorf("PurgeBatch = %d, PurgePassCap = %d; want 5000 rows per step statement and 20000 per pass", maintenance.PurgeBatch, maintenance.PurgePassCap)
	}
	if !strings.Contains(maintenance.PurgeTimeouts, "'lock_timeout', '200ms'") || !strings.Contains(maintenance.PurgeTimeouts, "'statement_timeout', '10s'") {
		t.Errorf("purge timeouts %q, want lock_timeout 200ms and statement_timeout 10s", maintenance.PurgeTimeouts)
	}
	start, complete, receipt := maintenance.PurgeCommandIDs("8a000000-0000-4000-8000-000000000001")
	again, _, _ := maintenance.PurgeCommandIDs("8a000000-0000-4000-8000-000000000001")
	other, _, _ := maintenance.PurgeCommandIDs("8a000000-0000-4000-8000-000000000002")
	if start != again || start == other || start == complete || complete == receipt || start == receipt {
		t.Errorf("purge command IDs %s %s %s (again %s, other family %s): want three distinct IDs, the same for the same family", start, complete, receipt, again, other)
	}
	for _, id := range []string{start, complete, receipt} {
		if len(id) != 36 || id[14] != '4' || !strings.ContainsRune("89ab", rune(id[19])) {
			t.Errorf("purge command ID %q is not a version 4 UUID", id)
		}
	}
}

// purgeTables are every family-scoped table the purge empties, as the
// maintenance login reads them: 000031 admits it to these tables whole, so a
// zero is not a tenant-visibility artefact.
var purgeTables = []string{"care_entities", "care_receipts", "sync_page_bindings", "sync_snapshot_sessions", "import_sessions", "device_sync_cursors"}

// importTables are the import staging the purge empties through
// import_sessions' cascades, read as the table owner in the family's
// context.
var importTables = []string{"import_chunks", "import_baby_candidates", "import_record_manifests", "import_baby_mappings"}

// maintenanceCount counts table's rows of familyID as the maintenance login.
func maintenanceCount(t *testing.T, h *testkit.Harness, table, familyID string) int {
	t.Helper()
	var n int
	if err := h.Maintenance.QueryRow(t.Context(), `SELECT count(*) FROM `+table+` WHERE family_id=$1`, familyID).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// seedPurgeable gives the harness family a row in every table the purge
// empties: a baby with 30 mixed records and a running timer (each with its
// receipt), a completed import whose mapping references a baby row, a
// continuation binding (page size 1), a snapshot session and a device cursor
// the wake worker flushed. Then the owner schedules the family's deletion.
// It returns the family's next_sequence.
func seedPurgeable(t *testing.T, h *testkit.Harness) uint64 {
	t.Helper()
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t, h.Limits)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	caretest.Seed(t, h, caretest.MixedBatch(caretest.BabyA, 30)...)
	caretest.Seed(t, h, caretest.StartTimer(caretest.BabyA, caretest.TimerA, "feed"))
	h.Import(t, h.Owner, "8a000000-0000-4000-8000-000000000002", "Mira", "2026-01-01") // a mapping to a baby row
	h.Import(t, h.Owner, "8a000000-0000-4000-8000-000000000003", "Noah", "2026-02-01") // a second import session
	_ = h.Pull(t, h.Caregiver, h.Cursor(0), nil)                                       // a continuation binding
	if _, err := h.Service.FullSnapshot(t.Context(), h.Caregiver, protocol.SnapshotRequest{}); err != nil {
		t.Fatal(err) // a snapshot session
	}
	tracker := wake.NewTracker(0)
	tracker.Synced(h.FamilyID, h.Caregiver.DeviceID, 1, 2, h.Now)
	worker := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: quietSender{}, Now: func() time.Time { return h.Now }}
	if _, err := worker.Tick(t.Context()); err != nil {
		t.Fatal(err) // a device cursor
	}
	for _, table := range purgeTables {
		if maintenanceCount(t, h, table, h.FamilyID) == 0 {
			t.Fatalf("seed left %s empty", table)
		}
	}
	// A baby-only import stages no record manifest; the rest it does.
	for _, table := range []string{"import_chunks", "import_baby_candidates", "import_baby_mappings"} {
		if h.Count(t, table, h.FamilyID) == 0 {
			t.Fatalf("seed left %s empty", table)
		}
	}
	if h.CareCount(t, "baby", h.FamilyID) < 1 || h.CareCount(t, "feed", h.FamilyID) < 1 {
		t.Fatal("seed has no baby or no record")
	}
	caretest.AssertCounters(t, h, h.FamilyID)
	sequence := h.FamilySequence(t)
	if _, err := h.Family.ScheduleDeletion(t.Context(), h.Owner, h.CommandID(), true); err != nil {
		t.Fatal(err)
	}
	for _, table := range memberTables {
		if ownerCount(t, h, table, h.FamilyID) == 0 {
			t.Fatalf("seed left %s empty", table)
		}
	}
	return sequence
}

// familyStatus is a family's status, read by the maintenance login (column
// SELECT on families.status, 000031).
func familyStatus(t *testing.T, h *testkit.Harness, familyID string) string {
	t.Helper()
	var status string
	if err := h.Maintenance.QueryRow(t.Context(), `SELECT status::text FROM families WHERE id=$1`, familyID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// drivePurge runs purger until familyID's purge completes, at most limit
// ticks, and returns the ticks it took.
func drivePurge(t *testing.T, h *testkit.Harness, purger *maintenance.Purger, familyID string, now time.Time, limit int) int {
	t.Helper()
	tick := 0
	for ; familyStatus(t, h, familyID) != "purge_completed"; tick++ {
		if tick > limit {
			t.Fatalf("purge did not finish in %d ticks", limit)
		}
		if _, err := purger.Run(t.Context(), now); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
	}
	return tick
}

// assertPurged fails t unless familyID holds no care row, receipt, binding,
// snapshot session, cursor or import staging row, its counters are zero and
// its audit chain holds exactly one purge start and one purge completion by
// the retention worker, under the purge's own command IDs.
func assertPurged(t *testing.T, h *testkit.Harness, familyID string) {
	t.Helper()
	for _, table := range purgeTables {
		if n := maintenanceCount(t, h, table, familyID); n != 0 {
			t.Fatalf("%s holds %d rows of the purged family", table, n)
		}
	}
	for _, table := range append([]string{"import_conflicts", "import_conflict_receipts"}, importTables...) {
		if n := h.Count(t, table, familyID); n != 0 {
			t.Fatalf("%s holds %d rows of the purged family", table, n)
		}
	}
	caretest.AssertCounters(t, h, familyID) // all four counters zero
	var record, charged, entities, wire int64
	if err := h.Maintenance.QueryRow(t.Context(), `SELECT care_record_count, care_charged_bytes, care_entity_count, care_wire_bytes FROM families WHERE id=$1`,
		familyID).Scan(&record, &charged, &entities, &wire); err != nil || record+charged+entities+wire != 0 {
		t.Fatalf("purged family counters %d %d %d %d (%v)", record, charged, entities, wire, err)
	}
	actor, err := authority.RetentionWorkerActorID(testkit.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	start, complete, receipt := maintenance.PurgeCommandIDs(familyID)
	for kind, command := range map[string]string{"family_purge_started": start, "family_purge_completed": complete} {
		if n := authorityCount(t, h, `SELECT count(*) FROM security_audit_events WHERE scope='family' AND scope_id=$1 AND kind::text=$2`, familyID, kind); n != 1 {
			t.Fatalf("%d %s audit rows, want one", n, kind)
		}
		if n := authorityCount(t, h, `SELECT count(*) FROM security_audit_events WHERE scope='family' AND scope_id=$1 AND kind::text=$2
			AND actor_account_id=$3 AND command_id=$4`, familyID, kind, actor, command); n != 1 {
			t.Fatalf("%s audit row not by the retention worker under %s", kind, command)
		}
	}
	if n := authorityCount(t, h, `SELECT count(*) FROM families WHERE id=$1 AND status='purge_completed' AND purge_command_id=$2 AND purge_receipt=$3`,
		familyID, start, receipt); n != 1 {
		t.Fatal("the purged family does not record the purge's start command and receipt")
	}
	if n := authorityCount(t, h, `SELECT count(*) FROM families WHERE id=$1 AND display_name=''`, familyID); n != 1 {
		t.Fatal("the purged family keeps its display name")
	}
	// No member name outlives the purge (owner decision 2026-10-08): the
	// display labels, their history, the memberships and the recovery claim
	// are gone.
	for _, table := range memberTables {
		if n := ownerCount(t, h, table, familyID); n != 0 {
			t.Fatalf("%s holds %d rows of the purged family", table, n)
		}
	}
	// The audit chains, which the purge keeps, still verify: every family
	// and account chain, the purged family's included.
	// (A chain a test failed on purpose stays failed.)
	failedBefore := authorityCount(t, h, `SELECT (SELECT count(*) FROM families WHERE chain_failed) + (SELECT count(*) FROM accounts WHERE chain_failed)`)
	report, err := authority.NewVerifier(h.Authority, nil).VerifyAll(t.Context())
	if err != nil || len(report.Failed) != failedBefore || report.Scopes == 0 {
		t.Fatalf("audit verification after the purge: %+v (%v), %d chains failed before", report, err, failedBefore)
	}
	for _, failed := range report.Failed {
		if failed.ID == familyID {
			t.Fatalf("the purged family's audit chain fails verification")
		}
	}
	if n := authorityCount(t, h, `SELECT count(*) FROM security_audit_events WHERE scope='family' AND scope_id=$1`, familyID); n < 3 {
		t.Fatalf("%d family audit rows after the purge: the chain is kept", n)
	}
}

// memberTables hold the family's member names and memberships: the purge
// deletes them after the care data.
var memberTables = []string{"family_actor_label_history", "family_actor_labels", "memberships", "family_recovery_claims"}

// ownerCount counts table's rows of familyID as the table owner under the
// authority flag and the family's tenant context (RLS admits them under one
// or the other).
func ownerCount(t *testing.T, h *testkit.Harness, table, familyID string) int {
	t.Helper()
	tx, err := h.Migrator.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `SELECT set_config('app.authority','1',true), set_config('app.family_id',$1,true)`, familyID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM `+table+` WHERE family_id=$1`, familyID).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestPurgeDeletesTheFamilysCareDataAndCompletes(t *testing.T) {
	h := testkit.New(t)
	// Another family, the outsider's, keeps everything.
	other, err := h.Family.Create(t.Context(), h.Outsider, h.CommandID(), "Other")
	if err != nil {
		t.Fatal(err)
	}
	outsider := h.Refresh("outsider")
	otherBatch := []protocol.Mutation{caretest.Baby(caretest.BabyB), caretest.Feed(caretest.BabyB, 1), caretest.Feed(caretest.BabyB, 2)}
	request := h.SyncRequest(nil, nil)
	request.Mutations = otherBatch
	if _, err := h.Service.ApplyAndPull(t.Context(), outsider, request); err != nil {
		t.Fatal(err)
	}
	// A cursor of the other family under a device the purged family also
	// has a cursor for (the caregiver's, seedPurgeable).
	tracker := wake.NewTracker(0)
	tracker.Synced(other.ID, h.Caregiver.DeviceID, 1, 1, h.Now)
	if _, err := (&wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: quietSender{}, Now: func() time.Time { return h.Now }}).Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	otherRows := map[string]int{}
	for _, table := range []string{"care_entities", "care_receipts", "device_sync_cursors"} {
		otherRows[table] = maintenanceCount(t, h, table, other.ID)
	}
	invitations := authorityCount(t, h, `SELECT count(*) FROM invitations WHERE family_id=$1`, h.FamilyID)
	sequence := seedPurgeable(t, h)
	// The other family is deleted an hour later, so it is not due when the
	// first one is: it keeps its member labels, memberships and its own
	// (active) recovery claim.
	h.Now = h.Now.Add(time.Hour)
	if _, err := h.Family.ScheduleDeletion(t.Context(), outsider, h.CommandID(), true); err != nil {
		t.Fatal(err)
	}
	h.Now = h.Now.Add(-time.Hour)
	for _, table := range memberTables {
		otherRows[table] = ownerCount(t, h, table, other.ID)
	}

	// Not due a microsecond before purge_after: nothing happens.
	purger := &maintenance.Purger{Authority: h.Authority, Pool: h.Maintenance, Batch: 7}
	due := h.Now.Add(30 * 24 * time.Hour)
	if _, err := purger.Run(t.Context(), due.Add(-time.Microsecond)); err != nil || familyStatus(t, h, h.FamilyID) != "deletion_pending" {
		t.Fatalf("a purge before purge_after: %v, status %s", err, familyStatus(t, h, h.FamilyID))
	}
	h.Now = due
	if ticks := drivePurge(t, h, purger, h.FamilyID, h.Now, 200); ticks < 3 {
		t.Fatalf("the purge took %d ticks, want a start, deletes and a completion", ticks)
	}
	assertPurged(t, h, h.FamilyID)
	if got := h.FamilySequence(t); got != sequence {
		t.Fatalf("next_sequence moved %d -> %d", sequence, got)
	}
	// Invitations are kept (a lost-acknowledgement Redeem retry still finds
	// its receipt).
	if n := authorityCount(t, h, `SELECT count(*) FROM invitations WHERE family_id=$1`, h.FamilyID); n != invitations || n == 0 {
		t.Fatalf("%d invitations after the purge, want the %d before it", n, invitations)
	}
	// The other family is untouched.
	for table, n := range otherRows {
		got := 0
		if slices.Contains(memberTables, table) {
			got = ownerCount(t, h, table, other.ID)
		} else {
			got = maintenanceCount(t, h, table, other.ID)
		}
		if got != n || n == 0 {
			t.Fatalf("the other family's %s went %d -> %d", table, n, got)
		}
	}
	if status := familyStatus(t, h, other.ID); status != "deletion_pending" {
		t.Fatalf("the other family is %s, want deletion_pending (not yet due)", status)
	}
	caretest.AssertCounters(t, h, other.ID)
	// A completed purge is no more work: the next tick does nothing.
	if n, err := purger.Run(t.Context(), h.Now); err != nil || n != 0 {
		t.Fatalf("a tick after completion: %d, %v", n, err)
	}
}

// A purge is crash-safe at every step. For each step a Purger fails there
// (before the step's transaction commits; for the authority commands, after
// they committed, so their acknowledgement is lost); FamilyPurgeWork then
// lists the started family with its persisted purge_command_id, and a fresh
// Purger over the same authority finishes the purge. No step runs twice to a
// different effect: the audit holds one start and one completion.
func TestPurgeResumesAfterACrashAtEveryStep(t *testing.T) {
	errCrash := errors.New("crash")
	for _, step := range maintenance.PurgeSteps {
		t.Run(step, func(t *testing.T) {
			h := testkit.New(t)
			seedPurgeable(t, h)
			h.Now = h.Now.Add(30 * 24 * time.Hour)
			start, complete, receipt := maintenance.PurgeCommandIDs(h.FamilyID)
			crashing := &maintenance.Purger{Authority: h.Authority, Pool: h.Maintenance, Batch: 50}
			crashed, before := false, -1
			crashing.FailAt(func(at string, _ int) error {
				if at != step {
					return nil
				}
				crashed = true
				switch at {
				case "care_receipts", "sync_page_bindings", "sync_snapshot_sessions", "device_sync_cursors", "import_sessions":
					before = maintenanceCount(t, h, at, h.FamilyID)
				case "care_records", "babies":
					before = maintenanceCount(t, h, "care_entities", h.FamilyID)
				case "label_history", "labels", "memberships", "recovery_claim":
					before = ownerCount(t, h, memberTable[at], h.FamilyID)
				}
				return errCrash
			})
			for tick := 0; !crashed; tick++ {
				if tick > 200 {
					t.Fatalf("the purge never reached %s", step)
				}
				if _, err := crashing.Run(t.Context(), h.Now); crashed != errors.Is(err, errCrash) || (!crashed && err != nil) {
					t.Fatalf("tick %d: %v (crashed %v)", tick, err, crashed)
				}
			}
			// The crashed step's transaction rolled back: its rows are still there.
			switch step {
			case "care_receipts", "sync_page_bindings", "sync_snapshot_sessions", "device_sync_cursors", "import_sessions":
				if n := maintenanceCount(t, h, step, h.FamilyID); n != before || n == 0 {
					t.Fatalf("after a crash in %s: %d rows, %d before its commit", step, n, before)
				}
			case "care_records", "babies":
				if n := maintenanceCount(t, h, "care_entities", h.FamilyID); n != before || n == 0 {
					t.Fatalf("after a crash in %s: %d care rows, %d before its commit", step, n, before)
				}
			case "label_history", "labels", "memberships", "recovery_claim":
				if n := ownerCount(t, h, memberTable[step], h.FamilyID); n != before || n == 0 {
					t.Fatalf("after a crash in %s: %d rows, %d before its commit", step, n, before)
				}
			case "zero":
				var charged int64
				if err := h.Maintenance.QueryRow(t.Context(), `SELECT care_charged_bytes FROM families WHERE id=$1`, h.FamilyID).Scan(&charged); err != nil || charged == 0 {
					t.Fatalf("after a crash before the zeroing commit the counters are %d (%v)", charged, err)
				}
			}
			work, err := h.Authority.FamilyPurgeWork(t.Context(), h.Now, 10)
			if err != nil {
				t.Fatal(err)
			}
			if step == "completed" {
				if len(work) != 0 || familyStatus(t, h, h.FamilyID) != "purge_completed" {
					t.Fatalf("after the completion committed: work %+v", work)
				}
				// The lost acknowledgement's exact retry returns the stored result.
				if _, err := h.Authority.CompleteFamilyPurge(t.Context(), authority.CompleteFamilyPurge{CommandID: complete, FamilyID: h.FamilyID,
					StartCommandID: start, Receipt: receipt, At: h.Now}); err != nil {
					t.Fatalf("complete retry: %v", err)
				}
			} else if len(work) != 1 || work[0] != (authority.PurgeWork{FamilyID: h.FamilyID, StartCommandID: start}) {
				t.Fatalf("after a crash at %s: work %+v, want the started family with its purge_command_id %s", step, work, start)
			}
			if step == "start" {
				// The lost acknowledgement's exact retry returns the stored result.
				if _, err := h.Authority.StartFamilyPurge(t.Context(), authority.StartFamilyPurge{CommandID: start, FamilyID: h.FamilyID, At: h.Now}); err != nil {
					t.Fatalf("start retry: %v", err)
				}
			}
			fresh := &maintenance.Purger{Authority: h.Authority, Pool: h.Maintenance, Batch: 50}
			drivePurge(t, h, fresh, h.FamilyID, h.Now.Add(time.Minute), 200)
			assertPurged(t, h, h.FamilyID)
		})
	}
}

// A purge that some other start began (another node's command, an older
// worker's ID) completes under the purge_command_id the family row
// persists, not one the worker derives (Phase 1 carry).
func TestPurgeCompletesTheStartInFlight(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA), caretest.Feed(caretest.BabyA, 1))
	if _, err := h.Family.ScheduleDeletion(t.Context(), h.Owner, h.CommandID(), true); err != nil {
		t.Fatal(err)
	}
	h.Now = h.Now.Add(30 * 24 * time.Hour)
	inFlight := h.CommandID()
	if _, err := h.Authority.StartFamilyPurge(t.Context(), authority.StartFamilyPurge{CommandID: inFlight, FamilyID: h.FamilyID, At: h.Now}); err != nil {
		t.Fatal(err)
	}
	if work, err := h.Authority.FamilyPurgeWork(t.Context(), h.Now, 10); err != nil || len(work) != 1 || work[0].StartCommandID != inFlight {
		t.Fatalf("work %+v (%v), want the purge in flight under %s", work, err, inFlight)
	}
	drivePurge(t, h, &maintenance.Purger{Authority: h.Authority, Pool: h.Maintenance}, h.FamilyID, h.Now, 20)
	if n := authorityCount(t, h, `SELECT count(*) FROM families WHERE id=$1 AND status='purge_completed' AND purge_command_id=$2`, h.FamilyID, inFlight); n != 1 {
		t.Fatal("the purge did not complete the start in flight")
	}
	if n := authorityCount(t, h, `SELECT count(*) FROM security_audit_events WHERE scope='family' AND scope_id=$1 AND kind::text='family_purge_started'`, h.FamilyID); n != 1 {
		t.Fatalf("%d purge starts, want the one in flight", n)
	}
	caretest.AssertCounters(t, h, h.FamilyID)
}

// One family whose purge fails for good never stops the others (review I1).
// Two due families; the lower ID's audit chain has failed (chain_failed, as
// the verifier leaves a broken chain), so every authority command on it is
// refused with scope_failed: at its start, or, once its purge started, at
// its completion. The purge quarantines it (one Error log, family ID and
// cause class) and purges the other in the same ticks; the failed family
// keeps its state.
func TestPurgeQuarantinesAFamilyThatKeepsFailing(t *testing.T) {
	for _, at := range []string{"start", "complete"} {
		t.Run(at, func(t *testing.T) {
			h := testkit.New(t)
			caretest.Seed(t, h, caretest.Baby(caretest.BabyA), caretest.Feed(caretest.BabyA, 1))
			other := deletionPendingFamily(t, h, 0) // the outsider's family
			if _, err := h.Family.ScheduleDeletion(t.Context(), h.Owner, h.CommandID(), true); err != nil {
				t.Fatal(err)
			}
			failing, healthy := h.FamilyID, other
			if other < failing {
				failing, healthy = other, h.FamilyID
			}
			h.Now = h.Now.Add(30 * 24 * time.Hour)
			want := "deletion_pending"
			if at == "complete" {
				start, _, _ := maintenance.PurgeCommandIDs(failing)
				if _, err := h.Authority.StartFamilyPurge(t.Context(), authority.StartFamilyPurge{CommandID: start, FamilyID: failing, At: h.Now}); err != nil {
					t.Fatal(err)
				}
				want = "purge_started"
			}
			h.AuthorityExec(t, `UPDATE families SET chain_failed=true WHERE id=$1`, failing)
			logs := &records{}
			purger := &maintenance.Purger{Authority: h.Authority, Pool: h.Maintenance, Logger: slog.New(logs)}
			for tick := 0; familyStatus(t, h, healthy) != "purge_completed"; tick++ {
				if tick > 20 {
					t.Fatalf("the healthy family's purge did not finish beside the failing one (status %s)", familyStatus(t, h, healthy))
				}
				if _, err := purger.Run(t.Context(), h.Now); err != nil {
					t.Fatalf("tick %d: %v", tick, err)
				}
			}
			for range 3 { // later ticks leave the quarantined family alone, silently
				if n, err := purger.Run(t.Context(), h.Now); err != nil || n != 0 {
					t.Fatalf("a tick after the healthy purge: %d, %v", n, err)
				}
			}
			assertPurged(t, h, healthy)
			if got := familyStatus(t, h, failing); got != want {
				t.Fatalf("the failing family is %s, want %s", got, want)
			}
			errorsLogged := logs.at(slog.LevelError)
			if len(errorsLogged) != 1 || attr(errorsLogged[0], "family_id") != failing || attr(errorsLogged[0], "cause") != "scope_failed" {
				t.Fatalf("error logs %+v, want one naming %s with cause scope_failed", errorsLogged, failing)
			}
		})
	}
}

// A step that times out tick after tick at the minimum batch (a statement
// that never fits statement_timeout however small) is retried
// maxPurgeTimeouts (3) times, then the family is quarantined.
func TestPurgeQuarantinesAFamilyWhoseStepKeepsTimingOut(t *testing.T) {
	h := testkit.New(t)
	seedPurgeable(t, h)
	h.Now = h.Now.Add(30 * 24 * time.Hour)
	logs := &records{}
	purger := &maintenance.Purger{Authority: h.Authority, Pool: h.Maintenance, Logger: slog.New(logs), Batch: maintenance.MinBatch}
	purger.FailAt(func(step string, _ int) error {
		if step == "care_receipts" {
			return &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}
		}
		return nil
	})
	if _, err := purger.Run(t.Context(), h.Now); err != nil { // the start
		t.Fatal(err)
	}
	receipts := maintenanceCount(t, h, "care_receipts", h.FamilyID)
	for tick := range 2 {
		if _, err := purger.Run(t.Context(), h.Now); err == nil || len(logs.at(slog.LevelError)) != 0 {
			t.Fatalf("timeout %d: %v with %d error logs, want the timeout returned and no quarantine yet", tick+1, err, len(logs.at(slog.LevelError)))
		}
	}
	if n, err := purger.Run(t.Context(), h.Now); err != nil || n != 0 {
		t.Fatalf("the third timeout: %d, %v; want the family quarantined", n, err)
	}
	if n, err := purger.Run(t.Context(), h.Now); err != nil || n != 0 {
		t.Fatalf("a tick after the quarantine: %d, %v", n, err)
	}
	if errorsLogged := logs.at(slog.LevelError); len(errorsLogged) != 1 || attr(errorsLogged[0], "family_id") != h.FamilyID ||
		attr(errorsLogged[0], "cause") != "statement_timeout" {
		t.Fatalf("error logs %+v, want one naming the family with cause statement_timeout", errorsLogged)
	}
	if n := maintenanceCount(t, h, "care_receipts", h.FamilyID); n != receipts || familyStatus(t, h, h.FamilyID) != "purge_started" {
		t.Fatalf("the quarantined family: %d receipts (%d before), status %s", n, receipts, familyStatus(t, h, h.FamilyID))
	}
	// A fresh process (a new Purger) meets it again: the quarantine lives in
	// memory only.
	drivePurge(t, h, &maintenance.Purger{Authority: h.Authority, Pool: h.Maintenance}, h.FamilyID, h.Now, 50)
	assertPurged(t, h, h.FamilyID)
}

// A statement timeout above the minimum batch only halves the batch: a
// family whose cursors time out at 3,000, 2,500 and 1,250 rows a statement
// (and fit at 625) is purged at the smaller batch and never quarantined
// (re-review m1).
func TestPurgeShrinksTheBatchBeforeItQuarantinesATimeout(t *testing.T) {
	h := testkit.New(t)
	family := deletionPendingFamily(t, h, 3000)
	h.Now = h.Now.Add(30 * 24 * time.Hour)
	logs := &records{}
	purger := &maintenance.Purger{Authority: h.Authority, Pool: h.Maintenance, Logger: slog.New(logs)}
	var sizes []int
	purger.FailAt(func(step string, n int) error {
		if step != "device_sync_cursors" {
			return nil
		}
		sizes = append(sizes, n)
		if n >= 1250 {
			return &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}
		}
		return nil
	})
	timedOut := 0
	for tick := 0; familyStatus(t, h, family) != "purge_completed"; tick++ {
		if tick > 30 {
			t.Fatalf("the purge never finished (cursor statements %v)", sizes)
		}
		if _, err := purger.Run(t.Context(), h.Now); err != nil {
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
				t.Fatalf("tick %d: %v", tick, err)
			}
			timedOut++
		}
	}
	if len(sizes) < 5 || !slices.Equal(sizes[:4], []int{3000, 2500, 1250, 625}) || timedOut != 3 {
		t.Fatalf("cursor statements %v with %d timed-out ticks, want 3000, 2500 and 1250 timing out, then 625 at a time", sizes, timedOut)
	}
	if errorsLogged := logs.at(slog.LevelError); len(errorsLogged) != 0 {
		t.Fatalf("error logs %+v, want no quarantine", errorsLogged)
	}
	assertPurged(t, h, family)
}

// Which failures quarantine a family (permanent) and which are retried
// (transient), pinned both ways (re-review m2).
func TestPurgeClassifiesFailures(t *testing.T) {
	pg := func(code string) error { return &pgconn.PgError{Code: code} }
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, c := range []struct {
		name      string
		ctx       context.Context
		err       error
		cause     string
		transient bool
	}{
		{"deadlock", t.Context(), pg("40P01"), "sqlstate_40P01", true},
		{"serialization failure", t.Context(), pg("40001"), "sqlstate_40001", true},
		{"lock timeout", t.Context(), pg("55P03"), "sqlstate_55P03", true},
		{"administrator shutdown", t.Context(), pg("57P01"), "sqlstate_57P01", true},
		{"connection failure", t.Context(), pg("08006"), "sqlstate_08006", true},
		{"too many connections", t.Context(), pg("53300"), "sqlstate_53300", true},
		{"statement timeout (counted at the minimum batch)", t.Context(), pg("57014"), "statement_timeout", true},
		{"wrapped deadlock", t.Context(), fmt.Errorf("maintenance: purge: care step: %w", pg("40P01")), "sqlstate_40P01", true},
		{"deadlock joined with the authority's unavailable", t.Context(), errors.Join(pg("40P01"), authority.ErrUnavailable), "sqlstate_40P01", true},
		{"EOF", t.Context(), fmt.Errorf("read: %w", io.EOF), "unavailable", true},
		{"net error", t.Context(), &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}, "unavailable", true},
		{"conn closed", t.Context(), errors.New("conn closed"), "unavailable", true},
		{"authority unavailable", t.Context(), fmt.Errorf("authority: begin: %w", authority.ErrUnavailable), "unavailable", true},
		{"cancelled run", cancelled, pg("23514"), "cancelled", true},
		{"context deadline", t.Context(), fmt.Errorf("slot: %w", context.DeadlineExceeded), "cancelled", true},
		{"failed audit chain", t.Context(), fmt.Errorf("maintenance: purge: start: %w", authority.ErrScopeFailed), "scope_failed", false},
		{"invalid command", t.Context(), authority.ErrInvalidCommand, "invalid_command", false},
		{"family unavailable", t.Context(), authority.ErrFamilyUnavailable, "family_unavailable", false},
		{"privilege error", t.Context(), pg("42501"), "sqlstate_42501", false},
		{"check violation", t.Context(), pg("23514"), "sqlstate_23514", false},
		{"foreign key violation", t.Context(), pg("23503"), "sqlstate_23503", false},
		{"read-only transaction", t.Context(), pg("25006"), "sqlstate_25006", false},
		{"internal error", t.Context(), pg("XX000"), "sqlstate_XX000", false},
		{"privilege error joined with the authority's unavailable", t.Context(), errors.Join(pg("42501"), authority.ErrUnavailable), "sqlstate_42501", false},
	} {
		if cause, transient := maintenance.PurgeCause(c.ctx, c.err); cause != c.cause || transient != c.transient {
			t.Errorf("%s: %q transient=%v, want %q transient=%v", c.name, cause, transient, c.cause, c.transient)
		}
	}
}

// The quarantine expires: a quarantined family is skipped for an hour and
// tried (and, failing again, quarantined and logged) again after it; and it
// is capped, the oldest entry evicted (and logged) to make room (re-review
// m4).
func TestPurgeQuarantineExpiresAndIsCapped(t *testing.T) {
	t.Run("cap and expiry", func(t *testing.T) {
		logs := &records{}
		purger := &maintenance.Purger{Logger: slog.New(logs)}
		purger.SetQuarantineCap(2)
		at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
		const a, b, c = "a0000000-0000-4000-8000-000000000001", "b0000000-0000-4000-8000-000000000001", "c0000000-0000-4000-8000-000000000001"
		purger.Quarantine(at, a, "scope_failed")
		purger.Quarantine(at.Add(time.Minute), b, "scope_failed")
		if got := sortedIDs(purger.Quarantined(at.Add(2 * time.Minute))); !slices.Equal(got, []string{a, b}) {
			t.Fatalf("quarantine %v, want a and b", got)
		}
		purger.Quarantine(at.Add(3*time.Minute), c, "sqlstate_42501")
		if got := sortedIDs(purger.Quarantined(at.Add(4 * time.Minute))); !slices.Equal(got, []string{b, c}) {
			t.Fatalf("quarantine at its cap %v, want the oldest (a) evicted", got)
		}
		var warned []slog.Record
		for _, record := range logs.at(slog.LevelWarn) {
			if record.Level == slog.LevelWarn {
				warned = append(warned, record)
			}
		}
		if len(warned) != 1 || attr(warned[0], "family_id") != a || len(logs.at(slog.LevelError)) != 3 {
			t.Fatalf("eviction logs %+v, want one naming a", warned)
		}
		if got := sortedIDs(purger.Quarantined(at.Add(time.Minute + maintenance.PurgeQuarantineTTL - time.Microsecond))); !slices.Equal(got, []string{b, c}) {
			t.Fatalf("a microsecond short of b's hour: %v", got)
		}
		if got := sortedIDs(purger.Quarantined(at.Add(time.Minute + maintenance.PurgeQuarantineTTL))); !slices.Equal(got, []string{c}) {
			t.Fatalf("an hour after b's quarantine: %v, want only c", got)
		}
		if got := purger.Quarantined(at.Add(3*time.Minute + maintenance.PurgeQuarantineTTL)); len(got) != 0 {
			t.Fatalf("an hour after c's quarantine: %v, want none", got)
		}
		if maintenance.PurgeQuarantineTTL != time.Hour || maintenance.PurgeQuarantineCap != 1000 {
			t.Fatalf("quarantine TTL %v and cap %d, want an hour and 1000 (FamilyPurgeWork's exclusion limit)", maintenance.PurgeQuarantineTTL, maintenance.PurgeQuarantineCap)
		}
	})
	t.Run("a failing family is tried again after an hour", func(t *testing.T) {
		h := testkit.New(t)
		family := deletionPendingFamily(t, h, 0)
		h.Now = h.Now.Add(30 * 24 * time.Hour)
		h.AuthorityExec(t, `UPDATE families SET chain_failed=true WHERE id=$1`, family)
		logs := &records{}
		purger := &maintenance.Purger{Authority: h.Authority, Pool: h.Maintenance, Logger: slog.New(logs)}
		for _, at := range []time.Duration{0, time.Minute, maintenance.PurgeQuarantineTTL - time.Microsecond} {
			if _, err := purger.Run(t.Context(), h.Now.Add(at)); err != nil {
				t.Fatal(err)
			}
			if n := len(logs.at(slog.LevelError)); n != 1 {
				t.Fatalf("at +%v: %d error logs, want the one quarantine", at, n)
			}
		}
		if _, err := purger.Run(t.Context(), h.Now.Add(maintenance.PurgeQuarantineTTL)); err != nil {
			t.Fatal(err)
		}
		if errorsLogged := logs.at(slog.LevelError); len(errorsLogged) != 2 || attr(errorsLogged[1], "family_id") != family {
			t.Fatalf("after an hour: %d error logs, want the family tried and quarantined again", len(errorsLogged))
		}
		if got := familyStatus(t, h, family); got != "deletion_pending" {
			t.Fatalf("the failing family is %s", got)
		}
	})
}

func sortedIDs(ids []string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}

// A transient failure (a lock timeout here) never quarantines: the same
// Purger retries the family on the next tick, however often it meets one.
func TestPurgeRetriesATransientFailure(t *testing.T) {
	h := testkit.New(t)
	seedPurgeable(t, h)
	h.Now = h.Now.Add(30 * 24 * time.Hour)
	logs := &records{}
	purger := &maintenance.Purger{Authority: h.Authority, Pool: h.Maintenance, Logger: slog.New(logs)}
	timeouts := 0
	purger.FailAt(func(step string, _ int) error {
		if step == "care_receipts" && timeouts < 5 {
			timeouts++
			return &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}
		}
		return nil
	})
	failed := 0
	for tick := 0; familyStatus(t, h, h.FamilyID) != "purge_completed"; tick++ {
		if tick > 50 {
			t.Fatal("the purge never finished after its lock timeouts")
		}
		if _, err := purger.Run(t.Context(), h.Now); err != nil {
			failed++
		}
	}
	if failed != 5 || len(logs.at(slog.LevelError)) != 0 {
		t.Fatalf("%d failed ticks and %d error logs, want the 5 lock timeouts returned and no quarantine", failed, len(logs.at(slog.LevelError)))
	}
	assertPurged(t, h, h.FamilyID)
}

// A completed purge clears the family's own display name (owner decision
// 2026-10-08): afterwards the family row's name is the empty string, no row
// of any table in the database (read as the superuser, below RLS) holds the
// old name, the completion's audit row records only that the name was
// cleared, an exact retry of the completion changes nothing, and every audit
// chain still verifies.
func TestPurgeClearsTheFamilyName(t *testing.T) {
	h := testkit.New(t)
	const name = "Quillfeather Household 7731"
	family, err := h.Family.Create(t.Context(), h.Outsider, h.CommandID(), name)
	if err != nil {
		t.Fatal(err)
	}
	outsider := h.Refresh("outsider")
	request := h.SyncRequest(nil, nil)
	request.Mutations = []protocol.Mutation{caretest.Baby(caretest.BabyB), caretest.Feed(caretest.BabyB, 1)}
	if _, err := h.Service.ApplyAndPull(t.Context(), outsider, request); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Family.ScheduleDeletion(t.Context(), outsider, h.CommandID(), true); err != nil {
		t.Fatal(err)
	}
	// The superuser on the test database: every table, every row, no RLS.
	config := h.Migrator.Config().ConnConfig.Copy()
	admin := h.Admin.Config().ConnConfig
	config.User, config.Password = admin.User, admin.Password
	superuser, err := pgx.ConnectConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer superuser.Close(context.Background())
	holding := func() []string {
		rows, err := superuser.Query(t.Context(), `SELECT format('%I.%I', n.nspname, c.relname) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE c.relkind = 'r' AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast') ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		var found []string
		for _, table := range tables {
			var n int
			if err := superuser.QueryRow(t.Context(), `SELECT count(*) FROM `+table+` x WHERE x::text ILIKE '%Quillfeather%'`).Scan(&n); err != nil {
				t.Fatalf("%s: %v", table, err)
			}
			if n > 0 {
				found = append(found, fmt.Sprintf("%s:%d", table, n))
			}
		}
		return found
	}
	if before := holding(); !slices.Equal(before, []string{"public.families:1"}) {
		t.Fatalf("before the purge the name is held by %v, want the family row only", before)
	}
	h.Now = h.Now.Add(30 * 24 * time.Hour)
	drivePurge(t, h, &maintenance.Purger{Authority: h.Authority, Pool: h.Maintenance}, family.ID, h.Now, 50)
	assertPurged(t, h, family.ID)
	if after := holding(); len(after) != 0 {
		t.Fatalf("after the purge the name is still held by %v", after)
	}
	var stored string
	if err := superuser.QueryRow(t.Context(), `SELECT display_name FROM families WHERE id=$1`, family.ID).Scan(&stored); err != nil || stored != "" {
		t.Fatalf("the purged family's display name %q (%v), want it cleared", stored, err)
	}
	var payload string
	if err := superuser.QueryRow(t.Context(), `SELECT payload::text FROM security_audit_events WHERE scope='family' AND scope_id=$1
		AND kind='family_purge_completed'`, family.ID).Scan(&payload); err != nil || !strings.Contains(payload, `"display_name_cleared": true`) {
		t.Fatalf("purge completion audit payload %s (%v), want display_name_cleared", payload, err)
	}
	// The completion's exact retry returns the stored result and writes
	// nothing.
	start, complete, receipt := maintenance.PurgeCommandIDs(family.ID)
	if _, err := h.Authority.CompleteFamilyPurge(t.Context(), authority.CompleteFamilyPurge{CommandID: complete, FamilyID: family.ID,
		StartCommandID: start, Receipt: receipt, At: h.Now.Add(time.Hour)}); err != nil {
		t.Fatalf("complete retry: %v", err)
	}
	if n := authorityCount(t, h, `SELECT count(*) FROM security_audit_events WHERE scope='family' AND scope_id=$1 AND kind='family_purge_completed'`,
		family.ID); n != 1 {
		t.Fatalf("%d purge completions after the retry", n)
	}
	if stored, err := familyDisplayName(t.Context(), superuser, family.ID); err != nil || stored != "" {
		t.Fatalf("after the retry the display name is %q (%v)", stored, err)
	}
}

func familyDisplayName(ctx context.Context, conn *pgx.Conn, familyID string) (string, error) {
	var name string
	err := conn.QueryRow(ctx, `SELECT display_name FROM families WHERE id=$1`, familyID).Scan(&name)
	return name, err
}

// memberTable is each member-name purge step's table.
var memberTable = map[string]string{"label_history": "family_actor_label_history", "labels": "family_actor_labels",
	"memberships": "memberships", "recovery_claim": "family_recovery_claims"}

// The cascade-order rule: an import_baby_mappings row references its shared
// baby's care row, ON DELETE CASCADE since 000037, so deleting the family's
// babies takes the mappings that name them and leaves the rest of the
// import staging; deleting import_sessions first (cascading through chunks,
// candidates, manifests, conflicts, receipts and mappings) and then the
// babies succeeds too. The purge step deletes import staging first, whatever
// else the family holds.
func TestImportStagingIsDeletedBeforeBabies(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	h.Import(t, h.Owner, "8a000000-0000-4000-8000-000000000002", "Mira", "2026-01-01")
	if h.Count(t, "import_baby_mappings", h.FamilyID) == 0 {
		t.Fatal("no import mapping")
	}
	if _, err := h.Family.ScheduleDeletion(t.Context(), h.Owner, h.CommandID(), true); err != nil {
		t.Fatal(err)
	}
	h.Now = h.Now.Add(30 * 24 * time.Hour)
	start, _, _ := maintenance.PurgeCommandIDs(h.FamilyID)
	if _, err := h.Authority.StartFamilyPurge(t.Context(), authority.StartFamilyPurge{CommandID: start, FamilyID: h.FamilyID, At: h.Now}); err != nil {
		t.Fatal(err)
	}

	// The rule, as the table owner in the family's context: babies first take
	// the mappings that name them, and only those.
	tx := caretest.TenantTx(t, h.Migrator, h.Owner.AccountID, h.FamilyID)
	if tag, err := tx.Exec(t.Context(), `DELETE FROM care_entities WHERE family_id=$1 AND kind=1`, h.FamilyID); err != nil || tag.RowsAffected() == 0 {
		t.Fatalf("deleting babies a mapping names: %v (%d rows)", err, tag.RowsAffected())
	}
	var mappings, sessions int
	if err := tx.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM import_baby_mappings WHERE family_id=$1),
		(SELECT count(*) FROM import_sessions WHERE family_id=$1)`, h.FamilyID).Scan(&mappings, &sessions); err != nil || mappings != 0 || sessions == 0 {
		t.Fatalf("after the babies: %d mappings, %d import sessions (%v); want the mappings gone and the sessions kept", mappings, sessions, err)
	}
	_ = tx.Rollback(t.Context())
	tx = caretest.TenantTx(t, h.Migrator, h.Owner.AccountID, h.FamilyID)
	if _, err := tx.Exec(t.Context(), `DELETE FROM import_sessions WHERE family_id=$1`, h.FamilyID); err != nil {
		t.Fatal(err)
	}
	if tag, err := tx.Exec(t.Context(), `DELETE FROM care_entities WHERE family_id=$1 AND kind=1`, h.FamilyID); err != nil || tag.RowsAffected() == 0 {
		t.Fatalf("babies after import staging: %v (%d rows)", err, tag.RowsAffected())
	}
	_ = tx.Rollback(t.Context())

	// The purge step, as the maintenance login, takes import staging first:
	// one session per call, its cascades with it, then the records, then the
	// babies.
	steps := []string{}
	for range 60 {
		var step string
		var deleted int
		if err := h.Maintenance.QueryRow(t.Context(), `SELECT step, deleted FROM app.purge_family_step($1, 1)`, h.FamilyID).Scan(&step, &deleted); err != nil {
			t.Fatal(err)
		}
		if (step == "none") != (deleted == 0) || deleted > 1 {
			t.Fatalf("purge step %s deleted %d rows at a limit of 1", step, deleted)
		}
		if len(steps) == 0 || steps[len(steps)-1] != step {
			steps = append(steps, step)
		}
		if step == "import_sessions" {
			for _, table := range append([]string{"import_conflicts", "import_conflict_receipts"}, importTables...) {
				if n := h.Count(t, table, h.FamilyID); n != 0 {
					t.Fatalf("%s holds %d rows after its import session went", table, n)
				}
			}
		}
		if step == "none" {
			break
		}
	}
	if got := strings.Join(steps, ","); !strings.HasPrefix(got, "import_sessions,") ||
		!strings.HasSuffix(got, ",babies,label_history,labels,memberships,recovery_claim,none") {
		t.Fatalf("purge steps %s, want import staging first, then the care rows, babies last of them, then the member names", got)
	}
	if n := maintenanceCount(t, h, "care_entities", h.FamilyID); n != 0 {
		t.Fatalf("%d care rows left", n)
	}
}

// Any delete of a family row cascades through import staging and the care
// rows, and import_baby_mappings' key to care_entities cascades too (000037),
// so the order the cascades fire in does not matter: as the table owner,
// deleting a family with a completed import mapping, care rows and receipts
// succeeds and leaves none of them.
func TestFamilyRowDeleteCascadesThroughImportMappings(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	caretest.Seed(t, h, caretest.MixedBatch(caretest.BabyA, 10)...)
	h.Import(t, h.Owner, "8a000000-0000-4000-8000-000000000002", "Mira", "2026-01-01")
	if h.Count(t, "import_baby_mappings", h.FamilyID) == 0 || maintenanceCount(t, h, "care_receipts", h.FamilyID) == 0 {
		t.Fatal("no import mapping or receipt")
	}
	tx, err := h.Migrator.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	// The authority flag and the family's tenant context: RLS admits the
	// family's rows of every table under one or the other.
	if _, err := tx.Exec(t.Context(), `SELECT set_config('app.authority','1',true), set_config('app.family_id',$1,true)`, h.FamilyID); err != nil {
		t.Fatal(err)
	}
	// The family's authority rows that reference it without a cascade go
	// first, as any family delete would remove them; the care and import
	// tables are left to the cascades.
	rows, err := tx.Query(t.Context(), `SELECT c.conrelid::regclass::text, a.attname FROM pg_constraint c
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
		WHERE c.contype = 'f' AND c.confrelid = 'public.families'::regclass AND c.confdeltype <> 'c' AND c.conparentid = 0 AND cardinality(c.conkey) = 1`)
	if err != nil {
		t.Fatal(err)
	}
	var references [][2]string
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			t.Fatal(err)
		}
		references = append(references, [2]string{table, column})
	}
	if rows.Err() != nil || len(references) == 0 {
		t.Fatalf("references to families: %v %v", references, rows.Err())
	}
	for _, reference := range references {
		if strings.Contains(reference[0], "care_") || strings.Contains(reference[0], "import_") {
			t.Fatalf("%s references families without a cascade", reference[0])
		}
		if _, err := tx.Exec(t.Context(), `DELETE FROM `+reference[0]+` WHERE `+reference[1]+`=$1`, h.FamilyID); err != nil {
			t.Fatalf("clear %s: %v", reference[0], err)
		}
		var left int
		if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM `+reference[0]+` WHERE `+reference[1]+`=$1`, h.FamilyID).Scan(&left); err != nil || left != 0 {
			t.Fatalf("%s keeps %d rows of the family (%v)", reference[0], left, err)
		}
	}
	if tag, err := tx.Exec(t.Context(), `DELETE FROM families WHERE id=$1`, h.FamilyID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("deleting a family with an import mapping: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, table := range purgeTables {
		if n := maintenanceCount(t, h, table, h.FamilyID); n != 0 {
			t.Fatalf("%s holds %d rows of the deleted family", table, n)
		}
	}
	for _, table := range importTables {
		if n := h.Count(t, table, h.FamilyID); n != 0 {
			t.Fatalf("%s holds %d rows of the deleted family", table, n)
		}
	}
}

// A purge never waits on its family row: while another transaction holds
// it, a tick does nothing, at once and without error; once it is free the
// purge proceeds.
func TestPurgeSkipsAHeldFamily(t *testing.T) {
	h := testkit.New(t)
	seedPurgeable(t, h)
	h.Now = h.Now.Add(30 * 24 * time.Hour)
	purger := &maintenance.Purger{Authority: h.Authority, Pool: h.Maintenance, Batch: 1000}
	if _, err := purger.Run(t.Context(), h.Now); err != nil || familyStatus(t, h, h.FamilyID) != "purge_started" {
		t.Fatalf("start: %v", err)
	}
	receipts := maintenanceCount(t, h, "care_receipts", h.FamilyID)
	holder, err := h.Maintenance.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(context.Background())
	if _, err := holder.Exec(t.Context(), `SELECT id FROM families WHERE id=$1 FOR NO KEY UPDATE`, h.FamilyID); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		began := time.Now()
		if n, err := purger.Run(t.Context(), h.Now); err != nil || n != 0 || time.Since(began) > time.Second {
			t.Fatalf("beside a held family: %d rows, %v after %v", n, err, time.Since(began))
		}
	}
	if n := maintenanceCount(t, h, "care_receipts", h.FamilyID); n != receipts {
		t.Fatalf("receipts %d -> %d while the family was held", receipts, n)
	}
	if err := holder.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n, err := purger.Run(t.Context(), h.Now); err != nil || n == 0 {
		t.Fatalf("once the family is free: %d rows, %v", n, err)
	}
}

// The purge's maintenance transactions each hold a worker slot: with every
// slot taken a tick waits (and gives up with its context) without touching
// the pool, and it runs once a slot frees.
func TestPurgeRunsInsideAWorkerSlot(t *testing.T) {
	h := testkit.New(t)
	seedPurgeable(t, h)
	h.Now = h.Now.Add(30 * 24 * time.Hour)
	start, _, _ := maintenance.PurgeCommandIDs(h.FamilyID)
	if _, err := h.Authority.StartFamilyPurge(t.Context(), authority.StartFamilyPurge{CommandID: start, FamilyID: h.FamilyID, At: h.Now}); err != nil {
		t.Fatal(err)
	}
	pool, err := database.OpenWithMax(t.Context(), h.Maintenance.Config().ConnString(), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	slots, err := database.NewWorkerSlots(pool)
	if err != nil {
		t.Fatal(err)
	}
	purger := &maintenance.Purger{Authority: h.Authority, Pool: pool, Slots: slots}
	release, err := slots.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := purger.Run(ctx, h.Now); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("with every slot held: %v, want to wait for one", err)
	}
	if acquired := pool.Stat().AcquireCount(); acquired != 0 {
		t.Fatalf("the purge took %d pool connections outside a slot", acquired)
	}
	release()
	if n, err := purger.Run(t.Context(), h.Now); err != nil || n == 0 || pool.Stat().AcquireCount() == 0 {
		t.Fatalf("with a free slot: %d rows, %v after %d acquisitions", n, err, pool.Stat().AcquireCount())
	}
}

// Each step statement deletes at most the batch: a tick of batch 7 removes a
// multiple of whole statements, every one of them at most 7 rows.
func TestPurgeStepsAreBounded(t *testing.T) {
	h := testkit.New(t)
	seedPurgeable(t, h)
	h.Now = h.Now.Add(30 * 24 * time.Hour)
	start, _, _ := maintenance.PurgeCommandIDs(h.FamilyID)
	if _, err := h.Authority.StartFamilyPurge(t.Context(), authority.StartFamilyPurge{CommandID: start, FamilyID: h.FamilyID, At: h.Now}); err != nil {
		t.Fatal(err)
	}
	receipts := maintenanceCount(t, h, "care_receipts", h.FamilyID)
	if receipts <= 7 {
		t.Fatalf("%d receipts, want more than one batch", receipts)
	}
	for _, limit := range []int{0, -1, 10001} {
		var pgErr *pgconn.PgError
		if _, err := h.Maintenance.Exec(t.Context(), `SELECT * FROM app.purge_family_step($1, $2)`, h.FamilyID, limit); !errors.As(err, &pgErr) || pgErr.Code != "22023" {
			t.Errorf("purge step limit %d: %v, want 22023", limit, err)
		}
	}
	type statement struct {
		step string
		n    int
	}
	var statements []statement
	purger := &maintenance.Purger{Authority: h.Authority, Pool: h.Maintenance, Batch: 7}
	purger.FailAt(func(step string, n int) error {
		statements = append(statements, statement{step, n})
		return nil
	})
	drivePurge(t, h, purger, h.FamilyID, h.Now, 200)
	assertPurged(t, h, h.FamilyID)
	// The steps run in order, each table's statements contiguous: no step
	// comes back once a later one ran.
	var order []string
	full := map[string]bool{}
	for _, s := range statements {
		if len(order) == 0 || order[len(order)-1] != s.step {
			order = append(order, s.step)
		}
		switch {
		case s.step == "import_sessions" && s.n != 1:
			t.Fatalf("an import step deleted %d sessions, want one at a time", s.n)
		case s.n > 7:
			t.Fatalf("a %s statement deleted %d rows, more than the batch of 7", s.step, s.n)
		case s.n == 7:
			full[s.step] = true
		}
	}
	want := []string{"care_receipts", "sync_page_bindings", "sync_snapshot_sessions", "device_sync_cursors", "import_sessions", "care_records", "babies",
		"label_history", "labels", "memberships", "recovery_claim", "zero", "complete", "completed"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("purge steps ran %v, want %v", order, want)
	}
	if !full["care_receipts"] || !full["care_records"] {
		t.Fatalf("no full batch of receipts or records (%v): the bound was never reached", full)
	}
}
