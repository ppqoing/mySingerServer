# Task 7B2B 实施报告：基础持久化身份接缝

## 结果

Task 7B2B 已完成。本次只扩展 BaseStoreActor 的拥有型消息与 ACK 身份边界，保留旧
`TaskItemIdentity` 构造调用，同时增加瞬态 TSV 任务文件的完整 `TaskFileIdentity` 路径。
消息进入有界队列、SQLite actor 执行以及 ACK 返回时，任务文件身份的 run、lane、行偏移、
行长度、item 和缺失掩码均原样保留。

## TDD 证据

先加入真实 `TransientTaskFileSet` 身份经过 actor ACK 的行为测试，在未实现新接缝时执行：

```text
cargo test -p dedup-node-engine --lib task_file_persist_ack_preserves_full_identity --locked -- --test-threads=1
exit 1
```

旧实现准确暴露 `new_task_file`、`BasePersistIdentity` 及其访问器缺失，ACK 仍只能承载
`TaskItemIdentity`。随后完成最小实现并通过：

- `base_persistence::tests`：5/5。
- `cargo check -p dedup-node-engine --locked`：通过。
- `rustfmt --edition 2024 --check`（本次两个 Rust 文件）：通过。
- `git diff --check`：通过。

## 已覆盖行为

- `BasePersistIdentity::Legacy(TaskItemIdentity)` 保留既有 `BasePersistMessage::new` 调用。
- `BasePersistIdentity::TaskFile(TaskFileIdentity)` 由明确的 `new_task_file` 构造，不合成
  任务表身份，也不只保存 item ID。
- `item_id`、`task_id`、`task_file_identity` 和 `into_task_file` 提供兼容日志、旧运行时 map
  以及新任务文件提交所需的访问边界。
- 真实任务文件行经 `BaseStoreActor` 后 ACK 的完整身份逐字段相等。
- persist 队列 `Full` 和 `Closed` 归还的原消息仍带完整任务文件身份。
- `queue_wait`、`transaction_elapsed` 和 `BasePersistOutcome` 语义未改变。
- 旧 BaseCompute 仅把身份字段访问改为 `item_id()`，Legacy 行为保持不变；旧 ACK 测试继续
  使用 `Legacy` 变体。

## 实现边界

- 修改 `crates/node-engine/src/scan/base_persistence.rs`：增加拥有型身份枚举、双构造器、
  访问器，以及 actor/队列行为测试。
- 修改 `crates/node-engine/src/scan/base_compute.rs`：仅适配 ACK/message 的 `item_id()` 和
  可选 `task_id()` 访问，未迁移生产 BaseCompute。
- 未修改 NodeStore、pipeline、worker、actor 调度、任务恢复、TaskCatalog、JSON、持久任务表、
  协议或 UI。

## 验证环境与风险

所有 Cargo 命令使用 `C:\tmp\rust-v2-core-scope-target`，并关闭增量/debug 信息、清除了
CC/CXX/AR/RANLIB/CFLAGS/CXXFLAGS/RUSTFLAGS/RUSTC_WRAPPER。执行期间 C/D 盘可用空间约
14.75/12.35 GiB，未触发 10 GiB 停止线。

`TaskFile` 构造器和完整身份访问器在生产构建中暂时显示 dead-code 警告，这是因为 Task7B2
尚未把生产 BaseCompute 迁移到任务文件路径；不影响 Legacy 编译和现有行为。后续接入时必须
让 persist ACK 直接携带同一 `TaskFileIdentity`，不得重新构造 `TaskItemIdentity`。
