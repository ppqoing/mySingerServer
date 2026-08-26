package phase2

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"dedup/internal/config"
	"dedup/internal/features"
	"dedup/internal/proto"
)

func TestBuildTasksNormalizesPairsChoosesOnlineCopiesAndBuildsStableShards(
	t *testing.T,
) {
	shaA := strings.Repeat("0a", 64)
	shaB := strings.Repeat("0b", 64)
	shaC := strings.Repeat("0c", 64)
	validPHash := features.EncodePHashParts([9]uint64{})
	validSobel, err := features.EncodeSobelHist([128]float32{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := buildSnapshot{
		Groups: []candidateGroup{
			{Kind: candidateImage, Members: []candidateMember{
				{FileID: 2, SHA512: shaB},
				{FileID: 1, SHA512: shaA},
			}},
			// The reversed duplicate must collapse to the same content pair.
			{Kind: candidateImage, Members: []candidateMember{
				{FileID: 1, SHA512: shaA},
				{FileID: 2, SHA512: shaB},
			}},
			{Kind: candidateImage, Members: []candidateMember{
				{FileID: 2, SHA512: shaB},
				{FileID: 3, SHA512: shaC},
			}},
			// A candidate group with only one distinct content key is unusable.
			{Kind: candidateImage, Members: []candidateMember{
				{FileID: 10, SHA512: shaC},
				{FileID: 11, SHA512: shaC},
			}},
		},
		Copies: []fileCopy{
			{ID: 10, MachineID: "offline-a", Path: `D:\a.jpg`, SHA512: shaA, Size: 10, MTime: 100, Status: proto.StatusDone},
			{ID: 11, MachineID: "online-z", Path: `E:\a.jpg`, SHA512: shaA, Size: 11, MTime: 101, Status: proto.StatusPartial},
			{ID: 20, MachineID: "online-a", Path: `D:\b.jpg`, SHA512: shaB, Size: 20, MTime: 200, Status: proto.StatusDone},
			{ID: 21, MachineID: "online-a", Path: `E:\b.jpg`, SHA512: shaB, Size: 21, MTime: 201, Status: proto.StatusDone},
			{ID: 30, MachineID: "offline-c", Path: `D:\c.jpg`, SHA512: shaC, Size: 30, MTime: 300, Status: proto.StatusDone},
			{ID: 31, MachineID: "deleted-c", Path: `E:\c.jpg`, SHA512: shaC, Size: 31, MTime: 301, Status: proto.StatusDeleted},
		},
		Features: map[string]featureState{
			shaA: {PHashParts: validPHash, SobelHist: []byte{1}}, // decoder-invalid Sobel
			shaB: {PHashParts: []byte{1}, SobelHist: validSobel}, // decoder-invalid pHash
			shaC: {},                                             // both absent
		},
	}
	online := map[string]bool{"online-a": true, "online-z": true}
	dispatcher := newDispatcher(
		staticSnapshotLoader{snapshot: snapshot},
		fakeSender{online: online},
		config.Phase2Config{TaskShardSize: 2},
		nil,
	)

	first, err := dispatcher.BuildTasks(context.Background(), proto.KindImage)
	if err != nil {
		t.Fatal(err)
	}
	second, err := dispatcher.BuildTasks(context.Background(), proto.KindImage)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 {
		t.Fatalf("tasks = %#v, want three machine shards", first)
	}
	if first[0].MachineID != "offline-c" ||
		first[1].MachineID != "online-a" ||
		first[2].MachineID != "online-z" {
		t.Fatalf("machine order = %#v", first)
	}
	assertSingleImageItem(
		t, first[0], shaC, `D:\c.jpg`,
		proto.FieldPHashParts|proto.FieldSobelHist,
	)
	assertSingleImageItem(
		t, first[1], shaB, `D:\b.jpg`,
		proto.FieldPHashParts,
	)
	assertSingleImageItem(
		t, first[2], shaA, `E:\a.jpg`,
		proto.FieldSobelHist,
	)
	for index := range first {
		if first[index].Task.TaskID == "" ||
			first[index].Task.TaskID != second[index].Task.TaskID {
			t.Fatalf("unstable task ID: first=%#v second=%#v", first, second)
		}
	}

	changed := snapshot
	changed.Copies = append([]fileCopy(nil), snapshot.Copies...)
	changed.Copies[1].MTime++
	changedDispatcher := newDispatcher(
		staticSnapshotLoader{snapshot: changed},
		fakeSender{online: online},
		config.Phase2Config{TaskShardSize: 2},
		nil,
	)
	changedTasks, err := changedDispatcher.BuildTasks(
		context.Background(),
		proto.KindImage,
	)
	if err != nil {
		t.Fatal(err)
	}
	if changedTasks[2].Task.TaskID == first[2].Task.TaskID {
		t.Fatal("task ID did not change when selected file mtime changed")
	}

	statusOnly := snapshot
	statusOnly.Copies = append([]fileCopy(nil), snapshot.Copies...)
	statusOnly.Copies[1].Status = proto.StatusDone
	statusDispatcher := newDispatcher(
		staticSnapshotLoader{snapshot: statusOnly},
		fakeSender{online: online},
		config.Phase2Config{TaskShardSize: 2},
		nil,
	)
	statusTasks, err := statusDispatcher.BuildTasks(
		context.Background(),
		proto.KindImage,
	)
	if err != nil {
		t.Fatal(err)
	}
	if statusTasks[2].Task.TaskID != first[2].Task.TaskID {
		t.Fatal("non-wire file status changed an otherwise identical task ID")
	}
}

func assertSingleImageItem(
	t *testing.T,
	routed RoutedTask,
	sha string,
	path string,
	mask uint32,
) {
	t.Helper()
	if len(routed.Task.Items) != 1 {
		t.Fatalf("task = %#v, want one item", routed)
	}
	item := routed.Task.Items[0]
	if item.SHA512 != sha || item.Path != path ||
		item.FieldsMask != mask || item.Kind != proto.KindImage {
		t.Fatalf("item = %#v", item)
	}
	if err := item.Validate(); err != nil {
		t.Fatalf("invalid item: %v", err)
	}
}

type staticSnapshotLoader struct {
	snapshot buildSnapshot
}

func (loader staticSnapshotLoader) loadBuildSnapshot(
	context.Context,
	uint8,
) (buildSnapshot, error) {
	return loader.snapshot, nil
}

type fakeSender struct {
	online map[string]bool
}

func (sender fakeSender) IsOnline(machineID string) bool {
	return sender.online[machineID]
}

func (fakeSender) Send(string, uint8, any) error {
	return nil
}

func TestBuildTasksSnapshotsOnlineStateOncePerMachine(t *testing.T) {
	shaA := strings.Repeat("3a", 64)
	shaB := strings.Repeat("3b", 64)
	snapshot := incompletePairSnapshot(shaA, shaB)
	snapshot.Copies = append(snapshot.Copies,
		fileCopy{
			ID: 3, MachineID: "machine-c", Path: `E:\a.jpg`,
			SHA512: shaA, Size: 10, MTime: 1, Status: proto.StatusDone,
		},
	)
	sender := &flappingOnlineSender{first: map[string]bool{
		"machine-a": true,
		"machine-b": true,
		"machine-c": false,
	}}
	dispatcher := newDispatcher(
		staticSnapshotLoader{snapshot: snapshot},
		sender,
		config.Phase2Config{TaskShardSize: 5000},
		nil,
	)
	if _, err := dispatcher.BuildTasks(context.Background(), proto.KindImage); err != nil {
		t.Fatal(err)
	}
	for machine, calls := range sender.calls {
		if calls != 1 {
			t.Fatalf("IsOnline(%q) calls=%d, want one snapshot read", machine, calls)
		}
	}
}

func TestBuildTasksRejectsInvalidSelectedIdentity(t *testing.T) {
	shaA := strings.Repeat("4a", 64)
	shaB := strings.Repeat("4b", 64)
	t.Run("negative file identity", func(t *testing.T) {
		snapshot := incompletePairSnapshot(shaA, shaB)
		snapshot.Copies[0].MTime = -1
		dispatcher := newDispatcher(
			staticSnapshotLoader{snapshot: snapshot},
			fakeSender{online: map[string]bool{}},
			config.Phase2Config{TaskShardSize: 5000},
			nil,
		)
		if _, err := dispatcher.BuildTasks(
			context.Background(),
			proto.KindImage,
		); err == nil {
			t.Fatal("BuildTasks accepted a negative selected mtime")
		}
	})
	t.Run("non-positive video duration", func(t *testing.T) {
		shaC := strings.Repeat("5e", 64)
		shaD := strings.Repeat("6e", 64)
		snapshot := buildSnapshot{
			Groups: []candidateGroup{
				{
					Kind: candidateVideo,
					Members: []candidateMember{
						{FileID: 1, SHA512: shaA},
						{FileID: 2, SHA512: shaB},
					},
				},
				{
					Kind: candidateVideo,
					Members: []candidateMember{
						{FileID: 3, SHA512: shaC},
						{FileID: 4, SHA512: shaD},
					},
				},
			},
			Copies: []fileCopy{
				{ID: 1, MachineID: "machine-a", Path: `D:\a.mp4`, SHA512: shaA, Size: 1, MTime: 1, Status: proto.StatusDone},
				{ID: 2, MachineID: "machine-b", Path: `D:\b.mp4`, SHA512: shaB, Size: 1, MTime: 1, Status: proto.StatusDone},
				{ID: 3, MachineID: "machine-a", Path: `D:\c.mp4`, SHA512: shaC, Size: 1, MTime: 1, Status: proto.StatusDone},
				{ID: 4, MachineID: "machine-b", Path: `D:\d.mp4`, SHA512: shaD, Size: 1, MTime: 1, Status: proto.StatusDone},
			},
			Features: map[string]featureState{
				shaA: {DurationMS: 0},
				shaB: {DurationMS: 1000},
				shaC: {DurationMS: 1000},
				shaD: {DurationMS: 1000},
			},
		}
		dispatcher := newDispatcher(
			staticSnapshotLoader{snapshot: snapshot},
			fakeSender{online: map[string]bool{}},
			config.Phase2Config{TaskShardSize: 5000},
			nil,
		)
		tasks, err := dispatcher.BuildTasks(
			context.Background(),
			proto.KindVideo,
		)
		if err != nil {
			t.Fatalf("BuildTasks aborted the whole batch on a non-positive video duration: %v", err)
		}
		// The zero-duration pair must be skipped while the valid pair is
		// still dispatched.
		for _, task := range tasks {
			for _, item := range task.Task.Items {
				if item.SHA512 == shaA || item.SHA512 == shaB {
					t.Fatalf("zero-duration pair was dispatched: %#v", item)
				}
			}
		}
		dispatched := 0
		for _, task := range tasks {
			dispatched += len(task.Task.Items)
		}
		if dispatched != 2 {
			t.Fatalf("valid pair was not dispatched alongside the skip: %d items", dispatched)
		}
	})
}

func TestBuildTasksRejectsCandidateGroupWithAnyMalformedLiveSHA(t *testing.T) {
	shaA := strings.Repeat("4c", 64)
	shaB := strings.Repeat("4d", 64)
	snapshot := incompletePairSnapshot(shaA, shaB)
	snapshot.Groups[0].Members = append(
		snapshot.Groups[0].Members,
		candidateMember{
			FileID: 3,
			SHA512: "NOT-CANONICAL",
			Status: proto.StatusDone,
		},
	)
	dispatcher := newDispatcher(
		staticSnapshotLoader{snapshot: snapshot},
		fakeSender{online: map[string]bool{}},
		config.Phase2Config{TaskShardSize: 5000},
		nil,
	)
	if _, err := dispatcher.BuildTasks(
		context.Background(),
		proto.KindImage,
	); err == nil {
		t.Fatal("BuildTasks manufactured a pair after dropping a malformed live SHA")
	}
}

func TestBuildTasksRejectsInvalidLiveCopyBeforeSelection(t *testing.T) {
	shaA := strings.Repeat("4e", 64)
	shaB := strings.Repeat("4f", 64)
	for _, test := range []struct {
		name   string
		mutate func(*fileCopy)
	}{
		{name: "empty machine", mutate: func(copy *fileCopy) {
			copy.MachineID = ""
		}},
		{name: "empty path", mutate: func(copy *fileCopy) {
			copy.Path = ""
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := incompletePairSnapshot(shaA, shaB)
			test.mutate(&snapshot.Copies[0])
			dispatcher := newDispatcher(
				staticSnapshotLoader{snapshot: snapshot},
				fakeSender{online: map[string]bool{}},
				config.Phase2Config{TaskShardSize: 5000},
				nil,
			)
			if _, err := dispatcher.BuildTasks(
				context.Background(),
				proto.KindImage,
			); err == nil {
				t.Fatal("BuildTasks silently treated an invalid live copy as absent")
			}
		})
	}

	t.Run("one valid plus one invalid copy is fail closed", func(t *testing.T) {
		snapshot := incompletePairSnapshot(shaA, shaB)
		snapshot.Copies = append(snapshot.Copies, fileCopy{
			ID: 3, MachineID: "", Path: `E:\a.jpg`,
			SHA512: shaA, Size: 10, MTime: 1, Status: proto.StatusDone,
		})
		dispatcher := newDispatcher(
			staticSnapshotLoader{snapshot: snapshot},
			fakeSender{online: map[string]bool{"machine-a": true}},
			config.Phase2Config{TaskShardSize: 5000},
			nil,
		)
		if _, err := dispatcher.BuildTasks(
			context.Background(),
			proto.KindImage,
		); err == nil {
			t.Fatal("BuildTasks ignored an invalid alternate live copy")
		}
	})
}

func TestBuildTasksRejectsEnvelopeAboveProtocolFrameLimit(t *testing.T) {
	shaA := strings.Repeat("7a", 64)
	shaB := strings.Repeat("7b", 64)
	snapshot := incompletePairSnapshot(shaA, shaB)
	snapshot.Copies[0].Path = strings.Repeat("x", proto.MaxFrameSize)
	dispatcher := newDispatcher(
		staticSnapshotLoader{snapshot: snapshot},
		fakeSender{online: map[string]bool{}},
		config.Phase2Config{TaskShardSize: 5000},
		nil,
	)
	if _, err := dispatcher.BuildTasks(
		context.Background(),
		proto.KindImage,
	); err == nil {
		t.Fatal("BuildTasks accepted an envelope above proto.MaxFrameSize")
	}
}

func TestBuildTasksShardBoundaries(t *testing.T) {
	for _, test := range []struct {
		items int
		want  []int
	}{
		{items: 4999, want: []int{4999}},
		{items: 5000, want: []int{5000}},
		{items: 5001, want: []int{5000, 1}},
	} {
		t.Run(fmt.Sprint(test.items), func(t *testing.T) {
			snapshot := largeImageSnapshot(test.items)
			dispatcher := newDispatcher(
				staticSnapshotLoader{snapshot: snapshot},
				fakeSender{online: map[string]bool{}},
				config.Phase2Config{TaskShardSize: 5000},
				nil,
			)
			tasks, err := dispatcher.BuildTasks(
				context.Background(),
				proto.KindImage,
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(tasks) != len(test.want) {
				t.Fatalf("shards=%d, want %#v", len(tasks), test.want)
			}
			for index, want := range test.want {
				if len(tasks[index].Task.Items) != want {
					t.Fatalf(
						"shard %d items=%d, want %d",
						index,
						len(tasks[index].Task.Items),
						want,
					)
				}
			}
		})
	}
}

func TestRestoreRejectsOversizedTaskEnvelope(t *testing.T) {
	sha := strings.Repeat("ea", 64)
	validItem := proto.Phase2Item{
		MachineID:  "machine-a",
		Path:       `D:\a.jpg`,
		SHA512:     sha,
		Size:       1,
		MTimeMS:    1,
		Kind:       proto.KindImage,
		FieldsMask: proto.FieldPHashParts,
	}
	t.Run("more than 5000 items", func(t *testing.T) {
		items := make([]proto.Phase2Item, 5001)
		for index := range items {
			items[index] = validItem
			items[index].Path = fmt.Sprintf(`D:\%05d.jpg`, index)
		}
		envelope := RoutedTask{
			MachineID: "machine-a",
			Task:      proto.Phase2Task{Items: items},
		}
		envelope.Task.TaskID = stableTaskID(envelope)
		target := phase2Target{
			Type: phase2TargetType, MachineID: envelope.MachineID,
			Task: envelope.Task,
		}
		if err := validateRestoredTarget(target, envelope); err == nil {
			t.Fatal("restore accepted more than 5000 items")
		}
	})
	t.Run("wire envelope above 16MiB", func(t *testing.T) {
		item := validItem
		item.Path = strings.Repeat("x", proto.MaxFrameSize)
		envelope := RoutedTask{
			MachineID: "machine-a",
			Task:      proto.Phase2Task{Items: []proto.Phase2Item{item}},
		}
		envelope.Task.TaskID = stableTaskID(envelope)
		target := phase2Target{
			Type: phase2TargetType, MachineID: envelope.MachineID,
			Task: envelope.Task,
		}
		if err := validateRestoredTarget(target, envelope); err == nil {
			t.Fatal("restore accepted an envelope above proto.MaxFrameSize")
		}
	})
}

func TestDispatchPendingPersistsEveryEnvelopeBeforeFirstSendAndRetriesByMachine(
	t *testing.T,
) {
	shaA := strings.Repeat("1a", 64)
	shaB := strings.Repeat("1b", 64)
	store := &memoryTaskStore{
		snapshots: map[uint8]buildSnapshot{
			proto.KindImage: incompletePairSnapshot(shaA, shaB),
			proto.KindVideo: {},
		},
	}
	sender := &recordingSender{
		online:  map[string]bool{"machine-a": true, "machine-b": true},
		sendErr: map[string]error{"machine-b": errors.New("connection reset")},
	}
	sender.beforeSend = func() {
		if len(store.persisted) != 2 {
			t.Fatalf(
				"send began after %d persisted envelopes, want all 2",
				len(store.persisted),
			)
		}
	}
	dispatcher := newDispatcher(
		store,
		sender,
		config.Phase2Config{TaskShardSize: 5000},
		nil,
	)

	err := dispatcher.DispatchPending(context.Background())
	if err == nil {
		t.Fatal("DispatchPending hid the machine-b send failure")
	}
	var admissionError interface{ DurablyAdmitted() bool }
	if !errors.As(err, &admissionError) || !admissionError.DurablyAdmitted() {
		t.Fatalf("send error durable-admission state = %T %v", err, err)
	}
	if len(store.persisted) != 2 || len(sender.sent) != 2 {
		t.Fatalf(
			"persisted=%d sent=%#v, want both durable before sends",
			len(store.persisted),
			sender.sent,
		)
	}
	if got := store.statusByMachine["machine-b"]; got != taskStatusSent {
		t.Fatalf("failed-send status = %q, want pending sent", got)
	}

	sender.sendErr = nil
	sender.sent = nil
	if err := dispatcher.DispatchMachinePending(
		context.Background(),
		"machine-b",
	); err != nil {
		t.Fatal(err)
	}
	if len(sender.sent) != 1 || sender.sent[0].machineID != "machine-b" {
		t.Fatalf("machine-scoped retry sent %#v", sender.sent)
	}
}

func TestRestorePendingAndTaskMessagesPreserveLifecycleUntilTaskDone(
	t *testing.T,
) {
	shaA := strings.Repeat("2a", 64)
	shaB := strings.Repeat("2b", 64)
	store := &memoryTaskStore{
		snapshots: map[uint8]buildSnapshot{
			proto.KindImage: incompletePairSnapshot(shaA, shaB),
			proto.KindVideo: {},
		},
	}
	sender := &recordingSender{
		online: map[string]bool{"machine-a": false, "machine-b": false},
	}
	first := newDispatcher(
		store,
		sender,
		config.Phase2Config{TaskShardSize: 5000},
		nil,
	)
	if err := first.DispatchPending(context.Background()); err != nil {
		t.Fatal(err)
	}

	restoredSender := &recordingSender{
		online: map[string]bool{"machine-a": true, "machine-b": false},
	}
	restored := newDispatcher(
		store,
		restoredSender,
		config.Phase2Config{TaskShardSize: 5000},
		nil,
	)
	if err := restored.RestorePending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := restored.DispatchMachinePending(
		context.Background(),
		"machine-a",
	); err != nil {
		t.Fatal(err)
	}
	if len(restoredSender.sent) != 1 {
		t.Fatalf("restored sends = %#v, want machine-a only", restoredSender.sent)
	}
	taskID := restoredSender.sent[0].task.TaskID

	if !restored.HandleMessage("machine-a", &proto.TaskAck{
		TaskID: taskID, Accepted: true, Reason: "resumed",
	}) {
		t.Fatal("accepted ack was not handled")
	}
	if got := store.statusByID[taskID]; got != taskStatusRunning {
		t.Fatalf("resumed status = %q", got)
	}
	if !restored.HandleMessage("machine-a", &proto.FeatureResult{
		TaskID: taskID, Seq: 1,
	}) {
		t.Fatal("feature result was not handled")
	}
	if got := store.statusByID[taskID]; got != taskStatusRunning {
		t.Fatalf("result status = %q", got)
	}
	if !restored.HandleMessage("machine-a", &proto.TaskDone{
		TaskID: taskID,
		Stats:  proto.TaskStats{Total: 1, Done: 1},
	}) {
		t.Fatal("TaskDone was not handled")
	}
	if got := store.statusByID[taskID]; got != taskStatusDone {
		t.Fatalf("done status = %q", got)
	}
	restoredSender.sent = nil
	if err := restored.DispatchMachinePending(
		context.Background(),
		"machine-a",
	); err != nil {
		t.Fatal(err)
	}
	if len(restoredSender.sent) != 0 {
		t.Fatalf("terminal task was resent: %#v", restoredSender.sent)
	}
}

func TestAlreadyDoneAckRemainsNonTerminalUntilReplayedTaskDone(t *testing.T) {
	taskID, status, stats, _, ok := phase2MessageState(&proto.TaskAck{
		TaskID:   "phase2-replay",
		Accepted: true,
		Reason:   "already_done",
		Total:    3,
		Stats:    &proto.TaskStats{Total: 3, Done: 3},
	})
	if !ok || taskID != "phase2-replay" ||
		isTerminalTaskStatus(status) ||
		stats.Total != 3 {
		t.Fatalf(
			"already_done state=(%q,%q,%#v,%v), want replayable nonterminal",
			taskID,
			status,
			stats,
			ok,
		)
	}
}

func TestAlreadyDoneAckPreservesReplayedStats(t *testing.T) {
	replayed := proto.TaskStats{
		Total: 3, Done: 2, Skipped: 1, Failed: 1, ElapsedMS: 99,
	}
	got := mergeTaskStats(
		proto.TaskStats{Done: 1},
		replayed,
		&proto.TaskAck{
			TaskID:   "phase2-replay",
			Accepted: true,
			Reason:   "already_done",
			Stats:    &replayed,
		},
	)
	if got != replayed {
		t.Fatalf("already_done stats=%#v, want replayed %#v", got, replayed)
	}
}

func TestDurableReadmissionCannotReviveTerminalMemoryState(t *testing.T) {
	memory := &taskMemory{tasks: make(map[string]*taskEntry)}
	task := persistedTask{
		Envelope: RoutedTask{
			MachineID: "machine-a",
			Task:      proto.Phase2Task{TaskID: "phase2-terminal"},
		},
		Status: taskStatusDone,
		Stats:  proto.TaskStats{Total: 1, Done: 1},
	}
	upsertMemoryTask(memory, task)
	readmitted := task
	readmitted.Status = taskStatusSent
	readmitted.Stats = proto.TaskStats{}
	upsertMemoryTask(memory, readmitted)

	memory.mu.Lock()
	entry := memory.tasks[task.Envelope.Task.TaskID]
	memory.mu.Unlock()
	entry.mu.Lock()
	got := entry.task
	entry.mu.Unlock()
	if got.Status != taskStatusDone ||
		got.Stats.Total != 1 || got.Stats.Done != 1 {
		t.Fatalf("terminal admission regressed: %#v", got)
	}
}

func TestDispatchPendingRetainsEarlierDurableAdmissionOnLaterPersistFailure(
	t *testing.T,
) {
	shaA := strings.Repeat("5a", 64)
	shaB := strings.Repeat("5b", 64)
	store := &failingPersistStore{
		memoryTaskStore: memoryTaskStore{snapshots: map[uint8]buildSnapshot{
			proto.KindImage: incompletePairSnapshot(shaA, shaB),
			proto.KindVideo: {},
		}},
		failAt: 2,
	}
	sender := &recordingSender{
		online: map[string]bool{"machine-a": true, "machine-b": true},
	}
	dispatcher := newDispatcher(
		store,
		sender,
		config.Phase2Config{TaskShardSize: 5000},
		nil,
	)
	err := dispatcher.DispatchPending(context.Background())
	if err == nil {
		t.Fatal("DispatchPending hid the second persistence failure")
	}
	var admissionError interface{ DurablyAdmitted() bool }
	if !errors.As(err, &admissionError) || admissionError.DurablyAdmitted() {
		t.Fatalf("persist error durable-admission state = %T %v", err, err)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("sent before complete admission: %#v", sender.sent)
	}
	if err := dispatcher.DispatchMachinePending(
		context.Background(),
		"machine-a",
	); err != nil {
		t.Fatal(err)
	}
	if len(sender.sent) != 1 || sender.sent[0].machineID != "machine-a" {
		t.Fatalf("earlier durable admission was lost locally: %#v", sender.sent)
	}
}

func TestDispatchPendingReusesOldCoverageAcrossRoutingAndMaskChanges(
	t *testing.T,
) {
	shaA := strings.Repeat("8a", 64)
	shaB := strings.Repeat("8b", 64)
	store := &memoryTaskStore{snapshots: map[uint8]buildSnapshot{
		proto.KindImage: incompletePairSnapshot(shaA, shaB),
		proto.KindVideo: {},
	}}
	sender := &recordingSender{online: map[string]bool{}}
	dispatcher := newDispatcher(
		store,
		sender,
		config.Phase2Config{TaskShardSize: 5000},
		nil,
	)
	if err := dispatcher.DispatchPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.persisted) != 2 {
		t.Fatalf("initial persisted=%d, want 2", len(store.persisted))
	}

	changed := incompletePairSnapshot(shaA, shaB)
	changed.Copies[0].MachineID = "machine-c"
	validSobel, err := features.EncodeSobelHist([128]float32{})
	if err != nil {
		t.Fatal(err)
	}
	changed.Features = map[string]featureState{
		shaA: {SobelHist: validSobel},
	}
	store.snapshots[proto.KindImage] = changed
	sender.online["machine-c"] = true
	if err := dispatcher.DispatchPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.persisted) != 2 {
		t.Fatalf(
			"routing/mask drift created overlapping tasks: %#v",
			store.persisted,
		)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("new overlapping envelope was sent: %#v", sender.sent)
	}
}

func TestDispatchPendingReusesOldCoverageAcrossShardSizeChanges(t *testing.T) {
	store := &memoryTaskStore{snapshots: map[uint8]buildSnapshot{
		proto.KindImage: largeImageSnapshot(5001),
		proto.KindVideo: {},
	}}
	dispatcher := newDispatcher(
		store,
		&recordingSender{online: map[string]bool{}},
		config.Phase2Config{TaskShardSize: 4999},
		nil,
	)
	if err := dispatcher.DispatchPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.persisted) != 2 {
		t.Fatalf("initial 4999-sized shards=%d, want 2", len(store.persisted))
	}
	dispatcher.cfg.TaskShardSize = 5000
	if err := dispatcher.DispatchPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.persisted) != 2 {
		t.Fatalf(
			"5000-sized rebuild created overlapping tasks: %d",
			len(store.persisted),
		)
	}
}

