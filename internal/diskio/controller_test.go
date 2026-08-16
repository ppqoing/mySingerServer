package diskio

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"
)

const mib = int64(1024 * 1024)

type acquireResult struct {
	grant Grant
	err   error
}

func testPolicy(initial, max int) PolicyConfig {
	return PolicyConfig{
		LeaseBytes:         4 * mib,
		MinLeaseBytes:      mib,
		MaxLeaseBytes:      16 * mib,
		HDDInitial:         initial,
		SSDInitial:         initial,
		MaxPerDisk:         max,
		HDDRandomMax:       8,
		Window:             2 * time.Second,
		IncreaseThreshold:  0.05,
		DecreaseThreshold:  0.08,
		MaxQueuedPerWorker: 1,
	}
}

func newTestController(t *testing.T, clock *fakeClock, workers, initial, max int, identities ...Identity) Controller {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	byDisk := make(map[DiskKey]Identity, len(identities))
	for _, identity := range identities {
		byDisk[identity.Key] = identity
	}
	return NewController(ctx, ControllerOptions{
		Clock:        clock,
		WorkerCount:  workers,
		Policy:       testPolicy(initial, max),
		Identities:   byDisk,
		CommandQueue: 128,
	})
}

func acquireAsync(c Controller, req Request) <-chan acquireResult {
	result := make(chan acquireResult, 1)
	go func() {
		grant, err := c.Acquire(context.Background(), req)
		result <- acquireResult{grant: grant, err: err}
	}()
	return result
}

func receiveAcquire(t *testing.T, result <-chan acquireResult) acquireResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for lease result")
		return acquireResult{}
	}
}

func assertStillWaiting(t *testing.T, result <-chan acquireResult) {
	t.Helper()
	select {
	case got := <-result:
		t.Fatalf("request unexpectedly completed: grant=%+v err=%v", got.grant, got.err)
	default:
	}
}

func waitForSnapshot(t *testing.T, c Controller, taskID, instanceID string, predicate func(Snapshot) bool) Snapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		snapshot := c.Snapshot(taskID, instanceID)
		if predicate(snapshot) {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatalf("snapshot condition not reached, last snapshot: %+v", snapshot)
		}
		runtime.Gosched()
	}
}

func request(id uint64, task, instance string, worker int, disk DiskKey, class SourceClass) Request {
	return Request{
		RequestID:  id,
		TaskID:     task,
		InstanceID: instance,
		WorkerID:   worker,
		Disk:       disk,
		Class:      class,
		WantBytes:  4 * mib,
	}
}

func reportFor(req Request, grant Grant, bytes int64, read, wait time.Duration) Report {
	return Report{
		LeaseID:    grant.LeaseID,
		Generation: grant.Generation,
		TaskID:     req.TaskID,
		InstanceID: req.InstanceID,
		WorkerID:   req.WorkerID,
		Disk:       req.Disk,
		Bytes:      bytes,
		Seeks:      grant.Seeks,
		ReadTime:   read,
		WaitTime:   wait,
		Completed:  true,
	}
}

func TestControllerAIMDUsesOnlyNPlusOnePlatformProbes(t *testing.T) {
	cfg := testPolicy(4, 24)
	tests := []struct {
		name    string
		current int
		sample  WindowSample
		want    int
	}{
		{
			name:    "throughput improves by five percent",
			current: 4,
			sample: WindowSample{Duration: 2 * time.Second, Bytes: 212 * mib, PreviousBytesPerSecond: 100 * float64(mib),
				Queued: 4, BusyWorkers: 4, WorkerCount: 8},
			want: 5,
		},
		{
			name:    "throughput drops by more than eight percent",
			current: 8,
			sample: WindowSample{Duration: 2 * time.Second, Bytes: 182 * mib, PreviousBytesPerSecond: 100 * float64(mib),
				Queued: 4, BusyWorkers: 4, WorkerCount: 16},
			want: 6,
		},
		{
			name:    "p95 wait spikes",
			current: 8,
			sample: WindowSample{Duration: 2 * time.Second, Bytes: 200 * mib, PreviousBytesPerSecond: 100 * float64(mib),
				P95Wait: 40 * time.Millisecond, PreviousP95Wait: 10 * time.Millisecond, Queued: 4, BusyWorkers: 4, WorkerCount: 16},
			want: 6,
		},
		{
			name:    "seek congestion",
			current: 8,
			sample: WindowSample{Duration: 2 * time.Second, Bytes: 200 * mib, PreviousBytesPerSecond: 100 * float64(mib),
				SeekCongested: true, Queued: 4, BusyWorkers: 4, WorkerCount: 16},
			want: 6,
		},
		{
			name:    "all workers busy prevents increase",
			current: 4,
			sample: WindowSample{Duration: 2 * time.Second, Bytes: 212 * mib, PreviousBytesPerSecond: 100 * float64(mib),
				Queued: 4, BusyWorkers: 8, WorkerCount: 8},
			want: 4,
		},
		{
			name:    "sustained queue and idle workers probes one higher",
			current: 4,
			sample: WindowSample{Duration: 2 * time.Second, Bytes: 200 * mib, PreviousBytesPerSecond: 100 * float64(mib),
				Queued: 4, BusyWorkers: 3, WorkerCount: 8},
			want: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextLimit(tt.current, tt.sample, cfg); got != tt.want {
				t.Fatalf("nextLimit(%d) = %d, want %d", tt.current, got, tt.want)
			}
		})
	}
}

