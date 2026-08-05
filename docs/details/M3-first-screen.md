# M3 一筛分析 — 详细实施文档

> 依据：`docs/architecture-plan.md` v1.1（§5.1 一筛流程、§6.2 中心库结果表、§9 默认参数、§10 里程碑 M3、§11 风险表）。
> 对应里程碑：**M3 一筛分析（1 周）**，验收标准："百万级特征一筛秒级出候选"。
> 依赖：M1（PostgreSQL 中心库表结构、上行链路、GUI 进程骨架）、M2（`image_features` / `video_features` 一阶段特征已写入中心库）。
> 本文档只覆盖 **GUI 进程内的一筛分析器**：从中心库读特征 → 生成候选 → 写回 `dup_groups` / `dup_members`。不改 Agent、不改通信协议、不改 `mediacore.dll`。

---

## 1. 目标与范围

### 1.1 目标

在 GUI 进程内实现一个可重复执行的一筛分析器（`firstscreen` 包），对中心库已有的一阶段特征做三类分析并落库：

1. **精确重复组**：按 `sha512` 分组，聚合跨 `machine_id` / `disk_no` 的全部路径，副本数 ≥2 成组。
2. **图片一筛**：PDQ-256（256bit = 4×64bit）按 4 个 64bit band 建倒排索引生成候选对（避免 O(n²) 全量两两比对），再依次经"长宽比差 ≤10% 剪枝 → 汉明距离 ≤ T1(31) 过滤"，PDQ Quality 双达标在 SQL 读取侧预过滤。
3. **视频一筛**：先按 `|duration 差| ≤ 2s` 滑窗剪枝，再对缩略图 PDQ-256 做汉明 ≤ T1(31) 过滤。

候选结果写入中心库 `dup_groups` / `dup_members`，`kind ∈ {exact, image_candidate, video_candidate}`，`score_json` 存一筛分数明细。重跑幂等。

性能目标（plan §10）：**百万级特征秒级出候选**。量化验收线见 §6.3（参考环境：GUI 机 16GB 内存、SSD、PostgreSQL 16 同机或千兆局域网）。

### 1.2 范围内

- `firstscreen` Go 包：特征批量读取（键集分页）、band 倒排、三类候选生成、结果事务化写回、分阶段耗时/内存指标。
- M3 所需中心库**索引** DDL（纯新增索引，不改已有表结构）。
- GUI 内部 HTTP 触发接口（GUI 进程内 Web 层，供页面按钮调用）。
- 单元测试 + 小规模 DB 集成测试 + 百万级大数据量验收测试（含合成数据生成器）。

### 1.3 不做什么

- **不做二阶段复筛**：不计算/比对分区 pHash、Sobel、视频 6 帧（M4）；不生成、不下发 `Phase2Task`（M4）。
- **不做相似组合并**：M3 的 `image_candidate` / `video_candidate` 只到"对"级别，不做并查集合并成相似组、不写 `kind=image/video`（M4）。
- **不做跨类型匹配**：图片特征与视频缩略图特征不互相比对。
- **不改 GUI↔Agent TCP 协议**：不新增 msgpack 消息类型（见 §4.12）。
- **不改 Agent 侧任何代码**，不动 `mediacore.dll` / Worker（见 §4.13）。
- **不做 UI 展示**：三类组的页面展示属 M4；M3 只保证数据正确落库并给出查询 SQL（§5.4）。
- **不做增量分析**：每轮全量重算 + 整类重写；增量触发与增量复筛属 M4/M6。
- **不做删除**（M5）。

### 1.4 前置契约（M1/M2 必须已保证）

| 契约 | 来源 | M3 的依赖方式 |
|---|---|---|
| 中心库已有 `files / image_features / video_features / dup_groups / dup_members` 表 | plan §6.2，M1 建表 | 直接读写 |
| `files.sha512` 为 `BYTEA`(64 字节)，可空；同内容多副本在 `files` 中有多行 | plan §6.1/§6.2 | 精确分组与成员反查 |
| `image_features.pdq256` / `video_features.thumb_pdq256` 为 `BYTEA`(32 字节)，**大端序**存储 4×uint64 | M2（mediacore 输出落库约定） | `pdqFromBytes` 解码（§4.3）。若 M2 实际为小端，只需改这一个函数 |
| `image_features.pdq_quality` 为整数（PDQ Quality，0~100），可空 | plan §6.1 | 质量双达标预过滤 |
| `video_features.duration_ms` 为整型毫秒，可空 | plan §6.1 | ±2s 剪枝 |
| `dup_groups.kind` 未被 CHECK 约束限定为三值 | M1 建表细节 | M3 需要写入 `image_candidate` / `video_candidate` 两个新枚举值，见 §5.2 与 §7 |
| GUI 配置中已有 PostgreSQL DSN | M1 | `Store` 取一条专用连接 |

---

## 2. 任务分解（checklist）

> 粒度到可单独验收；顺序即建议实施顺序。`[ ]` 未开始 / `[~]` 进行中 / `[x]` 完成。

- [x] **T1 建包与配置**：按 §3 建目录；落地 `config.go`（§4.2）全部字段与默认值；从 GUI 现有配置加载覆盖项（键名见 §5.5）。验收：`go build ./internal/firstscreen/` 通过。
- [x] **T2 汉明距离**：落地 `hamming.go`（§4.3）；单测 `TestHamming256` / `TestHamming256MutationConsistency` 通过（含与逐位朴素算法的随机对拍）。
- [x] **T3 band 倒排**：落地 `bandindex.go`（§4.4）；单测 `TestBandIndexRecallWithin3Bits` 通过（数学保证：汉明 ≤3 的对 100% 命中，见 §4.1 召回说明）。
- [x] **T4 图片一筛**：落地 `image_screen.go`（§4.6，含 `aspectClose`）；单测 `TestScreenImages` / `TestAspectClose` / `TestScreenImagesDeterministic` 通过。
- [x] **T5 视频一筛**：落地 `video_screen.go`（§4.7）；单测 `TestScreenVideosDurationBoundary` 通过（2000ms 通过 / 2001ms 剪掉的边界）。
- [x] **T6 精确分组**：落地 `exact.go`（§4.8）；单测 `TestExactCollector` 通过（跨 machine/disk 聚合、单副本不成组）。
- [x] **T7 中心库读取**：落地 `store.go` 的三路分页读取（§4.9）；集成测试验证分页不丢不重（构造跨页数据，页大小调小为 3 验证翻页）。
- [x] **T8 结果写回**：落地 `ReplaceResults`（§4.9）；集成测试 `TestIntegrationSmallDB` 通过（§6.2 用例，含重跑幂等断言）。
- [x] **T9 索引 DDL**：迁移文件合入 M1 迁移目录（§5.3）；对已有数据的库用 `CONCURRENTLY` 变体。验收：`\d+ files` 可见 `idx_files_sha512_id`。
- [x] **T10 编排与指标**：落地 `analyzer.go`（§4.10）；运行后 slog 输出 6 个阶段的 `elapsed_ms` 与行数/对数。
- [x] **T11 GUI 触发接线**：落地内部 HTTP 接口（§4.11）；`POST` 触发、`GET` 查询状态，重复触发返回 409。
- [x] **T12 大数据量验收**：落地 `acceptance_test.go`（§6.3）；百万级用例全部通过标准达成，实测数据填入 §6.3 结果表。
- [x] **T13 全量回归**：`go test ./internal/firstscreen/...` 全绿；integration 用例在 CI/本地 docker PG 16 可重复执行。

---

## 3. 目录与文件结构

以下路径相对 **GUI 模块根**（go.mod 由 M1 建立，示例 module 名 `mysinger/gui`，以 M1 实际为准）：

```
gui/
├── go.mod                                # M1 已建；M3 需要 github.com/jackc/pgx/v5（M1 上行链路已引入则复用，不新增第二种 PG 驱动）
├── internal/
│   └── firstscreen/
│       ├── config.go                     # Config 与默认值（§4.2）
│       ├── hamming.go                    # hamming256 / pdqFromBytes（§4.3）
│       ├── bandindex.go                  # 4×64bit band 倒排索引（§4.4）
│       ├── pairs.go                      # kind 常量 / CandidatePair / score_json（§4.5）
│       ├── image_screen.go               # 图片一筛（§4.6）
│       ├── video_screen.go               # 视频一筛（§4.7）
│       ├── exact.go                      # 精确重复组流式聚合（§4.8）
│       ├── store.go                      # PG 分页读取 + 结果事务化写回（§4.9）
│       ├── analyzer.go                   # Run 编排 + 分阶段指标（§4.10）
│       ├── firstscreen_test.go           # 单元测试与测试辅助函数（§6.1）
│       └── acceptance_test.go            // DB 集成 + 百万级验收（//go:build integration，§6.2/§6.3）
├── internal/web/                         # 以 M1 实际 web 层位置为准
│   └── analysis_handlers.go              # GUI 内部 HTTP 触发（§4.11）
└── db/migrations/                        # 以 M1 迁移目录为准
    └── 00XX_firstscreen_indexes.sql      # M3 新增索引（§5.3，编号顺延 M1 已有迁移）
```

说明：

- 分析逻辑全部在 `internal/firstscreen` 一个包内，对外只暴露 `Analyzer` / `Config` / `RunStats` / `Store`，GUI 其他模块不感知内部算法。
- 测试辅助（合成数据生成器）只存在于 `_test.go`，不进生产二进制。

---

## 4. 关键接口与结构体定义

### 4.1 候选生成算法（伪代码）

**算法 1：图片一筛（band 倒排 + 两级过滤）**

```
输入: F = image_features 全量（SQL 侧已过滤 pdq_quality ≥ Qmin 且 pdq256 非空）
输出: 候选对集合 P
1  I ← 空倒排索引   # key = (band号 0..3, 该段 64bit 值) → 已入库特征下标列表
2  for i, f ∈ F:
3      C ← ⋃_{b=0..3} I[(b, f.pdq[b])]     # 与 f 共享任一 64bit 段的全部"先来者"，去重
4      for j ∈ C:
5          g ← F[j]
6          if |ar(f) − ar(g)| / max(ar(f), ar(g)) > 10%:  continue   # 长宽比剪枝（plan §9）
7          d ← popcount(f.pdq XOR g.pdq)                            # 256bit 汉明距离
8          if d ≤ 31:  P ← P ∪ {(f, g, d)}                          # T1 过滤（plan §9）
9      把 i 追加进 I 的 4 个桶
10 return P
```

- 每个特征只与"先来者"配对 → 同一对只产出一次，无需全局去重。
- 复杂度：索引操作 O(4n) 次 map 存取 + Σ|候选桶| 次验证；桶长由数据相似度决定，随机数据桶长 ≈ 0。验证代价为 4 次 XOR+popcount（ns 级）。
- **召回特性（必须理解的设计取舍，plan §5.1 既定方案的固有性质）**：256bit 分 4 段，由鸽巢原理，汉明距离 ≤3 的对必共享至少一段 → **100% 命中**；距离 4~31 且差异位分散到 4 段的对**可能漏检**（极端如 8/8/8/7 分布）。真实近重复媒体（缩放开源/重压缩/水印）的差异通常集中在少数段，实测召回远高于该下界；本里程碑按 plan 接受该取舍，强化方案（第二套错位 band 布局）留 M6 评估，见 §7。

**算法 2：视频一筛（时长滑窗 + 汉明过滤）**

```
输入: V = video_features 全量（thumb_pdq256、duration_ms 非空）
输出: 候选对集合 P
1  按 duration_ms 升序排序 V
2  for i ∈ [0, n):
3      for j ∈ [i+1, n) 且 V[j].duration − V[i].duration ≤ 2000ms:   # 先时长剪枝（plan §9）
4          d ← popcount(V[i].thumb_pdq XOR V[j].thumb_pdq)
5          if d ≤ 31:  P ← P ∪ {(V[i], V[j], d, Δduration)}
6  return P
```

