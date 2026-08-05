package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"dedup/internal/m6bench"
)

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ";") }
func (s *stringList) Set(value string) error {
	for _, item := range strings.Split(value, ";") {
		if item = strings.TrimSpace(item); item != "" {
			*s = append(*s, item)
		}
	}
	return nil
}

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}

func runCLI(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("benchio", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var roots stringList
	flags.Var(&roots, "root", "read-only source root; repeat or separate with semicolon")
	extensions := flags.String(
		"ext",
		".jpg,.jpeg,.png,.webp,.bmp,.gif,.tif,.tiff,.mp4,.mkv,.mov,.avi,.webm,.m4v",
		"comma-separated media extensions",
	)
	maxFiles := flags.Int("max-files", 10000, "maximum selected files")
	duration := flags.Duration("duration", 10*time.Minute, "maximum total duration")
	streams := flags.Int("streams", 6, "parallel readers")
	blockKB := flags.Int("block-kb", 4096, "read buffer size in KiB")
	out := flags.String("out", "", "optional JSON output path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	result, err := m6bench.RunIO(context.Background(), m6bench.IOConfig{
		Roots: roots, Extensions: strings.Split(*extensions, ","),
		MaxFiles: *maxFiles, Duration: *duration,
		Streams: *streams, BlockBytes: *blockKB * 1024,
	})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if *out != "" {
		if err := m6bench.WriteJSON(*out, result); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if err := m6bench.EncodeJSON(stdout, result); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
