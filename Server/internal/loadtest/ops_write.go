package loadtest

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"babytracker/server/internal/protocol"
)

// familyState is what the generator knows of a family across its devices:
// the live timers (one per baby and kind, the server's one-live-timer rule)
// and each baby's growth profile version.
type familyState struct {
	mu     sync.Mutex
	timers map[string]*timerRef // babyID/kind
	growth map[string]*growthRef
	wake   familyWake // the hub-wake delay's acknowledgements and hint marks
}

type timerRef struct {
	id        string
	version   uint64
	startedAt time.Time
	busy      bool
	blocked   bool // a running timer the generator cannot stop (an open bottle, or refused)
	side      bool // a feed timer with a breast side running: its stop names that side's part
	pending   bool // the start is in flight
}

type growthRef struct {
	version uint64
	busy    bool
}

func newFamilyState() *familyState {
	return &familyState{timers: map[string]*timerRef{}, growth: map[string]*growthRef{}}
}

const loadTimeZone = "Europe/Zurich"

// plannedMutation is one mutation of a write and what its answer changes.
type plannedMutation struct {
	m protocol.Mutation
	// applied runs for an applied or already_applied outcome with its
	// canonical entity; superseded for a superseded one (it reports
	// whether that is the expected answer); release when the mutation did
	// not apply.
	applied    func(protocol.CanonicalEntity)
	superseded func(protocol.Outcome) bool
	release    func()
}

// opWrite is one arrival of the write class: a uniformly drawn owner or
// caregiver device writes 1 mutation, or 2–3 for MultiMutation of them.
func (g *generator) opWrite(ctx context.Context, intended time.Time) {
	d := g.randomDevice(g.bgWriters)
	if d == nil {
		return
	}
	g.write(ctx, d, intended, RouteSyncWrite, g.mutationCount())
}

func (g *generator) mutationCount() int {
	if g.rng.Float64() < DefaultWriteMix(g.withPump).MultiMutation {
		return 2 + g.rng.IntN(2)
	}
	return 1
}

// write sends n planned mutations as one POST /v1/sync with d's cursor.
// Every applied (or, on the retry, already_applied) outcome is ledgered
// once. A failed request is retried once after a second (Retry-After for a
// lost race, up to three times) with the same body, so the same mutation
// IDs: the lost-acknowledgement retry, answered applied or already_applied.
//
// The device renews its session first when it must (renewBeforeUse). It
// returns the outcome it recorded last ("" when nothing was sent).
func (g *generator) write(ctx context.Context, d *device, intended time.Time, route string, n int) string {
	renewed, ok := g.renewBeforeUse(ctx, d, intended)
	if !ok {
		g.skip(skipAccessNotRenewed, intended)
		return ""
	}
	intended = intended.Add(renewed)
	plans := make([]*plannedMutation, 0, n)
	for len(plans) < n {
		p := g.planMutation(d)
		if p == nil {
			break
		}
		plans = append(plans, p)
	}
	if len(plans) == 0 {
		g.skip("write_nothing_to_plan", intended)
		return ""
	}
	mutations := make([]protocol.Mutation, len(plans))
	capabilities := capabilitiesSync
	for i, p := range plans {
		mutations[i] = p.m
		if p.m.EntityKind == "timer" {
			capabilities = capabilitiesSyncTimer
		}
	}
	d.mu.Lock()
	cursor := g.cursor(d.FamilyID, d.Cursor)
	d.mu.Unlock()
	body := syncBody(capabilities, &cursor, mutations, nil)

	var response protocol.SyncResponse
	var res result
	retried, races := false, 0
	for {
		response, res = g.c.syncAs(ctx, d, body)
		g.noteSessionWait(route, res.SessionWait)
		intended = intended.Add(res.SessionWait)
		if res.ok() {
			break
		}
		var wait time.Duration
		retrying := ctx.Err() == nil
		switch {
		case res.lostRace() && races < refreshRetries:
			races++
			wait = retryDelay(res)
		case !res.lostRace() && retryable(res) && !retried:
			retried = true
			wait = time.Second
		default:
			retrying = false
		}
		g.record(route, intended, settled(res, retrying))
		if !retrying {
			if res.Outcome == OutcomeAccessExpired {
				g.expired(ctx, d)
			}
			for _, p := range plans {
				p.release()
			}
			return settled(res, false)
		}
		intended = time.Now().Add(wait)
		if !sleepFor(ctx, wait) {
			for _, p := range plans {
				p.release()
			}
			return settled(res, false)
		}
	}
	acked := time.Now()
	outcome, rejections := g.applyOutcomes(d, plans, response)
	if outcome == OutcomeOK && !g.ownFamily(d, response) {
		outcome = OutcomeCrossFamily
	}
	g.record(route, intended, outcome, rejections...)
	if outcome != OutcomeCrossFamily {
		// After the record: the measured span is the request's alone.
		g.noteAck(d, appliedSequence(response), acked)
	}
	if outcome == OutcomeCrossFamily {
		return outcome
	}
	d.tracked.applyChanges(response.Changes)
	if response.NextPageToken == nil {
		d.advanceCursor(response.HighWater.Sequence)
		d.tracked.complete(response.HighWater.Sequence)
	}
	return outcome
}

