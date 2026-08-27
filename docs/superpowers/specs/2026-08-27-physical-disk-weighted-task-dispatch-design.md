# Rust V2 按物理盘加权任务分发设计

日期：2026-08-27

## 1. 决策摘要

Rust V2 在获取扫描根下的文件列表之前，先解析每个扫描根对应的物理盘编号和介质类型，并冻结本次任务的磁盘拓扑。文件进入基础计算后，不再只依赖全局任务 FIFO，而是进入按物理盘划分的 Ready Queue。任务分发使用配置驱动的加权亏欠轮转，并同时受全局读取额度、逐物理盘硬上限、Worker 槽位和现有流水线 credit 限制。

权重不写死。每块物理盘从本次任务冻结的 Node 配置读取：

- HDD：`read.hdd_threads_per_disk`；
- SSD：`read.ssd_threads_per_disk`；
- 无法可靠判断类型的本地盘：`read.unknown_threads_per_disk`；
- 全部物理盘合计：`read.total_threads`。

同一数值同时表达该物理盘的调度权重和读取并发硬上限。全局额度小于全部逐盘额度之和时，调度器按权重跨多个分发窗口累计保持比例，并沿用现有老化保护，避免低权重 HDD 长期饥饿。

现有 `DiskReadScheduler` 继续负责真实读取许可、复合盘原子占用和最终硬限制。新增的任务分发层负责让正确物理盘的候选在 Worker/Hash 启动之前就可见，二者职责不重叠。

## 2. 背景与根因

当前链路已经有两层调度：

1. 枚举结果在进入 BaseCompute 前按扫描根做 1:1 轮转；
2. 文件已经进入读取请求队列后，`DiskReadScheduler` 按物理盘执行 FIFO、盘间轮转、类别权重和老化保护。

这解决了多个根长期按路径前缀串行的问题，但仍有以下缺口：

- 根轮转固定为 1:1，不能表达 SSD、HDD 和 Unknown 的配置权重；
- 多个根可能位于同一物理盘，按根计权会错误放大该盘额度；
- 路径缓存命中、远端缓存返回顺序和单文件失败会改变真正需要 Hash/媒体计算的任务比例，静态输入重排无法持续修正；
- BaseCompute 仍通过 `claim_next_item` 按 `item_id` 领取 queued 项，有限窗口前部可能再次只包含一块盘；
- 媒体许可 future 会占用 Worker admission 窗口。如果窗口前部全部属于同一 HDD，其他盘任务即使已经持久化，也可能无法进入许可候选。

因此，物理盘调度必须前移到任务分发边界，并在 Hash Ready 和 Media Ready 两个动态边界持续生效。

## 3. 目标与非目标

### 3.1 目标

1. 文件枚举前确定扫描根的物理盘编号和 HDD/SSD/Unknown 类型。
2. 同一物理盘上的多个盘符、分区或扫描根只获得一份逐盘权重和硬上限。
3. 不同物理盘在都有 Ready 任务时，按有效配置加权分发。
4. 全局额度不足时保持长期加权公平，并保证低权重盘有界等待。
5. 缓存命中、读取失败或 Worker 崩溃不能破坏后续 Ready 任务的按盘调度。
6. 任务取消、错误、进程退出和正常完成都必须恰好释放一次调度所有权。
7. SQLite 继续作为任务状态和恢复权威，不因调度优化丢失或重复任务项。
8. 保持最终结果集合、MD5、特征数据和按规范路径生成摘要的业务语义不变。

### 3.2 非目标

- 不修改 HDD/SSD/Unknown 的识别算法和配置默认值；
- 不自动调整 Worker 数、FFmpeg 线程数或读取配置；
- 不修复 Worker 原生崩溃、协议帧截断或媒体解码算法；
- 不引入 Node 代读媒体、共享内存或跨进程数据块服务；
- 不修改 `task_items` SQLite 表结构；
- 不触碰或替换 `I:\Tool` 下的部署文件；
- 本设计不要求不同物理盘的字节吞吐相等，公平对象是可分发任务和读取席位。

## 4. 枚举前磁盘规划

### 4.1 ScanRootPlan

