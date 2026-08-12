package agent

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"dedup/internal/worker"
)

func TestPoolRouterIsSoleConsumerAndRoutesInterleavedPhasesByFullOwner(t *testing.T) {
	pool := newPhase2FakePool()
	router := NewPoolRouter(
		pool,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	phase1 := worker.JobMsg{
		JobID:      router.NextJobID(),
		ScanTaskID: "scan-phase1",
		Path:       `D:\media\phase1.jpg`,
		Kind:       worker.MediaImage,
		Phase:      worker.Phase1,
	}
	phase2 := worker.JobMsg{
		JobID:      router.NextJobID(),
		ScanTaskID: "scan-phase2",
		Path:       `E:\media\phase2.mp4`,
		Kind:       worker.MediaVideo,
		Phase:      worker.Phase2,
	}
	phase1Terminal, cancel1, err := router.Register(&phase1)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel1()
	phase2Terminal, cancel2, err := router.Register(&phase2)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel2()

	pool.results <- &worker.JobResultMsg{
		JobID:      phase1.JobID,
		ScanTaskID: phase1.ScanTaskID,
		Path:       phase1.Path,
		Kind:       phase1.Kind,
		Phase:      worker.Phase2,
	}
	pool.crashes <- worker.CrashRecord{
		JobID:      phase2.JobID,
		ScanTaskID: "foreign-task",
		File:       phase2.Path,
	}
	pool.crashes <- worker.CrashRecord{
		JobID:      phase2.JobID,
		ScanTaskID: phase2.ScanTaskID,
		File:       phase2.Path,
		Reason:     "watchdog_video",
	}
	pool.results <- &worker.JobResultMsg{
		JobID:      phase1.JobID,
		ScanTaskID: phase1.ScanTaskID,
		Path:       phase1.Path,
		Kind:       phase1.Kind,
		Phase:      phase1.Phase,
	}

	select {
	case terminal := <-phase1Terminal:
		if terminal.result == nil ||
			terminal.result.ScanTaskID != phase1.ScanTaskID ||
			terminal.crash != nil {
			t.Fatalf("phase1 terminal=%#v", terminal)
		}
	case <-time.After(time.Second):
		t.Fatal("phase1 result was stolen")
	}
	select {
	case terminal := <-phase2Terminal:
		if terminal.crash == nil ||
			terminal.crash.ScanTaskID != phase2.ScanTaskID ||
			terminal.result != nil {
			t.Fatalf("phase2 terminal=%#v", terminal)
		}
	case <-time.After(time.Second):
		t.Fatal("phase2 crash was stolen")
	}
	pool.mu.Lock()
	resultsCalls := pool.resultsCalls
	crashesCalls := pool.crashesCalls
	pool.mu.Unlock()
	if resultsCalls != 1 || crashesCalls != 1 {
		t.Fatalf("global channel access results=%d crashes=%d, want sole 1/1",
			resultsCalls, crashesCalls)
	}
	if phase1.JobID == phase2.JobID {
		t.Fatalf("cross-phase JobID collision=%d", phase1.JobID)
	}
}

func TestPoolRouterPoolCloseTerminatesEveryRegisteredOwner(t *testing.T) {
	pool := newPhase2FakePool()
	router := NewPoolRouter(
		pool,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	jobs := []worker.JobMsg{
		{
			JobID: router.NextJobID(), ScanTaskID: "close-a",
			Path: `D:\a.jpg`, Kind: worker.MediaImage, Phase: worker.Phase1,
		},
		{
			JobID: router.NextJobID(), ScanTaskID: "close-b",
			Path: `E:\b.mp4`, Kind: worker.MediaVideo, Phase: worker.Phase2,
		},
	}
	terminals := make([]<-chan poolTerminal, 0, len(jobs))
	for index := range jobs {
		terminal, _, err := router.Register(&jobs[index])
		if err != nil {
			t.Fatal(err)
		}
		terminals = append(terminals, terminal)
	}
	close(pool.results)

	for index, terminal := range terminals {
		select {
		case got := <-terminal:
			if got.err == nil {
				t.Fatalf("terminal[%d]=%#v, want pool-close error", index, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("owner[%d] hung after pool close", index)
		}
	}
	if _, _, err := router.Register(&worker.JobMsg{
		JobID:      router.NextJobID(),
		ScanTaskID: "after-close",
		Path:       `F:\closed.jpg`,
		Kind:       worker.MediaImage,
		Phase:      worker.Phase2,
	}); err == nil {
		t.Fatal("Register after pool close unexpectedly succeeded")
	}
}

func TestPoolRouterRejectsForeignStageAndSourceResult(t *testing.T) {
	pool := newPhase2FakePool()
	router := NewPoolRouter(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	job := worker.JobMsg{
		JobID: router.NextJobID(), ScanTaskID: "stage-source", Path: `D:\media\owner.jpg`,
		Kind: worker.MediaImage, Phase: worker.Phase2,
		ScreenStage: worker.ScreenStageTwo, Source: worker.JobSourceManager,
	}
	terminal, cancel, err := router.Register(&job)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	for _, foreign := range []worker.JobResultMsg{
		{JobID: job.JobID, ScanTaskID: job.ScanTaskID, Path: job.Path, Kind: job.Kind, Phase: job.Phase, ScreenStage: worker.ScreenStageThree, Source: job.Source},
		{JobID: job.JobID, ScanTaskID: job.ScanTaskID, Path: job.Path, Kind: job.Kind, Phase: job.Phase, ScreenStage: job.ScreenStage, Source: worker.JobSourceLocal},
	} {
		copy := foreign
		pool.results <- &copy
	}
	select {
	case got := <-terminal:
		t.Fatalf("foreign result reached owner: %#v", got)
	case <-time.After(20 * time.Millisecond):
	}

	pool.results <- &worker.JobResultMsg{
		JobID: job.JobID, ScanTaskID: job.ScanTaskID, Path: job.Path, Kind: job.Kind,
		Phase: job.Phase, ScreenStage: job.ScreenStage, Source: job.Source,
	}
	select {
	case got := <-terminal:
		if got.result == nil || got.result.ScreenStage != job.ScreenStage || got.result.Source != job.Source {
			t.Fatalf("owner result=%#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("matching stage/source result was not routed")
	}
}

func TestPoolRouterForeignLogsDoNotExposePaths(t *testing.T) {
	pool := newPhase2FakePool()
	var output synchronizedBuffer
	router := NewPoolRouter(pool, slog.New(slog.NewJSONHandler(&output, nil)))
	path := `D:\private\customer-album\secret-name.jpg`
	job := worker.JobMsg{
		JobID: router.NextJobID(), ScanTaskID: "private-log", Path: path,
		Kind: worker.MediaImage, Phase: worker.Phase2,
		ScreenStage: worker.ScreenStageThree, Source: worker.JobSourceManager,
	}
	_, cancel, err := router.Register(&job)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	pool.results <- &worker.JobResultMsg{
		JobID: job.JobID, ScanTaskID: job.ScanTaskID, Path: path + ".foreign", Kind: job.Kind,
		Phase: job.Phase, ScreenStage: job.ScreenStage, Source: job.Source,
	}
	pool.crashes <- worker.CrashRecord{JobID: job.JobID, ScanTaskID: job.ScanTaskID, File: path + ".foreign"}
	var logged string
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		logged = output.String()
		if strings.Count(logged, worker.PathID(path+".foreign")) == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if strings.Contains(logged, "customer-album") || strings.Contains(logged, "secret-name.jpg") {
		t.Fatalf("router log leaked sensitive path: %s", logged)
	}
	if !strings.Contains(logged, worker.PathID(path+".foreign")) || !strings.Contains(logged, `"screen_stage":3`) || !strings.Contains(logged, `"source":"manager"`) {
		t.Fatalf("router log missing safe identity context: %s", logged)
	}
}

type synchronizedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *synchronizedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(data)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

type phase2FakePool struct {
	mu           sync.Mutex
	submitted    []worker.JobMsg
	results      chan *worker.JobResultMsg
	crashes      chan worker.CrashRecord
	resultsCalls int
	crashesCalls int
	endTasks     map[string]int
	onSubmit     func(worker.JobMsg)
	submitErr    error
}

func newPhase2FakePool() *phase2FakePool {
	return &phase2FakePool{
		results:  make(chan *worker.JobResultMsg, 64),
		crashes:  make(chan worker.CrashRecord, 64),
		endTasks: make(map[string]int),
	}
}

func (p *phase2FakePool) Submit(job *worker.JobMsg) error {
	if p.submitErr != nil {
		return p.submitErr
	}
	copy := *job
	copy.KnownSHA = append([]byte(nil), job.KnownSHA...)
	p.mu.Lock()
	p.submitted = append(p.submitted, copy)
	onSubmit := p.onSubmit
	p.mu.Unlock()
	if onSubmit != nil {
		onSubmit(copy)
	}
	return nil
}

func (p *phase2FakePool) Results() <-chan *worker.JobResultMsg {
	p.mu.Lock()
	p.resultsCalls++
	p.mu.Unlock()
	return p.results
}

func (p *phase2FakePool) Crashes() <-chan worker.CrashRecord {
	p.mu.Lock()
	p.crashesCalls++
	p.mu.Unlock()
	return p.crashes
}

func (p *phase2FakePool) Metrics() worker.MetricsSnapshot {
	return worker.MetricsSnapshot{}
}

func (p *phase2FakePool) EndTask(taskID string) {
	p.mu.Lock()
	p.endTasks[taskID]++
	p.mu.Unlock()
}

func (p *phase2FakePool) submittedSnapshot() []worker.JobMsg {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]worker.JobMsg(nil), p.submitted...)
}
