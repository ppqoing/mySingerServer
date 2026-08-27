# CPU 与磁盘 I/O 分阶段流水线架构调整计划

**日期：** 2026-08-23

**状态：** 待实施

**范围：** Rust V2 Node 基础计算任务

**目标：** 消除 Worker、磁盘许可和整文件生命周期绑定造成的资源气泡，使 MD5、缓存查询、媒体解码、CPU 特征计算和结果持久化能够有界重叠执行。

## 1. 文档关系与覆盖范围

本计划调整以下既有设计约束：

- `docs/superpowers/specs/2026-08-22-three-task-compute-pipeline-design.md` 第 7 节“Worker 文件会话协议”；
- 同一设计第 8 节“并发与背压”；
- `docs/superpowers/plans/2026-08-22-three-task-compute-pipeline.md` 中要求基础计算全程保留 Worker 槽位和磁盘许可的实现步骤。

发生冲突时，以本计划关于基础计算并发、MD5 所属进程、Worker 协议和磁盘许可生命周期的规定为准。以下契约保持不变：

- 基础计算、重复文件清单、二次特征计算三类任务模型；
- SQLite 为 Node 本地事实、PostgreSQL 为可选中心缓存；
- MD5 与文件大小组成内容键；
- 视频联系表缓存路径和特征格式；
- 单文件失败不终止整批任务；
- 任务项、阶段、outbox、运行详情和取消语义；
- 每块物理磁盘公平调度以及读取超时、重试和故障记录。

## 2. 本轮边界

### 2.1 计划内

- 判断并消除架构造成的 CPU、磁盘 I/O 空闲间隙；
- 将 MD5 读取、缓存解析、Worker 解码和持久化拆成独立有界阶段；
- 为 Hash 读取和 Worker 媒体读取分别授予磁盘许可；
- 使缓存查询不占用 Worker 槽位和磁盘许可；
- 增加任务成本感知、CPU 预算和 FFmpeg 线程预算；
- 增加阶段耗时、队列长度、资源等待时间和忙碌率指标；
- 建立可重复的基准测试、真实媒体只读验收和回滚门禁。

### 2.2 计划外

- SSD、HDD、Unknown 的识别算法和默认值；
- Worker 原生崩溃根因及崩溃恢复策略；
- 精确重复、相似图片和相似视频算法阈值；
- GPU 或硬件解码；
- 网络盘和远程文件系统；
- UI 大规模改版；
- 以 CPU 或磁盘始终达到 100% 作为目标。

## 3. 当前架构问题

当前基础计算的有效资源占用链为：

```text
取得磁盘许可和 Worker 槽位
  -> Worker 打开文件并计算完整 MD5
  -> Worker 保留文件会话，等待 Node 查询缓存
  -> 同一 Worker 执行 FFmpeg 探测、抽帧和特征计算
  -> Node 解析结果并持久化
  -> 释放 Worker 槽位和磁盘许可
```

该模型存在以下结构性问题：

1. `worker_count` 同时充当读取、缓存等待、解码和 CPU 计算并发，无法分别控制资源。
2. 每个 Worker 只有一个待续算文件会话；等待缓存时不能领取其他文件。
3. `DiskReadPermit` 覆盖缓存等待和 CPU 阶段，许可占用不等于正在读取磁盘。
4. 活动窗口上限等于 Worker 数，没有独立的 Hash 预取窗口和待解码缓冲区。
5. Hash 完成事件批量解析期间，远程缓存查询位于主调度循环关键路径。
6. 多个 Worker 容易同步进入 Hash、缓存等待和解码阶段，形成周期性 CPU/I/O 波峰和波谷。
7. FIFO 只保证公平，不区分小图片、大视频、顺序 Hash 和随机解码成本。
8. FFmpeg 解码线程数没有纳入 Node 的统一 CPU 预算。

## 4. 目标架构

基础计算调整为以下有界流水线：

```text
SQLite 任务项队列
        |
        v
Hash I/O 调度池 --------完整 MD5--------> 缓存解析服务
        ^                                  |          |
        |                                  |命中      |缺失
        |                                  v          v
共享物理盘预算                          完成队列   待解码队列
        ^                                             |
        |                                             v
        +-------------------------- Worker 媒体读取/解码池
                                                      |
                                                      v
                                               单写持久化队列
```

流水线遵循以下原则：

