# 节点托盘 UI、构建与验收实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:test-driven-development for every task and superpowers:verification-before-completion before reporting completion.

**Goal:** 用 Wails v2.12.0 + React 实现已确认的四页签轻量节点控制台、原生通知区域交互、结构化配置表单，并把 `nodetray.exe` 纳入可复现构建、发布清单与 Windows 动态验收。

**Architecture:** `nodetray/main.go` 只负责模式分派与 Wails 启动，`nodetray/app.go` 把前一计划的 context-aware 应用服务适配为无 context 的 Wails 绑定；React 只消费生成的类型化 bindings 和状态事件。通知区域使用 Windows `Shell_NotifyIcon`，窗口关闭由 Go 侧拦截为隐藏。前端静态资产通过 `go:embed` 打入单个普通权限 `nodetray.exe`。

**Tech Stack:** Go 1.22、Wails v2.12.0、WebView2、React 19.2.8、React DOM 19.2.8、TypeScript 5.9.3、Vite 8.2.0、Vitest 4.1.10、Testing Library 16.3.2、ESLint 10.8.0、PowerShell 7。

**前置计划:** [节点组件本机控制面实施计划](2026-08-02-node-control-plane.md)、[节点托盘后端实施计划](2026-08-02-node-tray-backend.md)

**前置设计:** [媒体节点托盘管理程序设计](../specs/2026-08-02-node-tray-design.md)

---

## 全局约束

- 页签固定为“总览”“Agent”“删除 Helper”“程序设置”，不加入远程节点管理页。
- UI 不展示或编辑原始 JSON；全部支持配置字段都有结构化控件，测试字段 `worker.crash_injection` 除外。
- Worker 只读；任何页面和托盘菜单都不得出现启动、停止、重启或删除单个 Worker 的入口。
- 密码输入默认隐藏，只在“替换密码”控件的短生命周期组件内存中存在，不进入全局状态仓库、浏览器存储、URL、事件 payload、测试快照或诊断复制内容；加载配置时后端不回显旧密码。
- 默认设置下关闭主窗口只隐藏；用户关闭“关闭时隐藏到托盘”后，关闭按钮必须显示退出对话框，不能静默结束组件。默认退出只退出托盘，“停止组件后退出”是第二个明确动作。
- Wails/WebView2 只加载内嵌本地资源，不允许远程脚本、字体、图片或开发服务器。
- `nodetray.exe` 使用 `asInvoker`；Helper 的管理员能力仍通过 one-shot UAC/固定计划任务实现。
- 本计划不决定 MSI/安装器格式，不加入自动更新服务。
- 当前目录没有 `.git` 元数据。提交步骤仅在 Git 工作树执行，否则记录 `N/A_NO_GIT_METADATA`。

## 固定版本与目录

```text
nodetray/
  main.go
  app.go
  app_test.go
  wails.json
  build/windows/nodetray.manifest
  build/windows/icon.ico
  frontend/
    package.json
    package-lock.json
    tsconfig.json
    vite.config.ts
    vitest.config.ts
    eslint.config.js
    index.html
    src/
      main.tsx
      App.tsx
      app.css
      styles/tokens.css
      bindings/backend.ts
      components/
      pages/
      state/
internal/nodetray/windows/tray/
scripts/build-nodetray.ps1
tests/windows/Test-NodeTray.ps1
```

`nodetray/frontend` 是独立前端包和独立 lockfile，不引用 `webui/node_modules`。与现有中央 `webui` 相同的依赖必须使用本计划头部钉死的精确版本。

## Task 1：建立 Wails v2.12.0 壳、普通权限入口和类型化绑定

**Files:**

- Create: `nodetray/main.go`
- Create: `nodetray/app.go`
- Create: `nodetray/app_test.go`
- Create: `nodetray/wails.json`
- Create: `nodetray/build/windows/nodetray.manifest`
- Create: `nodetray/frontend/package.json`
- Create: `nodetray/frontend/package-lock.json`
- Create: `nodetray/frontend/tsconfig.json`
- Create: `nodetray/frontend/vite.config.ts`
- Create: `nodetray/frontend/vitest.config.ts`
- Create: `nodetray/frontend/eslint.config.js`
- Create: `nodetray/frontend/index.html`
- Create: `nodetray/frontend/src/main.tsx`
- Create: `nodetray/frontend/src/App.tsx`
- Test: `nodetray/frontend/src/App.test.tsx`
- Modify: `go.mod`
- Modify: `go.sum`

### Step 1：先写 Wails Adapter 失败测试

`nodetray/app.go` 的公开方法不能把 Go `context.Context` 暴露给 TypeScript；它在 `Startup` 保存 Wails context，再调用前一计划的 `internal/nodetray/app.Service`：

