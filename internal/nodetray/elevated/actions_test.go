//go:build windows

package elevated

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"dedup/internal/helper"
	trayconfig "dedup/internal/nodetray/config"
	"dedup/internal/nodetray/windows/elevation"
	"dedup/internal/nodetray/windows/task"
	"github.com/vmihailenco/msgpack/v5"
	"golang.org/x/sys/windows"
)

const executorNonce = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func TestExecutorWritesStrictPreparedHelperConfigWithOneLastGood(t *testing.T) {
	executor, platform, target := newTestExecutor(t)
	first := validPreparedWrite(t, target, 120)
	response := executor.Execute(context.Background(), executorRequest(t, elevation.ActionWriteHelperConfig, first))
	assertExecutorSuccess(t, response)
	assertStrictHelperFile(t, target)
	if got := readTestFile(t, target+".last-good"); !reflect.DeepEqual(got, first.CanonicalJSON) {
		t.Fatalf("first last-good mismatch\n got: %s\nwant: %s", got, first.CanonicalJSON)
	}

	second := validPreparedWrite(t, strings.ToUpper(target), 121)
	response = executor.Execute(context.Background(), executorRequest(t, elevation.ActionWriteHelperConfig, second))
	assertExecutorSuccess(t, response)
	if got := readTestFile(t, target); !reflect.DeepEqual(got, second.CanonicalJSON) {
		t.Fatalf("formal Helper config mismatch\n got: %s\nwant: %s", got, second.CanonicalJSON)
	}
	if got := readTestFile(t, target+".last-good"); !reflect.DeepEqual(got, first.CanonicalJSON) {
		t.Fatalf("last-good did not retain previous version\n got: %s\nwant: %s", got, first.CanonicalJSON)
	}
	matches, err := filepath.Glob(target + ".last-good*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0] != target+".last-good" {
		t.Fatalf("last-good files = %#v", matches)
	}
	for _, path := range []string{target, target + ".last-good", target + ".lock"} {
		if !platform.wasProtected(path) {
			t.Fatalf("protected ACL was not applied to %s", filepath.Base(path))
		}
	}
}

func TestExecutorRejectsFormalHelperTargetChangedImmediatelyAfterReplace(t *testing.T) {
	executor, platform, target := newTestExecutor(t)
	executor.testHooks.replace = func(source, destination string) error {
		if err := platform.AtomicReplace(source, destination); err != nil {
			return err
		}
		if destination == target {
			return os.WriteFile(destination, []byte("corrupted"), 0o600)
		}
		return nil
	}

	response := executor.Execute(context.Background(), executorRequest(t, elevation.ActionWriteHelperConfig, validPreparedWrite(t, target, 120)))

	assertExecutorFailure(t, response, elevation.ErrorCodeSaveVerifyFailed)
}

