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
	"strings"
	"syscall"
	"time"
	"unicode"

	"dedup/internal/helper"
	trayconfig "dedup/internal/nodetray/config"
	"dedup/internal/nodetray/windows/elevation"
	"dedup/internal/nodetray/windows/task"
	"github.com/vmihailenco/msgpack/v5"
	"golang.org/x/sys/windows"
)

type Executor struct {
	// These exported fields are compatibility snapshots for existing callers.
	// Runtime authority is held only in frozen.
	HelperConfigPath string
	TaskService      task.Service

	TaskDefinition task.Definition
	TaskCapability task.Capability

	frozen    *executorAuthority
	platform  elevatedPlatform
	testHooks elevatedTestHooks
}

type executorAuthority struct {
	helperConfigPath string
	taskService      task.Service
	taskDefinition   task.Definition
	taskCapability   task.Capability
}

type elevatedTestHooks struct {
	beforeLock        func()
	stat              func(string) (os.FileInfo, error)
	beforeBackup      func()
	afterSync         func(tempPath, destination string) error
	beforeReplace     func(tempPath, destination string) error
	replace           func(tempPath, destination string) error
	beforeTaskService func()
}

func (executor *Executor) stat(path string) (os.FileInfo, error) {
	if executor.testHooks.stat != nil {
		return executor.testHooks.stat(path)
	}
	return os.Stat(path)
}

type elevatedPlatform interface {
	EnsureProtectedDirectory(string) error
	AcquireLock(context.Context, string) (io.Closer, error)
	Protect(string) error
	AtomicReplace(source, destination string) error
	SyncDirectory(string) error
}

func NewExecutor(
	helperConfigPath string,
	taskService task.Service,
	definition task.Definition,
	capability task.Capability,
) (*Executor, error) {
	return newExecutorWithPlatform(helperConfigPath, taskService, definition, capability, nativeElevatedPlatform{})
}

func newExecutorWithPlatform(
	helperConfigPath string,
	taskService task.Service,
	definition task.Definition,
	capability task.Capability,
	platform elevatedPlatform,
) (*Executor, error) {
	if platform == nil {
		return nil, errors.New("elevated action executor platform is required")
	}
	configured, err := cleanAbsoluteFilePath(helperConfigPath)
	if err != nil {
		return nil, errors.New("elevated action executor Helper path is invalid")
	}
	fixedPath, err := finalPathForComparison(configured)
	if err != nil {
		return nil, errors.New("elevated action executor Helper final path is invalid")
	}
	return &Executor{
		HelperConfigPath: configured,
		TaskService:      taskService,
		TaskDefinition:   definition,
		TaskCapability:   capability,
		frozen: &executorAuthority{
			helperConfigPath: fixedPath,
			taskService:      taskService,
			taskDefinition:   definition,
			taskCapability:   capability,
		},
		platform: platform,
	}, nil
}

func (executor *Executor) Execute(ctx context.Context, request elevation.Request) elevation.Response {
	response := elevation.Response{Version: elevation.ProtocolVersion, Nonce: safeResponseNonce(request.Nonce)}
	if executor == nil || ctx == nil || executor.frozen == nil || executor.platform == nil {
		return failResponse(response, elevation.ErrorCodeUnavailable, "action service unavailable")
	}
	if err := ctx.Err(); err != nil {
		return failResponse(response, elevation.ErrorCodeTimeout, "operation cancelled")
	}
	if err := elevation.ValidateRequest(request); err != nil {
		return failResponse(response, elevation.ErrorCodeInvalidRequest, "request rejected")
	}
	response.Nonce = request.Nonce
	switch request.Action {
	case elevation.ActionWriteHelperConfig:
		prepared, err := executor.validatePreparedWrite(request.Payload)
		if err != nil {
			return failResponse(response, elevation.ErrorCodeInvalidRequest, "request rejected")
		}
		if err := executor.savePreparedHelper(ctx, prepared); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return failResponse(response, elevation.ErrorCodeTimeout, "operation cancelled")
			}
			if errors.Is(err, trayconfig.ErrSaveVerify) {
				return failResponse(response, elevation.ErrorCodeSaveVerifyFailed, "configuration verification failed")
			}
			if errors.Is(err, trayconfig.ErrHelperConfigExists) {
				return failResponse(response, elevation.ErrorCodeHelperConfigExists, "helper configuration already exists")
			}
			return failResponse(response, elevation.ErrorCodeWriteFailed, "configuration write failed")
		}
	case elevation.ActionInstallHelperTask:
		if !executor.taskActionsAvailable() {
			return failResponse(response, elevation.ErrorCodeUnavailable, "task service unavailable")
		}
		var definition task.Definition
		if err := strictMsgpack(request.Payload, &definition); err != nil || definition != executor.frozen.taskDefinition {
			return failResponse(response, elevation.ErrorCodeInvalidRequest, "request rejected")
		}
		if hook := executor.testHooks.beforeTaskService; hook != nil {
			hook()
		}
		if err := ctx.Err(); err != nil {
			return failResponse(response, elevation.ErrorCodeTimeout, "operation cancelled")
		}
		if err := executor.frozen.taskService.Install(ctx, executor.frozen.taskDefinition); err != nil {
			return failResponse(response, elevation.ErrorCodeTaskFailed, "task operation failed")
		}
	case elevation.ActionRemoveHelperTask:
		if len(request.Payload) != 0 {
			return failResponse(response, elevation.ErrorCodeInvalidRequest, "request rejected")
		}
		if !executor.taskActionsAvailable() {
			return failResponse(response, elevation.ErrorCodeUnavailable, "task service unavailable")
		}
		if hook := executor.testHooks.beforeTaskService; hook != nil {
			hook()
		}
		if err := ctx.Err(); err != nil {
			return failResponse(response, elevation.ErrorCodeTimeout, "operation cancelled")
		}
		if err := executor.frozen.taskService.Remove(ctx); err != nil {
			return failResponse(response, elevation.ErrorCodeTaskFailed, "task operation failed")
		}
	default:
		return failResponse(response, elevation.ErrorCodeInvalidRequest, "request rejected")
	}
	response.OK = true
	return response
}

