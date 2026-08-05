# VideoCore 原生 FFmpeg 迁移实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 用动态链接 FFmpeg 库的 `videocore.dll` 完全替代 `ffmpeg.exe`、`ffprobe.exe` 与旧 `mediacore.dll`，让每个媒体文件在一个原生 session、一个 Windows 文件句柄和一次统一分析调用中完成 SHA-512、图片或视频特征、六帧采样及 3×2 联系表生成。

**Architecture:** Worker 通过版本化 `vc_*` C ABI 调用唯一原生 DLL；VideoCore 静态包含现有 SHA/PDQ/pHash/Sobel 算法，动态加载应用目录中的 FFmpeg DLL。Worker 先在同一 session 顺序计算 SHA-512，再向 Agent 查询内容缓存，仅把缺失字段和缺失帧交给一次 `vc_media_analyze`。VideoCore 只写调用方提供的临时 JPEG，Go 负责 `vc-grid-v1` 内容寻址、sidecar、漂移复核和原子发布；Agent 用一次数据库事务持久化合并结果。

**Tech Stack:** C++17、Windows `CreateFileW`/自定义 `AVIOContext`、FFmpeg libavformat 63/libavcodec 63/libavutil 61/libswscale 10、CMake 4.2.3、Visual Studio 2022 x64、vcpkg `x64-windows-static`、MinGW CGO 导入库、Go 1.22、MessagePack、SQLite/PostgreSQL 同步链路、PowerShell 验收脚本。

## Global Constraints

- 权威行为设计是 `docs/superpowers/specs/2026-07-31-videocore-ffmpeg-library-design.md`；实现不得通过计划中的简写弱化该设计。
- 最终仓库、构建 staging 和发布包只能保留 `videocore.dll` 与 `vc_*` ABI；不得保留 `mediacore.dll`、`libmediacore.a`、旧 `mc_*` 符号或旧原生工程。
- 最终生产路径不得启动 `ffmpeg.exe`、`ffprobe.exe`、`ffplay.exe` 或其他媒体解码子进程；Worker 进程管理本身仍可由 Agent 启停。
- FFmpeg 使用动态链接，实际递归 DLL 闭包与 `worker.exe`、`videocore.dll` 同目录；不得依赖系统 PATH 或机器上另装的 FFmpeg。
- SHA-512、图片解码、PDQ、9 分区 pHash、128 维 Sobel 现有行为必须逐字节兼容；不得借迁移修改算法、阈值或字节序。
- 视频固定采样 `{1,3,5,7,9,11}/12` 六个位置；联系表固定为灰度 3 列×2 行，无额外中点帧。
- 单文件只允许一次 `CreateFileW`；视频一次 analyze 只允许一个 `AVFormatContext`。普通 codec 恢复可重建 codec context，但不得重新打开文件。
- `vc_media_hash` 必须先于缓存查询；只有缺失字段或帧才进入 `vc_media_analyze`，且每个任务最多调用 analyze 一次。
- VideoCore 不决定最终缓存路径；Go 将 JPEG 发布到 `<thumb.cache_dir>\vc-grid-v1\<sha前2位>\<完整小写sha>.jpg` 并写 `<jpg>.json` sidecar。
- `thumb.cache_dir` 默认 `<data_dir>\thumbcache`；相对路径按 `data_dir` 解析，且必须拒绝与任一扫描根相等、互为父目录或子目录。
- 取消、超时或 stale 结果不得发布 SHA、联系表或部分特征；单帧普通失败可保留其他成功帧并用确定性占位格。
- 默认探测超时 15 秒、单帧超时 20 秒、Worker 视频硬看门狗 120 秒；最终 24 小时驻留不得用短跑或历史 M6 证据替代。
- 不得输出或写入 PostgreSQL DSN、密码或其他凭据；验收脚本只从 `$env:FS_PG_DSN` 读取并对日志脱敏。
- 本工作区当前没有 `.git` 元数据。每个任务仍给出 Git-backed checkout 的建议提交信息；在当前工作区执行时，改为在证据目录记录文件清单、命令、退出码和结果，不得初始化仓库或声称已提交。
- 删除旧链路是后置、不可逆的迁移步骤：只有旧 golden 已冻结、新路径兼容差分为零、Worker 恢复测试通过后才执行。

---

## File Map

### 新原生工程

- Create `videocore/CMakeLists.txt` — DLL、测试、FFmpeg 显式链接与运行时闭包。
- Create `videocore/vcpkg.json` — 静态图片依赖清单。
- Create `videocore/exports.def` — 唯一允许的 10 个 `vc_*` 导出。
- Create `videocore/include/videocore/videocore.h` — ABI 1 公共 C 头。
- Create `videocore/src/api.cpp` — C ABI 边界、结构校验和异常翻译。
- Create `videocore/src/error.{h,cpp}` — 稳定状态码与诊断文本。
- Create `videocore/src/runtime_info.{h,cpp}` — VideoCore/FFmpeg build/runtime 版本。
- Create `videocore/src/cancel_token.{h,cpp}` and `deadline.{h,cpp}` — 跨线程取消与单调截止时间。
- Create `videocore/src/win_file.{h,cpp}` — 单 `CreateFileW` 句柄、身份、大小和 mtime。
- Create `videocore/src/avio_bridge.{h,cpp}` — FFmpeg 自定义 read/seek/interrupt。
- Create `videocore/src/media_session.{h,cpp}` — session 生命周期、顺序 SHA 与 busy guard。
- Create `videocore/src/image_analysis.{h,cpp}` — 图片单次解码分析。
- Create `videocore/src/video_analysis.{h,cpp}` — 单 format context、六帧解码与帧特征。
- Create `videocore/src/contact_sheet.{h,cpp}` — 3×2 灰度联系表、占位格和 JPEG。
- Create `videocore/src/native_algorithms/**` — 从旧工程静态迁移 SHA/PDQ/pHash/Sobel。
- Copy `mediacore/src/pdq_upstream/**` to `videocore/src/pdq_upstream/**` byte-for-byte.
- Copy `mediacore/third_party/stb/**` to `videocore/third_party/stb/**` byte-for-byte.
- Create `videocore/tests/test_*.cpp` and `videocore/testdata/**` — ABI、兼容、媒体和韧性测试。

### Go、协议、缓存与持久化

- Modify `internal/proto/message.go` and `internal/proto/conn_test.go` — 追加字段/帧掩码兼容。
- Modify `internal/worker/messages.go` and `messages_test.go` — Ready、SHA 查询和合并结果。
- Create `internal/wproc/videocore/**` — CGO/stub 绑定与 Go 类型。
- Create `internal/store/content.{go,_test.go}` — 内容缓存 present/missing 查询。
- Create `internal/store/analysis.{go,_test.go}` — 合并结果单事务提交。
- Modify `internal/store/ddl.go`, `db.go`, `features.go`, `phase2.go`, `syncq.go` and tests.
- Create `internal/wproc/contact_sheet_cache.{go,_test.go}` — `vc-grid-v1` 路径、sidecar 和原子发布。
- Create `internal/wproc/pipeline_session.{go,_test.go}` — 单 session 合并管线。
- Modify `internal/wproc/pipeline.go`, `run.go` and tests.
- Modify `internal/worker/deduper.go`, `pool.go`, `supervisor.go` and tests.
- Modify `internal/config/agent.go`, `internal/wproc/config.go` and tests.

### 构建、供应链与验收

- Create `third_party/ffmpeg/manifest.schema.json`, `manifest.json`, `LICENSE.txt`, `NOTICE.md`, `SOURCE.md`.
- Modify `scripts/build.ps1` and `scripts/test-cgo.ps1`.
- Create `scripts/resolve_native_dependencies.ps1`.
- Create `scripts/verify_videocore_{native,supply_chain,compat,acceptance,static}.ps1`.
- Create `scripts/run_videocore_short_benchmark.ps1`, `run_videocore_soak.ps1`, `audit_videocore_soak.ps1`.
- Create `scripts/verify_videocore.ps1`.
- Create focused `integration/videocore_*_test.go`.
- Create `testdata/videocore/compat/**` and `testdata/videocore/acceptance/**`.
- Modify `scripts/verify_m3.ps1`, `scripts/verify_m4.ps1`, and `integration/m4_e2e_test.go`.
- Modify `README.md`, `docs/todolist.md`.
- Create `docs/acceptance/2026-07-31-videocore.md`.

### 最终删除项

- Delete `mediacore/`.
- Delete `internal/wproc/mediacore/`.
- Delete `internal/wproc/ffmpeg.go`, `ffmpeg_test.go`.
- Delete `internal/wproc/video_frames.go`, `video_frames_test.go`, `video_frames_integration_test.go`.
- Delete superseded `pipeline_video*` and `pipeline_phase2*` files after coverage moves.
- Delete `scripts/verify_m2.ps1`, `scripts/verify_m2_native.ps1`.
- Delete `integration/m2_e2e_test.go`, `integration/m2_acceptance_test.go`.
- Delete `bin/mediacore.dll`, `bin/tools/`, `third_party/ffmpeg/bin/ffmpeg.exe`, `ffprobe.exe`, `ffplay.exe`.

