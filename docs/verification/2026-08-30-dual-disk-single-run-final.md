# Rust V2 双物理盘单次真实媒体运行证据

日期：2026-08-30

## 结论

本次只执行了一次真实媒体运行，使用 `H:\pik\00000000000` 和 `I:\tmp` 两个不同物理盘上的媒体根。运行达到 1800 秒上限后任务被取消，未完成全部 39,018 个文件，因此本次真实媒体验收结论为 **FAIL**，结果正确性为 **INCONCLUSIVE**。没有把客户端正常退出或局部调度指标写成任务通过，也没有部署到 `I:\Tool`。

已确认的局部事实如下：

- SQLite 基础缓存查询是批量路径，39,018 项在 487 ms 完成，观测吞吐约 `80,058 项/s`，不是每秒不到 200 条的逐项查询。
- 两个物理盘存在有效重叠读取，说明按物理盘分 lane 的调度确实同时工作。
- Hash 与 Media 没有同一采样点重叠；Hash 结束后才进入 Media，当前流水线仍存在完整阶段屏障。
- 该屏障解释了“磁盘读取高、CPU 低”与“磁盘读取低、CPU 高”的交替现象；Worker 数量增加到 20 不能消除这一架构瓶颈。
- 观察到 26 条独立 Worker 崩溃：24 条退出码 `0xC0000374`（堆损坏），2 条退出码 `0xC0000005`（访问冲突）。Node 未意外退出，随后仍继续运行和补充 Worker，单个 Worker 崩溃没有阻塞整个 Node 进程。

## 证据绑定

### 源码、正式包和外部测试工具

| 项目 | 值 |
| --- | --- |
| 源码 revision | `751fc2d4e13c36559d721cf3d670549a27a4b3ef` |
| 源码 tree SHA-256 | `011a7a8b7609142d23f2e8e37adf744dc01760bedcc1dc0d12acdf7e23e8c814` |
| 正式包 | `D:\code\mySingerServer\.worktrees\core-scope-transient-runtime\dist-rust-v2\mySingerServer-rust-v2-win-x64.zip` |
| 正式包 SHA-256 | `417aa226a82a6655aa8200aba2fc613a69e4f07716ae7e87fb0ba6262a3d757b` |
| 正式包大小 | `70,321,172` 字节 |
| package manifest SHA-256 | `9d7517c848cfd5de12e9d897da8b60965b51c8c7eaa5c034dce22bba968b06eb` |
| runtime_acceptance.exe | `C:\tmp\rust-v2-core-scope-target-task7b2d2c1\x86_64-pc-windows-msvc\release\examples\runtime_acceptance.exe` |
| runtime_acceptance SHA-256 | `3fee5e29615416d8a9d509db777d44d991a36d6b6776358ac188d83eda53ed9d` |
| export_scan_result_summary.exe | `C:\tmp\rust-v2-core-scope-target-task7b2d2c1\x86_64-pc-windows-msvc\release\examples\export_scan_result_summary.exe` |
| exporter SHA-256 | `76b5b4631a4a1e372d505df1c46093e4b167ec7e460b85d53821ae35bac327eb` |

正式包通过了正式包构建和独立包校验；测试客户端、exporter、数据库和 runtime 数据没有塞入正式 ZIP。上表中的 exporter SHA 只绑定了外部工具版本，本次因任务未完成而没有执行结果导出。

### 两次启动尝试的边界

第一次尝试根目录为 `C:\tmp\rust-v2-final-single-751fc2d`。该尝试把正式包 source `release` 与测试布局目标 `release` 指向了同一路径，复制阶段发生 self-copy，属于预运行编排失败。该目录只有包文件和空的 `evidence`、`temp`、`tools`/数据目录，没有 `runtime.ndjson`、`system.ndjson` 或 `harness-result.json`，没有启动 Node、Worker 或真实媒体任务。该失败证据保留，不能算产品运行失败，也不能算一次真实跑测。

第二次尝试根目录为 `C:\tmp\rust-v2-final-run-751fc2d-attempt-02`，是本次唯一真实运行；所有运行结论均来自其 `evidence` 目录。没有执行 A-3、六轮 A/B 或第二次真实媒体运行。

## 实际运行配置和物理盘

| 项目 | 实际值 |
| --- | --- |
| 枚举器 | Everything |
| Worker 槽位 | 20 |
| 总读取线程 | 12 |
| HDD 每盘读取额度 | 1 |
| SSD 每盘读取额度 | 2 |
| Unknown 每盘读取额度 | 1 |
| Node hash_tasks | 12 |
| Node global_disk_permits | 12 |
| 完成规则 | 任务进入终态即结束；1800 秒仅为上限 |
| Runtime ID / Task ID | `01a04f58-d4bf-77c1-9a3b-1bd5daed4155` |

