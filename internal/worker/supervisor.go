//go:build windows

package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"dedup/internal/diskio"
	"dedup/internal/features"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

type poolTimer interface {
	C() <-chan time.Time
	Stop() bool
}

type poolClock interface {
	NewTimer(time.Duration) poolTimer
	AfterFunc(time.Duration, func()) poolTimer
	Now() time.Time
}

type realClock struct{}

func (realClock) NewTimer(duration time.Duration) poolTimer {
	return &realTimer{Timer: time.NewTimer(duration)}
}
func (realClock) AfterFunc(duration time.Duration, fn func()) poolTimer {
	return &realTimer{Timer: time.AfterFunc(duration, fn)}
}
func (realClock) Now() time.Time { return time.Now().UTC() }

type realTimer struct{ *time.Timer }

func (timer *realTimer) C() <-chan time.Time { return timer.Timer.C }

type managedProcess interface {
	PID() int
	Wait() (int32, error)
	Kill() error
	Close() error
}

type supervisorDeps struct {
	clock               poolClock
	pipeName            func(int) string
	listen              func(string) (net.Listener, error)
	launch              func(Config, string, int) (managedProcess, error)
	crash               func(CrashRecord)
	ready               func(ReadyMsg)
	beforeRegister      func()
	beforeWatchdogClaim func()
	beforeFailureCommit func(string)
	beforeClaimAttempt  func(string)
	logger              *slog.Logger
	errorLogger         *slog.Logger
}

