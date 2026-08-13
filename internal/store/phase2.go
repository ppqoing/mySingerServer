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

var ErrPhase2Stale = errors.New("store: stale phase-2 result")

type Phase2Frame struct {
	FrameIdx   int
	PDQ256     []byte
	Quality    int32
	PHashParts []byte
	SobelHist  []byte
	Error      string
}

type VideoFrameFeature struct {
	FrameIdx   int
	PDQ256     []byte
	PHashParts []byte
	SobelHist  []byte
}

type Phase2Result struct {
	MachineID  string
	Path       string
	Kind       MediaKind
	SHA512     []byte
	FieldsDone uint32
	PHashParts []byte
	SobelHist  []byte
	Frames     []Phase2Frame
	Errors     []FieldError
}

type Phase2Committed struct {
	SHA512        string
	MissingFields uint32
	MissingFrames uint8
}

func (d *DB) SavePhase2(ctx context.Context, result Phase2Result) error {
	sha, err := encodeSHA512(result.SHA512)
	if err != nil {
		return err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var fileID int64
	var storedSHA sql.NullString
	var missing uint32
	var priorError sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT id, sha512, missing_mask, error
		FROM files WHERE machine_id=?1 AND path=?2`,
		result.MachineID,
		result.Path,
	).Scan(&fileID, &storedSHA, &missing, &priorError); err != nil {
		return fmt.Errorf("store: load phase2 file: %w", err)
	}
	if !storedSHA.Valid || storedSHA.String != sha {
		return fmt.Errorf("%w: %s", ErrPhase2Stale, result.Path)
	}
	if err := validatePhase2Result(result); err != nil {
		return err
	}

	now := time.Now().Unix()
	changedImage := false
	changedFrames := make([]int, 0, len(result.Frames))
	switch result.Kind {
	case MediaImage:
		if result.FieldsDone&proto.FieldPHashParts != 0 {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO image_features (sha512, phash_parts)
				VALUES (?1, ?2)
				ON CONFLICT (sha512) DO UPDATE SET
					phash_parts=COALESCE(excluded.phash_parts, image_features.phash_parts);`,
				sha,
				result.PHashParts,
			); err != nil {
				return fmt.Errorf("store: save phase2 image pHash: %w", err)
			}
			changedImage = true
		}
		if result.FieldsDone&proto.FieldSobelHist != 0 {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO image_features (sha512, sobel_hist)
				VALUES (?1, ?2)
				ON CONFLICT (sha512) DO UPDATE SET
					sobel_hist=COALESCE(excluded.sobel_hist, image_features.sobel_hist);`,
				sha,
				result.SobelHist,
			); err != nil {
				return fmt.Errorf("store: save phase2 image Sobel: %w", err)
			}
			changedImage = true
		}
	case MediaVideo:
		for _, frame := range result.Frames {
			if frame.Error != "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO video_frames (sha512, frame_idx, pdq256, phash_parts, sobel_hist)
				VALUES (?1, ?2, ?3, ?4, ?5)
				ON CONFLICT (sha512, frame_idx) DO UPDATE SET
					pdq256=COALESCE(excluded.pdq256, video_frames.pdq256),
					phash_parts=COALESCE(excluded.phash_parts, video_frames.phash_parts),
					sobel_hist=COALESCE(excluded.sobel_hist, video_frames.sobel_hist);`,
				sha,
				frame.FrameIdx,
				frame.PDQ256,
				frame.PHashParts,
				frame.SobelHist,
			); err != nil {
				return fmt.Errorf(
					"store: save phase2 video frame %d: %w",
					frame.FrameIdx,
					err,
				)
			}
			changedFrames = append(changedFrames, frame.FrameIdx)
		}
	}

	saveMask := phase2SaveMask(result.Kind)
	derivedMissing, err := phase2MissingFromRows(ctx, tx, result.Kind, sha)
	if err != nil {
		return err
	}
	fieldsToDerive := result.FieldsDone
	if result.Kind == MediaImage {
		fieldsToDerive |= phase2Mask(result.Kind)
	}
	if result.Kind == MediaVideo &&
		missing&proto.FieldVideo6F != 0 &&
		derivedMissing&(proto.FieldVideo6FPHash|proto.FieldVideo6FSobel) == 0 {
		fieldsToDerive |= proto.FieldVideo6F
		derivedMissing &^= proto.FieldVideo6F
	}
	updatedMissing := missing&^fieldsToDerive | derivedMissing&fieldsToDerive
	phase2Done := updatedMissing&saveMask == 0
	status := proto.StatusPartial
	var errorValue any
	splitVideoComplete := result.Kind == MediaVideo &&
		result.FieldsDone&(proto.FieldVideo6FPHash|proto.FieldVideo6FSobel) != 0 &&
		updatedMissing&(proto.FieldVideo6FPHash|proto.FieldVideo6FSobel) == 0
	if updatedMissing == 0 {
		status = proto.StatusDone
	} else if summary := phase2ErrorText(result); summary != "" {
		errorValue = summary
	} else if priorError.Valid && !splitVideoComplete {
		errorValue = priorError.String
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE files SET status=?1, error=?2, missing_mask=?3,
			phase2_done=?4, updated_at=?5
		WHERE id=?6`,
		status,
		errorValue,
		updatedMissing,
		boolToInt(phase2Done),
		now,
		fileID,
	); err != nil {
		return fmt.Errorf("store: update phase2 file: %w", err)
	}
	if err := enqueuePhase1Sync(ctx, tx, "files", fmt.Sprint(fileID), now); err != nil {
		return err
	}
	if changedImage {
		if err := enqueuePhase1Sync(ctx, tx, "image_features", sha, now); err != nil {
			return err
		}
	}
	for _, frameIdx := range changedFrames {
		if err := enqueuePhase1Sync(
			ctx,
			tx,
			"video_frames",
			fmt.Sprintf("%s:%d", sha, frameIdx),
			now,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) Phase2MissingMask(
	ctx context.Context,
	machineID string,
	path string,
) (uint32, error) {
	var missing uint32
	if err := d.db.QueryRowContext(ctx, `
		SELECT missing_mask FROM files WHERE machine_id=?1 AND path=?2`,
		machineID,
		path,
	).Scan(&missing); err != nil {
		return 0, fmt.Errorf("store: load committed phase2 mask: %w", err)
	}
	return missing, nil
}

func (d *DB) Phase2CommittedState(
	ctx context.Context,
	machineID string,
	path string,
	kind MediaKind,
) (Phase2Committed, error) {
	return d.Phase2CommittedStateForFields(ctx, machineID, path, kind, phase2Mask(kind))
}

func (d *DB) Phase2CommittedStateForFields(
	ctx context.Context,
	machineID string,
	path string,
	kind MediaKind,
	requestedFields uint32,
) (Phase2Committed, error) {
	var state Phase2Committed
	var sha sql.NullString
	if err := d.db.QueryRowContext(ctx, `
		SELECT sha512 FROM files WHERE machine_id=?1 AND path=?2`,
		machineID,
		path,
	).Scan(&sha); err != nil {
		return state, fmt.Errorf("store: load phase2 committed file: %w", err)
	}
	if !sha.Valid {
		return state, fmt.Errorf("store: phase2 committed file has no SHA-512")
	}
	state.SHA512 = sha.String
	switch kind {
	case MediaImage:
		var pHash, sobel []byte
		err := d.db.QueryRowContext(ctx, `
			SELECT phash_parts, sobel_hist
			FROM image_features WHERE sha512=?1`,
			state.SHA512,
		).Scan(&pHash, &sobel)
		if err != nil && err != sql.ErrNoRows {
			return Phase2Committed{}, fmt.Errorf(
				"store: load committed phase2 image fields: %w",
				err,
			)
		}
		if err == sql.ErrNoRows {
			state.MissingFields = requestedFields &
				(proto.FieldPHashParts | proto.FieldSobelHist)
			return state, nil
		}
		if requestedFields&proto.FieldPHashParts != 0 {
			if _, decodeErr := features.DecodePHashParts(pHash); decodeErr != nil {
				state.MissingFields |= proto.FieldPHashParts
			}
		}
		if requestedFields&proto.FieldSobelHist != 0 {
			if _, decodeErr := features.DecodeSobelHist(sobel); decodeErr != nil {
				state.MissingFields |= proto.FieldSobelHist
			}
		}
		return state, nil
	case MediaVideo:
		state.MissingFrames = proto.FrameMaskFull
		rows, err := d.db.QueryContext(ctx, `
			SELECT frame_idx, pdq256, phash_parts, sobel_hist
			FROM video_frames
			WHERE sha512=?1 AND frame_idx BETWEEN 0 AND 5`,
			state.SHA512,
		)
		if err != nil {
			return Phase2Committed{}, fmt.Errorf(
				"store: load committed phase2 video frames: %w",
				err,
			)
		}
		defer rows.Close()
		for rows.Next() {
			var frameIdx int
			var pdq, pHash, sobel []byte
			if err := rows.Scan(&frameIdx, &pdq, &pHash, &sobel); err != nil {
				return Phase2Committed{}, fmt.Errorf(
					"store: scan committed phase2 video frame: %w",
					err,
				)
			}
			if !videoFramePayloadValid(requestedFields, pdq, pHash, sobel) {
				continue
			}
			state.MissingFrames &^= 1 << uint(frameIdx)
		}
		if err := rows.Err(); err != nil {
			return Phase2Committed{}, fmt.Errorf(
				"store: iterate committed phase2 video frames: %w",
				err,
			)
		}
		if state.MissingFrames != 0 {
			state.MissingFields = requestedFields & videoSixFrameFields()
		}
		return state, nil
	default:
		return Phase2Committed{}, fmt.Errorf(
			"store: invalid phase2 media kind %q",
			kind,
		)
	}
}

func validatePhase2Result(result Phase2Result) error {
	mask := phase2SaveMask(result.Kind)
	if mask == 0 {
		return fmt.Errorf("store: invalid phase2 media kind %q", result.Kind)
	}
	if result.FieldsDone&^mask != 0 {
		return fmt.Errorf(
			"store: phase2 fields %#x exceed media mask %#x",
			result.FieldsDone,
			mask,
		)
	}
	for _, fieldError := range result.Errors {
		field := fieldError.Field
		if field == 0 {
			continue
		}
		if field&(field-1) != 0 || field&mask != field {
			return fmt.Errorf(
				"store: phase2 error field %#x is not one media field from %#x",
				field,
				mask,
			)
		}
	}
	switch result.Kind {
	case MediaImage:
		if len(result.Frames) != 0 {
			return fmt.Errorf("store: phase2 image result contains video frames")
		}
		if err := validatePhase2StoreBlob(
			proto.FieldPHashParts,
			result.FieldsDone,
			result.PHashParts,
			func(blob []byte) error {
				_, err := features.DecodePHashParts(blob)
				return err
			},
			"phash_parts",
		); err != nil {
			return err
		}
		return validatePhase2StoreBlob(
			proto.FieldSobelHist,
			result.FieldsDone,
			result.SobelHist,
			func(blob []byte) error {
				_, err := features.DecodeSobelHist(blob)
				return err
			},
			"sobel_hist",
		)
	case MediaVideo:
		if len(result.PHashParts) != 0 || len(result.SobelHist) != 0 {
			return fmt.Errorf("store: phase2 video result contains image payload")
		}
		return validatePhase2Frames(result)
	default:
		return fmt.Errorf("store: invalid phase2 media kind %q", result.Kind)
	}
}

func validatePhase2StoreBlob(
	field uint32,
	fieldsDone uint32,
	blob []byte,
	decode func([]byte) error,
	name string,
) error {
	succeeded := fieldsDone&field != 0
	if !succeeded && len(blob) != 0 {
		return fmt.Errorf("store: unclaimed phase2 %s payload", name)
	}
	if !succeeded {
		return nil
	}
	if err := decode(blob); err != nil {
		return fmt.Errorf("store: invalid phase2 %s: %w", name, err)
	}
	return nil
}

func validatePhase2Frames(result Phase2Result) error {
	var seen [6]bool
	complete := 0
	for _, frame := range result.Frames {
		if frame.FrameIdx < 0 || frame.FrameIdx >= len(seen) {
			return fmt.Errorf("store: phase2 frame index %d out of range", frame.FrameIdx)
		}
		if seen[frame.FrameIdx] {
			return fmt.Errorf("store: duplicate phase2 frame index %d", frame.FrameIdx)
		}
		seen[frame.FrameIdx] = true
		if frame.Error != "" {
			if len(frame.PDQ256) != 0 || frame.Quality != 0 ||
				len(frame.PHashParts) != 0 || len(frame.SobelHist) != 0 {
				return fmt.Errorf(
					"store: errored phase2 frame %d contains feature payload",
					frame.FrameIdx,
				)
			}
			continue
		}
		splitPHash := result.FieldsDone&proto.FieldVideo6FPHash != 0
		splitSobel := result.FieldsDone&proto.FieldVideo6FSobel != 0
		if splitPHash && (len(frame.PDQ256) != 0 || frame.Quality != 0 || len(frame.SobelHist) != 0) {
			return fmt.Errorf("store: phase2 stage-two frame %d contains cross-stage payload", frame.FrameIdx)
		}
		if splitSobel && (len(frame.PDQ256) != 0 || frame.Quality != 0 || len(frame.PHashParts) != 0) {
			return fmt.Errorf("store: phase2 stage-three frame %d contains cross-stage payload", frame.FrameIdx)
		}
		if !splitPHash && !splitSobel && len(frame.PDQ256) != 32 {
			return fmt.Errorf(
				"store: phase2 frame %d PDQ length %d",
				frame.FrameIdx,
				len(frame.PDQ256),
			)
		}
		if frame.Quality < 0 || frame.Quality > 100 {
			return fmt.Errorf(
				"store: phase2 frame %d quality %d is invalid",
				frame.FrameIdx,
				frame.Quality,
			)
		}
		if !splitSobel {
			if _, err := features.DecodePHashParts(frame.PHashParts); err != nil {
				return fmt.Errorf(
					"store: phase2 frame %d phash_parts: %w",
					frame.FrameIdx,
					err,
				)
			}
		}
		if !splitPHash {
			if _, err := features.DecodeSobelHist(frame.SobelHist); err != nil {
				return fmt.Errorf(
					"store: phase2 frame %d sobel_hist: %w",
					frame.FrameIdx,
					err,
				)
			}
		}
		complete++
	}
	if result.FieldsDone&(proto.FieldVideo6F|proto.FieldVideo6FPHash|proto.FieldVideo6FSobel) != 0 && complete != len(seen) {
		return fmt.Errorf(
			"store: completed phase2 video has %d complete frames, want %d",
			complete,
			len(seen),
		)
	}
	return nil
}

func phase2Mask(kind MediaKind) uint32 {
	switch kind {
	case MediaImage:
		return proto.FieldPHashParts | proto.FieldSobelHist
	case MediaVideo:
		return proto.FieldVideo6F
	default:
		return 0
	}
}

func phase2SaveMask(kind MediaKind) uint32 {
	if kind == MediaVideo {
		return proto.FieldVideo6F | proto.FieldVideo6FPHash | proto.FieldVideo6FSobel
	}
	return phase2Mask(kind)
}

func phase2MissingFromRows(
	ctx context.Context,
	tx *sql.Tx,
	kind MediaKind,
	sha string,
) (uint32, error) {
	switch kind {
	case MediaImage:
		var hasPHash, hasSobel bool
		if err := tx.QueryRowContext(ctx, `
			SELECT phash_parts IS NOT NULL, sobel_hist IS NOT NULL
			FROM image_features WHERE sha512=?1`,
			sha,
		).Scan(&hasPHash, &hasSobel); err != nil {
			if err == sql.ErrNoRows {
				return proto.FieldPHashParts | proto.FieldSobelHist, nil
			}
			return 0, fmt.Errorf("store: load phase2 image presence: %w", err)
		}
		var missing uint32
		if !hasPHash {
			missing |= proto.FieldPHashParts
		}
		if !hasSobel {
			missing |= proto.FieldSobelHist
		}
		return missing, nil
	case MediaVideo:
		var completeLegacy, completePHash, completeSobel int
		if err := tx.QueryRowContext(ctx, `
			SELECT
				count(*) FILTER (WHERE pdq256 IS NOT NULL AND phash_parts IS NOT NULL AND sobel_hist IS NOT NULL),
				count(*) FILTER (WHERE phash_parts IS NOT NULL),
				count(*) FILTER (WHERE sobel_hist IS NOT NULL)
			FROM video_frames
			WHERE sha512=?1 AND frame_idx BETWEEN 0 AND 5`,
			sha,
		).Scan(&completeLegacy, &completePHash, &completeSobel); err != nil {
			return 0, fmt.Errorf("store: load phase2 video presence: %w", err)
		}
		var missing uint32
		if completeLegacy != 6 {
			missing |= proto.FieldVideo6F
		}
		if completePHash != 6 {
			missing |= proto.FieldVideo6FPHash
		}
		if completeSobel != 6 {
			missing |= proto.FieldVideo6FSobel
		}
		return missing, nil
	default:
		return 0, fmt.Errorf("store: invalid phase2 media kind %q", kind)
	}
}

func phase2ErrorText(result Phase2Result) string {
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
