package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentconfig "dedup/internal/config"
	"dedup/internal/helper"
	"dedup/internal/nodetray/traymodel"
)

func TestStoreRequiresAllConfiguredFilesToBeAbsoluteAndDistinct(t *testing.T) {
	root := t.TempDir()
	valid := Paths{
		TraySettings:     filepath.Join(root, "tray", "tray.json"),
		AgentConfig:      filepath.Join(root, "agent", "agent.json"),
		HelperConfig:     filepath.Join(root, "helper", "helper.json"),
		AgentExecutable:  `D:\nodetray-test-binaries\agent.exe`,
		HelperExecutable: `D:\nodetray-test-binaries\helper.exe`,
	}
	tests := []struct {
		name  string
		paths Paths
	}{
		{name: "missing Agent executable", paths: func() Paths { value := valid; value.AgentExecutable = ""; return value }()},
		{name: "relative Helper executable", paths: func() Paths { value := valid; value.HelperExecutable = "helper.exe"; return value }()},
		{name: "same Agent config and executable", paths: func() Paths { value := valid; value.AgentExecutable = value.AgentConfig; return value }()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewStore(tt.paths); err == nil {
				t.Fatal("NewStore accepted invalid configured file paths")
			}
		})
	}
}

func TestEnsureTraySettingsCreatesDefaultsOnlyForACompletelyAbsentFirstRun(t *testing.T) {
	store, paths := newTestStore(t)
	defaults := validTraySettings()
	if err := store.EnsureTraySettings(defaults); err != nil {
		t.Fatalf("EnsureTraySettings: %v", err)
	}
	if got, err := store.LoadTraySettings(); err != nil || !reflect.DeepEqual(got, defaults) {
		t.Fatalf("LoadTraySettings() = %#v, %v; want defaults", got, err)
	}
	if _, err := os.Stat(paths.TraySettings + ".last-good"); err != nil {
		t.Fatalf("first-run backup: %v", err)
	}
}

func TestEnsureTraySettingsDoesNotOverwriteCorruptSettings(t *testing.T) {
	store, paths := newTestStore(t)
	corrupt := []byte(`{"unexpected":"secret"}`)
	writeBytesFixture(t, paths.TraySettings, corrupt)
	if err := store.EnsureTraySettings(validTraySettings()); err == nil {
		t.Fatal("EnsureTraySettings accepted corrupt tray settings")
	}
	if got := readFixture(t, paths.TraySettings); !reflect.DeepEqual(got, corrupt) {
		t.Fatalf("corrupt tray settings were overwritten: %q", got)
	}
}

func TestStoreReturnsSafeInteractiveDefaultsOnlyForCompletelyAbsentComponentConfigs(t *testing.T) {
	store, paths := newTestStore(t)

	agent, err := store.LoadAgentForm()
	if err != nil {
		t.Fatalf("LoadAgentForm first run: %v", err)
	}
	if agent.ListenHost != "0.0.0.0" || agent.ListenPort != 9101 ||
		agent.Database.Host != "127.0.0.1" || agent.Database.Port != 5432 || agent.Database.Database != "dedup" ||
		agent.Database.Password != "" || agent.Database.PasswordStored || agent.Database.ReplacePassword ||
		agent.Worker.ExePath != filepath.Join(filepath.Dir(paths.AgentExecutable), "worker.exe") ||
		agent.DataDir != filepath.Join(filepath.Dir(paths.AgentConfig), "data") {
		t.Fatalf("unsafe Agent first-run form = %#v", agent)
	}
	if agent.Scan.ImageExts == nil || agent.Scan.VideoExts == nil {
		t.Fatalf("Agent first-run extension lists must serialize as arrays, got image=%#v video=%#v", agent.Scan.ImageExts, agent.Scan.VideoExts)
	}
	helper, err := store.LoadHelperForm()
	if err != nil {
		t.Fatalf("LoadHelperForm first run: %v", err)
	}
	if helper.PipeName != `\\.\pipe\dedup-delete` || len(helper.AllowedRoots) != 0 || len(helper.DeniedRoots) != 0 ||
		helper.DefaultMode != "soft" || helper.AllowHardDelete || helper.RecycleDirName != "$DedupRecycle" ||
		helper.LogDir != filepath.Join(filepath.Dir(paths.HelperConfig), "logs") {
		t.Fatalf("unsafe Helper first-run form = %#v", helper)
	}
	for _, path := range []string{paths.AgentConfig, paths.AgentConfig + ".last-good", paths.HelperConfig, paths.HelperConfig + ".last-good"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("first-run form read wrote %q: %v", filepath.Base(path), err)
		}
	}
}

