# Task 7B0：多在途任务与同身份 Hash→Media 续算

## 范围

本次只收敛瞬态 TSV 文件所有者和 dispatcher 的并发/续算边界，未接入
BaseCompute、actor、NodeStore、SQLite、协议、WorkerPool 或真实媒体。读取并发的最终
硬上限仍由唯一 `DiskReadScheduler` 的 global/per-disk permit 执行，TaskFileSet 不把
`per_disk_limit` 当作 ACK 在途数量上限。

## TDD 证据

首轮先加入行为测试，再运行旧接口：

- `transient_task_files` 因缺少 `abandon_in_flight` 以编译错误退出（exit 101）；
- `task_dispatch` 因缺少同身份 Media 续算、`is_continuation` 和 dispatcher abandon 接口
  以编译错误退出（exit 101）。

实现后新增的多在途、refill、乱序 ACK、scheduler 真实额度和续算行为均通过。新增
`refill_continues_while_another_identity_is_inflight` 使用 `limit=1`、4 行任务，连续
领取前三项，证明前项尚未 ACK 时仍可从已发布 TSV 继续补入窗口；旧的单
`in_flight` 早退路径不能完成该序列。

## 实现边界

- `LaneState.in_flight` 改为按完整 `TaskFileIdentity` 保存的集合；领取不再被单个在途
  项阻塞，`mark_completed`/`mark_failed` 只移除精确身份，`all_terminal` 和 `discard`
  要求全集为空。
- 预读窗口仍为 `max(2, per_disk_limit * 2)`；它只限制文件层预读内存，实际同盘活跃
  读取由 scheduler permit 控制。测试以真实 `DiskReadScheduler` 验证前五项可持有 permit、
  第六项等待释放，而不是由 TaskFileSet 拒绝第六项。
- `request_media_continuation` 校验原始 run/lane/item/offset/length、Base/`needs_md5`、
  原始 `P` 行和完整记录；不改 TSV、不追加第二行，通过同一 provider 申请
  `MediaDecode`，返回同一 identity/record 并以 `is_continuation()` 区分。
- 同 lane 仍最多保留一个等待 scheduler 的 future；登记的续算优先普通队首。provider
  失败保留续算意图以便重试，取消只清理等待 future、保持 `P`；有等待续算或 permit 时
  拒绝终态 ACK，取消收束后可用精确 `abandon_in_flight` 清理内存而不写 `F`。
- 仅修改 `crates/node-engine/src/task_files.rs`、`task_dispatch.rs` 及对应两个行为测试；
  没有复制 scheduler 的权重、亏欠、active 或老化状态。

## 验证

所有 Cargo 命令使用 `C:\tmp\rust-v2-core-scope-target`，关闭增量/debug 信息并清除
MinGW 编译环境变量。执行期间未触碰 `I:\Tool`、未运行真实媒体、未打包或部署。

| 命令 | 结果 |
|---|---:|
| `cargo test -p dedup-node-engine --test transient_task_files --locked -- --test-threads=1` | 25/25 通过 |
| `cargo test -p dedup-node-engine --test task_dispatch --locked -- --test-threads=1` | 18/18 通过 |
| `cargo test -p dedup-node-engine --test disk_scheduler --locked -- --test-threads=1` | 42/42 通过 |
| `cargo test -p dedup-node-engine --lib --locked -- --test-threads=1` | 66/66 通过 |
| `cargo fmt --all -- --check` | 通过 |
| `git diff --check` | 通过 |

## 后续风险

本提交只提供 Task 7B 后续迁移所需的文件调度 substrate；BaseCompute 仍需在后续任务中
接入 Hash→Media 内存上下文、SQLite ACK 和成功/失败/取消收尾。调度测试使用受控
provider 与真实 scheduler，不能替代双物理盘真实媒体验收。父任务的 NodeStore Task 7A
改动与本次文件边界分开提交。
