# mySingerServer Rust V2 工程说明（瞬态计算收口）

本文是 Rust V2 当前实现的长期架构约束。实现过程中只记录已经落地的结构、方案和验证边界，不记录流水账。旧 Go/C++ 工程仅用于行为参考和测试夹具；Rust V2 不链接、启动或打包旧实现，也不承担旧协议、旧数据库或旧配置兼容。

## 1. 设计目的和范围

Rust V2 面向 Windows x64，解决单机和可信局域网内多台主机的媒体重复检测：

- `node.exe` 完成本机扫描、缓存查询、基础特征计算、SQLite 当前事实、本地分析、结果查看和逐项删除。
- `desktop.exe` 手工连接多个节点，以 PostgreSQL 汇总同步事实并编排跨机器分析。
- `worker.exe` 隔离 FFmpeg 和媒体解析；崩溃只影响当前项，不破坏 Node 的数据库和其他 Worker。
- 精确重复使用 MD5 后按文件大小判断；相似图片和视频使用固定的两层筛选。
- 实现保持直接、短小、可测试；只在进程、协议、配置、数据库和文件系统边界校验一次。

本次架构收口的主功能是计算、同步、性能优化、多机器和去重。下列内容不再作为新产品入口的持久化需求：

| 保留 | 删除或不再使用 |
|---|---|
| 当前扫描、基础计算、缓存同步、图片/视频去重 | Node 持久计算任务、`TaskCatalog`、任务恢复和阶段恢复 |
| SQLite 内容、位置、特征、当前文件事实和 outbox | SQLite 新写入的任务、分析运行、候选、复核、删除历史 |
| PostgreSQL 多机器同步、中心当前索引和跨机器分析 | Desktop 消费 Node 本地分析、自动恢复旧中心分析运行 |
| 物理盘 SSD/HDD/Unknown 权重调度和老化保护 | 以 FIFO 或“读取线程池”作为唯一调度模型 |
| 当前运行的 TSV `P/C/F` 队列、Worker 事件和遥测 | JSON、`.idx`、用户可见分页游标和持久删除计划 |
| 最近一次成功本地分析 `result.tsv` | 本地分析结果历史、复核历史和删除历史 |
| 单轮双物理盘真实验收 | 生产环境磁盘满自动清理和无边界的重复跑测 |

旧表、旧 API、旧模块和测试夹具可以继续存在以维持编译或兼容边界，但新的产品入口不得调用它们保存或恢复上述历史。不要删除旧 schema，也不要把兼容代码误当作当前事实源。

## 2. 进程拓扑

```text
desktop.exe
  ├─ 手工 IP:port ── TCP + Protobuf V5 ── node.exe（机器 A）
  │                                      ├─ SQLite：长期内容、位置、特征、outbox/current facts
  │                                      ├─ data/runtime：本次运行的瞬态 TSV
  │                                      └─ worker.exe × N（匿名管道）
  ├─ 手工 IP:port ── TCP + Protobuf V5 ── node.exe（机器 B）
  └─ PostgreSQL（中心同步、跨机器分析）
```

Node 同时只接受一个管理连接，连接内请求以 `request_id` 复用并发处理。TCP 明文且无认证，只允许可信局域网使用，不增加认证、发现服务、云服务或 TLS 兼容层。Worker 不访问 SQLite、PostgreSQL 或 TCP。三个 `apps` 目录只装配依赖和生命周期，不承载业务规则。

## 3. crate 责任

| crate | 单一职责 | 不允许承担 |
|---|---|---|
| `dedup-core` | 强类型 ID、领域模型、阈值、配置、路径值对象和纯分组规则 | IO、数据库、FFmpeg |
| `dedup-protocol` | `node.proto` 生成类型、descriptor 和领域转换 | TCP 读写、业务编排 |
| `dedup-transport` | 四字节长度分帧、请求复用、事件和优先级写队列 | 领域决策、数据库 |
| `dedup-media` | MD5、像素管线、PDQ、pHash、Sobel、视频评分、JPG 联系表 | FFmpeg FFI、文件删除 |
| `dedup-media-ffmpeg` | 固定 DLL 加载、FFmpeg FFI、媒体探测和 RGB24 解码 | 特征算法、数据库 |
| `dedup-windows` | 应用目录、SMBIOS、文件枚举、Job Object、回收站和 Shell | 业务状态机 |
| `dedup-node-store` | SQLite schema、当前内容/位置/特征、outbox 和同步事实 | 网络和媒体计算 |
| `dedup-central-store` | PostgreSQL 中心索引、同步和跨机器结果 | UI、SQLite、媒体计算 |
| `dedup-node-engine` | Node actor、瞬态扫描/计算、WorkerPool、本地分析、预览和删除 | UI、中心跨机编排 |
| `dedup-desktop-core` | 节点会话、中心同步、跨机器分析和桌面状态 | SQLite 直连、媒体解码 |
| `dedup-desktop-ui` | Slint 页面、视图模型和回调绑定 | TCP、数据库和 FFmpeg |

