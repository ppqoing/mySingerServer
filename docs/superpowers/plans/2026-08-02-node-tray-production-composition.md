# 节点托盘生产组合闭包实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task, superpowers:test-driven-development for every implementation task, and superpowers:verification-before-completion before reporting completion. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让普通 Windows 构建的 `nodetray.exe` 具备真实、最小且可测试的生产组合，可在 Wails 启动后管理既有 Agent/Helper。

**Architecture:** 生产代码分为固定部署布局、Windows 进程/控制适配、运行时编排、Wails 生命周期接线四层。所有构造函数无副作用；组件认领/自动启动只发生在 Wails `OnStartup`，释放只发生在 `OnShutdown`。首次运行只创建不含媒体目录或数据库信息的 `tray.json`，不自动生成 Agent/Helper 配置。

**Tech Stack:** Go 1.22+、Wails v2.12.0、Windows API、现有 `internal/nodetray/*` 与 MessagePack 本机控制协议。

## Global Constraints

- 本计划是 UI/发布计划的 Task 9A，完成后才能执行原 Task 9 构建与供应链。
- 每个任务一次基础独立审查，仅报告 Critical 或导致个人工具不可用、越权、凭据泄露的 Important。
- 固定布局：程序在 `%ProgramFiles%\MySingerServer`，Agent/Helper 配置在 `%ProgramData%\MySingerServer\Node`，Tray 设置在 `%LOCALAPPDATA%\MySingerServer\NodeTray`。
- Supervisor 固定 `ReadyTimeout=30s`、`StopTimeout=15s`，不暴露给前端或命令行。
- 首次 Tray 默认：Agent 手动；Helper 禁用且手动；不开机启动；关闭时隐藏；刷新 2 秒；仅重要通知。
- Agent 参数只允许 `<agent.exe> --config <agent.json>`；Helper 继续使用现有手动 UAC 或固定计划任务。
- 不自动创建、复制或覆盖 `agent.json`、`helper.json`，不猜测媒体根、PostgreSQL DSN 或密码。
- `--background` 只改变初始窗口可见性，不扩大权限。
- 默认退出不隐式 Stop/ForceStop Agent、Helper 或 Worker。
- 测试全部使用 fake；禁止运行 GUI、UAC、Agent、Helper、Worker、计划任务、HKCU 或真实配置。
- 当前没有 `.git` 元数据；提交统一记录 `N/A_NO_GIT_METADATA`。

## File Map

```text
internal/nodetray/config/store.go              # 程序路径、首次 Tray 默认、配置摘要
internal/nodetray/process/launcher_windows.go  # Agent 无 shell 启动
internal/nodetray/process/terminator_windows.go# 可信身份复核后的显式终止
internal/nodetray/production/layout.go          # 固定部署布局
internal/nodetray/production/adapters.go        # Validator/Controller/Worker/Opener
internal/nodetray/production/managed.go         # Supervisor/Adopt/Factory
internal/nodetray/production/runtime.go         # 单实例、刷新、事件和关闭
internal/nodetray/production/windows.go         # Windows 原生依赖构造
nodetray/composition.go                         # Backend 生产组合
nodetray/composition_windows.go                 # Windows 普通模式入口
nodetray/elevated_windows.go                    # elevated-once 入口
```

---

### Task 1：固定部署布局、首次设置与配置摘要

**Files:**
- Create: `internal/nodetray/production/layout.go`
- Test: `internal/nodetray/production/layout_test.go`
- Modify: `internal/nodetray/config/store.go`
- Test: `internal/nodetray/config/store_test.go`
- Modify: `nodetray/main.go`
- Test: `nodetray/app_test.go`

**Interfaces:**
- Produces: `ResolveLayout(programFiles, programData, localAppData string) (Layout, error)`。
- Produces: `DefaultTraySettings() traymodel.TraySettings`。
- Produces: `config.Paths{TraySettings, AgentConfig, HelperConfig, AgentExecutable, HelperExecutable}`。
- Produces: `EnsureTraySettings(defaults)`、`AgentFingerprint()`、`HelperFingerprint()`。

