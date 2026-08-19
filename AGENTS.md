# mySingerServer Rust V2 工程说明

本文件是 Rust V2 的长期架构约束。实现过程中只更新已经落地的结构、方案和验证命令，
不记录逐步操作流水账。旧 Go/C++ 工程保留在仓库中仅用于行为与测试夹具参考；Rust V2
不链接、不启动、不打包任何旧实现产物，也不承担旧协议、数据库或配置兼容。

## 1. 设计目的

Rust V2 面向 Windows x64，解决单机和可信局域网内多台主机的媒体重复检测：

- `node.exe` 在单机上完成扫描、特征计算、本地 SQLite 分析、结果浏览和删除。
- `desktop.exe` 手工连接多个节点，以 PostgreSQL 汇总索引并编排跨机器分析。
- `worker.exe` 隔离 FFmpeg 和媒体解析；崩溃只影响当前任务项，不破坏节点数据库。
- 精确重复固定使用 MD5 后按文件大小判断；相似图片和视频固定采用两层筛选。
- 代码优先直接、短小、可测试；只在进程、协议、配置、数据库和文件系统边界校验一次，
  不为未确认需求增加兼容层、认证、发现服务或过度防御代码。

## 2. 进程拓扑

```text
desktop.exe
  ├─ 手工 IP:port ── TCP + Protobuf ── node.exe (机器 A)
  │                                      ├─ SQLite: data/node/node.db
  │                                      └─ 匿名管道 ── worker.exe × N
  ├─ 手工 IP:port ── TCP + Protobuf ── node.exe (机器 B)
  │                                      ├─ SQLite: data/node/node.db
  │                                      └─ 匿名管道 ── worker.exe × N
  └─ NoTls ── PostgreSQL（中心索引、同步游标、跨机器分析和复核）
```

节点同时只接受一个管理连接，但该连接可以复用并发请求。TCP 明文且无认证，只允许暴露在
可信局域网。Worker 由节点创建并放入 `KILL_ON_JOB_CLOSE` Job Object；Worker 不访问
SQLite、PostgreSQL 或 TCP。三个 `apps` 目录只装配依赖和生命周期，不承载业务逻辑。

## 3. crate 责任表

| crate | 单一职责 | 不允许承担的职责 |
|---|---|---|
| `dedup-core` | 强类型 ID、领域模型、阈值、配置、路径值对象和纯分组规则 | IO、数据库、FFmpeg |
| `dedup-protocol` | `node.proto` 生成类型与领域转换 | TCP 读写、业务编排 |
| `dedup-transport` | 4 字节长度分帧、请求复用、事件和优先级写队列 | 领域决策、数据库 |
| `dedup-media` | MD5、像素管线、PDQ、9 分块 pHash、128 维 Sobel、视频评分、JPG 联系表 | FFmpeg FFI、文件删除 |
| `dedup-media-ffmpeg` | 固定 DLL 加载、FFmpeg FFI、媒体探测与 RGB24 解码 | 特征算法、数据库 |
| `dedup-windows` | 应用目录、SMBIOS、文件枚举、Job Object、回收站与 Shell | 业务状态机 |
| `dedup-node-store` | 当前 V2 SQLite schema、事务、任务、分析、结果和 outbox | 网络和媒体计算 |
| `dedup-node-engine` | 扫描、Worker 池、本地分析、预览、删除与节点 actor | 直接 PostgreSQL 访问 |
| `dedup-desktop-core` | 节点会话、中心同步、PostgreSQL 访问、跨机器分析和 UI 状态 | SQLite 直连、媒体解码 |
| `dedup-desktop-ui` | Slint 页面、视图模型和回调绑定 | TCP、SQLite、PostgreSQL、FFmpeg |

所有业务源文件用中文 `//!` 说明职责；公开类型、函数和接口用中文 `///` 说明语义与错误。
业务 crate 启用 `#![warn(missing_docs)]`。`unsafe` 只留在 FFmpeg FFI 和必要 Windows API
边界，安全接口之外不得传播裸指针或原生句柄。

当前领域边界已经落地：`dedup-core` 提供 UUID v7 业务 ID、内容/位置键、九阈值验证、
桌面/节点 TOML 配置和 Windows 路径值对象；`dedup-windows` 提供从可执行文件绝对路径
推导的 `AppLayout`，以及 Raw SMBIOS Type 1/2 读取和机器 ID 计算。生产机器身份只来自
Win32 `GetSystemFirmwareTable(RSMB)`，不从配置注入。

协议边界也已落地：`proto/node.proto` 是唯一消息源，`dedup-protocol` 用固定 vendored
`protoc` 在 `OUT_DIR` 生成 Rust 类型和 descriptor set，并显式转换 `ContentKey`、
`LocationKey` 与 `Thresholds`。节点 Envelope 覆盖状态、任务、路径、分析、同步、快照、
文件读取和删除；WorkerEnvelope 只携带任务/项目/显示路径、槽位和计算结果，不含数据库或网络地址。

媒体像素边界已经落地：`Rgb24Image` 和 `GrayImage` 在构造时一次性验证紧凑缓冲区长度，
内部算法不重复检查尺寸。所有图片特征共用整数亮度公式和像素中心双线性缩放，避免一筛、
二筛使用不同像素语义。PDQ 按 `third_party/pdq/UPSTREAM.md` 固定的 Meta commit 独立移植为
纯 Rust；模块依次负责两轮 Jarosz 滤波与中心抽样、图像域 Quality、非 DC 16×16 DCT、
Torben 中位数和 256 位阈值。上游低 word 优先布局只在 `PdqHash` 构造边界转为 32 字节
规范序，SQLite、PostgreSQL、Protobuf 和汉明距离都直接使用该字节序，不再二次解释。

图片二筛也保持一个清晰入口：`compute_image_stage2` 从同一 `GrayImage` 生成九块 pHash 和
Sobel，调用者不能把两种特征拆成不同解码任务。pHash 先用共享的像素中心缩放到 96×96，
再按 3×3 行优先计算包含 DC 的 8×8 DCT；bit 使用系数行优先序，数据库 BLOB 固定为九个
小端 `u64`。Sobel 用同一缩放语义得到 128×128 灰度面，以 4×4 空间格和八个 `[0,π)`
硬方向 bin 形成 128 维 L2 向量。比较函数显式定义双零向量为 1、单零向量为 0。

