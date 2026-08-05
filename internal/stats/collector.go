package stats

import (
	"context"
	"runtime"
	"sort"
	"sync"
	"time"

	"dedup/internal/proto"
	"dedup/internal/worker"
)

type WorkerMetrics interface {
	Metrics() worker.MetricsSnapshot
}

type DiskSnapshot struct {
	DiskNo       int64   `json:"disk_no"`
	ReadBPS      float64 `json:"read_bps"`
	BusyFraction float64 `json:"busy_fraction"`
	FilesDone    int64   `json:"files_done"`
	BytesDone    int64   `json:"bytes_done"`
	PendingBytes int64   `json:"pending_bytes"`
}

type Snapshot struct {
	Time         time.Time      `json:"time"`
	WindowS      int            `json:"window_s"`
	CPU          float64        `json:"cpu"`
	RSSBytes     uint64         `json:"rss_bytes"`
	HeapBytes    uint64         `json:"heap_bytes"`
	Handles      uint64         `json:"handles"`
	Goroutines   int            `json:"goroutines"`
	Workers      int            `json:"workers"`
	PendingBytes int64          `json:"pending_bytes"`
	FilesDone    int64          `json:"files_done"`
	FilesFailed  int64          `json:"files_failed"`
	Crashes      int64          `json:"crashes"`
	ReadP95MS    float64        `json:"read_p95_ms"`
	DecodeP95MS  float64        `json:"decode_p95_ms"`
	Disks        []DiskSnapshot `json:"disks"`
}

type Sink interface {
	Write(Snapshot) error
}

type processSample struct {
	CPU      float64
	RSSBytes uint64
	Handles  uint64
}

type diskState struct {
	active       int64
	pendingBytes int64
	filesDone    int64
	bytesDone    int64
	busy         time.Duration
}

type sampleRecord struct {
	snapshot Snapshot
	read     histogram
	decode   histogram
}

type Collector struct {
	mu sync.Mutex

	historyLimit int
	history      []sampleRecord
	workers      WorkerMetrics
	process      func(time.Time) processSample
	disks        map[int64]*diskState
	pendingBytes int64
	read         histogram
	decode       histogram
	lastSample   time.Time
}

func New(history int, workers WorkerMetrics) *Collector {
	return newCollector(history, workers, newProcessSampler())
}

func newCollector(
	history int,
	workers WorkerMetrics,
	process func(time.Time) processSample,
) *Collector {
	if history < 1 {
		history = 1
	}
	if history > 300 {
		history = 300
	}
	if process == nil {
		process = func(time.Time) processSample { return processSample{} }
	}
	return &Collector{
		historyLimit: history,
		workers:      workers,
		process:      process,
		disks:        make(map[int64]*diskState),
	}
}

func (c *Collector) Begin(diskNo int64, bytes int64) {
	if bytes < 0 {
		bytes = 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	disk := c.disk(diskNo)
	disk.active++
	disk.pendingBytes += bytes
	c.pendingBytes += bytes
}

func (c *Collector) End(
	diskNo int64,
	bytes int64,
	elapsed time.Duration,
	read time.Duration,
	decode time.Duration,
) {
	if bytes < 0 {
		bytes = 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	disk := c.disk(diskNo)
	if disk.active > 0 {
		disk.active--
	}
	disk.pendingBytes -= bytes
	if disk.pendingBytes < 0 {
		disk.pendingBytes = 0
	}
	c.pendingBytes -= bytes
	if c.pendingBytes < 0 {
		c.pendingBytes = 0
	}
	disk.filesDone++
	disk.bytesDone += bytes
	if elapsed > 0 {
		disk.busy += elapsed
	}
	c.read.observe(read)
	c.decode.observe(decode)
}

func (c *Collector) disk(diskNo int64) *diskState {
	disk := c.disks[diskNo]
	if disk == nil {
		disk = &diskState{}
		c.disks[diskNo] = disk
	}
	return disk
}

func (c *Collector) Sample(now time.Time) Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	elapsed := now.Sub(c.lastSample)
	if c.lastSample.IsZero() || elapsed <= 0 {
		elapsed = time.Second
	}
	c.lastSample = now
	osSample := c.process(now)
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)

	result := Snapshot{
		Time:         now.UTC(),
		WindowS:      1,
		CPU:          osSample.CPU,
		RSSBytes:     osSample.RSSBytes,
		HeapBytes:    memory.HeapAlloc,
		Handles:      osSample.Handles,
		Goroutines:   runtime.NumGoroutine(),
		PendingBytes: c.pendingBytes,
		ReadP95MS:    durationMS(c.read.percentile(0.95)),
		DecodeP95MS:  durationMS(c.decode.percentile(0.95)),
	}
	if c.workers != nil {
		metrics := c.workers.Metrics()
		result.Workers = int(metrics.ReadyWorkers)
		result.FilesDone = metrics.FilesDone
		result.FilesFailed = metrics.FilesFailed
		result.Crashes = metrics.Crashes
	}

	diskNumbers := make([]int64, 0, len(c.disks))
	for diskNo := range c.disks {
		diskNumbers = append(diskNumbers, diskNo)
	}
	sort.Slice(diskNumbers, func(left, right int) bool {
		return diskNumbers[left] < diskNumbers[right]
	})
	for _, diskNo := range diskNumbers {
		disk := c.disks[diskNo]
		busy := float64(disk.busy) / float64(elapsed)
		if busy > 1 {
			busy = 1
		}
		result.Disks = append(result.Disks, DiskSnapshot{
			DiskNo:       diskNo,
			ReadBPS:      float64(disk.bytesDone) / elapsed.Seconds(),
			BusyFraction: busy,
			FilesDone:    disk.filesDone,
			BytesDone:    disk.bytesDone,
			PendingBytes: disk.pendingBytes,
		})
		disk.filesDone = 0
		disk.bytesDone = 0
		disk.busy = 0
	}

	record := sampleRecord{snapshot: result, read: c.read, decode: c.decode}
	c.read.reset()
	c.decode.reset()
	if len(c.history) == c.historyLimit {
		copy(c.history, c.history[1:])
		c.history[len(c.history)-1] = record
	} else {
		c.history = append(c.history, record)
	}
	return cloneSnapshot(result)
}

