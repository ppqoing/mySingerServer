# Dispatcher 同盘多在途身份验证记录

日期：2026-08-31

## 1. 验证范围

本次删除 `TaskFileDispatcher` 的“同一物理盘必须等待上一文件 SQLite ACK 才能交付下一文件”硬编码。一个物理盘仍只有一个 lane 和一份 TSV；Dispatcher 只按冻结的 `per_disk_limit` 保存精确身份窗口，真实磁盘许可、权重、Hash/Media 公平和老化继续由 `DiskReadScheduler` 负责。

Dispatcher 多在途实现 revision：`f91feb85ad7e5943bba5643250736e7450d833fb`。双盘当前席位修复 revision：`15872d7bd28dd730d9980c649496e5cc7db6f714`。同 lane 补位空窗修复 revision：`be40eb2`。正式候选包由本文档提交后的干净 HEAD 生成，最终 source tree SHA、包 SHA 和真实媒体终态由独立 evidence 绑定。

本次提交：

- `7a671ed0dee74ae8a5e57b811e7294386198f299`：精确 pending、lane 身份集合和配置窗口；
- `463406b8baac943b05bd117b803ab567776531dc`：continuation 复用窗口及跨身份 abandon 门禁；
- `e3497ec12b7e8c54af7565023326631d010b65b2`：多身份取消、permit 失败和状态字节门禁；
- `b9908a08acd461c2ec11bca158404f8ff32f2ed7`：真实 scheduler 的同盘额度与双盘配置门禁；
- `792814cb6a7479ce6aa9c34556ae770077ebf35d`：真实基础流式 Hash/Media 并发门禁；
- `f91feb85ad7e5943bba5643250736e7450d833fb`：适配取消等待指标夹具，使 Dispatcher 窗口为 2、全局读取容量为 1，继续制造真实许可等待；
- `15872d7bd28dd730d9980c649496e5cc7db6f714`：当前 `active/weight` 欠配额优先、等权约分和 Ready 盘超额轮转；
- `be40eb2`：交付当前项前创建并首轮轮询同 lane 下一许可请求，同时按真实 Hash/Media/Worker 空闲槽位释放禁止继续持有的预热许可。

## 2. RED 证据

以下失败都实际运行，且失败发生在目标行为断言，不是只读源码匹配：

1. `one_lane_dispatches_up_to_configured_limit_before_any_ack`：旧实现第二次 poll 返回 `Pending`，失败信息为“任务应已取得许可，实际为 Pending”。
2. `out_of_order_ack_releases_only_matching_same_lane_identity`：旧实现同样在第二身份交付前返回 `Pending`。
3. `same_lane_continuation_reuses_identity_window_slot` mutation：窗口满时错误拒绝 continuation，测试在 continuation 交付处返回 `Pending`。
4. `same_lane_pending_request_does_not_block_other_identity_abandon` mutation：B 的等待请求错误阻止 A abandon，`unwrap()` 得到 `InvalidInput`。
5. `cancellation_abandons_all_same_lane_identities_and_preserves_pending_rows` mutation：取消被错误写成失败，状态由期望 `[P,P,P]` 变成 `[F,F,F]`。
6. `same_lane_permit_failure_keeps_other_inflight_identities_intact` mutation：B 的 permit 失败错误写坏 A，状态由期望 `[P,P]` 变成 `[F,P]`。
7. `same_lane_hashes_continue_while_first_sqlite_ack_is_blocked` 单身份 mutation：首项 SQLite ACK 在途时其余同盘 Hash 未开始，1 秒门禁超时。
8. `same_lane_starts_multiple_media_workers_before_first_sqlite_ack` 单身份 mutation：首个 Worker 终态前无法取得四个同盘 `Started`，1 秒门禁超时。
9. `dispatcher_primes_same_lane_replacement_before_returning_current_task`：旧事件泵交付当前项后立即返回，同 lane 下一许可请求尚未创建，实际创建数为 1、期望为 2。
10. `admission_check_releases_a_primed_request_when_the_stage_closes`：初版补位实现未在 Media admission 关闭时释放已经 Ready 的预热许可，活动许可为 2、期望为 1。

