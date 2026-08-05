# NodeTray 完整退出实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让托盘右键“退出”可靠停止可信认领的 Helper、Agent 和 Worker，然后退出 NodeTray。

**Architecture:** `internal/nodetray/app.Service` 负责统一的“优雅停止 → 强制停止”退出编排；`nodetray/main.go` 的原生托盘命令直接调用该后端能力，不再依赖隐藏窗口的前端事件。Agent 的既有关闭契约负责收拢 Worker，强制回退仍只作用于可信认领的组件。

**Tech Stack:** Go 1.26、Wails v2.12、现有 NodeTray supervisor/component 接口、Go testing。

## Global Constraints

- 只强制结束托盘已可信认领的进程，不按进程名扫描或强杀其他实例。
- Helper 先于 Agent 停止；Agent 的退出负责关闭 Worker。
- 任一强制停止失败时保留托盘进程并返回脱敏错误。
- 不改变窗口右上角关闭按钮的“最小化到托盘”语义。
- 当前工作目录没有 `.git`，本计划不执行提交步骤，以测试和构建结果作为检查点。

---

### Task 1: 后端完整退出编排

**Files:**
- Modify: `internal/nodetray/app/service.go`
- Test: `internal/nodetray/app/service_test.go`

**Interfaces:**
- Consumes: `Component.Stop(context.Context)`、`Component.ForceStopClaimed(context.Context)`、`Service.exit`。
- Produces: `Service.ExitTray(context.Context, bool) traymodel.OperationResult` 的完整停止语义。

- [ ] **Step 1: 写失败测试**

在 `service_test.go` 增加表驱动测试，断言：

```go
result := service.ExitTray(context.Background(), true)
wantCalls := []string{"helper-stop", "agent-stop", "exit"}
```

并覆盖 Helper/Agent 的 `stop` 失败后调用 `force`、`force` 失败时不调用 `exit`。

- [ ] **Step 2: 验证测试按预期失败**

运行：

```powershell
go test -count=1 ./internal/nodetray/app -run '^TestExitTray'
```

预期：现有实现顺序为 Agent→Helper，且停止失败时不会强制回退。

- [ ] **Step 3: 写最小实现**

在 `service.go` 增加私有编排函数：

```go
func stopComponentForExit(ctx context.Context, component Component) traymodel.OperationResult {
    stopped := sanitizeOperation(component.Stop(ctx))
    if stopped.OK {
        return stopped
    }
    return sanitizeOperation(component.ForceStopClaimed(ctx))
}
```

`ExitTray(true)` 按 Helper→Agent 调用该函数；全部成功后才调用 `s.exit()`。

- [ ] **Step 4: 验证后端测试通过**

运行：

```powershell
go test -count=1 ./internal/nodetray/app
```

预期：PASS。

### Task 2: 托盘右键直接调用完整退出

**Files:**
- Modify: `nodetray/main.go`
- Test: `nodetray/app_test.go`

**Interfaces:**
- Consumes: `Backend.ExitTray(true)`。
- Produces: `traynative.ExitTray` 命令的同步完整退出行为。

- [ ] **Step 1: 写失败测试**

修改/增加托盘命令测试，单独触发：

```go
handleTrayCommand(ctx, backend, showConsole, traynative.ExitTray, notify)
```

断言记录包含 `helper-stop`、`agent-stop`、`exit`，并且没有 `window-close-requested` 事件。

- [ ] **Step 2: 验证测试按预期失败**

运行：

```powershell
go test -count=1 ./nodetray -run '^TestTrayExit'
```

预期：当前实现只发送窗口事件，测试失败。

- [ ] **Step 3: 写最小实现**

将 `nodetray/main.go` 的退出分支改为：

```go
case traynative.ExitTray:
    result := backend.ExitTray(true)
    if !result.OK {
        reportTrayAttention(ctx, "tray_exit_failed")
    }
```

- [ ] **Step 4: 验证托盘测试通过**

运行：

```powershell
go test -count=1 ./nodetray
```

预期：PASS。

### Task 3: 基础回归与修复版构建

**Files:**
- Verify: `cmd/agent/main.go`
- Verify: `internal/nodetray/...`
- Output: `.tmp/node-repair-20260803-003/nodetray.exe`
- Output: `.tmp/node-repair-20260803-003/agent.exe`

**Interfaces:**
- Consumes: Task 1 和 Task 2 的实现。
- Produces: 可替换的 NodeTray 与包含上一项摘要修复的 Agent 二进制。

- [ ] **Step 1: 运行相关 Go 测试**

```powershell
go test -count=1 ./nodetray ./internal/nodetray/... ./cmd/agent
```

预期：全部 PASS。

- [ ] **Step 2: 构建修复版程序**

使用项目固定 Go 1.26.5 和 Wails v2.12.0 构建 `nodetray.exe`，并以 `CGO_ENABLED=0` 构建 `agent.exe`。构建输出写入 `.tmp/node-repair-20260803-003`，不直接覆盖 `C:\Program Files\MySingerServer`。

- [ ] **Step 3: 校验产物**

对两个 EXE 计算 SHA-256，并确认文件非空；报告安装目录仍未修改，等待用户执行替换。