业务源文件用中文 `//!` 说明职责；公开类型、函数和接口用中文 `///` 说明语义和错误。业务 crate 启用 `#![warn(missing_docs)]`；`unsafe` 只留在 FFmpeg FFI 和必要的 Windows API 边界。

## 4. 领域、身份和协议约束

`dedup-core` 提供 UUID v7 运行/业务 ID、`ContentKey(md5,file_size)`、`LocationKey(machine_id,normalized_path)`、九项阈值、桌面/节点 TOML 配置和 Windows 路径值对象。`NormalizedPath` 只接受 Windows 绝对盘符或 UNC 路径，统一大小写、尾分隔符、`.`、`..` 和 `\\?\` 形式；`DisplayPath` 单独保留原始拼写供显示和文件访问。目录归属比较路径组件，不允许前缀误匹配。

生产机器身份只来自 Win32 `GetSystemFirmwareTable(RSMB)` 的 SMBIOS Type 1/2；`FixedIdentityProvider` 仅供测试，MachineId 不从配置注入。System UUID、System Serial、Baseboard Serial 依次 trim、转大写、跳过空值，以 NUL 分隔后按固定命名空间计算 SHA-256。

`proto/node.proto` 是唯一消息源，当前协议版本为 V5。Node 首帧是包含协议版本和产品标识 `mysingerserver-rust-v2` 的 Hello；节点 Envelope 覆盖状态、瞬态运行任务、扫描、分析、同步、快照、文件读取、结果窗口、删除和版本化 Node 配置。Worker V5 使用一次性 `ComputeBaseFeatures`/`BaseComputeResult` 及 `ProbeAndStage1`、`ComputeStage2`、`BuildContactSheet`，并保留 `BaseSourceReadComplete`、`Stage2SourceReadComplete`、`WorkerPhaseChanged` 和 `WorkerFailure`；Worker 不接收数据库或网络地址。

传输固定为四字节大端长度头加 Protobuf Envelope；零长度、截断和超过 8 MiB 的普通帧在 transport 边界拒绝，`FileChunk.data` 另限 1 MiB。请求 ID 非零且单调，读循环断开时一次性失败全部等待者；高低优先级写队列均有界，发送下一块前重新检查控制、进度、删除和同步 ACK。

## 5. 媒体算法和 FFmpeg

`Rgb24Image` 和 `GrayImage` 构造时一次验证紧凑缓冲区长度，内部算法不重复检查尺寸。图片特征共用整数亮度公式 `(77R + 150G + 29B + 128) >> 8` 和像素中心双线性缩放，避免一筛和二筛使用不同像素语义。

PDQ 按 `third_party/pdq/UPSTREAM.md` 锁定的 Meta commit 独立移植为纯 Rust：两轮 Jarosz 滤波和中心抽样、图像域 Quality、非 DC 16×16 DCT、Torben 中位数和 256 位阈值。上游低 word 优先布局只在 `PdqHash` 构造边界转为 32 字节规范序，SQLite、PostgreSQL、Protobuf 和汉明距离直接使用该序。

`compute_image_stage2` 从同一 `GrayImage` 生成九块 pHash 和 128 维 Sobel。pHash 使用 96×96 像素中心缩放、3×3 行优先块和包含 DC 的 8×8 DCT，数据库 BLOB 固定为九个小端 `u64`；Sobel 使用 128×128 灰度面、4×4 空间格和八个 `[0,π)` 硬方向 bin。比较函数明确规定双零向量为 1、单零向量为 0。

`screen_image_stage1` 和 `screen_image_stage2` 只消费已验证的 `Thresholds` 快照；PDQ 候选索引使用四个连续大端 `u64` band，共享任一 band 后才做完整阈值判断。视频每个槽位为 `VideoFrameFeatures { stage1, stage2 }`，固定采样位置为 `(1,3,5,7,9,11)/12` 六个中点。解码失败槽位不进入分母；一筛存在而二筛缺失时结果为 `Incomplete`，不少于四个有效完整帧后才产生视频结果。联系表复用这六个 RGB24 槽位，画布固定 3×2，缺失槽位为 `#60656F`，JPG quality 80，不参与评分。