每个 mutation 均在取得 RED 后恢复；最终 `git diff` 不含 mutation。

## 3. GREEN 与容量证据

- Dispatcher 默认 feature 全量：35/35 PASS；`test-hooks` 全量：36/36 PASS。
- DiskReadScheduler 全量：45/45 PASS，包含当前窗口 6:6、配置 5:1、Ready 盘多于全局席位的轮转，以及原有长期权重、逐盘硬上限、Hash/Media 公平和老化保护。
- 同 lane SSD 窗口 2：首个 ACK 前交付两个身份，第三个等待；第二项先 ACK 后只第二行变 `C`，第一、第三仍为 `P`。
- 同 lane HDD 窗口 1：第二身份在首项 ACK 前保持等待。
- 真实 scheduler，同一 PhysicalDisk27：`hdd_threads_per_disk=5`、`total_threads=5` 时首个 ACK 前持有五个不同身份，第六项等待；任一 permit 和身份窗口释放后第六项继续。
- 双盘窗口：SSD PhysicalDisk28 为 5，HDD PhysicalDisk29 为 1，全局 6；六个同时持有的真实 permit 实测为 SSD 5、HDD 1。比例来自测试配置，产品没有写死 5:1。
- continuation：窗口已满时仍以原 `TaskFileIdentity` 取得 Media permit，TSV 行数不增加，第三个普通身份继续等待窗口释放。
- 取消：三条未 ACK 行在全部身份 abandon 前后均为 `[P,P,P]`，没有误写 `F`。
- permit 失败：失败项仍为 `P`，同盘已交付兄弟身份保持可精确 ACK，之后失败项可按原队首重试。
- 真实事件泵：同盘窗口 3 时，首个 SQLite ACK 闸门未释放前三个 Hash reader 均已开始；同盘窗口 4、Worker 容量 4 时，首个 Worker 终态前四个不同 item 均收到 `Started`，随后按逆序终态完成并分别 ACK 为 `C`。
- 同 lane 补位：交付当前项前已经创建下一许可请求并只首轮轮询刚交付 lane；Base 和 Stage2 都用当下真实空闲槽位构造 admission，类别关闭后不保留隐藏许可。
- 既有 `first_hashed_media_miss_enters_worker_before_later_hash_finishes`、`active_media_does_not_block_later_hash_on_another_lane`、`cancellation_returns_pending_owner_without_acknowledging_rows` 均 PASS。
- NodeEngine 库全量：160/160 PASS。
- NodeEngine `--features test-hooks --all-targets`：退出码 0；全部测试目标通过，其中基础流水线 60/60、Dispatcher 36/36、磁盘调度 45/45、瞬态任务文件 25/25；固定 bench `elapsed_ms=125.107`、`persisted_completed=true`。
- Worker 进程协议：`WORKER_PROTOCOL_PROCESS_PASS`。
- Desktop Core 运行时验收协议：23/23 PASS。
- Desktop UI 绑定契约：15/15 PASS。

## 4. 非产品失败与修正

