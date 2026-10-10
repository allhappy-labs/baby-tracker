package care

import (
	"fmt"
	"reflect"
	"time"

	"babytracker/server/internal/protocol"
)

// The timer rules are records/timers.go at 342973dc (ApplyTimer and everything it
// calls) decided against the working set: the baby, the timer, a baby's
// live timer of a kind, a stop's completion id and a stopped timer's
// completion are rows Fetch read or this batch wrote, never a query of
// their own.
//
// The legacy timer was read back from active_timers and timer_components
// (GetTimer), which normalized every timestamp the database stores: the
// start, the running side's and bottle's starts, and each part's bounds. The
// stored wire keeps the payload as it was written (the bottle start and part
// bounds as the phone sent them), so readTimer normalizes them as the
// database did; every outcome, comparison and change built from a stored
// timer then matches the legacy one byte for byte.

// storedTimer is a timer row read as the legacy GetTimer read it.
type storedTimer struct {
	row Row
	// entity is GetTimer's Canonical(): the stored payload, nil once the
	// timer has stopped (or was deleted).
	entity protocol.CanonicalEntity
	// payload is the stored payload while the timer runs; zero once it has
	// ended (row keeps its baby and kind).
	payload protocol.TimerStartPayload
}

func readTimer(r Row) (storedTimer, error) {
	entity, err := r.Entity()
	if err != nil {
		return storedTimer{}, err
	}
	t := storedTimer{row: r}
	if p, ok := entity.Payload.(protocol.TimerStartPayload); ok {
		if t.payload, err = asStoredTimer(p); err != nil {
			return storedTimer{}, err
		}
		entity.Payload = t.payload
	}
	t.entity = entity
	return t, nil
}

// NormalizeTimerEntity is a decoded stored entity as the legacy readers
// rendered it from the timer tables: a live timer's payload with every
// timestamp normalized to whole microseconds and no parts as an empty list
// (asStoredTimer). Any other entity, and a timer tombstone, is returned as
// it is. The familyread readers and the export archive's entity sections
// use it; pulls (and the export journal) serve the stored wire as written,
// as the legacy journal did. Legacy snapshot pages also rendered timers this
// way; care snapshot pages serve the stored wire.
func NormalizeTimerEntity(entity protocol.CanonicalEntity) (protocol.CanonicalEntity, error) {
	if entity.Meta.TombstonedAt != nil {
		return entity, nil
	}
	p, ok := entity.Payload.(protocol.TimerStartPayload)
	if !ok {
		return entity, nil
	}
	normalized, err := asStoredTimer(p)
	if err != nil {
		return protocol.CanonicalEntity{}, err
	}
	entity.Payload = normalized
	return entity, nil
}

// asStoredTimer is p as the database stored and read it back: every
// timestamp normalized to whole microseconds (protocol.NormalizeTimestamp,
// which rounds the decimal fraction half-even), and the parts an empty list
// when there are none.
//
// One exception is not reproduced. PostgreSQL rounds a fraction finer than a
// microsecond through a double (rint), so an exact tie with seven or more
// fraction digits can land 1 µs away from NormalizeTimestamp (for example
// 13.2514955 is .251495 there and .251496 here). The decoder normalizes the
// start and the running side's start itself, so only the fields it leaves as
// sent can differ: the bottle start, part bounds and a finished bottle's
// end. The phones send milliseconds, so such a tie never reaches the server
// from them; this deliberately does not emulate PostgreSQL's rounding.
func asStoredTimer(p protocol.TimerStartPayload) (protocol.TimerStartPayload, error) {
	var err error
	at := func(value string) string {
		normalized, parseErr := protocol.NormalizeTimestamp(value)
		if parseErr != nil && err == nil {
			err = fmt.Errorf("care: stored timer time %q: %w", value, parseErr)
		}
		return normalized
	}
	optional := func(value *string) *string {
		if value == nil {
			return nil
		}
		normalized := at(*value)
		return &normalized
	}
	p.StartedAt = at(p.StartedAt)
	p.CurrentSideStartedAt = optional(p.CurrentSideStartedAt)
	p.CurrentBottleStartedAt = optional(p.CurrentBottleStartedAt)
	parts := make([]protocol.FeedComponent, len(p.CompletedComponents))
	for i, part := range p.CompletedComponents {
		part.StartedAt, part.EndedAt = optional(part.StartedAt), optional(part.EndedAt)
		parts[i] = part
	}
	p.CompletedComponents = parts
	return p, err
}

