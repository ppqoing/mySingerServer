package worker

import (
	"context"
	"fmt"
	"sync"

	"dedup/internal/proto"
	"dedup/internal/store"
)

type FeatureLookup interface {
	LookupImage(context.Context, []byte) (*store.ImageFeature, error)
	LookupVideo(context.Context, []byte) (*store.VideoFeature, error)
}

type contentLookup interface {
	LookupContent(
		context.Context,
		[]byte,
		store.MediaKind,
		uint32,
		uint8,
	) (store.ContentState, error)
}

type dedupeKey struct {
	task   string
	kind   MediaKind
	sha    [64]byte
	fields uint32
	frames uint8
}

type flight struct {
	owner    int64
	done     chan struct{}
	waiters  int
	reply    *SHAReplyMsg
	computed bool
}

type Deduper struct {
	lookup   any
	mu       sync.Mutex
	flights  map[dedupeKey]*flight
	computed map[dedupeKey]SHAReplyMsg
}

func NewDeduper(lookup any) *Deduper {
	return &Deduper{
		lookup: lookup, flights: make(map[dedupeKey]*flight),
		computed: make(map[dedupeKey]SHAReplyMsg),
	}
}

func (d *Deduper) Ask(ctx context.Context, query SHAQueryMsg) (SHAReplyMsg, error) {
	normalized, err := normalizeSHAQuery(query)
	if err != nil {
		return SHAReplyMsg{JobID: query.JobID}, err
	}
	query = normalized
	key, err := dedupeKeyForQuery(query)
	if err != nil {
		return SHAReplyMsg{JobID: query.JobID}, err
	}
	if d.lookup == nil {
		return SHAReplyMsg{JobID: query.JobID}, fmt.Errorf("worker deduper: feature lookup is required")
	}

	for {
		if err := ctx.Err(); err != nil {
			return SHAReplyMsg{JobID: query.JobID}, err
		}
		d.mu.Lock()
		if reply, ok := d.computed[key]; ok {
			d.mu.Unlock()
			reused := replyForJob(query.JobID, reply)
			reused.ReusedFlight = true
			return reused, nil
		}
		current := d.flights[key]
		if current == nil {
			current = &flight{owner: query.JobID, done: make(chan struct{})}
			d.flights[key] = current
			d.mu.Unlock()

			reply, found, err := d.lookupFeature(ctx, query)
			if err != nil {
				d.finish(key, current, nil, false)
				return SHAReplyMsg{JobID: query.JobID}, err
			}
			if found {
				reply.JobID = query.JobID
				d.finish(key, current, &reply, false)
				return replyForJob(query.JobID, reply), nil
			}
			reply.JobID = query.JobID
			return replyForJob(query.JobID, reply), nil
		}
		if current.owner == query.JobID {
			d.mu.Unlock()
			return SHAReplyMsg{JobID: query.JobID}, fmt.Errorf("worker deduper: job %d already owns this SHA", query.JobID)
		}
		current.waiters++
		done := current.done
		d.mu.Unlock()

		select {
		case <-done:
			d.mu.Lock()
			current.waiters--
			reply := current.reply
			computed := current.computed
			d.mu.Unlock()
			if reply != nil {
				reused := replyForJob(query.JobID, *reply)
				reused.ReusedFlight = computed
				return reused, nil
			}
			if err := ctx.Err(); err != nil {
				return SHAReplyMsg{JobID: query.JobID}, err
			}
		case <-ctx.Done():
			d.mu.Lock()
			current.waiters--
			d.mu.Unlock()
			return SHAReplyMsg{JobID: query.JobID}, ctx.Err()
		}
	}
}

// EndTask releases completed single-flight replies once TaskDone has captured
// the task metrics. Persistent feature-store hits in later scans are therefore
// not misreported as computed-flight reuse.
func (d *Deduper) EndTask(taskID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for key := range d.computed {
		if key.task == taskID {
			delete(d.computed, key)
		}
	}
}

