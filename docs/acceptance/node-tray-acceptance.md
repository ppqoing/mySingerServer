# 节点托盘验收记录模板

## 使用规则

本模板用于记录 `nodetray.exe` 的静态门禁与授权 Windows 动态验收。每个门禁
的“状态”只能填写下列一个值：

- `PASS`：命令实际执行成功，证据完整且结果满足合同；
- `FAIL`：命令已执行但结果不满足合同；
- `BLOCKED_NOT_RUN_DYNAMIC`：需要真实进程、交互桌面、UAC、计划任务、HKCU
  或其他未获授权/不具备条件的动态场景，因而没有运行。

不得因为源码存在、静态测试通过、运行时间看似足够或负责人接受边界，就把未
运行的动态项目填写为 `PASS`。本文件不预填任何 `PASS`，也不宣称安装包、代码
签名、WebView2 bundle 或真实部署已完成。

## 验收批次信息

复制本节并逐项填写；尖括号内容是说明，不是验收状态。

| 字段 | 记录 |
|---|---|
| 运行 ID | `<唯一运行 ID>` |
| 日期时间与时区 | `<ISO 8601>` |
| 机器 | `<主机名、Windows 版本、架构>` |
| 执行账号 | `<脱敏账号标识及是否管理员>` |
| 源码/产物版本 | `<提交、构建 ID 或 N/A_NO_GIT_METADATA>` |
| 测试根 | `<隔离的中性临时目录>` |
| 证据根 | `<绝对路径>` |
| 执行人 | `<姓名或内部标识>` |

测试根只能使用本次运行创建的隔离中性目录，不得把真实媒体目录、Agent 数据
目录或用户文件目录用作生成、输出或清理目标。

## 授权开关

在运行任何动态命令前逐项记录。未明确授权的项目必须保持关闭，对应场景填写
`BLOCKED_NOT_RUN_DYNAMIC`。

| 授权项 | 值（是/否） | 批准人/时间 | 影响与证据 |
|---|---|---|---|
| 允许进程启动、停止、重启 | `<是/否>` | `<记录>` | `<记录>` |
| 允许交互式 UAC | `<是/否>` | `<记录>` | `<记录>` |
| 允许创建、运行、停止、删除固定计划任务 | `<是/否>` | `<记录>` | `<记录>` |
| 允许修改和恢复当前账号 HKCU 登录启动 | `<是/否>` | `<记录>` | `<记录>` |
| 允许调用 Explorer 打开固定目录 | `<是/否>` | `<记录>` | `<记录>` |
| 允许使用 WebView2 缺失/失败隔离模拟 | `<是/否>` | `<记录>` | `<记录>` |
| 允许访问隔离测试配置与中性测试文件 | `<是/否>` | `<记录>` | `<记录>` |

## 单项门禁记录

每个门禁复制以下块，状态栏只能填写三个允许值之一，不得保留其他文字。

### `<门禁编号和名称>`

- 状态：`<PASS、FAIL 或 BLOCKED_NOT_RUN_DYNAMIC 三选一>`
- 开始/结束时间：`<ISO 8601>`
- 机器与账号：`<脱敏记录>`
- 前置条件：`<记录>`
- 实际命令：`<原样命令；先移除任何敏感值>`
- 退出码：`<整数或未运行>`
- 预期结果：`<合同>`
- 实际结果：`<脱敏摘要>`
- 证据路径：`<绝对路径>`
- 是否涉及进程：`<是/否>`
- 是否涉及 UAC：`<是/否>`
- 是否涉及计划任务：`<是/否>`
- 是否涉及 HKCU：`<是/否>`
- 是否完成凭据扫描：`<是/否；扫描报告路径>`
- 清理结果：`<恢复了哪些隔离状态；未清理项及原因>`
- 备注：`<限制、失败原因或阻塞原因>`

## 建议门禁清单

### 静态门禁

1. Go 单元测试与静态分析；
2. 前端测试、lint、类型检查和生产构建；
3. 四页签、键盘导航、窄窗口和 Worker 只读合同；
4. 交互式 Agent/Helper 表单、脱敏和字段校验合同；
5. Wails 版本、锁文件、可复现构建与供应链清单；
6. `asInvoker`、无 devtools/sourcemap、发布闭包和敏感信息扫描；
7. 中文部署文档、文件存在和本地链接检查。

### 授权 Windows 动态门禁

