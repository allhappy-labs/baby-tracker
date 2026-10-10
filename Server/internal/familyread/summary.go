package familyread

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"time"
	// Summary days come from caller-supplied IANA names. Embedding the zone
	// database makes their resolution independent of the image (the
	// distroless server image need not carry /usr/share/zoneinfo). records
	// already imports it for stored time_zone_id validation; importing it
	// here keeps the guarantee local to the code that depends on it.
	_ "time/tzdata"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

// zoneName admits IANA-shaped names only: slash-separated segments of
// letters, digits, '_', '+' and '-', starting with a letter. It rules out
// paths, dot segments, numeric offsets and surrounding space before
// time.LoadLocation is consulted.
var zoneName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z0-9_+-]+)*$`)

// errDayPartition reports a zone whose transitions defeat the bounded day
// iteration. It cannot happen for real zone data and is not client-coded.
var errDayPartition = errors.New("familyread: local day partition did not converge")

// loadZone resolves a summary timezone: a named IANA zone ("UTC" included,
// "Local" and empty rejected) as resolved by time.LoadLocation.
func loadZone(name string) (*time.Location, error) {
	if len(name) > 255 || name == "Local" || !zoneName.MatchString(name) {
		return nil, ErrInvalidTimezone
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, ErrInvalidTimezone
	}
	return loc, nil
}

// localDay is one local calendar date's part of the range: [start,end).
type localDay struct {
	date       string
	start, end time.Time
}

// localDays partitions [start,end) into local calendar days of loc. A day
// runs from the first instant its local date begins to the first instant a
// later local date begins, clipped to the range. Boundaries advance strictly
// through actual instants, so DST gaps and overlaps give 23- or 25-hour
// days, a skipped date gets no day, and a repeated wall-clock hour stays in
// the day it began in.
func localDays(start, end time.Time, loc *time.Location) ([]localDay, error) {
	var days []localDay
	for at := start; at.Before(end); {
		local := at.In(loc)
		year, month, day := local.Date()
		next, err := firstInstantOfDate(at, time.Date(year, month, day+1, 0, 0, 0, 0, time.UTC), loc)
		if err != nil {
			return nil, err
		}
		if len(days) == maxSummaryDays {
			return nil, errDayPartition
		}
		days = append(days, localDay{date: local.Format(time.DateOnly), start: at, end: earlier(next, end)})
		at = next
	}
	return days, nil
}

// firstInstantOfDate returns the first instant at or after from whose local
// date in loc is date (a UTC midnight label) or later. Within one zone period
// the offset is constant, so local time is monotone there and the answer is
// max(period start, date midnight at that offset) if that lies inside the
// period; otherwise the search continues in the next period.
func firstInstantOfDate(from, date time.Time, loc *time.Location) (time.Time, error) {
	at := from
	for range 8 {
		local := at.In(loc)
		_, offset := local.Zone()
		_, periodEnd := local.ZoneBounds()
		candidate := date.Add(-time.Duration(offset) * time.Second)
		if candidate.Before(at) {
			candidate = at
		}
		if periodEnd.IsZero() || candidate.Before(periodEnd) {
			return candidate.UTC(), nil
		}
		at = periodEnd
	}
	return time.Time{}, errDayPartition
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// summaryQuery is a validated SummaryQuery with its local-day partition.
type summaryQuery struct {
	babyID     string
	start, end time.Time
	zone       string
	loc        *time.Location
	days       []localDay
}

func normalizeSummaryQuery(q SummaryQuery) (summaryQuery, error) {
	start, end, err := normalizeRange(q.Start, q.End)
	if err != nil {
		return summaryQuery{}, err
	}
	if !protocol.ValidUUID(q.BabyID) {
		return summaryQuery{}, ErrInvalidBabyID
	}
	loc, err := loadZone(q.Timezone)
	if err != nil {
		return summaryQuery{}, err
	}
	days, err := localDays(start, end, loc)
	if err != nil {
		return summaryQuery{}, err
	}
	return summaryQuery{babyID: q.BabyID, start: start, end: end, zone: q.Timezone, loc: loc, days: days}, nil
}

func summaryArgs(familyID, babyID string, start, end time.Time, days []localDay) []any {
	bounds := make([]time.Time, 0, len(days)+1)
	for _, day := range days {
		bounds = append(bounds, day.start)
	}
	return []any{familyID, babyID, start, end, append(bounds, end)}
}

// Summary computes one baby's care summary over [Start,End) (see
// SummaryDefinitions) in a single guarded read at one family data revision,
// bounded by SummaryTimeout and the caller's context. The result is complete
// or an error: nothing is truncated, and it is never assembled from history
// pages.
func (s *Service) Summary(ctx context.Context, credential string, q SummaryQuery) (SummaryResult, error) {
	if err := s.ready(); err != nil {
		return SummaryResult{}, err
	}
	query, err := normalizeSummaryQuery(q)
	if err != nil {
		return SummaryResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.summaryTimeout)
	defer cancel()
	var result SummaryResult
	err = s.guard.WithIntegrationRead(ctx, credential, func(ctx context.Context, tx pgx.Tx, grant access.Grant) error {
		summary, err := s.readSummary(ctx, tx, grant.FamilyID, query)
		if err != nil {
			// A query cut off by the deadline reports the deadline.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		}
		result = summary
		return nil
	})
	if err != nil {
		return SummaryResult{}, err
	}
	return result, nil
}

// readSummary runs entirely inside the guard's read-only REPEATABLE READ
// transaction, so the revision, totals, observations and timers come from
// one snapshot.
func (s *Service) readSummary(ctx context.Context, tx pgx.Tx, familyID string, q summaryQuery) (SummaryResult, error) {
	asOf := s.now().UTC()
	revision, err := familyRevision(ctx, tx, familyID)
	if err != nil {
		return SummaryResult{}, err
	}
	if err := requireLiveBaby(ctx, tx, familyID, q.babyID); err != nil {
		return SummaryResult{}, err
	}
	result := SummaryResult{BabyID: q.babyID, Start: q.start, End: q.end, Timezone: q.zone,
		ElapsedSeconds: Decimal6(q.end.Sub(q.start).Microseconds()), Revision: revision, AsOf: asOf}
	if result.Days, err = readSummaryDays(ctx, tx, familyID, q); err != nil {
		return SummaryResult{}, err
	}
	for _, day := range result.Days {
		result.Totals.add(day.SummaryCounts)
	}
	if result.SupplementsByName, err = readSupplementDoses(ctx, tx, familyID, q); err != nil {
		return SummaryResult{}, err
	}
	if result.Measurements, err = readMeasurements(ctx, tx, familyID, q, s.maxMeasurements); err != nil {
		return SummaryResult{}, err
	}
	doses := 0
	for _, supplement := range result.SupplementsByName {
		doses += supplement.Doses
	}
	// Both lists repeat a predicate of the aggregate in the same snapshot.
	if doses != result.Totals.SupplementDoses || len(result.Measurements) != result.Totals.MeasurementObservations {
		return SummaryResult{}, dataInvalid("summary", q.babyID)
	}
	if result.ActiveTimers, err = readTimers(ctx, tx, familyID, q.babyID); err != nil {
		return SummaryResult{}, err
	}
	size, err := encodedBytes(result)
	if err != nil {
		return SummaryResult{}, err
	}
	if size > s.maxResultBytes {
		return SummaryResult{}, ErrResultTooLarge
	}
	return result, nil
}

func (c *SummaryCounts) add(day SummaryCounts) {
	c.Feed.Sessions += day.Feed.Sessions
	c.Feed.LoggedFeeds += day.Feed.LoggedFeeds
	c.Feed.BottleVolumeObservations += day.Feed.BottleVolumeObservations
	c.Feed.BottlesMissingVolume += day.Feed.BottlesMissingVolume
	if day.Feed.KnownBottleVolumeML != nil {
		total := *day.Feed.KnownBottleVolumeML
		if c.Feed.KnownBottleVolumeML != nil {
			total += *c.Feed.KnownBottleVolumeML
		}
		c.Feed.KnownBottleVolumeML = &total
	}
	c.Sleep.Sessions += day.Sleep.Sessions
	c.Sleep.CompletedSeconds += day.Sleep.CompletedSeconds
	c.Sleep.UnfinishedSleeps += day.Sleep.UnfinishedSleeps
	c.Diapers += day.Diapers
	c.SupplementDoses += day.SupplementDoses
	c.MeasurementObservations += day.MeasurementObservations
}

// readSummaryDays is the per-day totals, one per q.days entry. It runs
// careSummaryAggregateSQL and checks every row against the invariants the schema
// guarantees: one row per day in order, non-negative finite values, a volume
// exactly when there are volume observations, and no more sleep than the
// day's elapsed time.
func readSummaryDays(ctx context.Context, tx pgx.Tx, familyID string, q summaryQuery) ([]SummaryDay, error) {
	rows, err := tx.Query(ctx, careSummaryAggregateSQL, summaryArgs(familyID, q.babyID, q.start, q.end, q.days)...)
	if err != nil {
		return nil, err
	}
	days := make([]SummaryDay, 0, len(q.days))
	var (
		index                                                            int
		feedSessions, logged, observations, missing                      int64
		microML                                                          *int64
		sleepSessions, sleepMicros, unfinished, diapers, doses, measures int64
	)
	_, err = pgx.ForEachRow(rows, []any{&index, &feedSessions, &logged, &observations, &microML, &missing,
		&sleepSessions, &sleepMicros, &unfinished, &diapers, &doses, &measures}, func() error {
		if index != len(days) || index >= len(q.days) {
			return dataInvalid("summary", q.babyID)
		}
		bounds := q.days[index]
		elapsed := bounds.end.Sub(bounds.start).Microseconds()
		counts := []int64{feedSessions, logged, observations, missing, sleepSessions, unfinished, diapers, doses, measures}
		for _, count := range counts {
			if count < 0 {
				return dataInvalid("summary", q.babyID)
			}
		}
		if (microML != nil) != (observations > 0) || (microML != nil && *microML < 0) || sleepMicros < 0 || sleepMicros > elapsed {
			return dataInvalid("summary", q.babyID)
		}
		day := SummaryDay{Date: bounds.date, Start: bounds.start, End: bounds.end, ElapsedSeconds: Decimal6(elapsed)}
		day.Feed = FeedTotals{Sessions: int(feedSessions), LoggedFeeds: int(logged), BottleVolumeObservations: int(observations),
			BottlesMissingVolume: int(missing)}
		if microML != nil {
			volume := Decimal6(*microML)
			day.Feed.KnownBottleVolumeML = &volume
		}
		day.Sleep = SleepTotals{Sessions: int(sleepSessions), CompletedSeconds: Decimal6(sleepMicros), UnfinishedSleeps: int(unfinished)}
		day.Diapers, day.SupplementDoses, day.MeasurementObservations = int(diapers), int(doses), int(measures)
		days = append(days, day)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(days) != len(q.days) {
		return nil, dataInvalid("summary", q.babyID)
	}
	return days, nil
}

// readSupplementDoses is the selected supplement doses per name, in
// byte-wise name order (careSupplementsSQL).
func readSupplementDoses(ctx context.Context, tx pgx.Tx, familyID string, q summaryQuery) ([]SupplementDoses, error) {
	rows, err := tx.Query(ctx, careSupplementsSQL, familyID, q.babyID, q.start, q.end)
	if err != nil {
		return nil, err
	}
	doses := []SupplementDoses{}
	var name string
	var count int64
	_, err = pgx.ForEachRow(rows, []any{&name, &count}, func() error {
		doses = append(doses, SupplementDoses{Name: name, Doses: int(count)})
		return nil
	})
	return doses, err
}

// measurementObservation is the observation of the selected measurement id;
// entity must be its live measurement, of q's baby, measured in q's range.
func measurementObservation(id string, q summaryQuery, entity protocol.CanonicalEntity) (MeasurementObservation, error) {
	measurement, ok := entity.Payload.(protocol.MeasurementPayload)
	if !ok {
		return MeasurementObservation{}, dataInvalid("measurement", id)
	}
	meta := entity.Meta
	measured, parseErr := time.Parse(time.RFC3339Nano, measurement.MeasuredAt)
	measured = measured.UTC()
	if parseErr != nil || entity.EntityKind != "measurement" || meta.TombstonedAt != nil || meta.ID != id || measurement.BabyID != q.babyID ||
		measured.Before(q.start) || !measured.Before(q.end) {
		return MeasurementObservation{}, dataInvalid("measurement", id)
	}
	day := sort.Search(len(q.days), func(i int) bool { return q.days[i].end.After(measured) })
	return MeasurementObservation{ID: id, MeasuredAt: measured, Day: q.days[day].date,
		WeightKG: measurement.WeightKG, LengthCM: measurement.LengthCM, LengthPosition: measurement.LengthPosition,
		HeadCircumferenceCM: measurement.HeadCircumferenceCM}, nil
}
