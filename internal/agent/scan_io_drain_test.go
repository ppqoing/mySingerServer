package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"dedup/internal/diskio"
	fileenum "dedup/internal/enum"
	"dedup/internal/proto"
	"dedup/internal/store"
	"dedup/internal/worker"
)

// Break caught: scan preparation registers every media route up front and the
// HDD image phase submits only one job, so a 24-worker pool remains idle while
// the scan owns an unbounded hidden backlog.
func TestScanBoundedPipelineAllowsHDDImagesInFlightWithoutUnboundedRoutes(t *testing.T) {
	const workerCount = 24
	const fileCount = 120
	records := make([]fileenum.FileRecord, fileCount)
	for index := range records {
		records[index] = fileenum.FileRecord{
			Path:  fmt.Sprintf(`D:\media\%03d.jpg`, index),
			Size:  int64(1000 + index),
			MTime: int64(2000 + index),
		}
	}
	manager, cleanup := newTestScanManager(t, &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\media`: records,
	}}, nil)
	defer cleanup()
	manager.cfg.Worker.Count = workerCount
	manager.cfg.Scan.HDDStreams = workerCount
	manager.cfg.IO.MaxQueuedPerWorker = 4
	pendingFull := make(chan struct{})
	var pendingFullOnce sync.Once
	manager.pendingJobsFull = func() { pendingFullOnce.Do(func() { close(pendingFull) }) }
	pool := newFakeScanPool()
	pool.results = make(chan *worker.JobResultMsg, fileCount)
	submitted := make(chan worker.JobMsg, fileCount)
	pool.onSubmit = func(job worker.JobMsg) { submitted <- job }
	manager.pool = pool
	manager.router = NewPoolRouter(pool, nil)

	done := make(chan proto.TaskDone, 1)
	task := proto.ScanTask{TaskID: "bounded", InstanceID: "instance-a", Roots: []string{`D:\media`}, Phase: 1}
	if ack := manager.Handle(task, captureTaskDone(done)); !ack.Accepted {
		t.Fatalf("ack=%#v", ack)
	}

	first := []worker.JobMsg{
		receiveSubmittedScanJob(t, submitted),
		receiveSubmittedScanJob(t, submitted),
	}
	select {
	case <-pendingFull:
	case <-time.After(time.Second):
		manager.AbortInstance(task.TaskID, task.InstanceID)
		t.Fatal("pendingJobs producer did not reach its exact bounded capacity")
	}

	manager.router.mu.Lock()
	routes := len(manager.router.routes)
	manager.router.mu.Unlock()
	maxPending := manager.cfg.IO.MaxQueuedPerWorker * workerCount
	if routes > maxPending {
		t.Errorf("registered routes=%d exceed bounded pending capacity=%d", routes, maxPending)
	}
	state := manager.tasks[scanTaskIdentity(task)]
	if got := state.done.Load(); got != 0 {
		t.Errorf("done=%d before any durable worker result, want 0", got)
	}
	for _, job := range pool.submittedSnapshot() {
		if job.ScanTaskID != task.TaskID || job.ScanInstanceID != task.InstanceID || job.DiskKey != "physical:7" {
			t.Fatalf("submitted job lost exact I/O identity: %#v", job)
		}
	}

	go func() {
		for _, job := range first {
			pool.results <- successfulScanResult(job)
		}
		for index := len(first); index < fileCount; index++ {
			pool.results <- successfulScanResult(<-submitted)
		}
	}()
	select {
	case final := <-done:
		if final.Stats.Done != fileCount {
			t.Fatalf("TaskDone=%#v", final)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bounded scan did not complete")
	}
}

// Break caught: roots without physical extents share legacy DiskNo zero, so
// the last resolved UNC identity overwrites earlier roots and poisons JobMsg.
func TestScanBoundedPipelineKeepsDistinctDiskKeysWhenDiskNosAreEmpty(t *testing.T) {
	roots := []string{`\\server-a\share`, `\\server-b\share`}
	manager, cleanup := newTestScanManager(t, &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		roots[0]: {{Path: roots[0] + `\a.jpg`, Size: 10, MTime: 20}},
		roots[1]: {{Path: roots[1] + `\b.jpg`, Size: 11, MTime: 21}},
	}}, nil)
	defer cleanup()
	manager.resolver = func(root string) (diskio.Identity, error) {
		if root == roots[0] {
			return diskio.Identity{Key: "network:server-a/share"}, nil
		}
		return diskio.Identity{Key: "network:server-b/share"}, nil
	}
	pool := newFakeScanPool()
	submitted := make(chan worker.JobMsg, 2)
	pool.onSubmit = func(job worker.JobMsg) {
		submitted <- job
		pool.results <- successfulScanResult(job)
	}
	manager.pool = pool
	manager.router = NewPoolRouter(pool, nil)
	done := make(chan proto.TaskDone, 1)
	task := proto.ScanTask{TaskID: "distinct-network", InstanceID: "network-instance", Roots: roots, Phase: 1}
	manager.Handle(task, captureTaskDone(done))
	jobs := []worker.JobMsg{receiveSubmittedScanJob(t, submitted), receiveSubmittedScanJob(t, submitted)}
	got := map[string]string{jobs[0].Path: jobs[0].DiskKey, jobs[1].Path: jobs[1].DiskKey}
	if got[roots[0]+`\a.jpg`] != "network:server-a/share" || got[roots[1]+`\b.jpg`] != "network:server-b/share" {
		t.Fatalf("network job disk keys=%#v", got)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("network scan did not finish")
	}
}

// Break caught: scan holds dispatchMu in an uncancellable Background Submit,
// preventing Drain from publishing cancellation and returning under pressure.
func TestScanLeaseDrainCancelsBlockedSubmitBeforeReturning(t *testing.T) {
	manager, cleanup := newTestScanManager(t, &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\media`: {{Path: `D:\media\blocked.jpg`, Size: 10, MTime: 20}},
	}}, nil)
	defer cleanup()
	pool := newBlockingScanPool()
	manager.pool = pool
	manager.router = NewPoolRouter(pool, nil)
	controller := &scanDrainController{}
	manager.SetIOController(controller)
	done := make(chan proto.TaskDone, 1)
	task := proto.ScanTask{TaskID: "blocked-submit", InstanceID: "blocked-instance", Roots: []string{`D:\media`}, Phase: 1}
	manager.Handle(task, captureTaskDone(done))
	select {
	case <-pool.entered:
	case <-time.After(time.Second):
		t.Fatal("scan did not enter blocked Submit")
	}
	drained := make(chan struct{})
	go func() {
		manager.DrainInstance(task.TaskID, task.InstanceID, proto.TaskDrainStop)
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("Drain remained blocked behind Submit")
	}
	select {
	case final := <-done:
		if final.Reason != proto.TaskDrainStop || final.Stats.Done != 0 {
			t.Fatalf("blocked-submit TaskDone=%#v", final)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked-submit scan did not finish")
	}
}

