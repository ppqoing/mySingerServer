# NodeTray Lifecycle Repair Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复 NodeTray 修改配置、启动、取消启动、停止、重启和退出流程中的阻塞性问题；所有明确退出入口统一确认，确认后先强制结束全部已记录后台进程，全部确认退出后再关闭 UI 进程。

**Architecture:** 保留现有 Wails UI、`app.Service`、Supervisor actor、Windows 进程适配器四层边界。新增后端单一 `ForceExitAll` 协调器和一次性 Wails 退出授权；Supervisor 把长耗时操作移出命令循环并支持取消启动；配置保存返回正式文件 SHA-256 与运行漂移状态；程序设置按差异执行登录项和提权计划任务修改。

**Tech Stack:** Go 1.26.5、Wails v2.12.0、Windows API、React 19、TypeScript、Vitest、Testing Library、PowerShell 7。

## Global Constraints

- 本计划以已确认设计 [2026-08-03-node-tray-lifecycle-repair-design.md](../specs/2026-08-03-node-tray-lifecycle-repair-design.md) 为唯一产品语义基线，并取代旧计划 `2026-08-03-node-tray-exit-all.md`。
- 这是一个共享契约变更，Go 模型、Wails 绑定、前端类型与页面必须在同一实施序列内完成，不拆成互不兼容的并行改动。
- 所有显式退出入口都只打开同一个确认弹窗；取消不得修改后台或 UI 状态。
- 用户确认退出后跳过优雅停止，顺序固定为 Helper、Agent/Worker、UI；只要任一已记录后台进程未确认退出，UI 就不得关闭。
- 强制退出只使用 Supervisor 已记录 PID，不复核路径、启动时间、组件类型、配置摘要或管道握手；不得扫描进程名，也不得结束未跟踪进程。
- 普通停止和重启仍走优雅协议；取消正在启动的操作时可结束这次启动刚创建的 PID。
- 普通程序设置不得触发 UAC；只有 Helper 启动策略变化才调用计划任务提权接口。
- 不引入通用事务框架、复杂权限代理或退出时安全复核。
- 不操作当前机器上已安装的 Agent、Worker、Helper、计划任务、HKCU 登录项和真实 UAC。动态验收保持 `BLOCKED_NOT_RUN_DYNAMIC`，直至用户另行授权。
- 当前目录缺少可用 `.git` 元数据。每个任务末尾执行状态检查，但不得伪造提交、分支或提交哈希。
- 实施每个任务时必须先使用 `superpowers:test-driven-development`；准备声明完成前必须使用 `superpowers:verification-before-completion`。

---

## Task 1: 固化跨层结果模型和配置漂移字段

**Files:**

- Modify: `internal/nodetray/traymodel/model.go`
- Modify: `internal/nodetray/traymodel/model_test.go`

**Consumes:** 已有 `OperationResult`、`ComponentState` JSON 契约。

**Produces:** `ForceExitResult`、`ConfigApplyResult`，以及 `runtimeConfigSha256`、`savedConfigSha256`、`needsRestart`。

- [ ] **Step 1: 先写失败的 JSON 契约测试**

```go
func TestLifecycleRepairResultJSONContract(t *testing.T) {
	force := ForceExitResult{
		OK:               false,
		FailedComponents: []string{"helper", "worker:42"},
		ErrorCode:        "force_exit_failed",
		ErrorSummary:     "后台进程仍在运行",
	}
	config := ConfigApplyResult{
		OK:           true,
		Saved:        true,
		Restarted:    false,
		SHA256:       strings.Repeat("a", 64),
		NeedsRestart: true,
	}
	state := ComponentState{
		RuntimeConfigSHA256: strings.Repeat("b", 64),
		SavedConfigSHA256:   strings.Repeat("a", 64),
		NeedsRestart:        true,
	}

	assertJSONKeys(t, force, "ok", "failedComponents", "errorCode", "errorSummary")
	assertJSONKeys(t, config, "ok", "saved", "restarted", "sha256", "needsRestart", "errorCode", "errorSummary")
	assertJSONKeys(t, state, "runtimeConfigSha256", "savedConfigSha256", "needsRestart")
}
```

- [ ] **Step 2: 运行测试并确认 RED**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/nodetray/traymodel -run LifecycleRepair -count=1
```

预期：编译失败，提示三个新类型或字段不存在。

- [ ] **Step 3: 添加最小模型实现**

```go
type ForceExitResult struct {
	OK               bool     `json:"ok"`
	FailedComponents []string `json:"failedComponents"`
	ErrorCode        string   `json:"errorCode"`
	ErrorSummary     string   `json:"errorSummary"`
}

type ConfigApplyResult struct {
	OK           bool   `json:"ok"`
	Saved        bool   `json:"saved"`
	Restarted    bool   `json:"restarted"`
	SHA256       string `json:"sha256"`
	NeedsRestart bool   `json:"needsRestart"`
	ErrorCode    string `json:"errorCode"`
	ErrorSummary string `json:"errorSummary"`
}
```

在 `ComponentState` 中加入：

```go
RuntimeConfigSHA256 string `json:"runtimeConfigSha256"`
SavedConfigSHA256   string `json:"savedConfigSha256"`
NeedsRestart        bool   `json:"needsRestart"`
```

- [ ] **Step 4: 运行模型包全部测试并确认 GREEN**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/nodetray/traymodel -count=1
```

- [ ] **Step 5: 检查点**

确认 JSON 字段名与设计文档完全一致，且未修改已有字段名。

---

## Task 2: 提供无需身份复核的已跟踪 PID 终止与等待适配器

**Files:**

- Modify: `internal/nodetray/process/terminator_windows.go`
- Modify: `internal/nodetray/process/terminator_windows_test.go`
- Modify: `internal/nodetray/process/terminator_stub.go`
- Create: `internal/nodetray/process/waiter_windows.go`
- Create: `internal/nodetray/process/waiter_windows_test.go`
- Create: `internal/nodetray/process/waiter_stub.go`
- Modify: `nodetray/composition_windows.go`
- Modify: `nodetray/composition_windows_test.go`

