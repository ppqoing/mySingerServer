//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"

	"dedup/internal/shared/finalpath"
	"golang.org/x/sys/windows"
)

type guiReplacementProcess interface {
	Release() error
}

var (
	guiOpenProcess         = windows.OpenProcess
	guiWaitForSingleObject = windows.WaitForSingleObject
	guiCloseHandle         = windows.CloseHandle
	guiStartProcess        = func(executable string, args []string) (guiReplacementProcess, error) {
		command := exec.Command(executable, args...)
		if err := command.Start(); err != nil {
			return nil, err
		}
		return command.Process, nil
	}
)

func finalGUIExecutablePath() (string, error) {
	return resolveGUIExecutablePath(os.Executable, finalpath.ResolveExisting)
}

func openGUIBrowser(rawURL string) error {
	shell32 := syscall.NewLazyDLL("shell32.dll")
	proc := shell32.NewProc("ShellExecuteW")
	verb, _ := syscall.UTF16PtrFromString("open")
	url, _ := syscall.UTF16PtrFromString(rawURL)
	result, _, callErr := proc.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(url)), 0, 0, 1)
	if result <= 32 {
		return fmt.Errorf("open browser: %w", callErr)
	}
	return nil
}

func guiWaitForParent(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid parent PID %d", pid)
	}
	handle, err := guiOpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		if err == windows.ERROR_INVALID_PARAMETER {
			return nil
		}
		return fmt.Errorf("open parent process %d: %w", pid, err)
	}
	defer guiCloseHandle(handle)
	status, err := guiWaitForSingleObject(handle, windows.INFINITE)
	if err != nil {
		return fmt.Errorf("wait for parent process %d: %w", pid, err)
	}
	if status != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("wait for parent process %d returned status %#x", pid, status)
	}
	return nil
}

func guiStartReplacement(executable string, args []string) error {
	if !filepath.IsAbs(executable) {
		return fmt.Errorf("replacement executable is not absolute: %s", executable)
	}
	process, err := guiStartProcess(executable, args)
	if err != nil {
		return fmt.Errorf("start replacement GUI: %w", err)
	}
	if err := process.Release(); err != nil {
		return fmt.Errorf("release replacement GUI process: %w", err)
	}
	return nil
}

func showGUIStartupError(message string) {
	user32 := syscall.NewLazyDLL("user32.dll")
	proc := user32.NewProc("MessageBoxW")
	title, _ := syscall.UTF16PtrFromString("媒体去重管理器")
	text, _ := syscall.UTF16PtrFromString(message)
	proc.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), 0x10)
}
