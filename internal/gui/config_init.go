package gui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"dedup/internal/config"
)

type guiConfigInitLock interface {
	Release() error
}

func LoadOrCreateGUIConfig(path string) (_ *config.GUIConfig, err error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}

	cfg, err := config.LoadGUI(absolute)
	if err == nil {
		return cfg, nil
	}
	if !errors.Is(err, os.ErrNotExist) && !isGUIConfigInitTransientReadError(err) {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(absolute), err)
	}

	lock, err := lockGUIConfigInit(absolute)
	if err != nil {
		return nil, err
	}
	defer func() {
		if releaseErr := lock.Release(); err == nil && releaseErr != nil {
			err = releaseErr
		}
	}()

	cfg, err = config.LoadGUI(absolute)
	if err == nil {
		return cfg, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(absolute), err)
	}

	cfg = config.DefaultGUI()
	canonical, err := canonicalGUIConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("validate default %s: %w", filepath.Base(absolute), err)
	}
	if err := writeCanonicalGUIConfig(absolute, canonical, replaceFileAtomically, restrictGUIConfigPermissions, nil); err != nil {
		return nil, err
	}
	return cfg, nil
}
