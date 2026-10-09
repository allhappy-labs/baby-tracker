package records

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"babytracker/server/internal/protocol"
)

// Cleanup drops a brief switch and combines the untimed parts of one side it
// leaves adjacent. The side still running at the stop is a timed part, so it
// stays a part of its own.
func TestCleanupAtStopRemovesBriefSwitchAndCombinesMatchingBreasts(t *testing.T) {
	left, right := "left", "right"
	long, brief, short := uint32(300), uint32(3), uint32(120)
	started := "2026-09-28T12:00:00.000000Z"
	current := "2026-09-28T12:07:03.000000Z"
	timer := protocol.TimerStartPayload{
		BabyID: "10000000-0000-4000-8000-000000000001", TimerKind: "feed", StartedAt: started,
		TimeZoneID: "UTC", CurrentSide: &left, CurrentSideStartedAt: &current,
		CompletedComponents: []protocol.FeedComponent{
			{ID: "30000000-0000-4000-8000-000000000001", Kind: "breast", Side: &left, Seconds: &long},
			{ID: "30000000-0000-4000-8000-000000000002", Kind: "breast", Side: &right, Seconds: &brief},
			{ID: "30000000-0000-4000-8000-000000000003", Kind: "breast", Side: &left, Seconds: &short},
		},
	}
	stop := protocol.TimerStopPayload{BabyID: timer.BabyID, CompletedRecordID: "20000000-0000-4000-8000-000000000001",
		StoppedAt: "2026-09-28T12:09:03.000000Z", ComponentID: "30000000-0000-4000-8000-000000000004", CleanupShortBreastSegments: true}
	payload, kind, err := completedTimerPayload(timer, stop, time.Date(2026, 9, 28, 12, 9, 3, 0, time.UTC), protocol.DefaultLimits())
	if err != nil || kind != "feed" {
		t.Fatalf("completion kind=%q err=%v", kind, err)
	}
	feed := payload.(protocol.FeedPayload)
	if len(feed.Components) != 2 || feed.Components[0].ID != timer.CompletedComponents[0].ID ||
		feed.Components[0].Seconds == nil || *feed.Components[0].Seconds != 420 || feed.Components[0].StartedAt != nil {
		t.Fatalf("cleaned completion=%+v", feed)
	}
	running := feed.Components[1]
	if running.ID != stop.ComponentID || running.Seconds == nil || *running.Seconds != 120 ||
		running.StartedAt == nil || *running.StartedAt != current || running.EndedAt == nil || *running.EndedAt != stop.StoppedAt {
		t.Fatalf("running side part=%+v", running)
	}
}

func TestCleanupAtStopDiscardsAllBriefFeed(t *testing.T) {
	left, right := "left", "right"
	brief := uint32(3)
	started := "2026-09-28T12:00:00.000000Z"
	current := "2026-09-28T12:00:03.000000Z"
	timer := protocol.TimerStartPayload{
		BabyID: "10000000-0000-4000-8000-000000000001", TimerKind: "feed", StartedAt: started,
		TimeZoneID: "UTC", CurrentSide: &right, CurrentSideStartedAt: &current,
		CompletedComponents: []protocol.FeedComponent{{ID: "30000000-0000-4000-8000-000000000001", Kind: "breast", Side: &left, Seconds: &brief}},
	}
	stop := protocol.TimerStopPayload{BabyID: timer.BabyID, CompletedRecordID: "20000000-0000-4000-8000-000000000001",
		StoppedAt: "2026-09-28T12:00:11.000000Z", ComponentID: "30000000-0000-4000-8000-000000000002", CleanupShortBreastSegments: true}
	payload, _, err := completedTimerPayload(timer, stop, time.Date(2026, 9, 28, 12, 0, 11, 0, time.UTC), protocol.DefaultLimits())
	if err != nil || payload != nil {
		t.Fatalf("all-brief feed should have no completion; payload=%+v err=%v", payload, err)
	}
}

