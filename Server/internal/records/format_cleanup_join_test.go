package records

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5"
)

// Migration 27 joins past feeds and sleeps into sessions by the rule live
// tracking uses (BabyStore.continuedSessionID and sleepFalls in
// SessionJoin.swift, standard break limits feed 1800 s and sleep 900 s), and
// publishes every converted or joined record as one appended journal head.

const (
	fcBabyB   = "fc000000-0000-4000-8000-000000000011"
	fcBabyC   = "fc000000-0000-4000-8000-000000000012"
	fcEditorB = "fc000000-0000-4000-8000-000000000005"
	fcDeviceB = "fc000000-0000-4000-8000-000000000006"
)

// fcPartID numbers the join fixture's records so that id order is number order.
func fcPartID(n int) string { return fmt.Sprintf("fc000000-0000-4000-8000-%012d", 100000+n) }

// fcAt is a UTC instant in September 2026.
func fcAt(day int, clock string) string { return fmt.Sprintf("2026-09-%02dT%sZ", day, clock) }

// fcPart is one seeded record. Links name parts by number; 0 is no link.
type fcPart struct {
	n          int
	baby       string
	kind       string // feed, sleep, diaper, supplement, measurement, timer
	feedKind   string // breast, bottle, solids; for a timer, its timer kind
	version    int    // component_version (feed, timer) or payload_version (sleep)
	started    string
	ended      string // empty while still open
	link       int    // the session link stored at schema 26
	tombstoned bool
	entity     int64  // entity version; 0 means 1
	editor     string // last editor and actor of its head; empty means fcAccount
	device     string // device of its head; empty means fcDevice
	want       int    // the session link after 27
}

func (part fcPart) entityVersion() int64 {
	if part.entity == 0 {
		return 1
	}
	return part.entity
}

func (part fcPart) editorID() string {
	if part.editor == "" {
		return fcAccount
	}
	return part.editor
}

func (part fcPart) deviceID() string {
	if part.device == "" {
		return fcDevice
	}
	return part.device
}

// converted reports whether 27 moves the record's format: a live feed below
// v4 or a live sleep below v2.
func (part fcPart) converted() bool {
	return !part.tombstoned && ((part.kind == "feed" && part.version < 4) || (part.kind == "sleep" && part.version < 2))
}

// joined reports whether 27 links a feed or sleep that had no link.
func (part fcPart) joined() bool {
	return (part.kind == "feed" || part.kind == "sleep") && part.link == 0 && part.want != 0
}

// published reports whether 27 appends a journal head for the record: it was
// converted, joined, or both.
func (part fcPart) published() bool { return part.converted() || part.joined() || part.relinked() }

// relinked reports whether 27 moved a linked part with its root (ruling B-R13).
func (part fcPart) relinked() bool {
	return (part.kind == "feed" || part.kind == "sleep") && part.link != 0 && part.want != part.link
}