**Consumes:** Supervisor 已保存的 `process.Identity.PID`。

**Produces:** `DirectTerminator` 与 `PIDWaiter`；二者只按 PID 操作，不调用身份检查器。

- [ ] **Step 1: 先写失败的 Windows 单元测试**

用包内可替换 Win32 调用测试以下事实：

```go
func TestDirectTerminatorUsesRecordedPIDWithoutIdentityInspection(t *testing.T) {
	backend := newFakeTerminateBackend()
	terminator := newDirectTerminator(backend)

	err := terminator.Terminate(Identity{PID: 321}, 1)

	require.NoError(t, err)
	require.Equal(t, uint32(321), backend.openedPID)
	require.Equal(t, uint32(1), backend.exitCode)
}

func TestPIDWaiterReturnsOnlyAfterHandleIsSignaled(t *testing.T) {
	backend := newFakeWaitBackend(windows.WAIT_OBJECT_0)
	waiter := newPIDWaiter(backend)

	err := waiter.WaitPIDGone(context.Background(), 654)

	require.NoError(t, err)
	require.Equal(t, uint32(654), backend.openedPID)
}
```

另加无效 PID、`OpenProcess` 失败、等待超时和上下文取消测试。

- [ ] **Step 2: 运行测试并确认 RED**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/nodetray/process -run 'DirectTerminator|PIDWaiter' -count=1
```

- [ ] **Step 3: 实现直接终止器**

```go
type DirectTerminator struct {
	backend terminatorBackend
}

func NewDirectTerminator() *DirectTerminator {
	return newDirectTerminator(nativeDirectTerminatorBackend{})
}

func (t *DirectTerminator) Terminate(identity Identity, exitCode uint32) error {
	if identity.PID <= 0 {
		return errors.New("tracked pid must be positive")
	}
	if t == nil || t.backend == nil {
		return errors.New("direct termination is unavailable")
	}
	handle, err := t.backend.OpenForTerminate(identity.PID)
	if err != nil {
		return fmt.Errorf("open tracked pid %d: %w", identity.PID, err)
	}
	if handle == 0 {
		return fmt.Errorf("open tracked pid %d returned an empty handle", identity.PID)
	}
	defer t.backend.CloseProcessHandle(handle)
	if err := t.backend.Terminate(handle, exitCode); err != nil {
		return fmt.Errorf("terminate tracked pid %d: %w", identity.PID, err)
	}
	return nil
}
```

`nativeDirectTerminatorBackend.OpenForTerminate` 只请求 `PROCESS_TERMINATE`，不请求查询权限。保留现有 `TrustedTerminator` 供历史调用和测试使用，但生产 Supervisor 改为注入 `NewDirectTerminator()`。

- [ ] **Step 4: 实现只等待 PID 消失的适配器**

公开契约固定为：

```go
type PIDWaiter interface {
	WaitPIDGone(context.Context, int) error
}
```

Windows 实现用 `OpenProcess(SYNCHRONIZE, false, pid)` 获取句柄，以 100ms 为片轮询 `WaitForSingleObject`，每片检查 `ctx.Done()`；`ERROR_INVALID_PARAMETER` 视为目标已经退出。不得调用 `Inspect`、比较路径或启动时间。

- [ ] **Step 5: 更新 Windows 生产组合**

```go
terminator := process.NewDirectTerminator()
pidWaiter := process.NewPIDWaiter()
```

同一个直接终止器注入 Agent 和 Helper Supervisor；`pidWaiter` 留给 Task 5 的退出协调器依赖。

- [ ] **Step 6: 运行包测试并确认 GREEN**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/nodetray/process ./nodetray -run 'DirectTerminator|PIDWaiter|ProductionComposition' -count=1
```

- [ ] **Step 7: 检查点**

搜索生产组合，确认 `NewTrustedTerminator` 不再注入 Supervisor，同时保留“不扫描进程名”的边界。

---

## Task 3: 把 Supervisor 长操作移出 actor 循环并支持取消启动

**Files:**

- Modify: `internal/nodetray/supervisor/supervisor.go`
- Modify: `internal/nodetray/supervisor/supervisor_test.go`
- Modify: `internal/nodetray/supervisor/component.go`
- Modify: `internal/nodetray/supervisor/component_test.go`
- Modify: `internal/nodetray/production/managed.go`
- Modify: `internal/nodetray/production/managed_test.go`
- Modify: `internal/nodetray/app/service.go`
- Modify: `internal/nodetray/app/service_test.go`

**Consumes:** `Launcher`、`Inspector`、`Terminator`、现有状态事件。

**Produces:** 响应式 actor、`ForceStopTracked`、`start_cancelled` 和 `operation_conflict`。

- [ ] **Step 1: 先写并发回归测试**

```go
func TestStopCancelsStartWhileActorRemainsResponsive(t *testing.T) {
	fixture := newBlockedStartFixture(t)
	startDone := make(chan traymodel.OperationResult, 1)
	go func() { startDone <- fixture.supervisor.Start(context.Background()) }()
	fixture.awaitLifecycle(t, traymodel.LifecycleStarting)

	stop := fixture.supervisor.Stop(context.Background())
	start := <-startDone

	require.True(t, stop.OK)
	require.False(t, start.OK)
	require.Equal(t, "start_cancelled", start.ErrorCode)
	require.Equal(t, traymodel.LifecycleStopped, fixture.supervisor.State().Lifecycle)
}

func TestRestartDoesNotLaunchSecondProcessWhenStopFails(t *testing.T) {
	fixture := newRunningFixtureWithStopError(t)

	result := fixture.supervisor.Restart(context.Background())

	require.False(t, result.OK)
	require.Equal(t, 0, fixture.launcher.launchCount)
}
```

再加入“Start 期间再次 Start 返回 `operation_conflict`”“Stop 期间 Restart 返回 `operation_conflict`”和 actor 关闭时回复所有等待者的测试。
另加“状态显示 stopped 但仍保留 PID 时，`ForceStopTracked` 仍终止并等待该 PID”的回归测试。

