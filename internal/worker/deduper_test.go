package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"dedup/internal/store"
)

type lookupStub struct {
	mu         sync.Mutex
	imageCalls int
	videoCalls int
	image      func(context.Context, []byte) (*store.ImageFeature, error)
	video      func(context.Context, []byte) (*store.VideoFeature, error)
}

type contentLookupStub struct {
	*lookupStub
	content func(context.Context, []byte, store.MediaKind, uint32, uint8) (store.ContentState, error)
}

func (s *contentLookupStub) LookupContent(
	ctx context.Context,
	sha []byte,
	kind store.MediaKind,
	requestedFields uint32,
	requestedFrames uint8,
) (store.ContentState, error) {
	return s.content(ctx, sha, kind, requestedFields, requestedFrames)
}

func (s *lookupStub) LookupImage(ctx context.Context, sha []byte) (*store.ImageFeature, error) {
	s.mu.Lock()
	s.imageCalls++
	fn := s.image
	s.mu.Unlock()
	return fn(ctx, sha)
}

func (s *lookupStub) LookupVideo(ctx context.Context, sha []byte) (*store.VideoFeature, error) {
	s.mu.Lock()
	s.videoCalls++
	fn := s.video
	s.mu.Unlock()
	return fn(ctx, sha)
}

func (s *lookupStub) calls() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.imageCalls, s.videoCalls
}

type askResult struct {
	reply SHAReplyMsg
	err   error
}

func TestDeduperRepliesUseCallerJobID(t *testing.T) {
	sha := bytes64(19)
	d := NewDeduper(missLookup())
	if owner, err := d.Ask(context.Background(), SHAQueryMsg{JobID: 1000, SHA512: sha, Kind: MediaImage}); err != nil || owner.Found || owner.JobID != 1000 {
		t.Fatalf("owner Ask = (%+v, %v), want caller job ID 1000 miss", owner, err)
	}

	const waiters = 49
	type waiterResult struct {
		jobID int64
		askResult
	}
	results := make(chan waiterResult, waiters)
	for i := 0; i < waiters; i++ {
		go func(jobID int64) {
			reply, err := d.Ask(context.Background(), SHAQueryMsg{JobID: jobID, SHA512: sha, Kind: MediaImage})
			results <- waiterResult{jobID: jobID, askResult: askResult{reply: reply, err: err}}
		}(int64(1001 + i))
	}
	waitFor(t, "job-ID waiters", func() bool { return d.waiterCount(MediaImage, sha) == waiters })
	d.Resolve(JobResultMsg{JobID: 1000, Kind: MediaImage, SHA512: sha, FieldsDone: MaskAllImage, PDQ: []byte{1}})
	for range waiters {
		result := <-results
		if result.err != nil || !result.reply.Found || result.reply.JobID != result.jobID {
			t.Fatalf("caller %d got (%+v, %v), want found reply with matching job ID", result.jobID, result.reply, result.err)
		}
	}
}

func TestDeduperSingleFlight(t *testing.T) {
	sha := bytes64(20)
	lookup := missLookup()
	d := NewDeduper(lookup)
	owner, err := d.Ask(context.Background(), SHAQueryMsg{JobID: 100, SHA512: sha, Kind: MediaImage})
	if err != nil || owner.Found {
		t.Fatalf("owner Ask = (%+v, %v), want miss owner", owner, err)
	}

	const waiters = 49
	results := make(chan askResult, waiters)
	for i := 0; i < waiters; i++ {
		go func(jobID int64) {
			reply, err := d.Ask(context.Background(), SHAQueryMsg{JobID: jobID, SHA512: sha, Kind: MediaImage})
			results <- askResult{reply, err}
		}(int64(101 + i))
	}
	waitFor(t, "49 blocked waiters", func() bool { return d.waiterCount(MediaImage, sha) == waiters })
	if imageCalls, _ := lookup.calls(); imageCalls != 1 {
		t.Fatalf("image lookups = %d, want one", imageCalls)
	}

	d.Resolve(JobResultMsg{JobID: 100, Kind: MediaImage, SHA512: sha, FieldsDone: MaskAllImage, PDQ: []byte{1, 2, 3}, Quality: 88, Width: 800, Height: 600})
	all := make([]SHAReplyMsg, 0, waiters)
	for range waiters {
		result := <-results
		if result.err != nil || !result.reply.Found {
			t.Fatalf("waiter Ask = (%+v, %v), want found reply", result.reply, result.err)
		}
		all = append(all, result.reply)
	}
	all[0].PDQ[0] = 99
	if all[1].PDQ[0] != 1 {
		t.Fatalf("reply bytes were shared: second PDQ = %v", all[1].PDQ)
	}
}

