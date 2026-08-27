# 三类计算任务与 Worker 文件会话设计

## 1. 背景

当前扫描流水线由 Node 计算 MD5，再把媒体探测和特征计算派发给 Worker。该边界会让大量 Worker 在 MD5 阶段空闲，也会让完整文件读取、媒体探测和抽帧分别打开文件。现有扫描任务还同时承担缓存判断、基础计算和一筛派发，难以独立展示各阶段耗时、恢复状态和多机器二次计算进度。

本设计把计算流程拆为三类持久任务，并把文件 MD5 下沉到 Worker。基础计算使用可续算的 Worker 文件会话，在一次打开文件后完成 MD5，并根据 Node 返回的缓存缺失项继续计算必要的缩略图和一筛特征。

## 2. 目标

- 支持只使用本地 SQLite 的单机模式。
- 支持各 Node 直接连接可选 PostgreSQL 的多机器模式，远程机器不依赖 `desktop.exe`。
- 明确区分基础计算任务、重复文件清单生成任务和二次特征计算任务。
- MD5、媒体探测、抽帧和图片解码由 Worker 执行，Node 不再读取文件内容计算 MD5。
- 同一基础计算项尽量复用一个 Windows 文件句柄，减少重复打开和重复读取。
- 本地 SQLite、PostgreSQL、视频缩略图均支持缓存复用，只计算缺失数据。
- 不按整批等待 Worker，持续填充可用槽位，并按物理磁盘限制读取并发。
- 每个阶段独立计时，进度每 2 秒合并发布一次，阶段终态立即发布。
- 单文件失败不终止整批任务，Worker 崩溃项不会无限重试。

## 3. 非目标

- 不支持网络硬盘的物理磁盘并发识别。
- 不上传或共享 Node 本地视频缩略图文件。
- 不引入 PostgreSQL 分布式任务抢占、租约或 Node 间直接通信。
- 不自动迁移旧 SQLite 或 PostgreSQL 数据。
- 不改变精确重复和相似文件判定算法的阈值语义。

## 4. 运行模式与进程职责

### 4.1 单机模式

- Node 只打开本地 SQLite。
- Node 持有基础计算任务和二次特征计算任务。
- Desktop 通过 Node 协议发起本地重复文件清单生成；分析数据和终态保存在该 Node 的 SQLite。
- PostgreSQL 配置关闭时，任何任务都不得尝试连接中心数据库。

### 4.2 多机器模式

- 每个 Node 同时使用本地 SQLite，并直接连接可选 PostgreSQL。
- Node 将本地成功结果先提交 SQLite，再通过持久 outbox 同步 PostgreSQL。
- Desktop 持有并编排重复文件清单生成任务，在 PostgreSQL 中冻结跨机器输入、生成候选并保存任务状态。
- Desktop 按文件所在机器向各 Node 派发二次特征任务；远程机器只需运行 `node.exe` 和 Worker。
- PostgreSQL 不可用时，Node 记录警告并降级为 SQLite-only 计算。已提交的 outbox 在数据库恢复后重试。

## 5. 任务模型

### 5.1 基础计算任务

基础计算任务由 Node 创建和持久化，包含三个可观察阶段。

1. `enumerate_files`
   - 枚举所有普通文件并按规范路径排序去重。
   - 枚举期间不逐文件发布 UI 数量。
   - 完成后冻结文件清单，并一次性发布总文件数。
2. `lookup_base_cache`
   - 按机器 ID、规范路径和文件大小批量查询本地 SQLite。
   - 本地未命中且 PostgreSQL 已启用时，再批量查询中心缓存。
   - PostgreSQL 命中的内容和特征导入本地 SQLite。
   - 固定按最多 1000 个文件组成查询批次；一批完成本地/中心查询、缓存导入和任务项判定后，立即更新运行时进度并持久化阶段进度，Desktop 沿用 2 秒定时同步。
   - 查询完全部文件后阶段完成；中心库失败只产生警告。
3. `compute_base_features`
   - 完整缓存命中的文件直接计入初始完成数。
   - 其余文件按物理磁盘进入 Worker 文件会话。
   - Worker 先计算 MD5；Node 再按 MD5 和文件大小确认内容缓存及本地缩略图状态。
   - 同一 Worker 只继续计算缺失的媒体探测、视频缩略图和一筛特征。
   - 单文件结果事务写入 SQLite，成功后写 outbox。

### 5.2 重复文件清单生成任务

多机器任务由 Desktop 编排并持久化到 PostgreSQL；单机任务由 Desktop 调用目标 Node 的本地分析接口，持久数据保存在该 Node 的 SQLite。任务包含三个阶段。