func TestStoreDoesNotHideMissingOfficialConfigWhenBackupExists(t *testing.T) {
	store, paths := newTestStore(t)
	writeBytesFixture(t, paths.AgentConfig+".last-good", mustCanonicalJSON(t, fullyPopulatedAgentConfig()))
	writeBytesFixture(t, paths.HelperConfig+".last-good", mustCanonicalJSON(t, validHelperConfig(t)))

	if _, err := store.LoadAgentForm(); err == nil {
		t.Fatal("LoadAgentForm hid a missing official config behind first-run defaults")
	}
	if _, err := store.LoadHelperForm(); err == nil {
		t.Fatal("LoadHelperForm hid a missing official config behind first-run defaults")
	}
}

func TestStoreFingerprintsStrictCanonicalConfigurationsWithoutReturningTheirContents(t *testing.T) {
	store, paths := newTestStore(t)
	agent := fullyPopulatedAgentConfig()
	agent.Worker.ExePath = ""
	writeBytesFixture(t, paths.AgentConfig, mustCanonicalJSON(t, agent))
	helper := validHelperConfig(t)
	helper.LogDir = ""
	writeBytesFixture(t, paths.HelperConfig, mustCanonicalJSON(t, helper))

	agentDigest, err := store.AgentFingerprint()
	if err != nil || len(agentDigest) != 64 {
		t.Fatalf("AgentFingerprint() = %q, %v", agentDigest, err)
	}
	helperDigest, err := store.HelperFingerprint()
	if err != nil || len(helperDigest) != 64 {
		t.Fatalf("HelperFingerprint() = %q, %v", helperDigest, err)
	}
	for _, value := range []string{agentDigest, helperDigest} {
		if strings.Contains(value, "secret") || strings.Contains(value, "{") {
			t.Fatalf("fingerprint exposed configuration content: %q", value)
		}
	}
}

func TestStoreStrictlyRejectsUnknownFieldsAndTrailingValuesWithoutLeakingInput(t *testing.T) {
	store, paths := newTestStore(t)
	agentJSON := mustCanonicalJSON(t, fullyPopulatedAgentConfig())
	helperJSON := mustCanonicalJSON(t, validHelperConfig(t))
	trayJSON := mustCanonicalJSON(t, validTraySettings())

	tests := []struct {
		name   string
		path   string
		valid  []byte
		load   func() error
		secret string
	}{
		{
			name: "tray settings", path: paths.TraySettings, valid: trayJSON,
			load:   func() error { _, err := store.LoadTraySettings(); return err },
			secret: "tray-top-secret",
		},
		{
			name: "agent config", path: paths.AgentConfig, valid: agentJSON,
			load:   func() error { _, err := store.LoadAgentForm(); return err },
			secret: "agent-top-secret",
		},
		{
			name: "helper config", path: paths.HelperConfig, valid: helperJSON,
			load:   func() error { _, err := store.LoadHelperForm(); return err },
			secret: "helper-top-secret",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/unknown", func(t *testing.T) {
			var object map[string]any
			if err := json.Unmarshal(tt.valid, &object); err != nil {
				t.Fatal(err)
			}
			object["unexpected"] = tt.secret
			writeJSONFixture(t, tt.path, object)
			err := tt.load()
			if err == nil {
				t.Fatal("strict load accepted an unknown field")
			}
			assertErrorRedacted(t, err, tt.secret, filepath.Dir(tt.path))
		})
		t.Run(tt.name+"/trailing", func(t *testing.T) {
			writeBytesFixture(t, tt.path, append(append([]byte(nil), tt.valid...), []byte(` {"secret":"`+tt.secret+`"}`)...))
			err := tt.load()
			if err == nil {
				t.Fatal("strict load accepted a trailing JSON value")
			}
			assertErrorRedacted(t, err, tt.secret, filepath.Dir(tt.path))
		})
	}
}

