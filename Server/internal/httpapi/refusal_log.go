package httpapi

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
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
	// faults spaces the refusal fault lines (logRefusal) of the handler
	// this log serves.
	faults *refusalFaults
}

func newRefusalLog(logger *slog.Logger, interval time.Duration) *refusalLog {
	return &refusalLog{logger: logger, interval: interval, counts: map[refusalLogKey]int{}, faults: newRefusalFaults(time.Now)}
}

// refusalClass is a 503 whose fault line is spaced: a load-shedding or
// readiness refusal the node answers without a shared fault writer
// (logRefusal), or a lost race a shared fault writer answers
// (logServerFault, lostRaceClass). Each class has its own fault line, at
// most one a second (refusalFaults).
type refusalClass int

const (
	refusalServerBusy refusalClass = iota
	refusalNotReady
	refusalRateLimitUnavailable
	refusalIdentityUnavailable
	refusalSecurityBusy
	refusalCredentialRotationBusy
	refusalClasses
)

// refusalCodes are the classes' codes, as the client sees them.
var refusalCodes = [refusalClasses]string{"server_busy", "not_ready", "rate_limit_unavailable", "identity_unavailable", "security_busy",
	"credential_rotation_busy"}

// refusalFaultInterval is the least time between two fault lines of one
// refusal class.
const refusalFaultInterval = time.Second

// refusalFaults spaces each refusal class's fault lines at least
// refusalFaultInterval apart and counts the refusals left out between
// them. Lock-free: a refusal flood costs two atomics per request.
//
// Time is measured from start, read once at construction: with time.Now
// both readings carry the monotonic clock, so a wall-clock step (NTP, a
// manual change) neither silences a class nor lets lines through early.
type refusalFaults struct {
	now   func() time.Time
	start time.Time
	// last is the class's last line, in nanoseconds since start
	// (neverLogged before the first).
	last [refusalClasses]atomic.Int64
	// suppressed counts the class's refusals since its last line.
	suppressed [refusalClasses]atomic.Int64
}

// neverLogged is a class's last line before its first: far enough in the
// past that the first refusal always gets one.
const neverLogged = math.MinInt64 / 2

func newRefusalFaults(now func() time.Time) *refusalFaults {
	faults := &refusalFaults{now: now, start: now()}
	for class := range faults.last {
		faults.last[class].Store(neverLogged)
	}
	return faults
}

// admit reports whether a refusal of class gets a line now and, if it does,
// how many of the class's refusals went without one since its last line.
// The count is approximate at the boundary: a refusal that lands between
// the winning line's swap of last and its read of suppressed is counted in
// that line although it came after it.
func (f *refusalFaults) admit(class refusalClass) (int64, bool) {
	now := int64(f.now().Sub(f.start))
	for {
		last := f.last[class].Load()
		if now-last < int64(refusalFaultInterval) {
			f.suppressed[class].Add(1)
			return 0, false
		}
		if f.last[class].CompareAndSwap(last, now) {
			return f.suppressed[class].Swap(0), true
		}
	}
}

type refusalFaultsContextKey struct{}

func contextWithRefusalFaults(ctx context.Context, faults *refusalFaults) context.Context {
	return context.WithValue(ctx, refusalFaultsContextKey{}, faults)
}

// logRefusal leaves the fault line of a refusal of class at WARN, at most
// one a second per class: the route, the status, the refusal's code, the
// refusals of the class that went without a line since the last one, and
// the cause when there is one (causeAttrs). Outside the middleware (no
// logger or spacing on the request) it logs nothing.
func logRefusal(r *http.Request, class refusalClass, status int, cause error) {
	logger := requestLogger(r, nil)
	faults, _ := r.Context().Value(refusalFaultsContextKey{}).(*refusalFaults)
	if logger == nil || faults == nil {
		return
	}
	suppressed, ok := faults.admit(class)
	if !ok {
		return
	}
	attrs := []slog.Attr{
		slog.String("request_id", requestIDFromContext(r.Context())),
		slog.String("route", normalizedRoute(r.Pattern)),
		slog.Int("status", status),
		slog.String("refusal", refusalCodes[class]),
		slog.Int64("suppressed", suppressed),
	}
	attrs = append(attrs, causeAttrs(cause)...)
	logger.LogAttrs(context.WithoutCancel(r.Context()), slog.LevelWarn, "request refused", attrs...)
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