func TestPendingVideoFrameMaskZeroCoversAllFrames(t *testing.T) {
	sha := strings.Repeat("9a", 64)
	dispatcher := newDispatcher(
		staticSnapshotLoader{},
		fakeSender{online: map[string]bool{}},
		config.Phase2Config{TaskShardSize: 5000},
		nil,
	)
	upsertMemoryTask(dispatcher.ensureMemory(), persistedTask{
		Envelope: RoutedTask{
			MachineID: "machine-a",
			Task: proto.Phase2Task{
				TaskID: "phase2-old-video",
				Items: []proto.Phase2Item{{
					MachineID:  "machine-a",
					Path:       `D:\clip.mp4`,
					SHA512:     sha,
					Size:       1,
					MTimeMS:    1,
					Kind:       proto.KindVideo,
					FieldsMask: proto.FieldVideo6F,
					FrameMask:  0,
					DurationMS: 1000,
				}},
			},
		},
		Status: taskStatusRunning,
	})
	built := []RoutedTask{{
		MachineID: "machine-b",
		Task: proto.Phase2Task{
			TaskID: "phase2-new-video",
			Items: []proto.Phase2Item{{
				MachineID:  "machine-b",
				Path:       `E:\clip.mp4`,
				SHA512:     sha,
				Size:       1,
				MTimeMS:    1,
				Kind:       proto.KindVideo,
				FieldsMask: proto.FieldVideo6F,
				FrameMask:  proto.FrameMaskFull,
				DurationMS: 1000,
			}},
		},
	}}
	filtered, err := dispatcher.excludePendingCoverage(built)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 0 {
		t.Fatalf("legacy full-frame pending did not cover new work: %#v", filtered)
	}
}