在创建 Everything/Walker 枚举请求之前，为每个规范化、去重后的扫描根生成 `ScanRootPlan`：

```text
ScanRootPlan
  display_root
  normalized_root
  disk_key               排序去重后的底层 PhysicalDisk 编号集合
  physical_disk_id       供日志和遥测显示
  disk_kind              Hdd / Ssd / Unknown
  per_disk_limit         从本次冻结配置取得
```

规划阶段只按扫描根调用 Windows Storage API，不逐文件查询。根规划成功后才允许启动 Everything 或 Walker 获取文件列表。

### 4.2 根合并

- 多个根解析到相同 `disk_key` 时，合并为同一物理盘 lane；权重只计算一次。
- 同一物理盘的不同根对介质类型观察不一致时，按保守策略归为 Unknown；有效上限取 `unknown_threads_per_disk` 与全部已观察类型配置值的最小值。
- 物理盘编号成功但介质类型无法可靠识别时，使用 Unknown 配置。
- 物理盘编号无法解析时，在枚举前返回稳定错误，并记录具体扫描根；不得静默创建虚拟盘或退化为无上限读取。
- 复合卷的 `disk_key` 包含全部底层物理盘编号，后续任务必须原子占用全部相关盘。

### 4.3 跨卷边界

枚举器不跨越根内部的卷挂载点或重解析点。需要扫描另一个卷时，必须把该路径配置为独立扫描根，使其在枚举前拥有独立 `ScanRootPlan`。这保证文件继承的磁盘身份不会因目录树中的隐藏跨卷边界而失真。

### 4.4 文件归属

枚举结果完成全局规范路径去重后，按最具体扫描根继承对应 `ScanRootPlan`。归属规则沿用当前“组件数最多、同深度取规范根最小值”的确定性规则。任何文件不属于已规划根都返回 `InvalidResult`。

归属后的运行时记录只引用共享磁盘计划，不为每个文件复制完整 Windows 存储信息。

## 5. 数据流

```text
规范化、去重扫描根
        │
        ▼
枚举前解析 ScanRootPlan
        │
        ▼
Everything / Walker 获取文件列表
        │
        ▼
全局路径去重 + 最具体根归属
        │
        ▼
按物理盘权重形成 Path Cache 供给顺序
        │
        ├─ 完整缓存命中 ───────────────► 持久化终态
        │
        ▼
Hash Ready Queue（每物理盘一条 lane）
        │
        ▼
WeightedDiskDispatcher 选择 item_id
        │
        ▼
SQLite 按指定 item_id 原子 claim
        │
        ▼
DiskReadScheduler Hash permit + MD5
        │
        ├─ 内容缓存命中 ───────────────► 持久化终态
        │
        ▼
Media Ready Queue（继承同一 disk_key）
        │
        ▼
Worker/decode credit + 按盘选择 + Media permit
        │
        ▼
Worker 计算 ──────────────────────────► 持久化终态
```

按根交错仍保留为让不同根尽早进入 path-cache 批次的入口，但真正的 Hash 和 Media 分发权威是按物理盘的动态 Ready Queue。

## 6. WeightedDiskDispatcher

### 6.1 Lane 状态

每个唯一 `disk_key` 保存一条 lane：

```text
DiskLane<T>
  disk_key
  disk_kind
  quantum                  配置中的逐盘数值
  deficit                  跨分发调用保留的亏欠额度
  active                   已选择且尚未释放的读取席位
  bypass_count             可运行但被其他 lane 绕过的次数
  ready_hash               稳定 FIFO
  ready_media              稳定 FIFO
```

同盘根合并后，lane 内按规范路径和原持久化身份保持稳定顺序。HDD 不做随机洗牌，避免主动放大寻道；SSD 同样使用稳定 FIFO，保证可重复测试。

### 6.2 两级选择

调度分为两级：

1. 按物理盘 lane 使用加权亏欠轮转；
2. 选中 lane 后，在 Hash/Media 子队列之间沿用现有媒体与 Hash 类别权重及老化规则。

不新增另一套冲突的 Hash/Media 常量。实现时把现有纯选择规则复用或提取为共享策略，最终 `DiskReadScheduler` 仍会执行真实许可裁决。

### 6.3 加权亏欠轮转

