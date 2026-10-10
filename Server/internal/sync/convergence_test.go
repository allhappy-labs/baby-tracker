package sync_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"testing"

	"babytracker/server/internal/access"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/protocol"
	familysync "babytracker/server/internal/sync"
	"babytracker/server/internal/testkit"
)

// simRow is an entity as a device holds it: its version and its encoded
// canonical entity.
type simRow struct {
	version uint64
	wire    string
}

// simDevice is a phone's sync state, applied as the shipped client applies
// it (BabyStore+RemoteApply.swift): every change and snapshot entity
// replaces the stored one unconditionally (sync pages :69-71 and snapshot
// pages :87-89 go through applyCanonical -> OverlayMaterializer.replaceBase,
// :32-45), and the cursor is recorded only from a final page, one without
// next_page_token (:57-93, :104-112). A snapshot keeps entities it did not
// receive. Because the client cannot refuse an older version, the device
// fails the test when it is served one: an entity's version never goes
// back, and an equal version always carries the same wire.
type simDevice struct {
	name    string
	p       access.Principal
	rows    map[string]simRow
	cursor  *protocol.Cursor
	mode    string  // "" idle, "pull" or "snapshot": a paging in flight
	next    *string // the token the in-flight paging continues with
	last    *string // the last continuation token sent, for a lost-response replay
	pages   int
	replays int
}

func newSimDevice(name string, p access.Principal) *simDevice {
	return &simDevice{name: name, p: p, rows: map[string]simRow{}}
}

func (d *simDevice) apply(id string, entity any, version uint64) error {
	wire, err := json.Marshal(entity)
	if err != nil {
		return err
	}
	if current, ok := d.rows[id]; ok {
		if version < current.version {
			return fmt.Errorf("%s served %s at version %d after version %d", d.name, id, version, current.version)
		}
		if version == current.version && string(wire) != current.wire {
			return fmt.Errorf("%s served %s twice at version %d with different bytes", d.name, id, version)
		}
	}
	d.rows[id] = simRow{version: version, wire: string(wire)}
	return nil
}

// pull sends one pull page request (from the cursor, or the in-flight or
// replayed token) and applies it.
func (d *simDevice) pull(ctx context.Context, service *familysync.Service, request protocol.SyncRequest) error {
	response, err := service.ApplyAndPull(ctx, d.p, request)
	if err != nil {
		return fmt.Errorf("%s pull: %w", d.name, err)
	}
	raw, err := json.Marshal(response)
	if err != nil {
		return err
	}
	var decoded protocol.SyncResponse
	if err := decoded.UnmarshalJSON(raw); err != nil {
		return err
	}
	for _, change := range decoded.Changes {
		if err := d.apply(change.EntityID, change.SnapshotOrTombstone, change.EntityVersion); err != nil {
			return err
		}
	}
	d.pages++
	d.mode, d.next = "pull", decoded.NextPageToken
	if decoded.NextPageToken == nil {
		high := decoded.HighWater
		d.mode, d.cursor = "", &high
	}
	return nil
}

func (d *simDevice) snapshot(ctx context.Context, service *familysync.Service, token *string) error {
	response, err := service.FullSnapshot(ctx, d.p, protocol.SnapshotRequest{PageToken: token})
	if err != nil {
		return fmt.Errorf("%s snapshot: %w", d.name, err)
	}
	raw, err := json.Marshal(response)
	if err != nil {
		return err
	}
	var decoded protocol.SnapshotResponse
	if err := decoded.UnmarshalJSON(raw); err != nil {
		return err
	}
	for _, entity := range decoded.Entities {
		if err := d.apply(entity.Meta.ID, entity, entity.Meta.EntityVersion); err != nil {
			return err
		}
	}
	d.pages++
	d.mode, d.next = "snapshot", decoded.NextPageToken
	if decoded.NextPageToken == nil {
		high := decoded.HighWater
		d.mode, d.cursor = "", &high
	}
	return nil
}

func syncRequest(cursor *protocol.Cursor, token *string) protocol.SyncRequest {
	return protocol.SyncRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityRecordsV1}},
		Cursor: cursor, Mutations: []protocol.Mutation{}, PageToken: token}
}

// step advances the device by one page: the in-flight paging's next page,
// or a new pull (or, the first time and when allowed, a snapshot).
func (d *simDevice) step(ctx context.Context, service *familysync.Service, snapshotAllowed bool) error {
	switch {
	case d.mode == "pull":
		d.last = d.next
		return d.pull(ctx, service, syncRequest(nil, d.next))
	case d.mode == "snapshot":
		d.last = d.next
		return d.snapshot(ctx, service, d.next)
	case d.cursor == nil || snapshotAllowed:
		d.last = nil
		return d.snapshot(ctx, service, nil)
	default:
		d.last = nil
		return d.pull(ctx, service, syncRequest(d.cursor, nil))
	}
}

