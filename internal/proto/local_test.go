package proto

import (
	"reflect"
	"strings"
	"testing"

	"github.com/vmihailenco/msgpack/v5"

	"dedup/internal/nodectl"
)

// These cases fail if local control messages permit more than the fixed
// 4 MiB boundary, unknown commands, or topics that cannot be stable keys.
func TestLocalEnvelopeRejectsOversizedPayloadAndUnknownOperation(t *testing.T) {
	oversized := make([]byte, LocalPayloadMaxBytes+1)
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{"request payload", (LocalRequest{RequestID: "request-1", Operation: LocalOperationStatusGet, Payload: oversized}).Validate(), LocalPayloadTooLargeErrorCode},
		{"response payload", (LocalResponse{RequestID: "request-1", Payload: oversized}).Validate(), LocalPayloadTooLargeErrorCode},
		{"event payload", (LocalEvent{Sequence: 1, Topic: "analysis.progress", Payload: oversized}).Validate(), LocalPayloadTooLargeErrorCode},
		{"unknown operation", (LocalRequest{RequestID: "request-1", Operation: "local.unknown"}).Validate(), UnsupportedOperationErrorCode},
		{"whitespace topic", (LocalEvent{Sequence: 1, Topic: " analysis.progress"}).Validate(), InvalidLocalTopicErrorCode},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err == nil {
				t.Fatalf("Validate() succeeded, want %q", tt.want)
			}
			if tt.err.Error() != tt.want {
				t.Fatalf("Validate() error = %q, want %q", tt.err, tt.want)
			}
		})
	}

	if err := (LocalEvent{Sequence: 1, Topic: "analysis.progress"}).Validate(); err != nil {
		t.Fatalf("valid LocalEvent.Validate(): %v", err)
	}
	if err := (LocalRequest{RequestID: "request-2", Operation: LocalOperationShutdown}).Validate(); err != nil {
		t.Fatalf("known LocalRequest.Validate(): %v", err)
	}
}

func TestLocalLifecyclePayloadsRoundTripWithoutNewMessageTypes(t *testing.T) {
	status := nodectl.Status{
		Component: nodectl.ComponentAgent, MachineID: "node-" + strings.Repeat("1", 64), PID: 17,
		ExecutablePath: `C:\agent.exe`, ConfigSHA256: strings.Repeat("a", 64),
		Lifecycle: "running", ServiceReady: true, Ready: true, SyncHealthy: true,
	}
	tests := []any{
		LocalStatusGetResponse{Status: status},
		LocalConfigGetResponse{CanonicalJSON: []byte("{\n}\n"), SHA256: strings.Repeat("b", 64)},
		LocalConfigRequest{CanonicalJSON: []byte("{\n}\n")},
		LocalConfigValidateResponse{Valid: true, SHA256: strings.Repeat("c", 64), RestartRequired: true},
		LocalConfigSaveResponse{SHA256: strings.Repeat("d", 64), RestartRequired: true},
		LocalShutdownResponse{Accepted: true},
		LocalTaskCreateRequest{TaskID: "task-1", Roots: []string{`D:\\media`}, Mode: LocalTaskModeScanThenAnalysis, Rescan: true, Extensions: []string{".jpg", ".mp4"}},
		LocalTaskCreateResponse{Task: LocalTask{TaskID: "task-1", Mode: LocalTaskModeScanOnly, Status: "pending"}},
		LocalTaskListRequest{Offset: 2, Limit: 20},
		LocalTaskListResponse{Tasks: []LocalTask{{TaskID: "task-1", Status: "running"}}, Offset: 2, NextOffset: 3},
		LocalTaskIDRequest{TaskID: "task-1"},
		LocalTaskRetryResponse{Task: LocalTask{TaskID: "task-1", Status: "pending"}},
	}
	for _, want := range tests {
		encoded, err := msgpack.Marshal(want)
		if err != nil {
			t.Fatalf("marshal %T: %v", want, err)
		}
		got := reflect.New(reflect.TypeOf(want)).Interface()
		if err := msgpack.Unmarshal(encoded, got); err != nil {
			t.Fatalf("unmarshal %T: %v", want, err)
		}
		if !reflect.DeepEqual(reflect.ValueOf(got).Elem().Interface(), want) {
			t.Fatalf("round trip %T = %#v, want %#v", want, got, want)
		}
	}
}

// Break caught: malformed or ambiguous task envelopes otherwise produce
// unstable digests and can recover a different scan after restart.
func TestLocalTaskCreateRequestValidateRejectsNonCanonicalEnvelope(t *testing.T) {
	valid := LocalTaskCreateRequest{
		TaskID: "task-1", Roots: []string{`D:\\media`, `E:\\archive`},
		Mode: LocalTaskModeScanThenAnalysis, Extensions: []string{".jpg", ".mp4"},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid request: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*LocalTaskCreateRequest)
	}{
		{"empty task", func(in *LocalTaskCreateRequest) { in.TaskID = "" }},
		{"task whitespace", func(in *LocalTaskCreateRequest) { in.TaskID = " task-1" }},
		{"empty roots", func(in *LocalTaskCreateRequest) { in.Roots = nil }},
		{"blank root", func(in *LocalTaskCreateRequest) { in.Roots[0] = " " }},
		{"duplicate roots", func(in *LocalTaskCreateRequest) { in.Roots[1] = `d:/media` }},
		{"invalid mode", func(in *LocalTaskCreateRequest) { in.Mode = "scan_and_guess" }},
		{"extension whitespace", func(in *LocalTaskCreateRequest) { in.Extensions[0] = " .jpg" }},
		{"extension without dot", func(in *LocalTaskCreateRequest) { in.Extensions[0] = "jpg" }},
		{"uppercase extension", func(in *LocalTaskCreateRequest) { in.Extensions[0] = ".JPG" }},
		{"duplicate extension", func(in *LocalTaskCreateRequest) { in.Extensions[1] = ".jpg" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := valid
			input.Roots = append([]string(nil), valid.Roots...)
			input.Extensions = append([]string(nil), valid.Extensions...)
			test.mutate(&input)
			if err := input.Validate(); err == nil {
				t.Fatal("Validate succeeded")
			}
		})
	}
}

