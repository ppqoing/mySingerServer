# NodeTray 禁用 Helper 状态误报修复 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让未启用且没有真实进程的 Helper 在 NodeTray 中稳定显示为“未启用”，同时保留已启用错误、残留进程和计划任务漂移的真实状态。

**Architecture:** Go 服务在 `GetOverview` 的最终输出边界归一化“Helper 禁用且 PID 为 0”的组件状态，前端 Store 用同一条件过滤后续原始事件并清除旧 Helper 警报。React 不新增后端生命周期，只通过派生的 `helperDisabled` 展示中性状态；设置保存成功后刷新共享 Store，使策略值先于后续事件生效。

**Tech Stack:** Go 1.26.5、Wails v2.12.0、React 19.2、TypeScript 5.9、Vitest 4.1、PowerShell 7。

## Global Constraints

- 设计依据：`docs/superpowers/specs/2026-08-03-nodetray-disabled-helper-state-design.md`。
- Helper 未启用且原始 PID 为 0时，状态必须为现有 `stopped` 生命周期，不新增 Go 或 TypeScript 生命周期值。
- 中性禁用状态必须清空运行态、错误和注意字段；只保留合法的 64 位小写十六进制 `SavedConfigSHA256`。
- Helper 已启用时不得归一化错误；Helper 已禁用但 PID 大于 0时不得掩盖真实进程状态。
- `HelperTaskDrift` 独立计算并继续显示；禁用展示不能清除任务漂移事实。
- 不自动创建 Helper 配置，不自动启动或停止组件，不修改强制退出、控制协议、配置格式或 Wails 导出结构。
- 设置保存成功后必须重新获取共享 Overview，再处理后续实时事件。
- 当前源码目录没有 `.git` 元数据，标识为 `N/A_NO_GIT_METADATA`；不得初始化仓库。每个任务用测试结果代替提交检查点。
- 静态实现和独立构建不得覆盖 `C:\Program Files\MySingerServer`，不得启动真实 NodeTray、Agent、Worker 或 Helper，不得触发 UAC、计划任务或 HKCU 修改。
- 未实际执行的真实 Windows 验收必须记录为 `BLOCKED_NOT_RUN_DYNAMIC`，不得写成 PASS。

## 文件职责映射

- `internal/nodetray/app/service.go`：在 Overview 输出边界归一化禁用且无进程的 Helper 状态，并验证可保留的保存配置摘要。
- `internal/nodetray/app/service_test.go`：覆盖禁用无 PID、启用错误、禁用有 PID、摘要合法性、任务漂移和 Agent 不受影响。
- `nodetray/frontend/src/state/nodeStore.ts`：维持禁用 Helper 的实时事件不变量，清理重新获取 Overview 后的旧 Helper attention。
- `nodetray/frontend/src/state/nodeStore.test.ts`：覆盖原始 unavailable 事件、残留进程事件、attention 过滤和刷新清理。
- `nodetray/frontend/src/pages/SettingsPage.tsx`：设置保存成功后等待共享 Node Store 刷新。
- `nodetray/frontend/src/pages/SettingsPage.test.tsx`：证明成功提示和 pending 结束发生在共享刷新之后。
- `nodetray/frontend/src/components/StatusBadge.tsx`：提供不改变后端 lifecycle 的前端专用“未启用”展示。
- `nodetray/frontend/src/components/StatusBadge.test.tsx`：固定 disabled 标签、图标和 `data-lifecycle`。
- `nodetray/frontend/src/pages/OverviewPage.tsx`：派生 `helperDisabled`，控制最近异常和三个操作按钮。
- `nodetray/frontend/src/pages/OverviewPage.test.tsx`：覆盖禁用无 PID 和禁用有 PID 两条 UI 分支。
- `docs/acceptance/node-tray-disabled-helper-state-fix-2026-08-03.md`：记录 RED→GREEN、回归门禁、产物事实和动态验收边界。

---

### Task 1: 在 Go Overview 边界归一化禁用 Helper

**Files:**

- Modify: `internal/nodetray/app/service.go:143-203, 680-694`
- Modify: `internal/nodetray/app/service_test.go:247-277`

**Interfaces:**

- Produces: `normalizeDisabledHelperState(enabled bool, value traymodel.ComponentState) traymodel.ComponentState`。
- Produces: `isLowerSHA256(value string) bool`。
- Preserves: `Service.GetOverview(context.Context) (traymodel.Overview, error)` 的导出签名与 JSON 模型。
- Consumes: `TraySettings.HelperEnabled` 与已清理的 Helper `ComponentState`。

