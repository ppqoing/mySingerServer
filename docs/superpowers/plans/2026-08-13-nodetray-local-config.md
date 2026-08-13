# NodeTray 本地配置管理 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** NodeTray 直接读写 Agent、Helper 和自身的便携配置；保存不触发 UAC，只有启动 Helper 时通过现有 `runas` 启动器提权。

**Architecture:** `internal/nodetray/config.Store` 成为 Agent/Helper 配置的唯一生产读写边界，复用现有锁、原子替换、回读校验和 `.last-good`。Agent Socket Controller 继续负责状态、任务和生命周期，并只接收本地保存后的待切换监听端点；Helper 的手动与自动启动都走现有 `ManualHelperLauncher`。

**Tech Stack:** Go 1.23+、Wails v2、PowerShell 7、Windows ShellExecute `runas`、JSON、现有 `securefile` 原子写入。

## Global Constraints

- Agent、Helper 和 NodeTray 配置读取、验证、保存不得依赖 Agent/Helper Socket。
- 保存任何配置不得调用 UAC、提权写入器或计划任务安装。
- 只有启动 Helper 时允许出现 UAC；UAC 取消只使本次启动失败。
- Compute ZIP 必须直接包含 `data/agent/agent.json`、`data/helper/helper.json`、`data/nodetray/tray.json`。
- Helper 默认 `allowed_roots` 为空时允许 NodeTray 读取和编辑，但启动前必须拒绝无效配置。
- 旧 Socket 配置操作和旧提权写配置动作保留兼容，不在本次删除。
- 只运行受影响的配置、应用服务、启动接线和 Compute 打包合同测试。

---

### Task 1: 让配置 Store 直接保存 Helper 并读取可编辑默认配置

**Files:**
- Modify: `internal/nodetray/config/store.go`
- Modify: `internal/nodetray/config/store_test.go`
- Test: `internal/nodetray/config/store_windows_test.go`

**Interfaces:**
- Consumes: 现有 `Store.LoadAgentForm`、`Store.ValidateAgentForm`、`Store.SaveAgentForm`、`Store.PrepareHelperWrite`、`Store.saveLocked`。
- Produces: `func (s *Store) SaveHelperForm(value HelperForm) (string, error)`；`LoadHelperForm` 可严格解码 `allowed_roots: []` 的实际配置供编辑。

- [ ] **Step 1: 写 Helper 本地保存的失败测试**

在 `store_test.go` 增加真实文件测试，先创建有效 Helper 配置，再调用期望的新 API，并断言正式文件、`.last-good` 和返回 SHA：

```go
func TestStoreSaveHelperFormWritesLocalConfigAndLastGood(t *testing.T) {
	store, paths := newTestStore(t)
	first := HelperToForm(validHelperConfig(t))
	first.AllowedRoots = []string{runTempDir(t)}
	firstSHA, err := store.SaveHelperForm(first)
	if err != nil || len(firstSHA) != 64 {
		t.Fatalf("first save = %q, %v", firstSHA, err)
	}
	second := first
	second.FrameReadTimeoutSec++
	secondSHA, err := store.SaveHelperForm(second)
	if err != nil || secondSHA == firstSHA {
		t.Fatalf("second save = %q, %v", secondSHA, err)
	}
	if _, err := loadHelperConfig(paths.HelperConfig, paths.HelperExecutable); err != nil {
		t.Fatalf("formal Helper config: %v", err)
	}
	if _, err := loadHelperConfig(paths.HelperConfig+".last-good", paths.HelperExecutable); err != nil {
		t.Fatalf("last-good Helper config: %v", err)
	}
}
```

同时增加 `LoadHelperForm` 对实际 `helper.json` 中空 `allowed_roots` 的测试，断言默认 denied roots 和 log 目录仍可展示。

- [ ] **Step 2: 运行测试并确认 RED**

Run:

```powershell
go test -count=1 ./internal/nodetray/config -run 'SaveHelperFormWritesLocalConfig|LoadHelperFormAllowsEditableEmptyRoots'
```

Expected: FAIL，缺少 `SaveHelperForm`，且现有实际 Helper loader 拒绝空 `allowed_roots`。

- [ ] **Step 3: 实现最小本地保存**

在 `store.go` 中添加：

```go
func (s *Store) SaveHelperForm(value HelperForm) (string, error) {
	prepared, err := s.PrepareHelperWrite(value)
	if err != nil {
		return "", err
	}
	err = s.withWriteLock(s.paths.HelperConfig, func() error {
		return s.saveLocked(s.paths.HelperConfig, prepared.CanonicalJSON, s.helperCanonicalLoader)
	})
	if err != nil {
		return "", err
	}
	return prepared.SHA256, nil
}
```

