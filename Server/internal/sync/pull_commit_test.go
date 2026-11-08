package sync_test

import (
	"context"
	"errors"
	"testing"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	familysync "babytracker/server/internal/sync"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
)

// A pull is read with the COMMIT, so a refusal decided from its rows comes
// after a commit; that is safe only because the transaction wrote nothing,
// and the server enforces it: in a request transaction that already wrote
// or locked a row, the pull and the COMMIT never run, the transaction rolls
// back (authority.ErrPipelineAfterWrite) and its write with it.
func TestPullAfterAWriteNeverCommits(t *testing.T) {
	h := testkit.New(t)
	h.SeedBabies(t, 2)
	guard := h.AuthorityOn(t, h.Runtime)
	service := testkit.SyncServiceOn(t, h, guard)
	sequence := h.FamilySequence(t)
	for _, write := range []string{
		`UPDATE families SET next_sequence = next_sequence + 1 WHERE id = $1`,
		`SELECT 1 FROM families WHERE id = $1 FOR NO KEY UPDATE`,
	} {
		read := false
		err := guard.WithFamilyTx(t.Context(), h.Owner, "read", func(tx pgx.Tx, g access.Grant) error {
			if _, err := tx.Exec(t.Context(), write, g.FamilyID); err != nil {
				return err
			}
			out, err := familysync.ExchangeAtCommit(t.Context(), service, tx, h.Owner, g, h.SyncRequest(h.Cursor(0), nil))
			read = err == nil && out.Response.HighWater.Sequence != 0
			return err
		})
		if !errors.Is(err, authority.ErrPipelineAfterWrite) || read {
			t.Fatalf("pull after %q: %v (page served before the commit: %v), want ErrPipelineAfterWrite", write, err, read)
		}
		if got := h.FamilySequence(t); got != sequence {
			t.Fatalf("pull after %q: family at %d, want %d (the write rolled back)", write, got, sequence)
		}
	}
}

// The refusals decided from a pull's rows come after its commit, in its two
// round trips, and write nothing: a cursor ahead of the family
// (snapshot_required) and a continuation whose binding is gone
// (invalid_page_token).
func TestPullRefusalsFromItsRowsCostTwoRoundTrips(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t, h.Limits)
	h.SeedBabies(t, 3)
	pool, wire := testkit.CountedPool(t, h.Runtime)
	service := testkit.SyncServiceOn(t, h, h.AuthorityOn(t, pool))
	first, err := service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(h.Cursor(0), nil))
	if err != nil || first.NextPageToken == nil {
		t.Fatalf("first page: %+v %v", first, err)
	}
	ahead := h.SyncRequest(h.Cursor(h.FamilySequence(t)+100), nil)
	_, _ = service.ApplyAndPull(t.Context(), h.Owner, ahead) // prepares
	sequence, bindings := h.FamilySequence(t), h.Count(t, "sync_page_bindings", h.FamilyID)
	wire.Reset()
	if _, err := service.ApplyAndPull(t.Context(), h.Owner, ahead); !errors.Is(err, familysync.ErrSnapshotRequired) || wire.Trips() != 2 {
		t.Fatalf("cursor ahead: %v in %d round trips, want snapshot_required in 2: %q", err, wire.Trips(), wire.Statements())
	}
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
	if tag, err := tx.Exec(t.Context(), `DELETE FROM sync_page_bindings WHERE family_id = $1`, h.FamilyID); err != nil || tag.RowsAffected() == 0 {
		t.Fatalf("delete the bindings: %v (%d rows)", err, tag.RowsAffected())
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, _ = service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(nil, first.NextPageToken)) // prepares
	wire.Reset()
	if _, err := service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(nil, first.NextPageToken)); !errors.Is(err, familysync.ErrInvalidPageToken) ||
		wire.Trips() != 2 {
		t.Fatalf("continuation without its binding: %v in %d round trips, want invalid_page_token in 2: %q", err, wire.Trips(), wire.Statements())
	}
	if h.FamilySequence(t) != sequence || h.Count(t, "sync_page_bindings", h.FamilyID) != 0 || bindings == 0 {
		t.Fatalf("refused pulls wrote: family %d (want %d), bindings %d (want 0, from %d)", h.FamilySequence(t), sequence,
			h.Count(t, "sync_page_bindings", h.FamilyID), bindings)
	}
}

