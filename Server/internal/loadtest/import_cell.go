package loadtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"babytracker/server/internal/protocol"
)

// Import scenario shape (the plan's 20 imports of 2,000 records).
const (
	importCount   = 20
	importRecords = 2000
	importChunk   = 1000
	importBatch   = 100
	// Routes of the import cell.
	RouteImportResolve = "import_resolve"
	RouteImportWrite   = "import_write"
)

// importStagingTTL is how long the server keeps an unfinished import
// before it expires (BT_IMPORT_STAGING_TTL_HOURS, which the load stack
// leaves at the protocol's default, 168 h). An expired import no longer
// counts against the account's one unfinished import.
var importStagingTTL = time.Duration(protocol.DefaultLimits().ImportStagingTTLHours) * time.Hour

// importExtras: 20 owners of distinct families each import 2,000 bottle
// feeds for a new baby (preserve_local_ids): two manifest chunks (the baby
// on the first), the final one completing the mapping, then the records in
// batches of 100, started over the first half of the measurement. A 429 is
// waited out per Retry-After and the import resumes, as the iOS importer
// does (sync-mutation-device allows 1,200 a minute, so a 2,000-record
// import pauses once).
//
// The cell survives the runs before it, as a phone survives a crash in
// the middle of an import:
//   - an import is checkpointed (the device's PendingImport) before its
//     first chunk and cleared once its records apply; an owner holding one
//     resumes it first, resending its frozen chunks and mutations, then
//     imports as the cell does;
//   - an owner whose account holds an unfinished import this fleet cannot
//     resume (no checkpoint names it) is refused the new import's chunk 0
//     import_unfinished_limit: that answer is import_leftover (expected),
//     the owner is passed over until the import's staging TTL ends it, and
//     the cell imports with the next owner. No fresh seed is needed.
func importExtras(g *generator) []extraClass {
	candidates := importCandidates(g)
	n := min(importCount, len(candidates))
	window := g.cfg.Measure / 2
	var ops, next atomic.Int64
	stats := &importStats{}
	return []extraClass{{class: "import", rate: float64(n) / window.Seconds(), length: window,
		op: func(ctx context.Context, intended time.Time) {
			if int(ops.Add(1)) > n {
				return
			}
			stats.started.Add(1)
			for {
				i := int(next.Add(1)) - 1
				if i >= len(candidates) {
					return
				}
				switch g.importWith(ctx, candidates[i], intended, stats) {
				case importDone:
					stats.completed.Add(1)
					return
				case importFailed:
					return
				}
				stats.leftovers.Add(1)
				intended = time.Now()
			}
		},
		report: func() {
			g.cell("import", map[string]any{"imports": n, "started": stats.started.Load(), "completed": stats.completed.Load(),
				"records_each": importRecords, "pauses": stats.pauses.Load(),
				"paused_seconds": time.Duration(stats.pausedNanos.Load()).Seconds(),
				"resumed":        stats.resumed.Load(), "leftover_owners": stats.leftovers.Load()})
		}}}
}

// importCandidates is the cell's owners in the order it asks them: owners
// of distinct families by the seed, less those passed over for an
// unfinished import they cannot resume (ImportBlockedUntil).
func importCandidates(g *generator) []*device {
	now := time.Now()
	var out []*device
	for _, d := range distinctOwners(g, len(g.devices), 0x1a0) {
		if until := d.state().ImportBlockedUntil; until == nil || !until.After(now) {
			out = append(out, d)
		}
	}
	return out
}

// importStats are the import cell's figures.
type importStats struct {
	started, completed, pauses atomic.Int64
	pausedNanos                atomic.Int64
	resumed, leftovers         atomic.Int64
}

// maxImportPauses bounds the 429 pauses one import request waits out.
const maxImportPauses = 30