- 一次并行启动本地 PowerShell 失败：`CreateProcessWithLogonW failed: 1056`；改为串行命令后正常。
- 首次新增 HDD 回归时，环境继承 `CC/CXX=C:\Tools\WinLibs\mingw64\...`，使 SQLite 以 MinGW ABI 编译并由 MSVC 链接，出现 `___chkstk_ms`、`__isnan` 未解析；确认根因后只在 Cargo 子进程清除 `CC/CXX/AR/RANLIB/CFLAGS/CXXFLAGS/RUSTFLAGS/RUSTC_WRAPPER`，同一测试通过。
- 一次 Task 3 测试误调用 crate-private `cancel_pending_permit_requests`，在行为断言前编译失败；改用公开取消 token 路径后重新取得有效 RED/GREEN。
- 一次 mutation 恢复补丁匹配到相邻 `mark_failed` 行，GREEN 检查立即得到 `[F,F,F]`；检查真实 diff 后精确恢复 `mark_failed` 与 `abandon_in_flight` 各自语义，随后四项取消/失败回归及 Dispatcher 全量通过。
- 首次整 crate 回归中，旧 `pipeline_permit` 夹具仍把 Dispatcher 窗口和 Scheduler 全局容量同时设为 1，却断言存在第二个许可等待项，得到 `hash_waiting=0`、期望 1。产品实现按新窗口契约正确拒绝越界申请；夹具改为“窗口 2、全局容量 1”后重新形成真实等待，定向 1/1、文件全量 6/6、整 crate 全量全部通过。
- C 盘可用空间降至 9.93 GiB 时按约定停止重型命令，精确确认旧可再生 Cargo target `C:\tmp\rust-v2-main-desktop-target` 为 13.25 GiB、非重解析点且无其他进程使用后删除。当前 target、源码和证据均未删除；清理后 C 盘可用空间恢复至约 21.92 GiB。
- 一次不带 `--features test-hooks` 的 NodeEngine 整 crate 命令在执行测试前编译失败：`scan_roots` 和 `base_compute_pipeline` 无条件引用了 feature-gated 测试夹具。没有把该命令写成产品失败；按该工作树既定配置改用 `--features test-hooks` 后，库 160/160 及全部集成目标通过。

以上均未作为产品测试失败，也没有隐藏或拼接证据。

## 5. 首次双盘真实运行：当前窗口门禁失败

旧候选来源和边界：

- source revision：`a0275ece6a980539c49e7b7f2814499f8ea43a63`；source tree SHA-256：`d15ff2c9c7f21e6723b0385514d72d8c63990b6d86079354eb45a43ec6d05614`；
- 正式 ZIP SHA-256：`b186e54bd6c7f448f94419f0865eee7a8fae94e8ad06549a2937bfb621db9e08`；包内 manifest SHA-256：`32a266f52f819f69b17d8e94c4c44f9f7a7e43ec88840f448ad468afa70f7423`；
- 媒体根：`H:\pik\00000000000` → PhysicalDisk1（HP SSD EX900 1TB/NVMe），`I:\tmp` → PhysicalDisk2（INTEL SSDSC2BB800G6R/SATA）；
- Worker 20、全局读取席位 12、SSD 每盘 16、HDD 每盘 1、Everything，任务终态即结束；
- 原始 evidence：`C:\tmp\rust-v2-multi-inflight-single-run-a0275ec\evidence`。

运行约 498 秒后观察到当前窗口严重偏盘，故主动停止并保留全部证据，没有把未终态任务伪装为完成：

- 482 个同时包含两盘调度数据的任务快照中，两盘均有活动许可 255 个；PhysicalDisk1 为 0 且 PhysicalDisk2 占满 12 的样本为 200 个；
- PhysicalDisk1 活动峰值 10，PhysicalDisk2 活动峰值 12；最后有效样本仍为 `0:12`，两盘各有一个等待项；
- 最后累计 grant 为 PhysicalDisk1 `12187`、PhysicalDisk2 `9714`，说明长期会追赶，但不能抵消当前窗口长时间 `0:12` 的突发；
- Worker 非空闲峰值 20；媒体前后 39018 个文件的两份逐根 manifest SHA 完全一致；
- 任务仍为 `running`，没有结果摘要 SHA；该轮结论固定为 `INCONCLUSIVE`。中断后的报告脚本另出现 `Count` 属性输入异常，不改变运行未终态事实；
- 运行中另观察到 Worker 崩溃和非媒体文件缺少 `BaseSourceReadComplete`，它们是独立文件级证据，不在本次盘间席位修复中扩项。

源码根因位于旧 `select_weighted_lane`：等权 `16:16` 被当成“同一盘连续消费 16 个 deficit”，而全局只有 12 个席位，因此单盘可先吞满整个窗口。新增真实 actor RED：

