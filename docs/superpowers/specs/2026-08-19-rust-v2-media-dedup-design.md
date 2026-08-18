# mySingerServer Rust V2 媒体去重系统设计

## 1. 状态与结论

本设计已由项目负责人于 2026-08-19 逐节确认。

项目采用 Rust 从零整体重写。新实现只面向 Windows x64，不兼容旧版代码、
协议、数据库或配置。产品由管理工具、计算节点和隔离 Worker 三类进程组成：

- `desktop.exe`：Slint 管理界面，可同时连接局域网内多个计算节点。
- `node.exe`：带 Windows 托盘图标的计算节点，持有本机 SQLite 和任务状态。
- `worker.exe`：隔离媒体解码、哈希及图片/视频特征计算。

单机模式由 `desktop.exe` 连接本机 `node.exe`，不需要 PostgreSQL；除跨机器
去重外，扫描、精确重复、相似图片、相似视频、预览和删除均可本地完成。
跨机器模式由一个 `desktop.exe` 同时连接多个 `node.exe`，并使用 PostgreSQL
保存中心数据和执行跨机器候选筛选。

本设计借鉴 Czkawka 的任务分类、重复组复核和低干扰界面理念，但不复用其
内部结构。新系统针对多计算节点、中心同步和按需二筛重新设计。

## 2. 目标

1. 使用清晰、短路径、易测试的 Rust 模块替换旧项目的混合职责结构。
2. 计算节点可独立保存数据并完成单机媒体去重。
3. 管理工具可通过手工配置的 `IP:端口` 同时控制多个局域网节点。
4. 使用 TCP + Protobuf 统一承载任务、进度、结果、预览、同步和删除命令。
5. 精确重复使用 MD5 生成候选，再用文件大小确认。
6. 相似图片使用 PDQ 一筛，以及“9 分块 pHash + 128 维 Sobel”联合二筛。
7. 相似视频固定抽取六帧，每帧使用与图片一致的两层筛选并计算平均分。
8. 单机结果保存在 SQLite；管理工具支持自动和手动同步到 PostgreSQL。
9. 默认删除到 Windows 回收站，并允许在设置中切换为永久删除。
10. 视频使用 FFmpeg 共享 DLL 解码，不启动任何 FFmpeg 可执行程序。

## 3. 强制实施约束

### 3.1 范围约束

- 不扩大已确认需求。新的功能设想必须先单独确认，不能在实现过程中顺手加入。
- 不实现旧协议、旧表结构、旧配置、旧接口或旧数据迁移兼容层。
- 不加入节点自动发现、账号、认证、权限、TLS、消息加密或证书管理。
- 不加入云服务、浏览器管理端、移动端、自动删除策略或无人值守删除。
- 不为算法结果保存算法版本字段，也不设计算法版本迁移机制。
- 不生成相似图片缩略图；图片预览只在用户选中时读取原文件。
- 不设计多个管理工具实例之间的主从、选举或任务协调。一个管理工具实例可连接
  多个节点，这已经覆盖本设计的多主机目标。

### 3.2 简洁实现约束

- 不添加过多防御性编程。配置、TCP 消息、数据库行和文件路径在进入系统边界时
  校验一次，内部使用已验证的强类型数据，不在每一层重复校验。
- 不创建只有一个实现的 Repository、Service、Provider 等空壳接口。
- 只有确实存在多个实现时才使用 trait，例如 `WindowsWalker` 与
  `EverythingEnumerator` 共同实现文件枚举接口。
- 不增加自动重试链、降级算法、备用数据库、备用数据目录或多套网络协议。
- 文件级失败只影响当前文件；批次继续运行。失败项由用户明确发起重试。
- 优先使用直接的数据流和小型模块，不为假设中的未来需求提前抽象。
- `unsafe` 只允许存在于 FFmpeg FFI 和必要的 Windows API 边界中，安全封装之外
  不传播裸指针或原生句柄。
- 实现第一步创建并持续维护项目根 `AGENTS.md`，详细记录代码设计目的、整体架构、
  crate 职责、关键数据流、实现方案、不可破坏的不变量以及构建/测试命令。每完成
  一个实施阶段同步更新相关章节，不把它写成冗长的过程日志。
- 审查只执行计划规定的规格一致性、编译、测试和验收门禁；不反复引入新的审查轮次。

### 3.3 第三方依赖约束

- 允许通过 Cargo 下载第三方 Rust 库。
- 允许下载第三方提供的 Windows x64 预编译包；原生依赖有可靠预编译包时，
  不要求在本项目中重新编译。
- 下载的原生包必须在依赖清单中固定供应方、版本、归档文件名、下载地址、
  SHA-256、许可证和实际发布文件白名单。
- 第三方依赖只在开发、构建或打包阶段下载，应用运行时不联网下载依赖。
- 发布包必须携带所用第三方许可证。

## 4. 目标工作区

```text
AGENTS.md                   # Rust V2 设计与实现的项目 Agent 文档
Cargo.toml
apps/
  desktop/                 # desktop.exe 组合入口
  node/                    # node.exe 组合入口与托盘生命周期
  worker/                  # worker.exe 组合入口
crates/
  core/                    # 领域类型、算法参数和通用错误
  protocol/                # Protobuf 生成类型与消息转换
  transport/               # TCP 分帧、请求复用和事件推送
  node-engine/             # 扫描、任务编排、Worker 池和本地分析
  node-store/              # SQLite 表、查询、事务和同步 outbox
  desktop-core/            # 多节点会话、中心同步和跨机器分析
  desktop-ui/              # Slint 页面、模型和交互状态
  media/                   # MD5、PDQ、分块 pHash、Sobel、视频评分
  media-ffmpeg/            # FFmpeg 动态加载与安全解码接口
  windows/                 # 托盘、回收站、路径和进程 Job Object
proto/
  node.proto
scripts/
  fetch-ffmpeg.ps1
  build-release.ps1
deploy/
  central-v2.sql           # PostgreSQL 新库建表脚本，由用户手动执行
third_party/
  ffmpeg-dependency.json
docs/
  ui-preview/rust-v2/
```

