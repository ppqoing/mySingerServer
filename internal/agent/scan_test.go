package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"dedup/internal/config"
	"dedup/internal/diskio"
	fileenum "dedup/internal/enum"
	"dedup/internal/proto"
	"dedup/internal/store"
	"dedup/internal/worker"
)

func TestScanManagerRejectsInvalidTasks(t *testing.T) {
	manager, cleanup := newTestScanManager(t, nil, nil)
	defer cleanup()
	for _, task := range []proto.ScanTask{
		{TaskID: "phase-2", Roots: []string{`D:\media`}, Phase: 2},
		{TaskID: "empty", Phase: 1},
		{TaskID: "", Roots: []string{`D:\media`}, Phase: 1},
	} {
		ack := manager.Handle(task, nil)
		if ack.Accepted || ack.Reason == "" {
			t.Fatalf("Handle(%#v) = %#v, want rejected reason", task, ack)
		}
	}
}

func TestScanReportErrLogsSafePathIdentityAcrossWindowsVariants(t *testing.T) {
	var output bytes.Buffer
	manager := &ScanManager{errLog: slog.New(slog.NewJSONHandler(&output, nil))}
	path := `D:\İstanbul\Private\Album\Secret.JPG`
	message := `open d:/İstanbul\PRIVATE/ALBUM\SECRET.jpg failed; retry SECRET.JPG`
	state := &ScanState{Task: proto.ScanTask{TaskID: "scan-private-log"}}
	responses := make(chan proto.Error, 1)
	state.bindSender(func(msgType uint8, value any) error {
		if msgType == proto.MsgError {
			responses <- *value.(*proto.Error)
		}
		return nil
	})

	manager.reportErr(state, path, "hash", errors.New(message))
	logged := output.String()
	for _, secret := range []string{"stanbul", "private", "album", "secret.jpg"} {
		if strings.Contains(strings.ToLower(logged), secret) {
			t.Fatalf("scan error log leaked %q from Windows path variant: %s", secret, logged)
		}
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	if record["path_id"] != worker.PathID(path) || record["screen_stage"] != float64(worker.ScreenStageLegacy) || record["source"] != string(worker.JobSourceScan) {
		t.Fatalf("scan safe log context=%#v", record)
	}
	select {
	case response := <-responses:
		if response.Path != path || response.Msg != message || response.Stage != "hash" {
			t.Fatalf("authorized protocol response=%#v", response)
		}
	default:
		t.Fatal("scan reportErr emitted no authorized protocol response")
	}
}

func TestScanTaskResumesAndCompletesWithoutRestarting(t *testing.T) {
	hashStarted := make(chan struct{})
	releaseHash := make(chan struct{})
	var once sync.Once
	hasher := hasherFunc(func(string) (string, error) {
		once.Do(func() { close(hashStarted) })
		<-releaseHash
		return "hash", nil
	})
	enumr := &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\media`: {{Path: `D:\media\a.bin`, Size: 1, MTime: 100}},
	}}
	manager, cleanup := newTestScanManager(t, enumr, hasher)
	defer cleanup()

	done := make(chan proto.TaskDone, 1)
	sender := captureTaskDone(done)
	task := proto.ScanTask{TaskID: "task-1", Roots: []string{`D:\media`}, Phase: 1}
	if ack := manager.Handle(task, sender); !ack.Accepted || ack.Reason != "accepted" {
		t.Fatalf("first ack = %#v", ack)
	}
	select {
	case <-hashStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("hash did not start")
	}
	if ack := manager.Handle(task, sender); !ack.Accepted || ack.Reason != "resumed" {
		t.Fatalf("resume ack = %#v", ack)
	}
	close(releaseHash)
	select {
	case result := <-done:
		if result.Stats.Done != 1 || result.Stats.Failed != 0 {
			t.Fatalf("TaskDone stats = %#v", result.Stats)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not finish")
	}
	if enumr.callCount() != 1 {
		t.Fatalf("enumerator calls = %d, want 1", enumr.callCount())
	}
	if ack := manager.Handle(task, sender); !ack.Accepted || ack.Reason != "already_done" {
		t.Fatalf("done ack = %#v", ack)
	}
}

func TestCompletedScanAckCarriesFinalStats(t *testing.T) {
	enumr := &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\media`: {{Path: `D:\media\a.bin`, Size: 1, MTime: 100}},
	}}
	manager, cleanup := newTestScanManager(t, enumr, nil)
	defer cleanup()
	done := make(chan proto.TaskDone, 1)
	task := proto.ScanTask{
		TaskID: "task-completed-ack", Roots: []string{`D:\media`}, Phase: 1,
	}
	if ack := manager.Handle(task, captureTaskDone(done)); !ack.Accepted {
		t.Fatalf("first ack = %#v", ack)
	}
	var final proto.TaskDone
	select {
	case final = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not finish")
	}
	if final.Reason != "" {
		t.Fatalf("natural completion reason=%q", final.Reason)
	}

	ack := manager.Handle(task, nil)
	if ack.Reason != "already_done" || ack.Stats == nil ||
		*ack.Stats != final.Stats {
		t.Fatalf("completed ack = %#v, final = %#v", ack, final.Stats)
	}
}

// Break caught: a completed cache entry keyed only by task ID rejected or
// skipped a newly-created instance before the ten-minute cache expiry.
func TestScanNewInstanceWithReusedTaskIDStartsBeforeOldCacheExpires(t *testing.T) {
	for _, test := range []struct {
		name       string
		secondRoot string
	}{
		{name: "same envelope", secondRoot: `D:\media`},
		{name: "different envelope", secondRoot: `E:\other`},
	} {
		t.Run(test.name, func(t *testing.T) {
			enumr := &fakeEnumerator{records: map[string][]fileenum.FileRecord{}}
			manager, cleanup := newTestScanManager(t, enumr, nil)
			defer cleanup()
			done := make(chan proto.TaskDone, 2)
			first := proto.ScanTask{
				TaskID: "reused-task", InstanceID: "instance-old",
				Roots: []string{`D:\media`}, Phase: 1,
			}
			if ack := manager.Handle(first, captureTaskDone(done)); !ack.Accepted {
				t.Fatalf("first ack=%#v", ack)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("first instance did not finish")
			}

			second := first
			second.InstanceID = "instance-new"
			second.Roots = []string{test.secondRoot}
			ack, start := manager.Prepare(second, captureTaskDone(done))
			if !ack.Accepted || ack.Reason != "accepted" || start == nil {
				t.Fatalf("new instance ack=%#v start=%v", ack, start != nil)
			}
			start()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("new instance did not finish")
			}
			if enumr.callCount() != 2 {
				t.Fatalf("enumerator calls=%d want=2", enumr.callCount())
			}
		})
	}
}

// Break caught: run wrote the multiword startedAt value without the state lock
// while an immediately accepted Drain read it under the lock.
func TestScanStartAndImmediateDrainSynchronizeStartedAt(t *testing.T) {
	releaseEnumeration := make(chan struct{})
	enumr := releaseScanEnumerator{release: releaseEnumeration}
	manager, cleanup := newTestScanManager(t, enumr, nil)
	defer cleanup()
	task := proto.ScanTask{
		TaskID: "start-drain-race", InstanceID: "instance-race",
		Roots: []string{`D:\media`}, Phase: 1,
	}
	state := newScanState(task)
	done := make(chan proto.TaskDone, 1)
	state.bindSender(captureTaskDone(done))
	manager.mu.Lock()
	manager.tasks[scanTaskIdentity(task)] = state
	manager.mu.Unlock()

	start := make(chan struct{})
	runDone := make(chan struct{})
	drainDone := make(chan struct{})
	go func() {
		<-start
		manager.run(state)
		close(runDone)
	}()
	go func() {
		<-start
		accepted, _ := manager.DrainInstance(task.TaskID, task.InstanceID, proto.TaskDrainPause)
		if !accepted {
			t.Error("immediate drain was not accepted")
		}
		close(drainDone)
	}()
	close(start)
	<-drainDone
	close(releaseEnumeration)
	<-runDone
	final := <-done
	if final.Reason != proto.TaskDrainPause {
		t.Fatalf("TaskDone reason=%q", final.Reason)
	}
}