func TestControllerAIMDRequiresTwoSecondByteWindowAndHonorsHardCaps(t *testing.T) {
	cfg := testPolicy(4, 30)
	base := WindowSample{Duration: 2 * time.Second, Bytes: 212 * mib, PreviousBytesPerSecond: 100 * float64(mib), Queued: 8, BusyWorkers: 4, WorkerCount: 30}

	tooShort := base
	tooShort.Duration = 1999 * time.Millisecond
	if got := nextLimit(4, tooShort, cfg); got != 4 {
		t.Fatalf("short observation changed limit to %d", got)
	}
	tooSmall := base
	tooSmall.Bytes = mib - 1
	if got := nextLimit(4, tooSmall, cfg); got != 4 {
		t.Fatalf("small observation changed limit to %d", got)
	}
	if got := nextLimit(24, base, cfg); got != 24 {
		t.Fatalf("global hard cap exceeded: %d", got)
	}
	workerBound := base
	workerBound.WorkerCount = 6
	if got := nextLimit(6, workerBound, cfg); got != 6 {
		t.Fatalf("worker cap exceeded: %d", got)
	}
	hddRandom := base
	hddRandom.HDDRandom = true
	if got := nextLimit(8, hddRandom, cfg); got != 8 {
		t.Fatalf("HDD random cap exceeded: %d", got)
	}
}

func TestControllerFakeClockDrivesTwoSecondNPlusOneProbe(t *testing.T) {
	clock := newFakeClock()
	disk := DiskKey("disk")
	c := newTestController(t, clock, 4, 1, 4, Identity{Key: disk, KnownSSD: true, SSD: true})
	firstReq := request(1, "first", "1", 0, disk, SourceSequential)
	first := receiveAcquire(t, acquireAsync(c, firstReq))
	secondReq := request(2, "second", "1", 1, disk, SourceSequential)
	second := acquireAsync(c, secondReq)
	waitForSnapshot(t, c, "second", "1", func(s Snapshot) bool { return s.IOWaitWorkers == 1 })

	clock.Advance(2 * time.Second)
	c.Report(reportFor(firstReq, first.grant, 8*mib, 2*time.Second, 0))
	secondGrant := receiveAcquire(t, second)
	filler := receiveAcquire(t, acquireAsync(c, request(3, "filler", "1", 3, disk, SourceSequential)))
	if filler.err != nil {
		t.Fatal(filler.err)
	}
	third := acquireAsync(c, request(4, "third", "1", 2, disk, SourceSequential))
	waitForSnapshot(t, c, "third", "1", func(s Snapshot) bool { return s.IOWaitWorkers == 1 })

	clock.Advance(2 * time.Second)
	c.Report(reportFor(secondReq, secondGrant.grant, 9*mib, 2*time.Second, 0))
	if got := receiveAcquire(t, third); got.err != nil {
		t.Fatal(got.err)
	}
}

