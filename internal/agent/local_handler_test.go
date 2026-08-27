package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"dedup/internal/localdelete"
	"dedup/internal/localtask"
	"dedup/internal/proto"
	"dedup/internal/store"
	"dedup/internal/worker"
	"github.com/vmihailenco/msgpack/v5"
)

// Break caught: Socket deletion accepts an arbitrary path or dispatches
// prepare/execute/status without the strict review-bound DTOs.
func TestLocalDeleteHandlerDispatchesStrictReviewBoundCommands(t *testing.T) {
	service := &fakeLocalDeleteService{}
	handler := NewLocalDeleteHandler(service)
	preparePayload, _ := proto.EncodeLocalPayload(proto.LocalDeletePrepareRequest{RunID: "run", GroupID: "group"})
	response := handler.HandleLocal(context.Background(), proto.LocalRequest{
		RequestID: "prepare", Operation: proto.LocalOperationDeletePrepare, Payload: preparePayload,
	})
	if !response.OK || service.prepareCalls != 1 {
		t.Fatalf("prepare=%#v calls=%d", response, service.prepareCalls)
	}
	var preview proto.LocalDeletePreview
	if err := proto.DecodeLocalPayload(response.Payload, &preview); err != nil || preview.Token != "token" {
		t.Fatalf("preview=%#v err=%v", preview, err)
	}

	executePayload, _ := proto.EncodeLocalPayload(proto.LocalDeleteExecuteRequest{
		BatchID: "batch", SelectionDigest: "digest", Token: "token",
	})
	response = handler.HandleLocal(context.Background(), proto.LocalRequest{
		RequestID: "execute", Operation: proto.LocalOperationDeleteExecute, Payload: executePayload,
	})
	if !response.OK || service.executeCalls != 1 {
		t.Fatalf("execute=%#v calls=%d", response, service.executeCalls)
	}
	statusPayload, _ := proto.EncodeLocalPayload(proto.LocalDeleteStatusRequest{BatchID: "batch"})
	response = handler.HandleLocal(context.Background(), proto.LocalRequest{
		RequestID: "status", Operation: proto.LocalOperationDeleteStatus, Payload: statusPayload,
	})
	if !response.OK || service.statusCalls != 1 {
		t.Fatalf("status=%#v calls=%d", response, service.statusCalls)
	}

	malicious, _ := msgpack.Marshal(map[string]any{
		"run_id": "run", "group_id": "group", "path": `D:\private\source.jpg`,
	})
	response = handler.HandleLocal(context.Background(), proto.LocalRequest{
		RequestID: "malicious", Operation: proto.LocalOperationDeletePrepare, Payload: malicious,
	})
	if response.OK || response.ErrorCode != "invalid_delete_selection" || service.prepareCalls != 1 {
		t.Fatalf("malicious=%#v calls=%d", response, service.prepareCalls)
	}
}

func TestLocalDeleteSocketRequiresLoopbackNodeTrayAuth(t *testing.T) {
	const token = "delete-token"
	server, _ := newLocalControlTestServer(t)
	service := &fakeLocalDeleteService{}
	server.SetLocalControl(token, NewLocalDeleteHandler(service))
	payload, _ := proto.EncodeLocalPayload(proto.LocalDeletePrepareRequest{RunID: "run", GroupID: "group"})
	request := proto.LocalRequest{RequestID: "delete", Operation: proto.LocalOperationDeletePrepare, Payload: payload}

	manager, closeManager := startLocalControlTestConnection(t, server, net.ParseIP("127.0.0.1"))
	defer closeManager()
	writeLocalControlTestAuth(t, manager, proto.ClientAuth{Role: "manager", Token: token, Version: proto.ProtocolVersion})
	if result := readDeleteTestMessage(t, manager).(*proto.ClientAuthResult); result.Accepted {
		t.Fatalf("manager auth=%#v", result)
	}

	remote, closeRemote := startLocalControlTestConnection(t, server, net.ParseIP("10.2.3.4"))
	defer closeRemote()
	writeLocalControlTestAuth(t, remote, proto.ClientAuth{Role: "nodetray", Token: token, Version: proto.ProtocolVersion})
	if result := readDeleteTestMessage(t, remote).(*proto.ClientAuthResult); result.Accepted {
		t.Fatalf("remote NodeTray auth=%#v", result)
	}

	client, closeClient := startLocalControlTestConnection(t, server, net.ParseIP("127.0.0.1"))
	defer closeClient()
	writeLocalControlTestAuth(t, client, proto.ClientAuth{Role: "nodetray", Token: token, Version: proto.ProtocolVersion})
	if result := readDeleteTestMessage(t, client).(*proto.ClientAuthResult); !result.Accepted {
		t.Fatalf("loopback NodeTray auth=%#v", result)
	}
	if err := client.WriteFrame(proto.MsgLocalRequest, &request); err != nil {
		t.Fatal(err)
	}
	if response := readDeleteTestMessage(t, client).(*proto.LocalResponse); !response.OK || service.prepareCalls != 1 {
		t.Fatalf("delete response=%#v calls=%d", response, service.prepareCalls)
	}
}

