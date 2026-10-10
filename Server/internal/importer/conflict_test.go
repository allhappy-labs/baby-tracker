package importer_test

import (
	"slices"
	"strings"

	"babytracker/server/internal/protocol"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testkit"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func choose(h *testkit.Harness, out protocol.ImportResolveResponse, c protocol.ImportConflict, choice string, id *string) protocol.ImportConflictRequest {
	return protocol.ImportConflictRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{"records-v1", "import-resolver-v1"}}, CommandID: h.CommandID(), ResolverRevision: out.ResolverRevision, ConflictID: c.ConflictID, Choice: choice, CanonicalBabyID: id}
}
func ambiguous(t *testing.T) (*testkit.Harness, protocol.ImportResolveRequest, protocol.ImportResolveResponse) {
	t.Helper()
	h := testkit.New(t)
	seedBaby(t, h, "10000000-0000-4000-8000-000000000010", "Élie")
	seedBaby(t, h, "10000000-0000-4000-8000-000000000011", "Élie")
	r := testkit.ImportRequest(0, "Élie", "2026-01-12")
	out, e := h.Importer.Resolve(t.Context(), h.Caregiver, r)
	if e != nil || out.MappingComplete || len(out.Conflicts) != 1 {
		t.Fatalf("%+v %v", out, e)
	}
	return h, r, out
}
func TestJoinLocalAmbiguityRequiresDistinctOwnerChoices(t *testing.T) {
	h := testkit.New(t)
	shared := "10000000-0000-4000-8000-000000000010"
	seedBaby(t, h, shared, "Élie")
	r := testkit.ImportRequest(0, "Élie", "2026-01-12")
	b := r.Babies[0]
	b.LocalBabyID = "20000000-0000-4000-8000-000000000002"
	r.Babies = append(r.Babies, b)
	feed := testkit.MustMutation(t, "feed-bottle.json")
	feed.MutationID = h.CommandID()
	feed.EntityID = testkit.RecordA
	fp := feed.Payload.(protocol.FeedPayload)
	fp.BabyID = r.Babies[0].LocalBabyID
	feed.Payload = fp
	timer := testkit.MustMutation(t, "timer-feed.json")
	timer.MutationID = h.CommandID()
	timer.EntityID = testkit.TimerA
	tp := timer.Payload.(protocol.TimerStartPayload)
	tp.BabyID = b.LocalBabyID
	timer.Payload = tp
	for _, m := range []protocol.Mutation{feed, timer} {
		hash, _ := protocol.CanonicalContentHash(m.EntityKind, m.Payload)
		local := fp.BabyID
		if m.EntityKind == "timer" {
			local = tp.BabyID
		}
		r.Records = append(r.Records, protocol.RecordManifest{RecordID: m.EntityID, EntityKind: m.EntityKind, LocalBabyID: local, ContentSHA256: hex.EncodeToString(hash[:])})
	}
	r.TotalRecordCount = 2
	out, e := h.Importer.Resolve(t.Context(), h.Caregiver, r)
	if e != nil || out.MappingComplete || len(out.Mappings) != 0 || len(out.Conflicts) != 2 || out.Conflicts[0].Kind != "ambiguous_local_babies" {
		t.Fatalf("%+v %v", out, e)
	}
	// The owner's publishing choice announces the new high water to the live
	// hub once, after the commit; the other choices publish nothing.
	observer := &wakeObserver{}
	h.Importer.SetPublisher(observer)
	first := choose(h, out, out.Conflicts[0], "use_existing", &shared)
	out, e = h.Importer.ResolveConflict(t.Context(), h.Owner, r.ImportID, first)
	if e != nil || out.MappingComplete || len(out.Mappings) != 0 || len(out.Conflicts) != 1 || observer.published != 0 {
		t.Fatalf("first %+v %v, %d publishes", out, e, observer.published)
	}
	bad := choose(h, out, out.Conflicts[0], "use_existing", &shared)
	if _, e = h.Importer.ResolveConflict(t.Context(), h.Owner, r.ImportID, bad); e == nil || e.Error() != "import_target_already_assigned" {
		t.Fatalf("duplicate target %v", e)
	}
	last := choose(h, out, out.Conflicts[0], "keep_local", nil)
	out, e = h.Importer.ResolveConflict(t.Context(), h.Owner, r.ImportID, last)
	if e != nil || !out.MappingComplete || len(out.Mappings) != 2 || out.Mappings[0].SharedBabyID == out.Mappings[1].SharedBabyID {
		t.Fatalf("publication %+v %v", out, e)
	}
	if observer.published != 1 || observer.cursors[0].Sequence != h.FamilySequence(t) || observer.cursors[0].FamilyID != h.FamilyID {
		t.Fatalf("publication publishes=%d cursors=%+v at sequence %d", observer.published, observer.cursors, h.FamilySequence(t))
	}
	retry, e := h.Importer.ResolveConflict(t.Context(), h.Owner, r.ImportID, last)
	if e != nil || !reflect.DeepEqual(out, retry) {
		t.Fatalf("choice replay %+v %v", retry, e)
	}
	last.Choice = "use_existing"
	last.CanonicalBabyID = &shared
	if _, e = h.Importer.ResolveConflict(t.Context(), h.Owner, r.ImportID, last); e == nil || e.Error() != "command_id_reused" {
		t.Fatalf("choice reuse %v", e)
	}
	if observer.published != 1 {
		t.Fatalf("a replayed or refused choice published: %d", observer.published)
	}
	targets := map[string]string{}
	for _, mapping := range out.Mappings {
		targets[mapping.LocalBabyID] = mapping.SharedBabyID
	}
	fp.BabyID = targets[fp.BabyID]
	feed.Payload = fp
	feed.ImportID = &r.ImportID
	fo := apply(t, h, h.Caregiver, feed)
	tp.BabyID = targets[tp.BabyID]
	timer.Payload = tp
	timer.ImportID = &r.ImportID
	to := apply(t, h, h.Caregiver, timer)
	if fo.CanonicalEntity.Payload.(protocol.FeedPayload).BabyID != fp.BabyID || to.CanonicalEntity.Payload.(protocol.TimerStartPayload).BabyID != tp.BabyID || fp.BabyID == tp.BabyID {
		t.Fatal("imported references collapsed sibling selection")
	}
}
func TestOwnerResolutionRejectsChangedCandidateRevision(t *testing.T) {
	for _, change := range []string{"rename", "add", "delete"} {
		t.Run(change, func(t *testing.T) {
			h, r, out := ambiguous(t)
			req := choose(h, out, out.Conflicts[0], "keep_local", nil)
			switch change {
			case "rename":
				h.RenameBaby(t, out.Conflicts[0].CandidateBabyIDs[0], "Changed")
			case "add":
				seedBaby(t, h, "10000000-0000-4000-8000-000000000012", "Élie")
			case "delete":
				m := testkit.MustMutation(t, "baby.json")
				m.EntityID = out.Conflicts[0].CandidateBabyIDs[0]
				m.MutationID = h.CommandID()
				m.Operation = "delete"
				m.Payload = protocol.DeletePayload{}
				apply(t, h, h.Owner, m)
			}
			before := h.FamilySequence(t)
			if _, e := h.Importer.ResolveConflict(t.Context(), h.Owner, r.ImportID, req); e == nil || e.Error() != "import_resolution_stale" {
				t.Fatalf("stale choice %v", e)
			}
			out, e := h.Importer.Status(t.Context(), h.Caregiver, r.ImportID)
			if e != nil || out.MappingComplete || out.ResolverRevision <= req.ResolverRevision || h.Count(t, "import_baby_mappings", h.FamilyID) != 0 || h.FamilySequence(t) != before {
				t.Fatalf("stale published %+v %v", out, e)
			}
			req = choose(h, out, out.Conflicts[0], "keep_local", nil)
			out, e = h.Importer.ResolveConflict(t.Context(), h.Owner, r.ImportID, req)
			if e != nil || !out.MappingComplete {
				t.Fatalf("fresh choice %+v %v", out, e)
			}
		})
	}
}
func TestImportExpiryCannotReopenFinalizedAmbiguity(t *testing.T) {
	h, r, _ := ambiguous(t)
	h.ExecFamily(t, `UPDATE import_sessions SET expires_at=created_at+interval '1 second' WHERE family_id=$1`, h.FamilyID)
	h.Now = h.Now.Add(2 * time.Second)
	if _, e := h.Importer.Status(t.Context(), h.Caregiver, r.ImportID); e == nil || e.Error() != "import_expired" {
		t.Fatalf("expiry %v", e)
	}
	h.Now = h.Now.Add(-2 * time.Second)
	if _, e := h.Importer.Resolve(t.Context(), h.Caregiver, r); e == nil || e.Error() != "import_expired" {
		t.Fatalf("reopened %v", e)
	}
}
func TestImportConflictPagingBindsViewerRevisionKeyAndExpiry(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h, "10000000-0000-4000-8000-000000000010", "Élie")
	seedBaby(t, h, "10000000-0000-4000-8000-000000000011", "Élie")
	r := testkit.ImportRequest(0, "Élie", "2026-01-12")
	r.ImportID = "10000000-0000-4000-8000-000000000002"
	for i := 2; i <= 4; i++ {
		b := r.Babies[0]
		b.LocalBabyID = fmt.Sprintf("20000000-0000-4000-8000-%012d", i)
		r.Babies = append(r.Babies, b)
	}
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t, h.Limits)
	out, e := h.Importer.Resolve(t.Context(), h.Caregiver, r)
	if e != nil || out.ConflictCount != 4 || out.ConflictsComplete || out.NextConflictPageToken == nil {
		t.Fatalf("first page %+v %v", out, e)
	}
	token := out.NextConflictPageToken
	seen := map[string]bool{}
	for {
		for _, c := range out.Conflicts {
			if seen[c.ConflictID] {
				t.Fatal("duplicate page row")
			}
			seen[c.ConflictID] = true
		}
		if out.ConflictsComplete {
			break
		}
		out, e = h.Importer.StatusPage(t.Context(), h.Caregiver, r.ImportID, out.NextConflictPageToken)
		if e != nil {
			t.Fatal(e)
		}
	}
	if len(seen) != 4 {
		t.Fatal("truncated conflict traversal")
	}
	if _, e = h.Importer.StatusPage(t.Context(), h.Owner, r.ImportID, token); e == nil {
		t.Fatal("viewer token replay")
	}
	old := testkit.PageKeyring("page.v1", 0x77).Current
	h.RebuildSyncServiceWithPageKeys(t, testkit.PageKeyring("page.v2", 0x78, old))
	if _, e = h.Importer.StatusPage(t.Context(), h.Caregiver, r.ImportID, token); e != nil {
		t.Fatalf("retained key %v", e)
	}
	h.RebuildSyncServiceWithPageKeys(t, testkit.PageKeyring("page.v2", 0x78))
	if _, e = h.Importer.StatusPage(t.Context(), h.Caregiver, r.ImportID, token); e == nil {
		t.Fatal("removed key accepted")
	}
	h.RebuildSyncService(t, h.Limits)
	h.Now = h.Now.Add(16 * time.Minute)
	if _, e = h.Importer.StatusPage(t.Context(), h.Caregiver, r.ImportID, token); e == nil || e.Error() != "page_expired" {
		t.Fatalf("expired token %v", e)
	}
	h.Now = h.Now.Add(-16 * time.Minute)
	out, e = h.Importer.Status(t.Context(), h.Owner, r.ImportID)
	if e != nil {
		t.Fatal(e)
	}
	req := choose(h, out, out.Conflicts[0], "keep_local", nil)
	if _, e = h.Importer.ResolveConflict(t.Context(), h.Owner, r.ImportID, req); e != nil {
		t.Fatal(e)
	}
	if _, e = h.Importer.StatusPage(t.Context(), h.Caregiver, r.ImportID, token); e == nil || e.Error() != "import_resolution_stale" {
		t.Fatalf("stale token %v", e)
	}
}