## Dependency Order

```text
旧结果/fixture 冻结 ──────────────────────────────────────────────┐
供应链契约 ────────────────────────────────────────────────┐      │
ABI → runtime/cancel → 单句柄/SHA → 图片算法 → 图片分析   │      │
                                      └→ 视频六帧 → 联系表 → 原生韧性/闭包
协议掩码 ───────────────┬→ 内容缓存查询 ───────┐           │      │
                        └→ 合并事务 ───────────┼→ 单 session 管线
原生 ABI/导入库 → CGO ─────────────────────────┘           │
联系表缓存 ────────────────────────────────────────────────┘
单 session 管线 + 合并事务 → Supervisor/Ready → 新旧差分/动态验收
新旧差分=0 + 动态验收通过 → 删除旧链路/启用静态门禁
完整新包 → 短基准 → 24h 驻留 → README/验收报告/总门禁
```

---

### Task 1: 冻结旧实现兼容基线与二进制 fixture

**Files:**
- Create: `testdata/videocore/compat/manifest.json`
- Create: `testdata/videocore/compat/legacy-golden.json`
- Create: `testdata/videocore/compat/images/*`
- Create: `testdata/videocore/compat/videos/*`
- Create: `scripts/capture_videocore_legacy_golden.ps1`
- Create: `integration/videocore_compat_fixture_test.go`

**Interfaces:**
- `manifest.json` 固定每个 fixture 的相对路径、SHA-256、媒体类型、codec、duration、rotation、SAR 与预期场景标签。
- `legacy-golden.json` 固定 SHA-512、图片 PDQ/quality/pHash/Sobel，以及视频 duration、六个标准采样时间、旧实现选帧身份、显示尺寸与逐帧特征。
- fixture 至少覆盖 JPEG/PNG/WebP、普通 H.264、B 帧、90° 旋转、非 1:1 SAR、短视频、竖屏、VP9、HEVC、纯音频、截断容器和损坏 packet。
- 本任务必须在旧 `mediacore` 与 FFmpeg EXE 仍可运行时完成；golden 生成后只允许人工审查修正元数据，不允许用新实现覆盖。

- [ ] **Step 1: 写 fixture 完整性红测**

```go
func TestVideoCoreCompatibilityFixturesAreImmutable(t *testing.T) {
    manifest := loadCompatManifest(t)
    if len(manifest.Images) < 3 || len(manifest.Videos) < 9 {
        t.Fatal("compat fixture coverage is incomplete")
    }
    verifyAllSHA256(t, manifest)
    verifyGoldenCoversEveryFixture(t, manifest)
}
```

- [ ] **Step 2: 运行红测**

```powershell
go test -count=1 ./integration -run '^TestVideoCoreCompatibilityFixturesAreImmutable$'
```

Expected: `FAIL`，因为兼容清单和 golden 尚不存在。

- [ ] **Step 3: 选取或提交最小合法媒体 fixture，并写入 SHA-256 清单**

不得从 `I:\tmp`、`H:\pik\00000000000`、`G:\pik` 或 `D:\webdev` 复制真实媒体；只使用可再分发的合成 fixture。

- [ ] **Step 4: 用旧实现一次性生成 golden**

```powershell
pwsh -NoProfile -File .\scripts\capture_videocore_legacy_golden.ps1 `
  -Manifest .\testdata\videocore\compat\manifest.json `
  -OutFile .\testdata\videocore\compat\legacy-golden.json `
  -LegacyBinDir .\bin
```

Expected: 输出 fixture 数、旧组件版本、结果哈希；不改输入文件。

- [ ] **Step 5: 人工抽查并锁定 golden**

检查采样顺序、旋转后尺寸、SAR、PDQ 字节序、pHash 9 段顺序和 Sobel float 位模式。

- [ ] **Step 6: 运行绿测并记录检查点**

```powershell
go test -count=1 ./integration -run '^TestVideoCoreCompatibilityFixturesAreImmutable$'
```

Expected: `PASS`。

**Checkpoint:** 保存 manifest/golden SHA-256、生成命令和旧版本信息。

**Git-backed commit:** `test: freeze legacy media compatibility goldens`

---

### Task 2: 固定 FFmpeg 供应链与再分发门禁

**Files:**
- Create: `third_party/ffmpeg/manifest.schema.json`
- Create: `third_party/ffmpeg/manifest.json`
- Create: `third_party/ffmpeg/LICENSE.txt`
- Create: `third_party/ffmpeg/NOTICE.md`
- Create: `third_party/ffmpeg/SOURCE.md`
- Create: `scripts/verify_videocore_supply_chain.ps1`
- Create: `integration/videocore_supply_chain_test.go`

**Interfaces:**
- manifest 必须记录 `sdk_id=N-125444-g6d72600a30-20260703`、权威来源 URL、精确 commit/version、完整 configure flags、源码归档 SHA-256、每个头文件/`.lib`/`.dll.a`/运行 DLL 的 SHA-256、组件版本、许可证分类和 `redistributable`。
- `verify_videocore_supply_chain.ps1 -Mode Local|Release`：Local 可在来源证据不完整时报告 BLOCKED；Release 必须硬失败，直到许可证和再分发证据完整。
- 验证器必须重新流式计算文件哈希，不能信任 manifest 自报值。

- [ ] **Step 1: 写缺字段、哈希漂移、漏列 DLL 和未知许可证红测**

```powershell
go test -count=1 ./integration -run '^TestVideoCoreSupplyChain'
```

Expected: `FAIL`，报告 manifest/verifier 缺失。

- [ ] **Step 2: 写 JSON schema 和最小验证器**

验证器拒绝 `nonfree`、缺少源码说明、运行 DLL 未闭包列举和重复文件名。

- [ ] **Step 3: 只录入经来源方确认的数据**

若来源 URL、configure flags、源码归档哈希或许可证状态无法从当前 SDK 证明，将 `redistributable` 保持为 `false` 并记录阻断原因；禁止猜测。

- [ ] **Step 4: 运行 Local 绿测**

```powershell
pwsh -NoProfile -File .\scripts\verify_videocore_supply_chain.ps1 `
  -Manifest .\third_party\ffmpeg\manifest.json `
  -FFmpegRoot .\third_party\ffmpeg `
  -Mode Local `
  -Evidence .\artifacts\evidence\videocore-supply-local.json
```

Expected: 文件完整性通过；若权威再分发证据未补齐，明确输出 `RELEASE BLOCKED` 而不是 `PASS`。

- [ ] **Step 5: 取得权威证据后运行 Release 门禁**

```powershell
go test -count=1 ./integration -run '^TestVideoCoreSupplyChain'
pwsh -NoProfile -File .\scripts\verify_videocore_supply_chain.ps1 `
  -Manifest .\third_party\ffmpeg\manifest.json `
  -FFmpegRoot .\third_party\ffmpeg `
  -Mode Release `
  -Evidence .\artifacts\evidence\videocore-supply-release.json
```

Expected: `VIDEOCORE FFMPEG REDISTRIBUTION PASS`；在此之前不得发布安装包。

**Checkpoint:** 保存 schema 校验结果、组件版本和逐文件哈希。

**Git-backed commit:** `build: pin FFmpeg provenance and redistribution evidence`

---

### Task 3: 建立 `videocore` 工程与精确 ABI 1

**Files:**
- Create: `videocore/CMakeLists.txt`
- Create: `videocore/vcpkg.json`
- Create: `videocore/exports.def`
- Create: `videocore/include/videocore/videocore.h`
- Create: `videocore/src/api.cpp`
- Create: `videocore/src/error.h`
- Create: `videocore/src/error.cpp`
- Create: `videocore/tests/test_abi.cpp`

**Interfaces:**
- 唯一导出：`vc_abi_version`, `vc_version`, `vc_runtime_info`, `vc_cancel_create`, `vc_cancel_request`, `vc_cancel_free`, `vc_media_open_w`, `vc_media_hash`, `vc_media_analyze`, `vc_media_close`。
- `VC_ABI_VERSION=1`，`VC_VERSION_STRING="1.0.0"`，`VC_CALL=__cdecl`。
- 固定尺寸：SHA 64、PDQ 32、pHash 9、Sobel 128、帧槽 6、全帧掩码 `0x3f`。
- 状态码必须逐项匹配设计文档的 `0/-1..-13/-99`。
- 因 C 普通标识符冲突，类型写成 `struct vc_runtime_info`，函数保留 `vc_runtime_info(...)`，不得定义同名 typedef。

```c
struct vc_runtime_info {
    uint32_t struct_size;
    uint32_t abi_version;
    char videocore_version_utf8[32];
    char ffmpeg_build_id_utf8[64];
    uint32_t avformat_header_version;
    uint32_t avformat_runtime_version;
    uint32_t avcodec_header_version;
    uint32_t avcodec_runtime_version;
    uint32_t avutil_header_version;
    uint32_t avutil_runtime_version;
    uint32_t swscale_header_version;
    uint32_t swscale_runtime_version;
};
```

- [ ] **Step 1: 写 ABI 布局和错误结构测试**

测试 `struct_size/abi_version`、过小结构、非零保留位、错误消息 NUL 结尾、固定数组尺寸和 `__cdecl` 可链接性。

- [ ] **Step 2: 运行红测**

```powershell
& 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\cmake.exe' `
  -S videocore -B videocore\build -G 'Visual Studio 17 2022' -A x64 `
  '-DCMAKE_TOOLCHAIN_FILE=C:/vcpkg/scripts/buildsystems/vcpkg.cmake' `
  -DVCPKG_TARGET_TRIPLET=x64-windows-static `
  "-DVC_FFMPEG_ROOT=$((Resolve-Path 'third_party\ffmpeg').Path -replace '\\','/')"
& 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\cmake.exe' `
  --build videocore\build --config Release --target test_vc_abi