// importSend sends one import request; a 429 is recorded as import_paused,
// waited out per Retry-After (at least a second) and the request resent.
func (g *generator) importSend(ctx context.Context, route string, intended time.Time, stats *importStats,
	send func() (result, string)) bool {
	races := 0
	for pauses := 0; ; pauses++ {
		res, outcome := send()
		g.noteSessionWait(route, res.SessionWait)
		intended = intended.Add(res.SessionWait)
		if res.lostRace() && races < refreshRetries && ctx.Err() == nil {
			// A lost race: recorded under its code and retried per
			// Retry-After, as every other route does.
			races++
			g.record(route, intended, res.Outcome)
			wait := retryDelay(res)
			if !sleepFor(ctx, wait) {
				return false
			}
			intended = time.Now()
			continue
		}
		if res.Outcome == OutcomeRateLimited && pauses < maxImportPauses {
			g.record(route, intended, OutcomeImportPaused)
			wait := max(res.RetryAfter, time.Second)
			stats.pauses.Add(1)
			stats.pausedNanos.Add(int64(wait))
			if !sleepFor(ctx, wait) {
				return false
			}
			intended = time.Now()
			continue
		}
		g.record(route, intended, outcome)
		return res.ok() && outcome == OutcomeOK
	}
}

// distinctOwners is n owner devices of distinct families, by the seed.
func distinctOwners(g *generator, n int, stream uint64) []*device {
	seen := map[string]bool{}
	var out []*device
	for _, i := range newLockedRand(g.cfg.Seed, stream).rngPerm(len(g.devices)) {
		d := g.devices[i]
		if d.Role == "owner" && !seen[d.FamilyID] && len(out) < n {
			seen[d.FamilyID] = true
			out = append(out, d)
		}
	}
	return out
}

// importPlan is one import as the client sends it: the resolve chunks of
// its manifest, then the records' mutations.
type importPlan struct {
	importID  string
	chunks    []protocol.ImportResolveRequest
	mutations []protocol.Mutation
}

// newImportPlan is the import importID frozen at frozenAt: importRecords
// bottle feeds for one new baby (preserve_local_ids), in chunks of
// importChunk. Every id and time in it is a function of the two, so a
// later run rebuilds the very chunks (a replay the resolver accepts) and
// mutations (already_applied) from the checkpoint.
func newImportPlan(importID string, frozenAt time.Time) (importPlan, error) {
	plan := importPlan{importID: importID}
	id := func(label string, i int) string {
		sum := sha256.Sum256(fmt.Appendf(nil, "baby-tracker-load/import\x00%s\x00%s\x00%d", importID, label, i))
		return uuidV4(sum[:16])
	}
	baby := id("baby", 0)
	now := frozenAt.UTC()
	plan.mutations = make([]protocol.Mutation, importRecords)
	manifest := make([]protocol.RecordManifest, importRecords)
	for i := range plan.mutations {
		started := now.Add(-time.Duration(importRecords-i) * 3 * time.Hour)
		payload := map[string]any{"baby_id": baby, "kind": "bottle", "started_at": stamp(started), "ended_at": nil,
			"time_zone_id": loadTimeZone, "note": nil, "components": []any{map[string]any{"id": id("component", i), "kind": "bottle",
				"volume_ml": 60 + i%120, "contents": "formula", "seconds": nil, "started_at": nil, "ended_at": nil}},
			"feeding_session_id": nil}
		m, err := buildImportMutation(id("record", i), importID, now, payload)
		if err != nil {
			return plan, err
		}
		m.MutationID = id("mutation", i)
		hash, err := protocol.CanonicalContentHash("feed", m.Payload)
		if err != nil {
			return plan, err
		}
		plan.mutations[i] = m
		manifest[i] = protocol.RecordManifest{RecordID: m.EntityID, EntityKind: "feed", LocalBabyID: baby, ContentSHA256: hex.EncodeToString(hash[:])}
	}
	// Chunk 0 freezes the baby candidates; every later chunk sends an
	// empty list (the resolver refuses any other with
	// import_manifest_changed, and null as invalid_request).
	babies := []protocol.ImportBaby{{LocalBabyID: baby, Name: "Imported " + baby[:8], BirthDate: now.AddDate(0, -6, 0).Format("2006-01-02")}}
	for chunk := 0; chunk*importChunk < importRecords; chunk++ {
		end := min((chunk+1)*importChunk, importRecords)
		plan.chunks = append(plan.chunks, protocol.ImportResolveRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: capabilitiesImport},
			ImportID: plan.importID, ImportMode: "preserve_local_ids", NormalizationVersion: "nfc-trim-fold-nfc-v1",
			Babies: babies, Records: manifest[chunk*importChunk : end], TotalRecordCount: importRecords, ChunkIndex: uint32(chunk),
			Finalize: end == importRecords})
		babies = []protocol.ImportBaby{}
	}
	return plan, nil
}

// importResult is how one owner's turn in the cell ended.
type importResult int