1. `build_candidates`
   - 精确重复按 MD5 和文件大小直接生成候选。
   - 相似图片和视频使用完整一筛特征生成候选对。
   - 输入在阶段开始时冻结，后续同步不会改变本次运行范围。
2. `dispatch_stage2`
   - 查询候选涉及内容的二次特征完整性。
   - 已有完整二次特征的内容直接复用。
   - 缺失内容按机器分组，每个内容只派发一次二次特征计算任务。
   - Desktop 持久记录派发状态并等待 Node 终态；重连后不重复派发已完成内容。
3. `final_compare`
   - 使用完整二次特征逐候选精准判定。
   - 缺失结果保持为未完成，不当作低分或零分。
   - 最终重复清单和成员关系在一个事务中替换并发布。

### 5.3 二次特征计算任务

二次特征计算任务由文件所属 Node 创建和持久化，包含两个阶段。

1. `lookup_stage2_cache`
   - 先查询本地 SQLite，再查询可选 PostgreSQL。
   - PostgreSQL 命中的完整特征导入本地 SQLite。
2. `compute_stage2_features`
   - 图片从原图片计算九分块 pHash 和 Sobel。
   - 视频优先读取本地 `md5前两位/md5值.jpg` 的 3×2 六帧缩略图，并拆分六个槽位计算二次特征。
   - 缩略图缺失、损坏或格式不符时，Worker 回退读取原视频，重新生成缩略图后立即计算。
   - 原文件也不可读时只失败当前任务项。

## 6. 缓存语义

### 6.1 路径缓存

路径缓存键固定为机器 ID、规范路径和文件大小。查询顺序为本地 SQLite、可选 PostgreSQL。只有缓存记录声明的基础字段完整时，文件才能跳过 Worker 文件会话；部分记录只减少后续缺失项，不能伪装为完整命中。

### 6.2 内容缓存

Worker 返回 MD5 后，Node 使用 MD5 和文件大小查询本地 SQLite，再查询可选 PostgreSQL。PostgreSQL 命中的媒体类型和特征导入本地 SQLite。内容缓存只决定缺失计算项，不替代当前机器的活动位置写入。

### 6.3 缩略图缓存

- 缩略图只属于当前 Node，不写入 PostgreSQL。
- 路径固定为 `<thumbnail-root>/<md5 前两位>/<完整 md5>.jpg`。
- 命中必须同时满足文件存在、可解码和固定 3×2 联系表格式。
- 基础计算和二次特征计算都先检查缩略图缓存。
- 缩略图缺失时仅在确有视频计算需求时生成，生成后可被后续任务复用。

## 7. Worker 文件会话协议

### 7.1 消息

Worker 协议增加以下消息：

- `BeginBaseCompute`：携带任务 ID、任务项 ID、机器 ID、显示路径、规范路径、文件大小、物理磁盘 ID，以及 Node 已验证的读取块大小、单块超时和重试次数。
- `BaseHashReady`：返回 16 字节 MD5，Worker 仍保留槽位和文件会话。
- `ContinueBaseCompute`：携带缺失项掩码，表示是否需要媒体探测、一筛特征和视频缩略图。
- `BaseComputeResult`：返回 MD5、实际媒体类型、所请求的特征和可选缩略图。

缺失项掩码为零时，Worker 关闭文件会话并返回只含 MD5 的完成结果。Node 不向 Worker 发送数据库内容或已有大块特征。

### 7.2 文件句柄复用

- Worker 为每个基础计算项创建一个拥有所有权的 `WorkerFileSession`。
- 文件会话使用支持随机偏移读取的 Windows 文件句柄完成流式 MD5。
- Worker 只使用 `BeginBaseCompute` 携带的已验证读取参数，不在 Worker 内另读 Node 配置。
- MD5 完成后文件会话保持打开，等待 Node 的缓存判定结果。
- 图片解码和 FFmpeg 视频探测、Seek、抽帧通过同一会话提供的读取和 Seek 边界完成。
- FFmpeg 使用基于该文件会话的自定义 AVIO，不再按路径二次打开文件。
- 文件会话结束、取消、Worker 崩溃或协议断开时关闭句柄并释放磁盘许可。

Node 等待 PostgreSQL 时不无限占用 Worker：中心查询使用连接超时；超时后立即降级为本地缓存判定并发送续算消息。

## 8. 并发与背压

- Node 为每个物理磁盘维护独立排队通道。
- 同时满足磁盘类型对应的每盘读取上限、全局读取线程上限和 Worker 进程数上限。
- 一个活动 Worker 文件会话占用一个 Worker 槽位和一个物理磁盘读取许可。
- 不再收集完整 Worker 批次后统一等待；任何结果返回后立即从符合限制的磁盘队列补充下一项。
- Worker 槽位按循环顺序分配，不固定选择最小槽位。
- PostgreSQL 查询、SQLite 单写者提交和 outbox 同步不持有物理磁盘读取许可；只有等待同一 Worker 续算决定的短暂缓存查询保留该文件会话许可。
- Worker 结果进入有界完成队列，由 Node actor 串行提交 SQLite，避免并发写数据库。

