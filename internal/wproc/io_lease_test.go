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

// Break caught: a syntactically valid seek grant with zero seek tokens is
// treated as authorization and moves the real source handle.
func TestGovernedSourceRejectsZeroSeekTokenWithoutMovingHandle(t *testing.T) {
	client, parent, cleanup := newLeaseClientPipe(t)
	defer cleanup()
	path := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(path, []byte("governed"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	governed := &governedFile{file: file, lease: client}
	defer governed.Close()

	serverDone := make(chan error, 1)
	go func() {
		envelope, err := parent.Read()
		if err != nil {
			serverDone <- err
			return
		}
		request, err := worker.DecodeBody[worker.IOLeaseAcquireMsg](envelope)
		if err != nil || envelope.Type != worker.MsgIOLeaseAcquire || !request.WantSeek {
			serverDone <- errors.New("expected seek acquire")
			return
		}
		if err := parent.Write(worker.MsgIOLeaseGrant, worker.IOLeaseGrantMsg{
			JobID: request.JobID, RequestID: request.RequestID,
			LeaseID: 250, Generation: 9, Bytes: minIOLeaseWindowBytes, Seeks: 0,
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
		if err != nil || envelope.Type != worker.MsgIOLeaseReport || !report.Cancelled || report.Completed || report.Seeks != 0 {
			serverDone <- errors.New("zero-token seek grant was not cancelled")
			return
		}
		serverDone <- nil
	}()

	position, seekErr := governed.Seek(3, io.SeekStart)
	// Close the pipe after the operation returns so a broken implementation
	// that never reclaims the zero-token grant cannot strand the test reader.
	cleanup()
	serverErr := <-serverDone
	underlying, offsetErr := file.Seek(0, io.SeekCurrent)
	if seekErr == nil || !errors.Is(seekErr, errIOLeaseInfrastructure) || position != 0 {
		t.Fatalf("zero-token Seek=(%d,%v), want (0,infrastructure error)", position, seekErr)
	}
	if serverErr != nil {
		t.Fatal(serverErr)
	}
	if offsetErr != nil || underlying != 0 {
		t.Fatalf("underlying offset=(%d,%v), want 0", underlying, offsetErr)
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

// Break caught: an operation-level context error is used only to mark the
// report cancelled and is then forgotten before job completion.
func TestIOLeaseClientRemembersReadAndSeekCancellation(t *testing.T) {
	tests := []struct {
		name         string
		operationErr error
		seek         bool
	}{
		{name: "read canceled", operationErr: context.Canceled},
		{name: "seek deadline", operationErr: context.DeadlineExceeded, seek: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, parent, cleanup := newLeaseClientPipe(t)
			defer cleanup()
			serverDone := make(chan error, 1)
			go func() {
				envelope, err := parent.Read()
				if err != nil {
					serverDone <- err
					return
				}
				request, err := worker.DecodeBody[worker.IOLeaseAcquireMsg](envelope)
				if err != nil || envelope.Type != worker.MsgIOLeaseAcquire || request.WantSeek != test.seek {
					serverDone <- errors.New("unexpected cancellation acquire")
					return
				}
				grant := worker.IOLeaseGrantMsg{
					JobID: request.JobID, RequestID: request.RequestID,
					LeaseID: 280, Generation: 10, Bytes: request.WantBytes,
				}
				if test.seek {
					grant.Seeks = 1
				}
				if err := parent.Write(worker.MsgIOLeaseGrant, grant); err != nil {
					serverDone <- err
					return
				}
				envelope, err = parent.Read()
				if err != nil {
					serverDone <- err
					return
				}
				report, err := worker.DecodeBody[worker.IOLeaseReportMsg](envelope)
				if err != nil || envelope.Type != worker.MsgIOLeaseReport || !report.Cancelled || report.Completed {
					serverDone <- errors.New("operation cancellation was not reported")
					return
				}
				serverDone <- nil
			}()

			if test.seek {
				leaseID, err := client.BeforeSeek(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				client.AfterSeek(leaseID, time.Millisecond, test.operationErr)
			} else {
				leaseID, granted, err := client.BeforeRead(context.Background(), leaseReadChunk)
				if err != nil {
					t.Fatal(err)
				}
				client.AfterRead(leaseID, granted, time.Millisecond, test.operationErr)
			}
			if err := <-serverDone; err != nil {
				t.Fatal(err)
			}
			if err := client.finish(nil); !errors.Is(err, test.operationErr) {
				t.Fatalf("finish error=%v, want %v", err, test.operationErr)
			}
		})
	}
}

// Break caught: Phase 2 converts a context error observed before or between
// reads into a media FieldError and returns a nil infrastructure error.
func TestGovernedSourcePhase2PropagatesContextCancellation(t *testing.T) {
	t.Run("pre-canceled", func(t *testing.T) {
		job, deps, _ := newPhase2ImageHarness([]byte("phase two"))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := processPhase2WithDeps(ctx, testConfig(), job, deps)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Phase 2 error=%v, want context.Canceled", err)
		}
		if len(result.Errors) != 0 {
			t.Fatalf("Phase 2 cancellation became media errors: %#v", result.Errors)
		}
	})

	t.Run("deadline before start", func(t *testing.T) {
		job, deps, _ := newPhase2ImageHarness([]byte("phase two"))
		ctx, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
		defer cancel()
		result, err := processPhase2WithDeps(ctx, testConfig(), job, deps)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Phase 2 error=%v, want context.DeadlineExceeded", err)
		}
		if len(result.Errors) != 0 {
			t.Fatalf("Phase 2 deadline became media errors: %#v", result.Errors)
		}
	})

	t.Run("canceled after read", func(t *testing.T) {
		job, deps, _ := newPhase2ImageHarness([]byte("more than one byte"))
		ctx, cancel := context.WithCancel(context.Background())
		originalOpen := deps.open
		deps.open = func(path string) (readStatCloser, error) {
			file, err := originalOpen(path)
			if err != nil {
				return nil, err
			}
			return &cancelAfterReadFile{readStatCloser: file, cancel: cancel}, nil
		}
		cfg := testConfig()
		cfg.ReadChunkBytes = 1
		result, err := processPhase2WithDeps(ctx, cfg, job, deps)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Phase 2 error=%v, want context.Canceled", err)
		}
		if len(result.Errors) != 0 {
			t.Fatalf("Phase 2 read cancellation became media errors: %#v", result.Errors)
		}
	})
}

// Break caught: Preview turns cancellation before the first source operation,
// or immediately after a real source Read/Seek, into preview_io_failed.
func TestGovernedSourcePreviewPropagatesContextCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preview.jpg")
	writePreviewJPEG(t, path, 48, 24, false)
	job := previewJobForFile(t, path, worker.PreviewFormatJPEG)

	tests := []struct {
		name     string
		ctx      func() (context.Context, context.CancelFunc)
		cancelOn string
		wantErr  error
	}{
		{
			name: "pre-canceled",
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			},
			wantErr: context.Canceled,
		},
		{
			name: "deadline before start",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Unix(1, 0))
			},
			wantErr: context.DeadlineExceeded,
		},
		{
			name: "canceled after read",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			cancelOn: "read",
			wantErr:  context.Canceled,
		},
		{
			name: "canceled after seek",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			cancelOn: "seek",
			wantErr:  context.Canceled,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := test.ctx()
			defer cancel()
			result, err := generateImagePreviewWithOpen(ctx, &job, 256<<20, func() (previewSourceFile, error) {
				raw, openErr := os.Open(path)
				if openErr != nil {
					return nil, openErr
				}
				handle := sourceFileHandle(raw)
				if test.cancelOn != "" {
					handle = &cancelAfterSourceOperation{
						sourceFileHandle: handle,
						cancel:           cancel,
						operation:        test.cancelOn,
					}
				}
				return &governedFile{source: handle, lease: &unlimitedRecordingLease{}, ctx: ctx}, nil
			})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Preview error=%v, want %v", err, test.wantErr)
			}
			if result.PreviewErrorCode != "" {
				t.Fatalf("Preview cancellation became media error %q", result.PreviewErrorCode)
			}
		})
	}
}