- [ ] **Step 2: 运行定向测试并确认 RED 或超时**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/nodetray/supervisor -run 'CancelsStart|SecondProcess|OperationConflict' -count=1 -timeout 20s
```

- [ ] **Step 3: 改造 actor 消息模型**

命令循环只做状态转换和调度，长操作在 goroutine 中执行，通过 `operationDone` 回投结果：

```go
type operationKind string

const (
	operationStart   operationKind = "start"
	operationStop    operationKind = "stop"
	operationRestart operationKind = "restart"
)

type activeOperation struct {
	id     uint64
	kind   operationKind
	cancel context.CancelFunc
	reply  chan traymodel.OperationResult
}

type operationDone struct {
	id     uint64
	kind   operationKind
	result traymodel.OperationResult
}
```

规则固定如下：

1. actor 收到 Start 时先进入 `starting`，建立可取消上下文，再启动 goroutine；
2. actor 在操作进行中继续处理命令、退出事件和上下文关闭；
3. `starting` 状态收到 Stop 时调用 `active.cancel()`，终止已经创建的本次 PID，最终状态为 `stopped`；
4. 非允许组合立即返回 `operation_conflict`，不排队等待隐含串行化；
5. 完成消息必须校验 operation ID，过期完成消息只清理资源，不覆盖较新状态；
6. Restart 内部仍严格执行 stop 完成并确认退出后再 start，stop 失败直接返回。

- [ ] **Step 4: 添加 `ForceStopTracked` 契约**

将 `Component` 接口改为：

```go
type Component interface {
	Start(context.Context) traymodel.OperationResult
	Stop(context.Context) traymodel.OperationResult
	Restart(context.Context) traymodel.OperationResult
	ForceStopTracked(context.Context) traymodel.OperationResult
	Refresh(context.Context) traymodel.ComponentState
}
```

`ForceStopTracked` 必须：取消活动操作、读取 actor 内当前已跟踪 identity、直接调用 Terminator、等待该 identity 的退出事件，超时返回 `force_exit_timeout`。不得在此路径调用 `Inspector.Inspect` 或 `SameProcess`。

- [ ] **Step 5: 更新生产封装和 Service 测试替身**

`production.ManagedComponent` 仅转发 `ForceStopTracked`；删除业务层对旧 `ForceStopClaimed` 的引用。所有 fake component 增加可记录调用顺序的 `ForceStopTracked`。

- [ ] **Step 6: 运行 Supervisor 与封装测试并确认 GREEN**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/nodetray/supervisor ./internal/nodetray/production ./internal/nodetray/app -count=1 -timeout 60s
```

- [ ] **Step 7: 运行竞态测试**

```powershell
$env:CGO_ENABLED = '1'
$env:CC = 'C:\Users\Administrator\AppData\Local\Temp\winlibs-gcc\mingw64\bin\gcc.exe'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -race ./internal/nodetray/supervisor -count=1 -timeout 120s
```

- [ ] **Step 8: 检查点**

确认状态只有 actor 写入，工作 goroutine 只返回结果；确认正在 Start 时 Stop 能在测试时限内返回。

---

## Task 4: 完成正式配置复读、真实 SHA-256 和重启漂移状态

**Files:**

- Modify: `internal/nodetray/config/store.go`
- Modify: `internal/nodetray/config/store_test.go`
- Modify: `internal/nodetray/supervisor/supervisor.go`
- Modify: `internal/nodetray/supervisor/supervisor_test.go`
- Modify: `internal/nodetray/app/service.go`
- Modify: `internal/nodetray/app/service_test.go`
- Modify: `nodetray/app.go`
- Modify: `nodetray/app_test.go`

**Consumes:** 原子替换后的目标配置、运行实例 claimed SHA-256。

**Produces:** 经正式路径复读验证的摘要、`ConfigApplyResult` 和可靠 `needsRestart`。

- [ ] **Step 1: 先写原子保存最终目标复读测试**

```go
func TestWriteAtomicReReadsFormalTargetAfterReplace(t *testing.T) {
	fixture := newAtomicWriteFixture(t)
	fixture.afterReplace = func(path string) {
		require.NoError(t, os.WriteFile(path, []byte("corrupted"), 0o600))
	}

	_, err := fixture.store.SaveAgent(context.Background(), validAgentForm())

	require.Error(t, err)
	require.ErrorContains(t, err, "formal target verification")
}
```

用包内文件操作 seam 注入“替换后篡改”，而不是依赖不稳定的真实文件竞争。

- [ ] **Step 2: 写服务层保存/保存并重启测试**

覆盖四条路径：

```go
func TestSaveAgentReturnsFormalDigestAndNeedsRestart(t *testing.T)
func TestSaveAndRestartStopsBeforeStarting(t *testing.T)
func TestSaveAndRestartDoesNotStartWhenStopFails(t *testing.T)
func TestSaveVerifyFailureReturnsSavedFalse(t *testing.T)
```

关键断言：stop 失败时 `Saved=true`、`Restarted=false`、`NeedsRestart=true`，且 Start 调用次数为 0。

- [ ] **Step 3: 运行定向测试并确认 RED**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/nodetray/config ./internal/nodetray/app ./nodetray -run 'FormalTarget|SaveAgent|SaveAndRestart|SaveVerify' -count=1
```

- [ ] **Step 4: 在替换后复读正式目标**

`writeAtomic` 完成 rename/replace 后必须重新读取正式路径，并把摘要建立在该次读取内容上：

```go
formalBytes, err := os.ReadFile(target)
if err != nil {
	return "", fmt.Errorf("read formal target after replace: %w", err)
}
if !bytes.Equal(formalBytes, expectedBytes) {
	return "", errors.New("formal target verification mismatch")
}
sum := sha256.Sum256(formalBytes)
return hex.EncodeToString(sum[:]), nil
```

- [ ] **Step 5: 由 Supervisor 统一计算漂移状态**

增加 `UpdateExpectedSHA256(string)` 命令，由 actor 更新保存摘要并重新发布状态。状态计算固定为：

```go
state.RuntimeConfigSHA256 = claimedSHA256
state.SavedConfigSHA256 = spec.ExpectedSHA256
state.NeedsRestart = state.Lifecycle == traymodel.LifecycleRunning &&
	claimedSHA256 != "" && spec.ExpectedSHA256 != "" &&
	!strings.EqualFold(claimedSHA256, spec.ExpectedSHA256)
