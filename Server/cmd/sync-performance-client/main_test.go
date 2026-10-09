//go:build identitytest

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMeasurementFailureCounters(t *testing.T) {
	tests := []struct {
		name                        string
		failure                     error
		windowError                 error
		refusals, errors, deadlines int
		stop                        bool
	}{
		{name: "quota", failure: errors.New("/v1/sync http_429"), refusals: 1},
		{name: "transport", failure: errors.New("transport_failed"), errors: 1},
		{name: "non_http_error_suffix", failure: errors.New("transport_failed http_429"), errors: 1},
		{name: "server_failure", failure: errors.New("/v1/sync http_503"), errors: 1},
		{name: "mutation_not_applied", failure: errors.New("mutation_refused"), errors: 1},
		{name: "deadline", failure: errors.New("transport_failed"), windowError: context.DeadlineExceeded, deadlines: 1, stop: true},
		{name: "cancelled_window", failure: errors.New("transport_failed"), windowError: context.Canceled, errors: 1, stop: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got result
			stop := got.recordFailure(test.failure, test.windowError)
			if got.Refusals != test.refusals || got.RequestErrors != test.errors || got.DeadlineExpirations != test.deadlines || stop != test.stop {
				t.Fatalf("counts refusals=%d errors=%d deadlines=%d stop=%t", got.Refusals, got.RequestErrors, got.DeadlineExpirations, stop)
			}
		})
	}
}

func TestCellWorkersBoundConcurrencyAndRefusals(t *testing.T) {
	var active, peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(429)
	}))
	defer server.Close()
	path := t.TempDir() + "/manifest.json"
	if err := os.WriteFile(path, []byte(`[{"token":"synthetic"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	var commands strings.Builder
	for i := 1; i <= 6; i++ {
		fmt.Fprintf(&commands, "{\"operation\":%d,\"family\":0,\"kind\":\"empty_pull\"}\n", i)
	}
	var out bytes.Buffer
	c := client{base: server.URL, http: server.Client()}
	if err := c.serveCellIO(t.Context(), path, 2, strings.NewReader(commands.String()), &out); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&out)
	completions := 0
	for decoder.More() {
		var row map[string]any
		if err := decoder.Decode(&row); err != nil {
			t.Fatal(err)
		}
		if row["kind"] == "operation_complete" {
			completions++
			if row["status"] != "rate_limit" {
				t.Fatal("refusal treated as success", row)
			}
		}
	}
	if completions != 6 || peak.Load() != 2 || active.Load() != 0 {
		t.Fatalf("completions=%d peak=%d active=%d", completions, peak.Load(), active.Load())
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.serveCellIO(ctx, path, 2, strings.NewReader(commands.String()), &bytes.Buffer{}); err == nil {
		t.Fatal("cancelled commands accepted")
	}
	if err := c.serveCellIO(t.Context(), path, 2, strings.NewReader("{\"operation\":1,\"family\":999,\"kind\":\"empty_pull\"}\n"), &bytes.Buffer{}); err == nil {
		t.Fatal("unowned family accepted")
	}
}
