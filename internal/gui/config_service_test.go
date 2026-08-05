package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"dedup/internal/config"
)

func testGUIConfig() *config.GUIConfig {
	cfg := config.DefaultGUI()
	cfg.PGDSN = "postgres://user:pass@127.0.0.1:5432/dedup"
	cfg.Agents = []config.AgentEndpoint{{Addr: "192.168.1.10:9101"}}
	return cfg
}

func writeTestGUIConfig(t *testing.T, path string, cfg *config.GUIConfig) []byte {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestGUIConfigServiceUsesNonDefaultAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	defaultPath := filepath.Join(dir, "gui.json")
	customPath := filepath.Join(dir, "custom-gui.json")
	runtime := testGUIConfig()
	defaultBytes := writeTestGUIConfig(t, defaultPath, runtime)
	writeTestGUIConfig(t, customPath, runtime)

	service, err := NewGUIConfigService(customPath, runtime)
	if err != nil {
		t.Fatal(err)
	}
	changed := testGUIConfig()
	changed.Agents[0].Addr = "192.168.1.11:9101"
	if _, err := service.Save(context.Background(), changed); err != nil {
		t.Fatal(err)
	}

	gotCustom, err := config.LoadGUI(customPath)
	if err != nil {
		t.Fatal(err)
	}
	if gotCustom.Agents[0].Addr != "192.168.1.11:9101" {
		t.Fatalf("custom agent address = %q", gotCustom.Agents[0].Addr)
	}
	gotDefault, err := os.ReadFile(defaultPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotDefault, defaultBytes) {
		t.Fatal("save modified the default gui.json instead of the configured path")
	}
}

func TestGUIConfigServiceWritesCanonicalUTF8WithoutBOM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gui.json")
	runtime := testGUIConfig()
	writeTestGUIConfig(t, path, runtime)
	service, err := NewGUIConfigService(path, runtime)
	if err != nil {
		t.Fatal(err)
	}
	changed := testGUIConfig()
	changed.HeartbeatS = 30
	if _, err := service.Save(context.Background(), changed); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}) {
		t.Fatal("saved configuration has an UTF-8 BOM")
	}
	if !bytes.HasSuffix(data, []byte("\n")) || !bytes.Contains(data, []byte("\n  \"listen_addr\"")) {
		t.Fatalf("saved configuration is not canonical indented JSON: %q", data)
	}
	if _, err := config.LoadGUI(path); err != nil {
		t.Fatalf("saved configuration cannot be loaded: %v", err)
	}
}

func TestGUIConfigServiceReportsSavedAndRestartRequired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gui.json")
	runtime := testGUIConfig()
	writeTestGUIConfig(t, path, runtime)
	service, err := NewGUIConfigService(path, runtime)
	if err != nil {
		t.Fatal(err)
	}

	changed := testGUIConfig()
	changed.ListenAddr = "127.0.0.1:18080"
	result, err := service.Save(context.Background(), changed)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Saved || !result.RestartRequired {
		t.Fatalf("changed save result = %#v", result)
	}
	snapshot, err := service.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.RestartRequired || snapshot.Config.ListenAddr != "127.0.0.1:18080" {
		t.Fatalf("changed snapshot = %#v", snapshot)
	}

	result, err = service.Save(context.Background(), runtime)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Saved || result.RestartRequired {
		t.Fatalf("runtime restore result = %#v", result)
	}
}

func TestGUIConfigServiceSkipsSemanticallyIdenticalSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gui.json")
	runtime := testGUIConfig()
	writeTestGUIConfig(t, path, runtime)
	service, err := NewGUIConfigService(path, runtime)
	if err != nil {
		t.Fatal(err)
	}
	replaceCalls := 0
	service.replace = func(_, _ string) error {
		replaceCalls++
		return errors.New("replace should not be called")
	}

	result, err := service.Save(context.Background(), testGUIConfig())
	if err != nil {
		t.Fatal(err)
	}
	if result.Saved || result.RestartRequired || replaceCalls != 0 {
		t.Fatalf("identical save result=%#v replaceCalls=%d", result, replaceCalls)
	}
}

func TestGUIConfigServiceReplaceFailurePreservesOriginal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gui.json")
	runtime := testGUIConfig()
	original := writeTestGUIConfig(t, path, runtime)
	service, err := NewGUIConfigService(path, runtime)
	if err != nil {
		t.Fatal(err)
	}
	service.replace = func(_, _ string) error { return errors.New("synthetic replace failure") }
	changed := testGUIConfig()
	changed.HeartbeatS = 31

	if _, err := service.Save(context.Background(), changed); err == nil {
		t.Fatal("Save succeeded after replace failure")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("replace failure changed the original configuration")
	}
	temps, err := filepath.Glob(filepath.Join(dir, ".gui.json.*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("temporary files remain after failure: %v", temps)
	}
}

func TestGUIConfigServiceConcurrentSavesRemainCompleteJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gui.json")
	runtime := testGUIConfig()
	writeTestGUIConfig(t, path, runtime)
	service, err := NewGUIConfigService(path, runtime)
	if err != nil {
		t.Fatal(err)
	}

	configs := make([]*config.GUIConfig, 8)
	for index := range configs {
		cfg := testGUIConfig()
		cfg.Agents[0] = config.AgentEndpoint{
			Addr: "192.168.1." + string(rune('2'+index)) + ":9101",
		}
		configs[index] = cfg
	}
	var wg sync.WaitGroup
	errorsSeen := make(chan error, len(configs))
	for _, cfg := range configs {
		wg.Add(1)
		go func(candidate *config.GUIConfig) {
			defer wg.Done()
			_, saveErr := service.Save(context.Background(), candidate)
			errorsSeen <- saveErr
		}(cfg)
	}
	wg.Wait()
	close(errorsSeen)
	for saveErr := range errorsSeen {
		if saveErr != nil {
			t.Fatalf("concurrent Save: %v", saveErr)
		}
	}

	final, err := config.LoadGUI(path)
	if err != nil {
		t.Fatalf("final configuration is incomplete: %v", err)
	}
	matched := false
	for _, candidate := range configs {
		if reflect.DeepEqual(final, candidate) {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatalf("final configuration is a mixed write: %#v", final)
	}
}

func TestGUIConfigServiceRemovesTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gui.json")
	runtime := testGUIConfig()
	writeTestGUIConfig(t, path, runtime)
	service, err := NewGUIConfigService(path, runtime)
	if err != nil {
		t.Fatal(err)
	}
	changed := testGUIConfig()
	changed.HeartbeatS = 32
	if _, err := service.Save(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	temps, err := filepath.Glob(filepath.Join(dir, ".gui.json.*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("temporary files remain after success: %v", temps)
	}
}
