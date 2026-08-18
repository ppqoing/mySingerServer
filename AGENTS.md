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

节点 SQLite V2 使用 22 张严格表闭合内容、位置、特征、任务、分析、分组、复核、同步和删除。
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

SQLite 的业务更新与 outbox 递增序号同事务提交。ACK 只前进、不越过节点真实高水位，并裁剪
已确认行；请求游标落后于裁剪边界时返回 `SnapshotRequired`。整库快照固定一个读事务和起始
高水位，按稳定主键逐表分页；快照载荷和增量载荷只传播跨边界键，不泄露本地自增 `content_id`。

跨机器分析先冻结节点集合、各节点 task highwater 和 sync highwater。中心使用完整 stage1
数据批量生成候选，一筛结束后才批量派发缺失 stage2；数据库已有二筛结果时不派发。所有节点
计算完成且 stage2 同步过高水位后才最终筛选。失败运行保持 `partial`，显式重试只补缺失项。

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

## 8. 验收边界

当前已建立 Rust 1.97.1 工具链和 13 成员工作区，并完成领域 ID、配置/阈值、应用目录、
Windows 路径、SMBIOS 机器身份、完整 Protobuf 清单和 TCP 传输边界；真实当前主机的
SMBIOS 读取以及 loopback TCP 请求复用测试已执行通过。固定像素管线和 Meta PDQ 纯 Rust
移植也已完成，三张固定上游 JPEG 的 256 位 golden 与 Quality 均逐位通过。九分块 pHash、
128 维 Sobel、PDQ band 候选和图片两层联合筛选的位序/阈值测试也已通过。六帧中点、
有效/缺失槽位平均规则以及 3×2 JPG 联系表测试已通过。SQLite V2 schema、路径缓存、内容复用、
图片/视频特征完整性、事务 outbox、ACK 裁剪与稳定快照测试已通过。任务恢复、分析状态链、
冻结输入、稳定组分页、复核恢复和删除后缩组测试也已通过。Worker、节点服务和 UI 将在后续
任务按本文件架构填充。
静态测试、集成测试、发布包验证和 Windows 实际 GUI/托盘/回收站验收必须分开记录。没有实际
运行的 GUI、托盘、回收站、第二台物理主机或 PostgreSQL 项不得标记 PASS；可用双节点进程
集成测试证明协议与编排，但真实双物理机不可用时仍标 `BLOCKED`。
