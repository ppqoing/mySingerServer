# Rust V2 CPU / 磁盘 I/O 在途容量平滑设计（方案 B）

## 1. 决策摘要

本设计是 `2026-08-23-cpu-io-staged-pipeline-architecture.md` 的后续收敛方案，只处理真实媒体生产阶段出现的 CPU / 磁盘 I/O 反向相位，不重做已经完成的基础计算流水线。

核心决策：

1. 保留 Worker 直接读取媒体文件，不引入 Node 代读、跨进程数据块传输或新的读取线程池。
2. 将 Hash 与媒体读取的公平性从“历史许可发放次数”改为“当前实际在途许可占用”。
3. 两类读取都有需求时，为媒体读取保留与 Worker 数匹配的名义容量，同时维持少量 Hash 后台供给；任一类别没有需求时，另一类别可以借用全部空闲容量。
4. 已取得的读取许可不抢占，只约束后续许可授予。
5. 用下游信用控制 Hash 补位和解码 ownership，禁止下游一腾出空间就整批补满 Hash。
6. 先补齐可归因遥测，再以相同只读媒体集执行旧版/候选版 A/B；曲线更平滑不能代替总吞吐门禁。

本设计不以 CPU 或磁盘持续 100% 为目标。目标是在不降低整体吞吐的前提下，让 Hash 供给、媒体读取和 Worker 计算尽量持续重叠。

## 2. 文档关系与事实基线

### 2.1 上游文档

- 原架构计划：`docs/superpowers/plans/2026-08-23-cpu-io-staged-pipeline-architecture.md`
- 真实媒体诊断：`docs/verification/2026-08-24-real-media-cpu-io-diagnostic.md`
- 原计划最终门禁：`docs/verification/2026-08-24-cpu-io-pipeline-final-gate.md`
- 原计划执行账本：`.superpowers/sdd/2026-08-23-cpu-io-staged-pipeline-architecture/progress.md`

本设计覆盖原计划中 `MediaDecode:HashSequential = 3:1` 的“按授予次数”实现，以及 `fill_hash_tasks` 的一次性补满行为；其他已完成契约继续有效。

### 2.2 已确认运行事实

30 分钟真实媒体只读跑测中：

| 生产区间 | Worker CPU | 活动 Worker | 空闲 Worker | Hash I/O 许可 | 媒体 I/O 许可 | 解码队列 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| I: `(0,100) MiB/s` | 11.51 核 | 11.71 | 0.29 | 0.49 | 7.01 | 59.24 |
| I: `>=300 MiB/s` | 7.62 核 | 7.88 | 4.12 | 9.11 | 5.89 | 25.39 |

在 I: 非零读取样本中，吞吐与 Worker CPU 的时间加权相关系数为 `-0.570`，与空闲 Worker 为 `+0.528`。三轮扫描都观察到约 33%～36% 的高 I/O Worker CPU 降幅。

源码与运行指标共同表明：

- `DiskReadScheduler` 当前按成功授予次数维护 `3:1`，没有记录 Hash/Media 各自的活动许可数；
- 完整文件 Hash 的许可持有时间通常长于单次媒体源读取，授予次数比例不等于实际占用比例；
- `fill_hash_tasks` 每轮会把 `JoinSet` 直接补到 `hash_capacity`；
- `JoinSet::len()` 同时包含等待许可、正在读取和已完成但尚未归并的任务；
- `media_acquiring` 是 Node admission 预占，不是 WorkerPool 的真实 Worker slot；直接从 `active + media_acquiring <= worker_capacity` 中删除它会制造过量待派发许可，不能作为修复；
- Worker 槽位累计 CPU 分布均衡，没有少数 Worker 长期垄断任务的证据。

## 3. 设计范围

### 3.1 本轮包含

