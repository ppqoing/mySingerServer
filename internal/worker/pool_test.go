//go:build windows

package worker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dedup/internal/diskio"
	"dedup/internal/features"
	"dedup/internal/store"

	"github.com/Microsoft/go-winio"
)

// Each lifecycle test names the production break it catches:
//   - missing Ready timeout must not strand a slot;
//   - the watchdog kind must not collapse image/video into one reason;
//   - Close must set closing before a normal worker EOF can be classified.

func TestRuntimeSnapshotReadyWorkersDeepCopyTaskIdentityWithoutMediaPath(t *testing.T) {
	p := newPoolWithDeps(Config{WorkerCount: 2}, nil, supervisorDeps{})
	first := &workerProc{
		pool:  p,
		index: 0,
		proc:  &snapshotProcess{pid: 4101},
		ready: true,
		current: &activeJob{message: &JobMsg{
			JobID:      71,
			ScanTaskID: "scan-safe",
			Path:       `D:\private\media\secret.mp4`,
			Phase:      Phase2,
		}},
	}
	second := &workerProc{
		pool: p, index: 1, proc: &snapshotProcess{pid: 4102}, ready: true,
	}
	p.active[0] = first
	p.active[1] = second

	got := p.RuntimeSnapshot()
	if got.Expected != 2 || got.Ready != 2 || len(got.Workers) != 2 {
		t.Fatalf("snapshot counters = %#v", got)
	}
	if got.Workers[0].PID != 4101 || !got.Workers[0].Ready ||
		!strings.Contains(got.Workers[0].CurrentTaskSummary, "phase=2") ||
		!strings.Contains(got.Workers[0].CurrentTaskSummary, "job_id=71") ||
		strings.Contains(got.Workers[0].CurrentTaskSummary, "secret.mp4") {
		t.Fatalf("worker snapshot = %#v", got.Workers[0])
	}

	first.mu.Lock()
	first.current.message.ScanTaskID = "mutated-after-snapshot"
	first.mu.Unlock()
	if strings.Contains(got.Workers[0].CurrentTaskSummary, "mutated") {
		t.Fatalf("snapshot exposed mutable task state: %#v", got.Workers[0])
	}
}

func TestRuntimeSnapshotDoesNotTrustScanTaskIDAsDisplaySafe(t *testing.T) {
	p := newPoolWithDeps(Config{WorkerCount: 1}, nil, supervisorDeps{})
	p.active[0] = &workerProc{
		pool: p, index: 0, proc: &snapshotProcess{pid: 4151}, ready: true,
		current: &activeJob{message: &JobMsg{
			JobID: 72, ScanTaskID: `D:\private\media\task-secret.mp4`, Phase: Phase1,
		}},
	}
	got := p.RuntimeSnapshot()
	if strings.Contains(got.Workers[0].CurrentTaskSummary, "task-secret") ||
		strings.Contains(got.Workers[0].CurrentTaskSummary, `D:\private`) {
		t.Fatalf("snapshot trusted external scan task ID: %#v", got.Workers[0])
	}
}

func TestRuntimeSnapshotStartupFailureAndRespawnCountsRemainConsistent(t *testing.T) {
	p := newPoolWithDeps(Config{WorkerCount: 2}, nil, supervisorDeps{})
	p.active[1] = &workerProc{
		pool: p, index: 1, proc: &snapshotProcess{pid: 4202}, ready: true,
	}

	duringFailure := p.RuntimeSnapshot()
	if duringFailure.Expected != 2 || duringFailure.Ready != 1 ||
		len(duringFailure.Workers) != 2 || duringFailure.Workers[0].Ready ||
		duringFailure.Workers[0].PID != 0 ||
		duringFailure.Workers[0].LastErrorSummary == "" {
		t.Fatalf("startup-failure snapshot = %#v", duringFailure)
	}

	p.activeMu.Lock()
	p.active[0] = &workerProc{
		pool: p, index: 0, proc: &snapshotProcess{pid: 4201}, ready: true,
	}
	p.activeMu.Unlock()
	afterRespawn := p.RuntimeSnapshot()
	if afterRespawn.Ready != 2 || afterRespawn.LastErrorSummary != "" {
		t.Fatalf("respawn snapshot = %#v", afterRespawn)
	}
}

func TestRuntimeSnapshotKeepsCrashReasonWhileWorkerRespawns(t *testing.T) {
	p := newPoolWithDeps(Config{WorkerCount: 1}, nil, supervisorDeps{crash: func(CrashRecord) {}})
	p.deps.crash(CrashRecord{WorkerIndex: 0, PID: 4251, Reason: "watchdog_video", File: `D:\secret\clip.mp4`})

	got := p.RuntimeSnapshot()
	if got.Workers[0].LastErrorSummary != "watchdog_video" ||
		!strings.Contains(got.LastErrorSummary, "watchdog_video") ||
		strings.Contains(got.LastErrorSummary, "clip.mp4") {
		t.Fatalf("crash snapshot = %#v", got)
	}
}

func TestRuntimeSnapshotConcurrentReadersSeeBoundedConsistentState(t *testing.T) {
	p := newPoolWithDeps(Config{WorkerCount: 1}, nil, supervisorDeps{})
	w := &workerProc{
		pool: p, index: 0, proc: &snapshotProcess{pid: 4301}, ready: true,
	}
	p.active[0] = w

	var readers sync.WaitGroup
	for reader := 0; reader < 8; reader++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for iteration := 0; iteration < 250; iteration++ {
				got := p.RuntimeSnapshot()
				if got.Expected != 1 || len(got.Workers) != 1 ||
					got.Ready < 0 || got.Ready > got.Expected {
					t.Errorf("inconsistent snapshot = %#v", got)
					return
				}
			}
		}()
	}
	for iteration := 0; iteration < 250; iteration++ {
		w.mu.Lock()
		w.current = &activeJob{message: &JobMsg{
			JobID: int64(iteration + 1), ScanTaskID: fmt.Sprintf("task-%d", iteration), Phase: Phase1,
		}}
		w.current = nil
		w.mu.Unlock()
	}
	readers.Wait()
}

type snapshotProcess struct{ pid int }

func (p *snapshotProcess) PID() int           { return p.pid }
func (*snapshotProcess) Wait() (int32, error) { return 0, nil }
func (*snapshotProcess) Kill() error          { return nil }
func (*snapshotProcess) Close() error         { return nil }

