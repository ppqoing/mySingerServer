package phase2

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"dedup/internal/config"
	"dedup/internal/features"
	"dedup/internal/proto"
)

func TestRescreenerMergesOutOfOrderPartialImageAndPersistsBeforeResolvingOnce(t *testing.T) {
	shaA, shaB := rescreenSHA('a'), rescreenSHA('b')
	store := newFakeRescreenStore(reconcileSnapshot{
		Pairs: []candidatePair{testCandidate("image", shaB, shaA, 7)},
	})
	rescreener := newRescreener(store, rescreenConfig(), nil)
	if err := rescreener.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}

	callbacks := 0
	rescreener.SetOnPairResolved(func(key PairKey, verdict Verdict) {
		callbacks++
		if key != (PairKey{Kind: "image", SHAA: shaA, SHAB: shaB}) {
			t.Fatalf("callback key = %#v", key)
		}
		if verdict != VerdictYes {
			t.Fatalf("callback verdict = %v", verdict)
		}
		if len(store.scores) != 1 {
			t.Fatal("callback fired before durable upsert")
		}
	})

	partsBlob, sobelBlob := rescreenFeatureBlobs(t, 1)
	deliveries := []*BoundFeatureResult{
		rescreenResult(imageWireItem(shaB, proto.FieldSobelHist, nil, sobelBlob)),
		rescreenResult(imageWireItem(shaA, proto.FieldPHashParts, partsBlob, nil)),
		rescreenResult(imageWireItem(shaB, proto.FieldPHashParts, partsBlob, nil)),
		rescreenResult(imageWireItem(shaB, proto.FieldPHashParts, partsBlob, nil)),
		rescreenResult(imageWireItem(shaA, proto.FieldSobelHist, nil, sobelBlob)),
	}
	for index, result := range deliveries {
		if err := rescreener.HandleFeatureResult(context.Background(), result); err != nil {
			t.Fatalf("delivery %d: %v", index, err)
		}
	}

	if callbacks != 1 || store.upserts != 1 {
		t.Fatalf("callbacks=%d upserts=%d, want one each", callbacks, store.upserts)
	}
	key := PairKey{Kind: "image", SHAA: shaA, SHAB: shaB}
	score := store.scores[key]
	if score.Key != key || score.Verdict != "yes" {
		t.Fatalf("persisted score = %#v", score)
	}
	if score.Document.Version != pairScoreVersion ||
		score.Document.Image == nil ||
		!score.Document.Image.SobelEvaluated ||
		score.Document.Image.SobelCosine == nil {
		t.Fatalf("image document = %#v", score.Document)
	}
	progress := rescreener.Progress()
	if progress.UnresolvedPairs != 0 || progress.CachedEndpoints != 0 ||
		progress.InFlight != 0 {
		t.Fatalf("resolved progress = %#v", progress)
	}
}

func TestValidateVideoScoreDocumentAcceptsNegativeFiniteAverage(t *testing.T) {
	average := -0.5
	document := &videoScoreDocument{
		ValidFrames:      4,
		AverageEvaluated: true,
		Average:          &average,
		Frames:           make([]frameScoreDocument, 6),
	}
	for index := range document.Frames {
		document.Frames[index].FrameIdx = index
		if index >= document.ValidFrames {
			continue
		}
		ratio, cosine, similarity := 1.0, -0.5, -0.5
		document.Frames[index].Valid = true
		document.Frames[index].PHashPassRatio = &ratio
		document.Frames[index].SobelEvaluated = true
		document.Frames[index].SobelCosine = &cosine
		document.Frames[index].Similarity = &similarity
	}
	if err := validateVideoScoreDocument("no", document); err != nil {
		t.Fatalf("negative but finite Task 8 average was rejected: %v", err)
	}
}