- Hash I/O 并发只受物理磁盘和全局读取预算控制，不占用 FFmpeg Worker；
- Hash 完成后立即释放 Hash 磁盘许可；
- 缓存查询不持有任何 Worker 或磁盘许可；
- 只有需要补算的文件进入 Worker 队列；
- Worker 派发前重新取得媒体读取许可，完成文件访问后释放；
- 结果持久化不持有 Worker 和源媒体磁盘许可；
- 各阶段通过有界通道连接，下游拥塞会向上游施加背压；
- 任意阶段完成一项后立即补位，不等待整批文件同步完成。

## 5. 阶段与数据契约

### 5.1 Hash 输入

```rust
/// 等待完整内容哈希的持久任务项。
struct HashWorkItem {
    item_id: String,
    scanned: ScannedPath,
}
```

### 5.2 Hash 输出

```rust
/// 已完成内容哈希并可脱离磁盘许可进入缓存查询的任务项。
struct HashedBaseItem {
    item_id: String,
    scanned: ScannedPath,
    md5: [u8; 16],
    physical_disk_id: String,
}
```

Hash 输出不再携带文件版本或修订标识。本计划接受“扫描期间媒体文件不会被修改、替换或继续写入”的运行前提。

### 5.3 缓存解析输出

```rust
/// 缓存判定完成后等待直接提交或 Worker 补算的任务项。
enum ResolvedBaseItem {
    CacheHit(CacheHitCompletion),
    Compute(BaseComputeJob),
}
```

`BaseComputeJob` 包含任务身份、路径、MD5、媒体类型提示、缺失项掩码、物理盘身份和读取限制。

### 5.4 Worker 契约

基础计算改为一次请求、一次终态响应：

```text
ComputeBaseFeatures -> BaseComputeResult | WorkerFailure
```

废止基础计算主路径中的：

- `BeginBaseCompute`；
- `BaseHashReady`；
- `ContinueBaseCompute`；
- Worker 内单一 `PendingBaseSession`。

Worker 收到请求后执行：

1. 打开当前文件；
2. 校验文件可访问且当前长度与枚举长度一致；
3. 只计算 `missing_parts` 指定的内容；
4. 完成媒体读取后释放文件资源；
5. 返回可按任务项身份提交的计算结果。

Node 与 Worker 协议版本整体提升，不保留同一运行包内的新旧基础计算协议混用。

## 6. 并发与背压模型

### 6.1 独立并发窗口

| 阶段 | 并发来源 | 初始容量规则 |
|---|---|---|
| Hash I/O | 现有 `DiskReadConfig` | 每盘上限与 `total_threads` |
| 缓存查询 | Node 异步批处理 | `max(2, worker_count)`，设置硬上限 |
| 待解码队列 | 有界内存队列 | `worker_count * 2` |
| Worker 解码 | WorkerPool | 有效 Worker 数与 CPU 预算的较小值 |
| 持久化 | NodeStore 单写者 | 单写，允许小批量合并 |

所有乘法和容量计算必须使用受检算术并设置产品级硬上限。队列容量由配置推导，不增加无上限用户配置。

### 6.2 共享磁盘调度

`DiskReadScheduler` 增加读取类别：

```rust
enum DiskReadClass {
    HashSequential,
    MediaDecode,
}
```

同一物理盘内使用带老化的加权公平队列：

- 没有待解码项时，Hash 可以使用全部可用许可；
- 有待解码项时，优先防止 Worker 断粮；
- 连续解码授予达到预算后必须授予等待中的 Hash，防止 Hash 饿死；
- 等待超过阈值的项提升优先级；
- 不突破现有每盘和全局并发上限。

初始权重采用 `MediaDecode:HashSequential = 3:1`，该权重必须通过基准数据确认后才能成为最终默认值。

### 6.3 CPU 与 FFmpeg 线程预算

增加 Node 进程内 CPU 许可：

- 图片任务权重为 1；
- 视频任务权重为计划分配的 FFmpeg 线程数；
- 活跃任务权重总和不得超过有效 CPU 预算；
- Worker 数仍是进程隔离上限，不再代表全部 CPU 线程数；
- 使用 FFmpeg AVOption 显式设置解码线程数，不能依赖各解码器隐式默认值；
- 初始解码线程数由 `CPU 预算 / 最大活跃视频任务数` 推导，最少 1，并设置小型上限；
- 最终公式和上限通过混合媒体基准确定，不在缺少数据时固定为机器无关常量。

## 7. 媒体不可变前提

本计划按用户确认采用以下运行前提：

