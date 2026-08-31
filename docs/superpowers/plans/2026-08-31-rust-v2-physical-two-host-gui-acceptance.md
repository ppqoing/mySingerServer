# Rust V2 双实体机与真实 GUI 验收实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` to execute this plan task-by-task. Every code change uses `superpowers:test-driven-development`; completion claims use `superpowers:verification-before-completion`.

**目标：** 在本机与 `codex-192-168-1-6` 上各运行一套相同 Rust V2 正式候选，每机用一个任务扫描两块真实物理盘；由本机真实 `desktop.exe` 唯一管理两个 Node 和中心 PostgreSQL，完成一次扫描、同步、跨机分析及 GUI 视觉/交互验收。

**架构：** Docker PostgreSQL 只运行在本机并通过受限 LAN 地址发布；两个 Node 使用隔离运行根和相同资源配置；真实 GUI 是测试期间唯一 Node 管理连接。系统采样器只读取 Windows 性能数据；GUI 退出后只读观察器顺序导出两个 Node 和中心库终态。

**技术栈：** Rust 2024、Tokio、Protobuf、PowerShell 7/Windows PowerShell 5.1、Docker PostgreSQL 16、OpenSSH、Slint、Windows 性能计数器。

**设计书：** `docs/superpowers/specs/2026-08-31-rust-v2-physical-two-host-gui-acceptance-design.md`

## 全局约束

- 不触碰、部署、清理或覆盖 `I:\Tool`；不修改四个媒体根中的任何文件。
- 本机根固定为 `H:\pik\00000000000`、`I:\tmp`；远程根固定为 `D:\tmp`、`F:\tmp\10-31`。
- 本机 endpoint 固定 `192.168.1.17:43100`；远程 endpoint 固定 `192.168.1.6:43100`；两者 MachineId 必须不同。
- Worker 固定手动 20；全局读取 12；SSD 每盘 16、HDD 每盘 1、unknown 每盘 1；默认枚举器为 Everything。
- 每台机器只创建一个包含两个根的扫描任务；任务到终态即完成，不等待固定 1800 秒；不重复全量扫描。
- 真实 GUI 运行期间不启动其他 Node 协议客户端；观察器只在 GUI 完全退出后连接，且只读。
- Docker 对象固定为 `mysingerserver-rust-v2-dualhost-20260831` 和 `mysingerserver-rust-v2-dualhost-20260831-data`；不自动删除卷。
- PostgreSQL 只允许远程 `192.168.1.6` 访问本机测试端口 15439；测试结束停止容器并删除本轮临时防火墙规则。
- 密码运行时随机生成，禁止出现在 Git、stdout、报告、截图或完整 DSN 中。
- 实际操作只用绝对路径、PID、进程启动时间和运行根确认目标，不按进程名批量终止。
- 所有 PowerShell 公共函数、参数和关键变量以及新增 Rust 公开类型、函数添加中文注释。
- 行为测试必须先 RED 后 GREEN；禁止使用源码字符串匹配替代行为测试。
- 正式候选产品 revision 与验收工具 revision 分开记录；验收脚本提交不得被宣称已进入旧候选 ZIP。

## Task 1：让 PostgreSQL 容器安全发布到指定 LAN 地址

**文件：**

- 修改：`scripts/New-RustV2PostgresContainer.ps1`
- 修改：`tests/windows/Test-RustV2PostgresContainer.ps1`
- 修改：`deploy/README-管理端部署.md`
- 新增：`.superpowers/sdd/2026-08-31-rust-v2-physical-two-host-gui-acceptance/task-1-report.md`

- [ ] 在测试替身中记录 Docker argv，新增 RED：默认发布 `127.0.0.1:<port>:5432`、显式 `192.168.1.17` 发布 LAN 地址、主机名/通配符/带 Docker 参数的值被拒绝，非法值不会调用 Docker。
- [ ] 运行 `pwsh -NoProfile -File tests\windows\Test-RustV2PostgresContainer.ps1`，保存旧实现无法接受 `-HostAddress` 的 RED。
- [ ] 为脚本新增 `[string] $HostAddress = '127.0.0.1'`；使用 `System.Net.IPAddress::TryParse` 校验且只接受 IPv4/IPv6；发布参数由解析后的规范地址与已验证端口组成。
- [ ] 保留同名容器/卷拒绝、schema 3/22 表验证和默认 loopback 行为；不得新增覆盖或删除选项。
- [ ] 更新管理端部署文档，说明可信 LAN、防火墙最小范围及密码脱敏。
- [ ] 重跑 PowerShell 行为测试并执行 `git diff --check`；提交 `test: allow validated postgres host binding`。

