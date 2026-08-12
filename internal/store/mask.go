package store

import (
	"context"
	"database/sql"
	"fmt"

	"dedup/internal/proto"
)

type MediaKind string

const (
	MediaImage MediaKind = "image"
	MediaVideo MediaKind = "video"

	phaseOneFieldsMask = proto.FieldSHA512 | proto.FieldPDQ256 | proto.FieldThumb |
		proto.FieldVideoDuration | proto.FieldVideoContactSheet
)

// RequiredStageOneMask returns the non-optional fields required before a file
// is ready for first-screen duplicate analysis.
func RequiredStageOneMask(kind MediaKind) uint32 {
	switch kind {
	case MediaImage:
		return proto.FieldSHA512 | proto.FieldPDQ256
	case MediaVideo:
		return proto.FieldSHA512 | proto.FieldVideoDuration | proto.FieldVideoContactSheet
	default:
		return proto.FieldSHA512
	}
}

func phase1Mask(kind MediaKind) uint32 { return RequiredStageOneMask(kind) }

func (d *DB) MissingPhase1(ctx context.Context, row FileRow, kind MediaKind) (uint32, error) {
	return missingPhase1(ctx, d.db, row, kind, phase1Mask(kind))
}

type phase1Queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func missingPhase1(
	ctx context.Context,
	queryer phase1Queryer,
	row FileRow,
	kind MediaKind,
	required uint32,
) (uint32, error) {
	full := required & phaseOneFieldsMask
	if row.SHA512 == nil || *row.SHA512 == "" {
		return full, nil
	}

	var size, mtime int64
	err := queryer.QueryRowContext(ctx, `
		SELECT size, mtime FROM files WHERE machine_id=?1 AND path=?2`,
		row.MachineID, row.Path,
	).Scan(&size, &mtime)
	if err != nil {
		if err == sql.ErrNoRows {
			return full, nil
		}
		return 0, fmt.Errorf("store: load phase1 file: %w", err)
	}
	if size != row.Size || mtime != row.MTime {
		return full, nil
	}
	missing := full &^ proto.FieldSHA512

	switch kind {
	case MediaImage:
		if full&proto.FieldPDQ256 == 0 {
			return missing, nil
		}
		var width, height, quality int32
		var pdq []byte
		err := queryer.QueryRowContext(ctx,
			`SELECT width, height, pdq256, pdq_quality FROM image_features WHERE sha512=?1`, *row.SHA512,
		).Scan(&width, &height, &pdq, &quality)
		if err == sql.ErrNoRows || len(pdq) != 32 || width <= 0 || height <= 0 || quality < 0 || quality > 100 {
			return missing, nil
		}
		if err != nil {
			return 0, fmt.Errorf("store: load image feature: %w", err)
		}
		return missing &^ proto.FieldPDQ256, nil
	case MediaVideo:
		videoRequired := full & (proto.FieldThumb | proto.FieldVideoDuration | proto.FieldVideoContactSheet)
		if videoRequired == 0 {
			return missing, nil
		}
		var duration sql.NullInt64
		var path sql.NullString
		var pdq []byte
		var quality, width, height sql.NullInt64
		err := queryer.QueryRowContext(ctx, `
			SELECT duration_ms, thumb_path, thumb_pdq256, thumb_quality,
			       thumb_width, thumb_height
			FROM video_features WHERE sha512=?1`, *row.SHA512,
		).Scan(&duration, &path, &pdq, &quality, &width, &height)
		if err == sql.ErrNoRows {
			return missing, nil
		}
		if err != nil {
			return 0, fmt.Errorf("store: load video feature: %w", err)
		}
		durationOK := duration.Valid && duration.Int64 >= 0
		legacyContactOK := path.Valid && path.String != "" && len(pdq) != 0 &&
			quality.Valid && quality.Int64 >= 0 && quality.Int64 <= 100
		contactOK := legacyContactOK && len(pdq) == 32 &&
			width.Valid && width.Int64 > 0 && height.Valid && height.Int64 > 0
		if durationOK {
			missing &^= proto.FieldVideoDuration
		}
		if contactOK {
			missing &^= proto.FieldVideoContactSheet
		}
		if durationOK && legacyContactOK {
			missing &^= proto.FieldThumb
		}
		return missing, nil
	default:
		return missing, nil
	}
}