```go
type Backend struct {
	ctx     context.Context
	service *trayapp.Service
}

func NewBackend(service *trayapp.Service) *Backend
func (b *Backend) Startup(ctx context.Context)
func (b *Backend) Shutdown(ctx context.Context)
func (b *Backend) GetOverview() (traymodel.Overview, error)
func (b *Backend) GetAgentForm() (trayconfig.AgentForm, error)
func (b *Backend) ValidateAgent(value trayconfig.AgentForm) []trayconfig.FieldError
func (b *Backend) SaveAgent(value trayconfig.AgentForm) traymodel.OperationResult
func (b *Backend) SaveAndRestartAgent(value trayconfig.AgentForm) traymodel.OperationResult
func (b *Backend) StartAgent() traymodel.OperationResult
func (b *Backend) StopAgent() traymodel.OperationResult
func (b *Backend) RestartAgent() traymodel.OperationResult
func (b *Backend) ForceStopAgent() traymodel.OperationResult
func (b *Backend) GetHelperForm() (trayconfig.HelperForm, error)
func (b *Backend) ValidateHelper(value trayconfig.HelperForm) []trayconfig.FieldError
func (b *Backend) SaveHelper(value trayconfig.HelperForm) traymodel.OperationResult
func (b *Backend) StartHelper() traymodel.OperationResult
func (b *Backend) StopHelper() traymodel.OperationResult
func (b *Backend) RestartHelper() traymodel.OperationResult
func (b *Backend) ForceStopHelper() traymodel.OperationResult
func (b *Backend) GetTraySettings() (traymodel.TraySettings, error)
func (b *Backend) SaveTraySettings(value traymodel.TraySettings) traymodel.OperationResult
func (b *Backend) OpenLocation(kind traymodel.LocationKind) traymodel.OperationResult
func (b *Backend) ExitTray(stopComponents bool) traymodel.OperationResult
```

测试断言 Startup 前调用返回 `backend_not_started` 脱敏错误；Shutdown 取消事件订阅但不停止组件；每个方法准确转发一次。

### Step 2：运行 Go RED

Run: `go test ./nodetray -run TestBackend -count=1`

Expected: FAIL，Backend 尚未实现。

### Step 3：写最小 React 壳失败测试

`App.test.tsx` 断言存在四个 `role=tab`，初始选中“总览”，键盘左右键切换，未知路由不会出现空白页。先用测试桩替代生成 bindings。

### Step 4：创建精确依赖并运行前端 RED

`package.json` 使用：React 19.2.8、React DOM 19.2.8、TypeScript 5.9.3、Vite 8.2.0、Vitest 4.1.10、Testing Library React 16.3.2、user-event 14.6.1、jest-dom 7.0.0、ESLint 10.8.0，以及与 `webui/package.json` 相同的配套版本。运行 `npm install --package-lock-only` 生成 lockfile，随后只允许 `npm ci`。

Run: `npm ci`

Workdir: `nodetray/frontend`

Expected: PASS，依赖来自 lockfile。

Run: `npm test -- --run App.test.tsx`

Workdir: `nodetray/frontend`

Expected: FAIL，页签壳尚未实现。

### Step 5：实现 main、Wails Options 和本地静态资源

`main.go` 先分派 `--elevated-once`，普通模式再获取单实例并启动 Wails。Wails 入口使用 `//go:embed all:frontend/dist`，`assetserver.Options{Assets: assets}`，绑定唯一 `Backend`，并设置 OnStartup/OnShutdown。

窗口初始尺寸 1080×720、最小 860×600、浅色背景；Windows Options 禁止透明窗口和调试快捷入口。WebView2 用户数据目录放当前用户应用数据目录，不放媒体或共享配置目录。

`wails.json` 的项目名固定为 `nodetray`，因此按 Wails v2.12.0 规则使用 `build/windows/nodetray.manifest`。该 manifest 必须包含 `requestedExecutionLevel level="asInvoker"` 和 Windows 10/11 兼容声明，不能出现 `requireAdministrator`。

### Step 6：实现最小四页签 AppShell

先建立语义正确的 tablist/tab/tabpanel 和错误边界，不填业务表单。绑定包装层 `src/bindings/backend.ts` 只 re-export Wails 生成函数，测试通过 module mock 替换。

### Step 7：运行 GREEN 和生成绑定检查

Run: `go test ./nodetray -count=1`

Expected: PASS。

Run: `npm test -- --run App.test.tsx`

Workdir: `nodetray/frontend`

Expected: PASS。

Run: `go run github.com/wailsapp/wails/v2/cmd/wails@v2.12.0 generate module`

Workdir: `nodetray`

Expected: PASS，生成的 TypeScript 模型不包含 `context.Context` 或 `map[string]any`。

### Step 8：提交检查点

Run: `git add nodetray go.mod go.sum && git commit -m "feat: scaffold typed Wails node tray"`

Expected: Git 工作树中提交成功；无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## Task 2：实现统一视觉令牌、页签布局和响应式基础组件

**Files:**

- Create: `nodetray/frontend/src/styles/tokens.css`
- Create: `nodetray/frontend/src/app.css`
- Create: `nodetray/frontend/src/components/AppShell.tsx`
- Create: `nodetray/frontend/src/components/StatusBadge.tsx`
- Create: `nodetray/frontend/src/components/ComponentCard.tsx`
- Create: `nodetray/frontend/src/components/FormField.tsx`
- Create: `nodetray/frontend/src/components/ActionBar.tsx`
- Create: `nodetray/frontend/src/components/ConfirmDialog.tsx`
- Test: `nodetray/frontend/src/components/AppShell.test.tsx`
- Test: `nodetray/frontend/src/components/StatusBadge.test.tsx`
- Modify: `nodetray/frontend/src/App.tsx`

