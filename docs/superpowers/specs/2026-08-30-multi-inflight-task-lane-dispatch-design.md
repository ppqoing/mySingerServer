# Dispatcher 同盘多在途身份设计补充

日期：2026-08-30

## 1. 问题与结论

当前 `TaskFileDispatcher` 把每个物理盘 lane 建模为“一个未 ACK 身份”：首个文件取得读取许可后，直到对应 SQLite ACK 把任务行从 `P` 改为 `C/F`，同一 lane 才能交付下一文件。这个限制把磁盘读取许可、Worker 容量和 SQLite 提交错误地合并成一个串行屏障，使配置中的 SSD/HDD 逐盘并发额度无法在单个任务文件内生效。

本次解除硬编码的“每盘一个在途文件”，改为：

- 每个物理盘仍只有一个 `TaskDiskLane` 和一个 TSV 任务文件；
- Dispatcher 按 `TaskFileIdentity` 精确拥有多个未 ACK 文件；
- 同一 lane 的身份窗口上限来自冻结的 `TaskDiskLane.per_disk_limit`，不是写死常量；
- `DiskReadScheduler` 继续负责真实的全局额度、逐盘活动许可、配置权重、Hash/Media 公平和老化保护；
- continuation 复用原身份，不增加 TSV 行，也不重复占用身份窗口；
- 每个身份仍只有在自己的 SQLite ACK 到达后才从 `P` 改为 `C/F`，ACK 允许乱序。

这是一项 Dispatcher 所有权修订，不改变任务文件格式、数据库 schema、Worker 协议或媒体算法。

## 2. 已确认的现状

### 2.1 可以直接复用的能力

`TransientTaskFileSet` 的 `LaneState.in_flight` 已是 `BTreeSet<TaskFileIdentity>`，并已有行为测试证明：

- 同一 lane 可以领取多个不同身份；
- 预读窗口可在其他身份仍在途时继续补充；
- SQLite ACK 可以按任意顺序精确修改对应行首状态字节。

`DiskReadScheduler::acquire_lane` 已接收 `effective_limit` 与 `configured_weight`，能够执行：

- `read.total_threads` 全局硬上限；
- SSD/HDD/Unknown 的逐盘硬上限；
- 当前窗口先按 `active/configured_weight` 补足欠配额盘；
- 全局席位足够时每个 Ready 盘至少一个席位；
- Ready 盘名义总额超过全局席位时按约分权重、游标和完成边界轮转；
- 低权重 lane 的老化保护；
- Hash/Media 类别公平。

`task_file_base_stream` 也已经逐事件重新计算 Hash slot、Media Worker slot、远端查询和 SQLite ACK 状态，因此不需要增加第二个调度线程或新的读取线程池。

### 2.2 必须删除的串行假设

当前串行点集中在 `crates/node-engine/src/task_dispatch.rs`：

- `pending` 以 lane 文件名为键，只表达 lane 级等待；
- `in_flight_lanes` 保存 `lane -> 唯一 TaskFileIdentity`；
- `has_admitted_work` 和 `start_lane_requests` 看到任一不同身份在途就跳过该 lane；
- `poll_lane_requests` 明确拒绝同 lane 第二个不同身份；
- `abandon_in_flight` 因同 lane 的其他 pending 而拒绝清理当前身份；
- 测试 `one_lane_waits_for_ack_before_delivering_next_identity` 把上述实现细节误写成长期契约。

## 3. 方案比较

### 方案 A：同一 Dispatcher 内按精确身份维护有界窗口（采用）

保留一个物理盘 lane 和一个 TSV，Dispatcher 把 lane 的在途状态改为身份集合。普通队首在窗口有空位时申请许可，continuation 复用已经在途的身份。真实读取并发仍由 `DiskReadScheduler` 的 permit 控制。

优点：修改集中、任务文件顺序不变、权重只计算一次、取消和 ACK 可以逐身份闭合。

### 方案 B：把一个物理盘伪装成 N 个虚拟 lane（不采用）

该方案会让同一物理盘重复获得权重和老化机会，破坏“同盘根合并”和逐盘硬上限语义，也会制造多个 TSV owner。

### 方案 C：每个 Worker 或每个文件创建独立 Dispatcher（不采用）

该方案会拆散任务文件唯一 owner，使公平状态、取消清理和终态 ACK 分布在多个对象中，无法保持当前简单的单事件泵结构。

## 4. 状态模型

目标状态如下：

```rust
pub struct TaskFileDispatcher<Provider: TaskLanePermitProvider> {
    files: TransientTaskFileSet,
    provider: Provider,
    pending: BTreeMap<TaskFileIdentity, PendingPermit<Provider::Permit>>,
    continuations: BTreeMap<TaskFileIdentity, TaskFileRecord>,
    in_flight_by_lane: BTreeMap<String, BTreeSet<TaskFileIdentity>>,
    continuation_claimed: BTreeSet<TaskFileIdentity>,
    observed_epoch: u64,
    publication_wait: Option<Pin<Box<dyn Future<Output = u64> + Send>>>,
}
```

`pending` 改用完整身份作为键，避免同 lane 的一个等待请求阻止另一个身份做精确清理。实现仍保持每个 lane 最多一个普通队首许可 future；continuation 必须按稳定身份顺序优先，避免一次把整个 TSV 预领取进 scheduler。

每个 lane 的身份窗口计算为：

```text
已交付但未 ACK 的不同身份数
+ 尚未交付的普通队首许可请求数
<= TaskDiskLane.per_disk_limit
```

