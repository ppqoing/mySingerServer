//go:build windows

package enum

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	everythingErrorIPC = 2

	everythingRequestFullPath     = 0x00000004
	everythingRequestSize         = 0x00000010
	everythingRequestDateModified = 0x00000040
)

var (
	ErrIPC        = errors.New("everything: IPC unavailable (Everything not running?)")
	ErrEmptyIndex = errors.New("everything: index is empty or not ready")
	errProbeDone  = errors.New("everything: availability probe complete")
)

// EverythingEnumerator loads Everything64.dll only when first used. This
// preserves the architecture-plan v1.2 on-demand DLL boundary and avoids
// making process startup depend on the SDK being present.
type EverythingEnumerator struct {
	dllPath string

	mu    sync.Mutex
	dll   *windows.DLL
	procs everythingProcs
}

type everythingProcs struct {
	getLastError          *windows.Proc
	setSearch             *windows.Proc
	setMatchPath          *windows.Proc
	query                 *windows.Proc
	getNumResults         *windows.Proc
	getResultFullPathName *windows.Proc
	setRequestFlags       *windows.Proc
	getResultSize         *windows.Proc
	getResultDateModified *windows.Proc
	isFolderResult        *windows.Proc
	getMajorVersion       *windows.Proc
}

func NewEverythingEnumerator() *EverythingEnumerator {
	executable, err := os.Executable()
	if err != nil {
		return NewEverythingEnumeratorAt("Everything64.dll")
	}
	return NewEverythingEnumeratorAt(filepath.Join(filepath.Dir(executable), "Everything64.dll"))
}

func NewEverythingEnumeratorAt(dllPath string) *EverythingEnumerator {
	return &EverythingEnumerator{dllPath: dllPath}
}

func (e *EverythingEnumerator) Name() string { return "everything" }

func (e *EverythingEnumerator) Available() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.loadLocked(); err != nil {
		return err
	}
	err := e.queryLocked("", func(FileRecord) error {
		return errProbeDone
	})
	if errors.Is(err, errProbeDone) {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrEmptyIndex
}

func (e *EverythingEnumerator) Enum(root string, visit func(FileRecord) error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.loadLocked(); err != nil {
		return err
	}
	canonicalRoot, err := canonicalSearchRoot(root)
	if err != nil {
		return err
	}
	return e.queryLocked(canonicalRoot, func(record FileRecord) error {
		if !pathWithinRoot(record.Path, canonicalRoot) {
			return nil
		}
		return visit(record)
	})
}

func (e *EverythingEnumerator) loadLocked() error {
	if e.dll != nil {
		return nil
	}
	dll, err := windows.LoadDLL(e.dllPath)
	if err != nil {
		return fmt.Errorf("everything: load %s: %w", e.dllPath, err)
	}
	find := func(name string) (*windows.Proc, error) {
		proc, findErr := dll.FindProc(name)
		if findErr != nil {
			return nil, fmt.Errorf("everything: find %s: %w", name, findErr)
		}
		return proc, nil
	}

	var procs everythingProcs
	if procs.getLastError, err = find("Everything_GetLastError"); err != nil {
		_ = dll.Release()
		return err
	}
	if procs.setSearch, err = find("Everything_SetSearchW"); err != nil {
		_ = dll.Release()
		return err
	}
	if procs.setMatchPath, err = find("Everything_SetMatchPath"); err != nil {
		_ = dll.Release()
		return err
	}
	if procs.query, err = find("Everything_QueryW"); err != nil {
		_ = dll.Release()
		return err
	}
	if procs.getNumResults, err = find("Everything_GetNumResults"); err != nil {
		_ = dll.Release()
		return err
	}
	if procs.getResultFullPathName, err = find("Everything_GetResultFullPathNameW"); err != nil {
		_ = dll.Release()
		return err
	}
	if procs.setRequestFlags, err = find("Everything_SetRequestFlags"); err != nil {
		_ = dll.Release()
		return err
	}
	if procs.getResultSize, err = find("Everything_GetResultSize"); err != nil {
		_ = dll.Release()
		return err
	}
	if procs.getResultDateModified, err = find("Everything_GetResultDateModified"); err != nil {
		_ = dll.Release()
		return err
	}
	if procs.isFolderResult, err = find("Everything_IsFolderResult"); err != nil {
		_ = dll.Release()
		return err
	}
	if procs.getMajorVersion, err = find("Everything_GetMajorVersion"); err != nil {
		_ = dll.Release()
		return err
	}
	e.dll = dll
	e.procs = procs
	return nil
}

func (e *EverythingEnumerator) queryLocked(
	root string,
	visit func(FileRecord) error,
) error {
	searchText := root
	matchPath := uintptr(0)
	if strings.ContainsAny(root, `\/`) {
		// Everything search terms are space-delimited. Quoting keeps a path
		// containing spaces as one literal full-path term.
		searchText = `"` + root + `"`
		matchPath = 1
	}
	search, err := windows.UTF16PtrFromString(searchText)
	if err != nil {
		return fmt.Errorf("everything: bad root %q: %w", root, err)
	}
	e.procs.setSearch.Call(uintptr(unsafe.Pointer(search)))
	e.procs.setMatchPath.Call(matchPath)
	e.procs.setRequestFlags.Call(
		everythingRequestFullPath |
			everythingRequestSize |
			everythingRequestDateModified,
	)
	ok, _, _ := e.procs.query.Call(1)
	if ok == 0 {
		code, _, _ := e.procs.getLastError.Call()
		if code == everythingErrorIPC {
			return ErrIPC
		}
		return fmt.Errorf("everything: QueryW failed, lastError=%d", code)
	}

	count, _, _ := e.procs.getNumResults.Call()
	pathBuffer := make([]uint16, 32768)
	for index := uintptr(0); index < count; index++ {
		isFolder, _, _ := e.procs.isFolderResult.Call(index)
		if isFolder != 0 {
			continue
		}
		length, _, _ := e.procs.getResultFullPathName.Call(
			index,
			uintptr(unsafe.Pointer(&pathBuffer[0])),
			uintptr(len(pathBuffer)),
		)
		if length == 0 || length >= uintptr(len(pathBuffer)) {
			continue
		}
		size := int64(-1)
		var rawSize int64
		sizeOK, _, _ := e.procs.getResultSize.Call(
			index,
			uintptr(unsafe.Pointer(&rawSize)),
		)
		if sizeOK != 0 {
			size = rawSize
		}
		var modified windows.Filetime
		mtimeOK, _, _ := e.procs.getResultDateModified.Call(
			index,
			uintptr(unsafe.Pointer(&modified)),
		)
		var modifiedUnix int64
		if mtimeOK != 0 {
			raw := uint64(modified.HighDateTime)<<32 | uint64(modified.LowDateTime)
			modifiedUnix = int64(raw/10_000_000) - 11_644_473_600
		}
		if err := visit(FileRecord{
			Path:  cleanPath(windows.UTF16ToString(pathBuffer[:length])),
			Size:  size,
			MTime: modifiedUnix,
		}); err != nil {
			return err
		}
	}
	return nil
}

func canonicalSearchRoot(root string) (string, error) {
	path, err := canonicalExistingPath(root)
	if err != nil {
		return "", fmt.Errorf("everything: canonical root: %w", err)
	}
	return path, nil
}
