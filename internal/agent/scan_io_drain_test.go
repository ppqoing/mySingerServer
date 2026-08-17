package agent

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"dedup/internal/diskio"
	fileenum "dedup/internal/enum"
	"dedup/internal/proto"
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