func TestDeduperReusesLateComputedResultOnlyWithinTask(t *testing.T) {
	sha := bytes64(21)
	d := NewDeduper(missLookup())
	owner, err := d.Ask(context.Background(), SHAQueryMsg{
		JobID: 100, ScanTaskID: "task-one", SHA512: sha, Kind: MediaImage,
	})
	if err != nil || owner.Found {
		t.Fatalf("owner Ask = (%+v, %v), want miss", owner, err)
	}
	d.Resolve(JobResultMsg{
		JobID: 100, ScanTaskID: "task-one", Kind: MediaImage,
		SHA512: sha, FieldsDone: MaskAllImage, PDQ: []byte{1, 2, 3},
	})
	late, err := d.Ask(context.Background(), SHAQueryMsg{
		JobID: 101, ScanTaskID: "task-one", SHA512: sha, Kind: MediaImage,
	})
	if err != nil || !late.Found || !late.ReusedFlight {
		t.Fatalf("late same-task Ask = (%+v, %v), want computed-flight reuse", late, err)
	}
	d.EndTask("task-one")
	next, err := d.Ask(context.Background(), SHAQueryMsg{
		JobID: 102, ScanTaskID: "task-two", SHA512: sha, Kind: MediaImage,
	})
	if err != nil || next.Found || next.ReusedFlight {
		t.Fatalf("next-task Ask = (%+v, %v), want fresh lookup miss", next, err)
	}
}

func TestDeduperStoreHit(t *testing.T) {
	sha := bytes64(30)
	duration := int64(9100)
	quality := int32(72)
	width, height := int32(960), int32(540)
	lookup := &lookupStub{
		image: func(context.Context, []byte) (*store.ImageFeature, error) {
			return &store.ImageFeature{SHA512: sha, PDQ: []byte{3, 4}, Quality: 70, Width: 640, Height: 480}, nil
		},
		video: func(context.Context, []byte) (*store.VideoFeature, error) {
			return &store.VideoFeature{
				SHA512: sha, DurationMS: &duration, ThumbPath: "thumb.jpg",
				ThumbPDQ: bytes.Repeat([]byte{5}, 32), ThumbQuality: &quality,
				ThumbWidth: &width, ThumbHeight: &height,
			}, nil
		},
	}
	d := NewDeduper(lookup)
	image, err := d.Ask(context.Background(), SHAQueryMsg{JobID: 201, SHA512: sha, Kind: MediaImage})
	if err != nil || !image.Found || image.ReusedFlight || image.Width != 640 || image.Height != 480 || image.Quality != 70 || fmt.Sprint(image.PDQ) != "[3 4]" {
		t.Fatalf("image store hit = (%+v, %v)", image, err)
	}
	video, err := d.Ask(context.Background(), SHAQueryMsg{JobID: 202, SHA512: sha, Kind: MediaVideo})
	if err != nil || !video.Found || video.DurationMS == nil || *video.DurationMS != 9100 || video.ThumbPath != "thumb.jpg" || video.ThumbQuality == nil || *video.ThumbQuality != 72 || len(video.ThumbPDQ) != 32 || video.ThumbPDQ[0] != 5 {
		t.Fatalf("video store hit = (%+v, %v)", video, err)
	}
	if d.flightCount() != 0 {
		t.Fatalf("store hit left %d flights, want none", d.flightCount())
	}
}