1. 单实例、第二实例唤起、关闭窗口隐藏和通知区域菜单；
2. Agent 手动/自动启动、受控停止、重启、异常退出和未认领实例；
3. Worker Ready、Worker 崩溃状态可见以及无直接 Worker 管理动作；
4. Helper 手动 UAC 取消和同意；
5. 固定最高权限登录计划任务安装、定义校验、运行、停止、漂移拒绝和删除；
6. 当前账号“登录后启动托盘程序”启用、禁用和路径漂移拒绝；
7. “仅退出托盘程序”和“停止组件后退出”；
8. 15 秒停止超时且不自动强杀；
9. Agent/Helper 保存、单份 `.last-good`、ACL 和明确恢复；
10. WebView2 缺失、安装取消、安装失败和初始化失败隔离模拟；
11. 隐藏稳定后的内存、CPU、句柄数及 Agent/Worker 基准影响；
12. 通知、日志、JSON 证据、配置与备份的凭据扫描；
13. 测试根绝对路径复核、隔离状态恢复和无真实媒体变更。

## 凭据与证据要求

- 所有命令、日志、截图、JSON 和报告在归档前都要执行敏感信息扫描；
- 数据库连接、密码、令牌、删除确认值和其他凭据不得进入本文或证据；
- 需要证明字段存在时，只保留字段名、哈希或 `[REDACTED]`；
- 扫描命中必须人工区分文档术语、测试占位和真实敏感值；真实敏感值出现即为
  `FAIL`，先隔离证据再按项目流程处置；
- 证据路径必须可追溯到运行 ID，不能只写“见日志”。

## 当前动态验收状态

### 2026-08-03 安全静态批次

本批次仅执行脚本合同、只读 `-WhatIf`、Go/前端/供应链静态门禁；没有运行
`nodetray.exe`，没有启动、停止或重启 Agent/Helper/Worker，没有触发 UAC、计划
任务、HKCU、Explorer 或进程控制。源码目录无 `.git` 元数据，版本记录为
`N/A_NO_GIT_METADATA`。

| 门禁 | 实际命令/范围 | 状态 | 实际结果 |
|---|---|---|---|
| Task 11 安全脚本合同 | `pwsh -NoProfile -File tests/windows/Test-NodeTrayHarness.ps1` | `PASS` | 验证 WhatIf 不创建测试根、无授权动态 10/10 阻塞、动态证据绑定错误 fail-closed、ValidateOnly 不产生真实动态 PASS、资源结果只有在资源与吞吐量均 PASS 时才可汇总为 PASS，且所有离线夹具不写测试根输出 |
| WhatIf 发布产物就绪度 | `pwsh -NoProfile -File tests/windows/Test-NodeTray.ps1 -WhatIf -StageDir D:\code\mySingerServer\.tmp\nodetray-stage-task9 -TestRoot C:\tmp\mysingerserver-node-tray-44444444444444444444444444444444 -CentralTestPort 39281` | `FAIL` | 只读确认 nodetray 与 WebView2 Bootstrapper 存在；该独立托盘 stage 不含 Agent、Worker、Helper；仓库 Backend 也明确为 `BLOCKED_IMPLEMENTATION_DEPENDENCY`，缺少受控验收通道、真实场景实现和动态证据 writer；未创建 TestRoot、未执行 stage 或 executor |
| Go 节点托盘测试 | `go test ./internal/nodectl ./internal/agentcontrol ./internal/helpercontrol ./internal/nodetray/... ./nodetray -count=1` | `PASS` | 初次运行发现共享 Windows 8.3/最终路径回归；主任务最小修复后以同一命令复跑，列出的全部 Go 包通过 |
| 前端测试 | `npm test -- --run`（`nodetray/frontend`） | `PASS` | 18 个测试文件、86 个测试全部通过 |
| 前端 lint | `npm run lint`（`nodetray/frontend`） | `PASS` | ESLint 退出码 0 |
| 前端生产构建 | `npm run build`（`nodetray/frontend`） | `PASS` | TypeScript 检查和 Vite 生产构建通过；本命令没有启动 GUI |
| 托盘供应链静态门禁 | `pwsh -NoProfile -File scripts/test-node-tray-supply-chain.ps1` | `PASS` | 输出 `NODETRAY_SUPPLY_CHAIN_GATE_PASS`；没有执行真实托盘产物 |

完整命令、时间、限制和 RED→GREEN 记录见
`.superpowers/sdd/2026-08-02-node-tray-ui-release/task-11-report.md`。上述 `FAIL`
只能由同一门禁的实际成功复跑更新，不能因其他静态门禁通过而改写；发布 stage 构建
和真实 Windows 动态验收均未由本批次执行。

### 动态证据汇总合同

