package export

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"

	"babytracker/server/internal/access"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
)

// archiveOf is the owner's archive read through source in tx, a read-only
// transaction of isolation as Worker.readArchive opens it, encoded by
// EncodeArchive.
func archiveOf(t *testing.T, h *testkit.Harness, isolation pgx.TxIsoLevel, source careSource) ([]byte, error) {
	t.Helper()
	tx, err := h.Runtime.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: isolation, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(t.Context()))
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: h.Owner.AccountID, FamilyID: h.FamilyID}); err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err := tx.QueryRow(t.Context(), `SELECT audit_seq FROM public.families WHERE id=$1`, h.FamilyID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	grant := access.Grant{AccountID: h.Owner.AccountID, FamilyID: h.FamilyID, Role: "owner", Revision: uint64(revision)}
	archive, err := collectArchive(t.Context(), tx, grant, h.Info, h.Now, h.Authority.FamilyAuditPage, source)
	if err != nil {
		return nil, err
	}
	var encoded bytes.Buffer
	if _, err := EncodeArchive(t.Context(), &encoded, archive); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes(), nil
}

func mustArchive(t *testing.T, h *testkit.Harness, source careSource) goldenExport {
	t.Helper()
	raw, err := archiveOf(t, h, pgx.RepeatableRead, source)
	if err != nil {
		t.Fatal(err)
	}
	return splitArchive(t, h.Normalize(raw))
}

// sameBytes fails unless care's bytes are legacy's.
func sameBytes(t *testing.T, label string, legacy, care []byte) {
	t.Helper()
	if bytes.Equal(legacy, care) {
		return
	}
	i := 0
	for i < len(legacy) && i < len(care) && legacy[i] == care[i] {
		i++
	}
	excerpt := func(b []byte) string { return string(b[max(0, i-160):min(len(b), i+160)]) }
	t.Fatalf("%s: care differs from legacy at byte %d\nlegacy …%s…\n  care …%s…", label, i, excerpt(legacy), excerpt(care))
}

// journalItems is a journal section's changes.
func journalItems(t *testing.T, section json.RawMessage) []json.RawMessage {
	t.Helper()
	var changes []json.RawMessage
	if err := json.Unmarshal(section, &changes); err != nil {
		t.Fatal(err)
	}
	return changes
}

// sameJournal requires care's journal to be the latest change of each
// entity of legacy's, in order, byte for byte.
func sameJournal(t *testing.T, legacy, care json.RawMessage) {
	t.Helper()
	want := testkit.LatestPerEntity(t, journalItems(t, legacy))
	got := journalItems(t, care)
	if len(got) != len(want) {
		t.Fatalf("care journal has %d changes, legacy has %d entities", len(got), len(want))
	}
	for i := range want {
		sameBytes(t, fmt.Sprintf("journal change %d", i), want[i], got[i])
	}
}

// TestArchiveRendersMillisecondTimers exports a family whose running
// timers' stored parts and bottle start carry millisecond times (Phase 2
// Task 7's legacy parity case, kept care-only): the archive's active timers
// carry them normalized, as the legacy timer tables rendered them.
func TestArchiveRendersMillisecondTimers(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.MillisecondFamily()...)
	archive := mustArchive(t, h, careEntities{})
	if !bytes.Contains(archive.Sections["active_timers"], []byte(`"current_bottle_started_at":"2026-09-21T11:00:00.456000Z"`)) ||
		!bytes.Contains(archive.Sections["active_timers"], []byte(`"ended_at":"2026-09-21T11:20:00.789000Z"`)) {
		t.Fatalf("running timers are not rendered with normalized times: %s", archive.Sections["active_timers"])
	}
}

// TestExportMatchesLegacyCorpus runs the golden scenario through the
// care-backed services (the import step through the real importer) and
// exports it through the default source: every section of the recorded
// legacy archive (testdata/golden/legacy-export.json, never rewritten) is
// reproduced byte for byte except the journal, the one intended format
// change (Phase 2 export ruling): it is the latest change of each entity of
// the recorded journal, at its recorded sequence (LatestPerEntity), what a
// pull from cursor 0 serves.
func TestExportMatchesLegacyCorpus(t *testing.T) {
	h := testkit.NewAtFixtureClock(t)
	h.RunGolden(t)
	raw, err := os.ReadFile("testdata/golden/legacy-export.json")
	if err != nil {
		t.Fatal(err)
	}
	var recorded goldenExport
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatal(err)
	}
	got := mustArchive(t, h, careEntities{})
	// The recorded archive predates immunizations and pumps: they are the two
	// sections the care archive adds, they sit after measurements in that
	// order, and the golden scenario has neither, so both are empty.
	added := []string{"immunizations", "pumps"}
	var order []string
	for _, name := range got.SectionOrder {
		if slices.Contains(added, name) {
			if string(got.Sections[name]) != "[]" {
				t.Fatalf("the golden scenario has no %s: %s", name, got.Sections[name])
			}
			continue
		}
		order = append(order, name)
	}
	if fmt.Sprint(recorded.SectionOrder) != fmt.Sprint(order) {
		t.Fatalf("section order: recorded %v, care %v", recorded.SectionOrder, order)
	}
	if i := slices.Index(got.SectionOrder, "immunizations"); i < 1 || got.SectionOrder[i-1] != "measurements" || got.SectionOrder[i+1] != "pumps" {
		t.Fatalf("immunizations and pumps are not after measurements: %v", got.SectionOrder)
	}
	for _, name := range recorded.SectionOrder {
		var want bytes.Buffer
		if err := json.Compact(&want, recorded.Sections[name]); err != nil {
			t.Fatal(err)
		}
		if name == "journal" {
			sameJournal(t, want.Bytes(), got.Sections[name])
			continue
		}
		sameBytes(t, name, want.Bytes(), got.Sections[name])
	}
}

