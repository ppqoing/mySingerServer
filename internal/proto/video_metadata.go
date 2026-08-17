package proto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

const (
	maxVideoMetadataStreams  = 256
	maxVideoMetadataTagsSize = 64 << 10
	maxVideoMetadataSize     = 1 << 20
)

type VideoContainerMetadata struct {
	FormatName         string `msgpack:"format_name"`
	FormatLongName     string `msgpack:"format_long_name,omitempty"`
	StartTimeUS        *int64 `msgpack:"start_time_us,omitempty"`
	DurationUS         *int64 `msgpack:"duration_us,omitempty"`
	BitRate            *int64 `msgpack:"bit_rate,omitempty"`
	FileSize           *int64 `msgpack:"file_size,omitempty"`
	ProbeScore         *int32 `msgpack:"probe_score,omitempty"`
	TagsJSON           string `msgpack:"tags_json"`
	PrimaryVideoStream *int32 `msgpack:"primary_video_stream,omitempty"`
	DecoderName        string `msgpack:"decoder_name,omitempty"`
}

type VideoStreamMetadata struct {
	Index          int32  `msgpack:"index"`
	MediaType      string `msgpack:"media_type"`
	CodecID        int32  `msgpack:"codec_id"`
	CodecName      string `msgpack:"codec_name"`
	CodecLongName  string `msgpack:"codec_long_name,omitempty"`
	CodecTag       string `msgpack:"codec_tag,omitempty"`
	Profile        string `msgpack:"profile,omitempty"`
	Level          *int32 `msgpack:"level,omitempty"`
	TimeBase       string `msgpack:"time_base,omitempty"`
	StartTimeUS    *int64 `msgpack:"start_time_us,omitempty"`
	DurationUS     *int64 `msgpack:"duration_us,omitempty"`
	BitRate        *int64 `msgpack:"bit_rate,omitempty"`
	FrameCount     *int64 `msgpack:"frame_count,omitempty"`
	Disposition    uint32 `msgpack:"disposition"`
	Language       string `msgpack:"language,omitempty"`
	Title          string `msgpack:"title,omitempty"`
	TagsJSON       string `msgpack:"tags_json"`
	PixelFormat    string `msgpack:"pixel_format,omitempty"`
	BitDepth       *int32 `msgpack:"bit_depth,omitempty"`
	Width          *int32 `msgpack:"width,omitempty"`
	Height         *int32 `msgpack:"height,omitempty"`
	SAR            string `msgpack:"sar,omitempty"`
	DAR            string `msgpack:"dar,omitempty"`
	AvgFrameRate   string `msgpack:"avg_frame_rate,omitempty"`
	RealFrameRate  string `msgpack:"real_frame_rate,omitempty"`
	Rotation       *int32 `msgpack:"rotation,omitempty"`
	ColorRange     string `msgpack:"color_range,omitempty"`
	ColorSpace     string `msgpack:"color_space,omitempty"`
	ColorTransfer  string `msgpack:"color_transfer,omitempty"`
	ColorPrimaries string `msgpack:"color_primaries,omitempty"`
	ChromaLocation string `msgpack:"chroma_location,omitempty"`
	FieldOrder     string `msgpack:"field_order,omitempty"`
	SampleFormat   string `msgpack:"sample_format,omitempty"`
	ChannelLayout  string `msgpack:"channel_layout,omitempty"`
	SampleRate     *int32 `msgpack:"sample_rate,omitempty"`
	Channels       *int32 `msgpack:"channels,omitempty"`
	AudioBitDepth  *int32 `msgpack:"audio_bit_depth,omitempty"`
}

