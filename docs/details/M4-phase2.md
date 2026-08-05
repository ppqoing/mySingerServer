# M4 — 二阶段按需补算与复筛 实施文档

> 依据：`docs/architecture-plan.md` v1.1（下称 plan）。本文档不另起选型，所有默认参数与 plan 第 9 节一致。
> 上游依赖：M1（TCP 协议 / SQLite / 中心库 / Worker 池）、M2（`mediacore.dll` 内存解码 + PDQ-256 + 视频缩略图管线）、M3（GUI 一筛候选对生成）。
> 里程碑目标（plan §10）：一筛命中后自动下发补算，相似组分数明细可见。

---

## 1. 目标与范围

### 1.1 目标

对 M3 一筛产出的候选对（图片对 / 视频对，通常 <1% 体量）执行按需补算与复筛：

1. **GUI 侧 `Phase2Task` 生成**：从一筛候选对收集唯一 sha512 集合，按 `machine_id` 路由分发到对应 Agent，用字段位掩码指定缺失特征。
2. **`mediacore.dll` 二阶段能力**：分区 pHash（灰度面 → 3×3 分区 → 每区 DCT 感知哈希）；Sobel 结构块直方图（复用同一灰度面，不重复解码、不重复灰度化）。
3. **视频 6 帧管线**：按时长均分 6 个时间点 `(1/12, 3/12, ..., 11/12)` 逐帧 ffmpeg 截图，每帧走 PDQ-256 + 分区 pHash + Sobel。
4. **复筛判定**：图片 `pHash 分区通过比例 ≥ T2(0.80) → Sobel 相关度 ≥ T3(0.85)`；视频 `6 帧平均 ≥ T4(0.80)`，兜底 `≥4/6 帧通过`；并查集合并成组。
5. **GUI 三组展示**：精确重复组 / 相似图片组 / 相似视频组，含各级分数明细。

### 1.2 不做什么

以下各项**明确不在 M4 范围**，避免范围蔓延：

- **不做一阶段特征的任何改动**（SHA-512 / PDQ-256 / 缩略图管线属 M2，一筛 band 倒排属 M3）。M4 只消费其产物。
- **不做缩略图/帧图片的在线预览代理**：三组展示为路径 + 分数明细的文本/表格视图；图片预览需要 GUI 经 TCP 向 Agent 拉取缩略图，另行立项。
- **不做增量组合并优化**：复筛完成后对 `kind=image/video` 的组**全量重建**（基于 `pair_scores` 表重跑并查集），增量合并留待 M6 压测后再议。
- **不做删除**：删除链路属 M5。
- **不做帧 PDQ 参与复筛主链路**：帧 PDQ-256 计算并入库（plan §4.2 要求），但复筛判定只用 pHash 分区 + Sobel（plan §5.2"逐对走图片复筛流程"），避免双重阈值语义。
- **不做公有云部署适配**（plan 默认决策：局域网自建中心存储）。
- **不调整 plan 已定阈值**（T1~T4、宽容度、超时）；阈值调优属 M6。

### 1.3 与上游文档的衔接假设

M1~M3 详细文档若与本文假设的标识符不一致，**以各 M 文档为准修改 import 与类型名，模块划分与语义不变**。本文假设上游已提供：

| 假设 | 内容 |
|---|---|
| `shared/proto` 包 | 帧 envelope：`[4B 大端长度][msgpack body]`；消息类型常量；struct 以 **msgpack map 形式**编码（带字段名，`omitempty` 生效，追加字段向后兼容） |
| `agentpool.Pool`（M1，GUI 侧） | `Send(machineID string, msgType uint8, payload any) error`；`IsOnline(machineID string) bool` |
| `internal/mediacore` 包（M2，仅 Worker 进程 import） | Go 侧 `GrayImage` 包装：`DecodeFromMemory(buf []byte, maxDim int) (*GrayImage, error)`、`(*GrayImage).Free()`、`(*GrayImage).PDQ256() (hash [32]byte, quality int, err error)`；C 侧 `McGrayImage{uint8_t* data; int width; int height; int stride;}`、`mc_decode_image_from_memory`、`mc_pdq256`、`mc_free_gray`、错误码 `MC_OK=0 / MC_ERR_INVALID_ARG=-1 / MC_ERR_DECODE=-2`、导出宏 `MC_API`（`__declspec(dllexport)`）与 `MC_CALL`（`__cdecl`） |
| 中心库 `files` 表（M1） | `files(id BIGSERIAL PK, machine_id TEXT, disk_no INT, path TEXT, size BIGINT, mtime_ms BIGINT, sha512 BYTEA, kind SMALLINT, UNIQUE(machine_id, path))`，`kind`：1=图片 2=视频 |
| M3 输出 | 候选对列表（内存结构或中心库表），元素含两端 `machine_id/path/sha512/size/mtime_ms/duration_ms/kind` 与一筛汉明距离 |
| 中心库 `image_features` / `video_frames` 表 | plan §6.1 同构 PostgreSQL 版，二阶段列可空，Agent 上行写入 |

---

## 2. 任务分解（Checklist）

每项可单独验收；验收方法见第 6 节对应编号。

### 2.1 mediacore.dll 二阶段能力

- [x] **D1** `mediacore/include/mediacore.h` 追加 M4 段：常量、`McPhase2ImageOut`、`mc_phash_parts` / `mc_sobel_hist` / `mc_phase2_image` 声明（4.2.1）。验收：U1 编译通过。
- [x] **D2** `mediacore/src/phash_parts.cpp`：96×96 双线性缩放 + 3×3 分区 + 每区 32×32 DCT-II → 64bit（4.2.2）。验收：U1、U2。
- [x] **D3** `mediacore/src/sobel_hist.cpp`：128×128 缩放 + 3×3 Sobel + 4×4 网格 × 8 方向 bin 幅值加权直方图 + L2 归一化（4.2.3）。验收：U1、U3。
- [x] **D4** 导出函数单元测试 `mediacore/tests/test_phase2.cpp`：确定性、BLOB 维度、相似图/不相似图分数期望（4.2.4）。验收：U1~U5 全绿。
- [x] **D5** Go cgo 封装 `agent/internal/mediacore/phase2.go`：`(*GrayImage).Phase2()`（4.4.1）。验收：I1。

### 2.2 协议与 BLOB 编解码

- [x] **P1** `shared/proto/phase2.go`：`Phase2Task` / `Phase2Item` / `FieldError` / `FrameFeature` / `FeatureResult` 扩展字段 / 字段位掩码常量（4.3.1）。验收：P 包 `go test` 编解码往返用例。
- [x] **P2** `shared/features/blob.go`：`phash_parts`（76B）与 `sobel_hist`（516B）BLOB 编解码 + `Hamming64` + `SobelCosine`（含零范数规则）（4.3.2）。验收：编解码往返、版本字节校验、零向量用例。

### 2.3 Agent / Worker 二阶段流水线

- [x] **A1** `agent/internal/worker/video_frames.go`：ffmpeg 逐帧截图（6 个均分时间点，帧超时 20s，文件总超时 120s）（4.4.2）。验收：I2、I3。
- [x] **A2** `agent/internal/worker/phase2.go`：`Phase2Item` 处理主流程（stat 校验 → stale 检测 → 图片/视频分发 → 字段级错误）（4.4.3）。验收：I1~I4。
- [x] **A3** 主进程任务接入：`Phase2Task` 消息注册、按物理盘排队下发、`FeatureResult` 落本地 SQLite（`image_features.phash_parts/sobel_hist`、`video_frames` UPSERT）并置 `files.phase2_done`、错误一行一条写 `errors.log`。验收：I4、E2。
- [x] **A4** 看门狗复用：图片 30s / 视频 120s 超时 kill Worker 按崩溃处理（复用 M2 机制，仅确认二阶段任务也走同一看门狗）。验收：I3。

### 2.4 GUI Phase2Task 生成与分发

- [x] **G1** `gui/internal/phase2/dispatcher.go`：候选对 → 唯一 sha 集合（同 sha 去重选一副本）→ 查中心库缺失字段 → `fields_mask`/`frame_mask` → 按 `machine_id` 分组分片（5000/片）→ 路由下发（4.5.1）。验收：G 单测 + E1。
- [x] **G2** 自动触发接线：M3 一筛完成事件 → `BuildPhase2Tasks` → 下发；Agent 离线时该机器候选对标记延后重试。验收：E1。

### 2.5 复筛与成组

- [x] **R1** `gui/internal/phase2/unionfind.go`：并查集（路径压缩 + 按秩合并）（4.6.3）。验收：R 单测。
- [x] **R2** `gui/internal/phase2/judge.go`：图片对判定 `JudgeImagePair`、视频对判定 `JudgeVideoPair`（含有效帧 <4 → inconclusive、兜底 ≥4 帧通过）（4.6.2）。验收：R 单测全覆盖边界。
- [x] **R3** `gui/internal/phase2/rescreener.go`：`FeatureResult` 快路径缓存 → 双端齐触发判定 → `pair_scores` 落库；重启后从中心库恢复（4.6.4）。验收：I4、E2。
- [x] **R4** `gui/internal/phase2/groups.go`：`rebuildGroups(kind)` 全量重建 `dup_groups`/`dup_members`（含代表选择与 `score_json` 明细）（4.6.5）。验收：E2、E3。

### 2.6 GUI 三组展示

- [x] **U1** `gui/internal/web/groups.go`：`GET /api/groups`、`GET /api/groups/{id}`（4.7.1）。验收：API curl 用例。
- [x] **U2** `gui/web/groups.html`：三组 tab + 组列表 + 成员分数明细展开（4.7.2）。验收：E3 人工核对。

### 2.7 联调与验收

- [x] **T1** 确定性样本集已构造并分配给两个本地 Agent 身份；第二台独立 Windows 由项目所有者豁免。
- [x] **T2** E1~E4 端到端用例全部通过。
- [x] **T3** 崩溃/损坏注入用例 I3 通过，主进程零崩溃。

---

## 3. 目录与文件结构

仅列 M4 新增/触及的文件，均相对仓库根：

```
mediacore/
  include/mediacore.h              # 追加 M4 段（4.2.1）
  src/phash_parts.cpp              # 新增（4.2.2）
  src/sobel_hist.cpp               # 新增（4.2.3）
  tests/test_phase2.cpp            # 新增（4.2.4）
shared/
  proto/phase2.go                  # 新增：消息与位掩码（4.3.1）
  features/blob.go                 # 新增：BLOB 编解码与比对原语（4.3.2）
agent/
  internal/mediacore/phase2.go     # 新增：cgo 封装（4.4.1）
  internal/worker/phase2.go        # 新增：Worker 二阶段主流程（4.4.3）
  internal/worker/video_frames.go  # 新增：ffmpeg 6 帧管线（4.4.2）
  internal/agent/phase2_dispatch.go# 新增：主进程接入（消息注册/落库/errors.log）
gui/
  internal/phase2/dispatcher.go    # 新增：Phase2Task 生成与路由（4.5.1）
  internal/phase2/judge.go         # 新增：复筛判定（4.6.2）
  internal/phase2/unionfind.go     # 新增：并查集（4.6.3）
  internal/phase2/rescreener.go    # 新增：结果汇聚与触发（4.6.4）
  internal/phase2/groups.go        # 新增：组重建与入库（4.6.5）
  internal/phase2/config.go        # 新增：M4 配置项（5.2）
  internal/web/groups.go           # 新增：API（4.7.1）
  web/groups.html                  # 新增：三组展示页（4.7.2）
```

---

## 4. 关键接口与结构体定义

### 4.1 二阶段特征 BLOB 编码格式

所有 BLOB 小端序，首字节 `version` 用于演进。BLOB 由 Worker 内 DLL 输出的原生结构在 Go 侧编码，**跨进程/跨机器传输与落库统一使用该格式**。

#### 4.1.1 `phash_parts` BLOB — 76 字节

| 偏移 | 类型 | 值 | 说明 |
|---|---|---|---|
| 0 | uint8 | `1` | version |
| 1 | uint8 | `3` | rows |
| 2 | uint8 | `3` | cols |
| 3 | uint8 | `0` | flags（保留，须为 0） |
| 4..75 | 9 × uint64 LE | — | 每区 64bit DCT 感知哈希，row-major：`part[0]`=左上 … `part[8]`=右下 |

产生方式：灰度面双线性缩放至 96×96 → 分 3×3 区（每区 32×32）→ 每区 DCT-II 取左上 8×8 共 64 个系数 → 与 64 系数的中位数比较，大于记 1，bit i = 第 i 个系数（row-major）。

#### 4.1.2 `sobel_hist` BLOB — 516 字节

| 偏移 | 类型 | 值 | 说明 |
|---|---|---|---|
| 0 | uint8 | `1` | version |
| 1 | uint8 | `4` | grid（4×4 结构块） |
| 2 | uint8 | `8` | bins（每块 8 个方向 bin） |
| 3 | uint8 | `0` | flags（保留，须为 0） |
| 4..515 | 128 × float32 LE | — | 128 维直方图，row-major 块序、块内 bin 序；已整体 L2 归一化 |

产生方式：灰度面双线性缩放至 128×128 → 3×3 Sobel 得 `gx, gy` → 幅值 `mag = |gx|+|gy|`、无符号方向 `θ ∈ [0, π)` 量化 8 bin（每 bin 22.5°）→ 像素按所在 32×32 结构块将 `mag` 累加进对应 bin → 128 维向量 L2 归一化（零范数保持全零）。比对为两向量点积（即余弦相似度），零范数规则见 4.3.2。

#### 4.1.3 `video_frames` 行

视频不落单一 BLOB：每帧一行 `video_frames(sha512, frame_idx, pdq256 BLOB(32), phash_parts BLOB(76), sobel_hist BLOB(516))`，`frame_idx ∈ [0,6)`。`pdq256` 为 M2 已定义的 32 字节原始位序。

### 4.2 mediacore.dll 新增导出函数

#### 4.2.1 `mediacore/include/mediacore.h`（M4 追加段，完整）