func failResponse(response elevation.Response, code, summary string) elevation.Response {
	response.OK = false
	response.ErrorCode = code
	response.ErrorSummary = summary
	return response
}

func safeResponseNonce(value string) string {
	if elevation.ValidateNonce(value) == nil {
		return value
	}
	return strings.Repeat("0", 64)
}

func (executor *Executor) taskActionsAvailable() bool {
	return executor.frozen != nil &&
		executor.frozen.taskService != nil &&
		executor.frozen.taskCapability == task.CapabilityElevated &&
		validFrozenDefinition(executor.frozen.taskDefinition) &&
		windowsEqualPath(executor.frozen.taskDefinition.HelperConfig, executor.frozen.helperConfigPath)
}

func validFrozenDefinition(definition task.Definition) bool {
	if definition.UserSID == "" || strings.TrimSpace(definition.UserSID) != definition.UserSID ||
		strings.ContainsFunc(definition.UserSID, unicode.IsControl) || !strings.HasPrefix(definition.UserSID, "S-1-") {
		return false
	}
	executable, err := cleanAbsoluteFilePath(definition.HelperExecutable)
	if err != nil || !strings.EqualFold(filepath.Base(executable), "helper.exe") {
		return false
	}
	_, err = cleanAbsoluteFilePath(definition.HelperConfig)
	return err == nil
}

func (executor *Executor) validatePreparedWrite(payload []byte) (trayconfig.PreparedWrite, error) {
	var prepared trayconfig.PreparedWrite
	if err := strictMsgpack(payload, &prepared); err != nil {
		return trayconfig.PreparedWrite{}, err
	}
	configured, err := finalPathForComparison(executor.frozen.helperConfigPath)
	if err != nil {
		return trayconfig.PreparedWrite{}, err
	}
	target, err := finalPathForComparison(prepared.TargetPath)
	if err != nil || !strings.EqualFold(configured, target) {
		return trayconfig.PreparedWrite{}, errors.New("prepared target mismatch")
	}
	if len(prepared.SHA256) != sha256.Size*2 || strings.ToLower(prepared.SHA256) != prepared.SHA256 {
		return trayconfig.PreparedWrite{}, errors.New("prepared digest is invalid")
	}
	digestBytes, err := hex.DecodeString(prepared.SHA256)
	if err != nil || len(digestBytes) != sha256.Size {
		return trayconfig.PreparedWrite{}, errors.New("prepared digest is invalid")
	}
	digest := sha256.Sum256(prepared.CanonicalJSON)
	if !bytes.Equal(digest[:], digestBytes) {
		return trayconfig.PreparedWrite{}, errors.New("prepared digest mismatch")
	}
	canonical, err := validateHelperBytes(prepared.CanonicalJSON, executor.helperExecutable())
	if err != nil || !bytes.Equal(canonical, prepared.CanonicalJSON) {
		return trayconfig.PreparedWrite{}, errors.New("prepared Helper JSON is not canonical")
	}
	prepared.TargetPath = executor.frozen.helperConfigPath
	prepared.CanonicalJSON = append([]byte(nil), prepared.CanonicalJSON...)
	return prepared, nil
}

