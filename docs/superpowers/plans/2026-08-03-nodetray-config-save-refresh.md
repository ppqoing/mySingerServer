# NodeTray 配置保存与全界面刷新实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复“启用 Helper/手动模式”无法保存的问题，并让程序设置、Agent 配置和 Helper 配置实际保存后立即刷新所有相关界面状态。

**Architecture:** 后端在 Helper 策略发生变化时先读取固定计划任务的实际状态，只在实际状态与目标状态不一致时调用既有提权安装/删除流程。前端继续以 `NodeStateContext` 为唯一共享状态源，三个配置保存入口在确认配置已经落盘后显式等待 `refresh()`，不新增配置事件或全局表单缓存。

**Tech Stack:** Go 1.26、Wails v2.12.0、React 19、TypeScript 5.9、Vitest 4、PowerShell 构建脚本。

## Global Constraints

- Helper 手动启用且固定计划任务不存在时，不弹 UAC，不执行删除任务。
- 只有 `HelperEnabled` 或 `HelperStartMode` 变化时才检查任务状态；普通设置保存不得依赖任务服务。
- Agent/Helper 以 `ConfigApplyResult.saved` 判断是否刷新，即使 `ok == false` 但已落盘也必须刷新。
- 共享状态刷新失败不得回滚已保存配置。
- 不新增 `config-changed` 事件、全局配置缓存、安全校验或生命周期重构。
- 不修改 Agent、Helper 配置格式和 Wails 导出接口。
- 源码目录没有 `.git` 元数据；所有提交步骤记录为 `N/A_NO_GIT_METADATA`，不得初始化 Git。
- 真实安装目录替换和 GUI 实机验收不属于本计划，未执行时记录 `BLOCKED_NOT_RUN_DYNAMIC`。

---

## 文件结构

- 修改 `internal/nodetray/app/service.go`：按任务实际状态决定是否执行 Helper 任务策略。
- 修改 `internal/nodetray/app/service_test.go`：覆盖手动启用、任务状态一致及任务读取失败。
- 创建 `nodetray/frontend/src/test/createTestNodeStore.ts`：供配置页面测试复用最小 `NodeStore`。
- 修改 `nodetray/frontend/src/pages/SettingsPage.test.tsx`：覆盖从禁用切换为手动启用后保持勾选并刷新。
- 修改 `nodetray/frontend/src/pages/AgentPage.tsx`：Agent 配置落盘后等待共享刷新。
- 修改 `nodetray/frontend/src/pages/AgentPage.test.tsx`：覆盖普通保存、保存并重启、部分成功和未保存。
- 修改 `nodetray/frontend/src/pages/HelperPage.tsx`：Helper 配置落盘后等待共享刷新。
- 修改 `nodetray/frontend/src/pages/HelperPage.test.tsx`：覆盖成功、部分成功和未保存刷新规则。
- 生成 `artifacts/nodetray-config-save-refresh-fix/`：独立 Windows x64 构建产物。

### Task 1: 按 Helper 计划任务实际状态保存程序设置

**Files:**
- Modify: `internal/nodetray/app/service.go:476-544`
- Test: `internal/nodetray/app/service_test.go:934-1029`

**Interfaces:**
- Consumes: `TaskController.Inspect(context.Context) (nodetask.Status, error)`、`nodetask.Status.Installed`、现有 `applyHelperTaskPolicy`。
- Produces: `reconcileHelperTaskPolicy(context.Context, traymodel.TraySettings) traymodel.OperationResult`，供 `SaveTraySettings` 在 Helper 策略变化时调用。

- [ ] **Step 1: 写入手动启用 Helper 的失败回归测试**

在 `service_test.go` 增加：

```go
func TestSaveTraySettingsManualHelperEnableSkipsTaskRemovalWhenTaskAbsent(t *testing.T) {
	s, calls, store, _, _, elevated := serviceFixture(t)
	store.settings.HelperEnabled = false
	store.settings.HelperStartMode = traymodel.StartManual
	task := s.task.(*fakeTask)
	task.status = nodetask.Status{Installed: false}
	value := store.settings
	value.HelperEnabled = true

	result := s.SaveTraySettings(context.Background(), value)

	if !result.OK || !store.settings.HelperEnabled {
		t.Fatalf("result=%#v persisted=%#v", result, store.settings)
	}
	if !reflect.DeepEqual(*calls, []string{"save-settings"}) {
		t.Fatalf("calls=%v", *calls)
	}
	if task.inspectCalls != 2 || len(elevated.actions) != 0 {
		t.Fatalf("inspect=%d elevation=%v", task.inspectCalls, elevated.actions)
	}
}
```

