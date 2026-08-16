package proto

import (
	"encoding/binary"
	"errors"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

func TestConnRoundTripUsesNamedMapFields(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()

	send := NewConn(left)
	recv := NewConn(right)
	want := ScanTask{
		TaskID:     "task-1",
		InstanceID: "instance-1",
		Roots:      []string{`D:\媒体`, `E:\video`},
		Phase:      1,
		Options: ScanOptions{
			Rescan:     true,
			Extensions: []string{".jpg"},
		},
	}

	errCh := make(chan error, 1)
	go func() { errCh <- send.WriteFrame(MsgScanTask, &want) }()

	msgType, body, err := recv.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if msgType != MsgScanTask {
		t.Fatalf("message type = %d, want %d", msgType, MsgScanTask)
	}

	var raw map[string]any
	if err := msgpack.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode body as map: %v", err)
	}
	if raw["task_id"] != "task-1" || raw["instance_id"] != "instance-1" {
		t.Fatalf("scan identity = %#v/%#v", raw["task_id"], raw["instance_id"])
	}

	decoded, err := Decode(msgType, body)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got := decoded.(*ScanTask)
	if got.TaskID != want.TaskID || got.InstanceID != want.InstanceID || got.Phase != want.Phase ||
		len(got.Roots) != 2 || got.Roots[0] != want.Roots[0] ||
		!got.Options.Rescan || len(got.Options.Extensions) != 1 {
		t.Fatalf("round trip mismatch: %#v", got)
	}
}

func TestReadFrameRejectsInvalidLengthsAndGarbage(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		wantErr error
	}{
		{
			name:    "zero length",
			payload: []byte{0, 0, 0, 0},
			wantErr: ErrFrameTooLarge,
		},
		{
			name: "over 16MB",
			payload: func() []byte {
				var header [4]byte
				binary.BigEndian.PutUint32(header[:], uint32((16<<20)+1))
				return header[:]
			}(),
			wantErr: ErrFrameTooLarge,
		},
		{
			name: "garbage envelope",
			payload: func() []byte {
				body := []byte{0xc1, 0xc1, 0xc1}
				var header [4]byte
				binary.BigEndian.PutUint32(header[:], uint32(len(body)))
				return append(header[:], body...)
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			left, right := net.Pipe()
			defer left.Close()
			defer right.Close()
			go func() {
				_, _ = left.Write(tt.payload)
				_ = left.Close()
			}()

			_, _, err := NewConn(right).ReadFrame()
			if err == nil {
				t.Fatal("ReadFrame returned nil error")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestWriteFrameRejectsPayloadOver16MB(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()

	err := NewConn(left).WriteFrame(MsgError, &Error{
		Stage: "proto",
		Msg:   strings.Repeat("x", (16<<20)+1),
	})
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("error = %v, want ErrFrameTooLarge", err)
	}
}

func TestWriteFrameTimesOutWhenPeerStopsReading(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	writer := NewConn(left)
	writer.SetWriteTimeout(25 * time.Millisecond)

	started := time.Now()
	err := writer.WriteFrame(MsgError, &Error{
		Stage: "proto",
		Msg:   strings.Repeat("x", 1<<20),
	})
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("WriteFrame error = %v, want network timeout", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("WriteFrame blocked for %v despite timeout", elapsed)
	}
}

func TestConcurrentWritesNeverInterleaveFrames(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	writer := NewConn(left)
	reader := NewConn(right)

	const goroutines = 16
	const perGoroutine = 1000
	errCh := make(chan error, goroutines)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				if err := writer.WriteFrame(MsgPing, &Ping{TS: int64(g*perGoroutine + i)}); err != nil {
					errCh <- err
					return
				}
			}
		}(g)
	}
	go func() {
		wg.Wait()
		close(errCh)
	}()

	seen := make(map[int64]bool, goroutines*perGoroutine)
	for i := 0; i < goroutines*perGoroutine; i++ {
		msgType, body, err := reader.ReadFrame()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		msg, err := Decode(msgType, body)
		if err != nil {
			t.Fatalf("decode frame %d: %v", i, err)
		}
		ts := msg.(*Ping).TS
		if seen[ts] {
			t.Fatalf("duplicate timestamp %d", ts)
		}
		seen[ts] = true
	}
	for err := range errCh {
		t.Fatalf("writer: %v", err)
	}
}

