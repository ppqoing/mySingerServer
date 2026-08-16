//go:build windows

package wproc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"dedup/internal/worker"
)

const (
	minIOLeaseWindowBytes     int64 = 1 << 20
	defaultIOLeaseWindowBytes int64 = 4 << 20
	maxIOLeaseWindowBytes     int64 = 16 << 20
)

var errIOLeaseInfrastructure = errors.New("worker I/O lease infrastructure failure")

type IOLeaseClient interface {
	BeforeRead(ctx context.Context, want int) (leaseID uint64, granted int, err error)
	AfterRead(leaseID uint64, bytes int, elapsed time.Duration, err error)
	BeforeSeek(ctx context.Context) (leaseID uint64, err error)
	AfterSeek(leaseID uint64, elapsed time.Duration, err error)
}

type governedFile struct {
	file  *os.File
	lease IOLeaseClient
	ctx   context.Context
}

func openSource(ctx context.Context, job *worker.JobMsg, lease IOLeaseClient) (*governedFile, error) {
	if job == nil || job.Path == "" || lease == nil {
		return nil, fmt.Errorf("%w: source opener is not configured", errIOLeaseInfrastructure)
	}
	file, err := os.Open(fixPath(job.Path))
	if err != nil {
		return nil, err
	}
	return &governedFile{file: file, lease: lease, ctx: ctx}, nil
}

func (file *governedFile) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	if file == nil || file.file == nil || file.lease == nil {
		return 0, fmt.Errorf("%w: source read has no lease client", errIOLeaseInfrastructure)
	}
	ctx := file.context()
	leaseID, granted, err := file.lease.BeforeRead(ctx, len(buffer))
	if err != nil {
		return 0, err
	}
	if granted <= 0 || granted > len(buffer) {
		return 0, fmt.Errorf("%w: invalid read grant %d for %d bytes", errIOLeaseInfrastructure, granted, len(buffer))
	}
	started := time.Now()
	read, readErr := file.file.Read(buffer[:granted])
	if ctxErr := ctx.Err(); ctxErr != nil {
		readErr = ctxErr
	}
	file.lease.AfterRead(leaseID, read, time.Since(started), readErr)
	return read, readErr
}

func (file *governedFile) Seek(offset int64, whence int) (int64, error) {
	if file == nil || file.file == nil || file.lease == nil {
		return 0, fmt.Errorf("%w: source seek has no lease client", errIOLeaseInfrastructure)
	}
	ctx := file.context()
	leaseID, err := file.lease.BeforeSeek(ctx)
	if err != nil {
		return 0, err
	}
	started := time.Now()
	position, seekErr := file.file.Seek(offset, whence)
	if ctxErr := ctx.Err(); ctxErr != nil {
		seekErr = ctxErr
	}
	file.lease.AfterSeek(leaseID, time.Since(started), seekErr)
	return position, seekErr
}

func (file *governedFile) Stat() (os.FileInfo, error) {
	if file == nil || file.file == nil {
		return nil, os.ErrInvalid
	}
	return file.file.Stat()
}

func (file *governedFile) Close() error {
	if file == nil || file.file == nil {
		return nil
	}
	return file.file.Close()
}

func (file *governedFile) context() context.Context {
	if file.ctx != nil {
		return file.ctx
	}
	return context.Background()
}

type localIOLeaseClient struct {
	mu sync.Mutex

	ctx         context.Context
	rpc         *workerRPC
	job         *worker.JobMsg
	windowBytes int64

	grant     worker.IOLeaseGrantMsg
	remaining int64
	bytes     int64
	seeks     uint32
	readTime  time.Duration
	waitTime  time.Duration

	pendingKind     uint8
	pendingLeaseID  uint64
	pendingReserved int64
	terminalErr     error
}

func newLocalIOLeaseClient(ctx context.Context, rpc *workerRPC, job *worker.JobMsg, windowBytes int64) *localIOLeaseClient {
	if ctx == nil {
		ctx = context.Background()
	}
	if windowBytes < minIOLeaseWindowBytes {
		windowBytes = minIOLeaseWindowBytes
	}
	if windowBytes > maxIOLeaseWindowBytes {
		windowBytes = maxIOLeaseWindowBytes
	}
	return &localIOLeaseClient{ctx: ctx, rpc: rpc, job: job, windowBytes: windowBytes}
}

func (client *localIOLeaseClient) BeforeRead(ctx context.Context, want int) (uint64, int, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if want <= 0 {
		return 0, 0, fmt.Errorf("%w: read size %d is invalid", errIOLeaseInfrastructure, want)
	}
	if err := client.readyLocked(ctx); err != nil {
		return 0, 0, err
	}
	if client.pendingKind != 0 {
		return 0, 0, fmt.Errorf("%w: overlapping source operation", errIOLeaseInfrastructure)
	}
	requested := int64(want)
	if requested > maxIOLeaseWindowBytes {
		requested = maxIOLeaseWindowBytes
	}
	if client.grant.LeaseID == 0 || client.remaining < requested {
		if err := client.flushLocked(false); err != nil {
			return 0, 0, err
		}
		window := client.windowBytes
		if requested > window {
			window = requested
		}
		if err := client.acquireLocked(ctx, 1, window, false); err != nil {
			return 0, 0, err
		}
	}
	granted := requested
	if granted > client.remaining {
		granted = client.remaining
	}
	client.remaining -= granted
	client.pendingKind = 1
	client.pendingLeaseID = client.grant.LeaseID
	client.pendingReserved = granted
	return client.grant.LeaseID, int(granted), nil
}