func TestBreastCleanupCannotChangeSleepStop(t *testing.T) {
	started := "2026-09-28T12:00:00.000000Z"
	timer := protocol.TimerStartPayload{BabyID: "10000000-0000-4000-8000-000000000001", TimerKind: "sleep",
		StartedAt: started, TimeZoneID: "UTC"}
	stop := protocol.TimerStopPayload{BabyID: timer.BabyID, CompletedRecordID: "20000000-0000-4000-8000-000000000001",
		StoppedAt: "2026-09-28T12:01:00.000000Z", CleanupShortBreastSegments: true}
	if _, _, err := completedTimerPayload(timer, stop, time.Date(2026, 9, 28, 12, 1, 0, 0, time.UTC), protocol.DefaultLimits()); err == nil {
		t.Fatal("sleep stop accepted breast cleanup")
	}
}

func TestBriefBreastFollowedByBottleCompletesAsBottle(t *testing.T) {
	left, formula := "left", "formula"
	brief := uint32(3)
	started := "2026-09-28T12:00:00.000000Z"
	timer := protocol.TimerStartPayload{BabyID: "10000000-0000-4000-8000-000000000001", TimerKind: "feed",
		StartedAt: started, TimeZoneID: "UTC",
		CompletedComponents: []protocol.FeedComponent{
			{ID: "30000000-0000-4000-8000-000000000001", Kind: "breast", Side: &left, Seconds: &brief},
			{ID: "30000000-0000-4000-8000-000000000002", Kind: "bottle", VolumeML: protocol.MustParseDecimal("60"), Contents: &formula},
		},
	}
	stop := protocol.TimerStopPayload{BabyID: timer.BabyID, CompletedRecordID: "20000000-0000-4000-8000-000000000001",
		StoppedAt: "2026-09-28T12:00:04.000000Z", CleanupShortBreastSegments: true}
	payload, _, err := completedTimerPayload(timer, stop, time.Date(2026, 9, 28, 12, 0, 4, 0, time.UTC), protocol.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	feed := payload.(protocol.FeedPayload)
	if feed.Kind != "bottle" || len(feed.Components) != 1 || feed.Components[0].Kind != "bottle" {
		t.Fatalf("completion=%+v", feed)
	}
}

// The side still running at the stop becomes one more breast part, so a timer
// already holding MaxSideIntervals breast parts cannot be stopped into a feed,
// while one part fewer stops into a feed of exactly MaxSideIntervals parts.
func TestStopRefusesABreastPartBeyondTheSideIntervalLimit(t *testing.T) {
	limits := protocol.DefaultLimits()
	left, seconds := "left", uint32(1)
	current := "2026-09-28T12:30:00.000000Z"
	stopped := time.Date(2026, 9, 28, 12, 31, 0, 0, time.UTC)
	for _, test := range []struct {
		held    int
		wantErr error
	}{{limits.MaxSideIntervals - 1, nil}, {limits.MaxSideIntervals, protocol.ErrInvalidTimestamp}} {
		parts := make([]protocol.FeedComponent, test.held)
		for index := range parts {
			parts[index] = protocol.FeedComponent{ID: fmt.Sprintf("30000000-0000-4000-8000-%012d", index+1),
				Kind: "breast", Side: &left, Seconds: &seconds}
		}
		timer := protocol.TimerStartPayload{BabyID: "10000000-0000-4000-8000-000000000001", TimerKind: "feed",
			StartedAt: "2026-09-28T12:00:00.000000Z", TimeZoneID: "UTC", CurrentSide: &left, CurrentSideStartedAt: &current,
			CompletedComponents: parts}
		stop := protocol.TimerStopPayload{BabyID: timer.BabyID, CompletedRecordID: "20000000-0000-4000-8000-000000000001",
			StoppedAt: "2026-09-28T12:31:00.000000Z", ComponentID: "31000000-0000-4000-8000-000000000001"}
		payload, kind, err := completedTimerPayload(timer, stop, stopped, limits)
		if !errors.Is(err, test.wantErr) {
			t.Fatalf("held=%d err=%v want %v", test.held, err, test.wantErr)
		}
		if test.wantErr != nil {
			continue
		}
		if feed, ok := payload.(protocol.FeedPayload); kind != "feed" || !ok || len(feed.Components) != limits.MaxSideIntervals ||
			feed.Components[len(feed.Components)-1].ID != stop.ComponentID {
			t.Fatalf("held=%d completion kind=%q payload=%+v", test.held, kind, payload)
		}
	}
}