func TestStoreRejectsProtectedHelperInsideWritableConfigurationDirectory(t *testing.T) {
	root := t.TempDir()
	for _, helperPath := range []string{
		filepath.Join(root, "writable", "helper.json"),
		filepath.Join(root, "writable", "protected", "helper.json"),
	} {
		_, err := NewStore(Paths{
			TraySettings:     filepath.Join(root, "tray", "settings.json"),
			AgentConfig:      filepath.Join(root, "writable", "agent.json"),
			HelperConfig:     helperPath,
			AgentExecutable:  filepath.Join(root, "bin", "agent.exe"),
			HelperExecutable: filepath.Join(root, "bin", "helper.exe"),
		})
		if err == nil {
			t.Fatalf("NewStore accepted protected Helper path %q inside writable Agent directory", helperPath)
		}
		assertErrorRedacted(t, err, root, helperPath)
	}
}

func TestStoreWritesCanonicalTrayJSONAndOneLastGoodBackup(t *testing.T) {
	store, paths := newTestStore(t)
	first := validTraySettings()
	if err := store.SaveTraySettings(first); err != nil {
		t.Fatalf("SaveTraySettings(first): %v", err)
	}
	wantFirst := "{\n" +
		"  \"loginStartTray\": true,\n" +
		"  \"agentStartMode\": \"manual\",\n" +
		"  \"helperEnabled\": true,\n" +
		"  \"helperStartMode\": \"automatic\",\n" +
		"  \"closeToTray\": true,\n" +
		"  \"refreshIntervalSeconds\": 2,\n" +
		"  \"notificationLevel\": \"important\"\n" +
		"}\n"
	if got := string(readFixture(t, paths.TraySettings)); got != wantFirst {
		t.Fatalf("canonical tray JSON = %q, want %q", got, wantFirst)
	}

	second := first
	second.RefreshIntervalSeconds = 3
	second.NotificationLevel = traymodel.NotifyAll
	if err := store.SaveTraySettings(second); err != nil {
		t.Fatalf("SaveTraySettings(second): %v", err)
	}
	if got := string(readFixture(t, paths.TraySettings+".last-good")); got != wantFirst {
		t.Fatalf("last-good = %q, want first canonical version", got)
	}
	matches, err := filepath.Glob(paths.TraySettings + ".last-good*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0] != paths.TraySettings+".last-good" {
		t.Fatalf("last-good files = %#v, want one stable backup", matches)
	}
}

func TestStoreSaveAgentUsesActualCanonicalBytesForSHAAndPreservesSecretState(t *testing.T) {
	store, paths := newTestStore(t)
	base := fullyPopulatedAgentConfig()
	var legacyDocument map[string]any
	if err := json.Unmarshal(mustCanonicalJSON(t, base), &legacyDocument); err != nil {
		t.Fatal(err)
	}
	legacyDocument["machine_id"] = "legacy-manual-id"
	legacyJSON, err := json.MarshalIndent(legacyDocument, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeBytesFixture(t, paths.AgentConfig, append(legacyJSON, '\n'))
	form, err := AgentToForm(base)
	if err != nil {
		t.Fatal(err)
	}
	form.Database.ReplacePassword = true
	form.Database.Password = "replacement-secret"

	digest, err := store.SaveAgentForm(form)
	if err != nil {
		t.Fatalf("SaveAgentForm: %v", err)
	}
	written := readFixture(t, paths.AgentConfig)
	wantDigest := sha256.Sum256(written)
	if digest != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("digest = %q, want SHA-256 of actual bytes %x", digest, wantDigest)
	}
	if len(written) < 2 || written[len(written)-1] != '\n' || strings.Contains(string(written), "\"machine_id\"") {
		t.Fatalf("Agent JSON is not two-space indented with final newline: %q", written)
	}
	loaded, err := store.LoadAgentForm()
	if err != nil {
		t.Fatalf("LoadAgentForm: %v", err)
	}
	if loaded.Database.Password != "" || !loaded.Database.PasswordStored {
		t.Fatalf("loaded form = %#v", loaded)
	}
	backup, err := loadAgentFixture(paths.AgentConfig + ".last-good")
	if err != nil {
		t.Fatalf("last-good is not a strict valid Agent config: %v", err)
	}
	if backup.PGDSN != base.PGDSN {
		t.Fatalf("last-good = %#v, want previous effective config", backup)
	}
}