三个 `apps` 目录只负责装配依赖、启动运行时和处理进程生命周期，不放业务算法。
每个 crate 按功能拆分文件，例如 `scan/`、`tasks/`、`sync/`、`similarity/`，
避免再次形成包含大量无关职责的单文件。

Rust 标识符使用英文；文件和接口文档使用中文：

- 每个业务源码文件使用 `//!` 说明职责、输入、输出和依赖。
- 每个公开类型、函数、trait 和消息转换使用 `///` 说明语义与错误条件。
- 算法关键步骤注释“为什么这样做”、阈值含义和必须保持的不变量。
- 不为一眼可见的赋值或循环逐行写重复注释。
- 各业务 crate 启用 `#![warn(missing_docs)]`。

## 5. 进程与运行关系

```mermaid
flowchart LR
    D["desktop.exe / Slint"]
    PG[("PostgreSQL 中心库")]
    N1["node.exe / 节点 A"]
    N2["node.exe / 节点 B"]
    S1[("SQLite A")]
    S2[("SQLite B")]
    W1["worker.exe 池 A"]
    W2["worker.exe 池 B"]

    D <-->|"TCP + Protobuf"| N1
    D <-->|"TCP + Protobuf"| N2
    D <-->|"SQL"| PG
    N1 <--> S1
    N2 <--> S2
    N1 <-->|"匿名管道 + Protobuf"| W1
    N2 <-->|"匿名管道 + Protobuf"| W2
```

节点永远不直接连接 PostgreSQL。PostgreSQL 地址和凭据只保存在管理工具配置中。
节点任务在管理工具断线后继续运行，管理工具重新连接时重新查询任务和同步游标。

`worker.exe` 由 `node.exe` 创建并放入 Windows Job Object。节点通过重定向的
标准输入输出传输长度前缀 Protobuf 消息，不额外开放本机端口。媒体 DLL 只被
Worker 加载，因此解码器崩溃不会直接终止节点或桌面界面。

## 6. 便携式目录与数据位置

所有持久化运行数据放在可执行文件所在目录的 `data` 下，不使用
`%LOCALAPPDATA%`，也不设计其他目录回退。

```text
应用程序目录/
  desktop.exe
  node.exe
  worker.exe
  runtime/
    ffmpeg/
      avcodec-*.dll
      avformat-*.dll
      avutil-*.dll
      swscale-*.dll
      其他必需运行 DLL
  licenses/
  data/
    desktop/
      config.toml
      cache/
      logs/
    node/
      config.toml
      node.sqlite3
      cache/
        video-contact-sheets/
      logs/
```

路径基准由 `current_exe()` 的父目录确定，不能使用进程当前工作目录。首次启动
创建缺失目录和配置。发布包不携带会覆盖用户配置的 `data` 内容；升级只替换
程序与运行库。应用必须部署在当前用户具有写权限的目录。

`data/desktop/config.toml` 保存手工配置的节点地址、PostgreSQL 连接信息、
界面设置、相似度阈值和删除模式。`data/node/config.toml` 保存监听 IP、端口、
Worker 数量和枚举器选择，不保存可复制的随机机器 ID。

节点每次启动都从物理机器信息计算唯一 ID。输入固定为 SMBIOS System UUID、
系统序列号和主板序列号，按该顺序去除首尾空白、转换为大写并用 NUL 分隔，再对
`mysingerserver-v2-machine\0 + 输入` 计算 SHA-256，使用 64 个小写十六进制字符
作为 `MachineId`。空字段跳过；三个字段都不可用时节点启动失败。复制整个应用与
`data` 到另一台物理机器不会复制节点身份。

## 7. 核心领域模型

### 7.1 文件与内容

- `MachineId`：由 SMBIOS 物理机器信息按固定规则计算的 SHA-256 字符串。
- `NormalizedPath`：用于比较和索引的 Windows 绝对路径；比较不区分大小写。
- `DisplayPath`：保留原始大小写、供界面显示和实际访问的路径。
- `FileLocation`：机器 ID、路径、文件大小、内容引用和活动状态。
- `Content`：以内部 `content_id` 标识，保存 MD5、文件大小和媒体类型。
- `Md5Digest`：固定 16 字节，不使用十六进制字符串参与内部比较。

`Content` 不直接以 MD5 作为唯一主键。MD5 建普通索引，查到后再比较文件大小；
只有 MD5 与大小都一致才复用内容和特征。这与精确重复的判定顺序一致，不额外
增加 SHA 或逐字节复核。

### 7.2 特征

- `ImageStage1`：宽、高、PDQ-256 和 PDQ Quality。
- `ImageStage2`：9 个 64 位分块 pHash、128 个 `f32` Sobel 特征。
- `VideoFrameStage1`：帧槽位、时间位置、宽、高、PDQ-256 和 Quality。
- `VideoFrameStage2`：帧槽位、9 分块 pHash 和 128 维 Sobel。
- `VideoMetadata`：时长、宽、高和六个固定采样槽位。
- `ContactSheetRef`：节点本地联系表路径，只用于显示。

所有特征按内容保存，同一节点上的多个路径以及不同节点中 MD5 与大小相同的内容
共享一份特征。数据库和 Protobuf 消息都不携带算法版本字段。

### 7.3 任务与结果

- `Task` 状态固定为 `queued`、`running`、`completed`、`failed`、`cancelled`。
- `TaskItem` 保存单个文件的等待、运行、成功或失败结果。
- `AnalysisRun` 保存分析模式、选定节点、输入扫描任务、同步高水位、阈值快照和
  分析状态。
- `CandidatePair` 保存一筛候选两端及一筛分数。
- `DuplicateGroup` 保存精确、相似图片或相似视频的最终成员集合。
- `ReviewMark` 保存重复组成员的“未决定、保留、删除”复核状态。
- `SyncChange` 是 SQLite outbox 中带单调序号的同步变更。
- `DeleteBatch` 与 `DeleteItem` 保存删除计划和逐文件结果。

任务级 `failed` 只用于任务无法继续的错误；单个文件失败累加到任务统计，不让
整个批次失败。

## 8. SQLite 与 PostgreSQL 职责

### 8.1 节点 SQLite

SQLite 至少包含以下逻辑表：