```cpp
// ===================== M4：二阶段特征（分区 pHash / Sobel） =====================
// 前置：本文件 M2 段已定义 McGrayImage、MC_API、MC_CALL、
//       MC_OK / MC_ERR_INVALID_ARG / MC_ERR_DECODE（见 1.3 衔接假设）。
#include <stdint.h>

#define MC_ERR_TOO_SMALL      (-3)   /* 输入灰度面小于 8×8 */

#define MC_PHASH_GRID_ROWS    3
#define MC_PHASH_GRID_COLS    3
#define MC_PHASH_PARTS        9      /* 3×3 分区，每区一个 uint64 */
#define MC_PHASH_WORK_SIZE    96     /* 3 × 32 */
#define MC_PHASH_PART_SIZE    32
#define MC_PHASH_DCT_SIZE     8

#define MC_SOBEL_GRID         4
#define MC_SOBEL_BINS         8
#define MC_SOBEL_HIST_DIM     128    /* 4×4×8 */
#define MC_SOBEL_WORK_SIZE    128

#ifdef __cplusplus
extern "C" {
#endif

typedef struct McPhase2ImageOut {
    uint64_t phash_parts[MC_PHASH_PARTS];   /* row-major：0=左上 … 8=右下 */
    float    sobel_hist[MC_SOBEL_HIST_DIM]; /* 已 L2 归一化；零范数时全 0 */
} McPhase2ImageOut;

/* 分区 pHash：灰度面 → 96×96 → 3×3 区 → 每区 32×32 DCT-II 低频 8×8 → 64bit。
 * 返回 MC_OK / MC_ERR_INVALID_ARG / MC_ERR_TOO_SMALL。 */
MC_API int MC_CALL mc_phash_parts(const McGrayImage* img,
                                  uint64_t out_parts[MC_PHASH_PARTS]);

/* Sobel 结构块直方图：复用同一灰度面（不重复解码不重复灰度化）。
 * hist_len 必须 == MC_SOBEL_HIST_DIM。返回码同上。 */
MC_API int MC_CALL mc_sobel_hist(const McGrayImage* img, float* out_hist, int hist_len);

/* 一站式：一张灰度面算全部二阶段特征（任一失败返回首个错误码）。 */
MC_API int MC_CALL mc_phase2_image(const McGrayImage* img, McPhase2ImageOut* out);

#ifdef __cplusplus
}
#endif
```

#### 4.2.2 `mediacore/src/phash_parts.cpp`（完整实现）

```cpp
#include "../include/mediacore.h"

#include <cmath>
#include <cstring>
#include <algorithm>

namespace {

constexpr int kWork = MC_PHASH_WORK_SIZE;   // 96
constexpr int kPart = MC_PHASH_PART_SIZE;   // 32
constexpr int kDct  = MC_PHASH_DCT_SIZE;    // 8
constexpr double kPi = 3.14159265358979323846;

// cos((2x+1)·u·π / 64) 查找表，函数级 static 保证 C++11 起线程安全初始化。
struct CosTable {
    float v[kPart * kDct];
    CosTable() {
        for (int x = 0; x < kPart; ++x)
            for (int u = 0; u < kDct; ++u)
                v[x * kDct + u] = static_cast<float>(
                    std::cos((2.0 * x + 1.0) * u * kPi / (2.0 * kPart)));
    }
};

const CosTable& cosTable() {
    static const CosTable t;
    return t;
}

// 双线性缩放灰度面到 96×96（输出 float）。
void resizeTo96(const McGrayImage& src, float* dst /*96*96*/) {
    const double sx = static_cast<double>(src.width) / kWork;
    const double sy = static_cast<double>(src.height) / kWork;
    for (int y = 0; y < kWork; ++y) {
        double fy = (y + 0.5) * sy - 0.5;
        int y0 = static_cast<int>(std::floor(fy));
        double wy = fy - y0;
        if (y0 < 0) { y0 = 0; wy = 0.0; }
        int y1 = y0 + 1;
        if (y1 >= src.height) y1 = src.height - 1;
        const uint8_t* row0 = src.data + static_cast<size_t>(y0) * src.stride;
        const uint8_t* row1 = src.data + static_cast<size_t>(y1) * src.stride;
        for (int x = 0; x < kWork; ++x) {
            double fx = (x + 0.5) * sx - 0.5;
            int x0 = static_cast<int>(std::floor(fx));
            double wx = fx - x0;
            if (x0 < 0) { x0 = 0; wx = 0.0; }
            int x1 = x0 + 1;
            if (x1 >= src.width) x1 = src.width - 1;
            double p00 = row0[x0], p01 = row0[x1];
            double p10 = row1[x0], p11 = row1[x1];
            double top = p00 + (p01 - p00) * wx;
            double bot = p10 + (p11 - p10) * wx;
            dst[y * kWork + x] = static_cast<float>(top + (bot - top) * wy);
        }
    }
}

// 对 96×96 工作面中 (pr,pc) 区的 32×32 块做 DCT-II，取左上 8×8 生成 64bit。
uint64_t dctPhash64(const float* work, int pr, int pc, const CosTable& ct) {
    float coef[kDct * kDct];
    const int baseY = pr * kPart;
    const int baseX = pc * kPart;
    for (int v = 0; v < kDct; ++v) {
        for (int u = 0; u < kDct; ++u) {
            double sum = 0.0;
            for (int y = 0; y < kPart; ++y) {
                const float* row = work + (baseY + y) * kWork + baseX;
                double rowSum = 0.0;
                for (int x = 0; x < kPart; ++x)
                    rowSum += row[x] * ct.v[x * kDct + u];
                sum += rowSum * ct.v[y * kDct + v];
            }
            double cu = (u == 0) ? 1.0 / std::sqrt(2.0) : 1.0;
            double cv = (v == 0) ? 1.0 / std::sqrt(2.0) : 1.0;
            coef[v * kDct + u] = static_cast<float>(0.25 * cu * cv * sum);
        }
    }
    float sorted[kDct * kDct];
    std::memcpy(sorted, coef, sizeof(sorted));
    std::nth_element(sorted, sorted + kDct * kDct / 2, sorted + kDct * kDct);
    const float median = sorted[kDct * kDct / 2];
    uint64_t hash = 0;
    for (int i = 0; i < kDct * kDct; ++i)
        if (coef[i] > median) hash |= (1ULL << i);
    return hash;
}

} // namespace

extern "C" MC_API int MC_CALL mc_phash_parts(const McGrayImage* img,
                                             uint64_t out_parts[MC_PHASH_PARTS]) {
    if (!img || !img->data || !out_parts) return MC_ERR_INVALID_ARG;
    if (img->width < 8 || img->height < 8) return MC_ERR_TOO_SMALL;
    float work[kWork * kWork];
    resizeTo96(*img, work);
    const CosTable& ct = cosTable();
    for (int r = 0; r < MC_PHASH_GRID_ROWS; ++r)
        for (int c = 0; c < MC_PHASH_GRID_COLS; ++c)
            out_parts[r * MC_PHASH_GRID_COLS + c] = dctPhash64(work, r, c, ct);
    return MC_OK;
}
```

#### 4.2.3 `mediacore/src/sobel_hist.cpp`（完整实现）

```cpp
#include "../include/mediacore.h"

#include <cmath>
#include <vector>

namespace {

constexpr int kWork = MC_SOBEL_WORK_SIZE;   // 128
constexpr int kGrid = MC_SOBEL_GRID;        // 4
constexpr int kBins = MC_SOBEL_BINS;        // 8
constexpr int kDim  = MC_SOBEL_HIST_DIM;    // 128
constexpr double kPi = 3.14159265358979323846;

// 双线性缩放灰度面到 128×128（输出 float）。
void resizeTo128(const McGrayImage& src, float* dst /*128*128*/) {
    const double sx = static_cast<double>(src.width) / kWork;
    const double sy = static_cast<double>(src.height) / kWork;
    for (int y = 0; y < kWork; ++y) {
        double fy = (y + 0.5) * sy - 0.5;
        int y0 = static_cast<int>(std::floor(fy));
        double wy = fy - y0;
        if (y0 < 0) { y0 = 0; wy = 0.0; }
        int y1 = y0 + 1;
        if (y1 >= src.height) y1 = src.height - 1;
        const uint8_t* row0 = src.data + static_cast<size_t>(y0) * src.stride;
        const uint8_t* row1 = src.data + static_cast<size_t>(y1) * src.stride;
        for (int x = 0; x < kWork; ++x) {
            double fx = (x + 0.5) * sx - 0.5;
            int x0 = static_cast<int>(std::floor(fx));
            double wx = fx - x0;
            if (x0 < 0) { x0 = 0; wx = 0.0; }
            int x1 = x0 + 1;
            if (x1 >= src.width) x1 = src.width - 1;
            double p00 = row0[x0], p01 = row0[x1];
            double p10 = row1[x0], p11 = row1[x1];
            double top = p00 + (p01 - p00) * wx;
            double bot = p10 + (p11 - p10) * wx;
            dst[y * kWork + x] = static_cast<float>(top + (bot - top) * wy);
        }
    }
}

} // namespace

extern "C" MC_API int MC_CALL mc_sobel_hist(const McGrayImage* img, float* out_hist, int hist_len) {
    if (!img || !img->data || !out_hist) return MC_ERR_INVALID_ARG;
    if (hist_len != kDim) return MC_ERR_INVALID_ARG;
    if (img->width < 8 || img->height < 8) return MC_ERR_TOO_SMALL;

    std::vector<float> workBuf(kWork * kWork);
    resizeTo128(*img, workBuf.data());
    const float* w = workBuf.data();

    float hist[kDim];
    for (int i = 0; i < kDim; ++i) hist[i] = 0.0f;

    const int cell = kWork / kGrid;  // 32×32 像素/结构块
    for (int y = 1; y < kWork - 1; ++y) {
        for (int x = 1; x < kWork - 1; ++x) {
            const float tl = w[(y - 1) * kWork + (x - 1)];
            const float tc = w[(y - 1) * kWork + x];
            const float tr = w[(y - 1) * kWork + (x + 1)];
            const float ml = w[y * kWork + (x - 1)];
            const float mr = w[y * kWork + (x + 1)];
            const float bl = w[(y + 1) * kWork + (x - 1)];
            const float bc = w[(y + 1) * kWork + x];
            const float br = w[(y + 1) * kWork + (x + 1)];
            const float gx = (tr + 2.0f * mr + br) - (tl + 2.0f * ml + bl);
            const float gy = (bl + 2.0f * bc + br) - (tl + 2.0f * tc + tr);
            const float mag = std::fabs(gx) + std::fabs(gy);
            if (mag < 1e-6f) continue;  // 平坦像素不进直方图
            double ang = std::atan2(static_cast<double>(gy), static_cast<double>(gx)); // (-π, π]
            if (ang < 0.0) ang += kPi;  // 无符号方向 [0, π)
            int bin = static_cast<int>(ang / kPi * kBins);
            if (bin >= kBins) bin = kBins - 1;  // ang == π 边界
            const int gyi = y / cell;
            const int gxi = x / cell;
            hist[(gyi * kGrid + gxi) * kBins + bin] += mag;
        }
    }

    double norm = 0.0;
    for (int i = 0; i < kDim; ++i) norm += static_cast<double>(hist[i]) * hist[i];
    norm = std::sqrt(norm);
    if (norm > 1e-9) {
        for (int i = 0; i < kDim; ++i) out_hist[i] = static_cast<float>(hist[i] / norm);
    } else {
        for (int i = 0; i < kDim; ++i) out_hist[i] = 0.0f;  // 纯色面：零范数保持全零
    }
    return MC_OK;
}

extern "C" MC_API int MC_CALL mc_phase2_image(const McGrayImage* img, McPhase2ImageOut* out) {
    if (!out) return MC_ERR_INVALID_ARG;
    int rc = mc_phash_parts(img, out->phash_parts);
    if (rc != MC_OK) return rc;
    return mc_sobel_hist(img, out->sobel_hist, MC_SOBEL_HIST_DIM);
}
```

#### 4.2.4 `mediacore/tests/test_phase2.cpp`（完整，GoogleTest）

测试素材由测试代码程序化生成（合成渐变图、棋盘格、纯色图），不依赖外部图片文件；`decode` 辅助函数直接构造 `McGrayImage`。

```cpp
#include "../include/mediacore.h"

#include <gtest/gtest.h>
#include <cmath>
#include <cstdint>
#include <vector>

namespace {

// 生成 w×h 灰度面：水平正弦渐变 + 竖直棋盘格叠加，参数 phase 控制纹理相位。
std::vector<uint8_t> makeGray(int w, int h, double phase) {
    std::vector<uint8_t> buf(static_cast<size_t>(w) * h);
    for (int y = 0; y < h; ++y) {
        for (int x = 0; x < w; ++x) {
            double v = 127.0 + 100.0 * std::sin((x + phase * 10.0) * 0.05)
                     + ((x / 16 + y / 16) % 2 ? 20.0 : -20.0);
            if (v < 0.0) v = 0.0;
            if (v > 255.0) v = 255.0;
            buf[static_cast<size_t>(y) * w + x] = static_cast<uint8_t>(v);
        }
    }
    return buf;
}

McGrayImage wrap(std::vector<uint8_t>& buf, int w, int h) {
    McGrayImage img;
    img.data = buf.data();
    img.width = w;
    img.height = h;
    img.stride = w;
    return img;
}

int hamming64(uint64_t a, uint64_t b) {
    return __popcnt64(a ^ b);
}

double dotHist(const float* a, const float* b, int n) {
    double s = 0.0;
    for (int i = 0; i < n; ++i) s += static_cast<double>(a[i]) * b[i];
    return s;
}

} // namespace

TEST(Phase2, Deterministic) {
    auto buf = makeGray(400, 300, 1.0);
    auto img = wrap(buf, 400, 300);
    McPhase2ImageOut a, b;
    ASSERT_EQ(mc_phase2_image(&img, &a), MC_OK);
    ASSERT_EQ(mc_phase2_image(&img, &b), MC_OK);
    for (int i = 0; i < MC_PHASH_PARTS; ++i) EXPECT_EQ(a.phash_parts[i], b.phash_parts[i]);
    for (int i = 0; i < MC_SOBEL_HIST_DIM; ++i) EXPECT_FLOAT_EQ(a.sobel_hist[i], b.sobel_hist[i]);
}

TEST(Phase2, SimilarImageHighScores) {
    // 同纹理、不同尺寸与轻微相位偏移：模拟缩放/重压缩变体。
    auto bufA = makeGray(800, 600, 1.0);
    auto bufB = makeGray(680, 510, 1.05);
    auto imgA = wrap(bufA, 800, 600);
    auto imgB = wrap(bufB, 680, 510);
    McPhase2ImageOut a, b;
    ASSERT_EQ(mc_phase2_image(&imgA, &a), MC_OK);
    ASSERT_EQ(mc_phase2_image(&imgB, &b), MC_OK);
    int pass = 0;
    for (int i = 0; i < MC_PHASH_PARTS; ++i)
        if (hamming64(a.phash_parts[i], b.phash_parts[i]) <= 10) ++pass;
    EXPECT_GE(pass, 8);  // ≥80% 分区通过
    EXPECT_GE(dotHist(a.sobel_hist, b.sobel_hist, MC_SOBEL_HIST_DIM), 0.85);
}

TEST(Phase2, DifferentImageLowScores) {
    auto bufA = makeGray(800, 600, 1.0);
    auto bufB = makeGray(800, 600, 7.0);  // 相位完全不同的纹理
    auto imgA = wrap(bufA, 800, 600);
    auto imgB = wrap(bufB, 800, 600);
    McPhase2ImageOut a, b;
    ASSERT_EQ(mc_phase2_image(&imgA, &a), MC_OK);
    ASSERT_EQ(mc_phase2_image(&imgB, &b), MC_OK);
    int pass = 0;
    for (int i = 0; i < MC_PHASH_PARTS; ++i)
        if (hamming64(a.phash_parts[i], b.phash_parts[i]) <= 10) ++pass;
    EXPECT_LT(pass, 8);
}

TEST(Phase2, SolidColorZeroHist) {
    std::vector<uint8_t> buf(64 * 64, 128);  // 纯色
    auto img = wrap(buf, 64, 64);
    McPhase2ImageOut out;
    ASSERT_EQ(mc_phase2_image(&img, &out), MC_OK);
    for (int i = 0; i < MC_SOBEL_HIST_DIM; ++i) EXPECT_FLOAT_EQ(out.sobel_hist[i], 0.0f);
}

TEST(Phase2, ArgValidation) {
    McPhase2ImageOut out;
    EXPECT_EQ(mc_phase2_image(nullptr, &out), MC_ERR_INVALID_ARG);
    auto buf = makeGray(4, 4, 0.0);  // 小于 8×8
    auto img = wrap(buf, 4, 4);
    EXPECT_EQ(mc_phase2_image(&img, &out), MC_ERR_TOO_SMALL);
    float hist[MC_SOBEL_HIST_DIM];
    EXPECT_EQ(mc_sobel_hist(&img, hist, MC_SOBEL_HIST_DIM), MC_ERR_TOO_SMALL);
}
```

