package proto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/vmihailenco/msgpack/v5"

	"dedup/internal/nodectl"
)

func TestLocalTaskDisplayStatsJSONContract(t *testing.T) {
	encoded, err := json.Marshal(LocalTaskDisplayStats{
		SchemaVersion: LocalTaskDisplayStatsVersion,
		Speed:         12.5, Failures: 3, DurationMS: 192_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(encoded), `{"schema_version":1,"speed":12.5,"failures":3,"duration_ms":192000}`; got != want {
		t.Fatalf("json=%s, want %s", got, want)
	}
	var invalid LocalTaskDisplayStats
	if err := json.Unmarshal([]byte(`{"schema_version":1,"speed":"12.5","failures":3,"duration_ms":192000}`), &invalid); err == nil {
		t.Fatal("string speed unexpectedly accepted by numeric display stats contract")
	}
}

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

func TestLocalTaskControlPayloadRoundTrip(t *testing.T) {
	want := LocalTaskControlRequest{
		TaskID: "task-1", InstanceID: "instance-1", ExpectedRevision: 7,
	}
	payload, err := EncodeLocalPayload(want)
	if err != nil {
		t.Fatal(err)
	}
	var got LocalTaskControlRequest
	if err := DecodeLocalPayload(payload, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLocalTaskControlValidationRejectsUnstableIdentityOrRevision(t *testing.T) {
	valid := LocalTaskControlRequest{TaskID: "task-1", InstanceID: "instance-1", ExpectedRevision: 1}
	for name, mutate := range map[string]func(*LocalTaskControlRequest){
		"empty task id":       func(v *LocalTaskControlRequest) { v.TaskID = "" },
		"task id whitespace":  func(v *LocalTaskControlRequest) { v.TaskID = " task-1" },
		"task id trailing":    func(v *LocalTaskControlRequest) { v.TaskID = "task-1 " },
		"empty instance id":   func(v *LocalTaskControlRequest) { v.InstanceID = "" },
		"instance whitespace": func(v *LocalTaskControlRequest) { v.InstanceID = " instance-1" },
		"instance trailing":   func(v *LocalTaskControlRequest) { v.InstanceID = "instance-1 " },
		"zero revision":       func(v *LocalTaskControlRequest) { v.ExpectedRevision = 0 },
		"negative revision":   func(v *LocalTaskControlRequest) { v.ExpectedRevision = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if err := candidate.Validate(); err == nil || err.Error() != InvalidTaskControlErrorCode {
				t.Fatalf("Validate() error = %v, want %q", err, InvalidTaskControlErrorCode)
			}
		})
	}
}

func TestLocalTaskControlOperationsAreAllowed(t *testing.T) {
	for _, operation := range []string{
		LocalOperationTaskPause, LocalOperationTaskResume, LocalOperationTaskDelete,
	} {
		if !IsLocalOperation(operation) {
			t.Fatalf("IsLocalOperation(%q) = false", operation)
		}
	}
}

func TestLocalTaskControlResponseRoundTripPreservesCompleteSnapshot(t *testing.T) {
	want := LocalTaskControlResponse{
		Task: &LocalTask{
			TaskID: "task-1", InstanceID: "instance-1", Revision: 9,
			Source: "local", Mode: LocalTaskModeScanThenAnalysis, Stage: 2,
			Phase: "analysis", Status: "paused", Roots: []string{`D:\\media`},
			Rescan: true, Extensions: []string{".jpg"}, ProgressComplete: 4,
			ProgressTotal: 10, ProgressTotalKnown: true, StatsJSON: `{"stable":true}`,
			SafeErrorCode: "safe_error", SafeErrorMessage: "safe message",
			CreatedAt: 100, UpdatedAt: 200, StartedAt: 110, CompletedAt: 0,
		},
	}
	payload, err := EncodeLocalPayload(want)
	if err != nil {
		t.Fatal(err)
	}
	var got LocalTaskControlResponse
	if err := DecodeLocalPayload(payload, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}

	want = LocalTaskControlResponse{Deleted: true}
	payload, err = EncodeLocalPayload(want)
	if err != nil {
		t.Fatal(err)
	}
	var deleted LocalTaskControlResponse
	if err := DecodeLocalPayload(payload, &deleted); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(deleted, want) {
		t.Fatalf("deleted response = %#v, want %#v", deleted, want)
	}
}

func TestLegacyLocalTaskIDRequestStillDecodesForCancelAndRetry(t *testing.T) {
	payload, err := msgpack.Marshal(LocalTaskIDRequest{TaskID: "legacy-task"})
	if err != nil {
		t.Fatal(err)
	}
	var got LocalTaskIDRequest
	if err := DecodeLocalPayload(payload, &got); err != nil {
		t.Fatal(err)
	}
	if got.TaskID != "legacy-task" {
		t.Fatalf("legacy task id = %q", got.TaskID)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
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

// Break caught: a local delete request can carry caller-controlled paths or
// omit the committed review identity, bypassing the review-bound preview.
func TestLocalDeleteDTOsBindExecutionToPreparedReviewWithoutRequestPaths(t *testing.T) {
	prepare := LocalDeletePrepareRequest{RunID: "run-1", GroupID: "group-1"}
	if err := prepare.Validate(); err != nil {
		t.Fatalf("valid prepare: %v", err)
	}
	payload, err := EncodeLocalPayload(prepare)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]msgpack.RawMessage
	if err := msgpack.Unmarshal(payload, &raw); err != nil {
		t.Fatal(err)
	}
	if _, exists := raw["path"]; exists {
		t.Fatal("delete prepare exposed a request path")
	}

	execute := LocalDeleteExecuteRequest{BatchID: "batch-1", SelectionDigest: "digest", Token: "one-time"}
	if err := execute.Validate(); err != nil {
		t.Fatalf("valid execute: %v", err)
	}
	status := LocalDeleteStatusRequest{BatchID: "batch-1"}
	if err := status.Validate(); err != nil {
		t.Fatalf("valid status: %v", err)
	}

	for name, invalid := range map[string]any{
		"prepare run":    LocalDeletePrepareRequest{GroupID: "group-1"},
		"prepare group":  LocalDeletePrepareRequest{RunID: "run-1"},
		"execute batch":  LocalDeleteExecuteRequest{SelectionDigest: "digest", Token: "one-time"},
		"execute digest": LocalDeleteExecuteRequest{BatchID: "batch-1", Token: "one-time"},
		"execute token":  LocalDeleteExecuteRequest{BatchID: "batch-1", SelectionDigest: "digest"},
		"status batch":   LocalDeleteStatusRequest{},
	} {
		t.Run(name, func(t *testing.T) {
			validator := invalid.(interface{ Validate() error })
			if err := validator.Validate(); err == nil {
				t.Fatalf("invalid DTO accepted: %#v", invalid)
			}
		})
	}

	malicious, err := msgpack.Marshal(map[string]any{
		"run_id": "run-1", "group_id": "group-1", "path": `D:\\private\\source.jpg`,
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded LocalDeletePrepareRequest
	if err := DecodeLocalDeletePayload(malicious, &decoded); err == nil {
		t.Fatal("delete prepare accepted an unknown path")
	}
}
