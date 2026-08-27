package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"

	"dedup/internal/features"
	"dedup/internal/proto"
)

type ContentState struct {
	SHA512         []byte
	FieldsPresent  uint32
	MissingFields  uint32
	FramesPresent  uint8
	MissingFrames  uint8
	Image          *ImageFeature
	Video          *VideoFeature
	Frames         []VideoFrameFeature
	VideoContainer *proto.VideoContainerMetadata
	VideoStreams   []proto.VideoStreamMetadata
}

func (d *DB) LookupContent(
	ctx context.Context,
	sha []byte,
	kind MediaKind,
	requestedFields uint32,
	requestedFrames uint8,
) (ContentState, error) {
	shaText, err := encodeSHA512(sha)
	if err != nil {
		return ContentState{}, err
	}
	allowedFields := contentFieldMask(kind)
	if allowedFields == 0 {
		return ContentState{}, fmt.Errorf("store: invalid content media kind %q", kind)
	}
	if foreign := requestedFields &^ allowedFields; foreign != 0 {
		return ContentState{}, fmt.Errorf(
			"store: requested content fields %#x exceed %s mask %#x",
			foreign, kind, allowedFields,
		)
	}
	if requestedFrames&^proto.FrameMaskFull != 0 {
		return ContentState{}, fmt.Errorf("store: requested frames contain bits outside six frames")
	}
	if kind == MediaImage && requestedFrames != 0 {
		return ContentState{}, fmt.Errorf("store: image content cannot request video frames")
	}
	if kind == MediaVideo && requestedFields&videoSixFrameFields() != 0 && requestedFrames == 0 {
		requestedFrames = proto.FrameMaskFull
	}

	state := ContentState{
		SHA512:        append([]byte(nil), sha...),
		MissingFields: requestedFields,
		MissingFrames: requestedFrames,
	}
	if requestedFields&proto.FieldSHA512 != 0 {
		state.FieldsPresent |= proto.FieldSHA512
		state.MissingFields &^= proto.FieldSHA512
	}

	switch kind {
	case MediaImage:
		if err := d.lookupImageContent(ctx, shaText, requestedFields, &state); err != nil {
			return ContentState{}, err
		}
	case MediaVideo:
		if err := d.lookupVideoContent(ctx, shaText, requestedFields, requestedFrames, &state); err != nil {
			return ContentState{}, err
		}
	}
	return state, nil
}

func contentFieldMask(kind MediaKind) uint32 {
	switch kind {
	case MediaImage:
		return proto.FieldSHA512 | proto.FieldPDQ256 |
			proto.FieldPHashParts | proto.FieldSobelHist
	case MediaVideo:
		return proto.FieldSHA512 | proto.FieldThumb | proto.FieldVideo6F |
			proto.FieldVideoDuration | proto.FieldVideoContactSheet |
			proto.FieldVideo6FPHash | proto.FieldVideo6FSobel | proto.FieldVideoMetadata
	default:
		return 0
	}
}

