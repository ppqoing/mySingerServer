# Hash 与 Media 流式流水线设计

## 1. 目标

基础计算不再等待全部文件完成 Hash 后才开始 Media。每一个 Hash 完成后立即执行一次 SQLite 内容缓存单项查询；真正缺少媒体特征的项立即进入 Media Worker，仍未完成的 Hash 同时继续读取。

本次只解决 Hash/Media 整批屏障，不改变媒体算法、任务文件格式、物理盘权重调度、Worker 协议、SQLite 表结构或远端缓存语义。

## 2. 当前问题

当前瞬态基础计算的协调器虽然按 `Media -> Hash -> Media` 循环调用阶段函数，但 `run_task_file_hash_pass_with_remote` 会：

1. 持续领取所有剩余 Hash 行；
2. 等待全部 Hash `JoinSet` 完成；
3. 最后才批量查询 SQLite，并登记 Hash 到 Media 的续算；
4. 返回协调器后才启动 Media。

因此真实运行表现为最后一个 Hash 结束后第一个 Media 才开始，Hash 与 Media 重叠为零。

## 3. 方案选择

有三个可行方向：

1. **单一事件泵（采用）**：同一个协调循环同时监听 Hash 完成、远端单项查询完成、Worker 事件和 SQLite ACK。没有阶段函数等待整轮结束，所有任务文件和 Worker 所有权仍由一个 owner 串行修改。
2. **跨轮次 Hash 窗口（不采用）**：Media 运行时 Hash future 可以继续读取，但协调器仍会等当前 Media pass 返回后才处理新的 Hash 结果，不满足“Hash 完成立即查询”。
3. **独立 Hash/Media actor（不采用）**：可以完全并行，但需要跨 actor 拆分 dispatcher、上下文和 ACK 所有权，通道与错误收束明显更复杂。

采用第一种方案，在现有基础协调器内形成一个有界事件泵。

## 4. 采用设计

### 4.1 单一协调事件泵

基础协调器持续持有以下运行态：

- 有界 Hash `JoinSet`，上限为 `hash_capacity`；
- 等待可选远端单项查询的拥有型 Hash 结果，数量同样受 `hash_capacity` 限制；
- 已派发 Media 的活动表，上限为 `worker_capacity`；
- taskless 持久化消息与 SQLite ACK 状态；
- 唯一 `TaskFileDispatcher`、任务上下文和 manifest。

每个循环只处理一个已经发生的事件，然后重新计算 Hash/Media admission。`tokio::select!` 同时监听：

- Hash 读取完成；
- 可选远端单项查询完成；
- Worker `Started/SourceReadComplete/Completed/Crashed`；
- SQLite 持久化 ACK；
- 取消令牌。

dispatcher 使用现有 Hash/Media 联合 admission 和物理盘调度规则。任何分支都不能自行清空同阶段的全部工作后才返回主循环。

### 4.2 单个 Hash 立即查询

为基础计算增加语义明确的 `lookup_base_cache_by_key` 单项入口。该入口每次只接收一个 `ContentKey`，执行一次单项缓存查询调用并返回一个可选缓存记录；内部可以复用现有完整缓存装载逻辑，但不能缓存、合并或等待后续 Hash。图片、视频及帧表的既有完整性装载语义保持不变。

扫描开始前按路径查询 SQLite 的阶段仍保留现有批量查询，因为它处理的是已经冻结的扫描清单，不属于“已完成 Hash 等待 Media”的屏障。本次只把 Hash 后的 ContentKey 查询改为单项立即查询。

可选远端缓存同样不攒批：本地记录仍不完整时，对当前一个 ContentKey 立即启动一次异步查询；远端查询期间事件泵继续处理其他 Hash、Media 和 ACK。远端失败后保持现有降级逻辑，后续继续 SQLite-only。

查询结果保持现有规则：

