# Task 2 执行报告

## 状态

已在指定 worktree 的 `b434de67` 完成实现。运行任务详情只保留当前 Node/Desktop 进程内 registry：Node 启动不再恢复或发布旧 SQLite 任务；四类业务任务直接使用其业务 ID 作为运行任务 ID。schema 3 物理表和 Task 1 的启动期 transient 清理保持不变。

## RED / GREEN 证据

RED：

- `cargo test -p dedup-desktop-core --test runtime_tasks_e2e --locked -- --test-threads=1`
  - 原恢复测试在 Node 启动后仍期待旧 SQLite 任务；改写为验证启动空 registry、Desktop 完整空列表原子替换和重启后无旧摘要。
- `cargo test -p dedup-node-engine --features test-hooks stage2_create_and_shutdown_stay_responsive_while_worker_is_held --locked -- --test-threads=1`
  - 原行为拒绝活动任务重启；改为验证取消、等待收束后重启成功且无旧 running runtime 行。
- `cargo test -p dedup-node-engine --features test-hooks --locked -- --test-threads=1`
  - 首轮 `analysis_runtime_details` 1 项失败：控制协程误用 `NodeStore::open`，按保留的 Task 1 启动清理丢弃了 transient 行。改为从现有 store 使用 `reopen()`，并等待异步二筛消费者提交终态。

GREEN：

- `cargo check -p dedup-node-engine --locked`：通过。
- `cargo test -p dedup-desktop-core --test runtime_tasks_e2e --locked -- --test-threads=1`：1/1 通过。
- `cargo test -p dedup-node-engine --test worker_pipeline --locked -- --test-threads=1`：22/22 通过。
- `cargo test -p dedup-node-engine --features test-hooks stage2_create_and_shutdown_stay_responsive_while_worker_is_held --locked -- --test-threads=1`：门禁 1/1 通过。
- `cargo test -p dedup-node-engine --features test-hooks --locked -- --test-threads=1`：通过；库单元 60/60，`analysis_runtime_details` 4/4，`base_compute_pipeline` 59/59，及其余集成套件均通过。
- `cargo test -p dedup-node-store --locked -- --test-threads=1`：44/44 通过。
- `cargo test -p dedup-desktop-core --locked -- --test-threads=1`：本地可运行测试通过；PostgreSQL/打包环境依赖测试按既有条件跳过。
- `cargo test -p dedup-desktop-ui --test bindings_contract --locked -- --test-threads=1`：16/16 通过，包含真实 MainWindow 的两种事件顺序门禁。
- `cargo fmt --all -- --check`：通过。
- `git diff --check`：通过。

所有 Cargo 命令均清空编译环境变量并使用 `C:\tmp\rust-v2-core-scope-target`、关闭 incremental/debug info；每个重型阶段前 C 盘均大于 10 GiB。

## 改动范围

- `crates/node-engine/src/runtime_tasks.rs`：删除 Recovery kind/stage/恢复入口，新增业务 ID 直通的创建入口。
- `crates/node-engine/src/actor.rs`：删除 Node 启动恢复和恢复发布；扫描、本地分析、二筛、删除直接登记业务 ID；重启取消收束旧任务、销毁旧 Pool 后用生产配置创建新 Pool。
- `crates/node-engine/src/worker/pool.rs`：删除 prepare/requeue/restart 三阶段 API、命令和状态。
- `crates/node-store/src/tasks.rs`：删除恢复与计划 requeue API，保留 schema 3 表和启动清理。
- 测试：覆盖 Node 启动空运行表、Desktop 完整空列表替换、MainWindow 事件交错、活动 Worker 重启收束，以及 Task 1 启动清理与同进程 reopen 边界。

## 提交

初始实现：`ae6eab9`（`refactor: keep runtime tasks process-local`）。

## concerns

- `NodeEngine::spawn` 的可控测试池没有 worker 可执行文件配置，因此测试分支只归还已收束的测试 Pool；生产 `NodeRuntime::start` 保存不可变 `WorkerPoolConfig`，重启会丢弃旧 Pool 并创建新 Pool。
- PostgreSQL 和打包环境依赖的 desktop-core 测试未配置外部环境，按既有 `ignored` 条件跳过；未修改中心 PostgreSQL 同步恢复事实或协议版本。

## Fix round 1（独立审查修复）