`screen_image_stage1` 和 `screen_image_stage2` 只消费已验证的 `Thresholds` 快照：一筛返回
PDQ 汉明距离及 `1-hamming/256`，联合二筛返回 pHash 通过块数及 Sobel 余弦。候选索引把
PDQ 规范字节切成四个连续大端 `u64`，共享任一 band 才做完整阈值判断；这是已确认的近似
召回边界，不增加错位 band 或向量索引。`dedup-core::MediaKind` 是数据库、协议和任务共享的
图片/视频领域枚举，不由文件扩展名直接充当类型。

视频比较把每个槽位建模为 `VideoFrameFeatures { stage1, stage2 }`。`stage1=None` 明确表示
该端解码失败，双方任一失败的槽位不进入分母；双方一筛存在但任一 `stage2=None` 则表示
二筛数据尚未完整，整次结果保持 `ScreeningOutcome::Incomplete`。一筛有效但图片阈值失败
的帧计零；二筛 pHash 未达到通过块数的帧也计零，pHash 通过时帧分数取 Sobel 余弦。
有效帧达到四帧后才按冻结的视频平均阈值产生 Passed/Rejected，避免把缺失数据伪装为低分。

`sample_positions` 只产生 `(1,3,5,7,9,11)/12` 六个固定中点。联系表直接消费同一六槽位，
不触发额外解码；画布固定 3×2、RGB24，图片保持长宽比居中，缺失槽位填 `#60656F`，最后
由 Rust `image` 以 JPG quality 80 编码。联系表只是节点本地预览缓存，不参与任何评分。

FFmpeg 边界使用 BtbN 固定归档中的 8.0.1 x64 LGPL shared 构建，归档和五个 DLL 都在
`third_party/ffmpeg-dependency.json` 固定 SHA-256。`fetch-ffmpeg.ps1` 只发布依赖闭包中的
`avutil`、`swresample`、`swscale`、`avcodec`、`avformat` 与许可证；bindgen 产物已经提交，
普通构建不需要 LLVM 或头文件。加载器先固定默认搜索目录，再把
`worker.exe\..\runtime\ffmpeg` 加入白名单并以绝对路径按依赖顺序加载，因此当前目录和 PATH
不能替换 DLL。动态函数表与五个模块句柄由同一个 `Ffmpeg` 值持有，裸 format、codec、packet、
frame 和 sws context 只存在于短生命周期解码会话，安全边界外只返回媒体信息或紧凑 RGB24。
探测媒体类型读取实际解复用器；`image2`/`*_pipe` 的单帧伪时长不被误判为视频。

Worker 媒体流水线通过 `MediaDecoder` 隔离 FFmpeg：图片一筛只解码一次且绝不生成缩略图；
视频一筛严格按六个中点解码，单次灰度面计算 PDQ/Quality，并把同一批 RGB24 直接交给联系表，
不做第七次解码。联合二筛的每个请求槽位也只解码、转灰度一次，再共同生成九块 pHash 与
128 维 Sobel。槽位失败作为结果保存，不让一个坏帧终止其他视频帧。Worker 的内部结果使用
独立 Protobuf 载荷，进程边界一次校验固定 hash/向量长度，NodeEngine 只接收拥有所有权的值。

`worker.exe` 启动后先加载相对 DLL，成功才向 stdout 写 `WorkerReady`；stdin/stdout 永远只传
四字节长度头的 `WorkerEnvelope`，日志直接写 `data/node/logs/worker-<pid>.log`。`WorkerPool`
由单 actor 独占队列、进程槽位和 Job Object，空闲槽位与等待项配对后才更新运行快照。正常
结果、意外退出、用户取消和计划重启是四条独立路径：意外退出增加 failure count、当前项失败
并补建；取消删除等待项并终止/替换命中的 Worker，但不增加 failure count。计划重启必须先
`prepare_planned_restart` 冻结调度并返回运行项，NodeEngine 在 SQLite 将这些项改回 queued 后，
才能调用 `restart_after_requeue`；第二阶段会核对同一项集合，再终止、补建并等待全部 Ready。
Worker 全部加入 `KILL_ON_JOB_CLOSE` Job，节点退出不会留下孤儿进程，创建标志固定包含
`CREATE_NO_WINDOW`。

## 4. 数据所有权

- 节点 actor 串行独占一个 `NodeStore` 和一个 `WorkerPool`，所有 SQLite 写入经 actor 排序。
- 节点 SQLite 是本地扫描、缓存、任务、特征、本地分析、复核、删除和 outbox 的唯一事实源。
- PostgreSQL 由 `desktop.exe` 独占访问，只存中心索引、外部键、同步游标和跨机器结果；
  `desktop.exe` 不打开节点 SQLite，节点也不连接 PostgreSQL。
- 跨边界键固定为 `MachineId`、`ContentKey(md5,file_size)` 和
  `LocationKey(machine_id,normalized_path)`；扫描缓存查询再把 `file_size` 作为独立条件，
  因此跳过 MD5 的完整条件仍是机器 ID、规范路径和文件大小。SQLite 自增 ID 不通过网络或同步传播。
- 配置、SQLite、日志和缓存只写可执行程序同目录下的 `data`。当前工作目录和用户目录不作
  运行时回退。
