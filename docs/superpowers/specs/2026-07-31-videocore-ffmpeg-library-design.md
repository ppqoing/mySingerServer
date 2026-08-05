# VideoCore 原生 FFmpeg 迁移设计

## 状态

本设计已由项目负责人于 2026-07-31 逐节确认。

本设计将生产环境中基于 `ffmpeg.exe`、`ffprobe.exe` 子进程的视频处理
管线，以及现有 `mediacore.dll` ABI，统一替换为新的原生媒体核心
`videocore.dll`。

## 目标

- 在 C++ 中直接调用 FFmpeg C API，不再启动 FFmpeg 可执行程序。
- 将 `videocore.dll` 建设为唯一原生媒体计算 DLL。
- 把现有 SHA-512、图片解码、PDQ、分区 pHash 和 Sobel 实现静态编译进
  `videocore.dll`。
- 动态链接仓库固定版本的 FFmpeg 运行 DLL。
- 每个 Worker 任务只打开一次媒体文件，并复用同一原生 session 完成
  哈希、内容缓存查询、图片或视频解码及全部所需特征。
- 新视频内容未命中缓存时，在一次分析任务中同时生成原 Phase 1 和
  Phase 2 数据。
- 使用参与逐帧特征计算的同一组六个视频采样帧，生成一张灰度、三列两行
  的联系表缩略图。
- 保持 Worker 进程级崩溃隔离、取消、部分成功、文件漂移拒绝、缓存原子
  发布和内容级去重语义。

## 已确认决策

- 新 DLL 命名为 `videocore.dll`。
- 公共导出符号全部使用新的 `vc_*` 前缀。
- 不保留任何 `mc_*` 兼容导出。
- 新实现通过验收后，删除 `mediacore.dll`、其导入库、绑定、构建工程和
  运行产物。
- 原生算法以源码形式迁入并静态编译进 `videocore.dll`；
  `videocore.dll` 不依赖 `mediacore.dll`。
- FFmpeg 采用动态链接。发布包保留所需 FFmpeg 运行 DLL，但不包含
  `ffmpeg.exe`、`ffprobe.exe` 或 `ffplay.exe`。
- 新管线优化目标为：一个媒体文件句柄、一个原生 session、一次顺序
  SHA-512 读取和一个 FFmpeg session。由于完整 SHA-512 必须覆盖全部
  字节，而快速抽帧需要 seek，因此不承诺磁盘物理层面绝对只读一次。
- 缩略图缓存使用完整 SHA-512 按内容寻址。
- 视频缩略图由六个标准采样帧按三列两行合成，不再额外解码中点帧。
- 个别采样帧失败时使用确定性占位格；六帧全部失败时不生成缩略图。

## 不在范围内

- DLL 不访问数据库、不处理 Agent 网络通信、不承担任务调度或 UI 渲染。
- FFmpeg 类型、C++ 类型和 STL 类型不得穿过公共 ABI。
- DLL 不得从未知原生线程回调 Go。
- 第一版不启用硬件解码。确定性软件解码路径是特征兼容性的唯一权威路径。
- 本迁移不得静默修改匹配阈值，也不得在出现特征漂移时自行放宽验收条件。
- 不自动删除旧版缩略图缓存。

## 现状基线

当前 Worker 存在三类外部命令：

1. `ffprobe` 读取 `format=duration`，超时 15 秒。
2. `ffmpeg` 定位视频中点，生成灰度 JPEG 缩略图，超时 60 秒。
3. Phase 2 为每个请求帧单独启动一次 `ffmpeg`，通过 stdout 输出灰度
   PNG，每帧超时 20 秒。

新实现将删除上述生产命令路径。Agent、GUI 和 Helper 继续以非 CGO
方式构建；只有 `worker.exe` 链接新的导入库并加载 `videocore.dll`。

## 运行时架构

```text
agent.exe（不使用 CGO，不加载媒体 DLL）
  |
  +-- worker.exe × N
        |
        +-- videocore.dll
              |
              +-- 静态编译的 SHA/图片/PDQ/pHash/Sobel 代码
              +-- avformat/avcodec/avutil/swscale 运行 DLL
```