- Hash/Media 全局及每物理盘活动许可计数；
- 基于名义 seat 的公平选择、空闲借用、自然收回和老化保护；
- Hash 下游信用与渐进补位；
- 解码等待 ownership 的独立硬上限；
- Hash、媒体许可和 Worker phase 的精细状态遥测；
- Protobuf 增量兼容、Desktop 运行详情和验收 NDJSON 投影；
- 确定性行为测试、受控基准和真实媒体 A/B；
- 隔离测试便携包构建、包哈希、禁止部署条件和整包回滚要求。

### 3.2 本轮明确排除

- SSD/HDD/Unknown 识别与探测修复；
- 增加读取总许可、增加 Worker 数或 CPU 绑核；
- Node 代替 Worker 读取媒体数据、共享内存 Read Broker 或块级跨进程协议；
- FFmpeg session 复用、解码器内部线程策略、硬件解码；
- 拆分 Worker 内的 decode lane 与 feature lane；
- Worker 崩溃、媒体解码失败和崩溃文件路径修复；
- 约 120 秒任务最终化尾段；
- SQLite/PostgreSQL schema 修改；
- 生产目录替换、正式发布和部署执行；本轮只生成用于隔离 A/B 的测试便携包。

上述排除项可以独立立项，但不得混入本设计的 A/B 结论。

## 4. 约束与不变量

1. 媒体文件在扫描期间只读且不会被修改、替换或继续写入。
2. 全局读取硬上限和每物理盘硬上限沿用现有 `DiskReadConfig`，两类读取共享同一硬预算。
3. `active_total` 永远不得超过全局上限；任一底层盘 `active` 永远不得超过该盘上限。
4. 已授予许可不可抢占；取消、错误和正常完成均通过同一个 RAII Drop 边界归还计数。
5. 一类没有可授予请求时，另一类必须能够借用全部可用容量；除 6.2 定义的单个老化复合请求保留外，调度器保持 work-conserving。
6. 两类持续有需求时，两类都必须在有界等待内推进。
7. `active + media_acquiring + worker_dispatching <= worker_capacity` 继续成立；等待媒体许可或等待派发确认不得被解释成已启动 Worker。
8. 不增加持久任务项数量，不修改 `task_id + item_id + content_id` 的归并身份。
9. 新遥测字段只能追加 Protobuf tag，禁止重编号；旧节点缺字段显示 `null` 或 `—`，禁止从聚合字段猜测。
10. 控制器不依赖 Windows 磁盘延迟采样；延迟缺失不会导致任务失败或退化为错误状态。

## 5. 目标数据流

```text
SQLite 待处理项
      │ 下游信用
      ▼
Hash admission ──申请 Hash seat──┐
      │                           │
      ▼                           ▼
完整 MD5 ──释放 Hash seat──> DiskReadScheduler
      │                           ▲
      ▼                           │
缓存解析                         │
      │                           │
      ▼                           │
有界 decode ownership            │
      │                           │
      └──申请 Media seat─────────┘
                  │
                  ▼
             Worker decode
                  │ SourceReadComplete：释放 Media seat
                  ▼
             Worker feature
                  │ terminal：释放 Worker/CPU
                  ▼
               持久化
```

Hash、媒体读取和 Worker 计算仍然是跨文件并行；单文件仍保持 `Hash → cache → decode → feature → persist` 的正确性顺序。

## 6. 活动 seat 调度策略

### 6.1 名义容量

设：

- `T`：当前约束资源的读取许可总数；
- `W`：任务启动时从 `worker_process_ids().len()` 冻结的实际 Worker 容量；
- `M`：媒体名义 seat；
- `H`：Hash 名义 seat。

当 `T >= 2` 时，首个候选策略为：

```text
M = min(W, T - 1, floor(3T / 4))
H = T - M
```

当前生产候选 `T=16、W=12`，得到 `M=12、H=4`。

该公式把原来的 `Media:Hash = 3:1` 意图转换成实际在途容量，并保证至少保留一个 Hash seat。它是 A/B 候选策略，只有通过本设计门禁后才能成为生产默认值。

