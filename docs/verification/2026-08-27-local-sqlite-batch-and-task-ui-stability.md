# 本地 SQLite 批量查询与任务界面稳定性验证

日期：2026-08-27

## 结论

- 路径缓存和内容缓存已改为真正的 SQLite 批量查询，不再由 Node Engine 对每个任务项分别执行本地缓存查询。
- 1000 个输入的路径批次固定执行 2 条业务 `SELECT`；1000 个内容键批次同样固定执行 2 条业务 `SELECT`。
- SQLite 变量上限为 7 时，5 个路径输入被拆成 3 个子批、共执行 6 条业务 `SELECT`；变量上限不足一个请求时明确失败，不退回逐项查询。
- 内容查询结果由本地游标按 Hash 队首消费。解码背压期间保留游标和 Hash 项，不重复查询同一批 SQLite 记录。
- UI 的任务列表和运行中计数只由 `RuntimeTasksChanged` 写入；`ViewChanged` 不再使用旧任务快照覆盖它们。
- 控制器启动时先发布空的统一运行任务快照，再发布普通视图快照。

## RED 证据

1. NodeStore 旧实现缺少 `lookup_base_cache_by_paths` 和 `lookup_base_cache_by_keys`，新增契约测试在编译阶段失败；旧路径对 1000 个输入观察到 1000 次业务查询。
2. Node Engine 内容游标测试在旧实现上因缺少 `install_local_content_lookup_observer_for_test` 失败；旧主循环仍使用 `content_id_by_key` 和 `load_base_cache_record` 逐项回查。
3. UI 真实 `MainWindow` 事件交错测试在旧实现上失败：`RuntimeTasksChanged` 后再到达 `ViewChanged`，`running_count` 从预期 1 被覆盖为 0。
4. Desktop Core 启动顺序测试在旧实现上失败：第一条事件实际为 `ViewChanged`，而不是统一任务快照。

## GREEN 与回归证据

所有 Rust 命令均在当前 PowerShell 进程移除全局 `CC` 后运行，避免 MinGW `gcc` 污染 MSVC 链接。

| 范围 | 命令摘要 | 结果 |
| --- | --- | --- |
| NodeStore 全量 | `cargo test -p dedup-node-store --locked -- --test-threads=1` | 43/43 通过 |
| Node Engine 基础计算管线 | `cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1` | 59/59 通过 |
| Node Engine 库测试 | `cargo test -p dedup-node-engine --features test-hooks --lib --locked -- --test-threads=1` | 60/60 通过 |
| Desktop Core 任务控制器 | `cargo test -p dedup-desktop-core --test controller_runtime_tasks --locked -- --test-threads=1` | 6/6 通过 |
| Desktop UI 绑定契约 | `cargo test -p dedup-desktop-ui --test bindings_contract --locked -- --test-threads=1` | 16/16 通过 |
| 内容游标定向背压 | `local_compute_candidate_waits_without_consuming_context_when_credit_is_full` | 1/1 通过，3 个候选仅 1 次本地批量查询 |
| Node Engine 生产配置 | `cargo check -p dedup-node-engine --locked` | 通过，无 `test-hooks` |

`cargo fmt --all` 和 `git diff --check` 已通过。

## 独立终审

按要求使用 `gpt-5.6-sol`、`max` 对当前修复做只读终审，结论为 `REVIEW_CLEAN`，没有阻断项或真实功能缺陷。终审额外复验 NodeStore 内容缓存 11/11、批量 SQL/变量上限 3/3、Node Engine 本地游标背压 1/1、Desktop Core 启动顺序 1/1、Desktop UI 事件交错 1/1，均通过。

## 边界

- 本次验证针对本地 SQLite 批量查询、Node Engine 批量游标和 UI 单一任务数据源。
- 未运行 30 分钟 A/B 性能门禁；该门禁不属于本次界面闪烁和逐项 SQLite 查询修复的完成条件。
- 未打包、未部署，也未修改 `I:\Tool`。
