# NodeTray 握手与强制退出误判修复设计

日期：2026-08-03  
状态：方案 A 已确认，等待用户复核本文  
适用范围：Agent/Helper 控制握手、`ForceExitAll` 后台退出判定

## 1. 目标

修复以下两个已复现问题：

1. Agent 已按正确路径和配置启动、Worker 已 Ready，却因控制握手中的启动时间
   来源不同而被 NodeTray 判定为 `unclaimed_instance`；
2. 强制退出时 Agent 和 Worker 已关闭、Helper 从未启动，界面仍报告
   `workers、helper` 并拒绝退出。

修复后必须满足：

- Agent 和 Helper 不再因进程内部取样时间与 Windows 进程创建时间不同而握手失败；
- PID、组件类型、可执行文件路径和配置 SHA-256 仍参与握手；
- 未初始化、未记录 PID 的组件在强制退出中视为“没有需要终止的进程”；
- Worker 状态读取失败不再等同于“Worker 仍存活”；
- 已知 Worker PID 仍逐个等待退出；
- Helper、Agent 和已知 Worker 全部满足退出条件后，才允许关闭 UI。

## 2. 非目标与约束

- 不增加进程签名、文件哈希、用户身份、启动时间容差等复杂安全校验；
- 不扫描或按名称批量终止全机进程；
- 不改变配置格式、控制协议字段和 Wails 前端绑定；
- 不改变普通停止、启动中取消、重启失败短路和配置复读合同；
- 强制退出继续直接终止本次 NodeTray 已记录的 PID，不在终止前重新验证路径或
  启动时间；
- 不把真实 Windows 动态验收替换为单元测试结论。

## 3. 根因

### 3.1 握手启动时间不一致

Agent 和 Helper 把进程内部执行到初始化阶段时的 `time.Now()` 写入
`Status.StartedAtUnixMS`。NodeTray 则通过 Windows `GetProcessTimes` 取得进程真正
创建时间。`process.SameProcess` 要求二者毫秒值完全相同，因此正常进程也会被
拒绝。

配置摘要不是本次根因：Agent 与 NodeTray 使用同一份配置结构和规范 JSON 编码，
对应指纹合同测试已通过。

### 3.2 强制退出把“不可观测”误当成“仍存活”

`ForceExitAll` 在停止 Agent 前读取 Worker 快照。只要快照返回错误，就立即把
`workers` 写入最终失败列表；之后即使 Agent 和 Worker 都退出，该记录也不会被
清除。

Helper 被禁用且配置不存在时，生产工厂保留一个未初始化的 `SharedComponent`。
它没有 Supervisor，也没有任何已记录 PID，但 `ForceStopTracked` 返回
`unavailable`，随后被 `ForceExitAll` 解释成 Helper 未退出。

## 4. 方案选择

采用已确认的方案 A：

1. Windows Inspector 取得的进程身份是启动时间的唯一权威来源；
2. 控制状态中的启动时间保留为兼容字段，但不参与进程认领；
3. 未初始化的共享组件执行强制停止时直接成功；
4. Worker 快照错误不形成独立失败项；Agent 已确认退出时，依赖现有 Windows Job
   Object 的 `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` 合同结束该 Agent 管理的 Worker；
5. 能成功取得的 Worker PID 继续逐个等待，用于发现实际残留。

不采用时间容差。容差无法定义可靠边界，也不能解决两个时间值来源不同的问题。
不让 Agent/Helper 新增 Windows 进程查询逻辑，避免重复实现 NodeTray 已有的进程
身份能力。

## 5. 握手设计

### 5.1 认领条件

控制握手必须同时满足：

- `status.Component == spec.Component`；
- `status.ConfigSHA256 == spec.ExpectedSHA256`；
- `status.PID == inspectedIdentity.PID`；
- `status.ExecutablePath` 与 Inspector 返回的最终路径相同。

`status.StartedAtUnixMS` 不参与上述判断。Inspector 返回的
`Identity.StartedAtUnixMS` 仍用于 NodeTray 内部的 PID 复用防护、等待和状态展示。

### 5.2 新启动流程

```text
NodeTray 启动 Agent
        │
        ├─ Inspector 读取 PID、Windows 创建时间、最终路径
        │
        └─ 控制管道读取 Component、PID、路径、配置 SHA
                         │
                         └─ 比较组件、PID、路径、配置 SHA
                                      │
                                      └─ 使用 Inspector 身份认领
```

Agent 自报时间即使晚于 Windows 创建时间，也不会导致握手失败。状态模型的
`StartedAtUnixMS` 和运行时长必须使用 Inspector 身份计算，不能继续回写自报时间。

### 5.3 已运行进程认领

NodeTray 启动时发现控制管道已有 Agent 或 Helper：

1. 从状态取得 PID；
2. Inspector 按 PID 读取当前 Windows 身份；
3. 比较组件、PID、最终路径和配置 SHA；
4. 把 Inspector 身份传给 Supervisor 完成 Adopt。

不得使用状态中的自报时间构造候选身份，否则重启 NodeTray 后仍会出现同一误判。

## 6. 强制退出设计

### 6.1 未初始化组件

`SharedComponent.ForceStopTracked` 在内部组件为空时返回成功，含义是：本次
NodeTray 从未为该组件建立 Supervisor，也没有可终止的已记录 PID。

