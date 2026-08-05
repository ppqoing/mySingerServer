# M2 一阶段特征计算 — 详细实施文档

> 依据：`docs/architecture-plan.md` v1.2（下称 plan），里程碑 M2，工期约 2 周。
> 本文档与 plan 的选型、默认参数（§9）、协议语义（§4、§7）、数据模型（§6）保持一致；所有新增设计均为 plan 框架内的工程落地，不改架构、不换选型。
> 验收标准（plan §10 M2）：投喂损坏文件主进程存活、`crash.log` 有记录；同 SHA-512 只解码一次；缩略图按路径命中缓存。

## 0. 前置条件与接口假设

### 0.1 现仓库兼容补充（2026-07-27）

M1 已按架构计划 v1.2 落在仓库根模块 `dedup`，并非下文早期草案假设的
`agent/` 子模块。M2 实施以已交付的 M1 接口为准，具体约束如下：

- 新增 Go 代码使用根目录 `cmd/`、`internal/`、`scripts/`，导入路径为
  `dedup/internal/...`；下文出现的 `agent/` 路径均按此映射，不另建第二个 Go module。
- 保持现有 JSON 配置格式，扩展 `internal/config/agent.go`，不引入并行 TOML 配置。
- SQLite、PostgreSQL 和 GUI/TCP 协议中的 SHA-512 继续使用 128 字符小写十六进制
  `TEXT`，保持 M1 数据与同步兼容；Worker IPC 与 mediacore ABI 内部使用 64 字节，
  仅在主进程 Store/协议边界编码或解码。
- 字段位沿用架构计划 v1.2/M4 的冻结分配：
  `SHA=1<<0`、`PDQ=1<<1`、`Thumb=1<<2`、`PHash=1<<3`、
  `Sobel=1<<4`、`VideoFrames=1<<5`。视频时长与缩略图共同受
  `FieldThumb` 控制；部分成功通过 UPSERT/COALESCE 保留，下轮重试该组合位。
  下文单独列出的 `MaskVideoDur`/`MaskVideoThumb` 仅表示 Worker 内部步骤，
  不新增或移动持久化/TCP 位号。
- `SavePhase1` 写入 `image_features`/`video_features` 后必须扩展 M1 同步器消费相应
  `sync_queue` 行，禁止制造永久积压。
- Worker 关闭以 B8 为准：先发 `shutdown`，等待最多 3 秒，再强杀残留进程；
  正常关闭、退出码 0 或关闭期间的管道 EOF 不记 `crash.log`、不重生。
- PDQ 上游固定为官方 `facebook/ThreatExchange` 提交
  `baefb4ed67b6cdc1d4c82dbaef858d50866ac424`；只复制本文指定目录、LICENSE，
  并把完整提交写入 `pdq_upstream/COMMIT`。
- §6.1.3 的 SHA-512 `abc` 正确向量为
  `ddaf35a193617abacc417349ae20413112e6fa4e89a97ea20a9eeee64b55d39a2192992a274fc1a836ba3c23a3feebbd454d4423643ce80e2a9ac94fa54ca49f`；
  旧草案中的 `ba7816...` 是 SHA-256，不得用于 M2 门禁。

M2 在 M1 交付物之上开发，开工前确认以下 M1 接口已存在（若 M1 尚未落地，需先补齐对应骨架）：

- Go 模块根目录 `agent/`（module `mediadedup/agent`），含 `cmd/agent`、`internal/store`（SQLite，`modernc.org/sqlite`，WAL）、`internal/config`、`internal/logger`（slog + lumberjack，`agent.log`/`errors.log`/`crash.log` 三个 logger）、`internal/proto`（`[4B 大端长度][msgpack body]` 帧编解码）。
- `files` 表已建（plan §6.1），`sync_queue` 已建，调度器产出"待计算文件任务"（路径 + size + mtime + 图片/视频类别）。
- 构建环境：Go 1.22+；**mingw-w64 gcc**（winlibs 或 msys2 版，仅 worker.exe 构建需要）；**MSVC 2022 + vcpkg + CMake ≥ 3.20**（mediacore.dll 构建需要）；`ffmpeg.exe`/`ffprobe.exe` 随包分发于 `bin/tools/`。

### 对 plan 4.1 的一处工程修正（重要）

plan 4.1 写"Agent 主进程 fork 自身为 Worker 子进程（`Agent.exe --worker` 模式）"，plan §11 又要求"主进程不加载 DLL"。cgo 链接的 DLL 是**进程启动时加载**的：若单一 exe 内嵌 cgo，主进程启动即加载 `mediacore.dll`，两条要求冲突。M2 采用如下修正：

- **`agent.exe`**：`CGO_ENABLED=0` 构建，纯 Go，**零 DLL 依赖、零 cgo**（主进程崩溃面不变）。
- **`worker.exe`**：`CGO_ENABLED=1` 构建，cgo 静态链接 `mediacore.dll` 导入库，仅由 agent.exe 以子进程方式拉起（`worker.exe --pipe=\\.\pipe\xxx`）。监督、重生、看门狗语义与 plan 4.1 完全一致。
- 两 exe 由同一仓库、同一 `go build` 脚本产出，部署时与 `mediacore.dll`、`tools/` 同目录。

> 若后续坚持单 exe 部署，备选方案是 worker 模式内用 `golang.org/x/sys/windows` 的 `LazyDLL` 运行时加载 `mediacore.dll`（主进程不触发加载）。该方案不走 cgo，C ABI 头文件不变，仅绑定层重写，本文不展开。

## 1. 目标与范围

### 1.1 目标（完成标志）

1. `mediacore.dll`：内存缓冲解码（JPEG/PNG/WebP/GIF/BMP 等）→ u8 灰度面 → **PDQ-256 + Quality**，位序与官方 hex 表示一致；流式 SHA-512；导出稳定 C ABI；通过官方判据回归校验（见 §6.1）。
2. Worker 进程池：agent.exe 拉起/监督 N 个 worker.exe，命名管道 + 长度前缀 msgpack IPC，崩溃检测写 `crash.log`、Worker 自动重生、单文件看门狗（图片 30s / 视频 120s）。
3. Worker"只读一次"一阶段流水线：4MB 流式读 + SHA-512；图片 ≤256MB 驻留内存交 DLL 解码；同 SHA-512 single-flight 跳过解码；字段级 `missing_mask` 剪枝。
4. 视频缩略图管线：ffprobe 取时长（15s 超时）、ffmpeg 中点帧截图（60s 超时）、缩略图按 `sha1(path)` 缓存 + mtime 校验、缩略图 PDQ-256。
5. 结果落本地库（`image_features`/`video_features`/`files`），失败字段一行一条写 `errors.log`，整轮扫描不中断。

### 1.2 不做什么（明确排除）

- **不做二阶段特征**：分区 pHash、Sobel、视频 6 帧属 M4。本里程碑仅在 ABI/表结构中预留空列（`phash_parts`/`sobel_hist` 可空列建好，避免 M4 迁移表）。
- **不做一筛与查重分析**（M3，GUI 侧）；不改动 GUI 进程（FeatureResult 透传沿用 M1 协议）。
- **不做文件枚举与盘级调度**（M1 已有）；M2 的 Pool 只提供 `Submit/Results` 通道，由 M1 调度器喂任务。
- **不做中心库上行逻辑**（M1 已有）；仅按约定向 `sync_queue` 入队。
- **不做视频内容哈希 / TMK**；视频一阶段 = 时长 + 缩略图 PDQ-256。
- **不支持** HEIC/AVIF/RAW/TIFF/PSD：解码库不覆盖的一律按 decode 失败记录（一行 `errors.log`），后续按需再扩。
- **不做跨平台**：仅 Windows x64。DLL 内 SHA-512 直接用 Windows CNG（见 §4.2）。
- 不引入新的第三方服务/中间件；依赖仅限 plan §2 已列项 + `go-winio`（命名管道）+ msgpack 库（M1 已定）。

## 2. 任务分解（checklist）

> 粒度说明：每项均可单独提交、单独验收（括号内为完成判据）。建议顺序：A → B1/B2 → C → D。

### A. mediacore.dll（C++17）

- [x] A1 vcpkg 清单 + CMake 工程骨架，`x64-windows-static` 三件套 Release 构建出 `mediacore.dll`（构建脚本一键产出 `bin/mediacore.dll`）
- [x] A2 C ABI 头文件 `mediacore.h` + `api.cpp` 骨架（版本号、errbuf 约定、异常护栏，空实现可编译导出）
- [x] A3 SHA-512（Windows CNG 流式）+ NIST 已知向量单测（3 条官方向量全对）
- [x] A4 JPEG 后端（libjpeg-turbo，内存缓冲→RGB→灰度；坏头/截断返回 `MC_ERR_DECODE` 不崩溃）
- [x] A5 PNG 后端（libpng，setjmp 错误路径；坏 CRC/截断不崩溃）
- [x] A6 WebP 后端（libwebp）
- [x] A7 stb_image 兜底后端（GIF/BMP/TGA/PNM；magic 不识别的格式走到这里）
- [x] A8 PDQ-256 移植：`pdq_upstream/` 原样拷贝 pinned commit 的 `common/downscaling/hashing` 三目录 + LICENSE + COMMIT 记录；`pdq_api.cpp` 包装（位序导出 = 官方 hex）
- [x] A9 Level A 位精确回归：`ref_luma_hasher`（上游代码构建）vs `mc_luma_runner`（本移植），≥50 个 luma 向量 hash+quality **全部位精确一致**（官方判据 1）
- [x] A10 Level B 端到端回归：`pdq/data` 官方图集 + 自采 ≥20 图，参考实现 CLI 生成 golden；**quality≥80 的样本 HD≤10**（官方判据 2），其余记录分布
- [x] A11 损坏输入鲁棒性：`mc_fuzz_corpus` 对 §6.3 全部损坏语料跑 `mc_image_phase1`，进程不崩溃、无挂死、均返回明确错误码
- [x] A12 `exports.def` + dlltool 生成 `libmediacore.a`，Go 侧链接并跑通 `mc_version()` 调用

### B. Worker 进程池（agent.exe 侧）

- [x] B1 构建脚本：一条命令产出 `bin/agent.exe`(CGO=0) + `bin/worker.exe`(CGO=1) + `bin/mediacore.dll`
- [x] B2 `messages.go`（全部 IPC 消息 + 掩码常量）+ `ipc.go`（帧编解码）+ msgpack roundtrip 单测
- [x] B3 supervisor：命名管道创建、子进程拉起、Ready 握手（10s 超时）、DLL 版本记录
- [x] B4 看门狗：按 kind 取 30s/120s，超时 Kill 且只杀一次（原子标志）
- [x] B5 崩溃归类（watchdog/exit_code/pipe_eof）→ `crash.log` 一行 → 当前文件 `status=crash` → deduper 清理 → 重生（500ms 退避）
- [x] B6 worker.exe 启动即 `SetErrorMode(SEM_FAILCRITICALERRORS|SEM_NOGPFAULTERRORBOX)`，验证崩溃无 WER 弹窗挂起
- [x] B7 池 metrics 计数（done/failed/decode_calls/thumb_gen/singleflight_hits/crashes），TaskDone 时写 `agent.log`
- [x] B8 池优雅关闭：发 `shutdown` → 3s 后强杀残留 worker

### C. 一阶段流水线（worker.exe 侧）

- [x] C1 公共读盘函数：4MB 块流式读 + CNG SHA-512，图片 ≤256MB 驻留（长路径 `\\?\` 处理）
- [x] C2 图片流水线：mask 驱动、stat 漂移检测（size/mtime 变化→全掩码重算）、sha_query→缓存命中跳过解码、未命中走 `mc_image_phase1`
- [x] C3 ffprobe 时长：`format=duration`，15s 超时，解析失败记 `MaskVideoDur` 错误
- [x] C4 ffmpeg 中点帧：`-ss` 快速定位 + 灰度缩放缩略图，60s 超时，临时文件 + 原子替换
- [x] C5 缩略图缓存：`sha1(lower(clean(abspath)))` 为键 + sidecar meta（mtime+size）校验，命中跳过 ffmpeg
- [x] C6 缩略图 PDQ-256（复用 `mc_image_phase1`）
- [x] C7 `missing_mask` 计算函数 + 整文件跳过（mask=0 不派发）单测
- [x] C8 `SavePhase1` 事务：files 更新 + 特征 upsert + `sync_queue` 入队，一事务提交；`MarkCrash`
- [x] C9 `errors.log` 接线：每个 FieldError 一行（ts/path/stage/field_mask/err/worker_pid）
- [x] C10 崩溃注入开关（`WORKER_CRASH_INJECTION` + `__crash__`/`__hang__` 路径标记）

### D. 验收

- [x] D1 测试语料生成器（有效图/视频 + 8 类损坏变体，确定性生成）
- [x] D2 AC-1 损坏文件投喂（主进程存活、errors.log 一行一条、好文件特征齐全）
- [x] D3 AC-2 崩溃注入（10 个崩溃文件：crash.log ≥10 行、池补满、扫描完成）
- [x] D4 AC-3 看门狗（挂起注入 30s 被杀、reason=watchdog_image、重生）
- [x] D5 AC-4 同 SHA single-flight（100 份副本、8 worker，`decode_calls=1`）
- [x] D6 AC-5 缩略图缓存（二轮扫描 `thumb_gen=0`；mtime 变更后重新生成）
- [x] D7 AC-6 长路径/Unicode/只读/权限拒绝文件
- [x] D8 AC-7 PDQ 两级回归门禁纳入 CI 脚本
- [x] D9 AC-8 性能烟测基线记录（1000 图 SSD，供 M6 对比）

## 3. 目录与文件结构

新增/改动文件（相对仓库根；`★`=新增，`☆`=M1 已有文件上增量修改）：

```
repo-root/
├── mediacore/                          ★ C++17 DLL 工程
│   ├── CMakeLists.txt                  ★ §4.4
│   ├── vcpkg.json                      ★ §4.4
│   ├── exports.def                     ★ §4.4（dlltool 用导出表）
│   ├── README.md                       ★ 构建步骤 + pinned commit 记录
│   ├── include/mediacore/mediacore.h   ★ §4.1 C ABI（唯一对外头文件）
│   ├── src/
│   │   ├── api.cpp                     ★ §4.2 C ABI 实现：SHA-512/解码后端/灰度/组合接口
│   │   ├── stb_impl.cpp                ★ stb_image 实现 TU + RGB 加载包装
│   │   ├── pdq_api.cpp                 ★ §4.3 PDQ 包装（hash_u8_gray）
│   │   └── pdq_upstream/               ★ 上游原样拷贝（include 路径 pdq/cpp/... 不变）
│   │       ├── COMMIT                  ★ pinned commit hash
│   │       ├── LICENSE                 ★ 上游许可证原文
│   │       └── pdq/cpp/{common,downscaling,hashing}/...
│   ├── third_party/stb/stb_image.h     ★ 单头文件
│   ├── tests/
│   │   ├── make_luma.cpp               ★ Level A 向量生成器（确定性）
│   │   ├── mc_luma_runner.cpp          ★ Level A 本移植侧 runner
│   │   ├── ref_luma_main.cpp           ★ Level A 上游侧 runner（链接上游代码）
│   │   ├── endtoend_main.cpp           ★ Level B / SHA-512 / fuzz 三合一工具
│   │   ├── run_level_a.sh              ★ Git Bash 对比脚本
│   │   └── run_level_b.sh              ★
│   └── testdata/                       ★ 生成后提交：luma/*.bin、images/*、golden/*.tsv
├── agent/
│   ├── cmd/agent/main.go               ☆ 接线：worker.Pool、OnCrash→CrashNotice、metrics
│   ├── cmd/worker/main.go              ★ worker.exe 入口（--pipe/--worker-index）
│   ├── internal/worker/                ★ agent.exe 侧：进程池
│   │   ├── messages.go                 ★ §4.6 IPC 消息 + 掩码常量（父子共用）
│   │   ├── ipc.go                      ★ §4.6 帧编解码
│   │   ├── pool.go                     ★ §4.7 Pool/Submit/Results/Metrics/关闭
│   │   ├── supervisor.go               ★ §4.7 拉起/握手/崩溃归类/重生/看门狗
│   │   ├── deduper.go                  ★ §4.9 single-flight
│   │   └── pool_test.go                ★ B2/B7 单测 + helper-process 看门狗测试
│   ├── internal/wproc/                 ★ worker.exe 侧（只有此包树依赖 cgo）
│   │   ├── run.go                      ★ §4.8 主循环/sha_reply 泵/崩溃注入开关
│   │   ├── ipc.go                      ★ §4.8 子进程侧 IPC 薄封装
│   │   ├── pipeline.go                 ★ §4.8 readAndHash 公共函数 + 图片流水线
│   │   ├── pipeline_video.go           ★ §4.8 视频流水线
│   │   ├── ffmpeg.go                   ★ §4.8 ffprobe/ffmpeg 包装
│   │   ├── thumbcache.go               ★ §4.8 缩略图缓存
│   │   ├── fixpath.go                  ★ 长路径 \\?\ 处理
│   │   ├── hooks.go                    ★ mediacore 绑定别名（版本/调试钩子）
│   │   └── mediacore/bindings.go       ★ §4.5 cgo 绑定（全仓库唯一 cgo 文件）
│   ├── internal/store/
│   │   ├── migrations/002_phase1_features.sql  ★ §4.10
│   │   ├── features.go                 ★ §4.10 FeatureStore 实现
│   │   └── mask.go                     ★ §4.10 missing_mask 计算
│   ├── internal/config/config.go       ☆ 追加 §5.2 配置项
│   ├── scripts/build.ps1               ★ 一键构建（B1）
│   ├── test/e2e_m2_test.go             ★ AC-1~AC-8 验收测试
│   └── testdata/gen_corrupt.go         ★ D1 语料生成器（go run）
└── bin/                                ★ 构建产物目录（.gitignore）
    ├── agent.exe / worker.exe / mediacore.dll / libmediacore.a
    └── tools/{ffmpeg.exe, ffprobe.exe}
```

---

## 4. 关键接口与结构体定义

### 4.1 C ABI 头文件 `mediacore/include/mediacore/mediacore.h`

设计约定：

- 所有可失败函数返回 `int` 状态码（`MC_OK=0`），并带 `errbuf`：进入即写 `\0`，出错写人读消息（供 `errors.log` 原样记录）。不用线程局部 last-error（cgo 调用可能跨线程，TLS 不可靠）。
- 跨边界内存谁分配谁释放：DLL 内分配的灰度面由 `mc_free_image` 释放；Go 侧传入的缓冲区 DLL 不保留、不释放。
- 每个导出函数包 `try/catch(...)`，C++ 异常绝不抛出 C ABI。
- `uint8_t out_hash[32]` 的位序约定见 §4.3：保证 `hex.EncodeToString(out_hash)` 与官方 `Hash256::format()` 逐字符一致。

```c
#ifndef MEDIACORE_H
#define MEDIACORE_H

#include <stddef.h>
#include <stdint.h>

#if defined(_WIN32)
#  if defined(MEDIACORE_BUILD)
#    define MC_API __declspec(dllexport)
#  else
#    define MC_API __declspec(dllimport)
#  endif
#else
#  define MC_API __attribute__((visibility("default")))
#endif

#define MC_VERSION_STRING "1.0.0"

#define MC_PDQ256_BYTES 32   /* 256 bit */
#define MC_SHA512_BYTES 64
#define MC_ERRBUF_LEN 256