const (
	importFailed   importResult = iota
	importDone                  // the import's records all applied
	importLeftover              // the account holds an unfinished import this fleet cannot resume
)

// importRecords is one owner's turn: it reports whether its import
// completed.
func (g *generator) importRecords(ctx context.Context, d *device, intended time.Time, stats *importStats) bool {
	return g.importWith(ctx, d, intended, stats) == importDone
}

// importWith resumes d's unfinished import, if it has one still within the
// staging TTL (an older one has expired on the server), then imports anew.
func (g *generator) importWith(ctx context.Context, d *device, intended time.Time, stats *importStats) importResult {
	if p := d.state().PendingImport; p != nil {
		if time.Since(p.FrozenAt) >= importStagingTTL {
			d.finishImport(p.ImportID)
		} else {
			stats.resumed.Add(1)
			if r := g.runImport(ctx, d, intended, stats, *p); r != importDone {
				return r
			}
			intended = time.Now()
		}
	}
	p := PendingImport{ImportID: newUUID(), FrozenAt: time.Now().UTC().Truncate(time.Microsecond)}
	d.mu.Lock()
	d.PendingImport = &p
	d.mu.Unlock()
	return g.runImport(ctx, d, intended, stats, p)
}

// finishImport clears d's checkpointed import if it is still importID.
func (d *device) finishImport(importID string) {
	d.mu.Lock()
	if d.PendingImport != nil && d.PendingImport.ImportID == importID {
		d.PendingImport = nil
	}
	d.mu.Unlock()
}

// runImport sends the frozen import p: its chunks, then its records.
func (g *generator) runImport(ctx context.Context, d *device, intended time.Time, stats *importStats, p PendingImport) importResult {
	plan, err := newImportPlan(p.ImportID, p.FrozenAt)
	if err != nil {
		g.record(RouteImportResolve, intended, OutcomeProtocol)
		return importFailed
	}
	for _, body := range plan.chunks {
		leftover := false
		ok := g.importSend(ctx, RouteImportResolve, intended, stats, func() (result, string) {
			var answer protocol.ImportResolveResponse
			res := g.c.do(ctx, request{method: http.MethodPost, path: "/v1/family/import/resolve", device: d, session: d,
				capabilities: capabilitiesImport, body: body}, &answer)
			outcome := settled(res, false)
			switch {
			case body.ChunkIndex == 0 && res.Code == "import_unfinished_limit":
				// The import was never admitted (a checkpointed one whose
				// chunk 0 was lost is admitted now): another unfinished
				// import holds the account's one slot.
				outcome, leftover = OutcomeImportLeftover, true
			case res.ok() && body.Finalize && !answer.MappingComplete:
				// Conflicts an owner must decide: the records would be refused.
				outcome = OutcomeImportUnmapped
			}
			return res, outcome
		})
		if leftover {
			until := time.Now().Add(importStagingTTL)
			d.mu.Lock()
			d.ImportBlockedUntil = &until
			d.mu.Unlock()
			d.finishImport(p.ImportID)
			return importLeftover
		}
		if !ok {
			return importFailed
		}
		intended = time.Now()
	}
	for start := 0; start < importRecords; start += importBatch {
		batch := plan.mutations[start:min(start+importBatch, importRecords)]
		ok := g.importSend(ctx, RouteImportWrite, intended, stats, func() (result, string) {
			response, res := g.c.syncAs(ctx, d, syncBody(capabilitiesSync, nil, batch, nil))
			outcome := settled(res, false)
			if res.ok() {
				for _, o := range response.Outcomes {
					if o.Status != "applied" && o.Status != "already_applied" || o.CanonicalEntity == nil {
						outcome = OutcomeRejected
						continue
					}
					if o.CanonicalEntity.Meta.FamilyID != d.FamilyID {
						outcome = OutcomeCrossFamily
						continue
					}
					g.ledgerEntity(d.FamilyID, *o.CanonicalEntity)
				}
			}
			return res, outcome
		})
		if !ok {
			return importFailed
		}
		intended = time.Now()
	}
	d.finishImport(p.ImportID)
	return importDone
}

func buildImportMutation(entityID, importID string, now time.Time, payload map[string]any) (protocol.Mutation, error) {
	m, err := buildMutation(entityID, "feed", "create", 0, now, payload)
	if err != nil {
		return m, err
	}
	m.ImportID = &importID
	return m, nil
}
