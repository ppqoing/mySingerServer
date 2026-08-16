package proto

import (
	"fmt"
	"strings"

	"github.com/vmihailenco/msgpack/v5"
)

// Message types are split into connection management, GUI-to-Agent, and
// Agent-to-GUI ranges. New protocol versions may append fields and message
// types, but must not repurpose an existing value.
const (
	MsgPing             uint8 = 1
	MsgPong             uint8 = 2
	MsgHello            uint8 = 3
	MsgShutdown         uint8 = 4
	MsgClientAuth       uint8 = 5
	MsgClientAuthResult uint8 = 6

	MsgScanTask         uint8 = 10
	MsgTaskAck          uint8 = 11
	MsgPhase2Task       uint8 = 12
	MsgDeleteTask       uint8 = 13
	MsgConfigPush       uint8 = 14
	MsgStatsQuery       uint8 = 15
	MsgFilesystemBrowse uint8 = 16

	MsgTaskProgress           uint8 = 20
	MsgFeatureResult          uint8 = 21
	MsgTaskDone               uint8 = 22
	MsgError                  uint8 = 23
	MsgCrashNotice            uint8 = 24
	MsgDeleteReport           uint8 = 25
	MsgStatsReport            uint8 = 26
	MsgFilesystemBrowseResult uint8 = 27

	MsgLocalRequest  uint8 = 30
	MsgLocalResponse uint8 = 31
	MsgLocalEvent    uint8 = 32
)

const ProtocolVersion = 1

const (
	FilesystemEntryDrive     = "drive"
	FilesystemEntryDirectory = "directory"
	FilesystemEntryFile      = "file"
)

const (
	FieldSHA512 uint32 = 1 << 0
	FieldPDQ256 uint32 = 1 << 1
	// Deprecated: FieldThumb retains legacy bit 2 for wire compatibility.
	FieldThumb             uint32 = 1 << 2
	FieldPHashParts        uint32 = 1 << 3
	FieldSobelHist         uint32 = 1 << 4
	FieldVideo6F           uint32 = 1 << 5
	FieldVideoDuration     uint32 = 1 << 6
	FieldVideoContactSheet uint32 = 1 << 7
	FieldVideo6FPHash      uint32 = 1 << 8
	FieldVideo6FSobel      uint32 = 1 << 9
)

const FrameMaskFull uint8 = 0x3f

const (
	KindImage uint8 = 1
	KindVideo uint8 = 2
)

const (
	ScreenStageLegacy uint8 = 0
	ScreenStageTwo    uint8 = 2
	ScreenStageThree  uint8 = 3
)

const (
	StatusPending = "pending"
	StatusDone    = "done"
	StatusPartial = "partial"
	StatusFailed  = "failed"
	StatusCrash   = "crash"
	StatusDeleted = "deleted"
)

const (
	ModeSoft = "soft"
	ModeHard = "hard"
)

const (
	DeleteErrNotFound      = "E_NOT_FOUND"
	DeleteErrBadPath       = "E_BAD_PATH"
	DeleteErrPathDenied    = "E_PATH_DENIED"
	DeleteErrNotConfirmed  = "E_NOT_CONFIRMED"
	DeleteErrReadonly      = "E_READONLY"
	DeleteErrAccessDenied  = "E_ACCESS_DENIED"
	DeleteErrDeleteFailed  = "E_DELETE_FAILED"
	DeleteErrRecycleFailed = "E_RECYCLE_FAILED"
	DeleteErrInUse         = "E_IN_USE"
	DeleteErrReparse       = "E_REPARSE"
	DeleteErrBadMode       = "E_BAD_MODE"
	DeleteErrHelperLost    = "E_HELPER_LOST"
)

type Ping struct {
	TS int64 `msgpack:"ts"`
}

type Pong struct {
	TS int64 `msgpack:"ts"`
}

type FilesystemBrowseRequest struct {
	RequestID  string `msgpack:"request_id"`
	Path       string `msgpack:"path,omitempty"`
	ShowHidden bool   `msgpack:"show_hidden"`
	Cursor     string `msgpack:"cursor,omitempty"`
	Limit      int    `msgpack:"limit"`
}