- [ ] **Step 1: 编写禁用无 PID 的失败测试**

在 `service_test.go` 的 Overview 测试区添加：

```go
func TestOverviewNormalizesDisabledUnavailableHelperAndKeepsTaskDrift(t *testing.T) {
	s, _, store, agent, helper, _ := serviceFixture(t)
	store.settings.HelperEnabled = false
	store.settings.HelperStartMode = traymodel.StartManual
	agent.state = traymodel.ComponentState{
		Lifecycle: traymodel.Failed, ErrorCode: "agent_failed",
		ErrorSummary: "Agent still requires attention", NeedsAttention: true,
	}
	helper.state = traymodel.ComponentState{
		Lifecycle: traymodel.Failed, Healthy: true, Ready: true, PID: 0,
		StartedAtUnixMS: 99, UptimeSeconds: 88, WorkerReady: 1,
		WorkerExpected: 2, ActiveRequests: 3, ErrorCode: "unavailable",
		ErrorSummary: "Helper configuration unavailable", NeedsAttention: true,
		RuntimeConfigSHA256: strings.Repeat("b", 64),
		SavedConfigSHA256: strings.Repeat("a", 64), NeedsRestart: true,
	}
	s.task = &fakeTask{calls: &[]string{}, status: nodetask.Status{Installed: true}}

	overview, err := s.GetOverview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantHelper := traymodel.ComponentState{
		Lifecycle: traymodel.Stopped,
		SavedConfigSHA256: strings.Repeat("a", 64),
	}
	if !reflect.DeepEqual(overview.Helper, wantHelper) {
		t.Fatalf("disabled Helper = %#v, want %#v", overview.Helper, wantHelper)
	}
	if !overview.HelperTaskDrift {
		t.Fatal("installed Helper task drift was hidden by disabled normalization")
	}
	if overview.Agent.Lifecycle != traymodel.Failed || overview.Agent.ErrorCode != "agent_failed" {
		t.Fatalf("Agent state was changed by Helper normalization: %#v", overview.Agent)
	}
}
```

- [ ] **Step 2: 编写启用错误、禁用残留进程和摘要格式测试**

继续添加：

```go
func TestOverviewKeepsEnabledUnavailableHelper(t *testing.T) {
	s, _, _, _, helper, _ := serviceFixture(t)
	helper.state = attentionState("unavailable", "Helper configuration unavailable")

	overview, err := s.GetOverview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if overview.Helper.Lifecycle != traymodel.Failed ||
		overview.Helper.ErrorCode != "unavailable" || !overview.Helper.NeedsAttention {
		t.Fatalf("enabled Helper error was hidden: %#v", overview.Helper)
	}
}

func TestOverviewKeepsDisabledHelperWhenRealPIDExists(t *testing.T) {
	s, _, store, _, helper, _ := serviceFixture(t)
	store.settings.HelperEnabled = false
	helper.state = traymodel.ComponentState{
		Lifecycle: traymodel.Running, Healthy: true, Ready: true,
		PID: 4321, StartedAtUnixMS: 123, UptimeSeconds: 10,
		SavedConfigSHA256: strings.Repeat("a", 64),
	}

	overview, err := s.GetOverview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(overview.Helper, helper.state) {
		t.Fatalf("live disabled Helper was hidden: %#v", overview.Helper)
	}
}

func TestNormalizeDisabledHelperStateRejectsInvalidSavedDigest(t *testing.T) {
	got := normalizeDisabledHelperState(false, traymodel.ComponentState{
		Lifecycle: traymodel.Failed,
		SavedConfigSHA256: strings.Repeat("A", 64),
	})
	if got.SavedConfigSHA256 != "" || got.Lifecycle != traymodel.Stopped {
		t.Fatalf("invalid digest survived normalization: %#v", got)
	}
}
```

- [ ] **Step 3: 运行定向测试确认 RED**

```powershell
$env:GOCACHE='D:\code\mySingerServer\.tmp\nodetray-disabled-helper-go-cache'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test .\internal\nodetray\app `
  -run 'Test(Overview(NormalizesDisabledUnavailableHelperAndKeepsTaskDrift|KeepsEnabledUnavailableHelper|KeepsDisabledHelperWhenRealPIDExists)|NormalizeDisabledHelperStateRejectsInvalidSavedDigest)$' `
  -count=1
```