- `NormalizedPath` 只接受 Windows 绝对盘符或 UNC 路径，统一大小写、尾分隔符、`.`、`..`
  和 `\\?\` 形式；目录归属比较路径组件。`DisplayPath` 单独保留原始拼写供 UI 与文件访问。
- `MachineId` 的输入顺序固定为 System UUID、System Serial、Baseboard Serial；字段 trim、
  转大写、跳过空值后以 NUL 分隔，对固定命名空间前缀与字段计算 SHA-256。

节点 SQLite V2 使用 23 张严格表闭合内容、位置、特征、任务、分析、分组、复核、同步和删除。
`metadata.schema_id` 是不兼容版本标记：只自动初始化空数据库，旧库或未知 schema 直接拒绝打开。
`NodeStore` 独占连接并启用外键、WAL 与五秒 busy timeout；上层 actor 负责串行调用，不在每个
仓储函数内重复加锁。内容先按 `(md5,file_size)` 复用，再在同一事务写位置和 outbox。

特征完整性由存储边界统一定义：图片一筛必须同时具备尺寸、PDQ 和 Quality，二筛必须同时具备
九分块 pHash 与 128 维有限 Sobel；视频必须有六个槽位、至少四个成功且完整的一筛帧，二筛还须
覆盖每个成功帧。部分结果允许保存以支持恢复，但查询只向分析层返回完整结果。

任务与任务项分别持久化状态。领取只把一个稳定排序的 `queued` 项改为 `running`；结果、统计、
任务终态和任务内单调 `event_seq` 在同一事务提交后才能发事件。进程恢复只把遗留 `running`
项和所属任务退回 `queued`，成功、失败、取消项不重算；单文件失败计入统计，但其他项结束后
任务仍为 `completed`，只有任务级基础设施错误才使用 `failed`。

扫描实现由 `dedup-node-engine::scan` 统一编排。`FileEnumerator` 只有两个生产实现：
`WindowsWalker` 直接递归 Windows 文件系统，`EverythingEnumerator` 通过
`everything-ipc 0.1.4` 对每个根查询并在 Rust 端再次使用 `NormalizedPath::is_within`
确认组件边界。两者都返回全部普通文件的 `ScannedPath`，按规范路径排序去重；Everything
不可用或查询失败时当前任务直接返回一条明确错误，不在任务中切换 Walker。文件扩展名不写入
领域类型；Worker 以 FFmpeg 实际 probe 结果决定 `MediaKind`。

`ScanEngine::run` 在枚举前用 `create_scan_task` 原子保存任务和扫描根。完整列表按 1000 条调用
`lookup_scanned_paths`：机器、路径、大小命中直接完成 `reused` 项且不打开文件；未命中由
`SystemMd5` 用 1 MiB 缓冲流式读取，再以 MD5 索引和大小确认 `ContentKey`。已有完整特征只新增
位置引用；已有媒体内容缺少完整一筛时保存 `skipped_incomplete` 成功项，不自动启动 Worker；
新内容和用户明确的强制重算才通过 `Stage1Processor` 派发。生产适配器串行借用 `WorkerPool`，
Worker 只返回实际探测类型和拥有所有权的特征，NodeStore 负责写类型、图片/视频一筛、六槽记录、
联系表引用和 outbox。单文件读取或计算失败只增加任务失败项，扫描仍可完成。

真实扫描任务的最后一个任务项不会提前把任务标为 completed。只有全部项终态后，
`finalize_scan_task` 才在一个事务内读取已持久化扫描根，按路径组件失效本轮未出现的活动位置、
写 file outbox、推进任务事件并完成任务；返回值是该事务提交后的 outbox 高水位。枚举失败、
任务级失败或取消绝不调用收尾事务，因此不会误失效旧位置；扫描 `D:\A` 也不会影响 `D:\AB`。

本地分析由 `LocalAnalysisEngine` 完全在节点 SQLite 上执行。开始前先查询所有任务：任何
`queued/running` 都返回 `ComputationRunning`，所选 `failed/cancelled` 返回需重试或重选；
`completed` 任务允许包含文件级失败。输入只由这些任务的成功项连接当前活动位置并一次冻结，
阈值以 TOML 快照保存，随后按 `stage1_synced → screening` 推进。精确组直接按 ContentKey
聚合冻结位置，至少两个位置成组，不计算额外哈希。

相似一筛只装载 Store 定义的完整特征。缺字段的图片、缺六槽记录或有效帧不足的视频在线性地
增加 `analysis_runs.skipped_incomplete`，不进入候选。图片索引键是带位置的四个 PDQ band；
视频索引键是“槽位、band 位置、band 值”，任一对齐槽共享后才执行完整六帧平均。一筛通过的
全部 Candidate 在单事务替换并提交后，状态才进入 `phase2_dispatched`，从而保证不会边筛边派发。

二筛从候选提取并排序去重 ContentKey，先查 SQLite 的联合结果；完整结果零派发，缺失内容只选
第一个活动位置并创建一个持久 `analysis_stage2` 任务项。图片 Worker 结果必须同时保存九块 pHash
和 128 维 Sobel；视频只派发一筛成功槽位，每帧联合结果分别写表和 outbox。所有任务项终态后
才读取数据库做最终判定；任何候选缺结果都保存为 `Incomplete`，运行进入 `partial`，不把缺失
当零分。`retry_phase2` 只从未解决候选重新收集仍缺失的 ContentKey，并复用同一状态链。

最终相似分组调用 `dedup-core::group_by_representative`，本地与后续中心分析共用同一纯函数。
函数按 ContentKey 升序取尚未分组的代表，只加入与代表具有最终通过边的内容并立即占用，绝不
沿成员继续做连通分量扩张。NodeEngine 再把每个内容展开为冻结位置；代表内容按
`(machine_id,normalized_path)` 选最小位置，成员保存相对代表的一筛、pHash 通过块数和联合分数。
候选最终状态与精确/图片/视频组分别用整批事务替换，结果可在关闭并重开 SQLite 后通过分页 API
和复核 API 完整恢复，管理端不需要也不得直接读取节点数据库。

节点组合由 `dedup-node-engine::actor`、`server`、`preview` 和 `delete` 四个边界完成。
`NodeRuntime::start` 先经 `IdentityProvider` 取得物理 MachineId；生产入口只能使用
`SmbiosIdentityProvider`，`FixedIdentityProvider` 只供测试，MachineId 不进入配置文件。运行时随后
打开 `data/node/node.db`、恢复遗留 running 项、创建唯一 WorkerPool、绑定 TCP listener，再启动
唯一 NodeEngine actor。actor 通过容量 64 的命令通道串行独占 `NodeStore` 与 `WorkerPool`；网络
handler、托盘回调和后续桌面会话都只持有可克隆 `NodeEngineHandle`，不能直接访问 SQLite。

节点 TCP 首帧必须是协议版本 2、产品标识 `mysingerserver-rust-v2` 的 Hello。一个节点同时只接受
一个管理连接，第二连接立即收到 `NodeBusy`；已取得名额的连接使用 request_id 并发处理多个请求，
独立写任务串行输出响应。连接断开释放名额，服务关闭会停止 listener 并终止连接任务。actor 已
统一接入状态、任务查询/取消、路径浏览、扫描、本地分析、结果分页、复核、分析输入、跨机器批量
二筛、同步、快照、预览和删除，不得另建旁路协议。`PrepareAnalysisInput.scan_task_ids` 直接表达
中心运行选择的扫描任务；节点只从这些已完成任务的 succeeded 项连接当前活动位置，不创建伪本地
分析运行。`DispatchStage2` 先把整批唯一内容保存为 `analysis_stage2` 任务：SQLite 已有完整二筛时
只重新发布特征 outbox，不启动 Worker；本机也缺失时才调用唯一 WorkerPool 计算并保存。

`tasks.outbox_high_seq` 是任务终态的一部分：普通计算任务在最后一项完成事务中、扫描任务在路径
失效和完成事务中，保存当时 SQLite outbox 的真实高水位。管理端只能用这个持久值等待 PostgreSQL
游标，不能把查询时全库最高序号冒充某任务高水位。节点状态中的 queued/running 项数也直接来自
SQLite，Worker 忙碌数来自唯一 WorkerPool，不由 TCP 层维护第二份事实。

预览只接受当前活动 `LocationKey`。图片 `original` 直接 seek 并读取原文件，每块最多 1 MiB，
不创建目录或缩略图；视频 `contact_sheet` 只读取一筛已经写入 `data/node/cache/contact-sheets`
的 JPG。删除执行顺序固定为活动位置→实际大小→1 MiB 缓冲流式 MD5→文件系统操作；默认回收站
在短生命周期 STA 线程调用 Windows `IFileOperation`，同时设置 `FOF_ALLOWUNDO` 和
`FOFX_RECYCLEONDELETE`：前者保留旧系统语义，后者在 Windows 8+ 明确要求进入回收站；永久删除
才调用 `std::fs::remove_file`。所有结果整批交回 NodeStore；只有 recycled/deleted 写墓碑并立即缩组。

`dedup-core::logging::SizeRotatingWriter` 是三个进程共用的同步日志边界，生产固定 20 MiB、包含
当前文件在内保留 10 个文件。`node.exe` 首次启动只写 `data/node/config.toml`，Release 使用
Windows 子系统且不创建控制台；Slint `SystemTrayIcon` 复用旧托盘图标的独立副本，菜单只包含
运行状态、监听地址、打开日志目录、重启计算引擎和退出节点。重启严格执行 WorkerPool prepare、
SQLite 单事务 requeue、Worker terminate/recreate/Ready；退出先停 listener，再关闭 actor 并释放
Job Object。托盘回调只映射 `TrayCommand`，重复 Exit 不会发送第二次关闭。

## 5. 同步与分析状态机

扫描先生成文件列表，再以“机器 ID + 规范路径 + 文件大小”批量查询 SQLite。命中位置缓存
时跳过 MD5；否则计算 MD5 后以 `ContentKey` 查找已有元数据和特征，已有数据不重复计算。
媒体数据不完整时记为 `skipped_incomplete`，不进入一筛。

本地分析完全在 SQLite 内执行。每次分析冻结输入和阈值；只有相关计算任务全部结束后才能
开始筛选。精确重复为 MD5 索引后比较文件大小；图片一筛为 PDQ/Quality，二筛联合 9 分块
pHash 与 128 维 Sobel；视频均匀抽六帧，每帧走同一图片判定并取平均。分组以最小稳定键
作为代表，只加入与代表直接通过的成员，不做传递闭包。

分析状态链固定为 `collecting_stage1 → stage1_synced → screening → phase2_dispatched →
phase2_synced → finalizing → completed`；活动态可到 `partial/cancelled`，只有显式重试允许
`partial → phase2_dispatched`。输入只从所选 `completed` 任务的成功项连接当前活动文件，按
`(ContentKey,LocationKey)` 去重排序后一次封存。候选和最终组都是整批事务替换。

组分页游标编码 `(group_kind,representative_content_key,group_id)`，成员游标编码
`(machine_id,normalized_path)`，因此成员删除后可从旧位置继续且不重复。复核决定 UPSERT 到
SQLite；创建删除批次时才一次性验证每组至少一个当前活动 `Keep` 并冻结 Delete 项的 MD5/大小。
`recycled/deleted` 结果在一个事务中写结果、位置墓碑和 outbox，并移除组成员；少于两项即删组。
若删除代表，只从该计划中剩余的明确 Keep 按位置升序选择新代表，不重新筛选或扩组；
`failed/skipped` 保留位置和组，相同成功结果重复提交幂等。

中心同步每批最多 1000 条：先以 PostgreSQL 已提交 cursor 向节点 ACK，再拉取增量；中心
事务提交后才 ACK 新 cursor。节点 outbox 被裁剪而中心落后时执行整次快照。自动同步只由
连接成功、任务完成和每 5 秒追赶检查触发；手动同步进入同一队列，每节点最多一个同步循环。
`DesktopApp` 控制循环分别维护“配置的固定重连间隔”和“固定 5 秒追赶”两个 interval。重连 tick
先剔除已经失败的会话，再只连接缺失的手工端点；连接成功、首次观察到任务完成和追赶 tick 只向
节点自己的 `SyncTrigger` 通道排队。每个后台同步循环独占一个 `SyncEngine` 和一条 PG client，多个
节点并行，长增量或快照不阻塞唯一 UI 命令循环。设置更新只重建重连 interval 和同步循环，不创建
第二份 UI 状态。传输失败的会话必须从索引移除；PG client 失败则丢弃并在后续触发重新连接。

`NodeSession::connect` 通过 `ClientConnection` 把 Hello 作为 TCP 首帧发送，严格校验协议版本与
产品标识后查询 NodeStatus，并以节点返回的物理 MachineId 绑定该会话。任一传输错误结束当前
请求；`connect_with_retry` 只按配置的固定间隔重连同一个手工 IP:port，不重建或重试远端任务。
`ClientConnection` 最后一个所有者 Drop 时同步关闭发送队列，后台写端随即释放 TCP，使节点的
单管理连接名额可以被下一会话取得。每个节点使用一个有界 `SyncTrigger` 通道：连接成功、任务
完成、五秒 tick 只能产生 Automatic，用户操作产生 Manual；二者都进入同一个 `SyncEngine` 锁。

`SyncEngine::sync_node` 每轮先从 PG 读取该机器 cursor 并 ACK，即使上次发生“PG 已提交、ACK
发送前断线”也能先裁剪节点旧行。增量固定请求 1000 条，只有 `CentralStore::apply_sync_batch`
事务提交成功后才更新内存进度并 ACK；提交前失败保持旧 cursor，重连重放同一批。同步进度只
发布 Acknowledging、Incremental、Snapshot、CaughtUp 四个不可变阶段快照，UI 不自行推导游标。

SQLite 的业务更新与 outbox 递增序号同事务提交。ACK 只前进、不越过节点真实高水位，并裁剪
已确认行；请求游标落后于裁剪边界时返回 `SnapshotRequired`。整库快照固定一个读事务和起始
高水位，按稳定主键逐表分页；快照载荷和增量载荷只传播跨边界键，不泄露本地自增 `content_id`。

网络快照由节点 actor 保存 `OwnedSnapshot`：它用数据库文件的新只读连接执行一次 Deferred
事务，先读 outbox 高水位建立固定视图，再跨多个 ReadSnapshotPage 请求保留该事务。借用型
`Snapshot` 只服务节点内部同步测试，绝不以不安全方式跨线程。固定表序为 contents、files、图片
两层、视频元数据/两层六帧、deletion_tombstones；contact_sheets 不开放为中心快照表。最后墓碑
页完成或管理连接关闭时 token 和只读连接立即释放，断线不保留页游标。

中心收到 SnapshotRequired 后开启一个 `CentralSnapshot` PostgreSQL 事务，先把该机器旧位置设为
非活动并清空旧墓碑，再按上述表序写每页外部键载荷；全部页面完成才原子推进中心 cursor 并提交。
页面读取或连接中断时 Drop 自动回滚，下一连接重新 BeginSnapshot。节点成功删除在同一 SQLite
事务更新 delete item、位置、重复组、`deletion_tombstones` 与 file/tombstone 两条 outbox；中心
墓碑以机器/路径 UPSERT，因此即时删除结果更新组后再收到同一墓碑仍然幂等。

跨机器分析由 `dedup-desktop-core::analysis::CrossAnalysisCoordinator` 负责，公开入口固定为
`start`、`poll` 和 `retry_unresolved`。`start` 保存节点、扫描任务、创建时状态以及当时已知的两个
高水位；`poll` 每次都重新查询节点任务真实 `outbox_high_seq`，调用同一 `SyncEngine` 追赶中心，
再使用纯函数门禁判断，不让 UI 或网络层自行推断阶段。stage1 只有全部所选任务 `completed` 且
各机器 PG cursor 达到各任务 highwater 才继续；queued/running/failed/cancelled 或游标落后都保持
`collecting_stage1`，不生成部分候选。

门禁通过后，协调器按节点把同机所选任务合并分页读取，以 `(ContentKey,LocationKey)` 排序去重，
在 PostgreSQL 单事务插入 `analysis_run_inputs` 并封存。后续自动同步的新位置不进入既有运行；重试
选择二筛来源时也只允许冻结位置。中心特征查询始终从冻结输入反向连接 `contents` 和特征表：图片
缺任一宽高、PDQ、Quality 就跳过；视频必须有六槽记录、至少四个成功完整帧。图片按带位置四 band
建索引，视频按“槽位、band 位置、band 值”做并集，再调用与本地分析相同的完整评分函数。

一筛的完整 Candidate 集合必须先由 `replace_candidates` 提交，之后才能规划二筛。规划先跳过 PG
已有完整 stage2 的内容；PG 缺失时优先选择报告本机 SQLite 已缓存结果的在线节点，否则按
`(MachineId,NormalizedPath)` 选择在线活动位置。内容按节点归并，每个协议批次最多 1000 项，图片
不带槽位，视频只带一筛成功槽位。同机缓存命中仍派发一个持久任务来重新发布 outbox，但不会解码；
返回的任务 ID 追加到 `analysis_run_nodes`，其真实完成 highwater 构成新的 phase2 门禁。

phase2 对 queued/running 或游标落后继续等待；completed/failed/cancelled 都视为终态，随后只以 PG
特征完整性决定结果。任一候选缺一端完整联合特征即保存 `Incomplete` 并进入 `partial`，绝不以零分
拒绝。`retry_unresolved` 先重新读取 PG，已同步完整者直接复用，只对仍为 Incomplete 的唯一内容
重新选择节点和派发。全部完整后用共享 `dedup-core::group_by_representative` 生成代表直连组，精确
组按同一 ContentKey 的冻结位置生成；候选、直接证据分数、最终组和成员均由 PostgreSQL 事务保存。

中心 PostgreSQL 只接受管理员在空库手动执行 `deploy/central-v2.sql`。脚本使用
`schema_metadata.schema_id=mysingerserver-rust-v2` 标记全新且不兼容的产品 schema，建立节点、
同步游标、全局内容、机器位置、图片/视频两层特征、删除墓碑、分析输入、候选、分组、复核和
删除批次共 20 张表；故意不使用 `IF NOT EXISTS`，重复执行会失败，不承担迁移职责。
`CentralStore::connect` 只读验证产品标记和所需表列，缺失时只禁用中心模式，绝不创建或修改表。

中心内部 `contents.content_id` 只用于 PostgreSQL 连接；所有公开接口和节点同步载荷都使用
`ContentKey`/`LocationKey`。`apply_sync_batch` 先解码完整批次，在一个事务内先写内容、再写位置与
特征，最后按机器推进单一 cursor；来自节点的本地联系表路径不进中心库。宽高按 PostgreSQL
`INTEGER` 写入，文件大小、时间和序号按 `BIGINT` 写入，避免依赖数据库隐式类型转换。
同一 `(md5,file_size)` 在任意机器只映射一个中心内容，而每个机器路径各自保留位置记录。

桌面端采用严格单向交互。Slint 回调只把手工节点编辑、连接、刷新、同步、扫描、取消、路径浏览
和设置保存转换为有界 `UiCommand`；`DesktopApp` 的 Tokio 控制循环持有 `NodeSession`、中心查询
`CentralStore` 和唯一视图，每个节点后台同步循环另持有自己的 `SyncEngine` 与 PG client，结果再
作为不可变内部事件归并并发布 `UiEvent::ViewChanged`。GUI 线程只把事件映射为 Slint model 和属性，
不直接读写 TCP、SQLite、PostgreSQL、FFmpeg 或配置文件。节点连接按
手工配置顺序展示、并行建立，每个列表索引最多保存一个 session；编辑节点会丢弃旧会话，避免把
新地址错误绑定到旧物理 MachineId。PostgreSQL 未配置、schema 缺失或连接失败只改变中心能力和
诊断文案，节点扫描及本地 SQLite 分析仍然可用。

`desktop.exe` 首次启动只创建 `data/desktop/config.toml`、`data/desktop/cache` 和
`data/desktop/logs`，并写入默认节点 `127.0.0.1:39091`。日志复用 20 MiB × 10 滚动 writer；
应用路径全部从 executable 绝对路径推导，设置页显示实际值。界面使用 Slint fluent-dark 与项目
深色中性主题，固定八个导航入口：总览与节点、扫描任务、精确重复、相似图片、相似视频、跨机器、
删除复核、设置诊断。总览聚合在线数、任务、同步游标和 Worker 状态；扫描页显示持久任务逐项进度、
失败与不完整跳过计数，并在任何节点有 queued/running 项时统一禁用筛选。设置页一次校验 PostgreSQL
NoTls 连接、九项阈值、回收站/永久删除与重连间隔，并嵌入 `AboutSlint` 归属入口及可信局域网明文
警告。

结果交互层以 `dedup-desktop-core::results` 为唯一界面模型，不把 SQLite/Protobuf/PostgreSQL
记录直接暴露给 Slint。`ResultScope` 明确区分单节点运行和中心运行，二者都转换为 `GroupPage`、
`MemberPage`、`GroupView` 与 `MemberView`；每行携带稳定外部内容键/位置键、代表标志、两层分数、
媒体元数据、持久复核标记及一次派生的在线动作能力。组和成员分别沿用服务端复合游标；
`PagedWindow` 只保留固定数量的最近行，避免浏览大量结果时在管理端一次物化整库。Slint 页面
仅保存当前页模型和不透明 next cursor，不使用 offset，也不自行重算相似分数或删除资格。

八个导航入口现已形成闭环：扫描页可从已完成扫描任务创建本地精确/图片/视频分析；精确页在
节点 SQLite 与中心 PostgreSQL 之间切换；相似图片和相似视频页共用同一分页组件，但按组类别
请求正确分数和预览；跨机器页只触发 `start/poll/retry` 三个协调器入口并展示当前持久门禁；
删除复核页只操作当前已加载组。所有 Slint callback 仍只投递强类型 `UiCommand`，控制循环串行
持有当前 `LoadedMembersContext` 和 `PreparedDeleteContext`，任何换组或复核变化都会使旧确认失效，
因此 UI 不可能把旧组确认用于新组。完整 `UiEvent` 在 GUI 线程整体替换模型，避免控件各自维护
网络请求和可变业务状态。

图片预览在用户点击后由节点按 1 MiB 分块读取原文件，在 desktop 内存中解码并直接交给 Slint，
不写缓存、不生成缩略图；视频预览只读取计算阶段已经生成的六帧 3×2 JPG 联系表。离线或失活
成员仍显示路径和评分，但 `MemberActions` 会同时禁用预览、打开和删除。复核板从 SQLite/PG 已有
标记恢复；单项 Keep/Delete 与最大文件、最高分辨率、最高 Quality、路径包含四个快捷规则只
持久化标记，绝不隐式生成或执行删除任务。

删除分成“准备确认”和“执行”两个命令。确认摘要固定显示文件数、节点数、预计空间和当前
RecycleBin/Permanent 模式；任一组缺活动 Keep、没有显式 Delete 或目标节点离线都会禁用执行，
Permanent 只增加醒目且明确的不可恢复警示。单机删除使用节点已有分析组创建批次；中心删除先在
PostgreSQL 冻结外部键计划，再按物理 MachineId 批量派发给节点。节点可在没有对应本地分析运行时
执行中心计划，但仍逐项重验大小和 MD5；成功项在本机立即失活并发布 file/tombstone outbox，管理端
收到响应后立即调用 PG `apply_delete_results` 缩组。后续同步到达相同墓碑保持幂等；failed/skipped
保留在结果中，只有用户重新复核并再次确认才会重试。

`create_analysis_run` 把九项阈值 TOML、所选机器任务与两个高水位一次保存；
`insert_analysis_inputs` 只允许执行一次，事务提交时将运行标记为 frozen，数据库触发器拒绝原地
更新输入。候选键必须严格左右升序；候选与最终组都整批事务替换。中心组和成员分页沿用节点端的
稳定复合游标，复核使用外部位置键 UPSERT。中心删除计划只冻结已明确 Delete 且所在组至少有一个
活动 Keep 的成员；节点回报成功后直接移除相应组成员，少于两项即删组，删除代表时按机器/路径
选择明确 Keep。实际文件活动状态和墓碑仍由节点 outbox 同步，不由管理端猜测。

TCP 传输固定为四字节大端长度头加 Protobuf Envelope；零长度、截断和超过 8 MiB 的普通帧
在 `dedup-transport` 边界拒绝，`FileChunk.data` 另限 1 MiB。`ClientConnection` 用非零
原子 request ID 和 pending 表复用请求，读循环断开时一次性失败全部等待者，重连由
`dedup-desktop-core` 负责且 transport 不重试。高低队列都有界；一个低优先级块被选中后，
下一块发送前重新检查任务控制、进度、删除和同步 ACK 等高优先级消息。

## 6. 不可破坏的硬约束

- 只构建 `x86_64-pc-windows-msvc`，不主动按 Windows 版本号拒绝启动。
- 不添加旧代码兼容、TLS、认证、自动发现、云服务、Web 前端、移动端或自动删除。
- 算法定义和采样位置硬编码；九个匹配阈值可配置并快照到分析运行。
- PDQ 输入固定先用 `(77R + 150G + 29B + 128) >> 8` 转灰度；64×64 特例直接复制，
  其余尺寸按上游两轮 Jarosz 算法降采样。不得用通用缩放器替换 PDQ 降采样。
- pHash 的 3×3 块序、DCT bit 序和小端 BLOB，以及 Sobel 的空间格、方向 bin 和零向量
  规则都是持久化契约；不得因性能重写改变输出字节或向量索引。
- 图片不生成缩略图。视频联系表固定三列两行、RGB24、JPG 质量 80，复用六个成功抽帧。
- 视频一筛/二筛不得把解码失败槽位计入分母，也不得把尚未计算的二筛特征计成零分；
  二者分别对应“无有效槽位”和“Incomplete”语义。
- FFmpeg 固定从 `worker.exe` 相对路径 `runtime/ffmpeg` 加载五个 8.0.1 x64 LGPL DLL；
  不搜索当前目录或 PATH，不运行或发布 FFmpeg EXE。
- 删除默认进入回收站；永久删除必须由设置切换。每项删除前重新检查存在、大小和流式 MD5；
  只有成功删除才立即从重复组移除，失败或跳过仍保留。
- PostgreSQL schema 只由用户手动运行 `deploy/central-v2.sql` 创建；应用只校验，不隐式 DDL。
- 文件、模块、公开函数和接口保持详细中文职责注释；实现保持简洁，不重复校验内部强类型。

## 7. 构建与测试命令

固定工具链为 Rust `1.97.1-x86_64-pc-windows-msvc`，根 `.cargo/config.toml` 固定默认目标。

```powershell
Remove-Item Env:CC -ErrorAction SilentlyContinue
Remove-Item Env:CXX -ErrorAction SilentlyContinue
cargo fmt --all -- --check
cargo clippy --workspace --all-targets --locked -- -D warnings
cargo test --workspace --locked -- --test-threads=1
cargo build --workspace --release --locked --target x86_64-pc-windows-msvc
```

构建前清除泛化的 `CC`/`CXX` 覆盖，让 `cc-rs` 按 MSVC 目标自动发现 Visual Studio；若继承
MinGW GCC，bundled SQLite 会生成 MinGW ABI 符号并在 MSVC 链接阶段失败。不得把本机 VS
安装绝对路径写入项目。

真实 PostgreSQL 测试默认 `#[ignore]`，只在固定测试容器和
`DEDUP_TEST_POSTGRES_URL` 存在时显式运行。FFmpeg 集成测试使用已校验的测试 DLL 来源，
但仍通过生产相对路径加载。每项实现遵循 RED→GREEN→REFACTOR，并只运行计划指定门禁和一次
最终综合门禁，不追加无休止审查。