## Task 2：实现 GUI 退出后的只读双 Node 终态观察器

**文件：**

- 新增：`crates/desktop-core/examples/physical_two_host_observer.rs`
- 新增：`crates/desktop-core/tests/physical_two_host_observer_contract.rs`
- 必要时修改：`crates/desktop-core/src/node_session.rs`
- 必要时修改：`crates/desktop-core/Cargo.toml`
- 新增：`.superpowers/sdd/2026-08-31-rust-v2-physical-two-host-gui-acceptance/task-2-report.md`

- [ ] 用两个 loopback 协议替身写 RED：观察器严格顺序连接两个 endpoint；只发送 Hello、状态、任务/详情读取；遇到 NodeBusy 立即输出稳定诊断；不发送 CreateScan、Sync、Analysis、Delete、SaveConfig。
- [ ] 定义单行 NDJSON 记录 `observer_start`、`node_snapshot`、`observer_error`、`observer_result`；包含 endpoint、MachineId、产品/协议、最新任务统计/阶段/outbox highwater、pipeline 资源与逐盘指标。
- [ ] 通过环境变量或参数接收两个 endpoint 和输出路径；路径在边界规范化，输出只写调用方指定的隔离证据目录。
- [ ] 若当前协议未暴露某项，只输出明确的 `available=false`/缺失原因，不伪造值、不增加写协议。
- [ ] GREEN 命令：`cargo test -p dedup-desktop-core --test physical_two_host_observer_contract --locked -- --test-threads=1`。
- [ ] 回归：`cargo test -p dedup-desktop-core --test physical_two_hosts_e2e --locked --no-run`、`cargo fmt --all -- --check`、`git diff --check`；提交 `test: add readonly physical two host observer`。

## Task 3：实现双实体机 GUI 验收编排与裁决工具

**文件：**

- 新增：`tests/windows/Invoke-RustV2PhysicalTwoHostGuiAcceptance.ps1`
- 新增：`tests/windows/New-RustV2PhysicalTwoHostGuiReport.ps1`
- 新增：`tests/windows/Test-RustV2PhysicalTwoHostGuiAcceptance.ps1`
- 新增：`.superpowers/sdd/2026-08-31-rust-v2-physical-two-host-gui-acceptance/task-3-report.md`

- [ ] 用 fake Docker/SSH/process/performance provider 写 RED，覆盖：四根映射到四个预期物理盘、同 ZIP 双端 SHA 一致、两个不同 MachineId、每机恰好一个双根任务、GUI 是唯一管理连接、媒体清单前后一致。
- [ ] 覆盖安全 RED：任何目标解析到 `I:\Tool`、媒体根下写目录、远程 F 盘运行根、非本轮容器/防火墙规则或同 MachineId 必须在产生外部写入前失败。
- [ ] 编排脚本参数固定包括候选 ZIP、观察器路径、SSH 别名、四个媒体根、两个 endpoint、中心地址、证据根和最长任务秒数；默认最长 7200 秒。
- [ ] 预检依次确认包/manifest、空间、媒体根、盘号/介质类型、Docker、SSH、端口和已有本轮对象；发现同名容器或卷只报告冲突，不覆盖。
- [ ] 为本地/远程创建唯一隔离运行根；复制同一 ZIP并复验；生成两份 Node 与一份 Desktop 配置；证据仅保存脱敏配置和 SHA-256。
- [ ] 生成本地/远程 2 秒系统采样器，按 PID+启动时间记录 CPU/I/O/内存、逻辑核、物理盘吞吐/队列/活动率；远程采样结果复制回本机且保留原件。
- [ ] 创建精确临时防火墙规则，启动中心容器、两个 Node 和采样器；确认 endpoints、MachineId 后启动真实 `desktop.exe`。脚本打印 GUI 交互清单与截图目录并等待 GUI 正常退出，不自动伪造点击。
- [ ] GUI 退出后运行 Task 2 观察器，导出 PG schema/节点/cursor/analysis 摘要；停止本轮 PID、容器和临时防火墙规则，保留卷、运行根与证据。
- [ ] 报告器独立输出 Infra、Runtime、DiskSchedule、Sync、CrossAnalysis、GUI、MediaIntegrity 七个门禁的 PASS/FAIL/INCONCLUSIVE，以及总裁决；证据不足不得提升为 PASS。
- [ ] GREEN：`pwsh -NoProfile -File tests\windows\Test-RustV2PhysicalTwoHostGuiAcceptance.ps1`；并执行 `pwsh -NoProfile -File tests\windows\Test-RustV2PostgresContainer.ps1`、`git diff --check`；提交 `test: orchestrate physical two host gui acceptance`。

