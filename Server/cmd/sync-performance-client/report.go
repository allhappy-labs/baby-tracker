package main

import (
	"math"
	"sort"
)

// MatrixCell contains counts only. No server identifiers belong in artifacts.
type MatrixCell struct {
	Families        int    `json:"families"`
	Concurrency     int    `json:"concurrency"`
	RequestedEvents int    `json:"requested_events"`
	ActualEvents    int    `json:"actual_events"`
	Contention      string `json:"contention"`
}

func matrixCells() []MatrixCell {
	var cells []MatrixCell
	for _, events := range []int{248, 512, 2048} {
		for _, families := range []int{1, 32, 128} {
			for _, concurrency := range []int{1, 16, 64} {
				contention := "isolated_families"
				if concurrency > families {
					contention = "same_family_contention"
				}
				cells = append(cells, MatrixCell{Families: families, Concurrency: concurrency, RequestedEvents: events, Contention: contention})
			}
		}
	}
	return cells
}

type Distribution struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50_seconds"`
	P95   float64 `json:"p95_seconds"`
	P99   float64 `json:"p99_seconds"`
}

func distribution(values []float64) *Distribution {
	if len(values) == 0 {
		return nil
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	p := func(f float64) float64 { return sorted[int(math.Ceil(float64(len(sorted))*f))-1] }
	return &Distribution{len(sorted), p(.5), p(.95), p(.99)}
}

type FailureCounts struct {
	Errors     int `json:"errors"`
	Retries    int `json:"retries"`
	RateLimits int `json:"rate_limits"`
	Timeouts   int `json:"timeouts"`
}
type PeerPair struct {
	Operation         int     `json:"operation"`
	DurableSeconds    float64 `json:"durable_seconds"`
	ObservableSeconds float64 `json:"observable_seconds"`
	Complete          bool    `json:"complete"`
}
type CellReport struct {
	Cell           MatrixCell    `json:"cell"`
	Request        *Distribution `json:"request"`
	DurablePeer    *Distribution `json:"durable_peer"`
	ObservablePeer *Distribution `json:"observable_peer"`
	SuccessfulQPS  float64       `json:"successful_qps"`
	Failures       FailureCounts `json:"failures"`
	Completion     string        `json:"completion"`
	Source         string        `json:"source"`
	Image          string        `json:"image"`
	Build          string        `json:"build"`
	Environment    string        `json:"environment"`
	// Missing metrics remain null; a missing observer never implies zero cost.
	Resources      map[string]*float64 `json:"resources"`
	Pairs          []PeerPair          `json:"pairs"`
	requestSamples []float64
}

func newCellReport(cell MatrixCell) CellReport {
	metrics := map[string]*float64{}
	for _, name := range []string{"pool_acquire_wait_seconds", "transaction_fence_wait_seconds", "family_fence_wait_seconds", "security_fence_wait_seconds", "db_connections", "cpu_seconds", "allocations_bytes", "rss_bytes", "network_bytes", "filesystem_bytes", "integrity_pass_seconds"} {
		metrics[name] = nil
	}
	return CellReport{Cell: cell, Completion: "not_started", Build: "identitytest-no-race", Resources: metrics, Pairs: []PeerPair{}}
}
func (r *CellReport) addPair(pair PeerPair) {
	if pair.Complete && pair.Operation > 0 && pair.DurableSeconds >= 0 && pair.ObservableSeconds >= pair.DurableSeconds && !math.IsNaN(pair.ObservableSeconds) && !math.IsInf(pair.ObservableSeconds, 0) {
		r.Pairs = append(r.Pairs, pair)
	}
}
func (r *CellReport) summarize(elapsed float64) {
	var durable, observable []float64
	for _, pair := range r.Pairs {
		durable = append(durable, pair.DurableSeconds)
		observable = append(observable, pair.ObservableSeconds)
	}
	r.DurablePeer = distribution(durable)
	r.ObservablePeer = distribution(observable)
	r.Request = distribution(r.requestSamples)
	if elapsed > 0 {
		r.SuccessfulQPS = float64(len(r.Pairs)) / elapsed
	}
}
