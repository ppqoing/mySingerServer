package firstscreen

import (
	"context"
	"fmt"
	"runtime"
	"time"
)

type BenchmarkConfig struct {
	Rows        int
	ClusterSize int
	Seed        uint64
}

type BenchmarkResult struct {
	SchemaVersion  int    `json:"schema_version"`
	Kind           string `json:"kind"`
	Rows           int    `json:"rows"`
	ClusterSize    int    `json:"cluster_size"`
	Seed           uint64 `json:"seed"`
	ExpectedGroups int    `json:"expected_groups"`
	ActualGroups   int    `json:"actual_groups"`
	Candidates     int64  `json:"candidates"`
	BuildMS        int64  `json:"build_ms"`
	QueryMS        int64  `json:"query_ms"`
	TotalMS        int64  `json:"total_ms"`
	PeakHeapBytes  uint64 `json:"peak_heap_bytes"`
}

func RunBenchmark(
	ctx context.Context,
	cfg BenchmarkConfig,
) (BenchmarkResult, error) {
	if cfg.Rows < 100 || cfg.Rows > 2_000_000 {
		return BenchmarkResult{}, fmt.Errorf("benchscreen: rows must be in 100..2000000")
	}
	if cfg.ClusterSize < 2 || cfg.ClusterSize > 16 {
		return BenchmarkResult{}, fmt.Errorf("benchscreen: cluster size must be in 2..16")
	}
	if err := ctx.Err(); err != nil {
		return BenchmarkResult{}, err
	}
	started := time.Now()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	clusterRows := (cfg.Rows / 10 / cfg.ClusterSize) * cfg.ClusterSize
	clusterGroups := clusterRows / cfg.ClusterSize
	randomRows := cfg.Rows - clusterRows
	hashes := make([][4]uint64, 0, cfg.Rows)
	random := xorshift64{state: cfg.Seed}
	if random.state == 0 {
		random.state = 0x9e3779b97f4a7c15
	}
	for index := 0; index < randomRows; index++ {
		hashes = append(hashes, random.hash())
	}
	for group := 0; group < clusterGroups; group++ {
		base := random.hash()
		for member := 0; member < cfg.ClusterSize; member++ {
			current := base
			current[0] ^= uint64(1) << member
			hashes = append(hashes, current)
		}
	}

	parents := make([]uint32, cfg.Rows)
	sizes := make([]uint32, cfg.Rows)
	for index := range parents {
		parents[index] = uint32(index)
		sizes[index] = 1
	}
	find := func(value uint32) uint32 {
		for parents[value] != value {
			parents[value] = parents[parents[value]]
			value = parents[value]
		}
		return value
	}
	union := func(left, right uint32) {
		left, right = find(left), find(right)
		if left == right {
			return
		}
		if sizes[left] < sizes[right] {
			left, right = right, left
		}
		parents[right] = left
		sizes[left] += sizes[right]
	}

	index := newBandIndex(cfg.Rows)
	var scratch []uint32
	var queryDuration, buildDuration time.Duration
	var candidates int64
	for position, hash := range hashes {
		if position&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return BenchmarkResult{}, err
			}
		}
		stage := time.Now()
		scratch = index.query(hash, scratch)
		queryDuration += time.Since(stage)
		candidates += int64(len(scratch))
		for _, prior := range scratch {
			if hamming256(hash, hashes[prior]) <= 31 {
				union(uint32(position), prior)
			}
		}
		stage = time.Now()
		index.add(uint32(position), hash)
		buildDuration += time.Since(stage)
	}
	actualGroups := 0
	for index := range parents {
		if parents[index] == uint32(index) && sizes[index] > 1 {
			actualGroups++
		}
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	peak := uint64(0)
	if after.HeapAlloc > before.HeapAlloc {
		peak = after.HeapAlloc - before.HeapAlloc
	}
	return BenchmarkResult{
		SchemaVersion:  1,
		Kind:           "screen",
		Rows:           cfg.Rows,
		ClusterSize:    cfg.ClusterSize,
		Seed:           cfg.Seed,
		ExpectedGroups: clusterGroups,
		ActualGroups:   actualGroups,
		Candidates:     candidates,
		BuildMS:        buildDuration.Milliseconds(),
		QueryMS:        queryDuration.Milliseconds(),
		TotalMS:        time.Since(started).Milliseconds(),
		PeakHeapBytes:  peak,
	}, nil
}

type xorshift64 struct {
	state uint64
}

func (x *xorshift64) next() uint64 {
	value := x.state
	value ^= value << 13
	value ^= value >> 7
	value ^= value << 17
	x.state = value
	return value
}

func (x *xorshift64) hash() [4]uint64 {
	return [4]uint64{x.next(), x.next(), x.next(), x.next()}
}