- [ ] **Step 1: 写 RED 测试**

```go
func TestResolveLayoutSeparatesProgramsConfigsAndUserSettings(t *testing.T) {
    got, err := ResolveLayout(`C:\Program Files`, `C:\ProgramData`, `C:\Users\u\AppData\Local`)
    require.NoError(t, err)
    assert.Equal(t, `C:\Program Files\MySingerServer\nodetray.exe`, got.TrayExecutable)
    assert.Equal(t, `C:\ProgramData\MySingerServer\Node\agent.json`, got.AgentConfig)
    assert.Equal(t, `C:\Users\u\AppData\Local\MySingerServer\NodeTray\tray.json`, got.TraySettings)
}
```

还要覆盖：缺失 `tray.json` 只创建冻结默认；损坏文件 fail-closed；共享 Agent/Helper 校验使用注入 exe；摘要来自严格规范 JSON；`--background` 合法而未知参数拒绝。

- [ ] **Step 2: 运行 RED**

```powershell
go test ./internal/nodetray/production ./internal/nodetray/config ./nodetray -run 'TestResolveLayout|TestDefaultTray|TestStoreUsesInjectedExecutable|TestFingerprint|TestParseLaunchMode' -count=1
```

- [ ] **Step 3: 最小实现**

```go
type Layout struct {
    TrayExecutable, AgentExecutable, HelperExecutable string
    TraySettings, AgentConfig, HelperConfig string
    AgentLogs, HelperLogs string
}
```

`DefaultTraySettings` 返回全局约束中的固定值。Store 不再从 ProgramData 配置目录推导 exe。`launchMode` 增加 `background bool`，只允许无参数、单个 `--background` 或原 elevated-once 五参数。

- [ ] **Step 4: 运行 GREEN**

```powershell
go test ./internal/nodetray/production ./internal/nodetray/config ./nodetray -count=1
go test ./internal/nodetray/production ./internal/nodetray/config ./nodetray -count=20
```

- [ ] **Step 5: 记录 `N/A_NO_GIT_METADATA` 并做一次基础审查**

---

### Task 2：Agent 启动器与可信 Terminator

**Files:**
- Create/Test: `internal/nodetray/process/launcher_windows.go`, `launcher_windows_test.go`
- Create/Test: `internal/nodetray/process/terminator_windows.go`, `terminator_windows_test.go`
- Create: `internal/nodetray/process/launcher_stub.go`, `terminator_stub.go`

**Interfaces:**
- Produces: `NewAgentLauncher(inspector Inspector) supervisor.Launcher`。
- Produces: `NewTrustedTerminator(inspector Inspector) supervisor.Terminator`。

- [ ] **Step 1: 写 RED 测试**：精确参数、无 shell/任意 env；PID/创建时间/最终路径任一漂移时零终止；所有句柄关闭。
- [ ] **Step 2: 运行 RED**

```powershell
go test ./internal/nodetray/process -run 'TestAgentLauncher|TestTrustedTerminator' -count=1
```

- [ ] **Step 3: 最小实现**：启动器只允许 `exec.CommandContext(executable, "--config", configPath)` 且隐藏窗口，启动后由 Inspector 取得 Identity。Terminator 以最小权限打开句柄，重新核对完整 Identity 后才 `TerminateProcess`。
- [ ] **Step 4: 运行 GREEN**

```powershell
go test ./internal/nodetray/process -count=20
$env:GOOS='windows'; $env:GOARCH='amd64'; go test ./internal/nodetray/process -count=1
```

- [ ] **Step 5: 记录 `N/A_NO_GIT_METADATA` 并做一次基础审查**

---

### Task 3：Validator、动态指纹、固定 Controller、Worker 与目录打开