func TestImportConflictPagingAccountsForWholeByteEnvelope(t *testing.T) {
	h := testkit.New(t)
	h.Limits.MaxLiveBabies = 100
	h.Limits.SyncPageBytes = protocol.MinimumSyncPageBytes
	h.RebuildSyncService(t, h.Limits)
	for i := 0; i < 100; i++ {
		seedBaby(t, h, fmt.Sprintf("10000000-0000-4000-8000-%012d", 100+i), "Élie")
	}
	r := testkit.ImportRequest(0, "Élie", "2026-01-12")
	for i := 2; i <= 50; i++ {
		b := r.Babies[0]
		b.LocalBabyID = fmt.Sprintf("20000000-0000-4000-8000-%012d", i)
		r.Babies = append(r.Babies, b)
	}
	out, e := h.Importer.Resolve(t.Context(), h.Caregiver, r)
	if e != nil {
		t.Fatal(e)
	}
	total, pages := 0, 0
	for {
		raw, _ := json.Marshal(out)
		if len(raw) > h.Limits.SyncPageBytes || len(out.Conflicts) == 0 || out.ConflictCount != 50 {
			t.Fatalf("unbounded/empty page: bytes=%d count=%d", len(raw), out.ConflictCount)
		}
		total += len(out.Conflicts)
		pages++
		if out.ConflictsComplete {
			break
		}
		out, e = h.Importer.StatusPage(t.Context(), h.Caregiver, r.ImportID, out.NextConflictPageToken)
		if e != nil {
			t.Fatal(e)
		}
	}
	if total != 50 || pages < 2 {
		t.Fatalf("paging lost rows: count=%d pages=%d", total, pages)
	}
}

