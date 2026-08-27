package wproc

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"image/jpeg"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type ContactSheetPaths struct {
	JPEG     string
	TempJPEG string
}

type ContactSheetJPEG struct {
	Path          string
	Width, Height int
}

var (
	contactSheetTempName = regexp.MustCompile(`^[0-9a-f]{128}\.jpg\.tmp-[0-9]+-[0-9]+-[A-Za-z0-9_-]+$`)
	contactSheetShard    = regexp.MustCompile(`^[0-9a-f]{2}$`)
)

// PrepareContactSheetRoot creates and validates the configured cache root.
// Startup cleanup is deliberately limited to stale temps in current two-hex
// shards; legacy directories and final JPEGs are never traversed or removed.
func PrepareContactSheetRoot(root string) error {
	if root == "" {
		return fmt.Errorf("contact sheet cache root is empty")
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("absolute contact sheet cache root: %w", err)
	}
	absoluteRoot = filepath.Clean(absoluteRoot)
	if err := os.MkdirAll(absoluteRoot, 0o755); err != nil {
		return fmt.Errorf("create contact sheet cache root: %w", err)
	}
	canonical, err := contactSheetRoot(absoluteRoot)
	if err != nil {
		return err
	}
	return cleanContactSheetRootStaleTemps(canonical, time.Now().Add(-time.Hour))
}

func contactSheetPaths(root string, sha [64]byte, pid int, jobID int64, nonce string) (ContactSheetPaths, error) {
	if root == "" {
		return ContactSheetPaths{}, fmt.Errorf("contact sheet cache root is empty")
	}
	if pid < 0 || jobID < 0 || !validContactSheetNonce(nonce) {
		return ContactSheetPaths{}, fmt.Errorf("invalid contact sheet temp identifier")
	}
	absoluteRoot, err := contactSheetRoot(root)
	if err != nil {
		return ContactSheetPaths{}, err
	}
	shaHex := hex.EncodeToString(sha[:])
	directory, err := contactSheetPathUnderRoot(absoluteRoot, shaHex[:2])
	if err != nil {
		return ContactSheetPaths{}, err
	}
	if err := ensureContactSheetDirectory(absoluteRoot, directory); err != nil {
		return ContactSheetPaths{}, err
	}
	jpegPath, err := contactSheetPathUnderRoot(absoluteRoot, shaHex[:2], shaHex+".jpg")
	if err != nil {
		return ContactSheetPaths{}, err
	}
	identifier := strconv.Itoa(pid) + "-" + strconv.FormatInt(jobID, 10) + "-" + nonce
	return ContactSheetPaths{JPEG: jpegPath, TempJPEG: jpegPath + ".tmp-" + identifier}, nil
}

func lookupContactSheet(root string, sha [64]byte) (ContactSheetJPEG, bool, error) {
	jpegPath, err := contactSheetFinalPath(root, sha)
	if err != nil {
		return ContactSheetJPEG{}, false, err
	}
	info, err := os.Lstat(jpegPath)
	if err != nil {
		if os.IsNotExist(err) {
			return ContactSheetJPEG{Path: jpegPath}, false, nil
		}
		return ContactSheetJPEG{}, false, fmt.Errorf("stat contact sheet JPEG: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return ContactSheetJPEG{Path: jpegPath}, false, nil
	}
	data, err := os.ReadFile(jpegPath)
	if err != nil {
		if os.IsNotExist(err) || contactSheetTransientReadError(err) {
			return ContactSheetJPEG{Path: jpegPath}, false, nil
		}
		return ContactSheetJPEG{}, false, fmt.Errorf("read contact sheet JPEG: %w", err)
	}
	geometry, err := inspectRGBJPEG(data)
	if err != nil {
		return ContactSheetJPEG{Path: jpegPath}, false, nil
	}
	return ContactSheetJPEG{Path: jpegPath, Width: geometry.Width, Height: geometry.Height}, true, nil
}

func contactSheetTransientReadError(err error) bool {
	return errors.Is(err, syscall.Errno(32)) || errors.Is(err, syscall.Errno(33))
}

func publishContactSheet(paths ContactSheetPaths, validateSource func() error) error {
	return publishContactSheetWithReplace(paths, validateSource, atomicReplace)
}

func publishContactSheetWithReplace(paths ContactSheetPaths, validateSource func() error, replace func(string, string) error) (result error) {
	if err := validContactSheetPaths(paths); err != nil {
		return err
	}
	if validateSource == nil {
		return fmt.Errorf("contact sheet source validator is nil")
	}
	if replace == nil {
		return fmt.Errorf("contact sheet replace function is nil")
	}
	defer func() {
		if err := os.Remove(paths.TempJPEG); result == nil && err != nil && !os.IsNotExist(err) {
			result = fmt.Errorf("remove contact sheet temp JPEG: %w", err)
		}
	}()
	tempInfo, err := os.Lstat(paths.TempJPEG)
	if err != nil {
		return fmt.Errorf("stat contact sheet temp JPEG: %w", err)
	}
	if !tempInfo.Mode().IsRegular() || tempInfo.Size() == 0 {
		return fmt.Errorf("contact sheet temp JPEG must be a non-empty regular file")
	}
	file, err := os.OpenFile(paths.TempJPEG, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open contact sheet temp JPEG: %w", err)
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return fmt.Errorf("sync contact sheet temp JPEG: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close contact sheet temp JPEG: %w", closeErr)
	}
	data, err := os.ReadFile(paths.TempJPEG)
	if err != nil {
		return fmt.Errorf("read contact sheet temp JPEG: %w", err)
	}
	if _, err := inspectRGBJPEG(data); err != nil {
		return fmt.Errorf("validate contact sheet temp JPEG: %w", err)
	}
	if err := validateSource(); err != nil {
		return err
	}
	if err := replace(paths.TempJPEG, paths.JPEG); err != nil {
		return fmt.Errorf("commit contact sheet JPEG: %w", err)
	}
	return nil
}

func cleanContactSheetStaleTemps(paths ContactSheetPaths, olderThan time.Time) error {
	if err := validContactSheetPaths(paths); err != nil {
		return err
	}
	return cleanContactSheetDirectoryStaleTemps(filepath.Dir(paths.JPEG), olderThan)
}

func cleanContactSheetRootStaleTemps(root string, olderThan time.Time) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("list contact sheet cache root: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !contactSheetShard.MatchString(entry.Name()) || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		directory, err := contactSheetPathUnderRoot(root, entry.Name())
		if err != nil {
			return err
		}
		if err := ensureContactSheetDirectory(root, directory); err != nil {
			return err
		}
		if err := cleanContactSheetDirectoryStaleTemps(directory, olderThan); err != nil {
			return err
		}
	}
	return nil
}