## 8. 任务 19：Windows x64 便携发布包

任务 19 的设计目的，是把 Rust V2 已冻结的 Windows x64 运行闭包组装为可审计、可重复验证的
最小便携 ZIP，而不是制作安装器、迁移旧版目录或代替最终运行时验收。`scripts/build-release.ps1`
固定构建 `x86_64-pc-windows-msvc` Release 工作区，从全新的 `dist-rust-v2/staging` 开始组装；
`scripts/verify-release.ps1` 对 staging 目录或最终 ZIP 做同一套文件集合、PE 架构、许可证和哈希
检查。发布失败时不得沿用旧归档冒充本轮产物，也不得因打包方便而把仓库、target 或既有运行目录
整体复制进去。

x64 发布白名单只允许顶层 `desktop.exe`、`node.exe`、`worker.exe`，
`runtime/ffmpeg` 下固定五个 DLL，`schema/central-v2.sql`，`licenses` 许可证闭包，以及
`manifest/files.sha256`。不复制 `data`、`config.toml`、任何 SQLite/数据库、日志、缓存、旧 Go/C++
EXE 或 DLL，也不复制其他 Cargo 产物。三个 EXE 必须唯一存在且 PE Machine 为 `0x8664`；这既是
目标架构约束，也是防止旧版或调试辅助程序混入发布包的显式门禁。