func TestOwnerResolutionInvalidatesPriorChoiceBeforePublication(t *testing.T) {
	h := testkit.New(t)
	shared := "10000000-0000-4000-8000-000000000010"
	seedBaby(t, h, shared, "Élie")
	r := testkit.ImportRequest(0, "Élie", "2026-01-12")
	b := r.Babies[0]
	b.LocalBabyID = "20000000-0000-4000-8000-000000000002"
	r.Babies = append(r.Babies, b)
	out, e := h.Importer.Resolve(t.Context(), h.Caregiver, r)
	if e != nil {
		t.Fatal(e)
	}
	first := choose(h, out, out.Conflicts[0], "use_existing", &shared)
	out, e = h.Importer.ResolveConflict(t.Context(), h.Owner, r.ImportID, first)
	if e != nil {
		t.Fatal(e)
	}
	last := choose(h, out, out.Conflicts[0], "keep_local", nil)
	seedBaby(t, h, "10000000-0000-4000-8000-000000000011", "Élie")
	if _, e = h.Importer.ResolveConflict(t.Context(), h.Owner, r.ImportID, last); e == nil || e.Error() != "import_resolution_stale" {
		t.Fatalf("stale prior choice %v", e)
	}
	out, e = h.Importer.Status(t.Context(), h.Caregiver, r.ImportID)
	if e != nil || out.MappingComplete || len(out.Conflicts) != 2 || h.Count(t, "import_baby_mappings", h.FamilyID) != 0 {
		t.Fatalf("prior choice survived graph change %+v %v", out, e)
	}
}