对每物理盘使用该盘现有硬上限计算本地名义容量。若某盘 `T=1`，无法同时保留两类 seat，继续使用带老化保护的下一次授予轮换。

### 6.2 选择规则

调度器按以下顺序选择下一项：

1. 清理已经取消的队首请求；取消当前老化保留请求时同步清除保留。
2. 全局最多维护一个 `aged_reservation`。某请求达到现有 `MAX_CONFLICTING_BYPASSES=8` 老化界限后，选择其中最老者作为保留；若它暂时不能原子授予，只冻结会占用其任一相同底层盘或最后一个全局 seat 的年轻请求，不相交且不争抢最后一个全局 seat 的请求继续推进。保留在授予或取消时清除。
3. `aged_reservation` 当前可原子授予时立即优先授予。
4. 在不违反老化保留的候选中，计算 Hash 与 Media 当前是否各有可授予请求。
5. 只有一类可授予时，立即授予该类，不因名义容量保留而闲置许可。
6. 两类都可授予时，分别计算候选请求授予后的压力分数：`max(全局 active_class / nominal_class, 请求涉及且 T>=2 的各底层盘 active_class / nominal_class)`；选择分数较低的一类。
7. 比例相同时优先满足更老请求；年龄相同才使用既有物理盘 round-robin 顺序。
8. 复合盘请求只有在所有底层盘都能原子占用时才能授予，不允许部分预留。

所有比值使用整数交叉相乘比较，不依赖浮点舍入。全局容量与每盘容量同时参与判断。跨盘请求争抢最后一个全局 seat 时也必须应用类别份额，不能只在路径物理盘相交时才执行类别公平。

容量为 `T=1` 的约束资源没有名义比例分母，不参与压力分数；它沿用确定性的下一次授予轮换 `Media × 3 → Hash × 1`，并受同一老化界限保护。复合请求涉及多个 `T=1` 资源且各资源当前偏好冲突时，不等待偏好对齐，而是在所有当前可原子授予的冲突候选中选择最老请求，并用实际授予类别同步更新所涉及资源的轮换状态。

`aged_reservation` 是 work-conserving 的唯一例外：它只冻结真正会阻碍该老化请求取得完整资源集的年轻请求，不能冻结与其资源集合不相交的工作。

### 6.3 借用与收回

- Media 没有等待请求时，Hash 可以使用全部空闲 seat；
- Hash 没有等待请求时，Media 可以使用全部空闲 seat；
- 被借用的 seat 不抢占，只在现有许可自然 Drop 后，根据当前需求授予目标类别；
- 名义份额是软目标，不是新的硬上限；超过份额的类别在另一类存在可授予请求时会因压力分数更高而自然降级，但若它仍是唯一可授予工作，必须继续取得空闲 seat；
- 老化保留优先于压力分数，避免长期持续压力下的永久饥饿。

### 6.4 生命周期计数

`DiskReadPermit` 必须冻结 `DiskReadClass`，在授予时同时递增：

- 全局 total active；
- 全局 Hash 或 Media active；
- 每个底层盘 total active；
- 每个底层盘 Hash 或 Media active。

Drop 时以相反顺序精确递减并唤醒调度器。取消队列请求不改变 active；许可交付失败必须立即归还已经预留的全部计数。

## 7. 下游信用与渐进补位

### 7.1 Hash 状态拆分

Hash task 必须处于且只处于以下一个状态：

1. `hash_waiting_permit`：已生成 future，尚未取得读取许可；
2. `hash_reading`：持有 Hash 许可，正在完整读取 MD5；
3. `hash_completed_unjoined`：Hash 和许可释放均已完成，结果尚未被协调器归并；
4. 已归并到 `pending_hashed` 或后续内容解析状态。

`hashing.len()` 只保留为内部 task 数量，不再作为“正在读盘”的展示或调度依据。

### 7.2 Hash 输出信用与补位令牌

