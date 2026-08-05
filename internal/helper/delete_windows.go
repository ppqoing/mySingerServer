package helper

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"dedup/internal/proto"
	"golang.org/x/sys/windows"
)

const maxRecycleCollisions = 32
const windowsPathBufferLength = 32768

type processorOps struct {
	rename           func(string, string) error
	remove           func(string) error
	mkdir            func(string, fs.FileMode) error
	setAttributes    func(string, uint32) error
	revalidateSource func(ValidatedPath) (ValidatedPath, error)
	volumeID         func(string) (string, error)
}

func defaultProcessorOps() processorOps {
	return processorOps{
		rename:   moveFileNoReplace,
		remove:   os.Remove,
		mkdir:    os.Mkdir,
		volumeID: resolveVolumeID,
		setAttributes: func(path string, attributes uint32) error {
			pathUTF16, err := windows.UTF16PtrFromString(path)
			if err != nil {
				return err
			}
			return windows.SetFileAttributes(pathUTF16, attributes)
		},
	}
}

func (p *Processor) softDelete(
	ctx context.Context,
	validated ValidatedPath,
	taskID string,
) (string, error) {
	volumeRelative, err := ordinalRelativePath(
		validated.VolumeRoot,
		validated.Path,
	)
	if err != nil || volumeRelative == "." {
		return "", pathError(
			proto.DeleteErrBadPath,
			fmt.Errorf("cannot derive volume-relative recycle path"),
		)
	}
	baseDestination := filepath.Join(
		validated.VolumeRoot,
		p.cfg.RecycleDirName,
		taskID,
		volumeRelative,
	)
	if !ordinalEqualFold(
		filepath.VolumeName(validated.Path),
		filepath.VolumeName(baseDestination),
	) {
		return "", pathError(
			proto.DeleteErrRecycleFailed,
			fmt.Errorf("soft delete destination is on another volume"),
		)
	}
	if _, err := p.ops.revalidateSource(validated); err != nil {
		return "", err
	}
	if err := p.validator.ValidateRecycleTarget(baseDestination); err != nil {
		return "", err
	}
	if err := p.ensureSameVolume(validated.Path, baseDestination); err != nil {
		return "", err
	}
	if err := p.ensureRecycleParents(ctx, baseDestination); err != nil {
		return "", err
	}

	for collision := 0; collision <= maxRecycleCollisions; collision++ {
		if err := ctx.Err(); err != nil {
			return "", pathError(proto.DeleteErrDeleteFailed, err)
		}
		destination := recycleCollisionPath(baseDestination, collision)
		if err := p.validator.ValidateRecycleTarget(destination); err != nil {
			return "", err
		}
		exists, err := processorPathExists(destination)
		if err != nil {
			return "", mapMutationError(err, proto.DeleteErrRecycleFailed)
		}
		if exists {
			continue
		}
		if _, err := p.ops.revalidateSource(validated); err != nil {
			return "", err
		}
		if err := p.validator.ValidateRecycleTarget(destination); err != nil {
			return "", err
		}
		if err := p.ensureSameVolume(validated.Path, destination); err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", pathError(proto.DeleteErrDeleteFailed, err)
		}
		err = p.ops.rename(validated.Path, destination)
		if err == nil {
			return destination, nil
		}
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) ||
			errors.Is(err, windows.ERROR_FILE_EXISTS) ||
			errors.Is(err, fs.ErrExist) {
			continue
		}
		return "", mapMutationError(err, proto.DeleteErrRecycleFailed)
	}
	return "", pathError(
		proto.DeleteErrRecycleFailed,
		fmt.Errorf("recycle collision limit reached"),
	)
}

func (p *Processor) hardDelete(
	ctx context.Context,
	validated ValidatedPath,
) (bool, error) {
	current, err := p.ops.revalidateSource(validated)
	if err != nil {
		return false, err
	}
	readonlyCleared := false
	if current.Attributes&windows.FILE_ATTRIBUTE_READONLY != 0 {
		attributes := current.Attributes &^ windows.FILE_ATTRIBUTE_READONLY
		if err := p.ops.setAttributes(current.Path, attributes); err != nil {
			return false, pathError(proto.DeleteErrReadonly, err)
		}
		readonlyCleared = true
		current, err = p.ops.revalidateSource(validated)
		if err != nil {
			return readonlyCleared, err
		}
		if current.Attributes&windows.FILE_ATTRIBUTE_READONLY != 0 {
			return readonlyCleared, pathError(
				proto.DeleteErrReadonly,
				fmt.Errorf("readonly attribute remains after clear"),
			)
		}
	}
	if err := ctx.Err(); err != nil {
		return readonlyCleared, pathError(proto.DeleteErrDeleteFailed, err)
	}
	if err := p.ops.remove(current.Path); err != nil {
		return readonlyCleared, mapMutationError(
			err,
			proto.DeleteErrDeleteFailed,
		)
	}
	return readonlyCleared, nil
}