func (d *Deduper) Resolve(result JobResultMsg) {
	base, err := dedupeKeyForTask(result.ScanTaskID, result.Kind, result.SHA512)
	if err != nil {
		return
	}
	d.mu.Lock()
	key, current := d.flightForResolveLocked(base, result.JobID)
	if current == nil {
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()

	reply, complete := replyFromCommittedResult(result, key)
	if !complete {
		d.finish(key, current, nil, false)
		return
	}
	d.finish(key, current, &reply, true)
}

func (d *Deduper) FailByJob(jobID int64) {
	d.mu.Lock()
	var failed []dedupeKey
	for key, current := range d.flights {
		if current.owner == jobID {
			failed = append(failed, key)
		}
	}
	d.mu.Unlock()
	for _, key := range failed {
		d.mu.Lock()
		current := d.flights[key]
		if current != nil && current.owner == jobID {
			delete(d.flights, key)
			close(current.done)
		}
		d.mu.Unlock()
	}
}

func (d *Deduper) flightForResolveLocked(base dedupeKey, owner int64) (dedupeKey, *flight) {
	for key, current := range d.flights {
		if key.task == base.task && key.kind == base.kind && key.sha == base.sha &&
			current.owner == owner {
			return key, current
		}
	}
	return dedupeKey{}, nil
}

func (d *Deduper) lookupFeature(ctx context.Context, query SHAQueryMsg) (SHAReplyMsg, bool, error) {
	if lookup, ok := d.lookup.(contentLookup); ok {
		kind, err := storeKind(query.Kind)
		if err != nil {
			return SHAReplyMsg{}, false, err
		}
		state, err := lookup.LookupContent(
			ctx,
			cloneBytes(query.SHA512),
			kind,
			query.RequestedFields,
			query.RequestedFrames,
		)
		if err != nil {
			return SHAReplyMsg{}, false, fmt.Errorf("worker deduper: lookup content: %w", err)
		}
		reply := replyFromContentState(query, state)
		if err := reply.ValidateMasks(); err != nil {
			return SHAReplyMsg{}, false, fmt.Errorf("worker deduper: invalid content state: %w", err)
		}
		return reply, reply.Found, nil
	}

	legacy, ok := d.lookup.(FeatureLookup)
	if !ok {
		return SHAReplyMsg{}, false, fmt.Errorf("worker deduper: content lookup is required")
	}
	switch query.Kind {
	case MediaImage:
		feature, err := legacy.LookupImage(ctx, cloneBytes(query.SHA512))
		if err != nil {
			return SHAReplyMsg{}, false, fmt.Errorf("worker deduper: lookup image: %w", err)
		}
		if feature == nil {
			return missingReply(query), false, nil
		}
		reply := missingReply(query)
		if query.RequestedFields&MaskSHA512 != 0 {
			reply.FieldsPresent |= MaskSHA512
			reply.MissingFields &^= MaskSHA512
		}
		if query.RequestedFields&MaskImagePDQ != 0 && len(feature.PDQ) != 0 {
			reply.FieldsPresent |= MaskImagePDQ
			reply.MissingFields &^= MaskImagePDQ
		}
		reply.PDQ = cloneBytes(feature.PDQ)
		reply.Quality = feature.Quality
		reply.Width = feature.Width
		reply.Height = feature.Height
		reply.Found = reply.MissingFields == 0 && reply.MissingFrames == 0
		return reply, reply.Found, nil
	case MediaVideo:
		feature, err := legacy.LookupVideo(ctx, cloneBytes(query.SHA512))
		if err != nil {
			return SHAReplyMsg{}, false, fmt.Errorf("worker deduper: lookup video: %w", err)
		}
		reply := missingReply(query)
		if feature == nil {
			return reply, false, nil
		}
		durationOK := feature.DurationMS != nil && *feature.DurationMS >= 0
		legacyContactOK := durationOK && feature.ThumbPath != "" && len(feature.ThumbPDQ) == 32 &&
			feature.ThumbQuality != nil && *feature.ThumbQuality >= 0 && *feature.ThumbQuality <= 100
		contactOK := legacyContactOK && feature.ThumbWidth != nil && *feature.ThumbWidth > 0 &&
			feature.ThumbHeight != nil && *feature.ThumbHeight > 0
		present := uint32(0)
		if query.RequestedFields&MaskSHA512 != 0 && len(feature.SHA512) == 64 {
			present |= MaskSHA512
		}
		if query.RequestedFields&MaskVideoDuration != 0 && durationOK {
			present |= MaskVideoDuration
		}
		if query.RequestedFields&MaskVideoThumb != 0 && legacyContactOK {
			present |= MaskVideoThumb
		}
		if query.RequestedFields&MaskVideoContactSheet != 0 && contactOK {
			present |= MaskVideoContactSheet
		}
		reply.FieldsPresent = present
		reply.MissingFields &^= present
		reply.DurationMS = cloneInt64(feature.DurationMS)
		reply.ThumbPath = feature.ThumbPath
		reply.ThumbPDQ = cloneBytes(feature.ThumbPDQ)
		reply.ThumbQuality = cloneInt32(feature.ThumbQuality)
		reply.Found = reply.MissingFields == 0 && reply.MissingFrames == 0
		return reply, reply.Found, nil
	default:
		return SHAReplyMsg{}, false, fmt.Errorf("worker deduper: invalid media kind %d", query.Kind)
	}
}

func (d *Deduper) finish(
	key dedupeKey,
	current *flight,
	reply *SHAReplyMsg,
	computed bool,
) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.flights[key] != current {
		return
	}
	if reply != nil {
		copied := cloneReply(*reply)
		current.reply = &copied
		current.computed = computed
		if computed {
			d.computed[key] = cloneReply(copied)
		}
	}
	delete(d.flights, key)
	close(current.done)
}