#define MC_OK 0
#define MC_ERR_NULL_ARG (-1) /* 入参为空 */
#define MC_ERR_OOM (-2)      /* 内存分配失败 */
#define MC_ERR_DECODE (-3)   /* 解码失败：格式不支持或数据损坏 */
#define MC_ERR_SIZE (-4)     /* 尺寸越界（过小/过大/像素数超护栏） */
#define MC_ERR_INTERNAL (-99)

#ifdef __cplusplus
extern "C" {
#endif

MC_API const char* mc_version(void);

/* ---- SHA-512 流式（调用方按 4MB 块喂入，与 HDD 读块对齐） ---- */
typedef struct mc_sha512 mc_sha512; /* opaque */
MC_API mc_sha512* mc_sha512_new(void);
MC_API void mc_sha512_free(mc_sha512* ctx);
MC_API int mc_sha512_update(mc_sha512* ctx, const uint8_t* data, size_t len,
                            char* errbuf, size_t errbuf_len);
MC_API int mc_sha512_final(mc_sha512* ctx, uint8_t out[MC_SHA512_BYTES],
                           char* errbuf, size_t errbuf_len);

/* ---- 解码：内存缓冲 → u8 灰度面（BT.601） ---- */
typedef struct mc_image {
    int32_t width;
    int32_t height;
    uint8_t* gray; /* width*height 字节，DLL 内 malloc，mc_free_image 释放 */
} mc_image;

MC_API int mc_decode_gray(const uint8_t* buf, size_t len, mc_image* out,
                          char* errbuf, size_t errbuf_len);
MC_API void mc_free_image(mc_image* img);

/* ---- PDQ-256（官方算法移植，含 Quality 0-100） ---- */
MC_API int mc_pdq256_from_gray(const uint8_t* gray, int32_t width, int32_t height,
                               uint8_t out_hash[MC_PDQ256_BYTES], int32_t* out_quality,
                               char* errbuf, size_t errbuf_len);
MC_API int32_t mc_hamming_distance(const uint8_t a[MC_PDQ256_BYTES],
                                   const uint8_t b[MC_PDQ256_BYTES]);

/* ---- 图片一阶段组合接口：解码 + PDQ + 宽高（Worker 主路径，一次 cgo 调用） ---- */
MC_API int mc_image_phase1(const uint8_t* buf, size_t len,
                           uint8_t out_hash[MC_PDQ256_BYTES], int32_t* out_quality,
                           int32_t* out_w, int32_t* out_h,
                           char* errbuf, size_t errbuf_len);

/* ---- 测试钩子：仅供崩溃注入/看门狗验收，正常路径不得调用 ---- */
MC_API void mc_debug_crash(void);        /* 故意空指针写，制造访问违例 */
MC_API void mc_debug_sleep_ms(uint32_t ms);

#ifdef __cplusplus
}
#endif
#endif /* MEDIACORE_H */
```

### 4.2 DLL 内部实现 `mediacore/src/api.cpp`

要点：

- **SHA-512 用 Windows CNG**（`BCrypt*`）：系统组件、免移植错误、性能足够（4MB 块调用）。DLL 本就 Windows-only；若未来跨平台再替换为自实现，ABI 不变。
- **格式探测只看 magic bytes**，不信赖扩展名：JPEG `FF D8 FF`；PNG `89 50 4E 47 0D 0A 1A 0A`；WebP `RIFF....WEBP`；其余交 stb_image 兜底（GIF/BMP/TGA/PNM）；stb 也不认识 → `MC_ERR_DECODE`。
- **灰度转换**：解码统一得到 RGB8，再按整数 BT.601 `(77R+150G+29B+128)>>8` 转 u8 灰度。与官方 `fillFloatLumaFromRGB` 的浮点路径存在量化差，Level B 容差（§6.1）覆盖；灰度面定为 u8 是产品决策（M4 的 pHash/Sobel 复用同一灰度面，省内存）。
- **尺寸护栏**：短边 < 8px → `MC_ERR_SIZE`（PDQ 对图标级小图无意义，quality 必然极低）；像素总数 > 4 亿 → `MC_ERR_SIZE`（防畸形头声明超大尺寸导致巨额分配）。
- libpng 错误路径必须用 `setjmp/longjmp`（其错误回调约定），禁止在 png 回调里抛 C++ 异常。

```cpp
#include "mediacore/mediacore.h"

#include <cstdio>
#include <cstdarg>
#include <cstring>
#include <cstdlib>
#include <cstdint>
#include <vector>
#include <new>

#include <turbojpeg.h>
#include <png.h>
#include <webp/decode.h>

#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <bcrypt.h>
#pragma comment(lib, "bcrypt.lib")

/* stb_impl.cpp 提供：成功返回 RGB8 缓冲（调用方 free），失败返回 nullptr 并填 reason */
extern uint8_t* mc_stb_load_rgb(const uint8_t* buf, int len, int* w, int* h, const char** reason);

/* pdq_api.cpp 提供 */
namespace mediacore { namespace pdq {
int hash_u8_gray(const uint8_t* gray, int32_t w, int32_t h,
                 uint8_t out32[MC_PDQ256_BYTES], int32_t* quality,
                 char* errbuf, size_t errbuf_len);
}}

namespace {

const int64_t kMaxPixels = 400000000LL; /* 4 亿像素护栏 */
const int32_t kMinSide = 8;

void set_err(char* eb, size_t ebn, const char* fmt, ...) {
    if (!eb || ebn == 0) return;
    va_list ap;
    va_start(ap, fmt);
    vsnprintf(eb, ebn, fmt, ap);
    va_end(ap);
    eb[ebn - 1] = '\0';
}

/* RGB8 → u8 灰度（BT.601 整数近似）。分配结果并填 out。 */
int rgb_to_gray(const uint8_t* rgb, int w, int h, int comp,
                mc_image* out, char* eb, size_t ebn) {
    if (w < kMinSide || h < kMinSide) {
        set_err(eb, ebn, "image too small: %dx%d", w, h);
        return MC_ERR_SIZE;
    }
    int64_t px = (int64_t)w * (int64_t)h;
    if (px > kMaxPixels) {
        set_err(eb, ebn, "image too large: %dx%d", w, h);
        return MC_ERR_SIZE;
    }
    uint8_t* g = (uint8_t*)malloc((size_t)px);
    if (!g) {
        set_err(eb, ebn, "oom allocating gray plane (%lld bytes)", (long long)px);
        return MC_ERR_OOM;
    }
    for (int64_t i = 0; i < px; i++) {
        const uint8_t* p = rgb + i * comp;
        g[i] = (uint8_t)((77 * p[0] + 150 * p[1] + 29 * p[2] + 128) >> 8);
    }
    out->width = w;
    out->height = h;
    out->gray = g;
    return MC_OK;
}

int decode_jpeg(const uint8_t* buf, size_t len, mc_image* out, char* eb, size_t ebn) {
    tjhandle h = tjInitDecompress();
    if (!h) { set_err(eb, ebn, "tjInitDecompress failed"); return MC_ERR_DECODE; }
    int w = 0, hh = 0, subsamp = 0, cs = 0;
    if (tjDecompressHeader3(h, const_cast<uint8_t*>(buf), (unsigned long)len,
                            &w, &hh, &subsamp, &cs) != 0) {
        set_err(eb, ebn, "jpeg header: %s", tjGetErrorStr2(h));
        tjDestroy(h);
        return MC_ERR_DECODE;
    }
    int64_t px = (int64_t)w * (int64_t)hh;
    if (px > kMaxPixels) {
        set_err(eb, ebn, "jpeg too large: %dx%d", w, hh);
        tjDestroy(h);
        return MC_ERR_SIZE;
    }
    std::vector<uint8_t> rgb;
    try { rgb.resize((size_t)px * 3); }
    catch (...) { set_err(eb, ebn, "oom jpeg rgb"); tjDestroy(h); return MC_ERR_OOM; }
    /* 注意：不用 TJFLAG_FASTDCT，优先与参考实现像素一致性；M6 调优可再评估 */
    if (tjDecompress2(h, const_cast<uint8_t*>(buf), (unsigned long)len,
                      rgb.data(), w, 0, hh, TJPF_RGB, 0) != 0) {
        set_err(eb, ebn, "jpeg decode: %s", tjGetErrorStr2(h));
        tjDestroy(h);
        return MC_ERR_DECODE;
    }
    tjDestroy(h);
    return rgb_to_gray(rgb.data(), w, hh, 3, out, eb, ebn);
}

struct PngMem { const uint8_t* p; size_t len; size_t off; };

void png_read_fn(png_structp png, png_bytep dst, png_size_t n) {
    PngMem* m = (PngMem*)png_get_io_ptr(png);
    if (m->off + n > m->len) png_error(png, "truncated input");
    memcpy(dst, m->p + m->off, n);
    m->off += n;
}
void png_err_fn(png_structp png, png_const_charp) { longjmp(png_jmpbuf(png), 1); }
void png_warn_fn(png_structp, png_const_charp) {}

int decode_png(const uint8_t* buf, size_t len, mc_image* out, char* eb, size_t ebn) {
    png_structp png = png_create_read_struct(PNG_LIBPNG_VER_STRING, nullptr, png_err_fn, png_warn_fn);
    if (!png) { set_err(eb, ebn, "png_create_read_struct failed"); return MC_ERR_DECODE; }
    png_infop info = png_create_info_struct(png);
    if (!info) { png_destroy_read_struct(&png, nullptr, nullptr); set_err(eb, ebn, "oom png info"); return MC_ERR_OOM; }
    std::vector<uint8_t> rgba;
    std::vector<png_bytep> rows;
    if (setjmp(png_jmpbuf(png))) { /* longjmp 着陆点：一切 libpng 错误 */
        png_destroy_read_struct(&png, &info, nullptr);
        set_err(eb, ebn, "png decode error (corrupt or unsupported)");
        return MC_ERR_DECODE;
    }
    PngMem m{ buf, len, 0 };
    png_set_read_fn(png, &m, png_read_fn);
    png_read_info(png, info);
    png_uint_32 w = 0, h = 0;
    int bit_depth = 0, color_type = 0;
    png_get_IHDR(png, info, &w, &h, &bit_depth, &color_type, nullptr, nullptr, nullptr);
    if ((int64_t)w * (int64_t)h > kMaxPixels) png_error(png, "too large");
    if (bit_depth == 16) png_set_strip_16(png);
    if (color_type == PNG_COLOR_TYPE_PALETTE) png_set_palette_to_rgb(png);
    if (color_type == PNG_COLOR_TYPE_GRAY && bit_depth < 8) png_set_expand_gray_1_2_4_to_8(png);
    if (png_get_valid(png, info, PNG_INFO_tRNS)) png_set_tRNS_to_alpha(png);
    if (color_type & PNG_COLOR_MASK_ALPHA) png_set_strip_alpha(png);
    if (color_type == PNG_COLOR_TYPE_GRAY || color_type == PNG_COLOR_TYPE_GRAY_ALPHA) png_set_gray_to_rgb(png);
    png_read_update_info(png, info);
    size_t rowbytes = png_get_rowbytes(png, info);
    if (rowbytes != (size_t)w * 3) png_error(png, "unexpected rowbytes");
    try {
        rgba.resize(rowbytes * (size_t)h);
        rows.resize(h);
    } catch (...) { png_error(png, "oom"); }
    for (size_t y = 0; y < h; y++) rows[y] = rgba.data() + y * rowbytes;
    png_read_image(png, rows.data());
    png_read_end(png, nullptr);
    png_destroy_read_struct(&png, &info, nullptr);
    return rgb_to_gray(rgba.data(), (int)w, (int)h, 3, out, eb, ebn);
}

int decode_webp(const uint8_t* buf, size_t len, mc_image* out, char* eb, size_t ebn) {
    int w = 0, h = 0;
    if (!WebPGetInfo(buf, len, &w, &h)) { set_err(eb, ebn, "webp header invalid"); return MC_ERR_DECODE; }
    if ((int64_t)w * (int64_t)h > kMaxPixels) { set_err(eb, ebn, "webp too large: %dx%d", w, h); return MC_ERR_SIZE; }
    std::vector<uint8_t> rgb;
    try { rgb.resize((size_t)w * (size_t)h * 3); }
    catch (...) { set_err(eb, ebn, "oom webp rgb"); return MC_ERR_OOM; }
    if (!WebPDecodeRGBInto(buf, len, rgb.data(), (int)rgb.size(), w * 3)) {
        set_err(eb, ebn, "webp decode failed");
        return MC_ERR_DECODE;
    }
    return rgb_to_gray(rgb.data(), w, h, 3, out, eb, ebn);
}

int decode_stb(const uint8_t* buf, size_t len, mc_image* out, char* eb, size_t ebn) {
    if (len > 0x7fffffff) { set_err(eb, ebn, "buffer too large for stb"); return MC_ERR_SIZE; }
    int w = 0, h = 0;
    const char* reason = nullptr;
    uint8_t* rgb = mc_stb_load_rgb(buf, (int)len, &w, &h, &reason);
    if (!rgb) { set_err(eb, ebn, "stb: %s", reason ? reason : "unknown"); return MC_ERR_DECODE; }
    int rc = rgb_to_gray(rgb, w, h, 3, out, eb, ebn);
    free(rgb);
    return rc;
}

} /* anonymous namespace */

/* ================= C ABI ================= */

MC_API const char* mc_version(void) { return MC_VERSION_STRING; }

struct mc_sha512 {
    BCRYPT_ALG_HANDLE alg;
    BCRYPT_HASH_HANDLE h;
    std::vector<unsigned char> obj;
};

MC_API mc_sha512* mc_sha512_new(void) {
    mc_sha512* c = new (std::nothrow) mc_sha512();
    if (!c) return nullptr;
    if (BCryptOpenAlgorithmProvider(&c->alg, BCRYPT_SHA512_ALGORITHM, nullptr, 0) != 0) { delete c; return nullptr; }
    DWORD objLen = 0, cb = 0;
    if (BCryptGetProperty(c->alg, BCRYPT_OBJECT_LENGTH, (PUCHAR)&objLen, sizeof(objLen), &cb, 0) != 0) {
        BCryptCloseAlgorithmProvider(c->alg, 0); delete c; return nullptr;
    }
    try { c->obj.resize(objLen); }
    catch (...) { BCryptCloseAlgorithmProvider(c->alg, 0); delete c; return nullptr; }
    if (BCryptCreateHash(c->alg, &c->h, c->obj.data(), objLen, nullptr, 0, 0) != 0) {
        BCryptCloseAlgorithmProvider(c->alg, 0); delete c; return nullptr;
    }
    return c;
}

MC_API void mc_sha512_free(mc_sha512* ctx) {
    if (!ctx) return;
    if (ctx->h) BCryptDestroyHash(ctx->h);
    if (ctx->alg) BCryptCloseAlgorithmProvider(ctx->alg, 0);
    delete ctx;
}

MC_API int mc_sha512_update(mc_sha512* ctx, const uint8_t* data, size_t len,
                            char* errbuf, size_t errbuf_len) {
    if (errbuf && errbuf_len) errbuf[0] = '\0';
    if (!ctx || (!data && len)) { set_err(errbuf, errbuf_len, "null arg"); return MC_ERR_NULL_ARG; }
    if (len == 0) return MC_OK;
    if (len > 0xFFFFFFFF) { set_err(errbuf, errbuf_len, "chunk too large"); return MC_ERR_SIZE; }
    NTSTATUS st = BCryptHashData(ctx->h, (PUCHAR)data, (ULONG)len, 0);
    if (st != 0) { set_err(errbuf, errbuf_len, "BCryptHashData failed: 0x%lx", (unsigned long)st); return MC_ERR_INTERNAL; }
    return MC_OK;
}

MC_API int mc_sha512_final(mc_sha512* ctx, uint8_t out[MC_SHA512_BYTES],
                           char* errbuf, size_t errbuf_len) {
    if (errbuf && errbuf_len) errbuf[0] = '\0';
    if (!ctx || !out) { set_err(errbuf, errbuf_len, "null arg"); return MC_ERR_NULL_ARG; }
    NTSTATUS st = BCryptFinishHash(ctx->h, out, MC_SHA512_BYTES, 0);
    if (st != 0) { set_err(errbuf, errbuf_len, "BCryptFinishHash failed: 0x%lx", (unsigned long)st); return MC_ERR_INTERNAL; }
    return MC_OK;
}

MC_API int mc_decode_gray(const uint8_t* buf, size_t len, mc_image* out,
                          char* errbuf, size_t errbuf_len) {
    if (errbuf && errbuf_len) errbuf[0] = '\0';
    if (!buf || !out) { set_err(errbuf, errbuf_len, "null arg"); return MC_ERR_NULL_ARG; }
    out->width = 0; out->height = 0; out->gray = nullptr;
    try {
        if (len >= 3 && buf[0] == 0xFF && buf[1] == 0xD8 && buf[2] == 0xFF)
            return decode_jpeg(buf, len, out, errbuf, errbuf_len);
        if (len >= 8 && memcmp(buf, "\x89\x50\x4E\x47\x0D\x0A\x1A\x0A", 8) == 0)
            return decode_png(buf, len, out, errbuf, errbuf_len);
        if (len >= 12 && memcmp(buf, "RIFF", 4) == 0 && memcmp(buf + 8, "WEBP", 4) == 0)
            return decode_webp(buf, len, out, errbuf, errbuf_len);
        return decode_stb(buf, len, out, errbuf, errbuf_len);
    } catch (...) {
        set_err(errbuf, errbuf_len, "internal exception in decode");
        return MC_ERR_INTERNAL;
    }
}

MC_API void mc_free_image(mc_image* img) {
    if (!img) return;
    free(img->gray);
    img->gray = nullptr;
    img->width = 0;
    img->height = 0;
}

MC_API int mc_pdq256_from_gray(const uint8_t* gray, int32_t width, int32_t height,
                               uint8_t out_hash[MC_PDQ256_BYTES], int32_t* out_quality,
                               char* errbuf, size_t errbuf_len) {
    if (errbuf && errbuf_len) errbuf[0] = '\0';
    if (!gray || !out_hash || !out_quality) { set_err(errbuf, errbuf_len, "null arg"); return MC_ERR_NULL_ARG; }
    if (width < kMinSide || height < kMinSide) { set_err(errbuf, errbuf_len, "image too small: %dx%d", width, height); return MC_ERR_SIZE; }
    try {
        return mediacore::pdq::hash_u8_gray(gray, width, height, out_hash, out_quality, errbuf, errbuf_len);
    } catch (...) {
        set_err(errbuf, errbuf_len, "internal exception in pdq");
        return MC_ERR_INTERNAL;
    }
}

