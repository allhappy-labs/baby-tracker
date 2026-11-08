package loadtest

import (
	"context"
	"math"
	"net/http"
	"time"

	"babytracker/server/internal/protocol"
)

// rewindToZero is a cursor lag that rewinds to the family's first change.
const rewindToZero = math.MaxUint64

// drawLag draws a pull's cursor rewind from DefaultCursorMix: none (at its
// own head), 1–50 changes, 51–1,000 changes, or back to 0.
func (g *generator) drawLag() uint64 {
	mix := DefaultCursorMix()
	x := g.rng.Float64()
	switch {
	case x < mix.AtHead:
		return 0
	case x < mix.AtHead+mix.SmallLag:
		return 1 + uint64(g.rng.IntN(50))
	case x < mix.AtHead+mix.SmallLag+mix.LargeLag:
		return 51 + uint64(g.rng.IntN(950))
	}
	return rewindToZero
}

// opPull is one arrival of the pull class: a uniformly drawn device pulls
// from its cursor rewound by a lag drawn from the cursor mix.
func (g *generator) opPull(ctx context.Context, intended time.Time) {
	d := g.randomDevice(g.pullers)
	if d == nil {
		return
	}
	g.pullPages(ctx, d, intended, g.drawLag(), RouteSyncPull)
}

// cursor is the family cursor at sequence in the fleet's server domain.
func (g *generator) cursor(familyID string, sequence uint64) protocol.Cursor {
	return protocol.Cursor{ServerInstanceID: g.header.ServerInstanceID, RecoveryEpoch: g.header.RecoveryEpoch,
		FamilyID: familyID, Sequence: sequence}
}

// pullPages pulls d from its cursor rewound by rewind (never below 0) to
// the family's head. It reports whether the pull reached the last page.
func (g *generator) pullPages(ctx context.Context, d *device, intended time.Time, rewind uint64, route string) bool {
	d.mu.Lock()
	from := d.Cursor
	d.mu.Unlock()
	if rewind >= from {
		from = 0
	} else {
		from -= rewind
	}
	return g.pullFrom(ctx, d, from, intended, route)
}

// pullFrom pages from sequence until next_page_token is nil. Every page is
// one request of route: the first measured from intended, each later one
// from the previous page's completion (paging is a client loop). Every
// change must belong to d's family. The cursor advances only from the
// final page. The device renews its session first when it must
// (renewBeforeUse); a pull whose renewal fails is not sent.
func (g *generator) pullFrom(ctx context.Context, d *device, sequence uint64, intended time.Time, route string) bool {
	renewed, ok := g.renewBeforeUse(ctx, d, intended)
	if !ok {
		g.skip(skipAccessNotRenewed, intended)
		return false
	}
	intended = intended.Add(renewed)
	release, waited := d.holdSession()
	g.noteSessionWait(route, waited)
	complete, expired := g.pullHeld(ctx, d, sequence, intended.Add(waited), route)
	release()
	if expired {
		g.expired(ctx, d)
	}
	return complete
}

// pullHeld is pullFrom's paging; the caller holds d's session. It reports
// whether the pull completed and whether its access had expired.
func (g *generator) pullHeld(ctx context.Context, d *device, sequence uint64, intended time.Time, route string) (complete, expired bool) {
	cursor := g.cursor(d.FamilyID, sequence)
	var token *string
	races := 0
	for {
		body := syncBody(capabilitiesSync, &cursor, nil, nil)
		if token != nil {
			body = syncBody(capabilitiesSync, nil, nil, token)
		}
		page, res := g.pullPage(ctx, d, body)
		retrying := res.lostRace() && races < refreshRetries && ctx.Err() == nil
		g.record(route, intended, settled(res, retrying))
		if retrying {
			races++
			wait := retryDelay(res)
			intended = time.Now().Add(wait)
			if !sleepFor(ctx, wait) {
				return false, false
			}
			continue
		}
		if !res.ok() {
			return false, res.Outcome == OutcomeAccessExpired
		}
		d.tracked.applyChanges(page.changes)
		if page.next == nil {
			d.advanceCursor(page.highWater)
			d.tracked.complete(page.highWater)
			return true, false
		}
		token = page.next
		intended = time.Now()
	}
}

// pulled is what a pull page leaves a device: the high water, the next
// page token and, for a tracked device only, the changes.
type pulled struct {
	highWater uint64
	next      *string
	changes   []protocol.JournalChange
}

// pullPage sends one pull page as d and checks the answer's family. A
// tracked device decodes the page in full (protocol.SyncResponse) and keeps
// its changes for its map; any other reads it as a pullPage, the same
// outcomes without decoding the payloads.
func (g *generator) pullPage(ctx context.Context, d *device, body protocol.SyncRequest) (pulled, result) {
	if d.tracked != nil {
		response, res := g.c.sync(ctx, d, d.access(), body)
		if res.ok() && !g.ownFamily(d, response) {
			res.Outcome = OutcomeCrossFamily
		}
		return pulled{highWater: response.HighWater.Sequence, next: response.NextPageToken, changes: response.Changes}, res
	}
	var page pullPage
	res := g.c.do(ctx, request{method: http.MethodPost, path: "/v1/sync", device: d, bearer: d.access(),
		capabilities: body.Capabilities, body: body}, &page)
	if res.ok() && !page.ownFamily(d.FamilyID) {
		res.Outcome = OutcomeCrossFamily
	}
	return pulled{highWater: page.HighWater.Sequence, next: page.NextPageToken}, res
}

// ownFamily reports whether every change and outcome entity of a sync
// answer belongs to d's family.
func (g *generator) ownFamily(d *device, response protocol.SyncResponse) bool {
	if response.HighWater.FamilyID != d.FamilyID {
		return false
	}
	for _, change := range response.Changes {
		if change.SnapshotOrTombstone.Meta.FamilyID != d.FamilyID || change.Cursor.FamilyID != d.FamilyID {
			return false
		}
	}
	for _, outcome := range response.Outcomes {
		if outcome.CanonicalEntity != nil && outcome.CanonicalEntity.Meta.FamilyID != d.FamilyID {
			return false
		}
		for _, related := range outcome.RelatedEntities {
			if related.Meta.FamilyID != d.FamilyID {
				return false
			}
		}
	}
	return true
}

// applyChanges records a page's changes in a tracked device's map.
func (t *trackedState) applyChanges(changes []protocol.JournalChange) {
	if t == nil {
		return
	}
	for _, change := range changes {
		t.apply(change.SnapshotOrTombstone)
	}
}
