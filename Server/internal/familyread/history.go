package familyread

import (
	"context"
	"errors"
	"slices"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

// Query instants outside these bounds can hold no canonical record (stored
// event times lie in [1900, 2101)), so they are rejected as malformed.
var (
	minQueryInstant = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
	maxQueryInstant = time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
)

// maxSleepInterval mirrors the schema bound ended_at <= started_at + 7 days,
// which lets the sleep overlap scan start a fixed distance before the range.
const maxSleepInterval = "7 days"

// historyQuery is a validated HistoryQuery: UTC bounds rounded up to the
// database's microsecond grid (which keeps [start,end) exact for stored
// microsecond instants), kinds deduplicated in HistoryKinds order.
type historyQuery struct {
	babyID     string
	start, end time.Time
	kinds      []string
	limit      int
}

// normalizeRange applies the shared instant-range bound of history and
// summary: Start < End, both within [1900, 2200], at most MaxRangeElapsed
// apart (inclusive), returned in UTC rounded up to whole microseconds.
func normalizeRange(start, end time.Time) (time.Time, time.Time, error) {
	if start.IsZero() || end.IsZero() || !start.Before(end) || start.Before(minQueryInstant) || end.After(maxQueryInstant) {
		return time.Time{}, time.Time{}, ErrInvalidRange
	}
	if end.Sub(start) > MaxRangeElapsed {
		return time.Time{}, time.Time{}, ErrRangeTooLong
	}
	return ceilMicrosecond(start), ceilMicrosecond(end), nil
}

func normalizeHistoryQuery(q HistoryQuery) (historyQuery, error) {
	start, end, err := normalizeRange(q.Start, q.End)
	if err != nil {
		return historyQuery{}, err
	}
	limit := q.Limit
	if limit == 0 {
		limit = DefaultHistoryLimit
	}
	if limit < 1 || limit > MaxHistoryLimit {
		return historyQuery{}, ErrInvalidLimit
	}
	if !protocol.ValidUUID(q.BabyID) {
		return historyQuery{}, ErrInvalidBabyID
	}
	for _, kind := range q.Kinds {
		if !slices.Contains(historyKinds[:], kind) {
			return historyQuery{}, ErrInvalidKind
		}
	}
	kinds := make([]string, 0, len(historyKinds))
	for _, kind := range historyKinds {
		if len(q.Kinds) == 0 || slices.Contains(q.Kinds, kind) {
			kinds = append(kinds, kind)
		}
	}
	return historyQuery{babyID: q.BabyID, start: start, end: end, kinds: kinds, limit: limit}, nil
}

func ceilMicrosecond(at time.Time) time.Time {
	at = at.UTC()
	truncated := at.Truncate(time.Microsecond)
	if truncated.Before(at) {
		truncated = truncated.Add(time.Microsecond)
	}
	return truncated
}

func (s *Service) history(ctx context.Context, credential string, q HistoryQuery, fits HistoryBudget) (HistoryResult, error) {
	if err := s.ready(); err != nil {
		return HistoryResult{}, err
	}
	query, err := normalizeHistoryQuery(q)
	if err != nil {
		return HistoryResult{}, err
	}
	var claims *cursorClaims
	if q.Cursor != "" {
		opened, err := s.openCursor(q.Cursor)
		if err != nil {
			return HistoryResult{}, err
		}
		claims = &opened
	}
	var result HistoryResult
	err = s.guard.WithIntegrationRead(ctx, credential, func(ctx context.Context, tx pgx.Tx, grant access.Grant) error {
		grantID, ok := authority.IntegrationCredentialGrantID(credential)
		if !ok {
			return authority.ErrIntegrationUnauthorized
		}
		page, err := s.readHistory(ctx, tx, grant, grantID, query, claims, fits)
		if err != nil {
			return err
		}
		result = page
		return nil
	})
	if err != nil {
		return HistoryResult{}, err
	}
	return result, nil
}

// readHistory runs entirely inside the guard's read-only REPEATABLE READ
// transaction: the revision, continuation check, page, sessions and timers
// all come from one snapshot.
func (s *Service) readHistory(ctx context.Context, tx pgx.Tx, grant access.Grant, grantID string, q historyQuery,
	claims *cursorClaims, fits HistoryBudget) (HistoryResult, error) {
	asOf := s.now().UTC()
	revision, err := familyRevision(ctx, tx, grant.FamilyID)
	if err != nil {
		return HistoryResult{}, err
	}
	var instance string
	var epoch int64
	if err := tx.QueryRow(ctx, `SELECT instance_id::text,recovery_epoch FROM server_state WHERE singleton`).Scan(&instance, &epoch); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return HistoryResult{}, authority.ErrUnavailable
		}
		return HistoryResult{}, err
	}
	binding := cursorBinding{Grant: grantID, Account: grant.AccountID, Family: grant.FamilyID, Instance: instance, Epoch: uint64(epoch),
		Baby: q.babyID, Start: q.start.UnixMicro(), End: q.end.UnixMicro(), Kinds: q.kinds}
	var after *position
	if claims != nil {
		resumed, err := continueFrom(*claims, binding, revision, asOf)
		if err != nil {
			return HistoryResult{}, err
		}
		after = &resumed
	}
	if err := requireLiveBaby(ctx, tx, grant.FamilyID, q.babyID); err != nil {
		return HistoryResult{}, err
	}

	candidates, err := readCandidates(ctx, tx, grant.FamilyID, q, after)
	if err != nil {
		return HistoryResult{}, err
	}
	more := len(candidates) > q.limit
	if more {
		candidates = candidates[:q.limit]
	}
	entries, err := readEntries(ctx, tx, grant.FamilyID, q.babyID, candidates)
	if err != nil {
		return HistoryResult{}, err
	}
	if err := loadSessions(ctx, tx, grant.FamilyID, q.babyID, entries); err != nil {
		return HistoryResult{}, err
	}
	timers, err := readTimers(ctx, tx, grant.FamilyID, q.babyID)
	if err != nil {
		return HistoryResult{}, err
	}
	base := HistoryResult{Revision: revision, AsOf: asOf, ActiveTimers: timers}
	expires := asOf.Add(CursorTTL)
	return s.fitPage(base, entries, more, func(last HistoryEntry) (string, error) {
		return s.sealCursor(binding, revision, expires, last)
	}, fits)
}