### 4.3 协议消息扩展与 BLOB 编解码（Go）

#### 4.3.1 `shared/proto/phase2.go`（完整）

帧层（长度前缀 + msgpack）与 envelope 属 M1；本文件只新增消息体与常量。`MsgFeatureResult` 复用 M1/M2 已注册的类型号（此处以占位常量注释说明，禁止重复注册）。

```go
package proto

// —— 消息类型常量（M4 新增）——
// MsgFeatureResult 沿用 M1/M2 注册值；这里只注册全新消息。
const (
	MsgPhase2Task uint8 = 20 // GUI → Agent：二阶段按需补算任务
)

// —— 媒体类别（与中心库 files.kind 对齐）——
const (
	KindImage uint8 = 1
	KindVideo uint8 = 2
)

// —— 特征字段位掩码：Phase2Item.FieldsMask / FeatureResult.FieldsDone ——
// 一阶段位（0~2）由 M2 定义，此处并列保持单一来源；二阶段位（3~5）为 M4 新增。
const (
	FieldSHA512      uint32 = 1 << 0 // 一阶段
	FieldPDQ256      uint32 = 1 << 1 // 一阶段（图片 PDQ）
	FieldThumbPDQ256 uint32 = 1 << 2 // 一阶段（视频缩略图 PDQ）
	FieldPHashParts  uint32 = 1 << 3 // 二阶段：图片分区 pHash
	FieldSobelHist   uint32 = 1 << 4 // 二阶段：图片 Sobel 直方图
	FieldVideoFrames uint32 = 1 << 5 // 二阶段：视频 6 帧全套（PDQ+pHash+Sobel）
)

// FrameMaskFull 表示 6 帧全需补算（Phase2Item.FrameMask == 0 时按此处理）。
const FrameMaskFull uint8 = 0x3F

// Phase2Task 是 GUI → Agent 的二阶段任务（plan §7）。
// 同一 task 内所有 item 位于同一台 Agent；GUI 按 machine_id 分组生成。
type Phase2Task struct {
	TaskID string       `msgpack:"task_id"`
	Items  []Phase2Item `msgpack:"items"`
}

// Phase2Item 描述单个文件待补算的字段。
// SHA512/Size/MtimeMs 由 GUI 从中心库带出：Agent 执行前 stat 校验，
// size/mtime 未变则信任 SHA512 跳过重哈希；变了则重算并做 stale 检测（见 4.4.3）。
type Phase2Item struct {
	Path       string `msgpack:"path"`
	Kind       uint8  `msgpack:"kind"`        // KindImage / KindVideo
	FieldsMask uint32 `msgpack:"fields_mask"` // 缺失字段位掩码
	FrameMask  uint8  `msgpack:"frame_mask"`  // 视频：bit i=需补第 i 帧；0 视为 FrameMaskFull
	SHA512     []byte `msgpack:"sha512"`      // 64 字节
	Size       int64  `msgpack:"size"`
	MtimeMs    int64  `msgpack:"mtime_ms"`
	DurationMs int64  `msgpack:"duration_ms,omitempty"` // 视频必填（决定 6 帧时间点）
}

// FieldError 描述单字段级失败（失败只标记当前字段，其余照常回传，plan §4.2）。
type FieldError struct {
	Field uint32 `msgpack:"field"` // 对应 FieldXxx 位；0 表示文件级失败（如 stat 失败）
	Stage string `msgpack:"stage"` // stat / decode / phash / sobel / ffmpeg / stale
	Msg   string `msgpack:"msg"`
}

// FrameFeature 是视频单帧的二阶段特征（对应 video_frames 一行）。
type FrameFeature struct {
	FrameIdx   int    `msgpack:"frame_idx"`          // 0..5
	TimeMs     int64  `msgpack:"time_ms"`            // 截图时间点 = duration×(2i+1)/12
	PDQ256     []byte `msgpack:"pdq256,omitempty"`   // 32 字节
	Quality    int    `msgpack:"quality,omitempty"`  // PDQ Quality
	PHashParts []byte `msgpack:"phash_parts,omitempty"` // 76 字节 BLOB（4.1.1）
	SobelHist  []byte `msgpack:"sobel_hist,omitempty"`  // 516 字节 BLOB（4.1.2）
	Error      string `msgpack:"error,omitempty"`    // 该帧失败原因（ffmpeg/解码）
}

// FeatureResult 是 Agent → GUI 的结果回传（plan §7：批量流式回传，
// 每条消息一个文件，Agent 连续发送、GUI 流式消费）。
// M1/M2 已有字段保持原名；M4 追加字段全部 omitempty，
// 依赖 msgpack map 编码的向后兼容（旧端忽略未知键）。
type FeatureResult struct {
	TaskID     string `msgpack:"task_id"`
	Path       string `msgpack:"path"`
	Kind       uint8  `msgpack:"kind"`
	SHA512     []byte `msgpack:"sha512"`
	FieldsDone uint32 `msgpack:"fields_done"` // 本次成功字段位掩码

	// —— M1/M2 一阶段字段（已有，列出仅保持单一来源）——
	PDQ256       []byte `msgpack:"pdq256,omitempty"`
	Quality      int    `msgpack:"quality,omitempty"`
	Width        int    `msgpack:"width,omitempty"`
	Height       int    `msgpack:"height,omitempty"`
	DurationMs   int64  `msgpack:"duration_ms,omitempty"`
	ThumbPath    string `msgpack:"thumb_path,omitempty"`
	ThumbPDQ256  []byte `msgpack:"thumb_pdq256,omitempty"`
	ThumbQuality int    `msgpack:"thumb_quality,omitempty"`

	// —— M4 二阶段新增 ——
	PHashParts []byte         `msgpack:"phash_parts,omitempty"` // 图片：76 字节 BLOB
	SobelHist  []byte         `msgpack:"sobel_hist,omitempty"`  // 图片：516 字节 BLOB
	Frames     []FrameFeature `msgpack:"frames,omitempty"`      // 视频：6 帧（含失败帧）
	Errors     []FieldError   `msgpack:"errors,omitempty"`
}
```

#### 4.3.2 `shared/features/blob.go`（完整）

BLOB 编解码与比对原语。`SobelCosine` 的零范数规则在此统一定义：**双零向量 → 1.0**（两张纯色图内容视为一致；此类图 PDQ Quality 低，通常已被一筛质量剪枝，走到这里的极少）；**一零一非零 → 0.0**。

```go
package features

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
)

// —— phash_parts BLOB（4.1.1）——
const (
	PHashVersion      = 1
	PHashGridRows     = 3
	PHashGridCols     = 3
	PHashPartCount    = PHashGridRows * PHashGridCols // 9
	PHashPartsBlobLen = 4 + PHashPartCount*8          // 76
)

// —— sobel_hist BLOB（4.1.2）——
const (
	SobelVersion     = 1
	SobelGrid        = 4
	SobelBins        = 8
	SobelHistDim     = SobelGrid * SobelGrid * SobelBins // 128
	SobelHistBlobLen = 4 + SobelHistDim*4                // 516
)

var (
	ErrBlobTooShort = errors.New("features: blob too short")
	ErrBlobVersion  = errors.New("features: unsupported blob version")
	ErrBlobLayout   = errors.New("features: unexpected blob layout")
)

func EncodePHashParts(parts [PHashPartCount]uint64) []byte {
	b := make([]byte, PHashPartsBlobLen)
	b[0] = PHashVersion
	b[1] = PHashGridRows
	b[2] = PHashGridCols
	b[3] = 0
	for i, p := range parts {
		binary.LittleEndian.PutUint64(b[4+i*8:], p)
	}
	return b
}

func DecodePHashParts(blob []byte) ([PHashPartCount]uint64, error) {
	var parts [PHashPartCount]uint64
	if len(blob) < PHashPartsBlobLen {
		return parts, ErrBlobTooShort
	}
	if blob[0] != PHashVersion {
		return parts, fmt.Errorf("%w: %d", ErrBlobVersion, blob[0])
	}
	if blob[1] != PHashGridRows || blob[2] != PHashGridCols {
		return parts, ErrBlobLayout
	}
	for i := range parts {
		parts[i] = binary.LittleEndian.Uint64(blob[4+i*8:])
	}
	return parts, nil
}

func EncodeSobelHist(hist []float32) ([]byte, error) {
	if len(hist) != SobelHistDim {
		return nil, fmt.Errorf("features: sobel hist dim %d, want %d", len(hist), SobelHistDim)
	}
	b := make([]byte, SobelHistBlobLen)
	b[0] = SobelVersion
	b[1] = SobelGrid
	b[2] = SobelBins
	b[3] = 0
	for i, v := range hist {
		binary.LittleEndian.PutUint32(b[4+i*4:], math.Float32bits(v))
	}
	return b, nil
}

func DecodeSobelHist(blob []byte) ([]float32, error) {
	if len(blob) < SobelHistBlobLen {
		return nil, ErrBlobTooShort
	}
	if blob[0] != SobelVersion {
		return nil, fmt.Errorf("%w: %d", ErrBlobVersion, blob[0])
	}
	if blob[1] != SobelGrid || blob[2] != SobelBins {
		return nil, ErrBlobLayout
	}
	hist := make([]float32, SobelHistDim)
	for i := range hist {
		hist[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[4+i*4:]))
	}
	return hist, nil
}

// Hamming64 计算两个 64bit 分区哈希的汉明距离。
func Hamming64(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}

// PHashPassRatio 返回 3×3 分区中汉明距离 ≤ partThreshold 的区数比例（0~1）。
func PHashPassRatio(a, b [PHashPartCount]uint64, partThreshold int) float64 {
	pass := 0
	for i := 0; i < PHashPartCount; i++ {
		if Hamming64(a[i], b[i]) <= partThreshold {
			pass++
		}
	}
	return float64(pass) / float64(PHashPartCount)
}

// SobelCosine 计算两个已 L2 归一化直方图的余弦相似度（点积）。
// 零范数规则：双零 → 1.0；一零一非零 → 0.0。
func SobelCosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := 0; i < SobelHistDim && i < len(a) && i < len(b); i++ {
		fa, fb := float64(a[i]), float64(b[i])
		dot += fa * fb
		na += fa * fa
		nb += fb * fb
	}
	const eps = 1e-9
	if na < eps && nb < eps {
		return 1.0
	}
	if na < eps || nb < eps {
		return 0.0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
```

### 4.4 Agent / Worker 二阶段实现

#### 4.4.1 `agent/internal/mediacore/phase2.go`（完整 cgo 封装）

cgo 细节收敛在 M2 已建的 `internal/mediacore` 包内：本文件假设该包 cgo preamble 中已 typedef `McGrayImage` / `McPhase2ImageOut` 并声明 `mc_phase2_image`（若 M2 preamble 缺 M4 声明，按 4.2.1 在 M2 头文件追加后重新生成即可，**不要**在两个 preamble 中重复 typedef）。

```go
package mediacore

import (
	"errors"
	"fmt"
)

// Phase2ImageOut 是 DLL mc_phase2_image 输出的 Go 镜像（4.2.1）。
type Phase2ImageOut struct {
	PHashParts [9]uint64
	SobelHist  [128]float32
}

var errPhase2 = errors.New("mediacore: phase2 failed")

// Phase2 在同一灰度面上依次产出分区 pHash 与 Sobel 直方图，
// 不重复解码、不重复灰度化（plan §4.2 二阶段第 1 条）。
func (g *GrayImage) Phase2() (*Phase2ImageOut, error) {
	if g == nil || g.c == nil {
		return nil, fmt.Errorf("%w: nil gray image", errPhase2)
	}
	out := &Phase2ImageOut{}
	rc := mcPhase2Image(g.c, &out.PHashParts, &out.SobelHist) // 包内私有 cgo 适配
	if rc != mcOK {
		return nil, fmt.Errorf("%w: rc=%d", errPhase2, rc)
	}
	return out, nil
}
```

其中 `mcPhase2Image` 与 `mcOK` 是包内私有 cgo 适配（写在 M2 的 cgo 文件中，调用约定如下）：

