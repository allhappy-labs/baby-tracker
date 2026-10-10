package sync_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/protocol"
	familysync "babytracker/server/internal/sync"
	"babytracker/server/internal/testkit"
)

// seedFeeds seeds caretest.BabyA and n feeds (sequences 1..n+1) and
// returns the feed creates.
func seedFeeds(t *testing.T, h *testkit.Harness, n int) []protocol.Mutation {
	t.Helper()
	feeds := make([]protocol.Mutation, 0, n)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	for i := range n {
		feed := caretest.Feed(caretest.BabyA, i+1)
		caretest.Seed(t, h, feed)
		feeds = append(feeds, feed)
	}
	return feeds
}

// editFeed updates feed (its tag-th edit: a distinct mutation id) with a
// note.
func editFeed(t *testing.T, h *testkit.Harness, feed protocol.Mutation, observed uint64, tag int) {
	t.Helper()
	caretest.Seed(t, h, caretest.Update(feed, fmt.Sprintf("e%x", tag)+feed.MutationID[2:], observed, caretest.WithNote(fmt.Sprintf("edit %d", tag))))
}

func changeIDs(changes []protocol.JournalChange) []string {
	ids := make([]string, 0, len(changes))
	for _, c := range changes {
		ids = append(ids, c.EntityID)
	}
	return ids
}

// Review Focus 1: a continuation replayed after a concurrent edit (a lost
// response, the client retries the same page_token) returns the current
// rows of its range, never invalid_page_token; the edited row has moved
// past the range's high water and arrives in the next pull, and the device
// converges.
func TestSyncContinuationReplayAfterConcurrentEdit(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 3
	h.RebuildSyncService(t, h.Limits)
	feeds := seedFeeds(t, h, 9) // baby at 1, feeds 1..9 at 2..10
	device := newSimDevice("caregiver", h.Caregiver)
	if err := device.pull(t.Context(), h.Service, syncRequest(h.Cursor(0), nil)); err != nil || device.next == nil {
		t.Fatalf("page 1: %v, token %v", err, device.next)
	}
	token := device.next

	editFeed(t, h, feeds[4], 1, 1) // feed 5: sequence 6 -> 11

	var pages [2]protocol.SyncResponse
	for i := range pages {
		response, err := applyAndPull(t, h.Service, h.Caregiver, h.SyncRequest(nil, token))
		if err != nil {
			t.Fatalf("continuation request %d: %v", i+1, err)
		}
		pages[i] = response
		for _, c := range response.Changes {
			if c.EntityID == feeds[4].EntityID {
				t.Fatalf("continuation %d served the edited feed: %+v", i+1, c)
			}
		}
	}
	if want := []string{feeds[2].EntityID, feeds[3].EntityID, feeds[5].EntityID}; fmt.Sprint(changeIDs(pages[0].Changes)) != fmt.Sprint(want) ||
		fmt.Sprint(changeIDs(pages[1].Changes)) != fmt.Sprint(want) || pages[0].HighWater.Sequence != 10 || pages[1].HighWater.Sequence != 10 {
		t.Fatalf("replayed continuation pages %v / %v, want %v at high water 10", changeIDs(pages[0].Changes), changeIDs(pages[1].Changes), want)
	}
	if pages[0].NextPageToken == nil || pages[1].NextPageToken == nil || *pages[0].NextPageToken != *pages[1].NextPageToken {
		t.Fatalf("replay issued another successor: %v / %v", pages[0].NextPageToken, pages[1].NextPageToken)
	}
	// Every page of the continued pull stays within the range's high water:
	// the edited feed is not served until the next pull.
	for next := pages[0].NextPageToken; next != nil; {
		page, err := applyAndPull(t, h.Service, h.Caregiver, h.SyncRequest(nil, next))
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range page.Changes {
			if c.Cursor.Sequence > page.HighWater.Sequence || c.EntityID == feeds[4].EntityID {
				t.Fatalf("continuation served %s at %d past its high water %d", c.EntityID, c.Cursor.Sequence, page.HighWater.Sequence)
			}
		}
		next = page.NextPageToken
	}

	// The device continues from the replayed response, then pulls from the
	// high water: feed 5 arrives at version 2.
	device.last = token
	if err := device.replay(t.Context(), h.Service, "pull"); err != nil {
		t.Fatal(err)
	}
	if err := device.drain(t.Context(), h.Service); err != nil {
		t.Fatal(err)
	}
	if got := device.rows[feeds[4].EntityID]; got.version != 2 {
		t.Fatalf("feed 5 on the device at version %d, want 2", got.version)
	}
	after := h.Pull(t, h.Caregiver, h.Cursor(10), nil)
	if len(after.Changes) != 1 || after.Changes[0].EntityID != feeds[4].EntityID || after.Changes[0].EntityVersion != 2 {
		t.Fatalf("pull from the high water: %+v", after.Changes)
	}
	if err := sameAsServer(device, serverRows(t, h)); err != nil {
		t.Fatal(err)
	}
}