func TestControllerHDDRandomAndGlobalConcurrencyCaps(t *testing.T) {
	clock := newFakeClock()
	hdd := DiskKey("hdd")
	c := newTestController(t, clock, 30, 30, 30, Identity{Key: hdd, KnownSSD: true, SSD: false})

	results := make([]<-chan acquireResult, 9)
	grants := make([]acquireResult, 8)
	for i := range results {
		results[i] = acquireAsync(c, request(uint64(i+1), "task", "instance", i, hdd, SourceRandom))
		if i < 8 {
			grants[i] = receiveAcquire(t, results[i])
			if grants[i].err != nil {
				t.Fatal(grants[i].err)
			}
		}
	}
	waitForSnapshot(t, c, "task", "instance", func(s Snapshot) bool { return s.Concurrency == 8 && s.IOWaitWorkers == 1 })
	assertStillWaiting(t, results[8])
	c.Report(reportFor(request(1, "task", "instance", 0, hdd, SourceRandom), grants[0].grant, 4*mib, time.Second, 0))
	if got := receiveAcquire(t, results[8]); got.err != nil {
		t.Fatal(got.err)
	}

	ssd := DiskKey("ssd")
	c2 := newTestController(t, clock, 30, 30, 30, Identity{Key: ssd, KnownSSD: true, SSD: true})
	ssdResults := make([]<-chan acquireResult, 25)
	for i := range ssdResults {
		ssdResults[i] = acquireAsync(c2, request(uint64(i+1), "task", "instance", i, ssd, SourceSequential))
		if i < 24 {
			if got := receiveAcquire(t, ssdResults[i]); got.err != nil {
				t.Fatal(got.err)
			}
		}
	}
	waitForSnapshot(t, c2, "task", "instance", func(s Snapshot) bool { return s.Concurrency == 24 && s.IOWaitWorkers == 1 })
	assertStillWaiting(t, ssdResults[24])
}

func TestControllerRejectsWorkerIDsOutsideConfiguredSet(t *testing.T) {
	clock := newFakeClock()
	c := newTestController(t, clock, 2, 2, 2,
		Identity{Key: "negative", KnownSSD: true, SSD: true},
		Identity{Key: "upper", KnownSSD: true, SSD: true},
	)
	for _, req := range []Request{
		request(1, "negative", "1", -1, "negative", SourceSequential),
		request(2, "upper", "1", 2, "upper", SourceSequential),
	} {
		grant, err := c.Acquire(context.Background(), req)
		if !errors.Is(err, ErrInvalidWorker) {
			t.Fatalf("worker %d result grant=%+v err=%v, want ErrInvalidWorker", req.WorkerID, grant, err)
		}
	}
}

func TestControllerGlobalConcurrencyCapSpansDisks(t *testing.T) {
	clock := newFakeClock()
	c := newTestController(t, clock, 4, 4, 2,
		Identity{Key: "a", KnownSSD: true, SSD: true},
		Identity{Key: "b", KnownSSD: true, SSD: true},
		Identity{Key: "c", KnownSSD: true, SSD: true},
	)
	firstReq := request(1, "task", "1", 0, "a", SourceSequential)
	secondReq := request(2, "task", "1", 1, "b", SourceSequential)
	first := receiveAcquire(t, acquireAsync(c, firstReq))
	second := receiveAcquire(t, acquireAsync(c, secondReq))
	if first.err != nil || second.err != nil {
		t.Fatalf("initial grants failed: first=%v second=%v", first.err, second.err)
	}
	third := acquireAsync(c, request(3, "task", "1", 2, "c", SourceSequential))
	snapshot := waitForSnapshot(t, c, "task", "1", func(s Snapshot) bool {
		return s.Concurrency >= 3 || (s.Concurrency == 2 && s.IOWaitWorkers == 1)
	})
	if snapshot.Concurrency != 2 || snapshot.IOWaitWorkers != 1 {
		t.Fatalf("cross-disk global cap bypassed: %+v", snapshot)
	}
	assertStillWaiting(t, third)

	c.Report(reportFor(firstReq, first.grant, 4*mib, time.Second, 0))
	if got := receiveAcquire(t, third); got.err != nil {
		t.Fatalf("cross-disk waiter was not redispatched after global slot release: %v", got.err)
	}
}

