package firstscreen

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"time"
)

var analyzerStageNames = []string{
	"exact_group",
	"image_load",
	"image_screen",
	"video_load",
	"video_screen",
	"db_write",
}

type analyzerStore interface {
	StreamFilesBySHA(context.Context, func([64]byte, FileRef) error) error
	LoadImageFeatures(context.Context) ([]ImageFeature, error)
	LoadVideoFeatures(context.Context) ([]VideoFeature, error)
	ReplaceResults(context.Context, []ExactGroup, []CandidatePair) (int, int, int, error)
	BadRows() int
}

// Analyzer 一筛分析器：一次 Run = 精确分组 + 图片一筛 + 视频一筛 + 结果重写。
type Analyzer struct {
	store       analyzerStore
	cfg         Config
	log         *slog.Logger
	screenImage func([]ImageFeature, int, float64, int) ([]CandidatePair, error)
	screenVideo func([]VideoFeature, int64, int) ([]CandidatePair, error)
}

func NewAnalyzer(store *Store, cfg Config, log *slog.Logger) *Analyzer {
	return newAnalyzer(store, cfg, log)
}

func newAnalyzer(store analyzerStore, cfg Config, log *slog.Logger) *Analyzer {
	if log == nil {
		log = slog.Default()
	}
	return &Analyzer{
		store: store,
		cfg:   cfg,
		log:   log,
		screenImage: func(features []ImageFeature, hammingMax int, aspectTolerance float64, qualityMin int) ([]CandidatePair, error) {
			return screenImages(features, hammingMax, aspectTolerance, qualityMin), nil
		},
		screenVideo: func(features []VideoFeature, windowMs int64, hammingMax int) ([]CandidatePair, error) {
			return screenVideos(features, windowMs, hammingMax), nil
		},
	}
}

// RunStats 一轮分析的分阶段指标。它可直接序列化为 HTTP 状态响应。
type RunStats struct {
	FilesScanned   int              `json:"files_scanned"`
	ExactGroups    int              `json:"exact_groups"`
	ExactMembers   int              `json:"exact_members"`
	ImageFeatures  int              `json:"image_features"`
	ImagePairs     int              `json:"image_pairs"`
	VideoFeatures  int              `json:"video_features"`
	VideoPairs     int              `json:"video_pairs"`
	BadRows        int              `json:"bad_rows"`
	SkippedPairs   int              `json:"skipped_pairs"`
	GroupsWritten  int              `json:"groups_written"`
	MembersWritten int              `json:"members_written"`
	StageElapsedMs map[string]int64 `json:"stage_elapsed_ms"`
	HeapAllocBytes uint64           `json:"heap_alloc_bytes"`
}

// Run 执行一轮完整一筛。任一步失败均返回已收集的部分统计与阶段限定错误。
func (a *Analyzer) Run(ctx context.Context) (stats *RunStats, runErr error) {
	stats = &RunStats{StageElapsedMs: make(map[string]int64, len(analyzerStageNames))}
	for _, stage := range analyzerStageNames {
		stats.StageElapsedMs[stage] = 0
	}
	badRowsBaseline := a.store.BadRows()

	defer func() {
		stats.BadRows = a.store.BadRows() - badRowsBaseline
		runtime.GC()
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		stats.HeapAllocBytes = mem.HeapAlloc

		attrs := []any{
			"files_scanned", stats.FilesScanned,
			"exact_groups", stats.ExactGroups,
			"exact_members", stats.ExactMembers,
			"image_features", stats.ImageFeatures,
			"image_pairs", stats.ImagePairs,
			"video_features", stats.VideoFeatures,
			"video_pairs", stats.VideoPairs,
			"groups_written", stats.GroupsWritten,
			"members_written", stats.MembersWritten,
			"skipped_pairs", stats.SkippedPairs,
			"bad_rows", stats.BadRows,
			"heap_alloc", stats.HeapAllocBytes,
		}
		if runErr != nil {
			a.log.Error("firstscreen run failed", append(attrs, "error", runErr)...)
			return
		}
		a.log.Info("firstscreen run done", attrs...)
	}()

	step := func(name string, f func() error) error {
		started := time.Now()
		err := f()
		elapsed := time.Since(started).Milliseconds()
		stats.StageElapsedMs[name] = elapsed
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		a.log.Info("firstscreen stage done", "stage", name, "elapsed_ms", elapsed)
		return nil
	}

	var exact []ExactGroup
	var imageFeatures []ImageFeature
	var imagePairs []CandidatePair
	var videoFeatures []VideoFeature
	var videoPairs []CandidatePair

	if err := step("exact_group", func() error {
		collector := &exactCollector{}
		err := a.store.StreamFilesBySHA(ctx, func(sha [64]byte, file FileRef) error {
			stats.FilesScanned++
			collector.add(sha, file)
			return nil
		})
		if err != nil {
			return err
		}
		exact = collector.finish()
		stats.ExactGroups = len(exact)
		for _, group := range exact {
			stats.ExactMembers += len(group.Members)
		}
		return nil
	}); err != nil {
		return stats, err
	}

	if err := step("image_load", func() error {
		var err error
		imageFeatures, err = a.store.LoadImageFeatures(ctx)
		stats.ImageFeatures = len(imageFeatures)
		return err
	}); err != nil {
		return stats, err
	}

	if err := step("image_screen", func() error {
		var err error
		imagePairs, err = a.screenImage(
			imageFeatures,
			a.cfg.HammingMax,
			a.cfg.AspectTolerance,
			a.cfg.ImageQualityMin,
		)
		stats.ImagePairs = len(imagePairs)
		return err
	}); err != nil {
		return stats, err
	}

	if err := step("video_load", func() error {
		var err error
		videoFeatures, err = a.store.LoadVideoFeatures(ctx)
		stats.VideoFeatures = len(videoFeatures)
		return err
	}); err != nil {
		return stats, err
	}

	if err := step("video_screen", func() error {
		var err error
		videoPairs, err = a.screenVideo(
			videoFeatures,
			a.cfg.VideoDurationWindowMs,
			a.cfg.HammingMax,
		)
		stats.VideoPairs = len(videoPairs)
		return err
	}); err != nil {
		return stats, err
	}

	if err := step("db_write", func() error {
		pairs := make([]CandidatePair, 0, len(imagePairs)+len(videoPairs))
		pairs = append(pairs, imagePairs...)
		pairs = append(pairs, videoPairs...)
		groups, members, skipped, err := a.store.ReplaceResults(ctx, exact, pairs)
		if err != nil {
			return err
		}
		stats.GroupsWritten = groups
		stats.MembersWritten = members
		stats.SkippedPairs = skipped
		if skipped > 0 {
			a.log.Warn("candidate pairs skipped: files rows not synced yet", "pairs", skipped)
		}
		return nil
	}); err != nil {
		return stats, err
	}

	return stats, nil
}