func (d *DB) lookupImageContent(
	ctx context.Context,
	shaText string,
	requested uint32,
	state *ContentState,
) error {
	wanted := requested & (proto.FieldPDQ256 | proto.FieldPHashParts | proto.FieldSobelHist)
	if wanted == 0 {
		return nil
	}
	var storedSHA string
	var width, height, quality int32
	var pdq, pHash, sobel []byte
	err := d.db.QueryRowContext(ctx, `
		SELECT sha512, width, height, pdq256, pdq_quality, phash_parts, sobel_hist
		FROM image_features WHERE sha512=?1`, shaText,
	).Scan(&storedSHA, &width, &height, &pdq, &quality, &pHash, &sobel)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: lookup image content: %w", err)
	}
	decodedSHA, err := hex.DecodeString(storedSHA)
	if err != nil || len(decodedSHA) != 64 {
		return nil
	}

	feature := &ImageFeature{SHA512: decodedSHA}
	if requested&proto.FieldPDQ256 != 0 && len(pdq) == 32 &&
		width > 0 && height > 0 && quality >= 0 && quality <= 100 {
		feature.Width = width
		feature.Height = height
		feature.PDQ = append([]byte(nil), pdq...)
		feature.Quality = quality
		state.FieldsPresent |= proto.FieldPDQ256
		state.MissingFields &^= proto.FieldPDQ256
	}
	if requested&proto.FieldPHashParts != 0 {
		if _, decodeErr := features.DecodePHashParts(pHash); decodeErr == nil {
			feature.PHashParts = append([]byte(nil), pHash...)
			state.FieldsPresent |= proto.FieldPHashParts
			state.MissingFields &^= proto.FieldPHashParts
		}
	}
	if requested&proto.FieldSobelHist != 0 {
		if _, decodeErr := features.DecodeSobelHist(sobel); decodeErr == nil {
			feature.SobelHist = append([]byte(nil), sobel...)
			state.FieldsPresent |= proto.FieldSobelHist
			state.MissingFields &^= proto.FieldSobelHist
		}
	}
	if state.FieldsPresent&wanted != 0 {
		state.Image = feature
	}
	return nil
}

