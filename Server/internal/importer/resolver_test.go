package importer_test

import (
	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"encoding/hex"
	"reflect"
	"sync"
	"testing"
)

func TestConcurrentImportConvergesOnOneBaby(t *testing.T) {
	h := testkit.New(t)
	start := make(chan struct{})
	results := make(chan protocol.ImportResolveResponse, 2)
	var wg sync.WaitGroup
	for i, p := range []access.Principal{h.Owner, h.Caregiver} {
		wg.Add(1)
		go func(i int, p access.Principal) {
			defer wg.Done()
			<-start
			out, err := h.Importer.Resolve(t.Context(), p, testkit.ImportRequest(i, "Élie", "2026-01-12"))
			if err != nil {
				t.Error(err)
				return
			}
			results <- out
		}(i, p)
	}
	close(start)
	wg.Wait()
	close(results)
	ids := []string{}
	for out := range results {
		if !out.MappingComplete || len(out.Mappings) != 1 {
			t.Fatalf("incomplete: %+v", out)
		}
		ids = append(ids, out.Mappings[0].SharedBabyID)
	}
	if len(ids) != 2 || ids[0] != ids[1] || h.Count(t, "babies", h.FamilyID) != 1 {
		t.Fatal("concurrent import did not converge")
	}
}
func distinct(t *testing.T, mode string) {
	t.Helper()
	h := testkit.New(t)
	r := testkit.ImportRequest(0, "Élie", "2026-01-12")
	r.ImportMode = mode
	b := r.Babies[0]
	b.LocalBabyID = "20000000-0000-4000-8000-000000000002"
	b.SortOrder = 1
	r.Babies = append(r.Babies, b)
	out, err := h.Importer.Resolve(t.Context(), h.Owner, r)
	if err != nil || !out.MappingComplete || len(out.Mappings) != 2 {
		t.Fatalf("%+v %v", out, err)
	}
	for _, m := range out.Mappings {
		if m.LocalBabyID != m.SharedBabyID {
			t.Fatal("collapsed siblings")
		}
	}
	if h.Count(t, "babies", h.FamilyID) != 2 {
		t.Fatal("lost sibling")
	}
}
func TestCreateFamilyImportPreservesDistinctEqualLookingLocalBabies(t *testing.T) {
	distinct(t, "preserve_local_ids")
}
func TestMatchSharedPreservesDistinctUnmatchedLocalBabies(t *testing.T) { distinct(t, "match_shared") }
func TestManifestChangedChunkRejected(t *testing.T) {
	h := testkit.New(t)
	r := testkit.ImportRequest(0, "Élie", "2026-01-12")
	a, e := h.Importer.Resolve(t.Context(), h.Owner, r)
	if e != nil {
		t.Fatal(e)
	}
	b, e := h.Importer.Resolve(t.Context(), h.Owner, r)
	if e != nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("retry changed: %+v %v", b, e)
	}
	r.ImportMode = "preserve_local_ids"
	_, e = h.Importer.Resolve(t.Context(), h.Owner, r)
	if e == nil || e.Error() != "import_chunk_reused" {
		t.Fatalf("changed chunk: %v", e)
	}
}
func TestImportResumeAfterEachCommitBoundary(t *testing.T) {
	h := testkit.New(t)
	r := testkit.ImportRequest(0, "Élie", "2026-01-12")
	a, e := h.Importer.Resolve(t.Context(), h.Owner, r)
	if e != nil {
		t.Fatal(e)
	}
	h.RebuildSyncService(t)
	b, e := h.Importer.Status(t.Context(), h.Owner, r.ImportID)
	if e != nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("resume: %+v %v", b, e)
	}
}
func TestImportEmptyNonfinalChunkRejected(t *testing.T) {
	h := testkit.New(t)
	r := testkit.ImportRequest(0, "Élie", "2026-01-12")
	r.Finalize = false
	_, e := h.Importer.Resolve(t.Context(), h.Owner, r)
	if e == nil {
		t.Fatal("empty nonfinal admitted")
	}
	if h.Count(t, "import_sessions", h.FamilyID) != 0 {
		t.Fatal("persisted invalid session")
	}
}
func TestImportCrossAccountDenied(t *testing.T) {
	h := testkit.New(t)
	r := testkit.ImportRequest(0, "Élie", "2026-01-12")
	_, e := h.Importer.Resolve(t.Context(), h.Owner, r)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = h.Importer.Status(t.Context(), h.Caregiver, r.ImportID); e == nil {
		t.Fatal("foreign account status leaked")
	}
	if _, e = h.Importer.Resolve(t.Context(), h.Caregiver, r); e == nil {
		t.Fatal("foreign account replay admitted")
	}
}
func TestMappingNeverChangesAfterRename(t *testing.T) {
	h := testkit.New(t)
	r := testkit.ImportRequest(0, "Élie", "2026-01-12")
	a, e := h.Importer.Resolve(t.Context(), h.Owner, r)
	if e != nil {
		t.Fatal(e)
	}
	h.RenameBaby(t, a.Mappings[0].SharedBabyID, "New name")
	b, e := h.Importer.Status(t.Context(), h.Owner, r.ImportID)
	if e != nil || !reflect.DeepEqual(a.Mappings, b.Mappings) {
		t.Fatalf("mapping changed: %+v %v", b, e)
	}
}
func TestPreserveIDsModeDoesNotFuzzyMatchExistingBaby(t *testing.T) {
	h := testkit.New(t)
	r := testkit.ImportRequest(0, "Élie", "2026-01-12")
	if _, e := h.Importer.Resolve(t.Context(), h.Owner, r); e != nil {
		t.Fatal(e)
	}
	r = testkit.ImportRequest(1, "Élie", "2026-01-12")
	r.ImportMode = "preserve_local_ids"
	a, e := h.Importer.Resolve(t.Context(), h.Owner, r)
	if e != nil || a.Mappings[0].SharedBabyID != r.Babies[0].LocalBabyID || h.Count(t, "babies", h.FamilyID) != 2 {
		t.Fatalf("%+v %v", a, e)
	}
}
func TestEqualRecordManifestAfterRemapIsNoOp(t *testing.T) {
	h, r, m, original := recordSetup(t)
	out, e := h.Importer.Resolve(t.Context(), h.Caregiver, r)
	if e != nil || !out.MappingComplete || out.Mappings[0].SharedBabyID == r.Babies[0].LocalBabyID {
		t.Fatalf("remap %+v %v", out, e)
	}
	m.ImportID = &r.ImportID
	m.MutationID = h.CommandID()
	before := h.FamilySequence(t)
	got, e := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, m)
	if e != nil || got.Status != "applied" || !reflect.DeepEqual(got.CanonicalEntity, original.CanonicalEntity) || !reflect.DeepEqual(got.Cursor, original.Cursor) || h.FamilySequence(t) != before {
		t.Fatalf("no-op %+v canonical=%#v want=%#v %v", got, got.CanonicalEntity, original.CanonicalEntity, e)
	}
	m2 := m
	m2.ImportID = nil
	m2.MutationID = h.CommandID()
	m2.Operation = "update"
	payload := m2.Payload.(protocol.FeedPayload)
	note := "changed"
	payload.Note = &note
	m2.Payload = payload
	apply(t, h, h.Owner, m2)
	retry, e := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, m)
	if e != nil || retry.Status != "already_applied" || !reflect.DeepEqual(retry.CanonicalEntity, original.CanonicalEntity) || !reflect.DeepEqual(retry.Cursor, original.Cursor) {
		t.Fatalf("receipt changed %+v canonical=%#v want=%#v %v", retry, retry.CanonicalEntity, original.CanonicalEntity, e)
	}
}