## 9. 进度与计时

每个阶段首次实际执行时记录自己的开始时间。后续阶段不得继承前一阶段的耗时。
枚举完成时冻结的总文件数允许由后续基础计算入口以相同值重复确认；该操作必须幂等，不能把已完成枚举误判为阶段倒退。不同总数的重复冻结仍按状态错误拒绝。

- 枚举文件：完成后一次性显示 `总文件数/总文件数`。
- 基础缓存查询：`已查询文件数/总文件数`。
- 基础计算：缓存完整命中数作为初始值，随后按完成文件数增加。
- 候选生成：`已处理基础特征数/总基础特征数`，并单独显示已生成候选对数。
- 二次任务派发与等待：`已完成二次特征内容数/所需内容数`。
- 精准判重：`已处理候选对数/候选对总数`。
- 二次缓存查询：`已查询内容数/总内容数`。
- 二次特征计算：缓存命中数作为初始值，随后按完成内容数增加。

运行中进度、速度、ETA、失败数和跳过数每 2 秒合并发布一次。阶段完成、失败、取消和任务终态立即发布。所有 UI 进度条从左向右填充。

任务详情中的 Worker 行显示槽位、PID、文件、物理磁盘、当前子步骤和已完成文件数。基础计算子步骤至少区分 MD5、缓存判定、媒体探测、缩略图和一筛；二次计算标明缩略图复用或原视频回退。

## 10. 失败处理与恢复

- 单文件读取、媒体探测、解码或特征计算失败只结束当前任务项。
- 单文件 MD5 协议错误、续算失败、结果解析错误、缩略图写入错误或响应类型错误，都记录文件路径和错误到 Node 日志及任务“最近失败”，把该项置为失败并继续补位；不能把仍可继续的文件错误升级为整批失败。
- SQLite 不可用/损坏、任务持久状态非法、WorkerPool 基础设施完全不可用和取消仍属于任务级终止条件；任务级错误必须写入 Node 日志和任务详情，不能静默丢弃错误文本。
- 单块默认读取超时为 3 秒，并按 Node 配置重试；超过重试次数后记录物理损坏并跳过文件。
- Worker 崩溃记录机器 ID、文件路径、Worker PID、退出码、读取块偏移、块大小、首次发生时间、最近发生时间和重复次数。
- 崩溃项不在同一运行中自动无限重试；Worker 补建后继续处理其他文件。
- PostgreSQL 连接或查询失败只记录任务警告，不增加文件失败数。
- Node 重启后将遗留运行项恢复为排队状态。未完成的 Worker 文件会话从头开始，但已事务提交的特征和有效缩略图继续复用。
- Desktop 重启或网络中断后，从 PostgreSQL 或目标 Node SQLite 恢复清单任务阶段和派发记录，不重复提交已完成的二次内容。
- 取消任务会停止新派发，关闭当前文件会话，等待数据库提交边界完成后进入取消终态。

## 11. 配置

Node 配置增加可选 PostgreSQL 基础连接段：

- `enabled`
- `host`
- `port`
- `database`
- `username`
- `password`
- `connect_timeout_seconds`

关闭 `enabled` 即为 SQLite-only 单机模式。配置由设置页对选中 Node 执行“加载配置”和“保存并重启”；Node 保存本地配置后自行重启。密码字段必须在编辑和加载过程中保留用户输入，不能因 UI 状态刷新自动清空。

现有读取配置继续控制 HDD、SSD、未知磁盘的每盘线程数、总读取线程数、块大小、单块超时和重试次数。相对路径原样保存，并以 `node.exe` 所在目录解析。

## 12. 协议与存储变化

- Node 管理协议增加基础计算任务和二次特征任务的创建、取消、摘要及详情操作。
- 运行详情增加三类任务及本文定义的阶段 ID。
- Node/Worker 和 Desktop/Node 的协议版本统一升级为 V4，不保留旧协议兼容分支。
- SQLite 继续复用 `tasks`、`task_items`、`analysis_runs` 和现有特征表，并新增 `task_stages` 与 `analysis_run_stages`。阶段表固定保存阶段 ID、状态、已完成数、总数、总数是否已知、失败数、跳过数、开始时间、结束时间和警告文本；速度与 ETA 仍是运行时派生值，不持久化。
- PostgreSQL 新增 `analysis_run_stages` 和 `analysis_stage2_dispatches`。派发表以分析运行、机器 ID、MD5 和文件大小唯一标识一个二次内容请求，并保存 Node 任务 ID、派发状态和更新时间，供 Desktop 重启或重连后继续等待。
- PostgreSQL 的 `analysis_runs` 继续保存清单任务总状态和错误文本，阶段警告写入 `analysis_run_stages`，不增加另一套任务主表。
- Node outbox 覆盖基础特征和二次特征的幂等同步。
- SQLite 与 PostgreSQL schema 标识同步升级；只初始化空数据库，旧 schema 明确拒绝打开，不执行自动迁移。