func TestRescreenerRejectsDivergentDuplicateWithoutReplacingFirstValue(t *testing.T) {
	shaA, shaB := rescreenSHA('a'), rescreenSHA('b')
	store := newFakeRescreenStore(reconcileSnapshot{
		Pairs: []candidatePair{testCandidate("image", shaA, shaB, 3)},
	})
	rescreener := newRescreener(store, rescreenConfig(), nil)
	if err := rescreener.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}

	firstParts, sobel := rescreenFeatureBlobs(t, 1)
	secondParts, _ := rescreenFeatureBlobs(t, 2)
	if err := rescreener.HandleFeatureResult(
		context.Background(),
		rescreenResult(imageWireItem(shaA, proto.FieldPHashParts, firstParts, nil)),
	); err != nil {
		t.Fatal(err)
	}
	if err := rescreener.HandleFeatureResult(
		context.Background(),
		rescreenResult(imageWireItem(shaA, proto.FieldPHashParts, secondParts, nil)),
	); err == nil {
		t.Fatal("divergent duplicate pHash was accepted")
	}

	for _, item := range []proto.FeatureItem{
		imageWireItem(shaA, proto.FieldSobelHist, nil, sobel),
		imageWireItem(
			shaB,
			proto.FieldPHashParts|proto.FieldSobelHist,
			firstParts,
			sobel,
		),
	} {
		if err := rescreener.HandleFeatureResult(
			context.Background(),
			rescreenResult(item),
		); err != nil {
			t.Fatal(err)
		}
	}
	if store.upserts != 1 {
		t.Fatalf("upserts=%d, want first cached value remained usable", store.upserts)
	}
}

func TestRescreenerRejectsMalformedRelevantWireWithoutMutatingCache(t *testing.T) {
	shaA, shaB := rescreenSHA('a'), rescreenSHA('b')
	validParts, validSobel := rescreenFeatureBlobs(t, 1)
	nonFinite := append([]byte(nil), validSobel...)
	nonFinite[4], nonFinite[5], nonFinite[6], nonFinite[7] = 0, 0, 0xc0, 0x7f
	validFrame := rescreenWireFrame(t, 0, 1)

	tests := []struct {
		name string
		item proto.FeatureItem
	}{
		{
			name: "success bit without payload",
			item: imageWireItem(shaA, proto.FieldPHashParts, nil, nil),
		},
		{
			name: "payload without success bit",
			item: imageWireItem(shaA, 0, validParts, nil),
		},
		{
			name: "corrupt pHash",
			item: imageWireItem(shaA, proto.FieldPHashParts, []byte{1}, nil),
		},
		{
			name: "nonfinite Sobel",
			item: imageWireItem(shaA, proto.FieldSobelHist, nil, nonFinite),
		},
		{
			name: "bad PDQ",
			item: proto.FeatureItem{
				SHA512: shaA, Status: proto.StatusPartial,
				Frames: []proto.FrameFeature{{
					FrameIdx: 0, PDQ256: []byte{1},
					PHashParts: validParts, SobelHist: validSobel,
				}},
			},
		},
		{
			name: "invalid frame index",
			item: proto.FeatureItem{
				SHA512: shaA, Status: proto.StatusPartial,
				Frames: []proto.FrameFeature{{
					FrameIdx: 6, PDQ256: bytes.Repeat([]byte{1}, 32),
					PHashParts: validParts, SobelHist: validSobel,
				}},
			},
		},
		{
			name: "duplicate frame index in batch",
			item: proto.FeatureItem{
				SHA512: shaA, Status: proto.StatusPartial,
				Frames: []proto.FrameFeature{validFrame, validFrame},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind := "image"
			if len(tt.item.Frames) > 0 {
				kind = "video"
			}
			store := newFakeRescreenStore(reconcileSnapshot{
				Pairs: []candidatePair{testCandidate(kind, shaA, shaB, 1)},
			})
			rescreener := newRescreener(store, rescreenConfig(), nil)
			if err := rescreener.Restore(context.Background()); err != nil {
				t.Fatal(err)
			}
			before := rescreener.Progress()
			if err := rescreener.HandleFeatureResult(
				context.Background(),
				rescreenResult(tt.item),
			); err == nil {
				t.Fatal("malformed relevant wire was accepted")
			}
			after := rescreener.Progress()
			if after != before || store.upserts != 0 {
				t.Fatalf("malformed wire mutated state: before=%#v after=%#v", before, after)
			}
		})
	}
}