func (p *Processor) ensureRecycleParents(
	ctx context.Context,
	destination string,
) error {
	recycleRoot := filepath.Join(
		filepath.VolumeName(destination)+`\`,
		p.cfg.RecycleDirName,
	)
	parent := filepath.Dir(destination)
	relative, err := ordinalRelativePath(recycleRoot, parent)
	if err != nil && !ordinalEqualFold(recycleRoot, parent) {
		return pathError(proto.DeleteErrRecycleFailed, err)
	}
	directories := []string{recycleRoot}
	if relative != "." && relative != "" {
		current := recycleRoot
		for _, component := range strings.Split(relative, `\`) {
			current = filepath.Join(current, component)
			directories = append(directories, current)
		}
	}

	for _, directory := range directories {
		if err := ctx.Err(); err != nil {
			return pathError(proto.DeleteErrDeleteFailed, err)
		}
		if err := p.validator.ValidateRecycleTarget(destination); err != nil {
			return err
		}
		attributes, err := getFileAttributes(directory)
		if err == nil {
			if attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
				return pathError(
					proto.DeleteErrRecycleFailed,
					fmt.Errorf("recycle parent is not a directory: %s", directory),
				)
			}
			continue
		}
		if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) &&
			!errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return mapMutationError(err, proto.DeleteErrRecycleFailed)
		}
		if err := p.ops.mkdir(directory, 0o700); err != nil &&
			!errors.Is(err, windows.ERROR_ALREADY_EXISTS) &&
			!errors.Is(err, windows.ERROR_FILE_EXISTS) &&
			!errors.Is(err, fs.ErrExist) {
			return mapMutationError(err, proto.DeleteErrRecycleFailed)
		}
		if err := p.validator.ValidateRecycleTarget(destination); err != nil {
			return err
		}
		attributes, err = getFileAttributes(directory)
		if err != nil {
			return mapMutationError(err, proto.DeleteErrRecycleFailed)
		}
		if attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			return pathError(
				proto.DeleteErrRecycleFailed,
				fmt.Errorf("created recycle parent is not a directory: %s", directory),
			)
		}
	}
	return nil
}

func (p *Processor) revalidateSource(
	previous ValidatedPath,
) (ValidatedPath, error) {
	current, err := p.validator.ValidateFile(previous.Path)
	if err != nil {
		return ValidatedPath{}, err
	}
	if !ordinalEqualFold(current.Path, previous.Path) ||
		!ordinalEqualFold(current.VolumeRoot, previous.VolumeRoot) {
		return ValidatedPath{}, pathError(
			proto.DeleteErrBadPath,
			fmt.Errorf("source identity changed during validation"),
		)
	}
	return current, nil
}

func (p *Processor) ensureSameVolume(source, destination string) error {
	sourceID, err := p.ops.volumeID(source)
	if err != nil {
		return pathError(
			proto.DeleteErrRecycleFailed,
			fmt.Errorf("resolve source volume identity: %w", err),
		)
	}
	destinationID, err := p.ops.volumeID(destination)
	if err != nil {
		return pathError(
			proto.DeleteErrRecycleFailed,
			fmt.Errorf("resolve destination volume identity: %w", err),
		)
	}
	if sourceID == "" ||
		destinationID == "" ||
		!ordinalEqualFold(sourceID, destinationID) {
		return pathError(
			proto.DeleteErrRecycleFailed,
			fmt.Errorf("soft delete source and destination volumes differ"),
		)
	}
	return nil
}

func recycleCollisionPath(path string, collision int) string {
	if collision == 0 {
		return path
	}
	extension := filepath.Ext(path)
	return strings.TrimSuffix(path, extension) +
		fmt.Sprintf("_%d", collision) +
		extension
}

func moveFileNoReplace(from, to string) error {
	fromUTF16, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	toUTF16, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(fromUTF16, toUTF16, 0)
}

func resolveVolumeID(path string) (string, error) {
	return resolveVolumeIDAtPath(path, true)
}

func resolveVolumeIDAtPath(path string, allowDOSAlias bool) (string, error) {
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	mountPointBuffer := make([]uint16, windowsPathBufferLength)
	if err := windows.GetVolumePathName(
		pathUTF16,
		&mountPointBuffer[0],
		uint32(len(mountPointBuffer)),
	); err != nil {
		if allowDOSAlias {
			physicalPath, aliasErr := resolveDOSAliasPath(path)
			if aliasErr == nil {
				return resolveVolumeIDAtPath(physicalPath, false)
			}
		}
		return "", fmt.Errorf(
			"resolve volume mount point for %q: %w",
			path,
			err,
		)
	}
	mountPoint := windows.UTF16ToString(mountPointBuffer)
	if !strings.HasSuffix(mountPoint, `\`) {
		mountPoint += `\`
	}
	mountPointUTF16, err := windows.UTF16PtrFromString(mountPoint)
	if err != nil {
		return "", err
	}
	volumeIDBuffer := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumeNameForVolumeMountPoint(
		mountPointUTF16,
		&volumeIDBuffer[0],
		uint32(len(volumeIDBuffer)),
	); err != nil {
		if allowDOSAlias && errors.Is(err, windows.ERROR_NOT_A_REPARSE_POINT) {
			physicalPath, aliasErr := resolveDOSAliasPath(path)
			if aliasErr == nil {
				return resolveVolumeIDAtPath(physicalPath, false)
			}
			return "", fmt.Errorf(
				"resolve DOS alias after mount point %q: %w",
				mountPoint,
				aliasErr,
			)
		}
		return "", fmt.Errorf(
			"resolve volume GUID for mount point %q: %w",
			mountPoint,
			err,
		)
	}
	return windows.UTF16ToString(volumeIDBuffer), nil
}

func resolveDOSAliasPath(path string) (string, error) {
	normalizedPath, err := normalizeLocalAbsolute(path)
	if err != nil {
		return "", fmt.Errorf("DOS alias input path: %w", err)
	}
	volume := filepath.VolumeName(normalizedPath)
	if len(volume) != 2 ||
		!isASCIILetter(volume[0]) ||
		volume[1] != ':' {
		return "", fmt.Errorf("path has no drive-letter DOS alias")
	}
	volumeUTF16, err := windows.UTF16PtrFromString(volume)
	if err != nil {
		return "", err
	}
	targetBuffer := make([]uint16, windowsPathBufferLength)
	targetLength, err := windows.QueryDosDevice(
		volumeUTF16,
		&targetBuffer[0],
		uint32(len(targetBuffer)),
	)
	if err != nil {
		return "", err
	}
	return expandDOSAliasTarget(normalizedPath, targetBuffer, targetLength)
}

func expandDOSAliasTarget(
	path string,
	targetBuffer []uint16,
	targetLength uint32,
) (string, error) {
	normalizedPath, err := normalizeLocalAbsolute(path)
	if err != nil {
		return "", fmt.Errorf("DOS alias input path: %w", err)
	}
	volume := filepath.VolumeName(normalizedPath)
	if len(volume) != 2 ||
		!isASCIILetter(volume[0]) ||
		volume[1] != ':' {
		return "", fmt.Errorf("path has no ASCII drive-letter DOS alias")
	}
	if targetLength < 2 || uint64(targetLength) > uint64(len(targetBuffer)) {
		return "", fmt.Errorf("DOS alias target list length is invalid")
	}
	targets := targetBuffer[:targetLength]
	firstTerminator := -1
	for index, value := range targets {
		if value == 0 {
			firstTerminator = index
			break
		}
	}
	if firstTerminator <= 0 || firstTerminator+1 >= len(targets) {
		return "", fmt.Errorf("DOS alias target list is not double-NUL terminated")
	}
	for _, value := range targets[firstTerminator+1:] {
		if value != 0 {
			return "", fmt.Errorf("DOS alias has ambiguous multiple targets")
		}
	}
	target := windows.UTF16ToString(targets[:firstTerminator])
	const localDOSPrefix = `\??\`
	if !strings.HasPrefix(target, localDOSPrefix) {
		return "", fmt.Errorf("DOS alias does not target a local Win32 path")
	}
	target = strings.TrimPrefix(target, localDOSPrefix)
	normalizedTarget, err := normalizeLocalAbsolute(target)
	if err != nil {
		return "", fmt.Errorf("DOS alias target is not a safe local path: %w", err)
	}
	targetVolume := filepath.VolumeName(normalizedTarget)
	if len(targetVolume) != 2 ||
		!isASCIILetter(targetVolume[0]) ||
		targetVolume[1] != ':' {
		return "", fmt.Errorf("DOS alias target has no ASCII drive letter")
	}
	suffix := strings.TrimPrefix(normalizedPath[len(volume):], `\`)
	return filepath.Join(normalizedTarget, suffix), nil
}

func processorPathExists(path string) (bool, error) {
	_, err := getFileAttributes(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return false, nil
	}
	return false, err
}

func mapMutationError(err error, fallback string) error {
	if err == nil {
		return nil
	}
	var pathErr *PathError
	if errors.As(err, &pathErr) {
		return err
	}
	switch {
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND),
		errors.Is(err, windows.ERROR_PATH_NOT_FOUND),
		errors.Is(err, fs.ErrNotExist):
		return pathError(proto.DeleteErrNotFound, err)
	case errors.Is(err, windows.ERROR_SHARING_VIOLATION),
		errors.Is(err, windows.ERROR_LOCK_VIOLATION):
		return pathError(proto.DeleteErrInUse, err)
	case errors.Is(err, windows.ERROR_ACCESS_DENIED),
		errors.Is(err, fs.ErrPermission):
		return pathError(proto.DeleteErrAccessDenied, err)
	default:
		return pathError(fallback, err)
	}
}