func defaultSupervisorDeps() supervisorDeps {
	return supervisorDeps{
		clock: realClock{},
		pipeName: func(index int) string {
			return fmt.Sprintf(`\\.\pipe\dedup-worker-%d-%d-%d`, os.Getpid(), index, time.Now().UnixNano())
		},
		listen: func(name string) (net.Listener, error) {
			return winio.ListenPipe(name, nil)
		},
		launch:      launchWorkerProcess,
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		errorLogger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

type execManagedProcess struct {
	command *exec.Cmd
	job     windows.Handle

	handleMu  sync.Mutex
	killOnce  sync.Once
	killErr   error
	waitOnce  sync.Once
	waitCode  int32
	waitErr   error
	closeOnce sync.Once
	closeErr  error
}

func launchWorkerProcess(cfg Config, pipeName string, index int) (managedProcess, error) {
	if strings.TrimSpace(cfg.WorkerExe) == "" {
		return nil, fmt.Errorf("worker executable is required")
	}
	job, err := createWorkerJob()
	if err != nil {
		return nil, fmt.Errorf("create worker job object: %w", err)
	}
	command := exec.Command(cfg.WorkerExe,
		"--pipe="+pipeName,
		fmt.Sprintf("--worker-index=%d", index),
	)
	command.Env = append(os.Environ(), cfg.WorkerEnv...)
	if err := command.Start(); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	process := &execManagedProcess{command: command, job: job}
	processHandle, handleErr := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(command.Process.Pid),
	)
	var assignErr error
	if handleErr == nil {
		assignErr = windows.AssignProcessToJobObject(job, processHandle)
		_ = windows.CloseHandle(processHandle)
	}
	if handleErr != nil || assignErr != nil {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
		_ = process.Close()
		if handleErr != nil {
			return nil, fmt.Errorf("access worker process handle for job assignment: %w", handleErr)
		}
		return nil, fmt.Errorf("assign worker process to job object: %w", assignErr)
	}
	return process, nil
}

func (process *execManagedProcess) PID() int { return process.command.Process.Pid }
func (process *execManagedProcess) Kill() error {
	process.killOnce.Do(func() {
		process.handleMu.Lock()
		defer process.handleMu.Unlock()
		if process.job != 0 {
			process.killErr = windows.TerminateJobObject(process.job, 1)
		}
	})
	return process.killErr
}
func (process *execManagedProcess) Wait() (int32, error) {
	process.waitOnce.Do(func() {
		process.waitErr = process.command.Wait()
		if process.command.ProcessState == nil {
			process.waitCode = -1
		} else {
			process.waitCode = int32(process.command.ProcessState.ExitCode())
		}
		if terminateErr := process.Kill(); terminateErr != nil && process.waitErr == nil {
			process.waitErr = terminateErr
		}
		process.handleMu.Lock()
		job := process.job
		if job != 0 {
			status, waitErr := windows.WaitForSingleObject(job, windows.INFINITE)
			if waitErr != nil && process.waitErr == nil {
				process.waitErr = waitErr
			} else if waitErr == nil && status != uint32(windows.WAIT_OBJECT_0) && process.waitErr == nil {
				process.waitErr = fmt.Errorf("wait for worker job object returned status %#x", status)
			}
		}
		process.handleMu.Unlock()
	})
	return process.waitCode, process.waitErr
}

func (process *execManagedProcess) Close() error {
	process.closeOnce.Do(func() {
		process.handleMu.Lock()
		defer process.handleMu.Unlock()
		if process.job != 0 {
			process.closeErr = windows.CloseHandle(process.job)
			process.job = 0
		}
	})
	return process.closeErr
}

func createWorkerJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

type workerProc struct {
	pool  *Pool
	index int
	proc  managedProcess
	conn  net.Conn
	ipc   *IPCConn

	mu             sync.Mutex
	current        *activeJob
	nextGeneration uint64
	ready          bool
	readyMsg       ReadyMsg
	ioLeases       map[uint64]activeIOLease
	ioAcquireWG    sync.WaitGroup
	failureClaimed atomic.Bool
	killOnce       sync.Once
	done           chan struct{}
}

type activeJob struct {
	message    *JobMsg
	generation uint64
	terminal   bool
	timer      poolTimer
	ctx        context.Context
	cancel     context.CancelFunc
}

type leaseJobIdentity struct {
	jobID      int64
	taskID     string
	instanceID string
	diskKey    string
}

type activeIOLease struct {
	run      *activeJob
	identity leaseJobIdentity
	grant    IOLeaseGrantMsg
}

type workerOutcome struct {
	reason   string
	exitCode int32
	err      error
	protocol bool
}

func (p *Pool) supervise(index int) {
	defer p.wg.Done()
	for !p.closing.Load() {
		worker, err := p.launchAttempt(index)
		if err != nil {
			if p.closing.Load() {
				return
			}
			p.deps.logger.Error("worker start failed", "worker_index", index, "err", err)
			if !p.waitRespawn() {
				return
			}
			continue
		}
		if p.deps.beforeRegister != nil {
			p.deps.beforeRegister()
		}
		if !p.register(worker) {
			worker.abort()
			return
		}
		outcome := worker.run()
		if p.closing.Load() {
			p.unregister(worker)
			return
		}
		worker.classify(outcome)
		p.unregister(worker)
		if !p.waitRespawn() {
			return
		}
	}
}

func (p *Pool) launchAttempt(index int) (*workerProc, error) {
	name := p.deps.pipeName(index)
	listener, err := p.deps.listen(name)
	if err != nil {
		return nil, err
	}
	defer listener.Close()
	process, err := p.deps.launch(p.cfg, name, index)
	if err != nil {
		return nil, err
	}
	worker := &workerProc{pool: p, index: index, proc: process, done: make(chan struct{})}
	accept := make(chan struct {
		conn net.Conn
		err  error
	}, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		accept <- struct {
			conn net.Conn
			err  error
		}{conn: conn, err: acceptErr}
	}()

	timer := p.deps.clock.NewTimer(p.cfg.ReadyTimeout)
	defer timer.Stop()
	var conn net.Conn
	select {
	case accepted := <-accept:
		if accepted.err != nil {
			worker.abort()
			return nil, accepted.err
		}
		conn = accepted.conn
	case <-timer.C():
		worker.abort()
		return nil, fmt.Errorf("worker %d Ready timeout", index)
	case <-p.quit:
		worker.abort()
		return nil, ErrPoolClosed
	}
	worker.conn = conn
	worker.ipc = NewIPCConnWithMax(conn, p.cfg.IPCMaxFrameBytes)
	read := make(chan struct {
		env *Envelope
		err error
	}, 1)
	go func() {
		env, readErr := worker.ipc.Read()
		read <- struct {
			env *Envelope
			err error
		}{env: env, err: readErr}
	}()
	select {
	case first := <-read:
		if first.err != nil {
			conn.Close()
			worker.abort()
			return nil, first.err
		}
		if first.env.Type != MsgReady {
			conn.Close()
			worker.abort()
			return nil, fmt.Errorf("worker %d first message %q, want Ready", index, first.env.Type)
		}
		ready, decodeErr := DecodeBody[ReadyMsg](first.env)
		if decodeErr == nil {
			decodeErr = validateReady(ready, index, process.PID())
		}
		if decodeErr != nil {
			conn.Close()
			worker.abort()
			return nil, fmt.Errorf("worker %d incompatible Ready: %#v: %v", index, ready, decodeErr)
		}
		worker.ready = true
		worker.readyMsg = ready
		p.deps.logger.Info("worker ready", "worker_index", index, "pid", ready.PID, "dll_version", ready.DLLVersion)
		return worker, nil
	case <-timer.C():
		conn.Close()
		worker.abort()
		return nil, fmt.Errorf("worker %d Ready timeout", index)
	case <-p.quit:
		conn.Close()
		worker.abort()
		return nil, ErrPoolClosed
	}
}

func validateReady(ready ReadyMsg, index, pid int) error {
	if ready.WorkerIndex != index || ready.PID != pid {
		return fmt.Errorf("worker identity mismatch")
	}
	if ready.IPCVersion != IPCCompatibilityVersion || ready.DLLVersion != MediaCoreDLLVersion {
		return fmt.Errorf("IPC or DLL version mismatch")
	}
	if ready.VideoCoreABI != VideoCoreABIVersion || ready.VideoCoreVersion != VideoCoreVersion {
		return fmt.Errorf("VideoCore ABI or version mismatch")
	}
	if len(ready.FFmpegComponents) != 4 {
		return fmt.Errorf("FFmpeg component count %d, want 4", len(ready.FFmpegComponents))
	}
	wanted := map[string]bool{"avformat": false, "avcodec": false, "avutil": false, "swscale": false}
	for _, component := range ready.FFmpegComponents {
		seen, ok := wanted[component.Name]
		if !ok {
			return fmt.Errorf("unknown FFmpeg component %q", component.Name)
		}
		if seen {
			return fmt.Errorf("duplicate FFmpeg component %q", component.Name)
		}
		if component.BuildVersion == "" || component.RuntimeVersion == "" ||
			component.BuildMajor == 0 || component.RuntimeMajor == 0 ||
			component.BuildMajor != component.RuntimeMajor {
			return fmt.Errorf("FFmpeg component %q build/runtime mismatch", component.Name)
		}
		wanted[component.Name] = true
	}
	return nil
}

func (p *Pool) register(worker *workerProc) bool {
	p.activeMu.Lock()
	if p.closing.Load() {
		p.activeMu.Unlock()
		return false
	}
	p.active[worker.index] = worker
	p.activeMu.Unlock()
	p.metrics.readyWorkers.Add(1)
	offered := p.offerFree(worker)
	if offered && p.deps.ready != nil {
		p.deps.ready(worker.readyMsg)
	}
	return true
}

func (p *Pool) unregister(worker *workerProc) {
	p.removeFree(worker)
	p.activeMu.Lock()
	if p.active[worker.index] == worker {
		delete(p.active, worker.index)
	}
	p.activeMu.Unlock()
	if worker.ready {
		p.metrics.readyWorkers.Add(-1)
	}
	worker.stopWatchdog()
	worker.cancelCurrentJob()
	_ = worker.conn.Close()
	if p.cfg.IOBroker != nil {
		p.cfg.IOBroker.ReclaimWorker(worker.index)
	}
	worker.ioAcquireWG.Wait()
}

func (p *Pool) offerFree(worker *workerProc) bool {
	p.freeMu.Lock()
	defer p.freeMu.Unlock()
	select {
	case p.free <- worker:
		return true
	case <-p.quit:
		return false
	}
}

func (p *Pool) removeFree(target *workerProc) {
	p.freeMu.Lock()
	defer p.freeMu.Unlock()
	kept := make([]*workerProc, 0, cap(p.free))
	for {
		select {
		case worker := <-p.free:
			if worker != target {
				kept = append(kept, worker)
			}
		default:
			for _, worker := range kept {
				p.free <- worker
			}
			return
		}
	}
}

func (p *Pool) waitRespawn() bool {
	timer := p.deps.clock.NewTimer(p.cfg.RespawnDelay)
	defer timer.Stop()
	select {
	case <-timer.C():
		return !p.closing.Load()
	case <-p.quit:
		return false
	}
}

func (worker *workerProc) run() workerOutcome {
	exit := make(chan workerOutcome, 1)
	go func() {
		code, err := worker.proc.Wait()
		closeErr := worker.proc.Close()
		if err == nil {
			err = closeErr
		}
		exit <- workerOutcome{reason: "exit_code", exitCode: code, err: err}
	}()
	read := make(chan workerOutcome, 1)
	go worker.readLoop(read)
	select {
	case outcome := <-exit:
		_ = worker.conn.Close()
		readOutcome := <-read
		if readOutcome.protocol {
			readOutcome.exitCode = outcome.exitCode
			return readOutcome
		}
		if outcome.exitCode != 0 || worker.pool.closing.Load() {
			return outcome
		}
		readOutcome.exitCode = 0
		return readOutcome
	case outcome := <-read:
		if worker.pool.closing.Load() {
			return <-exit
		}
		grace := worker.pool.deps.clock.NewTimer(worker.pool.cfg.ExitGrace)
		defer grace.Stop()
		select {
		case exitOutcome := <-exit:
			if exitOutcome.exitCode != 0 && !outcome.protocol {
				exitOutcome.reason = "exit_code"
				return exitOutcome
			}
			outcome.exitCode = exitOutcome.exitCode
			return outcome
		case <-grace.C():
		}
		worker.kill()
		exitOutcome := <-exit
		outcome.exitCode = exitOutcome.exitCode
		return outcome
	case <-worker.done:
		worker.kill()
		return <-exit
	}
}

func (worker *workerProc) readLoop(out chan<- workerOutcome) {
	for {
		env, err := worker.ipc.Read()
		if err != nil {
			reason := "pipe_eof"
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				reason = "pipe_eof"
			}
			out <- workerOutcome{reason: reason, err: err}
			return
		}
		switch env.Type {
		case MsgResult:
			result, decodeErr := DecodeBody[JobResultMsg](env)
			if decodeErr != nil {
				worker.failProtocol(out, decodeErr)
				return
			}
			job, validationErr := worker.claimResult(&result)
			if validationErr != nil {
				worker.failProtocol(out, validationErr)
				return
			}
			if job == nil {
				continue
			}
			result.Phase = job.Phase
			result.ScreenStage = job.ScreenStage
			result.Source = job.Source
			result.ScanTaskID = job.ScanTaskID
			result.WorkerPID = worker.proc.PID()
			worker.pool.saveResult(*job, result)
			if worker.failureClaimed.Load() {
				continue
			}
			if !worker.pool.offerFree(worker) {
				out <- workerOutcome{reason: "pipe_eof", err: ErrPoolClosed}
				return
			}
		case MsgSHAQuery:
			query, decodeErr := DecodeBody[SHAQueryMsg](env)
			if decodeErr != nil {
				worker.failProtocol(out, decodeErr)
				return
			}
			if validationErr := worker.validateQuery(&query); validationErr != nil {
				worker.failProtocol(out, validationErr)
				return
			}
			reply, askErr := worker.pool.dedup.Ask(worker.pool.ctx, query)
			if askErr != nil {
				worker.pool.deps.logger.Error("feature lookup failed; worker will compute",
					"job_id", query.JobID, "err", askErr)
				reply = missingReply(query)
			}
			if reply.Found && reply.ReusedFlight {
				worker.pool.metrics.singleFlightHits.Add(1)
			}
			if writeErr := worker.ipc.Write(MsgSHAReply, reply); writeErr != nil {
				out <- workerOutcome{reason: "pipe_write", err: writeErr}
				return
			}
		case MsgIOLeaseAcquire:
			request, decodeErr := DecodeBody[IOLeaseAcquireMsg](env)
			if decodeErr != nil {
				worker.failProtocol(out, decodeErr)
				return
			}
			run, identity, requestContext, brokerRequest, validationErr := worker.validateLeaseAcquire(request)
			if validationErr != nil {
				worker.failProtocol(out, validationErr)
				return
			}
			worker.ioAcquireWG.Add(1)
			go func() {
				defer worker.ioAcquireWG.Done()
				worker.acquireIOLease(run, identity, requestContext, request, brokerRequest)
			}()
		case MsgIOLeaseReport:
			report, decodeErr := DecodeBody[IOLeaseReportMsg](env)
			if decodeErr != nil {
				worker.failProtocol(out, decodeErr)
				return
			}
			if validationErr := worker.reportIOLease(report); validationErr != nil {
				worker.failProtocol(out, validationErr)
				return
			}
		default:
			worker.failProtocol(out, fmt.Errorf("unexpected worker message %q", env.Type))
			return
		}
	}
}