func TestRescreenerValidatesEntireBoundBatchBeforeAtomicMerge(t *testing.T) {
	shaA, shaB := rescreenSHA('a'), rescreenSHA('b')
	store := newFakeRescreenStore(reconcileSnapshot{
		Pairs: []candidatePair{testCandidate("image", shaA, shaB, 1)},
	})
	rescreener := newRescreener(store, rescreenConfig(), nil)
	if err := rescreener.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	parts, _ := rescreenFeatureBlobs(t, 1)
	result := rescreenResult(
		imageWireItem(shaA, proto.FieldPHashParts, parts, nil),
		imageWireItem(shaB, proto.FieldSobelHist, nil, []byte{1}),
	)
	before := rescreener.Progress()
	if err := rescreener.HandleFeatureResult(context.Background(), result); err == nil {
		t.Fatal("batch with malformed second item was accepted")
	}
	after := rescreener.Progress()
	if after != before || store.upserts != 0 {
		t.Fatalf("batch partially merged before error: before=%#v after=%#v", before, after)
	}
}

func TestRescreenerMergesPartialVideoFramesByIndexAndJudgesAtMinimumValid(t *testing.T) {
	shaA, shaB := rescreenSHA('a'), rescreenSHA('b')
	store := newFakeRescreenStore(reconcileSnapshot{
		Pairs: []candidatePair{testCandidate("video", shaA, shaB, 2)},
	})
	rescreener := newRescreener(store, rescreenConfig(), nil)
	if err := rescreener.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, delivery := range []proto.FeatureItem{
		videoWireItem(t, shaB, 3, 1),
		videoWireItem(t, shaA, 2, 0),
		videoWireItem(t, shaB, 0, 2),
		videoWireItem(t, shaA, 3, 1),
		videoWireItem(t, shaB, 2),
	} {
		if err := rescreener.HandleFeatureResult(
			context.Background(),
			rescreenResult(delivery),
		); err != nil {
			t.Fatal(err)
		}
	}
	if store.upserts != 1 {
		t.Fatalf("video upserts=%d, want one at four paired valid frames", store.upserts)
	}
	score := store.onlyScore(t)
	if score.Verdict != "yes" || score.Document.Video == nil ||
		score.Document.Video.ValidFrames != 4 ||
		len(score.Document.Video.Frames) != 6 {
		t.Fatalf("video score = %#v", score)
	}
	for index, frame := range score.Document.Video.Frames {
		if frame.FrameIdx != index {
			t.Fatalf("frame[%d] index=%d", index, frame.FrameIdx)
		}
	}
}

func TestRescreenerDatabaseFailureAndOutcomeUnknownRemainRetryable(t *testing.T) {
	for _, commitOnError := range []bool{false, true} {
		t.Run(map[bool]string{false: "failure", true: "outcome unknown"}[commitOnError], func(t *testing.T) {
			shaA, shaB := rescreenSHA('a'), rescreenSHA('b')
			store := newFakeRescreenStore(reconcileSnapshot{
				Pairs:  []candidatePair{testCandidate("image", shaA, shaB, 1)},
				Images: readyImageCache(t, shaA, shaB),
			})
			store.upsertErr = errors.New("forced upsert error")
			store.commitOnError = commitOnError
			rescreener := newRescreener(store, rescreenConfig(), nil)
			callbacks := 0
			rescreener.SetOnPairResolved(func(PairKey, Verdict) { callbacks++ })

			if err := rescreener.Restore(context.Background()); err == nil {
				t.Fatal("Restore accepted forced upsert failure")
			}
			if callbacks != 0 || rescreener.Progress().UnresolvedPairs != 1 {
				t.Fatalf("false completion callbacks=%d progress=%#v", callbacks, rescreener.Progress())
			}
			if commitOnError && len(store.scores) != 1 {
				t.Fatal("outcome-unknown fake did not commit durable row")
			}
			store.upsertErr = nil
			if err := rescreener.RetryReady(context.Background()); err != nil {
				t.Fatal(err)
			}
			if callbacks != 1 || rescreener.Progress().UnresolvedPairs != 0 ||
				len(store.scores) != 1 {
				t.Fatalf("retry did not converge: callbacks=%d progress=%#v scores=%d",
					callbacks, rescreener.Progress(), len(store.scores))
			}
		})
	}
}

