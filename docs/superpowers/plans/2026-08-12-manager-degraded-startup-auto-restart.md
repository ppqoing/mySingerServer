# Manager 降级启动与保存后自动重启实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Manager 在首次无配置、PostgreSQL 不可用或 Agent 离线时仍能打开配置界面，并在保存配置后自动重启到新配置。

**Architecture:** 把始终可用的 HTTP 外壳与依赖 PostgreSQL 的业务运行时分离：先创建或读取配置、绑定监听器并启动外壳，再异步构建业务运行时。配置保存后由后端先启动一个等待父进程退出的同路径 `gui.exe`，响应前端后优雅关闭父进程；前端轮询新地址的健康端点并恢复页面。

**Tech Stack:** Go 1.26、`net/http`、pgx v5、Windows `OpenProcess/WaitForSingleObject`、React 19、TypeScript、Vitest、PowerShell 7、VS2022/MSVC 与已安装 MinGW。

## Global Constraints

- 除 `listen_addr` 无效或监听端口绑定失败外，PostgreSQL 和 Agent 错误不得阻止管理界面启动。
- `gui.json` 不存在时自动创建；已存在但损坏或字段非法时保持启动失败，不做修复或覆盖。
- 首次默认监听地址固定为 `127.0.0.1:18081`。
- 保存后的配置通过自动重启生效，不实现进程内热切换。
- 自动重启必须复用当前 `gui.exe` 的最终文件路径、同一绝对配置路径和 `-no-browser` 语义。
- 密码、完整 PostgreSQL DSN 和敏感路径不得进入 HTTP 错误、界面状态或日志。
- Manager 发布物仍是便携 ZIP，不生成安装包；ZIP 内保留模板，不预生成用户的 `gui.json`。
- 所有行为修改严格执行 RED → GREEN；每一项完成声明前运行新鲜验证。

---

### Task 1: 首次运行创建完整默认配置

**Files:**
- Modify: `internal/config/gui.go`
- Modify: `internal/config/config_test.go`
- Create: `internal/gui/config_init.go`
- Create: `internal/gui/config_init_test.go`
- Create: `internal/gui/config_init_lock_windows.go`
- Create: `internal/gui/config_init_lock_other.go`
- Modify: `internal/gui/config_service.go`

**Interfaces:**
- Produces: `config.DefaultGUI() *config.GUIConfig`，返回可直接持久化并通过 `ValidateGUI` 的完整默认配置。
- Produces: `gui.LoadOrCreateGUIConfig(path string) (*config.GUIConfig, error)`，只在目标不存在时发布默认配置，目标存在时严格读取。
- Consumes: 现有 `canonicalGUIConfig` 与 `replaceFileAtomically`，保持 UTF-8 无 BOM、规范 JSON和原子发布。

- [ ] **Step 1: 写默认值和首次创建的失败测试**

在 `internal/config/config_test.go` 添加：

```go
func TestDefaultGUIIsACompletePortableFirstRunConfiguration(t *testing.T) {
	cfg := DefaultGUI()
	if err := ValidateGUI(cfg); err != nil {
		t.Fatalf("DefaultGUI: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:18081" ||
		cfg.PGDSN != "postgres://dedup@127.0.0.1:5432/dedup" ||
		len(cfg.Agents) != 1 || cfg.Agents[0].Addr != "127.0.0.1:9101" {
		t.Fatalf("incomplete portable defaults: %#v", cfg)
	}
}
```

在 `internal/gui/config_init_test.go` 添加三个用例：不存在时创建并可由 `config.LoadGUI` 读取；已存在时不改写；32 个并发调用最终得到同一份完整 JSON且目录中没有临时文件。

- [ ] **Step 2: 运行测试并确认 RED**

Run:

```powershell
go test -count=1 ./internal/config ./internal/gui -run 'DefaultGUIIsACompletePortable|LoadOrCreateGUIConfig'
```

Expected: FAIL，原因分别为默认配置缺少 DSN/Agent、默认端口仍为 8080，以及 `LoadOrCreateGUIConfig` 尚不存在。

- [ ] **Step 3: 实现默认配置和并发安全的首次发布**

将 `DefaultGUI` 的顶层默认值补全：