// applyOutcomes ledgers every applied outcome and settles each plan. It
// returns the request's outcome: ok, or rejected_mutation when any
// mutation was refused (or answered unexpectedly), with each refused
// mutation's reason.
func (g *generator) applyOutcomes(d *device, plans []*plannedMutation, response protocol.SyncResponse) (string, []string) {
	var rejections []string
	byID := make(map[string]protocol.Outcome, len(response.Outcomes))
	for _, o := range response.Outcomes {
		byID[o.MutationID] = o
	}
	outcome := OutcomeOK
	for _, p := range plans {
		o, ok := byID[p.m.MutationID]
		// A replay (already_applied) answers as the original did: a retried
		// timer start first superseded is superseded, not applied.
		status := o.Status
		if status == "already_applied" && o.OriginalStatus != nil {
			status = *o.OriginalStatus
		}
		switch {
		case !ok:
			outcome = OutcomeProtocol
			p.release()
		case (status == "applied" || status == "already_applied") && o.CanonicalEntity != nil:
			if o.CanonicalEntity.Meta.FamilyID != d.FamilyID {
				p.release()
				continue // the request is classed cross_family
			}
			g.ledgerEntity(d.FamilyID, *o.CanonicalEntity)
			for _, related := range o.RelatedEntities {
				if related.Meta.FamilyID == d.FamilyID {
					g.ledgerEntity(d.FamilyID, related)
				}
			}
			p.applied(*o.CanonicalEntity)
		case status == "superseded" && p.superseded != nil && p.superseded(o):
			// The expected answer (a timer start meeting the baby's live
			// timer): nothing was written, nothing is ledgered.
		default:
			if outcome == OutcomeOK {
				outcome = OutcomeRejected
			}
			rejections = append(rejections, rejectionReason(status, o.Code))
			p.release()
		}
	}
	return outcome, rejections
}

// rejectionReason is a refused mutation's "<status>:<code>" (the status
// alone without a code). A status or code that is not a protocol token
// (lowercase letters, digits and underscores, at most 64) is "other", so
// the result's keys stay few whatever the server answers.
func rejectionReason(status string, code *string) string {
	token := func(s string) string {
		if s == "" || len(s) > 64 {
			return "other"
		}
		for _, c := range s {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
				return "other"
			}
		}
		return s
	}
	if code == nil {
		return token(status)
	}
	return token(status) + ":" + token(*code)
}

// appliedSequence is the highest family sequence the response's applied
// outcomes reached (0: none applied; a replay's is not new).
func appliedSequence(response protocol.SyncResponse) uint64 {
	var seq uint64
	for _, o := range response.Outcomes {
		if o.Status == "applied" && o.Cursor != nil {
			seq = max(seq, o.Cursor.Sequence)
		}
	}
	return seq
}

// ledgerEntity appends an acknowledged entity to the ledger.
func (g *generator) ledgerEntity(familyID string, entity protocol.CanonicalEntity) {
	sum, err := entityWireSHA256(entity)
	if err != nil {
		g.ledgerErrs.Add(1)
		return
	}
	if g.ledger.Append(LedgerEntry{FamilyID: familyID, EntityID: entity.Meta.ID, Kind: entity.EntityKind,
		Version: entity.Meta.EntityVersion, Deleted: entity.Meta.TombstonedAt != nil, WireSHA256: sum}) != nil {
		// The audit cannot see this acknowledged write: the run fails on it.
		g.ledgerErrs.Add(1)
		return
	}
	g.ledgered.Add(1)
}

