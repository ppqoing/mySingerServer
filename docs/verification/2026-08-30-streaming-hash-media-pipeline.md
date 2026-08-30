# Streaming Hash/Media 基础流水线验证记录

## 范围与结论

本记录验证 2026-08-30 的 Hash→ContentKey→Media 流式基础计算收口，以及同 lane 未 ACK
身份唯一的 dispatcher 契约。验证工作树为
`D:\code\mySingerServer\.worktrees\core-scope-transient-runtime`，Cargo target 为
`C:\tmp\rust-v2-core-scope-target-task7b2d2c1`。所有 Cargo 命令清空
`CC/CXX/AR/RANLIB/CFLAGS/CXXFLAGS/RUSTFLAGS/RUSTC_WRAPPER`，并设置
`CARGO_INCREMENTAL=0`、dev/test debug=0。

结论：当前自动化行为门禁通过；九轮固定微基准仍未通过性能门槛。九轮中位数比历史中位数慢
15.161%，超过 15% 退化门槛；因此不把本轮表述为性能成功，也未对性能实现作修改。C/D 盘在
每条重型命令前均大于 10 GiB，因此没有清理 target。

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

## 终审修复纳入范围

本轮纳入提交 `3f66e0667d337f2ab990d1fc3d327134aa3e7deb`
(`fix: preserve streaming error and event ownership`) 的三项修复：

- 基础设施错误不再取消共享用户 token；协调器和 scan runner 保留原始错误，而不误报为用户取消。
- 移除全局 `persist.is_empty()` dispatch gate 和 ACK biased select；一个 lane 的 SQLite ACK 在途时，
  另一 lane 的 Hash 仍可推进，而同 lane 仍由未 ACK identity 约束。
- Media 先由核心状态机判定 Worker 事件，再生成唯一 reporter effect；错误 identity/slot、重复事件和
  已终态事件不会污染 runtime registry，协议失败只投影为失败一次。

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

所有轮次均串行、独立执行：

```powershell
cargo bench -p dedup-node-engine --bench base_compute_pipeline --locked
```

| 轮次 | elapsed_ms | throughput_files_per_second | cache_wait_ms | worker_idle_before_hash_ms | decode_and_persist_ms | persisted_completed |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| 1 | 135.530 | 29.514 | 38.704 | 71.888 | 46.199 | true |
| 2 | 124.282 | 32.185 | 28.652 | 60.373 | 46.777 | true |
| 3 | 133.785 | 29.899 | 36.673 | 69.852 | 46.642 | true |
| 4 | 130.090 | 30.748 | 34.524 | 66.608 | 47.137 | true |
| 5 | 133.524 | 29.957 | 37.220 | 69.322 | 46.690 | true |
| 6 | 135.504 | 29.520 | 38.888 | 72.016 | 47.231 | true |
| 7 | 131.751 | 30.360 | 34.951 | 67.244 | 47.930 | true |
| 8 | 134.609 | 29.716 | 39.505 | 71.207 | 46.068 | true |
| 9 | 122.185 | 32.737 | 25.561 | 58.281 | 46.906 | true |

首组三轮作为 run 1--3 保留，其三样本中位数为 `133.785 ms`，相对历史中位数
`115.946 ms` 退化 `15.386%`，是初始 FAIL。为避免以追加轮次选择性替换该证据，随后预先固定
“当前 HEAD、同一 EXE、共九轮、以全部九轮 `elapsed_ms` 中位数裁决”的规则，并连续完成 run 4--9。
九轮排序后的第五值为 `133.524 ms`，相对历史中位数退化 `15.161%`，仍高于 15% 门槛
`133.3379 ms` `0.186 ms`；九轮最终裁决为 **FAIL**。

裁决始终只使用端到端 `elapsed_ms`，未以局部分段替代。分段仅用于诊断：九轮中位数分别为
`cache_wait_ms=36.673`、`worker_idle_before_hash_ms=69.322`、`decode_and_persist_ms=46.777`；
解码持久化约 46--48 ms 较稳定，等待/空闲阶段约 26--39/58--72 ms 波动。此次不修改性能代码，
保留失败证据供后续独立诊断。

- 基准 EXE：`C:\tmp\rust-v2-core-scope-target-task7b2d2c1\x86_64-pc-windows-msvc\release\deps\base_compute_pipeline-fc0c73da15de0b6f.exe`
- SHA-256（每轮核验一致）：`734FC74A008C96600428BB5C1836AC44CC4EC164D61FD3CC72383179B90A8DC5`
- 原始输出（已忽略）：`.superpowers/sdd/2026-08-30-streaming-hash-media-pipeline/task-5-final-gate-bench-run-{1..9}.log`

### 同机交替诊断（不替代九轮门禁）

