// Package wake wakes devices that are behind their family.
//
// The request path records progress in memory only (Tracker): the cursor a
// device presented (acknowledged) and the high water of the last final page
// served to it (delivered), and each committed advance of a family's
// sequence. A background Worker flushes that progress to
// device_sync_cursors on the maintenance pool, compares it with
// families.next_sequence, and wakes the devices that are behind: at most
// once a minute per device and once per sequence, at most MaxWakesPerTick a
// tick. The write transaction records no wake intent.
package wake

import (
	"slices"
	"strings"
	"sync"
	"time"

	"babytracker/server/internal/protocol"
)

// maxSequence is the largest cursor sequence (device_sync_cursors' CHECK and
// the protocol's JSON-safe integers).
const maxSequence = 1<<53 - 1

// defaultMaxDevices bounds the tracker between flushes: at the load model
// (500 to 700 pulls a second, a 2-second tick) a flush carries under 1,500
// devices, so the bound is only reached when flushes fail for minutes.
const defaultMaxDevices = 200_000

type deviceKey struct{ family, device string }

type progress struct {
	acked, delivered uint64
	seen             time.Time
}

// Tracker is the in-memory progress the request path records and the wake
// worker drains. Every method is safe for concurrent use and touches no
// database.
type Tracker struct {
	mu       sync.Mutex
	max      int
	devices  map[deviceKey]progress
	families map[string]uint64
	// deferred are families with a device that is behind but was woken
	// inside the last MinWakeInterval: they are scanned again when its
	// window ends, so the last change of a burst is announced too.
	deferred map[string]time.Time
	dropped  uint64
	// afterTake, in tests only, runs between the swap under the mutex and
	// the sort.
	afterTake func()
}

// NewTracker returns a tracker of at most maxDevices devices between
// flushes (0 means 200,000). Beyond it new devices are dropped until the
// next flush; known devices still update. Changed and deferred families
// share the same bound.
func NewTracker(maxDevices int) *Tracker {
	if maxDevices <= 0 {
		maxDevices = defaultMaxDevices
	}
	return &Tracker{max: maxDevices, devices: make(map[deviceKey]progress), families: make(map[string]uint64),
		deferred: make(map[string]time.Time)}
}

// Synced records one completed request: the cursor the device presented
// (acked, 0 for none) and, for a final page, the high water served
// (delivered, 0 otherwise). It never touches the database. An invalid
// identifier or sequence is dropped.
func (t *Tracker) Synced(familyID, deviceID string, acked, delivered uint64, at time.Time) {
	if t == nil {
		return
	}
	valid := protocol.ValidUUID(familyID) && protocol.ValidUUID(deviceID) && acked <= maxSequence && delivered <= maxSequence
	key := deviceKey{familyID, deviceID}
	t.mu.Lock()
	defer t.mu.Unlock()
	current, known := t.devices[key]
	if !valid || !known && len(t.devices) >= t.max {
		t.dropped++
		return
	}
	t.devices[key] = merged(current, progress{acked: acked, delivered: delivered, seen: at})
}

// Changed records a committed advance of the family's sequence.
func (t *Tracker) Changed(familyID string, sequence uint64) {
	if t == nil {
		return
	}
	valid := protocol.ValidUUID(familyID) && sequence <= maxSequence
	t.mu.Lock()
	defer t.mu.Unlock()
	current, known := t.families[familyID]
	if !valid || !known && len(t.families) >= t.max {
		t.dropped++
		return
	}
	t.families[familyID] = max(current, sequence)
}

// Dropped is the number of Synced and Changed calls dropped so far (invalid,
// or past the bound).
func (t *Tracker) Dropped() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dropped
}

func merged(a, b progress) progress {
	result := progress{acked: max(a.acked, b.acked), delivered: max(a.delivered, b.delivered), seen: a.seen}
	if b.seen.After(result.seen) {
		result.seen = b.seen
	}
	return result
}

// deviceProgress is one device's drained progress.
type deviceProgress struct {
	FamilyID, DeviceID string
	Acked, Delivered   uint64
	Seen               time.Time
}