FFmpeg 使用 BtbN 固定归档中的 8.0.1 x64 LGPL shared 构建，五个运行 DLL 的归档和 SHA-256 固定在 `third_party/ffmpeg-dependency.json`。Worker 只从 `worker.exe\..\runtime\ffmpeg` 按依赖顺序加载 `avutil-60.dll`、`swresample-6.dll`、`swscale-9.dll`、`avcodec-62.dll`、`avformat-62.dll`，不搜索当前目录或 PATH，不运行或发布 `ffmpeg.exe`、`ffprobe.exe`、`ffplay.exe`。媒体类型由实际解复用器 probe 决定，不能由扩展名直接充当类型；`image2`/`*_pipe` 的单帧伪时长不能误判为视频。

`WorkerFileSession` 为基础项只打开一次 Windows 文件句柄，先流式计算 MD5，再保留同一会话完成媒体探测、随机读取和 FFmpeg 自定义 AVIO 解码，不按路径二次打开。图片一筛只解码一次且不生成缩略图；视频一筛严格使用六个中点，二次视频优先复用本地联系表，缺失或损坏才回退原视频并重建。Worker 结果在进程边界校验 hash、尺寸、槽位和向量长度，NodeEngine 只接收拥有所有权的值。

## 6. 数据所有权和持久化边界

Node actor 串行独占一个 `NodeStore` 和一个 `WorkerPool`，所有 SQLite 更新按 actor 顺序提交。Node SQLite 使用 `PRAGMA user_version=3`、外键、WAL 和五秒 busy timeout；`metadata.schema_id` 是不兼容产品标记，只初始化空库，旧库或未知 schema 直接拒绝打开，不自动迁移。

新入口把 SQLite 限定为长期当前事实：

- `contents`、`files` 当前活动位置、文件大小、内容键、媒体类型和 `library_revision`；
- 图片/视频元数据、基础特征、二筛特征和联系表引用；
- `sync_outbox`、同步状态以及当前文件故障等必要诊断事实。

任务、分析和删除不再作为 Node SQLite 的新历史。仓库仍可能物理存在 `tasks`、`task_items`、`task_stages`、`analysis_runs`、`analysis_run_stages`、`review_marks`、`delete_batches`、`delete_items`、`deletion_tombstones` 等旧表/API；它们只作兼容 schema、旧测试或编译边界，新入口不写入、不查询来恢复任务或历史。新瞬态删除路径不生成 `deletion_tombstones`，只在 SQLite 事务中更新 `files.active=false` 和 file outbox。

Node 进程内 `RuntimeTaskRegistry` 保存当前 Task ID、Runtime ID、状态、统计、阶段、Worker、失败和路径事件；Desktop 也只在当前会话内保存同步/分析状态。速度、ETA、Worker、最近失败和遥测不写任务历史。Node 重启清空 runtime、任务列表、registry 和 partial 文件，不恢复旧任务或旧分析；最近一次成功结果文件保留供查看。

## 7. 扫描、缓存和基础计算

Node 扫描固定为：

```text
解析根目录/物理盘 → 稳定文件清单 → 批量查询缓存
    → 物理盘 TSV(P) → Worker → SQLite ACK → TSV(C/F)
    → 更新当前文件事实/outbox → 保存本次快照 → 清理 runtime
```

扫描收到根目录后，先解析每个根的物理磁盘编号、介质类型和配置额度，形成冻结 `ScanDiskPlan`，再调用枚举器。默认使用 `EverythingEnumerator`；不可用或完整枚举失败时才受控回退 `WindowsWalker`。Rust 端按规范路径排序、去重并用 `NormalizedPath::is_within` 校验组件边界。

路径缓存每批最多 1000 项，使用一次真实批量 `lookup_base_cache_by_paths`，不得循环逐文件 SELECT。MD5 得到后每批最多 1000 项，使用一次真实批量 `lookup_base_cache_by_keys`；可选 PostgreSQL 缓存也只批量查询、导入并做一次本地校准。完整命中直接形成完成数，只有真正缺少的项进入 Worker。

