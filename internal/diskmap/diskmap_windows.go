//go:build windows

package diskmap

import (
	"dedup/internal/diskio"
	"encoding/binary"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	ioctlStorageGetDeviceNumber      = 0x002D1080
	ioctlStorageQueryProperty        = 0x002D1400
	ioctlVolumeGetVolumeDiskExtents  = 0x00560000
	storageDeviceSeekPenaltyProperty = 7
	propertyStandardQuery            = 0
)

const (
	volumeDiskExtentsHeaderSize = 8
	diskExtentSize              = 24
	maxVolumeDiskExtents        = 128
)

type Info struct {
	MountPoint      string
	VolumeGUID      string
	DeviceType      uint32
	DeviceNumber    uint32
	PartitionNumber uint32
	IsSSD           bool
	MediaTypeKnown  bool
	Identity        diskio.Identity
}

var (
	kernel32                              = windows.NewLazySystemDLL("kernel32.dll")
	procGetVolumePathNameW                = kernel32.NewProc("GetVolumePathNameW")
	procGetVolumeNameForVolumeMountPointW = kernel32.NewProc(
		"GetVolumeNameForVolumeMountPointW",
	)
	deviceIoControl = windows.DeviceIoControl
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
	if network, ok := networkIdentity(mountPoint); ok {
		return &Info{MountPoint: mountPoint, Identity: network}, nil
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
	if err := deviceIoControl(
		handle,
		ioctlStorageGetDeviceNumber,
		nil,
		0,
		&deviceNumber[0],
		uint32(len(deviceNumber)),
		&returned,
		nil,
	); err != nil {
		// The extent identity below remains usable when a legacy device number
		// query is unavailable.
		returned = 0
	}
	info := &Info{
		MountPoint: mountPoint,
		VolumeGUID: guid,
		Identity:   resolveIdentity(handle, guid),
	}
	if returned == uint32(len(deviceNumber)) {
		info.DeviceType = binary.LittleEndian.Uint32(deviceNumber[0:4])
		info.DeviceNumber = binary.LittleEndian.Uint32(deviceNumber[4:8])
		info.PartitionNumber = binary.LittleEndian.Uint32(deviceNumber[8:12])
	}

	var query [12]byte
	binary.LittleEndian.PutUint32(query[0:4], storageDeviceSeekPenaltyProperty)
	binary.LittleEndian.PutUint32(query[4:8], propertyStandardQuery)
	var descriptor [12]byte
	queryErr := deviceIoControl(
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
	info.Identity.SSD = info.IsSSD
	info.Identity.KnownSSD = info.MediaTypeKnown
	return info, nil
}

func resolveIdentity(handle windows.Handle, volumeGUID string) diskio.Identity {
	identity := diskio.Identity{
		Key:    diskio.DiskKey("volume:" + strings.TrimRight(volumeGUID, `\`)),
		Local:  true,
		Volume: volumeGUID,
	}
	buffer := make([]byte, volumeDiskExtentsHeaderSize+diskExtentSize*maxVolumeDiskExtents)
	var returned uint32
	if err := deviceIoControl(
		handle,
		ioctlVolumeGetVolumeDiskExtents,
		nil,
		0,
		&buffer[0],
		uint32(len(buffer)),
		&returned,
		nil,
	); err != nil || returned < volumeDiskExtentsHeaderSize {
		return identity
	}
	count := binary.LittleEndian.Uint32(buffer[:4])
	if count == 0 || count > maxVolumeDiskExtents || uint64(volumeDiskExtentsHeaderSize)+uint64(count)*diskExtentSize > uint64(returned) {
		return identity
	}
	diskNos := make([]uint32, 0, count)
	for index := uint32(0); index < count; index++ {
		offset := volumeDiskExtentsHeaderSize + int(index)*diskExtentSize
		diskNos = append(diskNos, binary.LittleEndian.Uint32(buffer[offset+16:offset+20]))
	}
	sort.Slice(diskNos, func(i, j int) bool { return diskNos[i] < diskNos[j] })
	diskNos = uniqueDiskNumbers(diskNos)
	identity.DiskNos = diskNos
	if len(diskNos) == 1 {
		identity.Key = diskio.DiskKey("physical:" + strconv.FormatUint(uint64(diskNos[0]), 10))
		return identity
	}
	parts := make([]string, len(diskNos))
	for index, diskNo := range diskNos {
		parts[index] = strconv.FormatUint(uint64(diskNo), 10)
	}
	identity.Key = diskio.DiskKey("physical-set:" + strings.Join(parts, ","))
	return identity
}

func uniqueDiskNumbers(numbers []uint32) []uint32 {
	if len(numbers) < 2 {
		return numbers
	}
	write := 1
	for _, number := range numbers[1:] {
		if number == numbers[write-1] {
			continue
		}
		numbers[write] = number
		write++
	}
	return numbers[:write]
}

func networkIdentity(path string) (diskio.Identity, bool) {
	if !strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, `\\?\`) {
		return diskio.Identity{}, false
	}
	parts := strings.Split(strings.TrimPrefix(path, `\\`), `\`)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return diskio.Identity{}, false
	}
	volume := `\\` + parts[0] + `\` + parts[1]
	return diskio.Identity{
		Key:    diskio.DiskKey("network:" + strings.ToLower(parts[0]) + "/" + strings.ToLower(parts[1])),
		Local:  false,
		Volume: volume,
	}, true
}

func interpretSeekPenalty(descriptor [12]byte, err error) (isSSD, known bool) {
	if err != nil {
		// The documented conservative fallback is HDD.
		return false, false
	}
	return descriptor[8] == 0, true
}
