package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"dedup/internal/store"
)

// PathID is a stable, non-reversible identifier for correlating media logs
// without disclosing a directory or filename.
func PathID(path string) string {
	canonical := strings.ToLower(strings.ReplaceAll(filepath.Clean(path), "/", `\`))
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:8])
}

func redactKnownPath(text, path string) string {
	if path == "" {
		return text
	}
	redacted := strings.ReplaceAll(text, path, "<path>")
	if base := filepath.Base(path); base != "." && base != string(filepath.Separator) {
		redacted = strings.ReplaceAll(redacted, base, "<path>")
	}
	return redacted
}

const (
	MsgReady    = "ready"
	MsgJob      = "job"
	MsgShutdown = "shutdown"
	MsgSHAQuery = "sha_query"
	MsgSHAReply = "sha_reply"
	MsgResult   = "result"
)

const (
	IPCCompatibilityVersion = 1
	MediaCoreDLLVersion     = "1.0.0"
	VideoCoreABIVersion     = 1
	VideoCoreVersion        = "1.0.0"
)

type Envelope struct {
	Type string `msgpack:"type"`
	Body []byte `msgpack:"body"`
}

type MediaKind int8

const (
	MediaImage MediaKind = 1
	MediaVideo MediaKind = 2
)

type Phase int8

const (
	Phase1 Phase = 1
	Phase2 Phase = 2
)

type ScreenStage uint8

const (
	ScreenStageLegacy ScreenStage = 0
	ScreenStageTwo    ScreenStage = 2
	ScreenStageThree  ScreenStage = 3
)

type JobSource string

const (
	JobSourceManager JobSource = "manager"
	JobSourceLocal   JobSource = "local"
	JobSourceScan    JobSource = "scan"
)

const (
	MaskSHA512   uint32 = 1 << 0
	MaskImagePDQ uint32 = 1 << 1
	// Deprecated: MaskVideoThumb retains legacy bit 2 for wire compatibility.
	MaskVideoThumb        uint32 = 1 << 2
	MaskPHashParts        uint32 = 1 << 3
	MaskSobelHist         uint32 = 1 << 4
	MaskVideo6F           uint32 = 1 << 5
	MaskVideoDuration     uint32 = 1 << 6
	MaskVideoContactSheet uint32 = 1 << 7
	MaskVideo6FPHash      uint32 = 1 << 8
	MaskVideo6FSobel      uint32 = 1 << 9
)

var (
	MaskAllImage = store.RequiredStageOneMask(store.MediaImage)
	MaskAllVideo = store.RequiredStageOneMask(store.MediaVideo)
)

const (
	FrameMaskFull uint8  = 0x3f
	fieldMaskFull uint32 = MaskSHA512 | MaskImagePDQ | MaskVideoThumb |
		MaskPHashParts | MaskSobelHist | MaskVideo6F |
		MaskVideoDuration | MaskVideoContactSheet |
		MaskVideo6FPHash | MaskVideo6FSobel
)

type RuntimeComponent struct {
	Name           string `msgpack:"name"`
	BuildVersion   string `msgpack:"build_version"`
	RuntimeVersion string `msgpack:"runtime_version"`
	BuildMajor     uint32 `msgpack:"build_major"`
	RuntimeMajor   uint32 `msgpack:"runtime_major"`
}

type ReadyMsg struct {
	PID              int                `msgpack:"pid"`
	WorkerIndex      int                `msgpack:"worker_index"`
	IPCVersion       int                `msgpack:"ipc_version"`
	DLLVersion       string             `msgpack:"dll_version"`
	VideoCoreABI     uint32             `msgpack:"videocore_abi"`
	VideoCoreVersion string             `msgpack:"videocore_version"`
	FFmpegComponents []RuntimeComponent `msgpack:"ffmpeg_components"`
}

type JobMsg struct {
	JobID       int64       `msgpack:"job_id"`
	ScanTaskID  string      `msgpack:"scan_task_id"`
	Path        string      `msgpack:"path"`
	Kind        MediaKind   `msgpack:"kind"`
	Phase       Phase       `msgpack:"phase"`
	ScreenStage ScreenStage `msgpack:"screen_stage,omitempty"`
	Source      JobSource   `msgpack:"source,omitempty"`
	FieldsMask  uint32      `msgpack:"fields_mask"`
	Size        int64       `msgpack:"size"`
	MTimeUnix   int64       `msgpack:"mtime_unix"`
	KnownSHA    []byte      `msgpack:"known_sha,omitempty"`
	MTimeMS     int64       `msgpack:"mtime_ms,omitempty"`
	FrameMask   uint8       `msgpack:"frame_mask,omitempty"`
	DurationMS  int64       `msgpack:"duration_ms,omitempty"`
}

type SHAQueryMsg struct {
	JobID           int64     `msgpack:"job_id"`
	ScanTaskID      string    `msgpack:"-"`
	SHA512          []byte    `msgpack:"sha512"`
	Kind            MediaKind `msgpack:"kind"`
	RequestedFields uint32    `msgpack:"requested_fields"`
	RequestedFrames uint8     `msgpack:"requested_frames"`
}

type SHAReplyMsg struct {
	JobID           int64          `msgpack:"job_id"`
	Found           bool           `msgpack:"found"`
	RequestedFields uint32         `msgpack:"requested_fields"`
	FieldsPresent   uint32         `msgpack:"present_fields"`
	MissingFields   uint32         `msgpack:"missing_fields"`
	RequestedFrames uint8          `msgpack:"requested_frames"`
	FramesPresent   uint8          `msgpack:"present_frames"`
	MissingFrames   uint8          `msgpack:"missing_frames"`
	PDQ             []byte         `msgpack:"pdq,omitempty"`
	Quality         int32          `msgpack:"quality,omitempty"`
	Width           int32          `msgpack:"width,omitempty"`
	Height          int32          `msgpack:"height,omitempty"`
	DurationMS      *int64         `msgpack:"duration_ms,omitempty"`
	ThumbPath       string         `msgpack:"thumb_path,omitempty"`
	ThumbPDQ        []byte         `msgpack:"thumb_pdq,omitempty"`
	ThumbQuality    *int32         `msgpack:"thumb_quality,omitempty"`
	FrameResults    [6]FrameResult `msgpack:"frame_results,omitempty"`
	ReusedFlight    bool           `msgpack:"reused_flight,omitempty"`
}

func (msg SHAQueryMsg) ValidateMasks() error {
	if msg.RequestedFields&^fieldMaskFull != 0 {
		return fmt.Errorf("worker: SHA query requested_fields contains unknown bits %#x", msg.RequestedFields&^fieldMaskFull)
	}
	if msg.RequestedFrames&^FrameMaskFull != 0 {
		return fmt.Errorf("worker: SHA query requested_frames contains bits outside six frames")
	}
	return nil
}

func (msg SHAReplyMsg) ValidateMasks() error {
	if (msg.RequestedFields|msg.FieldsPresent|msg.MissingFields)&^fieldMaskFull != 0 {
		return fmt.Errorf("worker: SHA reply field masks contain unknown bits")
	}
	if msg.FieldsPresent&msg.MissingFields != 0 {
		return fmt.Errorf("worker: SHA reply present_fields overlaps missing_fields")
	}
	if msg.FieldsPresent|msg.MissingFields != msg.RequestedFields {
		return fmt.Errorf("worker: SHA reply field masks do not exactly cover requested_fields")
	}
	if (msg.RequestedFrames|msg.FramesPresent|msg.MissingFrames)&^FrameMaskFull != 0 {
		return fmt.Errorf("worker: SHA reply frame masks contain bits outside six frames")
	}
	if msg.FramesPresent&msg.MissingFrames != 0 {
		return fmt.Errorf("worker: SHA reply present_frames overlaps missing_frames")
	}
	if msg.FramesPresent|msg.MissingFrames != msg.RequestedFrames {
		return fmt.Errorf("worker: SHA reply frame masks do not exactly cover requested_frames")
	}
	return nil
}

type FieldError struct {
	Field uint32 `msgpack:"field"`
	Stage string `msgpack:"stage"`
	Msg   string `msgpack:"msg"`
}

type FrameFeature struct {
	FrameIdx   int    `msgpack:"frame_idx"`
	TimeMS     int64  `msgpack:"time_ms"`
	PDQ256     []byte `msgpack:"pdq256,omitempty"`
	Quality    int32  `msgpack:"quality,omitempty"`
	PHashParts []byte `msgpack:"phash_parts,omitempty"`
	SobelHist  []byte `msgpack:"sobel_hist,omitempty"`
	Error      string `msgpack:"error,omitempty"`
}

type FrameResult struct {
	FrameIdx   int    `msgpack:"frame_idx"`
	Status     int32  `msgpack:"status"`
	TimeMS     int64  `msgpack:"time_ms"`
	PDQ256     []byte `msgpack:"pdq256,omitempty"`
	Quality    int32  `msgpack:"quality,omitempty"`
	PHashParts []byte `msgpack:"phash_parts,omitempty"`
	SobelHist  []byte `msgpack:"sobel_hist,omitempty"`
}

type JobResultMsg struct {
	JobID              int64          `msgpack:"job_id"`
	ScanTaskID         string         `msgpack:"scan_task_id,omitempty"`
	WorkerPID          int            `msgpack:"-"`
	Phase              Phase          `msgpack:"-"`
	ScreenStage        ScreenStage    `msgpack:"screen_stage,omitempty"`
	Source             JobSource      `msgpack:"source,omitempty"`
	Path               string         `msgpack:"path"`
	Kind               MediaKind      `msgpack:"kind"`
	SHA512             []byte         `msgpack:"sha512,omitempty"`
	FieldsDone         uint32         `msgpack:"fields_done"`
	FramesDone         uint8          `msgpack:"frames_done"`
	DurationStatus     int32          `msgpack:"duration_status"`
	ContactSheetStatus int32          `msgpack:"contact_sheet_status"`
	ContactSheetWidth  int32          `msgpack:"contact_sheet_width"`
	ContactSheetHeight int32          `msgpack:"contact_sheet_height"`
	FrameResults       [6]FrameResult `msgpack:"frame_results"`
	PDQ                []byte         `msgpack:"pdq,omitempty"`
	Quality            int32          `msgpack:"quality,omitempty"`
	Width              int32          `msgpack:"width,omitempty"`
	Height             int32          `msgpack:"height,omitempty"`
	DurationMS         *int64         `msgpack:"duration_ms,omitempty"`
	ThumbPath          string         `msgpack:"thumb_path,omitempty"`
	ThumbPDQ           []byte         `msgpack:"thumb_pdq,omitempty"`
	ThumbQuality       *int32         `msgpack:"thumb_quality,omitempty"`
	PHashParts         []byte         `msgpack:"phash_parts,omitempty"`
	SobelHist          []byte         `msgpack:"sobel_hist,omitempty"`
	Frames             []FrameFeature `msgpack:"frames,omitempty"`
	Errors             []FieldError   `msgpack:"errors,omitempty"`
	ReadAttempts       int64          `msgpack:"read_attempts,omitempty"`
	DecodeAttempts     int64          `msgpack:"decode_attempts,omitempty"`
	ReadNS             int64          `msgpack:"read_ns,omitempty"`
	DecodeNS           int64          `msgpack:"decode_ns,omitempty"`
	ThumbMS            int64          `msgpack:"thumb_ms,omitempty"`
	Decoded            bool           `msgpack:"decoded,omitempty"`
	ThumbGenerated     bool           `msgpack:"thumb_generated,omitempty"`
	ThumbCacheHit      bool           `msgpack:"thumb_cache_hit,omitempty"`
}

func (msg JobResultMsg) ValidateVideoCoreMasks() error {
	if msg.FieldsDone&^fieldMaskFull != 0 {
		return fmt.Errorf("worker: job result fields_done contains unknown bits")
	}
	if msg.FramesDone&^FrameMaskFull != 0 {
		return fmt.Errorf("worker: job result frames_done contains bits outside six frames")
	}

	hasFrameResults := msg.FieldsDone&videoSixFrameWorkerFields() != 0 || msg.FramesDone != 0
	for _, frame := range msg.FrameResults {
		if frame.FrameIdx != 0 || frame.Status != 0 || frame.TimeMS != 0 || frameHasFeaturePayload(frame) {
			hasFrameResults = true
			break
		}
	}
	if !hasFrameResults {
		return nil
	}

	for index, frame := range msg.FrameResults {
		if frame.FrameIdx != index {
			return fmt.Errorf("worker: frame result slot %d has frame_idx %d", index, frame.FrameIdx)
		}
		done := msg.FramesDone&(1<<uint(index)) != 0
		if done && frame.Status != 0 {
			return fmt.Errorf("worker: successful frame %d has nonzero status %d", index, frame.Status)
		}
		if !done && frame.Status == 0 {
			return fmt.Errorf("worker: unsuccessful frame %d has success status", index)
		}
		if !done && frameHasFeaturePayload(frame) {
			return fmt.Errorf("worker: unsuccessful frame %d carries feature payload", index)
		}
	}
	return nil
}

func frameHasFeaturePayload(frame FrameResult) bool {
	return len(frame.PDQ256) != 0 || frame.Quality != 0 ||
		len(frame.PHashParts) != 0 || len(frame.SobelHist) != 0
}
