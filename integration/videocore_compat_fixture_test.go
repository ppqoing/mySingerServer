package integration

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	compatManifestSHA256 = "0450e25cc58b02b4d0de0df90f31342aadd2ec5a3276a0412ee565d321dd6e5a"
	compatGoldenSHA256   = "7657c39c4a43134bb79e622e1d9ea9c0123b39b5f4f0b5e9e98e092728876785"
)

var approvedLegacyComponents = map[string]compatGeneratorComponent{
	"image-feature-library": {
		Role: "image-feature-library", Path: "mediacore.dll",
		SHA256:  "2260110367bf43b368cbfc70dbdb316556588b5a60eb832f699292352ab463df",
		Version: "1.0.0",
	},
	"frame-extractor": {
		Role: "frame-extractor", Path: "tools/ffmpeg.exe",
		SHA256:  "5f3c767af1cdbb9c44ad14478ce5fc036aec20e6a724755caa2f70abb9655c3f",
		Version: "ffmpeg version N-125444-g6d72600a30-20260703 Copyright (c) 2000-2026 the FFmpeg developers",
	},
	"media-probe": {
		Role: "media-probe", Path: "tools/ffprobe.exe",
		SHA256:  "5d54bcd31343e6b0471bccc2159fa324af2af3ef986474343f572872e9fbeaac",
		Version: "ffprobe version N-125444-g6d72600a30-20260703 Copyright (c) 2007-2026 the FFmpeg developers",
	},
}

type compatManifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	Images        []compatFixture `json:"images"`
	Videos        []compatFixture `json:"videos"`
}

type compatFixture struct {
	Path           string   `json:"path"`
	SHA256         string   `json:"sha256"`
	MediaType      string   `json:"mediaType"`
	Codec          string   `json:"codec"`
	DurationMicros int64    `json:"durationMicros"`
	Rotation       int      `json:"rotation"`
	SAR            string   `json:"sar"`
	Scenarios      []string `json:"scenarios"`
}

type compatGolden struct {
	SchemaVersion          int                   `json:"schemaVersion"`
	Generator              compatGoldenGenerator `json:"generator"`
	StandardSampleMicros   []int64               `json:"standardSampleMicros"`
	ApprovedSemanticDeltas []compatSemanticDelta `json:"approvedSemanticDeltas"`
	Fixtures               []compatGoldenFixture `json:"fixtures"`
}

type compatGoldenGenerator struct {
	Kind       string                     `json:"kind"`
	Components []compatGeneratorComponent `json:"components"`
}

type compatGeneratorComponent struct {
	Role    string `json:"role"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Version string `json:"version"`
}

type compatSemanticDelta struct {
	ID                  string `json:"id"`
	FixturePath         string `json:"fixturePath"`
	Approval            string `json:"approval"`
	LegacyBehavior      string `json:"legacyBehavior"`
	LegacyDisplayWidth  int    `json:"legacyDisplayWidth"`
	LegacyDisplayHeight int    `json:"legacyDisplayHeight"`
	FutureBehavior      string `json:"futureBehavior"`
	FutureDisplayWidth  int    `json:"futureDisplayWidth"`
	FutureDisplayHeight int    `json:"futureDisplayHeight"`
}

type compatGoldenFixture struct {
	Path   string              `json:"path"`
	SHA512 string              `json:"sha512"`
	Image  *compatImageGolden  `json:"image,omitempty"`
	Video  *compatVideoGolden  `json:"video,omitempty"`
	Error  *compatCaptureError `json:"error,omitempty"`
}

type compatImageGolden struct {
	Width             int      `json:"width"`
	Height            int      `json:"height"`
	PDQHex            string   `json:"pdqHex"`
	Quality           *int     `json:"quality"`
	PHashPartsHex     []string `json:"pHashPartsHex"`
	SobelFloatBitsHex []string `json:"sobelFloatBitsHex"`
}

type compatVideoGolden struct {
	DurationMicros    int64               `json:"durationMicros"`
	SampleTimesMicros []int64             `json:"sampleTimesMicros"`
	Source            compatSourceGolden  `json:"source"`
	Frames            []compatFrameGolden `json:"frames"`
}

type compatSourceGolden struct {
	StreamType string `json:"streamType"`
	Codec      string `json:"codec"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Rotation   int    `json:"rotation"`
	SAR        string `json:"sar"`
	HasBFrames int    `json:"hasBFrames"`
}

type compatFrameGolden struct {
	SampleIndex       int                  `json:"sampleIndex"`
	RequestedMicros   int64                `json:"requestedMicros"`
	SelectedIdentity  *compatFrameIdentity `json:"selectedIdentity,omitempty"`
	DisplayWidth      int                  `json:"displayWidth,omitempty"`
	DisplayHeight     int                  `json:"displayHeight,omitempty"`
	OutputSHA256      string               `json:"outputFrameSHA256,omitempty"`
	PDQHex            string               `json:"pdqHex,omitempty"`
	Quality           *int                 `json:"quality,omitempty"`
	PHashPartsHex     []string             `json:"pHashPartsHex,omitempty"`
	SobelFloatBitsHex []string             `json:"sobelFloatBitsHex,omitempty"`
	Error             *compatCaptureError  `json:"error,omitempty"`
}

type compatFrameIdentity struct {
	SourceDecodeOrdinal int    `json:"sourceDecodeOrdinal"`
	PTS                 int64  `json:"pts"`
	PTSTimeMicros       int64  `json:"ptsTimeMicros"`
	KeyFrame            bool   `json:"keyFrame"`
	PictureType         string `json:"pictureType"`
}

