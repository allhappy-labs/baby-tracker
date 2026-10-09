package localstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/security"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMissingTrustedHeadCannotRecover(t *testing.T) {
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "ledger"), filepath.Join(root, "head"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Ledger.AppendCandidate(t.Context(), testCandidate(t, security.Head{Generation: 1}, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Heads.Load(t.Context()); security.Code(err) != "trusted_head_required" {
		t.Fatalf("code=%s", security.Code(err))
	}
}

func TestWalkCommittedCancelsBeforeLargeRevisionWork(t *testing.T) {
	store := initializedStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := store.Ledger.WalkCommitted(ctx, security.Head{Generation: 1, Revision: 9007199254740991, EventHash: [32]byte{1}}, func(security.Candidate) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}

func TestReadFileBoundedRejectsSparseOversize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sparse.json")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(1 << 30); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := readFileBounded(t.Context(), path, 4096)
	if !errors.Is(err, errFileTooLarge) || data != nil {
		t.Fatalf("bytes=%d err=%v", len(data), err)
	}
}

func TestProductionReadsRejectSparseOversizeFiles(t *testing.T) {
	t.Run("head", func(t *testing.T) {
		store := initializedStore(t)
		if err := os.Truncate(store.heads.path, 1<<30); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Heads.Load(t.Context()); security.Code(err) != "ledger_corrupt" {
			t.Fatalf("code=%s err=%v", security.Code(err), err)
		}
	})
	t.Run("candidate walk and idempotent append", func(t *testing.T) {
		store := initializedStore(t)
		candidate := testCandidate(t, security.Head{Generation: 1}, 1)
		if err := store.Ledger.AppendCandidate(t.Context(), candidate); err != nil {
			t.Fatal(err)
		}
		files, _ := filepath.Glob(filepath.Join(store.ledger.dir, "generation-*", "*.json"))
		if len(files) != 1 {
			t.Fatalf("candidate files=%d", len(files))
		}
		if err := os.Truncate(files[0], 1<<30); err != nil {
			t.Fatal(err)
		}
		head := security.Head{Generation: 1, Revision: 1, EventHash: candidate.EventHash}
		if _, err := store.Ledger.ReadCommitted(t.Context(), head); security.Code(err) != "ledger_corrupt" {
			t.Fatalf("walk code=%s err=%v", security.Code(err), err)
		}
		if err := store.Ledger.AppendCandidate(t.Context(), candidate); security.Code(err) != "ledger_corrupt" {
			t.Fatalf("append code=%s err=%v", security.Code(err), err)
		}
	})
}

func TestLedgerAppendIsExclusiveAndIdempotent(t *testing.T) {
	store := initializedStore(t)
	candidate := testCandidate(t, security.Head{Generation: 1}, 1)
	if err := store.Ledger.AppendCandidate(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	if err := store.Ledger.AppendCandidate(t.Context(), candidate); err != nil {
		t.Fatalf("idempotent append: %v", err)
	}
	changed := candidate
	changed.EventJSON = bytes.Replace(candidate.EventJSON, []byte("opaque-1"), []byte("changed-1"), 1)
	if err := store.Ledger.AppendCandidate(t.Context(), changed); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("code=%s", security.Code(err))
	}
}

func TestDiskEnvelopesRejectDuplicateKnownKeys(t *testing.T) {
	t.Run("head", func(t *testing.T) {
		store := initializedStore(t)
		data, err := os.ReadFile(store.heads.path)
		if err != nil {
			t.Fatal(err)
		}
		data = bytes.Replace(data, []byte(`"generation":1`), []byte(`"generation":1,"generation":1`), 1)
		if err := os.WriteFile(store.heads.path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Heads.Load(t.Context()); security.Code(err) != "ledger_corrupt" {
			t.Fatalf("code=%s", security.Code(err))
		}
	})
	t.Run("candidate", func(t *testing.T) {
		store := initializedStore(t)
		candidate := testCandidate(t, security.Head{Generation: 1}, 1)
		if err := store.Ledger.AppendCandidate(t.Context(), candidate); err != nil {
			t.Fatal(err)
		}
		head := security.Head{Generation: 1, Revision: 1, EventHash: candidate.EventHash}
		if swapped, err := store.Heads.CompareAndSwap(t.Context(), security.Head{Generation: 1}, head); err != nil || !swapped {
			t.Fatalf("swap=%v err=%v", swapped, err)
		}
		files, _ := filepath.Glob(filepath.Join(store.ledger.dir, "generation-*", "*.json"))
		data, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		data = bytes.Replace(data, []byte(`"command_id":`), []byte(`"command_id":"30000000-0000-4000-8000-000000000001","command_id":`), 1)
		if err := os.WriteFile(files[0], data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Ledger.ReadCommitted(t.Context(), head); security.Code(err) != "ledger_corrupt" {
			t.Fatalf("code=%s", security.Code(err))
		}
	})
}

func TestLedgerFaultMatrix(t *testing.T) {
	for _, name := range []string{"changed_bytes", "missing_middle", "valid_prefix_truncation", "head_hash_mismatch", "candidate_before_head", "head_before_projection"} {
		t.Run(name, func(t *testing.T) {
			store := initializedStore(t)
			first := testCandidate(t, security.Head{Generation: 1}, 1)
			if err := store.Ledger.AppendCandidate(t.Context(), first); err != nil {
				t.Fatal(err)
			}
			head0 := security.Head{Generation: 1}
			head1 := security.Head{Generation: 1, Revision: 1, EventHash: first.EventHash}
			if name == "candidate_before_head" {
				got, err := store.Ledger.ReadCommitted(t.Context(), head0)
				if err != nil || len(got) != 0 {
					t.Fatalf("suffix was treated as committed: %d %v", len(got), err)
				}
				return
			}
			if swapped, err := store.Heads.CompareAndSwap(t.Context(), head0, head1); err != nil || !swapped {
				t.Fatalf("commit first: swapped=%v err=%v", swapped, err)
			}
			if name == "head_before_projection" {
				projection := &countingProjection{head: head0}
				service, err := security.New(completeOptions(security.Options{Ledger: store.Ledger, Heads: store.Heads, Projection: projection, Fence: store.Fence, Now: func() time.Time { return time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC) }}))
				if err != nil {
					t.Fatal(err)
				}
				for range 2 {
					if err := service.Reconcile(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				if projection.applications != 1 || projection.head != head1 {
					t.Fatalf("applications=%d head=%+v", projection.applications, projection.head)
				}
				return
			}
			second := testCandidate(t, head1, 2)
			if err := store.Ledger.AppendCandidate(t.Context(), second); err != nil {
				t.Fatal(err)
			}
			head2 := security.Head{Generation: 1, Revision: 2, EventHash: second.EventHash}
			if swapped, err := store.Heads.CompareAndSwap(t.Context(), head1, head2); err != nil || !swapped {
				t.Fatalf("commit second: swapped=%v err=%v", swapped, err)
			}
			files, _ := filepath.Glob(filepath.Join(store.ledger.dir, "generation-*", "*.json"))
			switch name {
			case "changed_bytes":
				data, err := os.ReadFile(files[0])
				if err != nil {
					t.Fatal(err)
				}
				data = []byte(strings.Replace(string(data), "opaque-1", "changed-1", 1))
				if err := os.WriteFile(files[0], data, 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing_middle":
				if err := os.Remove(files[0]); err != nil {
					t.Fatal(err)
				}
			case "valid_prefix_truncation":
				if err := os.Remove(files[1]); err != nil {
					t.Fatal(err)
				}
			case "head_hash_mismatch":
				bad := head2
				bad.EventHash = [32]byte{1}
				if err := store.heads.write(bad); err != nil {
					t.Fatal(err)
				}
			}
			trusted, err := store.Heads.Load(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			candidates, err := store.Ledger.ReadCommitted(t.Context(), trusted)
			if err == nil {
				_, err = security.Replay(candidates)
			}
			if security.Code(err) != "ledger_corrupt" {
				t.Fatalf("code=%s err=%v", security.Code(err), err)
			}
		})
	}
}

func TestCommittedHeadSurvivesFilesystemReopenBeforeProjection(t *testing.T) {
	root := t.TempDir()
	ledgerDir, headDir := filepath.Join(root, "ledger"), filepath.Join(root, "heads")
	store, err := Open(ledgerDir, headDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InitializeEmpty(t.Context()); err != nil {
		t.Fatal(err)
	}
	prior := security.Head{Generation: 1}
	candidate := testCandidate(t, prior, 1)
	if err := store.Ledger.AppendCandidate(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	committed := security.Head{Generation: 1, Revision: 1, EventHash: candidate.EventHash}
	if swapped, err := store.Heads.CompareAndSwap(t.Context(), prior, committed); err != nil || !swapped {
		t.Fatalf("head commit swapped=%v err=%v", swapped, err)
	}

	projection := &countingProjection{head: prior}
	reopened, err := Open(ledgerDir, headDir)
	if err != nil {
		t.Fatal(err)
	}
	service, err := security.New(completeOptions(security.Options{
		Ledger: reopened.Ledger, Heads: reopened.Heads, Projection: projection, Fence: reopened.Fence,
		Now: func() time.Time { return time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC) },
	}))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := service.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if head, err := reopened.Heads.Load(t.Context()); err != nil || head != committed {
		t.Fatalf("reopened head=%+v err=%v", head, err)
	}
	candidates, err := reopened.Ledger.ReadCommitted(t.Context(), committed)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("reopened candidates=%d err=%v", len(candidates), err)
	}
	state, err := security.Replay(candidates)
	if err != nil {
		t.Fatal(err)
	}
	if account, ok := state.Account("10000000-0000-4000-8000-000000000001"); !ok || account.Status != "active" {
		t.Fatalf("reopened account=%+v present=%v", account, ok)
	}
	if receipt, ok := state.Receipt("10000000-0000-4000-8000-000000000001", candidate.CommandID); !ok || receipt.Result.Revision != 1 {
		t.Fatalf("reopened receipt=%+v present=%v", receipt, ok)
	}
	if projection.applications != 1 || projection.head != committed {
		t.Fatalf("applications=%d projection=%+v", projection.applications, projection.head)
	}
}

func completeOptions(options security.Options) security.Options {
	options.Runtime = &pgxpool.Pool{}
	options.Decide = func(context.Context, access.Principal, string, string) (access.Grant, error) {
		return access.Grant{}, security.ErrUnavailable
	}
	options.Epoch = func(context.Context) (uint64, error) { return 1, nil }
	return options
}

type countingProjection struct {
	head         security.Head
	applications int
}

func (projection *countingProjection) Head(context.Context) (security.Head, error) {
	return projection.head, nil
}
func (projection *countingProjection) Apply(_ context.Context, candidate security.Candidate) error {
	next := security.Head{Generation: candidate.Prior.Generation, Revision: candidate.Prior.Revision + 1, EventHash: candidate.EventHash}
	if projection.head == next {
		return nil
	}
	if projection.head != candidate.Prior {
		return errors.New("projection prior mismatch")
	}
	projection.head = next
	projection.applications++
	return nil
}

func initializedStore(t *testing.T) Store {
	t.Helper()
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "ledger"), filepath.Join(root, "head"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InitializeEmpty(t.Context()); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestInitializeEmptyRecoversOnlyExactInterruptedGenesisTemp(t *testing.T) {
	root := t.TempDir()
	ledgerDir := filepath.Join(root, "ledger")
	headDir := filepath.Join(root, "head")
	store, err := Open(ledgerDir, headDir)
	if err != nil {
		t.Fatal(err)
	}
	exact := filepath.Join(headDir, "head-g1-r0-0000000000000000000000000000000000000000000000000000000000000000.tmp")
	if err := os.WriteFile(exact, []byte("interrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.InitializeEmpty(t.Context()); err != nil {
		t.Fatalf("exact interrupted genesis temp was not recoverable: %v", err)
	}
	if _, err := os.Stat(exact); !os.IsNotExist(err) {
		t.Fatalf("stale exact temp still exists: %v", err)
	}

	otherRoot := t.TempDir()
	other, err := Open(filepath.Join(otherRoot, "ledger"), filepath.Join(otherRoot, "head"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other.heads.dir, "unexpected.tmp"), []byte("do not remove"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := other.InitializeEmpty(t.Context()); err == nil {
		t.Fatal("unexpected head artifact was accepted")
	}
}

func testCandidate(t *testing.T, prior security.Head, revision uint64) security.Candidate {
	t.Helper()
	commandID := "30000000-0000-4000-8000-" + map[uint64]string{1: "000000000001", 2: "000000000002"}[revision]
	event := security.Event{
		Version: "v1", Generation: 1, Revision: revision, PreviousHash: hex.EncodeToString(prior.EventHash[:]),
		CommandID: commandID, CommandHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ActorAccountID: "10000000-0000-4000-8000-000000000001", Kind: "account_registered",
		At:      time.Date(2026, 9, 20, 12, 0, int(revision), 0, time.UTC).Format(time.RFC3339Nano),
		Effects: []security.Effect{{Kind: "account_registered", AccountID: "10000000-0000-4000-8000-000000000001", IdentityKey: "opaque-" + string(rune('0'+revision)), Status: "active"}},
	}
	encoded, err := security.EncodeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	return security.Candidate{CommandID: commandID, Prior: prior, EventJSON: encoded, EventHash: sha256.Sum256(encoded)}
}
