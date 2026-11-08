package loadtest

import (
	"testing"
	"time"
)

func TestHistogramQuantilesWithinThreePercent(t *testing.T) {
	var h Histogram
	for ms := 1; ms <= 1000; ms++ {
		h.Record(time.Duration(ms) * time.Millisecond)
	}
	for _, c := range []struct {
		q    float64
		want time.Duration
	}{{0.5, 500 * time.Millisecond}, {0.95, 950 * time.Millisecond}, {0.99, 990 * time.Millisecond}, {1, time.Second}} {
		got := h.Quantile(c.q)
		if got < c.want || float64(got) > float64(c.want)*1.032 {
			t.Errorf("q%.2f = %v, want %v..+3.2%%", c.q, got, c.want)
		}
	}
	var small Histogram
	small.Record(10 * time.Microsecond)
	if small.Quantile(1) != 10*time.Microsecond {
		t.Errorf("exact range: %v", small.Quantile(1))
	}
}

func TestHistogramMergeAndOverflow(t *testing.T) {
	var a, b Histogram
	a.Record(time.Millisecond)
	b.Record(200 * time.Second)
	a.Merge(&b)
	if a.Total != 2 || a.Overflow != 1 || a.Quantile(1) != time.Duration(histMaxMicros)*time.Microsecond {
		t.Fatalf("merge: total %d overflow %d q1 %v", a.Total, a.Overflow, a.Quantile(1))
	}
}

// Every bucket's upper bound is the largest value that maps to it, and the
// next value starts the next bucket: the buckets tile 0..histMaxMicros with
// no gap and no overlap, and each is at most 1/32 of its lower bound wide.
func TestHistogramBucketsTileTheRange(t *testing.T) {
	if got := bucketOf(histMaxMicros); got != histBuckets-1 {
		t.Fatalf("the largest value lands in bucket %d, want %d", got, histBuckets-1)
	}
	lower := uint64(0)
	for i := range histBuckets {
		upper := bucketUpper(i)
		if bucketOf(lower) != i || bucketOf(upper) != i {
			t.Fatalf("bucket %d [%d, %d]: bucketOf(lower) %d, bucketOf(upper) %d", i, lower, upper, bucketOf(lower), bucketOf(upper))
		}
		if lower >= histExact && float64(upper-lower+1) > float64(lower)/histPerOctave {
			t.Fatalf("bucket %d [%d, %d] is wider than 1/%d", i, lower, upper, histPerOctave)
		}
		lower = upper + 1
	}
	if lower != histMaxMicros+1 {
		t.Fatalf("buckets end at %d, want %d", lower-1, histMaxMicros)
	}
}

func TestHistogramQuantileNeverUnderReports(t *testing.T) {
	for _, us := range []uint64{0, 1, 63, 64, 65, 66, 127, 128, 1000, 299_999, 300_000, 300_001, 9_999_999, histMaxMicros} {
		var h Histogram
		h.Record(time.Duration(us) * time.Microsecond)
		h.Record(time.Duration(us) * time.Microsecond)
		got := h.Quantile(0.5)
		want := time.Duration(us) * time.Microsecond
		if got < want || float64(got) > float64(want)*(1+1.0/histPerOctave)+float64(time.Microsecond) {
			t.Errorf("%d µs reported as %v", us, got)
		}
	}
	var empty Histogram
	if empty.Quantile(0.95) != 0 {
		t.Fatal("an empty histogram reported a latency")
	}
	var neg Histogram
	neg.Record(-time.Second)
	if neg.Total != 1 || neg.Quantile(1) != 0 {
		t.Fatalf("a negative duration: total %d q1 %v", neg.Total, neg.Quantile(1))
	}
}