// familyChange is one drained family: its highest recorded sequence (0 for
// a deferred family that changed no further).
type familyChange struct {
	FamilyID string
	Sequence uint64
}

// drained is one tick's work: every device recorded since the last drain,
// in (family, device) order, and up to maxFamilies families, in ID order.
type drained struct {
	devices  []deviceProgress
	families []familyChange
}

// drain takes every recorded device and up to maxFamilies (0: all) of the
// changed families and the deferred families due at now. The mutex is held
// only to swap the maps out (and to put back families past maxFamilies):
// building and sorting the slices happens after it is released, so a
// request's Synced never waits for a sort, whatever the tracker holds.
func (t *Tracker) drain(now time.Time, maxFamilies int) drained {
	devices, families := t.take(now)
	if t.afterTake != nil {
		t.afterTake()
	}
	var work drained
	if len(devices) > 0 {
		work.devices = make([]deviceProgress, 0, len(devices))
		for key, value := range devices {
			work.devices = append(work.devices, deviceProgress{FamilyID: key.family, DeviceID: key.device,
				Acked: value.acked, Delivered: value.delivered, Seen: value.seen})
		}
		slices.SortFunc(work.devices, func(a, b deviceProgress) int {
			if c := strings.Compare(a.FamilyID, b.FamilyID); c != 0 {
				return c
			}
			return strings.Compare(a.DeviceID, b.DeviceID)
		})
	}
	if len(families) > 0 {
		work.families = make([]familyChange, 0, len(families))
		for family, sequence := range families {
			work.families = append(work.families, familyChange{FamilyID: family, Sequence: sequence})
		}
		slices.SortFunc(work.families, func(a, b familyChange) int { return strings.Compare(a.FamilyID, b.FamilyID) })
		if maxFamilies > 0 && len(work.families) > maxFamilies {
			t.requeue(work.families[maxFamilies:])
			work.families = work.families[:maxFamilies:maxFamilies]
		}
	}
	return work
}

// take swaps out, under the mutex, the recorded devices and the changed
// families together with the deferred families due at now. An empty map is
// not swapped out, and not returned either: the caller reads what take
// returns after the mutex is released, while Synced and Changed keep
// writing the tracker's own maps.
func (t *Tracker) take(now time.Time) (map[deviceKey]progress, map[string]uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var devices map[deviceKey]progress
	if len(t.devices) > 0 {
		devices, t.devices = t.devices, make(map[deviceKey]progress)
	}
	var families map[string]uint64
	if len(t.families) > 0 {
		families, t.families = t.families, make(map[string]uint64)
	}
	for family, at := range t.deferred {
		_, changed := families[family]
		switch {
		case changed:
			delete(t.deferred, family) // scanned now; the scan defers it again if needed
		case !at.After(now):
			if len(families) == 0 {
				families = make(map[string]uint64)
			}
			families[family] = 0
			delete(t.deferred, family)
		}
	}
	return devices, families
}

// restore puts drained work back after a failed tick: progress recorded
// since merges with it (newer values win), and its families are scanned
// again on the next tick. Restored entries are not new, so the bound does
// not refuse them.
func (t *Tracker) restore(work drained) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, d := range work.devices {
		key := deviceKey{d.FamilyID, d.DeviceID}
		t.devices[key] = merged(t.devices[key], progress{acked: d.Acked, delivered: d.Delivered, seen: d.Seen})
	}
	t.requeueLocked(work.families)
}

// requeue scans families again on the next tick.
func (t *Tracker) requeue(families []familyChange) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.requeueLocked(families)
}

func (t *Tracker) requeueLocked(families []familyChange) {
	for _, f := range families {
		t.families[f.FamilyID] = max(t.families[f.FamilyID], f.Sequence)
	}
}

// deferFamily scans familyID again at at (the earliest time asked for
// stands), unless it changes first.
func (t *Tracker) deferFamily(familyID string, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	current, known := t.deferred[familyID]
	if known {
		if at.Before(current) {
			t.deferred[familyID] = at
		}
		return
	}
	if len(t.deferred) >= t.max {
		t.dropped++
		return
	}
	t.deferred[familyID] = at
}