// replay re-sends the last continuation token, as a client whose response
// was lost does, and continues from that response.
func (d *simDevice) replay(ctx context.Context, service *familysync.Service, kind string) error {
	if d.last == nil {
		return nil
	}
	d.replays++
	if kind == "snapshot" {
		return d.snapshot(ctx, service, d.last)
	}
	return d.pull(ctx, service, syncRequest(nil, d.last))
}

// drain finishes the device's paging and pulls to the end.
func (d *simDevice) drain(ctx context.Context, service *familysync.Service) error {
	for pages := 0; d.mode != "" || d.cursor == nil; pages++ {
		if pages > 10000 {
			return fmt.Errorf("%s: paging does not end", d.name)
		}
		if err := d.step(ctx, service, false); err != nil {
			return err
		}
	}
	if err := d.pull(ctx, service, syncRequest(d.cursor, nil)); err != nil {
		return err
	}
	for pages := 0; d.mode != ""; pages++ {
		if pages > 10000 {
			return fmt.Errorf("%s: pull does not end", d.name)
		}
		if err := d.step(ctx, service, false); err != nil {
			return err
		}
	}
	return nil
}

// sameAsServer compares the device's map with {id: (entity_version, wire)}
// of every care_entities row, tombstones included.
func sameAsServer(device *simDevice, server map[string]simRow) error {
	var diffs []string
	for id, row := range server {
		got, ok := device.rows[id]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("missing %s v%d", id, row.version))
		} else if got != row {
			diffs = append(diffs, fmt.Sprintf("%s: device v%d server v%d (wire equal %t)", id, got.version, row.version, got.wire == row.wire))
		}
	}
	for id := range device.rows {
		if _, ok := server[id]; !ok {
			diffs = append(diffs, "extra "+id)
		}
	}
	if len(diffs) == 0 {
		return nil
	}
	sort.Strings(diffs)
	return fmt.Errorf("%s diverged from the server (%d): %s", device.name, len(diffs), strings.Join(diffs[:min(len(diffs), 10)], "; "))
}

func serverRows(t *testing.T, h *testkit.Harness) map[string]simRow {
	t.Helper()
	out := map[string]simRow{}
	for id, row := range caretest.Rows(t, h) {
		out[id] = simRow{version: row.EntityVersion, wire: string(row.Wire)}
	}
	return out
}

// simWriter writes random batches and keeps the model it needs to build
// valid mutations from its own outcomes.
type simWriter struct {
	rng      *rand.Rand
	next     int
	babies   []string
	live     map[string]bool // baby -> live
	records  map[string]protocol.Mutation
	versions map[string]uint64
	deleted  map[string]bool
	timers   map[string]string // live timer -> baby
}

func newSimWriter(seed uint64) *simWriter {
	return &simWriter{rng: rand.New(rand.NewPCG(seed, seed^0x5eed)), live: map[string]bool{}, records: map[string]protocol.Mutation{},
		versions: map[string]uint64{}, deleted: map[string]bool{}, timers: map[string]string{}}
}

func (w *simWriter) id(prefix string) string {
	w.next++
	return fmt.Sprintf("%s000000-0000-4000-8000-%012d", prefix, w.next)
}

