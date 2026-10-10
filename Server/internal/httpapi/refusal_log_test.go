package httpapi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/requestlog"
)

type summaryLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *summaryLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *summaryLogBuffer) entries(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(b.buf.Bytes()))
	for scanner.Scan() {
		var entry map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatalf("log line %q: %v", scanner.Text(), err)
		}
		out = append(out, entry)
	}
	return out
}

// Marked over-limit refusals are counted and summarized per route and status
// at most once per interval; every other request keeps its own line with a
// request ID. Nothing is scheduled while no refusal is pending.
func TestMarkedRefusalsAreAggregatedNotLoggedPerRequest(t *testing.T) {
	logs := &summaryLogBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	summary := newRefusalLog(logger, 100*time.Millisecond)
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("outcome") {
		case "refused":
			requestlog.Aggregate(r.Context())
			w.WriteHeader(http.StatusTooManyRequests)
		case "busy":
			requestlog.Aggregate(r.Context())
			w.WriteHeader(http.StatusServiceUnavailable)
		case "panic":
			requestlog.Aggregate(r.Context())
			panic("boom")
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	handler := withRefusalLog(logger, summary, mux)
	serve := func(outcome string) {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/mcp?outcome="+outcome, nil))
	}
	for range 5 {
		serve("refused")
	}
	serve("busy")
	serve("admitted")
	serve("panic")
	// A panic is never aggregated: its own ERROR line (the recovered value by
	// type) and its 500's line.
	entries := logs.entries(t)
	if len(entries) != 3 || entries[0]["msg"] != "request completed" || entries[0]["status"] != float64(200) || entries[0]["request_id"] == "" ||
		entries[1]["msg"] != "request panicked" || entries[1]["level"] != "ERROR" || entries[2]["status"] != float64(500) {
		t.Fatalf("per-request lines: %v", entries)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(logs.entries(t)) < 5 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	counts := map[float64]float64{}
	for _, entry := range logs.entries(t)[3:] {
		if entry["msg"] != "requests refused summary" || entry["route"] != "/mcp" || entry["window_seconds"] == nil {
			t.Fatalf("summary line: %v", entry)
		}
		counts[entry["status"].(float64)] += entry["count"].(float64)
	}
	if counts[429] != 5 || counts[503] != 1 || len(counts) != 2 {
		t.Fatalf("summary counts: %v", counts)
	}
	time.Sleep(250 * time.Millisecond)
	if n := len(logs.entries(t)); n != 5 {
		t.Fatalf("idle summary emitted extra lines: %d", n)
	}
	summary.mu.Lock()
	pending := summary.timer != nil
	summary.mu.Unlock()
	if pending {
		t.Fatal("summary timer still pending while idle")
	}
	// A new refusal after an idle period starts a new summary window.
	serve("refused")
	deadline = time.Now().Add(2 * time.Second)
	for len(logs.entries(t)) < 6 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if last := logs.entries(t); len(last) != 6 || last[5]["count"] != float64(1) {
		t.Fatalf("second window: %v", last)
	}
}

// Without the middleware's mark, Aggregate is a no-op.
func TestAggregateWithoutAMarkIsANoOp(t *testing.T) {
	requestlog.Aggregate(httptest.NewRequest(http.MethodGet, "/", nil).Context())
}