为 `LoadHelperForm` 使用严格 JSON 解码的编辑读取路径：未知字段和尾随内容仍拒绝；`LogDir` 为空时只为表单补成 `data/helper/logs`；不在读取阶段要求非空 `allowed_roots`。保存与启动仍使用完整 `helper.ValidateConfig`。

- [ ] **Step 4: 验证 GREEN 和 Windows DACL 回归**

Run:

```powershell
go test -count=1 ./internal/nodetray/config -run 'SaveHelperFormWritesLocalConfig|LoadHelperFormAllowsEditableEmptyRoots|ProtectedWindowsDACL'
```

Expected: PASS；正式文件和替换后的文件保持现有受限 DACL。

- [ ] **Step 5: 精确提交**

```powershell
git add -- internal/nodetray/config/store.go internal/nodetray/config/store_test.go internal/nodetray/config/store_windows_test.go
git commit -m "feat: save helper config from nodetray"
```

---

### Task 2: 应用服务改为本地 Agent/Helper 配置并仅在 Helper 启动时提权

**Files:**
- Modify: `internal/nodetray/app/service.go`
- Modify: `internal/nodetray/app/service_test.go`
- Modify: `internal/nodetray/bootstrap/bootstrap.go`
- Modify: `internal/nodetray/bootstrap/bootstrap_test.go`
- Modify: `internal/nodetray/agentclient/controller.go`
- Modify: `internal/nodetray/agentclient/controller_test.go`

**Interfaces:**
- Consumes: Task 1 的 `Store.SaveHelperForm` 和现有 `Store.SaveAgentForm`。
- Produces: `AgentEndpointController.StageAgentEndpoint(config.AgentForm) error`；应用服务不再依赖 `AgentConfigGateway`；自动 Helper 启动直接调用 `Managed.Start`。

- [ ] **Step 1: 写 Agent 离线配置与 Helper 无 UAC 保存的失败测试**

调整 `fakeStore` 实现 Agent/Helper 本地方法，并新增行为断言：

```go
func TestAgentAndHelperConfigUseLocalStoreWithoutElevation(t *testing.T) {
	s, calls, _, _, _, _ := serviceFixture(t)
	if _, err := s.GetAgentForm(context.Background()); err != nil {
		t.Fatal(err)
	}
	if result := s.SaveAgent(context.Background(), validAgentForm()); !result.OK {
		t.Fatalf("SaveAgent = %#v", result)
	}
	if result := s.SaveHelper(context.Background(), validHelperForm()); !result.OK {
		t.Fatalf("SaveHelper = %#v", result)
	}
	for _, call := range *calls {
		if strings.Contains(call, "socket-config") || strings.Contains(call, "elevate-write_helper_config") {
			t.Fatalf("configuration used forbidden route: %v", *calls)
		}
	}
}
```

新增 Helper 启动测试：无效空 roots 时不调用 `helper-start`；有效配置时调用一次 `helper-start`，其底层 Windows launcher 已由现有测试锁定为 `runas`。

- [ ] **Step 2: 写自动启动不走计划任务的失败测试**

在 `bootstrap_test.go` 将自动 Helper 期望从 `task-run` 改为 `helper-start`：

```go
func TestBootstrapAutomaticHelperUsesElevatedComponentStart(t *testing.T) {
	deps, calls, _, _, _ := bootstrapFixture(t, bootstrapSettings(traymodel.StartManual, traymodel.StartAutomatic, false))
	if _, err := Run(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(*calls, "task-run") || !slices.Contains(*calls, "helper-start") {
		t.Fatalf("calls = %v", *calls)
	}
}
```

- [ ] **Step 3: 运行测试并确认 RED**

Run:

```powershell
go test -count=1 ./internal/nodetray/app ./internal/nodetray/bootstrap ./internal/nodetray/agentclient -run 'LocalStoreWithoutElevation|AutomaticHelperUsesElevatedComponentStart|HelperStartRejectsInvalidConfig|StageAgentEndpoint'
```

Expected: FAIL；Agent 仍走 Socket、Helper 保存仍调用 elevation、自动启动仍调用计划任务。

- [ ] **Step 4: 改造 Service 的配置依赖**

将 `app.Store` 扩为：

```go
type Store interface {
	LoadTraySettings() (traymodel.TraySettings, error)
	SaveTraySettings(traymodel.TraySettings) error
	LoadAgentForm() (config.AgentForm, error)
	ValidateAgentForm(config.AgentForm) []config.FieldError
	SaveAgentForm(config.AgentForm) (string, error)
	LoadHelperForm() (config.HelperForm, error)
	ValidateHelperForm(config.HelperForm) []config.FieldError
	SaveHelperForm(config.HelperForm) (string, error)
	AgentFingerprint() (string, error)
	HelperFingerprint() (string, error)
}
```

删除 Service 生产路径对 `AgentConfigGateway` 和 Helper 配置 elevation 的调用：