func TestControllerHDDSequentialWindowCanProbePastRandomCap(t *testing.T) {
	clock := newFakeClock()
	disk := DiskKey("hdd")
	c := newTestController(t, clock, 12, 8, 12, Identity{Key: disk, KnownSSD: true, SSD: false})
	activeRequests := make([]Request, 8)
	activeGrants := make([]acquireResult, 8)
	for i := range activeRequests {
		activeRequests[i] = request(uint64(i+1), "active", "1", i, disk, SourceSequential)
		activeGrants[i] = receiveAcquire(t, acquireAsync(c, activeRequests[i]))
		if activeGrants[i].err != nil {
			t.Fatal(activeGrants[i].err)
		}
	}
	queuedFirst := acquireAsync(c, request(9, "queued-first", "1", 8, disk, SourceSequential))
	waitForSnapshot(t, c, "queued-first", "1", func(s Snapshot) bool { return s.IOWaitWorkers == 1 })
	queuedSecond := acquireAsync(c, request(10, "queued-second", "1", 9, disk, SourceSequential))
	waitForSnapshot(t, c, "queued-second", "1", func(s Snapshot) bool { return s.IOWaitWorkers == 1 })

	clock.Advance(2 * time.Second)
	c.Report(reportFor(activeRequests[0], activeGrants[0].grant, 8*mib, 2*time.Second, 0))
	if got := receiveAcquire(t, queuedFirst); got.err != nil {
		t.Fatal(got.err)
	}
	if got := receiveAcquire(t, queuedSecond); got.err != nil {
		t.Fatal(got.err)
	}
}

func TestControllerMixedHDDWindowCapsOnlyRandomLeases(t *testing.T) {
	clock := newFakeClock()
	disk := DiskKey("hdd")
	c := newTestController(t, clock, 12, 12, 12, Identity{Key: disk, KnownSSD: true, SSD: false})
	randomRequests := make([]Request, 8)
	randomGrants := make([]acquireResult, 8)
	for i := range randomRequests {
		randomRequests[i] = request(uint64(i+1), "random", "1", i, disk, SourceRandom)
		randomRequests[i].WantSeek = true
		randomGrants[i] = receiveAcquire(t, acquireAsync(c, randomRequests[i]))
		if randomGrants[i].err != nil {
			t.Fatal(randomGrants[i].err)
		}
	}
	sequentialRequests := make([]Request, 4)
	sequentialGrants := make([]acquireResult, 4)
	for i := range sequentialRequests {
		sequentialRequests[i] = request(uint64(9+i), "sequential", "1", 8+i, disk, SourceSequential)
		sequentialGrants[i] = receiveAcquire(t, acquireAsync(c, sequentialRequests[i]))
		if sequentialGrants[i].err != nil {
			t.Fatal(sequentialGrants[i].err)
		}
	}

	c.Report(reportFor(sequentialRequests[0], sequentialGrants[0].grant, 8*mib, time.Second, 0))
	ninthRandomReq := request(20, "random", "1", 8, disk, SourceRandom)
	ninthRandomReq.WantSeek = true
	ninthRandom := acquireAsync(c, ninthRandomReq)
	waitForSnapshot(t, c, "random", "1", func(s Snapshot) bool { return s.Concurrency == 8 && s.IOWaitWorkers == 1 })
	assertStillWaiting(t, ninthRandom)

	clock.Advance(2 * time.Second)
	c.Report(reportFor(randomRequests[0], randomGrants[0].grant, 8*mib, time.Second, 0))
	if got := receiveAcquire(t, ninthRandom); got.err != nil {
		t.Fatal(got.err)
	}
	sequentialAfterMixed := acquireAsync(c, request(21, "sequential-new", "1", 0, disk, SourceSequential))
	if got := receiveAcquire(t, sequentialAfterMixed); got.err != nil {
		t.Fatal(got.err)
	}
	if snapshot := c.Snapshot("random", "1"); snapshot.Concurrency != 8 {
		t.Fatalf("random concurrency = %d, want exactly 8", snapshot.Concurrency)
	}
}

func TestControllerFairnessGivesQueuedTaskMinimumShare(t *testing.T) {
	clock := newFakeClock()
	disk := DiskKey("disk")
	c := newTestController(t, clock, 4, 2, 2, Identity{Key: disk, KnownSSD: true, SSD: true})

	a1Req := request(1, "A", "1", 0, disk, SourceSequential)
	a2Req := request(2, "A", "1", 1, disk, SourceSequential)
	a1 := receiveAcquire(t, acquireAsync(c, a1Req))
	a2 := receiveAcquire(t, acquireAsync(c, a2Req))
	a3 := acquireAsync(c, request(3, "A", "1", 2, disk, SourceSequential))
	waitForSnapshot(t, c, "A", "1", func(s Snapshot) bool { return s.IOWaitWorkers == 1 })
	b1 := acquireAsync(c, request(4, "B", "1", 3, disk, SourceSequential))
	waitForSnapshot(t, c, "B", "1", func(s Snapshot) bool { return s.IOWaitWorkers == 1 })

	c.Report(reportFor(a1Req, a1.grant, 4*mib, time.Second, 0))
	if got := receiveAcquire(t, b1); got.err != nil {
		t.Fatal(got.err)
	}
	assertStillWaiting(t, a3)
	c.Report(reportFor(a2Req, a2.grant, 4*mib, time.Second, 0))
	if got := receiveAcquire(t, a3); got.err != nil {
		t.Fatal(got.err)
	}
}

