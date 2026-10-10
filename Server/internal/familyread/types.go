// Package familyread answers the private read-only MCP tools (list_babies,
// get_history) from the canonical family data. Every read runs inside the
// integration guard's read-only REPEATABLE READ transaction, so one result
// reflects one family data revision under live family authority.
package familyread

import (
	"context"
	"errors"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

const (
	// DefaultHistoryLimit applies when HistoryQuery.Limit is zero.
	DefaultHistoryLimit = 50
	// MaxHistoryLimit is the largest accepted HistoryQuery.Limit.
	MaxHistoryLimit = 100
	// MaxRangeElapsed is the shared 31-day instant-range bound: at most 744
	// elapsed hours, inclusive, with Start < End.
	MaxRangeElapsed = 744 * time.Hour
	// MaxResultBytes bounds the JSON encoding of one domain result, before
	// any transport envelope. It is body-independent; the transport adds its
	// own exact final-envelope check through HistoryWithBudget.
	MaxResultBytes = 256 << 10
	// CursorTTL is how long a history continuation stays valid.
	CursorTTL = 15 * time.Minute
	// MaxBabies is the most live babies one result carries: the upper bound
	// protocol.Limits.Validate allows for max_live_babies.
	MaxBabies = 100
)

// historyKinds is the allowlist of care kinds history returns, in the
// canonical tie-break order (byte-wise ascending). Babies, growth profiles and
// running timers are never history entries.
var historyKinds = [...]string{"diaper", "feed", "measurement", "sleep", "supplement"}

// HistoryKinds returns the allowed HistoryQuery.Kinds values in canonical
// tie-break order.
func HistoryKinds() []string { return append([]string(nil), historyKinds[:]...) }

// HistorySelection documents exactly which canonical records get_history
// returns. Task 6 places this text in the tool description.
const HistorySelection = `Returns canonical care records for one live baby in the half-open instant range [start,end), at most 744 elapsed hours (31 days) with start < end. No timezone applies: bounds are absolute instants.
Selection per kind (deleted records never appear):
- feed: logged feed records whose start is in [start,end). A logged feed with no end (an instantaneous bottle or solids log) is a completed record. Feed parts (breast, bottle, solids components) are embedded in their feed.
- sleep: completed sleeps (end after start) whose interval overlaps [start,end), including one that began before start; zero-length completed sleeps and unfinished sleeps (no end) whose start is in [start,end). An unfinished sleep is flagged unfinished; no running duration is computed for it.
- diaper, supplement: point events whose start is in [start,end). A supplement is one logged dose with no quantity.
- measurement: dated observations whose measured_at is in [start,end); absent fields stay absent.
Entries are ordered by (start, kind, id) ascending. Each feed or sleep entry carries its effective session (session link, or its own id when unlinked) and the earliest start among that session's live stored records of the same baby and kind; joined sessions appear as several linked records, so the number of entries is not the number of sessions.
active_timers lists the baby's currently running feed/sleep timers as of as_of, regardless of the range; they are never history entries or completed records.
next_cursor continues the same query (same baby, range and kinds) for 15 minutes; any change to the family's data in between returns history_changed, and the query must restart from the first page. A page may hold fewer entries than the limit to fit the response size; if a single record cannot fit, result_too_large is returned and nothing is skipped.`

// Guard is the integration read seam: *authority.IntegrationReader (one
// read-only repeatable-read transaction on the MCP pool per call).
type Guard interface {
	WithIntegrationRead(ctx context.Context, credential string, run func(context.Context, pgx.Tx, access.Grant) error) error
}

// HistoryQuery selects one baby's care history in [Start,End). Kinds empty
// means every kind in HistoryKinds; Limit zero means DefaultHistoryLimit.
// A continuation repeats the same BabyID, Start, End and Kinds with Cursor.
type HistoryQuery struct {
	BabyID     string
	Start, End time.Time
	Kinds      []string
	Limit      int
	Cursor     string
}

// Session identifies a feed or sleep record's effective session group: its
// session link, or its own ID when unlinked, scoped to family, baby and kind.
// EarliestMemberStart is the earliest start among that group's live stored
// records (open sleeps included, running timers excluded); it may precede the
// requested range. The ID need not name a surviving record.
type Session struct {
	ID                  string    `json:"id"`
	EarliestMemberStart time.Time `json:"earliest_member_start"`
}

// HistoryEntry is one canonical root record. Start is the event instant
// (measured_at for measurements); End is the stored end for feeds, sleeps and
// diapers when present. Unfinished is true only for a sleep with no end.
type HistoryEntry struct {
	Kind       string                   `json:"kind"`
	ID         string                   `json:"id"`
	Start      time.Time                `json:"start"`
	End        *time.Time               `json:"end"`
	Unfinished bool                     `json:"unfinished"`
	Session    *Session                 `json:"session,omitempty"`
	Entity     protocol.CanonicalEntity `json:"entity"`
}

// ActiveTimer is a currently running feed or sleep timer: current state as
// of HistoryResult.AsOf, never a completed record. No running duration is
// computed.
type ActiveTimer struct {
	ID        string                   `json:"id"`
	TimerKind string                   `json:"timer_kind"`
	StartedAt time.Time                `json:"started_at"`
	Entity    protocol.CanonicalEntity `json:"entity"`
}

// HistoryResult is one page read at one family data revision.
type HistoryResult struct {
	Revision     uint64         `json:"revision"`
	AsOf         time.Time      `json:"as_of"`
	Entries      []HistoryEntry `json:"entries"`
	NextCursor   string         `json:"next_cursor,omitempty"`
	ActiveTimers []ActiveTimer  `json:"active_timers"`
}

// BabiesResult lists the family's live babies ordered by sort order then ID.
type BabiesResult struct {
	Revision uint64                     `json:"revision"`
	Babies   []protocol.CanonicalEntity `json:"babies"`
}

// HistoryBudget reports whether a complete candidate result (metadata,
// timers, entries and candidate cursor) fits the caller's final encoding
// limit. It must be pure, bounded and monotone in size (if a result fits, the
// same result with fewer trailing entries fits). It runs inside the guarded
// read and is never retained.
type HistoryBudget func(HistoryResult) (bool, error)

// Error is a stable, client-safe familyread failure. Compare with errors.Is
// against the exported values; Code returns the wire code.
type Error struct{ code string }

func (e *Error) Error() string { return e.code }

var (
	// ErrInvalidRange: zero/out-of-bounds instants or Start >= End.
	ErrInvalidRange = &Error{"invalid_range"}
	// ErrRangeTooLong: more than MaxRangeElapsed between Start and End.
	ErrRangeTooLong = &Error{"range_too_long"}
	// ErrInvalidLimit: Limit below zero or above MaxHistoryLimit.
	ErrInvalidLimit = &Error{"invalid_limit"}
	// ErrInvalidKind: a kind outside HistoryKinds.
	ErrInvalidKind = &Error{"invalid_kind"}
	// ErrInvalidBabyID: not a canonical lowercase UUID.
	ErrInvalidBabyID = &Error{"invalid_baby_id"}
	// ErrBabyNotFound: no live baby with that ID in the authorized family.
	ErrBabyNotFound = &Error{"baby_not_found"}
	// ErrInvalidCursor: forged, malformed, other-key, or bound to another
	// grant, family, baby, range, kinds, instance or epoch.
	ErrInvalidCursor = &Error{"invalid_cursor"}
	// ErrCursorExpired: the continuation is older than CursorTTL.
	ErrCursorExpired = &Error{"cursor_expired"}
	// ErrHistoryChanged: the family's data revision changed since the
	// continuation was issued; restart from the first page.
	ErrHistoryChanged = &Error{"history_changed"}
	// ErrResultTooLarge: the fixed fields or a single entity cannot fit the
	// result budget. Nothing is silently omitted.
	ErrResultTooLarge = &Error{"result_too_large"}
	// ErrInvalidTimezone: not a loadable IANA zone name ("Local", empty and
	// malformed names included).
	ErrInvalidTimezone = &Error{"invalid_timezone"}
	// ErrDataInvalid: a stored canonical record failed decoding or a schema
	// invariant. It is never repaired or skipped.
	ErrDataInvalid = &Error{"data_invalid"}
)

// Code returns the stable code of a familyread error, or "" for any other
// error (guard errors keep their own security codes).
func Code(err error) string {
	var coded *Error
	if errors.As(err, &coded) {
		return coded.code
	}
	return ""
}
