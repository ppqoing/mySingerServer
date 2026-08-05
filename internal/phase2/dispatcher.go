package phase2

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"dedup/internal/config"
	"dedup/internal/features"
	"dedup/internal/proto"
)

type candidateMember struct {
	FileID int64
	SHA512 string
	Status string
}

type candidateGroup struct {
	Kind    string
	Members []candidateMember
}

type fileCopy struct {
	ID        int64
	MachineID string
	Path      string
	SHA512    string
	Size      int64
	MTime     int64
	Status    string
}

type frameFeature struct {
	FrameIdx   int
	PDQ256     []byte
	PHashParts []byte
	SobelHist  []byte
}

type featureState struct {
	PHashParts []byte
	SobelHist  []byte
	DurationMS int64
	Frames     []frameFeature
}

type buildSnapshot struct {
	Groups   []candidateGroup
	Copies   []fileCopy
	Features map[string]featureState
}

type snapshotLoader interface {
	loadBuildSnapshot(context.Context, uint8) (buildSnapshot, error)
}

type Sender interface {
	IsOnline(machineID string) bool
	Send(machineID string, msgType uint8, value any) error
}

// RoutedTask binds one immutable Phase2 wire envelope to its destination.
type RoutedTask struct {
	MachineID string
	Task      proto.Phase2Task
}

type BoundFeatureItem struct {
	Kind uint8
	Item proto.FeatureItem
}

type BoundFeatureResult struct {
	TaskID string
	Seq    uint64
	Items  []BoundFeatureItem
}

type Dispatcher struct {
	loader          snapshotLoader
	sender          Sender
	cfg             config.Phase2Config
	log             *slog.Logger
	memory          *taskMemory
	memoryOnce      sync.Once
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
	lifecycleMu     sync.Mutex
	lifecycleClosed bool
	lifecycleWG     sync.WaitGroup
	admissionMu     sync.Mutex
}

func newDispatcher(
	loader snapshotLoader,
	sender Sender,
	cfg config.Phase2Config,
	logger *slog.Logger,
) *Dispatcher {
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	return &Dispatcher{
		loader:          loader,
		sender:          sender,
		cfg:             cfg,
		log:             logger,
		memory:          &taskMemory{tasks: make(map[string]*taskEntry)},
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
	}
}

// Shutdown cancels lifecycle persistence and waits for admitted message
// handlers to leave the dispatcher.
func (d *Dispatcher) Shutdown() {
	d.lifecycleMu.Lock()
	if !d.lifecycleClosed {
		d.lifecycleClosed = true
		d.lifecycleCancel()
	}
	d.lifecycleMu.Unlock()
	d.lifecycleWG.Wait()
}

func (d *Dispatcher) beginLifecycle() bool {
	d.lifecycleMu.Lock()
	defer d.lifecycleMu.Unlock()
	if d.lifecycleClosed {
		return false
	}
	d.lifecycleWG.Add(1)
	return true
}