```go
// 以下片段并入 M2 的 cgo 文件（如 mediacore.go），不是独立文件。
// preamble 追加（类型 McGrayImage 已有，勿重复 typedef）：
//
//   typedef struct McPhase2ImageOut {
//       uint64_t phash_parts[9];
//       float    sobel_hist[128];
//   } McPhase2ImageOut;
//   extern int mc_phase2_image(const McGrayImage* img, McPhase2ImageOut* out);
//
// Go 适配函数：
//
//   const mcOK = 0
//
//   func mcPhase2Image(img *C.McGrayImage, parts *[9]uint64, hist *[128]float32) int {
//       var out C.McPhase2ImageOut
//       rc := C.mc_phase2_image(img, &out)
//       if rc != mcOK {
//           return int(rc)
//       }
//       for i := 0; i < 9; i++ {
//           parts[i] = uint64(out.phash_parts[i])
//       }
//       for i := 0; i < 128; i++ {
//           hist[i] = float32(out.sobel_hist[i])
//       }
//       return mcOK
//   }
```

#### 4.4.2 `agent/internal/worker/video_frames.go`（完整）

视频 6 帧管线：时间点 `t_i = duration_ms × (2i+1) / 12`（i=0..5，即 1/12 … 11/12，plan §4.2），每帧一个 ffmpeg 子进程（天然崩溃隔离，plan §2），`-ss` 前置快速 seek，PNG 经 stdout 回传交 DLL 从内存解码。帧子进程超时 20s，整个视频 120s 总预算（plan §9 视频单文件超时）。

```go
package worker

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"time"

	"example.com/dedup/agent/internal/mediacore"
	"example.com/dedup/shared/features"
	"example.com/dedup/shared/proto"
)

const (
	videoTotalTimeout = 120 * time.Second // plan §9：视频单文件超时
	frameCmdTimeout   = 20 * time.Second  // 单帧 ffmpeg 子进程超时
	frameCount        = 6
	decodeMaxDim      = 512 // 灰度面最长边（与 M2 普扫对齐）
)

// ffmpegFrame 在 tMs 毫秒处截一帧，返回 PNG 字节。
func ffmpegFrame(ctx context.Context, ffmpegBin, path string, tMs int64) ([]byte, error) {
	fctx, cancel := context.WithTimeout(ctx, frameCmdTimeout)
	defer cancel()
	sec := strconv.FormatFloat(float64(tMs)/1000.0, 'f', 3, 64)
	cmd := exec.CommandContext(fctx, ffmpegBin,
		"-hide_banner", "-loglevel", "error",
		"-ss", sec,
		"-i", path,
		"-frames:v", "1",
		"-f", "image2pipe",
		"-vcodec", "png",
		"-")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg frame@%dms: %v: %s", tMs, err, stderr.String())
	}
	if stdout.Len() == 0 {
		return nil, fmt.Errorf("ffmpeg frame@%dms: empty output", tMs)
	}
	return stdout.Bytes(), nil
}

// frameTimeMs 返回第 idx 帧的截图时间点：duration×(2i+1)/12。
func frameTimeMs(durationMs int64, idx int) int64 {
	return durationMs * int64(2*idx+1) / 12
}

// videoPhase2Frames 对视频执行 6 帧二阶段流程。
// frameMask：bit i = 需要补算第 i 帧；每帧 PDQ-256 + 分区 pHash + Sobel（plan §4.2）。
// 失败只影响当前帧：错误写入 FrameFeature.Error，其余帧照常。
func (w *Worker) videoPhase2Frames(item *proto.Phase2Item) []proto.FrameFeature {
	ctx, cancel := context.WithTimeout(w.ctx, videoTotalTimeout)
	defer cancel()

	mask := item.FrameMask
	if mask == 0 {
		mask = proto.FrameMaskFull
	}
	frames := make([]proto.FrameFeature, 0, frameCount)
	for i := 0; i < frameCount; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue // 该帧中心库已有，跳过
		}
		tMs := frameTimeMs(item.DurationMs, i)
		fr := proto.FrameFeature{FrameIdx: i, TimeMs: tMs}

		png, err := ffmpegFrame(ctx, w.cfg.FFmpegBin, item.Path, tMs)
		if err != nil {
			fr.Error = err.Error()
			frames = append(frames, fr)
			continue
		}
		g, err := mediacore.DecodeFromMemory(png, decodeMaxDim)
		if err != nil {
			fr.Error = fmt.Sprintf("decode frame@%dms: %v", tMs, err)
			frames = append(frames, fr)
			continue
		}
		hash, quality, err := g.PDQ256()
		if err != nil {
			g.Free()
			fr.Error = fmt.Sprintf("pdq frame@%dms: %v", tMs, err)
			frames = append(frames, fr)
			continue
		}
		out, err := g.Phase2()
		g.Free()
		if err != nil {
			fr.Error = fmt.Sprintf("phase2 frame@%dms: %v", tMs, err)
			frames = append(frames, fr)
			continue
		}
		fr.PDQ256 = hash[:]
		fr.Quality = quality
		fr.PHashParts = features.EncodePHashParts(out.PHashParts)
		sobelBlob, err := features.EncodeSobelHist(out.SobelHist[:])
		if err != nil {
			fr.Error = fmt.Sprintf("encode sobel frame@%dms: %v", tMs, err)
			frames = append(frames, fr)
			continue
		}
		fr.SobelHist = sobelBlob
		frames = append(frames, fr)
	}
	return frames
}
```

#### 4.4.3 `agent/internal/worker/phase2.go`（完整）

Worker 二阶段主流程。**图片"只读一次"**：文件字节读一次 → 内存解码一次 → 同一灰度面算 pHash+Sobel（plan §4.2、§2 风险表"二阶段图片需二次读盘"为已接受成本）。**stale 检测**：文件在普扫后被修改时，本对候选已失效——重算 SHA-512 校验，不一致则整体作废并上报 `stage=stale`，由 GUI 安排下轮普扫重算一阶段。

```go
package worker

import (
	"crypto/sha512"
	"fmt"
	"io"
	"os"

	"example.com/dedup/agent/internal/mediacore"
	"example.com/dedup/shared/features"
	"example.com/dedup/shared/proto"
)

const hashBlockSize = 4 << 20 // 4MB，与 HDD 读块对齐（plan §2）

// HandlePhase2Item 处理单个二阶段 item，返回待回传的 FeatureResult（永不返回 nil；
// 文件级失败也携带 Errors 返回，保证 GUI 侧可对账）。
func (w *Worker) HandlePhase2Item(item *proto.Phase2Item) *proto.FeatureResult {
	res := &proto.FeatureResult{
		TaskID: w.curTaskID,
		Path:   item.Path,
		Kind:   item.Kind,
		SHA512: item.SHA512,
	}

	// 1. stat 校验：size/mtime 未变 → 信任 GUI 带来的 SHA512，跳过重哈希。
	fi, err := os.Stat(item.Path)
	if err != nil {
		res.Errors = append(res.Errors, proto.FieldError{Field: 0, Stage: "stat", Msg: err.Error()})
		return res
	}
	if fi.Size() != item.Size || fi.ModTime().UnixMilli() != item.MtimeMs {
		sum, herr := hashFile(item.Path)
		if herr != nil {
			res.Errors = append(res.Errors, proto.FieldError{Field: proto.FieldSHA512, Stage: "hash", Msg: herr.Error()})
			return res
		}
		res.SHA512 = sum
		if !equalBytes(sum, item.SHA512) {
			// 文件内容已变：本次补算结果对原候选对无意义，整体作废。
			res.Errors = append(res.Errors, proto.FieldError{
				Field: 0, Stage: "stale",
				Msg: "file changed since phase1; phase2 result discarded, reschedule phase1",
			})
			return res
		}
	}

	// 2. 按类别分发（single-flight 字段过滤由主进程排队时完成，Worker 信任 fields_mask）。
	if item.Kind == proto.KindImage {
		w.imagePhase2(item, res)
	} else {
		res.Frames = w.videoPhase2Frames(item)
		ok := 0
		for _, fr := range res.Frames {
			if fr.Error == "" {
				ok++
			}
		}
		if ok == frameCount {
			res.FieldsDone |= proto.FieldVideoFrames
		}
		// 部分成功的帧照常回传落库（UPSERT per 帧），缺失帧下轮用 frame_mask 补。
	}
	return res
}

// imagePhase2：读一次 → 解码一次 → 同一灰度面算分区 pHash + Sobel。
func (w *Worker) imagePhase2(item *proto.Phase2Item, res *proto.FeatureResult) {
	needPHash := item.FieldsMask&proto.FieldPHashParts != 0
	needSobel := item.FieldsMask&proto.FieldSobelHist != 0
	if !needPHash && !needSobel {
		return
	}
	buf, err := os.ReadFile(item.Path) // 主进程已按 256MB 内存驻留阈值过滤（plan §9）
	if err != nil {
		res.Errors = append(res.Errors, proto.FieldError{Field: item.FieldsMask, Stage: "read", Msg: err.Error()})
		return
	}
	g, err := mediacore.DecodeFromMemory(buf, decodeMaxDim)
	if err != nil {
		res.Errors = append(res.Errors, proto.FieldError{Field: item.FieldsMask, Stage: "decode", Msg: err.Error()})
		return
	}
	defer g.Free()

	out, err := g.Phase2()
	if err != nil {
		res.Errors = append(res.Errors, proto.FieldError{Field: item.FieldsMask, Stage: "phash", Msg: err.Error()})
		return
	}
	if needPHash {
		res.PHashParts = features.EncodePHashParts(out.PHashParts)
		res.FieldsDone |= proto.FieldPHashParts
	}
	if needSobel {
		blob, err := features.EncodeSobelHist(out.SobelHist[:])
		if err != nil {
			res.Errors = append(res.Errors, proto.FieldError{Field: proto.FieldSobelHist, Stage: "sobel", Msg: err.Error()})
		} else {
			res.SobelHist = blob
			res.FieldsDone |= proto.FieldSobelHist
		}
	}
}

// hashFile 以 4MB 块流式重算 SHA-512（仅 stale 检测路径使用）。
func hashFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha512.New()
	buf := make([]byte, hashBlockSize)
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return nil, fmt.Errorf("hash %s: %w", path, err)
	}
	return h.Sum(nil), nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

主进程接入（A3）要点：收到 `Phase2Task` 后按 item 所在物理盘号入盘级队列（复用 M1/M2 调度器）；派发给 Worker 前联查本地 SQLite 做字段级剪枝（`image_features.phash_parts NOT NULL` 等），补 `frame_mask`；Worker 返回 `FeatureResult` 后落库（UPSERT `image_features`/`video_frames`，按 sha512 主键），更新 `files.phase2_done` 与 `missing_mask`，逐字段错误写 `errors.log` 一行一条（plan §8），再经 TCP 流式回传 GUI。落库 SQL 见 5.1。

### 4.5 GUI 侧 Phase2Task 生成与分发

#### 4.5.1 `gui/internal/phase2/dispatcher.go`（完整）

流程（plan §5.2）：一筛候选对 → 收集唯一 sha512 集合（同 sha 跨机器多副本只算一次，选首个可用副本；选中机器离线时换副本）→ 查中心库缺失字段生成 `fields_mask`/`frame_mask` → 按 `machine_id` 分组 → 分片（默认 5000 items/片）→ 经连接池路由下发。

```go
package phase2

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"

	"example.com/dedup/shared/proto"
)

// FileRef / CandidatePair 与 M3 一筛输出对齐（M3 文档若命名不同，以 M3 为准）。
type FileRef struct {
	FileID     int64
	MachineID  string
	Path       string
	SHA512     [64]byte
	Size       int64
	MtimeMs    int64
	DurationMs int64 // 视频有效
	Kind       uint8 // proto.KindImage / proto.KindVideo
}

type CandidatePair struct {
	Kind    uint8
	A, B    FileRef
	Hamming int // 一筛 PDQ 汉明距离，记入分数明细
}

// featureState 是中心库中某 sha 的二阶段已有字段（缺失字段剪枝依据，plan §4.4）。
type featureState struct {
	hasPHash  bool
	hasSobel  bool
	frameMask uint8 // 视频：bit i = 第 i 帧已有完整特征
}

func shaKey(s [64]byte) string { return hex.EncodeToString(s[:]) }

// BuildPhase2Tasks 把候选对转换为 machineID → Phase2Task 分片列表。
// shardSize ≤ 0 时取默认 5000。
func BuildPhase2Tasks(ctx context.Context, db *sql.DB, batchNo int64, pairs []CandidatePair, shardSize int) (map[string][]*proto.Phase2Task, error) {
	if shardSize <= 0 {
		shardSize = 5000
	}
	// 1. 收集唯一文件：按 sha 去重，保留任一可访问副本（特征以 sha512 为索引，
	//    同内容 N 个副本只算一次，plan §5.2）。
	uniq := make(map[string]FileRef, len(pairs))
	order := make([]string, 0, len(pairs)*2)
	for _, p := range pairs {
		for _, f := range []FileRef{p.A, p.B} {
			k := shaKey(f.SHA512)
			if _, ok := uniq[k]; !ok {
				uniq[k] = f
				order = append(order, k)
			}
		}
	}

	// 2. 查中心库缺失字段
	states := make(map[string]featureState, len(uniq))
	for _, k := range order {
		st, err := loadFeatureState(ctx, db, uniq[k])
		if err != nil {
			return nil, fmt.Errorf("load feature state %s: %w", uniq[k].Path, err)
		}
		states[k] = st
	}

	// 3. 生成 items 并按 machine 分组
	byMachine := make(map[string][]proto.Phase2Item)
	for _, k := range order {
		f := uniq[k]
		st := states[k]
		item := proto.Phase2Item{
			Path:       f.Path,
			Kind:       f.Kind,
			SHA512:     f.SHA512[:],
			Size:       f.Size,
			MtimeMs:    f.MtimeMs,
			DurationMs: f.DurationMs,
		}
		if f.Kind == proto.KindImage {
			if !st.hasPHash {
				item.FieldsMask |= proto.FieldPHashParts
			}
			if !st.hasSobel {
				item.FieldsMask |= proto.FieldSobelHist
			}
			if item.FieldsMask == 0 {
				continue // 特征已齐，无需补算
			}
		} else {
			missing := ^st.frameMask & proto.FrameMaskFull
			if missing == 0 {
				continue
			}
			item.FieldsMask = proto.FieldVideoFrames
			item.FrameMask = missing // 只补缺失帧
		}
		byMachine[f.MachineID] = append(byMachine[f.MachineID], item)
	}

	// 4. 分片
	out := make(map[string][]*proto.Phase2Task, len(byMachine))
	for machineID, items := range byMachine {
		for i := 0; i < len(items); i += shardSize {
			end := i + shardSize
			if end > len(items) {
				end = len(items)
			}
			out[machineID] = append(out[machineID], &proto.Phase2Task{
				TaskID: fmt.Sprintf("p2-%d-%s-%d", batchNo, machineID, i/shardSize),
				Items:  items[i:end],
			})
		}
	}
	return out, nil
}