// timerMutationBabyID is the baby a timer command names.
func timerMutationBabyID(m protocol.Mutation) string {
	switch p := m.Payload.(type) {
	case protocol.TimerStartPayload:
		return p.BabyID
	case protocol.TimerBottleFinishPayload:
		return p.BabyID
	case protocol.TimerStopPayload:
		return p.BabyID
	case protocol.TimerEditPayload:
		return p.BabyID
	}
	return ""
}

// timerMeta is the meta of a timer command's entity at version, kept by
// creator ("" is the actor), as the legacy timerMeta built it.
func (ws *workingSet) timerMeta(m protocol.Mutation, version uint64, creator string) (protocol.EntityMeta, error) {
	client, err := protocol.NormalizeTimestamp(m.ClientMutationTime)
	if err != nil {
		return protocol.EntityMeta{}, err
	}
	if creator == "" {
		creator = ws.cmd.Grant.AccountID
	}
	return protocol.EntityMeta{ID: m.EntityID, FamilyID: ws.cmd.Grant.FamilyID, EntityVersion: version, CreatorAccountID: creator,
		LastEditorAccountID: ws.cmd.Grant.AccountID, ClientMutationTime: client, AcceptedAt: ws.accepted}, nil
}

// change is the change envelope of m's offset-th change in this batch (1:
// the next sequence).
func (ws *workingSet) change(m protocol.Mutation, offset uint64, completed *string) Change {
	return Change{Seq: ws.sequence + offset, ActorAccountID: ws.cmd.Grant.AccountID, DeviceID: ws.cmd.DeviceID, Operation: m.Operation,
		AcceptedAt: ws.cmd.AcceptedAt, CompletedRecordID: completed}
}

// outcomeAt is an outcome carrying entity (and related, [] when nil) at
// the cursor sequence: the legacy timerOutcome, also the baby delete's outcome.
func (ws *workingSet) outcomeAt(m protocol.Mutation, status string, code *string, entity protocol.CanonicalEntity,
	related []protocol.CanonicalEntity, sequence uint64) protocol.Outcome {
	if related == nil {
		related = []protocol.CanonicalEntity{}
	}
	cursor := protocol.Cursor{ServerInstanceID: ws.cmd.Server.InstanceID, RecoveryEpoch: ws.cmd.Server.RecoveryEpoch,
		FamilyID: ws.cmd.Grant.FamilyID, Sequence: sequence}
	return protocol.Outcome{MutationID: m.MutationID, Status: status, CanonicalEntity: &entity, RelatedEntities: related,
		Cursor: &cursor, Code: code}
}

// applyTimer is the legacy ApplyTimer: the baby must be live, then the command.
func (ws *workingSet) applyTimer(m protocol.Mutation) (protocol.Outcome, error, error) {
	baby, ok := ws.Lookup(timerMutationBabyID(m))
	if !ok || baby.Kind != "baby" {
		return rejection(m, "rejected", "baby_not_found", nil), nil, nil
	}
	if !baby.Live() {
		entity, err := baby.Entity()
		if err != nil {
			return protocol.Outcome{}, nil, err
		}
		return rejection(m, "rejected", "baby_deleted", &entity), nil, nil
	}
	switch m.Operation {
	case "timer_start":
		return ws.startTimer(m)
	case "timer_finish_bottle", "timer_stop", "update":
		return ws.changeTimer(m)
	}
	return rejection(m, "rejected", "invalid_mutation", nil), nil, nil
}

// liveTimer is baby's live timer of kind (at most one: the one-live-timer
// rule), fetched or written by this batch.
func (ws *workingSet) liveTimer(babyID, kind string) (Row, bool) {
	for _, r := range ws.rows {
		if r.Kind == "timer" && r.Live() && r.BabyID != nil && *r.BabyID == babyID && r.TimerKind != nil && *r.TimerKind == kind {
			return r, true
		}
	}
	return Row{}, false
}

