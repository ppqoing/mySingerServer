# NodeTray 生命周期修复验收记录（2026-08-03）

## 结论

本轮已完成“修改配置 → 启动 → 重启 → 关闭”相关阻塞性流程修复，并生成独立
`nodetray.exe`。Go、竞态、前端测试、前端构建和独立发布构建均实际通过。

真实 Windows GUI、UAC、计划任务、HKCU 和已安装后台进程没有在本轮运行；这些
项目保持 `BLOCKED_NOT_RUN_DYNAMIC`，不以静态测试替代。

| 门禁 | 状态 | 实际结果 |
|---|---|---|
| NodeTray Go 全量测试 | `PASS` | 14 个包通过 |
| Supervisor/app 竞态测试 | `PASS` | 2 个包通过，未报告 data race |
| 前端测试 | `PASS` | 19 个测试文件、94 个测试通过 |
| 前端 lint | `PASS` | 0 error；保留 3 个 Fast Refresh warning |
| 前端 TypeScript/Vite 生产构建 | `PASS` | 构建成功 |
| 独立 NodeTray 发布构建 | `PASS` | Wails v2.12.0、Windows amd64、`asInvoker` |
| 真实 Windows 交互验收 | `BLOCKED_NOT_RUN_DYNAMIC` | 本轮未操作真实 GUI、后台进程或系统配置 |

源码目录没有 `.git` 元数据，本批次版本标识为 `N/A_NO_GIT_METADATA`。

## 已修复的阻塞性问题

1. 启动期间点击停止会取消就绪等待，强制结束本次刚创建的 PID，并返回稳定的
   `start_cancelled`，不再等待完整启动超时。
2. 同一组件的启停操作串行化；冲突返回 `operation_conflict`。重启的停止阶段失败
   时立即结束，不会启动第二个实例。
3. 配置原子替换后使用正式 loader 复读并比较规范内容；失败返回
   `save_verify_failed`，且不会继续启动或重启。
4. 界面展示的运行配置 SHA-256、已保存配置 SHA-256 和 `needsRestart` 全部来自
   后端，避免前后端摘要算法漂移。
5. 程序设置只执行实际发生变化的登录项或 Helper 策略操作；普通设置保存不会
   触发 UAC。UAC 取消时不写入磁盘，后期失败返回 `settings_partially_applied` 并
   回读实际状态。
6. 托盘启停命令异步派发，避免菜单回调阻塞 UI；启动中只允许“取消启动”。
7. 窗口关闭、托盘退出和设置页退出汇入同一确认弹窗。确认后只调用一次
   `ForceExitAll`；后端先强制结束 Helper，再结束 Agent，并等待退出前快照中的
   Worker PID 全部消失，最后才关闭 UI。任何失败都会保留 UI 并允许重试。

按个人使用要求，强制退出直接使用本次运行记录的 PID，不做路径、启动时间或
身份二次核验。

## 自动化合同证据

### 启动、停止和重启

- `TestStopCancelsStartingHandshakeWithoutWaitingForReadyTimeout`
- `TestRestartCompletesStopBeforeStartingAgain`
- `TestAutomaticHelperRestartShortCircuitsBeforeTaskRunWhenControlledStopFails`
- `TestSaveAndRestartAgentUsesSaveStopStartOrderAndShortCircuits`

### 配置保存和摘要

- `TestStoreSaveAgentUsesActualCanonicalBytesForSHAAndPreservesSecretState`
- `TestStoreSaveAgentRejectsFormalTargetChangedImmediatelyAfterReplace`
- `TestSaveAgentMapsFormalRereadFailureToStableVerifyCode`
- `TestSaveAgentReturnsFormalDigestAndRuntimeDrift`

### 设置差异应用

- `TestSaveTraySettingsOrdinaryChangeSkipsLoginWritesAndElevation`
- `TestSaveTraySettingsUACCancelDoesNotPersistRequestedPolicy`
- `TestSaveTraySettingsLateFailureReturnsPartiallyAppliedAndReloadsActualState`

### 强制退出和 UI 关闭顺序

- `TestForceExitAllForcesEveryBackgroundComponentBeforeSuccess`
- `TestForceExitAllContinuesAfterFailureAndAggregatesSurvivors`
- `TestForceExitAllAuthorizesWailsQuitOnlyAfterBackgroundSuccess`
- `TestForceExitAllFailureKeepsUIOpen`
- `ExitDialog` 前端测试验证取消无副作用、确认只调用一次 `ForceExitAll`、失败后
  保留弹窗并允许重试。