- 已排序 → 内层循环遇首个超窗即 `break`；复杂度 O(n·k)，k = ±2s 窗内平均条数。
- 视频缩略图规模通常远小于图片；时长剪枝先行是因为它是一次整数比较，比 popcount 更便宜，且窗内条数 k 通常很小。时长分布病态集中的风险见 §7。

**算法 3：精确重复组（流式分组）**

```
输入: files 表按 (sha512, id) 升序的键集分页流（sha512 非空）
1  顺序消费行；sha512 变化时 flush 当前缓冲
2  缓冲内行数 ≥2 → 输出一个 ExactGroup（成员跨 machine_id/disk_no 的全部路径）
```

### 4.2 配置（`config.go`）

```go
package firstscreen

// Config 一筛分析器配置。默认值见 DefaultConfig，与 architecture-plan §9 对齐；
// 标注 "M3 新增" 的为 plan 未给值、由本里程碑引入的调参（见 §5.5 配置项表）。
type Config struct {
	HammingMax            int     // T1：PDQ 汉明阈值（图片与视频缩略图共用），默认 31（plan §9）
	AspectTolerance       float64 // 图片长宽比宽容度，默认 0.10（plan §9）
	VideoDurationWindowMs int64   // 视频时长差剪枝窗口，默认 2000（plan §9）
	ImageQualityMin       int     // 图片 PDQ Quality 下限（双达标），默认 50（M3 新增调参）
	ReadPageSize          int     // 特征/文件键集分页大小，默认 50000（M3 新增）
	GroupInsertBatch      int     // dup_groups 单批 Batch 插入条数，默认 1000（M3 新增）
	SHAResolveChunk       int     // 候选 sha 反查 files 的 ANY 分块大小，默认 10000（M3 新增）
}

// DefaultConfig 返回与 plan §9 对齐的默认配置。
func DefaultConfig() Config {
	return Config{
		HammingMax:            31,
		AspectTolerance:       0.10,
		VideoDurationWindowMs: 2000,
		ImageQualityMin:       50,
		ReadPageSize:          50000,
		GroupInsertBatch:      1000,
		SHAResolveChunk:       10000,
	}
}
```

固定常量（不开放配置）：`bandCount = 4`（由 PDQ-256 = 4×64bit 结构决定，plan §5.1）；`sha512Len = 64`、`pdqLen = 32`。

### 4.3 汉明距离与 PDQ 字节序（`hamming.go`）

```go
package firstscreen

import (
	"encoding/binary"
	"math/bits"
)

// hamming256 计算两个 PDQ-256（4×64bit）的汉明距离。
func hamming256(a, b [4]uint64) int {
	return bits.OnesCount64(a[0]^b[0]) +
		bits.OnesCount64(a[1]^b[1]) +
		bits.OnesCount64(a[2]^b[2]) +
		bits.OnesCount64(a[3]^b[3])
}

// pdqFromBytes 将数据库存储的 32 字节 PDQ-256 解码为 4 个 uint64。
// 字节序契约：大端（与 M2 mediacore 落库约定一致）。若 M2 实际为小端，只改本函数。
func pdqFromBytes(b []byte) ([4]uint64, bool) {
	var h [4]uint64
	if len(b) != 32 {
		return h, false
	}
	for i := 0; i < 4; i++ {
		h[i] = binary.BigEndian.Uint64(b[i*8 : i*8+8])
	}
	return h, true
}
```

### 4.4 band 倒排索引（`bandindex.go`）

```go
package firstscreen

// bandKey 倒排索引键：第 band 段（0..3）+ 该段的 64bit 值。
type bandKey struct {
	band uint8
	val  uint64
}

// bandIndex 4×64bit band 倒排。单 goroutine 使用：先 query 消费结果，再 add。
// 内存估算见 §4.1 与 §6.3（约 90B/特征×4 桶）。
type bandIndex struct {
	m     map[bandKey][]uint32
	stamp []uint32 // 与特征下标等长的时间戳数组，query 内 O(1) 去重
	cur   uint32   // 当前查询时间戳
}

func newBandIndex(capHint int) *bandIndex {
	return &bandIndex{
		m:     make(map[bandKey][]uint32, capHint*4),
		stamp: make([]uint32, 0, capHint),
	}
}

// query 返回与 h 至少共享一个 64bit band 的已入库特征下标（去重、无序）。
// 返回切片复用 scratch，调用方必须在下一次 query 前消费完毕。
func (b *bandIndex) query(h [4]uint64, scratch []uint32) []uint32 {
	out := scratch[:0]
	b.cur++
	if b.cur == 0 { // uint32 回绕（约 42 亿次查询）防御：清零重来
		for i := range b.stamp {
			b.stamp[i] = 0
		}
		b.cur = 1
	}
	for band := uint8(0); band < 4; band++ {
		for _, idx := range b.m[bandKey{band: band, val: h[band]}] {
			if b.stamp[idx] != b.cur {
				b.stamp[idx] = b.cur
				out = append(out, idx)
			}
		}
	}
	return out
}

// add 把特征下标 idx 按 4 段入库。idx 为特征切片下标，允许跳号（质量过滤行不入库）。
func (b *bandIndex) add(idx uint32, h [4]uint64) {
	for band := uint8(0); band < 4; band++ {
		k := bandKey{band: band, val: h[band]}
		b.m[k] = append(b.m[k], idx)
	}
	for len(b.stamp) <= int(idx) { // stamp 与特征下标对齐，按需增长
		b.stamp = append(b.stamp, 0)
	}
}
```

设计要点：

- `stamp` 时间戳去重：一对特征可能共享多个 band，同一查询内用 O(1) 时间戳标记避免重复验证，避免每查询分配 `map[uint32]struct{}`（百万次查询的 GC 压力）。
- 只与"先来者"配对：pair (j, i) 中 j < i 恒成立，天然全局唯一。

### 4.5 候选对与分数（`pairs.go`）

```go
package firstscreen

import (
	"bytes"
	"encoding/hex"
	"fmt"
)

// dup_groups.kind 取值。image / video（复筛确认组）由 M4 写入，M3 不使用。
const (
	KindExact          = "exact"
	KindImageCandidate = "image_candidate"
	KindVideoCandidate = "video_candidate"
)

// M3Kinds 每次 Run 整体重写的 kind 集合（M4 产出的 kind 不受影响）。
var M3Kinds = []string{KindExact, KindImageCandidate, KindVideoCandidate}

// CandidatePair 一对一筛候选。ShaA 恒为字典序较小者（规范化，保证确定性）。
// 候选组的稳定标识 = (Kind, ShaA, ShaB)——dup_groups.id 会随重跑变化，禁止跨里程碑引用（见 §7）。
type CandidatePair struct {
	Kind           string
	ShaA           [64]byte
	ShaB           [64]byte
	Hamming        int   // 缩略图/图片 PDQ 汉明距离
	DurationDiffMs int64 // 仅视频：|时长差|
	QualityA       int   // 图片 PDQ Quality / 视频缩略图 Quality（ShaA 侧）
	QualityB       int
}

func newCandidatePair(kind string, s1, s2 [64]byte, hamming int, durDiffMs int64, q1, q2 int) CandidatePair {
	if bytes.Compare(s1[:], s2[:]) > 0 {
		s1, s2 = s2, s1
		q1, q2 = q2, q1
	}
	return CandidatePair{
		Kind:           kind,
		ShaA:           s1,
		ShaB:           s2,
		Hamming:        hamming,
		DurationDiffMs: durDiffMs,
		QualityA:       q1,
		QualityB:       q2,
	}
}

// less 提供确定性排序（按 ShaA, ShaB 字典序），便于测试对拍与人工比对。
func (p CandidatePair) less(q CandidatePair) bool {
	if c := bytes.Compare(p.ShaA[:], q.ShaA[:]); c != 0 {
		return c < 0
	}
	return bytes.Compare(p.ShaB[:], q.ShaB[:]) < 0
}

// scoreJSON 生成 dup_members.score_json（sideA=true 表示 ShaA 侧成员）。
// 结构约定见 §5.4。
func (p CandidatePair) scoreJSON(sideA bool) []byte {
	qSelf, qPeer := p.QualityA, p.QualityB
	peer := hex.EncodeToString(p.ShaB[:])
	if !sideA {
		qSelf, qPeer = p.QualityB, p.QualityA
		peer = hex.EncodeToString(p.ShaA[:])
	}
	if p.Kind == KindVideoCandidate {
		return []byte(fmt.Sprintf(
			`{"hamming":%d,"duration_diff_ms":%d,"quality_self":%d,"quality_peer":%d,"peer_sha512":"%s"}`,
			p.Hamming, p.DurationDiffMs, qSelf, qPeer, peer))
	}
	return []byte(fmt.Sprintf(
		`{"hamming":%d,"quality_self":%d,"quality_peer":%d,"peer_sha512":"%s"}`,
		p.Hamming, qSelf, qPeer, peer))
}
```

### 4.6 图片一筛（`image_screen.go`）

```go
package firstscreen

import (
	"math"
	"sort"
)

// ImageFeature 一行 image_features（pdq256 已解码）。
type ImageFeature struct {
	SHA512  [64]byte
	PDQ     [4]uint64
	Quality int
	Width   int
	Height  int
}

// aspectClose 长宽比剪枝：|r1-r2|/max(r1,r2) ≤ tol 视为相近。
// 任一侧尺寸缺失（≤0，解码异常行）时不启用剪枝、放行交由后续判定——宽/高是 plan §1 约定的一阶段附带产出，正常行必有。
func aspectClose(w1, h1, w2, h2 int, tol float64) bool {
	if w1 <= 0 || h1 <= 0 || w2 <= 0 || h2 <= 0 {
		return true
	}
	r1 := float64(w1) / float64(h1)
	r2 := float64(w2) / float64(h2)
	hi := math.Max(r1, r2)
	return (hi-math.Min(r1, r2))/hi <= tol
}

// screenImages 图片一筛。过滤顺序：质量双达标（行内过滤，与 SQL 预过滤一致）→
// band 倒排出候选 → 长宽比剪枝 → 汉明 ≤ hammingMax。
// 输出按 (ShaA, ShaB) 排序，结果确定。feats 只读。
func screenImages(feats []ImageFeature, hammingMax int, aspectTol float64, qualityMin int) []CandidatePair {
	idx := newBandIndex(len(feats))
	scratch := make([]uint32, 0, 256)
	var pairs []CandidatePair
	for i, f := range feats {
		if f.Quality < qualityMin {
			continue // 双达标：低质量行既不入索引也不参与配对（与 SQL 预过滤语义一致）
		}
		for _, j := range idx.query(f.PDQ, scratch) {
			g := feats[j]
			if !aspectClose(f.Width, f.Height, g.Width, g.Height, aspectTol) {
				continue
			}
			d := hamming256(f.PDQ, g.PDQ)
			if d > hammingMax {
				continue
			}
			pairs = append(pairs, newCandidatePair(KindImageCandidate, f.SHA512, g.SHA512, d, 0, f.Quality, g.Quality))
		}
		idx.add(uint32(i), f.PDQ)
	}
	sort.Slice(pairs, func(a, b int) bool { return pairs[a].less(pairs[b]) })
	return pairs
}
```

### 4.7 视频一筛（`video_screen.go`）