```

Expected: 链接失败，缺少 ABI 符号。

- [ ] **Step 3: 写公共头和安全失败壳**

所有可扩展结构前两字段固定；公共结构不得出现 `bool`、C++ enum、STL、异常、FFmpeg 类型或拥有所有权的 C++ 指针。

- [ ] **Step 4: 在 ABI 边界捕获异常并统一填充 `vc_error`**

`std::bad_alloc` 映射 `VC_ERR_OOM`，其他异常映射 `VC_ERR_INTERNAL`；消息始终 NUL 结尾。

- [ ] **Step 5: 运行绿测**

```powershell
& 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\ctest.exe' `
  --test-dir videocore\build -C Release -R '^videocore_abi$' --output-on-failure
```

Expected: `100% tests passed`。

**Checkpoint:** 记录公共头 SHA-256、ABI 布局输出和导出定义。

**Git-backed commit:** `feat(videocore): establish versioned C ABI`

---

### Task 4: 实现 Runtime Info、取消令牌和截止时间

**Files:**
- Create: `videocore/src/runtime_info.{h,cpp}`
- Create: `videocore/src/cancel_token.{h,cpp}`
- Create: `videocore/src/deadline.{h,cpp}`
- Modify: `videocore/src/api.cpp`
- Modify: `videocore/CMakeLists.txt`
- Create: `videocore/tests/test_runtime_info.cpp`
- Create: `videocore/tests/test_cancel.cpp`

**Interfaces:**
- `vc_runtime_info` 返回 VideoCore 版本、FFmpeg build ID、四组件 header/runtime 版本。
- CMake 用绝对路径链接 `avformat.lib`, `avcodec.lib`, `avutil.lib`, `swscale.lib`；不得使用 `link_directories`。
- token 内部为原子取消位和引用计数；session 持有引用，调用方提前 free 不得悬垂。
- 显式取消和超时同时成立时返回 `VC_ERR_CANCELLED`。

```cpp
int32_t CheckInterrupt(const CancelState* state, Deadline deadline) noexcept;
void SetError(vc_error* out, int32_t code, int32_t ffmpeg_code,
              uint32_t win32_code, const char* message_utf8) noexcept;
```

- [ ] **Step 1: 写 runtime major 错配与跨线程取消红测**
- [ ] **Step 2: 运行红测**

```powershell
& 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\ctest.exe' `
  --test-dir videocore\build -C Release -R 'videocore_(runtime|cancel)' --output-on-failure
```

Expected: runtime/cancel 测试缺失或断言失败。

- [ ] **Step 3: 实现 header/runtime 版本采集与主版本门禁**

分别调用 `avformat_version()`、`avcodec_version()`、`avutil_version()`、`swscale_version()` 和 `av_version_info()`。

- [ ] **Step 4: 实现无锁取消和 `steady_clock` 截止时间**

取消请求不得分配内存；重复 request/free 安全；假时钟可确定测试 timeout。

- [ ] **Step 5: 将运行 DLL 复制到测试程序同级后运行绿测**

```powershell
& 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\ctest.exe' `
  --test-dir videocore\build -C Release -R 'videocore_(runtime|cancel)' --output-on-failure
```

Expected: build/runtime 主版本一致；注入 `63/62` 时返回 `VC_ERR_ABI`。

**Checkpoint:** 保存运行组件版本和错配测试证据。

**Git-backed commit:** `feat(videocore): add runtime gates and cancellation`

---

### Task 5: 实现 UTF-16 单句柄 session、自定义 AVIO 与 SHA-512

**Files:**
- Create: `videocore/src/win_file.{h,cpp}`
- Create: `videocore/src/avio_bridge.{h,cpp}`
- Create: `videocore/src/media_session.{h,cpp}`
- Create: `videocore/src/native_algorithms/sha512.{h,cpp}`
- Modify: `videocore/src/api.cpp`
- Create: `videocore/tests/test_media_session.cpp`
- Create: `videocore/tests/test_unicode_paths.cpp`

**Interfaces:**

```cpp
struct FileIdentity {
    uint64_t volume_serial;
    uint64_t file_id_high;
    uint64_t file_id_low;
};

int ReadPacket(void* opaque, uint8_t* buffer, int size);
int64_t SeekPacket(void* opaque, int64_t offset, int whence);
```

- `vc_media_open_w` 接收 UTF-16 code unit 和显式长度，拒绝空路径、内嵌 NUL 和 URL，支持 UNC 与 `\\?\` 长路径。
- `CreateFileW` 每 session 只调用一次；`AVSEEK_SIZE` 返回文件大小，其他 seek 使用 `SetFilePointerEx`。
- SHA 前 seek 0，流式读至 EOF；图片可在 hash 时按 `image_max_bytes` 同步收集字节，视频不得整体缓存。

- [ ] **Step 1: 写 SHA 标准向量、open 计数、AVIO seek 和 Unicode 路径红测**
- [ ] **Step 2: 运行红测**

```powershell
& 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\ctest.exe' `
  --test-dir videocore\build -C Release -R 'videocore_(session|unicode)' --output-on-failure
```

Expected: `vc_media_open_w` 仍返回安全壳错误。

- [ ] **Step 3: 实现 WinFile RAII、身份与元数据快照**
- [ ] **Step 4: 迁移 CNG SHA-512 分块逻辑并缓存一次结果**
- [ ] **Step 5: 实现 AVIO read/seek/size，并在阻塞边界检查取消**
- [ ] **Step 6: 运行绿测**

```powershell
& 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\ctest.exe' `
  --test-dir videocore\build -C Release -R 'videocore_(session|unicode)' --output-on-failure