### Step 1：写组件失败测试

测试要求：

- 四页签始终同序；当前页签有文本/ARIA 状态，不只靠颜色；
- `StatusBadge` 对五种生命周期分别输出图标 + 中文文本；
- `ComponentCard` 的操作区在 pending 时禁用冲突动作；
- 860 px 以下改为单列，不发生横向滚动；
- `ConfirmDialog` 打开后焦点进入、Tab 循环、Escape 按策略关闭、关闭后焦点返回触发器。

### Step 2：运行 RED

Run: `npm test -- --run src/components`

Workdir: `nodetray/frontend`

Expected: FAIL，组件尚未实现。

### Step 3：实现轻量统一视觉系统

令牌固定为中性浅色背景、单一蓝色强调、有限状态色、8 px 网格、6/10/14 px 圆角层级；正文 14 px，标题 18/24 px。状态色必须同时配图标和文本。不得引入远程字体、图标 CDN 或重量级 UI 框架；图标使用仓库内联 SVG 组件且有 `aria-hidden`/可访问标签。

表单区使用一致 label/help/error 垂直节奏；常用设置直接显示，高级设置用可键盘展开的 `<details>`，不得把高级字段藏到 JSON。

### Step 4：运行 GREEN 与无障碍静态检查

Run: `npm test -- --run src/components`

Workdir: `nodetray/frontend`

Expected: PASS。

Run: `npm run lint`

Workdir: `nodetray/frontend`

Expected: PASS。

### Step 5：提交检查点

Run: `git add nodetray/frontend/src && git commit -m "feat: add lightweight tabbed tray design system"`

Expected: Git 工作树中提交成功；无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## Task 3：实现状态仓库、总览页和只读 Worker 状态

**Files:**

- Create: `nodetray/frontend/src/state/nodeStore.ts`
- Create: `nodetray/frontend/src/state/nodeStore.test.ts`
- Create: `nodetray/frontend/src/pages/OverviewPage.tsx`
- Test: `nodetray/frontend/src/pages/OverviewPage.test.tsx`
- Create: `nodetray/frontend/src/components/WorkerSummary.tsx`
- Test: `nodetray/frontend/src/components/WorkerSummary.test.tsx`

### Step 1：写状态事件失败测试

`nodeStore` 初次调用 `GetOverview`，再订阅 Wails `component-state`、`operation-progress`、`attention-required` 事件。测试覆盖乱序旧事件丢弃、同组件合并、卸载解除订阅、错误摘要截断和事件 payload 无秘密字段。

状态只驻留内存，不写 localStorage/sessionStorage/IndexedDB。

### Step 2：写总览失败测试

断言总览显示：Agent 状态/PID/启动方式/运行时长；Worker `ready / expected` 与最近脱敏异常；删除 Helper 状态/启动方式/UAC 提示。只提供组件级 Agent 重启/停止、Helper 启动等已确认快捷动作。

加入负断言：不存在 `启动 Worker`、`停止 Worker`、`重启 Worker`、`删除 Worker` 按钮；Worker 行没有任何 action role。

### Step 3：运行 RED

Run: `npm test -- --run 'src/state|OverviewPage|WorkerSummary'`

Workdir: `nodetray/frontend`

Expected: FAIL。

### Step 4：实现状态仓库和总览交互

动作按钮调用类型化 Backend，pending 期间按组件禁用；操作完成以后以返回结果和后端事件为准，不在前端乐观伪造 running/stopped。错误使用页内 attention 区和原生通知请求，不弹出包含秘密的堆栈。

### Step 5：运行 GREEN

Run: `npm test -- --run 'src/state|OverviewPage|WorkerSummary'`

Workdir: `nodetray/frontend`

Expected: PASS。

### Step 6：提交检查点

Run: `git add nodetray/frontend/src/state nodetray/frontend/src/pages/OverviewPage.tsx nodetray/frontend/src/components/WorkerSummary.tsx && git commit -m "feat: show node component overview"`

Expected: Git 工作树中提交成功；无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## Task 4：实现 Agent 全量交互式配置页

**Files:**

- Create: `nodetray/frontend/src/pages/AgentPage.tsx`
- Create: `nodetray/frontend/src/pages/AgentPage.test.tsx`
- Create: `nodetray/frontend/src/components/DatabaseFields.tsx`
- Create: `nodetray/frontend/src/components/PathPicker.tsx`
- Create: `nodetray/frontend/src/components/TagListField.tsx`
- Create: `nodetray/frontend/src/state/useDirtyForm.ts`
- Test: `nodetray/frontend/src/state/useDirtyForm.test.tsx`

### Step 1：写全字段和秘密处理失败测试

测试按设计分组断言全部支持参数有结构化控件：节点、数据库分段、扫描 `scan.*`、同步 `sync.*`、协议、Worker、管线、缩略图、IPC、删除转发、调优。`worker.crash_injection` 不出现。

