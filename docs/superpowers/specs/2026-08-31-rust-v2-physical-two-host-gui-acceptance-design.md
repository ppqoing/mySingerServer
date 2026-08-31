# Rust V2 双实体机、双物理盘与真实 GUI 验收设计

日期：2026-08-31  
状态：用户已批准，进入实施与真实验收
适用分支：`codex/core-scope-transient-runtime`

## 1. 背景与已确认事实

本轮需要在两台真实 Windows 机器上运行同一个 Rust V2 候选版本，每台机器各扫描两个位于不同物理盘的真实媒体根，并由本机真实 Release `desktop.exe` 作为唯一管理端连接两个 Node 和一个中心 PostgreSQL。验收同时观察真实 CPU、物理盘 I/O、Worker、同步、跨机分析和 GUI 视觉/交互表现。

已确认环境：

- 本机 LAN 地址为 `192.168.1.17`；原真实媒体根为 `H:\pik\00000000000` 和 `I:\tmp`，分别映射到两块物理盘。
- 远程 SSH 别名为 `codex-192-168-1-6`，实际地址为 `192.168.1.6`，Windows 10、Windows PowerShell 5.1 可用。
- 远程 `D:\tmp` 位于 Disk 0，设备 `ST4000VX007-2DT616`，HDD/RAID，空闲约 56.2 GiB。
- 远程 `F:\tmp\10-31` 位于 Disk 1，设备 `HUH721212ALE601`，HDD/RAID，空闲约 12.8 GiB。
- 远程 D/F 是不同物理磁盘；远程另有一块处于 Predictive Failure 且离线的 Disk 3，本轮不得使用。
- 远程没有 Docker；中心 PostgreSQL 只能在本机 Docker Desktop 中创建。
- 当前 `New-RustV2PostgresContainer.ps1` 固定发布到 `127.0.0.1`，远程 Node 无法连接，必须增加受校验的宿主监听地址参数。
- 生产 `MachineId` 固定来自 SMBIOS。同一实体机启动两份正式 Node 会产生相同 MachineId，不能替代双实体机测试。
- 每个 Node 同时只接受一个管理连接。真实 GUI 和另一个 runtime acceptance 客户端不能同时连接同一 Node。

## 2. 目标

1. 使用一个隔离的 PostgreSQL 16 Docker 容器和新命名卷，执行当前 `deploy/central-v2.sql` 并验证 schema 3、22 张表。
2. 在本机和远程机分别运行一套隔离的正式候选 Node/Worker/Everything，不覆盖任何现有安装。
3. 每台机器只创建一个扫描任务；每个任务同时包含本机的两个真实媒体根。
4. 使用真实 Release `desktop.exe` 作为两台 Node 的唯一管理连接，实际完成任务下发、状态观察、同步和一次跨机器分析。
5. 在任务运行期间分别采集两台机器的 CPU、内存、Worker 和物理盘 I/O，不另占 Node 管理连接。
6. 对真实 GUI 执行可复核的视觉与交互验收，记录窗口截图、交互步骤、状态和异常。
7. 任务终态后关闭 GUI，再由只读观察器依次连接两台 Node，导出最终任务、MachineId、pipeline metrics 和中心同步高水位，不重复扫描。
8. 分开裁决基础设施、任务终态、磁盘调度、同步、跨机分析、GUI 和文件级失败，证据不足时明确标记 `INCONCLUSIVE`。

## 3. 非目标

- 不修改 Node 的“单管理连接”协议，不增加第二个观察连接。
- 不允许注入或伪造生产 MachineId。
- 不在同一实体机启动两个 Node 冒充双机。
- 不重复执行第二轮全量真实媒体扫描。
- 不在本轮执行删除、回收站或永久删除操作。
- 不修改媒体文件，不在媒体根中写缓存、日志或证据。
- 不在远程机安装 Docker。
- 不使用旧 Go/C++ Agent、GUI、Worker、schema 或发布包。
- 不部署、替换、清理 `I:\Tool`，也不改动远程已存在的生产目录。

## 4. 方案选择

