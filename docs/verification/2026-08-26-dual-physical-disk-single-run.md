# Rust V2 双物理盘单轮真实媒体诊断报告

日期：2026-08-26

## 结论

本轮已经完成唯一一次真实媒体执行尝试，不再重跑。架构结论明确：**当前上游按规范化路径顺序创建并领取任务，导致 H 根全部处理完后才开始 I 根；`DiskReadScheduler` 在 I 请求尚未进入等待队列时无法进行跨盘公平调度。双物理盘没有得到预期的并行利用。**

同时，运行数据定量确认了用户观察到的相位交替：磁盘高吞吐、队列较高时，Worker 活跃数和 CPU 较低；磁盘读取较低时，Worker 活跃数和 CPU 较高。增加 Worker 数量或读取线程数不能解决“另一块盘的请求尚不可见”这一上游供给问题。

本轮验收状态为 **INCONCLUSIVE**，不是 PASS：运行到约 1,628 秒时，Windows 系统采样器遇到 Worker 退出竞态并异常中止，任务尚未产生 `runtime_result`、数据库快照和结果摘要。有效运行窗口足以支持调度架构判断，但不能证明任务正确完成或候选可部署。

- 调度设计预期：**NOT_MET**
- 运行验收：**INCONCLUSIVE**
- 部署结论：**NON_DEPLOYABLE**

## 测试范围

| 项目 | 值 |
|---|---|
| 媒体根 1 | `H:\pik\00000000000`，24,232 项 |
| 媒体根 2 | `I:\tmp`，14,786 项 |
| 总项数 | 39,018 |
| H 物理盘 | Disk 1，HP SSD EX900 1TB，NVMe |
| I 物理盘 | Disk 2，INTEL SSDSC2BB800G6R，SATA |
| Worker | 20 |
| 全局读取线程 | 12 |
| 每盘配置 | HDD=1、SSD=16、Unknown=1 |
| 保留核心 | 1；运行时 CPU budget=23 |
| 运行模式 | `SingleRun=true`，任务终态即退出，1,800 秒仅为最大期限 |
| 枚举器 | `windows_walker` |

两根均为存在的绝对路径、非重解析点，并由 Windows Storage API 确认为不同物理盘。运行前无 Node、Worker 或 runtime acceptance 残留。外部已有 Everything PID 6292，仅作为环境噪声记录，本轮未调用或停止它。

## 产物和门禁

受影响的新鲜门禁均通过：

- `RUST_V2_RUNTIME_ACCEPTANCE_HARNESS_PASS`
- `RUST_V2_RUNTIME_ACCEPTANCE_REPORT_PASS`
- `RUST_V2_RUNTIME_ACCEPTANCE_AST_PASS`
- `runtime_acceptance_contract`：22/22
- `git diff --check`：通过

唯一 B 测试包：

- 构建：`RUST_V2_CPU_IO_TEST_PACKAGE_PASS`
- 正式包校验：`PACKAGE_PASS`
- ZIP SHA-256：`d1736901a5610d712683a139c11f96ded2723ce9e1f9c3966a6be99ba9c7672b`
- 正式包保持 4 个顶层 EXE、5 个 FFmpeg DLL；不包含 data、数据库、acceptance client 或 exporter。
- 测试包标记 `test_only=true`、`deployable=false`，没有部署到生产目录。

外置工具：

- `runtime_acceptance.exe`：`b494fbf8ac470960850b3b383962900604457115eba5599831a7c0cf0b37a3fc`
- `export_scan_result_summary.exe`：`20b5419c5b01bf31cbd53dd26685e50fed780c5bbe8904a5bca5d8f5e029a498`

固定合成基准恰好运行三轮，结果为 125.470、131.993、125.719 ms，中位数 125.719 ms，三轮均 `persisted_completed=true`。相对历史 A 中位数 115.946 ms 慢 8.429%，未达到“至少改善 15%”门禁。合成基准不属于真实媒体重复跑测。

## 唯一真实媒体运行结果

- 开始：`2026-08-26T12:03:19.790Z`
- 最后 runtime 样本：elapsed=1,628 秒，状态仍为 `running`
- 最后计算计数：38,406/39,018，stage failed=11，skipped=34
- runtime 样本：1,628 条；system 样本：561 条
- `runtime_result`：缺失
- exporter：未执行，exit=-1
- Measure marker：`RUST_V2_RUNTIME_ACCEPTANCE_MEASURE_FAIL`
- Supervisor：`TimedOut=false`、`ExitConfirmed=true`，客户端和 Node 身份有效
- 媒体前后整体及逐根 SHA-256 完全一致
- 收尾后无 Node、Worker、runtime acceptance 或 exporter 进程残留

