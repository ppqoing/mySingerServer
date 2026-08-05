package phase2

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"dedup/internal/config"
	"dedup/internal/features"
	"dedup/internal/proto"
)

const pairScoreVersion = 1

type PairKey struct {
	Kind string
	SHAA string
	SHAB string
}

type firstScreenTrace struct {
	Present        bool
	Hamming        int
	DurationDiffMS *int64
	QualityA       int
	QualityB       int
}

type candidatePair struct {
	Key   PairKey
	Trace firstScreenTrace
}

type pairScoreDocument struct {
	Version     int                       `json:"version"`
	Kind        string                    `json:"kind"`
	Verdict     string                    `json:"verdict"`
	FirstScreen *firstScreenScoreDocument `json:"first_screen,omitempty"`
	Image       *imageScoreDocument       `json:"image,omitempty"`
	Video       *videoScoreDocument       `json:"video,omitempty"`
}

type firstScreenScoreDocument struct {
	Hamming        int    `json:"hamming"`
	DurationDiffMS *int64 `json:"duration_diff_ms,omitempty"`
	QualityA       int    `json:"quality_a"`
	QualityB       int    `json:"quality_b"`
}

type imageScoreDocument struct {
	PHashEvaluated bool     `json:"phash_evaluated"`
	PHashPassRatio *float64 `json:"phash_pass_ratio,omitempty"`
	SobelEvaluated bool     `json:"sobel_evaluated"`
	SobelCosine    *float64 `json:"sobel_cosine,omitempty"`
}

type videoScoreDocument struct {
	ValidFrames      int                  `json:"valid_frames"`
	PassedFrames     int                  `json:"passed_frames"`
	AverageEvaluated bool                 `json:"average_evaluated"`
	Average          *float64             `json:"average,omitempty"`
	Frames           []frameScoreDocument `json:"frames"`
}

type frameScoreDocument struct {
	FrameIdx       int      `json:"frame_idx"`
	Valid          bool     `json:"valid"`
	PHashPassRatio *float64 `json:"phash_pass_ratio,omitempty"`
	SobelEvaluated bool     `json:"sobel_evaluated"`
	SobelCosine    *float64 `json:"sobel_cosine,omitempty"`
	Similarity     *float64 `json:"similarity,omitempty"`
	Passed         bool     `json:"passed"`
}

type persistedPairScore struct {
	Key      PairKey
	Verdict  string
	Document pairScoreDocument
}

type imageFeatureCache struct {
	PHashParts *[9]uint64
	PHashRaw   []byte
	SobelHist  *[128]float32
	SobelRaw   []byte
}

type videoFrameCache struct {
	PDQ256     *[32]byte
	PDQRaw     []byte
	PHashParts *[9]uint64
	PHashRaw   []byte
	SobelHist  *[128]float32
	SobelRaw   []byte
}

type videoFeatureCache struct {
	Frames [6]videoFrameCache
}

type reconcileSnapshot struct {
	Pairs    []candidatePair
	Resolved map[PairKey]persistedPairScore
	Images   map[string]imageFeatureCache
	Videos   map[string]videoFeatureCache
}

type rescreenStore interface {
	reconcile(context.Context) (reconcileSnapshot, error)
	upsertScore(context.Context, persistedPairScore) (persistedPairScore, error)
	hasRelevantActivePhase2(context.Context, []PairKey) (bool, error)
}

type pairRuntime struct {
	pair       candidatePair
	evaluating bool
}

type endpointKey struct {
	Kind string
	SHA  string
}

type RescreenProgress struct {
	Generation      uint64
	TotalPairs      int
	ResolvedPairs   int
	UnresolvedPairs int
	CachedEndpoints int
	InFlight        int
}

type Rescreener struct {
	store rescreenStore
	cfg   config.Phase2Config
	log   *slog.Logger

	ioMu sync.Mutex
	mu   sync.Mutex

	generation uint64
	total      int
	resolved   int
	pairs      map[PairKey]*pairRuntime
	waiters    map[endpointKey]map[PairKey]struct{}
	images     map[string]imageFeatureCache
	videos     map[string]videoFeatureCache
	onResolved func(PairKey, Verdict)
}

func NewRescreener(
	pool postgresDB,
	cfg config.Phase2Config,
	logger *slog.Logger,
) *Rescreener {
	return newRescreener(&postgresRescreenStore{pool: pool}, cfg, logger)
}

