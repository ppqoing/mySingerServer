package helper

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unsafe"

	"dedup/internal/proto"
	"golang.org/x/sys/windows"
)

var compareStringOrdinal = windows.NewLazySystemDLL("kernel32.dll").
	NewProc("CompareStringOrdinal")

type ValidatedPath struct {
	Path       string
	VolumeRoot string
	Relative   string
	Attributes uint32
}

type validatedRoot struct {
	path        string
	volumeRoot  string
	recycleRoot string
}

type Validator struct {
	allowed []validatedRoot
	denied  []string
	recycle string
}

func NewValidator(cfg Config) (*Validator, error) {
	if err := validateRecycleDirName(cfg.RecycleDirName); err != nil {
		return nil, err
	}
	allowed, denied, err := normalizeRootLists(
		cfg.AllowedRoots,
		cfg.DeniedRoots,
		cfg.RecycleDirName,
	)
	if err != nil {
		return nil, err
	}
	v := &Validator{
		allowed: make([]validatedRoot, 0, len(allowed)),
		denied:  denied,
		recycle: cfg.RecycleDirName,
	}
	for _, root := range allowed {
		volumeRoot := filepath.VolumeName(root) + `\`
		v.allowed = append(v.allowed, validatedRoot{
			path:        root,
			volumeRoot:  volumeRoot,
			recycleRoot: filepath.Join(volumeRoot, cfg.RecycleDirName),
		})
	}
	return v, nil
}

func (v *Validator) ValidateFile(path string) (ValidatedPath, error) {
	normalized, err := normalizeLocalAbsolute(path)
	if err != nil {
		return ValidatedPath{}, pathError(proto.DeleteErrBadPath, err)
	}
	root, ok := v.matchAllowedRoot(normalized)
	if !ok {
		return ValidatedPath{}, pathError(
			proto.DeleteErrPathDenied,
			fmt.Errorf("path is outside allowed roots"),
		)
	}
	for _, denied := range v.denied {
		if equalOrBelow(normalized, denied) {
			return ValidatedPath{}, pathError(
				proto.DeleteErrPathDenied,
				fmt.Errorf("path is inside a denied root"),
			)
		}
	}
	if equalOrBelow(normalized, root.recycleRoot) {
		return ValidatedPath{}, pathError(
			proto.DeleteErrPathDenied,
			fmt.Errorf("direct recycle-tree input is forbidden"),
		)
	}
	attributes, exists, err := walkExistingPath(root.path, normalized)
	if err != nil {
		return ValidatedPath{}, err
	}
	if !exists {
		return ValidatedPath{}, pathError(
			proto.DeleteErrNotFound,
			fmt.Errorf("file does not exist"),
		)
	}
	if attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return ValidatedPath{}, pathError(
			proto.DeleteErrBadPath,
			fmt.Errorf("path is a directory"),
		)
	}
	relative, err := ordinalRelativePath(root.path, normalized)
	if err != nil || relative == "." {
		return ValidatedPath{}, pathError(
			proto.DeleteErrBadPath,
			fmt.Errorf("cannot derive safe relative path"),
		)
	}
	return ValidatedPath{
		Path:       normalized,
		VolumeRoot: root.volumeRoot,
		Relative:   relative,
		Attributes: uint32(attributes),
	}, nil
}

func (v *Validator) ValidateRecycleTarget(path string) error {
	normalized, err := normalizeLocalAbsolute(path)
	if err != nil {
		return pathError(proto.DeleteErrBadPath, err)
	}
	volume := filepath.VolumeName(normalized)
	var recycleRoot string
	for _, root := range v.allowed {
		if ordinalEqualFold(filepath.VolumeName(root.path), volume) {
			recycleRoot = root.recycleRoot
			break
		}
	}
	if recycleRoot == "" || !strictDescendant(normalized, recycleRoot) {
		return pathError(
			proto.DeleteErrPathDenied,
			fmt.Errorf("recycle target is outside the configured recycle tree"),
		)
	}
	_, _, err = walkExistingPath(recycleRoot, normalized)
	return err
}

func (v *Validator) matchAllowedRoot(path string) (validatedRoot, bool) {
	var matched validatedRoot
	found := false
	for _, root := range v.allowed {
		if equalOrBelow(path, root.path) &&
			(!found || len(root.path) > len(matched.path)) {
			matched = root
			found = true
		}
	}
	return matched, found
}

func walkExistingPath(start, target string) (uint32, bool, error) {
	if !equalOrBelow(target, start) {
		return 0, false, pathError(
			proto.DeleteErrBadPath,
			fmt.Errorf("path escaped validation root"),
		)
	}
	relative, err := ordinalRelativePath(start, target)
	if err != nil {
		return 0, false, pathError(proto.DeleteErrBadPath, err)
	}
	current := start
	parts := []string{}
	if relative != "." {
		parts = strings.Split(relative, `\`)
	}
	for index := 0; index <= len(parts); index++ {
		if index > 0 {
			current = filepath.Join(current, parts[index-1])
		}
		attributes, err := getFileAttributes(current)
		if err != nil {
			if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
				errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
				return 0, false, nil
			}
			if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				return 0, false, pathError(proto.DeleteErrAccessDenied, err)
			}
			return 0, false, pathError(proto.DeleteErrBadPath, err)
		}
		if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return 0, false, pathError(
				proto.DeleteErrReparse,
				fmt.Errorf("reparse point at %s", current),
			)
		}
		if index < len(parts) && attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			return 0, false, pathError(
				proto.DeleteErrBadPath,
				fmt.Errorf("non-directory ancestor at %s", current),
			)
		}
		if index == len(parts) {
			return attributes, true, nil
		}
	}
	panic("unreachable")
}

func ordinalRelativePath(root, target string) (string, error) {
	if ordinalEqualFold(root, target) {
		return ".", nil
	}
	prefix := root
	if !strings.HasSuffix(prefix, `\`) {
		prefix += `\`
	}
	if len(target) <= len(prefix) ||
		!ordinalEqualFold(target[:len(prefix)], prefix) {
		return "", fmt.Errorf("path is not an ordinal descendant of root")
	}
	relative := target[len(prefix):]
	if relative == "" {
		return "", fmt.Errorf("path has no descendant components")
	}
	return relative, nil
}

func getFileAttributes(path string) (uint32, error) {
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.GetFileAttributes(pathUTF16)
}

func ensureOrdinalIgnoreCase() error {
	if err := compareStringOrdinal.Find(); err != nil {
		return fmt.Errorf("helper config: Windows ordinal comparison unavailable: %w", err)
	}
	if !ordinalEqualFold("A", "a") || ordinalEqualFold("K", "K") {
		return fmt.Errorf("helper config: Windows ordinal comparison self-check failed")
	}
	return nil
}

func ordinalEqualFold(left, right string) bool {
	leftUTF16, err := windows.UTF16FromString(left)
	if err != nil {
		return false
	}
	rightUTF16, err := windows.UTF16FromString(right)
	if err != nil {
		return false
	}
	result, _, _ := compareStringOrdinal.Call(
		uintptr(unsafe.Pointer(&leftUTF16[0])),
		uintptr(len(leftUTF16)-1),
		uintptr(unsafe.Pointer(&rightUTF16[0])),
		uintptr(len(rightUTF16)-1),
		1,
	)
	return result == 2
}

func pathError(code string, err error) error {
	return &PathError{Code: code, Err: err}
}
