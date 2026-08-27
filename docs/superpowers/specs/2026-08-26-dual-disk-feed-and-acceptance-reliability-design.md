# Rust V2 双盘任务供给与验收可靠性修复设计

日期：2026-08-26

## 1. 决策摘要

本设计处理 2026-08-26 双物理盘单轮真实媒体测试暴露出的三个独立问题：

1. **任务供给串行化。** 完整清单按 `normalized_path` 排序后直接进入 BaseCompute，H 根全部排在 I 根之前；I 盘请求在 H 前缀耗尽前无法进入 `DiskReadScheduler`。
2. **系统采样器退出竞态。** `Get-Process` 返回后 Worker 退出，`TotalProcessorTime` 变为 `null`，StrictMode 下读取 `TotalMilliseconds` 使整轮验收中止。
3. **结果导出器首次打开自变更误判。** 导出器在打开 SQLite 前冻结 WAL/SHM，首次只读打开可能初始化或刷新 SHM，随后把自身引起的变化误判成外部写入。

采用以下修复：

- 保留枚举结果的全局排序和去重契约；在去重完成后、BaseCompute 创建/领取任务项之前，按扫描根建立有界逻辑队列并做确定性 round-robin 合并。
- 保留现有 `DiskReadScheduler` 的物理盘识别、全局/每盘许可、类别 seat、复合盘原子许可和老化保护。根轮转只解决“请求可见性”，不替代盘级调度。
- 采样器先冻结一份进程属性快照；单个进程在快照期间退出时记录稳定 skip 事件、清理该 PID 世代基线并继续采集其他进程、逻辑核和物理盘。
- 结果导出器先打开 SQLite，再冻结主库/WAL/SHM 内容；查询结束后仍严格复验，从而容忍首次打开的自变更但继续拒绝真正的外部变化。
- 新增逐物理盘运行时许可遥测，使后续报告能区分“请求尚未出现”“等待许可”“持有许可”，不再只用 OS 磁盘流量反推调度状态。

本设计不以两盘持续 100% 占用为目标。正确目标是：当两个根都有待处理文件且全局/每盘许可允许时，两盘请求能够同时可见并推进；CPU、缓存命中、媒体类型和单文件耗时仍会造成合理波动。

## 2. 事实基线

事实来源：[Rust V2 双物理盘单轮真实媒体诊断报告](../../verification/2026-08-26-dual-physical-disk-single-run.md)。

- 媒体根：`H:\pik\00000000000`（24,232 项）与 `I:\tmp`（14,786 项）。
- H 为 PhysicalDisk1，I 为 PhysicalDisk2。
- Worker=20、全局读取许可=12。
- H Worker 最后出现于 elapsed=1,003 秒，I Worker 首次出现于 elapsed=1,004 秒；Worker 路径交叠为 0 秒。
- 两盘 OS 读取同时出现仅约 3.18 秒，占系统采样窗口约 0.20%。
- 运行约 1,628 秒时仍为 `running`，系统采样器因 `TotalMilliseconds` 属性错误退出；无 `runtime_result` 和结果摘要。
- 媒体前后清单 SHA-256 完全一致，运行后无产品残留进程。

源码边界：

1. `crates/windows/src/walker.rs` 逐根完整遍历。
2. `crates/node-engine/src/scan/enumerator.rs` 和 `everything.rs` 生成全局稳定排序、去重的 `Vec<ScannedPath>`。
3. `crates/node-engine/src/actor.rs` 在完整枚举后调用 `BaseComputeEngine::run_existing`。
4. `crates/node-engine/src/scan/base_compute.rs` 从 `remaining_rows: VecDeque<ScannedPath>` 每批 `pop_front()`。
5. `crates/node-store/src/tasks.rs` 按任务项身份领取 queued 项。
6. `crates/node-engine/src/scan/pipeline.rs` 领取文件后才解析存储位置并申请读取许可。
7. `crates/node-engine/src/io/scheduler.rs` 只能在请求已经入队后执行物理盘 FIFO、盘间轮转和公平授予。

因此，`DiskReadScheduler` 不是本次串行现象的首要缺陷；上游没有同时提供 H/I 请求才是直接架构原因。

## 3. 范围

### 3.1 本轮包含