func TestControllerRoundRobinWhenTasksOutnumberSlots(t *testing.T) {
	clock := newFakeClock()
	disk := DiskKey("disk")
	c := newTestController(t, clock, 4, 1, 1, Identity{Key: disk, KnownSSD: true, SSD: true})

	a1Req := request(1, "A", "1", 0, disk, SourceSequential)
	a1 := receiveAcquire(t, acquireAsync(c, a1Req))
	a2Req := request(2, "A", "1", 1, disk, SourceSequential)
	bReq := request(3, "B", "1", 2, disk, SourceSequential)
	cReq := request(4, "C", "1", 3, disk, SourceSequential)
	a2 := acquireAsync(c, a2Req)
	waitForSnapshot(t, c, "A", "1", func(s Snapshot) bool { return s.IOWaitWorkers == 1 })
	b := acquireAsync(c, bReq)
	waitForSnapshot(t, c, "B", "1", func(s Snapshot) bool { return s.IOWaitWorkers == 1 })
	cResult := acquireAsync(c, cReq)
	waitForSnapshot(t, c, "C", "1", func(s Snapshot) bool { return s.IOWaitWorkers == 1 })

	c.Report(reportFor(a1Req, a1.grant, 4*mib, time.Second, 0))
	bGrant := receiveAcquire(t, b)
	assertStillWaiting(t, a2)
	assertStillWaiting(t, cResult)
	c.Report(reportFor(bReq, bGrant.grant, 4*mib, time.Second, 0))
	cGrant := receiveAcquire(t, cResult)
	assertStillWaiting(t, a2)
	c.Report(reportFor(cReq, cGrant.grant, 4*mib, time.Second, 0))
	if got := receiveAcquire(t, a2); got.err != nil {
		t.Fatal(got.err)
	}
}

func TestControllerChooseTaskUsesAgeToPreventStarvation(t *testing.T) {
	now := time.Unix(1_700_000_100, 0)
	old := TaskIdentity{TaskID: "old", InstanceID: "1"}
	recent := TaskIdentity{TaskID: "recent", InstanceID: "1"}
	queues := map[TaskIdentity]*taskQueue{
		old:    {items: []*pendingRequest{{enqueued: now.Add(-10 * time.Second)}}, lastGranted: now.Add(-time.Second)},
		recent: {items: []*pendingRequest{{enqueued: now.Add(-time.Second)}}, lastGranted: time.Time{}},
	}
	if got := chooseTask(now, queues); got != old {
		t.Fatalf("chooseTask = %+v, want oldest waiting task %+v", got, old)
	}
}

func TestControllerCancelTaskAndContextCancelOnlyWaitingRequests(t *testing.T) {
	clock := newFakeClock()
	disk := DiskKey("disk")
	c := newTestController(t, clock, 3, 1, 1, Identity{Key: disk, KnownSSD: true, SSD: true})
	activeReq := request(1, "active", "1", 0, disk, SourceSequential)
	active := receiveAcquire(t, acquireAsync(c, activeReq))

	paused := acquireAsync(c, request(2, "paused", "1", 1, disk, SourceSequential))
	waitForSnapshot(t, c, "paused", "1", func(s Snapshot) bool { return s.IOWaitWorkers == 1 })
	c.CancelTask("paused", "1")
	if got := receiveAcquire(t, paused); !errors.Is(got.err, ErrTaskCancelled) {
		t.Fatalf("CancelTask error = %v, want ErrTaskCancelled", got.err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	contextResult := make(chan acquireResult, 1)
	go func() {
		grant, err := c.Acquire(ctx, request(3, "context", "1", 2, disk, SourceSequential))
		contextResult <- acquireResult{grant: grant, err: err}
	}()
	waitForSnapshot(t, c, "context", "1", func(s Snapshot) bool { return s.IOWaitWorkers == 1 })
	cancel()
	if got := receiveAcquire(t, contextResult); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("context cancellation error = %v, want context.Canceled", got.err)
	}

	c.Report(reportFor(activeReq, active.grant, 4*mib, time.Second, 0))
	waitForSnapshot(t, c, "active", "1", func(s Snapshot) bool { return s.Concurrency == 0 })
}

func TestControllerContextCancelRedispatchesNextFIFORequest(t *testing.T) {
	clock := newFakeClock()
	disk := DiskKey("disk")
	c := newTestController(t, clock, 2, 2, 2, Identity{Key: disk, KnownSSD: true, SSD: true})
	active := receiveAcquire(t, acquireAsync(c, request(1, "task", "1", 0, disk, SourceSequential)))
	if active.err != nil {
		t.Fatal(active.err)
	}

	headContext, cancelHead := context.WithCancel(context.Background())
	headResult := make(chan acquireResult, 1)
	go func() {
		grant, err := c.Acquire(headContext, request(2, "task", "1", 0, disk, SourceSequential))
		headResult <- acquireResult{grant: grant, err: err}
	}()
	waitForSnapshot(t, c, "task", "1", func(s Snapshot) bool { return s.Concurrency == 1 && s.IOWaitWorkers == 1 })
	tailResult := acquireAsync(c, request(3, "task", "1", 1, disk, SourceSequential))
	waitForSnapshot(t, c, "task", "1", func(s Snapshot) bool { return s.Concurrency == 1 && s.IOWaitWorkers == 2 })
	assertStillWaiting(t, tailResult)

	cancelHead()
	if got := receiveAcquire(t, headResult); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("cancelled FIFO head error = %v, want context.Canceled", got.err)
	}
	if got := receiveAcquire(t, tailResult); got.err != nil {
		t.Fatalf("eligible FIFO tail did not receive idle slot: %v", got.err)
	}
}