func TestExecutorRejectsPreparedWriteDriftUnknownAndInvalidHelperWithoutLeaks(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, prepared trayconfig.PreparedWrite) []byte
	}{
		{
			name: "target drift",
			mutate: func(t *testing.T, prepared trayconfig.PreparedWrite) []byte {
				prepared.TargetPath = filepath.Join(t.TempDir(), "other-helper.json")
				return mustMsgpack(t, prepared)
			},
		},
		{
			name: "sha drift",
			mutate: func(t *testing.T, prepared trayconfig.PreparedWrite) []byte {
				prepared.SHA256 = strings.Repeat("0", 64)
				return mustMsgpack(t, prepared)
			},
		},
		{
			name: "non canonical JSON",
			mutate: func(t *testing.T, prepared trayconfig.PreparedWrite) []byte {
				var value helper.Config
				if err := json.Unmarshal(prepared.CanonicalJSON, &value); err != nil {
					t.Fatal(err)
				}
				prepared.CanonicalJSON, _ = json.Marshal(value)
				prepared.SHA256 = digestHex(prepared.CanonicalJSON)
				return mustMsgpack(t, prepared)
			},
		},
		{
			name: "unknown Helper JSON field",
			mutate: func(t *testing.T, prepared trayconfig.PreparedWrite) []byte {
				var object map[string]any
				if err := json.Unmarshal(prepared.CanonicalJSON, &object); err != nil {
					t.Fatal(err)
				}
				object["unexpected"] = "do-not-leak-secret"
				prepared.CanonicalJSON, _ = json.MarshalIndent(object, "", "  ")
				prepared.CanonicalJSON = append(prepared.CanonicalJSON, '\n')
				prepared.SHA256 = digestHex(prepared.CanonicalJSON)
				return mustMsgpack(t, prepared)
			},
		},
		{
			name: "invalid Helper config",
			mutate: func(t *testing.T, prepared trayconfig.PreparedWrite) []byte {
				var value helper.Config
				if err := json.Unmarshal(prepared.CanonicalJSON, &value); err != nil {
					t.Fatal(err)
				}
				value.PipeName = "not-a-pipe"
				prepared.CanonicalJSON = canonicalJSON(t, value)
				prepared.SHA256 = digestHex(prepared.CanonicalJSON)
				return mustMsgpack(t, prepared)
			},
		},
		{
			name: "unknown PreparedWrite field",
			mutate: func(t *testing.T, prepared trayconfig.PreparedWrite) []byte {
				body := mustMsgpack(t, prepared)
				var object map[string]any
				if err := msgpack.Unmarshal(body, &object); err != nil {
					t.Fatal(err)
				}
				object["unexpected"] = "do-not-leak-secret"
				return mustMsgpack(t, object)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor, _, target := newTestExecutor(t)
			prepared := validPreparedWrite(t, target, 120)
			payload := test.mutate(t, prepared)
			response := executor.Execute(context.Background(), elevation.Request{
				Version: elevation.ProtocolVersion,
				Nonce:   executorNonce,
				Action:  elevation.ActionWriteHelperConfig,
				Payload: payload,
			})
			assertExecutorFailure(t, response, elevation.ErrorCodeInvalidRequest)
			for _, leaked := range []string{filepath.Dir(target), "do-not-leak-secret", "not-a-pipe"} {
				if strings.Contains(response.ErrorSummary, leaked) {
					t.Fatalf("response leaked %q: %#v", leaked, response)
				}
			}
			if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid request touched formal target: %v", err)
			}
		})
	}
}

func TestExecutorHelperWriteFailuresKeepAValidFormalOrLastGood(t *testing.T) {
	tests := []struct {
		name  string
		hooks func(target string) elevatedTestHooks
	}{
		{
			name: "reread validation",
			hooks: func(target string) elevatedTestHooks {
				return elevatedTestHooks{afterSync: func(temp, destination string) error {
					if destination == target {
						return os.WriteFile(temp, []byte(`{"unexpected":"fault-secret"}`), 0o600)
					}
					return nil
				}}
			},
		},
		{
			name: "before replace",
			hooks: func(target string) elevatedTestHooks {
				return elevatedTestHooks{beforeReplace: func(_, destination string) error {
					if destination == target {
						return errors.New("fault-secret")
					}
					return nil
				}}
			},
		},
		{
			name: "replace",
			hooks: func(target string) elevatedTestHooks {
				return elevatedTestHooks{replace: func(_, destination string) error {
					if destination == target {
						return errors.New("fault-secret")
					}
					return nil
				}}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor, _, target := newTestExecutor(t)
			first := validPreparedWrite(t, target, 120)
			assertExecutorSuccess(t, executor.Execute(context.Background(), executorRequest(t, elevation.ActionWriteHelperConfig, first)))
			executor.testHooks = test.hooks(target)
			second := validPreparedWrite(t, target, 121)
			response := executor.Execute(context.Background(), executorRequest(t, elevation.ActionWriteHelperConfig, second))
			assertExecutorFailure(t, response, elevation.ErrorCodeWriteFailed)
			if strings.Contains(response.ErrorSummary, "fault-secret") || strings.Contains(response.ErrorSummary, filepath.Dir(target)) {
				t.Fatalf("failure response leaked details: %#v", response)
			}
			if _, err := loadStrictHelper(target); err != nil {
				t.Fatalf("formal config invalid after failure: %v", err)
			}
			if _, err := loadStrictHelper(target + ".last-good"); err != nil {
				t.Fatalf("last-good invalid after failure: %v", err)
			}
		})
	}
}