## Task 4：构建并冻结验收输入

**输入/产物：**

- 正式候选：`C:\tmp\rust-v2-weighted-candidate-fb42a7e\formal\mySingerServer-rust-v2-win-x64.zip`
- 产品 revision：`fb42a7e1d97ec65c952e1f6f7e7e4a09e8a5545e`
- 预期 ZIP SHA-256：`d9f4daafb51cd218a875492f575d7cdbac35ff9f7fd9436bfe360f22c7f62f5b`
- 工具 target：`C:\tmp\rust-v2-physical-two-host-gui-tools-target`
- 验收根：`C:\tmp\rust-v2-physical-two-host-gui-20260831`

- [ ] 精确盘点 C/D 空间；不足时只清理本项目明确可再生的 Cargo target，记录前后空间，不做广域清理。
- [ ] 运行 `scripts\verify-release.ps1` 复核正式 ZIP；记录 ZIP、manifest、四个 EXE 和 FFmpeg DLL SHA-256。
- [ ] 用固定 target 构建 Task 2 观察器，不把测试 EXE塞入正式 ZIP；记录工具 revision、路径和 SHA-256。
- [ ] 运行自动契约：Postgres 容器脚本测试、双机 GUI harness 测试、observer contract、`bindings_contract`、`window_contract`、`offscreen_layout`。
- [ ] 生成 `run-input-manifest.json`，锁定四个媒体根、盘号/类型、endpoint、候选/工具 revision、配置指纹和计划路径；不包含密码。

## Task 5：执行一次双实体机、双物理盘、真实 GUI 全量验收

- [ ] 记录四个媒体根的 before 清单（规范路径、长度、LastWriteTimeUtc），不得计算内容 hash、不得在根内写文件。
- [ ] 创建本机隔离 PostgreSQL 容器并验证 schema 3/22 表；创建只允许 `192.168.1.6` 访问 15439 的本轮防火墙规则；从远程验证 TCP 可达。
- [ ] 在本机和远程隔离根解压同一候选并复验 SHA；写入 Worker 20、全局读取 12、SSD16/HDD1/unknown1、Everything 默认及中心 PG 配置。
- [ ] 启动两台 Node 和两个系统采样器，确认 `192.168.1.17:43100` 与 `192.168.1.6:43100` 可达、MachineId 不同、远程运行根只在 `D:\tmp`。
- [ ] 启动真实 Release `desktop.exe`；在 GUI 连续创建本地 H/I 双根任务和远程 D/F 双根任务，不创建第二轮扫描。
- [ ] 在任务运行时完成总览、节点、任务/详情、数据库/同步、跨机分析、结果滑动窗口、审核/删除确认等交互；保存 1440×900 与 1080×700 真实截图，绝不提交删除。
- [ ] 任一任务到终态立即进入同步和一次跨机分析，不等待 1800 秒；若 7200 秒未终态则停止本轮并裁决 INCONCLUSIVE，不拼接后续轮次。
- [ ] 正常关闭 GUI；顺序运行只读观察器；导出中心 schema、两节点 cursor/highwater、分析候选/分组；记录所有 PID 终态和退出原因。
- [ ] 停止本轮 Node/Worker/采样器/容器，移除本轮临时防火墙；保留命名卷、本地/远程运行根和全部证据。
- [ ] 记录 after 媒体清单并逐项比较；生成 `harness-result.json`、`report.md` 和证据 SHA-256 清单。

## Task 6：最终验证与审查

- [ ] 逐项核对计划和设计书；任何未执行 GUI 场景、缺失截图、采样断档或观察器字段均明确写成 INCONCLUSIVE，不补造证据。
- [ ] 运行 `cargo fmt --all -- --check`、相关 Rust/PowerShell 定向测试、`git diff --check`，保存原始输出。
- [ ] 生成从计划基线到最终 HEAD 的 review package；使用 `gpt-5.6-sol`、`max` 对实现和验收证据做最终审查。
- [ ] 对审查中的真实 Critical/Important 仅做一轮集中修复与一次复审；不得因修复工具而重跑全量媒体扫描。
- [ ] 最终报告分别给出产品候选结论、双盘调度结论、中心同步/跨机分析结论、GUI 结论和证据限制；不扩大为生产部署结论。