func TestScanDrainStopsDispatchAndWaitsForInFlightResult(t *testing.T) {
	enumr := &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\media`: {
			{Path: `D:\media\a.jpg`, Size: 10, MTime: 20},
			{Path: `D:\media\b.jpg`, Size: 11, MTime: 21},
		},
	}}
	manager, cleanup := newTestScanManager(t, enumr, nil)
	defer cleanup()
	manager.cfg.Scan.HDDStreams = 1
	pool := newFakeScanPool()
	submitted := make(chan worker.JobMsg, 1)
	pool.onSubmit = func(job worker.JobMsg) { submitted <- job }
	manager.pool = pool
	manager.router = NewPoolRouter(pool, nil)
	done := make(chan proto.TaskDone, 1)
	items := make(chan proto.FeatureItem, 2)
	task := proto.ScanTask{TaskID: "task-drain", Roots: []string{`D:\media`}, Phase: 1}
	sender := func(messageType uint8, value any) error {
		switch messageType {
		case proto.MsgFeatureResult:
			for _, item := range value.(*proto.FeatureResult).Items {
				items <- item
			}
		case proto.MsgTaskDone:
			done <- *value.(*proto.TaskDone)
		}
		return nil
	}
	if ack := manager.Handle(task, sender); !ack.Accepted {
		t.Fatalf("ack=%#v", ack)
	}
	job := <-submitted
	accepted, _ := manager.Drain(task.TaskID, proto.TaskDrainPause)
	if !accepted {
		t.Fatal("drain was not accepted")
	}
	if acceptedAgain, _ := manager.Drain(task.TaskID, proto.TaskDrainPause); !acceptedAgain {
		t.Fatal("second drain was not idempotently accepted")
	}
	if got := len(pool.submittedSnapshot()); got != 1 {
		t.Fatalf("submissions after drain=%d want1", got)
	}
	select {
	case terminal := <-done:
		t.Fatalf("TaskDone arrived before in-flight result: %#v", terminal)
	default:
	}
	pool.results <- &worker.JobResultMsg{
		JobID: job.JobID, ScanTaskID: job.ScanTaskID, Path: job.Path,
		Kind: job.Kind, Phase: job.Phase, Source: job.Source,
		FieldsDone: job.FieldsMask, SHA512: bytes.Repeat([]byte{0x44}, 64),
	}
	select {
	case final := <-done:
		if final.Reason != proto.TaskDrainPause || final.Stats.Done != 1 {
			t.Fatalf("TaskDone=%#v", final)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drained scan did not finish")
	}
	select {
	case item := <-items:
		if item.Path != job.Path || item.Status != proto.StatusDone {
			t.Fatalf("in-flight item=%#v", item)
		}
	default:
		t.Fatal("in-flight result was not published before TaskDone")
	}
	manager.router.mu.Lock()
	routes := len(manager.router.routes)
	manager.router.mu.Unlock()
	if routes != 0 {
		t.Fatalf("registered routes after drain=%d", routes)
	}
	resume, start := manager.Prepare(task, sender)
	if !resume.Accepted || resume.Reason != "resumed" || start == nil {
		t.Fatalf("drained task resume=%#v start=%v", resume, start != nil)
	}
	start()
	sawRemaining := false
	for range 2 {
		var resumedJob worker.JobMsg
		select {
		case resumedJob = <-submitted:
		case <-time.After(5 * time.Second):
			t.Fatal("resumed scan did not dispatch remaining work")
		}
		sawRemaining = sawRemaining || resumedJob.Path != job.Path
		pool.results <- &worker.JobResultMsg{
			JobID: resumedJob.JobID, ScanTaskID: resumedJob.ScanTaskID, Path: resumedJob.Path,
			Kind: resumedJob.Kind, Phase: resumedJob.Phase, Source: resumedJob.Source,
			FieldsDone: resumedJob.FieldsMask, SHA512: bytes.Repeat([]byte{0x55}, 64),
		}
	}
	if !sawRemaining {
		t.Fatal("resumed scan did not start the previously undispatched file")
	}
	select {
	case final := <-done:
		if final.Reason != "" || final.Stats.Done != 2 {
			t.Fatalf("resumed TaskDone=%#v", final)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resumed scan did not finish")
	}
}

func TestScanHDDPreservesCategoryBoundariesAndImageOrderWithOverlap(t *testing.T) {
	enumr := &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\media`: {
			{Path: `D:\media\a-video.mp4`, Size: 1 << 30, MTime: 20},
			{Path: `D:\media\b-large.jpg`, Size: 200 << 20, MTime: 21},
			{Path: `D:\media\c-small.jpg`, Size: 96_728, MTime: 22},
			{Path: `D:\media\d-other.txt`, Size: 100, MTime: 23},
		},
	}}
	hashEntered := make(chan string, 1)
	releaseHash := make(chan struct{})
	manager, cleanup := newTestScanManager(t, enumr, hasherFunc(func(path string) (string, error) {
		hashEntered <- path
		<-releaseHash
		return strings.Repeat("ab", 64), nil
	}))
	defer cleanup()
	manager.cfg.Scan.HDDStreams = 2
	pool := newFakeScanPool()
	submitted := make(chan worker.JobMsg, 3)
	pool.onSubmit = func(job worker.JobMsg) { submitted <- job }
	manager.pool = pool
	manager.router = NewPoolRouter(pool, nil)
	done := make(chan proto.TaskDone, 1)
	task := proto.ScanTask{TaskID: "task-hdd-media-order", Roots: []string{`D:\media`}, Phase: 1}
	if ack := manager.Handle(task, captureTaskDone(done)); !ack.Accepted {
		t.Fatalf("ack=%#v", ack)
	}

	complete := func(job worker.JobMsg) {
		result := &worker.JobResultMsg{
			JobID: job.JobID, ScanTaskID: job.ScanTaskID, Path: job.Path,
			Kind: job.Kind, Phase: job.Phase, Source: job.Source,
			FieldsDone: job.FieldsMask, SHA512: bytes.Repeat([]byte{0x6a}, 64),
		}
		if job.Kind == worker.MediaImage {
			result.PDQ = bytes.Repeat([]byte{0x3c}, 32)
			result.Quality, result.Width, result.Height = 90, 1280, 1280
		} else {
			duration, quality := int64(60_000), int32(90)
			result.DurationMS = &duration
			result.ThumbPath = `D:\cache\sheet.jpg`
			result.ThumbPDQ = bytes.Repeat([]byte{0x4d}, 32)
			result.ThumbQuality = &quality
			result.ContactSheetWidth, result.ContactSheetHeight = 768, 512
		}
		pool.results <- result
	}
	images := []worker.JobMsg{
		receiveSubmittedScanJob(t, submitted),
		receiveSubmittedScanJob(t, submitted),
	}
	if images[0].Path != `D:\media\c-small.jpg` || images[1].Path != `D:\media\b-large.jpg` {
		t.Fatalf("image submission order=[%q %q]", images[0].Path, images[1].Path)
	}
	select {
	case unexpected := <-submitted:
		t.Fatalf("later category crossed unfinished image batch: %#v", unexpected)
	default:
	}
	complete(images[0])
	complete(images[1])
	select {
	case path := <-hashEntered:
		if path != `D:\media\d-other.txt` {
			t.Fatalf("other category path=%q", path)
		}
	case <-time.After(time.Second):
		t.Fatal("other category did not start after images")
	}
	select {
	case unexpected := <-submitted:
		t.Fatalf("video crossed unfinished other category: %#v", unexpected)
	default:
	}
	close(releaseHash)
	video := receiveSubmittedScanJob(t, submitted)
	if video.Path != `D:\media\a-video.mp4` {
		t.Fatalf("video submission path=%q", video.Path)
	}
	complete(video)
	select {
	case final := <-done:
		if final.Reason != "" || final.Stats.Failed != 0 {
			t.Fatalf("TaskDone=%#v", final)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HDD media scan did not finish")
	}
}