预期：测试因 `normalizeDisabledHelperState` 尚未定义或 disabled Helper 仍为 `failed/unavailable` 而失败。

- [ ] **Step 4: 实现最小归一化函数**

在 `service.go` 的输出清理函数附近添加：

```go
func normalizeDisabledHelperState(enabled bool, value traymodel.ComponentState) traymodel.ComponentState {
	if enabled || value.PID > 0 {
		return value
	}
	normalized := traymodel.ComponentState{Lifecycle: traymodel.Stopped}
	if isLowerSHA256(value.SavedConfigSHA256) {
		normalized.SavedConfigSHA256 = value.SavedConfigSHA256
	}
	return normalized
}

func isLowerSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
```

在 `GetOverview` 完成计划任务检查之后、返回之前调用：

```go
	overview.Helper = normalizeDisabledHelperState(settings.HelperEnabled, overview.Helper)
```

该调用必须位于 `HelperTaskDrift` 计算之后：归一化清除组件 `NeedsAttention`，但不改写独立的漂移字段。

- [ ] **Step 5: 格式化并运行测试确认 GREEN**

```powershell
& 'C:\tmp\go1.26.5\go\bin\gofmt.exe' -w `
  '.\internal\nodetray\app\service.go' `
  '.\internal\nodetray\app\service_test.go'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test .\internal\nodetray\app -count=1
```

预期：`internal/nodetray/app` 全部通过；禁用无 PID 的状态只剩 `stopped` 和合法保存摘要，漂移仍为 true。

- [ ] **Step 6: 记录无 Git 检查点**

```powershell
if (Test-Path -LiteralPath '.git') { throw 'unexpected Git metadata appeared' }
'Task 1 PASS - N/A_NO_GIT_METADATA'
```

预期：输出 Task 1 PASS，不创建提交。

---

### Task 2: 在 Node Store 阻止禁用 Helper 事件重新污染状态

**Files:**

- Modify: `nodetray/frontend/src/state/nodeStore.ts:148-198`
- Modify: `nodetray/frontend/src/state/nodeStore.test.ts:105-171`

**Interfaces:**

- Produces: `isDisabledHelperWithoutProcess(overview: NodeOverview): boolean`，仅供 Store 内部使用。
- Preserves: `NodeStore`、`NodeOverview`、Wails 事件名和事件负载结构。
- Consumes: Task 1 产生的 `helperEnabled=false + helper.pid=0` Overview 不变量。

- [ ] **Step 1: 编写禁用事件守卫失败测试**

在 `nodeStore.test.ts` 添加：

```ts
it('忽略禁用无 PID Helper 的 unavailable 和 attention，但接受真实 PID', async () => {
  const value = initialOverview()
  value.helperEnabled = false
  value.helper = component('stopped', 0, 0)
  const harness = createHarness(value)
  const store = createNodeStore(harness.dependencies)
  const started = store.start()
  harness.resolveOverview(value)
  await started

  const disabledState = store.getSnapshot().overview?.helper
  harness.handlers.get('component-state')!({
    component: 'helper',
    state: {
      ...component('failed', 0, 0),
      errorCode: 'unavailable',
      errorSummary: '组件不可用',
      needsAttention: true,
    },
  })
  harness.handlers.get('attention-required')!({
    component: 'helper', code: 'unavailable', summary: '组件不可用',
  })
  expect(store.getSnapshot().overview?.helper).toEqual(disabledState)
  expect(store.getSnapshot().attention).toBeNull()

  harness.handlers.get('component-state')!({
    component: 'helper', state: component('running', 3301, 300),
  })
  expect(store.getSnapshot().overview?.helper.pid).toBe(3301)
  harness.handlers.get('attention-required')!({
    component: 'helper', code: 'live_helper_warning', summary: '残留 Helper 仍在运行',
  })
  expect(store.getSnapshot().attention?.code).toBe('live_helper_warning')
})
```

- [ ] **Step 2: 编写重新获取 Overview 清理旧 attention 的失败测试**

继续添加：

