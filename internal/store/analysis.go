package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"dedup/internal/features"
	"dedup/internal/proto"
)

var ErrStale = errors.New("store: stale analysis result")

type AnalysisResult struct {
	MachineID string
	Path      string
	Kind      MediaKind
	Size      int64
	MTime     int64
	SHA512    []byte

	RequestedFields uint32
	FieldsDone      uint32
	RequestedFrames uint8

	PDQ     []byte
	Quality int32
	Width   int32
	Height  int32

	DurationMS   *int64
	ThumbPath    string
	ThumbPDQ     []byte
	ThumbQuality *int32
	ThumbWidth   *int32
	ThumbHeight  *int32

	PHashParts []byte
	SobelHist  []byte
	Frames     []Phase2Frame
	Errors     []FieldError
}

type CommittedState struct {
	FieldsPresent uint32
	MissingFields uint32
	FramesPresent uint8
	MissingFrames uint8
}

func (d *DB) SaveAnalysis(ctx context.Context, result AnalysisResult) (CommittedState, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return CommittedState{}, err
	}
	defer tx.Rollback()

	var fileID int64
	var storedSize, storedMTime int64
	var storedSHA sql.NullString
	var priorMissing uint32
	var priorError sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT id, size, mtime, sha512, missing_mask, error
		FROM files WHERE machine_id=?1 AND path=?2`,
		result.MachineID,
		result.Path,
	).Scan(&fileID, &storedSize, &storedMTime, &storedSHA, &priorMissing, &priorError)
	if err != nil {
		if err == sql.ErrNoRows {
			return CommittedState{}, fmt.Errorf("%w: %s", ErrStale, result.Path)
		}
		return CommittedState{}, fmt.Errorf("store: load analysis file: %w", err)
	}
	if storedSize != result.Size || storedMTime != result.MTime {
		return CommittedState{}, fmt.Errorf("%w: %s", ErrStale, result.Path)
	}
	sha, err := encodeSHA512(result.SHA512)
	if err != nil {
		return CommittedState{}, err
	}
	if storedSHA.Valid && storedSHA.String != sha {
		return CommittedState{}, fmt.Errorf("%w: %s", ErrStale, result.Path)
	}

	requestedFrames, err := validateAnalysisResult(result)
	if err != nil {
		return CommittedState{}, err
	}

	now := time.Now().Unix()
	changedFeature := false
	changedFrames := make([]int, 0, len(result.Frames))
	switch result.Kind {
	case MediaImage:
		if result.FieldsDone&proto.FieldPDQ256 != 0 {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO image_features
					(sha512, width, height, pdq256, pdq_quality)
				VALUES (?1, ?2, ?3, ?4, ?5)
				ON CONFLICT (sha512) DO UPDATE SET
					width=excluded.width,
					height=excluded.height,
					pdq256=excluded.pdq256,
					pdq_quality=excluded.pdq_quality;`,
				sha, result.Width, result.Height, result.PDQ, result.Quality,
			); err != nil {
				return CommittedState{}, fmt.Errorf("store: save analysis image PDQ: %w", err)
			}
			changedFeature = true
		}
		if result.FieldsDone&proto.FieldPHashParts != 0 {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO image_features (sha512, phash_parts) VALUES (?1, ?2)
				ON CONFLICT (sha512) DO UPDATE SET phash_parts=excluded.phash_parts;`,
				sha, result.PHashParts,
			); err != nil {
				return CommittedState{}, fmt.Errorf("store: save analysis image pHash: %w", err)
			}
			changedFeature = true
		}
		if result.FieldsDone&proto.FieldSobelHist != 0 {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO image_features (sha512, sobel_hist) VALUES (?1, ?2)
				ON CONFLICT (sha512) DO UPDATE SET sobel_hist=excluded.sobel_hist;`,
				sha, result.SobelHist,
			); err != nil {
				return CommittedState{}, fmt.Errorf("store: save analysis image Sobel: %w", err)
			}
			changedFeature = true
		}
	case MediaVideo:
		if result.FieldsDone&(proto.FieldVideoDuration|proto.FieldThumb) != 0 {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO video_features (sha512, duration_ms) VALUES (?1, ?2)
				ON CONFLICT (sha512) DO UPDATE SET duration_ms=excluded.duration_ms;`,
				sha, *result.DurationMS,
			); err != nil {
				return CommittedState{}, fmt.Errorf("store: save analysis video duration: %w", err)
			}
			changedFeature = true
		}
		if result.FieldsDone&(proto.FieldVideoContactSheet|proto.FieldThumb) != 0 {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO video_features
					(sha512, thumb_path, thumb_pdq256, thumb_quality, thumb_width, thumb_height)
				VALUES (?1, ?2, ?3, ?4, ?5, ?6)
				ON CONFLICT (sha512) DO UPDATE SET
					thumb_path=excluded.thumb_path,
					thumb_pdq256=excluded.thumb_pdq256,
					thumb_quality=excluded.thumb_quality,
					thumb_width=excluded.thumb_width,
					thumb_height=excluded.thumb_height;`,
				sha,
				result.ThumbPath,
				result.ThumbPDQ,
				*result.ThumbQuality,
				*result.ThumbWidth,
				*result.ThumbHeight,
			); err != nil {
				return CommittedState{}, fmt.Errorf("store: save analysis contact sheet: %w", err)
			}
			changedFeature = true
		}
		for _, frame := range result.Frames {
			if frame.Error != "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO video_frames (sha512, frame_idx, pdq256, phash_parts, sobel_hist)
				VALUES (?1, ?2, ?3, ?4, ?5)
				ON CONFLICT (sha512, frame_idx) DO UPDATE SET
					pdq256=excluded.pdq256,
					phash_parts=excluded.phash_parts,
					sobel_hist=excluded.sobel_hist;`,
				sha, frame.FrameIdx, frame.PDQ256, frame.PHashParts, frame.SobelHist,
			); err != nil {
				return CommittedState{}, fmt.Errorf(
					"store: save analysis video frame %d: %w", frame.FrameIdx, err,
				)
			}
			changedFrames = append(changedFrames, frame.FrameIdx)
		}
	}

	state, err := committedAnalysisState(
		ctx, tx, result.Kind, sha, result.RequestedFields, requestedFrames,
	)
	if err != nil {
		return CommittedState{}, err
	}
	updatedMissing := priorMissing&^result.RequestedFields | state.MissingFields
	phase1Done := analysisPhase1Done(result.Kind, updatedMissing)
	phase2Done := updatedMissing&phase2Mask(result.Kind) == 0
	status := proto.StatusPartial
	if updatedMissing == 0 {
		status = proto.StatusDone
	} else if state.FieldsPresent == 0 && state.FramesPresent == 0 &&
		(len(result.Errors) != 0 || hasAnalysisFrameErrors(result.Frames)) {
		status = proto.StatusFailed
	}
	var errorValue any
	if status != proto.StatusDone {
		if summary := analysisErrorText(result); summary != "" {
			errorValue = summary
		} else if priorError.Valid {
			errorValue = priorError.String
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE files SET sha512=?1, status=?2, error=?3, missing_mask=?4,
			phase1_done=?5, phase2_done=?6, updated_at=?7
		WHERE id=?8`,
		sha,
		status,
		errorValue,
		updatedMissing,
		boolToInt(phase1Done),
		boolToInt(phase2Done),
		now,
		fileID,
	); err != nil {
		return CommittedState{}, fmt.Errorf("store: update analysis file: %w", err)
	}

	if err := enqueuePhase1Sync(ctx, tx, "files", fmt.Sprint(fileID), now); err != nil {
		return CommittedState{}, err
	}
	if changedFeature {
		table := "image_features"
		if result.Kind == MediaVideo {
			table = "video_features"
		}
		if err := enqueuePhase1Sync(ctx, tx, table, sha, now); err != nil {
			return CommittedState{}, err
		}
	}
	for _, frameIdx := range changedFrames {
		if err := enqueuePhase1Sync(
			ctx, tx, "video_frames", fmt.Sprintf("%s:%d", sha, frameIdx), now,
		); err != nil {
			return CommittedState{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return CommittedState{}, err
	}
	return state, nil
}

func validateAnalysisResult(result AnalysisResult) (uint8, error) {
	allowed := contentFieldMask(result.Kind)
	if allowed == 0 {
		return 0, fmt.Errorf("store: invalid analysis media kind %q", result.Kind)
	}
	if result.RequestedFields&^allowed != 0 {
		return 0, fmt.Errorf("store: requested analysis fields exceed media mask")
	}
	if result.FieldsDone&^result.RequestedFields != 0 {
		return 0, fmt.Errorf("store: completed analysis fields were not requested")
	}
	if result.RequestedFrames&^proto.FrameMaskFull != 0 {
		return 0, fmt.Errorf("store: requested analysis frames exceed six slots")
	}
	requestedFrames := result.RequestedFrames
	if result.Kind == MediaVideo && result.RequestedFields&proto.FieldVideo6F != 0 && requestedFrames == 0 {
		requestedFrames = proto.FrameMaskFull
	}
	for _, fieldError := range result.Errors {
		if fieldError.Field == 0 {
			continue
		}
		if fieldError.Field&(fieldError.Field-1) != 0 ||
			fieldError.Field&result.RequestedFields == 0 ||
			fieldError.Field&result.FieldsDone != 0 {
			return 0, fmt.Errorf("store: invalid analysis error field %#x", fieldError.Field)
		}
	}

	switch result.Kind {
	case MediaImage:
		if requestedFrames != 0 || len(result.Frames) != 0 {
			return 0, fmt.Errorf("store: image analysis contains video frames")
		}
		if err := validateAnalysisPDQ(result); err != nil {
			return 0, err
		}
		if err := validateAnalysisBlob(
			proto.FieldPHashParts,
			result.FieldsDone,
			result.PHashParts,
			func(blob []byte) error { _, err := features.DecodePHashParts(blob); return err },
			"phash_parts",
		); err != nil {
			return 0, err
		}
		if err := validateAnalysisBlob(
			proto.FieldSobelHist,
			result.FieldsDone,
			result.SobelHist,
			func(blob []byte) error { _, err := features.DecodeSobelHist(blob); return err },
			"sobel_hist",
		); err != nil {
			return 0, err
		}
		if result.DurationMS != nil || result.ThumbPath != "" || len(result.ThumbPDQ) != 0 ||
			result.ThumbQuality != nil || result.ThumbWidth != nil || result.ThumbHeight != nil {
			return 0, fmt.Errorf("store: image analysis contains video payload")
		}
	case MediaVideo:
		if len(result.PDQ) != 0 || result.Quality != 0 || result.Width != 0 || result.Height != 0 ||
			len(result.PHashParts) != 0 || len(result.SobelHist) != 0 {
			return 0, fmt.Errorf("store: video analysis contains image payload")
		}
		durationDone := result.FieldsDone&(proto.FieldVideoDuration|proto.FieldThumb) != 0
		if durationDone {
			if result.DurationMS == nil || *result.DurationMS < 0 {
				return 0, fmt.Errorf("store: invalid analysis video duration")
			}
		} else if result.DurationMS != nil {
			return 0, fmt.Errorf("store: unclaimed analysis video duration")
		}
		contactDone := result.FieldsDone&(proto.FieldVideoContactSheet|proto.FieldThumb) != 0
		if contactDone {
			if result.ThumbPath == "" || len(result.ThumbPDQ) != 32 ||
				result.ThumbQuality == nil || *result.ThumbQuality < 0 || *result.ThumbQuality > 100 ||
				result.ThumbWidth == nil || *result.ThumbWidth <= 0 ||
				result.ThumbHeight == nil || *result.ThumbHeight <= 0 {
				return 0, fmt.Errorf("store: invalid analysis contact sheet payload")
			}
		} else if result.ThumbPath != "" || len(result.ThumbPDQ) != 0 ||
			result.ThumbQuality != nil || result.ThumbWidth != nil || result.ThumbHeight != nil {
			return 0, fmt.Errorf("store: unclaimed analysis contact sheet payload")
		}
		if err := validateAnalysisFrames(result.Frames, requestedFrames); err != nil {
			return 0, err
		}
	}
	return requestedFrames, nil
}

func validateAnalysisPDQ(result AnalysisResult) error {
	if result.FieldsDone&proto.FieldPDQ256 == 0 {
		if len(result.PDQ) != 0 || result.Quality != 0 || result.Width != 0 || result.Height != 0 {
			return fmt.Errorf("store: unclaimed analysis image PDQ payload")
		}
		return nil
	}
	if len(result.PDQ) != 32 || result.Quality < 0 || result.Quality > 100 ||
		result.Width <= 0 || result.Height <= 0 {
		return fmt.Errorf("store: invalid analysis image PDQ payload")
	}
	return nil
}

func validateAnalysisBlob(
	field uint32,
	done uint32,
	blob []byte,
	decode func([]byte) error,
	name string,
) error {
	if done&field == 0 {
		if len(blob) != 0 {
			return fmt.Errorf("store: unclaimed analysis %s payload", name)
		}
		return nil
	}
	if err := decode(blob); err != nil {
		return fmt.Errorf("store: invalid analysis %s: %w", name, err)
	}
	return nil
}

func validateAnalysisFrames(frames []Phase2Frame, requested uint8) error {
	var seen uint8
	for _, frame := range frames {
		if frame.FrameIdx < 0 || frame.FrameIdx > 5 {
			return fmt.Errorf("store: analysis frame index %d out of range", frame.FrameIdx)
		}
		bit := uint8(1 << uint(frame.FrameIdx))
		if seen&bit != 0 {
			return fmt.Errorf("store: duplicate analysis frame index %d", frame.FrameIdx)
		}
		seen |= bit
		if requested&bit == 0 {
			return fmt.Errorf("store: analysis frame %d was not requested", frame.FrameIdx)
		}
		if frame.Error != "" {
			if len(frame.PDQ256) != 0 || frame.Quality != 0 ||
				len(frame.PHashParts) != 0 || len(frame.SobelHist) != 0 {
				return fmt.Errorf("store: failed analysis frame %d contains payload", frame.FrameIdx)
			}
			continue
		}
		if len(frame.PDQ256) != 32 || frame.Quality < 0 || frame.Quality > 100 {
			return fmt.Errorf("store: invalid analysis frame %d PDQ payload", frame.FrameIdx)
		}
		if _, err := features.DecodePHashParts(frame.PHashParts); err != nil {
			return fmt.Errorf("store: invalid analysis frame %d phash_parts: %w", frame.FrameIdx, err)
		}
		if _, err := features.DecodeSobelHist(frame.SobelHist); err != nil {
			return fmt.Errorf("store: invalid analysis frame %d sobel_hist: %w", frame.FrameIdx, err)
		}
	}
	return nil
}

func committedAnalysisState(
	ctx context.Context,
	tx *sql.Tx,
	kind MediaKind,
	sha string,
	requestedFields uint32,
	requestedFrames uint8,
) (CommittedState, error) {
	state := CommittedState{
		MissingFields: requestedFields,
		MissingFrames: requestedFrames,
	}
	if requestedFields&proto.FieldSHA512 != 0 {
		state.FieldsPresent |= proto.FieldSHA512
		state.MissingFields &^= proto.FieldSHA512
	}
	switch kind {
	case MediaImage:
		var width, height, quality int32
		var pdq, pHash, sobel []byte
		err := tx.QueryRowContext(ctx, `
			SELECT width, height, pdq256, pdq_quality, phash_parts, sobel_hist
			FROM image_features WHERE sha512=?1`, sha,
		).Scan(&width, &height, &pdq, &quality, &pHash, &sobel)
		if err != nil && err != sql.ErrNoRows {
			return CommittedState{}, fmt.Errorf("store: load committed analysis image: %w", err)
		}
		if err == nil {
			if requestedFields&proto.FieldPDQ256 != 0 && len(pdq) == 32 &&
				width > 0 && height > 0 && quality >= 0 && quality <= 100 {
				state.FieldsPresent |= proto.FieldPDQ256
				state.MissingFields &^= proto.FieldPDQ256
			}
			if requestedFields&proto.FieldPHashParts != 0 {
				if _, err := features.DecodePHashParts(pHash); err == nil {
					state.FieldsPresent |= proto.FieldPHashParts
					state.MissingFields &^= proto.FieldPHashParts
				}
			}
			if requestedFields&proto.FieldSobelHist != 0 {
				if _, err := features.DecodeSobelHist(sobel); err == nil {
					state.FieldsPresent |= proto.FieldSobelHist
					state.MissingFields &^= proto.FieldSobelHist
				}
			}
		}
	case MediaVideo:
		var duration, quality, thumbWidth, thumbHeight sql.NullInt64
		var thumbPath sql.NullString
		var thumbPDQ []byte
		err := tx.QueryRowContext(ctx, `
			SELECT duration_ms, thumb_path, thumb_pdq256, thumb_quality,
			       thumb_width, thumb_height
			FROM video_features WHERE sha512=?1`, sha,
		).Scan(&duration, &thumbPath, &thumbPDQ, &quality, &thumbWidth, &thumbHeight)
		if err != nil && err != sql.ErrNoRows {
			return CommittedState{}, fmt.Errorf("store: load committed analysis video: %w", err)
		}
		if err == nil {
			durationOK := duration.Valid && duration.Int64 >= 0
			legacyContactOK := thumbPath.Valid && thumbPath.String != "" &&
				len(thumbPDQ) == 32 && quality.Valid && quality.Int64 >= 0 && quality.Int64 <= 100
			contactOK := legacyContactOK && thumbWidth.Valid && thumbWidth.Int64 > 0 &&
				thumbHeight.Valid && thumbHeight.Int64 > 0
			if requestedFields&proto.FieldVideoDuration != 0 && durationOK {
				state.FieldsPresent |= proto.FieldVideoDuration
				state.MissingFields &^= proto.FieldVideoDuration
			}
			if requestedFields&proto.FieldVideoContactSheet != 0 && contactOK {
				state.FieldsPresent |= proto.FieldVideoContactSheet
				state.MissingFields &^= proto.FieldVideoContactSheet
			}
			if requestedFields&proto.FieldThumb != 0 && durationOK && legacyContactOK {
				state.FieldsPresent |= proto.FieldThumb
				state.MissingFields &^= proto.FieldThumb
			}
		}
		frameMask, err := committedFrameMask(ctx, tx, sha)
		if err != nil {
			return CommittedState{}, err
		}
		state.FramesPresent = requestedFrames & frameMask
		state.MissingFrames = requestedFrames &^ frameMask
		if requestedFields&proto.FieldVideo6F != 0 && frameMask == proto.FrameMaskFull {
			state.FieldsPresent |= proto.FieldVideo6F
			state.MissingFields &^= proto.FieldVideo6F
		}
	}
	return state, nil
}

func committedFrameMask(ctx context.Context, tx *sql.Tx, sha string) (uint8, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT frame_idx, pdq256, phash_parts, sobel_hist
		FROM video_frames WHERE sha512=?1 AND frame_idx BETWEEN 0 AND 5`, sha,
	)
	if err != nil {
		return 0, fmt.Errorf("store: load committed analysis frames: %w", err)
	}
	defer rows.Close()
	var mask uint8
	for rows.Next() {
		var frameIdx int
		var pdq, pHash, sobel []byte
		if err := rows.Scan(&frameIdx, &pdq, &pHash, &sobel); err != nil {
			return 0, fmt.Errorf("store: scan committed analysis frame: %w", err)
		}
		if len(pdq) != 32 {
			continue
		}
		if _, err := features.DecodePHashParts(pHash); err != nil {
			continue
		}
		if _, err := features.DecodeSobelHist(sobel); err != nil {
			continue
		}
		mask |= 1 << uint(frameIdx)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: iterate committed analysis frames: %w", err)
	}
	return mask, nil
}

func analysisPhase1Done(kind MediaKind, missing uint32) bool {
	switch kind {
	case MediaImage:
		return missing&(proto.FieldSHA512|proto.FieldPDQ256) == 0
	case MediaVideo:
		return missing&(proto.FieldSHA512|proto.FieldThumb|
			proto.FieldVideoDuration|proto.FieldVideoContactSheet) == 0
	default:
		return false
	}
}

func hasAnalysisFrameErrors(frames []Phase2Frame) bool {
	for _, frame := range frames {
		if frame.Error != "" {
			return true
		}
	}
	return false
}

func analysisErrorText(result AnalysisResult) string {
	parts := make([]string, 0, len(result.Errors)+len(result.Frames))
	if text := fieldErrorsText(result.Errors); text != "" {
		parts = append(parts, text)
	}
	for _, frame := range result.Frames {
		if frame.Error != "" {
			parts = append(parts, fmt.Sprintf("frame[%d]: %s", frame.FrameIdx, frame.Error))
		}
	}
	return strings.Join(parts, "; ")
}
