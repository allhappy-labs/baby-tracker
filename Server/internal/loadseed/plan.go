//go:build identitytest

package loadseed

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"time"

	"babytracker/server/internal/protocol"
)

// FamilyPlan is one family's seeded shape and care history, planned from
// the seed and the family's index alone (planFamily).
type FamilyPlan struct {
	Index int
	// Viewer: the fourth device is a viewer's (else the caregiver's second).
	Viewer bool
	// MCP: the owner holds an MCP integration grant.
	MCP      bool
	TimeZone string
	Days     int
	Babies   []BabyPlan
	// RunningPumpID is the family's running pump, if it has one.
	RunningPumpID string
	// Changes are the care mutations in acceptance order.
	Changes  []PlannedChange
	entities int
}

// BabyPlan is a planned baby. RunningTimerID and RunningTimerKind name its
// running timer, if it has one.
type BabyPlan struct {
	ID, Name, BirthDate              string
	RunningTimerID, RunningTimerKind string
}

// PlannedChange is one care mutation as a phone sends it: by the writer
// Actor (0 the owner, 1 the caregiver) from its device Slot (0 or 1,
// modulo the devices it has), accepted at AcceptedAt.
type PlannedChange struct {
	Mutation   protocol.Mutation
	Actor      int
	Slot       int
	AcceptedAt time.Time
}

// Entities is the number of care_entities rows the plan leaves.
func (p FamilyPlan) Entities() int { return p.entities }

// Mutations is the plan's mutations in acceptance order.
func (p FamilyPlan) Mutations() []protocol.Mutation {
	out := make([]protocol.Mutation, len(p.Changes))
	for i, c := range p.Changes {
		out[i] = c.Mutation
	}
	return out
}

var (
	timeZones   = []string{"Europe/Zurich", "Europe/Kyiv", "Europe/London", "America/New_York", "America/Los_Angeles", "Asia/Tokyo", "Australia/Sydney"}
	babyNames   = []string{"Olivia", "Noah", "Emma", "Liam", "Mia", "Leon", "Sofia", "Elias", "Anna", "Luca", "Mila", "Theo", "Zoe", "Max", "Ivy", "Ben"}
	babyIcons   = []string{"boy.1", "boy.2", "boy.3", "boy.4", "girl.1", "girl.2", "girl.3", "girl.4"}
	supplements = []string{"Vitamin D", "Iron drops", "Probiotic"}
	foods       = []string{"banana", "carrot", "apple", "oatmeal", "yogurt", "pear", "sweet potato", "rice"}
	edits       = []string{"Edited", "Fixed the time", "Corrected"}
)

const (
	payloadTimeLayout = "2006-01-02T15:04:05.000000Z"
	// historyMargin ends the tracked history this long before AsOf; the
	// running timers and pump start inside it.
	historyMargin = 10 * time.Minute
)

// planner builds one family's plan from its own random stream.
type planner struct {
	spec       Spec
	index      int
	r          *rand.Rand
	zone       *time.Location
	zoneName   string
	start, end time.Time
	entities   uint64
	mutations  uint64
	components uint64
	changes    []PlannedChange
}

// familyShape draws the family's devices from a stream of its own, so the
// authority stage reads them without planning the history. The MCP grants
// are a quota, not a draw: ceil(Families × MCPShare) families spread evenly,
// so a small seed (the 100-family smoke) still has the integrations its
// model's MCP calls need.
func familyShape(spec Spec, i int) (viewer, mcp bool) {
	r := rand.New(rand.NewPCG(spec.Seed^0x5eed_5a9e_0000_0001, uint64(i)))
	mcp = math.Ceil(float64(i+1)*spec.MCPShare) > math.Ceil(float64(i)*spec.MCPShare)
	return r.Float64() < spec.ViewerShare, mcp
}