普通 `Start`、`Stop`、`Restart` 和 `Refresh` 的 `unavailable` 行为保持不变，避免
把配置缺失误报为可操作状态。

### 6.2 Worker 快照

`ForceExitAll` 仍在终止 Agent 前尝试取得 Worker PID：

- 快照成功：保存所有正 PID，Agent 退出后逐个调用 `WaitPIDGone`；
- 快照失败：不添加通用 `workers` 失败项，继续处理 Helper 和 Agent；
- 已知 PID 等待失败：返回精确的 `worker:<PID>`；
- Agent 强制停止失败：返回 `agent`，不再额外添加无法证明存活的 `workers`。

Worker 由 Agent 为每个进程创建独立 Windows Job Object，并启用
`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`。Agent 被确认终止后，Agent 持有的 Job
句柄由 Windows 关闭，所属 Worker 会被内核结束。因此快照不可用时，Agent 已确认
退出是其管理 Worker 完成退出的依据；快照可用时继续等待具体 Worker PID，提供
更强的残留检测。

### 6.3 退出顺序与成功条件

```text
尝试快照 Worker PID
        │
强制停止 Helper（未初始化则成功）
        │
强制停止 Agent并等待 Agent PID 消失
        │
等待已成功快照的 Worker PID 消失
        │
失败列表为空 ──> 授权 Wails Quit ──> 关闭 UI
```

失败列表只允许出现：

- `helper`：存在已记录 Helper PID但终止或等待失败；
- `agent`：存在已记录 Agent PID但终止或等待失败；
- `worker:<PID>`：已记录 Worker PID等待失败。

不再返回含义模糊的通用 `workers`。

## 7. 错误处理与可观测性

- 握手错误继续使用 `unclaimed_instance`，但摘要区分组件、PID、路径或配置指纹不
  匹配，不再使用“identity or config fingerprint”合并文案；
- Worker 快照错误不进入最终存活列表；本轮不新增日志或事件依赖，底层错误继续
  通过 Worker provider 返回值和 Go 单元测试验证；
- 强制退出失败仍保留确认弹窗和 UI，允许重试；
- 成功结果的 `failedComponents` 必须是空数组而非 `null`；
- 不向 UI、日志或事件输出配置内容、连接串或媒体路径。

## 8. 测试设计

### 8.1 Supervisor 与生产适配层

必须新增或修改以下合同测试：

1. Agent 状态启动时间晚于 Inspector 时间时仍可完成 Start；
2. Helper 状态启动时间晚于 Inspector 时间时仍可完成 Start；
3. Component、PID、路径或配置 SHA 任一不匹配仍拒绝握手；
4. `ComponentState.StartedAtUnixMS` 使用 Inspector 时间；
5. Adopt 忽略状态自报时间并使用 Inspector 身份；
6. Inspector 在 Adopt 前后发生 PID、创建时间或路径变化时仍拒绝认领。

### 8.2 强制退出服务

必须覆盖：

1. 未初始化 Shared Helper 的 `ForceStopTracked` 返回成功；
2. Worker 快照失败、Helper 未初始化、Agent 强制退出成功时整体成功；
3. Worker 快照失败且 Agent 强制退出失败时只返回 `agent`；
4. Worker 快照成功时按快照顺序等待所有正 PID；
5. 已知 Worker PID等待失败时返回 `worker:<PID>`并保留 UI；
6. Helper 或 Agent 失败不阻止其他组件继续处理；
7. 所有后台结果成功后 Backend 才调用 Wails Quit。

### 8.3 回归验证

执行：

- NodeTray Go 全量测试；
- Supervisor、production、app 的 Windows race 测试；
- 前端测试、lint 和生产构建；
- Wails 绑定检查与独立 NodeTray 构建；
- 在真实 Windows 会话执行一次 Agent 启动和一次 Helper 禁用状态的强制退出。

真实动态验收必须记录：Agent PID、两个 Worker PID、握手结果、退出前后进程列表和
UI 是否只在后台退出后关闭。未运行时保持 `BLOCKED_NOT_RUN_DYNAMIC`。

## 9. 影响文件

预计修改：

- `internal/nodetray/supervisor/component.go`
- `internal/nodetray/supervisor/supervisor.go`
- `internal/nodetray/supervisor/component_test.go`
- `internal/nodetray/supervisor/supervisor_test.go`
- `internal/nodetray/production/managed.go`
- `internal/nodetray/production/managed_test.go`
- `internal/nodetray/app/service.go`
- `internal/nodetray/app/service_test.go`
- `docs/deployment/node-tray.md`
- `docs/acceptance/node-tray-lifecycle-repair-2026-08-03.md`

如只修改 Go 返回语义而 Wails 导出签名不变，则不重新生成前端绑定；独立构建仍
必须确认绑定未漂移。

## 10. 验收标准

只有同时满足以下条件，才能声明静态修复完成：

1. 具有不同自报启动时间的 Agent/Helper 握手测试通过；
2. PID、路径、组件和配置 SHA 负向测试仍全部通过；
3. 未初始化 Helper 不再进入强制退出失败列表；
4. Worker 快照错误不再产生通用 `workers`；
5. 已知 Worker PID 未退出时 UI 仍保持打开；
6. 后台成功结果出现后 Backend 才授权关闭 UI；
7. Go 全量、race、前端和独立构建门禁通过；
8. 文档明确区分静态通过与未运行的真实动态验收。