func TestControllerStaleReportsOnlyReclaimLease(t *testing.T) {
	clock := newFakeClock()
	disk := DiskKey("disk")
	c := newTestController(t, clock, 2, 1, 1, Identity{Key: disk, KnownSSD: true, SSD: true})
	firstReq := request(1, "task", "new", 0, disk, SourceSequential)
	first := receiveAcquire(t, acquireAsync(c, firstReq))
	second := acquireAsync(c, request(2, "next", "1", 1, disk, SourceSequential))
	waitForSnapshot(t, c, "next", "1", func(s Snapshot) bool { return s.IOWaitWorkers == 1 })

	stale := reportFor(firstReq, first.grant, 512*mib, 2*time.Second, time.Second)
	stale.Generation++
	c.Report(stale)
	if got := receiveAcquire(t, second); got.err != nil {
		t.Fatal(got.err)
	}
	snapshot := c.Snapshot("task", "new")
	if snapshot.Concurrency != 0 || snapshot.SequentialBytes != 0 || snapshot.SeekCount != 0 || snapshot.EffectiveBytesPerSecond != 0 {
		t.Fatalf("stale generation modified snapshot: %+v", snapshot)
	}

	thirdReq := request(3, "task", "new", 0, disk, SourceSequential)
	third := acquireAsync(c, thirdReq)
	waitForSnapshot(t, c, "task", "new", func(s Snapshot) bool { return s.IOWaitWorkers == 1 })
	c.ReclaimWorker(1)
	thirdGrant := receiveAcquire(t, third)
	wrongInstance := reportFor(thirdReq, thirdGrant.grant, 512*mib, 2*time.Second, time.Second)
	wrongInstance.InstanceID = "old"
	c.Report(wrongInstance)
	snapshot = c.Snapshot("task", "new")
	if snapshot.Concurrency != 0 || snapshot.SequentialBytes != 0 || snapshot.EffectiveBytesPerSecond != 0 {
		t.Fatalf("stale instance modified snapshot: %+v", snapshot)
	}
}

func TestControllerReclaimWorkerReturnsUnusedBudget(t *testing.T) {
	clock := newFakeClock()
	disk := DiskKey("disk")
	c := newTestController(t, clock, 2, 1, 1, Identity{Key: disk, KnownSSD: true, SSD: true})
	first := receiveAcquire(t, acquireAsync(c, request(1, "first", "1", 0, disk, SourceSequential)))
	if first.grant.Bytes == 0 {
		t.Fatal("first lease has no byte budget")
	}
	second := acquireAsync(c, request(2, "second", "1", 1, disk, SourceSequential))
	waitForSnapshot(t, c, "second", "1", func(s Snapshot) bool { return s.IOWaitWorkers == 1 })
	c.ReclaimWorker(0)
	if got := receiveAcquire(t, second); got.err != nil {
		t.Fatal(got.err)
	}
	if snapshot := c.Snapshot("first", "1"); snapshot.Concurrency != 0 || snapshot.SequentialBytes != 0 {
		t.Fatalf("crashed worker consumed budget or stayed active: %+v", snapshot)
	}
}

