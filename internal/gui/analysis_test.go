package gui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"dedup/internal/firstscreen"
)

type analysisRunnerFunc func(context.Context) (*firstscreen.RunStats, error)

func (f analysisRunnerFunc) Run(ctx context.Context) (*firstscreen.RunStats, error) {
	return f(ctx)
}

type channelAnalysisRunner struct {
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
	stats    *firstscreen.RunStats
	err      error
}

func newChannelAnalysisRunner(stats *firstscreen.RunStats, err error) *channelAnalysisRunner {
	return &channelAnalysisRunner{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		finished: make(chan struct{}),
		stats:    stats,
		err:      err,
	}
}

func (r *channelAnalysisRunner) Run(ctx context.Context) (*firstscreen.RunStats, error) {
	close(r.started)
	select {
	case <-r.release:
	case <-ctx.Done():
		close(r.finished)
		return r.stats, ctx.Err()
	}
	close(r.finished)
	return r.stats, r.err
}

type firstScreenStatusDocument struct {
	Running bool                       `json:"running"`
	Last    *firstscreen.RunStats      `json:"last"`
	LastErr string                     `json:"last_err"`
	Raw     map[string]json.RawMessage `json:"-"`
}

func TestFirstScreenHTTPRunLifecycleAndConcurrentStatus(t *testing.T) {
	result := &firstscreen.RunStats{
		FilesScanned: 3,
		ImagePairs:   1,
		StageElapsedMs: map[string]int64{
			"image_screen": 7,
		},
	}
	runner := newChannelAnalysisRunner(result, nil)
	routes := NewAPI(nil, nil, nil, runner).Routes()

	requestContext, cancelRequest := context.WithCancel(context.Background())
	runRequest := httptest.NewRequest(
		http.MethodPost,
		"/api/analysis/firstscreen/run",
		nil,
	).WithContext(requestContext)
	runResponse := httptest.NewRecorder()
	routes.ServeHTTP(runResponse, runRequest)
	if runResponse.Code != http.StatusAccepted {
		t.Fatalf("first POST status = %d body=%s", runResponse.Code, runResponse.Body.String())
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("accepted analysis did not start")
	}

	cancelRequest()
	runningStatus, runningCode := readFirstScreenStatus(t, routes)
	if runningCode != http.StatusOK || !runningStatus.Running {
		t.Fatalf("status after request cancellation = (%d, %#v), want running", runningCode, runningStatus)
	}

	conflictResponse := httptest.NewRecorder()
	routes.ServeHTTP(conflictResponse, httptest.NewRequest(
		http.MethodPost,
		"/api/analysis/firstscreen/run",
		nil,
	))
	if conflictResponse.Code != http.StatusConflict {
		t.Fatalf("second POST status = %d body=%s", conflictResponse.Code, conflictResponse.Body.String())
	}

	const readers = 16
	statusErrors := make(chan error, readers)
	var readersStarted sync.WaitGroup
	readersStarted.Add(readers)
	var readersDone sync.WaitGroup
	readersDone.Add(readers)
	for range readers {
		go func() {
			defer readersDone.Done()
			readersStarted.Done()
			for range 50 {
				response := httptest.NewRecorder()
				routes.ServeHTTP(response, httptest.NewRequest(
					http.MethodGet,
					"/api/analysis/firstscreen/status",
					nil,
				))
				if response.Code != http.StatusOK {
					statusErrors <- errors.New(response.Body.String())
					return
				}
				var snapshot firstScreenStatusDocument
				if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
					statusErrors <- err
					return
				}
			}
		}()
	}
	readersStarted.Wait()
	close(runner.release)
	readersDone.Wait()
	close(statusErrors)
	for err := range statusErrors {
		t.Errorf("concurrent status request: %v", err)
	}

	select {
	case <-runner.finished:
	case <-time.After(time.Second):
		t.Fatal("analysis did not finish")
	}
	finished := waitFirstScreenStopped(t, routes)
	if finished.LastErr != "" {
		t.Fatalf("last_err = %q, want empty", finished.LastErr)
	}
	if finished.Last == nil ||
		finished.Last.FilesScanned != 3 ||
		finished.Last.StageElapsedMs["image_screen"] != 7 {
		t.Fatalf("finished status = %#v", finished)
	}

	result.FilesScanned = 99
	result.StageElapsedMs["image_screen"] = 99
	snapshot, _ := readFirstScreenStatus(t, routes)
	if snapshot.Last.FilesScanned != 3 || snapshot.Last.StageElapsedMs["image_screen"] != 7 {
		t.Fatalf("status retained runner-owned pointer/map: %#v", snapshot.Last)
	}
}