// Break caught: a delete abort reaches the dispatcher but never exercises the
// real controller queue, leaving a protocol-level lease request stuck behind
// the current small HDD window.
func TestScanLeaseDeleteAbortCancelsRealPendingLease(t *testing.T) {
	records := []fileenum.FileRecord{
		{Path: `D:\media\active.jpg`, Size: 10, MTime: 20},
		{Path: `D:\media\waiting.jpg`, Size: 11, MTime: 21},
	}
	manager, cleanup := newTestScanManager(t, &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\media`: records,
	}}, nil)
	defer cleanup()
	manager.cfg.Scan.HDDStreams = 2
	controllerCtx, stopController := context.WithCancel(context.Background())
	defer stopController()
	controller := newSingleWindowController(controllerCtx)
	pool := newRealLeaseScanPool(controller, false)
	manager.pool = pool
	manager.router = NewPoolRouter(pool, nil)
	manager.SetIOController(controller)

	done := make(chan proto.TaskDone, 1)
	task := proto.ScanTask{TaskID: "real-waiting-lease", InstanceID: "delete-instance", Roots: []string{`D:\media`}, Phase: 1}
	manager.Handle(task, captureTaskDone(done))
	receiveLeaseBarrier(t, pool.granted, "first real lease grant")
	receiveLeaseBarrier(t, pool.waiting, "second protocol lease request")
	waitForControllerIOWait(t, controller, task, 1)

	if accepted, _ := manager.DrainInstance(task.TaskID, task.InstanceID, proto.TaskDrainDelete); !accepted {
		t.Fatal("delete drain was rejected")
	}
	if !manager.AbortInstance(task.TaskID, task.InstanceID) {
		t.Fatal("delete abort was rejected")
	}
	select {
	case err := <-pool.leaseErrors:
		if !errors.Is(err, diskio.ErrTaskCancelled) {
			t.Fatalf("waiting lease error=%v, want ErrTaskCancelled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("real pending lease was not cancelled")
	}
	select {
	case final := <-done:
		if final.Reason != proto.TaskDrainDelete || final.Stats.Done != 0 {
			t.Fatalf("delete-abort TaskDone=%#v", final)
		}
	case <-time.After(time.Second):
		t.Fatal("delete abort did not release submitted route waiters")
	}
	close(pool.release)
	receiveLeaseBarrier(t, pool.reported, "cancelled active lease report")
}

// Break caught: Drain counts an acquired HDD window before that window has
// reported and published its durable terminal.
func TestScanLeasePauseCompletesOnlyAcquiredRealWindowAfterDurableTerminal(t *testing.T) {
	manager, cleanup := newTestScanManager(t, &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\media`: {{Path: `D:\media\active.jpg`, Size: 10, MTime: 20}},
	}}, nil)
	defer cleanup()
	controllerCtx, stopController := context.WithCancel(context.Background())
	defer stopController()
	controller := newSingleWindowController(controllerCtx)
	pool := newRealLeaseScanPool(controller, true)
	manager.pool = pool
	manager.router = NewPoolRouter(pool, nil)
	manager.SetIOController(controller)

	done := make(chan proto.TaskDone, 1)
	task := proto.ScanTask{TaskID: "real-active-lease", InstanceID: "pause-instance", Roots: []string{`D:\media`}, Phase: 1}
	manager.Handle(task, captureTaskDone(done))
	receiveLeaseBarrier(t, pool.granted, "real lease grant")
	if accepted, _ := manager.DrainInstance(task.TaskID, task.InstanceID, proto.TaskDrainPause); !accepted {
		t.Fatal("pause drain was rejected")
	}
	state := manager.tasks[scanTaskIdentity(task)]
	if got := state.done.Load(); got != 0 {
		t.Fatalf("Done=%d while acquired window is still reading, want 0", got)
	}
	close(pool.release)
	receiveLeaseBarrier(t, pool.reported, "real lease report")
	if got := state.done.Load(); got != 0 {
		t.Fatalf("Done=%d after lease report but before durable terminal, want 0", got)
	}
	close(pool.publish)
	select {
	case final := <-done:
		if final.Reason != proto.TaskDrainPause || final.Stats.Done != 1 {
			t.Fatalf("pause TaskDone=%#v", final)
		}
	case <-time.After(time.Second):
		t.Fatal("pause did not complete the acquired durable window")
	}
}

