package agent

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dedup/internal/proto"
	"dedup/internal/store"
	"dedup/internal/worker"
)

func TestPhase2PrepareRejectsInvalidEnvelopeAtomically(t *testing.T) {
	tooMany := make([]proto.Phase2Item, 5001)
	for index := range tooMany {
		tooMany[index] = validPhase2Image(
			fmt.Sprintf(`D:\media\too-many-%d.jpg`, index),
		)
	}
	invalidItem := validPhase2Image(`D:\media\invalid.jpg`)
	invalidItem.FieldsMask = 0
	wrongMachine := validPhase2Image(`D:\media\wrong-machine.jpg`)
	wrongMachine.MachineID = "machine-b"
	duplicate := validPhase2Image(`D:\media\duplicate.jpg`)
	conflictingPath := validPhase2Image(`D:\media\conflict-path.jpg`)
	conflictingPath.SHA512 = duplicate.SHA512

	tests := []struct {
		name string
		task proto.Phase2Task
		want string
	}{
		{
			name: "empty task id",
			task: proto.Phase2Task{Items: []proto.Phase2Item{
				validPhase2Image(`D:\media\a.jpg`),
			}},
			want: "empty task_id",
		},
		{
			name: "empty items",
			task: proto.Phase2Task{TaskID: "empty-items"},
			want: "empty items",
		},
		{
			name: "shard limit",
			task: proto.Phase2Task{TaskID: "too-many", Items: tooMany},
			want: "5000",
		},
		{
			name: "invalid item",
			task: proto.Phase2Task{
				TaskID: "invalid-item",
				Items:  []proto.Phase2Item{validPhase2Image(`D:\media\ok.jpg`), invalidItem},
			},
			want: "fields_mask",
		},
		{
			name: "wrong machine",
			task: proto.Phase2Task{
				TaskID: "wrong-machine",
				Items:  []proto.Phase2Item{wrongMachine},
			},
			want: "machine_id",
		},
		{
			name: "duplicate machine path",
			task: proto.Phase2Task{
				TaskID: "duplicate-path",
				Items:  []proto.Phase2Item{duplicate, duplicate},
			},
			want: "duplicate",
		},
		{
			name: "conflicting sha path identity",
			task: proto.Phase2Task{
				TaskID: "conflicting-sha",
				Items:  []proto.Phase2Item{duplicate, conflictingPath},
			},
			want: "conflicting",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := NewPhase2Manager("machine-a")
			var executions atomic.Int64
			manager.run = func(*phase2State) {
				executions.Add(1)
			}

			ack, start := manager.Prepare(test.task, nil)

			if ack.Accepted || !strings.Contains(ack.Reason, test.want) {
				t.Fatalf("Prepare(%s) ack=%#v, want rejection containing %q",
					test.name, ack, test.want)
			}
			if start != nil {
				start()
			}
			if got := executions.Load(); got != 0 {
				t.Fatalf("invalid task executed %d times, want 0", got)
			}
			manager.mu.Lock()
			retained := len(manager.tasks)
			manager.mu.Unlock()
			if retained != 0 {
				t.Fatalf("invalid task retained %d task entries, want 0", retained)
			}
		})
	}
}

func TestPhase2EnvelopeUsesStageAwareValidationAndStageIsPartOfIdentity(t *testing.T) {
	manager := NewPhase2Manager("machine-a")
	stageTwoItem := validPhase2Image(`D:\media\stage-envelope.jpg`)
	stageTwoItem.FieldsMask = proto.FieldPHashParts
	stageTwo := proto.Phase2Task{
		TaskID: "stage-envelope",
		Stage:  proto.ScreenStageTwo,
		Items:  []proto.Phase2Item{stageTwoItem},
	}

	ack, _ := manager.Prepare(stageTwo, nil)
	if !ack.Accepted {
		t.Fatalf("stage-two ack=%#v, want accepted", ack)
	}
	ack, _ = manager.Prepare(clonePhase2TaskForTest(stageTwo), nil)
	if !ack.Accepted || ack.Reason != "resumed" {
		t.Fatalf("same stage envelope ack=%#v, want resumed", ack)
	}

	stageThree := clonePhase2TaskForTest(stageTwo)
	stageThree.Stage = proto.ScreenStageThree
	stageThree.Items[0].FieldsMask = proto.FieldSobelHist
	ack, _ = manager.Prepare(stageThree, nil)
	if ack.Accepted || !strings.Contains(ack.Reason, "task_id envelope mismatch") {
		t.Fatalf("different stage envelope ack=%#v, want mismatch", ack)
	}

	invalidStageTwo := clonePhase2TaskForTest(stageTwo)
	invalidStageTwo.TaskID = "stage-envelope-invalid"
	invalidStageTwo.Items[0].FieldsMask = proto.FieldSobelHist
	ack, _ = manager.Prepare(invalidStageTwo, nil)
	if ack.Accepted || !strings.Contains(ack.Reason, "stage-two") {
		t.Fatalf("invalid stage-two ack=%#v, want stage-aware rejection", ack)
	}
}