func TestPoolReadyTimeoutKillsAttemptAndRespawns(t *testing.T) {
	h := newLifecycleHarness(t, workerScript{ready: false}, workerScript{ready: true})
	p := h.newPool(Config{WorkerCount: 1, ReadyTimeout: 10 * time.Second, RespawnDelay: 500 * time.Millisecond})
	p.Start()
	t.Cleanup(p.Close)

	readyTimer := h.clock.next(t, 10*time.Second)
	readyTimer.fire()
	if got := <-h.kills; got != 0 {
		t.Fatalf("ready-timeout killed worker index %d, want 0", got)
	}
	select {
	case got := <-h.reaps:
		if got != 0 {
			t.Fatalf("ready-timeout reaped worker index %d, want 0", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ready-timeout process was killed but not reaped")
	}
	h.clock.next(t, 500*time.Millisecond).fire()
	ready := h.ready(t)
	if ready.WorkerIndex != 0 {
		t.Fatalf("replacement Ready index = %d, want 0", ready.WorkerIndex)
	}
	if got := p.Metrics().ReadyWorkers; got != 1 {
		t.Fatalf("ready workers = %d, want 1", got)
	}
}

func TestPoolLogsLaunchOrReadyFailureBeforeRespawn(t *testing.T) {
	h := newLifecycleHarness(t, workerScript{ready: false})
	p := h.newPool(Config{WorkerCount: 1, ReadyTimeout: 10 * time.Second})
	p.Start()
	t.Cleanup(p.Close)
	h.clock.next(t, 10*time.Second).fire()
	h.clock.next(t, 500*time.Millisecond)
	record := findJSONLogRecord(t, h.mainLog.Bytes(), "worker start failed")
	if got := record["worker_index"]; got != float64(0) {
		t.Fatalf("worker_index = %#v, want 0", got)
	}
	errText, ok := record["err"].(string)
	if !ok || !strings.Contains(errText, "Ready timeout") {
		t.Fatalf("launch/Ready error = %#v, want Ready timeout diagnostic", record["err"])
	}
}

func TestPoolRejectsIncompatibleIPCOrDLLReadyAndRespawns(t *testing.T) {
	h := newLifecycleHarness(
		t,
		workerScript{ready: true, readyIPCVersion: IPCCompatibilityVersion + 1},
		workerScript{ready: true, readyDLLVersion: "9.9.9"},
		workerScript{ready: true},
	)
	p := h.newPool(Config{WorkerCount: 1, RespawnDelay: 500 * time.Millisecond})
	p.Start()
	t.Cleanup(p.Close)

	nextRespawn := func() *manualTimer {
		t.Helper()
		for {
			select {
			case timer := <-h.clock.created:
				if timer.duration == 10*time.Second {
					waitFor(t, "incompatible Ready timer stop", timer.stopped.Load)
					continue
				}
				if timer.duration != 500*time.Millisecond {
					t.Fatalf("timer duration = %s, want 500ms", timer.duration)
				}
				return timer
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for respawn timer")
				return nil
			}
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		select {
		case <-h.reaps:
		case <-time.After(2 * time.Second):
			t.Fatalf("incompatible Ready attempt %d was not reaped", attempt+1)
		}
		nextRespawn().fire()
	}
	ready := h.ready(t)
	if ready.IPCVersion != IPCCompatibilityVersion ||
		ready.DLLVersion != MediaCoreDLLVersion {
		t.Fatalf("replacement Ready = %#v", ready)
	}
}

// Break caught: an IPC v1 Worker is admitted after lease messages become a
// required part of the parent/child protocol.
func TestWorkerCompatibilityRejectsIPCVersionOne(t *testing.T) {
	ready := validReadyForTest()
	ready.PID = 9001
	ready.WorkerIndex = 3
	ready.IPCVersion = 1
	if err := validateReady(ready, 3, 9001); err == nil {
		t.Fatal("IPC v1 Ready unexpectedly accepted")
	}
}

type ioLeaseAcquireResult struct {
	grant diskio.Grant
	err   error
}

type ioLeaseBroker struct {
	acquires chan diskio.Request
	results  chan ioLeaseAcquireResult
	reports  chan diskio.Report
	reclaims chan int
}

func newIOLeaseBroker() *ioLeaseBroker {
	return &ioLeaseBroker{
		acquires: make(chan diskio.Request, 8), results: make(chan ioLeaseAcquireResult, 8),
		reports: make(chan diskio.Report, 8), reclaims: make(chan int, 8),
	}
}

func (broker *ioLeaseBroker) Acquire(ctx context.Context, request diskio.Request) (diskio.Grant, error) {
	select {
	case broker.acquires <- request:
	case <-ctx.Done():
		return diskio.Grant{}, ctx.Err()
	}
	select {
	case result := <-broker.results:
		return result.grant, result.err
	case <-ctx.Done():
		return diskio.Grant{}, ctx.Err()
	}
}

func (broker *ioLeaseBroker) Report(report diskio.Report)      { broker.reports <- report }
func (*ioLeaseBroker) CancelTask(string, string)               {}
func (broker *ioLeaseBroker) ReclaimWorker(workerID int)       { broker.reclaims <- workerID }
func (*ioLeaseBroker) Snapshot(string, string) diskio.Snapshot { return diskio.Snapshot{} }

type ioLeaseWorkerHarness struct {
	worker *workerProc
	child  net.Conn
	ipc    *IPCConn
	out    chan workerOutcome
}

func newIOLeaseWorkerHarness(t *testing.T, broker diskio.Controller, index int, job *JobMsg) *ioLeaseWorkerHarness {
	t.Helper()
	parent, child := net.Pipe()
	poolCtx, poolCancel := context.WithCancel(context.Background())
	runCtx, runCancel := context.WithCancel(poolCtx)
	pool := &Pool{ctx: poolCtx, cancel: poolCancel, cfg: Config{IOBroker: broker}}
	worker := &workerProc{
		pool: pool, index: index, proc: &snapshotProcess{pid: 9100 + index},
		conn: parent, ipc: NewIPCConn(parent), done: make(chan struct{}),
		current: &activeJob{message: job, ctx: runCtx, cancel: runCancel},
	}
	harness := &ioLeaseWorkerHarness{worker: worker, child: child, ipc: NewIPCConn(child), out: make(chan workerOutcome, 1)}
	go worker.readLoop(harness.out)
	t.Cleanup(func() {
		runCancel()
		poolCancel()
		_ = parent.Close()
		_ = child.Close()
	})
	return harness
}

func receiveLeaseAcquire(t *testing.T, broker *ioLeaseBroker) diskio.Request {
	t.Helper()
	select {
	case request := <-broker.acquires:
		return request
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for broker Acquire")
		return diskio.Request{}
	}
}

func receiveLeaseEnvelope(t *testing.T, ipc *IPCConn, wantType string) *Envelope {
	t.Helper()
	envelope, err := ipc.Read()
	if err != nil {
		t.Fatalf("read %s: %v", wantType, err)
	}
	if envelope.Type != wantType {
		t.Fatalf("message type = %q, want %q", envelope.Type, wantType)
	}
	return envelope
}

// Break caught: a Worker can select another task, instance, disk, or worker
// identity and thereby escape the lease policy selected by the parent job.
func TestPoolIOLeaseAcquireUsesTrustedCurrentJobIdentity(t *testing.T) {
	broker := newIOLeaseBroker()
	job := &JobMsg{JobID: 101, ScanTaskID: "trusted-task", ScanInstanceID: "trusted-instance", DiskKey: "trusted-disk"}
	h := newIOLeaseWorkerHarness(t, broker, 4, job)
	acquire := IOLeaseAcquireMsg{
		JobID: job.JobID, RequestID: 201, TaskID: "forged-task", InstanceID: "forged-instance",
		DiskKey: "forged-disk", Class: 2, WantBytes: 2 << 20, WantSeek: true,
	}
	if err := h.ipc.Write(MsgIOLeaseAcquire, acquire); err != nil {
		t.Fatal(err)
	}
	request := receiveLeaseAcquire(t, broker)
	if request.TaskID != job.ScanTaskID || request.InstanceID != job.ScanInstanceID ||
		request.Disk != diskio.DiskKey(job.DiskKey) || request.WorkerID != 4 {
		t.Fatalf("broker request used untrusted identity: %#v", request)
	}
	broker.results <- ioLeaseAcquireResult{grant: diskio.Grant{LeaseID: 301, Generation: 7, Bytes: 2 << 20, Seeks: 1}}
	grant, err := DecodeBody[IOLeaseGrantMsg](receiveLeaseEnvelope(t, h.ipc, MsgIOLeaseGrant))
	if err != nil {
		t.Fatal(err)
	}
	if grant.JobID != job.JobID || grant.RequestID != acquire.RequestID || grant.LeaseID != 301 || grant.Generation != 7 {
		t.Fatalf("grant = %#v", grant)
	}
}

// Break caught: an absent broker is interpreted as unlimited I/O permission.
func TestPoolIOLeaseBrokerUnavailableReturnsCancel(t *testing.T) {
	job := &JobMsg{JobID: 102, ScanTaskID: "task", ScanInstanceID: "instance", DiskKey: "disk"}
	h := newIOLeaseWorkerHarness(t, nil, 0, job)
	request := IOLeaseAcquireMsg{
		JobID: job.JobID, RequestID: 202, TaskID: "task", InstanceID: "instance",
		DiskKey: "disk", Class: 1, WantBytes: 1 << 20,
	}
	if err := h.ipc.Write(MsgIOLeaseAcquire, request); err != nil {
		t.Fatal(err)
	}
	cancel, err := DecodeBody[IOLeaseCancelMsg](receiveLeaseEnvelope(t, h.ipc, MsgIOLeaseCancel))
	if err != nil {
		t.Fatal(err)
	}
	if cancel.JobID != job.JobID || cancel.RequestID != request.RequestID {
		t.Fatalf("cancel = %#v", cancel)
	}
}

// Break caught: broker task cancellation is converted into an unlimited grant
// or leaves the Worker blocked without an explicit cancel response.
func TestPoolIOLeaseTaskCancellationReturnsCancel(t *testing.T) {
	broker := newIOLeaseBroker()
	job := &JobMsg{JobID: 103, ScanTaskID: "task", ScanInstanceID: "instance", DiskKey: "disk"}
	h := newIOLeaseWorkerHarness(t, broker, 1, job)
	request := IOLeaseAcquireMsg{
		JobID: job.JobID, RequestID: 203, TaskID: "task", InstanceID: "instance",
		DiskKey: "disk", Class: 1, WantBytes: 1 << 20,
	}
	if err := h.ipc.Write(MsgIOLeaseAcquire, request); err != nil {
		t.Fatal(err)
	}
	_ = receiveLeaseAcquire(t, broker)
	broker.results <- ioLeaseAcquireResult{err: diskio.ErrTaskCancelled}
	cancel, err := DecodeBody[IOLeaseCancelMsg](receiveLeaseEnvelope(t, h.ipc, MsgIOLeaseCancel))
	if err != nil {
		t.Fatal(err)
	}
	if cancel.JobID != job.JobID || cancel.RequestID != request.RequestID {
		t.Fatalf("cancel = %#v", cancel)
	}
}

// Break caught: a broker grant is written after the slot has moved to another
// job, letting the new job consume a stale lease.
func TestPoolIOLeaseGrantRechecksCurrentBeforeWrite(t *testing.T) {
	broker := newIOLeaseBroker()
	oldJob := &JobMsg{JobID: 104, ScanTaskID: "task", ScanInstanceID: "old", DiskKey: "disk"}
	h := newIOLeaseWorkerHarness(t, broker, 2, oldJob)
	request := IOLeaseAcquireMsg{
		JobID: oldJob.JobID, RequestID: 204, TaskID: "task", InstanceID: "old",
		DiskKey: "disk", Class: 1, WantBytes: 1 << 20,
	}
	if err := h.ipc.Write(MsgIOLeaseAcquire, request); err != nil {
		t.Fatal(err)
	}
	_ = receiveLeaseAcquire(t, broker)
	newCtx, newCancel := context.WithCancel(context.Background())
	t.Cleanup(newCancel)
	h.worker.mu.Lock()
	h.worker.current = &activeJob{
		message: &JobMsg{JobID: 105, ScanTaskID: "task", ScanInstanceID: "new", DiskKey: "disk"},
		ctx:     newCtx, cancel: newCancel,
	}
	h.worker.mu.Unlock()
	broker.results <- ioLeaseAcquireResult{grant: diskio.Grant{LeaseID: 304, Generation: 8, Bytes: 1 << 20}}
	select {
	case report := <-broker.reports:
		if !report.Cancelled || report.Completed || report.LeaseID != 304 {
			t.Fatalf("stale grant reclaim report = %#v", report)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stale grant was not reclaimed")
	}
	_ = h.child.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if envelope, err := h.ipc.Read(); err == nil {
		t.Fatalf("stale grant was written to Worker: %#v", envelope)
	}
}

// Break caught: a late report from an old instance contributes statistics to
// the new current job instead of merely releasing the old lease.
func TestPoolIOLeaseStaleInstanceReportOnlyReclaims(t *testing.T) {
	broker := newIOLeaseBroker()
	oldJob := &JobMsg{JobID: 106, ScanTaskID: "task", ScanInstanceID: "old", DiskKey: "disk-old"}
	h := newIOLeaseWorkerHarness(t, broker, 3, oldJob)
	request := IOLeaseAcquireMsg{
		JobID: oldJob.JobID, RequestID: 206, TaskID: "task", InstanceID: "old",
		DiskKey: "disk-old", Class: 1, WantBytes: 2 << 20,
	}
	if err := h.ipc.Write(MsgIOLeaseAcquire, request); err != nil {
		t.Fatal(err)
	}
	_ = receiveLeaseAcquire(t, broker)
	broker.results <- ioLeaseAcquireResult{grant: diskio.Grant{LeaseID: 306, Generation: 9, Bytes: 2 << 20}}
	_, _ = DecodeBody[IOLeaseGrantMsg](receiveLeaseEnvelope(t, h.ipc, MsgIOLeaseGrant))
	newCtx, newCancel := context.WithCancel(context.Background())
	t.Cleanup(newCancel)
	h.worker.mu.Lock()
	h.worker.current = &activeJob{
		message: &JobMsg{JobID: 107, ScanTaskID: "task", ScanInstanceID: "new", DiskKey: "disk-new"},
		ctx:     newCtx, cancel: newCancel,
	}
	h.worker.mu.Unlock()
	report := IOLeaseReportMsg{
		JobID: oldJob.JobID, RequestID: request.RequestID, LeaseID: 306, Generation: 9,
		TaskID: "forged", InstanceID: "forged", DiskKey: "forged",
		Bytes: 1 << 20, ReadNS: 10, WaitNS: 20, Completed: true,
	}
	if err := h.ipc.Write(MsgIOLeaseReport, report); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-broker.reports:
		if got.TaskID != "task" || got.InstanceID != "new" || got.Disk != "disk-new" ||
			got.WorkerID != 3 || !got.Cancelled || got.Completed {
			t.Fatalf("stale report was not trusted-identity reclaim only: %#v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stale report reclaim")
	}
}

// Break caught: a dead Worker leaves its active or queued lease attached to a
// slot forever, reducing broker capacity after respawn.
func TestPoolIOLeaseWorkerExitReclaimsWorker(t *testing.T) {
	broker := newIOLeaseBroker()
	pool := &Pool{cfg: Config{IOBroker: broker}, active: map[int]*workerProc{}}
	left, right := net.Pipe()
	t.Cleanup(func() { _ = right.Close() })
	worker := &workerProc{pool: pool, index: 5, conn: left}
	pool.active[5] = worker
	pool.unregister(worker)
	select {
	case got := <-broker.reclaims:
		if got != 5 {
			t.Fatalf("reclaimed worker = %d, want 5", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker exit did not reclaim broker slot")
	}
}

type closeBlockingIOBroker struct {
	started   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
	finished  chan struct{}
	reclaimed chan int
	releaseMu sync.Once
}

func newCloseBlockingIOBroker() *closeBlockingIOBroker {
	return &closeBlockingIOBroker{
		started: make(chan struct{}), cancelled: make(chan struct{}),
		release: make(chan struct{}), finished: make(chan struct{}),
		reclaimed: make(chan int, 1),
	}
}

func (broker *closeBlockingIOBroker) Acquire(ctx context.Context, _ diskio.Request) (diskio.Grant, error) {
	close(broker.started)
	<-ctx.Done()
	close(broker.cancelled)
	<-broker.release
	close(broker.finished)
	return diskio.Grant{}, ctx.Err()
}

func (*closeBlockingIOBroker) Report(diskio.Report)                    {}
func (*closeBlockingIOBroker) CancelTask(string, string)               {}
func (broker *closeBlockingIOBroker) ReclaimWorker(workerID int)       { broker.reclaimed <- workerID }
func (*closeBlockingIOBroker) Snapshot(string, string) diskio.Snapshot { return diskio.Snapshot{} }
func (broker *closeBlockingIOBroker) releaseAcquire() {
	broker.releaseMu.Do(func() { close(broker.release) })
}

// Break caught: Pool.Close returns while a context-cancelled broker Acquire
// goroutine is still unwinding, leaking Worker-owned shutdown work.
func TestPoolIOLeaseCloseWaitsForBlockedAcquire(t *testing.T) {
	broker := newCloseBlockingIOBroker()
	t.Cleanup(broker.releaseAcquire)
	h := newLifecycleHarness(t, workerScript{ready: true, hangJob: true, acquireOnJob: true})
	p := h.newPool(Config{WorkerCount: 1, IOBroker: broker})
	p.Start()
	h.ready(t)
	job := JobMsg{
		JobID: 108, ScanTaskID: "task-close", ScanInstanceID: "instance-close",
		DiskKey: "disk-close", Path: `D:\media\close.jpg`, Kind: MediaImage, Phase: Phase1,
	}
	if err := p.Submit(&job); err != nil {
		t.Fatal(err)
	}
	h.dispatched(t)
	select {
	case <-broker.started:
	case <-time.After(2 * time.Second):
		t.Fatal("broker Acquire did not start")
	}

	closeDone := make(chan struct{})
	go func() {
		p.Close()
		close(closeDone)
	}()
	select {
	case <-broker.cancelled:
	case <-time.After(2 * time.Second):
		broker.releaseAcquire()
		t.Fatal("Pool.Close did not cancel broker Acquire")
	}
	select {
	case workerID := <-broker.reclaimed:
		if workerID != 0 {
			broker.releaseAcquire()
			t.Fatalf("reclaimed worker = %d, want 0", workerID)
		}
	case <-time.After(2 * time.Second):
		broker.releaseAcquire()
		t.Fatal("Pool.Close did not reclaim broker worker")
	}
	select {
	case <-closeDone:
		broker.releaseAcquire()
		t.Fatal("Pool.Close returned before broker Acquire goroutine finished")
	case <-time.After(100 * time.Millisecond):
	}

	broker.releaseAcquire()
	select {
	case <-broker.finished:
	case <-time.After(2 * time.Second):
		t.Fatal("broker Acquire did not finish after release")
	}
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Pool.Close did not return after broker Acquire finished")
	}
}

func TestPoolCrashDeliveryReservesBoundedChannelForActiveTerminals(t *testing.T) {
	p := &Pool{
		crashes: make(chan CrashRecord, 1),
		quit:    make(chan struct{}),
	}
	before := runtime.NumGoroutine()
	for range 2048 {
		if p.publishCrash(CrashRecord{Reason: "idle_exit"}) {
			t.Fatal("idle crash consumed active-terminal channel capacity")
		}
	}
	if after := runtime.NumGoroutine(); after != before {
		t.Fatalf("publishCrash goroutines before=%d after=%d", before, after)
	}
	if got := len(p.crashes); got != 0 {
		t.Fatalf("idle crashes filled terminal channel: length=%d", got)
	}
	active := CrashRecord{
		JobID: 2, ScanTaskID: "task-active", File: `D:\media\active.jpg`,
	}
	if !p.publishCrash(active) {
		t.Fatal("active crash terminal was not delivered")
	}
	if got := <-p.crashes; got != active {
		t.Fatalf("active crash terminal=%#v, want %#v", got, active)
	}
}

func TestPoolActiveCrashDeliveryWaitsForStaleDrainAndUnblocksOnClose(t *testing.T) {
	p := &Pool{
		crashes: make(chan CrashRecord, 1),
		quit:    make(chan struct{}),
	}
	stale := CrashRecord{
		JobID: 1, ScanTaskID: "task-stale", File: `D:\media\stale.jpg`,
	}
	active := CrashRecord{
		JobID: 2, ScanTaskID: "task-active", File: `D:\media\active.jpg`,
	}
	p.crashes <- stale
	delivered := make(chan bool, 1)
	go func() {
		delivered <- p.publishCrash(active)
	}()
	select {
	case result := <-delivered:
		t.Fatalf("active crash returned before stale drain: delivered=%t", result)
	default:
	}
	if got := <-p.crashes; got != stale {
		t.Fatalf("first crash=%#v, want stale %#v", got, stale)
	}
	select {
	case result := <-delivered:
		if !result {
			t.Fatal("active crash was dropped after stale drain")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active crash remained blocked after stale drain")
	}
	if got := <-p.crashes; got != active {
		t.Fatalf("second crash=%#v, want active %#v", got, active)
	}

	p.crashes <- stale
	unblocked := make(chan bool, 1)
	go func() {
		unblocked <- p.publishCrash(active)
	}()
	p.closing.Store(true)
	close(p.quit)
	select {
	case result := <-unblocked:
		if result {
			t.Fatal("closing pool reported blocked active terminal as delivered")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("closing pool did not unblock active crash delivery")
	}
}

func TestWorkerResultAndSHAQueryValidationRejectForeignOrMalformedPayloads(t *testing.T) {
	knownSHA := bytes64(0x31)
	job := &JobMsg{
		JobID: 801, ScanTaskID: "task-validate", Path: `D:\media\a.jpg`,
		Kind: MediaImage, Phase: Phase1, FieldsMask: MaskAllImage,
		KnownSHA: knownSHA,
	}
	valid := &JobResultMsg{
		JobID: job.JobID, Path: job.Path, Kind: job.Kind,
		SHA512: knownSHA, FieldsDone: MaskAllImage,
		PDQ:     bytes.Repeat([]byte{0x44}, 32),
		Quality: 80, Width: 20, Height: 10,
	}
	if err := validateWorkerResult(job, valid); err != nil {
		t.Fatalf("valid result rejected: %v", err)
	}
	tests := []struct {
		name string
		edit func(*JobResultMsg)
	}{
		{"job ID", func(result *JobResultMsg) { result.JobID++ }},
		{"path", func(result *JobResultMsg) { result.Path = `D:\foreign.jpg` }},
		{"kind", func(result *JobResultMsg) { result.Kind = MediaVideo }},
		{"field subset", func(result *JobResultMsg) { result.FieldsDone |= MaskVideoThumb }},
		{"SHA length", func(result *JobResultMsg) { result.SHA512 = []byte{1} }},
		{"known SHA mismatch", func(result *JobResultMsg) { result.SHA512 = bytes64(0x32) }},
		{"PDQ length", func(result *JobResultMsg) { result.PDQ = []byte{1} }},
		{"quality", func(result *JobResultMsg) { result.Quality = 101 }},
		{"dimensions", func(result *JobResultMsg) { result.Width = 0 }},
		{"foreign error field", func(result *JobResultMsg) {
			result.Errors = []FieldError{{Field: MaskVideoThumb, Stage: "decode", Msg: "bad"}}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := *valid
			result.SHA512 = append([]byte(nil), valid.SHA512...)
			result.PDQ = append([]byte(nil), valid.PDQ...)
			tc.edit(&result)
			if err := validateWorkerResult(job, &result); err == nil {
				t.Fatalf("validateWorkerResult accepted %#v", result)
			}
		})
	}
	phase1CombinedError := *valid
	phase1CombinedError.Errors = []FieldError{{
		Field: job.FieldsMask, Stage: "read", Msg: "whole phase-1 attempt failed",
	}}
	if err := validateWorkerResult(job, &phase1CombinedError); err == nil {
		t.Fatal("merged validator accepted a multi-bit field error")
	}
	phase1WithPhase2Payload := *valid
	phase1WithPhase2Payload.PHashParts = features.EncodePHashParts([9]uint64{})
	if err := validateWorkerResult(job, &phase1WithPhase2Payload); err == nil {
		t.Fatal("phase-1 validator accepted phase-2 payload")
	}
	if err := validateSHAQuery(job, &SHAQueryMsg{
		JobID: job.JobID, SHA512: knownSHA, Kind: job.Kind,
	}); err != nil {
		t.Fatalf("valid SHA query rejected: %v", err)
	}
	for _, query := range []SHAQueryMsg{
		{JobID: job.JobID + 1, SHA512: knownSHA, Kind: job.Kind},
		{JobID: job.JobID, SHA512: knownSHA, Kind: MediaVideo},
		{JobID: job.JobID, SHA512: []byte{1}, Kind: job.Kind},
		{JobID: job.JobID, SHA512: bytes64(0x32), Kind: job.Kind},
	} {
		if err := validateSHAQuery(job, &query); err == nil {
			t.Fatalf("validateSHAQuery accepted %#v", query)
		}
	}
}

func TestVideoBaseFeaturesValidatorAcceptsContactFailurePartial(t *testing.T) {
	job := &JobMsg{
		JobID: 811, ScanTaskID: "task-contact-partial", Path: `D:\media\partial.mp4`,
		Kind: MediaVideo, Phase: Phase1,
		FieldsMask: store.RequiredStageOneMask(store.MediaVideo),
	}
	duration := int64(4321)
	result := &JobResultMsg{
		JobID: job.JobID, ScanTaskID: job.ScanTaskID, Path: job.Path, Kind: job.Kind,
		SHA512: bytes64(0x51), FieldsDone: MaskSHA512 | MaskVideoDuration,
		DurationMS: &duration,
		Errors: []FieldError{{
			Field: MaskVideoContactSheet, Stage: "thumb_cache", Msg: "publish failed",
		}},
	}
	if err := validateWorkerResult(job, result); err != nil {
		t.Fatalf("valid contact failure partial rejected: %v", err)
	}
}

func TestValidateWorkerResultAcceptsOnlyRequestedPhase2ImagePayload(t *testing.T) {
	knownSHA := bytes64(0x62)
	validPHash, validSobel := validPhase2Blobs(t)
	newValid := func() (*JobMsg, *JobResultMsg) {
		return &JobMsg{
				JobID: 821, ScanTaskID: "task-phase2-image",
				Path: `D:\media\phase2.jpg`, Kind: MediaImage, Phase: Phase2,
				FieldsMask: MaskPHashParts | MaskSobelHist, KnownSHA: append([]byte(nil), knownSHA...),
			}, &JobResultMsg{
				JobID: 821, Path: `D:\media\phase2.jpg`, Kind: MediaImage,
				SHA512:     append([]byte(nil), knownSHA...),
				FieldsDone: MaskPHashParts | MaskSobelHist,
				PHashParts: append([]byte(nil), validPHash...),
				SobelHist:  append([]byte(nil), validSobel...),
			}
	}
	job, result := newValid()
	if err := validateWorkerResult(job, result); err != nil {
		t.Fatalf("valid phase-2 image result rejected: %v", err)
	}
	job, result = newValid()
	result.FieldsDone = MaskPHashParts
	result.SobelHist = nil
	if err := validateWorkerResult(job, result); err != nil {
		t.Fatalf("valid partial phase-2 image result rejected: %v", err)
	}

	tests := []struct {
		name string
		edit func(*JobMsg, *JobResultMsg)
	}{
		{"known SHA missing", func(job *JobMsg, _ *JobResultMsg) { job.KnownSHA = nil }},
		{"known SHA length", func(job *JobMsg, _ *JobResultMsg) { job.KnownSHA = []byte{1} }},
		{"result SHA missing", func(_ *JobMsg, result *JobResultMsg) { result.SHA512 = nil }},
		{"result SHA mismatch", func(_ *JobMsg, result *JobResultMsg) { result.SHA512 = bytes64(0x63) }},
		{"phase-1 result bit", func(_ *JobMsg, result *JobResultMsg) { result.FieldsDone |= MaskSHA512 }},
		{"phase-1 payload", func(_ *JobMsg, result *JobResultMsg) {
			result.PDQ = bytes.Repeat([]byte{1}, 32)
		}},
		{"video payload", func(_ *JobMsg, result *JobResultMsg) {
			result.Frames = []FrameFeature{{FrameIdx: 0, Error: "failed"}}
		}},
		{"phash length", func(_ *JobMsg, result *JobResultMsg) { result.PHashParts = []byte{1} }},
		{"phash version", func(_ *JobMsg, result *JobResultMsg) { result.PHashParts[0]++ }},
		{"sobel length", func(_ *JobMsg, result *JobResultMsg) { result.SobelHist = []byte{1} }},
		{"sobel version", func(_ *JobMsg, result *JobResultMsg) { result.SobelHist[0]++ }},
		{"sobel non-finite", func(_ *JobMsg, result *JobResultMsg) {
			binary.LittleEndian.PutUint32(result.SobelHist[4:], 0x7fc00000)
		}},
		{"successful phash missing", func(_ *JobMsg, result *JobResultMsg) { result.PHashParts = nil }},
		{"successful sobel missing", func(_ *JobMsg, result *JobResultMsg) { result.SobelHist = nil }},
		{"unclaimed phash payload", func(_ *JobMsg, result *JobResultMsg) {
			result.FieldsDone &^= MaskPHashParts
		}},
		{"unclaimed sobel payload", func(_ *JobMsg, result *JobResultMsg) {
			result.FieldsDone &^= MaskSobelHist
		}},
		{"foreign error bit", func(_ *JobMsg, result *JobResultMsg) {
			result.Errors = []FieldError{{Field: MaskVideo6F, Stage: "frame", Msg: "bad"}}
		}},
		{"combined error bits", func(_ *JobMsg, result *JobResultMsg) {
			result.Errors = []FieldError{{
				Field: MaskPHashParts | MaskSobelHist, Stage: "decode", Msg: "bad",
			}}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			job, result := newValid()
			tc.edit(job, result)
			if err := validateWorkerResult(job, result); err == nil {
				t.Fatalf("validateWorkerResult accepted job=%#v result=%#v", job, result)
			}
		})
	}

	job, result = newValid()
	result.Errors = []FieldError{{Field: 0, Stage: "stale", Msg: "changed"}}
	if err := validateWorkerResult(job, result); err != nil {
		t.Fatalf("phase-2 file-level error rejected: %v", err)
	}
	job, result = newValid()
	result.FieldsDone &^= MaskPHashParts
	result.PHashParts = nil
	result.Errors = []FieldError{{Field: MaskPHashParts, Stage: "phash", Msg: "bad"}}
	if err := validateWorkerResult(job, result); err != nil {
		t.Fatalf("phase-2 single requested field error rejected: %v", err)
	}
}

func TestValidateWorkerResultAcceptsPartialAndCompletePhase2VideoFrames(t *testing.T) {
	knownSHA := bytes64(0x71)
	validPHash, validSobel := validPhase2Blobs(t)
	frame := func(index int) FrameFeature {
		return FrameFeature{
			FrameIdx: index, TimeMS: int64(index+1) * 1000,
			PDQ256:  bytes.Repeat([]byte{byte(index + 1)}, 32),
			Quality: 75, PHashParts: append([]byte(nil), validPHash...),
			SobelHist: append([]byte(nil), validSobel...),
		}
	}
	newValid := func() (*JobMsg, *JobResultMsg) {
		frames := make([]FrameFeature, 6)
		for i := range frames {
			frames[i] = frame(i)
		}
		return &JobMsg{
				JobID: 831, ScanTaskID: "task-phase2-video",
				Path: `D:\media\phase2.mp4`, Kind: MediaVideo, Phase: Phase2,
				FieldsMask: MaskVideo6F, KnownSHA: append([]byte(nil), knownSHA...),
			}, &JobResultMsg{
				JobID: 831, Path: `D:\media\phase2.mp4`, Kind: MediaVideo,
				SHA512:     append([]byte(nil), knownSHA...),
				FieldsDone: MaskVideo6F, Frames: frames,
			}
	}
	job, result := newValid()
	if err := validateWorkerResult(job, result); err != nil {
		t.Fatalf("zero FrameMask must normalize to full phase-2 video set: %v", err)
	}
	job, result = newValid()
	result.FieldsDone = 0
	result.Frames = result.Frames[:2]
	if err := validateWorkerResult(job, result); err != nil {
		t.Fatalf("valid partial phase-2 video result rejected: %v", err)
	}
	job, result = newValid()
	result.FieldsDone = 0
	result.Frames = []FrameFeature{{FrameIdx: 4, TimeMS: 5000, Error: "ffmpeg failed"}}
	if err := validateWorkerResult(job, result); err != nil {
		t.Fatalf("valid errored phase-2 video frame rejected: %v", err)
	}
	job, result = newValid()
	job.FrameMask = 1 << 1
	result.FieldsDone = 0
	result.Frames = []FrameFeature{frame(4)}
	if err := validateWorkerResult(job, result); err == nil {
		t.Fatal("worker result outside the requested FrameMask reached the persistence boundary")
	}

	tests := []struct {
		name string
		edit func(*JobMsg, *JobResultMsg)
	}{
		{"image job bit", func(job *JobMsg, _ *JobResultMsg) { job.FieldsMask |= MaskPHashParts }},
		{"top-level phash", func(_ *JobMsg, result *JobResultMsg) {
			result.PHashParts = append([]byte(nil), validPHash...)
		}},
		{"top-level sobel", func(_ *JobMsg, result *JobResultMsg) {
			result.SobelHist = append([]byte(nil), validSobel...)
		}},
		{"duplicate index", func(_ *JobMsg, result *JobResultMsg) {
			result.Frames[1].FrameIdx = result.Frames[0].FrameIdx
		}},
		{"negative index", func(_ *JobMsg, result *JobResultMsg) { result.Frames[0].FrameIdx = -1 }},
		{"high index", func(_ *JobMsg, result *JobResultMsg) { result.Frames[0].FrameIdx = 6 }},
		{"missing PDQ", func(_ *JobMsg, result *JobResultMsg) { result.Frames[0].PDQ256 = nil }},
		{"PDQ length", func(_ *JobMsg, result *JobResultMsg) { result.Frames[0].PDQ256 = []byte{1} }},
		{"negative quality", func(_ *JobMsg, result *JobResultMsg) { result.Frames[0].Quality = -1 }},
		{"high quality", func(_ *JobMsg, result *JobResultMsg) { result.Frames[0].Quality = 101 }},
		{"phash version", func(_ *JobMsg, result *JobResultMsg) {
			result.Frames[0].PHashParts[0]++
		}},
		{"sobel non-finite", func(_ *JobMsg, result *JobResultMsg) {
			binary.LittleEndian.PutUint32(result.Frames[0].SobelHist[4:], 0x7f800000)
		}},
		{"error with payload", func(_ *JobMsg, result *JobResultMsg) {
			result.FieldsDone = 0
			result.Frames = []FrameFeature{frame(0)}
			result.Frames[0].Error = "decode failed"
		}},
		{"done with only five frames", func(_ *JobMsg, result *JobResultMsg) {
			result.Frames = result.Frames[:5]
		}},
		{"combined field error", func(_ *JobMsg, result *JobResultMsg) {
			result.Errors = []FieldError{{
				Field: MaskVideo6F | MaskPHashParts, Stage: "frames", Msg: "bad",
			}}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			job, result := newValid()
			tc.edit(job, result)
			if err := validateWorkerResult(job, result); err == nil {
				t.Fatalf("validateWorkerResult accepted job=%#v result=%#v", job, result)
			}
		})
	}
}

func TestValidateWorkerResultRejectsForeignScreenStageOrSource(t *testing.T) {
	pHash, _ := validPhase2Blobs(t)
	job := &JobMsg{
		JobID: 839, ScanTaskID: "stage-owner", Path: `D:\media\stage-owner.jpg`,
		Kind: MediaImage, Phase: Phase2, ScreenStage: ScreenStageTwo, Source: JobSourceManager,
		FieldsMask: MaskPHashParts, KnownSHA: bytes64(0x78),
	}
	base := JobResultMsg{
		JobID: job.JobID, Path: job.Path, Kind: job.Kind,
		ScreenStage: job.ScreenStage, Source: job.Source,
		SHA512: append([]byte(nil), job.KnownSHA...), FieldsDone: MaskPHashParts,
		PHashParts: pHash,
	}
	if err := validateWorkerResult(job, &base); err != nil {
		t.Fatalf("matching stage/source rejected: %v", err)
	}
	foreignStage := base
	foreignStage.ScreenStage = ScreenStageThree
	if err := validateWorkerResult(job, &foreignStage); err == nil {
		t.Fatal("foreign screen stage accepted")
	}
	foreignSource := base
	foreignSource.Source = JobSourceLocal
	if err := validateWorkerResult(job, &foreignSource); err == nil {
		t.Fatal("foreign source accepted")
	}
}

func TestValidateWorkerResultVideoSixFrameStagePayloadIsolation(t *testing.T) {
	pHash, sobel := validPhase2Blobs(t)
	tests := []struct {
		name      string
		stage     ScreenStage
		field     uint32
		wantPHash bool
		wantSobel bool
	}{
		{name: "stage two", stage: ScreenStageTwo, field: MaskVideo6FPHash, wantPHash: true},
		{name: "stage three", stage: ScreenStageThree, field: MaskVideo6FSobel, wantSobel: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := &JobMsg{
				JobID: 840, ScanTaskID: "video-stage", Path: `D:\media\video-stage.mp4`,
				Kind: MediaVideo, Phase: Phase2, ScreenStage: test.stage, Source: JobSourceManager,
				FieldsMask: test.field, FrameMask: FrameMaskFull, KnownSHA: bytes64(0x79),
			}
			result := &JobResultMsg{
				JobID: job.JobID, Path: job.Path, Kind: job.Kind,
				ScreenStage: job.ScreenStage, Source: job.Source,
				SHA512: append([]byte(nil), job.KnownSHA...), FieldsDone: test.field,
			}
			for index := 0; index < 6; index++ {
				frame := FrameFeature{FrameIdx: index, TimeMS: int64(index+1) * 1000}
				if test.wantPHash {
					frame.PHashParts = append([]byte(nil), pHash...)
				}
				if test.wantSobel {
					frame.SobelHist = append([]byte(nil), sobel...)
				}
				result.Frames = append(result.Frames, frame)
			}
			if err := validateWorkerResult(job, result); err != nil {
				t.Fatalf("valid split-stage result rejected: %v", err)
			}
			leaked := *result
			leaked.Frames = append([]FrameFeature(nil), result.Frames...)
			if test.wantPHash {
				leaked.Frames[0].SobelHist = append([]byte(nil), sobel...)
			} else {
				leaked.Frames[0].PHashParts = append([]byte(nil), pHash...)
			}
			if err := validateWorkerResult(job, &leaked); err == nil {
				t.Fatal("split-stage result carrying foreign feature accepted")
			}
		})
	}
}

func TestPoolStampsTrustedPhaseOnlyAfterClaimingValidatedResult(t *testing.T) {
	knownSHA := bytes64(0x79)
	result := JobResultMsg{
		JobID: 841, Phase: Phase1, Path: `D:\media\trusted-phase.jpg`,
		Kind: MediaImage, SHA512: append([]byte(nil), knownSHA...),
	}
	h := newLifecycleHarness(t, workerScript{ready: true, result: &result})
	p := h.newPool(Config{MachineID: "machine-a", WorkerCount: 1})
	p.Start()
	t.Cleanup(p.Close)
	h.ready(t)
	job := JobMsg{
		JobID: result.JobID, ScanTaskID: "task-trusted-phase",
		Path: result.Path, Kind: MediaImage, Phase: Phase2,
		FieldsMask: MaskPHashParts, KnownSHA: append([]byte(nil), knownSHA...),
	}
	if err := p.Submit(&job); err != nil {
		t.Fatal(err)
	}
	select {
	case published := <-p.Results():
		if published.Phase != Phase2 {
			t.Fatalf("published trusted Phase=%d, want %d", published.Phase, Phase2)
		}
		if published.ScanTaskID != job.ScanTaskID {
			t.Fatalf("published scan_task_id=%q, want %q", published.ScanTaskID, job.ScanTaskID)
		}
	case crash := <-p.Crashes():
		t.Fatalf("valid phase-2 result crashed worker: %#v", crash)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for trusted phase result")
	}
}

func validPhase2Blobs(t *testing.T) ([]byte, []byte) {
	t.Helper()
	pHash := features.EncodePHashParts([9]uint64{1, 2, 3, 4, 5, 6, 7, 8, 9})
	var histogram [128]float32
	for i := range histogram {
		histogram[i] = float32(i) / 128
	}
	sobel, err := features.EncodeSobelHist(histogram)
	if err != nil {
		t.Fatal(err)
	}
	return pHash, sobel
}

func TestMalformedWorkerResultOrForeignSHAQueryNeverWritesStoreAndRespawns(t *testing.T) {
	tests := []struct {
		name   string
		script workerScript
	}{
		{
			name: "wrong result path",
			script: workerScript{ready: true, result: &JobResultMsg{
				JobID: 811, Path: `D:\foreign.jpg`, Kind: MediaImage,
			}},
		},
		{
			name: "foreign SHA query",
			script: workerScript{
				ready: true, queryOnJob: true,
				queryOverride: &SHAQueryMsg{
					JobID: 999, SHA512: bytes64(0x41), Kind: MediaImage,
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newLifecycleHarness(t, tc.script, workerScript{ready: true})
			p := h.newPool(Config{MachineID: "machine-a", WorkerCount: 1})
			p.Start()
			t.Cleanup(p.Close)
			h.ready(t)
			job := JobMsg{
				JobID: 811, ScanTaskID: "task-malformed",
				Path: `D:\media\expected.jpg`, Kind: MediaImage,
				Phase: Phase1, FieldsMask: MaskAllImage,
			}
			if err := p.Submit(&job); err != nil {
				t.Fatal(err)
			}
			h.dispatched(t)
			crash := h.crash(t)
			if crash.JobID != job.JobID || crash.File != job.Path ||
				crash.Reason != "pipe_eof" {
				t.Fatalf("malformed protocol crash = %#v", crash)
			}
			if got := h.store.saveCountValue(); got != 0 {
				t.Fatalf("malformed protocol Store writes=%d, want 0", got)
			}
			h.clock.next(t, 500*time.Millisecond).fire()
			h.ready(t)
		})
	}
}

func TestPoolCloseRejectsWorkerWhoseReadyCompletesAfterClosingSnapshot(t *testing.T) {
	h := newLifecycleHarness(t, workerScript{ready: true})
	entered := make(chan struct{})
	release := make(chan struct{})
	h.beforeRegister = func() {
		close(entered)
		<-release
	}
	p := h.newPool(Config{WorkerCount: 1, ShutdownTimeout: 3 * time.Second})
	p.Start()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not reach pre-register gate")
	}
	closed := make(chan struct{})
	go func() {
		p.Close()
		close(closed)
	}()
	fallback := h.clock.next(t, 3*time.Second)
	close(release)
	fallback.fire()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close hung after Ready completed behind the closing snapshot")
	}
	select {
	case <-h.reaps:
	case <-time.After(2 * time.Second):
		t.Fatal("late Ready process was not reaped")
	}
}

func TestPoolSubmitCannotEnqueueAfterCloseLinearizes(t *testing.T) {
	for iteration := 0; iteration < 100; iteration++ {
		h := newLifecycleHarness(t)
		p := h.newPool(Config{WorkerCount: 1})
		p.jobs = make(chan *JobMsg, 1)
		p.jobs <- &JobMsg{JobID: -1}
		submitEntered := make(chan struct{})
		releaseSubmit := make(chan struct{})
		closeEntered := make(chan struct{})
		allowClose := make(chan struct{})
		p.beforeSubmit = func() {
			close(submitEntered)
			<-releaseSubmit
		}
		p.beforeClose = func() {
			close(closeEntered)
			<-allowClose
		}
		submitted := make(chan error, 1)
		go func() {
			submitted <- p.Submit(&JobMsg{JobID: int64(iteration + 1)})
		}()
		<-submitEntered
		closed := make(chan struct{})
		go func() {
			p.Close()
			close(closed)
		}()
		<-closeEntered
		close(releaseSubmit)
		close(allowClose)
		<-closed
		<-p.jobs // make the queue send-ready after quit is closed
		if err := <-submitted; !errors.Is(err, ErrPoolClosed) {
			t.Fatalf("iteration %d Submit error = %v, want ErrPoolClosed after Close linearized", iteration, err)
		}
	}
}

func TestPoolStopAcceptingUnblocksSubmitButDefersFinalClose(t *testing.T) {
	h := newLifecycleHarness(t)
	p := h.newPool(Config{WorkerCount: 1})
	p.jobs = make(chan *JobMsg, 1)
	p.jobs <- &JobMsg{JobID: -1}
	submitEntered := make(chan struct{})
	p.beforeSubmit = func() { close(submitEntered) }
	submitted := make(chan error, 1)
	go func() {
		submitted <- p.Submit(&JobMsg{JobID: 1})
	}()
	<-submitEntered

	p.StopAccepting()
	if err := <-submitted; !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("Submit error=%v, want ErrPoolClosed", err)
	}
	select {
	case _, open := <-p.Results():
		if !open {
			t.Fatal("StopAccepting performed the final results-channel close")
		}
	default:
	}

	var closeCalls atomic.Int64
	p.beforeClose = func() { closeCalls.Add(1) }
	p.Close()
	p.Close()
	if calls := closeCalls.Load(); calls != 1 {
		t.Fatalf("final Close calls=%d, want exactly once", calls)
	}
	if _, open := <-p.Results(); open {
		t.Fatal("final Close did not close results")
	}
}

func TestPoolRepeatedIdleExitKeepsReplacementSupervisedAndCapacityRestored(t *testing.T) {
	exitThree := int32(3)
	h := newLifecycleHarness(t,
		workerScript{ready: true, exitAfterReady: &exitThree},
		workerScript{ready: true, exitAfterReady: &exitThree},
		workerScript{ready: true},
	)
	p := h.newPool(Config{WorkerCount: 1, RespawnDelay: 500 * time.Millisecond})
	p.Start()
	t.Cleanup(p.Close)
	h.ready(t)
	_ = h.crash(t)
	h.clock.next(t, 500*time.Millisecond).fire()
	h.ready(t)
	_ = h.crash(t)
	// The second replacement must already be under Wait/read supervision;
	// otherwise its immediate exit is missed behind a stale free-list entry.
	h.clock.next(t, 500*time.Millisecond).fire()
	h.ready(t)
	job := JobMsg{JobID: 61, Path: `D:\media\after-idle-exits.jpg`, Kind: MediaImage, Phase: Phase1}
	if err := p.Submit(&job); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-p.Results():
		if result.JobID != job.JobID {
			t.Fatalf("result JobID = %d, want %d", result.JobID, job.JobID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restored slot did not process a job")
	}
	if got := p.Metrics().ReadyWorkers; got != 1 {
		t.Fatalf("ready workers = %d, want 1", got)
	}
}

func TestPoolIdleExitAndEOFRecordCrashWithoutMarkingFile(t *testing.T) {
	exitThree := int32(3)
	exitZero := int32(0)
	for _, tc := range []struct {
		name   string
		code   *int32
		reason string
	}{
		{name: "nonzero exit", code: &exitThree, reason: "exit_code"},
		{name: "clean EOF", code: &exitZero, reason: "pipe_eof"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLifecycleHarness(t, workerScript{ready: true, exitAfterReady: tc.code})
			p := h.newPool(Config{WorkerCount: 1})
			p.Start()
			t.Cleanup(p.Close)
			h.ready(t)
			crash := h.crash(t)
			if crash.Reason != tc.reason || crash.File != "" || crash.ExitCode != *tc.code {
				t.Fatalf("idle crash = %#v, want reason=%q empty file exit=%d", crash, tc.reason, *tc.code)
			}
			if got := p.Metrics().Crashes; got != 1 {
				t.Fatalf("idle crash metric = %d, want 1", got)
			}
			if got := h.store.crashCount(); got != 0 {
				t.Fatalf("idle failure MarkCrash calls = %d, want 0", got)
			}
		})
	}
}

func TestPoolWatchdogUsesStableReasonForMediaKind(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    MediaKind
		timeout time.Duration
		reason  string
	}{
		{name: "image", kind: MediaImage, timeout: 30 * time.Second, reason: "watchdog_image"},
		{name: "video", kind: MediaVideo, timeout: 120 * time.Second, reason: "watchdog_video"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLifecycleHarness(t, workerScript{ready: true, hangJob: true})
			p := h.newPool(Config{
				MachineID: "machine-a", WorkerCount: 1,
				ReadyTimeout: 10 * time.Second, ImageTimeout: 30 * time.Second,
				VideoTimeout: 120 * time.Second, RespawnDelay: 500 * time.Millisecond,
			})
			p.Start()
			t.Cleanup(p.Close)
			h.ready(t)

			job := JobMsg{JobID: 71, Path: `D:\media\hung.bin`, Kind: tc.kind, Phase: Phase1}
			if err := p.Submit(&job); err != nil {
				t.Fatalf("Submit: %v", err)
			}
			h.dispatched(t)
			h.clock.next(t, tc.timeout).fire()
			crash := h.crash(t)
			if crash.Reason != tc.reason || crash.File != job.Path {
				t.Fatalf("crash = %#v, want reason=%q file=%q", crash, tc.reason, job.Path)
			}
			if got := h.store.crashCount(); got != 1 {
				t.Fatalf("MarkCrash calls = %d, want 1", got)
			}
			if got := p.Metrics().Crashes; got != 1 {
				t.Fatalf("crashes metric = %d, want 1", got)
			}
			if got := p.Metrics().FilesFailed; got != 1 {
				t.Fatalf("active crash files_failed = %d, want 1", got)
			}
		})
	}
}

// Break caught: an ephemeral preview timeout is persisted as a scan crash,
// changing files.status/sync state even though preview is not analysis work.
func TestPoolPreviewCrashPublishesTerminalWithoutPersistingOrFileMetrics(t *testing.T) {
	h := newLifecycleHarness(t, workerScript{ready: true, hangJob: true})
	p := h.newPool(Config{
		MachineID: "machine-a", WorkerCount: 1,
		ImageTimeout: 30 * time.Second, RespawnDelay: 500 * time.Millisecond,
	})
	p.Start()
	t.Cleanup(p.Close)
	h.ready(t)
	job := JobMsg{
		JobID: 72, ScanTaskID: "preview-timeout", Path: `D:\media\preview.jpg`,
		Kind: MediaImage, Phase: PhasePreview, ScreenStage: ScreenStagePreview,
		Source: JobSourceLocal,
	}
	if err := p.Submit(&job); err != nil {
		t.Fatal(err)
	}
	h.dispatched(t)
	h.clock.next(t, 30*time.Second).fire()
	crash := h.crash(t)
	if crash.JobID != job.JobID || crash.ScanTaskID != job.ScanTaskID || crash.Reason != "watchdog_image" {
		t.Fatalf("preview terminal = %#v", crash)
	}
	if got := h.store.crashCount(); got != 0 {
		t.Fatalf("preview crash persisted through MarkCrash %d times", got)
	}
	if got := p.Metrics(); got.FilesDone != 0 || got.FilesFailed != 0 {
		t.Fatalf("preview crash changed scan file metrics: %#v", got)
	}
}

func TestPoolCloseSendsShutdownAndNormalEOFDoesNotCrashOrRespawn(t *testing.T) {
	h := newLifecycleHarness(t, workerScript{ready: true, exitOnShutdown: true})
	p := h.newPool(Config{WorkerCount: 1, ReadyTimeout: 10 * time.Second, ShutdownTimeout: 3 * time.Second})
	p.Start()
	h.ready(t)

	closed := make(chan struct{})
	go func() {
		p.Close()
		close(closed)
	}()
	select {
	case <-h.shutdowns:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not observe Shutdown")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not finish after graceful worker exit")
	}
	if got := p.Metrics().Crashes; got != 0 {
		t.Fatalf("crashes after graceful Close = %d, want 0", got)
	}
	if got := h.launches.Load(); got != 1 {
		t.Fatalf("launches after graceful Close = %d, want 1", got)
	}
	select {
	case crash := <-h.crashes:
		t.Fatalf("unexpected crash record during Close: %#v", crash)
	default:
	}
}

func TestPoolClassifiesExitEOFTruncatedAndPipeWrite(t *testing.T) {
	exitThree := int32(3)
	tests := []struct {
		name       string
		script     workerScript
		wantReason string
		wantExit   int32
	}{
		{name: "exit code 3", script: workerScript{ready: true, exitOnJob: &exitThree}, wantReason: "exit_code", wantExit: 3},
		{name: "clean EOF", script: workerScript{ready: true, eofOnJob: true}, wantReason: "pipe_eof"},
		{name: "truncated body", script: workerScript{ready: true, truncatedOnJob: true}, wantReason: "exit_code", wantExit: 2},
		{name: "pipe write", script: workerScript{ready: true, failParentWrite: true, hangJob: true}, wantReason: "pipe_write", wantExit: -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newLifecycleHarness(t, tc.script)
			p := h.newPool(Config{MachineID: "machine-a", WorkerCount: 1})
			p.Start()
			t.Cleanup(p.Close)
			h.ready(t)

			job := JobMsg{JobID: 83, Path: `D:\media\broken.jpg`, Kind: MediaImage, Phase: Phase1}
			if err := p.Submit(&job); err != nil {
				t.Fatalf("Submit: %v", err)
			}
			crash := h.crash(t)
			if crash.Reason != tc.wantReason || crash.ExitCode != tc.wantExit {
				t.Fatalf("crash = %#v, want reason=%q exit=%d", crash, tc.wantReason, tc.wantExit)
			}
			if got := h.store.crashCount(); got != 1 {
				t.Fatalf("MarkCrash calls = %d, want 1", got)
			}
		})
	}
}