func TestStoreSaveAgentRejectsFormalTargetChangedImmediatelyAfterReplace(t *testing.T) {
	store, paths := newTestStore(t)
	base := fullyPopulatedAgentConfig()
	writeBytesFixture(t, paths.AgentConfig, mustCanonicalJSON(t, base))
	form, err := AgentToForm(base)
	if err != nil {
		t.Fatal(err)
	}
	store.testHooks.replace = func(tempPath, destination string) error {
		if err := platformAtomicReplace(tempPath, destination); err != nil {
			return err
		}
		if destination == paths.AgentConfig {
			return os.WriteFile(destination, []byte("corrupted"), 0o600)
		}
		return nil
	}

	if _, err := store.SaveAgentForm(form); !errors.Is(err, ErrSaveVerify) {
		t.Fatalf("SaveAgentForm error = %v, want ErrSaveVerify", err)
	}
}

func TestStoreSaveAgentUsesValidLastGoodAsSecretBaseWhenFormalFileIsMissing(t *testing.T) {
	store, paths := newTestStore(t)
	base := fullyPopulatedAgentConfig()
	writeBytesFixture(t, paths.AgentConfig+".last-good", mustCanonicalJSON(t, base))
	form, err := AgentToForm(base)
	if err != nil {
		t.Fatal(err)
	}
	if form.Database.Password != "" || !form.Database.PasswordStored || form.Database.ReplacePassword {
		t.Fatalf("unexpected form secret flags: %#v", form.Database)
	}

	if _, err := store.SaveAgentForm(form); err != nil {
		t.Fatalf("SaveAgentForm: %v", err)
	}
	saved, err := loadAgentFixture(paths.AgentConfig)
	if err != nil {
		t.Fatal(err)
	}
	if saved.PGDSN != base.PGDSN {
		t.Fatalf("saved DSN did not preserve last-good credential state")
	}
}

func TestStorePrepareHelperWriteValidatesCanonicalizesCopiesAndNeverWritesProtectedTarget(t *testing.T) {
	store, paths := newTestStore(t)
	form := HelperToForm(validHelperConfig(t))
	prepared, err := store.PrepareHelperWrite(form)
	if err != nil {
		t.Fatalf("PrepareHelperWrite: %v", err)
	}
	if prepared.TargetPath != paths.HelperConfig {
		t.Fatalf("TargetPath = %q, want configured Helper target", prepared.TargetPath)
	}
	wantDigest := sha256.Sum256(prepared.CanonicalJSON)
	if prepared.SHA256 != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("SHA256 = %q, want %x", prepared.SHA256, wantDigest)
	}
	if len(prepared.CanonicalJSON) < 2 || prepared.CanonicalJSON[len(prepared.CanonicalJSON)-1] != '\n' ||
		!strings.Contains(string(prepared.CanonicalJSON), "\n  \"pipe_name\"") {
		t.Fatalf("Helper JSON is not canonical: %q", prepared.CanonicalJSON)
	}
	if _, err := helper.LoadConfig(writePreparedFixture(t, prepared.CanonicalJSON), paths.HelperExecutable); err != nil {
		t.Fatalf("prepared Helper JSON fails shared strict validation: %v", err)
	}
	if _, err := os.Stat(paths.HelperConfig); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("PrepareHelperWrite touched protected target: %v", err)
	}
	if _, err := os.Stat(paths.HelperConfig + ".last-good"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("PrepareHelperWrite touched protected backup: %v", err)
	}

	prepared.CanonicalJSON[0] = 'X'
	again, err := store.PrepareHelperWrite(form)
	if err != nil {
		t.Fatal(err)
	}
	if again.CanonicalJSON[0] != '{' {
		t.Fatal("PreparedWrite.CanonicalJSON shares mutable state across calls")
	}
}