**Files:**
- Create/Test: `internal/nodetray/production/adapters.go`, `adapters_test.go`
- Modify/Test: `internal/nodetray/config/store.go`, `store_test.go`
- Modify/Test: `internal/nodetray/app/service.go`, `service_test.go`
- Modify/Test: `internal/nodetray/supervisor/supervisor.go`, `supervisor_test.go`

**Interfaces:**
- Produces: `NewValidator(store) app.Validator`。
- Produces: 只拨 `AgentPipeName()`/`HelperPipeName()` 的固定 Controller。
- Produces: 只映射 Agent Status.Workers 的 WorkerProvider。
- Produces: 只打开四个冻结 LocationKind 的 LocationOpener。
- Produces: `Supervisor.UpdateSpec(expectedSHA256 string)`，保存成功后更新下一次 Start/Adopt 的摘要。

- [ ] **Step 1: 写 RED 测试**：component/machine/SHA 漂移拒绝；保存后新摘要生效；未知 Location 零 Explorer；Validator 错误不含密码/DSN。
- [ ] **Step 2: 运行 RED**

```powershell
go test ./internal/nodetray/production ./internal/nodetray/app ./internal/nodetray/supervisor -run 'TestController|TestFingerprint|TestWorkerProvider|TestLocationOpener|TestUpdateSpec' -count=1
```

- [ ] **Step 3: 最小实现**：Controller 每次 Status 核对固定 component、machine ID 和当前摘要；SaveAgent/Helper 成功后串行更新 Supervisor spec。旧运行实例只能显示 drift/unclaimed。
- [ ] **Step 4: 运行 GREEN 与泄露扫描**

```powershell
go test ./internal/nodetray/production ./internal/nodetray/app ./internal/nodetray/supervisor -count=20
rg -n "postgres(ql)?://|password\s*[:=]" internal/nodetray/production
```

- [ ] **Step 5: 记录 `N/A_NO_GIT_METADATA` 并做一次基础审查**

---

### Task 4：Managed Factory、单实例、刷新和 Wails 生命周期

**Files:**
- Create/Test: `internal/nodetray/production/managed.go`, `managed_test.go`
- Create/Test: `internal/nodetray/production/runtime.go`, `runtime_test.go`
- Create: `internal/nodetray/production/windows.go`
- Modify/Test: `internal/nodetray/bootstrap/bootstrap.go`, `bootstrap_test.go`
- Modify/Test: `nodetray/app.go`, `app_test.go`

**Interfaces:**
- Produces: 包装 Supervisor 的 `ManagedComponent`。
- Produces: `Runtime.Start(context.Context) error` 与幂等 `Close() error`。
- Produces: 单实例 lease/activation、刷新 scheduler、固定 Wails emitter。

- [ ] **Step 1: 写 RED 测试**：第二实例只 signal 且零组件启动；Wails Startup 前零组件启动；Shutdown/默认 Exit 零 Stop；慢前端不阻塞；Adopt 经过 Controller、Inspector、Supervisor 三重复核。
- [ ] **Step 2: 运行 RED**

```powershell
go test ./internal/nodetray/production ./internal/nodetray/bootstrap ./nodetray -run 'TestSecondInstance|TestRuntimeStartsOnly|TestRuntimeEvent|TestManagedAdopt' -count=1
```

- [ ] **Step 3: 最小实现**：Factory 使用 Task 1 固定路径、Task 2 进程适配、Task 3 当前摘要和 30s/15s 超时。Runtime 在 Start 中取得当前用户 lease；重复实例发送固定 activation frame；刷新只发固定 `component-state`、`worker-summary`、`attention`。
- [ ] **Step 4: Backend 生命周期接线**：`Startup` 保存 Wails context 后调用 runtime.Start；`Shutdown` 幂等关闭 ticker、订阅、listener、lease，不停止组件。后台模式只隐藏初始窗口。
- [ ] **Step 5: 运行 GREEN**

```powershell
go test ./internal/nodetray/production ./internal/nodetray/bootstrap ./nodetray -count=1
go test ./internal/nodetray/production ./internal/nodetray/bootstrap ./nodetray -count=20
```

