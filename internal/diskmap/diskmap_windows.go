//go:build windows

package diskmap

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	ioctlStorageGetDeviceNumber      = 0x002D1080
	ioctlStorageQueryProperty        = 0x002D1400
	storageDeviceSeekPenaltyProperty = 7
	propertyStandardQuery            = 0
)

type Info struct {
	MountPoint      string
	VolumeGUID      string
	DeviceType      uint32
	DeviceNumber    uint32
	PartitionNumber uint32
	IsSSD           bool
	MediaTypeKnown  bool
}

var (
	kernel32                              = windows.NewLazySystemDLL("kernel32.dll")
	procGetVolumePathNameW                = kernel32.NewProc("GetVolumePathNameW")
	procGetVolumeNameForVolumeMountPointW = kernel32.NewProc(
		"GetVolumeNameForVolumeMountPointW",
	)
)

func MountPointOf(path string) (string, error) {
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	buffer := make([]uint16, 1024)
	ok, _, callErr := procGetVolumePathNameW.Call(
		uintptr(unsafe.Pointer(pathPointer)),
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(len(buffer)),
	)
	if ok == 0 {
		return "", fmt.Errorf("diskmap: GetVolumePathNameW(%s): %w", path, callErr)
	}
	return windows.UTF16ToString(buffer), nil
}

func Resolve(mountPoint string) (*Info, error) {
	if !strings.HasSuffix(mountPoint, `\`) {
		mountPoint += `\`
	}
	mountPointer, err := windows.UTF16PtrFromString(mountPoint)
	if err != nil {
		return nil, err
	}
	guidBuffer := make([]uint16, 128)
	ok, _, callErr := procGetVolumeNameForVolumeMountPointW.Call(
		uintptr(unsafe.Pointer(mountPointer)),
		uintptr(unsafe.Pointer(&guidBuffer[0])),
		uintptr(len(guidBuffer)),
	)
	if ok == 0 {
		return nil, fmt.Errorf(
			"diskmap: GetVolumeNameForVolumeMountPointW(%s): %w",
			mountPoint,
			callErr,
		)
	}
	guid := windows.UTF16ToString(guidBuffer)
	openPath, err := windows.UTF16PtrFromString(strings.TrimSuffix(guid, `\`))
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		openPath,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("diskmap: open volume %s: %w", guid, err)
	}
	defer windows.CloseHandle(handle)

	var deviceNumber [12]byte
	var returned uint32
	if err := windows.DeviceIoControl(
		handle,
		ioctlStorageGetDeviceNumber,
		nil,
		0,
		&deviceNumber[0],
		uint32(len(deviceNumber)),
		&returned,
		nil,
	); err != nil {
		return nil, fmt.Errorf("diskmap: IOCTL_STORAGE_GET_DEVICE_NUMBER: %w", err)
	}
	info := &Info{
		MountPoint:      mountPoint,
		VolumeGUID:      guid,
		DeviceType:      binary.LittleEndian.Uint32(deviceNumber[0:4]),
		DeviceNumber:    binary.LittleEndian.Uint32(deviceNumber[4:8]),
		PartitionNumber: binary.LittleEndian.Uint32(deviceNumber[8:12]),
	}

	var query [12]byte
	binary.LittleEndian.PutUint32(query[0:4], storageDeviceSeekPenaltyProperty)
	binary.LittleEndian.PutUint32(query[4:8], propertyStandardQuery)
	var descriptor [12]byte
	queryErr := windows.DeviceIoControl(
		handle,
		ioctlStorageQueryProperty,
		&query[0],
		uint32(len(query)),
		&descriptor[0],
		uint32(len(descriptor)),
		&returned,
		nil,
	)
	info.IsSSD, info.MediaTypeKnown = interpretSeekPenalty(descriptor, queryErr)
	return info, nil
}

func interpretSeekPenalty(descriptor [12]byte, err error) (isSSD, known bool) {
	if err != nil {
		// The documented conservative fallback is HDD.
		return false, false
	}
	return descriptor[8] == 0, true
}