```go
func DefaultGUI() *GUIConfig {
	return &GUIConfig{
		ListenAddr: "127.0.0.1:18081",
		PGDSN:      "postgres://dedup@127.0.0.1:5432/dedup",
		Agents:     []AgentEndpoint{{Addr: "127.0.0.1:9101"}},
		HeartbeatS: 15,
		FirstScreen: defaultFirstScreen(),
		Phase2:      defaultPhase2(),
	}
}
```

`LoadOrCreateGUIConfig` 先尝试 `config.LoadGUI`；仅对 `os.ErrNotExist` 进入创建路径。在 Windows 上用基于配置绝对路径 SHA-256 的命名互斥量串行化多个启动进程，在其他平台用同目录锁文件实现等价边界。持锁后再次严格加载目标；仍不存在才调用从 `GUIConfigService` 抽出的 `writeCanonicalGUIConfig`，通过同目录临时文件、`Sync`、验证和 `replaceFileAtomically` 发布。禁止使用 `WriteFile` 直接覆盖最终路径。同步更新 `config_test.go` 中旧的 8080 默认值断言为 18081。

- [ ] **Step 4: 运行聚焦测试并确认 GREEN**

Run:

```powershell
go test -count=1 ./internal/config ./internal/gui -run 'DefaultGUIIsACompletePortable|LoadOrCreateGUIConfig|GUIConfigService'
```

Expected: PASS。

- [ ] **Step 5: 提交 Task 1**

```powershell
git add -- internal/config/gui.go internal/config/config_test.go internal/gui/config_init.go internal/gui/config_init_test.go internal/gui/config_init_lock_windows.go internal/gui/config_init_lock_other.go internal/gui/config_service.go
git commit -m "feat: create manager first-run config"
```

---

### Task 2: 建立不依赖 PostgreSQL 的 HTTP 外壳与运行状态

**Files:**
- Create: `internal/gui/runtime_host.go`
- Create: `internal/gui/runtime_host_test.go`
- Create: `internal/gui/runtime_status.go`
- Create: `internal/gui/runtime_status_test.go`
- Modify: `internal/gui/httpapi.go`
- Modify: `internal/gui/config_http.go`

**Interfaces:**
- Produces: `type RuntimeStatus`，JSON 字段为 `database_state`、`database_error_code`、`agents`、`restarting` 和 `recovery_url`。
- Produces: `type RuntimeHost`，实现 `http.Handler`、`BeginAnalysisShutdown()` 和 `WaitForAnalysis()`。
- Produces: `NewRuntimeHost(config guiConfigStore, configuredAgents []config.AgentEndpoint) *RuntimeHost`。
- Produces: `Install(*API)`、`SetDatabaseConnecting()`、`SetDatabaseFailure(error)` 与 `SetRestartState(restarting bool, recoveryURL string)`。
- Produces: `ClassifyRuntimeFailure(error) RuntimeFailure`，只返回稳定码与固定中文摘要。
- Consumes: 已安装完整 `API.Routes()`；未安装时只暴露静态页面、配置、状态、健康端点和离线 Agent 快照。

- [ ] **Step 1: 写降级路由失败测试**

在 `runtime_host_test.go` 用 `httptest` 证明：

```go
func TestRuntimeHostServesConfigurationWhileDatabaseIsUnavailable(t *testing.T) {
	host := NewRuntimeHost(&fakeGUIConfigStore{loadSnapshot: GUIConfigSnapshot{
		Config: testGUIConfig(),
	}}, []config.AgentEndpoint{{Addr: "127.0.0.1:9101"}})
	host.SetDatabaseFailure(&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED})

	assertStatus(t, host, http.MethodGet, "/", http.StatusOK)
	assertStatus(t, host, http.MethodGet, "/api/config", http.StatusOK)
	assertStatus(t, host, http.MethodGet, "/api/runtime/status", http.StatusOK)
	assertStatus(t, host, http.MethodGet, "/api/tasks", http.StatusServiceUnavailable)
}
```

同时断言 503 body 精确为 `{"error":"database_unavailable"}`，`/api/agents` 返回已配置地址且 `online=false`，`/api/restart/health` 返回 200 并带 `Access-Control-Allow-Origin: *`。