func TestVideoBaseFeaturesLegacyLookupAdapterSeparatesLegacyThumbAndContact(t *testing.T) {
	sha := bytes64(0x3a)
	duration, quality := int64(9100), int32(72)
	width, height := int32(960), int32(540)
	tests := []struct {
		name        string
		requested   uint32
		withDims    bool
		wantPresent uint32
		wantMissing uint32
		wantFound   bool
	}{
		{
			name: "legacy thumbnail without dimensions", requested: MaskSHA512 | MaskVideoThumb,
			wantPresent: MaskSHA512 | MaskVideoThumb, wantFound: true,
		},
		{
			name: "new contact without dimensions", requested: MaskSHA512 | MaskVideoDuration | MaskVideoContactSheet,
			wantPresent: MaskSHA512 | MaskVideoDuration, wantMissing: MaskVideoContactSheet,
		},
		{
			name: "new contact with dimensions", requested: MaskSHA512 | MaskVideoDuration | MaskVideoContactSheet,
			withDims: true, wantPresent: MaskSHA512 | MaskVideoDuration | MaskVideoContactSheet, wantFound: true,
		},
	}
	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			feature := &store.VideoFeature{
				SHA512: sha, DurationMS: &duration, ThumbPath: "thumb.jpg",
				ThumbPDQ: bytes.Repeat([]byte{5}, 32), ThumbQuality: &quality,
			}
			if tt.withDims {
				feature.ThumbWidth, feature.ThumbHeight = &width, &height
			}
			d := NewDeduper(&lookupStub{
				image: func(context.Context, []byte) (*store.ImageFeature, error) { return nil, nil },
				video: func(context.Context, []byte) (*store.VideoFeature, error) { return feature, nil },
			})
			reply, err := d.Ask(context.Background(), SHAQueryMsg{
				JobID: int64(203 + index), SHA512: sha, Kind: MediaVideo,
				RequestedFields: tt.requested,
			})
			if err != nil {
				t.Fatalf("Ask: %v", err)
			}
			if reply.Found != tt.wantFound || reply.FieldsPresent != tt.wantPresent ||
				reply.MissingFields != tt.wantMissing {
				t.Fatalf("legacy adapter masks = found:%t present:%#x missing:%#x, want %t/%#x/%#x",
					reply.Found, reply.FieldsPresent, reply.MissingFields,
					tt.wantFound, tt.wantPresent, tt.wantMissing)
			}
		})
	}
}

func TestDefaultStageOneDeduperDoesNotDependOnMutableMaskAliases(t *testing.T) {
	originalImage, originalVideo := MaskAllImage, MaskAllVideo
	MaskAllImage, MaskAllVideo = 0, MaskVideoThumb
	t.Cleanup(func() { MaskAllImage, MaskAllVideo = originalImage, originalVideo })
	image, err := normalizeSHAQuery(SHAQueryMsg{Kind: MediaImage})
	if err != nil {
		t.Fatal(err)
	}
	video, err := normalizeSHAQuery(SHAQueryMsg{Kind: MediaVideo})
	if err != nil {
		t.Fatal(err)
	}
	if image.RequestedFields != store.RequiredStageOneMask(store.MediaImage) ||
		video.RequestedFields != store.RequiredStageOneMask(store.MediaVideo) {
		t.Fatalf("deduper defaults = image:%#x video:%#x",
			image.RequestedFields, video.RequestedFields)
	}
}

func TestDeduperMarksOnlyActiveFlightWaitersAsSingleFlightReuse(t *testing.T) {
	sha := bytes64(32)
	d := NewDeduper(missLookup())
	owner, err := d.Ask(context.Background(), SHAQueryMsg{
		JobID: 301, SHA512: sha, Kind: MediaImage,
	})
	if err != nil || owner.Found || owner.ReusedFlight {
		t.Fatalf("owner = (%#v, %v)", owner, err)
	}
	waiterDone := make(chan askResult, 1)
	go func() {
		reply, askErr := d.Ask(context.Background(), SHAQueryMsg{
			JobID: 302, SHA512: sha, Kind: MediaImage,
		})
		waiterDone <- askResult{reply: reply, err: askErr}
	}()
	waitFor(t, "single-flight source waiter", func() bool {
		return d.waiterCount(MediaImage, sha) == 1
	})
	d.Resolve(JobResultMsg{
		JobID: 301, Kind: MediaImage, SHA512: sha,
		FieldsDone: MaskAllImage, PDQ: []byte{1},
	})
	waiter := <-waiterDone
	if waiter.err != nil || !waiter.reply.Found || !waiter.reply.ReusedFlight {
		t.Fatalf("waiter = (%#v, %v), want active-flight reuse", waiter.reply, waiter.err)
	}
}

