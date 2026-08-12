package agent

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"dedup/internal/localtask"
	"dedup/internal/proto"
	"dedup/internal/worker"
	"github.com/vmihailenco/msgpack/v5"
)

// Break caught: groups/review/preview operations are decoded inconsistently or
// a strict-preview decode failure still reaches the service with a secret path.
func TestLocalResultHandlerDispatchesGroupsReviewAndFileIDOnlyPreview(t *testing.T) {
	reviews := &fakeLocalReviewService{}
	previews := &fakeLocalPreviewService{}
	handler := NewLocalResultHandler(reviews, previews)

	listPayload, _ := proto.EncodeLocalPayload(proto.LocalGroupListRequest{Scope: "current", Limit: 20})
	if response := handler.HandleLocal(context.Background(), proto.LocalRequest{RequestID: "list", Operation: proto.LocalOperationGroupsList, Payload: listPayload}); !response.OK || reviews.listCalls != 1 {
		t.Fatalf("groups list response=%#v calls=%d", response, reviews.listCalls)
	}
	reviewPayload, _ := proto.EncodeLocalPayload(proto.LocalReviewSaveRequest{
		RunID: "run", GroupID: "group", Reviewer: "user",
		Decisions: []proto.LocalReviewDecision{{FileID: 1, Decision: "keep"}},
	})
	if response := handler.HandleLocal(context.Background(), proto.LocalRequest{RequestID: "review", Operation: proto.LocalOperationReviewSave, Payload: reviewPayload}); !response.OK || reviews.saveCalls != 1 {
		t.Fatalf("review response=%#v calls=%d", response, reviews.saveCalls)
	}
	previewPayload, _ := proto.EncodeLocalPayload(proto.LocalImagePreviewRequest{FileID: 1, MaxWidth: 10, MaxHeight: 10, Format: "jpeg", Quality: 80})
	if response := handler.HandleLocal(context.Background(), proto.LocalRequest{RequestID: "preview", Operation: proto.LocalOperationPreviewImage, Payload: previewPayload}); !response.OK || previews.calls != 1 {
		t.Fatalf("preview response=%#v calls=%d", response, previews.calls)
	}

	maliciousPayload, _ := msgpack.Marshal(map[string]any{
		"file_id": int64(1), "max_width": 10, "max_height": 10,
		"format": "jpeg", "quality": 80, "path": `D:\private\source.jpg`,
	})
	response := handler.HandleLocal(context.Background(), proto.LocalRequest{RequestID: "malicious", Operation: proto.LocalOperationPreviewImage, Payload: maliciousPayload})
	if response.OK || response.ErrorCode != "invalid_preview" || previews.calls != 1 {
		t.Fatalf("malicious response=%#v preview calls=%d", response, previews.calls)
	}
}

func TestLocalResultHandlerPreservesStablePreviewMemoryLimit(t *testing.T) {
	previews := &fakeLocalPreviewService{err: errors.New("preview_memory_limit")}
	handler := NewLocalResultHandler(&fakeLocalReviewService{}, previews)
	payload, _ := proto.EncodeLocalPayload(proto.LocalImagePreviewRequest{
		FileID: 1, MaxWidth: 10, MaxHeight: 10, Format: "jpeg", Quality: 80,
	})
	response := handler.HandleLocal(context.Background(), proto.LocalRequest{
		RequestID: "budget", Operation: proto.LocalOperationPreviewImage, Payload: payload,
	})
	if response.OK || response.ErrorCode != "preview_memory_limit" {
		t.Fatalf("memory limit response = %#v", response)
	}
}

// Break caught: NodeTray-only result APIs accidentally inherit Manager/non-
// loopback privileges from the regular Agent protocol.
func TestLocalResultSocketRequiresLoopbackNodeTrayAuth(t *testing.T) {
	const token = "result-token"
	server, _ := newLocalControlTestServer(t)
	handler := NewLocalResultHandler(&fakeLocalReviewService{}, &fakeLocalPreviewService{})
	server.SetLocalControl(token, handler)
	payload, _ := proto.EncodeLocalPayload(proto.LocalGroupListRequest{Scope: "current", Limit: 20})
	request := proto.LocalRequest{RequestID: "groups", Operation: proto.LocalOperationGroupsList, Payload: payload}

	manager, closeManager := startLocalControlTestConnection(t, server, net.ParseIP("127.0.0.1"))
	defer closeManager()
	writeLocalControlTestAuth(t, manager, proto.ClientAuth{Role: "manager", Token: token, Version: proto.ProtocolVersion})
	if result := readDeleteTestMessage(t, manager).(*proto.ClientAuthResult); result.Accepted {
		t.Fatalf("manager auth = %#v", result)
	}

	remote, closeRemote := startLocalControlTestConnection(t, server, net.ParseIP("10.2.3.4"))
	defer closeRemote()
	writeLocalControlTestAuth(t, remote, proto.ClientAuth{Role: "nodetray", Token: token, Version: proto.ProtocolVersion})
	if result := readDeleteTestMessage(t, remote).(*proto.ClientAuthResult); result.Accepted {
		t.Fatalf("non-loopback auth = %#v", result)
	}

	client, closeClient := startLocalControlTestConnection(t, server, net.ParseIP("127.0.0.1"))
	defer closeClient()
	writeLocalControlTestAuth(t, client, proto.ClientAuth{Role: "nodetray", Token: token, Version: proto.ProtocolVersion})
	if result := readDeleteTestMessage(t, client).(*proto.ClientAuthResult); !result.Accepted {
		t.Fatalf("NodeTray auth = %#v", result)
	}
	if err := client.WriteFrame(proto.MsgLocalRequest, &request); err != nil {
		t.Fatal(err)
	}
	if response := readDeleteTestMessage(t, client).(*proto.LocalResponse); !response.OK {
		t.Fatalf("groups response = %#v", response)
	}
}