type fakeLocalDeleteService struct {
	prepareCalls int
	executeCalls int
	statusCalls  int
}

func (fake *fakeLocalDeleteService) Prepare(context.Context, localdelete.DeleteSelection) (localdelete.DeletePreview, error) {
	fake.prepareCalls++
	return proto.LocalDeletePreview{BatchID: "batch", SelectionDigest: "digest", Token: "token"}, nil
}
func (fake *fakeLocalDeleteService) Execute(context.Context, localdelete.DeleteExecution) (localdelete.DeleteBatch, error) {
	fake.executeCalls++
	return proto.LocalDeleteBatch{BatchID: "batch", Status: "succeeded"}, nil
}
func (fake *fakeLocalDeleteService) Status(context.Context, string) (localdelete.DeleteBatch, error) {
	fake.statusCalls++
	return proto.LocalDeleteBatch{BatchID: "batch", Status: "succeeded"}, nil
}

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

// Break caught: the manager channel (no loopback NodeTray auth) either keeps
// every local operation unauthorized — breaking the Web preview proxy — or
// inherits the full NodeTray surface including config/delete/shutdown.
func TestLocalResultSocketAllowsManagerChannelPreviewWhitelistOnly(t *testing.T) {
	const token = "manager-token"
	server, _ := newLocalControlTestServer(t)
	reviews := &fakeLocalReviewService{}
	previews := &fakeLocalPreviewService{}
	server.SetLocalControl(token, NewLocalResultHandler(reviews, previews))

	manager, closeManager := startLocalControlTestConnection(t, server, net.ParseIP("10.2.3.4"))
	defer closeManager()
	previewPayload, _ := proto.EncodeLocalPayload(proto.LocalImagePreviewRequest{
		FileID: 1, MaxWidth: 10, MaxHeight: 10, Format: "jpeg", Quality: 80,
	})
	if err := manager.WriteFrame(proto.MsgLocalRequest, &proto.LocalRequest{
		RequestID: "manager-preview", Operation: proto.LocalOperationPreviewImage, Payload: previewPayload,
	}); err != nil {
		t.Fatal(err)
	}
	if response := readDeleteTestMessage(t, manager).(*proto.LocalResponse); !response.OK || previews.calls != 1 {
		t.Fatalf("manager preview response = %#v calls=%d", response, previews.calls)
	}
	reviewPayload, _ := proto.EncodeLocalPayload(proto.LocalReviewSaveRequest{
		RunID: "run", GroupID: "group", Reviewer: "web",
		Decisions: []proto.LocalReviewDecision{{FileID: 1, Decision: "keep"}},
	})
	if err := manager.WriteFrame(proto.MsgLocalRequest, &proto.LocalRequest{
		RequestID: "manager-review", Operation: proto.LocalOperationReviewSave, Payload: reviewPayload,
	}); err != nil {
		t.Fatal(err)
	}
	if response := readDeleteTestMessage(t, manager).(*proto.LocalResponse); !response.OK || reviews.saveCalls != 1 {
		t.Fatalf("manager review response = %#v calls=%d", response, reviews.saveCalls)
	}
	groupsPayload, _ := proto.EncodeLocalPayload(proto.LocalGroupListRequest{Scope: "current", Limit: 20})
	if err := manager.WriteFrame(proto.MsgLocalRequest, &proto.LocalRequest{
		RequestID: "manager-groups", Operation: proto.LocalOperationGroupsList, Payload: groupsPayload,
	}); err != nil {
		t.Fatal(err)
	}
	if response := readDeleteTestMessage(t, manager).(*proto.LocalResponse); response.OK || response.ErrorCode != "unauthorized" || reviews.listCalls != 0 {
		t.Fatalf("manager groups response = %#v calls=%d", response, reviews.listCalls)
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
	created           chan struct{}
	createRelease     chan struct{}
	pauseTask         localtask.Task
	resumeTask        localtask.Task
	cancelTask        localtask.Task
	retryTask         localtask.Task
	deleteResult      localtask.ControlResult
	pauseRequest      localtask.ControlRequest
	resumeRequest     localtask.ControlRequest
	cancelRequest     localtask.ControlRequest
	retryRequest      localtask.ControlRequest
	deleteRequest     localtask.ControlRequest
	legacyCancel      string
	legacyRetry       string
	pauseCalls        int
	resumeCalls       int
	cancelCalls       int
	retryCalls        int
	deleteCalls       int
	legacyCancelCalls int
	legacyRetryCalls  int
	pauseErr          error
	resumeErr         error
	cancelErr         error
	retryErr          error
	deleteErr         error
	legacyCancelErr   error
	legacyRetryErr    error
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

func (s *fakeLocalTaskService) Pause(_ context.Context, request localtask.ControlRequest) (localtask.Task, error) {
	s.pauseCalls++
	s.pauseRequest = request
	return s.pauseTask, s.pauseErr
}

func (s *fakeLocalTaskService) ResumeTask(_ context.Context, request localtask.ControlRequest) (localtask.Task, error) {
	s.resumeCalls++
	s.resumeRequest = request
	return s.resumeTask, s.resumeErr
}

func (s *fakeLocalTaskService) Cancel(_ context.Context, request localtask.ControlRequest) (localtask.Task, error) {
	s.cancelCalls++
	s.cancelRequest = request
	return s.cancelTask, s.cancelErr
}

func (s *fakeLocalTaskService) Delete(_ context.Context, request localtask.ControlRequest) (localtask.ControlResult, error) {
	s.deleteCalls++
	s.deleteRequest = request
	return s.deleteResult, s.deleteErr
}

func (s *fakeLocalTaskService) Retry(_ context.Context, request localtask.ControlRequest) (localtask.Task, error) {
	s.retryCalls++
	s.retryRequest = request
	return s.retryTask, s.retryErr
}

func (s *fakeLocalTaskService) LegacyCancel(_ context.Context, taskID string) (localtask.Task, error) {
	s.legacyCancelCalls++
	s.legacyCancel = taskID
	return s.cancelTask, s.legacyCancelErr
}

func (s *fakeLocalTaskService) LegacyRetry(_ context.Context, taskID string) (localtask.Task, error) {
	s.legacyRetryCalls++
	s.legacyRetry = taskID
	return s.retryTask, s.legacyRetryErr
}

// Break caught: versioned lifecycle commands may be decoded as legacy task-ID
// requests, routed to the wrong Service method, or return an incomplete task.
func TestLocalTaskHandlerRoutesVersionedLifecycleControls(t *testing.T) {
	request := proto.LocalTaskControlRequest{TaskID: "task-1", InstanceID: "instance-1", ExpectedRevision: 7}
	for _, test := range []struct {
		name      string
		operation string
		configure func(*fakeLocalTaskService)
		calls     func(*fakeLocalTaskService) int
		got       func(*fakeLocalTaskService) localtask.ControlRequest
	}{
		{name: "pause", operation: proto.LocalOperationTaskPause, configure: func(s *fakeLocalTaskService) { s.pauseTask = taskSnapshot("pausing", 8) }, calls: func(s *fakeLocalTaskService) int { return s.pauseCalls }, got: func(s *fakeLocalTaskService) localtask.ControlRequest { return s.pauseRequest }},
		{name: "resume", operation: proto.LocalOperationTaskResume, configure: func(s *fakeLocalTaskService) { s.resumeTask = taskSnapshot("pending", 8) }, calls: func(s *fakeLocalTaskService) int { return s.resumeCalls }, got: func(s *fakeLocalTaskService) localtask.ControlRequest { return s.resumeRequest }},
		{name: "cancel", operation: proto.LocalOperationTaskCancel, configure: func(s *fakeLocalTaskService) { s.cancelTask = taskSnapshot("stopping", 8) }, calls: func(s *fakeLocalTaskService) int { return s.cancelCalls }, got: func(s *fakeLocalTaskService) localtask.ControlRequest { return s.cancelRequest }},
		{name: "retry", operation: proto.LocalOperationTaskRetry, configure: func(s *fakeLocalTaskService) { s.retryTask = taskSnapshot("pending", 8) }, calls: func(s *fakeLocalTaskService) int { return s.retryCalls }, got: func(s *fakeLocalTaskService) localtask.ControlRequest { return s.retryRequest }},
		{name: "delete", operation: proto.LocalOperationTaskDelete, configure: func(s *fakeLocalTaskService) {
			task := taskSnapshot("deleting", 8)
			s.deleteResult = localtask.ControlResult{Task: &task}
		}, calls: func(s *fakeLocalTaskService) int { return s.deleteCalls }, got: func(s *fakeLocalTaskService) localtask.ControlRequest { return s.deleteRequest }},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &fakeLocalTaskService{}
			test.configure(service)
			response := handleLocalTaskRequest(t, service, test.operation, request)
			if !response.OK || test.calls(service) != 1 {
				t.Fatalf("response=%#v calls=%d", response, test.calls(service))
			}
			if got := test.got(service); got != request {
				t.Fatalf("request=%#v want=%#v", got, request)
			}
			var accepted proto.LocalTaskControlResponse
			if err := proto.DecodeLocalPayload(response.Payload, &accepted); err != nil || accepted.Task == nil || accepted.Task.Status == "" || accepted.Task.Revision != 8 {
				t.Fatalf("accepted=%#v err=%v", accepted, err)
			}
		})
	}
}

