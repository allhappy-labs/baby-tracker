package familyread

import (
	"strconv"
	"time"

	"babytracker/server/internal/protocol"
)

const (
	// SummaryTimeout bounds one Summary call's guarded read. It equals the
	// MCP request's five-second total deadline, so it never extends a
	// caller's deadline; the transport still owns the whole-request limit.
	SummaryTimeout = 5 * time.Second
	// MaxSummaryMeasurements is the most measurement observations one
	// summary lists. More returns ErrResultTooLarge; none is ever dropped.
	MaxSummaryMeasurements = 500
	// maxSummaryDays bounds the local-day partition of a 744-hour range. A
	// range touches at most 33 local dates in real zones; this is a guard.
	maxSummaryDays = 64
)

// SummaryDefinitions documents exactly what get_summary counts. Task 6 places
// this text in the tool description.
const SummaryDefinitions = `Summarizes one live baby's canonical care records in the half-open instant range [start,end), at most 744 elapsed hours (31 days) with start < end. timezone is a required IANA time zone database name such as Europe/Zurich or UTC ("Local", empty names, paths and numeric offsets are rejected). It only splits the range into local calendar days: a day runs from the first instant its local date begins to the first instant the next local date begins, clipped to [start,end). Days can therefore be partial at the range edges or 23/25 hours long across daylight-saving changes, and there may be more than 31 of them; elapsed_seconds is always the actual elapsed time. Deleted records never count. Every total is the sum of its days.
- feed.sessions: distinct feeding sessions (the feeding session link, or the feed's own id when unlinked) whose earliest live logged feed starts in [start,end), counted on that feed's day. A session whose first part began before start is not counted, even if later parts fall in range. Running timers never count or move a session's start. The number of logged feeds is not the number of sessions.
- feed.logged_feeds: logged feed records whose start is in [start,end), including instantaneous bottle or solids logs with no end.
- feed.known_bottle_volume_ml: the sum of the recorded volumes (millilitres) of the bottle parts of those logged feeds, each part once, on its feed's start day. It is null when bottle_volume_observations is 0, and 0 only when recorded bottle volumes total zero. bottles_missing_volume counts bottle parts with no recorded volume; they are never added as zero. Running timer bottles never count.
- sleep.sessions: distinct sleep sessions (the sleep session link, or the sleep's own id when unlinked) that contain at least one completed sleep (one with an end, including zero-length) and whose earliest live stored sleep, finished or not, starts in [start,end), counted on that sleep's day.
- sleep.completed_seconds: per local day, the length of the union of completed sleep intervals (end after start) clipped to that day and to [start,end). Overlapping or joined sleeps count once; a sleep crossing midnight is split between its days. Zero-length sleeps, unfinished sleeps and running timers add no time.
- sleep.unfinished_sleeps: stored sleeps with no end that start in [start,end). They are excluded from completed totals and no running duration is computed.
- diapers, supplement_doses, measurement_observations: records whose start (measured_at for measurements) is in [start,end). A supplement dose is one logged record; no quantity or unit is inferred. supplements_by_name counts doses per supplement name.
- measurements: every observation in range with its instant, day and unit-named values (weight_kg, length_cm, head_circumference_cm). A value that was not measured is omitted, never zero, and values are never summed.
- active_timers: the baby's running feed or sleep timers as of as_of, regardless of the range; they are never part of any total.
Durations and volumes are exact decimals with at most six fractional digits. These are recorded-event definitions; they need not match the app's charts.`

// SummaryQuery selects one baby's summary over [Start,End), split into local
// days of the IANA zone Timezone.
type SummaryQuery struct {
	BabyID     string
	Start, End time.Time
	Timezone   string
}

// Decimal6 is an exact non-negative decimal quantity held as an integer
// count of millionths. It encodes as a plain JSON number with trailing
// fractional zeros trimmed (82800, 1.5, 0.000001), never in exponent form.
type Decimal6 int64

