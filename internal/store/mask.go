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
)

func phase1Mask(kind MediaKind) uint32 {
	switch kind {
	case MediaImage:
		return proto.FieldSHA512 | proto.FieldPDQ256
	case MediaVideo:
		return proto.FieldSHA512 | proto.FieldThumb
	default:
		return proto.FieldSHA512
	}
}

func (d *DB) MissingPhase1(ctx context.Context, row FileRow, kind MediaKind) (uint32, error) {
	full := phase1Mask(kind)
	if row.SHA512 == nil || *row.SHA512 == "" {
		return full, nil
	}

	var size, mtime int64
	err := d.db.QueryRowContext(ctx, `
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

	switch kind {
	case MediaImage:
		var pdq []byte
		err := d.db.QueryRowContext(ctx,
			`SELECT pdq256 FROM image_features WHERE sha512=?1`, *row.SHA512,
		).Scan(&pdq)
		if err == sql.ErrNoRows || len(pdq) == 0 {
			return proto.FieldPDQ256, nil
		}
		if err != nil {
			return 0, fmt.Errorf("store: load image feature: %w", err)
		}
		return 0, nil
	case MediaVideo:
		var duration sql.NullInt64
		var path sql.NullString
		var pdq []byte
		var quality sql.NullInt64
		err := d.db.QueryRowContext(ctx, `
			SELECT duration_ms, thumb_path, thumb_pdq256, thumb_quality
			FROM video_features WHERE sha512=?1`, *row.SHA512,
		).Scan(&duration, &path, &pdq, &quality)
		if err == sql.ErrNoRows {
			return proto.FieldThumb, nil
		}
		if err != nil {
			return 0, fmt.Errorf("store: load video feature: %w", err)
		}
		if !duration.Valid || !path.Valid || path.String == "" || len(pdq) == 0 || !quality.Valid {
			return proto.FieldThumb, nil
		}
		return 0, nil
	default:
		return 0, nil
	}
}