- [ ] **Step 2: 写安全错误分类失败测试**

在 `runtime_status_test.go` 使用结构化认证错误和拨号错误，只允许得到稳定码与固定摘要：

```go
tests := []struct {
	err  error
	want string
}{
	{err: &pgconn.PgError{Code: "28P01", Message: "password authentication failed"}, want: "postgres_auth_failed"},
	{err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, want: "postgres_unreachable"},
}
for _, test := range tests {
	status := ClassifyRuntimeFailure(test.err)
	if status.Code != test.want || strings.Contains(status.Summary, "password") {
		t.Fatalf("unsafe status: %#v", status)
	}
}
```

- [ ] **Step 3: 运行测试并确认 RED**

Run:

```powershell
go test -count=1 ./internal/gui -run 'RuntimeHost|RuntimeFailure'
```

Expected: FAIL，`RuntimeHost`、状态模型和稳定 503 尚不存在。

- [ ] **Step 4: 实现外壳、动态委派和状态快照**

`RuntimeHost.ServeHTTP` 的路由顺序必须固定：

```go
switch {
case request.URL.Path == "/api/config":
	h.configAPI.ServeHTTP(response, request)
case request.URL.Path == "/api/runtime/status":
	h.handleRuntimeStatus(response, request)
case request.URL.Path == "/api/restart/health":
	h.handleRestartHealth(response, request)
case request.URL.Path == "/api/agents" && h.current() == nil:
	h.handleOfflineAgents(response, request)
case strings.HasPrefix(request.URL.Path, "/api/") && h.current() == nil:
	writeJSON(response, http.StatusServiceUnavailable, map[string]string{"error": "database_unavailable"})
case h.current() != nil:
	h.current().Routes().ServeHTTP(response, request)
default:
	h.static.ServeHTTP(response, request)
}
```

动态运行时和状态用 `sync.RWMutex` 保护；状态只保存稳定码与固定中文摘要，不保存原始 `error` 或 DSN。`Install` 原子替换委派目标；关闭方法在没有完整运行时时必须安全返回。

- [ ] **Step 5: 运行聚焦回归并确认 GREEN**

Run:

```powershell
go test -count=1 ./internal/gui -run 'RuntimeHost|RuntimeFailure|GUIConfigHTTP|EmbeddedReact'
```

Expected: PASS。

- [ ] **Step 6: 提交 Task 2**

```powershell
git add -- internal/gui/runtime_host.go internal/gui/runtime_host_test.go internal/gui/runtime_status.go internal/gui/runtime_status_test.go internal/gui/httpapi.go internal/gui/config_http.go
git commit -m "feat: serve manager shell without database"
```

---

### Task 3: 调整启动顺序并异步构建业务运行时

**Files:**
- Create: `cmd/gui/operational_runtime.go`
- Create: `cmd/gui/operational_runtime_test.go`
- Modify: `cmd/gui/main.go`
- Modify: `cmd/gui/main_test.go`

**Interfaces:**
- Produces: `buildOperationalRuntime(ctx context.Context, cfg *config.GUIConfig, logger *slog.Logger) (*operationalRuntime, error)`。
- Produces: `operationalRuntime.API() *gui.API` 与幂等 `Close()`。
- Produces: `serveBoundGUI(...)`，接收已经成功绑定的 `net.Listener`，不再在数据库初始化之后绑定。
- Consumes: Task 1 的 `LoadOrCreateGUIConfig` 和 Task 2 的 `RuntimeHost`。

- [ ] **Step 1: 把当前失败行为改写成期望行为测试**

替换 `TestGUIPingFailureIsLoggedBeforeInteractiveNotification`：注入一个必然拒绝连接的 DSN、假的 `guiListen` 和可取消服务器，断言事件顺序为：

```go
want := []string{"listen", "serve", "postgres-error"}
```

并断言 `executeGUI` 不触发启动失败弹窗。新增 `TestGUIBindsAndServesBeforeOperationalRuntimeRestore`，用阻塞的运行时工厂证明 HTTP 已经 Serve 后，数据库恢复才继续。

- [ ] **Step 2: 运行测试并确认 RED**

Run:

```powershell
go test -count=1 ./cmd/gui -run 'PingFailure|BindsAndServesBeforeOperationalRuntime|MissingConfiguration'
```

Expected: FAIL，现有 `run` 仍在 `guiListen` 前执行 `pg.Ping`，且配置缺失直接退出。

- [ ] **Step 3: 抽取完整业务运行时**

把 `pgxpool.New`、Ping、TaskRegistry 恢复、Phase2 恢复、DeleteService、Agent Pool、AnalysisHandlers 和回调装配移动到 `operational_runtime.go`。任何中间步骤失败都关闭已经创建的资源；成功后由 `RuntimeHost.Install(runtime.API())` 一次性发布。

`run` 的核心顺序改为：

```go
cfg, err := gui.LoadOrCreateGUIConfig(runtimePaths.ConfigPath)
configService, err := gui.NewGUIConfigService(runtimePaths.ConfigPath, cfg)
host := gui.NewRuntimeHost(configService, cfg.Agents)
listener, err := guiListen("tcp", cfg.ListenAddr)
// 绑定成功后立即 Serve；浏览器也只在这里打开。
go initializeOperationalRuntime(processContext, cfg, host, logger)
return serveBoundGUI(processContext, cancelProcess, server, listener, host, 5*time.Second)
```

后台初始化失败时调用 `host.SetDatabaseFailure(err)`，日志只记录 `RuntimeFailure.Code` 和固定摘要，不记录原始错误或连接字符串，也不调用 `cancelProcess`。

- [ ] **Step 4: 运行启动链路测试并确认 GREEN**

Run:

```powershell
go test -count=1 ./cmd/gui ./internal/gui -run 'GUI|OperationalRuntime|RuntimeHost'
```

Expected: PASS；测试不得真实访问 PostgreSQL。

- [ ] **Step 5: 提交 Task 3**

```powershell
git add -- cmd/gui/main.go cmd/gui/main_test.go cmd/gui/operational_runtime.go cmd/gui/operational_runtime_test.go
git commit -m "feat: start manager before external services"
```

---

### Task 4: 保存响应后自动重启同一个 GUI 可执行文件

**Files:**
- Create: `cmd/gui/restart.go`
- Create: `cmd/gui/restart_test.go`
- Modify: `cmd/gui/platform_windows.go`
- Modify: `cmd/gui/platform_windows_test.go`
- Modify: `cmd/gui/platform_other.go`
- Modify: `cmd/gui/main.go`
- Modify: `internal/gui/config_service.go`
- Modify: `internal/gui/config_http.go`
- Modify: `internal/gui/config_http_test.go`

**Interfaces:**
- Extends: `GUIConfigSaveResult`，增加 `Restarting bool` (`restarting`) 与 `RecoveryURL string` (`recovery_url`)。
- Produces: `type guiRestartCoordinator interface { Pending() bool; Prepare(*config.GUIConfig) (string, error); Commit() }`。
- Produces: `newGUIRestartCoordinator(executable, configPath string, parentPID int, cancel context.CancelFunc)`。
- Produces: 内部 CLI 参数 `-wait-parent-pid <pid>`；`guiWaitForParent(pid int) error`；`guiStartReplacement(exe string, args []string) error`。

- [ ] **Step 1: 写后端重启协议失败测试**

在 `config_http_test.go` 新增：配置有变化时先保存、调用 `Prepare`、写出并 Flush 包含 `restarting=true` 和 `recovery_url` 的 200 响应，最后才调用 `Commit`。用记录事件的 ResponseWriter 和 fake coordinator 断言：

```go
want := []string{"save", "prepare", "write", "flush", "commit"}
```

另测重启进行中返回 HTTP 409 `restart_in_progress`；启动替代进程失败返回 `restart_launch_failed` 且响应标记 `saved=true`。

- [ ] **Step 2: 写 Windows 父进程等待和命令行传播失败测试**

在 `restart_test.go` 与 `platform_windows_test.go` 注入启动函数，断言参数精确包含：

```text
-config <绝对路径> -no-browser -wait-parent-pid <当前 PID>
```

并用可控等待器证明 `-wait-parent-pid` 完成前不读取配置、不绑定端口。显式未使用 `-no-browser` 的父进程，自动重启仍传递 `-no-browser`，避免重复弹出浏览器。