// loadFeatureState 联查中心库 image_features / video_frames 得已有字段。
func loadFeatureState(ctx context.Context, db *sql.DB, f FileRef) (featureState, error) {
	var st featureState
	if f.Kind == proto.KindImage {
		err := db.QueryRowContext(ctx,
			`SELECT (phash_parts IS NOT NULL), (sobel_hist IS NOT NULL)
			   FROM image_features WHERE sha512 = $1`, f.SHA512[:]).
			Scan(&st.hasPHash, &st.hasSobel)
		if err == sql.ErrNoRows {
			return st, nil
		}
		return st, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT frame_idx FROM video_frames
		  WHERE sha512 = $1 AND pdq256 IS NOT NULL AND phash_parts IS NOT NULL AND sobel_hist IS NOT NULL`,
		f.SHA512[:])
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var idx int
		if err := rows.Scan(&idx); err != nil {
			return st, err
		}
		if idx >= 0 && idx < 6 {
			st.frameMask |= 1 << uint(idx)
		}
	}
	return st, rows.Err()
}
```

#### 4.5.2 自动触发与路由（接线说明，G2）

M3 一筛产出候选对后（事件或轮询 `scan_tasks.status`），GUI 依次：

1. `BuildPhase2Tasks` 生成任务；同时把候选对登记进 `Rescreener`（4.6.4）。
2. 对每台机器：`agentpool.Pool.IsOnline(machineID)` 在线 → `Send(machineID, MsgPhase2Task, task)`；离线 → 该批 items 的 sha 在 `Rescreener` 中标记"等待机器"，候选对保持 pending，机器上线后重发（任务级 ACK + 断点续传语义沿用 M1，plan §7）。
3. 同 sha 多副本时若选中机器离线：从中心库 `files` 按 sha 查另一在线机器副本重生成 item（`SELECT machine_id, path, size, mtime_ms FROM files WHERE sha512=$1 AND kind=$2`）。
4. `FeatureResult` 到达 → `Rescreener.OnFeatureResult`（快路径）；Agent 侧同时落本地库并随 5min 周期上行中心库（持久路径，供 GUI 重启恢复）。

### 4.6 复筛算法

#### 4.6.1 判定规则（伪代码）

```
# 图片对（a, b），特征均为 4.1 定义的 BLOB 解码结果
function JUDGE_IMAGE_PAIR(a, b):
    ratio = #{i ∈ 0..8 | hamming(a.parts[i], b.parts[i]) ≤ 10} / 9      # 分区通过比例
    if ratio < T2(0.80):
        return NOT_SIMILAR(score=ratio)                                  # 短路，不算 Sobel
    cos = sobel_cosine(a.hist, b.hist)   # 零范数规则：双零=1.0，一零=0.0
    if cos ≥ T3(0.85):
        return SIMILAR(final=cos, phash_ratio=ratio)
    return NOT_SIMILAR(score=cos, phash_ratio=ratio)

# 视频对（a, b），各 6 帧
function JUDGE_VIDEO_PAIR(a, b):
    valid = 0; sum = 0; passed = 0
    for i in 0..5:
        if a.frame[i] 缺失 or b.frame[i] 缺失:  continue               # 抽帧失败剔除
        valid += 1
        ratio_i = 帧 i 分区通过比例（同图片规则）
        if ratio_i < T2(0.80):  sim_i = 0
        else:                   sim_i = sobel_cosine(a.frame[i].hist, b.frame[i].hist)
        sum += sim_i
        if ratio_i ≥ T2 and sim_i ≥ T3(0.85):  passed += 1             # 该帧"通过"
    if valid < 4:  return INCONCLUSIVE                                  # 证据不足
    avg = sum / valid
    if avg ≥ T4(0.80) or passed ≥ 4:  return SIMILAR(final=avg, per_frame=6 帧明细)
    return NOT_SIMILAR(final=avg, per_frame=6 帧明细)

# 成组
function BUILD_GROUPS(kind):          # kind = image / video，分开建
    edges = 全部 SIMILAR 对（sha_a, sha_b）
    components = union_find(edges)
    每组：成员 = 组内每个 sha 的全部文件实例；representative = PDQ/thumb Quality 最高
          （平手取 machine_id+path 字典序最小）；score_json 记录各级分数明细
```

#### 4.6.2 `gui/internal/phase2/judge.go`（完整）

```go
package phase2

import (
	"example.com/dedup/shared/features"
	"example.com/dedup/shared/proto"
)

// Verdict 是复筛判定结论。
type Verdict int

const (
	VerdictNo           Verdict = iota // 不相似
	VerdictYes                         // 相似
	VerdictInconclusive                // 证据不足（有效帧 <4 / 特征缺失）
)

// ImagePairScore 是图片对复筛结果（全字段入 pair_scores，供分数明细展示）。
type ImagePairScore struct {
	PHashPassRatio float64
	SobelCosine    float64 // 短路灯未算时为 0
	Verdict        Verdict
}

// JudgeImagePair 实现 4.6.1 的图片对规则。
func JudgeImagePair(aParts, bParts [features.PHashPartCount]uint64, aHist, bHist []float32, cfg *Config) ImagePairScore {
	ratio := features.PHashPassRatio(aParts, bParts, cfg.PHashPartThreshold)
	sc := ImagePairScore{PHashPassRatio: ratio, Verdict: VerdictNo}
	if ratio < cfg.PHashPassT2 {
		return sc
	}
	sc.SobelCosine = features.SobelCosine(aHist, bHist)
	if sc.SobelCosine >= cfg.SobelT3 {
		sc.Verdict = VerdictYes
	}
	return sc
}

// FrameScore 是视频单帧比对明细。
type FrameScore struct {
	FrameIdx       int
	Valid          bool    // 双端帧特征齐全
	PHashPassRatio float64 // 无效帧为 0
	SobelCosine    float64 // 无效/短路帧为 0
	Sim            float64 // 该帧相似度（无效/短路帧为 0）
	Passed         bool    // 该帧是否"通过"（ratio≥T2 且 sim≥T3）
}

// VideoPairScore 是视频对复筛结果。
type VideoPairScore struct {
	Frames       [6]FrameScore
	ValidFrames  int
	AvgSim       float64
	PassedFrames int
	Verdict      Verdict
}

// FramePhase2 是单帧解码后的二阶段特征（内存形态）。
type FramePhase2 struct {
	PDQ256     [32]byte
	Quality    int
	PHashParts [features.PHashPartCount]uint64
	SobelHist  []float32
}

// JudgeVideoPair 实现 4.6.1 的视频对规则：6 帧逐对比对取平均值 ≥ T4，兜底 ≥4/6 帧通过。
func JudgeVideoPair(aFrames, bFrames [6]*FramePhase2, cfg *Config) VideoPairScore {
	var sc VideoPairScore
	sum := 0.0
	for i := 0; i < 6; i++ {
		fs := FrameScore{FrameIdx: i}
		a, b := aFrames[i], bFrames[i]
		if a == nil || b == nil {
			sc.Frames[i] = fs
			continue // 抽帧失败/未补算：剔除出分母
		}
		fs.Valid = true
		sc.ValidFrames++
		fs.PHashPassRatio = features.PHashPassRatio(a.PHashParts, b.PHashParts, cfg.PHashPartThreshold)
		if fs.PHashPassRatio >= cfg.PHashPassT2 {
			fs.SobelCosine = features.SobelCosine(a.SobelHist, b.SobelHist)
			fs.Sim = fs.SobelCosine
		}
		fs.Passed = fs.PHashPassRatio >= cfg.PHashPassT2 && fs.Sim >= cfg.SobelT3
		if fs.Passed {
			sc.PassedFrames++
		}
		sum += fs.Sim
		sc.Frames[i] = fs
	}
	if sc.ValidFrames < cfg.VideoMinValidFrames {
		sc.Verdict = VerdictInconclusive
		return sc
	}
	sc.AvgSim = sum / float64(sc.ValidFrames)
	if sc.AvgSim >= cfg.VideoAvgT4 || sc.PassedFrames >= cfg.VideoMinPassedFrames {
		sc.Verdict = VerdictYes
	}
	return sc
}

// 确保未使用告警消除：proto 仅用于文档化类型对齐。
var _ = proto.KindImage
```

（注：`var _ = proto.KindImage` 仅为保持 import 的显式依赖说明，实际工程中若未用到可删除该行及 import。）

#### 4.6.3 `gui/internal/phase2/unionfind.go`（完整）

```go
package phase2

// UnionFind 以 sha512 hex 为元素的并查集（路径压缩 + 按秩合并）。
// Find 对未注册元素自动插入，故无需显式 Add。
type UnionFind struct {
	parent map[string]string
	rank   map[string]int
}

func NewUnionFind() *UnionFind {
	return &UnionFind{
		parent: make(map[string]string),
		rank:   make(map[string]int),
	}
}

func (u *UnionFind) Find(x string) string {
	p, ok := u.parent[x]
	if !ok {
		u.parent[x] = x
		u.rank[x] = 0
		return x
	}
	if p != x {
		u.parent[x] = u.Find(p) // 路径压缩
	}
	return u.parent[x]
}

func (u *UnionFind) Union(a, b string) {
	ra, rb := u.Find(a), u.Find(b)
	if ra == rb {
		return
	}
	if u.rank[ra] < u.rank[rb] {
		ra, rb = rb, ra
	}
	u.parent[rb] = ra
	if u.rank[ra] == u.rank[rb] {
		u.rank[ra]++
	}
}

// Groups 返回 root → 成员列表（仅含 ≥2 个成员的组；单元素组对去重无意义）。
func (u *UnionFind) Groups() map[string][]string {
	groups := make(map[string][]string)
	for x := range u.parent {
		root := u.Find(x)
		groups[root] = append(groups[root], x)
	}
	for root, members := range groups {
		if len(members) < 2 {
			delete(groups, root)
		}
	}
	return groups
}
```

#### 4.6.4 `gui/internal/phase2/rescreener.go`（完整）

结果汇聚：`FeatureResult` 快路径更新内存特征缓存 → 该 sha 涉及的 pending 对双端齐 → 判定 → `pair_scores` 落库（幂等 UPSERT）。`pair_scores` 是复筛结果的持久层：GUI 重启后候选对从 M3 输出重放，特征从中心库 `image_features`/`video_frames` 重建缓存，已判对不重复判定。

```go
package phase2

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"

	"example.com/dedup/shared/features"
	"example.com/dedup/shared/proto"
)

// Rescreener 汇聚二阶段结果并对双端齐的候选对做复筛判定。
// 非线程安全：由 GUI 的消息消费 goroutine 单线程驱动。
type Rescreener struct {
	db  *sql.DB
	cfg *Config
	log *slog.Logger

	pairs  []CandidatePair
	status []Verdict // 与 pairs 对齐；未判定为 VerdictNo 且 judged=false
	judged []bool

	imgFeats map[string]*imageFeat // sha hex → 图片二阶段特征
	vidFeats map[string][6]*FramePhase2
	waiters  map[string][]int // sha hex → 未判定候选对索引

	OnPairJudged func(idx int, v Verdict) // 可选：UI 进度回调
}

type imageFeat struct {
	parts [features.PHashPartCount]uint64
	hist  []float32
}

func NewRescreener(db *sql.DB, cfg *Config, log *slog.Logger, pairs []CandidatePair) *Rescreener {
	r := &Rescreener{
		db:       db,
		cfg:      cfg,
		log:      log,
		pairs:    pairs,
		status:   make([]Verdict, len(pairs)),
		judged:   make([]bool, len(pairs)),
		imgFeats: make(map[string]*imageFeat),
		vidFeats: make(map[string][6]*FramePhase2),
		waiters:  make(map[string][]int),
	}
	for i, p := range pairs {
		// 跳过 pair_scores 中已判定的对（重启恢复）
		if r.loadVerdict(p) {
			r.judged[i] = true
			continue
		}
		ka, kb := shaKey(p.A.SHA512), shaKey(p.B.SHA512)
		r.waiters[ka] = append(r.waiters[ka], i)
		r.waiters[kb] = append(r.waiters[kb], i)
	}
	return r
}

// OnFeatureResult 处理一条 Agent 回传的二阶段结果（快路径）。
func (r *Rescreener) OnFeatureResult(ctx context.Context, res *proto.FeatureResult) error {
	if len(res.SHA512) != 64 {
		return fmt.Errorf("rescreener: bad sha512 len %d for %s", len(res.SHA512), res.Path)
	}
	var sha [64]byte
	copy(sha[:], res.SHA512)
	key := hex.EncodeToString(sha[:])

	if res.Kind == proto.KindImage {
		if res.FieldsDone&(proto.FieldPHashParts|proto.FieldSobelHist) == proto.FieldPHashParts|proto.FieldSobelHist {
			parts, err := features.DecodePHashParts(res.PHashParts)
			if err != nil {
				return fmt.Errorf("rescreener: decode phash %s: %w", res.Path, err)
			}
			hist, err := features.DecodeSobelHist(res.SobelHist)
			if err != nil {
				return fmt.Errorf("rescreener: decode sobel %s: %w", res.Path, err)
			}
			r.imgFeats[key] = &imageFeat{parts: parts, hist: hist}
		}
	} else {
		frames := r.vidFeats[key]
		for _, fr := range res.Frames {
			if fr.Error != "" || fr.FrameIdx < 0 || fr.FrameIdx >= 6 ||
				len(fr.PHashParts) == 0 || len(fr.SobelHist) == 0 {
				continue
			}
			parts, err := features.DecodePHashParts(fr.PHashParts)
			if err != nil {
				return fmt.Errorf("rescreener: decode frame phash %s#%d: %w", res.Path, fr.FrameIdx, err)
			}
			hist, err := features.DecodeSobelHist(fr.SobelHist)
			if err != nil {
				return fmt.Errorf("rescreener: decode frame sobel %s#%d: %w", res.Path, fr.FrameIdx, err)
			}
			fp := &FramePhase2{Quality: fr.Quality, PHashParts: parts, SobelHist: hist}
			if len(fr.PDQ256) == 32 {
				copy(fp.PDQ256[:], fr.PDQ256)
			}
			frames[fr.FrameIdx] = fp
		}
		r.vidFeats[key] = frames
	}

	// 触发涉及该 sha 的 pending 对
	for _, idx := range r.waiters[key] {
		if r.judged[idx] {
			continue
		}
		if err := r.tryJudge(ctx, idx); err != nil {
			return err
		}
	}
	return nil
}

// tryJudge：双端特征齐 → 判定 → pair_scores 落库。
func (r *Rescreener) tryJudge(ctx context.Context, idx int) error {
	p := r.pairs[idx]
	ka, kb := shaKey(p.A.SHA512), shaKey(p.B.SHA512)

	if p.Kind == proto.KindImage {
		a, aok := r.imgFeats[ka]
		b, bok := r.imgFeats[kb]
		if !aok || !bok {
			return nil
		}
		sc := JudgeImagePair(a.parts, b.parts, a.hist, b.hist, r.cfg)
		r.status[idx] = sc.Verdict
		r.judged[idx] = true
		err := r.saveImagePairScore(ctx, p, sc)
		if err != nil {
			return fmt.Errorf("rescreener: save pair score: %w", err)
		}
		if r.OnPairJudged != nil {
			r.OnPairJudged(idx, sc.Verdict)
		}
		return nil
	}

	a, aok := r.vidFeats[ka]
	b, bok := r.vidFeats[kb]
	if !aok || !bok {
		return nil
	}
	sc := JudgeVideoPair(a, b, r.cfg)
	if sc.Verdict == VerdictInconclusive {
		return nil // 帧可能还在路上；TaskDone 时由 SweepInconclusive 终判
	}
	r.status[idx] = sc.Verdict
	r.judged[idx] = true
	if err := r.saveVideoPairScore(ctx, p, sc); err != nil {
		return fmt.Errorf("rescreener: save pair score: %w", err)
	}
	if r.OnPairJudged != nil {
		r.OnPairJudged(idx, sc.Verdict)
	}
	return nil
}

// SweepInconclusive 在 Phase2Task 全部 TaskDone 后调用：
// 对仍未判定的对按"当前已有特征"终判，证据不足的以 INCONCLUSIVE 落库，
// 保证 GUI 展示可收敛、不悬挂。
func (r *Rescreener) SweepInconclusive(ctx context.Context) error {
	for idx := range r.pairs {
		if r.judged[idx] {
			continue
		}
		p := r.pairs[idx]
		ka, kb := shaKey(p.A.SHA512), shaKey(p.B.SHA512)
		if p.Kind == proto.KindVideo {
			a := r.vidFeats[ka]
			b := r.vidFeats[kb]
			sc := JudgeVideoPair(a, b, r.cfg)
			r.status[idx] = sc.Verdict
			r.judged[idx] = true
			if err := r.saveVideoPairScore(ctx, p, sc); err != nil {
				return err
			}
		} else {
			// 图片双端不齐：无法判定
			r.status[idx] = VerdictInconclusive
			r.judged[idx] = true
			if err := r.saveImagePairScore(ctx, p, ImagePairScore{Verdict: VerdictInconclusive}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Rescreener) loadVerdict(p CandidatePair) bool {
	var v int
	err := r.db.QueryRow(
		`SELECT verdict FROM pair_scores WHERE sha_a = $1 AND sha_b = $2 AND kind = $3`,
		minSHA(p.A.SHA512, p.B.SHA512), maxSHA(p.A.SHA512, p.B.SHA512), kindCode(p.Kind),
	).Scan(&v)
	if err != nil {
		return false
	}
	return true
}

func (r *Rescreener) saveImagePairScore(ctx context.Context, p CandidatePair, sc ImagePairScore) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO pair_scores (sha_a, sha_b, kind, phash_pass_ratio, sobel_cosine,
		                          frame_scores, final_score, verdict, hamming_t1, computed_at)
		 VALUES ($1,$2,$3,$4,$5,NULL,$6,$7,$8, now())
		 ON CONFLICT (sha_a, sha_b, kind) DO UPDATE SET
		   phash_pass_ratio = EXCLUDED.phash_pass_ratio,
		   sobel_cosine     = EXCLUDED.sobel_cosine,
		   final_score      = EXCLUDED.final_score,
		   verdict          = EXCLUDED.verdict,
		   computed_at      = now()`,
		minSHA(p.A.SHA512, p.B.SHA512), maxSHA(p.A.SHA512, p.B.SHA512), kindCode(p.Kind),
		sc.PHashPassRatio, sc.SobelCosine, sc.SobelCosine, int(sc.Verdict), p.Hamming)
	return err
}