密码控件：`type=password`、默认空显示表示保留、显式“替换密码”进入编辑、复制诊断不包含密码、前端错误快照不包含密码值。

### Step 2：写校验/保存行为失败测试

断言失焦调用局部校验，保存调用完整 Validate；字段/列表错误定位到具体 `FieldError.Field`；“保存”不调用 Restart；“保存并重启 Agent”先确认未保存影响再调用单一 Backend 方法；失败保持 dirty；成功更新基线并显示配置 SHA 摘要。

路径选择器只接受 Go 后端返回的本地绝对路径，不允许前端自行枚举文件系统。

### Step 3：运行 RED

Run: `npm test -- --run 'AgentPage|useDirtyForm'`

Workdir: `nodetray/frontend`

Expected: FAIL。

### Step 4：实现常用/高级表单和只读 Worker 区

常用区显示 machine/listen/data/database/worker count；高级折叠区承载剩余所有字段。数字控件显示单位和范围，扩展名用标签列表。页首显示 Agent 组件级启动/停止/重启；页底只读 Worker 列表显示索引、PID、Ready、当前任务摘要和最近错误，无操作按钮。

### Step 5：运行 GREEN

Run: `npm test -- --run 'AgentPage|useDirtyForm'`

Workdir: `nodetray/frontend`

Expected: PASS。

### Step 6：提交检查点

Run: `git add nodetray/frontend/src/pages/AgentPage.tsx nodetray/frontend/src/components nodetray/frontend/src/state/useDirtyForm.ts && git commit -m "feat: add interactive agent configuration page"`

Expected: Git 工作树中提交成功；无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## Task 5：实现删除 Helper 配置、风险确认和 UAC 状态

**Files:**

- Create: `nodetray/frontend/src/pages/HelperPage.tsx`
- Create: `nodetray/frontend/src/pages/HelperPage.test.tsx`
- Create: `nodetray/frontend/src/components/RootListField.tsx`
- Test: `nodetray/frontend/src/components/RootListField.test.tsx`
- Create: `nodetray/frontend/src/components/UACProgress.tsx`
- Test: `nodetray/frontend/src/components/UACProgress.test.tsx`

### Step 1：写路径白名单和硬删除失败测试

断言 `allowed_roots` 可用目录选择器添加/移除、不能为空、逐项显示规范化/系统目录/重叠错误；`denied_roots` 同样是列表控件。前端不把任何真实媒体路径写入测试快照，使用 `C:\fixtures\media-a` 等中性临时路径。

新配置 `allow_hard_delete` 默认关闭；加载已有 true 值时不静默关闭，显示持续高风险警告；保存 true 必须二次确认，取消时不调用 SaveHelper。

### Step 2：写 UAC 和生命周期失败测试

手动启动显示“将请求管理员权限”，调用 StartHelper 后进入 UAC 等待状态；`UACCancelled` 恢复原状态并显示非错误提示。自动模式显示计划任务状态，不提供任意任务名/账号/命令编辑框。

停止超时对话框提供返回、仅退出托盘和“强制结束已认领 Helper”；强制动作必须二次确认后单独调用 `ForceStopHelper`。后端再次核对 PID、创建时间和最终路径，本版本从不自动执行强制结束。

### Step 3：运行 RED

Run: `npm test -- --run 'HelperPage|RootListField|UACProgress'`

Workdir: `nodetray/frontend`

Expected: FAIL。

### Step 4：实现 Helper 页

按管道、路径、删除、协议、日志分组；页首为 Helper 组件级启动/停止/重启。Helper 禁用时保留已保存表单但禁用启动动作；保存受保护配置的 UAC 结果使用 `OperationResult` 呈现，前端不缓存提升 nonce 或配置 JSON。

### Step 5：运行 GREEN

Run: `npm test -- --run 'HelperPage|RootListField|UACProgress'`

Workdir: `nodetray/frontend`

Expected: PASS。

### Step 6：提交检查点

Run: `git add nodetray/frontend/src/pages/HelperPage.tsx nodetray/frontend/src/components && git commit -m "feat: add protected helper configuration page"`

Expected: Git 工作树中提交成功；无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## Task 6：实现程序设置、关闭/退出语义和未保存修改保护

**Files:**

- Create: `nodetray/frontend/src/pages/SettingsPage.tsx`
- Test: `nodetray/frontend/src/pages/SettingsPage.test.tsx`
- Create: `nodetray/frontend/src/components/ExitDialog.tsx`
- Test: `nodetray/frontend/src/components/ExitDialog.test.tsx`
- Modify: `nodetray/frontend/src/App.tsx`
- Modify: `nodetray/app.go`
- Test: `nodetray/app_test.go`

### Step 1：写设置矩阵失败测试

设置页包含：登录后启动托盘程序；Agent 自动/手动；Helper 启用；Helper 自动/手动；关闭窗口时隐藏到托盘；1–3 秒状态刷新间隔；“重要通知/全部通知”级别；打开 Agent/Helper 日志目录和配置备份目录。文案不得写成 Windows 服务或“无人登录也会运行”。Helper 禁用时自动/手动控件禁用且后端校验一致。打开目录只提交固定 `LocationKind` 枚举，不提交任意路径。