func (d Decimal6) String() string {
	whole, fraction := int64(d)/1_000_000, int64(d)%1_000_000
	sign := ""
	if d < 0 {
		sign, whole, fraction = "-", -whole, -fraction
	}
	text := sign + strconv.FormatInt(whole, 10)
	if fraction == 0 {
		return text
	}
	digits := strconv.FormatInt(fraction+1_000_000, 10)[1:]
	for digits[len(digits)-1] == '0' {
		digits = digits[:len(digits)-1]
	}
	return text + "." + digits
}

func (d Decimal6) MarshalJSON() ([]byte, error) { return []byte(d.String()), nil }

// FeedTotals counts feeds; see SummaryDefinitions for each predicate.
type FeedTotals struct {
	Sessions                 int       `json:"sessions"`
	LoggedFeeds              int       `json:"logged_feeds"`
	BottleVolumeObservations int       `json:"bottle_volume_observations"`
	KnownBottleVolumeML      *Decimal6 `json:"known_bottle_volume_ml"`
	BottlesMissingVolume     int       `json:"bottles_missing_volume"`
}

// SleepTotals counts sleeps; see SummaryDefinitions for each predicate.
type SleepTotals struct {
	Sessions         int      `json:"sessions"`
	CompletedSeconds Decimal6 `json:"completed_seconds"`
	UnfinishedSleeps int      `json:"unfinished_sleeps"`
}

// SummaryCounts are one day's (or the whole range's) recorded-event totals.
type SummaryCounts struct {
	Feed                    FeedTotals  `json:"feed"`
	Sleep                   SleepTotals `json:"sleep"`
	Diapers                 int         `json:"diapers"`
	SupplementDoses         int         `json:"supplement_doses"`
	MeasurementObservations int         `json:"measurement_observations"`
}

// SummaryDay is one local calendar day of the summary zone, clipped to the
// range: [Start,End) with its actual ElapsedSeconds.
type SummaryDay struct {
	Date           string    `json:"date"`
	Start          time.Time `json:"start"`
	End            time.Time `json:"end"`
	ElapsedSeconds Decimal6  `json:"elapsed_seconds"`
	SummaryCounts
}

// SupplementDoses counts logged doses of one supplement name.
type SupplementDoses struct {
	Name  string `json:"name"`
	Doses int    `json:"doses"`
}

// MeasurementObservation is one dated measurement. Day is the summary day it
// is attributed to. Values carry their unit in the field name; a value that
// was not measured is absent.
type MeasurementObservation struct {
	ID                  string            `json:"id"`
	MeasuredAt          time.Time         `json:"measured_at"`
	Day                 string            `json:"day"`
	WeightKG            *protocol.Decimal `json:"weight_kg,omitempty"`
	LengthCM            *protocol.Decimal `json:"length_cm,omitempty"`
	LengthPosition      *string           `json:"length_position,omitempty"`
	HeadCircumferenceCM *protocol.Decimal `json:"head_circumference_cm,omitempty"`
}

// SummaryResult is one baby's summary read at one family data revision.
// Start and End are the normalized range (UTC, microseconds) and Totals is
// the sum of Days. ActiveTimers are current facts as of AsOf, never part of
// any total.
type SummaryResult struct {
	BabyID            string                   `json:"baby_id"`
	Start             time.Time                `json:"start"`
	End               time.Time                `json:"end"`
	Timezone          string                   `json:"timezone"`
	ElapsedSeconds    Decimal6                 `json:"elapsed_seconds"`
	Revision          uint64                   `json:"revision"`
	AsOf              time.Time                `json:"as_of"`
	Totals            SummaryCounts            `json:"totals"`
	Days              []SummaryDay             `json:"days"`
	SupplementsByName []SupplementDoses        `json:"supplements_by_name"`
	Measurements      []MeasurementObservation `json:"measurements"`
	ActiveTimers      []ActiveTimer            `json:"active_timers"`
}
