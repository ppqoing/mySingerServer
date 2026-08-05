package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"dedup/internal/config"
)

type GUIConfigSnapshot struct {
	Config          *config.GUIConfig `json:"config"`
	RestartRequired bool              `json:"restart_required"`
}

type GUIConfigSaveResult struct {
	Saved           bool `json:"saved"`
	RestartRequired bool `json:"restart_required"`
}

type GUIConfigService struct {
	mu               sync.Mutex
	path             string
	runtimeCanonical []byte
	replace          func(source, destination string) error
}

func NewGUIConfigService(path string, runtime *config.GUIConfig) (*GUIConfigService, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	runtimeCanonical, err := canonicalGUIConfig(runtime)
	if err != nil {
		return nil, fmt.Errorf("validate runtime config: %w", err)
	}
	return &GUIConfigService{
		path:             absolute,
		runtimeCanonical: runtimeCanonical,
		replace:          replaceFileAtomically,
	}, nil
}

func (s *GUIConfigService) Load() (GUIConfigSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg, err := config.LoadGUI(s.path)
	if err != nil {
		return GUIConfigSnapshot{}, fmt.Errorf("read %s: %w", filepath.Base(s.path), err)
	}
	canonical, err := canonicalGUIConfig(cfg)
	if err != nil {
		return GUIConfigSnapshot{}, fmt.Errorf("validate %s: %w", filepath.Base(s.path), err)
	}
	return GUIConfigSnapshot{
		Config:          cfg,
		RestartRequired: !bytes.Equal(canonical, s.runtimeCanonical),
	}, nil
}

func (s *GUIConfigService) Save(ctx context.Context, cfg *config.GUIConfig) (GUIConfigSaveResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	canonical, err := canonicalGUIConfig(cfg)
	if err != nil {
		return GUIConfigSaveResult{}, err
	}
	restartRequired := !bytes.Equal(canonical, s.runtimeCanonical)

	current, loadErr := config.LoadGUI(s.path)
	if loadErr == nil {
		currentCanonical, canonicalErr := canonicalGUIConfig(current)
		if canonicalErr != nil {
			return GUIConfigSaveResult{}, fmt.Errorf("validate %s: %w", filepath.Base(s.path), canonicalErr)
		}
		if bytes.Equal(currentCanonical, canonical) {
			return GUIConfigSaveResult{Saved: false, RestartRequired: restartRequired}, nil
		}
	} else {
		var pathErr *os.PathError
		if errors.As(loadErr, &pathErr) && !errors.Is(pathErr.Err, os.ErrNotExist) {
			return GUIConfigSaveResult{}, fmt.Errorf("read %s: %w", filepath.Base(s.path), loadErr)
		}
	}

	if err := ctx.Err(); err != nil {
		return GUIConfigSaveResult{}, err
	}
	temp, err := os.CreateTemp(filepath.Dir(s.path), "."+filepath.Base(s.path)+".*.tmp")
	if err != nil {
		return GUIConfigSaveResult{}, fmt.Errorf("create temporary config for %s: %w", filepath.Base(s.path), err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)

	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return GUIConfigSaveResult{}, fmt.Errorf("set temporary config permissions for %s: %w", filepath.Base(s.path), err)
	}
	if _, err := temp.Write(canonical); err != nil {
		_ = temp.Close()
		return GUIConfigSaveResult{}, fmt.Errorf("write temporary config for %s: %w", filepath.Base(s.path), err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return GUIConfigSaveResult{}, fmt.Errorf("sync temporary config for %s: %w", filepath.Base(s.path), err)
	}
	if err := temp.Close(); err != nil {
		return GUIConfigSaveResult{}, fmt.Errorf("close temporary config for %s: %w", filepath.Base(s.path), err)
	}

	verified, err := config.LoadGUI(tempPath)
	if err != nil {
		return GUIConfigSaveResult{}, fmt.Errorf("verify temporary config for %s: %w", filepath.Base(s.path), err)
	}
	verifiedCanonical, err := canonicalGUIConfig(verified)
	if err != nil || !bytes.Equal(verifiedCanonical, canonical) {
		if err != nil {
			return GUIConfigSaveResult{}, fmt.Errorf("verify temporary config for %s: %w", filepath.Base(s.path), err)
		}
		return GUIConfigSaveResult{}, fmt.Errorf("verify temporary config for %s: canonical mismatch", filepath.Base(s.path))
	}
	if err := ctx.Err(); err != nil {
		return GUIConfigSaveResult{}, err
	}
	if err := s.replace(tempPath, s.path); err != nil {
		return GUIConfigSaveResult{}, fmt.Errorf("replace %s: %w", filepath.Base(s.path), err)
	}
	return GUIConfigSaveResult{Saved: true, RestartRequired: restartRequired}, nil
}

func canonicalGUIConfig(cfg *config.GUIConfig) ([]byte, error) {
	if err := config.ValidateGUI(cfg); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	return append(data, '\n'), nil
}