func TestStorePrepareDefaultHelperWriteIsCreateOnlyAndNeverOverwrites(t *testing.T) {
	store, paths := newTestStore(t)
	defaultPath := filepath.Join(filepath.Dir(paths.HelperExecutable), "helper.default.json")
	writeJSONFixture(t, defaultPath, validHelperConfig(t))
	prepared, err := store.PrepareDefaultHelperWrite()
	if err != nil {
		t.Fatalf("PrepareDefaultHelperWrite: %v", err)
	}
	if !prepared.CreateOnly {
		t.Fatal("default prepared write must be create-only")
	}
	if prepared.TargetPath != paths.HelperConfig {
		t.Fatalf("target = %q", prepared.TargetPath)
	}
	if err := os.MkdirAll(filepath.Dir(paths.HelperConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	writeBytesFixture(t, paths.HelperConfig, []byte("existing"))
	if _, err := store.PrepareDefaultHelperWrite(); !errors.Is(err, ErrHelperConfigExists) {
		t.Fatalf("existing target error = %v", err)
	}
	if got := readFixture(t, paths.HelperConfig); string(got) != "existing" {
		t.Fatal("existing helper was changed")
	}
}

func TestStorePrepareDefaultHelperWriteRejectsInvalidDefault(t *testing.T) {
	store, paths := newTestStore(t)
	defaultPath := filepath.Join(filepath.Dir(paths.HelperExecutable), "helper.default.json")
	writeBytesFixture(t, defaultPath, []byte(`{"allowed_roots":[]} trailing`))
	if _, err := store.PrepareDefaultHelperWrite(); err == nil {
		t.Fatal("invalid default was accepted")
	}
	if _, err := os.Stat(paths.HelperConfig); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target was written: %v", err)
	}
}

func TestStoreFormValidationUsesEditBaseAndSharedValidatorsWithoutWriting(t *testing.T) {
	store, paths := newTestStore(t)
	base := agentconfig.DefaultAgent()
	base.PGDSN = "postgres://user:fixture-password@db.example/media"
	base.DataDir = `D:\nodetray-test-data`
	writeJSONFixture(t, paths.AgentConfig, base)
	before := readFixture(t, paths.AgentConfig)
	form, err := AgentToForm(base)
	if err != nil {
		t.Fatalf("AgentToForm: %v", err)
	}
	form.Worker.ImageMemoryMB = 257
	form.Database.Password = "replacement-secret"
	form.Database.ReplacePassword = true

	agentErrors := store.ValidateAgentForm(form)
	wantAgent := []FieldError{{Field: "agent", Code: "invalid", Message: "Agent 配置无效"}}
	if !reflect.DeepEqual(agentErrors, wantAgent) {
		t.Fatalf("ValidateAgentForm = %#v, want %#v", agentErrors, wantAgent)
	}
	if after := readFixture(t, paths.AgentConfig); !bytes.Equal(after, before) {
		t.Fatal("ValidateAgentForm wrote the Agent configuration")
	}

	helperForm := HelperToForm(validHelperConfig(t))
	helperForm.MaxEntriesPerFrame = 2001
	helperErrors := store.ValidateHelperForm(helperForm)
	wantHelper := []FieldError{{Field: "helper", Code: "invalid", Message: "Helper 配置无效"}}
	if !reflect.DeepEqual(helperErrors, wantHelper) {
		t.Fatalf("ValidateHelperForm = %#v, want %#v", helperErrors, wantHelper)
	}
	if _, err := os.Stat(paths.HelperConfig); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ValidateHelperForm touched protected target: %v", err)
	}
	joined := fmt.Sprint(agentErrors, helperErrors)
	for _, secret := range []string{"fixture-password", "replacement-secret", "postgres://", `D:\nodetray-test-data`} {
		if strings.Contains(joined, secret) {
			t.Fatalf("validation leaked %q in %q", secret, joined)
		}
	}
}

