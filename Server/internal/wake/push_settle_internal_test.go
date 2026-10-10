package wake

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type warnings struct {
	mu      sync.Mutex
	records []slog.Record
}

func (w *warnings) Enabled(context.Context, slog.Level) bool { return true }
func (w *warnings) WithAttrs([]slog.Attr) slog.Handler       { return w }
func (w *warnings) WithGroup(string) slog.Handler            { return w }
func (w *warnings) Handle(_ context.Context, record slog.Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.records = append(w.records, record)
	return nil
}

func settleOf(at time.Time, rows int) pendingSettle {
	settle := pendingSettle{reservedAt: at}
	for i := range rows {
		job := pushJob{family: "f", device: string(rune('a' + i%26)), sequence: 2}
		switch i % 3 {
		case 0:
			settle.forget = append(settle.forget, job)
		case 1:
			settle.undelivered = append(settle.undelivered, job)
		default:
			settle.unsent = append(settle.unsent, job)
		}
	}
	return settle
}

// The kept settles are bounded: past maxSettleRows device rows the oldest
// are dropped (whole), logged once at Warn with the rows dropped; the
// newest is always kept. An empty settle is not kept.
func TestKeptSettlesAreBounded(t *testing.T) {
	logs := &warnings{}
	w := &Worker{Logger: slog.New(logs)}
	start := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	w.keepSettle(settleOf(start, 0))
	if len(w.settles) != 0 || w.settleRows != 0 {
		t.Fatalf("an empty settle was kept: %d settles, %d rows", len(w.settles), w.settleRows)
	}
	for i := range 4 {
		w.keepSettle(settleOf(start.Add(time.Duration(i)*time.Second), maxPushTargets))
	}
	if len(w.settles) != 4 || w.settleRows != maxSettleRows || len(logs.records) != 0 {
		t.Fatalf("at the cap: %d settles, %d rows, %d logs; want 4, %d, none", len(w.settles), w.settleRows, len(logs.records), maxSettleRows)
	}
	w.keepSettle(settleOf(start.Add(4*time.Second), 3))
	if len(w.settles) != 4 || w.settleRows != 3*maxPushTargets+3 || !w.settles[0].reservedAt.Equal(start.Add(time.Second)) ||
		!w.settles[3].reservedAt.Equal(start.Add(4*time.Second)) {
		t.Fatalf("past the cap: %d settles, %d rows, oldest at %v; want the oldest dropped", len(w.settles), w.settleRows, w.settles[0].reservedAt)
	}
	if len(logs.records) != 1 || logs.records[0].Level != slog.LevelWarn {
		t.Fatalf("logs %+v, want one Warn", logs.records)
	}
	dropped := int64(-1)
	logs.records[0].Attrs(func(attr slog.Attr) bool {
		if attr.Key == "rows" {
			dropped = attr.Value.Int64()
		}
		return true
	})
	if dropped != maxPushTargets {
		t.Fatalf("the Warn says %d rows dropped, want %d", dropped, maxPushTargets)
	}
	if maxSettleRows != 4*maxPushTargets {
		t.Fatalf("maxSettleRows %d", maxSettleRows)
	}
}