func (request FilesystemBrowseRequest) Validate() error {
	if request.RequestID == "" {
		return fmt.Errorf("proto: filesystem browse request_id required")
	}
	if request.Path != "" && !isWindowsAbsoluteBrowsePath(request.Path) {
		return fmt.Errorf("proto: filesystem browse path must be drive-absolute or UNC")
	}
	if request.Limit < 0 || request.Limit > 500 {
		return fmt.Errorf("proto: filesystem browse limit must be between 1 and 500")
	}
	if request.Cursor != "" && len(request.Cursor) > 1024 {
		return fmt.Errorf("proto: filesystem browse cursor exceeds 1024 bytes")
	}
	return nil
}

func isWindowsAbsoluteBrowsePath(path string) bool {
	if len(path) >= 3 && isASCIIAlpha(path[0]) && path[1] == ':' && isPathSeparator(path[2]) {
		return true
	}
	if !strings.HasPrefix(path, `\\`) {
		return false
	}
	rest := path[2:]
	serverEnd := strings.IndexAny(rest, `\\/`)
	if serverEnd <= 0 {
		return false
	}
	share := strings.TrimLeft(rest[serverEnd:], `\\/`)
	return share != "" && strings.IndexAny(share, `\\/`) != 0
}

func isASCIIAlpha(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}

func isPathSeparator(value byte) bool {
	return value == '\\' || value == '/'
}

type FilesystemEntry struct {
	Name       string `msgpack:"name"`
	Path       string `msgpack:"path"`
	Kind       string `msgpack:"kind"`
	Hidden     bool   `msgpack:"hidden"`
	System     bool   `msgpack:"system"`
	Selectable bool   `msgpack:"selectable"`
}

type FilesystemBrowseResponse struct {
	RequestID   string            `msgpack:"request_id"`
	CurrentPath string            `msgpack:"current_path,omitempty"`
	ParentPath  string            `msgpack:"parent_path,omitempty"`
	Entries     []FilesystemEntry `msgpack:"entries"`
	NextCursor  string            `msgpack:"next_cursor,omitempty"`
	ErrorCode   string            `msgpack:"error_code,omitempty"`
}

type Hello struct {
	Version   int    `msgpack:"version"`
	MachineID string `msgpack:"machine_id,omitempty"`
	Hostname  string `msgpack:"hostname,omitempty"`
	PID       int    `msgpack:"pid"`
	Role      string `msgpack:"role,omitempty"`
}

type ScanTask struct {
	TaskID     string      `msgpack:"task_id"`
	InstanceID string      `msgpack:"instance_id,omitempty"`
	Roots      []string    `msgpack:"roots"`
	Phase      uint8       `msgpack:"phase"`
	Options    ScanOptions `msgpack:"options"`
}

type ScanOptions struct {
	Rescan     bool     `msgpack:"rescan"`
	Extensions []string `msgpack:"extensions,omitempty"`
}

type TaskAck struct {
	TaskID   string     `msgpack:"task_id"`
	Accepted bool       `msgpack:"accepted"`
	Reason   string     `msgpack:"reason"`
	Total    int64      `msgpack:"total"`
	Stats    *TaskStats `msgpack:"stats,omitempty"`
}

type Phase2Item struct {
	Path       string `msgpack:"path"`
	FieldsMask uint32 `msgpack:"fields_mask"`
	MachineID  string `msgpack:"machine_id"`
	SHA512     string `msgpack:"sha512"`
	Size       int64  `msgpack:"size"`
	MTimeMS    int64  `msgpack:"mtime_ms"`
	Kind       uint8  `msgpack:"kind"`
	FrameMask  uint8  `msgpack:"frame_mask"`
	DurationMS int64  `msgpack:"duration_ms"`
}

func (item Phase2Item) Validate() error {
	return item.validateForStage(ScreenStageLegacy)
}