- `content_output_credit` 容量等于现有 `queue_capacity`，不新增隐藏缓冲；
- Hash 开始前必须同时取得一个 Hash task slot 和一个 `content_output_credit`；
- credit 随同该文件在 `hash future → pending_hashed → content context/pending_compute` 间转移；
- 文件离开内容供给阶段、进入媒体许可获取或终态持久化后归还 credit；
- 没有下游 credit 时不得继续 claim SQLite 项，因此完成 Hash 永远拥有可归并位置；
- 取消、Hash 失败、缓存命中和普通成功都必须恰好归还一次 credit。

`content_output_credit` 只负责内存与结果位置安全，不承担补位节奏控制。另设容量为 `hash_capacity` 的 `hash_refill_token`：

- 任务启动时放入 `hash_capacity` 个令牌，用于渐进预热；
- 每次成功 claim 并启动一个 Hash 才消耗一个令牌；没有 task slot 或 output credit 时保留令牌，不空耗；
- 第一次注册 `MediaDecode` 请求时从预热态永久进入稳定态，先清空尚未使用的预热令牌，再为触发该转换的文件产生一个稳定态替代令牌；该令牌就是下一条规则中的唯一替代令牌，不重复计数；
- 无论是否已经进入稳定态，一个文件离开内容供给阶段进入媒体许可获取或单项终态时都恰好产生一个替代令牌；Hash 失败且整个任务仍继续属于单项终态，只产生这一个令牌，保证全缓存命中或全单项失败任务不会在首个 Hash 窗口后停住；
- 任务级取消只归还 output credit，不产生新令牌；
- 令牌数始终不超过 `hash_capacity`，成功、失败和取消路径都不得重复产生或重复消费；
- 只有在上游 item 生产者已永久关闭，并且一次持有令牌的 `claim_next_item` 权威返回无可领取项时，才能置位 `input_exhausted`、清空全部 available token；此后离开内容供给的在途文件不再产生令牌。若上游尚未关闭，暂时无 queued item 时保留令牌并等待上游事件，禁止误判结束或忙轮询。

第一版沿用现有 `channel_capacity` 数值，不扩大通道容量；新增的 `2W` 解码等待边界会在该通用上限之前产生背压。是否进一步缩小 `channel_capacity` 不属于本设计。

### 7.3 渐进 Hash 补位

- 初始化阶段由预置 `hash_refill_token` 在已有下游 credit 范围内渐进预热 Hash 窗口；第一次注册 `MediaDecode` 许可请求后永久进入稳定阶段，本任务内不再回到预热状态；
- 稳定运行后，一个文件离开内容供给阶段最多产生一个替代 Hash 令牌，output credit 的普通归还本身不触发补位；
- 每次外层主循环最多尝试消费一个令牌；只有成功 claim 并启动 Hash 才真正消费，随后必须重新经过一次 `select` 事件边界，禁止在同一轮通过 `while hashing.len() < hash_capacity` 将全部空位补满；
- `input_exhausted` 置位后不再尝试 claim；available token 归零只表示输入已经权威耗尽，不属于 credit 泄漏或任务失败；
- 禁止一次 `drain_ready_hash_results` 排空全部 ready Hash 后立即等量批量补位；
- 媒体等待且存在 Worker admission 缺口时，新 Hash 仍受活动 seat 策略约束；
- Worker 处于 feature 高位而媒体没有读取需求时，Hash 可以借用空闲磁盘 seat，保持后台供给。

该规则限制的是补位突发，不限制单类无竞争时的最终工作守恒。

### 7.4 解码 ownership

新增容量固定为 `2W` 的 `decode_credit`。本地缓存和远端内容两条会产生 `BaseComputeJob` 的路径，都必须先取得一个 credit；没有 credit 时保留原内容上下文并返回事件循环，不得先创建无界的 `pending_compute` 或许可 future。

credit 随单项依次转移：

```text
pending_compute
  -> media_acquiring
  -> worker_dispatching
  -> active(worker_slot=None)
  -> authoritative Worker Started：释放 decode_credit
```

