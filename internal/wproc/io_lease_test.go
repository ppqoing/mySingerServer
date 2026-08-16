//go:build windows

package wproc

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"dedup/internal/worker"
)

const leaseReadChunk = 64 << 10

// Break caught: every 64 KiB source read crosses IPC even though a 4 MiB
// grant still has local tokens, or the exhausted window is reused forever.
func TestIOLeaseClientReusesFourMiBGrantAndRefillsOnlyWhenExhausted(t *testing.T) {
	client, parent, cleanup := newLeaseClientPipe(t)
	defer cleanup()

	var mu sync.Mutex
	var requests []worker.IOLeaseAcquireMsg
	reports := make(chan worker.IOLeaseReportMsg, 2)
	serverDone := make(chan error, 1)
	go func() {
		defer cleanup()
		for grantIndex := uint64(1); grantIndex <= 2; grantIndex++ {
			envelope, err := parent.Read()
			if err != nil {
				serverDone <- err
				return
			}
			request, err := worker.DecodeBody[worker.IOLeaseAcquireMsg](envelope)
			if err != nil || envelope.Type != worker.MsgIOLeaseAcquire {
				serverDone <- errors.New("expected lease acquire")
				return
			}
			mu.Lock()
			requests = append(requests, request)
			mu.Unlock()
			if err := parent.Write(worker.MsgIOLeaseGrant, worker.IOLeaseGrantMsg{
				JobID: request.JobID, RequestID: request.RequestID,
				LeaseID: 100 + grantIndex, Generation: 7, Bytes: defaultIOLeaseWindowBytes,
			}); err != nil {
				serverDone <- err
				return
			}
			envelope, err = parent.Read()
			if err != nil {
				serverDone <- err
				return
			}
			report, err := worker.DecodeBody[worker.IOLeaseReportMsg](envelope)
			if err != nil || envelope.Type != worker.MsgIOLeaseReport {
				serverDone <- errors.New("expected lease report")
				return
			}
			reports <- report
		}
		serverDone <- nil
	}()

	for index := 0; index < 65; index++ {
		leaseID, granted, err := client.BeforeRead(context.Background(), leaseReadChunk)
		if err != nil {
			t.Fatalf("BeforeRead %d: %v", index, err)
		}
		if granted != leaseReadChunk {
			t.Fatalf("BeforeRead %d granted=%d, want %d", index, granted, leaseReadChunk)
		}
		client.AfterRead(leaseID, granted, time.Millisecond, nil)
	}
	if err := client.finish(nil); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("lease acquire count=%d, want 2", len(requests))
	}
	for index, request := range requests {
		if request.RequestID != uint64(index+1) || request.WantBytes != defaultIOLeaseWindowBytes || request.WantSeek {
			t.Fatalf("lease request %d = %#v", index, request)
		}
	}
	first, second := <-reports, <-reports
	if first.Bytes != defaultIOLeaseWindowBytes || !first.Completed || first.Cancelled {
		t.Fatalf("first report = %#v", first)
	}
	if second.Bytes != leaseReadChunk || !second.Completed || second.Cancelled {
		t.Fatalf("second report = %#v", second)
	}
}