func TestPoolEOFFirstThenExitThreeUsesBoundedExitArbitration(t *testing.T) {
	h := newLifecycleHarness(t, workerScript{ready: true, eofBeforeExit: true})
	p := h.newPool(Config{MachineID: "machine-a", WorkerCount: 1, ExitGrace: 40 * time.Millisecond})
	p.Start()
	t.Cleanup(p.Close)
	h.ready(t)
	job := JobMsg{JobID: 89, Path: `D:\media\eof-before-exit.jpg`, Kind: MediaImage, Phase: Phase1}
	if err := p.Submit(&job); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.eofBeforeExit:
	case <-time.After(2 * time.Second):
		t.Fatal("helper did not close pipe before exiting")
	}
	h.clock.next(t, 30*time.Second)
	grace := h.clock.next(t, 40*time.Millisecond)
	close(h.releaseExit)
	crash := h.crash(t)
	grace.Stop()
	if crash.Reason != "exit_code" || crash.ExitCode != 3 {
		t.Fatalf("EOF-first crash = %#v, want exit_code 3", crash)
	}
}

func TestPoolConcurrentFailureSignalsClassifyAndKillOnce(t *testing.T) {
	h := newLifecycleHarness(t, workerScript{ready: true, hangJob: true})
	p := h.newPool(Config{MachineID: "machine-a", WorkerCount: 1, ImageTimeout: 30 * time.Second})
	p.Start()
	t.Cleanup(p.Close)
	h.ready(t)
	job := JobMsg{JobID: 91, Path: `D:\media\race.jpg`, Kind: MediaImage, Phase: Phase1}
	if err := p.Submit(&job); err != nil {
		t.Fatal(err)
	}
	h.dispatched(t)
	watchdog := h.clock.next(t, 30*time.Second)
	h.forceEOF()
	watchdog.fire()

	_ = h.crash(t)
	if got := h.store.crashCount(); got != 1 {
		t.Fatalf("MarkCrash calls = %d, want 1", got)
	}
	select {
	case <-h.kills:
	case <-time.After(2 * time.Second):
		t.Fatal("expected worker Kill")
	}
	select {
	case <-h.reaps:
	case <-time.After(2 * time.Second):
		t.Fatal("killed worker was not reaped")
	}
	h.clock.next(t, 500*time.Millisecond)
	select {
	case extra := <-h.kills:
		t.Fatalf("worker Kill called more than once, extra index=%d", extra)
	default:
	}
	if got := p.Metrics().Crashes; got != 1 {
		t.Fatalf("crashes metric = %d, want 1", got)
	}
}