MC_API int32_t mc_hamming_distance(const uint8_t a[MC_PDQ256_BYTES], const uint8_t b[MC_PDQ256_BYTES]) {
    if (!a || !b) return -1;
    uint64_t wa[4], wb[4];
    memcpy(wa, a, 32);
    memcpy(wb, b, 32);
    int32_t d = 0;
    for (int i = 0; i < 4; i++) {
        uint64_t x = wa[i] ^ wb[i];
        x = x - ((x >> 1) & 0x5555555555555555ULL);
        x = (x & 0x3333333333333333ULL) + ((x >> 2) & 0x3333333333333333ULL);
        x = (x + (x >> 4)) & 0x0F0F0F0F0F0F0F0FULL;
        d += (int32_t)((x * 0x0101010101010101ULL) >> 56);
    }
    return d;
}

MC_API int mc_image_phase1(const uint8_t* buf, size_t len,
                           uint8_t out_hash[MC_PDQ256_BYTES], int32_t* out_quality,
                           int32_t* out_w, int32_t* out_h,
                           char* errbuf, size_t errbuf_len) {
    if (errbuf && errbuf_len) errbuf[0] = '\0';
    if (!buf || !out_hash || !out_quality || !out_w || !out_h) {
        set_err(errbuf, errbuf_len, "null arg");
        return MC_ERR_NULL_ARG;
    }
    mc_image img;
    int rc = mc_decode_gray(buf, len, &img, errbuf, errbuf_len);
    if (rc != MC_OK) return rc;
    *out_w = img.width;   /* 宽高在解码成功后即填，PDQ 失败也不丢 */
    *out_h = img.height;
    rc = mc_pdq256_from_gray(img.gray, img.width, img.height, out_hash, out_quality, errbuf, errbuf_len);
    mc_free_image(&img);
    return rc;
}

MC_API void mc_debug_crash(void) {
    volatile char* p = (volatile char*)nullptr;
    *p = 1; /* 故意访问违例，供崩溃注入验收 */
}

MC_API void mc_debug_sleep_ms(uint32_t ms) { Sleep(ms); }
```

`mediacore/src/stb_impl.cpp`（stb 单独编译单元，避免宏污染）：

```cpp
#define STB_IMAGE_IMPLEMENTATION
#define STBI_FAILURE_USERMSG
#define STBI_NO_STDIO          /* 只用内存接口，禁掉文件 IO 面 */
#define STBI_NO_HDR
#define STBI_NO_PIC
#define STBI_NO_PSD
#include "stb/stb_image.h"

#include <cstdint>
#include <cstdlib>
#include <cstring>

/* 成功：返回 malloc 的 RGB8 缓冲；失败：返回 nullptr 并填 reason */
uint8_t* mc_stb_load_rgb(const uint8_t* buf, int len, int* w, int* h, const char** reason) {
    int channels = 0;
    stbi_uc* img = stbi_load_from_memory(buf, len, w, h, &channels, 3);
    if (!img) {
        *reason = stbi_failure_reason();
        return nullptr;
    }
    return img; /* stbi 用 malloc 分配，调用方 free() 释放，匹配 */
}
```

### 4.3 PDQ-256 移植方案（含 Quality，官方判据回归）

**移植策略：原样拷贝，不做任何修改。** 从 `github.com/facebook/ThreatExchange` pinned commit 拷贝 `pdq/cpp/` 下的 `common/`、`downscaling/`、`hashing/` 三个目录到 `mediacore/src/pdq_upstream/pdq/cpp/`（保留目录层级，上游 `#include <pdq/cpp/...>` 路径原样可用）；**不拷** `io/`、`main/`、`test/`、`index/`（IO 层由 §4.2 解码后端替代）。将上游 `LICENSE` 与 `git rev-parse HEAD` 的 commit hash 分别存为 `pdq_upstream/LICENSE`、`pdq_upstream/COMMIT`，并在 `mediacore/README.md` 注明来源。

已核对的上游关键 API（`pdq/cpp/hashing/pdqhashing.h`，与 pinned 版一致后再动手）：

- `fillFloatLumaFromGrey(uint8_t* pbase, int numRows, int numCols, int rowStride, int colStride, float* luma)` —— 直接吃 u8 灰度面，与我们的灰度面定义天然对齐。
- `pdqHash256FromFloatLuma(float* fullBuffer1, float* fullBuffer2, int numRows, int numCols, float buffer64x64[64][64], float buffer16x64[16][64], float buffer16x16[16][16], Hash256& hash, int& quality)` —— 一次调用产出 hash + quality。
- `Hash256`（`pdq/cpp/common/pdqhashtypes.h`）：`Hash16 w[16]`，恰好 32 字节；**官方 hex 格式为 `w[15]→w[0]` 每词 `%04hx`**（见 `pdqhashtypes.cpp` 的 `format()`）。

**位序导出约定（必须遵守，否则 golden 对不上）**：导出的 32 字节 = `w[15], w[14], …, w[0]`，每词大端两字节。如此 `hex.EncodeToString(out32)` 与官方 `Hash256::format()` 逐字符相同，Go 侧可直接与 golden hex 字符串比对。`mc_hamming_distance` 与字节序无关（popcount(XOR)）。

`mediacore/src/pdq_api.cpp`：

```cpp
#include "mediacore/mediacore.h"

#include <pdq/cpp/hashing/pdqhashing.h>

#include <cstdarg>
#include <cstdio>
#include <vector>

namespace mediacore { namespace pdq {

int hash_u8_gray(const uint8_t* gray, int32_t w, int32_t h,
                 uint8_t out32[MC_PDQ256_BYTES], int32_t* quality,
                 char* errbuf, size_t errbuf_len) {
    using namespace facebook::pdq::hashing;
    const int numRows = (int)h;
    const int numCols = (int)w;
    std::vector<float> fullBuffer1((size_t)numRows * (size_t)numCols);
    std::vector<float> fullBuffer2((size_t)numRows * (size_t)numCols);
    /* u8 灰度面 → float luma：与参考实现共用同一转换函数，保证 Level A 输入一致 */
    fillFloatLumaFromGrey(const_cast<uint8_t*>(gray), numRows, numCols,
                          numCols /* rowStride */, 1 /* colStride */, fullBuffer1.data());
    float buffer64x64[64][64];
    float buffer16x64[16][64];
    float buffer16x16[16][16];
    Hash256 hash;
    int q = 0;
    pdqHash256FromFloatLuma(fullBuffer1.data(), fullBuffer2.data(), numRows, numCols,
                            buffer64x64, buffer16x64, buffer16x16, hash, q);
    /* 位序导出：w[15]→w[0]，每词大端 → hex(32B) == Hash256::format() */
    for (int i = 0; i < HASH256_NUM_WORDS; i++) {
        uint16_t v = (uint16_t)hash.w[HASH256_NUM_WORDS - 1 - i];
        out32[i * 2] = (uint8_t)(v >> 8);
        out32[i * 2 + 1] = (uint8_t)(v & 0xFF);
    }
    *quality = q;
    (void)errbuf; (void)errbuf_len;
    return MC_OK;
}

}} /* namespace mediacore::pdq */
```

算法背景（供评审，不另实现）：灰度 luma → Jarosz 盒滤波降采样到 64×64 → `pdqImageDomainQualityMetric` 出 Quality（0-100）→ 64×64 DCT 取左上 16×16 低频系数 → 以中位数为阈值量化出 256 bit。上游参考阈值（README）：相似判定距离 ≤31（与 plan T1=31 一致），quality ≤49 的哈希建议丢弃（M3 剪枝用）。

### 4.4 构建与链接（vcpkg + CMake + dlltool）

`mediacore/vcpkg.json`（manifest 模式，锁依赖）：

```json
{
  "name": "mediacore",
  "version-string": "1.0.0",
  "dependencies": [
    "libjpeg-turbo",
    "libpng",
    "libwebp"
  ]
}
```

`mediacore/CMakeLists.txt`：

```cmake
cmake_minimum_required(VERSION 3.20)
project(mediacore LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

find_package(libjpeg-turbo CONFIG REQUIRED)
find_package(PNG REQUIRED)
find_package(WebP CONFIG REQUIRED)

file(GLOB PDQ_SOURCES CONFIGURE_DEPENDS
    "${CMAKE_CURRENT_SOURCE_DIR}/src/pdq_upstream/pdq/cpp/common/*.cpp"
    "${CMAKE_CURRENT_SOURCE_DIR}/src/pdq_upstream/pdq/cpp/downscaling/*.cpp"
    "${CMAKE_CURRENT_SOURCE_DIR}/src/pdq_upstream/pdq/cpp/hashing/*.cpp")

add_library(mediacore SHARED
    src/api.cpp
    src/pdq_api.cpp
    src/stb_impl.cpp
    ${PDQ_SOURCES})

target_include_directories(mediacore PRIVATE
    include
    src/pdq_upstream   # 上游 #include <pdq/cpp/...> 路径根
    third_party)       # stb

target_compile_definitions(mediacore PRIVATE MEDIACORE_BUILD NOMINMAX WIN32_LEAN_AND_MEAN)
target_link_libraries(mediacore PRIVATE
    libjpeg-turbo::turbojpeg
    PNG::PNG
    WebP::webp
    bcrypt)
set_target_properties(mediacore PROPERTIES OUTPUT_NAME "mediacore")

option(MEDIACORE_BUILD_TESTS "build test tools" ON)
if(MEDIACORE_BUILD_TESTS)
    add_executable(mc_luma_runner tests/mc_luma_runner.cpp)
    target_include_directories(mc_luma_runner PRIVATE include)
    target_link_libraries(mc_luma_runner PRIVATE mediacore)

    add_executable(mc_endtoend tests/endtoend_main.cpp)
    target_include_directories(mc_endtoend PRIVATE include)
    target_link_libraries(mc_endtoend PRIVATE mediacore)

    add_executable(mc_make_luma tests/make_luma.cpp)
endif()
```

`mediacore/exports.def`（供 dlltool 生成 MinGW 导入库；导出集与 `mediacore.h` 一一对应，新增导出必须同步）：

```
LIBRARY mediacore
EXPORTS
    mc_version
    mc_sha512_new
    mc_sha512_free
    mc_sha512_update
    mc_sha512_final
    mc_decode_gray
    mc_free_image
    mc_pdq256_from_gray
    mc_hamming_distance
    mc_image_phase1
    mc_debug_crash
    mc_debug_sleep_ms
```

构建步骤（Git Bash / PowerShell 均可，`VCPKG_ROOT` 已设置；产出归集到仓库 `bin/`）：

```bash
# 1. DLL（静态运行时三件套，产物除 bcrypt 外无外部 DLL 依赖，免 DLL hell）
cmake -B mediacore/build -S mediacore \
  -DCMAKE_TOOLCHAIN_FILE="$VCPKG_ROOT/scripts/buildsystems/vcpkg.cmake" \
  -DVCPKG_TARGET_TRIPLET=x64-windows-static -A x64
cmake --build mediacore/build --config Release
cp mediacore/build/Release/mediacore.dll bin/

# 2. MinGW 导入库（Go cgo 用 GNU ld，需 .a 导入库；dlltool 来自 mingw-w64）
dlltool -d mediacore/exports.def -l bin/libmediacore.a -D mediacore.dll

# 3. Go 双二进制（B1）
CGO_ENABLED=0 go build -o bin/agent.exe  ./agent/cmd/agent
CGO_ENABLED=1 go build -o bin/worker.exe ./agent/cmd/worker
```

> 备选：GNU ld 支持直接链接 DLL（`-l:mediacore.dll`），可跳过 dlltool；但显式 `.def + dlltool` 在 CI 上更确定，本文以其为准。

### 4.5 cgo 绑定 `agent/internal/wproc/mediacore/bindings.go`

全仓库唯一 cgo 文件，只在 worker.exe 编译单元内。所有调用同步、不跨调用保留 Go 指针（满足 cgo pointer 规则）。

```go
// Package mediacore 是 mediacore.dll 的 cgo 绑定。仅 worker.exe 使用。
package mediacore

/*
#cgo CFLAGS:  -I${SRCDIR}/../../../mediacore/include
#cgo windows LDFLAGS: -L${SRCDIR}/../../../bin -lmediacore
#include <stdlib.h>
#include "mediacore/mediacore.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

const (
	PDQ256Bytes = 32
	SHA512Bytes = 64
	errBufLen   = 256
)

// DecodeError 对应 MC_ERR_* 状态码。
type DecodeError struct {
	Code int
	Msg  string
}

func (e *DecodeError) Error() string { return fmt.Sprintf("mediacore(%d): %s", e.Code, e.Msg) }

func errFrom(code C.int, eb *C.char) error {
	switch int(code) {
	case 0:
		return nil
	default:
		return &DecodeError{Code: int(code), Msg: C.GoString(eb)}
	}
}

// Version 返回 DLL 版本，用于 Ready 握手登记。
func Version() string { return C.GoString(C.mc_version()) }

// SHA512 流式哈希（4MB 块喂入）。
type SHA512 struct{ ctx *C.mc_sha512 }

func NewSHA512() (*SHA512, error) {
	c := C.mc_sha512_new()
	if c == nil {
		return nil, errors.New("mediacore: mc_sha512_new failed")
	}
	return &SHA512{ctx: c}, nil
}

func (s *SHA512) Update(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	var eb [errBufLen]C.char
	rc := C.mc_sha512_update(s.ctx, (*C.uint8_t)(unsafe.Pointer(&p[0])),
		C.size_t(len(p)), &eb[0], C.size_t(errBufLen))
	return errFrom(rc, &eb[0])
}

func (s *SHA512) Final() ([SHA512Bytes]byte, error) {
	var out [SHA512Bytes]byte
	var eb [errBufLen]C.char
	rc := C.mc_sha512_final(s.ctx, (*C.uint8_t)(unsafe.Pointer(&out[0])),
		&eb[0], C.size_t(errBufLen))
	return out, errFrom(rc, &eb[0])
}

func (s *SHA512) Close() {
	if s.ctx != nil {
		C.mc_sha512_free(s.ctx)
		s.ctx = nil
	}
}

// Phase1Result 是图片一阶段（解码 + PDQ + 宽高）的结果。
type Phase1Result struct {
	Hash    [PDQ256Bytes]byte
	Quality int32
	Width   int32
	Height  int32
}

// ImagePhase1 对内存中的图片字节做解码 + PDQ-256。小图/坏图返回 *DecodeError。
func ImagePhase1(buf []byte) (Phase1Result, error) {
	var r Phase1Result
	if len(buf) == 0 {
		return r, &DecodeError{Code: -1, Msg: "empty buffer"}
	}
	var eb [errBufLen]C.char
	var q, w, h C.int32_t
	rc := C.mc_image_phase1(
		(*C.uint8_t)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)),
		(*C.uint8_t)(unsafe.Pointer(&r.Hash[0])), &q, &w, &h,
		&eb[0], C.size_t(errBufLen))
	if err := errFrom(rc, &eb[0]); err != nil {
		return r, err
	}
	r.Quality, r.Width, r.Height = int32(q), int32(w), int32(h)
	return r, nil
}

// HammingDistance 计算两个 PDQ-256 的汉明距离（验收/排障用；一筛在 GUI 侧用 SQL 算）。
func HammingDistance(a, b [PDQ256Bytes]byte) int {
	return int(C.mc_hamming_distance(
		(*C.uint8_t)(unsafe.Pointer(&a[0])),
		(*C.uint8_t)(unsafe.Pointer(&b[0]))))
}

// DebugCrash 故意令进程访问违例（仅崩溃注入验收用）。
func DebugCrash() { C.mc_debug_crash() }

// DebugSleepMS 在 DLL 内睡眠（仅看门狗验收用）。
func DebugSleepMS(ms uint32) { C.mc_debug_sleep_ms(C.uint32_t(ms)) }
```

### 4.6 IPC 消息定义（命名管道 + 长度前缀 msgpack）

帧格式与 plan §7 的 TCP 协议一致：`[4B 大端长度][msgpack body]`，复用 M1 `internal/proto` 的帧编解码；下述 `ipc.go` 是管道场景的薄封装（带 16MB 帧长护栏）。msgpack 库与 M1 一致（示例用 `github.com/vmihailenco/msgpack/v5`；若 M1 已定其他库，以 M1 为准，仅换 tag 写法）。

会话流程：`worker dial → Ready → (父)Job → (子)sha_query? → (父)sha_reply → (子)result → 下一 Job … → shutdown`。父进程对每个 worker 同一时刻只发一个 Job；worker 在 `sha_query` 后阻塞等 `sha_reply`（期间父进程不会发新 Job）。

`agent/internal/worker/messages.go`（父子两侧共用）：

