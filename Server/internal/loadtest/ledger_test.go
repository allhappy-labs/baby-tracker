package loadtest

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLedgerKeepsLatestVersionAndIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := CreateLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	l.Append(LedgerEntry{FamilyID: "f", EntityID: "e", Kind: "feed", Version: 1, WireSHA256: "a"})
	l.Append(LedgerEntry{FamilyID: "f", EntityID: "e", Kind: "feed", Version: 2, WireSHA256: "b"})
	l.Close()
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	entries, err := ReadLedger(path)
	if err != nil || len(entries) != 1 || entries[0].Version != 2 {
		t.Fatalf("%v %+v", err, entries)
	}
	if _, err := CreateLedger(path); err == nil {
		t.Fatal("an existing ledger was overwritten")
	}
}

func TestLedgerConcurrentAppendsKeepEveryEntity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := CreateLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			fam := string(rune('a' + g))
			for v := uint64(3); v >= 1; v-- { // out of order: the latest still wins
				for e := range 100 {
					if err := l.Append(LedgerEntry{FamilyID: fam, EntityID: string(rune('A'+e%26)) + string(rune('0'+e/26)), Kind: "sleep", Version: v, Deleted: v == 3 && e%10 == 0, WireSHA256: "h"}); err != nil {
						t.Error(err)
					}
				}
			}
		})
	}
	wg.Wait()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(LedgerEntry{FamilyID: "x", EntityID: "y", Version: 1}); err == nil {
		t.Fatal("append after close accepted")
	}
	entries, err := ReadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 800 {
		t.Fatalf("%d entities, want 800", len(entries))
	}
	deleted := 0
	for _, e := range entries {
		if e.Version != 3 {
			t.Fatalf("%+v is not the latest version", e)
		}
		if e.Deleted {
			deleted++
		}
	}
	if deleted != 80 {
		t.Fatalf("%d tombstones, want 80", deleted)
	}
	for i := 1; i < len(entries); i++ {
		a, b := entries[i-1], entries[i]
		if a.FamilyID > b.FamilyID || (a.FamilyID == b.FamilyID && a.EntityID >= b.EntityID) {
			t.Fatalf("entries not in (family, entity) order at %d", i)
		}
	}
}

// Two acknowledgements of one version that disagree on the wire mean the
// server gave the same version two contents: the audit must not pick one.
func TestReadLedgerRejectsConflictingVersions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := CreateLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	same := LedgerEntry{FamilyID: "f", EntityID: "e", Kind: "feed", Version: 2, WireSHA256: "a"}
	l.Append(same)
	l.Append(same) // a retried acknowledgement is harmless
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if entries, err := ReadLedger(path); err != nil || len(entries) != 1 {
		t.Fatalf("identical duplicate: %v %+v", err, entries)
	}

	path2 := filepath.Join(t.TempDir(), "ledger.jsonl")
	l2, err := CreateLedger(path2)
	if err != nil {
		t.Fatal(err)
	}
	l2.Append(same)
	other := same
	other.WireSHA256 = "b"
	l2.Append(other)
	if err := l2.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLedger(path2); err == nil {
		t.Fatal("two contents for one version were accepted")
	}

	// Concurrent appends can land out of order: A(v), C(v+1), B(v). B must
	// still be checked against A, not only against the latest entry C.
	path3 := filepath.Join(t.TempDir(), "ledger.jsonl")
	l3, err := CreateLedger(path3)
	if err != nil {
		t.Fatal(err)
	}
	later := same
	later.Version, later.WireSHA256 = 3, "c"
	l3.Append(same)
	l3.Append(later)
	l3.Append(other)
	if err := l3.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLedger(path3); err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("A(v), C(v+1), B(v) was accepted: %v", err)
	}
}

// ReadLedgerCounted counts every acknowledgement, retries included, beside
// the one entry per entity it returns.
func TestReadLedgerCountedCountsEveryAcknowledgement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := CreateLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	a := LedgerEntry{FamilyID: "f", EntityID: "e", Kind: "feed", Version: 1, WireSHA256: "a"}
	b := a
	b.Version, b.WireSHA256 = 2, "b"
	c := LedgerEntry{FamilyID: "f", EntityID: "g", Kind: "diaper", Version: 1, WireSHA256: "c"}
	for _, e := range []LedgerEntry{b, a, a, c} {
		l.Append(e)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	entries, lines, err := ReadLedgerCounted(path)
	if err != nil || lines != 4 || len(entries) != 2 || entries[0] != b || entries[1] != c {
		t.Fatalf("entries %+v lines %d err %v", entries, lines, err)
	}
}

func TestReadLedgerRejectsCorruptLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	if err := os.WriteFile(path, []byte(`{"family_id":"f","entity_id":"e","kind":"feed","version":1,"wire_sha256":"a"}`+"\n{not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLedger(path); err == nil {
		t.Fatal("a corrupt ledger was read")
	}
	if _, err := ReadLedger(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("a missing ledger was read")
	}
}