func TestRescreenerDoesNotHoldStateMutexAcrossPersistence(t *testing.T) {
	shaA, shaB := rescreenSHA('a'), rescreenSHA('b')
	store := newFakeRescreenStore(reconcileSnapshot{
		Pairs:  []candidatePair{testCandidate("image", shaA, shaB, 1)},
		Images: readyImageCache(t, shaA, shaB),
	})
	store.upsertEntered = make(chan struct{})
	store.upsertRelease = make(chan struct{})
	rescreener := newRescreener(store, rescreenConfig(), nil)
	done := make(chan error, 1)
	go func() { done <- rescreener.Restore(context.Background()) }()
	select {
	case <-store.upsertEntered:
	case <-time.After(time.Second):
		t.Fatal("upsert did not start")
	}

	progressDone := make(chan RescreenProgress, 1)
	go func() { progressDone <- rescreener.Progress() }()
	select {
	case progress := <-progressDone:
		if progress.InFlight != 1 {
			t.Fatalf("progress while persisting = %#v", progress)
		}
	case <-time.After(time.Second):
		t.Fatal("Progress blocked behind PostgreSQL I/O")
	}
	close(store.upsertRelease)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRescreenerEvictsResolvedCacheAndIgnoresUnrelatedStream(t *testing.T) {
	shaA, shaB, shaC := rescreenSHA('a'), rescreenSHA('b'), rescreenSHA('c')
	store := newFakeRescreenStore(reconcileSnapshot{
		Pairs: []candidatePair{
			testCandidate("image", shaA, shaB, 1),
			testCandidate("image", shaB, shaC, 2),
		},
	})
	rescreener := newRescreener(store, rescreenConfig(), nil)
	if err := rescreener.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	parts, sobel := rescreenFeatureBlobs(t, 1)
	for _, sha := range []string{shaA, shaB} {
		if err := rescreener.HandleFeatureResult(
			context.Background(),
			rescreenResult(imageWireItem(
				sha,
				proto.FieldPHashParts|proto.FieldSobelHist,
				parts,
				sobel,
			)),
		); err != nil {
			t.Fatal(err)
		}
	}
	if got := rescreener.Progress(); got.UnresolvedPairs != 1 || got.CachedEndpoints != 1 {
		t.Fatalf("overlap progress = %#v, want only shared B cached", got)
	}

	unrelated := rescreenSHA('f')
	if err := rescreener.HandleFeatureResult(
		context.Background(),
		rescreenResult(imageWireItem(
			unrelated,
			proto.FieldPHashParts|proto.FieldSobelHist,
			parts,
			sobel,
		)),
	); err != nil {
		t.Fatal(err)
	}
	if got := rescreener.Progress(); got.CachedEndpoints != 1 {
		t.Fatalf("unrelated stream was retained: %#v", got)
	}

	if err := rescreener.HandleFeatureResult(
		context.Background(),
		rescreenResult(imageWireItem(
			shaC,
			proto.FieldPHashParts|proto.FieldSobelHist,
			parts,
			sobel,
		)),
	); err != nil {
		t.Fatal(err)
	}
	if got := rescreener.Progress(); got.UnresolvedPairs != 0 || got.CachedEndpoints != 0 {
		t.Fatalf("final cache not evicted: %#v", got)
	}
}

func TestRescreenerRestoreUsesDurableResolvedAndReadyFeaturesButKeepsPartial(t *testing.T) {
	shaA, shaB := rescreenSHA('a'), rescreenSHA('b')
	shaC, shaD := rescreenSHA('c'), rescreenSHA('d')
	shaE, shaF := rescreenSHA('e'), rescreenSHA('f')
	resolvedPair := testCandidate("image", shaC, shaD, 2)
	resolvedScore := testImagePersistedScore(t, resolvedPair)
	partialParts, _ := rescreenFeatureBlobs(t, 4)
	partial := imageFeatureCache{PHashRaw: partialParts}
	parts, _ := features.DecodePHashParts(partialParts)
	partial.PHashParts = &parts
	store := newFakeRescreenStore(reconcileSnapshot{
		Pairs: []candidatePair{
			testCandidate("image", shaA, shaB, 1),
			resolvedPair,
			testCandidate("image", shaE, shaF, 3),
		},
		Resolved: map[PairKey]persistedPairScore{resolvedPair.Key: resolvedScore},
		Images: mergeImageCaches(
			readyImageCache(t, shaA, shaB),
			map[string]imageFeatureCache{shaE: partial},
		),
	})
	rescreener := newRescreener(store, rescreenConfig(), nil)
	if err := rescreener.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.upserts != 1 {
		t.Fatalf("ready restore upserts=%d, want one", store.upserts)
	}
	progress := rescreener.Progress()
	if progress.TotalPairs != 3 || progress.ResolvedPairs != 2 ||
		progress.UnresolvedPairs != 1 || progress.CachedEndpoints != 1 {
		t.Fatalf("restored progress = %#v", progress)
	}
}

func TestRescreenerReloadReplacesGenerationAndLateRemovedResultsCannotReappear(t *testing.T) {
	shaA, shaB := rescreenSHA('a'), rescreenSHA('b')
	shaC, shaD := rescreenSHA('c'), rescreenSHA('d')
	store := newFakeRescreenStore(reconcileSnapshot{
		Pairs: []candidatePair{testCandidate("image", shaA, shaB, 1)},
	})
	rescreener := newRescreener(store, rescreenConfig(), nil)
	if err := rescreener.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	firstGeneration := rescreener.Progress().Generation
	store.snapshot = reconcileSnapshot{
		Pairs: []candidatePair{testCandidate("image", shaC, shaD, 2)},
	}
	if err := rescreener.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rescreener.Progress().Generation <= firstGeneration {
		t.Fatal("reload did not advance generation")
	}

	parts, sobel := rescreenFeatureBlobs(t, 1)
	for _, sha := range []string{shaA, shaB} {
		if err := rescreener.HandleFeatureResult(
			context.Background(),
			rescreenResult(imageWireItem(
				sha,
				proto.FieldPHashParts|proto.FieldSobelHist,
				parts,
				sobel,
			)),
		); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.scores) != 0 || rescreener.Progress().CachedEndpoints != 0 {
		t.Fatal("removed generation reappeared after late results")
	}
}

func TestRescreenerFinalizesOnlyAfterDurableAllTerminalBoundary(t *testing.T) {
	shaA, shaB := rescreenSHA('a'), rescreenSHA('b')
	store := newFakeRescreenStore(reconcileSnapshot{
		Pairs: []candidatePair{testCandidate("image", shaA, shaB, 1)},
	})
	store.activePhase2 = true
	rescreener := newRescreener(store, rescreenConfig(), nil)
	if err := rescreener.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	finalized, err := rescreener.FinalizeIfIdle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if finalized || store.upserts != 0 {
		t.Fatal("first terminal shard swept while another durable task remained active")
	}

	store.activePhase2 = false
	finalized, err = rescreener.FinalizeIfIdle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !finalized || store.upserts != 1 {
		t.Fatalf("safe finalization finalized=%v upserts=%d", finalized, store.upserts)
	}
	score := store.onlyScore(t)
	if score.Verdict != "inconclusive" || score.Document.Image == nil ||
		score.Document.Image.SobelEvaluated ||
		score.Document.Image.SobelCosine != nil {
		t.Fatalf("incomplete image document = %#v", score.Document)
	}
}

func TestRescreenerBarrierErrorPreventsFinalization(t *testing.T) {
	shaA, shaB := rescreenSHA('a'), rescreenSHA('b')
	store := newFakeRescreenStore(reconcileSnapshot{
		Pairs: []candidatePair{testCandidate("image", shaA, shaB, 1)},
	})
	store.activeErr = errors.New("barrier query failed")
	rescreener := newRescreener(store, rescreenConfig(), nil)
	if err := rescreener.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	finalized, err := rescreener.FinalizeIfIdle(context.Background())
	if err == nil || finalized {
		t.Fatalf("barrier failure finalized=%v err=%v", finalized, err)
	}
	if store.upserts != 0 || len(store.scores) != 0 {
		t.Fatal("barrier failure persisted an inconclusive score")
	}
}

func TestRescreenerConcurrentDuplicateDeliveryFiresOneCallback(t *testing.T) {
	shaA, shaB := rescreenSHA('a'), rescreenSHA('b')
	store := newFakeRescreenStore(reconcileSnapshot{
		Pairs:  []candidatePair{testCandidate("image", shaA, shaB, 1)},
		Images: readyImageCache(t, shaA, shaB),
	})
	rescreener := newRescreener(store, rescreenConfig(), nil)
	var callbackCount int
	var callbackMu sync.Mutex
	rescreener.SetOnPairResolved(func(PairKey, Verdict) {
		callbackMu.Lock()
		callbackCount++
		callbackMu.Unlock()
	})
	if err := rescreener.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}

	parts, sobel := rescreenFeatureBlobs(t, 1)
	result := rescreenResult(imageWireItem(
		shaA,
		proto.FieldPHashParts|proto.FieldSobelHist,
		parts,
		sobel,
	))
	var wait sync.WaitGroup
	for index := 0; index < 20; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := rescreener.HandleFeatureResult(context.Background(), result); err != nil {
				t.Errorf("duplicate delivery: %v", err)
			}
		}()
	}
	wait.Wait()
	callbackMu.Lock()
	defer callbackMu.Unlock()
	if callbackCount != 1 || store.upserts != 1 {
		t.Fatalf("callback=%d upserts=%d, want one each", callbackCount, store.upserts)
	}
}