// Publication's record conflict check reads the import's baby mappings in
// one statement, however many records the manifest holds: a 30-record
// import's publishing choice sends exactly the statements a 3-record
// import's does (it once sent one mapping lookup per record, under the
// family lock).
func TestPublicationRecordCheckReadsMappingsOnce(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h, "10000000-0000-4000-8000-000000000010", "Élie")
	seedBaby(t, h, "10000000-0000-4000-8000-000000000011", "Élie")
	pool, wire := testkit.CountedPool(t, h.Runtime)
	counted := h.ImporterOn(h.AuthorityOn(t, pool))
	shared := "10000000-0000-4000-8000-000000000010"
	statements := map[int][]string{}
	for i, records := range []int{3, 30} {
		r := testkit.ImportRequest(i, "Élie", "2026-01-12")
		for n := range records {
			r.Records = append(r.Records, protocol.RecordManifest{RecordID: fmt.Sprintf("3%07d-0000-4000-8000-%012d", i, n+1), EntityKind: "feed",
				LocalBabyID: r.Babies[0].LocalBabyID, ContentSHA256: strings.Repeat("ab", 32)})
		}
		r.TotalRecordCount = uint32(records)
		out, e := h.Importer.Resolve(t.Context(), h.Caregiver, r)
		if e != nil || out.MappingComplete || len(out.Conflicts) != 1 {
			t.Fatalf("%d records: %+v %v", records, out, e)
		}
		wire.Reset()
		out, e = counted.ResolveConflict(t.Context(), h.Owner, r.ImportID, choose(h, out, out.Conflicts[0], "use_existing", &shared))
		if e != nil || !out.MappingComplete {
			t.Fatalf("%d records: publication %+v %v", records, out, e)
		}
		statements[records] = wire.Statements()
	}
	reads := map[int]int{}
	for records, sent := range statements {
		for _, sql := range sent {
			if strings.Contains(sql, "FROM import_baby_mappings") {
				reads[records]++
			}
		}
	}
	if len(statements[30]) != len(statements[3]) || reads[30] != reads[3] {
		t.Fatalf("publication sent %d statements (%d mapping reads) for 3 records, %d (%d) for 30", len(statements[3]), reads[3],
			len(statements[30]), reads[30])
	}
}

