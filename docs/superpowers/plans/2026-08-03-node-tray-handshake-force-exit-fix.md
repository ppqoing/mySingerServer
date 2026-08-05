# NodeTray 握手与强制退出误判修复 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复 Agent/Helper 启动时间来源不一致导致的握手失败，并消除 Worker 快照失败和未初始化 Helper 导致的强制退出误报。

**Architecture:** Windows Inspector 返回的 `process.Identity` 继续作为 NodeTray 内部唯一权威进程身份；控制状态只用组件、PID、最终路径和配置 SHA-256 证明它对应同一进程。强制退出把“没有已初始化组件”和“无法读取 Worker 快照”与“已知 PID 仍存活”分开，只对已记录的后台 PID失败保留 UI。

**Tech Stack:** Go 1.26.5、Windows `GetProcessTimes`、Wails v2.12.0、React 19、TypeScript 5.9、Vitest 4、PowerShell 7。

## Global Constraints

- 设计依据：`docs/superpowers/specs/2026-08-03-node-tray-handshake-force-exit-fix-design.md`。
- 不增加进程签名、用户身份、启动时间容差或全机进程扫描。
- 强制退出继续直接终止本次 NodeTray 已记录的 PID，不在终止前重新验证路径或启动时间。
- 不修改配置格式、Wails 导出签名或前端 `ForceExitResult` 结构。
- 不修改 Agent/Helper 控制协议字段；`StartedAtUnixMS` 保留，但不参与握手认领。
- Worker 快照失败不能产生通用 `workers` 失败项；已知残留只能报告为 `worker:<PID>`。
- 后台失败时 UI 保持打开；后台结果全部成功后才允许 Wails Quit。
- 当前源码目录没有 `.git` 元数据，标识为 `N/A_NO_GIT_METADATA`；不得擅自初始化仓库。每个任务用测试结果代替提交检查点。
- 真实 Agent、Helper、Worker、UAC、计划任务和 HKCU 操作与静态实现分开；未实际执行的动态项保持 `BLOCKED_NOT_RUN_DYNAMIC`。

## 文件职责映射

- `internal/nodetray/process/identity.go`：区分“相同 PID+路径”和“完整 Windows 进程身份”。
- `internal/nodetray/supervisor/component.go`：验证控制状态认领条件，并用 Inspector 身份生成 UI 状态。
- `internal/nodetray/supervisor/supervisor.go`：把权威身份传入 Start、Stop、Refresh、Adopt 状态转换，返回明确握手摘要。
- `internal/nodetray/production/managed.go`：Adopt 时忽略状态自报时间；未初始化组件强制停止幂等成功。
- `internal/nodetray/app/service.go`：Worker 快照失败不再形成虚假的后台存活结论。
- 对应 `*_test.go`：以 RED→GREEN 固化上述行为。
- `docs/deployment/node-tray.md`、`docs/acceptance/node-tray-lifecycle-repair-2026-08-03.md`：记录新语义、门禁结果和动态验收边界。

---

### Task 1: 让 Supervisor 只使用 Inspector 启动时间

**Files:**

- Modify: `internal/nodetray/process/identity.go:21-28`
- Modify: `internal/nodetray/process/identity_test.go:12-40`
- Modify: `internal/nodetray/supervisor/component.go:11-61`
- Modify: `internal/nodetray/supervisor/component_test.go:10-65`
- Modify: `internal/nodetray/supervisor/supervisor.go:388-399, 565-570, 643-656, 670-683`
- Modify: `internal/nodetray/supervisor/supervisor_test.go:268-343`

**Interfaces:**

- Produces: `process.SamePIDAndExecutable(expected, actual process.Identity) bool`。
- Produces: `statusClaimError(spec Spec, expected process.Identity, status nodectl.Status) error`。
- Produces: `stateFromStatus(spec Spec, identity process.Identity, status nodectl.Status, lifecycle traymodel.Lifecycle) traymodel.ComponentState`。
- Preserves: `process.SameProcess` 继续严格比较 PID、Windows 创建时间和最终路径，用于 NodeTray 自己跟踪 PID复用。

- [ ] **Step 1: 为 PID+路径比较编写失败测试**

在 `identity_test.go` 添加：

