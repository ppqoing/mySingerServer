//go:build windows

package task

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	coInitMultithreaded       = 0
	clsctxInprocServer        = 1
	variantBSTRType           = 8
	taskCreateOrUpdate        = 6
	taskLogonInteractiveToken = 3
	taskStateRunning          = 4

	hresultAccessDenied   = 0x80070005
	hresultFileNotFound   = 0x80070002
	hresultPathNotFound   = 0x80070003
	hresultTaskNotRunning = 0x8004130B

	iTaskServiceGetFolder = 7
	iTaskServiceConnect   = 10

	iTaskFolderCreateFolder = 11
	iTaskFolderGetTask      = 13
	iTaskFolderDeleteTask   = 15
	iTaskFolderRegisterTask = 16

	iRegisteredTaskGetState      = 9
	iRegisteredTaskRun           = 12
	iRegisteredTaskGetLastResult = 16
	iRegisteredTaskStop          = 23
)

var (
	ole32DLL             = windows.NewLazySystemDLL("ole32.dll")
	oleAut32DLL          = windows.NewLazySystemDLL("oleaut32.dll")
	procCoInitializeEx   = ole32DLL.NewProc("CoInitializeEx")
	procCoUninitialize   = ole32DLL.NewProc("CoUninitialize")
	procCoCreateInstance = ole32DLL.NewProc("CoCreateInstance")
	procSysAllocString   = oleAut32DLL.NewProc("SysAllocString")
	procSysFreeString    = oleAut32DLL.NewProc("SysFreeString")

	clsidTaskScheduler = windows.GUID{
		Data1: 0x0F87369F,
		Data2: 0xA4E5,
		Data3: 0x4CFC,
		Data4: [8]byte{0xBD, 0x3E, 0x73, 0xE6, 0x15, 0x45, 0x72, 0xDD},
	}
	iidTaskService = windows.GUID{
		Data1: 0x2FABA4C7,
		Data2: 0x4DA9,
		Data3: 0x4013,
		Data4: [8]byte{0x96, 0x97, 0x20, 0xCC, 0x3F, 0xD4, 0x0F, 0x85},
	}
)

type comSchedulerBackend struct{}

type comObject struct {
	vtable *[64]uintptr
}

type variant struct {
	Type     uint16
	Reserved [3]uint16
	Value    [2]uintptr
}

type schedulerSession struct {
	service      *comObject
	uninitialize bool
	lockedThread bool
}

func newPlatformSchedulerBackend() (schedulerBackend, error) {
	return comSchedulerBackend{}, nil
}

