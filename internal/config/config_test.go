package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func validGUIConfigForValidation() *GUIConfig {
	cfg := DefaultGUI()
	cfg.PGDSN = "postgres://user:pass@127.0.0.1:5432/dedup"
	cfg.Agents = []AgentEndpoint{{Addr: "192.168.1.10:9101"}}
	return cfg
}

func requireGUIFieldError(t *testing.T, err error, want FieldError) {
	t.Helper()
	var validationErr *GUIValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error type = %T, want *GUIValidationError: %v", err, err)
	}
	for _, field := range validationErr.Fields {
		if field.Field == want.Field && field.Code == want.Code {
			return
		}
	}
	t.Fatalf("field errors = %#v, want field=%q code=%q", validationErr.Fields, want.Field, want.Code)
}

func writeAgentWithDelete(t *testing.T, deleteValues map[string]any) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"machine_id": "machine-a",
		"pg_dsn":     "postgres://localhost/dedup",
		"delete":     deleteValues,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAgentAppliesDocumentedDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte(`{
		"machine_id":"machine-a",
		"pg_dsn":"postgres://user:pass@localhost/dedup"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadAgent(path)
	if err != nil {
		t.Fatalf("LoadAgent: %v", err)
	}
	if cfg.ListenAddr != "0.0.0.0:9101" || cfg.DataDir != "./data" {
		t.Fatalf("unexpected base defaults: %#v", cfg)
	}
	if cfg.MachineID != "" {
		t.Fatalf("legacy machine_id survived as runtime identity: %q", cfg.MachineID)
	}
	if !cfg.UseEverything {
		t.Fatal("UseEverything = false, want true")
	}
	if cfg.Scan.HDDReadBlockMB != 4 || cfg.Scan.HDDStreams != 2 || cfg.Scan.SSDStreams != 6 {
		t.Fatalf("unexpected scan defaults: %#v", cfg.Scan)
	}
	if cfg.SyncInterval() != 5*time.Minute ||
		cfg.Sync.TriggerRows != 50000 || cfg.Sync.UpsertBatch != 5000 {
		t.Fatalf("unexpected sync defaults: %#v", cfg.Sync)
	}
	if cfg.Proto.HeartbeatS != 15 {
		t.Fatalf("heartbeat = %d, want 15", cfg.Proto.HeartbeatS)
	}
	if cfg.Tuning != (TuningConfig{
		StatsEnabled:   true,
		StatsIntervalS: 1,
		StatsHistoryS:  300,
		PendingBytesMB: 1024,
		StatsLogMB:     32,
	}) {
		t.Fatalf("unexpected tuning defaults: %#v", cfg.Tuning)
	}
	wantDelete := DeleteConfig{
		PipeName:           `\\.\pipe\dedup-delete`,
		MaxEntriesPerFrame: 2000,
		DialTimeoutMS:      500,
		HelloTimeoutS:      5,
		ReportTimeoutS:     600,
	}
	if cfg.Delete != wantDelete {
		t.Fatalf("delete defaults = %#v, want %#v", cfg.Delete, wantDelete)
	}
}

func TestLoadAgentValidatesTuningBoundariesAndLoopbackPprof(t *testing.T) {
	valid := []string{
		`"stats_interval_s":1`,
		`"stats_interval_s":60`,
		`"stats_history_s":1`,
		`"stats_history_s":300`,
		`"pending_bytes_mb":1`,
		`"pending_bytes_mb":16384`,
		`"stats_log_mb":1`,
		`"stats_log_mb":1024`,
		`"pprof_addr":"127.0.0.1:6060"`,
		`"pprof_addr":"[::1]:6060"`,
	}
	for _, fragment := range valid {
		t.Run("valid/"+fragment, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.json")
			body := `{"machine_id":"machine-a","pg_dsn":"postgres://localhost/dedup","tuning":{` + fragment + `}}`
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadAgent(path, `C:\agent.exe`, 4); err != nil {
				t.Fatalf("loadAgent rejected %s: %v", fragment, err)
			}
		})
	}

	invalid := []string{
		`"stats_interval_s":0`,
		`"stats_interval_s":61`,
		`"stats_history_s":0`,
		`"stats_history_s":301`,
		`"pending_bytes_mb":0`,
		`"pending_bytes_mb":16385`,
		`"stats_log_mb":0`,
		`"stats_log_mb":1025`,
		`"pprof_addr":"0.0.0.0:6060"`,
		`"pprof_addr":"192.168.1.5:6060"`,
		`"pprof_addr":"localhost"`,
	}
	for _, fragment := range invalid {
		t.Run("invalid/"+fragment, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.json")
			body := `{"machine_id":"machine-a","pg_dsn":"postgres://localhost/dedup","tuning":{` + fragment + `}}`
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadAgent(path, `C:\agent.exe`, 4); err == nil {
				t.Fatalf("loadAgent accepted %s", fragment)
			}
		})
	}
}

func TestLoadAgentRejectsMissingDSN(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte(`{"machine_id":"machine-a"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAgent(path); err == nil {
		t.Fatal("LoadAgent accepted missing pg_dsn")
	}
}

func TestLoadAgentAcceptsMissingAndIgnoresLegacyMachineID(t *testing.T) {
	load := func(t *testing.T, body string) *AgentConfig {
		t.Helper()
		path := filepath.Join(t.TempDir(), "agent.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadAgent(path, `C:\portable\agent.exe`, 4)
		if err != nil {
			t.Fatalf("loadAgent: %v", err)
		}
		return cfg
	}

	without := load(t, `{"pg_dsn":"postgres://localhost/dedup"}`)
	if without.MachineID != "" {
		t.Fatalf("runtime MachineID before injection = %q", without.MachineID)
	}
	legacy := load(t, `{"machine_id":"legacy-manual-id","pg_dsn":"postgres://localhost/dedup"}`)
	encoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.MachineID != "" || bytes.Contains(encoded, []byte(`"machine_id"`)) {
		t.Fatalf("legacy ID survived: cfg=%#v json=%s", legacy, encoded)
	}
}

func TestLoadAgentResolvesWorkerPipelineThumbAndIPCDefaults(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "portable", "agent.exe")
	path := filepath.Join(root, "agent.json")
	if err := os.WriteFile(path, []byte(`{
		"machine_id":"machine-a",
		"pg_dsn":"postgres://user:pass@localhost/dedup",
		"data_dir":"D:\\dedup-data"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadAgent(path, executable, 12)
	if err != nil {
		t.Fatalf("loadAgent: %v", err)
	}
	exeDir := filepath.Dir(executable)
	if cfg.Worker.Count != 12 ||
		cfg.Worker.ExePath != filepath.Join(exeDir, "worker.exe") ||
		cfg.Worker.ImageTimeoutS != 30 ||
		cfg.Worker.VideoTimeoutS != 120 ||
		cfg.Worker.ImageMemoryMB != 256 ||
		cfg.Worker.RespawnDelayMS != 500 {
		t.Fatalf("worker defaults = %#v", cfg.Worker)
	}
	if cfg.Pipeline.ReadChunkKB != 4096 {
		t.Fatalf("pipeline defaults = %#v", cfg.Pipeline)
	}
	if cfg.Thumb.CacheDir != filepath.Join(`D:\dedup-data`, "thumbcache") ||
		cfg.Thumb.TileMaxSide != 256 ||
		cfg.Thumb.ProbeTimeoutS != 15 ||
		cfg.Thumb.NativeTimeoutS != 60 ||
		cfg.Thumb.FrameTimeoutS != 20 {
		t.Fatalf("thumb defaults = %#v", cfg.Thumb)
	}
	if cfg.IPC.MaxFrameMB != 16 || cfg.Worker.CrashInjection {
		t.Fatalf("IPC/crash defaults = %#v / %#v", cfg.IPC, cfg.Worker)
	}
}

func TestLoadAgentPreservesExplicitWorkerPathsAndBuildsExactWorkerEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte(`{
		"machine_id":"machine-a",
		"pg_dsn":"postgres://localhost/dedup",
		"worker":{
			"count":3,
			"exe_path":"D:\\runtime\\worker-custom.exe",
			"image_timeout_s":31,
			"video_timeout_s":121,
			"image_memory_mb":128,
			"respawn_delay_ms":750,
			"crash_injection":true
		},
		"pipeline":{"read_chunk_kb":2048},
		"thumb":{
			"cache_dir":"D:\\cache",
			"tile_max_side":512,
			"probe_timeout_s":16,
			"native_timeout_s":61,
			"frame_timeout_s":21
		},
		"ipc":{"max_frame_mb":8}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadAgent(path, `C:\portable\agent.exe`, runtime.NumCPU())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`WPROC_THUMB_CACHE=D:\cache`,
		"WPROC_TILE_MAX_SIDE=512",
		"WPROC_PROBE_TIMEOUT_S=16",
		"WPROC_NATIVE_TIMEOUT_S=61",
		"WPROC_FRAME_TIMEOUT_S=21",
		"WPROC_IMAGE_MEM_MB=128",
		"WPROC_IPC_MAX_MB=8",
	}
	if got := cfg.WorkerEnv(); !reflect.DeepEqual(got, want) {
		t.Fatalf("WorkerEnv:\nwant %#v\n got %#v", want, got)
	}
}

func TestLoadAgentRejectsOutOfBoundsWorkerPipelineThumbAndIPCValues(t *testing.T) {
	tests := []string{
		`"worker":{"count":-1}`,
		`"worker":{"count":1025}`,
		`"worker":{"image_timeout_s":0}`,
		`"worker":{"video_timeout_s":3601}`,
		`"worker":{"image_memory_mb":257}`,
		`"worker":{"respawn_delay_ms":0}`,
		`"pipeline":{"read_chunk_kb":16385}`,
		`"thumb":{"tile_max_side":0}`,
		`"thumb":{"probe_timeout_s":3601}`,
		`"thumb":{"native_timeout_s":0}`,
		`"thumb":{"frame_timeout_s":3601}`,
		`"ipc":{"max_frame_mb":0}`,
		`"ipc":{"max_frame_mb":17}`,
	}
	for _, fragment := range tests {
		t.Run(fragment, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.json")
			body := `{"machine_id":"machine-a","pg_dsn":"postgres://localhost/dedup",` + fragment + `}`
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadAgent(path, `C:\portable\agent.exe`, 8); err == nil {
				t.Fatalf("loadAgent accepted %s", fragment)
			}
		})
	}
}

func TestVideoCoreConfigDefaultsAndResolvesAbsoluteCache(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	path := filepath.Join(root, "agent.json")
	body := `{"machine_id":"machine-a","pg_dsn":"postgres://localhost/dedup","data_dir":` +
		strconv.Quote(dataDir) + `}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadAgent(path, filepath.Join(root, "agent.exe"), 4)
	if err != nil {
		t.Fatal(err)
	}
	wantCache := filepath.Join(dataDir, "thumbcache")
	if cfg.Thumb.CacheDir != wantCache || !filepath.IsAbs(cfg.Thumb.CacheDir) {
		t.Fatalf("cache_dir = %q, want absolute %q", cfg.Thumb.CacheDir, wantCache)
	}
	if cfg.Thumb.TileMaxSide != 256 || cfg.Thumb.ProbeTimeoutS != 15 ||
		cfg.Thumb.NativeTimeoutS != 60 || cfg.Thumb.FrameTimeoutS != 20 {
		t.Fatalf("VideoCore thumb defaults = %#v", cfg.Thumb)
	}

	relativePath := filepath.Join(root, "relative.json")
	relativeBody := `{"machine_id":"machine-a","pg_dsn":"postgres://localhost/dedup","data_dir":` +
		strconv.Quote(dataDir) + `,"thumb":{"cache_dir":"cache"}}`
	if err := os.WriteFile(relativePath, []byte(relativeBody), 0o600); err != nil {
		t.Fatal(err)
	}
	relativeCfg, err := loadAgent(relativePath, filepath.Join(root, "agent.exe"), 4)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dataDir, "cache"); relativeCfg.Thumb.CacheDir != want {
		t.Fatalf("relative cache_dir = %q, want %q", relativeCfg.Thumb.CacheDir, want)
	}
}

func TestVideoCoreConfigRejectsLegacyThumbFields(t *testing.T) {
	for _, key := range []string{"max_side", "ffmpeg_path", "ffprobe_path", "ffprobe_timeout_s", "ffmpeg_timeout_s"} {
		t.Run(key, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.json")
			body := `{"machine_id":"machine-a","pg_dsn":"postgres://localhost/dedup","thumb":{"` + key + `":1}}`
			if strings.HasSuffix(key, "_path") {
				body = `{"machine_id":"machine-a","pg_dsn":"postgres://localhost/dedup","thumb":{"` + key + `":"legacy.exe"}}`
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadAgent(path, filepath.Join(t.TempDir(), "agent.exe"), 4); err == nil ||
				!strings.Contains(err.Error(), key) {
				t.Fatalf("legacy thumb key %q error = %v, want explicit rejection", key, err)
			}
		})
	}
}

func TestThumbCacheRootOverlap(t *testing.T) {
	tests := []struct {
		name  string
		cache string
		roots []string
		ok    bool
	}{
		{name: "equal case insensitive", cache: `C:\Media`, roots: []string{`c:\media`}},
		{name: "cache below scan", cache: `C:\media\thumbcache`, roots: []string{`C:\media`}},
		{name: "scan below cache", cache: `C:\cache`, roots: []string{`C:\cache\media`}},
		{name: "sibling accepted", cache: `C:\cache`, roots: []string{`C:\media`}, ok: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateThumbCacheRoots(test.cache, test.roots)
			if test.ok && err != nil {
				t.Fatalf("ValidateThumbCacheRoots rejected siblings: %v", err)
			}
			if !test.ok && err == nil {
				t.Fatalf("ValidateThumbCacheRoots(%q, %#v) succeeded", test.cache, test.roots)
			}
		})
	}
}

func TestWorkerEnvHasNoFFmpegExecutable(t *testing.T) {
	cfg := DefaultAgent()
	cfg.Thumb.CacheDir = `D:\cache`
	cfg.Thumb.TileMaxSide = 384
	cfg.Thumb.ProbeTimeoutS = 16
	cfg.Thumb.NativeTimeoutS = 61
	cfg.Thumb.FrameTimeoutS = 21
	cfg.Worker.ImageMemoryMB = 128
	cfg.IPC.MaxFrameMB = 8
	want := []string{
		`WPROC_THUMB_CACHE=D:\cache`,
		"WPROC_TILE_MAX_SIDE=384",
		"WPROC_PROBE_TIMEOUT_S=16",
		"WPROC_NATIVE_TIMEOUT_S=61",
		"WPROC_FRAME_TIMEOUT_S=21",
		"WPROC_IMAGE_MEM_MB=128",
		"WPROC_IPC_MAX_MB=8",
	}
	if got := cfg.WorkerEnv(); !reflect.DeepEqual(got, want) {
		t.Fatalf("WorkerEnv:\nwant %#v\n got %#v", want, got)
	}
}

func TestValidateGUIAcceptsDefaultedLoadableConfig(t *testing.T) {
	if err := ValidateGUI(validGUIConfigForValidation()); err != nil {
		t.Fatalf("ValidateGUI: %v", err)
	}
}

func TestDefaultGUIIsACompletePortableFirstRunConfiguration(t *testing.T) {
	cfg := DefaultGUI()
	if err := ValidateGUI(cfg); err != nil {
		t.Fatalf("DefaultGUI: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:18081" ||
		cfg.PGDSN != "postgres://dedup@127.0.0.1:5432/dedup" ||
		len(cfg.Agents) != 1 || cfg.Agents[0].Addr != "127.0.0.1:9101" {
		t.Fatalf("incomplete portable defaults: %#v", cfg)
	}
}

func TestValidateGUIRejectsNetworkAndDSNFieldsWithStablePaths(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*GUIConfig)
		want   FieldError
	}{
		{
			name: "listen address without port",
			mutate: func(cfg *GUIConfig) {
				cfg.ListenAddr = "127.0.0.1"
			},
			want: FieldError{Field: "listen_addr", Code: "invalid_address"},
		},
		{
			name: "listen address port out of range",
			mutate: func(cfg *GUIConfig) {
				cfg.ListenAddr = "127.0.0.1:70000"
			},
			want: FieldError{Field: "listen_addr", Code: "invalid_address"},
		},
		{
			name: "invalid postgres DSN",
			mutate: func(cfg *GUIConfig) {
				cfg.PGDSN = "not a postgres dsn"
			},
			want: FieldError{Field: "pg_dsn", Code: "invalid_dsn"},
		},
		{
			name: "agent address without port",
			mutate: func(cfg *GUIConfig) {
				cfg.Agents[0].Addr = "192.168.1.10"
			},
			want: FieldError{Field: "agents[0].addr", Code: "invalid_address"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validGUIConfigForValidation()
			test.mutate(cfg)
			requireGUIFieldError(t, ValidateGUI(cfg), test.want)
		})
	}
}

func TestValidateGUIRejectsDuplicateAgentsWithIndexedPath(t *testing.T) {
	cfg := validGUIConfigForValidation()
	cfg.Agents = append(cfg.Agents, AgentEndpoint{
		Addr: "192.168.1.10:9101",
	})

	requireGUIFieldError(t, ValidateGUI(cfg), FieldError{
		Field: "agents[1].addr",
		Code:  "duplicate",
	})
}

func TestLoadGUIIgnoresLegacyMachineIDAndNewEncodingRemovesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gui.json")
	body := []byte(`{
		"listen_addr":"127.0.0.1:18080",
		"pg_dsn":"postgres://fixture.invalid/dedup",
		"heartbeat_s":15,
		"agents":[{"machine_id":"machine-a","addr":"127.0.0.1:9101"}]
	}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGUI(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Agents) != 1 || cfg.Agents[0].Addr != "127.0.0.1:9101" {
		t.Fatalf("Agents = %#v", cfg.Agents)
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(`"machine_id"`)) {
		t.Fatalf("new GUI encoding retained legacy ID: %s", encoded)
	}
}

func TestLoadGUIAndValidateGUIShareAnalysisBoundaries(t *testing.T) {
	cfg := validGUIConfigForValidation()
	cfg.Phase2.VideoFrames = 5
	requireGUIFieldError(t, ValidateGUI(cfg), FieldError{
		Field: "phase2.video_frames",
		Code:  "fixed_value",
	})

	path := filepath.Join(t.TempDir(), "gui.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadGUI(path)
	requireGUIFieldError(t, err, FieldError{
		Field: "phase2.video_frames",
		Code:  "fixed_value",
	})
}

func TestLoadGUIAppliesDefaultsAndValidatesEndpoints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gui.json")
	if err := os.WriteFile(path, []byte(`{
		"pg_dsn":"postgres://user:pass@localhost/dedup",
		"agents":[{"machine_id":"machine-a","addr":"127.0.0.1:9101"}]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGUI(path)
	if err != nil {
		t.Fatalf("LoadGUI: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:18081" || cfg.HeartbeatS != 15 {
		t.Fatalf("unexpected GUI defaults: %#v", cfg)
	}
	if cfg.FirstScreen != (FirstScreenConfig{
		HammingMax:            31,
		AspectTolerance:       0.10,
		VideoDurationWindowMs: 2000,
		ImageQualityMin:       50,
		ReadPageSize:          50000,
		GroupInsertBatch:      1000,
		SHAResolveChunk:       10000,
	}) {
		t.Fatalf("unexpected first-screen defaults: %#v", cfg.FirstScreen)
	}
}

func TestLoadGUIRejectsExistingFilesMissingRequiredConnectionFields(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "empty", body: `{}`},
		{name: "missing DSN", body: `{"listen_addr":"127.0.0.1:18081","agents":[{"addr":"127.0.0.1:9101"}]}`},
		{name: "missing Agent", body: `{"listen_addr":"127.0.0.1:18081","pg_dsn":"postgres://dedup@127.0.0.1:5432/dedup"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gui.json")
			original := []byte(test.body)
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}

			if _, err := LoadGUI(path); err == nil {
				t.Fatal("LoadGUI accepted an existing configuration missing required fields")
			}
			current, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(current, original) {
				t.Fatalf("LoadGUI rewrote invalid existing configuration:\n got %q\nwant %q", current, original)
			}
		})
	}
}

func TestLoadGUIKeepsOmittedFirstScreenFieldsAtDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gui.json")
	if err := os.WriteFile(path, []byte(`{
		"pg_dsn":"postgres://user:pass@localhost/dedup",
		"agents":[{"machine_id":"machine-a","addr":"127.0.0.1:9101"}],
		"firstscreen":{
			"hamming_max":12
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGUI(path)
	if err != nil {
		t.Fatalf("LoadGUI: %v", err)
	}
	want := FirstScreenConfig{
		HammingMax:            12,
		AspectTolerance:       0.10,
		VideoDurationWindowMs: 2000,
		ImageQualityMin:       50,
		ReadPageSize:          50000,
		GroupInsertBatch:      1000,
		SHAResolveChunk:       10000,
	}
	if cfg.FirstScreen != want {
		t.Fatalf("FirstScreen = %#v, want %#v", cfg.FirstScreen, want)
	}
}

func TestLoadGUIPreservesExplicitValidFirstScreenZeroValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gui.json")
	if err := os.WriteFile(path, []byte(`{
		"pg_dsn":"postgres://user:pass@localhost/dedup",
		"agents":[{"machine_id":"machine-a","addr":"127.0.0.1:9101"}],
		"firstscreen":{
			"hamming_max":0,
			"aspect_tolerance":0,
			"video_duration_window_ms":0,
			"image_quality_min":0,
			"read_page_size":1,
			"group_insert_batch":1,
			"sha_resolve_chunk":1
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGUI(path)
	if err != nil {
		t.Fatalf("LoadGUI: %v", err)
	}
	want := FirstScreenConfig{
		HammingMax:            0,
		AspectTolerance:       0,
		VideoDurationWindowMs: 0,
		ImageQualityMin:       0,
		ReadPageSize:          1,
		GroupInsertBatch:      1,
		SHAResolveChunk:       1,
	}
	if cfg.FirstScreen != want {
		t.Fatalf("FirstScreen = %#v, want %#v", cfg.FirstScreen, want)
	}
}

func TestLoadGUIRejectsInvalidFirstScreenValues(t *testing.T) {
	tests := []string{
		`"hamming_max":-1`,
		`"hamming_max":257`,
		`"aspect_tolerance":-0.1`,
		`"aspect_tolerance":1.1`,
		`"video_duration_window_ms":-1`,
		`"image_quality_min":-1`,
		`"image_quality_min":101`,
		`"read_page_size":0`,
		`"read_page_size":-1`,
		`"group_insert_batch":0`,
		`"group_insert_batch":-1`,
		`"sha_resolve_chunk":0`,
		`"sha_resolve_chunk":-1`,
	}
	for _, fragment := range tests {
		t.Run(fragment, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gui.json")
			body := `{
				"pg_dsn":"postgres://user:pass@localhost/dedup",
				"agents":[{"machine_id":"machine-a","addr":"127.0.0.1:9101"}],
				"firstscreen":{` + fragment + `}
			}`
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadGUI(path); err == nil {
				t.Fatalf("LoadGUI accepted %s", fragment)
			}
		})
	}
}

func TestLoadGUIAppliesAndPreservesPhase2Configuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gui.json")
	if err := os.WriteFile(path, []byte(`{
		"pg_dsn":"postgres://user:pass@localhost/dedup",
		"agents":[{"machine_id":"machine-a","addr":"127.0.0.1:9101"}]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGUI(path)
	if err != nil {
		t.Fatal(err)
	}
	wantDefaults := Phase2Config{PHashPassT2: 0.80, PHashPartThreshold: 10, SobelT3: 0.85, VideoFrames: 6, VideoAvgT4: 0.80, VideoMinPassed: 4, VideoMinValid: 4, VideoFileTimeoutS: 120, VideoFrameCommandTimeoutS: 20, ImageFileTimeoutS: 30, TaskShardSize: 5000, AutoDispatch: true}
	if cfg.Phase2 != wantDefaults {
		t.Fatalf("Phase2 defaults = %#v, want %#v", cfg.Phase2, wantDefaults)
	}

	if err := os.WriteFile(path, []byte(`{
		"pg_dsn":"postgres://user:pass@localhost/dedup",
		"agents":[{"machine_id":"machine-a","addr":"127.0.0.1:9101"}],
		"phase2":{"phash_pass_t2":0,"phash_part_threshold":0,"sobel_t3":0,"video_frames":6,"video_avg_t4":0,"video_min_passed":1,"video_min_valid":1,"video_file_timeout_s":1,"video_frame_command_timeout_s":1,"image_file_timeout_s":1,"task_shard_size":1,"auto_dispatch":false}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadGUI(path)
	if err != nil {
		t.Fatal(err)
	}
	wantExplicit := Phase2Config{VideoFrames: 6, VideoMinPassed: 1, VideoMinValid: 1, VideoFileTimeoutS: 1, VideoFrameCommandTimeoutS: 1, ImageFileTimeoutS: 1, TaskShardSize: 1}
	if cfg.Phase2 != wantExplicit {
		t.Fatalf("explicit Phase2 config = %#v, want %#v", cfg.Phase2, wantExplicit)
	}
}

func TestLoadGUIRejectsEveryInvalidPhase2Boundary(t *testing.T) {
	invalid := []string{
		`"phash_pass_t2":-0.000001`, `"phash_pass_t2":1.000001`, `"phash_part_threshold":-1`, `"phash_part_threshold":65`,
		`"sobel_t3":-0.000001`, `"sobel_t3":1.000001`, `"video_frames":5`, `"video_frames":7`,
		`"video_avg_t4":-0.000001`, `"video_avg_t4":1.000001`, `"video_min_passed":0`, `"video_min_passed":7`,
		`"video_min_valid":0`, `"video_min_valid":7`, `"video_file_timeout_s":0`, `"video_frame_command_timeout_s":0`,
		`"image_file_timeout_s":0`, `"task_shard_size":0`, `"task_shard_size":5001`,
	}
	for _, fragment := range invalid {
		t.Run(fragment, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gui.json")
			body := `{"pg_dsn":"postgres://user:pass@localhost/dedup","agents":[{"machine_id":"machine-a","addr":"127.0.0.1:9101"}],"phase2":{` + fragment + `}}`
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadGUI(path); err == nil {
				t.Fatalf("LoadGUI accepted %s", fragment)
			}
		})
	}
	for _, value := range []string{"NaN", "+Inf", "-Inf"} {
		t.Run(value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gui.json")
			body := `{"pg_dsn":"postgres://user:pass@localhost/dedup","agents":[{"machine_id":"machine-a","addr":"127.0.0.1:9101"}],"phase2":{"phash_pass_t2":` + value + `}}`
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadGUI(path); err == nil {
				t.Fatalf("LoadGUI accepted %s", value)
			}
		})
	}
}

func TestPhase2ConfigValidateRejectsNonFiniteEveryThreshold(t *testing.T) {
	for _, tt := range []struct {
		name  string
		apply func(*Phase2Config)
	}{
		{"phash NaN", func(c *Phase2Config) { c.PHashPassT2 = math.NaN() }},
		{"phash positive infinity", func(c *Phase2Config) { c.PHashPassT2 = math.Inf(1) }},
		{"phash negative infinity", func(c *Phase2Config) { c.PHashPassT2 = math.Inf(-1) }},
		{"sobel NaN", func(c *Phase2Config) { c.SobelT3 = math.NaN() }},
		{"sobel positive infinity", func(c *Phase2Config) { c.SobelT3 = math.Inf(1) }},
		{"sobel negative infinity", func(c *Phase2Config) { c.SobelT3 = math.Inf(-1) }},
		{"video NaN", func(c *Phase2Config) { c.VideoAvgT4 = math.NaN() }},
		{"video positive infinity", func(c *Phase2Config) { c.VideoAvgT4 = math.Inf(1) }},
		{"video negative infinity", func(c *Phase2Config) { c.VideoAvgT4 = math.Inf(-1) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultPhase2()
			tt.apply(&cfg)
			if err := cfg.validate(); err == nil {
				t.Fatal("Phase2Config.validate accepted non-finite threshold")
			}
		})
	}
}

func TestLoadAgentAcceptsExactDeleteConfigurationBounds(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]any
		want   DeleteConfig
	}{
		{
			name: "minimum",
			values: map[string]any{
				"pipe_name":             `\\.\pipe\a`,
				"max_entries_per_frame": 1,
				"dial_timeout_ms":       1,
				"hello_timeout_s":       1,
				"report_timeout_s":      1,
			},
			want: DeleteConfig{
				PipeName:           `\\.\pipe\a`,
				MaxEntriesPerFrame: 1,
				DialTimeoutMS:      1,
				HelloTimeoutS:      1,
				ReportTimeoutS:     1,
			},
		},
		{
			name: "maximum",
			values: map[string]any{
				"pipe_name":             `\\.\pipe\` + strings.Repeat("Z", 128),
				"max_entries_per_frame": 2000,
				"dial_timeout_ms":       30000,
				"hello_timeout_s":       60,
				"report_timeout_s":      3600,
			},
			want: DeleteConfig{
				PipeName:           `\\.\pipe\` + strings.Repeat("Z", 128),
				MaxEntriesPerFrame: 2000,
				DialTimeoutMS:      30000,
				HelloTimeoutS:      60,
				ReportTimeoutS:     3600,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadAgent(writeAgentWithDelete(t, tt.values), `C:\agent.exe`, 4)
			if err != nil {
				t.Fatalf("loadAgent: %v", err)
			}
			if cfg.Delete != tt.want {
				t.Fatalf("Delete = %#v, want %#v", cfg.Delete, tt.want)
			}
		})
	}
}

func TestLoadAgentRejectsEveryInvalidDeleteConfigurationBoundary(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value any
	}{
		{"empty pipe suffix", "pipe_name", `\\.\pipe\`},
		{"pipe suffix over 128", "pipe_name", `\\.\pipe\` + strings.Repeat("a", 129)},
		{"pipe suffix slash", "pipe_name", `\\.\pipe\a/b`},
		{"pipe suffix backslash", "pipe_name", `\\.\pipe\a\b`},
		{"pipe suffix whitespace", "pipe_name", `\\.\pipe\a b`},
		{"pipe suffix non ASCII", "pipe_name", `\\.\pipe\删除`},
		{"remote pipe", "pipe_name", `\\server\pipe\dedup-delete`},
		{"device alias", "pipe_name", `\\?\pipe\dedup-delete`},
		{"wrong prefix case", "pipe_name", `\\.\PIPE\dedup-delete`},
		{"entries below minimum", "max_entries_per_frame", 0},
		{"entries above maximum", "max_entries_per_frame", 2001},
		{"dial below minimum", "dial_timeout_ms", 0},
		{"dial above maximum", "dial_timeout_ms", 30001},
		{"hello below minimum", "hello_timeout_s", 0},
		{"hello above maximum", "hello_timeout_s", 61},
		{"report below minimum", "report_timeout_s", 0},
		{"report above maximum", "report_timeout_s", 3601},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := loadAgent(
				writeAgentWithDelete(t, map[string]any{tt.field: tt.value}),
				`C:\agent.exe`,
				4,
			); err == nil {
				t.Fatalf("loadAgent accepted %s=%#v", tt.field, tt.value)
			}
		})
	}
}

func TestAgentExampleContainsDeleteDefaultsWithoutHelperLaunchSettings(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "agent.example.json")
	cfg, err := loadAgent(path, `C:\portable\agent.exe`, 4)
	if err != nil {
		t.Fatalf("loadAgent(%s): %v", path, err)
	}
	want := DeleteConfig{
		PipeName:           `\\.\pipe\dedup-delete`,
		MaxEntriesPerFrame: 2000,
		DialTimeoutMS:      500,
		HelloTimeoutS:      5,
		ReportTimeoutS:     600,
	}
	if cfg.Delete != want {
		t.Fatalf("example Delete = %#v, want %#v", cfg.Delete, want)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	deleteMap, ok := raw["delete"].(map[string]any)
	if !ok {
		t.Fatalf("example delete JSON = %#v, want object", raw["delete"])
	}
	for _, forbidden := range []string{
		"helper_exe", "helper_exe_path", "helper_path",
		"elevate", "elevation", "auto_launch",
	} {
		if _, exists := deleteMap[forbidden]; exists {
			t.Fatalf("example delete JSON contains forbidden helper setting %q", forbidden)
		}
	}
}

func TestLoadAgentRejectsForbiddenHelperLifecycleSettings(t *testing.T) {
	forbidden := []struct {
		key   string
		value any
	}{
		{"helper_exe", `C:\portable\delete-helper.exe`},
		{"helper_exe_path", `C:\portable\delete-helper.exe`},
		{"helper_path", `C:\portable\delete-helper.exe`},
		{"elevation", true},
		{"elevate", true},
		{"auto_launch", true},
		{"auto_restart", true},
	}
	for _, setting := range forbidden {
		for _, location := range []string{"top-level", "delete"} {
			t.Run(location+"/"+setting.key, func(t *testing.T) {
				body := map[string]any{
					"machine_id": "machine-a",
					"pg_dsn":     "postgres://localhost/dedup",
				}
				if location == "top-level" {
					body[setting.key] = setting.value
				} else {
					body["delete"] = map[string]any{setting.key: setting.value}
				}
				data, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "agent.json")
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := loadAgent(path, `C:\portable\agent.exe`, 4); err == nil {
					t.Fatalf("loadAgent accepted forbidden %s setting %q", location, setting.key)
				}
			})
		}
	}
}

func TestValidateAgentReturnsIndependentNormalizedCopyWithoutMutatingInput(t *testing.T) {
	cfg := DefaultAgent()
	cfg.MachineID = "machine-a"
	cfg.PGDSN = "postgres://localhost/dedup"
	cfg.DataDir = `D:\agent-data`
	cfg.Scan.ImageExts = []string{".jpg"}
	cfg.Scan.VideoExts = []string{".mp4"}

	validated, err := ValidateAgent(cfg, `C:\suite\agent.exe`, 6)
	if err != nil {
		t.Fatalf("ValidateAgent: %v", err)
	}
	if validated == cfg {
		t.Fatal("ValidateAgent returned the input pointer")
	}
	if validated.Worker.Count != 6 || validated.Worker.ExePath != `C:\suite\worker.exe` {
		t.Fatalf("normalized worker = %#v", validated.Worker)
	}
	if cfg.Worker.Count != 0 || cfg.Worker.ExePath != "" || cfg.Thumb.CacheDir != "" {
		t.Fatalf("ValidateAgent mutated input defaults: %#v", cfg)
	}
	validated.Scan.ImageExts[0] = ".changed"
	validated.Scan.VideoExts[0] = ".changed"
	if cfg.Scan.ImageExts[0] != ".jpg" || cfg.Scan.VideoExts[0] != ".mp4" {
		t.Fatal("ValidateAgent returned slices shared with input")
	}
}

func TestValidateAgentLoadRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	for _, body := range []string{
		`{"machine_id":"machine-a","pg_dsn":"postgres://localhost/dedup","unknown":true}`,
		`{"machine_id":"machine-a","pg_dsn":"postgres://localhost/dedup"} {}`,
	} {
		path := filepath.Join(t.TempDir(), "agent.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadAgent(path, `C:\suite\agent.exe`, 4); err == nil {
			t.Fatalf("loadAgent accepted non-strict JSON: %s", body)
		}
	}
}
