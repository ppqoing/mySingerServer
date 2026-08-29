# Task7B2D2C3：基础任务 Hash/Media 交错协调实施报告

## 范围

本次只增加瞬态基础任务的外层协调边界，不触及 actor、finalize、恢复、TaskCatalog 或任务生命周期持久化。
协调器只创建一次 `TaskFileBaseComputePending`，之后始终传递同一个 pending owner：

- 每轮先执行 Media pass；Media 被 Hash 队首阻塞时保留 `HashPending`，再进入 Hash pass。
- Media 结果必须先经过 `persist_task_file_media_results` 并收到 SQLite ACK，下一轮才允许 Hash。
- Hash 命中缺失内容时请求原任务身份的 Media continuation，下一轮再次由 Media pass 派发，不追加任务文件行。
- 单文件 Worker 失败交给已有持久化边界写入 F，其他文件继续；正常完成只在上下文为空、Hash 行耗尽且 dispatcher 返回 `Drained` 后交回 owner。
- 取消、Worker/Store/dispatcher 错误均返回带精确 pending 的错误；阶段函数已收束读取许可、Worker 和 ACK 后，协调器按上下文解除在途身份，保留未 ACK 行为 P，最终由上层决定 discard。

## TDD 与行为证据

初始 stub 的真实行为测试未能从 `started` 通道观察到 Worker 派发，RED 暴露了协调入口缺失实际阶段驱动的问题。最小实现后新增并通过四项行为测试：

1. `media_is_persisted_before_hash_starts`：首个 Media 的 SQLite persist 被闸门暂停时，Hash 不会开始；ACK 放行后才继续。
2. `hash_creates_one_media_continuation_with_same_identity`：Hash 后 Media 使用原 `item_id`，任务文件仍只有一行并最终写 C。
3. `one_media_failure_does_not_stop_the_next_file`：首项 Worker 崩溃写 F，后项仍派发并写 C。
4. `cancellation_returns_pending_owner_without_acknowledging_rows`：取消返回精确 pending，未 ACK 行保持 P，owner 可安全 discard。

## 验证

使用固定共享 target `C:\tmp\rust-v2-core-scope-target-task7b2d2c1`，并清除继承的 CC/CXX/RUSTFLAGS 等环境变量：

- coordinator（`--features test-hooks`）：4/4 通过。
- coordinator（默认 feature）：3/3 通过。
- `task_file_base_compute`：11/11 通过。
- `task_file_media_compute`：7/7 通过。
- `task_file_media_persistence`：12/12 通过。
- 两个改动 Rust 文件 `rustfmt --edition 2024 --check`：通过。
- `git diff --check`：通过。

首次链接尝试受外部继承编译环境影响出现 `___chkstk_ms`/`__isnan` 解析错误；清除这些环境变量后重新编译并通过，未发现产品源码链接错误。仅有既有 unused/dead-code 警告。

## 变更与边界

变更文件为 `crates/node-engine/src/scan/task_file_base_coordinator.rs` 与 `crates/node-engine/src/scan/mod.rs`。本次未运行真实媒体、未打包、未部署、未触碰 `I:\Tool`；上层仍需在后续接入生产入口并按其生命周期决定正常 owner 的 discard。

验证结束时 C 盘约 18.04 GiB、D 盘约 11.95 GiB 可用。