func TestPoolActiveFailureOwnershipIsAtomicAgainstEOF(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		script workerScript
		start  func(*testing.T, *lifecycleHarness)
	}{
		{
			name:   "watchdog",
			reason: "watchdog_image",
			script: workerScript{ready: true, hangJob: true},
			start: func(t *testing.T, h *lifecycleHarness) {
				t.Helper()
				h.dispatched(t)
				go h.clock.next(t, 30*time.Second).fire()
			},
		},
		{
			name:   "pipe_write",
			reason: "pipe_write",
			script: workerScript{ready: true, hangJob: true, failParentWrite: true},
			start:  func(*testing.T, *lifecycleHarness) {},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newLifecycleHarness(t, test.script)
			failureEntered := make(chan struct{})
			releaseFailure := make(chan struct{})
			processEntered := make(chan struct{})
			releaseProcess := make(chan struct{})
			var failureOnce sync.Once
			var processOnce sync.Once
			h.beforeFailureCommit = func(reason string) {
				if reason != test.reason {
					return
				}
				failureOnce.Do(func() { close(failureEntered) })
				<-releaseFailure
			}
			h.beforeClaimAttempt = func(reason string) {
				if reason != "pipe_eof" {
					return
				}
				processOnce.Do(func() { close(processEntered) })
				<-releaseProcess
			}
			p := h.newPool(Config{
				MachineID:    "machine-a",
				WorkerCount:  1,
				ImageTimeout: 30 * time.Second,
			})
			p.Start()
			t.Cleanup(p.Close)
			h.ready(t)
			active := p.activeSnapshot()
			if len(active) != 1 {
				t.Fatalf("active workers = %d, want 1", len(active))
			}
			job := JobMsg{
				JobID: 121,
				Path:  `D:\media\atomic-failure.jpg`,
				Kind:  MediaImage,
				Phase: Phase1,
			}
			if err := p.Submit(&job); err != nil {
				t.Fatal(err)
			}
			test.start(t, h)
			select {
			case <-failureEntered:
			case <-time.After(2 * time.Second):
				t.Fatalf("%s did not reach failure-commit gate", test.reason)
			}
			h.forceEOF()
			select {
			case <-processEntered:
			case <-time.After(2 * time.Second):
				close(releaseFailure)
				t.Fatal("EOF classification did not contend for failure ownership")
			}
			exposed := active[0].mu.TryLock()
			if exposed {
				active[0].mu.Unlock()
			}
			close(releaseFailure)
			crash := h.crash(t)
			close(releaseProcess)
			if exposed {
				t.Fatal("job terminal state was published before failure ownership while worker.mu remained available")
			}
			if crash.Reason != test.reason || crash.File != job.Path {
				t.Fatalf("crash = %#v, want reason %q and active file %q", crash, test.reason, job.Path)
			}
			if got := h.store.crashCount(); got != 1 {
				t.Fatalf("MarkCrash calls = %d, want 1", got)
			}
			if got := p.Metrics().Crashes; got != 1 {
				t.Fatalf("crash metric = %d, want 1", got)
			}
			select {
			case extra := <-h.crashes:
				t.Fatalf("duplicate idle crash after active failure won: %#v", extra)
			default:
			}
		})
	}
}

