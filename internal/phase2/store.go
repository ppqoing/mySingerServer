package phase2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"dedup/internal/features"
	"dedup/internal/proto"
)

var ErrPairScoreConflict = errors.New("phase2: conflicting durable pair score")

type postgresRescreenStore struct {
	pool postgresDB
}

func (store *postgresRescreenStore) reconcile(
	ctx context.Context,
) (snapshot reconcileSnapshot, err error) {
	if store.pool == nil {
		return snapshot, fmt.Errorf("phase2: PostgreSQL pool is nil")
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead,
	})
	if err != nil {
		return snapshot, fmt.Errorf("phase2: begin rescreener reconciliation: %w", err)
	}
	defer func() {
		if err == nil {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx),
			5*time.Second,
		)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

	snapshot.Pairs, err = loadCandidatePairs(ctx, tx)
	if err != nil {
		return snapshot, err
	}
	if err = auditAllPairScores(ctx, tx); err != nil {
		return snapshot, err
	}
	if err = deleteStalePairScores(ctx, tx, snapshot.Pairs); err != nil {
		return snapshot, err
	}
	snapshot.Resolved, err = loadResolvedScores(ctx, tx, snapshot.Pairs)
	if err != nil {
		return snapshot, err
	}

	imageSHAs, videoSHAs := unresolvedEndpointSHAs(
		snapshot.Pairs,
		snapshot.Resolved,
	)
	snapshot.Images, err = loadRescreenImages(ctx, tx, imageSHAs)
	if err != nil {
		return snapshot, err
	}
	snapshot.Videos, err = loadRescreenVideos(ctx, tx, videoSHAs)
	if err != nil {
		return snapshot, err
	}
	if err = tx.Commit(ctx); err != nil {
		return snapshot, fmt.Errorf("phase2: commit rescreener reconciliation: %w", err)
	}
	return snapshot, nil
}

type candidateMemberRow struct {
	FileID int64
	SHA    *string
	Status string
	Score  []byte
}

type candidateGroupRows struct {
	ID      int64
	Kind    string
	Members []candidateMemberRow
}

func loadCandidatePairs(
	ctx context.Context,
	tx pgx.Tx,
) ([]candidatePair, error) {
	rows, err := tx.Query(ctx, `
		SELECT g.id,g.kind,m.file_id,f.sha512,f.status,m.score_json
		FROM dup_groups AS g
		JOIN dup_members AS m ON m.group_id=g.id
		JOIN files AS f ON f.id=m.file_id
		WHERE g.kind IN ('image_candidate','video_candidate')
		ORDER BY g.id,m.file_id`)
	if err != nil {
		return nil, fmt.Errorf("phase2: query candidate score members: %w", err)
	}
	defer rows.Close()
	var groups []candidateGroupRows
	for rows.Next() {
		var (
			groupID int64
			kind    string
			member  candidateMemberRow
		)
		if err := rows.Scan(
			&groupID,
			&kind,
			&member.FileID,
			&member.SHA,
			&member.Status,
			&member.Score,
		); err != nil {
			return nil, fmt.Errorf("phase2: scan candidate score member: %w", err)
		}
		if len(groups) == 0 || groups[len(groups)-1].ID != groupID {
			groups = append(groups, candidateGroupRows{ID: groupID, Kind: kind})
		}
		groups[len(groups)-1].Members = append(
			groups[len(groups)-1].Members,
			member,
		)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("phase2: read candidate score members: %w", err)
	}

	byKey := make(map[PairKey]candidatePair)
	for _, group := range groups {
		pair, usable, err := candidatePairFromGroup(group)
		if err != nil {
			return nil, err
		}
		if !usable {
			continue
		}
		if existing, duplicate := byKey[pair.Key]; duplicate {
			merged, err := mergeCandidateTrace(existing.Trace, pair.Trace)
			if err != nil {
				return nil, fmt.Errorf(
					"phase2: conflicting duplicate candidate %#v: %w",
					pair.Key,
					err,
				)
			}
			existing.Trace = merged
			byKey[pair.Key] = existing
			continue
		}
		byKey[pair.Key] = pair
	}
	pairs := make([]candidatePair, 0, len(byKey))
	for _, pair := range byKey {
		pairs = append(pairs, pair)
	}
	sort.Slice(pairs, func(i, j int) bool {
		keys := []PairKey{pairs[i].Key, pairs[j].Key}
		sortPairKeys(keys)
		return keys[0] == pairs[i].Key && pairs[i].Key != pairs[j].Key
	})
	return pairs, nil
}