```go
package worker

// ---- 信封类型 ----
const (
	MsgReady    = "ready"     // 子→父：握手
	MsgJob      = "job"       // 父→子：计算任务
	MsgShutdown = "shutdown"  // 父→子：优雅退出
	MsgShaQuery = "sha_query" // 子→父：single-flight 特征查询
	MsgShaReply = "sha_reply" // 父→子：特征查询应答
	MsgResult   = "result"    // 子→父：任务结果
)

// Envelope 是所有 IPC 帧的外层信封；Body 按 Type 二次解码。
type Envelope struct {
	Type string `msgpack:"type"`
	Body []byte `msgpack:"body"` // 内层消息的 msgpack 字节
}

// ---- 媒体类别 / 阶段 ----
type MediaKind int8

const (
	MediaImage MediaKind = 1
	MediaVideo MediaKind = 2
)

func KindName(k MediaKind) string {
	if k == MediaVideo {
		return "video"
	}
	return "image"
}

type Phase int8

const (
	Phase1 Phase = 1
	Phase2 Phase = 2 // M4 使用，M2 仅保留枚举
)

// ---- 字段级缺失掩码（bit=1 表示该字段缺失、需补算）----
const (
	MaskSHA512     uint32 = 1 << 0 // sha512
	MaskImagePDQ   uint32 = 1 << 1 // 图片 pdq256+quality+width+height
	MaskVideoDur   uint32 = 1 << 2 // 视频 duration_ms
	MaskVideoThumb uint32 = 1 << 3 // 视频 thumb_path+thumb_pdq256+thumb_quality

	MaskAllImage = MaskSHA512 | MaskImagePDQ
	MaskAllVideo = MaskSHA512 | MaskVideoDur | MaskVideoThumb
)

// ---- 消息体 ----

type ReadyMsg struct {
	PID         int    `msgpack:"pid"`
	WorkerIndex int    `msgpack:"worker_index"`
	DLLVersion  string `msgpack:"dll_version"`
}

type JobMsg struct {
	JobID      int64     `msgpack:"job_id"`
	ScanTaskID int64     `msgpack:"scan_task_id"`
	Path       string    `msgpack:"path"`
	Kind       MediaKind `msgpack:"kind"`
	Phase      Phase     `msgpack:"phase"`
	FieldsMask uint32    `msgpack:"fields_mask"` // 见 Mask*；1=需补算
	Size       int64     `msgpack:"size"`
	MTimeUnix  int64     `msgpack:"mtime_unix"`
	KnownSHA   []byte    `msgpack:"known_sha,omitempty"` // size+mtime 未变时库内已有 sha512（64B）
}

type ShaQueryMsg struct {
	JobID  int64     `msgpack:"job_id"`
	SHA512 []byte    `msgpack:"sha512"`
	Kind   MediaKind `msgpack:"kind"`
}

// ShaReplyMsg：Found=false 表示无缓存，需自行解码；字段按 Kind 取用。
type ShaReplyMsg struct {
	JobID int64 `msgpack:"job_id"`
	Found bool  `msgpack:"found"`
	// image
	PDQ     []byte `msgpack:"pdq,omitempty"`
	Quality int32  `msgpack:"quality,omitempty"`
	Width   int32  `msgpack:"width,omitempty"`
	Height  int32  `msgpack:"height,omitempty"`
	// video
	DurationMS   int64  `msgpack:"duration_ms,omitempty"`
	ThumbPath    string `msgpack:"thumb_path,omitempty"`
	ThumbPDQ     []byte `msgpack:"thumb_pdq,omitempty"`
	ThumbQuality int32  `msgpack:"thumb_quality,omitempty"`
}

// FieldError 记录单个字段的失败（errors.log 一行一条的来源）。
type FieldError struct {
	Field uint32 `msgpack:"field"` // 失败的 Mask* 位
	Stage string `msgpack:"stage"` // stat/open/read/sha512/decode/ffprobe/ffmpeg/thumb_pdq
	Msg   string `msgpack:"msg"`
}

type JobResultMsg struct {
	JobID      int64      `msgpack:"job_id"`
	Path       string     `msgpack:"path"`
	Kind       MediaKind  `msgpack:"kind"`
	SHA512     []byte     `msgpack:"sha512,omitempty"`
	FieldsDone uint32     `msgpack:"fields_done"` // 本次完成的 Mask* 位
	PDQ        []byte     `msgpack:"pdq,omitempty"`
	Quality    int32      `msgpack:"quality,omitempty"`
	Width      int32      `msgpack:"width,omitempty"`
	Height     int32      `msgpack:"height,omitempty"`
	DurationMS   int64    `msgpack:"duration_ms,omitempty"`
	ThumbPath    string   `msgpack:"thumb_path,omitempty"`
	ThumbPDQ     []byte   `msgpack:"thumb_pdq,omitempty"`
	ThumbQuality int32    `msgpack:"thumb_quality,omitempty"`
	Errors       []FieldError `msgpack:"errors,omitempty"`
	// 统计（池 metrics 与验收用）
	ReadMS         int64 `msgpack:"read_ms,omitempty"`
	DecodeMS       int64 `msgpack:"decode_ms,omitempty"`
	ThumbMS        int64 `msgpack:"thumb_ms,omitempty"`
	Decoded        bool  `msgpack:"decoded,omitempty"`         // 本次是否真的调用 DLL 解码
	ThumbGenerated bool  `msgpack:"thumb_generated,omitempty"` // 本次是否真的跑了 ffmpeg
	ThumbCacheHit  bool  `msgpack:"thumb_cache_hit,omitempty"`
}
```

`agent/internal/worker/ipc.go`：

```go
package worker

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"github.com/vmihailenco/msgpack/v5"
)

// MaxFrameBytes 帧长护栏（结果消息远小于此，防对端异常撑爆内存）。
const MaxFrameBytes = 16 << 20

// IPCConn 封装一条命名管道连接，父子两侧共用；写侧加锁（读循环与派发 goroutine 可能并发写）。
type IPCConn struct {
	r  io.Reader
	w  *bufio.Writer
	wm sync.Mutex
}

func NewIPCConn(rwc io.ReadWriter) *IPCConn {
	return &IPCConn{r: bufio.NewReaderSize(rwc, 64<<10), w: bufio.NewWriterSize(rwc, 64<<10)}
}

// WriteEnv 发送一帧：先 marshal 内层 body，再包信封。
func (c *IPCConn) WriteEnv(msgType string, body interface{}) error {
	bb, err := msgpack.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body %s: %w", msgType, err)
	}
	payload, err := msgpack.Marshal(&Envelope{Type: msgType, Body: bb})
	if err != nil {
		return fmt.Errorf("marshal envelope %s: %w", msgType, err)
	}
	if len(payload) > MaxFrameBytes {
		return fmt.Errorf("frame too large: %d", len(payload))
	}
	c.wm.Lock()
	defer c.wm.Unlock()
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := c.w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := c.w.Write(payload); err != nil {
		return err
	}
	return c.w.Flush()
}

// ReadEnv 读取一帧；io.EOF 表示对端关闭（父侧视为 worker 死亡信号之一）。
func (c *IPCConn) ReadEnv() (*Envelope, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > MaxFrameBytes {
		return nil, fmt.Errorf("bad frame length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return nil, err
	}
	var env Envelope
	if err := msgpack.Unmarshal(buf, &env); err != nil {
		return nil, fmt.Errorf("unmarshal envelope: %w", err)
	}
	return &env, nil
}

// DecodeBodyFor 解出内层消息。
func DecodeBodyFor(env *Envelope, out interface{}) error {
	if err := msgpack.Unmarshal(env.Body, out); err != nil {
		return fmt.Errorf("unmarshal body %s: %w", env.Type, err)
	}
	return nil
}
```

### 4.7 Worker 池核心（agent.exe 侧）

职责与语义（与 plan 4.1 对齐）：

- N = `worker.count`（默认 NumCPU）个常驻 worker.exe；管道名含父 PID + 序号 + 纳秒 nonce，防碰撞。
- 崩溃检测三通道：**管道读 EOF / 非零退出码 / 看门狗超时**；任一触发 → `crash.log` 一行（ts、pid、worker_index、file、exit_code、reason）→ 当前文件 `status=crash`（本轮不再派发，下轮由 missing_mask 自动补算）→ deduper 清理其 single-flight 占有 → **重生**（500ms 退避防狂刷）。
- 看门狗：派发即启动 `time.AfterFunc`（图片 30s / 视频 120s，可配），超时 `Process.Kill()`；用 `atomic.CompareAndSwap` 保证"杀"与"归类"只发生一次。
- 结果处理顺序固定：`SavePhase1` 落库 → `dedup.Resolve` 唤醒等待者 → `errors.log` 逐条 → 转发 `Results` 通道（M1 主循环做进度统计与 GUI `FeatureResult` 透传）。

`agent/internal/worker/pool.go`：

```go
package worker

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Config 进程池配置（值来自 §5.2 配置表）。
type Config struct {
	WorkerExe    string        // worker.exe 绝对路径
	WorkerCount  int           // = CPU 核数
	ImageTimeout time.Duration // 默认 30s
	VideoTimeout time.Duration // 默认 120s
	RespawnDelay time.Duration // 默认 500ms
	WorkerEnv    []string      // 注入 worker 的 WPROC_* 环境变量（由 main 按 §5.2 配置组装）
}

// FeatureStore 由 internal/store 实现（§4.10）。
type FeatureStore interface {
	LookupImage(sha []byte) (*ImageFeature, error)
	LookupVideo(sha []byte) (*VideoFeature, error)
	SavePhase1(res *JobResultMsg) error
	MarkCrash(path string, errMsg string) error
}

// Metrics 池级计数（验收用；TaskDone 时汇总写 agent.log）。
type Metrics struct {
	FilesDone        atomic.Int64
	FilesFailed      atomic.Int64
	DecodeCalls      atomic.Int64 // 真实 DLL 解码次数（AC-4 判据）
	ThumbGenerated   atomic.Int64 // 真实 ffmpeg 截图次数（AC-5 判据）
	ThumbCacheHits   atomic.Int64
	SingleFlightHits atomic.Int64
	Crashes          atomic.Int64
}

// ImageFeature / VideoFeature 与 §4.10 表结构对应。
type ImageFeature struct {
	SHA512   []byte
	Width    int32
	Height   int32
	PDQ      []byte // nil = 未算/失败
	Quality  int32
}

type VideoFeature struct {
	SHA512       []byte
	DurationMS   *int64 // 指针可空：区分 0 与未知
	ThumbPath    string
	ThumbPDQ     []byte
	ThumbQuality int32
}

type Pool struct {
	cfg      Config
	store    FeatureStore
	log      *slog.Logger // agent.log
	errLog   *slog.Logger // errors.log
	crashLog *slog.Logger // crash.log
	dedup    *Deduper
	Metrics  Metrics

	// OnCrash 由 main 接线：转发 GUI CrashNotice（plan §7）；可为 nil。
	OnCrash func(pid int, job *JobMsg, exitCode int32, reason string)

	jobs    chan *JobMsg
	results chan *JobResultMsg
	free    chan *workerProc
	quit    chan struct{}
	once    sync.Once
	wg      sync.WaitGroup

	activeMu sync.Mutex
	active   map[int]*workerProc // index → 当前活着的 worker（关闭时强杀用）
}

func NewPool(cfg Config, store FeatureStore, log, errLog, crashLog *slog.Logger) *Pool {
	return &Pool{
		cfg:      cfg,
		store:    store,
		log:      log,
		errLog:   errLog,
		crashLog: crashLog,
		dedup:    NewDeduper(store),
		jobs:     make(chan *JobMsg, 1024),
		results:  make(chan *JobResultMsg, 1024),
		free:     make(chan *workerProc, cfg.WorkerCount),
		quit:     make(chan struct{}),
		active:   make(map[int]*workerProc),
	}
}

func (p *Pool) Start() {
	for i := 0; i < p.cfg.WorkerCount; i++ {
		p.wg.Add(1)
		go p.supervise(i)
	}
	p.wg.Add(1)
	go p.dispatchLoop()
}

// Submit 投递一个计算任务（M1 调度器调用；背压由 channel 容量 + M1 调度器承担）。
func (p *Pool) Submit(j *JobMsg) { p.jobs <- j }

// Results 结果流（M1 主循环消费：进度统计 + FeatureResult 透传 GUI）。
func (p *Pool) Results() <-chan *JobResultMsg { return p.results }

// Close 优雅关闭：杀全部 worker（worker 无状态，直接 Kill），等监督 goroutine 退出。
func (p *Pool) Close() {
	p.once.Do(func() {
		close(p.quit)
		p.activeMu.Lock()
		for _, w := range p.active {
			if w.cmd != nil && w.cmd.Process != nil {
				_ = w.cmd.Process.Kill()
			}
		}
		p.activeMu.Unlock()
		p.wg.Wait()
	})
}

// dispatchLoop：从 jobs 取任务 → 等空闲 worker → 下发 + 启动看门狗。
func (p *Pool) dispatchLoop() {
	defer p.wg.Done()
	for {
		select {
		case <-p.quit:
			return
		case job := <-p.jobs:
			var w *workerProc
			select {
			case <-p.quit:
				return
			case w = <-p.free:
			}
			w.mu.Lock()
			w.cur = job
			w.mu.Unlock()
			timeout := p.cfg.ImageTimeout
			if job.Kind == MediaVideo {
				timeout = p.cfg.VideoTimeout
			}
			w.timer = time.AfterFunc(timeout, func() {
				if atomic.CompareAndSwapInt32(&w.dead, 0, 1) {
					w.mu.Lock()
					w.reason = "watchdog_" + KindName(job.Kind)
					w.mu.Unlock()
					p.log.Warn("watchdog kill worker",
						"worker_index", w.index, "path", job.Path, "timeout", timeout.String())
					if w.cmd != nil && w.cmd.Process != nil {
						_ = w.cmd.Process.Kill()
					}
				}
			})
			if err := w.conn.WriteEnv(MsgJob, job); err != nil {
				w.timer.Stop()
				if atomic.CompareAndSwapInt32(&w.dead, 0, 1) {
					w.mu.Lock()
					w.reason = "pipe_write"
					w.mu.Unlock()
					if w.cmd != nil && w.cmd.Process != nil {
						_ = w.cmd.Process.Kill()
					}
				}
			}
		}
	}
}

// handleEnvelope 处理来自 worker 的消息（在每个 worker 的读循环 goroutine 内运行）。
func (p *Pool) handleEnvelope(w *workerProc, env *Envelope) {
	switch env.Type {
	case MsgResult:
		var res JobResultMsg
		if err := DecodeBodyFor(env, &res); err != nil {
			p.log.Error("bad result msg", "err", err)
			return
		}
		if w.timer != nil {
			w.timer.Stop()
		}
		w.mu.Lock()
		w.cur = nil
		w.mu.Unlock()
		// 1) 落库（含 files/features/sync_queue，一事务）
		if err := p.store.SavePhase1(&res); err != nil {
			p.log.Error("save phase1 failed", "path", res.Path, "err", err)
		}
		// 2) 唤醒 single-flight 等待者
		p.dedup.Resolve(&res)
		// 3) errors.log：每失败字段一行
		for _, fe := range res.Errors {
			p.errLog.Error("file error",
				"path", res.Path, "stage", fe.Stage, "field_mask", fe.Field,
				"err", fe.Msg, "worker_pid", pidOf(w))
		}
		// 4) metrics
		if len(res.Errors) > 0 {
			p.Metrics.FilesFailed.Add(1)
		} else {
			p.Metrics.FilesDone.Add(1)
		}
		if res.Decoded {
			p.Metrics.DecodeCalls.Add(1)
		}
		if res.ThumbGenerated {
			p.Metrics.ThumbGenerated.Add(1)
		}
		if res.ThumbCacheHit {
			p.Metrics.ThumbCacheHits.Add(1)
		}
		select {
		case p.results <- &res:
		case <-p.quit:
		}
		// 5) worker 回到空闲队列
		select {
		case p.free <- w:
		case <-p.quit:
		}
	case MsgShaQuery:
		var q ShaQueryMsg
		if err := DecodeBodyFor(env, &q); err != nil {
			p.log.Error("bad sha_query msg", "err", err)
			return
		}
		// dedup.Ask 可能阻塞等待同 SHA 的首问者算完；该 worker 反正也在等回复，无死锁。
		rep := p.dedup.Ask(&q)
		if rep.Found {
			p.Metrics.SingleFlightHits.Add(1)
		}
		if err := w.conn.WriteEnv(MsgShaReply, rep); err != nil {
			if atomic.CompareAndSwapInt32(&w.dead, 0, 1) {
				w.mu.Lock()
				w.reason = "pipe_write"
				w.mu.Unlock()
				if w.cmd != nil && w.cmd.Process != nil {
					_ = w.cmd.Process.Kill()
				}
			}
		}
	default:
		p.log.Warn("unexpected msg from worker", "type", env.Type, "worker_index", w.index)
	}
}

func pidOf(w *workerProc) int {
	if w.cmd != nil && w.cmd.Process != nil {
		return w.cmd.Process.Pid
	}
	return 0
}

func sleepOrQuit(quit <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-quit:
		return false
	case <-t.C:
		return true
	}
}
```

`agent/internal/worker/supervisor.go`：

```go
package worker

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Microsoft/go-winio"
)

// workerProc 一个受监督的 worker 子进程实例。
type workerProc struct {
	index  int
	cmd    *exec.Cmd
	ln     net.Listener
	conn   *IPCConn
	timer  *time.Timer
	exitCh chan exitInfo

	mu     sync.Mutex
	cur    *JobMsg // 当前在算任务（nil=空闲）
	reason string  // 崩溃原因（watchdog_* / pipe_write 预设；否则按退出码归类）
	dead   int32   // 原子标志：保证 kill/归类只发生一次
}

type exitInfo struct {
	err      error
	exitCode int
}

// supervise 是每个 worker 槽位的监督循环：拉起 → 服务 → 崩溃善后 → 退避重生。
func (p *Pool) supervise(index int) {
	defer p.wg.Done()
	for {
		select {
		case <-p.quit:
			return
		default:
		}
		w, err := p.spawnWorker(index)
		if err != nil {
			p.log.Error("worker spawn failed", "worker_index", index, "err", err)
			if !sleepOrQuit(p.quit, p.cfg.RespawnDelay) {
				return
			}
			continue
		}
		p.activeMu.Lock()
		p.active[index] = w
		p.activeMu.Unlock()

		p.runWorker(w) // 阻塞至 worker 死亡并完成善后

		p.activeMu.Lock()
		delete(p.active, index)
		p.activeMu.Unlock()
		if !sleepOrQuit(p.quit, p.cfg.RespawnDelay) {
			return
		}
	}
}

// spawnWorker 建管、拉进程、等连接、Ready 握手。
func (p *Pool) spawnWorker(index int) (*workerProc, error) {
	pipeName := fmt.Sprintf(`\\.\pipe\mediadedup-w%d-%d-%d`, os.Getpid(), index, time.Now().UnixNano())
	ln, err := winio.ListenPipe(pipeName, &winio.PipeConfig{
		InputBufferSize:  64 << 10,
		OutputBufferSize: 64 << 10,
	})
	if err != nil {
		return nil, fmt.Errorf("listen pipe: %w", err)
	}
	cmd := exec.Command(p.cfg.WorkerExe,
		"--pipe", pipeName, "--worker-index", strconv.Itoa(index))
	cmd.Stdout = os.Stdout // worker 不写业务日志；透传便于调试
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), p.cfg.WorkerEnv...) // 注入 WPROC_*（含验收期 WORKER_CRASH_INJECTION）
	if err := cmd.Start(); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("start worker.exe: %w", err)
	}
	w := &workerProc{index: index, cmd: cmd, ln: ln, exitCh: make(chan exitInfo, 1)}
	fail := func(e error) (*workerProc, error) {
		_ = cmd.Process.Kill()
		_, _ = w.waitExit(3 * time.Second)
		_ = ln.Close()
		return nil, e
	}
	// 等 worker dial（10s 超时）
	type accRes struct {
		c   net.Conn
		err error
	}
	accCh := make(chan accRes, 1)
	go func() {
		c, err := ln.Accept()
		accCh <- accRes{c, err}
	}()
	var nc net.Conn
	select {
	case ar := <-accCh:
		if ar.err != nil {
			return fail(fmt.Errorf("accept: %w", ar.err))
		}
		nc = ar.c
	case <-time.After(10 * time.Second):
		return fail(errors.New("worker connect timeout"))
	}
	w.conn = NewIPCConn(nc)
	// 进程退出监控（与读循环并列）
	go func() {
		err := cmd.Wait()
		ec := 0
		var ee *exec.ExitError
		switch {
		case errors.As(err, &ee):
			ec = ee.ExitCode()
		case err != nil:
			ec = -1
		}
		w.exitCh <- exitInfo{err: err, exitCode: ec}
	}()
	// Ready 握手（10s）
	_ = nc.SetReadDeadline(time.Now().Add(10 * time.Second))
	env, err := w.conn.ReadEnv()
	_ = nc.SetReadDeadline(time.Time{})
	if err != nil {
		return fail(fmt.Errorf("read ready: %w", err))
	}
	if env.Type != MsgReady {
		return fail(fmt.Errorf("expect ready, got %s", env.Type))
	}
	var ready ReadyMsg
	if err := DecodeBodyFor(env, &ready); err != nil {
		return fail(err)
	}
	p.log.Info("worker ready",
		"worker_index", index, "pid", ready.PID, "dll_version", ready.DLLVersion)
	// 新 worker 立即可用
	select {
	case p.free <- w:
	case <-p.quit:
		return fail(errors.New("pool closing"))
	}
	return w, nil
}

func (w *workerProc) waitExit(d time.Duration) (exitInfo, bool) {
	select {
	case ei := <-w.exitCh:
		return ei, true
	case <-time.After(d):
		return exitInfo{exitCode: -1}, false
	}
}

// runWorker 启动读循环并阻塞至 worker 死亡，然后做崩溃善后（plan 4.1 语义）。
func (p *Pool) runWorker(w *workerProc) {
	readErr := make(chan error, 1)
	go func() {
		for {
			env, err := w.conn.ReadEnv()
			if err != nil {
				readErr <- err
				return
			}
			p.handleEnvelope(w, env)
		}
	}()
	// 进程退出是主信号（看门狗 Kill 也走这里）；管道错误为辅
	exit := <-w.exitCh
	select {
	case <-readErr:
	case <-time.After(2 * time.Second):
	}
	// 归类崩溃原因
	w.mu.Lock()
	reason := w.reason
	cur := w.cur
	w.mu.Unlock()
	if reason == "" {
		if exit.exitCode != 0 {
			reason = "exit_code"
		} else {
			reason = "pipe_eof"
		}
	}
	file := ""
	if cur != nil {
		file = cur.Path
	}
	p.crashLog.Error("worker crashed",
		"pid", pidOf(w), "worker_index", w.index, "file", file,
		"exit_code", exit.exitCode, "reason", reason)
	p.Metrics.Crashes.Add(1)
	if cur != nil {
		// 当前文件标记 crash（本轮不再派发；下轮 missing_mask 自动补算）
		if err := p.store.MarkCrash(cur.Path, "worker crash: "+reason); err != nil {
			p.log.Error("mark crash failed", "path", cur.Path, "err", err)
		}
		// 释放其 single-flight 占有，唤醒等待者重试
		p.dedup.FailByJob(cur.JobID)
		if p.OnCrash != nil {
			p.OnCrash(pidOf(w), cur, int32(exit.exitCode), reason)
		}
	}
	_ = w.ln.Close()
}
```