func TestDecodeSupportsReservedV12Messages(t *testing.T) {
	for _, tt := range []struct {
		msgType uint8
		value   any
	}{
		{MsgStatsQuery, &StatsQuery{}},
		{MsgStatsReport, &StatsReport{CPU: 0.5, Workers: 4}},
	} {
		body, err := msgpack.Marshal(tt.value)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Decode(tt.msgType, body); err != nil {
			t.Fatalf("Decode(%d): %v", tt.msgType, err)
		}
	}
}

func TestStatsMessagesRoundTripAppendOnlyFields(t *testing.T) {
	queryBody, err := msgpack.Marshal(&StatsQuery{WindowSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	decodedQuery, err := Decode(MsgStatsQuery, queryBody)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodedQuery.(*StatsQuery).WindowSeconds; got != 60 {
		t.Fatalf("WindowSeconds = %d, want 60", got)
	}

	want := &StatsReport{
		Disks: []DiskStats{{
			DiskNo: 2, ReadBPS: 1234, BusyFraction: 0.75,
			FilesDone: 7, PendingBytes: 4096,
		}},
		CPU: 1.5, Workers: 4, WindowS: 60, RSSBytes: 100,
		HeapBytes: 80, Handles: 12, PendingBytes: 4096,
		FilesDone: 7, FilesFailed: 1, Crashes: 2,
		ReadP95MS: 3.5, DecodeP95MS: 8.25,
	}
	reportBody, err := msgpack.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	decodedReport, err := Decode(MsgStatsReport, reportBody)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodedReport.(*StatsReport); !reflect.DeepEqual(got, want) {
		t.Fatalf("StatsReport round trip = %#v, want %#v", got, want)
	}

	legacyBody, err := msgpack.Marshal(map[string]any{
		"cpu": float64(0.5), "workers": int64(4),
	})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := Decode(MsgStatsReport, legacyBody)
	if err != nil {
		t.Fatal(err)
	}
	gotLegacy := legacy.(*StatsReport)
	if gotLegacy.CPU != 0.5 || gotLegacy.Workers != 4 ||
		gotLegacy.WindowS != 0 || gotLegacy.PendingBytes != 0 {
		t.Fatalf("legacy StatsReport decoded incompatibly: %#v", gotLegacy)
	}
}

func TestDecodeRejectsUnknownMessageType(t *testing.T) {
	if _, err := Decode(255, []byte{0x80}); err == nil {
		t.Fatal("Decode returned nil error for unknown type")
	}
}

func TestExtendedFeatureAndTaskStatsUseLiteralCompatibleMapFields(t *testing.T) {
	duration := int64(5000)
	thumbQuality := int32(91)
	item := FeatureItem{
		Path: `D:\media\a.jpg`, SHA512: strings.Repeat("ab", 64),
		Size: 10, MTime: 20, Status: StatusPartial,
		FieldsDone: FieldSHA512, PDQ256: strings.Repeat("01", 32),
		Quality: 88, Width: 640, Height: 480, DurationMS: &duration,
		ThumbPath: `D:\cache\a.jpg`, ThumbPDQ256: strings.Repeat("02", 32),
		ThumbQuality: &thumbQuality,
		FieldErrors:  []FieldError{{Field: FieldPDQ256, Stage: "decode", Msg: "bad image"}},
	}
	body, err := msgpack.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := msgpack.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{
		"path", "sha512", "size", "mtime", "status", "fields_done",
		"pdq256", "quality", "width", "height", "duration_ms", "thumb_path",
		"thumb_pdq256", "thumb_quality", "field_errors",
	}
	for _, key := range wantKeys {
		if _, ok := raw[key]; !ok {
			t.Fatalf("encoded map missing literal key %q: %#v", key, raw)
		}
	}
	if _, exists := raw["FieldsDone"]; exists {
		t.Fatalf("Go field name leaked into protocol map: %#v", raw)
	}

	type oldFeatureItem struct {
		Path   string `msgpack:"path"`
		SHA512 string `msgpack:"sha512,omitempty"`
		Size   int64  `msgpack:"size"`
		MTime  int64  `msgpack:"mtime"`
		Status string `msgpack:"status"`
		Err    string `msgpack:"err,omitempty"`
	}
	var old oldFeatureItem
	if err := msgpack.Unmarshal(body, &old); err != nil {
		t.Fatalf("old receiver rejected new map additions: %v", err)
	}
	if old.Path != item.Path || old.SHA512 != item.SHA512 || old.Status != item.Status {
		t.Fatalf("old receiver lost original fields: %#v", old)
	}

	oldBody, err := msgpack.Marshal(oldFeatureItem{
		Path: `D:\old.bin`, SHA512: strings.Repeat("cd", 64), Status: StatusDone,
	})
	if err != nil {
		t.Fatal(err)
	}
	var current FeatureItem
	if err := msgpack.Unmarshal(oldBody, &current); err != nil {
		t.Fatalf("new receiver rejected old map: %v", err)
	}
	if current.Path != `D:\old.bin` || current.FieldsDone != 0 ||
		current.DurationMS != nil || len(current.FieldErrors) != 0 {
		t.Fatalf("new receiver defaults = %#v", current)
	}

	stats := TaskStats{
		Total: 7, Done: 6, Failed: 1, ElapsedMS: 900,
		FilesDone: 5, FilesFailed: 1, DecodeCalls: 4,
		ReadAttempts: 6, DecodeAttempts: 5,
		ThumbGenerated: 3, ThumbCacheHits: 2, SingleFlightHits: 1, Crashes: 1,
	}
	statsBody, err := msgpack.Marshal(stats)
	if err != nil {
		t.Fatal(err)
	}
	var statsRaw map[string]any
	if err := msgpack.Unmarshal(statsBody, &statsRaw); err != nil {
		t.Fatal(err)
	}
	wantStats := map[string]any{
		"files_done": int64(5), "files_failed": int64(1), "decode_calls": int64(4),
		"read_attempts": int64(6), "decode_attempts": int64(5),
		"thumb_generated": int64(3), "thumb_cache_hits": int64(2),
		"singleflight_hits": int64(1), "crashes": int64(1),
	}
	for key, want := range wantStats {
		if !reflect.DeepEqual(statsRaw[key], want) {
			t.Fatalf("TaskStats %s = %#v, want %#v", key, statsRaw[key], want)
		}
	}
}

func TestPhase2MessagesPreserveMapCompatibilityAndOptionalFields(t *testing.T) {
	item := Phase2Item{
		MachineID:  "machine-a",
		Path:       `D:\media\video.mp4`,
		SHA512:     strings.Repeat("ab", 64),
		Size:       1234,
		MTimeMS:    5678,
		DurationMS: 60000,
		Kind:       KindVideo,
		FieldsMask: FieldVideo6F,
		FrameMask:  FrameMaskFull,
	}
	body, err := msgpack.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := msgpack.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"machine_id", "path", "sha512", "size", "mtime_ms", "duration_ms", "kind", "fields_mask", "frame_mask"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("Phase2Item map missing %q: %#v", key, raw)
		}
	}

	type oldPhase2Item struct {
		Path       string `msgpack:"path"`
		FieldsMask uint32 `msgpack:"fields_mask"`
	}
	var old oldPhase2Item
	if err := msgpack.Unmarshal(body, &old); err != nil {
		t.Fatalf("old receiver rejected appended Phase2Item fields: %v", err)
	}
	if old.Path != item.Path || old.FieldsMask != item.FieldsMask {
		t.Fatalf("old Phase2Item = %#v, want path/mask preserved", old)
	}

	oldBody, err := msgpack.Marshal(oldPhase2Item{Path: `D:\old.jpg`, FieldsMask: FieldPHashParts})
	if err != nil {
		t.Fatal(err)
	}
	var current Phase2Item
	if err := msgpack.Unmarshal(oldBody, &current); err != nil {
		t.Fatalf("new receiver rejected old Phase2Item: %v", err)
	}
	if current.Path != `D:\old.jpg` || current.FieldsMask != FieldPHashParts || current.SHA512 != "" || current.MachineID != "" || current.FrameMask != 0 {
		t.Fatalf("new Phase2Item defaults = %#v", current)
	}

	feature := FeatureItem{
		Path:       `D:\media\image.jpg`,
		SHA512:     strings.Repeat("cd", 64),
		Status:     StatusPartial,
		FieldsDone: FieldPHashParts | FieldSobelHist,
		PHashParts: []byte{1, 3, 3, 0},
		SobelHist:  []byte{1, 4, 8, 0},
		Frames: []FrameFeature{{
			FrameIdx: 2, TimeMS: 12500, PDQ256: []byte{9}, Quality: 80,
			PHashParts: []byte{1, 3, 3, 0}, SobelHist: []byte{1, 4, 8, 0}, Error: "frame decode warning",
		}},
	}
	featureBody, err := msgpack.Marshal(feature)
	if err != nil {
		t.Fatal(err)
	}
	var featureRaw map[string]any
	if err := msgpack.Unmarshal(featureBody, &featureRaw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"phash_parts", "sobel_hist", "frames"} {
		if _, ok := featureRaw[key]; !ok {
			t.Fatalf("FeatureItem map missing %q: %#v", key, featureRaw)
		}
	}
	type oldFeatureItem struct {
		Path       string `msgpack:"path"`
		SHA512     string `msgpack:"sha512,omitempty"`
		Size       int64  `msgpack:"size"`
		MTime      int64  `msgpack:"mtime"`
		Status     string `msgpack:"status"`
		FieldsDone uint32 `msgpack:"fields_done,omitempty"`
	}
	var oldFeature oldFeatureItem
	if err := msgpack.Unmarshal(featureBody, &oldFeature); err != nil {
		t.Fatalf("old receiver rejected populated FeatureItem additions: %v", err)
	}
	if oldFeature.Path != feature.Path || oldFeature.SHA512 != feature.SHA512 || oldFeature.Status != feature.Status || oldFeature.FieldsDone != feature.FieldsDone {
		t.Fatalf("old FeatureItem receiver lost original fields: %#v", oldFeature)
	}
	var emptyRaw map[string]any
	if emptyBody, err := msgpack.Marshal(FeatureItem{}); err != nil {
		t.Fatal(err)
	} else if err := msgpack.Unmarshal(emptyBody, &emptyRaw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"phash_parts", "sobel_hist", "frames"} {
		if _, exists := emptyRaw[key]; exists {
			t.Fatalf("empty optional field %q was encoded: %#v", key, emptyRaw)
		}
	}
}

