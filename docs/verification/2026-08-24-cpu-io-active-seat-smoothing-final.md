# Rust V2 CPU / 磁盘 I/O 活跃席位平滑最终裁决

最终裁决：`FAIL`

> 固定 benchmark 已形成完整、版本绑定的硬门禁失败证据。真实媒体六轮 A/B 被用户明确覆盖为一次全量运行，因此六轮比较仍为 `INCONCLUSIVE`；这不会把已经确认的 benchmark `FAIL` 升级为 PASS。即使局部调度指标有改善，部署门禁未通过。

## 最终范围

- 工作树：`D:\code\mySingerServer\.worktrees\rust-v2-media-dedup`。
- 媒体根：`I:\tmp`，只读。
- 用户最终要求：不再重复六轮；候选 B 使用 Worker20 / Read12 完成一次全量任务，任务进入终态即完成，不等待满 1800 秒。
- 未部署、未打正式发布标签、未复制生产目录、未读取/写入/替换 `I:\Tool\mySingerServer-rust-v2-win-x64`。

## 固定 benchmark 硬门禁

| 项目 | A | B |
| --- | ---: | ---: |
| 三轮 `elapsed_ms` | 115.946 / 119.375 / 110.058 | 129.940 / 126.830 / 136.577 |
| 中位数 | 115.946 ms | 129.940 ms |
| 相对改善 | — | -12.069412% |
| 要求 | — | ≥15% |
| 结果 | — | `FAIL` |

B benchmark EXE SHA-256：`d2a9bbb6f20815ca2b3c234db92b61eb14e6d262cf855725c921f9380b0f85ad`；最终 manifest：`C:\tmp\rust-v2-cpu-io-ab\benchmark\benchmark-manifest.json`，SHA-256 `455b9d6bfe04fc35849d76819204a0887e079e0a8782a79f5c7b1e12279ac17d`。

## 单次真实媒体结果

| 项目 | 结果 |
| --- | --- |
| 配置 | 候选 B、Worker20、Read12 |
| 第一遍 runtime task | `e1e91ec6-dc0e-4a12-bb0a-94d0902d5824` |
| 持久 task | `01a03c82-ce3c-78a0-abf6-6a15a2fc0242` |
| 终态 | `completed`，760 s |
| runtime 计数 | 14757 completed / 8 failed / 21 skipped / 14786 total |
| DB / JSONL 计数 | 14757 succeeded / 29 failed / 14786 total |
| 严格摘要 | `INCONCLUSIVE` |
| JSONL SHA-256 | `93c95980c7d00b4caef8f1b8140ad065e0f31852653e3ab025bc362773fba664` |
| 媒体清单 | before/after 相同，14786 文件、116823273431 bytes |

29 个失败项包括 8 个 FFmpeg 不可解码输入和 21 个 Worker 管道协议帧截断。任务执行和持久化行数闭合，但不能称全部媒体成功、无 Worker 问题或自动 harness PASS。原运行在首轮终态后按用户要求停止，自动启动的第二遍未纳入结果；旁路只读快照和结果绑定保存在 `C:\tmp\rust-v2-worker20-read12-first-scan-finalization-20260826`。

## 架构观测

- 计算阶段含尾段平均活跃 Worker `9.634/20`、空闲 `10.366/20`；排除最终尾段后平均 worker slot 仍只有 `11.41/20`。
- 20 Worker 全活跃只占约 `17.85%`；存在空闲 Worker 的时间约 `82%`。
- 高磁盘读取样本约 `373.446 MiB/s`，但 Worker CPU 只有约 `6.498` 核、平均空闲 Worker `13.815`、读队列 `13.173`。
- 20 Worker 全活跃时 Worker CPU 升高，磁盘吞吐与队列回落，说明 CPU 与 I/O 高负载仍以不同相位出现。
- 最终约 `107.950 s` 无新增完成项，20 Worker 全 idle，Hash/Media I/O 与 pipeline CPU 为 0，直到任务转为 completed。

因此，本次数据不支持“只需继续增加 Worker 或读取线程”的解释。Read12 已能形成 P95 约 20 的磁盘读队列；继续增加读取并发更可能增加排队。详细数据见 `docs/verification/2026-08-26-worker20-read12-single-run.md`。

## 新鲜最终门禁

Task 14 在最终工作树上重新执行：