func TestDeduperComputedVideoStageReplyKeepsFixedFramePayload(t *testing.T) {
	sha := bytes64(34)
	d := NewDeduper(missLookup())
	query := SHAQueryMsg{
		JobID: 341, ScanTaskID: "stage-two", SHA512: sha, Kind: MediaVideo,
		RequestedFields: MaskVideo6FPHash, RequestedFrames: FrameMaskFull,
	}
	owner, err := d.Ask(context.Background(), query)
	if err != nil || owner.Found {
		t.Fatalf("owner = (%#v, %v)", owner, err)
	}
	frames := [6]FrameResult{}
	for index := range frames {
		frames[index] = FrameResult{FrameIdx: index, PHashParts: []byte{byte(index + 1)}}
	}
	d.Resolve(JobResultMsg{
		JobID: query.JobID, ScanTaskID: query.ScanTaskID, Kind: MediaVideo, SHA512: sha,
		FieldsDone: MaskVideo6FPHash, FramesDone: FrameMaskFull, FrameResults: frames,
	})

	query.JobID++
	reused, err := d.Ask(context.Background(), query)
	if err != nil || !reused.Found || !reused.ReusedFlight {
		t.Fatalf("reused = (%#v, %v)", reused, err)
	}
	for index, frame := range reused.FrameResults {
		if frame.FrameIdx != index || !bytes.Equal(frame.PHashParts, []byte{byte(index + 1)}) {
			t.Fatalf("frame %d = %#v, want fixed pHash payload", index, frame)
		}
	}
}

func TestDeduperConcurrentWaiterOnBlockedPersistentStoreHitIsNotSingleFlightReuse(t *testing.T) {
	sha := bytes64(33)
	lookupEntered := make(chan struct{})
	releaseLookup := make(chan struct{})
	lookup := &lookupStub{
		image: func(context.Context, []byte) (*store.ImageFeature, error) {
			close(lookupEntered)
			<-releaseLookup
			return &store.ImageFeature{
				SHA512: sha, PDQ: bytes.Repeat([]byte{0x77}, 32),
				Quality: 80, Width: 20, Height: 10,
			}, nil
		},
		video: func(context.Context, []byte) (*store.VideoFeature, error) {
			return nil, nil
		},
	}
	d := NewDeduper(lookup)
	ownerDone := make(chan askResult, 1)
	go func() {
		reply, err := d.Ask(context.Background(), SHAQueryMsg{
			JobID: 311, SHA512: sha, Kind: MediaImage,
		})
		ownerDone <- askResult{reply: reply, err: err}
	}()
	<-lookupEntered
	waiterDone := make(chan askResult, 1)
	go func() {
		reply, err := d.Ask(context.Background(), SHAQueryMsg{
			JobID: 312, SHA512: sha, Kind: MediaImage,
		})
		waiterDone <- askResult{reply: reply, err: err}
	}()
	waitFor(t, "blocked store-hit waiter", func() bool {
		return d.waiterCount(MediaImage, sha) == 1
	})
	close(releaseLookup)
	for _, result := range []askResult{<-ownerDone, <-waiterDone} {
		if result.err != nil || !result.reply.Found || result.reply.ReusedFlight {
			t.Fatalf("persistent store hit = (%#v, %v), want found without reuse", result.reply, result.err)
		}
	}
}

