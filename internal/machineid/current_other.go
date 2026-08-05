//go:build !windows

package machineid

import "errors"

// Current is unsupported because this project's identity contract uses WMI
// and the 64-bit Windows MachineGuid registry value.
func Current() (Result, error) {
	return Result{}, errors.New("machine identity unavailable: unsupported platform")
}