```

Expected: 每个 session `CreateFileW=1`；emoji、空格和长路径通过；内嵌 NUL 失败。

**Checkpoint:** 保存 open/read/seek 计数；UNC 实机缺失时记录 `NOT_RUN`，不得写 PASS。

**Git-backed commit:** `feat(videocore): add UTF-16 single-handle sessions`

---

### Task 6: 逐字节迁移图片算法

**Files:**
- Create: `videocore/src/native_algorithms/gray_image.h`
- Create: `videocore/src/native_algorithms/image_decode.{h,cpp}`
- Create: `videocore/src/native_algorithms/pdq.{h,cpp}`
- Create: `videocore/src/native_algorithms/phash_parts.{h,cpp}`
- Create: `videocore/src/native_algorithms/sobel_hist.{h,cpp}`
- Create: `videocore/src/native_algorithms/stb_impl.cpp`
- Copy: `mediacore/src/pdq_upstream/**` → `videocore/src/pdq_upstream/**`
- Copy: `mediacore/third_party/stb/**` → `videocore/third_party/stb/**`
- Create: `videocore/tests/test_image_compat.cpp`
- Copy: `mediacore/testdata/luma/**` → `videocore/testdata/luma/**`
- Copy: `mediacore/testdata/level_b/**` → `videocore/testdata/level_b/**`

**Interfaces:**

```cpp
struct GrayImage {
    int32_t width;
    int32_t height;
    int32_t stride;
    std::vector<uint8_t> pixels;
};

struct ImageFeatures {
    std::array<uint8_t, 32> pdq;
    int32_t quality;
    std::array<uint64_t, 9> phash_parts;
    std::array<float, 128> sobel_hist;
};
```

- [ ] **Step 1: 用 Task 1 golden 写逐字节兼容红测**

损坏输入必须失败且清零输出；Sobel 按 float 原始位比较。

- [ ] **Step 2: 运行红测**

```powershell
& 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\ctest.exe' `
  --test-dir videocore\build -C Release -R '^videocore_image_compat$' --output-on-failure
```

Expected: 算法未迁移或 golden 不匹配。

- [ ] **Step 3: 原样复制上游 PDQ、stb 代码和许可证**
- [ ] **Step 4: 原样迁移灰度、PDQ、96×96 九分区 pHash、128 维 Sobel**

整数 BT.601 固定为 `(77R + 150G + 29B + 128) >> 8`；只改命名空间和数据适配。

- [ ] **Step 5: 固定 C++17 与 `/fp:precise` 并运行绿测**

```powershell
& 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\ctest.exe' `
  --test-dir videocore\build -C Release -R '^videocore_image_compat$' --output-on-failure
```

Expected: 所有 fixture 逐字节一致；差异输出 fixture、字段、旧值和新值十六进制。

**Checkpoint:** 保存兼容汇总和第三方许可证复制校验。

**Git-backed commit:** `feat(videocore): migrate image features byte-for-byte`

---

### Task 7: 实现图片单次解码分析

**Files:**
- Create: `videocore/src/image_analysis.{h,cpp}`
- Modify: `videocore/src/media_session.cpp`
- Modify: `videocore/src/api.cpp`
- Create: `videocore/tests/test_image_analysis.cpp`

**Interfaces:**
- `vc_media_analyze` 在图片路径上从 Task 5 收集的 bounded bytes 解码一次，从同一灰度图计算全部请求特征。
- 未请求字段保持 `VC_ITEM_NOT_REQUESTED`；已完成字段显式写入 fulfilled mask。
- hash 未完成时 analyze 返回 `VC_ERR_INVALID_ARG`，图片 analyze 不再次读取文件。

- [ ] **Step 1: 写一次 decode、部分 mask 和 hash 前调用红测**
- [ ] **Step 2: 运行红测**

```powershell
& 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\ctest.exe' `
  --test-dir videocore\build -C Release -R '^videocore_image_analysis$' --output-on-failure
```

Expected: `vc_media_analyze` 返回未支持。

- [ ] **Step 3: 实现图片分支和解码计数注入点**
- [ ] **Step 4: 明确填写 state/error/fulfilled mask**
- [ ] **Step 5: 运行绿测**

```powershell
& 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\ctest.exe' `
  --test-dir videocore\build -C Release -R '^videocore_image_analysis$' --output-on-failure
```

Expected: 同时请求 PDQ/pHash/Sobel 时 decode 计数严格为 1。

**Checkpoint:** 保存 decode/read 计数与 mask 结果。

**Git-backed commit:** `feat(videocore): analyze images with one decode`

---

### Task 8: 实现单 FFmpeg session 的视频探测与六帧特征

**Files:**
- Create: `videocore/src/video_analysis.{h,cpp}`
- Create: `videocore/tests/test_video_analysis.cpp`
- Copy selected fixtures from `testdata/videocore/compat/videos/` to `videocore/testdata/video/`.
- Create: `videocore/testdata/video/manifest.json`
- Modify: `videocore/CMakeLists.txt`

**Interfaces:**
- 每次 `vc_media_analyze` 只创建一个 `AVFormatContext`，将 Task 5 的 `AVIOContext` 设为 `pb`，启用 `AVFMT_FLAG_CUSTOM_IO` 和 interrupt callback。
- 一个 codec context 顺序 seek/decode；普通 codec 错误可重建 codec context，format context 失效则保留已成功帧并标记剩余帧失败。
- 采样点按防溢出公式计算：

```cpp
const int numerators[6] = {1, 3, 5, 7, 9, 11};
const int64_t q = duration_ms / 12;
const int64_t r = duration_ms % 12;
const int64_t sample_ms = q * numerator + (r * numerator) / 12;
```

- `FrameMask==0` 在原生请求入口归一化为 `0x3f`；其他值只解码置位槽。
- 应用 display matrix 旋转和 SAR 后，用 swscale 得到不裁剪的灰度帧，再调用 Task 6 特征算法。

- [ ] **Step 1: 写采样、mask、B 帧、旋转、SAR、短视频和损坏输入红测**
- [ ] **Step 2: 运行红测**

```powershell
& 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\ctest.exe' `
  --test-dir videocore\build -C Release -R '^videocore_video_analysis$' --output-on-failure
```

Expected: duration/frame 未实现。

- [ ] **Step 3: 实现自定义 AVIO 探测与最佳视频流选择**

纯音频和无视频流返回 `VC_ERR_NO_FRAME`；探测超过 deadline 返回 `VC_ERR_TIMEOUT`。

- [ ] **Step 4: 实现六个升序 seek/decode 槽和部分失败状态**
- [ ] **Step 5: 实现旋转/SAR 校正、灰度转换和逐帧特征**
- [ ] **Step 6: 运行绿测**

```powershell
& 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\ctest.exe' `
  --test-dir videocore\build -C Release -R '^videocore_video_analysis$' --output-on-failure
```

Expected: 一个 `AVFormatContext`、一个文件 HANDLE；时间戳、显示尺寸和特征与 manifest 一致。

**Checkpoint:** 保存 format/codec/open 计数与逐 fixture diff。

**Git-backed commit:** `feat(videocore): decode six deterministic video samples`

---

### Task 9: 生成确定性 3×2 灰度联系表

**Files:**
- Create: `videocore/src/contact_sheet.{h,cpp}`
- Modify: `videocore/src/video_analysis.cpp`
- Create: `videocore/tests/test_contact_sheet.cpp`

**Interfaces:**

```cpp
struct ContactSheetResult {
    int32_t state;
    uint32_t successful_mask;
    uint32_t placeholder_mask;
    int32_t width;
    int32_t height;
    int32_t tile_width;
    int32_t tile_height;
    ImageFeatures features;
};
```

- 最长边缩放至 `tile_max_side`，另一边整数舍入且至少 1；统一 tile 后画布严格 `3w×2h`，行优先、无间距、不裁剪。
- 缺帧占位格：背景 luma 96、X 为 luma 192，最短边小于 64 时线宽 1，否则 2。
- 部分缺帧返回 partial；六帧全失败返回 `VC_ERR_NO_FRAME` 且不写 JPEG。
- 从合成灰度画布计算联系表 PDQ/quality；JPEG 质量固定为命名常量并由 fixture 锁定。
- VideoCore 只写请求中的 UTF-16 临时路径，返回前关闭文件，不生成最终路径或 sidecar。

- [ ] **Step 1: 写行优先布局、占位像素和重复确定性红测**
- [ ] **Step 2: 运行红测**

```powershell
& 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\ctest.exe' `
  --test-dir videocore\build -C Release -R '^videocore_contact_sheet$' --output-on-failure
```

Expected: 联系表函数缺失。

- [ ] **Step 3: 实现 tile 归一化、3×2 拼接和占位格**
- [ ] **Step 4: 计算联系表特征并编码灰度 JPEG**
- [ ] **Step 5: 写六帧全失败“不产生文件”测试并运行绿测**

```powershell
& 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\ctest.exe' `
  --test-dir videocore\build -C Release -R '^videocore_contact_sheet$' --output-on-failure
```

Expected: 两次输出逐像素和 JPEG 哈希一致，placeholder mask 精确。

**Checkpoint:** 保存画布尺寸、成功/占位掩码、JPEG SHA-256。

**Git-backed commit:** `feat(videocore): generate deterministic six-frame contact sheets`

---

### Task 10: 完成原生韧性、精确导出和递归 DLL 闭包

**Files:**
- Create: `videocore/tests/test_resilience.cpp`
- Create: `scripts/test-videocore-exports.ps1`
- Create: `scripts/verify_videocore_native.ps1`
- Create: `scripts/resolve_native_dependencies.ps1`
- Create: `integration/videocore_build_test.go`
- Modify: `videocore/src/media_session.cpp`
- Modify: `videocore/src/video_analysis.cpp`
- Modify: `videocore/src/contact_sheet.cpp`
- Modify: `videocore/CMakeLists.txt`
- Modify: `scripts/build.ps1`

**Interfaces:**
- 在 open、hash read、probe、seek、packet read、decode、feature、JPEG encode 边界检查 cancel/deadline。
- 同一 session 的第二个并发业务调用返回 `VC_ERR_INVALID_ARG`；不同 session 可并行。
- `Resolve-NativeDependencyClosure` 对 DLL import 图大小写不敏感、去重、容忍环；非系统 DLL unresolved、重名歧义或仓库外来源均失败。
- `build.ps1` 新增 `-VideoCoreOnly` 和 `-StageDir`；StageDir 必须不存在，拒绝复用旧 `bin`。
- 生成 `videocore.dll`、`internal/wproc/videocore/libvideocore.a`、`native-dependencies.json`；不复制 FFmpeg EXE。

- [ ] **Step 1: 写各阶段取消、假时钟超时、并发和 500 次资源循环红测**
- [ ] **Step 2: 运行红测**

```powershell
& 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\ctest.exe' `
  --test-dir videocore\build -C Release -R '^videocore_resilience$' --output-on-failure
```

Expected: 中断、资源计数或并发断言失败。

- [ ] **Step 3: 用 RAII 收口所有 HANDLE、AVIO/format/codec/frame/packet/sws/JPEG 资源**
- [ ] **Step 4: 写精确导出红测并修正 `exports.def`**

```powershell
pwsh -NoProfile -File .\scripts\test-videocore-exports.ps1 `
  -Dll .\videocore\build\Release\videocore.dll `
  -Def .\videocore\exports.def
```

Expected: 最终为 `10/10 exact exports`，多余或缺失符号均失败。

- [ ] **Step 5: 实现递归闭包解析与全新原生 staging**

```powershell
pwsh -NoProfile -File .\scripts\build.ps1 `
  -VideoCoreOnly `
  -StageDir .\artifacts\stage\vc-native `
  -CMake 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\cmake.exe' `
  -VcpkgRoot 'C:\vcpkg' `
  -CC 'gcc' `
  -Dlltool 'dlltool'
```

Expected: CTest 全通过、生成 MinGW 导入库，staging 无 `tools` 和 FFmpeg EXE。

- [ ] **Step 6: 运行原生总绿测**

```powershell
pwsh -NoProfile -File .\scripts\verify_videocore_native.ps1
```

Expected: build、CTest、精确导出、runtime major、递归闭包全部通过；live-resource 计数归零。

**Checkpoint:** 保存 CTest XML、导出列表、依赖图和 staging 哈希。

**Git-backed commit:** `build(videocore): verify exports and package runtime closure`

---

### Task 11: 追加协议字段掩码、FrameMask 和 Runtime Ready

**Files:**
- Modify: `internal/proto/message.go`
- Modify: `internal/proto/conn_test.go`
- Modify: `internal/worker/messages.go`
- Modify: `internal/worker/messages_test.go`

**Interfaces:**

```go
const (
    FieldVideoDuration     uint32 = 1 << 6
    FieldVideoContactSheet uint32 = 1 << 7
    FrameMaskFull          uint8  = 0x3f
)

type RuntimeComponent struct {
    Name           string `msgpack:"name"`
    BuildVersion   string `msgpack:"build_version"`
    RuntimeVersion string `msgpack:"runtime_version"`
    BuildMajor     uint32 `msgpack:"build_major"`
    RuntimeMajor   uint32 `msgpack:"runtime_major"`
}
```

- Ready 追加 VideoCore ABI/版本和 FFmpeg build/runtime 组件列表。
- SHA query/reply 追加 requested/present/missing fields 和 frames。
- Job result 追加 fields done、frames done、duration/contact sheet 独立状态、联系表尺寸和 6 个显式 frame result。
- 保留旧 bit 数字；`FieldThumb/MaskVideoThumb` 标记 deprecated，禁止把旧 bit 2 重释为新联系表。
- present/missing 不得重叠，二者并集必须等于 requested；帧位不得超出 `0x3f`。

- [ ] **Step 1: 写 MessagePack map 兼容、未知字段和旧形状零值红测**
- [ ] **Step 2: 运行红测**

```powershell
go test -count=1 ./internal/proto ./internal/worker `
  -run 'Test(VideoCoreProtocol|SHAReplyMasks|MergedResultMapCompatibility)'
```

Expected: 缺少新常量或结构字段。

- [ ] **Step 3: 只追加常量和字段，不切业务路径**
- [ ] **Step 4: 加掩码校验和显式逐帧状态校验**
- [ ] **Step 5: 运行绿测**

```powershell
go test -count=1 ./internal/proto ./internal/worker `
  -run 'Test(MessageRoundTrip|ExtendedWorkerMessages|VideoCoreProtocol|SHAReplyMasks|MergedResultMapCompatibility)'
```

Expected: 新旧 map fixture 均可解码。

**Checkpoint:** 仅协议追加，不修改生产调度。

**Git-backed commit:** `feat(protocol): add videocore field and frame masks`

---

### Task 12: 建立 VideoCore CGO 与无 CGO stub 绑定

**Files:**
- Create: `internal/wproc/videocore/bindings.go`
- Create: `internal/wproc/videocore/bindings_stub.go`
- Create: `internal/wproc/videocore/media.go`
- Create: `internal/wproc/videocore/image.go`
- Create: `internal/wproc/videocore/phase2.go`
- Create: `internal/wproc/videocore/bindings_test.go`
- Create: `internal/wproc/videocore/media_test.go`
- Create: `internal/wproc/videocore/bindings_stub_test.go`
- Modify: `scripts/test-cgo.ps1`

**Interfaces:**

```go
type NativeError struct {
    Code       int32
    FFmpegCode int32
    Win32Code  uint32
    Message    string
}

type OpenOptions struct {
    Kind             worker.MediaKind
    ImageMemoryBytes int64
    NativeTimeout    time.Duration
}

type AnalysisRequest struct {
    Fields          uint32
    FrameMask       uint8
    KnownDurationMS int64
    ProbeTimeout    time.Duration
    FrameTimeout    time.Duration
    TileMaxSide     int32
    TempJPEGPath    string
}

func Runtime() (RuntimeInfo, error)
func Open(ctx context.Context, path string, options OpenOptions) (*Session, error)
func (s *Session) Hash() ([64]byte, error)
func (s *Session) Analyze(ctx context.Context, request AnalysisRequest) (AnalysisResult, error)
func (s *Session) Close() error
```

- 宽路径用 UTF-16 code unit 和显式长度；Go 侧先拒绝内嵌 NUL。
- 所有 C 结构先填 `struct_size/abi_version`。
- `Session` mutex 禁止并发，Close 幂等；取消 goroutine 等待 `ctx.Done()` 调用 `vc_cancel_request`，CGO 返回后 context 再次获胜。

- [ ] **Step 1: 写 stub、ABI、NUL、错误映射、幂等关闭和取消竞态红测**
- [ ] **Step 2: 运行红测**

```powershell
go test -count=1 ./internal/wproc/videocore `
  -run 'Test(RuntimeRejectsMajorMismatch|OpenRejectsEmbeddedNUL|SessionCloseIdempotent|AnalyzeCancellationWins)'
```

Expected: 包或接口不存在。

- [ ] **Step 3: 写最小 C 包装、Go 类型转换和 stub**
- [ ] **Step 4: 修改 `test-cgo.ps1` 检查 `videocore.dll/libvideocore.a`**
- [ ] **Step 5: 运行无 CGO 与 CGO 绿测**

```powershell
$env:CGO_ENABLED = '0'
go test -count=1 ./internal/wproc/videocore -run '^TestUnavailable'
$env:CGO_ENABLED = '1'
pwsh -NoProfile -File .\scripts\test-cgo.ps1 `
  -DllDir .\artifacts\stage\vc-native `
  -Packages @('./internal/wproc/videocore')
```

Expected: stub 明确 unavailable；CGO lifecycle/runtime/cancel 测试通过。

**Checkpoint:** 保存 CGO/stub 两套测试输出，不将 DLL 加入系统 PATH。

**Git-backed commit:** `feat(videocore): add session based cgo bindings`

---

### Task 13: 实现内容缓存 present/missing 查询

**Files:**
- Create: `internal/store/content.go`
- Create: `internal/store/content_test.go`
- Modify: `internal/store/features.go`
- Modify: `internal/store/phase2.go`
- Modify: `internal/worker/deduper.go`
- Modify: `internal/worker/deduper_test.go`

**Interfaces:**

```go
type ContentState struct {
    SHA512        []byte
    FieldsPresent uint32
    MissingFields uint32
    FramesPresent uint8
    MissingFrames uint8
    Image          *ImageFeature
    Video          *VideoFeature
    Frames         []VideoFrameFeature
}

func (d *DB) LookupContent(
    ctx context.Context,
    sha []byte,
    kind MediaKind,
    requestedFields uint32,
    requestedFrames uint8,
) (ContentState, error)
```

- 完整命中：两个 missing mask 均为 0。
- 部分视频命中必须返回已存在数据和精确缺失帧；不得把 5/6 帧退化为全 miss。
- single-flight 键加入 requested fields/frames；Deduper 只缓存已 commit 的 state。

- [ ] **Step 1: 写全命中、仅 duration、5/6 帧和损坏 blob 红测**
- [ ] **Step 2: 运行红测**

```powershell
go test -count=1 ./internal/store ./internal/worker `
  -run 'TestLookupContent|TestDeduperPartialVideoReturnsExactMasks'
```

Expected: `LookupContent` 未定义或 partial 被当作 miss。

- [ ] **Step 3: 从现有 feature 表派生 present/missing**
- [ ] **Step 4: 修改 Deduper lookup、reply 和 single-flight**
- [ ] **Step 5: 运行绿测**

```powershell
go test -count=1 ./internal/store ./internal/worker -run 'TestLookupContent|TestDeduper'
```

Expected: 5 个有效帧返回 `FramesPresent=0x1f`、`MissingFrames=0x20`。

**Checkpoint:** 保存四类查询结果和 SQL trace 摘要。

**Git-backed commit:** `feat(store): return exact content cache masks`

---

### Task 14: 合并 Phase 1/2 为单事务持久化

**Files:**
- Create: `internal/store/analysis.go`
- Create: `internal/store/analysis_test.go`
- Modify: `internal/store/ddl.go`
- Modify: `internal/store/db.go`
- Modify: `internal/store/features.go`
- Modify: `internal/store/phase2.go`
- Modify: `internal/store/syncq.go`
- Modify: related `internal/store/*_test.go`

**Interfaces:**

```go
var ErrStale = errors.New("store: stale analysis result")

type CommittedState struct {
    FieldsPresent uint32
    MissingFields uint32
    FramesPresent uint8
    MissingFrames uint8
}

func (d *DB) SaveAnalysis(
    ctx context.Context,
    result AnalysisResult,
) (CommittedState, error)
```

- `video_features` 增加 nullable `thumb_width/thumb_height`，同步加载器同步升级。
- 事务顺序：读取并锁定 file row → 校验 size/mtime/SHA → 校验 payload → 写成功字段 → 写成功帧 → 重算 missing → 更新 file → 写 sync_queue → commit。
- stale 必须在任何 payload 写入前返回；失败或未请求字段不 UPDATE，失败帧不覆盖已有成功帧。

- [ ] **Step 1: 写 merged atomic、stale、partial frames、sync_queue rollback 红测**
- [ ] **Step 2: 运行红测**

```powershell
go test -count=1 ./internal/store `
  -run 'TestSaveAnalysis(MergedAtomic|StaleNoOp|PartialFrames|Rollback)'
```

Expected: `SaveAnalysis` 不存在。

- [ ] **Step 3: 实现 schema 迁移与单事务**
- [ ] **Step 4: 暂留旧 SavePhase1/SavePhase2 作为测试适配器**
- [ ] **Step 5: 运行绿测**

```powershell
go test -count=1 ./internal/store `
  -run 'TestSaveAnalysis|TestPendingSyncBatch|TestOpenIsIdempotent'
```

Expected: commit 后四类记录同时可见，回滚后均不可见。

**Checkpoint:** 保存事务注入故障结果和 schema 版本。

**Git-backed commit:** `feat(store): persist merged analysis atomically`

---

### Task 15: 实现 `vc-grid-v1` 内容寻址缓存与 sidecar

**Files:**
- Create: `internal/wproc/contact_sheet_cache.go`
- Create: `internal/wproc/contact_sheet_cache_test.go`
- Modify: `internal/wproc/atomic_replace_windows.go`
- Modify: `internal/wproc/atomic_replace_other.go`

**Interfaces:**

```go
type ContactSheetPaths struct {
    JPEG        string
    Sidecar     string
    TempJPEG    string
    TempSidecar string
}

func contactSheetPaths(root string, sha [64]byte, pid int, jobID int64, nonce string) (ContactSheetPaths, error)
func lookupContactSheet(root string, sha [64]byte) (ContactSheetMeta, bool, error)
func publishContactSheet(paths ContactSheetPaths, meta ContactSheetMeta, validateSource func() error) error
```

- 最终路径固定为 `<root>\vc-grid-v1\<sha前2位>\<完整小写sha>.jpg` 与 `<jpg>.json`。
- sidecar 包含 schema/pipeline 版本、源 SHA/size、JPEG SHA-256、画布/tile 尺寸、六个采样状态、VideoCore 和 FFmpeg 版本。
- 命中必须验证 JPEG 常规非空、sidecar schema/pipeline/SHA、实际 JPEG SHA-256。
- 发布顺序：JPEG 校验与 Sync → source drift 检查 → JPEG 原子替换 → sidecar 写/Sync/原子替换。
- 只清理超过一小时且属于 `vc-grid-v1` 命名规则的 temp。

- [ ] **Step 1: 写路径、逃逸、sidecar 和不匹配组合红测**
- [ ] **Step 2: 写并发 writer/reader 红测**

```powershell
go test -count=1 ./internal/wproc `
  -run 'TestContactSheet(CachePath|Sidecar|ConcurrentPublish|RejectsEscape)'
```

Expected: 内容寻址 API 不存在。

- [ ] **Step 3: 实现路径和命中验证**
- [ ] **Step 4: 实现限定清理和原子发布**
- [ ] **Step 5: 运行绿测**

```powershell
go test -count=1 ./internal/wproc -run '^TestContactSheet'
```

Expected: 相同 SHA、不同源路径得到同一 JPEG；读者不接受不匹配组合。

**Checkpoint:** 保存路径样例、并发测试计数和原子替换证据。

**Git-backed commit:** `feat(cache): add vc-grid-v1 contact sheet cache`

---

### Task 16: 切换为一次 open/hash/analyze 的合并管线

**Files:**
- Create: `internal/wproc/pipeline_session.go`
- Create: `internal/wproc/pipeline_session_test.go`
- Modify: `internal/wproc/pipeline.go`
- Modify: `internal/wproc/pipeline_video.go`
- Modify: `internal/wproc/pipeline_phase2.go`
- Modify: `internal/wproc/video_frames.go`
- Modify: `internal/wproc/run.go`
- Modify: `internal/wproc/run_test.go`

**Interfaces:**

```go
type mediaSession interface {
    Hash() ([64]byte, error)
    Analyze(context.Context, videocore.AnalysisRequest) (videocore.AnalysisResult, error)
    Close() error
}

func processMediaWithDeps(
    ctx context.Context,
    cfg Config,
    job *worker.JobMsg,
    deps sessionPipelineDeps,
) (*worker.JobResultMsg, error)
```

- 固定流程：dispatch metadata 校验 → Open 一次 → defer Close → Hash 一次 → SHA query → 计算 missing masks → 全命中直接返回 → 为联系表分配同目录 temp → Analyze 最多一次 → context 二次检查 → 文件身份/size/mtime 复核 → 发布缓存 → 返回合并结果。
- 视频不得额外解码中点帧；部分 miss 只传精确缺失 fields/frames。
- 单帧普通失败保留其他成功槽；六帧全失败不发布 JPEG/sidecar。
- context cancelled 或 stale 时清空 SHA、派生字段和帧，清理 temp。
- error stage 只允许：`native_open`, `native_hash`, `image_decode`, `video_probe`, `video_frame`, `video_contact_sheet`, `feature_compute`, `thumb_cache`, `stale`。

- [ ] **Step 1: 写 fake session 次数红测**

断言一次 open/hash/analyze/close；完整缓存命中 analyze 为 0。

- [ ] **Step 2: 写 partial mask、取消、stale、单帧失败和全帧失败红测**

```powershell
go test -count=1 ./internal/wproc `
  -run 'TestSessionPipeline(OneOpenOneHashOneAnalyze|CompleteHitSkipsAnalyze|PartialMask|Cancellation|Stale|PartialFrames)'
```

Expected: 当前仍走 ffprobe/ffmpeg 和独立 Phase 2 管线。

- [ ] **Step 3: 实现统一 pipeline，暂不删除旧文件**
- [ ] **Step 4: 让 serve 路径调用统一 pipeline**
- [ ] **Step 5: 运行绿测**

```powershell
go test -count=1 ./internal/wproc -run 'TestSessionPipeline|TestServe'
```

Expected: 所有退出路径 Close 恰好一次；全命中不 analyze。

**Checkpoint:** 保存 fake 调用次数、missing mask 和 stale 结果。

**Git-backed commit:** `feat(worker): analyze media through one videocore session`

---

### Task 17: Supervisor 接入 Runtime Ready 与合并事务

**Files:**
- Modify: `internal/worker/pool.go`
- Modify: `internal/worker/supervisor.go`
- Modify: `internal/worker/pool_test.go`
- Modify: `internal/worker/process_tree_windows_test.go`
- Modify: `internal/wproc/run.go`

**Interfaces:**

```go
type FeatureStore interface {
    LookupContent(context.Context, []byte, store.MediaKind, uint32, uint8) (store.ContentState, error)
    SaveAnalysis(context.Context, store.AnalysisResult) (store.CommittedState, error)
    MarkCrash(context.Context, string, string, string) error
}
```

- `saveResult` 只调用一次 `SaveAnalysis`，以返回的 committed masks 修正发布结果。
- `validateWorkerResult` 按 requested fields/frames 校验；禁止携带未请求 payload；失败帧必须非零 status 且无 feature payload。
- Ready 在 IPC、VideoCore ABI/版本、FFmpeg build/runtime major 全部匹配后才进入 free pool。
- 原生 crash/watchdog 保持现有替代流程：失败当前任务、旧 Worker 退出、新 Worker Ready、Agent PID 不变、后续任务可成功。

- [ ] **Step 1: 修改 fake store 接口制造编译红灯**
- [ ] **Step 2: 写 merged save 一次、rollback 不发布、stale 清空、runtime mismatch 红测**

```powershell
go test -count=1 ./internal/worker `
  -run 'TestPool(SavesMergedResultOnce|RejectsVideoCoreRuntimeMismatch|StalePublishesNothing|ReplacementReadyAfterNativeCrash)'
```

Expected: 仍要求旧 SavePhase1/SavePhase2 或接受旧 Ready。

- [ ] **Step 3: 修改 pool 持久化和 result validation**
- [ ] **Step 4: 修改 supervisor Ready 门禁**
- [ ] **Step 5: 运行 worker 全包绿测**

```powershell
go test -count=1 ./internal/worker
```

Expected: 合并结果一次事务；替代 Worker 测试继续通过。

**Checkpoint:** 保存 save 调用计数、Ready 拒绝原因和替代 PID 链。

**Git-backed commit:** `refactor(worker): persist merged videocore results`

---

### Task 18: 迁移配置并完成全量 staging 构建

**Files:**
- Modify: `internal/config/agent.go`
- Modify: `internal/config/config_test.go`
- Modify: `internal/wproc/config.go`
- Modify: `internal/wproc/config_test.go`
- Modify: `scripts/build.ps1`
- Modify: `scripts/test-cgo.ps1`
- Create: `integration/videocore_build_test.go`

**Interfaces:**

```go
type ThumbConfig struct {
    CacheDir       string `json:"cache_dir"`
    TileMaxSide    int    `json:"tile_max_side"`
    ProbeTimeoutS  int    `json:"probe_timeout_s"`
    NativeTimeoutS int    `json:"native_timeout_s"`
    FrameTimeoutS  int    `json:"frame_timeout_s"`
}
```

- Worker 环境只保留 `WPROC_THUMB_CACHE`, `WPROC_TILE_MAX_SIDE`, `WPROC_PROBE_TIMEOUT_S`, `WPROC_NATIVE_TIMEOUT_S`, `WPROC_FRAME_TIMEOUT_S`, `WPROC_IMAGE_MEM_MB`, `WPROC_IPC_MAX_MB`。
- 删除 `thumb.ffmpeg_path`, `thumb.ffprobe_path`, `thumb.max_side`, `WPROC_FFMPEG`, `WPROC_FFPROBE`。
- `ValidateThumbCacheRoots` 拒绝缓存根与扫描根相等、父子重叠。
- 完整 staging 包含 agent/gui/helper/worker/videocore 和递归 FFmpeg DLL 闭包；Agent/GUI/Helper `CGO_ENABLED=0`，Worker `CGO_ENABLED=1`。

- [ ] **Step 1: 写新配置、默认值、绝对化和 root overlap 红测**
- [ ] **Step 2: 写 staging 内容和 import 边界红测**

```powershell
go test -count=1 ./internal/config ./internal/wproc ./integration `
  -run 'Test(VideoCoreConfig|ThumbCacheRootOverlap|WorkerEnvHasNoFFmpegExecutable|VideoCoreBuild)'
```

Expected: 旧字段/环境或构建输出仍存在。

- [ ] **Step 3: 修改配置和 WorkerEnv**
- [ ] **Step 4: 完成构建顺序和 release manifest**

```powershell
pwsh -NoProfile -File .\scripts\build.ps1 `
  -StageDir .\artifacts\stage\vc-full `
  -Go 'go' `
  -CC 'gcc' `
  -Windres 'windres' `
  -Dlltool 'dlltool' `
  -CMake 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\cmake.exe' `
  -VcpkgRoot 'C:\vcpkg'
```

Expected: 全新 staging，无 `tools` 和媒体 EXE；FFmpeg DLL 与 Worker 同级。

- [ ] **Step 5: 运行配置和 CGO 绿测**

```powershell
go test -count=1 ./internal/config ./internal/wproc
pwsh -NoProfile -File .\scripts\test-cgo.ps1 `
  -DllDir .\artifacts\stage\vc-full `
  -Packages @('./internal/wproc/videocore','./internal/wproc','./internal/worker')
```

Expected: 配置、stub/CGO 和 runtime Ready 均通过。

**Checkpoint:** 保存 staging manifest、PE imports 和配置归一化样例。

**Git-backed commit:** `build: package worker with videocore runtime closure`

---

### Task 19: 建立兼容差分、空 PATH 和 crash/hang 动态验收

**Files:**
- Create: `scripts/verify_videocore_compat.ps1`
- Create: `scripts/verify_videocore_acceptance.ps1`
- Create: `integration/videocore_compat_test.go`
- Create: `integration/videocore_acceptance_test.go`
- Create: `testdata/videocore/acceptance/*`

**Interfaces:**
- compatibility 总是写 `compat-diff.json`；SHA、图片特征、duration、六帧身份/尺寸/特征任一差异退出 1；不提供容差或阈值放宽参数。
- acceptance build tag 为 `windows && videocoreacceptance`。
- 空 PATH 测试必须使用绝对 staging/config，持续采样完整 Agent 后代；只允许 `worker.exe`，解码子进程数必须为 0。
- crash/hang 必须证明 Agent PID 不变、旧 Worker PID 消失、新 Worker Ready、故障任务失败、后续合法任务成功。
- hang 最终证据使用真实约 120000 ms 硬看门狗，不得用短 timeout 替代。

- [ ] **Step 1: 写“篡改一个 golden 字节必失败”红测**

```powershell
go test -count=1 ./integration -run '^TestVideoCoreCompatibilityGate$'
```

Expected: 差异输出精确 JSON path、旧值和新值。

- [ ] **Step 2: 运行真实兼容门禁**

```powershell
pwsh -NoProfile -File .\scripts\verify_videocore_compat.ps1 `
  -Manifest .\testdata\videocore\compat\manifest.json `
  -Golden .\testdata\videocore\compat\legacy-golden.json `
  -StageDir .\artifacts\stage\vc-full `
  -Evidence .\artifacts\evidence\videocore-compat.json
```

Expected: `VIDEOCORE COMPAT PASS differences=0`。

- [ ] **Step 3: 写动态验收 harness 红测**

```powershell
go test -count=1 ./integration -run '^TestVideoCoreAcceptanceHarness$'
```

Expected: 当前缺少验收脚本或进程树证据。

- [ ] **Step 4: 运行空 PATH、crash 和 hang 验收**

```powershell
pwsh -NoProfile -File .\scripts\verify_videocore_acceptance.ps1 `
  -StageDir .\artifacts\stage\vc-full `
  -CorpusDir .\testdata\videocore\acceptance `
  -PGDSN $env:FS_PG_DSN `
  -EvidenceDir .\artifacts\evidence\videocore-acceptance
```

Expected:

```text
AC-1 PASS empty_path=true decoder_children=0
AC-2 PASS agent_pid_unchanged=true replacement_ready=true followup_done=true
AC-3 PASS watchdog_ms>=120000 agent_pid_unchanged=true followup_done=true
```

- [ ] **Step 5: 审计凭据脱敏和残留**

证据包含 `acceptance.json`, `process-tree.jsonl`, `ready.jsonl` 和 PID 链；不得包含 DSN；测试进程残留为 0。

**Checkpoint:** 只有 compat 差异为 0 且三项动态验收通过，Task 20 才可开始。

**Git-backed commit:** `test: prove videocore compatibility and worker recovery`

---

### Task 20: 删除旧链路并启用不可回退静态门禁

**Files:**
- Create: `scripts/verify_videocore_static.ps1`
- Create: `integration/videocore_static_gate_test.go`
- Modify: `scripts/verify_m3.ps1`
- Modify: `scripts/verify_m4.ps1`
- Modify: `integration/m4_e2e_test.go`
- Delete: `mediacore/`
- Delete: `internal/wproc/mediacore/`
- Delete: `internal/wproc/ffmpeg.go`, `ffmpeg_test.go`
- Delete: `internal/wproc/video_frames.go`, `video_frames_test.go`, `video_frames_integration_test.go`
- Delete: superseded `internal/wproc/pipeline_video*`, `pipeline_phase2*`
- Delete: `scripts/verify_m2.ps1`, `scripts/verify_m2_native.ps1`
- Delete: `integration/m2_e2e_test.go`, `integration/m2_acceptance_test.go`
- Delete: `bin/mediacore.dll`, `bin/tools/`
- Delete: `third_party/ffmpeg/bin/ffmpeg.exe`, `ffprobe.exe`, `ffplay.exe`

**Interfaces:**
- 静态门禁失败于生产 `internal/wproc` 中 `exec.Command*` 媒体调用、旧配置/env、旧原生名字、FFmpeg EXE、`tools` 目录或任何 `mc_*` 导出。
- staging 中 Agent/GUI/Helper 不得 import VideoCore/FFmpeg；Worker 必须 import `videocore.dll`。
- DLL 导出必须与 `videocore/exports.def` 完全一致；release manifest 哈希必须与 staging 一致。
- M3/M4 验证使用已提交合成 fixture，不得运行 FFmpeg EXE 生成语料。

- [ ] **Step 1: 写静态门禁并确认对当前旧树为红**

```powershell
go test -count=1 ./integration -run '^TestVideoCoreStaticGate$'
```

Expected: 列出 `mediacore`、FFmpeg EXE、tools 和旧配置命中。

- [ ] **Step 2: 再确认 Task 19 前置证据**

```powershell
$compat = Get-Content -Raw -LiteralPath .\artifacts\evidence\videocore-compat.json | ConvertFrom-Json
$acceptance = Get-Content -Raw -LiteralPath .\artifacts\evidence\videocore-acceptance\acceptance.json | ConvertFrom-Json
if ($compat.differences -ne 0 -or -not $acceptance.pass) { throw 'replacement evidence is not complete' }
```

- [ ] **Step 3: 使用 `apply_patch` 删除已被覆盖的旧源文件**

删除前逐项对照本任务清单；不得删除设计、历史验收报告或 Task 1 golden。

- [ ] **Step 4: 删除旧二进制与 FFmpeg EXE，更新 M3/M4 验证**
- [ ] **Step 5: 运行静态绿测**

```powershell
pwsh -NoProfile -File .\scripts\verify_videocore_static.ps1 `
  -RepoRoot . `
  -StageDir .\artifacts\stage\vc-full `
  -Dumpbin 'dumpbin.exe' `
  -Evidence .\artifacts\evidence\videocore-static.json
go test -count=1 ./integration -run '^TestVideoCoreStaticGate$'
```

Expected: `VIDEOCORE STATIC PASS forbidden=0`。

- [ ] **Step 6: 运行 Go 与 M3/M4 回归**

```powershell
go test -count=1 ./...
pwsh -NoProfile -File .\scripts\verify_m3.ps1
pwsh -NoProfile -File .\scripts\verify_m4.ps1
```

Expected: 所有本地可运行门禁通过；需要外部数据库的项必须明确 PASS/FAIL/NOT_RUN。

**Checkpoint:** 记录删除清单、static evidence、全包测试和 M3/M4 结果。

**Git-backed commit:** `refactor: remove legacy mediacore and ffmpeg executable paths`

---

### Task 21: 运行短性能回归与不可压缩 24 小时驻留

**Files:**
- Create: `scripts/run_videocore_short_benchmark.ps1`
- Create: `scripts/run_videocore_soak.ps1`
- Create: `scripts/audit_videocore_soak.ps1`
- Create: `integration/videocore_benchmark_contract_test.go`

**Interfaces:**
- 短测输出 `videocore-short.json`：wall/CPU、读取字节、句柄峰值/结束值、Worker RSS、原生 live-resource、文件数、失败数、结果漂移数及相对旧基线比例。
- 设计未确认性能退化百分比，因此不得发明通过阈值；硬门禁是指标齐全、无 crash、无结果漂移，并如实报告相对变化。
- soak 固定 `DurationHours=24`；审计要求 `elapsed_ms>=86400000`、`stop_reason=duration_complete`、launcher/child exit 0、漂移 0、无重启风暴、无持续单调资源增长、日志无凭据。

- [ ] **Step 1: 写 benchmark/soak 合同红测**

```powershell
go test -count=1 ./integration -run '^TestVideoCore(Benchmark|Soak)Contract'
```

Expected: 缺少指标、状态文件或短于 24h 时失败。

- [ ] **Step 2: 运行短测**

```powershell
pwsh -NoProfile -File .\scripts\run_videocore_short_benchmark.ps1 `
  -StageDir .\artifacts\stage\vc-full `
  -CorpusManifest .\testdata\videocore\compat\manifest.json `
  -LegacyBaseline .\testdata\videocore\compat\legacy-golden.json `
  -EvidenceDir .\artifacts\evidence\videocore-short
```

Expected: `VIDEOCORE SHORT BENCH PASS result_drift=0`。

- [ ] **Step 3: 审阅相对性能并记录，不自行放宽功能门禁**
- [ ] **Step 4: 在确认的生成语料目录启动完整 24h 驻留**

```powershell
pwsh -NoProfile -File .\scripts\run_videocore_soak.ps1 `
  -StageDir .\artifacts\stage\vc-full `
  -CorpusRoot 'D:\videocore-generated-corpus' `
  -ConfirmGeneratedCorpus `
  -DurationHours 24 `
  -EvidenceDir .\artifacts\evidence\videocore-soak-24h
```

不得把 `D:\m6-generated-corpus` 或任何真实媒体目录作为生成/清理目标。

- [ ] **Step 5: 审计 24h 证据**

```powershell
pwsh -NoProfile -File .\scripts\audit_videocore_soak.ps1 `
  -EvidenceDir .\artifacts\evidence\videocore-soak-24h
```

Expected: `VIDEOCORE SOAK 24H PASS`。

**Checkpoint:** 保存 short JSON、soak.json、launcher.status.json、stats.jsonl、process-tree.jsonl、result-diff.json 和审计摘要。

**Git-backed commit:** `test: add videocore performance and 24-hour residency gates`

---

### Task 22: 更新中文 README、验收记录与总门禁

**Files:**
- Modify: `README.md`
- Modify: `docs/todolist.md`
- Create: `docs/acceptance/2026-07-31-videocore.md`
- Create: `scripts/verify_videocore.ps1`
- Create: `integration/videocore_documentation_test.go`

**Documentation Contract:**
- README 快速启动改为 `worker.exe + videocore.dll + 应用本地 FFmpeg DLL 闭包`，删除 `mediacore.dll`、`bin\tools` 和 ffmpeg/ffprobe 路径说明。
- 架构图改为 `Worker → videocore.dll → FFmpeg DLL`，说明图片算法静态进入 VideoCore。
- 配置文档写清 `thumb.cache_dir`、`tile_max_side=256`、三个 timeout、默认路径、扫描根重叠限制和 `vc-grid-v1` 文件布局。
- 解释六帧联系表、占位格、sidecar、内容寻址和失败语义。
- 构建说明列出 CMake/vcpkg/MSVC/MinGW、全新 staging、`libvideocore.a` 和 CGO 边界。
- 验收文档严格区分 local/static、dynamic、release redistribution、short benchmark、24h soak；未运行项写 `NOT_RUN`。
- 历史 M2/M6 文档保持历史事实，不回写成当前 VideoCore 证据。

- [ ] **Step 1: 写 README/验收合同红测**

```powershell
go test -count=1 ./integration -run '^TestVideoCoreDocumentationContract$'
```

Expected: README 仍包含旧运行方式。

- [ ] **Step 2: 更新 README 和 todo**
- [ ] **Step 3: 从实际证据生成验收文档**

文档引用精确命令、退出码、导出集合、依赖闭包、哈希、版本、diff、故障 PID、短测数字和 24h 数字；不得根据计划预填 PASS。

- [ ] **Step 4: 实现总门禁脚本**

总门禁按顺序运行 native、CGO、非 CGO build、supply release、compat、static、empty PATH、process tree、crash/hang、short、soak audit。

- [ ] **Step 5: 运行最终门禁**

```powershell
pwsh -NoProfile -File .\scripts\verify_videocore.ps1 `
  -Go 'go' `
  -CC 'gcc' `
  -Windres 'windres' `
  -Dlltool 'dlltool' `
  -CMake 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\cmake.exe' `
  -VcpkgRoot 'C:\vcpkg' `
  -Dumpbin 'dumpbin.exe' `
  -PGDSN $env:FS_PG_DSN `
  -SoakEvidence .\artifacts\evidence\videocore-soak-24h `
  -EvidenceDir .\artifacts\evidence\videocore-final
```

Expected final summary:

```text
NATIVE CTEST PASS
CGO GO PASS
NON-CGO BUILD PASS
SUPPLY CHAIN RELEASE PASS
COMPAT PASS differences=0
STATIC PASS forbidden=0
EMPTY PATH PASS
PROCESS TREE PASS decoder_children=0
CRASH/HANG RECOVERY PASS
SHORT BENCH PASS result_drift=0
SOAK 24H PASS
VIDEOCORE VERIFY PASS
```

- [ ] **Step 6: 运行文档绿测**

```powershell
go test -count=1 ./integration -run '^TestVideoCoreDocumentationContract$'
```

Expected: `PASS`。

**Checkpoint:** 发布最终证据索引；若任一硬门禁未通过，验收结论必须为 `BLOCKED`，不得写完成。

**Git-backed commit:** `docs: publish videocore usage and acceptance evidence`

---

## Final Review Checklist

- [ ] 每个 ABI 结构都含正确的 `struct_size/abi_version`，导出仅 10 个 `vc_*`。
- [ ] `vc_runtime_info` 的函数名与 `struct vc_runtime_info` 标签不产生 C typedef 冲突。
- [ ] 一个媒体任务只有一次 Open、一次 Hash、最多一次 Analyze、一次 Close。
- [ ] 视频只有一个文件 HANDLE 和一个 `AVFormatContext`，无中点帧。
- [ ] 六帧采样、旋转/SAR、占位格和 3×2 联系表均由 deterministic fixture 锁定。
- [ ] 图片算法逐字节兼容，未修改匹配阈值。
- [ ] present/missing masks 和单事务提交在 partial/stale/rollback 下正确。
- [ ] `vc-grid-v1` 路径、sidecar、JPEG SHA-256 和原子发布通过并发测试。
- [ ] Ready 拒绝 ABI/FFmpeg 主版本不匹配。
- [ ] staging 在空 PATH 下可运行，媒体解码子进程数为 0。
- [ ] crash/hang 后 Agent PID 不变、替代 Worker Ready、后续任务成功。
- [ ] 旧源码、旧导入库、旧 DLL、FFmpeg EXE、旧配置和媒体 `exec.Command*` 全部为 0。
- [ ] FFmpeg Release 供应链和许可证证据真实完整；不完整时保持 BLOCKED。
- [ ] 短性能报告无结果漂移，完整 24h 驻留通过审计。
- [ ] README 和验收文档为中文，且所有 PASS 都引用本次实际证据。