```

停止状态不显示“需要重启”；下次 Start 使用更新后的 expected SHA-256。

- [ ] **Step 6: 把 Save API 改成 `ConfigApplyResult`**

Backend 和 Service 的 Agent/Helper 保存方法全部返回 `traymodel.ConfigApplyResult`。保存只执行写入、正式复读、更新 expected SHA 和 Refresh；保存并重启额外按 stop/确认退出/start 执行。稳定错误码使用 `save_verify_failed`，不把摘要留给前端自行计算。

- [ ] **Step 7: 运行受影响 Go 测试并确认 GREEN**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/nodetray/config ./internal/nodetray/supervisor ./internal/nodetray/app ./nodetray -count=1 -timeout 90s
```

- [ ] **Step 8: 检查点**

确认任何保存成功结果都携带 64 位正式文件 SHA-256；停止失败后没有第二实例。

---

## Task 5: 实现单一 `ForceExitAll` 协调器，后台全部退出后才授权 UI 退出

**Files:**

- Modify: `internal/nodetray/app/service.go`
- Modify: `internal/nodetray/app/service_test.go`
- Modify: `nodetray/app.go`
- Modify: `nodetray/app_test.go`
- Modify: `nodetray/composition.go`
- Modify: `nodetray/composition_test.go`
- Modify: `nodetray/composition_windows.go`
- Modify: `nodetray/composition_windows_test.go`
- Modify: `nodetray/main.go`
- Modify: `nodetray/composition_bindings.go`

**Consumes:** Helper/Agent `ForceStopTracked`、退出前 Worker PID 快照、`PIDWaiter`。

**Produces:** `Service.ForceExitAll`、`Backend.ForceExitAll`、一次性 `exitAuthorized` 和严格 UI 退出门禁。

- [ ] **Step 1: 先写服务层顺序与失败聚合测试**

```go
func TestForceExitAllForcesEveryBackgroundComponentBeforeSuccess(t *testing.T) {
	calls := []string{}
	service := newForceExitServiceFixture(&calls,
		workers(41, 42),
		forceResult("helper", true),
		forceResult("agent", true),
		waitResults(nil, nil),
	)

	result := service.ForceExitAll(context.Background())

	require.True(t, result.OK)
	require.Equal(t, []string{
		"workers:snapshot", "helper:force", "agent:force", "worker:41:wait", "worker:42:wait",
	}, calls)
}

func TestForceExitAllContinuesAfterHelperFailureAndKeepsFailure(t *testing.T) {
	calls := []string{}
	service := newForceExitServiceFixture(&calls,
		workers(41),
		forceResult("helper", false),
		forceResult("agent", true),
		waitResults(errors.New("still alive")),
	)

	result := service.ForceExitAll(context.Background())

	require.False(t, result.OK)
	require.ElementsMatch(t, []string{"helper", "worker:41"}, result.FailedComponents)
	require.Contains(t, calls, "agent:force")
}
```

另加 Helper/Agent 未运行、Worker 快照失败、15 秒等待超时和重复重试测试。

- [ ] **Step 2: 先写 Backend/Wails 退出门禁测试**

```go
func TestForceExitAllAuthorizesQuitOnlyAfterBackendSuccess(t *testing.T) {
	quitCalls := 0
	backend := newBackendWithForceExitResult(forceExitSuccess(), func(context.Context) { quitCalls++ })

	result := backend.ForceExitAll()

	require.True(t, result.OK)
	require.Equal(t, 1, quitCalls)
	require.False(t, backend.onBeforeClose(context.Background()))
}

func TestForceExitAllFailureKeepsUIOpen(t *testing.T) {
	quitCalls := 0
	backend := newBackendWithForceExitResult(forceExitFailure("agent"), func(context.Context) { quitCalls++ })

	result := backend.ForceExitAll()

	require.False(t, result.OK)
	require.Zero(t, quitCalls)
	require.True(t, backend.onBeforeClose(context.Background()))
}
```

再加“第二实例检测可调用内部 `authorizeAndQuit`，不会被 `OnBeforeClose` 自身否决”的回归测试。
同时覆盖 `closeToTray=true` 只隐藏、`closeToTray=false` 发出统一退出请求，并断言失败摘要不包含路径、命令行、配置内容、密码或控制令牌。

- [ ] **Step 3: 运行定向测试并确认 RED**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/nodetray/app ./nodetray -run 'ForceExitAll|AuthorizesQuit|SecondInstance' -count=1
```

- [ ] **Step 4: 实现服务协调器**

依赖增加：

```go
type ProcessWaiter interface {
	WaitPIDGone(context.Context, int) error
}
```

`ForceExitAll` 逻辑固定为：

1. 先快照 Worker PID，避免 Agent 被结束后丢失可等待列表；
2. 调用 Helper `ForceStopTracked`；
3. 无论 Helper 成败都调用 Agent `ForceStopTracked`；
4. 逐个等待快照 Worker PID 消失；
5. 聚合 `helper`、`agent`、`worker:<pid>`，只有失败列表为空才返回 `OK=true`；
6. Service 不调用 Wails Quit，不负责关闭 UI。

若 Worker 快照失败，将 `workers` 加入失败列表，但仍继续强制结束 Helper 和 Agent；因为无法证明全部 Worker 已退出，该次结果必须失败并保留 UI。

等待总上限使用 `const forceExitTimeout = 15 * time.Second`，超时码为 `force_exit_timeout`，其他失败码为 `force_exit_failed`。

- [ ] **Step 5: 实现 Backend 授权与 Quit 注入**

```go
type Backend struct {
	service        *app.Service
	quit           func(context.Context)
	exitAuthorized atomic.Bool
}