## 实际执行的验证

| 范围 | 命令摘要 | 结果 |
|---|---|---|
| Go 全量 | `go test ./internal/nodetray/... ./nodetray -count=1 -timeout 180s` | 退出码 0 |
| Go race | `go test -race ./internal/nodetray/supervisor ./internal/nodetray/app -count=1 -timeout 180s` | 退出码 0 |
| 前端测试 | `npm test` | 19/19 文件、94/94 测试通过 |
| 前端 lint | `npm run lint` | 退出码 0，0 error、3 warning |
| 前端构建 | `npm run build` | 退出码 0 |
| Wails 绑定 | `wails v2.12.0 generate module` | 退出码 0；旧 `ExitTray` 绑定已移除 |
| 独立构建 | `scripts/build-nodetray.ps1 ... -OutDir artifacts/nodetray-lifecycle-repair` | 退出码 0，最终状态 PASS |

第一次独立构建只因沙箱拒绝默认 AppData Go 缓存而退出；改用仓库内
`GOCACHE`/`GOTMPDIR` 后以同一源码成功复跑。该环境问题不属于产品缺陷。

计划中提到的 `scripts/Test-NodeTraySupplyChain.ps1` 在当前源码树不存在，因此未能
单独运行。现有 `scripts/build-nodetray.ps1` 已在成功构建中完成 Wails、WebView2、
PE 架构、执行级别和发布闭包检查；不得把缺失的独立脚本写成单独 `PASS`。

## 发布产物

产物目录：`D:\code\mySingerServer\artifacts\nodetray-lifecycle-repair`

| 文件 | 大小 | SHA-256 | 签名 |
|---|---:|---|---|
| `nodetray.exe` | 13,144,064 字节 | `CB3B3FFFBCC6663139FBE15FF2185252AA8AC8D5331F6B9F5F953DFBF43344CD` | `NotSigned` |
| `MicrosoftEdgeWebview2Setup.exe` | 1,793,816 字节 | `7EBC4CE80143EF89CEA86A61EA151502868DB6CAAA678B8B43660A66ACE11C3A` | `Valid` |

`nodetray.exe` 未签名是当前构建事实，不影响本轮个人使用流程验证，但发布到其他
机器前应按部署要求单独处理。

## 未运行的真实动态项目

| 项目 | 状态 | 原因 |
|---|---|---|
| 实际窗口关闭、托盘退出和统一确认弹窗 | `BLOCKED_NOT_RUN_DYNAMIC` | 未运行真实 GUI |
| Agent/Worker 启动中取消、重启、强制退出 | `BLOCKED_NOT_RUN_DYNAMIC` | 未操作真实后台进程 |
| Helper UAC、计划任务和强制退出 | `BLOCKED_NOT_RUN_DYNAMIC` | 未触发 UAC 或计划任务 |
| 登录启动差异修改 | `BLOCKED_NOT_RUN_DYNAMIC` | 未修改 HKCU |
| WebView2 缺失、取消和失败模拟 | `BLOCKED_NOT_RUN_DYNAMIC` | 未运行交互式隔离模拟 |

上述动态项只有在真实 Windows 会话实际执行并记录结果后，才能改写为 `PASS` 或
`FAIL`。

## 握手与强制退出误判修复批次

### 结论

本批次修复了以下两个现场问题：

1. Agent/Helper 控制状态自报启动时间与 Windows 进程创建时间存在毫秒差异时，
   NodeTray 不再错误返回 `control handshake identity or config fingerprint does not match`；
2. Worker 快照不可用或 Helper 从未初始化时，统一强制退出不再错误显示
   `workers、helper`。已知且未退出的 Worker 仍精确报告为 `worker:<PID>`。

后台组件全部处理成功后才授权 Wails 关闭 UI；Agent、Helper 或已知 Worker PID
仍失败时继续保留界面。当前源码目录仍无 `.git` 元数据，版本标识为
`N/A_NO_GIT_METADATA`。