### 审查发现与 RED

- `cargo test -p worker --test worker_pool --no-run --locked`：原 fixture 仍调用已删除的 `prepare_planned_restart` 与 `restart_after_requeue`，报 `E0599` 两处。
- 替换 fixture 为“活动旧 Pool → `shutdown().await` → 等待旧 PID 退出 → 同配置新 Pool → 新 PID 可派发”后，在实现前同一编译命令报 `shutdown` 缺失的 `E0599` 两处。
- `cargo test -p dedup-desktop-core --test controller_runtime_tasks runtime_list_failure_removes_old_summary_and_only_selected_details_stale --locked -- --test-threads=1`：新增真实 TCP 会话用例超时，证明列表传输失败时旧 Node 摘要被错误回填。

### 修复

- `WorkerPool::shutdown(self).await` 向唯一 actor 发送单阶段关闭命令，等待所有已发送终止命令的 slot 报告退出、清空运行快照，并等待 actor/Job 释放。生产 `restart_engine` 在启动新 Pool 前必须等待该关闭完成；没有启动配置的可控测试池只验证收束，明确拒绝冒充生产重建。
- 删除 worker 进程 fixture 的 `FakeStore` 和旧两阶段 API 调用，改为真实旧 PID 退出与新 Pool 派发验证。
- Desktop 运行任务刷新在列表传输错误时不回填旧摘要；仅当前选中详情的独立请求失败保留详情并标记 stale。
- `runtime_tasks_e2e` 预置真实 `running` task/item，并通过独立 rusqlite 连接确认 Node 启动事务清除了旧行；旧 `runtime_recovery` 测试改为同一进程事实边界。

### Fix 验证

- `cargo test -p worker --test worker_pool --no-run --locked`：通过。
- `cargo test -p worker --test worker_pool --locked -- --test-threads=1`：3/3 通过（未配置 FFmpeg fixture 时真实媒体分支按设计返回）。
- `cargo test -p dedup-desktop-core --test controller_runtime_tasks --locked -- --test-threads=1`：7/7 通过，包含列表失败分支。
- `cargo test -p dedup-desktop-core --test runtime_tasks_e2e --locked -- --test-threads=1`：1/1 通过。
- `cargo test -p dedup-node-engine --features test-hooks --locked -- --test-threads=1`：通过；库单元 60/60，受影响集成套件通过。
- `cargo test -p dedup-node-store --locked -- --test-threads=1`：44/44 通过。
- `cargo test -p dedup-desktop-ui --test bindings_contract --locked -- --test-threads=1`：16/16 通过。

### 更正

此前报告中“`cargo test -p dedup-desktop-core --locked` 全量通过”不成立：该命令在 `node_config_controller` 子测试挂起，随后被停止，不能计为通过。本轮只报告上述实际退出的 desktop-core 聚焦测试；未重新运行该可能挂起的全量命令。

Fix round 1：`cf7e3664`（`fix: await worker pool shutdown before restart`）。

## Fix round 2（关闭 join 与超时收束）

### RED / 修复

- 审查复现：旧实现的 slot driver 是 detached `tokio::spawn`；它可先发布 `Exited`、后继续执行，Pool actor 因而可能在 driver 和 Job 资源尚未收束时回复关闭；若永远不发布 `Exited`，重启可永久等待。
- 新增 `shutdown_waits_for_driver_join_after_exited_event`：driver 先发送 `Exited` 后受 `Notify` gate 阻塞；未释放 gate 时关闭 future 不得完成，释放后才通过。
- 新增 `shutdown_timeout_aborts_driver_and_clears_runtime_state`：driver 接收 `Terminate` 后永不发送 `Exited`；暂停 Tokio 时间推进固定 1 秒上限，关闭返回 `ShutdownTimeout`，并断言 running/PID/CPU 快照均为空。
- `SlotHandle` 现在唯一持有 driver `JoinHandle`。关闭使用统一 deadline 先等所有 `Exited`，再逐一 join；超时、关闭通道或 join 异常时中止并 await 全部残余 driver，随后 actor 返回、Job drop。`WorkerPool::shutdown` 即使收到关闭错误也总会 await pool actor。
- `restart_engine` 在旧 Pool 已完成资源释放但报告关闭异常时，仍以保留的 `WorkerPoolConfig` 尝试创建并安装新 Pool；新 Pool 成功时返回清晰诊断错误但引擎可用，新启动失败时配置仍保留供下一次重试。可控 actor 夹具没有真实 worker 启动配置，继续只作为活动任务取消/收束证据，不宣称它验证生产重建。