// Break caught: a completed deletion receipt is discarded instead of producing
// the idempotent deleted acknowledgement expected by the tray.
func TestLocalTaskDeleteReturnsDeletionReceipt(t *testing.T) {
	service := &fakeLocalTaskService{deleteResult: localtask.ControlResult{Deleted: true}}
	response := handleLocalTaskRequest(t, service, proto.LocalOperationTaskDelete,
		proto.LocalTaskControlRequest{TaskID: "task-1", InstanceID: "instance-1", ExpectedRevision: 7})
	var accepted proto.LocalTaskControlResponse
	if !response.OK || proto.DecodeLocalPayload(response.Payload, &accepted) != nil || !accepted.Deleted || accepted.Task != nil {
		t.Fatalf("response=%#v accepted=%#v", response, accepted)
	}
}

// Break caught: cancel/retry either lose backwards compatibility or accept a
// partial versioned payload as an unsafe legacy control.
func TestLocalTaskHandlerLimitsLegacyControlsToStrictTaskIDPayloads(t *testing.T) {
	for _, test := range []struct {
		name      string
		operation string
		calls     func(*fakeLocalTaskService) int
		got       func(*fakeLocalTaskService) string
	}{
		{name: "cancel", operation: proto.LocalOperationTaskCancel, calls: func(s *fakeLocalTaskService) int { return s.legacyCancelCalls }, got: func(s *fakeLocalTaskService) string { return s.legacyCancel }},
		{name: "retry", operation: proto.LocalOperationTaskRetry, calls: func(s *fakeLocalTaskService) int { return s.legacyRetryCalls }, got: func(s *fakeLocalTaskService) string { return s.legacyRetry }},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &fakeLocalTaskService{cancelTask: taskSnapshot("stopping", 8), retryTask: taskSnapshot("pending", 8)}
			response := handleLocalTaskRequest(t, service, test.operation, proto.LocalTaskIDRequest{TaskID: "task-1"})
			if !response.OK || test.calls(service) != 1 || test.got(service) != "task-1" {
				t.Fatalf("legacy response=%#v calls=%d taskID=%q", response, test.calls(service), test.got(service))
			}
		})
	}

	partial, err := msgpack.Marshal(map[string]any{"task_id": "task-1", "instance_id": "instance-1"})
	if err != nil {
		t.Fatal(err)
	}
	service := &fakeLocalTaskService{}
	response := NewLocalTaskHandler(service).HandleLocal(context.Background(), proto.LocalRequest{RequestID: "partial", Operation: proto.LocalOperationTaskCancel, Payload: partial})
	if response.OK || response.ErrorCode != proto.InvalidTaskControlErrorCode || service.legacyCancelCalls != 0 || service.cancelCalls != 0 {
		t.Fatalf("partial response=%#v service=%#v", response, service)
	}

	extra, err := msgpack.Marshal(map[string]any{"task_id": "task-1", "unexpected": true})
	if err != nil {
		t.Fatal(err)
	}
	response = NewLocalTaskHandler(service).HandleLocal(context.Background(), proto.LocalRequest{RequestID: "extra", Operation: proto.LocalOperationTaskRetry, Payload: extra})
	if response.OK || response.ErrorCode != proto.InvalidTaskControlErrorCode || service.legacyRetryCalls != 0 || service.retryCalls != 0 {
		t.Fatalf("extra response=%#v service=%#v", response, service)
	}

	response = handleLocalTaskRequest(t, service, proto.LocalOperationTaskPause, proto.LocalTaskIDRequest{TaskID: "task-1"})
	if response.OK || response.ErrorCode != proto.InvalidTaskControlErrorCode || service.pauseCalls != 0 {
		t.Fatalf("pause legacy response=%#v service=%#v", response, service)
	}
}

