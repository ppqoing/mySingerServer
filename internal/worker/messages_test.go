package worker

import (
	"reflect"
	"strings"
	"testing"

	"github.com/vmihailenco/msgpack/v5"
)

func TestRedactKnownPathHandlesMixedSeparatorsAndUnicode(t *testing.T) {
	tests := []struct {
		name string
		path string
		text string
	}{
		{
			name: "mixed separators",
			path: `D:\Private\Album\Secret.JPG`,
			text: `open D:/Private\Album/Secret.JPG failed`,
		},
		{
			name: "unicode case mapping",
			path: `D:\İstanbul\Album\Secret.JPG`,
			text: `open D:\İstanbul\Album\Secret.JPG failed`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			redacted := RedactKnownPath(test.text, test.path)
			lower := strings.ToLower(redacted)
			for _, secret := range []string{"private", "stanbul", "album", "secret.jpg"} {
				if strings.Contains(lower, secret) {
					t.Fatalf("redacted text leaked %q: %q", secret, redacted)
				}
			}
			if !strings.Contains(redacted, "<path>") {
				t.Fatalf("redacted text = %q, want path marker", redacted)
			}
		})
	}
}

// Break caught: an image preview is accidentally routed as a phase-2 feature
// job, or its encoded bytes/options are omitted from the worker wire payload.
func TestImagePreviewMessagesRoundTripMemoryPayload(t *testing.T) {
	job := JobMsg{
		JobID: 601, ScanTaskID: "preview-601", Path: `C:\media\preview.jpg`,
		Kind: MediaImage, Phase: PhasePreview, ScreenStage: ScreenStagePreview,
		Source: JobSourceLocal, Size: 1234, MTimeUnix: 1720000000,
		KnownSHA: bytes64(0x61), PreviewFormat: PreviewFormatJPEG,
		PreviewMaxWidth: 640, PreviewMaxHeight: 480, PreviewQuality: 82,
	}
	jobBody, err := msgpack.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	jobEnvelope := &Envelope{Type: MsgJob, Body: jobBody}
	decodedJob, err := DecodeBody[JobMsg](jobEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decodedJob, job) {
		t.Fatalf("preview job round trip = %#v, want %#v", decodedJob, job)
	}

	result := JobResultMsg{
		JobID: 601, Path: job.Path, Kind: MediaImage,
		SHA512: bytes64(0x61), PreviewFormat: PreviewFormatJPEG,
		PreviewWidth: 320, PreviewHeight: 240,
		PreviewBytes: []byte{0xff, 0xd8, 0xff, 0xd9},
	}
	resultBody, err := msgpack.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	resultEnvelope := &Envelope{Type: MsgResult, Body: resultBody}
	decodedResult, err := DecodeBody[JobResultMsg](resultEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decodedResult, result) {
		t.Fatalf("preview result round trip = %#v, want %#v", decodedResult, result)
	}
}