采用“GUI 唯一管理端”方案：真实 GUI 下发两台机器的扫描并显示运行状态；外部采样器只读取 Windows 性能计数器和进程信息，不连接 Node；GUI 退出后只读观察器再依次连接 Node 固化终态。

不采用以下方案：

- “自动 acceptance client 驱动扫描、GUI 事后查看”：不能证明真实扫描期间 GUI 的状态、视觉和交互。
- “修改 Node 允许第二管理连接”：会扩大协议、会话所有权和并发模型，超出本次验收范围。
- “先后运行 GUI 扫描和另一轮自动全量扫描”：重复读取同一批真实媒体，不能增加本轮结论价值。

## 5. 拓扑与所有权

```text
本机 Windows 192.168.1.17
  ├─ Docker PostgreSQL 16
  │    └─ 192.168.1.17:15439 / dedup_v2 / schema 3
  ├─ 本地隔离 Node 192.168.1.17:43100
  │    ├─ H:\pik\00000000000 -> PhysicalDisk1
  │    └─ I:\tmp              -> PhysicalDisk2
  ├─ 真实 Release desktop.exe（唯一管理端）
  │    ├─ Node A: 192.168.1.17:43100
  │    ├─ Node B: 192.168.1.6:43100
  │    └─ PostgreSQL: 192.168.1.17:15439
  └─ 本地系统采样器（不连接 Node）

远程 Windows 192.168.1.6
  ├─ 远程隔离 Node 192.168.1.6:43100
  │    ├─ D:\tmp       -> Disk 0 / HDD
  │    └─ F:\tmp\10-31 -> Disk 1 / HDD
  └─ 远程系统采样器（不连接 Node）
```

PostgreSQL 保存中心同步和跨机器分析数据。每个 Node 仍独占自己的 SQLite、任务文件、缓存和 WorkerPool。Desktop 是唯一 Node 管理连接，同时拥有中心同步与跨机分析编排。系统采样器不得读取 SQLite、PostgreSQL 或 Node 协议。

## 6. 需要实现的验收工具

### 6.1 PostgreSQL 容器脚本适配

修改：

- `scripts/New-RustV2PostgresContainer.ps1`
- `tests/windows/Test-RustV2PostgresContainer.ps1`
- `deploy/README-管理端部署.md`

新增 `HostAddress` 参数：

- 默认值保持 `127.0.0.1`，现有本机行为不变。
- 只接受可解析的 IPv4/IPv6 地址，不接受主机名、通配字符串或额外 Docker 参数。
- Docker publish 固定为 `${HostAddress}:${HostPort}:5432`，不得拼接未经校验的原始文本。
- 行为测试必须先证明旧脚本无法表达 LAN 地址，再验证默认 loopback、显式 LAN 地址、非法地址拒绝和参数不会注入额外 Docker argv。
- 脚本继续拒绝已存在的同名容器或卷，不迁移、不覆盖、不删除数据。

本次使用唯一名称，避免影响已有 Docker 对象：

- 容器：`mysingerserver-rust-v2-dualhost-20260831`
- 卷：`mysingerserver-rust-v2-dualhost-20260831-data`
- 地址：`192.168.1.17:15439`
- 数据库：`dedup_v2`
- 用户：`dedup`

数据库密码在执行时随机生成，只保存在内存和隔离运行配置中。报告、stdout、截图和 Git 文件只保存已脱敏 DSN 与配置 SHA-256。

### 6.2 双实体机 GUI 验收编排器

新增：

- `tests/windows/Invoke-RustV2PhysicalTwoHostGuiAcceptance.ps1`
- `tests/windows/Test-RustV2PhysicalTwoHostGuiAcceptance.ps1`
- `tests/windows/New-RustV2PhysicalTwoHostGuiReport.ps1`

编排器只负责：