// Import conflict pages resume after the last served conflict id by the
// uuid, so each page must also be read in uuid order, whatever the
// database's collation. Under ICU's numeric collation the text of these
// conflict ids sorts otherwise; a page read in text order served conflicts
// past ones it had not served yet, and the next page, resuming after the
// last uuid, skipped them for good.
func TestImportConflictPagesFollowUUIDOrderUnderANumericCollation(t *testing.T) {
	h := testkit.NewWithICUCollation(t, "und-u-kn-true")
	seedBaby(t, h, "10000000-0000-4000-8000-000000000010", "Élie")
	seedBaby(t, h, "10000000-0000-4000-8000-000000000011", "Élie")
	r := testkit.ImportRequest(0, "Élie", "2026-01-12")
	for i := 2; i <= 24; i++ {
		b := r.Babies[0]
		b.LocalBabyID = fmt.Sprintf("20000000-0000-4000-8000-%012d", i)
		r.Babies = append(r.Babies, b)
	}
	h.Limits.SyncPageSize = 3
	h.RebuildSyncService(t, h.Limits)
	out, e := h.Importer.Resolve(t.Context(), h.Caregiver, r)
	if e != nil || out.ConflictCount != uint64(len(r.Babies)) || out.ConflictsComplete {
		t.Fatalf("first page %+v %v", out, e)
	}
	// Not vacuous: this database sorts these conflict ids' text otherwise.
	var textOrderDiffers bool
	tx, e := h.Migrator.Begin(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(t.Context())
	if e = tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: h.Caregiver.AccountID, FamilyID: h.FamilyID}); e != nil {
		t.Fatal(e)
	}
	if e = tx.QueryRow(t.Context(), `SELECT array_agg(conflict_id::text ORDER BY conflict_id::text) <> array_agg(conflict_id::text ORDER BY conflict_id)
		FROM import_conflicts WHERE family_id = $1 AND import_id = $2`, h.FamilyID, r.ImportID).Scan(&textOrderDiffers); e != nil {
		t.Fatal(e)
	}
	if !textOrderDiffers {
		t.Fatal("the numeric collation sorts these conflict ids' text in uuid order: the test proves nothing")
	}
	var served []string
	for pages := 0; ; pages++ {
		if pages > len(r.Babies) {
			t.Fatal("conflict paging does not end")
		}
		for _, c := range out.Conflicts {
			served = append(served, c.ConflictID)
		}
		if out.ConflictsComplete {
			break
		}
		if out, e = h.Importer.StatusPage(t.Context(), h.Caregiver, r.ImportID, out.NextConflictPageToken); e != nil {
			t.Fatal(e)
		}
	}
	distinct := map[string]bool{}
	for _, id := range served {
		distinct[id] = true
	}
	if len(served) != len(r.Babies) || len(distinct) != len(r.Babies) || !slices.IsSorted(served) {
		t.Fatalf("pages served %d conflicts (%d distinct) of %d, in uuid order %v: %v", len(served), len(distinct), len(r.Babies), slices.IsSorted(served), served)
	}
}