func (b *Backend) ForceExitAll() traymodel.ForceExitResult {
	ctx, service, ok := b.ready()
	if !ok {
		return traymodel.ForceExitResult{ErrorCode: "backend_not_ready", ErrorSummary: "后端尚未就绪"}
	}
	result := service.ForceExitAll(ctx)
	if result.OK {
		b.authorizeAndQuit(ctx)
	}
	return result
}

func (b *Backend) authorizeAndQuit(ctx context.Context) {
	b.exitAuthorized.Store(true)
	b.quit(ctx)
}
```

`onBeforeClose` 先检查 `exitAuthorized`：为 true 返回 false；否则按 `closeToTray` 隐藏或发出统一退出请求并返回 true。生产 composition 明确注入 `inputs.Quit`。

- [ ] **Step 6: 删除混淆退出语义**

删除或停止导出 `ExitTray(bool)`；托盘、设置页、窗口关闭都不得直接调用 `Quit` 或分别 ForceStop 组件。旧的前端绑定在 Task 6 重生成后消失。

- [ ] **Step 7: 运行退出协调器和组合测试并确认 GREEN**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/nodetray/app ./nodetray -count=1 -timeout 90s
```

- [ ] **Step 8: 检查点**

确认 `quit` 的唯一正常退出调用点在 `result.OK` 分支之后；任何失败均保留 UI 以供重试。

---

## Task 6: 统一所有退出入口和确认弹窗

**Files:**

- Modify: `nodetray/main.go`
- Modify: `nodetray/app_test.go`
- Modify: `nodetray/frontend/src/App.tsx`
- Modify: `nodetray/frontend/src/App.test.tsx`
- Modify: `nodetray/frontend/src/components/ExitDialog.tsx`
- Modify: `nodetray/frontend/src/components/ExitDialog.test.tsx`
- Modify: `nodetray/frontend/src/pages/SettingsPage.tsx`
- Modify: `nodetray/frontend/src/pages/SettingsPage.test.tsx`
- Modify: `nodetray/frontend/src/bindings/backend.ts`
- Regenerate: `nodetray/frontend/wailsjs/go/main/Backend.d.ts`
- Regenerate: `nodetray/frontend/wailsjs/go/main/Backend.js`
- Regenerate: `nodetray/frontend/wailsjs/go/models.ts`

**Consumes:** `force-exit-requested`、`window-close-requested`、`Backend.ForceExitAll`。

**Produces:** 一个弹窗、一条后端请求、失败后保留并重试的 UI 流程。

- [ ] **Step 1: 生成新 Wails 绑定**

```powershell
Push-Location 'nodetray'
try {
    & 'C:\tmp\go1.26.5\go\bin\go.exe' run github.com/wailsapp/wails/v2/cmd/wails@v2.12.0 generate module
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
} finally {
    Pop-Location
}
```

确认生成结果包含 `ForceExitAll(): Promise<models.ForceExitResult>`，Save API 返回 `ConfigApplyResult`，且不再暴露 `ExitTray`。

- [ ] **Step 2: 先写前端失败测试**

```tsx
it('confirms once and sends one ForceExitAll request', async () => {
  const forceExitAll = vi.fn().mockResolvedValue({
    ok: true,
    failedComponents: [],
    errorCode: '',
    errorSummary: '',
  })
  render(<ExitDialog open onCancel={vi.fn()} forceExitAll={forceExitAll} />)

  await userEvent.click(screen.getByRole('button', { name: '强制退出全部进程' }))

  expect(forceExitAll).toHaveBeenCalledTimes(1)
})

it('keeps the dialog open and offers retry when a process survives', async () => {
  const forceExitAll = vi.fn().mockResolvedValue({
    ok: false,
    failedComponents: ['worker:42'],
    errorCode: 'force_exit_timeout',
    errorSummary: '后台进程仍在运行',
  })
  render(<ExitDialog open onCancel={vi.fn()} forceExitAll={forceExitAll} />)

  await userEvent.click(screen.getByRole('button', { name: '强制退出全部进程' }))

  expect(screen.getByText(/worker:42/)).toBeInTheDocument()
  expect(screen.getByRole('button', { name: '重试强制退出' })).toBeEnabled()
})
```

App 测试分别发出 `force-exit-requested` 和 `window-close-requested`，断言两者打开同一个 `ExitDialog`。设置页“退出”只发起同一前端请求，不直接调后端退出。
再加入存在未保存 Agent、Helper 或设置草稿时，弹窗明确显示“未保存更改将丢失”的测试。

- [ ] **Step 3: 运行测试并确认 RED**

```powershell
& 'D:\application\nodejs\npm.cmd' test -- --run src/components/ExitDialog.test.tsx src/App.test.tsx src/pages/SettingsPage.test.tsx
```

工作目录：`nodetray/frontend`。

- [ ] **Step 4: 简化 `ExitDialog`**

删除 `ExitTray`、`ForceStopAgent`、`ForceStopHelper` 的分支和二次选择。弹窗只保留取消与确认；存在未保存草稿时显示丢失警告；确认期间按钮禁用；失败时显示 `failedComponents` 和后端摘要，并把主按钮文本改为“重试强制退出”。成功后无需前端主动关闭窗口，后端会在门禁通过后退出 Wails。

- [ ] **Step 5: 统一事件入口**

`App.tsx` 同时订阅两个事件名并调用同一个 `openExitDialog`：

```ts
const exitEvents = ['force-exit-requested', 'window-close-requested'] as const
const unsubs = exitEvents.map((eventName) => EventsOn(eventName, openExitDialog))
return () => unsubs.forEach((unsubscribe) => unsubscribe())
```

原生托盘 Exit 命令只调用 Show/聚焦 UI 并发出 `force-exit-requested`；不得调用 Backend 生命周期 API。设置页退出按钮也走同一个应用内回调。

- [ ] **Step 6: 运行前端测试和 Go 入口测试并确认 GREEN**