func (item Phase2Item) validateForStage(stage uint8) error {
	if item.MachineID == "" {
		return fmt.Errorf("proto: phase2 item machine_id required")
	}
	if item.Path == "" {
		return fmt.Errorf("proto: phase2 item path required")
	}
	if !isCanonicalSHA512(item.SHA512) {
		return fmt.Errorf("proto: phase2 item sha512 must be 128 lowercase hexadecimal characters")
	}
	if item.Size < 0 || item.MTimeMS < 0 {
		return fmt.Errorf("proto: phase2 item size and mtime_ms must not be negative")
	}
	if item.FrameMask&^FrameMaskFull != 0 {
		return fmt.Errorf("proto: phase2 item frame_mask uses bits outside six frames")
	}
	if stage == ScreenStageLegacy && (item.FieldsMask == 0 || item.FieldsMask&^(FieldPHashParts|FieldSobelHist|FieldVideo6F) != 0) {
		return fmt.Errorf("proto: phase2 item fields_mask must contain only phase-2 fields")
	}
	switch item.Kind {
	case KindImage:
		switch stage {
		case ScreenStageLegacy:
			if item.FieldsMask&FieldVideo6F != 0 {
				return fmt.Errorf("proto: image phase2 item cannot request video frames")
			}
		case ScreenStageTwo:
			if item.FieldsMask != FieldPHashParts {
				return fmt.Errorf("proto: stage-two image phase2 item must request pHash fields")
			}
		case ScreenStageThree:
			if item.FieldsMask != FieldSobelHist {
				return fmt.Errorf("proto: stage-three image phase2 item must request Sobel fields")
			}
		default:
			return fmt.Errorf("proto: phase2 stage %d is invalid", stage)
		}
	case KindVideo:
		var expected uint32
		switch stage {
		case ScreenStageLegacy:
			expected = FieldVideo6F
		case ScreenStageTwo:
			expected = FieldVideo6FPHash
		case ScreenStageThree:
			expected = FieldVideo6FSobel
		default:
			return fmt.Errorf("proto: phase2 stage %d is invalid", stage)
		}
		if item.FieldsMask != expected {
			if stage == ScreenStageLegacy {
				return fmt.Errorf("proto: video phase2 item must request video frames")
			}
			return fmt.Errorf("proto: video phase2 item must request fields for stage %d", stage)
		}
		if item.DurationMS <= 0 {
			return fmt.Errorf("proto: video phase2 item duration_ms must be positive")
		}
	default:
		return fmt.Errorf("proto: phase2 item kind %d is invalid", item.Kind)
	}
	return nil
}