// Break caught: seeks consume byte-window tokens or one seek grant is silently
// reused for later random access.
func TestIOLeaseClientAcquiresEachSeekSeparately(t *testing.T) {
	client, parent, cleanup := newLeaseClientPipe(t)
	defer cleanup()

	serverDone := make(chan error, 1)
	go func() {
		defer cleanup()
		for index := uint64(1); index <= 2; index++ {
			envelope, err := parent.Read()
			if err != nil {
				serverDone <- err
				return
			}
			request, err := worker.DecodeBody[worker.IOLeaseAcquireMsg](envelope)
			if err != nil || envelope.Type != worker.MsgIOLeaseAcquire || !request.WantSeek ||
				request.RequestID != index || request.WantBytes != minIOLeaseWindowBytes {
				serverDone <- errors.New("invalid seek acquire")
				return
			}
			if err := parent.Write(worker.MsgIOLeaseGrant, worker.IOLeaseGrantMsg{
				JobID: request.JobID, RequestID: request.RequestID,
				LeaseID: 200 + index, Generation: 8, Bytes: minIOLeaseWindowBytes, Seeks: 1,
			}); err != nil {
				serverDone <- err
				return
			}
			envelope, err = parent.Read()
			if err != nil {
				serverDone <- err
				return
			}
			report, err := worker.DecodeBody[worker.IOLeaseReportMsg](envelope)
			if err != nil || envelope.Type != worker.MsgIOLeaseReport || report.Seeks != 1 || !report.Completed {
				serverDone <- errors.New("invalid seek report")
				return
			}
		}
		serverDone <- nil
	}()

	for index := 0; index < 2; index++ {
		leaseID, err := client.BeforeSeek(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		client.AfterSeek(leaseID, time.Millisecond, nil)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

// Break caught: a parent-side generation cancellation is ignored, leaving a
// source read blocked or granting unrestricted local access.
func TestIOLeaseClientCancelWakesBlockedAcquire(t *testing.T) {
	client, parent, cleanup := newLeaseClientPipe(t)
	defer cleanup()

	go func() {
		envelope, err := parent.Read()
		if err != nil {
			return
		}
		request, err := worker.DecodeBody[worker.IOLeaseAcquireMsg](envelope)
		if err == nil {
			_ = parent.Write(worker.MsgIOLeaseCancel, worker.IOLeaseCancelMsg{
				JobID: request.JobID, RequestID: request.RequestID,
			})
		}
	}()

	if _, _, err := client.BeforeRead(context.Background(), leaseReadChunk); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquire error=%v, want context.Canceled", err)
	}
}

// Break caught: parent pipe EOF is interpreted as an unlimited grant or as a
// normal media EOF rather than a Worker infrastructure failure.
func TestIOLeaseClientPipeEOFIsInfrastructureError(t *testing.T) {
	client, parent, cleanup := newLeaseClientPipe(t)
	defer cleanup()

	go func() {
		_, _ = parent.Read()
		cleanup()
	}()

	_, _, err := client.BeforeRead(context.Background(), leaseReadChunk)
	if err == nil || !errors.Is(err, errIOLeaseInfrastructure) || errors.Is(err, context.Canceled) {
		t.Fatalf("pipe EOF error=%v, want infrastructure error", err)
	}
}

// Break caught: governedFile touches the source handle when the lease client
// denied the operation, allowing source I/O to bypass policy.
func TestGovernedSourceDoesNotReadOrSeekWithoutGrant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(path, []byte("governed"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	denied := errors.New("lease denied")
	governed := &governedFile{file: file, lease: denyingLease{err: denied}}
	t.Cleanup(func() { _ = governed.Close() })

	if n, err := governed.Read(make([]byte, 4)); n != 0 || !errors.Is(err, denied) {
		t.Fatalf("Read=(%d,%v), want (0,denied)", n, err)
	}
	if offset, err := file.Seek(0, io.SeekCurrent); err != nil || offset != 0 {
		t.Fatalf("offset after denied read=(%d,%v), want 0", offset, err)
	}
	if offset, err := governed.Seek(3, io.SeekStart); offset != 0 || !errors.Is(err, denied) {
		t.Fatalf("Seek=(%d,%v), want (0,denied)", offset, err)
	}
	if offset, err := file.Seek(0, io.SeekCurrent); err != nil || offset != 0 {
		t.Fatalf("offset after denied seek=(%d,%v), want 0", offset, err)
	}
}

type denyingLease struct{ err error }

func (lease denyingLease) BeforeRead(context.Context, int) (uint64, int, error) {
	return 0, 0, lease.err
}
func (denyingLease) AfterRead(uint64, int, time.Duration, error) {}
func (lease denyingLease) BeforeSeek(context.Context) (uint64, error) {
	return 0, lease.err
}
func (denyingLease) AfterSeek(uint64, time.Duration, error) {}

func newLeaseClientPipe(t *testing.T) (*localIOLeaseClient, *worker.IPCConn, func()) {
	t.Helper()
	server, parent := net.Pipe()
	job := &worker.JobMsg{
		JobID: 41, ScanTaskID: "task", ScanInstanceID: "instance", DiskKey: "disk",
		Path: `C:\source.bin`, Kind: worker.MediaImage, Phase: worker.Phase1,
	}
	rpc := newWorkerRPC(worker.NewIPCConn(server), job)
	client := newLocalIOLeaseClient(context.Background(), rpc, job, defaultIOLeaseWindowBytes)
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			_ = server.Close()
			_ = parent.Close()
		})
	}
	t.Cleanup(cleanup)
	return client, worker.NewIPCConn(parent), cleanup
}