- 多扫描根的确定性、去重后 round-robin 执行顺序；
- 单根、空清单、大小不均衡、重叠根、重复根和 UNC 根行为；
- BaseCompute 首批任务可同时包含不同根；
- Worker 退出期间系统采样不中止；
- SQLite 首次只读打开不被错误认定为外部 sidecar 修改；
- 逐物理盘 Hash/Media 等待数、活动许可、累计授予和累计释放遥测；
- 协议增量兼容、验收 NDJSON 和报告投影；
- 确定性行为测试、固定合成基准、正式包结构校验和最终代码审查。

### 3.2 本轮明确排除

- SSD/HDD/Unknown 识别逻辑和默认参数；
- 增加 Worker 数、全局读取许可或每盘许可；
- Node 代替 Worker 读取媒体、跨进程数据块服务或共享内存读取代理；
- FFmpeg 解码算法、线程策略和硬件解码；
- Worker 原生崩溃、协议帧截断和 ACK 身份缺陷的根因修复；
- 修改媒体文件、清理既有运行证据或触碰 `I:\Tool`；
- 再次执行 H/I 全量真实媒体测试；如实现后确需复测，必须由用户另行授权一次新的独立运行。

Worker 崩溃/ACK 风险继续作为独立 `NON_DEPLOYABLE` 门禁。本设计只保证采样器能够容忍 Worker 退出，不把崩溃本身伪装成已修复。

## 4. 目标数据流

```text
Windows Walker / Everything
          │
          ▼
全量 normalized_path 排序 + 跨根去重
          │
          ▼
RootFairInputOrder
  ├─ Root H: H1 H2 H3 后续项
  ├─ Root I: I1 I2 I3 后续项
  └─ 输出: H1 I1 H2 I2 H3 I3 后续轮转项
          │
          ▼
BaseCompute 有界 path batch / credit / refill
          │
          ▼
SQLite queued item / Hash admission
          │
          ▼
按真实文件位置解析 PhysicalDiskId
          │
          ▼
DiskReadScheduler
  ├─ 全局许可
  ├─ 每底层物理盘许可
  ├─ Hash / Media active seat
  ├─ 盘间 round-robin
  └─ 复合盘原子授予与老化保护
```

执行顺序可以变化，业务结果顺序不能变化。结果摘要、同步和正确性比较继续按 `normalized_path` 排序。

## 5. 多根输入顺序

### 5.1 根归属

新增私有 `RootFairInputOrder`：

1. 将 `ScanOptions.roots` 转换成 `NormalizedPath`。
2. 对重复规范根去重，并按规范根字典序建立稳定 bucket 顺序。
3. 每个已经全局去重的 `ScannedPath` 归入一个根：选择能够包含该路径的**组件数最多**的根；这使重叠根的归属确定且不会重复执行。
4. 根组件数相同则选择规范根字典序最小者。
5. 任一行不属于任何输入根时返回 `ScanError::InvalidResult`，不得静默落入兜底队列。

### 5.2 round-robin

- 每个 bucket 内保持原 `normalized_path` 顺序。
- 每轮从每个非空 bucket 取一项；bucket 空后从轮转集合删除。
- 单根输入保持原顺序；空输入保持为空。
- 输出长度、总字节数和路径集合必须与输入完全相同。
- 不在此层解析每个文件的物理盘，不增加文件系统 I/O。

该算法按文件数公平，不按字节数公平。不同文件大小的吞吐平滑交给现有有界流水线和物理盘调度器；本轮不引入未验证的大小估算权重。

### 5.3 与批次和恢复的关系

- round-robin 在 `BaseComputeEngine::run_existing` 之前一次完成；因此首个 path-cache 批次即可包含多个根。
- `reserve_scan_path`、缓存命中、单项失败和任务恢复语义不变。
- 已持久化的旧 queued/running 项仍按现有恢复规则先处理；新 reserve 的项目继续使用轮转后的顺序。
- 不修改 SQLite schema，不依赖媒体文件在两次打开之间做版本一致性验证。

## 6. 采样器退出竞态

系统采样必须把单个进程视为可消失的观测对象，而不是整轮运行的强依赖。

### 6.1 快照规则

对一个 PID：

1. 先读取 `Id`、`ProcessName`、启动时间、`TotalProcessorTime`、`WorkingSet64`、`PrivateMemorySize64` 到局部不可变快照。
2. 任一必要属性为 `null`，或 getter 抛出“进程已退出/对象不可用”类异常时，返回稳定 skip：`PROCESS_EXITED_DURING_SAMPLE`。
3. 只有快照完整后才更新 `PreviousCpu` / `PreviousIo`。
4. skip 时删除该 PID 的所有进程世代基线，防止 PID 复用继承旧累计值。
5. 继续采样其余进程、逻辑核和物理盘，并正常追加本 tick 的 `system_sample`。