// Review Focus 1: a range emptied entirely by edits ends without a token
// (no token loop), and the next pull delivers the edited rows.
func TestEmptiedRangeEndsWithoutToken(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 3
	h.RebuildSyncService(t, h.Limits)
	feeds := seedFeeds(t, h, 5) // baby at 1, feeds at 2..6
	first, err := applyAndPull(t, h.Service, h.Caregiver, h.SyncRequest(h.Cursor(0), nil))
	if err != nil || first.NextPageToken == nil || first.HighWater.Sequence != 6 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	for _, feed := range feeds[2:] { // every row of (3, 6]
		editFeed(t, h, feed, 1, 2)
	}
	page, err := applyAndPull(t, h.Service, h.Caregiver, h.SyncRequest(nil, first.NextPageToken))
	if err != nil || len(page.Changes) != 0 || page.NextPageToken != nil || page.HighWater.Sequence != 6 {
		t.Fatalf("emptied continuation=%+v err=%v", page, err)
	}
	raw, err := json.Marshal(page)
	if err != nil || !strings.Contains(string(raw), `"changes":[]`) || !strings.Contains(string(raw), `"next_page_token":null`) {
		t.Fatalf("emptied continuation on the wire: %s %v", raw, err)
	}
	next := h.Pull(t, h.Caregiver, &page.HighWater, nil)
	if want := []string{feeds[2].EntityID, feeds[3].EntityID, feeds[4].EntityID}; fmt.Sprint(changeIDs(next.Changes)) != fmt.Sprint(want) {
		t.Fatalf("pull after the emptied range: %v, want %v", changeIDs(next.Changes), want)
	}
}

