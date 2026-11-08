package loadtest

import (
	"math"
	"testing"
	"time"
)

func TestArrivalsArePoissonAtTheRate(t *testing.T) {
	a := NewArrivals(1000, 7)
	n := 0
	for {
		at, ok := a.Next()
		if !ok || at >= time.Minute {
			break
		}
		n++
	}
	if n < 58200 || n > 61800 {
		t.Fatalf("%d arrivals in 60 s at 1000/s", n)
	}
	if _, ok := NewArrivals(0, 1).Next(); ok {
		t.Fatal("a zero rate arrived")
	}
	for _, rate := range []float64{-1, math.NaN(), math.Inf(1)} {
		if _, ok := NewArrivals(rate, 1).Next(); ok {
			t.Fatalf("rate %v arrived", rate)
		}
	}
}

// The schedule is a function of the rate and the seed alone: nothing about
// how long an operation takes can move a later arrival.
func TestArrivalsAreDeterministicPerSeed(t *testing.T) {
	a, b, c := NewArrivals(50, 9), NewArrivals(50, 9), NewArrivals(50, 10)
	same, differs := true, false
	prev := time.Duration(-1)
	for range 1000 {
		x, _ := a.Next()
		y, _ := b.Next()
		z, _ := c.Next()
		same = same && x == y
		differs = differs || x != z
		if x < prev {
			t.Fatalf("arrivals went backwards: %v after %v", x, prev)
		}
		prev = x
	}
	if !same || !differs {
		t.Fatalf("same seed equal %v, different seed differs %v", same, differs)
	}
}

// Poisson gaps are exponential: the coefficient of variation is 1. A fixed
// tick would give 0 and hide the bursts a real fleet produces.
func TestArrivalGapsAreExponential(t *testing.T) {
	a := NewArrivals(100, 21)
	var prev time.Duration
	var sum, sumSq float64
	const n = 100_000
	for range n {
		at, _ := a.Next()
		gap := (at - prev).Seconds()
		prev = at
		sum += gap
		sumSq += gap * gap
	}
	mean := sum / n
	cv := math.Sqrt(sumSq/n-mean*mean) / mean
	if math.Abs(mean-0.01) > 0.0003 || math.Abs(cv-1) > 0.03 {
		t.Fatalf("gap mean %.5f s (want 0.01), cv %.3f (want 1)", mean, cv)
	}
}