// BuildTasks derives deterministic, machine-routed Phase2 task envelopes from
// the current M3 candidate snapshot.
func (d *Dispatcher) BuildTasks(
	ctx context.Context,
	kind uint8,
) ([]RoutedTask, error) {
	if kind != proto.KindImage && kind != proto.KindVideo {
		return nil, fmt.Errorf("phase2: unsupported kind %d", kind)
	}
	if d.loader == nil || d.sender == nil {
		return nil, fmt.Errorf("phase2: dispatcher is not configured")
	}
	if d.cfg.TaskShardSize < 1 || d.cfg.TaskShardSize > maxShardItems {
		return nil, fmt.Errorf(
			"phase2: task shard size must be between 1 and %d",
			maxShardItems,
		)
	}
	snapshot, err := d.loader.loadBuildSnapshot(ctx, kind)
	if err != nil {
		return nil, err
	}

	expectedCandidate := candidateImage
	if kind == proto.KindVideo {
		expectedCandidate = candidateVideo
	}
	pairs, err := normalizedPairs(snapshot.Groups, expectedCandidate)
	if err != nil {
		return nil, err
	}
	relevantSHAs := make(map[string]struct{}, len(pairs)*2)
	for _, pair := range pairs {
		relevantSHAs[pair[0]] = struct{}{}
		relevantSHAs[pair[1]] = struct{}{}
	}
	copies, err := chooseCopies(snapshot.Copies, relevantSHAs, d.sender)
	if err != nil {
		return nil, err
	}

	type selectedItem struct {
		item proto.Phase2Item
	}
	var selected []selectedItem
	seenSHA := make(map[string]bool)
	for _, pair := range pairs {
		firstCopy, firstOK := copies[pair[0]]
		secondCopy, secondOK := copies[pair[1]]
		if !firstOK || !secondOK {
			continue
		}
		if err := validateSelectedIdentity(kind, pair[0], firstCopy, snapshot.Features[pair[0]]); err != nil {
			return nil, err
		}
		if err := validateSelectedIdentity(kind, pair[1], secondCopy, snapshot.Features[pair[1]]); err != nil {
			return nil, err
		}
		for _, entry := range []struct {
			sha  string
			copy fileCopy
		}{
			{sha: pair[0], copy: firstCopy},
			{sha: pair[1], copy: secondCopy},
		} {
			if seenSHA[entry.sha] {
				continue
			}
			item, needed, itemErr := buildItem(
				kind,
				entry.sha,
				entry.copy,
				snapshot.Features[entry.sha],
			)
			if itemErr != nil {
				return nil, itemErr
			}
			if !needed {
				continue
			}
			seenSHA[entry.sha] = true
			selected = append(selected, selectedItem{item: item})
		}
	}

	sort.Slice(selected, func(i, j int) bool {
		a, b := selected[i].item, selected[j].item
		if a.MachineID != b.MachineID {
			return a.MachineID < b.MachineID
		}
		if a.SHA512 != b.SHA512 {
			return a.SHA512 < b.SHA512
		}
		return a.Path < b.Path
	})

	var routed []RoutedTask
	for start := 0; start < len(selected); {
		machineID := selected[start].item.MachineID
		machineEnd := start
		for machineEnd < len(selected) &&
			selected[machineEnd].item.MachineID == machineID {
			machineEnd++
		}
		for shardStart := start; shardStart < machineEnd; shardStart += d.cfg.TaskShardSize {
			shardEnd := min(shardStart+d.cfg.TaskShardSize, machineEnd)
			task := RoutedTask{
				MachineID: machineID,
				Task: proto.Phase2Task{
					Items: make([]proto.Phase2Item, 0, shardEnd-shardStart),
				},
			}
			for _, entry := range selected[shardStart:shardEnd] {
				task.Task.Items = append(task.Task.Items, entry.item)
			}
			if err := finalizeTaskEnvelope(&task); err != nil {
				return nil, err
			}
			routed = append(routed, task)
		}
		start = machineEnd
	}
	return routed, nil
}

func finalizeTaskEnvelope(task *RoutedTask) error {
	task.Task.TaskID = stableTaskID(*task)
	if _, err := proto.EncodeFramePayload(proto.MsgPhase2Task, &task.Task); err != nil {
		return fmt.Errorf("phase2: invalid task wire envelope: %w", err)
	}
	return nil
}

func normalizedPairs(
	groups []candidateGroup,
	expectedKind string,
) ([][2]string, error) {
	unique := make(map[[2]string]struct{})
	for _, group := range groups {
		if group.Kind != expectedKind {
			continue
		}
		distinct := make(map[string]struct{}, 2)
		for _, member := range group.Members {
			if member.Status == proto.StatusDeleted {
				continue
			}
			if !isCanonicalSHA512(member.SHA512) {
				return nil, fmt.Errorf(
					"phase2: %s group has noncanonical live SHA-512 on file %d",
					expectedKind,
					member.FileID,
				)
			}
			distinct[member.SHA512] = struct{}{}
		}
		if len(distinct) != 2 {
			continue
		}
		shas := make([]string, 0, 2)
		for sha := range distinct {
			shas = append(shas, sha)
		}
		sort.Strings(shas)
		unique[[2]string{shas[0], shas[1]}] = struct{}{}
	}
	pairs := make([][2]string, 0, len(unique))
	for pair := range unique {
		pairs = append(pairs, pair)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i][0] != pairs[j][0] {
			return pairs[i][0] < pairs[j][0]
		}
		return pairs[i][1] < pairs[j][1]
	})
	return pairs, nil
}