func TestDeduperPartialVideoStoreHitBecomesOwner(t *testing.T) {
	sha := bytes64(31)
	duration := int64(42)
	quality := int32(73)
	width, height := int32(960), int32(540)
	complete := func() *store.VideoFeature {
		return &store.VideoFeature{
			SHA512: sha, DurationMS: &duration, ThumbPath: "thumb.jpg",
			ThumbPDQ: bytes.Repeat([]byte{1}, 32), ThumbQuality: &quality,
			ThumbWidth: &width, ThumbHeight: &height,
		}
	}
	cases := []struct {
		name  string
		edit  func(*store.VideoFeature)
		found bool
	}{
		{"missing duration", func(feature *store.VideoFeature) { feature.DurationMS = nil }, false},
		{"missing thumbnail path", func(feature *store.VideoFeature) { feature.ThumbPath = "" }, false},
		{"missing thumbnail PDQ", func(feature *store.VideoFeature) { feature.ThumbPDQ = nil }, false},
		{"missing thumbnail quality", func(feature *store.VideoFeature) { feature.ThumbQuality = nil }, false},
		{"missing contact width", func(feature *store.VideoFeature) { feature.ThumbWidth = nil }, false},
		{"missing contact height", func(feature *store.VideoFeature) { feature.ThumbHeight = nil }, false},
		{"complete bundle", func(*store.VideoFeature) {}, true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			feature := complete()
			tc.edit(feature)
			d := NewDeduper(&lookupStub{
				image: func(context.Context, []byte) (*store.ImageFeature, error) { return nil, nil },
				video: func(context.Context, []byte) (*store.VideoFeature, error) { return feature, nil },
			})
			jobID := int64(220 + i)
			reply, err := d.Ask(context.Background(), SHAQueryMsg{JobID: jobID, SHA512: sha, Kind: MediaVideo})
			if err != nil || reply.Found != tc.found || reply.JobID != jobID {
				t.Fatalf("video store Ask = (%+v, %v), want found=%t and caller job ID", reply, err, tc.found)
			}
			if tc.found && d.flightCount() != 0 {
				t.Fatalf("complete video hit left %d flights", d.flightCount())
			}
			if !tc.found && d.flightCount() != 1 {
				t.Fatalf("partial video hit left %d flights, want owner flight", d.flightCount())
			}
		})
	}
}

func TestDeduperPartialVideoReturnsExactMasks(t *testing.T) {
	sha := bytes64(34)
	duration := int64(4200)
	lookup := &contentLookupStub{
		lookupStub: missLookup(),
		content: func(
			_ context.Context,
			gotSHA []byte,
			kind store.MediaKind,
			requestedFields uint32,
			requestedFrames uint8,
		) (store.ContentState, error) {
			if !bytes.Equal(gotSHA, sha) || kind != store.MediaVideo {
				t.Fatalf("LookupContent key = (%x, %q), want video SHA", gotSHA, kind)
			}
			wantFields := MaskVideoDuration | MaskVideoContactSheet | MaskVideo6F
			if requestedFields != wantFields || requestedFrames != FrameMaskFull {
				t.Fatalf("LookupContent request = %#x/%#x, want %#x/%#x",
					requestedFields, requestedFrames, wantFields, FrameMaskFull)
			}
			return store.ContentState{
				SHA512:        cloneBytes(sha),
				FieldsPresent: MaskVideoDuration,
				MissingFields: MaskVideoContactSheet | MaskVideo6F,
				FramesPresent: 0x1f,
				MissingFrames: 0x20,
				Video:         &store.VideoFeature{SHA512: cloneBytes(sha), DurationMS: &duration},
			}, nil
		},
	}
	d := NewDeduper(lookup)
	query := SHAQueryMsg{
		JobID:           230,
		ScanTaskID:      "partial-video",
		SHA512:          sha,
		Kind:            MediaVideo,
		RequestedFields: MaskVideoDuration | MaskVideoContactSheet | MaskVideo6F,
		RequestedFrames: FrameMaskFull,
	}
	reply, err := d.Ask(context.Background(), query)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if reply.Found {
		t.Fatalf("partial reply Found = true: %#v", reply)
	}
	if reply.RequestedFields != query.RequestedFields ||
		reply.FieldsPresent != MaskVideoDuration ||
		reply.MissingFields != MaskVideoContactSheet|MaskVideo6F ||
		reply.RequestedFrames != FrameMaskFull ||
		reply.FramesPresent != 0x1f || reply.MissingFrames != 0x20 {
		t.Fatalf("partial reply masks = %#v", reply)
	}
	if reply.DurationMS == nil || *reply.DurationMS != duration {
		t.Fatalf("partial reply duration = %#v, want %d", reply.DurationMS, duration)
	}
	if err := reply.ValidateMasks(); err != nil {
		t.Fatalf("partial reply masks invalid: %v", err)
	}
	if d.flightCount() != 1 {
		t.Fatalf("partial reply flights = %d, want owner retained", d.flightCount())
	}
}