func TestSuccessfulRetrySendsEarlierPartialAdmissionAfterBarrier(t *testing.T) {
	shaA := strings.Repeat("aa", 64)
	shaB := strings.Repeat("ab", 64)
	store := &failingPersistStore{
		memoryTaskStore: memoryTaskStore{snapshots: map[uint8]buildSnapshot{
			proto.KindImage: incompletePairSnapshot(shaA, shaB),
			proto.KindVideo: {},
		}},
		failAt: 2,
	}
	sender := &recordingSender{
		online: map[string]bool{"machine-a": true, "machine-b": true},
	}
	dispatcher := newDispatcher(
		store,
		sender,
		config.Phase2Config{TaskShardSize: 5000},
		nil,
	)
	if err := dispatcher.DispatchPending(context.Background()); err == nil {
		t.Fatal("first partial admission unexpectedly succeeded")
	}
	if len(sender.sent) != 0 {
		t.Fatalf("partial barrier sent %#v", sender.sent)
	}
	store.failAt = 0
	if err := dispatcher.DispatchPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender.sent) != 2 {
		t.Fatalf("successful barrier sent %#v, want old+new pending", sender.sent)
	}
}

func TestConcurrentDispatchPendingSerializesBuildAndAdmission(t *testing.T) {
	shaA := strings.Repeat("ba", 64)
	shaB := strings.Repeat("bb", 64)
	store := &concurrencyStore{
		memoryTaskStore: memoryTaskStore{snapshots: map[uint8]buildSnapshot{
			proto.KindImage: incompletePairSnapshot(shaA, shaB),
			proto.KindVideo: {},
		}},
	}
	dispatcher := newDispatcher(
		store,
		&recordingSender{online: map[string]bool{}},
		config.Phase2Config{TaskShardSize: 5000},
		nil,
	)
	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			errs <- dispatcher.DispatchPending(context.Background())
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if store.maxActive != 1 {
		t.Fatalf("concurrent builds=%d, want admission gate max 1", store.maxActive)
	}
	if len(store.persisted) != 2 {
		t.Fatalf("concurrent dispatch persisted overlaps: %d tasks", len(store.persisted))
	}
}

