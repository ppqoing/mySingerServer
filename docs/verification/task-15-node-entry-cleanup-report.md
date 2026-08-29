# Task15 Node 入口收口验证报告

## 范围

本次只收口 Node 入口的瞬态任务边界，不改 `analysis_runtime_details.rs`、Windows
脚本和 `desktop-core`。运行任务只存在于当前进程；旧 SQLite 任务、分组和复核历史
保留为兼容存储，但不再作为 Node 入口的活动或结果来源。

## 实施结果

1. 启动清理继续精确删除 `data/runtime`，并删除 `results/latest-analysis.partial.tsv`；
   `latest-analysis.result.tsv` 保留，其它 results 文件不触碰。
2. `RuntimeTaskRegistry::activity_counts` 定义并固定 NodeStatus 规则：registry 不建立
   queued 项状态，`queued_items=0`；每个当前进程非终态任务计一个 `running_items`；
   Completed、Failed、Cancelled 均不计。NodeStatus 不再查询 SQLite `task_items`。
3. `CancelTask` 只接受当前 `active_job` 中与请求 ID 完全匹配的瞬态扫描、二筛或本地分析；
   未知、旧 SQLite、非当前任务和已结束 ID 返回 NotFound，不调用 `store.cancel_task`。
4. `ListGroups` 与 `ListGroupMembers` 直接拒绝旧 SQLite 分组/复核查询，错误信息指向
   `ReadLocalResultWindow`。旧表中已有历史数据也不会返回。
5. `NodeRuntime::start_inner` 与 `NodeEngine::spawn` 均传入
   `disk_full_cleaner=None`。测试工厂仍可显式注入兼容清理器，主缓存 artifact registry
   保留；生产写满按普通写入错误处理。

## TDD 证据

### RED

- `runtime_tasks::activity_counts_use_only_current_non_terminal_runtime_tasks`：旧实现
  无 `activity_counts`，编译失败；新增规则后通过。
- `node_status_uses_current_runtime_registry_not_legacy_task_items`：旧实现读取旧 SQLite
  running 项，实际 `running_items=1`，期望为 0。
- `cancel_task_rejects_legacy_persistent_id_without_mutation`：旧实现返回成功并取消旧
  SQLite 任务，期望为 NotFound。
- `legacy_group_queries_are_rejected_in_favor_of_local_result_window`：旧实现返回已写入
  SQLite 的历史组页，期望为拒绝并指向本地结果窗口。
- 启动清理测试先以 partial 文件残留运行，旧实现未删除该文件，测试失败。

### GREEN

- `cargo test -p dedup-node-engine --features test-hooks --test runtime_tasks --locked -- --test-threads=1`
  - 19 passed。
- `cargo test -p dedup-node-engine --features test-hooks --test node_actor --locked -- --test-threads=1`
  - 9 passed。
- `cargo test -p dedup-node-engine --features test-hooks --lib --locked -- --test-threads=1`
  - 156 passed。
- `cargo test -p dedup-node-engine --features test-hooks --test node_server --locked -- --test-threads=1`
  - 3 passed。
- `rustfmt --edition 2024 --check`（4 个 Node 文件）、`cargo fmt --all -- --check`、
  `git diff --check` 均通过。

默认 feature 的 `cargo test -p dedup-node-engine --lib --locked` 仍被共享工作树中
已有的 `analysis/phase2.rs` 对 test-hooks 专用 `BasePersistTestController` 的条件编译
错误阻塞；本任务未改该文件。

## 兼容残留

`NodeStore::task_activity_counts`、`cancel_task`、旧分组/复核查询方法、
`DiskFullCleaner` 模块及测试注入工厂仍保留，供兼容 API 和已有底层测试使用；Node
生产入口不再调用这些旧任务/结果接口，也不启用磁盘满清理。

