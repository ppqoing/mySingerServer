package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"dedup/internal/localtask"
	"dedup/internal/proto"
	"dedup/internal/worker"
)

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