完整性判断拒绝空值、默认值、长度错误和失败占位符。图片一筛必须同时具备尺寸、PDQ、Quality，二筛必须具备九块 pHash 和 128 维有限 Sobel；视频必须满足六槽位、至少四个成功完整一筛帧及相应二筛覆盖。缓存阶段只查询，插入/更新只在计算结果、同步导入、文件变化或删除成功确实需要时执行。

每个物理盘 lane 的 TSV 每行一个真实待处理文件，格式为 UTF-8、LF、无 BOM，状态为 `P`（待处理）、`C`（SQLite 已确认完成）或 `F`（失败/跳过）。缓存已有完整内容的文件不得写入任务文件；TSV 只服务当前运行，不是恢复源。

Worker 终态先由 Node 完成必要 SQLite 当前事实写入，再把 TSV 行从 `P` 标为 `C`。读取失败或 Worker 崩溃标为 `F` 并记录当前文件故障，其他项继续。全部项到达终态后，Node 更新当前文件、失效本轮未出现的位置并推进 outbox/library revision；快照保存后发布完成。取消、枚举失败和任务级错误不得误失效旧位置或伪装完成。

## 8. 物理盘调度、Worker 和遥测

任务分发时已冻结物理盘 lane，不把所有路径混在单一 FIFO 中。有效额度来自配置中的 `ssd_threads_per_disk`、`hdd_threads_per_disk`、`unknown_threads_per_disk`，并受 `total_threads`、Worker 数和全局磁盘许可共同限制。全局额度不足时按权重轮转/deficit 选择；老化保护让长期等待的 HDD 或其他 lane 获得机会，避免饥饿。比例由配置决定，不能把示例 `5:1` 写死。

Hash 和 Media 在同一调度 epoch 联合判断真实 slot、refill token、output credit、盘额度和全局额度；Media refill 不能绕过仍然可派发的 Hash。任务完成、失败或取消后立即补位，不等待整批。读取调度器是文件读取许可和盘公平层，不等同于独立的计算线程池。

Worker 启动后加载固定 DLL，成功才输出 `WorkerReady`；stdin/stdout 使用四字节长度头的 WorkerEnvelope，日志写入 `data/node/logs/worker-<pid>.log`。所有 Worker 进入 `KILL_ON_JOB_CLOSE` Job Object，创建标志含 `CREATE_NO_WINDOW`；Node 退出不留下孤儿 Worker。WorkerPool 由 actor 独占，Node 不在 TCP 层维护第二份 Worker 事实。

正常完成、读取失败、取消、计划关闭和意外崩溃是独立路径。崩溃事件带 Runtime ID、任务项 ID、规范路径、阶段和错误，Node 在 registry、运行 NDJSON 和验收证据中记录；必要的单文件故障可写成 SQLite 当前诊断事实，但不创建崩溃历史。

保留 Worker Started、阶段切换、SourceReadComplete、Completed、Crashed、队列/额度、任务路径、CPU 和磁盘 IO 遥测。验收按 PID 和进程启动世代计算 CPU/IO 增量，并记录逻辑核、物理盘读写/队列/延迟及等待时间。遥测错误是旁路错误，不改变主状态机，也不能泄漏资源所有权。

## 9. 本地分析、结果和预览

本地分析只接受当前进程刚完成的扫描快照和匹配的 `library_revision`，不扫描旧任务表，不恢复旧分析运行。输入按 `(ContentKey, LocationKey)` 排序去重后批量查询 SQLite；精确重复按 ContentKey/文件大小分组，图片和视频沿用固定的一筛、二筛和代表直连分组规则。缺失二筛时只创建当前 runtime 的瞬态计算项。

分析完成后只发布固定的 `latest-analysis.result.tsv`，每行一条结果，采用临时文件校验和原子替换。`latest-analysis.partial.tsv` 只是发布中间文件，启动、失败、取消或不完整时清除；失败不得覆盖上一份成功结果。只保留最近一次成功结果，不生成 JSON、`.idx` 或历史结果。

结果查看使用 `start_index + visible_count` 的滑动窗口动态加载，新窗口整体替换旧窗口；UI 不显示 next cursor、上一页/下一页或分页按钮，不把窗口历史追加到模型。内部如需少量不透明检查点，只能服务当前会话，不能变成用户分页 API。复核标记只在当前进程结果上存在，换结果或进程退出即清空。

