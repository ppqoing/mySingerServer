# Task 7B2D2B0：TaskFileDispatcher 阶段 admission

## 范围

本次只为 `TaskFileDispatcher` 增加 Hash/Media 阶段 admission，不接入 Store、Worker、Actor，
也不复制 `DiskReadScheduler` 的公平状态。默认 `next/poll_next` 继续同时允许 Hash 与 Media，
保持已有调用行为。

## TDD 证据

先在基线 `7b5ebdef6888368d5cc3797b725676ad6bebe111` 加入真实 dispatcher 行为测试，执行
`cargo test -p dedup-node-engine --test task_dispatch --locked -- --test-threads=1`，旧实现因缺少
`TaskDispatchAdmission`、`TaskDispatchPoll` 和 admission 轮询接口而编译失败，确认测试门禁有效。

实现后 `task_dispatch` 全量 26/26 通过，覆盖：

- Hash-only 不为已知 MD5 的 Media 行申请 permit，并明确返回 `Blocked(MediaPending)`；
- Hash-only 正常派发 `needs_md5` 行；
- 多 lane 先派发 Hash，随后在 Media 仍为 `P` 时立即 Blocked，provider 不申请 Media；
- 默认 `next` 仍能派发 Media；
- 从禁止 Media 切换为允许 Media 时，原 `P` 行按同一身份正常重试；
- admission 切换会丢弃被禁止的等待 future，但不重复启动，且切回后可重新派发；
- 取消只收掉等待请求，任务文件状态继续保持 `P`。

## 实现结果

- 新增 `TaskDispatchAdmission`，提供 `all/hash_only/media_only` 构造入口，仅表达本轮允许的类别。
- 新增 `TaskDispatchPoll::{Task, Drained, Blocked}` 与 `TaskDispatchBlockReason::{HashPending, MediaPending}`，
  将 admission 阻塞与取消、读取错误、全部终态区分开。
- 新增 `next_with_admission` 和 `poll_next_with_admission`；旧 `next/poll_next` 委托默认全类别 admission。
- `start_lane_requests` 只为允许类别创建 provider future；已有允许 future 不取消；类别切换后禁止的
  future 被安全丢弃，任务行不从 `P` 弹出。
- 任务文件封闭且无可派发的允许类别时，扫描冻结队首和续算意图并立即返回明确 Blocked，避免永久等待
  或忙循环；真实 permit 仍完全由现有 scheduler 管理。
- `TransientTaskFileSet` 增加生产端全局封闭状态查询，供 dispatcher 做上述边界判断。

## 修改文件

- `crates/node-engine/src/task_dispatch.rs`
- `crates/node-engine/src/task_files.rs`
- `crates/node-engine/tests/task_dispatch.rs`

## 验证

Cargo 使用 `CARGO_TARGET_DIR=C:\tmp\rust-v2-core-scope-target`，关闭增量/debug 信息并清除
CC/CXX/AR/RANLIB/CFLAGS/CXXFLAGS/RUSTFLAGS/RUSTC_WRAPPER。期间 D 盘短暂低于 10 GiB，已暂停重型命令，
仅清理本工作树可再生缓存 `D:\code\mySingerServer\.worktrees\core-scope-transient-runtime\target`，
未清理源代码或证据。

| 验证 | 结果 |
|---|---:|
| `cargo test -p dedup-node-engine --test task_dispatch --locked -- --test-threads=1` | 26/26 通过 |
| `cargo test -p dedup-node-engine --test transient_task_files --locked -- --test-threads=1` | 25/25 通过 |
| `cargo test -p dedup-node-engine --test disk_scheduler --locked -- --test-threads=1` | 42/42 通过 |
| `cargo fmt --all -- --check` | 通过 |
| `git diff --check` | 通过 |

构建仍只有 B2B 尚未接入主循环导致的既有 dead-code 警告；没有测试失败或运行时错误。

## 后续边界

本次未把 admission 接到 BaseCompute 主循环，未改变 scheduler 公平算法、permit 所有权、SQLite、Worker、
任务恢复、真实媒体、打包或部署。主循环接入由后续 Task7B2D 负责。
