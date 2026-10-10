package care

import (
	"errors"
	"strings"
	"time"

	"babytracker/server/internal/protocol"
)

// The record rules are the legacy Repository.Apply (records/repository.go at 342973dc)
// decided against the working set instead of per-mutation queries: the kind
// probe, the parent and session checks and the version all read the
// entity's current row, which is the fetched row or this batch's last write
// of it.

const maximumPageSnapshotID = "ffffffff-ffff-4fff-bfff-ffffffffffff"

func rejection(m protocol.Mutation, status, code string, entity *protocol.CanonicalEntity) protocol.Outcome {
	return protocol.Outcome{MutationID: m.MutationID, Status: status, CanonicalEntity: entity,
		RelatedEntities: []protocol.CanonicalEntity{}, Code: &code}
}

// applyRecord decides a record mutation and, when it applies, writes its
// row into the working set. Past the size and validation checks every
// mutation shares, timers go to applyTimer and baby deletes to deleteBaby.
func (ws *workingSet) applyRecord(m protocol.Mutation) (outcome protocol.Outcome, fatal, err error) {
	cmd := ws.cmd
	canonical, err := protocol.CanonicalMutationBytes(m)
	if err != nil {
		return protocol.Outcome{}, err, nil
	}
	if len(canonical) > cmd.Limits.SyncMutationBytes {
		return rejection(m, "rejected", "mutation_too_large", nil), nil, nil
	}
	if err := ValidateMutation(m, cmd.Limits); err != nil {
		return rejection(m, "rejected", rejectionCode(err), nil), nil, nil
	}
	if m.EntityKind == "timer" {
		return ws.applyTimer(m)
	}
	if m.EntityKind == "baby" && m.Operation == "delete" {
		return ws.deleteBaby(m)
	}

	// The kind probe: one primary-key row instead of eight tables.
	current, found := ws.Lookup(m.EntityID)
	if m.Operation == "create" && found {
		code := "entity_exists"
		if current.Kind != m.EntityKind {
			code = "entity_kind_collision"
		}
		return rejection(m, "rejected", code, nil), nil, nil
	}
	if m.Operation != "create" && !found {
		return rejection(m, "rejected", "entity_not_found", nil), nil, nil
	}
	if m.Operation != "create" && current.Kind != m.EntityKind {
		return rejection(m, "rejected", "entity_kind_collision", nil), nil, nil
	}

	// Validation admitted the time, so a parse failure here (as any
	// write-path re-parse of a validated or stored timestamp) is a broken
	// invariant: an infrastructure error, and the transaction rolls back.
	client, err := protocol.NormalizeTimestamp(m.ClientMutationTime)
	if err != nil {
		return protocol.Outcome{}, nil, err
	}
	meta := protocol.EntityMeta{ID: m.EntityID, FamilyID: cmd.Grant.FamilyID, EntityVersion: 1, CreatorAccountID: cmd.Grant.AccountID,
		LastEditorAccountID: cmd.Grant.AccountID, ClientMutationTime: client, AcceptedAt: ws.accepted}
	var existing *protocol.CanonicalEntity
	var previous *Row
	if m.Operation != "create" {
		entity, err := current.Entity()
		if err != nil {
			return protocol.Outcome{}, nil, err
		}
		existing, previous = &entity, &current
		meta.EntityVersion = entity.Meta.EntityVersion + 1
		meta.CreatorAccountID = entity.Meta.CreatorAccountID
	}

	if m.EntityKind != "baby" {
		babyID := mutationBabyID(m)
		if existing != nil {
			existingBabyID := mutationBabyIDFromEntity(*existing)
			if existing.Meta.TombstonedAt != nil {
				// A tombstone keeps its baby in the row (the legacy
				// storedBabyID read).
				existingBabyID = ""
				if current.BabyID != nil {
					existingBabyID = *current.BabyID
				}
			}
			if existingBabyID != babyID {
				return rejection(m, "rejected", "record_parent_mismatch", existing), nil, nil
			}
		}
		baby, ok := ws.Lookup(babyID)
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
	} else if m.Operation == "create" && ws.liveBabies >= cmd.Limits.MaxLiveBabies {
		return protocol.Outcome{}, ErrQuotaExceeded, nil
	}
	if existing != nil && existing.Meta.TombstonedAt != nil {
		return rejection(m, "superseded", "entity_deleted", existing), nil, nil
	}
	if m.Operation != "delete" {
		switch p := m.Payload.(type) {
		case protocol.FeedPayload:
			if p.FeedingSessionID != nil && ws.sessionOwnerMismatch(p.BabyID, "feed", *p.FeedingSessionID) {
				return rejection(m, "rejected", "record_parent_mismatch", existing), nil, nil
			}
		case protocol.SleepPayload:
			if p.SleepSessionID != nil && ws.sessionOwnerMismatch(p.BabyID, "sleep", *p.SleepSessionID) {
				return rejection(m, "rejected", "record_parent_mismatch", existing), nil, nil
			}
		case protocol.MeasurementPayload:
			measured, _ := time.Parse(time.RFC3339Nano, p.MeasuredAt)
			babyRow, _ := ws.Lookup(p.BabyID)
			baby, err := babyRow.Entity()
			if err != nil {
				return protocol.Outcome{}, nil, err
			}
			birth := ""
			if payload, ok := baby.Payload.(protocol.BabyPayload); ok {
				birth = payload.BirthDate
			}
			zone, _ := time.LoadLocation(p.TimeZoneID)
			if measured.After(cmd.AcceptedAt) || measured.In(zone).Format("2006-01-02") < birth {
				return rejection(m, "rejected", "invalid_timestamp", existing), nil, nil
			}
			if existing != nil && existing.Payload.(protocol.MeasurementPayload).LoggedAt != p.LoggedAt {
				return rejection(m, "rejected", "record_logged_at_mismatch", existing), nil, nil
			}
		case protocol.SupplementPayload:
			if existing != nil && m.Operation == "update" && existing.Payload.(protocol.SupplementPayload).LoggedAt != p.LoggedAt {
				return rejection(m, "rejected", "record_logged_at_mismatch", existing), nil, nil
			}
		}
	}

	payload := m.Payload
	// An update from a build that predates icons leaves `icon` out. The
	// stored icon is kept in the entity itself, so the wire, the change
	// other phones pull and the response all carry it (main's
	// records/repository.go at fce422d5, with existing decoded from the
	// stored wire). A present icon replaces it; no wire form clears it.
	if next, ok := payload.(protocol.BabyPayload); ok && next.Icon == nil && existing != nil {
		if old, ok := existing.Payload.(protocol.BabyPayload); ok && old.Icon != nil {
			next.Icon = old.Icon
			payload = next
		}
	}
	entity := protocol.CanonicalEntity{EntityKind: m.EntityKind, Meta: meta, Payload: payload}
	if m.Operation == "delete" {
		accepted := ws.accepted
		entity.Meta.TombstonedAt, entity.Payload = &accepted, nil
	}
	row, err := Encode(entity, previous, Change{Seq: ws.sequence + 1, ActorAccountID: cmd.Grant.AccountID, DeviceID: cmd.DeviceID,
		Operation: m.Operation, AcceptedAt: cmd.AcceptedAt})
	if err != nil {
		return protocol.Outcome{}, nil, err
	}
	fits, err := ws.fitsMinimumPage(row)
	if err != nil {
		return protocol.Outcome{}, nil, err
	}
	if !fits {
		return rejection(m, "rejected", "mutation_too_large", existing), nil, nil
	}
	if _, err := ws.nextSequence(); err != nil {
		return protocol.Outcome{}, err, nil
	}
	// Quotas, as chargeUsage: the entity's charge replaces the stored one
	// (and the live reserve is taken on create, released on delete); a
	// delete is never refused.
	if m.Operation != "delete" && !ws.admits(previous, row) {
		return protocol.Outcome{}, ErrQuotaExceeded, nil
	}
	if err := ws.write(previous, row); err != nil {
		return protocol.Outcome{}, nil, err
	}
	cursor := protocol.Cursor{ServerInstanceID: cmd.Server.InstanceID, RecoveryEpoch: cmd.Server.RecoveryEpoch, FamilyID: cmd.Grant.FamilyID,
		Sequence: row.ChangedSeq}
	outcome = protocol.Outcome{MutationID: m.MutationID, Status: "applied", CanonicalEntity: &entity,
		RelatedEntities: []protocol.CanonicalEntity{}, Cursor: &cursor}
	if existing != nil && m.ObservedEntityVersion < existing.Meta.EntityVersion {
		outcome.OverwroteNewerVersion = true
	}
	return outcome, nil, nil
}