- `files`：机器、规范路径、显示路径、大小、内容引用、活动状态。
- `contents`：MD5、大小、媒体类型和基础元数据。
- `image_stage1`、`image_stage2`：图片两层特征。
- `video_metadata`、`video_frame_stage1`、`video_frame_stage2`：视频及六帧特征。
- `contact_sheets`：视频联系表本地缓存引用。
- `tasks`、`task_items`：可恢复任务和逐文件进度。
- `analysis_runs`：本地分析状态、阈值快照和输入任务。
- `analysis_run_inputs`：分析创建时冻结的内容键和文件位置，不随后续扫描扩张。
- `candidate_pairs`：本地一筛及二筛候选对和分数。
- `duplicate_groups`、`group_members`：本地最终重复组和成员。
- `review_marks`：本地复核选择。
- `sync_outbox`：待同步变更及单调序号。
- `sync_state`：中心已确认序号和已经清理的最高 outbox 序号。
- `delete_batches`、`delete_items`：删除结果和墓碑。

所有节点写入由 `node.exe` 串行提交；Worker 不直接访问 SQLite。SQLite 使用 WAL，
一次文件计算结果及其 outbox 记录在同一事务提交。

`node.exe` 在 `data/node/node.sqlite3` 不存在时直接创建当前 V2 全部表和索引。
本项目不兼容旧 SQLite；已有文件不符合当前新结构时节点明确报错，不执行旧表迁移。

### 8.2 中心 PostgreSQL

PostgreSQL 保存：

- 节点和最后连接状态；
- 已同步文件位置、内容、媒体元数据和两层特征；
- 每个节点的已提交同步游标；
- 跨机器分析运行、不可变输入、阈值快照、一筛候选、二筛等待项和最终重复组；
- 中心重复组的复核标记；
- 删除墓碑与逐节点删除结果。

PostgreSQL 不由 `desktop.exe` 自动建表或迁移。仓库提供完整的
`deploy/central-v2.sql`，用户在空数据库中手动执行一次；管理工具连接后只检查
所需表和列是否存在，不符合时禁用中心模式并显示建库脚本路径。

视频联系表 JPG 不上传 PostgreSQL。中心库只保存拥有该内容的节点和引用，界面
需要显示时再通过 TCP 从在线节点读取。

## 9. 扫描与内容复用

扫描流程固定为：

1. 把本次扫描根的规范路径集合持久化到扫描任务，再枚举完整文件列表，生成规范
   路径、显示路径和文件大小。
2. 使用“机器 ID + 规范路径 + 文件大小”批量查询 SQLite。
3. 命中时复用已有 MD5 和内容引用，跳过文件读取及 MD5 计算。
4. 未命中时读取文件并计算 MD5。
5. 使用 MD5 索引查询 `contents`，再比较文件大小。
6. MD5 与大小都已存在时，只新增或更新路径引用，复用媒体信息和已有特征。
7. 新内容才进入媒体探测和一筛特征计算。
8. 只有枚举和扫描完整成功后，才把“同一机器、严格位于本次扫描根内、但本轮未
   出现”的旧路径标记为非活动并写入同步 outbox。路径归属使用规范化目录边界
   判断，不能使用字符串前缀。任务失败或取消时不执行路径失效。

修改时间不参与跳过条件。若某文件在同一路径被替换且大小保持不变，普通扫描会
继续复用原数据；用户通过任务中的“强制重新计算”选项忽略缓存并重算 MD5 和特征。

一筛只读取所需数据完整的内容：图片必须同时有宽、高、PDQ 和 Quality；视频必须
完成六个均匀采样槽位的提取记录，并至少有四个成功帧，每个成功帧都必须包含宽、
高、PDQ 和 Quality。数据不完整的内容直接跳过并计入 `skipped_incomplete`，不自动
补算。用户只有通过“强制重新计算”或显式重试失败项才会重新计算缺失的一筛数据。

文件枚举提供两个真实实现：

- `WindowsWalker`：使用 Windows 文件系统 API 递归枚举，始终可用。
- `EverythingEnumerator`：用户明确启用且本机依赖可用时使用。

两者输出相同的 `ScannedPath`，后续扫描流程不感知枚举来源。不增加第三种备用
枚举器，也不在任务运行中反复切换实现。

## 10. 精确重复

精确重复按照以下顺序判定：

1. 通过 MD5 索引聚合可能相同的内容。
2. 在相同 MD5 内再按文件大小分组。
3. 同一 MD5 与大小组合包含至少两个活动路径时形成精确重复组。

判定不使用修改时间，不计算额外 SHA，也不逐字节复核。组成员可以来自同一机器、
不同机器或同一内容的多个磁盘路径。

## 11. 相似图片算法

### 11.1 固定特征定义与可配置阈值

特征提取参数全部硬编码，不进入配置：

- 图片和视频帧都先转换为 RGB24，再按
  `Y = (77×R + 150×G + 29×B + 128) >> 8` 得到 8 位灰度面。
- 所有缩放使用像素中心对齐的双线性插值，边缘坐标钳制到最近像素。
- PDQ 使用固定提交的 Meta ThreatExchange PDQ 参考算法进行 Rust 等价移植，
  参考提交记录在 `third_party/pdq/UPSTREAM.md`；32 字节结果按参考实现的大端
  位序保存，并用官方向量做逐位一致测试。
- 分块 pHash 把灰度面缩放到 `96×96`，按行优先切成 9 个 `32×32` 块。每块执行
  固定二维 DCT-II：余弦项为 `cos((2x+1)uπ/64)`，系数乘
  `0.25×cu×cv`，其中 `u/v=0` 时 `cu/cv=1/√2`，否则为 `1`。取左上 `8×8`
  （包含 DC）共 64 个系数，以全部 64 个系数的上中位数为阈值；行优先第 `i` 个
  系数大于中位数时设置 `uint64` 的 bit i。
  九个 `uint64` 按块行优先保存，数据库 BLOB 使用小端字节序。
- Sobel 把灰度面缩放到 `128×128`，忽略一像素边界，使用标准 `3×3` Sobel 计算
  `gx/gy`，幅值固定为 `|gx| + |gy|`。幅值小于 `1e-6` 的像素跳过；无符号方向
  `[0,π)` 直接量化到 8 个不插值的 bin，并按 `4×4` 个 `32×32` 空间格累加，
  得到 128 维向量后做 L2 归一化。范数不大于 `1e-9` 时保存全零向量。
