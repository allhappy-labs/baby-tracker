package liveactivity

import (
	"errors"
	"sort"
	"time"
	"unicode/utf8"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
)

const MaximumSummaryParts = 8

// MaximumDisplayNameBytes bounds escaped JSON as well as UTF-8. UUIDs, typed
// state, revision and the APNs start envelope then fit comfortably in 4 KiB.
const MaximumDisplayNameBytes = 96

type DevicePreferences struct{ FeedBreakMinutes int }
type DesiredCard struct {
	Scope     ActivityScope
	BabyName  string
	TimerID   *string
	SessionID *string
	Phase     string
	Content   ContentState
	StaleAt   *time.Time
	ExpiresAt *time.Time
	Sequence  uint64
	MayStart  bool
	CanWrite  bool
}

// DisplayName truncates on Unicode scalar boundaries. The same input always
// has the same bytes; even JSON's worst-case HTML escaping remains bounded.
func DisplayName(name string) string {
	if len(name) <= MaximumDisplayNameBytes {
		return name
	}
	n := MaximumDisplayNameBytes - 3
	for n > 0 && !utf8.RuneStart(name[n]) {
		n--
	}
	return name[:n] + "…"
}

// Project is independent of database, transport, generation and delivery state.
// Missing/deleted babies and expired breaks naturally reconcile to no cards.
func Project(s CareSnapshot, preferences DevicePreferences, now time.Time) ([]DesiredCard, error) {
	cards := []DesiredCard{}
	if s.Baby.Meta.TombstonedAt != nil || s.Baby.Payload == nil {
		return cards, nil
	}
	baby, ok := s.Baby.Payload.(protocol.BabyPayload)
	if !ok {
		return nil, errors.New("invalid baby snapshot")
	}
	minutes := preferences.FeedBreakMinutes
	if minutes < 5 || minutes > 120 || minutes%5 != 0 {
		return nil, errors.New("invalid feed break preference")
	}
	base := func(kind string) DesiredCard {
		return DesiredCard{Scope: ActivityScope{ServerInstanceID: s.Cursor.ServerInstanceID, RecoveryEpoch: s.Cursor.RecoveryEpoch, FamilyID: s.Cursor.FamilyID, BabyID: s.Baby.Meta.ID, KindRaw: kind}, BabyName: DisplayName(baby.Name), Sequence: s.Cursor.Sequence, CanWrite: access.RoleMayWrite(s.Grant.Role)}
	}
	for _, entity := range s.Timers {
		if entity.Meta.TombstonedAt != nil {
			continue
		}
		timer, ok := entity.Payload.(protocol.TimerStartPayload)
		if !ok {
			return nil, errors.New("invalid timer snapshot")
		}
		started := timer.StartedAt
		if timer.CurrentBottleStartedAt != nil {
			started = *timer.CurrentBottleStartedAt
		}
		instant, err := time.Parse(time.RFC3339Nano, started)
		if err != nil {
			return nil, err
		}
		// A running sleep's clock counts the sleep it continues
		// (CareSnapshot.EarlierAsleepSeconds); a feed, or a sleep that
		// continues none, counts only itself.
		if timer.TimerKind == "sleep" && timer.SessionID != nil {
			instant = instant.Add(-time.Duration(s.EarlierAsleepSeconds) * time.Second)
		}
		card := base(timer.TimerKind)
		id := entity.Meta.ID
		card.TimerID = &id
		card.SessionID = timer.SessionID
		card.Phase = "running"
		card.MayStart = true
		card.Content = ContentState{StartedAt: foundationTime(instant), ActivityKind: timerActivityKind(timer), Phase: card.Phase, Parts: []PartLabel{}}
		cards = append(cards, card)
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].Scope.KindRaw < cards[j].Scope.KindRaw })
	// Both active kinds survive; either running kind suppresses a feed break.
	if len(cards) > 0 {
		return cards, nil
	}
	latest := s.LastMilk
	if latest == nil {
		for i := range s.SessionMembers {
			e := &s.SessionMembers[i]
			p, ok := e.Payload.(protocol.FeedPayload)
			if !ok || e.Meta.TombstonedAt != nil || p.Kind == "solids" {
				continue
			}
			if latest == nil || feedEnd(*latest).Before(feedEnd(*e)) || (feedEnd(*latest).Equal(feedEnd(*e)) && effectiveSession(*latest) < effectiveSession(*e)) {
				latest = e
			}
		}
	}
	if latest == nil {
		return cards, nil
	}
	sessionID := effectiveSession(*latest)
	ended := feedEnd(*latest)
	deadline := ended.Add(time.Duration(minutes) * time.Minute)
	if now.After(deadline) {
		return cards, nil
	}
	members := append([]protocol.CanonicalEntity(nil), s.SessionMembers...)
	sort.Slice(members, func(i, j int) bool {
		a := members[i].Payload.(protocol.FeedPayload)
		b := members[j].Payload.(protocol.FeedPayload)
		ta, _ := time.Parse(time.RFC3339Nano, a.StartedAt)
		tb, _ := time.Parse(time.RFC3339Nano, b.StartedAt)
		if ta.Equal(tb) {
			return members[i].Meta.ID < members[j].Meta.ID
		}
		return ta.Before(tb)
	})
	parts := []PartLabel{}
	for _, member := range members {
		if member.Meta.TombstonedAt != nil || effectiveSession(member) != sessionID {
			continue
		}
		for _, c := range member.Payload.(protocol.FeedPayload).Components {
			if c.Kind != "breast" && c.Kind != "bottle" {
				continue
			}
			seconds := uint64(0)
			if c.Seconds != nil {
				seconds = uint64(*c.Seconds)
			}
			parts = append(parts, PartLabel{Side: c.Side, Seconds: seconds})
		}
	}
	total := uint64(len(parts))
	if s.TotalSessionParts > total {
		total = s.TotalSessionParts
	}
	if len(parts) > MaximumSummaryParts {
		parts = parts[len(parts)-MaximumSummaryParts:]
	}
	kind := "timedBottle"
	if len(parts) > 0 && parts[len(parts)-1].Side != nil {
		if *parts[len(parts)-1].Side == "left" {
			kind = "breastLeft"
		} else {
			kind = "breastRight"
		}
	}
	suggested := "left"
	if s.LastBreast != nil {
		p := s.LastBreast.Payload.(protocol.FeedPayload)
		for i := len(p.Components) - 1; i >= 0; i-- {
			if p.Components[i].Kind == "breast" && p.Components[i].Side != nil {
				if *p.Components[i].Side == "left" {
					suggested = "right"
				}
				break
			}
		}
	}
	card := base("feed")
	card.SessionID = &sessionID
	card.Phase = "feedBreak"
	card.StaleAt = &deadline
	// Swift's exactly-at-limit instant remains actionable. Reconcile immediately
	// after it, never retire at the inclusive boundary.
	expiry := deadline.Add(time.Millisecond)
	card.ExpiresAt = &expiry
	end := foundationTime(deadline)
	card.Content = ContentState{StartedAt: foundationTime(ended), ActivityKind: kind, Phase: card.Phase, BreakEndsAt: &end, Parts: parts, SuggestedSide: &suggested, OmittedParts: total - uint64(len(parts))}
	return append(cards, card), nil
}
func foundationTime(t time.Time) float64 {
	return float64(t.Unix()) + float64(t.Nanosecond())/1e9 - FoundationReferenceUnixSeconds
}
func effectiveSession(e protocol.CanonicalEntity) string {
	p := e.Payload.(protocol.FeedPayload)
	if p.FeedingSessionID != nil {
		return *p.FeedingSessionID
	}
	return e.Meta.ID
}

func feedEnd(e protocol.CanonicalEntity) time.Time {
	p := e.Payload.(protocol.FeedPayload)
	v := p.StartedAt
	if p.EndedAt != nil {
		v = *p.EndedAt
	}
	t, _ := time.Parse(time.RFC3339Nano, v)
	return t
}
func timerActivityKind(t protocol.TimerStartPayload) string {
	if t.TimerKind == "sleep" {
		return "sleep"
	}
	if t.CurrentBottleStartedAt != nil {
		return "timedBottle"
	}
	if t.CurrentSide != nil {
		if *t.CurrentSide == "left" {
			return "breastLeft"
		}
		return "breastRight"
	}
	if len(t.CompletedComponents) > 0 {
		c := t.CompletedComponents[len(t.CompletedComponents)-1]
		if c.Kind == "bottle" && c.Contents != nil {
			switch *c.Contents {
			case "breast_milk":
				return "breastMilkBottle"
			case "formula":
				return "formulaBottle"
			case "mixed":
				return "mixedBottle"
			}
		}
	}
	return "paused"
}