func TestHandleMessageSerializesTransitionsPreservesStatsAndUsesBoundedContext(
	t *testing.T,
) {
	shaA := strings.Repeat("6a", 64)
	shaB := strings.Repeat("6b", 64)
	store := newBlockingUpdateStore(incompletePairSnapshot(shaA, shaB))
	sender := &recordingSender{
		online: map[string]bool{"machine-a": false, "machine-b": false},
	}
	dispatcher := newDispatcher(
		store,
		sender,
		config.Phase2Config{TaskShardSize: 5000},
		nil,
	)
	if err := dispatcher.DispatchPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	var taskID string
	for _, task := range store.persisted {
		if task.Envelope.MachineID == "machine-a" {
			taskID = task.Envelope.Task.TaskID
		}
	}
	if taskID == "" {
		t.Fatal("machine-a task missing")
	}

	store.blockRunning = make(chan struct{})
	progressDone := make(chan struct{})
	go func() {
		dispatcher.HandleMessage("machine-a", &proto.TaskProgress{
			TaskID: taskID, Done: 2, Total: 10,
		})
		close(progressDone)
	}()
	select {
	case <-store.runningEntered:
	case <-time.After(time.Second):
		t.Fatal("progress did not enter durable transition")
	}
	doneDone := make(chan struct{})
	go func() {
		dispatcher.HandleMessage("machine-a", &proto.TaskDone{
			TaskID: taskID,
			Stats:  proto.TaskStats{Total: 10, Done: 10},
		})
		close(doneDone)
	}()
	select {
	case status := <-store.updateEntered:
		t.Fatalf("concurrent transition entered store with status %q", status)
	case <-time.After(30 * time.Millisecond):
	}
	close(store.blockRunning)
	select {
	case <-progressDone:
	case <-time.After(time.Second):
		t.Fatal("progress transition did not finish")
	}
	select {
	case <-doneDone:
	case <-time.After(time.Second):
		t.Fatal("done transition did not finish")
	}

	dispatcher.HandleMessage("machine-a", &proto.FeatureResult{
		TaskID: taskID, Seq: 2,
	})
	memory := dispatcher.ensureMemory()
	memory.mu.Lock()
	entry := memory.tasks[taskID]
	memory.mu.Unlock()
	entry.mu.Lock()
	final := entry.task
	entry.mu.Unlock()
	if final.Status != taskStatusDone ||
		final.Stats.Total != 10 || final.Stats.Done != 10 {
		t.Fatalf("terminal task regressed or lost stats: %#v", final)
	}
	if !store.allUpdatesBounded {
		t.Fatal("a durable message update used an unbounded context")
	}
}