func TestMessageRoundTrip(t *testing.T) {
	duration := int64(6543)
	thumbQuality := int32(91)
	frame := FrameFeature{
		FrameIdx: 2, TimeMS: 2500, PDQ256: []byte{1, 2, 3},
		Quality: 73, PHashParts: []byte{4, 5}, SobelHist: []byte{6, 7},
		Error: "frame error",
	}
	cases := []struct {
		name  string
		value any
		new   func() any
	}{
		{"envelope", Envelope{Type: MsgJob, Body: []byte{1, 2, 3}}, func() any { return new(Envelope) }},
		{"ready", ReadyMsg{PID: 41, WorkerIndex: 3, IPCVersion: IPCCompatibilityVersion, DLLVersion: MediaCoreDLLVersion}, func() any { return new(ReadyMsg) }},
		{"job", JobMsg{JobID: 92, ScanTaskID: "550e8400-e29b-41d4-a716-446655440000", Path: `C:\media\sample.jpg`, Kind: MediaImage, Phase: Phase2, ScreenStage: ScreenStageTwo, Source: JobSourceManager, FieldsMask: MaskPHashParts, Size: 123456, MTimeUnix: 1720000000, KnownSHA: bytes64(7), MTimeMS: 1720000000123, FrameMask: 0x15, DurationMS: 6543}, func() any { return new(JobMsg) }},
		{"sha query", SHAQueryMsg{JobID: 93, SHA512: bytes64(8), Kind: MediaVideo}, func() any { return new(SHAQueryMsg) }},
		{"sha reply", SHAReplyMsg{JobID: 94, Found: true, PDQ: []byte{4, 5}, Quality: 86, Width: 1920, Height: 1080, DurationMS: &duration, ThumbPath: `C:\thumbs\sample.jpg`, ThumbPDQ: []byte{6, 7}, ThumbQuality: &thumbQuality}, func() any { return new(SHAReplyMsg) }},
		{"field error", FieldError{Field: MaskPHashParts, Stage: "decode", Msg: "invalid image"}, func() any { return new(FieldError) }},
		{"frame feature", frame, func() any { return new(FrameFeature) }},
		{"job result", JobResultMsg{JobID: 95, Path: `C:\media\clip.mp4`, Kind: MediaVideo, SHA512: bytes64(9), FieldsDone: MaskAllVideo, PDQ: []byte{8, 9}, Quality: 77, Width: 1280, Height: 720, DurationMS: &duration, ThumbPath: `C:\thumbs\clip.jpg`, ThumbPDQ: []byte{10, 11}, ThumbQuality: &thumbQuality, PHashParts: []byte{12, 13}, SobelHist: []byte{14, 15}, Frames: []FrameFeature{frame}, Errors: []FieldError{{Field: MaskVideo6F, Stage: "frames", Msg: "skipped"}}, ReadAttempts: 1, DecodeAttempts: 1, ReadNS: 12_000_000, DecodeNS: 34_000_000, ThumbMS: 56, Decoded: true, ThumbGenerated: true, ThumbCacheHit: false}, func() any { return new(JobResultMsg) }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := msgpack.Marshal(tc.value)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got := tc.new()
			if err := msgpack.Unmarshal(encoded, got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !reflect.DeepEqual(tc.value, reflect.ValueOf(got).Elem().Interface()) {
				t.Fatalf("round trip mismatch\nwant: %#v\n got: %#v", tc.value, got)
			}
		})
	}
}