- Sobel 比较使用余弦：双方全零为 `1.0`，仅一方全零为 `0.0`，其他情况使用点积。

上述定义不保存算法版本，也不允许运行时切换。未来若明确修改任何特征定义，必须
清空相关一筛/二筛特征并全部重算，不能把新旧特征混合比较。

匹配阈值允许配置，因为阈值只消费既有特征，不改变特征字节。管理工具从
`data/desktop/config.toml` 读取阈值，每次创建本地或中心 `AnalysisRun` 时完整保存
阈值快照，并把同一快照发送给节点。本设计默认值为：

| 配置项 | 默认值 |
|---|---:|
| `pdq_quality_min` | `50` |
| `aspect_tolerance` | `0.10` |
| `pdq_hamming_max` | `31` |
| `phash_part_hamming_max` | `10` |
| `phash_min_passed_parts` | `8` |
| `sobel_min` | `0.85` |
| `video_min_valid_frames` | `4` |
| `video_stage1_min` | `0.80` |
| `video_stage2_min` | `0.80` |

### 11.2 一筛

图片只解码一次得到灰度面，并计算：

- PDQ-256；
- PDQ Quality；
- 宽和高。

使用当前 `AnalysisRun` 阈值快照的一筛条件为：

- 两端 PDQ Quality 均不低于 `pdq_quality_min`；
- 长宽比相对差不超过 `aspect_tolerance`；
- PDQ-256 汉明距离不超过 `pdq_hamming_max`。

候选生成使用 PDQ 的四个 64 位 band 倒排索引，共享任一 band 的内容进入精确
阈值检查。该结构简单、内存可控，但相似匹配本身是近似召回，不承诺汉明距离
`4..31` 的所有组合都必然进入候选。本版不增加第二套错位索引或复杂向量数据库。

图片一筛分数为：

```text
stage1_score = 1 - pdq_hamming / 256
```

### 11.3 联合二筛

一筛候选需要的二筛特征在一次 Worker 任务、一次解码中共同计算：

- 把灰度图缩放到 `96×96`，按 `3×3` 分成 9 块；每块计算一个 64 位 pHash。
- 把灰度图缩放到 `128×128`，计算 `4×4` 空间格 × `8` 个方向 bin，得到
  128 维 L2 归一化 Sobel 结构特征。

使用当前 `AnalysisRun` 阈值快照的通过条件为：

- 每个 pHash 分块汉明距离不超过 `phash_part_hamming_max`；
- 至少 `phash_min_passed_parts` 个分块通过；
- Sobel 余弦相似度不低于 `sobel_min`。

pHash 和 Sobel 是同一个二筛的联合条件，不拆成第三筛，也不分别派发任务。
最终图片分数使用 Sobel 余弦相似度，并同时展示 pHash 通过块数。

## 12. 相似视频算法

### 12.1 固定抽帧

视频在时间轴上均匀抽取六帧。令槽位 `i ∈ [0,5]`，采样位置硬编码为：

```text
t[i] = duration × (2×i + 1) / 12

即 1/12, 3/12, 5/12, 7/12, 9/12, 11/12
```

每个槽位保存自己的图片一筛特征；二筛只为候选视频按需补算。不同视频按相同
槽位比较，不进行镜头匹配、动态时间规整或额外关键帧搜索。

### 12.2 视频一筛

每个对齐帧使用当前分析运行中的图片一筛阈值。双方都成功解码的同一槽位是
“有效对齐帧”；其中任一端 Quality 或长宽比未通过，或 PDQ 汉明超过阈值时，
该有效帧分数为 `0`。全部条件通过时为 `1 - hamming / 256`。任一端解码失败的
槽位不进入分母。

有效对齐帧数必须达到 `video_min_valid_frames`；对有效帧求平均值。一筛平均分
不低于 `video_stage1_min` 时形成视频候选。候选生成使用六个槽位各自的 PDQ band
索引取并集，再执行完整平均分计算。

### 12.3 视频联合二筛

每个有效对齐帧执行图片联合二筛：

1. 若 9 分块 pHash 中至少 `phash_min_passed_parts` 块的汉明距离不超过
   `phash_part_hamming_max`，该帧进入 Sobel 判定。
2. pHash 未通过时帧分数为 `0`。
3. pHash 通过时，帧分数等于该帧 Sobel 余弦相似度。
4. 有效帧数达到 `video_min_valid_frames` 后，对有效帧求平均值。
5. 平均分不低于 `video_stage2_min` 时形成最终相似视频组。

二筛结果必须持久化：图片写入 `image_stage2`，视频每帧写入
`video_frame_stage2`，随后写同一 SQLite 事务的 outbox。管理工具派发二筛前先
查询 PostgreSQL 是否已有完整结果；节点收到任务后再查询 SQLite。任一数据库已
有当前内容的完整 pHash 与 Sobel 时都跳过 Worker 计算；SQLite 已有而 PostgreSQL
尚无时只补写/重放同步变更。缺少结果才派发或执行计算，失败项不自动重试。

### 12.4 视频联系表

联系表只用于界面预览，不参与任何相似度计算：

- 复用上述六个采样帧，不额外抽帧；
- 使用 `3×2` 网格合并；
- 画布像素格式为 RGB24；
- 编码为 JPG，质量固定为 80；
- 缺失槽位使用固定灰色单元格；
- 按内容缓存在 `data/node/cache/video-contact-sheets`。

FFmpeg 负责解码和 RGB24 转换；网格合成与 JPG 质量 80 编码由 Rust 完成。

## 13. TCP + Protobuf 协议

### 13.1 连接与分帧

- 节点监听 `data/node/config.toml` 中手工配置的 IP 和端口。
- 管理工具为每个节点维护一条持久 TCP 连接。
- 每个节点只允许一个活动管理连接；第二个连接返回 `NODE_BUSY` 后关闭。一个管理
  工具同时连接多个节点，不等于一个节点接受多个管理工具。