// startTimer is the legacy startTimer: the same start resent is superseded by
// its timer, another entity's id is refused, and the baby's live timer of
// the kind supersedes a new one.
func (ws *workingSet) startTimer(m protocol.Mutation) (protocol.Outcome, error, error) {
	payload := m.Payload.(protocol.TimerStartPayload)
	if current, found := ws.Lookup(m.EntityID); found {
		if current.Kind != "timer" {
			return rejection(m, "rejected", "entity_kind_collision", nil), nil, nil
		}
		existing, err := readTimer(current)
		if err != nil {
			return protocol.Outcome{}, nil, err
		}
		if current.Live() && reflect.DeepEqual(existing.payload, payload) {
			return ws.outcomeAt(m, "superseded", nil, existing.entity, nil, current.ChangedSeq), nil, nil
		}
		return rejection(m, "rejected", "entity_exists", nil), nil, nil
	}
	if live, ok := ws.liveTimer(payload.BabyID, payload.TimerKind); ok {
		existing, err := readTimer(live)
		if err != nil {
			return protocol.Outcome{}, nil, err
		}
		return ws.outcomeAt(m, "superseded", nil, existing.entity, nil, live.ChangedSeq), nil, nil
	}
	if payload.SessionID != nil && ws.sessionOwnerMismatch(payload.BabyID, payload.TimerKind, *payload.SessionID) {
		return rejection(m, "rejected", "record_parent_mismatch", nil), nil, nil
	}
	meta, err := ws.timerMeta(m, 1, "")
	if err != nil {
		return protocol.Outcome{}, nil, err
	}
	return ws.writeTimer(m, nil, protocol.CanonicalEntity{EntityKind: "timer", Meta: meta, Payload: payload}, false)
}

// writeTimer writes a running timer's new state (a start, a finished bottle,
// an edit) as the legacy startTimer and persistTimerChange did: the minimum
// page, the sequence, then the quota, which a timer change never escapes.
func (ws *workingSet) writeTimer(m protocol.Mutation, previous *Row, entity protocol.CanonicalEntity, overwrote bool) (protocol.Outcome, error, error) {
	row, err := Encode(entity, previous, ws.change(m, 1, nil))
	if err != nil {
		return protocol.Outcome{}, nil, err
	}
	fits, err := ws.fitsMinimumPage(row)
	if err != nil {
		return protocol.Outcome{}, nil, err
	}
	if !fits {
		return rejection(m, "rejected", "mutation_too_large", nil), nil, nil
	}
	if err := ws.claim(1); err != nil {
		return protocol.Outcome{}, err, nil
	}
	if !ws.admits(previous, row) {
		return protocol.Outcome{}, ErrQuotaExceeded, nil
	}
	if err := ws.write(previous, row); err != nil {
		return protocol.Outcome{}, nil, err
	}
	outcome := ws.outcomeAt(m, "applied", nil, entity, nil, row.ChangedSeq)
	outcome.OverwroteNewerVersion = overwrote
	return outcome, nil, nil
}