`physical-disk-map.json` 显示根目录确实落在两个不同的物理盘：

- `H:\pik\00000000000` → `PhysicalDisk1`，Disk 1，`HP SSD EX900 1TB`，NVMe，partition 4；
- `I:\tmp` → `PhysicalDisk2`，Disk 2，`INTEL SSDSC2BB800G6R`，SATA，partition 2。

磁盘映射文件 SHA-256 为 `78c424f42e06b9a4248d4c07a61f3053efb3100ade16e3827033be6b6b017303`。运行前后媒体清单的总体 SHA-256 均为 `3302f70759eb894e2c456913fc7668be9001bd24f0dcf5060081babd47ee30eb`；两个根的分项 SHA 也分别保持为 `8d88ca8a6abcded840a7a874911f59d140d1ad95e3f2b9ed14d7eadcfa8be147` 和 `03a554790321db83f9bac2f72dbb07c26ae50d73e8f8d40fcaf5a6364ec4dc31`。

## 任务结果和验收状态

Everything 枚举出 39,018 个文件，总容量约 218.25 GiB。`runtime.ndjson` 有 1,799 条运行样本，随后写入一个 `runtime_result`；客户端日志中的 `RUST_V2_RUNTIME_ACCEPTANCE_PASS duration=1800 samples=1799 scans=1` 只表示测试客户端完成了采样和退出，不表示扫描任务完成。

`runtime_result` 的事实为：

- `scans_started=1`，`failed_scans=0`；
- `cancelled_at_deadline=true`；
- 该 Task 的终态为 `cancelled`；
- `correctness=INCONCLUSIVE`，没有 `latest_completed_persistent_task_id`；
- Node 没有非预期退出；
- 终态 `runtime_sample` 没有在取消边界前写出，终态 ownership 归零因此只能判定为 INCONCLUSIVE，不能伪造 PASS。

截至最后运行样本，`overall_completed=0` 是当前协议未把逐项 Worker 完成实时投影到整体计数的观测缺口，不应被解读为“没有任何 Worker 工作”；同时也不能用 Worker 局部计数替代任务完成事实。`task_file_stats` 没有由当前运行协议暴露，任务终态后 runtime 目录也按设计清理。

## SQLite 批量缓存查询

在最后运行样本中，`lookup_base_cache` 阶段为：

| 项目 | 值 |
| --- | ---: |
| 查询完成项 | 39,018 |
| 总项 | 39,018 |
| 阶段耗时 | 487 ms |
| 观测吞吐 | 80,058.009 项/s |

这是同一批次查询阶段的墙钟结果，证明本次实际运行使用了批量缓存查询路径；它不代表后续 SQLite 插入或更新被提前执行。计算结果的插入/更新仍在需要时由 Worker ACK 和 NodeStore 事务完成。

## 双盘调度和利用率

运行遥测记录了两个物理盘在相同采样窗口有读取活动：runtime 双盘活动样本为 1,625 个，系统采样中两个目标盘同时有非零读取的样本为 504/611。逐盘许可最终计数为：

| 物理盘 | 有效容量 | waiting 峰值 | active 峰值 | grant | release |
| --- | ---: | ---: | ---: | ---: | ---: |
| PhysicalDisk1（H） | 2 | 1 | 2 | 45,628 | 45,626 |
| PhysicalDisk2（I） | 2 | 1 | 2 | 23,552 | 23,550 |

因此，本次双物理盘分 lane 和同时读取的设计预期得到局部证据支持；两个盘的有效许可都没有被另一盘完全占满。由于任务在截止时间取消，不能从这一次运行推导完整任务吞吐或最终结果正确率。

### Hash/Media 阶段边界

按 runtime 样本的 pipeline 指标对齐：最后一个 Hash 读取活动样本约在 elapsed 374 秒，第一次 Media 读取活动样本约在 elapsed 375 秒，同一采样点的 Hash/Media overlap 为 0。也就是说，当前实际流程近似为：

```text
Everything 枚举 19.698 s
        ↓
SQLite 批量路径缓存查询 0.487 s
        ↓
Hash 阶段（直到约 374 s）
        ↓  无同采样点 overlap
Media 解码/特征阶段（约 375 s 以后）
```

这与用户观察到的利用率交替完全一致：Hash 阶段磁盘吞吐高而 CPU 较低，Media 阶段 Worker CPU 较高而磁盘吞吐下降。问题的主因是 Hash→Media 的阶段屏障和当前流水线没有把已完成 Hash 的项持续交给 Media，而不是两个物理盘没有被识别或 Worker 数量太少。

原始采样按阶段取采样点算术平均后的主要指标为：