- 不使用 HTTP、JSON、WebSocket 或 SSE。
- 不进行节点发现、认证、加密或 TLS。
- 每帧格式为“4 字节大端无符号长度 + Protobuf `Envelope`”。
- 普通 Envelope 硬限制为 8 MiB，文件和同步快照使用 1 MiB 的 `FileChunk`，不放进
  单个巨大消息。

`Envelope` 包含 `request_id` 和 `oneof payload`。大于零的 `request_id` 用于
请求、响应和同一请求的分块；节点主动推送的任务进度、完成和状态变化使用
`request_id = 0`。`transport` crate 负责请求 ID、写队列和响应分派，业务 crate
不直接读写 TCP 字节。所有主动事件必须携带 `task_id` 和该任务单调递增的
`event_seq`；重连后管理工具以任务查询结果为准，不要求补发旧事件。

单连接内部使用两个有界发送队列：任务控制、进度、删除和同步 ACK 属于高优先级；
原图、联系表和全量快照分块属于低优先级。每发送一个低优先级块都重新检查高优先
级队列，避免大文件预览阻塞任务控制。

协议和数据库边界不传输节点本地 `content_id`。内容统一使用
`ContentKey{md5[16], file_size}`，文件位置使用
`LocationKey{machine_id, normalized_path}`；PostgreSQL 自行映射中心 `content_id`。

该协议按用户要求不设认证和加密，因此配置为局域网地址后，任何能连接该端口的
客户端都具有提交任务和删除文件的能力。系统把所配置局域网视为可信网络。

### 13.2 主要消息

协议至少包含：

- 节点信息与状态查询；
- 创建、取消、查询和列出任务；
- 任务确认、进度、逐文件错误和完成事件；
- 节点路径浏览和扫描请求；
- 创建本地分析、查询分析运行、分页查询重复组和成员、保存复核标记；
- 分页读取分析运行的不可变输入快照；
- 一筛结束后的批量二筛任务；
- 同步增量拉取、全量快照、提交确认和 outbox ACK；
- 原图、视频联系表及文件分块读取；
- 删除计划、逐文件删除结果和失败项重试。

协议只服务新版本，不保留旧字段映射或协商旧协议。桌面和节点必须使用同一次
发布生成的 `.proto` 类型。

## 14. 节点任务与 Worker 生命周期

`node-engine` 负责持久化任务、按文件生成 `TaskItem`、调度 Worker、合并结果并
推送进度。Worker 池只处理 CPU/媒体工作，不管理网络或数据库。

- 单个文件失败后记录阶段和错误文本，继续下一个文件。
- Worker 崩溃时，当前文件标记失败，节点创建替代 Worker；不自动重试该文件。
- 取消任务时，等待项直接标记取消；正在处理该任务的 Worker 被终止并替换。
- 节点启动时把遗留 `running` 项恢复为 `queued`，已完成项不重复计算。
- 管理工具断线不取消节点任务；重连后从 SQLite 状态恢复界面进度。

“意外 Worker 崩溃”和“计划重启计算引擎”使用不同路径。计划重启先把 Worker 池
置为 `restarting`，在 SQLite 事务中把池内运行项改回 `queued`，再终止 Worker，
并抑制本次退出的崩溃失败处理；意外退出才把当前文件标记为失败。

选定节点仍存在 `queued` 或 `running` 的扫描/一筛计算任务时，管理工具禁用
“开始筛选”。单机模式等待本节点全部相关计算结束后才向节点提交本地分析；跨机
器模式等待所有选定节点相关计算结束，并达到各自同步高水位后才开始中心一筛。
分析已经进入一筛后生成的按需二筛任务属于该分析运行，不阻止该运行继续完成。
这里的“计算结束”要求相关任务状态为 `completed`；任务内允许存在已经记录的单文件
失败并按不完整数据跳过。任务级 `failed` 或 `cancelled` 不满足门禁，用户必须重试
任务或从本次运行中移除对应节点后才能开始。

`node.exe` 是无控制台 Windows 托盘程序。右键菜单固定包含：

- 当前运行状态与监听地址；
- 打开日志目录；
- 重启计算引擎；
- 退出节点。

“重启计算引擎”只重建 Worker 池，不清空 SQLite。正在运行的项目回到队列，
等待重建后的 Worker 继续执行。

## 15. SQLite 到 PostgreSQL 同步

### 15.1 自动同步

当管理工具已连接节点且 PostgreSQL 可用时，自动执行：

1. 从 PostgreSQL 读取该机器最后成功提交的同步游标。
2. 先向节点发送一次幂等的 `SyncAck(center_cursor)`，使“中心已提交但上次 ACK
   丢失”的旧行也能清理。
3. 通过节点协议按游标拉取 SQLite `sync_outbox` 变更。
4. 每批固定最多 1000 条。
5. 在一个 PostgreSQL 事务中 UPSERT 本批数据，并更新该机器中心游标。
6. 事务提交成功后，管理工具向节点发送 `SyncAck(committed_seq)`。
7. 节点把 `committed_seq` 写入 `sync_state.acked_seq`，随后才允许删除不大于该
   序号的 outbox 行，并把 `pruned_through_seq` 更新为实际已经清理的最高序号。
8. ACK 成功或连接中断后才处理下一批；重复 ACK 幂等。
9. 新的本地结果继续触发后续批次，直到追平节点最新序号。

事务失败时不推进游标，不设计备用写入路径。重复拉取通过内容键和序号 UPSERT
保持幂等。

`acked_seq` 和 `pruned_through_seq` 初始均为 `0`。若 PostgreSQL 游标小于节点
`pruned_through_seq`，说明所需增量已经清理，节点返回
`SNAPSHOT_REQUIRED`。管理工具启动一次 SQLite 只读快照；节点记录快照开始时的
`snapshot_high_seq`，按表和内容键分页发送当前基础数据及仍有效的删除墓碑。管理
工具在 PostgreSQL 完成快照事务后把中心游标设为 `snapshot_high_seq`，再继续拉取
更大序号的 outbox，并发送 `SyncAck(snapshot_high_seq)`。快照连接中断时整次重来，
不增加复杂的分块恢复状态。

### 15.2 手动同步