### 4.8 Worker 子进程主流水线（worker.exe 侧）

入口 `agent/cmd/worker/main.go`：

```go
// worker.exe：Agent 的可牺牲计算子进程。唯一 cgo 二进制。
package main

import (
	"flag"
	"fmt"
	"os"

	"mediadedup/agent/internal/wproc"
)

func main() {
	pipe := flag.String("pipe", "", `命名管道全路径 \\.\pipe\...`)
	index := flag.Int("worker-index", -1, "worker 槽位序号")
	flag.Parse()
	if *pipe == "" {
		fmt.Fprintln(os.Stderr, "worker: --pipe required")
		os.Exit(2)
	}
	os.Exit(wproc.Run(*pipe, *index))
}
```

`agent/internal/wproc/run.go`（主循环 + 崩溃注入开关）：

```go
// Package wproc 是 worker.exe 的全部逻辑：IPC 主循环 + 一阶段流水线。
package wproc

import (
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"mediadedup/agent/internal/worker"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// Config 流水线配置（由环境变量注入，父进程在 spawn 时设置；见 §5.2）。
type Config struct {
	ReadChunkBytes   int           // 4MB
	ImageMemBytes    int64         // 256MB
	FFprobePath      string
	FFmpegPath       string
	FFprobeTimeout   time.Duration // 15s
	FFmpegTimeout    time.Duration // 60s
	ThumbCacheDir    string
	ThumbMaxSide     int // 256
	CrashInjection   bool
}

func ConfigFromEnv() Config {
	return Config{
		ReadChunkBytes: envInt("WPROC_READ_CHUNK_KB", 4096) << 10,
		ImageMemBytes:  int64(envInt("WPROC_IMAGE_MEM_MB", 256)) << 20,
		FFprobePath:    envStr("WPROC_FFPROBE", `tools\ffprobe.exe`),
		FFmpegPath:     envStr("WPROC_FFMPEG", `tools\ffmpeg.exe`),
		FFprobeTimeout: time.Duration(envInt("WPROC_FFPROBE_TIMEOUT_S", 15)) * time.Second,
		FFmpegTimeout:  time.Duration(envInt("WPROC_FFMPEG_TIMEOUT_S", 60)) * time.Second,
		ThumbCacheDir:  envStr("WPROC_THUMB_CACHE", `thumbcache`),
		ThumbMaxSide:   envInt("WPROC_THUMB_MAX_SIDE", 256),
		CrashInjection: os.Getenv("WORKER_CRASH_INJECTION") == "1",
	}
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// suppressWERDialogs 关闭 WER 崩溃弹窗/ critical 错误框，保证崩溃即退出，
// 否则 WerFault.exe 弹窗会挂住 worker，看门狗语义被破坏。
func suppressWERDialogs() {
	k32 := windows.NewLazySystemDLL("kernel32.dll")
	const SEM_FAILCRITICALERRORS = 0x0001
	const SEM_NOGPFAULTERRORBOX = 0x0002
	k32.NewProc("SetErrorMode").Call(SEM_FAILCRITICALERRORS | SEM_NOGPFAULTERRORBOX)
}

// Run 是 worker 主入口：dial → Ready → 循环收 Job。父进程死亡（管道断）即退出。
func Run(pipeName string, index int) int {
	suppressWERDialogs()
	cfg := ConfigFromEnv()

	timeout := 10 * time.Second
	nc, err := winio.DialPipe(pipeName, &timeout)
	if err != nil {
		return 2
	}
	defer nc.Close()
	c := newIPCConn(nc)

	if err := c.WriteEnv(worker.MsgReady, worker.ReadyMsg{
		PID:         os.Getpid(),
		WorkerIndex: index,
		DLLVersion:  mediacoreVersion(),
	}); err != nil {
		return 2
	}

	for {
		env, err := c.ReadEnv()
		if err != nil {
			return 0 // 父进程关闭管道：正常退出
		}
		switch env.Type {
		case worker.MsgJob:
			var job worker.JobMsg
			if err := worker.DecodeBodyFor(env, &job); err != nil {
				return 2
			}
			res := processJob(c, &cfg, &job)
			if err := c.WriteEnv(worker.MsgResult, res); err != nil {
				return 2 // 父进程没了
			}
		case worker.MsgShutdown:
			return 0
		default:
			// sha_reply 只会在 processJob 的 pumpShaReply 内被消费；此处忽略
		}
	}
}

// processJob 按媒体类别分发；崩溃注入在进 DLL 前触发。
func processJob(c *ipcConn, cfg *Config, job *worker.JobMsg) *worker.JobResultMsg {
	if cfg.CrashInjection {
		if strings.Contains(job.Path, "__crash__") {
			mediacoreDebugCrash() // 真实访问违例：进程死亡，退出码 0xC0000005
		}
		if strings.Contains(job.Path, "__hang__") {
			mediacoreDebugSleep(600_000) // 挂起 10 分钟：触发看门狗
		}
	}
	if job.Kind == worker.MediaVideo {
		return processVideo(c, cfg, job)
	}
	return processImage(c, cfg, job)
}

// pumpShaReply 发送 sha_query 后继续泵消息，直到收到本 job 的 sha_reply。
// 父进程在此期间不会派发新 Job（协议约定），所以只可能收到 sha_reply。
func pumpShaReply(c *ipcConn, jobID int64, sha []byte, kind worker.MediaKind) (*worker.ShaReplyMsg, error) {
	if err := c.WriteEnv(worker.MsgShaQuery, worker.ShaQueryMsg{
		JobID: jobID, SHA512: sha, Kind: kind,
	}); err != nil {
		return nil, err
	}
	for {
		env, err := c.ReadEnv()
		if err != nil {
			return nil, err // 父进程死亡 → 调用方 exit(2)
		}
		if env.Type != worker.MsgShaReply {
			continue
		}
		var rep worker.ShaReplyMsg
		if err := worker.DecodeBodyFor(env, &rep); err != nil {
			return nil, err
		}
		if rep.JobID == jobID {
			return &rep, nil
		}
	}
}
```

`agent/internal/wproc/ipc.go`（子进程侧薄封装，直接复用 `internal/worker` 的帧编解码）：

```go
package wproc

import (
	"net"

	"mediadedup/agent/internal/worker"
)

// ipcConn 是 worker.IPCConn 的本包别名（帧编解码父子共用，见 §4.6）。
type ipcConn = worker.IPCConn

func newIPCConn(nc net.Conn) *ipcConn { return worker.NewIPCConn(nc) }
```

`agent/internal/wproc/pipeline.go`（公共读盘 + 图片流水线）：

```go
package wproc

import (
	"io"
	"os"
	"time"

	"mediadedup/agent/internal/wproc/mediacore"
	"mediadedup/agent/internal/worker"
)

// readAndHash 4MB 块流式读文件；needSHA 时边读边算 SHA-512；
// resident=true 且文件 ≤ ImageMemBytes 时字节驻留内存返回（图片解码用）。
// 返回 (sha, buf, readMS, ferr)；ferr 非 nil 即失败（调用方直接挂到结果里）。
func readAndHash(cfg *Config, job *worker.JobMsg, resident bool) ([]byte, []byte, int64, *worker.FieldError) {
	t0 := time.Now()
	f, err := os.Open(fixPath(job.Path))
	if err != nil {
		return nil, nil, 0, &worker.FieldError{Field: job.FieldsMask, Stage: "open", Msg: err.Error()}
	}
	defer f.Close()

	needSHA := job.FieldsMask&worker.MaskSHA512 != 0
	var h *mediacore.SHA512
	if needSHA {
		h, err = mediacore.NewSHA512()
		if err != nil {
			return nil, nil, 0, &worker.FieldError{Field: worker.MaskSHA512, Stage: "sha512", Msg: err.Error()}
		}
		defer h.Close()
	}

	var buf []byte
	if resident && job.Size <= cfg.ImageMemBytes {
		buf = make([]byte, 0, job.Size)
	}
	chunk := make([]byte, cfg.ReadChunkBytes)
	for {
		n, rerr := f.Read(chunk)
		if n > 0 {
			b := chunk[:n]
			if h != nil {
				if err := h.Update(b); err != nil {
					return nil, nil, 0, &worker.FieldError{Field: worker.MaskSHA512, Stage: "sha512", Msg: err.Error()}
				}
			}
			if buf != nil {
				buf = append(buf, b...)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, nil, 0, &worker.FieldError{Field: job.FieldsMask, Stage: "read", Msg: rerr.Error()}
		}
	}

	var sha []byte
	if needSHA {
		sum, err := h.Final()
		if err != nil {
			return nil, nil, 0, &worker.FieldError{Field: worker.MaskSHA512, Stage: "sha512", Msg: err.Error()}
		}
		sha = sum[:]
	} else if len(job.KnownSHA) == mediacore.SHA512Bytes {
		sha = job.KnownSHA
	}
	return sha, buf, time.Since(t0).Milliseconds(), nil
}

// statDrift 检查派发后文件是否变化；变化则返回全掩码（本轮全量重算）。
func statDrift(job *worker.JobMsg) (uint32, *worker.FieldError) {
	fi, err := os.Stat(fixPath(job.Path))
	if err != nil {
		return 0, &worker.FieldError{Field: job.FieldsMask, Stage: "stat", Msg: err.Error()}
	}
	if fi.Size() != job.Size || fi.ModTime().Unix() != job.MTimeUnix {
		if job.Kind == worker.MediaVideo {
			return worker.MaskAllVideo, nil
		}
		return worker.MaskAllImage, nil
	}
	return job.FieldsMask, nil
}

// processImage 图片一阶段（plan 4.2）：读+SHA → single-flight → DLL 解码+PDQ。
func processImage(c *ipcConn, cfg *Config, job *worker.JobMsg) *worker.JobResultMsg {
	res := &worker.JobResultMsg{JobID: job.JobID, Path: job.Path, Kind: worker.MediaImage}

	mask, serr := statDrift(job)
	if serr != nil {
		res.Errors = append(res.Errors, *serr)
		return res
	}
	job.FieldsMask = mask

	// 1. 读盘（≤256MB 驻留）+ SHA-512
	sha, buf, readMS, ferr := readAndHash(cfg, job, true)
	res.ReadMS = readMS
	if ferr != nil {
		res.Errors = append(res.Errors, *ferr)
		return res
	}
	res.SHA512 = sha
	if job.FieldsMask&worker.MaskSHA512 != 0 {
		res.FieldsDone |= worker.MaskSHA512
	}

	if job.FieldsMask&worker.MaskImagePDQ == 0 {
		return res // 只缺 SHA：完成
	}

	// 2. single-flight：同 SHA 已有特征则复用，跳过解码
	rep, err := pumpShaReply(c, job.JobID, sha, worker.MediaImage)
	if err != nil {
		exitParentGone()
	}
	if rep.Found {
		res.PDQ, res.Quality, res.Width, res.Height = rep.PDQ, rep.Quality, rep.Width, rep.Height
		res.FieldsDone |= worker.MaskImagePDQ
		return res
	}

	// 3. DLL 解码 + PDQ（超内存阈值无法驻留解码 → 仅 PDQ 字段失败）
	if buf == nil {
		res.Errors = append(res.Errors, worker.FieldError{
			Field: worker.MaskImagePDQ, Stage: "decode",
			Msg: "image exceeds memory threshold (256MB), sha512 only",
		})
		return res
	}
	t0 := time.Now()
	pr, derr := mediacore.ImagePhase1(buf)
	res.DecodeMS = time.Since(t0).Milliseconds()
	if derr != nil {
		res.Errors = append(res.Errors, worker.FieldError{
			Field: worker.MaskImagePDQ, Stage: "decode", Msg: derr.Error(),
		})
		return res
	}
	res.Decoded = true
	res.PDQ = pr.Hash[:]
	res.Quality = pr.Quality
	res.Width = pr.Width
	res.Height = pr.Height
	res.FieldsDone |= worker.MaskImagePDQ
	return res
}

// exitParentGone：父进程已死，worker 无存在意义，立即退出（退出码非 0 无所谓，父进程已不在）。
func exitParentGone() { os.Exit(2) }
```

`agent/internal/wproc/pipeline_video.go`（视频一阶段）：

```go
package wproc

import (
	"os"
	"time"

	"mediadedup/agent/internal/wproc/mediacore"
	"mediadedup/agent/internal/worker"
)

// processVideo 视频一阶段（plan 4.2）：SHA → single-flight → ffprobe 时长
// → 缩略图（缓存优先，否则 ffmpeg 中点帧）→ 缩略图 PDQ。
func processVideo(c *ipcConn, cfg *Config, job *worker.JobMsg) *worker.JobResultMsg {
	res := &worker.JobResultMsg{JobID: job.JobID, Path: job.Path, Kind: worker.MediaVideo}

	mask, serr := statDrift(job)
	if serr != nil {
		res.Errors = append(res.Errors, *serr)
		return res
	}
	job.FieldsMask = mask

	// 1. 读盘 + SHA-512（视频不驻留）
	sha, _, readMS, ferr := readAndHash(cfg, job, false)
	res.ReadMS = readMS
	if ferr != nil {
		res.Errors = append(res.Errors, *ferr)
		return res
	}
	res.SHA512 = sha
	if job.FieldsMask&worker.MaskSHA512 != 0 {
		res.FieldsDone |= worker.MaskSHA512
	}

	needMeta := job.FieldsMask&(worker.MaskVideoDur|worker.MaskVideoThumb) != 0
	if !needMeta {
		return res
	}

	// 2. single-flight
	rep, err := pumpShaReply(c, job.JobID, sha, worker.MediaVideo)
	if err != nil {
		exitParentGone()
	}
	if rep.Found {
		res.DurationMS = rep.DurationMS
		res.ThumbPath, res.ThumbPDQ, res.ThumbQuality = rep.ThumbPath, rep.ThumbPDQ, rep.ThumbQuality
		res.FieldsDone |= job.FieldsMask & (worker.MaskVideoDur | worker.MaskVideoThumb)
		return res
	}

	// 3. ffprobe 时长（15s 超时）
	var durationMS int64
	durationKnown := false
	if job.FieldsMask&worker.MaskVideoDur != 0 {
		d, derr := ffprobeDuration(cfg, job.Path)
		if derr != nil {
			res.Errors = append(res.Errors, worker.FieldError{
				Field: worker.MaskVideoDur, Stage: "ffprobe", Msg: derr.Error(),
			})
		} else {
			durationMS = d
			durationKnown = true
			res.DurationMS = d
			res.FieldsDone |= worker.MaskVideoDur
		}
	}

	// 4. 缩略图：缓存命中直接用；否则 ffmpeg 中点帧生成（60s 超时）
	if job.FieldsMask&worker.MaskVideoThumb != 0 {
		fi, statErr := os.Stat(fixPath(job.Path))
		if statErr != nil {
			res.Errors = append(res.Errors, worker.FieldError{
				Field: worker.MaskVideoThumb, Stage: "stat", Msg: statErr.Error(),
			})
			return res
		}
		thumbPath, hit, lerr := thumbCacheLookup(cfg, job.Path, fi)
		if lerr != nil {
			res.Errors = append(res.Errors, worker.FieldError{
				Field: worker.MaskVideoThumb, Stage: "ffmpeg", Msg: lerr.Error(),
			})
			return res
		}
		res.ThumbCacheHit = hit
		if !hit {
			// 中点帧；时长未知（ffprobe 失败）时退回 0s 首帧
			seekSec := 0.0
			if durationKnown {
				seekSec = float64(durationMS) / 2000.0
			}
			t0 := time.Now()
			if gerr := ffmpegShot(cfg, job.Path, seekSec, thumbPath); gerr != nil {
				res.Errors = append(res.Errors, worker.FieldError{
					Field: worker.MaskVideoThumb, Stage: "ffmpeg", Msg: gerr.Error(),
				})
				return res
			}
			res.ThumbMS = time.Since(t0).Milliseconds()
			res.ThumbGenerated = true
			if werr := thumbCacheWriteMeta(cfg, job.Path, fi); werr != nil {
				// 元信息写失败不致命：下轮缓存不命中重新生成
				res.Errors = append(res.Errors, worker.FieldError{
					Field: 0, Stage: "ffmpeg", Msg: "thumb meta write: " + werr.Error(),
				})
			}
		}

		// 5. 缩略图 PDQ（缩略图是小 JPEG，直接整读交 DLL）
		data, rerr := os.ReadFile(thumbPath)
		if rerr != nil {
			res.Errors = append(res.Errors, worker.FieldError{
				Field: worker.MaskVideoThumb, Stage: "thumb_pdq", Msg: rerr.Error(),
			})
			return res
		}
		pr, derr := mediacore.ImagePhase1(data)
		if derr != nil {
			res.Errors = append(res.Errors, worker.FieldError{
				Field: worker.MaskVideoThumb, Stage: "thumb_pdq", Msg: derr.Error(),
			})
			return res
		}
		res.Decoded = true
		res.ThumbPath = thumbPath
		res.ThumbPDQ = pr.Hash[:]
		res.ThumbQuality = pr.Quality
		res.FieldsDone |= worker.MaskVideoThumb
	}
	return res
}
```

