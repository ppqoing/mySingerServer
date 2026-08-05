package m6bench

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type IOConfig struct {
	Roots      []string
	Extensions []string
	MaxFiles   int
	Duration   time.Duration
	Streams    int
	BlockBytes int
}

type IOResult struct {
	SchemaVersion int         `json:"schema_version"`
	Kind          string      `json:"kind"`
	Roots         []string    `json:"roots"`
	Files         int64       `json:"files"`
	Bytes         int64       `json:"bytes"`
	Errors        int64       `json:"errors"`
	ElapsedMS     int64       `json:"elapsed_ms"`
	MiBPerSecond  float64     `json:"mib_per_second"`
	StopReason    string      `json:"stop_reason"`
	Streams       int         `json:"streams"`
	BlockKB       int         `json:"block_kb"`
	Latency       Percentiles `json:"latency_ms"`
	Selected      []string    `json:"-"`
}

func RunIO(parent context.Context, cfg IOConfig) (IOResult, error) {
	if len(cfg.Roots) == 0 || len(cfg.Extensions) == 0 {
		return IOResult{}, fmt.Errorf("benchio: roots and extensions are required")
	}
	if cfg.MaxFiles < 1 || cfg.MaxFiles > 1_000_000 ||
		cfg.Duration <= 0 || cfg.Streams < 1 || cfg.Streams > 1024 ||
		cfg.BlockBytes < 1 || cfg.BlockBytes > 16<<20 {
		return IOResult{}, fmt.Errorf("benchio: invalid bounds")
	}
	roots := make([]string, 0, len(cfg.Roots))
	for _, root := range cfg.Roots {
		absolute, err := filepath.Abs(root)
		if err != nil {
			return IOResult{}, fmt.Errorf("benchio: root %q: %w", root, err)
		}
		info, err := os.Stat(absolute)
		if err != nil {
			return IOResult{}, fmt.Errorf("benchio: root %q: %w", absolute, err)
		}
		if !info.IsDir() {
			return IOResult{}, fmt.Errorf("benchio: root %q is not a directory", absolute)
		}
		roots = append(roots, filepath.Clean(absolute))
	}
	extensions := make(map[string]struct{}, len(cfg.Extensions))
	for _, extension := range cfg.Extensions {
		extension = strings.ToLower(strings.TrimSpace(extension))
		if extension == "" {
			continue
		}
		if !strings.HasPrefix(extension, ".") {
			extension = "." + extension
		}
		extensions[extension] = struct{}{}
	}
	if len(extensions) == 0 {
		return IOResult{}, fmt.Errorf("benchio: no valid extensions")
	}

	ctx, cancel := context.WithTimeout(parent, cfg.Duration)
	defer cancel()
	started := time.Now()
	selected, reachedLimit, err := selectFiles(ctx, roots, extensions, cfg.MaxFiles)
	if err != nil && ctx.Err() == nil {
		return IOResult{}, err
	}
	result := IOResult{
		SchemaVersion: SchemaVersion,
		Kind:          "io",
		Roots:         roots,
		Streams:       cfg.Streams,
		BlockKB:       cfg.BlockBytes / 1024,
		Selected:      append([]string(nil), selected...),
	}

	jobs := make(chan string)
	type fileResult struct {
		bytes   int64
		elapsed time.Duration
		err     error
	}
	results := make(chan fileResult, cfg.Streams)
	var workers sync.WaitGroup
	for index := 0; index < cfg.Streams; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			buffer := make([]byte, cfg.BlockBytes)
			for path := range jobs {
				fileStarted := time.Now()
				count, readErr := readFile(ctx, path, buffer)
				results <- fileResult{
					bytes: count, elapsed: time.Since(fileStarted), err: readErr,
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, path := range selected {
			select {
			case jobs <- path:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()
	latencies := make([]time.Duration, 0, len(selected))
	for current := range results {
		result.Files++
		result.Bytes += current.bytes
		latencies = append(latencies, current.elapsed)
		if current.err != nil {
			result.Errors++
		}
	}
	elapsed := time.Since(started)
	result.ElapsedMS = elapsed.Milliseconds()
	if elapsed > 0 {
		result.MiBPerSecond = float64(result.Bytes) / (1024 * 1024) / elapsed.Seconds()
	}
	result.Latency = DurationPercentiles(latencies)
	switch {
	case ctx.Err() != nil:
		result.StopReason = "duration"
	case reachedLimit:
		result.StopReason = "max_files"
	default:
		result.StopReason = "completed"
	}
	return result, nil
}

func selectFiles(
	ctx context.Context,
	roots []string,
	extensions map[string]struct{},
	maxFiles int,
) ([]string, bool, error) {
	var selected []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.Type().IsRegular() {
				if _, ok := extensions[strings.ToLower(filepath.Ext(path))]; ok {
					selected = append(selected, path)
					if len(selected) == maxFiles {
						return fs.SkipAll
					}
				}
			}
			return nil
		})
		if err != nil {
			return selected, false, fmt.Errorf("benchio: enumerate %q: %w", root, err)
		}
		if len(selected) == maxFiles {
			break
		}
	}
	sort.SliceStable(selected, func(left, right int) bool {
		leftFold, rightFold := strings.ToLower(selected[left]), strings.ToLower(selected[right])
		if leftFold == rightFold {
			return selected[left] < selected[right]
		}
		return leftFold < rightFold
	})
	return selected, len(selected) == maxFiles, nil
}

func readFile(ctx context.Context, path string, buffer []byte) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		count, readErr := file.Read(buffer)
		total += int64(count)
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}