// fitsMinimumPage reports whether row, served alone, fits the smallest
// sync page and snapshot page a client may ask for at the largest cursor
// and page token (the legacy entityFitsMinimumPage, measured on the wire).
func (ws *workingSet) fitsMinimumPage(row Row) (bool, error) {
	cmd := ws.cmd
	token := strings.Repeat("x", protocol.MaximumPageTokenLength)
	server := protocol.ServerInfo{InstanceID: cmd.Server.InstanceID, RecoveryEpoch: cmd.Server.RecoveryEpoch}
	highWater := protocol.Cursor{ServerInstanceID: server.InstanceID, RecoveryEpoch: server.RecoveryEpoch, FamilyID: cmd.Grant.FamilyID,
		Sequence: protocol.MaxSafeInteger}
	change, err := protocol.EncodeChange(highWater, row.Kind, row.ID, row.EntityVersion, row.ActorAccountID, row.DeviceID, row.Operation,
		ws.accepted, row.Wire)
	if err != nil {
		return false, err
	}
	if protocol.IncrementalPageEncodedBytesRaw(server, cmd.Grant.FamilyID, protocol.MaxSafeInteger, len(change), 1, &token) > protocol.MinimumSyncPageBytes {
		return false, nil
	}
	return protocol.SnapshotPageEncodedBytesRaw(server, maximumPageSnapshotID, highWater, len(row.Wire), 1, &token) <= protocol.MinimumSyncPageBytes, nil
}