func TestPoolResultWinsTerminalRaceAgainstStartedWatchdog(t *testing.T) {
	h := newLifecycleHarness(t, workerScript{ready: true, gateFirstResult: true})
	watchdogEntered := make(chan struct{})
	releaseWatchdog := make(chan struct{})
	h.beforeWatchdogClaim = func() {
		close(watchdogEntered)
		<-releaseWatchdog
	}
	p := h.newPool(Config{MachineID: "machine-a", WorkerCount: 1, ImageTimeout: 30 * time.Second})
	p.Start()
	t.Cleanup(p.Close)
	h.ready(t)
	first := JobMsg{JobID: 95, Path: `D:\media\result-wins.jpg`, Kind: MediaImage, Phase: Phase1}
	if err := p.Submit(&first); err != nil {
		t.Fatal(err)
	}
	h.dispatched(t)
	watchdog := h.clock.next(t, 30*time.Second)
	go watchdog.fire()
	select {
	case <-watchdogEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog callback did not enter terminal gate")
	}
	close(h.releaseResult)
	select {
	case result := <-p.Results():
		if result.JobID != first.JobID {
			t.Fatalf("first result JobID = %d, want %d", result.JobID, first.JobID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("result did not finish while watchdog callback was gated")
	}
	close(releaseWatchdog)
	second := JobMsg{JobID: 96, Path: `D:\media\slot-still-live.jpg`, Kind: MediaImage, Phase: Phase1}
	if err := p.Submit(&second); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-p.Results():
		if result.JobID != second.JobID {
			t.Fatalf("second result JobID = %d, want %d", result.JobID, second.JobID)
		}
	case crash := <-h.crashes:
		t.Fatalf("result-winning race still classified crash: %#v", crash)
	case <-time.After(2 * time.Second):
		t.Fatal("slot was lost after result/watchdog race")
	}
	select {
	case crash := <-h.crashes:
		t.Fatalf("unexpected crash after result won terminal race: %#v", crash)
	default:
	}
	select {
	case index := <-h.kills:
		t.Fatalf("worker %d was killed after result won terminal race", index)
	default:
	}
}

func TestPoolWatchdogWinsTerminalRaceAndLateResultIsNotPublished(t *testing.T) {
	h := newLifecycleHarness(t, workerScript{ready: true, gateFirstResult: true})
	p := h.newPool(Config{MachineID: "machine-a", WorkerCount: 1, ImageTimeout: 30 * time.Second})
	p.Start()
	t.Cleanup(p.Close)
	h.ready(t)
	job := JobMsg{JobID: 97, Path: `D:\media\watchdog-wins.jpg`, Kind: MediaImage, Phase: Phase1}
	if err := p.Submit(&job); err != nil {
		t.Fatal(err)
	}
	h.dispatched(t)
	h.clock.next(t, 30*time.Second).fire()
	crash := h.crash(t)
	if crash.Reason != "watchdog_image" || crash.File != job.Path {
		t.Fatalf("watchdog crash = %#v", crash)
	}
	close(h.releaseResult)
	select {
	case <-h.reaps:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog-killed process was not reaped")
	}
	select {
	case result := <-p.Results():
		t.Fatalf("late result published after watchdog owned terminal state: %#v", result)
	default:
	}
}

func TestPoolCrashFailsActiveDeduperOwnerOnce(t *testing.T) {
	h := newLifecycleHarness(t, workerScript{ready: true, hangJob: true})
	p := h.newPool(Config{MachineID: "machine-a", WorkerCount: 1, ImageTimeout: 30 * time.Second})
	p.Start()
	t.Cleanup(p.Close)
	h.ready(t)
	sha := make([]byte, 64)
	sha[0] = 0x7a
	first, err := p.dedup.Ask(context.Background(), SHAQueryMsg{JobID: 101, SHA512: sha, Kind: MediaImage})
	if err != nil || first.Found {
		t.Fatalf("owner Ask = %#v, %v", first, err)
	}
	job := JobMsg{JobID: 101, Path: `D:\media\owner.jpg`, Kind: MediaImage, Phase: Phase1}
	if err := p.Submit(&job); err != nil {
		t.Fatal(err)
	}
	h.dispatched(t)
	h.clock.next(t, 30*time.Second).fire()
	_ = h.crash(t)

	retryDone := make(chan error, 1)
	go func() {
		reply, askErr := p.dedup.Ask(context.Background(), SHAQueryMsg{JobID: 102, SHA512: sha, Kind: MediaImage})
		if askErr == nil && reply.Found {
			askErr = errors.New("retry unexpectedly found a cached result")
		}
		retryDone <- askErr
	}()
	select {
	case askErr := <-retryDone:
		if askErr != nil {
			t.Fatalf("retry Ask: %v", askErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deduper owner was not released by crash")
	}
}

func TestPoolMarkCrashFailureIsLoggedWithoutDuplicateClassificationOrRedispatch(t *testing.T) {
	sentinel := errors.New("sqlite mark crash sentinel")
	h := newLifecycleHarness(t,
		workerScript{ready: true, hangJob: true},
		workerScript{ready: true},
	)
	h.store.markCrashErr = sentinel
	p := h.newPool(Config{MachineID: "machine-a", WorkerCount: 1, ImageTimeout: 30 * time.Second})
	p.Start()
	t.Cleanup(p.Close)
	h.ready(t)
	sha := make([]byte, 64)
	sha[0] = 0x5c
	if reply, err := p.dedup.Ask(context.Background(), SHAQueryMsg{JobID: 111, SHA512: sha, Kind: MediaImage}); err != nil || reply.Found {
		t.Fatalf("owner Ask = %#v, %v", reply, err)
	}
	job := JobMsg{JobID: 111, Path: `D:\media\mark-crash-failed.jpg`, Kind: MediaImage, Phase: Phase1}
	if err := p.Submit(&job); err != nil {
		t.Fatal(err)
	}
	h.dispatched(t)
	h.clock.next(t, 30*time.Second).fire()
	crash := h.crash(t)
	h.forceEOF()
	if got := h.store.crashCount(); got != 1 {
		t.Fatalf("MarkCrash calls = %d, want 1", got)
	}
	if got := p.Metrics().Crashes; got != 1 {
		t.Fatalf("crash metric = %d, want 1", got)
	}
	retry, err := p.dedup.Ask(context.Background(), SHAQueryMsg{JobID: 112, SHA512: sha, Kind: MediaImage})
	if err != nil || retry.Found {
		t.Fatalf("deduper retry after MarkCrash failure = %#v, %v; want new owner", retry, err)
	}
	record := findJSONLogRecord(t, h.mainLog.Bytes(), "mark crash failed")
	for key, want := range map[string]any{
		"worker_index": float64(0),
		"pid":          float64(1000),
		"path_id":      PathID(job.Path),
		"reason":       "watchdog_image",
		"err":          sentinel.Error(),
	} {
		if got := record[key]; got != want {
			t.Fatalf("agent error log %s = %#v, want %#v; record=%#v", key, got, want, record)
		}
	}
	if crash.Reason != "watchdog_image" || crash.File != job.Path {
		t.Fatalf("crash record = %#v", crash)
	}
	h.clock.next(t, 500*time.Millisecond).fire()
	h.ready(t)
	next := JobMsg{JobID: 113, Path: `D:\media\after-mark-crash-failure.jpg`, Kind: MediaImage, Phase: Phase1}
	if err := p.Submit(&next); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-p.Results():
		if result.JobID != next.JobID {
			t.Fatalf("replacement first result JobID = %d, want %d; crashed job was redispatched", result.JobID, next.JobID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replacement did not process next job")
	}
	select {
	case extra := <-h.crashes:
		t.Fatalf("duplicate crash classification: %#v", extra)
	default:
	}
}

func findJSONLogRecord(t *testing.T, data []byte, message string) map[string]any {
	t.Helper()
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("agent log line is not JSON: %v; line=%q", err, line)
		}
		if record["msg"] == message {
			return record
		}
	}
	t.Fatalf("agent log has no %q record; data=%q", message, data)
	return nil
}

func TestPoolMetricsReflectResultAndSingleFlight(t *testing.T) {
	result := JobResultMsg{
		JobID: 201, Path: `D:\media\done.jpg`, Kind: MediaImage,
		Decoded: true, ThumbGenerated: true, ThumbCacheHit: true,
		ReadAttempts: 1, DecodeAttempts: 1,
		ReadNS: 500_000, DecodeNS: 250_000,
		Errors: []FieldError{{Field: MaskImagePDQ, Stage: "decode", Msg: "partial"}},
	}
	h := newLifecycleHarness(t, workerScript{ready: true, queryOnJob: true, requireQueryFound: true, result: &result})
	h.store.image = &store.ImageFeature{SHA512: make([]byte, 64), PDQ: make([]byte, 32), Quality: 88, Width: 40, Height: 30}
	p := h.newPool(Config{WorkerCount: 1})
	p.Start()
	t.Cleanup(p.Close)
	h.ready(t)
	job := JobMsg{
		JobID: 201, Path: result.Path, Kind: MediaImage, Phase: Phase1,
		FieldsMask: MaskImagePDQ,
	}
	if err := p.Submit(&job); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-p.Results():
		if got.JobID != job.JobID {
			t.Fatalf("result job = %d, want %d", got.JobID, job.JobID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for result")
	}
	got := p.Metrics()
	if got.FilesDone != 0 || got.FilesFailed != 1 || got.DecodeCalls != 1 ||
		got.ReadAttempts != 1 || got.DecodeAttempts != 1 ||
		got.ReadNS != 500_000 || got.DecodeNS != 250_000 ||
		got.ThumbGenerated != 1 || got.ThumbCacheHits != 1 || got.SingleFlightHits != 0 {
		t.Fatalf("metrics = %#v", got)
	}
	var errorRecord map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(h.errorLog.Bytes()), &errorRecord); err != nil {
		t.Fatalf("errors.log record is not JSON: %v", err)
	}
	for key, want := range map[string]any{
		"path_id": PathID(result.Path), "stage": "decode", "field_mask": float64(MaskImagePDQ), "err": "partial",
	} {
		if value := errorRecord[key]; value != want {
			t.Fatalf("errors.log %s = %#v, want %#v", key, value, want)
		}
	}
	if workerPID, ok := errorRecord["worker_pid"].(float64); !ok || workerPID == 0 {
		t.Fatalf("errors.log worker_pid = %#v, want nonzero", errorRecord["worker_pid"])
	}
}

func TestPoolMetricsCountFailedSubMillisecondAttempts(t *testing.T) {
	p := &Pool{
		ctx:     context.Background(),
		results: make(chan *JobResultMsg, 1),
		quit:    make(chan struct{}),
		dedup:   NewDeduper(nil),
		deps: supervisorDeps{
			logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
			errorLogger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
	}
	p.saveResult(JobMsg{JobID: 301, Path: `D:\media\broken.jpg`, Kind: MediaImage, Phase: Phase1, FieldsMask: MaskImagePDQ}, JobResultMsg{
		JobID: 301, Phase: Phase1, Path: `D:\media\broken.jpg`, Kind: MediaImage,
		ReadAttempts: 1, DecodeAttempts: 1,
		ReadNS: 125_000, DecodeNS: 375_000,
		Errors: []FieldError{{Field: MaskImagePDQ, Stage: "decode", Msg: "broken"}},
	})
	got := p.Metrics()
	if got.FilesFailed != 1 || got.FilesDone != 0 || got.DecodeCalls != 0 ||
		got.ReadAttempts != 1 || got.DecodeAttempts != 1 ||
		got.ReadNS != 125_000 || got.DecodeNS != 375_000 {
		t.Fatalf("failed sub-millisecond metrics = %#v", got)
	}
}

func TestPoolDeduperLookupFailureFallsBackToWorkerComputation(t *testing.T) {
	h := newLifecycleHarness(t, workerScript{ready: true, queryOnJob: true})
	h.store.lookupErr = errors.New("sqlite temporarily unavailable")
	p := h.newPool(Config{WorkerCount: 1})
	p.Start()
	t.Cleanup(p.Close)
	h.ready(t)
	job := JobMsg{JobID: 203, Path: `D:\media\lookup-fallback.jpg`, Kind: MediaImage, Phase: Phase1}
	if err := p.Submit(&job); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-p.Results():
		if result.JobID != job.JobID {
			t.Fatalf("result JobID = %d, want %d", result.JobID, job.JobID)
		}
	case crash := <-h.crashes:
		t.Fatalf("lookup error incorrectly crashed worker: %#v", crash)
	case <-time.After(2 * time.Second):
		t.Fatal("lookup error did not fall back to worker computation")
	}
}

func TestPoolVideoLookupFailurePreservesRequestedMasks(t *testing.T) {
	observed := make(chan SHAReplyMsg, 1)
	fields := uint32(MaskVideoDuration | MaskVideoContactSheet)
	query := SHAQueryMsg{
		JobID: 204, SHA512: make([]byte, 64), Kind: MediaVideo,
		RequestedFields: fields,
	}
	h := newLifecycleHarness(t, workerScript{
		ready: true, queryOnJob: true, queryOverride: &query,
		replyObserved: observed,
	})
	h.store.lookupErr = errors.New("sqlite temporarily unavailable")
	p := h.newPool(Config{WorkerCount: 1})
	p.Start()
	t.Cleanup(p.Close)
	h.ready(t)
	if err := p.Submit(&JobMsg{
		JobID: query.JobID, ScanTaskID: "scan-204",
		Path: `D:\media\lookup-fallback.mp4`, Kind: MediaVideo,
		Phase: Phase1, FieldsMask: MaskSHA512 | fields,
	}); err != nil {
		t.Fatal(err)
	}
	reply := <-observed
	if reply.RequestedFields != fields || reply.MissingFields != fields ||
		reply.FieldsPresent != 0 || reply.RequestedFrames != 0 ||
		reply.MissingFrames != 0 {
		t.Fatalf("fallback reply = %#v", reply)
	}
	if err := reply.ValidateMasks(); err != nil {
		t.Fatalf("fallback reply masks: %v", err)
	}
}

func TestPoolStoreFailureDoesNotResolveDeduperOrPublishSuccess(t *testing.T) {
	sha := make([]byte, 64)
	sha[0] = 0x44
	result := JobResultMsg{
		JobID: 401, Path: `D:\media\store-failed.jpg`, Kind: MediaImage,
		SHA512: sha, FieldsDone: MaskAllImage, PDQ: make([]byte, 32),
		Quality: 90, Width: 40, Height: 30,
	}
	h := newLifecycleHarness(t, workerScript{ready: true, result: &result})
	h.store.saveErr = errors.New("sqlite commit failed")
	p := h.newPool(Config{WorkerCount: 1})
	p.Start()
	t.Cleanup(p.Close)
	h.ready(t)
	owner, err := p.dedup.Ask(context.Background(), SHAQueryMsg{JobID: 401, SHA512: sha, Kind: MediaImage})
	if err != nil || owner.Found {
		t.Fatalf("owner Ask = %#v, %v", owner, err)
	}
	waiter := make(chan SHAReplyMsg, 1)
	waiterErr := make(chan error, 1)
	go func() {
		reply, askErr := p.dedup.Ask(context.Background(), SHAQueryMsg{JobID: 402, SHA512: sha, Kind: MediaImage})
		if askErr != nil {
			waiterErr <- askErr
			return
		}
		waiter <- reply
	}()
	waitForDeduperWaiter(t, p.dedup, MediaImage, sha)
	if err := p.Submit(&JobMsg{
		JobID: 401, Path: result.Path, Kind: MediaImage, Phase: Phase1,
		FieldsMask: MaskAllImage,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case published := <-p.Results():
		if len(published.Errors) != 1 || published.Errors[0].Stage != "store" {
			t.Fatalf("published result errors = %#v, want explicit store failure", published.Errors)
		}
		if published.FieldsDone != 0 {
			t.Fatalf("store failure published fields_done=%d, want 0", published.FieldsDone)
		}
		if len(published.SHA512) != 0 || len(published.PDQ) != 0 ||
			published.DurationMS != nil || published.ThumbPath != "" ||
			len(published.ThumbPDQ) != 0 || published.ThumbQuality != nil {
			t.Fatalf("store failure leaked uncommitted payload: %#v", published)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("store failure result was not published")
	}
	select {
	case reply := <-waiter:
		if reply.Found {
			t.Fatalf("deduper waiter received uncommitted features: %#v", reply)
		}
	case askErr := <-waiterErr:
		t.Fatalf("deduper waiter error: %v", askErr)
	case <-time.After(2 * time.Second):
		t.Fatal("deduper waiter was not released after store failure")
	}
	if got := p.Metrics(); got.FilesDone != 0 || got.FilesFailed != 1 {
		t.Fatalf("metrics after store failure = %#v", got)
	}
}

func TestPoolPublishesOnlyFieldsActuallyClearedByCommittedStoreState(t *testing.T) {
	duration := int64(5000)
	result := JobResultMsg{
		JobID: 403, Path: `D:\media\partial-video.mp4`, Kind: MediaVideo,
		SHA512: bytes64(0x51), FieldsDone: MaskVideoThumb,
		DurationMS: &duration, ThumbPath: `D:\cache\partial.jpg`,
		ThumbPDQ: bytes.Repeat([]byte{1}, 32), ContactSheetWidth: 300, ContactSheetHeight: 200,
	}
	quality := int32(80)
	result.ThumbQuality = &quality
	h := newLifecycleHarness(t, workerScript{ready: true, result: &result})
	h.store.missingMask = MaskVideoThumb
	p := h.newPool(Config{WorkerCount: 1})
	p.Start()
	t.Cleanup(p.Close)
	h.ready(t)
	job := JobMsg{
		JobID: 403, ScanTaskID: "task-partial-video",
		Path: result.Path, Kind: MediaVideo, Phase: Phase1,
		FieldsMask: MaskVideoThumb,
	}
	if err := p.Submit(&job); err != nil {
		t.Fatal(err)
	}
	select {
	case published := <-p.Results():
		if published.FieldsDone != 0 {
			t.Fatalf("published fields_done=%d, store still reports thumb missing", published.FieldsDone)
		}
		if published.ScanTaskID != job.ScanTaskID {
			t.Fatalf("published scan_task_id=%q, want %q", published.ScanTaskID, job.ScanTaskID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for partial video result")
	}
}

func TestPoolPhase2UsesOnlyPhase2StoreMasksCommittedFieldsAndLeavesDeduperUntouched(t *testing.T) {
	h := newLifecycleHarness(t)
	h.store.phase2MissingMask = MaskSobelHist
	p := h.newPool(Config{MachineID: "machine-phase2"})
	t.Cleanup(p.Close)
	sha := bytes64(0x81)
	query := SHAQueryMsg{
		JobID: 901, ScanTaskID: "task-phase2-pool",
		SHA512: sha, Kind: MediaImage,
	}
	owner, err := p.dedup.Ask(context.Background(), query)
	if err != nil || owner.Found {
		t.Fatalf("deduper owner Ask=%#v err=%v", owner, err)
	}
	pHash, sobel := validPhase2Blobs(t)
	p.saveResult(JobMsg{JobID: query.JobID, ScanTaskID: query.ScanTaskID, Path: `D:\media\phase2-pool.jpg`, Kind: MediaImage, Phase: Phase2, FieldsMask: MaskPHashParts | MaskSobelHist, KnownSHA: sha}, JobResultMsg{
		JobID: query.JobID, ScanTaskID: query.ScanTaskID,
		Phase: Phase2, Path: `D:\media\phase2-pool.jpg`, Kind: MediaImage,
		SHA512: sha, FieldsDone: MaskPHashParts | MaskSobelHist,
		PHashParts: pHash, SobelHist: sobel,
	})
	published := <-p.Results()
	if published.FieldsDone != MaskPHashParts {
		t.Fatalf("published fields_done=%#x, want only committed pHash", published.FieldsDone)
	}
	if h.store.saveCountValue() != 0 || h.store.phase2SaveCountValue() != 1 {
		t.Fatalf("store calls phase1=%d phase2=%d, want 0/1",
			h.store.saveCountValue(), h.store.phase2SaveCountValue())
	}
	saved := h.store.lastPhase2Result()
	if saved.MachineID != "machine-phase2" || saved.Path != published.Path ||
		saved.Kind != store.MediaImage || !bytes.Equal(saved.SHA512, sha) ||
		!bytes.Equal(saved.PHashParts, pHash) || !bytes.Equal(saved.SobelHist, sobel) {
		t.Fatalf("saved phase2 result=%#v", saved)
	}
	key, err := dedupeKeyForTask(query.ScanTaskID, query.Kind, query.SHA512)
	if err != nil {
		t.Fatal(err)
	}
	p.dedup.mu.Lock()
	flightStillOwned := p.dedup.flights[key] != nil
	p.dedup.mu.Unlock()
	if flightStillOwned {
		t.Fatal("successful merged save did not resolve deduper flight")
	}
}

func TestPoolPhase2StaleImageWithRealStoreSanitizesPublishedResultAndWritesNothing(t *testing.T) {
	db := openPoolPhase2Store(t)
	const (
		machineID = "machine-phase2-stale-image"
		path      = `D:\media\phase2-stale-image.jpg`
	)
	currentSHA := bytes64(0xb1)
	staleSHA := bytes64(0xb2)
	rowPK := seedCurrentPhase2Ownership(t, db, machineID, path, currentSHA)
	before := snapshotPoolPhase2PublicState(
		t, db, machineID, path, rowPK,
		[]string{hex.EncodeToString(currentSHA), hex.EncodeToString(staleSHA)}, nil,
	)

	p := NewPool(Config{MachineID: machineID}, db, nil, nil, nil)
	t.Cleanup(p.Close)
	pHash, sobel := validPhase2Blobs(t)
	p.saveResult(JobMsg{JobID: 931, Path: path, Kind: MediaImage, Phase: Phase2, FieldsMask: MaskPHashParts | MaskSobelHist, KnownSHA: staleSHA, Size: 100, MTimeMS: 200}, JobResultMsg{
		JobID: 931, Phase: Phase2, Path: path, Kind: MediaImage,
		SHA512: staleSHA, FieldsDone: MaskPHashParts | MaskSobelHist,
		PHashParts: pHash, SobelHist: sobel,
	})
	published := <-p.Results()

	assertStalePhase2PublishedResult(t, published)
	if len(published.SHA512) != 0 {
		t.Fatalf("stale image retained SHA=%x", published.SHA512)
	}
	after := snapshotPoolPhase2PublicState(
		t, db, machineID, path, rowPK,
		[]string{hex.EncodeToString(currentSHA), hex.EncodeToString(staleSHA)}, nil,
	)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("stale image changed public store state\nbefore=%#v\nafter=%#v", before, after)
	}
	if got := p.Metrics(); got.FilesDone != 0 || got.FilesFailed != 1 {
		t.Fatalf("stale image metrics=%#v, want one failed file", got)
	}
}

func TestPoolPhase2StalePartialVideoWithRealStoreClearsFramesAndWritesNothing(t *testing.T) {
	db := openPoolPhase2Store(t)
	const (
		machineID = "machine-phase2-stale-video"
		path      = `D:\media\phase2-stale-video.mp4`
	)
	currentSHA := bytes64(0xc1)
	staleSHA := bytes64(0xc2)
	rowPK := seedCurrentPhase2Ownership(t, db, machineID, path, currentSHA)
	staleSHAHex := hex.EncodeToString(staleSHA)
	before := snapshotPoolPhase2PublicState(
		t, db, machineID, path, rowPK,
		[]string{hex.EncodeToString(currentSHA), staleSHAHex},
		[]string{staleSHAHex + ":0"},
	)

	p := NewPool(Config{MachineID: machineID}, db, nil, nil, nil)
	t.Cleanup(p.Close)
	pHash, sobel := validPhase2Blobs(t)
	p.saveResult(JobMsg{JobID: 932, Path: path, Kind: MediaVideo, Phase: Phase2, FieldsMask: MaskVideo6F, FrameMask: 1, KnownSHA: staleSHA, Size: 100, MTimeMS: 200}, JobResultMsg{
		JobID: 932, Phase: Phase2, Path: path, Kind: MediaVideo,
		SHA512: staleSHA, FieldsDone: 0,
		Frames: []FrameFeature{{
			FrameIdx: 0, TimeMS: 100, PDQ256: bytes.Repeat([]byte{3}, 32),
			Quality: 80, PHashParts: pHash, SobelHist: sobel,
		}},
	})
	published := <-p.Results()

	assertStalePhase2PublishedResult(t, published)
	if len(published.SHA512) != 0 {
		t.Fatalf("stale video retained SHA=%x", published.SHA512)
	}
	after := snapshotPoolPhase2PublicState(
		t, db, machineID, path, rowPK,
		[]string{hex.EncodeToString(currentSHA), staleSHAHex},
		[]string{staleSHAHex + ":0"},
	)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("stale video changed public store state\nbefore=%#v\nafter=%#v", before, after)
	}
	if got := p.Metrics(); got.FilesDone != 0 || got.FilesFailed != 1 {
		t.Fatalf("stale video metrics=%#v, want one failed file", got)
	}
}

func TestPoolPhase2StaleSentinelSkipsMissingMaskQuery(t *testing.T) {
	h := newLifecycleHarness(t)
	h.store.phase2SaveErr = fmt.Errorf("ownership changed: %w", store.ErrPhase2Stale)
	p := h.newPool(Config{MachineID: "machine-phase2-stale-fake"})
	t.Cleanup(p.Close)
	pHash, sobel := validPhase2Blobs(t)
	p.saveResult(JobMsg{JobID: 933, Path: `D:\media\phase2-stale-fake.jpg`, Kind: MediaImage, Phase: Phase2, FieldsMask: MaskPHashParts | MaskSobelHist, KnownSHA: bytes64(0xd1)}, JobResultMsg{
		JobID: 933, Phase: Phase2,
		Path: `D:\media\phase2-stale-fake.jpg`, Kind: MediaImage,
		SHA512: bytes64(0xd1), FieldsDone: MaskPHashParts | MaskSobelHist,
		PHashParts: pHash, SobelHist: sobel,
	})
	published := <-p.Results()

	assertStalePhase2PublishedResult(t, published)
	if got := h.store.phase2MissingMaskCallCount(); got != 0 {
		t.Fatalf("Phase2MissingMask calls=%d after stale sentinel, want 0", got)
	}
}

func assertStalePhase2PublishedResult(t *testing.T, result *JobResultMsg) {
	t.Helper()
	if result.FieldsDone != 0 || result.PHashParts != nil ||
		result.SobelHist != nil || result.Frames != nil {
		t.Fatalf("stale phase2 result leaked feature payload: %#v", result)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("stale phase2 errors=%#v, want exactly one", result.Errors)
	}
	if result.Errors[0].Field != 0 || result.Errors[0].Stage != "stale" ||
		result.Errors[0].Msg == "" {
		t.Fatalf("stale phase2 error=%#v, want field=0 stage=stale message", result.Errors[0])
	}
}

type poolPhase2PublicState struct {
	fileRows   []store.FileRow
	queueRows  map[string][]store.SyncQueueRow
	images     []store.ImageFeatureSyncRow
	frames     []store.VideoFrameSyncRow
	missing    uint32
	queueCount int64
}

func openPoolPhase2Store(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedCurrentPhase2Ownership(
	t *testing.T,
	db *store.DB,
	machineID, path string,
	sha []byte,
) string {
	t.Helper()
	ctx := context.Background()
	if err := db.UpsertEnumerated(ctx, []store.EnumUpsert{{
		MachineID: machineID, DiskNo: 1, Path: path,
		Size: 100, MTime: 200, MissingBase: 0,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyHashResults(ctx, machineID, []store.HashResult{{
		Path: path, SHA512: hex.EncodeToString(sha), Size: 100, MTime: 200,
	}}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.PendingSyncRows(ctx, "files", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("seeded files queue=%#v, want exactly one row", rows)
	}
	missing, err := db.Phase2MissingMask(ctx, machineID, path)
	if err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Fatalf("seeded Phase2MissingMask=%#x, want 0", missing)
	}
	return rows[0].RowPK
}

func snapshotPoolPhase2PublicState(
	t *testing.T,
	db *store.DB,
	machineID, path, rowPK string,
	imageSHAs, frameKeys []string,
) poolPhase2PublicState {
	t.Helper()
	ctx := context.Background()
	state := poolPhase2PublicState{
		queueRows: make(map[string][]store.SyncQueueRow),
	}
	var err error
	state.fileRows, err = db.LoadFilesByIDs(ctx, []string{rowPK})
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"files", "image_features", "video_features", "video_frames"} {
		state.queueRows[table], err = db.PendingSyncRows(ctx, table, 100)
		if err != nil {
			t.Fatal(err)
		}
	}
	state.images, err = db.LoadImageFeaturesBySHAs(ctx, imageSHAs)
	if err != nil {
		t.Fatal(err)
	}
	state.frames, err = db.LoadVideoFramesByKeys(ctx, frameKeys)
	if err != nil {
		t.Fatal(err)
	}
	state.missing, err = db.Phase2MissingMask(ctx, machineID, path)
	if err != nil {
		t.Fatal(err)
	}
	state.queueCount, err = db.PendingSyncCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestPoolPhase2StoreFailureClearsOnlyPhase2PayloadAndAddsOneErrorPerAttemptedBit(t *testing.T) {
	h := newLifecycleHarness(t)
	h.store.phase2SaveErr = errors.New("phase2 transaction failed")
	p := h.newPool(Config{MachineID: "machine-phase2"})
	t.Cleanup(p.Close)
	sha := bytes64(0x91)
	pHash, sobel := validPhase2Blobs(t)
	p.saveResult(JobMsg{JobID: 911, ScanTaskID: "task-phase2-failure", Path: `D:\media\phase2-failure.jpg`, Kind: MediaImage, Phase: Phase2, FieldsMask: MaskPHashParts | MaskSobelHist, KnownSHA: sha}, JobResultMsg{
		JobID: 911, ScanTaskID: "task-phase2-failure",
		Phase: Phase2, Path: `D:\media\phase2-failure.jpg`, Kind: MediaImage,
		SHA512: sha, FieldsDone: MaskPHashParts | MaskSobelHist,
		PHashParts: pHash, SobelHist: sobel,
	})
	published := <-p.Results()
	if published.FieldsDone != 0 || published.PHashParts != nil ||
		published.SobelHist != nil || published.Frames != nil {
		t.Fatalf("phase2 store failure leaked feature payload: %#v", published)
	}
	if len(published.SHA512) != 0 {
		t.Fatalf("phase2 store failure retained uncommitted SHA=%x", published.SHA512)
	}
	if len(published.Errors) != 1 {
		t.Fatalf("phase2 store errors=%#v, want one transaction error", published.Errors)
	}
	for _, fieldError := range published.Errors {
		if fieldError.Stage != "store" || fieldError.Msg != "phase2 transaction failed" {
			t.Fatalf("unexpected phase2 store error=%#v", fieldError)
		}
	}
	if h.store.saveCountValue() != 0 || h.store.phase2SaveCountValue() != 1 {
		t.Fatalf("store calls phase1=%d phase2=%d, want 0/1",
			h.store.saveCountValue(), h.store.phase2SaveCountValue())
	}
}

func TestPoolPhase2LogsEachTopLevelAndErroredFrameOnce(t *testing.T) {
	h := newLifecycleHarness(t)
	h.store.phase2MissingMask = MaskVideo6F
	p := h.newPool(Config{MachineID: "machine-phase2"})
	t.Cleanup(p.Close)
	pHash, sobel := validPhase2Blobs(t)
	p.saveResult(JobMsg{JobID: 921, Path: `D:\media\phase2-log.mp4`, Kind: MediaVideo, Phase: Phase2, FieldsMask: MaskVideo6F, KnownSHA: bytes64(0xa1)}, JobResultMsg{
		JobID: 921, Phase: Phase2,
		Path: `D:\media\phase2-log.mp4`, Kind: MediaVideo, SHA512: bytes64(0xa1),
		Frames: []FrameFeature{
			{
				FrameIdx: 0, PDQ256: bytes.Repeat([]byte{1}, 32),
				Quality: 70, PHashParts: pHash, SobelHist: sobel,
			},
			{FrameIdx: 1, Error: "ffmpeg failed"},
		},
		Errors: []FieldError{{
			Field: MaskVideo6F, Stage: "frames", Msg: "partial video",
		}},
	})
	<-p.Results()
	records := jsonLogRecords(t, h.errorLog.String())
	if len(records) != 2 {
		t.Fatalf("phase2 error log records=%d %#v, want top-level+frame exactly once",
			len(records), records)
	}
	stageCounts := map[string]int{}
	for _, record := range records {
		stageCounts[record["stage"].(string)]++
	}
	if stageCounts["frames"] != 1 || stageCounts["frame"] != 1 {
		t.Fatalf("phase2 error log stage counts=%#v, want frames:1 frame:1", stageCounts)
	}
	if got := p.Metrics(); got.FilesFailed != 1 || got.FilesDone != 0 {
		t.Fatalf("phase2 errored frame metrics=%#v", got)
	}
}

func TestPoolPhase2ErrorLogsUsePathIDAndStageContext(t *testing.T) {
	h := newLifecycleHarness(t)
	h.store.phase2MissingMask = MaskVideo6FPHash
	p := h.newPool(Config{MachineID: "machine-private-log"})
	t.Cleanup(p.Close)
	path := `D:\private\customer-album\secret-name.mp4`
	p.saveResult(JobMsg{
		JobID: 922, Path: path, Kind: MediaVideo, Phase: Phase2,
		FieldsMask: MaskVideo6FPHash, FrameMask: 1,
		ScreenStage: ScreenStageTwo, Source: JobSourceManager, KnownSHA: bytes64(0xa2),
	}, JobResultMsg{
		JobID: 922, Path: path, Kind: MediaVideo, Phase: Phase2,
		ScreenStage: ScreenStageTwo, Source: JobSourceManager, SHA512: bytes64(0xa2),
		Errors: []FieldError{{Field: MaskVideo6FPHash, Stage: "video_frame", Msg: "decode failed for " + path}},
		Frames: []FrameFeature{{FrameIdx: 0, Error: "native failed for " + path}},
	})
	<-p.Results()
	if strings.Contains(h.errorLog.String(), "customer-album") || strings.Contains(h.errorLog.String(), "secret-name.mp4") {
		t.Fatalf("error log leaked sensitive path: %s", h.errorLog.String())
	}
	for _, record := range jsonLogRecords(t, h.errorLog.String()) {
		if record["path_id"] != PathID(path) || record["screen_stage"] != float64(ScreenStageTwo) || record["source"] != string(JobSourceManager) {
			t.Fatalf("safe log context = %#v", record)
		}
	}
}

func TestPoolAllFrameFailuresPersistPublishAndLogStableErrors(t *testing.T) {
	h := newLifecycleHarness(t)
	h.store.phase2MissingMask = MaskVideo6FPHash
	h.store.missingFrames = FrameMaskFull
	p := h.newPool(Config{MachineID: "machine-all-frame-errors"})
	t.Cleanup(p.Close)
	path := `D:\private\all-failed.mp4`
	frames := [6]FrameResult{}
	for index := range frames {
		frames[index] = FrameResult{FrameIdx: index, Status: -20 - int32(index), TimeMS: int64(index) * 1000}
	}
	p.saveResult(JobMsg{
		JobID: 923, Path: path, Kind: MediaVideo, Phase: Phase2,
		FieldsMask: MaskVideo6FPHash, FrameMask: FrameMaskFull,
		ScreenStage: ScreenStageTwo, Source: JobSourceManager, KnownSHA: bytes64(0xa3),
	}, JobResultMsg{
		JobID: 923, Path: path, Kind: MediaVideo, Phase: Phase2,
		ScreenStage: ScreenStageTwo, Source: JobSourceManager, SHA512: bytes64(0xa3),
		FrameResults: frames,
	})
	published := <-p.Results()
	if len(published.Frames) != 6 || published.FieldsDone != 0 {
		t.Fatalf("all-failed result=%#v, want six error-only frames", published)
	}
	for index, frame := range published.Frames {
		if frame.FrameIdx != index || frame.Error != fmt.Sprintf("native_status_%d", -20-int32(index)) ||
			len(frame.PDQ256) != 0 || len(frame.PHashParts) != 0 || len(frame.SobelHist) != 0 {
			t.Fatalf("failed frame[%d]=%#v", index, frame)
		}
	}
	records := jsonLogRecords(t, h.errorLog.String())
	if len(records) != 6 {
		t.Fatalf("all-failed logs=%d %#v, want six", len(records), records)
	}
	for _, record := range records {
		if record["path_id"] != PathID(path) || record["screen_stage"] != float64(ScreenStageTwo) || record["source"] != string(JobSourceManager) {
			t.Fatalf("all-failed log context=%#v", record)
		}
	}
}

func jsonLogRecords(t *testing.T, data string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode JSON log %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func waitForDeduperWaiter(t *testing.T, deduper *Deduper, kind MediaKind, sha []byte) {
	t.Helper()
	key, err := dedupeKeyFor(kind, sha)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		deduper.mu.Lock()
		flight := deduper.flights[key]
		waiting := flight != nil && flight.waiters > 0
		deduper.mu.Unlock()
		if waiting {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("deduper waiter did not block behind owner")
}

func TestPoolDeliversResultBeforeReapingImmediateWorkerExit(t *testing.T) {
	h := newLifecycleHarness(t, workerScript{ready: true, exitAfterResult: true})
	p := h.newPool(Config{WorkerCount: 1})
	p.Start()
	t.Cleanup(p.Close)
	h.ready(t)
	job := JobMsg{JobID: 205, Path: `D:\media\fast-exit.jpg`, Kind: MediaImage, Phase: Phase1}
	if err := p.Submit(&job); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-p.Results():
		if result.JobID != job.JobID {
			t.Fatalf("result JobID = %d, want %d", result.JobID, job.JobID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("result was lost when worker exited immediately after writing it")
	}
	select {
	case <-h.reaps:
	case <-time.After(2 * time.Second):
		t.Fatal("immediately exiting worker was not reaped")
	}
}

func TestPoolPipeWriteCrashDoesNotRedispatchSameJobAfterRespawn(t *testing.T) {
	h := newLifecycleHarness(t,
		workerScript{ready: true, failParentWrite: true},
		workerScript{ready: true},
	)
	p := h.newPool(Config{MachineID: "machine-a", WorkerCount: 1})
	p.Start()
	t.Cleanup(p.Close)
	h.ready(t)
	job := JobMsg{JobID: 211, Path: `D:\media\write-failed.jpg`, Kind: MediaImage, Phase: Phase1}
	if err := p.Submit(&job); err != nil {
		t.Fatal(err)
	}
	_ = h.crash(t)
	h.clock.next(t, 500*time.Millisecond).fire()
	h.ready(t)
	next := JobMsg{JobID: 212, Path: `D:\media\next.jpg`, Kind: MediaImage, Phase: Phase1}
	if err := p.Submit(&next); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-p.Results():
		if result.JobID != next.JobID {
			t.Fatalf("replacement's first result JobID = %d, want new job %d; crashed job was redispatched", result.JobID, next.JobID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replacement did not process next job")
	}
}

func TestPoolCloseFallbackKillsResidualAndConcurrentCloseIsIdempotent(t *testing.T) {
	h := newLifecycleHarness(t, workerScript{ready: true, ignoreShutdown: true})
	p := h.newPool(Config{WorkerCount: 1, ShutdownTimeout: 3 * time.Second})
	p.Start()
	h.ready(t)

	const callers = 12
	var wait sync.WaitGroup
	wait.Add(callers)
	for range callers {
		go func() {
			defer wait.Done()
			p.Close()
		}()
	}
	select {
	case <-h.shutdowns:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not receive Shutdown")
	}
	h.clock.next(t, 3*time.Second).fire()
	select {
	case <-h.kills:
	case <-time.After(2 * time.Second):
		t.Fatal("fallback did not kill residual worker")
	}
	done := make(chan struct{})
	go func() {
		wait.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent Close callers did not all return")
	}
	if got := p.Metrics().Crashes; got != 0 {
		t.Fatalf("crashes during fallback close = %d, want 0", got)
	}
	if got := h.launches.Load(); got != 1 {
		t.Fatalf("launches after close = %d, want 1", got)
	}
}

func TestPoolCloseTimerStartsEvenWhenShutdownPipeWriteBlocks(t *testing.T) {
	h := newLifecycleHarness(t, workerScript{ready: true, blockAfterReady: true})
	p := h.newPool(Config{WorkerCount: 1, ShutdownTimeout: 3 * time.Second})
	p.Start()
	h.ready(t)

	closed := make(chan struct{})
	go func() {
		p.Close()
		close(closed)
	}()
	h.clock.next(t, 3*time.Second).fire()
	select {
	case <-h.kills:
	case <-time.After(2 * time.Second):
		t.Fatal("fallback did not kill worker with blocked Shutdown write")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after killing blocked Shutdown write")
	}
}

func TestPoolCloseFallbackCancelsBlockedStoreWrite(t *testing.T) {
	h := newLifecycleHarness(t, workerScript{ready: true, exitAfterResult: true})
	h.store.saveStarted = make(chan struct{})
	h.store.blockSave = true
	p := h.newPool(Config{WorkerCount: 1, ShutdownTimeout: 3 * time.Second})
	p.Start()
	h.ready(t)
	job := JobMsg{JobID: 271, Path: `D:\media\blocked-store.jpg`, Kind: MediaImage, Phase: Phase1}
	if err := p.Submit(&job); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.store.saveStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Store write did not start")
	}
	closed := make(chan struct{})
	go func() {
		p.Close()
		close(closed)
	}()
	h.clock.next(t, 3*time.Second).fire()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not cancel blocked Store write at fallback deadline")
	}
}

func TestPoolCloseWhileJobActiveIsNormalAndRejectsLaterSubmit(t *testing.T) {
	h := newLifecycleHarness(t, workerScript{ready: true, hangJob: true})
	p := h.newPool(Config{MachineID: "machine-a", WorkerCount: 1})
	p.Start()
	h.ready(t)
	job := JobMsg{JobID: 251, Path: `D:\media\closing.jpg`, Kind: MediaImage, Phase: Phase1}
	if err := p.Submit(&job); err != nil {
		t.Fatal(err)
	}
	h.dispatched(t)
	p.Close()
	if got := h.store.crashCount(); got != 0 {
		t.Fatalf("Close marked active file crashed %d times, want 0", got)
	}
	if got := p.Metrics().Crashes; got != 0 {
		t.Fatalf("Close crash metric = %d, want 0", got)
	}
	if err := p.Submit(&JobMsg{JobID: 252}); !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("Submit after Close error = %v, want ErrPoolClosed", err)
	}
}

func TestPoolRealWindowsNamedPipeHelperLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("real helper-process integration")
	}
	store := &poolTestStore{}
	exited := make(chan int, 1)
	waited := make(chan processWait, 1)
	deps := defaultSupervisorDeps()
	deps.pipeName = func(index int) string {
		return `\\.\pipe\dedup-real-helper-` + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	deps.launch = func(_ Config, pipeName string, index int) (managedProcess, error) {
		command := exec.Command(os.Args[0], "-test.run=^TestPoolHelperProcess$")
		command.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"GO_HELPER_PIPE="+pipeName,
			"GO_HELPER_INDEX="+strconv.Itoa(index),
		)
		if err := command.Start(); err != nil {
			return nil, err
		}
		exited <- command.Process.Pid
		return &observedProcess{managedProcess: &execManagedProcess{command: command}, waited: waited}, nil
	}
	ready := make(chan ReadyMsg, 1)
	deps.ready = func(msg ReadyMsg) { ready <- msg }
	p := newPoolWithDeps(Config{WorkerCount: 1}, store, deps)
	p.Start()
	t.Cleanup(p.Close)
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		p.Close()
		t.Fatal("real helper did not become Ready")
	}
	job := JobMsg{JobID: 301, Path: `D:\media\real.jpg`, Kind: MediaImage, Phase: Phase1}
	if err := p.Submit(&job); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-p.Results():
		if result.JobID != job.JobID || result.Path != job.Path {
			t.Fatalf("real helper result = %#v", result)
		}
	case <-time.After(5 * time.Second):
		p.Close()
		t.Fatal("real helper did not return result")
	}
	pid := <-exited
	p.Close()
	select {
	case outcome := <-waited:
		if outcome.code != 0 || outcome.err != nil {
			t.Fatalf("helper pid %d Wait = code %d, err=%v; want normal exit", pid, outcome.code, outcome.err)
		}
	default:
		t.Fatalf("helper pid %d was not reaped before Pool.Close returned", pid)
	}
	if got := p.Metrics().Crashes; got != 0 {
		t.Fatalf("real helper crashes = %d, want 0", got)
	}
}

type workerScript struct {
	ready             bool
	hangJob           bool
	exitOnShutdown    bool
	ignoreShutdown    bool
	exitOnJob         *int32
	exitAfterReady    *int32
	eofOnJob          bool
	truncatedOnJob    bool
	failParentWrite   bool
	queryOnJob        bool
	acquireOnJob      bool
	requireQueryFound bool
	result            *JobResultMsg
	exitAfterResult   bool
	blockAfterReady   bool
	eofBeforeExit     bool
	gateFirstResult   bool
	readyIPCVersion   int
	readyDLLVersion   string
	readyOverride     *ReadyMsg
	queryOverride     *SHAQueryMsg
	replyObserved     chan<- SHAReplyMsg
}

func validReadyForTest() ReadyMsg {
	return ReadyMsg{
		IPCVersion: IPCCompatibilityVersion, DLLVersion: MediaCoreDLLVersion,
		VideoCoreABI: VideoCoreABIVersion, VideoCoreVersion: VideoCoreVersion,
		FFmpegComponents: []RuntimeComponent{
			{Name: "avformat", BuildVersion: "63.1.0", RuntimeVersion: "63.2.0", BuildMajor: 63, RuntimeMajor: 63},
			{Name: "avcodec", BuildVersion: "63.1.0", RuntimeVersion: "63.2.0", BuildMajor: 63, RuntimeMajor: 63},
			{Name: "avutil", BuildVersion: "61.1.0", RuntimeVersion: "61.2.0", BuildMajor: 61, RuntimeMajor: 61},
			{Name: "swscale", BuildVersion: "10.1.0", RuntimeVersion: "10.2.0", BuildMajor: 10, RuntimeMajor: 10},
		},
	}
}

type lifecycleHarness struct {
	t                   *testing.T
	clock               *manualClock
	scripts             chan workerScript
	listeners           sync.Map
	kills               chan int
	reaps               chan int
	readyCh             chan ReadyMsg
	dispatch            chan JobMsg
	shutdowns           chan struct{}
	crashes             chan CrashRecord
	launches            atomic.Int64
	store               *poolTestStore
	mainLog             bytes.Buffer
	errorLog            bytes.Buffer
	beforeRegister      func()
	beforeWatchdogClaim func()
	beforeFailureCommit func(string)
	beforeClaimAttempt  func(string)
	eofBeforeExit       chan struct{}
	releaseExit         chan struct{}
	releaseResult       chan struct{}
	connMu              sync.Mutex
	children            []net.Conn
}

func newLifecycleHarness(t *testing.T, scripts ...workerScript) *lifecycleHarness {
	t.Helper()
	h := &lifecycleHarness{
		t: t, clock: newManualClock(), scripts: make(chan workerScript, len(scripts)),
		kills: make(chan int, 16), readyCh: make(chan ReadyMsg, 16),
		reaps:    make(chan int, 16),
		dispatch: make(chan JobMsg, 16), shutdowns: make(chan struct{}, 16),
		crashes: make(chan CrashRecord, 16), store: &poolTestStore{},
		eofBeforeExit: make(chan struct{}, 16), releaseExit: make(chan struct{}),
		releaseResult: make(chan struct{}),
	}
	for _, script := range scripts {
		if !script.ignoreShutdown {
			script.exitOnShutdown = true
		}
		h.scripts <- script
	}
	return h
}

func (h *lifecycleHarness) newPool(cfg Config) *Pool {
	h.t.Helper()
	deps := supervisorDeps{
		clock: h.clock,
		pipeName: func(index int) string {
			return `\\.\pipe\dedup-test-` + time.Now().Format("150405.000000000")
		},
		listen: func(name string) (net.Listener, error) {
			l := newChannelListener()
			h.listeners.Store(name, l)
			return l, nil
		},
		launch: func(_ Config, name string, index int) (managedProcess, error) {
			h.launches.Add(1)
			script := <-h.scripts
			value, ok := h.listeners.Load(name)
			if !ok {
				h.t.Fatalf("listener %q not registered before launch", name)
			}
			l := value.(*channelListener)
			parent, child := net.Pipe()
			if script.failParentWrite {
				parent = &writeFailConn{Conn: parent}
			}
			h.connMu.Lock()
			h.children = append(h.children, child)
			h.connMu.Unlock()
			l.accept <- parent
			proc := newFakeProcess(1000+index, index, h.kills, h.reaps)
			go h.serveScript(child, proc, index, script)
			return proc, nil
		},
		ready: func(ready ReadyMsg) {
			h.readyCh <- ready
		},
		crash: func(record CrashRecord) {
			h.crashes <- record
		},
		logger:              slog.New(slog.NewJSONHandler(&h.mainLog, nil)),
		errorLogger:         slog.New(slog.NewJSONHandler(&h.errorLog, nil)),
		beforeRegister:      h.beforeRegister,
		beforeWatchdogClaim: h.beforeWatchdogClaim,
		beforeFailureCommit: h.beforeFailureCommit,
		beforeClaimAttempt:  h.beforeClaimAttempt,
	}
	return newPoolWithDeps(cfg, h.store, deps)
}

func (h *lifecycleHarness) serveScript(conn net.Conn, proc *fakeProcess, index int, script workerScript) {
	defer conn.Close()
	ipc := NewIPCConn(conn)
	if script.ready {
		ipcVersion := script.readyIPCVersion
		if ipcVersion == 0 {
			ipcVersion = IPCCompatibilityVersion
		}
		dllVersion := script.readyDLLVersion
		if dllVersion == "" {
			dllVersion = MediaCoreDLLVersion
		}
		ready := validReadyForTest()
		ready.PID = proc.PID()
		ready.WorkerIndex = index
		ready.IPCVersion = ipcVersion
		ready.DLLVersion = dllVersion
		if script.readyOverride != nil {
			ready = *script.readyOverride
			ready.PID = proc.PID()
			ready.WorkerIndex = index
		}
		if err := ipc.Write(MsgReady, ready); err != nil {
			proc.finish(2)
			return
		}
		if script.exitAfterReady != nil {
			proc.finish(*script.exitAfterReady)
			return
		}
		if script.blockAfterReady {
			<-proc.exitedCh
			return
		}
	}
	for {
		env, err := ipc.Read()
		if err != nil {
			proc.finish(0)
			return
		}
		switch env.Type {
		case MsgJob:
			job, err := DecodeBody[JobMsg](env)
			if err != nil {
				proc.finish(2)
				return
			}
			h.dispatch <- job
			if script.acquireOnJob {
				request := IOLeaseAcquireMsg{
					JobID: job.JobID, RequestID: 1,
					TaskID: job.ScanTaskID, InstanceID: job.ScanInstanceID, DiskKey: job.DiskKey,
					Class: 1, WantBytes: 1 << 20,
				}
				if err := ipc.Write(MsgIOLeaseAcquire, request); err != nil {
					proc.finish(2)
					return
				}
			}
			if script.gateFirstResult {
				<-h.releaseResult
				script.gateFirstResult = false
			}
			if script.eofBeforeExit {
				_ = conn.Close()
				h.eofBeforeExit <- struct{}{}
				<-h.releaseExit
				proc.finish(3)
				return
			}
			if script.queryOnJob {
				query := SHAQueryMsg{JobID: job.JobID, SHA512: make([]byte, 64), Kind: job.Kind}
				if script.queryOverride != nil {
					query = *script.queryOverride
				}
				if err := ipc.Write(MsgSHAQuery, query); err != nil {
					proc.finish(2)
					return
				}
				replyEnv, err := ipc.Read()
				if err != nil || replyEnv.Type != MsgSHAReply {
					proc.finish(2)
					return
				}
				reply, err := DecodeBody[SHAReplyMsg](replyEnv)
				if err == nil && script.replyObserved != nil {
					script.replyObserved <- reply
				}
				if err != nil || (script.requireQueryFound && !reply.Found) {
					proc.finish(2)
					return
				}
			}
			if script.exitOnJob != nil {
				proc.finish(*script.exitOnJob)
				return
			}
			if script.eofOnJob {
				proc.finish(0)
				return
			}
			if script.truncatedOnJob {
				var header [4]byte
				binary.BigEndian.PutUint32(header[:], 32)
				_, _ = conn.Write(append(header[:], 0x81))
				proc.finish(2)
				return
			}
			if !script.hangJob {
				result := JobResultMsg{JobID: job.JobID, Path: job.Path, Kind: job.Kind}
				if script.result != nil {
					result = *script.result
				}
				_ = ipc.Write(MsgResult, result)
				if script.exitAfterResult {
					proc.finish(0)
					return
				}
			}
		case MsgShutdown:
			h.shutdowns <- struct{}{}
			if script.exitOnShutdown {
				proc.finish(0)
				return
			}
		}
	}
}

func (h *lifecycleHarness) forceEOF() {
	h.connMu.Lock()
	children := append([]net.Conn(nil), h.children...)
	h.connMu.Unlock()
	for _, child := range children {
		_ = child.Close()
	}
}

type writeFailConn struct{ net.Conn }

func (c *writeFailConn) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func (h *lifecycleHarness) ready(t *testing.T) ReadyMsg {
	t.Helper()
	select {
	case ready := <-h.readyCh:
		return ready
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Ready")
		return ReadyMsg{}
	}
}

func (h *lifecycleHarness) dispatched(t *testing.T) JobMsg {
	t.Helper()
	select {
	case job := <-h.dispatch:
		return job
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dispatched job")
		return JobMsg{}
	}
}

func (h *lifecycleHarness) crash(t *testing.T) CrashRecord {
	t.Helper()
	select {
	case crash := <-h.crashes:
		return crash
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for crash record")
		return CrashRecord{}
	}
}

type manualClock struct {
	created chan *manualTimer
}

func newManualClock() *manualClock {
	return &manualClock{created: make(chan *manualTimer, 64)}
}

func (c *manualClock) NewTimer(d time.Duration) poolTimer {
	timer := &manualTimer{duration: d, ch: make(chan time.Time, 1)}
	c.created <- timer
	return timer
}

func (c *manualClock) AfterFunc(d time.Duration, fn func()) poolTimer {
	timer := &manualTimer{duration: d, ch: make(chan time.Time, 1), fn: fn}
	c.created <- timer
	return timer
}

func (c *manualClock) Now() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

func (c *manualClock) next(t *testing.T, want time.Duration) *manualTimer {
	t.Helper()
	for {
		select {
		case timer := <-c.created:
			if timer.stopped.Load() {
				continue
			}
			if timer.duration != want {
				if timer.duration == defaultExitGrace {
					deadline := time.Now().Add(2 * time.Second)
					for time.Now().Before(deadline) &&
						!timer.stopped.Load() {
						runtime.Gosched()
					}
					if timer.stopped.Load() {
						continue
					}
				}
				t.Fatalf("timer duration = %s, want %s", timer.duration, want)
			}
			return timer
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s timer", want)
			return nil
		}
	}
}

type manualTimer struct {
	duration time.Duration
	ch       chan time.Time
	fn       func()
	stopped  atomic.Bool
}

func (t *manualTimer) C() <-chan time.Time { return t.ch }
func (t *manualTimer) Stop() bool          { return t.stopped.CompareAndSwap(false, true) }
func (t *manualTimer) fire() {
	if t.stopped.Load() {
		return
	}
	if t.fn != nil {
		t.fn()
		return
	}
	t.ch <- time.Unix(1_700_000_001, 0)
}

type channelListener struct {
	accept chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newChannelListener() *channelListener {
	return &channelListener{accept: make(chan net.Conn, 1), closed: make(chan struct{})}
}

func (l *channelListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.accept:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *channelListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}
func (l *channelListener) Addr() net.Addr { return testAddr("named-pipe") }

type testAddr string

func (a testAddr) Network() string { return string(a) }
func (a testAddr) String() string  { return string(a) }

type fakeProcess struct {
	pid      int
	index    int
	kills    chan<- int
	reaps    chan<- int
	done     chan int32
	endOnce  sync.Once
	exitedCh chan struct{}
}

type processWait struct {
	code int32
	err  error
}

type observedProcess struct {
	managedProcess
	waited chan<- processWait
}

func (p *observedProcess) Wait() (int32, error) {
	code, err := p.managedProcess.Wait()
	p.waited <- processWait{code: code, err: err}
	return code, err
}

func newFakeProcess(pid, index int, kills, reaps chan<- int) *fakeProcess {
	return &fakeProcess{
		pid: pid, index: index, kills: kills, reaps: reaps,
		done: make(chan int32, 1), exitedCh: make(chan struct{}),
	}
}
func (p *fakeProcess) PID() int { return p.pid }
func (p *fakeProcess) Wait() (int32, error) {
	code := <-p.done
	p.reaps <- p.index
	return code, nil
}
func (p *fakeProcess) Kill() error {
	p.kills <- p.index
	p.finish(-1)
	return nil
}
func (p *fakeProcess) Close() error { return nil }
func (p *fakeProcess) finish(code int32) {
	p.endOnce.Do(func() {
		close(p.exitedCh)
		p.done <- code
	})
}

type poolTestStore struct {
	mu                 sync.Mutex
	crashes            []string
	image              *store.ImageFeature
	lookupErr          error
	saveErr            error
	phase2SaveErr      error
	markCrashErr       error
	blockSave          bool
	missingMask        uint32
	phase2MissingMask  uint32
	missingFrames      uint8
	saveStarted        chan struct{}
	saveOnce           sync.Once
	saveCount          int
	phase2SaveCount    int
	phase2MissingCalls int
	phase2Results      []store.Phase2Result
}

// Break caught: a preview result is sent through SaveAnalysis and persists
// thumbnail-like data instead of remaining an in-memory response.
func TestImagePreviewPoolBypassesFeatureStore(t *testing.T) {
	backend := &poolTestStore{}
	pool := &Pool{
		ctx: context.Background(), store: backend, dedup: NewDeduper(backend),
		results: make(chan *JobResultMsg, 1), quit: make(chan struct{}),
	}
	job := JobMsg{
		JobID: 702, Path: `D:\media\source.jpg`, Kind: MediaImage,
		Phase: PhasePreview, ScreenStage: ScreenStagePreview, Source: JobSourceLocal,
		KnownSHA: bytes64(0x72), PreviewFormat: PreviewFormatJPEG,
		PreviewMaxWidth: 100, PreviewMaxHeight: 100, PreviewQuality: 80,
	}
	pool.saveResult(job, JobResultMsg{
		JobID: job.JobID, Path: job.Path, Kind: job.Kind,
		SHA512: bytes64(0x72), PreviewFormat: PreviewFormatJPEG,
		PreviewWidth: 50, PreviewHeight: 40, PreviewBytes: []byte{1, 2, 3},
	})
	if got := backend.saveCountValue() + backend.phase2SaveCountValue(); got != 0 {
		t.Fatalf("preview persisted through feature store %d times", got)
	}
	select {
	case result := <-pool.results:
		if len(result.PreviewBytes) != 3 {
			t.Fatalf("published preview bytes = %d", len(result.PreviewBytes))
		}
	default:
		t.Fatal("preview result was not published")
	}
	if got := pool.Metrics(); got.FilesDone != 0 || got.FilesFailed != 0 {
		t.Fatalf("successful preview changed scan file metrics: %#v", got)
	}
	pool.saveResult(job, JobResultMsg{
		JobID: job.JobID, Path: job.Path, Kind: job.Kind,
		SHA512: bytes64(0x72), PreviewErrorCode: "preview_too_large",
	})
	<-pool.results
	if got := pool.Metrics(); got.FilesDone != 0 || got.FilesFailed != 0 {
		t.Fatalf("failed preview changed scan file metrics: %#v", got)
	}
}

func (s *poolTestStore) LookupContent(_ context.Context, _ []byte, kind store.MediaKind, requestedFields uint32, requestedFrames uint8) (store.ContentState, error) {
	if s.lookupErr != nil {
		return store.ContentState{}, s.lookupErr
	}
	state := store.ContentState{MissingFields: requestedFields, MissingFrames: requestedFrames}
	if requestedFields&MaskSHA512 != 0 {
		state.FieldsPresent |= MaskSHA512
		state.MissingFields &^= MaskSHA512
	}
	if kind == store.MediaImage && s.image != nil && requestedFields&MaskImagePDQ != 0 {
		state.Image = s.image
		state.FieldsPresent |= MaskImagePDQ
		state.MissingFields &^= MaskImagePDQ
	}
	return state, nil
}

func (s *poolTestStore) SaveAnalysis(ctx context.Context, result store.AnalysisResult) (store.CommittedState, error) {
	isPhase2 := result.RequestedFields&(MaskPHashParts|MaskSobelHist|videoSixFrameWorkerFields()) != 0 && result.RequestedFields&(MaskSHA512|MaskImagePDQ|MaskVideoThumb|MaskVideoDuration|MaskVideoContactSheet) == 0
	s.mu.Lock()
	if isPhase2 {
		s.phase2SaveCount++
		s.phase2Results = append(s.phase2Results, store.Phase2Result{MachineID: result.MachineID, Path: result.Path, Kind: result.Kind, SHA512: cloneBytes(result.SHA512), FieldsDone: result.FieldsDone, PHashParts: cloneBytes(result.PHashParts), SobelHist: cloneBytes(result.SobelHist), Frames: append([]store.Phase2Frame(nil), result.Frames...), Errors: append([]store.FieldError(nil), result.Errors...)})
	} else {
		s.saveCount++
	}
	s.mu.Unlock()
	if isPhase2 && s.phase2SaveErr != nil {
		if errors.Is(s.phase2SaveErr, store.ErrPhase2Stale) {
			return store.CommittedState{}, store.ErrStale
		}
		return store.CommittedState{}, s.phase2SaveErr
	}
	if !isPhase2 && s.saveErr != nil {
		return store.CommittedState{}, s.saveErr
	}
	if s.blockSave {
		s.saveOnce.Do(func() { close(s.saveStarted) })
		<-ctx.Done()
		return store.CommittedState{}, ctx.Err()
	}
	missing := s.missingMask
	if isPhase2 {
		missing = s.phase2MissingMask
	}
	return store.CommittedState{FieldsPresent: result.RequestedFields &^ missing, MissingFields: missing, FramesPresent: result.RequestedFrames &^ s.missingFrames, MissingFrames: s.missingFrames}, nil
}

func (s *poolTestStore) Phase1MissingMask(context.Context, string, string) (uint32, error) {
	return s.missingMask, nil
}

func (s *poolTestStore) LookupImage(context.Context, []byte) (*store.ImageFeature, error) {
	return s.image, s.lookupErr
}
func (s *poolTestStore) LookupVideo(context.Context, []byte) (*store.VideoFeature, error) {
	return nil, nil
}
func (s *poolTestStore) SavePhase1(ctx context.Context, _ store.Phase1Result) error {
	s.mu.Lock()
	s.saveCount++
	s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	if !s.blockSave {
		return nil
	}
	s.saveOnce.Do(func() { close(s.saveStarted) })
	<-ctx.Done()
	return ctx.Err()
}
func (s *poolTestStore) SavePhase2(_ context.Context, result store.Phase2Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phase2SaveCount++
	s.phase2Results = append(s.phase2Results, result)
	return s.phase2SaveErr
}
func (s *poolTestStore) Phase2MissingMask(context.Context, string, string) (uint32, error) {
	s.mu.Lock()
	s.phase2MissingCalls++
	s.mu.Unlock()
	return s.phase2MissingMask, nil
}
func (s *poolTestStore) saveCountValue() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveCount
}
func (s *poolTestStore) phase2SaveCountValue() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase2SaveCount
}
func (s *poolTestStore) phase2MissingMaskCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase2MissingCalls
}
func (s *poolTestStore) lastPhase2Result() store.Phase2Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.phase2Results) == 0 {
		return store.Phase2Result{}
	}
	return s.phase2Results[len(s.phase2Results)-1]
}
func (s *poolTestStore) MarkCrash(_ context.Context, _, path, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.crashes = append(s.crashes, path)
	return s.markCrashErr
}
func (s *poolTestStore) crashCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.crashes)
}

func TestPoolHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	pipeName := os.Getenv("GO_HELPER_PIPE")
	index, err := strconv.Atoi(os.Getenv("GO_HELPER_INDEX"))
	if err != nil {
		os.Exit(10)
	}
	timeout := 5 * time.Second
	conn, err := winio.DialPipe(pipeName, &timeout)
	if err != nil {
		os.Exit(11)
	}
	ipc := NewIPCConn(conn)
	ready := validReadyForTest()
	ready.PID, ready.WorkerIndex = os.Getpid(), index
	if err := ipc.Write(MsgReady, ready); err != nil {
		os.Exit(12)
	}
	for {
		env, readErr := ipc.Read()
		if readErr != nil {
			os.Exit(13)
		}
		switch env.Type {
		case MsgJob:
			job, decodeErr := DecodeBody[JobMsg](env)
			if decodeErr != nil {
				os.Exit(14)
			}
			if writeErr := ipc.Write(MsgResult, JobResultMsg{
				JobID: job.JobID, Path: job.Path, Kind: job.Kind, FieldsDone: job.FieldsMask,
			}); writeErr != nil {
				os.Exit(15)
			}
		case MsgShutdown:
			_ = conn.Close()
			os.Exit(0)
		default:
			os.Exit(16)
		}
	}
}