`harness-result.json` 的 `run_status=FAIL` 表示采样器异常后客户端被终止，不表示扫描任务自然进入 failed 终态。由于缺少 `runtime_result`，正式报告按证据完整性规则输出 INCONCLUSIVE。

## 双盘利用率

以下统计以 `system_sample.sample_interval_ms` 加权；MiB/s 使用 1 MiB=1,048,576 bytes。

| 指标 | H / Disk 1 | I / Disk 2 |
|---|---:|---:|
| 全窗口平均读取 | 67.2 MiB/s | 70.8 MiB/s |
| 盘活跃时平均读取 | 155.6 MiB/s | 196.8 MiB/s |
| 平均读取次数 | 85.6/s | 44.9/s |
| 平均读队列 | 0.159 | 1.875 |
| 读队列 P95 / 峰值 | 0 / 14 | 15 / 23 |
| 有读取活动时间 | 701.98 s | 584.31 s |

两盘系统样本同时出现读取的时间仅约 **3.18 秒**，约占完整采样窗口 **0.20%**。更严格的 Worker 路径交叉验证结果为：

- H Worker 活跃 823.61 秒，活跃时平均约 15.05 个 H Worker；
- I Worker 活跃 592.78 秒，活跃时平均约 11.04 个 I Worker；
- H 与 I Worker 同时出现：**0 秒**；
- 最后一个 H Worker 样本：elapsed=1,003 秒；
- 第一个 I Worker 样本：elapsed=1,004 秒。

切换点与 H 根的 24,232 项边界高度吻合：elapsed=1,003 时计算约 24,182 项且 Worker 全为 H；下一秒开始出现 I Worker。系统盘样本也从“H 有流量、I 为 0”直接切换为“H 为 0、I 有流量”。因此约 3.18 秒系统读流量重叠应视为采样窗口/操作系统尾流，不是两个根的 Worker 并行处理。

## CPU、Worker 和流水线相位

CPU 使用 `system_sample.sample_interval_ms` 加权，并按 24 个逻辑处理器归一化：

| 指标 | 加权平均 |
|---|---:|
| Node CPU | 3.12% |
| Worker CPU 合计 | 45.66% |
| Node + Worker | 48.78% |
| 主机整体 CPU | 62.39% |

I 盘的高低读取分桶能清楚说明相位交替：

| I 盘相位 | 持续 | I Worker 平均活跃 | Node+Worker CPU | 平均队列 |
|---|---:|---:|---:|---:|
| 高读取，活动样本 P75，读取 >=368.48 MiB/s | 146.08 s | 5.70，约 14.30 个空闲 | 26.33% | 13.08，P95=22 |
| 低读取，活动样本 P25，读取 <=24.10 MiB/s | 147.18 s | 12.97，约 7.03 个空闲 | 53.68% | 约 0 |

这证明“磁盘高时 Worker 空闲多、CPU 低；磁盘低时 Worker 忙、CPU 高”不是任务管理器错觉，而是当前读取、解码和特征计算分批推进的真实行为。Worker=20、TotalRead=12 能在单盘相位内占满部分资源，但不能让尚未进入候选队列的另一块盘参与。

当前生产遥测只有全局 `hash_io`、`media_io` 和 `global_disk_permits=12`，没有逐物理盘 permit 的等待、持有和完成计数。因此本报告可以确认实际盘流量及 Worker 路径顺序，但不能声称每块盘具体获得了多少调度许可。磁盘读写延迟字段在全部样本中不可用，`PercentDiskTime` 也可能超过 100，未作为标准利用率使用。

## 架构原因

证据与源码链路一致：

