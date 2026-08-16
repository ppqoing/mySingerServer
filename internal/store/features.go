package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"dedup/internal/proto"
)

type FieldError struct {
	Field uint32
	Stage string
	Msg   string
}

type Phase1Result struct {
	MachineID    string
	Path         string
	Kind         MediaKind
	SHA512       []byte
	FieldsDone   uint32
	PDQ          []byte
	Quality      int32
	Width        int32
	Height       int32
	DurationMS   *int64
	ThumbPath    string
	ThumbPDQ     []byte
	ThumbQuality *int32
	Errors       []FieldError
}

type ImageFeature struct {
	SHA512     []byte
	Width      int32
	Height     int32
	PDQ        []byte
	Quality    int32
	PHashParts []byte
	SobelHist  []byte
}

type VideoFeature struct {
	SHA512       []byte
	DurationMS   *int64
	ThumbPath    string
	ThumbPDQ     []byte
	ThumbQuality *int32
	ThumbWidth   *int32
	ThumbHeight  *int32
}

func encodeSHA512(sha []byte) (string, error) {
	if len(sha) != 64 {
		return "", fmt.Errorf("store: SHA-512 must be exactly 64 bytes, got %d", len(sha))
	}
	return hex.EncodeToString(sha), nil
}

func (d *DB) LookupImage(ctx context.Context, sha []byte) (*ImageFeature, error) {
	shaText, err := encodeSHA512(sha)
	if err != nil {
		return nil, err
	}
	var feature ImageFeature
	var storedSHA string
	err = d.db.QueryRowContext(ctx, `
		SELECT sha512, width, height, pdq256, pdq_quality FROM image_features WHERE sha512=?1`, shaText,
	).Scan(&storedSHA, &feature.Width, &feature.Height, &feature.PDQ, &feature.Quality)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: lookup image: %w", err)
	}
	feature.SHA512, err = hex.DecodeString(storedSHA)
	if err != nil {
		return nil, fmt.Errorf("store: decode image SHA-512: %w", err)
	}
	return &feature, nil
}

func (d *DB) LookupVideo(ctx context.Context, sha []byte) (*VideoFeature, error) {
	shaText, err := encodeSHA512(sha)
	if err != nil {
		return nil, err
	}
	var feature VideoFeature
	var storedSHA string
	var duration sql.NullInt64
	var thumbPath sql.NullString
	var quality, thumbWidth, thumbHeight sql.NullInt64
	err = d.db.QueryRowContext(ctx, `
		SELECT sha512, duration_ms, thumb_path, thumb_pdq256, thumb_quality,
		       thumb_width, thumb_height
		FROM video_features WHERE sha512=?1`, shaText,
	).Scan(
		&storedSHA,
		&duration,
		&thumbPath,
		&feature.ThumbPDQ,
		&quality,
		&thumbWidth,
		&thumbHeight,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: lookup video: %w", err)
	}
	feature.SHA512, err = hex.DecodeString(storedSHA)
	if err != nil {
		return nil, fmt.Errorf("store: decode video SHA-512: %w", err)
	}
	if duration.Valid {
		value := duration.Int64
		feature.DurationMS = &value
	}
	if thumbPath.Valid {
		feature.ThumbPath = thumbPath.String
	}
	if quality.Valid {
		value := int32(quality.Int64)
		feature.ThumbQuality = &value
	}
	if thumbWidth.Valid {
		value := int32(thumbWidth.Int64)
		feature.ThumbWidth = &value
	}
	if thumbHeight.Valid {
		value := int32(thumbHeight.Int64)
		feature.ThumbHeight = &value
	}
	return &feature, nil
}

