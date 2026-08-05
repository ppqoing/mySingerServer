package wproc

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var errThumbnailPublishConflict = fmt.Errorf("thumbnail cache publish conflict")

type thumbMeta struct {
	MTimeUnix  int64  `json:"mtime_unix"`
	Size       int64  `json:"size"`
	JPEGSHA256 string `json:"jpeg_sha256"`
}

func thumbCacheKey(path string) (string, error) {
	return thumbCacheKeyWithAbs(path, filepath.Abs)
}

func thumbCacheKeyWithAbs(path string, absolutePath func(string) (string, error)) (string, error) {
	absolute, err := absolutePath(path)
	if err != nil {
		return "", fmt.Errorf("absolute thumbnail source path: %w", err)
	}
	normalized := strings.ToLower(filepath.Clean(absolute))
	sum := sha1.Sum([]byte(normalized))
	return hex.EncodeToString(sum[:]), nil
}

func thumbPathFor(cfg Config, source string) (string, error) {
	key, err := thumbCacheKey(source)
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg.ThumbCacheDir, key[:2], key+".jpg"), nil
}

func thumbCacheLookup(cfg Config, source string, info fs.FileInfo) (string, bool, error) {
	thumb, hit, _, err := thumbCacheLookupWithDigest(cfg, source, info)
	return thumb, hit, err
}

func thumbCacheLookupWithDigest(cfg Config, source string, info fs.FileInfo) (string, bool, string, error) {
	thumb, err := thumbPathFor(cfg, source)
	if err != nil {
		return "", false, "", err
	}
	if err := thumbCacheCleanStaleTemps(thumb, time.Now().Add(-time.Hour)); err != nil {
		return thumb, false, "", err
	}
	thumbInfo, err := os.Stat(thumb)
	if err != nil {
		if os.IsNotExist(err) {
			return thumb, false, "", nil
		}
		return thumb, false, "", fmt.Errorf("stat thumbnail: %w", err)
	}
	if !thumbInfo.Mode().IsRegular() || thumbInfo.Size() == 0 {
		return thumb, false, "", nil
	}
	raw, err := os.ReadFile(thumb + ".json")
	if err != nil {
		if os.IsNotExist(err) {
			return thumb, false, "", nil
		}
		return thumb, false, "", fmt.Errorf("read thumbnail sidecar: %w", err)
	}
	var meta thumbMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return thumb, false, "", nil
	}
	if meta.MTimeUnix != info.ModTime().Unix() || meta.Size != info.Size() {
		return thumb, false, "", nil
	}
	if !validSHA256Hex(meta.JPEGSHA256) {
		return thumb, false, "", nil
	}
	actual, err := fileSHA256Hex(thumb)
	if err != nil {
		if os.IsNotExist(err) {
			return thumb, false, "", nil
		}
		return thumb, false, "", fmt.Errorf("hash thumbnail: %w", err)
	}
	if actual != meta.JPEGSHA256 {
		return thumb, false, "", nil
	}
	return thumb, true, meta.JPEGSHA256, nil
}

func thumbCacheWriteMeta(cfg Config, source string, info fs.FileInfo, expectedJPEG string) error {
	if !validSHA256Hex(expectedJPEG) {
		return fmt.Errorf("expected thumbnail SHA-256 is invalid")
	}
	thumb, err := thumbPathFor(cfg, source)
	if err != nil {
		return err
	}
	thumbInfo, err := os.Stat(thumb)
	if err != nil {
		return fmt.Errorf("thumbnail must be committed before sidecar: %w", err)
	}
	if !thumbInfo.Mode().IsRegular() || thumbInfo.Size() == 0 {
		return fmt.Errorf("thumbnail must be a non-empty regular file before sidecar")
	}
	actual, err := fileSHA256Hex(thumb)
	if err != nil {
		return fmt.Errorf("hash thumbnail before sidecar: %w", err)
	}
	if actual != expectedJPEG {
		return fmt.Errorf("%w: expected %s, found %s", errThumbnailPublishConflict, expectedJPEG, actual)
	}
	if err := os.MkdirAll(filepath.Dir(thumb), 0o755); err != nil {
		return fmt.Errorf("create thumbnail directory: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(thumb), filepath.Base(thumb)+".json.tmp-*")
	if err != nil {
		return fmt.Errorf("create sidecar temp: %w", err)
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		_ = temp.Close()
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()

	encoder := json.NewEncoder(temp)
	if err := encoder.Encode(thumbMeta{
		MTimeUnix: info.ModTime().Unix(), Size: info.Size(), JPEGSHA256: expectedJPEG,
	}); err != nil {
		return fmt.Errorf("encode sidecar: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync sidecar temp: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close sidecar temp: %w", err)
	}
	actual, err = fileSHA256Hex(thumb)
	if err != nil {
		return fmt.Errorf("hash thumbnail before sidecar commit: %w", err)
	}
	if actual != expectedJPEG {
		return fmt.Errorf("%w: expected %s, found %s", errThumbnailPublishConflict, expectedJPEG, actual)
	}
	if err := atomicReplace(tempPath, thumb+".json"); err != nil {
		return fmt.Errorf("commit sidecar: %w", err)
	}
	committed = true
	return nil
}

func bytesSHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func fileSHA256Hex(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func thumbCacheCleanStaleTemps(thumb string, olderThan time.Time) error {
	matches, err := filepath.Glob(thumb + ".tmp-*")
	if err != nil {
		return fmt.Errorf("list thumbnail temps: %w", err)
	}
	sidecars, err := filepath.Glob(thumb + ".json.tmp-*")
	if err != nil {
		return fmt.Errorf("list sidecar temps: %w", err)
	}
	matches = append(matches, sidecars...)
	for _, path := range matches {
		info, statErr := os.Stat(path)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return fmt.Errorf("stat cache temp: %w", statErr)
		}
		if !info.ModTime().Before(olderThan) {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale cache temp: %w", err)
		}
	}
	return nil
}
