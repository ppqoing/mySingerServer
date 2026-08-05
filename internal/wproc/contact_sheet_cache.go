package wproc

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"dedup/internal/wproc/videocore"
)

const contactSheetPipeline = "vc-grid-v1"

type ContactSheetPaths struct {
	JPEG        string
	Sidecar     string
	TempJPEG    string
	TempSidecar string
}

type ContactSheetSample struct {
	TimeMS int64  `json:"time_ms"`
	Status string `json:"status"`
}

// ContactSheetMeta is the complete, content-addressed description of a grid.
type ContactSheetMeta struct {
	SchemaVersion    int                           `json:"schema_version"`
	Pipeline         string                        `json:"pipeline"`
	SourceSHA512     string                        `json:"source_sha512"`
	SourceSize       int64                         `json:"source_size"`
	JPEGSHA256       string                        `json:"jpeg_sha256"`
	CanvasWidth      int                           `json:"canvas_width"`
	CanvasHeight     int                           `json:"canvas_height"`
	TileWidth        int                           `json:"tile_width"`
	TileHeight       int                           `json:"tile_height"`
	Samples          [6]ContactSheetSample         `json:"samples"`
	VideoCoreVersion string                        `json:"videocore_version"`
	FFmpeg           [4]videocore.RuntimeComponent `json:"ffmpeg"`
}

var contactSheetTempName = regexp.MustCompile(`^[0-9a-f]{128}\.jpg(?:\.json)?\.tmp-[0-9]+-[0-9]+-[A-Za-z0-9_-]+$`)

type contactSheetPublishLock interface {
	Release() error
}

// The cache is process-local. Serializing the two-file commit prevents two
// local writers from leaving a final JPEG and sidecar from different writers.
var contactSheetPublishMu sync.Mutex

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
	cacheDirectory, err := contactSheetPathUnderRoot(absoluteRoot, contactSheetPipeline)
	if err != nil {
		return ContactSheetPaths{}, err
	}
	if err := ensureContactSheetDirectory(absoluteRoot, cacheDirectory); err != nil {
		return ContactSheetPaths{}, err
	}
	directory, err := contactSheetPathUnderRoot(cacheDirectory, shaHex[:2])
	if err != nil {
		return ContactSheetPaths{}, err
	}
	if err := ensureContactSheetDirectory(absoluteRoot, directory); err != nil {
		return ContactSheetPaths{}, err
	}
	jpeg, err := contactSheetPathUnderRoot(absoluteRoot, contactSheetPipeline, shaHex[:2], shaHex+".jpg")
	if err != nil {
		return ContactSheetPaths{}, err
	}
	identifier := strconv.Itoa(pid) + "-" + strconv.FormatInt(jobID, 10) + "-" + nonce
	return ContactSheetPaths{
		JPEG:        jpeg,
		Sidecar:     jpeg + ".json",
		TempJPEG:    jpeg + ".tmp-" + identifier,
		TempSidecar: jpeg + ".json.tmp-" + identifier,
	}, nil
}