// Break caught: local.preview.image accepts a caller-controlled path or
// unbounded dimensions instead of only a database file ID and safe options.
func TestLocalImagePreviewRequestWireHasNoPathAndValidatesBounds(t *testing.T) {
	request := LocalImagePreviewRequest{
		FileID: 41, MaxWidth: 640, MaxHeight: 480, Format: "webp", Quality: 82,
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	payload, err := EncodeLocalPayload(request)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]msgpack.RawMessage
	if err := msgpack.Unmarshal(payload, &raw); err != nil {
		t.Fatal(err)
	}
	if _, exists := raw["path"]; exists {
		t.Fatal("preview request exposed a path field")
	}
	for _, mutate := range []func(*LocalImagePreviewRequest){
		func(in *LocalImagePreviewRequest) { in.FileID = 0 },
		func(in *LocalImagePreviewRequest) { in.MaxWidth = 0 },
		func(in *LocalImagePreviewRequest) { in.MaxHeight = 8193 },
		func(in *LocalImagePreviewRequest) { in.Format = "png" },
		func(in *LocalImagePreviewRequest) { in.Quality = 101 },
	} {
		invalid := request
		mutate(&invalid)
		if err := invalid.Validate(); err == nil {
			t.Fatalf("invalid preview request accepted: %#v", invalid)
		}
	}
}

// Break caught: msgpack silently ignores a caller-controlled path field,
// defeating the file-ID-only boundary even though the Go DTO has no Path.
func TestLocalImagePreviewRequestStrictDecodeRejectsUnknownPath(t *testing.T) {
	payload, err := msgpack.Marshal(map[string]any{
		"file_id": int64(41), "max_width": int32(640), "max_height": int32(480),
		"format": "jpeg", "quality": int32(80), "path": `D:\private\source.jpg`,
	})
	if err != nil {
		t.Fatal(err)
	}
	var request LocalImagePreviewRequest
	if err := DecodeLocalImagePreviewPayload(payload, &request); err == nil {
		t.Fatal("preview request accepted unknown path field")
	}
}

// Break caught: making preview strict globally rejects additive fields on all
// existing local-control DTOs and breaks rolling NodeTray/Agent compatibility.
func TestDecodeLocalPayloadKeepsUnknownFieldCompatibilityOutsidePreview(t *testing.T) {
	payload, err := msgpack.Marshal(map[string]any{
		"offset": 0, "limit": 20, "future_optional_field": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var request LocalTaskListRequest
	if err := DecodeLocalPayload(payload, &request); err != nil {
		t.Fatalf("additive local field was rejected: %v", err)
	}
	if request.Offset != 0 || request.Limit != 20 {
		t.Fatalf("decoded request = %#v", request)
	}
}

// Break caught: exactly 4 MiB of image bytes becomes an oversized msgpack
// response after field overhead and is rejected only after expensive work.
func TestLocalImagePreviewResponseEncodingStaysWithinPayloadLimit(t *testing.T) {
	response := LocalImagePreviewResponse{
		MIME: "image/webp", Width: 640, Height: 480,
		Bytes: make([]byte, MaxLocalPreviewEncodedBytes),
	}
	payload, err := EncodeLocalPayload(response)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) > LocalPayloadMaxBytes {
		t.Fatalf("preview payload bytes=%d", len(payload))
	}
	response.Bytes = make([]byte, MaxLocalPreviewEncodedBytes+1)
	if _, err := EncodeLocalPayload(response); err == nil {
		t.Fatal("preview response above safe encoded-byte cap was accepted")
	}
}

// Break caught: group query and review submissions accept unstable paging or
// ambiguous decisions before reaching the Agent's SQLite boundary.
func TestLocalReviewDTOsValidatePagingFiltersAndExplicitDecisions(t *testing.T) {
	query := LocalGroupListRequest{Scope: "current", Category: "image", ReviewStatus: "undecided", Limit: 200}
	if err := query.Validate(); err != nil {
		t.Fatal(err)
	}
	query.Limit = 201
	if err := query.Validate(); err == nil {
		t.Fatal("group query limit above 200 was accepted")
	}
	query = LocalGroupListRequest{Scope: "history", Limit: 20}
	if err := query.Validate(); err == nil {
		t.Fatal("history query without run_id was accepted")
	}

	review := LocalReviewSaveRequest{
		RunID: "run-1", GroupID: "group-1", Reviewer: "local-user",
		Decisions: []LocalReviewDecision{{FileID: 1, Decision: "keep"}, {FileID: 2, Decision: "delete"}},
	}
	if err := review.Validate(); err != nil {
		t.Fatal(err)
	}
	review.Decisions[1].Decision = "erase"
	if err := review.Validate(); err == nil {
		t.Fatal("invalid review decision was accepted")
	}
}