func fcOptional(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func fcLink(n int) any {
	if n == 0 {
		return nil
	}
	return fcPartID(n)
}

const (
	fcBreast = "breast"
	fcBottle = "bottle"
	fcSolids = "solids"
)

// fcJoinParts is the join matrix. Every case sits on its own day or baby so
// the cases cannot reach one another; the comments say why each link is what
// it is.
var fcJoinParts = []fcPart{
	// Sep 10, baby A: a gap of exactly 1800 s joins; 1801 s does not. Baby
	// B's feed inside A's gap is not A's session (it would be the latest end).
	{n: 1, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(10, "08:00:00"), ended: fcAt(10, "08:10:00")},
	{n: 2, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(10, "08:40:00"), ended: fcAt(10, "08:50:00"),
		entity: 3, editor: fcEditorB, device: fcDeviceB, want: 1},
	{n: 3, baby: fcBaby, kind: "feed", feedKind: fcBottle, version: 3, started: fcAt(10, "09:20:01"), ended: fcAt(10, "09:25:00")},
	{n: 4, baby: fcBabyB, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(10, "08:20:00"), ended: fcAt(10, "08:30:00"),
		editor: fcEditorB, device: fcDeviceB},
	// Sep 11: a sleep gap of exactly 900 s joins; 901 s does not.
	{n: 5, baby: fcBaby, kind: "sleep", version: 1, started: fcAt(11, "13:00:00"), ended: fcAt(11, "14:00:00")},
	{n: 6, baby: fcBaby, kind: "sleep", version: 1, started: fcAt(11, "14:15:00"), ended: fcAt(11, "15:00:00"),
		editor: fcEditorB, device: fcDeviceB, want: 5},
	{n: 7, baby: fcBaby, kind: "sleep", version: 1, started: fcAt(11, "15:15:01"), ended: fcAt(11, "16:00:00")},
	// Sep 12: a completed sleep inside the gap keeps the feeds apart. A sleep
	// ending exactly at the session's end, or starting exactly at the part's
	// start, does not occupy the gap (strict comparisons, as sleepFalls).
	{n: 8, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(12, "08:00:00"), ended: fcAt(12, "08:10:00")},
	{n: 9, baby: fcBaby, kind: "sleep", version: 1, started: fcAt(12, "08:15:00"), ended: fcAt(12, "08:25:00")},
	{n: 10, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(12, "08:30:00"), ended: fcAt(12, "08:40:00")},
	{n: 11, baby: fcBaby, kind: "sleep", version: 1, started: fcAt(12, "10:30:00"), ended: fcAt(12, "11:10:00")},
	{n: 12, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(12, "11:00:00"), ended: fcAt(12, "11:10:00")},
	{n: 13, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(12, "11:20:00"), ended: fcAt(12, "11:30:00"), want: 12},
	{n: 14, baby: fcBaby, kind: "sleep", version: 1, started: fcAt(12, "11:50:00"), ended: fcAt(12, "12:30:00")},
	{n: 15, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(12, "11:50:00"), ended: fcAt(12, "12:00:00"), want: 12},
	// Sep 13: a diaper, a supplement, a measurement and the other baby's
	// sleep in the gap do not block; joins chain to the first part's id;
	// solids never join and are never joined to.
	{n: 16, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(13, "08:00:00"), ended: fcAt(13, "08:10:00")},
	{n: 17, baby: fcBaby, kind: "diaper", started: fcAt(13, "08:15:00")},
	{n: 18, baby: fcBaby, kind: "supplement", started: fcAt(13, "08:16:00")},
	{n: 19, baby: fcBaby, kind: "measurement", started: fcAt(13, "08:17:00")},
	{n: 20, baby: fcBabyB, kind: "sleep", version: 1, started: fcAt(13, "08:12:00"), ended: fcAt(13, "08:20:00")},
	{n: 21, baby: fcBaby, kind: "feed", feedKind: fcBottle, version: 3, started: fcAt(13, "08:30:00"), ended: fcAt(13, "08:35:00"), want: 16},
	{n: 22, baby: fcBaby, kind: "feed", feedKind: fcSolids, version: 2, started: fcAt(13, "08:50:00")},
	{n: 23, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(13, "09:00:00"), ended: fcAt(13, "09:10:00"), want: 16},
	{n: 24, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(13, "09:35:00"), ended: fcAt(13, "09:40:00"), want: 16},
	{n: 25, baby: fcBaby, kind: "feed", feedKind: fcSolids, version: 2, started: fcAt(13, "10:00:00"), ended: fcAt(13, "10:05:00")},
	{n: 26, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(13, "10:20:00"), ended: fcAt(13, "10:30:00")},
	// Sep 14 (before the cutover): an unlinked part joins whatever its stored
	// version (v4 feed 28, v2 sleep 36); existing links stay unless their
	// whole session moves: 32, the root 33 is linked to, joins 31 (10 minutes
	// earlier) and takes 33 with it (ruling B-R13).
	{n: 27, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(14, "08:00:00"), ended: fcAt(14, "08:10:00")},
	{n: 28, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 4, started: fcAt(14, "08:20:00"), ended: fcAt(14, "08:30:00"), want: 27},
	{n: 29, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(14, "08:40:00"), ended: fcAt(14, "08:50:00"), want: 27},
	{n: 30, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 4, started: fcAt(14, "09:00:00"), ended: fcAt(14, "09:10:00"), link: 27, want: 27},
	{n: 31, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(14, "10:40:00"), ended: fcAt(14, "10:50:00")},
	{n: 32, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(14, "11:00:00"), ended: fcAt(14, "11:10:00"), want: 31},
	{n: 33, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 4, started: fcAt(14, "11:15:00"), ended: fcAt(14, "11:20:00"), link: 32, want: 31},
	{n: 34, baby: fcBaby, kind: "sleep", version: 1, started: fcAt(14, "13:00:00"), ended: fcAt(14, "14:00:00")},
	{n: 35, baby: fcBaby, kind: "sleep", version: 2, started: fcAt(14, "14:05:00"), ended: fcAt(14, "14:30:00"), link: 34, want: 34},
	{n: 36, baby: fcBaby, kind: "sleep", version: 2, started: fcAt(14, "14:40:00"), ended: fcAt(14, "15:00:00"), want: 34},
	{n: 37, baby: fcBaby, kind: "sleep", version: 1, started: fcAt(14, "15:05:00"), ended: fcAt(14, "15:30:00"), want: 34},
	// Sessions across midnight keep their first part's id.
	{n: 38, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(15, "23:40:00"), ended: fcAt(15, "23:55:00")},
	{n: 39, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(16, "00:15:00"), ended: fcAt(16, "00:25:00"), want: 38},
	{n: 40, baby: fcBaby, kind: "sleep", version: 1, started: fcAt(16, "23:00:00"), ended: fcAt(16, "23:50:00")},
	{n: 41, baby: fcBaby, kind: "sleep", version: 1, started: fcAt(17, "00:00:00"), ended: fcAt(17, "05:00:00"), want: 40},
	// Sep 18: tombstoned entries take no part. The deleted feed would bridge
	// 42 and 44; the deleted sleep would keep 45 and 47 apart.
	{n: 42, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(18, "08:00:00"), ended: fcAt(18, "08:10:00")},
	{n: 43, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(18, "08:20:00"), ended: fcAt(18, "08:50:00"), tombstoned: true, entity: 2},
	{n: 44, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(18, "09:10:00"), ended: fcAt(18, "09:20:00")},
	{n: 45, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(18, "10:00:00"), ended: fcAt(18, "10:10:00")},
	{n: 46, baby: fcBaby, kind: "sleep", version: 1, started: fcAt(18, "10:12:00"), ended: fcAt(18, "10:20:00"), tombstoned: true, entity: 2},
	{n: 47, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(18, "10:30:00"), ended: fcAt(18, "10:40:00"), want: 45},
	// Baby B, Sep 18-19: an open sleep blocks every later feed join of its
	// own baby only, and not joins before it started.
	{n: 48, baby: fcBabyB, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(18, "08:00:00"), ended: fcAt(18, "08:10:00")},
	{n: 49, baby: fcBabyB, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(18, "08:20:00"), ended: fcAt(18, "08:30:00"), want: 48},
	{n: 50, baby: fcBabyB, kind: "sleep", version: 1, started: fcAt(19, "08:00:00")},
	{n: 51, baby: fcBabyB, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(19, "09:00:00"), ended: fcAt(19, "09:10:00")},
	{n: 52, baby: fcBabyB, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(19, "09:20:00"), ended: fcAt(19, "09:30:00")},
	// Sep 19, baby A: two sessions end together. Swift's max(by:) keeps the
	// first in (startedAt, id) order, so the one that started first wins even
	// though the other has the smaller id.
	{n: 53, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 4, started: fcAt(19, "08:10:00"), ended: fcAt(19, "08:30:00")},
	{n: 54, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 4, started: fcAt(19, "08:00:00"), ended: fcAt(19, "08:30:00")},
	{n: 55, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(19, "08:40:00"), ended: fcAt(19, "08:50:00"), want: 54},
	// Sep 20, baby A: a past feed whose session a running feed timer
	// continues stays where it is: moving it would split the timer's part from
	// its session.
	{n: 56, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(20, "08:00:00"), ended: fcAt(20, "08:10:00")},
	{n: 57, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(20, "08:20:00"), ended: fcAt(20, "08:30:00")},
	{n: 58, baby: fcBaby, kind: "timer", feedKind: "feed", version: 5, started: fcAt(20, "08:40:00"), link: 57, want: 57},
	// Baby C: a running sleep timer blocks feed joins after it started, not
	// before.
	{n: 59, baby: fcBabyC, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(19, "08:00:00"), ended: fcAt(19, "08:10:00")},
	{n: 60, baby: fcBabyC, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(19, "08:20:00"), ended: fcAt(19, "08:30:00"), want: 59},
	{n: 61, baby: fcBabyC, kind: "timer", feedKind: "sleep", version: 5, started: fcAt(20, "07:00:00")},
	{n: 62, baby: fcBabyC, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(20, "08:00:00"), ended: fcAt(20, "08:10:00")},
	{n: 63, baby: fcBabyC, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(20, "08:20:00"), ended: fcAt(20, "08:30:00")},
	// Oct 2, around the redesign cutover (16:37:50Z, ruling B-R9): parts that
	// started before it are evaluated whatever their version; from it on the
	// installed build decided every part live, so an unlinked part stays.
	{n: 64, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 4, started: "2026-10-02T15:50:00Z", ended: "2026-10-02T16:00:00Z"},
	{n: 65, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 4, started: "2026-10-02T16:20:00Z", ended: "2026-10-02T16:30:00Z", want: 64},
	{n: 66, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 4, started: "2026-10-02T16:37:49Z", want: 64},
	{n: 67, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 4, started: "2026-10-02T16:37:50Z", ended: "2026-10-02T16:45:00Z"},
	{n: 68, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 4, started: "2026-10-02T16:50:00Z", ended: "2026-10-02T17:00:00Z"},
	// A v2 sleep a current client already linked (here by a Combine the rule
	// itself would not make: a 2 h gap) keeps its link and is not published.
	{n: 69, baby: fcBaby, kind: "sleep", version: 1, started: "2026-10-02T09:00:00Z", ended: "2026-10-02T09:30:00Z"},
	{n: 70, baby: fcBaby, kind: "sleep", version: 2, started: "2026-10-02T09:40:00Z", ended: "2026-10-02T10:00:00Z", want: 69},
	{n: 71, baby: fcBaby, kind: "sleep", version: 2, started: "2026-10-02T12:00:00Z", ended: "2026-10-02T12:30:00Z", link: 69, want: 69},
	{n: 72, baby: fcBaby, kind: "sleep", version: 2, started: "2026-10-02T18:00:00Z", ended: "2026-10-02T19:00:00Z"},
	{n: 73, baby: fcBaby, kind: "sleep", version: 2, started: "2026-10-02T19:05:00Z", ended: "2026-10-02T20:00:00Z"},
	// Baby B, Sep 21: a sleep session is continued only once every part has
	// ended. 74 ended 10 minutes before 76 starts, but 75, linked to it, is
	// still running, so 76 starts a session of its own. (Baby B, whose open
	// sleep 50 already keeps its feeds apart, so no feed case is touched.)
	{n: 74, baby: fcBabyB, kind: "sleep", version: 1, started: fcAt(21, "08:00:00"), ended: fcAt(21, "09:00:00")},
	{n: 75, baby: fcBabyB, kind: "sleep", version: 2, started: fcAt(21, "09:05:00"), link: 74, want: 74},
	{n: 76, baby: fcBabyB, kind: "sleep", version: 1, started: fcAt(21, "09:10:00"), ended: fcAt(21, "10:00:00")},
	// Sep 22, baby A: a part still running ends its session at its start.
	// 78 is open from 08:30, so 79 (25 minutes later) continues 77's session,
	// though 77 itself ended 45 minutes before 79.
	{n: 77, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(22, "08:00:00"), ended: fcAt(22, "08:10:00")},
	{n: 78, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(22, "08:30:00"), want: 77},
	{n: 79, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 2, started: fcAt(22, "08:55:00"), ended: fcAt(22, "09:05:00"), want: 77},
	// Sep 23, baby A: a sleep root whose session a linked sleep already
	// continues joins 80 (10 minutes earlier), and its member moves with it.
	{n: 80, baby: fcBaby, kind: "sleep", version: 1, started: fcAt(23, "08:00:00"), ended: fcAt(23, "09:00:00")},
	{n: 81, baby: fcBaby, kind: "sleep", version: 1, started: fcAt(23, "09:10:00"), ended: fcAt(23, "10:00:00"), want: 80},
	{n: 82, baby: fcBaby, kind: "sleep", version: 2, started: fcAt(23, "10:05:00"), ended: fcAt(23, "10:30:00"), link: 81, want: 80},
	// Ruling B-R13: a root an old build's Combine made moves with its whole
	// session. Sep 24: a single breast feed, then 9 minutes later a breast +
	// bottle session (root 84, member 85) become one session of 3 parts.
	{n: 83, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 4, started: fcAt(24, "10:23:00"), ended: fcAt(24, "10:26:00")},
	{n: 84, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 4, started: fcAt(24, "10:35:00"), ended: fcAt(24, "10:45:00"), want: 83},
	{n: 85, baby: fcBaby, kind: "feed", feedKind: fcBottle, version: 4, started: fcAt(24, "10:45:00"), ended: fcAt(24, "10:50:00"), link: 84, want: 83},
	// Sep 25: chained roots. 89 is 25 minutes after 88 ended but 35 after its
	// own root 87 did, so it joins only because 88 moved with 87 and counts in
	// 86's session end.
	{n: 86, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 4, started: fcAt(25, "08:00:00"), ended: fcAt(25, "08:10:00")},
	{n: 87, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 4, started: fcAt(25, "08:20:00"), ended: fcAt(25, "08:30:00"), want: 86},
	{n: 88, baby: fcBaby, kind: "feed", feedKind: fcBottle, version: 4, started: fcAt(25, "08:30:00"), ended: fcAt(25, "08:40:00"), link: 87, want: 86},
	{n: 89, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 4, started: fcAt(25, "09:05:00"), ended: fcAt(25, "09:15:00"), want: 86},
	{n: 90, baby: fcBaby, kind: "feed", feedKind: fcBottle, version: 4, started: fcAt(25, "09:15:00"), ended: fcAt(25, "09:20:00"), link: 89, want: 86},
	// Sep 26: a root more than the limit after the session before it stays,
	// and so does its member.
	{n: 93, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 4, started: fcAt(26, "08:00:00"), ended: fcAt(26, "08:10:00")},
	{n: 94, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 4, started: fcAt(26, "08:40:01"), ended: fcAt(26, "08:50:00")},
	{n: 95, baby: fcBaby, kind: "feed", feedKind: fcBottle, version: 4, started: fcAt(26, "08:50:00"), ended: fcAt(26, "08:55:00"), link: 94, want: 94},
	// Sep 27: a sleep in the gap keeps a root and its member where they are.
	{n: 96, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 4, started: fcAt(27, "08:00:00"), ended: fcAt(27, "08:10:00")},
	{n: 97, baby: fcBaby, kind: "sleep", version: 1, started: fcAt(27, "08:12:00"), ended: fcAt(27, "08:18:00")},
	{n: 98, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 4, started: fcAt(27, "08:25:00"), ended: fcAt(27, "08:35:00")},
	{n: 99, baby: fcBaby, kind: "feed", feedKind: fcBottle, version: 4, started: fcAt(27, "08:35:00"), ended: fcAt(27, "08:40:00"), link: 98, want: 98},
	// Oct 2 after the cutover: a root the installed build made is never
	// moved, though it starts 10 minutes after 68 ended.
	{n: 91, baby: fcBaby, kind: "feed", feedKind: fcBreast, version: 4, started: "2026-10-02T17:10:00Z", ended: "2026-10-02T17:20:00Z"},
	{n: 92, baby: fcBaby, kind: "feed", feedKind: fcBottle, version: 4, started: "2026-10-02T17:20:00Z", ended: "2026-10-02T17:25:00Z", link: 91, want: 91},
}

// fcJournalFromSnapshot writes a record's journal head as the schema-26
// server built it: that server's snapshot SQL entity for the record.
func fcJournalFromSnapshot(t *testing.T, tx pgx.Tx, sequence int64, id string, version int64, actor, device, operation string) {
	t.Helper()
	result, err := tx.Exec(t.Context(), `INSERT INTO mutation_journal(family_id,sequence,entity_kind,entity_id,entity_version,actor_account_id,device_id,operation,canonical_entity,accepted_at)
		SELECT $1,$2,typed.canonical_entity->>'entity_kind',typed.id,$3,$4,$5,$6,typed.canonical_entity,$7
		FROM (`+fcSchema26SnapshotUnion+`) typed WHERE typed.id=$8`, fcFamily, sequence, version, actor, device, operation, fcAccepted, id)
	if err != nil || result.RowsAffected() != 1 {
		t.Fatalf("journal head for %s: rows=%d err=%v", id, result.RowsAffected(), err)
	}
}

// fcSeedParts writes babies B and C and then every part in the given order,
// each with its journal head, and returns the family's high water.
func fcSeedParts(t *testing.T, fixture *testpg.Fixture, parts []fcPart) int64 {
	t.Helper()
	tx := fcTenantTx(t, fixture)
	sequence := int64(1)
	for index, baby := range []string{fcBabyB, fcBabyC} {
		sequence++
		fcExec(t, tx, `INSERT INTO babies(family_id,id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,last_sequence,storage_bytes,name,normalized_name,normalization_version,birth_date,sort_order)
			VALUES($1,$2,1,$3,$3,$4,$5,$6,1,$7,$7,'nfc-trim-fold-nfc-v1','2026-08-01',$8)`,
			fcFamily, baby, fcAccount, fcClientTime, fcAccepted, sequence, fmt.Sprintf("baby%d", index+2), index+1)
		fcJournalFromSnapshot(t, tx, sequence, baby, 1, fcAccount, fcDevice, "create")
	}
	for _, part := range parts {
		sequence++
		var tombstoned any
		operation := "create"
		if part.tombstoned {
			tombstoned, operation = fcAccepted, "delete"
		}
		id := fcPartID(part.n)
		common := []any{fcFamily, id, part.baby, part.entityVersion(), fcAccount, part.editorID(), fcClientTime, fcAccepted, tombstoned, sequence}
		switch part.kind {
		case "feed":
			fcExec(t, tx, `INSERT INTO feed_entries(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,kind,started_at,ended_at,time_zone_id,component_version,feeding_session_id)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,200,$11,$12,$13,'Europe/Zurich',$14,$15)`,
				append(common, part.feedKind, part.started, fcOptional(part.ended), part.version, fcLink(part.link))...)
			// putFeed: the components replace whatever the legacy projection
			// derived; a tombstone removed them.
			fcExec(t, tx, `DELETE FROM feed_components WHERE family_id=$1 AND feed_id=$2`, fcFamily, id)
			if !part.tombstoned {
				component := fmt.Sprintf("fc000000-0000-4000-8000-%012d", 200000+part.n)
				switch part.feedKind {
				case fcBreast:
					fcExec(t, tx, `INSERT INTO feed_components(family_id,feed_id,component_id,position,kind,side,seconds) VALUES($1,$2,$3,0,'breast','left',300)`, fcFamily, id, component)
				case fcBottle:
					fcExec(t, tx, `INSERT INTO feed_components(family_id,feed_id,component_id,position,kind,volume_ml,contents) VALUES($1,$2,$3,0,'bottle',90,'formula')`, fcFamily, id, component)
				case fcSolids:
					fcExec(t, tx, `INSERT INTO feed_components(family_id,feed_id,component_id,position,kind,foods,amount) VALUES($1,$2,$3,0,'solids',ARRAY['banana'],'some')`, fcFamily, id, component)
				}
			}
		case "sleep":
			fcExec(t, tx, `INSERT INTO sleep_entries(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,started_at,ended_at,time_zone_id,payload_version,sleep_session_id)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,150,$11,$12,'Europe/Zurich',$13,$14)`,
				append(common, part.started, fcOptional(part.ended), part.version, fcLink(part.link))...)
		case "diaper":
			fcExec(t, tx, `INSERT INTO diaper_entries(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,started_at,time_zone_id,contents)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,150,$11,'Europe/Zurich','dirty')`, append(common, part.started)...)
		case "supplement":
			fcExec(t, tx, `INSERT INTO supplement_entries(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,started_at,time_zone_id,logged_at,name)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,150,$11,'Europe/Zurich',$11,'Vitamin D')`, append(common, part.started)...)
		case "measurement":
			fcExec(t, tx, `INSERT INTO measurement_entries(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,measured_at,logged_at,time_zone_id,weight_kg)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,150,$11,$11,'Europe/Zurich',4.2)`, append(common, part.started)...)
		case "timer":
			operation = "timer_start"
			var side, sideStarted any
			if part.feedKind == "feed" {
				side, sideStarted = "left", part.started
			}
			fcExec(t, tx, `INSERT INTO active_timers(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,timer_kind,started_at,time_zone_id,current_side,current_side_started_at,component_version,session_id)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,100,$11,$12,'Europe/Zurich',$13,$14,$15,$16)`,
				append(common, part.feedKind, part.started, side, sideStarted, part.version, fcLink(part.link))...)
		default:
			t.Fatalf("unknown part kind %q", part.kind)
		}
		fcJournalFromSnapshot(t, tx, sequence, id, part.entityVersion(), part.editorID(), part.deviceID(), operation)
	}
	fcExec(t, tx, `UPDATE families SET next_sequence=$2 WHERE id=$1`, fcFamily, sequence)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return sequence
}

// fcAppended is one journal head 000027 appends.
type fcAppended struct {
	kind, id          string
	sequence, version int64
	actor, device     string
}

// fcAssertAppended checks that the journal rows after before are exactly
// want, in order: ordinary updates by the record's last editor from its
// previous head's device, all stamped with the migration's time, each now the
// record's head; and that next_sequence covers them.
func fcAssertAppended(t *testing.T, tx pgx.Tx, before int64, want []fcAppended) {
	t.Helper()
	if got := fcCount(t, tx, `SELECT next_sequence FROM families WHERE id=$1`, fcFamily); got != before+int64(len(want)) {
		t.Errorf("next_sequence=%d, want %d", got, before+int64(len(want)))
	}
	type row struct {
		fcAppended
		operation string
		accepted  time.Time
	}
	rows, err := tx.Query(t.Context(), `SELECT sequence,entity_kind,entity_id::text,entity_version,actor_account_id::text,device_id::text,operation,accepted_at
		FROM mutation_journal WHERE family_id=$1 AND sequence>$2 ORDER BY sequence`, fcFamily, before)
	if err != nil {
		t.Fatal(err)
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.sequence, &r.kind, &r.id, &r.version, &r.actor, &r.device, &r.operation, &r.accepted); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("appended %d journal rows, want %d\ngot  %+v\nwant %+v", len(got), len(want), got, want)
	}
	seeded, err := time.Parse(time.RFC3339Nano, fcAccepted)
	if err != nil {
		t.Fatal(err)
	}
	for index, appended := range got {
		if appended.fcAppended != want[index] || appended.operation != "update" {
			t.Errorf("appended row %d = %+v %s, want %+v update", index, appended.fcAppended, appended.operation, want[index])
		}
		if !appended.accepted.Equal(got[0].accepted) || !appended.accepted.After(seeded) {
			t.Errorf("appended row %d accepted_at=%s, first %s", index, appended.accepted, got[0].accepted)
		}
		table := map[string]string{"feed": "feed_entries", "sleep": "sleep_entries"}[appended.kind]
		if table == "" {
			t.Errorf("appended row %d publishes a %s", index, appended.kind)
			continue
		}
		var version, last int64
		var accepted time.Time
		if err := tx.QueryRow(t.Context(), `SELECT entity_version,last_sequence,accepted_at FROM `+table+` WHERE family_id=$1 AND id=$2`,
			fcFamily, appended.id).Scan(&version, &last, &accepted); err != nil {
			t.Fatal(err)
		}
		if version != appended.version || last != appended.sequence || !accepted.Equal(appended.accepted) {
			t.Errorf("%s %s version=%d head=%d accepted=%s, want %d/%d/%s", appended.kind, appended.id, version, last, accepted,
				appended.version, appended.sequence, appended.accepted)
		}
	}
}