### Step 2：写关闭和退出失败测试

断言：

- `CloseToTray=true` 时窗口关闭事件只调用 HideWindow，不调用 `ExitTray`；
- `CloseToTray=false` 时窗口关闭事件显示同一个 ExitDialog，不静默退出；
- 表单 dirty 时隐藏仍保留内存草稿；
- 真正退出前 dirty 时提供返回、放弃修改并退出，不能静默丢失；
- ExitDialog 默认聚焦“仅退出托盘程序”，调用 `ExitTray(false)`；
- “停止组件后退出”调用 `ExitTray(true)`；
- 组件停止失败/超时时不自动关闭托盘，显示返回、仅退出托盘、强制结束已认领组件三个明确选择；强制项再次确认并分别调用 `ForceStopAgent`/`ForceStopHelper`。

### Step 3：运行 RED

Run: `npm test -- --run 'SettingsPage|ExitDialog|App'`

Workdir: `nodetray/frontend`

Expected: FAIL。

Run: `go test ./nodetray -run 'Test(Close|Exit)' -count=1`

Expected: FAIL。

### Step 4：实现设置和 native close adapter

Wails `OnBeforeClose` 或等价窗口关闭回调只发出 `window-close-requested`，由 Backend 隐藏窗口；系统关机会话使用独立路径，不阻塞 Windows 注销。只有托盘菜单“退出”或设置页退出按钮展示 ExitDialog。

### Step 5：运行 GREEN

Run: `npm test -- --run 'SettingsPage|ExitDialog|App'`

Workdir: `nodetray/frontend`

Expected: PASS。

Run: `go test ./nodetray -count=1`

Expected: PASS。

### Step 6：提交检查点

Run: `git add nodetray/app.go nodetray/app_test.go nodetray/frontend/src && git commit -m "feat: implement tray settings and exit semantics"`

Expected: Git 工作树中提交成功；无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## Task 7：实现 Windows 通知区域图标、菜单和原生通知

**Files:**

- Create: `internal/nodetray/windows/tray/tray_windows.go`
- Create: `internal/nodetray/windows/tray/tray_stub.go`
- Create: `internal/nodetray/windows/tray/menu.go`
- Create: `internal/nodetray/windows/tray/notification.go`
- Test: `internal/nodetray/windows/tray/menu_test.go`
- Test: `internal/nodetray/windows/tray/notification_test.go`
- Create: `nodetray/build/windows/icon.ico`
- Modify: `nodetray/main.go`

### Step 1：写纯菜单模型失败测试

接口：

```go
type Command string

const (
	ShowConsole       Command = "show-console"
	StartAgent        Command = "start-agent"
	RestartAgent      Command = "restart-agent"
	StopAgent         Command = "stop-agent"
	StartHelper       Command = "start-helper"
	StopHelper        Command = "stop-helper"
	OpenLogs          Command = "open-logs"
	OpenSettings      Command = "open-settings"
	ExitTray          Command = "exit-tray"
)

type Snapshot struct {
	Agent traymodel.ComponentState
	Helper traymodel.ComponentState
	HelperEnabled bool
}

func BuildMenu(snapshot Snapshot) []Item
```

测试断言菜单显示节点名和总体健康度、Agent 状态、Worker ready/expected、Helper 状态，并提供打开控制台、打开日志目录、程序设置和退出；冲突动作禁用；没有任何单 Worker 命令；Helper 手动启动项明确标注管理员权限。

### Step 2：写通知合并失败测试

通知只覆盖：启动失败、异常退出、Worker 长时间未齐、配置损坏/运行配置漂移、需要 UAC。相同 `component + code` 在 30 秒内合并；正文脱敏且不超过 Windows 安全长度；状态刷新不产生通知风暴。

### Step 3：运行 RED

Run: `go test ./internal/nodetray/windows/tray -count=1`

Expected: FAIL。

### Step 4：实现 Shell_NotifyIcon 生命周期

使用 `Shell_NotifyIconW` 添加/修改/删除图标、隐藏消息窗口接收点击和 TaskbarCreated；Explorer 重启后重新添加。双击或菜单“显示节点控制台”调用 Wails WindowShow + WindowUnminimise + WindowCenter/前台激活。图标资源内嵌，不从当前目录动态加载。

托盘线程崩溃不得停止组件；main 记录脱敏错误并保留 Wails 窗口，通知用户重启托盘。

### Step 5：运行 GREEN

Run: `go test ./internal/nodetray/windows/tray -count=1`

Expected: PASS。

Run: `go test ./nodetray ./internal/nodetray/windows/tray -count=1`

Expected: PASS。

### Step 6：提交检查点

Run: `git add internal/nodetray/windows/tray nodetray/build/windows/icon.ico nodetray/main.go && git commit -m "feat: add native node tray controls"`

Expected: Git 工作树中提交成功；无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## Task 8：加入前端安全、错误边界和完整静态门禁

