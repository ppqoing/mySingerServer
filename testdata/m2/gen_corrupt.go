//go:build ignore

// Command gen_corrupt creates the deterministic M2 acceptance corpus.
package main

import (
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const corpusSeed = "m2-corpus-seed-v1"

type manifestFile struct {
	Path           string `json:"path"`
	SHA512         string `json:"sha512"`
	Class          string `json:"class"`
	Classification string `json:"classification"`
	Size           int64  `json:"size"`
}

type manifest struct {
	Version string            `json:"version"`
	Seed    string            `json:"seed"`
	Counts  map[string]int    `json:"counts"`
	Sources map[string]string `json:"sources"`
	Files   []manifestFile    `json:"files"`
}

type generator struct {
	root   string
	ffmpeg string
	files  []manifestFile
}

func main() {
	var output, ffmpeg string
	flag.StringVar(&output, "out", "", "output directory")
	flag.StringVar(&ffmpeg, "ffmpeg", "", "path to ffmpeg.exe")
	flag.Parse()
	if output == "" || ffmpeg == "" {
		fatalf("-out and -ffmpeg are required")
	}
	root, err := filepath.Abs(output)
	must(err)
	must(os.MkdirAll(root, 0o755))
	if _, err := os.Stat(ffmpeg); err != nil {
		fatalf("ffmpeg: %v", err)
	}
	g := &generator{root: root, ffmpeg: ffmpeg}
	g.generate()
}

func (g *generator) generate() {
	seedImage := patternedImage(320, 240, 0)
	jpegPath := g.writeJPEG("base/valid.jpg", seedImage, "valid_image", "image")
	pngPath := g.writePNG("base/valid.png", seedImage, "valid_image", "image")
	jpegBytes := mustRead(jpegPath)
	g.writeBytes("base/wrongext.png", jpegBytes, "valid_image_wrong_extension", "image")
	webpPath := g.path("base/valid.webp")
	g.runFFmpeg(
		"-i", pngPath,
		"-frames:v", "1",
		"-c:v", "libwebp",
		"-lossless", "1",
		webpPath,
	)
	g.record("base/valid.webp", "valid_image", "image")

	g.generateCorrupt(jpegBytes, mustRead(pngPath))

	valid5 := g.path("base/valid5s.mp4")
	g.runFFmpeg(
		"-f", "lavfi",
		"-i", "testsrc=duration=5:size=320x240:rate=10",
		"-c:v", "mpeg4",
		"-q:v", "5",
		"-pix_fmt", "yuv420p",
		valid5,
	)
	g.record("base/valid5s.mp4", "valid_video_5s", "video")
	valid8 := g.path("base/valid8s.mp4")
	g.runFFmpeg(
		"-f", "lavfi",
		"-i", "testsrc2=duration=8:size=320x240:rate=10",
		"-c:v", "mpeg4",
		"-q:v", "5",
		"-pix_fmt", "yuv420p",
		valid8,
	)
	g.record("base/valid8s.mp4", "valid_video_8s", "video")
	videoBytes := mustRead(valid5)
	g.writeBytes("base/copy_of_valid5s.mp4", videoBytes, "valid_video_copy", "video")
	g.writeBytes("base/trunc50.mp4", videoBytes[:len(videoBytes)/2], "truncated_video", "video_error")

	for index := 0; index < 100; index++ {
		g.writeBytes(
			fmt.Sprintf("singleflight/images/image_%03d.jpg", index),
			jpegBytes,
			"singleflight_image",
			"image",
		)
	}
	for index := 0; index < 20; index++ {
		g.writeBytes(
			fmt.Sprintf("singleflight/videos/video_%03d.mp4", index),
			videoBytes,
			"singleflight_video",
			"video",
		)
	}
	for index := 0; index < 10; index++ {
		distinct := append([]byte(nil), videoBytes...)
		distinct = append(distinct, []byte(fmt.Sprintf("m2-cache-video-%02d", index))...)
		g.writeBytes(
			fmt.Sprintf("cache/video_%02d.mp4", index),
			distinct,
			"cache_video",
			"video",
		)
	}
	for index := 0; index < 10; index++ {
		g.writeBytes(
			fmt.Sprintf("injection/img__crash__%02d.jpg", index),
			jpegBytes,
			"crash_image",
			"native_crash",
		)
	}
	g.writeBytes(
		"injection/slow__hang__.jpg",
		jpegBytes,
		"hang_image",
		"watchdog_crash",
	)

	g.writeBytes("paths/图片_😀 副本.jpg", jpegBytes, "unicode_image", "image")
	g.writeBytes("paths/readonly.jpg", jpegBytes, "readonly_image", "image")
	g.writeBytes("paths/denied.jpg", jpegBytes, "denied_image", "open_error")
	longRelative := "paths/long"
	for len(g.path(filepath.ToSlash(longRelative)+"/long.jpg")) <= 280 {
		longRelative += "/" + strings.Repeat("segment", 4)
	}
	g.writeBytes(
		filepath.ToSlash(longRelative)+"/long.jpg",
		jpegBytes,
		"long_path_image",
		"image",
	)

	for index := 0; index < 1000; index++ {
		// The width/height pair is a deterministic base-100 encoding of the
		// index, so every smoke JPEG has distinct encoded content even when
		// the pixel pattern's byte channels wrap.
		width := 64 + index%100
		height := 64 + index/100
		g.writeJPEG(
			fmt.Sprintf("smoke/image_%04d.jpg", index),
			patternedImage(width, height, index+1),
			"smoke_image",
			"image",
		)
	}
	for index := 0; index < 2000; index++ {
		// A larger, disjoint deterministic corpus warms the long-lived Agent
		// before AC-8 measures a separate set of 1000 unique images. Widths
		// start above the measured corpus maximum, so the encoded JPEGs cannot
		// share content with the formal measurement set.
		width := 192 + index%100
		height := 80 + index/100
		g.writeJPEG(
			fmt.Sprintf("warmup/image_%04d.jpg", index),
			patternedImage(width, height, index+10_001),
			"warmup_image",
			"image",
		)
	}

	sort.Slice(g.files, func(i, j int) bool {
		return g.files[i].Path < g.files[j].Path
	})
	output := manifest{
		Version: "1",
		Seed:    corpusSeed,
		Counts: map[string]int{
			"corrupt_classes": 8,
			"smoke_images":    1000,
			"warmup_images":   2000,
			"single_images":   100,
			"single_videos":   20,
			"cache_videos":    10,
			"crash_images":    10,
			"hang_images":     1,
			"manifest_files":  len(g.files),
		},
		Sources: map[string]string{
			"valid_jpeg": hashBytes(jpegBytes),
			"valid_png":  hashBytes(mustRead(pngPath)),
			"valid_webp": hashBytes(mustRead(webpPath)),
			"valid5s":    hashBytes(videoBytes),
			"valid8s":    hashBytes(mustRead(valid8)),
		},
		Files: g.files,
	}
	data, err := json.MarshalIndent(output, "", "  ")
	must(err)
	data = append(data, '\n')
	must(os.WriteFile(g.path("manifest.json"), data, 0o644))
	fmt.Printf("M2 corpus PASS files=%d manifest=%s\n", len(g.files), g.path("manifest.json"))
}

func (g *generator) generateCorrupt(jpegBytes, pngBytes []byte) {
	g.writeBytes("corrupt/empty.jpg", nil, "corrupt_empty", "image_error")
	g.writeBytes("corrupt/tiny.jpg", []byte{0xff, 0xd8, 0xff}, "corrupt_tiny", "image_error")
	g.writeBytes(
		"corrupt/jpeg_trunc50.jpg",
		jpegBytes[:len(jpegBytes)/2],
		"corrupt_jpeg_trunc50",
		"image_error",
	)
	g.writeBytes(
		"corrupt/jpeg_trunc95.jpg",
		jpegBytes[:len(jpegBytes)*95/100],
		"corrupt_jpeg_trunc95",
		"image_error",
	)
	zeroed := append([]byte(nil), jpegBytes...)
	start := len(zeroed) / 3
	end := start + 4096
	if end > len(zeroed) {
		end = len(zeroed)
	}
	for index := start; index < end; index++ {
		zeroed[index] = 0
	}
	g.writeBytes("corrupt/jpeg_zeroed_mid.jpg", zeroed, "corrupt_jpeg_zeroed", "image_error")
	badMagic := append([]byte(nil), jpegBytes...)
	copy(badMagic, []byte{0, 0x11, 0x22})
	g.writeBytes("corrupt/jpeg_badmagic.jpg", badMagic, "corrupt_bad_magic", "image_error")
	badChunk := append([]byte(nil), pngBytes...)
	if len(badChunk) > 32 {
		badChunk[29] ^= 0xff
	}
	g.writeBytes("corrupt/png_bad_chunk.png", badChunk, "corrupt_png_chunk", "image_error")
	oversized := append([]byte(nil), pngBytes...)
	if len(oversized) > 24 {
		for index := 16; index < 24; index++ {
			oversized[index] = 0xff
		}
	}
	g.writeBytes("corrupt/png_oversized.png", oversized, "corrupt_dimensions", "image_error")
}

func patternedImage(width, height, variant int) image.Image {
	output := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			output.SetRGBA(x, y, color.RGBA{
				R: uint8((x*3 + variant*17) % 256),
				G: uint8((y*5 + variant*29) % 256),
				B: uint8((x + y + variant*11) % 256),
				A: 255,
			})
		}
	}
	return output
}