func TestScanDrainFlushesEnumeratedTailWithWorkContext(t *testing.T) {
	enumr := &barrierEnumerator{
		first:   fileenum.FileRecord{Path: `D:\media\tail.bin`, Size: 10, MTime: 20},
		second:  fileenum.FileRecord{Path: `D:\media\after.bin`, Size: 11, MTime: 21},
		visited: make(chan struct{}), release: make(chan struct{}),
	}
	manager, cleanup := newTestScanManager(t, enumr, nil)
	defer cleanup()
	done := make(chan proto.TaskDone, 1)
	task := proto.ScanTask{TaskID: "task-tail-drain", Roots: []string{`D:\media`}, Phase: 1}
	manager.Handle(task, captureTaskDone(done))
	<-enumr.visited
	if accepted, _ := manager.Drain(task.TaskID, proto.TaskDrainStop); !accepted {
		t.Fatal("drain was not accepted")
	}
	close(enumr.release)
	select {
	case final := <-done:
		if final.Reason != proto.TaskDrainStop {
			t.Fatalf("reason=%q", final.Reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drained enumeration did not finish")
	}
	pending, err := manager.st.PendingSnapshot(context.Background(), manager.cfg.MachineID)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, files := range pending {
		for _, file := range files {
			paths = append(paths, file.Path)
		}
	}
	if !reflect.DeepEqual(paths, []string{`D:\media\tail.bin`}) {
		t.Fatalf("durable enumerated tail=%v", paths)
	}
}

func TestScanDrainAfterPendingSnapshotDoesNotCountPendingAsCached(t *testing.T) {
	enumr := &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\media`: {{Path: `D:\media\pending.bin`, Size: 10, MTime: 20}},
	}}
	manager, cleanup := newTestScanManager(t, enumr, nil)
	defer cleanup()
	pendingReady := make(chan struct{})
	releasePending := make(chan struct{})
	manager.pendingSnapshot = func(ctx context.Context, machineID string) (map[int64][]store.PendingFile, error) {
		pending, err := manager.st.PendingSnapshot(ctx, machineID)
		close(pendingReady)
		<-releasePending
		return pending, err
	}
	done := make(chan proto.TaskDone, 1)
	task := proto.ScanTask{TaskID: "task-pending-window", Roots: []string{`D:\media`}, Phase: 1}
	manager.Handle(task, captureTaskDone(done))
	<-pendingReady
	if accepted, _ := manager.Drain(task.TaskID, proto.TaskDrainPause); !accepted {
		t.Fatal("drain was not accepted")
	}
	close(releasePending)
	select {
	case final := <-done:
		if final.Reason != proto.TaskDrainPause || final.Stats.Total != 1 ||
			final.Stats.Done != 0 || final.Stats.Skipped != 0 {
			t.Fatalf("drain-window TaskDone=%#v", final)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drain-window scan did not finish")
	}
}

func TestScanJoinsProgressLoopBeforeFinalProgressAndTaskDone(t *testing.T) {
	hashStarted := make(chan struct{})
	releaseHash := make(chan struct{})
	hasher := hasherFunc(func(string) (string, error) {
		close(hashStarted)
		<-releaseHash
		return "hash", nil
	})
	enumr := &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\media`: {{Path: `D:\media\progress.bin`, Size: 10, MTime: 20}},
	}}
	manager, cleanup := newTestScanManager(t, enumr, hasher)
	defer cleanup()
	ticks := make(chan time.Time, 1)
	manager.progressTicks = func() (<-chan time.Time, func()) {
		return ticks, func() {}
	}
	loopEntered := make(chan struct{})
	releaseLoop := make(chan struct{})
	featureSent := make(chan struct{})
	done := make(chan proto.TaskDone, 1)
	var mu sync.Mutex
	progressCalls := 0
	messageOrder := make([]uint8, 0, 5)
	sender := func(msgType uint8, value any) error {
		if msgType == proto.MsgTaskProgress {
			mu.Lock()
			progressCalls++
			call := progressCalls
			mu.Unlock()
			if call == 2 {
				close(loopEntered)
				<-releaseLoop
			}
		}
		mu.Lock()
		messageOrder = append(messageOrder, msgType)
		mu.Unlock()
		switch msgType {
		case proto.MsgFeatureResult:
			close(featureSent)
		case proto.MsgTaskDone:
			done <- *value.(*proto.TaskDone)
		}
		return nil
	}
	manager.Handle(proto.ScanTask{TaskID: "task-progress-join", Roots: []string{`D:\media`}, Phase: 1}, sender)
	<-hashStarted
	ticks <- time.Now()
	<-loopEntered
	close(releaseHash)
	<-featureSent
	select {
	case final := <-done:
		t.Fatalf("TaskDone overtook blocked progress send: %#v", final)
	case <-time.After(250 * time.Millisecond):
	}
	close(releaseLoop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not finish after progress send was released")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(messageOrder) == 0 || messageOrder[len(messageOrder)-1] != proto.MsgTaskDone {
		t.Fatalf("message order=%v, want TaskDone last", messageOrder)
	}
}

func TestScanDrainPreservesTerminalReason(t *testing.T) {
	for _, reason := range []proto.TaskDrainReason{proto.TaskDrainPause, proto.TaskDrainStop, proto.TaskDrainDelete, proto.TaskDrainProcessShutdown} {
		t.Run(string(reason), func(t *testing.T) {
			manager, cleanup := newTestScanManager(t, nil, nil)
			defer cleanup()
			done := make(chan proto.TaskDone, 1)
			task := proto.ScanTask{TaskID: "task-reason-" + string(reason), Roots: []string{`D:\media`}, Phase: 1}
			ack, start := manager.Prepare(task, captureTaskDone(done))
			if !ack.Accepted {
				t.Fatalf("ack=%#v", ack)
			}
			if accepted, _ := manager.Drain(task.TaskID, reason); !accepted {
				t.Fatal("drain was not accepted before start")
			}
			start()
			select {
			case final := <-done:
				if final.Reason != reason {
					t.Fatalf("reason=%q want=%q", final.Reason, reason)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("scan did not finish")
			}
		})
	}
}

func TestScanManagerRejectsTaskIDReusedWithDifferentEnvelope(t *testing.T) {
	hashStarted := make(chan struct{})
	releaseHash := make(chan struct{})
	var once sync.Once
	hasher := hasherFunc(func(string) (string, error) {
		once.Do(func() { close(hashStarted) })
		<-releaseHash
		return "hash", nil
	})
	enumr := &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\one`: {{Path: `D:\one\a.bin`, Size: 1, MTime: 100}},
	}}
	manager, cleanup := newTestScanManager(t, enumr, hasher)
	defer cleanup()

	original := proto.ScanTask{
		TaskID: "task-envelope", Roots: []string{`D:\one`}, Phase: 1,
	}
	if ack := manager.Handle(original, nil); !ack.Accepted {
		t.Fatalf("original ack = %#v", ack)
	}
	select {
	case <-hashStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("hash did not start")
	}
	conflict := original
	conflict.Roots = []string{`D:\two`}
	ack := manager.Handle(conflict, nil)
	if ack.Accepted || !strings.Contains(ack.Reason, "envelope mismatch") {
		t.Fatalf("conflicting ack = %#v, want rejection", ack)
	}
	close(releaseHash)
}

func TestPreparedScanStartsOnlyAfterAckAndCanStartOnResume(t *testing.T) {
	hashStarted := make(chan struct{})
	hasher := hasherFunc(func(string) (string, error) {
		close(hashStarted)
		return "hash", nil
	})
	enumr := &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\one`: {{Path: `D:\one\a.bin`, Size: 1, MTime: 100}},
	}}
	manager, cleanup := newTestScanManager(t, enumr, hasher)
	defer cleanup()
	task := proto.ScanTask{
		TaskID: "task-prepared", Roots: []string{`D:\one`}, Phase: 1,
	}

	ack, _ := manager.Prepare(task, nil)
	if !ack.Accepted || ack.Reason != "accepted" {
		t.Fatalf("prepared ack = %#v", ack)
	}
	select {
	case <-hashStarted:
		t.Fatal("scan started before ACK callback")
	case <-time.After(50 * time.Millisecond):
	}

	ack, start := manager.Prepare(task, nil)
	if !ack.Accepted || ack.Reason != "resumed" || start == nil {
		t.Fatalf("resumed prepared ack = %#v start=%v", ack, start != nil)
	}
	start()
	select {
	case <-hashStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("resumed prepared scan did not start")
	}
}

