package gui

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"dedup/internal/config"
)

func TestLoadOrCreateGUIConfigCreatesCompleteDefaultForMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manager", "gui.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := LoadOrCreateGUIConfig(path)
	if err != nil {
		t.Fatalf("LoadOrCreateGUIConfig: %v", err)
	}
	if err := config.ValidateGUI(got); err != nil {
		t.Fatalf("created configuration is incomplete: %v", err)
	}
	loaded, err := config.LoadGUI(path)
	if err != nil {
		t.Fatalf("LoadGUI created configuration: %v", err)
	}
	if !reflect.DeepEqual(loaded, got) {
		t.Fatalf("LoadGUI created configuration = %#v, want %#v", loaded, got)
	}
}

func TestLoadOrCreateGUIConfigDoesNotRewriteExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gui.json")
	original := []byte("{\n  \"listen_addr\": \"127.0.0.1:18081\",\n  \"pg_dsn\": \"postgres://dedup@127.0.0.1:5432/dedup\",\n  \"agents\": [{\"addr\": \"127.0.0.1:9101\"}]\n}\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadOrCreateGUIConfig(path)
	if err != nil {
		t.Fatalf("LoadOrCreateGUIConfig: %v", err)
	}
	if err := config.ValidateGUI(got); err != nil {
		t.Fatalf("existing configuration was not read strictly: %v", err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, original) {
		t.Fatalf("existing configuration was rewritten:\n got %q\nwant %q", current, original)
	}
}

func TestLoadOrCreateGUIConfigConcurrentCallsPublishOneCompleteJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gui.json")
	const callers = 32
	results := make([]*config.GUIConfig, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for index := 0; index < callers; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			cfg, err := LoadOrCreateGUIConfig(path)
			if err == nil {
				results[index] = cfg
			}
			errs <- err
		}(index)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	loaded, err := config.LoadGUI(path)
	if err != nil {
		t.Fatalf("LoadGUI final configuration: %v", err)
	}
	if err := config.ValidateGUI(loaded); err != nil {
		t.Fatalf("final configuration is incomplete: %v", err)
	}
	for index, got := range results {
		if !reflect.DeepEqual(got, loaded) {
			t.Fatalf("concurrent result %d = %#v, want %#v", index, got, loaded)
		}
	}
	if temps, err := filepath.Glob(filepath.Join(dir, ".gui.json.*.tmp")); err != nil {
		t.Fatal(err)
	} else if len(temps) != 0 {
		t.Fatalf("temporary files remain: %v", temps)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := canonicalGUIConfig(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, canonical) {
		t.Fatalf("final JSON = %s, want canonical %s", data, canonical)
	}
	if len(data) == 0 {
		t.Fatal(fmt.Errorf("final configuration is empty"))
	}
}