func TestFirstScreenHTTPRetainsRunnerErrorAndRecoversPanic(t *testing.T) {
	t.Run("error", func(t *testing.T) {
		runnerErr := errors.New("database write failed")
		partial := &firstscreen.RunStats{
			FilesScanned:   4,
			StageElapsedMs: map[string]int64{"db_write": 2},
		}
		runner := newChannelAnalysisRunner(partial, runnerErr)
		routes := NewAPI(nil, nil, nil, runner).Routes()

		response := httptest.NewRecorder()
		routes.ServeHTTP(response, httptest.NewRequest(
			http.MethodPost,
			"/api/analysis/firstscreen/run",
			nil,
		))
		if response.Code != http.StatusAccepted {
			t.Fatalf("POST status = %d body=%s", response.Code, response.Body.String())
		}
		<-runner.started
		close(runner.release)

		status := waitFirstScreenStopped(t, routes)
		if status.LastErr != runnerErr.Error() {
			t.Fatalf("last_err = %q, want %q", status.LastErr, runnerErr)
		}
		if status.Last == nil || status.Last.FilesScanned != 4 {
			t.Fatalf("partial last = %#v", status.Last)
		}
	})

	t.Run("panic", func(t *testing.T) {
		routes := NewAPI(nil, nil, nil, analysisRunnerFunc(func(context.Context) (*firstscreen.RunStats, error) {
			panic("runner exploded")
		})).Routes()

		response := httptest.NewRecorder()
		routes.ServeHTTP(response, httptest.NewRequest(
			http.MethodPost,
			"/api/analysis/firstscreen/run",
			nil,
		))
		if response.Code != http.StatusAccepted {
			t.Fatalf("POST status = %d body=%s", response.Code, response.Body.String())
		}

		status := waitFirstScreenStopped(t, routes)
		if !strings.Contains(status.LastErr, "runner panic: runner exploded") {
			t.Fatalf("last_err = %q, want recovered panic text", status.LastErr)
		}
	})
}

func TestFirstScreenHTTPReturnsServiceUnavailableWhenRunnerMissing(t *testing.T) {
	routes := NewAPI(nil, nil, nil).Routes()

	runResponse := httptest.NewRecorder()
	routes.ServeHTTP(runResponse, httptest.NewRequest(
		http.MethodPost,
		"/api/analysis/firstscreen/run",
		nil,
	))
	if runResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST status = %d body=%s", runResponse.Code, runResponse.Body.String())
	}

	status, statusCode := readFirstScreenStatus(t, routes)
	if statusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET status = %d, want 503", statusCode)
	}
	if status.Running || status.Last != nil || status.LastErr == "" {
		t.Fatalf("unconfigured status = %#v", status)
	}
}

func TestFirstScreenShutdownRejectsNewRunAndWaitsForAcceptedRun(t *testing.T) {
	runner := newChannelAnalysisRunner(&firstscreen.RunStats{
		StageElapsedMs: map[string]int64{},
	}, nil)
	api := NewAPI(nil, nil, nil, runner)
	routes := api.Routes()

	firstResponse := httptest.NewRecorder()
	routes.ServeHTTP(firstResponse, httptest.NewRequest(
		http.MethodPost,
		"/api/analysis/firstscreen/run",
		nil,
	))
	if firstResponse.Code != http.StatusAccepted {
		t.Fatalf("first POST status = %d body=%s", firstResponse.Code, firstResponse.Body.String())
	}
	<-runner.started

	api.BeginAnalysisShutdown()
	rejectedResponse := httptest.NewRecorder()
	routes.ServeHTTP(rejectedResponse, httptest.NewRequest(
		http.MethodPost,
		"/api/analysis/firstscreen/run",
		nil,
	))
	if rejectedResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST during shutdown status = %d body=%s", rejectedResponse.Code, rejectedResponse.Body.String())
	}

	waitReturned := make(chan struct{})
	go func() {
		api.WaitForAnalysis()
		close(waitReturned)
	}()
	select {
	case <-waitReturned:
		t.Fatal("WaitForAnalysis returned before accepted run completed")
	default:
	}

	close(runner.release)
	select {
	case <-waitReturned:
	case <-time.After(time.Second):
		t.Fatal("WaitForAnalysis did not observe accepted run completion")
	}
	status, code := readFirstScreenStatus(t, routes)
	if code != http.StatusOK || status.Running {
		t.Fatalf("status after shutdown wait = (%d, %#v)", code, status)
	}
}