// sessionOwnerMismatch reports whether session already groups live records
// of another baby or of the other kind (records/sessions.go at 342973dc): a live feed or
// sleep whose id or session is session, or a live timer in session, of
// another kind or baby.
func (ws *workingSet) sessionOwnerMismatch(babyID, kind, session string) bool {
	return ws.anyRow(func(r Row) bool {
		if !r.Live() {
			return false
		}
		baby := ""
		if r.BabyID != nil {
			baby = *r.BabyID
		}
		inSession := r.SessionID != nil && *r.SessionID == session
		switch r.Kind {
		case "feed", "sleep":
			return (r.ID == session || inSession) && (r.Kind != kind || baby != babyID)
		case "timer":
			return inSession && (r.TimerKind == nil || *r.TimerKind != kind || baby != babyID)
		}
		return false
	})
}

// mutationBabyID is the baby a record mutation names.
func mutationBabyID(m protocol.Mutation) string {
	switch p := m.Payload.(type) {
	case protocol.FeedPayload:
		return p.BabyID
	case protocol.SleepPayload:
		return p.BabyID
	case protocol.GrowthProfilePayload:
		return p.BabyID
	case protocol.MeasurementPayload:
		return p.BabyID
	case protocol.SupplementPayload:
		return p.BabyID
	case protocol.DiaperPayload:
		return p.BabyID
	case protocol.DeletePayload:
		if p.BabyID != nil {
			return *p.BabyID
		}
	}
	return ""
}

// mutationSessionID is the session a feed, sleep, timer start or timer edit
// joins.
func mutationSessionID(m protocol.Mutation) string {
	if m.Operation == "delete" {
		return ""
	}
	var session *string
	switch p := m.Payload.(type) {
	case protocol.FeedPayload:
		session = p.FeedingSessionID
	case protocol.SleepPayload:
		session = p.SleepSessionID
	case protocol.TimerStartPayload:
		session = p.SessionID
	case protocol.TimerEditPayload:
		session = p.SessionID
	}
	if session == nil {
		return ""
	}
	return *session
}

// mutationBabyIDFromEntity is the baby of a live record.
func mutationBabyIDFromEntity(entity protocol.CanonicalEntity) string {
	switch p := entity.Payload.(type) {
	case protocol.FeedPayload:
		return p.BabyID
	case protocol.SleepPayload:
		return p.BabyID
	case protocol.GrowthProfilePayload:
		return p.BabyID
	case protocol.MeasurementPayload:
		return p.BabyID
	case protocol.SupplementPayload:
		return p.BabyID
	case protocol.DiaperPayload:
		return p.BabyID
	}
	return ""
}

// rejectionCode is the outcome code of a validation error.
func rejectionCode(err error) string {
	switch {
	case errors.Is(err, protocol.ErrInvalidTimestamp):
		return "invalid_timestamp"
	case errors.Is(err, protocol.ErrInvalidDecimal):
		return "invalid_decimal"
	case errors.Is(err, protocol.ErrInvalidText):
		return "invalid_text"
	case errors.Is(err, protocol.ErrInvalidCollection):
		return "invalid_collection"
	case errors.Is(err, protocol.ErrUnsupportedResource):
		return "unsupported_resource"
	default:
		return "invalid_mutation"
	}
}