- SQLite 完整命中：投递 taskless 持久化，收到 ACK 后将 TSV 行改为 `C`；
- 本地不完整且远端可用：只对当前 ContentKey 执行一次远端单项查询；
- 真正缺少媒体字段：沿用同一 TSV 行和身份登记 Media continuation；
- Hash 读取失败：投递失败记录，收到 ACK 后改为 `F`。

### 4.3 真正并行而不是小屏障

单项查询和续算登记完成后，缺少媒体字段的同一任务身份立即成为 Media continuation。事件泵下一次联合 admission 即可派发它；其他 Hash future 不取消、不 join，继续后台读取并持有各自磁盘许可。

Media Worker 运行期间，新的 Hash 完成事件仍由同一个事件泵立即消费并单项查询，不需要等待当前 Media 完成。Hash、Media、远端单项查询和 SQLite ACK 因而可以同时在途。

这是一条有界的流式流水线，不引入无界内存队列，也不要求等待整个任务、当前 Media 或固定数量文件。

## 5. 所有权与取消

- `TaskFileDispatcher` 仍是任务文件唯一 owner，所有 `P -> C/F` 只发生在 SQLite ACK 后。
- Hash permit 仍由对应读取 future 持有，读取完成立即释放，不跨缓存查询或 ACK。
- Media permit 仍保留到 `BaseSourceReadComplete`。
- Worker 槽位、崩溃和单文件失败仍由现有 Media 状态机处理。
- 取消或任务级错误时，事件泵取消并 join 全部 Hash/远端 future，取消 Worker、释放媒体许可，再清理 dispatcher 的精确在途身份并返回 pending owner；取消不写 `F`。
- 正常终态必须同时满足：剩余 Hash 为零、Hash/远端 future 为空、Media/持久化无在途、上下文为空、dispatcher 为 `Drained`。

## 6. 调度与背压

- 不新增读取线程池；继续使用 `DiskReadScheduler` 的全局额度、物理盘额度、SSD/HDD 配置权重和老化保护。
- 不绕过 `TaskFileDispatcher`。Hash 和 Media 仍通过同一个 dispatcher 请求许可，Hash 后的同 lane Media continuation 保持既有优先语义。
- Hash 在途数量不超过 `hash_capacity`，Media 活动数量不超过 `worker_capacity`。
- Hash 完成结果不能进入无界 `Vec`；事件泵每次取出一项立即查询，本地查询结束后只进入远端 future、Media continuation 或持久化队列之一。
- SQLite 写入仍由单写 actor 串行 ACK；ACK 背压时不提前改任务文件状态。

## 7. 行为验证

先增加一个旧实现必失败的真实协调器测试：

1. 同一任务准备至少两个需要 Hash 后 Media 的文件；
2. 第一个 Hash 立即完成，第二个 Hash 由读取闸门保持未完成；
3. 记录 SQLite ContentKey 查询调用，确认第一次查询数量为 1，且发生在第二个 Hash 完成前；
4. 不释放第二个 Hash 闸门，断言已经收到第一个 Media Worker `Started`；
5. 再让第二个 Hash 完成，同时暂不结束第一个 Media，断言第二次单项查询已经发生，证明查询不等待当前 Media；
6. 完成两个 Worker，断言 SQLite 结果正确、两行均为 `C`、身份未增加第二行，且所有 ContentKey 查询调用数量恒为 1。

补充回归：

- 取消发生在“Media 已启动、另一个 Hash 仍在途”时，Hash permit、Worker、任务文件 owner 全部收束，TSV 未 ACK 行保持 `P`；
- Hash 读取失败不阻断已经启动的 Media；
- 完整缓存命中不启动 Worker；
- 现有物理盘权重、Worker 崩溃、taskless ACK、任务文件和基础计算测试全量通过；
- 固定基准只用于防止明显退化，本次不自动启动真实媒体、打包或部署。

## 8. 不做事项

- 不新增恢复任务、TaskCatalog、索引文件或分页；
- 不修改 SQLite schema，也不修改扫描路径缓存的批量查询；
- 不解决本轮真实运行中独立存在的 Worker 堆损坏/访问冲突；
- 不重新执行真实媒体验收，除非后续明确要求。