- [ ] **Step 2: 运行测试并确认 RED**

Run:

```powershell
go test -count=1 ./internal/nodetray/app -run '^TestSaveTraySettingsManualHelperEnableSkipsTaskRemovalWhenTaskAbsent$'
```

Expected: FAIL；调用序列包含 `elevate-remove_helper_task`，证明当前实现错误地提权删除不存在的任务。

- [ ] **Step 3: 增加任务已经满足目标及任务读取失败测试**

增加两个测试：

```go
func TestSaveTraySettingsHelperTaskAlreadyMatchesTargetSkipsElevation(t *testing.T) {
	s, calls, store, _, _, elevated := serviceFixture(t)
	task := s.task.(*fakeTask)
	task.status = nodetask.Status{Installed: true}
	value := store.settings
	value.HelperStartMode = traymodel.StartAutomatic

	result := s.SaveTraySettings(context.Background(), value)

	if !result.OK || store.settings.HelperStartMode != traymodel.StartAutomatic {
		t.Fatalf("result=%#v persisted=%#v", result, store.settings)
	}
	if !reflect.DeepEqual(*calls, []string{"save-settings"}) || len(elevated.actions) != 0 {
		t.Fatalf("calls=%v elevation=%v", *calls, elevated.actions)
	}
}

func TestSaveTraySettingsHelperTaskInspectFailureDoesNotPersistPolicy(t *testing.T) {
	s, calls, store, _, _, elevated := serviceFixture(t)
	store.settings.HelperEnabled = false
	task := s.task.(*fakeTask)
	task.err = errors.New("scheduler unavailable")
	value := store.settings
	value.HelperEnabled = true

	result := s.SaveTraySettings(context.Background(), value)

	if result.OK || result.ErrorCode != "task_failed" || store.settings.HelperEnabled {
		t.Fatalf("result=%#v persisted=%#v", result, store.settings)
	}
	if len(*calls) != 0 || len(elevated.actions) != 0 {
		t.Fatalf("calls=%v elevation=%v", *calls, elevated.actions)
	}
}
```

- [ ] **Step 4: 运行新增测试并确认 RED**

Run:

```powershell
go test -count=1 ./internal/nodetray/app -run '^TestSaveTraySettings(ManualHelperEnable|HelperTaskAlreadyMatches|HelperTaskInspectFailure)'
```

Expected: FAIL；前两个测试观测到不必要的 elevation，读取失败测试仍继续执行旧策略。

- [ ] **Step 5: 实现最小任务协调逻辑**

在 `service.go` 增加：

```go
func (s *Service) reconcileHelperTaskPolicy(ctx context.Context, value traymodel.TraySettings) traymodel.OperationResult {
	if s.task == nil {
		return operationFailure("task_failed", "计划任务状态服务不可用")
	}
	status, err := s.task.Inspect(ctx)
	if err != nil {
		return operationFailure("task_failed", "计划任务状态读取失败")
	}
	desiredInstalled := value.HelperEnabled && value.HelperStartMode == traymodel.StartAutomatic
	if status.Installed == desiredInstalled {
		return traymodel.OperationResult{OK: true}
	}
	return s.applyHelperTaskPolicy(ctx, value)
}
```

把 `SaveTraySettings` 中 Helper 策略变化分支改为：

```go
if helperPolicyChanged {
	if result := s.reconcileHelperTaskPolicy(ctx, value); !result.OK {
		return result
	}
}
```

保留 `applyHelperTaskPolicy` 的现有安装、删除和 UAC 取消语义。

- [ ] **Step 6: 运行 Task 1 测试并确认 GREEN**

Run:

```powershell
go test -count=1 ./internal/nodetray/app -run '^TestSaveTraySettings'
```

Expected: PASS；现有安装任务、UAC 取消和部分应用测试同时通过。

- [ ] **Step 7: 记录提交状态**

记录 `N/A_NO_GIT_METADATA`；不执行 `git add`、`git commit`，不初始化仓库。

### Task 2: 程序设置与 Agent 配置保存后刷新共享状态

**Files:**
- Modify: `nodetray/frontend/src/pages/AgentPage.tsx:167-194`
- Create: `nodetray/frontend/src/test/createTestNodeStore.ts`
- Test: `nodetray/frontend/src/pages/SettingsPage.test.tsx:33-132`
- Test: `nodetray/frontend/src/pages/AgentPage.test.tsx:1-230`

