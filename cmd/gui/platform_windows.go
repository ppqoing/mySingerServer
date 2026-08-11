//go:build windows

package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"dedup/internal/shared/finalpath"
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

func showGUIStartupError(message string) {
	user32 := syscall.NewLazyDLL("user32.dll")
	proc := user32.NewProc("MessageBoxW")
	title, _ := syscall.UTF16PtrFromString("媒体去重管理器")
	text, _ := syscall.UTF16PtrFromString(message)
	proc.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), 0x10)
}