1. 两块等权 SSD、各 12 个 Ready 请求、全局 12：旧实现实际 `[0,12]`，期望 `[6,6]`；
2. 三块等权 Ready 盘争两个全局席位：旧实现首轮两个许可落到同一 lane，违反超额轮转。

修复后先按当前 `active/configured_weight` 选择欠配额盘，权重用最大公约数约分；压力相同才消费原 deficit 和游标，老化门禁不变。等权 `16:16` 等价于 `1:1`，SSD/HDD `5:1` 仍为 `5:1`。该阶段定向 GREEN、DiskReadScheduler 45/45、TaskFileDispatcher 当时 34/34、NodeEngine `test-hooks` 全套、Worker 进程协议、Desktop Core 23/23、Desktop UI 15/15 均通过；补位空窗修复后的 Dispatcher 最新结果见第 3 节 36/36。

## 6. 第二次双盘诊断：权重修复生效但补位仍有空窗

候选 revision `2ff4785ec64e781faacacba4c7c0164780341cf9` 的正式 ZIP SHA-256 为 `24cf1bc0d5af7860f96e17367baac55450f192395b6fefc99b0f487b2c44fd74`，原始 evidence 位于 `C:\tmp\rust-v2-multi-inflight-final-2ff4785-attempt-02\evidence`。运行配置仍为 H/I 双盘、Worker 20、全局读取 12、SSD 每盘 16、Everything。

该轮早期约 90 个有效样本中，两盘同时活动由旧轮的 4 个提高到 61 个，证明 `active/weight` 修复已经生效；但仍有 28 个 `PhysicalDisk1=0、PhysicalDisk2=12` 样本。确认重复 `0:12` 后主动停止，外层正常收束并保留媒体前后清单；任务未到终态，`run_status=FAIL`、结果摘要为 `INCONCLUSIVE`，不能作为最终验收。

源码继续定位到 `TaskFileDispatcher::poll_lane_requests`：旧实现交付一项后立即返回，刚交付 lane 的替代请求只能等下一次外层事件泵才创建。这个短暂的非 Ready 空窗会让另一盘连续借走全部 12 个席位。`be40eb2` 改为在返回当前项前创建并首轮轮询同 lane 下一请求；只预热刚交付 lane，并用真实 Hash/Media/Worker 空闲槽位约束，避免为追求盘公平而越过阶段容量。

修复的定向 RED/GREEN、Dispatcher 36/36、NodeEngine `test-hooks --all-targets` 和格式检查均已通过。该轮仍是失败诊断证据，不与后续最终运行拼接。

## 7. 第三次双盘诊断：只对当前 waiter 加权仍不能满足固定计算席位

候选 revision `20a25a0` 的 source tree SHA-256 为 `bbd425db8060fa4157ac36846871d4ad7a1bd3212597f882f131257a5373d17c`，正式 ZIP SHA-256 为 `bb656c9a9095f9912bf1a39f1b75ee0ccd35439cca59490341022529c75c3685`，原始 evidence 位于 `C:\tmp\rust-v2-multi-inflight-final-20a25a0\evidence`。配置仍为 H/I 双物理盘、Worker 20、全局读取 12、SSD 每盘 16、Everything。

同 lane 补位预热减少了空窗，但没有闭合用户要求的“按物理盘权重分配计算线程”：

- 前 54 个样本中 PhysicalDisk1/2 平均 active 为 `2.33/9.65`，`0:12` 有 13 个样本，两盘同时 active 41 个样本，累计 grant 为 `1463/421`；
- elapsed 163 秒时最近 60 个样本平均 active 为 `2.50/9.22`，仍有 10 个 `0:12` 样本；最后窗口约为 `5:7`，累计 grant 为 `3522/1327`；
- 两盘都有等待项，但先到且读取较慢的 PhysicalDisk2 已持有大量非抢占 permit，PhysicalDisk1 后到时只能等待自然释放，`active/weight` 只能约束“下一次授予”，不能约束当前任务的固定盘间席位；
- 确认问题后只停止精确 runtime client，harness 正常清理，外层退出码为 0；`run_status=FAIL`、客户端码 `-1`、结果摘要 `INCONCLUSIVE`；媒体前后联合 SHA-256 均为 `3302f70759eb894e2c456913fc7668be9001bd24f0dcf5060081babd47ee30eb`。该轮不是验收通过证据。

