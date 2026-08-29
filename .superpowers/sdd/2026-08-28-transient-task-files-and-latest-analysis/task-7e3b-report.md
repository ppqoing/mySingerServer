# Task 7E3B：Node Actor 接入瞬态 Stage2 生产运行

## 结果

外部 `DispatchStage2` 已切换到 `run_stage2_batch_production`。二筛运行 ID、取消、进度、终态统计和 outbox 高水位仅由当前进程 `RuntimeTaskRegistry` 保存；该路径不再写入 SQLite 的 `tasks`、`task_items`、`task_stages`。

## 实现范围

- `JobIdentity` 用 `TransientStage2(TaskId)` 表示外部二筛，`BackgroundJob::Stage2` 与 `ActiveJob` 共享同一取消令牌。
- Actor 启动生产二筛时传入 runtime 根目录、联系表缓存、当前磁盘读取配置、有效 Worker 数、`MAX_BASE_TASK_BATCH + Worker 数` 的持久化容量、取消令牌和 PostgreSQL 缓存配置。
- 新增小型 `BackgroundTerminal`，后台仅在 WorkerPool、Store 和 task-file 目录均收束后交还终态；actor 归还 Pool 后才以真实 highwater 发布 Completed。失败与取消不携带伪造高水位。
- 生产二筛向已有 RuntimeTaskReporter 写入 `LookupStage2Cache → ComputeStage2Features` 阶段；完整缓存命中以零 Worker 正常收束。单文件 F 仍使批次 Completed，并保留精确 completed/failed 统计。
- `CancelTask`、重启和关闭对活动瞬态扫描/二筛只取消运行资源并等待收束，不再更新旧任务表。`QueryTask` 与 `ListTasks` 优先返回 RuntimeTask 的真实 highwater。
- 保留本地分析的 `WorkerPoolStage2Processor` 和旧处理路径；未迁移 Desktop 生产代码。
- 为避免 task-file runner 在 `await` 中持有非 `Sync` 的生产结构，二筛派发前复制冻结上下文；不改变任务上下文或 permit 所有权。
- `cross_phase2` 的缓存重发回归改为直接验证完整 Stage2 特征重发：SQLite outbox 高水位前进、零 Worker、旧任务表为空，不再读取旧任务快照。

## TDD 证据

先添加并实际运行 Actor 行为用例，旧实现分别出现：Worker 持有时不保留 task-file 目录、完整缓存命中无法以 RuntimeTask Completed 收束、单文件 F 使批次等待超时。完成最小接线后，新增瞬态目录/高水位、缓存、取消、重启关闭、单文件 F 用例均通过。

## 验证

在每个 Cargo 命令前确认 C、D 盘可用空间分别约 24.87 GiB、11.95 GiB，满足不少于 10 GiB 的门槛；命令串行执行并使用指定的 `C:\\tmp\\rust-v2-core-scope-target-task7b2d2c1` 构建目录：

- `cargo test -p dedup-node-engine --features test-hooks actor --locked -- --test-threads=1`：通过，Actor 定向 16 项通过。
- `cargo test -p dedup-node-engine --features test-hooks --lib --locked -- --test-threads=1`：通过，141 项通过。
- `cargo test -p dedup-desktop-core --test cross_phase2 --locked -- --test-threads=1`：通过，3 项通过。
- `cargo fmt --all -- --check`：通过。
- `git diff --check`：通过；仅有现有 Windows 行尾转换提示，无空白错误。

构建仍会报告基线中已有的未使用导入/测试辅助项警告；本任务未扩大清理范围。