func (c *Collector) Report(windowSeconds int) Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.history) == 0 {
		return Snapshot{WindowS: 0}
	}
	if windowSeconds < 1 {
		windowSeconds = 1
	}
	if windowSeconds > len(c.history) {
		windowSeconds = len(c.history)
	}
	selected := c.history[len(c.history)-windowSeconds:]
	result := cloneSnapshot(selected[len(selected)-1].snapshot)
	result.WindowS = windowSeconds
	result.Disks = nil
	var read, decode histogram
	disks := make(map[int64]*DiskSnapshot)
	for _, record := range selected {
		read.merge(record.read)
		decode.merge(record.decode)
		for _, current := range record.snapshot.Disks {
			disk := disks[current.DiskNo]
			if disk == nil {
				copy := DiskSnapshot{DiskNo: current.DiskNo}
				disk = &copy
				disks[current.DiskNo] = disk
			}
			disk.FilesDone += current.FilesDone
			disk.BytesDone += current.BytesDone
			disk.ReadBPS += current.ReadBPS
			disk.BusyFraction += current.BusyFraction
			disk.PendingBytes = current.PendingBytes
		}
	}
	result.ReadP95MS = durationMS(read.percentile(0.95))
	result.DecodeP95MS = durationMS(decode.percentile(0.95))
	diskNumbers := make([]int64, 0, len(disks))
	for diskNo := range disks {
		diskNumbers = append(diskNumbers, diskNo)
	}
	sort.Slice(diskNumbers, func(left, right int) bool {
		return diskNumbers[left] < diskNumbers[right]
	})
	for _, diskNo := range diskNumbers {
		disk := *disks[diskNo]
		disk.ReadBPS /= float64(windowSeconds)
		disk.BusyFraction /= float64(windowSeconds)
		result.Disks = append(result.Disks, disk)
	}
	return result
}

func (c *Collector) Stats(windowSeconds int) proto.StatsReport {
	snapshot := c.Report(windowSeconds)
	report := proto.StatsReport{
		CPU:          snapshot.CPU,
		Workers:      snapshot.Workers,
		WindowS:      snapshot.WindowS,
		RSSBytes:     snapshot.RSSBytes,
		HeapBytes:    snapshot.HeapBytes,
		Handles:      snapshot.Handles,
		PendingBytes: snapshot.PendingBytes,
		FilesDone:    snapshot.FilesDone,
		FilesFailed:  snapshot.FilesFailed,
		Crashes:      snapshot.Crashes,
		ReadP95MS:    snapshot.ReadP95MS,
		DecodeP95MS:  snapshot.DecodeP95MS,
	}
	for _, disk := range snapshot.Disks {
		report.Disks = append(report.Disks, proto.DiskStats{
			DiskNo:       disk.DiskNo,
			ReadBPS:      disk.ReadBPS,
			BusyFraction: disk.BusyFraction,
			FilesDone:    disk.FilesDone,
			PendingBytes: disk.PendingBytes,
		})
	}
	return report
}

func (c *Collector) Run(
	ctx context.Context,
	interval time.Duration,
	sink Sink,
	onError func(error),
) {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			snapshot := c.Sample(now)
			if sink != nil {
				if err := sink.Write(snapshot); err != nil && onError != nil {
					onError(err)
				}
			}
		}
	}
}

func durationMS(value time.Duration) float64 {
	return float64(value) / float64(time.Millisecond)
}

func cloneSnapshot(value Snapshot) Snapshot {
	value.Disks = append([]DiskSnapshot(nil), value.Disks...)
	return value
}