1. 校验工作树、候选包、SSH 别名、媒体根、物理盘映射、端口、Docker 和可用空间。
2. 创建隔离容器、临时精确防火墙规则和本地/远程运行根。
3. 将同一正式候选包复制到两台机器并逐端验证 ZIP、manifest 和 EXE SHA-256。
4. 写入两套 Node 配置和一套 Desktop 配置；证据只保存脱敏副本和指纹。
5. 启动本地/远程系统采样器和 Node，等待两个 endpoint 可达且 MachineId 不同。
6. 启动真实 `desktop.exe`，把控制权交给 GUI 交互阶段。
7. GUI 退出后运行只读终态观察器、中心库导出和报告生成。
8. 停止本轮进程、采样器、临时防火墙规则和容器，但保留容器卷、运行根和证据。

编排器不得自动点击 GUI、不得伪造 UI 状态、不得在没有真实截图和交互记录时把 GUI 标记为 PASS。

### 6.3 只读双 Node 终态观察器

新增一个测试专用、外置的 Desktop Core example 或等价测试客户端。它在 GUI 完全退出后顺序连接两个 endpoint，只读取：

- Hello 与协议/product id；
- NodeStatus、MachineId 和 endpoint；
- 最新运行任务的 ID、类型、状态、总数、完成、失败、跳过和阶段；
- pipeline metrics 中 Worker、Hash/Media 许可和逐物理盘 waiting/active/grant/release；
- 任务 `outbox_high_seq`；
- PostgreSQL 中每个 MachineId 的 committed cursor、节点记录、文件位置数量；
- 本轮跨机器分析状态、候选状态和分组数量。

观察器不得创建扫描、同步、分析、删除或配置写入。若 GUI 未释放管理连接，必须返回明确的 `NodeBusy`/连接占用诊断，而不是重试到看似成功。

## 7. 运行配置

### 7.1 候选包

两台机器必须使用同一个经 `verify-release.ps1` 通过的正式 ZIP。运行前后都记录：

- 产品 source revision 和 source tree SHA-256；
- ZIP 绝对路径、大小和 SHA-256；
- package manifest SHA-256；
- `desktop.exe`、`node.exe`、`worker.exe`、`Everything.exe` 和 FFmpeg DLL SHA-256。

如果为本轮脚本变更重新提交文档或测试工具，但产品二进制没有变化，报告必须区分“产品 revision”和“验收工具 revision”，不得声称脚本文档提交已经进入候选 ZIP。

### 7.2 Node 配置

两台 Node 使用相同资源配置：

- Worker：手动 `20`；
- 全局读取席位：`12`；
- SSD 每盘：`16`；
- HDD 每盘：`1`；
- unknown 每盘：`1`；
- 枚举器：默认 `Everything`；Everything 不可用时允许产品自身回退 Windows Walker，但必须在证据中记录。

本机 Node：

- endpoint：`192.168.1.17:43100`；
- 扫描根：`H:\pik\00000000000`、`I:\tmp`；
- data/config/log/cache 全部位于本机独立测试运行根。

远程 Node：

- endpoint：`192.168.1.6:43100`；
- 扫描根：`D:\tmp`、`F:\tmp\10-31`；
- 发布包、data/config/log/cache 全部位于 `D:\tmp` 下的独立测试运行根，禁止向空间偏紧的 F 盘写缓存或证据。

两台 Node 的 `[postgres]` 都启用并指向 `192.168.1.17:15439/dedup_v2`。Desktop 使用同一中心库 URL，并预置上述两个 endpoint。

## 8. 执行顺序

1. 记录本地/远程媒体清单：规范路径、长度和 LastWriteTimeUtc；只读，不计算媒体内容 hash。
2. 创建并验证中心容器，确认远程 TCP 可达，确认 schema 3 和 22 张表。
3. 解压并校验两份候选；写入隔离配置；记录配置脱敏指纹。
4. 启动两台 Node 和两个系统采样器；确认 endpoint、进程代际和不同 MachineId。
5. 启动真实 Desktop；确认 PostgreSQL Ready 和两个节点 Online。
6. 通过 GUI 向本地 Node 创建一个含 H/I 两根的任务，再向远程 Node 创建一个含 D/F 两根的任务。两次操作连续完成，不创建每盘独立任务。
7. 在任务运行时切换总览、节点和任务页，记录视觉、交互、任务阶段、Worker、失败项和状态稳定性。
8. 两个任务都到终态后，通过 GUI执行同步；要求中心 cursor 分别追平对应任务 highwater。
9. 通过 GUI 创建一次跨机器分析并轮询到终态；允许真实数据得到零组，但不允许遗留 Incomplete 被伪装成 Completed。
10. 依次验证结果、审核和删除页面；不提交真实删除。
11. 正常关闭 Desktop，运行只读双 Node 观察器和中心库导出。
12. 停止 Node、Worker、采样器、容器和临时防火墙规则；再次记录媒体清单并生成报告。