func TestExecutorFirstHelperWriteTargetFailureLeavesStrictLastGood(t *testing.T) {
	executor, platform, target := newTestExecutor(t)
	executor.testHooks = elevatedTestHooks{beforeReplace: func(_, destination string) error {
		if destination == target {
			return errors.New("first-target-failure-secret")
		}
		return nil
	}}
	prepared := validPreparedWrite(t, target, 120)
	response := executor.Execute(context.Background(), executorRequest(t, elevation.ActionWriteHelperConfig, prepared))
	assertExecutorFailure(t, response, elevation.ErrorCodeWriteFailed)
	if strings.Contains(response.ErrorSummary, "first-target-failure-secret") || strings.Contains(response.ErrorSummary, filepath.Dir(target)) {
		t.Fatalf("failure response leaked details: %#v", response)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed first write published formal target: %v", err)
	}
	if _, err := loadStrictHelper(target + ".last-good"); err != nil {
		t.Fatalf("failed first write did not leave strict last-good: %v", err)
	}
	if !reflect.DeepEqual(readTestFile(t, target+".last-good"), prepared.CanonicalJSON) {
		t.Fatal("first-write last-good does not contain the validated pending version")
	}
	if !platform.wasProtected(target + ".last-good") {
		t.Fatal("first-write last-good was not protected")
	}
}

func TestExecutorFirstLastGoodFailureDoesNotAttemptFormalTarget(t *testing.T) {
	executor, _, target := newTestExecutor(t)
	var targetAttempted atomic.Bool
	executor.testHooks = elevatedTestHooks{beforeReplace: func(_, destination string) error {
		switch destination {
		case target + ".last-good":
			return errors.New("first-backup-failure-secret")
		case target:
			targetAttempted.Store(true)
		}
		return nil
	}}
	prepared := validPreparedWrite(t, target, 120)
	response := executor.Execute(context.Background(), executorRequest(t, elevation.ActionWriteHelperConfig, prepared))
	assertExecutorFailure(t, response, elevation.ErrorCodeWriteFailed)
	if targetAttempted.Load() {
		t.Fatal("formal target was attempted after first last-good publication failed")
	}
	for _, path := range []string{target, target + ".last-good"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("first last-good failure changed the no-config state at %s: %v", filepath.Base(path), err)
		}
	}
}

func TestExecutorRechecksCancellationBeforeLockBackupReplaceAndTaskService(t *testing.T) {
	t.Run("before lock", func(t *testing.T) {
		executor, _, target := newTestExecutor(t)
		prepared := validPreparedWrite(t, target, 120)
		ctx, cancel := context.WithCancel(context.Background())
		executor.testHooks.beforeLock = cancel
		response := executor.Execute(ctx, executorRequest(t, elevation.ActionWriteHelperConfig, prepared))
		assertExecutorFailure(t, response, elevation.ErrorCodeTimeout)
		if _, err := os.Stat(target + ".lock"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cancelled write acquired lock: %v", err)
		}
	})

	t.Run("before backup", func(t *testing.T) {
		executor, _, target := newTestExecutor(t)
		first := validPreparedWrite(t, target, 120)
		assertExecutorSuccess(t, executor.Execute(context.Background(), executorRequest(t, elevation.ActionWriteHelperConfig, first)))
		formalBefore := readTestFile(t, target)
		backupBefore := readTestFile(t, target+".last-good")
		ctx, cancel := context.WithCancel(context.Background())
		executor.testHooks.beforeBackup = cancel
		second := validPreparedWrite(t, target, 121)
		response := executor.Execute(ctx, executorRequest(t, elevation.ActionWriteHelperConfig, second))
		assertExecutorFailure(t, response, elevation.ErrorCodeTimeout)
		if !bytes.Equal(readTestFile(t, target), formalBefore) || !bytes.Equal(readTestFile(t, target+".last-good"), backupBefore) {
			t.Fatal("cancelled write changed formal or last-good before backup boundary")
		}
	})

	t.Run("before replace", func(t *testing.T) {
		executor, _, target := newTestExecutor(t)
		prepared := validPreparedWrite(t, target, 120)
		ctx, cancel := context.WithCancel(context.Background())
		executor.testHooks.beforeReplace = func(_, destination string) error {
			if destination == target {
				cancel()
			}
			return nil
		}
		response := executor.Execute(ctx, executorRequest(t, elevation.ActionWriteHelperConfig, prepared))
		assertExecutorFailure(t, response, elevation.ErrorCodeTimeout)
		if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cancelled write committed formal target: %v", err)
		}
	})

	for _, action := range []elevation.Action{elevation.ActionInstallHelperTask, elevation.ActionRemoveHelperTask} {
		t.Run("before task service "+string(action), func(t *testing.T) {
			executor, _, _ := newTestExecutor(t)
			service := executor.TaskService.(*fakeTaskService)
			ctx, cancel := context.WithCancel(context.Background())
			executor.testHooks.beforeTaskService = cancel
			var payload any
			if action == elevation.ActionInstallHelperTask {
				payload = executor.TaskDefinition
			}
			response := executor.Execute(ctx, executorRequest(t, action, payload))
			assertExecutorFailure(t, response, elevation.ErrorCodeTimeout)
			if service.installCalls != 0 || service.removeCalls != 0 {
				t.Fatalf("cancelled action reached task service: install=%d remove=%d", service.installCalls, service.removeCalls)
			}
		})
	}
}

