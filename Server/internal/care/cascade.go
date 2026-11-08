package care

import (
	"slices"
	"strings"

	"babytracker/server/internal/protocol"
)

// The baby-delete cascade is the legacy DeleteBaby (records/child_delete.go at 342973dc)
// decided against the working set. The family lock Fetch took is the only
// lock: the baby, its live rows (fetched by baby, without wire) and the rows
// this batch wrote are all in the working set, so the cascade is one write
// set whatever its size, written in the batch's one write round trip.

// cascadeKindOrder is the legacy cascade order of a baby's children: feed,
// sleep, diaper, supplement, growth_profile, measurement, timer.
var cascadeKindOrder = map[string]int{"feed": 0, "sleep": 1, "diaper": 2, "supplement": 3, "growth_profile": 4, "measurement": 5, "immunization": 6, "timer": 7}

// deleteBaby tombstones the baby, then every live child in cascade order
// (by kind, then id), each at the next sequence: version+1, creator kept,
// the deleting account as last editor, no completion for a timer. The
// outcome is the baby's tombstone at the last sequence. A delete is never
// refused by a quota; the children leave the record count and release
// their reserves.
func (ws *workingSet) deleteBaby(m protocol.Mutation) (protocol.Outcome, error, error) {
	current, found := ws.Lookup(m.EntityID)
	if !found || current.Kind != "baby" {
		return rejection(m, "rejected", "entity_not_found", nil), nil, nil
	}
	baby, err := current.Entity()
	if err != nil {
		return protocol.Outcome{}, nil, err
	}
	if !current.Live() {
		return rejection(m, "superseded", "entity_deleted", &baby), nil, nil
	}
	client, err := protocol.NormalizeTimestamp(m.ClientMutationTime)
	if err != nil {
		return protocol.Outcome{}, nil, err
	}
	tombstoneOf := func(r Row) protocol.CanonicalEntity {
		accepted := ws.accepted
		return protocol.CanonicalEntity{EntityKind: r.Kind, Payload: nil, Meta: protocol.EntityMeta{ID: r.ID, FamilyID: ws.cmd.Grant.FamilyID,
			EntityVersion: r.EntityVersion + 1, CreatorAccountID: r.CreatorAccountID, LastEditorAccountID: ws.cmd.Grant.AccountID,
			ClientMutationTime: client, AcceptedAt: accepted, TombstonedAt: &accepted}}
	}
	root := tombstoneOf(current)
	rootRow, err := Encode(root, &current, ws.change(m, 1, nil))
	if err != nil {
		return protocol.Outcome{}, nil, err
	}
	fits, err := ws.fitsMinimumPage(rootRow)
	if err != nil {
		return protocol.Outcome{}, nil, err
	}
	if !fits {
		return rejection(m, "rejected", "mutation_too_large", nil), nil, nil
	}
	children := ws.cascadeTargets(m.EntityID)
	if err := ws.claim(uint64(1 + len(children))); err != nil {
		return protocol.Outcome{}, err, nil
	}
	if err := ws.write(&current, rootRow); err != nil {
		return protocol.Outcome{}, nil, err
	}
	for i := range children {
		child := children[i]
		row, err := Encode(tombstoneOf(child), &child, ws.change(m, 1, nil))
		if err != nil {
			return protocol.Outcome{}, nil, err
		}
		if err := ws.write(&child, row); err != nil {
			return protocol.Outcome{}, nil, err
		}
	}
	return ws.outcomeAt(m, "applied", nil, root, nil, ws.sequence), nil, nil
}

// cascadeTargets is every live child of baby in its current state, in
// cascade order: the rows this batch read or wrote (a row created earlier
// in the batch included), then the fetched live rows of the baby the batch
// has not touched.
func (ws *workingSet) cascadeTargets(baby string) []Row {
	child := func(r Row) bool { return r.Live() && r.Kind != "baby" && r.BabyID != nil && *r.BabyID == baby }
	var targets []Row
	for _, r := range ws.rows {
		if child(r) {
			targets = append(targets, r)
		}
	}
	for _, r := range ws.cascade {
		if _, current := ws.rows[r.ID]; !current && child(r) {
			targets = append(targets, r)
		}
	}
	slices.SortFunc(targets, func(x, y Row) int {
		if d := cascadeKindOrder[x.Kind] - cascadeKindOrder[y.Kind]; d != 0 {
			return d
		}
		return strings.Compare(x.ID, y.ID)
	})
	return targets
}