func (r *Rescreener) saveVideoPairScore(ctx context.Context, p CandidatePair, sc VideoPairScore) error {
	frameJSON := marshalFrameScores(&sc)
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO pair_scores (sha_a, sha_b, kind, phash_pass_ratio, sobel_cosine,
		                          frame_scores, final_score, verdict, hamming_t1, computed_at)
		 VALUES ($1,$2,$3,NULL,NULL,$4,$5,$6,$7, now())
		 ON CONFLICT (sha_a, sha_b, kind) DO UPDATE SET
		   frame_scores = EXCLUDED.frame_scores,
		   final_score  = EXCLUDED.final_score,
		   verdict      = EXCLUDED.verdict,
		   computed_at  = now()`,
		minSHA(p.A.SHA512, p.B.SHA512), maxSHA(p.A.SHA512, p.B.SHA512), kindCode(p.Kind),
		frameJSON, sc.AvgSim, int(sc.Verdict), p.Hamming)
	return err
}
```

辅助函数与 JSON 明细编码（同文件）：

```go
// minSHA/maxSHA：pair_scores 主键规范化（无序对 → 有序键）。
func minSHA(a, b [64]byte) []byte {
	for i := 0; i < 64; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return a[:]
			}
			return b[:]
		}
	}
	return a[:]
}

func maxSHA(a, b [64]byte) []byte {
	for i := 0; i < 64; i++ {
		if a[i] != b[i] {
			if a[i] > b[i] {
				return a[:]
			}
			return b[:]
		}
	}
	return b[:]
}

// kindCode：dup/pair 表 kind 编码（0=exact 1=image 2=video，与 dup_groups.kind 对齐）。
func kindCode(kind uint8) int {
	if kind == proto.KindVideo {
		return 2
	}
	return 1
}

// frameScoreJSON / videoDetailJSON：pair_scores.frame_scores 的 JSONB 内容。
type frameScoreJSON struct {
	FrameIdx       int     `json:"i"`
	Valid          bool    `json:"valid"`
	PHashPassRatio float64 `json:"phash_ratio,omitempty"`
	Sim            float64 `json:"sim,omitempty"`
	Passed         bool    `json:"passed"`
}

type videoDetailJSON struct {
	ValidFrames  int              `json:"valid_frames"`
	AvgSim       float64          `json:"avg"`
	PassedFrames int              `json:"passed_frames"`
	Frames       []frameScoreJSON `json:"frames"`
}

func marshalFrameScores(sc *VideoPairScore) []byte {
	d := videoDetailJSON{
		ValidFrames:  sc.ValidFrames,
		AvgSim:       sc.AvgSim,
		PassedFrames: sc.PassedFrames,
		Frames:       make([]frameScoreJSON, 0, 6),
	}
	for _, fs := range sc.Frames {
		d.Frames = append(d.Frames, frameScoreJSON{
			FrameIdx:       fs.FrameIdx,
			Valid:          fs.Valid,
			PHashPassRatio: fs.PHashPassRatio,
			Sim:            fs.Sim,
			Passed:         fs.Passed,
		})
	}
	b, err := json.Marshal(d)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}
```

（`marshalFrameScores` 用 `encoding/json`，import 时补上。）

#### 4.6.5 `gui/internal/phase2/groups.go`（完整）

组重建：`SweepInconclusive` 完成后，对 `kind=image/video` 分别执行：从 `pair_scores` 取 `verdict=1` 的边 → 并查集 → 事务内删除该 kind 旧组并重建。**成员 = 组内每个 sha 的全部文件实例**（跨机器副本全部列出）；`score_json` 记录成员 sha 相对代表 sha 的边分数（无直接边时取该成员 sha 的最大分数边，标注 `via`）。

```go
package phase2

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

type pairEdge struct {
	a, b   string // sha hex（规范化：a < b）
	score  float64
	detail []byte // pair_scores.frame_scores（图片为 NULL）
}

// RebuildGroups 全量重建指定 kind（1=image 2=video）的相似组。幂等，可重跑。
func RebuildGroups(ctx context.Context, db *sql.DB, kind int) (int, error) {
	// 1. 取相似边
	rows, err := db.QueryContext(ctx,
		`SELECT sha_a, sha_b, final_score, frame_scores
		   FROM pair_scores WHERE kind = $1 AND verdict = 1`, kind)
	if err != nil {
		return 0, err
	}
	var edges []pairEdge
	for rows.Next() {
		var e pairEdge
		var shaA, shaB []byte
		var detail sql.NullString
		if err := rows.Scan(&shaA, &shaB, &e.score, &detail); err != nil {
			rows.Close()
			return 0, err
		}
		e.a, e.b = hex.EncodeToString(shaA), hex.EncodeToString(shaB)
		if detail.Valid {
			e.detail = []byte(detail.String)
		}
		edges = append(edges, e)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	rows.Close()

	// 2. 并查集合并
	uf := NewUnionFind()
	for _, e := range edges {
		uf.Union(e.a, e.b)
	}
	components := uf.Groups()

	// 3. 组内边索引（score_json 用）
	edgeByKey := make(map[string]pairEdge, len(edges))
	for _, e := range edges {
		edgeByKey[e.a+"|"+e.b] = e
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// 4. 清旧组（dup_members 由 ON DELETE CASCADE 级联）
	if _, err := tx.ExecContext(ctx, `DELETE FROM dup_groups WHERE kind = $1`, kind); err != nil {
		return 0, err
	}

	// 5. 逐组重建
	n := 0
	for _, shas := range components {
		sort.Strings(shas)
		if err := insertGroup(ctx, tx, kind, shas, edgeByKey); err != nil {
			return 0, err
		}
		n++
	}
	return n, tx.Commit()
}

// insertGroup 写入一组：representative = Quality 最高（平手取路径字典序最小）的文件。
func insertGroup(ctx context.Context, tx *sql.Tx, kind int, shas []string, edgeByKey map[string]pairEdge) error {
	// 5.1 取组内全部文件实例（成员 = 每个 sha 的所有副本，跨机器/跨盘）
	fileRows, err := tx.QueryContext(ctx,
		`SELECT f.id, f.machine_id, f.path, f.sha512,
		        COALESCE(im.pdq_quality, v.thumb_quality, 0) AS quality
		   FROM files f
		   LEFT JOIN image_features im ON im.sha512 = f.sha512
		   LEFT JOIN (SELECT sha512, thumb_quality FROM video_features) v ON v.sha512 = f.sha512
		  WHERE f.sha512 = ANY(
		    SELECT decode(x, 'hex') FROM unnest($1::text[]) AS x)`, shas)
	if err != nil {
		return err
	}
	type member struct {
		id       int64
		machine  string
		path     string
		sha      string
		quality  int
		score    float64
		detail   []byte
	}
	var members []member
	for fileRows.Next() {
		var m member
		var shaB []byte
		if err := fileRows.Scan(&m.id, &m.machine, &m.path, &shaB, &m.quality); err != nil {
			fileRows.Close()
			return err
		}
		m.sha = hex.EncodeToString(shaB)
		members = append(members, m)
	}
	if err := fileRows.Err(); err != nil {
		return err
	}
	fileRows.Close()
	if len(members) == 0 {
		return fmt.Errorf("rebuild groups: no files for component (kind=%d)", kind)
	}

	// 5.2 代表选择：Quality 最高；平手取 machine_id+path 字典序最小
	rep := members[0]
	for _, m := range members[1:] {
		if m.quality > rep.quality ||
			(m.quality == rep.quality && (m.machine+m.path) < (rep.machine+rep.path)) {
			rep = m
		}
	}

	// 5.3 写 dup_groups
	var groupID int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO dup_groups (kind, representative_file_id, member_count, created_at)
		 VALUES ($1, $2, $3, now()) RETURNING id`,
		kind, rep.id, len(members)).Scan(&groupID)
	if err != nil {
		return err
	}

	// 5.4 写 dup_members：score_json = 相对代表 sha 的边分数明细
	for _, m := range members {
		var scoreJSON []byte
		if m.id == rep.id {
			scoreJSON = []byte(`{"role":"representative"}`)
		} else {
			key := m.sha + "|" + rep.sha
			if m.sha > rep.sha {
				key = rep.sha + "|" + m.sha
			}
			e, ok := edgeByKey[key]
			if !ok {
				// 与代表无直接边：取该成员 sha 的最大分数关联边
				best := -1.0
				for _, other := range shas {
					if other == m.sha {
						continue
					}
					k2 := m.sha + "|" + other
					if m.sha > other {
						k2 = other + "|" + m.sha
					}
					if e2, ok2 := edgeByKey[k2]; ok2 && e2.score > best {
						best = e2.score
						e = e2
						ok = true
					}
				}
			}
			if ok {
				payload := map[string]any{
					"role":        "member",
					"vs_rep_sha":  rep.sha,
					"final_score": e.score,
				}
				if len(e.detail) > 0 {
					var detail any
					if json.Unmarshal(e.detail, &detail) == nil {
						payload["detail"] = detail
					}
				}
				scoreJSON, _ = json.Marshal(payload)
			} else {
				scoreJSON = []byte(`{"role":"member"}`)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO dup_members (group_id, file_id, score_json) VALUES ($1, $2, $3)`,
			groupID, m.id, scoreJSON); err != nil {
			return err
		}
	}
	return nil
}
```

（精确组 `kind=0` 由 M1 按 sha512 分组产出，M4 不动；三组展示统一读 `dup_groups`。）

### 4.7 GUI 三组展示

三类组（plan §5.3）：精确重复组（`kind=0`，M1 产出）/ 相似图片组（`kind=1`）/ 相似视频组（`kind=2`），统一读 `dup_groups` + `dup_members`，展示各级分数明细。M4 只新增 API 与页面；M1 已有 Web 服务框架时，handler 直接注册进去。

#### 4.7.1 `gui/internal/web/groups.go`（完整）

```go
package web

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
)

type groupSummary struct {
	ID             int64    `json:"id"`
	Kind           int      `json:"kind"`
	MemberCount    int      `json:"member_count"`
	RepMachine     string   `json:"rep_machine"`
	RepPath        string   `json:"rep_path"`
	Machines       []string `json:"machines"`
	CreatedAt      string   `json:"created_at"`
}

type groupListResponse struct {
	Total  int64          `json:"total"`
	Groups []groupSummary `json:"groups"`
}

type memberDetail struct {
	FileID    int64           `json:"file_id"`
	MachineID string          `json:"machine_id"`
	Path      string          `json:"path"`
	Size      int64           `json:"size"`
	ScoreJSON json.RawMessage `json:"score_json"` // 各级分数明细（4.6.5）
}

type groupDetailResponse struct {
	ID      int64          `json:"id"`
	Kind    int            `json:"kind"`
	Members []memberDetail `json:"members"`
}

// handleGroups：GET /api/groups?kind=0|1|2&page=1&size=50
func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind, _ := strconv.Atoi(q.Get("kind"))
	page, _ := strconv.Atoi(q.Get("page"))
	size, _ := strconv.Atoi(q.Get("size"))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 500 {
		size = 50
	}

	var total int64
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT count(*) FROM dup_groups WHERE kind = $1`, kind).Scan(&total); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT g.id, g.kind, g.member_count, f.machine_id, f.path,
		        to_char(g.created_at, 'YYYY-MM-DD HH24:MI:SS')
		   FROM dup_groups g
		   JOIN files f ON f.id = g.representative_file_id
		  WHERE g.kind = $1
		  ORDER BY g.member_count DESC, g.id
		  LIMIT $2 OFFSET $3`, kind, size, (page-1)*size)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	resp := groupListResponse{Total: total, Groups: []groupSummary{}}
	var ids []int64
	for rows.Next() {
		var g groupSummary
		if err := rows.Scan(&g.ID, &g.Kind, &g.MemberCount, &g.RepMachine, &g.RepPath, &g.CreatedAt); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp.Groups = append(resp.Groups, g)
		ids = append(ids, g.ID)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 每组的机器分布
	for i, id := range ids {
		mrows, err := s.db.QueryContext(r.Context(),
			`SELECT DISTINCT f.machine_id FROM dup_members m
			   JOIN files f ON f.id = m.file_id WHERE m.group_id = $1 ORDER BY 1`, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for mrows.Next() {
			var m string
			if err := mrows.Scan(&m); err == nil {
				resp.Groups[i].Machines = append(resp.Groups[i].Machines, m)
			}
		}
		mrows.Close()
	}
	writeJSON(w, resp)
}

// handleGroupDetail：GET /api/groups/{id}
func (s *Server) handleGroupDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	resp := groupDetailResponse{ID: id, Members: []memberDetail{}}
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT kind FROM dup_groups WHERE id = $1`, id).Scan(&resp.Kind); err != nil {
		http.Error(w, "group not found", http.StatusNotFound)
		return
	}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT m.file_id, f.machine_id, f.path, f.size, m.score_json
		   FROM dup_members m JOIN files f ON f.id = m.file_id
		  WHERE m.group_id = $1
		  ORDER BY (m.score_json->>'role' = 'representative') DESC, f.machine_id, f.path`, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var m memberDetail
		if err := rows.Scan(&m.FileID, &m.MachineID, &m.Path, &m.Size, &m.ScoreJSON); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp.Members = append(resp.Members, m)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, resp)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
```

路由注册（M1 Web 框架为 `net/http` ServeMux，Go 1.22 路径参数语法）：

```go
mux.HandleFunc("GET /api/groups", s.handleGroups)
mux.HandleFunc("GET /api/groups/{id}", s.handleGroupDetail)
mux.Handle("GET /groups", http.FileServer(http.Dir("web"))) // groups.html
```

#### 4.7.2 `gui/web/groups.html`（完整）

纯原生 JS 单页：三个 tab（精确 / 相似图片 / 相似视频）+ 组列表 + 展开成员分数明细。

```html
<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>去重结果</title>
<style>
  body { font-family: system-ui, sans-serif; margin: 24px; color: #222; }
  .tabs button { padding: 6px 16px; margin-right: 8px; cursor: pointer; }
  .tabs button.active { background: #2563eb; color: #fff; border: none; border-radius: 4px; }
  table { border-collapse: collapse; width: 100%; margin-top: 16px; }
  th, td { border: 1px solid #ddd; padding: 6px 10px; font-size: 13px; text-align: left; }
  th { background: #f3f4f6; }
  .group-row { cursor: pointer; }
  .group-row:hover { background: #eff6ff; }
  .members td { background: #fafafa; }
  pre { margin: 4px 0; font-size: 12px; white-space: pre-wrap; word-break: break-all; }
  .rep { font-weight: 600; }
  #pager { margin-top: 12px; }
</style>
</head>
<body>
<h2>重复文件组</h2>
<div class="tabs">
  <button data-kind="0" class="active">精确重复</button>
  <button data-kind="1">相似图片</button>
  <button data-kind="2">相似视频</button>
</div>
<table>
  <thead><tr><th>#</th><th>成员数</th><th>代表文件</th><th>机器分布</th><th>创建时间</th></tr></thead>
  <tbody id="tbody"></tbody>
</table>
<div id="pager">
  <button id="prev">上一页</button>
  <span id="pageinfo"></span>
  <button id="next">下一页</button>
</div>
<script>
let kind = 0, page = 1, size = 50, total = 0;
const tbody = document.getElementById('tbody');
const pageinfo = document.getElementById('pageinfo');

document.querySelectorAll('.tabs button').forEach(btn => {
  btn.onclick = () => {
    document.querySelectorAll('.tabs button').forEach(b => b.classList.remove('active'));
    btn.classList.add('active');
    kind = +btn.dataset.kind; page = 1; loadGroups();
  };
});
document.getElementById('prev').onclick = () => { if (page > 1) { page--; loadGroups(); } };
document.getElementById('next').onclick = () => { if (page * size < total) { page++; loadGroups(); } };

async function loadGroups() {
  const res = await fetch(`/api/groups?kind=${kind}&page=${page}&size=${size}`);
  const data = await res.json();
  total = data.total;
  tbody.innerHTML = '';
  for (const g of data.groups) {
    const tr = document.createElement('tr');
    tr.className = 'group-row';
    tr.innerHTML = `<td>${g.id}</td><td>${g.member_count}</td>` +
      `<td>[${g.rep_machine}] ${escapeHtml(g.rep_path)}</td>` +
      `<td>${g.machines.join(', ')}</td><td>${g.created_at}</td>`;
    tr.onclick = () => toggleMembers(tr, g.id);
    tbody.appendChild(tr);
  }
  pageinfo.textContent = ` 第 ${page} 页 / 共 ${total} 组 `;
}

async function toggleMembers(tr, id) {
  const next = tr.nextSibling;
  if (next && next.classList && next.classList.contains('members')) { next.remove(); return; }
  const res = await fetch(`/api/groups/${id}`);
  const data = await res.json();
  const mtr = document.createElement('tr');
  mtr.className = 'members';
  let html = `<td colspan="5"><table><thead><tr>` +
    `<th>机器</th><th>路径</th><th>大小</th><th>分数明细</th></tr></thead><tbody>`;
  for (const m of data.members) {
    const isRep = m.score_json && m.score_json.role === 'representative';
    html += `<tr class="${isRep ? 'rep' : ''}"><td>${escapeHtml(m.machine_id)}</td>` +
      `<td>${escapeHtml(m.path)}</td><td>${m.size}</td>` +
      `<td><pre>${escapeHtml(JSON.stringify(m.score_json, null, 1))}</pre></td></tr>`;
  }
  mtr.innerHTML = html + '</tbody></table></td>';
  tr.after(mtr);
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, c =>
    ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
}

loadGroups();
</script>
</body>
</html>
```

---

## 5. 数据模型与配置项

### 5.1 SQL DDL

#### 5.1.1 Agent 本地 SQLite（M4 涉及的写入）

表结构在 M1 建库时已按 plan §6.1 创建（`image_features.phash_parts/sobel_hist`、`video_frames` 各列、`files.phase2_done/missing_mask` 均已存在），**M4 无 DDL 变更**，仅新增写入语句：

```sql
-- 图片二阶段落库（Worker 结果到达时执行；特征以 sha512 为索引，single-flight）
INSERT INTO image_features (sha512, phash_parts, sobel_hist)
VALUES (?1, ?2, ?3)
ON CONFLICT (sha512) DO UPDATE SET
  phash_parts = COALESCE(excluded.phash_parts, image_features.phash_parts),
  sobel_hist  = COALESCE(excluded.sobel_hist,  image_features.sobel_hist);

-- 视频逐帧落库（失败帧不产生行；UPSERT per 帧）
INSERT INTO video_frames (sha512, frame_idx, pdq256, phash_parts, sobel_hist)
VALUES (?1, ?2, ?3, ?4, ?5)
ON CONFLICT (sha512, frame_idx) DO UPDATE SET
  pdq256      = excluded.pdq256,
  phash_parts = excluded.phash_parts,
  sobel_hist  = excluded.sobel_hist;

-- 文件级状态推进（图片二阶段两列齐 / 视频 6 帧齐时）
UPDATE files SET phase2_done = 1, missing_mask = ?2, updated_at = unixepoch()
WHERE machine_id = ?3 AND path = ?1;
```

#### 5.1.2 中心库 PostgreSQL（M4 新增一张表 + 结果表 DDL 细化）

`image_features` / `video_frames` / `video_features` 中心表为 plan §6.1 同构版（M1/M2 已建，Agent 上行写入，GUI 不写）。**M4 新增 `pair_scores`**：复筛判定结果的持久层——支撑 GUI 重启恢复、组全量重建与分数明细展示；写入者只有 GUI。

```sql
-- M4 新增：候选对复筛结果（主键规范化：sha_a < sha_b）
CREATE TABLE IF NOT EXISTS pair_scores (
  sha_a            BYTEA      NOT NULL,
  sha_b            BYTEA      NOT NULL,
  kind             SMALLINT   NOT NULL,             -- 1=image 2=video
  phash_pass_ratio REAL,                            -- 图片：分区通过比例
  sobel_cosine     REAL,                            -- 图片：Sobel 相关度
  frame_scores     JSONB,                           -- 视频：{avg, valid_frames, passed_frames, frames[]}
  final_score      REAL,                            -- 图片=sobel_cosine；视频=avg
  verdict          SMALLINT   NOT NULL,             -- 0=不相似 1=相似 2=证据不足
  hamming_t1       INT,                             -- 一筛汉明距离（明细溯源）
  computed_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (sha_a, sha_b, kind),
  CHECK (sha_a < sha_b)
);
CREATE INDEX IF NOT EXISTS idx_pair_scores_verdict ON pair_scores (kind, verdict);

-- 结果表（plan §6.2 细化；若 M1 已建则保持，本文仅明确语义）
CREATE TABLE IF NOT EXISTS dup_groups (
  id                    BIGSERIAL PRIMARY KEY,
  kind                  SMALLINT NOT NULL,          -- 0=exact 1=image 2=video
  representative_file_id BIGINT  NOT NULL REFERENCES files(id),
  member_count          INT      NOT NULL,
  created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS dup_members (
  group_id   BIGINT NOT NULL REFERENCES dup_groups(id) ON DELETE CASCADE,
  file_id    BIGINT NOT NULL REFERENCES files(id),
  score_json JSONB,                                 -- 各级分数明细（4.6.5）
  PRIMARY KEY (group_id, file_id)
);
CREATE INDEX IF NOT EXISTS idx_dup_groups_kind ON dup_groups (kind, member_count DESC);
```

### 5.2 配置项表

与 plan §9 一致的项直接引用；M4 细化新增的项标注"新增"。配置载体沿用 M1 的 GUI 配置文件（如 `gui.toml`），GUI 经 `ConfigPush` 把 Agent 侧需要的项（超时等）随任务下发。

| 配置键 | 默认值 | 含义 | 来源 |
|---|---|---|---|
| `phase2.phash.grid` | `3x3` | pHash 分区数 | plan §9 |
| `phase2.phash.pass_t2` | `0.80` | 分区通过比例阈值 T2 | plan §9 |
| `phase2.phash.part_threshold` | `10` | 单区 64bit 汉明通过阈值 | **新增** |
| `phase2.phash.work_size` | `96` | pHash 工作面边长（3×32） | **新增** |
| `phase2.sobel.grid` | `4` | Sobel 结构块边数（4×4） | **新增** |
| `phase2.sobel.bins` | `8` | 方向量化 bin 数 | **新增** |
| `phase2.sobel.work_size` | `128` | Sobel 工作面边长 | **新增** |
| `phase2.sobel.t3` | `0.85` | Sobel 直验相关度阈值 T3 | plan §9 |
| `phase2.video.frames` | `6` | 二阶段抽帧数（均分 1/12…11/12） | plan §9 |
| `phase2.video.avg_t4` | `0.80` | 6 帧平均相似度阈值 T4 | plan §9 |
| `phase2.video.min_passed` | `4` | 兜底：最少通过帧数 | plan §5.2 |
| `phase2.video.min_valid` | `4` | 最少有效帧数（不足 → inconclusive） | **新增** |
| `phase2.video.file_timeout_sec` | `120` | 视频单文件超时（看门狗） | plan §9 |
| `phase2.video.frame_cmd_timeout_sec` | `20` | 单帧 ffmpeg 子进程超时 | **新增** |
| `phase2.image.file_timeout_sec` | `30` | 图片单文件超时（看门狗） | plan §9 |
| `phase2.task.shard_size` | `5000` | 单 Phase2Task 最大 items | **新增** |
| `phase2.auto_dispatch` | `true` | 一筛完成自动下发二阶段 | **新增** |
| `worker.image_mem_threshold_mb` | `256` | 图片内存驻留阈值（超限走 M2 降级路径） | plan §9 |
| `decode.max_dim` | `512` | 灰度面最长边（与 M2 普扫对齐） | **新增**（建议值，M2 已对齐） |
| `sync.interval` | `5min / 5万行` | Agent 上行周期（二阶段结果同链路） | plan §9 |

`Config` Go 结构（`gui/internal/phase2/config.go`）：

```go
package phase2

// Config 是 M4 全部可调参数（默认值见 5.2 表）。
type Config struct {
	PHashPartThreshold int     `toml:"phash_part_threshold"` // 默认 10
	PHashPassT2        float64 `toml:"phash_pass_t2"`        // 默认 0.80
	SobelT3            float64 `toml:"sobel_t3"`             // 默认 0.85
	VideoAvgT4         float64 `toml:"video_avg_t4"`         // 默认 0.80
	VideoMinPassedFrames int   `toml:"video_min_passed"`     // 默认 4
	VideoMinValidFrames  int   `toml:"video_min_valid"`      // 默认 4
	TaskShardSize      int     `toml:"task_shard_size"`      // 默认 5000
	AutoDispatch       bool    `toml:"auto_dispatch"`        // 默认 true
}

func DefaultConfig() *Config {
	return &Config{
		PHashPartThreshold:   10,
		PHashPassT2:          0.80,
		SobelT3:              0.85,
		VideoAvgT4:           0.80,
		VideoMinPassedFrames: 4,
		VideoMinValidFrames:  4,
		TaskShardSize:        5000,
		AutoDispatch:         true,
	}
}
```

---

## 6. 测试与验收用例

层级：U（DLL 单元）→ I（Agent/Worker 集成）→ G/R（GUI 单元）→ E（端到端）。全部通过后才勾选 todolist 中 M4 状态。

### 6.1 DLL 单元测试（U1~U5）

对应 4.2.4 的 gtest 用例，构建运行：

```bash
cd mediacore && cmake -B build -DMC_BUILD_TESTS=ON && cmake --build build --config Release
./build/tests/Release/test_phase2.exe
```

| # | 用例 | 通过标准 |
|---|---|---|
| U1 | 编译链接 + `Phase2.Deterministic` | 同图两次调用 `mc_phase2_image` 输出逐位一致 |
| U2 | `Phase2.SimilarImageHighScores` | 合成相似图：分区通过 ≥8/9 且 Sobel 点积 ≥0.85 |
| U3 | `Phase2.DifferentImageLowScores` | 合成不相似图：分区通过 <8/9 |
| U4 | `Phase2.SolidColorZeroHist` | 纯色面 Sobel 直方图全零（零范数路径） |
| U5 | `Phase2.ArgValidation` | NULL / <8×8 / 错误 hist_len 均返回对应错误码，不崩溃 |

### 6.2 Agent / Worker 集成测试（I1~I4）

| # | 用例 | 步骤 | 通过标准 |
|---|---|---|---|
| I1 | 图片二阶段端到端（Worker 内） | 构造 `Phase2Item{kind=image, fields_mask=FieldPHashParts\|FieldSobelHist}` 指向一张真实 JPG，直接调用 `HandlePhase2Item` | `FieldsDone` 含两位；`PHashParts` 长 76、`SobelHist` 长 516；version 字节均为 1 |
| I2 | 视频 6 帧管线 | 对 60s 测试视频构造 `Phase2Item{kind=video, duration_ms=60000, frame_mask=0x3F}` | 返回 6 帧，时间点为 2500/7500/…/27500ms ±50ms；每帧 PDQ 32B + BLOB 齐全；`FieldsDone` 含 `FieldVideoFrames` |
| I3 | 损坏与超时注入 | ① 截断的 MP4（只有前 1MB）② 0 字节 JPG ③ 把 `ffmpeg` 替换为 sleep 300 的假二进制 | 主进程与 Worker 池均存活；帧级/字段级 error 回传；`errors.log` 一行一条；看门狗 120s 内 kill 假 ffmpeg 路径并写 `crash.log` |
| I4 | stale 检测 | 普扫后修改文件内容再下二阶段任务 | 回传 `stage=stale` 错误，结果不落 `image_features`，GUI 侧该对判 inconclusive |

G/R 单元测试（Go test，GUI 侧）：

| # | 用例 | 通过标准 |
|---|---|---|
| G1 | `BuildPhase2Tasks`：10 对候选含 3 个重复 sha、2 台机器、1 个已齐特征 | items 数 = 唯一 sha 数 − 已齐数；按机器分组；`fields_mask`/`frame_mask` 正确；分片 ≤5000 |
| R1 | 并查集：链式边 A-B、B-C、D-E | 得 2 组，成员正确；单边自动插入 |
| R2 | `JudgeImagePair` 边界：构造 parts 使通过比例恰为 0.80/0.79；hist 使余弦恰为 0.85/0.84；双零 hist | 比例 0.80 进入 Sobel、0.79 短路；0.85 判 Yes、0.84 判 No；双零 → cos=1.0 |
| R3 | `JudgeVideoPair`：6 帧全过 / 仅 avg≥0.8 / 仅 4 帧过 / 3 有效帧 | 分别判 Yes / Yes（平均路径）/ Yes（兜底）/ Inconclusive |

### 6.3 端到端验收（E1~E4）

#### 6.3.1 样本集构造（可执行命令）

准备：两台机器各挂一块测试盘；工具：`ffmpeg`、`ImageMagick`（`magick`）。源素材：20 张互不相同的高清照片（`src_img/00.jpg`…`19.jpg`）、5 段互不相同的 30~120s 视频（`src_vid/0.mp4`…`4.mp4`）。另备 10 张/3 段**完全无关**的干扰素材。

```bash
# ── 相似图片：每源图 4 个变体（共 20×5=100 张）──
mkdir -p samples/img
for i in $(seq 0 19); do
  cp "src_img/$i.jpg" "samples/img/${i}_orig.jpg"
  magick "src_img/$i.jpg" -resize 85%            "samples/img/${i}_resize.jpg"
  magick "src_img/$i.jpg" -quality 70            "samples/img/${i}_recompress.jpg"
  magick "src_img/$i.jpg" -modulate 110          "samples/img/${i}_bright.jpg"
  magick "src_img/$i.jpg" -attenuate 2 +noise Gaussian "samples/img/${i}_noise.jpg"
done
# 精确重复：任选 10 张原图各复制 2 份到不同目录
for i in $(seq 0 9); do
  cp "samples/img/${i}_orig.jpg" "samples/img/${i}_copyA.jpg"
  cp "samples/img/${i}_orig.jpg" "samples/img/${i}_copyB.jpg"
done
# 干扰：10 张无关图直接放入
cp unrelated/*.jpg samples/img/

# ── 相似视频：每源视频 4 个变体（共 5×5=25 段）──
mkdir -p samples/vid
for i in $(seq 0 4); do
  cp "src_vid/$i.mp4" "samples/vid/${i}_orig.mp4"
  ffmpeg -y -i "src_vid/$i.mp4" -c:v libx264 -crf 28 -c:a copy          "samples/vid/${i}_reenc.mp4"
  ffmpeg -y -i "src_vid/$i.mp4" -vf scale=1280:-2 -c:v libx264 -crf 23 -c:a aac "samples/vid/${i}_720p.mp4"
  ffmpeg -y -ss 1 -i "src_vid/$i.mp4" -t 58 -c copy                     "samples/vid/${i}_trim.mp4"
  ffmpeg -y -i "src_vid/$i.mp4" -vf "fade=t=in:st=0:d=1,fade=t=out:st=28:d=1" -c:a copy "samples/vid/${i}_fade.mp4"
done
# 精确重复：3 段原视频各复制 1 份
for i in 0 1 2; do cp "samples/vid/${i}_orig.mp4" "samples/vid/${i}_copy.mp4"; done
# 干扰：3 段无关视频直接放入
cp unrelated/*.mp4 samples/vid/

# ── 坏文件（I3 端到端版）──
head -c 1048576 /dev/urandom > samples/img/corrupt.jpg
head -c 1048576 "src_vid/0.mp4" > samples/vid/truncated.mp4
```

把 `samples/` 一分为二（按文件散列取模）分置两台机器。ground truth 清单：`img` 组 = 每个源图 {orig, resize, recompress, bright, noise}（+copyA/copyB 与 orig 同 sha，属精确组）；`vid` 组 = 每源视频 5 段（trim 段时长 −2s 仍在 ±2s 剪枝内）。

#### 6.3.2 用例与通过标准

| # | 用例 | 步骤 | 通过标准 |
|---|---|---|---|
| E1 | 自动下发 | 双机普扫 → M3 一筛完成 | 无需人工干预，两台 Agent 均在 60s 内收到 `Phase2Task`；`scan_tasks` 出现 phase=2 记录；task items 总数 = 唯一 sha 数（同 sha 多副本只算一次，SQL 核对：`SELECT count(DISTINCT sha512) FROM files WHERE sha512 IN (候选集)`） |
| E2 | 相似组正确性 | 复筛完成（`pair_scores` 无 pending）后查 API | `GET /api/groups?kind=1`：20 个源图组全部出现且同组 ≥5 成员（recall ≥90% 按变体对计）；无关干扰图不出现在任何相似组（精确率 100%）；`kind=2`：5 个源视频组各含 5 成员（trim 变体允许个别帧 invalid 但组须成立）；`kind=0`：复制的文件 100% 按 sha 成组 |
| E3 | 分数明细可见 | 浏览器开 `/groups` | 相似图片组成员 `score_json` 含 `final_score`（=Sobel 余弦）；相似视频组含 `detail.avg/passed_frames/frames[]` 六项；代表项标 `role=representative`；三 tab 切换正常 |
| E4 | 坏文件不中断 | E1~E2 全程 | 主进程零崩溃；`crash.log`/`errors.log` 有 `corrupt.jpg`、`truncated.mp4` 记录；其余样本判定不受影响 |

量化判定 SQL（在中心库执行）：

```sql
-- E2 图片召回：每个源图应有组且成员覆盖其变体（按路径前缀统计）
SELECT g.id, count(*) AS members
FROM dup_groups g
JOIN dup_members m ON m.group_id = g.id
JOIN files f ON f.id = m.file_id
WHERE g.kind = 1 AND f.path LIKE '%\_orig.jpg' ESCAPE '\'
GROUP BY g.id;

-- E2 干扰精确率：干扰素材不应出现在 kind=1/2 的组中
SELECT count(*) FROM dup_members m
JOIN files f ON f.id = m.file_id
JOIN dup_groups g ON g.id = m.group_id
WHERE g.kind IN (1,2) AND f.path LIKE '%unrelated%';   -- 期望 0

-- E1 同 SHA 只算一次：二阶段特征行数 = 唯一 sha 数，而非文件数
SELECT (SELECT count(*) FROM image_features WHERE phash_parts IS NOT NULL) AS feature_rows,
       (SELECT count(DISTINCT f.sha512) FROM files f
         JOIN dup_members m ON true JOIN dup_groups g ON g.kind = 1
        WHERE f.sha512 IN (SELECT sha_a FROM pair_scores WHERE kind=1
                           UNION SELECT sha_b FROM pair_scores WHERE kind=1)) AS uniq_sha;
```

### 6.4 性能基准（参考线，非硬门槛）

- 单 Worker 图片二阶段吞吐 ≥ 50 张/s（96² DCT + 128² Sobel，纯 CPU，不含读盘）。
- 60s 视频 6 帧全流程 ≤ 15s（含 6 次 ffmpeg 子进程冷启动）。
- 1 万对候选的复筛判定（特征已在内存）≤ 5s；`RebuildGroups` 10 万边 ≤ 10s。
- 超出参考线 2 倍以上时记录 profiling 数据，留 M6 处理。

---

## 7. 风险与注意事项

| # | 风险/事项 | 说明与缓解 |
|---|---|---|
| 1 | **旋转/裁剪变体漏判** | 分区 pHash 与 Sobel 结构直方图对 >5° 旋转、大幅裁剪天然敏感，这类变体会在 pHash 分区级被拒。属已知算法边界，M4 不处理；验收样本不含旋转大角度项，阈值调优与算法增强（如旋转不变特征）留 M6 之后评估 |
| 2 | **零范数 Sobel 向量** | 纯色/近纯色图直方图为零，已在 `SobelCosine` 定义双零=1.0、一零=0.0 规则（4.3.2）。此类图 PDQ Quality 低，正常已被一筛质量剪枝拦截；若实测误判，先调质量剪枝而非改本规则 |
| 3 | **视频抽帧失败导致误杀** | 损坏时间点/编码缺陷会让个别帧抽取失败。规则是剔除出分母 + 有效帧 <4 判 inconclusive（不误判相似、也不冤枉不相似）；inconclusive 对在 GUI 可见（`verdict=2`），下轮 `frame_mask` 补算后重判 |
| 4 | **二阶段二次读盘 IO** | plan §11 已接受（候选 <1%）。注意实现上图片整文件 `os.ReadFile` 前由主进程按 256MB 阈值过滤（plan §9）；超限图片走 M2 降级路径或标记跳过，不得无界分配内存 |
| 5 | **stale 文件竞态** | 普扫到二阶段之间文件被修改/删除：stat 校验 + SHA 重算兜底（4.4.3），stale 结果**不落库**（避免错误特征以新 sha 入库污染一筛），该文件等下轮普扫重算一阶段 |
| 6 | **BLOB 版本演进** | `phash_parts`/`sobel_hist` 首字节 version 当前为 1；调整分区数/网格/bin 数必须递增 version，旧 BLOB 解码报错后由缺失字段剪枝触发重算，不可原地改格式 |
| 7 | **组全量重建的写放大** | `RebuildGroups` 每轮 DELETE+INSERT 整个 kind（4.6.5）。百万级候选 <1% 时组规模有限，事务内完成可接受；`pair_scores` 幂等 UPSERT 保证可重跑。若 M6 压测暴露瓶颈，再改增量合并 |
| 8 | **cgo 崩溃面** | 新增导出函数全部工作在已解码的内存灰度面上，无外部输入直接驱动，崩溃面与 M2 相同、仍收敛在 Worker 进程内；主进程零 cgo 原则不变（plan §3） |
| 9 | **msgpack 兼容性前提** | `FeatureResult` 扩展依赖 M1 约定的 map 编码 + `omitempty`；若 M1 实际用了数组编码，新增字段会破坏兼容——开工前先在 M1 代码中确认编码形式，必要时把 M4 字段移入独立的 `Phase2Result` 消息 |
| 10 | **帧时间点与 ±2s 剪枝的交互** | trim 类变体（去头尾）时长差 ≤2s 可过一筛，但 6 个均分时间点内容已偏移；实测若此类变体召回低，优先评估"按内容对齐起点"方案（M6 后再议），M4 严格按 plan 的均分语义实现 |

---

## 附：验收通过判定

M4 完成的充要条件：第 6 节 U1~U5、I1~I4、G1/R1~R3、E1~E4 全部通过，且 `docs/todolist.md` 中 M4 行状态可勾选为 `[x]`。