staging 每次组装前只清理固定 `dist-rust-v2/staging`，并验证该路径确实是发布根的直接子目录。
组装完成后，`manifest/files.sha256` 以小写 SHA-256、正斜杠相对路径和确定性排序覆盖当时除清单
自身外的每个普通文件；验证器要求实际文件与清单一一对应，并重新计算每个文件哈希。ZIP 直接以
staging 内容为归档根，不额外嵌套 `staging` 目录，固定输出
`dist-rust-v2/mySingerServer-rust-v2-win-x64.zip`；同目录 `.zip.sha256` sidecar 保存归档 SHA-256，
验证器在 sidecar 存在时重新计算并拒绝不一致值。包内文件清单证明解压内容，sidecar 证明 ZIP
本体，二者不能互相替代。

许可证闭包固定包含 `Project-MIT.txt`、`Rust-Third-Party-Licenses.html`、
`Slint-Royalty-Free-2.0.txt`、`PDQ-BSD-3-Clause.txt` 和 `FFmpeg-LGPL-3.0.txt`，且每项必须非空。
项目根 MIT 许可证只复制为 `Project-MIT.txt`，不被第三方生成流程改写。
`scripts/generate-third-party-notices.ps1 -Destination <licenses目录>` 固定验证
`cargo-about 0.9.1`，以 `cargo metadata --locked --offline` 得到解析闭包并逐项核对 Cargo.lock；
随后只为 `x86_64-pc-windows-msvc` 生成包含 crate 名称、版本和许可证正文的 Rust HTML。Slint
子 crate 的 LicenseRef 正文不是 cargo-about 可自动映射的 license file，因此脚本从 metadata
选出全部 Slint 包并把官方 Royalty-free 2.0 正文显式追加到 HTML，同时复制独立 Slint 和 Meta PDQ
BSD-3-Clause 文本。FFmpeg LGPL 文本仍只由已校验的 `fetch-ffmpeg.ps1` 写入同一目录；任一工具、
正文或锁定依赖缺失都会终止发布，不允许用空文件或跳过许可证检查继续打包。