func lookupContactSheet(root string, sha [64]byte) (ContactSheetMeta, bool, error) {
	jpeg, err := contactSheetFinalPath(root, sha)
	if err != nil {
		return ContactSheetMeta{}, false, err
	}
	info, err := os.Lstat(jpeg)
	if err != nil {
		if os.IsNotExist(err) {
			return ContactSheetMeta{}, false, nil
		}
		return ContactSheetMeta{}, false, fmt.Errorf("stat contact sheet JPEG: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return ContactSheetMeta{}, false, nil
	}
	sidecarInfo, err := os.Lstat(jpeg + ".json")
	if err != nil {
		if os.IsNotExist(err) {
			return ContactSheetMeta{}, false, nil
		}
		return ContactSheetMeta{}, false, fmt.Errorf("stat contact sheet sidecar: %w", err)
	}
	if !sidecarInfo.Mode().IsRegular() {
		return ContactSheetMeta{}, false, nil
	}
	raw, err := os.ReadFile(jpeg + ".json")
	if err != nil {
		if os.IsNotExist(err) || contactSheetTransientReadError(err) {
			return ContactSheetMeta{}, false, nil
		}
		return ContactSheetMeta{}, false, fmt.Errorf("read contact sheet sidecar: %w", err)
	}
	var meta ContactSheetMeta
	if err := json.Unmarshal(raw, &meta); err != nil || !validContactSheetMeta(meta, sha) {
		return ContactSheetMeta{}, false, nil
	}
	digest, err := fileSHA256Hex(jpeg)
	if err != nil {
		if os.IsNotExist(err) || contactSheetTransientReadError(err) {
			return ContactSheetMeta{}, false, nil
		}
		return ContactSheetMeta{}, false, fmt.Errorf("hash contact sheet JPEG: %w", err)
	}
	if digest != meta.JPEGSHA256 {
		return ContactSheetMeta{}, false, nil
	}
	return meta, true, nil
}

// A reader racing a Windows replacement can briefly see a sharing or lock
// violation. It is an incomplete cache combination, not a cache hit or error.
func contactSheetTransientReadError(err error) bool {
	return errors.Is(err, syscall.Errno(32)) || errors.Is(err, syscall.Errno(33))
}

func publishContactSheet(paths ContactSheetPaths, meta ContactSheetMeta, validateSource func() error) error {
	return publishContactSheetWithHook(paths, meta, validateSource, nil)
}

func publishContactSheetWithHook(paths ContactSheetPaths, meta ContactSheetMeta, validateSource func() error, afterJPEG func() error) (result error) {
	contactSheetPublishMu.Lock()
	defer contactSheetPublishMu.Unlock()

	if err := validContactSheetPaths(paths); err != nil {
		return err
	}
	if validateSource == nil {
		return fmt.Errorf("contact sheet source validator is nil")
	}
	lock, err := lockContactSheetPublish(paths.JPEG)
	if err != nil {
		return err
	}
	defer func() {
		if err := lock.Release(); result == nil && err != nil {
			result = err
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
	_, statErr := file.Stat()
	if statErr == nil {
		statErr = file.Sync()
	}
	closeErr := file.Close()
	if statErr != nil {
		return statErr
	}
	if closeErr != nil {
		return fmt.Errorf("close contact sheet temp JPEG: %w", closeErr)
	}

	digest, err := fileSHA256Hex(paths.TempJPEG)
	if err != nil {
		return fmt.Errorf("hash contact sheet temp JPEG: %w", err)
	}
	meta.JPEGSHA256 = digest
	sha, err := contactSheetSHAFromJPEGPath(paths.JPEG)
	if err != nil {
		return err
	}
	if !validContactSheetMeta(meta, sha) {
		return fmt.Errorf("contact sheet metadata is incomplete or invalid")
	}
	if err := validateSource(); err != nil {
		return err
	}
	if err := atomicReplace(paths.TempJPEG, paths.JPEG); err != nil {
		return fmt.Errorf("commit contact sheet JPEG: %w", err)
	}
	if afterJPEG != nil {
		if err := afterJPEG(); err != nil {
			return err
		}
	}
	if err := writeContactSheetSidecar(paths.TempSidecar, meta); err != nil {
		return err
	}
	if err := atomicReplace(paths.TempSidecar, paths.Sidecar); err != nil {
		return fmt.Errorf("commit contact sheet sidecar: %w", err)
	}
	return nil
}

func cleanContactSheetStaleTemps(paths ContactSheetPaths, olderThan time.Time) error {
	if err := validContactSheetPaths(paths); err != nil {
		return err
	}
	directory := filepath.Dir(paths.JPEG)
	entries, err := os.ReadDir(directory)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("list contact sheet temps: %w", err)
	}
	base := filepath.Base(paths.JPEG)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, base+".tmp-") && !strings.HasPrefix(name, base+".json.tmp-") {
			continue
		}
		if !contactSheetTempName.MatchString(name) || entry.Type()&os.ModeSymlink != 0 {
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
		if err := os.Remove(filepath.Join(directory, name)); err != nil && !os.IsNotExist(err) {
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
	cacheDirectory, err := contactSheetPathUnderRoot(absoluteRoot, contactSheetPipeline)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(cacheDirectory); err == nil {
		if err := ensureContactSheetDirectory(absoluteRoot, cacheDirectory); err != nil {
			return "", err
		}
	}
	shaDirectory, err := contactSheetPathUnderRoot(absoluteRoot, contactSheetPipeline, shaHex[:2])
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(shaDirectory); err == nil {
		if err := ensureContactSheetDirectory(absoluteRoot, shaDirectory); err != nil {
			return "", err
		}
	}
	return contactSheetPathUnderRoot(absoluteRoot, contactSheetPipeline, shaHex[:2], shaHex+".jpg")
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
	sha, err := contactSheetSHAFromJPEGPath(paths.JPEG)
	if err != nil {
		return err
	}
	if paths.Sidecar != paths.JPEG+".json" || filepath.Dir(paths.TempJPEG) != filepath.Dir(paths.JPEG) || filepath.Dir(paths.TempSidecar) != filepath.Dir(paths.JPEG) {
		return fmt.Errorf("contact sheet paths are not colocated")
	}
	prefix := paths.JPEG + ".tmp-"
	sidecarPrefix := paths.JPEG + ".json.tmp-"
	if !strings.HasPrefix(paths.TempJPEG, prefix) || !strings.HasPrefix(paths.TempSidecar, sidecarPrefix) {
		return fmt.Errorf("contact sheet temp paths are invalid")
	}
	identifier := strings.TrimPrefix(paths.TempJPEG, prefix)
	if identifier == "" || strings.TrimPrefix(paths.TempSidecar, sidecarPrefix) != identifier {
		return fmt.Errorf("contact sheet temp paths do not match")
	}
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
	_ = sha
	return nil
}

func contactSheetSHAFromJPEGPath(jpeg string) ([64]byte, error) {
	var sha [64]byte
	name := filepath.Base(jpeg)
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
	if filepath.Base(filepath.Dir(jpeg)) != encoded[:2] || filepath.Base(filepath.Dir(filepath.Dir(jpeg))) != contactSheetPipeline {
		return sha, fmt.Errorf("contact sheet JPEG path does not use cache layout")
	}
	return sha, nil
}

func validContactSheetMeta(meta ContactSheetMeta, sha [64]byte) bool {
	if meta.SchemaVersion != 1 || meta.Pipeline != contactSheetPipeline || meta.SourceSHA512 != hex.EncodeToString(sha[:]) || meta.SourceSize < 0 || !validSHA256Hex(meta.JPEGSHA256) {
		return false
	}
	if meta.CanvasWidth <= 0 || meta.CanvasHeight <= 0 || meta.TileWidth <= 0 || meta.TileHeight <= 0 || !validContactSheetVersion(meta.VideoCoreVersion) {
		return false
	}
	for _, sample := range meta.Samples {
		if sample.TimeMS < 0 || strings.TrimSpace(sample.Status) == "" || strings.TrimSpace(sample.Status) != sample.Status {
			return false
		}
	}
	expectedNames := [4]string{"avformat", "avcodec", "avutil", "swscale"}
	for index, component := range meta.FFmpeg {
		if component.Name != expectedNames[index] || component.HeaderVersion == 0 || component.RuntimeVersion == 0 {
			return false
		}
	}
	return true
}

func validContactSheetVersion(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\r\n\x00")
}

func writeContactSheetSidecar(path string, meta ContactSheetMeta) error {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("contact sheet sidecar temp is not a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("stat contact sheet sidecar temp: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open contact sheet sidecar temp: %w", err)
	}
	encoder := json.NewEncoder(file)
	encodeErr := encoder.Encode(meta)
	if encodeErr == nil {
		encodeErr = file.Sync()
	}
	closeErr := file.Close()
	if encodeErr != nil {
		return fmt.Errorf("write contact sheet sidecar temp: %w", encodeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close contact sheet sidecar temp: %w", closeErr)
	}
	return nil
}