func dedupeKeyFor(kind MediaKind, sha []byte) (dedupeKey, error) {
	return dedupeKeyForTask("", kind, sha)
}

func dedupeKeyForTask(taskID string, kind MediaKind, sha []byte) (dedupeKey, error) {
	if kind != MediaImage && kind != MediaVideo {
		return dedupeKey{}, fmt.Errorf("worker deduper: invalid media kind %d", kind)
	}
	if len(sha) != 64 {
		return dedupeKey{}, fmt.Errorf("worker deduper: SHA-512 must be exactly 64 bytes, got %d", len(sha))
	}
	fields := store.RequiredStageOneMask(store.MediaImage)
	if kind == MediaVideo {
		fields = store.RequiredStageOneMask(store.MediaVideo)
	}
	return dedupeKey{
		task: taskID, kind: kind, sha: shaKey(sha), fields: fields,
	}, nil
}

func dedupeKeyForQuery(query SHAQueryMsg) (dedupeKey, error) {
	key, err := dedupeKeyForTask(query.ScanTaskID, query.Kind, query.SHA512)
	if err != nil {
		return dedupeKey{}, err
	}
	key.fields = query.RequestedFields
	key.frames = query.RequestedFrames
	return key, nil
}

func normalizeSHAQuery(query SHAQueryMsg) (SHAQueryMsg, error) {
	if err := query.ValidateMasks(); err != nil {
		return query, err
	}
	switch query.Kind {
	case MediaImage:
		if query.RequestedFields == 0 {
			query.RequestedFields = store.RequiredStageOneMask(store.MediaImage)
		}
		if query.RequestedFrames != 0 {
			return query, fmt.Errorf("worker deduper: image query cannot request video frames")
		}
	case MediaVideo:
		if query.RequestedFields == 0 {
			query.RequestedFields = store.RequiredStageOneMask(store.MediaVideo)
		}
		if query.RequestedFields&videoSixFrameWorkerFields() != 0 && query.RequestedFrames == 0 {
			query.RequestedFrames = FrameMaskFull
		}
	default:
		return query, fmt.Errorf("worker deduper: invalid media kind %d", query.Kind)
	}
	return query, nil
}

func storeKind(kind MediaKind) (store.MediaKind, error) {
	switch kind {
	case MediaImage:
		return store.MediaImage, nil
	case MediaVideo:
		return store.MediaVideo, nil
	default:
		return "", fmt.Errorf("worker deduper: invalid media kind %d", kind)
	}
}

func missingReply(query SHAQueryMsg) SHAReplyMsg {
	return SHAReplyMsg{
		JobID:           query.JobID,
		RequestedFields: query.RequestedFields,
		MissingFields:   query.RequestedFields,
		RequestedFrames: query.RequestedFrames,
		MissingFrames:   query.RequestedFrames,
	}
}