FFmpeg 运行闭包严格等于 `avutil-60.dll`、`swresample-6.dll`、`swscale-9.dll`、
`avcodec-62.dll`、`avformat-62.dll`，名称集合和数量都必须完全相等。任何位置出现
`ffmpeg.exe`、`ffprobe.exe` 或 `ffplay.exe` 都是发布失败；便携包只携带 Worker 所需 LGPL shared
DLL，不提供转码命令行工具，也不把下载归档的 `bin` 目录整体复制进包。

`deploy/central-v2.sql` 仅复制为包内 `schema/central-v2.sql`，供用户在目标 PostgreSQL 上手工
执行。构建器、验证器、node 和 desktop 都不得执行该脚本、创建中心业务表或修改现有数据库；
验证器只能确认 schema 文件存在，不能把“文件已打包”解释为“数据库已建库”。

`scripts/verify-release.ps1` 是静态发布物验证器，不启动三个程序，也不证明 DLL 可真实加载、Worker
Ready、任意当前目录启动、首次运行只在包旁创建 `data`、GUI/托盘/回收站或双物理机可用。
`tests/windows/Test-RustV2Package.ps1` 使用最小 PE fixture 验证静态规则的成功与拒绝路径，也不能
替代一次使用真实 Release EXE、真实 FFmpeg DLL 和真实许可证生成物构建 ZIP。静态脚本检查、package
fixture、真实打包验证和 Windows 运行时验收必须分别记录，后一步不得由前一步推导 PASS。