```ts
it('刷新为禁用无 PID Overview 时清除旧 Helper attention', async () => {
  const enabled = initialOverview()
  const disabled = initialOverview()
  disabled.helperEnabled = false
  disabled.helper = component('stopped', 0, 0)
  const handlers = new Map<string, EventHandler>()
  const getOverview = vi.fn()
    .mockResolvedValueOnce(enabled)
    .mockResolvedValueOnce(disabled)
  const store = createNodeStore({
    getOverview,
    onEvent: (name, handler) => {
      handlers.set(name, handler)
      return () => undefined
    },
  })

  await store.start()
  handlers.get('attention-required')!({
    component: 'helper', code: 'unavailable', summary: '组件不可用',
  })
  expect(store.getSnapshot().attention?.component).toBe('helper')

  await store.start()
  expect(getOverview).toHaveBeenCalledTimes(2)
  expect(store.getSnapshot().overview?.helperEnabled).toBe(false)
  expect(store.getSnapshot().attention).toBeNull()
})
```

- [ ] **Step 3: 运行 Store 测试确认 RED**

```powershell
Push-Location '.\nodetray\frontend'
try {
  & 'D:\application\nodejs\npm.cmd' test -- src/state/nodeStore.test.ts
  if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
} finally {
  Pop-Location
}
```

预期：禁用 Helper 被 unavailable 事件覆盖，或旧 attention 在第二次 `start()` 后仍存在，测试失败。

- [ ] **Step 4: 实现 Store 内部策略谓词和两个事件守卫**

在 `nodeStore.ts` 添加内部谓词：

```ts
function isDisabledHelperWithoutProcess(overview: NodeOverview): boolean {
  return !overview.helperEnabled && overview.helper.pid <= 0
}
```

在 `handleComponentState` 解析成功并取得 Overview 后、watermark 处理前添加：

```ts
    if (
      parsed.component === 'helper' &&
      isDisabledHelperWithoutProcess(overview) &&
      parsed.state.pid <= 0
    ) {
      return
    }
```

在 `handleAttentionRequired` 中添加：

```ts
    const overview = snapshot.overview
    if (
      parsed.component === 'helper' &&
      overview &&
      isDisabledHelperWithoutProcess(overview)
    ) {
      return
    }
```

- [ ] **Step 5: 在 Overview 刷新发布时清除旧 Helper attention**

把成功加载 Overview 的发布改为：

```ts
        const attention = (
          isDisabledHelperWithoutProcess(overview) &&
          snapshot.attention?.component === 'helper'
        ) ? null : snapshot.attention
        publish({ ...snapshot, overview, attention, loading: false })
```

不得清除 `agent` 或 `tray` attention；禁用但 PID 大于 0时也不得清除 Helper attention。

- [ ] **Step 6: 运行 Store 测试确认 GREEN**

```powershell
Push-Location '.\nodetray\frontend'
try {
  & 'D:\application\nodejs\npm.cmd' test -- src/state/nodeStore.test.ts
  if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
} finally {
  Pop-Location
}
```

预期：Store 测试全部通过；PID 0 的误报被忽略，PID 3301 的真实状态和后续警报仍被接受。

- [ ] **Step 7: 记录无 Git 检查点**

```powershell
if (Test-Path -LiteralPath '.git') { throw 'unexpected Git metadata appeared' }
'Task 2 PASS - N/A_NO_GIT_METADATA'
```

---

### Task 3: 设置保存成功后刷新共享 Overview

**Files:**

- Modify: `nodetray/frontend/src/pages/SettingsPage.tsx:1-91`
- Modify: `nodetray/frontend/src/pages/SettingsPage.test.tsx:1-92`

**Interfaces:**

- Consumes: `useOptionalNodeState(): NodeStateValue | null` 的既有 `refresh(): Promise<void>`。
- Produces: 设置保存顺序 `SaveTraySettings OK → form.commit → await nodeState.refresh → success status`。
- Preserves: `SettingsPageDependencies` 和 Wails 绑定签名。

- [ ] **Step 1: 编写共享刷新顺序失败测试**

在 `SettingsPage.test.tsx` 增加 `NodeStateProvider`、`NodeSnapshot` 和 `NodeStore` 导入，并添加：

