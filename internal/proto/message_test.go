package proto

import (
	"reflect"
	"strings"
	"testing"

	"github.com/vmihailenco/msgpack/v5"
)

// These cases fail if Decode omits a new message type or decodes its wire
// fields into the wrong concrete envelope.
func TestDecodeClientAuthAndLocalEnvelope(t *testing.T) {
	for _, tt := range []struct {
		name string
		got  uint8
		want uint8
	}{
		{"client auth", MsgClientAuth, 5},
		{"client auth result", MsgClientAuthResult, 6},
		{"local request", MsgLocalRequest, 30},
		{"local response", MsgLocalResponse, 31},
		{"local event", MsgLocalEvent, 32},
	} {
		if tt.got != tt.want {
			t.Fatalf("%s message type = %d, want %d", tt.name, tt.got, tt.want)
		}
	}

	tests := []struct {
		name    string
		msgType uint8
		value   any
		want    any
	}{
		{"client auth", MsgClientAuth, ClientAuth{Role: "nodetray", Token: "token", Version: ProtocolVersion}, &ClientAuth{}},
		{"client auth result", MsgClientAuthResult, ClientAuthResult{Accepted: true}, &ClientAuthResult{}},
		{"local request", MsgLocalRequest, LocalRequest{RequestID: "request-1", Operation: LocalOperationStatusGet, Payload: []byte{1}}, &LocalRequest{}},
		{"local response", MsgLocalResponse, LocalResponse{RequestID: "request-1", OK: false, ErrorCode: "invalid_config", Payload: []byte{2}}, &LocalResponse{}},
		{"local event", MsgLocalEvent, LocalEvent{Sequence: 7, Topic: "analysis.progress", Payload: []byte{3}}, &LocalEvent{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := msgpack.Marshal(tt.value)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := Decode(tt.msgType, body)
			if err != nil {
				t.Fatalf("Decode(%d): %v", tt.msgType, err)
			}
			if reflect.TypeOf(decoded) != reflect.TypeOf(tt.want) {
				t.Fatalf("Decode(%d) type = %T, want %T", tt.msgType, decoded, tt.want)
			}
			if !reflect.DeepEqual(reflect.ValueOf(decoded).Elem().Interface(), tt.value) {
				t.Fatalf("Decode(%d) value = %#v, want %#v", tt.msgType, reflect.ValueOf(decoded).Elem().Interface(), tt.value)
			}
		})
	}
}

// These cases fail if stage two accidentally permits Sobel work, or assigns
// the new video pHash bit to an image/video request incorrectly.
func TestPhase2TaskStageTwoAcceptsOnlyPHashFields(t *testing.T) {
	validSHA := strings.Repeat("ab", 64)
	for _, tt := range []struct {
		name string
		item Phase2Item
		want bool
	}{
		{"image pHash", validPhase2Image(validSHA, FieldPHashParts), true},
		{"image sobel", validPhase2Image(validSHA, FieldSobelHist), false},
		{"image combined", validPhase2Image(validSHA, FieldPHashParts|FieldSobelHist), false},
		{"video pHash", validPhase2Video(validSHA, FieldVideo6FPHash), true},
		{"video sobel", validPhase2Video(validSHA, FieldVideo6FSobel), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := (Phase2Task{TaskID: "task-1", Stage: ScreenStageTwo, Items: []Phase2Item{tt.item}}).Validate()
			if (err == nil) != tt.want {
				t.Fatalf("Stage 2 Validate() error = %v, want valid=%t", err, tt.want)
			}
		})
	}
}

// These cases fail if stage three accidentally permits pHash work, or assigns
// the new video Sobel bit to an image/video request incorrectly.
func TestPhase2TaskStageThreeAcceptsOnlySobelFields(t *testing.T) {
	validSHA := strings.Repeat("ab", 64)
	for _, tt := range []struct {
		name string
		item Phase2Item
		want bool
	}{
		{"image sobel", validPhase2Image(validSHA, FieldSobelHist), true},
		{"image pHash", validPhase2Image(validSHA, FieldPHashParts), false},
		{"image combined", validPhase2Image(validSHA, FieldPHashParts|FieldSobelHist), false},
		{"video sobel", validPhase2Video(validSHA, FieldVideo6FSobel), true},
		{"video pHash", validPhase2Video(validSHA, FieldVideo6FPHash), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := (Phase2Task{TaskID: "task-1", Stage: ScreenStageThree, Items: []Phase2Item{tt.item}}).Validate()
			if (err == nil) != tt.want {
				t.Fatalf("Stage 3 Validate() error = %v, want valid=%t", err, tt.want)
			}
		})
	}
}

// This case fails if adding staged work changes stage-zero's legacy combined
// image mask or legacy video frame mask behavior.
func TestLegacyPhase2TaskStillAcceptsCombinedFields(t *testing.T) {
	validSHA := strings.Repeat("ab", 64)
	task := Phase2Task{
		TaskID: "legacy-task",
		Items: []Phase2Item{
			validPhase2Image(validSHA, FieldPHashParts|FieldSobelHist),
			validPhase2Video(validSHA, FieldVideo6F),
		},
	}
	if err := task.Validate(); err != nil {
		t.Fatalf("legacy Phase2Task.Validate(): %v", err)
	}
}

func validPhase2Image(sha string, fields uint32) Phase2Item {
	return Phase2Item{MachineID: "machine-a", Path: `D:\media\image.jpg`, SHA512: sha, Kind: KindImage, FieldsMask: fields}
}

func validPhase2Video(sha string, fields uint32) Phase2Item {
	return Phase2Item{MachineID: "machine-a", Path: `D:\media\video.mp4`, SHA512: sha, Size: 1, MTimeMS: 1, DurationMS: 1, Kind: KindVideo, FrameMask: FrameMaskFull, FieldsMask: fields}
}
