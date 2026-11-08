package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/testkit"
)

type liveRefreshFunc func(context.Context, int) ([]authority.KeyedDigest, error)

func (f liveRefreshFunc) LiveRefreshDigests(ctx context.Context, limit int) ([]authority.KeyedDigest, error) {
	return f(ctx, limit)
}

// syncBuffer is a log sink the warm goroutine may write while the test
// reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// lines is the JSON log lines written so far.
func (b *syncBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		out = append(out, entry)
	}
	return out
}

func digestOf(fill byte) authority.KeyedDigest {
	digest := authority.KeyedDigest{KeyID: "session.v1"}
	for i := range digest.Sum {
		digest.Sum[i] = fill
	}
	return digest
}

// The warm loads up to one generation of digests and logs one line with
// how many it holds and how long it took; a failed or stalled load leaves
// the set empty, logs why, and never holds startup past its budget.
func TestStartKnownRefreshWarmLoadsOrDegrades(t *testing.T) {
	t.Run("loaded", func(t *testing.T) {
		logs := &syncBuffer{}
		var asked int
		source := liveRefreshFunc(func(_ context.Context, limit int) ([]authority.KeyedDigest, error) {
			asked = limit
			return []authority.KeyedDigest{digestOf(1), digestOf(2)}, nil
		})
		known := httpapi.NewKnownRefreshes(nil)
		startKnownRefreshWarm(t.Context(), source, known, time.Second, slog.New(slog.NewJSONHandler(logs, nil)))()
		if asked != httpapi.KnownRefreshWarmLimit {
			t.Fatalf("asked for %d digests, want %d", asked, httpapi.KnownRefreshWarmLimit)
		}
		lines := logs.lines(t)
		if len(lines) != 1 || lines[0]["msg"] != "known refresh tokens loaded" || lines[0]["level"] != "INFO" ||
			lines[0]["digests"] != float64(2) || lines[0]["limit"] != float64(httpapi.KnownRefreshWarmLimit) || lines[0]["duration_ms"] == nil {
			t.Fatalf("log %v", lines)
		}
		if held := known.Warm(nil); held != 2 {
			t.Fatalf("set holds %d", held)
		}
	})
	t.Run("failed", func(t *testing.T) {
		logs := &syncBuffer{}
		source := liveRefreshFunc(func(context.Context, int) ([]authority.KeyedDigest, error) {
			return nil, errors.New("authority: live refresh digests: relation missing")
		})
		known := httpapi.NewKnownRefreshes(nil)
		startKnownRefreshWarm(t.Context(), source, known, time.Second, slog.New(slog.NewJSONHandler(logs, nil)))()
		lines := logs.lines(t)
		if len(lines) != 1 || lines[0]["msg"] != "known refresh tokens not loaded" || lines[0]["level"] != "WARN" ||
			!strings.Contains(lines[0]["error"].(string), "relation missing") || lines[0]["duration_ms"] == nil {
			t.Fatalf("log %v", lines)
		}
		if held := known.Warm(nil); held != 0 {
			t.Fatalf("a failed load left %d digests", held)
		}
	})
	t.Run("stalled", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		sawDeadline := make(chan bool, 1)
		source := liveRefreshFunc(func(ctx context.Context, _ int) ([]authority.KeyedDigest, error) {
			_, ok := ctx.Deadline()
			sawDeadline <- ok
			<-release // ignores its context
			return nil, context.Canceled
		})
		started := time.Now()
		startKnownRefreshWarm(t.Context(), source, httpapi.NewKnownRefreshes(nil), 50*time.Millisecond, slog.New(slog.DiscardHandler))()
		if took := time.Since(started); took > 50*time.Millisecond+knownRefreshWarmGrace+time.Second {
			t.Fatalf("a stalled load held startup %v", took)
		}
		if !<-sawDeadline {
			t.Fatal("the load ran without a deadline")
		}
	})
}

// Over the database, the warm loads the live sessions' digests through the
// request authority (runtime pool): the harness's three signed-in subjects,
// not a consumed parent.
func TestStartKnownRefreshWarmOverTheDatabase(t *testing.T) {
	h := testkit.New(t)
	consumed := h.RefreshToken("owner")
	h.Refresh("owner")
	logs := &syncBuffer{}
	known := httpapi.NewKnownRefreshes(nil)
	startKnownRefreshWarm(t.Context(), h.Authority, known, knownRefreshWarmBudget, slog.New(slog.NewJSONHandler(logs, nil)))()
	lines := logs.lines(t)
	if len(lines) != 1 || lines[0]["msg"] != "known refresh tokens loaded" || lines[0]["digests"] != float64(3) {
		t.Fatalf("log %v", lines)
	}
	parent, err := h.Auth.RefreshDigests(consumed)
	if err != nil {
		t.Fatal(err)
	}
	if held := known.Warm(parent[:0]); held != 3 {
		t.Fatalf("set holds %d", held)
	}
}