func (worker *workerProc) validateLeaseAcquire(request IOLeaseAcquireMsg) (*activeJob, leaseJobIdentity, context.Context, diskio.Request, error) {
	if err := request.Validate(); err != nil {
		return nil, leaseJobIdentity{}, nil, diskio.Request{}, err
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	run := worker.current
	if run == nil || run.terminal || worker.failureClaimed.Load() || run.message == nil {
		return nil, leaseJobIdentity{}, nil, diskio.Request{}, fmt.Errorf("worker %d requested I/O lease without an active job", worker.index)
	}
	identity := leaseIdentityOf(run.message)
	if request.JobID != identity.jobID {
		return nil, leaseJobIdentity{}, nil, diskio.Request{}, fmt.Errorf("worker %d I/O lease job identity mismatch", worker.index)
	}
	if identity.taskID == "" || identity.instanceID == "" || identity.diskKey == "" {
		return nil, leaseJobIdentity{}, nil, diskio.Request{}, fmt.Errorf("worker %d current job has incomplete I/O identity", worker.index)
	}
	requestContext := run.ctx
	if requestContext == nil {
		requestContext = worker.pool.ctx
	}
	if requestContext == nil {
		requestContext = context.Background()
	}
	return run, identity, requestContext, diskio.Request{
		RequestID: request.RequestID,
		TaskID:    identity.taskID, InstanceID: identity.instanceID,
		WorkerID: worker.index, Disk: diskio.DiskKey(identity.diskKey),
		Class: diskio.SourceClass(request.Class), WantBytes: request.WantBytes, WantSeek: request.WantSeek,
	}, nil
}

func (worker *workerProc) acquireIOLease(
	run *activeJob,
	identity leaseJobIdentity,
	requestContext context.Context,
	request IOLeaseAcquireMsg,
	brokerRequest diskio.Request,
) {
	broker := worker.pool.cfg.IOBroker
	if broker == nil {
		worker.writeLeaseCancelIfCurrent(run, identity, request)
		return
	}
	grant, err := broker.Acquire(requestContext, brokerRequest)
	if err != nil {
		worker.writeLeaseCancelIfCurrent(run, identity, request)
		return
	}
	wireGrant := IOLeaseGrantMsg{
		JobID: identity.jobID, RequestID: request.RequestID,
		LeaseID: grant.LeaseID, Generation: grant.Generation,
		Bytes: grant.Bytes, Seeks: grant.Seeks,
	}
	if err := wireGrant.ValidateFor(request); err != nil {
		worker.reclaimIOGrant(identity, grant)
		worker.writeLeaseCancelIfCurrent(run, identity, request)
		return
	}

	worker.mu.Lock()
	if worker.current != run || run.terminal || worker.failureClaimed.Load() ||
		run.message == nil || leaseIdentityOf(run.message) != identity {
		worker.mu.Unlock()
		worker.reclaimIOGrant(identity, grant)
		return
	}
	if worker.ioLeases == nil {
		worker.ioLeases = make(map[uint64]activeIOLease)
	}
	if _, exists := worker.ioLeases[grant.LeaseID]; exists {
		worker.mu.Unlock()
		worker.reclaimIOGrant(identity, grant)
		_ = worker.conn.Close()
		return
	}
	worker.ioLeases[grant.LeaseID] = activeIOLease{
		run: run, identity: identity, grant: wireGrant,
	}
	writeErr := worker.ipc.Write(MsgIOLeaseGrant, wireGrant)
	worker.mu.Unlock()
	if writeErr != nil {
		_ = worker.conn.Close()
	}
}

func (worker *workerProc) writeLeaseCancelIfCurrent(run *activeJob, identity leaseJobIdentity, request IOLeaseAcquireMsg) {
	cancel := IOLeaseCancelMsg{JobID: identity.jobID, RequestID: request.RequestID}
	worker.mu.Lock()
	if worker.current != run || run.terminal || worker.failureClaimed.Load() ||
		run.message == nil || leaseIdentityOf(run.message) != identity {
		worker.mu.Unlock()
		return
	}
	writeErr := worker.ipc.Write(MsgIOLeaseCancel, cancel)
	worker.mu.Unlock()
	if writeErr != nil {
		_ = worker.conn.Close()
	}
}

func (worker *workerProc) reportIOLease(message IOLeaseReportMsg) error {
	broker := worker.pool.cfg.IOBroker
	if broker == nil {
		return fmt.Errorf("worker %d reported I/O lease without a broker", worker.index)
	}
	worker.mu.Lock()
	lease, exists := worker.ioLeases[message.LeaseID]
	if !exists {
		worker.mu.Unlock()
		return fmt.Errorf("worker %d reported unknown I/O lease %d", worker.index, message.LeaseID)
	}
	if err := message.ValidateFor(lease.grant); err != nil {
		worker.mu.Unlock()
		return err
	}
	delete(worker.ioLeases, message.LeaseID)
	current := worker.current
	fresh := current == lease.run && current != nil && !current.terminal &&
		current.message != nil && leaseIdentityOf(current.message) == lease.identity
	trusted := leaseJobIdentity{}
	if current != nil && current.message != nil {
		trusted = leaseIdentityOf(current.message)
	}
	worker.mu.Unlock()

	report := diskio.Report{
		LeaseID: message.LeaseID, Generation: message.Generation,
		TaskID: trusted.taskID, InstanceID: trusted.instanceID,
		WorkerID: worker.index, Disk: diskio.DiskKey(trusted.diskKey),
		Bytes: message.Bytes, Seeks: message.Seeks,
		ReadTime: time.Duration(message.ReadNS), WaitTime: time.Duration(message.WaitNS),
		Completed: message.Completed, Cancelled: message.Cancelled,
	}
	if !fresh {
		report.Completed = false
		report.Cancelled = true
	}
	broker.Report(report)
	return nil
}

func (worker *workerProc) reclaimIOGrant(identity leaseJobIdentity, grant diskio.Grant) {
	worker.pool.cfg.IOBroker.Report(diskio.Report{
		LeaseID: grant.LeaseID, Generation: grant.Generation,
		TaskID: identity.taskID, InstanceID: identity.instanceID,
		WorkerID: worker.index, Disk: diskio.DiskKey(identity.diskKey),
		Cancelled: true,
	})
}

func leaseIdentityOf(job *JobMsg) leaseJobIdentity {
	if job == nil {
		return leaseJobIdentity{}
	}
	return leaseJobIdentity{
		jobID: job.JobID, taskID: job.ScanTaskID,
		instanceID: job.ScanInstanceID, diskKey: job.DiskKey,
	}
}

func (worker *workerProc) failProtocol(out chan<- workerOutcome, err error) {
	// A protocol violation is terminal. Closing our end immediately tells the
	// child to stop instead of leaving it blocked until ExitGrace expires.
	_ = worker.conn.Close()
	out <- workerOutcome{reason: "pipe_eof", err: err, protocol: true}
}

func (worker *workerProc) assign(job *JobMsg, timeout time.Duration) bool {
	worker.mu.Lock()
	if worker.current != nil || worker.failureClaimed.Load() {
		worker.mu.Unlock()
		return false
	}
	worker.nextGeneration++
	jobContext, cancelJob := context.WithCancel(worker.pool.ctx)
	run := &activeJob{message: job, generation: worker.nextGeneration, ctx: jobContext, cancel: cancelJob}
	worker.current = run
	run.timer = worker.pool.deps.clock.AfterFunc(timeout, func() {
		if worker.pool.deps.beforeWatchdogClaim != nil {
			worker.pool.deps.beforeWatchdogClaim()
		}
		reason := "watchdog_image"
		if job.Kind == MediaVideo {
			reason = "watchdog_video"
		}
		if worker.claim(reason, -1, nil, run) {
			worker.kill()
		}
	})
	worker.mu.Unlock()
	if err := worker.ipc.Write(MsgJob, job); err != nil {
		if worker.claim("pipe_write", -1, err, run) {
			worker.kill()
		}
		// The job is terminal for this scan once the active worker owns and
		// classifies the pipe-write crash. Only a stale/busy free-list entry
		// returns false and is eligible for redispatch.
		return true
	}
	return true
}

func (worker *workerProc) classify(outcome workerOutcome) {
	if worker.pool.closing.Load() {
		return
	}
	if worker.claim(outcome.reason, outcome.exitCode, outcome.err, nil) && outcome.reason != "exit_code" {
		worker.kill()
	}
}

func (worker *workerProc) claim(reason string, exitCode int32, cause error, owned *activeJob) bool {
	if worker.pool.deps.beforeClaimAttempt != nil {
		worker.pool.deps.beforeClaimAttempt(reason)
	}
	if worker.pool.closing.Load() {
		return false
	}
	worker.mu.Lock()
	if worker.pool.closing.Load() || worker.failureClaimed.Load() {
		worker.mu.Unlock()
		return false
	}
	run := worker.current
	if owned != nil && run != owned {
		worker.mu.Unlock()
		return false
	}
	if run != nil && run.terminal {
		worker.mu.Unlock()
		return false
	}
	if worker.pool.deps.beforeFailureCommit != nil {
		worker.pool.deps.beforeFailureCommit(reason)
	}
	if run != nil {
		run.terminal = true
		worker.current = nil
	}
	worker.failureClaimed.Store(true)
	worker.mu.Unlock()
	if run != nil && run.timer != nil {
		run.timer.Stop()
	}
	if run != nil && run.cancel != nil {
		run.cancel()
	}
	var job *JobMsg
	if run != nil {
		job = run.message
	}
	file := ""
	if job != nil {
		file = job.Path
	}
	record := CrashRecord{
		Timestamp: worker.pool.deps.clock.Now(), PID: worker.proc.PID(),
		WorkerIndex: worker.index, File: file, ExitCode: exitCode, Reason: reason,
	}
	if job != nil {
		record.JobID = job.JobID
		record.ScanTaskID = job.ScanTaskID
	}
	worker.pool.metrics.crashes.Add(1)
	if job != nil && job.Phase != PhasePreview {
		worker.pool.metrics.filesFailed.Add(1)
	}
	if job != nil && job.Phase != PhasePreview && worker.pool.store != nil {
		message := reason
		if cause != nil {
			message += ": " + cause.Error()
		}
		if err := worker.pool.store.MarkCrash(worker.pool.ctx, worker.pool.cfg.MachineID, job.Path, message); err != nil {
			worker.pool.deps.logger.Error("mark crash failed",
				"worker_index", worker.index,
				"pid", worker.proc.PID(),
				"path_id", PathID(job.Path),
				"screen_stage", job.ScreenStage,
				"source", job.Source,
				"reason", reason,
				"err", RedactKnownPath(err.Error(), job.Path),
			)
		}
	}
	if job != nil && job.Phase != PhasePreview {
		worker.pool.dedup.FailByJob(job.JobID)
	}
	if worker.pool.deps.crash != nil {
		worker.pool.deps.crash(record)
	}
	worker.pool.publishCrash(record)
	return true
}

func (worker *workerProc) claimResult(result *JobResultMsg) (*JobMsg, error) {
	worker.mu.Lock()
	run := worker.current
	if run == nil || run.terminal || worker.failureClaimed.Load() {
		worker.mu.Unlock()
		return nil, nil
	}
	if err := validateWorkerResult(run.message, result); err != nil {
		worker.mu.Unlock()
		return nil, err
	}
	run.terminal = true
	worker.current = nil
	worker.mu.Unlock()
	if run.timer != nil {
		run.timer.Stop()
	}
	if run.cancel != nil {
		run.cancel()
	}
	return run.message, nil
}

func (worker *workerProc) cancelCurrentJob() {
	worker.mu.Lock()
	run := worker.current
	worker.mu.Unlock()
	if run != nil && run.cancel != nil {
		run.cancel()
	}
}

func (worker *workerProc) validateQuery(query *SHAQueryMsg) error {
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if worker.current == nil ||
		worker.current.terminal ||
		worker.failureClaimed.Load() {
		return fmt.Errorf("worker %d sent SHA query without an active job", worker.index)
	}
	if err := validateSHAQuery(worker.current.message, query); err != nil {
		return err
	}
	query.ScanTaskID = worker.current.message.ScanTaskID
	return nil
}

func validateSHAQuery(job *JobMsg, query *SHAQueryMsg) error {
	if job == nil || query == nil {
		return fmt.Errorf("worker protocol: nil job or SHA query")
	}
	if job.Phase != Phase1 && job.Phase != Phase2 {
		return fmt.Errorf("worker protocol: invalid job phase %d", job.Phase)
	}
	if query.JobID != job.JobID || query.Kind != job.Kind {
		return fmt.Errorf(
			"worker protocol: SHA query identity mismatch job=%d/%d kind=%d/%d",
			query.JobID,
			job.JobID,
			query.Kind,
			job.Kind,
		)
	}
	if len(query.SHA512) != 64 {
		return fmt.Errorf(
			"worker protocol: SHA query must contain 64 bytes, got %d",
			len(query.SHA512),
		)
	}
	if job.Phase == Phase2 && len(job.KnownSHA) != 64 {
		return fmt.Errorf("worker protocol: phase-2 job known SHA-512 length %d", len(job.KnownSHA))
	}
	if len(job.KnownSHA) != 0 && !bytes.Equal(job.KnownSHA, query.SHA512) {
		return fmt.Errorf("worker protocol: SHA query does not match known job SHA")
	}
	return nil
}

func validateWorkerResult(job *JobMsg, result *JobResultMsg) error {
	if job == nil || result == nil {
		return fmt.Errorf("worker protocol: nil job or result")
	}
	if result.JobID != job.JobID ||
		result.Path != job.Path ||
		result.Kind != job.Kind {
		return fmt.Errorf(
			"worker protocol: result identity mismatch job=%d/%d result_path_id=%s job_path_id=%s kind=%d/%d",
			result.JobID,
			job.JobID,
			PathID(result.Path),
			PathID(job.Path),
			result.Kind,
			job.Kind,
		)
	}
	if result.ScanTaskID != "" && result.ScanTaskID != job.ScanTaskID {
		return fmt.Errorf("worker protocol: result scan_task_id mismatch")
	}
	implicitPhaseOneSource := job.Phase == Phase1 && result.ScreenStage == ScreenStageLegacy && result.Source == ""
	if !implicitPhaseOneSource && result.ScreenStage != job.ScreenStage {
		return fmt.Errorf("worker protocol: result screen_stage mismatch")
	}
	if !implicitPhaseOneSource && result.Source != job.Source {
		return fmt.Errorf("worker protocol: result source mismatch")
	}
	if result.FieldsDone&^job.FieldsMask != 0 {
		return fmt.Errorf(
			"worker protocol: result fields %#x exceed job mask %#x",
			result.FieldsDone,
			job.FieldsMask,
		)
	}
	if job.Phase != Phase1 && job.Phase != Phase2 && job.Phase != PhasePreview {
		return fmt.Errorf("worker protocol: invalid job phase %d", job.Phase)
	}
	if job.Phase == PhasePreview {
		return validateImagePreviewResult(job, result)
	}
	return validateMergedWorkerResult(job, result)
}

func validateImagePreviewResult(job *JobMsg, result *JobResultMsg) error {
	if job.Kind != MediaImage || job.Source != JobSourceLocal ||
		job.ScreenStage != ScreenStagePreview || job.FieldsMask != 0 ||
		job.FrameMask != 0 || job.Size < 0 || job.MTimeUnix <= 0 ||
		len(job.KnownSHA) != 64 || job.PreviewMaxWidth <= 0 ||
		job.PreviewMaxHeight <= 0 || job.PreviewQuality < 1 ||
		job.PreviewQuality > 100 || !validPreviewFormat(job.PreviewFormat) {
		return fmt.Errorf("worker protocol: invalid image preview request")
	}
	if len(result.SHA512) != 64 || !bytes.Equal(job.KnownSHA, result.SHA512) {
		return fmt.Errorf("worker protocol: image preview SHA-512 mismatch")
	}
	if result.FieldsDone != 0 || result.FramesDone != 0 || len(result.Errors) != 0 ||
		len(result.PDQ) != 0 || len(result.PHashParts) != 0 ||
		len(result.SobelHist) != 0 || len(result.Frames) != 0 {
		return fmt.Errorf("worker protocol: image preview carried feature payload")
	}
	if result.PreviewErrorCode != "" {
		if !validPreviewErrorCode(result.PreviewErrorCode) ||
			result.PreviewFormat != "" || result.PreviewWidth != 0 ||
			result.PreviewHeight != 0 || len(result.PreviewBytes) != 0 {
			return fmt.Errorf("worker protocol: invalid image preview failure")
		}
		return nil
	}
	if result.PreviewFormat != job.PreviewFormat || result.PreviewWidth <= 0 ||
		result.PreviewHeight <= 0 || result.PreviewWidth > job.PreviewMaxWidth ||
		result.PreviewHeight > job.PreviewMaxHeight || len(result.PreviewBytes) == 0 ||
		len(result.PreviewBytes) > MaxPreviewBytes {
		return fmt.Errorf("worker protocol: invalid image preview response")
	}
	return nil
}

func validPreviewFormat(format string) bool {
	return format == PreviewFormatJPEG || format == PreviewFormatWebP
}

func validPreviewErrorCode(code string) bool {
	switch code {
	case "stale_preview", "preview_io_failed", "preview_decode_failed",
		"preview_encode_failed", "preview_too_large", "preview_memory_limit":
		return true
	default:
		return false
	}
}

func validateMergedWorkerResult(job *JobMsg, result *JobResultMsg) error {
	allowed := uint32(0)
	switch job.Kind {
	case MediaImage:
		allowed = MaskSHA512 | MaskImagePDQ | MaskPHashParts | MaskSobelHist
	case MediaVideo:
		allowed = MaskSHA512 | MaskVideoThumb | MaskVideo6F | MaskVideoDuration |
			MaskVideoContactSheet | MaskVideo6FPHash | MaskVideo6FSobel | MaskVideoMetadata
	default:
		return fmt.Errorf("worker protocol: invalid media kind %d", job.Kind)
	}
	if job.FieldsMask&^allowed != 0 {
		return fmt.Errorf("worker protocol: job fields %#x exceed media mask %#x", job.FieldsMask, allowed)
	}
	requestedFrames := normalizedRequestedFrames(*job)
	if job.FrameMask&^FrameMaskFull != 0 || (job.Kind == MediaImage && requestedFrames != 0) {
		return fmt.Errorf("worker protocol: invalid requested frame mask %#x", job.FrameMask)
	}
	if result.FramesDone&^requestedFrames != 0 {
		return fmt.Errorf("worker protocol: completed frames %#x exceed requested %#x", result.FramesDone, requestedFrames)
	}
	if len(result.SHA512) != 0 && len(result.SHA512) != 64 {
		return fmt.Errorf("worker protocol: result SHA-512 length %d", len(result.SHA512))
	}
	if result.FieldsDone&MaskSHA512 != 0 && len(result.SHA512) != 64 {
		return fmt.Errorf("worker protocol: completed SHA-512 is missing")
	}
	if job.Phase == Phase2 {
		if len(job.KnownSHA) != 64 || len(result.SHA512) != 64 || !bytes.Equal(job.KnownSHA, result.SHA512) {
			return fmt.Errorf("worker protocol: phase-2 SHA-512 does not match KnownSHA")
		}
	} else if len(job.KnownSHA) != 0 && len(result.SHA512) != 0 && !bytes.Equal(job.KnownSHA, result.SHA512) {
		return fmt.Errorf("worker protocol: result SHA-512 does not match known job SHA")
	}
	for _, fieldError := range result.Errors {
		if fieldError.Field == 0 {
			continue
		}
		if fieldError.Field&(fieldError.Field-1) != 0 || fieldError.Field&job.FieldsMask == 0 || fieldError.Field&result.FieldsDone != 0 {
			return fmt.Errorf("worker protocol: invalid error field %#x", fieldError.Field)
		}
	}
	if job.Kind == MediaImage {
		return validateMergedImageResult(result)
	}
	return validateMergedVideoResult(job, result, requestedFrames)
}

func validateMergedImageResult(result *JobResultMsg) error {
	if result.DurationMS != nil || result.ThumbPath != "" || len(result.ThumbPDQ) != 0 || result.ThumbQuality != nil || result.ContactSheetWidth != 0 || result.ContactSheetHeight != 0 || result.FramesDone != 0 || len(result.Frames) != 0 {
		return fmt.Errorf("worker protocol: image result contains video payload")
	}
	for _, frame := range result.FrameResults {
		if frame.FrameIdx != 0 || frame.Status != 0 || frame.TimeMS != 0 || frameHasFeaturePayload(frame) {
			return fmt.Errorf("worker protocol: image result contains fixed frame payload")
		}
	}
	if result.FieldsDone&MaskImagePDQ != 0 {
		if len(result.PDQ) != 32 || result.Quality < 0 || result.Quality > 100 || result.Width <= 0 || result.Height <= 0 {
			return fmt.Errorf("worker protocol: completed image PDQ payload is invalid")
		}
	} else if len(result.PDQ) != 0 || result.Quality != 0 || result.Width != 0 || result.Height != 0 {
		return fmt.Errorf("worker protocol: unclaimed image PDQ payload")
	}
	if err := validatePhase2ImageBlob(MaskPHashParts, result.FieldsDone, result.PHashParts, func(blob []byte) error { _, err := features.DecodePHashParts(blob); return err }, "phash_parts"); err != nil {
		return err
	}
	return validatePhase2ImageBlob(MaskSobelHist, result.FieldsDone, result.SobelHist, func(blob []byte) error { _, err := features.DecodeSobelHist(blob); return err }, "sobel_hist")
}

func validateMergedVideoResult(job *JobMsg, result *JobResultMsg, requestedFrames uint8) error {
	if len(result.PDQ) != 0 || result.Quality != 0 || result.Width != 0 || result.Height != 0 || len(result.PHashParts) != 0 || len(result.SobelHist) != 0 {
		return fmt.Errorf("worker protocol: video result contains image payload")
	}
	durationDone := result.FieldsDone&(MaskVideoDuration|MaskVideoThumb) != 0
	if durationDone {
		if result.DurationMS == nil || *result.DurationMS < 0 {
			return fmt.Errorf("worker protocol: invalid video duration payload")
		}
	} else if result.DurationMS != nil {
		return fmt.Errorf("worker protocol: unclaimed video duration payload")
	}
	contactDone := result.FieldsDone&(MaskVideoContactSheet|MaskVideoThumb) != 0
	if contactDone {
		if result.ThumbPath == "" || len(result.ThumbPDQ) != 32 || result.ThumbQuality == nil || *result.ThumbQuality < 0 || *result.ThumbQuality > 100 || result.ContactSheetWidth <= 0 || result.ContactSheetHeight <= 0 {
			return fmt.Errorf("worker protocol: invalid video contact-sheet payload")
		}
	} else if result.ThumbPath != "" || len(result.ThumbPDQ) != 0 || result.ThumbQuality != nil || result.ContactSheetWidth != 0 || result.ContactSheetHeight != 0 {
		return fmt.Errorf("worker protocol: unclaimed video contact-sheet payload")
	}
	if len(result.Frames) != 0 {
		return validatePhase2VideoResult(job, result)
	}
	for index, frame := range result.FrameResults {
		bit := uint8(1 << uint(index))
		if requestedFrames&bit == 0 {
			if frame.FrameIdx != 0 || frame.Status != 0 || frame.TimeMS != 0 || frameHasFeaturePayload(frame) {
				return fmt.Errorf("worker protocol: unrequested frame slot %d carries data", index)
			}
			continue
		}
		if frame.FrameIdx != index {
			return fmt.Errorf("worker protocol: frame slot %d has index %d", index, frame.FrameIdx)
		}
		done := result.FramesDone&bit != 0
		if done {
			if frame.Status != 0 {
				return fmt.Errorf("worker protocol: invalid successful frame %d", index)
			}
			if err := validatePhase2FramePayload(job.ScreenStage, FrameFeature{
				FrameIdx: frame.FrameIdx, TimeMS: frame.TimeMS, PDQ256: frame.PDQ256,
				Quality: frame.Quality, PHashParts: frame.PHashParts, SobelHist: frame.SobelHist,
			}); err != nil {
				return fmt.Errorf("worker protocol: frame %d: %w", index, err)
			}
		} else if frame.Status == 0 || frameHasFeaturePayload(frame) {
			return fmt.Errorf("worker protocol: invalid failed frame %d", index)
		}
	}
	if result.FieldsDone&job.FieldsMask != 0 && result.FramesDone != requestedFrames {
		return fmt.Errorf("worker protocol: video6f completed with frames %#x, want %#x", result.FramesDone, requestedFrames)
	}
	return nil
}

func validatePhase2ImageBlob(
	field uint32,
	fieldsDone uint32,
	blob []byte,
	decode func([]byte) error,
	name string,
) error {
	succeeded := fieldsDone&field != 0
	if !succeeded && len(blob) != 0 {
		return fmt.Errorf("worker protocol: unclaimed phase-2 %s payload", name)
	}
	if !succeeded {
		return nil
	}
	if err := decode(blob); err != nil {
		return fmt.Errorf("worker protocol: invalid phase-2 %s: %w", name, err)
	}
	return nil
}

func validatePhase2VideoResult(job *JobMsg, result *JobResultMsg) error {
	wanted := uint32(MaskVideo6F)
	switch job.ScreenStage {
	case ScreenStageLegacy:
	case ScreenStageTwo:
		wanted = MaskVideo6FPHash
	case ScreenStageThree:
		wanted = MaskVideo6FSobel
	default:
		return fmt.Errorf("worker protocol: invalid phase-2 screen stage %d", job.ScreenStage)
	}
	if job.FieldsMask != wanted {
		return fmt.Errorf(
			"worker protocol: phase-2 video job fields %#x, want %#x for stage %d",
			job.FieldsMask, wanted, job.ScreenStage,
		)
	}
	const fullFrameMask uint8 = 1<<6 - 1
	effectiveFrameMask := job.FrameMask
	if effectiveFrameMask == 0 {
		effectiveFrameMask = fullFrameMask
	}
	if effectiveFrameMask&^fullFrameMask != 0 {
		return fmt.Errorf(
			"worker protocol: phase-2 video job contains invalid frame mask %#x",
			effectiveFrameMask,
		)
	}
	if len(result.PHashParts) != 0 || len(result.SobelHist) != 0 {
		return fmt.Errorf("worker protocol: phase-2 video result contains image payload")
	}
	var seen [6]bool
	complete := 0
	for _, frame := range result.Frames {
		if frame.FrameIdx < 0 || frame.FrameIdx >= len(seen) {
			return fmt.Errorf(
				"worker protocol: phase-2 frame index %d is out of range",
				frame.FrameIdx,
			)
		}
		if effectiveFrameMask&(1<<uint(frame.FrameIdx)) == 0 {
			return fmt.Errorf(
				"worker protocol: phase-2 frame index %d was not requested",
				frame.FrameIdx,
			)
		}
		if seen[frame.FrameIdx] {
			return fmt.Errorf(
				"worker protocol: duplicate phase-2 frame index %d",
				frame.FrameIdx,
			)
		}
		seen[frame.FrameIdx] = true
		if frame.Error != "" {
			if len(frame.PDQ256) != 0 || frame.Quality != 0 ||
				len(frame.PHashParts) != 0 || len(frame.SobelHist) != 0 {
				return fmt.Errorf(
					"worker protocol: errored phase-2 frame %d contains feature payload",
					frame.FrameIdx,
				)
			}
			continue
		}
		if err := validatePhase2FramePayload(job.ScreenStage, frame); err != nil {
			return fmt.Errorf("worker protocol: phase-2 frame %d: %w", frame.FrameIdx, err)
		}
		complete++
	}
	if result.FieldsDone&wanted != 0 && complete != len(seen) {
		return fmt.Errorf(
			"worker protocol: completed phase-2 video has %d complete frames, want %d",
			complete,
			len(seen),
		)
	}
	return nil
}

func validatePhase2FramePayload(stage ScreenStage, frame FrameFeature) error {
	switch stage {
	case ScreenStageLegacy:
		if len(frame.PDQ256) != 32 || frame.Quality < 0 || frame.Quality > 100 {
			return fmt.Errorf("invalid legacy PDQ payload")
		}
		if _, err := features.DecodePHashParts(frame.PHashParts); err != nil {
			return fmt.Errorf("phash_parts: %w", err)
		}
		if _, err := features.DecodeSobelHist(frame.SobelHist); err != nil {
			return fmt.Errorf("sobel_hist: %w", err)
		}
	case ScreenStageTwo:
		if len(frame.PDQ256) != 0 || frame.Quality != 0 || len(frame.SobelHist) != 0 {
			return fmt.Errorf("stage-two frame contains foreign payload")
		}
		if _, err := features.DecodePHashParts(frame.PHashParts); err != nil {
			return fmt.Errorf("phash_parts: %w", err)
		}
	case ScreenStageThree:
		if len(frame.PDQ256) != 0 || frame.Quality != 0 || len(frame.PHashParts) != 0 {
			return fmt.Errorf("stage-three frame contains foreign payload")
		}
		if _, err := features.DecodeSobelHist(frame.SobelHist); err != nil {
			return fmt.Errorf("sobel_hist: %w", err)
		}
	}
	return nil
}

func (worker *workerProc) stopWatchdog() {
	worker.mu.Lock()
	run := worker.current
	worker.mu.Unlock()
	if run != nil && run.timer != nil {
		run.timer.Stop()
	}
}

func (worker *workerProc) kill() {
	worker.killOnce.Do(func() { _ = worker.proc.Kill() })
}

func (worker *workerProc) abort() {
	if worker.conn != nil {
		_ = worker.conn.Close()
	}
	worker.kill()
	_, _ = worker.proc.Wait()
	_ = worker.proc.Close()
}

func (worker *workerProc) sendShutdown() {
	if worker.ipc != nil {
		_ = worker.ipc.Write(MsgShutdown, struct{}{})
	}
}