界面提供“立即同步”按钮，使用与自动同步完全相同的代码路径。节点曾经离线单机
运行产生的 SQLite 结果，在之后连接管理工具和 PostgreSQL 后可从旧游标开始分批
上传，不要求用户复制或直接打开 SQLite 文件。

同步范围包括：

- 文件位置、活动状态和内容 MD5/大小；
- 媒体元数据；
- 图片与视频的一筛和二筛特征；
- 删除结果及墓碑。

视频联系表 JPG 和原媒体文件不上传中心库。

## 16. 跨机器去重编排

### 16.1 分析运行状态

本地和中心分析都持久化相同状态：

```text
collecting_stage1
-> stage1_synced
-> screening
-> phase2_dispatched
-> phase2_synced
-> finalizing
-> completed / partial / cancelled
```

创建 `AnalysisRun` 时固定输入扫描任务、选定节点和完整阈值快照。节点完成扫描及
一筛特征计算后，在任务完成结果中返回当前 `outbox_high_seq`。跨机器分析必须同时
满足以下两个条件才能从 `collecting_stage1` 进入 `stage1_synced`：

1. 所有选定节点的相关计算任务都已完成，任何节点仍在计算时不能启动筛选；
2. PostgreSQL 中每个节点的中心游标都达到该节点任务返回的 `outbox_high_seq`。

达到高水位后，本次运行只消费已固定的输入任务和内容集合；之后自动同步进来的
新扫描数据留给下一次分析，不修改当前候选集合。

为使“固定输入”可执行而不是只靠时序约定，节点从所选已完成任务的 `TaskItem`
生成去重后的 `(ContentKey, LocationKey)` 列表，并在分页返回给管理工具的同时写入
本地 `analysis_run_inputs`。中心模式由管理工具把相同列表写入 PostgreSQL 的
`analysis_run_inputs`；本地模式直接使用 SQLite 中的列表。一筛只从该表连接内容
及特征，后续扫描或同步的新位置不会进入本运行。位置在快照后失效时仍保留历史
结果，但界面按当前活动状态禁用文件操作。

本地运行不需要网络同步；相关扫描任务完成并生成输入快照后，直接把状态从
`collecting_stage1` 置为 `stage1_synced`，该状态在本地表示“一筛输入已就绪”。

### 16.2 跨机器流程

跨机器分析由 `desktop-core` 使用 PostgreSQL 编排：

1. 使用中心库中的 MD5/大小生成精确重复组。
2. 只读取一筛数据完整的内容；不完整记录直接跳过并累计统计。
3. 使用中心库中的 PDQ、Quality、尺寸和视频帧数据完成全部相似一筛。
4. 把完整候选集合持久化后，查询候选两端的联合二筛特征。
5. 数据库已有完整 pHash 与 Sobel 的内容直接复用，不生成二筛任务。
6. 一筛全部结束后，才把剩余缺失特征按拥有内容的节点分组并批量派发。
7. 节点再次查询 SQLite；本地已有完整结果时跳过 Worker，只确保结果进入 outbox。
8. 缺失结果由节点一次解码共同计算 pHash 和 Sobel，持久化 SQLite 和 outbox。
9. 管理工具继续按每批 1000 条自动同步二筛结果到 PostgreSQL。
10. 每个节点完成本运行的二筛批次时返回新的 `outbox_high_seq`；管理工具等待所有
    已派发批次进入终态，并等待 PostgreSQL 游标达到各自的新高水位。
11. 所需二筛结果齐全后统一执行最终判定并生成相似组，不在节点仍计算时增量筛选。

同一内容存在于多个节点时，优先选择在线且已有可访问路径的一个节点计算；中心
特征按 MD5 与大小复用。节点离线或二筛失败导致候选无法完成时，运行状态为
`partial`，保留已完成结果和未解决候选。系统不自动重试；用户在节点恢复或修复
文件后明确重试，随后可继续同一运行。`partial` 允许在显式重试时回到
`phase2_dispatched`；缺失二筛数据的候选保留为未解决，不按零分判为不相似。

### 16.3 本地 SQLite 分析

单机模式由管理工具通过 TCP 创建本地分析任务，但全部分析在 `node.exe` 中直接
使用 SQLite 完成：

1. 等待本节点相关计算任务全部完成。
2. 在 `analysis_runs` 写入输入扫描任务和阈值快照。
3. 从 SQLite 完成精确分组和相似一筛，把候选写入 `candidate_pairs`。
4. 查询 SQLite 二筛结果，仅对缺失内容批量调度 Worker。
5. 把二筛结果写入 SQLite 后完成最终筛选。
6. 把最终组与成员写入 `duplicate_groups` 和 `group_members`。
7. 管理工具通过分页协议读取运行、组、成员和复核标记，不直接打开 SQLite。

### 16.4 代表文件分组规则

精确重复直接按 MD5 与大小成组。相似图片和相似视频不使用传递闭包，而采用代表
文件中心规则：

1. 按 `ContentKey(md5, file_size)` 升序遍历尚未分组的内容。
2. 当前内容的活动位置按 `(machine_id, normalized_path)` 升序，第一项作为代表文件。
3. 只加入与代表内容存在“直接通过最终二筛”的尚未分组内容，并展开这些内容的
   全部活动文件位置。
4. 不因为新成员与第三个内容相似而继续扩张；A≈B、B≈C、A≉C 时不会把 C 经 B
   链式加入 A 的组。
5. 每个内容在一次分析运行中最多属于一个相似组；相似组必须至少包含两个不同的
   `ContentKey` 且至少有两个文件位置，避免把纯精确重复再次显示为相似组。

组成员保留其与代表内容的直接一筛和二筛分数，保证界面和删除复核可解释且结果
顺序确定。

## 17. Slint 界面与交互

管理工具使用 Slint。UI 只操作 `desktop-core` 暴露的状态和命令，不直接访问
TCP、SQLite 或 FFmpeg。大文件列表和重复组使用虚拟化模型，避免一次创建全部
控件。组和成员采用稳定游标分页；本地模式经节点协议查询 SQLite，中心模式查询
PostgreSQL，UI 使用同一组视图模型。

界面包含：

1. 总览与节点；
2. 扫描与任务；
3. 精确重复；
4. 相似图片；
5. 相似视频；
6. 跨机器分析；
7. 删除复核；
8. 设置与诊断。

