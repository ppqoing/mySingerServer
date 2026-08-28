# Task 7B2D2B1：封闭任务文件的 Hash 批处理阶段

## 范围

本次新增独立的 `task_file_base_compute` 阶段，只接收已经 seal 的
`BaseTaskProduction`，处理需要 MD5 的任务行、按 ContentKey 批量查询 SQLite，
并把结果交给 taskless 持久化 actor。完整缓存命中在 ACK 后转为 `C`，单文件读取
失败在 ACK 后转为 `F`，基础缓存不完整的行保持原身份和 `P`，交给后续 Media
阶段。没有接入 Worker、actor 主循环、任务恢复或 finalize。

## 实现

- 使用 `TaskDispatchAdmission::hash_only()`；已知 MD5/Media 行不申请 Hash provider。
- dispatcher 已取得的 permit 直接传给 `read_with_permit`，不二次 acquire；每个读取
  future 完成时立即释放 permit，缓存查询和 ACK 不占用读取额度。
- Hash 事件循环以显式 `hash_capacity` 限制 `JoinSet` 在途读取数，同时轮询 dispatcher
  和已完成的读取结果，避免等待下一 permit 时阻塞已经完成的文件。
- pending 显式返回 `remaining_hash_rows` 与 `blocked_reason`；例如同 lane 的 Media
  队首会返回 `MediaPending` 和剩余 Hash 数量，不把被阻塞误报为完成。
- ContentKey 查询按最多 1000 项分批，保持 Hash 结果与查询返回顺序一致。
- 完整缓存通过 `upsert_content_and_location` 生成 taskless persist ACK；只有 ACK 成功
  后才写任务文件 `C`、移除上下文并增加 resolved/cache-hit 统计。
- 不完整缓存通过同一 `TaskFileIdentity` 登记 Media continuation，仍保持 `P`。
- 读取失败生成 Failed persist；ACK 后才写 `F`，并继续处理后续行。
- Store、ACK 身份、持久化 actor 关闭或 dispatcher 错误均返回带所有权的 pending，
  由调用方决定 discard。
- 测试专用窄 observer 只记录 ContentKey 批次大小，不改变生产结构。
- `actor.rs` 仅补充既有 test-hooks 测试所缺的 `NormalizedPath` import；不改变生产逻辑，
  用于让本任务要求的 feature 聚焦测试可编译。

生产实现约 604 行，行为测试约 631 行；Media Worker 尚未接入。

## TDD 与验证

初始实现的首个聚焦测试暴露了最后一项 Hash 读取完成后仍向 dispatcher 等待下一队首
导致的挂起。根因为 Hash 行仍为 `P` 等待 ACK，而 dispatcher 此时没有可发布队首，
不会产生新的 publication 通知。修复为按 identity 的 `needs_md5` 计数控制 Hash 阶段，
Hash 耗尽后立即进入 ACK 收束，未改变任务行的 ACK 门禁。

本轮先加入两个 RED：两个可用额度的 Hash 必须在任一释放前同时进入读取；Media 队首
阻塞同 lane 后必须保留 `remaining_hash_rows=1` 并报告 `MediaPending`。随后采用
`JoinSet + tokio::select!` 最小实现：dispatcher 只负责交付 permit，在途读取独立推进，
读取完成即释放 permit；结果按 dispatcher 序号恢复后继续最多 1000 项一次的 ContentKey
批量查询。

验证均使用 `CARGO_TARGET_DIR=C:\\tmp\\rust-v2-core-scope-target`、清除 CC/CXX/
AR/RANLIB/CFLAGS/CXXFLAGS/RUSTFLAGS/RUSTC_WRAPPER，并串行运行：

| 验证 | 结果 |
|---|---:|
| `cargo test -p dedup-node-engine --lib task_file_base_compute --features test-hooks --locked -- --test-threads=1` | 8/8 通过 |
| `cargo test -p dedup-node-engine --test base_task_producer --locked -- --test-threads=1` | 14/14 通过 |
| `cargo test -p dedup-node-engine --test task_dispatch --locked -- --test-threads=1` | 27/27 通过 |
| `cargo test -p dedup-node-engine --test pipeline_permit --locked -- --test-threads=1` | 6/6 通过（前序验证） |
| `cargo test -p dedup-node-engine --lib base_persistence --features test-hooks --locked -- --test-threads=1` | 5/5 通过（前序验证） |
| `cargo test -p dedup-node-engine --test transient_task_files --locked -- --test-threads=1` | 25/25 通过（前序验证） |
| `cargo test -p dedup-node-engine --lib --features test-hooks --locked -- --test-threads=1` | 80/80 通过 |
| `cargo fmt --all`、`git diff --check` | 通过 |

测试期间未修改 `I:\\Tool`、生产目录或 SQLite schema；未执行真实媒体、打包和部署。
最后一次测试后 C/D 可用空间约为 12.34/11.96 GiB，均未触发 10 GiB 停止线。

## 后续边界

后续阶段需要把 pending 所有权接到实际 Media Worker 和主 actor；本提交不宣称基础
计算端到端已接通。