| 合同 | 状态 | 自动化证据 |
|---|---|---|
| Agent/Helper 自报启动时间不参与握手认领 | `PASS` | `TestStatusClaimRequiresComponentPIDPathAndConfigFingerprint`、`TestStartAcceptsSelfReportedTimeDriftAndPublishesInspectorTime` |
| UI 状态使用 Inspector 创建时间 | `PASS` | `TestStartAcceptsSelfReportedTimeDriftAndPublishesInspectorTime`、`TestManagedAdoptUsesInspectedCreationTimeInsteadOfReportedTime` |
| Inspector 内部完整身份漂移仍拒绝 | `PASS` | `TestSameProcessRejectsEveryIdentityDrift`、`TestManagedAdoptRejectsInspectedIdentityDriftInsideSupervisor` |
| 未初始化 Helper 强制停止幂等成功 | `PASS` | `TestUninitializedSharedComponentForceStopIsIdempotent` |
| Worker 快照失败不生成通用 `workers` | `PASS` | `TestForceExitAllIgnoresWorkerSnapshotFailureWhenTrackedComponentsExit`、`TestForceExitAllSnapshotFailureAndAgentFailureReportsOnlyAgent` |
| 已知 Worker PID 残留继续阻止 UI 退出 | `PASS` | `TestForceExitAllContinuesAfterFailureAndAggregatesSurvivors`、`TestForceExitAllFailureKeepsUIOpen` |
| 真实 Windows Agent 启动与统一强制退出 | `BLOCKED_NOT_RUN_DYNAMIC` | 本批次未替换已安装程序，也未启动真实 Agent/Helper/Worker |

### RED 到 GREEN 证据

- `SamePIDAndExecutable` 测试最初因函数不存在而编译失败；实现 PID 与最终路径比较
  后 `internal/nodetray/process` 通过。
- 自报时间偏差 Adopt 测试最初得到 `identity_mismatch`；改用 Inspector 身份后
  `internal/nodetray/production` 通过。
- 未初始化 SharedComponent 测试最初得到 `unavailable`；仅将
  `ForceStopTracked` 改为幂等成功后通过。
- Worker 快照失败测试最初分别得到 `workers` 和 `workers、agent`；移除通用
  Worker 存活推断后通过。

### 实际执行的门禁

| 范围 | 实际命令摘要 | 结果 |
|---|---|---|
| Go 全量 | `go test ./internal/nodetray/... ./nodetray -count=1 -timeout 180s` | 退出码 0，14 个包通过 |
| Windows race | `go test -race ./internal/nodetray/supervisor ./internal/nodetray/production ./internal/nodetray/app -count=1 -timeout 180s` | 退出码 0，3 个包通过，无 `DATA RACE` |
| 前端测试 | `npm test` | 19/19 文件、94/94 测试通过 |
| 前端 lint | `npm run lint` | 退出码 0，0 error、3 个既有 Fast Refresh warning |
| TypeScript/Vite | `npm run build` | 退出码 0，39 个模块完成生产构建 |
| Wails 绑定静态检查 | 检查 `ForceExitAll`、`ForceExitResult` 并搜索旧 `ExitTray(` | 当前绑定存在，旧绑定不存在 |
| 独立构建 | `scripts/build-nodetray.ps1 -Go C:\tmp\go1.26.5\go\bin\go.exe -Npm D:\application\nodejs\npm.cmd -OutDir artifacts\nodetray-handshake-force-exit-fix` | 退出码 0，最终 JSON 状态 `PASS` |

构建报告为 Go 1.26.5、Wails v2.12.0、Windows amd64、执行级别
`asInvoker`。构建脚本同时重新执行了前端和 Go 门禁，结果仍通过。

### 发布产物

产物目录：`D:\code\mySingerServer\artifacts\nodetray-handshake-force-exit-fix`

| 文件 | 大小 | SHA-256 | 签名 |
|---|---:|---|---|
| `nodetray.exe` | 13,146,112 字节 | `7F01AC4ECE63474DF5CF6614434FE135F9ABFD679D5C70318B6BA3A3EC5598AB` | `NotSigned` |
| `MicrosoftEdgeWebview2Setup.exe` | 1,793,816 字节 | `7EBC4CE80143EF89CEA86A61EA151502868DB6CAAA678B8B43660A66ACE11C3A` | `Valid` |

### 动态验收边界

本批次没有覆盖或启动 `C:\Program Files\MySingerServer` 中的真实程序，没有触发
UAC、计划任务或 HKCU 修改，也没有运行真实 Agent/Helper/Worker。以下项目保持
`BLOCKED_NOT_RUN_DYNAMIC`：

- Agent 启动后不再出现握手异常；
- `helperEnabled=false` 时统一强制退出不再显示 `helper`；
- Agent 退出触发 Job Object 关闭后真实 Worker 全部退出；
- 后台全部退出后真实 UI 进程关闭。