## 9. 验收边界

当前已建立 Rust 1.97.1 工具链和 13 成员工作区，并完成领域 ID、配置/阈值、应用目录、
Windows 路径、SMBIOS 机器身份、完整 Protobuf 清单和 TCP 传输边界；真实当前主机的
SMBIOS 读取以及 loopback TCP 请求复用测试已执行通过。固定像素管线和 Meta PDQ 纯 Rust
移植也已完成，三张固定上游 JPEG 的 256 位 golden 与 Quality 均逐位通过。九分块 pHash、
128 维 Sobel、PDQ band 候选和图片两层联合筛选的位序/阈值测试也已通过。六帧中点、
有效/缺失槽位平均规则以及 3×2 JPG 联系表测试已通过。SQLite V2 schema、路径缓存、内容复用、
图片/视频特征完整性、事务 outbox、ACK 裁剪与稳定快照测试已通过。任务恢复、分析状态链、
冻结输入、稳定组分页、复核恢复和删除后缩组测试也已通过；桌面管理 UI 的八个页面、统一本地/
中心结果模型、有限分页窗口、按需内存预览、快捷复核、删除确认和中心外部删除计划已接入并通过
定向测试与 x64 Release 编译。FFmpeg 固定供应清单、无 EXE 发布、受限 DLL 搜索、缺失 DLL 报错、JPEG/MP4 探测和
MP4 首尾 RGB24 解码集成测试已通过。真实 worker.exe 的 Ready、图片一筛结果、连续调度、
计划重启、意外退出补建、取消替换和 Job 关闭清理进程测试已通过，节点 actor、SQLite、WorkerPool
与 TCP listener 已由 NodeRuntime 完成装配。扫描阶段的 Walker 全文件契约、Everything 不可用明确错误、路径大小缓存、
强制重算、已有内容复用、不完整内容跳过、局部根失效和失败不失效测试已通过；扫描一筛生产适配器
已经直接连接 WorkerPool，节点 actor 后续只负责生命周期和命令串行化，不重新实现扫描规则。
纯 SQLite 本地分析的活动任务门禁、失败/取消重选、精确组、图片与视频一筛、二筛缓存零派发、
单端缺失、partial 精确重试、代表直连不传递、稳定成员分页及关闭重开后的复核恢复测试已通过。
节点单管理连接、连接内并发、actor 命令边界、任务高水位、原图/联系表分块、永久删除复核、
删除后缩组、20 MiB×10 日志和无 GUI 托盘命令状态测试已通过；Slint SystemTrayIcon 声明与
`node.exe` 已完成真实 MSVC 编译。Windows 桌面用户上下文中的真实删除验收已经用
`SHQueryRecycleBinW` 和 Explorer 同时证明默认删除进入回收站、存在“还原”动作，而 Permanent
不增加回收站项目；测试项随后已精确还原并清理。Computer Use 能读取 desktop 总览、八个导航标签、
在线本地节点和 24 个 Worker，但当前辅助接口不能提供 Slint 文本坐标、不能截屏且不暴露任务栏托盘，
因此页面逐项交互与托盘右键/重启/退出仍为 `PARTIAL`，不得由静态托盘测试推导 GUI PASS。
中心 `central-v2.sql` 已在计划专用 PostgreSQL 16 Alpine 空库手工执行成功，第二次执行因表已存在
明确失败；另一个空库经 `CentralStore::connect` 后仍无业务表。真实数据库测试已验证两机器共享
内容键、同 MD5 不同大小、不可变输入、候选事务、两页稳定组游标、复核删除计划和成功删除后缩组。
同步门禁已验证 2501 条严格拆为 1000/1000/501、提交前失败不 ACK、提交后 ACK 丢失由下一连接
首 ACK 收敛、快照中断整次重来、同端点断开重连和连接 Drop 释放单管理名额。真实 PostgreSQL
测试另已覆盖图片/视频 stage1+stage2、六帧、位置活动状态、删除墓碑、全量快照替换与未提交
快照 Drop 回滚；联系表 outbox 被中心明确忽略，PostgreSQL 不存在联系表或原媒体表。
跨机器编排的纯测试已覆盖 stage1/phase2 双门禁、完整一筛、PG 缓存跳过、节点缓存零 Worker 重发、
确定性节点选择、Incomplete 语义和 A-B-C 代表不传递。另已在真实 loopback TCP、节点 SQLite 与
PostgreSQL 容器上执行整链集成：扫描任务查询、outbox 同步、输入封存、中心图片两筛缓存复用和
最终图片组均通过；第二台物理主机仍留到最终验收，不由该 loopback 测试冒充。
静态测试、集成测试、发布包验证和 Windows 实际 GUI/托盘/回收站验收必须分开记录。没有实际
运行的 GUI、托盘、回收站、第二台物理主机或 PostgreSQL 项不得标记 PASS；可用双节点进程
集成测试证明协议与编排，但真实双物理机不可用时仍标 `BLOCKED`。