// batch is 1-5 mutations; owner says whether baby deletes may be in it.
func (w *simWriter) batch(owner bool) []protocol.Mutation {
	size := 1 + w.rng.IntN(5)
	batch := make([]protocol.Mutation, 0, size)
	used := map[string]bool{}
	for attempt := 0; len(batch) < size && attempt < 100; attempt++ {
		var m protocol.Mutation
		roll := w.rng.IntN(100)
		switch {
		case roll < 12 || len(w.babies) == 0:
			if w.liveBabies() >= 4 { // well under max_live_babies, counting this batch's creates
				if len(w.babies) == 0 {
					return batch // only babies of this batch exist yet
				}
				continue
			}
			m = caretest.Baby(w.id("1a"))
			w.live[m.EntityID] = true // reserved: counted until the outcome is learned
		case roll < 16 && owner:
			baby := w.babies[w.rng.IntN(len(w.babies))]
			if !w.live[baby] {
				continue
			}
			m = caretest.DeleteBaby(baby)
		case roll < 50:
			kinds := []string{"feed", "sleep", "diaper", "supplement", "measurement"}
			w.next++
			m = caretest.Record(kinds[w.rng.IntN(len(kinds))], w.babies[w.rng.IntN(len(w.babies))], w.next)
		case roll < 75 && len(w.records) > 0:
			create := w.pick()
			observed := w.versions[create.EntityID]
			if observed > 1 && w.rng.IntN(4) == 0 {
				observed-- // a stale edit: last writer wins, overwrote_newer
			}
			m = caretest.Update(create, w.id("0e"), observed, w.edit())
		case roll < 87 && len(w.records) > 0:
			create := w.pick()
			m = caretest.Delete(create, w.id("0f"), w.versions[create.EntityID])
		case roll < 94:
			kind := []string{"feed", "sleep"}[w.rng.IntN(2)]
			m = caretest.StartTimer(w.babies[w.rng.IntN(len(w.babies))], w.id("5a"), kind)
		default:
			if len(w.timers) == 0 {
				continue
			}
			ids := make([]string, 0, len(w.timers))
			for id := range w.timers {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			timer := ids[w.rng.IntN(len(ids))]
			m = caretest.Stop(w.timers[timer], timer, w.id("2b"))
		}
		if used[m.EntityID] {
			continue // one mutation per entity per batch keeps the model simple
		}
		used[m.EntityID] = true
		batch = append(batch, m)
	}
	return batch
}

// edit changes a record's note, or a measurement's weight.
func (w *simWriter) edit() func(protocol.Payload) protocol.Payload {
	note := caretest.WithNote(fmt.Sprintf("edit %d", w.next))
	weight := protocol.MustParseDecimal(fmt.Sprintf("7.%03d", w.next%1000))
	return func(p protocol.Payload) protocol.Payload {
		if measurement, ok := p.(protocol.MeasurementPayload); ok {
			measurement.WeightKG = weight
			return measurement
		}
		return note(p)
	}
}

func (w *simWriter) liveBabies() int {
	n := 0
	for _, live := range w.live {
		if live {
			n++
		}
	}
	return n
}

func (w *simWriter) pick() protocol.Mutation {
	ids := make([]string, 0, len(w.records))
	for id := range w.records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return w.records[ids[w.rng.IntN(len(ids))]]
}

// learn updates the model from the batch's outcomes.
func (w *simWriter) learn(batch []protocol.Mutation, outcomes []protocol.Outcome) {
	for i, m := range batch {
		o := outcomes[i]
		if o.Status != "applied" || o.CanonicalEntity == nil {
			if m.EntityKind == "baby" && m.Operation == "create" {
				delete(w.live, m.EntityID)
			}
			continue
		}
		version := o.CanonicalEntity.Meta.EntityVersion
		tombstoned := o.CanonicalEntity.Meta.TombstonedAt != nil
		switch {
		case m.EntityKind == "baby" && m.Operation == "create":
			w.babies = append(w.babies, m.EntityID)
		case m.EntityKind == "baby":
			w.live[m.EntityID] = false
			for timer, baby := range w.timers {
				if baby == m.EntityID {
					delete(w.timers, timer)
				}
			}
		case m.EntityKind == "timer" && m.Operation == "timer_start":
			w.timers[m.EntityID] = caretest.BabyOf(m)
		case m.EntityKind == "timer":
			delete(w.timers, m.EntityID)
		case m.Operation == "create":
			w.records[m.EntityID], w.versions[m.EntityID] = m, version
		case tombstoned:
			delete(w.records, m.EntityID)
		default:
			w.versions[m.EntityID] = version
		}
	}
}

// write applies one batch as p through ApplyAndPull (from the current high
// water) and learns its outcomes.
func (w *simWriter) write(ctx context.Context, service *familysync.Service, h *testkit.Harness, p access.Principal, owner bool, highWater uint64) error {
	batch := w.batch(owner)
	request := syncRequest(h.Cursor(highWater), nil)
	request.Mutations = batch
	for _, m := range batch {
		if m.EntityKind == "timer" {
			request.Capabilities = append(request.Capabilities, protocol.CapabilityTimerCommandsV1)
			break
		}
	}
	response, err := service.ApplyAndPull(ctx, p, request)
	if err != nil {
		return fmt.Errorf("write batch %v: %w", describeBatch(batch), err)
	}
	w.learn(batch, response.Outcomes)
	return nil
}

func describeBatch(batch []protocol.Mutation) []string {
	out := make([]string, 0, len(batch))
	for _, m := range batch {
		out = append(out, m.EntityKind+" "+m.Operation)
	}
	return out
}

// newSim is a harness at page size 3 with three devices: the owner, the
// caregiver and a second caregiver phone (account snapshot cap: 2).
func newSim(t *testing.T) (*testkit.Harness, []*simDevice) {
	t.Helper()
	h := testkit.New(t)
	h.Limits.SyncPageSize = 3
	h.RebuildSyncService(t, h.Limits)
	second, err := h.NewDevice("caregiver")
	if err != nil {
		t.Fatal(err)
	}
	return h, []*simDevice{newSimDevice("owner", h.Owner), newSimDevice("caregiver", h.Caregiver), newSimDevice("caregiver-2", second)}
}

// TestPullAndSnapshotConvergeUnderAnyInterleaving interleaves random write
// batches (creates, updates, stale updates, deletes, timer starts and
// stops, baby creates and cascading deletes) by the owner and the
// caregiver with single pages of in-flight pulls and snapshots on three
// devices and replays of their last continuation token. After every device
// drains and pulls to the end, each device's map equals the server's rows.
func TestPullAndSnapshotConvergeUnderAnyInterleaving(t *testing.T) {
	const seeds, steps = 40, 120
	for seed := range uint64(seeds) {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			h, devices := newSim(t)
			rng := rand.New(rand.NewPCG(seed, 0xc0ffee))
			writer := newSimWriter(seed)
			snapshots := map[string]int{} // account -> snapshot sessions started
			snapshotAllowed := func(d *simDevice) bool {
				// A device without a cursor must snapshot; re-snapshotting
				// is rare and kept within the account cap (2, no expiry
				// on the fixed clock).
				return d.cursor == nil || (rng.IntN(10) == 0 && snapshots[d.p.AccountID] < 2-countWithoutCursor(devices, d.p.AccountID))
			}
			for step := range steps {
				var err error
				switch roll := rng.IntN(10); {
				case roll < 4:
					owner := rng.IntN(2) == 0
					p := h.Caregiver
					if owner {
						p = h.Owner
					}
					err = writer.write(t.Context(), h.Service, h, p, owner, h.FamilySequence(t))
				case roll < 9:
					d := devices[rng.IntN(len(devices))]
					startsSnapshot := d.mode == "" && snapshotAllowed(d)
					if startsSnapshot {
						snapshots[d.p.AccountID]++
					}
					err = d.step(t.Context(), h.Service, startsSnapshot && d.cursor != nil)
				default:
					d := devices[rng.IntN(len(devices))]
					if d.mode != "" {
						err = d.replay(t.Context(), h.Service, d.mode)
					}
				}
				if err != nil {
					t.Fatalf("seed %d step %d: %v", seed, step, err)
				}
			}
			server := serverRows(t, h)
			for _, d := range devices {
				if d.cursor == nil && d.mode == "" {
					snapshots[d.p.AccountID]++
				}
				if err := d.drain(t.Context(), h.Service); err != nil {
					t.Fatalf("seed %d drain: %v", seed, err)
				}
				if err := sameAsServer(d, server); err != nil {
					t.Fatalf("seed %d: %v", seed, err)
				}
			}
		})
	}
}