func TestExecutorTaskActionsUseOnlyFrozenDefinitionAndEmptyRemovePayload(t *testing.T) {
	executor, _, _ := newTestExecutor(t)
	service := executor.TaskService.(*fakeTaskService)
	definition := executor.TaskDefinition

	response := executor.Execute(context.Background(), executorRequest(t, elevation.ActionInstallHelperTask, definition))
	assertExecutorSuccess(t, response)
	if service.installCalls != 1 || service.lastDefinition != definition {
		t.Fatalf("Install calls=%d definition=%#v", service.installCalls, service.lastDefinition)
	}

	for name, payload := range map[string][]byte{
		"field drift": mustMsgpack(t, task.Definition{
			HelperExecutable: definition.HelperExecutable,
			HelperConfig:     definition.HelperConfig,
			UserSID:          "S-1-5-21-1-2-3-9999",
		}),
		"unknown field": withUnknownMsgpackField(t, definition),
	} {
		t.Run(name, func(t *testing.T) {
			response := executor.Execute(context.Background(), elevation.Request{
				Version: elevation.ProtocolVersion, Nonce: executorNonce,
				Action: elevation.ActionInstallHelperTask, Payload: payload,
			})
			assertExecutorFailure(t, response, elevation.ErrorCodeInvalidRequest)
		})
	}
	if service.installCalls != 1 {
		t.Fatalf("invalid installs reached service: %d", service.installCalls)
	}

	assertExecutorSuccess(t, executor.Execute(context.Background(), elevation.Request{
		Version: elevation.ProtocolVersion, Nonce: executorNonce,
		Action: elevation.ActionRemoveHelperTask,
	}))
	if service.removeCalls != 1 {
		t.Fatalf("Remove calls=%d", service.removeCalls)
	}
	response = executor.Execute(context.Background(), elevation.Request{
		Version: elevation.ProtocolVersion, Nonce: executorNonce,
		Action: elevation.ActionRemoveHelperTask, Payload: []byte{0x80},
	})
	assertExecutorFailure(t, response, elevation.ErrorCodeInvalidRequest)
	if service.removeCalls != 1 {
		t.Fatalf("non-empty Remove reached service: %d", service.removeCalls)
	}
}

func TestExecutorPublicCompatibilityFieldsCannotReplaceFrozenAuthority(t *testing.T) {
	executor, _, target := newTestExecutor(t)
	originalService := executor.TaskService.(*fakeTaskService)
	originalDefinition := executor.TaskDefinition

	alternateRoot := filepath.Join(t.TempDir(), "alternate-valid")
	alternateTarget := filepath.Join(alternateRoot, "config", "helper.json")
	alternateService := &fakeTaskService{}
	alternateDefinition := task.Definition{
		HelperExecutable: filepath.Join(alternateRoot, "bin", "helper.exe"),
		HelperConfig:     alternateTarget,
		UserSID:          "S-1-5-21-900-901-902-1001",
	}
	executor.HelperConfigPath = alternateTarget
	executor.TaskService = alternateService
	executor.TaskDefinition = alternateDefinition
	executor.TaskCapability = task.CapabilityUser

	prepared := validPreparedWrite(t, target, 120)
	assertExecutorSuccess(t, executor.Execute(context.Background(), executorRequest(t, elevation.ActionWriteHelperConfig, prepared)))
	assertStrictHelperFile(t, target)
	if _, err := os.Stat(alternateTarget); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mutated public path became write authority: %v", err)
	}

	assertExecutorSuccess(t, executor.Execute(context.Background(), executorRequest(t, elevation.ActionInstallHelperTask, originalDefinition)))
	assertExecutorSuccess(t, executor.Execute(context.Background(), executorRequest(t, elevation.ActionRemoveHelperTask, nil)))
	if originalService.installCalls != 1 || originalService.removeCalls != 1 || originalService.lastDefinition != originalDefinition {
		t.Fatalf("frozen service calls install=%d remove=%d definition=%#v", originalService.installCalls, originalService.removeCalls, originalService.lastDefinition)
	}
	if alternateService.installCalls != 0 || alternateService.removeCalls != 0 {
		t.Fatalf("mutated public service became authority: install=%d remove=%d", alternateService.installCalls, alternateService.removeCalls)
	}
}