func replyFromContentState(query SHAQueryMsg, state store.ContentState) SHAReplyMsg {
	reply := SHAReplyMsg{
		JobID:           query.JobID,
		RequestedFields: query.RequestedFields,
		FieldsPresent:   state.FieldsPresent,
		MissingFields:   state.MissingFields,
		RequestedFrames: query.RequestedFrames,
		FramesPresent:   state.FramesPresent,
		MissingFrames:   state.MissingFrames,
	}
	if state.Image != nil {
		reply.PDQ = cloneBytes(state.Image.PDQ)
		reply.Quality = state.Image.Quality
		reply.Width = state.Image.Width
		reply.Height = state.Image.Height
	}
	if state.Video != nil {
		reply.DurationMS = cloneInt64(state.Video.DurationMS)
		reply.ThumbPath = state.Video.ThumbPath
		reply.ThumbPDQ = cloneBytes(state.Video.ThumbPDQ)
		reply.ThumbQuality = cloneInt32(state.Video.ThumbQuality)
	}
	if state.FieldsPresent&MaskVideoMetadata != 0 {
		reply.VideoContainer, reply.VideoStreams = cloneVideoMetadata(
			state.VideoContainer, state.VideoStreams,
		)
	}
	for _, frame := range state.Frames {
		if frame.FrameIdx < 0 || frame.FrameIdx >= len(reply.FrameResults) {
			continue
		}
		reply.FrameResults[frame.FrameIdx] = FrameResult{
			FrameIdx: frame.FrameIdx, Status: 0,
			PDQ256:     cloneBytes(frame.PDQ256),
			PHashParts: cloneBytes(frame.PHashParts),
			SobelHist:  cloneBytes(frame.SobelHist),
		}
	}
	reply.Found = reply.MissingFields == 0 && reply.MissingFrames == 0
	return reply
}

func shaKey(sha []byte) [64]byte {
	var result [64]byte
	copy(result[:], sha)
	return result
}

func replyFromCommittedResult(result JobResultMsg, key dedupeKey) (SHAReplyMsg, bool) {
	reply := SHAReplyMsg{
		JobID:           result.JobID,
		RequestedFields: key.fields,
		MissingFields:   key.fields,
		RequestedFrames: key.frames,
		MissingFrames:   key.frames,
		PDQ:             cloneBytes(result.PDQ),
		Quality:         result.Quality,
		Width:           result.Width,
		Height:          result.Height,
		DurationMS:      cloneInt64(result.DurationMS),
		ThumbPath:       result.ThumbPath,
		ThumbPDQ:        cloneBytes(result.ThumbPDQ),
		ThumbQuality:    cloneInt32(result.ThumbQuality),
	}
	if len(result.SHA512) == 64 && key.fields&MaskSHA512 != 0 {
		reply.FieldsPresent |= MaskSHA512
	}
	if key.fields&MaskImagePDQ != 0 && result.FieldsDone&MaskImagePDQ != 0 && len(result.PDQ) != 0 {
		reply.FieldsPresent |= MaskImagePDQ
	}
	if key.fields&MaskVideoThumb != 0 && result.FieldsDone&MaskVideoThumb != 0 &&
		result.DurationMS != nil && result.ThumbPath != "" &&
		len(result.ThumbPDQ) != 0 && result.ThumbQuality != nil {
		reply.FieldsPresent |= MaskVideoThumb
	}
	if key.fields&MaskPHashParts != 0 && result.FieldsDone&MaskPHashParts != 0 && len(result.PHashParts) != 0 {
		reply.FieldsPresent |= MaskPHashParts
	}
	if key.fields&MaskSobelHist != 0 && result.FieldsDone&MaskSobelHist != 0 && len(result.SobelHist) != 0 {
		reply.FieldsPresent |= MaskSobelHist
	}
	if key.fields&MaskVideoDuration != 0 && result.FieldsDone&MaskVideoDuration != 0 && result.DurationMS != nil {
		reply.FieldsPresent |= MaskVideoDuration
	}
	if key.fields&MaskVideoContactSheet != 0 && result.FieldsDone&MaskVideoContactSheet != 0 &&
		result.ThumbPath != "" && len(result.ThumbPDQ) != 0 && result.ThumbQuality != nil {
		reply.FieldsPresent |= MaskVideoContactSheet
	}
	if key.fields&MaskVideoMetadata != 0 && result.FieldsDone&MaskVideoMetadata != 0 &&
		result.VideoContainer != nil {
		reply.FieldsPresent |= MaskVideoMetadata
		reply.VideoContainer, reply.VideoStreams = cloneVideoMetadata(
			result.VideoContainer, result.VideoStreams,
		)
	}
	reply.FramesPresent = result.FramesDone & key.frames
	if key.fields&MaskVideo6F != 0 && result.FieldsDone&MaskVideo6F != 0 &&
		reply.FramesPresent == key.frames {
		reply.FieldsPresent |= MaskVideo6F
	}
	if key.fields&MaskVideo6FPHash != 0 && result.FieldsDone&MaskVideo6FPHash != 0 &&
		reply.FramesPresent == key.frames {
		reply.FieldsPresent |= MaskVideo6FPHash
	}
	if key.fields&MaskVideo6FSobel != 0 && result.FieldsDone&MaskVideo6FSobel != 0 &&
		reply.FramesPresent == key.frames {
		reply.FieldsPresent |= MaskVideo6FSobel
	}
	for index, frame := range result.FrameResults {
		bit := uint8(1 << uint(index))
		if key.frames&bit == 0 || result.FramesDone&bit == 0 {
			continue
		}
		reply.FrameResults[index] = FrameResult{
			FrameIdx: frame.FrameIdx, Status: 0, TimeMS: frame.TimeMS,
			PDQ256: cloneBytes(frame.PDQ256), Quality: frame.Quality,
			PHashParts: cloneBytes(frame.PHashParts), SobelHist: cloneBytes(frame.SobelHist),
		}
	}
	for _, frame := range result.Frames {
		if frame.FrameIdx < 0 || frame.FrameIdx >= len(reply.FrameResults) {
			continue
		}
		reply.FrameResults[frame.FrameIdx] = FrameResult{
			FrameIdx: frame.FrameIdx, Status: 0, TimeMS: frame.TimeMS,
			PDQ256: cloneBytes(frame.PDQ256), Quality: frame.Quality,
			PHashParts: cloneBytes(frame.PHashParts), SobelHist: cloneBytes(frame.SobelHist),
		}
	}
	reply.MissingFields &^= reply.FieldsPresent
	reply.MissingFrames &^= reply.FramesPresent
	reply.Found = reply.MissingFields == 0 && reply.MissingFrames == 0
	return reply, reply.Found
}

