package agent

import (
	"crypto/sha512"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dedup/internal/proto"
)

func TestGoHasherMatchesSHA512ReferenceAcrossBlocks(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "empty", data: nil},
		{name: "abc", data: []byte("abc")},
		{name: "crosses 4MB blocks", data: makePattern(10 << 20)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "input.bin")
			if err := os.WriteFile(path, tt.data, 0o600); err != nil {
				t.Fatal(err)
			}
			sum := sha512.Sum512(tt.data)
			want := hex.EncodeToString(sum[:])

			got, err := (GoHasher{}).HashFile(path)
			if err != nil {
				t.Fatalf("HashFile: %v", err)
			}
			if got != want {
				t.Fatalf("hash = %s, want %s", got, want)
			}
		})
	}
}

func TestLongPathPrefixUsesUNCDeviceSyntax(t *testing.T) {
	path := `\\server\share\` + strings.Repeat(`directory\`, 30) + "file.bin"
	want := `\\?\UNC\server\share\` + strings.Repeat(`directory\`, 30) + "file.bin"
	if got := longPathPrefix(path); got != want {
		t.Fatalf("longPathPrefix(%q) = %q, want %q", path, got, want)
	}
}

func TestDefaultStageOneMediaKindAndMissingBaseAreCaseInsensitive(t *testing.T) {
	tests := []struct {
		path string
		kind string
		mask uint32
	}{
		{path: `D:\照片\A.JPG`, kind: "image", mask: 3},
		{
			path: `D:\video\a.mkv`, kind: "video",
			mask: proto.FieldSHA512 | proto.FieldVideoDuration |
				proto.FieldVideoContactSheet | proto.FieldVideoMetadata,
		},
		{path: `D:\other\a.txt`, kind: "other", mask: 1},
	}
	for _, tt := range tests {
		if got := MediaKind(tt.path); got != tt.kind {
			t.Errorf("MediaKind(%q) = %q, want %q", tt.path, got, tt.kind)
		}
		if got := MissingBase(tt.path); got != tt.mask {
			t.Errorf("MissingBase(%q) = %06b, want %06b", tt.path, got, tt.mask)
		}
	}
}

func TestMediaKindWithExtensionsReplacesDefaultTables(t *testing.T) {
	imageExts := []string{".raw"}
	videoExts := []string{".movie"}
	if got := MediaKindWithExtensions("photo.RAW", imageExts, videoExts); got != "image" {
		t.Fatalf("custom image kind = %q, want image", got)
	}
	if got := MediaKindWithExtensions("clip.movie", imageExts, videoExts); got != "video" {
		t.Fatalf("custom video kind = %q, want video", got)
	}
	if got := MediaKindWithExtensions("default.jpg", imageExts, videoExts); got != "other" {
		t.Fatalf("default extension survived replacement: %q", got)
	}
}

func makePattern(size int) []byte {
	out := make([]byte, size)
	for i := range out {
		out[i] = byte((i*31 + 7) % 251)
	}
	return out
}