func TestStoreAgentSaveFailureLeavesOfficialUnchangedAndAtLeastOneStrictLastGood(t *testing.T) {
	tests := []struct {
		name  string
		hooks func(target string) storeTestHooks
	}{
		{
			name: "reread validation",
			hooks: func(target string) storeTestHooks {
				return storeTestHooks{afterSync: func(tempPath, destination string) error {
					if destination == target {
						return os.WriteFile(tempPath, []byte(`{"pg_dsn":"failure-secret"}`), 0o600)
					}
					return nil
				}}
			},
		},
		{
			name: "atomic replace",
			hooks: func(target string) storeTestHooks {
				return storeTestHooks{beforeReplace: func(_, destination string) error {
					if destination == target {
						return errors.New("replace-failure-secret")
					}
					return nil
				}}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, paths := newTestStore(t)
			base := fullyPopulatedAgentConfig()
			original := mustCanonicalJSON(t, base)
			writeBytesFixture(t, paths.AgentConfig, original)
			form, err := AgentToForm(base)
			if err != nil {
				t.Fatal(err)
			}
			store.testHooks = tt.hooks(paths.AgentConfig)
			_, err = store.SaveAgentForm(form)
			if err == nil {
				t.Fatal("SaveAgentForm unexpectedly succeeded during injected failure")
			}
			assertErrorRedacted(t, err, "failure-secret", base.PGDSN, filepath.Dir(paths.AgentConfig))
			if got := readFixture(t, paths.AgentConfig); !reflect.DeepEqual(got, original) {
				t.Fatalf("formal Agent config changed after failure\n got: %s\nwant: %s", got, original)
			}
			if _, err := loadAgentFixture(paths.AgentConfig); err != nil {
				t.Fatalf("formal config is invalid after failure: %v", err)
			}
			if _, err := loadAgentFixture(paths.AgentConfig + ".last-good"); err != nil {
				t.Fatalf("last-good is invalid after failure: %v", err)
			}
		})
	}
}

func TestStoreFirstSaveTargetFailureLeavesStrictLastGood(t *testing.T) {
	tests := []struct {
		name  string
		hooks func(target string) storeTestHooks
	}{
		{
			name: "after sync validation failure",
			hooks: func(target string) storeTestHooks {
				return storeTestHooks{afterSync: func(tempPath, destination string) error {
					if destination == target {
						return os.WriteFile(tempPath, []byte(`{"unexpected":"target-after-sync"}`), 0o600)
					}
					return nil
				}}
			},
		},
		{
			name: "before replace failure",
			hooks: func(target string) storeTestHooks {
				return storeTestHooks{beforeReplace: func(_, destination string) error {
					if destination == target {
						return errors.New("target-before-replace")
					}
					return nil
				}}
			},
		},
		{
			name: "replace failure",
			hooks: func(target string) storeTestHooks {
				return storeTestHooks{replace: func(tempPath, destination string) error {
					if destination == target {
						return errors.New("target-replace")
					}
					return platformAtomicReplace(tempPath, destination)
				}}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, paths := newTestStore(t)
			settings := validTraySettings()
			store.testHooks = tt.hooks(paths.TraySettings)
			err := store.SaveTraySettings(settings)
			if err == nil {
				t.Fatal("first SaveTraySettings unexpectedly succeeded")
			}
			if _, statErr := os.Stat(paths.TraySettings); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failed first save published formal target: %v", statErr)
			}
			restored, _, loadErr := loadTraySettings(paths.TraySettings + ".last-good")
			if loadErr != nil {
				t.Fatalf("first-save target failure did not leave strict last-good: %v", loadErr)
			}
			if !reflect.DeepEqual(restored, settings) {
				t.Fatalf("last-good = %#v, want pending first settings %#v", restored, settings)
			}
		})
	}
}

func TestStoreFirstSaveLastGoodFailureDoesNotAttemptTarget(t *testing.T) {
	store, paths := newTestStore(t)
	var targetAttempted atomic.Bool
	store.testHooks = storeTestHooks{
		beforeReplace: func(_, destination string) error {
			switch destination {
			case paths.TraySettings + ".last-good":
				return errors.New("last-good-publish-failure")
			case paths.TraySettings:
				targetAttempted.Store(true)
			}
			return nil
		},
	}
	if err := store.SaveTraySettings(validTraySettings()); err == nil {
		t.Fatal("first SaveTraySettings unexpectedly succeeded")
	}
	if targetAttempted.Load() {
		t.Fatal("Store attempted formal target after last-good publication failed")
	}
	for _, path := range []string{paths.TraySettings, paths.TraySettings + ".last-good"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("last-good failure changed initial no-config state at %s: %v", filepath.Base(path), err)
		}
	}
}