func newRescreener(
	store rescreenStore,
	cfg config.Phase2Config,
	logger *slog.Logger,
) *Rescreener {
	return &Rescreener{
		store:   store,
		cfg:     cfg,
		log:     logger,
		pairs:   make(map[PairKey]*pairRuntime),
		waiters: make(map[endpointKey]map[PairKey]struct{}),
		images:  make(map[string]imageFeatureCache),
		videos:  make(map[string]videoFeatureCache),
	}
}

func (rescreener *Rescreener) SetOnPairResolved(
	callback func(PairKey, Verdict),
) {
	rescreener.mu.Lock()
	rescreener.onResolved = callback
	rescreener.mu.Unlock()
}

func (rescreener *Rescreener) Restore(ctx context.Context) error {
	return rescreener.Reload(ctx)
}

func (rescreener *Rescreener) Reload(ctx context.Context) error {
	if rescreener.store == nil {
		return fmt.Errorf("phase2: rescreener store is nil")
	}
	if err := validateJudgeConfig(rescreener.cfg); err != nil {
		return err
	}

	rescreener.ioMu.Lock()
	defer rescreener.ioMu.Unlock()
	snapshot, err := rescreener.store.reconcile(ctx)
	if err != nil {
		return fmt.Errorf("phase2: reconcile rescreener: %w", err)
	}
	if err := rescreener.installSnapshot(snapshot); err != nil {
		return err
	}
	return rescreener.retryReadyLocked(ctx)
}

func (rescreener *Rescreener) installSnapshot(
	snapshot reconcileSnapshot,
) error {
	pairs := make(map[PairKey]*pairRuntime)
	waiters := make(map[endpointKey]map[PairKey]struct{})
	for _, pair := range snapshot.Pairs {
		if err := validatePairKey(pair.Key); err != nil {
			return err
		}
		if _, duplicate := pairs[pair.Key]; duplicate {
			return fmt.Errorf("phase2: duplicate reconciled pair %#v", pair.Key)
		}
		if _, resolved := snapshot.Resolved[pair.Key]; resolved {
			continue
		}
		pairs[pair.Key] = &pairRuntime{pair: pair}
		for _, sha := range []string{pair.Key.SHAA, pair.Key.SHAB} {
			endpoint := endpointKey{Kind: pair.Key.Kind, SHA: sha}
			if waiters[endpoint] == nil {
				waiters[endpoint] = make(map[PairKey]struct{})
			}
			waiters[endpoint][pair.Key] = struct{}{}
		}
	}
	for key := range snapshot.Resolved {
		if _, exists := findCandidate(snapshot.Pairs, key); !exists {
			return fmt.Errorf("phase2: resolved score has no active candidate %#v", key)
		}
	}

	images := make(map[string]imageFeatureCache)
	videos := make(map[string]videoFeatureCache)
	for endpoint := range waiters {
		switch endpoint.Kind {
		case "image":
			if feature, exists := snapshot.Images[endpoint.SHA]; exists &&
				!imageFeatureEmpty(feature) {
				images[endpoint.SHA] = cloneImageFeatureCache(feature)
			}
		case "video":
			if feature, exists := snapshot.Videos[endpoint.SHA]; exists &&
				!videoFeatureEmpty(feature) {
				videos[endpoint.SHA] = cloneVideoFeatureCache(feature)
			}
		}
	}

	rescreener.mu.Lock()
	rescreener.generation++
	rescreener.total = len(snapshot.Pairs)
	rescreener.resolved = len(snapshot.Resolved)
	rescreener.pairs = pairs
	rescreener.waiters = waiters
	rescreener.images = images
	rescreener.videos = videos
	rescreener.mu.Unlock()
	return nil
}

func (rescreener *Rescreener) HandleFeatureResult(
	ctx context.Context,
	result *BoundFeatureResult,
) error {
	updates, err := decodeBoundFeatureResult(result)
	if err != nil {
		return err
	}

	rescreener.ioMu.Lock()
	defer rescreener.ioMu.Unlock()
	rescreener.mu.Lock()
	if err := rescreener.mergeUpdatesLocked(updates); err != nil {
		rescreener.mu.Unlock()
		return err
	}
	rescreener.mu.Unlock()
	return rescreener.retryReadyLocked(ctx)
}