// Break caught: hash progress advances before the SQLite transaction commits.
// The sender boundary is after ApplyHashResults and before Done.Add, so this
// test observes both sides of the actual durable-store boundary.
func TestScanLeaseStopUpdatesDoneOnlyAfterSQLiteDurability(t *testing.T) {
	path := `D:\media\durable.txt`
	manager, cleanup := newTestScanManager(t, &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\media`: {{Path: path, Size: 10, MTime: 20}},
	}}, hasherFunc(func(string) (string, error) { return "durable-hash", nil }))
	defer cleanup()
	persisted := make(chan struct{})
	releaseSender := make(chan struct{})
	done := make(chan proto.TaskDone, 1)
	var persistedOnce sync.Once
	task := proto.ScanTask{TaskID: "sqlite-durable", InstanceID: "stop-instance", Roots: []string{`D:\media`}, Phase: 1}
	sender := func(msgType uint8, value any) error {
		switch msgType {
		case proto.MsgFeatureResult:
			pending, err := manager.st.PendingSnapshot(context.Background(), manager.cfg.MachineID)
			if err != nil {
				t.Errorf("PendingSnapshot at durable boundary: %v", err)
			} else if countPendingFiles(pending) != 0 {
				t.Errorf("SQLite still has pending hash work after durable publish: %#v", pending)
			}
			if got := manager.tasks[scanTaskIdentity(task)].done.Load(); got != 0 {
				t.Errorf("Done=%d before post-commit publication returns, want 0", got)
			}
			persistedOnce.Do(func() { close(persisted) })
			<-releaseSender
		case proto.MsgTaskDone:
			done <- *value.(*proto.TaskDone)
		}
		return nil
	}
	manager.Handle(task, sender)
	receiveLeaseBarrier(t, persisted, "SQLite durable result boundary")
	if accepted, _ := manager.DrainInstance(task.TaskID, task.InstanceID, proto.TaskDrainStop); !accepted {
		t.Fatal("stop drain was rejected")
	}
	if got := manager.tasks[scanTaskIdentity(task)].done.Load(); got != 0 {
		t.Fatalf("Done=%d while durable publication is blocked, want 0", got)
	}
	close(releaseSender)
	select {
	case final := <-done:
		if final.Reason != proto.TaskDrainStop || final.Stats.Done != 1 {
			t.Fatalf("durable stop TaskDone=%#v", final)
		}
	case <-time.After(time.Second):
		t.Fatal("durable stop did not finish")
	}
}

// Break caught: a drain closes dispatch before the disk controller observes
// the durable reason, or counts queued work as complete while the current
// bounded windows are still waiting for their persisted terminals.
func TestScanLeaseDrainPublishesReasonThenCancelsExactInstance(t *testing.T) {
	records := []fileenum.FileRecord{
		{Path: `D:\media\a.jpg`, Size: 10, MTime: 20},
		{Path: `D:\media\b.jpg`, Size: 11, MTime: 21},
		{Path: `D:\media\c.jpg`, Size: 12, MTime: 22},
	}
	manager, cleanup := newTestScanManager(t, &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\media`: records,
	}}, nil)
	defer cleanup()
	manager.cfg.Scan.HDDStreams = 2
	pool := newFakeScanPool()
	submitted := make(chan worker.JobMsg, len(records))
	pool.onSubmit = func(job worker.JobMsg) { submitted <- job }
	manager.pool = pool
	manager.router = NewPoolRouter(pool, nil)

	enteredCancel := make(chan struct{})
	releaseCancel := make(chan struct{})
	controller := &scanDrainController{cancel: func(taskID, instanceID string) {
		state := manager.tasks[scanIdentity{taskID: taskID, instanceID: instanceID}]
		state.mu.Lock()
		reason := state.drainReason
		state.mu.Unlock()
		if reason != proto.TaskDrainPause {
			t.Errorf("controller saw drain reason=%q, want pause", reason)
		}
		close(enteredCancel)
		<-releaseCancel
	}}
	manager.SetIOController(controller)
	done := make(chan proto.TaskDone, 1)
	task := proto.ScanTask{TaskID: "lease-drain", InstanceID: "instance-lease", Roots: []string{`D:\media`}, Phase: 1}
	if ack := manager.Handle(task, captureTaskDone(done)); !ack.Accepted {
		t.Fatalf("ack=%#v", ack)
	}
	inFlight := []worker.JobMsg{
		receiveSubmittedScanJob(t, submitted),
		receiveSubmittedScanJob(t, submitted),
	}

	drainDone := make(chan struct{})
	go func() {
		manager.DrainInstance(task.TaskID, task.InstanceID, proto.TaskDrainPause)
		close(drainDone)
	}()
	select {
	case <-enteredCancel:
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not cancel controller task")
	}
	before := len(inFlight)
	select {
	case unexpected := <-submitted:
		t.Fatalf("new dispatch crossed controller cancel boundary: %#v", unexpected)
	default:
	}
	close(releaseCancel)
	<-drainDone
	for _, job := range inFlight {
		pool.results <- successfulScanResult(job)
	}
	select {
	case final := <-done:
		if final.Reason != proto.TaskDrainPause || final.Stats.Done != int64(before) || final.Stats.Done >= int64(len(records)) {
			t.Fatalf("drained TaskDone=%#v submitted=%d", final, before)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drained scan did not wait for current persisted terminals")
	}
	if got := controller.cancelled(); len(got) != 1 || got[0] != (diskio.TaskIdentity{TaskID: task.TaskID, InstanceID: task.InstanceID}) {
		t.Fatalf("controller cancellations=%#v", got)
	}
}