func TestStoreTempIsSameDirectorySyncedClosedAndValidatedBeforeReplace(t *testing.T) {
	store, paths := newTestStore(t)
	base := fullyPopulatedAgentConfig()
	writeBytesFixture(t, paths.AgentConfig, mustCanonicalJSON(t, base))
	form, err := AgentToForm(base)
	if err != nil {
		t.Fatal(err)
	}
	var targetSynced atomic.Bool
	store.testHooks = storeTestHooks{
		afterSync: func(tempPath, destination string) error {
			if destination != paths.AgentConfig {
				return nil
			}
			if filepath.Dir(tempPath) != filepath.Dir(destination) {
				t.Fatalf("temp directory = %q, target directory = %q", filepath.Dir(tempPath), filepath.Dir(destination))
			}
			file, err := os.OpenFile(tempPath, os.O_RDWR, 0)
			if err != nil {
				t.Fatalf("temp file was not closed after sync: %v", err)
			}
			_ = file.Close()
			targetSynced.Store(true)
			return nil
		},
		beforeReplace: func(_, destination string) error {
			if destination == paths.AgentConfig && !targetSynced.Load() {
				t.Fatal("replace reached before target temp sync hook")
			}
			return nil
		},
	}
	if _, err := store.SaveAgentForm(form); err != nil {
		t.Fatalf("SaveAgentForm: %v", err)
	}
	if !targetSynced.Load() {
		t.Fatal("target temp was not synchronized before replace")
	}
}

func TestStoreFileLockSerializesConcurrentSavesAcrossStoreInstances(t *testing.T) {
	first, paths := newTestStore(t)
	second, err := NewStore(paths)
	if err != nil {
		t.Fatal(err)
	}
	base := fullyPopulatedAgentConfig()
	writeBytesFixture(t, paths.AgentConfig, mustCanonicalJSON(t, base))
	firstForm, _ := AgentToForm(base)
	firstForm.DataDir = `D:\first-writer`
	secondForm, _ := AgentToForm(base)
	secondForm.DataDir = `D:\second-writer`

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})
	first.testHooks = storeTestHooks{beforeReplace: func(_, destination string) error {
		if destination == paths.AgentConfig {
			close(firstEntered)
			<-releaseFirst
		}
		return nil
	}}
	second.testHooks = storeTestHooks{beforeReplace: func(_, destination string) error {
		if destination == paths.AgentConfig {
			close(secondEntered)
		}
		return nil
	}}

	firstDone := make(chan error, 1)
	go func() { _, err := first.SaveAgentForm(firstForm); firstDone <- err }()
	select {
	case <-firstEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first Store never reached target replace")
	}
	secondDone := make(chan error, 1)
	go func() { _, err := second.SaveAgentForm(secondForm); secondDone <- err }()
	select {
	case <-secondEntered:
		t.Fatal("second Store entered save while first Store held the file lock")
	case <-time.After(150 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second save: %v", err)
	}
	loaded, err := second.LoadAgentForm()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DataDir != secondForm.DataDir {
		t.Fatalf("final DataDir = %q, want second serialized writer", loaded.DataDir)
	}
}