func (rescreener *Rescreener) RetryReady(ctx context.Context) error {
	rescreener.ioMu.Lock()
	defer rescreener.ioMu.Unlock()
	return rescreener.retryReadyLocked(ctx)
}

func (rescreener *Rescreener) FinalizeIfIdle(
	ctx context.Context,
) (bool, error) {
	rescreener.ioMu.Lock()
	defer rescreener.ioMu.Unlock()

	rescreener.mu.Lock()
	keys := make([]PairKey, 0, len(rescreener.pairs))
	for key := range rescreener.pairs {
		keys = append(keys, key)
	}
	rescreener.mu.Unlock()
	if len(keys) == 0 {
		return true, nil
	}
	sortPairKeys(keys)
	active, err := rescreener.store.hasRelevantActivePhase2(ctx, keys)
	if err != nil {
		return false, fmt.Errorf("phase2: check finalization barrier: %w", err)
	}
	if active {
		return false, nil
	}
	if err := rescreener.finalizeLocked(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (rescreener *Rescreener) Progress() RescreenProgress {
	rescreener.mu.Lock()
	defer rescreener.mu.Unlock()
	inFlight := 0
	for _, pair := range rescreener.pairs {
		if pair.evaluating {
			inFlight++
		}
	}
	return RescreenProgress{
		Generation:      rescreener.generation,
		TotalPairs:      rescreener.total,
		ResolvedPairs:   rescreener.resolved,
		UnresolvedPairs: len(rescreener.pairs),
		CachedEndpoints: len(rescreener.images) + len(rescreener.videos),
		InFlight:        inFlight,
	}
}

type decodedFeatureUpdate struct {
	kind   string
	sha    string
	image  imageFeatureCache
	frames map[int]videoFrameCache
}

func decodeBoundFeatureResult(
	result *BoundFeatureResult,
) ([]decodedFeatureUpdate, error) {
	if result == nil || result.TaskID == "" || len(result.Items) == 0 {
		return nil, fmt.Errorf("phase2: invalid bound feature result envelope")
	}
	updates := make([]decodedFeatureUpdate, 0, len(result.Items))
	for index, bound := range result.Items {
		item := bound.Item
		if !isCanonicalSHA512(item.SHA512) {
			return nil, fmt.Errorf("phase2: result item %d has noncanonical SHA-512", index)
		}
		if !validResultStatus(item.Status) {
			return nil, fmt.Errorf("phase2: result item %d has invalid status %q", index, item.Status)
		}
		update := decodedFeatureUpdate{sha: item.SHA512}
		switch bound.Kind {
		case proto.KindImage:
			update.kind = "image"
			feature, err := decodeImageWireItem(item)
			if err != nil {
				return nil, fmt.Errorf("phase2: image result item %d: %w", index, err)
			}
			update.image = feature
		case proto.KindVideo:
			update.kind = "video"
			frames, err := decodeVideoWireItem(item)
			if err != nil {
				return nil, fmt.Errorf("phase2: video result item %d: %w", index, err)
			}
			update.frames = frames
		default:
			return nil, fmt.Errorf("phase2: result item %d has invalid bound kind %d", index, bound.Kind)
		}
		updates = append(updates, update)
	}
	return updates, nil
}

func decodeImageWireItem(item proto.FeatureItem) (imageFeatureCache, error) {
	var result imageFeatureCache
	const imageFields = proto.FieldPHashParts | proto.FieldSobelHist
	if len(item.Frames) != 0 || item.FieldsDone&^imageFields != 0 {
		return result, fmt.Errorf("contains non-image phase-2 fields")
	}
	hasPHash := len(item.PHashParts) != 0
	hasSobel := len(item.SobelHist) != 0
	if hasPHash != (item.FieldsDone&proto.FieldPHashParts != 0) {
		return result, fmt.Errorf("pHash success bit/payload mismatch")
	}
	if hasSobel != (item.FieldsDone&proto.FieldSobelHist != 0) {
		return result, fmt.Errorf("Sobel success bit/payload mismatch")
	}
	if hasPHash {
		parts, err := features.DecodePHashParts(item.PHashParts)
		if err != nil {
			return result, err
		}
		result.PHashParts = &parts
		result.PHashRaw = append([]byte(nil), item.PHashParts...)
	}
	if hasSobel {
		hist, err := features.DecodeSobelHist(item.SobelHist)
		if err != nil {
			return result, err
		}
		result.SobelHist = &hist
		result.SobelRaw = append([]byte(nil), item.SobelHist...)
	}
	return result, nil
}

func decodeVideoWireItem(item proto.FeatureItem) (map[int]videoFrameCache, error) {
	if len(item.PHashParts) != 0 || len(item.SobelHist) != 0 ||
		item.FieldsDone&^proto.FieldVideo6F != 0 {
		return nil, fmt.Errorf("contains non-video phase-2 fields")
	}
	if item.FieldsDone&proto.FieldVideo6F != 0 && len(item.Frames) == 0 {
		return nil, fmt.Errorf("video success bit has no frame payload")
	}
	frames := make(map[int]videoFrameCache, len(item.Frames))
	for index, frame := range item.Frames {
		if frame.FrameIdx < 0 || frame.FrameIdx >= 6 {
			return nil, fmt.Errorf("frame %d index %d is invalid", index, frame.FrameIdx)
		}
		if _, duplicate := frames[frame.FrameIdx]; duplicate {
			return nil, fmt.Errorf("duplicate frame index %d", frame.FrameIdx)
		}
		hasPayload := len(frame.PDQ256) != 0 ||
			len(frame.PHashParts) != 0 ||
			len(frame.SobelHist) != 0
		if frame.Error != "" {
			if hasPayload {
				return nil, fmt.Errorf("errored frame %d also has payload", frame.FrameIdx)
			}
			continue
		}
		if len(frame.PDQ256) != 32 {
			return nil, fmt.Errorf("frame %d PDQ length %d, want 32", frame.FrameIdx, len(frame.PDQ256))
		}
		parts, err := features.DecodePHashParts(frame.PHashParts)
		if err != nil {
			return nil, fmt.Errorf("frame %d pHash: %w", frame.FrameIdx, err)
		}
		hist, err := features.DecodeSobelHist(frame.SobelHist)
		if err != nil {
			return nil, fmt.Errorf("frame %d Sobel: %w", frame.FrameIdx, err)
		}
		var pdq [32]byte
		copy(pdq[:], frame.PDQ256)
		frames[frame.FrameIdx] = videoFrameCache{
			PDQ256: &pdq, PDQRaw: append([]byte(nil), frame.PDQ256...),
			PHashParts: &parts, PHashRaw: append([]byte(nil), frame.PHashParts...),
			SobelHist: &hist, SobelRaw: append([]byte(nil), frame.SobelHist...),
		}
	}
	return frames, nil
}

func (rescreener *Rescreener) mergeUpdatesLocked(
	updates []decodedFeatureUpdate,
) error {
	nextImages := make(map[string]imageFeatureCache)
	nextVideos := make(map[string]videoFeatureCache)
	for _, update := range updates {
		endpoint := endpointKey{Kind: update.kind, SHA: update.sha}
		if len(rescreener.waiters[endpoint]) == 0 {
			continue
		}
		switch update.kind {
		case "image":
			current, exists := nextImages[update.sha]
			if !exists {
				current = cloneImageFeatureCache(rescreener.images[update.sha])
			}
			if err := mergeImageFeature(&current, update.image); err != nil {
				return fmt.Errorf("phase2: merge image %s: %w", update.sha, err)
			}
			nextImages[update.sha] = current
		case "video":
			current, exists := nextVideos[update.sha]
			if !exists {
				current = cloneVideoFeatureCache(rescreener.videos[update.sha])
			}
			for frameIndex, frame := range update.frames {
				if err := mergeVideoFrame(
					&current.Frames[frameIndex],
					frame,
				); err != nil {
					return fmt.Errorf(
						"phase2: merge video %s frame %d: %w",
						update.sha,
						frameIndex,
						err,
					)
				}
			}
			nextVideos[update.sha] = current
		}
	}
	for sha, feature := range nextImages {
		if !imageFeatureEmpty(feature) {
			rescreener.images[sha] = feature
		}
	}
	for sha, feature := range nextVideos {
		if !videoFeatureEmpty(feature) {
			rescreener.videos[sha] = feature
		}
	}
	return nil
}

func mergeImageFeature(
	current *imageFeatureCache,
	update imageFeatureCache,
) error {
	if update.PHashParts != nil {
		if current.PHashParts != nil && !bytes.Equal(current.PHashRaw, update.PHashRaw) {
			return fmt.Errorf("divergent duplicate pHash")
		}
		if current.PHashParts == nil {
			current.PHashParts = update.PHashParts
			current.PHashRaw = append([]byte(nil), update.PHashRaw...)
		}
	}
	if update.SobelHist != nil {
		if current.SobelHist != nil && !bytes.Equal(current.SobelRaw, update.SobelRaw) {
			return fmt.Errorf("divergent duplicate Sobel")
		}
		if current.SobelHist == nil {
			current.SobelHist = update.SobelHist
			current.SobelRaw = append([]byte(nil), update.SobelRaw...)
		}
	}
	return nil
}

func mergeVideoFrame(
	current *videoFrameCache,
	update videoFrameCache,
) error {
	if current.PDQ256 != nil {
		if !bytes.Equal(current.PDQRaw, update.PDQRaw) ||
			!bytes.Equal(current.PHashRaw, update.PHashRaw) ||
			!bytes.Equal(current.SobelRaw, update.SobelRaw) {
			return fmt.Errorf("divergent duplicate frame")
		}
		return nil
	}
	*current = cloneVideoFrameCache(update)
	return nil
}

type pendingEvaluation struct {
	generation uint64
	pair       candidatePair
	score      persistedPairScore
	verdict    Verdict
}

func (rescreener *Rescreener) retryReadyLocked(ctx context.Context) error {
	for {
		evaluation, ready, err := rescreener.nextEvaluation(false)
		if err != nil {
			return err
		}
		if !ready {
			return nil
		}
		if err := rescreener.persistEvaluation(ctx, evaluation); err != nil {
			return err
		}
	}
}

func (rescreener *Rescreener) finalizeLocked(ctx context.Context) error {
	for {
		evaluation, ready, err := rescreener.nextEvaluation(true)
		if err != nil {
			return err
		}
		if !ready {
			return nil
		}
		if err := rescreener.persistEvaluation(ctx, evaluation); err != nil {
			return err
		}
	}
}

func (rescreener *Rescreener) nextEvaluation(
	final bool,
) (pendingEvaluation, bool, error) {
	rescreener.mu.Lock()
	defer rescreener.mu.Unlock()
	keys := make([]PairKey, 0, len(rescreener.pairs))
	for key, state := range rescreener.pairs {
		if !state.evaluating {
			keys = append(keys, key)
		}
	}
	sortPairKeys(keys)
	for _, key := range keys {
		state := rescreener.pairs[key]
		score, verdict, ready, err := rescreener.evaluatePairLocked(state.pair, final)
		if err != nil {
			return pendingEvaluation{}, false, err
		}
		if !ready {
			continue
		}
		state.evaluating = true
		return pendingEvaluation{
			generation: rescreener.generation,
			pair:       state.pair,
			score:      score,
			verdict:    verdict,
		}, true, nil
	}
	return pendingEvaluation{}, false, nil
}

func (rescreener *Rescreener) evaluatePairLocked(
	pair candidatePair,
	final bool,
) (persistedPairScore, Verdict, bool, error) {
	switch pair.Key.Kind {
	case "image":
		left := rescreener.images[pair.Key.SHAA]
		right := rescreener.images[pair.Key.SHAB]
		if left.PHashParts == nil || left.SobelHist == nil ||
			right.PHashParts == nil || right.SobelHist == nil {
			if !final {
				return persistedPairScore{}, VerdictNo, false, nil
			}
			score := makeIncompleteImagePersistedScore(pair)
			return score, VerdictInconclusive, true, nil
		}
		imageScore, err := JudgeImagePair(
			*left.PHashParts,
			*right.PHashParts,
			*left.SobelHist,
			*right.SobelHist,
			rescreener.cfg,
		)
		if err != nil {
			return persistedPairScore{}, VerdictNo, false, err
		}
		score := makeImagePersistedScore(pair, imageScore)
		return score, imageScore.Verdict, true, nil
	case "video":
		left := framePhase2Array(rescreener.videos[pair.Key.SHAA])
		right := framePhase2Array(rescreener.videos[pair.Key.SHAB])
		videoScore, err := JudgeVideoPair(left, right, rescreener.cfg)
		if err != nil {
			return persistedPairScore{}, VerdictNo, false, err
		}
		if videoScore.Verdict == VerdictInconclusive && !final {
			return persistedPairScore{}, VerdictNo, false, nil
		}
		score := makeVideoPersistedScore(pair, videoScore)
		return score, videoScore.Verdict, true, nil
	default:
		return persistedPairScore{}, VerdictNo, false,
			fmt.Errorf("phase2: unsupported pair kind %q", pair.Key.Kind)
	}
}

func (rescreener *Rescreener) persistEvaluation(
	ctx context.Context,
	evaluation pendingEvaluation,
) error {
	_, err := rescreener.store.upsertScore(ctx, evaluation.score)
	if err != nil {
		rescreener.mu.Lock()
		if rescreener.generation == evaluation.generation {
			if state := rescreener.pairs[evaluation.pair.Key]; state != nil {
				state.evaluating = false
			}
		}
		rescreener.mu.Unlock()
		return fmt.Errorf("phase2: persist pair score: %w", err)
	}

	rescreener.mu.Lock()
	if rescreener.generation != evaluation.generation {
		rescreener.mu.Unlock()
		return nil
	}
	state := rescreener.pairs[evaluation.pair.Key]
	if state == nil || !state.evaluating {
		rescreener.mu.Unlock()
		return nil
	}
	delete(rescreener.pairs, evaluation.pair.Key)
	rescreener.resolved++
	for _, sha := range []string{
		evaluation.pair.Key.SHAA,
		evaluation.pair.Key.SHAB,
	} {
		endpoint := endpointKey{Kind: evaluation.pair.Key.Kind, SHA: sha}
		delete(rescreener.waiters[endpoint], evaluation.pair.Key)
		if len(rescreener.waiters[endpoint]) == 0 {
			delete(rescreener.waiters, endpoint)
			if endpoint.Kind == "image" {
				delete(rescreener.images, endpoint.SHA)
			} else {
				delete(rescreener.videos, endpoint.SHA)
			}
		}
	}
	callback := rescreener.onResolved
	rescreener.mu.Unlock()
	if callback != nil {
		callback(evaluation.pair.Key, evaluation.verdict)
	}
	return nil
}

func makeImagePersistedScore(
	pair candidatePair,
	score ImagePairScore,
) persistedPairScore {
	ratio := score.PHashPassRatio
	document := pairScoreDocument{
		Version: pairScoreVersion,
		Kind:    pair.Key.Kind,
		Verdict: verdictText(score.Verdict),
		Image: &imageScoreDocument{
			PHashEvaluated: true,
			PHashPassRatio: &ratio,
			SobelEvaluated: score.SobelEvaluated,
		},
		FirstScreen: traceDocument(pair.Trace),
	}
	if score.SobelEvaluated {
		cosine := score.SobelCosine
		document.Image.SobelCosine = &cosine
	}
	return persistedPairScore{
		Key: pair.Key, Verdict: document.Verdict, Document: document,
	}
}

func makeIncompleteImagePersistedScore(
	pair candidatePair,
) persistedPairScore {
	document := pairScoreDocument{
		Version:     pairScoreVersion,
		Kind:        pair.Key.Kind,
		Verdict:     verdictText(VerdictInconclusive),
		FirstScreen: traceDocument(pair.Trace),
		Image:       &imageScoreDocument{},
	}
	return persistedPairScore{
		Key: pair.Key, Verdict: document.Verdict, Document: document,
	}
}

func makeVideoPersistedScore(
	pair candidatePair,
	score VideoPairScore,
) persistedPairScore {
	video := &videoScoreDocument{
		ValidFrames:      score.ValidFrames,
		PassedFrames:     score.PassedFrames,
		AverageEvaluated: score.AverageEvaluated,
		Frames:           make([]frameScoreDocument, len(score.Frames)),
	}
	if score.AverageEvaluated {
		average := score.AvgSim
		video.Average = &average
	}
	for index, frame := range score.Frames {
		detail := frameScoreDocument{
			FrameIdx:       frame.FrameIdx,
			Valid:          frame.Valid,
			SobelEvaluated: frame.SobelEvaluated,
			Passed:         frame.Passed,
		}
		if frame.Valid {
			ratio, similarity := frame.PHashPassRatio, frame.Sim
			detail.PHashPassRatio = &ratio
			detail.Similarity = &similarity
		}
		if frame.SobelEvaluated {
			cosine := frame.SobelCosine
			detail.SobelCosine = &cosine
		}
		video.Frames[index] = detail
	}
	document := pairScoreDocument{
		Version:     pairScoreVersion,
		Kind:        pair.Key.Kind,
		Verdict:     verdictText(score.Verdict),
		FirstScreen: traceDocument(pair.Trace),
		Video:       video,
	}
	return persistedPairScore{
		Key: pair.Key, Verdict: document.Verdict, Document: document,
	}
}

func traceDocument(trace firstScreenTrace) *firstScreenScoreDocument {
	if !trace.Present {
		return nil
	}
	return &firstScreenScoreDocument{
		Hamming:        trace.Hamming,
		DurationDiffMS: cloneInt64Pointer(trace.DurationDiffMS),
		QualityA:       trace.QualityA,
		QualityB:       trace.QualityB,
	}
}

func verdictText(verdict Verdict) string {
	switch verdict {
	case VerdictYes:
		return "yes"
	case VerdictInconclusive:
		return "inconclusive"
	default:
		return "no"
	}
}

func framePhase2Array(feature videoFeatureCache) [6]*FramePhase2 {
	var frames [6]*FramePhase2
	for index, cached := range feature.Frames {
		if cached.PDQ256 == nil || cached.PHashParts == nil ||
			cached.SobelHist == nil {
			continue
		}
		frames[index] = &FramePhase2{
			PDQ256:     *cached.PDQ256,
			PHashParts: *cached.PHashParts,
			SobelHist:  *cached.SobelHist,
		}
	}
	return frames
}

func validatePairKey(key PairKey) error {
	if key.Kind != "image" && key.Kind != "video" {
		return fmt.Errorf("phase2: invalid pair kind %q", key.Kind)
	}
	if !isCanonicalSHA512(key.SHAA) || !isCanonicalSHA512(key.SHAB) ||
		key.SHAA >= key.SHAB {
		return fmt.Errorf("phase2: invalid normalized pair key %#v", key)
	}
	return nil
}

func validResultStatus(status string) bool {
	switch status {
	case proto.StatusDone, proto.StatusPartial, proto.StatusFailed, proto.StatusCrash:
		return true
	default:
		return false
	}
}

func sortPairKeys(keys []PairKey) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Kind != keys[j].Kind {
			return keys[i].Kind < keys[j].Kind
		}
		if keys[i].SHAA != keys[j].SHAA {
			return keys[i].SHAA < keys[j].SHAA
		}
		return keys[i].SHAB < keys[j].SHAB
	})
}