func TestDeduperRequestKeyIsolationUsesNormalizedFieldsAndFrames(t *testing.T) {
	sha := bytes64(35)
	lookup := &contentLookupStub{
		lookupStub: missLookup(),
		content: func(
			_ context.Context,
			_ []byte,
			_ store.MediaKind,
			requestedFields uint32,
			requestedFrames uint8,
		) (store.ContentState, error) {
			return store.ContentState{
				SHA512:        cloneBytes(sha),
				MissingFields: requestedFields,
				MissingFrames: requestedFrames,
			}, nil
		},
	}
	d := NewDeduper(lookup)
	first, err := d.Ask(context.Background(), SHAQueryMsg{
		JobID: 240, ScanTaskID: "request-keys", SHA512: sha, Kind: MediaVideo,
		RequestedFields: MaskVideoDuration,
	})
	if err != nil || first.Found {
		t.Fatalf("first Ask = (%#v, %v), want duration miss owner", first, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	second, err := d.Ask(ctx, SHAQueryMsg{
		JobID: 241, ScanTaskID: "request-keys", SHA512: sha, Kind: MediaVideo,
		RequestedFields: MaskVideo6F,
		RequestedFrames: 0x03,
	})
	if err != nil || second.Found {
		t.Fatalf("second Ask = (%#v, %v), want independent frame miss owner", second, err)
	}
	if second.RequestedFields != MaskVideo6F || second.MissingFields != MaskVideo6F ||
		second.RequestedFrames != 0x03 || second.MissingFrames != 0x03 {
		t.Fatalf("second request masks = %#v", second)
	}
	if d.flightCount() != 2 {
		t.Fatalf("isolated request flights = %d, want two", d.flightCount())
	}
}

func TestDeduperOwnerCrashRetry(t *testing.T) {
	sha := bytes64(40)
	d := NewDeduper(missLookup())
	owner, err := d.Ask(context.Background(), SHAQueryMsg{JobID: 300, SHA512: sha, Kind: MediaVideo})
	if err != nil || owner.Found {
		t.Fatalf("initial owner Ask = (%+v, %v)", owner, err)
	}

	const waiters = 49
	results := make(chan askResult, waiters)
	for i := 0; i < waiters; i++ {
		go func(jobID int64) {
			reply, err := d.Ask(context.Background(), SHAQueryMsg{JobID: jobID, SHA512: sha, Kind: MediaVideo})
			results <- askResult{reply, err}
		}(int64(301 + i))
	}
	waitFor(t, "crash waiters", func() bool { return d.waiterCount(MediaVideo, sha) == waiters })
	d.FailByJob(300)

	newOwner := <-results
	if newOwner.err != nil || newOwner.reply.Found {
		t.Fatalf("retry owner Ask = (%+v, %v), want miss owner", newOwner.reply, newOwner.err)
	}
	waitFor(t, "retry waiters", func() bool { return d.waiterCount(MediaVideo, sha) == waiters-1 })
	flightOwner, flightWaiters, exists := d.flightState(MediaVideo, sha)
	if !exists || flightOwner != newOwner.reply.JobID || flightWaiters != waiters-1 {
		t.Fatalf("retry flight = (owner=%d, waiters=%d, exists=%t), want owner=%d and %d waiters", flightOwner, flightWaiters, exists, newOwner.reply.JobID, waiters-1)
	}
	duration := int64(1000)
	quality := int32(80)
	d.Resolve(JobResultMsg{JobID: newOwner.reply.JobID, Kind: MediaVideo, SHA512: sha, FieldsDone: MaskAllVideo, DurationMS: &duration, ThumbPath: "retry.jpg", ThumbPDQ: []byte{7, 8}, ThumbQuality: &quality})
	for i := 1; i < waiters; i++ {
		result := <-results
		if result.err != nil || !result.reply.Found {
			t.Fatalf("retry waiter Ask = (%+v, %v), want found", result.reply, result.err)
		}
	}
}

func TestDeduperRejectsInvalidInput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		d     *Deduper
		query SHAQueryMsg
	}{
		{"nil lookup", NewDeduper(nil), SHAQueryMsg{JobID: 1, SHA512: bytes64(1), Kind: MediaImage}},
		{"invalid kind", NewDeduper(missLookup()), SHAQueryMsg{JobID: 1, SHA512: bytes64(1), Kind: MediaKind(99)}},
		{"short SHA", NewDeduper(missLookup()), SHAQueryMsg{JobID: 1, SHA512: []byte{1}, Kind: MediaImage}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.d.Ask(context.Background(), tc.query); err == nil {
				t.Fatal("Ask unexpectedly succeeded")
			}
		})
	}
}