func TestDispatcherShutdownCancelsAdmittedLifecyclePersistence(t *testing.T) {
	shaA := strings.Repeat("ca", 64)
	shaB := strings.Repeat("cb", 64)
	store := &cancelUpdateStore{
		memoryTaskStore: memoryTaskStore{snapshots: map[uint8]buildSnapshot{
			proto.KindImage: incompletePairSnapshot(shaA, shaB),
			proto.KindVideo: {},
		}},
		entered: make(chan struct{}),
	}
	dispatcher := newDispatcher(
		store,
		&recordingSender{online: map[string]bool{}},
		config.Phase2Config{TaskShardSize: 5000},
		nil,
	)
	if err := dispatcher.DispatchPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	taskID := store.persisted[0].Envelope.Task.TaskID
	handled := make(chan struct{})
	go func() {
		dispatcher.HandleMessage(
			store.persisted[0].Envelope.MachineID,
			&proto.TaskProgress{TaskID: taskID, Done: 1, Total: 2},
		)
		close(handled)
	}()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("lifecycle update did not enter store")
	}
	shutdown := make(chan struct{})
	go func() {
		dispatcher.Shutdown()
		close(shutdown)
	}()
	select {
	case <-shutdown:
	case <-time.After(time.Second):
		t.Fatal("dispatcher shutdown did not cancel/wait lifecycle update")
	}
	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatal("cancelled lifecycle handler did not return")
	}
}

