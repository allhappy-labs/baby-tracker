package importer_test

import (
	"babytracker/server/internal/access"
	"babytracker/server/internal/care"
	"babytracker/server/internal/family"
	"babytracker/server/internal/protocol"
	familysync "babytracker/server/internal/sync"
	"babytracker/server/internal/testkit"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestImportStagingCommitResumeAndByteRollback(t *testing.T) {
	h, r, m, _ := recordSetup(t)
	r.Finalize = false
	r.TotalRecordCount = 2
	first, e := h.Importer.Resolve(t.Context(), h.Caregiver, r)
	if e != nil || first.ManifestComplete || first.MappingComplete || first.NextChunkIndex != 1 {
		t.Fatalf("stage %+v %v", first, e)
	}
	h.RebuildSyncService(t, h.Limits)
	status, e := h.Importer.Status(t.Context(), h.Caregiver, r.ImportID)
	if e != nil || !reflect.DeepEqual(first, status) {
		t.Fatalf("resume %+v %v", status, e)
	}
	next := r
	next.Babies = []protocol.ImportBaby{}
	next.ChunkIndex = 1
	next.Finalize = true
	next.Records = append([]protocol.RecordManifest(nil), r.Records...)
	next.Records[0].RecordID = testkit.RecordB
	beforeCount, beforeBytes := h.FamilyUsage(t)
	charged := h.Limits.MaxChargedStorageBytesPerFamily - 1
	h.ExecFamily(t, `UPDATE families SET care_charged_bytes=$2 WHERE id=$1`, h.FamilyID, charged)
	if _, e = h.Importer.Resolve(t.Context(), h.Caregiver, next); !errors.Is(e, care.ErrQuotaExceeded) {
		t.Fatalf("storage cap admission: %v", e)
	}
	if count, bytes := h.FamilyUsage(t); count != beforeCount || bytes != charged {
		t.Fatal("failed chunk charged bytes")
	}
	status, e = h.Importer.Status(t.Context(), h.Caregiver, r.ImportID)
	if e != nil || status.NextChunkIndex != 1 || status.ManifestComplete {
		t.Fatalf("failed chunk committed %+v %v", status, e)
	}
	h.ExecFamily(t, `UPDATE families SET care_charged_bytes=$2 WHERE id=$1`, h.FamilyID, beforeBytes)
	final, e := h.Importer.Resolve(t.Context(), h.Caregiver, next)
	if e != nil || !final.MappingComplete {
		t.Fatalf("finalize %+v %v", final, e)
	}
	replay, e := h.Importer.Resolve(t.Context(), h.Caregiver, r)
	if e != nil || !reflect.DeepEqual(first, replay) {
		t.Fatalf("old chunk changed after completion %+v %v", replay, e)
	}
	m.ImportID = &r.ImportID
	m.EntityID = testkit.RecordB
	m.MutationID = h.CommandID()
	created := apply(t, h, h.Caregiver, m)
	h.RebuildSyncService(t, h.Limits)
	out, e := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, m)
	if e != nil || out.Status != "already_applied" || !reflect.DeepEqual(out.Cursor, created.Cursor) {
		t.Fatalf("record crash retry %+v %v", out, e)
	}
}
func TestImportHistoryAndActiveAdmissionRaces(t *testing.T) {
	for _, mode := range []string{"history", "active"} {
		t.Run(mode, func(t *testing.T) {
			h := testkit.New(t)
			if mode == "history" {
				h.Limits.ImportMaxSessionsPerAccount = 1
				h.RebuildSyncService(t, h.Limits)
			}
			start := make(chan struct{})
			var successes atomic.Int32
			var wg sync.WaitGroup
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					r := testkit.ImportRequest(i, "Élie", "2026-01-12")
					if mode == "active" {
						r.Finalize = false
						r.TotalRecordCount = 2
						r.Records = []protocol.RecordManifest{{RecordID: fmt.Sprintf("40000000-0000-4000-8000-%012d", i+1), LocalBabyID: r.Babies[0].LocalBabyID, EntityKind: "feed", ContentSHA256: fmt.Sprintf("%064d", 0)}}
					}
					_, e := h.Importer.Resolve(t.Context(), h.Owner, r)
					if e == nil {
						successes.Add(1)
					} else {
						expected := "import_history_limit"
						if mode == "active" {
							expected = "import_unfinished_limit"
						}
						if e.Error() != expected {
							t.Error(e)
						}
					}
				}(i)
			}
			close(start)
			wg.Wait()
			if successes.Load() != 1 || h.Count(t, "import_sessions", h.FamilyID) != 1 {
				t.Fatal("racy admission exceeded cap")
			}
			if mode == "active" {
				h.ExecFamily(t, `UPDATE import_sessions SET expires_at=created_at+interval '1 second' WHERE family_id=$1`, h.FamilyID)
				h.Now = h.Now.Add(2 * time.Second)
				r := testkit.ImportRequest(2, "Other", "2026-01-12")
				if _, e := h.Importer.Resolve(t.Context(), h.Owner, r); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
}
func TestImportHistoryCountsExpiredAndCompleteRows(t *testing.T) {
	h := testkit.New(t)
	h.Limits.ImportMaxSessionsPerAccount = 2
	h.RebuildSyncService(t, h.Limits)
	r := testkit.ImportRequest(0, "Élie", "2026-01-12")
	if _, e := h.Importer.Resolve(t.Context(), h.Owner, r); e != nil {
		t.Fatal(e)
	}
	r = testkit.ImportRequest(1, "Other", "2026-01-12")
	r.TotalRecordCount = 2
	r.Finalize = false
	r.Records = []protocol.RecordManifest{{RecordID: testkit.RecordA, LocalBabyID: r.Babies[0].LocalBabyID, EntityKind: "feed", ContentSHA256: fmt.Sprintf("%064d", 0)}}
	if _, e := h.Importer.Resolve(t.Context(), h.Owner, r); e != nil {
		t.Fatal(e)
	}
	h.ExecFamily(t, `UPDATE import_sessions SET expires_at=created_at+interval '1 second' WHERE family_id=$1 AND NOT mapping_complete`, h.FamilyID)
	h.Now = h.Now.Add(2 * time.Second)
	_, _ = h.Importer.Status(t.Context(), h.Owner, r.ImportID)
	r = testkit.ImportRequest(2, "Third", "2026-01-12")
	if _, e := h.Importer.Resolve(t.Context(), h.Owner, r); e == nil || e.Error() != "import_history_limit" {
		t.Fatalf("history %v", e)
	}
}
func TestImportMappedParentTombstoneKindAndConcurrentEdit(t *testing.T) {
	for _, mode := range []string{"parent", "tombstone", "kind", "edit"} {
		t.Run(mode, func(t *testing.T) {
			h, r, m, _ := recordSetup(t)
			expected := "record_content_conflict"
			if mode == "parent" {
				seedBaby(t, h, r.Babies[0].LocalBabyID, "Other")
				r.ImportMode = "preserve_local_ids"
				r.Babies[0].Name = "Other"
				p := m.Payload.(protocol.FeedPayload)
				p.BabyID = r.Babies[0].LocalBabyID
				m.Payload = p
			}
			if mode == "kind" {
				r.Records[0].RecordID = m.Payload.(protocol.FeedPayload).BabyID
				m.EntityID = r.Records[0].RecordID
			}
			if _, e := h.Importer.Resolve(t.Context(), h.Caregiver, r); e != nil {
				t.Fatal(e)
			}
			if mode == "tombstone" {
				d := m
				d.Operation = "delete"
				d.MutationID = h.CommandID()
				id := m.Payload.(protocol.FeedPayload).BabyID
				d.Payload = protocol.DeletePayload{BabyID: &id}
				apply(t, h, h.Owner, d)
				expected = "record_deleted_conflict"
			}
			if mode == "edit" {
				d := m
				d.Operation = "update"
				d.MutationID = h.CommandID()
				p := d.Payload.(protocol.FeedPayload)
				note := "intervening edit"
				p.Note = &note
				d.Payload = p
				apply(t, h, h.Owner, d)
			}
			m.ImportID = &r.ImportID
			m.MutationID = h.CommandID()
			before := h.FamilySequence(t)
			out, e := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, m)
			if e != nil || out.Code == nil || *out.Code != expected || h.FamilySequence(t) != before {
				t.Fatalf("%+v %v", out, e)
			}
		})
	}
}
func TestImportTimerManifestAndCollisionAndForbiddenOperations(t *testing.T) {
	h := testkit.New(t)
	r := testkit.ImportRequest(0, "Élie", "2026-01-12")
	m := testkit.MustMutation(t, "timer-feed.json")
	m.EntityID = testkit.TimerA
	m.MutationID = h.CommandID()
	p := m.Payload.(protocol.TimerStartPayload)
	p.BabyID = r.Babies[0].LocalBabyID
	m.Payload = p
	hash, _ := protocol.CanonicalContentHash("timer", p)
	r.Records = []protocol.RecordManifest{{RecordID: m.EntityID, EntityKind: "timer", LocalBabyID: p.BabyID, ContentSHA256: hex.EncodeToString(hash[:])}}
	r.TotalRecordCount = 1
	if _, e := h.Importer.Resolve(t.Context(), h.Owner, r); e != nil {
		t.Fatal(e)
	}
	m.ImportID = &r.ImportID
	apply(t, h, h.Owner, m)
	m.MutationID = h.CommandID()
	out, e := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if e != nil || out.Status == "applied" {
		t.Fatalf("timer dedup %+v %v", out, e)
	}
	m.MutationID = h.CommandID()
	m.Operation = "timer_stop"
	m.Payload = protocol.TimerStopPayload{BabyID: p.BabyID, StoppedAt: "2026-09-20T15:10:00Z", CompletedRecordID: testkit.RecordA}
	out, e = h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if e != nil || out.Code == nil || *out.Code != "invalid_import_operation" {
		t.Fatalf("stop %+v %v", out, e)
	}
	if !h.TimerIsLive(t, testkit.TimerA) {
		t.Fatal("import stop changed timer")
	}
}

// wakeObserver records post-commit publishes (the only hint; the write
// transaction schedules nothing).
type wakeObserver struct {
	published int
	cursors   []protocol.Cursor
}

func (w *wakeObserver) Publish(_ context.Context, _ string, cursor protocol.Cursor) error {
	w.published++
	w.cursors = append(w.cursors, cursor)
	return nil
}

func TestImportFinalizationObservesOneHighestCursorAndReplayIsSilent(t *testing.T) {
	h := testkit.New(t)
	request := testkit.ImportRequest(0, "First", "2026-01-12")
	second := request.Babies[0]
	second.LocalBabyID = "20000000-0000-4000-8000-000000000099"
	second.Name = "Second"
	second.SortOrder = 1
	request.Babies = append(request.Babies, second)
	observer := &wakeObserver{}
	h.Importer.SetPublisher(observer)
	before := h.FamilySequence(t)
	if _, err := h.Importer.Resolve(t.Context(), h.Owner, request); err != nil {
		t.Fatal(err)
	}
	after := h.FamilySequence(t)
	if after != before+2 || observer.published != 1 || len(observer.cursors) != 1 || observer.cursors[0].Sequence != after {
		t.Fatalf("before=%d after=%d observer=%+v", before, after, observer)
	}
	if _, err := h.Importer.Resolve(t.Context(), h.Owner, request); err != nil {
		t.Fatal(err)
	}
	if observer.published != 1 {
		t.Fatalf("replay observed %+v", observer)
	}
}
func TestImportEqualNoOpAdvancesNoSequenceAndPublishesNothing(t *testing.T) {
	h, r, m, _ := recordSetup(t)
	if _, e := h.Importer.Resolve(t.Context(), h.Caregiver, r); e != nil {
		t.Fatal(e)
	}
	observer := &wakeObserver{}
	h.Service = familysync.New(familysync.Options{Guard: h.Authority, Server: h.Info, Limits: h.Limits, Now: func() time.Time { return h.Now },
		Publisher: observer, PageKeys: testkit.PageKeyring("page.v1", 0x77), Allowed: family.Allowed})
	h.Service.SetImportAdmission(h.Importer)
	m.ImportID = &r.ImportID
	m.MutationID = h.CommandID()
	before := h.FamilySequence(t)
	apply(t, h, h.Caregiver, m)
	if h.FamilySequence(t) != before || observer.published != 0 {
		t.Fatal("equal admission advanced the family sequence or published a high water")
	}
}

func TestImportFamilyUnfinishedAndHistoryCapsAreIndependent(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h, "10000000-0000-4000-8000-000000000010", "Élie")
	seedBaby(t, h, "10000000-0000-4000-8000-000000000011", "Élie")
	principals := []access.Principal{h.Owner, h.Caregiver, h.AddCaregiver(t, "third"), h.AddCaregiver(t, "fourth"), h.AddCaregiver(t, "fifth")}
	for i, p := range principals {
		_, e := h.Importer.Resolve(t.Context(), p, testkit.ImportRequest(i, "Élie", "2026-01-12"))
		if i < 4 && e != nil {
			t.Fatal(e)
		}
		if i == 4 && (e == nil || e.Error() != "import_unfinished_limit") {
			t.Fatalf("family unfinished cap %v", e)
		}
	}
	h.ExecFamily(t, `UPDATE import_sessions SET expires_at=created_at+interval '1 second' WHERE family_id=$1`, h.FamilyID)
	h.Now = h.Now.Add(2 * time.Second)
	h.Limits.ImportMaxSessionsPerFamily = 10
	h.RebuildSyncService(t, h.Limits)
	for i := 0; i < 6; i++ {
		if _, e := h.Importer.Resolve(t.Context(), h.Owner, testkit.ImportRequest(i+5, fmt.Sprintf("History %d", i), "2026-01-12")); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := h.Importer.Resolve(t.Context(), principals[4], testkit.ImportRequest(4, "Élie", "2026-01-12")); e == nil || e.Error() != "import_history_limit" {
		t.Fatalf("family history cap %v", e)
	}
}

func TestImportResumeNewDeviceAndRevocationDenial(t *testing.T) {
	h := testkit.New(t)
	r := testkit.ImportRequest(0, "Élie", "2026-01-12")
	original, e := h.Importer.Resolve(t.Context(), h.Caregiver, r)
	if e != nil {
		t.Fatal(e)
	}
	p, e := h.NewDevice("caregiver")
	if e != nil {
		t.Fatal(e)
	}
	retry, e := h.Importer.Resolve(t.Context(), p, r)
	if e != nil || !reflect.DeepEqual(original, retry) {
		t.Fatalf("cross-device replay %+v %v", retry, e)
	}
	if _, e = h.Family.Remove(t.Context(), h.Owner, h.CommandID(), p.AccountID); e != nil {
		t.Fatal(e)
	}
	if _, e = h.Importer.Status(t.Context(), p, r.ImportID); e == nil {
		t.Fatal("removed member resumed import")
	}
	if _, e = h.Importer.Status(t.Context(), h.Outsider, r.ImportID); e == nil {
		t.Fatal("foreign account viewed import")
	}
}

func TestImportStagingRejectsGapsDuplicatesAndChangedImmutableFields(t *testing.T) {
	h, r, _, _ := recordSetup(t)
	r.TotalRecordCount = 2
	r.Finalize = false
	if _, e := h.Importer.Resolve(t.Context(), h.Caregiver, r); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []string{"gap", "duplicate", "mode", "count", "babies"} {
		t.Run(tc, func(t *testing.T) {
			next := r
			next.Babies = []protocol.ImportBaby{}
			next.Records = append([]protocol.RecordManifest{}, r.Records...)
			next.ChunkIndex = 1
			next.Finalize = true
			next.Records[0].RecordID = testkit.RecordB
			switch tc {
			case "gap":
				next.ChunkIndex = 2
			case "duplicate":
				next.Records[0].RecordID = r.Records[0].RecordID
			case "mode":
				next.ImportMode = "preserve_local_ids"
			case "count":
				next.TotalRecordCount = 3
			case "babies":
				next.Babies = r.Babies
			}
			if _, e := h.Importer.Resolve(t.Context(), h.Caregiver, next); e == nil {
				t.Fatal("invalid chunk admitted")
			}
			status, e := h.Importer.Status(t.Context(), h.Caregiver, r.ImportID)
			if e != nil || status.NextChunkIndex != 1 || status.MappingComplete {
				t.Fatalf("invalid chunk changed checkpoint %+v %v", status, e)
			}
		})
	}
}

func TestImportUnfinishedAccountCapIsGlobalAcrossFamilies(t *testing.T) {
	h := testkit.New(t)
	r := testkit.ImportRequest(0, "Élie", "2026-01-12")
	r.Finalize = false
	r.TotalRecordCount = 2
	r.Records = []protocol.RecordManifest{{RecordID: testkit.RecordA, EntityKind: "feed", LocalBabyID: r.Babies[0].LocalBabyID, ContentSHA256: fmt.Sprintf("%064d", 0)}}
	if _, e := h.Importer.Resolve(t.Context(), h.Caregiver, r); e != nil {
		t.Fatal(e)
	}
	old := h.Caregiver
	if _, e := h.Family.Leave(t.Context(), h.Caregiver, h.CommandID()); e != nil {
		t.Fatal(e)
	}
	var reauthErr error
	h.Caregiver, reauthErr = h.Reauthenticate("caregiver")
	if reauthErr != nil {
		t.Fatal(reauthErr)
	}
	if _, e := h.Family.Create(t.Context(), h.Caregiver, h.CommandID(), "Second family"); e != nil {
		t.Fatal(e)
	}
	h.Caregiver = h.Refresh("caregiver")
	if _, e := h.Importer.Status(t.Context(), h.Caregiver, r.ImportID); e == nil || e.Error() != "import_not_found" {
		t.Fatalf("foreign family leaked import: %v", e)
	}
	if _, e := h.Importer.Status(t.Context(), old, r.ImportID); e == nil {
		t.Fatal("stale family session admitted")
	}
	if _, e := h.Importer.Resolve(t.Context(), h.Caregiver, testkit.ImportRequest(1, "Other", "2026-01-12")); e == nil || e.Error() != "import_unfinished_limit" {
		t.Fatalf("global account cap %v", e)
	}
}