func chooseCopies(
	candidates []fileCopy,
	relevantSHAs map[string]struct{},
	sender Sender,
) (map[string]fileCopy, error) {
	bySHA := make(map[string][]fileCopy)
	machines := make(map[string]struct{})
	for _, candidate := range candidates {
		if candidate.Status == proto.StatusDeleted {
			continue
		}
		if _, relevant := relevantSHAs[candidate.SHA512]; !relevant {
			continue
		}
		if err := validateLiveCopyIdentity(candidate); err != nil {
			return nil, err
		}
		bySHA[candidate.SHA512] = append(bySHA[candidate.SHA512], candidate)
		machines[candidate.MachineID] = struct{}{}
	}
	online := make(map[string]bool, len(machines))
	for machineID := range machines {
		online[machineID] = sender.IsOnline(machineID)
	}
	chosen := make(map[string]fileCopy, len(bySHA))
	for sha, copies := range bySHA {
		sort.Slice(copies, func(i, j int) bool {
			onlineI, onlineJ := online[copies[i].MachineID], online[copies[j].MachineID]
			if onlineI != onlineJ {
				return onlineI
			}
			if copies[i].MachineID != copies[j].MachineID {
				return copies[i].MachineID < copies[j].MachineID
			}
			if copies[i].Path != copies[j].Path {
				return copies[i].Path < copies[j].Path
			}
			return copies[i].ID < copies[j].ID
		})
		chosen[sha] = copies[0]
	}
	return chosen, nil
}

func validateLiveCopyIdentity(copy fileCopy) error {
	if !isCanonicalSHA512(copy.SHA512) ||
		copy.MachineID == "" ||
		copy.Path == "" ||
		copy.Size < 0 ||
		copy.MTime < 0 {
		return fmt.Errorf(
			"phase2: invalid live file identity id=%d sha512=%q",
			copy.ID,
			copy.SHA512,
		)
	}
	switch copy.Status {
	case proto.StatusPending, proto.StatusDone, proto.StatusPartial,
		proto.StatusFailed, proto.StatusCrash:
		return nil
	default:
		return fmt.Errorf(
			"phase2: invalid live file status %q for id=%d",
			copy.Status,
			copy.ID,
		)
	}
}

func buildItem(
	kind uint8,
	sha string,
	copy fileCopy,
	state featureState,
) (proto.Phase2Item, bool, error) {
	item := proto.Phase2Item{
		Path:      copy.Path,
		MachineID: copy.MachineID,
		SHA512:    sha,
		Size:      copy.Size,
		MTimeMS:   copy.MTime,
		Kind:      kind,
	}
	if kind == proto.KindImage {
		if _, err := features.DecodePHashParts(state.PHashParts); err != nil {
			item.FieldsMask |= proto.FieldPHashParts
		}
		if _, err := features.DecodeSobelHist(state.SobelHist); err != nil {
			item.FieldsMask |= proto.FieldSobelHist
		}
		if item.FieldsMask == 0 {
			return item, false, nil
		}
		if err := item.Validate(); err != nil {
			return proto.Phase2Item{}, false, fmt.Errorf(
				"phase2: invalid selected image item %s: %w",
				sha,
				err,
			)
		}
		return item, true, nil
	}

	item.DurationMS = state.DurationMS
	validFrames := make(map[int]bool, int(proto.FrameMaskFull))
	for _, frame := range state.Frames {
		if frame.FrameIdx < 0 || frame.FrameIdx >= 6 ||
			len(frame.PDQ256) != 32 {
			continue
		}
		if _, err := features.DecodePHashParts(frame.PHashParts); err != nil {
			continue
		}
		if _, err := features.DecodeSobelHist(frame.SobelHist); err != nil {
			continue
		}
		validFrames[frame.FrameIdx] = true
	}
	for index := 0; index < 6; index++ {
		if !validFrames[index] {
			item.FrameMask |= 1 << index
		}
	}
	if item.FrameMask != 0 {
		item.FieldsMask = proto.FieldVideo6F
	}
	if item.FieldsMask == 0 {
		return item, false, nil
	}
	if err := item.Validate(); err != nil {
		return proto.Phase2Item{}, false, fmt.Errorf(
			"phase2: invalid selected video item %s: %w",
			sha,
			err,
		)
	}
	return item, true, nil
}

func validateSelectedIdentity(
	kind uint8,
	sha string,
	copy fileCopy,
	state featureState,
) error {
	if copy.MachineID == "" || copy.Path == "" ||
		!isCanonicalSHA512(sha) ||
		copy.Size < 0 || copy.MTime < 0 {
		return fmt.Errorf("phase2: invalid selected file identity for %s", sha)
	}
	switch copy.Status {
	case proto.StatusPending, proto.StatusDone, proto.StatusPartial,
		proto.StatusFailed, proto.StatusCrash:
	default:
		return fmt.Errorf(
			"phase2: invalid selected file status %q for %s",
			copy.Status,
			sha,
		)
	}
	if kind == proto.KindVideo && state.DurationMS <= 0 {
		return fmt.Errorf(
			"phase2: video duration must be positive for %s",
			sha,
		)
	}
	return nil
}

