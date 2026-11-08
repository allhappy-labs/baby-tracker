// Package loadtest is the capacity load generator's core: open-loop arrivals,
// latency recorded against each request's intended start, the load model, the
// SLO verdict and the ledger of acknowledged writes. It is pure Go and needs
// no database.
package loadtest

import (
	"math"
	"math/bits"
	"time"
)

// Latencies are recorded in microseconds from 0 to 2^27-1 (about 134 s):
// exact below 64 µs, then 32 buckets per power of two, so a reported
// quantile is at most 1/32 (about 3%) above the true value. Quantiles report
// a bucket's upper bound, never less than the true value (conservative).
const (
	histExact     = 64
	histPerOctave = 32
	histMaxMicros = 1<<27 - 1
	histBuckets   = histExact + 21*histPerOctave
)

// Histogram is a fixed-size log-linear latency histogram. The zero value is
// empty and ready; histograms merge by addition and copy by value.
type Histogram struct {
	Counts    [histBuckets]uint64 `json:"counts"`
	Total     uint64              `json:"total"`
	MaxMicros uint64              `json:"max_us"`
	Overflow  uint64              `json:"overflow"`
}

func bucketOf(us uint64) int {
	if us < histExact {
		return int(us)
	}
	if us > histMaxMicros {
		us = histMaxMicros
	}
	m := bits.Len64(us) - 6
	sub := us >> uint(m)
	return histExact + (m-1)*histPerOctave + int(sub-32)
}

func bucketUpper(i int) uint64 {
	if i < histExact {
		return uint64(i)
	}
	m := (i-histExact)/histPerOctave + 1
	sub := uint64((i-histExact)%histPerOctave + 32)
	return (sub+1)<<uint(m) - 1
}

// Record adds one latency. Negative durations count as zero; durations above
// the range count in the last bucket and in Overflow.
func (h *Histogram) Record(d time.Duration) {
	us := uint64(max(d, 0) / time.Microsecond)
	if us > histMaxMicros {
		h.Overflow++
	}
	h.Counts[bucketOf(us)]++
	h.Total++
	h.MaxMicros = max(h.MaxMicros, min(us, histMaxMicros))
}

// Merge adds every sample of o into h.
func (h *Histogram) Merge(o *Histogram) {
	for i, c := range o.Counts {
		h.Counts[i] += c
	}
	h.Total += o.Total
	h.Overflow += o.Overflow
	h.MaxMicros = max(h.MaxMicros, o.MaxMicros)
}

// Quantile returns the q-quantile (q in [0, 1]) as the upper bound of the
// bucket holding it, capped at the largest recorded value: never below the
// true value. An empty histogram reports 0.
func (h *Histogram) Quantile(q float64) time.Duration {
	if h.Total == 0 {
		return 0
	}
	if math.IsNaN(q) || q > 1 {
		q = 1
	}
	q = max(q, 0)
	rank := max(uint64(math.Ceil(q*float64(h.Total))), 1)
	var seen uint64
	for i, c := range h.Counts {
		seen += c
		if seen >= rank {
			return time.Duration(min(bucketUpper(i), h.MaxMicros)) * time.Microsecond
		}
	}
	return time.Duration(h.MaxMicros) * time.Microsecond
}