| 阶段 | Node+Worker CPU（折算整机） | Worker CPU（折算整机） | H 读吞吐 | I 读吞吐 |
| --- | ---: | ---: | ---: | ---: |
| Hash | 约 3.62%（24 逻辑核整机口径） | 约 0% | 约 314.24 MiB/s | 约 307.22 MiB/s |
| Media | 约 33.09%（应用口径） | 约 32.93% | 约 26.14 MiB/s | 约 11.70 MiB/s |

全运行窗口的 Worker CPU 折算为整机占用后平均约 25.75%，单个采样窗口峰值约 72.19%；Worker 非空闲数平均 6.74/20，峰值 20。系统物理盘采样的全窗口平均/峰值读吞吐为：PhysicalDisk1（H）`89.32/1,022.58 MiB/s`，PhysicalDisk2（I）`76.51/380.46 MiB/s`。这些是运行采样观察值，不是整机总 CPU、硬件额定性能，也不构成性能门禁通过。

## Worker 崩溃隔离和完整路径

从全部 `runtime_sample.failures` 按 `stage_id + display_path + message` 去重后得到 26 条独立 Worker 崩溃记录，其中 24 条退出码为 `-1073740940`（无符号十六进制 `0xC0000374`，堆损坏），2 条为 `-1073741819`（`0xC0000005`，访问冲突）。Node 仍保持运行并继续补充/调度 Worker，说明单个 Worker 崩溃不会把整个 Node 任务直接卡死；但这些文件项本身失败，任务尚未完成，不能把“隔离成功”写成“计算成功”。

完整路径均保存在 `runtime.ndjson` 的 failure 记录中，独立记录如下：

1. `0xC0000374` — `H:\pik\00000000000\100多位UP主提督舰长定制福利\BBB up100+ cchdzcom\1067\B站 肖肖乐oxo\p\13972444277916301_photo_msg10978.jpg`
2. `0xC0000374` — `H:\pik\00000000000\100多位UP主提督舰长定制福利\BBB up100+ cchdzcom\1067\B站 肖肖乐oxo\p\13972444301265605_photo_msg10986.jpg`
3. `0xC0000374` — `H:\pik\00000000000\100多位UP主提督舰长定制福利\BBB up100+ cchdzcom\1067\B站 肖肖乐oxo\p\13972444352930149_photo_msg10995.jpg`
4. `0xC0000374` — `H:\pik\00000000000\100多位UP主提督舰长定制福利\BBB up100+ cchdzcom\1067\B站 肖肖乐oxo\p\13972444352930149_photo_msg10996.jpg`
5. `0xC0000374` — `H:\pik\00000000000\100多位UP主提督舰长定制福利\BBB up100+ cchdzcom\1067\B站 肖肖乐oxo\p\13972444352930149_photo_msg11001.jpg`
6. `0xC0000374` — `H:\pik\00000000000\100多位UP主提督舰长定制福利\BBB up100+ cchdzcom\1067\B站 呓小涵baby 提督\V\13993910619502797_1 (34)_1748101207862_msg13746.mp4`
7. `0xC0000374` — `H:\pik\00000000000\100多位UP主提督舰长定制福利\BBB up100+ cchdzcom\1067\B站up 安妮塔Oo\B站 安妮塔Oo 提督\P\14052061357222933_photo_msg865.jpg`
8. `0xC0000005` — `H:\pik\00000000000\170\0n0s0f0w0\pictures\1696236705551.jpg`
9. `0xC0000374` — `H:\pik\00000000000\170\0n0s0f0w0\pictures\1696251045587~01.jpg`
10. `0xC0000374` — `H:\pik\00000000000\170\0n0s0f0w0\pictures\20240604_210858.jpg`
11. `0xC0000374` — `H:\pik\00000000000\Kaeru Kukki\Kaeru Kukki\77v+200p\p\35tlxtyp.jpeg`
12. `0xC0000374` — `H:\pik\00000000000\Kaeru Kukki\Kaeru Kukki\77v+200p\p\9lcs4wql.jpeg`
13. `0xC0000374` — `H:\pik\00000000000\Kaeru Kukki\Kaeru Kukki\77v+200p\p\h41ewomt.jpeg`
14. `0xC0000374` — `H:\pik\00000000000\Kaeru Kukki\Kaeru Kukki\77v+200p\p\j1oeai9c.jpeg`
15. `0xC0000374` — `I:\tmp\bt\大皮股\1765564617814.jpg`
16. `0xC0000374` — `I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [2号酱@Rouer22]【注销】\[Twitter][萝莉] [2号酱@Rouer22]【注销】\P-2号酱@Rouer22 (17).jpg`
17. `0xC0000374` — `I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [Fariskitten猫型人偶]【注销】\[Twitter][萝莉] [Fariskitten猫型人偶]【注销】\V-Fariskitten猫型人偶 (27).mp4`
18. `0xC0000374` — `I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [i.k@Criskissly]【注销】\[Twitter][萝莉] [i.k@Criskissly]【注销】\V-i.k@Criskissly (5).mp4`
19. `0xC0000374` — `I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [i.k@Criskissly]【注销】\[Twitter][萝莉] [i.k@Criskissly]【注销】\V-i.k@Criskissly (6).mp4`
20. `0xC0000374` — `I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [Kit@kittyxkum]\[Twitter][萝莉] [Kit@kittyxkum]\V-Kit@kittyxkum (305).mp4`
21. `0xC0000374` — `I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [Kit@kittyxkum]\[Twitter][萝莉] [Kit@kittyxkum]\V-Kit@kittyxkum (311).mp4`
22. `0xC0000374` — `I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [Kit@kittyxkum]\[Twitter][萝莉] [Kit@kittyxkum]\V-Kit@kittyxkum (316).mp4`
23. `0xC0000374` — `I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [Loli Gumy@LoliGumy]\[Twitter][萝莉] [Loli Gumy@LoliGumy]\V-Loli Gumy@LoliGumy (3).mp4`
24. `0xC0000374` — `I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [不许凶然然@zkr03zkr]【注销】\[Twitter][萝莉] [不许凶然然@zkr03zkr]【注销】\V-不许凶然然@zkr03zkr (3).mp4`
25. `0xC0000374` — `I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [姜兔兔@nainai010821]\[Twitter][萝莉] [姜兔兔@nainai010821]\V-姜兔兔@nainai010821 (52).mp4`
26. `0xC0000005` — `I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [小瑶萝莉酱@kissyaoyao]\[Twitter][萝莉] [小瑶萝莉酱@kissyaoyao]\P-小瑶萝莉酱@kissyaoyao (180).jpg`

