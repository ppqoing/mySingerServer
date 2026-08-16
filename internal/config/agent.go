package config

import (
	"bytes"
	"dedup/internal/diskio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type AgentConfig struct {
	MachineID     string         `json:"-"`
	ListenAddr    string         `json:"listen_addr"`
	DataDir       string         `json:"data_dir"`
	PGDSN         string         `json:"pg_dsn"`
	UseEverything bool           `json:"use_everything"`
	Scan          ScanConfig     `json:"scan"`
	Sync          SyncConfig     `json:"sync"`
	Proto         ProtoConfig    `json:"proto"`
	Worker        WorkerConfig   `json:"worker"`
	Pipeline      PipelineConfig `json:"pipeline"`
	Thumb         ThumbConfig    `json:"thumb"`
	IPC           IPCConfig      `json:"ipc"`
	Delete        DeleteConfig   `json:"delete"`
	Tuning        TuningConfig   `json:"tuning"`
	IO            IOConfig       `json:"io"`
}

// UnmarshalJSON accepts the obsolete top-level machine_id field so existing
// installations can load once, but deliberately discards it. All other
// unknown fields remain errors.
func (c *AgentConfig) UnmarshalJSON(data []byte) error {
	type plainAgentConfig AgentConfig
	wire := struct {
		*plainAgentConfig
		LegacyMachineID json.RawMessage `json:"machine_id"`
	}{
		plainAgentConfig: (*plainAgentConfig)(c),
	}
	c.MachineID = ""
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return err
	}
	c.MachineID = ""
	return nil
}

type ScanConfig struct {
	HDDReadBlockMB     int      `json:"hdd_read_block_mb"`
	HDDStreams         int      `json:"hdd_streams_per_disk"`
	SSDStreams         int      `json:"ssd_streams_per_disk"`
	ImageMemResidentMB int      `json:"image_mem_resident_mb"`
	ImageTimeoutS      int      `json:"image_timeout_s"`
	VideoTimeoutS      int      `json:"video_timeout_s"`
	ImageExts          []string `json:"image_exts"`
	VideoExts          []string `json:"video_exts"`
}

type SyncConfig struct {
	IntervalS   int `json:"interval_s"`
	TriggerRows int `json:"trigger_rows"`
	UpsertBatch int `json:"upsert_batch"`
}

type ProtoConfig struct {
	HeartbeatS int `json:"heartbeat_s"`
}

type WorkerConfig struct {
	Count          int    `json:"count"`
	ExePath        string `json:"exe_path"`
	ImageTimeoutS  int    `json:"image_timeout_s"`
	VideoTimeoutS  int    `json:"video_timeout_s"`
	ImageMemoryMB  int    `json:"image_memory_mb"`
	RespawnDelayMS int    `json:"respawn_delay_ms"`
	CrashInjection bool   `json:"crash_injection"`
}

type PipelineConfig struct {
	ReadChunkKB int `json:"read_chunk_kb"`
}

type ThumbConfig struct {
	CacheDir       string `json:"cache_dir"`
	TileMaxSide    int    `json:"tile_max_side"`
	ProbeTimeoutS  int    `json:"probe_timeout_s"`
	NativeTimeoutS int    `json:"native_timeout_s"`
	FrameTimeoutS  int    `json:"frame_timeout_s"`
}

type IPCConfig struct {
	MaxFrameMB int `json:"max_frame_mb"`
}

type DeleteConfig struct {
	PipeName           string `json:"pipe_name"`
	MaxEntriesPerFrame int    `json:"max_entries_per_frame"`
	DialTimeoutMS      int    `json:"dial_timeout_ms"`
	HelloTimeoutS      int    `json:"hello_timeout_s"`
	ReportTimeoutS     int    `json:"report_timeout_s"`
}

type TuningConfig struct {
	StatsEnabled   bool   `json:"stats_enabled"`
	StatsIntervalS int    `json:"stats_interval_s"`
	StatsHistoryS  int    `json:"stats_history_s"`
	PendingBytesMB int    `json:"pending_bytes_mb"`
	StatsLogMB     int    `json:"stats_log_mb"`
	PprofAddr      string `json:"pprof_addr"`
}

type IOConfig struct {
	LeaseMB            int     `json:"lease_mb"`
	MinLeaseMB         int     `json:"min_lease_mb"`
	MaxLeaseMB         int     `json:"max_lease_mb"`
	HDDInitial         int     `json:"hdd_initial"`
	SSDInitial         int     `json:"ssd_initial"`
	MaxPerDisk         int     `json:"max_per_disk"`
	HDDRandomMax       int     `json:"hdd_random_max"`
	WindowMS           int     `json:"window_ms"`
	IncreaseThreshold  float64 `json:"increase_threshold"`
	DecreaseThreshold  float64 `json:"decrease_threshold"`
	MaxQueuedPerWorker int     `json:"max_queued_per_worker"`
}