- 扫描根中的媒体文件在任务期间不会被下载程序、同步软件或用户修改；
- 文件不会在 Node 完成 Hash 后、Worker 再次打开前被替换；
- 不为极端外部改写场景增加文件版本、USN、文件 ID 或前后时间戳校验；
- 不增加 `FileChanged` 重试状态和对应协议字段。

仍保留现有基础错误处理：文件无法打开、实际长度与枚举长度不一致、读取失败或解码失败时，只失败当前文件并继续任务。这些检查用于处理缺失和普通 I/O 错误，不构成文件版本一致性机制。

## 8. 可观察性

为每个任务累计以下阶段指标：

- `hash_queue_wait_ms`、`hash_read_ms`、`hash_bytes`；
- `cache_queue_wait_ms`、`cache_lookup_ms`；
- `decode_queue_wait_ms`、`media_read_decode_ms`；
- `feature_cpu_ms`、`persist_ms`；
- Hash、缓存、待解码、持久化队列当前长度和峰值；
- Hash 许可、媒体读取许可和 CPU 许可当前占用；
- Worker 的 `idle/hash_wait/decode/feature/result_wait` 状态；
- 每种媒体类型和大小桶的完成数、字节数和耗时。

运行详情每 2 秒合并发布；终态立即发布。高频指标只保存在内存和结构化日志，不逐文件同步 PostgreSQL，也不为本轮增加 SQLite 大量明细行。

性能判断使用以下指标，不以单个 Worker CPU 是否平均为依据：

- 整体文件数/秒和有效字节数/秒；
- 有待处理任务时全部 Worker 同时空闲的时间占比；
- CPU 预算利用率和物理盘许可利用率；
- 各阶段 P50/P95/P99 等待及执行时间；
- 任务尾部最后 10% 文件耗时；
- 内存峰值和队列峰值。

## 9. 实施任务

实现代码时使用行为测试驱动；测试必须观察调度结果、许可计数、队列背压和持久化结果，禁止用 `read_source()`、`contains()` 或静态源码匹配代替行为验证。

### Task 0：冻结基线与性能夹具

**主要文件：**

- Create: `crates/node-engine/tests/base_compute_utilization.rs`
- Create: `crates/node-engine/benches/base_compute_pipeline.rs`
- Modify: `crates/node-engine/Cargo.toml`
- Create: `docs/verification/2026-08-23-cpu-io-pipeline-baseline.md`

- [ ] 建立可控 Hash、缓存等待、Worker 解码和持久化测试替身。
- [ ] 覆盖“小文件、大文件、缓存命中、缓存缺失、缓存延迟”混合负载。
- [ ] 记录旧架构各阶段耗时、资源空闲时间和总体吞吐，作为同机对照基线。
- [ ] 固定随机种子、文件清单和运行参数；真实媒体保持只读。

### Task 1：建立独立 Hash I/O 池

**主要文件：**

- Modify: `crates/node-engine/src/scan/pipeline.rs`
- Modify: `crates/node-engine/src/scan/base_compute.rs`
- Modify: `crates/node-engine/src/io/retrying_reader.rs`
- Modify: `crates/node-engine/tests/base_compute_pipeline.rs`
- Modify: `crates/node-engine/tests/scan_parallelism.rs`

- [ ] 复用 `ScheduledFileReader::read` 和 `RetryingFileReader` 在 Node 侧完成 MD5。
- [ ] 使用 `PipelineFileReader` 测试边界，不为测试引入生产分支。
- [ ] Hash 完成后先释放磁盘许可，再发送 `HashedBaseItem`。
- [ ] Hash 活动数只受磁盘读取配置控制，不受 Worker 数直接限制。
- [ ] 证明 Worker 全部忙碌时 Hash 仍可推进到有界队列容量。
- [ ] 证明缓存查询阻塞时不持有 Hash 磁盘许可。

### Task 2：拆出非阻塞缓存解析服务

**主要文件：**

- Modify: `crates/node-engine/src/scan/base_compute.rs`
- Modify: `crates/node-engine/src/central_cache.rs`
- Modify: `crates/node-store/src/content.rs`
- Modify: `crates/node-engine/tests/base_compute_pipeline.rs`

- [ ] 将 SQLite 查询、PostgreSQL 批量查询和缓存导入封装为有界缓存解析阶段。
- [ ] 保持 SQLite-only 和 PostgreSQL 降级语义不变。
- [ ] 远程查询等待不得阻塞 Hash 完成事件和 Worker 完成事件归并。
- [ ] 缓存命中项直接进入持久化完成队列；缺失项进入待解码队列。
- [ ] 证明全部缓存查询等待时 Worker 槽位为零占用。