原始每秒快照的 `failures` 数组累计包含 `17,231` 条重复观察，不是独立崩溃次数。本次报告按全部 runtime sample 的 `stage_id + display_path + message` 去重并按退出码归类，独立崩溃数为 26（`0xC0000374` 24 条、`0xC0000005` 2 条）。

## 结果导出和媒体完整性

由于任务被取消且没有完成扫描，`result-summary.tsv` 没有生成，`harness-result.json` 中 `result_summary_status=INCONCLUSIVE`、`result_summary_sha256=null`、`exporter_exit_code=-1`。这表示本轮没有可验证的最终分析结果，不表示 exporter 本身崩溃，也不能用空结果代替正确性证明。

媒体清单前后逐项一致：运行前后均为 39,018 个文件、约 218.25 GiB，路径、长度和 `LastWriteTimeUtc` 均一致，整体媒体 manifest SHA 未变化。运行只读媒体根，没有执行删除或修改媒体文件。

## 下一步判断

本次证据把问题边界收窄为：

1. 物理盘识别、双盘 lane 和配置额度已生效；
2. SQLite 批量缓存查询已达到约 80k 项/s；
3. Worker 崩溃不会使 Node 进程整体退出，崩溃项和完整路径可记录；
4. 尚未解决的主要性能问题是 Hash 阶段与 Media 阶段的全量屏障，以及逐项完成计数没有及时投影到运行快照；
5. 26 个实际 Worker 崩溃仍需单独修复或隔离判定，不能因 Node 存活而忽略。

下一次代码修复应优先让已完成 Hash 的项进入内容缓存判定后持续交给 Media，取消整批 Hash→Media 屏障，并补齐逐项完成/失败的实时计数；在此之前不应宣称 CPU/磁盘利用率目标已达成。本次已经按要求完成一次真实运行，不重复执行第二轮。

## 原始证据位置

- `C:\tmp\rust-v2-final-run-751fc2d-attempt-02\evidence\runtime.ndjson`
- `C:\tmp\rust-v2-final-run-751fc2d-attempt-02\evidence\system.ndjson`
- `C:\tmp\rust-v2-final-run-751fc2d-attempt-02\evidence\harness-result.json`
- `C:\tmp\rust-v2-final-run-751fc2d-attempt-02\evidence\physical-disk-map.json`
- `C:\tmp\rust-v2-final-run-751fc2d-attempt-02\evidence\media-before.json`
- `C:\tmp\rust-v2-final-run-751fc2d-attempt-02\evidence\media-after.json`
- `C:\tmp\rust-v2-final-run-751fc2d-attempt-02\evidence\report.md`

本文件只记录该单次运行的证据，不生成多轮聚合，不恢复旧任务，不修改或清理原始 evidence。