```go
func TestSamePIDAndExecutableIgnoresReportedTimeButRejectsPIDOrPath(t *testing.T) {
	base := Identity{PID: 42, StartedAtUnixMS: 123456, ExecutablePath: `C:\Program Files\Node\agent.exe`}
	reported := base
	reported.StartedAtUnixMS += 250
	if !SamePIDAndExecutable(base, reported) {
		t.Fatal("PID and executable match was rejected because reported time drifted")
	}
	for _, drifted := range []Identity{
		{PID: 43, ExecutablePath: base.ExecutablePath},
		{PID: 42, ExecutablePath: `C:\Other\agent.exe`},
	} {
		if SamePIDAndExecutable(base, drifted) {
			t.Fatalf("drifted PID or path was accepted: %+v", drifted)
		}
	}
}
```

- [ ] **Step 2: 运行测试确认 RED**

```powershell
$env:GOCACHE='D:\code\mySingerServer\.tmp\nodetray-go-cache'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test .\internal\nodetray\process -run '^TestSamePIDAndExecutable' -count=1
```

预期：编译失败，提示 `undefined: SamePIDAndExecutable`。

- [ ] **Step 3: 实现两级身份比较**

在 `identity.go` 实现并让 `SameProcess` 复用：

```go
func SamePIDAndExecutable(expected, actual Identity) bool {
	return expected.PID > 0 &&
		expected.PID == actual.PID &&
		expected.ExecutablePath != "" &&
		actual.ExecutablePath != "" &&
		sameExecutablePath(expected.ExecutablePath, actual.ExecutablePath)
}

func SameProcess(expected, actual Identity) bool {
	return SamePIDAndExecutable(expected, actual) &&
		expected.StartedAtUnixMS > 0 &&
		expected.StartedAtUnixMS == actual.StartedAtUnixMS
}
```

- [ ] **Step 4: 修改握手测试，证明只忽略自报时间**

从 `TestStatusClaimsExactProcessIdentityAndFingerprint` 的拒绝表中移除
`creation time`，新增：

```go
reportedTimeDrift := base
reportedTimeDrift.StartedAtUnixMS += 250
if !statusClaimsProcess(spec, identity, reportedTimeDrift) {
	t.Fatal("matching PID, path and fingerprint was rejected because self-reported time drifted")
}

state := stateFromStatus(spec, identity, reportedTimeDrift, traymodel.Running)
if state.PID != identity.PID || state.StartedAtUnixMS != identity.StartedAtUnixMS {
	t.Fatalf("state used self-reported identity: %#v", state)
}
```

保留并继续验证 Component、PID、最终路径和配置 SHA-256 的四类负向用例。

- [ ] **Step 5: 运行 Supervisor 测试确认 RED**

```powershell
$env:GOCACHE='D:\code\mySingerServer\.tmp\nodetray-go-cache'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test .\internal\nodetray\supervisor -run 'TestStatusClaims|TestComponent' -count=1
```

预期：时间漂移仍被拒绝，或 `stateFromStatus` 参数数量不匹配。

- [ ] **Step 6: 实现明确握手错误和权威状态转换**

在 `component.go` 增加：

```go
import "errors"

func statusClaimError(spec Spec, expected process.Identity, status nodectl.Status) error {
	switch {
	case status.Component != spec.Component:
		return errors.New("control handshake component does not match")
	case status.PID != expected.PID:
		return errors.New("control handshake PID does not match")
	case !process.SamePIDAndExecutable(expected, process.Identity{
		PID: status.PID, ExecutablePath: status.ExecutablePath,
	}):
		return errors.New("control handshake executable path does not match")
	case status.ConfigSHA256 != spec.ExpectedSHA256:
		return errors.New("control handshake config fingerprint does not match")
	default:
		return nil
	}
}

func statusClaimsProcess(spec Spec, expected process.Identity, status nodectl.Status) bool {
	return statusClaimError(spec, expected, status) == nil
}
```

把 `stateFromStatus` 改为接收 `identity process.Identity`，并用
`identity.PID`、`identity.StartedAtUnixMS` 计算 PID、启动时间和运行时长。不要读取
`status.StartedAtUnixMS` 生成状态。

- [ ] **Step 7: 更新 Supervisor 的四条调用路径**

按下列对应关系修改 `supervisor.go`：

