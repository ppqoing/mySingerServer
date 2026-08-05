package m6bench

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	CorpusMarker   = ".m6-corpus-owner.json"
	corpusManifest = ".m6-corpus-manifest.json"
)

var protectedCorpusRoots = []string{
	`I:\tmp`,
	`H:\pik\00000000000`,
}

type CorpusConfig struct {
	Root            string
	Files           int
	DuplicateGroups int
	SparseFiles     int
	Seed            uint64
	RunID           string
}

type CorpusFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
	Sparse bool   `json:"sparse,omitempty"`
}

type CorpusManifest struct {
	SchemaVersion   int          `json:"schema_version"`
	RunID           string       `json:"run_id"`
	Root            string       `json:"root"`
	Seed            uint64       `json:"seed"`
	DuplicateGroups int          `json:"duplicate_groups"`
	Files           []CorpusFile `json:"files"`
}

type corpusOwner struct {
	SchemaVersion int       `json:"schema_version"`
	RunID         string    `json:"run_id"`
	Root          string    `json:"root"`
	CreatedAt     time.Time `json:"created_at"`
}

func GenerateCorpus(
	ctx context.Context,
	cfg CorpusConfig,
) (CorpusManifest, error) {
	root, err := validateCorpusRoot(cfg.Root)
	if err != nil {
		return CorpusManifest{}, err
	}
	if cfg.Files < 1 || cfg.Files > 2_000_000 ||
		cfg.DuplicateGroups < 0 || cfg.DuplicateGroups*2 > cfg.Files ||
		cfg.SparseFiles < 0 || cfg.SparseFiles > 10_000 ||
		!syncRunIDPattern.MatchString(cfg.RunID) {
		return CorpusManifest{}, fmt.Errorf("corpusgen: invalid bounded configuration")
	}
	if err := prepareCorpusRoot(root, cfg.RunID); err != nil {
		return CorpusManifest{}, err
	}
	owner := corpusOwner{
		SchemaVersion: SchemaVersion,
		RunID:         cfg.RunID,
		Root:          root,
		CreatedAt:     time.Now().UTC(),
	}
	if err := WriteJSON(filepath.Join(root, CorpusMarker), owner); err != nil {
		return CorpusManifest{}, fmt.Errorf("corpusgen: write marker: %w", err)
	}

	manifest := CorpusManifest{
		SchemaVersion:   SchemaVersion,
		RunID:           cfg.RunID,
		Root:            root,
		Seed:            cfg.Seed,
		DuplicateGroups: cfg.DuplicateGroups,
		Files:           make([]CorpusFile, 0, cfg.Files+cfg.SparseFiles),
	}
	filesDir := filepath.Join(root, "files")
	if err := os.MkdirAll(filesDir, 0o755); err != nil {
		return CorpusManifest{}, err
	}
	for index := 0; index < cfg.Files; index++ {
		if index&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return CorpusManifest{}, err
			}
		}
		contentIndex := index
		if index < cfg.DuplicateGroups*2 {
			contentIndex = index / 2
		}
		content := corpusBytes(cfg.Seed, contentIndex)
		relative := filepath.Join("files", fmt.Sprintf("%09d.bin", index))
		path := filepath.Join(root, relative)
		if err := os.WriteFile(path, content, 0o600); err != nil {
			return CorpusManifest{}, fmt.Errorf("corpusgen: write %q: %w", relative, err)
		}
		sum := sha256.Sum256(content)
		manifest.Files = append(manifest.Files, CorpusFile{
			Path: relative, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:]),
		})
	}
	if cfg.SparseFiles > 0 {
		sparseDir := filepath.Join(root, "sparse")
		if err := os.MkdirAll(sparseDir, 0o755); err != nil {
			return CorpusManifest{}, err
		}
		for index := 0; index < cfg.SparseFiles; index++ {
			relative := filepath.Join("sparse", fmt.Sprintf("%05d.sparse", index))
			path := filepath.Join(root, relative)
			file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return CorpusManifest{}, err
			}
			size := int64(64 << 20)
			truncateErr := file.Truncate(size)
			closeErr := file.Close()
			if truncateErr != nil {
				return CorpusManifest{}, truncateErr
			}
			if closeErr != nil {
				return CorpusManifest{}, closeErr
			}
			manifest.Files = append(manifest.Files, CorpusFile{
				Path: relative, Size: size, Sparse: true,
			})
		}
	}
	if err := WriteJSON(filepath.Join(root, corpusManifest), manifest); err != nil {
		return CorpusManifest{}, fmt.Errorf("corpusgen: write manifest: %w", err)
	}
	return manifest, nil
}