func TestDeduperStoreErrorReleasesWaiters(t *testing.T) {
	sha := bytes64(50)
	release := make(chan struct{})
	storeErr := errors.New("database unavailable")
	var lookupCount int
	var lookupCountMu sync.Mutex
	lookup := &lookupStub{image: func(context.Context, []byte) (*store.ImageFeature, error) {
		lookupCountMu.Lock()
		lookupCount++
		first := lookupCount == 1
		lookupCountMu.Unlock()
		if first {
			<-release
			return nil, storeErr
		}
		return nil, nil
	}, video: func(context.Context, []byte) (*store.VideoFeature, error) { return nil, nil }}
	d := NewDeduper(lookup)
	ownerResults := make(chan askResult, 1)
	go func() {
		reply, err := d.Ask(context.Background(), SHAQueryMsg{JobID: 400, SHA512: sha, Kind: MediaImage})
		ownerResults <- askResult{reply, err}
	}()
	waitFor(t, "store-error owner flight", func() bool { return d.flightCount() == 1 })

	const waiters = 3
	results := make(chan askResult, waiters)
	for i := 0; i < waiters; i++ {
		go func(jobID int64) {
			reply, err := d.Ask(context.Background(), SHAQueryMsg{JobID: jobID, SHA512: sha, Kind: MediaImage})
			results <- askResult{reply, err}
		}(int64(401 + i))
	}
	waitFor(t, "store-error waiters", func() bool { return d.waiterCount(MediaImage, sha) == waiters })
	close(release)
	owner := <-ownerResults
	if !errors.Is(owner.err, storeErr) {
		t.Fatalf("owner error = %v, want %v", owner.err, storeErr)
	}
	newOwner := <-results
	if newOwner.err != nil || newOwner.reply.Found {
		t.Fatalf("released retry owner = (%+v, %v)", newOwner.reply, newOwner.err)
	}
	waitFor(t, "released retry waiters", func() bool { return d.waiterCount(MediaImage, sha) == waiters-1 })
	d.Resolve(JobResultMsg{JobID: newOwner.reply.JobID, Kind: MediaImage, SHA512: sha, FieldsDone: MaskAllImage, PDQ: []byte{1}})
	for i := 1; i < waiters; i++ {
		result := <-results
		if result.err != nil || !result.reply.Found {
			t.Fatalf("released waiter = (%+v, %v)", result.reply, result.err)
		}
	}
}

func TestDeduperWaitCancellation(t *testing.T) {
	sha := bytes64(60)
	d := NewDeduper(missLookup())
	if owner, err := d.Ask(context.Background(), SHAQueryMsg{JobID: 500, SHA512: sha, Kind: MediaImage}); err != nil || owner.Found {
		t.Fatalf("owner Ask = (%+v, %v)", owner, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan askResult, 1)
	go func() {
		reply, err := d.Ask(ctx, SHAQueryMsg{JobID: 501, SHA512: sha, Kind: MediaImage})
		result <- askResult{reply, err}
	}()
	waitFor(t, "cancellable waiter", func() bool { return d.waiterCount(MediaImage, sha) == 1 })
	cancel()
	if got := <-result; !errors.Is(got.err, context.Canceled) {
		t.Fatalf("cancelled Ask error = %v, want context.Canceled", got.err)
	}
	waitFor(t, "removed cancelled waiter", func() bool { return d.waiterCount(MediaImage, sha) == 0 })
	d.Resolve(JobResultMsg{JobID: 500, Kind: MediaImage, SHA512: sha, FieldsDone: MaskAllImage, PDQ: []byte{2}})
}

func TestDeduperCancellationWinsReleasedFlight(t *testing.T) {
	sha := bytes64(61)
	d := NewDeduper(missLookup())
	if owner, err := d.Ask(context.Background(), SHAQueryMsg{JobID: 510, SHA512: sha, Kind: MediaImage}); err != nil || owner.Found {
		t.Fatalf("owner Ask = (%+v, %v)", owner, err)
	}
	ctx := &gatedCancelContext{done: make(chan struct{}), entered: make(chan struct{}, 1), permit: make(chan struct{})}
	results := make(chan askResult, 1)
	go func() {
		reply, err := d.Ask(ctx, SHAQueryMsg{JobID: 511, SHA512: sha, Kind: MediaImage})
		results <- askResult{reply: reply, err: err}
	}()
	waitFor(t, "released cancellable waiter", func() bool { return d.waiterCount(MediaImage, sha) == 1 })
	<-ctx.entered
	close(ctx.done)
	d.FailByJob(510)
	close(ctx.permit)

	got := <-results
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("released cancelled Ask = (%+v, %v), want context.Canceled", got.reply, got.err)
	}
	owner, _, exists := d.flightState(MediaImage, sha)
	if exists && owner == 511 {
		t.Fatal("cancelled waiter became retry owner")
	}
}