- [ ] **Step 3: 运行测试并确认 RED**

Run:

```powershell
go test -count=1 ./cmd/gui ./internal/gui -run 'Restart|WaitForParent|ConfigHTTPPut.*Restart'
```

Expected: FAIL，重启协调器、响应字段和内部参数尚不存在。

- [ ] **Step 4: 实现条件等待与重启握手**

Windows 实现使用：

```go
handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
defer windows.CloseHandle(handle)
_, err = windows.WaitForSingleObject(handle, windows.INFINITE)
```

替代进程通过当前最终 `gui.exe` 绝对路径启动并立即 `Release` 句柄；不得从 `PATH` 查找。`Prepare` 使用原子状态拒绝重复重启，先启动等待子进程，成功后返回由新 `listen_addr` 计算的 URL。重启子进程始终增加 `-no-browser`，因为当前浏览器页面负责恢复；原进程显式使用 `-no-browser` 时其语义也得到保留。HTTP handler 写入并 Flush 响应后调用 `Commit` 取消父进程上下文。保存成功但替代进程启动失败时不回滚 `gui.json`。

- [ ] **Step 5: 运行聚焦测试并确认 GREEN**

Run:

```powershell
go test -count=1 ./cmd/gui ./internal/gui -run 'Restart|WaitForParent|ConfigHTTP'
```

Expected: PASS。

- [ ] **Step 6: 提交 Task 4**

```powershell
git add -- cmd/gui/restart.go cmd/gui/restart_test.go cmd/gui/platform_windows.go cmd/gui/platform_windows_test.go cmd/gui/platform_other.go cmd/gui/main.go internal/gui/config_service.go internal/gui/config_http.go internal/gui/config_http_test.go
git commit -m "feat: restart manager after config save"
```

---

### Task 5: 在界面显示降级状态并等待自动重启

**Files:**
- Modify: `webui/src/api/contracts.ts`
- Modify: `webui/src/api/appApi.ts`
- Modify: `webui/src/api/appApi.test.ts`
- Modify: `webui/src/features/settings/GUISettingsPage.tsx`
- Modify: `webui/src/features/settings/GUISettingsPage.test.tsx`
- Modify: `webui/src/features/overview/OverviewPage.tsx`
- Create: `webui/src/features/overview/OverviewPage.test.tsx`
- Modify: `webui/src/features/settings/GUISettingsPage.css`
- Regenerate: `internal/gui/web/index.html`
- Regenerate: `internal/gui/web/groups.html`
- Regenerate: `internal/gui/web/assets/*`

**Interfaces:**
- Extends: `GUIConfigSaveResult`，加入 `restarting` 与 `recoveryURL`。
- Produces: `RuntimeStatus` 和 `AppApi.getRuntimeStatus(signal?)`。
- Produces: `waitForManager(recoveryURL, signal)`，轮询 `${origin}/api/restart/health`，成功后返回。

- [ ] **Step 1: 写 API 合同失败测试**

在 `appApi.test.ts` 断言以下响应能正确解码，字段缺失会失败：

```json
{
  "saved": true,
  "restart_required": true,
  "restarting": true,
  "recovery_url": "http://127.0.0.1:28081/"
}
```

同时为 `/api/runtime/status` 添加 `connecting`、`connected`、`error` 三态解析测试。

- [ ] **Step 2: 写设置页和总览页失败测试**

设置页测试保存后立即显示“配置已保存，Manager 正在自动重启”，禁用保存/重新加载按钮，轮询新 URL；健康检查成功后调用注入的 `navigate("http://127.0.0.1:28081/#/settings")`。超时显示“重启后监听失败，请检查 data\\logs\\gui.log”。

总览页测试数据库不可用时显示“PostgreSQL 未连接”和“打开 GUI 设置”链接，不能只显示通用请求错误。

- [ ] **Step 3: 运行测试并确认 RED**

Run:

```powershell
Set-Location webui
npm test -- src/api/appApi.test.ts src/features/settings/GUISettingsPage.test.tsx src/features/overview/OverviewPage.test.tsx
```

Expected: FAIL，新的响应字段、状态 API 和重启等待 UI 尚不存在。

