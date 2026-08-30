# Streaming Hash/Media 基础流水线验证记录

## 范围与结论

本记录验证 2026-08-30 的 Hash→ContentKey→Media 流式基础计算收口，以及同 lane 未 ACK
身份唯一的 dispatcher 契约。验证工作树为
`D:\code\mySingerServer\.worktrees\core-scope-transient-runtime`，Cargo target 为
`C:\tmp\rust-v2-core-scope-target-task7b2d2c1`。所有 Cargo 命令清空
`CC/CXX/AR/RANLIB/CFLAGS/CXXFLAGS/RUSTFLAGS/RUSTC_WRAPPER`，并设置
`CARGO_INCREMENTAL=0`、dev/test debug=0。

结论：自动化行为门禁与固定微基准均通过。三轮基准中位数比历史中位数慢 8.149%，未超过
15% 退化门槛。C/D 盘在每条重型命令前均大于 10 GiB，因此没有清理 target。

本记录不代表真实媒体、最终打包或部署验收：本轮没有运行真实媒体、没有构建/检查安装包，
也没有部署到任何环境。

## 旧测试债务：RED→GREEN

旧测试 `one_lane_dispatches_two_permits_before_either_ack` 的期望是同一 lane 在首项 ACK 前交付
第二个不同身份。对当前“每 lane 一个未 ACK identity”实现运行后，该断言 RED；旧的第六项同
lane scheduler 测试也会在此契约下等待。该 RED 是过时期望，不是把产品改回多 owner。

测试改为 `one_lane_waits_for_ack_before_delivering_next_identity`：首项交付后第二项保持 Pending，
只有 `mark_completed` 清除首项 in-flight 后才交付。原第六项额度覆盖未删除，改为六个独立
dispatcher lane 共用同一真实物理盘 scheduler：前五项取得额度，第六项等待，释放前五项后才
继续。提交：`a01a3204640cd2181dd807446dd8d610321fb5b3`
(`test: align dispatcher lane ownership`)。

## 行为覆盖

- `first_hashed_media_miss_enters_worker_before_later_hash_finishes` 对两次 Hash 完成分别断言
  `lookup_key_batch_sizes_for_test() == [1, 1]`：每次 SQLite ContentKey 查询大小都是 1。
- 同一测试在第二个 Hash 查询完成时首个 Media Worker 仍 active，证明没有 Hash/Media 全批
  drain；`active_media_does_not_block_later_hash_on_another_lane` 覆盖另一物理盘 Hash 继续推进。
- 远端路径的 `remote_success_in_flight_after_failure_stays_local_only` 同样验证两个独立单键请求，
  并在远端故障后保持 local-only。
- `cancellation_drains_hash_remote_media_and_preserves_unacked_rows`、
  `cancellation_returns_pending_owner_without_acknowledging_rows` 与基础流水线取消用例覆盖取消后
  Hash、远端、Media、Worker、ACK owner 收束为零且 TSV `P` 行不被伪 ACK。
- `no_output_credit_keeps_third_item_unclaimed_with_capacity_two` 验证容量为二时第三项不能越过
  scheduler/output-credit 限流；`scheduler_holds_sixth_independent_lane_on_shared_physical_disk`
  验证共享物理盘的真实 scheduler 上限。

扫描前路径缓存仍使用最多 1000 项的批量 `lookup_base_cache_by_paths`；这和 Hash 后单键
ContentKey 查询是不同边界，未被本次改动混同。

## 固定微基准

串行独立执行三次：

```powershell
cargo bench -p dedup-node-engine --bench base_compute_pipeline --locked
```

| 轮次 | elapsed_ms | throughput_files_per_second | persisted_completed |
| --- | ---: | ---: | --- |
| 1 | 138.397 | 28.902 | true |
| 2 | 125.394 | 31.899 | true |
| 3 | 116.911 | 34.214 | true |

中位数为 `125.394 ms`；历史中位数为 `115.946 ms`；退化率为 `8.149%`，低于
`15%`（`133.3379 ms`）停止阈值。此结论只使用端到端 `elapsed_ms`，未以局部分段代替。

- 基准 EXE：`C:\tmp\rust-v2-core-scope-target-task7b2d2c1\x86_64-pc-windows-msvc\release\deps\base_compute_pipeline-fc0c73da15de0b6f.exe`
- SHA-256：`08EC930367AF8DDDAF537AC55A2CE2A2B944BF506077DFF30A361EEB75DDFF12`
- 原始输出（已忽略）：`.superpowers/sdd/2026-08-30-streaming-hash-media-pipeline/task-5-bench-run-{1,2,3}.log`

## 最终自动化门禁

命令严格串行、均使用 `--test-threads=1` 且退出 0：

| 命令 | 通过数 | 编译 warnings |
| --- | ---: | ---: |
| `cargo test -p dedup-node-store --locked -- --test-threads=1` | 80 | 0 |
| `cargo test -p dedup-node-engine --lib --locked -- --test-threads=1` | 133 | 23 |
| `cargo test -p dedup-node-engine --features test-hooks --lib --locked -- --test-threads=1` | 153 | 12 |
| `cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1` | 60 | 20 |
| `cargo test -p dedup-node-engine --features test-hooks --test transient_task_files --locked -- --test-threads=1` | 25 | 20 |
| `cargo test -p dedup-node-engine --test disk_scheduler --locked -- --test-threads=1` | 42 | 17 |
| `cargo test -p dedup-node-engine --test scan_runtime_details --locked -- --test-threads=1` | 11 | 17 |
| `cargo test -p dedup-node-engine --test task_dispatch --locked -- --test-threads=1` | 27 | 17 |

合计 `531 passed; 0 failed; 0 ignored`。warnings 均是现存的 unused import/dead code
编译警告；本次没有把 warnings 当作通过条件的一部分，也没有为消警改动无关实现。

最后执行 `cargo fmt --all -- --check` 和 `git diff --check`，两者均退出 0。

## 验收边界

上述为受控单元/集成行为测试和固定 4 文件微基准：可证明单键查询、流式推进、owner 收束、
调度额度与此机器上的固定耗时。它不能证明 FFmpeg 对真实媒体的解码结果、最终便携包内容、
安装/升级路径或部署环境表现；这些验收本轮明确未执行。