type fakeRescreenStore struct {
	mu sync.Mutex

	snapshot      reconcileSnapshot
	scores        map[PairKey]persistedPairScore
	upserts       int
	upsertErr     error
	commitOnError bool
	activePhase2  bool
	activeErr     error
	upsertEntered chan struct{}
	upsertRelease chan struct{}
	enteredOnce   sync.Once
}

func newFakeRescreenStore(snapshot reconcileSnapshot) *fakeRescreenStore {
	return &fakeRescreenStore{
		snapshot: snapshot,
		scores:   make(map[PairKey]persistedPairScore),
	}
}

func (store *fakeRescreenStore) reconcile(context.Context) (reconcileSnapshot, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return cloneReconcileSnapshot(store.snapshot), nil
}

func (store *fakeRescreenStore) upsertScore(
	_ context.Context,
	score persistedPairScore,
) (persistedPairScore, error) {
	if store.upsertEntered != nil {
		store.enteredOnce.Do(func() { close(store.upsertEntered) })
		<-store.upsertRelease
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.upserts++
	if store.upsertErr == nil || store.commitOnError {
		store.scores[score.Key] = score
	}
	if store.upsertErr != nil {
		return persistedPairScore{}, store.upsertErr
	}
	return score, nil
}

func (store *fakeRescreenStore) hasRelevantActivePhase2(
	context.Context,
	[]PairKey,
) (bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.activePhase2, store.activeErr
}

func (store *fakeRescreenStore) onlyScore(t *testing.T) persistedPairScore {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.scores) != 1 {
		t.Fatalf("scores=%#v, want exactly one", store.scores)
	}
	for _, score := range store.scores {
		return score
	}
	panic("unreachable")
}