- `GetAgentForm`、`ValidateAgent`、`SaveAgent` 直接调用 Store。
- `SaveHelper` 直接调用 `Store.SaveHelperForm`。
- 两种保存仅在成功后更新对应 Supervisor fingerprint，并以 `Refresh(ctx).NeedsRestart` 返回漂移状态。
- `SaveTraySettings` 不调用 `ensureDefaultHelperConfig` 或 `reconcileHelperTaskPolicy`。
- `StartHelper` 先加载并完整验证 Helper 配置，再调用组件 `Start`。
- `RestartHelper` 使用组件 `Restart`，不调用计划任务。

- [ ] **Step 5: 保留 Agent 端点切换但移除配置 Socket**

在 `agentclient.Controller` 添加只更新本地连接元数据的方法：

```go
func (c *Controller) StageAgentEndpoint(value trayconfig.AgentForm) error {
	endpoint, err := LoopbackEndpoint(net.JoinHostPort(value.ListenHost, strconv.Itoa(value.ListenPort)))
	if err != nil {
		return errors.New("agent_config_invalid")
	}
	c.mu.Lock()
	c.pendingEndpoint = endpoint
	c.mu.Unlock()
	return nil
}
```

Service 保存 Agent 成功后调用该方法；`StartAgent` 和保存后重启路径在启动前调用 `PromotePendingEndpoint`。不得调用 `local.config.get`、`local.config.validate` 或 `local.config.save`。

- [ ] **Step 6: 自动 Helper 启动改走组件启动**

在 `bootstrap.Run` 中将：

```go
dependencies.Task.Run(ctx)
```

替换为：

```go
reportOperation(dependencies.Attention, "helper", helper.Start(ctx), "Helper 自动启动失败")
```

这会进入 `process.ManualHelperLauncher`，由现有 `ShellExecute` `runas` 合同产生 UAC。

- [ ] **Step 7: 验证 GREEN**

Run:

```powershell
go test -count=1 ./internal/nodetray/app ./internal/nodetray/bootstrap ./internal/nodetray/agentclient -run 'LocalStoreWithoutElevation|AutomaticHelperUsesElevatedComponentStart|HelperStartRejectsInvalidConfig|StageAgentEndpoint|SaveAndRestartAgent'
```

Expected: PASS。

- [ ] **Step 8: 精确提交**

```powershell
git add -- internal/nodetray/app/service.go internal/nodetray/app/service_test.go internal/nodetray/bootstrap/bootstrap.go internal/nodetray/bootstrap/bootstrap_test.go internal/nodetray/agentclient/controller.go internal/nodetray/agentclient/controller_test.go
git commit -m "refactor: manage component configs locally"
```

---

### Task 3: 修改 Windows 生产接线，Socket 只保留控制职责

**Files:**
- Modify: `nodetray/composition.go`
- Modify: `nodetray/composition_test.go`
- Modify: `nodetray/composition_windows.go`
- Modify: `nodetray/composition_windows_test.go`
- Modify: `nodetray/app_test.go`

**Interfaces:**
- Consumes: Task 2 的本地 Store 配置接口与 `StageAgentEndpoint`。
- Produces: 生产 `Service` 使用 `native.Store` 管理配置；`sharedAgentController` 只注入 LocalAgent、Worker、生命周期和端点更新职责。

- [ ] **Step 1: 写生产接线失败测试**

把原 `TestProductionCompositionRoutesAgentConfigThroughInjectedSocketGateway` 改为：

```go
func TestProductionCompositionRoutesAgentAndHelperConfigThroughStore(t *testing.T) {
	inputs := validCompositionInputs()
	backend, err := composeProductionBackendWith(inputs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.service.GetAgentForm(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.service.GetHelperForm(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(inputs.Store.(*recordingStore).calls, "load-agent") {
		t.Fatalf("config did not use Store")
	}
}
```

Windows 接线测试断言不再构造 `production.NewAgentConfigGateway(sharedAgentController)`。

- [ ] **Step 2: 运行测试并确认 RED**

Run:

```powershell
go test -count=1 ./nodetray -run 'ProductionCompositionRoutesAgentAndHelperConfigThroughStore|WindowsProduction.*LocalConfig'
```

Expected: FAIL，生产组合仍要求并注入 Socket AgentConfig gateway。

- [ ] **Step 3: 实现最小接线调整**

- 从 `productionCompositionInputs`、`trayapp.Dependencies` 和 composition 必填校验中移除 `AgentConfig`。
- 将共享 Agent Controller 作为端点更新器注入 Service，但保留它作为 `LocalAgent` 和 `WorkerProvider`。
- 保留 `CloseAgentControl`，确保退出时关闭唯一 Socket client。
- `windowsProductionStore` 直接满足 Task 2 扩展后的 Store 接口。
- 不删除 `production.AgentConfigGateway` 和 Agent 端兼容协议代码。