```go
// Start
claimErr := statusClaimError(s.spec, identity, status)
state := stateFromStatus(s.spec, identity, status, traymodel.Running)

// Stop / Refresh
claimErr := statusClaimError(claimSpec, s.claimed, status)
state := stateFromStatus(claimSpec, s.claimed, status, lifecycle)

// Adopt
claimErr := statusClaimError(s.spec, candidate, status)
state := stateFromStatus(s.spec, candidate, status, lifecycle)
```

`claimErr != nil` 时继续返回 `unclaimed_instance`，但将 `claimErr` 传给
`failLocked`，使摘要明确指出 Component、PID、路径或配置指纹不匹配。

- [ ] **Step 8: 添加完整 Start 回归测试**

在 `supervisor_test.go` 添加：

```go
func TestStartAcceptsSelfReportedTimeDriftAndPublishesInspectorTime(t *testing.T) {
	spec := testAgentSpec()
	identity := testIdentity(spec.ExecutablePath, 1001, 123456)
	status := readyAgentStatus(spec, identity)
	status.StartedAtUnixMS += 250
	inspector := newFakeInspector(identity)
	s := New(spec, &fakeLauncher{identities: []process.Identity{identity}}, inspector,
		&fakeController{status: status}, &fakeTerminator{})

	states, cancel := s.Subscribe(4)
	defer cancel()
	<-states // initial stopped
	if result := s.Start(context.Background()); !result.OK {
		t.Fatalf("Start = %#v", result)
	}
	var running traymodel.ComponentState
	for running.Lifecycle != traymodel.Running {
		running = <-states
	}
	if running.StartedAtUnixMS != identity.StartedAtUnixMS {
		t.Fatalf("running state = %#v", running)
	}
}
```

现有 `newFakeInspector(identity)` 和 `&fakeTerminator{}` 可直接使用；不得引入定时
休眠。

- [ ] **Step 9: 运行 Task 1 全部测试并格式化**

```powershell
& 'C:\tmp\go1.26.5\go\bin\gofmt.exe' -w `
  internal\nodetray\process\identity.go `
  internal\nodetray\process\identity_test.go `
  internal\nodetray\supervisor\component.go `
  internal\nodetray\supervisor\component_test.go `
  internal\nodetray\supervisor\supervisor.go `
  internal\nodetray\supervisor\supervisor_test.go
$env:GOCACHE='D:\code\mySingerServer\.tmp\nodetray-go-cache'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test .\internal\nodetray\process .\internal\nodetray\supervisor -count=1
```

预期：两个包全部 `ok`。

- [ ] **Step 10: 记录检查点**

记录 Task 1 的命令、退出码和测试名。当前为 `N/A_NO_GIT_METADATA`，不得运行
`git init`；如果未来在正式 Git checkout 执行，则提交信息使用：

```text
fix: use inspected process time for tray handshake
```

---

### Task 2: 修复 NodeTray 启动时对已有进程的 Adopt

**Files:**

- Modify: `internal/nodetray/production/managed.go:31-50`
- Modify: `internal/nodetray/production/managed_test.go:115-170`

**Interfaces:**

- Consumes: `process.SamePIDAndExecutable`（Task 1）。
- Produces: `ManagedComponent.Adopt` 使用 Inspector 身份作为 Supervisor candidate。
- Preserves: Inspector 第一次检查与 Supervisor 内部第二次检查之间发生完整身份漂移时仍 fail-closed。

- [ ] **Step 1: 添加自报时间漂移 Adopt 失败测试**

在 `managed_test.go` 添加：

```go
func TestManagedAdoptUsesInspectedCreationTimeInsteadOfReportedTime(t *testing.T) {
	identity := managedIdentity()
	reported := managedStatus(identity)
	reported.StartedAtUnixMS += 250
	controller := &managedController{statuses: []nodectl.Status{reported, reported, reported}}
	inspector := &managedInspector{identities: []process.Identity{identity, identity, identity}}
	managed := newManagedForTest(controller, inspector)

	if result := managed.Adopt(context.Background()); !result.OK {
		t.Fatalf("Adopt = %#v", result)
	}
	state := managed.Refresh(context.Background())
	if state.StartedAtUnixMS != identity.StartedAtUnixMS {
		t.Fatalf("Refresh = %#v", state)
	}
}
```

- [ ] **Step 2: 运行测试确认 RED**

```powershell
$env:GOCACHE='D:\code\mySingerServer\.tmp\nodetray-go-cache'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test .\internal\nodetray\production -run '^TestManagedAdoptUsesInspectedCreationTime' -count=1
```