```go
package firstscreen

import (
	"bytes"
	"sort"
)

// VideoFeature 一行 video_features（thumb_pdq256 已解码）。
// 视频一筛不做质量过滤（plan §5.1 未要求）；ThumbQuality 仅随 score_json 落库供 M4 参考。
type VideoFeature struct {
	SHA512       [64]byte
	DurationMs   int64
	ThumbPDQ     [4]uint64
	ThumbQuality int
}

// screenVideos 视频一筛：先 |时长差| ≤ windowMs 滑窗剪枝，再缩略图 PDQ-256 汉明 ≤ hammingMax。
// 注意：会就地对 feats 按时长排序（调用方不再复用该切片）。输出按 (ShaA, ShaB) 排序。
func screenVideos(feats []VideoFeature, windowMs int64, hammingMax int) []CandidatePair {
	sort.Slice(feats, func(i, j int) bool {
		if feats[i].DurationMs != feats[j].DurationMs {
			return feats[i].DurationMs < feats[j].DurationMs
		}
		return bytes.Compare(feats[i].SHA512[:], feats[j].SHA512[:]) < 0
	})
	var pairs []CandidatePair
	for i := 0; i < len(feats); i++ {
		a := feats[i]
		for j := i + 1; j < len(feats); j++ {
			b := feats[j]
			diff := b.DurationMs - a.DurationMs // b ≥ a（已排序）
			if diff > windowMs {
				break
			}
			d := hamming256(a.ThumbPDQ, b.ThumbPDQ)
			if d > hammingMax {
				continue
			}
			pairs = append(pairs, newCandidatePair(KindVideoCandidate, a.SHA512, b.SHA512, d, diff, a.ThumbQuality, b.ThumbQuality))
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].less(pairs[j]) })
	return pairs
}
```

### 4.8 精确重复组聚合（`exact.go`）

```go
package firstscreen

// FileRef files 表一行的分析视图。
type FileRef struct {
	ID        int64
	MachineID string
	DiskNo    int
	Path      string
	Size      int64
}

// ExactGroup 同一 SHA-512 的全部文件副本（≥2 行，可跨 machine_id/disk_no）。
type ExactGroup struct {
	SHA512  [64]byte
	Members []FileRef // 按 files.id 升序（输入流即 (sha512,id) 升序）
}

// exactCollector 流式聚合器：输入必须按 (sha512, id) 升序到达（由 Store 的 SQL 保证）。
// 内存只缓存"当前 sha"的缓冲与全部成组结果。
type exactCollector struct {
	cur    [64]byte
	has    bool
	buf    []FileRef
	groups []ExactGroup
}

func (c *exactCollector) add(sha [64]byte, f FileRef) {
	if c.has && sha != c.cur {
		c.flush()
	}
	c.has = true
	c.cur = sha
	c.buf = append(c.buf, f)
}

func (c *exactCollector) flush() {
	if len(c.buf) >= 2 { // 单副本不成组
		members := make([]FileRef, len(c.buf))
		copy(members, c.buf)
		c.groups = append(c.groups, ExactGroup{SHA512: c.cur, Members: members})
	}
	c.buf = c.buf[:0]
}

// finish 冲刷尾部并返回全部组。调用后 collector 不可再用。
func (c *exactCollector) finish() []ExactGroup {
	if c.has {
		c.flush()
	}
	return c.groups
}
```

### 4.9 中心库读写（store.go）

> 本节描述生产可信契约与关键代码形态；若示例与源码发生差异，以
> internal/firstscreen/store.go 为唯一权威。示例省略重复扫描与错误包装细节，
> 但下列类型、事务边界、数据质量和提交语义不得弱化。

#### 4.9.1 存储边界与中心库键类型

中心库 sha512 是 canonical lowercase、128 字符十六进制 TEXT。Go 算法边界才用
[64]byte；数据库读写一律传输 TEXT，并通过 shaFromText 验证。Store 一次 Run
独占、非并发安全；字段依赖窄接口，便于包装真实 pgx 事务做失败边界注入：

