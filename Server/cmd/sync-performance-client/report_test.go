package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestMatrixBoundsAndHonestFailures(t *testing.T) {
	cells := matrixCells()
	if len(cells) != 27 {
		t.Fatalf("cells=%d", len(cells))
	}
	seen := map[MatrixCell]bool{}
	for _, cell := range cells {
		if seen[cell] {
			t.Fatal("duplicate cell")
		}
		seen[cell] = true
		if (cell.Concurrency > cell.Families) != (cell.Contention == "same_family_contention") {
			t.Fatal("dishonest contention label")
		}
	}
	start := time.Unix(100, 0)
	budget := MatrixBudget{Started: start}
	if !budget.CellDeadline(start.Add(1190 * time.Second)).Equal(start.Add(1200 * time.Second)) {
		t.Fatal("global bound not respected")
	}
	if !budget.CellDeadline(start.Add(10 * time.Second)).Equal(start.Add(130 * time.Second)) {
		t.Fatal("cell bound not respected")
	}
	report := newCellReport(cells[0])
	report.Cell.ActualEvents = 47
	report.Failures.RateLimits = 1
	report.Completion = "setup_refused"
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if report.SuccessfulQPS != 0 || report.DurablePeer != nil || report.Cell.ActualEvents != 47 {
		t.Fatal("incomplete cell claimed success")
	}
	for _, key := range []string{"source", "image", "build", "environment", "request", "durable_peer", "observable_peer", "failures", "completion"} {
		if !strings.Contains(string(raw), `"`+key+`"`) {
			t.Fatal("missing field", key)
		}
	}
	for _, sensitive := range []string{"access_token", "refresh_token", "family_id", "account_id", "subject", "payload"} {
		if strings.Contains(string(raw), sensitive) {
			t.Fatal("sensitive field", sensitive)
		}
	}
}

func TestPercentilesUseOnlyCompletedPairsWithFailuresReported(t *testing.T) {
	report := newCellReport(matrixCells()[0])
	report.Failures.Errors = 2
	report.Failures.Timeouts = 1
	report.addPair(PeerPair{Operation: 1, DurableSeconds: 0.1, ObservableSeconds: 0.2, Complete: true})
	report.addPair(PeerPair{Operation: 2, DurableSeconds: 90, ObservableSeconds: 91, Complete: false})
	report.addPair(PeerPair{Operation: 3, DurableSeconds: 0.3, ObservableSeconds: 0.4, Complete: true})
	report.summarize(30)
	if report.DurablePeer.Count != 2 || report.DurablePeer.P95 != 0.3 || report.ObservablePeer.P95 != 0.4 {
		t.Fatal("incomplete pair included")
	}
	if report.Failures.Errors != 2 || report.Failures.Timeouts != 1 || report.SuccessfulQPS != 2.0/30.0 {
		t.Fatal("failure erased/count as success")
	}
	empty := newCellReport(matrixCells()[0])
	empty.summarize(30)
	if empty.DurablePeer != nil || empty.ObservablePeer != nil || empty.Request != nil {
		t.Fatal("empty percentile success")
	}
}