func (g *generator) writeJPEG(
	relative string,
	img image.Image,
	class string,
	classification string,
) string {
	path := g.path(relative)
	must(os.MkdirAll(filepath.Dir(path), 0o755))
	file, err := os.Create(path)
	must(err)
	must(jpeg.Encode(file, img, &jpeg.Options{Quality: 90}))
	must(file.Close())
	g.record(relative, class, classification)
	return path
}

func (g *generator) writePNG(
	relative string,
	img image.Image,
	class string,
	classification string,
) string {
	path := g.path(relative)
	must(os.MkdirAll(filepath.Dir(path), 0o755))
	file, err := os.Create(path)
	must(err)
	must(png.Encode(file, img))
	must(file.Close())
	g.record(relative, class, classification)
	return path
}

func (g *generator) writeBytes(
	relative string,
	data []byte,
	class string,
	classification string,
) {
	path := g.path(relative)
	must(os.MkdirAll(filepath.Dir(path), 0o755))
	must(os.WriteFile(path, data, 0o644))
	g.record(relative, class, classification)
}

func (g *generator) runFFmpeg(arguments ...string) {
	output := arguments[len(arguments)-1]
	must(os.MkdirAll(filepath.Dir(output), 0o755))
	base := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-nostdin",
		"-y",
	}
	outputOptions := []string{
		"-threads", "1",
		"-fflags", "+bitexact",
		"-flags:v", "+bitexact",
		"-map_metadata", "-1",
	}
	commandArguments := append([]string(nil), base...)
	commandArguments = append(commandArguments, arguments[:len(arguments)-1]...)
	commandArguments = append(commandArguments, outputOptions...)
	commandArguments = append(commandArguments, output)
	command := exec.Command(g.ffmpeg, commandArguments...)
	if output, err := command.CombinedOutput(); err != nil {
		fatalf("ffmpeg: %v\n%s", err, output)
	}
}

func (g *generator) record(relative, class, classification string) {
	data := mustRead(g.path(relative))
	g.files = append(g.files, manifestFile{
		Path:           filepath.ToSlash(relative),
		SHA512:         hashBytes(data),
		Class:          class,
		Classification: classification,
		Size:           int64(len(data)),
	})
}

func (g *generator) path(relative string) string {
	return filepath.Join(g.root, filepath.FromSlash(relative))
}

func hashBytes(data []byte) string {
	sum := sha512.Sum512(data)
	return hex.EncodeToString(sum[:])
}

func mustRead(path string) []byte {
	data, err := os.ReadFile(path)
	must(err)
	return data
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func fatalf(format string, values ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", values...)
	os.Exit(1)
}