- [ ] **Step 4: 验证 GREEN**

Run:

```powershell
go test -count=1 ./nodetray ./internal/nodetray/app ./internal/nodetray/production -run 'ProductionComposition|AgentConfig|LocalConfig|SaveAgent|SaveHelper'
```

Expected: PASS。

- [ ] **Step 5: 精确提交**

```powershell
git add -- nodetray/composition.go nodetray/composition_test.go nodetray/composition_windows.go nodetray/composition_windows_test.go nodetray/app_test.go
git commit -m "refactor: wire nodetray to local config store"
```

---

### Task 4: 发布实际 Helper 配置并生成双 ZIP

**Files:**
- Modify: `scripts/package-node-release.ps1`
- Modify: `scripts/test-package-node-release.ps1`
- Modify: `scripts/test-node-tray-supply-chain.ps1`
- Modify: `deploy/README-节点部署.md`

**Interfaces:**
- Consumes: stage 中的 `agent.default.json`、`helper.default.json`、`nodetray.default.json`。
- Produces: Compute ZIP 中三份实际配置和新的双 ZIP 发布物。

- [ ] **Step 1: 更新包合同测试并确认 RED**

把精确文件表中的 `helper.default.json` 改为 `data/helper/helper.json`，删除“不允许预建 data/helper”的断言，并增加：

```powershell
$helperPath = Join-Path $payloadRoot 'data\helper\helper.json'
Assert-True (Test-Path -LiteralPath $helperPath -PathType Leaf) 'Helper config missing'
$helper = Get-Content -Raw -LiteralPath $helperPath | ConvertFrom-Json
Assert-True (@($helper.allowed_roots).Count -eq 0) 'Helper default must not authorize a root'
Assert-True (-not ($actualFiles -contains 'helper.default.json')) 'manual-copy Helper template must not ship'
```

Run:

```powershell
pwsh -NoProfile -File scripts/test-package-node-release.ps1
```

Expected: FAIL，旧包仍只有根目录 `helper.default.json`。

- [ ] **Step 2: 修改打包映射和供应链断言**

在 `package-node-release.ps1` 中把 Helper 默认文件映射为实际配置：

```powershell
Copy-RequiredFile -SourceRoot $stage -RelativeSource 'helper.default.json' `
    -DestinationRoot $payload -RelativeDestination 'data\helper\helper.json'
```

从根目录通用复制表删除 `helper.default.json`。同步供应链脚本和中文部署说明：配置保存不提权，Helper 启动才出现 UAC。

- [ ] **Step 3: 验证包合同 GREEN**

Run:

```powershell
pwsh -NoProfile -File scripts/test-package-node-release.ps1
pwsh -NoProfile -File scripts/test-node-tray-supply-chain.ps1
```

Expected: 两项 PASS；Compute 精确文件表包含三份实际配置。

- [ ] **Step 4: 运行受影响包聚焦回归**

Run:

```powershell
go test -count=1 ./internal/nodetray/config ./internal/nodetray/app ./internal/nodetray/bootstrap ./internal/nodetray/agentclient ./internal/nodetray/production ./nodetray
git diff --check
```

Expected: PASS；不运行 `go test ./...`。

- [ ] **Step 5: 精确提交发布合同**

```powershell
git add -- scripts/package-node-release.ps1 scripts/test-package-node-release.ps1 scripts/test-node-tray-supply-chain.ps1 deploy/README-节点部署.md
git commit -m "fix: ship editable helper configuration"
```

- [ ] **Step 6: 构建 NodeTray 并生成双 ZIP**

使用现有验证发布中的未变 Agent/Worker/Helper/GUI/native 文件，只重新构建本次发生代码变化的 `nodetray.exe`，然后执行：

```powershell
pwsh -NoProfile -File scripts/package-portable-release.ps1 `
  -StageDir '<fresh-stage>' `
  -OutputDir 'D:\code\mySingerServer\publish' `
  -ReleaseId "$(Get-Date -Format yyyyMMdd)-main-$(git rev-parse --short HEAD)" `
  -BuildDate (Get-Date -Format yyyy-MM-dd) `
  -SourceRevision (git rev-parse HEAD)
```

构建时使用项目现有 `scripts/build-nodetray.ps1`，不重复执行已知无关的 VideoCore 全量 provenance 门禁。

- [ ] **Step 7: 验证发布物**

逐个校验 ZIP sidecar SHA-256，并从 Compute ZIP 读取：

```text
MySingerServer-Compute/data/agent/agent.json
MySingerServer-Compute/data/helper/helper.json
MySingerServer-Compute/data/nodetray/tray.json
```

确认根目录没有 `helper.default.json`，manifest 的 `source_revision` 等于最终提交，最后报告两个 ZIP 的绝对路径和 SHA-256。