func isCanonicalSHA512(value string) bool {
	if len(value) != 128 {
		return false
	}
	for _, ch := range value {
		if !(ch >= '0' && ch <= '9') && !(ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

type Phase2Task struct {
	TaskID string       `msgpack:"task_id"`
	Stage  uint8        `msgpack:"stage,omitempty"`
	Items  []Phase2Item `msgpack:"items"`
}

func (task Phase2Task) Validate() error {
	switch task.Stage {
	case ScreenStageLegacy, ScreenStageTwo, ScreenStageThree:
	default:
		return fmt.Errorf("proto: phase2 stage %d is invalid", task.Stage)
	}
	for _, item := range task.Items {
		if err := item.validateForStage(task.Stage); err != nil {
			return err
		}
	}
	return nil
}

type DeleteTask struct {
	TaskID    string   `msgpack:"task_id"`
	Seq       uint32   `msgpack:"seq,omitempty"`
	LastSeq   uint32   `msgpack:"last_seq,omitempty"`
	Mode      string   `msgpack:"mode,omitempty"`
	Confirmed bool     `msgpack:"confirmed,omitempty"`
	Entries   []string `msgpack:"entries"`
}

type ConfigPush struct {
	KV map[string]string `msgpack:"kv"`
}

// StatsQuery and StatsReport were reserved by architecture-plan v1.2 for M6.
type StatsQuery struct {
	WindowSeconds int `msgpack:"window_seconds,omitempty"`
}

type Shutdown struct{}

type DiskStats struct {
	DiskNo       int64   `msgpack:"disk_no"`
	ReadBPS      float64 `msgpack:"read_bps"`
	BusyFraction float64 `msgpack:"busy_frac"`
	FilesDone    int64   `msgpack:"files_done,omitempty"`
	PendingBytes int64   `msgpack:"pending_bytes,omitempty"`
}

type StatsReport struct {
	Disks        []DiskStats `msgpack:"disks,omitempty"`
	CPU          float64     `msgpack:"cpu"`
	Workers      int         `msgpack:"workers"`
	WindowS      int         `msgpack:"window_s,omitempty"`
	RSSBytes     uint64      `msgpack:"rss_bytes,omitempty"`
	HeapBytes    uint64      `msgpack:"heap_bytes,omitempty"`
	Handles      uint64      `msgpack:"handles,omitempty"`
	PendingBytes int64       `msgpack:"pending_bytes,omitempty"`
	FilesDone    int64       `msgpack:"files_done,omitempty"`
	FilesFailed  int64       `msgpack:"files_failed,omitempty"`
	Crashes      int64       `msgpack:"crashes,omitempty"`
	ReadP95MS    float64     `msgpack:"read_p95_ms,omitempty"`
	DecodeP95MS  float64     `msgpack:"decode_p95_ms,omitempty"`
}

type TaskProgress struct {
	TaskID     string  `msgpack:"task_id"`
	Done       int64   `msgpack:"done"`
	Total      int64   `msgpack:"total"`
	TotalKnown bool    `msgpack:"total_known"`
	Failed     int64   `msgpack:"failed"`
	ElapsedMS  int64   `msgpack:"elapsed_ms"`
	Speed      float64 `msgpack:"speed"`
}

type FeatureItem struct {
	Path         string         `msgpack:"path"`
	SHA512       string         `msgpack:"sha512,omitempty"`
	Size         int64          `msgpack:"size"`
	MTime        int64          `msgpack:"mtime"`
	Status       string         `msgpack:"status"`
	Err          string         `msgpack:"err,omitempty"`
	FieldsDone   uint32         `msgpack:"fields_done,omitempty"`
	PDQ256       string         `msgpack:"pdq256,omitempty"`
	Quality      int32          `msgpack:"quality,omitempty"`
	Width        int32          `msgpack:"width,omitempty"`
	Height       int32          `msgpack:"height,omitempty"`
	DurationMS   *int64         `msgpack:"duration_ms,omitempty"`
	ThumbPath    string         `msgpack:"thumb_path,omitempty"`
	ThumbPDQ256  string         `msgpack:"thumb_pdq256,omitempty"`
	ThumbQuality *int32         `msgpack:"thumb_quality,omitempty"`
	FieldErrors  []FieldError   `msgpack:"field_errors,omitempty"`
	PHashParts   []byte         `msgpack:"phash_parts,omitempty"`
	SobelHist    []byte         `msgpack:"sobel_hist,omitempty"`
	Frames       []FrameFeature `msgpack:"frames,omitempty"`
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

type FieldError struct {
	Field uint32 `msgpack:"field"`
	Stage string `msgpack:"stage"`
	Msg   string `msgpack:"msg"`
}

type FeatureResult struct {
	TaskID string        `msgpack:"task_id"`
	Seq    uint64        `msgpack:"seq"`
	Items  []FeatureItem `msgpack:"items"`
}

type TaskStats struct {
	Total            int64   `msgpack:"total"`
	Done             int64   `msgpack:"done"`
	Skipped          int64   `msgpack:"skipped"`
	Failed           int64   `msgpack:"failed"`
	ScanErrors       int64   `msgpack:"scan_errors,omitempty"`
	ElapsedMS        int64   `msgpack:"elapsed_ms"`
	FilesDone        int64   `msgpack:"files_done,omitempty"`
	FilesFailed      int64   `msgpack:"files_failed,omitempty"`
	DecodeCalls      int64   `msgpack:"decode_calls,omitempty"`
	ReadAttempts     int64   `msgpack:"read_attempts,omitempty"`
	DecodeAttempts   int64   `msgpack:"decode_attempts,omitempty"`
	AvgReadMS        float64 `msgpack:"avg_read_ms,omitempty"`
	AvgDecodeMS      float64 `msgpack:"avg_decode_ms,omitempty"`
	ThumbGenerated   int64   `msgpack:"thumb_generated,omitempty"`
	ThumbCacheHits   int64   `msgpack:"thumb_cache_hits,omitempty"`
	SingleFlightHits int64   `msgpack:"singleflight_hits,omitempty"`
	Crashes          int64   `msgpack:"crashes,omitempty"`
}

type TaskDone struct {
	TaskID string          `msgpack:"task_id"`
	Stats  TaskStats       `msgpack:"stats"`
	Reason TaskDrainReason `msgpack:"reason,omitempty"`
}

func (done TaskDone) Validate() error {
	switch done.Reason {
	case "", TaskDrainPause, TaskDrainStop, TaskDrainDelete, TaskDrainProcessShutdown:
		return nil
	default:
		return fmt.Errorf("%s", InvalidTaskDrainReasonErrorCode)
	}
}

type Error struct {
	TaskID string `msgpack:"task_id,omitempty"`
	Path   string `msgpack:"path,omitempty"`
	Stage  string `msgpack:"stage"`
	Msg    string `msgpack:"msg"`
}

type CrashNotice struct {
	TaskID   string `msgpack:"task_id,omitempty"`
	PID      int    `msgpack:"pid"`
	Path     string `msgpack:"path"`
	ExitCode int    `msgpack:"exit_code"`
}

type DeleteResult struct {
	Path            string `msgpack:"path"`
	OK              bool   `msgpack:"ok"`
	ErrCode         string `msgpack:"err_code,omitempty"`
	Err             string `msgpack:"err,omitempty"`
	ReadonlyCleared bool   `msgpack:"readonly_cleared,omitempty"`
	RecycledTo      string `msgpack:"recycled_to,omitempty"`
	Uncertain       bool   `msgpack:"uncertain,omitempty"`
	StateSyncErr    string `msgpack:"state_sync_err,omitempty"`
}

type DeleteStats struct {
	Total     int `msgpack:"total"`
	OK        int `msgpack:"ok"`
	Failed    int `msgpack:"failed"`
	Uncertain int `msgpack:"uncertain,omitempty"`
}

type DeleteReport struct {
	TaskID  string         `msgpack:"task_id"`
	Seq     uint32         `msgpack:"seq,omitempty"`
	LastSeq uint32         `msgpack:"last_seq,omitempty"`
	Stats   DeleteStats    `msgpack:"stats"`
	Entries []DeleteResult `msgpack:"entries"`
}

// Decode unmarshals a message body into the concrete map-encoded message type.
func Decode(msgType uint8, body []byte) (any, error) {
	var value any
	switch msgType {
	case MsgPing:
		value = &Ping{}
	case MsgPong:
		value = &Pong{}
	case MsgHello:
		value = &Hello{}
	case MsgShutdown:
		value = &Shutdown{}
	case MsgClientAuth:
		value = &ClientAuth{}
	case MsgClientAuthResult:
		value = &ClientAuthResult{}
	case MsgScanTask:
		value = &ScanTask{}
	case MsgTaskAck:
		value = &TaskAck{}
	case MsgPhase2Task:
		value = &Phase2Task{}
	case MsgDeleteTask:
		value = &DeleteTask{}
	case MsgConfigPush:
		value = &ConfigPush{}
	case MsgStatsQuery:
		value = &StatsQuery{}
	case MsgFilesystemBrowse:
		value = &FilesystemBrowseRequest{}
	case MsgTaskProgress:
		value = &TaskProgress{}
	case MsgFeatureResult:
		value = &FeatureResult{}
	case MsgTaskDone:
		value = &TaskDone{}
	case MsgError:
		value = &Error{}
	case MsgCrashNotice:
		value = &CrashNotice{}
	case MsgDeleteReport:
		value = &DeleteReport{}
	case MsgStatsReport:
		value = &StatsReport{}
	case MsgFilesystemBrowseResult:
		value = &FilesystemBrowseResponse{}
	case MsgLocalRequest:
		value = &LocalRequest{}
	case MsgLocalResponse:
		value = &LocalResponse{}
	case MsgLocalEvent:
		value = &LocalEvent{}
	default:
		return nil, fmt.Errorf("proto: unknown message type %d", msgType)
	}
	if err := msgpack.Unmarshal(body, value); err != nil {
		return nil, fmt.Errorf("proto: decode type=%d: %w", msgType, err)
	}
	return value, nil
}
