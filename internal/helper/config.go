package helper

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"dedup/internal/proto"
)

type Config struct {
	PipeName             string   `json:"pipe_name"`
	AllowedRoots         []string `json:"allowed_roots"`
	DeniedRoots          []string `json:"denied_roots"`
	DefaultMode          string   `json:"default_mode"`
	AllowHardDelete      bool     `json:"allow_hard_delete"`
	RecycleDirName       string   `json:"recycle_dir_name"`
	MaxEntriesPerFrame   int      `json:"max_entries_per_frame"`
	FrameReadTimeoutSec  int      `json:"frame_read_timeout_sec"`
	FrameWriteTimeoutSec int      `json:"frame_write_timeout_sec"`
	LogDir               string   `json:"log_dir"`
}

func LoadConfig(path, executable string) (Config, error) {
	cfg := Config{
		DefaultMode:          proto.ModeSoft,
		AllowHardDelete:      true,
		RecycleDirName:       "$DedupRecycle",
		MaxEntriesPerFrame:   2000,
		FrameReadTimeoutSec:  120,
		FrameWriteTimeoutSec: 60,
	}
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("helper config: open: %w", err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("helper config: decode: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return Config{}, fmt.Errorf("helper config: decode: %w", err)
	}
	return ValidateConfig(cfg, executable)
}

// ValidateConfig returns a normalized copy without sharing root slices with cfg.
func ValidateConfig(cfg Config, executable string) (Config, error) {
	cfg.AllowedRoots = append([]string(nil), cfg.AllowedRoots...)
	cfg.DeniedRoots = append([]string(nil), cfg.DeniedRoots...)
	if err := validatePipeName(cfg.PipeName); err != nil {
		return Config{}, err
	}
	if cfg.DefaultMode != proto.ModeSoft && cfg.DefaultMode != proto.ModeHard {
		return Config{}, fmt.Errorf("helper config: default_mode must be soft or hard")
	}
	if cfg.MaxEntriesPerFrame < 1 || cfg.MaxEntriesPerFrame > 2000 {
		return Config{}, fmt.Errorf("helper config: max_entries_per_frame must be 1..2000")
	}
	if cfg.FrameReadTimeoutSec < 1 || cfg.FrameReadTimeoutSec > 3600 ||
		cfg.FrameWriteTimeoutSec < 1 || cfg.FrameWriteTimeoutSec > 3600 {
		return Config{}, fmt.Errorf("helper config: frame timeouts must be 1..3600 seconds")
	}
	if err := validateRecycleDirName(cfg.RecycleDirName); err != nil {
		return Config{}, err
	}

	allowed, denied, err := normalizeRootLists(
		cfg.AllowedRoots,
		cfg.DeniedRoots,
		cfg.RecycleDirName,
	)
	if err != nil {
		return Config{}, err
	}
	cfg.AllowedRoots = allowed
	cfg.DeniedRoots = denied

	if cfg.LogDir == "" {
		executablePath, err := normalizeLocalAbsolute(executable)
		if err != nil {
			return Config{}, fmt.Errorf("helper config: executable path: %w", err)
		}
		cfg.LogDir = filepath.Join(filepath.Dir(executablePath), "logs")
	} else {
		cfg.LogDir, err = normalizeLocalAbsolute(cfg.LogDir)
		if err != nil {
			return Config{}, fmt.Errorf("helper config: log_dir: %w", err)
		}
	}
	return cfg, nil
}

func validatePipeName(name string) error {
	const prefix = `\\.\pipe\`
	if !strings.HasPrefix(name, prefix) {
		return fmt.Errorf("helper config: invalid pipe_name")
	}
	suffix := name[len(prefix):]
	if len(suffix) < 1 || len(suffix) > 128 {
		return fmt.Errorf("helper config: invalid pipe_name")
	}
	for i := 0; i < len(suffix); i++ {
		ch := suffix[i]
		if (ch >= 'A' && ch <= 'Z') ||
			(ch >= 'a' && ch <= 'z') ||
			(ch >= '0' && ch <= '9') ||
			ch == '.' || ch == '_' || ch == '-' {
			continue
		}
		return fmt.Errorf("helper config: invalid pipe_name")
	}
	return nil
}

func validateRecycleDirName(name string) error {
	if len(name) < 1 || len(name) > 64 || name == "." || name == ".." ||
		strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return fmt.Errorf("helper config: invalid recycle_dir_name")
	}
	for i := 0; i < len(name); i++ {
		ch := name[i]
		if (ch >= 'A' && ch <= 'Z') ||
			(ch >= 'a' && ch <= 'z') ||
			(ch >= '0' && ch <= '9') ||
			ch == '.' || ch == '_' || ch == '$' || ch == '-' {
			continue
		}
		return fmt.Errorf("helper config: invalid recycle_dir_name")
	}
	base := name
	if dot := strings.IndexByte(base, '.'); dot >= 0 {
		base = base[:dot]
	}
	upper := strings.ToUpper(base)
	if upper == "CON" || upper == "PRN" || upper == "AUX" || upper == "NUL" ||
		isReservedNumberedDevice(upper, "COM") ||
		isReservedNumberedDevice(upper, "LPT") {
		return fmt.Errorf("helper config: invalid recycle_dir_name")
	}
	return nil
}

func isReservedNumberedDevice(value, prefix string) bool {
	return len(value) == 4 &&
		strings.HasPrefix(value, prefix) &&
		value[3] >= '1' && value[3] <= '9'
}

func normalizeRootLists(
	allowedValues []string,
	deniedValues []string,
	recycleDirName string,
) ([]string, []string, error) {
	if len(allowedValues) == 0 {
		return nil, nil, fmt.Errorf("helper config: allowed_roots must not be empty")
	}
	if err := ensureOrdinalIgnoreCase(); err != nil {
		return nil, nil, err
	}
	policy, err := loadSystemPathPolicy()
	if err != nil {
		return nil, nil, err
	}
	allowed, err := normalizeDistinctRoots(allowedValues, "allowed_roots")
	if err != nil {
		return nil, nil, err
	}
	for _, root := range allowed {
		if err := policy.validateAllowedRoot(root, recycleDirName); err != nil {
			return nil, nil, err
		}
	}
	denied, err := normalizeDistinctRoots(deniedValues, "denied_roots")
	if err != nil {
		return nil, nil, err
	}
	return allowed, denied, nil
}

func normalizeDistinctRoots(values []string, label string) ([]string, error) {
	result := make([]string, 0, len(values))
	for _, value := range values {
		root, err := normalizeLocalAbsolute(value)
		if err != nil {
			return nil, fmt.Errorf("helper config: %s: %w", label, err)
		}
		for _, existing := range result {
			if ordinalEqualFold(existing, root) {
				return nil, fmt.Errorf("helper config: duplicate %s entry %q", label, root)
			}
		}
		result = append(result, root)
	}
	return result, nil
}

func normalizeLocalAbsolute(value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value ||
		strings.IndexByte(value, 0) >= 0 {
		return "", fmt.Errorf("path must be a non-empty local absolute path")
	}
	value = strings.ReplaceAll(value, "/", `\`)
	lower := strings.ToLower(value)
	if strings.HasPrefix(value, `\\`) ||
		strings.HasPrefix(lower, `\??\`) {
		return "", fmt.Errorf("UNC and device paths are forbidden")
	}
	volume := filepath.VolumeName(value)
	if len(volume) != 2 ||
		!isASCIILetter(volume[0]) ||
		volume[1] != ':' ||
		!filepath.IsAbs(value) {
		return "", fmt.Errorf("path must be drive-letter absolute")
	}
	clean := filepath.Clean(value)
	if clean == "." || clean == `\` || filepath.VolumeName(clean) == "" {
		return "", fmt.Errorf("path is root-ambiguous")
	}
	components := pathComponents(clean)
	for _, component := range components {
		if strings.HasSuffix(component, ".") ||
			strings.HasSuffix(component, " ") {
			return "", fmt.Errorf("path component has an unsafe trailing dot or space")
		}
		if strings.Contains(component, "~") {
			return "", fmt.Errorf("path component may be an 8.3 alias")
		}
	}
	return clean, nil
}

func isASCIILetter(ch byte) bool {
	return ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z'
}

func pathComponents(path string) []string {
	volume := filepath.VolumeName(path)
	remainder := strings.TrimPrefix(path[len(volume):], `\`)
	if remainder == "" {
		return nil
	}
	return strings.Split(remainder, `\`)
}

type systemPathPolicy struct {
	systemVolumeRoot string
	systemRoot       string
	programDirs      []string
	programData      string
	usersRoot        string
}

func loadSystemPathPolicy() (systemPathPolicy, error) {
	systemRootValue := os.Getenv("SystemRoot")
	systemRoot, err := normalizeLocalAbsolute(systemRootValue)
	if err != nil {
		return systemPathPolicy{}, fmt.Errorf("helper config: SystemRoot: %w", err)
	}
	volumeRoot := filepath.VolumeName(systemRoot) + `\`
	programData := os.Getenv("ProgramData")
	if programData == "" {
		programData = filepath.Join(volumeRoot, "ProgramData")
	}
	programData, err = normalizeLocalAbsolute(programData)
	if err != nil {
		return systemPathPolicy{}, fmt.Errorf("helper config: ProgramData: %w", err)
	}
	programValues := []string{
		os.Getenv("ProgramFiles"),
		os.Getenv("ProgramFiles(x86)"),
		os.Getenv("ProgramW6432"),
		filepath.Join(volumeRoot, "Program Files"),
		filepath.Join(volumeRoot, "Program Files (x86)"),
	}
	programDirs := make([]string, 0, len(programValues))
	for _, value := range programValues {
		if value == "" {
			continue
		}
		normalized, err := normalizeLocalAbsolute(value)
		if err != nil {
			return systemPathPolicy{}, fmt.Errorf("helper config: Program Files: %w", err)
		}
		duplicate := false
		for _, existing := range programDirs {
			if ordinalEqualFold(existing, normalized) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			programDirs = append(programDirs, normalized)
		}
	}
	return systemPathPolicy{
		systemVolumeRoot: volumeRoot,
		systemRoot:       systemRoot,
		programDirs:      programDirs,
		programData:      programData,
		usersRoot:        filepath.Join(volumeRoot, "Users"),
	}, nil
}

func (p systemPathPolicy) validateAllowedRoot(root, recycleDirName string) error {
	volumeRoot := filepath.VolumeName(root) + `\`
	if ordinalEqualFold(root, volumeRoot) ||
		ordinalEqualFold(root, p.systemVolumeRoot) {
		return fmt.Errorf("helper config: volume root is forbidden")
	}
	if equalOrBelow(root, p.systemRoot) {
		return fmt.Errorf("helper config: SystemRoot is forbidden")
	}
	for _, programDir := range p.programDirs {
		if equalOrBelow(root, programDir) {
			return fmt.Errorf("helper config: Program Files is forbidden")
		}
	}
	if equalOrBelow(root, p.programData) {
		return fmt.Errorf("helper config: ProgramData is forbidden")
	}
	if ordinalEqualFold(root, p.usersRoot) {
		return fmt.Errorf("helper config: broad Users root is forbidden")
	}
	if equalOrBelow(root, filepath.Join(volumeRoot, recycleDirName)) {
		return fmt.Errorf("helper config: recycle tree root is forbidden")
	}
	return nil
}

func equalOrBelow(path, root string) bool {
	if ordinalEqualFold(path, root) {
		return true
	}
	prefix := root
	if !strings.HasSuffix(prefix, `\`) {
		prefix += `\`
	}
	return len(path) > len(prefix) &&
		ordinalEqualFold(path[:len(prefix)], prefix)
}

func strictDescendant(path, root string) bool {
	return !ordinalEqualFold(path, root) && equalOrBelow(path, root)
}