```powershell
& 'D:\application\nodejs\npm.cmd' test -- --run src/components/ExitDialog.test.tsx src/App.test.tsx src/pages/SettingsPage.test.tsx
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./nodetray -run 'Tray|Close|Exit' -count=1
```

- [ ] **Step 7: 检查点**

全仓搜索 `ExitTray(`、`ForceStopAgent(`、`ForceStopHelper(`，确认前端退出路径没有残留分步调用。

---

## Task 7: 程序设置按差异应用，普通保存不再弹 UAC

**Files:**

- Modify: `internal/nodetray/app/service.go`
- Modify: `internal/nodetray/app/service_test.go`
- Modify: `nodetray/frontend/src/pages/SettingsPage.tsx`
- Modify: `nodetray/frontend/src/pages/SettingsPage.test.tsx`

**Consumes:** 保存前实际设置、提交表单、登录项状态、Helper 计划任务状态。

**Produces:** 最小副作用设置更新；失败后返回真实部分状态并让 UI 重载。

- [ ] **Step 1: 先写设置差异测试矩阵**

```go
func TestSaveTraySettingsOrdinaryChangeSkipsLoginAndElevation(t *testing.T) {
	fixture := newSettingsFixture(t)
	form := fixture.current
	form.RefreshIntervalSeconds++

	result := fixture.service.SaveTraySettings(context.Background(), form)

	require.True(t, result.OK)
	require.Zero(t, fixture.login.writeCalls)
	require.Zero(t, fixture.task.installCalls+fixture.task.removeCalls)
}

func TestSaveTraySettingsHelperPolicyChangeRunsElevationBeforeDiskCommit(t *testing.T) {
	fixture := newSettingsFixture(t)
	form := fixture.current
	form.HelperPolicy = "always"

	result := fixture.service.SaveTraySettings(context.Background(), form)

	require.True(t, result.OK)
	require.Equal(t, []string{"task:install", "settings:save", "settings:reload", "login:read", "task:inspect"}, fixture.calls)
}

func TestSaveTraySettingsUACCancelDoesNotPersistRequestedPolicy(t *testing.T)
func TestSaveTraySettingsLateFailureReturnsPartiallyAppliedAndActualState(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/nodetray/app -run SaveTraySettings -count=1
```

- [ ] **Step 3: 实现明确的差异应用顺序**

保存流程固定为：

1. 读取当前磁盘设置、当前登录项和当前 Helper 任务状态；
2. 比较普通设置、`startAtLogin`、Helper 策略三个差异组；
3. Helper 策略变化时先执行需要 UAC 的 install/remove；UAC 取消立即返回，磁盘不得写入请求的新策略；
4. 登录项变化时只执行一次 enable/disable；未变化时不触碰 HKCU；
5. 保存磁盘设置；
6. 重新读取磁盘、登录项和任务状态，并据此返回结果；
7. 后期失败且已有副作用时返回 `settings_partially_applied`，不得伪装成全量回滚成功。

此处只写显式分支，不引入事务框架。

- [ ] **Step 4: 前端失败后重载真实状态**

设置页收到 `settings_partially_applied` 或其他失败结果后，先展示错误，再调用已有 load API 重读表单；重读失败时保留错误提示，不能继续展示未经确认的提交值。

- [ ] **Step 5: 运行后端和前端设置测试并确认 GREEN**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/nodetray/app -run SaveTraySettings -count=1
Push-Location 'nodetray\frontend'
try {
    & 'D:\application\nodejs\npm.cmd' test -- --run src/pages/SettingsPage.test.tsx
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
} finally {
    Pop-Location
}
```

- [ ] **Step 6: 检查点**

普通显示、刷新间隔或关闭到托盘设置变化时，测试替身的提权调用数必须为 0。

---

## Task 8: 前端显示配置漂移并按全局生命周期禁用冲突操作

**Files:**

- Modify: `nodetray/frontend/src/state/nodeStore.ts`
- Modify: `nodetray/frontend/src/state/nodeStore.test.ts`
- Create: `nodetray/frontend/src/state/NodeStateContext.tsx`
- Create: `nodetray/frontend/src/state/NodeStateContext.test.tsx`
- Modify: `nodetray/frontend/src/App.tsx`
- Modify: `nodetray/frontend/src/App.test.tsx`
- Modify: `nodetray/frontend/src/pages/OverviewPage.tsx`
- Modify: `nodetray/frontend/src/pages/OverviewPage.test.tsx`
- Modify: `nodetray/frontend/src/pages/AgentPage.tsx`
- Modify: `nodetray/frontend/src/pages/AgentPage.test.tsx`
- Modify: `nodetray/frontend/src/pages/HelperPage.tsx`
- Modify: `nodetray/frontend/src/pages/HelperPage.test.tsx`

**Consumes:** Wails `ComponentState` 与 `ConfigApplyResult`。

**Produces:** 应用级单一生命周期快照、可靠按钮状态和“已保存，需重启”反馈。

- [ ] **Step 1: 先写 store 解析测试**

```ts
it('keeps runtime and saved digests plus restart drift', () => {
  const state = parseComponentState({
    lifecycle: 'running',
    runtimeConfigSha256: 'b'.repeat(64),
    savedConfigSha256: 'a'.repeat(64),
    needsRestart: true,
  })

  expect(state.runtimeConfigSha256).toBe('b'.repeat(64))
  expect(state.savedConfigSha256).toBe('a'.repeat(64))
  expect(state.needsRestart).toBe(true)
})
```

- [ ] **Step 2: 写页面交互失败测试**

```tsx
it('shows saved-but-restart-required without claiming a restart', async () => {
  const saveAgent = vi.fn().mockResolvedValue({
    ok: true,
    saved: true,
    restarted: false,
    sha256: 'a'.repeat(64),
    needsRestart: true,
    errorCode: '',
    errorSummary: '',
  })
  renderAgentPage({ lifecycle: 'running', saveAgent })

  await userEvent.click(screen.getByRole('button', { name: '保存' }))

  expect(screen.getByText('配置已保存，需要重启后生效')).toBeInTheDocument()
})