预期：返回 `identity_mismatch`。

- [ ] **Step 3: 让 Adopt 传递 Inspector 身份**

把 `ManagedComponent.Adopt` 的候选构造改为：

```go
reported := process.Identity{PID: status.PID, ExecutablePath: status.ExecutablePath}
actual, err := m.inspector.Inspect(status.PID)
if err != nil || !process.SamePIDAndExecutable(actual, reported) {
	return managedFailure("identity_mismatch", "组件身份已变化")
}
state := m.supervisor.Adopt(ctx, actual)
if state.Lifecycle == traymodel.Failed ||
	state.PID != actual.PID || state.StartedAtUnixMS != actual.StartedAtUnixMS {
	return managedFailure(managedAdoptCode(state.ErrorCode), "组件认领失败")
}
```

不得把 `status.StartedAtUnixMS` 复制进 `actual`。

- [ ] **Step 4: 改写负向测试的职责边界**

把原先“状态时间与第一次 Inspector 时间不同即拒绝”的用例改成成功合同。新增
Inspector 两次读取发生时间变化的拒绝用例：

```go
func TestManagedAdoptRejectsInspectedIdentityDriftInsideSupervisor(t *testing.T) {
	first := managedIdentity()
	second := first
	second.StartedAtUnixMS++
	status := managedStatus(first)
	controller := &managedController{statuses: []nodectl.Status{status, status}}
	inspector := &managedInspector{identities: []process.Identity{first, second}}
	managed := newManagedForTest(controller, inspector)

	result := managed.Adopt(context.Background())
	if result.OK || result.ErrorCode != "unclaimed_instance" {
		t.Fatalf("Adopt = %#v", result)
	}
}
```

保留 PID 和路径不匹配测试。

- [ ] **Step 5: 格式化并运行 production 测试**

```powershell
& 'C:\tmp\go1.26.5\go\bin\gofmt.exe' -w `
  internal\nodetray\production\managed.go `
  internal\nodetray\production\managed_test.go
$env:GOCACHE='D:\code\mySingerServer\.tmp\nodetray-go-cache'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test .\internal\nodetray\production -count=1
```

预期：包全部 `ok`；状态自报时间漂移成功，Inspector 自身身份漂移仍失败。

- [ ] **Step 6: 记录检查点**

记录测试输出。当前不提交；未来 Git 提交信息：

```text
fix: adopt tray components with inspected identity
```

---

### Task 3: 让未初始化组件的强制停止幂等成功

**Files:**

- Modify: `internal/nodetray/production/managed.go:349-353`
- Modify: `internal/nodetray/production/managed_test.go`

**Interfaces:**

- Produces: `(*SharedComponent).ForceStopTracked(context.Context)` 在没有内部组件时返回 `traymodel.OperationResult{OK: true}`。
- Preserves: 未初始化组件的 Start、Stop、Restart、Refresh 继续返回 unavailable。

- [ ] **Step 1: 编写未初始化 Helper 的失败测试**

```go
func TestUninitializedSharedComponentForceStopIsIdempotent(t *testing.T) {
	shared := &SharedComponent{}
	if result := shared.ForceStopTracked(context.Background()); !result.OK {
		t.Fatalf("ForceStopTracked = %#v", result)
	}
	if result := shared.Start(context.Background()); result.OK || result.ErrorCode != "unavailable" {
		t.Fatalf("Start = %#v", result)
	}
	if state := shared.Refresh(context.Background()); state.ErrorCode != "unavailable" {
		t.Fatalf("Refresh = %#v", state)
	}
}
```

- [ ] **Step 2: 运行测试确认 RED**

```powershell
$env:GOCACHE='D:\code\mySingerServer\.tmp\nodetray-go-cache'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test .\internal\nodetray\production -run '^TestUninitializedSharedComponentForceStopIsIdempotent$' -count=1
```

预期：`ForceStopTracked` 返回 `unavailable`。

- [ ] **Step 3: 实现最小幂等语义**

```go
func (s *SharedComponent) ForceStopTracked(ctx context.Context) traymodel.OperationResult {
	if value := s.snapshot(); value != nil {
		return value.ForceStopTracked(ctx)
	}
	return traymodel.OperationResult{OK: true}
}
```