Worker 继续承担硬隔离边界。原生访问违例或不协作的解码器挂起可以终止
Worker，但原生媒体代码不会被加载进 Agent，也不会直接终止 Agent。

FFmpeg 运行 DLL 作为应用本地依赖，与 `worker.exe`、`videocore.dll`
放在同一目录。不得继续放在 `bin\tools`，因为 Worker 启动阶段解析
`videocore.dll` 时，业务代码还没有机会修改 DLL 搜索路径。

## 目标仓库结构

```text
videocore/
  CMakeLists.txt
  vcpkg.json
  exports.def
  include/videocore/videocore.h
  src/
    api.cpp
    media_session.cpp
    image_analysis.cpp
    video_analysis.cpp
    contact_sheet.cpp
    native_algorithms/
    pdq_upstream/
  tests/

internal/wproc/videocore/
  bindings.go
  bindings_stub.go
  image.go
  media.go
  phase2.go
  libvideocore.a
```

实施过程中可以暂时保留新旧工程并行，用于差分验证。最终交付的仓库和
发布包不得保留旧原生工程、旧绑定、旧 DLL 或旧导入库。

## 公共 C ABI

### ABI 基本规则

- `VC_ABI_VERSION` 从 `1` 开始。
- `VC_VERSION_STRING` 从 `1.0.0` 开始。
- Windows 导出使用 `VC_API`，并显式定义 `VC_CALL` 为 `__cdecl`。
- 公共整数全部使用固定宽度 C 类型。
- 可扩展结构的前两个字段固定为 `uint32_t struct_size` 和
  `uint32_t abi_version`。
- 公共结构不得包含编译器相关的 `bool`、C++ enum、STL、异常、
  FFmpeg 类型或拥有所有权的 C++ 指针。
- 特征数组尺寸固定：
  - SHA-512：64 字节；
  - PDQ-256：32 字节；
  - 分区 pHash：9 个 `uint64_t`；
  - Sobel 直方图：128 个 `float`；
  - 视频帧：6 个固定槽位。
- 调用方提供的输入和输出缓冲区只在一次调用期间借用。
- DLL 分配的每个不透明句柄必须由对应的 DLL 释放函数释放。
- 调用方不得直接对 DLL 内存使用 `free` 或 `delete`。
- 同一个媒体 session 不允许并发调用；其取消令牌允许从另一线程请求取消。

### 状态码

| 名称 | 数值 |
|---|---:|
| `VC_OK` | `0` |
| `VC_ERR_INVALID_ARG` | `-1` |
| `VC_ERR_ABI` | `-2` |
| `VC_ERR_OOM` | `-3` |
| `VC_ERR_IO` | `-4` |
| `VC_ERR_UNSUPPORTED` | `-5` |
| `VC_ERR_DEMUX` | `-6` |
| `VC_ERR_DECODE` | `-7` |
| `VC_ERR_ENCODE` | `-8` |
| `VC_ERR_NO_FRAME` | `-9` |
| `VC_ERR_OUTPUT_TOO_LARGE` | `-10` |
| `VC_ERR_CANCELLED` | `-11` |
| `VC_ERR_TIMEOUT` | `-12` |
| `VC_ERR_STALE` | `-13` |
| `VC_ERR_INTERNAL` | `-99` |

错误结构定义为：

```c
typedef struct vc_error {
    uint32_t struct_size;
    uint32_t abi_version;
    int32_t code;
    int32_t ffmpeg_code;
    uint32_t win32_code;
    char message_utf8[512];
} vc_error;
```

诊断文本允许截断，但必须始终以 NUL 结尾。所有 C++ 异常必须在返回
C ABI 前被捕获并转换。

### 核心入口

