package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"dedup/internal/proto"
)

type videoMetadataQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (d *DB) LookupVideoMetadata(
	ctx context.Context,
	sha []byte,
) (*proto.VideoContainerMetadata, []proto.VideoStreamMetadata, error) {
	shaText, err := encodeSHA512(sha)
	if err != nil {
		return nil, nil, err
	}
	container, streams, complete, err := loadVideoMetadata(ctx, d.db, shaText)
	if err != nil {
		return nil, nil, err
	}
	if !complete {
		return nil, nil, nil
	}
	return container, streams, nil
}

func replaceVideoMetadata(
	ctx context.Context,
	tx *sql.Tx,
	sha string,
	container *proto.VideoContainerMetadata,
	streams []proto.VideoStreamMetadata,
) error {
	if err := proto.ValidateVideoMetadata(container, streams); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO video_containers(
			sha512,format_name,format_long_name,start_time_us,duration_us,bit_rate,file_size,
			probe_score,tags_json,primary_video_stream,decoder_name
		) VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11)
		ON CONFLICT(sha512) DO UPDATE SET
			format_name=excluded.format_name,
			format_long_name=excluded.format_long_name,
			start_time_us=excluded.start_time_us,
			duration_us=excluded.duration_us,
			bit_rate=excluded.bit_rate,
			file_size=excluded.file_size,
			probe_score=excluded.probe_score,
			tags_json=excluded.tags_json,
			primary_video_stream=excluded.primary_video_stream,
			decoder_name=excluded.decoder_name;`,
		sha, container.FormatName, nullableMetadataText(container.FormatLongName),
		container.StartTimeUS, container.DurationUS, container.BitRate, container.FileSize,
		container.ProbeScore, container.TagsJSON, container.PrimaryVideoStream,
		nullableMetadataText(container.DecoderName),
	); err != nil {
		return fmt.Errorf("store: upsert video container: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM video_streams WHERE sha512=?1`, sha); err != nil {
		return fmt.Errorf("store: delete old video streams: %w", err)
	}
	ordered := append([]proto.VideoStreamMetadata(nil), streams...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Index < ordered[j].Index })
	for _, stream := range ordered {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO video_streams(
				sha512,stream_index,media_type,codec_id,codec_name,codec_long_name,codec_tag,
				profile,level,time_base,start_time_us,duration_us,bit_rate,frame_count,
				disposition,language,title,tags_json,pixel_format,bit_depth,width,height,
				sar,dar,avg_frame_rate,real_frame_rate,rotation,color_range,color_space,
				color_transfer,color_primaries,chroma_location,field_order,sample_format,
				sample_rate,channels,channel_layout,audio_bit_depth
			) VALUES(
				?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12,?13,?14,?15,?16,?17,?18,?19,
				?20,?21,?22,?23,?24,?25,?26,?27,?28,?29,?30,?31,?32,?33,?34,?35,?36,?37,?38
			);`,
			sha, stream.Index, stream.MediaType, stream.CodecID, stream.CodecName,
			nullableMetadataText(stream.CodecLongName), nullableMetadataText(stream.CodecTag),
			nullableMetadataText(stream.Profile), stream.Level, nullableMetadataText(stream.TimeBase),
			stream.StartTimeUS, stream.DurationUS, stream.BitRate, stream.FrameCount, stream.Disposition,
			nullableMetadataText(stream.Language), nullableMetadataText(stream.Title), stream.TagsJSON,
			nullableMetadataText(stream.PixelFormat), stream.BitDepth, stream.Width, stream.Height,
			nullableMetadataText(stream.SAR), nullableMetadataText(stream.DAR),
			nullableMetadataText(stream.AvgFrameRate), nullableMetadataText(stream.RealFrameRate),
			stream.Rotation, nullableMetadataText(stream.ColorRange), nullableMetadataText(stream.ColorSpace),
			nullableMetadataText(stream.ColorTransfer), nullableMetadataText(stream.ColorPrimaries),
			nullableMetadataText(stream.ChromaLocation), nullableMetadataText(stream.FieldOrder),
			nullableMetadataText(stream.SampleFormat), stream.SampleRate, stream.Channels,
			nullableMetadataText(stream.ChannelLayout), stream.AudioBitDepth,
		); err != nil {
			return fmt.Errorf("store: insert video stream %d: %w", stream.Index, err)
		}
	}
	return nil
}

func loadVideoMetadata(
	ctx context.Context,
	queryer videoMetadataQueryer,
	sha string,
) (*proto.VideoContainerMetadata, []proto.VideoStreamMetadata, bool, error) {
	var container proto.VideoContainerMetadata
	var formatLongName, decoderName sql.NullString
	var startTime, duration, bitRate, fileSize sql.NullInt64
	var probeScore, primaryVideo sql.NullInt64
	err := queryer.QueryRowContext(ctx, `
		SELECT format_name,format_long_name,start_time_us,duration_us,bit_rate,file_size,
		       probe_score,tags_json,primary_video_stream,decoder_name
		FROM video_containers WHERE sha512=?1`, sha,
	).Scan(
		&container.FormatName, &formatLongName, &startTime, &duration, &bitRate, &fileSize,
		&probeScore, &container.TagsJSON, &primaryVideo, &decoderName,
	)
	if err == sql.ErrNoRows {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("store: load video container: %w", err)
	}
	container.FormatLongName = nullStringValue(formatLongName)
	container.StartTimeUS = nullInt64Ptr(startTime)
	container.DurationUS = nullInt64Ptr(duration)
	container.BitRate = nullInt64Ptr(bitRate)
	container.FileSize = nullInt64Ptr(fileSize)
	container.ProbeScore = nullInt32Ptr(probeScore)
	container.PrimaryVideoStream = nullInt32Ptr(primaryVideo)
	container.DecoderName = nullStringValue(decoderName)

	rows, err := queryer.QueryContext(ctx, `
		SELECT stream_index,media_type,codec_id,codec_name,codec_long_name,codec_tag,
		       profile,level,time_base,start_time_us,duration_us,bit_rate,frame_count,
		       disposition,language,title,tags_json,pixel_format,bit_depth,width,height,
		       sar,dar,avg_frame_rate,real_frame_rate,rotation,color_range,color_space,
		       color_transfer,color_primaries,chroma_location,field_order,sample_format,
		       sample_rate,channels,channel_layout,audio_bit_depth
		FROM video_streams WHERE sha512=?1 ORDER BY stream_index`, sha)
	if err != nil {
		return nil, nil, false, fmt.Errorf("store: load video streams: %w", err)
	}
	defer rows.Close()
	var streams []proto.VideoStreamMetadata
	for rows.Next() {
		var stream proto.VideoStreamMetadata
		var codecLongName, codecTag, profile, timeBase, language, title sql.NullString
		var pixelFormat, sar, dar, avgFrameRate, realFrameRate sql.NullString
		var colorRange, colorSpace, colorTransfer, colorPrimaries, chromaLocation sql.NullString
		var fieldOrder, sampleFormat, channelLayout sql.NullString
		var level, bitDepth, width, height, rotation, sampleRate, channels, audioBitDepth sql.NullInt64
		var startTime, duration, bitRate, frameCount sql.NullInt64
		if err := rows.Scan(
			&stream.Index, &stream.MediaType, &stream.CodecID, &stream.CodecName,
			&codecLongName, &codecTag, &profile, &level, &timeBase, &startTime, &duration,
			&bitRate, &frameCount, &stream.Disposition, &language, &title, &stream.TagsJSON,
			&pixelFormat, &bitDepth, &width, &height, &sar, &dar, &avgFrameRate,
			&realFrameRate, &rotation, &colorRange, &colorSpace, &colorTransfer,
			&colorPrimaries, &chromaLocation, &fieldOrder, &sampleFormat, &sampleRate,
			&channels, &channelLayout, &audioBitDepth,
		); err != nil {
			return nil, nil, false, fmt.Errorf("store: scan video stream: %w", err)
		}
		stream.CodecLongName = nullStringValue(codecLongName)
		stream.CodecTag = nullStringValue(codecTag)
		stream.Profile = nullStringValue(profile)
		stream.Level = nullInt32Ptr(level)
		stream.TimeBase = nullStringValue(timeBase)
		stream.StartTimeUS = nullInt64Ptr(startTime)
		stream.DurationUS = nullInt64Ptr(duration)
		stream.BitRate = nullInt64Ptr(bitRate)
		stream.FrameCount = nullInt64Ptr(frameCount)
		stream.Language = nullStringValue(language)
		stream.Title = nullStringValue(title)
		stream.PixelFormat = nullStringValue(pixelFormat)
		stream.BitDepth = nullInt32Ptr(bitDepth)
		stream.Width = nullInt32Ptr(width)
		stream.Height = nullInt32Ptr(height)
		stream.SAR = nullStringValue(sar)
		stream.DAR = nullStringValue(dar)
		stream.AvgFrameRate = nullStringValue(avgFrameRate)
		stream.RealFrameRate = nullStringValue(realFrameRate)
		stream.Rotation = nullInt32Ptr(rotation)
		stream.ColorRange = nullStringValue(colorRange)
		stream.ColorSpace = nullStringValue(colorSpace)
		stream.ColorTransfer = nullStringValue(colorTransfer)
		stream.ColorPrimaries = nullStringValue(colorPrimaries)
		stream.ChromaLocation = nullStringValue(chromaLocation)
		stream.FieldOrder = nullStringValue(fieldOrder)
		stream.SampleFormat = nullStringValue(sampleFormat)
		stream.SampleRate = nullInt32Ptr(sampleRate)
		stream.Channels = nullInt32Ptr(channels)
		stream.ChannelLayout = nullStringValue(channelLayout)
		stream.AudioBitDepth = nullInt32Ptr(audioBitDepth)
		streams = append(streams, stream)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, false, fmt.Errorf("store: iterate video streams: %w", err)
	}
	if err := proto.ValidateVideoMetadata(&container, streams); err != nil {
		return nil, nil, false, nil
	}
	return &container, streams, true, nil
}

func nullableMetadataText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullStringValue(value sql.NullString) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

func nullInt64Ptr(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64
	return &result
}

func nullInt32Ptr(value sql.NullInt64) *int32 {
	if !value.Valid || value.Int64 < -1<<31 || value.Int64 > 1<<31-1 {
		return nil
	}
	result := int32(value.Int64)
	return &result
}