// drawKind draws a kind from the write mix.
func (g *generator) drawKind() string {
	mix := DefaultWriteMix(g.withPump)
	x := g.rng.Float64()
	for _, k := range mix.Kinds {
		if x < k.Share {
			return k.Kind
		}
		x -= k.Share
	}
	return mix.Kinds[0].Kind
}

// drawOperation draws create, update or delete from the write mix.
func (g *generator) drawOperation() string {
	mix := DefaultWriteMix(g.withPump)
	x := g.rng.Float64()
	switch {
	case x < mix.Create:
		return "create"
	case x < mix.Create+mix.Update:
		return "update"
	}
	return "delete"
}

// planMutation draws one mutation for d. Updates and deletes change an
// entity d created this run (another in-flight mutation never touches the
// same entity); with none, they are creates. Timers start, or stop the
// baby's live timer; growth profiles update the baby's profile.
func (g *generator) planMutation(d *device) *plannedMutation {
	kind := g.drawKind()
	if kind != "pump" && len(d.BabyIDs) == 0 {
		if !g.withPump {
			return nil
		}
		kind = "pump"
	}
	switch kind {
	case "timer":
		if p := g.planTimer(d); p != nil {
			return p
		}
		kind = "sleep"
	case "growth_profile":
		if p := g.planGrowth(d); p != nil {
			return p
		}
		kind = "diaper"
	}
	operation := g.drawOperation()
	var own *ownEntity
	if operation != "create" {
		own = d.claimOwn(kind, g.rng)
		if own == nil {
			operation = "create"
		}
	}
	now := time.Now().UTC()
	if operation == "create" {
		baby := ""
		if kind != "pump" {
			baby = d.BabyIDs[g.rng.IntN(len(d.BabyIDs))]
		}
		own = &ownEntity{id: newUUID(), kind: kind, babyID: baby, loggedAt: stamp(now), busy: true}
	}
	var payload map[string]any
	observed := uint64(0)
	if operation != "create" {
		observed = own.version
	}
	if operation == "delete" {
		payload = map[string]any{}
		if kind != "pump" {
			payload["baby_id"] = own.babyID
		}
	} else {
		payload = g.recordPayload(kind, own, now)
	}
	m := mustMutation(own.id, kind, operation, observed, now, payload)
	return &plannedMutation{m: m,
		applied: func(entity protocol.CanonicalEntity) {
			d.mu.Lock()
			defer d.mu.Unlock()
			own.version, own.busy = entity.Meta.EntityVersion, false
			own.deleted = entity.Meta.TombstonedAt != nil
			if operation == "create" {
				d.entities = append(d.entities, own)
			}
		},
		release: func() {
			d.mu.Lock()
			own.busy = false
			d.mu.Unlock()
		}}
}