**Interfaces:**
- Consumes: `useOptionalNodeState()` 返回的 `refresh(): Promise<void>`、`ConfigApplyResult.saved`。
- Produces: `createTestNodeStore(start: NodeStore['start']): NodeStore`；Agent 普通保存和保存并重启在配置落盘后等待共享刷新；程序设置启用 Helper 的 UI 回归证据。

- [ ] **Step 1: 为程序设置启用 Helper 增加 UI 回归测试**

先创建测试专用 Store：

```ts
// nodetray/frontend/src/test/createTestNodeStore.ts
import type { NodeSnapshot, NodeStore } from '../state/nodeStore'

export function createTestNodeStore(start: NodeStore['start']): NodeStore {
  const snapshot: NodeSnapshot = {
    overview: null, operation: null, attention: null,
    loading: false, errorSummary: '',
  }
  return {
    start,
    dispose: () => undefined,
    subscribe: () => () => undefined,
    getSnapshot: () => snapshot,
  }
}
```

在 `SettingsPage.test.tsx` 导入 `createTestNodeStore` 并新增：

```ts
it('启用手动 Helper 后保存、刷新且保持勾选', async () => {
  const saveTraySettings = vi.fn(async (value: traymodel.TraySettings) => {
    void value
    return ok
  })
  const start = vi.fn(async () => undefined)
  render(
    <NodeStateProvider store={createTestNodeStore(start)}>
      <SettingsPage
        dependencies={dependencies({
          getTraySettings: vi.fn(async () => settings({
            helperEnabled: false,
            helperStartMode: 'manual',
          })),
          saveTraySettings,
        })}
        onDirtyChange={() => undefined}
        onRequestExit={() => undefined}
      />
    </NodeStateProvider>,
  )
  const enabled = await screen.findByLabelText('启用 Helper')
  await waitFor(() => expect(start).toHaveBeenCalledOnce())

  await userEvent.setup().click(enabled)
  await userEvent.setup().click(screen.getByRole('button', { name: '保存程序设置' }))

  await waitFor(() => expect(start).toHaveBeenCalledTimes(2))
  expect(saveTraySettings).toHaveBeenCalledOnce()
  expect(saveTraySettings.mock.calls[0][0]).toMatchObject({
    helperEnabled: true,
    helperStartMode: 'manual',
  })
  expect(enabled).toBeChecked()
  expect(await screen.findByText('程序设置已保存。')).toBeVisible()
})
```

- [ ] **Step 2: 运行程序设置测试并确认现有前端行为**

Run:

```powershell
npm --prefix nodetray/frontend test -- src/pages/SettingsPage.test.tsx
```

Expected: PASS；该测试锁定前端不会自行取消勾选，后端 Task 1 负责真实持久化。

- [ ] **Step 3: 为 Agent 的 `saved` 刷新规则写失败测试**

在 `AgentPage.test.tsx` 导入：

```ts
import { NodeStateProvider } from '../state/NodeStateContext'
import { createTestNodeStore } from '../test/createTestNodeStore'
```

增加以下测试（共享 Store 的第一次 `start` 来自 Provider 挂载，第二次来自保存刷新）：

```ts
it('Agent 已落盘但后续失败时仍刷新并显示错误', async () => {
  const start = vi.fn(async () => undefined)
  const deps = dependencies({
    saveAgent: vi.fn(async () => ({
      ...configOK,
      ok: false,
      saved: true,
      errorCode: 'fingerprint_update_failed',
    })),
  })
  render(
    <NodeStateProvider store={createTestNodeStore(start)}>
      <AgentPage dependencies={deps} componentState={{ lifecycle: 'stopped' }} />
    </NodeStateProvider>,
  )
  await screen.findByDisplayValue('node-a')
  await waitFor(() => expect(start).toHaveBeenCalledOnce())

  await userEvent.setup().click(screen.getByRole('button', { name: '保存配置' }))

  await waitFor(() => expect(start).toHaveBeenCalledTimes(2))
  expect(screen.getByRole('alert')).toBeVisible()
  expect(screen.getByText('配置未修改')).toBeVisible()
})

it('Agent 未落盘时不刷新共享状态', async () => {
  const start = vi.fn(async () => undefined)
  const deps = dependencies({
    saveAgent: vi.fn(async () => ({
      ...configOK,
      ok: false,
      saved: false,
      errorCode: 'save_failed',
    })),
  })
  render(
    <NodeStateProvider store={createTestNodeStore(start)}>
      <AgentPage dependencies={deps} componentState={{ lifecycle: 'stopped' }} />
    </NodeStateProvider>,
  )
  await screen.findByDisplayValue('node-a')
  await waitFor(() => expect(start).toHaveBeenCalledOnce())

  await userEvent.setup().click(screen.getByRole('button', { name: '保存配置' }))

  await screen.findByRole('alert')
  expect(start).toHaveBeenCalledOnce()
})
```