已确认的整套预览图：

![总览与节点](../../ui-preview/rust-v2/01-overview-nodes.png)

![扫描与任务](../../ui-preview/rust-v2/02-scan-tasks.png)

![精确重复与跨机器分析](../../ui-preview/rust-v2/03-exact-cross-machine.png)

![相似图片与相似视频](../../ui-preview/rust-v2/04-similar-media.png)

![复核与删除](../../ui-preview/rust-v2/05-review-delete.png)

![设置与诊断](../../ui-preview/rust-v2/06-settings-diagnostics.png)

### 17.1 图片预览

图片不生成或缓存缩略图。用户选中成员时，管理工具向拥有该路径的节点请求原图，
按块读取到内存，由 Slint 按视图尺寸缩放显示。节点离线时仍显示中心记录和分数，
但禁用预览、打开路径和删除操作。

### 17.2 视频预览

视频列表显示节点本地生成的联系表 JPG。管理工具只在需要显示时从在线节点读取，
中心库不保存二进制图片。

### 17.3 复核操作

每个重复组支持标记“保留”和“删除”，并提供按大小、分辨率、Quality 或路径的
快捷选择。快捷选择只改变复核标记，永远不会直接执行删除。相似结果同时显示
一筛分数、pHash 通过块数和 Sobel/视频平均分。

本地复核标记保存到节点 SQLite `review_marks`，中心复核标记保存到 PostgreSQL，
都以 `analysis_run_id + group_id + location_key` 定位。管理工具重启或重新连接后
恢复已保存标记。每组执行删除前必须至少保留一个仍活动的文件。

## 18. 删除流程

默认模式为移动到 Windows 回收站；设置中可切换为永久删除。执行前固定显示：

- 文件数量；
- 涉及节点；
- 预计释放空间；
- 当前删除模式；
- 每组保留文件。

用户确认后，管理工具按拥有节点拆分删除计划并通过 TCP 发送。节点对每个文件只
执行一次明确的身份检查：

1. 路径仍存在；
2. 当前文件大小等于计划大小；
3. 重新计算的当前 MD5 等于计划 MD5。

任一条件不符时跳过该文件。回收站模式使用 Windows Shell 回收站能力，永久模式
直接删除。逐文件结果固定为：

- `recycled`；
- `deleted`；
- `skipped`；
- `failed`。

成功项写入 SQLite 墓碑并自动同步 PostgreSQL。系统不实现分布式事务；不同节点
的部分成功结果原样保留，界面只允许重新提交失败或跳过后经用户重新确认的项目。

删除任务完成后立即更新产生该删除计划的重复组，不等待下一次扫描：

- `recycled` 和 `deleted` 项从对应 `group_members` 直接删除，并把文件位置设为
  非活动；同一事务写删除结果、墓碑和 outbox。
- `skipped` 与 `failed` 项保留在组内并显示结果，不假定文件已经消失。
- 组剩余活动成员少于两个时直接删除该重复组及复核标记。
- 若原代表文件被删除，删除计划中明确保留的第一个活动文件成为新代表；不重新
  执行相似筛选，也不通过其他成员扩张分组。
- 本地组由节点更新；中心组由管理工具在收到逐文件结果后于 PostgreSQL 事务中
  更新。跨节点部分成功时只删除已经成功的成员。

## 19. FFmpeg DLL 运行时

### 19.1 来源与发布