- [ ] **Step 4: 实现类型、轮询和页面状态**

`waitForManager` 每 250 ms 请求无凭据健康端点，总时限 30 秒，使用 `AbortSignal` 停止。跨端口健康端点依赖 Task 2 的 `Access-Control-Allow-Origin: *`。设置页仅在 `result.restarting` 时进入重启状态；语义相同且 `saved=false` 时仍显示“配置未变化”。

总览先轮询 `getRuntimeStatus`；数据库未连接时停止扫描、任务和分析轮询，避免连续 503，并提供 React Router 链接到 `/settings`。

- [ ] **Step 5: 运行 Web 聚焦测试并确认 GREEN**

Run:

```powershell
Set-Location webui
npm test -- src/api/appApi.test.ts src/features/settings/GUISettingsPage.test.tsx src/features/overview/OverviewPage.test.tsx
npm run lint
npm run build
```

Expected: 全部 PASS，无 TypeScript 或 ESLint 错误。

- [ ] **Step 6: 提交 Task 5**

```powershell
git add -- webui/src/api/contracts.ts webui/src/api/appApi.ts webui/src/api/appApi.test.ts webui/src/features/settings/GUISettingsPage.tsx webui/src/features/settings/GUISettingsPage.test.tsx webui/src/features/settings/GUISettingsPage.css webui/src/features/overview/OverviewPage.tsx webui/src/features/overview/OverviewPage.test.tsx internal/gui/web
git commit -m "feat: show manager recovery and restart state"
```

---

### Task 6: 更新便携包入口、模板和发布合同

**Files:**
- Modify: `deploy/gui.example.json`
- Modify: `deploy/Start-Manager.ps1`
- Modify: `deploy/README-管理端部署.md`
- Modify: `scripts/test-package-manager-release.ps1`
- Modify: `scripts/test-node-tray-supply-chain.ps1`

**Interfaces:**
- Consumes: 缺失 `-config` 目标会由 `gui.exe` 自动创建的语义。
- Preserves: Manager ZIP 只含 `gui.exe`、`gui.example.json`、`Start-Manager.ps1`、中文说明和 `release-manifest.json`。

- [ ] **Step 1: 写发布合同失败断言**

在 `test-package-manager-release.ps1` 增加静态合同：模板 `listen_addr` 必须为 `127.0.0.1:18081`；启动脚本不得因 `gui.json` 缺失而 `throw`；README 必须说明直接双击时自动创建配置、外部依赖不可用仍可进入设置、保存后自动重启。

- [ ] **Step 2: 运行合同并确认 RED**

Run:

```powershell
pwsh -NoProfile -File scripts/test-package-manager-release.ps1
pwsh -NoProfile -File scripts/test-node-tray-supply-chain.ps1
```

Expected: FAIL，旧脚本仍要求手工复制模板，模板仍使用 8080。

- [ ] **Step 3: 最小更新发布文件**

`Start-Manager.ps1` 保留绝对同目录调用，但删除配置存在性检查：

```powershell
$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
& (Join-Path $root 'gui.exe') -config (Join-Path $root 'gui.json') @args
exit $LASTEXITCODE
```

模板端口改为 18081；中文说明改为“模板仅供参考，首次双击自动生成 `gui.json`”，同时明确 PostgreSQL/Agent 未连接不会阻止设置页。

- [ ] **Step 4: 运行发布合同并确认 GREEN**

Run:

```powershell
pwsh -NoProfile -File scripts/test-package-manager-release.ps1
pwsh -NoProfile -File scripts/test-node-tray-supply-chain.ps1
```

Expected: PASS。

- [ ] **Step 5: 提交 Task 6**

```powershell
git add -- deploy/gui.example.json deploy/Start-Manager.ps1 deploy/README-管理端部署.md scripts/test-package-manager-release.ps1 scripts/test-node-tray-supply-chain.ps1
git commit -m "docs: update manager portable first run"
```

---

### Task 7: 全量验证、真实便携运行验收与重新发布

**Files:**
- No tracked source changes expected; Task 5 has already committed regenerated embedded Web assets.
- Generated outside Git: `artifacts/stage-manager-restart-20260812/`
- Generated outside Git: `artifacts/releases/MySingerServer-manager-win-x64-portable-20260812-r2.zip`
- Generated outside Git: matching `.zip.sha256`