任务 19 的四个 PowerShell 文件已经全部通过 AST 解析；package fixture 实际输出
`RUST_V2_PACKAGE_TEST_PASS`，覆盖完整目录、完整 ZIP，以及错误 sidecar、禁带 FFmpeg EXE、缺失
`worker.exe`、非 x64 PE 和缺失 Slint 许可证的拒绝路径。固定 Cargo x64 工作区 Release 构建已
实际通过；notices 生成实际核对 699 个解析包；FFmpeg 锁定归档和五 DLL SHA 已验证。任务 20 在
回收站修复之后重新执行完整构建，`build-release.ps1`、独立 `verify-release.ps1` 和 package fixture
均输出 PASS。当前 ZIP 为 `dist-rust-v2/mySingerServer-rust-v2-win-x64.zip`，大小 64,804,379 字节，
SHA-256 为 `b99f71d79f51aab92092360cde32b6d9d13f887a7729b008927232fe9c7b4c9e`，白名单共 15 个文件。

任务 20 的真实单节点便携链覆盖扫描缓存、精确/图片/视频分析、分页、复核、永久删除和重启恢复；
双 loopback 节点覆盖独立机器身份、SQLite、高水位和输入隔离；真实 PostgreSQL 16 测试覆盖
1000/1000/501 增量、stage2、提交失败、ACK 丢失、八表快照与墓碑重放，并完成 stage1→双节点
phase2→最终图片组。桌面控制器另以已成功建立后再关闭/重启的真实 TCP 节点验证重连；真实 PG
backend 被服务端终止后能重新连接并再次 ACK，故障期间的长 PullChanges 也不阻塞 Shutdown。完整命令、结果、
GUI/托盘限制、回收站证据和复跑方法记录在
`docs/verification/2026-08-19-rust-v2-acceptance.md`。第二台真实 Windows x64 物理主机不可用，
所以真实多物理机仍为 `BLOCKED`；loopback 双节点 PASS 不能替代这一边界。