**Files:**

- Create: `nodetray/frontend/src/components/AppErrorBoundary.tsx`
- Test: `nodetray/frontend/src/components/AppErrorBoundary.test.tsx`
- Create: `nodetray/frontend/src/security.test.ts`
- Modify: `nodetray/frontend/index.html`
- Modify: `nodetray/frontend/vite.config.ts`
- Modify: `nodetray/main.go`

### Step 1：写安全失败测试

测试扫描源码/构建产物，拒绝：`http://`、`https://` 远程资源、`eval`/`new Function`、source map、dev server 地址、localStorage/sessionStorage/IndexedDB、密码写入日志、HTML 注入。CSP 至少为：

```text
default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; frame-src 'none'; base-uri 'none'
```

### Step 2：写 WebView 错误路径失败测试

Runtime 缺失时在初始化 Wails 前显示原生中文提示和官方安装引导，不启动 Agent/Helper。初始化失败写脱敏日志后退出；前端 ErrorBoundary 只尝试重建窗口一次，第二次失败提示重启托盘，不触发组件退出。

### Step 3：运行 RED

Run: `npm test -- --run 'security|AppErrorBoundary'`

Workdir: `nodetray/frontend`

Expected: FAIL。

### Step 4：实现 CSP、生产 Vite 配置和错误边界

Vite 生产配置固定 `sourcemap: false`、不拆出远程动态资源、构建空 outDir；Wails 开发 server 仅在显式开发命令使用，发布脚本不能传 devtools 标志。

### Step 5：运行完整前端门禁

Run: `npm test`

Workdir: `nodetray/frontend`

Expected: PASS。

Run: `npm run lint`

Workdir: `nodetray/frontend`

Expected: PASS。

Run: `npm run build`

Workdir: `nodetray/frontend`

Expected: PASS，生成 `dist` 且无 `.map`。

Run: `rg -n "https?://|eval\(|new Function|localStorage|sessionStorage|indexedDB|sourceMappingURL" nodetray/frontend/dist nodetray/frontend/src`

Expected: 0 个未审查命中；CSP 文本命中需人工标记为允许。

### Step 6：提交检查点

Run: `git add nodetray/frontend nodetray/main.go && git commit -m "test: harden embedded tray frontend"`

Expected: Git 工作树中提交成功；无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## Task 9：纳入可复现构建、供应链清单和发布闭包

**Files:**

- Create: `scripts/build-nodetray.ps1`
- Create: `scripts/test-node-tray-supply-chain.ps1`
- Create: `third_party/webview2/manifest.schema.json`
- Create: `third_party/webview2/manifest.json`
- Modify: `scripts/build.ps1`
- Modify: `docs/deployment/node-tray-security.md`

### Step 1：先写构建脚本静态失败测试

供应链测试断言：

- Wails 命令固定 `github.com/wailsapp/wails/v2/cmd/wails@v2.12.0`；
- `package-lock.json` 存在并使用 `npm ci`；
- WebView2 Evergreen Bootstrapper 只允许来自预下载缓存，manifest 记录官方 URL、SHA-256、大小、获取时间和许可证路径；
- 生产参数包含 `-webview2 embed`，不含 devtools/sourcemap；
- `build/windows/nodetray.manifest` 为 `asInvoker`；
- staging 中必须有 `nodetray.exe`，没有前端源码、node_modules 或测试凭据。

Run: `pwsh -NoProfile -File scripts/test-node-tray-supply-chain.ps1`

Expected: FAIL，构建集成尚未实现。

### Step 2：实现独立构建脚本

参数：

```powershell
param(
    [string]$Go = "go",
    [string]$Npm = "npm",
    [string]$OutDir = "artifacts\stage",
    [string]$WebView2Bootstrapper = "third_party\webview2\MicrosoftEdgeWebview2Setup.exe",
    [switch]$SkipFrontendTests
)
```

执行顺序：验证工具版本 → `npm ci` → test/lint/build → 校验 Bootstrapper SHA → Wails generate → `go test ./nodetray ./internal/nodetray/...` → 只删除 `nodetray/build/bin` 和旧的生成 `.syso` → `go run ...@v2.12.0 build -webview2 embed -o nodetray.exe` → PE manifest/架构检查 → 复制到 fresh stage。Wails v2.12.0 的 `-clean` 会重建整个 build 目录，因此发布脚本禁止使用它，避免删除受版本控制的 `nodetray.manifest` 和 `icon.ico`。失败时不留下半成品正式 stage。

### Step 3：集成现有 `scripts/build.ps1`

增加显式 `[switch]$SkipNodeTrayBuild`，默认完整发布构建包含 nodetray；`-VideoCoreOnly`/`-MediacoreOnly` 不构建托盘。最终 required files 和 release manifest 增加：

```text
nodetray.exe
agent.exe
gui.exe
worker.exe
helper.exe
videocore.dll
agent.example.json
helper.example.json
MicrosoftEdgeWebview2Setup.exe
```