// planFamily plans family i of spec, deterministically from
// rand.NewPCG(spec.Seed, i): its babies, history length, every care
// mutation and its acceptance time.
func planFamily(spec Spec, i int) FamilyPlan {
	p := &planner{spec: spec, index: i, r: rand.New(rand.NewPCG(spec.Seed, uint64(i)))}
	plan := FamilyPlan{Index: i}
	plan.Viewer, plan.MCP = familyShape(spec, i)
	p.zoneName = timeZones[p.r.IntN(len(timeZones))]
	zone, err := time.LoadLocation(p.zoneName)
	if err != nil {
		panic(fmt.Sprintf("loadseed: time zone %s: %v", p.zoneName, err))
	}
	p.zone, plan.TimeZone = zone, p.zoneName
	plan.Days = p.drawDays()
	p.end = spec.AsOf.Add(-historyMargin)
	p.start = p.end.Add(-time.Duration(plan.Days) * 24 * time.Hour)
	babies := 1
	if p.r.Float64() < spec.TwinShare {
		babies = 2
	}
	// Tracking starts at birth or up to two weeks after it; twins share a
	// birthday.
	born := p.localMidnight(p.start.Add(-time.Duration(p.r.IntN(15)) * 24 * time.Hour))
	for b := 0; b < babies; b++ {
		plan.Babies = append(plan.Babies, p.baby(b, born))
	}
	for b := range plan.Babies {
		p.babyHistory(&plan.Babies[b], born)
	}
	if p.r.Float64() < spec.History.PumpFamilyShare {
		p.pumps()
	}
	if p.r.Float64() < spec.History.RunningPumpShare {
		plan.RunningPumpID = p.runningPump()
	}
	slices.SortStableFunc(p.changes, func(a, b PlannedChange) int { return a.AcceptedAt.Compare(b.AcceptedAt) })
	plan.Changes, plan.entities = p.changes, int(p.entities)
	return plan
}

func (p *planner) drawDays() int {
	days := p.spec.History.sortedDays()
	total := 0.0
	for _, d := range days {
		total += p.spec.History.DaysWeights[d]
	}
	x := p.r.Float64() * total
	for _, d := range days {
		x -= p.spec.History.DaysWeights[d]
		if x < 0 {
			return d
		}
	}
	return days[len(days)-1]
}

func (p *planner) localMidnight(at time.Time) time.Time {
	local := at.In(p.zone)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, p.zone)
}

// count is rate×days as a whole number: the integer part, plus one with
// the fraction's probability.
func (p *planner) count(rate float64, days float64) int {
	expected := rate * days
	n := int(expected)
	if p.r.Float64() < expected-float64(n) {
		n++
	}
	return n
}

// between is a uniform instant in [from, to), to the millisecond.
func (p *planner) between(from, to time.Time) time.Time {
	span := to.Sub(from).Milliseconds()
	if span <= 0 {
		return from.Truncate(time.Millisecond)
	}
	return from.Add(time.Duration(p.r.Int64N(span)) * time.Millisecond).Truncate(time.Millisecond)
}

func (p *planner) minutes(from, to int) time.Duration {
	return time.Duration(from+p.r.IntN(to-from+1)) * time.Minute
}

func (p *planner) pick(values []string) string { return values[p.r.IntN(len(values))] }

func (p *planner) nextEntity() string {
	id := entityID(p.spec.Seed, p.index, p.entities)
	p.entities++
	return id
}

func (p *planner) nextComponent() string {
	id := domainID(domainComponent, p.spec.Seed, p.index, p.components)
	p.components++
	return id
}

// stamp is at as a payload timestamp, in the normalized form the server
// decodes a phone's timestamp to (whole microseconds, UTC), so the
// planned mutation is the one the server holds after decoding.
func stamp(at time.Time) string { return at.UTC().Format(payloadTimeLayout) }

func ptr[T any](value T) *T { return &value }

// add plans one mutation sent at client (the phone's clock, to the
// millisecond) and accepted shortly after it.
func (p *planner) add(kind, operation, id string, observed uint64, client time.Time, payload protocol.Payload, actor, slot int) time.Time {
	m := protocol.Mutation{MutationID: domainID(domainMutation, p.spec.Seed, p.index, p.mutations), EntityID: id, EntityKind: kind,
		Operation: operation, PayloadVersion: protocol.CurrentPayloadVersion(kind), ObservedEntityVersion: observed,
		ClientMutationTime: stamp(client), Payload: payload}
	p.mutations++
	// The request reaches the server 80 ms to 3 s later, to the microsecond.
	accepted := client.Add(time.Duration(80_000+p.r.IntN(2_920_000)) * time.Microsecond).UTC()
	p.changes = append(p.changes, PlannedChange{Mutation: m, Actor: actor, Slot: slot, AcceptedAt: accepted})
	return accepted
}