func TestPhase2ItemValidateEnforcesPortableTaskContract(t *testing.T) {
	validSHA := strings.Repeat("ab", 64)
	validImage := Phase2Item{
		MachineID: "machine-a", Path: `D:\media\image.jpg`, SHA512: validSHA,
		Size: 0, MTimeMS: 0, Kind: KindImage, FieldsMask: FieldPHashParts | FieldSobelHist,
	}
	validVideo := Phase2Item{
		MachineID: "machine-a", Path: `D:\media\video.mp4`, SHA512: validSHA,
		Size: 10, MTimeMS: 20, DurationMS: 1, Kind: KindVideo, FieldsMask: FieldVideo6F, FrameMask: FrameMaskFull,
	}
	for _, tt := range []struct {
		name string
		item Phase2Item
		want bool
	}{
		{"valid image with required zero values", validImage, true},
		{"valid video", validVideo, true},
		{"missing machine", func() Phase2Item { x := validImage; x.MachineID = ""; return x }(), false},
		{"missing path", func() Phase2Item { x := validImage; x.Path = ""; return x }(), false},
		{"uppercase SHA", func() Phase2Item { x := validImage; x.SHA512 = strings.Repeat("AB", 64); return x }(), false},
		{"short SHA", func() Phase2Item { x := validImage; x.SHA512 = "ab"; return x }(), false},
		{"negative size", func() Phase2Item { x := validImage; x.Size = -1; return x }(), false},
		{"negative mtime", func() Phase2Item { x := validImage; x.MTimeMS = -1; return x }(), false},
		{"invalid kind", func() Phase2Item { x := validImage; x.Kind = 99; return x }(), false},
		{"empty fields mask", func() Phase2Item { x := validImage; x.FieldsMask = 0; return x }(), false},
		{"phase one field", func() Phase2Item { x := validImage; x.FieldsMask = FieldSHA512; return x }(), false},
		{"image video mask", func() Phase2Item { x := validImage; x.FieldsMask = FieldVideo6F; return x }(), false},
		{"video missing duration", func() Phase2Item { x := validVideo; x.DurationMS = 0; return x }(), false},
		{"high frame mask bit", func() Phase2Item { x := validVideo; x.FrameMask = 0x40; return x }(), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.item.Validate()
			if (err == nil) != tt.want {
				t.Fatalf("Validate() error = %v, want valid=%t", err, tt.want)
			}
		})
	}
}

