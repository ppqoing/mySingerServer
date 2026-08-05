package firstscreen

import (
	"context"
	"reflect"
	"testing"
)

func TestRunBenchmarkUsesProductionIndexDeterministically(t *testing.T) {
	cfg := BenchmarkConfig{Rows: 10_000, ClusterSize: 4, Seed: 20260729}
	first, err := RunBenchmark(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RunBenchmark(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	first.BuildMS, first.QueryMS, first.TotalMS, first.PeakHeapBytes = 0, 0, 0, 0
	second.BuildMS, second.QueryMS, second.TotalMS, second.PeakHeapBytes = 0, 0, 0, 0
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("nondeterministic results:\nfirst=%#v\nsecond=%#v", first, second)
	}
	if first.ExpectedGroups == 0 || first.ActualGroups != first.ExpectedGroups ||
		first.Candidates == 0 {
		t.Fatalf("incorrect grouping = %#v", first)
	}
}

func TestRunBenchmarkRejectsBoundsAndCancellation(t *testing.T) {
	for _, cfg := range []BenchmarkConfig{
		{},
		{Rows: 100, ClusterSize: 1, Seed: 1},
		{Rows: 2_000_001, ClusterSize: 4, Seed: 1},
	} {
		if _, err := RunBenchmark(context.Background(), cfg); err == nil {
			t.Fatalf("accepted invalid config %#v", cfg)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RunBenchmark(ctx, BenchmarkConfig{
		Rows: 1_000_000, ClusterSize: 4, Seed: 1,
	}); err == nil {
		t.Fatal("cancelled benchmark returned nil error")
	}
}