把现有“保存并重启仅在 dirty 时确认”测试的第二次执行放入
`<NodeStateProvider store={createTestNodeStore(start)}>`，确认成功后增加：

```ts
await waitFor(() => expect(start).toHaveBeenCalledTimes(2))
```

- [ ] **Step 4: 运行 Agent 定向测试并确认 RED**

Run:

```powershell
npm --prefix nodetray/frontend test -- src/pages/AgentPage.test.tsx
```

Expected: FAIL；`saved=true` 的保存路径没有调用共享 Store `start`。

- [ ] **Step 5: 在 Agent 保存分支等待共享刷新**

把 `AgentPage.tsx` 的 `result.saved` 分支改成：

```ts
if (result.saved) {
  const committed = afterSuccessfulSave(form.value)
  form.commit(committed)
  setErrors({})
  setValidationCodes({})
  setStatus(configApplyStatus(result))
  await nodeState?.refresh()
}
```

保持 `if (!result.ok)` 在刷新之后执行，确保部分成功既刷新实际状态又显示失败。

- [ ] **Step 6: 运行 Settings 和 Agent 测试并确认 GREEN**

Run:

```powershell
npm --prefix nodetray/frontend test -- src/pages/SettingsPage.test.tsx src/pages/AgentPage.test.tsx
```

Expected: PASS；成功、部分成功、未保存和保存并重启分支均符合刷新合同。

- [ ] **Step 7: 记录提交状态**

记录 `N/A_NO_GIT_METADATA`；不执行 Git 操作。

## 最终评审修复轮 1（2026-08-03）

- Helper 页面必须对启用状态、启动方式、任务漂移和生命周期统一消费共享 NodeState 快照，本地总览仅作首次加载回退。
- 补齐真实共享快照发布、Helper 任务删除方向、保存并重启失败和刷新期间 pending 的合同测试。

### Task 3: Helper 配置保存后刷新共享状态

**Files:**
- Modify: `nodetray/frontend/src/pages/HelperPage.tsx:134-159`
- Test: `nodetray/frontend/src/pages/HelperPage.test.tsx:1-190`

**Interfaces:**
- Consumes: `useOptionalNodeState()` 返回的 `refresh(): Promise<void>`、`ConfigApplyResult.saved`、Task 2 的 `createTestNodeStore(start: NodeStore['start']): NodeStore`。
- Produces: Helper 配置落盘后等待共享 Overview 刷新。

- [ ] **Step 1: 为 Helper 的 `saved` 刷新规则写失败测试**

在 `HelperPage.test.tsx` 导入 `NodeStateProvider` 和 Task 2 创建的
`createTestNodeStore`。

新增测试覆盖：

```ts
const start = vi.fn(async () => undefined)
const savedButFailed = { ...configOK, ok: false, saved: true, errorCode: 'fingerprint_update_failed' }
render(
  <NodeStateProvider store={createTestNodeStore(start)}>
    <HelperPage dependencies={dependencies({ saveHelper: async () => savedButFailed })} />
  </NodeStateProvider>,
)
await screen.findByRole('heading', { name: '删除 Helper 配置' })
await waitFor(() => expect(start).toHaveBeenCalledOnce())
await userEvent.setup().click(screen.getByRole('button', { name: '保存 Helper 配置' }))
await waitFor(() => expect(start).toHaveBeenCalledTimes(2))
expect(screen.getByRole('alert')).toBeVisible()
```

再增加未落盘分支：

```ts
it('Helper 未落盘时不刷新共享状态', async () => {
  const start = vi.fn(async () => undefined)
  const failed = {
    ...configOK,
    ok: false,
    saved: false,
    errorCode: 'save_failed',
  }
  render(
    <NodeStateProvider store={createTestNodeStore(start)}>
      <HelperPage dependencies={dependencies({ saveHelper: async () => failed })} />
    </NodeStateProvider>,
  )
  await screen.findByRole('heading', { name: '删除 Helper 配置' })
  await waitFor(() => expect(start).toHaveBeenCalledOnce())

  await userEvent.setup().click(screen.getByRole('button', { name: '保存 Helper 配置' }))

  await screen.findByRole('alert')
  expect(start).toHaveBeenCalledOnce()
})
```