it('allows stop as cancel while starting and disables conflicting actions', () => {
  renderAgentPage({ lifecycle: 'starting' })

  expect(screen.getByRole('button', { name: '取消启动' })).toBeEnabled()
  expect(screen.getByRole('button', { name: '启动' })).toBeDisabled()
  expect(screen.getByRole('button', { name: '重启' })).toBeDisabled()
})
```

HelperPage 使用相同断言；Overview 显示保存摘要、运行摘要和需要重启标记。页面摘要只显示 SHA-256 的前 12 位，完整摘要保留在可复制的 title 或详情文本中。

- [ ] **Step 3: 运行定向测试并确认 RED**

```powershell
& 'D:\application\nodejs\npm.cmd' test -- --run src/state/nodeStore.test.ts src/state/NodeStateContext.test.tsx src/pages/AgentPage.test.tsx src/pages/HelperPage.test.tsx src/pages/OverviewPage.test.tsx
```

工作目录：`nodetray/frontend`。

- [ ] **Step 4: 扩展 store 与共享 Context**

`nodeStore.ts` 把三个新增字段加入精确字段白名单。`NodeStateContext` 在 App 根部只创建一个 store，使用 `useSyncExternalStore` 暴露快照与 refresh；Overview、Agent、Helper 共用该快照，不各自维护互相漂移的 lifecycle 副本。

- [ ] **Step 5: 统一按钮状态表**

页面按照以下状态启用操作：

| 生命周期 | Start | Stop/取消启动 | Restart | Save and restart |
|---|---:|---:|---:|---:|
| stopped / failed | 是 | 否 | 否 | 否 |
| starting | 否 | 是，文案“取消启动” | 否 | 否 |
| running | 否 | 是 | 是 | 是 |
| stopping / restarting | 否 | 否 | 否 | 否 |

本地请求 pending 继续作为第二层禁用条件。收到 `operation_conflict` 时刷新全局状态并显示后端摘要。

- [ ] **Step 6: 使用后端结果而不是前端重算摘要**

Agent/Helper 的保存提示只读取 `ConfigApplyResult`。`Saved=true && Restarted=false && NeedsRestart=true` 显示“配置已保存，需要重启后生效”；stop 失败的保存并重启不得显示“重启成功”。

- [ ] **Step 7: 运行所有受影响前端测试并确认 GREEN**

```powershell
& 'D:\application\nodejs\npm.cmd' test -- --run src/state src/pages src/App.test.tsx
```

工作目录：`nodetray/frontend`。

- [ ] **Step 8: 检查点**

Agent、Helper、Overview 对同一组件的 lifecycle、摘要和 needsRestart 必须来自同一快照。

---

## Task 9: 原生托盘消息循环只异步派发生命周期命令

**Files:**

- Modify: `internal/nodetray/windows/tray/tray_windows.go`
- Modify: `internal/nodetray/windows/tray/tray_stub.go`
- Modify: `internal/nodetray/windows/tray/lifecycle.go`
- Modify: `internal/nodetray/windows/tray/lifecycle_test.go`
- Modify: `internal/nodetray/windows/tray/menu.go`
- Modify: `internal/nodetray/windows/tray/menu_test.go`
- Modify: `nodetray/main.go`
- Modify: `nodetray/app_test.go`

**Consumes:** 原生菜单命令与已有 `Options.Handle(Command)`。

**Produces:** 不被 Start/Stop/Restart 阻塞的 Win32 window proc。

- [ ] **Step 1: 先写消息循环非阻塞测试**

```go
func TestLifecycleCommandDispatchDoesNotBlockWindowProc(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	options := Options{Handle: func(Command) {
		close(started)
		<-release
	}}

	dispatchLifecycleCommand(options, CommandStartAgent)

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler was not dispatched")
	}
	close(release)
}
```

另加 handler panic 不得结束托盘循环、连续冲突命令由后端返回 `operation_conflict` 的测试。菜单模型测试使用 Task 8 的同一状态表，断言 `starting` 时只允许 Stop/取消启动，`stopping/restarting` 时不允许任何冲突命令。

- [ ] **Step 2: 运行测试并确认 RED 或阻塞**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/nodetray/windows/tray -run LifecycleCommandDispatch -count=1 -timeout 10s
```

- [ ] **Step 3: 增加单一异步派发函数**

```go
func dispatchLifecycleCommand(options Options, command Command) {
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil && options.ReportError != nil {
				options.ReportError(fmt.Errorf("tray command %s panicked: %v", command, recovered))
			}
		}()
		options.Handle(command)
	}()
}
```

Win32 window proc 只调用该函数，立即返回消息处理；Start/Stop/Restart 的状态变化仍由 Supervisor actor 串行决策。Exit 命令仅发 UI 事件，不进入此生命周期 handler。

`menu.go` 根据最新 `ComponentState` 更新启用状态和 `starting` 状态下的“取消启动”文案；菜单禁用只改善交互，最终合法性仍由 Supervisor 判定。