```c
uint32_t VC_CALL vc_abi_version(void);
const char* VC_CALL vc_version(void);
int32_t VC_CALL vc_runtime_info(vc_runtime_info* out, vc_error* err);

int32_t VC_CALL vc_cancel_create(vc_cancel_token** out, vc_error* err);
void VC_CALL vc_cancel_request(vc_cancel_token* token);
void VC_CALL vc_cancel_free(vc_cancel_token* token);

int32_t VC_CALL vc_media_open_w(
    const uint16_t* path,
    uint32_t path_units,
    const vc_media_open_options* options,
    vc_cancel_token* cancel,
    vc_media_session** out,
    vc_error* err);

int32_t VC_CALL vc_media_hash(
    vc_media_session* session,
    uint8_t out_sha512[64],
    vc_error* err);

int32_t VC_CALL vc_media_analyze(
    vc_media_session* session,
    const vc_analysis_request* request,
    vc_analysis_result* out,
    vc_error* err);

void VC_CALL vc_media_close(vc_media_session* session);
```

`vc_runtime_info` 返回 VideoCore ABI/版本，以及编译期和实际加载的
FFmpeg 组件版本。Worker Ready 必须包含这些信息；头文件、导入库和
运行 DLL 主版本不匹配时，Worker 不得开始接收任务。

`vc_media_open_options` 包含预期媒体类型、图片内存上限、原生操作总
超时，以及必须为零的保留标志。

`vc_analysis_request` 包含：

- 请求特征掩码；
- 六帧 `FrameMask`；
- 已知时长；
- 探测超时；
- 单帧超时；
- 联系表单格最长边；
- 临时 JPEG 的 UTF-16 路径及显式长度。

`vc_analysis_result` 包含：

- 顶层媒体信息；
- 独立的时长和联系表状态；
- 图片特征；
- 六个固定帧槽位；
- 操作与解码耗时计数。

每个帧槽位包含标准索引、采样时间、状态、PDQ/Quality、pHash 和
Sobel。字段只有在状态为 `VC_OK` 时才可使用，调用方不得通过输出字节
是否为零推断成功。

## Windows 文件与 Unicode 契约