媒体许可失败、Worker 派发失败、Started 前收到终态以及任务取消都必须释放 credit。派发 ACK 只把状态从 `worker_dispatching` 转成 `active(worker_slot=None)`，不能提前释放；只有 Node 归并权威 `Started` 事件才能释放。

保留两个独立边界：

1. `worker_admission_owned = active + media_acquiring + worker_dispatching <= W`；
2. `decode_waiting_owned = pending_compute + media_acquiring + worker_dispatching + active(worker_slot=None) <= 2W`。

当前 `W=12` 时，本候选实现的解码等待 ownership 固定上限为 `24`。该上限不包含已经归并 Started 的 Worker，但必须包含待派发内容、等待许可、许可结果 ready 尚未归并、派发中和 ACK 后尚未归并 Started 五种状态。若 A/B 未通过，不在本设计内临时改成其他倍数，而是保留原始证据后另立调参任务。

该边界替代当前由 `queue_capacity + worker_capacity` 推导出的宽泛解码展示容量；它不允许通过增加等待 future 来伪造 Worker 利用率。

## 8. 运行时遥测

### 8.1 新增 current/peak 字段

- `hash_waiting_permit`
- `hash_reading`
- `hash_completed_unjoined`
- `media_permit_waiting`
- `media_acquire_ready`
- `media_permit_ready`
- `worker_dispatching`
- `worker_start_pending`
- `worker_decode`
- `worker_feature`
- `worker_result_wait`
- `worker_phase_unknown`
- `content_output_credit_owned`
- `hash_refill_token_available`
- `decode_credit_owned`

同时保留现有：

- `hash_io`、`media_io`；
- `worker_slots`、`cpu_weight`；
- 聚合 Hash、decode、persist queue；
- 每阶段等待/服务延迟和吞吐。

### 8.2 状态守恒

必须能够验证：

```text
hashing.len()
  = hash_waiting_permit
  + hash_reading
  + hash_completed_unjoined

media_acquiring.len()
  = media_permit_waiting
  + media_acquire_ready

media_permit_ready
  <= media_acquire_ready

active.len()
  = worker_start_pending
  + worker_decode
  + worker_feature
  + worker_result_wait
  + worker_phase_unknown

active(worker_slot=None)
  = worker_start_pending

worker_admission_owned
  = active.len()
  + media_acquiring.len()
  + worker_dispatching

decode_credit_owned
  = pending_compute.len()
  + media_acquiring.len()
  + worker_dispatching
  + worker_start_pending

content_output_credit_owned <= queue_capacity
hash_refill_token_available <= hash_capacity
worker_admission_owned <= W
decode_credit_owned <= 2W
```

`media_acquire_ready` 包含成功许可、错误和 `None` 等所有“future 已完成但协调器尚未 join”的结果；`media_permit_ready` 只是其中确实持有许可的成功子集。`worker_dispatching` 表示 run 请求正在派发且尚未收到派发 ACK；`worker_start_pending` 表示 ACK 已收到但 Node 尚未归并权威 `Started`，此时 Worker 可能已经实际运行，不能把该字段解释为 WorkerPool 内部队列长度。

Worker phase 以现有实时事件为权威，`worker_decode`、`worker_feature`、`worker_result_wait` 和 `worker_phase_unknown` 必须互斥；Started 后尚未收到权威阶段事件，或活动项只有 idle/未知阶段时记录 unknown，不得猜测。Hash 不得伪装成 Worker phase。旧消息没有新字段时显示缺失，不从 `hash_queue` 或 `decode_queue` 反推。

### 8.3 采样边界

- 正确性计数在每次状态迁移时更新；
- Desktop/远程运行详情继续按现有低频合并发布；
- 真实 A/B 的任务内采样目标周期固定为 1,000 ms，系统采样目标周期固定为 2,000 ms；所有统计仍按真实 `sample_interval_ms` 加权；
- 生产相关性分析只包含基础计算 running 且 I/O 非零的生产窗口；约 120 秒最终化尾段必须排除。