func TestStoreRestoreBackupsStrictlyValidatesBeforePublishing(t *testing.T) {
	t.Run("Agent", func(t *testing.T) {
		store, paths := newTestStore(t)
		base := fullyPopulatedAgentConfig()
		writeBytesFixture(t, paths.AgentConfig+".last-good", mustCanonicalJSON(t, base))
		writeBytesFixture(t, paths.AgentConfig, []byte(`{"machine_id":"corrupt"}`))
		if err := store.RestoreAgentBackup(); err != nil {
			t.Fatalf("RestoreAgentBackup: %v", err)
		}
		loaded, err := store.LoadAgentForm()
		if err != nil {
			t.Fatal(err)
		}
		if loaded.DataDir != base.DataDir {
			t.Fatalf("restored DataDir = %q, want %q", loaded.DataDir, base.DataDir)
		}

		writeBytesFixture(t, paths.AgentConfig+".last-good", []byte(`{"unexpected":"backup-secret"}`))
		before := readFixture(t, paths.AgentConfig)
		err = store.RestoreAgentBackup()
		if err == nil {
			t.Fatal("RestoreAgentBackup accepted invalid backup")
		}
		assertErrorRedacted(t, err, "backup-secret", filepath.Dir(paths.AgentConfig))
		if got := readFixture(t, paths.AgentConfig); !reflect.DeepEqual(got, before) {
			t.Fatal("invalid Agent backup changed formal config")
		}
	})

	t.Run("HelperPreparedOnly", func(t *testing.T) {
		store, paths := newTestStore(t)
		cfg := validHelperConfig(t)
		writeBytesFixture(t, paths.HelperConfig+".last-good", mustCanonicalJSON(t, cfg))
		prepared, err := store.RestoreHelperBackup()
		if err != nil {
			t.Fatalf("RestoreHelperBackup: %v", err)
		}
		if prepared.TargetPath != paths.HelperConfig || len(prepared.CanonicalJSON) == 0 {
			t.Fatalf("prepared restore = %#v", prepared)
		}
		if _, err := os.Stat(paths.HelperConfig); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("RestoreHelperBackup wrote protected target: %v", err)
		}

		prepared.CanonicalJSON[0] = 'X'
		again, err := store.RestoreHelperBackup()
		if err != nil {
			t.Fatal(err)
		}
		if again.CanonicalJSON[0] != '{' {
			t.Fatal("RestoreHelperBackup returned shared mutable bytes")
		}
	})
}

func newTestStore(t *testing.T) (*Store, Paths) {
	t.Helper()
	root := t.TempDir()
	paths := Paths{
		TraySettings:     filepath.Join(root, "tray", "settings.json"),
		AgentConfig:      filepath.Join(root, "agent", "agent.json"),
		HelperConfig:     filepath.Join(root, "helper", "helper.json"),
		AgentExecutable:  `D:\nodetray-test-binaries\agent.exe`,
		HelperExecutable: filepath.Join(root, "bin", "helper.exe"),
	}
	store, err := NewStore(paths)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	writeJSONFixture(t, filepath.Join(filepath.Dir(paths.HelperExecutable), "helper.default.json"), validHelperConfig(t))
	return store, paths
}

func validTraySettings() traymodel.TraySettings {
	return traymodel.TraySettings{
		LoginStartTray:         true,
		AgentStartMode:         traymodel.StartManual,
		HelperEnabled:          true,
		HelperStartMode:        traymodel.StartAutomatic,
		CloseToTray:            true,
		RefreshIntervalSeconds: 2,
		NotificationLevel:      traymodel.NotifyImportant,
	}
}

func validHelperConfig(t *testing.T) helper.Config {
	t.Helper()
	root := `D:\nodetray-test-media`
	return helper.Config{
		PipeName:             `\\.\pipe\dedup-delete`,
		AllowedRoots:         []string{root},
		DeniedRoots:          []string{filepath.Join(root, "private")},
		DefaultMode:          "soft",
		AllowHardDelete:      false,
		RecycleDirName:       "$DedupRecycle",
		MaxEntriesPerFrame:   2000,
		FrameReadTimeoutSec:  120,
		FrameWriteTimeoutSec: 60,
		LogDir:               `D:\nodetray-test-logs`,
	}
}

func mustCanonicalJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func writeJSONFixture(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeBytesFixture(t, path, data)
}

func writeBytesFixture(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writePreparedFixture(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "helper.json")
	writeBytesFixture(t, path, data)
	return path
}

func loadAgentFixture(path string) (*agentconfig.AgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := agentconfig.DefaultAgent()
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(cfg); err != nil {
		return nil, err
	}
	return agentconfig.ValidateAgent(cfg, filepath.Join(filepath.Dir(path), "agent.exe"), 4)
}

func assertErrorRedacted(t *testing.T, err error, forbidden ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, value := range forbidden {
		if value != "" && strings.Contains(err.Error(), value) {
			t.Fatalf("error leaked forbidden value %q: %v", value, err)
		}
	}
}
