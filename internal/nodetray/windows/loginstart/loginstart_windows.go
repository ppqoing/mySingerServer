//go:build windows

package loginstart

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	runKeyPath   = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValueName = "MySingerServerNodeTray"
)

var errValueNotFound = errors.New("loginstart value not found")

var resolveFinalExecutablePath = finalDOSPath

type Service interface {
	Enabled() (bool, string, error)
	Enable(executable string) error
	Disable() error
}

type registryBackend interface {
	Get() (string, error)
	Set(value string) error
	Delete() error
}

type service struct {
	backend          registryBackend
	executable       string
	expectedRunValue string
}

func New(executable string) (Service, error) {
	return newServiceWithBackend(executable, windowsRegistryBackend{})
}

func newServiceWithBackend(executable string, backend registryBackend) (*service, error) {
	if backend == nil {
		return nil, errors.New("loginstart: registry backend is required")
	}
	canonical, err := canonicalTrayExecutable(executable)
	if err != nil {
		return nil, err
	}
	return &service{
		backend:          backend,
		executable:       canonical,
		expectedRunValue: `"` + canonical + `" --background`,
	}, nil
}

func (s *service) Enabled() (bool, string, error) {
	value, err := s.backend.Get()
	if errors.Is(err, errValueNotFound) {
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("loginstart: read fixed Run value: %w", err)
	}
	return value == s.expectedRunValue, value, nil
}

func (s *service) Enable(executable string) error {
	canonical, err := canonicalTrayExecutable(executable)
	if err != nil {
		return err
	}
	if !strings.EqualFold(canonical, s.executable) {
		return errors.New("loginstart: executable path does not match current deployment")
	}
	if err := s.backend.Set(s.expectedRunValue); err != nil {
		return fmt.Errorf("loginstart: write fixed Run value: %w", err)
	}
	return nil
}

func (s *service) Disable() error {
	err := s.backend.Delete()
	if errors.Is(err, errValueNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("loginstart: delete fixed Run value: %w", err)
	}
	return nil
}

func canonicalTrayExecutable(value string) (string, error) {
	if err := validateTrayExecutablePath(value); err != nil {
		return "", err
	}
	file, err := os.Open(value)
	if err != nil {
		return "", errors.New("loginstart: executable is unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("loginstart: executable is not a regular file")
	}
	canonical, err := resolveFinalExecutablePath(windows.Handle(file.Fd()))
	if err != nil {
		return "", fmt.Errorf("loginstart: resolve executable path: %w", err)
	}
	if strings.HasPrefix(canonical, `\\?\UNC\`) {
		canonical = `\\` + strings.TrimPrefix(canonical, `\\?\UNC\`)
	} else {
		canonical = strings.TrimPrefix(canonical, `\\?\`)
	}
	canonical = filepath.Clean(canonical)
	if err := validateTrayExecutablePath(canonical); err != nil {
		return "", errors.New("loginstart: resolved executable is not nodetray.exe")
	}
	return canonical, nil
}

func validateTrayExecutablePath(value string) error {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsRune(value, '"') {
		return errors.New("loginstart: invalid executable path")
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return errors.New("loginstart: invalid executable path")
		}
	}
	if !filepath.IsAbs(value) || !strings.EqualFold(filepath.Base(value), "nodetray.exe") ||
		!strings.EqualFold(filepath.Ext(value), ".exe") {
		return errors.New("loginstart: executable must be an absolute nodetray.exe path")
	}
	return nil
}

func finalDOSPath(handle windows.Handle) (string, error) {
	size := uint32(512)
	for {
		buffer := make([]uint16, size)
		length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], size, 0)
		if err != nil {
			return "", err
		}
		if length < size {
			return windows.UTF16ToString(buffer[:length]), nil
		}
		size = length + 1
	}
}

type windowsRegistryBackend struct{}

func (windowsRegistryBackend) Get() (string, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", errValueNotFound
	}
	if err != nil {
		return "", err
	}
	defer key.Close()
	value, _, err := key.GetStringValue(runValueName)
	if errors.Is(err, registry.ErrNotExist) {
		return "", errValueNotFound
	}
	return value, err
}

func (windowsRegistryBackend) Set(value string) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	return key.SetStringValue(runValueName, value)
}

func (windowsRegistryBackend) Delete() error {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return errValueNotFound
	}
	if err != nil {
		return err
	}
	defer key.Close()
	err = key.DeleteValue(runValueName)
	if errors.Is(err, registry.ErrNotExist) {
		return errValueNotFound
	}
	return err
}