**Interfaces:**
- Consumes: 前六项全部实现。
- Produces: 新 Manager 便携 ZIP、SHA-256 sidecar 和真实首次启动/自动重启证据。

- [ ] **Step 1: 运行格式、Go 测试、竞态敏感串行回归和 vet**

Run:

```powershell
gofmt -w cmd/gui internal/config internal/gui
git diff --check
go test -p=1 -count=1 ./cmd/gui ./internal/config ./internal/gui
go vet ./cmd/gui ./internal/config ./internal/gui
```

Expected: 全部退出 0。

- [ ] **Step 2: 运行完整 Web 门禁**

Run:

```powershell
Set-Location webui
npm test
npm run lint
npm run build
Set-Location ..
```

Expected: 全部退出 0，生成的 Web 资源已刷新并嵌入 Go。

- [ ] **Step 3: 运行 Windows 供应链和发布合同**

Run:

```powershell
pwsh -NoProfile -File scripts/test-package-manager-release.ps1
pwsh -NoProfile -File scripts/test-node-tray-supply-chain.ps1
```

Expected: PASS。

- [ ] **Step 4: 使用本机 VS2022/MSVC 与系统 MinGW 创建全新 stage**

先从 VS2022 Developer PowerShell 运行：

```powershell
pwsh -NoProfile -File scripts/build.ps1 `
  -CC gcc `
  -Windres windres `
  -Dlltool dlltool `
  -StageDir artifacts/stage-manager-restart-20260812
```

Expected: 构建脚本使用 VS2022/MSVC 完成 VideoCore，使用系统 MinGW 完成 Windows Go/资源步骤，且全新 stage 中存在 `gui.exe`。

- [ ] **Step 5: 打包新的 Manager 便携 ZIP**

Run:

```powershell
$revision = git rev-parse HEAD
pwsh -NoProfile -File scripts/package-manager-release.ps1 `
  -StageDir artifacts/stage-manager-restart-20260812 `
  -OutputDir artifacts/releases `
  -ReleaseId portable-20260812-r2 `
  -BuildDate 2026-08-12 `
  -SourceRevision $revision
```

Expected: `MANAGER RELEASE PACKAGE PASS`，ZIP 与 sidecar 同时生成。

- [ ] **Step 6: 在无配置、无可用默认 PostgreSQL 的隔离目录做真实启动验收**

解压到新的临时目录，确认没有 `gui.json` 后启动 `gui.exe -no-browser`。验收：

```powershell
# 断言 127.0.0.1:18081 可访问
# 断言 gui.json 已自动生成且可解析
# 断言 GET /api/config = 200
# 断言 GET /api/runtime/status 的 database_state = error
# 断言 GET /api/tasks = 503 且 error = database_unavailable
```

随后通过 `PUT /api/config` 只把 `listen_addr` 改为一个经检查空闲的端口，断言旧进程退出、新 PID 出现、新端口健康端点恢复、旧端口释放，并且新进程在 PostgreSQL 仍不可用时继续提供配置界面。验收脚本只终止它自己启动并记录 PID 的进程。

- [ ] **Step 7: 验证发布内容和哈希**

Run:

```powershell
$zip = 'artifacts/releases/MySingerServer-manager-win-x64-portable-20260812-r2.zip'
(Get-FileHash -LiteralPath $zip -Algorithm SHA256).Hash
Get-Content -LiteralPath "$zip.sha256"
```

解压后必须恰好包含 `gui.exe`、`gui.example.json`、`Start-Manager.ps1`、`README-管理端部署.md`、`release-manifest.json`，不得包含 `gui.json`、数据库文件或 Compute 组件。

- [ ] **Step 8: 最终状态检查**

```powershell
git diff --check
git status --short
```

Expected: 除既有 `.codex-temp/` 和已声明的发布产物外没有新的工作树变化。不得暂存 `.codex-temp/`、stage、解压验收目录或其他用户文件；若格式化意外产生跟踪差异，先返回对应 Task 修复并重新运行门禁，不在 Task 7 临时追加实现提交。