`agent/internal/wproc/ffmpeg.go`：

```go
package wproc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ffprobeDuration 取容器时长（毫秒）；15s 超时（plan 4.2）。
func ffprobeDuration(cfg *Config, path string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.FFprobeTimeout)
	defer cancel()
	args := []string{
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path,
	}
	out, err := exec.CommandContext(ctx, cfg.FFprobePath, args...).Output()
	if ctx.Err() == context.DeadlineExceeded {
		return 0, fmt.Errorf("ffprobe timeout after %s", cfg.FFprobeTimeout)
	}
	if err != nil {
		return 0, fmt.Errorf("ffprobe: %v", err)
	}
	s := strings.TrimSpace(string(out))
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("ffprobe duration %q unparseable", s)
	}
	return int64(f * 1000), nil
}

// ffmpegShot 用快速 seek 取 seekSec 处一帧，灰度缩放后写 dst（先写临时文件再替换）。
// 说明：-ss 在 -i 之前是关键帧级快速定位，中点缩略图允许此误差（M4 六帧校验另议）。
func ffmpegShot(cfg *Config, src string, seekSec float64, dst string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.FFmpegTimeout)
	defer cancel()
	tmp := dst + ".tmp-" + strconv.Itoa(os.Getpid())
	_ = os.Remove(tmp)
	defer os.Remove(tmp)

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("thumb dir: %w", err)
	}
	vf := fmt.Sprintf("scale='min(%d,iw)':-2,format=gray", cfg.ThumbMaxSide)
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-ss", strconv.FormatFloat(seekSec, 'f', 3, 64),
		"-i", src,
		"-frames:v", "1",
		"-an", "-sn", "-dn",
		"-vf", vf,
		"-q:v", "3",
		"-f", "image2", "-y", tmp,
	}
	cmd := exec.CommandContext(ctx, cfg.FFmpegPath, args...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("ffmpeg timeout after %s", cfg.FFmpegTimeout)
	}
	if err != nil {
		return fmt.Errorf("ffmpeg: %v: %s", err, strings.TrimSpace(string(out)))
	}
	fi, err := os.Stat(tmp)
	if err != nil || fi.Size() == 0 {
		return fmt.Errorf("ffmpeg produced no thumbnail")
	}
	// Windows 下 os.Rename 不能覆盖已存在目标：先删再换（缓存文件可重建，可接受）
	_ = os.Remove(dst)
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("commit thumbnail: %w", err)
	}
	return nil
}
```

`agent/internal/wproc/thumbcache.go`（plan 4.4.2：按路径判断已存在 + mtime 校验）：

```go
package wproc

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// thumbCacheKey = sha1(lower(clean(abspath)))：同一路径恒定同键；
// 两级目录（前 2 hex 字符）防单目录文件过多。
func thumbCacheKey(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	norm := strings.ToLower(filepath.Clean(abs))
	sum := sha1.Sum([]byte(norm))
	return hex.EncodeToString(sum[:])
}

func thumbPathFor(cfg *Config, path string) string {
	key := thumbCacheKey(path)
	return filepath.Join(cfg.ThumbCacheDir, key[:2], key+".jpg")
}

type thumbMeta struct {
	MTimeUnix int64 `json:"mtime_unix"`
	Size      int64 `json:"size"`
}

// thumbCacheLookup 命中判定：缩略图与 sidecar meta 都在，且源 mtime+size 未变。
// 返回 (缩略图路径, 是否命中, 错误)；未命中不报错，由调用方生成。
func thumbCacheLookup(cfg *Config, src string, fi fs.FileInfo) (string, bool, error) {
	tp := thumbPathFor(cfg, src)
	if _, err := os.Stat(tp); err != nil {
		return tp, false, nil // 缩略图不存在：未命中
	}
	mb, err := os.ReadFile(tp + ".json")
	if err != nil {
		return tp, false, nil // meta 缺失：视为未命中（重新生成并补写）
	}
	var m thumbMeta
	if err := json.Unmarshal(mb, &m); err != nil {
		return tp, false, nil
	}
	if m.MTimeUnix != fi.ModTime().Unix() || m.Size != fi.Size() {
		return tp, false, nil // 源已变：未命中
	}
	return tp, true, nil
}

// thumbCacheWriteMeta 在缩略图生成成功后写 sidecar。
func thumbCacheWriteMeta(cfg *Config, src string, fi fs.FileInfo) error {
	tp := thumbPathFor(cfg, src)
	m := thumbMeta{MTimeUnix: fi.ModTime().Unix(), Size: fi.Size()}
	b, err := json.Marshal(&m)
	if err != nil {
		return err
	}
	return os.WriteFile(tp+".json", b, 0o644)
}
```

`agent/internal/wproc/fixpath.go`：

```go
package wproc

import (
	"path/filepath"
	"strings"
)

// fixPath 对超长路径加 \\?\ 前缀（Windows API 的 MAX_PATH 限制绕过）。
func fixPath(p string) string {
	if len(p) >= 240 && !strings.HasPrefix(p, `\\?\`) {
		if abs, err := filepath.Abs(p); err == nil {
			return `\\?\` + abs
		}
	}
	return p
}
```

`agent/internal/wproc/hooks.go`（cgo 绑定的进程内别名，便于 `run.go` 不直接依赖 cgo 包路径细节）：

```go
package wproc

import "mediadedup/agent/internal/wproc/mediacore"

func mediacoreVersion() string          { return mediacore.Version() }
func mediacoreDebugCrash()              { mediacore.DebugCrash() }
func mediacoreDebugSleep(ms uint32)     { mediacore.DebugSleepMS(ms) }
```

### 4.9 single-flight `agent/internal/worker/deduper.go`

语义（plan 4.2 步骤 2 的落地）：特征以 SHA-512 为主键，**同内容只解码一次**。

- 首个查询某 SHA 的 worker → 查库未命中 → 注册 flight 成为 owner，收到 `Found=false` 去解码；
- 期间第二个查询同 SHA 的 worker → 阻塞在 flight 上，owner 结果落库后被唤醒，收到 `Found=true` 直接复用；
- owner 崩溃（`FailByJob`）→ 等待者被唤醒拿到 `Found=false`，**回到 Ask 重试**（其中一个成为新 owner），不会被无辜拖死（看门狗不会因等待触发：等待发生在父进程侧 Ask，worker 本身在等 sha_reply——注意：**等待 flight 的时间计入该 worker 的单文件看门狗**。同 SHA 批量场景 owner 正常解码远快于 30s/120s，可接受）。
- 非并发场景（第二个文件晚于第一个算完）：Ask 直接查库命中。

```go
package worker

import (
	"sync"
)

// flight 一个进行中的同 SHA 计算。
type flight struct {
	done    chan struct{}
	ownerID int64        // owner 的 JobID（崩溃清理用）
	reply   *ShaReplyMsg // owner 完成后填充；Found=false 表示需要重试
}

// Deduper 同 SHA-512 特征计算的 single-flight 合并器。
type Deduper struct {
	mu      sync.Mutex
	store   FeatureStore
	flights map[string]*flight
	byJob   map[int64]string // JobID → flight key（崩溃清理用）
}

func NewDeduper(store FeatureStore) *Deduper {
	return &Deduper{
		store:   store,
		flights: make(map[string]*flight),
		byJob:   make(map[int64]string),
	}
}

func flightKey(kind MediaKind, sha []byte) string {
	return string(rune('0'+kind)) + "|" + string(sha)
}

// Ask 处理 worker 的 sha_query。可能阻塞（等同 SHA owner 算完）。
func (d *Deduper) Ask(q *ShaQueryMsg) *ShaReplyMsg {
	for {
		rep, retry := d.askOnce(q)
		if !retry {
			rep.JobID = q.JobID
			return rep
		}
	}
}

// askOnce 返回 (reply, needRetry)。
func (d *Deduper) askOnce(q *ShaQueryMsg) (*ShaReplyMsg, bool) {
	key := flightKey(q.Kind, q.SHA512)

	d.mu.Lock()
	if f, ok := d.flights[key]; ok {
		d.mu.Unlock()
		<-f.done // 等 owner 完成或失败
		if f.reply.Found {
			rep := *f.reply
			return &rep, false
		}
		return nil, true // owner 崩了：重试抢 owner
	}
	f := &flight{done: make(chan struct{}), ownerID: q.JobID}
	d.flights[key] = f
	d.byJob[q.JobID] = key
	d.mu.Unlock()

	// 我是 owner：先查库（非并发场景直接命中）
	rep := &ShaReplyMsg{Found: false}
	switch q.Kind {
	case MediaImage:
		if feat, err := d.store.LookupImage(q.SHA512); err == nil && feat != nil && feat.PDQ != nil {
			rep.Found = true
			rep.PDQ, rep.Quality = feat.PDQ, feat.Quality
			rep.Width, rep.Height = feat.Width, feat.Height
		}
	case MediaVideo:
		if feat, err := d.store.LookupVideo(q.SHA512); err == nil && feat != nil &&
			feat.DurationMS != nil && feat.ThumbPDQ != nil {
			rep.Found = true
			rep.DurationMS = *feat.DurationMS
			rep.ThumbPath, rep.ThumbPDQ, rep.ThumbQuality = feat.ThumbPath, feat.ThumbPDQ, feat.ThumbQuality
		}
	}
	if rep.Found {
		d.finish(key, q.JobID, rep)
	}
	// 未命中：保持 flight 注册，owner 去解码；Resolve/FailByJob 收尾
	return rep, false
}

// Resolve 在 owner 结果落库后调用：构建缓存应答并唤醒全部等待者。
func (d *Deduper) Resolve(res *JobResultMsg) {
	if len(res.SHA512) == 0 {
		return
	}
	key := flightKey(res.Kind, res.SHA512)
	rep := &ShaReplyMsg{Found: false}
	switch res.Kind {
	case MediaImage:
		if res.FieldsDone&MaskImagePDQ != 0 {
			rep.Found = true
			rep.PDQ, rep.Quality = res.PDQ, res.Quality
			rep.Width, rep.Height = res.Width, res.Height
		}
	case MediaVideo:
		if res.FieldsDone&(MaskVideoDur|MaskVideoThumb) == (MaskVideoDur | MaskVideoThumb) {
			rep.Found = true
			rep.DurationMS = res.DurationMS
			rep.ThumbPath, rep.ThumbPDQ, rep.ThumbQuality = res.ThumbPath, res.ThumbPDQ, res.ThumbQuality
		}
	}
	d.finish(key, res.JobID, rep)
}

// FailByJob owner 崩溃时调用：唤醒等待者重试（reply.Found=false）。
func (d *Deduper) FailByJob(jobID int64) {
	d.mu.Lock()
	key, ok := d.byJob[jobID]
	d.mu.Unlock()
	if !ok {
		return
	}
	d.finish(key, jobID, &ShaReplyMsg{Found: false})
}

// finish 关闭 flight 并清理登记。幂等（重复调用安全）。
func (d *Deduper) finish(key string, jobID int64, rep *ShaReplyMsg) {
	d.mu.Lock()
	f, ok := d.flights[key]
	if ok {
		delete(d.flights, key)
		delete(d.byJob, jobID)
	}
	d.mu.Unlock()
	if ok {
		f.reply = rep
		close(f.done)
	}
}
```

### 4.10 SQL DDL 与 missing_mask 计算

`agent/internal/store/migrations/002_phase1_features.sql`（按 plan §6.1；二阶段列现在就建为可空，M4 免迁移；`video_frames` 表属 M4，此处不建）：

```sql
-- M2：一阶段特征表
CREATE TABLE IF NOT EXISTS image_features (
    sha512       BLOB PRIMARY KEY,          -- 64 字节
    width        INTEGER NOT NULL,
    height       INTEGER NOT NULL,
    pdq256       BLOB,                      -- 32 字节；NULL=未算/失败
    pdq_quality  INTEGER,                   -- 0-100
    phash_parts  BLOB,                      -- 二阶段（M4），可空
    sobel_hist   BLOB,                      -- 二阶段（M4），可空
    updated_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS video_features (
    sha512        BLOB PRIMARY KEY,
    duration_ms   INTEGER,                  -- NULL=未知（ffprobe 失败）
    thumb_path    TEXT,
    thumb_pdq256  BLOB,                     -- 32 字节
    thumb_quality INTEGER,
    updated_at    INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_files_sha512 ON files(sha512);
CREATE INDEX IF NOT EXISTS idx_files_status ON files(status);
```

`agent/internal/store/mask.go`（plan 4.4.1 字段级剪枝）：

```go
package store

import "mediadedup/agent/internal/worker"

// FileRow 是 files 表的读取投影（M1 表）。
type FileRow struct {
	ID        int64
	Path      string
	Size      int64
	MTimeUnix int64
	SHA512    []byte // nil=未算
}

// Phase1MissingMask 计算某文件的一阶段字段级缺失掩码。
// 规则：
//  1. 无记录 或 size/mtime 已变 → 全掩码（全部重算）；
//  2. sha512 缺失 → 全掩码（无 sha 无法联查特征行）；
//  3. 按特征行/字段是否存在逐位置位；上轮 failed/crash 的文件特征缺失 → 自动补算；
//  4. 返回 0 = 本阶段特征已齐 → 整文件跳过，不派发。
func Phase1MissingMask(kind worker.MediaKind, curSize, curMTime int64,
	row *FileRow, img *worker.ImageFeature, vid *worker.VideoFeature) uint32 {

	full := worker.MaskAllImage
	if kind == worker.MediaVideo {
		full = worker.MaskAllVideo
	}
	if row == nil || row.Size != curSize || row.MTimeUnix != curMTime {
		return full
	}
	if len(row.SHA512) != 64 {
		return full
	}
	var mask uint32
	if kind == worker.MediaImage {
		if img == nil || img.PDQ == nil {
			mask |= worker.MaskImagePDQ
		}
	} else {
		if vid == nil || vid.DurationMS == nil {
			mask |= worker.MaskVideoDur
		}
		if vid == nil || vid.ThumbPDQ == nil || vid.ThumbPath == "" {
			mask |= worker.MaskVideoThumb
		}
	}
	return mask
}
```

`agent/internal/store/features.go`（`FeatureStore` 实现；UPSERT 需 SQLite ≥3.24，`modernc.org/sqlite` 满足）：

```go
package store

import (
	"database/sql"
	"encoding/hex"
	"strings"
	"time"

	"mediadedup/agent/internal/worker"
)

type Store struct{ db *sql.DB } // M1 已有；此处仅展示 M2 增量方法

func (s *Store) LookupImage(sha []byte) (*worker.ImageFeature, error) {
	var f worker.ImageFeature
	err := s.db.QueryRow(
		`SELECT sha512, width, height, pdq256, COALESCE(pdq_quality,0) FROM image_features WHERE sha512=?`, sha).
		Scan(&f.SHA512, &f.Width, &f.Height, &f.PDQ, &f.Quality)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

func (s *Store) LookupVideo(sha []byte) (*worker.VideoFeature, error) {
	var f worker.VideoFeature
	var thumbPath sql.NullString
	var thumbQ sql.NullInt64
	err := s.db.QueryRow(
		`SELECT sha512, duration_ms, thumb_path, thumb_pdq256, thumb_quality FROM video_features WHERE sha512=?`, sha).
		Scan(&f.SHA512, &f.DurationMS, &thumbPath, &f.ThumbPDQ, &thumbQ)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f.ThumbPath = thumbPath.String
	f.ThumbQuality = int32(thumbQ.Int64)
	return &f, nil
}

// SavePhase1 一事务：files 更新（sha/状态/掩码清位/phase1_done）+ 特征 UPSERT + sync_queue 入队。
func (s *Store) SavePhase1(res *worker.JobResultMsg) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()

	status := "done"
	if len(res.Errors) > 0 {
		if res.FieldsDone == 0 {
			status = "failed"
		} else {
			status = "partial"
		}
	}
	errMsg := joinErrMsgs(res.Errors)

	// files：清掉已完成位；掩码归零则 phase1_done=1（?3 = FieldsDone 复用）
	if _, err := tx.Exec(`
		UPDATE files SET sha512 = COALESCE(?1, sha512),
		    status = ?2,
		    missing_mask = missing_mask & ~?3,
		    phase1_done = CASE WHEN (missing_mask & ~?3) = 0 THEN 1 ELSE 0 END,
		    error = ?4, updated_at = ?5
		WHERE path = ?6`,
		nullBlob(res.SHA512), status, res.FieldsDone, errMsg, now, res.Path); err != nil {
		return err
	}

	switch res.Kind {
	case worker.MediaImage:
		if res.FieldsDone&worker.MaskImagePDQ != 0 {
			if _, err := tx.Exec(`
				INSERT INTO image_features(sha512, width, height, pdq256, pdq_quality, updated_at)
				VALUES(?1,?2,?3,?4,?5,?6)
				ON CONFLICT(sha512) DO UPDATE SET
				    width=excluded.width, height=excluded.height,
				    pdq256=excluded.pdq256, pdq_quality=excluded.pdq_quality,
				    updated_at=excluded.updated_at`,
				res.SHA512, res.Width, res.Height, res.PDQ, res.Quality, now); err != nil {
				return err
			}
			if err := enqueue(tx, "image_features", res.SHA512); err != nil {
				return err
			}
		}
	case worker.MediaVideo:
		if res.FieldsDone&(worker.MaskVideoDur|worker.MaskVideoThumb) != 0 {
			if _, err := tx.Exec(`
				INSERT INTO video_features(sha512, duration_ms, thumb_path, thumb_pdq256, thumb_quality, updated_at)
				VALUES(?1,?2,?3,?4,?5,?6)
				ON CONFLICT(sha512) DO UPDATE SET
				    duration_ms  = COALESCE(excluded.duration_ms,  video_features.duration_ms),
				    thumb_path   = COALESCE(excluded.thumb_path,   video_features.thumb_path),
				    thumb_pdq256 = COALESCE(excluded.thumb_pdq256, video_features.thumb_pdq256),
				    thumb_quality= COALESCE(excluded.thumb_quality,video_features.thumb_quality),
				    updated_at   = excluded.updated_at`,
				res.SHA512, nullInt64Ptr(res.DurationMS, res.FieldsDone&worker.MaskVideoDur != 0),
				nullString(res.ThumbPath), nullBlob(res.ThumbPDQ), nullInt32(res.ThumbQuality), now); err != nil {
				return err
			}
			if err := enqueue(tx, "video_features", res.SHA512); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// MarkCrash 崩溃善后：文件标 crash（status），错误入列；特征字段保持缺失，下轮自动补算。
func (s *Store) MarkCrash(path string, errMsg string) error {
	_, err := s.db.Exec(
		`UPDATE files SET status='crash', error=?, updated_at=? WHERE path=?`,
		errMsg, time.Now().Unix(), path)
	return err
}

func enqueue(tx *sql.Tx, table string, sha []byte) error {
	_, err := tx.Exec(
		`INSERT INTO sync_queue(table_name, row_pk, synced) VALUES(?,?,0)`, table, hex.EncodeToString(sha))
	return err
}

func joinErrMsgs(fes []worker.FieldError) string {
	if len(fes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(fes))
	for _, fe := range fes {
		parts = append(parts, fe.Stage+": "+fe.Msg)
	}
	return strings.Join(parts, " | ")
}

func nullBlob(b []byte) interface{} {
	if len(b) == 0 {
		return nil
	}
	return b
}

func nullString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func nullInt32(v int32) interface{} { return v }

func nullInt64Ptr(v int64, ok bool) interface{} {
	if !ok {
		return nil
	}
	return v
}
```