- `git diff --check`：PASS；`cargo fmt --all -- --check`：PASS。
- Rust：protocol `4/4`、result summary `20/20`、disk scheduler `24/24`、base pipeline `51/51`、utilization `3/3`、runtime tasks `14/14`、runtime acceptance contract `16/16`，全部 PASS。
- Windows：Harness、Runtime Report、CPU/I/O A/B Report、Package fixture 四个最终 marker 全部 PASS。
- A/B `verify-release.ps1`：两套 formal ZIP 均 `PACKAGE_PASS`；sidecar、manifest、4 个 x64 EXE、5 个 FFmpeg DLL、禁入文件和 metadata 绑定均通过。

静态、行为与包边界通过只证明实现和工具契约未回归，不能覆盖 benchmark 性能失败或单次真实媒体中的 29 个失败项。

## 冻结包与回滚参考

| 项目 | A | B |
| --- | --- | --- |
| formal ZIP | `C:\tmp\rust-v2-cpu-io-ab\packages\A\formal\A-formal.zip` | `C:\tmp\rust-v2-cpu-io-ab\packages\B\formal\B-formal.zip` |
| ZIP SHA-256 | `b60a8925080453a290406b6cdbff457cfef847de002a29d3f0360222392efbba` | `0ccd94e3fa7c2ebf7f4d6f1b8ad10904e5f94aa7115b40b7293004f50dacde47` |
| source fingerprint | `7ddfb195000b05d65a2281e78fad63d1dad1366f9549b1fd8e9afef4e55d4c46` | `fbd2d41763a60f1ddc2461198529e8e76d9c5a1d307bb2441e976bf040e834b3` |
| manifest SHA-256 | `765c3d98b46925f4d7bf1d23c747131f51c6c6a849281a7d319a99317f6eb6fa` | `34c17388d349b9e5f827e25bccd9c14349b0325978c8b6c47950abc407015b09` |

A formal ZIP 仅作为测试回滚参考；本任务没有部署 B，也没有执行生产回滚。

## 原六轮验收状态

用户覆盖后不再运行 A-3/B-3 或重新开始六轮。原 `A,B,B,A,A,B` 的吞吐、低/高 I/O 差、resource bubble、内存、尾部与 canonical 六份一致性门禁均保持 `INCONCLUSIVE`，不得由本次单轮替代。

## 历史首次六轮尝试（原始记录保留）

以下内容是早期首次尝试的原始裁决，所列旧 B 包和旧输出根已被后续 round9 冻结与用户覆盖取代，仅用于审计历史，不能代表最终实物。

## 固定输入

- 媒体根：`I:\tmp`（只读）；媒体语义清单 SHA-256：`333f9b747d6bf7aa85f8da868e85ebe181eb072eebd73df7cf95ddaf1e211fec`。
- 输出根：`C:\tmp\rust-v2-cpu-io-ab\runs`；任务命令日志：`C:\tmp\rust-v2-cpu-io-ab\task-13\measure-six.stdout-stderr.log`。
- 固定顺序：`A-1, B-1, B-2, A-2, A-3, B-3`；配置为 Worker12/HDD1/SSD16/Unknown1/TotalRead16/Reserved1、Duration1800、system sample2秒、task snapshot1秒。
- A ZIP SHA：`b60a8925080453a290406b6cdbff457cfef847de002a29d3f0360222392efbba`。
- B ZIP SHA：`86416c2b91b1b97ae3867296fd279bc411c6e2baa985104b5150fa6e6c0284cd`。
- Cargo.lock SHA：`db7464102569bd4bbb1a4b756490e1b80a5159eefd1767b329ec8e309e8ac563`。
- 外置工具 SHA：runtime `e669e144c607caaef5490c7b1a43ffa138147df82f62eed640826a862e86a52f`；exporter `746e8bf88ca7384d6afe4a1cd037bec00210de8989354c582654caa1ce41209e`。

## B 固定 benchmark

| 项目 | A | B | 结论 |
| --- | --- | --- | --- |
| 三轮 `elapsed_ms` | 115.946 / 119.375 / 110.058 | 131.156 / 134.882 / 106.967 | FAIL |
| 中位数 | 115.946 ms | 131.156 ms | FAIL |
| 改善门禁 | — | -13.117%，要求至少 15% | FAIL |