- [ ] **Step 6: 记录 `N/A_NO_GIT_METADATA` 并做一次基础审查**

---

### Task 5：普通 production composition 与 elevated-once

**Files:**
- Create/Test: `nodetray/composition.go`, `composition_test.go`
- Create/Test: `nodetray/composition_windows.go`, `composition_windows_test.go`
- Create: `nodetray/composition_stub.go`
- Create/Test: `nodetray/elevated_windows.go`, `elevated_windows_test.go`
- Modify: `nodetray/main.go`, `composition_bindings.go`

**Interfaces:**
- Produces: 普通 Windows build 的真实 `composeBackend()`。
- Produces: 固定 Helper config/task 能力的 `runElevatedOnce(pipe, nonce)`。

- [ ] **Step 1: 写 RED 测试**：构造后所有 Service 依赖非空且零进程/注册表/UAC调用；elevated Executor 只持有固定 Helper 路径、固定任务、`CapabilityElevated`。
- [ ] **Step 2: 运行 RED**

```powershell
$env:GOOS='windows'; $env:GOARCH='amd64'; go test ./nodetray -run 'TestProductionComposition|TestElevatedOnce' -count=1
```

- [ ] **Step 3: 普通组合**：Windows init 只安装无副作用构造函数；真实启动仍在 Wails Startup。bindings 构造只用于生成 20 个绑定，不能触发 OS 副作用。
- [ ] **Step 4: elevated-once**：已校验 pipe/nonce 后调用 `elevation.ServeOnce`，Handler 为固定 `elevated.NewExecutor(helperConfig, elevatedTaskService, taskDefinition, CapabilityElevated)`。
- [ ] **Step 5: 运行 GREEN**

```powershell
$env:GOOS='windows'; $env:GOARCH='amd64'; go test ./nodetray -count=20
go test ./nodetray ./internal/nodetray/... -count=1
```

- [ ] **Step 6: 记录 `N/A_NO_GIT_METADATA` 并做一次基础审查**

---

### Task 6：Task 9A 静态验收

**Files:**
- Create: `.superpowers/sdd/2026-08-02-node-tray-ui-release/task-9a-report.md`
- Create: `.superpowers/sdd/2026-08-02-node-tray-ui-release/task-9a-review.md`
- Modify: `.superpowers/sdd/2026-08-02-node-tray-ui-release/progress.md`

- [ ] **Step 1: 完整无副作用门禁**

```powershell
go test ./nodetray ./internal/nodetray/... -count=1
go test ./nodetray ./internal/nodetray/... -count=20
go vet ./nodetray ./internal/nodetray/...
go build -buildvcs=false ./nodetray
```

- [ ] **Step 2: 静态边界检查**

```powershell
rg -n "cmd\.exe|powershell|schtasks|taskkill|postgres(ql)?://|password\s*[:=]" nodetray internal/nodetray
go test ./nodetray -run 'TestManifestRequestsAsInvoker|TestProductionComposition' -count=1
```

- [ ] **Step 3: 只做一次基础总审查**：仅检查 composition 仍不可用、默认退出误停组件、身份绕过、UAC/任务越权、凭据泄露五类硬问题。
- [ ] **Step 4: 记录边界**：静态 PASS 不代表 WebView2、GUI、UAC、组件、任务、HKCU、PostgreSQL 动态验收；原 Task 11 继续 `BLOCKED_NOT_RUN_DYNAMIC`，直到明确授权。
- [ ] **Step 5: 记录 `N/A_NO_GIT_METADATA`**

## Self-Review

- 覆盖普通模式、`--background`、elevated-once、固定路径、首次设置、启停/认领、动态摘要、Worker 只读、单实例、事件和关闭顺序。
- 没有未冻结的路径、默认值、超时或权限能力。
- 新类型在首次产生任务中定义，后续任务只消费已经定义的接口。
- 每任务一次基础审查，不增加风格或增强型审查。