为验证九轮 FAIL 是否只是同机瞬时环境波动，在见到诊断结果前冻结了旧固定夹具 EXE 为 A、当前
EXE 为 B 的顺序：`A,B,B,A,A,B,B,A,A,B,B,A`。A 的路径为
`C:\tmp\rust-v2-cpu-io-baseline-assets-20260823\base_compute_pipeline-old.exe`，SHA-256 为
`8802E8409ADD61331364EE772237DB665A13C22899CC4164DB8BD53C7EBD45C5`；B 为上述当前 EXE，SHA-256
为 `734FC74A008C96600428BB5C1836AC44CC4EC164D61FD3CC72383179B90A8DC5`。每次均直接运行 EXE，
全部退出 0，并核对 `seed=0x20260823C0DE0000`、`files=4`、`cache_hits=2`、`hash_sessions=3`、
`media_decode_jobs=2` 与 `persisted_completed=true`。

| run | EXE | elapsed_ms | cache_wait_ms | worker_idle_before_hash_ms | decode_and_persist_ms |
| --- | --- | ---: | ---: | ---: | ---: |
| 01 | A | 118.339 | 38.890 | 39.568 | 63.009 |
| 02 | B | 136.060 | 38.622 | 71.433 | 47.307 |
| 03 | B | 135.947 | 39.156 | 71.128 | 47.680 |
| 04 | A | 115.304 | 35.640 | 36.241 | 64.053 |
| 05 | A | 112.617 | 34.870 | 35.760 | 61.671 |
| 06 | B | 132.608 | 34.575 | 67.669 | 47.589 |
| 07 | B | 136.908 | 40.799 | 72.864 | 47.406 |
| 08 | A | 110.709 | 31.290 | 31.980 | 62.877 |
| 09 | A | 115.334 | 35.122 | 35.774 | 63.831 |
| 10 | B | 127.353 | 31.502 | 63.575 | 46.883 |
| 11 | B | 128.624 | 31.195 | 63.556 | 48.078 |
| 12 | A | 115.201 | 35.188 | 36.086 | 63.936 |

| EXE | elapsed_ms 中位数 | cache_wait_ms 中位数 | worker_idle_before_hash_ms 中位数 | decode_and_persist_ms 中位数 |
| --- | ---: | ---: | ---: | ---: |
| A | 115.253 | 35.155 | 35.930 | 63.420 |
| B | 134.278 | 36.596 | 69.401 | 47.498 |

B 相对 A 的端到端中位数为 `+19.025 ms`（`+16.507%`），且 B 的最小值 `127.353 ms` 仍高于
A 的最大值 `118.339 ms`。cache wait 只增加 `1.441 ms`，而 B 的 worker idle 增加 `33.473 ms`；
B 的 decode/persist 反而减少 `15.919 ms`。这支持“当前 EXE 的差异集中在 cache 后到 Hash 前的
worker idle 阶段，不能仅归因于同机瞬时环境”的诊断假设；它不是源级根因证明，仍需在后续针对
该阶段追踪调度/等待状态。

A 的 SHA `8802…45C5` 不是历史 `115.946 ms` 参考所对应的 SHA，故此 A/B 对照只作环境和因果
诊断，绝不覆盖、替换或重新裁决上述当前 B 九轮 `133.524 ms` / `+15.161%` 的 FAIL。原始 stdout
（已忽略）为 `.superpowers/sdd/2026-08-30-streaming-hash-media-pipeline/task-5-same-host-diagnostic-{01..12}-{A|B}.log`。

## 最终自动化门禁

命令严格串行、均使用 `--test-threads=1` 且退出 0：

| 命令 | 通过数 | 编译 warnings |
| --- | ---: | ---: |
| `cargo test -p dedup-node-store --locked -- --test-threads=1` | 80 | 0 |
| `cargo test -p dedup-node-engine --lib --locked -- --test-threads=1` | 137 | 23 |
| `cargo test -p dedup-node-engine --features test-hooks --lib --locked -- --test-threads=1` | 158 | 12 |
| `cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1` | 60 | 20 |
| `cargo test -p dedup-node-engine --features test-hooks --test transient_task_files --locked -- --test-threads=1` | 25 | 20 |
| `cargo test -p dedup-node-engine --test disk_scheduler --locked -- --test-threads=1` | 42 | 17 |
| `cargo test -p dedup-node-engine --test scan_runtime_details --locked -- --test-threads=1` | 11 | 17 |
| `cargo test -p dedup-node-engine --test task_dispatch --locked -- --test-threads=1` | 27 | 17 |

合计 `540 passed; 0 failed; 0 ignored`。warnings 均是现存的 unused import/dead code
编译警告；本次没有把 warnings 当作通过条件的一部分，也没有为消警改动无关实现。

最后执行 `cargo fmt --all -- --check` 和 `git diff --check`，两者均退出 0。

## 验收边界

上述为受控单元/集成行为测试和固定 4 文件微基准：可证明单键查询、流式推进、owner 收束和
调度额度；九轮微基准仍超过退化阈值，不能证明此 HEAD 的性能回归已关闭。它也不能证明 FFmpeg
对真实媒体的解码结果、最终便携包内容、安装/升级路径或部署环境表现；这些验收本轮明确未执行。