type fakeLocalReviewService struct {
	listCalls   int
	detailCalls int
	saveCalls   int
}

func (fake *fakeLocalReviewService) List(context.Context, proto.LocalGroupListRequest) (proto.LocalGroupListResponse, error) {
	fake.listCalls++
	return proto.LocalGroupListResponse{}, nil
}
func (fake *fakeLocalReviewService) Detail(context.Context, proto.LocalGroupDetailRequest) (proto.LocalGroupDetailResponse, error) {
	fake.detailCalls++
	return proto.LocalGroupDetailResponse{}, nil
}
func (fake *fakeLocalReviewService) Save(context.Context, proto.LocalReviewSaveRequest) (proto.LocalReviewSaveResponse, error) {
	fake.saveCalls++
	return proto.LocalReviewSaveResponse{Saved: true}, nil
}

type fakeLocalPreviewService struct {
	calls int
	err   error
}

func (fake *fakeLocalPreviewService) Preview(context.Context, proto.LocalImagePreviewRequest) (proto.LocalImagePreviewResponse, error) {
	fake.calls++
	return proto.LocalImagePreviewResponse{MIME: "image/jpeg", Width: 1, Height: 1, Bytes: []byte{1}}, fake.err
}

// Break caught: a local StageWorker consumes the process-wide Results channel
// itself or submits before registration, losing a fast terminal result.
func TestLocalStageWorkerRegistersBeforeSubmitAndWaitsForTerminal(t *testing.T) {
	pool := newPhase2FakePool()
	router := NewPoolRouter(pool, nil)
	pool.onSubmit = func(job worker.JobMsg) {
		pool.results <- &worker.JobResultMsg{JobID: job.JobID, ScanTaskID: job.ScanTaskID, Path: job.Path, Kind: job.Kind, Phase: job.Phase, ScreenStage: job.ScreenStage, Source: job.Source, SHA512: append([]byte(nil), job.KnownSHA...)}
	}
	adapter := NewLocalStageWorker(pool, router)
	job := &worker.JobMsg{ScanTaskID: "local-task", Path: `D:\media\a.jpg`, Kind: worker.MediaImage, Phase: worker.Phase2, ScreenStage: worker.ScreenStageTwo, Source: worker.JobSourceLocal, KnownSHA: make([]byte, 64)}
	result, err := adapter.Execute(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if job.JobID <= 0 || result.JobID != job.JobID {
		t.Fatalf("job/result IDs = %d/%d", job.JobID, result.JobID)
	}
	if pool.resultsCalls != 1 || pool.crashesCalls != 1 {
		t.Fatalf("global channels were acquired %d/%d times, want router only once", pool.resultsCalls, pool.crashesCalls)
	}
}

// Break caught: cancellation leaves a PoolRouter route alive, allowing a late
// terminal result to pair with abandoned local work.
func TestLocalStageWorkerCancellationCleansRegisteredRoute(t *testing.T) {
	pool := newPhase2FakePool()
	router := NewPoolRouter(pool, nil)
	adapter := NewLocalStageWorker(pool, router)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	job := &worker.JobMsg{ScanTaskID: "cancel", Path: `D:\media\a.jpg`, Kind: worker.MediaImage, Phase: worker.Phase2, ScreenStage: worker.ScreenStageTwo, Source: worker.JobSourceLocal}
	if _, err := adapter.Execute(ctx, job); !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute error = %v", err)
	}
	router.mu.Lock()
	routes := len(router.routes)
	router.mu.Unlock()
	if routes != 0 {
		t.Fatalf("routes after cancellation = %d", routes)
	}
}