func CleanCorpus(root, expectedRunID string) error {
	validated, err := validateCorpusRoot(root)
	if err != nil {
		return err
	}
	owner, err := loadCorpusOwner(validated)
	if err != nil {
		return err
	}
	if owner.RunID != expectedRunID {
		return fmt.Errorf("corpusgen: ownership run ID mismatch")
	}
	var manifest CorpusManifest
	data, err := os.ReadFile(filepath.Join(validated, corpusManifest))
	if err != nil {
		return fmt.Errorf("corpusgen: read manifest: %w", err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("corpusgen: decode manifest: %w", err)
	}
	if manifest.RunID != expectedRunID ||
		!strings.EqualFold(filepath.Clean(manifest.Root), validated) {
		return fmt.Errorf("corpusgen: manifest ownership mismatch")
	}
	for _, entry := range manifest.Files {
		path, err := ownedCorpusPath(validated, entry.Path)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("corpusgen: remove %q: %w", entry.Path, err)
		}
	}
	_ = os.Remove(filepath.Join(validated, "files"))
	_ = os.Remove(filepath.Join(validated, "sparse"))
	if err := os.Remove(filepath.Join(validated, corpusManifest)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(filepath.Join(validated, CorpusMarker)); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(validated)
	return nil
}

func corpusBytes(seed uint64, index int) []byte {
	var input [16]byte
	binary.LittleEndian.PutUint64(input[:8], seed)
	binary.LittleEndian.PutUint64(input[8:], uint64(index))
	sum := sha256.Sum256(input[:])
	return bytes.Repeat(sum[:], 128)
}

func validateCorpusRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("corpusgen: root is required")
	}
	if strings.HasPrefix(root, `\\`) {
		return "", fmt.Errorf("corpusgen: UNC roots are forbidden")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	volumeRoot := filepath.VolumeName(absolute) + string(filepath.Separator)
	if strings.EqualFold(absolute, volumeRoot) {
		return "", fmt.Errorf("corpusgen: drive roots are forbidden")
	}
	working, err := os.Getwd()
	if err == nil {
		working, _ = filepath.Abs(working)
		if strings.EqualFold(absolute, filepath.Clean(working)) {
			return "", fmt.Errorf("corpusgen: workspace root is forbidden")
		}
	}
	for _, protected := range protectedCorpusRoots {
		protected = filepath.Clean(protected)
		if strings.EqualFold(absolute, protected) ||
			strings.HasPrefix(strings.ToLower(absolute), strings.ToLower(protected)+string(filepath.Separator)) {
			return "", fmt.Errorf("corpusgen: protected media root is read-only")
		}
	}
	for current := absolute; ; current = filepath.Dir(current) {
		info, statErr := os.Lstat(current)
		if statErr == nil {
			if info.Mode()&fs.ModeSymlink != 0 {
				return "", fmt.Errorf("corpusgen: reparse/symlink root is forbidden")
			}
			break
		}
		if !os.IsNotExist(statErr) {
			return "", statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return absolute, nil
}

func prepareCorpusRoot(root, runID string) error {
	info, err := os.Stat(root)
	if os.IsNotExist(err) {
		return os.MkdirAll(root, 0o755)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("corpusgen: root is not a directory")
	}
	owner, ownerErr := loadCorpusOwner(root)
	if ownerErr == nil {
		if owner.RunID != runID {
			return fmt.Errorf("corpusgen: existing corpus belongs to another run")
		}
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("corpusgen: non-empty unmarked directory is forbidden")
	}
	return nil
}

func loadCorpusOwner(root string) (corpusOwner, error) {
	var owner corpusOwner
	data, err := os.ReadFile(filepath.Join(root, CorpusMarker))
	if err != nil {
		return owner, fmt.Errorf("corpusgen: read ownership marker: %w", err)
	}
	if err := json.Unmarshal(data, &owner); err != nil {
		return owner, fmt.Errorf("corpusgen: decode ownership marker: %w", err)
	}
	if owner.RunID == "" || !strings.EqualFold(filepath.Clean(owner.Root), filepath.Clean(root)) {
		return owner, fmt.Errorf("corpusgen: invalid ownership marker")
	}
	return owner, nil
}

func ownedCorpusPath(root, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) {
		return "", fmt.Errorf("corpusgen: invalid manifest path")
	}
	path := filepath.Clean(filepath.Join(root, relative))
	if !strings.HasPrefix(strings.ToLower(path), strings.ToLower(root)+string(filepath.Separator)) {
		return "", fmt.Errorf("corpusgen: manifest path escapes root")
	}
	return path, nil
}