func (d *DB) SavePhase1(ctx context.Context, result Phase1Result) error {
	if err := validatePhase1FeaturePayload(result); err != nil {
		return err
	}
	var sha string
	var shaValue any
	switch {
	case len(result.SHA512) == 64:
		var err error
		sha, err = encodeSHA512(result.SHA512)
		if err != nil {
			return err
		}
		shaValue = sha
	case validPreSHAFailure(result):
		// A stat/open/read failure can legitimately happen before SHA-512
		// exists. Persist the original terminal field error without
		// manufacturing a second store failure.
		shaValue = nil
	default:
		return fmt.Errorf("store: SHA-512 must be exactly 64 bytes, got %d", len(result.SHA512))
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var fileID int64
	var missing uint32
	if err := tx.QueryRowContext(ctx, `
		SELECT id, missing_mask FROM files WHERE machine_id=?1 AND path=?2`,
		result.MachineID, result.Path,
	).Scan(&fileID, &missing); err != nil {
		return fmt.Errorf("store: load phase1 file: %w", err)
	}

	succeeded := uint32(0)
	if result.FieldsDone&proto.FieldSHA512 != 0 {
		succeeded |= proto.FieldSHA512
	}
	switch result.Kind {
	case MediaImage:
		if result.FieldsDone&proto.FieldPDQ256 != 0 && len(result.PDQ) != 0 {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO image_features (sha512, width, height, pdq256, pdq_quality)
				VALUES (?1, ?2, ?3, ?4, ?5)
				ON CONFLICT (sha512) DO UPDATE SET width=excluded.width, height=excluded.height,
				pdq256=excluded.pdq256, pdq_quality=excluded.pdq_quality;`,
				sha, result.Width, result.Height, result.PDQ, result.Quality,
			); err != nil {
				return fmt.Errorf("store: save image feature: %w", err)
			}
			succeeded |= proto.FieldPDQ256
		}
	case MediaVideo:
		if result.FieldsDone&(proto.FieldThumb|proto.FieldVideoDuration) != 0 &&
			result.DurationMS != nil {
			var duration any
			if result.DurationMS != nil {
				duration = *result.DurationMS
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO video_features (sha512, duration_ms) VALUES (?1, ?2)
				ON CONFLICT (sha512) DO UPDATE SET duration_ms=excluded.duration_ms;`,
				sha, duration,
			); err != nil {
				return fmt.Errorf("store: save video duration: %w", err)
			}
			succeeded |= result.FieldsDone & (proto.FieldThumb | proto.FieldVideoDuration)
		}
		if result.FieldsDone&(proto.FieldThumb|proto.FieldVideoContactSheet) != 0 &&
			(result.ThumbPath != "" || len(result.ThumbPDQ) != 0 || result.ThumbQuality != nil) {
			var quality any
			if result.ThumbQuality != nil {
				quality = *result.ThumbQuality
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO video_features
					(sha512, thumb_path, thumb_pdq256, thumb_quality, thumb_width, thumb_height)
				VALUES (?1, NULLIF(?2, ''), ?3, ?4, ?5, ?6)
				ON CONFLICT (sha512) DO UPDATE SET
					thumb_path=COALESCE(excluded.thumb_path, video_features.thumb_path),
					thumb_pdq256=COALESCE(excluded.thumb_pdq256, video_features.thumb_pdq256),
					thumb_quality=COALESCE(excluded.thumb_quality, video_features.thumb_quality),
					thumb_width=COALESCE(excluded.thumb_width, video_features.thumb_width),
					thumb_height=COALESCE(excluded.thumb_height, video_features.thumb_height);`,
				sha, result.ThumbPath, nullableBytes(result.ThumbPDQ), quality,
				nullablePositiveInt32(result.Width), nullablePositiveInt32(result.Height),
			); err != nil {
				return fmt.Errorf("store: save video contact sheet: %w", err)
			}
			succeeded |= result.FieldsDone & (proto.FieldThumb | proto.FieldVideoContactSheet)
		}
	}

	updatedMissing := missing
	if succeeded&proto.FieldSHA512 != 0 {
		updatedMissing &^= proto.FieldSHA512
	}
	if sha != "" && result.Kind == MediaImage && imageFeatureExists(ctx, tx, sha) {
		updatedMissing &^= proto.FieldPDQ256
	}
	if sha != "" && result.Kind == MediaVideo {
		updatedMissing &^= videoFeatureFields(ctx, tx, sha)
	}
	status, phase1Done := stageOneState(result.Kind, updatedMissing, len(result.Errors) != 0)
	var errorText any
	if status != proto.StatusDone && len(result.Errors) != 0 {
		errorText = fieldErrorsText(result.Errors)
	}
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `
		UPDATE files SET sha512=COALESCE(?1, sha512), status=?2, error=?3,
		    missing_mask=?4, phase1_done=?5, updated_at=?6
		WHERE id=?7`,
		shaValue, status, errorText, updatedMissing, boolToInt(phase1Done), now, fileID,
	); err != nil {
		return fmt.Errorf("store: update phase1 file: %w", err)
	}
	if succeeded != 0 || len(result.Errors) != 0 {
		if err := enqueuePhase1Sync(ctx, tx, "files", fmt.Sprint(fileID), now); err != nil {
			return err
		}
		if result.Kind == MediaImage && succeeded&proto.FieldPDQ256 != 0 {
			if err := enqueuePhase1Sync(ctx, tx, "image_features", sha, now); err != nil {
				return err
			}
		}
		if result.Kind == MediaVideo && succeeded&(proto.FieldThumb|proto.FieldVideoDuration|proto.FieldVideoContactSheet) != 0 {
			if err := enqueuePhase1Sync(ctx, tx, "video_features", sha, now); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func validPreSHAFailure(result Phase1Result) bool {
	if len(result.SHA512) != 0 ||
		result.FieldsDone != 0 ||
		len(result.Errors) == 0 ||
		len(result.PDQ) != 0 ||
		result.Quality != 0 ||
		result.Width != 0 ||
		result.Height != 0 ||
		result.DurationMS != nil ||
		result.ThumbPath != "" ||
		len(result.ThumbPDQ) != 0 ||
		result.ThumbQuality != nil {
		return false
	}
	for _, fieldError := range result.Errors {
		if fieldError.Field&proto.FieldSHA512 == 0 {
			return false
		}
		switch fieldError.Stage {
		case "stat", "open", "read", "sha512", "native_open", "native_hash", "stale":
		default:
			return false
		}
	}
	return true
}

func validatePhase1FeaturePayload(result Phase1Result) error {
	switch result.Kind {
	case MediaImage:
		if result.FieldsDone&proto.FieldPDQ256 != 0 &&
			(len(result.PDQ) != 32 || result.Quality < 0 || result.Quality > 100 ||
				result.Width <= 0 || result.Height <= 0) {
			return fmt.Errorf("store: invalid phase1 image PDQ payload")
		}
	case MediaVideo:
		if result.FieldsDone&proto.FieldVideoDuration != 0 &&
			(result.DurationMS == nil || *result.DurationMS < 0) {
			return fmt.Errorf("store: invalid phase1 video duration payload")
		}
		if result.FieldsDone&proto.FieldVideoContactSheet != 0 &&
			(result.ThumbPath == "" || len(result.ThumbPDQ) != 32 ||
				result.ThumbQuality == nil || *result.ThumbQuality < 0 || *result.ThumbQuality > 100 ||
				result.Width <= 0 || result.Height <= 0) {
			return fmt.Errorf("store: invalid phase1 video contact sheet payload")
		}
	}
	return nil
}

func (d *DB) MarkCrash(ctx context.Context, machineID, path, message string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var fileID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM files WHERE machine_id=?1 AND path=?2`, machineID, path,
	).Scan(&fileID); err != nil {
		return fmt.Errorf("store: load crash file: %w", err)
	}
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `
		UPDATE files SET status=?1, error=?2, updated_at=?3 WHERE id=?4`,
		proto.StatusCrash, message, now, fileID,
	); err != nil {
		return fmt.Errorf("store: mark crash: %w", err)
	}
	if err := enqueuePhase1Sync(ctx, tx, "files", fmt.Sprint(fileID), now); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) Phase1MissingMask(
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
		return 0, fmt.Errorf("store: load committed phase1 mask: %w", err)
	}
	return missing, nil
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func imageFeatureExists(ctx context.Context, tx *sql.Tx, sha string) bool {
	var width, height, quality int32
	var pdq []byte
	return tx.QueryRowContext(ctx, `
		SELECT width, height, pdq256, pdq_quality FROM image_features WHERE sha512=?1`, sha,
	).Scan(&width, &height, &pdq, &quality) == nil && len(pdq) == 32 &&
		width > 0 && height > 0 && quality >= 0 && quality <= 100
}

func nullablePositiveInt32(value int32) any {
	if value <= 0 {
		return nil
	}
	return value
}

func videoFeatureFields(ctx context.Context, tx *sql.Tx, sha string) uint32 {
	var duration sql.NullInt64
	var path sql.NullString
	var pdq []byte
	var quality, width, height sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT duration_ms, thumb_path, thumb_pdq256, thumb_quality, thumb_width, thumb_height
		FROM video_features WHERE sha512=?1`, sha,
	).Scan(&duration, &path, &pdq, &quality, &width, &height)
	if err != nil {
		return 0
	}
	fields := uint32(0)
	durationOK := duration.Valid && duration.Int64 >= 0
	legacyContactOK := path.Valid && path.String != "" && len(pdq) == 32 &&
		quality.Valid && quality.Int64 >= 0 && quality.Int64 <= 100
	if durationOK {
		fields |= proto.FieldVideoDuration
	}
	if legacyContactOK && width.Valid && width.Int64 > 0 && height.Valid && height.Int64 > 0 {
		fields |= proto.FieldVideoContactSheet
	}
	if durationOK && legacyContactOK {
		fields |= proto.FieldThumb
	}
	return fields
}

func fieldErrorsText(errors []FieldError) string {
	parts := make([]string, 0, len(errors))
	for _, fieldError := range errors {
		if fieldError.Stage == "" {
			parts = append(parts, fieldError.Msg)
		} else {
			parts = append(parts, fieldError.Stage+": "+fieldError.Msg)
		}
	}
	return strings.Join(parts, "; ")
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func enqueuePhase1Sync(ctx context.Context, tx *sql.Tx, table, rowPK string, now int64) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sync_queue (table_name, row_pk, synced, enqueued_at, generation)
		VALUES (?1, ?2, 0, ?3, 1)
		ON CONFLICT (table_name, row_pk) DO UPDATE SET
			synced=0, enqueued_at=excluded.enqueued_at, generation=sync_queue.generation+1;`,
		table, rowPK, now,
	); err != nil {
		return fmt.Errorf("store: enqueue %s/%s: %w", table, rowPK, err)
	}
	return nil
}