func TestControllerReclaimWorkerRejectsItsQueuedWindowBeforeRedispatch(t *testing.T) {
	clock := newFakeClock()
	disk := DiskKey("disk")
	c := newTestController(t, clock, 2, 1, 1, Identity{Key: disk, KnownSSD: true, SSD: true})
	active := receiveAcquire(t, acquireAsync(c, request(1, "task", "1", 0, disk, SourceSequential)))
	if active.err != nil {
		t.Fatal(active.err)
	}
	crashedWorkerQueue := acquireAsync(c, request(2, "task", "1", 0, disk, SourceSequential))
	waitForSnapshot(t, c, "task", "1", func(s Snapshot) bool { return s.IOWaitWorkers == 1 })
	healthyWorkerQueue := acquireAsync(c, request(3, "task", "1", 1, disk, SourceSequential))
	waitForSnapshot(t, c, "task", "1", func(s Snapshot) bool { return s.IOWaitWorkers == 2 })

	c.ReclaimWorker(0)
	if got := receiveAcquire(t, crashedWorkerQueue); !errors.Is(got.err, ErrWorkerReclaimed) {
		t.Fatalf("crashed worker queued request = %+v, want ErrWorkerReclaimed", got)
	}
	if got := receiveAcquire(t, healthyWorkerQueue); got.err != nil {
		t.Fatal(got.err)
	}
}

func TestControllerAllowsOnlyOneEffectiveWindowPerWorker(t *testing.T) {
	clock := newFakeClock()
	disk := DiskKey("disk")
	c := newTestController(t, clock, 2, 2, 2, Identity{Key: disk, KnownSSD: true, SSD: true})
	firstReq := request(1, "task", "1", 0, disk, SourceSequential)
	first := receiveAcquire(t, acquireAsync(c, firstReq))
	second := acquireAsync(c, request(2, "task", "1", 0, disk, SourceSequential))
	waitForSnapshot(t, c, "task", "1", func(s Snapshot) bool { return s.Concurrency == 1 && s.IOWaitWorkers == 1 })
	assertStillWaiting(t, second)
	c.Report(reportFor(firstReq, first.grant, 4*mib, time.Second, 0))
	if got := receiveAcquire(t, second); got.err != nil {
		t.Fatal(got.err)
	}
}

func TestControllerLeaseWindowIsBoundedAndSeekIsSingle(t *testing.T) {
	clock := newFakeClock()
	disk := DiskKey("disk")
	c := newTestController(t, clock, 2, 2, 2, Identity{Key: disk, KnownSSD: true, SSD: true})
	large := request(1, "task", "1", 0, disk, SourceSequential)
	large.WantBytes = 64 * mib
	largeGrant := receiveAcquire(t, acquireAsync(c, large))
	if largeGrant.grant.Bytes != 16*mib || largeGrant.grant.Seeks != 0 {
		t.Fatalf("large grant = %+v, want 16 MiB sequential window", largeGrant.grant)
	}
	seek := request(2, "task", "1", 1, disk, SourceRandom)
	seek.WantBytes = 64 * mib
	seek.WantSeek = true
	seekGrant := receiveAcquire(t, acquireAsync(c, seek))
	if seekGrant.grant.Bytes < mib || seekGrant.grant.Bytes > 16*mib || seekGrant.grant.Seeks != 1 {
		t.Fatalf("seek grant = %+v, want bounded single-seek window", seekGrant.grant)
	}
}