func TestExecutorLiteralWithoutFrozenAuthorityFailsClosed(t *testing.T) {
	_, platform, target := newTestExecutor(t)
	definition := task.Definition{
		HelperExecutable: filepath.Join(filepath.Dir(target), "helper.exe"),
		HelperConfig:     target,
		UserSID:          "S-1-5-21-100-200-300-1001",
	}
	literal := &Executor{
		HelperConfigPath: target,
		TaskService:      &fakeTaskService{},
		TaskDefinition:   definition,
		TaskCapability:   task.CapabilityElevated,
		platform:         platform,
	}
	prepared := validPreparedWrite(t, target, 120)
	response := literal.Execute(context.Background(), executorRequest(t, elevation.ActionWriteHelperConfig, prepared))
	assertExecutorFailure(t, response, elevation.ErrorCodeUnavailable)
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("literal executor wrote without frozen authority: %v", err)
	}
}

func TestExecutorTaskActionsFailClosedWithoutElevatedFrozenCapability(t *testing.T) {
	executor, platform, target := newTestExecutor(t)
	validDefinition := executor.TaskDefinition
	for _, frozen := range []struct {
		service    task.Service
		definition task.Definition
		capability task.Capability
	}{
		{service: nil, definition: validDefinition, capability: task.CapabilityElevated},
		{service: &fakeTaskService{}, definition: task.Definition{}, capability: task.CapabilityElevated},
		{service: &fakeTaskService{}, definition: validDefinition, capability: task.CapabilityUser},
	} {
		value, err := newExecutorWithPlatform(target, frozen.service, frozen.definition, frozen.capability, platform)
		if err != nil {
			t.Fatalf("newExecutorWithPlatform: %v", err)
		}
		response := value.Execute(context.Background(), executorRequest(t, elevation.ActionInstallHelperTask, validDefinition))
		assertExecutorFailure(t, response, elevation.ErrorCodeUnavailable)
	}
}

func TestExecutorProtectedDescriptorUsesTrustedOwnerAndNoOrdinaryMutationACE(t *testing.T) {
	descriptor, err := protectedSecurityDescriptor()
	if err != nil {
		t.Fatalf("protectedSecurityDescriptor: %v", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		t.Fatal(err)
	}
	if owner == nil || !owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
		t.Fatalf("protected owner = %v, want Administrators", owner)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("protected descriptor inherits its DACL")
	}
	sddl := descriptor.String()
	for _, wanted := range []string{"O:BA", ";;;SY)", ";;;BA)"} {
		if !strings.Contains(sddl, wanted) {
			t.Fatalf("descriptor missing %q: %s", wanted, sddl)
		}
	}
	for _, forbidden := range []string{";;;WD)", ";;;BU)", ";;;AU)", ";;;IU)", ";;;NU)"} {
		if strings.Contains(sddl, forbidden) {
			t.Fatalf("descriptor grants forbidden trustee %q: %s", forbidden, sddl)
		}
	}
}

func newTestExecutor(t *testing.T) (*Executor, *fakeElevatedPlatform, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "guid-0f53725e-7fb5-43c2-bdb8-f2f214259218")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	root = canonicalTestDirectory(t, root)
	target := filepath.Join(root, "config", "helper.json")
	definition := task.Definition{
		HelperExecutable: filepath.Join(root, "bin", "helper.exe"),
		HelperConfig:     target,
		UserSID:          "S-1-5-21-100-200-300-1001",
	}
	service := &fakeTaskService{}
	platform := &fakeElevatedPlatform{protected: make(map[string]bool)}
	executor, err := newExecutorWithPlatform(target, service, definition, task.CapabilityElevated, platform)
	if err != nil {
		t.Fatalf("newExecutorWithPlatform: %v", err)
	}
	return executor, platform, target
}

func canonicalTestDirectory(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	resolved, resolveErr := finalPathFromOpenFile(file)
	closeErr := file.Close()
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	return resolved
}