// changeTimer is the legacy changeTimer: a finish, a stop or an edit of a
// timer the family holds.
func (ws *workingSet) changeTimer(m protocol.Mutation) (protocol.Outcome, error, error) {
	current, found := ws.Lookup(m.EntityID)
	if !found || current.Kind != "timer" {
		return rejection(m, "rejected", "entity_not_found", nil), nil, nil
	}
	timer, err := readTimer(current)
	if err != nil {
		return protocol.Outcome{}, nil, err
	}
	if edit, ok := m.Payload.(protocol.TimerEditPayload); ok {
		return ws.editTimer(m, timer, edit)
	}
	entity := timer.entity
	if current.BabyID == nil || *current.BabyID != timerMutationBabyID(m) {
		return rejection(m, "rejected", "record_parent_mismatch", &entity), nil, nil
	}
	if !current.Live() {
		return ws.endedTimer(m, timer)
	}
	switch payload := m.Payload.(type) {
	case protocol.TimerBottleFinishPayload:
		p := timer.payload
		if p.TimerKind != "feed" || p.CurrentBottleStartedAt == nil {
			return rejection(m, "rejected", "invalid_timer_kind", &entity), nil, nil
		}
		started, startErr := parseTimerTime(*p.CurrentBottleStartedAt)
		finished, finishErr := parseTimerTime(payload.FinishedAt)
		seconds := int64(finished.Sub(started) / time.Second)
		if startErr != nil || finishErr != nil || seconds < 1 || seconds > 604800 || timerHasComponent(p.CompletedComponents, payload.ComponentID) {
			return rejection(m, "rejected", "invalid_timer_time", &entity), nil, nil
		}
		partSeconds := uint32(seconds)
		part := protocol.FeedComponent{ID: payload.ComponentID, Kind: "bottle", Seconds: &partSeconds, VolumeML: payload.VolumeML,
			Contents: payload.Contents, StartedAt: p.CurrentBottleStartedAt, EndedAt: &payload.FinishedAt}
		p.CompletedComponents = append(append([]protocol.FeedComponent{}, p.CompletedComponents...), part)
		p.CurrentBottleStartedAt = nil
		return ws.persistTimer(m, timer, p)
	case protocol.TimerStopPayload:
		if timer.payload.CurrentBottleStartedAt != nil {
			return rejection(m, "rejected", "active_bottle_unfinished", &entity), nil, nil
		}
		return ws.stopTimer(m, timer, payload)
	}
	return rejection(m, "rejected", "invalid_mutation", nil), nil, nil
}

// persistTimer is the legacy persistTimerChange: the running timer's next
// version with payload.
func (ws *workingSet) persistTimer(m protocol.Mutation, timer storedTimer, payload protocol.TimerStartPayload) (protocol.Outcome, error, error) {
	meta, err := ws.timerMeta(m, timer.row.EntityVersion+1, timer.row.CreatorAccountID)
	if err != nil {
		return protocol.Outcome{}, nil, err
	}
	entity := protocol.CanonicalEntity{EntityKind: "timer", Meta: meta, Payload: payload}
	return ws.writeTimer(m, &timer.row, entity, m.ObservedEntityVersion < meta.EntityVersion-1)
}