func TestPhase2ItemEncodesRequiredZeroValueKeys(t *testing.T) {
	item := Phase2Item{
		MachineID: "machine-a", Path: `D:\media\image.jpg`, SHA512: strings.Repeat("ab", 64),
		Size: 0, MTimeMS: 0, DurationMS: 0, Kind: KindImage, FieldsMask: FieldPHashParts, FrameMask: 0,
	}
	body, err := msgpack.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := msgpack.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"size", "mtime_ms", "duration_ms", "frame_mask"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("required zero-value key %q omitted: %#v", key, raw)
		}
	}
}

func TestDeleteProtocolNumericAssignmentsAndErrorCodesAreStable(t *testing.T) {
	messageTypes := []struct {
		name string
		got  uint8
		want uint8
	}{
		{"ping", MsgPing, 1},
		{"pong", MsgPong, 2},
		{"hello", MsgHello, 3},
		{"shutdown", MsgShutdown, 4},
		{"scan task", MsgScanTask, 10},
		{"task ack", MsgTaskAck, 11},
		{"phase2 task", MsgPhase2Task, 12},
		{"delete task", MsgDeleteTask, 13},
		{"config push", MsgConfigPush, 14},
		{"stats query", MsgStatsQuery, 15},
		{"task progress", MsgTaskProgress, 20},
		{"feature result", MsgFeatureResult, 21},
		{"task done", MsgTaskDone, 22},
		{"error", MsgError, 23},
		{"crash notice", MsgCrashNotice, 24},
		{"delete report", MsgDeleteReport, 25},
		{"stats report", MsgStatsReport, 26},
	}
	for _, tt := range messageTypes {
		if tt.got != tt.want {
			t.Errorf("%s message type = %d, want %d", tt.name, tt.got, tt.want)
		}
	}

	errorCodes := []struct {
		name string
		got  string
		want string
	}{
		{"not found", DeleteErrNotFound, "E_NOT_FOUND"},
		{"bad path", DeleteErrBadPath, "E_BAD_PATH"},
		{"path denied", DeleteErrPathDenied, "E_PATH_DENIED"},
		{"not confirmed", DeleteErrNotConfirmed, "E_NOT_CONFIRMED"},
		{"readonly", DeleteErrReadonly, "E_READONLY"},
		{"access denied", DeleteErrAccessDenied, "E_ACCESS_DENIED"},
		{"delete failed", DeleteErrDeleteFailed, "E_DELETE_FAILED"},
		{"recycle failed", DeleteErrRecycleFailed, "E_RECYCLE_FAILED"},
		{"in use", DeleteErrInUse, "E_IN_USE"},
		{"reparse", DeleteErrReparse, "E_REPARSE"},
		{"bad mode", DeleteErrBadMode, "E_BAD_MODE"},
		{"helper lost", DeleteErrHelperLost, "E_HELPER_LOST"},
	}
	for _, tt := range errorCodes {
		if tt.got != tt.want {
			t.Errorf("%s error code = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
	if ModeSoft != "soft" || ModeHard != "hard" {
		t.Fatalf("delete modes = %q/%q, want soft/hard", ModeSoft, ModeHard)
	}
}

func TestDeleteProtocolDecodesLegacyMapsWithZeroValueAdditions(t *testing.T) {
	legacyTaskBody, err := msgpack.Marshal(map[string]any{
		"task_id": "legacy-task",
		"entries": []string{`D:\media\old.jpg`},
	})
	if err != nil {
		t.Fatal(err)
	}
	decodedTask, err := Decode(MsgDeleteTask, legacyTaskBody)
	if err != nil {
		t.Fatalf("Decode legacy DeleteTask: %v", err)
	}
	task := decodedTask.(*DeleteTask)
	if task.TaskID != "legacy-task" || !reflect.DeepEqual(task.Entries, []string{`D:\media\old.jpg`}) ||
		task.Seq != 0 || task.LastSeq != 0 || task.Mode != "" || task.Confirmed {
		t.Fatalf("legacy DeleteTask decoded as %#v", task)
	}

	legacyReportBody, err := msgpack.Marshal(map[string]any{
		"task_id": "legacy-task",
		"entries": []map[string]any{{
			"path": `D:\media\old.jpg`,
			"ok":   false,
			"err":  "access denied",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	decodedReport, err := Decode(MsgDeleteReport, legacyReportBody)
	if err != nil {
		t.Fatalf("Decode legacy DeleteReport: %v", err)
	}
	report := decodedReport.(*DeleteReport)
	if report.TaskID != "legacy-task" || report.Seq != 0 || report.LastSeq != 0 ||
		report.Stats != (DeleteStats{}) || len(report.Entries) != 1 {
		t.Fatalf("legacy DeleteReport decoded as %#v", report)
	}
	result := report.Entries[0]
	if result.Path != `D:\media\old.jpg` || result.OK || result.Err != "access denied" ||
		result.ErrCode != "" || result.ReadonlyCleared || result.RecycledTo != "" ||
		result.Uncertain || result.StateSyncErr != "" {
		t.Fatalf("legacy DeleteResult decoded as %#v", result)
	}
}

func TestDeleteProtocolNewMapsRoundTripAndUnknownModeRemainsData(t *testing.T) {
	task := DeleteTask{
		TaskID:    "delete-7",
		Seq:       2,
		LastSeq:   4,
		Mode:      "future-mode",
		Confirmed: true,
		Entries:   []string{`D:\media\a.jpg`, `D:\media\b.jpg`},
	}
	body, err := msgpack.Marshal(&task)
	if err != nil {
		t.Fatal(err)
	}
	var taskRaw map[string]msgpack.RawMessage
	if err := msgpack.Unmarshal(body, &taskRaw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"task_id", "seq", "last_seq", "mode", "confirmed", "entries"} {
		if _, ok := taskRaw[key]; !ok {
			t.Fatalf("DeleteTask wire map missing %q", key)
		}
	}
	decoded, err := Decode(MsgDeleteTask, body)
	if err != nil {
		t.Fatalf("Decode DeleteTask with unknown mode: %v", err)
	}
	if got := decoded.(*DeleteTask); !reflect.DeepEqual(*got, task) {
		t.Fatalf("DeleteTask round trip = %#v, want %#v", got, task)
	}

	report := DeleteReport{
		TaskID:  "delete-7",
		Seq:     2,
		LastSeq: 4,
		Stats:   DeleteStats{Total: 2, OK: 1, Failed: 1, Uncertain: 1},
		Entries: []DeleteResult{{
			Path:            `D:\media\a.jpg`,
			OK:              true,
			ErrCode:         DeleteErrRecycleFailed,
			Err:             "recycle fallback used",
			ReadonlyCleared: true,
			RecycledTo:      `D:\$Recycle.Bin\a.jpg`,
			Uncertain:       true,
			StateSyncErr:    "database unavailable",
		}},
	}
	body, err = msgpack.Marshal(&report)
	if err != nil {
		t.Fatal(err)
	}
	var reportRaw map[string]msgpack.RawMessage
	if err := msgpack.Unmarshal(body, &reportRaw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"task_id", "seq", "last_seq", "stats", "entries"} {
		if _, ok := reportRaw[key]; !ok {
			t.Fatalf("DeleteReport wire map missing %q", key)
		}
	}
	var resultMaps []map[string]msgpack.RawMessage
	if err := msgpack.Unmarshal(reportRaw["entries"], &resultMaps); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"path", "ok", "err_code", "err", "readonly_cleared",
		"recycled_to", "uncertain", "state_sync_err",
	} {
		if _, ok := resultMaps[0][key]; !ok {
			t.Fatalf("DeleteResult wire map missing %q", key)
		}
	}
	decoded, err = Decode(MsgDeleteReport, body)
	if err != nil {
		t.Fatalf("Decode new DeleteReport: %v", err)
	}
	if got := decoded.(*DeleteReport); !reflect.DeepEqual(*got, report) {
		t.Fatalf("DeleteReport round trip = %#v, want %#v", got, report)
	}
}

func TestHelloOptionalIdentityAndRoleMapFields(t *testing.T) {
	hello := Hello{Version: 1, PID: 4321, Role: "helper"}
	body, err := msgpack.Marshal(&hello)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]msgpack.RawMessage
	if err := msgpack.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	var version, pid int
	var role string
	if err := msgpack.Unmarshal(raw["version"], &version); err != nil {
		t.Fatal(err)
	}
	if err := msgpack.Unmarshal(raw["pid"], &pid); err != nil {
		t.Fatal(err)
	}
	if err := msgpack.Unmarshal(raw["role"], &role); err != nil {
		t.Fatal(err)
	}
	if version != 1 || pid != 4321 || role != "helper" {
		t.Fatalf("Hello wire values = version=%d pid=%d role=%q", version, pid, role)
	}
	if _, ok := raw["machine_id"]; ok {
		t.Fatalf("empty machine_id was not omitted: %#v", raw)
	}
	if _, ok := raw["hostname"]; ok {
		t.Fatalf("empty hostname was not omitted: %#v", raw)
	}
}

func TestDecodeSupportsShutdown(t *testing.T) {
	decoded, err := Decode(MsgShutdown, []byte{0x80})
	if err != nil {
		t.Fatalf("Decode(MsgShutdown): %v", err)
	}
	if _, ok := decoded.(*Shutdown); !ok {
		t.Fatalf("Decode(MsgShutdown) type = %T, want *Shutdown", decoded)
	}
}

func TestVideoCoreProtocolFieldBitsRemainAppendOnly(t *testing.T) {
	if FieldThumb != 1<<2 {
		t.Fatalf("legacy FieldThumb bit = %#x, want %#x", FieldThumb, uint32(1<<2))
	}
	if FieldVideo6F != 1<<5 {
		t.Fatalf("legacy FieldVideo6F bit = %#x, want %#x", FieldVideo6F, uint32(1<<5))
	}
	if FieldVideoDuration != 1<<6 {
		t.Fatalf("FieldVideoDuration = %#x, want %#x", FieldVideoDuration, uint32(1<<6))
	}
	if FieldVideoContactSheet != 1<<7 {
		t.Fatalf("FieldVideoContactSheet = %#x, want %#x", FieldVideoContactSheet, uint32(1<<7))
	}
	if FrameMaskFull != 0x3f {
		t.Fatalf("FrameMaskFull = %#x, want 0x3f", FrameMaskFull)
	}
	if FieldThumb == FieldVideoContactSheet {
		t.Fatal("legacy FieldThumb bit was reinterpreted as the contact-sheet field")
	}
}