// Break caught: lease protocol fields are dropped or renamed on the wire, so
// the Agent and Worker silently disagree about the lease they are exchanging.
func TestIOLeaseMessagesRoundTrip(t *testing.T) {
	acquire := IOLeaseAcquireMsg{
		JobID: 71, RequestID: 72, TaskID: "task-71", InstanceID: "instance-71",
		DiskKey: "disk-0", Class: 1, WantBytes: 4 << 20, WantSeek: true,
	}
	grant := IOLeaseGrantMsg{
		JobID: 71, RequestID: 72, LeaseID: 73, Generation: 4,
		Bytes: 4 << 20, Seeks: 1,
	}
	report := IOLeaseReportMsg{
		JobID: 71, RequestID: 72, LeaseID: 73, Generation: 4,
		TaskID: "task-71", InstanceID: "instance-71", DiskKey: "disk-0",
		Bytes: 3 << 20, Seeks: 1, ReadNS: 20_000, WaitNS: 30_000,
		Completed: true,
	}
	cancel := IOLeaseCancelMsg{JobID: 71, RequestID: 72}

	for _, tc := range []struct {
		name  string
		value any
		new   func() any
	}{
		{"acquire", acquire, func() any { return new(IOLeaseAcquireMsg) }},
		{"grant", grant, func() any { return new(IOLeaseGrantMsg) }},
		{"report", report, func() any { return new(IOLeaseReportMsg) }},
		{"cancel", cancel, func() any { return new(IOLeaseCancelMsg) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := msgpack.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			got := tc.new()
			if err := msgpack.Unmarshal(encoded, got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(tc.value, reflect.ValueOf(got).Elem().Interface()) {
				t.Fatalf("round trip mismatch\nwant: %#v\n got: %#v", tc.value, got)
			}
		})
	}
}

// Break caught: malformed or over-budget Worker lease requests reach the
// broker, or an over-sized/mismatched grant/report is accepted as authoritative.
func TestIOLeaseMessageValidationRejectsUnsafeBoundaries(t *testing.T) {
	validAcquire := IOLeaseAcquireMsg{
		JobID: 81, RequestID: 82, TaskID: "task-81", InstanceID: "instance-81",
		DiskKey: "disk-1", Class: 1, WantBytes: 4 << 20,
	}
	for _, tc := range []struct {
		name   string
		mutate func(*IOLeaseAcquireMsg)
	}{
		{"empty task", func(msg *IOLeaseAcquireMsg) { msg.TaskID = "" }},
		{"empty instance", func(msg *IOLeaseAcquireMsg) { msg.InstanceID = "" }},
		{"empty disk", func(msg *IOLeaseAcquireMsg) { msg.DiskKey = "" }},
		{"unknown class", func(msg *IOLeaseAcquireMsg) { msg.Class = 99 }},
		{"over 16 MiB", func(msg *IOLeaseAcquireMsg) { msg.WantBytes = (16 << 20) + 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := validAcquire
			tc.mutate(&msg)
			if err := msg.Validate(); err == nil {
				t.Fatalf("Validate(%#v) unexpectedly succeeded", msg)
			}
		})
	}

	validGrant := IOLeaseGrantMsg{
		JobID: 81, RequestID: 82, LeaseID: 83, Generation: 2,
		Bytes: validAcquire.WantBytes,
	}
	overGrant := validGrant
	overGrant.Bytes++
	if err := overGrant.ValidateFor(validAcquire); err == nil {
		t.Fatal("grant larger than request unexpectedly validated")
	}

	report := IOLeaseReportMsg{
		JobID: 81, RequestID: 82, LeaseID: 83, Generation: 3,
		TaskID: "task-81", InstanceID: "instance-81", DiskKey: "disk-1",
		Bytes: 1 << 20, Completed: true,
	}
	if err := report.ValidateFor(validGrant); err == nil {
		t.Fatal("report with mismatched generation unexpectedly validated")
	}

	report.Generation = validGrant.Generation
	report.Completed = false
	report.Cancelled = false
	if err := report.ValidateFor(validGrant); err == nil {
		t.Fatal("report without a terminal state unexpectedly validated")
	}
}

func TestDefaultStageOneWorkerMasksUseExplicitVideoFields(t *testing.T) {
	if MaskAllImage != MaskSHA512|MaskImagePDQ {
		t.Fatalf("image stage-one mask = %#x", uint32(MaskAllImage))
	}
	wantVideo := uint32(MaskSHA512 | MaskVideoDuration | MaskVideoContactSheet)
	if MaskAllVideo != wantVideo {
		t.Fatalf("video stage-one mask = %#x, want %#x", uint32(MaskAllVideo), wantVideo)
	}
	if MaskAllVideo&MaskVideoThumb != 0 {
		t.Fatalf("video stage-one mask retained legacy thumbnail bit %#x", uint32(MaskAllVideo))
	}
}

func TestExtendedWorkerMessagesUseLiteralMapAdditionsAndOldShapeDefaults(t *testing.T) {
	readyBody, err := msgpack.Marshal(ReadyMsg{
		PID: 41, WorkerIndex: 3,
		IPCVersion: IPCCompatibilityVersion,
		DLLVersion: MediaCoreDLLVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	var readyRaw map[string]any
	if err := msgpack.Unmarshal(readyBody, &readyRaw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"pid", "worker_index", "ipc_version", "dll_version"} {
		if _, exists := readyRaw[key]; !exists {
			t.Fatalf("Ready map missing literal key %q: %#v", key, readyRaw)
		}
	}

	oldReadyBody, err := msgpack.Marshal(map[string]any{
		"pid": 41, "worker_index": 3, "dll_version": MediaCoreDLLVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	var oldReady ReadyMsg
	if err := msgpack.Unmarshal(oldReadyBody, &oldReady); err != nil {
		t.Fatal(err)
	}
	if oldReady.IPCVersion != 0 {
		t.Fatalf("old Ready ipc_version=%d, want zero for supervisor rejection", oldReady.IPCVersion)
	}

	resultBody, err := msgpack.Marshal(JobResultMsg{
		JobID: 51, ScanTaskID: "task-a", Path: `D:\a.jpg`,
	})
	if err != nil {
		t.Fatal(err)
	}
	var resultRaw map[string]any
	if err := msgpack.Unmarshal(resultBody, &resultRaw); err != nil {
		t.Fatal(err)
	}
	if resultRaw["scan_task_id"] != "task-a" {
		t.Fatalf("JobResult scan_task_id = %#v", resultRaw["scan_task_id"])
	}

	replyBody, err := msgpack.Marshal(SHAReplyMsg{
		JobID: 52, Found: true, ReusedFlight: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var replyRaw map[string]any
	if err := msgpack.Unmarshal(replyBody, &replyRaw); err != nil {
		t.Fatal(err)
	}
	if replyRaw["reused_flight"] != true {
		t.Fatalf("SHAReply reused_flight = %#v", replyRaw["reused_flight"])
	}

	jobBody, err := msgpack.Marshal(JobMsg{
		JobID: 53, Phase: Phase2, MTimeMS: 1234, FrameMask: 0x21,
		DurationMS: 9876,
	})
	if err != nil {
		t.Fatal(err)
	}
	var jobRaw map[string]any
	if err := msgpack.Unmarshal(jobBody, &jobRaw); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]int64{
		"mtime_ms": 1234, "frame_mask": 0x21, "duration_ms": 9876,
	} {
		if got, ok := integerMapValue(jobRaw[key]); !ok || got != want {
			t.Fatalf("Job map key %q = %#v, want %d", key, jobRaw[key], want)
		}
	}

	oldJobBody, err := msgpack.Marshal(map[string]any{
		"job_id": int64(54), "phase": int8(Phase1),
	})
	if err != nil {
		t.Fatal(err)
	}
	var oldJob JobMsg
	if err := msgpack.Unmarshal(oldJobBody, &oldJob); err != nil {
		t.Fatal(err)
	}
	if oldJob.MTimeMS != 0 || oldJob.FrameMask != 0 || oldJob.DurationMS != 0 {
		t.Fatalf("old Job decoded additive fields = %#v, want zero defaults", oldJob)
	}

	phase2Body, err := msgpack.Marshal(JobResultMsg{
		JobID: 55, Phase: Phase2, PHashParts: []byte{1},
		SobelHist: []byte{2}, Frames: []FrameFeature{{FrameIdx: 3, TimeMS: 42}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var phase2Raw map[string]any
	if err := msgpack.Unmarshal(phase2Body, &phase2Raw); err != nil {
		t.Fatal(err)
	}
	if _, exists := phase2Raw["phase"]; exists {
		t.Fatalf("trusted Phase leaked onto worker wire: %#v", phase2Raw)
	}
	for _, key := range []string{"phash_parts", "sobel_hist", "frames"} {
		if _, exists := phase2Raw[key]; !exists {
			t.Fatalf("JobResult map missing literal key %q: %#v", key, phase2Raw)
		}
	}

	childBody, err := msgpack.Marshal(map[string]any{
		"job_id": int64(56), "phase": int8(Phase1),
	})
	if err != nil {
		t.Fatal(err)
	}
	var childResult JobResultMsg
	if err := msgpack.Unmarshal(childBody, &childResult); err != nil {
		t.Fatal(err)
	}
	if childResult.Phase != 0 {
		t.Fatalf("child supplied trusted Phase=%d, want ignored zero", childResult.Phase)
	}

	oldResultBody, err := msgpack.Marshal(map[string]any{"job_id": int64(57)})
	if err != nil {
		t.Fatal(err)
	}
	var oldResult JobResultMsg
	if err := msgpack.Unmarshal(oldResultBody, &oldResult); err != nil {
		t.Fatal(err)
	}
	if oldResult.Phase != 0 || oldResult.PHashParts != nil ||
		oldResult.SobelHist != nil || oldResult.Frames != nil {
		t.Fatalf("old JobResult decoded additive fields = %#v, want zero defaults", oldResult)
	}
}

func bytes64(seed byte) []byte {
	value := make([]byte, 64)
	for i := range value {
		value[i] = seed + byte(i)
	}
	return value
}

func integerMapValue(value any) (int64, bool) {
	switch typed := value.(type) {
	case int8:
		return int64(typed), true
	case int64:
		return typed, true
	case uint8:
		return int64(typed), true
	case uint64:
		return int64(typed), true
	default:
		return 0, false
	}
}

func TestVideoCoreProtocolReadyIsAdditiveAndMapCompatible(t *testing.T) {
	components := []RuntimeComponent{{
		Name: "avformat", BuildVersion: "63.1.0", RuntimeVersion: "63.1.2",
		BuildMajor: 63, RuntimeMajor: 63,
	}}
	ready := ReadyMsg{
		PID: 41, WorkerIndex: 3, IPCVersion: IPCCompatibilityVersion,
		DLLVersion: MediaCoreDLLVersion, VideoCoreABI: 1,
		VideoCoreVersion: "1.0.0", FFmpegComponents: components,
	}
	body, err := msgpack.Marshal(ready)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]msgpack.RawMessage
	if err := msgpack.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"pid", "worker_index", "ipc_version", "dll_version",
		"videocore_abi", "videocore_version", "ffmpeg_components",
	} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("Ready map missing additive key %q", key)
		}
	}

	legacy, err := msgpack.Marshal(map[string]any{
		"pid": 7, "worker_index": 2, "ipc_version": 1,
		"dll_version": "1.0.0", "future_ready_field": "ignored",
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded ReadyMsg
	if err := msgpack.Unmarshal(legacy, &decoded); err != nil {
		t.Fatalf("decode legacy Ready with unknown key: %v", err)
	}
	if decoded.VideoCoreABI != 0 || decoded.VideoCoreVersion != "" || decoded.FFmpegComponents != nil {
		t.Fatalf("legacy Ready additions = %#v, want safe zero values", decoded)
	}
}

func TestSHAReplyMasksRequireExactRequestedCoverage(t *testing.T) {
	validQuery := SHAQueryMsg{
		RequestedFields: MaskSHA512 | MaskVideoDuration | MaskVideoContactSheet,
		RequestedFrames: FrameMaskFull,
	}
	if err := validQuery.ValidateMasks(); err != nil {
		t.Fatalf("valid SHA query: %v", err)
	}
	if err := (SHAQueryMsg{RequestedFields: MaskSHA512, RequestedFrames: 0x40}).ValidateMasks(); err == nil {
		t.Fatal("SHA query accepted a frame bit outside FrameMaskFull")
	}

	validReply := SHAReplyMsg{
		RequestedFields: validQuery.RequestedFields,
		FieldsPresent:   MaskSHA512 | MaskVideoDuration,
		MissingFields:   MaskVideoContactSheet,
		RequestedFrames: FrameMaskFull,
		FramesPresent:   0x1f,
		MissingFrames:   0x20,
	}
	if err := validReply.ValidateMasks(); err != nil {
		t.Fatalf("valid SHA reply: %v", err)
	}

	invalid := []struct {
		name string
		edit func(*SHAReplyMsg)
	}{
		{"field overlap", func(reply *SHAReplyMsg) { reply.MissingFields |= MaskSHA512 }},
		{"field gap", func(reply *SHAReplyMsg) { reply.MissingFields = 0 }},
		{"foreign field", func(reply *SHAReplyMsg) { reply.FieldsPresent |= 1 << 31 }},
		{"frame overlap", func(reply *SHAReplyMsg) { reply.MissingFrames |= 1 }},
		{"frame gap", func(reply *SHAReplyMsg) { reply.MissingFrames = 0 }},
		{"foreign frame", func(reply *SHAReplyMsg) { reply.FramesPresent |= 0x40 }},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			got := validReply
			tc.edit(&got)
			if err := got.ValidateMasks(); err == nil {
				t.Fatalf("ValidateMasks accepted %#v", got)
			}
		})
	}
}

func TestMergedResultMapCompatibilityUsesExplicitFrameStatus(t *testing.T) {
	frames := [6]FrameResult{}
	for index := range frames {
		frames[index] = FrameResult{FrameIdx: index, Status: -10 - int32(index)}
	}
	frames[0] = FrameResult{
		FrameIdx: 0, Status: 0, TimeMS: 100,
		PDQ256: []byte{1}, Quality: 80,
	}
	result := JobResultMsg{
		JobID: 99, Path: `D:\media\clip.mp4`, Kind: MediaVideo,
		FieldsDone:         MaskVideoDuration | MaskVideo6F,
		FramesDone:         1,
		DurationStatus:     0,
		ContactSheetStatus: -20,
		ContactSheetWidth:  768,
		ContactSheetHeight: 512,
		FrameResults:       frames,
	}
	if err := result.ValidateVideoCoreMasks(); err != nil {
		t.Fatalf("valid merged result: %v", err)
	}
	body, err := msgpack.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]msgpack.RawMessage
	if err := msgpack.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"fields_done", "frames_done", "duration_status", "contact_sheet_status",
		"contact_sheet_width", "contact_sheet_height", "frame_results",
	} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("JobResult map missing additive key %q", key)
		}
	}
	var frameMaps []map[string]msgpack.RawMessage
	if err := msgpack.Unmarshal(raw["frame_results"], &frameMaps); err != nil {
		t.Fatal(err)
	}
	if len(frameMaps) != 6 {
		t.Fatalf("frame_results length = %d, want 6", len(frameMaps))
	}
	for index, frameMap := range frameMaps {
		if _, ok := frameMap["status"]; !ok {
			t.Fatalf("frame %d omitted explicit status", index)
		}
	}

	badStatus := result
	badStatus.FrameResults[1].Status = 0
	if err := badStatus.ValidateVideoCoreMasks(); err == nil {
		t.Fatal("result accepted an unsuccessful frame with success status")
	}
	badPayload := result
	badPayload.FrameResults[1].PDQ256 = []byte{9}
	if err := badPayload.ValidateVideoCoreMasks(); err == nil {
		t.Fatal("result accepted payload on a failed frame")
	}
	badMask := result
	badMask.FramesDone = 0x40
	if err := badMask.ValidateVideoCoreMasks(); err == nil {
		t.Fatal("result accepted frames_done outside FrameMaskFull")
	}
	declaredFramesWithImplicitSuccess := JobResultMsg{
		FieldsDone: MaskVideo6F,
	}
	if err := declaredFramesWithImplicitSuccess.ValidateVideoCoreMasks(); err == nil {
		t.Fatal("result declared MaskVideo6F with no done frames and six implicit success statuses")
	}
	for _, field := range []uint32{MaskVideo6FPHash, MaskVideo6FSobel} {
		declaredFramesWithImplicitSuccess.FieldsDone = field
		if err := declaredFramesWithImplicitSuccess.ValidateVideoCoreMasks(); err == nil {
			t.Fatalf("result declared split frame field %#x with no done frames and six implicit success statuses", field)
		}
	}

	legacy, err := msgpack.Marshal(map[string]any{
		"job_id": int64(7), "path": `D:\legacy.mp4`, "kind": int8(MediaVideo),
		"future_result_field": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded JobResultMsg
	if err := msgpack.Unmarshal(legacy, &decoded); err != nil {
		t.Fatalf("decode legacy JobResult with unknown key: %v", err)
	}
	if decoded.FramesDone != 0 || decoded.DurationStatus != 0 || decoded.ContactSheetStatus != 0 ||
		decoded.ContactSheetWidth != 0 || decoded.ContactSheetHeight != 0 ||
		!reflect.DeepEqual(decoded.FrameResults, [6]FrameResult{}) {
		t.Fatalf("legacy JobResult additions = %#v, want safe zero values", decoded)
	}
}
