package sync_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"babytracker/server/internal/access"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/protocol"
	familysync "babytracker/server/internal/sync"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
)

var errWarmUp = errors.New("warm-up: roll back")

// TestSyncRequestRoundTrips pins each request shape's round trips, the
// guard (BEGIN and the authority read) and COMMIT included: a mutation
// batch whose pull fits one page is 4 at any size and 5 when its pull has
// more, a first snapshot page with more 4, a snapshot continuation with
// more 4. A pull page is read with the COMMIT: 2 round trips, first page
// or last continuation; a pull page with more stores its binding in a
// second transaction (BEGIN with the authority read, then the binding with
// the COMMIT): 4, replayed or not.
func TestSyncRequestRoundTrips(t *testing.T) {
	h := testkit.New(t)
	pool, wire := testkit.CountedPool(t, h.Runtime)
	guard := h.AuthorityOn(t, pool)
	service := testkit.SyncServiceOn(t, h, guard)
	measure := func(label string, run func(pgx.Tx, access.Grant) error) int {
		t.Helper()
		warm := guard.WithFamilyTx(t.Context(), h.Owner, "read", func(tx pgx.Tx, g access.Grant) error {
			if err := run(tx, g); err != nil {
				return err
			}
			return errWarmUp
		})
		if !errors.Is(warm, errWarmUp) {
			t.Fatalf("%s warm-up: %v", label, warm)
		}
		wire.Reset()
		if err := guard.WithFamilyTx(t.Context(), h.Owner, "read", run); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		return wire.Trips()
	}
	// exchange runs the service's own exchange (ServeSync's: a pull goes
	// with the COMMIT and is settled after it).
	exchange := func(request protocol.SyncRequest, check func(familysync.Exchanged)) func(pgx.Tx, access.Grant) error {
		return func(tx pgx.Tx, g access.Grant) error {
			out, err := familysync.ExchangeAtCommit(t.Context(), service, tx, h.Owner, g, request)
			if err != nil || out.Fatal != nil {
				return fmt.Errorf("exchange: %v %v", err, out.Fatal)
			}
			if check != nil {
				check(out)
			}
			return nil
		}
	}
	// committed runs one request in a request transaction and settles it,
	// counting the round trips of both. A pull's statements are sent only
	// by a commit, so a committed request warms them (a rolled-back
	// warm-up sends none).
	committed := func(label string, request protocol.SyncRequest) (int, familysync.Exchanged) {
		t.Helper()
		var out familysync.Exchanged
		wire.Reset()
		if err := guard.WithFamilyTx(t.Context(), h.Owner, "read", exchange(request, func(o familysync.Exchanged) { out = o })); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if err := familysync.Settle(t.Context(), service, h.Owner, &out); err != nil {
			t.Fatalf("%s: settle: %v", label, err)
		}
		return wire.Trips(), out
	}
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	request := func(mutations []protocol.Mutation) protocol.SyncRequest {
		r := h.SyncRequest(h.Cursor(h.FamilySequence(t)), nil)
		r.Mutations = mutations
		return r
	}
	for i, size := range []int{1, 20, 100} {
		baby := fmt.Sprintf("10000000-0000-4000-8000-%012d", 900+i)
		caretest.Seed(t, h, caretest.Baby(baby))
		batch := caretest.MixedBatch(baby, size)
		got := measure(fmt.Sprintf("batch of %d", size), exchange(request(batch), func(out familysync.Exchanged) {
			if len(out.Response.Outcomes) != size || out.Response.NextPageToken != nil || out.Publish == nil {
				t.Fatalf("batch of %d: %d outcomes, token %v, publish %v", size, len(out.Response.Outcomes), out.Response.NextPageToken, out.Publish)
			}
		}))
		if got != 4 {
			t.Fatalf("batch of %d with a one-page pull: %d round trips, want 4: %q", size, got, wire.Statements())
		}
	}
	// A pull: BEGIN with the authority read, then the read-only check, the
	// pull and the COMMIT.
	committed("warm-up pull", h.SyncRequest(h.Cursor(0), nil))
	if got, out := committed("pull-only", h.SyncRequest(h.Cursor(0), nil)); got != 2 || len(out.Response.EncodedChanges) == 0 ||
		out.Response.NextPageToken != nil || !endsWith(wire.Statements(), "SELECT 1 / (pg_catalog.pg_current_xact_id_if_assigned()", "WITH hw AS", "commit") {
		t.Fatalf("pull-only: %d round trips, %d changes, want 2 ending check, pull, commit: %q", got, len(out.Response.EncodedChanges), wire.Statements())
	}

	// Pages of 2 over the family's rows.
	h.Limits.SyncPageSize = 2
	h.RebuildSyncService(t, h.Limits)
	service = testkit.SyncServiceOn(t, h, guard)
	var first protocol.SnapshotResponse
	snapshot := func(token *string, keep *protocol.SnapshotResponse) func(pgx.Tx, access.Grant) error {
		return func(tx pgx.Tx, g access.Grant) error {
			response, err := service.SnapshotPage(t.Context(), tx, h.Owner, g, protocol.SnapshotRequest{PageToken: token})
			if keep != nil {
				*keep = response
			}
			return err
		}
	}
	if got := measure("first snapshot page", snapshot(nil, &first)); got != 4 || first.NextPageToken == nil {
		t.Fatalf("first snapshot page with more: %d round trips (token %v), want 4: %q", got, first.NextPageToken, wire.Statements())
	}
	// The capture reads the family state (its recorded high water) before
	// the first page: a row written between the two is then on the page at
	// a sequence above the high water and the next pull re-delivers it.
	// The other order would leave it on the page at its old version below
	// the high water, never re-delivered.
	stateAt, pageAt := -1, -1
	for i, statement := range wire.Statements() {
		switch {
		case strings.HasPrefix(statement, "SELECT next_sequence, care_record_count") && !strings.Contains(statement, "FOR NO KEY UPDATE"):
			stateAt = i
		case strings.HasPrefix(statement, "SELECT kind, id::text, wire FROM care_entities"):
			pageAt = i
		}
	}
	if stateAt < 0 || pageAt < 0 || stateAt > pageAt {
		t.Fatalf("snapshot capture reads the family state at %d and the page at %d: %q", stateAt, pageAt, wire.Statements())
	}
	var page protocol.SnapshotResponse
	if got := measure("snapshot continuation", snapshot(first.NextPageToken, &page)); got != 4 || page.NextPageToken == nil {
		t.Fatalf("snapshot continuation with more: %d round trips, want 4: %q", got, wire.Statements())
	}

	// A pull page is read with the COMMIT and writes nothing in that
	// transaction; a page with more stores its binding in a second one,
	// authorized again, with that COMMIT: 4 round trips, and 2 for a page
	// without more.
	if _, warm := committed("warm-up first page", h.SyncRequest(h.Cursor(0), nil)); warm.Response.NextPageToken == nil {
		t.Fatal("warm-up first page: no token")
	} else if _, warm = committed("warm-up continuation", h.SyncRequest(nil, warm.Response.NextPageToken)); warm.Response.NextPageToken == nil {
		t.Fatal("warm-up continuation: no token")
	}
	got, pull := committed("first pull page", h.SyncRequest(h.Cursor(0), nil))
	if got != 4 || pull.Response.NextPageToken == nil || statementsWith(wire.Statements(), "BEGIN ISOLATION LEVEL") != 2 ||
		!endsWith(wire.Statements(), "WITH hw AS", "commit", "BEGIN", "SELECT matches", "SELECT 1 / (pg_catalog.pg_current_xact_id_if_assigned()",
			"INSERT INTO sync_page_bindings", "commit") {
		t.Fatalf("first pull page with more: %d round trips (token %v), want 4 ending pull, commit, then the binding with a commit: %q", got,
			pull.Response.NextPageToken, wire.Statements())
	}
	token := pull.Response.NextPageToken
	for continued := 0; ; continued++ {
		got, next := committed("pull continuation", h.SyncRequest(nil, token))
		if next.Response.NextPageToken == nil {
			if got != 2 || continued == 0 || statementsWith(wire.Statements(), "sync_page_bindings(") != 0 ||
				!endsWith(wire.Statements(), "SELECT 1 / (pg_catalog.pg_current_xact_id_if_assigned()", "SELECT b.kind::text", "commit") {
				t.Fatalf("last pull continuation (after %d with more): %d round trips, want 2 and no binding: %q", continued, got, wire.Statements())
			}
			break
		}
		// The continuation's binding is read back after its INSERT, in the
		// second commit's round trip: a replay must find the binding it
		// stored.
		if got != 4 || !endsWith(wire.Statements(), "INSERT INTO sync_page_bindings", "SELECT b.kind::text", "commit") {
			t.Fatalf("pull continuation with more: %d round trips, want 4 ending binding, read-back, commit: %q", got, wire.Statements())
		}
		// Replayed (a lost response), it finds that binding, in the same 4.
		got, replay := committed("replayed pull continuation", h.SyncRequest(nil, token))
		if got != 4 || replay.Response.NextPageToken == nil || *replay.Response.NextPageToken != *next.Response.NextPageToken {
			t.Fatalf("replayed pull continuation: %d round trips, token %v want %v: %q", got, replay.Response.NextPageToken,
				*next.Response.NextPageToken, wire.Statements())
		}
		token = next.Response.NextPageToken
	}

	// A write stores its binding before the COMMIT (the commit must not
	// precede a check that rolls the write back): a batch whose pull has
	// more is 5 round trips.
	baby := "10000000-0000-4000-8000-000000000990"
	caretest.Seed(t, h, caretest.Baby(baby))
	var write familysync.Exchanged
	behind := h.SyncRequest(h.Cursor(0), nil)
	behind.Mutations = caretest.MixedBatch(baby, 3)
	got = measure("batch with more", exchange(behind, func(out familysync.Exchanged) { write = out }))
	if got != 5 || write.Response.NextPageToken == nil || !endsWith(wire.Statements(), "INSERT INTO sync_page_bindings", "commit") {
		t.Fatalf("batch with a pull with more: %d round trips (token %v), want 5: %q", got, write.Response.NextPageToken, wire.Statements())
	}
}