func stableTaskID(task RoutedTask) string {
	payload := struct {
		Version int                `json:"version"`
		Machine string             `json:"machine_id"`
		Items   []proto.Phase2Item `json:"items"`
	}{
		Version: 1,
		Machine: task.MachineID,
		Items:   task.Task.Items,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("phase2: marshal deterministic task ID: %v", err))
	}
	sum := sha256.Sum256(raw)
	return "phase2-" + hex.EncodeToString(sum[:])
}

// BindFeatureResult validates an entire result batch against its still-active
// Task 7 envelope before returning an ownership-independent deep copy.
func (d *Dispatcher) BindFeatureResult(
	machineID string,
	result *proto.FeatureResult,
) (*BoundFeatureResult, error) {
	if result == nil || result.TaskID == "" || len(result.Items) == 0 {
		return nil, fmt.Errorf("phase2: invalid feature result envelope")
	}
	memory := d.ensureMemory()
	memory.mu.Lock()
	entry := memory.tasks[result.TaskID]
	memory.mu.Unlock()
	if entry == nil {
		return nil, fmt.Errorf("phase2: unknown feature result task %q", result.TaskID)
	}
	entry.mu.Lock()
	task := clonePersistedTask(entry.task)
	pendingTerminal := entry.pendingTerminal
	entry.mu.Unlock()
	if task.Envelope.MachineID != machineID {
		return nil, fmt.Errorf(
			"phase2: feature result machine %q does not own task %q",
			machineID,
			result.TaskID,
		)
	}
	if pendingTerminal || isTerminalTaskStatus(task.Status) {
		return nil, fmt.Errorf("phase2: feature result task %q is terminal", result.TaskID)
	}

	requests := make(map[string]proto.Phase2Item, len(task.Envelope.Task.Items))
	for _, requested := range task.Envelope.Task.Items {
		if _, duplicate := requests[requested.Path]; duplicate {
			return nil, fmt.Errorf("phase2: task %q has duplicate request path", result.TaskID)
		}
		requests[requested.Path] = requested
	}
	bound := &BoundFeatureResult{
		TaskID: result.TaskID,
		Seq:    result.Seq,
		Items:  make([]BoundFeatureItem, 0, len(result.Items)),
	}
	seenPaths := make(map[string]struct{}, len(result.Items))
	seenSHAs := make(map[string]struct{}, len(result.Items))
	for index, item := range result.Items {
		requested, exists := requests[item.Path]
		if !exists {
			return nil, fmt.Errorf(
				"phase2: feature result item %d path %q was not requested",
				index,
				item.Path,
			)
		}
		if item.SHA512 != requested.SHA512 {
			return nil, fmt.Errorf("phase2: feature result item %d SHA mismatch", index)
		}
		if _, duplicate := seenPaths[item.Path]; duplicate {
			return nil, fmt.Errorf("phase2: duplicate feature result path %q", item.Path)
		}
		if _, duplicate := seenSHAs[item.SHA512]; duplicate {
			return nil, fmt.Errorf("phase2: duplicate feature result SHA %q", item.SHA512)
		}
		seenPaths[item.Path] = struct{}{}
		seenSHAs[item.SHA512] = struct{}{}
		if item.FieldsDone&^requested.FieldsMask != 0 {
			return nil, fmt.Errorf(
				"phase2: feature result item %d reports unrequested fields",
				index,
			)
		}
		if len(item.PHashParts) != 0 &&
			requested.FieldsMask&proto.FieldPHashParts == 0 {
			return nil, fmt.Errorf("phase2: feature result item %d has unrequested pHash", index)
		}
		if len(item.SobelHist) != 0 &&
			requested.FieldsMask&proto.FieldSobelHist == 0 {
			return nil, fmt.Errorf("phase2: feature result item %d has unrequested Sobel", index)
		}
		if len(item.Frames) != 0 &&
			requested.FieldsMask&proto.FieldVideo6F == 0 {
			return nil, fmt.Errorf("phase2: feature result item %d has unrequested frames", index)
		}
		hasFileError := false
		for _, fieldError := range item.FieldErrors {
			if fieldError.Field == 0 {
				hasFileError = true
				continue
			}
			if fieldError.Field&(fieldError.Field-1) != 0 ||
				fieldError.Field&^(proto.FieldPHashParts|
					proto.FieldSobelHist|proto.FieldVideo6F) != 0 ||
				fieldError.Field&^requested.FieldsMask != 0 {
				return nil, fmt.Errorf(
					"phase2: feature result item %d has invalid field error mask",
					index,
				)
			}
			if fieldError.Field&item.FieldsDone != 0 {
				return nil, fmt.Errorf(
					"phase2: feature result item %d reports a field as both failed and done",
					index,
				)
			}
		}
		effectiveFrameMask := requested.FrameMask
		if effectiveFrameMask == 0 && requested.Kind == proto.KindVideo {
			effectiveFrameMask = proto.FrameMaskFull
		}
		frameIndexes := make(map[int]proto.FrameFeature, len(item.Frames))
		for _, frame := range item.Frames {
			if frame.FrameIdx < 0 || frame.FrameIdx >= 6 ||
				effectiveFrameMask&(1<<uint(frame.FrameIdx)) == 0 {
				return nil, fmt.Errorf(
					"phase2: feature result item %d frame %d was not requested",
					index,
					frame.FrameIdx,
				)
			}
			if _, duplicate := frameIndexes[frame.FrameIdx]; duplicate {
				return nil, fmt.Errorf(
					"phase2: feature result item %d duplicates frame %d",
					index,
					frame.FrameIdx,
				)
			}
			frameIndexes[frame.FrameIdx] = frame
		}
		if hasFileError {
			if item.FieldsDone != 0 ||
				len(item.PHashParts) != 0 ||
				len(item.SobelHist) != 0 {
				return nil, fmt.Errorf(
					"phase2: feature result item %d combines a file error with successful fields",
					index,
				)
			}
			for _, frame := range item.Frames {
				if frame.Error == "" ||
					len(frame.PDQ256) != 0 ||
					len(frame.PHashParts) != 0 ||
					len(frame.SobelHist) != 0 {
					return nil, fmt.Errorf(
						"phase2: feature result item %d combines a file error with a successful frame",
						index,
					)
				}
			}
		}
		if item.FieldsDone&proto.FieldVideo6F != 0 {
			for frameIndex := 0; frameIndex < 6; frameIndex++ {
				if effectiveFrameMask&(1<<uint(frameIndex)) == 0 {
					continue
				}
				frame, exists := frameIndexes[frameIndex]
				if !exists {
					return nil, fmt.Errorf(
						"phase2: feature result item %d video success misses frame %d",
						index,
						frameIndex,
					)
				}
				if frame.Error != "" || len(frame.PDQ256) != 32 {
					return nil, fmt.Errorf(
						"phase2: feature result item %d video success has incomplete frame %d",
						index,
						frameIndex,
					)
				}
				if _, err := features.DecodePHashParts(frame.PHashParts); err != nil {
					return nil, fmt.Errorf(
						"phase2: feature result item %d frame %d pHash: %w",
						index,
						frameIndex,
						err,
					)
				}
				if _, err := features.DecodeSobelHist(frame.SobelHist); err != nil {
					return nil, fmt.Errorf(
						"phase2: feature result item %d frame %d Sobel: %w",
						index,
						frameIndex,
						err,
					)
				}
			}
		}
		bound.Items = append(bound.Items, BoundFeatureItem{
			Kind: requested.Kind,
			Item: cloneBoundFeatureItem(item),
		})
	}
	return bound, nil
}

func cloneBoundFeatureItem(item proto.FeatureItem) proto.FeatureItem {
	item.FieldErrors = append([]proto.FieldError(nil), item.FieldErrors...)
	item.PHashParts = append([]byte(nil), item.PHashParts...)
	item.SobelHist = append([]byte(nil), item.SobelHist...)
	if item.DurationMS != nil {
		value := *item.DurationMS
		item.DurationMS = &value
	}
	frames := item.Frames
	item.Frames = make([]proto.FrameFeature, len(frames))
	for index, frame := range frames {
		item.Frames[index] = frame
		item.Frames[index].PDQ256 = append([]byte(nil), frame.PDQ256...)
		item.Frames[index].PHashParts = append([]byte(nil), frame.PHashParts...)
		item.Frames[index].SobelHist = append([]byte(nil), frame.SobelHist...)
	}
	return item
}

func isCanonicalSHA512(value string) bool {
	if len(value) != 128 {
		return false
	}
	for _, ch := range value {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}