func TestControllerAbnormalLeaseConfigCannotExceedAbsoluteBounds(t *testing.T) {
	for _, test := range []struct {
		name string
		min  int64
		max  int64
	}{
		{name: "both above absolute maximum", min: 32 * mib, max: 64 * mib},
		{name: "minimum above configured maximum", min: 32 * mib, max: 2 * mib},
	} {
		t.Run(test.name, func(t *testing.T) {
			clock := newFakeClock()
			policy := testPolicy(1, 1)
			policy.MinLeaseBytes = test.min
			policy.MaxLeaseBytes = test.max
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			c := NewController(ctx, ControllerOptions{
				Clock:       clock,
				WorkerCount: 1,
				Policy:      policy,
				Identities: map[DiskKey]Identity{
					"disk": {Key: "disk", KnownSSD: true, SSD: true},
				},
			})
			req := request(1, "task", "1", 0, "disk", SourceSequential)
			req.WantBytes = 64 * mib
			grant, err := c.Acquire(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if grant.Bytes != 16*mib {
				t.Fatalf("grant bytes = %d, want absolute 16 MiB maximum", grant.Bytes)
			}
		})
	}
}

func TestControllerHistoryCachesAreBoundedWithoutEvictingInUseFairness(t *testing.T) {
	clock := newFakeClock()
	c := &controller{clock: clock, options: normalizeOptions(ControllerOptions{WorkerCount: 4, Policy: testPolicy(2, 4)})}
	state := newOwnerState()
	disk := &diskState{
		active:   make(map[uint64]*activeLease),
		queues:   make(map[TaskIdentity]*taskQueue),
		fairness: make(map[TaskIdentity]*fairnessHistory),
	}
	state.disks["disk"] = disk
	activeID := TaskIdentity{TaskID: "active", InstanceID: "1"}
	pendingID := TaskIdentity{TaskID: "pending", InstanceID: "1"}
	activeReq := request(1, activeID.TaskID, activeID.InstanceID, 0, "disk", SourceSequential)
	disk.active[1] = &activeLease{req: activeReq, grant: Grant{LeaseID: 1, Generation: 1}}
	disk.queues[pendingID] = &taskQueue{items: []*pendingRequest{{req: request(2, pendingID.TaskID, pendingID.InstanceID, 1, "disk", SourceSequential)}}}

	activeHistory, ok := c.ensureTaskHistory(&state, activeID)
	if !ok {
		t.Fatal("failed to admit active history")
	}
	activeHistory.stats.sequentialBytes = 11
	pendingHistory, ok := c.ensureTaskHistory(&state, pendingID)
	if !ok {
		t.Fatal("failed to admit pending history")
	}
	pendingHistory.generation = 7
	activeFairness, ok := c.ensureDiskFairness(&state, disk, activeID)
	if !ok {
		t.Fatal("failed to admit active fairness")
	}
	activeFairness.grants = 9
	pendingFairness, ok := c.ensureDiskFairness(&state, disk, pendingID)
	if !ok {
		t.Fatal("failed to admit pending fairness")
	}
	pendingFairness.grants = 5

	for i := 0; i < maxTaskHistoryEntries*2; i++ {
		identity := TaskIdentity{TaskID: fmt.Sprintf("history-%04d", i), InstanceID: "1"}
		if _, ok := c.ensureTaskHistory(&state, identity); !ok {
			t.Fatalf("task history admission %d failed despite evictable entries", i)
		}
		if _, ok := c.ensureDiskFairness(&state, disk, identity); !ok {
			t.Fatalf("fairness admission %d failed despite evictable entries", i)
		}
	}

	if len(state.histories) > maxTaskHistoryEntries {
		t.Fatalf("task histories grew to %d, limit %d", len(state.histories), maxTaskHistoryEntries)
	}
	if len(disk.fairness) > maxTaskHistoryEntries {
		t.Fatalf("disk fairness histories grew to %d, limit %d", len(disk.fairness), maxTaskHistoryEntries)
	}
	if got := state.histories[activeID]; got == nil || got.stats.sequentialBytes != 11 {
		t.Fatalf("active history was evicted or changed: %+v", got)
	}
	if got := state.histories[pendingID]; got == nil || got.generation != 7 {
		t.Fatalf("pending history was evicted or changed: %+v", got)
	}
	if got := disk.fairness[activeID]; got == nil || got.grants != 9 {
		t.Fatalf("active fairness was evicted or changed: %+v", got)
	}
	if got := disk.fairness[pendingID]; got == nil || got.grants != 5 {
		t.Fatalf("pending fairness was evicted or changed: %+v", got)
	}
}

func TestControllerWindowWaitSamplesUseFixedCapacityRing(t *testing.T) {
	var window observationWindow
	for i := 0; i < maxWindowWaitSamples+17; i++ {
		window.addWaitSample(time.Duration(i) * time.Millisecond)
	}
	samples := window.waitSamples()
	if len(samples) != maxWindowWaitSamples {
		t.Fatalf("wait sample count = %d, want %d", len(samples), maxWindowWaitSamples)
	}
	if samples[0] != 17*time.Millisecond || samples[len(samples)-1] != time.Duration(maxWindowWaitSamples+16)*time.Millisecond {
		t.Fatalf("ring did not retain newest bounded samples: first=%v last=%v", samples[0], samples[len(samples)-1])
	}
}
