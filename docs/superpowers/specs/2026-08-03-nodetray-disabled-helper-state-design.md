# NodeTray 禁用 Helper 状态误报修复设计

## 背景

当前实机中 Helper 明确处于未启用状态，没有 Helper PID，也不存在
`C:\ProgramData\MySingerServer\Helper\helper.json`。NodeTray 仍在总览页把 Helper
显示为“异常”，并在“最近异常”和页面警报中显示“组件不可用”。

错误链路如下：

1. `Factory.NewHelper` 无法读取 Helper 配置指纹，保留一个尚未初始化的
   `SharedComponent`；
2. `SharedComponent.Refresh` 对未初始化组件返回 `unavailable`；
3. `Service.GetOverview` 不区分 `helperEnabled`，无条件刷新 Helper；
4. 后台周期刷新继续发布 Helper 的 `component-state` 和
   `attention-required` 事件；
5. 前端无条件合并这些事件并显示错误摘要。

该现象不是 Helper 进程异常，而是禁用策略没有进入状态模型。强制退出中未初始化
Helper 的幂等停止已经修复，本设计不改变该行为。

## 目标

- Helper 未启用且没有真实 PID 时，总览保留 Helper 卡片，状态显示“未启用”；
- 禁用态不显示“组件不可用”，最近异常显示 `—`，操作按钮全部禁用；
- Helper 已启用但配置缺失时继续报告真实配置或组件不可用错误；
- Helper 已禁用但仍有真实 PID 时继续显示实际进程状态，不掩盖残留进程；
- Helper 计划任务与禁用策略不一致时继续显示任务漂移；
- 不改变强制退出、配置格式、Wails 导出结构或 Go 生命周期枚举。

## 非目标

- 不自动创建 Helper 配置；
- 不自动启用、启动、停止或删除 Helper；
- 不新增 `disabled` Go 生命周期或修改控制协议；
- 不隐藏已启用 Helper 的初始化失败；
- 不修改 Agent 状态语义；
- 不增加全机进程扫描或新的身份校验。

## 方案比较

### 方案一：后端状态归一化与前端事件守卫

这是采用的方案。

后端在生成 Overview 时把“策略禁用且没有 PID”的 Helper 归一化为中性停止状态；
前端事件存储层同时拒绝重新污染该状态的无效 Helper 事件。UI 只负责把该中性状态
显示成“未启用”。

优点是托盘健康判断、初始 Overview 和实时页面一致，不需要扩展跨语言模型；代价是
后端和前端事件边界都要各有一条窄规则。

### 方案二：仅在前端隐藏错误

只改 `OverviewPage`，根据 `helperEnabled` 隐藏错误文字。实现最少，但后台 Overview、
托盘健康判断和注意事件仍将 Helper 视为异常，属于遮挡症状，不采用。

### 方案三：新增 `disabled` 生命周期

在 Go、Wails 和 TypeScript 中增加完整生命周期。语义最直观，但会扩大模型、绑定、
状态机和测试面；当前只需要表达策略禁用，不采用。

## 后端状态策略

### 归一化条件

`Service.GetOverview` 先读取 TraySettings，再取得 Helper 原始状态。只有同时满足以下
条件时才归一化：

- `settings.HelperEnabled == false`；
- Helper 原始状态 `PID <= 0`。

PID 条件用于防止禁用策略掩盖仍在运行或尚未退出的真实 Helper。只要 PID 大于 0，
Overview 必须保留原始生命周期、PID 和错误信息。

### 中性禁用状态

归一化后的 `ComponentState` 使用现有字段：

- `Lifecycle = stopped`；
- `Healthy = false`；
- `Ready = false`；
- `PID = 0`；
- `StartedAtUnixMS = 0`；
- `UptimeSeconds = 0`；
- `ErrorCode = ""`；
- `ErrorSummary = ""`；
- `NeedsAttention = false`；
- `RuntimeConfigSHA256 = ""`；
- `NeedsRestart = false`；
- 如果原始状态已经带有合法的 `SavedConfigSHA256`，保留该摘要，否则保持空值。

不新增后端 `disabled` 生命周期。`helperEnabled` 仍是策略状态的唯一权威字段，
`Lifecycle` 只描述进程状态。

### 计划任务漂移

`HelperTaskDrift` 保持现有独立计算：禁用 Helper 时若固定计划任务仍然安装，仍显示
漂移提示。它代表真实系统残留，不属于“组件不可用”误报。

### 操作语义

- `StartHelper` 在禁用时继续返回 `helper_disabled`；
- Stop、Restart 和普通 Refresh 的底层 SharedComponent 合同不改变；
- `ForceStopTracked` 对未初始化组件继续幂等成功；
- 本设计不通过状态归一化触发任何进程操作。

## 前端实时事件策略

初始 Overview 已经是中性状态，但周期刷新可能继续发布未初始化 Helper 的
`component-state` 和 `attention-required`。`nodeStore` 必须维持后端策略不变量：

### Helper 组件状态事件