func TestFirstScreenSuccessHookIsGatedBySuccessAndShutdown(t *testing.T) {
	t.Run("successful admitted hook is waited", func(t *testing.T) {
		runner := newChannelAnalysisRunner(
			&firstscreen.RunStats{StageElapsedMs: map[string]int64{}},
			nil,
		)
		hookStarted := make(chan struct{})
		hookRelease := make(chan struct{})
		handlers := NewAnalysisHandlers(runner, func() error {
			close(hookStarted)
			<-hookRelease
			return nil
		})
		mux := http.NewServeMux()
		handlers.Register(mux)
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(
			http.MethodPost,
			"/api/analysis/firstscreen/run",
			nil,
		))
		<-runner.started
		close(runner.release)
		select {
		case <-hookStarted:
		case <-time.After(time.Second):
			t.Fatal("successful run did not admit hook")
		}
		handlers.BeginShutdown()
		waited := make(chan struct{})
		go func() {
			handlers.Wait()
			close(waited)
		}()
		select {
		case <-waited:
			t.Fatal("shutdown did not wait for admitted hook")
		default:
		}
		close(hookRelease)
		select {
		case <-waited:
		case <-time.After(time.Second):
			t.Fatal("shutdown did not finish after hook")
		}
	})

	t.Run("shutdown before hook admission skips hook", func(t *testing.T) {
		runner := newChannelAnalysisRunner(
			&firstscreen.RunStats{StageElapsedMs: map[string]int64{}},
			nil,
		)
		called := make(chan struct{}, 1)
		handlers := NewAnalysisHandlers(runner, func() error {
			called <- struct{}{}
			return nil
		})
		mux := http.NewServeMux()
		handlers.Register(mux)
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(
			http.MethodPost,
			"/api/analysis/firstscreen/run",
			nil,
		))
		<-runner.started
		handlers.BeginShutdown()
		close(runner.release)
		handlers.Wait()
		select {
		case <-called:
			t.Fatal("hook ran after shutdown won admission race")
		default:
		}
	})

	t.Run("runner failure never calls hook", func(t *testing.T) {
		runner := newChannelAnalysisRunner(nil, errors.New("M3 failed"))
		called := make(chan struct{}, 1)
		handlers := NewAnalysisHandlers(runner, func() error {
			called <- struct{}{}
			return nil
		})
		mux := http.NewServeMux()
		handlers.Register(mux)
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(
			http.MethodPost,
			"/api/analysis/firstscreen/run",
			nil,
		))
		<-runner.started
		close(runner.release)
		handlers.Wait()
		select {
		case <-called:
			t.Fatal("hook ran after failed M3")
		default:
		}
	})
}

func TestFirstScreenHTTPCancelStopsRunningAnalysis(t *testing.T) {
	runner := newChannelAnalysisRunner(&firstscreen.RunStats{
		StageElapsedMs: map[string]int64{},
	}, nil)
	routes := NewAPI(nil, nil, nil, runner).Routes()

	runResponse := httptest.NewRecorder()
	routes.ServeHTTP(runResponse, httptest.NewRequest(
		http.MethodPost,
		"/api/analysis/firstscreen/run",
		nil,
	))
	if runResponse.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d body=%s", runResponse.Code, runResponse.Body.String())
	}
	<-runner.started

	cancelResponse := httptest.NewRecorder()
	routes.ServeHTTP(cancelResponse, httptest.NewRequest(
		http.MethodPost,
		"/api/analysis/firstscreen/cancel",
		nil,
	))
	if cancelResponse.Code != http.StatusOK {
		t.Fatalf("cancel status = %d body=%s", cancelResponse.Code, cancelResponse.Body.String())
	}
	// 取消幂等：运行未收口前重复取消仍返回 200。
	repeatResponse := httptest.NewRecorder()
	routes.ServeHTTP(repeatResponse, httptest.NewRequest(
		http.MethodPost,
		"/api/analysis/firstscreen/cancel",
		nil,
	))
	if repeatResponse.Code != http.StatusOK {
		t.Fatalf("repeat cancel status = %d body=%s", repeatResponse.Code, repeatResponse.Body.String())
	}

	status := waitFirstScreenStopped(t, routes)
	if status.LastErr != "已取消" {
		t.Fatalf("last_err after cancel = %q", status.LastErr)
	}

	// 空闲后再次取消 → 409。
	idleResponse := httptest.NewRecorder()
	routes.ServeHTTP(idleResponse, httptest.NewRequest(
		http.MethodPost,
		"/api/analysis/firstscreen/cancel",
		nil,
	))
	if idleResponse.Code != http.StatusConflict {
		t.Fatalf("idle cancel status = %d body=%s", idleResponse.Code, idleResponse.Body.String())
	}
}

func TestFirstScreenHTTPCancelWithoutRunnerIs503(t *testing.T) {
	routes := NewAPI(nil, nil, nil).Routes()
	response := httptest.NewRecorder()
	routes.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/api/analysis/firstscreen/cancel",
		nil,
	))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("cancel status = %d body=%s", response.Code, response.Body.String())
	}
}

func readFirstScreenStatus(t *testing.T, routes http.Handler) (firstScreenStatusDocument, int) {
	t.Helper()
	response := httptest.NewRecorder()
	routes.ServeHTTP(response, httptest.NewRequest(
		http.MethodGet,
		"/api/analysis/firstscreen/status",
		nil,
	))

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode status: %v body=%s", err, response.Body.String())
	}
	for _, key := range []string{"running", "last", "last_err"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("status JSON missing %q: %s", key, response.Body.String())
		}
	}
	var document firstScreenStatusDocument
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatalf("decode status document: %v", err)
	}
	document.Raw = raw
	return document, response.Code
}

func waitFirstScreenStopped(t *testing.T, routes http.Handler) firstScreenStatusDocument {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		status, code := readFirstScreenStatus(t, routes)
		if code == http.StatusOK && !status.Running {
			return status
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("analysis remained running")
	return firstScreenStatusDocument{}
}