```tsx
it('保存成功后等待共享 Overview 刷新再结束 pending', async () => {
  let finishRefresh!: () => void
  const refreshPending = new Promise<void>((resolve) => { finishRefresh = resolve })
  const start = vi.fn()
    .mockResolvedValueOnce(undefined)
    .mockImplementationOnce(() => refreshPending)
  const snapshot: NodeSnapshot = {
    overview: null, operation: null, attention: null,
    loading: false, errorSummary: '',
  }
  const store: NodeStore = {
    start,
    dispose: vi.fn(),
    subscribe: () => () => undefined,
    getSnapshot: () => snapshot,
  }

  render(
    <NodeStateProvider store={store}>
      <SettingsPage
        dependencies={dependencies()}
        onDirtyChange={() => undefined}
        onRequestExit={() => undefined}
      />
    </NodeStateProvider>,
  )
  await screen.findByLabelText('登录后启动托盘程序')
  await waitFor(() => expect(start).toHaveBeenCalledOnce())

  await userEvent.setup().click(screen.getByRole('button', { name: '保存程序设置' }))
  await waitFor(() => expect(start).toHaveBeenCalledTimes(2))
  expect(screen.getByRole('button', { name: '保存程序设置' })).toBeDisabled()
  expect(screen.queryByText('程序设置已保存。')).not.toBeInTheDocument()

  finishRefresh()
  expect(await screen.findByText('程序设置已保存。')).toBeVisible()
  expect(screen.getByRole('button', { name: '保存程序设置' })).toBeEnabled()
})
```

- [ ] **Step 2: 运行 SettingsPage 测试确认 RED**

```powershell
Push-Location '.\nodetray\frontend'
try {
  & 'D:\application\nodejs\npm.cmd' test -- src/pages/SettingsPage.test.tsx
  if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
} finally {
  Pop-Location
}
```

预期：`start` 只有 Provider 初始化的一次调用，成功提示在共享刷新之前出现。

- [ ] **Step 3: 保存成功路径等待共享刷新**

在 `SettingsPage.tsx` 导入并使用可选上下文：

```tsx
import { useOptionalNodeState } from '../state/NodeStateContext'

export function SettingsPage({
  dependencies = productionDependencies,
  onDirtyChange,
  onRequestExit,
}: SettingsPageProps): ReactNode {
  const nodeState = useOptionalNodeState()
  const form = useDirtyForm(emptySettings)
  const [loaded, setLoaded] = useState(false)
  const [pending, setPending] = useState(false)
  const [attention, setAttention] = useState('')
  const [status, setStatus] = useState('')
```

把保存成功分支固定为：

```ts
      form.commit(normalized)
      await nodeState?.refresh()
      setStatus('程序设置已保存。')
```

保存失败路径仍只重读实际设置，不把失败结果发布为成功 Overview。

- [ ] **Step 4: 运行 SettingsPage 测试确认 GREEN**

```powershell
Push-Location '.\nodetray\frontend'
try {
  & 'D:\application\nodejs\npm.cmd' test -- src/pages/SettingsPage.test.tsx
  if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
} finally {
  Pop-Location
}
```

预期：现有设置测试与新增顺序测试全部通过。

- [ ] **Step 5: 记录无 Git 检查点**

```powershell
if (Test-Path -LiteralPath '.git') { throw 'unexpected Git metadata appeared' }
'Task 3 PASS - N/A_NO_GIT_METADATA'
```

---

### Task 4: 在总览中显示“未启用”并保留残留进程操作

**Files:**

- Modify: `nodetray/frontend/src/components/StatusBadge.tsx:3-48`
- Modify: `nodetray/frontend/src/components/StatusBadge.test.tsx:5-24`
- Modify: `nodetray/frontend/src/pages/OverviewPage.tsx:96-160`
- Modify: `nodetray/frontend/src/pages/OverviewPage.test.tsx:64-193`

**Interfaces:**

- Produces: `StatusBadge({ lifecycle, disabled?: boolean })`；`disabled` 只控制前端展示。
- Produces: `helperDisabled = !overview.helperEnabled && overview.helper.pid <= 0`。
- Preserves: 禁用但 PID 大于 0时的真实 lifecycle 和停止按钮能力。

- [ ] **Step 1: 编写 StatusBadge 禁用展示失败测试**

在 `StatusBadge.test.tsx` 添加：

```tsx
it('disabled 覆盖错误生命周期并使用中性未启用展示', () => {
  const { container } = render(<StatusBadge lifecycle="failed" disabled />)

  expect(screen.getByText('未启用')).toBeVisible()
  expect(container.querySelector('.status-badge')).toHaveAttribute('data-lifecycle', 'disabled')
  expect(container.querySelector('svg')).toHaveAttribute('data-icon', 'pause')
  expect(screen.queryByText('异常')).not.toBeInTheDocument()
})
```