采样记录追加 `process_sample_skips`，至少包含 `process_id` 和稳定 `reason`。报告把 skip 计数列为诊断信息；少量 Worker 退出 skip 不使运行 INCONCLUSIVE，整个采样文件缺失或最大采样间隔超限仍按原门禁处理。

## 7. 结果导出 sidecar 时序

目标顺序固定为：

```text
validate_arguments
  → canonical_cache_root
  → open_read_only_database
  → capture_sidecars
  → load task/items/features
  → close connection
  → verify_sidecars
  → 原子提交 canonical/metadata/lease
```

含义：

- SQLite 首次打开自身造成的 SHM 初始化发生在快照前，不再误报。
- 快照之后主库、WAL 或 SHM 内容发生变化仍返回 `InvalidArgument`。
- 不使用 `immutable=1`，因为它可能忽略 WAL 中尚未 checkpoint 的有效数据。
- 不以 mtime 单独判断变化；沿用内容身份、长度和 SHA-256 校验。

## 8. 逐物理盘遥测

新增 `RuntimeDiskReadMetrics`，由读取许可的真实生命周期更新：

- `physical_disk_id`；
- `capacity`；
- `hash_waiting` / `media_waiting`；
- `hash_active` / `media_active`；
- `hash_granted_total` / `media_granted_total`；
- `hash_released_total` / `media_released_total`。

复合位置的请求对每个底层 `PhysicalDiskId` 各计一次 waiting/active，许可仍只占一个全局 seat。waiting guard、active guard 和真实 `DiskReadPermit` 使用同一 RAII 生命周期，取消、错误、接收端关闭和正常 Drop 都必须恰好归零一次。

协议只在 `RuntimePipelineMetrics` 追加 `repeated RuntimeDiskReadMetrics disk_reads = 28`。旧 Node 缺字段时客户端输出空数组/`null`，不得从全局 `hash_io`、`media_io` 或 Worker 路径反推逐盘许可。

## 9. 不变量

1. 枚举完成后的路径集合、总文件数和总字节数不变。
2. 去重先于轮转；一条规范路径最多执行一次。
3. `DiskReadScheduler` 的全局、每盘和复合盘硬上限不变。
4. 轮转不能绕过缓存、output credit、decode credit、Worker admission 或持久化 ACK。
5. 单个进程采样失败不能终止运行；采样器自身无法继续写证据时必须显式 INCONCLUSIVE。
6. 导出器只容忍打开前的自身 sidecar 变化；打开并冻结之后的变化仍拒绝。
7. 新协议字段只追加，不改变既有 tag、Envelope 或持久 schema。
8. 最终任务正确性仍由终态、MD5、媒体类型、全部特征 payload、缩略图和联系表规范化摘要决定。

## 10. 验证与发布裁决

实施阶段只允许以下验证：

- PowerShell 受控进程退出 fixture；
- SQLite WAL/SHM 受控时序 fixture；
- Rust 两根/三根/重叠根/不均衡根行为测试；
- BaseCompute 受控 reader + 两个虚拟物理盘集成测试；
- 现有 DiskReadScheduler、BaseCompute、runtime/protocol、Windows harness/report 回归；
- 固定四文件合成 benchmark 三轮，仅防止明显回退；
- 正式 ZIP 结构和哈希校验。

本计划不再运行 H/I 真实媒体。代码和包即使通过上述门禁，仍因 Worker 崩溃/ACK 风险保持 `NON_DEPLOYABLE`；生产替换必须另行修复该门禁并取得用户授权。

## 11. 备选方案及取舍

- **只增加 Worker/读取许可：拒绝。** I 请求不可见时只会扩大 H 前缀并发。
- **只修改 DiskReadScheduler：拒绝。** 调度器不能选择尚未入队的请求。
- **Node 统一代读并向 Worker 传块：拒绝。** 会引入跨进程复制、FFmpeg 随机访问和缓存生命周期复杂度。
- **枚举阶段逐文件解析物理盘再排序：暂不采用。** 能更精确，但会给 39,018 项增加额外 Windows 存储查询；本次两个根已明确位于不同物理盘，根轮转能用最小改动恢复请求可见性。
- **去重后按根轮转：采用。** 不增加媒体 I/O，不改存储 schema，并让现有盘级调度器获得可裁决的多盘请求。