func candidatePairFromGroup(
	group candidateGroupRows,
) (candidatePair, bool, error) {
	kind, ok := candidateKindText(group.Kind)
	if !ok {
		return candidatePair{}, false,
			fmt.Errorf("phase2: invalid candidate kind %q", group.Kind)
	}
	live := make([]candidateMemberRow, 0, len(group.Members))
	distinct := make(map[string]struct{})
	for _, member := range group.Members {
		if member.Status == proto.StatusDeleted {
			continue
		}
		if member.SHA == nil || !isCanonicalSHA512(*member.SHA) {
			return candidatePair{}, false, fmt.Errorf(
				"phase2: %s group %d has noncanonical live SHA on file %d",
				group.Kind,
				group.ID,
				member.FileID,
			)
		}
		distinct[*member.SHA] = struct{}{}
		live = append(live, member)
	}
	if len(distinct) != 2 {
		return candidatePair{}, false, nil
	}
	shas := make([]string, 0, 2)
	for sha := range distinct {
		shas = append(shas, sha)
	}
	sort.Strings(shas)
	key := PairKey{Kind: kind, SHAA: shas[0], SHAB: shas[1]}
	var trace firstScreenTrace
	for _, member := range live {
		parsed, present, err := parseCandidateMemberTrace(
			kind,
			*member.SHA,
			key,
			member.Score,
		)
		if err != nil {
			return candidatePair{}, false, fmt.Errorf(
				"phase2: candidate group %d file %d score: %w",
				group.ID,
				member.FileID,
				err,
			)
		}
		if !present {
			continue
		}
		trace, err = mergeCandidateTrace(trace, parsed)
		if err != nil {
			return candidatePair{}, false, fmt.Errorf(
				"phase2: candidate group %d conflicting member scores: %w",
				group.ID,
				err,
			)
		}
	}
	return candidatePair{Key: key, Trace: trace}, true, nil
}

type candidateMemberScore struct {
	Hamming        *int   `json:"hamming"`
	DurationDiffMS *int64 `json:"duration_diff_ms,omitempty"`
	QualitySelf    *int   `json:"quality_self"`
	QualityPeer    *int   `json:"quality_peer"`
	PeerSHA512     string `json:"peer_sha512"`
}

func parseCandidateMemberTrace(
	kind, self string,
	key PairKey,
	raw []byte,
) (firstScreenTrace, bool, error) {
	if len(raw) == 0 {
		return firstScreenTrace{}, false, nil
	}
	var score candidateMemberScore
	if err := decodeStrictJSON(raw, &score); err != nil {
		return firstScreenTrace{}, false, err
	}
	if score.Hamming == nil || score.QualitySelf == nil ||
		score.QualityPeer == nil || !isCanonicalSHA512(score.PeerSHA512) {
		return firstScreenTrace{}, false, fmt.Errorf("missing or invalid trace fields")
	}
	peer := key.SHAA
	if self == key.SHAA {
		peer = key.SHAB
	} else if self != key.SHAB {
		return firstScreenTrace{}, false, fmt.Errorf("member SHA is outside normalized pair")
	}
	if score.PeerSHA512 != peer {
		return firstScreenTrace{}, false, fmt.Errorf("peer SHA does not point to the other endpoint")
	}
	if *score.Hamming < 0 || *score.Hamming > 256 ||
		*score.QualitySelf < 0 || *score.QualitySelf > 100 ||
		*score.QualityPeer < 0 || *score.QualityPeer > 100 {
		return firstScreenTrace{}, false, fmt.Errorf("trace numeric value is out of range")
	}
	if kind == "video" {
		if score.DurationDiffMS == nil || *score.DurationDiffMS < 0 {
			return firstScreenTrace{}, false, fmt.Errorf("video duration trace is missing or invalid")
		}
	} else if score.DurationDiffMS != nil {
		return firstScreenTrace{}, false, fmt.Errorf("image trace contains video duration")
	}
	trace := firstScreenTrace{
		Present:        true,
		Hamming:        *score.Hamming,
		DurationDiffMS: cloneInt64Pointer(score.DurationDiffMS),
	}
	if self == key.SHAA {
		trace.QualityA = *score.QualitySelf
		trace.QualityB = *score.QualityPeer
	} else {
		trace.QualityA = *score.QualityPeer
		trace.QualityB = *score.QualitySelf
	}
	return trace, true, nil
}

