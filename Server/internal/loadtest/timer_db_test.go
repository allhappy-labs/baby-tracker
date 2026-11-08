package loadtest

import (
	"testing"

	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

// The generator's timer lifecycle on a real schema: a timer it starts, it
// can stop, as a phone does. A feed timer's stop names the running side's
// part (the server refuses a running side's seconds with no part id as
// invalid_timer_time: the soak's 25 sync_write rejections).
func TestGeneratorTimerStartsAndStopsApply(t *testing.T) {
	for _, kind := range []string{"feed", "sleep"} {
		t.Run(kind, func(t *testing.T) {
			h := testkit.New(t)
			createBaby(t, h)
			g := testGenerator(t, newFakeV1(t), 1)
			d := g.writers[0]
			d.BabyIDs = []string{fixtureBaby}
			plan := func(operation string) *plannedMutation {
				for range 1000 {
					p := g.planTimer(d)
					if p == nil {
						continue
					}
					var timerKind string
					switch payload := p.m.Payload.(type) {
					case protocol.TimerStartPayload:
						timerKind = payload.TimerKind
					case protocol.TimerStopPayload:
						for key, ref := range g.families[d.FamilyID].timers {
							if ref.id == p.m.EntityID {
								timerKind = key[len(fixtureBaby)+1:]
							}
						}
					}
					if p.m.Operation == operation && timerKind == kind {
						return p
					}
					p.release()
				}
				t.Fatalf("no %s %s planned", kind, operation)
				return nil
			}
			start := plan("timer_start")
			start.m.MutationID = h.CommandID()
			start.applied(*applyOne(t, h, start.m).CanonicalEntity)
			stop := plan("timer_stop")
			stop.m.MutationID = h.CommandID()
			applyOne(t, h, stop.m)
		})
	}
}