// record plans a record's create and, by the profile's shares, its edits
// and deletion. edit returns the payload of the n-th edit (n from 1).
func (p *planner) record(kind string, babyID *string, client time.Time, payload protocol.Payload,
	edit func(protocol.Payload, int) protocol.Payload) {
	id := p.nextEntity()
	actor, slot := p.r.IntN(2), p.r.IntN(2)
	at := p.add(kind, "create", id, 0, client, payload, actor, slot)
	version := uint64(1)
	if edit != nil && p.r.Float64() < p.spec.History.UpdateShare {
		updates := 1 + p.r.IntN(3) // versions 2 to 4
		for n := 1; n <= updates; n++ {
			client := at.Add(p.minutes(2, 360)).Truncate(time.Millisecond)
			if !client.Before(p.end) {
				break
			}
			payload = edit(payload, n)
			at = p.add(kind, "update", id, version, client, payload, p.r.IntN(2), p.r.IntN(2))
			version++
		}
	}
	if p.r.Float64() < p.spec.History.TombstoneShare {
		client := at.Add(p.minutes(2, 720)).Truncate(time.Millisecond)
		if client.Before(p.end) {
			p.add(kind, "delete", id, version, client, protocol.DeletePayload{BabyID: babyID}, p.r.IntN(2), p.r.IntN(2))
		}
	}
}

func (p *planner) baby(b int, born time.Time) BabyPlan {
	baby := BabyPlan{ID: p.nextEntity(), Name: p.pick(babyNames), BirthDate: born.Format("2006-01-02")}
	payload := protocol.BabyPayload{Name: baby.Name, BirthDate: baby.BirthDate, SortOrder: uint32(b)}
	if p.r.IntN(2) == 0 {
		payload.Icon = ptr(p.pick(babyIcons))
	}
	// The owner adds the baby when tracking starts, then its growth profile.
	created := p.start.Add(-time.Hour + time.Duration(b)*time.Minute)
	p.add("baby", "create", baby.ID, 0, created, payload, 0, 0)
	sex := "girls"
	if p.r.IntN(2) == 0 {
		sex = "boys"
	}
	profile := protocol.GrowthProfilePayload{BabyID: baby.ID, ReferenceSex: sex}
	if p.r.IntN(3) == 0 {
		profile.ExpectedDueDate = ptr(born.AddDate(0, 0, p.r.IntN(15)-7).Format("2006-01-02"))
	}
	p.add("growth_profile", "create", protocol.GrowthProfileID(baby.ID), 0, created.Add(10*time.Minute), profile, 0, 0)
	p.entities++ // the growth profile's row (its ID is derived from the baby's)
	return baby
}