// endsWith reports whether statements end with ones starting with prefixes,
// in order.
func endsWith(statements []string, prefixes ...string) bool {
	if len(statements) < len(prefixes) {
		return false
	}
	tail := statements[len(statements)-len(prefixes):]
	for i, prefix := range prefixes {
		if !strings.HasPrefix(strings.TrimSpace(tail[i]), prefix) {
			return false
		}
	}
	return true
}

func statementsWith(statements []string, fragment string) int {
	n := 0
	for _, s := range statements {
		if strings.Contains(s, fragment) {
			n++
		}
	}
	return n
}

// TestImportTaggedMutationRoundTrips measures what import admission adds to
// a batch: each import-tagged record reads its import session and its
// manifest row (two statements, one round trip each) inside the batch's
// Fetch-to-write window; the care entities it judges come from the batch's
// working set, never from another read.
func TestImportTaggedMutationRoundTrips(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	records := make([]protocol.Mutation, 0, 4)
	manifests := make([]protocol.RecordManifest, 0, 4)
	importID := "0c000000-0000-4000-8000-0000000000aa"
	for n := range 4 {
		m := caretest.Record("diaper", caretest.BabyA, 500+n)
		m.ImportID = &importID
		hash, err := protocol.CanonicalContentHash(m.EntityKind, m.Payload)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, m)
		manifests = append(manifests, protocol.RecordManifest{RecordID: m.EntityID, EntityKind: m.EntityKind, LocalBabyID: caretest.BabyA,
			ContentSHA256: fmt.Sprintf("%x", hash[:])})
	}
	resolve := protocol.ImportResolveRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1",
		Capabilities: []string{protocol.CapabilityRecordsV1, protocol.CapabilityImportResolverV1}},
		ImportID: importID, ImportMode: "preserve_local_ids", NormalizationVersion: "nfc-trim-fold-nfc-v1",
		Babies:  []protocol.ImportBaby{{LocalBabyID: caretest.BabyA, Name: "Baby a1", BirthDate: "2026-01-12"}},
		Records: manifests, TotalRecordCount: uint32(len(manifests)), Finalize: true}
	if out, err := h.Importer.Resolve(t.Context(), h.Owner, resolve); err != nil || !out.MappingComplete {
		t.Fatalf("resolve %+v %v", out, err)
	}
	pool, wire := testkit.CountedPool(t, h.Runtime)
	guard := h.AuthorityOn(t, pool)
	service := testkit.SyncServiceOn(t, h, guard)
	for _, batch := range [][]protocol.Mutation{records[:1], records[1:4]} {
		request := h.SyncRequest(h.Cursor(h.FamilySequence(t)), nil)
		request.Mutations = batch
		run := func(tx pgx.Tx, g access.Grant) error {
			out, err := service.Exchange(t.Context(), tx, h.Owner, g, request)
			if err != nil || out.Fatal != nil || len(out.Response.Outcomes) != len(batch) {
				return fmt.Errorf("exchange: %v %v %+v", err, out.Fatal, out.Response.Outcomes)
			}
			for _, o := range out.Response.Outcomes {
				if o.Status != "applied" {
					return fmt.Errorf("import outcome %+v", o)
				}
			}
			return nil
		}
		if err := guard.WithFamilyTx(t.Context(), h.Owner, "read", func(tx pgx.Tx, g access.Grant) error {
			if err := run(tx, g); err != nil {
				return err
			}
			return errWarmUp
		}); !errors.Is(err, errWarmUp) {
			t.Fatal(err)
		}
		wire.Reset()
		if err := guard.WithFamilyTx(t.Context(), h.Owner, "read", run); err != nil {
			t.Fatal(err)
		}
		trips := wire.Trips()
		t.Logf("import-tagged batch of %d: %d round trips (4 + %d per import-tagged mutation)", len(batch), trips, (trips-4)/len(batch))
		if trips != 4+2*len(batch) {
			t.Fatalf("import-tagged batch of %d: %d round trips, want %d: %q", len(batch), trips, 4+2*len(batch), wire.Statements())
		}
	}
}