不要修改其他 SharedComponent 方法。

- [ ] **Step 4: 格式化并运行测试**

```powershell
& 'C:\tmp\go1.26.5\go\bin\gofmt.exe' -w `
  internal\nodetray\production\managed.go `
  internal\nodetray\production\managed_test.go
$env:GOCACHE='D:\code\mySingerServer\.tmp\nodetray-go-cache'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test .\internal\nodetray\production -count=1
```

预期：全部 `ok`。

- [ ] **Step 5: 记录检查点**

当前不提交；未来 Git 提交信息：

```text
fix: make absent tray component force stop idempotent
```

---

### Task 4: 只把已知存活 PID计入强制退出失败

**Files:**

- Modify: `internal/nodetray/app/service.go:309-355`
- Modify: `internal/nodetray/app/service_test.go:718-760`
- Verify: `nodetray/app_test.go:519-559`
- Verify: `nodetray/frontend/src/components/ExitDialog.test.tsx`

**Interfaces:**

- Consumes: 未初始化 Helper 的幂等 ForceStop（Task 3）。
- Produces: `ForceExitAll` 不再返回通用 `workers`，只返回 `helper`、`agent` 或 `worker:<PID>`。
- Preserves: 操作顺序为 Worker 快照、Helper 强制停止、Agent 强制停止、已知 Worker PID等待。

- [ ] **Step 1: 编写 Worker 快照失败但后台退出成功的 RED 测试**

```go
func TestForceExitAllIgnoresWorkerSnapshotFailureWhenTrackedComponentsExit(t *testing.T) {
	s, calls, _, agent, helper, _ := serviceFixture(t)
	s.workers = fakeWorkers{err: errors.New("control unavailable"), calls: calls}
	s.processWaiter = &fakeProcessWaiter{calls: calls, errs: map[int]error{}}
	agent.results["force"] = traymodel.OperationResult{OK: true}
	helper.results["force"] = traymodel.OperationResult{OK: true}

	result := s.ForceExitAll(context.Background())
	if !result.OK || len(result.FailedComponents) != 0 {
		t.Fatalf("ForceExitAll = %#v", result)
	}
	if !reflect.DeepEqual(*calls, []string{"workers-snapshot", "helper-force", "agent-force"}) {
		t.Fatalf("calls = %v", *calls)
	}
}
```

- [ ] **Step 2: 编写快照失败与 Agent失败不重复报错的 RED 测试**

```go
func TestForceExitAllSnapshotFailureAndAgentFailureReportsOnlyAgent(t *testing.T) {
	s, _, _, agent, helper, _ := serviceFixture(t)
	s.workers = fakeWorkers{err: errors.New("control unavailable")}
	helper.results["force"] = traymodel.OperationResult{OK: true}
	agent.results["force"] = traymodel.OperationResult{ErrorCode: "force_exit_failed"}

	result := s.ForceExitAll(context.Background())
	if result.OK || !reflect.DeepEqual(result.FailedComponents, []string{"agent"}) {
		t.Fatalf("ForceExitAll = %#v", result)
	}
}
```

- [ ] **Step 3: 运行测试确认 RED**

```powershell
$env:GOCACHE='D:\code\mySingerServer\.tmp\nodetray-go-cache'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test .\internal\nodetray\app -run '^TestForceExitAll' -count=1
```

预期：结果仍包含 `workers`。

- [ ] **Step 4: 删除通用 Worker 失败项**

把 Worker 捕获块改为：

```go
workers := []traymodel.WorkerState{}
if s.workers != nil {
	if snapshot, err := s.workers.Snapshot(ctx); err == nil {
		workers = snapshot
	}
}
```

后续 Helper、Agent 和已知 Worker PID等待代码保持原顺序。不要因为快照失败提前
返回，也不要添加 `workers`。

- [ ] **Step 5: 强化已知 Worker PID 精确失败测试**

保留 `TestForceExitAllContinuesAfterFailureAndAggregatesSurvivors`，并明确断言：

```go
wantFailed := []string{"helper", "worker:41"}
if !reflect.DeepEqual(result.FailedComponents, wantFailed) {
	t.Fatalf("failedComponents = %v, want %v", result.FailedComponents, wantFailed)
}
```

再增加 PID 为 0 时不调用 waiter 的断言，防止无效 Worker 槽位被误报。