func findCandidate(
	pairs []candidatePair,
	key PairKey,
) (candidatePair, bool) {
	for _, pair := range pairs {
		if pair.Key == key {
			return pair, true
		}
	}
	return candidatePair{}, false
}

func imageFeatureEmpty(feature imageFeatureCache) bool {
	return feature.PHashParts == nil && feature.SobelHist == nil
}

func videoFeatureEmpty(feature videoFeatureCache) bool {
	for _, frame := range feature.Frames {
		if frame.PDQ256 != nil || frame.PHashParts != nil ||
			frame.SobelHist != nil {
			return false
		}
	}
	return true
}

func cloneImageFeatureCache(
	feature imageFeatureCache,
) imageFeatureCache {
	cloned := feature
	cloned.PHashRaw = append([]byte(nil), feature.PHashRaw...)
	cloned.SobelRaw = append([]byte(nil), feature.SobelRaw...)
	if feature.PHashParts != nil {
		value := *feature.PHashParts
		cloned.PHashParts = &value
	}
	if feature.SobelHist != nil {
		value := *feature.SobelHist
		cloned.SobelHist = &value
	}
	return cloned
}

func cloneVideoFeatureCache(
	feature videoFeatureCache,
) videoFeatureCache {
	var cloned videoFeatureCache
	for index, frame := range feature.Frames {
		cloned.Frames[index] = cloneVideoFrameCache(frame)
	}
	return cloned
}

func cloneVideoFrameCache(frame videoFrameCache) videoFrameCache {
	cloned := frame
	cloned.PDQRaw = append([]byte(nil), frame.PDQRaw...)
	cloned.PHashRaw = append([]byte(nil), frame.PHashRaw...)
	cloned.SobelRaw = append([]byte(nil), frame.SobelRaw...)
	if frame.PDQ256 != nil {
		value := *frame.PDQ256
		cloned.PDQ256 = &value
	}
	if frame.PHashParts != nil {
		value := *frame.PHashParts
		cloned.PHashParts = &value
	}
	if frame.SobelHist != nil {
		value := *frame.SobelHist
		cloned.SobelHist = &value
	}
	return cloned
}

func cloneInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