// Break caught: backend errors leak implementation details or lose their
// stable control-plane meaning for tray recovery and conflict handling.
func TestLocalTaskHandlerMapsControlErrorsWithoutLeakingBackendText(t *testing.T) {
	secret := errors.New("database secret must never cross the local socket")
	for _, test := range []struct {
		name      string
		operation string
		configure func(*fakeLocalTaskService, error)
		err       error
		want      string
	}{
		{name: "stale", operation: proto.LocalOperationTaskPause, configure: func(s *fakeLocalTaskService, err error) { s.pauseErr = err }, err: fmt.Errorf("wrapped: %w", store.ErrLocalTaskStale), want: "stale_task"},
		{name: "instance mismatch", operation: proto.LocalOperationTaskResume, configure: func(s *fakeLocalTaskService, err error) { s.resumeErr = err }, err: fmt.Errorf("wrapped: %w", store.ErrLocalTaskInstanceMismatch), want: "task_instance_mismatch"},
		{name: "invalid state", operation: proto.LocalOperationTaskCancel, configure: func(s *fakeLocalTaskService, err error) { s.cancelErr = err }, err: fmt.Errorf("wrapped: %w", store.ErrLocalTaskTransition), want: "invalid_task_state"},
		{name: "not found", operation: proto.LocalOperationTaskRetry, configure: func(s *fakeLocalTaskService, err error) { s.retryErr = err }, err: sql.ErrNoRows, want: "task_not_found"},
		{name: "reused legacy task", operation: proto.LocalOperationTaskCancel, configure: func(s *fakeLocalTaskService, err error) { s.legacyCancelErr = err }, err: fmt.Errorf("wrapped: %w", localtask.ErrTaskInstanceRequired), want: "task_instance_required"},
		{name: "delete failure", operation: proto.LocalOperationTaskDelete, configure: func(s *fakeLocalTaskService, err error) { s.deleteErr = err }, err: secret, want: "task_delete_failed"},
		{name: "other control failure", operation: proto.LocalOperationTaskPause, configure: func(s *fakeLocalTaskService, err error) { s.pauseErr = err }, err: secret, want: "task_control_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &fakeLocalTaskService{}
			test.configure(service, test.err)
			input := any(proto.LocalTaskControlRequest{TaskID: "task-1", InstanceID: "instance-1", ExpectedRevision: 7})
			if test.name == "reused legacy task" {
				input = proto.LocalTaskIDRequest{TaskID: "task-1"}
			}
			response := handleLocalTaskRequest(t, service, test.operation, input)
			if response.OK || response.ErrorCode != test.want || response.ErrorCode == secret.Error() {
				t.Fatalf("response=%#v want=%q", response, test.want)
			}
		})
	}

	response := NewLocalTaskHandler(nil).HandleLocal(context.Background(), proto.LocalRequest{RequestID: "unavailable", Operation: proto.LocalOperationTaskPause})
	if response.OK || response.ErrorCode != "local_task_unavailable" {
		t.Fatalf("unavailable=%#v", response)
	}
}