func (c IOConfig) Policy(workerCount int) (diskio.PolicyConfig, error) {
	if workerCount < 1 ||
		c.LeaseMB < 1 || c.LeaseMB > 16 ||
		c.MinLeaseMB < 1 || c.MinLeaseMB > c.LeaseMB ||
		c.MaxLeaseMB < c.LeaseMB || c.MaxLeaseMB > 16 ||
		c.MaxPerDisk < 1 || c.MaxPerDisk > 24 ||
		c.HDDInitial < 1 || c.HDDInitial > c.MaxPerDisk ||
		c.SSDInitial < 1 || c.SSDInitial > c.MaxPerDisk ||
		c.HDDRandomMax < 1 || c.HDDRandomMax > c.HDDInitial ||
		c.WindowMS < 1 || c.WindowMS > 60_000 ||
		c.MaxQueuedPerWorker < 1 || c.MaxQueuedPerWorker > 1024 ||
		math.IsNaN(c.IncreaseThreshold) || math.IsInf(c.IncreaseThreshold, 0) ||
		math.IsNaN(c.DecreaseThreshold) || math.IsInf(c.DecreaseThreshold, 0) ||
		c.DecreaseThreshold < 0 || c.IncreaseThreshold > 1 ||
		c.DecreaseThreshold >= c.IncreaseThreshold {
		return diskio.PolicyConfig{}, fmt.Errorf("config: IO policy value out of bounds")
	}
	const mebibyte = int64(1024 * 1024)
	return diskio.PolicyConfig{
		LeaseBytes:         int64(c.LeaseMB) * mebibyte,
		MinLeaseBytes:      int64(c.MinLeaseMB) * mebibyte,
		MaxLeaseBytes:      int64(c.MaxLeaseMB) * mebibyte,
		HDDInitial:         c.HDDInitial,
		SSDInitial:         c.SSDInitial,
		MaxPerDisk:         c.MaxPerDisk,
		HDDRandomMax:       c.HDDRandomMax,
		Window:             time.Duration(c.WindowMS) * time.Millisecond,
		IncreaseThreshold:  c.IncreaseThreshold,
		DecreaseThreshold:  c.DecreaseThreshold,
		MaxQueuedPerWorker: c.MaxQueuedPerWorker,
	}, nil
}

func (c *AgentConfig) SyncInterval() time.Duration {
	return time.Duration(c.Sync.IntervalS) * time.Second
}

func (c *AgentConfig) StatsInterval() time.Duration {
	return time.Duration(c.Tuning.StatsIntervalS) * time.Second
}

func DefaultAgent() *AgentConfig {
	return &AgentConfig{
		ListenAddr:    "0.0.0.0:9101",
		DataDir:       "./data",
		UseEverything: true,
		Scan: ScanConfig{
			HDDReadBlockMB:     4,
			HDDStreams:         2,
			SSDStreams:         6,
			ImageMemResidentMB: 256,
			ImageTimeoutS:      30,
			VideoTimeoutS:      120,
		},
		Sync: SyncConfig{
			IntervalS:   300,
			TriggerRows: 50000,
			UpsertBatch: 5000,
		},
		Proto: ProtoConfig{HeartbeatS: 15},
		Worker: WorkerConfig{
			ImageTimeoutS: 30, VideoTimeoutS: 120, ImageMemoryMB: 256,
			RespawnDelayMS: 500,
		},
		Pipeline: PipelineConfig{ReadChunkKB: 4096},
		Thumb: ThumbConfig{
			TileMaxSide: 256, ProbeTimeoutS: 15, NativeTimeoutS: 60, FrameTimeoutS: 20,
		},
		IPC: IPCConfig{MaxFrameMB: 16},
		Delete: DeleteConfig{
			PipeName:           `\\.\pipe\dedup-delete`,
			MaxEntriesPerFrame: 2000,
			DialTimeoutMS:      500,
			HelloTimeoutS:      5,
			ReportTimeoutS:     600,
		},
		Tuning: TuningConfig{
			StatsEnabled:   true,
			StatsIntervalS: 1,
			StatsHistoryS:  300,
			PendingBytesMB: 1024,
			StatsLogMB:     32,
		},
		IO: IOConfig{
			LeaseMB:            4,
			MinLeaseMB:         1,
			MaxLeaseMB:         16,
			HDDInitial:         2,
			SSDInitial:         4,
			MaxPerDisk:         24,
			HDDRandomMax:       1,
			WindowMS:           1000,
			IncreaseThreshold:  0.80,
			DecreaseThreshold:  0.60,
			MaxQueuedPerWorker: 4,
		},
	}
}

