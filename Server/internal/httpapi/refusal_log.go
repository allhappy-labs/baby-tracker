package httpapi

import (
	"log/slog"
	"sort"
	"sync"
	"time"
)

// refusalLogInterval is the longest a marked refusal waits to be reported.
const refusalLogInterval = 10 * time.Second

type refusalLogKey struct {
	route  string
	status int
}

// refusalLog counts marked refusals per route and status and writes one
// summary line per key at most once per interval. A timer runs only while
// counts are pending, so an idle server schedules nothing.
type refusalLog struct {
	logger   *slog.Logger
	interval time.Duration
	mu       sync.Mutex
	counts   map[refusalLogKey]int
	since    time.Time
	timer    *time.Timer
}

func newRefusalLog(logger *slog.Logger, interval time.Duration) *refusalLog {
	return &refusalLog{logger: logger, interval: interval, counts: map[refusalLogKey]int{}}
}

func (l *refusalLog) add(route string, status int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.counts[refusalLogKey{route: route, status: status}]++
	if l.timer == nil {
		l.since = time.Now()
		l.timer = time.AfterFunc(l.interval, l.flush)
	}
}

func (l *refusalLog) flush() {
	l.mu.Lock()
	counts, since := l.counts, l.since
	l.counts, l.timer = map[refusalLogKey]int{}, nil
	l.mu.Unlock()
	keys := make([]refusalLogKey, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].route != keys[j].route {
			return keys[i].route < keys[j].route
		}
		return keys[i].status < keys[j].status
	})
	window := time.Since(since)
	for _, key := range keys {
		l.logger.Info("requests refused summary",
			slog.String("route", key.route),
			slog.Int("status", key.status),
			slog.Int("count", counts[key]),
			slog.Float64("window_seconds", window.Seconds()),
		)
	}
}