func TestPhase2StageTwoCreatesOnlyManagerStageTwoJob(t *testing.T) {
	pool := newPhase2FakePool()
	router := NewPoolRouter(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer close(pool.results)
	path := `D:\media\stage-two-only.jpg`
	item := validPhase2Image(path)
	item.FieldsMask = proto.FieldPHashParts
	manager := NewPhase2ManagerWithRuntime(
		"machine-a",
		&phase2CommittedFake{states: map[string]store.Phase2Committed{
			path: {MissingFields: proto.FieldPHashParts},
		}},
		pool,
		router,
		func(string) (int64, bool, error) { return 1, false, nil },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	submitted := make(chan worker.JobMsg, 2)
	pool.onSubmit = func(job worker.JobMsg) {
		submitted <- job
		pool.results <- &worker.JobResultMsg{
			JobID: job.JobID, ScanTaskID: job.ScanTaskID, Path: job.Path,
			Kind: job.Kind, Phase: job.Phase, ScreenStage: job.ScreenStage,
			Source: job.Source, FieldsDone: worker.MaskPHashParts,
			SHA512:     append([]byte(nil), job.KnownSHA...),
			PHashParts: []byte{7},
		}
	}
	task := proto.Phase2Task{TaskID: "stage-two-only", Stage: proto.ScreenStageTwo, Items: []proto.Phase2Item{item}}
	ack, start := manager.Prepare(task, nil)
	if !ack.Accepted || start == nil {
		t.Fatalf("ack=%#v start_nil=%t", ack, start == nil)
	}
	start()

	select {
	case job := <-submitted:
		if job.ScreenStage != worker.ScreenStageTwo || job.Source != worker.JobSourceManager ||
			job.FieldsMask != worker.MaskPHashParts {
			t.Fatalf("stage-two job=%#v", job)
		}
	case <-time.After(time.Second):
		t.Fatal("stage-two job was not submitted")
	}

	deadline := time.After(time.Second)
	for {
		manager.mu.Lock()
		state := manager.tasks[task.TaskID]
		manager.mu.Unlock()
		state.mu.Lock()
		done := state.status == proto.StatusDone
		state.mu.Unlock()
		if done {
			break
		}
		select {
		case extra := <-submitted:
			t.Fatalf("stage two implicitly submitted another job: %#v", extra)
		case <-deadline:
			t.Fatal("stage-two task did not complete")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case extra := <-submitted:
		t.Fatalf("stage two implicitly submitted stage three: %#v", extra)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestPhase2CachedStageStillSubmitsOriginalRequestAndReturnsPayload(t *testing.T) {
	for _, test := range []struct {
		name    string
		stage   uint8
		field   uint32
		payload []byte
	}{
		{name: "stage two pHash", stage: proto.ScreenStageTwo, field: proto.FieldPHashParts, payload: []byte{2, 2}},
		{name: "stage three Sobel", stage: proto.ScreenStageThree, field: proto.FieldSobelHist, payload: []byte{3, 3}},
	} {
		t.Run(test.name, func(t *testing.T) {
			pool := newPhase2FakePool()
			router := NewPoolRouter(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
			defer close(pool.results)
			path := `D:\cache\` + test.name + `.jpg`
			item := validPhase2Image(path)
			item.FieldsMask = test.field
			manager := NewPhase2ManagerWithRuntime(
				"machine-a",
				&phase2CommittedFake{states: map[string]store.Phase2Committed{path: {MissingFields: 0}}},
				pool, router,
				func(string) (int64, bool, error) { return 1, false, nil },
				slog.New(slog.NewTextHandler(io.Discard, nil)),
			)
			pool.onSubmit = func(job worker.JobMsg) {
				result := &worker.JobResultMsg{
					JobID: job.JobID, ScanTaskID: job.ScanTaskID, Path: job.Path, Kind: job.Kind,
					Phase: job.Phase, ScreenStage: job.ScreenStage, Source: job.Source,
					SHA512: append([]byte(nil), job.KnownSHA...), FieldsDone: job.FieldsMask,
				}
				if test.field == proto.FieldPHashParts {
					result.PHashParts = append([]byte(nil), test.payload...)
				} else {
					result.SobelHist = append([]byte(nil), test.payload...)
				}
				pool.results <- result
			}
			features := make(chan proto.FeatureItem, 1)
			task := proto.Phase2Task{TaskID: "cached-" + test.name, Stage: test.stage, Items: []proto.Phase2Item{item}}
			ack, start := manager.Prepare(task, func(msgType uint8, value any) error {
				if msgType == proto.MsgFeatureResult {
					features <- value.(*proto.FeatureResult).Items[0]
				}
				return nil
			})
			if !ack.Accepted || start == nil {
				t.Fatalf("ack=%#v", ack)
			}
			start()
			select {
			case feature := <-features:
				if feature.FieldsDone != test.field ||
					(test.field == proto.FieldPHashParts && !bytes.Equal(feature.PHashParts, test.payload)) ||
					(test.field == proto.FieldSobelHist && !bytes.Equal(feature.SobelHist, test.payload)) {
					t.Fatalf("cached feature=%#v", feature)
				}
			case <-time.After(time.Second):
				t.Fatal("cached feature result was not emitted")
			}
			submitted := pool.submittedSnapshot()
			if len(submitted) != 1 || submitted[0].FieldsMask != test.field || submitted[0].ScreenStage != worker.ScreenStage(test.stage) {
				t.Fatalf("cached jobs=%#v, want one original stage request", submitted)
			}
		})
	}
}

func TestPhase2PartialVideoCacheStillRequestsOriginalFrameMask(t *testing.T) {
	pool := newPhase2FakePool()
	router := NewPoolRouter(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer close(pool.results)
	path := `D:\cache\partial-video.mp4`
	item := validPhase2Video(path, proto.FrameMaskFull)
	item.FieldsMask = proto.FieldVideo6FPHash
	manager := NewPhase2ManagerWithRuntime(
		"machine-a",
		&phase2CommittedFake{states: map[string]store.Phase2Committed{path: {
			MissingFields: proto.FieldVideo6FPHash, MissingFrames: 0x04,
		}}},
		pool, router,
		func(string) (int64, bool, error) { return 1, false, nil },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	pool.onSubmit = func(job worker.JobMsg) {
		pool.results <- &worker.JobResultMsg{
			JobID: job.JobID, ScanTaskID: job.ScanTaskID, Path: job.Path, Kind: job.Kind,
			Phase: job.Phase, ScreenStage: job.ScreenStage, Source: job.Source,
			SHA512: append([]byte(nil), job.KnownSHA...), FieldsDone: job.FieldsMask,
		}
	}
	task := proto.Phase2Task{TaskID: "partial-video-cache", Stage: proto.ScreenStageTwo, Items: []proto.Phase2Item{item}}
	ack, start := manager.Prepare(task, nil)
	if !ack.Accepted || start == nil {
		t.Fatalf("ack=%#v", ack)
	}
	start()
	deadline := time.Now().Add(time.Second)
	for len(pool.submittedSnapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	submitted := pool.submittedSnapshot()
	if len(submitted) != 1 || submitted[0].FieldsMask != proto.FieldVideo6FPHash || submitted[0].FrameMask != proto.FrameMaskFull {
		t.Fatalf("partial-cache job=%#v, want original field/frame masks", submitted)
	}
}

func TestPhase2FeatureMapsAllFixedFrameFailuresWithoutPayload(t *testing.T) {
	accepted := validPhase2Video(`D:\private\all-failed.mp4`, proto.FrameMaskFull)
	accepted.FieldsMask = proto.FieldVideo6FPHash
	job := &worker.JobMsg{
		Kind: worker.MediaVideo, FieldsMask: worker.MaskVideo6FPHash, FrameMask: worker.FrameMaskFull,
		ScreenStage: worker.ScreenStageTwo, Source: worker.JobSourceManager,
	}
	result := &worker.JobResultMsg{ScreenStage: job.ScreenStage, Source: job.Source}
	for index := range result.FrameResults {
		result.FrameResults[index] = worker.FrameResult{FrameIdx: index, Status: -30 - int32(index)}
	}
	feature := phase2FeatureFromWorker(accepted, job, result)
	if feature.Status != proto.StatusFailed || len(feature.Frames) != 6 {
		t.Fatalf("all-failed feature=%#v", feature)
	}
	for index, frame := range feature.Frames {
		if frame.FrameIdx != index || frame.Error != fmt.Sprintf("native_status_%d", -30-int32(index)) ||
			len(frame.PHashParts) != 0 || len(frame.SobelHist) != 0 || len(frame.PDQ256) != 0 {
			t.Fatalf("feature frame[%d]=%#v", index, frame)
		}
	}
}

func TestPhase2PrepareConcurrentDuplicateCreatesOneLogicalExecution(t *testing.T) {
	manager := NewPhase2Manager("machine-a")
	var executions atomic.Int64
	executed := make(chan struct{})
	manager.run = func(state *phase2State) {
		executions.Add(1)
		manager.complete(state, proto.TaskStats{
			Total: 1,
			Done:  1,
		})
		close(executed)
	}
	task := proto.Phase2Task{
		TaskID: "phase2-concurrent",
		Items: []proto.Phase2Item{
			validPhase2Image(`D:\media\one.jpg`),
		},
	}

	const callers = 32
	acks := make(chan proto.TaskAck, callers)
	var calls sync.WaitGroup
	calls.Add(callers)
	for range callers {
		go func() {
			defer calls.Done()
			ack, start := manager.Prepare(task, nil)
			acks <- ack
			if start != nil {
				start()
			}
		}()
	}
	calls.Wait()
	close(acks)
	<-executed

	var accepted, resumed, alreadyDone int
	for ack := range acks {
		if !ack.Accepted {
			t.Fatalf("duplicate Prepare rejected: %#v", ack)
		}
		switch ack.Reason {
		case "accepted":
			accepted++
		case "resumed":
			resumed++
		case "already_done":
			alreadyDone++
		default:
			t.Fatalf("duplicate Prepare reason=%q", ack.Reason)
		}
	}
	if accepted != 1 || resumed+alreadyDone != callers-1 {
		t.Fatalf("reasons accepted=%d resumed=%d done=%d, want 1/%d total",
			accepted, resumed, alreadyDone, callers-1)
	}
	if got := executions.Load(); got != 1 {
		t.Fatalf("logical executions=%d, want 1", got)
	}
	ack, start := manager.Prepare(task, nil)
	if !ack.Accepted || ack.Reason != "already_done" || start == nil ||
		ack.Stats == nil || ack.Stats.Done != 1 {
		t.Fatalf("completed duplicate ack=%#v start=%v", ack, start != nil)
	}
}

func TestPhase2PrepareRetainsImmutableEnvelopeAndRejectsAnyConflict(t *testing.T) {
	manager := NewPhase2Manager("machine-a")
	original := proto.Phase2Task{
		TaskID: "phase2-envelope",
		Items: []proto.Phase2Item{
			validPhase2Image(`D:\media\first.jpg`),
			validPhase2Image(`D:\media\second.jpg`),
		},
	}
	pristine := clonePhase2TaskForTest(original)

	ack, _ := manager.Prepare(original, nil)
	if !ack.Accepted || ack.Reason != "accepted" {
		t.Fatalf("first Prepare ack=%#v", ack)
	}
	original.Items[0].Path = `D:\caller-mutated.jpg`
	ack, _ = manager.Prepare(pristine, nil)
	if !ack.Accepted || ack.Reason != "resumed" {
		t.Fatalf("caller mutation changed retained envelope: %#v", ack)
	}

	conflicts := []proto.Phase2Task{
		{
			TaskID: pristine.TaskID,
			Items:  []proto.Phase2Item{pristine.Items[1], pristine.Items[0]},
		},
		clonePhase2TaskForTest(pristine),
		clonePhase2TaskForTest(pristine),
	}
	conflicts[1].Items[0].Size++
	conflicts[2].Items[1].FrameMask = 1
	for index, conflict := range conflicts {
		ack, start := manager.Prepare(conflict, nil)
		if ack.Accepted ||
			!strings.Contains(ack.Reason, "task_id envelope mismatch") ||
			start != nil {
			t.Fatalf("conflict[%d] ack=%#v start=%v", index, ack, start != nil)
		}
	}
}

func TestPhase2SchedulesPerPhysicalDiskAndDeepCopiesPartialResults(t *testing.T) {
	pool := newPhase2FakePool()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := NewPoolRouter(pool, log)
	committed := &phase2CommittedFake{states: map[string]store.Phase2Committed{
		`D:\disk-a\first.jpg`: {
			MissingFields: proto.FieldPHashParts | proto.FieldSobelHist,
		},
		`D:\disk-a\second.jpg`: {
			MissingFields: proto.FieldSobelHist,
		},
		`E:\disk-b\clip.mp4`: {
			MissingFields: proto.FieldVideo6F,
			MissingFrames: 1<<1 | 1<<4,
		},
		`F:\disk-c\already.jpg`: {},
	}}
	resolver := func(path string) (int64, bool, error) {
		switch path[0] {
		case 'D':
			return 10, false, nil
		case 'E':
			return 20, false, nil
		case 'F':
			return 30, true, nil
		default:
			return -1, false, errors.New("unknown disk")
		}
	}
	manager := NewPhase2ManagerWithRuntime(
		"machine-a",
		committed,
		pool,
		router,
		resolver,
		log,
	)

	firstSubmitted := make(chan struct{})
	secondSubmitted := make(chan struct{})
	otherDiskSubmitted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var firstResult *worker.JobResultMsg
	var videoResult *worker.JobResultMsg
	pool.onSubmit = func(job worker.JobMsg) {
		switch job.Path {
		case `D:\disk-a\first.jpg`:
			close(firstSubmitted)
			<-releaseFirst
			firstResult = &worker.JobResultMsg{
				JobID: job.JobID, ScanTaskID: job.ScanTaskID,
				Path: job.Path, Kind: job.Kind, Phase: worker.Phase2,
				ScreenStage: job.ScreenStage, Source: job.Source,
				SHA512:     append([]byte(nil), job.KnownSHA...),
				FieldsDone: worker.MaskPHashParts,
				PHashParts: []byte{1, 2, 3},
				Errors: []worker.FieldError{{
					Field: worker.MaskSobelHist,
					Stage: "sobel",
					Msg:   "controlled partial",
				}},
			}
			pool.results <- firstResult
		case `D:\disk-a\second.jpg`:
			close(secondSubmitted)
			pool.results <- &worker.JobResultMsg{
				JobID: job.JobID, ScanTaskID: job.ScanTaskID,
				Path: job.Path, Kind: job.Kind, Phase: worker.Phase2,
				ScreenStage: job.ScreenStage, Source: job.Source,
				SHA512:     append([]byte(nil), job.KnownSHA...),
				FieldsDone: worker.MaskSobelHist,
				SobelHist:  []byte{4, 5, 6},
			}
		case `E:\disk-b\clip.mp4`:
			close(otherDiskSubmitted)
			videoResult = &worker.JobResultMsg{
				JobID: job.JobID, ScanTaskID: job.ScanTaskID,
				Path: job.Path, Kind: job.Kind, Phase: worker.Phase2,
				ScreenStage: job.ScreenStage, Source: job.Source,
				SHA512: append([]byte(nil), job.KnownSHA...),
				Frames: []worker.FrameFeature{{
					FrameIdx:   1,
					TimeMS:     750,
					PDQ256:     bytes.Repeat([]byte{7}, 32),
					Quality:    80,
					PHashParts: []byte{8, 9},
					SobelHist:  []byte{10, 11},
				}},
			}
			pool.results <- videoResult
		case `F:\disk-c\already.jpg`:
			pool.results <- &worker.JobResultMsg{
				JobID: job.JobID, ScanTaskID: job.ScanTaskID,
				Path: job.Path, Kind: job.Kind, Phase: worker.Phase2,
				ScreenStage: job.ScreenStage, Source: job.Source,
				SHA512: append([]byte(nil), job.KnownSHA...), FieldsDone: job.FieldsMask,
				PHashParts: []byte{12}, SobelHist: []byte{13},
			}
		default:
			t.Errorf("unexpected pool submission: %#v", job)
		}
	}

	task := proto.Phase2Task{
		TaskID: "phase2-schedule",
		Items: []proto.Phase2Item{
			validPhase2Image(`D:\disk-a\first.jpg`),
			validPhase2Image(`D:\disk-a\second.jpg`),
			validPhase2Video(`E:\disk-b\clip.mp4`, 0),
			validPhase2Image(`F:\disk-c\already.jpg`),
		},
	}
	done := make(chan proto.TaskDone, 1)
	var messagesMu sync.Mutex
	var batches []proto.FeatureResult
	sender := func(msgType uint8, value any) error {
		switch msgType {
		case proto.MsgFeatureResult:
			messagesMu.Lock()
			batches = append(batches, *value.(*proto.FeatureResult))
			messagesMu.Unlock()
		case proto.MsgTaskDone:
			done <- *value.(*proto.TaskDone)
		}
		return nil
	}
	ack, start := manager.Prepare(task, sender)
	if !ack.Accepted || start == nil {
		t.Fatalf("Prepare ack=%#v start=%v", ack, start != nil)
	}
	start()

	select {
	case <-firstSubmitted:
	case <-time.After(time.Second):
		t.Fatal("first disk did not submit")
	}
	select {
	case <-otherDiskSubmitted:
	case <-time.After(time.Second):
		t.Fatal("independent physical disk made no progress")
	}
	select {
	case <-secondSubmitted:
		t.Fatal("same physical disk reordered before first terminal")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case <-secondSubmitted:
	case <-time.After(time.Second):
		t.Fatal("second same-disk item did not submit after first terminal")
	}

	var final proto.TaskDone
	select {
	case final = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Phase2 task did not finish")
	}
	if final.Stats.Total != 4 || final.Stats.Done != 4 ||
		final.Stats.Skipped != 0 {
		t.Fatalf("TaskDone stats=%#v", final.Stats)
	}

	submitted := pool.submittedSnapshot()
	if len(submitted) != 4 {
		t.Fatalf("submissions=%#v, want one original job per request", submitted)
	}
	jobs := make(map[string]worker.JobMsg, len(submitted))
	for _, job := range submitted {
		jobs[job.Path] = job
		if job.Phase != worker.Phase2 || len(job.KnownSHA) != 64 ||
			job.Size != 10 || job.MTimeMS != 20 {
			t.Fatalf("phase2 job mapping=%#v", job)
		}
	}
	if jobs[`D:\disk-a\first.jpg`].FieldsMask !=
		worker.MaskPHashParts|worker.MaskSobelHist {
		t.Fatalf("first mask=%#x", jobs[`D:\disk-a\first.jpg`].FieldsMask)
	}
	if jobs[`D:\disk-a\second.jpg`].FieldsMask != worker.MaskPHashParts|worker.MaskSobelHist {
		t.Fatalf("second mask=%#x", jobs[`D:\disk-a\second.jpg`].FieldsMask)
	}
	videoJob := jobs[`E:\disk-b\clip.mp4`]
	if videoJob.FieldsMask != worker.MaskVideo6F ||
		videoJob.FrameMask != worker.FrameMaskFull ||
		videoJob.DurationMS != 12000 {
		t.Fatalf("video job=%#v", videoJob)
	}

	messagesMu.Lock()
	items := make(map[string]proto.FeatureItem)
	var lastSequence uint64
	for _, batch := range batches {
		if batch.Seq <= lastSequence {
			t.Fatalf("non-monotonic batch sequence %d after %d",
				batch.Seq, lastSequence)
		}
		lastSequence = batch.Seq
		for _, item := range batch.Items {
			items[item.Path] = item
		}
	}
	messagesMu.Unlock()
	first := items[`D:\disk-a\first.jpg`]
	if first.Status != proto.StatusPartial ||
		first.FieldsDone != proto.FieldPHashParts ||
		!bytes.Equal(first.PHashParts, []byte{1, 2, 3}) ||
		len(first.FieldErrors) != 1 {
		t.Fatalf("partial image item=%#v", first)
	}
	video := items[`E:\disk-b\clip.mp4`]
	if video.Status != proto.StatusPartial || video.FieldsDone != 0 ||
		len(video.Frames) != 1 ||
		!bytes.Equal(video.Frames[0].PDQ256, bytes.Repeat([]byte{7}, 32)) {
		t.Fatalf("partial video item=%#v", video)
	}
	noop := items[`F:\disk-c\already.jpg`]
	if noop.Status != proto.StatusDone || noop.FieldsDone != proto.FieldPHashParts|proto.FieldSobelHist {
		t.Fatalf("cached item=%#v", noop)
	}

	firstResult.PHashParts[0] = 99
	firstResult.Errors[0].Msg = "mutated after publication"
	videoResult.Frames[0].PDQ256[0] = 99
	if first.PHashParts[0] != 1 ||
		first.FieldErrors[0].Msg != "controlled partial" ||
		video.Frames[0].PDQ256[0] != 7 {
		t.Fatalf("published payload aliased Worker memory: first=%#v video=%#v",
			first, video)
	}
	pool.mu.Lock()
	endCalls := pool.endTasks[task.TaskID]
	pool.mu.Unlock()
	if endCalls != 1 {
		t.Fatalf("EndTask calls=%d, want 1", endCalls)
	}
}

func TestPhase2FeatureFromWorkerFiltersFramesOutsideEffectiveMask(t *testing.T) {
	accepted := validPhase2Video(`D:\frames\filtered.mp4`, 1<<1)
	job := &worker.JobMsg{
		Path:       accepted.Path,
		Kind:       worker.MediaVideo,
		Phase:      worker.Phase2,
		FieldsMask: worker.MaskVideo6F,
		FrameMask:  accepted.FrameMask,
	}
	frame := func(index int) worker.FrameFeature {
		return worker.FrameFeature{
			FrameIdx:   index,
			TimeMS:     int64(index+1) * 1000,
			PDQ256:     bytes.Repeat([]byte{byte(index + 1)}, 32),
			Quality:    80,
			PHashParts: []byte{1},
			SobelHist:  []byte{2},
		}
	}
	result := &worker.JobResultMsg{
		Path:   accepted.Path,
		Kind:   worker.MediaVideo,
		Frames: []worker.FrameFeature{frame(1), frame(4)},
	}

	filtered := phase2FeatureFromWorker(accepted, job, result)
	if len(filtered.Frames) != 1 || filtered.Frames[0].FrameIdx != 1 {
		t.Fatalf("filtered Frames=%#v, want only requested frame 1", filtered.Frames)
	}

	job.FrameMask = 0
	full := phase2FeatureFromWorker(accepted, job, result)
	if len(full.Frames) != 2 {
		t.Fatalf("zero FrameMask Frames=%#v, want full-mask normalization", full.Frames)
	}
}

func TestPhase2FeatureFromWorkerMapsFixedVideoFramesWithoutCrossStagePayload(t *testing.T) {
	accepted := validPhase2Video(`D:\media\fixed-stage.mp4`, proto.FrameMaskFull)
	accepted.FieldsMask = proto.FieldVideo6FPHash
	job := &worker.JobMsg{
		Path: accepted.Path, Kind: worker.MediaVideo, Phase: worker.Phase2,
		ScreenStage: worker.ScreenStageTwo, Source: worker.JobSourceManager,
		FieldsMask: worker.MaskVideo6FPHash, FrameMask: worker.FrameMaskFull,
	}
	result := &worker.JobResultMsg{
		Path: accepted.Path, Kind: worker.MediaVideo, Phase: worker.Phase2,
		ScreenStage: worker.ScreenStageTwo, Source: worker.JobSourceManager,
		FieldsDone: worker.MaskVideo6FPHash, FramesDone: worker.FrameMaskFull,
	}
	for index := range result.FrameResults {
		result.FrameResults[index] = worker.FrameResult{
			FrameIdx: index, TimeMS: int64(index+1) * 1000,
			PHashParts: []byte{byte(index + 1)},
		}
	}

	feature := phase2FeatureFromWorker(accepted, job, result)
	if feature.Status != proto.StatusDone || feature.FieldsDone != proto.FieldVideo6FPHash || len(feature.Frames) != 6 {
		t.Fatalf("fixed-frame feature=%#v", feature)
	}
	for index, frame := range feature.Frames {
		if len(frame.PHashParts) == 0 || len(frame.SobelHist) != 0 || len(frame.PDQ256) != 0 {
			t.Fatalf("feature frame[%d] leaked payload: %#v", index, frame)
		}
	}
}

func TestPhase2CrashEmitsNoticeAndOneCrashTerminal(t *testing.T) {
	pool := newPhase2FakePool()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := NewPoolRouter(pool, log)
	path := `D:\crash\image.jpg`
	manager := NewPhase2ManagerWithRuntime(
		"machine-a",
		&phase2CommittedFake{states: map[string]store.Phase2Committed{
			path: {MissingFields: proto.FieldPHashParts},
		}},
		pool,
		router,
		func(string) (int64, bool, error) { return 1, false, nil },
		log,
	)
	pool.onSubmit = func(job worker.JobMsg) {
		pool.crashes <- worker.CrashRecord{
			JobID: job.JobID, ScanTaskID: job.ScanTaskID,
			File: job.Path, PID: 4321, ExitCode: -1,
			Reason: "watchdog_image",
		}
	}
	notices := make(chan proto.CrashNotice, 1)
	items := make(chan proto.FeatureItem, 1)
	done := make(chan proto.TaskDone, 1)
	task := proto.Phase2Task{
		TaskID: "phase2-crash",
		Items:  []proto.Phase2Item{validPhase2Image(path)},
	}
	_, start := manager.Prepare(task, func(msgType uint8, value any) error {
		switch msgType {
		case proto.MsgCrashNotice:
			notices <- *value.(*proto.CrashNotice)
		case proto.MsgFeatureResult:
			items <- value.(*proto.FeatureResult).Items[0]
		case proto.MsgTaskDone:
			done <- *value.(*proto.TaskDone)
		}
		return nil
	})
	start()
	select {
	case notice := <-notices:
		if notice.TaskID != task.TaskID || notice.Path != path ||
			notice.PID != 4321 {
			t.Fatalf("CrashNotice=%#v", notice)
		}
	case <-time.After(time.Second):
		t.Fatal("no CrashNotice")
	}
	select {
	case item := <-items:
		if item.Status != proto.StatusCrash || item.Path != path ||
			item.SHA512 != task.Items[0].SHA512 {
			t.Fatalf("crash FeatureItem=%#v", item)
		}
	case <-time.After(time.Second):
		t.Fatal("no crash FeatureItem")
	}
	select {
	case final := <-done:
		if final.Stats.Done != 1 || final.Stats.Failed != 1 ||
			final.Stats.Crashes != 1 {
			t.Fatalf("TaskDone=%#v", final)
		}
	case <-time.After(time.Second):
		t.Fatal("crash task did not finish")
	}
}

func TestPhase2PoolCloseSynthesizesEveryUnresolvedTerminalAndEndsTask(t *testing.T) {
	pool := newPhase2FakePool()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := NewPoolRouter(pool, log)
	paths := []string{`D:\close\a.jpg`, `E:\close\b.jpg`}
	manager := NewPhase2ManagerWithRuntime(
		"machine-a",
		&phase2CommittedFake{states: map[string]store.Phase2Committed{
			paths[0]: {MissingFields: proto.FieldPHashParts},
			paths[1]: {MissingFields: proto.FieldPHashParts},
		}},
		pool,
		router,
		func(path string) (int64, bool, error) {
			return int64(path[0]), false, nil
		},
		log,
	)
	var closeOnce sync.Once
	pool.onSubmit = func(worker.JobMsg) {
		closeOnce.Do(func() { close(pool.results) })
	}
	done := make(chan proto.TaskDone, 1)
	var mu sync.Mutex
	var got []proto.FeatureItem
	task := proto.Phase2Task{
		TaskID: "phase2-pool-close",
		Items: []proto.Phase2Item{
			validPhase2Image(paths[0]),
			validPhase2Image(paths[1]),
		},
	}
	_, start := manager.Prepare(task, func(msgType uint8, value any) error {
		if msgType == proto.MsgFeatureResult {
			mu.Lock()
			got = append(got, value.(*proto.FeatureResult).Items...)
			mu.Unlock()
		}
		if msgType == proto.MsgTaskDone {
			done <- *value.(*proto.TaskDone)
		}
		return nil
	})
	start()
	select {
	case final := <-done:
		if final.Stats.Done != 2 || final.Stats.Failed != 2 {
			t.Fatalf("TaskDone=%#v", final)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pool-close drain hung")
	}
	mu.Lock()
	if len(got) != 2 {
		t.Fatalf("pool-close terminal items=%#v", got)
	}
	for _, item := range got {
		if item.Status != proto.StatusFailed ||
			!strings.Contains(item.Err, "pool") {
			t.Fatalf("pool-close item=%#v", item)
		}
	}
	mu.Unlock()
	pool.mu.Lock()
	endCalls := pool.endTasks[task.TaskID]
	pool.mu.Unlock()
	if endCalls != 1 {
		t.Fatalf("EndTask calls=%d, want 1", endCalls)
	}
}

func TestPhase2CommittedSHAOwnershipMismatchIsStaleWithoutSkipOrSubmit(t *testing.T) {
	pool := newPhase2FakePool()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := NewPoolRouter(pool, log)
	path := `D:\ownership\changed.jpg`
	item := validPhase2Image(path)
	other := sha512.Sum512([]byte("different current owner"))
	manager := NewPhase2ManagerWithRuntime(
		"machine-a",
		&phase2CommittedFake{states: map[string]store.Phase2Committed{
			path: {
				SHA512:        hex.EncodeToString(other[:]),
				MissingFields: 0,
			},
		}},
		pool,
		router,
		func(string) (int64, bool, error) { return 1, false, nil },
		log,
	)
	t.Cleanup(func() {
		_ = manager.Shutdown(context.Background())
		close(pool.results)
	})
	terminal := runSinglePhase2Item(t, manager, "phase2-stale-owner", item)
	if terminal.Status != proto.StatusFailed ||
		terminal.SHA512 != item.SHA512 ||
		len(terminal.FieldErrors) != 1 ||
		terminal.FieldErrors[0].Field != 0 ||
		terminal.FieldErrors[0].Stage != "stale" ||
		len(terminal.PHashParts) != 0 ||
		len(terminal.SobelHist) != 0 {
		t.Fatalf("ownership mismatch terminal=%#v", terminal)
	}
	if got := len(pool.submittedSnapshot()); got != 0 {
		t.Fatalf("ownership mismatch submitted %d jobs, want 0", got)
	}
}

func TestPhase2FailuresAfterCommittedLookupUseOriginalFieldMask(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*phase2FakePool, *PoolRouter)
	}{
		{
			name: "register after router close",
			setup: func(pool *phase2FakePool, router *PoolRouter) {
				close(pool.results)
				waitForPoolRouterClosed(t, router)
			},
		},
		{
			name: "submit failure",
			setup: func(pool *phase2FakePool, _ *PoolRouter) {
				pool.submitErr = errors.New("controlled submit failure")
			},
		},
		{
			name: "route closes after submit",
			setup: func(pool *phase2FakePool, _ *PoolRouter) {
				pool.onSubmit = func(worker.JobMsg) {
					close(pool.results)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			pool := newPhase2FakePool()
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			router := NewPoolRouter(pool, log)
			path := `D:\pruned\image.jpg`
			item := validPhase2Image(path)
			manager := NewPhase2ManagerWithRuntime(
				"machine-a",
				&phase2CommittedFake{states: map[string]store.Phase2Committed{
					path: {
						SHA512:        item.SHA512,
						MissingFields: proto.FieldSobelHist,
					},
				}},
				pool,
				router,
				func(string) (int64, bool, error) { return 1, false, nil },
				log,
			)
			test.setup(pool, router)
			terminal := runSinglePhase2Item(
				t,
				manager,
				"phase2-pruned-"+test.name,
				item,
			)
			if terminal.Status != proto.StatusFailed ||
				len(terminal.FieldErrors) != 1 ||
				terminal.FieldErrors[0].Field != proto.FieldPHashParts|proto.FieldSobelHist ||
				terminal.FieldErrors[0].Stage != "worker" {
				t.Fatalf("pruned failure terminal=%#v", terminal)
			}
			_ = manager.Shutdown(context.Background())
		})
	}
}

func TestPhase2FailuresBeforeOwnershipUseFileLevelAccurateStage(t *testing.T) {
	for _, test := range []struct {
		name     string
		resolver Phase2DiskResolver
		storeErr error
		want     string
	}{
		{
			name: "disk resolution",
			resolver: func(string) (int64, bool, error) {
				return 0, false, errors.New("controlled disk failure")
			},
			want: "disk",
		},
		{
			name: "committed state read",
			resolver: func(string) (int64, bool, error) {
				return 1, false, nil
			},
			storeErr: errors.New("controlled store failure"),
			want:     "store",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			pool := newPhase2FakePool()
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			router := NewPoolRouter(pool, log)
			path := `D:\preownership\image.jpg`
			item := validPhase2Image(path)
			committed := &phase2CommittedFake{
				states: map[string]store.Phase2Committed{},
				errors: map[string]error{path: test.storeErr},
			}
			manager := NewPhase2ManagerWithRuntime(
				"machine-a",
				committed,
				pool,
				router,
				test.resolver,
				log,
			)
			terminal := runSinglePhase2Item(
				t,
				manager,
				"phase2-preownership-"+test.name,
				item,
			)
			if terminal.Status != proto.StatusFailed ||
				len(terminal.FieldErrors) != 1 ||
				terminal.FieldErrors[0].Field != 0 ||
				terminal.FieldErrors[0].Stage != test.want {
				t.Fatalf("pre-ownership terminal=%#v", terminal)
			}
			_ = manager.Shutdown(context.Background())
			close(pool.results)
		})
	}
}

func TestScanAndPhase2UseSharedRouterJobIDAllocator(t *testing.T) {
	scans, cleanup := newTestScanManager(t, nil, nil)
	defer cleanup()
	pool := newPhase2FakePool()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := NewPoolRouter(pool, log)
	scans.pool = pool
	scans.router = router
	scanWork, _ := scans.preparePending(
		&ScanState{Task: proto.ScanTask{TaskID: "phase1-owner"}},
		map[int64][]store.PendingFile{
			1: {{
				Path:        `D:\scan\phase1.jpg`,
				Size:        10,
				MTime:       20,
				MissingMask: worker.MaskImagePDQ,
			}},
		},
	)
	phase1 := scanWork[1][0]

	path := `E:\scan\phase2.jpg`
	phase2 := NewPhase2ManagerWithRuntime(
		"machine-a",
		&phase2CommittedFake{states: map[string]store.Phase2Committed{
			path: {MissingFields: proto.FieldPHashParts},
		}},
		pool,
		router,
		func(string) (int64, bool, error) { return 2, false, nil },
		log,
	)
	pool.onSubmit = func(job worker.JobMsg) {
		pool.results <- &worker.JobResultMsg{
			JobID: job.JobID, ScanTaskID: job.ScanTaskID,
			Path: job.Path, Kind: job.Kind, Phase: worker.Phase2,
			ScreenStage: job.ScreenStage, Source: job.Source,
			SHA512:     append([]byte(nil), job.KnownSHA...),
			FieldsDone: job.FieldsMask,
			PHashParts: []byte{1},
		}
	}
	done := make(chan proto.TaskDone, 1)
	_, start := phase2.Prepare(proto.Phase2Task{
		TaskID: "phase2-owner",
		Items:  []proto.Phase2Item{validPhase2Image(path)},
	}, captureTaskDone(done))
	start()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("phase2 task did not finish")
	}
	submitted := pool.submittedSnapshot()
	if len(submitted) != 1 {
		t.Fatalf("phase2 submissions=%#v", submitted)
	}
	if phase1.media.JobID == submitted[0].JobID {
		t.Fatalf("cross-phase JobID collision=%d", phase1.media.JobID)
	}
}

func TestPhase2SenderFailureKeepsWorkAndReconnectReplaysOrderedImmutableTerminal(t *testing.T) {
	pool := newPhase2FakePool()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := NewPoolRouter(pool, log)
	paths := []string{`D:\replay\a.jpg`, `D:\replay\b.jpg`}
	manager := NewPhase2ManagerWithRuntime(
		"machine-a",
		&phase2CommittedFake{states: map[string]store.Phase2Committed{
			paths[0]: {MissingFields: proto.FieldPHashParts},
			paths[1]: {MissingFields: proto.FieldPHashParts},
		}},
		pool,
		router,
		func(string) (int64, bool, error) { return 1, false, nil },
		log,
	)
	var sourceMu sync.Mutex
	var sources []*worker.JobResultMsg
	pool.onSubmit = func(job worker.JobMsg) {
		result := &worker.JobResultMsg{
			JobID: job.JobID, ScanTaskID: job.ScanTaskID,
			Path: job.Path, Kind: job.Kind, Phase: worker.Phase2,
			ScreenStage: job.ScreenStage, Source: job.Source,
			SHA512:     append([]byte(nil), job.KnownSHA...),
			FieldsDone: job.FieldsMask,
			PHashParts: []byte{byte(len(sources) + 1)},
		}
		sourceMu.Lock()
		sources = append(sources, result)
		sourceMu.Unlock()
		pool.results <- result
	}
	var oldFeatureCalls atomic.Int64
	oldSender := func(msgType uint8, _ any) error {
		if msgType == proto.MsgFeatureResult {
			oldFeatureCalls.Add(1)
			return errors.New("connection lost")
		}
		return nil
	}
	task := proto.Phase2Task{
		TaskID: "phase2-replay",
		Items: []proto.Phase2Item{
			validPhase2Image(paths[0]),
			validPhase2Image(paths[1]),
		},
	}
	_, start := manager.Prepare(task, oldSender)
	start()
	waitForPhase2EndTask(t, pool, task.TaskID)

	sourceMu.Lock()
	for _, source := range sources {
		source.PHashParts[0] = 99
	}
	sourceMu.Unlock()
	var replayMu sync.Mutex
	var replayTypes []uint8
	var replaySeq []uint64
	var replayBytes []byte
	replayedDone := make(chan proto.TaskDone, 1)
	newSender := func(msgType uint8, value any) error {
		replayMu.Lock()
		replayTypes = append(replayTypes, msgType)
		if msgType == proto.MsgFeatureResult {
			result := value.(*proto.FeatureResult)
			replaySeq = append(replaySeq, result.Seq)
			replayBytes = append(
				replayBytes,
				result.Items[0].PHashParts[0],
			)
		}
		replayMu.Unlock()
		if msgType == proto.MsgTaskDone {
			replayedDone <- *value.(*proto.TaskDone)
		}
		return nil
	}
	ack, replay, detach := manager.PrepareConnection(task, newSender)
	if !ack.Accepted || ack.Reason != "already_done" ||
		replay == nil || detach == nil {
		t.Fatalf("reconnect ack=%#v replay=%v detach=%v",
			ack, replay != nil, detach != nil)
	}
	replay()
	select {
	case final := <-replayedDone:
		if final.Stats.Done != 2 {
			t.Fatalf("replayed TaskDone=%#v", final)
		}
	case <-time.After(time.Second):
		t.Fatal("reconnect did not replay TaskDone")
	}
	detach()
	replayMu.Lock()
	if !bytes.Equal(replaySeqBytes(replaySeq), []byte{1, 2}) ||
		!bytes.Equal(replayBytes, []byte{1, 2}) ||
		len(replayTypes) != 3 ||
		replayTypes[0] != proto.MsgFeatureResult ||
		replayTypes[1] != proto.MsgFeatureResult ||
		replayTypes[2] != proto.MsgTaskDone {
		t.Fatalf("replay types=%v seq=%v bytes=%v",
			replayTypes, replaySeq, replayBytes)
	}
	replayMu.Unlock()
	if oldFeatureCalls.Load() != 1 {
		t.Fatalf("failed old sender FeatureResult calls=%d, want detach after 1",
			oldFeatureCalls.Load())
	}
	if got := len(pool.submittedSnapshot()); got != 2 {
		t.Fatalf("reconnect resubmitted jobs: submissions=%d, want 2", got)
	}
	pool.mu.Lock()
	endCalls := pool.endTasks[task.TaskID]
	pool.mu.Unlock()
	if endCalls != 1 {
		t.Fatalf("logical EndTask calls=%d, want 1", endCalls)
	}
}

func TestPhase2OldConnectionDetachCannotClearNewBinding(t *testing.T) {
	pool := newPhase2FakePool()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := NewPoolRouter(pool, log)
	path := `D:\binding\image.jpg`
	manager := NewPhase2ManagerWithRuntime(
		"machine-a",
		&phase2CommittedFake{states: map[string]store.Phase2Committed{
			path: {MissingFields: proto.FieldPHashParts},
		}},
		pool,
		router,
		func(string) (int64, bool, error) { return 1, false, nil },
		log,
	)
	release := make(chan struct{})
	pool.onSubmit = func(job worker.JobMsg) {
		<-release
		pool.results <- &worker.JobResultMsg{
			JobID: job.JobID, ScanTaskID: job.ScanTaskID,
			Path: job.Path, Kind: job.Kind, Phase: worker.Phase2,
			ScreenStage: job.ScreenStage, Source: job.Source,
			SHA512:     append([]byte(nil), job.KnownSHA...),
			FieldsDone: job.FieldsMask, PHashParts: []byte{1},
		}
	}
	task := proto.Phase2Task{
		TaskID: "phase2-binding",
		Items:  []proto.Phase2Item{validPhase2Image(path)},
	}
	_, startOld, detachOld := manager.PrepareConnection(
		task,
		func(uint8, any) error { return nil },
	)
	startOld()
	newDone := make(chan struct{}, 1)
	_, startNew, detachNew := manager.PrepareConnection(
		task,
		func(msgType uint8, _ any) error {
			if msgType == proto.MsgTaskDone {
				newDone <- struct{}{}
			}
			return nil
		},
	)
	detachOld()
	startNew()
	close(release)
	select {
	case <-newDone:
	case <-time.After(time.Second):
		t.Fatal("old connection detach cleared newer sender binding")
	}
	detachNew()
}

func TestPhase2ReconnectBindingStaysPendingUntilAckActivation(t *testing.T) {
	manager := NewPhase2Manager("machine-a")
	task := proto.Phase2Task{
		TaskID: "phase2-pending-binding",
		Items:  []proto.Phase2Item{validPhase2Image(`D:\binding\pending.jpg`)},
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	manager.run = func(state *phase2State) {
		close(entered)
		<-release
		state.send(proto.MsgTaskProgress, &proto.TaskProgress{TaskID: task.TaskID})
		state.publish(proto.FeatureItem{
			Path: task.Items[0].Path, Status: proto.StatusDone,
		})
		manager.complete(state, proto.TaskStats{Total: 1, Done: 1})
		close(finished)
	}
	_, startOld, detachOld := manager.PrepareConnection(task, nil)
	startOld()
	<-entered

	newMessages := make(chan uint8, 4)
	ack, activate, detachNew := manager.PrepareConnection(
		task,
		func(msgType uint8, _ any) error {
			newMessages <- msgType
			return nil
		},
	)
	if !ack.Accepted || ack.Reason != "resumed" || activate == nil {
		t.Fatalf("reconnect ack=%#v activate=%v", ack, activate != nil)
	}
	close(release)
	<-finished
	select {
	case msgType := <-newMessages:
		t.Fatalf("pending reconnect received message %#x before Ack activation", msgType)
	default:
	}

	activate()
	for _, want := range []uint8{proto.MsgFeatureResult, proto.MsgTaskDone} {
		select {
		case got := <-newMessages:
			if got != want {
				t.Fatalf("activated replay type=%#x, want %#x", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("activated reconnect did not replay %#x", want)
		}
	}
	detachOld()
	detachNew()
}

func TestPhase2ConcurrentFirstAdmissionsReplayAfterEachAckActivation(t *testing.T) {
	manager := NewPhase2Manager("machine-a")
	task := proto.Phase2Task{
		TaskID: "phase2-concurrent-first-admission",
		Items:  []proto.Phase2Item{validPhase2Image(`D:\binding\concurrent.jpg`)},
	}
	manager.run = func(state *phase2State) {
		state.publish(proto.FeatureItem{
			Path: task.Items[0].Path, Status: proto.StatusDone,
		})
		manager.complete(state, proto.TaskStats{Total: 1, Done: 1})
	}
	firstMessages := make(chan uint8, 4)
	secondMessages := make(chan uint8, 4)
	firstAck, activateFirst, detachFirst := manager.PrepareConnection(
		task,
		func(msgType uint8, _ any) error {
			firstMessages <- msgType
			return nil
		},
	)
	secondAck, activateSecond, detachSecond := manager.PrepareConnection(
		task,
		func(msgType uint8, _ any) error {
			secondMessages <- msgType
			return nil
		},
	)
	if firstAck.Reason != "accepted" || secondAck.Reason != "resumed" {
		t.Fatalf("concurrent admission acks first=%#v second=%#v", firstAck, secondAck)
	}

	activateSecond()
	for _, want := range []uint8{proto.MsgFeatureResult, proto.MsgTaskDone} {
		select {
		case got := <-secondMessages:
			if got != want {
				t.Fatalf("second activation type=%#x, want %#x", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("second activation did not receive %#x", want)
		}
	}
	select {
	case got := <-firstMessages:
		t.Fatalf("unactivated first admission received %#x", got)
	default:
	}

	activateFirst()
	for _, want := range []uint8{proto.MsgFeatureResult, proto.MsgTaskDone} {
		select {
		case got := <-firstMessages:
			if got != want {
				t.Fatalf("first activation replay type=%#x, want %#x", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("first activation did not replay %#x", want)
		}
	}
	detachFirst()
	detachSecond()
}

func TestPhase2ShutdownRejectsAdmissionWaitsForDrainAndRemovesTaskState(t *testing.T) {
	pool := newPhase2FakePool()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := NewPoolRouter(pool, log)
	path := `D:\shutdown\image.jpg`
	manager := NewPhase2ManagerWithRuntime(
		"machine-a",
		&phase2CommittedFake{states: map[string]store.Phase2Committed{
			path: {MissingFields: proto.FieldPHashParts},
		}},
		pool,
		router,
		func(string) (int64, bool, error) { return 1, false, nil },
		log,
	)
	submitted := make(chan worker.JobMsg, 1)
	release := make(chan struct{})
	pool.onSubmit = func(job worker.JobMsg) {
		submitted <- job
		<-release
		pool.results <- &worker.JobResultMsg{
			JobID: job.JobID, ScanTaskID: job.ScanTaskID,
			Path: job.Path, Kind: job.Kind, Phase: worker.Phase2,
			ScreenStage: job.ScreenStage, Source: job.Source,
			SHA512:     append([]byte(nil), job.KnownSHA...),
			FieldsDone: job.FieldsMask, PHashParts: []byte{1},
		}
	}
	task := proto.Phase2Task{
		TaskID: "phase2-shutdown",
		Items:  []proto.Phase2Item{validPhase2Image(path)},
	}
	_, start := manager.Prepare(task, nil)
	start()
	select {
	case <-submitted:
	case <-time.After(time.Second):
		t.Fatal("job was not submitted before shutdown")
	}
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- manager.Shutdown(context.Background())
	}()
	waitForPhase2Closing(t, manager)
	rejected, rejectedStart := manager.Prepare(proto.Phase2Task{
		TaskID: "phase2-after-shutdown",
		Items:  []proto.Phase2Item{validPhase2Image(`D:\shutdown\late.jpg`)},
	}, nil)
	if rejected.Accepted ||
		!strings.Contains(rejected.Reason, "shutting down") ||
		rejectedStart != nil {
		t.Fatalf("shutdown admission=%#v start=%v",
			rejected, rejectedStart != nil)
	}
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before terminal: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not drain accepted task")
	}
	manager.mu.Lock()
	retained := len(manager.tasks)
	manager.mu.Unlock()
	if retained != 0 {
		t.Fatalf("shutdown retained %d task entries, want 0", retained)
	}
	pool.mu.Lock()
	endCalls := pool.endTasks[task.TaskID]
	pool.mu.Unlock()
	if endCalls != 1 {
		t.Fatalf("shutdown EndTask calls=%d, want 1", endCalls)
	}
	close(pool.results)
}

func TestPhase2ShutdownDeadlineCancelsBlockedCommittedStateBeforeReturning(t *testing.T) {
	pool := newPhase2FakePool()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := NewPoolRouter(pool, log)
	blocked := &cancellablePhase2CommittedStore{
		entered:  make(chan struct{}),
		canceled: make(chan struct{}),
		release:  make(chan struct{}),
	}
	manager := NewPhase2ManagerWithRuntime(
		"machine-a",
		blocked,
		pool,
		router,
		func(string) (int64, bool, error) { return 1, false, nil },
		log,
	)
	t.Cleanup(func() {
		blocked.releaseOnce.Do(func() { close(blocked.release) })
		close(pool.results)
	})
	task := proto.Phase2Task{
		TaskID: "phase2-cancel-store",
		Items: []proto.Phase2Item{
			validPhase2Image(`D:\shutdown\blocked-store.jpg`),
		},
	}
	_, start := manager.Prepare(task, nil)
	start()
	select {
	case <-blocked.entered:
	case <-time.After(time.Second):
		t.Fatal("committed-state read did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := manager.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error=%v, want deadline exceeded", err)
	}
	select {
	case <-blocked.canceled:
	default:
		t.Fatal("Shutdown returned while committed-state goroutine was still blocked")
	}
	manager.mu.Lock()
	retained := len(manager.tasks)
	manager.mu.Unlock()
	if retained != 0 {
		t.Fatalf("Shutdown retained %d task entries after cancellation", retained)
	}
}

func TestPhase2ShutdownDeadlineCancelsBlockedRouteBeforeReturning(t *testing.T) {
	pool := newPhase2FakePool()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := NewPoolRouter(pool, log)
	path := `D:\shutdown\blocked-route.jpg`
	manager := NewPhase2ManagerWithRuntime(
		"machine-a",
		&phase2CommittedFake{states: map[string]store.Phase2Committed{
			path: {MissingFields: proto.FieldPHashParts},
		}},
		pool,
		router,
		func(string) (int64, bool, error) { return 1, false, nil },
		log,
	)
	t.Cleanup(func() { close(pool.results) })
	submitted := make(chan struct{})
	pool.onSubmit = func(worker.JobMsg) { close(submitted) }
	task := proto.Phase2Task{
		TaskID: "phase2-cancel-route",
		Items:  []proto.Phase2Item{validPhase2Image(path)},
	}
	_, start := manager.Prepare(task, nil)
	start()
	select {
	case <-submitted:
	case <-time.After(time.Second):
		t.Fatal("route-blocked job was not submitted")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := manager.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error=%v, want deadline exceeded", err)
	}
	router.mu.Lock()
	routes := len(router.routes)
	router.mu.Unlock()
	if routes != 0 {
		t.Fatalf("Shutdown returned with %d registered worker routes", routes)
	}
	manager.mu.Lock()
	retained := len(manager.tasks)
	manager.mu.Unlock()
	if retained != 0 {
		t.Fatalf("Shutdown retained %d task entries after route cancellation", retained)
	}
}

func TestPhase2ShutdownDisconnectsInFlightSenderBeforeWaiting(t *testing.T) {
	manager := NewPhase2Manager("machine-a")
	task := proto.Phase2Task{
		TaskID: "phase2-cancel-sender",
		Items: []proto.Phase2Item{
			validPhase2Image(`D:\shutdown\blocked-sender.jpg`),
		},
	}
	senderEntered := make(chan struct{})
	senderReleased := make(chan struct{})
	runFinished := make(chan struct{})
	var releaseOnce sync.Once
	var disconnectCalls atomic.Int64
	manager.run = func(state *phase2State) {
		state.send(proto.MsgTaskProgress, &proto.TaskProgress{TaskID: task.TaskID})
		manager.complete(state, proto.TaskStats{Total: 1, Done: 1})
		close(runFinished)
	}
	_, start, _ := manager.PrepareConnectionWithDisconnect(
		task,
		func(uint8, any) error {
			close(senderEntered)
			<-senderReleased
			return errors.New("connection closed")
		},
		func() {
			disconnectCalls.Add(1)
			releaseOnce.Do(func() { close(senderReleased) })
		},
	)
	start()
	select {
	case <-senderEntered:
	case <-time.After(time.Second):
		t.Fatal("sender call did not enter")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-runFinished:
	default:
		t.Fatal("Shutdown returned before in-flight sender and task goroutine exited")
	}
	if got := disconnectCalls.Load(); got != 1 {
		t.Fatalf("connection disconnect calls=%d, want exactly 1", got)
	}
}

func TestPhase2ShutdownDeadlineStopsBlockedPoolSubmitBeforeReturning(t *testing.T) {
	pool := newBlockingSubmitPool()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := NewPoolRouter(pool, log)
	path := `D:\shutdown\blocked-submit.jpg`
	manager := NewPhase2ManagerWithRuntime(
		"machine-a",
		&phase2CommittedFake{states: map[string]store.Phase2Committed{
			path: {MissingFields: proto.FieldPHashParts},
		}},
		pool,
		router,
		func(string) (int64, bool, error) { return 1, false, nil },
		log,
	)
	t.Cleanup(func() { close(pool.results) })
	task := proto.Phase2Task{
		TaskID: "phase2-cancel-submit",
		Items:  []proto.Phase2Item{validPhase2Image(path)},
	}
	_, start := manager.Prepare(task, nil)
	start()
	select {
	case <-pool.submitEntered:
	case <-time.After(time.Second):
		t.Fatal("pool Submit did not block")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- manager.Shutdown(ctx) }()
	var shutdownErr error
	select {
	case shutdownErr = <-shutdownDone:
	case <-time.After(200 * time.Millisecond):
		pool.StopAccepting()
		<-shutdownDone
		t.Fatal("Shutdown deadline did not stop the blocked pool Submit")
	}
	if !errors.Is(shutdownErr, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error=%v, want deadline exceeded", shutdownErr)
	}
	select {
	case <-pool.submitExited:
	default:
		t.Fatal("Shutdown returned while pool Submit goroutine was still blocked")
	}
	if calls := pool.stopCalls.Load(); calls != 1 {
		t.Fatalf("StopAccepting calls=%d, want 1", calls)
	}
	router.mu.Lock()
	routes := len(router.routes)
	router.mu.Unlock()
	if routes != 0 {
		t.Fatalf("Shutdown returned with %d routes after blocked Submit", routes)
	}
	manager.mu.Lock()
	retained := len(manager.tasks)
	manager.mu.Unlock()
	if retained != 0 {
		t.Fatalf("Shutdown retained %d task entries after blocked Submit", retained)
	}
}

func TestPhase2ShutdownCancelsAckedPendingBindingBeforeLateActivation(t *testing.T) {
	pool := newPhase2FakePool()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := NewPoolRouter(pool, log)
	path := `D:\shutdown\pending-acked-route.jpg`
	manager := NewPhase2ManagerWithRuntime(
		"machine-a",
		&phase2CommittedFake{states: map[string]store.Phase2Committed{
			path: {MissingFields: proto.FieldPHashParts},
		}},
		pool,
		router,
		func(string) (int64, bool, error) { return 1, false, nil },
		log,
	)
	t.Cleanup(func() { close(pool.results) })
	submitted := make(chan struct{})
	pool.onSubmit = func(worker.JobMsg) { close(submitted) }
	task := proto.Phase2Task{
		TaskID: "phase2-shutdown-pending-acked",
		Items:  []proto.Phase2Item{validPhase2Image(path)},
	}
	var messages atomic.Int64
	var disconnects atomic.Int64
	ack, lateActivate, detach := manager.PrepareConnectionWithDisconnect(
		task,
		func(uint8, any) error {
			messages.Add(1)
			return nil
		},
		func() { disconnects.Add(1) },
	)
	if !ack.Accepted || lateActivate == nil || detach == nil {
		t.Fatalf("pending admission ack=%#v activate=%v detach=%v",
			ack, lateActivate != nil, detach != nil)
	}
	manager.mu.Lock()
	state := manager.tasks[task.TaskID]
	manager.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := manager.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error=%v, want deadline exceeded", err)
	}
	select {
	case <-submitted:
	default:
		t.Fatal("Shutdown did not start the accepted pending task")
	}

	lateActivate()
	state.send(proto.MsgTaskProgress, &proto.TaskProgress{TaskID: task.TaskID})
	state.mu.Lock()
	bound := state.sender != nil
	state.mu.Unlock()
	detach()

	if bound {
		t.Fatal("late post-Shutdown activation rebound a sender")
	}
	if got := messages.Load(); got != 0 {
		t.Fatalf("post-Shutdown pending binding sent/replayed %d messages", got)
	}
	if got := disconnects.Load(); got != 1 {
		t.Fatalf("pending connection disconnect calls=%d, want exactly 1", got)
	}
	router.mu.Lock()
	routes := len(router.routes)
	router.mu.Unlock()
	if routes != 0 {
		t.Fatalf("Shutdown returned with %d routes", routes)
	}
	manager.mu.Lock()
	retained := len(manager.tasks)
	connections := len(manager.connections)
	manager.mu.Unlock()
	if retained != 0 {
		t.Fatalf("Shutdown retained %d task entries", retained)
	}
	if connections != 0 {
		t.Fatalf("Shutdown retained %d connection bindings", connections)
	}
}

func TestPhase2ShutdownCancelsConcurrentFirstAndResumePendingBindings(t *testing.T) {
	manager := NewPhase2Manager("machine-a")
	task := proto.Phase2Task{
		TaskID: "phase2-shutdown-concurrent-pending",
		Items: []proto.Phase2Item{
			validPhase2Image(`D:\shutdown\concurrent-pending.jpg`),
		},
	}
	var runs atomic.Int64
	runFinished := make(chan struct{})
	manager.run = func(state *phase2State) {
		runs.Add(1)
		manager.complete(state, proto.TaskStats{Total: 1, Done: 1})
		close(runFinished)
	}
	var firstMessages, secondMessages atomic.Int64
	var firstDisconnects, secondDisconnects atomic.Int64
	firstAck, activateFirst, detachFirst := manager.PrepareConnectionWithDisconnect(
		task,
		func(uint8, any) error {
			firstMessages.Add(1)
			return nil
		},
		func() { firstDisconnects.Add(1) },
	)
	secondAck, activateSecond, detachSecond := manager.PrepareConnectionWithDisconnect(
		task,
		func(uint8, any) error {
			secondMessages.Add(1)
			return nil
		},
		func() { secondDisconnects.Add(1) },
	)
	if firstAck.Reason != "accepted" || secondAck.Reason != "resumed" {
		t.Fatalf("pending acks first=%#v second=%#v", firstAck, secondAck)
	}
	manager.mu.Lock()
	state := manager.tasks[task.TaskID]
	manager.mu.Unlock()

	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runFinished:
	default:
		t.Fatal("Shutdown returned before accepted task goroutine exited")
	}
	activateSecond()
	activateFirst()
	state.send(proto.MsgTaskProgress, &proto.TaskProgress{TaskID: task.TaskID})
	state.mu.Lock()
	bound := state.sender != nil
	state.mu.Unlock()
	detachFirst()
	detachSecond()

	if bound {
		t.Fatal("late concurrent pending activation rebound a sender")
	}
	if got := firstMessages.Load(); got != 0 {
		t.Fatalf("first pending sender received %d post-Shutdown messages", got)
	}
	if got := secondMessages.Load(); got != 0 {
		t.Fatalf("resume pending sender received %d post-Shutdown messages", got)
	}
	if got := firstDisconnects.Load(); got != 1 {
		t.Fatalf("first pending disconnect calls=%d, want 1", got)
	}
	if got := secondDisconnects.Load(); got != 1 {
		t.Fatalf("resume pending disconnect calls=%d, want 1", got)
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("logical task runs=%d, want 1", got)
	}
	manager.mu.Lock()
	retained := len(manager.tasks)
	connections := len(manager.connections)
	manager.mu.Unlock()
	if retained != 0 {
		t.Fatalf("Shutdown retained %d task entries", retained)
	}
	if connections != 0 {
		t.Fatalf("Shutdown retained %d connection bindings", connections)
	}
}

func TestPhase2CompleteLinearizesEndTaskBeforeBlockingTaskDoneSender(t *testing.T) {
	pool := newPhase2FakePool()
	manager := NewPhase2Manager("machine-a")
	manager.pool = pool
	task := proto.Phase2Task{
		TaskID: "phase2-end-task-before-done",
		Items: []proto.Phase2Item{
			validPhase2Image(`D:\terminal\end-before-done.jpg`),
		},
	}
	senderEntered := make(chan int, 1)
	releaseSender := make(chan struct{})
	runFinished := make(chan struct{})
	var doneCalls atomic.Int64
	manager.run = func(state *phase2State) {
		manager.complete(state, proto.TaskStats{Total: 1, Done: 1})
		close(runFinished)
	}
	ack, start, detach := manager.PrepareConnection(
		task,
		func(msgType uint8, _ any) error {
			if msgType != proto.MsgTaskDone {
				return nil
			}
			doneCalls.Add(1)
			pool.mu.Lock()
			endCalls := pool.endTasks[task.TaskID]
			pool.mu.Unlock()
			senderEntered <- endCalls
			<-releaseSender
			return nil
		},
	)
	if !ack.Accepted || start == nil || detach == nil {
		t.Fatalf("PrepareConnection ack=%#v start=%v detach=%v",
			ack, start != nil, detach != nil)
	}
	start()
	observedEndCalls := <-senderEntered
	close(releaseSender)
	select {
	case <-runFinished:
	case <-time.After(time.Second):
		t.Fatal("complete did not return after TaskDone sender release")
	}
	detach()

	if observedEndCalls != 1 {
		t.Fatalf("TaskDone became visible with EndTask calls=%d, want 1",
			observedEndCalls)
	}
	pool.mu.Lock()
	finalEndCalls := pool.endTasks[task.TaskID]
	pool.mu.Unlock()
	if finalEndCalls != 1 {
		t.Fatalf("final EndTask calls=%d, want exactly 1", finalEndCalls)
	}
	if got := doneCalls.Load(); got != 1 {
		t.Fatalf("TaskDone calls=%d, want exactly 1", got)
	}
}

func validPhase2Image(path string) proto.Phase2Item {
	sum := sha512.Sum512([]byte(path))
	return proto.Phase2Item{
		Path:       path,
		FieldsMask: proto.FieldPHashParts | proto.FieldSobelHist,
		MachineID:  "machine-a",
		SHA512:     hex.EncodeToString(sum[:]),
		Size:       10,
		MTimeMS:    20,
		Kind:       proto.KindImage,
	}
}

func validPhase2Video(path string, frameMask uint8) proto.Phase2Item {
	sum := sha512.Sum512([]byte(path))
	return proto.Phase2Item{
		Path:       path,
		FieldsMask: proto.FieldVideo6F,
		MachineID:  "machine-a",
		SHA512:     hex.EncodeToString(sum[:]),
		Size:       10,
		MTimeMS:    20,
		Kind:       proto.KindVideo,
		FrameMask:  frameMask,
		DurationMS: 12000,
	}
}

func clonePhase2TaskForTest(task proto.Phase2Task) proto.Phase2Task {
	task.Items = append([]proto.Phase2Item(nil), task.Items...)
	return task
}

type phase2CommittedFake struct {
	mu     sync.Mutex
	states map[string]store.Phase2Committed
	errors map[string]error
	reads  []string
}

type cancellablePhase2CommittedStore struct {
	entered     chan struct{}
	canceled    chan struct{}
	release     chan struct{}
	enterOnce   sync.Once
	cancelOnce  sync.Once
	releaseOnce sync.Once
}

type blockingSubmitPool struct {
	submitEntered chan struct{}
	submitExited  chan struct{}
	stop          chan struct{}
	results       chan *worker.JobResultMsg
	crashes       chan worker.CrashRecord
	enterOnce     sync.Once
	exitOnce      sync.Once
	stopOnce      sync.Once
	stopCalls     atomic.Int64
}

func newBlockingSubmitPool() *blockingSubmitPool {
	return &blockingSubmitPool{
		submitEntered: make(chan struct{}),
		submitExited:  make(chan struct{}),
		stop:          make(chan struct{}),
		results:       make(chan *worker.JobResultMsg),
		crashes:       make(chan worker.CrashRecord),
	}
}

func (p *blockingSubmitPool) Submit(*worker.JobMsg) error {
	p.enterOnce.Do(func() { close(p.submitEntered) })
	<-p.stop
	p.exitOnce.Do(func() { close(p.submitExited) })
	return worker.ErrPoolClosed
}

func (p *blockingSubmitPool) StopAccepting() {
	p.stopCalls.Add(1)
	p.stopOnce.Do(func() { close(p.stop) })
}

func (p *blockingSubmitPool) Results() <-chan *worker.JobResultMsg {
	return p.results
}

func (p *blockingSubmitPool) Crashes() <-chan worker.CrashRecord {
	return p.crashes
}

func (p *blockingSubmitPool) Metrics() worker.MetricsSnapshot {
	return worker.MetricsSnapshot{}
}

func (s *cancellablePhase2CommittedStore) Phase2CommittedStateForFields(
	ctx context.Context,
	_ string,
	_ string,
	_ store.MediaKind,
	_ uint32,
) (store.Phase2Committed, error) {
	s.enterOnce.Do(func() { close(s.entered) })
	select {
	case <-ctx.Done():
		s.cancelOnce.Do(func() { close(s.canceled) })
		return store.Phase2Committed{}, ctx.Err()
	case <-s.release:
		return store.Phase2Committed{}, errors.New("test store released")
	}
}

func waitForPhase2EndTask(
	t *testing.T,
	pool *phase2FakePool,
	taskID string,
) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		pool.mu.Lock()
		ended := pool.endTasks[taskID]
		pool.mu.Unlock()
		if ended != 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("EndTask(%q) was not called", taskID)
}

func waitForPhase2Closing(t *testing.T, manager *Phase2Manager) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		manager.mu.Lock()
		closing := manager.closing
		manager.mu.Unlock()
		if closing {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("Phase2 manager did not enter shutdown")
}

func waitForPoolRouterClosed(t *testing.T, router *PoolRouter) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		router.mu.Lock()
		closed := router.closed
		router.mu.Unlock()
		if closed {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("PoolRouter did not observe pool close")
}

func runSinglePhase2Item(
	t *testing.T,
	manager *Phase2Manager,
	taskID string,
	item proto.Phase2Item,
) proto.FeatureItem {
	t.Helper()
	features := make(chan proto.FeatureItem, 1)
	done := make(chan struct{}, 1)
	ack, start := manager.Prepare(proto.Phase2Task{
		TaskID: taskID,
		Items:  []proto.Phase2Item{item},
	}, func(msgType uint8, value any) error {
		switch msgType {
		case proto.MsgFeatureResult:
			features <- value.(*proto.FeatureResult).Items[0]
		case proto.MsgTaskDone:
			done <- struct{}{}
		}
		return nil
	})
	if !ack.Accepted || start == nil {
		t.Fatalf("Prepare ack=%#v start=%v", ack, start != nil)
	}
	start()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Phase2 task did not finish")
	}
	select {
	case feature := <-features:
		return feature
	default:
		t.Fatal("Phase2 task emitted no terminal FeatureItem")
		return proto.FeatureItem{}
	}
}

func replaySeqBytes(values []uint64) []byte {
	out := make([]byte, len(values))
	for index, value := range values {
		out[index] = byte(value)
	}
	return out
}

func (s *phase2CommittedFake) Phase2CommittedStateForFields(
	_ context.Context,
	_ string,
	path string,
	_ store.MediaKind,
	_ uint32,
) (store.Phase2Committed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads = append(s.reads, path)
	if err := s.errors[path]; err != nil {
		return store.Phase2Committed{}, err
	}
	state := s.states[path]
	if state.SHA512 == "" {
		sum := sha512.Sum512([]byte(path))
		state.SHA512 = hex.EncodeToString(sum[:])
	}
	return state, nil
}