func mergeCandidateTrace(
	current, incoming firstScreenTrace,
) (firstScreenTrace, error) {
	switch {
	case !current.Present:
		return cloneFirstScreenTrace(incoming), nil
	case !incoming.Present:
		return cloneFirstScreenTrace(current), nil
	case !equalFirstScreenTrace(current, incoming):
		return firstScreenTrace{}, fmt.Errorf("first-screen trace differs")
	default:
		return cloneFirstScreenTrace(current), nil
	}
}

func deleteStalePairScores(
	ctx context.Context,
	tx pgx.Tx,
	pairs []candidatePair,
) error {
	kinds := make([]string, len(pairs))
	left := make([]string, len(pairs))
	right := make([]string, len(pairs))
	for index, pair := range pairs {
		kinds[index] = pair.Key.Kind
		left[index] = pair.Key.SHAA
		right[index] = pair.Key.SHAB
	}
	if _, err := tx.Exec(ctx, `
		WITH active(kind,sha_a,sha_b) AS (
			SELECT * FROM unnest($1::text[],$2::text[],$3::text[])
		)
		DELETE FROM pair_scores AS score
		WHERE NOT EXISTS (
			SELECT 1 FROM active
			WHERE active.kind=score.kind
			  AND active.sha_a=LEAST(score.sha_a,score.sha_b)
			  AND active.sha_b=GREATEST(score.sha_a,score.sha_b)
		)`,
		kinds,
		left,
		right,
	); err != nil {
		return fmt.Errorf("phase2: delete stale pair scores: %w", err)
	}
	return nil
}

func loadResolvedScores(
	ctx context.Context,
	tx pgx.Tx,
	pairs []candidatePair,
) (map[PairKey]persistedPairScore, error) {
	pairsByKey := make(map[PairKey]candidatePair, len(pairs))
	for _, pair := range pairs {
		pairsByKey[pair.Key] = pair
	}
	rows, err := tx.Query(ctx, `
		SELECT kind,sha_a,sha_b,phase2_json,verdict
		FROM pair_scores
		ORDER BY kind,sha_a,sha_b`)
	if err != nil {
		return nil, fmt.Errorf("phase2: query durable pair scores: %w", err)
	}
	defer rows.Close()
	resolved := make(map[PairKey]persistedPairScore)
	for rows.Next() {
		var (
			key     PairKey
			raw     []byte
			verdict string
		)
		if err := rows.Scan(
			&key.Kind,
			&key.SHAA,
			&key.SHAB,
			&raw,
			&verdict,
		); err != nil {
			return nil, fmt.Errorf("phase2: scan durable pair score: %w", err)
		}
		if err := validatePairKey(key); err != nil {
			return nil, err
		}
		pair, active := pairsByKey[key]
		if !active {
			return nil, fmt.Errorf("phase2: durable score survived without active candidate %#v", key)
		}
		score, err := decodePersistedPairScore(raw, key, verdict, pair.Trace)
		if err != nil {
			return nil, err
		}
		if _, duplicate := resolved[key]; duplicate {
			return nil, fmt.Errorf("phase2: duplicate durable pair score %#v", key)
		}
		resolved[key] = score
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("phase2: read durable pair scores: %w", err)
	}
	return resolved, nil
}

func decodePersistedPairScore(
	raw []byte,
	key PairKey,
	verdict string,
	trace firstScreenTrace,
) (persistedPairScore, error) {
	if len(raw) == 0 {
		return persistedPairScore{}, fmt.Errorf("phase2: pair %#v has null phase2_json", key)
	}
	var document pairScoreDocument
	if err := decodeStrictJSON(raw, &document); err != nil {
		return persistedPairScore{}, fmt.Errorf("phase2: decode pair %#v: %w", key, err)
	}
	if err := validatePairScoreJSONShape(raw, key.Kind); err != nil {
		return persistedPairScore{}, fmt.Errorf(
			"phase2: pair %#v document shape: %w",
			key,
			err,
		)
	}
	score := persistedPairScore{Key: key, Verdict: verdict, Document: document}
	if err := validatePersistedPairScore(score, trace); err != nil {
		return persistedPairScore{}, err
	}
	return score, nil
}

