package wproc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"dedup/internal/worker"
)

type workerRPC struct {
	mu sync.Mutex

	conn          *worker.IPCConn
	job           *worker.JobMsg
	nextRequestID uint64
	failed        error
}

func newWorkerRPC(conn *worker.IPCConn, job *worker.JobMsg) *workerRPC {
	return &workerRPC{conn: conn, job: job}
}

func (rpc *workerRPC) querySHA(query *worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
	rpc.mu.Lock()
	defer rpc.mu.Unlock()
	if err := rpc.readyLocked(); err != nil {
		return nil, err
	}
	if query == nil || query.JobID != rpc.job.JobID {
		return nil, rpc.failLocked(fmt.Errorf("SHA query job identity mismatch"))
	}
	if err := rpc.conn.Write(worker.MsgSHAQuery, query); err != nil {
		return nil, rpc.failLocked(fmt.Errorf("write SHA query: %w", err))
	}
	envelope, err := rpc.conn.Read()
	if err != nil {
		return nil, rpc.failLocked(fmt.Errorf("read SHA reply: %w", err))
	}
	if envelope.Type != worker.MsgSHAReply {
		return nil, rpc.failLocked(fmt.Errorf("unexpected %q while awaiting SHA reply", envelope.Type))
	}
	reply, err := worker.DecodeBody[worker.SHAReplyMsg](envelope)
	if err != nil {
		return nil, rpc.failLocked(err)
	}
	if reply.JobID != query.JobID {
		return nil, rpc.failLocked(fmt.Errorf("SHA reply job identity mismatch"))
	}
	return &reply, nil
}

func (rpc *workerRPC) acquireIOLease(ctx context.Context, class uint8, want int64, seek bool) (
	worker.IOLeaseAcquireMsg,
	worker.IOLeaseGrantMsg,
	time.Duration,
	error,
) {
	rpc.mu.Lock()
	defer rpc.mu.Unlock()
	var request worker.IOLeaseAcquireMsg
	var grant worker.IOLeaseGrantMsg
	if err := rpc.readyLocked(); err != nil {
		return request, grant, 0, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return request, grant, 0, err
	}
	rpc.nextRequestID++
	request = worker.IOLeaseAcquireMsg{
		JobID: rpc.job.JobID, RequestID: rpc.nextRequestID,
		TaskID: rpc.job.ScanTaskID, InstanceID: rpc.job.ScanInstanceID, DiskKey: rpc.job.DiskKey,
		Class: class, WantBytes: want, WantSeek: seek,
	}
	if err := request.Validate(); err != nil {
		return request, grant, 0, rpc.failLocked(err)
	}
	started := time.Now()
	if err := rpc.conn.Write(worker.MsgIOLeaseAcquire, request); err != nil {
		return request, grant, 0, rpc.failLocked(fmt.Errorf("write I/O lease acquire: %w", err))
	}
	envelope, err := rpc.conn.Read()
	wait := time.Since(started)
	if err != nil {
		return request, grant, wait, rpc.failLocked(fmt.Errorf("read I/O lease response: %w", err))
	}
	switch envelope.Type {
	case worker.MsgIOLeaseGrant:
		grant, err = worker.DecodeBody[worker.IOLeaseGrantMsg](envelope)
		if err != nil {
			return request, grant, wait, rpc.failLocked(err)
		}
		if err := grant.ValidateFor(request); err != nil {
			return request, grant, wait, rpc.failLocked(err)
		}
		return request, grant, wait, nil
	case worker.MsgIOLeaseCancel:
		cancel, decodeErr := worker.DecodeBody[worker.IOLeaseCancelMsg](envelope)
		if decodeErr != nil {
			return request, grant, wait, rpc.failLocked(decodeErr)
		}
		if cancel.JobID != request.JobID || cancel.RequestID != request.RequestID {
			return request, grant, wait, rpc.failLocked(fmt.Errorf("I/O lease cancel identity mismatch"))
		}
		return request, grant, wait, context.Canceled
	default:
		return request, grant, wait, rpc.failLocked(fmt.Errorf("unexpected %q while awaiting I/O lease", envelope.Type))
	}
}

func (rpc *workerRPC) reportIOLease(report worker.IOLeaseReportMsg) error {
	rpc.mu.Lock()
	defer rpc.mu.Unlock()
	if err := rpc.readyLocked(); err != nil {
		return err
	}
	if report.JobID != rpc.job.JobID {
		return rpc.failLocked(fmt.Errorf("I/O lease report job identity mismatch"))
	}
	if err := rpc.conn.Write(worker.MsgIOLeaseReport, report); err != nil {
		return rpc.failLocked(fmt.Errorf("write I/O lease report: %w", err))
	}
	return nil
}

func (rpc *workerRPC) readyLocked() error {
	if rpc.failed != nil {
		return rpc.failed
	}
	if rpc.conn == nil || rpc.job == nil || rpc.job.JobID <= 0 {
		return rpc.failLocked(fmt.Errorf("RPC job is not configured"))
	}
	return nil
}

func (rpc *workerRPC) failLocked(err error) error {
	if err == nil {
		err = errors.New("unknown RPC failure")
	}
	if rpc.failed == nil {
		rpc.failed = fmt.Errorf("%w: %v", errIOLeaseInfrastructure, err)
	}
	return rpc.failed
}