任务进入终态即完成，不要求等待固定 1800 秒。安全上限按每台任务 7200 秒设置；超时只停止本轮进程并保留证据，结论为 `INCONCLUSIVE`，不得拼接后续轮次。

## 9. GUI 视觉与交互验收

使用真实 Release 窗口和真实两节点/中心数据，不用静态 fixture 冒充动态结果。至少保存以下截图：

1. 总览：PostgreSQL 正常、两个节点在线、MachineId 不同。
2. 节点页：本地和远程节点的地址、状态、任务与同步信息。
3. 任务页运行中：两台机器同时存在真实任务，阶段、进度、Worker 和失败数可区分。
4. 任务详情：本地双盘和远程双盘的路径、阶段和资源信息不串台。
5. 数据库/同步页：两节点游标追平；截图不得包含明文密码或完整带凭据 DSN。
6. 跨机器分析运行中和终态页。
7. 结果窗口：滑动窗口加载、节点来源、路径、媒体类型和终态显示正确。
8. 审核/删除页：未满足前置条件时按钮禁用；满足条件时只检查确认界面，不执行删除。

交互记录至少覆盖：

- 导航切换、节点选择、刷新、手工同步；
- 两台 Node 的扫描根选择和任务创建；
- 任务选择、详情展开和失败项查看；
- 跨机器分析开始、轮询、结果窗口滚动；
- 窗口 1440×900 与 1080×700 下无关键控件遮挡、截断或不可操作；
- 连接失败或页面刷新不得让两个节点数据互相替换、反复闪烁或回到旧任务。

自动契约仍运行 `bindings_contract`、`window_contract` 和 `offscreen_layout`；它们只证明组件行为和布局，不替代真实窗口截图。真实视觉判断由截图、交互日志和人工结论共同构成。

## 10. 性能与调度证据

两台系统采样器每 2 秒记录：

- node/worker/desktop/Everything 的 PID、启动代际、CPU 增量、读写字节、Working Set 和 Private Memory；
- 逐逻辑核 CPU；
- 逐物理盘读写吞吐、队列、活动时间和可用延迟；
- 采样时间戳、实际间隔和采集耗时。

Node 终态观察器记录 pipeline metrics：

- Worker busy 峰值；
- Hash、Media、CPU weight、Worker slot 的 current/peak/capacity；
- 每个 physical_disk_id 的 waiting、active、grant 和 release；
- Hash/Media 重叠样本和逐盘共同等待时的权重分配。

本机 H/I 都按 SSD 配置参与任务级权重，12 个全局读取席位在两盘共同等待时目标为 6:6；一盘没有 Ready 工作后另一盘可使用释放额度。远程 D/F 都按 HDD 配置，每盘硬上限为 1，两盘共同等待时目标为 1:1；不得为了提高 CPU 利用率绕过配置把 HDD 额度自动抬高。

## 11. 证据布局

本机证据根使用唯一目录，例如：

`C:\tmp\rust-v2-physical-two-host-gui-20260831\evidence`

至少包含：

- `harness-result.json`：运行身份、包、配置、endpoint、MachineId、状态和裁决；
- `local-system.ndjson`、`remote-system.ndjson`；
- 两台 Node 的日志和 Worker 崩溃日志；
- Desktop 日志；
- `node-observer.ndjson`；
- `postgres-summary.json`，不含密码；
- `media-before/after` 及逐根 manifest；
- `screenshots/` 和 `interaction-log.md`；
- `report.md`。

远程只保留运行根、采样原始文件和进程日志；最终证据复制回本机后逐文件计算 SHA-256。复制成功前不得删除远程原件。