// fcLinks reads every part's session link after the migration.
func fcLinks(t *testing.T, tx pgx.Tx, parts []fcPart) map[int]int {
	t.Helper()
	byID := map[string]int{"": 0}
	for _, part := range parts {
		byID[fcPartID(part.n)] = part.n
	}
	links := map[int]int{}
	for _, part := range parts {
		query := map[string]string{
			"feed":  `SELECT coalesce(feeding_session_id::text,'') FROM feed_entries WHERE family_id=$1 AND id=$2`,
			"sleep": `SELECT coalesce(sleep_session_id::text,'') FROM sleep_entries WHERE family_id=$1 AND id=$2`,
			"timer": `SELECT coalesce(session_id::text,'') FROM active_timers WHERE family_id=$1 AND id=$2`,
		}[part.kind]
		if query == "" {
			continue
		}
		var link string
		if err := tx.QueryRow(t.Context(), query, fcFamily, fcPartID(part.n)).Scan(&link); err != nil {
			t.Fatal(err)
		}
		n, known := byID[link]
		if !known {
			t.Fatalf("part %d links to unknown %s", part.n, link)
		}
		links[part.n] = n
	}
	return links
}

// fcAppendedRows is the appended journal as text, without the migration's
// timestamp, for comparing two runs.
func fcAppendedRows(t *testing.T, tx pgx.Tx, before int64) []string {
	t.Helper()
	rows, err := tx.Query(t.Context(), `SELECT concat_ws(' ',sequence,entity_kind,entity_id,entity_version,actor_account_id,device_id,operation,
			(canonical_entity #- '{meta,accepted_at}')::text)
		FROM mutation_journal WHERE family_id=$1 AND sequence>$2 ORDER BY sequence`, fcFamily, before)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		result = append(result, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestFormatCleanupMigrationJoinsPastEntriesIntoSessions(t *testing.T) {
	fixture := testpg.NewThrough(t, 26)
	fcSeedFamily(t, fixture)
	before := fcSeedParts(t, fixture, fcJoinParts)
	// The seeds are what the schema-26 server built.
	fcAssertHeadsMatchSnapshot(t, fixture, fcSchema26SnapshotUnion)

	if err := fixture.MigrateThrough(t.Context(), 27); err != nil {
		t.Fatal(err)
	}
	tx := fcTenantTx(t, fixture)

	t.Run("sessions follow the live rule", func(t *testing.T) {
		links := fcLinks(t, tx, fcJoinParts)
		for _, part := range fcJoinParts {
			if got, isLinked := links[part.n]; isLinked && got != part.want {
				baby := map[string]string{fcBaby: "A", fcBabyB: "B", fcBabyC: "C"}[part.baby]
				t.Errorf("part %d (%s %s, baby %s) links to part %d, want %d", part.n, part.kind, part.started, baby, got, part.want)
			}
		}
	})

	t.Run("every converted or joined record is published once", func(t *testing.T) {
		var want []fcAppended
		for _, kind := range []string{"feed", "sleep"} { // snapshot kind order 2, 3
			ordered := slices.Clone(fcJoinParts)
			slices.SortFunc(ordered, func(left, right fcPart) int { return left.n - right.n }) // id order
			for _, part := range ordered {
				if part.kind == kind && part.published() {
					want = append(want, fcAppended{kind: kind, id: fcPartID(part.n), sequence: before + int64(len(want)) + 1,
						version: part.entityVersion() + 1, actor: part.editorID(), device: part.deviceID()})
				}
			}
		}
		fcAssertAppended(t, tx, before, want)
		// Heads follow the union: a part both converted and joined is
		// published once, a part only joined (a pre-cutover v4 feed or v2
		// sleep), only relinked (moved with its root) or only converted once,
		// and a record needing none of these (linked or post-cutover current
		// format, tombstoned, other kind) not at all.
		var both, joinedOnly, relinkedOnly, convertedOnly, neither int
		for _, part := range fcJoinParts {
			switch {
			case part.converted() && part.joined():
				both++
			case part.joined():
				joinedOnly++
			case part.relinked() && !part.converted():
				relinkedOnly++
			case part.converted():
				convertedOnly++
			default:
				neither++
			}
		}
		if both == 0 || joinedOnly == 0 || relinkedOnly == 0 || convertedOnly == 0 || neither == 0 ||
			len(want) != both+joinedOnly+relinkedOnly+convertedOnly {
			t.Errorf("heads=%d, want both %d + joined only %d + relinked only %d + converted only %d (neither %d)",
				len(want), both, joinedOnly, relinkedOnly, convertedOnly, neither)
		}
		if got := fcCount(t, tx, `SELECT count(*) FROM (SELECT entity_id FROM mutation_journal WHERE family_id=$1 AND sequence>$2
			GROUP BY entity_id HAVING count(*)>1) twice`, fcFamily, before); got != 0 {
			t.Errorf("%d records published more than once", got)
		}
	})

	t.Run("appended heads equal the snapshot and carry the session ids", func(t *testing.T) {
		if compared := fcAssertHeadsMatchSnapshot(t, fixture, fcSchema27SnapshotUnion); compared != len(fcJoinParts)+3 {
			t.Errorf("compared %d heads, want %d", compared, len(fcJoinParts)+3)
		}
		for _, part := range fcJoinParts {
			if !part.published() {
				continue
			}
			key := map[string]string{"feed": "feeding_session_id", "sleep": "sleep_session_id"}[part.kind]
			var raw, session string
			var present bool
			if err := tx.QueryRow(t.Context(), `SELECT canonical_entity::text,canonical_entity->'payload' ? $3,coalesce(canonical_entity->'payload'->>$3,'')
				FROM mutation_journal WHERE family_id=$1 AND sequence>$4 AND entity_id=$2`, fcFamily, fcPartID(part.n), key, before).Scan(&raw, &present, &session); err != nil {
				t.Fatalf("part %d head: %v", part.n, err)
			}
			want := ""
			if part.want != 0 {
				want = fcPartID(part.want)
			}
			if !present || session != want {
				t.Errorf("part %d head %s=%q present=%t, want %q", part.n, key, session, present, want)
			}
			fcAssertCurrentEntity(t, tx, fmt.Sprintf("part %d head", part.n), raw)
		}
	})

	t.Run("the join is deterministic", func(t *testing.T) {
		// The same records written in the opposite order (so stored in a
		// different physical order, with different old sequences) join and
		// publish identically.
		reversed := slices.Clone(fcJoinParts)
		slices.Reverse(reversed)
		other := testpg.NewThrough(t, 26)
		fcSeedFamily(t, other)
		if otherBefore := fcSeedParts(t, other, reversed); otherBefore != before {
			t.Fatalf("reversed seed high water %d, want %d", otherBefore, before)
		}
		if err := other.MigrateThrough(t.Context(), 27); err != nil {
			t.Fatal(err)
		}
		otherTx := fcTenantTx(t, other)
		if got, want := fcLinks(t, otherTx, fcJoinParts), fcLinks(t, tx, fcJoinParts); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("links differ\nreversed %v\nforward  %v", got, want)
		}
		got, want := fcAppendedRows(t, otherTx, before), fcAppendedRows(t, tx, before)
		if !slices.Equal(got, want) {
			t.Errorf("appended journal differs\nreversed %q\nforward  %q", got, want)
		}
		if len(want) == 0 {
			t.Error("nothing appended")
		}
	})
}