FFmpeg 官方只发布源码，因此 Windows DLL 使用第三方预编译包。依赖选择固定为
[BtbN FFmpeg Builds](https://github.com/BtbN/FFmpeg-Builds) 提供的 Windows x64
LGPL shared 构建，目标 FFmpeg 系列为 8.0.1。

`scripts/fetch-ffmpeg.ps1` 根据 `third_party/ffmpeg-dependency.json` 下载归档并
校验 SHA-256，只提取允许清单中的头文件、导入信息、运行 DLL 和许可证。发布包
不包含 `ffmpeg.exe`、`ffprobe.exe` 或 `ffplay.exe`。

当前 `G:\Code\ffmpeg-8.0.1-full_build\bin` 是仅包含可执行程序的静态工具包，
不作为新系统运行依赖，也不要求配置全局 PATH。

### 19.2 Rust 边界

FFmpeg DLL 相对位置硬编码为 `worker.exe` 所在目录下的 `runtime\ffmpeg`，不能
通过配置、当前工作目录或系统 PATH 覆盖。Worker 使用 `current_exe()` 得到绝对
目录，调用 `SetDefaultDllDirectories(LOAD_LIBRARY_SEARCH_SYSTEM32 |
LOAD_LIBRARY_SEARCH_USER_DIRS)`，再用 `AddDllDirectory` 加入该固定目录，并按依赖
清单中的固定文件名和顺序通过带相同搜索标志的 `LoadLibraryExW` 加载 `avutil`、
`swresample`、`swscale`、`avcodec`、`avformat` 及清单列出的传递 DLL；加载只搜索
`runtime\ffmpeg` 和 System32，任一必需 DLL 缺失时 Worker 启动失败。

`media-ffmpeg` 把动态加载的 FFmpeg C API 封装为少量安全接口：

- `probe_media(path)`；
- `decode_frame_at(path, position)`；
- `convert_to_rgb24(frame)`。

FFmpeg 结构体、错误码、裸指针和生命周期全部停留在该 crate 内。`media` crate
只接收拥有所有权的灰度或 RGB24 像素缓冲区。PDQ、pHash、Sobel、联系表合成和
JPEG 质量 80 编码均由 Rust 完成。

依赖清单中的 FFmpeg 包版本属于构建依赖信息，不写入 SQLite/PostgreSQL 的算法
数据，也不参与缓存键。

## 20. 错误处理与日志

- 对用户显示的错误包含任务、文件、阶段和简洁原因。
- 单文件 IO、解码或特征失败记录在 `TaskItem`，不终止批次。
- Worker 崩溃只失败当前文件并补充 Worker，不自动重试。
- TCP 断线关闭当前会话；管理工具按固定间隔重连，节点任务继续运行。
- PostgreSQL 同步失败保留已提交游标，下一次连接继续同一批。
- 删除失败保存逐文件结果，只重试用户再次确认的失败项。

日志使用 `tracing` 输出带时间、级别、组件、任务 ID 和文件路径的滚动文本日志。
`desktop` 与 `node/worker` 分目录保存，默认单文件 20 MiB、最多保留 10 个文件。
托盘菜单和设置页可直接打开对应日志目录。

## 21. 测试与验收

### 21.1 单元测试

- 路径规范化和“机器 ID + 路径 + 大小”缓存命中。
- 扫描根目录边界、局部扫描失效范围，以及失败/取消任务不失效旧路径。
- MD5 索引后按大小确认，以及多路径内容复用。
- 一筛完整性过滤、`skipped_incomplete` 和显式重试。
- PDQ 汉明、Quality、长宽比和 band 候选规则。
- 固定灰度公式、缩放、PDQ 字节序、9 分块 pHash 位序和 Sobel 零向量规则。
- 六帧抽取位置、有效帧门槛和两层视频平均分。
- Protobuf 编解码、4 字节分帧和请求 ID 分派。
- 单管理连接、8 MiB Envelope、1 MiB 分块和控制消息优先发送。
- SQLite 任务恢复、本地分析持久化、outbox ACK/清理和删除墓碑事务。
- SMBIOS 输入规范化和物理机器 ID 的确定性。

### 21.2 固定媒体测试

- 使用固定图片覆盖原图、缩放、重新压缩、轻微裁剪、水印和明显不同内容。
- 使用固定视频覆盖转码、码率变化、分辨率变化和明显不同内容。
- PDQ、pHash、Sobel 和视频平均分必须与提交的 golden 数据一致。
- 六帧联系表必须是 `3×2`、RGB24、JPG 质量 80，并使用同一组采样帧。
- FFmpeg DLL 能从 `runtime/ffmpeg` 加载并完成探测、定位和解码。

### 21.3 数据与协议集成测试

- 扫描后批量命中跳过 MD5；强制重算绕过缓存。
- 相同 MD5 与大小复用特征，不同大小不复用。
- 管理工具连接一个节点完成全部单机功能，不启动 PostgreSQL。
- 自动和手动同步均按 1000 条事务推进游标。
- 在提交前中断同步后，重新连接从最后提交游标继续。
- 中心游标早于 outbox 保留起点时执行全量快照，再继续增量。
- 分析运行只读取创建时冻结的输入位置，后续扫描数据不会混入。
- 所有选定节点完成计算且中心游标达到任务高水位前，筛选入口不可用。
- 一筛结束前不派发二筛；结束后按节点批量派发联合 pHash+Sobel。
- SQLite 或 PostgreSQL 已有完整二筛结果时不启动 Worker。
- 已派发二筛任务全部结束且二筛高水位同步完成前，不执行最终筛选。
- 本地 SQLite 分析可分页恢复候选、最终组、成员和复核标记。
- 代表文件只吸收直接通过成员，不发生 A-B-C 链式扩张。

### 21.4 Windows 端到端验收

- `node.exe` 无控制台运行，托盘状态、打开日志、重启引擎和退出可用。
- 两个 Windows x64 节点连接同一管理工具，完成跨机器精确、图片和视频去重。
- 节点离线时中心结果仍可查看，预览、打开和删除按钮正确禁用。
- 默认删除进入回收站；切换设置后永久删除；混合成功结果可见且只重试失败项。
- 删除成功后对应成员立即从本地或中心重复组移除，少于两个成员的组被删除。
- 发布目录不包含 FFmpeg EXE，DLL 依赖闭包和许可证完整。
- Worker 只从硬编码相对目录 `runtime\ffmpeg` 加载 DLL，不依赖全局 PATH。
- 空 PostgreSQL 手动执行建库 SQL 后可连接；管理工具不会自动修改中心结构。
- 应用从非当前工作目录启动时仍只读写“应用目录/data”。

未执行的 GUI、真实多机、回收站或媒体运行测试必须标记为 `PARTIAL` 或
`BLOCKED`，不能用静态检查代替动态验收。

## 22. 发布构成

发布目标固定为 `x86_64-pc-windows-msvc`。程序不根据 Windows 版本主动拒绝
运行。

```text
desktop.exe
node.exe
worker.exe
runtime/ffmpeg/*.dll
licenses/*
schema/central-v2.sql
```

首启创建 `data`。构建脚本必须检查：三个 EXE 存在、目标架构为 x64、FFmpeg
DLL 白名单及传递依赖齐全、SHA-256 与依赖清单一致、许可证存在、发布包中不存在
FFmpeg EXE，并把 `deploy/central-v2.sql` 复制为 `schema/central-v2.sql` 供用户
手动创建 PostgreSQL。

## 23. 实施边界与完成定义

旧 Go/C++ 项目只作为行为和测试样本参考。Rust 生产路径不调用旧可执行程序、旧
DLL、旧协议或旧数据库。为保护当前工作区中的未提交修改，旧文件的最终删除必须
放在 Rust 版本通过验收后的独立清理步骤中，不能为了让目录看起来“完成”而提前
扩大删除范围。

本设计完成的判据是：

1. 三个 Rust x64 程序按新工作区构建并使用统一协议协作；
2. 单节点无需 PostgreSQL 完成全部本地功能；
3. 多节点通过管理工具和 PostgreSQL 完成跨机器两层去重；
4. SQLite 自动/手动同步、离线恢复和每批 1000 条事务符合约定；
5. 所有节点计算结束并同步到固定高水位后才开始筛选，二筛结果可持久化和复用；
6. 本地 SQLite 分析、代表文件分组、分页查询和复核状态形成完整闭环；
7. 图片不生成缩略图，视频联系表符合 JPG/RGB24/质量 80 要求；
8. 回收站与永久删除均经过复核和文件身份检查，成功成员立即从组中删除；
9. 发布包仅携带所需 FFmpeg DLL，不依赖任何 FFmpeg EXE；
10. 代码结构、中文文档注释和简洁实现约束通过检查；
11. 自动测试通过，真实 GUI、多节点和 Windows 删除验收有明确运行证据。

详细实施顺序、文件级任务和验证命令在本规格获批后另行编写实施计划。