> 注：`video_features.duration_ms` 列允许 NULL，`worker.VideoFeature.DurationMS *int64` 与之对应；`SavePhase1` 里只有 `MaskVideoDur` 完成位才写时长，`COALESCE` 保证部分失败不覆盖已有值。`sync_queue` 表结构以 M1 为准（`table_name, row_pk, synced`），若 M1 定义不同以 M1 为准适配。

---

## 5. 数据模型与配置项

### 5.1 运行期目录布局

```
bin/                              # 部署目录
├── agent.exe                     # 主进程（CGO_ENABLED=0，不加载任何 DLL）
├── worker.exe                    # 计算子进程（cgo，启动即加载 mediacore.dll）
├── mediacore.dll
├── libmediacore.a                # 仅构建期需要（cgo 导入库）
└── tools/
    ├── ffmpeg.exe
    └── ffprobe.exe
<data_dir>/                       # M1 配置的数据目录
├── agent.db / agent.db-wal / agent.db-shm
├── logs/
│   ├── agent.log                 # 启动/任务/进度/metrics 汇总
│   ├── errors.log                # 每文件每失败字段一行
│   └── crash.log                 # worker 崩溃一行一次
└── thumbcache/
    └── <xx>/<sha1(path)>.jpg     # 缩略图（灰度缩放 JPEG）
        <sha1(path)>.jpg.json     # sidecar：{"mtime_unix":…,"size":…}
```

### 5.2 配置项表（全部可配；与 plan §9 默认值一致，新增项已注明）

| 配置键（TOML 路径） | 默认值 | plan §9 / §4 对应 | 说明 |
|---|---|---|---|
| `worker.count` | `0`（= `runtime.NumCPU()`） | Worker 数 = CPU 核数 | 常驻 worker.exe 数 |
| `worker.exe_path` | `<exe_dir>/worker.exe` | — | |
| `worker.image_timeout_s` | `30` | 图片单文件超时 30s | 看门狗 |
| `worker.video_timeout_s` | `120` | 视频单文件超时 120s | 看门狗 |
| `worker.image_memory_mb` | `256` | 图片内存驻留阈值 256MB | 超过则只算 SHA-512 |
| `worker.respawn_delay_ms` | `500` | —（新增工程护栏） | 崩溃重生退避 |
| `pipeline.read_chunk_kb` | `4096` | HDD 读块 4MB | 流式读 + SHA-512 块大小；SSD 1MB 调优属 M6 |
| `thumb.cache_dir` | `<data_dir>/thumbcache` | 缩略图落本地缓存 | |
| `thumb.max_side` | `256` | 视频缩略图"灰度缩放"（plan 未定具体尺寸，新增默认） | 长边上限，等比 |
| `thumb.ffmpeg_path` | `<exe_dir>/tools/ffmpeg.exe` | ffmpeg CLI 子进程 | |
| `thumb.ffprobe_path` | `<exe_dir>/tools/ffprobe.exe` | ffprobe CLI 子进程 | |
| `thumb.ffprobe_timeout_s` | `15` | plan 4.2：ffprobe 超时 15s | |
| `thumb.ffmpeg_timeout_s` | `60` | —（新增；含在 120s 单文件预算内） | 截图子进程超时 |
| `ipc.max_frame_mb` | `16` | —（新增协议护栏） | 单帧上限 |
| `worker.crash_injection` | `false` | — | 仅验收用，生产严禁开启 |

注入方式：agent.exe 读配置 → 组装 `WPROC_*` 环境变量列表（`Pool.Config.WorkerEnv`）→ spawn worker.exe 时传入 `cmd.Env`；worker.exe 侧 `ConfigFromEnv()`（§4.8）读取。二阶段阈值类参数（T1~T4 等）属 M3/M4，本文不列。

### 5.3 日志 schema（slog JSON 行；键名以 M1 handler 为准，下例为默认 JSONHandler）

`errors.log` —— 每文件每失败字段一行（plan §8）：

```json
{"time":"2026-07-26T16:00:01.234+08:00","level":"ERROR","msg":"file error","path":"D:\\media\\bad.jpg","stage":"decode","field_mask":2,"err":"mediacore(-3): jpeg decode: Unsupported color conversion","worker_pid":12345}
```

- `stage` 枚举：`stat / open / read / sha512 / decode / ffprobe / ffmpeg / thumb_pdq`
- `field_mask`：失败的 `Mask*` 位（§4.6）

`crash.log` —— worker 崩溃一行一次（plan §8、4.1）：

```json
{"time":"2026-07-26T16:01:11.005+08:00","level":"ERROR","msg":"worker crashed","pid":12345,"worker_index":2,"file":"D:\\media\\x__crash__.jpg","exit_code":-1073741819,"reason":"exit_code"}
```

- `reason` 枚举：`watchdog_image / watchdog_video / exit_code / pipe_eof / pipe_write`
- 访问违例退出码为 `0xC0000005`（slog 输出十进制 `-1073741819`）
- worker 空闲时崩溃（无当前任务）`file` 为空字符串

`agent.log` 任务级汇总行（验收取数点）：每轮扫描任务完成时输出一行，含 `files_done / files_failed / decode_calls / thumb_generated / thumb_cache_hits / singleflight_hits / crashes / elapsed_ms`（来自 `Pool.Metrics`）。

---

## 6. 测试与验收用例

### 6.1 C++ 侧回归（接入 Go 前的硬门禁）

判据采用 [pdq/README.md 官方"正确性"定义](https://github.com/facebook/ThreatExchange/blob/main/pdq/README.md)：

- **Level A（位精确，必须 100% 通过）**：上游参考实现生成的字节数组（灰度 luma）喂给本移植，产出的 hash + quality 必须与参考实现**逐位一致**。
- **Level B（端到端）**：完整图片经各自解码管线，**quality ≥ 80 的样本汉明距离 ≤ 10**（官方容差；我们期望绝大多数为 0，记录分布）。

CMake 追加（`MEDIACORE_BUILD_TESTS` 分支内）：

```cmake
add_executable(ref_luma_hasher tests/ref_luma_main.cpp ${PDQ_SOURCES})
target_include_directories(ref_luma_hasher PRIVATE src/pdq_upstream)
```

#### 6.1.1 Level A：`tests/make_luma.cpp`（确定性向量生成器，72 个）

`.lumabin` 格式：`int32 w, int32 h` + `w*h` 字节灰度。覆盖：极小图（8×8）、非 64 整数倍（63×65、65×63、31×2048）、方形/横竖长条、4K 大图；图案含随机/渐变/纯色/棋盘/斜纹（纯色与棋盘是 quality 低分与高边缘场景）。

```cpp
#include <cstdint>
#include <cstdio>
#include <string>
#include <vector>

static uint32_t xs = 0x12345678u;
static uint32_t xorshift(void) {
    xs ^= xs << 13; xs ^= xs >> 17; xs ^= xs << 5;
    return xs;
}

static void writeLuma(const std::string& dir, const char* name, int w, int h, int pattern) {
    std::vector<uint8_t> g((size_t)w * (size_t)h);
    for (int y = 0; y < h; y++) {
        for (int x = 0; x < w; x++) {
            uint8_t v;
            switch (pattern) {
            case 0: v = (uint8_t)(xorshift() & 0xFF); break;                 /* 随机噪点 */
            case 1: v = (uint8_t)((x * 255) / (w > 1 ? w - 1 : 1)); break;   /* 水平渐变 */
            case 2: v = (uint8_t)((y * 255) / (h > 1 ? h - 1 : 1)); break;   /* 垂直渐变 */
            case 3: v = 128; break;                                          /* 纯色 */
            case 4: v = (uint8_t)(((x / 8) + (y / 8)) % 2 ? 255 : 0); break; /* 棋盘 */
            default: v = (uint8_t)((x + y) & 0xFF); break;                   /* 斜纹 */
            }
            g[(size_t)y * w + x] = v;
        }
    }
    std::string path = dir + "/" + name + ".lumabin";
    FILE* f = fopen(path.c_str(), "wb");
    if (!f) { fprintf(stderr, "cannot write %s\n", path.c_str()); exit(1); }
    int32_t wh[2] = { w, h };
    fwrite(wh, 4, 2, f);
    fwrite(g.data(), 1, g.size(), f);
    fclose(f);
}

int main(int argc, char** argv) {
    if (argc < 2) { fprintf(stderr, "usage: mc_make_luma <outdir>\n"); return 1; }
    static const int sizes[][2] = {
        {8, 8}, {16, 16}, {63, 65}, {64, 64}, {65, 63}, {100, 100},
        {127, 255}, {256, 256}, {640, 480}, {1920, 1080}, {4096, 3072}, {31, 2048},
    };
    char name[128];
    int n = 0;
    for (const auto& s : sizes) {
        for (int p = 0; p < 6; p++) {
            snprintf(name, sizeof(name), "luma_%dx%d_p%d", s[0], s[1], p);
            writeLuma(argv[1], name, s[0], s[1], p);
            n++;
        }
    }
    printf("wrote %d luma vectors to %s\n", n, argv[1]);
    return 0;
}
```

`tests/mc_luma_runner.cpp`（本移植侧 runner，链接 mediacore.dll）：

```cpp
#include "mediacore/mediacore.h"
#include <cstdint>
#include <cstdio>
#include <vector>

int main(int argc, char** argv) {
    if (argc < 2) { fprintf(stderr, "usage: mc_luma_runner <file.lumabin>\n"); return 2; }
    FILE* f = fopen(argv[1], "rb");
    if (!f) return 2;
    int32_t wh[2];
    if (fread(wh, 4, 2, f) != 2) { fclose(f); return 2; }
    std::vector<uint8_t> g((size_t)wh[0] * (size_t)wh[1]);
    if (fread(g.data(), 1, g.size(), f) != g.size()) { fclose(f); return 2; }
    fclose(f);
    uint8_t hash[MC_PDQ256_BYTES];
    int32_t quality = 0;
    char eb[MC_ERRBUF_LEN];
    int rc = mc_pdq256_from_gray(g.data(), wh[0], wh[1], hash, &quality, eb, sizeof(eb));
    if (rc != MC_OK) { fprintf(stderr, "pdq failed: %s\n", eb); return 1; }
    for (int i = 0; i < MC_PDQ256_BYTES; i++) printf("%02x", hash[i]);
    printf(" %d\n", quality);
    return 0;
}
```

`tests/ref_luma_main.cpp`（参考侧 runner，只编上游移植件，与我们的 DLL 无关）：

```cpp
#include <pdq/cpp/hashing/pdqhashing.h>
#include <cstdint>
#include <cstdio>
#include <vector>

using namespace facebook::pdq::hashing;

int main(int argc, char** argv) {
    if (argc < 2) { fprintf(stderr, "usage: ref_luma_hasher <file.lumabin>\n"); return 2; }
    FILE* f = fopen(argv[1], "rb");
    if (!f) return 2;
    int32_t wh[2];
    if (fread(wh, 4, 2, f) != 2) { fclose(f); return 2; }
    const int numCols = wh[0], numRows = wh[1];
    std::vector<uint8_t> g((size_t)numRows * (size_t)numCols);
    if (fread(g.data(), 1, g.size(), f) != g.size()) { fclose(f); return 2; }
    fclose(f);
    std::vector<float> fb1(g.size()), fb2(g.size());
    fillFloatLumaFromGrey(g.data(), numRows, numCols, numCols, 1, fb1.data());
    float b64[64][64], b16x64[16][64], b16[16][16];
    Hash256 hash;
    int q = 0;
    pdqHash256FromFloatLuma(fb1.data(), fb2.data(), numRows, numCols, b64, b16x64, b16, hash, q);
    printf("%s %d\n", hash.format().c_str(), q);
    return 0;
}
```

`tests/run_level_a.sh`（Git Bash 执行；**任一不一致即 FAIL，不得带债接入**）：

```bash
#!/usr/bin/env bash
set -u
FAIL=0; N=0
for f in mediacore/testdata/luma/*.lumabin; do
  N=$((N+1))
  A=$(bin/mc_luma_runner.exe "$f")  || { echo "RUNNER-FAIL $f"; FAIL=1; continue; }
  B=$(bin/ref_luma_hasher.exe "$f") || { echo "REF-FAIL $f";    FAIL=1; continue; }
  if [ "$A" != "$B" ]; then
    echo "MISMATCH $f: ours=[$A] ref=[$B]"; FAIL=1
  fi
done
echo "level-a: $N vectors checked"
if [ $FAIL -eq 0 ]; then echo "LEVEL-A PASS"; else echo "LEVEL-A FAIL"; exit 1; fi
```

> 若出现 MISMATCH，按序排查：① 32B 导出位序（`w[15]→w[0]` 大端）；② 上游拷贝是否被改动；③ `fillFloatLumaFromGrey` 的 stride 参数；④ 编译器浮点选项（不要开 `/fp:fast`——CMake 默认 `/fp:precise`，禁止改）。

#### 6.1.2 Level B：`tests/endtoend_main.cpp`（端到端 / HD / SHA-512 三合一工具）

```cpp
#include "mediacore/mediacore.h"
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <string>
#include <vector>

static std::vector<uint8_t> readAll(const char* path) {
    FILE* f = fopen(path, "rb");
    if (!f) { fprintf(stderr, "open %s failed\n", path); exit(2); }
    fseek(f, 0, SEEK_END);
    long n = ftell(f);
    fseek(f, 0, SEEK_SET);
    std::vector<uint8_t> b((size_t)n);
    if (n > 0 && fread(b.data(), 1, (size_t)n, f) != (size_t)n) { fclose(f); exit(2); }
    fclose(f);
    return b;
}

static std::string hexOf(const uint8_t* p, size_t n) {
    std::string s;
    char t[3];
    for (size_t i = 0; i < n; i++) { snprintf(t, sizeof(t), "%02x", p[i]); s += t; }
    return s;
}

static int hexVal(char c) {
    if (c >= '0' && c <= '9') return c - '0';
    if (c >= 'a' && c <= 'f') return c - 'a' + 10;
    if (c >= 'A' && c <= 'F') return c - 'A' + 10;
    return -1;
}

static bool fromHex(const std::string& s, uint8_t out[32]) {
    if (s.size() != 64) return false;
    for (int i = 0; i < 32; i++) {
        int hi = hexVal(s[i * 2]), lo = hexVal(s[i * 2 + 1]);
        if (hi < 0 || lo < 0) return false;
        out[i] = (uint8_t)((hi << 4) | lo);
    }
    return true;
}

static int sha512OfBytes(const uint8_t* p, size_t n, uint8_t out[64]) {
    mc_sha512* c = mc_sha512_new();
    if (!c) return -1;
    char eb[MC_ERRBUF_LEN];
    size_t off = 0;
    while (off < n) {
        size_t chunk = n - off;
        if (chunk > (4u << 20)) chunk = (4u << 20); /* 4MB 块，模拟生产路径 */
        if (mc_sha512_update(c, p + off, chunk, eb, sizeof(eb)) != MC_OK) { mc_sha512_free(c); return -1; }
        off += chunk;
    }
    int rc = mc_sha512_final(c, out, eb, sizeof(eb));
    mc_sha512_free(c);
    return rc;
}

int main(int argc, char** argv) {
    if (argc >= 3 && strcmp(argv[1], "hash") == 0) {
        std::vector<uint8_t> b = readAll(argv[2]);
        uint8_t h[32];
        int32_t q = 0, w = 0, hh = 0;
        char eb[MC_ERRBUF_LEN];
        int rc = mc_image_phase1(b.data(), b.size(), h, &q, &w, &hh, eb, sizeof(eb));
        if (rc != MC_OK) { fprintf(stderr, "decode/pdq failed: %s\n", eb); return 1; }
        printf("%s %d %d %d\n", hexOf(h, 32).c_str(), q, w, hh);
        return 0;
    }
    if (argc >= 4 && strcmp(argv[1], "hd") == 0) {
        uint8_t a[32], b[32];
        if (!fromHex(argv[2], a) || !fromHex(argv[3], b)) { fprintf(stderr, "bad hex\n"); return 2; }
        printf("%d\n", mc_hamming_distance(a, b));
        return 0;
    }
    if (argc >= 3 && strcmp(argv[1], "sha512str") == 0) {
        uint8_t out[64];
        if (sha512OfBytes((const uint8_t*)argv[2], strlen(argv[2]), out) != MC_OK) return 1;
        printf("%s\n", hexOf(out, 64).c_str());
        return 0;
    }
    if (argc >= 3 && strcmp(argv[1], "sha512file") == 0) {
        std::vector<uint8_t> b = readAll(argv[2]);
        uint8_t out[64];
        if (b.empty()) { /* 空文件也要能算（空输入向量） */ }
        if (sha512OfBytes(b.data(), b.size(), out) != MC_OK) return 1;
        printf("%s\n", hexOf(out, 64).c_str());
        return 0;
    }
    fprintf(stderr, "usage: mc_endtoend hash <img> | hd <hex1> <hex2> | sha512str <s> | sha512file <f>\n");
    return 2;
}
```

golden 生成（开发者执行一次并提交 `mediacore/testdata/golden/level_b.tsv`）：按上游 `pdq/cpp` README 构建参考实现 CLI，对 **`pdq/data` 官方图集全部** + 自采 ≥20 张混合图（JPEG/PNG/WebP/BMP/GIF，含小图、横竖长图）逐张取 `<hex> <quality>`，写成 TSV：`图片路径\t参考hex\t参考quality`。

`tests/run_level_b.sh`：

```bash
#!/usr/bin/env bash
set -u
FAIL=0; N=0; CHECKED=0
while IFS=$'\t' read -r img ref_hex ref_q; do
  [ -z "$img" ] && continue
  case "$img" in \#*) continue;; esac
  N=$((N+1))
  OUT=$(bin/mc_endtoend.exe hash "$img") || { echo "HASH-FAIL $img"; FAIL=1; continue; }
  our_hex=$(echo "$OUT" | awk '{print $1}')
  our_q=$(echo "$OUT" | awk '{print $2}')
  hd=$(bin/mc_endtoend.exe hd "$our_hex" "$ref_hex")
  echo "sample $img: hd=$hd ref_q=$ref_q our_q=$our_q"
  if [ "$ref_q" -ge 80 ]; then
    CHECKED=$((CHECKED+1))
    if [ "$hd" -gt 10 ]; then echo "LEVEL-B VIOLATION $img hd=$hd (>10)"; FAIL=1; fi
  fi
done < mediacore/testdata/golden/level_b.tsv
echo "level-b: $N samples, $CHECKED judged (ref quality>=80)"
if [ $FAIL -eq 0 ]; then echo "LEVEL-B PASS"; else echo "LEVEL-B FAIL"; exit 1; fi
```

#### 6.1.3 SHA-512 官方向量 + 损坏语料不崩溃

```bash
#!/usr/bin/env bash
# tests/run_sha512_and_fuzz.sh：NIST 向量 + fuzz 语料鲁棒性
set -u
FAIL=0

check() { # check <name> <got> <want>
  if [ "$2" != "$3" ]; then echo "SHA512 MISMATCH $1: got=$2 want=$3"; FAIL=1; fi
}
check "abc" "$(bin/mc_endtoend.exe sha512str abc)" \
  "ddaf35a193617abacc417349ae20413112e6fa4e89a97ea20a9eeee64b55d39a2192992a274fc1a836ba3c23a3feebbd454d4423643ce80e2a9ac94fa54ca49f"
check "empty" "$(bin/mc_endtoend.exe sha512str '')" \
  "cf83e1357eefb8bdf1542850d66d8007d620e4050b5715dc83f4a921d36ce9ce47d0d13c5d85f2b0ff8318d2877eec2f63b931bd47417a81a538327af927da3e"
head -c 1000000 /dev/zero | tr '\0' 'a' > /tmp/a1m.bin
check "1M-a" "$(bin/mc_endtoend.exe sha512file /tmp/a1m.bin)" \
  "e718483d0ce769644e2e42c7bc15b4638e1f98b13b2044285632a803afa973ebde0ff244877ea60a4cb0432ce577c31beb009c5c2c49aa2e4eadb217ad8cc09b"

# fuzz 语料：每个损坏文件都应干净地返回错误（退出码 1），绝不崩溃（139/134/0xC0000005）
for f in agent/testdata/corpus/*; do
  bin/mc_endtoend.exe hash "$f" >/dev/null 2>&1
  rc=$?
  case "$f" in
    *seed.jpg|*seed.png|*wrongext.png) [ $rc -ne 0 ] && { echo "VALID-REJECTED $f"; FAIL=1; } ;;
    *) if [ $rc -gt 1 ]; then echo "FUZZ-CRASH $f rc=$rc"; FAIL=1; fi ;;
  esac
done
echo "sha512+fuzz done"
if [ $FAIL -eq 0 ]; then echo "SHA512-FUZZ PASS"; else echo "SHA512-FUZZ FAIL"; exit 1; fi
```

### 6.2 Go 侧单元测试

| 测试 | 位置 | 断言要点 |
|---|---|---|
| `TestMessageRoundtrip` | `internal/worker/pool_test.go` | 全部消息类型 msgpack marshal/unmarshal 后字段逐一相等 |
| `TestFrameRoundtrip` | 同上 | `net.Pipe` 上 WriteEnv/ReadEnv 往返；超长帧（>16MB）被拒绝 |
| `TestDeduperSingleFlight` | 同上 | store mock 未命中；50 goroutine 同 SHA Ask：1 个 Found=false，49 个阻塞；Resolve 后 49 个收到 Found=true 且字段一致 |
| `TestDeduperOwnerCrashRetry` | 同上 | owner Ask 后 FailByJob：等待者被唤醒拿 Found=false，重试后其一成为新 owner |
| `TestDeduperStoreHit` | 同上 | 库中已有特征：Ask 直接 Found=true，无 flight 残留 |
| `TestPhase1MissingMask` | `internal/store/mask_test.go` | 表驱动：无记录/尺寸变/mtime 变/特征齐/单字段缺/crash 后补算 6 组用例 |
| `TestSavePhase1Idempotent` | `internal/store/features_test.go` | 同一结果写两次：特征行仍 1 行；files.missing_mask 清位正确；partial 不覆盖已有字段（COALESCE） |
| `TestMarkCrash` | 同上 | status=crash、error 写入、missing_mask 不变 |
| `TestThumbCache` | `internal/wproc/thumbcache_test.go` | 键稳定性（大小写/相对路径）；无 meta→miss；mtime 变→miss；一致→hit |
| `TestFixPath` | `internal/wproc/fixpath_test.go` | ≥240 字符路径加 `\\?\` 前缀；短路径不变 |
| `TestWatchdogKillsHungWorker` | `internal/worker/pool_test.go` | helper 进程握手后睡眠 60s；ImageTimeout=1s：1s 内被 Kill、crash.log reason=watchdog_image、文件 MarkCrash、新 worker Ready |
| `TestCrashRespawn` | 同上 | helper 进程握手后 `os.Exit(3)`：crash.log reason=exit_code、exit_code=3、池自动补满 |
| `TestFFprobeFFmpeg` | `internal/wproc/ffmpeg_test.go` | 对生成的 5s 视频：duration∈[4900,5100]ms；中点截图产出非空 JPEG；`tools/` 缺失时 `t.Skip` |

helper 进程模式（看门狗/崩溃测试用）：测试二进制以 `GO_WANT_HELPER_PROCESS=1` 重启自身，helper 分支里 dial 管道、发 Ready、按环境变量指示 sleep 或 exit。

### 6.3 验收用例（AC；全部通过方可勾选 M2）

#### D1 语料生成器 `agent/testdata/gen_corrupt.go`

```go
//go:build ignore

// 语料生成器：go run testdata/gen_corrupt.go <outdir>
package main

import (
	"crypto/rand"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
)

func main() {
	out := "."
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	dir := filepath.Join(out, "corpus")
	must(os.MkdirAll(dir, 0o755))

	// 有效种子图（确定性图案，可复现）
	img := image.NewRGBA(image.Rect(0, 0, 320, 240))
	for y := 0; y < 240; y++ {
		for x := 0; x < 320; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), uint8((x + y) % 256), 255})
		}
	}
	jpgPath := filepath.Join(dir, "seed.jpg")
	writeJPEG(jpgPath, img)
	writePNG(filepath.Join(dir, "seed.png"), img)

	jpgBytes, err := os.ReadFile(jpgPath)
	must(err)
	n := len(jpgBytes)

	write(filepath.Join(dir, "trunc50.jpg"), jpgBytes[:n/2])      // 截断 50%
	write(filepath.Join(dir, "trunc95.jpg"), jpgBytes[:n*95/100]) // 截断尾部
	mid := append([]byte(nil), jpgBytes...)
	for i := n / 2; i < n/2+4096 && i < n; i++ {
		mid[i] = 0
	}
	write(filepath.Join(dir, "zeroed_mid.jpg"), mid) // 中段清零
	bad := append([]byte(nil), jpgBytes...)
	copy(bad, []byte{0x00, 0x11, 0x22})
	write(filepath.Join(dir, "badmagic.jpg"), bad) // 坏 magic
	garbage := make([]byte, 4096)
	if _, err := rand.Read(garbage); err != nil {
		panic(err)
	}
	write(filepath.Join(dir, "garbage.jpg"), garbage)                          // 纯随机
	write(filepath.Join(dir, "empty.jpg"), nil)                                // 空文件
	write(filepath.Join(dir, "renamed_txt.jpg"), []byte("这不是图片 not an image\n")) // 文本改名
	write(filepath.Join(dir, "wrongext.png"), jpgBytes)                        // JPEG 内容 .png 后缀（应解码成功）

	fmt.Println("corpus written to", dir)
}

