//go:build windows

package stats

import (
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	psapiDLL                  = windows.NewLazySystemDLL("psapi.dll")
	procGetProcessMemoryInfo  = psapiDLL.NewProc("GetProcessMemoryInfo")
	kernel32StatsDLL          = windows.NewLazySystemDLL("kernel32.dll")
	procGetProcessHandleCount = kernel32StatsDLL.NewProc("GetProcessHandleCount")
)

type processMemoryCounters struct {
	Size                       uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

func newProcessSampler() func(time.Time) processSample {
	var mu sync.Mutex
	var lastWall time.Time
	var lastCPU uint64
	return func(now time.Time) processSample {
		mu.Lock()
		defer mu.Unlock()
		handle := windows.CurrentProcess()
		var creation, exit, kernel, user windows.Filetime
		_ = windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user)
		cpuTicks := filetimeTicks(kernel) + filetimeTicks(user)
		cpu := 0.0
		if !lastWall.IsZero() && now.After(lastWall) && cpuTicks >= lastCPU {
			cpuSeconds := float64(cpuTicks-lastCPU) / 10_000_000
			cpu = cpuSeconds / now.Sub(lastWall).Seconds() * 100
			maxCPU := float64(runtime.NumCPU() * 100)
			if cpu > maxCPU {
				cpu = maxCPU
			}
		}
		lastWall, lastCPU = now, cpuTicks

		var memory processMemoryCounters
		memory.Size = uint32(unsafe.Sizeof(memory))
		_, _, _ = procGetProcessMemoryInfo.Call(
			uintptr(handle),
			uintptr(unsafe.Pointer(&memory)),
			uintptr(memory.Size),
		)
		var handles uint32
		_, _, _ = procGetProcessHandleCount.Call(
			uintptr(handle),
			uintptr(unsafe.Pointer(&handles)),
		)
		return processSample{
			CPU: cpu, RSSBytes: uint64(memory.WorkingSetSize), Handles: uint64(handles),
		}
	}
}

func filetimeTicks(value windows.Filetime) uint64 {
	return uint64(value.HighDateTime)<<32 | uint64(value.LowDateTime)
}
