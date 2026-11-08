package liveactivity

import (
	"sort"
	"sync"

	"babytracker/server/internal/protocol"
)

// MaximumDirtyFamilies bounds Dirty. A family recorded past it is dropped:
// the worker's once-a-minute sweep (Queue.SweepWorkPage) dirties every
// registered family whose sequence moved past its last flush, so a dropped
// change is late, never lost.
const MaximumDirtyFamilies = 100_000

// Dirty collects committed family advances from the post-commit change feed
// (a delivery.ChangeRecorder beside the wake tracker) until the worker's
// next poll flushes them (Queue.FlushDirty). Dirtying never happens in the
// write transaction. Safe for concurrent use.
type Dirty struct {
	mu       sync.Mutex
	families map[string]uint64
}

func NewDirty() *Dirty { return &Dirty{families: map[string]uint64{}} }

// Changed records that familyID committed up to sequence; the newest
// sequence of a family wins. An invalid family ID is ignored.
func (d *Dirty) Changed(familyID string, sequence uint64) {
	if d == nil || !protocol.ValidUUID(familyID) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.record(familyID, sequence)
}

func (d *Dirty) record(familyID string, sequence uint64) {
	if d.families == nil {
		d.families = map[string]uint64{}
	}
	if current, ok := d.families[familyID]; ok {
		if sequence > current {
			d.families[familyID] = sequence
		}
		return
	}
	if len(d.families) >= MaximumDirtyFamilies {
		return
	}
	d.families[familyID] = sequence
}

// take empties d and returns what it held, ordered by family ID.
func (d *Dirty) take() (families []string, sequences []uint64) {
	if d == nil {
		return nil, nil
	}
	d.mu.Lock()
	held := d.families
	d.families = map[string]uint64{}
	d.mu.Unlock()
	families = make([]string, 0, len(held))
	for family := range held {
		families = append(families, family)
	}
	sort.Strings(families)
	sequences = make([]uint64, len(families))
	for i, family := range families {
		sequences[i] = held[family]
	}
	return families, sequences
}

// restore puts families that were not flushed back; a newer sequence
// recorded meanwhile wins.
func (d *Dirty) restore(families []string, sequences []uint64) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, family := range families {
		d.record(family, sequences[i])
	}
}

// Len is the number of families waiting for the next flush.
func (d *Dirty) Len() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.families)
}