func TestScanContinuesAfterHashFailureAndOnlyIncludesRequestedRoot(t *testing.T) {
	enumr := &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\one`: {
			{Path: `D:\one\a.bin`, Size: 10, MTime: 10},
			{Path: `D:\one\bad.bin`, Size: 20, MTime: 20},
		},
	}}
	hasher := hasherFunc(func(path string) (string, error) {
		if filepath.Base(path) == "bad.bin" {
			return "", errors.New("access denied")
		}
		return "goodhash", nil
	})
	manager, cleanup := newTestScanManager(t, enumr, hasher)
	defer cleanup()

	// Seed a pending file outside the requested root. A D:\one scan must not
	// accidentally consume unrelated backlog from D:\two.
	if err := manager.st.UpsertEnumerated(context.Background(), []store.EnumUpsert{{
		MachineID: "machine-a", DiskNo: 1, Path: `D:\two\pending.bin`,
		Size: 1, MTime: 1, MissingBase: proto.FieldSHA512,
	}, {
		MachineID: "machine-a", DiskNo: 1, Path: `D:\one\stale.bin`,
		Size: 1, MTime: 1, MissingBase: proto.FieldSHA512,
	}}); err != nil {
		t.Fatal(err)
	}

	var messagesMu sync.Mutex
	var messages []any
	done := make(chan proto.TaskDone, 1)
	sender := func(msgType uint8, value any) error {
		messagesMu.Lock()
		messages = append(messages, value)
		messagesMu.Unlock()
		if msgType == proto.MsgTaskDone {
			done <- *value.(*proto.TaskDone)
		}
		return nil
	}
	task := proto.ScanTask{TaskID: "task-root", Roots: []string{`D:\one`}, Phase: 1}
	if ack := manager.Handle(task, sender); !ack.Accepted {
		t.Fatalf("ack = %#v", ack)
	}
	select {
	case result := <-done:
		if result.Stats.Done != 2 || result.Stats.Failed != 1 {
			t.Fatalf("stats = %#v", result.Stats)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not finish")
	}

	pending, err := manager.st.PendingSnapshot(context.Background(), "machine-a")
	if err != nil {
		t.Fatal(err)
	}
	var outsideStillPending, staleStillPending bool
	for _, files := range pending {
		for _, file := range files {
			if file.Path == `D:\two\pending.bin` {
				outsideStillPending = true
			}
			if file.Path == `D:\one\stale.bin` {
				staleStillPending = true
			}
		}
	}
	if !outsideStillPending {
		t.Fatal("scan consumed a pending file outside its requested root")
	}
	if !staleStillPending {
		t.Fatal("scan consumed a stale database path not seen by this enumeration")
	}

	messagesMu.Lock()
	defer messagesMu.Unlock()
	var sawError, sawGood, sawBad bool
	for _, message := range messages {
		switch value := message.(type) {
		case *proto.Error:
			sawError = value.Path == `D:\one\bad.bin` && value.Stage == "hash"
		case *proto.FeatureResult:
			for _, item := range value.Items {
				if item.Path == `D:\one\a.bin` && item.Status == proto.StatusDone &&
					item.Size == 10 && item.MTime == 10 {
					sawGood = true
				}
				if item.Path == `D:\one\bad.bin` && item.Status == proto.StatusFailed {
					sawBad = true
				}
			}
		}
	}
	if !sawError || !sawGood || !sawBad {
		t.Fatalf("messages missing error/good/bad result: %#v", messages)
	}
}

func TestScanDoesNotReportDurableSuccessWhenResultCommitFails(t *testing.T) {
	enumr := &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\media`: {{Path: `D:\media\a.bin`, Size: 10, MTime: 20}},
	}}
	var manager *ScanManager
	hasher := hasherFunc(func(string) (string, error) {
		if err := manager.st.Close(); err != nil {
			return "", err
		}
		return "hash-not-persisted", nil
	})
	var cleanup func()
	manager, cleanup = newTestScanManager(t, enumr, hasher)
	defer cleanup()

	var messagesMu sync.Mutex
	var messages []any
	done := make(chan proto.TaskDone, 1)
	sender := func(msgType uint8, value any) error {
		messagesMu.Lock()
		messages = append(messages, value)
		messagesMu.Unlock()
		if msgType == proto.MsgTaskDone {
			done <- *value.(*proto.TaskDone)
		}
		return nil
	}
	task := proto.ScanTask{
		TaskID: "task-store-failure", Roots: []string{`D:\media`}, Phase: 1,
	}
	if ack := manager.Handle(task, sender); !ack.Accepted {
		t.Fatalf("ack = %#v", ack)
	}
	select {
	case result := <-done:
		if result.Stats.Failed != 1 || result.Stats.ScanErrors != 1 {
			t.Fatalf("TaskDone stats = %#v, want one persistence failure", result.Stats)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not finish")
	}

	messagesMu.Lock()
	defer messagesMu.Unlock()
	var sawStoreError, sawFailedResult, sawFalseSuccess bool
	for _, message := range messages {
		switch value := message.(type) {
		case *proto.Error:
			sawStoreError = sawStoreError || value.Stage == "store"
		case *proto.FeatureResult:
			for _, item := range value.Items {
				if item.Path != `D:\media\a.bin` {
					continue
				}
				sawFailedResult = item.Status == proto.StatusFailed && item.Err != ""
				sawFalseSuccess = item.Status == proto.StatusDone
			}
		}
	}
	if !sawStoreError || !sawFailedResult || sawFalseSuccess {
		t.Fatalf(
			"messages storeError=%v failedResult=%v falseSuccess=%v: %#v",
			sawStoreError,
			sawFailedResult,
			sawFalseSuccess,
			messages,
		)
	}
}

func TestScanCountsDiskResolutionFailure(t *testing.T) {
	manager, cleanup := newTestScanManager(t, nil, nil)
	defer cleanup()
	manager.resolver = func(string) (diskio.Identity, error) {
		return diskio.Identity{}, errors.New("volume unavailable")
	}

	done := make(chan proto.TaskDone, 1)
	task := proto.ScanTask{
		TaskID: "task-bad-volume", Roots: []string{`Z:\missing`}, Phase: 1,
	}
	if ack := manager.Handle(task, captureTaskDone(done)); !ack.Accepted {
		t.Fatalf("ack = %#v", ack)
	}
	select {
	case result := <-done:
		if result.Stats.Failed != 1 || result.Stats.ScanErrors != 1 ||
			result.Stats.Total != 0 {
			t.Fatalf("TaskDone stats = %#v, want one root failure", result.Stats)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not finish")
	}
}

func TestUnknownDiskMediaLogDoesNotExposeRootOrMount(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	root := `D:\Private\Media`
	mount := `D:\Private`
	logUnknownDiskMedia(logger, root, mount, 7)
	got := output.String()
	for _, value := range []string{"Private", "Media", root, mount} {
		if strings.Contains(got, value) {
			t.Fatalf("log leaked %q: %q", value, got)
		}
	}
	if !strings.Contains(got, "path_id") || !strings.Contains(got, "device_number=7") {
		t.Fatalf("safe fields missing: %q", got)
	}
}

func TestScanCountsEnumerationFailure(t *testing.T) {
	enumr := &fakeEnumerator{
		records: map[string][]fileenum.FileRecord{
			`D:\media`: {{Path: `D:\media\partial.bin`, Size: 1, MTime: 1}},
		},
		errors: map[string]error{
			`D:\media`: errors.New("walk interrupted"),
		},
	}
	manager, cleanup := newTestScanManager(t, enumr, nil)
	defer cleanup()

	done := make(chan proto.TaskDone, 1)
	task := proto.ScanTask{
		TaskID: "task-enum-failure", Roots: []string{`D:\media`}, Phase: 1,
	}
	if ack := manager.Handle(task, captureTaskDone(done)); !ack.Accepted {
		t.Fatalf("ack = %#v", ack)
	}
	select {
	case result := <-done:
		if result.Stats.Failed != 1 || result.Stats.ScanErrors != 1 {
			t.Fatalf("TaskDone stats = %#v, want one enumeration failure", result.Stats)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not finish")
	}
}

func TestFailedOldSenderDoesNotUnbindNewlyResumedConnection(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	newSenderCalled := make(chan struct{}, 1)
	makeSender := func(old bool) Sender {
		return func(uint8, any) error {
			if old {
				close(started)
				<-release
				return errors.New("old connection closed")
			}
			newSenderCalled <- struct{}{}
			return nil
		}
	}
	state := &ScanState{}
	state.bindSender(makeSender(true))
	oldDone := make(chan struct{})
	go func() {
		defer close(oldDone)
		state.send(proto.MsgTaskProgress, &proto.TaskProgress{})
	}()
	<-started
	state.bindSender(makeSender(false))
	close(release)
	<-oldDone
	state.send(proto.MsgTaskProgress, &proto.TaskProgress{})
	select {
	case <-newSenderCalled:
	case <-time.After(time.Second):
		t.Fatal("old sender failure cleared the newly resumed sender")
	}
}

func TestFeatureResultSequenceAllocationAndSendAreLinearized(t *testing.T) {
	state := &ScanState{Task: proto.ScanTask{TaskID: "task-seq"}}
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	received := make(chan uint64, 2)
	state.bindSender(func(msgType uint8, value any) error {
		if msgType != proto.MsgFeatureResult {
			return nil
		}
		sequence := value.(*proto.FeatureResult).Seq
		if sequence == 1 {
			close(firstEntered)
			<-releaseFirst
		}
		received <- sequence
		return nil
	})
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		state.publishFeatures([]proto.FeatureItem{{Path: "first"}})
	}()
	<-firstEntered
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		state.publishFeatures([]proto.FeatureItem{{Path: "second"}})
	}()
	select {
	case sequence := <-received:
		t.Fatalf("sequence %d was sent before sequence 1 completed", sequence)
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseFirst)
	<-firstDone
	<-secondDone
	if first, second := <-received, <-received; first != 1 || second != 2 {
		t.Fatalf("send order = [%d %d], want [1 2]", first, second)
	}
}