1. `crates/windows/src/walker.rs:37-67` 对根排序后，完整递归当前根，再取下一根。
2. `crates/node-engine/src/scan/enumerator.rs:43-61` 把完整结果放入 `Vec`，再按 `normalized_path` 全局排序去重；大写规范化后全部 `H:\...` 排在 `I:\...` 前。
3. `crates/node-engine/src/actor.rs:1941-1983` 等整个枚举完成后才进入 BaseCompute。
4. `crates/node-engine/src/scan/base_compute.rs:652-679,1817-1859` 把清单转为 `VecDeque`，通过 `pop_front()` 按顺序准备每批最多 1,000 项。
5. `crates/node-store/src/tasks.rs:293-420,1034-1056` 按输入顺序创建任务项，并用 `ORDER BY item_id LIMIT 1` 领取下一项，继续保持 H 前缀。
6. `crates/node-engine/src/io/scheduler.rs:496-642,916-1012` 的分盘 FIFO、轮转和老化保护只对已经进入调度器的请求生效。
7. `crates/node-engine/src/scan/pipeline.rs:267-345,372-440` 直到文件被上游领取后才解析物理盘并申请读取许可。

因此问题不是 `DiskReadScheduler` 不会在两个可见盘之间公平调度，而是 **I 盘请求在 H 前缀处理完之前根本不可见**。提高 Worker、全局读取线程或每盘 SSD permit 只会加大当前单盘批次，不会自动发现 I 候选。

## 采样器异常边界

本轮提前结束的直接原因是系统采样器没有容忍 Worker 进程退出竞态：

- 最后一条成功 system 样本：`12:30:33.018Z`，仍包含 Worker PID 2344；
- Node 日志：`12:30:36.466Z`，PID 2344 崩溃，退出码 -1073741819；同一时刻替补 Worker 28004 就绪；
- `tests/windows/Measure-RustV2RuntimeAcceptance.ps1:1493-1498` 枚举进程后调用采样；
- `tests/windows/Measure-RustV2RuntimeAcceptance.ps1:1327` 无保护地读取 `$Process.TotalProcessorTime.TotalMilliseconds`；
- Worker 在 `Get-Process` 后、属性读取前退出时，`TotalProcessorTime` 可变为 null，在 StrictMode 下产生本轮完全相同的 `TotalMilliseconds` 属性错误。

证据等级：采样器异常导致提前结束为高；具体由 PID 2344 的退出/替换窗口触发为中高，因为原脚本没有把异常 PID 和行号写入证据。

本轮还观察到 Worker 协议帧截断和少量 FFmpeg 无法解码输入。按本次任务范围，不把崩溃修复混入 CPU/双盘调度结论；但它们以及已知 Worker ACK 风险仍使候选保持 NON_DEPLOYABLE。

## 建议调整顺序

1. **先让不同盘候选同时可见。** 在全局去重之后、创建任务项之前，把结果按根或物理盘拆成有界队列，再用 round-robin/加权 round-robin 合并 claim 顺序。当前 H/I 已确认不同盘，最小实现可先按根轮转；长期方案应按 `PhysicalDiskId` 建 ready queue。
2. **保持结果确定性与执行顺序解耦。** 执行顺序可以跨盘交错，最终结果与正确性摘要继续按 `normalized_path` 排序。轮转必须放在跨根去重之后。
3. **为流水线增加有界低水位补给。** 每盘维持小型 read-ahead 队列和全局内存/解码 credit，队列低于水位时持续补充，减少“读盘批次”和“CPU 批次”的大幅摆动；不要改成无界缓存或简单增加线程。
4. **补齐逐盘遥测。** 至少记录每个 `PhysicalDiskId` 的 waiting/current permits、ready queue、dispatch、read bytes 和完成数，否则无法验证 12 个全局 permit 的真实分配。
5. **修复采样器竞态后再做下一次验收。** 单个进程样本失败应跳过该进程并清理世代基线，不得中止整个任务；增加“进程在解析后退出”的行为测试。按用户要求，本报告对应的真实媒体运行不会重跑。
6. **最后才调参数。** 在多盘候选供给和遥测闭环前，继续提高 Worker 或读取线程不会解决根级串行问题。

## 证据位置

- 唯一根：`C:\tmp\rust-v2-dual-disk-final-20260826-20260826-195024`
- 冻结信息：`freeze.json`
- 预检：`preflight.json`
- 包：`packages\B`
- 工具：`tools\tools-manifest.json`
- 基准：`benchmark\benchmark-manifest.json`
- 真实运行：`run\B-1`
- runtime：`run\B-1\evidence\runtime.ndjson`
- system：`run\B-1\evidence\system.ndjson`
- harness：`run\B-1\evidence\harness-result.json`
- Node 日志：`run\B-1\release\data\node\logs\node.log`
- fallback 报告：`run\B-1\evidence\report.md`

本次未部署、未修改媒体、未触碰 `I:\Tool`，所有旧错误和本轮证据均保留。
