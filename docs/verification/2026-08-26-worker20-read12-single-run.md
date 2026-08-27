# Rust V2 Worker20 / Read12 单次真实媒体运行报告

## 结论

- 任务执行状态：`COMPLETED_WITH_ITEM_FAILURES`。第一遍全量任务在 `760 s` 进入 `completed`，不再以等待满 `1800 s` 作为完成条件。
- 严格验收状态：`INCONCLUSIVE`。14786 项中 14757 项成功、29 项失败；其中 21 项为 Worker 管道协议帧截断，不能称为“全部媒体成功”或“无 Worker 问题”。
- 架构观测结论：增加到 Worker20 / Read12 后，CPU 与磁盘 I/O 的相位分离仍明显。稳定计算段平均只有约 `9.63 / 20` 个 Worker 活跃，I 盘高吞吐样本中平均约 `13.82 / 20` 个 Worker 空闲。
- 发布裁决不变：固定 benchmark 的候选 B 中位数 `129.940 ms`，基线 A 中位数 `115.946 ms`，相对改善 `-12.069412%`，15% 门禁为 `FAIL`。本次单轮不能替代原六轮 A/B 正确性与性能门禁。
- 未部署，未读取、写入或替换 `I:\Tool\mySingerServer-rust-v2-win-x64`。

## 运行边界

| 项目 | 值 |
| --- | --- |
| 媒体根 | `I:\tmp`，只读 |
| 候选 | B |
| Worker | `20` |
| 全局读取线程 / permit | `12` |
| 系统采样目标 | `2 s` |
| 任务采样目标 | `1 s` |
| 原运行根 | `C:\tmp\rust-v2-cpu-io-ab\single-worker20-read12` |
| 原证据根 | `C:\tmp\rust-v2-cpu-io-ab\single-worker20-read12\evidence` |
| runtime task ID | `e1e91ec6-dc0e-4a12-bb0a-94d0902d5824` |
| 持久 task ID | `01a03c82-ce3c-78a0-abf6-6a15a2fc0242` |
| 旁路最终化根 | `C:\tmp\rust-v2-worker20-read12-first-scan-finalization-20260826` |

用户将本轮完成条件明确改为“单次全量任务进入终态”。客户端在第一遍完成后自动创建了第二个任务 `413d37ae-2052-4626-a9b7-f6be3f2af1f9`；该重复任务在采样 `elapsed=1064` 时随 PID `19720` 的测试进程树停止。第二遍未完成、未拼入本报告，也未再启动任何真实媒体测试。

## 第一遍任务终态

| 指标 | 值 |
| --- | ---: |
| 总项数 | 14786 |
| 成功 | 14757 |
| 失败 | 8 |
| 跳过 | 21 |
| runtime 终态 | `completed` |
| 持久 DB 终态 | `completed` |
| 持久 DB failed_items | 29 |
| 取消 | 0 |

runtime 的 `14757 + 8 + 21 = 14786` 与持久 DB 的 `14757 succeeded + 29 failed = 14786` 闭合。29 个失败项分为：

- 8 个 `.torrent` 被 FFmpeg 判为不可解码输入；
- 21 个 `.jpg/.mp4` 出现 `Worker 管道分帧失败: 协议帧被截断`。

第一遍期间系统样本观察到约 53 个不同 Worker PID，而配置席位为 20，说明存在 Worker 替换或进程 churn。证据可排除 Node 整体退出，但不能把 21 个管道错误解释为普通媒体解码失败。

## 阶段时间

| 阶段 | 时间 |
| --- | ---: |
| 枚举文件完成 | `15 s` |
| 基础缓存查询完成 | `67 s` |
| 基础计算开始 | `16 s` |
| 90% 任务项进入终态 | `601 s` |
| 全任务终态 | `760 s` |
| 90% 到终态尾段 | `159 s` |
| 全程吞吐 | `19.455 items/s` |
| 计算阶段吞吐 | `19.874 items/s` |

## 样本覆盖

| 证据 | 样本 | 范围 | 最大间隔 |
| --- | ---: | --- | ---: |
| runtime | 760 | elapsed `1..760` | `5.344 s` |
| system | 273 | elapsed `0..758` | `8.494 s` |

系统采样虽然按 `sample_interval_ms` 覆盖了首轮墙钟，但最大间隔超过原自动门禁的 `6 s`，因此本报告把系统指标作为架构观测，不冒充完整自动验收 PASS。

## CPU 与磁盘 I/O

下表统计缓存查询完成到任务终态前的计算阶段，包含约 108 秒无新增完成项的最终尾段；系统样本为 elapsed `68..758`，共 247 条，均按实际 `sample_interval_ms` 加权。

| 指标 | 加权均值 | P50 | P95 | 峰值 |
| --- | ---: | ---: | ---: | ---: |
| Worker CPU 核当量 | 9.388 | 10.293 | 19.798 | 20.499 |
| Node + Worker CPU 核当量 | 9.970 | 10.887 | 20.101 | 20.977 |
| 整机 CPU | 46.134% | 46.583% | 91.750% | 97.792% |
| I 盘读取 | 161.028 MiB/s | 53.767 MiB/s | 376.200 MiB/s | 382.564 MiB/s |
| I 盘读队列 | 4.618 | 0 | 20 | 22 |