## 13. UI 变化

- 任务中心分别显示“基础计算”“重复文件清单”“二次特征计算”。
- 每类任务使用自己的阶段名称、进度单位和详情字段，不把多个阶段伪装成同时运行。
- 清单任务显示候选对数、二次特征需求数、已完成数和最终判重数量。
- 基础任务显示缓存来源、本地缩略图复用数量、当前活动磁盘和 Worker。
- 二次任务显示 SQLite 命中、PostgreSQL 命中、缩略图复用、原视频回退和失败数量。
- PostgreSQL 离线降级以警告状态展示，不把仍在正常本地计算的任务标为失败。

## 14. 验收策略

实现采用行为测试驱动，只运行本次改动直接相关的测试：

1. Worker 文件会话只打开一次文件，并在 MD5 后根据缺失项继续计算。
2. Worker 返回的 MD5 与已知文件内容一致，Node 不再调用本地 MD5 实现。
3. SQLite 命中、PostgreSQL 命中、部分命中和 PostgreSQL 离线降级。
4. 有效缩略图复用、损坏缩略图重建以及原视频回退。
5. 单盘限流、不同物理磁盘并行、全局上限和 Worker 循环调度。
6. 三类任务的阶段开始时间、进度计数、2 秒合并发布和终态立即发布。
7. Worker 崩溃、读取超时、Node 重启和 Desktop 重连后的恢复行为。
8. SQLite/PostgreSQL 新 schema 初始化、旧 schema 拒绝和 outbox 幂等同步。

相关自动化测试全部通过后，最后执行一次只读真实媒体半小时计算验收；不扩展到无关功能测试。

## 15. 实施顺序

1. 冻结三类任务、阶段 ID、缓存完整性和 Worker 续算协议的行为测试。
2. 实现 Worker 文件会话、MD5 和句柄支持的 FFmpeg AVIO。
3. 改造 Node 基础计算状态机、持续调度和本地缓存写入。
4. 增加 Node 可选 PostgreSQL 查询、导入和 outbox 同步。
5. 实现 Node 二次特征任务及缩略图复用/回退。
6. 改造 Desktop 重复文件清单任务和多 Node 二次任务协调。
7. 更新任务中心、Node 配置页、协议版本和数据库重建脚本。
8. 运行相关测试并完成最终真实媒体半小时验收。

## 16. 已落地实现映射

以下名称用于维护者从本设计直接定位当前实现，不改变前述产品语义：

- 配置：`dedup_core::NodeConfig.postgres: NodePostgresConfig`，包含 `enabled`、`host`、`port`、
  `database`、`username`、`password`、`connect_timeout_seconds`。
- 中心存储：`dedup-central-store::CentralStore`；Node 侧可降级入口为
  `dedup_node_engine::central_cache::NodeRemoteFeatureCache`。
- 基础计算：`scan::base_compute::BaseComputeEngine::run_existing`；Worker 会话为
  `worker::file_session::WorkerFileSession`。
- 二次计算：`analysis::phase2::PersistentStage2Executor`，按本地 SQLite、可选 PostgreSQL、
  本地联系表、原媒体回退顺序补齐内容。
- 运行进度：`RuntimeTaskRegistry`、`RuntimeTaskReporter`、`RuntimeStage`；Worker 详情协议字段为
  `current_step` 和 `cache_detail`。
- SQLite：`PRAGMA user_version=3`，阶段表为 `task_stages`、`analysis_run_stages`。
- PostgreSQL：`schema_metadata.schema_id=mysingerserver-rust-v2-central-schema-3`，新增
  `analysis_run_stages`、`analysis_stage2_dispatches`。
- 协议：`dedup_protocol::PROTOCOL_VERSION=4`；基础 Worker 消息为 `BeginBaseCompute`、
  `BaseHashReady`、`ContinueBaseCompute`、`BaseComputeResult`。

相关验证使用计划中的定向 `cargo test -p ... --test ... --locked -- --test-threads=1` 命令；
发布使用 `scripts/build-release.ps1`，最终真实媒体只读验收另行记录，不由静态打包验证替代。