Media continuation 的身份已经在集合内，因此只申请新的 Media 读取许可，不增加身份窗口计数。实际同时持有的磁盘 permit 仍由 `DiskReadScheduler` 限制；窗口只是防止 SQLite 极慢时无界积累未 ACK 上下文。

## 5. 分发流程

每次事件泵重新进入 Dispatcher 时执行：

1. 丢弃当前 admission 已禁止的 pending future；任务行仍为 `P`。
2. 对每个 lane 先选择尚未 pending 的最早 continuation；它复用原身份。
3. 若没有可派发 continuation，且该 lane 的身份窗口未满，则观察一个普通 TSV 队首并申请许可。
4. scheduler 许可成功后，普通项调用 `take_lane_exact` 领取并加入该 lane 的身份集合；continuation 只做原身份与派生记录校验。
5. 每次只向事件泵返回一个 `DispatchedTask`，随后重新计算 Hash/Media admission 和全部容量。

Dispatcher 不自己实现权重轮转，也不一次性发满某个 SSD。它只持续把真实 Ready 请求暴露给同一个 `DiskReadScheduler`，由 scheduler 在多个物理盘和 Hash/Media 类别之间裁决。

## 6. ACK、失败、取消与续算

### 6.1 SQLite ACK

- `mark_completed(identity)` 和 `mark_failed(identity)` 只验证、修改并释放该身份；
- 释放一个身份后，仅该 lane 的窗口增加一个空位；
- 其他同 lane 身份及其 permit、Worker、continuation 不受影响；
- 第二项可以先 ACK，第一项仍保持 `P`，不要求 FIFO ACK。

### 6.2 continuation

- Hash→Media 继续使用同一 `TaskFileIdentity` 和同一 TSV 行；
- 同身份不能重复登记 continuation；
- continuation pending 时不能提前写 `C/F`；
- continuation 不增加 lane 身份计数，但必须重新取得 Media permit；
- 若同 lane 普通队首正在等待 permit，允许撤下该普通 future，让已在途 continuation 先行；普通行保持 `P`，稍后按原身份重试。

### 6.3 permit 或单文件失败

- scheduler permit future 失败时只移除对应 pending，普通 TSV 行保持 `P`，允许重试；
- 已交付文件的单文件失败经 SQLite 故障事务 ACK 后写 `F`；
- 一个身份失败不能删除同 lane 其他身份或 continuation。

### 6.4 取消和任务级错误

收束顺序保持不变：

```text
停止新分发
  -> 丢弃全部 pending permit future
  -> cancel/join Hash 与远端 future
  -> cancel/drain Worker，释放 Media permit
  -> 丢弃未 ACK 持久化动作
  -> 按 in_flight_identities 快照逐身份 abandon
  -> discard 运行目录
```

`abandon_in_flight(identity)` 只检查该身份是否仍有 pending future，不因同 lane 的其他身份拒绝清理。取消不写 `F`，所有未 ACK 行保持 `P`。

## 7. 与现有容量的关系

有效并行同时受以下边界约束：

- lane 身份窗口：`TaskDiskLane.per_disk_limit`；
- 真实逐盘 permit：`TaskDiskLane.per_disk_limit`；
- 全局 permit：`DiskReadConfig.total_threads`；
- Hash：`TaskFileBaseCoordinatorOptions.hash_capacity`；
- Media：`TaskFileBaseCoordinatorOptions.worker_capacity` 和实际 Worker 槽位；
- SQLite：有界 `BaseStoreActor` 队列和逐条 ACK。

示例不是常量：若配置为 SSD 5、HDD 1、全局 6，两个 lane 持续 Ready 时形成 5 个 SSD 席位和 1 个 HDD 席位；若全局仅 3，则先保证两个 Ready 盘各一个，剩余席位给欠配额盘，并在完成边界继续加权轮转。若三块等权盘争两个全局席位，首轮覆盖两盘，任一 permit 释放后轮到尚未覆盖的第三盘。若只有一盘 Ready，它可以借满全部可用全局席位，后到盘从后续自然释放边界补足，不抢占在途读取。

## 8. 验收标准

实现必须同时满足：

1. 同一 SSD lane、额度 5 时，首个 ACK 前可交付 5 个不同身份，第 6 个等待；任意一个身份 ACK 后第 6 个可继续。
2. HDD lane、额度 1 时保持串行。
3. 同 lane 多身份允许乱序 ACK，只有对应字节变为 `C/F`。
4. continuation 与普通队首可共存，continuation 复用身份且不增加 TSV 行。
5. admission 切换、permit 失败、取消和任务级错误都不串项、不泄漏 permit、不误写 `F`。
6. SSD/HDD 双 lane 按配置值和全局额度运行；等权双 SSD、配置 5:1、Ready 盘多于全局席位三种当前窗口行为均有真实 actor 测试，原有长期权重、老化和 Hash/Media 公平测试不退化。
7. 真实基础流水线在同一物理盘上能在首个 SQLite ACK 前启动多个 Hash 或多个 Media Worker。
8. 单次双物理盘真实媒体运行中，目标盘活动许可峰值不再被 Dispatcher 固定为 1；最终任务若未完成仍必须报告 FAIL/INCONCLUSIVE。

## 9. 不做事项

- 不修改 TSV 列、SQLite schema、Protobuf 或 Worker 计算协议；
- 不新增恢复任务、索引文件、分页、TaskCatalog 或第二套任务表；
- 不复制物理盘 lane，不新增读取线程池；
- 不在本项中修复独立的 Worker 原生崩溃、Everything、验收清理或 exporter 问题；
- 不部署到 `I:\Tool`；
- 历史验证文档保留原样，只新增本次修订记录，不回写历史结论。