- [ ] **Step 2: 编写禁用无 PID 与残留 PID 的总览失败测试**

在 `OverviewPage.test.tsx` 添加：

```tsx
it('Helper 禁用且无 PID 时显示未启用、清空最近异常并禁用全部操作', async () => {
  const value = overview()
  value.helperEnabled = false
  value.helper = {
    ...component('failed', 0),
    errorCode: 'unavailable', errorSummary: '组件不可用', needsAttention: true,
  }
  value.helperTaskDrift = true
  render(<OverviewPage store={testStore(value)} actions={successfulActions()} />)

  const helper = await screen.findByRole('article', { name: '删除 Helper' })
  expect(helper).toHaveTextContent('未启用')
  expect(within(helper).queryByText('异常')).not.toBeInTheDocument()
  expect(within(helper).getByText('最近异常').parentElement).toHaveTextContent('—')
  expect(helper).not.toHaveTextContent('组件不可用')
  expect(helper).toHaveTextContent('计划任务配置已漂移')
  for (const button of within(helper).getAllByRole('button')) {
    expect(button).toBeDisabled()
  }
})

it('Helper 禁用但有真实 PID 时显示运行态并允许停止', async () => {
  const value = overview()
  value.helperEnabled = false
  value.helper = component('running', 3301)
  render(<OverviewPage store={testStore(value)} actions={successfulActions()} />)

  const helper = await screen.findByRole('article', { name: '删除 Helper' })
  expect(helper).toHaveTextContent('运行中')
  expect(helper).toHaveTextContent('3301')
  expect(within(helper).getByRole('button', { name: '启动 Helper' })).toBeDisabled()
  expect(within(helper).getByRole('button', { name: '停止 Helper' })).toBeEnabled()
  expect(within(helper).getByRole('button', { name: '重启 Helper' })).toBeDisabled()
})
```

- [ ] **Step 3: 运行两个组件测试确认 RED**

```powershell
Push-Location '.\nodetray\frontend'
try {
  & 'D:\application\nodejs\npm.cmd' test -- `
    src/components/StatusBadge.test.tsx `
    src/pages/OverviewPage.test.tsx
  if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
} finally {
  Pop-Location
}
```

预期：disabled 属性尚不存在，总览仍显示“异常”或“组件不可用”，测试失败。

- [ ] **Step 4: 实现 StatusBadge 的前端专用禁用态**

在 `StatusBadge.tsx` 添加常量并扩展参数：

```tsx
const disabledDefinition: StatusDefinition = { label: '未启用', icon: 'pause' }

export function StatusBadge({
  lifecycle,
  disabled = false,
}: {
  lifecycle: string
  disabled?: boolean
}): ReactNode {
  const definition = disabled ? disabledDefinition : definitions[lifecycle] ?? fallback
  const safeLifecycle = disabled ? 'disabled' : lifecycle in definitions ? lifecycle : 'unknown'

  return (
    <span className="status-badge" data-lifecycle={safeLifecycle}>
      <StatusIcon kind={definition.icon} />
      <span>{definition.label}</span>
    </span>
  )
}
```

`app.css` 的默认 `.status-badge` 已使用中性色；不要为 disabled 增加新的警告或错误颜色。

- [ ] **Step 5: 在 OverviewPage 使用同一禁用谓词**

在计算 lifecycle actions 后添加：

```tsx
  const helperDisabled = !overview.helperEnabled && overview.helper.pid <= 0
```

修改 Helper 卡片的相关表达式：

```tsx
status={<StatusBadge lifecycle={overview.helper.lifecycle} disabled={helperDisabled} />}

<div><dt>最近异常</dt><dd>{helperDisabled ? '—' : overview.helper.errorSummary || '—'}</dd></div>

<button className="button-primary" type="button"
  disabled={!overview.helperEnabled || !helperActions.start}
  onClick={() => void runAction('helper', actions.startHelper)}>启动 Helper</button>
<button className="button-secondary" type="button"
  disabled={helperDisabled || !helperActions.stop}
  onClick={() => void runAction('helper', actions.stopHelper)}>
  {helperActions.cancelStart ? '取消启动' : '停止 Helper'}
</button>
<button className="button-secondary" type="button"
  disabled={!overview.helperEnabled || !helperActions.restart}
  onClick={() => void runAction('helper', actions.restartHelper)}>重启 Helper</button>
```