Windows `PercentDiskTime` 在该物理盘计数器中可超过 100%，不把它当作占用率门禁；使用吞吐和读队列解释磁盘压力。

若只取仍有完成项增长的有效计算段 elapsed `67..652`，整机 CPU 加权均值为 `53.23%`，Worker CPU 为 `46.28%`；I 盘读取均值为 `199.43 MB/s`，P95 为 `395.14 MB/s`，读队列均值为 `5.46`、P95 为 `20`。因此，即使排除最终尾段，也没有形成持续的 CPU 与磁盘同时高占用。

### Worker 席位与相位

计算阶段（含最终尾段）elapsed `67..760` 共约 `693.647 s`：

| 指标 | 加权值 |
| --- | ---: |
| 平均活跃 Worker | 9.634 / 20 |
| 平均空闲 Worker | 10.366 / 20 |
| Worker 席位利用率 | 48.169% |
| 20 个 Worker 全活跃的时间占比 | 17.85% |
| 存在空闲 Worker 的时间占比 | 82.001% |
| 同时存在 permit 等待和空闲 Worker 的时间占比 | 58.652% |
| 平均 decode Worker | 5.805 |
| 平均 feature Worker | 3.811 |
| 平均 Hash I/O | 2.884 / 12 |
| 平均 Media I/O | 5.872 / 12 |

有效计算段 elapsed `67..652` 中，`worker_slots.current` 平均为 `11.41`，20 席全满约占 `21.35%`；elapsed `653..760` 的 `107.950 s` 内完成数不再增长，20 个 Worker 全部为 `idle`，Hash/Media I/O 和 CPU pipeline 当前值均为 0，直到任务状态转为 `completed`。

### 低 / 高 I/O 对照

| 计算窗口 | I 盘读取 | Worker CPU | 空闲 Worker | 读队列 |
| --- | ---: | ---: | ---: | ---: |
| 低于或等于读取 P50 | 14.273 MiB/s | 9.738 核 | 9.698 | 0 |
| 高读取（≥ 368.611 MiB/s） | 373.446 MiB/s | 6.498 核 | 13.815 | 13.173 |
| 零读取样本 | 0 | 2.475 核 | 17.442 | 0 |

按活跃 Worker 数分组也得到同一方向：1–11 个 Worker 活跃时，I 盘平均读取 `307.58 MB/s`、Worker CPU `28.10%`、读队列 `9.46`；20 个 Worker 全活跃时，I 盘平均读取降至 `80.85 MB/s`、Worker CPU 升至 `76.62%`、读队列降至 `0.61`。

磁盘读取与 Worker CPU 的样本相关系数约为 `-0.1806`，只作为解释性指标。更重要的直接证据是：高读取时磁盘队列已经很深，但 Worker CPU 和活跃席位反而下降；CPU 高峰时磁盘吞吐和队列回落。把读取线程继续从 12 往上加，首先增加的更可能是磁盘排队，而不是稳定提高 CPU 与磁盘的同时利用率。

## 停机后持久证据

测试进程全部退出后创建了只读 `node.db` / WAL / SHM 快照：

- snapshot metadata SHA-256：`cc677d4115d76fffcf8c4debf956487434ec19f01cbe5f43cbf3bfb2698bf6ea`；
- 三个源文件复制前后长度和 SHA 均一致；
- 持久结果 JSONL：14786 行，14757 `succeeded`、29 `failed`；
- canonical JSONL SHA-256：`93c95980c7d00b4caef8f1b8140ad065e0f31852653e3ab025bc362773fba664`；
- metadata / lease / JSONL 三件套绑定有效；
- overall status：`INCONCLUSIVE`，diagnostics 为 29 个 item status 加 1 个 task state。

旁路 `media-after.json` 与原 `media-before.json` 的文件 SHA-256 均为 `03a554790321db83f9bac2f72dbb07c26ae50d73e8f8d40fcaf5a6364ec4dc31`，语义 SHA-256 均为 `6e824e924a11fc8e70cdcd8f0a5345917d69db7e86c0c3fe554d04c7e89f0920`；文件数 14786、总字节 116823273431，证明媒体清单未变化。

原运行因按用户要求在第一遍终态后停止，未生成 `harness-result.json` 和自动 report。旁路结果绑定的是持久 DB task ID，不改写原 evidence，因此可以证明执行和持久结果，但不能伪装成原 harness 自动 PASS。

## 架构判断

本次数据否定了“主要只是 Worker 数量或读取线程太少”的解释：

1. 20 个 Worker 在含尾段的计算窗口平均只使用约 9.63 个席位；排除尾段后也只有约 11.41 个席位；
2. 12 个读取 permit 已能把 I 盘推到约 373 MiB/s，并形成平均 13、P95 20 的高读队列；
3. 高 I/O 区间 CPU 和活跃 Worker 下降，低 I/O 区间 CPU 才上升，相位分离仍存在；
4. 21 个 Worker 管道截断和多次 Worker 替换会污染部分利用率，但不会把“高 I/O 时大量 Worker 空闲”反转为充分重叠；
5. 固定 benchmark 仍比 A 慢约 12.07%，所以不能部署。

因此，下一步若继续优化，应优先缩短“读取完成但尚未形成可计算工作”与“Worker 等待 permit/输入”的窗口，并降低大文件长尾；不建议仅继续增加读取线程或 Worker 数量。