### Fix 验证

- `cargo test -p dedup-node-engine shutdown_ --lib --features test-hooks --locked`：5/5 通过，含两条新增关闭行为测试。
- `cargo test -p dedup-node-engine stage2_create_and_shutdown_stay_responsive_while_worker_is_held --lib --features test-hooks --locked`：1/1 通过。
- `cargo test -p worker --test worker_pool --no-run --locked`：通过；运行：3/3 通过。
- `cargo test -p dedup-desktop-core --test controller_runtime_tasks --locked`：7/7 通过；`runtime_tasks_e2e`：1/1 通过。
- `cargo test -p dedup-node-store --locked`：44/44 通过；`cargo test -p dedup-desktop-ui --test bindings_contract --locked`：16/16 通过。
- `cargo test -p dedup-node-engine --features test-hooks --locked`：未通过；库单元 62/62、多个集成套件通过，但 `base_compute_pipeline` 为 55/59，四个已有的流水线时序断言失败。逐项复跑中前三项通过；`lookup_cache_reports_each_completed_thousand_file_batch` 仍在 `SQLite 应保存基础缓存查询阶段` 失败，未触碰该路径，未把该全量命令计为通过。
- `cargo fmt --all -- --check`、`git diff --check` 通过；`rg` 未发现 `prepare_planned_restart`、`restart_after_requeue`、`begin_restored`、`begin_recovery` 产品调用。

Fix round 2：本提交（`fix: join worker drivers during shutdown`）。

## Fix round 3（取消路径 driver 收束与同进程观察）

### RED / 修复

- 独立复现 `cargo test -p dedup-node-engine --test base_compute_pipeline lookup_cache_reports_each_completed_thousand_file_batch --features test-hooks --locked`：退出 1，`SQLite 应保存基础缓存查询阶段`。根因是测试观察协程在同一进程以 `NodeStore::open` 打开第二连接，误触 Task 1 已定义的启动期 transient 清理；并非该测试要验证的启动行为。
- 独立审查还指出：取消路径等待 `Exited` 没有 deadline，且普通 Exited/取消路径直接 Drop `SlotHandle` 会 detach driver。新增 `exited_event_waits_for_driver_before_installing_replacement`：旧 driver 在 Exited 后受 gate 阻塞时 replacement 计数保持零，释放 gate 后才安装。
- 新增 `cancel_timeout_aborts_driver_and_keeps_pool_shutdown_reachable`：运行 slot 接收 Terminate 后不回 Exited；虚拟时间推进固定上限后取消返回 `ShutdownTimeout`，旧 driver 被 abort+await、共享运行/PID/CPU 清空，并继续完成 Pool 关闭。这是 actor 重启前“取消 → 收束旧 Pool → 关闭/新建”调用链的无进程夹具证据。
- `lookup_cache_reports_each_completed_thousand_file_batch` 现在在交出 `store` 前创建 `store.reopen()` 只读 observer；删除错误的 `observer_machine`/二次 `open`。产品代码未改。
- 普通 Exited 与取消路径均把被移除的 `SlotHandle` 移交给 join helper；正常情况等待 driver 返回，deadline/driver 异常时 abort+await 后才清状态或安装 replacement。取消命令等待 Exited 使用同一固定上限，不再在该 Pool actor 内无界等待。

### Fix 验证

- `lookup_cache_reports_each_completed_thousand_file_batch`：1/1 通过。
- 新增 gate 与取消超时测试：各 1/1 通过；`cargo test -p dedup-node-engine --lib --features test-hooks --locked`：64/64 通过。
- `cargo test -p worker --test worker_pool --no-run --locked`：通过；运行 3/3 通过。
- `controller_runtime_tasks`：7/7 通过；`runtime_tasks_e2e`：1/1 通过。
- 完整 `base_compute_pipeline` 的默认并行运行仍有 3 条既有时序断言失败；按该套件时序边界使用 `-- --test-threads=1` 串行运行后 59/59 通过。聚焦 reopen 回归也实际通过。

Fix round 3：本提交（取消/driver 收束与测试 observer 适配）。