func TestLifecyclePersistencePreservesLastErrorAcrossProgress(t *testing.T) {
	shaA := strings.Repeat("da", 64)
	shaB := strings.Repeat("db", 64)
	store := &memoryTaskStore{snapshots: map[uint8]buildSnapshot{
		proto.KindImage: incompletePairSnapshot(shaA, shaB),
		proto.KindVideo: {},
	}}
	dispatcher := newDispatcher(
		store,
		&recordingSender{online: map[string]bool{}},
		config.Phase2Config{TaskShardSize: 5000},
		nil,
	)
	if err := dispatcher.DispatchPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	task := store.persisted[0]
	taskID := task.Envelope.Task.TaskID
	machineID := task.Envelope.MachineID
	dispatcher.HandleMessage(machineID, &proto.Error{
		TaskID: taskID, Stage: "decode", Msg: "bad frame",
	})
	dispatcher.HandleMessage(machineID, &proto.TaskProgress{
		TaskID: taskID, Done: 1, Total: 2,
	})
	if got := store.lastErrByID[taskID]; got != "bad frame" {
		t.Fatalf("durable last error=%q, want preserved", got)
	}
	entry := dispatcher.ensureMemory().tasks[taskID]
	entry.mu.Lock()
	got := entry.task.LastErr
	entry.mu.Unlock()
	if got != "bad frame" {
		t.Fatalf("memory last error=%q, want preserved", got)
	}
}

func TestOutcomeUnknownTerminalCommitCannotBeRegressedByReplay(t *testing.T) {
	shaA := strings.Repeat("dc", 64)
	shaB := strings.Repeat("dd", 64)
	store := &outcomeUnknownTerminalStore{
		memoryTaskStore: memoryTaskStore{snapshots: map[uint8]buildSnapshot{
			proto.KindImage: incompletePairSnapshot(shaA, shaB),
			proto.KindVideo: {},
		}},
	}
	dispatcher := newDispatcher(
		store,
		&recordingSender{online: map[string]bool{}},
		config.Phase2Config{TaskShardSize: 5000},
		nil,
	)
	if err := dispatcher.DispatchPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	task := store.persisted[0]
	taskID := task.Envelope.Task.TaskID
	machineID := task.Envelope.MachineID
	dispatcher.HandleMessage(machineID, &proto.Error{
		TaskID: taskID, Stage: "decode", Msg: "terminal detail",
	})
	terminalStats := proto.TaskStats{
		Total: 3, Done: 2, Failed: 1, ElapsedMS: 77,
	}
	dispatcher.HandleMessage(machineID, &proto.TaskDone{
		TaskID: taskID, Stats: terminalStats,
	})
	if store.durable.Status != taskStatusDone {
		t.Fatalf("outcome-unknown update did not commit terminal: %#v", store.durable)
	}

	replayStats := proto.TaskStats{Total: 3, Done: 1}
	dispatcher.HandleMessage(machineID, &proto.TaskAck{
		TaskID: taskID, Accepted: true, Reason: "already_done",
		Stats: &replayStats,
	})
	dispatcher.HandleMessage(machineID, &proto.TaskProgress{
		TaskID: taskID, Done: 1, Total: 3,
	})
	entry := dispatcher.ensureMemory().tasks[taskID]
	entry.mu.Lock()
	memoryState := entry.task
	entry.mu.Unlock()
	if memoryState.Status != taskStatusDone ||
		memoryState.Stats != terminalStats ||
		memoryState.LastErr != "terminal detail" {
		t.Fatalf("memory terminal state regressed after replay: %#v", memoryState)
	}
	if store.durable.Status != taskStatusDone ||
		store.durable.Stats != terminalStats ||
		store.durable.LastErr != "terminal detail" {
		t.Fatalf("durable terminal state regressed after replay: %#v", store.durable)
	}
}

