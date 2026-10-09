package localstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"babytracker/server/internal/security"
	"golang.org/x/sys/unix"
)

func integrityFixture(t *testing.T) (Store, security.Head, security.Head) {
	t.Helper()
	store := initializedStore(t)
	first := testCandidate(t, security.Head{Generation: 1}, 1)
	if err := store.Ledger.AppendCandidate(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	head1 := security.Head{Generation: 1, Revision: 1, EventHash: first.EventHash}
	second := testCandidate(t, head1, 2)
	if err := store.Ledger.AppendCandidate(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	head2 := security.Head{Generation: 1, Revision: 2, EventHash: second.EventHash}
	return store, head1, head2
}

func TestIntegrityInventoryRejectsSameHeadChanges(t *testing.T) {
	t.Run("uncommitted sibling", func(t *testing.T) {
		store, head1, _ := integrityFixture(t)
		files, err := filepath.Glob(filepath.Join(store.ledger.dir, "generation-*", "00000000000000000002-*.json"))
		if err != nil || len(files) != 1 {
			t.Fatalf("sibling: %v %v", files, err)
		}
		if err := os.WriteFile(files[0], []byte("not JSON"), 0o600); err != nil {
			t.Fatal(err)
		}
		inventory, candidates, err := store.ledger.ReadValidatedInventory(t.Context(), head1)
		if err != nil || len(candidates) != 1 || len(inventory.Files) != 1 {
			t.Fatalf("sibling selected: %v %d", err, len(candidates))
		}
	})
	for _, mutation := range []string{"corrupt", "delete", "directory", "symlink", "oversize", "replace"} {
		t.Run(mutation, func(t *testing.T) {
			store, head1, _ := integrityFixture(t)
			inventory, candidates, err := store.ledger.ReadValidatedInventory(t.Context(), head1)
			if err != nil || len(candidates) != 1 {
				t.Fatalf("cold: %d %v", len(candidates), err)
			}
			path := filepath.Join(store.ledger.dir, "generation-00000000000000000001", inventory.Files[0].SelectedName)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "corrupt":
				original[len(original)-2] ^= 1
				err = os.WriteFile(path, original, 0o600)
			case "delete":
				err = os.Remove(path)
			case "directory":
				if err = os.Remove(path); err == nil {
					err = os.Mkdir(path, 0o700)
				}
			case "symlink":
				if err = os.Remove(path); err == nil {
					err = os.Symlink(filepath.Join(store.ledger.dir, "outside"), path)
				}
			case "oversize":
				err = os.Truncate(path, 69<<10)
			case "replace":
				original[0] ^= 1
				if err = os.Remove(path); err == nil {
					err = os.WriteFile(path, original, 0o600)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := store.ledger.VerifyInventory(t.Context(), inventory); security.Code(err) != "ledger_corrupt" {
				t.Fatalf("verify: %v", err)
			}
			if _, _, err := store.ledger.ReadValidatedExtension(t.Context(), inventory, head1); security.Code(err) != "ledger_corrupt" {
				t.Fatalf("same head: %v", err)
			}
		})
	}
}

func TestIntegrityInventoryExtensionAndCancellation(t *testing.T) {
	t.Run("invalid suffix link", func(t *testing.T) {
		store, head1, head2 := integrityFixture(t)
		inventory, _, err := store.ledger.ReadValidatedInventory(t.Context(), head1)
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(store.ledger.dir, "generation-00000000000000000001", selectedFilename(2, head2.EventHash))
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		data = bytes.Replace(data, []byte(`"revision":1`), []byte(`"revision":0`), 1)
		if err := os.WriteFile(file, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.ledger.ReadValidatedExtension(t.Context(), inventory, head2); security.Code(err) != "ledger_corrupt" {
			t.Fatalf("link: %v", err)
		}
	})
	store, head1, head2 := integrityFixture(t)
	inventory, candidates, err := store.ledger.ReadValidatedInventory(t.Context(), head1)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("cold: %d %v", len(candidates), err)
	}
	next, extension, err := store.ledger.ReadValidatedExtension(t.Context(), inventory, head2)
	if err != nil || len(next.Files) != 2 || len(extension) != 1 || next.Files[0] != inventory.Files[0] {
		t.Fatalf("extension: files=%d candidates=%d err=%v", len(next.Files), len(extension), err)
	}
	if next.Head != head2 || extension[0].Prior != head1 {
		t.Fatalf("wrong suffix: %+v %+v", next.Head, extension)
	}
	firstJSON := append([]byte(nil), candidates[0].EventJSON...)
	candidates[0].EventJSON[0] ^= 1
	_, reread, err := store.ledger.ReadValidatedInventory(t.Context(), head1)
	if err != nil || len(reread) != 1 || !bytes.Equal(reread[0].EventJSON, firstJSON) {
		t.Fatalf("candidate mutation reached reread: %v", err)
	}
	candidates[0].EventJSON = firstJSON
	firstFile := inventory.Files[0]
	next.Files[0].RawDigest[0] ^= 1
	if inventory.Files[0] != firstFile {
		t.Fatal("extension inventory aliases input slice")
	}
	next.Files[0] = firstFile
	if err := store.ledger.VerifyInventory(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	reads := 0
	store.ledger.selectedReadHook = func() { reads++ }
	same, suffix, err := store.ledger.ReadValidatedExtension(t.Context(), next, head2)
	store.ledger.selectedReadHook = nil
	if err != nil || len(suffix) != 0 || reads != 2 || same.Head != next.Head {
		t.Fatalf("same head: reads=%d suffix=%d err=%v", reads, len(suffix), err)
	}
	// A verified inventory hashes raw bytes without decoding an envelope again.
	selected := filepath.Join(store.ledger.dir, "generation-00000000000000000001", next.Files[0].SelectedName)
	if err := os.WriteFile(selected, []byte("not JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	invalidJSON := next
	invalidJSON.Files = append([]security.CommittedFile(nil), next.Files...)
	invalidJSON.Files[0].RawDigest = sha256.Sum256([]byte("not JSON"))
	if err := store.ledger.VerifyInventory(t.Context(), invalidJSON); err != nil {
		t.Fatalf("raw verification reparsed file: %v", err)
	}
	if _, _, err := store.ledger.ReadValidatedInventory(t.Context(), head2); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("cold accepted invalid envelope: %v", err)
	}
	// Restore the selected bytes for cancellation and path tests.
	first := testCandidate(t, security.Head{Generation: 1}, 1)
	encoded, err := json.Marshal(diskCandidate{first.CommandID, toDiskHead(first.Prior), json.RawMessage(first.EventJSON), hex.EncodeToString(first.EventHash[:])})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(selected, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	ctxMid, cancelMid := context.WithCancel(t.Context())
	reads = 0
	store.ledger.selectedReadHook = func() { reads++; cancelMid() }
	if err := store.ledger.VerifyInventory(ctxMid, next); !errors.Is(err, context.Canceled) || reads != 1 {
		t.Fatalf("mid read: reads=%d err=%v", reads, err)
	}
	store.ledger.selectedReadHook = nil
	// The caller cannot redirect selected paths or replace the retained raw proof.
	malicious := next
	malicious.Files = append([]security.CommittedFile(nil), next.Files...)
	malicious.Files[0].SelectedName = "../outside"
	if err := store.ledger.VerifyInventory(t.Context(), malicious); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("path: %v", err)
	}
	malicious = next
	malicious.Files = append([]security.CommittedFile(nil), next.Files...)
	malicious.Files[0].RawDigest = sha256.Sum256([]byte("wrong"))
	if err := store.ledger.VerifyInventory(t.Context(), malicious); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("digest: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := store.ledger.ReadValidatedInventory(ctx, head2); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel cold: %v", err)
	}
	if err := store.ledger.VerifyInventory(ctx, next); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel verify: %v", err)
	}
	if _, _, err := store.ledger.ReadValidatedExtension(ctx, inventory, head2); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel extension: %v", err)
	}
	artifacts, err := filepath.Glob(filepath.Join(store.ledger.dir, ".walk-*.tmp"))
	if err != nil || len(artifacts) != 0 {
		t.Fatalf("walk artifacts: %v %v", artifacts, err)
	}
}

func TestIntegrityInventoryRejectsOpenBoundarySwap(t *testing.T) {
	t.Run("generation symlink", func(t *testing.T) {
		store, head1, _ := integrityFixture(t)
		inventory, _, err := store.ledger.ReadValidatedInventory(t.Context(), head1)
		if err != nil {
			t.Fatal(err)
		}
		generation := filepath.Join(store.ledger.dir, "generation-00000000000000000001")
		retained := filepath.Join(store.ledger.dir, "retained-generation")
		if err := os.Rename(generation, retained); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("retained-generation", generation); err != nil {
			t.Fatal(err)
		}
		if err := store.ledger.VerifyInventory(t.Context(), inventory); security.Code(err) != "ledger_corrupt" {
			t.Fatalf("generation symlink: %v", err)
		}
		if err := os.Remove(generation); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(retained, generation); err != nil {
			t.Fatal(err)
		}
		if err := store.ledger.VerifyInventory(t.Context(), inventory); err != nil {
			t.Fatalf("restored generation: %v", err)
		}
	})
	for _, kind := range []string{"symlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			store, head1, _ := integrityFixture(t)
			inventory, _, err := store.ledger.ReadValidatedInventory(t.Context(), head1)
			if err != nil {
				t.Fatal(err)
			}
			selected := filepath.Join(store.ledger.dir, "generation-00000000000000000001", inventory.Files[0].SelectedName)
			retained := filepath.Join(filepath.Dir(selected), "retained.json")
			store.ledger.selectedBeforeOpenHook = func() {
				if err := os.Rename(selected, retained); err != nil {
					t.Fatal(err)
				}
				if kind == "symlink" {
					if err := os.Symlink("retained.json", selected); err != nil {
						t.Fatal(err)
					}
				} else if err := unix.Mkfifo(selected, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			defer func() { store.ledger.selectedBeforeOpenHook = nil }()
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			if err := store.ledger.VerifyInventory(ctx, inventory); security.Code(err) != "ledger_corrupt" {
				t.Fatalf("%s substitution: %v", kind, err)
			}
			store.ledger.selectedBeforeOpenHook = nil
			if err := os.Remove(selected); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(retained, selected); err != nil {
				t.Fatal(err)
			}
			if err := store.ledger.VerifyInventory(t.Context(), inventory); err != nil {
				t.Fatalf("restored fixture: %v", err)
			}
		})
	}
}

func TestIntegrityInventoryCancelsWithinReadAndClosesDescriptor(t *testing.T) {
	store, head1, _ := integrityFixture(t)
	inventory, _, err := store.ledger.ReadValidatedInventory(t.Context(), head1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var opened *os.File
	chunks := 0
	store.ledger.selectedChunkHook = func(file *os.File) { opened = file; chunks++; cancel() }
	defer func() { store.ledger.selectedChunkHook = nil }()
	if err := store.ledger.VerifyInventory(ctx, inventory); !errors.Is(err, context.Canceled) || chunks != 1 {
		t.Fatalf("within-read cancellation: chunks=%d err=%v", chunks, err)
	}
	if opened == nil {
		t.Fatal("read hook did not observe descriptor")
	}
	if _, err := opened.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("descriptor not closed: %v", err)
	}
}
