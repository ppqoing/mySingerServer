# Task 7D3C：Desktop 运行任务单一事实源

## 范围

本步只调整 `dedup-desktop-core` 的扫描任务控制路径：当前连接和当前进程中的
`RuntimeTaskControllerState`/RuntimeTask 快照是任务中心、运行数量和任务详情的唯一来源。
不新增 `TaskCatalog`、任务恢复、历史任务、分页或 Node analysis 结果；跨机器分析仍保留
所需的持久任务查询，留给 Task8 后续适配。

## 实现

- `CreateScan` 成功后只接受 Node 的接受响应，立即刷新当前连接的 RuntimeTask 列表；不再
  调用旧的 `QueryTask`，也不把持久任务摘要写入 `DesktopViewState` 的旧任务集合。
- `refresh_nodes` 只刷新节点连接状态和同步高水位，不再调用 `ListTasks`，因此旧的持久
  扫描列表不会覆盖运行任务快照。
- Node 的 RuntimeTask 终态事件继续立即刷新运行任务列表；`completed` 事件直接触发自动
  同步，替代原先从 `ListTasks` 推断“首次完成”的逻辑。连接成功和固定追赶同步保持不变。
- ViewChanged 与 RuntimeTasksChanged 继续分开发布；本步没有增加第二个任务状态写入者。
- `DesktopViewState` 中的旧 `tasks` 字段暂保留给已有兼容性视图测试和未迁移接口，但生产
  Desktop 控制循环不再写入它。跨机器分析 `start`/`refresh_tasks` 的 `query_task` 仍用于
  校验已选持久扫描任务及其 `outbox_high_seq`，明确列为 Task8 待适配边界。

## TDD 与验证

先以真实 TCP NodeServer 夹具固定旧行为：

- `create_scan_uses_runtime_task_snapshot_without_querying_legacy_task` 在旧实现中真实失败，
  命中 `QueryTask`（调用次数 1，期望 0）。
- `refresh_does_not_read_legacy_task_list_or_overwrite_runtime_snapshot` 用旧任务列表探针
  和 RuntimeTask 列表探针验证刷新边界。

修复后使用 `C:\tmp\rust-v2-core-scope-target-task7b2d2c1` 运行：

- `cargo test -p dedup-desktop-core --test controller_runtime_tasks --locked -- --test-threads=1`：9/9 通过。
- `cargo test -p dedup-desktop-core --locked -- --test-threads=1`：已执行；app、controller、
  reconnect、runtime task、cross-analysis 等已执行项目通过，既有
  `cross_phase2::node_cache_is_republished_without_worker_computation` 在共享 Node actor
  未提交改动下失败于 `task.outbox_high_seq > before`，不经过本步 Desktop app 代码，未越界修复。
- `rustfmt --edition 2024 --check crates/desktop-core/src/app.rs
  crates/desktop-core/tests/controller_runtime_tasks.rs`：通过。
- `git diff --check`（仅本步文件）：通过。

未运行真实媒体、未打包、未部署、未触碰 `I:\Tool`。共享工作树中的 Node actor、scan
导出和 pipeline 改动不属于本步，也不会随本步提交。

## 后续边界

Task8 再处理跨机器分析对已完成扫描的持久任务校验、重启/历史语义和其它非当前运行态
需求；本步不把这些持久查询伪装成 Desktop 运行任务列表，也不新增恢复层。