图片原图预览按最多 1 MiB 分块读取，不生成缩略图；视频预览只读取基础计算已经生成的 3×2 JPG 联系表，不额外抽帧。预览只接受当前活动 `LocationKey`，失活或离线位置禁用打开、预览和删除。

## 10. 删除

复核后创建当前 runtime 的 `delete.tasks.tsv`，每行一个删除位置，使用 `P/C/F` 三态并顺序逐项执行。执行前重验当前活动位置、实际大小和流式 MD5；文件系统删除或回收站操作成功后，先在 SQLite 事务中更新 `files.active=false` 和 file outbox，再把 TSV 行从 `P` 改为 `C`。身份变化、文件缺失或文件系统失败改为 `F` 并继续。

删除不保存删除批次、删除历史或恢复日志。Node 启动不扫描旧删除队列，运行结束清理本次 runtime。新删除路径不写 `deletion_tombstones`；旧 tombstone 表/API 可保留作兼容，但不应成为新删除事实源。默认使用 Windows 回收站，永久删除才调用文件系统移除；成功项从当前结果/组中移除，失败或跳过保留当前事实。

生产入口不启用磁盘满自动清理；遗留 cleaner 模块只能供兼容测试或显式隔离工具使用。

## 11. Desktop、同步和跨机器分析

Desktop 不打开 Node SQLite，不读取 Node 的 `latest-analysis.result.tsv`，也不把 Node 本地分析结果复制到中心。Desktop 的多机器能力通过 PostgreSQL 保存中心当前索引、同步游标、冻结输入、候选和跨机器分析结果；桌面自己的结果模型只消费中心查询结果。

中心同步每批最多 1000 条。节点 SQLite 更新与 outbox 序号同一事务提交；中心事务提交后才推进并 ACK 游标。提交前失败保持旧进度并重放同批；节点 outbox 已裁剪而中心落后时执行整次快照。增量只同步内容、位置、特征和文件活动状态等必要当前数据，不依赖新删除 tombstone。

连接成功、任务终态和定时追赶都进入每节点唯一的有界同步循环；长增量或快照不阻塞 UI 命令循环。传输失败的会话从索引移除，PG client 失败由后续连接重建。`NodeSession` 绑定节点返回的物理 MachineId，重连只按固定手工 endpoint 进行，不恢复远端旧任务。

跨机器分析保留当前会话的 `start`、`poll`、`retry_unresolved`；门禁要求所选当前扫描任务已完成且同步游标达到真实 outbox 高水位，再按 `(ContentKey,LocationKey)` 冻结输入、执行候选和二筛。中心已有完整缓存时跳过计算，缺失时按在线节点和当前活动位置选择来源。进程重启不恢复旧 analysis run、旧阶段或旧任务，需要时由用户重新发起。

中心 PostgreSQL 只由管理员手动执行 `deploy/central-v2.sql` 建库；应用只读校验 schema，不隐式 DDL。公开接口和同步载荷只使用 MachineId/ContentKey/LocationKey，不传播 SQLite 自增 ID；Node 的本地联系表路径不进入中心库。

## 12. Node、配置和 UI 边界

`NodeRuntime::start` 先取得生产 SMBIOS MachineId，按配置仓储解析 data/cache/log/runtime 路径，清理本次 runtime 残留和 partial，打开 SQLite，创建唯一 WorkerPool、绑定 listener，再启动 Node actor。actor 使用有界命令通道串行独占 Store 和 Pool；网络 handler、托盘回调和 Desktop 会话只持有可克隆 handle。

配置快照使用完整原文 SHA-256 和原始字段；保存经过同一进程锁、路径/字段校验和 journal 双文件事务，版本冲突不写文件。只有保存和 Node prepare 都成功才返回 `NodeRestartAccepted`；替代进程等待旧进程完全退出后按 bootstrap→配置→日志→SQLite/Worker/listener 顺序启动。配置里的 Worker 数、读取线程、SSD/HDD/Unknown 额度由 Node 真实采用并写入运行配置证据。

Desktop Slint 回调只转换为有界强类型 `UiCommand`，GUI 线程只把不可变 `UiEvent` 映射为模型，不直接访问 TCP、SQLite、PostgreSQL、FFmpeg 或配置文件。总览显示当前节点、同步和 Worker 状态；扫描/运行区域显示当前 registry 任务和阶段；结果区域使用滑动窗口。未实现的数据、日志筛选、导出和环境版本能力显示禁用原因，不填造数据。