func (executor *Executor) helperExecutable() string {
	if executor.frozen != nil && validFrozenDefinition(executor.frozen.taskDefinition) &&
		windowsEqualPath(executor.frozen.taskDefinition.HelperConfig, executor.frozen.helperConfigPath) {
		return executor.frozen.taskDefinition.HelperExecutable
	}
	if executor.frozen == nil {
		return ""
	}
	return filepath.Join(filepath.Dir(executor.frozen.helperConfigPath), "helper.exe")
}

func strictMsgpack(payload []byte, destination any) error {
	if len(payload) == 0 || len(payload) > elevation.MaxPayloadSize {
		return errors.New("invalid action payload")
	}
	reader := bytes.NewReader(payload)
	decoder := msgpack.NewDecoder(reader)
	decoder.DisallowUnknownFields(true)
	if err := decoder.Decode(destination); err != nil {
		return errors.New("strict action payload decode failed")
	}
	if reader.Len() != 0 {
		return errors.New("trailing action payload")
	}
	return nil
}

func validateHelperBytes(data []byte, executable string) ([]byte, error) {
	var config helper.Config
	reader := bytes.NewReader(data)
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing Helper JSON")
	}
	normalized, err := helper.ValidateConfig(config, executable)
	if err != nil {
		return nil, err
	}
	canonical, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(canonical, '\n'), nil
}