- 本地路径通过 UTF-16 code unit 加显式长度进入 ABI。
- 拒绝内嵌 NUL。
- 支持 UNC 和 `\\?\` 长路径。
- 使用 `CreateFileW` 打开本地文件。
- 使用自定义 `AVIOContext` 通过同一个 Windows 句柄读取，使路径行为
  不依赖 FFmpeg 的窄字符转换。
- session 在打开时记录文件身份、大小和时间。
- 原始协议路径仍由 Go 持有和上报。
- URL 或网络媒体不属于本地文件 ABI。

## 单 Session 数据流

### 公共准入流程

1. Go 校验派发任务中的文件大小和修改时间。
2. `vc_media_open_w` 打开一个 Windows 文件句柄并创建一个
   `vc_media_session`。
3. `vc_media_hash` 顺序完成一次 SHA-512。
4. Worker 保持 session 打开，并将 SHA-512 发给 Agent 查询内容缓存。
5. Agent 返回已有字段、缺失图片或视频字段掩码及缺失帧掩码。
6. 完整命中内容缓存时，直接关闭 session，不执行解码。
7. 部分命中或完全未命中时，仅针对缺失字段调用一次
   `vc_media_analyze`。
8. Go 在发布结果前验证路径仍然指向派发时的同一文件。
9. 所有成功、失败、取消和 panic 恢复路径都必须关闭 session。

图片在 SHA 读取期间收集到的受限字节缓冲会直接用于解码。现有图片内存
上限继续作为硬性准入条件。

视频禁止整体驻留内存。FFmpeg AVIO 层复用同一个 Windows 句柄，并尽量
利用 SHA 顺序读取形成的系统缓存。一次 `vc_media_analyze` 只能创建一个
`AVFormatContext`。解码器上下文在全部采样点之间复用；普通解码错误允许
只重建 codec context。格式上下文失效后，不重新打开媒体文件，而是终止
后续采样并返回已完成结果与剩余错误。

## 图片分析

图片缓存未命中时，只解码一次，并从同一灰度面计算请求的 PDQ、pHash 和
Sobel。

SHA-512 和全部图片特征编码必须与旧实现逐字节一致。图片算法只允许迁移
源文件和命名空间，不得在本任务中重写算法。

## 视频分析

### 六个标准采样点

```text
duration * {1, 3, 5, 7, 9, 11} / 12
```

整数计算必须避免溢出，并保持现有毫秒采样标识。请求的采样点按时间升序
处理。`FrameMask == 0` 表示请求全部六帧；非零掩码只计算置位槽位，用于
旧数据或部分数据补算。

在特征计算和联系表合成前，解码器先应用显示旋转和像素宽高比校正。帧保持
显示宽高比，不裁剪。

### 合并结果

正常的新视频缓存未命中结果包含：

- `vc_media_hash` 返回的 SHA-512；
- 视频时长，单位毫秒；
- 六个逐帧状态；
- 每帧 PDQ、Quality、pHash 和 Sobel；
- 联系表 PDQ、Quality、宽度和高度；
- 临时联系表 JPEG 状态；
- 操作和解码计时。

Agent/Worker 协议和持久化层接收合并的 Phase 1/Phase 2 结果。旧数据可以
只请求缺失字段。只有六个标准槽位全部成功时才设置六帧完成位；成功的部分
帧及其独立错误必须保留。

## 六帧联系表缩略图

不再单独解码视频中点帧。

同一组六个标准帧按行优先排列：

```text
+---------+---------+---------+
|  帧 0   |  帧 1   |  帧 2   |
+---------+---------+---------+
|  帧 3   |  帧 4   |  帧 5   |
+---------+---------+---------+
```

规则：

- `thumb.tile_max_side` 默认 256 像素，表示单个格子的最长边。
- 六个格子尺寸相同，尺寸根据校正后的显示宽高比确定。
- 等比例缩放，不裁剪。
- 画布尺寸严格为三个格子宽、两个格子高。
- 格子之间不留间距，不添加边框、文本或时间戳，不引入字体依赖。
- 画布和 JPEG 均为灰度。
- 初始编码规则保持现有 JPEG `q:v=3` 的质量意图。
- 联系表 PDQ 和 Quality 基于合成后的灰度画布计算。
- 逐帧特征仍分别基于各自解码帧计算。

个别帧失败时：

- 失败槽位使用确定性占位格；
- 占位格背景 luma 固定为 96；
- 两条对角线 luma 固定为 192；
- 单格最短边小于 64 像素时线宽为 1 像素，否则为 2 像素；
- 成功帧及其特征保持有效；
- 联系表标记为部分成功，逐帧错误继续上报；
- 六帧全部失败时不生成联系表。

## 缩略图缓存路径

缓存根目录继续使用 Agent 配置字段 `thumb.cache_dir`。

- 空值默认解析为 `<data_dir>\thumbcache`。
- 显式相对路径相对于 `data_dir` 解析，不得相对于进程工作目录解析。
- 解析结果必须是绝对路径。
- 缓存根目录不得等于任何媒体扫描根目录，也不得位于扫描根目录之下。
- 缓存根目录不得包含媒体扫描根目录。

最终内容寻址路径为：

```text
<thumb.cache_dir>\vc-grid-v1\<sha512前2位>\<完整小写sha512>.jpg
```

sidecar 路径为：

```text
<完整小写sha512>.jpg.json
```

sidecar 记录：

- schema 版本和联系表管线版本；
- 源文件 SHA-512 和大小；
- JPEG SHA-256；
- 画布与单格尺寸；
- 六个采样时间和槽位状态；
- VideoCore 版本；
- FFmpeg 组件版本。

临时 JPEG 和 sidecar 与最终文件位于同一目录：

```text
<sha512>.jpg.tmp-<worker-pid>-<job-id>-<随机值>
<sha512>.jpg.json.tmp-<worker-pid>-<job-id>-<随机值>
```

Go 负责：

- 创建目录；
- 分配唯一临时路径；
- 验证最终路径仍位于缓存根目录；
- 同步文件；
- 计算 JPEG SHA-256；
- 校验源文件漂移；
- 原子替换 JPEG；
- 最后提交 sidecar。

VideoCore 只写分析请求中指定的临时 JPEG 路径。

同一个 Agent 上的相同内容共享联系表。文件内容变化后 SHA-512 改变，因此
得到新的缓存路径。现有作用域内清理逻辑可以移除超过一小时的临时文件。
任何实现都不得自动删除旧路径算法缓存或其他媒体文件。

## 取消、截止时间与恢复

- `vc_cancel_token` 内部使用原子取消标志。
- 任务 context 结束时，Go goroutine 调用 `vc_cancel_request`。
- 原生打开、探测、seek、读包和解码通过 `AVIOInterruptCB` 检查取消标志
  和单调时钟 deadline。
- 探测预算保持 15 秒。
- 每个请求采样点保持 20 秒预算。
- 视频任务继续使用 Agent 的 120 秒硬看门狗。
- CGO 返回后 Go 必须再次检查 context；已经取消的结果不得发布。
- 普通帧解码失败时，允许 flush 或重建解码器状态，并在可能时继续处理
  后续采样点。
- 不可恢复的解复用错误将剩余帧标记为失败，但保留之前完成的帧。
- 不轮询中断回调的原生挂起只能通过终止 Worker 进程恢复。
- 原生访问违例会终止 Worker 进程。
- 两类硬故障都由 Agent 记录当前任务失败，创建替代 Worker，等待 Ready，
  并在 Agent PID 不变的前提下继续后续任务。

## 文件漂移与发布规则

session 绑定已经打开的文件句柄。Go 仍需在打开前和原生分析后验证路径。

路径身份、大小或修改时间发生变化时：

- 清空 SHA 和全部派生字段；
- 不提交联系表临时文件；
- 不提交 sidecar；
- 不持久化任何部分帧；
- 结果状态返回 `stale`。

联系表发布顺序：

1. VideoCore 完成并关闭临时 JPEG。
2. Go 验证其为非空、常规 JPEG 文件。
3. Go 同步文件并计算 SHA-256。
4. Go 再次执行源文件身份和漂移检查。
5. Go 原子替换最终 JPEG。
6. Go 写入、同步并原子替换 sidecar。

同一 SHA 的并发写入允许竞争，但读取方只有在最终 JPEG 与 sidecar 的
SHA-256 和管线版本一致时，才能判定缓存命中。

## 错误阶段映射

稳定的 Go 和日志阶段名替换面向可执行程序的旧名称：

- `native_open`；
- `native_hash`；
- `image_decode`；
- `video_probe`；
- `video_frame`；
- `video_contact_sheet`；
- `feature_compute`；
- `thumb_cache`；
- `stale`。

顶层错误表示无法继续取得有效结果的失败。时长、联系表和每个帧槽位具有
独立状态。单帧失败不得清空其他帧。正常业务条件下，只有 `stale` 会使
全部派生字段失效。

## 配置迁移

删除：

- `thumb.ffmpeg_path`；
- `thumb.ffprobe_path`；
- `WPROC_FFMPEG`；
- `WPROC_FFPROBE`。

替换：

- `thumb.max_side` 替换为 `thumb.tile_max_side`。

保留：

- 缩略图缓存根目录；
- 探测与原生操作超时；
- 20 秒单帧超时；
- 120 秒 Worker 视频硬看门狗；
- Worker 数量；
- 图片内存上限；
- IPC 和结果尺寸护栏。

配置加载阶段一次性解析缓存根目录，并在启动 Worker 前拒绝缓存目录与媒体
扫描根目录重叠。

## 构建与打包

`videocore.dll` 使用现有 MSVC、CMake、vcpkg 工具链和 C++17 构建。
现有图片依赖继续静态链接。FFmpeg MSVC 导入库通过显式目标或绝对路径
链接，不使用全局 `link_directories`。

实现至少使用 libavformat、libavcodec、libavutil 和 libswscale。构建脚本
必须递归审计实际 DLL 依赖图并打包完整应用本地闭包，不得假设第一层导入
就是全部依赖。

构建流程：

1. 配置并构建 VideoCore。
2. 执行原生 CTest。
3. 验证精确的 `vc_*` 导出集合。
4. 生成 `internal\wproc\videocore\libvideocore.a`。
5. 使用 CGO 和新导入库构建 Worker。
6. 不使用 CGO 构建 Agent、GUI 和 Helper。
7. 创建全新 staging 目录。
8. 将 `videocore.dll` 和经审计的 FFmpeg DLL 闭包复制到 Worker 同级目录。
9. 断言不存在旧媒体 DLL、旧导入库、FFmpeg 可执行程序或 `tools` 运行目录。

空 PATH 运行是强制验收项。

## FFmpeg 供应链与许可证

当前 SDK 标识为开发快照 `N-125444-g6d72600a30-20260703`：

- libavcodec/libavformat 主版本 63；
- libavutil 主版本 61；
- libswscale 主版本 10。

在判定构建可分发前，仓库必须记录机器可读 FFmpeg 清单：

- 上游或分发方来源 URL；
- 精确版本或 commit；
- 完整 configure flags；
- 源码归档 SHA-256；
- 头文件、MSVC 导入库、MinGW 导入库和运行 DLL 哈希；
- 组件主版本；
- 许可证分类；
- 随包许可证、NOTICE 和对应源码说明。

动态链接不免除 LGPL/GPL 义务。未确认的 GPL、nonfree 或源码来源状态会
阻止“发布包可再分发”的验收结论，但不阻止本地工程验证。

Worker Ready 上报实际加载的 FFmpeg 版本。头文件、导入库和运行 DLL 主
版本不一致时，Worker 不得 Ready。

## 协议与持久化变更

- Worker SHA 查询和回复增加已有字段掩码及六帧掩码。
- 正常新媒体结果可以同时携带 Phase 1 和 Phase 2 字段。
- 现有消息继续使用 msgpack map 编码；新增可选字段不得复用旧数字含义。
- Agent 在一个数据库事务中提交同一合并结果中全部成功的文件、图片或视频
  特征及视频帧记录。
- 出错或未请求的字段不得覆盖已有记录。
- 旧数据和部分数据只请求缺失字段或帧掩码。
- 完整内容缓存命中时跳过原生解码和联系表生成。
- 数据库存储的联系表路径是 Agent 本机的绝对最终路径。

## 验证设计

### 原生测试

- ABI 版本、结构尺寸拒绝、精确导出集合和错误缓冲 NUL 结尾。
- SHA-512 标准向量和新旧精确一致性。
- 现有图片 fixture 的 PDQ、pHash、Sobel 逐字节一致性。
- 固定有效视频覆盖时长、旋转、像素宽高比、B 帧、短视频、竖屏和多编码。
- 六个精确采样身份和联系表行优先布局。
- 确定性占位像素和部分联系表状态。
- 截断容器、损坏 packet、纯音频、无视频流、不支持编码和零字节输入。
- 中文、emoji、空格、UNC 和 `\\?\` 长路径。
- 打开、探测、seek、读包、解码和 JPEG 编码阶段取消。
- 超时和取消必须返回不同状态。
- 成功、失败、取消循环后无原生句柄和内存持续增长。
- 独立 session 并发运行。

### Go 与集成测试

- 每个缓存未命中任务只有一个 session、一个媒体文件打开、一次 SHA 过程
  和一个正常 FFmpeg session。
- SHA 内容缓存完整命中时跳过解码。
- 部分缓存命中时只请求缺失字段和帧。
- 合并 Phase 1/Phase 2 结果和事务持久化。
- 内容寻址联系表路径和跨路径复用。
- 并发缓存写入不会暴露 JPEG/sidecar 不一致组合。
- 取消结果不得发布。
- 分析期间替换源文件会清空全部派生字段。
- 普通帧错误不会丢弃其他成功帧。
- 六帧全部失败时不生成联系表。
- 原生崩溃或挂起后替代 Worker Ready，Agent PID 保持不变。
- 替代 Worker 能继续完成后续有效媒体。

### 兼容性门禁

- SHA-512 和图片特征必须完全一致。
- 固定视频 fixture 的时长、选帧身份、尺寸和特征编码必须一致。
- 任意特征不一致都必须生成差分报告；不得静默放宽阈值或候选规则。
- 联系表不要求兼容旧中点缩略图，因为三列两行联系表是已确认的产品变更。

### 禁止外部程序门禁

- 生产 `internal/wproc` 媒体代码不包含 `exec.Command*`。
- 生产配置不包含 FFmpeg 可执行程序路径。
- 全新 staging 不包含 FFmpeg、ffprobe 或 ffplay 可执行程序。
- 媒体分析期间完整采样进程树，只允许 Agent 和 Worker，不得出现解码子进程。

### 打包与驻留门禁

- 全新 staging 在空 PATH 下能够启动并分析 fixture。
- 原生依赖递归闭包完整且全部哈希固定。
- `mediacore.dll`、`libmediacore.a` 和旧 `mc_*` 导出不存在。
- 短时性能回归记录相对当前基线的耗时、CPU、读取字节、句柄和原生内存。
- 最终发布验收包含 24 小时 Worker 驻留，检查原生内存增长、句柄增长、
  重启风暴和结果漂移。

## 迁移顺序

1. 引入新原生 ABI 和原生测试，生产流程暂时保持旧实现。
2. 增加新 CGO 绑定和聚焦 Go 测试。
3. 扩展 SHA 缓存查询、回复和合并结果持久化。
4. 将图片处理切换到 VideoCore，并证明逐字节一致。
5. 将视频处理切换到单 session 分析路径。
6. 增加六帧联系表缓存和结果路径更新。
7. 替换构建、打包、验证、验收和 README 契约。
8. 执行兼容性、故障隔离、空 PATH 和无子进程门禁。
9. 删除旧 DLL 代码、绑定、导入库、可执行程序配置和过时测试。
10. 执行最终构建、回归、打包审计和发布驻留测试。

## 风险与缓解措施

1. **原生挂起比子进程更难停止。**  
   使用 `AVIOInterruptCB`、原子取消、阶段 deadline 和现有 Worker 硬看门狗。

2. **FFmpeg 原生崩溃会终止 Worker。**  
   保持 Agent 不加载原生依赖，并验证替代 Worker Ready 和后续任务完成。

3. **直接库调用可能与 CLI 选择不同帧。**  
   固定相同 FFmpeg 构建，并要求固定 fixture 的帧和特征一致。

4. **合并 Phase 结果增大协议负载。**  
   使用固定数组、字段掩码和 IPC 尺寸护栏，不传输原始帧。

5. **联系表改变缩略图语义。**  
   使用版本化缓存路径，并把已确认的三列两行布局作为新输出契约。

6. **动态 DLL 搜索可能在 Worker Ready 前失败。**  
   将审计后的运行 DLL 闭包放在 Worker 同级目录，并在空 PATH 下测试。

7. **FFmpeg 分发许可证当前缺少完整记录。**  
   把来源与许可证清单设为发布门禁。

8. **内容寻址缓存会积累旧版本。**  
   隔离缓存版本，禁止自动删除，由管理员决定后续清理。

## 完成标准

只有同时满足以下条件，迁移才算完成：

- `videocore.dll` 是唯一原生计算 DLL；
- Worker 只使用 `vc_*`；
- 图片和视频计算通过兼容性门禁；
- 每个缓存未命中任务复用一个已打开媒体 session；
- 六帧生成已确认的三列两行联系表；
- 不再配置、打包或启动任何 FFmpeg 可执行程序；
- 原生取消和 Worker 硬恢复均通过；
- 缓存发布继续保持原子性和 stale 安全；
- FFmpeg 运行依赖和再分发证据完成固定；
- 最终干净发布包和发布驻留测试全部通过。