func cloneReply(reply SHAReplyMsg) SHAReplyMsg {
	reply.PDQ = cloneBytes(reply.PDQ)
	reply.DurationMS = cloneInt64(reply.DurationMS)
	reply.ThumbPDQ = cloneBytes(reply.ThumbPDQ)
	reply.ThumbQuality = cloneInt32(reply.ThumbQuality)
	reply.VideoContainer, reply.VideoStreams = cloneVideoMetadata(
		reply.VideoContainer, reply.VideoStreams,
	)
	for index := range reply.FrameResults {
		reply.FrameResults[index].PDQ256 = cloneBytes(reply.FrameResults[index].PDQ256)
		reply.FrameResults[index].PHashParts = cloneBytes(reply.FrameResults[index].PHashParts)
		reply.FrameResults[index].SobelHist = cloneBytes(reply.FrameResults[index].SobelHist)
	}
	return reply
}

func cloneVideoMetadata(
	container *proto.VideoContainerMetadata,
	streams []proto.VideoStreamMetadata,
) (*proto.VideoContainerMetadata, []proto.VideoStreamMetadata) {
	return cloneVideoContainer(container), cloneVideoStreams(streams)
}

func cloneVideoContainer(value *proto.VideoContainerMetadata) *proto.VideoContainerMetadata {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.StartTimeUS = cloneInt64(value.StartTimeUS)
	cloned.DurationUS = cloneInt64(value.DurationUS)
	cloned.BitRate = cloneInt64(value.BitRate)
	cloned.FileSize = cloneInt64(value.FileSize)
	cloned.ProbeScore = cloneInt32(value.ProbeScore)
	cloned.PrimaryVideoStream = cloneInt32(value.PrimaryVideoStream)
	return &cloned
}

func cloneVideoStreams(values []proto.VideoStreamMetadata) []proto.VideoStreamMetadata {
	if values == nil {
		return nil
	}
	cloned := append([]proto.VideoStreamMetadata(nil), values...)
	for index := range cloned {
		value := &cloned[index]
		value.Level = cloneInt32(value.Level)
		value.StartTimeUS = cloneInt64(value.StartTimeUS)
		value.DurationUS = cloneInt64(value.DurationUS)
		value.BitRate = cloneInt64(value.BitRate)
		value.FrameCount = cloneInt64(value.FrameCount)
		value.BitDepth = cloneInt32(value.BitDepth)
		value.Width = cloneInt32(value.Width)
		value.Height = cloneInt32(value.Height)
		value.Rotation = cloneInt32(value.Rotation)
		value.SampleRate = cloneInt32(value.SampleRate)
		value.Channels = cloneInt32(value.Channels)
		value.AudioBitDepth = cloneInt32(value.AudioBitDepth)
	}
	return cloned
}

func replyForJob(jobID int64, reply SHAReplyMsg) SHAReplyMsg {
	reply = cloneReply(reply)
	reply.JobID = jobID
	return reply
}

func cloneBytes(value []byte) []byte {
	return append([]byte(nil), value...)
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneInt32(value *int32) *int32 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