// A snapshot without staging pages current rows: during paging a row is
// created with a key before the resume, a paged row is updated and an
// unpaged row deleted. After the snapshot and one pull the device equals
// the server. Admission caps hold, and a family over snapshot_max_entities
// is refused before any page.
func TestSnapshotWithoutStagingConvergesUnderConcurrentWrites(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 3
	h.RebuildSyncService(t, h.Limits)
	feeds := seedFeeds(t, h, 6)
	// A tombstone from before the capture, on a later page: only the
	// snapshot can deliver it (the next pull starts at the high water).
	caretest.Seed(t, h, caretest.Delete(feeds[3], "0f"+feeds[3].MutationID[2:], 1))
	device := newSimDevice("owner", h.Owner)
	if err := device.snapshot(t.Context(), h.Service, nil); err != nil || device.next == nil {
		t.Fatalf("snapshot page 1: %v", err)
	}
	// Page 1 is the baby and the first two feeds (key order); the resume key
	// is feed 2. Feed ids are 2000000n-…: a new feed 20000000-… sorts first.
	before := caretest.Record("feed", caretest.BabyA, 0)
	caretest.Seed(t, h, before)
	editFeed(t, h, feeds[0], 1, 3)
	caretest.Seed(t, h, caretest.Delete(feeds[5], "0f"+feeds[5].MutationID[2:], 1))
	for device.mode != "" {
		if err := device.step(t.Context(), h.Service, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := device.rows[before.EntityID]; ok {
		t.Fatal("a row created before the resume key reached the snapshot")
	}
	if device.rows[feeds[0].EntityID].version != 1 {
		t.Fatal("the paged row is not at its paged version")
	}
	if err := device.pull(t.Context(), h.Service, syncRequest(device.cursor, nil)); err != nil || device.mode != "" {
		t.Fatalf("pull after the snapshot: %v (mode %q)", err, device.mode)
	}
	if err := sameAsServer(device, serverRows(t, h)); err != nil {
		t.Fatal(err)
	}

	// Admission: the account holds one session; a second is admitted, a
	// third refused before anything is written.
	if _, err := fullSnapshot(t, h.Service, h.Owner, protocol.SnapshotRequest{}); err != nil {
		t.Fatal(err)
	}
	sessions, bindings := h.Count(t, "sync_snapshot_sessions", h.FamilyID), h.Count(t, "sync_page_bindings", h.FamilyID)
	if _, err := fullSnapshot(t, h.Service, h.Owner, protocol.SnapshotRequest{}); !errors.Is(err, familysync.ErrSnapshotLimit) {
		t.Fatalf("third active snapshot: %v", err)
	}
	if h.Count(t, "sync_snapshot_sessions", h.FamilyID) != sessions || h.Count(t, "sync_page_bindings", h.FamilyID) != bindings {
		t.Fatal("a refused snapshot wrote a session or a binding")
	}

	// A family over snapshot_max_entities is refused before any page.
	h.Limits.SnapshotMaxEntities = 1000
	h.RebuildSyncService(t, h.Limits)
	more := make([]protocol.Mutation, 0, 1000)
	for n := range 1000 {
		more = append(more, caretest.Record("diaper", caretest.BabyA, n+1))
	}
	caretest.Seed(t, h, more...)
	if _, err := fullSnapshot(t, h.Service, h.Caregiver, protocol.SnapshotRequest{}); !errors.Is(err, familysync.ErrSnapshotLimit) {
		t.Fatalf("over snapshot_max_entities: %v", err)
	}
	if h.Count(t, "sync_snapshot_sessions", h.FamilyID) != sessions {
		t.Fatal("an over-size snapshot wrote a session")
	}
}

// A cursor ahead of the family high water needs a snapshot. A pull-only
// request refuses it and writes nothing. A mutation batch is applied and
// committed first (as the legacy per-mutation commits were), then refused;
// the client's retry from a valid cursor gets already_applied.
func TestCursorAheadOfTheHighWaterRequiresASnapshot(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA), caretest.Feed(caretest.BabyA, 1))
	ahead := h.Cursor(50)
	bindings := h.Count(t, "sync_page_bindings", h.FamilyID)
	if _, err := applyAndPull(t, h.Service, h.Owner, h.SyncRequest(ahead, nil)); !errors.Is(err, familysync.ErrSnapshotRequired) {
		t.Fatalf("pull-only from a cursor ahead: %v", err)
	}
	if h.FamilySequence(t) != 2 || h.Count(t, "sync_page_bindings", h.FamilyID) != bindings || h.Count(t, "care_receipts", h.FamilyID) != 2 {
		t.Fatal("a refused pull wrote something")
	}
	feed := caretest.Feed(caretest.BabyA, 2)
	request := h.SyncRequest(ahead, nil)
	request.Mutations = []protocol.Mutation{feed}
	if _, err := applyAndPull(t, h.Service, h.Owner, request); !errors.Is(err, familysync.ErrSnapshotRequired) {
		t.Fatalf("batch from a cursor ahead: %v", err)
	}
	if h.FamilySequence(t) != 3 || h.CareCount(t, "feed", h.FamilyID) != 2 || h.Count(t, "care_receipts", h.FamilyID) != 3 {
		t.Fatalf("the batch was not committed before the refusal: sequence %d", h.FamilySequence(t))
	}
	request.Cursor = h.Cursor(3)
	retry, err := applyAndPull(t, h.Service, h.Owner, request)
	if err != nil || len(retry.Outcomes) != 1 || retry.Outcomes[0].Status != "already_applied" || retry.Outcomes[0].OriginalStatus == nil ||
		*retry.Outcomes[0].OriginalStatus != "applied" || h.FamilySequence(t) != 3 {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
}
