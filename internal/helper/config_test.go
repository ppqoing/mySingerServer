package helper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dedup/internal/proto"
)

func TestConfigAppliesExactDefaultsAndNormalizesPaths(t *testing.T) {
	root := filepath.Join(runTempDir(t), "Media")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(runTempDir(t), "bin", "delete-helper.exe")
	path := writeHelperConfig(t, map[string]any{
		"pipe_name":     `\\.\pipe\dedup-delete`,
		"allowed_roots": []string{root + `\.`},
	})

	cfg, err := LoadConfig(path, executable)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DefaultMode != proto.ModeSoft ||
		!cfg.AllowHardDelete ||
		cfg.RecycleDirName != "$DedupRecycle" ||
		cfg.MaxEntriesPerFrame != 2000 ||
		cfg.FrameReadTimeoutSec != 120 ||
		cfg.FrameWriteTimeoutSec != 60 {
		t.Fatalf("defaults = %#v", cfg)
	}
	wantRoot := getLongExistingPath(t, filepath.Clean(root))
	if len(cfg.AllowedRoots) != 1 || cfg.AllowedRoots[0] != wantRoot {
		t.Fatalf("allowed_roots = %#v, want %q", cfg.AllowedRoots, wantRoot)
	}
	executableBase := filepath.Dir(filepath.Dir(executable))
	wantLog := filepath.Join(
		getLongExistingPath(t, executableBase),
		"bin",
		"logs",
	)
	if cfg.LogDir != wantLog {
		t.Fatalf("log_dir = %q, want %q", cfg.LogDir, wantLog)
	}
}

func TestConfigRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	root := filepath.Join(runTempDir(t), "media")
	executable := filepath.Join(runTempDir(t), "delete-helper.exe")
	for _, tt := range []struct {
		name string
		body string
	}{
		{
			name: "unknown field",
			body: `{"pipe_name":"\\\\.\\pipe\\dedup-delete","allowed_roots":[` +
				quoteJSON(t, root) + `],"unexpected":true}`,
		},
		{
			name: "trailing object",
			body: `{"pipe_name":"\\\\.\\pipe\\dedup-delete","allowed_roots":[` +
				quoteJSON(t, root) + `]} {}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(runTempDir(t), "helper.json")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(path, executable); err == nil {
				t.Fatalf("LoadConfig accepted %s", tt.name)
			}
		})
	}
}

func TestConfigRejectsInvalidPipeBoundsModesAndRecycleNames(t *testing.T) {
	root := filepath.Join(runTempDir(t), "media")
	executable := filepath.Join(runTempDir(t), "delete-helper.exe")
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
		{"device pipe", "pipe_name", `\\?\pipe\dedup-delete`},
		{"wrong prefix case", "pipe_name", `\\.\PIPE\dedup-delete`},
		{"entries below minimum", "max_entries_per_frame", 0},
		{"entries above maximum", "max_entries_per_frame", 2001},
		{"read timeout below minimum", "frame_read_timeout_sec", 0},
		{"read timeout above maximum", "frame_read_timeout_sec", 3601},
		{"write timeout below minimum", "frame_write_timeout_sec", 0},
		{"write timeout above maximum", "frame_write_timeout_sec", 3601},
		{"bad default mode", "default_mode", "erase"},
		{"empty recycle", "recycle_dir_name", ""},
		{"dot recycle", "recycle_dir_name", "."},
		{"dotdot recycle", "recycle_dir_name", ".."},
		{"reserved recycle", "recycle_dir_name", "CoM1"},
		{"reserved with extension", "recycle_dir_name", "NUL.txt"},
		{"recycle trailing dot", "recycle_dir_name", "recycle."},
		{"recycle trailing space", "recycle_dir_name", "recycle "},
		{"recycle slash", "recycle_dir_name", "a/b"},
		{"recycle backslash", "recycle_dir_name", `a\b`},
		{"recycle colon", "recycle_dir_name", "a:b"},
		{"recycle non ASCII", "recycle_dir_name", "删除"},
		{"recycle too long", "recycle_dir_name", strings.Repeat("a", 65)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := map[string]any{
				"pipe_name":     `\\.\pipe\dedup-delete`,
				"allowed_roots": []string{root},
			}
			values[tt.field] = tt.value
			if _, err := LoadConfig(writeHelperConfig(t, values), executable); err == nil {
				t.Fatalf("LoadConfig accepted %s=%#v", tt.field, tt.value)
			}
		})
	}
}

func TestConfigAcceptsInclusiveBoundsModesAndHardDeleteValues(t *testing.T) {
	root := filepath.Join(runTempDir(t), "media")
	executable := filepath.Join(runTempDir(t), "delete-helper.exe")
	tests := []struct {
		name   string
		field  string
		value  any
		assert func(*testing.T, Config)
	}{
		{
			name:  "entries minimum",
			field: "max_entries_per_frame",
			value: 1,
			assert: func(t *testing.T, cfg Config) {
				if cfg.MaxEntriesPerFrame != 1 {
					t.Fatalf("MaxEntriesPerFrame = %d, want 1", cfg.MaxEntriesPerFrame)
				}
			},
		},
		{
			name:  "entries maximum",
			field: "max_entries_per_frame",
			value: 2000,
			assert: func(t *testing.T, cfg Config) {
				if cfg.MaxEntriesPerFrame != 2000 {
					t.Fatalf("MaxEntriesPerFrame = %d, want 2000", cfg.MaxEntriesPerFrame)
				}
			},
		},
		{
			name:  "read timeout minimum",
			field: "frame_read_timeout_sec",
			value: 1,
			assert: func(t *testing.T, cfg Config) {
				if cfg.FrameReadTimeoutSec != 1 {
					t.Fatalf("FrameReadTimeoutSec = %d, want 1", cfg.FrameReadTimeoutSec)
				}
			},
		},
		{
			name:  "read timeout maximum",
			field: "frame_read_timeout_sec",
			value: 3600,
			assert: func(t *testing.T, cfg Config) {
				if cfg.FrameReadTimeoutSec != 3600 {
					t.Fatalf("FrameReadTimeoutSec = %d, want 3600", cfg.FrameReadTimeoutSec)
				}
			},
		},
		{
			name:  "write timeout minimum",
			field: "frame_write_timeout_sec",
			value: 1,
			assert: func(t *testing.T, cfg Config) {
				if cfg.FrameWriteTimeoutSec != 1 {
					t.Fatalf("FrameWriteTimeoutSec = %d, want 1", cfg.FrameWriteTimeoutSec)
				}
			},
		},
		{
			name:  "write timeout maximum",
			field: "frame_write_timeout_sec",
			value: 3600,
			assert: func(t *testing.T, cfg Config) {
				if cfg.FrameWriteTimeoutSec != 3600 {
					t.Fatalf("FrameWriteTimeoutSec = %d, want 3600", cfg.FrameWriteTimeoutSec)
				}
			},
		},
		{
			name:  "explicit soft mode",
			field: "default_mode",
			value: proto.ModeSoft,
			assert: func(t *testing.T, cfg Config) {
				if cfg.DefaultMode != proto.ModeSoft {
					t.Fatalf("DefaultMode = %q, want %q", cfg.DefaultMode, proto.ModeSoft)
				}
			},
		},
		{
			name:  "explicit hard mode",
			field: "default_mode",
			value: proto.ModeHard,
			assert: func(t *testing.T, cfg Config) {
				if cfg.DefaultMode != proto.ModeHard {
					t.Fatalf("DefaultMode = %q, want %q", cfg.DefaultMode, proto.ModeHard)
				}
			},
		},
		{
			name:  "hard delete true",
			field: "allow_hard_delete",
			value: true,
			assert: func(t *testing.T, cfg Config) {
				if !cfg.AllowHardDelete {
					t.Fatal("AllowHardDelete = false, want true")
				}
			},
		},
		{
			name:  "hard delete false",
			field: "allow_hard_delete",
			value: false,
			assert: func(t *testing.T, cfg Config) {
				if cfg.AllowHardDelete {
					t.Fatal("AllowHardDelete = true, want false")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := map[string]any{
				"pipe_name":     `\\.\pipe\dedup-delete`,
				"allowed_roots": []string{root},
			}
			values[tt.field] = tt.value
			cfg, err := LoadConfig(writeHelperConfig(t, values), executable)
			if err != nil {
				t.Fatalf("LoadConfig rejected %s=%#v: %v", tt.field, tt.value, err)
			}
			tt.assert(t, cfg)
		})
	}
}

func TestConfigRejectsAmbiguousDuplicateAndProtectedRoots(t *testing.T) {
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		t.Fatal("NEEDS_CONTEXT: SystemRoot is unavailable")
	}
	volume := filepath.VolumeName(systemRoot) + `\`
	programFiles := os.Getenv("ProgramFiles")
	if programFiles == "" {
		programFiles = filepath.Join(volume, "Program Files")
	}
	programData := os.Getenv("ProgramData")
	if programData == "" {
		programData = filepath.Join(volume, "ProgramData")
	}
	usersRoot := filepath.Join(volume, "Users")
	executable := filepath.Join(runTempDir(t), "delete-helper.exe")
	validRoot := filepath.Join(runTempDir(t), "media")

	tests := []struct {
		name    string
		allowed []string
		denied  []string
		recycle string
	}{
		{"empty allowed list", nil, nil, "$DedupRecycle"},
		{"empty root", []string{""}, nil, "$DedupRecycle"},
		{"drive relative", []string{`C:media`}, nil, "$DedupRecycle"},
		{"volume relative", []string{`\media`}, nil, "$DedupRecycle"},
		{"UNC", []string{`\\server\share\media`}, nil, "$DedupRecycle"},
		{"device path", []string{`\\?\C:\media`}, nil, "$DedupRecycle"},
		{"NT device path", []string{`\??\C:\media`}, nil, "$DedupRecycle"},
		{"volume root", []string{volume}, nil, "$DedupRecycle"},
		{"system root", []string{systemRoot}, nil, "$DedupRecycle"},
		{"below system root", []string{filepath.Join(systemRoot, "Temp")}, nil, "$DedupRecycle"},
		{"program files", []string{programFiles}, nil, "$DedupRecycle"},
		{"below program files", []string{filepath.Join(programFiles, "Dedup")}, nil, "$DedupRecycle"},
		{"program data", []string{programData}, nil, "$DedupRecycle"},
		{"broad users", []string{usersRoot}, nil, "$DedupRecycle"},
		{
			"allowed duplicate after case and slash normalization",
			[]string{validRoot, strings.ToUpper(strings.ReplaceAll(validRoot, `\`, `/`)) + "/."},
			nil,
			"$DedupRecycle",
		},
		{
			"denied duplicate after normalization",
			[]string{validRoot},
			[]string{
				filepath.Join(validRoot, "private"),
				strings.ToUpper(filepath.Join(validRoot, "private")) + `\.`,
			},
			"$DedupRecycle",
		},
		{
			"allowed root in recycle tree",
			[]string{filepath.Join(volume, "$Task2Recycle", "nested")},
			nil,
			"$Task2Recycle",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := map[string]any{
				"pipe_name":        `\\.\pipe\dedup-delete`,
				"allowed_roots":    tt.allowed,
				"denied_roots":     tt.denied,
				"recycle_dir_name": tt.recycle,
			}
			if _, err := LoadConfig(writeHelperConfig(t, values), executable); err == nil {
				t.Fatalf("LoadConfig accepted protected/ambiguous roots %#v", tt.allowed)
			}
		})
	}
}

func TestConfigAllowsNarrowUserMediaAndNormalizesExplicitLogDir(t *testing.T) {
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		t.Fatal("NEEDS_CONTEXT: SystemRoot is unavailable")
	}
	volume := filepath.VolumeName(systemRoot) + `\`
	root := filepath.Join(volume, "Users", "task2-fixture-user", "media")
	logDir := filepath.Join(runTempDir(t), "logs", "..", "helper-logs")
	cfg, err := LoadConfig(writeHelperConfig(t, map[string]any{
		"pipe_name":     `\\.\pipe\dedup-delete`,
		"allowed_roots": []string{root},
		"log_dir":       strings.ReplaceAll(logDir, `\`, `/`),
	}), filepath.Join(runTempDir(t), "delete-helper.exe"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	wantLog := filepath.Join(
		getLongExistingPath(t, filepath.Dir(filepath.Clean(logDir))),
		filepath.Base(filepath.Clean(logDir)),
	)
	if cfg.LogDir != wantLog {
		t.Fatalf("log_dir = %q, want %q", cfg.LogDir, wantLog)
	}
}

func TestConfigUsesWindowsOrdinalIgnoreCaseForRootIdentity(t *testing.T) {
	base := runTempDir(t)
	asciiK := filepath.Join(base, "K-media")
	kelvinSign := filepath.Join(base, "K-media")
	cfg, err := LoadConfig(writeHelperConfig(t, map[string]any{
		"pipe_name":     `\\.\pipe\dedup-delete`,
		"allowed_roots": []string{asciiK, kelvinSign},
	}), filepath.Join(runTempDir(t), "delete-helper.exe"))
	if err != nil {
		t.Fatalf("LoadConfig rejected OrdinalIgnoreCase-distinct roots: %v", err)
	}
	if len(cfg.AllowedRoots) != 2 {
		t.Fatalf("allowed_roots = %#v, want two distinct roots", cfg.AllowedRoots)
	}
}

func TestConfigRejectsUnsafeWin32AliasComponents(t *testing.T) {
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		t.Fatal("NEEDS_CONTEXT: SystemRoot is unavailable")
	}
	volumeRoot := filepath.VolumeName(systemRoot) + `\`
	executable := filepath.Join(runTempDir(t), "delete-helper.exe")
	for _, root := range []string{
		systemRoot + ".",
		filepath.Join(volumeRoot, filepath.Base(systemRoot)+" ", "nested"),
		filepath.Join(runTempDir(t), "MEDIA~1"),
	} {
		t.Run(strings.ReplaceAll(root, `\`, "_"), func(t *testing.T) {
			if _, err := LoadConfig(writeHelperConfig(t, map[string]any{
				"pipe_name":     `\\.\pipe\dedup-delete`,
				"allowed_roots": []string{root},
			}), executable); err == nil {
				t.Fatalf("LoadConfig accepted unsafe Win32 alias root %q", root)
			}
		})
	}
}

func TestConfigRejectsLexicalProgramFilesShortAliasWithoutFilesystemAccess(t *testing.T) {
	const shortProgramFiles = `C:\PROGRA~1`
	if _, err := LoadConfig(writeHelperConfig(t, map[string]any{
		"pipe_name":     `\\.\pipe\dedup-delete`,
		"allowed_roots": []string{shortProgramFiles},
	}), filepath.Join(runTempDir(t), "delete-helper.exe")); err == nil {
		t.Fatalf(
			"LoadConfig accepted Program Files short alias %q",
			shortProgramFiles,
		)
	}
}

func TestConfigRejectsNonLocalAbsoluteLogDirs(t *testing.T) {
	root := filepath.Join(runTempDir(t), "media")
	executable := filepath.Join(runTempDir(t), "delete-helper.exe")
	for _, value := range []string{
		`logs`,
		`C:logs`,
		`\logs`,
		`\\server\share\logs`,
		`\\?\C:\logs`,
		`\??\C:\logs`,
	} {
		t.Run(strings.ReplaceAll(value, `\`, "_"), func(t *testing.T) {
			if _, err := LoadConfig(writeHelperConfig(t, map[string]any{
				"pipe_name":     `\\.\pipe\dedup-delete`,
				"allowed_roots": []string{root},
				"log_dir":       value,
			}), executable); err == nil {
				t.Fatalf("LoadConfig accepted log_dir %q", value)
			}
		})
	}
}

func writeHelperConfig(t *testing.T, values map[string]any) string {
	t.Helper()
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(runTempDir(t), "helper.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func quoteJSON(t *testing.T, value string) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestValidateConfigReturnsIndependentNormalizedCopyWithoutMutatingInput(t *testing.T) {
	root := filepath.Join(runTempDir(t), "validate-media")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	inputRoot := root + `\.`
	cfg := Config{
		PipeName:             `\\.\pipe\dedup-delete`,
		AllowedRoots:         []string{inputRoot},
		DeniedRoots:          []string{filepath.Join(root, "private")},
		DefaultMode:          proto.ModeSoft,
		AllowHardDelete:      false,
		RecycleDirName:       "$DedupRecycle",
		MaxEntriesPerFrame:   2000,
		FrameReadTimeoutSec:  120,
		FrameWriteTimeoutSec: 60,
	}

	validated, err := ValidateConfig(cfg, filepath.Join(runTempDir(t), "helper.exe"))
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	if cfg.AllowedRoots[0] != inputRoot || cfg.LogDir != "" {
		t.Fatalf("ValidateConfig mutated input: %#v", cfg)
	}
	if validated.AllowedRoots[0] == inputRoot || validated.LogDir == "" {
		t.Fatalf("ValidateConfig did not normalize output: %#v", validated)
	}
	validated.AllowedRoots[0] = `Z:\changed`
	validated.DeniedRoots[0] = `Z:\changed`
	if cfg.AllowedRoots[0] != inputRoot || cfg.DeniedRoots[0] == `Z:\changed` {
		t.Fatal("ValidateConfig returned root slices shared with input")
	}
}
