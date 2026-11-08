package sync_test

import (
	"context"
	"errors"
	"testing"

	"babytracker/server/internal/care/caretest"
	familysync "babytracker/server/internal/sync"
	"babytracker/server/internal/testkit"
)

// A first page's lookup is random, so a binding already stored under it is
// another page's: with the lookup forced to repeat, the second first page
// finds the first one's binding (another high water), is refused
// invalid_page_token after a commit that wrote nothing, and leaves the
// bindings and the family as they were.
func TestFirstPageWhoseLookupIsTakenIsRefused(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t, h.Limits)
	h.SeedBabies(t, 3)
	familysync.SetPageLookup(h.Service, func(b []byte) (int, error) {
		for i := range b {
			b[i] = 0x5a
		}
		return len(b), nil
	})
	first, err := applyAndPull(t, h.Service, h.Owner, h.SyncRequest(h.Cursor(0), nil))
	if err != nil || first.NextPageToken == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	caretest.Seed(t, h, caretest.Baby("10000000-0000-4000-8000-000000000777"))
	bindings, sequence := h.Count(t, "sync_page_bindings", h.FamilyID), h.FamilySequence(t)
	again, err := applyAndPull(t, h.Service, h.Owner, h.SyncRequest(h.Cursor(0), nil))
	if !errors.Is(err, familysync.ErrInvalidPageToken) || again.NextPageToken != nil {
		t.Fatalf("first page onto a taken lookup: %+v err=%v, want invalid_page_token", again, err)
	}
	if h.Count(t, "sync_page_bindings", h.FamilyID) != bindings || h.FamilySequence(t) != sequence {
		t.Fatalf("the refused first page changed the bindings (%d, want %d) or the family (%d, want %d)",
			h.Count(t, "sync_page_bindings", h.FamilyID), bindings, h.FamilySequence(t), sequence)
	}
	familysync.SetPageLookup(h.Service, nil)
	if next, err := applyAndPull(t, h.Service, h.Owner, h.SyncRequest(h.Cursor(0), nil)); err != nil || next.NextPageToken == nil {
		t.Fatalf("first page with a random lookup=%+v err=%v", next, err)
	}
}

// A pull page's binding goes with the COMMIT, and a continuation's is read
// back in that round trip: a replayed continuation whose successor is
// stored with other limits than the replay derives is refused
// invalid_page_token, as when the binding was checked before the commit,
// and nothing else changes. The intact replay is accepted.
func TestReplayedContinuationMustFindItsOwnBinding(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t, h.Limits)
	h.SeedBabies(t, 3)
	first, err := applyAndPull(t, h.Service, h.Owner, h.SyncRequest(h.Cursor(0), nil))
	if err != nil || first.NextPageToken == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	page, err := applyAndPull(t, h.Service, h.Owner, h.SyncRequest(nil, first.NextPageToken))
	if err != nil || page.NextPageToken == nil {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	if replay, err := applyAndPull(t, h.Service, h.Owner, h.SyncRequest(nil, first.NextPageToken)); err != nil ||
		replay.NextPageToken == nil || *replay.NextPageToken != *page.NextPageToken {
		t.Fatalf("intact replay=%+v err=%v", replay, err)
	}
	bindings := h.Count(t, "sync_page_bindings", h.FamilyID)
	// The owner's tenant context reaches its bindings (family_context).
	tx, err := h.Migrator.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `SELECT set_config('app.family_id', $1, true), set_config('app.account_id', $2, true)`,
		h.FamilyID, h.Owner.AccountID); err != nil {
		t.Fatal(err)
	}
	tag, err := tx.Exec(t.Context(), `UPDATE sync_page_bindings SET byte_limit = byte_limit - 1
		WHERE family_id = $1 AND kind = 'sync' AND resume_sequence = (SELECT max(resume_sequence) FROM sync_page_bindings WHERE family_id = $1)`,
		h.FamilyID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("tamper the successor binding: %v (%d rows)", err, tag.RowsAffected())
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	replay, err := applyAndPull(t, h.Service, h.Owner, h.SyncRequest(nil, first.NextPageToken))
	if !errors.Is(err, familysync.ErrInvalidPageToken) || replay.NextPageToken != nil {
		t.Fatalf("replay onto another stored binding: %+v err=%v, want invalid_page_token", replay, err)
	}
	if got := h.Count(t, "sync_page_bindings", h.FamilyID); got != bindings {
		t.Fatalf("bindings %d after the refused replay, want %d", got, bindings)
	}
	// The service goes on: a first page from the cursor.
	if again, err := applyAndPull(t, h.Service, h.Owner, h.SyncRequest(h.Cursor(0), nil)); err != nil || again.NextPageToken == nil {
		t.Fatalf("pull after the refusal=%+v err=%v", again, err)
	}
}
