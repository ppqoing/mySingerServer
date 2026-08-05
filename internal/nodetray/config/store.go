package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	agentconfig "dedup/internal/config"
	"dedup/internal/helper"
	"dedup/internal/nodetray/traymodel"
)

var ErrSaveVerify = errors.New("save_verify_failed")

type Paths struct {
	TraySettings     string
	AgentConfig      string
	HelperConfig     string
	AgentExecutable  string
	HelperExecutable string
}

type Store struct {
	paths     Paths
	testHooks storeTestHooks
}

type PreparedWrite struct {
	TargetPath    string
	CanonicalJSON []byte
	SHA256        string
}

// storeTestHooks are intentionally private fault boundaries. Production stores
// leave them zero-valued; package tests use them to prove that sync/validation
// happens before the atomic replacement publishes a file.
type storeTestHooks struct {
	afterSync     func(tempPath, destination string) error
	beforeReplace func(tempPath, destination string) error
	replace       func(tempPath, destination string) error
}

type canonicalLoader func(path string) ([]byte, error)

func NewStore(paths Paths) (*Store, error) {
	paths = Paths{
		TraySettings:     filepath.Clean(paths.TraySettings),
		AgentConfig:      filepath.Clean(paths.AgentConfig),
		HelperConfig:     filepath.Clean(paths.HelperConfig),
		AgentExecutable:  filepath.Clean(paths.AgentExecutable),
		HelperExecutable: filepath.Clean(paths.HelperExecutable),
	}
	values := []string{paths.TraySettings, paths.AgentConfig, paths.HelperConfig, paths.AgentExecutable, paths.HelperExecutable}
	for _, value := range values {
		if value == "" || value == "." || !filepath.IsAbs(value) || filepath.Base(value) == "." {
			return nil, errors.New("node config store: invalid configured path")
		}
	}
	for i := range values {
		for j := i + 1; j < len(values); j++ {
			if strings.EqualFold(values[i], values[j]) {
				return nil, errors.New("node config store: configured paths must be distinct")
			}
		}
	}
	helperDirectory := filepath.Dir(paths.HelperConfig)
	for _, writableDirectory := range []string{
		filepath.Dir(paths.TraySettings),
		filepath.Dir(paths.AgentConfig),
	} {
		if sameOrBelowDirectory(helperDirectory, writableDirectory) {
			return nil, errors.New("node config store: protected Helper path overlaps a writable configuration directory")
		}
	}
	if err := platformValidateProtectedHelper(paths.HelperConfig); err != nil {
		return nil, errors.New("node config store: protected Helper ACL validation failed")
	}
	return &Store{paths: paths}, nil
}

