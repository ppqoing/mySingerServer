package stats

import (
	"testing"
	"time"
)

func TestHistogramPercentileUsesFixedBuckets(t *testing.T) {
	var h histogram
	for _, value := range []time.Duration{
		100 * time.Microsecond,
		time.Millisecond,
		10 * time.Millisecond,
		100 * time.Millisecond,
		time.Second,
	} {
		h.observe(value)
	}
	if got := h.percentile(0.50); got < time.Millisecond || got > 10*time.Millisecond {
		t.Fatalf("p50 = %v, want bucket in [1ms,10ms]", got)
	}
	if got := h.percentile(0.95); got < time.Second {
		t.Fatalf("p95 = %v, want final 1s sample bucket", got)
	}
}

func TestHistogramRemainsBoundedAfterMillionObservations(t *testing.T) {
	var h histogram
	for index := 0; index < 1_000_000; index++ {
		h.observe(time.Duration(index%10_000) * time.Microsecond)
	}
	if len(h.buckets) != latencyBucketCount {
		t.Fatalf("bucket count = %d, want %d", len(h.buckets), latencyBucketCount)
	}
	if h.count != 1_000_000 {
		t.Fatalf("count = %d, want 1000000", h.count)
	}
	h.reset()
	if h.count != 0 {
		t.Fatalf("count after reset = %d, want 0", h.count)
	}
}