type cancelAfterSourceOperation struct {
	sourceFileHandle
	cancel    context.CancelFunc
	operation string
	once      sync.Once
}

func (file *cancelAfterSourceOperation) Read(buffer []byte) (int, error) {
	read, err := file.sourceFileHandle.Read(buffer)
	if file.operation == "read" {
		file.once.Do(file.cancel)
	}
	return read, err
}

func (file *cancelAfterSourceOperation) Seek(offset int64, whence int) (int64, error) {
	position, err := file.sourceFileHandle.Seek(offset, whence)
	if file.operation == "seek" {
		file.once.Do(file.cancel)
	}
	return position, err
}

type unlimitedRecordingLease struct {
	mu       sync.Mutex
	next     uint64
	readOpen bool
	seekOpen bool
}

func (lease *unlimitedRecordingLease) BeforeRead(_ context.Context, want int) (uint64, int, error) {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	lease.next++
	lease.readOpen = true
	return lease.next, want, nil
}

func (lease *unlimitedRecordingLease) AfterRead(_ uint64, _ int, _ time.Duration, _ error) {
	lease.mu.Lock()
	lease.readOpen = false
	lease.mu.Unlock()
}

func (lease *unlimitedRecordingLease) BeforeSeek(_ context.Context) (uint64, error) {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	lease.next++
	lease.seekOpen = true
	return lease.next, nil
}