### Task 3：改为一次性 Worker 基础计算协议

**主要文件：**

- Modify: `proto/node.proto`
- Modify: `crates/protocol/src/lib.rs`
- Modify: `crates/protocol/tests/worker_base_compute_wire.rs`
- Modify: `crates/node-engine/src/worker/pipeline.rs`
- Modify: `crates/node-engine/src/worker/pool.rs`
- Modify: `crates/node-engine/src/worker/file_session.rs`
- Modify: `crates/node-engine/tests/worker_base_session.rs`
- Modify: `apps/worker/src/main.rs`

- [ ] 新增 `ComputeBaseFeatures` 一次性请求并提升协议版本。
- [ ] 请求携带 MD5、缺失项掩码、读取限制和 CPU 预算。
- [ ] 删除基础计算主路径的 Pending/Continue 会话状态。
- [ ] Worker 在一次请求内打开一次文件并完成所需媒体读取与计算。
- [ ] 保持取消、进程替换和任务身份归并契约。

### Task 4：分离 Hash 与媒体读取许可

**主要文件：**

- Modify: `crates/node-engine/src/io/scheduler.rs`
- Modify: `crates/node-engine/src/scan/pipeline.rs`
- Modify: `crates/node-engine/src/scan/base_compute.rs`
- Modify: `crates/node-engine/tests/disk_scheduler.rs`
- Modify: `crates/node-engine/tests/base_compute_utilization.rs`

- [ ] 增加 `DiskReadClass` 和同盘加权公平队列。
- [ ] Worker 派发前取得 `MediaDecode` 许可，Worker 不再持有 Hash 许可。
- [ ] Worker 完成源文件访问后释放媒体读取许可；结果落库不持有许可。
- [ ] 验证 Hash 和媒体读取都不突破每盘与全局上限。
- [ ] 验证持续 Hash 压力下媒体解码不会饿死，持续解码压力下 Hash 也能推进。

### Task 5：增加 CPU 预算和成本感知调度

**主要文件：**

- Modify: `crates/node-engine/src/worker/pool.rs`
- Modify: `crates/node-engine/src/scan/base_compute.rs`
- Modify: `crates/media-ffmpeg/src/decode.rs`
- Modify: `crates/media-ffmpeg/src/ffi.rs`
- Modify: `crates/node-engine/tests/base_compute_utilization.rs`
- Modify: `crates/media-ffmpeg/tests/custom_io.rs`

- [ ] 增加 CPU 加权许可和显式 FFmpeg 解码线程参数。
- [ ] 按媒体类型、文件大小和等待时间形成小文件/大文件加权队列。
- [ ] 使用老化规则防止大文件永久等待。
- [ ] Worker 返回后立即归还 CPU 许可并补位。
- [ ] 通过可控测试证明 CPU 权重总和不超过预算且队列保持工作守恒。

### Task 6：将持久化移出计算资源生命周期

**主要文件：**

- Modify: `crates/node-engine/src/scan/base_compute.rs`
- Modify: `crates/node-engine/src/contact_sheet_cache.rs`
- Modify: `crates/node-store/src/tasks.rs`
- Modify: `crates/node-engine/tests/base_compute_pipeline.rs`

- [ ] Worker 结果先释放 Worker、CPU 和源磁盘许可，再进入单写队列。
- [ ] SQLite、联系表原子写入和 outbox 保持现有事务顺序。
- [ ] 单写失败仍停止相关任务，不能提前显示成功。
- [ ] 乱序 Worker 结果必须按 `task_id/item_id/content_id` 精确归并。

### Task 7：运行详情和配置投影

**主要文件：**

- Modify: `crates/node-engine/src/runtime_tasks.rs`
- Modify: `crates/protocol/src/convert.rs`
- Modify: `crates/desktop-core/src/runtime_tasks.rs`
- Modify: `crates/desktop-ui/src/models.rs`
- Modify: `crates/node-engine/tests/scan_runtime_details.rs`
- Modify: `crates/protocol/tests/runtime_tasks_wire.rs`

- [ ] 增加阶段队列、等待耗时、资源占用和 Worker 子状态投影。
- [ ] 保持每 2 秒合并、终态立即发布。
- [ ] 缺少新指标时显示 `—`，不得构造估算值冒充实时数据。
- [ ] 不把高频性能明细写入 PostgreSQL。