func candidateArgs(familyID string, q historyQuery, after *position) []any {
	args := []any{familyID, q.babyID, q.start, q.end, q.limit + 1}
	if after != nil {
		args = append(args, after.start, after.id)
	}
	return args
}

// selectCandidates runs a candidate statement (careCandidateSQL) and reads
// its keys.
func selectCandidates(ctx context.Context, tx pgx.Tx, sql string, args []any) ([]position, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (position, error) {
		var candidate position
		err := row.Scan(&candidate.start, &candidate.kind, &candidate.id)
		candidate.start = candidate.start.UTC()
		return candidate, err
	})
}

// historyEntry is candidate's entry for the root record entity, which must
// be a live record of candidate's kind and id owned by babyID.
func historyEntry(candidate position, babyID string, entity protocol.CanonicalEntity) (HistoryEntry, error) {
	entry := HistoryEntry{Kind: candidate.kind, ID: candidate.id, Start: candidate.start}
	var owner string
	var end *string
	switch payload := entity.Payload.(type) {
	case protocol.FeedPayload:
		owner, end = payload.BabyID, payload.EndedAt
		entry.Session = &Session{ID: effectiveSession(payload.FeedingSessionID, candidate.id)}
	case protocol.SleepPayload:
		owner, end = payload.BabyID, payload.EndedAt
		entry.Unfinished = payload.EndedAt == nil
		entry.Session = &Session{ID: effectiveSession(payload.SleepSessionID, candidate.id)}
	case protocol.DiaperPayload:
		owner, end = payload.BabyID, payload.EndedAt
	case protocol.SupplementPayload:
		owner = payload.BabyID
	case protocol.MeasurementPayload:
		owner = payload.BabyID
	default:
		return HistoryEntry{}, dataInvalid(candidate.kind, candidate.id)
	}
	meta := entity.Meta
	if entity.EntityKind != candidate.kind || meta.TombstonedAt != nil || meta.ID != candidate.id || owner != babyID {
		return HistoryEntry{}, dataInvalid(candidate.kind, candidate.id)
	}
	if end != nil {
		ended, err := time.Parse(time.RFC3339Nano, *end)
		if err != nil {
			return HistoryEntry{}, dataInvalid(candidate.kind, candidate.id)
		}
		ended = ended.UTC()
		entry.End = &ended
	}
	entry.Entity = entity
	return entry, nil
}

