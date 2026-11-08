package importer_test

import (
	"fmt"
	"strings"
	"testing"

	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

// A resolve's round trips do not grow with its records (C14): a chunk's
// records are checked in one statement and their manifests written in one,
// with one charge for the chunk's tail, and a published import's mappings
// are read once. A finalizing chunk of 1,000 records costs what one of 10
// costs.
func TestResolveRoundTripsDoNotGrowWithRecords(t *testing.T) {
	h := testkit.New(t)
	pool, wire := testkit.CountedPool(t, h.Runtime)
	counted := h.ImporterOn(h.AuthorityOn(t, pool))
	request := func(i, records int) protocol.ImportResolveRequest {
		r := testkit.ImportRequest(i, fmt.Sprintf("Baby %d", i), "2026-01-12")
		for n := range records {
			r.Records = append(r.Records, protocol.RecordManifest{RecordID: fmt.Sprintf("4%07d-0000-4000-8000-%012d", i, n+1), EntityKind: "feed",
				LocalBabyID: r.Babies[0].LocalBabyID, ContentSHA256: strings.Repeat("ab", 32)})
		}
		r.TotalRecordCount = uint32(records)
		return r
	}
	// The first resolve prepares the connection's statements.
	if out, e := counted.Resolve(t.Context(), h.Owner, request(0, 10)); e != nil || !out.MappingComplete {
		t.Fatalf("warm: %+v %v", out, e)
	}
	type measured struct{ trips, statements, manifestWrites, charges, mappingReads int }
	got := map[int]measured{}
	for i, records := range []int{10, 1000} {
		wire.Reset()
		out, e := counted.Resolve(t.Context(), h.Owner, request(i+1, records))
		if e != nil || !out.MappingComplete || len(out.Mappings) != 1 || len(out.Conflicts) != 0 {
			t.Fatalf("%d records: %+v %v", records, out, e)
		}
		m := measured{trips: wire.Trips()}
		for _, sql := range wire.Statements() {
			m.statements++
			switch {
			case strings.Contains(sql, "INSERT INTO import_record_manifests"):
				m.manifestWrites++
			case strings.Contains(sql, "care_charged_bytes"):
				m.charges++
			case strings.Contains(sql, "FROM import_baby_mappings"):
				m.mappingReads++
			}
		}
		t.Logf("%d records: %+v", records, m)
		got[records] = m
	}
	if got[1000] != got[10] {
		t.Errorf("a resolve of 1,000 records cost %+v, of 10 records %+v", got[1000], got[10])
	}
	if got[10].manifestWrites != 1 || got[10].mappingReads != 1 {
		t.Errorf("a resolve wrote its manifests in %d statements and read its mappings %d times, want 1 and 1", got[10].manifestWrites, got[10].mappingReads)
	}
}