- [ ] **Step 2: 运行 Helper 测试并确认 RED**

Run:

```powershell
npm --prefix nodetray/frontend test -- src/pages/HelperPage.test.tsx
```

Expected: FAIL；当前 Helper 保存分支只提交表单，不刷新共享 Store。

- [ ] **Step 3: 在 Helper 保存分支等待共享刷新**

把 `HelperPage.tsx` 的 `result.saved` 分支改成：

```ts
if (result.saved) {
  form.commit(new config.HelperForm(form.value))
  setStatus(configApplyStatus(result))
  await nodeState?.refresh()
}
```

保持 `!result.ok` 判断在刷新之后；刷新期间 `pending` 保持为 `true`。

- [ ] **Step 4: 运行 Helper 定向测试并确认 GREEN**

Run:

```powershell
npm --prefix nodetray/frontend test -- src/pages/HelperPage.test.tsx
```

Expected: PASS。

- [ ] **Step 5: 记录提交状态**

记录 `N/A_NO_GIT_METADATA`；不执行 Git 操作。

### Task 4: 完整验证与独立构建产物

**Files:**
- Modify: `nodetray/app_test.go:377-395`（仅当完整门禁确认旧调用次数与新策略冲突时更新期望）
- Verify: `internal/nodetray/app/service.go`
- Verify: `nodetray/frontend/src/pages/SettingsPage.tsx`
- Verify: `nodetray/frontend/src/pages/AgentPage.tsx`
- Verify: `nodetray/frontend/src/pages/HelperPage.tsx`
- Create: `artifacts/nodetray-config-save-refresh-fix/`

**Interfaces:**
- Consumes: Task 1 至 Task 3 的实现和测试。
- Produces: Go、前端和 Windows x64 构建证据，以及独立 `nodetray.exe`。

- [ ] **Step 1: 格式化修改的 Go 文件**

Run:

```powershell
& 'C:\tmp\go1.26.5\go\bin\gofmt.exe' -w internal/nodetray/app/service.go internal/nodetray/app/service_test.go
```

Expected: 退出码 0，仅格式化本计划修改的 Go 文件。

- [ ] **Step 2: 运行后端定向回归**

Run:

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./internal/nodetray/app -run '^TestSaveTraySettings'
```

Expected: PASS。

- [ ] **Step 3: 运行 NodeTray Go 完整相关测试**

Run:

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./internal/nodetray/... ./nodetray
```

Expected: PASS；如果环境门禁阻塞，记录具体命令、错误和 `BLOCKED`，不得冒充通过。

- [ ] **Step 4: 运行前端完整测试、Lint 和构建**

Run:

```powershell
npm --prefix nodetray/frontend test
npm --prefix nodetray/frontend run lint
npm --prefix nodetray/frontend run build
```

Expected: 三条命令均退出码 0，Vitest 无失败，ESLint 无错误，TypeScript/Vite 构建成功。

- [ ] **Step 5: 构建独立 Windows x64 NodeTray 产物**

Run:

```powershell
& .\scripts\build-nodetray.ps1 -Go 'C:\tmp\go1.26.5\go\bin\go.exe' -OutDir 'artifacts\nodetray-config-save-refresh-fix'
```

Expected: 退出码 0，并生成：

```text
artifacts\nodetray-config-save-refresh-fix\nodetray.exe
artifacts\nodetray-config-save-refresh-fix\MicrosoftEdgeWebview2Setup.exe
```

- [ ] **Step 6: 核对产物并记录动态验收边界**

Run:

```powershell
Get-FileHash -Algorithm SHA256 -LiteralPath 'artifacts\nodetray-config-save-refresh-fix\nodetray.exe'
Get-Item -LiteralPath 'artifacts\nodetray-config-save-refresh-fix\nodetray.exe' | Select-Object FullName,Length,LastWriteTime
```

Expected: 输出非空 SHA-256、绝对路径、大小和时间。真实安装替换与 GUI 验收未执行时记录
`BLOCKED_NOT_RUN_DYNAMIC`。

- [ ] **Step 7: 记录提交状态**

记录 `N/A_NO_GIT_METADATA`；不执行 Git 操作。
