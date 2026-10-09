package records

import (
	"context"
	"errors"
	"reflect"
	"time"

	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

type TimerRecord struct {
	Payload           protocol.TimerStartPayload
	Meta              protocol.EntityMeta
	CompletedRecordID *string
	LastSequence      uint64
}

func (record TimerRecord) Canonical() protocol.CanonicalEntity {
	entity := protocol.CanonicalEntity{EntityKind: "timer", Meta: record.Meta, Payload: record.Payload}
	if record.Meta.TombstonedAt != nil {
		entity.Payload = nil
	}
	return entity
}

func (r *Repository) GetTimer(ctx context.Context, tx pgx.Tx, familyID, id string) (TimerRecord, error) {
	return r.getTimer(ctx, tx, familyID, id, true)
}

// GetTimerSnapshot reads the same canonical record without a mutation lock.
// Callers supply a consistent read-only snapshot transaction.
func (r *Repository) GetTimerSnapshot(ctx context.Context, tx pgx.Tx, familyID, id string) (TimerRecord, error) {
	return r.getTimer(ctx, tx, familyID, id, false)
}

func (r *Repository) getTimer(ctx context.Context, tx pgx.Tx, familyID, id string, lock bool) (TimerRecord, error) {
	lockClause := ""
	if lock {
		lockClause = " FOR UPDATE"
	}
	var record TimerRecord
	var tombstone, currentSide, currentStarted, completed, sessionID *string
	err := tx.QueryRow(ctx, `SELECT id::text,family_id::text,entity_version,creator_account_id::text,last_editor_account_id::text,
		to_char(client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
		to_char(accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
		CASE WHEN tombstoned_at IS NULL THEN NULL ELSE to_char(tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
		last_sequence,baby_id::text,timer_kind,to_char(started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),time_zone_id,
		current_side,CASE WHEN current_side_started_at IS NULL THEN NULL ELSE to_char(current_side_started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
		completed_record_id::text,
		CASE WHEN current_bottle_started_at IS NULL THEN NULL ELSE to_char(current_bottle_started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
		session_id::text
		FROM active_timers WHERE family_id=$1 AND id=$2`+lockClause, familyID, id).Scan(
		&record.Meta.ID, &record.Meta.FamilyID, &record.Meta.EntityVersion, &record.Meta.CreatorAccountID,
		&record.Meta.LastEditorAccountID, &record.Meta.ClientMutationTime, &record.Meta.AcceptedAt, &tombstone,
		&record.LastSequence, &record.Payload.BabyID, &record.Payload.TimerKind, &record.Payload.StartedAt,
		&record.Payload.TimeZoneID, &currentSide, &currentStarted, &completed, &record.Payload.CurrentBottleStartedAt,
		&sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return record, ErrNotFound
	}
	if err != nil {
		return record, err
	}
	record.Meta.TombstonedAt = tombstone
	record.Payload.CurrentSide, record.Payload.CurrentSideStartedAt = currentSide, currentStarted
	record.CompletedRecordID = completed
	record.Payload.SessionID = sessionID
	components, err := tx.Query(ctx, `SELECT component_id::text,kind,side,seconds,volume_ml::text,contents,foods,amount,
		CASE WHEN started_at IS NULL THEN NULL ELSE to_char(started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
		CASE WHEN ended_at IS NULL THEN NULL ELSE to_char(ended_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END
		FROM timer_components WHERE family_id=$1 AND timer_id=$2 ORDER BY position`, familyID, id)
	if err != nil {
		return record, err
	}
	defer components.Close()
	record.Payload.CompletedComponents = []protocol.FeedComponent{}
	for components.Next() {
		var component protocol.FeedComponent
		var componentVolume *string
		if err = components.Scan(&component.ID, &component.Kind, &component.Side, &component.Seconds, &componentVolume,
			&component.Contents, &component.Foods, &component.Amount, &component.StartedAt, &component.EndedAt); err != nil {
			return record, err
		}
		if componentVolume != nil {
			component.VolumeML, err = protocol.ParseDecimal(*componentVolume)
			if err != nil {
				return record, err
			}
		}
		record.Payload.CompletedComponents = append(record.Payload.CompletedComponents, component)
	}
	err = components.Err()
	return record, err
}

func (r *Repository) ApplyTimer(ctx context.Context, tx pgx.Tx, command ApplyCommand) (protocol.Outcome, error) {
	m := command.Mutation
	babyID := timerMutationBabyID(m)
	baby, err := r.get(ctx, tx, "baby", command.Grant.FamilyID, babyID)
	if errors.Is(err, ErrNotFound) {
		return rejection(m, "rejected", "baby_not_found", nil), nil
	}
	if err != nil {
		return protocol.Outcome{}, err
	}
	if baby.Meta.TombstonedAt != nil {
		return rejection(m, "rejected", "baby_deleted", &baby), nil
	}
	switch m.Operation {
	case "timer_start":
		return r.startTimer(ctx, tx, command)
	case "timer_finish_bottle", "timer_stop", "update":
		return r.changeTimer(ctx, tx, command)
	default:
		return rejection(m, "rejected", "invalid_mutation", nil), nil
	}
}

func (r *Repository) startTimer(ctx context.Context, tx pgx.Tx, command ApplyCommand) (protocol.Outcome, error) {
	m := command.Mutation
	payload := m.Payload.(protocol.TimerStartPayload)
	kind, err := FindEntityKind(ctx, tx, command.Grant.FamilyID, m.EntityID)
	if err == nil {
		if kind != "timer" {
			return rejection(m, "rejected", "entity_kind_collision", nil), nil
		}
		existing, getErr := r.GetTimer(ctx, tx, command.Grant.FamilyID, m.EntityID)
		if getErr != nil {
			return protocol.Outcome{}, getErr
		}
		if existing.Meta.TombstonedAt == nil && reflect.DeepEqual(existing.Payload, payload) {
			return timerOutcome(m, "superseded", nil, existing.Canonical(), nil, existing.LastSequence, command), nil
		}
		return rejection(m, "rejected", "entity_exists", nil), nil
	}
	if !errors.Is(err, ErrNotFound) {
		return protocol.Outcome{}, err
	}
	existing, err := r.findLiveTimer(ctx, tx, command.Grant.FamilyID, payload.BabyID, payload.TimerKind)
	if err == nil {
		return timerOutcome(m, "superseded", nil, existing.Canonical(), nil, existing.LastSequence, command), nil
	}
	if !errors.Is(err, ErrNotFound) {
		return protocol.Outcome{}, err
	}
	if payload.SessionID != nil {
		mismatch, mismatchErr := sessionOwnerMismatch(ctx, tx, command.Grant.FamilyID, payload.BabyID, payload.TimerKind, *payload.SessionID)
		if mismatchErr != nil {
			return protocol.Outcome{}, mismatchErr
		}
		if mismatch {
			return rejection(m, "rejected", "record_parent_mismatch", nil), nil
		}
	}
	meta, err := timerMeta(command, 1, "")
	if err != nil {
		return protocol.Outcome{}, err
	}
	entity := protocol.CanonicalEntity{EntityKind: "timer", Meta: meta, Payload: payload}
	if !entityFitsMinimumPage(command, entity) {
		return rejection(m, "rejected", "mutation_too_large", nil), nil
	}
	bytes, err := EntityStorageBytes(entity)
	if err != nil {
		return protocol.Outcome{}, err
	}
	if err := r.putTimer(ctx, tx, meta, payload, nil, bytes); err != nil {
		return protocol.Outcome{}, err
	}
	changes, err := AppendJournal(ctx, tx, command.Grant, command.DeviceID, m,
		[]protocol.CanonicalEntity{entity}, command.ServerInstanceID, command.RecoveryEpoch)
	if err != nil {
		return protocol.Outcome{}, err
	}
	journalBytes, err := JournalStorageBytes(changes[0])
	if err != nil {
		return protocol.Outcome{}, err
	}
	if err := chargeUsage(ctx, tx, command.Grant.FamilyID, 0, bytes+journalBytes+deletionReserveBytes, command.Limits, false); err != nil {
		return protocol.Outcome{}, err
	}
	last := changes[0].Cursor
	return protocol.Outcome{MutationID: m.MutationID, Status: "applied", CanonicalEntity: &entity,
		RelatedEntities: []protocol.CanonicalEntity{}, Cursor: &last}, nil
}

func (r *Repository) changeTimer(ctx context.Context, tx pgx.Tx, command ApplyCommand) (protocol.Outcome, error) {
	m := command.Mutation
	record, err := r.GetTimer(ctx, tx, command.Grant.FamilyID, m.EntityID)
	if errors.Is(err, ErrNotFound) {
		return rejection(m, "rejected", "entity_not_found", nil), nil
	}
	if err != nil {
		return protocol.Outcome{}, err
	}
	if edit, ok := m.Payload.(protocol.TimerEditPayload); ok {
		return r.editTimer(ctx, tx, command, record, edit)
	}
	if record.Payload.BabyID != timerMutationBabyID(m) {
		entity := record.Canonical()
		return rejection(m, "rejected", "record_parent_mismatch", &entity), nil
	}
	if record.Meta.TombstonedAt != nil {
		return r.endedTimerOutcome(ctx, tx, command, record)
	}
	switch payload := m.Payload.(type) {
	case protocol.TimerBottleFinishPayload:
		if record.Payload.TimerKind != "feed" || record.Payload.CurrentBottleStartedAt == nil {
			entity := record.Canonical()
			return rejection(m, "rejected", "invalid_timer_kind", &entity), nil
		}
		started, startErr := parseTimerTime(*record.Payload.CurrentBottleStartedAt)
		finished, finishErr := parseTimerTime(payload.FinishedAt)
		seconds := int64(finished.Sub(started) / time.Second)
		if startErr != nil || finishErr != nil || seconds < 1 || seconds > 604800 || timerHasComponent(record.Payload.CompletedComponents, payload.ComponentID) {
			entity := record.Canonical()
			return rejection(m, "rejected", "invalid_timer_time", &entity), nil
		}
		componentSeconds := uint32(seconds)
		component := protocol.FeedComponent{ID: payload.ComponentID, Kind: "bottle", Seconds: &componentSeconds, VolumeML: payload.VolumeML, Contents: payload.Contents,
			StartedAt: record.Payload.CurrentBottleStartedAt, EndedAt: &payload.FinishedAt}
		record.Payload.CompletedComponents = append(record.Payload.CompletedComponents, component)
		record.Payload.CurrentBottleStartedAt = nil
		return r.persistTimerChange(ctx, tx, command, record)
	case protocol.TimerStopPayload:
		if record.Payload.CurrentBottleStartedAt != nil {
			entity := record.Canonical()
			return rejection(m, "rejected", "active_bottle_unfinished", &entity), nil
		}
		return r.stopTimer(ctx, tx, command, record, payload)
	default:
		return rejection(m, "rejected", "invalid_mutation", nil), nil
	}
}

// editTimer applies a caregiver's correction to a running timer: a new start,
// new values for finished parts, or both. The rules are judged against the
// row GetTimer locked, so an edit racing a switch, a bottle or a stop from the
// other phone sees whatever committed first. Every refusal carries the stored
// timer so the phone that sent the edit can put its copy back.
func (r *Repository) editTimer(ctx context.Context, tx pgx.Tx, command ApplyCommand, record TimerRecord, edit protocol.TimerEditPayload) (protocol.Outcome, error) {
	refuse := func(code string) (protocol.Outcome, error) {
		entity := record.Canonical()
		return rejection(command.Mutation, "rejected", code, &entity), nil
	}
	if record.Meta.TombstonedAt != nil {
		return r.endedTimerOutcome(ctx, tx, command, record)
	}
	if record.Payload.BabyID != edit.BabyID {
		return refuse("record_parent_mismatch")
	}
	if record.Payload.TimerKind == "sleep" && len(edit.Components) > 0 {
		return refuse("invalid_timer_kind")
	}
	before := record.Payload
	edited := before
	edited.CompletedComponents = append([]protocol.FeedComponent{}, before.CompletedComponents...)
	for _, change := range edit.Components {
		index := -1
		for position, component := range edited.CompletedComponents {
			if component.ID == change.ID {
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
	accepted := command.AcceptedAt.UTC()
	start, err := parseTimerTime(edited.StartedAt)
	if err != nil {
		return protocol.Outcome{}, err
	}
	var sideStart *time.Time
	if edited.CurrentSide != nil && edited.CurrentSideStartedAt != nil {
		parsed, parseErr := parseTimerTime(*edited.CurrentSideStartedAt)
		if parseErr != nil {
			return protocol.Outcome{}, parseErr
		}
		sideStart = &parsed
	}
	var bottleStart *time.Time
	if edited.CurrentBottleStartedAt != nil {
		parsed, parseErr := parseTimerTime(*edited.CurrentBottleStartedAt)
		if parseErr != nil {
			return protocol.Outcome{}, parseErr
		}
		bottleStart = &parsed
	}
	if edit.StartedAt != nil {
		normalized, normalizeErr := protocol.NormalizeTimestamp(*edit.StartedAt)
		if normalizeErr != nil {
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
	for _, component := range edited.CompletedComponents {
		if component.Kind == "breast" && component.Seconds != nil {
			breast += time.Duration(*component.Seconds) * time.Second
		}
		if component.Seconds != nil && (component.Kind == "breast" || component.Kind == "bottle") {
			measured += time.Duration(*component.Seconds) * time.Second
		}
	}
	if measured > reference.Sub(start) || breast > time.Duration(command.Limits.MaxSideTotalSeconds)*time.Second {
		return refuse("invalid_timer_time")
	}
	if edit.SessionID != nil {
		mismatch, mismatchErr := sessionOwnerMismatch(ctx, tx, command.Grant.FamilyID, record.Payload.BabyID, record.Payload.TimerKind, *edit.SessionID)
		if mismatchErr != nil {
			return protocol.Outcome{}, mismatchErr
		}
		if mismatch {
			return refuse("record_parent_mismatch")
		}
	}
	edited.SessionID = edit.SessionID
	if reflect.DeepEqual(edited, before) {
		return timerOutcome(command.Mutation, "applied", nil, record.Canonical(), nil, record.LastSequence, command), nil
	}
	record.Payload = edited
	return r.persistTimerChange(ctx, tx, command, record)
}

func (r *Repository) persistTimerChange(ctx context.Context, tx pgx.Tx, command ApplyCommand, record TimerRecord) (protocol.Outcome, error) {
	oldBytes, err := storedEntityBytes(ctx, tx, command.Grant.FamilyID, "timer", record.Meta.ID)
	if err != nil {
		return protocol.Outcome{}, err
	}
	meta, err := timerMeta(command, record.Meta.EntityVersion+1, record.Meta.CreatorAccountID)
	if err != nil {
		return protocol.Outcome{}, err
	}
	record.Meta = meta
	entity := record.Canonical()
	if !entityFitsMinimumPage(command, entity) {
		return rejection(command.Mutation, "rejected", "mutation_too_large", nil), nil
	}
	bytes, err := EntityStorageBytes(entity)
	if err != nil {
		return protocol.Outcome{}, err
	}
	if err := r.putTimer(ctx, tx, meta, record.Payload, nil, bytes); err != nil {
		return protocol.Outcome{}, err
	}
	changes, err := AppendJournal(ctx, tx, command.Grant, command.DeviceID, command.Mutation,
		[]protocol.CanonicalEntity{entity}, command.ServerInstanceID, command.RecoveryEpoch)
	if err != nil {
		return protocol.Outcome{}, err
	}
	journalBytes, err := JournalStorageBytes(changes[0])
	if err != nil {
		return protocol.Outcome{}, err
	}
	if err := chargeUsage(ctx, tx, command.Grant.FamilyID, 0, bytes-oldBytes+journalBytes, command.Limits, false); err != nil {
		return protocol.Outcome{}, err
	}
	last := changes[0].Cursor
	outcome := protocol.Outcome{MutationID: command.Mutation.MutationID, Status: "applied", CanonicalEntity: &entity,
		RelatedEntities: []protocol.CanonicalEntity{}, Cursor: &last}
	if command.Mutation.ObservedEntityVersion < record.Meta.EntityVersion-1 {
		outcome.OverwroteNewerVersion = true
	}
	return outcome, nil
}

func (r *Repository) stopTimer(ctx context.Context, tx pgx.Tx, command ApplyCommand, record TimerRecord, payload protocol.TimerStopPayload) (protocol.Outcome, error) {
	stopped, err := parseTimerTime(payload.StoppedAt)
	if err != nil {
		return protocol.Outcome{}, err
	}
	started, err := parseTimerTime(record.Payload.StartedAt)
	if err != nil {
		return protocol.Outcome{}, err
	}
	if stopped.Before(started) || stopped.After(started.Add(7*24*time.Hour)) {
		entity := record.Canonical()
		return rejection(command.Mutation, "rejected", "invalid_timer_time", &entity), nil
	}
	completionPayload, completionKind, err := completedTimerPayload(record.Payload, payload, stopped, command.Limits)
	if err != nil {
		entity := record.Canonical()
		return rejection(command.Mutation, "rejected", "invalid_timer_time", &entity), nil
	}
	var completion protocol.CanonicalEntity
	if completionPayload != nil {
		if _, err := FindEntityKind(ctx, tx, command.Grant.FamilyID, payload.CompletedRecordID); err == nil {
			return rejection(command.Mutation, "rejected", "completed_record_exists", nil), nil
		} else if !errors.Is(err, ErrNotFound) {
			return protocol.Outcome{}, err
		}
		completionMeta, metaErr := timerMeta(command, 1, "")
		if metaErr != nil {
			return protocol.Outcome{}, metaErr
		}
		completionMeta.ID = payload.CompletedRecordID
		completion = protocol.CanonicalEntity{EntityKind: completionKind, Meta: completionMeta, Payload: completionPayload}
	}
	tombstoneMeta, err := timerMeta(command, record.Meta.EntityVersion+1, record.Meta.CreatorAccountID)
	if err != nil {
		return protocol.Outcome{}, err
	}
	accepted := tombstoneMeta.AcceptedAt
	tombstoneMeta.TombstonedAt = &accepted
	tombstone := protocol.CanonicalEntity{EntityKind: "timer", Meta: tombstoneMeta, Payload: nil}
	if !entityFitsMinimumPage(command, tombstone) || (completionPayload != nil && !entityFitsMinimumPage(command, completion)) {
		return rejection(command.Mutation, "rejected", "mutation_too_large", nil), nil
	}
	oldTimerBytes, err := storedEntityBytes(ctx, tx, command.Grant.FamilyID, "timer", record.Meta.ID)
	if err != nil {
		return protocol.Outcome{}, err
	}
	timerBytes, err := EntityStorageBytes(tombstone)
	if err != nil {
		return protocol.Outcome{}, err
	}
	var completionBytes int64
	var completedID *string
	if completionPayload != nil {
		completionBytes, err = EntityStorageBytes(completion)
		if err != nil {
			return protocol.Outcome{}, err
		}
		completedID = &payload.CompletedRecordID
	}
	if err := r.putTimer(ctx, tx, tombstoneMeta, record.Payload, completedID, timerBytes); err != nil {
		return protocol.Outcome{}, err
	}
	entities := []protocol.CanonicalEntity{tombstone}
	if completionPayload != nil {
		if err := r.put(ctx, tx, completion, completionBytes); err != nil {
			return protocol.Outcome{}, err
		}
		entities = append(entities, completion)
	}
	changes, err := AppendJournal(ctx, tx, command.Grant, command.DeviceID, command.Mutation,
		entities, command.ServerInstanceID, command.RecoveryEpoch)
	if err != nil {
		return protocol.Outcome{}, err
	}
	journalBytes := int64(0)
	for _, change := range changes {
		value, sizeErr := JournalStorageBytes(change)
		if sizeErr != nil {
			return protocol.Outcome{}, sizeErr
		}
		journalBytes += value
	}
	completedCount := int64(0)
	if completionPayload != nil {
		completedCount = 1
	}
	if err := chargeUsage(ctx, tx, command.Grant.FamilyID, completedCount,
		timerBytes-oldTimerBytes+completionBytes+journalBytes, command.Limits, false); err != nil {
		return protocol.Outcome{}, err
	}
	last := changes[len(changes)-1].Cursor
	return protocol.Outcome{MutationID: command.Mutation.MutationID, Status: "applied", CanonicalEntity: &tombstone,
		RelatedEntities: entities[1:], Cursor: &last}, nil
}

// completedTimerPayload builds the record a stopped timer leaves: a sleep, or a
// feed of the finished parts plus the side still running at the stop. Both
// continue the timer's session.
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

func (r *Repository) endedTimerOutcome(ctx context.Context, tx pgx.Tx, command ApplyCommand, record TimerRecord) (protocol.Outcome, error) {
	related := []protocol.CanonicalEntity{}
	cursor := record.LastSequence
	if record.CompletedRecordID != nil {
		kind := "feed"
		if record.Payload.TimerKind == "sleep" {
			kind = "sleep"
		}
		completion, err := r.get(ctx, tx, kind, command.Grant.FamilyID, *record.CompletedRecordID)
		if err != nil {
			return protocol.Outcome{}, err
		}
		related = append(related, completion)
		cursor, err = entityLastSequence(ctx, tx, command.Grant.FamilyID, kind, *record.CompletedRecordID)
		if err != nil {
			return protocol.Outcome{}, err
		}
	}
	code := "timer_ended"
	return timerOutcome(command.Mutation, "superseded", &code, record.Canonical(), related, cursor, command), nil
}

func timerOutcome(m protocol.Mutation, status string, code *string, entity protocol.CanonicalEntity,
	related []protocol.CanonicalEntity, sequence uint64, command ApplyCommand) protocol.Outcome {
	if related == nil {
		related = []protocol.CanonicalEntity{}
	}
	cursor := protocol.Cursor{ServerInstanceID: command.ServerInstanceID, RecoveryEpoch: command.RecoveryEpoch,
		FamilyID: command.Grant.FamilyID, Sequence: sequence}
	return protocol.Outcome{MutationID: m.MutationID, Status: status, CanonicalEntity: &entity,
		RelatedEntities: related, Cursor: &cursor, Code: code}
}

func (r *Repository) findLiveTimer(ctx context.Context, tx pgx.Tx, familyID, babyID, kind string) (TimerRecord, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM active_timers
		WHERE family_id=$1 AND baby_id=$2 AND timer_kind=$3 AND tombstoned_at IS NULL`, familyID, babyID, kind).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return TimerRecord{}, ErrNotFound
	}
	if err != nil {
		return TimerRecord{}, err
	}
	return r.GetTimer(ctx, tx, familyID, id)
}

// putTimer writes a timer and replaces its finished parts; a stopped timer
// keeps none. component_version is pinned to the current version by the
// schema, so it is neither written nor read.
func (r *Repository) putTimer(ctx context.Context, tx pgx.Tx, meta protocol.EntityMeta, payload protocol.TimerStartPayload,
	completedRecordID *string, bytes int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO active_timers(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,
		client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,timer_kind,started_at,time_zone_id,current_side,
		current_side_started_at,completed_record_id,current_bottle_started_at,session_id)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,0,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		ON CONFLICT(family_id,id) DO UPDATE SET entity_version=EXCLUDED.entity_version,
		last_editor_account_id=EXCLUDED.last_editor_account_id,client_mutation_time=EXCLUDED.client_mutation_time,
		accepted_at=EXCLUDED.accepted_at,tombstoned_at=EXCLUDED.tombstoned_at,started_at=EXCLUDED.started_at,last_sequence=0,storage_bytes=EXCLUDED.storage_bytes,
		current_side=EXCLUDED.current_side,current_side_started_at=EXCLUDED.current_side_started_at,
		completed_record_id=EXCLUDED.completed_record_id,current_bottle_started_at=EXCLUDED.current_bottle_started_at,
		session_id=EXCLUDED.session_id`, meta.FamilyID, meta.ID, payload.BabyID, meta.EntityVersion,
		meta.CreatorAccountID, meta.LastEditorAccountID, meta.ClientMutationTime, meta.AcceptedAt, meta.TombstonedAt, bytes,
		payload.TimerKind, payload.StartedAt, payload.TimeZoneID, payload.CurrentSide, payload.CurrentSideStartedAt,
		completedRecordID, payload.CurrentBottleStartedAt, payload.SessionID)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM timer_components WHERE family_id=$1 AND timer_id=$2`, meta.FamilyID, meta.ID); err != nil {
		return err
	}
	if meta.TombstonedAt != nil {
		return nil
	}
	for index, component := range payload.CompletedComponents {
		var componentVolume any
		if component.VolumeML != nil {
			componentVolume = component.VolumeML.String()
		}
		if _, err = tx.Exec(ctx, `INSERT INTO timer_components(family_id,timer_id,component_id,position,kind,side,seconds,volume_ml,contents,foods,amount,started_at,ended_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, meta.FamilyID, meta.ID, component.ID, index, component.Kind,
			component.Side, component.Seconds, componentVolume, component.Contents, component.Foods, component.Amount, component.StartedAt, component.EndedAt); err != nil {
			return err
		}
	}
	return nil
}

func timerHasComponent(components []protocol.FeedComponent, id string) bool {
	for _, component := range components {
		if component.ID == id {
			return true
		}
	}
	return false
}

func timerMeta(command ApplyCommand, version uint64, creator string) (protocol.EntityMeta, error) {
	accepted, err := protocol.NormalizeTimestamp(command.AcceptedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return protocol.EntityMeta{}, err
	}
	client, err := protocol.NormalizeTimestamp(command.Mutation.ClientMutationTime)
	if err != nil {
		return protocol.EntityMeta{}, err
	}
	if creator == "" {
		creator = command.Grant.AccountID
	}
	return protocol.EntityMeta{ID: command.Mutation.EntityID, FamilyID: command.Grant.FamilyID, EntityVersion: version,
		CreatorAccountID: creator, LastEditorAccountID: command.Grant.AccountID,
		ClientMutationTime: client, AcceptedAt: accepted}, nil
}

func timerMutationBabyID(m protocol.Mutation) string {
	switch payload := m.Payload.(type) {
	case protocol.TimerStartPayload:
		return payload.BabyID
	case protocol.TimerBottleFinishPayload:
		return payload.BabyID
	case protocol.TimerStopPayload:
		return payload.BabyID
	case protocol.TimerEditPayload:
		return payload.BabyID
	}
	return ""
}

func parseTimerTime(value string) (time.Time, error) {
	normalized, err := protocol.NormalizeTimestamp(value)
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse("2006-01-02T15:04:05.000000Z", normalized)
}

func entityLastSequence(ctx context.Context, tx pgx.Tx, familyID, kind, id string) (uint64, error) {
	table, err := tableFor(kind)
	if err != nil {
		return 0, err
	}
	var sequence uint64
	err = tx.QueryRow(ctx, "SELECT last_sequence FROM "+table+" WHERE family_id=$1 AND id=$2", familyID, id).Scan(&sequence)
	return sequence, err
}