func TestDispatcherBindsAndValidatesEntireFeatureResultAgainstPendingEnvelope(
	t *testing.T,
) {
	shaImage := strings.Repeat("8a", 64)
	shaVideo := strings.Repeat("8b", 64)
	task := proto.Phase2Task{
		TaskID: "phase2-bind-task",
		Items: []proto.Phase2Item{
			{
				Path: `D:\image.jpg`, MachineID: "machine-a",
				SHA512: shaImage, Kind: proto.KindImage,
				FieldsMask: proto.FieldPHashParts | proto.FieldSobelHist,
			},
			{
				Path: `D:\video.mp4`, MachineID: "machine-a",
				SHA512: shaVideo, Kind: proto.KindVideo,
				FieldsMask: proto.FieldVideo6F, FrameMask: 1<<1 | 1<<4,
				DurationMS: 12000,
			},
		},
	}
	dispatcher := newDispatcher(
		&memoryTaskStore{},
		&recordingSender{online: map[string]bool{}},
		config.Phase2Config{TaskShardSize: 5000},
		nil,
	)
	upsertMemoryTask(dispatcher.ensureMemory(), persistedTask{
		Envelope: RoutedTask{MachineID: "machine-a", Task: task},
		Status:   taskStatusRunning,
	})

	result := &proto.FeatureResult{
		TaskID: task.TaskID,
		Seq:    7,
		Items: []proto.FeatureItem{
			{
				Path: `D:\video.mp4`, SHA512: shaVideo,
				Status: proto.StatusPartial,
				Frames: []proto.FrameFeature{{FrameIdx: 4}},
			},
			{
				Path: `D:\image.jpg`, SHA512: shaImage,
				Status:     proto.StatusPartial,
				FieldsDone: proto.FieldPHashParts,
				PHashParts: []byte{1, 2, 3},
			},
		},
	}
	bound, err := dispatcher.BindFeatureResult("machine-a", result)
	if err != nil {
		t.Fatal(err)
	}
	if bound.TaskID != task.TaskID || bound.Seq != 7 ||
		len(bound.Items) != 2 ||
		bound.Items[0].Kind != proto.KindVideo ||
		bound.Items[1].Kind != proto.KindImage {
		t.Fatalf("bound result = %#v", bound)
	}
	result.Items[0].Frames[0].FrameIdx = 1
	result.Items[1].PHashParts[0] = 9
	if bound.Items[0].Item.Frames[0].FrameIdx != 4 ||
		bound.Items[1].Item.PHashParts[0] != 1 {
		t.Fatal("bound result shares mutable input payload")
	}
	fileError, err := dispatcher.BindFeatureResult("machine-a", &proto.FeatureResult{
		TaskID: task.TaskID,
		Items: []proto.FeatureItem{{
			Path: `D:\image.jpg`, SHA512: shaImage,
			Status: proto.StatusFailed,
			FieldErrors: []proto.FieldError{{
				Field: 0, Stage: "stale", Msg: "file identity changed",
			}},
		}},
	})
	if err != nil || len(fileError.Items) != 1 {
		t.Fatalf("file-level Field=0 error binding: bound=%#v err=%v", fileError, err)
	}

	badResults := []struct {
		name      string
		machineID string
		result    *proto.FeatureResult
	}{
		{
			name: "wrong machine", machineID: "machine-b",
			result: &proto.FeatureResult{TaskID: task.TaskID, Items: []proto.FeatureItem{{
				Path: `D:\image.jpg`, SHA512: shaImage,
			}}},
		},
		{
			name: "unknown task", machineID: "machine-a",
			result: &proto.FeatureResult{TaskID: "unknown", Items: []proto.FeatureItem{{
				Path: `D:\image.jpg`, SHA512: shaImage,
			}}},
		},
		{
			name: "wrong path", machineID: "machine-a",
			result: &proto.FeatureResult{TaskID: task.TaskID, Items: []proto.FeatureItem{{
				Path: `D:\other.jpg`, SHA512: shaImage,
			}}},
		},
		{
			name: "wrong SHA", machineID: "machine-a",
			result: &proto.FeatureResult{TaskID: task.TaskID, Items: []proto.FeatureItem{{
				Path: `D:\image.jpg`, SHA512: strings.Repeat("8c", 64),
			}}},
		},
		{
			name: "duplicate item", machineID: "machine-a",
			result: &proto.FeatureResult{TaskID: task.TaskID, Items: []proto.FeatureItem{
				{Path: `D:\image.jpg`, SHA512: shaImage},
				{Path: `D:\image.jpg`, SHA512: shaImage},
			}},
		},
		{
			name: "field beyond request", machineID: "machine-a",
			result: &proto.FeatureResult{TaskID: task.TaskID, Items: []proto.FeatureItem{{
				Path: `D:\image.jpg`, SHA512: shaImage,
				FieldsDone: proto.FieldVideo6F,
			}}},
		},
		{
			name: "frame beyond request", machineID: "machine-a",
			result: &proto.FeatureResult{TaskID: task.TaskID, Items: []proto.FeatureItem{{
				Path: `D:\video.mp4`, SHA512: shaVideo,
				Frames: []proto.FrameFeature{{FrameIdx: 2}},
			}}},
		},
		{
			name: "video success bit misses requested frame", machineID: "machine-a",
			result: &proto.FeatureResult{TaskID: task.TaskID, Items: []proto.FeatureItem{{
				Path: `D:\video.mp4`, SHA512: shaVideo,
				Status: proto.StatusDone, FieldsDone: proto.FieldVideo6F,
				Frames: []proto.FrameFeature{{FrameIdx: 4}},
			}}},
		},
		{
			name: "field error overlaps success", machineID: "machine-a",
			result: &proto.FeatureResult{TaskID: task.TaskID, Items: []proto.FeatureItem{{
				Path: `D:\image.jpg`, SHA512: shaImage,
				Status: proto.StatusPartial, FieldsDone: proto.FieldPHashParts,
				PHashParts: []byte{1},
				FieldErrors: []proto.FieldError{{
					Field: proto.FieldPHashParts, Stage: "decode", Msg: "failed",
				}},
			}}},
		},
		{
			name: "field error uses multiple bits", machineID: "machine-a",
			result: &proto.FeatureResult{TaskID: task.TaskID, Items: []proto.FeatureItem{{
				Path: `D:\image.jpg`, SHA512: shaImage,
				Status: proto.StatusFailed,
				FieldErrors: []proto.FieldError{{
					Field: proto.FieldPHashParts | proto.FieldSobelHist,
					Stage: "decode", Msg: "failed",
				}},
			}}},
		},
		{
			name: "file error plus success payload", machineID: "machine-a",
			result: &proto.FeatureResult{TaskID: task.TaskID, Items: []proto.FeatureItem{{
				Path: `D:\image.jpg`, SHA512: shaImage,
				Status: proto.StatusPartial, FieldsDone: proto.FieldPHashParts,
				PHashParts: []byte{1},
				FieldErrors: []proto.FieldError{{
					Field: 0, Stage: "stale", Msg: "file changed",
				}},
			}}},
		},
	}
	for _, tt := range badResults {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := dispatcher.BindFeatureResult(tt.machineID, tt.result); err == nil {
				t.Fatal("BindFeatureResult accepted invalid envelope/result binding")
			}
		})
	}

	entry := dispatcher.ensureMemory().tasks[task.TaskID]
	entry.mu.Lock()
	entry.task.Status = taskStatusDone
	entry.mu.Unlock()
	if _, err := dispatcher.BindFeatureResult("machine-a", &proto.FeatureResult{
		TaskID: task.TaskID,
		Items:  []proto.FeatureItem{{Path: `D:\image.jpg`, SHA512: shaImage}},
	}); err == nil {
		t.Fatal("BindFeatureResult accepted a terminal task")
	}
}

func incompletePairSnapshot(firstSHA, secondSHA string) buildSnapshot {
	return buildSnapshot{
		Groups: []candidateGroup{{
			Kind: candidateImage,
			Members: []candidateMember{
				{FileID: 1, SHA512: firstSHA},
				{FileID: 2, SHA512: secondSHA},
			},
		}},
		Copies: []fileCopy{
			{ID: 1, MachineID: "machine-a", Path: `D:\a.jpg`, SHA512: firstSHA, Size: 10, MTime: 1, Status: proto.StatusDone},
			{ID: 2, MachineID: "machine-b", Path: `D:\b.jpg`, SHA512: secondSHA, Size: 20, MTime: 2, Status: proto.StatusDone},
		},
		Features: map[string]featureState{},
	}
}

func largeImageSnapshot(itemCount int) buildSnapshot {
	snapshot := buildSnapshot{Features: make(map[string]featureState)}
	shas := make([]string, itemCount)
	for index := range shas {
		shas[index] = fmt.Sprintf("%0128x", index+1)
		snapshot.Copies = append(snapshot.Copies, fileCopy{
			ID:        int64(index + 1),
			MachineID: "machine-a",
			Path:      fmt.Sprintf(`D:\%05d.jpg`, index),
			SHA512:    shas[index],
			Size:      1,
			MTime:     1,
			Status:    proto.StatusDone,
		})
	}
	for index := 1; index < len(shas); index++ {
		snapshot.Groups = append(snapshot.Groups, candidateGroup{
			Kind: candidateImage,
			Members: []candidateMember{
				{FileID: int64(index), SHA512: shas[index-1]},
				{FileID: int64(index + 1), SHA512: shas[index]},
			},
		})
	}
	return snapshot
}