func auditAllPairScores(
	ctx context.Context,
	tx pgx.Tx,
) error {
	rows, err := tx.Query(ctx, `
		SELECT kind,sha_a,sha_b,phase2_json,verdict
		FROM pair_scores
		ORDER BY kind,sha_a,sha_b`)
	if err != nil {
		return fmt.Errorf("phase2: query pair score audit: %w", err)
	}
	defer rows.Close()
	seen := make(map[PairKey]struct{})
	for rows.Next() {
		var (
			key     PairKey
			raw     []byte
			verdict string
		)
		if err := rows.Scan(
			&key.Kind,
			&key.SHAA,
			&key.SHAB,
			&raw,
			&verdict,
		); err != nil {
			return fmt.Errorf("phase2: scan pair score audit: %w", err)
		}
		if err := validatePairKey(key); err != nil {
			return err
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("phase2: duplicate durable pair score %#v", key)
		}
		seen[key] = struct{}{}
		var document pairScoreDocument
		if err := decodeStrictJSON(raw, &document); err != nil {
			return fmt.Errorf("phase2: decode pair %#v: %w", key, err)
		}
		if err := validatePairScoreJSONShape(raw, key.Kind); err != nil {
			return fmt.Errorf(
				"phase2: pair %#v document shape: %w",
				key,
				err,
			)
		}
		trace, err := traceFromScoreDocument(key.Kind, document.FirstScreen)
		if err != nil {
			return fmt.Errorf("phase2: pair %#v trace: %w", key, err)
		}
		score := persistedPairScore{
			Key:      key,
			Verdict:  verdict,
			Document: document,
		}
		if err := validatePersistedPairScore(score, trace); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("phase2: read pair score audit: %w", err)
	}
	return nil
}

func traceFromScoreDocument(
	kind string,
	document *firstScreenScoreDocument,
) (firstScreenTrace, error) {
	if document == nil {
		return firstScreenTrace{}, nil
	}
	if document.Hamming < 0 || document.Hamming > 256 ||
		document.QualityA < 0 || document.QualityA > 100 ||
		document.QualityB < 0 || document.QualityB > 100 {
		return firstScreenTrace{}, fmt.Errorf("numeric value is out of range")
	}
	switch kind {
	case "image":
		if document.DurationDiffMS != nil {
			return firstScreenTrace{}, fmt.Errorf("image trace contains video duration")
		}
	case "video":
		if document.DurationDiffMS == nil || *document.DurationDiffMS < 0 {
			return firstScreenTrace{}, fmt.Errorf("video duration is missing or invalid")
		}
	default:
		return firstScreenTrace{}, fmt.Errorf("invalid pair kind %q", kind)
	}
	return firstScreenTrace{
		Present:        true,
		Hamming:        document.Hamming,
		DurationDiffMS: cloneInt64Pointer(document.DurationDiffMS),
		QualityA:       document.QualityA,
		QualityB:       document.QualityB,
	}, nil
}

func validatePairScoreJSONShape(raw []byte, kind string) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return err
	}
	required := []string{"version", "kind", "verdict"}
	branch := "image"
	if kind == "video" {
		branch = "video"
	}
	required = append(required, branch)
	if err := requireJSONKeys(root, required...); err != nil {
		return err
	}
	if rawTrace, present := root["first_screen"]; present {
		var trace map[string]json.RawMessage
		if err := json.Unmarshal(rawTrace, &trace); err != nil {
			return fmt.Errorf("first_screen: %w", err)
		}
		traceKeys := []string{"hamming", "quality_a", "quality_b"}
		if kind == "video" {
			traceKeys = append(traceKeys, "duration_diff_ms")
		} else if _, unexpected := trace["duration_diff_ms"]; unexpected {
			return fmt.Errorf("image first_screen contains duration_diff_ms")
		}
		if err := requireJSONKeys(trace, traceKeys...); err != nil {
			return fmt.Errorf("first_screen: %w", err)
		}
	}
	var branchObject map[string]json.RawMessage
	if err := json.Unmarshal(root[branch], &branchObject); err != nil {
		return fmt.Errorf("%s: %w", branch, err)
	}
	if kind == "image" {
		if err := requireJSONKeys(
			branchObject,
			"phash_evaluated",
			"sobel_evaluated",
		); err != nil {
			return fmt.Errorf("image: %w", err)
		}
		return nil
	}
	if err := requireJSONKeys(
		branchObject,
		"valid_frames",
		"passed_frames",
		"average_evaluated",
		"frames",
	); err != nil {
		return fmt.Errorf("video: %w", err)
	}
	var frames []map[string]json.RawMessage
	if err := json.Unmarshal(branchObject["frames"], &frames); err != nil {
		return fmt.Errorf("video frames: %w", err)
	}
	for index, frame := range frames {
		if err := requireJSONKeys(
			frame,
			"frame_idx",
			"valid",
			"sobel_evaluated",
			"passed",
		); err != nil {
			return fmt.Errorf("video frame %d: %w", index, err)
		}
	}
	return nil
}