func TestExtensionFilterIsCaseInsensitive(t *testing.T) {
	if !extIn(`D:\photo.JPG`, []string{".jpg"}) {
		t.Fatal("uppercase extension did not match lowercase filter")
	}
	if extIn(`D:\photo.png`, []string{".jpg"}) {
		t.Fatal("wrong extension matched")
	}
}

func TestScanRoutesMediaToPoolWithMissingMaskAndKnownSHAWhileOtherFilesUseGoHasher(t *testing.T) {
	knownSHAHex := strings.Repeat("ab", 64)
	enumr := &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\media`: {
			{Path: `D:\media\known.jpg`, Size: 10, MTime: 20},
			{Path: `D:\media\clip.mp4`, Size: 30, MTime: 40},
			{Path: `D:\media\plain.txt`, Size: 50, MTime: 60},
		},
	}}
	var hashed []string
	manager, cleanup := newTestScanManager(t, enumr, hasherFunc(func(path string) (string, error) {
		hashed = append(hashed, path)
		return strings.Repeat("cd", 64), nil
	}))
	defer cleanup()
	if err := manager.st.UpsertEnumerated(context.Background(), []store.EnumUpsert{{
		MachineID: "machine-a", DiskNo: 1, Path: `D:\media\known.jpg`,
		Size: 10, MTime: 20, MissingBase: proto.FieldSHA512 | proto.FieldPDQ256,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.st.ApplyHashResults(context.Background(), "machine-a", []store.HashResult{{
		Path: `D:\media\known.jpg`, SHA512: knownSHAHex,
	}}); err != nil {
		t.Fatal(err)
	}

	pool := newFakeScanPool()
	pool.addMetrics(worker.MetricsSnapshot{
		FilesDone: 40, FilesFailed: 30, DecodeCalls: 20,
		ReadAttempts: 50, DecodeAttempts: 20,
		ReadNS: 100_000_000, DecodeNS: 200_000_000,
		ThumbGenerated: 10, ThumbCacheHits: 9,
		SingleFlightHits: 8, Crashes: 7,
	})
	pool.onSubmit = func(job worker.JobMsg) {
		result := &worker.JobResultMsg{
			JobID: job.JobID, ScanTaskID: job.ScanTaskID,
			Path: job.Path, Kind: job.Kind, Phase: job.Phase,
			SHA512:     append([]byte(nil), job.KnownSHA...),
			FieldsDone: job.FieldsMask,
		}
		if job.FieldsMask&worker.MaskSHA512 != 0 {
			result.SHA512 = bytes.Repeat([]byte{0x11}, 64)
		}
		if job.Kind == worker.MediaImage {
			result.PDQ = bytes.Repeat([]byte{0x22}, 32)
			result.Quality, result.Width, result.Height = 88, 640, 480
		} else {
			duration, quality := int64(5000), int32(91)
			result.DurationMS, result.ThumbQuality = &duration, &quality
			result.ThumbPath = `D:\cache\clip.jpg`
			result.ThumbPDQ = bytes.Repeat([]byte{0x33}, 32)
		}
		pool.addMetrics(worker.MetricsSnapshot{
			FilesDone: 1, DecodeCalls: 1, ReadAttempts: 1, DecodeAttempts: 1,
			ReadNS: 12_000_000, DecodeNS: 34_000_000,
		})
		pool.results <- result
	}
	manager.pool = pool
	var taskLog bytes.Buffer
	manager.log = slog.New(slog.NewJSONHandler(&taskLog, nil))

	done := make(chan proto.TaskDone, 1)
	var mu sync.Mutex
	var features []proto.FeatureItem
	sender := func(msgType uint8, value any) error {
		switch msgType {
		case proto.MsgFeatureResult:
			mu.Lock()
			features = append(features, value.(*proto.FeatureResult).Items...)
			mu.Unlock()
		case proto.MsgTaskDone:
			done <- *value.(*proto.TaskDone)
		}
		return nil
	}
	task := proto.ScanTask{TaskID: "task-media-routing", Roots: []string{`D:\media`}, Phase: 1}
	if ack := manager.Handle(task, sender); !ack.Accepted {
		t.Fatalf("ack = %#v", ack)
	}
	select {
	case final := <-done:
		if final.Stats.Done != 3 || final.Stats.FilesDone != 2 ||
			final.Stats.DecodeCalls != 2 ||
			final.Stats.AvgReadMS != 12 || final.Stats.AvgDecodeMS != 34 {
			t.Fatalf("TaskDone stats = %#v", final.Stats)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not finish")
	}
	if len(hashed) != 1 || hashed[0] != `D:\media\plain.txt` {
		t.Fatalf("Go hasher paths = %#v, want only non-media", hashed)
	}
	submitted := pool.submittedSnapshot()
	if len(submitted) != 2 {
		t.Fatalf("pool submissions = %#v", submitted)
	}
	var known, video *worker.JobMsg
	for index := range submitted {
		switch submitted[index].Path {
		case `D:\media\known.jpg`:
			known = &submitted[index]
		case `D:\media\clip.mp4`:
			video = &submitted[index]
		}
	}
	if known == nil || known.FieldsMask != worker.MaskImagePDQ ||
		len(known.KnownSHA) != 64 || known.ScanTaskID != task.TaskID {
		t.Fatalf("known-SHA image job = %#v", known)
	}
	if video == nil || video.FieldsMask != worker.MaskAllVideo ||
		len(video.KnownSHA) != 0 || video.ScanTaskID != task.TaskID {
		t.Fatalf("video job = %#v", video)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(features) != 3 {
		t.Fatalf("feature items = %#v", features)
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(taskLog.Bytes()), &record); err != nil {
		t.Fatalf("scan summary log = %q: %v", taskLog.Bytes(), err)
	}
	for key, want := range map[string]float64{
		"files_done": 2, "files_failed": 0, "decode_calls": 2,
		"thumb_generated": 0, "thumb_cache_hits": 0,
		"singleflight_hits": 0, "crashes": 0,
	} {
		if got := record[key]; got != want {
			t.Fatalf("scan summary %s = %#v, want %#v; record=%#v", key, got, want, record)
		}
	}
	if _, exists := record["elapsed_ms"]; !exists {
		t.Fatalf("scan summary missing elapsed_ms: %#v", record)
	}
}

func TestPreparePendingSkipsMediaRowsWhosePhase1MaskIsZero(t *testing.T) {
	manager, cleanup := newTestScanManager(t, nil, nil)
	defer cleanup()
	pool := newFakeScanPool()
	manager.pool = pool
	state := &ScanState{Task: proto.ScanTask{TaskID: "task-mask-zero"}}
	work, routes := manager.preparePending(state, map[int64][]store.PendingFile{
		1: {{
			Path:        `D:\media\complete.jpg`,
			MissingMask: proto.FieldPHashParts,
		}},
	})
	if len(work) != 0 || len(routes) != 0 || len(pool.submittedSnapshot()) != 0 {
		t.Fatalf("mask-zero prepare = work %#v routes %#v submits %#v", work, routes, pool.submittedSnapshot())
	}
}

func TestDefaultStageOnePreparePendingDoesNotDependOnMutableWorkerAliases(t *testing.T) {
	originalImage, originalVideo := worker.MaskAllImage, worker.MaskAllVideo
	worker.MaskAllImage, worker.MaskAllVideo = 0, worker.MaskVideoThumb
	t.Cleanup(func() {
		worker.MaskAllImage, worker.MaskAllVideo = originalImage, originalVideo
	})
	m, cleanup := newTestScanManager(t, nil, nil)
	defer cleanup()
	m.pool = newFakeScanPool()
	state := &ScanState{Task: proto.ScanTask{TaskID: "task-required-mask"}}
	pending := map[int64][]store.PendingFile{1: {
		{Path: `D:\image.jpg`, MissingMask: store.RequiredStageOneMask(store.MediaImage)},
		{Path: `D:\video.mp4`, MissingMask: store.RequiredStageOneMask(store.MediaVideo)},
	}}
	work, _ := m.preparePending(state, pending)
	if len(work[1]) != 2 || work[1][0].media == nil || work[1][1].media == nil ||
		work[1][0].media.FieldsMask != store.RequiredStageOneMask(store.MediaImage) ||
		work[1][1].media.FieldsMask != store.RequiredStageOneMask(store.MediaVideo) {
		t.Fatalf("prepared required masks = %#v", work[1])
	}
}

func TestImageNoThumbnailFeatureItemKeepsImageDimensionsOnly(t *testing.T) {
	job := &worker.JobMsg{
		Path: `D:\media\photo.jpg`, Kind: worker.MediaImage,
		FieldsMask: worker.MaskAllImage, Size: 10, MTimeUnix: 20,
	}
	result := &worker.JobResultMsg{
		Kind: worker.MediaImage, FieldsDone: worker.MaskAllImage,
		SHA512: bytes.Repeat([]byte{1}, 64), PDQ: bytes.Repeat([]byte{2}, 32),
		Quality: 87, Width: 640, Height: 480,
	}
	item := featureItemFromWorker(job, result)
	if item.Status != proto.StatusDone || item.Width != 640 || item.Height != 480 ||
		item.ThumbPath != "" || item.ThumbPDQ256 != "" || item.ThumbQuality != nil {
		t.Fatalf("image feature item = %#v", item)
	}
}

func TestVideoBaseFeaturesFeatureItemUsesContactSheetDimensions(t *testing.T) {
	duration, quality := int64(4321), int32(91)
	job := &worker.JobMsg{
		Path: `D:\media\clip.mp4`, Kind: worker.MediaVideo,
		FieldsMask: worker.MaskAllVideo, Size: 30, MTimeUnix: 40,
	}
	result := &worker.JobResultMsg{
		Kind: worker.MediaVideo, FieldsDone: worker.MaskAllVideo,
		SHA512: bytes.Repeat([]byte{3}, 64), DurationMS: &duration,
		ThumbPath: `D:\cache\clip.jpg`, ThumbPDQ: bytes.Repeat([]byte{4}, 32),
		ThumbQuality: &quality, ContactSheetWidth: 960, ContactSheetHeight: 540,
	}
	item := featureItemFromWorker(job, result)
	if item.Status != proto.StatusDone || item.Width != 960 || item.Height != 540 ||
		item.ThumbPath != result.ThumbPath {
		t.Fatalf("video feature item = %#v", item)
	}
}

func TestVideoBaseFeaturesMissingContactSheetIsPartial(t *testing.T) {
	duration := int64(4321)
	job := &worker.JobMsg{
		Path: `D:\media\partial.mp4`, Kind: worker.MediaVideo,
		FieldsMask: worker.MaskAllVideo,
	}
	result := &worker.JobResultMsg{
		Kind:       worker.MediaVideo,
		FieldsDone: worker.MaskSHA512 | worker.MaskVideoDuration,
		SHA512:     bytes.Repeat([]byte{5}, 64), DurationMS: &duration,
		Errors: []worker.FieldError{{
			Field: worker.MaskVideoContactSheet, Stage: "contact_sheet", Msg: "decode failed",
		}},
	}
	item := featureItemFromWorker(job, result)
	if item.Status != proto.StatusPartial ||
		item.FieldsDone != worker.MaskSHA512|worker.MaskVideoDuration {
		t.Fatalf("partial video feature item = %#v", item)
	}
}

func TestMetricAveragesUseAttemptDenominatorsAndNanosecondPrecision(t *testing.T) {
	readMS, decodeMS := metricAveragesMS(worker.MetricsSnapshot{
		FilesDone:      1,
		FilesFailed:    3,
		Crashes:        1,
		DecodeCalls:    0,
		ReadAttempts:   2,
		DecodeAttempts: 1,
		ReadNS:         1_500_000,
		DecodeNS:       250_000,
	})
	if readMS != 0.75 || decodeMS != 0.25 {
		t.Fatalf("attempt averages = read:%v decode:%v, want 0.75/0.25", readMS, decodeMS)
	}

	readMS, decodeMS = metricAveragesMS(worker.MetricsSnapshot{
		FilesFailed: 1,
		Crashes:     1,
	})
	if readMS != 0 || decodeMS != 0 {
		t.Fatalf("crash without reported attempts changed averages = read:%v decode:%v", readMS, decodeMS)
	}
}

func TestScanForwardsPartialAndStoreFailureWithoutDoubleCompleting(t *testing.T) {
	enumr := &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\media`: {
			{Path: `D:\media\partial.jpg`, Size: 10, MTime: 20},
			{Path: `D:\media\store-fail.jpg`, Size: 30, MTime: 40},
		},
	}}
	manager, cleanup := newTestScanManager(t, enumr, nil)
	defer cleanup()
	pool := newFakeScanPool()
	pool.onSubmit = func(job worker.JobMsg) {
		result := &worker.JobResultMsg{
			JobID: job.JobID, ScanTaskID: job.ScanTaskID,
			Path: job.Path, Kind: job.Kind, Phase: job.Phase,
		}
		if strings.Contains(job.Path, "partial") {
			result.SHA512 = bytes.Repeat([]byte{0x44}, 64)
			result.FieldsDone = worker.MaskSHA512
			result.Errors = []worker.FieldError{{
				Field: worker.MaskImagePDQ,
				Stage: "decode",
				Msg:   "bad pixels",
			}}
		} else {
			result.Errors = []worker.FieldError{{
				Field: job.FieldsMask,
				Stage: "store",
				Msg:   "sqlite commit failed",
			}}
		}
		pool.addMetrics(worker.MetricsSnapshot{FilesFailed: 1})
		pool.results <- result
	}
	manager.pool = pool
	done := make(chan proto.TaskDone, 1)
	items := make(chan proto.FeatureItem, 2)
	sender := func(msgType uint8, value any) error {
		if msgType == proto.MsgFeatureResult {
			for _, item := range value.(*proto.FeatureResult).Items {
				items <- item
			}
		}
		if msgType == proto.MsgTaskDone {
			done <- *value.(*proto.TaskDone)
		}
		return nil
	}
	manager.Handle(proto.ScanTask{
		TaskID: "task-partial-store", Roots: []string{`D:\media`}, Phase: 1,
	}, sender)
	select {
	case final := <-done:
		if final.Stats.Done != 2 || final.Stats.Failed != 2 ||
			final.Stats.FilesFailed != 2 {
			t.Fatalf("TaskDone = %#v", final)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not finish")
	}
	got := map[string]proto.FeatureItem{}
	for range 2 {
		item := <-items
		got[item.Path] = item
	}
	partial := got[`D:\media\partial.jpg`]
	if partial.Status != proto.StatusPartial ||
		partial.FieldsDone != proto.FieldSHA512 ||
		len(partial.FieldErrors) != 1 ||
		partial.FieldErrors[0].Stage != "decode" {
		t.Fatalf("partial item = %#v", partial)
	}
	failed := got[`D:\media\store-fail.jpg`]
	if failed.Status != proto.StatusFailed || failed.FieldsDone != 0 ||
		len(failed.FieldErrors) != 1 ||
		failed.FieldErrors[0].Stage != "store" {
		t.Fatalf("store-failure item = %#v", failed)
	}
}