func validPreparedWrite(t *testing.T, target string, readTimeout int) trayconfig.PreparedWrite {
	t.Helper()
	root := filepath.Dir(filepath.Dir(target))
	config := helper.Config{
		PipeName:             `\\.\pipe\dedup-delete`,
		AllowedRoots:         []string{filepath.Join(root, "media")},
		DeniedRoots:          []string{filepath.Join(root, "media", "private")},
		DefaultMode:          "soft",
		AllowHardDelete:      false,
		RecycleDirName:       "$DedupRecycle",
		MaxEntriesPerFrame:   2000,
		FrameReadTimeoutSec:  readTimeout,
		FrameWriteTimeoutSec: 60,
		LogDir:               filepath.Join(root, "logs"),
	}
	normalized, err := helper.ValidateConfig(config, filepath.Join(filepath.Dir(target), "helper.exe"))
	if err != nil {
		t.Fatalf("ValidateConfig fixture: %v", err)
	}
	data := canonicalJSON(t, normalized)
	return trayconfig.PreparedWrite{TargetPath: target, CanonicalJSON: data, SHA256: digestHex(data)}
}

func executorRequest(t *testing.T, action elevation.Action, payload any) elevation.Request {
	t.Helper()
	var body []byte
	if payload != nil {
		body = mustMsgpack(t, payload)
	}
	return elevation.Request{Version: elevation.ProtocolVersion, Nonce: executorNonce, Action: action, Payload: body}
}

func canonicalJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func digestHex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func mustMsgpack(t *testing.T, value any) []byte {
	t.Helper()
	data, err := msgpack.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func withUnknownMsgpackField(t *testing.T, value any) []byte {
	t.Helper()
	body := mustMsgpack(t, value)
	var object map[string]any
	if err := msgpack.Unmarshal(body, &object); err != nil {
		t.Fatal(err)
	}
	object["unexpected"] = "value"
	return mustMsgpack(t, object)
}

func assertExecutorSuccess(t *testing.T, response elevation.Response) {
	t.Helper()
	if !response.OK || response.Version != elevation.ProtocolVersion || response.Nonce != executorNonce ||
		response.ErrorCode != "" || response.ErrorSummary != "" {
		t.Fatalf("unexpected success response: %#v", response)
	}
}

func assertExecutorFailure(t *testing.T, response elevation.Response, code string) {
	t.Helper()
	if response.OK || response.Version != elevation.ProtocolVersion || response.Nonce != executorNonce ||
		response.ErrorCode != code || response.ErrorSummary == "" {
		t.Fatalf("unexpected failure response: %#v", response)
	}
	if err := elevation.ValidateResponse(executorNonce, response); err != nil {
		t.Fatalf("failure response violates message contract: %v", err)
	}
}

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Base(path), err)
	}
	return data
}

func loadStrictHelper(path string) (helper.Config, error) {
	return helper.LoadConfig(path, filepath.Join(filepath.Dir(path), "helper.exe"))
}

func assertStrictHelperFile(t *testing.T, path string) {
	t.Helper()
	if _, err := loadStrictHelper(path); err != nil {
		t.Fatalf("strict Helper load %s: %v", filepath.Base(path), err)
	}
}

type fakeTaskService struct {
	installCalls   int
	removeCalls    int
	lastDefinition task.Definition
}

func (*fakeTaskService) Inspect(context.Context) (task.Status, error) { return task.Status{}, nil }
func (service *fakeTaskService) Install(_ context.Context, definition task.Definition) error {
	service.installCalls++
	service.lastDefinition = definition
	return nil
}
func (service *fakeTaskService) Remove(context.Context) error { service.removeCalls++; return nil }
func (*fakeTaskService) Run(context.Context) error            { return errors.New("unexpected Run") }
func (*fakeTaskService) Stop(context.Context) error           { return errors.New("unexpected Stop") }

type fakeElevatedPlatform struct {
	mu        sync.Mutex
	protected map[string]bool
}

func (platform *fakeElevatedPlatform) EnsureProtectedDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return platform.Protect(path)
}

func (platform *fakeElevatedPlatform) AcquireLock(_ context.Context, path string) (io.Closer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := platform.Protect(path); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func (platform *fakeElevatedPlatform) Protect(path string) error {
	platform.mu.Lock()
	defer platform.mu.Unlock()
	platform.protected[filepath.Clean(path)] = true
	return nil
}

func (platform *fakeElevatedPlatform) AtomicReplace(source, destination string) error {
	if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(source, destination)
}

func (*fakeElevatedPlatform) SyncDirectory(string) error { return nil }

func (platform *fakeElevatedPlatform) wasProtected(path string) bool {
	platform.mu.Lock()
	defer platform.mu.Unlock()
	return platform.protected[filepath.Clean(path)]
}
