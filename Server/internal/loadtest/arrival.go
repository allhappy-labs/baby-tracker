package loadtest

import (
	"math"
	"math/rand/v2"
	"time"
)

// Arrivals is an open-loop Poisson process: each intended start is fixed in
// advance from the rate and the seed alone and never waits for an earlier
// operation. It is not safe for concurrent use; each Dispatcher.Run owns one.
type Arrivals struct {
	rate float64
	rng  *rand.Rand
	at   time.Duration
}

// NewArrivals returns the arrival schedule for ratePerSecond. A rate that is
// not a positive finite number yields no arrivals.
func NewArrivals(ratePerSecond float64, seed uint64) *Arrivals {
	return &Arrivals{rate: ratePerSecond, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
}

// Next returns the next intended start as an offset from the run's start.
func (a *Arrivals) Next() (time.Duration, bool) {
	if !(a.rate > 0) || math.IsInf(a.rate, 1) {
		return 0, false
	}
	a.at += time.Duration(a.rng.ExpFloat64() / a.rate * float64(time.Second))
	return a.at, true
}