type compatCaptureError struct {
	Stage    string `json:"stage"`
	ExitCode *int   `json:"exitCode,omitempty"`
	Message  string `json:"message"`
}

type probeDocument struct {
	Programs     []json.RawMessage `json:"programs"`
	StreamGroups []json.RawMessage `json:"stream_groups"`
	Streams      []probeStream     `json:"streams"`
	Format       struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

type probeStream struct {
	Index      int    `json:"index"`
	CodecName  string `json:"codec_name"`
	CodecType  string `json:"codec_type"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	HasBFrames int    `json:"has_b_frames"`
	SAR        string `json:"sample_aspect_ratio"`
	SideData   []struct {
		Rotation int `json:"rotation"`
	} `json:"side_data_list"`
}

type probedFixture struct {
	Stream         probeStream
	Codec          string
	SAR            string
	Rotation       int
	DurationMicros int64
}

func TestVideoCoreCompatibilityFixturesAreImmutable(t *testing.T) {
	root := repositoryRoot(t)
	compatRoot := filepath.Join(root, "testdata", "videocore", "compat")
	manifestPath := filepath.Join(compatRoot, "manifest.json")
	goldenPath := filepath.Join(compatRoot, "legacy-golden.json")

	manifest := loadCompatManifest(t, manifestPath)
	if len(manifest.Images) < 3 || len(manifest.Videos) < 9 {
		t.Fatalf("compat fixture coverage is incomplete: images=%d videos=%d", len(manifest.Images), len(manifest.Videos))
	}
	verifyFixtureContract(t, manifest)
	verifyAllSHA256(t, compatRoot, manifest)
	probes := verifyProbeMetadata(t, root, compatRoot, manifest)
	golden := loadCompatGolden(t, goldenPath)
	legacyBin, legacyAvailable := prepareLegacyGenerator(t, root)
	t.Run("legacy generator runtime", func(t *testing.T) {
		if !legacyAvailable {
			t.Skip("legacy mediacore.dll is not available; frozen fixture and golden checks still run")
		}
		t.Run("capture rejects junction fixture path", func(t *testing.T) {
			verifyCaptureRejectsJunction(t, root, legacyBin, compatRoot, manifestPath)
		})
		t.Run("capture deadline leaves no partial golden", func(t *testing.T) {
			verifyCaptureDeadlineCleanup(t, root, legacyBin, compatRoot, manifestPath)
		})
		t.Run("capture timeout terminates helper process tree", func(t *testing.T) {
			verifyCaptureTerminatesHelperTree(t, root, legacyBin, compatRoot)
		})
		t.Run("capture atomic write cleans flushed temporary on failure", func(t *testing.T) {
			verifyCaptureAtomicFailureCleanup(t, root, legacyBin, compatRoot, manifestPath)
		})
		t.Run("capture success reproduces frozen golden", func(t *testing.T) {
			verifyCaptureSuccessReproducesFrozenGolden(t, root, legacyBin, compatRoot, manifestPath, goldenPath)
		})
		verifyLegacyComponents(t, legacyBin, golden.Generator)
	})
	for _, violation := range qualityPresenceViolations(golden) {
		t.Error(violation)
	}
	t.Run("quality presence mutations", func(t *testing.T) {
		verifyQualityPresenceMutations(t, golden)
	})
	verifyFileCheckpoint(t, manifestPath, compatManifestSHA256)
	verifyFileCheckpoint(t, goldenPath, compatGoldenSHA256)
	verifyGoldenCoversEveryFixture(t, compatRoot, manifest, golden)
	verifyGoldenMatchesProbe(t, golden, probes)
	verifyGoldenFeatureSemantics(t, manifest, golden)
	verifyApprovedSARDifference(t, golden)
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	return root
}

func prepareLegacyGenerator(t *testing.T, root string) (string, bool) {
	t.Helper()
	sources := map[string]string{
		"mediacore.dll":                       filepath.Join(root, "bin", "mediacore.dll"),
		filepath.Join("tools", "ffmpeg.exe"):  filepath.Join(root, "third_party", "ffmpeg", "bin", "ffmpeg.exe"),
		filepath.Join("tools", "ffprobe.exe"): filepath.Join(root, "third_party", "ffmpeg", "bin", "ffprobe.exe"),
	}
	for _, source := range sources {
		if _, err := os.Stat(source); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", false
			}
			t.Fatalf("stat legacy generator component %s: %v", source, err)
		}
	}
	legacyBin := filepath.Join(t.TempDir(), "legacy-bin")
	for relative, source := range sources {
		destination := filepath.Join(legacyBin, relative)
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			t.Fatal(err)
		}
		copyFile(t, source, destination)
	}
	return legacyBin, true
}

func loadCompatManifest(t *testing.T, path string) compatManifest {
	t.Helper()
	var manifest compatManifest
	decodeStrictJSONFile(t, path, &manifest)
	if manifest.SchemaVersion != 1 {
		t.Fatalf("manifest schemaVersion=%d, want 1", manifest.SchemaVersion)
	}
	return manifest
}

func loadCompatGolden(t *testing.T, path string) compatGolden {
	t.Helper()
	var golden compatGolden
	decodeStrictJSONFile(t, path, &golden)
	if golden.SchemaVersion != 1 {
		t.Fatalf("legacy golden schemaVersion=%d, want 1", golden.SchemaVersion)
	}
	return golden
}

func decodeStrictJSONFile(t *testing.T, path string, destination any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	decodeStrictJSON(t, path, data, destination)
}

func decodeStrictJSON(t *testing.T, label string, data []byte, destination any) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		t.Fatalf("decode %s: %v", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("decode %s: trailing JSON value", label)
	}
}

func verifyFixtureContract(t *testing.T, manifest compatManifest) {
	t.Helper()
	requiredScenarios := map[string]bool{
		"jpeg": false, "png": false, "webp": false,
		"h264": false, "b_frames": false, "rotation_90": false,
		"non_square_sar": false, "short_video": false, "portrait": false,
		"vp9": false, "hevc": false, "audio_only": false,
		"truncated_container": false, "corrupt_packet": false,
	}
	seenPaths := make(map[string]struct{})
	for _, fixture := range allFixtures(manifest) {
		verifyLowerHex(t, fixture.Path+" manifest SHA-256", fixture.SHA256, sha256.Size*2)
		if fixture.Path == "" || fixture.MediaType == "" || fixture.Codec == "" ||
			fixture.SAR == "" || len(fixture.Scenarios) == 0 {
			t.Errorf("fixture %q has incomplete immutable metadata", fixture.Path)
		}
		clean := filepath.ToSlash(filepath.Clean(fixture.Path))
		if filepath.IsAbs(fixture.Path) || clean == ".." || strings.HasPrefix(clean, "../") {
			t.Errorf("fixture path %q is not a safe relative path", fixture.Path)
		}
		if _, exists := seenPaths[fixture.Path]; exists {
			t.Errorf("fixture path %q is duplicated", fixture.Path)
		}
		seenPaths[fixture.Path] = struct{}{}
		for _, scenario := range fixture.Scenarios {
			if _, required := requiredScenarios[scenario]; required {
				requiredScenarios[scenario] = true
			}
		}
	}
	var missing []string
	for scenario, present := range requiredScenarios {
		if !present {
			missing = append(missing, scenario)
		}
	}
	sort.Strings(missing)
	if len(missing) != 0 {
		t.Errorf("compat manifest is missing required scenarios: %s", strings.Join(missing, ", "))
	}
}

func allFixtures(manifest compatManifest) []compatFixture {
	return append(append([]compatFixture(nil), manifest.Images...), manifest.Videos...)
}

func verifyAllSHA256(t *testing.T, compatRoot string, manifest compatManifest) {
	t.Helper()
	for _, fixture := range allFixtures(manifest) {
		actual := fileDigest(t, filepath.Join(compatRoot, filepath.FromSlash(fixture.Path)), sha256.New())
		if actual != fixture.SHA256 {
			t.Errorf("fixture %q SHA-256=%s, want %s", fixture.Path, actual, fixture.SHA256)
		}
	}
}

func verifyFileCheckpoint(t *testing.T, path, want string) {
	t.Helper()
	actual := fileDigest(t, path, sha256.New())
	if actual != want {
		t.Errorf("%s SHA-256=%s, frozen checkpoint=%s", filepath.Base(path), actual, want)
	}
}

func fileDigest(t *testing.T, path string, hasher interface {
	io.Writer
	Sum([]byte) []byte
}) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer file.Close()
	if _, err := io.Copy(hasher, file); err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func verifyProbeMetadata(t *testing.T, root, compatRoot string, manifest compatManifest) map[string]probedFixture {
	t.Helper()
	ffprobe := filepath.Join(root, "third_party", "ffmpeg", "bin", "ffprobe.exe")
	results := make(map[string]probedFixture, len(allFixtures(manifest)))
	for _, fixture := range allFixtures(manifest) {
		probe := runFFprobe(t, ffprobe, filepath.Join(compatRoot, filepath.FromSlash(fixture.Path)))
		var stream *probeStream
		wantStreamType := "video"
		if fixture.MediaType == "audio" {
			wantStreamType = "audio"
		}
		for index := range probe.Streams {
			if probe.Streams[index].CodecType == wantStreamType {
				stream = &probe.Streams[index]
				break
			}
		}
		if stream == nil {
			t.Errorf("%s has no probed %s stream", fixture.Path, wantStreamType)
			continue
		}
		codec := stream.CodecName
		if codec == "mjpeg" {
			codec = "jpeg"
		}
		if codec != fixture.Codec {
			t.Errorf("%s probed codec=%s, manifest=%s", fixture.Path, codec, fixture.Codec)
		}
		if fixture.DurationMicros > 0 {
			actualDuration := durationMicros(t, fixture.Path, probe.Format.Duration)
			if actualDuration != fixture.DurationMicros {
				t.Errorf("%s probed duration=%d, manifest=%d", fixture.Path, actualDuration, fixture.DurationMicros)
			}
		}
		rotation := 0
		if len(stream.SideData) != 0 {
			rotation = stream.SideData[0].Rotation
		}
		if rotation != fixture.Rotation {
			t.Errorf("%s probed rotation=%d, manifest=%d", fixture.Path, rotation, fixture.Rotation)
		}
		actualSAR := stream.SAR
		if actualSAR == "" {
			actualSAR = "n/a"
		}
		if actualSAR != fixture.SAR {
			t.Errorf("%s probed SAR=%s, manifest=%s", fixture.Path, actualSAR, fixture.SAR)
		}
		probed := probedFixture{
			Stream:   *stream,
			Codec:    codec,
			SAR:      actualSAR,
			Rotation: rotation,
		}
		if fixture.DurationMicros > 0 {
			probed.DurationMicros = durationMicros(t, fixture.Path, probe.Format.Duration)
		}
		results[fixture.Path] = probed
	}
	return results
}

func runFFprobe(t *testing.T, ffprobe, fixture string) probeDocument {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(
		ctx,
		ffprobe,
		"-v", "error",
		"-show_entries", "format=duration:stream=index,codec_type,codec_name,width,height,sample_aspect_ratio,has_b_frames:stream_side_data=rotation",
		"-of", "json",
		fixture,
	)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("ffprobe %s timed out: %v", fixture, ctx.Err())
	}
	if err != nil {
		t.Fatalf("ffprobe %s: %v: %s", fixture, err, output)
	}
	var result probeDocument
	decodeStrictJSON(t, "ffprobe "+fixture, output, &result)
	return result
}

func durationMicros(t *testing.T, label, raw string) int64 {
	t.Helper()
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		t.Fatalf("%s has invalid probe duration %q", label, raw)
	}
	return int64(math.Round(value * 1_000_000))
}

func verifyLegacyComponents(t *testing.T, legacyBin string, generator compatGoldenGenerator) {
	t.Helper()
	if generator.Kind != "legacy-mediacore-plus-ffmpeg-exe" {
		t.Errorf("legacy generator kind=%q", generator.Kind)
	}
	if len(generator.Components) != len(approvedLegacyComponents) {
		t.Errorf("legacy generator components=%d, want %d", len(generator.Components), len(approvedLegacyComponents))
	}
	seen := make(map[string]bool)
	for _, component := range generator.Components {
		approved, exists := approvedLegacyComponents[component.Role]
		if !exists {
			t.Errorf("unapproved legacy component role %q", component.Role)
			continue
		}
		if seen[component.Role] {
			t.Errorf("duplicate legacy component role %q", component.Role)
		}
		seen[component.Role] = true
		if component != approved {
			t.Errorf("legacy component %q=%+v, approved=%+v", component.Role, component, approved)
		}
		actual := fileDigest(t, filepath.Join(legacyBin, filepath.FromSlash(component.Path)), sha256.New())
		if actual != component.SHA256 {
			t.Errorf("legacy component %q actual SHA-256=%s, golden=%s", component.Role, actual, component.SHA256)
		}
	}
}

func verifyGoldenCoversEveryFixture(t *testing.T, compatRoot string, manifest compatManifest, golden compatGolden) {
	t.Helper()
	if fmt.Sprint(golden.StandardSampleMicros) != "[83000 250000 416000 583000 750000 916000]" {
		t.Errorf("normalized standard sample times=%v", golden.StandardSampleMicros)
	}
	entries := make(map[string]compatGoldenFixture, len(golden.Fixtures))
	for _, entry := range golden.Fixtures {
		if _, exists := entries[entry.Path]; exists {
			t.Errorf("legacy golden path %q is duplicated", entry.Path)
		}
		entries[entry.Path] = entry
		verifyLowerHex(t, entry.Path+" SHA-512", entry.SHA512, sha512.Size*2)
		actual := fileDigest(t, filepath.Join(compatRoot, filepath.FromSlash(entry.Path)), sha512.New())
		if actual != entry.SHA512 {
			t.Errorf("%s SHA-512=%s, golden=%s", entry.Path, actual, entry.SHA512)
		}
		if boolCount(entry.Image != nil, entry.Video != nil, entry.Error != nil) != 1 {
			t.Errorf("%s must have exactly one of image, video, error", entry.Path)
		}
	}
	for _, fixture := range allFixtures(manifest) {
		entry, exists := entries[fixture.Path]
		if !exists {
			t.Errorf("legacy golden does not cover fixture %q", fixture.Path)
			continue
		}
		if fixture.MediaType == "image" && entry.Image == nil && entry.Error == nil {
			t.Errorf("%s has no image result", fixture.Path)
		}
		if fixture.MediaType != "image" && entry.Video == nil && entry.Error == nil {
			t.Errorf("%s has no media result", fixture.Path)
		}
	}
	if len(entries) != len(allFixtures(manifest)) {
		t.Errorf("legacy golden fixture count=%d, manifest fixture count=%d", len(entries), len(allFixtures(manifest)))
	}
}

func verifyGoldenMatchesProbe(t *testing.T, golden compatGolden, probes map[string]probedFixture) {
	t.Helper()
	for _, entry := range golden.Fixtures {
		probe, exists := probes[entry.Path]
		if !exists {
			t.Errorf("%s has no independent probe result", entry.Path)
			continue
		}
		if entry.Image != nil {
			if entry.Image.Width != probe.Stream.Width || entry.Image.Height != probe.Stream.Height {
				t.Errorf("%s legacy image dimensions=%dx%d, probe=%dx%d",
					entry.Path, entry.Image.Width, entry.Image.Height, probe.Stream.Width, probe.Stream.Height)
			}
			continue
		}
		if entry.Video == nil {
			continue
		}
		source := entry.Video.Source
		want := compatSourceGolden{
			StreamType: probe.Stream.CodecType,
			Codec:      probe.Codec,
			Width:      probe.Stream.Width,
			Height:     probe.Stream.Height,
			Rotation:   probe.Rotation,
			SAR:        probe.SAR,
			HasBFrames: probe.Stream.HasBFrames,
		}
		if source != want {
			t.Errorf("%s golden source=%+v, independent probe=%+v", entry.Path, source, want)
		}
		if entry.Video.DurationMicros != probe.DurationMicros {
			t.Errorf("%s golden duration=%d, independent probe=%d",
				entry.Path, entry.Video.DurationMicros, probe.DurationMicros)
		}
	}
}

func verifyGoldenFeatureSemantics(t *testing.T, manifest compatManifest, golden compatGolden) {
	t.Helper()
	manifestByPath := make(map[string]compatFixture)
	for _, fixture := range allFixtures(manifest) {
		manifestByPath[fixture.Path] = fixture
	}
	volatileDiagnostic := []string{"elapsed=", "speed=", " fps=", " @ 0x", " @ 0000"}
	for _, entry := range golden.Fixtures {
		fixture := manifestByPath[entry.Path]
		if entry.Error != nil {
			verifyStableError(t, entry.Path, *entry.Error, volatileDiagnostic)
			continue
		}
		if entry.Image != nil {
			verifyFeatureHex(t, entry.Path, entry.Image.PDQHex, entry.Image.PHashPartsHex, entry.Image.SobelFloatBitsHex)
			if entry.Image.Width <= 0 || entry.Image.Height <= 0 {
				t.Errorf("%s image dimensions=%dx%d, want positive", entry.Path, entry.Image.Width, entry.Image.Height)
			}
			continue
		}
		video := entry.Video
		if video.DurationMicros != fixture.DurationMicros {
			t.Errorf("%s golden duration=%d, manifest duration=%d", entry.Path, video.DurationMicros, fixture.DurationMicros)
		}
		wantTimes := legacySampleTimes(fixture.DurationMicros)
		if fmt.Sprint(video.SampleTimesMicros) != fmt.Sprint(wantTimes) {
			t.Errorf("%s sample times=%v, formula=%v", entry.Path, video.SampleTimesMicros, wantTimes)
		}
		if len(video.Frames) != 6 {
			t.Errorf("%s has %d frames, want 6", entry.Path, len(video.Frames))
			continue
		}
		verifySourceMetadata(t, fixture, video.Source)
		lastOrdinal := -1
		lastPTS := int64(math.MinInt64)
		nonZeroOrdinal := false
		successes := 0
		for index, frame := range video.Frames {
			if frame.SampleIndex != index || frame.RequestedMicros != wantTimes[index] {
				t.Errorf("%s frame %d does not preserve formula order", entry.Path, index)
			}
			if boolCount(frame.SelectedIdentity != nil, frame.Error != nil) != 1 {
				t.Errorf("%s frame %d must have exactly one of selectedIdentity or error", entry.Path, index)
				continue
			}
			if frame.Error != nil {
				verifyStableError(t, fmt.Sprintf("%s frame %d", entry.Path, index), *frame.Error, volatileDiagnostic)
				if frame.DisplayWidth != 0 || frame.DisplayHeight != 0 || frame.OutputSHA256 != "" ||
					frame.PDQHex != "" || len(frame.PHashPartsHex) != 0 || len(frame.SobelFloatBitsHex) != 0 {
					t.Errorf("%s frame %d error result contains success fields", entry.Path, index)
				}
				continue
			}
			successes++
			identity := frame.SelectedIdentity
			if identity.SourceDecodeOrdinal < lastOrdinal || identity.PTSTimeMicros < lastPTS {
				t.Errorf("%s frame %d source identity regressed", entry.Path, index)
			}
			lastOrdinal = identity.SourceDecodeOrdinal
			lastPTS = identity.PTSTimeMicros
			nonZeroOrdinal = nonZeroOrdinal || identity.SourceDecodeOrdinal != 0
			if identity.PictureType == "" {
				t.Errorf("%s frame %d picture type is empty", entry.Path, index)
			}
			if frame.DisplayWidth <= 0 || frame.DisplayHeight <= 0 {
				t.Errorf("%s frame %d display dimensions=%dx%d, want positive", entry.Path, index, frame.DisplayWidth, frame.DisplayHeight)
			}
			verifyLowerHex(t, fmt.Sprintf("%s frame %d output SHA-256", entry.Path, index), frame.OutputSHA256, sha256.Size*2)
			verifyFeatureHex(t, fmt.Sprintf("%s frame %d", entry.Path, index), frame.PDQHex, frame.PHashPartsHex, frame.SobelFloatBitsHex)
		}
		if fixture.MediaType == "video" && successes > 1 && !nonZeroOrdinal {
			t.Errorf("%s source decode ordinals are all zero", entry.Path)
		}
	}
}

func verifySourceMetadata(t *testing.T, fixture compatFixture, source compatSourceGolden) {
	t.Helper()
	if source.Codec != fixture.Codec || source.Rotation != fixture.Rotation || source.SAR != fixture.SAR {
		t.Errorf("%s typed source=%+v does not match manifest codec/rotation/SAR", fixture.Path, source)
	}
	wantType := "video"
	if fixture.MediaType == "audio" {
		wantType = "audio"
	}
	if source.StreamType != wantType {
		t.Errorf("%s source streamType=%q, want %q", fixture.Path, source.StreamType, wantType)
	}
	if wantType == "video" && (source.Width <= 0 || source.Height <= 0) {
		t.Errorf("%s source dimensions=%dx%d, want positive", fixture.Path, source.Width, source.Height)
	}
	if wantType == "audio" && (source.Width != 0 || source.Height != 0 || source.HasBFrames != 0) {
		t.Errorf("%s audio source contains video metadata: %+v", fixture.Path, source)
	}
}

func verifyApprovedSARDifference(t *testing.T, golden compatGolden) {
	t.Helper()
	if len(golden.ApprovedSemanticDeltas) != 1 {
		t.Fatalf("approved semantic deltas=%d, want 1", len(golden.ApprovedSemanticDeltas))
	}
	delta := golden.ApprovedSemanticDeltas[0]
	want := compatSemanticDelta{
		ID:                  "sar-corrected-feature-geometry",
		FixturePath:         "videos/h264-sar-4x3.mp4",
		Approval:            "approved-design-delta",
		LegacyBehavior:      "raw-pixel-scaling-before-features",
		LegacyDisplayWidth:  512,
		LegacyDisplayHeight: 341,
		FutureBehavior:      "apply-sar-before-feature-scaling",
		FutureDisplayWidth:  512,
		FutureDisplayHeight: 256,
	}
	if delta != want {
		t.Errorf("SAR semantic delta=%+v, want %+v", delta, want)
	}
	for _, entry := range golden.Fixtures {
		if entry.Path != delta.FixturePath || entry.Video == nil {
			continue
		}
		for _, frame := range entry.Video.Frames {
			if frame.SelectedIdentity != nil &&
				(frame.DisplayWidth != delta.LegacyDisplayWidth || frame.DisplayHeight != delta.LegacyDisplayHeight) {
				t.Errorf("%s legacy frame %d dimensions=%dx%d, approved legacy=%dx%d",
					entry.Path, frame.SampleIndex, frame.DisplayWidth, frame.DisplayHeight,
					delta.LegacyDisplayWidth, delta.LegacyDisplayHeight)
			}
		}
		return
	}
	t.Errorf("SAR semantic delta fixture %q is absent", delta.FixturePath)
}

func legacySampleTimes(durationMicros int64) []int64 {
	durationMS := (durationMicros + 500) / 1000
	quotient, remainder := durationMS/12, durationMS%12
	result := make([]int64, 6)
	for index, multiplier := range []int64{1, 3, 5, 7, 9, 11} {
		result[index] = (quotient*multiplier + remainder*multiplier/12) * 1000
	}
	return result
}

func verifyFeatureHex(t *testing.T, label, pdq string, pHash, sobel []string) {
	t.Helper()
	verifyLowerHex(t, label+" PDQ", pdq, 64)
	if len(pHash) != 9 || len(sobel) != 128 {
		t.Errorf("%s feature arrays pHash=%d Sobel=%d, want 9/128", label, len(pHash), len(sobel))
	}
	for index, value := range pHash {
		verifyLowerHex(t, fmt.Sprintf("%s pHash part %d", label, index), value, 16)
	}
	for index, value := range sobel {
		verifyLowerHex(t, fmt.Sprintf("%s Sobel float bits %d", label, index), value, 8)
	}
}

func verifyLowerHex(t *testing.T, label, value string, wantLength int) {
	t.Helper()
	if len(value) != wantLength || value != strings.ToLower(value) {
		t.Errorf("%s=%q is not %d lowercase hex characters", label, value, wantLength)
		return
	}
	if _, err := hex.DecodeString(value); err != nil {
		t.Errorf("%s=%q is not hexadecimal: %v", label, value, err)
	}
}

func verifyStableError(t *testing.T, label string, capture compatCaptureError, forbidden []string) {
	t.Helper()
	if capture.Stage == "" || capture.Message == "" {
		t.Errorf("%s error is incomplete: %+v", label, capture)
	}
	lower := strings.ToLower(capture.Message)
	for _, unstable := range forbidden {
		if strings.Contains(lower, unstable) {
			t.Errorf("%s error contains unstable diagnostic %q: %q", label, unstable, capture.Message)
		}
	}
}

func boolCount(values ...bool) int {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count
}

func verifyCaptureRejectsJunction(t *testing.T, root, legacyBin, compatRoot, manifestPath string) {
	t.Helper()
	if os.PathSeparator != '\\' {
		t.Skip("junction behavior is Windows-specific")
	}
	tempRoot := t.TempDir()
	tempCompat := filepath.Join(tempRoot, "compat")
	if err := os.MkdirAll(filepath.Join(tempCompat, "videos"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, manifestPath, filepath.Join(tempCompat, "manifest.json"))
	videoEntries, err := os.ReadDir(filepath.Join(compatRoot, "videos"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range videoEntries {
		if !entry.Type().IsRegular() {
			continue
		}
		copyFile(t,
			filepath.Join(compatRoot, "videos", entry.Name()),
			filepath.Join(tempCompat, "videos", entry.Name()),
		)
	}
	junction := filepath.Join(tempCompat, "images")
	linkOutput, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", junction, filepath.Join(compatRoot, "images")).CombinedOutput()
	if err != nil {
		t.Fatalf("create test junction: %v: %s", err, linkOutput)
	}
	t.Cleanup(func() {
		_ = os.Remove(junction)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	outPath := filepath.Join(tempCompat, "legacy-golden.json")
	command := exec.CommandContext(
		ctx,
		"pwsh", "-NoProfile", "-File",
		filepath.Join(root, "scripts", "capture_videocore_legacy_golden.ps1"),
		"-Manifest", filepath.Join(tempCompat, "manifest.json"),
		"-OutFile", outPath,
		"-LegacyBinDir", legacyBin,
	)
	output, runErr := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("junction rejection capture timed out: %v", ctx.Err())
	}
	if runErr == nil {
		t.Fatalf("capture accepted fixture path through junction; output=%s", output)
	}
	if !strings.Contains(strings.ToLower(string(output)), "reparse") {
		t.Fatalf("capture rejected junction for wrong reason: %s", output)
	}
	if _, err := os.Stat(outPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("junction rejection left final golden: %v", err)
	}
}

func copyFile(t *testing.T, source, destination string) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read %s: %v", source, err)
	}
	if err := os.WriteFile(destination, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", destination, err)
	}
}

func verifyCaptureDeadlineCleanup(t *testing.T, root, legacyBin, compatRoot, manifestPath string) {
	t.Helper()
	tempCompat := prepareCompatCopy(t, compatRoot, manifestPath)
	outPath := filepath.Join(tempCompat, "legacy-golden.json")
	output, runErr := runCaptureScript(
		t, root, legacyBin, tempCompat, outPath,
		"VIDEOCORE_CAPTURE_PROCESS_TIMEOUT_MS=1",
	)
	if runErr == nil {
		t.Fatalf("capture ignored finite process deadline; output=%s", output)
	}
	if !strings.Contains(strings.ToLower(string(output)), "process deadline exceeded") {
		t.Fatalf("capture deadline failed for wrong reason: %s", output)
	}
	verifyNoCaptureOutputs(t, tempCompat, outPath)
}

func verifyCaptureAtomicFailureCleanup(t *testing.T, root, legacyBin, compatRoot, manifestPath string) {
	t.Helper()
	tempCompat := prepareCompatCopy(t, compatRoot, manifestPath)
	outPath := filepath.Join(tempCompat, "legacy-golden.json")
	output, runErr := runCaptureScript(
		t, root, legacyBin, tempCompat, outPath,
		"VIDEOCORE_CAPTURE_FAULT_AFTER_TEMP_WRITE=1",
	)
	if runErr == nil {
		t.Fatalf("capture ignored post-flush fault injection; output=%s", output)
	}
	if !strings.Contains(strings.ToLower(string(output)), "injected failure after temporary golden flush") {
		t.Fatalf("capture atomic fault failed for wrong reason: %s", output)
	}
	verifyNoCaptureOutputs(t, tempCompat, outPath)
}

func verifyCaptureTerminatesHelperTree(t *testing.T, root, legacyBin, compatRoot string) {
	t.Helper()
	testRoot := t.TempDir()
	outPath := filepath.Join(testRoot, "legacy-golden.json")
	pidFile := filepath.Join(testRoot, "helper-pids.txt")
	helper := filepath.Join(root, "integration", "testdata", "videocore_timeout_tree_helper.ps1")
	output, runErr := runCaptureScript(
		t, root, legacyBin, compatRoot, outPath,
		"VIDEOCORE_CAPTURE_PROCESS_TIMEOUT_MS=1000",
		"VIDEOCORE_CAPTURE_TIMEOUT_HELPER="+helper,
		"VIDEOCORE_CAPTURE_TIMEOUT_HELPER_PID_FILE="+pidFile,
	)
	if runErr == nil {
		t.Fatalf("capture ignored timeout process-tree helper; output=%s", output)
	}
	const wantTermination = "process deadline exceeded; process tree terminated within 2s grace: pwsh.exe"
	if !strings.Contains(strings.ToLower(string(output)), wantTermination) {
		t.Fatalf("capture helper timeout has unstable or wrong termination error: %s", output)
	}
	pidData, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read timeout helper PIDs: %v; output=%s", err, output)
	}
	fields := strings.Fields(string(pidData))
	if len(fields) < 2 {
		t.Fatalf("timeout helper recorded %d PIDs, want parent and child: %q", len(fields), pidData)
	}
	var pids []int
	for _, field := range fields {
		pid, err := strconv.Atoi(field)
		if err != nil || pid <= 0 {
			t.Fatalf("invalid timeout helper PID %q", field)
		}
		pids = append(pids, pid)
	}
	waitForPIDsToExit(t, pids, 5*time.Second)
	verifyNoCaptureOutputs(t, testRoot, outPath)
}

func waitForPIDsToExit(t *testing.T, pids []int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var alive []int
		for _, pid := range pids {
			command := fmt.Sprintf(
				"if (Get-Process -Id %d -ErrorAction SilentlyContinue) { exit 0 }; exit 1",
				pid,
			)
			if exec.Command("pwsh", "-NoProfile", "-Command", command).Run() == nil {
				alive = append(alive, pid)
			}
		}
		if len(alive) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout helper processes are still alive after %s: %v", timeout, alive)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func prepareCompatCopy(t *testing.T, compatRoot, manifestPath string) string {
	t.Helper()
	tempCompat := filepath.Join(t.TempDir(), "compat")
	for _, directory := range []string{"images", "videos"} {
		if err := os.MkdirAll(filepath.Join(tempCompat, directory), 0o755); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(filepath.Join(compatRoot, directory))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.Type().IsRegular() {
				copyFile(t,
					filepath.Join(compatRoot, directory, entry.Name()),
					filepath.Join(tempCompat, directory, entry.Name()),
				)
			}
		}
	}
	copyFile(t, manifestPath, filepath.Join(tempCompat, "manifest.json"))
	return tempCompat
}

func verifyCaptureSuccessReproducesFrozenGolden(t *testing.T, root, legacyBin, compatRoot, manifestPath, repositoryGoldenPath string) {
	t.Helper()
	tempCompat := prepareCompatCopy(t, compatRoot, manifestPath)
	outPath := filepath.Join(tempCompat, "captured-legacy-golden.json")
	if _, err := os.Stat(outPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary capture output must start absent: %v", err)
	}

	repositoryGoldenBefore := fileDigest(t, repositoryGoldenPath, sha256.New())
	output, runErr := runCaptureScript(t, root, legacyBin, tempCompat, outPath)
	if runErr != nil {
		t.Fatalf("normal capture failed: %v\n%s", runErr, output)
	}

	var captured compatGolden
	decodeStrictJSONFile(t, outPath, &captured)
	actualSHA256 := fileDigest(t, outPath, sha256.New())
	wantSHA256 := compatGoldenSHA256
	if actualSHA256 != wantSHA256 {
		t.Fatalf("captured golden SHA-256=%s, frozen checkpoint=%s", actualSHA256, wantSHA256)
	}
	if after := fileDigest(t, repositoryGoldenPath, sha256.New()); after != repositoryGoldenBefore {
		t.Fatalf("normal capture modified repository frozen golden: before=%s after=%s", repositoryGoldenBefore, after)
	}

	temporary, err := filepath.Glob(filepath.Join(tempCompat, ".captured-legacy-golden.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temporary) != 0 {
		t.Fatalf("successful capture leaked temporary golden files: %v", temporary)
	}
}

func runCaptureScript(t *testing.T, root, legacyBin, compatRoot, outPath string, environment ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(
		ctx,
		"pwsh", "-NoProfile", "-File",
		filepath.Join(root, "scripts", "capture_videocore_legacy_golden.ps1"),
		"-Manifest", filepath.Join(compatRoot, "manifest.json"),
		"-OutFile", outPath,
		"-LegacyBinDir", legacyBin,
	)
	command.Env = append(os.Environ(), environment...)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("capture script test timed out: %v", ctx.Err())
	}
	return output, err
}

func verifyNoCaptureOutputs(t *testing.T, compatRoot, outPath string) {
	t.Helper()
	if _, err := os.Stat(outPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("failed capture left final golden: %v", err)
	}
	temporary, err := filepath.Glob(filepath.Join(compatRoot, ".legacy-golden.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temporary) != 0 {
		t.Errorf("failed capture leaked temporary golden files: %v", temporary)
	}
}

func verifyQualityPresenceMutations(t *testing.T, golden compatGolden) {
	t.Helper()
	t.Run("missing image quality", func(t *testing.T) {
		mutated := cloneCompatGolden(t, golden)
		for index := range mutated.Fixtures {
			if mutated.Fixtures[index].Image != nil {
				mutated.Fixtures[index].Image.Quality = nil
				break
			}
		}
		if len(qualityPresenceViolations(mutated)) == 0 {
			t.Fatal("quality validation accepted a successful image with missing quality")
		}
	})
	t.Run("missing frame quality", func(t *testing.T) {
		mutated := cloneCompatGolden(t, golden)
		found := false
		for fixtureIndex := range mutated.Fixtures {
			video := mutated.Fixtures[fixtureIndex].Video
			if video == nil {
				continue
			}
			for frameIndex := range video.Frames {
				if video.Frames[frameIndex].SelectedIdentity != nil {
					video.Frames[frameIndex].Quality = nil
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			t.Fatal("golden has no successful frame to mutate")
		}
		if len(qualityPresenceViolations(mutated)) == 0 {
			t.Fatal("quality validation accepted a successful frame with missing quality")
		}
	})
	t.Run("error frame carrying quality", func(t *testing.T) {
		mutated := cloneCompatGolden(t, golden)
		found := false
		zero := 0
		for fixtureIndex := range mutated.Fixtures {
			video := mutated.Fixtures[fixtureIndex].Video
			if video == nil {
				continue
			}
			for frameIndex := range video.Frames {
				if video.Frames[frameIndex].Error != nil {
					video.Frames[frameIndex].Quality = &zero
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			t.Fatal("golden has no error frame to mutate")
		}
		if len(qualityPresenceViolations(mutated)) == 0 {
			t.Fatal("quality validation accepted an error frame carrying quality")
		}
	})
}

func cloneCompatGolden(t *testing.T, golden compatGolden) compatGolden {
	t.Helper()
	data, err := json.Marshal(golden)
	if err != nil {
		t.Fatal(err)
	}
	var clone compatGolden
	decodeStrictJSON(t, "cloned golden", data, &clone)
	return clone
}

func qualityPresenceViolations(golden compatGolden) []string {
	var violations []string
	for _, fixture := range golden.Fixtures {
		if fixture.Image != nil && fixture.Image.Quality == nil {
			violations = append(violations, fixture.Path+" successful image is missing quality")
		}
		if fixture.Video == nil {
			continue
		}
		for _, frame := range fixture.Video.Frames {
			label := fmt.Sprintf("%s frame %d", fixture.Path, frame.SampleIndex)
			if frame.SelectedIdentity != nil && frame.Quality == nil {
				violations = append(violations, label+" successful result is missing quality")
			}
			if frame.Error != nil && frame.Quality != nil {
				violations = append(violations, label+" error result carries quality")
			}
		}
	}
	return violations
}