// claimOwn picks a live, idle entity of kind that d created and marks it
// busy; nil when there is none.
func (d *device) claimOwn(kind string, rng *lockedRand) *ownEntity {
	d.mu.Lock()
	defer d.mu.Unlock()
	var candidates []*ownEntity
	for _, e := range d.entities {
		if e.kind == kind && !e.busy && !e.deleted {
			candidates = append(candidates, e)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	e := candidates[rng.IntN(len(candidates))]
	e.busy = true
	return e
}

// recordPayload is a care record's payload in the shape of
// api/fixtures/v1/<kind>.json, timed around now.
func (g *generator) recordPayload(kind string, own *ownEntity, now time.Time) map[string]any {
	b := own.babyID
	switch kind {
	case "feed":
		if g.rng.IntN(2) == 0 {
			return map[string]any{"baby_id": b, "kind": "breast", "started_at": stamp(now.Add(-20 * time.Minute)),
				"ended_at": stamp(now.Add(-10 * time.Minute)), "time_zone_id": loadTimeZone, "note": nil,
				"components": []any{
					map[string]any{"id": newUUID(), "kind": "breast", "side": "left", "seconds": 300},
					map[string]any{"id": newUUID(), "kind": "breast", "side": "right", "seconds": 300}},
				"feeding_session_id": nil}
		}
		return map[string]any{"baby_id": b, "kind": "bottle", "started_at": stamp(now.Add(-15 * time.Minute)), "ended_at": nil,
			"time_zone_id": loadTimeZone, "note": nil,
			"components": []any{map[string]any{"id": newUUID(), "kind": "bottle", "volume_ml": 60 + 10*g.rng.IntN(13),
				"contents": "formula", "seconds": nil, "started_at": nil, "ended_at": nil}},
			"feeding_session_id": nil}
	case "diaper":
		return map[string]any{"baby_id": b, "started_at": stamp(now), "ended_at": nil, "time_zone_id": loadTimeZone, "note": nil,
			"contents": "mixed", "consistency": "soft", "color": "mustard"}
	case "sleep":
		return map[string]any{"baby_id": b, "started_at": stamp(now.Add(-time.Hour)), "ended_at": stamp(now), "time_zone_id": loadTimeZone,
			"note": nil, "kind_override": "nap", "sleep_session_id": nil}
	case "supplement":
		return map[string]any{"baby_id": b, "started_at": stamp(now), "time_zone_id": loadTimeZone, "note": nil, "name": "Vitamin D",
			"logged_at": own.loggedAt}
	case "measurement":
		return map[string]any{"baby_id": b, "measured_at": stamp(now.Add(-time.Minute)), "logged_at": own.loggedAt,
			"time_zone_id": loadTimeZone, "weight_kg": 7.125, "length_cm": 65.5, "head_circumference_cm": nil, "length_position": "lying"}
	case "immunization":
		return map[string]any{"baby_id": b, "given_at": stamp(now.Add(-time.Hour)), "time_zone_id": loadTimeZone,
			"vaccine": "Prevenar 13", "dose": 1 + g.rng.IntN(3), "note": nil}
	case "pump":
		return map[string]any{"started_at": stamp(now.Add(-20 * time.Minute)), "ended_at": stamp(now), "time_zone_id": loadTimeZone,
			"side": "both", "volume_ml": 90.5, "note": nil}
	}
	panic("loadtest: no payload for kind " + kind)
}

// planTimer starts a timer on one of d's babies, or stops the live timer
// the family holds for that baby and kind. nil when that timer is busy or
// cannot be stopped.
func (g *generator) planTimer(d *device) *plannedMutation {
	baby := d.BabyIDs[g.rng.IntN(len(d.BabyIDs))]
	timerKind := "sleep"
	if g.rng.IntN(2) == 0 {
		timerKind = "feed"
	}
	key := baby + "/" + timerKind
	fam := g.families[d.FamilyID]
	now := time.Now().UTC()
	fam.mu.Lock()
	defer fam.mu.Unlock()
	if ref := fam.timers[key]; ref != nil {
		if ref.busy || ref.blocked || ref.pending {
			return nil
		}
		ref.busy = true
		stopped := now
		if earliest := ref.startedAt.Add(2 * time.Second); stopped.Before(earliest) {
			stopped = earliest
		}
		payload := map[string]any{"baby_id": baby, "completed_record_id": newUUID(), "stopped_at": stamp(stopped),
			"note": nil, "sleep_kind_override": nil}
		// As the phone stops a feed (BabyStore+Feeding.swift): the running
		// side's seconds become a part whose id the client assigns (the
		// server refuses them unnamed as invalid_timer_time), and taps under
		// 30 seconds are cleaned up.
		if timerKind == "feed" {
			payload["cleanup_short_breast_segments"] = true
			if ref.side {
				payload["component_id"] = newUUID()
			}
		}
		m := mustMutation(ref.id, "timer", "timer_stop", ref.version, now, payload)
		return &plannedMutation{m: m,
			applied: func(protocol.CanonicalEntity) {
				fam.mu.Lock()
				if fam.timers[key] == ref {
					delete(fam.timers, key)
				}
				fam.mu.Unlock()
			},
			// Refused or lost: forget the timer. The next start on this
			// baby meets the live timer (if any) and adopts it again.
			release: func() {
				fam.mu.Lock()
				if fam.timers[key] == ref {
					delete(fam.timers, key)
				}
				fam.mu.Unlock()
			}}
	}
	ref := &timerRef{id: newUUID(), startedAt: now, pending: true, side: timerKind == "feed"}
	fam.timers[key] = ref
	payload := map[string]any{"baby_id": baby, "timer_kind": timerKind, "started_at": stamp(now), "time_zone_id": loadTimeZone,
		"completed_components": []any{}, "current_side": nil, "current_side_started_at": nil, "session_id": nil}
	if timerKind == "feed" {
		payload["current_side"], payload["current_side_started_at"] = "left", stamp(now)
	}
	m := mustMutation(ref.id, "timer", "timer_start", 0, now, payload)
	return &plannedMutation{m: m,
		applied: func(entity protocol.CanonicalEntity) {
			fam.mu.Lock()
			ref.version, ref.pending = entity.Meta.EntityVersion, false
			fam.mu.Unlock()
		},
		// The baby already has a live timer of the kind (another device's,
		// or one the seed left running): the start is superseded by it, as
		// a phone's would be, and the family adopts it.
		superseded: func(o protocol.Outcome) bool {
			if o.CanonicalEntity == nil || o.CanonicalEntity.EntityKind != "timer" || o.CanonicalEntity.Meta.TombstonedAt != nil {
				return false
			}
			payload, ok := o.CanonicalEntity.Payload.(protocol.TimerStartPayload)
			if !ok {
				return false
			}
			started, _ := time.Parse(time.RFC3339Nano, payload.StartedAt)
			fam.mu.Lock()
			defer fam.mu.Unlock()
			adopted := &timerRef{id: o.CanonicalEntity.Meta.ID, version: o.CanonicalEntity.Meta.EntityVersion, startedAt: started,
				side: payload.CurrentSide != nil}
			adopted.blocked = payload.CurrentBottleStartedAt != nil ||
				payload.TimerKind == "feed" && payload.CurrentSide == nil && len(payload.CompletedComponents) == 0
			fam.timers[key] = adopted
			return true
		},
		release: func() {
			fam.mu.Lock()
			if fam.timers[key] == ref {
				delete(fam.timers, key)
			}
			fam.mu.Unlock()
		}}
}

// planGrowth updates one of d's babies' growth profile (the seed gave each
// baby one, at protocol.GrowthProfileID). The observed version is the last
// one the generator saw (1 before any): a stale one is last-writer-wins.
func (g *generator) planGrowth(d *device) *plannedMutation {
	baby := d.BabyIDs[g.rng.IntN(len(d.BabyIDs))]
	fam := g.families[d.FamilyID]
	fam.mu.Lock()
	ref := fam.growth[baby]
	if ref == nil {
		ref = &growthRef{version: 1}
		fam.growth[baby] = ref
	}
	if ref.busy {
		fam.mu.Unlock()
		return nil
	}
	ref.busy = true
	observed := ref.version
	fam.mu.Unlock()
	sex := "girls"
	if g.rng.IntN(2) == 0 {
		sex = "boys"
	}
	now := time.Now().UTC()
	m := mustMutation(protocol.GrowthProfileID(baby), "growth_profile", "update", observed, now,
		map[string]any{"baby_id": baby, "reference_sex": sex, "expected_due_date": nil})
	return &plannedMutation{m: m,
		applied: func(entity protocol.CanonicalEntity) {
			fam.mu.Lock()
			ref.version = max(ref.version, entity.Meta.EntityVersion)
			ref.busy = false
			fam.mu.Unlock()
		},
		release: func() {
			fam.mu.Lock()
			ref.busy = false
			fam.mu.Unlock()
		}}
}

// stamp is a wire timestamp (microseconds, UTC), the server's normalized
// form.
func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000Z") }

// buildMutation builds a mutation from its fixture-shaped JSON through the
// protocol's own decoder, so the generator sends only what a client may.
func buildMutation(entityID, kind, operation string, observed uint64, now time.Time, payload map[string]any) (protocol.Mutation, error) {
	raw, err := json.Marshal(map[string]any{"mutation_id": newUUID(), "entity_id": entityID, "entity_kind": kind,
		"operation": operation, "payload_version": protocol.CurrentPayloadVersion(kind), "observed_entity_version": observed,
		"client_mutation_time": stamp(now), "import_id": nil, "payload": payload})
	if err != nil {
		return protocol.Mutation{}, err
	}
	return protocol.DecodeMutation(raw)
}

func mustMutation(entityID, kind, operation string, observed uint64, now time.Time, payload map[string]any) protocol.Mutation {
	m, err := buildMutation(entityID, kind, operation, observed, now, payload)
	if err != nil {
		panic(fmt.Sprintf("loadtest: the generator built an invalid %s %s: %v", kind, operation, err))
	}
	return m
}