## 9. 错误、取消与恢复

1. 调度策略不得增加新的持久任务状态。
2. 等待许可的取消请求从队列移除，不取得 seat。
3. 已取得许可的取消必须等待底层读取收束，再通过 RAII Drop 归还所有全局/每盘/分类计数。
4. Hash future、输出 credit、decode credit、媒体许可 future、worker dispatch 和 ActiveBase 必须在任务终态前全部归零；补位令牌不是 ownership，但必须在“上游关闭且无可领取项”边界清空 available 数量并置位 `input_exhausted`。
5. Worker 终态没有先收到 `BaseSourceReadComplete` 时，继续由现有 terminal 路径兜底释放媒体许可。
6. 外部系统采样缺失或旧协议没有新增诊断字段时，不得改变文件计算结果；内部所有权下溢、重复 Drop、身份错配或突破硬上限继续按不变量错误处理，并必须在测试中稳定暴露。
7. 任务恢复继续从持久化的未完成 item 重走 Hash，不保存或恢复瞬时 seat、future、credit 和内存队列。

## 10. 设计依据

- Tokio Semaphore 保证请求顺序公平，但不提供不同服务时间类别的活动容量份额：[Tokio Semaphore](https://docs.rs/tokio/latest/tokio/sync/struct.Semaphore.html)
- DRR 说明按请求个数轮询在请求成本不等时会失真；本设计只借鉴“成本不同不能只按次数公平”，不实现完整 DRR：[Deficit Round Robin](https://web.stanford.edu/class/ee384x/EE384X/papers/DRR.pdf)
- Kubernetes API Priority and Fairness 使用名义 seat、借出和借入表达共享容量；本设计只借鉴 work-conserving seat，不引入拒绝请求：[Kubernetes APF](https://kubernetes.io/docs/concepts/cluster-administration/flow-control/)
- Reactive Streams 规定由下游需求控制上游生产并保持缓冲有界；本设计据此引入 Hash 输出 credit：[Reactive Streams](https://github.com/reactive-streams/reactive-streams-jvm)
- SEDA 使用显式阶段和有界队列隔离供给与消费；本设计不采用其丢弃负载或动态线程池部分：[SEDA](https://www.usenix.org/event/usits03/tech/full_papers/welsh/welsh.pdf)
- DRFQ 指出独立按当前瓶颈切换资源调度可能振荡；本设计首版只使用一个确定性 seat 策略，不叠加两个快速 CPU/I/O 自适应控制器：[DRFQ](https://people.eecs.berkeley.edu/~alig/papers/drfq.pdf)

这些资料用于确定调度原则，不证明 `12/4` 是本媒体集的最优参数；最终默认值仍由同语料 A/B 决定。

## 11. 验证设计

### 11.1 Scheduler 确定性行为测试

测试不得依赖短时间 `sleep` 或源码字符串匹配，使用 permit gate、`Notify`、actor barrier 和显式 Drop：

- `T=4` 且两类持续等待时，活动许可收敛到 Media 3、Hash 1；
- 一类无等待时，另一类可以借满 4 个 seat；
- Hash 已借满时出现媒体需求，不抢占现有许可，后续释放依次恢复 Media 名义份额；
- 名义份额只影响压力分数，不会在另一类不可授予时把空闲 seat 变成硬保留；
- `T=1` 时按 `Media × 3 → Hash × 1` 轮换并受老化边界保护；多个容量 1 资源偏好冲突时选择最老可原子授予请求且同步更新轮换状态；
- 不相交磁盘仍能 work-conserving 推进；
- 一个不可授予的老化复合请求只冻结资源相交或争抢最后一个全局 seat 的年轻请求，授予或取消后清除保留；
- 复合盘只能原子取得所有底层盘许可；
- 非队首取消、交付接收端关闭、并发 Drop 后全部计数归零；
- 全局和每盘 total/class active 从不超过硬上限。

### 11.2 BaseCompute 行为测试

- 没有下游 output credit 或 refill token 时不 claim 新 item；
- 稳定态下，一个文件离开内容供给阶段最多生成一个替代 Hash 令牌；普通 credit 归还不直接补位，任务取消不产生令牌；
- 每轮主循环最多成功启动一个替代 Hash，随后重新经过 `select` 事件边界；
- 多个 ready Hash 不再引发一次性排空和整窗补满；
- 没有任何媒体请求的全缓存命中或全单项失败任务仍能逐项补位直至结束；
- 上游未关闭时暂时无 queued item 不清空令牌；上游关闭且权威 claim 返回空后置位 `input_exhausted`，清空 available token，后续在途完成不再补令牌；
- 媒体等待且 Worker admission 有缺口时，新释放 seat 优先恢复媒体目标；
- `media_acquiring + worker_dispatching + active` 继续计入 Worker admission，不能超过 W；
- `decode_waiting_owned` 永不超过 `2W`；
- 本地缓存和远端内容路径都必须先取得 decode credit；Started、派发失败、许可失败、Started 前终态和取消分别恰好释放一次；
- feature 阶段表现为 `media_io=0` 且 Worker/CPU 仍活动；
- 成功、缓存命中、读取失败、媒体许可失败和取消后所有 credit/计数归零；
- 乱序 Worker 终态仍按冻结身份写回正确内容。

### 11.3 遥测和协议测试

- 所有拆分状态及 output/refill/decode credit 的 current、peak、capacity 和守恒关系，包括 ready 结果、真实许可子集、派发中、Started 待归并和未知 Worker phase；
- 新字段 Protobuf round-trip、固定 tag、旧消息缺字段兼容；
- Desktop/NDJSON 对缺失字段显示 `null` 或 `—`；
- Worker phase 不包含 Hash；
- 最终化尾段不进入生产相位统计；
- 不规则采样间隔按真实时间加权。

### 11.4 原固定混合基准 A/B

继续使用原计划 Task 0 的固定基准，不把真实媒体相位指标与该门禁混为一个数值：

```powershell
cargo bench -p dedup-node-engine --bench base_compute_pipeline --locked
```

要求旧版和候选版各运行至少三次，固定：

- seed：`0x2026_08_23_C0DE_0000`；
- 文件清单、缓存命中、Hash session、媒体解码任务及夹具参数；
- `Cargo.lock`、Rust/Cargo 版本和构建配置；
- 每个版本的源码修订、基准可执行文件路径与 SHA-256；
- 使用 `elapsed_ms` 三轮中位数裁决原计划 15% 总墙钟门禁。

`decode_and_persist_ms`、`worker_idle_before_hash_ms` 和 files/s 只作分段解释，不能替代 `elapsed_ms`。

### 11.5 真实媒体 A/B

使用同机、同一只读媒体根、相同 Worker/读取配置，旧版与候选版各运行至少三次，顺序固定为：

```text
A, B, B, A, A, B
```

要求：

- A/B 使用独立数据库、缓存、日志和临时目录；
- 生产媒体只读，前后路径、长度、mtime 清单一致；
- 系统样本使用时间戳之前最近的任务快照；快照年龄不得超过 2,500 ms，且至少覆盖 95% 的生产积分时长，否则结论为 `INCONCLUSIVE`；
- 低/高 I/O 分桶阈值从基线生产样本计算，再原样用于候选版；
- 约 120 秒最终化尾段单独报告，不进入相位指标；
- 比较三轮中位数，不用单轮最好值；
- 结果摘要按 normalized path 排序，逐项包含终态、MD5、媒体类型、全部特征 payload SHA-256、缩略图/联系表 SHA-256，最后对规范化 JSONL 再计算整体 SHA-256。

## 12. 验收与部署门禁

### 12.1 正确性硬门禁

任一条件失败即禁止形成正式发布包或替换生产目录；为执行 A/B 生成的隔离候选包必须明确标记为测试用途，不受此处的循环前置限制：

- A/B 的成功、失败、跳过数量或结果摘要不一致；
- 媒体前后清单不一致；
- 遗留 queued/running item；
- 任一队列、credit、Worker、CPU 或磁盘许可突破硬上限；
- 取消或错误后存在不归零 ownership；
- 样本覆盖不足或必要新遥测缺失。

### 12.2 性能硬门禁

- 候选生产阶段结算文件/秒不得低于基线中位数的 95%；
- “媒体等待时的空闲 Worker-seconds”定义为生产样本中 `media_permit_waiting > 0` 时的 `Σ(sample_interval_seconds × (W - worker_slots.current))`，候选中位数至少降低 50%；
- 从基线非零读取生产样本计算时间加权 P25/P75，分别作为低/高 I/O 阈值，并原样应用候选；两个区间的空闲 Worker 均值差至少降低 50%；
- Worker CPU 核当量按 `ΣWorker CpuDeltaMs / sample_interval_ms` 计算，高/低 I/O 区间均值差至少降低 50%；
- 磁盘读取队列加权 P95 使用生产样本的时间加权经验分位数，候选不得超过基线 110%；
- 每个样本的进程私有内存定义为 `Node PrivateBytes + 所有同一 run-root 活 Worker PrivateBytes`，候选峰值不得超过基线 125%；
- 有待处理项时 `worker_slots.current=0 && hash_io.current=0 && media_io.current=0` 的资源气泡积分至少降低 50%；
- 缓存查询等待期间，由该等待项持有的 Worker slot、Hash 许可和媒体许可必须为 0；
- 将文件按持久化 ACK 顺序排列，从完成数首次达到总数 90% 到最后一项 ACK 的生产尾部跨度不得高于基线；
- 单文件完成时间定义为 item claim 到持久化 ACK，P95 不得超过基线 110%；
- 吞吐与 Worker CPU/空闲 Worker 的相关系数只作为佐证，不能单独决定通过；
- CPU 或磁盘达到 100% 不是通过条件。

原架构计划规定的 15% 门禁专指 11.4 固定混合基准的 `elapsed_ms` 中位数。若方案 B 只改善真实媒体生产相位但固定基准未达到 15%，结论只能写成“调度指标改善、部署门禁未通过”；只有用户基于完整原始数据明确豁免，才能改变部署裁决。

## 13. 发布、回滚与完成定义

### 13.1 测试包、发布边界和回滚

- A/B 使用旧版和候选版两个完整便携目录，不混用 Node、Worker、Desktop 或协议库；
- 报告必须绑定源码修订、完整包 SHA-256、运行配置和证据目录；
- 未通过全部门禁时不替换 `I:\Tool\mySingerServer-rust-v2-win-x64`；
- 正式部署不在本设计实施范围；后续若用户批准部署且触发回退条件，必须整体恢复上一版便携目录，禁止只替换单个 EXE；
- 回退不覆盖旧生产数据库，A/B 数据和原始证据继续保留。

### 13.2 完成定义

只有同时满足以下条件，本设计才算完成：

1. 活动 seat 策略、借用、自然收回、老化和所有硬上限均有确定性行为测试；
2. Hash 输出 credit 和 `2W` decode credit 在成功、失败、取消路径上守恒，补位令牌在输入耗尽边界确定性清空；
3. 新遥测能够区分等待、读取、完成未归并、派发中、Started 待归并、decode、feature、结果等待和未知阶段；
4. 所有相关 Rust、wire、Desktop 和 Windows 报告测试通过；
5. 同语料三对 A/B 原始证据完整；
6. 正确性和性能门禁全部通过，或用户基于原始证据明确接受未达到的门槛；
7. 隔离 A/B 候选包、SHA-256、测试记录和完整回滚包相互对应；生产部署记录由后续单独批准的部署任务生成。

本设计获用户规格确认后，下一步才生成逐文件、逐测试、逐提交的实施计划；本文件本身不授权修改生产代码或部署。