type memoryTaskStore struct {
	snapshots       map[uint8]buildSnapshot
	persisted       []persistedTask
	statusByID      map[string]string
	statusByMachine map[string]string
	lastErrByID     map[string]string
}

type failingPersistStore struct {
	memoryTaskStore
	failAt int
	calls  int
}

type concurrencyStore struct {
	memoryTaskStore
	mu        sync.Mutex
	active    int
	maxActive int
}

type cancelUpdateStore struct {
	memoryTaskStore
	entered chan struct{}
	once    sync.Once
}

var errTerminalCommitOutcomeUnknown = errors.New(
	"forced terminal commit outcome unknown",
)

type outcomeUnknownTerminalStore struct {
	memoryTaskStore
	durable      persistedTask
	terminalOnce sync.Once
}

func (store *outcomeUnknownTerminalStore) updateTask(
	_ context.Context,
	taskID string,
	machineID string,
	status string,
	stats proto.TaskStats,
	lastErr string,
) (durableTaskState, error) {
	if isTerminalTaskStatus(store.durable.Status) {
		return durableTaskState{
			Status:  store.durable.Status,
			Stats:   store.durable.Stats,
			LastErr: store.durable.LastErr,
		}, nil
	}
	store.durable = persistedTask{
		Envelope: RoutedTask{
			MachineID: machineID,
			Task:      proto.Phase2Task{TaskID: taskID},
		},
		Status:  status,
		Stats:   stats,
		LastErr: lastErr,
	}
	var result error
	if isTerminalTaskStatus(status) {
		store.terminalOnce.Do(func() {
			result = errTerminalCommitOutcomeUnknown
		})
	}
	if result != nil {
		return durableTaskState{}, result
	}
	return durableTaskState{Status: status, Stats: stats, LastErr: lastErr}, nil
}

func (store *cancelUpdateStore) updateTask(
	ctx context.Context,
	_ string,
	_ string,
	_ string,
	_ proto.TaskStats,
	_ string,
) (durableTaskState, error) {
	store.once.Do(func() { close(store.entered) })
	<-ctx.Done()
	return durableTaskState{}, ctx.Err()
}

func (store *concurrencyStore) loadBuildSnapshot(
	ctx context.Context,
	kind uint8,
) (buildSnapshot, error) {
	store.mu.Lock()
	store.active++
	if store.active > store.maxActive {
		store.maxActive = store.active
	}
	store.mu.Unlock()
	time.Sleep(10 * time.Millisecond)
	snapshot, err := store.memoryTaskStore.loadBuildSnapshot(ctx, kind)
	store.mu.Lock()
	store.active--
	store.mu.Unlock()
	return snapshot, err
}

func (store *failingPersistStore) persistPending(
	ctx context.Context,
	task persistedTask,
) (persistedTask, error) {
	store.calls++
	if store.calls == store.failAt {
		return persistedTask{}, errors.New("forced persist failure")
	}
	return store.memoryTaskStore.persistPending(ctx, task)
}

type blockingUpdateStore struct {
	memoryTaskStore
	blockRunning      chan struct{}
	runningEntered    chan struct{}
	updateEntered     chan string
	allUpdatesBounded bool
	once              sync.Once
}

func newBlockingUpdateStore(snapshot buildSnapshot) *blockingUpdateStore {
	return &blockingUpdateStore{
		memoryTaskStore: memoryTaskStore{snapshots: map[uint8]buildSnapshot{
			proto.KindImage: snapshot,
			proto.KindVideo: {},
		}},
		runningEntered:    make(chan struct{}),
		updateEntered:     make(chan string, 4),
		allUpdatesBounded: true,
	}
}

func (store *blockingUpdateStore) updateTask(
	ctx context.Context,
	taskID string,
	machineID string,
	status string,
	stats proto.TaskStats,
	lastErr string,
) (durableTaskState, error) {
	if _, ok := ctx.Deadline(); !ok {
		store.allUpdatesBounded = false
	}
	if status == taskStatusRunning && store.blockRunning != nil {
		store.once.Do(func() { close(store.runningEntered) })
		<-store.blockRunning
	} else {
		store.updateEntered <- status
	}
	return store.memoryTaskStore.updateTask(
		ctx, taskID, machineID, status, stats, lastErr,
	)
}

func (store *memoryTaskStore) loadBuildSnapshot(
	_ context.Context,
	kind uint8,
) (buildSnapshot, error) {
	return store.snapshots[kind], nil
}

func (store *memoryTaskStore) persistPending(
	_ context.Context,
	task persistedTask,
) (persistedTask, error) {
	if store.statusByID == nil {
		store.statusByID = make(map[string]string)
		store.statusByMachine = make(map[string]string)
		store.lastErrByID = make(map[string]string)
	}
	store.persisted = append(store.persisted, task)
	store.statusByID[task.Envelope.Task.TaskID] = taskStatusSent
	store.statusByMachine[task.Envelope.MachineID] = taskStatusSent
	task.Status = taskStatusSent
	return task, nil
}

func (store *memoryTaskStore) restorePending(
	context.Context,
) ([]persistedTask, error) {
	var restored []persistedTask
	for _, task := range store.persisted {
		if store.statusByID[task.Envelope.Task.TaskID] != taskStatusDone &&
			store.statusByID[task.Envelope.Task.TaskID] != taskStatusFailed {
			restored = append(restored, task)
		}
	}
	return restored, nil
}

func (store *memoryTaskStore) updateTask(
	_ context.Context,
	taskID string,
	machineID string,
	status string,
	stats proto.TaskStats,
	lastErr string,
) (durableTaskState, error) {
	store.statusByID[taskID] = status
	store.statusByMachine[machineID] = status
	store.lastErrByID[taskID] = lastErr
	return durableTaskState{Status: status, Stats: stats, LastErr: lastErr}, nil
}

type sentTask struct {
	machineID string
	task      proto.Phase2Task
}

type recordingSender struct {
	online     map[string]bool
	sendErr    map[string]error
	beforeSend func()
	sent       []sentTask
}

type flappingOnlineSender struct {
	first map[string]bool
	calls map[string]int
}

func (sender *flappingOnlineSender) IsOnline(machineID string) bool {
	if sender.calls == nil {
		sender.calls = make(map[string]int)
	}
	sender.calls[machineID]++
	if sender.calls[machineID] == 1 {
		return sender.first[machineID]
	}
	return !sender.first[machineID]
}

func (*flappingOnlineSender) Send(string, uint8, any) error {
	return nil
}

func (sender *recordingSender) IsOnline(machineID string) bool {
	return sender.online[machineID]
}

func (sender *recordingSender) Send(
	machineID string,
	msgType uint8,
	value any,
) error {
	if sender.beforeSend != nil {
		sender.beforeSend()
	}
	if msgType != proto.MsgPhase2Task {
		panic("unexpected message type")
	}
	task := value.(*proto.Phase2Task)
	sender.sent = append(sender.sent, sentTask{machineID: machineID, task: *task})
	return sender.sendErr[machineID]
}
