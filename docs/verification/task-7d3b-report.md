# Task7D3B 瞬态扫描运行时切换验证报告

## 范围

本次只把 Node 基础扫描切换为当前进程内的瞬态运行任务。扫描仍使用现有枚举、按物理盘冻结的读取计划、任务文件、WorkerPool、SQLite 单写者和最终清单收尾；没有改 Desktop、Stage2、协议、分析或删除语义。

主要约束如下：

- `CreateScan` 只生成 `TaskId` 和 `RuntimeTaskRegistry` 项，不写入 `tasks`、`task_items` 或 `task_stages`。
- 扫描身份使用 `JobIdentity::TransientScan`。取消、失败和关机不调用旧持久任务的取消、失败、完成或阶段写入接口。
- 生产运行目录固定为 `data_path/runtime/<run_id>`，不与缓存目录混用；枚举前先冻结 `ScanDiskPlan`，并把同一个 `ScheduledFileReader` clone 同时用于读取许可和 Hash 读取。
- 只有任务文件 runner 完成 SQLite 收尾、清理 run 目录并返回后，actor 才保存最近成功扫描快照；随后才发布 RuntimeTask Completed。失败和取消不覆盖最近成功快照。
- Worker 事件仅旁路投影到既有 `RuntimeTaskReporter`，遥测失败不改变任务文件主状态机，也不会跳过 dispatcher、writer 或 Store actor 清理。

## 已验证证据

以下验证均使用既有隔离 target `C:\tmp\rust-v2-core-scope-target-task7b2d2c1`，顺序执行且未触碰生产目录：

| 验证项 | 结果 |
| --- | ---: |
| 新增瞬态扫描行为测试 | 1/1 PASS |
| 成功终态晚于最新扫描快照测试 | 1/1 PASS |
| `node-engine` `actor::tests` | 12/12 PASS |
| `node-engine` `--features test-hooks --lib` | 122/122 PASS |
| `base_compute_pipeline` | 59/59 PASS |
| `desktop-core` `controller_runtime_tasks` | 9/9 PASS |
| 指定 `cross_phase2` | 2/3 PASS |

`cross_phase2` 唯一失败项为 `node_cache_is_republished_without_worker_computation`，表现为 `task.outbox_high_seq > before`。该项属于 Task8 Stage2 的缓存重新发布语义，不属于本次 Task7D3B Node actor 瞬态扫描切换，未纳入本提交修复。

## 静态检查与边界

- 指定 Node 文件已通过 Rust 2024 `rustfmt`。
- `git diff --check` 通过。
- 未执行真实媒体跑测、打包、部署或 `I:\Tool` 操作。
- 共享工作树中的 Desktop 并行修改未纳入本提交。
