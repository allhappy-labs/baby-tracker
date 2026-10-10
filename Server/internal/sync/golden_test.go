package sync_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

type goldenSync struct {
	Outcomes  [][]json.RawMessage `json:"outcomes"`
	Imports   []json.RawMessage   `json:"imports"`
	HighWater []uint64            `json:"high_water"`
	FullPull  goldenPull          `json:"full_pull"`
	MidPull   goldenPull          `json:"mid_pull"`
	Snapshot  goldenSnapshot      `json:"snapshot"`
}

type goldenPull struct {
	Changes   []json.RawMessage `json:"changes"`
	HighWater json.RawMessage   `json:"high_water"`
}

type goldenSnapshot struct {
	Entities  []json.RawMessage `json:"entities"`
	HighWater json.RawMessage   `json:"high_water"`
}

// goldenPageSize keeps every recorded pull and snapshot several pages long,
// so the corpus goes through continuation tokens.
const goldenPageSize = 7

// TestCareWireMatchesLegacyCorpus runs the golden scenario through the
// care-backed service (the import step through the real importer) and
// compares it with the committed legacy corpus (testdata/golden/
// legacy-sync.json, recorded at the Phase 2 base and never rewritten):
//   - every step's outcomes and the import resolve response are byte-equal;
//   - the family high water after each step is equal;
//   - a full pull and a pull from a mid-history cursor serve each entity's
//     latest change: testkit.LatestPerEntity of the recorded pull, byte for
//     byte, with the recorded high water (the one intended difference:
//     pulls deliver latest state, not every intermediate version);
//   - a full snapshot's entities are byte-equal (the corpus holds no
//     running timer with millisecond timestamps, the one place snapshot
//     rows differ from the legacy rendering by ruling).
func TestCareWireMatchesLegacyCorpus(t *testing.T) {
	h := testkit.NewAtFixtureClock(t)
	run := h.RunGolden(t)
	h.Limits.SyncPageSize = goldenPageSize
	h.RebuildSyncService(t, h.Limits)
	full := pullAll(t, h, h.Owner, 0)
	mid := pullAll(t, h, h.Caregiver, run.HighWater[4])
	snap := snapshotAll(t, h, h.Owner)

	raw, err := os.ReadFile("testdata/golden/legacy-sync.json")
	if err != nil {
		t.Fatal(err)
	}
	var legacy goldenSync
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	if len(run.Outcomes) != len(legacy.Outcomes) {
		t.Fatalf("%d steps, corpus %d", len(run.Outcomes), len(legacy.Outcomes))
	}
	for step := range legacy.Outcomes {
		sameItems(t, fmt.Sprintf("step %d outcomes", step+1), legacy.Outcomes[step], run.Outcomes[step])
	}
	sameItems(t, "imports", legacy.Imports, run.Imports)
	if fmt.Sprint(run.HighWater) != fmt.Sprint(legacy.HighWater) {
		t.Fatalf("high waters %v, corpus %v", run.HighWater, legacy.HighWater)
	}
	sameItems(t, "full pull", testkit.LatestPerEntity(t, compactAll(t, legacy.FullPull.Changes)), full.Changes)
	sameBytes(t, "full pull high water", compact(t, legacy.FullPull.HighWater), full.HighWater)
	sameItems(t, "mid pull", testkit.LatestPerEntity(t, compactAll(t, legacy.MidPull.Changes)), mid.Changes)
	sameBytes(t, "mid pull high water", compact(t, legacy.MidPull.HighWater), mid.HighWater)
	sameItems(t, "snapshot", legacy.Snapshot.Entities, snap.Entities)
	sameBytes(t, "snapshot high water", compact(t, legacy.Snapshot.HighWater), snap.HighWater)
}

// compact is raw (indented in the corpus file) as the wire writes it.
func compact(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, raw); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func compactAll(t *testing.T, items []json.RawMessage) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		out = append(out, compact(t, item))
	}
	return out
}

// sameItems requires got to be want (compacted) item for item, byte for byte.
func sameItems(t *testing.T, label string, want, got []json.RawMessage) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s: %d items, corpus %d", label, len(got), len(want))
	}
	for i := range want {
		sameBytes(t, fmt.Sprintf("%s[%d]", label, i), compact(t, want[i]), compact(t, got[i]))
	}
}

func sameBytes(t *testing.T, label string, want, got []byte) {
	t.Helper()
	if bytes.Equal(want, got) {
		return
	}
	i := 0
	for i < len(want) && i < len(got) && want[i] == got[i] {
		i++
	}
	excerpt := func(b []byte) string { return string(b[max(0, i-80):min(len(b), i+80)]) }
	t.Fatalf("%s differs at byte %d\ncorpus …%s…\n  care …%s…", label, i, excerpt(want), excerpt(got))
}

type goldenPage struct {
	Changes       []json.RawMessage `json:"changes"`
	Entities      []json.RawMessage `json:"entities"`
	HighWater     json.RawMessage   `json:"high_water"`
	NextPageToken *string           `json:"next_page_token"`
}

// decodeGoldenPage encodes a response as writeJSON does (minus its newline)
// and splits it, keeping each change's and entity's exact bytes.
func decodeGoldenPage(t *testing.T, response any) goldenPage {
	t.Helper()
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var page goldenPage
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	return page
}

// pullAll pulls from after (0: no cursor) with no mutations, following
// next_page_token to the end, and concatenates the normalized changes.
func pullAll(t *testing.T, h *testkit.Harness, principal access.Principal, after uint64) goldenPull {
	t.Helper()
	var cursor *protocol.Cursor
	if after > 0 {
		cursor = h.Cursor(after)
	}
	request := h.SyncRequest(cursor, nil)
	result := goldenPull{Changes: []json.RawMessage{}}
	for pages := 0; ; pages++ {
		if pages > 100 {
			t.Fatal("pull did not end")
		}
		response, err := h.Service.ApplyAndPull(t.Context(), principal, request)
		if err != nil {
			t.Fatalf("pull page %d: %v", pages, err)
		}
		page := decodeGoldenPage(t, response)
		for _, change := range page.Changes {
			result.Changes = append(result.Changes, h.Normalize(change))
		}
		result.HighWater = h.Normalize(page.HighWater)
		if page.NextPageToken == nil {
			return result
		}
		request = h.SyncRequest(nil, page.NextPageToken)
	}
}

// snapshotAll reads a full snapshot page by page and concatenates the
// normalized entities.
func snapshotAll(t *testing.T, h *testkit.Harness, principal access.Principal) goldenSnapshot {
	t.Helper()
	request := protocol.SnapshotRequest{}
	result := goldenSnapshot{Entities: []json.RawMessage{}}
	for pages := 0; ; pages++ {
		if pages > 100 {
			t.Fatal("snapshot did not end")
		}
		response, err := h.Service.FullSnapshot(t.Context(), principal, request)
		if err != nil {
			t.Fatalf("snapshot page %d: %v", pages, err)
		}
		page := decodeGoldenPage(t, response)
		for _, entity := range page.Entities {
			result.Entities = append(result.Entities, h.Normalize(entity))
		}
		result.HighWater = h.Normalize(page.HighWater)
		if page.NextPageToken == nil {
			return result
		}
		request = protocol.SnapshotRequest{PageToken: page.NextPageToken}
	}
}