func requireJSONKeys(
	object map[string]json.RawMessage,
	keys ...string,
) error {
	if object == nil {
		return fmt.Errorf("expected object")
	}
	for _, key := range keys {
		raw, exists := object[key]
		if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("missing required field %q", key)
		}
	}
	return nil
}

func validatePersistedPairScore(
	score persistedPairScore,
	trace firstScreenTrace,
) error {
	if err := validatePairKey(score.Key); err != nil {
		return err
	}
	if score.Verdict != "yes" && score.Verdict != "no" &&
		score.Verdict != "inconclusive" {
		return fmt.Errorf("phase2: pair %#v has invalid verdict %q", score.Key, score.Verdict)
	}
	document := score.Document
	if document.Version != pairScoreVersion ||
		document.Kind != score.Key.Kind ||
		document.Verdict != score.Verdict {
		return fmt.Errorf("phase2: pair %#v document identity mismatch", score.Key)
	}
	if !equalTraceDocument(trace, document.FirstScreen) {
		return fmt.Errorf("phase2: pair %#v first-screen trace mismatch", score.Key)
	}
	switch score.Key.Kind {
	case "image":
		if document.Image == nil || document.Video != nil {
			return fmt.Errorf("phase2: image pair %#v has invalid document branch", score.Key)
		}
		return validateImageScoreDocument(score.Verdict, document.Image)
	case "video":
		if document.Video == nil || document.Image != nil {
			return fmt.Errorf("phase2: video pair %#v has invalid document branch", score.Key)
		}
		return validateVideoScoreDocument(score.Verdict, document.Video)
	default:
		return fmt.Errorf("phase2: pair %#v has invalid kind", score.Key)
	}
}

func validateImageScoreDocument(
	verdict string,
	document *imageScoreDocument,
) error {
	if document.PHashEvaluated != (document.PHashPassRatio != nil) ||
		document.SobelEvaluated != (document.SobelCosine != nil) {
		return fmt.Errorf("phase2: image score evaluation-state mismatch")
	}
	if document.PHashPassRatio != nil &&
		!finiteInRange(*document.PHashPassRatio, 0, 1) {
		return fmt.Errorf("phase2: image pHash ratio is invalid")
	}
	if document.SobelCosine != nil &&
		!finiteInRange(*document.SobelCosine, -1, 1) {
		return fmt.Errorf("phase2: image Sobel cosine is invalid")
	}
	if verdict == "inconclusive" {
		if document.PHashEvaluated || document.SobelEvaluated {
			return fmt.Errorf("phase2: inconclusive image publishes evaluated thresholds")
		}
		return nil
	}
	if !document.PHashEvaluated {
		return fmt.Errorf("phase2: conclusive image lacks pHash evaluation")
	}
	if verdict == "yes" && !document.SobelEvaluated {
		return fmt.Errorf("phase2: yes image lacks Sobel evaluation")
	}
	return nil
}

func validateVideoScoreDocument(
	verdict string,
	document *videoScoreDocument,
) error {
	if len(document.Frames) != 6 ||
		document.ValidFrames < 0 || document.ValidFrames > 6 ||
		document.PassedFrames < 0 || document.PassedFrames > document.ValidFrames ||
		document.AverageEvaluated != (document.Average != nil) {
		return fmt.Errorf("phase2: video aggregate state is invalid")
	}
	if document.Average != nil && !finiteInRange(*document.Average, -1, 1) {
		return fmt.Errorf("phase2: video average is invalid")
	}
	valid, passed := 0, 0
	for index, frame := range document.Frames {
		if frame.FrameIdx != index ||
			frame.Valid != (frame.PHashPassRatio != nil) ||
			frame.Valid != (frame.Similarity != nil) ||
			frame.SobelEvaluated != (frame.SobelCosine != nil) {
			return fmt.Errorf("phase2: video frame %d evaluation-state mismatch", index)
		}
		if !frame.Valid && (frame.SobelEvaluated || frame.Passed) {
			return fmt.Errorf("phase2: invalid video frame %d carries evaluated state", index)
		}
		if frame.PHashPassRatio != nil &&
			!finiteInRange(*frame.PHashPassRatio, 0, 1) {
			return fmt.Errorf("phase2: video frame %d pHash ratio invalid", index)
		}
		if frame.SobelCosine != nil &&
			!finiteInRange(*frame.SobelCosine, -1, 1) {
			return fmt.Errorf("phase2: video frame %d Sobel cosine invalid", index)
		}
		if frame.Similarity != nil &&
			!finiteInRange(*frame.Similarity, -1, 1) {
			return fmt.Errorf("phase2: video frame %d similarity invalid", index)
		}
		if frame.Passed && !frame.SobelEvaluated {
			return fmt.Errorf("phase2: video frame %d passed without Sobel", index)
		}
		if frame.Valid {
			valid++
		}
		if frame.Passed {
			passed++
		}
	}
	if valid != document.ValidFrames || passed != document.PassedFrames {
		return fmt.Errorf("phase2: video aggregate counts do not match frames")
	}
	if verdict == "inconclusive" && document.AverageEvaluated {
		return fmt.Errorf("phase2: inconclusive video publishes final average")
	}
	if verdict != "inconclusive" && !document.AverageEvaluated {
		return fmt.Errorf("phase2: conclusive video lacks final average")
	}
	return nil
}