func (client *localIOLeaseClient) AfterRead(leaseID uint64, bytes int, elapsed time.Duration, operationErr error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.pendingKind != 1 || client.pendingLeaseID != leaseID || client.grant.LeaseID != leaseID {
		client.rememberLocked(fmt.Errorf("%w: read lease %d is not pending", errIOLeaseInfrastructure, leaseID))
		return
	}
	actual := int64(bytes)
	if actual < 0 || actual > client.pendingReserved {
		client.rememberLocked(fmt.Errorf("%w: read reported %d bytes from %d", errIOLeaseInfrastructure, bytes, client.pendingReserved))
		actual = 0
	}
	client.remaining += client.pendingReserved - actual
	client.bytes += actual
	if elapsed > 0 {
		client.readTime += elapsed
	}
	client.clearPendingLocked()
	if operationErr != nil {
		cancelled := isContextError(operationErr) || client.contextErrLocked(nil) != nil
		client.rememberLocked(client.flushLocked(cancelled))
	}
}

func (client *localIOLeaseClient) BeforeSeek(ctx context.Context) (uint64, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if err := client.readyLocked(ctx); err != nil {
		return 0, err
	}
	if client.pendingKind != 0 {
		return 0, fmt.Errorf("%w: overlapping source operation", errIOLeaseInfrastructure)
	}
	if err := client.flushLocked(false); err != nil {
		return 0, err
	}
	if err := client.acquireLocked(ctx, 2, minIOLeaseWindowBytes, true); err != nil {
		return 0, err
	}
	client.pendingKind = 2
	client.pendingLeaseID = client.grant.LeaseID
	return client.grant.LeaseID, nil
}

func (client *localIOLeaseClient) AfterSeek(leaseID uint64, elapsed time.Duration, operationErr error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.pendingKind != 2 || client.pendingLeaseID != leaseID || client.grant.LeaseID != leaseID {
		client.rememberLocked(fmt.Errorf("%w: seek lease %d is not pending", errIOLeaseInfrastructure, leaseID))
		return
	}
	client.seeks = 1
	if elapsed > 0 {
		client.readTime += elapsed
	}
	client.clearPendingLocked()
	cancelled := isContextError(operationErr) || client.contextErrLocked(nil) != nil
	client.rememberLocked(client.flushLocked(cancelled))
}

func (client *localIOLeaseClient) finish(operationErr error) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.pendingKind != 0 {
		client.rememberLocked(fmt.Errorf("%w: source operation did not report completion", errIOLeaseInfrastructure))
	}
	cancelled := isContextError(operationErr) || client.contextErrLocked(nil) != nil
	client.rememberLocked(client.flushLocked(cancelled))
	return client.terminalErr
}

func (client *localIOLeaseClient) readyLocked(ctx context.Context) error {
	if client.terminalErr != nil {
		return client.terminalErr
	}
	if client.rpc == nil || client.job == nil {
		return fmt.Errorf("%w: lease client is not configured", errIOLeaseInfrastructure)
	}
	return client.contextErrLocked(ctx)
}

func (client *localIOLeaseClient) acquireLocked(ctx context.Context, class uint8, want int64, seek bool) error {
	request, grant, wait, err := client.rpc.acquireIOLease(client.contextFor(ctx), class, want, seek)
	if err != nil {
		client.rememberLocked(err)
		return err
	}
	client.grant = grant
	client.remaining = grant.Bytes
	client.bytes = 0
	client.seeks = 0
	client.readTime = 0
	client.waitTime = wait
	if ctxErr := client.contextErrLocked(ctx); ctxErr != nil {
		client.rememberLocked(client.flushLocked(true))
		client.rememberLocked(ctxErr)
		return ctxErr
	}
	_ = request
	return nil
}

func (client *localIOLeaseClient) flushLocked(cancelled bool) error {
	if client.grant.LeaseID == 0 {
		return nil
	}
	report := worker.IOLeaseReportMsg{
		JobID: client.grant.JobID, RequestID: client.grant.RequestID,
		LeaseID: client.grant.LeaseID, Generation: client.grant.Generation,
		TaskID: client.job.ScanTaskID, InstanceID: client.job.ScanInstanceID, DiskKey: client.job.DiskKey,
		Bytes: client.bytes, Seeks: client.seeks,
		ReadNS: client.readTime.Nanoseconds(), WaitNS: client.waitTime.Nanoseconds(),
		Completed: !cancelled, Cancelled: cancelled,
	}
	grant := client.grant
	client.grant = worker.IOLeaseGrantMsg{}
	client.remaining = 0
	client.bytes = 0
	client.seeks = 0
	client.readTime = 0
	client.waitTime = 0
	if err := report.ValidateFor(grant); err != nil {
		return fmt.Errorf("%w: invalid lease report: %v", errIOLeaseInfrastructure, err)
	}
	if err := client.rpc.reportIOLease(report); err != nil {
		return err
	}
	return nil
}

func (client *localIOLeaseClient) contextFor(ctx context.Context) context.Context {
	if ctx != nil {
		return ctx
	}
	return client.ctx
}

func (client *localIOLeaseClient) contextErrLocked(ctx context.Context) error {
	if err := client.ctx.Err(); err != nil {
		return err
	}
	if ctx != nil {
		return ctx.Err()
	}
	return nil
}

func (client *localIOLeaseClient) clearPendingLocked() {
	client.pendingKind = 0
	client.pendingLeaseID = 0
	client.pendingReserved = 0
}

func (client *localIOLeaseClient) rememberLocked(err error) {
	if err != nil && client.terminalErr == nil {
		client.terminalErr = err
	}
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

var _ io.ReadSeekCloser = (*governedFile)(nil)