## 13. 不可破坏的硬约束

- 只构建 `x86_64-pc-windows-msvc`，不主动按 Windows 版本号拒绝启动。
- 不添加旧实现链接、TLS、认证、自动发现、Web、移动端、云服务或自动删除。
- 固定像素语义、PDQ、pHash、Sobel、六帧采样和视频缺失规则；九项匹配阈值可配置并随当前分析输入快照使用。
- 图片不生成缩略图；视频联系表固定三列两行、RGB24、JPG quality 80，并复用六个成功抽帧。
- 视频解码失败槽位不进入分母，缺失二筛不得当作零分；缺少完整特征的缓存项不得视为命中。
- FFmpeg 只加载五个锁定 8.0.1 x64 LGPL DLL，不发布或运行任何 FFmpeg EXE。
- 每项删除前复核存在、大小和流式 MD5；只有文件系统删除和 SQLite 当前事实提交都成功才标记 `C`。
- PostgreSQL schema 只由用户手动部署；应用不隐式创建或迁移中心数据库。
- 文件、模块、公开函数、方法、类和变量保持中文职责注释；实现保持简洁，不重复校验内部强类型。

## 14. 构建、打包和验证

固定工具链为 Rust `1.97.1-x86_64-pc-windows-msvc`，根 `.cargo/config.toml` 固定目标和链接栈。常规验证命令：

```powershell
Remove-Item Env:CC -ErrorAction SilentlyContinue
Remove-Item Env:CXX -ErrorAction SilentlyContinue
cargo fmt --all -- --check
cargo clippy --workspace --all-targets --locked -- -D warnings
cargo test --workspace --locked -- --test-threads=1
cargo build --workspace --release --locked --target x86_64-pc-windows-msvc
```

`scripts/build-release.ps1` 从全新的 `dist-rust-v2/staging` 组装正式便携 ZIP；`scripts/verify-release.ps1` 对目录或 ZIP 做静态集合、PE 架构、许可证、manifest 和 sidecar 校验，不启动程序，也不证明真实 Worker、DLL、GUI、回收站或双盘运行。

正式包白名单固定为顶层 `desktop.exe`、`node.exe`、`worker.exe`、`Everything.exe`，`runtime/ffmpeg` 下五个 DLL，`schema/central-v2.sql`、许可证闭包和 `manifest/files.sha256`。不得带入 data、数据库、日志、缓存、测试客户端、旧实现产物或 `ffmpeg.exe`/`ffprobe.exe`/`ffplay.exe`。Everything 的许可证和 NOTICE 也必须非空；包内 manifest 证明解压文件，ZIP sidecar 证明归档本体。

静态包测试、测试夹具、真实 Release 运行和 GUI/托盘验收必须分别记录。构建和验证只写仓库 `dist-rust-v2` 或隔离临时目录，不触碰生产 `I:\Tool`。

真实 CPU/磁盘验收只做一轮完整媒体运行，不执行六轮 A/B，不追加 A-3。媒体根固定为 `H:\pik\00000000000` 与 `I:\tmp`，显式使用 Everything；目标配置为 Worker 20、读取线程 12，SSD/HDD 每盘额度仍从配置文件读取。采样记录两个物理盘并行读取、权重和老化保护、Worker 终态/崩溃路径、CPU、磁盘 IO、队列和最终结果 SHA。任务到达明确终态即可结束，不把等待 1800 秒作为唯一完成条件。

未实际执行的命令不得写成 PASS。验收证据至少绑定源码/包 SHA、配置快照、媒体前后清单、当前任务终态、Worker 事件、运行 NDJSON、系统采样和报告路径。

## 15. 开发规则

- 只在指定 worktree 修改文件，保留其他代理的未提交改动；不执行 broad clean、reset 或覆盖用户数据。
- 代码变更先补行为测试，再做最小实现；文档、测试报告和注释使用中文。
- 不新增 JSON、`.idx`、用户可见分页或持久任务恢复；需求冲突时回到本收口边界。
- 定向验证后运行 `cargo fmt --all -- --check` 和 `git diff --check`；重型测试使用隔离 target 并记录退出码和证据路径。
- 生产部署、包替换、删除目录或清理非临时数据必须由用户明确授权；文档更新本身不执行部署。
