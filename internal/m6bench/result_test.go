package m6bench

import (
	"testing"
	"time"
)

func TestDurationPercentilesUseSortedLiteralSamples(t *testing.T) {
	got := DurationPercentiles([]time.Duration{
		5 * time.Millisecond,
		time.Millisecond,
		3 * time.Millisecond,
		2 * time.Millisecond,
		4 * time.Millisecond,
	})
	if got.P50 != 3 || got.P95 != 5 || got.P99 != 5 {
		t.Fatalf("percentiles = %#v, want p50=3 p95=5 p99=5", got)
	}
}