// interleaved is careEntities with a write committed on another connection
// while the archive is between two pages of each read: after the first
// entity is visited, and after the first change is visited. Stream and
// StreamChanges read the next page only after the visits of the current
// one, so the next page is read after the write committed.
type interleaved struct {
	careEntities
	t                             *testing.T
	h                             *testkit.Harness
	duringEntities, duringJournal []protocol.Mutation
	once                          [2]sync.Once
}

func (s *interleaved) write(i int, batch []protocol.Mutation) {
	s.once[i].Do(func() {
		if result := caretest.ApplyCommitted(s.t, s.h, s.h.Owner, batch); result.Fatal != nil || len(result.Outcomes) != len(batch) {
			s.t.Errorf("interleaved write: %v", result.Fatal)
		}
	})
}

func (s *interleaved) entities(ctx context.Context, tx pgx.Tx, familyID string, visit func(protocol.CanonicalEntity) error) error {
	return s.careEntities.entities(ctx, tx, familyID, func(e protocol.CanonicalEntity) error {
		s.write(0, s.duringEntities)
		return visit(e)
	})
}

func (s *interleaved) journal(ctx context.Context, tx pgx.Tx, familyID string, through uint64, identity protocol.ServerInfo,
	visit func(protocol.JournalChange) error) error {
	return s.careEntities.journal(ctx, tx, familyID, through, identity, func(j protocol.JournalChange) error {
		s.write(1, s.duringJournal)
		return visit(j)
	})
}

// TestCareArchiveIsOneSnapshot pins the archive's consistency over paged
// reads (care.Stream and care.StreamChanges read 1000 rows per statement).
// Writes committed while the archive is between pages — updating and
// deleting entities it has not reached yet, creating one, and moving
// entities' changes past the high water it read — leave it byte-identical
// to the archive read before them, because the archive is one repeatable-
// read snapshot; careEntities refuses a read-committed transaction, where
// each page would see the writes.
func TestCareArchiveIsOneSnapshot(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	var feeds []protocol.Mutation
	for i := 1; i <= 1200; i++ {
		feeds = append(feeds, caretest.Feed(caretest.BabyA, i))
	}
	for from := 0; from < len(feeds); from += 400 {
		caretest.Seed(t, h, feeds[from:from+400]...)
	}
	before := mustArchive(t, h, careEntities{})

	// Feeds are paged in id order (after the baby) and changes in sequence
	// order; feeds 1100.. are on the second page of both.
	edit := func(n int, mutationID string) protocol.Mutation {
		return caretest.Update(feeds[n-1], mutationID, 1, caretest.WithNote("edited "+mutationID[:8]))
	}
	source := &interleaved{careEntities: careEntities{}, t: t, h: h,
		duringEntities: []protocol.Mutation{edit(1150, "0e000000-0000-4000-8000-000000001150"),
			caretest.Delete(feeds[1159], "0d000000-0000-4000-8000-000000001160", 1), caretest.Feed(caretest.BabyA, 1201)},
		duringJournal: []protocol.Mutation{edit(1170, "0e000000-0000-4000-8000-000000001170"), edit(3, "0e000000-0000-4000-8000-000000000003")}}
	raw, err := archiveOf(t, h, pgx.RepeatableRead, source)
	if err != nil {
		t.Fatal(err)
	}
	during := splitArchive(t, h.Normalize(raw))
	after := mustArchive(t, h, careEntities{})
	for _, name := range before.SectionOrder {
		sameBytes(t, name, before.Sections[name], during.Sections[name])
	}
	// The writes did commit in between: a later archive has them.
	if bytes.Equal(before.Sections["feeds"], after.Sections["feeds"]) || bytes.Equal(before.Sections["journal"], after.Sections["journal"]) ||
		bytes.Equal(before.Sections["high_water"], after.Sections["high_water"]) {
		t.Fatal("the interleaved writes are not visible after the archive")
	}

	if _, err := archiveOf(t, h, pgx.ReadCommitted, careEntities{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("read-committed care archive: %v", err)
	}
}