// Break caught: legacy cancel/retry can target a new task that reused a
// deleted task ID, instead of requiring an instance-aware control request.
func TestLocalTaskHandlerRequiresInstanceForReusedLegacyTaskID(t *testing.T) {
	service := newReceiptBlockedLocalTaskService(t)
	handler := NewLocalTaskHandler(service)
	for _, operation := range []string{proto.LocalOperationTaskCancel, proto.LocalOperationTaskRetry} {
		t.Run(operation, func(t *testing.T) {
			payload, err := proto.EncodeLocalPayload(proto.LocalTaskIDRequest{TaskID: "reused-task"})
			if err != nil {
				t.Fatal(err)
			}
			response := handler.HandleLocal(context.Background(), proto.LocalRequest{RequestID: operation, Operation: operation, Payload: payload})
			if response.OK || response.ErrorCode != "task_instance_required" {
				t.Fatalf("response=%#v", response)
			}
		})
	}
}

func handleLocalTaskRequest(t *testing.T, service LocalTaskService, operation string, input any) proto.LocalResponse {
	t.Helper()
	payload, err := proto.EncodeLocalPayload(input)
	if err != nil {
		t.Fatal(err)
	}
	return NewLocalTaskHandler(service).HandleLocal(context.Background(), proto.LocalRequest{RequestID: operation, Operation: operation, Payload: payload})
}