本次修复新增每个 Dispatcher 独立的 `DiskReadLaneGroup`。只有真实任务 TSV lane 登记；任务组总席位为 `total_threads`，每盘先给 1，再按配置权重和逐盘硬上限分配剩余席位。Worker、Hash 和 Media 槽位由流水线独立限制，不反向截断读取并行。登记盘在短暂无 waiter 时仍保留目标，先到盘不能借满；盘终态注销后剩余盘重新分配。已发 permit 不抢占，Hash/Media 公平、老化和全局/逐盘硬上限仍由原 scheduler actor 维护。

新增行为证据：

1. 两块等权 SSD、任务组 12：第二盘的 12 个请求先到时只交付 6 个，第一盘后到立即交付保留的 6 个；
2. SSD/HDD `5:1`、任务组 6：先到 SSD 只能交付 5 个，HDD 保留 1 个；
3. 读取线程 12、Worker 6、两块等权盘：读取目标仍为每盘 6，Worker/Hash/Media 容量独立门控；
4. 三块等权盘争两个席位：每次释放边界轮转覆盖第三盘；
5. 一个盘终态注销后，其已释放席位立即重新分配给剩余盘；
6. 真实 `TaskFileDispatcher + DiskReadScheduler` 首个 12 项窗口为 `[6,6]`。

首次提交后的 5.6 Sol Max 只读审查发现两个有效生命周期缺口：首次 grouped acquire 前没有结构性冻结物理盘集合；Dispatcher Drop 清理任务组早于关闭 pending 许可 future。对应旧实现行为测试分别证明晚登记新盘会被接受、清理回调能观察到未关闭 future。修复后，Dispatcher 在 `seal` 或首个许可 future 创建前冻结完整 lane 集合，冻结后新增盘返回明确错误；Drop 先关闭 pending/continuation/publication future，再清任务组并触发最终唤醒。

审查同时建议把任务组读取席位限制为 `min(total_threads, Worker 数)`。真实流水线回归证明该建议与现有资源所有权冲突：Worker=1、读取线程=3 时需要允许三个 Hash 读取并发；同盘 Hash/Media 也必须能同时占用两个独立读取许可。因此保留读取总预算 `total_threads`，Worker、Hash、Media 槽位继续独立门控。新增行为测试确认 Worker 6 不会把等权双盘读取目标从 6 错降为 3，Worker 2 也不会阻止三块盘各取得一个仍在全局读取预算内的许可；当真实盘数超过 `total_threads` 时仍由既有权重游标和老化保护轮转。

最终代码验证结果：DiskReadScheduler 50/50 PASS，TaskFileDispatcher 39/39 PASS；NodeEngine `--features test-hooks --all-targets` 退出码 0，其中库测试 160/160、基础计算流水线 60/60，固定 bench `elapsed_ms=128.061` 且 `persisted_completed=true`；Worker 进程协议输出 `WORKER_PROTOCOL_PROCESS_PASS`；Desktop Core 运行时验收协议 23/23、Desktop UI 绑定契约 15/15。格式检查和 `git diff --check` 均通过。候选包、双盘真实媒体终态和最终只读审查仍按下节门禁执行。

## 8. 待完成门禁

- 绑定本次任务级权重席位修复后干净 HEAD 的正式候选包与外置验收客户端/结果导出器 SHA；
- 修复候选在 `H:\pik\00000000000` 与 `I:\tmp` 上的一次双物理盘真实媒体终态运行；
- 逐盘 active 峰值、Worker 峰值、CPU/IO、任务终态、崩溃完整路径和结果导出 SHA；
- `gpt-5.6-sol`、reasoning `max` 最终只读审查。

本次不部署、不替换、不清理 `I:\Tool`。