- [ ] **Step 6: 格式化并运行 app、Backend 和前端退出测试**

```powershell
& 'C:\tmp\go1.26.5\go\bin\gofmt.exe' -w `
  internal\nodetray\app\service.go `
  internal\nodetray\app\service_test.go
$env:GOCACHE='D:\code\mySingerServer\.tmp\nodetray-go-cache'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test .\internal\nodetray\app .\nodetray -run 'ForceExitAll' -count=1
Push-Location nodetray\frontend
try {
  & 'D:\application\nodejs\npm.cmd' test -- ExitDialog.test.tsx App.test.tsx
  if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
} finally {
  Pop-Location
}
```

预期：Go 测试与退出弹窗测试全部通过；Backend 仍只在 `result.OK` 后调用 Quit。

- [ ] **Step 7: 记录检查点**

当前不提交；未来 Git 提交信息：

```text
fix: avoid false force-exit survivor reports
```

---

### Task 5: 全量回归、独立构建和中文文档收口

**Files:**

- Modify: `docs/deployment/node-tray.md:161-171`
- Modify: `docs/acceptance/node-tray-lifecycle-repair-2026-08-03.md`
- Verify: `scripts/build-nodetray.ps1`
- Output: `artifacts/nodetray-handshake-force-exit-fix/`

**Interfaces:**

- Consumes: Tasks 1–4 的完整 Go 行为。
- Produces: 可复制的 Windows amd64 NodeTray 产物和区分静态/动态的验收记录。

- [ ] **Step 1: 更新中文部署说明**

在退出章节明确写入：

```markdown
- 控制状态中的启动时间仅用于兼容展示，NodeTray 以 Windows Inspector 返回的
  创建时间作为权威身份时间；
- 未初始化且没有记录 PID 的组件在强制退出时视为无需终止；
- Worker 快照不可用不代表 Worker 存活。已知 Worker PID 仍逐个等待；快照不可用
  时以 Agent 退出触发 Job Object 的 KILL_ON_JOB_CLOSE 作为其 Worker 结束依据；
- 失败列表只显示 helper、agent 或 worker:<PID>，不再显示通用 workers。
```

- [ ] **Step 2: 运行 Go 全量测试**

```powershell
$env:GOCACHE='D:\code\mySingerServer\.tmp\nodetray-go-cache'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test .\internal\nodetray\... .\nodetray -count=1 -timeout 180s
```

预期：全部列出的 Go 包 `ok`。

- [ ] **Step 3: 运行 Windows race 测试**

```powershell
$env:GOCACHE='D:\code\mySingerServer\.tmp\nodetray-race-go-cache'
$env:CGO_ENABLED='1'
$env:CC='C:\Users\Administrator\AppData\Local\Temp\winlibs-gcc\mingw64\bin\gcc.exe'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -race `
  .\internal\nodetray\supervisor `
  .\internal\nodetray\production `
  .\internal\nodetray\app `
  -count=1 -timeout 180s
```

预期：三个包通过且无 `DATA RACE`。

- [ ] **Step 4: 运行前端全量门禁**

