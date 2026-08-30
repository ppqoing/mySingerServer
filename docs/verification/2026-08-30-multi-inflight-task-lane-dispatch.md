# Dispatcher 同盘多在途身份验证记录

日期：2026-08-31

## 1. 验证范围

本次删除 `TaskFileDispatcher` 的“同一物理盘必须等待上一文件 SQLite ACK 才能交付下一文件”硬编码。一个物理盘仍只有一个 lane 和一份 TSV；Dispatcher 只按冻结的 `per_disk_limit` 保存精确身份窗口，真实磁盘许可、权重、Hash/Media 公平和老化继续由 `DiskReadScheduler` 负责。

当前实现 revision：`f91feb85ad7e5943bba5643250736e7450d833fb`。最终文档提交和候选包完成后补记最终 revision、source tree SHA、包 SHA 与真实媒体证据。

本次提交：

- `7a671ed0dee74ae8a5e57b811e7294386198f299`：精确 pending、lane 身份集合和配置窗口；
- `463406b8baac943b05bd117b803ab567776531dc`：continuation 复用窗口及跨身份 abandon 门禁；
- `e3497ec12b7e8c54af7565023326631d010b65b2`：多身份取消、permit 失败和状态字节门禁；
- `b9908a08acd461c2ec11bca158404f8ff32f2ed7`：真实 scheduler 的同盘额度与双盘配置门禁；
- `792814cb6a7479ce6aa9c34556ae770077ebf35d`：真实基础流式 Hash/Media 并发门禁；
- `f91feb85ad7e5943bba5643250736e7450d833fb`：适配取消等待指标夹具，使 Dispatcher 窗口为 2、全局读取容量为 1，继续制造真实许可等待。

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

每个 mutation 均在取得 RED 后恢复；最终 `git diff` 不含 mutation。

## 3. GREEN 与容量证据

- Dispatcher 全量：34/34 PASS。
- DiskReadScheduler 全量：42/42 PASS，包含配置权重、全局不足、逐盘硬上限、Hash/Media 公平和老化保护。
- 同 lane SSD 窗口 2：首个 ACK 前交付两个身份，第三个等待；第二项先 ACK 后只第二行变 `C`，第一、第三仍为 `P`。
- 同 lane HDD 窗口 1：第二身份在首项 ACK 前保持等待。
- 真实 scheduler，同一 PhysicalDisk27：`hdd_threads_per_disk=5`、`total_threads=5` 时首个 ACK 前持有五个不同身份，第六项等待；任一 permit 和身份窗口释放后第六项继续。
- 双盘窗口：SSD PhysicalDisk28 为 5，HDD PhysicalDisk29 为 1，全局 6；六个同时持有的真实 permit 实测为 SSD 5、HDD 1。比例来自测试配置，产品没有写死 5:1。
- continuation：窗口已满时仍以原 `TaskFileIdentity` 取得 Media permit，TSV 行数不增加，第三个普通身份继续等待窗口释放。
- 取消：三条未 ACK 行在全部身份 abandon 前后均为 `[P,P,P]`，没有误写 `F`。
- permit 失败：失败项仍为 `P`，同盘已交付兄弟身份保持可精确 ACK，之后失败项可按原队首重试。
- 真实事件泵：同盘窗口 3 时，首个 SQLite ACK 闸门未释放前三个 Hash reader 均已开始；同盘窗口 4、Worker 容量 4 时，首个 Worker 终态前四个不同 item 均收到 `Started`，随后按逆序终态完成并分别 ACK 为 `C`。
- 既有 `first_hashed_media_miss_enters_worker_before_later_hash_finishes`、`active_media_does_not_block_later_hash_on_another_lane`、`cancellation_returns_pending_owner_without_acknowledging_rows` 均 PASS。
- NodeEngine 库全量：160/160 PASS。
- NodeEngine 整 crate：退出码 0；全部测试目标通过，其中基础流水线 60/60、Dispatcher 34/34、瞬态任务文件 25/25、`pipeline_permit` 6/6。
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

以上均未作为产品测试失败，也没有隐藏或拼接证据。

## 5. 待完成门禁

- 正式候选包与外置验收客户端/结果导出器 SHA；
- `H:\pik\00000000000` 与 `I:\tmp` 的唯一一次双物理盘真实媒体运行；
- 逐盘 active 峰值、Worker 峰值、CPU/IO、任务终态、崩溃完整路径和结果导出 SHA；
- `gpt-5.6-sol`、reasoning `max` 最终只读审查。

本次不部署、不替换、不清理 `I:\Tool`。