// editTimer is the legacy editTimer: a caregiver's correction of a running
// timer (a new start, new values for finished parts, a session), judged
// against the timer as the batch has left it. Every refusal carries the
// stored timer; an edit that changes nothing is applied at the timer's
// cursor and writes no change.
func (ws *workingSet) editTimer(m protocol.Mutation, timer storedTimer, edit protocol.TimerEditPayload) (protocol.Outcome, error, error) {
	refuse := func(code string) (protocol.Outcome, error, error) {
		entity := timer.entity
		return rejection(m, "rejected", code, &entity), nil, nil
	}
	if !timer.row.Live() {
		return ws.endedTimer(m, timer)
	}
	if timer.payload.BabyID != edit.BabyID {
		return refuse("record_parent_mismatch")
	}
	if timer.payload.TimerKind == "sleep" && len(edit.Components) > 0 {
		return refuse("invalid_timer_kind")
	}
	before := timer.payload
	edited := before
	edited.CompletedComponents = append([]protocol.FeedComponent{}, before.CompletedComponents...)
	for _, change := range edit.Components {
		index := -1
		for position, part := range edited.CompletedComponents {
			if part.ID == change.ID {
				index = position
			}
		}
		if index < 0 {
			return refuse("timer_component_not_found")
		}
		if edited.CompletedComponents[index].Kind != change.Kind {
			return refuse("invalid_mutation")
		}
		if change.Kind == "bottle" && !reflect.DeepEqual(change.Seconds, edited.CompletedComponents[index].Seconds) {
			return refuse("invalid_timer_time")
		}
		edited.CompletedComponents[index] = change
	}
	accepted := ws.cmd.AcceptedAt.UTC()
	start, err := parseTimerTime(edited.StartedAt)
	if err != nil {
		return protocol.Outcome{}, nil, err
	}
	var sideStart *time.Time
	if edited.CurrentSide != nil && edited.CurrentSideStartedAt != nil {
		parsed, err := parseTimerTime(*edited.CurrentSideStartedAt)
		if err != nil {
			return protocol.Outcome{}, nil, err
		}
		sideStart = &parsed
	}
	var bottleStart *time.Time
	if edited.CurrentBottleStartedAt != nil {
		parsed, err := parseTimerTime(*edited.CurrentBottleStartedAt)
		if err != nil {
			return protocol.Outcome{}, nil, err
		}
		bottleStart = &parsed
	}
	if edit.StartedAt != nil {
		normalized, err := protocol.NormalizeTimestamp(*edit.StartedAt)
		if err != nil {
			return refuse("invalid_timer_time")
		}
		moved, _ := parseTimerTime(normalized)
		// A minute of grace for a phone clock running ahead; a week back is
		// the longest a completed record may span.
		if moved.After(accepted.Add(time.Minute)) || moved.Before(accepted.Add(-7*24*time.Hour)) {
			return refuse("invalid_timer_time")
		}
		if sideStart != nil {
			if sideStart.Equal(start) {
				// The side has run since the timer began, so it began when the
				// timer did: moving one moves the other.
				sideStart = &moved
				edited.CurrentSideStartedAt = &normalized
			} else if moved.After(*sideStart) {
				return refuse("invalid_timer_time")
			}
		}
		if bottleStart != nil {
			if bottleStart.Equal(start) {
				bottleStart = &moved
				edited.CurrentBottleStartedAt = &normalized
			} else if moved.After(*bottleStart) {
				return refuse("invalid_timer_time")
			}
		}
		start, edited.StartedAt = moved, normalized
	}
	// The finished breast parts all happened before the running side began,
	// or before now when none runs.
	reference := accepted
	if sideStart != nil {
		reference = *sideStart
	}
	if bottleStart != nil {
		reference = *bottleStart
	}
	var breast, measured time.Duration
	for _, part := range edited.CompletedComponents {
		if part.Kind == "breast" && part.Seconds != nil {
			breast += time.Duration(*part.Seconds) * time.Second
		}
		if part.Seconds != nil && (part.Kind == "breast" || part.Kind == "bottle") {
			measured += time.Duration(*part.Seconds) * time.Second
		}
	}
	if measured > reference.Sub(start) || breast > time.Duration(ws.cmd.Limits.MaxSideTotalSeconds)*time.Second {
		return refuse("invalid_timer_time")
	}
	if edit.SessionID != nil && ws.sessionOwnerMismatch(before.BabyID, before.TimerKind, *edit.SessionID) {
		return refuse("record_parent_mismatch")
	}
	edited.SessionID = edit.SessionID
	if reflect.DeepEqual(edited, before) {
		return ws.outcomeAt(m, "applied", nil, timer.entity, nil, timer.row.ChangedSeq), nil, nil
	}
	return ws.persistTimer(m, timer, edited)
}