func taskSnapshot(status string, revision int64) localtask.Task {
	return localtask.Task{TaskID: "task-1", InstanceID: "instance-1", Revision: revision, Mode: proto.LocalTaskModeScanOnly, Status: status}
}

func newReceiptBlockedLocalTaskService(t *testing.T) localtask.Service {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &receiptTaskRunner{started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(runner.release) })
	service := localtask.NewService("machine-a", db, runner)
	request := localtask.CreateRequest{TaskID: "reused-task", Roots: []string{`D:\\media`}, Mode: proto.LocalTaskModeScanOnly}
	original, err := service.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("task did not start")
	}
	current := waitTaskForHandler(t, service, original.TaskID, "running")
	if _, err := service.Delete(context.Background(), localtask.ControlRequest{
		TaskID: current.TaskID, InstanceID: current.InstanceID, ExpectedRevision: current.Revision,
	}); err != nil {
		t.Fatal(err)
	}
	waitReceiptForHandler(t, db, original)
	replacement, err := service.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.InstanceID == original.InstanceID {
		t.Fatalf("replacement instance=%q original=%q", replacement.InstanceID, original.InstanceID)
	}
	return service
}

type receiptTaskRunner struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (runner *receiptTaskRunner) Run(control localtask.RunControl, _ localtask.CreateRequest, _ localtask.Task, _ func(localtask.ProgressUpdate) error) error {
	runner.once.Do(func() { close(runner.started) })
	select {
	case <-control.Drain:
		return localtask.ErrDrainRequested
	case <-control.Context.Done():
		return control.Context.Err()
	case <-runner.release:
		return nil
	}
}

func waitReceiptForHandler(t *testing.T, db *store.DB, task localtask.Task) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := db.LoadLocalTaskDeletionReceipt(context.Background(), "machine-a", task.TaskID, task.InstanceID); err == nil {
			return
		} else if !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("deletion receipt for task %q was not persisted", task.TaskID)
}

func waitTaskForHandler(t *testing.T, service localtask.Service, taskID, status string) localtask.Task {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		page, err := service.List(context.Background(), localtask.ListRequest{Limit: 200})
		if err != nil {
			t.Fatal(err)
		}
		for _, task := range page.Items {
			if task.TaskID == taskID && task.Status == status {
				return task
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("task %q did not reach %s", taskID, status)
	return localtask.Task{}
}