启动和重启继续受 `helperEnabled` 控制；停止只在“禁用且无 PID”时额外禁用，因此禁用但仍运行的 Helper 可以被手动停止。

- [ ] **Step 6: 运行组件测试确认 GREEN**

```powershell
Push-Location '.\nodetray\frontend'
try {
  & 'D:\application\nodejs\npm.cmd' test -- `
    src/components/StatusBadge.test.tsx `
    src/pages/OverviewPage.test.tsx
  if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
} finally {
  Pop-Location
}
```

预期：禁用无 PID 显示“未启用”且按钮全禁用；PID 3301 显示“运行中”且仅停止可用。

- [ ] **Step 7: 记录无 Git 检查点**

```powershell
if (Test-Path -LiteralPath '.git') { throw 'unexpected Git metadata appeared' }
'Task 4 PASS - N/A_NO_GIT_METADATA'
```

---

### Task 5: 执行回归门禁、构建独立产物并记录验收

**Files:**

- Create: `docs/acceptance/node-tray-disabled-helper-state-fix-2026-08-03.md`
- Create: `artifacts/nodetray-disabled-helper-state-fix/nodetray.exe`
- Create: `artifacts/nodetray-disabled-helper-state-fix/MicrosoftEdgeWebview2Setup.exe`

**Interfaces:**

- Consumes: Tasks 1–4 的 Go 与前端行为。
- Produces: 自动化门禁记录、独立 x64 `asInvoker` NodeTray 产物、实际 SHA-256 和签名状态。
- Preserves: `C:\Program Files\MySingerServer` 和当前真实进程状态。

- [ ] **Step 1: 运行 NodeTray Go 全量与竞态门禁**

```powershell
$env:GOCACHE='D:\code\mySingerServer\.tmp\nodetray-disabled-helper-go-cache'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test `
  .\internal\nodetray\... .\nodetray -count=1 -timeout 180s
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -race `
  .\internal\nodetray\app .\internal\nodetray\production `
  -count=1 -timeout 180s
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
```

预期：两个命令退出码均为 0，竞态输出不含 `DATA RACE`。不使用 `go test ./...`，因为本修复不包含环境工具链受限的 `cmd/helper`。

- [ ] **Step 2: 运行前端完整门禁**

```powershell
Push-Location '.\nodetray\frontend'
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

预期：Vitest、ESLint、TypeScript 和 Vite 均退出码 0。已有 Fast Refresh warning 可以如实记录，但不得出现 error。

- [ ] **Step 3: 静态确认没有扩展 lifecycle 或 Wails 模型**

```powershell
$goDisabled = rg -n 'Disabled\s+Lifecycle|Lifecycle\s*=\s*"disabled"' internal\nodetray nodetray 2>$null
if ($LASTEXITCODE -eq 0) { $goDisabled; throw 'backend disabled lifecycle was added' }
if ($LASTEXITCODE -ne 1) { throw 'lifecycle scan failed' }
rg -n 'type Lifecycle|HelperEnabled|helperEnabled' `
  internal\nodetray\traymodel\model.go `
  nodetray\frontend\src\state\nodeStore.ts
```

预期：后端没有 `disabled` 生命周期；既有 HelperEnabled 字段仍是策略权威。

- [ ] **Step 4: 构建新的独立 NodeTray 产物**

```powershell
$cache='D:\code\mySingerServer\.tmp\nodetray-disabled-helper-build-cache'
$temp='D:\code\mySingerServer\.tmp\nodetray-disabled-helper-build-temp'
New-Item -ItemType Directory -Path $cache,$temp -Force | Out-Null
$env:GOCACHE=$cache
$env:GOTMPDIR=$temp
& '.\scripts\build-nodetray.ps1' `
  -Go 'C:\tmp\go1.26.5\go\bin\go.exe' `
  -Npm 'D:\application\nodejs\npm.cmd' `
  -OutDir 'artifacts\nodetray-disabled-helper-state-fix'