func TestLocalStageWorkerUsesCancelableSchedulerSubmission(t *testing.T) {
	pool := &contextSubmitPool{phase2FakePool: newPhase2FakePool(), entered: make(chan struct{})}
	router := NewPoolRouter(pool, nil)
	adapter := NewLocalStageWorker(pool, router)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := adapter.Execute(ctx, &worker.JobMsg{ScanTaskID: "queued", Path: `D:\media\queued.jpg`, Kind: worker.MediaImage, Phase: worker.Phase2, ScreenStage: worker.ScreenStageTwo, Source: worker.JobSourceLocal})
		done <- err
	}()
	<-pool.entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Execute=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Execute did not cancel queued submit")
	}
	if !pool.usedContext {
		t.Fatal("Execute bypassed SubmitContext")
	}
}

type contextSubmitPool struct {
	*phase2FakePool
	entered     chan struct{}
	usedContext bool
}

func (p *contextSubmitPool) SubmitContext(ctx context.Context, _ *worker.JobMsg) error {
	p.usedContext = true
	close(p.entered)
	<-ctx.Done()
	return ctx.Err()
}

// Break caught: task operations are decoded inconsistently or run inline,
// blocking the Socket loop and delaying ping/status responses under load.
func TestLocalTaskHandlerDispatchesTaskCommandsWithoutBlockingHeartbeat(t *testing.T) {
	service := &fakeLocalTaskService{created: make(chan struct{}), createRelease: make(chan struct{})}
	handler := NewLocalTaskHandler(service)
	payload, _ := proto.EncodeLocalPayload(proto.LocalTaskCreateRequest{TaskID: "task-1", Roots: []string{`D:\media`}, Mode: proto.LocalTaskModeScanOnly})
	response := handler.HandleLocal(context.Background(), proto.LocalRequest{RequestID: "create", Operation: proto.LocalOperationTaskCreate, Payload: payload})
	if !response.OK {
		t.Fatalf("create response = %#v", response)
	}
	select {
	case <-service.created:
	case <-time.After(time.Second):
		t.Fatal("create was not dispatched")
	}
	close(service.createRelease)
}

func TestLocalTaskSocketRequiresNodeTrayAuthAndKeepsHeartbeatResponsive(t *testing.T) {
	const token = "task-token"
	server, _ := newLocalControlTestServer(t)
	service := &fakeLocalTaskService{created: make(chan struct{}), createRelease: make(chan struct{})}
	server.SetLocalControl(token, NewLocalTaskHandler(service))
	client, closeClient := startLocalControlTestConnection(t, server, []byte{127, 0, 0, 1})
	defer closeClient()
	payload, _ := proto.EncodeLocalPayload(proto.LocalTaskCreateRequest{TaskID: "socket-task", Roots: []string{`D:\media`}, Mode: proto.LocalTaskModeScanOnly})
	request := proto.LocalRequest{RequestID: "unauthorized", Operation: proto.LocalOperationTaskCreate, Payload: payload}
	if err := client.WriteFrame(proto.MsgLocalRequest, &request); err != nil {
		t.Fatal(err)
	}
	unauthorized := readDeleteTestMessage(t, client).(*proto.LocalResponse)
	if unauthorized.OK || unauthorized.ErrorCode != "unauthorized" {
		t.Fatalf("response = %#v", unauthorized)
	}
	writeLocalControlTestAuth(t, client, proto.ClientAuth{Role: "nodetray", Token: token, Version: proto.ProtocolVersion})
	if result := readDeleteTestMessage(t, client).(*proto.ClientAuthResult); !result.Accepted {
		t.Fatalf("auth = %#v", result)
	}
	request.RequestID = "authorized"
	if err := client.WriteFrame(proto.MsgLocalRequest, &request); err != nil {
		t.Fatal(err)
	}
	if response := readDeleteTestMessage(t, client).(*proto.LocalResponse); !response.OK {
		t.Fatalf("response = %#v", response)
	}
	if err := client.WriteFrame(proto.MsgPing, &proto.Ping{TS: 99}); err != nil {
		t.Fatal(err)
	}
	if pong := readDeleteTestMessage(t, client).(*proto.Pong); pong.TS != 99 {
		t.Fatalf("pong = %#v", pong)
	}
	close(service.createRelease)
}

type fakeLocalTaskService struct {
	created       chan struct{}
	createRelease chan struct{}
}

func (s *fakeLocalTaskService) Create(_ context.Context, request localtask.CreateRequest) (localtask.Task, error) {
	if s.created == nil {
		s.created = make(chan struct{})
	}
	close(s.created)
	go func() { <-s.createRelease }()
	return proto.LocalTask{TaskID: request.TaskID, Mode: request.Mode, Status: "pending"}, nil
}
func (*fakeLocalTaskService) List(context.Context, localtask.ListRequest) (localtask.Page[localtask.Task], error) {
	return localtask.Page[localtask.Task]{}, nil
}
func (*fakeLocalTaskService) Cancel(context.Context, string) error { return nil }
func (*fakeLocalTaskService) Retry(context.Context, string) (localtask.Task, error) {
	return localtask.Task{}, nil
}
func (*fakeLocalTaskService) Resume(context.Context) error { return nil }
