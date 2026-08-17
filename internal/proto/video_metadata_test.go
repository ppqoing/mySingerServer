package proto

import (
	"reflect"
	"strings"
	"testing"

	"github.com/vmihailenco/msgpack/v5"
)

func TestVideoMetadataContractRejectsNonCanonicalAndOversizedPayloads(t *testing.T) {
	validContainer := VideoContainerMetadata{FormatName: "matroska", TagsJSON: `{}`}
	validStream := VideoStreamMetadata{
		Index: 0, MediaType: "video", CodecID: 27, CodecName: "h264", TagsJSON: `{}`,
	}

	for _, test := range []struct {
		name      string
		container VideoContainerMetadata
		streams   []VideoStreamMetadata
	}{
		{
			name:      "container tags are not canonical",
			container: VideoContainerMetadata{FormatName: "matroska", TagsJSON: `{"b":1,"a":2}`},
			streams:   []VideoStreamMetadata{validStream},
		},
		{
			name:      "stream tags exceed 64 KiB",
			container: validContainer,
			streams: []VideoStreamMetadata{{
				Index: 0, MediaType: "video", CodecID: 27, CodecName: "h264",
				TagsJSON: `{"x":"` + strings.Repeat("x", 64<<10) + `"}`,
			}},
		},
		{
			name:      "metadata exceeds 1 MiB",
			container: validContainer,
			streams: func() []VideoStreamMetadata {
				streams := make([]VideoStreamMetadata, 256)
				for index := range streams {
					streams[index] = validStream
					streams[index].Index = int32(index)
					streams[index].Title = strings.Repeat("x", 5<<10)
				}
				return streams
			}(),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateVideoMetadata(&test.container, test.streams); err == nil {
				t.Fatal("ValidateVideoMetadata accepted invalid metadata")
			}
		})
	}
}

func TestVideoMetadataFeatureItemMessageRoundTripsEveryFieldAndNil(t *testing.T) {
	value := int64(123)
	index := int32(2)
	item := FeatureItem{
		Path: `D:\clip.mkv`, FieldsDone: FieldVideoMetadata,
		VideoContainer: &VideoContainerMetadata{
			FormatName: "matroska", DurationUS: &value, TagsJSON: `{}`, PrimaryVideoStream: &index,
		},
		VideoStreams: []VideoStreamMetadata{
			{Index: 0, MediaType: "audio", CodecID: 86018, CodecName: "aac", TagsJSON: `{}`},
			{Index: 2, MediaType: "video", CodecID: 27, CodecName: "h264", DurationUS: &value, TagsJSON: `{}`},
		},
	}
	body, err := msgpack.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var got FeatureItem
	if err := msgpack.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, item) {
		t.Fatalf("metadata feature round trip mismatch\ngot=%#v\nwant=%#v", got, item)
	}
	if got.VideoStreams[0].DurationUS != nil || got.VideoStreams[0].Level != nil {
		t.Fatalf("N/A values became synthetic zeroes: %#v", got.VideoStreams[0])
	}
}

func TestVideoMetadataContractValidatesCompleteStreamSetAndNilUnknowns(t *testing.T) {
	container := VideoContainerMetadata{FormatName: "matroska", TagsJSON: `{}`}
	base := VideoStreamMetadata{
		Index: 0, MediaType: "video", CodecID: 27, CodecName: "h264", TagsJSON: `{}`,
	}
	if err := ValidateVideoMetadata(&container, []VideoStreamMetadata{base}); err != nil {
		t.Fatalf("valid metadata rejected: %v", err)
	}
	if container.StartTimeUS != nil || container.DurationUS != nil || base.Level != nil ||
		base.FrameCount != nil || base.SampleRate != nil {
		t.Fatal("unknown/N/A values must remain nil")
	}

	for _, test := range []struct {
		name    string
		streams []VideoStreamMetadata
	}{
		{"257 streams", append(make([]VideoStreamMetadata, 256), base)},
		{"duplicate stream index", []VideoStreamMetadata{base, base}},
		{"negative stream index", []VideoStreamMetadata{{Index: -1, MediaType: "video", CodecName: "h264", TagsJSON: `{}`}}},
		{"unknown media type", []VideoStreamMetadata{{Index: 0, MediaType: "unknown", CodecName: "x", TagsJSON: `{}`}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			streams := append([]VideoStreamMetadata(nil), test.streams...)
			for index := range streams {
				if streams[index].CodecName == "" {
					streams[index] = base
					streams[index].Index = int32(index)
				}
			}
			if test.name == "257 streams" {
				for index := range streams {
					streams[index] = base
					streams[index].Index = int32(index)
				}
			}
			if err := ValidateVideoMetadata(&container, streams); err == nil {
				t.Fatal("ValidateVideoMetadata accepted invalid stream collection")
			}
		})
	}
}