初次编排的字节差异（A 尾 LF/verbose cargo 与严格 `cargo -V` 产物不同）保留在 `benchmark\B-initial-mismatch`；按 Ruling 8 使用 `rustc -Vv`、`cargo -Vv`、LF-only、最终 LF 重生成后，B 三文件 SHA 已逐项等于 A：`7fad09dd8c16d49de37932475436003b5b1763c1015c5ce081ad7692db368ca3` / `d950eb0ce4b58c936e802cfffba2b474fd5b60951a2c3702851b749699883221` / `7f3c9524bfc925cccfec04cb318c948a03b5c7c4825b23dc969617da29594715`。B 三轮已完成，EXE SHA 为 `d0ff2470fbe6e245f7f8f4325663f43c1f2d5ff588dbff23eb7e0879233d016c`；实测 B median `131.156 ms`，改善 `-13.117%`，固定 benchmark 门禁 FAIL。

## 六轮真实媒体逐轮状态

| 轮次 | run status | canonical result SHA | media manifest SHA | 说明 |
| --- | --- | --- | --- | --- |
| A-1 | INCONCLUSIVE | 缺失 | before/after 均 `333f9b747d6bf7aa85f8da868e85ebe181eb072eebd73df7cf95ddaf1e211fec` | `RUST_V2_ACCEPTANCE_EVIDENCE_EXISTS`，未启动 Node/Worker |
| B-1 | 未执行 | 缺失 | 缺失 | A-1 基础设施失败后停止 |
| B-2 | 未执行 | 缺失 | 缺失 | 同上 |
| A-2 | 未执行 | 缺失 | 缺失 | 同上 |
| A-3 | 未执行 | 缺失 | 缺失 | 同上 |
| B-3 | 未执行 | 缺失 | 缺失 | 同上 |

A-1 原始证据：`runs\A-1\runner.stderr.log` SHA-256 `24d4632f20965336c81a60d123dbff0adb04d8ff7ee1b3dfd2ee214bd39a7db1`；`runs\A-1\ab-run-result.json` SHA-256 `ccab6b4b47d568c33c4bd3c40c5059017505a281507c5477a5d4a0e452bc6479`；顶层 `ab-run-manifest.json` SHA-256 `245d54ff6753dafc7ea3af68c7661eb10d0319ccf9e3d8a7b8b1e4ffa64bf392`。媒体 manifest pretty 文件 SHA-256 为 `0e85f03f5b973f2f67e43ddf383238eaf02c31e852ca657c353646199f41b919`。

## 硬门禁

正确性门禁（total/succeeded/failed/skipped、queued/running、canonical result SHA、ownership peak/capacity、cache wait ownership violation、snapshot coverage、Node/Worker unexpected exit）：`INCONCLUSIVE`。六个 canonical result SHA 不存在，不能以 overall counters 替代。

性能门禁（production throughput、media-wait idle Worker-seconds、低/高 I/O idle 差、低/高 I/O Worker CPU 差、disk queue weighted P95、private bytes peak、resource bubble、cache wait ownership、90%→last ACK tail、item completion P95）：因没有完整六轮 A/B 样本均为 `INCONCLUSIVE`；fixed benchmark ≥15% 已实测为 `FAIL`（改善 `-13.117%`）。

聚合脚本尝试：`New-RustV2CpuIoAbReport.ps1` exit `3`，marker `RUST_V2_CPU_IO_AB_REPORT_INCONCLUSIVE`，部分根触发 `Index was outside the bounds of the array`；因此本文件是基于保留原始证据的人工聚合，不冒充自动 PASS/FAIL 报告。

## 边界与后续条件

`Measure-RustV2CpuIoAb.ps1` 预创建 `A-1\evidence`，而 `Measure-RustV2RuntimeAcceptance.ps1` 校验该目录必须不存在，导致 `RUST_V2_ACCEPTANCE_EVIDENCE_EXISTS`。按 brief，单轮基础设施失败后必须保留当前根并停止；任何修复都必须使用新 output root 重跑完整六轮。Task 13 不修改脚本、不删除证据、不部署。

实施账本：`docs/verification/2026-08-24-cpu-io-active-seat-smoothing-implementation.md`；Task 13 报告：`.superpowers/sdd/2026-08-24-cpu-io-active-seat-smoothing/task-13-report.md`。