```

预期：最终 JSON 状态为 `PASS`，输出包含 x64、`asInvoker` 的 `nodetray.exe` 和已校验的 WebView2 Bootstrapper；构建过程不启动产物。

- [ ] **Step 5: 记录产物大小、SHA-256 和签名事实**

```powershell
$stage='D:\code\mySingerServer\artifacts\nodetray-disabled-helper-state-fix'
Get-ChildItem -LiteralPath $stage -File | ForEach-Object {
  [pscustomobject]@{
    Name=$_.Name
    Length=$_.Length
    SHA256=(Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash
    Signature=(Get-AuthenticodeSignature -LiteralPath $_.FullName).Status
  }
} | Format-Table -AutoSize
```

预期：输出每个实际文件的值。不得沿用上一产物哈希，也不得预设新 `nodetray.exe` 已签名。

- [ ] **Step 6: 写入专项验收记录**

创建 `docs/acceptance/node-tray-disabled-helper-state-fix-2026-08-03.md`，使用以下固定结构并填入 Steps 1–5 的实际输出：

```markdown
# NodeTray 禁用 Helper 状态误报修复验收记录

## 自动化行为

| 场景 | 结果 | 证据 |
|---|---|---|
| Helper 禁用、PID 0、配置不可用 | PASS 或 FAIL | Go 测试名称与退出码 |
| Helper 启用、配置不可用 | PASS 或 FAIL | Go 测试名称与退出码 |
| Helper 禁用但有真实 PID | PASS 或 FAIL | Go 与 React 测试名称 |
| Helper 任务漂移保持可见 | PASS 或 FAIL | Go 与 React 测试名称 |
| 禁用 Helper 原始事件与旧 attention | PASS 或 FAIL | nodeStore 测试名称 |
| 设置保存后刷新共享 Overview | PASS 或 FAIL | SettingsPage 测试名称 |

## 回归门禁

记录 Go 全量、Go race、Vitest、ESLint、TypeScript/Vite 和独立构建的实际命令、退出码与计数。

## 发布产物

记录产物绝对目录、文件大小、SHA-256、签名状态、Windows amd64 和 asInvoker 事实。

## 动态验收边界

没有替换或启动真实安装目录程序时，以下项目记录为 BLOCKED_NOT_RUN_DYNAMIC：

- 实机 Helper 卡片显示“未启用”；
- 页面不再出现“组件不可用”；
- 最近异常显示“—”，三个 Helper 操作按钮禁用；
- Agent 与 Worker 实机状态不受影响。
```

表格每一项只能填写本轮实际观察到的 PASS、FAIL 或 `BLOCKED_NOT_RUN_DYNAMIC`。

- [ ] **Step 7: 执行文档自检**

```powershell
$docs=@(
  'docs\superpowers\specs\2026-08-03-nodetray-disabled-helper-state-design.md',
  'docs\superpowers\plans\2026-08-03-nodetray-disabled-helper-state-fix.md',
  'docs\acceptance\node-tray-disabled-helper-state-fix-2026-08-03.md'
)
$docs | ForEach-Object {
  if (-not (Test-Path -LiteralPath $_)) { throw "missing document: $_" }
}
$pattern=@(('TO'+'DO'),('T'+'BD'),('待'+'定'),('稍后'+'补充'),('适当'+'处理'),('类似'+'处理')) -join '|'
$hits=rg -n $pattern @docs 2>$null
if($LASTEXITCODE -eq 0){ $hits; throw 'document placeholders found' }
if($LASTEXITCODE -ne 1){ throw 'placeholder scan failed' }
```

预期：三个文档均存在且没有占位符。

- [ ] **Step 8: 保持真实 Windows 验收为独立授权步骤**

未获得明确的实机替换和启动授权时，不复制文件、不结束进程、不启动 GUI，验收记录保持 `BLOCKED_NOT_RUN_DYNAMIC`。

获得授权后的执行顺序固定为：

1. 通过当前 NodeTray 的统一强制退出关闭旧后台进程和 UI；
2. 以管理员 PowerShell 备份并替换 `C:\Program Files\MySingerServer\nodetray.exe`；
3. 启动新 NodeTray，保持 `helperEnabled=false`；
4. 确认 Helper 卡片为“未启用”、最近异常为 `—`、三个按钮禁用且无页面警报；
5. 确认 Agent/Worker 状态不变；
6. 保存进程和界面证据，失败时保留旧 EXE 以便回退。

- [ ] **Step 9: 最终无 Git 检查点**

```powershell
if (Test-Path -LiteralPath '.git') { throw 'unexpected Git metadata appeared' }
'Implementation gates recorded - N/A_NO_GIT_METADATA'
```

预期：验收记录包含所有实际结果，源码目录仍无 Git 元数据。未来在正式 checkout 中收口时建议提交信息：

```text
fix: normalize disabled helper state in nodetray
```