// babyHistory plans a baby's records over the history window, then its
// running timer.
func (p *planner) babyHistory(baby *BabyPlan, born time.Time) {
	h := p.spec.History
	days := p.end.Sub(p.start).Hours() / 24
	babyID := ptr(baby.ID)
	note := func(n int) *string { return ptr(edits[(n-1)%len(edits)]) }
	for range p.count(h.FeedsPerDay, days) {
		at := p.between(p.start, p.end.Add(-40*time.Minute))
		age := at.Sub(born).Hours() / 24
		feed := p.feed(baby.ID, at, age)
		client := at
		if feed.EndedAt != nil {
			client, _ = time.Parse(time.RFC3339Nano, *feed.EndedAt)
		}
		p.record("feed", babyID, client.Add(time.Duration(p.r.IntN(30))*time.Second), feed, func(payload protocol.Payload, n int) protocol.Payload {
			f := payload.(protocol.FeedPayload)
			f.Note = note(n)
			return f
		})
	}
	for range p.count(h.DiapersPerDay, days) {
		at := p.between(p.start, p.end)
		diaper := protocol.DiaperPayload{BabyID: baby.ID, StartedAt: stamp(at), TimeZoneID: p.zoneName}
		switch x := p.r.IntN(100); {
		case x < 50:
			diaper.Contents = "wet"
		case x < 70:
			diaper.Contents = "dirty"
		case x < 95:
			diaper.Contents = "mixed"
		default:
			diaper.Contents = "dry"
		}
		if diaper.Contents == "dirty" || diaper.Contents == "mixed" {
			diaper.Consistency = ptr(p.pick([]string{"soft", "runny", "firm"}))
			diaper.Color = ptr(p.pick([]string{"yellow", "mustard", "brown", "green"}))
		}
		p.record("diaper", babyID, at.Add(time.Duration(p.r.IntN(60))*time.Second), diaper, func(payload protocol.Payload, n int) protocol.Payload {
			d := payload.(protocol.DiaperPayload)
			d.Note = note(n)
			return d
		})
	}
	for range p.count(h.SleepsPerDay, days) {
		duration := p.minutes(20, 180)
		at := p.between(p.start, p.end.Add(-duration-time.Minute))
		sleep := protocol.SleepPayload{BabyID: baby.ID, StartedAt: stamp(at), EndedAt: ptr(stamp(at.Add(duration))), TimeZoneID: p.zoneName}
		switch x := p.r.IntN(100); {
		case x < 15:
			sleep.KindOverride = ptr("nap")
		case x < 30:
			sleep.KindOverride = ptr("night")
		}
		p.record("sleep", babyID, at.Add(duration).Add(time.Duration(p.r.IntN(30))*time.Second), sleep, func(payload protocol.Payload, n int) protocol.Payload {
			s := payload.(protocol.SleepPayload)
			s.Note = note(n)
			return s
		})
	}
	for range p.count(h.SupplementsPerDay, days) {
		at := p.between(p.start, p.end.Add(-time.Hour))
		logged := at.Add(p.minutes(0, 30)).Truncate(time.Millisecond)
		supplement := protocol.SupplementPayload{BabyID: baby.ID, StartedAt: stamp(at), TimeZoneID: p.zoneName, Name: p.pick(supplements), LoggedAt: stamp(logged)}
		p.record("supplement", babyID, logged, supplement, func(payload protocol.Payload, n int) protocol.Payload {
			s := payload.(protocol.SupplementPayload)
			s.Note = note(n)
			return s
		})
	}
	birthWeight := 3.0 + float64(p.r.IntN(80))/100
	for week := p.start.Add(time.Duration(p.r.IntN(24*60)) * time.Minute); week.Before(p.end.Add(-3 * time.Hour)); week = week.Add(MeasurementEveryDays * 24 * time.Hour) {
		at := week.Truncate(time.Millisecond)
		age := at.Sub(born).Hours() / 24
		weight := birthWeight + 0.03*min(age, 90) + 0.015*max(0, age-90)
		length := 50 + 0.1*min(age, 120) + 0.04*max(0, age-120)
		logged := at.Add(p.minutes(1, 120)).Truncate(time.Millisecond)
		measurement := protocol.MeasurementPayload{BabyID: baby.ID, MeasuredAt: stamp(at), LoggedAt: stamp(logged), TimeZoneID: p.zoneName,
			WeightKG: protocol.MustParseDecimal(fmt.Sprintf("%.2f", weight)), LengthCM: protocol.MustParseDecimal(fmt.Sprintf("%.1f", length)),
			LengthPosition: ptr("lying")}
		p.record("measurement", babyID, logged, measurement, func(payload protocol.Payload, n int) protocol.Payload {
			m := payload.(protocol.MeasurementPayload)
			m.WeightKG = protocol.MustParseDecimal(fmt.Sprintf("%.2f", weight+0.01*float64(n)))
			return m
		})
	}
	for _, dose := range immunizationSchedule {
		at := born.Add(time.Duration(dose.AgeDays)*24*time.Hour + p.minutes(8*60, 17*60)).Truncate(time.Millisecond)
		if at.Before(p.start) || !at.Before(p.end.Add(-time.Hour)) {
			continue
		}
		immunization := protocol.ImmunizationPayload{BabyID: baby.ID, GivenAt: stamp(at), TimeZoneID: p.zoneName, Vaccine: dose.Vaccine, Dose: ptr(dose.Dose)}
		p.record("immunization", babyID, at.Add(p.minutes(1, 60)), immunization, func(payload protocol.Payload, n int) protocol.Payload {
			i := payload.(protocol.ImmunizationPayload)
			i.Note = note(n)
			return i
		})
	}
	if p.r.Float64() < p.spec.History.RunningTimerShare {
		p.runningTimer(baby)
	}
}