func ValidateVideoMetadata(container *VideoContainerMetadata, streams []VideoStreamMetadata) error {
	if container == nil {
		return fmt.Errorf("proto: video metadata container required")
	}
	if container.FormatName == "" {
		return fmt.Errorf("proto: video metadata format_name required")
	}
	if err := validateCanonicalTagsJSON("container", container.TagsJSON); err != nil {
		return err
	}
	if len(streams) > maxVideoMetadataStreams {
		return fmt.Errorf("proto: video metadata has %d streams, maximum is %d", len(streams), maxVideoMetadataStreams)
	}

	budget := videoMetadataBudget{remaining: maxVideoMetadataSize}
	if !consumeVideoContainerMetadataBudget(&budget, *container) {
		return fmt.Errorf("proto: video metadata exceeds %d bytes", maxVideoMetadataSize)
	}
	seen := make(map[int32]string, len(streams))
	for _, stream := range streams {
		if stream.Index < 0 {
			return fmt.Errorf("proto: video metadata stream index %d is negative", stream.Index)
		}
		if _, exists := seen[stream.Index]; exists {
			return fmt.Errorf("proto: duplicate video metadata stream index %d", stream.Index)
		}
		seen[stream.Index] = stream.MediaType
		switch stream.MediaType {
		case "video", "audio", "subtitle", "data", "attachment":
		default:
			return fmt.Errorf("proto: invalid video metadata media type %q", stream.MediaType)
		}
		if stream.CodecName == "" {
			return fmt.Errorf("proto: video metadata stream %d codec_name required", stream.Index)
		}
		if err := validateCanonicalTagsJSON(fmt.Sprintf("stream %d", stream.Index), stream.TagsJSON); err != nil {
			return err
		}
		if !consumeVideoStreamMetadataBudget(&budget, stream) {
			return fmt.Errorf("proto: video metadata exceeds %d bytes", maxVideoMetadataSize)
		}
	}
	if container.PrimaryVideoStream != nil {
		if mediaType, exists := seen[*container.PrimaryVideoStream]; !exists || mediaType != "video" {
			return fmt.Errorf("proto: primary video stream %d is not a video stream", *container.PrimaryVideoStream)
		}
	}
	return nil
}

func validateCanonicalTagsJSON(owner, value string) error {
	if len(value) > maxVideoMetadataTagsSize {
		return fmt.Errorf("proto: video metadata %s tags exceed %d bytes", owner, maxVideoMetadataTagsSize)
	}
	decoder := json.NewDecoder(bytes.NewBufferString(value))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return fmt.Errorf("proto: video metadata %s tags must be a JSON object", owner)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("proto: video metadata %s tags contain trailing JSON", owner)
	}
	canonical, err := json.Marshal(object)
	if err != nil || string(canonical) != value {
		return fmt.Errorf("proto: video metadata %s tags are not canonical JSON", owner)
	}
	return nil
}

type videoMetadataBudget struct {
	remaining int
}

func (budget *videoMetadataBudget) consume(part int) bool {
	if part < 0 || budget.remaining < 0 || part > budget.remaining {
		return false
	}
	budget.remaining -= part
	return true
}

func videoMetadataPartsFit(limit int, parts ...int) bool {
	budget := videoMetadataBudget{remaining: limit}
	for _, part := range parts {
		if !budget.consume(part) {
			return false
		}
	}
	return true
}

func consumeVideoContainerMetadataBudget(budget *videoMetadataBudget, value VideoContainerMetadata) bool {
	return consumeVideoMetadataBudgetParts(
		budget,
		64,
		len(value.FormatName),
		len(value.FormatLongName),
		len(value.TagsJSON),
		len(value.DecoderName),
	)
}

func consumeVideoStreamMetadataBudget(budget *videoMetadataBudget, value VideoStreamMetadata) bool {
	return consumeVideoMetadataBudgetParts(
		budget,
		160,
		len(value.MediaType),
		len(value.CodecName),
		len(value.CodecLongName),
		len(value.CodecTag),
		len(value.Profile),
		len(value.TimeBase),
		len(value.Language),
		len(value.Title),
		len(value.TagsJSON),
		len(value.PixelFormat),
		len(value.SAR),
		len(value.DAR),
		len(value.AvgFrameRate),
		len(value.RealFrameRate),
		len(value.ColorRange),
		len(value.ColorSpace),
		len(value.ColorTransfer),
		len(value.ColorPrimaries),
		len(value.ChromaLocation),
		len(value.FieldOrder),
		len(value.SampleFormat),
		len(value.ChannelLayout),
	)
}

func consumeVideoMetadataBudgetParts(budget *videoMetadataBudget, parts ...int) bool {
	for _, part := range parts {
		if !budget.consume(part) {
			return false
		}
	}
	return true
}