// A page with more stores its binding after the pull's commit, in a second
// request transaction that authorizes the principal again: a session
// rotated (or revoked) in between refuses the request (session_revoked)
// and stores no binding, so a token is never served without its binding
// and never bound for a session that is no longer live.
func TestPullBindingIsStoredOnlyForALiveSession(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t, h.Limits)
	h.SeedBabies(t, 3)
	guard := h.AuthorityOn(t, h.Runtime)
	service := testkit.SyncServiceOn(t, h, guard)
	owner := h.Owner
	var out familysync.Exchanged
	if err := guard.WithFamilyTx(t.Context(), owner, "read", func(tx pgx.Tx, g access.Grant) error {
		var err error
		out, err = familysync.ExchangeAtCommit(t.Context(), service, tx, owner, g, h.SyncRequest(h.Cursor(0), nil))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if out.Response.NextPageToken != nil {
		t.Fatalf("the page was served before its commit: %+v", out.Response)
	}
	bindings := h.Count(t, "sync_page_bindings", h.FamilyID)
	h.Owner = h.Refresh("owner") // consumes owner's session
	if err := familysync.Settle(t.Context(), service, owner, &out); !errors.Is(err, authority.ErrSessionRevoked) || out.Response.NextPageToken != nil {
		t.Fatalf("settle for a rotated session: %v (token %v), want session_revoked and no page", err, out.Response.NextPageToken)
	}
	if got := h.Count(t, "sync_page_bindings", h.FamilyID); got != bindings {
		t.Fatalf("bindings %d after the refused settle, want %d", got, bindings)
	}
	// The live session's pull stores its binding and continues.
	page, err := applyAndPull(t, service, h.Owner, h.SyncRequest(h.Cursor(0), nil))
	if err != nil || page.NextPageToken == nil || h.Count(t, "sync_page_bindings", h.FamilyID) != bindings+1 {
		t.Fatalf("live pull: %+v %v, %d bindings (want %d)", page, err, h.Count(t, "sync_page_bindings", h.FamilyID), bindings+1)
	}
	if next, err := applyAndPull(t, service, h.Owner, h.SyncRequest(nil, page.NextPageToken)); err != nil || len(next.Changes) != 1 {
		t.Fatalf("continuation: %+v %v", next, err)
	}
}

// Exchange, the exported seam, never leaves a page to settle: in a request
// transaction it reads the pull itself and stores a page's binding before
// the commit, so its Exchanged is complete when it returns (three round
// trips for a page with more, as before the pull went with the COMMIT).
func TestExchangeReturnsACompletePage(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t, h.Limits)
	h.SeedBabies(t, 3)
	guard := h.AuthorityOn(t, h.Runtime)
	service := testkit.SyncServiceOn(t, h, guard)
	bindings := h.Count(t, "sync_page_bindings", h.FamilyID)
	var out familysync.Exchanged
	if err := guard.WithFamilyTx(t.Context(), h.Owner, "read", func(tx pgx.Tx, g access.Grant) error {
		var err error
		out, err = service.Exchange(t.Context(), tx, h.Owner, g, h.SyncRequest(h.Cursor(0), nil))
		if err == nil && (familysync.Unsettled(out) || out.Response.NextPageToken == nil || len(out.Response.EncodedChanges) != 1) {
			t.Errorf("Exchange returned an incomplete page inside its transaction: unsettled %v, %+v", familysync.Unsettled(out), out.Response)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := h.Count(t, "sync_page_bindings", h.FamilyID); got != bindings+1 {
		t.Fatalf("bindings %d after Exchange's page with more, want %d", got, bindings+1)
	}
	if next, err := applyAndPull(t, service, h.Owner, h.SyncRequest(nil, out.Response.NextPageToken)); err != nil || len(next.Changes) != 1 {
		t.Fatalf("continuation of Exchange's page: %+v %v", next, err)
	}
}

// The binding transaction stores only the binding the page's token names
// for the grant it authorized: a page whose binding names another family
// than the binding transaction grants (the session's family changed
// between the two) is refused as unavailable, retryable, and nothing is
// stored.
func TestPullBindingMustBeTheGrantsFamily(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t, h.Limits)
	h.SeedBabies(t, 3)
	guard := h.AuthorityOn(t, h.Runtime)
	service := testkit.SyncServiceOn(t, h, guard)
	var out familysync.Exchanged
	if err := guard.WithFamilyTx(t.Context(), h.Owner, "read", func(tx pgx.Tx, g access.Grant) error {
		var err error
		out, err = familysync.ExchangeAtCommit(t.Context(), service, tx, h.Owner, g, h.SyncRequest(h.Cursor(0), nil))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !familysync.SetPendingFamily(&out, "30000000-0000-4000-8000-0000000000ff") {
		t.Fatal("the page with more has no pending binding")
	}
	bindings := h.Count(t, "sync_page_bindings", h.FamilyID)
	if err := familysync.Settle(t.Context(), service, h.Owner, &out); !errors.Is(err, authority.ErrUnavailable) || out.Response.NextPageToken != nil {
		t.Fatalf("settle of another family's binding: %v (token %v), want unavailable and no page", err, out.Response.NextPageToken)
	}
	if got := h.Count(t, "sync_page_bindings", h.FamilyID); got != bindings {
		t.Fatalf("bindings %d after the refused settle, want %d", got, bindings)
	}
}