func (lease *unlimitedRecordingLease) AfterSeek(_ uint64, _ time.Duration, _ error) {
	lease.mu.Lock()
	lease.seekOpen = false
	lease.mu.Unlock()
}

type cancelAfterReadFile struct {
	readStatCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (file *cancelAfterReadFile) Read(buffer []byte) (int, error) {
	read, err := file.readStatCloser.Read(buffer)
	file.once.Do(file.cancel)
	return read, err
}

func TestIOLeaseClientClampsWindowToOneAndSixteenMiB(t *testing.T) {
	tests := []struct {
		name       string
		configured int64
		wantWindow int64
	}{
		{name: "one MiB minimum", configured: 1, wantWindow: minIOLeaseWindowBytes},
		{name: "sixteen MiB maximum", configured: 64 << 20, wantWindow: maxIOLeaseWindowBytes},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, parent, cleanup := newLeaseClientPipeWithWindow(t, test.configured)
			defer cleanup()
			serverDone := make(chan error, 1)
			go func() {
				envelope, err := parent.Read()
				if err != nil {
					serverDone <- err
					return
				}
				request, err := worker.DecodeBody[worker.IOLeaseAcquireMsg](envelope)
				if err != nil || request.WantBytes != test.wantWindow || request.WantSeek {
					serverDone <- errors.New("window was not clamped")
					return
				}
				if err := parent.Write(worker.MsgIOLeaseGrant, worker.IOLeaseGrantMsg{
					JobID: request.JobID, RequestID: request.RequestID,
					LeaseID: 310, Generation: 14, Bytes: request.WantBytes,
				}); err != nil {
					serverDone <- err
					return
				}
				envelope, err = parent.Read()
				if err != nil || envelope.Type != worker.MsgIOLeaseReport {
					serverDone <- errors.New("missing clamped-window report")
					return
				}
				serverDone <- nil
			}()
			leaseID, granted, err := client.BeforeRead(context.Background(), 1)
			if err != nil || granted != 1 {
				t.Fatalf("BeforeRead=(%d,%v), want (1,nil)", granted, err)
			}
			client.AfterRead(leaseID, 1, time.Millisecond, nil)
			if err := client.finish(nil); err != nil {
				t.Fatal(err)
			}
			if err := <-serverDone; err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Break caught: a short source read burns the entire reservation instead of
// refunding unused local tokens, forcing an unnecessary second parent acquire.
func TestIOLeaseClientRefundsShortReadReservation(t *testing.T) {
	client, parent, cleanup := newLeaseClientPipeWithWindow(t, minIOLeaseWindowBytes)
	defer cleanup()
	serverDone := make(chan error, 1)
	go func() {
		envelope, err := parent.Read()
		if err != nil {
			serverDone <- err
			return
		}
		request, err := worker.DecodeBody[worker.IOLeaseAcquireMsg](envelope)
		if err != nil || request.WantBytes != minIOLeaseWindowBytes {
			serverDone <- errors.New("unexpected short-read acquire")
			return
		}
		if err := parent.Write(worker.MsgIOLeaseGrant, worker.IOLeaseGrantMsg{
			JobID: request.JobID, RequestID: request.RequestID,
			LeaseID: 320, Generation: 15, Bytes: request.WantBytes,
		}); err != nil {
			serverDone <- err
			return
		}
		envelope, err = parent.Read()
		if err != nil {
			serverDone <- err
			return
		}
		if envelope.Type == worker.MsgIOLeaseAcquire {
			unexpected, _ := worker.DecodeBody[worker.IOLeaseAcquireMsg](envelope)
			_ = parent.Write(worker.MsgIOLeaseCancel, worker.IOLeaseCancelMsg{
				JobID: unexpected.JobID, RequestID: unexpected.RequestID,
			})
			serverDone <- errors.New("short read did not refund unused tokens")
			return
		}
		if envelope.Type != worker.MsgIOLeaseReport {
			serverDone <- errors.New("expected final short-read report")
			return
		}
		serverDone <- nil
	}()

	leaseID, granted, err := client.BeforeRead(context.Background(), int(minIOLeaseWindowBytes))
	if err != nil || granted != int(minIOLeaseWindowBytes) {
		t.Fatalf("first BeforeRead=(%d,%v)", granted, err)
	}
	client.AfterRead(leaseID, 1, time.Millisecond, nil)
	leaseID, granted, err = client.BeforeRead(context.Background(), int(minIOLeaseWindowBytes-1))
	if err != nil || granted != int(minIOLeaseWindowBytes-1) {
		t.Fatalf("refunded BeforeRead=(%d,%v)", granted, err)
	}
	client.AfterRead(leaseID, granted, time.Millisecond, nil)
	if err := client.finish(nil); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

// These cases lock the synchronous single-reader RPC protocol closed: a
// stale/duplicate grant or an unrelated envelope may never authorize source I/O.
func TestWorkerRPCRejectsOutOfOrderDuplicateAndUnexpectedLeaseReplies(t *testing.T) {
	tests := []struct {
		name  string
		reply func(*worker.IPCConn, worker.IOLeaseAcquireMsg) error
	}{
		{
			name: "out of order request id",
			reply: func(parent *worker.IPCConn, request worker.IOLeaseAcquireMsg) error {
				return parent.Write(worker.MsgIOLeaseGrant, worker.IOLeaseGrantMsg{
					JobID: request.JobID, RequestID: request.RequestID + 1,
					LeaseID: 330, Generation: 16, Bytes: request.WantBytes,
				})
			},
		},
		{
			name: "unexpected envelope",
			reply: func(parent *worker.IPCConn, request worker.IOLeaseAcquireMsg) error {
				return parent.Write(worker.MsgSHAReply, worker.SHAReplyMsg{JobID: request.JobID})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rpc, parent, cleanup := newWorkerRPCPipe(t)
			defer cleanup()
			serverDone := make(chan error, 1)
			go func() {
				envelope, err := parent.Read()
				if err != nil {
					serverDone <- err
					return
				}
				request, err := worker.DecodeBody[worker.IOLeaseAcquireMsg](envelope)
				if err != nil {
					serverDone <- err
					return
				}
				serverDone <- test.reply(parent, request)
			}()
			_, _, _, err := rpc.acquireIOLease(context.Background(), 1, minIOLeaseWindowBytes, false)
			if err == nil || !errors.Is(err, errIOLeaseInfrastructure) {
				t.Fatalf("RPC error=%v, want infrastructure failure", err)
			}
			if err := <-serverDone; err != nil {
				t.Fatal(err)
			}
		})
	}

	t.Run("duplicate previous grant", func(t *testing.T) {
		rpc, parent, cleanup := newWorkerRPCPipe(t)
		defer cleanup()
		serverDone := make(chan error, 1)
		go func() {
			firstEnvelope, err := parent.Read()
			if err != nil {
				serverDone <- err
				return
			}
			first, err := worker.DecodeBody[worker.IOLeaseAcquireMsg](firstEnvelope)
			if err != nil {
				serverDone <- err
				return
			}
			oldGrant := worker.IOLeaseGrantMsg{
				JobID: first.JobID, RequestID: first.RequestID,
				LeaseID: 340, Generation: 17, Bytes: first.WantBytes,
			}
			if err := parent.Write(worker.MsgIOLeaseGrant, oldGrant); err != nil {
				serverDone <- err
				return
			}
			if _, err := parent.Read(); err != nil {
				serverDone <- err
				return
			}
			serverDone <- parent.Write(worker.MsgIOLeaseGrant, oldGrant)
		}()
		if _, _, _, err := rpc.acquireIOLease(context.Background(), 1, minIOLeaseWindowBytes, false); err != nil {
			t.Fatal(err)
		}
		_, _, _, err := rpc.acquireIOLease(context.Background(), 1, minIOLeaseWindowBytes, false)
		if err == nil || !errors.Is(err, errIOLeaseInfrastructure) {
			t.Fatalf("duplicate grant error=%v, want infrastructure failure", err)
		}
		if err := <-serverDone; err != nil {
			t.Fatal(err)
		}
	})
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
	return newLeaseClientPipeWithWindow(t, defaultIOLeaseWindowBytes)
}

func newLeaseClientPipeWithWindow(t *testing.T, window int64) (*localIOLeaseClient, *worker.IPCConn, func()) {
	t.Helper()
	server, parent := net.Pipe()
	job := &worker.JobMsg{
		JobID: 41, ScanTaskID: "task", ScanInstanceID: "instance", DiskKey: "disk",
		Path: `C:\source.bin`, Kind: worker.MediaImage, Phase: worker.Phase1,
	}
	rpc := newWorkerRPC(worker.NewIPCConn(server), job)
	client := newLocalIOLeaseClient(context.Background(), rpc, job, window)
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

func newWorkerRPCPipe(t *testing.T) (*workerRPC, *worker.IPCConn, func()) {
	t.Helper()
	server, parent := net.Pipe()
	job := &worker.JobMsg{
		JobID: 42, ScanTaskID: "task", ScanInstanceID: "instance", DiskKey: "disk",
		Path: `C:\source.bin`, Kind: worker.MediaImage, Phase: worker.Phase1,
	}
	rpc := newWorkerRPC(worker.NewIPCConn(server), job)
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			_ = server.Close()
			_ = parent.Close()
		})
	}
	t.Cleanup(cleanup)
	return rpc, worker.NewIPCConn(parent), cleanup
}