## 12. 裁决规则

### 12.1 PASS

只有以下全部满足才可将整轮标记为 PASS：

- 容器、schema、远程连接、两个 endpoint 和两个不同 MachineId 均有效；
- 每台机器恰好一个包含两个指定根的扫描任务，并到 `completed`；
- 两台 Node 未意外退出，所有 grant/release 守恒；
- 本机共同等待时符合 6:6，远程共同等待时符合 1:1；一盘无 Ready 工作后的额度重分配符合配置；
- 两节点中心 cursor 均追平各自任务 highwater；
- 跨机器分析到 Completed，且没有未解释的 Incomplete；
- GUI 指定交互全部可执行，真实截图和状态没有串台、闪烁、遮挡或错误终态；
- 媒体前后清单逐项一致；
- 必需证据完整且哈希可复核。

### 12.2 INCONCLUSIVE

以下情况不证明产品错误，但不能写 PASS：

- Docker/UAC/SSH/防火墙/端口/远程交互会话不可用；
- 远程 Node 无法在非交互 SSH 会话中创建托盘或需要无法确认的 UAC；
- GUI、Node、PG 或采样器证据缺失；
- 任务超时或用户主动停止；
- 系统采样间隔超限，无法支持性能结论；
- 文件级失败导致结果摘要 MISSING，但任务级状态机、调度和 GUI 仍可单独裁决。

### 12.3 FAIL

以下为产品或验收实现硬失败：

- 两 endpoint 返回相同 MachineId；
- 任一任务 failed/cancelled，或任务根与冻结输入不一致；
- 全局/逐盘许可越界、共同等待时违反配置权重、grant/release 不守恒；
- PG 已提交但 cursor/highwater 无法收敛；
- 跨机分析把 Incomplete 误写为 Completed；
- GUI 把两节点状态互相覆盖、任务不断切换、真实交互不可达或展示错误终态；
- 媒体路径、长度或 LastWriteTimeUtc 发生变化；
- 编排器触碰 `I:\Tool`、媒体根写入、旧生产目录或非本轮 Docker 对象。

各子门禁独立报告。磁盘调度 PASS、GUI PASS 或 runtime task completed 都不得扩大解释为“所有文件和整套产品 PASS”。

## 13. 安全、清理与保留

- 防火墙只临时允许 `192.168.1.6` 访问本机 PostgreSQL 测试端口，只允许本机 GUI 访问两个 Node endpoint；规则名称绑定唯一 run id。
- 所有启动和停止都以可执行文件绝对路径、PID、启动时间和运行根共同确认，不按进程名批量终止。
- 远程递归写入只允许在预先解析并校验的 `D:\tmp\rust-v2-physical-two-host-*` 目录内。
- 本机递归写入只允许在 `C:\tmp\rust-v2-physical-two-host-*`、候选 dist 和当前工作树的跟踪文件内。
- 不自动删除 Docker 命名卷、运行根、截图、日志或失败证据。测试结束只停止容器和进程并移除临时防火墙规则。
- 不执行 `docker volume rm`、`docker system prune`、广域文件清理或生产目录覆盖。
- 若磁盘空间不足，只盘点本项目可再生 Cargo target/测试缓存；未经精确路径确认不清理任何文件。

## 14. 实施原则

- 新脚本和行为变更使用 TDD：先得到目标行为 RED，再做最小 GREEN。
- PowerShell 参数、函数和重要变量添加中文注释，说明用途、输入和安全边界。
- 不把 Docker、SSH、UAC 或真实 GUI 依赖塞进普通单元测试；普通测试使用可记录 argv 的替身，真实环境只在显式验收入口运行。
- 现有单机 `Measure-RustV2RuntimeAcceptance.ps1` 保持单 Node 职责；双机 GUI 编排使用独立脚本，避免把单机采样器改成远程部署系统。
- 任何凭据只存在于隔离运行配置；仓库、报告和测试夹具只能使用假密码或脱敏占位符。
- 最终实现、测试和证据必须经过独立代码审查；审查通过前不合并、不推送、不部署生产。