- lane 第一次进入轮次时，`deficit += quantum`；
- 每分发一个单位成本任务，`deficit -= 1`；
- lane 的额度耗尽、队列为空或触及逐盘硬上限后，游标移到下一 lane；
- 空 lane 不累计历史额度，避免重新出现任务时突发占满全局席位；
- 游标和未用完 deficit 跨调用保留，因此全局额度小于权重总和时，多个窗口累计仍保持比例；
- 任务单位成本固定为 1，不按文件大小猜测服务时间。

例如一个 SSD、一个 HDD，配置权重 5 和 1：

- `total_threads=6` 时，一个完整窗口目标为 5:1；
- `total_threads=3` 时，前一个窗口可以是 3:0，后续窗口继续使用未完成轮次，累计六次选择为 5:1；
- SSD lane 为空时，HDD 仍不能突破自己的逐盘硬上限 1。

### 6.4 全局与逐盘限制

一个候选只有同时满足以下条件才可选择：

- 全局 active 小于 `read.total_threads`；
- 候选涉及的每个底层物理盘 active 都小于对应逐盘上限；
- Hash 阶段仍有 hash slot、output credit 和 refill token；
- Media 阶段仍有 Worker 槽位、decode credit 和输出 ownership；
- 任务和任务项仍处于允许领取的状态。

调度选择前先验证非磁盘资源，避免取得磁盘席位后等待 Worker 或 credit。

### 6.5 老化保护

沿用当前 `MAX_CONFLICTING_BYPASSES=8` 的有界绕过语义：

- lane 在自身可运行、但更年轻 lane 获得冲突席位时增加 bypass；
- 达到阈值后形成唯一老化保留；
- 老化 lane 可运行时优先取得下一可用冲突席位；
- 老化 lane 因自身逐盘硬上限阻塞时，不冻结与它不相交的其他物理盘；
- 任务取消、队首变化或成功分发后清除相应保留。

这保证低权重 HDD 不会因全局额度长期被 SSD 占用而无限等待。

## 7. 调度所有权与真实读取许可

### 7.1 DiskAdmissionLease

WeightedDiskDispatcher 选中任务时创建 `DiskAdmissionLease`，原子增加全局和全部底层盘 active。其生命周期覆盖：

```text
选择任务
  → 按 item_id claim
  → 等待真实 DiskReadScheduler permit
  → 读取源文件
  → 源读取完成或任一错误/取消
```

claim 失败、permit 失败、任务取消和 future Drop 都必须通过 RAII 恰好释放一次。真实 permit 成功后，admission lease 与 `ScheduledReadPermit` 绑定，并在源读取完成时一起释放。

磁盘席位不等待 Worker 后续 CPU 特征计算结束。源读取一完成，下一 Ready 项即可补位，使磁盘读取和 CPU 计算保持流水重叠。

### 7.2 最终硬保护

`DiskReadScheduler` 继续校验并执行：

- 全局读取上限；
- 每底层物理盘上限；
- Hash/Media 类别席位；
- 复合卷原子占用；
- FIFO、轮转和老化；
- permit Drop 守恒。

任务分发层不能绕过真实 permit。两层计数不一致时立即返回基础设施错误并记录磁盘身份、任务项和全部计数，不允许继续扩大漂移。

## 8. SQLite 与恢复

### 8.1 按指定任务项领取

NodeStore 增加内部接口：

```text
claim_item(task_id, item_id, expected_stage, now_ms)
```

事务内仅在以下条件全部成立时把项目更新为 running：

- 所属任务仍为 queued 或 running；
- item 属于指定 task；
- item 状态为 queued；
- stage 与期望阶段一致。

返回值区分“成功领取”“项目已被终结或取消”“身份/阶段不一致”。不使用先查询、后更新的非原子组合。

现有 `claim_next_item` 保留给其他任务类型，不把物理盘调度语义扩散到所有 NodeStore 调用方。

### 8.2 Ready Queue 发布

path cache 决定需要 MD5 后，必须先成功把 SQLite 项转为 `queued/read_md5`，再把 `item_id` 发布到 Hash Ready Queue。SQLite 写入失败时不得出现仅存在于内存的候选。