`Test-NodeTray.ps1` 的非 WhatIf 入口现在可以汇总由外部授权执行器生成的 10 个
场景证据，但汇总器本身不会启动、停止或重启进程，也不会触发 UAC、计划任务或
HKCU。真实汇总要求证据索引和每个证据文件都位于同一
`C:\tmp\mysingerserver-node-tray-<guid>` 测试根内，并严格绑定运行 ID、stage、当前
用户 SID 哈希、授权矩阵、stage 文件哈希、场景编号、清理恢复记录和凭据扫描。
任一绑定、哈希或恢复证据不满足即 fail-closed。

本批次只运行 `-WhatIf -ValidateEvidenceOnly` 的仓库 `.tmp` 合同夹具。该模式只验证
JSON 结构和逻辑绑定，输出 `would_summarize_status` 供测试纯汇总逻辑，但
`dynamic_acceptance` 始终为 `BLOCKED_NOT_RUN_DYNAMIC`；它不能把离线夹具转换为
真实动态 PASS。

仓库内执行器固定为 `tests/windows/Test-NodeTrayBackend.ps1`，主脚本会只读核对其
仓库归属、AST、SHA-256、参数合同和能力标记。主脚本已经预留“全授权且完整
preflight → 调用该执行器 → 回读同一 TestRoot 内动态证据 → 再执行严格验证”的
闭环；但当前 Backend 明确声明 `BLOCKED_IMPLEMENTATION_DEPENDENCY`，不会被调用。
WhatIf 和动态汇总 JSON 均提供明确字段
`executor_capability=BLOCKED_IMPLEMENTATION_DEPENDENCY` 与
`executor_invoked=false`，避免调用方从嵌套对象推断或误认为骨架已运行。
代码证据表明当前仍缺少三项必需能力：

- nodetray 只有 GUI/background/elevated-once 启动模式，没有 TestRoot 限定的验收
  控制通道；
- Backend 的七个场景函数仍是逐项 fail-closed 骨架；
- Backend 尚未实现满足 10 场景合同的动态证据 writer。

因此本批次不能把该骨架描述为可用 executor，也不能解除真实动态 blocked。只有
上述三项以新的 TDD 和授权动态验收补齐后，能力标记才可改为
`READY_FOR_AUTHORIZED_DYNAMIC`。

`Measure-NodeTrayResources.ps1 -ValidateResultOnly` 同样只验证离线样本和吞吐证据
的联合状态。资源阈值通过但吞吐证据缺失时，整体仍为
`BLOCKED_NOT_RUN_DYNAMIC`；只有两者都通过时，纯逻辑字段
`would_summarize_status` 才可为 PASS，而真实 `dynamic_acceptance` 仍保持阻塞。

### 动态项目

截至 2026-08-03，本任务没有获得启动/停止真实 Agent、Helper、Worker，弹出
UAC，创建计划任务，修改 HKCU，打开 Explorer 或运行 WebView2 隔离模拟的授权。
因此下列真实动态项保持 `BLOCKED_NOT_RUN_DYNAMIC`，静态检查结果不能替代：

| 动态项目 | 状态 | 原因 |
|---|---|---|
| 托盘单实例、窗口、通知区域与退出交互 | `BLOCKED_NOT_RUN_DYNAMIC` | 未运行真实 GUI/Explorer/进程场景 |
| Agent 手动/自动启停、重启和 Worker Ready | `BLOCKED_NOT_RUN_DYNAMIC` | 未授权启动 Agent/Worker |
| Helper 手动 UAC 与受控停止 | `BLOCKED_NOT_RUN_DYNAMIC` | 未授权 UAC/Helper 进程 |
| 固定最高权限登录计划任务 | `BLOCKED_NOT_RUN_DYNAMIC` | 未授权计划任务变更 |
| 当前账号登录启动 | `BLOCKED_NOT_RUN_DYNAMIC` | 未授权 HKCU 变更 |
| 15 秒停止超时和显式强制结束 | `BLOCKED_NOT_RUN_DYNAMIC` | 未授权进程控制 |
| `.last-good`、ACL 和恢复的真实路径验证 | `BLOCKED_NOT_RUN_DYNAMIC` | 未运行受保护目录动态测试 |
| WebView2 缺失、取消和失败模拟 | `BLOCKED_NOT_RUN_DYNAMIC` | 未授权交互式隔离模拟 |
| 隐藏状态资源测量和吞吐影响 | `BLOCKED_NOT_RUN_DYNAMIC` | 未启动发布产物 |
| 动态日志、通知、配置和证据凭据扫描 | `BLOCKED_NOT_RUN_DYNAMIC` | 没有动态证据可扫描 |

后续只有在明确授权的隔离 Windows 会话中实际运行、保存命令和证据并完成清理
复核后，才能把对应状态改为 `PASS` 或 `FAIL`。