当当前 Overview 满足 `helperEnabled == false` 且当前 Helper PID 为 0 时：

- incoming Helper state 的 PID 也为 0：忽略该事件；
- incoming Helper state 的 PID 大于 0：接受该事件，显示真实残留进程。

Agent 事件及已启用 Helper 事件保持现有合并逻辑。

### Helper 注意事件

当 Helper 未启用且当前 Helper PID 为 0 时，忽略 component 为 `helper` 的
`attention-required`。如果已经观测到 Helper PID，则不应用该忽略规则。

重新获取 Overview 后，如果新状态是禁用且无 PID，同时 Store 中现有 attention 的
component 为 `helper`，必须清除这条旧 attention，防止禁用前或启动阶段的旧消息
继续占据页面警报。

设置保存成功后必须重新获取 Overview，使 `helperEnabled` 的新值先进入 Store，再由
Store 按新策略处理后续事件。不得只在 React 组件内部缓存禁用状态。

## UI 展示

`StatusBadge` 增加前端专用的禁用展示入口，不改变后端 lifecycle：

- Helper 未启用且 PID 为 0：标签“未启用”，使用中性暂停图标，
  `data-lifecycle="disabled"`；
- 其他情况：继续按真实 lifecycle 显示“已停止”“运行中”或“异常”。

Helper 卡片继续显示：

- 启用状态；
- 启动方式；
- PID；
- 运行/保存配置摘要；
- 最近异常；
- 计划任务漂移。

禁用且无 PID 时最近异常为 `—`，启动、停止和重启按钮全部禁用。禁用但 PID 大于
0 时卡片显示实际状态；停止能力沿用现有生命周期规则，不增加自动操作。

## 数据流

```text
TraySettings.HelperEnabled
        |
        v
Service.GetOverview ---- Helper.Refresh 原始状态
        |                         |
        |  disabled && PID == 0   |
        +------ 中性归一化 <-------+
        |
        v
NodeOverview(helperEnabled + helper state)
        |
        v
nodeStore 事件守卫 ---- component-state / attention-required
        |
        v
OverviewPage + StatusBadge("未启用")
```

## 错误处理边界

| 场景 | 结果 |
|---|---|
| Helper 未启用、无 PID、无配置 | 显示未启用，不显示异常 |
| Helper 未启用、存在真实 PID | 显示实际生命周期和 PID |
| Helper 已启用、配置缺失 | 保留 unavailable/config 错误 |
| Helper 已启用、配置有效、未运行 | 显示已停止 |
| Helper 已禁用但计划任务仍安装 | 显示未启用，同时显示任务漂移 |
| 强制退出未初始化 Helper | 幂等成功，不进入失败列表 |
| 已知 Helper PID 强制退出失败 | 保留 UI 并报告 helper |

## 测试设计

### Go 服务测试

- `GetOverview` 在 Helper 禁用、unavailable 且 PID 为 0 时返回中性停止状态；
- Helper 已启用时不归一化 unavailable；
- Helper 禁用但 PID 大于 0 时保留真实运行状态；
- Helper 禁用时仍保留真实 `HelperTaskDrift`；
- Agent 状态不受 Helper 规则影响。

### TypeScript Store 测试

- 禁用 Helper 的 PID 0 unavailable state 不覆盖中性状态；
- 禁用 Helper 的 attention event 不产生页面警报；
- 禁用 Helper 的 PID 大于 0 state 仍被接受；
- Helper 启用后 unavailable state 和 attention event 重新生效。

### React 组件测试

- 禁用 Helper 卡片显示“未启用”和最近异常 `—`；
- 禁用 Helper 三个操作按钮全部禁用；
- 启用但 unavailable 时仍显示“异常”和错误摘要；
- 计划任务漂移提示不被禁用展示隐藏。

### 回归门禁

- `go test ./internal/nodetray/... ./nodetray`；
- `go test -race ./internal/nodetray/app ./internal/nodetray/production`；
- 前端 Vitest、ESLint、TypeScript 和 Vite build；
- 现有强制退出、Helper 设置保存和 Wails 关闭顺序测试继续通过。

## 验收标准

1. 当前实机场景中 Helper 卡片显示“未启用”，页面不再出现“组件不可用”；
2. 最近异常显示 `—`，三个 Helper 操作按钮均禁用；
3. Agent 和 Worker 状态不受影响；
4. Helper 启用但配置缺失时仍能看到明确错误；
5. 禁用但真实 Helper 仍存活时不会被显示为无进程；
6. 强制退出结果继续只根据真实后台失败决定是否关闭 UI；
7. 未执行真实 Windows 验收时必须记录为 `BLOCKED_NOT_RUN_DYNAMIC`。

## 交付与版本边界

实现后生成新的独立 NodeTray 产物，不覆盖现有产物目录。真实安装目录替换、GUI
确认和进程验收保持单独授权。当前源码目录没有 `.git` 元数据，设计文档标识为
`N/A_NO_GIT_METADATA`，不得初始化或伪造 Git 提交。