Hash 完成且内容缓存仍缺失时，Media Ready 记录继承已冻结的 `disk_key`，并继续持有现有 output/decode ownership；缓存命中项不进入 Media Queue。

### 8.3 任务恢复

Node 重启或任务恢复时：

1. 沿用现有规则把可恢复 running 项恢复为 queued；
2. 从 `task_scan_roots` 读取扫描根；
3. 使用本次启动的有效配置重新生成 `ScanRootPlan`；
4. 查询该任务的 queued 项及显示路径；
5. 按最具体根重新归入磁盘 lane；
6. 恢复稳定 FIFO 和加权游标初始状态；
7. 继续使用 `claim_item` 原子领取。

Ready Queue 不持久化 deficit、游标和 bypass。重启后这些瞬时公平状态归零，但任务项、结果和失败状态不会丢失或重复。配置变更在 Node 重启后对恢复任务生效。

本方案不增加 SQLite 列或迁移版本。

## 9. 取消与失败处理

- 任务取消后禁止新选择、claim 和 Worker dispatch；所有等待 future 通过 Drop 释放 admission 与 permit。
- 单文件 Hash/媒体许可失败只终结对应任务项，随后立即继续其他 lane。
- Worker 崩溃发生在源读取完成后时，不再持有磁盘席位；当前文件按既有规则失败或跳过，其他文件继续。
- Worker 崩溃发生在媒体源读取完成前时，Worker 终态和 permit/lease 清理必须分别守恒，不能依赖对方先到达。
- 复合卷候选无法同时取得全部底层盘席位时保持 queued，不允许部分计数或部分 claim。
- 根计划解析失败属于 EnumerateFiles 前置失败；错误必须包含显示根和稳定错误码 `SCAN_ROOT_STORAGE_RESOLVE_FAILED`。
- 介质类型为 Unknown 不是错误，按 Unknown 配置正常调度。

## 10. 遥测

保留现有逐盘 waiting、active、granted 和 released 字段，并追加任务分发层指标：

- `configured_weight`；
- `configured_limit`；
- `hash_ready_current/peak`；
- `media_ready_current/peak`；
- `dispatch_selected_total`；
- `global_limit_wait_total`；
- `disk_limit_wait_total`；
- `worker_slot_wait_total`；
- `bypass_total`；
- `aged_forced_total`。

报告必须区分：

1. Ready Queue 没有该盘任务；
2. 有任务但等待全局额度；
3. 有任务但等待逐盘额度；
4. 已选中但等待真实 permit；
5. 正在读取；
6. 源读取完成、Worker 继续 CPU 计算。

权重验收只统计至少两个物理盘 Ready Queue 同时非空且候选可运行的窗口。某盘任务已经耗尽后的比例变化不能判为调度失败。实际字节吞吐受文件大小和设备性能影响，不作为任务权重相等的替代证据。

## 11. 测试设计

### 11.1 根计划

- 断言根计划在枚举器首次调用之前完成；
- 同一物理盘的两个盘符合并为一条 lane；
- 不同物理盘分别建 lane；
- SSD/HDD/Unknown 正确选择配置值；
- 物理盘编号失败在枚举前返回稳定错误；
- 重复根、重叠根和最具体根归属保持确定；
- 跨卷重解析点不被隐式遍历；
- 复合卷保存排序去重后的全部底层盘。

### 11.2 纯调度器行为

| 场景 | 必须证明 |
|---|---|
| SSD=5、HDD=1、全局6 | 首个完整选择窗口为5:1 |
| SSD=5、HDD=1、全局3 | 六次累计为5:1，游标跨窗口保持 |
| 两SSD、两HDD | 每块盘独立应用自身权重 |
| 同盘两个根 | 合并为一份权重和 active 上限 |
| 一条 lane 为空 | 其他 lane 工作保持推进，但不突破逐盘上限 |
| 低权重 lane 持续有任务 | 可运行时绕过次数不超过老化阈值 |
| 复合卷 | 全部底层盘原子增加和释放 |
| 取消/错误/Drop | 全局及逐盘 active 最终归零 |
| Hash 与 Media 同时等待 | 沿用既有类别权重且两类均不饥饿 |

### 11.3 BaseCompute 集成