func sameOrBelowDirectory(path, root string) bool {
	relative, err := filepath.Rel(strings.ToLower(root), strings.ToLower(path))
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return relative == "." ||
		(relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func (s *Store) LoadTraySettings() (traymodel.TraySettings, error) {
	value, _, err := loadTraySettings(s.paths.TraySettings)
	if err != nil {
		return traymodel.TraySettings{}, storeError(s.paths.TraySettings, "strict load failed")
	}
	return value, nil
}

func (s *Store) EnsureTraySettings(defaults traymodel.TraySettings) error {
	if err := defaults.Validate(); err != nil {
		return storeError(s.paths.TraySettings, "default validation failed")
	}
	if _, err := os.Stat(s.paths.TraySettings); err == nil {
		_, err := s.LoadTraySettings()
		return err
	} else if !errors.Is(err, os.ErrNotExist) {
		return storeError(s.paths.TraySettings, "initialization check failed")
	}
	if _, err := os.Stat(s.paths.TraySettings + ".last-good"); err == nil {
		if _, err := trayCanonicalLoader(s.paths.TraySettings + ".last-good"); err != nil {
			return storeError(s.paths.TraySettings, "last-good validation failed")
		}
		return storeError(s.paths.TraySettings, "settings missing with last-good present")
	} else if !errors.Is(err, os.ErrNotExist) {
		return storeError(s.paths.TraySettings, "initialization check failed")
	}
	return s.withWriteLock(s.paths.TraySettings, func() error {
		if _, err := os.Stat(s.paths.TraySettings); err == nil {
			_, err := s.LoadTraySettings()
			return err
		} else if !errors.Is(err, os.ErrNotExist) {
			return storeError(s.paths.TraySettings, "initialization check failed")
		}
		if _, err := os.Stat(s.paths.TraySettings + ".last-good"); err == nil {
			if _, err := trayCanonicalLoader(s.paths.TraySettings + ".last-good"); err != nil {
				return storeError(s.paths.TraySettings, "last-good validation failed")
			}
			return storeError(s.paths.TraySettings, "settings missing with last-good present")
		} else if !errors.Is(err, os.ErrNotExist) {
			return storeError(s.paths.TraySettings, "initialization check failed")
		}
		data, err := canonicalJSON(defaults)
		if err != nil {
			return storeError(s.paths.TraySettings, "canonical encoding failed")
		}
		if err := s.saveLocked(s.paths.TraySettings, data, trayCanonicalLoader); err != nil {
			return storeError(s.paths.TraySettings, "atomic initialization failed")
		}
		return nil
	})
}

func (s *Store) SaveTraySettings(value traymodel.TraySettings) error {
	if err := value.Validate(); err != nil {
		return storeError(s.paths.TraySettings, "validation failed")
	}
	data, err := canonicalJSON(value)
	if err != nil {
		return storeError(s.paths.TraySettings, "canonical encoding failed")
	}
	return s.withWriteLock(s.paths.TraySettings, func() error {
		if err := s.saveLocked(s.paths.TraySettings, data, trayCanonicalLoader); err != nil {
			return storeError(s.paths.TraySettings, "atomic save failed")
		}
		return nil
	})
}

func (s *Store) LoadAgentForm() (AgentForm, error) {
	cfg, _, err := loadAgentConfig(s.paths.AgentConfig, s.paths.AgentExecutable)
	if err != nil {
		if configAndBackupAbsent(s.paths.AgentConfig) {
			form, defaultErr := firstRunAgentForm(s.paths)
			if defaultErr != nil {
				return AgentForm{}, storeError(s.paths.AgentConfig, "first-run form unavailable")
			}
			return form, nil
		}
		return AgentForm{}, storeError(s.paths.AgentConfig, "strict load failed")
	}
	form, err := AgentToForm(cfg)
	if err != nil {
		return AgentForm{}, storeError(s.paths.AgentConfig, "form conversion failed")
	}
	return form, nil
}

func (s *Store) SaveAgentForm(value AgentForm) (string, error) {
	var digest string
	err := s.withWriteLock(s.paths.AgentConfig, func() error {
		base, err := s.loadAgentEditBase()
		if err != nil {
			return storeError(s.paths.AgentConfig, "existing configuration invalid")
		}
		cfg, err := AgentFromForm(value, base)
		if err != nil {
			return storeError(s.paths.AgentConfig, "form validation failed")
		}
		cfg, err = agentconfig.ValidateAgent(cfg, s.paths.AgentExecutable, runtime.NumCPU())
		if err != nil {
			return storeError(s.paths.AgentConfig, "shared validation failed")
		}
		data, err := canonicalJSON(cfg)
		if err != nil {
			return storeError(s.paths.AgentConfig, "canonical encoding failed")
		}
		if err := s.saveLocked(s.paths.AgentConfig, data, s.agentCanonicalLoader); err != nil {
			return storeErrorCause(s.paths.AgentConfig, "atomic save failed", err)
		}
		digest = sha256Hex(data)
		return nil
	})
	if err != nil {
		return "", err
	}
	return digest, nil
}

// ValidateAgentForm applies the same edit-base conversion and shared Agent
// validator as SaveAgentForm, without taking a write lock or publishing data.
func (s *Store) ValidateAgentForm(value AgentForm) []FieldError {
	if s == nil {
		return []FieldError{{Field: "agent", Code: "unavailable", Message: "Agent 配置验证不可用"}}
	}
	base, err := s.loadAgentEditBase()
	if err != nil {
		return []FieldError{{Field: "agent", Code: "invalid_base", Message: "现有 Agent 配置不可用"}}
	}
	cfg, err := AgentFromForm(value, base)
	if err != nil {
		return stableFieldErrors(err, "agent", "Agent 配置无效")
	}
	if _, err := agentconfig.ValidateAgent(cfg, s.paths.AgentExecutable, runtime.NumCPU()); err != nil {
		return []FieldError{{Field: "agent", Code: "invalid", Message: "Agent 配置无效"}}
	}
	return nil
}

func (s *Store) LoadHelperForm() (HelperForm, error) {
	cfg, _, err := loadHelperConfig(s.paths.HelperConfig, s.paths.HelperExecutable)
	if err != nil {
		if configAndBackupAbsent(s.paths.HelperConfig) {
			return firstRunHelperForm(s.paths), nil
		}
		return HelperForm{}, storeError(s.paths.HelperConfig, "strict load failed")
	}
	return HelperToForm(cfg), nil
}

func configAndBackupAbsent(path string) bool {
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	if _, err := os.Stat(path + ".last-good"); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	return true
}

func firstRunAgentForm(paths Paths) (AgentForm, error) {
	cfg := agentconfig.DefaultAgent()
	cfg.DataDir = filepath.Join(filepath.Dir(paths.AgentConfig), "data")
	cfg.PGDSN = "postgres://127.0.0.1:5432/dedup?sslmode=prefer"
	cfg.Worker.Count = runtime.NumCPU()
	cfg.Worker.ExePath = filepath.Join(filepath.Dir(paths.AgentExecutable), "worker.exe")
	cfg.Thumb.CacheDir = filepath.Join(cfg.DataDir, "thumbcache")
	return AgentToForm(cfg)
}

func firstRunHelperForm(paths Paths) HelperForm {
	return HelperForm{
		PipeName:             `\\.\pipe\dedup-delete`,
		AllowedRoots:         []string{},
		DeniedRoots:          []string{},
		DefaultMode:          "soft",
		AllowHardDelete:      false,
		RecycleDirName:       "$DedupRecycle",
		MaxEntriesPerFrame:   2000,
		FrameReadTimeoutSec:  120,
		FrameWriteTimeoutSec: 60,
		LogDir:               filepath.Join(filepath.Dir(paths.HelperConfig), "logs"),
	}
}

func (s *Store) PrepareHelperWrite(value HelperForm) (PreparedWrite, error) {
	cfg, err := HelperFromForm(value)
	if err != nil {
		return PreparedWrite{}, storeError(s.paths.HelperConfig, "form validation failed")
	}
	cfg, err = helper.ValidateConfig(cfg, s.paths.HelperExecutable)
	if err != nil {
		return PreparedWrite{}, storeError(s.paths.HelperConfig, "shared validation failed")
	}
	data, err := canonicalJSON(cfg)
	if err != nil {
		return PreparedWrite{}, storeError(s.paths.HelperConfig, "canonical encoding failed")
	}
	return preparedWrite(s.paths.HelperConfig, data), nil
}

// ValidateHelperForm applies the same conversion and shared Helper validator as
// PrepareHelperWrite without writing the protected Helper target.
func (s *Store) ValidateHelperForm(value HelperForm) []FieldError {
	if s == nil {
		return []FieldError{{Field: "helper", Code: "unavailable", Message: "Helper 配置验证不可用"}}
	}
	cfg, err := HelperFromForm(value)
	if err != nil {
		return stableFieldErrors(err, "helper", "Helper 配置无效")
	}
	if _, err := helper.ValidateConfig(cfg, s.paths.HelperExecutable); err != nil {
		return []FieldError{{Field: "helper", Code: "invalid", Message: "Helper 配置无效"}}
	}
	return nil
}

func stableFieldErrors(err error, field, message string) []FieldError {
	var fieldErr *FieldError
	if errors.As(err, &fieldErr) && fieldErr != nil {
		return []FieldError{{Field: fieldErr.Field, Code: fieldErr.Code, Message: fieldErr.Message}}
	}
	return []FieldError{{Field: field, Code: "invalid", Message: message}}
}

func (s *Store) AgentFingerprint() (string, error) {
	data, err := s.agentCanonicalLoader(s.paths.AgentConfig)
	if err != nil {
		return "", storeError(s.paths.AgentConfig, "strict load failed")
	}
	return sha256Hex(data), nil
}

func (s *Store) HelperFingerprint() (string, error) {
	data, err := s.helperCanonicalLoader(s.paths.HelperConfig)
	if err != nil {
		return "", storeError(s.paths.HelperConfig, "strict load failed")
	}
	return sha256Hex(data), nil
}

func (s *Store) RestoreAgentBackup() error {
	return s.withWriteLock(s.paths.AgentConfig, func() error {
		data, err := s.agentCanonicalLoader(s.paths.AgentConfig + ".last-good")
		if err != nil {
			return storeError(s.paths.AgentConfig, "last-good validation failed")
		}
		if err := s.writeAtomic(s.paths.AgentConfig, data, s.agentCanonicalLoader); err != nil {
			return storeError(s.paths.AgentConfig, "last-good restore failed")
		}
		return nil
	})
}

func (s *Store) RestoreHelperBackup() (PreparedWrite, error) {
	data, err := s.helperCanonicalLoader(s.paths.HelperConfig + ".last-good")
	if err != nil {
		return PreparedWrite{}, storeError(s.paths.HelperConfig, "last-good validation failed")
	}
	return preparedWrite(s.paths.HelperConfig, data), nil
}

func (s *Store) loadAgentEditBase() (*agentconfig.AgentConfig, error) {
	cfg, _, err := loadAgentConfig(s.paths.AgentConfig, s.paths.AgentExecutable)
	if err == nil {
		return cfg, nil
	}
	backup, _, backupErr := loadAgentConfig(s.paths.AgentConfig+".last-good", s.paths.AgentExecutable)
	if backupErr == nil {
		return backup, nil
	}
	if errors.Is(err, os.ErrNotExist) && errors.Is(backupErr, os.ErrNotExist) {
		return nil, nil
	}
	return nil, err
}

func (s *Store) withWriteLock(target string, action func() error) error {
	if err := ensureWritableDirectory(filepath.Dir(target)); err != nil {
		return storeError(target, "prepare directory failed")
	}
	lock, err := platformAcquireLock(target + ".lock")
	if err != nil {
		return storeError(target, "lock acquisition failed")
	}
	actionErr := action()
	closeErr := lock.Close()
	if actionErr != nil {
		return actionErr
	}
	if closeErr != nil {
		return storeError(target, "lock release failed")
	}
	return nil
}

func (s *Store) saveLocked(target string, data []byte, loader canonicalLoader) error {
	backup := target + ".last-good"
	_, statErr := os.Stat(target)
	switch {
	case statErr == nil:
		oldData, err := loader(target)
		if err == nil {
			if err := s.writeAtomic(backup, oldData, loader); err != nil {
				return err
			}
		} else if _, backupErr := loader(backup); backupErr != nil {
			return err
		}
	case errors.Is(statErr, os.ErrNotExist):
		if _, backupStatErr := os.Stat(backup); backupStatErr == nil {
			if _, err := loader(backup); err != nil {
				return err
			}
		} else if errors.Is(backupStatErr, os.ErrNotExist) {
			if err := s.writeAtomic(backup, data, loader); err != nil {
				return err
			}
		} else {
			return backupStatErr
		}
	default:
		return statErr
	}
	return s.writeAtomic(target, data, loader)
}

func (s *Store) writeAtomic(target string, data []byte, loader canonicalLoader) (err error) {
	directory := filepath.Dir(target)
	temp, err := os.CreateTemp(directory, "."+filepath.Base(target)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()
	if err := temp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := platformRestrictWritable(tempPath); err != nil {
		return err
	}
	if hook := s.testHooks.afterSync; hook != nil {
		if err := hook(tempPath, target); err != nil {
			return err
		}
	}
	validated, err := loader(tempPath)
	if err != nil || !bytes.Equal(validated, data) {
		if err != nil {
			return err
		}
		return errors.New("canonical reread mismatch")
	}
	if hook := s.testHooks.beforeReplace; hook != nil {
		if err := hook(tempPath, target); err != nil {
			return err
		}
	}
	replace := platformAtomicReplace
	if hook := s.testHooks.replace; hook != nil {
		replace = hook
	}
	if err := replace(tempPath, target); err != nil {
		return err
	}
	formal, err := loader(target)
	if err != nil {
		return fmt.Errorf("%w: formal target invalid", ErrSaveVerify)
	}
	if !bytes.Equal(formal, data) {
		return fmt.Errorf("%w: formal target mismatch", ErrSaveVerify)
	}
	if err := platformSyncDirectory(directory); err != nil {
		return err
	}
	return nil
}

func ensureWritableDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return platformRestrictWritable(path)
}

func loadTraySettings(path string) (traymodel.TraySettings, []byte, error) {
	var value traymodel.TraySettings
	if err := strictDecodeFile(path, &value); err != nil {
		return traymodel.TraySettings{}, nil, err
	}
	if err := value.Validate(); err != nil {
		return traymodel.TraySettings{}, nil, err
	}
	data, err := canonicalJSON(value)
	return value, data, err
}

func trayCanonicalLoader(path string) ([]byte, error) {
	_, data, err := loadTraySettings(path)
	return data, err
}

func loadAgentConfig(path, executable string) (*agentconfig.AgentConfig, []byte, error) {
	cfg := agentconfig.DefaultAgent()
	if err := strictDecodeFile(path, cfg); err != nil {
		return nil, nil, err
	}
	cfg, err := agentconfig.ValidateAgent(cfg, executable, runtime.NumCPU())
	if err != nil {
		return nil, nil, err
	}
	data, err := canonicalJSON(cfg)
	return cfg, data, err
}

func (s *Store) agentCanonicalLoader(path string) ([]byte, error) {
	_, data, err := loadAgentConfig(path, s.paths.AgentExecutable)
	return data, err
}

func loadHelperConfig(path, executable string) (helper.Config, []byte, error) {
	cfg, err := helper.LoadConfig(path, executable)
	if err != nil {
		return helper.Config{}, nil, err
	}
	data, err := canonicalJSON(cfg)
	return cfg, data, err
}

func (s *Store) helperCanonicalLoader(path string) ([]byte, error) {
	_, data, err := loadHelperConfig(path, s.paths.HelperExecutable)
	return data, err
}

func strictDecodeFile(path string, destination any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func canonicalJSON(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func preparedWrite(target string, data []byte) PreparedWrite {
	copyOfData := append([]byte(nil), data...)
	return PreparedWrite{
		TargetPath:    target,
		CanonicalJSON: copyOfData,
		SHA256:        sha256Hex(copyOfData),
	}
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func storeError(path, summary string) error {
	base := filepath.Base(path)
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = "config"
	}
	return errors.New("node config store " + base + ": " + summary)
}

func storeErrorCause(path, summary string, cause error) error {
	if errors.Is(cause, ErrSaveVerify) {
		return fmt.Errorf("%w: %v", ErrSaveVerify, storeError(path, summary))
	}
	return storeError(path, summary)
}