// stopTimer is the legacy stopTimer: the timer's tombstone naming its
// completion, then the completion (a sleep, or a feed unless cleanup left no
// part), at two consecutive sequences. The outcome is the tombstone with the
// completion related, at the last sequence.
func (ws *workingSet) stopTimer(m protocol.Mutation, timer storedTimer, payload protocol.TimerStopPayload) (protocol.Outcome, error, error) {
	entity := timer.entity
	stopped, err := parseTimerTime(payload.StoppedAt)
	if err != nil {
		return protocol.Outcome{}, nil, err
	}
	started, err := parseTimerTime(timer.payload.StartedAt)
	if err != nil {
		return protocol.Outcome{}, nil, err
	}
	if stopped.Before(started) || stopped.After(started.Add(7*24*time.Hour)) {
		return rejection(m, "rejected", "invalid_timer_time", &entity), nil, nil
	}
	completionPayload, completionKind, err := completedTimerPayload(timer.payload, payload, stopped, ws.cmd.Limits)
	if err != nil {
		return rejection(m, "rejected", "invalid_timer_time", &entity), nil, nil
	}
	var completion protocol.CanonicalEntity
	var completedID *string
	if completionPayload != nil {
		// Any entity of the family, live or not, of any kind, holds the id.
		if _, exists := ws.Lookup(payload.CompletedRecordID); exists {
			return rejection(m, "rejected", "completed_record_exists", nil), nil, nil
		}
		meta, err := ws.timerMeta(m, 1, "")
		if err != nil {
			return protocol.Outcome{}, nil, err
		}
		meta.ID = payload.CompletedRecordID
		completion = protocol.CanonicalEntity{EntityKind: completionKind, Meta: meta, Payload: completionPayload}
		id := payload.CompletedRecordID
		completedID = &id
	}
	tombstoneMeta, err := ws.timerMeta(m, timer.row.EntityVersion+1, timer.row.CreatorAccountID)
	if err != nil {
		return protocol.Outcome{}, nil, err
	}
	accepted := tombstoneMeta.AcceptedAt
	tombstoneMeta.TombstonedAt = &accepted
	tombstone := protocol.CanonicalEntity{EntityKind: "timer", Meta: tombstoneMeta, Payload: nil}
	tombstoneRow, err := Encode(tombstone, &timer.row, ws.change(m, 1, completedID))
	if err != nil {
		return protocol.Outcome{}, nil, err
	}
	fits, err := ws.fitsMinimumPage(tombstoneRow)
	if err != nil {
		return protocol.Outcome{}, nil, err
	}
	var completionRow Row
	if fits && completedID != nil {
		if completionRow, err = Encode(completion, nil, ws.change(m, 2, nil)); err != nil {
			return protocol.Outcome{}, nil, err
		}
		if fits, err = ws.fitsMinimumPage(completionRow); err != nil {
			return protocol.Outcome{}, nil, err
		}
	}
	if !fits {
		return rejection(m, "rejected", "mutation_too_large", nil), nil, nil
	}
	changes := uint64(1)
	delta := ws.delta.Replace(&timer.row, tombstoneRow)
	if completedID != nil {
		changes++
		delta = delta.Replace(nil, completionRow)
	}
	if err := ws.claim(changes); err != nil {
		return protocol.Outcome{}, err, nil
	}
	if !ws.admitsDelta(delta) {
		return protocol.Outcome{}, ErrQuotaExceeded, nil
	}
	if err := ws.write(&timer.row, tombstoneRow); err != nil {
		return protocol.Outcome{}, nil, err
	}
	related := []protocol.CanonicalEntity{}
	if completedID != nil {
		if err := ws.write(nil, completionRow); err != nil {
			return protocol.Outcome{}, nil, err
		}
		related = append(related, completion)
	}
	return ws.outcomeAt(m, "applied", nil, tombstone, related, ws.sequence), nil, nil
}

// endedTimer is the legacy endedTimerOutcome: a command on an ended timer is
// superseded (timer_ended) with its completion, at the completion's
// sequence, or at the timer's when it ended without one.
func (ws *workingSet) endedTimer(m protocol.Mutation, timer storedTimer) (protocol.Outcome, error, error) {
	related := []protocol.CanonicalEntity{}
	cursor := timer.row.ChangedSeq
	if id := timer.row.CompletedRecordID; id != nil {
		completion, ok := ws.Lookup(*id)
		if !ok {
			return protocol.Outcome{}, nil, fmt.Errorf("care: completion %s of stopped timer %s was not fetched", *id, timer.row.ID)
		}
		entity, err := completion.Entity()
		if err != nil {
			return protocol.Outcome{}, nil, err
		}
		related, cursor = append(related, entity), completion.ChangedSeq
	}
	code := "timer_ended"
	return ws.outcomeAt(m, "superseded", &code, timer.entity, related, cursor), nil, nil
}