```powershell
Push-Location nodetray\frontend
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

预期：Vitest、ESLint、TypeScript 和 Vite 全部退出码 0。已有 Fast Refresh warning
可以记录，但不得出现 error。

- [ ] **Step 5: 确认 Wails 绑定没有漂移**

```powershell
rg -n 'ForceExitAll|ForceExitResult' nodetray\frontend\wailsjs\go nodetray\frontend\wailsjs\go\models.ts
$old = rg -n 'ExitTray\(' nodetray\frontend\wailsjs nodetray\frontend\src 2>$null
if ($LASTEXITCODE -eq 0) { $old; throw '旧 ExitTray 绑定重新出现' }
if ($LASTEXITCODE -ne 1) { throw 'ExitTray 静态检查失败' }
```

预期：`ForceExitAll` 存在，旧 `ExitTray(` 不存在。Go 导出签名未变，不运行绑定
生成命令。

- [ ] **Step 6: 构建新的独立产物**

```powershell
$cache='D:\code\mySingerServer\.tmp\nodetray-fix-build-cache'
$temp='D:\code\mySingerServer\.tmp\nodetray-fix-build-temp'
New-Item -ItemType Directory -Path $cache,$temp -Force | Out-Null
$env:GOCACHE=$cache
$env:GOTMPDIR=$temp
& '.\scripts\build-nodetray.ps1' `
  -Go 'C:\tmp\go1.26.5\go\bin\go.exe' `
  -Npm 'D:\application\nodejs\npm.cmd' `
  -OutDir 'artifacts\nodetray-handshake-force-exit-fix'
```

预期：最终 JSON 状态为 `PASS`，产物包含 `nodetray.exe` 和
`MicrosoftEdgeWebview2Setup.exe`，架构为 Windows amd64，执行级别为
`asInvoker`。

- [ ] **Step 7: 核对产物哈希与签名事实**

```powershell
$stage='D:\code\mySingerServer\artifacts\nodetray-handshake-force-exit-fix'
Get-ChildItem -LiteralPath $stage -File | ForEach-Object {
  [pscustomobject]@{
    Name=$_.Name
    Length=$_.Length
    SHA256=(Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash
    Signature=(Get-AuthenticodeSignature -LiteralPath $_.FullName).Status
  }
} | Format-Table -AutoSize
```

把实际大小、SHA-256 和签名状态写入验收记录，不预设 `nodetray.exe` 已签名。

- [ ] **Step 8: 更新专项验收记录**

在 `docs/acceptance/node-tray-lifecycle-repair-2026-08-03.md` 增加本次修复批次：

```markdown
## 握手与强制退出误判修复批次

- Agent/Helper 自报启动时间不参与握手：PASS（自动化）
- Inspector 内部完整身份漂移仍拒绝：PASS（自动化）
- 未初始化 Helper 强制停止幂等成功：PASS（自动化）
- Worker 快照失败不再生成通用 workers：PASS（自动化）
- 已知 Worker PID残留仍阻止 UI退出：PASS（自动化）
- 真实 Windows Agent 启动与统一强制退出：BLOCKED_NOT_RUN_DYNAMIC，除非本批次实际执行并保存证据
```

附上 Steps 2–7 的实际命令、退出码和产物值。

- [ ] **Step 9: 执行文档自检**

```powershell
$docs=@(
  'docs\superpowers\specs\2026-08-03-node-tray-handshake-force-exit-fix-design.md',
  'docs\superpowers\plans\2026-08-03-node-tray-handshake-force-exit-fix.md',
  'docs\deployment\node-tray.md',
  'docs\acceptance\node-tray-lifecycle-repair-2026-08-03.md'
)
$docs | ForEach-Object { if(-not (Test-Path -LiteralPath $_)){ throw "missing document: $_" } }
$pattern=@(('TO'+'DO'),('T'+'BD'),('待'+'定'),('稍后'+'补充'),('适当'+'处理'),('类似'+'处理')) -join '|'
$hits=rg -n $pattern @docs 2>$null
if($LASTEXITCODE -eq 0){ $hits; throw 'document placeholders found' }
if($LASTEXITCODE -ne 1){ throw 'placeholder scan failed' }
```

预期：文件均存在且无占位符。

- [ ] **Step 10: 单独执行真实 Windows 动态验收**

仅在用户明确要求执行真实进程验证时进行：

1. 通过当前 NodeTray 弹窗强制退出旧进程；
2. 以管理员 PowerShell 将新 `nodetray.exe` 复制到
   `C:\Program Files\MySingerServer\nodetray.exe`；
3. 启动 NodeTray，点击 Agent 启动；
4. 记录 Agent PID、两个 Worker PID和 UI 状态，确认不再出现握手异常；
5. 保持 `helperEnabled=false`，触发统一强制退出；
6. 确认不再显示 `workers、helper`，且 Agent/Worker 退出后 UI关闭；
7. 使用只读命令保存退出后证据：

```powershell
Get-CimInstance Win32_Process |
  Where-Object { $_.Name -in @('nodetray.exe','agent.exe','worker.exe','helper.exe') } |
  Select-Object Name,ProcessId,ParentProcessId,CreationDate,ExecutablePath
```

未执行该步骤时必须记录 `BLOCKED_NOT_RUN_DYNAMIC`，不能写成 PASS。

- [ ] **Step 11: 最终检查点**

确认验收记录包含所有实际输出，源码仍为 `N/A_NO_GIT_METADATA`。不得初始化或伪造
Git 提交；未来正式 checkout 的收口提交信息使用：

```text
fix: repair nodetray handshake and force-exit detection
```