func (d *DB) lookupVideoContent(
	ctx context.Context,
	shaText string,
	requestedFields uint32,
	requestedFrames uint8,
	state *ContentState,
) error {
	if requestedFields&proto.FieldVideoMetadata != 0 {
		container, streams, complete, err := loadVideoMetadata(ctx, d.db, shaText)
		if err != nil {
			return err
		}
		if complete {
			state.VideoContainer = container
			state.VideoStreams = streams
			state.FieldsPresent |= proto.FieldVideoMetadata
			state.MissingFields &^= proto.FieldVideoMetadata
		}
	}
	wantsHeader := requestedFields&(proto.FieldThumb|proto.FieldVideoDuration|proto.FieldVideoContactSheet) != 0
	if wantsHeader {
		var storedSHA string
		var duration sql.NullInt64
		var thumbPath sql.NullString
		var thumbPDQ []byte
		var thumbQuality, thumbWidth, thumbHeight sql.NullInt64
		err := d.db.QueryRowContext(ctx, `
			SELECT sha512, duration_ms, thumb_path, thumb_pdq256, thumb_quality,
			       thumb_width, thumb_height
			FROM video_features WHERE sha512=?1`, shaText,
		).Scan(
			&storedSHA, &duration, &thumbPath, &thumbPDQ, &thumbQuality,
			&thumbWidth, &thumbHeight,
		)
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("store: lookup video content: %w", err)
		}
		if err == nil {
			decodedSHA, decodeErr := hex.DecodeString(storedSHA)
			if decodeErr == nil && len(decodedSHA) == 64 {
				feature := &VideoFeature{SHA512: decodedSHA}
				durationOK := duration.Valid && duration.Int64 >= 0
				legacyContactOK := durationOK && thumbPath.Valid && thumbPath.String != "" &&
					len(thumbPDQ) == 32 && thumbQuality.Valid &&
					thumbQuality.Int64 >= 0 && thumbQuality.Int64 <= 100
				contactOK := legacyContactOK &&
					thumbWidth.Valid && thumbWidth.Int64 > 0 &&
					thumbHeight.Valid && thumbHeight.Int64 > 0
				if durationOK && (requestedFields&(proto.FieldVideoDuration|proto.FieldThumb) != 0) {
					value := duration.Int64
					feature.DurationMS = &value
				}
				wantsLegacyContact := requestedFields&proto.FieldThumb != 0 && legacyContactOK
				wantsContact := requestedFields&proto.FieldVideoContactSheet != 0 && contactOK
				if wantsLegacyContact || wantsContact {
					value := int32(thumbQuality.Int64)
					feature.ThumbPath = thumbPath.String
					feature.ThumbPDQ = append([]byte(nil), thumbPDQ...)
					feature.ThumbQuality = &value
					if contactOK {
						width := int32(thumbWidth.Int64)
						height := int32(thumbHeight.Int64)
						feature.ThumbWidth = &width
						feature.ThumbHeight = &height
					}
				}
				if requestedFields&proto.FieldVideoDuration != 0 && durationOK {
					state.FieldsPresent |= proto.FieldVideoDuration
					state.MissingFields &^= proto.FieldVideoDuration
				}
				if requestedFields&proto.FieldVideoContactSheet != 0 && contactOK {
					state.FieldsPresent |= proto.FieldVideoContactSheet
					state.MissingFields &^= proto.FieldVideoContactSheet
				}
				if requestedFields&proto.FieldThumb != 0 && legacyContactOK {
					state.FieldsPresent |= proto.FieldThumb
					state.MissingFields &^= proto.FieldThumb
				}
				headerFields := proto.FieldThumb | proto.FieldVideoDuration |
					proto.FieldVideoContactSheet
				if state.FieldsPresent&headerFields != 0 {
					state.Video = feature
				}
			}
		}
	}

	if requestedFrames != 0 {
		rows, err := d.db.QueryContext(ctx, `
			SELECT frame_idx, pdq256, phash_parts, sobel_hist
			FROM video_frames
			WHERE sha512=?1 AND frame_idx BETWEEN 0 AND 5
			ORDER BY frame_idx`, shaText,
		)
		if err != nil {
			return fmt.Errorf("store: lookup video frames: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var frame VideoFrameFeature
			if err := rows.Scan(&frame.FrameIdx, &frame.PDQ256, &frame.PHashParts, &frame.SobelHist); err != nil {
				return fmt.Errorf("store: scan video content frame: %w", err)
			}
			bit := uint8(1 << uint(frame.FrameIdx))
			if requestedFrames&bit == 0 || !videoFramePayloadValid(requestedFields, frame.PDQ256, frame.PHashParts, frame.SobelHist) {
				continue
			}
			if requestedFields&proto.FieldVideo6F != 0 {
				frame.PDQ256 = append([]byte(nil), frame.PDQ256...)
			} else {
				frame.PDQ256 = nil
			}
			if requestedFields&(proto.FieldVideo6F|proto.FieldVideo6FPHash) != 0 {
				frame.PHashParts = append([]byte(nil), frame.PHashParts...)
			} else {
				frame.PHashParts = nil
			}
			if requestedFields&(proto.FieldVideo6F|proto.FieldVideo6FSobel) != 0 {
				frame.SobelHist = append([]byte(nil), frame.SobelHist...)
			} else {
				frame.SobelHist = nil
			}
			state.Frames = append(state.Frames, frame)
			state.FramesPresent |= bit
			state.MissingFrames &^= bit
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: iterate video content frames: %w", err)
		}
	}
	if state.MissingFrames == 0 {
		completed := requestedFields & videoSixFrameFields()
		state.FieldsPresent |= completed
		state.MissingFields &^= completed
	}
	return nil
}

func videoSixFrameFields() uint32 {
	return proto.FieldVideo6F | proto.FieldVideo6FPHash | proto.FieldVideo6FSobel
}

func videoFramePayloadValid(fields uint32, pdq, pHash, sobel []byte) bool {
	if fields&proto.FieldVideo6F != 0 && len(pdq) != 32 {
		return false
	}
	if fields&(proto.FieldVideo6F|proto.FieldVideo6FPHash) != 0 {
		if _, err := features.DecodePHashParts(pHash); err != nil {
			return false
		}
	}
	if fields&(proto.FieldVideo6F|proto.FieldVideo6FSobel) != 0 {
		if _, err := features.DecodeSobelHist(sobel); err != nil {
			return false
		}
	}
	return true
}