func effectiveSession(link *string, id string) string {
	if link != nil {
		return *link
	}
	return id
}

// loadSessions fills each feed/sleep entry's earliest live member start with
// one indexed lookup per kind (careSessionSQL), restricted to the page's
// effective sessions.
func loadSessions(ctx context.Context, tx pgx.Tx, familyID, babyID string, entries []HistoryEntry) error {
	for _, kind := range []string{"feed", "sleep"} {
		var keys []string
		for _, entry := range entries {
			if entry.Kind == kind && !slices.Contains(keys, entry.Session.ID) {
				keys = append(keys, entry.Session.ID)
			}
		}
		if len(keys) == 0 {
			continue
		}
		rows, err := tx.Query(ctx, careSessionSQL(kind), familyID, babyID, keys)
		if err != nil {
			return err
		}
		earliest := map[string]time.Time{}
		var key string
		var start time.Time
		_, err = pgx.ForEachRow(rows, []any{&key, &start}, func() error {
			earliest[key] = start.UTC()
			return nil
		})
		if err != nil {
			return err
		}
		for i := range entries {
			if entries[i].Kind != kind {
				continue
			}
			first, ok := earliest[entries[i].Session.ID]
			if !ok || first.After(entries[i].Start) {
				return dataInvalid(entries[i].Kind, entries[i].ID)
			}
			entries[i].Session.EarliestMemberStart = first
		}
	}
	return nil
}

// activeTimer is the running timer id of babyID; entity must be its live
// timer entity.
func activeTimer(id, babyID string, entity protocol.CanonicalEntity) (ActiveTimer, error) {
	payload, ok := entity.Payload.(protocol.TimerStartPayload)
	if !ok || entity.Meta.TombstonedAt != nil || entity.Meta.ID != id || payload.BabyID != babyID {
		return ActiveTimer{}, dataInvalid("timer", id)
	}
	started, err := time.Parse(time.RFC3339Nano, payload.StartedAt)
	if err != nil {
		return ActiveTimer{}, dataInvalid("timer", id)
	}
	return ActiveTimer{ID: id, TimerKind: payload.TimerKind, StartedAt: started.UTC(), Entity: entity}, nil
}

// fitPage returns the longest canonical-order prefix of entries whose
// complete result (fixed fields, timers, entries and the cursor bound to the
// last kept entry) fits both the body-independent byte bound and fits. The
// first entity is never skipped: if it, or the fixed fields, cannot fit, the
// whole read fails with ErrResultTooLarge.
func (s *Service) fitPage(base HistoryResult, entries []HistoryEntry, more bool, mint func(HistoryEntry) (string, error), fits HistoryBudget) (HistoryResult, error) {
	try := func(count int) (HistoryResult, bool, error) {
		candidate := base
		candidate.Entries = entries[:count:count]
		if count < len(entries) || more {
			cursor, err := mint(entries[count-1])
			if err != nil {
				return HistoryResult{}, false, err
			}
			candidate.NextCursor = cursor
		}
		size, err := encodedBytes(candidate)
		if err != nil || size > s.maxResultBytes {
			return HistoryResult{}, false, err
		}
		if fits != nil {
			ok, err := fits(candidate)
			if err != nil || !ok {
				return HistoryResult{}, false, err
			}
		}
		return candidate, true, nil
	}
	if result, ok, err := try(len(entries)); err != nil || ok {
		return result, err
	}
	if len(entries) < 2 {
		return HistoryResult{}, ErrResultTooLarge
	}
	best, ok, err := try(1)
	if err != nil {
		return HistoryResult{}, err
	}
	if !ok {
		return HistoryResult{}, ErrResultTooLarge
	}
	// try(low) fits and try(high) does not; the budget is monotone in size.
	low, high := 1, len(entries)
	for high-low > 1 {
		middle := (low + high) / 2
		candidate, ok, err := try(middle)
		if err != nil {
			return HistoryResult{}, err
		}
		if ok {
			best, low = candidate, middle
		} else {
			high = middle
		}
	}
	return best, nil
}
