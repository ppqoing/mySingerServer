package stats

import (
	"context"
	"testing"
	"time"

	"dedup/internal/worker"
)

type fakeWorkerMetrics struct {
	value worker.MetricsSnapshot
}

func (f *fakeWorkerMetrics) Metrics() worker.MetricsSnapshot {
	return f.value
}

type fakeProcessSampler struct {
	values []processSample
	index  int
}

func (f *fakeProcessSampler) sample(time.Time) processSample {
	value := f.values[f.index]
	if f.index < len(f.values)-1 {
		f.index++
	}
	return value
}

func TestCollectorReportsWindowDeltasAndBalancedDiskWork(t *testing.T) {
	workers := &fakeWorkerMetrics{}
	process := &fakeProcessSampler{values: []processSample{
		{CPU: 10, RSSBytes: 100, Handles: 5},
		{CPU: 20, RSSBytes: 200, Handles: 6},
	}}
	collector := newCollector(3, workers, process.sample)
	start := time.Unix(100, 0)

	collector.Begin(2, 4096)
	workers.value = worker.MetricsSnapshot{
		FilesDone: 2, FilesFailed: 1, Crashes: 1, ReadyWorkers: 4,
	}
	collector.Sample(start)
	if got := collector.Report(1); got.PendingBytes != 4096 {
		t.Fatalf("pending bytes = %d, want 4096", got.PendingBytes)
	}

	collector.End(2, 4096, 20*time.Millisecond, 5*time.Millisecond, 9*time.Millisecond)
	workers.value.FilesDone = 5
	collector.Sample(start.Add(time.Second))
	got := collector.Report(2)
	if got.FilesDone != 5 || got.FilesFailed != 1 || got.Crashes != 1 {
		t.Fatalf("worker totals = done:%d failed:%d crashes:%d", got.FilesDone, got.FilesFailed, got.Crashes)
	}
	if got.PendingBytes != 0 || len(got.Disks) != 1 ||
		got.Disks[0].DiskNo != 2 || got.Disks[0].FilesDone != 1 {
		t.Fatalf("disk report = %#v, pending=%d", got.Disks, got.PendingBytes)
	}
	if got.ReadP95MS < 5 || got.DecodeP95MS < 9 {
		t.Fatalf("latencies = read %.3f decode %.3f", got.ReadP95MS, got.DecodeP95MS)
	}
	if got.CPU != 20 || got.RSSBytes != 200 || got.Handles != 6 || got.Workers != 4 {
		t.Fatalf("process/worker gauges = %#v", got)
	}
}

func TestCollectorRingKeepsOnlyConfiguredHistory(t *testing.T) {
	collector := newCollector(3, nil, func(time.Time) processSample { return processSample{} })
	start := time.Unix(1000, 0)
	for index := 0; index < 5; index++ {
		collector.Begin(1, 10)
		collector.End(1, 10, time.Millisecond, time.Millisecond, 0)
		collector.Sample(start.Add(time.Duration(index) * time.Second))
	}
	got := collector.Report(300)
	if got.WindowS != 3 || len(collector.history) != 3 {
		t.Fatalf("window/history = %d/%d, want 3/3", got.WindowS, len(collector.history))
	}
	if got.Disks[0].FilesDone != 3 {
		t.Fatalf("window files = %d, want 3", got.Disks[0].FilesDone)
	}
}

type memorySink struct {
	snapshots []Snapshot
}

func (s *memorySink) Write(snapshot Snapshot) error {
	s.snapshots = append(s.snapshots, snapshot)
	return nil
}

func TestCollectorRunSamplesUntilCancellation(t *testing.T) {
	collector := newCollector(2, nil, func(time.Time) processSample { return processSample{} })
	sink := &memorySink{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		collector.Run(ctx, 5*time.Millisecond, sink, nil)
		close(done)
	}()
	deadline := time.After(time.Second)
	for len(sink.snapshots) < 2 {
		select {
		case <-deadline:
			t.Fatal("collector did not sample twice")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done
}

func TestCollectorStatsMapsSnapshotToProtocol(t *testing.T) {
	collector := newCollector(2, nil, func(time.Time) processSample {
		return processSample{CPU: 25, RSSBytes: 512, Handles: 7}
	})
	collector.Begin(9, 2048)
	collector.End(9, 2048, 4*time.Millisecond, 2*time.Millisecond, 3*time.Millisecond)
	collector.Sample(time.Unix(2000, 0))
	report := collector.Stats(1)
	if report.WindowS != 1 || report.CPU != 25 || report.RSSBytes != 512 ||
		report.Handles != 7 || len(report.Disks) != 1 ||
		report.Disks[0].DiskNo != 9 || report.Disks[0].FilesDone != 1 {
		t.Fatalf("protocol report = %#v", report)
	}
}