### Task 8：回归、基准与真实媒体验收

**主要文件：**

- Modify: `docs/verification/2026-08-23-cpu-io-pipeline-baseline.md`
- Create: `docs/verification/2026-08-23-cpu-io-pipeline-acceptance.md`

- [ ] 运行协议、磁盘调度、基础计算、Worker 会话、缓存、取消和恢复定向测试。
- [ ] 使用相同机器、配置、媒体清单和冷/热缓存条件分别运行旧基线与新流水线至少 3 次。
- [ ] 取中位数比较吞吐、资源气泡、P95 等待、尾部耗时和内存峰值。
- [ ] 运行一次 30 分钟真实媒体只读测试；数据库、缓存和日志使用隔离目录。
- [ ] 生成中文验收报告，明确静态测试、基准测试和真实运行结论的边界。

## 10. 行为验收矩阵

| 场景 | 必须满足的行为 |
|---|---|
| 缓存查询被阻塞 | Hash 可推进到有界队列容量；不占用 Worker 和磁盘许可 |
| Worker 全部忙碌 | Hash 可继续填充待解析/待解码队列，达到容量后产生背压 |
| Hash 读取被阻塞 | 已经进入待解码队列的文件继续由 Worker 处理 |
| 全部缓存命中 | 不启动 FFmpeg Worker，只完成 Hash、缓存解析和提交 |
| 缓存全部缺失 | Worker 持续补位，不等待整批 Hash 或整批解码完成 |
| 小文件和大视频混合 | 小文件可快速完成，大文件在老化后必定获得调度 |
| 结果乱序返回 | 每个结果写入正确任务项和内容 ID |
| 任务取消 | 停止新领取，取消排队和读取，终态提交边界保持一致 |
| 队列达到上限 | 上游等待，不继续增长内存 |

## 11. 性能验收门禁

性能门禁以 Task 0 同机基线为准，初始目标如下：

1. 混合媒体基准的总墙钟时间中位数至少降低 15%；
2. 有待处理任务时“全部 Worker 空闲且没有磁盘读取”的资源气泡时间至少降低 50%；
3. 缓存查询等待期间占用的 Worker 槽位和磁盘许可必须为 0；
4. 任务尾部最后 10% 文件耗时不得高于基线；
5. P95 单文件完成时间不得恶化超过 10%；
6. 峰值内存不得超过基线 1.25 倍，且必须受队列容量约束；
7. 结果数量、MD5、媒体类型、特征、缩略图和任务终态必须与基线一致；
8. 未达到吞吐目标时不得仅凭 CPU 或磁盘曲线更平滑宣称优化成功。

如果硬件或媒体分布使 15% 吞吐目标不适用，验收报告必须保留原始数据并由用户决定是否接受，不得自行降低门禁。

## 12. 发布与回滚

- Node、Worker、Desktop 和协议库必须成套打包，不允许只替换单个可执行文件；
- 本轮原则上不修改 SQLite/PostgreSQL schema，已有活动任务在重启恢复后从当前未完成任务项重新进入 Hash 阶段；
- 发布前保留上一版完整便携目录和 SHA-256；
- 新包先使用隔离数据库、缓存和真实媒体只读根运行烟雾测试；
- 再执行同配置的短时 A/B，确认任务进度、吞吐和资源指标；
- 若出现结果不一致、任务无法收束、吞吐门禁失败或内存无界增长，整体恢复上一版目录；
- 回滚不混用新旧协议可执行文件，也不复制新运行中的数据库覆盖旧环境。

## 13. 完成定义

只有同时满足以下条件，才能声明本架构调整完成：

1. MD5 不再占用 FFmpeg Worker；
2. 缓存查询不持有 Worker、Hash 许可或媒体读取许可；
3. Hash、缓存、解码和持久化存在独立有界队列；
4. Hash 与媒体读取使用独立生命周期的磁盘许可；
5. Worker 使用一次性基础计算请求，不再等待 Continue；
6. CPU 和 FFmpeg 线程总量受统一预算控制；
7. 小文件和大文件混合负载不存在永久饥饿；
8. 所有行为测试、协议测试和相关恢复测试通过；
9. 同机三轮基准满足性能门禁；
10. 30 分钟真实媒体只读验收完成并形成中文报告；
11. 发布包、部署文件、SHA-256 和回滚包均完成核验。