~~~go
type storeConn interface {
    Query(context.Context, string, ...any) (pgx.Rows, error)
    BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

type Store struct {
    conn    storeConn
    cfg     Config
    badRows int
}
~~~

正常构造由 NewStore 把应用的 pgx 连接放入 storeConn。读接口只依赖 Query，写接口
只额外依赖 BeginTx；不得把 Store.conn 收窄回具体连接类型。

#### 4.9.2 三个键集读取器

首分页游标必须是真正 SQL NULL，而不是空字符串哨兵。实现使用 invalid pgtype.Text；
读到每行后，无论是否采用，都先用原始 TEXT 更新游标，保证坏行在页边界也会终止。

图片页：

~~~sql
SELECT sha512,width,height,pdq256,pdq_quality
FROM image_features
WHERE ($1::text IS NULL OR sha512 > $1::text)
  AND pdq256 IS NOT NULL
  AND pdq_quality >= $2
ORDER BY sha512
LIMIT $3
~~~

视频页：

~~~sql
SELECT sha512,duration_ms,thumb_pdq256,thumb_quality
FROM video_features
WHERE ($1::text IS NULL OR sha512 > $1::text)
  AND thumb_pdq256 IS NOT NULL
  AND duration_ms IS NOT NULL
ORDER BY sha512
LIMIT $2
~~~

图片应用 ImageQualityMin；视频不做质量过滤，thumb_quality 为 NULL 时映射为 0。
两类 feature 行均执行 shaFromText 与 32 字节 PDQ 解码。任一失败时跳过整行并令
Store.badRows 加 1；同一坏行只计一次。BadRows 是 Store 实例局部计数。

files 按 (sha512,id) 全局有序流式读取：

~~~sql
SELECT sha512,id,machine_id,disk_no,path,size
FROM files
WHERE sha512 IS NOT NULL
  AND ($1::text IS NULL OR (sha512,id) > ($1::text,$2))
ORDER BY sha512,id
LIMIT $3
~~~

files.sha512 是身份级数据：shaFromText 失败必须立即返回 hard error，不得跳过或计入
badRows。回调、context、Query、Scan、Rows 错误均保留 cause。部分索引
idx_files_sha512_id 支撑该行值键集扫描。

#### 4.9.3 候选 SHA 反查

ReplaceResults 只反查 candidate pair 两侧；exact group 已携带 FileRef，不重复查询。
反查必须：

1. 把 [64]byte 编码为 canonical lowercase 128 字符 TEXT；
2. 对 TEXT 排序，使分块与查询顺序确定；
3. 每块最多 SHAResolveChunk 个 SHA；
4. 使用 TEXT 数组查询并按 (sha512,id) 排序；
5. 对返回 TEXT 再用 shaFromText 验证，非法值为 hard error。

~~~sql
SELECT id,sha512,machine_id,disk_no,path,size
FROM files
WHERE sha512 = ANY($1::text[])
ORDER BY sha512,id
~~~

返回 map[[64]byte][]FileRef；同一 SHA 的 FileRef 按 id 升序，包含全部副本。

#### 4.9.4 写入前组装契约

exact：

- Members 为空直接跳过；
- Members 可为任意顺序，代表必须显式扫描并取最小 file ID，禁止依赖第一个成员；
- 所有给定成员均写入，score 固定为 {"basis":"sha512"}。

candidate：

- ShaA 或 ShaB 任一侧无 files 时整对跳过，skipped 加 1，禁止半组；
- 代表是 canonical ShaA 侧全部副本中的最小 file ID；
- ShaA、ShaB 两侧全部副本都成为成员；
- score_json 由 CandidatePair.scoreJSON(sideA) 生成，self/peer 质量与 peer SHA 必须
  随成员侧别正确互换；video 还包含 duration_diff_ms；
- member_count 等于该组实际计划写入的成员行数。

M3 只组装 exact、image_candidate、video_candidate。image、video 属 M4，既不删除
也不重写。

#### 4.9.5 单事务整类替换

接口保持：

~~~go
func (s *Store) ReplaceResults(
    ctx context.Context,
    exact []ExactGroup,
    pairs []CandidatePair,
) (groupsWritten, membersWritten, skipped int, err error)
~~~

关键顺序：

1. BeginTx，隔离级别 pgx.RepeatableRead；
2. 仅针对 M3Kinds 删除 dup_members，再删除 dup_groups；
3. 按 canonical TEXT、排序和 chunk 规则反查 candidate files；
4. 按 GroupInsertBatch 将 dup_groups 排入 pgx.Batch，每条 RETURNING id；
5. 每个返回 group ID 与其成员展开为 CopyFrom 行；
6. BatchResults 在成功和行扫描失败路径都必须 Close；
7. 用一次 CopyFrom 写入 dup_members；
8. 所有远程操作成功后只调用一次 Commit。

~~~sql
DELETE FROM dup_members
WHERE group_id IN (
    SELECT id FROM dup_groups WHERE kind = ANY($1::text[])
);

DELETE FROM dup_groups
WHERE kind = ANY($1::text[]);
~~~

组用 Batch + RETURNING id，成员用 CopyFrom：

~~~go
for each bounded group chunk {
    batch := new(pgx.Batch)
    queue every group with representative and len(members)
    results := tx.SendBatch(ctx, batch)
    scan every returned group ID
    close results
}
copy all member rows to dup_members
~~~

成功返回实际写入的 group/member 数和 skipped。任何错误或提交结果未知时三个返回计数
均为 0，调用者不得把内存阶段计数当成已提交事实。

#### 4.9.6 rollback 与 Commit outcome

事务开始后的所有错误都尝试 rollback。rollback 不复用已取消的调用 context，必须
去掉取消信号并设置有限时限：

~~~go
defer func() {
    if err == nil {
        return
    }
    rollbackCtx, cancel := context.WithTimeout(
        context.WithoutCancel(ctx),
        5*time.Second,
    )
    defer cancel()
    _ = tx.Rollback(rollbackCtx)
}()
~~~

Commit 错误分两类：

- errors.Is(commitErr, pgx.ErrTxCommitRollback)：服务端明确回滚，旧 M3 结果仍在；
- 其他 Commit 错误：outcome ambiguous。服务端可能未提交，也可能已提交但 ACK
  丢失。必须保留原 cause，并增加 ErrCommitOutcomeUnknown 标记。

~~~go
var ErrCommitOutcomeUnknown =
    errors.New("firstscreen: commit outcome unknown")

if commitErr := tx.Commit(ctx); commitErr != nil {
    if errors.Is(commitErr, pgx.ErrTxCommitRollback) {
        return 0, 0, 0, fmt.Errorf(
            "commit result replacement rolled back: %w",
            commitErr,
        )
    }
    return 0, 0, 0, errors.Join(
        ErrCommitOutcomeUnknown,
        fmt.Errorf("commit result replacement: %w", commitErr),
    )
}
~~~

ambiguous 分支不得声称旧 M3 或新 M3 存在。调用者通过 errors.Is 识别
ErrCommitOutcomeUnknown，并以完全相同的 exact/pairs 输入重试整个 ReplaceResults。
同事务“删除 M3 三类 + 全量重写”使相同输入幂等 reconcile 到同一语义结果，同时
继续保留 M4 image/video 数据。

### 4.10 分析编排（`analyzer.go`）

> **权威性**：生产实现以 `internal/firstscreen/analyzer.go` 为唯一权威；本节规定必须保持的源码级契约，不再复制一份容易漂移的完整实现。

#### 4.10.1 边界与依赖注入

```go
type analyzerStore interface {
	StreamFilesBySHA(context.Context, func([64]byte, FileRef) error) error
	LoadImageFeatures(context.Context) ([]ImageFeature, error)
	LoadVideoFeatures(context.Context) ([]VideoFeature, error)
	ReplaceResults(context.Context, []ExactGroup, []CandidatePair) (int, int, int, error)
	BadRows() int
}

func NewAnalyzer(store *Store, cfg Config, log *slog.Logger) *Analyzer {
	return newAnalyzer(store, cfg, log)
}
```

公开构造器继续接收 `*Store`，不扩大 M3 的公共 API；`Analyzer` 内部只依赖上述窄接口，测试可替换数据库边界。`Analyzer` 另持有两个包内筛选函数依赖：

- `screenImage func([]ImageFeature, int, float64, int) ([]CandidatePair, error)`
- `screenVideo func([]VideoFeature, int64, int) ([]CandidatePair, error)`

生产默认适配现有纯函数 `screenImages` / `screenVideos`，因此不会凭空引入算法错误；返回 `error` 只用于包内编排测试强制覆盖 `image_screen` 和 `video_screen` 失败。这个注入点不是公开扩展机制。

一个 `Analyzer` 顺序复用构造时传入的同一个 `Store`。一次 `Run` 不创建第二个 Store，也不启动算法 goroutine；当前 Store 与 Analyzer 都不承诺并发安全，调用方必须串行执行 `Run`。

#### 4.10.2 指标契约

```go
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
```

`StageElapsedMs` 在 Run 开始时必须预置以下六个键，即使阶段未执行或耗时不足 1 ms 也必须存在：

1. `exact_group`
2. `image_load`
3. `image_screen`
4. `video_load`
5. `video_screen`
6. `db_write`

每个阶段使用 `time.Now()` 与 `time.Since(started)` 的单调时间部分计时。成功和失败阶段都写回耗时；因早期失败未执行的后续阶段保持 `0`。每个成功阶段输出 `firstscreen stage done` 及 `stage`、`elapsed_ms`。

`Store.BadRows()` 是 Store 生命周期内的累加计数，不是天然的单轮指标。每次 Run 必须在任何阶段开始前记录 baseline，并在所有返回路径计算：

```go
stats.BadRows = store.BadRows() - badRowsBaseline
```

因此连续复用同一个 Analyzer 时，每轮只报告本轮新增坏行；第二轮在 `exact_group` 早退时为 `0`，不会泄露第一轮历史值。

`Run` 使用 defer 覆盖成功和失败两类返回路径：先填充 run-local `BadRows`，再执行 `runtime.GC()`，随后 `runtime.ReadMemStats()` 填充 `HeapAllocBytes`，最后记录整轮成功或失败结构化日志。日志必须包含扫描行数、精确成员/组、图片/视频特征和候选对、写入组/成员、跳过对、坏行及堆占用。`HeapAllocBytes` 是 GC 后瞬时值，不是峰值；峰值仍按 §6.3 外部采样。

#### 4.10.3 六阶段状态机与失败语义

六个阶段必须是独立、顺序的顶层 step，不允许把 screen 嵌套进 load：

```text
exact_group → image_load → image_screen → video_load → video_screen → db_write
```

- step 对底层错误使用 `fmt.Errorf("%s: %w", stage, err)` 包装，保留 `errors.Is/As` cause。
- 任一阶段失败立即返回同一个 `*RunStats`，已完成阶段统计保留，失败阶段耗时已记录，后续阶段保持零值。
- `db_write` 是唯一调用 `ReplaceResults` 的阶段；任一更早阶段失败都绝不写库。
- 图片和视频候选使用新的目标切片，严格先 append 图片对、再 append 视频对；不得用可能复用输入 backing array 的原地拼接。
- `ReplaceResults` 自身以单事务重写三类 M3 结果。只有 `err == nil` 时，Analyzer 才采纳 `GroupsWritten`、`MembersWritten`、`SkippedPairs` 并在 `skipped > 0` 时输出 warning。
- `ReplaceResults` 返回错误时，即使驱动或测试替身同时返回非零计数，三项写入统计也必须保持 `0`，且不得输出 skipped warning；对外仍返回 `db_write` 阶段前缀和原始 cause。

### 4.11 GUI 内部触发（HTTP，进程内 Web 层）

> **权威性**：HTTP 生命周期以 `internal/gui/analysis.go`、路由组合以 `internal/gui/httpapi.go`、数据库连接与 Analyzer 组合以 `cmd/gui/main.go` 为唯一权威。本节只规定必须保持的生产契约。

M3 不增加 GUI↔Agent 协议；触发走 GUI 自身 Web 服务。HTTP 层依赖以下窄接口：

```go
type AnalysisRunner interface {
	Run() (*firstscreen.RunStats, error)
}

type analysisStatus struct {
	Running bool                  `json:"running"`
	Last    *firstscreen.RunStats `json:"last"`
	LastErr string                `json:"last_err"`
}
```

`AnalysisRunner.Run()` 不接收 HTTP request context。生产 runner 在构造时捕获 GUI 进程的 shutdown context，所以：

- POST 返回或客户端取消请求不会取消已经接受的分析；
- 收到进程 shutdown 信号时，同一个 context 会取消 Acquire 或正在执行的 `Analyzer.Run`；
- 不使用 request context，也不以 `context.Background()` 伪造一个无法响应 shutdown 的生命周期。

`NewAPI(pool, tasks, pg, runners ...AnalysisRunner)` 使用类型安全的 variadic 可选依赖。现有三参数调用继续编译；未传 runner 时两个路由仍注册，POST 与 GET 都明确返回 `503`，而不是 nil panic。GET 的 `503` body 仍稳定包含 `running`、`last`、`last_err` 三字段。

#### 4.11.1 HTTP 状态机

路由固定为：

- `POST /api/analysis/firstscreen/run`
- `GET /api/analysis/firstscreen/status`

POST 在 mutex 内完成 runner 可用性检查和 admission：

- 无 runner：`503 Service Unavailable`；
- 已进入进程 shutdown：`503 Service Unavailable`，不接受新一轮；
- 已有一轮运行：`409 Conflict`；
- 空闲：先置 `running=true`，再启动唯一必要的 HTTP 异步 goroutine，立即返回 `202 Accepted`。

分析算法自身不在 GUI 层创建 goroutine；六阶段仍由 `Analyzer.Run` 顺序执行。

异步 goroutine 必须用 defer 统一收尾，并 recover runner panic。成功、普通错误、panic 三条路径都将 `running` 还原为 false；普通错误文本和 panic 文本进入 `last_err`，普通错误同时保留 runner 返回的 partial `last`。panic 不得使 GUI 进程崩溃或让状态永久停在 running。

GET 返回：

```json
{
  "running": false,
  "last": null,
  "last_err": ""
}
```

三个 JSON 字段始终存在。`AnalysisHandlers` 在 mutex 内持有状态；接收 runner 结果时复制 `RunStats` 及其 `StageElapsedMs` map，生成每次 GET 快照时再次复制，随后解锁再编码。不得把内部指针/map 直接交给并发 encoder，或保留 runner 所有的可变指针。

#### 4.11.2 每轮专用数据库连接

`cmd/gui/main.go` 的生产组合使用窄 pool/connection/factory 边界以便单测：

```go
type analysisPoolConn interface {
	Conn() *pgx.Conn
	Release()
}

type analysisPool interface {
	Acquire(context.Context) (analysisPoolConn, error)
}

type analysisEngine interface {
	Run(context.Context) (*firstscreen.RunStats, error)
}
```

`pooledAnalysisRunner` 捕获 process shutdown context、pool、firstscreen Config、logger 与 Analyzer factory。每次 `Run()` 都必须：

1. 使用 shutdown context 独立调用一次 `pgxpool.Acquire`；
2. Acquire 成功后立即 `defer Release()`；
3. 用本轮连接的 `conn.Conn()` 构造 `firstscreen.NewStore`；
4. 用该 Store 构造新的 `firstscreen.NewAnalyzer`；
5. 把同一个 shutdown context 传给 `Analyzer.Run`。

因此两轮运行有两次独立 Acquire/Release，不长期占用或共享 `*pgx.Conn`。Analyzer 成功、返回错误或 panic 展开时都执行 Release；Acquire 失败不构造 Store/Analyzer、不产生连接泄漏，并将带 cause 的 acquire 错误交给 HTTP 状态机显示在 `last_err`。M4 可在同一 runner 边界上增加普扫完成后的自动触发，M3 仅提供手动 HTTP 入口。

#### 4.11.3 进程 shutdown 门禁与等待

HTTP handler 返回 `202` 后，分析 goroutine 已脱离该 request，
`http.Server.Shutdown` 不会替 GUI 等待它。因此 GUI 必须显式协调：

资源型入口必须是返回 error 的 `run(args)`；只允许无资源的 `main` 在
`run` 完整返回后记录最终错误并调用 `os.Exit`。不得在已经创建 pool、
专用连接或 goroutine 的函数内部 `os.Exit`，否则 defer Release/Close 会被跳过。

`serveAndDrain` 通过窄 `guiHTTPServer` 与 `analysisLifecycle` 接口编排。
无论 `ListenAndServe` 因信号返回 `http.ErrServerClosed`、因 listener 故障
返回其他 error，还是返回 nil，都执行同一个尾部：

1. 调用 `BeginAnalysisShutdown()`，在 mutex 内永久关闭 analysis POST
   admission，此后的 POST 返回 `503`；
2. 调用 process cancel，使 Acquire、Analyzer 和 Agent pool 收到取消；
3. 调用 `WaitForAnalysis()`，等待已接受 run 的 defer 收尾并关闭 `runDone`；
4. 等待唯一的 server shutdown goroutine返回；该 goroutine 也只调用一次
   `Server.Shutdown`，不会因共同尾部产生双 Shutdown；
5. 对非 `ErrServerClosed` 的 serve error 保留 cause 并返回；signal
   `ErrServerClosed` 与 nil 正常返回不误报；
6. `run` 返回时执行 defer `pgxpool.Close`，最后才回到 `main`。

这保证 admission close → process cancel → runner exit → 专用连接 Release →
Wait 返回 → pool Close 的顺序。pgxpool v5.7.2 的 `Close` 本身也会阻塞到
借出连接归还，但这里只把它作为最终资源兜底，不依赖它代替 HTTP admission
与 goroutine join。

### 4.12 通信协议影响（msgpack）

**本里程碑不新增、不修改任何 GUI↔Agent TCP 消息**（plan §7 消息集保持不变）。一筛的输入全部来自中心库，输出全部写回中心库；Agent 不感知一筛发生。M4 的 `Phase2Task` 才会消费 M3 的候选结果，消息语义沿用 plan §7 已定定义。

### 4.13 C++ / cgo 影响

**无**。一筛消费的 PDQ-256 / Quality / 时长 / 宽高全部在 M2 已由 `mediacore.dll` 产出并上行；M3 为纯 Go、纯内存计算，不加载 DLL、不引入 cgo。唯一跨语言契约是 PDQ-256 的 32 字节大端存储序（§4.3）。

---

## 5. 数据模型与配置项

### 5.1 读取的表（plan §6.2 已定结构，此处仅引用）

```sql
-- M1 已建。M3 使用到的列：
-- files:               id, machine_id, disk_no, path, size, sha512
-- image_features:      sha512(PK), width, height, pdq256, pdq_quality
-- video_features:      sha512(PK), duration_ms, thumb_pdq256, thumb_quality
-- 类型约定：sha512 BYTEA(64B)；pdq256/thumb_pdq256 BYTEA(32B 大端)；
--           width/height/pdq_quality/thumb_quality INTEGER；duration_ms BIGINT。
```

### 5.2 写入的表与 kind 枚举

```sql
-- M1 已建（plan §6.2），M3 原样使用，不改表结构：
CREATE TABLE IF NOT EXISTS dup_groups (
    id                    BIGSERIAL PRIMARY KEY,
    kind                  TEXT NOT NULL,       -- 枚举见下
    representative_file_id BIGINT REFERENCES files(id),
    member_count          INT  NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS dup_members (
    group_id   BIGINT NOT NULL REFERENCES dup_groups(id) ON DELETE CASCADE,
    file_id    BIGINT NOT NULL REFERENCES files(id),
    score_json JSONB,
    PRIMARY KEY (group_id, file_id)
);
```

`kind` 枚举全集与写入方：

| kind | 含义 | 写入方 |
|---|---|---|
| `exact` | 精确重复组（SHA-512 一致，≥2 副本） | **M3** |
| `image_candidate` | 图片一筛候选对（未经复筛确认） | **M3** |
| `video_candidate` | 视频一筛候选对（未经复筛确认） | **M3** |
| `image` / `video` | 复筛确认后的相似图片组 / 相似视频组 | M4（M3 不触碰） |

**重跑幂等语义**：每次 `Run` 在单事务内 `DELETE` 三类 M3 kind 的全部旧行后重写。因此：

- `dup_groups.id` 不稳定，跨里程碑/跨模块引用候选组必须用内容键 `(kind, sha_a, sha_b)`（M4 契约，见 §7）。
- `representative_file_id` 取值规则：exact 组 = 组内最小 `file_id`；候选组 = ShaA（字典序较小 sha）侧最小 `file_id`，保证重跑确定性。

### 5.3 M3 新增索引（纯新增，不改表结构）

迁移文件 `db/migrations/00XX_firstscreen_indexes.sql`（编号顺延 M1 迁移）：

```sql
-- 精确分组的流式有序扫描：部分索引只覆盖 sha512 非空行，
-- 使 qFilesBySHAPage 走纯索引有序扫描，避免百万行排序。
CREATE INDEX IF NOT EXISTS idx_files_sha512_id
    ON files (sha512, id) WHERE sha512 IS NOT NULL;

-- 结果表按 kind 清理/过滤；dup_members 双向检索（按组、按文件）。
CREATE INDEX IF NOT EXISTS idx_dup_groups_kind   ON dup_groups (kind);
CREATE INDEX IF NOT EXISTS idx_dup_members_file  ON dup_members (file_id);
-- dup_members(group_id) 已由主键 (group_id, file_id) 覆盖，无需单独建。

-- 说明：
-- 1. image_features / video_features 的键集分页走主键 sha512 有序扫描，无需新增索引；
--    质量/非空过滤是顺序过滤条件，全表 sweep 场景下建索引无收益。
-- 2. 视频时长滑窗在 Go 内存排序完成，不为 duration_ms 建索引；
--    若后续 GUI 有按时长浏览的页面需求再议。
-- 3. 对已有大量数据的存量库，改用 CONCURRENTLY 变体逐一执行（不能在事务块内）：
--    CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_files_sha512_id ON files (sha512, id) WHERE sha512 IS NOT NULL;
--    CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_dup_groups_kind  ON dup_groups (kind);
--    CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_dup_members_file ON dup_members (file_id);
```

### 5.4 `score_json` 结构约定

| kind | 内容 | 示例 |
|---|---|---|
| `exact` | 固定 `{"basis":"sha512"}` | `{"basis":"sha512"}` |
| `image_candidate` | 一筛分数 | `{"hamming":17,"quality_self":82,"quality_peer":76,"peer_sha512":"ab12…"}` |
| `video_candidate` | 一筛分数（含时长差） | `{"hamming":9,"duration_diff_ms":380,"quality_self":70,"quality_peer":66,"peer_sha512":"cd34…"}` |

- 同一候选组所有成员的 `hamming`（及视频的 `duration_diff_ms`）相同；`quality_self` 为本行质量、`quality_peer` 为对端质量、`peer_sha512` 为对端 sha 的小写 hex。
- 视频的 `quality_*` 是缩略图 PDQ Quality，仅记录不过滤（plan §5.1）。
- M4 复筛后会在**新 kind**（`image`/`video`）的成员里追加 pHash/Sobel/6 帧分数，不回写 M3 的行。

M4/UI 查询示例（验收也用它做对拍）：

```sql
-- 候选对列表（展开为路径对）
SELECT g.id, g.kind,
       fa.path AS path_a, fb.path AS path_b,
       ma.score_json->>'hamming' AS hamming
FROM dup_groups g
JOIN dup_members ma ON ma.group_id = g.id
JOIN files fa ON fa.id = ma.file_id
JOIN dup_members mb ON mb.group_id = g.id AND mb.file_id > ma.file_id
JOIN files fb ON fb.id = mb.file_id
WHERE g.kind = 'image_candidate'
ORDER BY (ma.score_json->>'hamming')::int
LIMIT 100;
```

### 5.5 配置项表（GUI 配置文件，与 plan §9 对齐）

| 配置键 | Go 字段 | 默认值 | 来源 | 说明 |
|---|---|---|---|---|
| `firstscreen.hamming_max` | `HammingMax` | 31 | plan §9 T1 | 图片/视频缩略图 PDQ 汉明阈值 |
| `firstscreen.aspect_tolerance` | `AspectTolerance` | 0.10 | plan §9 | 图片长宽比宽容度 |
| `firstscreen.video_duration_window_ms` | `VideoDurationWindowMs` | 2000 | plan §9 | 视频时长差剪枝窗口 |
| `firstscreen.image_quality_min` | `ImageQualityMin` | **50** | M3 新增 | 图片 PDQ Quality 双达标下限；plan 只要求"双达标"未给值，此默认待架构确认（§7） |
| `firstscreen.read_page_size` | `ReadPageSize` | 50000 | M3 新增 | 特征/文件键集分页大小 |
| `firstscreen.group_insert_batch` | `GroupInsertBatch` | 1000 | M3 新增 | `dup_groups` Batch 插入批大小 |
| `firstscreen.sha_resolve_chunk` | `SHAResolveChunk` | 10000 | M3 新增 | 候选 sha 反查 `files` 的 ANY 分块 |
| `postgres.dsn` | （M1 已有） | — | M1 | 中心库连接串，复用不重复定义 |

固定常量：`band = 4×64bit`（PDQ-256 结构决定，plan §5.1）。

---

## 6. 测试与验收用例

### 6.1 单元测试（`firstscreen_test.go`，纯内存、无需数据库）

运行：`go test ./internal/firstscreen/`。通过标准：全部用例通过；`hamming256` / `aspectClose` / `bandIndex` / 两个 `screen*` 的核心分支覆盖到位。

```go
package firstscreen

import (
	"encoding/binary"
	"math/rand"
	"reflect"
	"testing"
)

// ---------- 测试辅助（acceptance_test.go 也复用） ----------

func randBytes(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	rng.Read(b)
	return b
}

func randPDQ(rng *rand.Rand) [4]uint64 {
	return [4]uint64{rng.Uint64(), rng.Uint64(), rng.Uint64(), rng.Uint64()}
}

// mutatePDQ 在全部 256 bit 内随机翻转 bits 个不同位。
func mutatePDQ(base [4]uint64, bits int, rng *rand.Rand) [4]uint64 {
	out := base
	used := make(map[int]struct{}, bits)
	for len(used) < bits {
		p := rng.Intn(256)
		if _, ok := used[p]; ok {
			continue
		}
		used[p] = struct{}{}
		out[p/64] ^= 1 << uint(p%64)
	}
	return out
}

// mutateLow128 只在前 128 bit（band 0/1）内翻转，保证 band 2/3 与原哈希一致（候选必命中）。
func mutateLow128(base [4]uint64, bits int, rng *rand.Rand) [4]uint64 {
	out := base
	used := make(map[int]struct{}, bits)
	for len(used) < bits {
		p := rng.Intn(128)
		if _, ok := used[p]; ok {
			continue
		}
		used[p] = struct{}{}
		out[p/64] ^= 1 << uint(p%64)
	}
	return out
}

func flip(h [4]uint64, pos ...int) [4]uint64 {
	for _, p := range pos {
		h[p/64] ^= 1 << uint(p%64)
	}
	return h
}

func pdqBytes(h [4]uint64) []byte {
	b := make([]byte, 32)
	for i := 0; i < 4; i++ {
		binary.BigEndian.PutUint64(b[i*8:], h[i])
	}
	return b
}

func sha(b byte) (s [64]byte) { s[0] = b; return }

// shaSlice 同 sha，返回 []byte（CopyFrom 行值用；函数返回的数组不可直接切片）。
func shaSlice(b byte) []byte { s := sha(b); return s[:] }

// ---------- 用例 ----------

func TestHamming256(t *testing.T) {
	if got := hamming256([4]uint64{}, [4]uint64{}); got != 0 {
		t.Fatalf("self hamming = %d, want 0", got)
	}
	a := [4]uint64{0, 0, 0, 0}
	b := [4]uint64{^uint64(0), 0, 1, 0}
	if got := hamming256(a, b); got != 65 {
		t.Fatalf("hamming = %d, want 65", got)
	}
}

func TestHamming256MutationConsistency(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 2000; i++ {
		x := randPDQ(rng)
		want := rng.Intn(33)
		y := mutatePDQ(x, want, rng)
		if got := hamming256(x, y); got != want {
			t.Fatalf("mutated %d bits, hamming = %d", want, got)
		}
	}
}

// 数学保证：汉明 ≤3 的对必被 band 倒排命中（鸽巢：4 段分 ≤3 个差异，必有 1 段全同）。
func TestBandIndexRecallWithin3Bits(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	var feats [][4]uint64
	for i := 0; i < 3000; i++ {
		h := randPDQ(rng)
		if i%5 == 0 && len(feats) > 0 {
			h = mutatePDQ(feats[rng.Intn(len(feats))], rng.Intn(4), rng)
		}
		feats = append(feats, h)
	}
	idx := newBandIndex(len(feats))
	scratch := make([]uint32, 0, 64)
	found := make(map[[2]int]bool)
	for i, h := range feats {
		for _, j := range idx.query(h, scratch) {
			found[[2]int{int(j), i}] = true
		}
		idx.add(uint32(i), h)
	}
	for i := 0; i < len(feats); i++ {
		for j := 0; j < i; j++ {
			if hamming256(feats[i], feats[j]) <= 3 && !found[[2]int{j, i}] {
				t.Fatalf("pair (%d,%d) hamming<=3 missed by band index", j, i)
			}
		}
	}
}

func TestAspectClose(t *testing.T) {
	cases := []struct {
		w1, h1, w2, h2 int
		want           bool
	}{
		{1920, 1080, 1920, 1080, true},  // 完全相同
		{1920, 1080, 1920, 1180, true},  // 差约 8.4% < 10%
		{1920, 1080, 1440, 1080, false}, // 差 25%
		{1000, 1000, 1000, 1111, true},  // 1.0 vs 0.9001 ≈ 10% 边界内
		{1000, 1000, 1000, 1120, false}, // 超出 10%
		{0, 1080, 1920, 1080, true},     // 尺寸缺失放行
	}
	for _, c := range cases {
		if got := aspectClose(c.w1, c.h1, c.w2, c.h2, 0.10); got != c.want {
			t.Errorf("aspectClose(%d,%d,%d,%d) = %v, want %v", c.w1, c.h1, c.w2, c.h2, got, c.want)
		}
	}
}

func TestScreenImages(t *testing.T) {
	base := [4]uint64{0xF0F0F0F0F0F0F0F0, 0x0F0F0F0F0F0F0F0F, 0xAAAABBBBCCCCDDDD, 0x1111222233334444}
	feats := []ImageFeature{
		{SHA512: sha(1), PDQ: base, Quality: 80, Width: 1920, Height: 1080},
		{SHA512: sha(2), PDQ: flip(base, 0, 1, 2, 3, 4), Quality: 70, Width: 1920, Height: 1080}, // 距 base 5 位 → 与 1 成对
		{SHA512: sha(3), PDQ: flip(base, 0, 1, 2, 3, 4), Quality: 60, Width: 1440, Height: 1080}, // 长宽比差 25% → 剪枝
		{SHA512: sha(4), PDQ: flip(base, 0, 1, 2, 3, 4), Quality: 40, Width: 1920, Height: 1080}, // 质量不达标 → 出局
		{SHA512: sha(5), PDQ: randPDQ(rand.New(rand.NewSource(9))), Quality: 90, Width: 1920, Height: 1080},
	}
	pairs := screenImages(feats, 31, 0.10, 50)
	if len(pairs) != 1 {
		t.Fatalf("pairs = %d, want 1: %+v", len(pairs), pairs)
	}
	p := pairs[0]
	if p.Kind != KindImageCandidate || p.ShaA != sha(1) || p.ShaB != sha(2) || p.Hamming != 5 {
		t.Fatalf("unexpected pair: %+v", p)
	}
}

// 输入顺序不影响输出（排序后确定性）。
func TestScreenImagesDeterministic(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	var feats []ImageFeature
	for c := 0; c < 50; c++ {
		base := randPDQ(rng)
		for m := 0; m < 3; m++ {
			feats = append(feats, ImageFeature{
				SHA512: sha(byte(len(feats)%250 + 1)), PDQ: mutateLow128(base, 1+rng.Intn(8), rng),
				Quality: 80, Width: 800, Height: 600,
			})
		}
	}
	p1 := screenImages(feats, 31, 0.10, 50)
	rng.Shuffle(len(feats), func(i, j int) { feats[i], feats[j] = feats[j], feats[i] })
	p2 := screenImages(feats, 31, 0.10, 50)
	if !reflect.DeepEqual(p1, p2) {
		t.Fatalf("non-deterministic output: %d vs %d pairs", len(p1), len(p2))
	}
	if len(p1) != 50*3 { // 每簇 C(3,2)=3 对
		t.Fatalf("pairs = %d, want 150", len(p1))
	}
}

func TestScreenVideosDurationBoundary(t *testing.T) {
	thumb := [4]uint64{0xDEADBEEFCAFEBABE, 1, 2, 3}
	feats := []VideoFeature{
		{SHA512: sha(1), DurationMs: 60000, ThumbPDQ: thumb, ThumbQuality: 80},
		{SHA512: sha(2), DurationMs: 62000, ThumbPDQ: flip(thumb, 0, 1, 2), ThumbQuality: 80}, // Δ=2000 恰好通过
		{SHA512: sha(3), DurationMs: 62001, ThumbPDQ: flip(thumb, 0, 1, 2), ThumbQuality: 80}, // 与 1 差 2001 剪掉；与 2 差 1、d=0 成对
		{SHA512: sha(4), DurationMs: 60500, ThumbPDQ: randPDQ(rand.New(rand.NewSource(5))), ThumbQuality: 80},
	}
	pairs := screenVideos(feats, 2000, 31)
	if len(pairs) != 2 {
		t.Fatalf("pairs = %d, want 2: %+v", len(pairs), pairs)
	}
	if pairs[0].ShaA != sha(1) || pairs[0].ShaB != sha(2) || pairs[0].Hamming != 3 || pairs[0].DurationDiffMs != 2000 {
		t.Fatalf("unexpected pair[0]: %+v", pairs[0])
	}
	if pairs[1].ShaA != sha(2) || pairs[1].ShaB != sha(3) || pairs[1].Hamming != 0 || pairs[1].DurationDiffMs != 1 {
		t.Fatalf("unexpected pair[1]: %+v", pairs[1])
	}
}

func TestExactCollector(t *testing.T) {
	col := &exactCollector{}
	add := func(b byte, id int64, machine string, disk int) {
		col.add(sha(b), FileRef{ID: id, MachineID: machine, DiskNo: disk, Path: "p", Size: 1})
	}
	add(1, 10, "m1", 0) // sha1：跨机器跨盘 3 副本 → 成组
	add(1, 11, "m2", 1)
	add(1, 12, "m1", 2)
	add(2, 20, "m1", 0) // sha2：单副本 → 不成组
	add(3, 30, "m1", 0) // sha3：2 副本同盘不同路径 → 成组
	add(3, 31, "m1", 0)
	groups := col.finish()
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}
	if groups[0].SHA512 != sha(1) || len(groups[0].Members) != 3 {
		t.Fatalf("group[0] = %+v", groups[0])
	}
	if groups[1].SHA512 != sha(3) || len(groups[1].Members) != 2 {
		t.Fatalf("group[1] = %+v", groups[1])
	}
	if groups[0].Members[0].ID != 10 { // 成员按 id 升序，首行即代表
		t.Fatalf("members not ordered by id: %+v", groups[0].Members)
	}
}
```

### 6.2 小规模功能集成（`TestIntegrationSmallDB`，docker PG 16）

> **权威性**：数据构造和数据库断言以
> `internal/firstscreen/small_acceptance_test.go` 为唯一权威；门禁和 evidence
> 以 `scripts/verify_m3.ps1` 为唯一权威。本节不再复制易漂移的测试实现。

#### 6.2.1 隔离、建表与清理

- `TestIntegrationSmallDB` 从显式环境变量 `FS_PG_DSN` 连接 PostgreSQL 16；
  普通 `go test` 未配置 DSN 时可以 skip，但 verifier 把 skip 视为失败。
- 每次测试使用 crypto-random 的 `m3_small_<token>` schema，不 truncate 或修改
  public schema。
- 随机 schema 创建前和 `central.sql` 两次执行后，复用 Task 4 的完整 public
  catalog snapshot，逐项比较全部 relation（包括 sequence）、column、
  constraint 和 index；完全相同才设置 `public_unchanged=true`。该独立验收
  gate 执行期间要求 public catalog 稳定，避免把外部并发 DDL 混入证明。
- 创建 schema 后显式执行 `SET search_path TO <quoted-schema>`，再读取并执行
  `deploy/central.sql` **两次**；任一次失败都终止验收，从而验证 schema 幂等。
- cleanup 先 `SET search_path TO public`，再 `DROP SCHEMA ... CASCADE`，随后从
  `pg_namespace` 查询 residual。residual 必须为 0，并写入结构化验收 marker。

#### 6.2.2 确定性数据和精确统计

计划所称“小 20 行”数据集按实际表行计算是 22 行：
13 `files` + 5 `image_features` + 4 `video_features`。

| 数据 | 构造 | 精确期望 |
|---|---|---|
| 图片 A1/A2 | 16:9、quality 80/90、PDQ hamming=3 | 唯一 `image_candidate` |
| 图片 A3 | 与近邻相似但 quality=30 | SQL 过滤，不进入 Analyzer |
| 图片 A4 | 相似 PDQ 但 4:3 | aspect 拒绝 |
| 图片 A5 | far PDQ | hamming 拒绝 |
| 视频 V1/V2 | 60000/61500ms、hamming=1 | 接受，duration diff=1500 |
| 视频 V2/V3 | 61500/62600ms、相同 PDQ | 接受，duration diff=1100 |
| 视频 V1/V3 | duration diff=2600 | duration window 拒绝 |
| 视频 V4 | far PDQ | hamming 拒绝 |
| 精确 A2 | 两个 file 副本 | 一个 2-member exact group |
| 精确 E | 三个 file 副本，无特征 | 一个 3-member exact group |

配置必须使用 `ReadPageSize=3`。每轮精确统计：

| 指标 | 值 |
|---|---:|
| `FilesScanned` | 13 |
| `ImageFeatures` | 4 |
| `VideoFeatures` | 4 |
| `ExactGroups` / `ExactMembers` | 2 / 5 |
| `ImagePairs` / `VideoPairs` | 1 / 2 |
| `GroupsWritten` / `MembersWritten` | 5 / 12 |
| `SkippedPairs` / `BadRows` | 0 / 0 |

`StageElapsedMs` 必须恰好包含六阶段键。

#### 6.2.3 数据库语义断言

验收不只检查计数，还逐组检查：

- M3 恰好写入 2 exact、1 image candidate、2 video candidate；
- exact representative 是组内最小 file id；
- candidate representative 是规范化 ShaA 一侧最小 file id；
- `member_count` 与全部实际成员逐一一致，包含 A2 的两个副本；
- 每个成员的 `score_json` 都按所在 side 检查 peer SHA、self/peer quality、
  hamming 与 video duration diff；exact 成员必须是 `{"basis":"sha512"}`；
- 预置 `kind=image` 和 `kind=video` 的 M4 sentinel，包含 group 元数据、时间戳、
  member 和 JSON；首次运行及 rerun 后 byte-for-byte snapshot 不变。

第二轮使用新 Store/Analyzer 执行。统计和不含 group id 的语义 snapshot 必须与
第一轮相同；测试不得依赖重写后生成的新 `dup_groups.id`。

#### 6.2.4 结构化 marker 与 verifier

通过的 verbose 测试在 cleanup 后输出一行：

```text
M3_SMALL_ACCEPTANCE {"run_id":"...","counts":{...},"stage_keys":[...],
"cleanup_residual":0,"rerun":true,"central_sql_runs":2,
"read_page_size":3,"sentinel_preserved":true,"public_unchanged":true}
```

`verify_m3.ps1` 必须显式接收 `-Go` 和非空 `-PGDSN`，可选 `-GCC`；
所有工具路径绝对化并检查存在。未传 GCC 时只允许从 PATH 或本机 WinLibs
目录自动发现。脚本临时设置：

- `FS_PG_DSN` 和 `DEDUP_TEST_PG_DSN`；
- `CGO_ENABLED=1`；
- PATH 前缀：`repo/bin`、`repo/bin/tools`、GCC bin、Go bin；
- verifier run id 和 workspace 内 evidence 路径。

所有环境变量和当前目录必须在 finally 中恢复。DSN 不进入命令记录、报告或
evidence；子进程输出写 evidence 前也要做 secret redaction。

Task 10 最终控制器的 quick 模式依次执行：

1. 用控制器内的安全全仓枚举 helper 收集仓库自有 Go 源并执行 `gofmt -l`；
   当前共 104 个文件，包含目录清单之外的
   `testdata/m2/gen_corrupt.go`。枚举按路径段排除 `.git/`、
   `.superpowers/`（含 evidence/tmp）、`.tmp/`、`vendor/`、
   `node_modules/`、`third_party/`、`build/`、`dist/`、`out/`、`bin/`、
   `obj/`、`.cache/` 等元数据、证据、临时、构建和依赖目录；路径越界、
   零文件、重复文件、非 `.go`、不存在文件，以及 root→file 任一组件带
   `FileAttributes.ReparsePoint` 都失败。组件在列表返回前再次检查，防止校验
   过程中 junction/reparse 替换后沿用旧结论；任一 `gofmt -l` 文件名输出也
   失败；
2. 临时设置 `CGO_ENABLED=0` 后执行不缩包的全仓 `go test ./...`，只按顶层
   名称排除 PostgreSQL small/contracts 和 m3scale；无论结果如何都先恢复
   `CGO_ENABLED=1`，再继续 CGO 门禁；
3. 保留 CGO 全仓 unit、M3 三包 race（firstscreen、GUI、cmd/gui）和全仓
   `go vet ./...`；
4. 使用 `-v -count=1 -run '^TestPG(Keyset|ReplaceResults)'` 单独运行 10 个
   PostgreSQL 顶层合同；每个预期测试必须恰好一条顶层 RUN、一条顶层 PASS、
   无 SKIP，覆盖 schema×2、三个精确索引、真实
   `EXPLAIN (ANALYZE, BUFFERS)`、失败回滚、未知 commit 和 M4 保留；
5. 单独运行并严格解析 `TestIntegrationSmallDB` marker；
6. 在控制器内运行 Task 8 marker 12-case、scale marker 23-case、cleanup
   marker 7-case 独立负向矩阵，各自必须恰好输出一条精确 PASS；
7. 将 schema/index inspection 及 small、PostgreSQL contracts、可选 scale
   cleanup audit 写入独立日志和 `m3-evidence.json` 的机器可读字段。

任一 native exit code 非 0 都失败。即使 Go exit 0，verifier 仍必须拒绝：
测试缺失、named PASS 缺失、SKIP、marker 缺失/重复/非法、六阶段键不完整、
任一精确计数不符、rerun/sentinel/central×2/page-size 证明缺失，以及
cleanup residual 非 0 或 public catalog 前后不一致。marker 验证严格按
JSON 原生类型 fail-closed：required property 必须存在且非 null；整数只接受
PowerShell 整型（拒绝 string/bool/double），布尔只接受 `[bool]`，run id
只接受非空 string，counts 必须为 object，stage keys 必须恰好是六个唯一、
非空 string。独立无 Pester 负向矩阵至少覆盖 missing/null/string cleanup、
null/string zero count、string bool、重复和非 string stage。PostgreSQL
不可达和空 DSN 必须非 0。

每个外部 gate 在调用前先创建 UTF-8 空日志，因此无输出的 `gofmt`/`go vet`
也必须有真实零字节日志。最终 PASS 前 verifier 按动态 required gate 列表
逐项确认状态为 PASS、exit code 为原生整数 0，且日志真实存在并位于本次
evidence 目录。无论成功或失败，控制器先写 summary，再为每个 required gate
输出且只输出一行 `M3 GATE <name> <PASS|FAIL|NOT_RUN> ...`；missing gate/log
必然非零，先发生的主失败及 fallback cleanup 失败原因同时保留。最后仅输出
一条清晰的 `M3 VERIFY PASS`，或以非零 `M3 VERIFY FAIL` 结束。

每次执行生成唯一 run id 和独立目录：

```text
.superpowers/evidence/m3-<run-id>/m3-evidence.json
```

默认不带 scale 开关的是 quick 回归，保持 Task 9 的默认行为；它不是“完整证明
M3”。完整证明必须运行 `verify_m3.ps1 -RunScale`，兼容别名 `-Scale`；
同时传入两者仍只执行一次 seed/reuse。Task 9 authority 证据继续保留，Task 10
最终候选另行记录。
### 6.3 大数据量验收（`TestAcceptanceM3`）

> **权威性**：实现以带 `m3scale` build tag 的
> `internal/firstscreen/scale_acceptance_test.go` 为准；门禁以
> `scripts/verify_m3.ps1 -Scale` 和
> `scripts/verify_m3_scale_marker.ps1` 为准。默认 verifier 不编译或运行
> 百万级测试。

#### 6.3.1 确定性规模与算术修正

seed 固定为 1。所有 SHA-512 是 128 字符 canonical lowercase TEXT，
image/video/exact 使用互斥 domain + ordinal 编码，路径逐行唯一。

| 表/结果 | 精确值 |
|---|---:|
| `image_features` | 1,000,000 |
| 其中 random / q=30 filtered / 4-member clusters | 960,000 / 9,600 / 10,000 |
| `image_features_loaded` / `image_pairs` | 990,400 / 60,000 |
| `video_features` | 200,000 |
| 其中 random / 4-member clusters / `video_pairs` | 190,000 / 2,500 / 15,000 |
| `files` | 1,350,000 |
| exact groups / exact members | 50,000 / 150,000 |
| groups / members written | 125,000 / 300,000 |

原草案的 `2+i%3` 在 `i=0..49,999` 上求和是 **149,999**，不是
150,000。权威生成器保留每组公式，仅给最后一组 `i=49,999` 增加一个副本，
因此 exact group 仍恰好 50,000，exact members 修正为 150,000，不改变任何
验收总数。

生成必须 bounded：自定义 `pgx.CopyFromSource` 复用单行 value buffer，
每 50,000 行完成一次 CopyFrom，任何时刻不得持有百万 `[][]any` 或 135 万
file rows。图片 random、不同 cluster 的每个 band key 互斥；同簇仅共享
cluster band，四成员两两 hamming≤31，数学上恰好产生 `C(4,2)`。视频按
1 小时时间轴生成，用 bounded 2 秒 active window 逐行证明所有非簇/跨簇
PDQ hamming>31；每簇四成员在同一 duration slot 且仅簇内相近。

#### 6.3.2 隔离、双进程与清理

- verifier 生成唯一 `m3_scale_<sanitized-run-id>`；Go 测试要求 schema 与
  run id 推导值精确相等并符合安全前缀，拒绝 public 或其他 schema。
- `FS_M3_SEED=1` 的 fresh process 在创建 schema 前取完整 public catalog
  baseline，创建后设置 quoted search path，执行 `central.sql` 两次。此时
  只允许做 intermediate DeepEqual，**不得**据此设置
  `public_unchanged=true`。必须在 seed、两次 Analyzer、semantic/DB assertions
  全部完成后再取 final snapshot，覆盖 CREATE→Analyzer/DB 整个窗口且
  DeepEqual 才置 true。snapshot 包含 public 全部 relations（含 sequence）、
  columns、constraints、indexes。
- seed process 以固定 50k chunk 灌数、`ANALYZE`，运行 Analyzer 两次并证明
  semantic signature 不变；成功时保留 schema 给下一 process。
- `FS_M3_SEED=0` 的 fresh process 先核对 1.35m/1m/200k 物理行数，seed
  duration 和 chunks 必须全为 0，再运行 Analyzer 并与已有 semantic
  signature 比较。reuse process 入口先取自己的 public baseline；只有本
  run schema DROP 成功、`cleanup_residual=0`，再取 final public snapshot
  且 DeepEqual 后，reuse marker 才允许 `public_unchanged=true`。
- 失败时 Go cleanup 与 verifier cleanup mode 都只接受相同的 run-owned
  schema；`DROP SCHEMA IF EXISTS` 使重复清理安全。禁止 truncate/drop public
  或扫描、删除其他前缀 schema。

#### 6.3.3 测量与严格 marker

每次 Analyzer 在启动 50ms sampler 前同步读取一次 `HeapInuse`，结束时再读
一次。每轮必须有六个 stage，且：

| 指标 | 上限 |
|---|---:|
| `image_screen` | 5,000 ms |
| `video_screen` | 3,000 ms |
| Analyzer 端到端 | 90,000 ms |
| peak `HeapInuse` | 4 GiB |

两 process 的单一 `M3_SCALE_ACCEPTANCE` JSON marker 记录 run/schema、
seeded/reused、PG 16 version、public snapshot、central runs、seed duration
和 chunk 数、物理行数、三条 `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)` 的
root/index/actual rows/time/buffers、每轮 counts/stages/total/peak、DB totals、
semantic/idempotent、性能结果、schema preserve/cleanup/residual，以及
`second_windows_status=USER_WAIVED`。

PowerShell validator 对 required、null、object/array、整数、浮点、bool、
string 和 exact value 全部 fail-closed。`total_ms`、`peak_heap_bytes` 必须是
native integer 且严格大于 0；EXPLAIN `execution_ms` 必须是 native numeric
且严格大于 0（planning 允许 0）。23-case scale matrix 以历史 authority
marker 做 mutation，覆盖 0、性能上界、精确计数和 run/schema/state。
`-RunScale`（兼容 `-Scale`）在 quick 全部门禁通过后，于同一 evidence 中
依次运行 `scale_seed`、`scale_reuse`；seed/reuse 都是 required gate，并继续
要求真实日志、共同 run/schema、reuse residual=0 和完整 public 生命周期窗口。
Task 9 的两个 fresh authority run 保持有效；Task 10 完整控制器候选作为最终
门禁的独立证据，不覆盖 Task 9 authority。

fallback cleanup 必须输出且只输出一个机器 marker：

```text
M3_SCALE_CLEANUP {"run_id":"...","schema":"...","cleanup_residual":0}
```

Go cleanup-only 对 residual 非 0 必须测试失败。PowerShell 不只检查 Go exit
0，还严格要求 named PASS、唯一 marker、同 run/schema、native integer
residual=0；missing/duplicate/wrong/null/string/nonzero 由 7-case 独立矩阵
证明。若原 gate 与 cleanup 都失败，最终 failure 必须同时保留两段原因。

<!-- 以下为最初设计草案，已由上面的 bounded 双进程契约取代，保留在源码注释中
仅用于历史算术追踪，不得作为实现或运行手册。

**合成数据规模**（确定性种子，期望计数由公式精确给出）：

| 表 | 规模 | 构造 |
|---|---|---|
| `image_features` | 1,000,000 行 | 960,000 随机（每 100 行置 1 行 q=30，验证质量过滤）；10,000 簇 × 4 成员，簇内低 128bit 变异 1~8 位、q∈[60,95]、同尺寸 |
| `video_features` | 200,000 行 | 190,000 随机（时长均匀分布于 1h 内）；2,500 簇 × 4 成员，簇内时长 ±900ms、缩略图低 128bit 变异 |
| `files` | 1,350,000 行 | 每个特征 sha 恰好 1 行；另有 50,000 个纯精确组 sha × (2 + i%3) 副本（跨 machine/disk），共 150,000 行 |

**期望计数**：`image_pairs = 10000×C(4,2) = 60,000`；`video_pairs = 2500×C(4,2) = 15,000`；`exact_groups = 50,000`；`files_scanned = 1,350,000`；`groups_written = 125,000`；`members_written = 60000×2 + 15000×2 + 150000 = 300,000`；`image_features_loaded = 990,400`（9600 行 q=30 被 SQL 过滤）。

**耗时测量方法**（双重）：

1. `RunStats.StageElapsedMs`：6 个阶段（`exact_group / image_load / image_screen / video_load / video_screen / db_write`）分别计时，slog 输出；"秒级出候选"对应 `image_screen` 与 `video_screen` 两个纯计算阶段。
2. 进程内 50ms 采样 `runtime.MemStats.HeapInuse` 取峰值（采样器代码如下，随测试运行）。

**通过标准**（参考环境：GUI 机 16GB RAM、SSD、PG 16 同机或千兆 LAN、Go 1.22+）：

| 指标 | 验收线 | 说明 |
|---|---|---|
| 计数 | 与期望**精确相等** | 含重跑幂等（第二次计数相同且 `dup_groups` 总行数不翻倍） |
| `image_screen` 阶段 | ≤ 5s | "百万级秒级出候选"核心指标 |
| `video_screen` 阶段 | ≤ 3s | — |
| 端到端（读+算+写） | ≤ 90s | 含 PG 读取与结果写回 |
| 峰值 HeapInuse | ≤ 4GB | 16GB 机器留有余量（plan §11） |

> 拼接说明：本块与 §6.2 代码块同属 `acceptance_test.go`。合并时去掉本块顶部的
> `//go:build integration` 注释与 `package firstscreen` 行（文件只需各一份），
> 两个代码块的 import 取并集。

```go
//go:build integration

package firstscreen

import (
	"context"
	"crypto/sha512"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	imgRandom   = 960_000
	imgClusters = 10_000
	imgClusterN = 4
	vidRandom   = 190_000
	vidClusters = 2_500
	vidClusterN = 4
	exactGroupN = 50_000
)

func copyChunked(ctx context.Context, conn *pgx.Conn, table string, cols []string, rows [][]any) error {
	const chunk = 100_000
	for start := 0; start < len(rows); start += chunk {
		end := min(start+chunk, len(rows))
		if _, err := conn.CopyFrom(ctx, pgx.Identifier{table}, cols, pgx.CopyFromRows(rows[start:end])); err != nil {
			return fmt.Errorf("copy %s: %w", table, err)
		}
	}
	return nil
}

func seedSynth(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	rng := rand.New(rand.NewSource(1))
	truncateAll(t, conn)

	imgRows := make([][]any, 0, imgRandom+imgClusters*imgClusterN)
	fileRows := make([][]any, 0, 1_400_000)
	addFile := func(machine string, disk int, path string, size int64, sha []byte) {
		fileRows = append(fileRows, []any{machine, disk, path, size, sha})
	}

	for i := 0; i < imgRandom; i++ { // 随机图片：每 100 行 1 行低质量
		s := randBytes(rng, 64)
		q := 50 + i%50
		if i%100 == 0 {
			q = 30
		}
		imgRows = append(imgRows, []any{s, 1920, 1080, pdqBytes(randPDQ(rng)), q})
		addFile("m1", i%3, fmt.Sprintf("D:/img/r%d.jpg", i), int64(1_000_000+i), s)
	}
	for c := 0; c < imgClusters; c++ { // 图片近重复簇：低 128bit 变异 → 候选必命中
		base := randPDQ(rng)
		for m := 0; m < imgClusterN; m++ {
			s := randBytes(rng, 64)
			imgRows = append(imgRows, []any{s, 1920, 1080, pdqBytes(mutateLow128(base, 1+rng.Intn(8), rng)), 60 + rng.Intn(36)})
			addFile("m1", c%3, fmt.Sprintf("D:/img/c%d_%d.jpg", c, m), 2_000_000, s)
		}
	}
	if err := copyChunked(ctx, conn, "image_features", []string{"sha512", "width", "height", "pdq256", "pdq_quality"}, imgRows); err != nil {
		t.Fatal(err)
	}

	vidRows := make([][]any, 0, vidRandom+vidClusters*vidClusterN)
	for i := 0; i < vidRandom; i++ {
		s := randBytes(rng, 64)
		vidRows = append(vidRows, []any{s, rng.Int63n(3_600_000), pdqBytes(randPDQ(rng)), 50 + rng.Intn(50)})
		addFile("m1", i%3, fmt.Sprintf("D:/vid/r%d.mp4", i), int64(50_000_000+i), s)
	}
	for c := 0; c < vidClusters; c++ {
		base := randPDQ(rng)
		d := 60_000 + rng.Int63n(3_000_000)
		for m := 0; m < vidClusterN; m++ {
			s := randBytes(rng, 64)
			vidRows = append(vidRows, []any{s, d - 900 + rng.Int63n(1801), pdqBytes(mutateLow128(base, 1+rng.Intn(8), rng)), 50 + rng.Intn(50)})
			addFile("m1", c%3, fmt.Sprintf("D:/vid/c%d_%d.mp4", c, m), 60_000_000, s)
		}
	}
	if err := copyChunked(ctx, conn, "video_features", []string{"sha512", "duration_ms", "thumb_pdq256", "thumb_quality"}, vidRows); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < exactGroupN; i++ { // 纯精确组：2+(i%3) 副本，共 150,000 行
		sum := sha512.Sum512([]byte(fmt.Sprintf("exact-%d", i)))
		for k := 0; k < 2+i%3; k++ {
			addFile(fmt.Sprintf("m%d", k%2+1), k%3, fmt.Sprintf("D:/dup/g%d_%d.bin", i, k), 5_000_000, sum[:])
		}
	}
	if err := copyChunked(ctx, conn, "files", []string{"machine_id", "disk_no", "path", "size", "sha512"}, fileRows); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"files", "image_features", "video_features"} {
		if _, err := conn.Exec(ctx, "ANALYZE "+tbl); err != nil {
			t.Fatalf("analyze %s: %v", tbl, err)
		}
	}
}

// samplePeakHeap 每 50ms 采样 HeapInuse，取峰值（验收内存指标）。
func samplePeakHeap(stop <-chan struct{}, peak *atomic.Uint64) {
	tk := time.NewTicker(50 * time.Millisecond)
	defer tk.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tk.C:
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			for {
				cur := peak.Load()
				if m.HeapInuse <= cur || peak.CompareAndSwap(cur, m.HeapInuse) {
					break
				}
			}
		}
	}
}

func TestAcceptanceM3(t *testing.T) {
	conn := mustConn(t)
	if os.Getenv("FS_SEED") == "1" {
		t.Log("seeding synthetic dataset ...")
		seedSynth(t, conn)
	}
	cfg := DefaultConfig()
	an := NewAnalyzer(NewStore(conn, cfg), cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	stop := make(chan struct{})
	var peak atomic.Uint64
	go samplePeakHeap(stop, &peak)
	t0 := time.Now()
	st, err := an.Run(context.Background())
	close(stop)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	total := time.Since(t0)
	t.Logf("stages: %+v", st.StageElapsedMs)
	t.Logf("total=%s peak_heap=%.2fGB", total, float64(peak.Load())/(1<<30))

	// 计数精确断言
	checks := []struct {
		name string
		got  int
		want int
	}{
		{"files_scanned", st.FilesScanned, 1_350_000},
		{"image_features_loaded", st.ImageFeatures, 990_400},
		{"video_features_loaded", st.VideoFeatures, 200_000},
		{"image_pairs", st.ImagePairs, 60_000},
		{"video_pairs", st.VideoPairs, 15_000},
		{"exact_groups", st.ExactGroups, 50_000},
		{"groups_written", st.GroupsWritten, 125_000},
		{"members_written", st.MembersWritten, 300_000},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}

	// 性能验收线（§6.3 表）
	if ms := st.StageElapsedMs["image_screen"]; ms > 5_000 {
		t.Errorf("image_screen = %dms, want ≤ 5000ms", ms)
	}
	if ms := st.StageElapsedMs["video_screen"]; ms > 3_000 {
		t.Errorf("video_screen = %dms, want ≤ 3000ms", ms)
	}
	if total > 90*time.Second {
		t.Errorf("total = %s, want ≤ 90s", total)
	}
	if peak.Load() > 4<<30 {
		t.Errorf("peak heap = %.2fGB, want ≤ 4GB", float64(peak.Load())/(1<<30))
	}

	// 幂等：重跑计数相同。
	st2, err := an.Run(context.Background())
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if st2.GroupsWritten != st.GroupsWritten || st2.MembersWritten != st.MembersWritten {
		t.Errorf("rerun not idempotent: %+v vs %+v", st2, st)
	}
	var groupTotal int
	if err := conn.QueryRow(context.Background(),
		`SELECT count(*) FROM dup_groups`).Scan(&groupTotal); err != nil || groupTotal != 125_000 {
		t.Errorf("dup_groups total = %d, want 125000 (err=%v)", groupTotal, err)
	}
}
```

-->

### 6.4 验收运行手册

```powershell
# 0. 先启动 PostgreSQL 16；DSN 只作为显式参数传入，不写入 evidence。

# 1. quick 回归：format、pure-Go、CGO unit/race/vet、10 个 PG contracts、
#    small acceptance、12/23/7 三组负向矩阵、schema/index/cleanup audit。
#    quick 不运行百万级 seed/reuse，不能作为“完整证明 M3”。
& .\scripts\verify_m3.ps1 `
    -Go <go.exe> `
    -PGDSN <explicit-dsn> `
    -GCC <gcc.exe>

# 2. 完整证明：在 quick 全部 required gates 之后再运行 scale seed/reuse。
& .\scripts\verify_m3.ps1 `
    -Go <go.exe> `
    -PGDSN <explicit-dsn> `
    -GCC <gcc.exe> `
    -RunScale

# -Scale 是 -RunScale 的兼容别名；两者同时传入也只运行一次。
```

通过标准：summary `status=PASS`，动态 required gates 全为 PASS 且日志真实，
§6.1/§6.2 全绿，§6.3 全部断言（计数精确相等 + 性能验收线 + 幂等）通过。
Task 10 候选 `20260728-091611-043-0c8f007a` 的实测如下；该候选仍须 root
广审，本文不据此提前勾选完成项：

| 指标 | 验收线 | 实测 |
|---|---|---|
| image_screen | ≤ 5s | 最大 818 ms |
| video_screen | ≤ 3s | 最大 173 ms |
| 端到端 | ≤ 90s | 最大 13,479 ms |
| 峰值 HeapInuse | ≤ 4GiB | 最大 979,148,800 bytes |
| 候选/组计数 | 精确相等 | 60,000 image / 15,000 video / 125,000 groups |

---

## 7. 风险与注意事项

1. **band 倒排的召回上界（最重要）**：4×64bit 分段在数学上只保证汉明 ≤3 的对 100% 召回（鸽巢）；距离 4~31 且差异位分散到 4 段的对可能漏检（极端 8/8/8/7 分布）。这是 plan §5.1 既定方案的固有取舍，T1=31 只作为**过滤器**而非召回保证。若实测漏检不可接受，强化方案是加第二套错位 64bit band 布局（bit 64 偏移起切）做双索引取并集，内存翻倍、召回显著提升——留 M6 评估，不在 M3 实施。
2. **退化簇导致单桶爆炸**：全黑/全白/纯色图片的 PDQ 趋同（近似全零），若大量存在会使某个 band 桶内条数巨大，候选验证退化为 O(k²)。防线是 `image_quality_min`（平坦图 Quality 很低，默认 50 基本滤净）；同时该阈值是 plan 未给值的 M3 新增默认，**需架构确认**。验收后应抽查线上桶长分布（`RunStats` 暂不含，可按需在 `screenImages` 加最大桶长日志）。
3. **视频时长病态集中**：大量同时长视频（监控分段、连拍导出）会使滑窗内条数 k 暴增，O(n·k) 退化。M3 按 plan 接受；缓解路径（如需）：窗口对数超阈值时退化为"先 band 倒排（缩略图）后时长验证"，属 M6 调优项。验收数据的时长分布应接近真实库再下结论。
4. **结果表幂等语义与 M4 契约**：M3 每次整类删除重写，`dup_groups.id` 不稳定。M4 消费候选必须用内容键 `(kind, sha_a, sha_b)` 关联，禁止持久化引用 `dup_groups.id`。M1 建表时 `kind` 列**不得**加三值 CHECK 约束（M3 起枚举扩到 5 值）。
5. **分析期间的数据竞争**：Agent 上行是异步的（plan §6.3 5min/5万行）。可能出现"特征已上行、files 行未上行"→ 该候选对本轮跳过（`skipped_pairs` 计数并 Warn 日志），下轮自动覆盖；写库用 `Repeatable Read` 事务保证删除/重写看到一致快照。精确分组不校验 `files.status`，只要求 `sha512` 非空。
6. **PDQ 字节序契约**：`pdqFromBytes` 按大端解码 32B BLOB，这是 M2 落库约定的隐性依赖；若 M2 实现为小端，症状是"随机数据也能大量命中 band"→ 候选爆炸，集成测试会立刻暴露。该契约需回写进 M2 文档。
7. **GUI 单机内存**：百万级图片特征 + 倒排 ≈ 1~2GB（特征切片 ~110B/行 + band 索引 ~90B/桶×4）；千万级会逼近 8GB。plan §11 已要求 GUI 机 ≥16GB；超限时的降级路径是按 band 分 4 轮建索引（每轮只建 1 段，候选并集后去重），内存降为 1/4、读取放大 4 倍，按需再议，不在 M3 实现。
8. **单文件超长路径/非法行**：`sha512`/`pdq256` 长度非法的行是 M1/M2 数据质量事故，M3 对特征行跳过计数（`bad_rows`）、对 `files` 行直接报错（主键级损坏不可静默），均不中断整轮。