func cloneReconcileSnapshot(snapshot reconcileSnapshot) reconcileSnapshot {
	cloned := snapshot
	cloned.Pairs = append([]candidatePair(nil), snapshot.Pairs...)
	cloned.Resolved = make(map[PairKey]persistedPairScore, len(snapshot.Resolved))
	for key, value := range snapshot.Resolved {
		cloned.Resolved[key] = value
	}
	cloned.Images = make(map[string]imageFeatureCache, len(snapshot.Images))
	for key, value := range snapshot.Images {
		cloned.Images[key] = cloneImageFeatureCache(value)
	}
	cloned.Videos = make(map[string]videoFeatureCache, len(snapshot.Videos))
	for key, value := range snapshot.Videos {
		cloned.Videos[key] = cloneVideoFeatureCache(value)
	}
	return cloned
}

func rescreenConfig() config.Phase2Config {
	return config.Phase2Config{
		PHashPassT2: 0.80, PHashPartThreshold: 10, SobelT3: 0.85,
		VideoFrames: 6, VideoAvgT4: 0.80, VideoMinPassed: 4, VideoMinValid: 4,
	}
}

func rescreenSHA(fill byte) string {
	return strings.Repeat(string(fill), 128)
}

func testCandidate(kind, left, right string, hamming int) candidatePair {
	if right < left {
		left, right = right, left
	}
	return candidatePair{
		Key: PairKey{Kind: kind, SHAA: left, SHAB: right},
		Trace: firstScreenTrace{
			Present: true, Hamming: hamming, QualityA: 70, QualityB: 80,
		},
	}
}