// feed is a breast, bottle or solids feed at at, the baby age days old.
func (p *planner) feed(babyID string, at time.Time, age float64) protocol.FeedPayload {
	feed := protocol.FeedPayload{BabyID: babyID, StartedAt: stamp(at), TimeZoneID: p.zoneName}
	x := p.r.IntN(100)
	switch {
	case x < 15 && age > 150:
		feed.Kind = "solids"
		chosen := []string{p.pick(foods)}
		if p.r.IntN(2) == 0 {
			chosen = append(chosen, p.pick(foods))
		}
		feed.Components = []protocol.FeedComponent{{ID: p.nextComponent(), Kind: "solids", Foods: chosen,
			Amount: ptr(p.pick([]string{"taste", "some", "lots"}))}}
	case x < 50:
		feed.Kind = "bottle"
		volume := 60 + 10*p.r.IntN(13)
		feed.Components = []protocol.FeedComponent{{ID: p.nextComponent(), Kind: "bottle",
			VolumeML: protocol.MustParseDecimal(fmt.Sprint(volume)), Contents: ptr(p.pick([]string{"formula", "breast_milk", "mixed"}))}}
		if p.r.IntN(2) == 0 {
			feed.EndedAt = ptr(stamp(at.Add(p.minutes(5, 25))))
		}
	default:
		feed.Kind = "breast"
		total := 0
		sides := []string{"left", "right"}
		if p.r.IntN(2) == 0 {
			sides = []string{"right", "left"}
		}
		for s := 0; s < 1+p.r.IntN(2); s++ {
			seconds := uint32(300 + p.r.IntN(601))
			total += int(seconds)
			feed.Components = append(feed.Components, protocol.FeedComponent{ID: p.nextComponent(), Kind: "breast", Side: ptr(sides[s]), Seconds: ptr(seconds)})
		}
		feed.EndedAt = ptr(stamp(at.Add(time.Duration(total) * time.Second)))
	}
	return feed
}

func (p *planner) pumps() {
	days := p.end.Sub(p.start).Hours() / 24
	for range p.count(p.spec.History.PumpsPerDay, days) {
		duration := p.minutes(10, 30)
		at := p.between(p.start, p.end.Add(-duration-5*time.Minute))
		pump := protocol.PumpPayload{StartedAt: stamp(at), EndedAt: ptr(stamp(at.Add(duration))), TimeZoneID: p.zoneName,
			Side: p.pick([]string{"left", "right", "both"}), VolumeML: protocol.MustParseDecimal(fmt.Sprintf("%d.%d", 40+p.r.IntN(160), 5*p.r.IntN(2)))}
		p.record("pump", nil, at.Add(duration).Add(time.Duration(p.r.IntN(60))*time.Second), pump, func(payload protocol.Payload, n int) protocol.Payload {
			u := payload.(protocol.PumpPayload)
			u.Note = ptr(edits[(n-1)%len(edits)])
			return u
		})
	}
}

// runningTimer starts the baby's running feed or sleep timer in the last
// ninety minutes before AsOf.
func (p *planner) runningTimer(baby *BabyPlan) {
	at := p.spec.AsOf.Add(-p.minutes(5, 90)).Truncate(time.Millisecond)
	timer := protocol.TimerStartPayload{BabyID: baby.ID, StartedAt: stamp(at), TimeZoneID: p.zoneName, CompletedComponents: []protocol.FeedComponent{}}
	timer.TimerKind = "sleep"
	if p.r.IntN(10) < 6 {
		timer.TimerKind = "feed"
		timer.CurrentSide = ptr(p.pick([]string{"left", "right"}))
		timer.CurrentSideStartedAt = ptr(stamp(at))
	}
	baby.RunningTimerID, baby.RunningTimerKind = p.nextEntity(), timer.TimerKind
	p.add("timer", "timer_start", baby.RunningTimerID, 0, at, timer, p.r.IntN(2), p.r.IntN(2))
}

// runningPump starts the family's running pump in the last forty minutes
// before AsOf.
func (p *planner) runningPump() string {
	at := p.spec.AsOf.Add(-p.minutes(5, 40)).Truncate(time.Millisecond)
	id := p.nextEntity()
	p.add("pump", "create", id, 0, at, protocol.PumpPayload{StartedAt: stamp(at), TimeZoneID: p.zoneName, Side: "both"}, p.r.IntN(2), p.r.IntN(2))
	return id
}