// completedTimerPayload builds the record a stopped timer leaves: a sleep, or
// a feed of the finished parts plus the side still running at the stop. Both
// continue the timer's session. A nil payload (kind "feed") is a feed stop
// whose cleanup left no part: the timer ends without a completion.
func completedTimerPayload(timer protocol.TimerStartPayload, stop protocol.TimerStopPayload, stopped time.Time, limits protocol.Limits) (protocol.Payload, string, error) {
	if stop.CleanupShortBreastSegments && timer.TimerKind != "feed" {
		return nil, "", protocol.ErrInvalidMutation
	}
	ended, _ := protocol.NormalizeTimestamp(stop.StoppedAt)
	if timer.TimerKind == "sleep" {
		payload := protocol.SleepPayload{BabyID: timer.BabyID, StartedAt: timer.StartedAt, EndedAt: &ended,
			TimeZoneID: timer.TimeZoneID, Note: stop.Note, KindOverride: stop.SleepKindOverride, SleepSessionID: timer.SessionID}
		if err := validateSleep(payload, limits); err != nil {
			return nil, "", err
		}
		return payload, "sleep", nil
	}
	components := append([]protocol.FeedComponent(nil), timer.CompletedComponents...)
	if timer.CurrentSide == nil {
		if len(components) == 0 && !stop.CleanupShortBreastSegments {
			return nil, "", protocol.ErrInvalidMutation
		}
	} else {
		current, err := parseTimerTime(*timer.CurrentSideStartedAt)
		if err != nil || stopped.Before(current) || breastPartCount(components) >= limits.MaxSideIntervals {
			return nil, "", protocol.ErrInvalidTimestamp
		}
		seconds := uint32(stopped.Sub(current) / time.Second)
		if seconds > 0 {
			if stop.ComponentID == "" || timerHasComponent(components, stop.ComponentID) {
				return nil, "", protocol.ErrInvalidMutation
			}
			components = append(components, protocol.FeedComponent{ID: stop.ComponentID, Kind: "breast", Side: timer.CurrentSide,
				Seconds: &seconds, StartedAt: timer.CurrentSideStartedAt, EndedAt: &ended})
		}
	}
	if stop.CleanupShortBreastSegments {
		components = cleanShortBreastComponents(components)
		if len(components) == 0 {
			return nil, "feed", nil
		}
	}
	payload := protocol.FeedPayload{BabyID: timer.BabyID, Kind: cleanedFeedKind(components), StartedAt: timer.StartedAt, EndedAt: &ended,
		TimeZoneID: timer.TimeZoneID, Note: stop.Note, Components: components, FeedingSessionID: timer.SessionID}
	if err := validateFeed(payload, limits); err != nil {
		return nil, "", err
	}
	return payload, "feed", nil
}

func cleanShortBreastComponents(components []protocol.FeedComponent) []protocol.FeedComponent {
	cleaned := make([]protocol.FeedComponent, 0, len(components))
	for _, component := range components {
		if component.Kind == "breast" && component.Seconds != nil {
			if *component.Seconds < 30 {
				continue
			}
			if len(cleaned) > 0 {
				previous := &cleaned[len(cleaned)-1]
				if previous.Kind == "breast" && previous.Side != nil && component.Side != nil && *previous.Side == *component.Side && previous.StartedAt == nil && component.StartedAt == nil {
					seconds := *previous.Seconds + *component.Seconds
					previous.Seconds = &seconds
					continue
				}
			}
		}
		cleaned = append(cleaned, component)
	}
	return cleaned
}

func cleanedFeedKind(components []protocol.FeedComponent) string {
	hasBottle := false
	for _, component := range components {
		if component.Kind == "breast" {
			return "breast"
		}
		if component.Kind == "bottle" {
			hasBottle = true
		}
	}
	if hasBottle {
		return "bottle"
	}
	return "breast"
}

// timerHasComponent reports whether components hold a part with id.
func timerHasComponent(components []protocol.FeedComponent, id string) bool {
	for _, component := range components {
		if component.ID == id {
			return true
		}
	}
	return false
}

// breastPartCount is how many breast parts a feed or timer holds. Each is one
// timed side; MaxSideIntervals bounds them.
func breastPartCount(components []protocol.FeedComponent) int {
	count := 0
	for _, component := range components {
		if component.Kind == "breast" {
			count++
		}
	}
	return count
}

// parseTimerTime is a timer timestamp at whole microseconds, as stored.
func parseTimerTime(value string) (time.Time, error) {
	normalized, err := protocol.NormalizeTimestamp(value)
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse("2006-01-02T15:04:05.000000Z", normalized)
}