func rescreenFeatureBlobs(t *testing.T, seed uint64) ([]byte, []byte) {
	t.Helper()
	var parts [9]uint64
	parts[0] = seed
	var hist [128]float32
	hist[0] = 1
	return features.EncodePHashParts(parts), mustEncodeSobel(t, hist)
}

func mustEncodeSobel(t *testing.T, hist [128]float32) []byte {
	t.Helper()
	blob, err := features.EncodeSobelHist(hist)
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

func rescreenResult(items ...proto.FeatureItem) *BoundFeatureResult {
	bound := &BoundFeatureResult{TaskID: "phase2-task", Seq: 1}
	for _, item := range items {
		kind := uint8(proto.KindImage)
		if len(item.Frames) != 0 || item.FieldsDone&proto.FieldVideo6F != 0 {
			kind = proto.KindVideo
		}
		bound.Items = append(bound.Items, BoundFeatureItem{
			Kind: kind,
			Item: item,
		})
	}
	return bound
}

func imageWireItem(
	sha string,
	fields uint32,
	parts, sobel []byte,
) proto.FeatureItem {
	status := proto.StatusPartial
	if fields == proto.FieldPHashParts|proto.FieldSobelHist {
		status = proto.StatusDone
	}
	return proto.FeatureItem{
		SHA512: sha, Status: status, FieldsDone: fields,
		PHashParts: append([]byte(nil), parts...),
		SobelHist:  append([]byte(nil), sobel...),
	}
}

func rescreenWireFrame(t *testing.T, index int, seed uint64) proto.FrameFeature {
	t.Helper()
	parts, sobel := rescreenFeatureBlobs(t, seed)
	return proto.FrameFeature{
		FrameIdx: index, PDQ256: bytes.Repeat([]byte{byte(seed)}, 32),
		Quality: 80, PHashParts: parts, SobelHist: sobel,
	}
}

func videoWireItem(t *testing.T, sha string, indexes ...int) proto.FeatureItem {
	t.Helper()
	item := proto.FeatureItem{SHA512: sha, Status: proto.StatusPartial}
	for _, index := range indexes {
		item.Frames = append(item.Frames, rescreenWireFrame(t, index, 1))
	}
	return item
}

func readyImageCache(t *testing.T, shas ...string) map[string]imageFeatureCache {
	t.Helper()
	partsRaw, sobelRaw := rescreenFeatureBlobs(t, 1)
	parts, err := features.DecodePHashParts(partsRaw)
	if err != nil {
		t.Fatal(err)
	}
	sobel, err := features.DecodeSobelHist(sobelRaw)
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[string]imageFeatureCache, len(shas))
	for _, sha := range shas {
		partsCopy, sobelCopy := parts, sobel
		result[sha] = imageFeatureCache{
			PHashParts: &partsCopy, PHashRaw: append([]byte(nil), partsRaw...),
			SobelHist: &sobelCopy, SobelRaw: append([]byte(nil), sobelRaw...),
		}
	}
	return result
}

func mergeImageCaches(
	left, right map[string]imageFeatureCache,
) map[string]imageFeatureCache {
	result := make(map[string]imageFeatureCache, len(left)+len(right))
	for key, value := range left {
		result[key] = value
	}
	for key, value := range right {
		result[key] = value
	}
	return result
}

func testImagePersistedScore(
	t *testing.T,
	pair candidatePair,
) persistedPairScore {
	t.Helper()
	cfg := rescreenConfig()
	partsRaw, sobelRaw := rescreenFeatureBlobs(t, 1)
	parts, _ := features.DecodePHashParts(partsRaw)
	sobel, _ := features.DecodeSobelHist(sobelRaw)
	score, err := JudgeImagePair(parts, parts, sobel, sobel, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return makeImagePersistedScore(pair, score)
}

func TestCloneReconcileSnapshotIsDeepForMutableFeatureBytes(t *testing.T) {
	sha := rescreenSHA('a')
	original := reconcileSnapshot{Images: readyImageCache(t, sha)}
	cloned := cloneReconcileSnapshot(original)
	cloned.Images[sha].PHashRaw[0] ^= 0xff
	if reflect.DeepEqual(cloned.Images[sha].PHashRaw, original.Images[sha].PHashRaw) {
		t.Fatal("test fake snapshot clone shares mutable bytes")
	}
}