func TestScanVideoDurationOnlySuccessIsPartialAndForwardsDuration(t *testing.T) {
	enumr := &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\media`: {{Path: `D:\media\duration-only.mp4`, Size: 30, MTime: 40}},
	}}
	manager, cleanup := newTestScanManager(t, enumr, nil)
	defer cleanup()
	pool := newFakeScanPool()
	pool.onSubmit = func(job worker.JobMsg) {
		duration := int64(5432)
		pool.addMetrics(worker.MetricsSnapshot{FilesFailed: 1})
		pool.results <- &worker.JobResultMsg{
			JobID: job.JobID, ScanTaskID: job.ScanTaskID,
			Path: job.Path, Kind: worker.MediaVideo, Phase: job.Phase,
			SHA512:     bytes.Repeat([]byte{0x55}, 64),
			DurationMS: &duration,
			Errors: []worker.FieldError{{
				Field: worker.MaskVideoThumb,
				Stage: "ffmpeg",
				Msg:   "thumbnail failed",
			}},
		}
	}
	manager.pool = pool
	done := make(chan proto.TaskDone, 1)
	items := make(chan proto.FeatureItem, 1)
	manager.Handle(proto.ScanTask{
		TaskID: "task-duration-partial", Roots: []string{`D:\media`}, Phase: 1,
	}, func(msgType uint8, value any) error {
		if msgType == proto.MsgFeatureResult {
			items <- value.(*proto.FeatureResult).Items[0]
		}
		if msgType == proto.MsgTaskDone {
			done <- *value.(*proto.TaskDone)
		}
		return nil
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not finish")
	}
	item := <-items
	if item.Status != proto.StatusPartial ||
		item.DurationMS == nil || *item.DurationMS != 5432 ||
		len(item.FieldErrors) != 1 ||
		item.FieldErrors[0].Stage != "ffmpeg" {
		t.Fatalf("duration-only item = %#v", item)
	}
}

func TestScanInvalidPersistedSHAIsPerFileFailureWithoutPoolSubmission(t *testing.T) {
	enumr := &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\media`: {{Path: `D:\media\bad.jpg`, Size: 10, MTime: 20}},
	}}
	manager, cleanup := newTestScanManager(t, enumr, nil)
	defer cleanup()
	if err := manager.st.UpsertEnumerated(context.Background(), []store.EnumUpsert{{
		MachineID: "machine-a", DiskNo: 1, Path: `D:\media\bad.jpg`,
		Size: 10, MTime: 20, MissingBase: proto.FieldSHA512 | proto.FieldPDQ256,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.st.ApplyHashResults(context.Background(), "machine-a", []store.HashResult{{
		Path: `D:\media\bad.jpg`, SHA512: "not-hex",
	}}); err != nil {
		t.Fatal(err)
	}
	pool := newFakeScanPool()
	manager.pool = pool
	done := make(chan proto.TaskDone, 1)
	var failed bool
	sender := func(msgType uint8, value any) error {
		if msgType == proto.MsgFeatureResult {
			for _, item := range value.(*proto.FeatureResult).Items {
				failed = failed || item.Path == `D:\media\bad.jpg` &&
					item.Status == proto.StatusFailed &&
					strings.Contains(item.Err, "persisted SHA-512")
			}
		}
		if msgType == proto.MsgTaskDone {
			done <- *value.(*proto.TaskDone)
		}
		return nil
	}
	manager.Handle(proto.ScanTask{
		TaskID: "task-invalid-sha", Roots: []string{`D:\media`}, Phase: 1,
	}, sender)
	select {
	case final := <-done:
		if final.Stats.Done != 1 || final.Stats.Failed != 1 {
			t.Fatalf("stats = %#v", final.Stats)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not finish")
	}
	if len(pool.submittedSnapshot()) != 0 || !failed {
		t.Fatalf("submissions=%#v failed=%t", pool.submittedSnapshot(), failed)
	}
}

func TestScanCorrelatesMediaTerminalEventAndForwardsCrashNoticeToReboundSender(t *testing.T) {
	enumr := &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\media`: {{Path: `D:\media\crash.jpg`, Size: 10, MTime: 20}},
	}}
	manager, cleanup := newTestScanManager(t, enumr, nil)
	defer cleanup()
	pool := newFakeScanPool()
	pool.onSubmit = func(job worker.JobMsg) {
		pool.results <- &worker.JobResultMsg{
			JobID: job.JobID + 999, ScanTaskID: "foreign-task",
			Path: `D:\foreign.jpg`, Kind: worker.MediaImage,
		}
		pool.addMetrics(worker.MetricsSnapshot{FilesFailed: 1, Crashes: 1})
		pool.crashes <- worker.CrashRecord{
			JobID: job.JobID, ScanTaskID: job.ScanTaskID, File: job.Path,
			PID: 4321, ExitCode: -1073741819, Reason: "exit_code",
		}
	}
	manager.pool = pool
	oldBlocked := make(chan struct{})
	oldRelease := make(chan struct{})
	oldSender := func(msgType uint8, _ any) error {
		if msgType == proto.MsgTaskProgress {
			close(oldBlocked)
			<-oldRelease
			return errors.New("disconnected")
		}
		return nil
	}
	task := proto.ScanTask{TaskID: "task-crash", Roots: []string{`D:\media`}, Phase: 1}
	ack, start := manager.Prepare(task, oldSender)
	if !ack.Accepted {
		t.Fatalf("ack = %#v", ack)
	}
	start()
	<-oldBlocked
	done := make(chan proto.TaskDone, 1)
	crashNotices := make(chan proto.CrashNotice, 1)
	newSender := func(msgType uint8, value any) error {
		switch msgType {
		case proto.MsgCrashNotice:
			crashNotices <- *value.(*proto.CrashNotice)
		case proto.MsgTaskDone:
			done <- *value.(*proto.TaskDone)
		}
		return nil
	}
	resume, _ := manager.Prepare(task, newSender)
	if !resume.Accepted || resume.Reason != "resumed" {
		t.Fatalf("resume = %#v", resume)
	}
	close(oldRelease)
	select {
	case notice := <-crashNotices:
		if notice.TaskID != task.TaskID || notice.Path != `D:\media\crash.jpg` ||
			notice.PID != 4321 || notice.ExitCode != -1073741819 {
			t.Fatalf("CrashNotice = %#v", notice)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no crash notice")
	}
	select {
	case final := <-done:
		if final.Stats.Done != 1 || final.Stats.Failed != 1 ||
			final.Stats.Crashes != 1 {
			t.Fatalf("TaskDone = %#v", final)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not finish")
	}
}

func TestScanDrainsStaleCrashBeforeActiveTerminalAndCompletesRoute(t *testing.T) {
	enumr := &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\media`: {{Path: `D:\media\active-crash.jpg`, Size: 10, MTime: 20}},
	}}
	manager, cleanup := newTestScanManager(t, enumr, nil)
	defer cleanup()
	pool := newFakeScanPool()
	pool.crashes = make(chan worker.CrashRecord, 1)
	pool.crashes <- worker.CrashRecord{
		JobID: 999, ScanTaskID: "task-stale", File: `D:\media\stale.jpg`,
	}
	pool.onSubmit = func(job worker.JobMsg) {
		pool.addMetrics(worker.MetricsSnapshot{FilesFailed: 1, Crashes: 1})
		// This models Pool's reliable bounded active-terminal send: it waits
		// until collectMedia drains the stale notification already buffered.
		pool.crashes <- worker.CrashRecord{
			JobID: job.JobID, ScanTaskID: job.ScanTaskID, File: job.Path,
			PID: 9876, ExitCode: -1, Reason: "watchdog_image",
		}
	}
	manager.pool = pool
	done := make(chan proto.TaskDone, 1)
	items := make(chan proto.FeatureItem, 1)
	manager.Handle(proto.ScanTask{
		TaskID: "task-active", Roots: []string{`D:\media`}, Phase: 1,
	}, func(msgType uint8, value any) error {
		switch msgType {
		case proto.MsgFeatureResult:
			items <- value.(*proto.FeatureResult).Items[0]
		case proto.MsgTaskDone:
			done <- *value.(*proto.TaskDone)
		}
		return nil
	})
	select {
	case item := <-items:
		if item.Path != `D:\media\active-crash.jpg` ||
			item.Status != proto.StatusCrash {
			t.Fatalf("active crash item=%#v", item)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("active crash did not terminate its media route")
	}
	select {
	case final := <-done:
		if final.Stats.Done != 1 || final.Stats.Failed != 1 ||
			final.Stats.Crashes != 1 {
			t.Fatalf("TaskDone=%#v", final)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not complete after active crash terminal")
	}
}

func TestScanManagerCancelUnknownTask(t *testing.T) {
	manager, cleanup := newTestScanManager(t, nil, nil)
	defer cleanup()
	cancelled, stats := manager.Cancel("no-such-task")
	if cancelled || stats != nil {
		t.Fatalf("Cancel unknown task = (%v, %#v), want (false, nil)", cancelled, stats)
	}
}

func TestScanManagerCancelStopsRunningScanAndReportsDoneStatsAfterwards(t *testing.T) {
	hashStarted := make(chan struct{})
	releaseHash := make(chan struct{})
	var once sync.Once
	hasher := hasherFunc(func(string) (string, error) {
		once.Do(func() { close(hashStarted) })
		<-releaseHash
		return "hash", nil
	})
	enumr := &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\media`: {
			{Path: `D:\media\a.bin`, Size: 1, MTime: 100},
			{Path: `D:\media\b.bin`, Size: 1, MTime: 100},
		},
	}}
	manager, cleanup := newTestScanManager(t, enumr, hasher)
	defer cleanup()

	done := make(chan proto.TaskDone, 1)
	task := proto.ScanTask{TaskID: "task-cancel", Roots: []string{`D:\media`}, Phase: 1}
	if ack := manager.Handle(task, captureTaskDone(done)); !ack.Accepted {
		t.Fatalf("ack = %#v", ack)
	}
	select {
	case <-hashStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("hash did not start")
	}
	cancelled, stats := manager.Cancel("task-cancel")
	if !cancelled || stats != nil {
		t.Fatalf("Cancel running = (%v, %#v), want (true, nil)", cancelled, stats)
	}
	// Repeated cancel of a task still unwinding is idempotent.
	again, againStats := manager.Cancel("task-cancel")
	if !again || againStats != nil {
		t.Fatalf("repeated Cancel = (%v, %#v), want (true, nil)", again, againStats)
	}
	close(releaseHash)
	select {
	case result := <-done:
		if result.TaskID != "task-cancel" || result.Reason != "cancelled" {
			t.Fatalf("TaskDone = %#v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled scan did not emit its terminal receipt")
	}
	// After completion the same task id reports its final stats instead of
	// accepting another cancel.
	after, afterStats := manager.Cancel("task-cancel")
	if after || afterStats == nil {
		t.Fatalf("post-completion Cancel = (%v, %#v), want (false, stats)", after, afterStats)
	}
}

func TestScanManagerCancelBeforeStartSkipsEnumeration(t *testing.T) {
	enumr := &fakeEnumerator{records: map[string][]fileenum.FileRecord{
		`D:\media`: {{Path: `D:\media\a.bin`, Size: 1, MTime: 100}},
	}}
	manager, cleanup := newTestScanManager(t, enumr, nil)
	defer cleanup()
	done := make(chan proto.TaskDone, 1)
	task := proto.ScanTask{TaskID: "task-pre-cancel", Roots: []string{`D:\media`}, Phase: 1}
	ack, start := manager.Prepare(task, captureTaskDone(done))
	if !ack.Accepted {
		t.Fatalf("prepared ack = %#v", ack)
	}
	cancelled, stats := manager.Cancel("task-pre-cancel")
	if !cancelled || stats != nil {
		t.Fatalf("Cancel = (%v, %#v), want (true, nil)", cancelled, stats)
	}
	start()
	select {
	case result := <-done:
		if result.Reason != "cancelled" {
			t.Fatalf("TaskDone reason = %q, want cancelled", result.Reason)
		}
		if result.Stats.ScanErrors != 0 || result.Stats.Total != 0 {
			t.Fatalf("cancelled-before-start stats = %#v, want zero work and no scan error", result.Stats)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled scan did not reach its terminal receipt")
	}
	if enumr.callCount() != 0 {
		t.Fatalf("enumerator ran after cancellation: %d calls", enumr.callCount())
	}
}

func TestScanManagerCancelDuringEnumerationAbortsWithoutScanError(t *testing.T) {
	enumStarted := make(chan struct{})
	var manager *ScanManager
	var once sync.Once
	enumr := &fakeEnumerator{
		records: map[string][]fileenum.FileRecord{
			`D:\media`: {
				{Path: `D:\media\a.bin`, Size: 1, MTime: 100},
				{Path: `D:\media\b.bin`, Size: 1, MTime: 100},
			},
		},
		onRecord: func(fileenum.FileRecord) {
			once.Do(func() {
				close(enumStarted)
				manager.Cancel("task-enum-cancel")
			})
		},
	}
	var cleanup func()
	manager, cleanup = newTestScanManager(t, enumr, nil)
	defer cleanup()
	done := make(chan proto.TaskDone, 1)
	task := proto.ScanTask{TaskID: "task-enum-cancel", Roots: []string{`D:\media`}, Phase: 1}
	if ack := manager.Handle(task, captureTaskDone(done)); !ack.Accepted {
		t.Fatalf("ack = %#v", ack)
	}
	select {
	case <-enumStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("enumeration did not start")
	}
	select {
	case result := <-done:
		// The cancel landed before the first record visit, so no record was
		// enumerated and the interruption is not counted as a scan error.
		if result.Reason != "cancelled" {
			t.Fatalf("TaskDone reason = %q, want cancelled", result.Reason)
		}
		if result.Stats.ScanErrors != 0 || result.Stats.Total != 0 {
			t.Fatalf("cancel-during-enum stats = %#v, want zero work and no scan error", result.Stats)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled scan did not finish")
	}
}

type fakeScanPool struct {
	mu        sync.Mutex
	submitted []worker.JobMsg
	metrics   worker.MetricsSnapshot
	results   chan *worker.JobResultMsg
	crashes   chan worker.CrashRecord
	onSubmit  func(worker.JobMsg)
}

func newFakeScanPool() *fakeScanPool {
	return &fakeScanPool{
		results: make(chan *worker.JobResultMsg, 32),
		crashes: make(chan worker.CrashRecord, 32),
	}
}

func (p *fakeScanPool) Submit(job *worker.JobMsg) error {
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

func (p *fakeScanPool) Results() <-chan *worker.JobResultMsg { return p.results }
func (p *fakeScanPool) Crashes() <-chan worker.CrashRecord   { return p.crashes }
func (p *fakeScanPool) Metrics() worker.MetricsSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.metrics
}
func (p *fakeScanPool) addMetrics(delta worker.MetricsSnapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.metrics.FilesDone += delta.FilesDone
	p.metrics.FilesFailed += delta.FilesFailed
	p.metrics.DecodeCalls += delta.DecodeCalls
	p.metrics.ReadAttempts += delta.ReadAttempts
	p.metrics.DecodeAttempts += delta.DecodeAttempts
	p.metrics.ReadNS += delta.ReadNS
	p.metrics.DecodeNS += delta.DecodeNS
	p.metrics.ThumbGenerated += delta.ThumbGenerated
	p.metrics.ThumbCacheHits += delta.ThumbCacheHits
	p.metrics.SingleFlightHits += delta.SingleFlightHits
	p.metrics.Crashes += delta.Crashes
}
func (p *fakeScanPool) submittedSnapshot() []worker.JobMsg {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]worker.JobMsg(nil), p.submitted...)
}

func newTestScanManager(
	t *testing.T,
	enumr fileenum.Enumerator,
	hasher Hasher,
) (*ScanManager, func()) {
	t.Helper()
	if enumr == nil {
		enumr = &fakeEnumerator{records: map[string][]fileenum.FileRecord{}}
	}
	if hasher == nil {
		hasher = hasherFunc(func(string) (string, error) { return "hash", nil })
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultAgent()
	cfg.MachineID = "machine-a"
	cfg.Scan.HDDStreams = 2
	cfg.Scan.SSDStreams = 4
	var logOutput bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logOutput, nil))
	manager := NewScanManagerWithResolver(
		cfg,
		db,
		enumr,
		hasher,
		log,
		log,
		func(string) (diskio.Identity, error) {
			return diskio.Identity{Key: "physical:7", Local: true, DiskNos: []uint32{1}, KnownSSD: true}, nil
		},
	)
	return manager, func() { _ = db.Close() }
}

func captureTaskDone(done chan<- proto.TaskDone) Sender {
	return func(msgType uint8, value any) error {
		if msgType == proto.MsgTaskDone {
			done <- *value.(*proto.TaskDone)
		}
		return nil
	}
}

type hasherFunc func(string) (string, error)

func (fn hasherFunc) HashFile(path string) (string, error) { return fn(path) }

type fakeEnumerator struct {
	mu      sync.Mutex
	calls   int
	records map[string][]fileenum.FileRecord
	errors  map[string]error
	// onRecord, when set, runs before each record is handed to visit; tests
	// use it to trigger cancellation mid-enumeration.
	onRecord func(fileenum.FileRecord)
}

type barrierEnumerator struct {
	first   fileenum.FileRecord
	second  fileenum.FileRecord
	visited chan struct{}
	release chan struct{}
}

type releaseScanEnumerator struct {
	release <-chan struct{}
}

func (releaseScanEnumerator) Name() string     { return "release" }
func (releaseScanEnumerator) Available() error { return nil }
func (e releaseScanEnumerator) Enum(_ string, _ func(fileenum.FileRecord) error) error {
	<-e.release
	return nil
}

func (*barrierEnumerator) Name() string     { return "barrier" }
func (*barrierEnumerator) Available() error { return nil }
func (e *barrierEnumerator) Enum(_ string, visit func(fileenum.FileRecord) error) error {
	if err := visit(e.first); err != nil {
		return err
	}
	close(e.visited)
	<-e.release
	return visit(e.second)
}

func (f *fakeEnumerator) Name() string     { return "fake" }
func (f *fakeEnumerator) Available() error { return nil }
func (f *fakeEnumerator) Enum(root string, visit func(fileenum.FileRecord) error) error {
	f.mu.Lock()
	f.calls++
	records := append([]fileenum.FileRecord(nil), f.records[root]...)
	f.mu.Unlock()
	for _, record := range records {
		if f.onRecord != nil {
			f.onRecord(record)
		}
		if err := visit(record); err != nil {
			return err
		}
	}
	return f.errors[root]
}

func (f *fakeEnumerator) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}