func countWithoutCursor(devices []*simDevice, account string) int {
	n := 0
	for _, d := range devices {
		if d.p.AccountID == account && d.cursor == nil && d.mode != "snapshot" {
			n++
		}
	}
	return n
}

// TestConcurrentWritersAndPagersConverge runs the simulation with real
// concurrency: a writer goroutine racing two pager goroutines (run under
// -race). After the writer stops, both devices drain and equal the server.
func TestConcurrentWritersAndPagersConverge(t *testing.T) {
	for seed := range uint64(4) {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			h, devices := newSim(t)
			pagers := devices[:2]
			writer := newSimWriter(seed + 1000)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var wg sync.WaitGroup
			errs := make(chan error, 3)
			var writes sync.WaitGroup
			writes.Add(1)
			go func() {
				defer writes.Done()
				for i := range 60 {
					owner := i%2 == 0
					p := h.Caregiver
					if owner {
						p = h.Owner
					}
					// The writer's own pull starts at 0: it does not track the
					// family high water across goroutines.
					if err := writer.write(ctx, h.Service, h, p, owner, 0); err != nil {
						errs <- err
						return
					}
				}
			}()
			for i, d := range pagers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					rng := rand.New(rand.NewPCG(seed, uint64(i)))
					for range 80 {
						var err error
						if d.mode != "" && rng.IntN(5) == 0 {
							err = d.replay(ctx, h.Service, d.mode)
						} else {
							err = d.step(ctx, h.Service, false)
						}
						if err != nil {
							errs <- err
							return
						}
					}
				}()
			}
			writes.Wait()
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatalf("seed %d: %v", seed, err)
			}
			server := serverRows(t, h)
			for _, d := range pagers {
				if err := d.drain(t.Context(), h.Service); err != nil {
					t.Fatalf("seed %d drain: %v", seed, err)
				}
				if err := sameAsServer(d, server); err != nil {
					t.Fatalf("seed %d: %v", seed, err)
				}
			}
		})
	}
}