func cleanContactSheetDirectoryStaleTemps(directory string, olderThan time.Time) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("list contact sheet temps: %w", err)
	}
	for _, entry := range entries {
		if !contactSheetTempName.MatchString(entry.Name()) || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("stat contact sheet temp: %w", err)
		}
		if !info.Mode().IsRegular() || !info.ModTime().Before(olderThan) {
			continue
		}
		if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale contact sheet temp: %w", err)
		}
	}
	return nil
}

func contactSheetFinalPath(root string, sha [64]byte) (string, error) {
	if root == "" {
		return "", fmt.Errorf("contact sheet cache root is empty")
	}
	absoluteRoot, err := contactSheetRoot(root)
	if err != nil {
		return "", err
	}
	shaHex := hex.EncodeToString(sha[:])
	shaDirectory, err := contactSheetPathUnderRoot(absoluteRoot, shaHex[:2])
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(shaDirectory); err == nil {
		if err := ensureContactSheetDirectory(absoluteRoot, shaDirectory); err != nil {
			return "", err
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat contact sheet shard: %w", err)
	}
	return contactSheetPathUnderRoot(absoluteRoot, shaHex[:2], shaHex+".jpg")
}

func contactSheetRoot(root string) (string, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("absolute contact sheet cache root: %w", err)
	}
	absoluteRoot = filepath.Clean(absoluteRoot)
	info, err := os.Lstat(absoluteRoot)
	if err != nil {
		return "", fmt.Errorf("stat contact sheet cache root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("contact sheet cache root is not a normal directory")
	}
	canonical, err := contactSheetCanonicalDirectory(absoluteRoot)
	if err != nil {
		return "", fmt.Errorf("canonical contact sheet cache root: %w", err)
	}
	return filepath.Clean(canonical), nil
}

func ensureContactSheetDirectory(root, directory string) error {
	if err := contactSheetPathWithinRoot(root, directory); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if os.IsNotExist(err) {
		if err := os.Mkdir(directory, 0o755); err != nil && !os.IsExist(err) {
			return fmt.Errorf("create contact sheet cache directory: %w", err)
		}
		info, err = os.Lstat(directory)
	}
	if err != nil {
		return fmt.Errorf("stat contact sheet cache directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("contact sheet cache directory is not a normal directory")
	}
	canonical, err := contactSheetCanonicalDirectory(directory)
	if err != nil {
		return fmt.Errorf("canonical contact sheet cache directory: %w", err)
	}
	if err := contactSheetPathWithinRoot(root, canonical); err != nil {
		return fmt.Errorf("contact sheet cache directory escapes root: %w", err)
	}
	return nil
}

func contactSheetPathUnderRoot(root string, parts ...string) (string, error) {
	path := filepath.Clean(filepath.Join(append([]string{root}, parts...)...))
	if err := contactSheetPathWithinRoot(root, path); err != nil {
		return "", err
	}
	return path, nil
}

func contactSheetPathWithinRoot(root, path string) error {
	path = filepath.Clean(path)
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("contact sheet cache path escapes root")
	}
	return nil
}

func validContactSheetNonce(nonce string) bool {
	if nonce == "" || strings.Contains(nonce, "..") {
		return false
	}
	for _, runeValue := range nonce {
		if !(runeValue >= 'a' && runeValue <= 'z') && !(runeValue >= 'A' && runeValue <= 'Z') && !(runeValue >= '0' && runeValue <= '9') && runeValue != '_' && runeValue != '-' {
			return false
		}
	}
	return true
}

func validContactSheetPaths(paths ContactSheetPaths) error {
	if !filepath.IsAbs(paths.JPEG) {
		return fmt.Errorf("contact sheet JPEG path is not absolute")
	}
	if _, err := contactSheetSHAFromJPEGPath(paths.JPEG); err != nil {
		return err
	}
	if filepath.Dir(paths.TempJPEG) != filepath.Dir(paths.JPEG) {
		return fmt.Errorf("contact sheet paths are not colocated")
	}
	prefix := paths.JPEG + ".tmp-"
	if !strings.HasPrefix(paths.TempJPEG, prefix) {
		return fmt.Errorf("contact sheet temp path is invalid")
	}
	identifier := strings.TrimPrefix(paths.TempJPEG, prefix)
	parts := strings.Split(identifier, "-")
	if len(parts) < 3 {
		return fmt.Errorf("contact sheet temp identifier is invalid")
	}
	if _, err := strconv.ParseUint(parts[0], 10, 0); err != nil {
		return fmt.Errorf("contact sheet temp pid is invalid")
	}
	if _, err := strconv.ParseInt(parts[1], 10, 64); err != nil || strings.HasPrefix(parts[1], "-") {
		return fmt.Errorf("contact sheet temp job id is invalid")
	}
	if !validContactSheetNonce(strings.Join(parts[2:], "-")) {
		return fmt.Errorf("contact sheet temp nonce is invalid")
	}
	return nil
}

func contactSheetSHAFromJPEGPath(jpegPath string) ([64]byte, error) {
	var sha [64]byte
	name := filepath.Base(jpegPath)
	if !strings.HasSuffix(name, ".jpg") || len(name) != len(sha)*2+len(".jpg") {
		return sha, fmt.Errorf("contact sheet JPEG path is invalid")
	}
	encoded := strings.TrimSuffix(name, ".jpg")
	if strings.ToLower(encoded) != encoded {
		return sha, fmt.Errorf("contact sheet JPEG SHA is not lowercase")
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != len(sha) {
		return sha, fmt.Errorf("contact sheet JPEG SHA is invalid")
	}
	copy(sha[:], decoded)
	if filepath.Base(filepath.Dir(jpegPath)) != encoded[:2] {
		return sha, fmt.Errorf("contact sheet JPEG path does not use cache layout")
	}
	return sha, nil
}

type contactSheetGeometry struct {
	Width, Height int
}

func inspectRGBJPEG(data []byte) (contactSheetGeometry, error) {
	components, err := jpegSOFComponents(data)
	if err != nil {
		return contactSheetGeometry{}, err
	}
	if components != 3 {
		return contactSheetGeometry{}, fmt.Errorf("JPEG components = %d, want 3", components)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return contactSheetGeometry{}, err
	}
	bounds := decoded.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		return contactSheetGeometry{}, fmt.Errorf("JPEG dimensions are invalid")
	}
	return contactSheetGeometry{Width: bounds.Dx(), Height: bounds.Dy()}, nil
}

func jpegSOFComponents(data []byte) (int, error) {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return 0, fmt.Errorf("JPEG SOI is missing")
	}
	for offset := 2; offset < len(data); {
		if data[offset] != 0xff {
			return 0, fmt.Errorf("JPEG marker is malformed")
		}
		for offset < len(data) && data[offset] == 0xff {
			offset++
		}
		if offset >= len(data) {
			break
		}
		marker := data[offset]
		offset++
		if marker == 0xd8 || marker == 0xd9 || marker >= 0xd0 && marker <= 0xd7 {
			continue
		}
		if offset+2 > len(data) {
			break
		}
		segmentLength := int(data[offset])<<8 | int(data[offset+1])
		if segmentLength < 2 || offset+segmentLength > len(data) {
			return 0, fmt.Errorf("JPEG segment is truncated")
		}
		if isJPEGSOF(marker) {
			if segmentLength < 8 {
				return 0, fmt.Errorf("JPEG SOF is truncated")
			}
			return int(data[offset+7]), nil
		}
		if marker == 0xda {
			break
		}
		offset += segmentLength
	}
	return 0, fmt.Errorf("JPEG SOF is missing")
}

func isJPEGSOF(marker byte) bool {
	return marker >= 0xc0 && marker <= 0xcf && marker != 0xc4 && marker != 0xc8 && marker != 0xcc
}