func unresolvedEndpointSHAs(
	pairs []candidatePair,
	resolved map[PairKey]persistedPairScore,
) ([]string, []string) {
	imageSet := make(map[string]struct{})
	videoSet := make(map[string]struct{})
	for _, pair := range pairs {
		if _, done := resolved[pair.Key]; done {
			continue
		}
		target := imageSet
		if pair.Key.Kind == "video" {
			target = videoSet
		}
		target[pair.Key.SHAA] = struct{}{}
		target[pair.Key.SHAB] = struct{}{}
	}
	return sortedSet(imageSet), sortedSet(videoSet)
}

func loadRescreenImages(
	ctx context.Context,
	tx pgx.Tx,
	shas []string,
) (map[string]imageFeatureCache, error) {
	result := make(map[string]imageFeatureCache)
	if len(shas) == 0 {
		return result, nil
	}
	relevant := stringSet(shas)
	rows, err := tx.Query(ctx, `
		SELECT sha512,phash_parts,sobel_hist
		FROM image_features
		WHERE sha512=ANY($1::text[])
		ORDER BY sha512`,
		shas,
	)
	if err != nil {
		return nil, fmt.Errorf("phase2: query rescreener image features: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			sha      string
			pHashRaw []byte
			sobelRaw []byte
		)
		if err := rows.Scan(&sha, &pHashRaw, &sobelRaw); err != nil {
			return nil, fmt.Errorf("phase2: scan rescreener image feature: %w", err)
		}
		if !isCanonicalSHA512(sha) {
			return nil, fmt.Errorf("phase2: image feature has noncanonical SHA %q", sha)
		}
		if _, expected := relevant[sha]; !expected {
			return nil, fmt.Errorf("phase2: image feature returned unrelated SHA %q", sha)
		}
		var feature imageFeatureCache
		if len(pHashRaw) != 0 {
			parts, err := features.DecodePHashParts(pHashRaw)
			if err != nil {
				return nil, fmt.Errorf("phase2: decode durable image pHash %s: %w", sha, err)
			}
			feature.PHashParts = &parts
			feature.PHashRaw = append([]byte(nil), pHashRaw...)
		}
		if len(sobelRaw) != 0 {
			hist, err := features.DecodeSobelHist(sobelRaw)
			if err != nil {
				return nil, fmt.Errorf("phase2: decode durable image Sobel %s: %w", sha, err)
			}
			feature.SobelHist = &hist
			feature.SobelRaw = append([]byte(nil), sobelRaw...)
		}
		if !imageFeatureEmpty(feature) {
			result[sha] = feature
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("phase2: read rescreener image features: %w", err)
	}
	return result, nil
}

func loadRescreenVideos(
	ctx context.Context,
	tx pgx.Tx,
	shas []string,
) (map[string]videoFeatureCache, error) {
	result := make(map[string]videoFeatureCache)
	if len(shas) == 0 {
		return result, nil
	}
	relevant := stringSet(shas)
	rows, err := tx.Query(ctx, `
		SELECT sha512,frame_idx,pdq256,phash_parts,sobel_hist
		FROM video_frames
		WHERE sha512=ANY($1::text[])
		ORDER BY sha512,frame_idx`,
		shas,
	)
	if err != nil {
		return nil, fmt.Errorf("phase2: query rescreener video frames: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			sha        string
			frameIndex int
			pdqRaw     []byte
			pHashRaw   []byte
			sobelRaw   []byte
		)
		if err := rows.Scan(&sha, &frameIndex, &pdqRaw, &pHashRaw, &sobelRaw); err != nil {
			return nil, fmt.Errorf("phase2: scan rescreener video frame: %w", err)
		}
		if !isCanonicalSHA512(sha) {
			return nil, fmt.Errorf("phase2: video frame has noncanonical SHA %q", sha)
		}
		if _, expected := relevant[sha]; !expected {
			return nil, fmt.Errorf("phase2: video frame returned unrelated SHA %q", sha)
		}
		if frameIndex < 0 || frameIndex >= 6 {
			return nil, fmt.Errorf("phase2: durable video frame index %d invalid", frameIndex)
		}
		feature := result[sha]
		frame := &feature.Frames[frameIndex]
		if len(pdqRaw) != 0 {
			if len(pdqRaw) != 32 {
				return nil, fmt.Errorf("phase2: durable video PDQ length %d", len(pdqRaw))
			}
			var pdq [32]byte
			copy(pdq[:], pdqRaw)
			frame.PDQ256 = &pdq
			frame.PDQRaw = append([]byte(nil), pdqRaw...)
		}
		if len(pHashRaw) != 0 {
			parts, err := features.DecodePHashParts(pHashRaw)
			if err != nil {
				return nil, fmt.Errorf("phase2: decode durable frame pHash %s#%d: %w", sha, frameIndex, err)
			}
			frame.PHashParts = &parts
			frame.PHashRaw = append([]byte(nil), pHashRaw...)
		}
		if len(sobelRaw) != 0 {
			hist, err := features.DecodeSobelHist(sobelRaw)
			if err != nil {
				return nil, fmt.Errorf("phase2: decode durable frame Sobel %s#%d: %w", sha, frameIndex, err)
			}
			frame.SobelHist = &hist
			frame.SobelRaw = append([]byte(nil), sobelRaw...)
		}
		result[sha] = feature
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("phase2: read rescreener video frames: %w", err)
	}
	return result, nil
}

func (store *postgresRescreenStore) upsertScore(
	ctx context.Context,
	score persistedPairScore,
) (persistedPairScore, error) {
	if store.pool == nil {
		return persistedPairScore{}, fmt.Errorf("phase2: PostgreSQL pool is nil")
	}
	trace := firstScreenTrace{}
	if score.Document.FirstScreen != nil {
		trace = firstScreenTrace{
			Present:        true,
			Hamming:        score.Document.FirstScreen.Hamming,
			DurationDiffMS: cloneInt64Pointer(score.Document.FirstScreen.DurationDiffMS),
			QualityA:       score.Document.FirstScreen.QualityA,
			QualityB:       score.Document.FirstScreen.QualityB,
		}
	}
	if err := validatePersistedPairScore(score, trace); err != nil {
		return persistedPairScore{}, err
	}
	raw, err := json.Marshal(score.Document)
	if err != nil {
		return persistedPairScore{}, fmt.Errorf("phase2: marshal pair score: %w", err)
	}
	var (
		returnedRaw     []byte
		returnedVerdict string
	)
	err = store.pool.QueryRow(ctx, `
		INSERT INTO pair_scores(kind,sha_a,sha_b,phase2_json,verdict)
		VALUES($1,$2,$3,$4::jsonb,$5)
		ON CONFLICT(kind,sha_a,sha_b) DO UPDATE SET
			phase2_json=pair_scores.phase2_json,
			verdict=pair_scores.verdict,
			created_at=pair_scores.created_at
		WHERE pair_scores.phase2_json=EXCLUDED.phase2_json
		  AND pair_scores.verdict=EXCLUDED.verdict
		RETURNING phase2_json,verdict`,
		score.Key.Kind,
		score.Key.SHAA,
		score.Key.SHAB,
		raw,
		score.Verdict,
	).Scan(&returnedRaw, &returnedVerdict)
	if errors.Is(err, pgx.ErrNoRows) {
		return persistedPairScore{}, ErrPairScoreConflict
	}
	if err != nil {
		return persistedPairScore{}, fmt.Errorf("phase2: upsert pair score: %w", err)
	}
	returned, err := decodePersistedPairScore(
		returnedRaw,
		score.Key,
		returnedVerdict,
		trace,
	)
	if err != nil {
		return persistedPairScore{}, err
	}
	if !reflect.DeepEqual(returned, score) {
		return persistedPairScore{}, ErrPairScoreConflict
	}
	return returned, nil
}

func (store *postgresRescreenStore) hasRelevantActivePhase2(
	ctx context.Context,
	unresolved []PairKey,
) (bool, error) {
	relevant := make(map[endpointKey]struct{}, len(unresolved)*2)
	for _, key := range unresolved {
		if err := validatePairKey(key); err != nil {
			return false, err
		}
		relevant[endpointKey{Kind: key.Kind, SHA: key.SHAA}] = struct{}{}
		relevant[endpointKey{Kind: key.Kind, SHA: key.SHAB}] = struct{}{}
	}
	if len(relevant) == 0 {
		return false, nil
	}
	rows, err := store.pool.Query(ctx, `
		SELECT id,machine_id,phase,target
		FROM scan_tasks
		WHERE status IN ('sent','acked','running')
		ORDER BY id`)
	if err != nil {
		return false, fmt.Errorf("phase2: query active task barrier: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id        string
			machineID string
			phase     int
			raw       []byte
		)
		if err := rows.Scan(&id, &machineID, &phase, &raw); err != nil {
			return false, fmt.Errorf("phase2: scan active task barrier: %w", err)
		}
		var discriminator map[string]json.RawMessage
		if err := json.Unmarshal(raw, &discriminator); err != nil {
			return false, fmt.Errorf("phase2: active task %s target JSON: %w", id, err)
		}
		rawType, hasType := discriminator["type"]
		if !hasType {
			continue
		}
		var targetType string
		if err := json.Unmarshal(rawType, &targetType); err != nil {
			return false, fmt.Errorf("phase2: active task %s target type: %w", id, err)
		}
		if targetType == "scan" {
			continue
		}
		if targetType != phase2TargetType || phase != 2 {
			return false, fmt.Errorf(
				"phase2: active task %s has invalid discriminator/phase",
				id,
			)
		}
		var target phase2Target
		if err := decodeStrictJSON(raw, &target); err != nil {
			return false, fmt.Errorf("phase2: active task %s target: %w", id, err)
		}
		envelope := RoutedTask{MachineID: machineID, Task: target.Task}
		if id != target.Task.TaskID {
			return false, fmt.Errorf("phase2: active task %s target task ID mismatch", id)
		}
		if err := validateRestoredTarget(target, envelope); err != nil {
			return false, fmt.Errorf("phase2: active task %s: %w", id, err)
		}
		for _, item := range target.Task.Items {
			kind, ok := protocolKindText(item.Kind)
			if !ok {
				return false, fmt.Errorf("phase2: active task %s has invalid item kind", id)
			}
			if _, matches := relevant[endpointKey{Kind: kind, SHA: item.SHA512}]; matches {
				return true, nil
			}
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("phase2: read active task barrier: %w", err)
	}
	return false, nil
}

func candidateKindText(kind string) (string, bool) {
	switch kind {
	case candidateImage:
		return "image", true
	case candidateVideo:
		return "video", true
	default:
		return "", false
	}
}

func protocolKindText(kind uint8) (string, bool) {
	switch kind {
	case proto.KindImage:
		return "image", true
	case proto.KindVideo:
		return "video", true
	default:
		return "", false
	}
}

func decodeStrictJSON(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func finiteInRange(value, minimum, maximum float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) &&
		value >= minimum && value <= maximum
}

func equalFirstScreenTrace(left, right firstScreenTrace) bool {
	if left.Present != right.Present ||
		left.Hamming != right.Hamming ||
		left.QualityA != right.QualityA ||
		left.QualityB != right.QualityB {
		return false
	}
	if left.DurationDiffMS == nil || right.DurationDiffMS == nil {
		return left.DurationDiffMS == nil && right.DurationDiffMS == nil
	}
	return *left.DurationDiffMS == *right.DurationDiffMS
}

func equalTraceDocument(
	trace firstScreenTrace,
	document *firstScreenScoreDocument,
) bool {
	if !trace.Present {
		return document == nil
	}
	if document == nil {
		return false
	}
	return equalFirstScreenTrace(trace, firstScreenTrace{
		Present:        true,
		Hamming:        document.Hamming,
		DurationDiffMS: document.DurationDiffMS,
		QualityA:       document.QualityA,
		QualityB:       document.QualityB,
	})
}

func cloneFirstScreenTrace(trace firstScreenTrace) firstScreenTrace {
	trace.DurationDiffMS = cloneInt64Pointer(trace.DurationDiffMS)
	return trace
}

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}
