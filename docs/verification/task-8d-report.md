# Task8D：当前进程任务查询协议切换记录

## 范围

本步骤只调整 `node-engine` actor 的任务查询边界，不增加 `TaskCatalog`，不实现任务恢复，
不修改 Worker、扫描 task-file、NodeStore 特征写入或 Desktop。

## 目标行为

- `QueryTask` 只读取当前进程 `RuntimeTaskRegistry`。运行中的任务返回运行态；最近一次成功
  扫描由 `latest_completed_scan` 提供完成态和真实 `outbox_high_seq`；旧进程、取消/失败任务
  或非最近完成扫描返回 `NotFound`。
- `ListTasks` 只投影当前进程 registry，继续保留原协议游标和分页字段，不读取 SQLite
  `tasks` 表。
- `PrepareAnalysisInput` 必须只接收一个且正好等于最近一次完成扫描的任务 ID；输入从
  `CompletedScanSnapshot.resolved_files` 稳定排序、去重后，再用 SQLite 基础缓存批量查询
  内容类型和特征完整性。请求不再读取 `analysis_inputs`、`tasks` 或 `task_items`。

## TDD 证据

已在 `crates/node-engine/src/actor.rs` 增加真实 actor 行为断言：

- 运行中的瞬态扫描通过 `QueryTask/ListTasks` 返回 `TaskRunning`；
- 取消后的旧任务 ID 返回 `NotFound`；
- 成功事件观察到的最新扫描可通过 `QueryTask` 返回真实 outbox 高水位；
- 最新扫描可生成分析输入，随机旧 ID 返回 `NotFound`。

本轮在共享 target 空闲后实际执行了以下定向验证（清理外部编译器环境变量，单线程）：

- `cargo test -p dedup-node-engine --lib scan_create_query_and_cancel_stay_responsive_while_worker_is_held --locked -- --test-threads=1`
  通过，1 passed，116 filtered out。
- 完成态夹具最初漏发 `BaseSourceReadComplete`，产品按协议把提前到达的 Worker 终态记为文件失败，
  因而快照输入为空。补齐该真实事件顺序后未修改产品逻辑，也未放宽断言。
- `cargo test -p dedup-node-engine --features test-hooks --lib --locked -- --test-threads=1`：
  128/128 通过；其中完成态测试同时验证最新快照、真实 `outbox_high_seq`、批量分析输入和旧 ID 拒绝。

编辑阶段已执行 `rustfmt --edition 2024 --check crates/node-engine/src/actor.rs` 和
`git diff --check`，均通过。

## 实现边界

协议字段保持不变。运行摘要转换为旧 `TaskSummary` 只在 actor 内完成，未复制第二份任务
事实源；`outbox_high_seq` 仅绑定当前最新完成扫描快照，其余当前进程运行任务不伪造高水位。