- [ ] **Step 4: 运行托盘与入口测试并确认 GREEN**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/nodetray/windows/tray ./nodetray -run 'Lifecycle|Tray' -count=1 -timeout 30s
```

- [ ] **Step 5: 检查点**

检查 window proc 路径不存在对 Component Start、Stop、Restart、ForceExitAll 的同步等待。

---

## Task 10: 全链路静态验收、构建与文档收口

**Files:**

- Modify: `docs/operations/nodetray-deployment.md`
- Create: `docs/operations/nodetray-lifecycle-repair-acceptance-2026-08-03.md`
- Verify only: `scripts/build-nodetray.ps1`
- Verify only: `scripts/Test-NodeTray.ps1`
- Verify only: all files changed in Tasks 1–9

**Consumes:** 全部实现和测试结果。

**Produces:** 可复验静态证据、构建产物和明确未执行的真实机器验收项。

- [ ] **Step 1: 格式化并重新生成最终绑定**

```powershell
& 'C:\tmp\go1.26.5\go\bin\gofmt.exe' -w @(
  'internal\nodetray\traymodel\model.go',
  'internal\nodetray\process\terminator_windows.go',
  'internal\nodetray\process\terminator_stub.go',
  'internal\nodetray\process\waiter_windows.go',
  'internal\nodetray\process\waiter_stub.go',
  'internal\nodetray\supervisor\supervisor.go',
  'internal\nodetray\supervisor\component.go',
  'internal\nodetray\production\managed.go',
  'internal\nodetray\config\store.go',
  'internal\nodetray\app\service.go',
  'internal\nodetray\windows\tray\lifecycle.go',
  'nodetray\app.go',
  'nodetray\composition.go',
  'nodetray\composition_windows.go',
  'nodetray\main.go'
)
Push-Location 'nodetray'
try {
    & 'C:\tmp\go1.26.5\go\bin\go.exe' run github.com/wailsapp/wails/v2/cmd/wails@v2.12.0 generate module
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
} finally {
    Pop-Location
}
```

把本次新增测试文件也加入实际 `gofmt` 参数；命令不得格式化第三方或生成的 JavaScript。

- [ ] **Step 2: 运行 NodeTray Go 全量测试**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/nodetray/... ./nodetray -count=1 -timeout 180s
```

预期：全部 PASS。不要在这一阶段运行会连接已安装 Agent 固定命名管道的真实 `internal/nodectl` 集成测试。

- [ ] **Step 3: 运行 Supervisor 竞态测试**

```powershell
$env:CGO_ENABLED = '1'
$env:CC = 'C:\Users\Administrator\AppData\Local\Temp\winlibs-gcc\mingw64\bin\gcc.exe'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -race ./internal/nodetray/supervisor ./internal/nodetray/app -count=1 -timeout 180s
```

预期：PASS 且无 data race。

- [ ] **Step 4: 运行前端全量测试、Lint 和生产构建**

```powershell
Push-Location 'nodetray\frontend'
try {
    & 'D:\application\nodejs\npm.cmd' test
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    & 'D:\application\nodejs\npm.cmd' run lint
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    & 'D:\application\nodejs\npm.cmd' run build
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
} finally {
    Pop-Location
}
```

预期：测试、ESLint、TypeScript 与 Vite 构建全部通过。

- [ ] **Step 5: 运行供应链静态检查并构建独立验收产物**

```powershell
& '.\scripts\Test-NodeTraySupplyChain.ps1'
& '.\scripts\build-nodetray.ps1' `
  -GoExe 'C:\tmp\go1.26.5\go\bin\go.exe' `
  -NpmCmd 'D:\application\nodejs\npm.cmd' `
  -OutDir 'artifacts\nodetray-lifecycle-repair'
```

只允许写仓库内 `artifacts\nodetray-lifecycle-repair`，不得启动构建出的 EXE，不得修改系统设置。

- [ ] **Step 6: 执行静态契约搜索**

```powershell
rg -n 'ExitTray\(|ForceStopClaimed\(|NewTrustedTerminator\(' internal/nodetray nodetray
rg -n 'ForceExitAll|force-exit-requested|exitAuthorized|needsRestart|settings_partially_applied|start_cancelled|operation_conflict' internal/nodetray nodetray
```

第一条只允许命中明确保留的历史兼容测试或类型定义；任何生产退出路径命中都必须处理。第二条必须覆盖模型、服务、Backend、绑定、前端页面和测试。

- [ ] **Step 7: 写中文验收报告**

`nodetray-lifecycle-repair-acceptance-2026-08-03.md` 必须逐项记录：

- 静态阻塞缺陷与对应修复任务；
- 每条实际执行命令、退出码、关键通过数量与产物路径；
- 退出顺序和失败时 UI 保留的自动化证据；
- 启动取消、重启不产生第二实例的自动化证据；
- 正式配置复读和 needsRestart 的自动化证据；
- 普通设置无 UAC 的替身测试证据；
- 真实进程、UAC、计划任务、HKCU、窗口和托盘人工验收标记为 `BLOCKED_NOT_RUN_DYNAMIC`，原因写明“未获得本轮真实机器操作授权”。

- [ ] **Step 8: 复核计划契约和占位符**

```powershell
$placeholderPattern = @(('TO' + 'DO'), ('T' + 'BD'), ('待' + '定'), ('稍后' + '补充'), ('适当' + '处理'), ('类似' + '处理')) -join '|'
rg -n $placeholderPattern `
  'docs\superpowers\specs\2026-08-03-node-tray-lifecycle-repair-design.md' `
  'docs\superpowers\plans\2026-08-03-node-tray-lifecycle-repair.md' `
  'docs\operations\nodetray-lifecycle-repair-acceptance-2026-08-03.md'
```

预期：无命中。

- [ ] **Step 9: 最终状态检查**

```powershell
Get-ChildItem -LiteralPath 'artifacts\nodetray-lifecycle-repair' -File -Recurse |
  Select-Object FullName, Length, LastWriteTime
git status --short
```

若 `.git` 仍不可用，验收报告如实记录“当前工作目录无可用 Git 元数据”，以文件清单和测试输出作为交付证据。

## Completion Gate

只有同时满足下列条件，才可声明“静态实现完成”：

1. Tasks 1–9 的 RED/GREEN 测试证据齐全；
2. NodeTray Go 全量测试、Supervisor/app race、前端 test/lint/build、供应链检查和独立构建全部通过；
3. 所有显式退出入口都汇入同一个确认弹窗和一次 `ForceExitAll`；
4. Backend 仅在 Helper、Agent 和退出前快照 Worker 全部确认消失后授权 Wails Quit；
5. 保存返回正式目标 SHA-256，保存并重启在 stop 失败时不启动第二实例；
6. 普通设置保存的自动化测试证明没有登录项或 UAC 副作用；
7. 动态真实机器项没有被误报为 PASS，而是明确保持 `BLOCKED_NOT_RUN_DYNAMIC`。