- path cache 命中比例严重不同时，真正进入 Hash/Media 的剩余项仍按动态权重选择；
- 首批 queued 项即使按 `item_id` 集中在一块盘，也能通过 `claim_item` 从另一盘选择；
- HDD=1 时，不允许多个同 HDD 媒体许可 future 占满全部 Worker admission；
- SSD=5/HDD=1/Worker>=6 时，两盘首批真实 Worker 路径为5:1；
- Worker slot、output credit、decode credit、persist ownership 和磁盘 admission 均不越界；
- 单文件读取失败或 Worker 崩溃后，下一盘任务继续分发。

### 11.4 NodeStore 与恢复

- `claim_item` 仅成功一次，错误 task、stage 或状态均拒绝；
- 取消与 claim 竞争只有一个事务结果生效；
- 崩溃恢复后 queued 项全部重新进入正确磁盘 lane；
- 任务项集合、最终统计和事件序号与调度前一致；
- 恢复不依赖新增 SQLite 列。

### 11.5 遥测与真实验收

- configured、ready、wait、selected、bypass、aged 和现有 permit 字段协议往返兼容；
- 全部计数满足 released 不超过 granted、active 不超过配置、终态归零；
- 报告只在双盘同时 Ready 的有效窗口计算权重比例；
- 真实媒体验收分别报告任务分发比例、实际读吞吐、队列、CPU 和 Worker phase，不把请求公平误写为字节吞吐相等。

## 12. 验收标准

1. 所有根都在文件列表获取前完成物理盘编号和类型解析。
2. 同一物理盘上的多个根不会重复获得配置权重。
3. 当一个 SSD 和一个 HDD 均持续 Ready，且配置为 5 和 1、全局为 6 时，首个完整窗口分发 5 个 SSD 项和 1 个 HDD 项。
4. 全局额度不足时，累计选择误差不超过一个单位任务，且任何持续可运行 lane 的绕过不超过老化阈值。
5. 全局、逐盘、Worker、Hash、Media、decode 和 persist ownership 全部通过守恒测试。
6. 缓存命中、单文件失败、取消和 Worker 崩溃不会造成其他物理盘停止推进。
7. Node 重启恢复后无任务丢失、重复 claim 或最终统计漂移。
8. 现有 DiskReadScheduler、BaseCompute、NodeStore、协议、运行时报告和正式包定向回归全部通过。
9. 真实媒体文件前后清单不变；未获部署授权前不替换 `I:\Tool`。

## 13. 备选方案与取舍

### 13.1 只修改按根输入顺序

拒绝作为最终方案。它改动最小，但多个根可能属于同一物理盘，缓存命中和异步完成也会使静态比例失真。

### 13.2 动态每物理盘 Ready Queue

采用。它在真正需要 Hash/媒体计算的边界持续按盘调度，能够使用配置权重、处理全局额度不足、合并同盘根并支持恢复。

### 13.3 把完整任务状态并入 DiskReadScheduler actor

拒绝。虽然可以形成单一调度权威，但会让读取许可 actor 耦合 SQLite、缓存、Worker、持久化和任务恢复，扩大故障面。

### 13.4 逐文件重新解析物理盘

拒绝作为主路径。用户要求在获取文件列表前确定物理盘，逐文件 Windows 存储查询会增加大量重复开销。跨卷路径通过独立扫描根表达。

## 14. 组件边界

预计实施涉及以下边界，精确文件由实施计划固定：

- Node actor：枚举前建立并冻结 `ScanRootPlan`；
- scan input order：从按根 1:1 扩展为按物理盘计划生成候选供给；
- BaseCompute：维护 Hash/Media 每盘 Ready Queue 和 admission lease；
- ScheduledFileReader：使用冻结根计划申请真实读取许可，不再逐文件发现磁盘身份；
- NodeStore：增加按指定 item 原子 claim 与恢复所需 queued 项读取接口；
- runtime/protocol/report：投影分发层逐盘指标；
- tests：覆盖根计划、纯调度、BaseCompute、恢复、守恒和报告语义。

所有新增类型、方法、字段和关键变量都必须使用中文注释说明用途、所有权、失败行为和恢复边界。