func platformResolveFinalHelper(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("task: helper executable is unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("task: helper executable is not a regular file")
	}
	resolved, err := finalDOSPath(windows.Handle(file.Fd()))
	if err != nil {
		return "", errors.New("task: resolve helper executable failed")
	}
	if strings.HasPrefix(resolved, `\\?\UNC\`) {
		resolved = `\\` + strings.TrimPrefix(resolved, `\\?\UNC\`)
	} else {
		resolved = strings.TrimPrefix(resolved, `\\?\`)
	}
	return filepath.Clean(resolved), nil
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

func (comSchedulerBackend) Inspect(ctx context.Context, path string) (Status, error) {
	if err := requireFixedPath(ctx, path); err != nil {
		return Status{}, err
	}
	session, err := openSchedulerSession(ctx)
	if err != nil {
		return Status{}, err
	}
	defer session.Close()
	folder, err := session.getFolder(fixedFolderPath)
	if err != nil {
		return Status{}, err
	}
	defer folder.Release()
	registered, err := getRegisteredTask(folder, fixedTaskName)
	if err != nil {
		return Status{}, err
	}
	defer registered.Release()
	var state int32
	if err := callCOM(registered, iRegisteredTaskGetState, uintptr(unsafe.Pointer(&state))); err != nil {
		return Status{}, err
	}
	var lastResult int32
	if err := callCOM(registered, iRegisteredTaskGetLastResult, uintptr(unsafe.Pointer(&lastResult))); err != nil {
		return Status{}, err
	}
	if err := contextError(ctx); err != nil {
		return Status{}, err
	}
	return Status{Installed: true, Running: state == taskStateRunning, LastResult: uint32(lastResult)}, nil
}

func (comSchedulerBackend) Register(ctx context.Context, path string, registration taskRegistration) error {
	if err := requireFixedPath(ctx, path); err != nil {
		return err
	}
	if registration.Path != TaskPath {
		return ErrBackend
	}
	xmlText, err := renderTaskXML(registration)
	if err != nil {
		return ErrBackend
	}
	session, err := openSchedulerSession(ctx)
	if err != nil {
		return err
	}
	defer session.Close()
	folder, err := session.ensureFixedFolder(ctx)
	if err != nil {
		return err
	}
	defer folder.Release()
	name, freeName, err := allocateBSTR(fixedTaskName)
	if err != nil {
		return err
	}
	defer freeName()
	xmlValue, freeXML, err := allocateBSTR(xmlText)
	if err != nil {
		return err
	}
	defer freeXML()
	sid, freeSID, err := allocateBSTR(registration.Principal.UserSID)
	if err != nil {
		return err
	}
	defer freeSID()
	user := variant{Type: variantBSTRType, Value: [2]uintptr{sid}}
	password := variant{}
	sddl := variant{}
	var registered *comObject
	err = callCOM(folder, iTaskFolderRegisterTask,
		name,
		xmlValue,
		taskCreateOrUpdate,
		uintptr(unsafe.Pointer(&user)),
		uintptr(unsafe.Pointer(&password)),
		taskLogonInteractiveToken,
		uintptr(unsafe.Pointer(&sddl)),
		uintptr(unsafe.Pointer(&registered)),
	)
	if registered != nil {
		registered.Release()
	}
	if err != nil {
		return err
	}
	return contextError(ctx)
}

func (comSchedulerBackend) Run(ctx context.Context, path string) error {
	return withRegisteredTask(ctx, path, func(registered *comObject) error {
		parameters := variant{}
		var running *comObject
		err := callCOM(registered, iRegisteredTaskRun,
			uintptr(unsafe.Pointer(&parameters)),
			uintptr(unsafe.Pointer(&running)),
		)
		if running != nil {
			running.Release()
		}
		return err
	})
}

func (comSchedulerBackend) Stop(ctx context.Context, path string) error {
	return withRegisteredTask(ctx, path, func(registered *comObject) error {
		return callCOM(registered, iRegisteredTaskStop, 0)
	})
}

func (comSchedulerBackend) Delete(ctx context.Context, path string) error {
	if err := requireFixedPath(ctx, path); err != nil {
		return err
	}
	session, err := openSchedulerSession(ctx)
	if err != nil {
		return err
	}
	defer session.Close()
	folder, err := session.getFolder(fixedFolderPath)
	if err != nil {
		return err
	}
	defer folder.Release()
	name, freeName, err := allocateBSTR(fixedTaskName)
	if err != nil {
		return err
	}
	defer freeName()
	if err := callCOM(folder, iTaskFolderDeleteTask, name, 0); err != nil {
		return err
	}
	return contextError(ctx)
}

const (
	fixedFolderPath = `\MySingerServer`
	fixedFolderName = "MySingerServer"
	fixedTaskName   = "DeleteHelper"
)

func requireFixedPath(ctx context.Context, path string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if path != TaskPath {
		return ErrBackend
	}
	return nil
}

func withRegisteredTask(ctx context.Context, path string, operation func(*comObject) error) error {
	if err := requireFixedPath(ctx, path); err != nil {
		return err
	}
	session, err := openSchedulerSession(ctx)
	if err != nil {
		return err
	}
	defer session.Close()
	folder, err := session.getFolder(fixedFolderPath)
	if err != nil {
		return err
	}
	defer folder.Release()
	registered, err := getRegisteredTask(folder, fixedTaskName)
	if err != nil {
		return err
	}
	defer registered.Release()
	if err := operation(registered); err != nil {
		return err
	}
	return contextError(ctx)
}

func openSchedulerSession(ctx context.Context) (*schedulerSession, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	runtime.LockOSThread()
	hr, _, _ := procCoInitializeEx.Call(0, coInitMultithreaded)
	if err := mapHRESULT(uint32(hr)); err != nil {
		runtime.UnlockOSThread()
		return nil, err
	}
	session := &schedulerSession{uninitialize: true, lockedThread: true}
	hr, _, _ = procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidTaskScheduler)),
		0,
		clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidTaskService)),
		uintptr(unsafe.Pointer(&session.service)),
	)
	if err := mapHRESULT(uint32(hr)); err != nil {
		session.Close()
		return nil, err
	}
	if session.service == nil {
		session.Close()
		return nil, ErrBackend
	}
	empty := variant{}
	if err := callCOM(session.service, iTaskServiceConnect,
		uintptr(unsafe.Pointer(&empty)),
		uintptr(unsafe.Pointer(&empty)),
		uintptr(unsafe.Pointer(&empty)),
		uintptr(unsafe.Pointer(&empty)),
	); err != nil {
		session.Close()
		return nil, err
	}
	if err := contextError(ctx); err != nil {
		session.Close()
		return nil, err
	}
	return session, nil
}

func (s *schedulerSession) getFolder(path string) (*comObject, error) {
	value, freeValue, err := allocateBSTR(path)
	if err != nil {
		return nil, err
	}
	defer freeValue()
	var folder *comObject
	if err := callCOM(s.service, iTaskServiceGetFolder, value, uintptr(unsafe.Pointer(&folder))); err != nil {
		return nil, err
	}
	if folder == nil {
		return nil, ErrBackend
	}
	return folder, nil
}

func (s *schedulerSession) ensureFixedFolder(ctx context.Context) (*comObject, error) {
	folder, err := s.getFolder(fixedFolderPath)
	if err == nil {
		return folder, nil
	}
	if !errors.Is(err, ErrTaskNotInstalled) {
		return nil, err
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	root, err := s.getFolder(`\`)
	if err != nil {
		return nil, err
	}
	defer root.Release()
	name, freeName, err := allocateBSTR(fixedFolderName)
	if err != nil {
		return nil, err
	}
	defer freeName()
	empty := variant{}
	var created *comObject
	if err := callCOM(root, iTaskFolderCreateFolder,
		name,
		uintptr(unsafe.Pointer(&empty)),
		uintptr(unsafe.Pointer(&created)),
	); err != nil {
		if existing, retryErr := s.getFolder(fixedFolderPath); retryErr == nil {
			return existing, nil
		}
		return nil, err
	}
	if created == nil {
		return nil, ErrBackend
	}
	return created, nil
}

func (s *schedulerSession) Close() {
	if s.service != nil {
		s.service.Release()
		s.service = nil
	}
	if s.uninitialize {
		procCoUninitialize.Call()
		s.uninitialize = false
	}
	if s.lockedThread {
		runtime.UnlockOSThread()
		s.lockedThread = false
	}
}

func getRegisteredTask(folder *comObject, name string) (*comObject, error) {
	value, freeValue, err := allocateBSTR(name)
	if err != nil {
		return nil, err
	}
	defer freeValue()
	var registered *comObject
	if err := callCOM(folder, iTaskFolderGetTask, value, uintptr(unsafe.Pointer(&registered))); err != nil {
		return nil, err
	}
	if registered == nil {
		return nil, ErrBackend
	}
	return registered, nil
}

func allocateBSTR(value string) (uintptr, func(), error) {
	encoded, err := windows.UTF16PtrFromString(value)
	if err != nil {
		return 0, func() {}, ErrBackend
	}
	result, _, _ := procSysAllocString.Call(uintptr(unsafe.Pointer(encoded)))
	if result == 0 && value != "" {
		return 0, func() {}, ErrBackend
	}
	return result, func() {
		if result != 0 {
			procSysFreeString.Call(result)
		}
	}, nil
}

func callCOM(object *comObject, method int, arguments ...uintptr) error {
	if object == nil || object.vtable == nil || method < 0 || method >= len(object.vtable) || object.vtable[method] == 0 {
		return ErrBackend
	}
	values := make([]uintptr, 1, len(arguments)+1)
	values[0] = uintptr(unsafe.Pointer(object))
	values = append(values, arguments...)
	result, _, _ := syscall.SyscallN(object.vtable[method], values...)
	return mapHRESULT(uint32(result))
}

func (object *comObject) Release() {
	if object == nil || object.vtable == nil || object.vtable[2] == 0 {
		return
	}
	syscall.SyscallN(object.vtable[2], uintptr(unsafe.Pointer(object)))
}

func mapHRESULT(value uint32) error {
	if int32(value) >= 0 {
		return nil
	}
	switch value {
	case hresultAccessDenied:
		return ErrAccessDenied
	case hresultFileNotFound, hresultPathNotFound:
		return ErrTaskNotInstalled
	case hresultTaskNotRunning:
		return ErrTaskNotRunning
	default:
		return ErrBackend
	}
}

var _ schedulerBackend = comSchedulerBackend{}