// Break caught: a late terminal from a drained instance is accepted by a
// replacement task that reused the task ID, advancing the replacement before
// its own durable result arrives.
func TestScanLeaseDrainStaleReplacementDoesNotAdvanceNewInstance(t *testing.T) {
	path := `D:\media\replacement.jpg`
	manager, cleanup := newTestScanManager(t, &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\media`: {{Path: path, Size: 10, MTime: 20}},
	}}, nil)
	defer cleanup()
	manager.cfg.Scan.HDDStreams = 1
	pool := newFakeScanPool()
	submitted := make(chan worker.JobMsg, 2)
	pool.onSubmit = func(job worker.JobMsg) { submitted <- job }
	manager.pool = pool
	manager.router = NewPoolRouter(pool, nil)

	oldDone := make(chan proto.TaskDone, 1)
	oldTask := proto.ScanTask{TaskID: "replacement", InstanceID: "old-instance", Roots: []string{`D:\media`}, Phase: 1}
	manager.Handle(oldTask, captureTaskDone(oldDone))
	oldJob := receiveSubmittedScanJob(t, submitted)
	manager.DrainInstance(oldTask.TaskID, oldTask.InstanceID, proto.TaskDrainPause)
	manager.AbortInstance(oldTask.TaskID, oldTask.InstanceID)
	select {
	case final := <-oldDone:
		if final.Reason != proto.TaskDrainPause || final.Stats.Done != 0 {
			t.Fatalf("old TaskDone=%#v", final)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old instance did not abort")
	}

	newDone := make(chan proto.TaskDone, 1)
	newTask := oldTask
	newTask.InstanceID = "new-instance"
	manager.Handle(newTask, captureTaskDone(newDone))
	newJob := receiveSubmittedScanJob(t, submitted)
	if oldJob.ScanInstanceID != oldTask.InstanceID || newJob.ScanInstanceID != newTask.InstanceID || oldJob.JobID == newJob.JobID {
		t.Fatalf("replacement jobs lost exact identity: old=%#v new=%#v", oldJob, newJob)
	}
	pool.results <- successfulScanResult(oldJob)
	if got := manager.tasks[scanTaskIdentity(newTask)].done.Load(); got != 0 {
		t.Fatalf("new instance done=%d after stale terminal, want 0", got)
	}
	pool.results <- successfulScanResult(newJob)
	select {
	case final := <-newDone:
		if final.Stats.Done != 1 || final.Reason != "" {
			t.Fatalf("new TaskDone=%#v", final)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("new instance did not complete")
	}
}

type scanDrainController struct {
	mu     sync.Mutex
	calls  []diskio.TaskIdentity
	cancel func(string, string)
}

type realLeaseScanPool struct {
	controller   diskio.Controller
	results      chan *worker.JobResultMsg
	crashes      chan worker.CrashRecord
	granted      chan struct{}
	waiting      chan struct{}
	reported     chan struct{}
	release      chan struct{}
	publish      chan struct{}
	leaseErrors  chan error
	publishFirst bool

	mu   sync.Mutex
	next int
}

func newRealLeaseScanPool(controller diskio.Controller, publishFirst bool) *realLeaseScanPool {
	return &realLeaseScanPool{
		controller: controller, results: make(chan *worker.JobResultMsg, 2), crashes: make(chan worker.CrashRecord),
		granted: make(chan struct{}), waiting: make(chan struct{}), reported: make(chan struct{}),
		release: make(chan struct{}), publish: make(chan struct{}), leaseErrors: make(chan error, 1),
		publishFirst: publishFirst,
	}
}

func (pool *realLeaseScanPool) Submit(job *worker.JobMsg) error {
	return pool.SubmitContext(context.Background(), job)
}

func (pool *realLeaseScanPool) SubmitContext(ctx context.Context, job *worker.JobMsg) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	pool.mu.Lock()
	workerID := pool.next
	pool.next++
	pool.mu.Unlock()
	copyJob := *job
	go pool.runLease(workerID, copyJob)
	return nil
}

func (pool *realLeaseScanPool) runLease(workerID int, job worker.JobMsg) {
	if workerID == 1 {
		close(pool.waiting)
	}
	grant, err := pool.controller.Acquire(context.Background(), diskio.Request{
		RequestID: uint64(workerID + 1), TaskID: job.ScanTaskID, InstanceID: job.ScanInstanceID,
		WorkerID: workerID, Disk: diskio.DiskKey(job.DiskKey), Class: diskio.SourceRandom, WantBytes: 1 << 20, WantSeek: true,
	})
	if err != nil {
		pool.leaseErrors <- err
		return
	}
	if workerID != 0 {
		pool.controller.Report(diskio.Report{
			LeaseID: grant.LeaseID, Generation: grant.Generation, TaskID: job.ScanTaskID, InstanceID: job.ScanInstanceID,
			WorkerID: workerID, Disk: diskio.DiskKey(job.DiskKey), Cancelled: true,
		})
		return
	}
	close(pool.granted)
	<-pool.release
	pool.controller.Report(diskio.Report{
		LeaseID: grant.LeaseID, Generation: grant.Generation, TaskID: job.ScanTaskID, InstanceID: job.ScanInstanceID,
		WorkerID: workerID, Disk: diskio.DiskKey(job.DiskKey), Bytes: 1 << 20, Completed: true,
	})
	close(pool.reported)
	if pool.publishFirst {
		<-pool.publish
		pool.results <- successfulScanResult(job)
	}
}

func (pool *realLeaseScanPool) Results() <-chan *worker.JobResultMsg { return pool.results }
func (pool *realLeaseScanPool) Crashes() <-chan worker.CrashRecord   { return pool.crashes }
func (*realLeaseScanPool) Metrics() worker.MetricsSnapshot           { return worker.MetricsSnapshot{} }

func newSingleWindowController(ctx context.Context) diskio.Controller {
	return diskio.NewController(ctx, diskio.ControllerOptions{
		WorkerCount: 2,
		Policy: diskio.PolicyConfig{
			LeaseBytes: 1 << 20, MinLeaseBytes: 1 << 20, MaxLeaseBytes: 1 << 20,
			HDDInitial: 1, SSDInitial: 1, MaxPerDisk: 1, HDDRandomMax: 1, MaxQueuedPerWorker: 4,
		},
		Identities: map[diskio.DiskKey]diskio.Identity{
			"physical:7": {Key: "physical:7", Local: true, KnownSSD: true, SSD: false},
		},
	})
}

func waitForControllerIOWait(t *testing.T, controller diskio.Controller, task proto.ScanTask, want int) {
	t.Helper()
	reached := make(chan struct{})
	go func() {
		for controller.Snapshot(task.TaskID, task.InstanceID).IOWaitWorkers != want {
		}
		close(reached)
	}()
	receiveLeaseBarrier(t, reached, "controller I/O wait snapshot")
}

func receiveLeaseBarrier(t *testing.T, barrier <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-barrier:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func countPendingFiles(pending map[int64][]store.PendingFile) int {
	total := 0
	for _, files := range pending {
		total += len(files)
	}
	return total
}

type blockingScanPool struct {
	entered chan struct{}
	results chan *worker.JobResultMsg
	crashes chan worker.CrashRecord
	once    sync.Once
}

func newBlockingScanPool() *blockingScanPool {
	return &blockingScanPool{entered: make(chan struct{}), results: make(chan *worker.JobResultMsg), crashes: make(chan worker.CrashRecord)}
}
func (pool *blockingScanPool) Submit(job *worker.JobMsg) error {
	return pool.SubmitContext(context.Background(), job)
}
func (pool *blockingScanPool) SubmitContext(ctx context.Context, _ *worker.JobMsg) error {
	pool.once.Do(func() { close(pool.entered) })
	<-ctx.Done()
	return ctx.Err()
}
func (pool *blockingScanPool) Results() <-chan *worker.JobResultMsg { return pool.results }
func (pool *blockingScanPool) Crashes() <-chan worker.CrashRecord   { return pool.crashes }
func (*blockingScanPool) Metrics() worker.MetricsSnapshot           { return worker.MetricsSnapshot{} }

func (*scanDrainController) Acquire(context.Context, diskio.Request) (diskio.Grant, error) {
	return diskio.Grant{}, nil
}
func (*scanDrainController) Report(diskio.Report) {}
func (controller *scanDrainController) CancelTask(taskID, instanceID string) {
	controller.mu.Lock()
	controller.calls = append(controller.calls, diskio.TaskIdentity{TaskID: taskID, InstanceID: instanceID})
	callback := controller.cancel
	controller.mu.Unlock()
	if callback != nil {
		callback(taskID, instanceID)
	}
}
func (*scanDrainController) ReclaimWorker(int)                       {}
func (*scanDrainController) Snapshot(string, string) diskio.Snapshot { return diskio.Snapshot{} }
func (controller *scanDrainController) cancelled() []diskio.TaskIdentity {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return append([]diskio.TaskIdentity(nil), controller.calls...)
}

func receiveSubmittedScanJob(t *testing.T, submitted <-chan worker.JobMsg) worker.JobMsg {
	t.Helper()
	select {
	case job := <-submitted:
		return job
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for scan submission")
		return worker.JobMsg{}
	}
}

func successfulScanResult(job worker.JobMsg) *worker.JobResultMsg {
	return &worker.JobResultMsg{
		JobID: job.JobID, ScanTaskID: job.ScanTaskID, Path: job.Path,
		Kind: job.Kind, Phase: job.Phase, Source: job.Source,
		FieldsDone: job.FieldsMask, SHA512: bytes.Repeat([]byte{0x44}, 64),
		PDQ: bytes.Repeat([]byte{0x55}, 32), Quality: 90, Width: 640, Height: 480,
	}
}