func (executor *Executor) savePreparedHelper(ctx context.Context, prepared trayconfig.PreparedWrite) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := executor.platform.EnsureProtectedDirectory(filepath.Dir(executor.frozen.helperConfigPath)); err != nil {
		return err
	}
	if hook := executor.testHooks.beforeLock; hook != nil {
		hook()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	lock, err := executor.platform.AcquireLock(ctx, executor.frozen.helperConfigPath+".lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	return executor.saveLocked(ctx, prepared.CanonicalJSON, prepared.CreateOnly)
}

func (executor *Executor) saveLocked(ctx context.Context, data []byte, createOnly bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target := executor.frozen.helperConfigPath
	backup := target + ".last-good"
	if createOnly {
		for _, path := range []string{target, backup} {
			if _, err := executor.stat(path); err == nil {
				return trayconfig.ErrHelperConfigExists
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	_, targetStatErr := executor.stat(target)
	switch {
	case targetStatErr == nil:
		oldData, err := executor.loadCanonicalHelperFile(target)
		if err == nil {
			if hook := executor.testHooks.beforeBackup; hook != nil {
				hook()
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := executor.writeAtomic(ctx, backup, oldData); err != nil {
				return err
			}
		} else if _, backupErr := executor.loadCanonicalHelperFile(backup); backupErr != nil {
			return err
		}
	case errors.Is(targetStatErr, os.ErrNotExist):
		_, backupStatErr := executor.stat(backup)
		switch {
		case backupStatErr == nil:
			if _, err := executor.loadCanonicalHelperFile(backup); err != nil {
				return err
			}
		case errors.Is(backupStatErr, os.ErrNotExist):
			if hook := executor.testHooks.beforeBackup; hook != nil {
				hook()
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := executor.writeAtomic(ctx, backup, data); err != nil {
				return err
			}
		default:
			return backupStatErr
		}
	default:
		return targetStatErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return executor.writeAtomic(ctx, target, data)
}

func (executor *Executor) loadCanonicalHelperFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return validateHelperBytes(data, executor.helperExecutable())
}

func (executor *Executor) writeAtomic(ctx context.Context, destination string, data []byte) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	directory := filepath.Dir(destination)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(destination)+".tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	if err := executor.platform.Protect(temporaryPath); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if hook := executor.testHooks.afterSync; hook != nil {
		if err := hook(temporaryPath, destination); err != nil {
			return err
		}
	}
	validated, err := executor.loadCanonicalHelperFile(temporaryPath)
	if err != nil || !bytes.Equal(validated, data) {
		if err != nil {
			return err
		}
		return errors.New("Helper canonical reread mismatch")
	}
	if hook := executor.testHooks.beforeReplace; hook != nil {
		if err := hook(temporaryPath, destination); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	replace := executor.platform.AtomicReplace
	if hook := executor.testHooks.replace; hook != nil {
		replace = hook
	}
	if err := replace(temporaryPath, destination); err != nil {
		return err
	}
	if err := executor.platform.Protect(destination); err != nil {
		return err
	}
	formal, err := executor.loadCanonicalHelperFile(destination)
	if err != nil {
		return trayconfig.ErrSaveVerify
	}
	if !bytes.Equal(formal, data) {
		return trayconfig.ErrSaveVerify
	}
	return executor.platform.SyncDirectory(directory)
}

func cleanAbsoluteFilePath(value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsFunc(value, unicode.IsControl) ||
		strings.ContainsRune(value, '"') || !filepath.IsAbs(value) {
		return "", errors.New("invalid absolute file path")
	}
	cleaned := filepath.Clean(value)
	if filepath.Base(cleaned) == "." || cleaned == filepath.VolumeName(cleaned)+string(filepath.Separator) {
		return "", errors.New("invalid absolute file path")
	}
	return cleaned, nil
}

func finalPathForComparison(value string) (string, error) {
	cleaned, err := cleanAbsoluteFilePath(value)
	if err != nil {
		return "", err
	}
	remaining := make([]string, 0, 4)
	cursor := cleaned
	for {
		file, openErr := os.Open(cursor)
		if openErr == nil {
			info, statErr := file.Stat()
			resolved, finalErr := finalPathFromOpenFile(file)
			closeErr := file.Close()
			if statErr != nil {
				return "", statErr
			}
			if finalErr != nil {
				return "", finalErr
			}
			if closeErr != nil {
				return "", closeErr
			}
			if len(remaining) != 0 && !info.IsDir() {
				return "", errors.New("existing path ancestor is not a directory")
			}
			for index := len(remaining) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, remaining[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(openErr, os.ErrNotExist) {
			return "", openErr
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			return "", openErr
		}
		remaining = append(remaining, filepath.Base(cursor))
		cursor = parent
	}
}

func finalPathFromOpenFile(file *os.File) (string, error) {
	if file == nil {
		return "", errors.New("final path file is unavailable")
	}
	size := uint32(512)
	for {
		buffer := make([]uint16, size)
		length, err := windows.GetFinalPathNameByHandle(windows.Handle(file.Fd()), &buffer[0], size, 0)
		if err != nil {
			return "", err
		}
		if length < size {
			resolved := windows.UTF16ToString(buffer[:length])
			if strings.HasPrefix(resolved, `\\?\UNC\`) {
				resolved = `\\` + strings.TrimPrefix(resolved, `\\?\UNC\`)
			} else {
				resolved = strings.TrimPrefix(resolved, `\\?\`)
			}
			return filepath.Clean(resolved), nil
		}
		size = length + 1
	}
}

func windowsEqualPath(left, right string) bool {
	leftFinal, leftErr := finalPathForComparison(left)
	rightFinal, rightErr := finalPathForComparison(right)
	return leftErr == nil && rightErr == nil && strings.EqualFold(leftFinal, rightFinal)
}

type nativeElevatedPlatform struct{}

func (nativeElevatedPlatform) EnsureProtectedDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return protectNamedObject(path)
}

type nativeLock struct {
	handle windows.Handle
}

func (lock *nativeLock) Close() error {
	if lock == nil || lock.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(lock.handle)
	lock.handle = 0
	return err
}

func (nativeElevatedPlatform) AcquireLock(ctx context.Context, path string) (io.Closer, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		handle, openErr := windows.CreateFile(
			name,
			windows.GENERIC_READ|windows.GENERIC_WRITE|windows.WRITE_DAC|windows.WRITE_OWNER,
			0,
			nil,
			windows.OPEN_ALWAYS,
			windows.FILE_ATTRIBUTE_NORMAL,
			0,
		)
		if openErr == nil {
			if err := protectHandle(handle); err != nil {
				windows.CloseHandle(handle)
				return nil, err
			}
			return &nativeLock{handle: handle}, nil
		}
		if !errors.Is(openErr, windows.ERROR_SHARING_VIOLATION) && !errors.Is(openErr, windows.ERROR_LOCK_VIOLATION) {
			return nil, openErr
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (nativeElevatedPlatform) Protect(path string) error {
	return protectNamedObject(path)
}

func (nativeElevatedPlatform) AtomicReplace(source, destination string) error {
	source16, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destination16, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		err = windows.MoveFileEx(source16, destination16, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
		if err == nil || attempt == 99 || !replaceTransient(err) {
			return err
		}
		time.Sleep(time.Millisecond)
	}
}

func replaceTransient(err error) bool {
	return errors.Is(err, syscall.Errno(5)) || errors.Is(err, syscall.Errno(32)) || errors.Is(err, syscall.Errno(33))
}

func (nativeElevatedPlatform) SyncDirectory(string) error { return nil }

func protectedSecurityDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return nil, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return windows.SecurityDescriptorFromString(
		"O:BAG:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;GRGX;;;" + user.User.Sid.String() + ")",
	)
}

func protectNamedObject(path string) error {
	descriptor, err := protectedSecurityDescriptor()
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner,
		nil,
		dacl,
		nil,
	)
}

func protectHandle(handle windows.Handle) error {
	descriptor, err := protectedSecurityDescriptor()
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner,
		nil,
		dacl,
		nil,
	)
}