func apply(t *testing.T, h *testkit.Harness, p access.Principal, m protocol.Mutation) protocol.Outcome {
	t.Helper()
	out, e := h.Service.Apply(t.Context(), p, h.FamilyID, m)
	if e != nil || out.Status != "applied" {
		t.Fatalf("apply %+v %v", out, e)
	}
	return out
}
func seedBaby(t *testing.T, h *testkit.Harness, id, name string) {
	t.Helper()
	m := testkit.MustMutation(t, "baby.json")
	m.MutationID = h.CommandID()
	m.EntityID = id
	m.Payload = protocol.BabyPayload{Name: name, BirthDate: "2026-01-12"}
	apply(t, h, h.Owner, m)
}
func recordSetup(t *testing.T) (*testkit.Harness, protocol.ImportResolveRequest, protocol.Mutation, protocol.Outcome) {
	t.Helper()
	h := testkit.New(t)
	baby := "10000000-0000-4000-8000-000000000010"
	seedBaby(t, h, baby, "Élie")
	m := testkit.MustMutation(t, "feed-bottle.json")
	m.EntityID = testkit.RecordA
	m.MutationID = h.CommandID()
	payload := m.Payload.(protocol.FeedPayload)
	payload.BabyID = baby
	m.Payload = payload
	original := apply(t, h, h.Owner, m)
	r := testkit.ImportRequest(0, "Élie", "2026-01-12")
	hash, e := protocol.CanonicalContentHash(m.EntityKind, m.Payload)
	if e != nil {
		t.Fatal(e)
	}
	r.Records = []protocol.RecordManifest{{RecordID: m.EntityID, EntityKind: m.EntityKind, LocalBabyID: r.Babies[0].LocalBabyID, ContentSHA256: hex.EncodeToString(hash[:])}}
	r.TotalRecordCount = 1
	return h, r, m, original
}
func TestDifferentContentImportNeverUsesLWW(t *testing.T) {
	h, r, m, _ := recordSetup(t)
	payload := m.Payload.(protocol.FeedPayload)
	note := "local change"
	payload.Note = &note
	m.Payload = payload
	hash, _ := protocol.CanonicalContentHash(m.EntityKind, m.Payload)
	r.Records[0].ContentSHA256 = hex.EncodeToString(hash[:])
	out, e := h.Importer.Resolve(t.Context(), h.Caregiver, r)
	if e != nil || !out.MappingComplete || len(out.Conflicts) != 1 || out.Conflicts[0].Kind != "record_content_conflict" {
		t.Fatalf("%+v %v", out, e)
	}
	m.ImportID = &r.ImportID
	m.MutationID = h.CommandID()
	before := h.FamilySequence(t)
	got, e := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, m)
	if e != nil || got.Code == nil || *got.Code != "record_content_conflict" || h.FamilySequence(t) != before {
		t.Fatalf("LWW %+v %v", got, e)
	}
}
func TestDifferentRecordUUIDsRemainSeparate(t *testing.T) {
	h, r, m, _ := recordSetup(t)
	r.Records[0].RecordID = testkit.RecordB
	m.EntityID = testkit.RecordB
	if _, e := h.Importer.Resolve(t.Context(), h.Caregiver, r); e != nil {
		t.Fatal(e)
	}
	m.MutationID = h.CommandID()
	m.ImportID = &r.ImportID
	apply(t, h, h.Caregiver, m)
	if h.Count(t, "feed_entries", h.FamilyID) != 2 {
		t.Fatal("equal-looking distinct records collapsed")
	}
}
func TestDeletedMappedBabyTerminatesImportWrites(t *testing.T) {
	h, r, m, _ := recordSetup(t)
	if _, e := h.Importer.Resolve(t.Context(), h.Caregiver, r); e != nil {
		t.Fatal(e)
	}
	del := testkit.MustMutation(t, "baby.json")
	del.EntityID = m.Payload.(protocol.FeedPayload).BabyID
	del.MutationID = h.CommandID()
	del.Operation = "delete"
	del.Payload = protocol.DeletePayload{}
	apply(t, h, h.Owner, del)
	m.ImportID = &r.ImportID
	m.MutationID = h.CommandID()
	out, e := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, m)
	if e != nil || out.Code == nil || *out.Code != "baby_deleted" {
		t.Fatalf("%+v %v", out, e)
	}
}
func TestImportUUIDCollisionDoesNotOverwrite(t *testing.T) {
	h := testkit.New(t)
	r := testkit.ImportRequest(0, "Élie", "2026-01-12")
	seedBaby(t, h, r.Babies[0].LocalBabyID, "Other child")
	r.ImportMode = "preserve_local_ids"
	before := h.FamilySequence(t)
	out, e := h.Importer.Resolve(t.Context(), h.Owner, r)
	if e != nil || out.MappingComplete || len(out.Conflicts) != 1 || out.Conflicts[0].Kind != "baby_uuid_collision" || h.FamilySequence(t) != before {
		t.Fatalf("collision %+v %v", out, e)
	}
}
