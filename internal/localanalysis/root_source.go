package localanalysis

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"dedup/internal/firstscreen"
)

type rootScopedCandidateSource struct {
	source firstscreen.CandidateSource
	roots  []string
}

func newRootScopedCandidateSource(source firstscreen.CandidateSource, roots []string) (*rootScopedCandidateSource, error) {
	normalized, err := validateTaskRoots(roots)
	if err != nil {
		return nil, err
	}
	return &rootScopedCandidateSource{source: source, roots: normalized}, nil
}

func validateTaskRoots(roots []string) ([]string, error) {
	if len(roots) == 0 {
		return nil, fmt.Errorf("localanalysis: task roots are required")
	}
	normalized := make([]string, len(roots))
	for index, root := range roots {
		if hasParentPathComponent(root) {
			return nil, fmt.Errorf("localanalysis: task root must not contain a parent component")
		}
		clean := filepath.Clean(root)
		if !filepath.IsAbs(clean) {
			return nil, fmt.Errorf("localanalysis: task root must be absolute")
		}
		volume := filepath.VolumeName(clean)
		if volume == "" || strings.EqualFold(clean, volume+string(filepath.Separator)) {
			return nil, fmt.Errorf("localanalysis: task root must not be a drive root")
		}
		normalized[index] = strings.ToLower(clean)
	}
	return normalized, nil
}

func hasParentPathComponent(path string) bool {
	for _, component := range strings.FieldsFunc(path, func(r rune) bool { return r == '\\' || r == '/' }) {
		if component == ".." {
			return true
		}
	}
	return false
}

func (s *rootScopedCandidateSource) StreamActiveFiles(ctx context.Context, machineID string, visit func(firstscreen.File) error) error {
	return s.source.StreamActiveFiles(ctx, machineID, func(file firstscreen.File) error {
		within, err := pathWithinTaskRoots(file.Path, s.roots)
		if err != nil {
			return err
		}
		if within {
			return visit(file)
		}
		return nil
	})
}

func (s *rootScopedCandidateSource) LoadImageFeatures(ctx context.Context, hashes []string) (map[string]firstscreen.ImageFeature, error) {
	return s.source.LoadImageFeatures(ctx, hashes)
}

func (s *rootScopedCandidateSource) LoadVideoFeatures(ctx context.Context, hashes []string) (map[string]firstscreen.VideoFeature, error) {
	return s.source.LoadVideoFeatures(ctx, hashes)
}

func pathWithinTaskRoots(path string, roots []string) (bool, error) {
	cleanPath := filepath.Clean(path)
	if !filepath.IsAbs(cleanPath) {
		return false, fmt.Errorf("localanalysis: task root scope requires absolute file paths")
	}
	cleanPath = strings.ToLower(cleanPath)
	for _, root := range roots {
		if !strings.EqualFold(filepath.VolumeName(root), filepath.VolumeName(cleanPath)) {
			continue
		}
		rel, err := filepath.Rel(root, cleanPath)
		if err != nil {
			return false, fmt.Errorf("localanalysis: task root scope: %w", err)
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)) {
			return true, nil
		}
	}
	return false, nil
}