func LoadAgent(path string) (*AgentConfig, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("config: executable: %w", err)
	}
	return loadAgent(path, executable, runtime.NumCPU())
}

func loadAgent(path, executable string, cpuCount int) (*AgentConfig, error) {
	cfg := DefaultAgent()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if key := forbiddenAgentSetting(raw); key != "" {
		return nil, fmt.Errorf("config: forbidden Agent setting %q", key)
	}
	if deleteRaw, ok := raw["delete"]; ok {
		var deleteSettings map[string]json.RawMessage
		if err := json.Unmarshal(deleteRaw, &deleteSettings); err == nil {
			if key := forbiddenAgentSetting(deleteSettings); key != "" {
				return nil, fmt.Errorf("config: forbidden Agent delete setting %q", key)
			}
		}
	}
	if thumbRaw, ok := raw["thumb"]; ok {
		var thumbSettings map[string]json.RawMessage
		if err := json.Unmarshal(thumbRaw, &thumbSettings); err == nil {
			if key := forbiddenThumbSetting(thumbSettings); key != "" {
				return nil, fmt.Errorf("config: obsolete Agent thumb setting %q", key)
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return ValidateAgent(cfg, executable, cpuCount)
}

// ValidateAgent returns a normalized copy without mutating cfg.
func ValidateAgent(cfg *AgentConfig, executable string, cpuCount int) (*AgentConfig, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config: Agent configuration is required")
	}
	validated := *cfg
	validated.Scan.ImageExts = append([]string(nil), cfg.Scan.ImageExts...)
	validated.Scan.VideoExts = append([]string(nil), cfg.Scan.VideoExts...)
	cfg = &validated

	if cfg.ListenAddr == "" || cfg.DataDir == "" {
		return nil, fmt.Errorf("config: listen_addr and data_dir required")
	}
	if cpuCount < 1 {
		return nil, fmt.Errorf("config: CPU count must be positive")
	}
	if cfg.Worker.Count == 0 {
		cfg.Worker.Count = cpuCount
	}
	exeDir := filepath.Dir(executable)
	if cfg.Worker.ExePath == "" {
		cfg.Worker.ExePath = filepath.Join(exeDir, "worker.exe")
	}
	thumbCache := cfg.Thumb.CacheDir
	if thumbCache == "" {
		thumbCache = filepath.Join(cfg.DataDir, "thumbcache")
	} else if !filepath.IsAbs(thumbCache) {
		thumbCache = filepath.Join(cfg.DataDir, thumbCache)
	}
	thumbCache, err := filepath.Abs(thumbCache)
	if err != nil {
		return nil, fmt.Errorf("config: resolve thumb cache_dir: %w", err)
	}
	cfg.Thumb.CacheDir = filepath.Clean(thumbCache)
	if cfg.Scan.HDDStreams < 1 || cfg.Scan.SSDStreams < 1 ||
		cfg.Sync.IntervalS < 1 || cfg.Sync.TriggerRows < 1 ||
		cfg.Sync.UpsertBatch < 1 || cfg.Proto.HeartbeatS < 1 {
		return nil, fmt.Errorf("config: stream, sync, and heartbeat values must be positive")
	}
	if cfg.Worker.Count < 1 || cfg.Worker.Count > 1024 ||
		cfg.Worker.ImageTimeoutS < 1 || cfg.Worker.ImageTimeoutS > 3600 ||
		cfg.Worker.VideoTimeoutS < 1 || cfg.Worker.VideoTimeoutS > 3600 ||
		cfg.Worker.ImageMemoryMB < 1 || cfg.Worker.ImageMemoryMB > 256 ||
		cfg.Worker.RespawnDelayMS < 1 || cfg.Worker.RespawnDelayMS > 60_000 ||
		cfg.Pipeline.ReadChunkKB < 1 || cfg.Pipeline.ReadChunkKB > 16_384 ||
		cfg.Thumb.TileMaxSide < 1 || cfg.Thumb.TileMaxSide > 8192 ||
		cfg.Thumb.ProbeTimeoutS < 1 || cfg.Thumb.ProbeTimeoutS > 3600 ||
		cfg.Thumb.NativeTimeoutS < 1 || cfg.Thumb.NativeTimeoutS > 3600 ||
		cfg.Thumb.FrameTimeoutS < 1 || cfg.Thumb.FrameTimeoutS > 3600 ||
		cfg.IPC.MaxFrameMB < 1 || cfg.IPC.MaxFrameMB > 16 {
		return nil, fmt.Errorf("config: worker, pipeline, thumb, or IPC value out of bounds")
	}
	if !validDeletePipeName(cfg.Delete.PipeName) ||
		cfg.Delete.MaxEntriesPerFrame < 1 || cfg.Delete.MaxEntriesPerFrame > 2000 ||
		cfg.Delete.DialTimeoutMS < 1 || cfg.Delete.DialTimeoutMS > 30_000 ||
		cfg.Delete.HelloTimeoutS < 1 || cfg.Delete.HelloTimeoutS > 60 ||
		cfg.Delete.ReportTimeoutS < 1 || cfg.Delete.ReportTimeoutS > 3600 {
		return nil, fmt.Errorf("config: delete value out of bounds")
	}
	if cfg.Tuning.StatsIntervalS < 1 || cfg.Tuning.StatsIntervalS > 60 ||
		cfg.Tuning.StatsHistoryS < 1 || cfg.Tuning.StatsHistoryS > 300 ||
		cfg.Tuning.PendingBytesMB < 1 || cfg.Tuning.PendingBytesMB > 16_384 ||
		cfg.Tuning.StatsLogMB < 1 || cfg.Tuning.StatsLogMB > 1024 {
		return nil, fmt.Errorf("config: tuning value out of bounds")
	}
	if cfg.Tuning.PprofAddr != "" && !loopbackAddress(cfg.Tuning.PprofAddr) {
		return nil, fmt.Errorf("config: pprof_addr must be a loopback host:port")
	}
	if _, err := cfg.IO.Policy(cfg.Worker.Count); err != nil {
		return nil, err
	}
	return cfg, nil
}

func loopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func forbiddenAgentSetting(settings map[string]json.RawMessage) string {
	for _, key := range []string{
		"helper_exe",
		"helper_exe_path",
		"helper_path",
		"elevation",
		"elevate",
		"auto_launch",
		"auto_restart",
	} {
		if _, exists := settings[key]; exists {
			return key
		}
	}
	return ""
}

func forbiddenThumbSetting(settings map[string]json.RawMessage) string {
	for _, key := range []string{
		"max_side",
		"ffmpeg_path",
		"ffprobe_path",
		"ffprobe_timeout_s",
		"ffmpeg_timeout_s",
	} {
		if _, exists := settings[key]; exists {
			return key
		}
	}
	return ""
}

func ValidateThumbCacheRoots(cacheRoot string, scanRoots []string) error {
	cache, err := filepath.Abs(cacheRoot)
	if err != nil {
		return fmt.Errorf("config: resolve thumb cache root: %w", err)
	}
	cache = filepath.Clean(cache)
	for _, root := range scanRoots {
		scan, err := filepath.Abs(root)
		if err != nil {
			return fmt.Errorf("config: resolve scan root %q: %w", root, err)
		}
		scan = filepath.Clean(scan)
		if pathContainsWindows(cache, scan) || pathContainsWindows(scan, cache) {
			return fmt.Errorf("config: thumb cache root %q overlaps scan root %q", cache, scan)
		}
	}
	return nil
}

func pathContainsWindows(parent, child string) bool {
	relative, err := filepath.Rel(strings.ToLower(parent), strings.ToLower(child))
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func validDeletePipeName(name string) bool {
	const prefix = `\\.\pipe\`
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	suffix := name[len(prefix):]
	if len(suffix) < 1 || len(suffix) > 128 {
		return false
	}
	for i := 0; i < len(suffix); i++ {
		ch := suffix[i]
		if (ch >= 'A' && ch <= 'Z') ||
			(ch >= 'a' && ch <= 'z') ||
			(ch >= '0' && ch <= '9') ||
			ch == '.' || ch == '_' || ch == '-' {
			continue
		}
		return false
	}
	return true
}

func (c *AgentConfig) WorkerEnv() []string {
	return []string{
		"WPROC_THUMB_CACHE=" + c.Thumb.CacheDir,
		"WPROC_TILE_MAX_SIDE=" + strconv.Itoa(c.Thumb.TileMaxSide),
		"WPROC_PROBE_TIMEOUT_S=" + strconv.Itoa(c.Thumb.ProbeTimeoutS),
		"WPROC_NATIVE_TIMEOUT_S=" + strconv.Itoa(c.Thumb.NativeTimeoutS),
		"WPROC_FRAME_TIMEOUT_S=" + strconv.Itoa(c.Thumb.FrameTimeoutS),
		"WPROC_IMAGE_MEM_MB=" + strconv.Itoa(c.Worker.ImageMemoryMB),
		"WPROC_IPC_MAX_MB=" + strconv.Itoa(c.IPC.MaxFrameMB),
	}
}