func writeJPEG(path string, img image.Image) {
	f, err := os.Create(path)
	must(err)
	defer f.Close()
	must(jpeg.Encode(f, img, &jpeg.Options{Quality: 90}))
}

func writePNG(path string, img image.Image) {
	f, err := os.Create(path)
	must(err)
	defer f.Close()
	must(png.Encode(f, img))
}

func write(path string, b []byte) { must(os.WriteFile(path, b, 0o644)) }

func must(err error) {
	if err != nil {
		panic(err)
	}
}
```

视频语料（Git Bash，一次性）：

```bash
ffmpeg -hide_banner -loglevel error -f lavfi -i testsrc=duration=5:size=640x360:rate=25 \
  -pix_fmt yuv420p -y corpus/valid5s.mp4
ffmpeg -hide_banner -loglevel error -f lavfi -i testsrc2=duration=8:size=320x240:rate=15 \
  -pix_fmt yuv420p -y corpus/valid8s.mp4
SZ=$(stat -c%s corpus/valid5s.mp4); head -c $((SZ/2)) corpus/valid5s.mp4 > corpus/trunc50.mp4
cp corpus/valid5s.mp4 corpus/copy_of_valid5s.mp4   # 同内容副本（single-flight 用）
```

#### AC-1 损坏文件投喂（对应 M2 验收"投喂损坏文件主进程存活"）

准备：`corpus/` 中 2 有效图 + 8 类损坏各复制 16 份（`for i in $(seq 1 16)`）+ 3 视频 + 1 损坏视频 ≈ 200 文件；`worker.count=4`；全新数据目录。

步骤：启动 agent.exe（记录 PID），下发对该目录的一阶段扫描，等 TaskDone。

通过标准：

- [x] agent.exe PID 不变、扫描完成（200/200 有终态）
- [x] `crash.log` **无**新增行（解码失败是错误不是崩溃）
- [x] `errors.log` 每行合法 JSON 且含 `path/stage/err`；行数 = 各损坏文件失败字段总数；同一文件不同字段失败各占一行
- [x] 有效图（含 `wrongext.png`，验证 magic 探测而非扩展名）在 `image_features` 有 `pdq256/quality/width/height`；有效视频有 `duration_ms/thumb_path/thumb_pdq256/thumb_quality`；`trunc50.mp4` 有 `failed/partial` 状态与 ffmpeg/ffprobe 错误行
- [x] `files.status` 分布：有效=`done`；损坏=`failed`（全字段败）或 `partial`（sha 成功、特征败）
- [x] 第二轮重扫：损坏文件被自动补算重试（`missing_mask` 非 0），结果一致、无重复特征行

#### AC-2 崩溃注入（对应"crash.log 有记录"）

准备：90 个有效图 + 10 个名为 `img__crash__NN.jpg` 的有效图副本；agent 启动环境含 `WORKER_CRASH_INJECTION=1`（worker 继承）；`worker.count=4`。

步骤：全量扫描 100 文件，等 TaskDone。

通过标准：

- [x] agent.exe 存活，扫描完成
- [x] `crash.log` 恰好 10 行新记录：每行 `file` 含 `__crash__`、`exit_code=-1073741819`（0xC0000005）、`reason=exit_code`、`pid` 非 0
- [x] 10 个崩溃文件 `files.status='crash'`；其余 90 个 `done` 且特征齐全
- [x] `agent.log` 中 `worker ready` 行数 = 4（初始）+ 10（重生）= 14（验证池补满、扫描未中断）
- [x] 无 WER 弹窗/挂起（总耗时与无注入基线同量级；若出现 WerFault 进程滞留即 FAIL，回查 §4.8 `suppressWERDialogs`）

#### AC-3 看门狗超时

准备：9 个有效图 + 1 个 `slow__hang__.jpg`；注入开启；`worker.image_timeout_s=30`。

通过标准：

- [x] 派发后约 30s（±3s）`crash.log` 出现一行：`reason=watchdog_image`、`file` 含 `__hang__`
- [x] 该文件 `status='crash'`；worker 重生；其余 9 个 `done`
- [x] 全程 agent 存活；总耗时 < 60s（挂起 600s 的注入被及时掐断）

#### AC-4 同 SHA single-flight（对应"同 SHA 只解码一次"）

准备：全新数据目录；100 份同一图片的不同名副本（内容唯一，库中无先验）；`worker.count=8`。

通过标准：

- [x] TaskDone 汇总行 `decode_calls=1` 且 `singleflight_hits=99`
- [x] `image_features` 该 SHA 仅 1 行；100 个 `files` 行均 `done` 且 `sha512` 相同
- [x] 视频同理：20 份同一视频副本 → `thumb_generated=1`（ffmpeg 只跑 1 次，19 次被 single-flight 拦截——注意副本路径不同，缩略图缓存键不同，排除缓存干扰）

#### AC-5 缩略图缓存（对应"缩略图按路径命中缓存"）

准备：10 个不同视频；Round1 全新数据目录全量扫描。

通过标准：

- [x] Round1：`thumb_generated=10`；`thumbcache/` 下 10 个 `.jpg` + 10 个 `.json`
- [x] Round2（清空 `video_features` 与 `files`，**保留** `thumbcache/` 与源文件）：`thumb_generated=0`、`thumb_cache_hits=10`，特征全部重新落库且与 Round1 一致
- [x] Round3：`touch` 其中 1 个视频改 mtime → 仅该文件 `thumb_generated=1`（sidecar 校验失效重新生成），其余 9 个仍 hit

#### AC-6 长路径 / Unicode / 权限

- [x] 目录含 `图片_😀 副本.jpg`（Unicode+空格+中文）→ `done`
- [x] 嵌套目录构造 >260 字符路径的有效图 → `done`（验证 `\\?\` 处理）
- [x] 只读属性有效图（`attrib +R`）→ `done`（读取不受只读影响）
- [x] 拒绝访问文件（`icacls <f> /deny Everyone:R`；若管理员仍可读，改用测试程序以无共享模式独占打开）→ `errors.log` 一行 `stage=open`、该文件 `failed`、扫描继续

#### AC-7 PDQ 回归门禁

- [x] `run_level_a.sh` → `LEVEL-A PASS`（72/72 位精确）
- [x] `run_level_b.sh` → `LEVEL-B PASS`（quality≥80 样本全部 HD≤10）
- [x] `run_sha512_and_fuzz.sh` → `SHA512-FUZZ PASS`
- [x] 三个脚本纳入 CI（或 `scripts/verify_m2.ps1` 一键执行），B/C 任务合并前必须全绿

#### AC-8 性能烟测（基线，不设硬门槛）

- [x] SSD 上 1000 张混合尺寸图片、`worker.count=8`：记录 TaskDone 汇总行的吞吐（files/s）、`decode_calls`、平均 `read_ms/decode_ms`，存入 CI 工件供 M6 对比
- [x] 扫描期间主进程 RSS 无单调增长（粗查：任务管理器/`Get-Process` 采样 3 次）

### 6.4 验收与 plan §10 M2 行的映射

| plan 验收标准 | 本文用例 |
|---|---|
| 投喂损坏文件主进程存活、crash.log 有记录 | AC-1（存活+不崩）+ AC-2（崩溃有记录）+ AC-3（看门狗） |
| 同 SHA 只解码一次 | AC-4 |
| 缩略图按路径命中缓存 | AC-5 |

---

## 7. 风险与注意事项

1. **PDQ 移植质量（plan §11 已列，本文给硬门禁）**：Level A 位精确回归（§6.1.1）是通过接入的前置条件，任何 MISMATCH 不得带债合并。最易踩的坑是位序（`w[15]→w[0]` 大端导出，§4.3）与浮点编译选项（禁 `/fp:fast`）。上游 commit 必须 pin 住并记录在 `pdq_upstream/COMMIT`，升级上游是一次独立评审。
2. **双二进制已由架构计划 v1.2 确认**（§0）：`agent.exe`(CGO=0) + `worker.exe`(cgo)，不得退回会让主进程启动即加载 DLL 的单 exe cgo 方案。
3. **u8 灰度面是跨里程碑决策**：M4 的分区 pHash / Sobel 将复用 `mc_decode_gray` 产出的同一 u8 灰度面（DLL 内扩导出函数即可，ABI 向后兼容）。M4 设计时不得改为 float 面，否则解码路径要重做。PDQ 内部 u8→float 转换用上游 `fillFloatLumaFromGrey`，与参考实现对齐。
4. **worker 被杀时的孤儿 ffmpeg**：看门狗 Kill worker 后，其 ffmpeg 孙进程可能短暂残留（无 ctx 取消方）。单帧截图通常秒级结束，风险可控；`tmp-<pid>` 临时文件会在下次缓存未命中时清理。根治方案（Job Object `KILL_ON_JOB_CLOSE`）列入 M6 调优，本里程碑不阻塞。
5. **"毒文件"跨轮崩溃循环**：可靠令 DLL 崩溃的文件每轮重扫都会杀一个 worker（`status=crash` 下轮自动补算）。单轮内不循环（崩溃文件当轮不重派），跨轮有界（每轮至多杀一次/文件）。如出现真实毒文件语料，加"连续 N 轮 crash 则拉黑"计数器——列入 M6 评估，plan 当前语义保留。
6. **WER 弹窗挂起**：未抑制时 worker 崩溃会弹 WerFault 并挂住进程，看门狗语义被破坏。worker 入口必须第一行调 `suppressWERDialogs()`（§4.8），AC-2 含验证。域控环境若有 WER 组策略（如 `DontShowUI` 被改为 0）需回归此用例。
7. **ffmpeg 快速 seek 精度**：`-ss` 在 `-i` 前是关键帧级定位，中点帧可能落在最近关键帧而非精确中点——对缩略图 PDQ 无影响（内容语义不变），可接受。**注意 M4 的 6 帧校验若要求精确帧位置，须改为输出侧精确 seek（`-ss` 放 `-i` 后）**，届时单独评估性能。
8. **缩略图缓存的边界**：校验键是 `mtime+size`（plan 语义），改内容但精确保留 mtime/size 的极端情况会漏检，可接受。缓存无容量上限，百万视频 ≈ 每个缩略图 ~10KB + sidecar，约 20GB 级别——容量治理（LRU/上限）列入 M6；`thumbcache` 目录删除即重建，无状态风险。
9. **cgo/CRT 纪律**：DLL 用 `x64-windows-static`（静态 CRT），除 bcrypt 外无外部 DLL 依赖；跨边界内存严格"谁分配谁释放"（`mc_free_image`）；DLL 函数不保留 Go 传入指针（同步使用即返回）。新增导出函数必须同步 `exports.def` 并走 dlltool 重新生成 `libmediacore.a`，否则 Go 侧链接报 `undefined reference`。
10. **single-flight 等待计入看门狗**：worker 等 `sha_reply`（flight 排队）的时间算在其单文件 30s/120s 预算内。同 SHA 大批量 + owner 解码极慢（如 200MB PNG）的叠加场景理论上有误杀等待者的可能；等待者被杀后文件标 crash 下轮补算，结果正确性不受影响，概率极低，接受。
11. **Quality 只记录不裁剪**：上游建议 quality ≤49 的哈希丢弃（M3 一筛剪枝用），M2 对所有图都计算并落库，不做任何阈值过滤。
12. **`modernc.org/sqlite` 与 UPSERT**：`ON CONFLICT DO UPDATE` 需 SQLite ≥3.24（modernc 内建版本满足）；所有写路径（`SavePhase1`/`MarkCrash`）仅在 agent.exe 主进程，worker 不碰 SQLite——崩溃不会损伤 WAL。
13. **命名管道安全**：管道名含父 PID + 纳秒 nonce，本机同用户才可猜中；如需进一步隔离可在 `winio.PipeConfig.SecurityDescriptor` 加 SDDL（限当前用户），本里程碑默认不加。
14. **构建环境钉版**：mingw-w64 gcc、vcpkg（manifest baseline）、ffmpeg 版本进 `agent/scripts/build.ps1` 头部注释；ffmpeg/ffprobe 随包分发、不依赖目标机 PATH。
