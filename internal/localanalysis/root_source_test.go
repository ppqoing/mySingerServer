package localanalysis

import (
	"context"
	"strings"
	"testing"

	"dedup/internal/firstscreen"
)

type rootSourceFixture struct{ files []firstscreen.File }

func (s rootSourceFixture) StreamActiveFiles(_ context.Context, _ string, visit func(firstscreen.File) error) error {
	for _, file := range s.files {
		if err := visit(file); err != nil {
			return err
		}
	}
	return nil
}

func (rootSourceFixture) LoadImageFeatures(context.Context, []string) (map[string]firstscreen.ImageFeature, error) {
	return nil, nil
}

func (rootSourceFixture) LoadVideoFeatures(context.Context, []string) (map[string]firstscreen.VideoFeature, error) {
	return nil, nil
}

func TestRootScopedCandidateSourceStreamsOnlyFilesWithinRoots(t *testing.T) {
	source := rootSourceFixture{files: []firstscreen.File{
		{FileRef: firstscreen.FileRef{ID: 1, MachineID: "m", Path: `I:\tmp\wallpa\a.mp4`}},
		{FileRef: firstscreen.FileRef{ID: 2, MachineID: "m", Path: `i:\TMP\WALLPA\nested\b.mp4`}},
		{FileRef: firstscreen.FileRef{ID: 3, MachineID: "m", Path: `I:\tmp\wallpaper\c.mp4`}},
		{FileRef: firstscreen.FileRef{ID: 4, MachineID: "m", Path: `H:\pik\d.jpg`}},
	}}
	scoped, err := newRootScopedCandidateSource(source, []string{`I:\tmp\wallpa`})
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	err = scoped.StreamActiveFiles(context.Background(), "m", func(file firstscreen.File) error {
		got = append(got, file.ID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("streamed IDs = %v, want [1 2]", got)
	}
}

func TestRootScopedCandidateSourceRejectsInvalidRootsAndEscapingPaths(t *testing.T) {
	tests := []struct {
		name  string
		roots []string
		files []firstscreen.File
	}{
		{name: "empty roots"},
		{name: "relative root", roots: []string{`tmp\wallpa`}},
		{name: "drive root", roots: []string{`I:\`}},
		{name: "parent component root", roots: []string{`I:\tmp\wallpa\..\wallpaper`}},
		{name: "relative file", roots: []string{`I:\tmp\wallpa`}, files: []firstscreen.File{{FileRef: firstscreen.FileRef{ID: 5, Path: `..\outside.mp4`}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scoped, err := newRootScopedCandidateSource(rootSourceFixture{files: test.files}, test.roots)
			if err != nil {
				return
			}
			err = scoped.StreamActiveFiles(context.Background(), "m", func(firstscreen.File) error { return nil })
			if err == nil || !strings.Contains(err.Error(), "root") {
				t.Fatalf("StreamActiveFiles error = %v, want root-scope error", err)
			}
		})
	}
}