配置示例分别从现有 `deploy/agent.example.json`、`deploy/helper.example.json` 复制并进入 release manifest。保留现有 `gui.exe` 和 VideoCore 递归 FFmpeg DLL 闭包，不改变 fresh VideoCore stage 安全边界；托盘构建应接收已经验证的 stage 路径，而不是预先创建冲突目录。

### Step 4：运行供应链 GREEN

Run: `pwsh -NoProfile -File scripts/test-node-tray-supply-chain.ps1`

Expected: PASS。

Run: `pwsh -NoProfile -File scripts/build-nodetray.ps1 -OutDir C:\tmp\mysingerserver-nodetray-stage`

Expected: PASS，fresh stage 产生 `nodetray.exe` 和经 manifest 验证的 Bootstrapper；不触发 UAC、计划任务、注册表或组件启动。

### Step 5：验证 PE、版本和静态闭包

Run: `go version -m C:\tmp\mysingerserver-nodetray-stage\nodetray.exe`

Expected: 输出包含 Wails v2.12.0 和仓库 module，不含非预期替换。

Run: `pwsh -NoProfile -Command "Get-AuthenticodeSignature -LiteralPath 'C:\tmp\mysingerserver-nodetray-stage\nodetray.exe' | Select-Object Status,StatusMessage"`

Expected: 记录签名状态；未配置发布证书时不得虚假宣称已签名，发布门禁按项目签名政策决定阻断。

### Step 6：提交检查点

Run: `git add scripts/build-nodetray.ps1 scripts/test-node-tray-supply-chain.ps1 scripts/build.ps1 third_party/webview2 docs/deployment/node-tray-security.md && git commit -m "build: package pinned Wails node tray"`

Expected: Git 工作树中提交成功；无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## Task 10：更新中文部署/使用文档和验收记录模板

**Files:**

- Create: `docs/deployment/node-tray.md`
- Create: `docs/acceptance/node-tray-acceptance.md`
- Modify: `README.md`
- Modify: `docs/deployment/m5-helper.md`

### Step 1：先写文档合同检查

用 `rg` 断言文档必须包含：四页签、登录启动语义、Agent/Helper 自动/手动、Worker 只读、Helper UAC、固定计划任务、关闭隐藏、两种退出、配置备份恢复、停止超时、WebView2 缺失、卸载/删除不在范围、动态验收状态。

Run: `rg -n "总览|Agent|删除 Helper|程序设置|登录后启动|自动|手动|Worker.*只读|UAC|计划任务|仅退出托盘|停止组件后退出|last-good|WebView2|BLOCKED_NOT_RUN_DYNAMIC" docs/deployment/node-tray.md docs/acceptance/node-tray-acceptance.md README.md`

Expected: 在文档生成前缺少文件或命中不足而 FAIL。

### Step 2：编写可执行中文快速开始

README 入口先给操作员流程：启动托盘 → 导入/填写 Agent → 测试并保存 → 启动 Agent → 查看 Worker Ready → 可选配置 Helper。随后分别写管理员部署、开发构建、排障和安全边界。不得要求用户直接修改 JSON；兼容 CLI 配置只放迁移说明。

Helper 部署文档改为：Helper 是节点托盘可选组件；Agent 不自动启动 Helper；手动模式 UAC，自动模式固定最高权限登录任务；`allowed_roots` 必须窄范围；默认软删除并建议关闭硬删除。

### Step 3：编写验收记录模板

每个门禁状态只允许 `PASS`、`FAIL`、`BLOCKED_NOT_RUN_DYNAMIC`，记录命令、时间、机器、证据路径、是否涉及 UAC/计划任务/HKCU/进程和凭据扫描。模板不得预填 PASS。

### Step 4：运行文档合同和链接检查

Run: `rg -n "总览|Agent|删除 Helper|程序设置|登录后启动|自动|手动|Worker.*只读|UAC|计划任务|仅退出托盘|停止组件后退出|last-good|WebView2|BLOCKED_NOT_RUN_DYNAMIC" docs/deployment/node-tray.md docs/acceptance/node-tray-acceptance.md README.md`

Expected: 所有主题至少一个有效命中。

Run: `pwsh -NoProfile -Command "$files=@('docs/deployment/node-tray.md','docs/acceptance/node-tray-acceptance.md','docs/deployment/m5-helper.md'); foreach($f in $files){if(-not(Test-Path -LiteralPath $f)){throw \"missing $f\"}}"`

Expected: PASS。

### Step 5：提交检查点

Run: `git add README.md docs/deployment/node-tray.md docs/deployment/m5-helper.md docs/acceptance/node-tray-acceptance.md && git commit -m "docs: explain node tray deployment and acceptance"`

Expected: Git 工作树中提交成功；无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## Task 11：执行授权 Windows 动态验收和资源测量

**Files:**

- Create: `tests/windows/Test-NodeTray.ps1`
- Create: `tests/windows/Measure-NodeTrayResources.ps1`
- Modify: `docs/acceptance/node-tray-acceptance.md`

### Step 1：实现无副作用预检模式

`Test-NodeTray.ps1 -WhatIf` 检查：Wails/WebView2 产物、临时配置根、当前用户、固定任务路径是否可测试、中央 TCP 测试端口是否可用。不得触发 UAC、写 Run 键、创建任务或启动组件。

