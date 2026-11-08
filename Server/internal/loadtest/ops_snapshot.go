package loadtest

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"babytracker/server/internal/protocol"
)

// snapshotPicks bounds the draws for a device whose account has no active
// snapshot session.
const snapshotPicks = 8

// opSnapshot is one arrival of the snapshot class: a device of an account
// with no active snapshot session pages a snapshot to the end, then pulls
// from its high water.
func (g *generator) opSnapshot(ctx context.Context, intended time.Time) {
	for range snapshotPicks {
		d := g.randomDevice(g.pullers)
		if d != nil && g.accounts[d.AccountID].tryStartSnapshot(time.Now()) {
			g.snapshotAndPull(ctx, d, intended, RouteSyncPull)
			return
		}
	}
	g.skip("snapshot_no_idle_account", intended)
}

// snapshotAndPull pages GET /v1/sync/snapshot to the end (snapshot_page
// requests: the first measured from intended, each later one from the
// previous page's completion), then pulls from the snapshot's high water.
// A tracked device seeds its map from it. It reports whether both
// finished.
func (g *generator) snapshotAndPull(ctx context.Context, d *device, intended time.Time, pullRoute string) bool {
	renewed, ok := g.renewBeforeUse(ctx, d, intended)
	if !ok {
		g.skip(skipAccessNotRenewed, intended)
		return false
	}
	intended = intended.Add(renewed)
	release, waited := d.holdSession()
	g.noteSessionWait(RouteSnapshot, waited)
	highWater, complete, expired := g.snapshotHeld(ctx, d, intended.Add(waited))
	release()
	if expired {
		g.expired(ctx, d)
	}
	if !complete {
		return false
	}
	// The pull starts from the high water afresh (no page token), so it
	// takes the session on its own.
	complete = g.pullFrom(ctx, d, highWater, time.Now(), pullRoute)
	if complete && d.tracked != nil {
		d.tracked.markSeeded()
	}
	return complete
}

// snapshotHeld pages the snapshot to its end; the caller holds d's session.
// It reports the snapshot's high water, whether it reached the last page
// and whether its access had expired.
func (g *generator) snapshotHeld(ctx context.Context, d *device, intended time.Time) (highWater uint64, complete, expired bool) {
	var token *string
	races := 0
	for {
		path := "/v1/sync/snapshot"
		if token != nil {
			path += "?page_token=" + url.QueryEscape(*token)
		}
		page, res := g.snapshotPage(ctx, d, path)
		retrying := res.lostRace() && races < refreshRetries && ctx.Err() == nil
		g.record(RouteSnapshot, intended, settled(res, retrying))
		if retrying {
			races++
			wait := retryDelay(res)
			intended = time.Now().Add(wait)
			if !sleepFor(ctx, wait) {
				return 0, false, false
			}
			continue
		}
		if !res.ok() {
			return 0, false, res.Outcome == OutcomeAccessExpired
		}
		if d.tracked != nil {
			for _, entity := range page.entities {
				d.tracked.apply(entity)
			}
		}
		if page.next == nil {
			return page.highWater, true, false
		}
		token = page.next
		intended = time.Now()
	}
}

// snapped is what a snapshot page leaves a device: the high water, the
// next page token and, for a tracked device only, the entities.
type snapped struct {
	highWater uint64
	next      *string
	entities  []protocol.CanonicalEntity
}

// snapshotPage sends one snapshot page request (path) as d and checks the
// answer's family: in full (protocol.SnapshotResponse) for a tracked
// device, which keeps the entities for its map; as a snapshotPage, the
// same outcomes without decoding the payloads, for any other.
func (g *generator) snapshotPage(ctx context.Context, d *device, path string) (snapped, result) {
	req := request{method: http.MethodGet, path: path, device: d, bearer: d.access(), capabilities: capabilitiesSnapshot}
	if d.tracked != nil {
		var page protocol.SnapshotResponse
		res := g.c.do(ctx, req, &page)
		if res.ok() && !snapshotOwnFamily(d, page) {
			res.Outcome = OutcomeCrossFamily
		}
		return snapped{highWater: page.HighWater.Sequence, next: page.NextPageToken, entities: page.Entities}, res
	}
	var page snapshotPage
	res := g.c.do(ctx, req, &page)
	if res.ok() && !page.ownFamily(d.FamilyID) {
		res.Outcome = OutcomeCrossFamily
	}
	return snapped{highWater: page.HighWater.Sequence, next: page.NextPageToken}, res
}

func snapshotOwnFamily(d *device, page protocol.SnapshotResponse) bool {
	if page.HighWater.FamilyID != d.FamilyID {
		return false
	}
	for _, entity := range page.Entities {
		if entity.Meta.FamilyID != d.FamilyID {
			return false
		}
	}
	return true
}

// seedTracked fills a tracked device's map at warm-up: a snapshot when its
// account has no active one, else a pull from 0 (which returns every
// entity at its current version too).
func (g *generator) seedTracked(ctx context.Context, d *device, intended time.Time) {
	if g.accounts[d.AccountID].tryStartSnapshot(time.Now()) {
		g.snapshotAndPull(ctx, d, intended, RouteSyncPull)
		return
	}
	if g.pullFrom(ctx, d, 0, intended, RouteSyncPull) {
		d.tracked.markSeeded()
	}
}