func TestDeduperPreCancelledAskDoesNotBecomeOwner(t *testing.T) {
	sha := bytes64(62)
	d := NewDeduper(missLookup())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reply, err := d.Ask(ctx, SHAQueryMsg{JobID: 512, SHA512: sha, Kind: MediaImage})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled Ask = (%+v, %v), want context.Canceled", reply, err)
	}
	if d.flightCount() != 0 {
		t.Fatalf("pre-cancelled Ask left %d flights", d.flightCount())
	}
}

func TestDeduperIgnoresForeignResolve(t *testing.T) {
	sha := bytes64(70)
	d := NewDeduper(missLookup())
	if owner, err := d.Ask(context.Background(), SHAQueryMsg{JobID: 600, SHA512: sha, Kind: MediaImage}); err != nil || owner.Found {
		t.Fatalf("owner Ask = (%+v, %v)", owner, err)
	}
	results := make(chan askResult, 1)
	go func() {
		reply, err := d.Ask(context.Background(), SHAQueryMsg{JobID: 601, SHA512: sha, Kind: MediaImage})
		results <- askResult{reply, err}
	}()
	waitFor(t, "foreign-resolve waiter", func() bool { return d.waiterCount(MediaImage, sha) == 1 })
	d.Resolve(JobResultMsg{JobID: 999, Kind: MediaImage, SHA512: sha, FieldsDone: MaskAllImage, PDQ: []byte{3}})
	flightOwner, flightWaiters, exists := d.flightState(MediaImage, sha)
	if !exists || flightOwner != 600 || flightWaiters != 1 {
		t.Fatalf("foreign Resolve changed flight to (owner=%d, waiters=%d, exists=%t), want owner=600 and one waiter", flightOwner, flightWaiters, exists)
	}
	d.Resolve(JobResultMsg{JobID: 600, Kind: MediaImage, SHA512: sha, FieldsDone: MaskAllImage, PDQ: []byte{3}})
	result := <-results
	if result.err != nil || !result.reply.Found {
		t.Fatalf("owner Resolve result = (%+v, %v)", result.reply, result.err)
	}
}

func missLookup() *lookupStub {
	return &lookupStub{
		image: func(context.Context, []byte) (*store.ImageFeature, error) { return nil, nil },
		video: func(context.Context, []byte) (*store.VideoFeature, error) { return nil, nil },
	}
}

func (d *Deduper) waiterCount(kind MediaKind, sha []byte) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	waiters := 0
	keySHA := shaKey(sha)
	for key, flight := range d.flights {
		if key.kind == kind && key.sha == keySHA {
			waiters += flight.waiters
		}
	}
	return waiters
}

func (d *Deduper) flightCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.flights)
}

func (d *Deduper) flightState(kind MediaKind, sha []byte) (int64, int, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	keySHA := shaKey(sha)
	for key, flight := range d.flights {
		if key.kind == kind && key.sha == keySHA {
			return flight.owner, flight.waiters, true
		}
	}
	return 0, 0, false
}

func waitFor(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

type gatedCancelContext struct {
	done    chan struct{}
	entered chan struct{}
	permit  chan struct{}
}

func (c *gatedCancelContext) Deadline() (time.Time, bool) { return time.Time{}, false }

func (c *gatedCancelContext) Done() <-chan struct{} {
	c.entered <- struct{}{}
	<-c.permit
	return c.done
}

func (c *gatedCancelContext) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

func (c *gatedCancelContext) Value(any) any { return nil }