Run: `pwsh -NoProfile -File tests/windows/Test-NodeTray.ps1 -WhatIf -StageDir C:\tmp\mysingerserver-nodetray-stage`

Expected: PASS 或明确列出授权缺口。

### Step 2：实现显式授权矩阵

脚本必须分别要求 `-AllowProcessControl`、`-AllowUAC`、`-AllowTaskScheduler`、`-AllowHKCUStartup`。只在开关存在时运行对应场景：

1. 单实例与第二实例唤醒；
2. Agent 启动、Worker Ready、停止、重启、托盘重启后认领；
3. 非目标同名进程不被认领或结束；
4. Helper 手动 UAC 取消与同意；
5. 固定计划任务安装、定义校验、运行、停止、删除；
6. 登录启动启用/禁用和路径漂移；
7. 关闭窗口隐藏；默认退出后组件保持运行；停止组件后退出；
8. 停止超时不自动强杀；
9. WebView2 缺失/初始化失败的隔离模拟；
10. 配置 ACL/备份 ACL、通知/日志/证据凭据扫描。

测试只使用 `C:\tmp\mysingerserver-node-tray-<guid>` 中的中性语料和配置；不得使用或清理任何真实媒体目录。脚本清理前解析绝对路径并确认仍位于自己的 GUID 根。

### Step 3：实现资源测量

`Measure-NodeTrayResources.ps1` 在窗口隐藏并稳定 2 分钟后采样 5 分钟，报告 `nodetray.exe` 和 WebView2 子进程总 Private Working Set、平均/峰值 CPU、句柄数；同时记录 Agent/Worker 基准任务的吞吐差异。目标：隐藏状态总私有工作集不超过 256 MiB，稳定 5 分钟平均 CPU 低于单核的 1%，媒体处理延迟无明显回归。若超标，状态为 FAIL 并保存原始样本，不用平均值掩盖峰值。

### Step 4：先运行全部静态门禁

Run: `go test ./internal/nodectl ./internal/agentcontrol ./internal/helpercontrol ./internal/nodetray/... ./nodetray -count=1`

Expected: PASS。

Run: `npm test`

Workdir: `nodetray/frontend`

Expected: PASS。

Run: `npm run lint`

Workdir: `nodetray/frontend`

Expected: PASS。

Run: `npm run build`

Workdir: `nodetray/frontend`

Expected: PASS。

Run: `pwsh -NoProfile -File scripts/test-node-tray-supply-chain.ps1`

Expected: PASS。

### Step 5：在明确授权的隔离 Windows 会话运行动态验收

Run: `pwsh -NoProfile -File tests/windows/Test-NodeTray.ps1 -StageDir C:\tmp\mysingerserver-nodetray-stage -TestRoot C:\tmp\mysingerserver-node-tray-acceptance -AllowProcessControl -AllowUAC -AllowTaskScheduler -AllowHKCUStartup`

Expected: 所有授权场景 PASS 并输出脱敏 JSON。缺少任一权限或交互桌面时，对应项目写 `BLOCKED_NOT_RUN_DYNAMIC`，静态门禁仍单独记录。

Run: `pwsh -NoProfile -File tests/windows/Measure-NodeTrayResources.ps1 -NodeTrayExe C:\tmp\mysingerserver-nodetray-stage\nodetray.exe -DurationSec 300 -WarmupSec 120 -OutFile C:\tmp\mysingerserver-node-tray-acceptance\resources.json`

Expected: 生成资源 JSON，并按设计目标判定 PASS/FAIL。

### Step 6：完成证据审计

Run: `rg -n "postgres(ql)?://|password\s*[:=]|token\s*[:=]|secret\s*[:=]" C:\tmp\mysingerserver-node-tray-acceptance docs/acceptance/node-tray-acceptance.md`

Expected: 0 个真实凭据命中；测试占位秘密也只能以哈希/`[REDACTED]` 出现在证据。

### Step 7：最终提交检查点

Run: `git add tests/windows docs/acceptance/node-tray-acceptance.md && git commit -m "test: verify node tray user experience"`

Expected: Git 工作树中提交成功；无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## 完成定义

- 四页签、键盘导航、窄窗口布局和统一视觉令牌通过前端测试。
- Agent/Helper 全部支持参数均可交互式修改，密码/硬删除/路径列表遵守安全设计。
- Worker 状态可见，但源码、DOM、菜单和绑定中没有直接 Worker 管理动作。
- 托盘菜单、关闭隐藏、单实例唤醒、两种退出行为与停止超时语义已验证。
- Wails v2.12.0、Node 依赖、WebView2 Bootstrapper 和前端 lockfile 进入供应链门禁。
- `nodetray.exe` 为 `asInvoker`，生产包无 dev server、source map、远程资源或测试凭据。
- 中文快速开始、管理员部署、排障、安全与验收文档完整。
- Windows/UAC/计划任务/HKCU/资源动态验收已 PASS；不能运行的项目明确为 `BLOCKED_NOT_RUN_DYNAMIC`。
