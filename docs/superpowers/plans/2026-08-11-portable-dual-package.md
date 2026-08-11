# 完全便携双发布包实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让计算端和远端管理端都能从任意本地可写目录运行，并从一次构建中生成职责隔离的 Compute 与 Manager 两个 Windows x64 ZIP。

**Architecture:** 计算端以 `nodetray.exe` 最终真实路径的父目录作为唯一便携根，所有程序和运行数据都从该根解析。管理端以 `gui.exe` 所在目录解析默认配置和日志，在 HTTP 监听成功后打开浏览器。发布阶段分别构造 Compute 与 Manager 候选包，全部验证通过后才发布两个 ZIP 和 sidecar。

**Tech Stack:** Go 1.26.5、Windows API、Wails v2、PowerShell 7、PostgreSQL/pgx、现有 NodeTray/Agent/Worker/Helper/Everything 发布链路。

## Global Constraints

- 不读取、不迁移或回退到旧 `ProgramData`、`LocalAppData` 配置和数据。
- 默认路径不得依赖当前工作目录；显式 `-config` 继续优先。
- 同一台机器保持一个活动计算端实例，不增加多实例命名空间。
- 支持本地磁盘和可移动磁盘中的绝对路径；拒绝 UNC 根。
- 计算包保留 NodeTray、Agent、Worker、Helper、Everything 和全部计算依赖，不包含 `gui.exe`。
- 管理包仅包含 GUI、无敏感信息模板、启动脚本、中文说明和 manifest，不包含计算依赖或 PostgreSQL。
- GUI 默认自动打开浏览器，`-no-browser` 必须禁止该动作。
- 按已确认选择，不因计算目录允许普通用户写入而禁用 Helper；文档必须保留提权风险警告。
- 只提交本计划涉及的文件，保留现有未提交文档、图文件和 `.codex-temp/`。
- 真实 GUI、UAC、计划任务、外部 PostgreSQL、远端 Agent 和媒体扫描未运行时必须报告 `PARTIAL` 或 `BLOCKED`。

---

### Task 1: 建立计算端便携布局模型

**Files:**
- Modify: `internal/nodetray/production/layout.go`
- Modify: `internal/nodetray/production/layout_test.go`

**Interfaces:**
- Consumes: 当前 NodeTray 可执行文件的最终绝对路径。
- Produces: `ResolvePortableLayout(trayExecutable string) (Layout, error)`；扩展后的 `Layout.Root` 和 `Layout.WebViewData`。

- [ ] **Step 1: 写便携布局失败测试**

用表驱动测试固定以下结果：

```go
func TestResolvePortableLayoutUsesExecutableDirectoryForProgramsAndData(t *testing.T) {
    executable := `D:\便携 工具\MySingerServer-Compute\nodetray.exe`
    got, err := ResolvePortableLayout(executable)
    if err != nil { t.Fatal(err) }
    want := Layout{
        Root:             `D:\便携 工具\MySingerServer-Compute`,
        TrayExecutable:   executable,
        AgentExecutable:  `D:\便携 工具\MySingerServer-Compute\agent.exe`,
        HelperExecutable: `D:\便携 工具\MySingerServer-Compute\helper.exe`,
        TraySettings:     `D:\便携 工具\MySingerServer-Compute\data\nodetray\tray.json`,
        AgentConfig:      `D:\便携 工具\MySingerServer-Compute\data\agent\agent.json`,
        HelperConfig:     `D:\便携 工具\MySingerServer-Compute\data\helper\helper.json`,
        AgentLogs:        `D:\便携 工具\MySingerServer-Compute\data\agent\logs`,
        HelperLogs:       `D:\便携 工具\MySingerServer-Compute\data\helper\logs`,
        WebViewData:      `D:\便携 工具\MySingerServer-Compute\data\nodetray\webview2`,
    }
    if !reflect.DeepEqual(got, want) { t.Fatalf("layout=%#v want=%#v", got, want) }
}
```

另加 `TestResolvePortableLayoutRejectsRelativeUNCAndWrongExecutableName`，覆盖相对路径、`\\server\share\nodetray.exe`、根目录和非 `nodetray.exe` 文件名。

- [ ] **Step 2: 运行测试并确认 RED**

Run:

```powershell
New-Item -ItemType Directory -Force -Path '.codex-temp\portable-dual-package\gocache','.codex-temp\portable-dual-package\gotmp' | Out-Null
$env:GOCACHE=(Resolve-Path '.codex-temp\portable-dual-package\gocache').Path
$env:GOTMPDIR=(Resolve-Path '.codex-temp\portable-dual-package\gotmp').Path
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 .\internal\nodetray\production -run '^TestResolvePortableLayout'
```

Expected: FAIL，因为 `ResolvePortableLayout`、`Root` 和 `WebViewData` 尚不存在。

- [ ] **Step 3: 实现最小布局生成器**

在 `Layout` 增加：

```go
Root        string
WebViewData string
```

实现：

```go
func ResolvePortableLayout(trayExecutable string) (Layout, error)
```

规则为：清理后必须是本地绝对路径，卷名不得以 `\\` 开头，文件名必须忽略大小写等于 `nodetray.exe`；根为父目录。所有数据路径严格位于 `<root>\data` 下，程序路径位于根目录。沿用 `strictlyBelow` 检查每个生成路径未逃逸根。

移除生产代码不再需要的 `ResolveLayout(programFiles, programData, localAppData)`；同步把原固定布局测试替换为便携布局测试。

- [ ] **Step 4: 运行 GREEN 与包回归**

Run:

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 .\internal\nodetray\production
```

Expected: PASS。

- [ ] **Step 5: 提交布局模型**

```powershell
git add -- internal/nodetray/production/layout.go internal/nodetray/production/layout_test.go
git commit -m "feat: resolve portable compute layout"
```

---

### Task 2: 将 NodeTray、UAC 和 WebView2 接入便携根

**Files:**
- Modify: `nodetray/composition_windows.go`
- Modify: `nodetray/composition_windows_test.go`
- Modify: `nodetray/elevated_windows.go`
- Modify: `nodetray/elevated_windows_test.go`
- Modify: `nodetray/composition.go`
- Modify: `nodetray/composition_test.go`
- Modify: `nodetray/app.go`
- Modify: `nodetray/main.go`
- Modify: `nodetray/app_test.go`

**Interfaces:**
- Consumes: `production.ResolvePortableLayout(self.ExecutablePath)`。
- Produces: 普通入口与 `--elevated-once` 共用的便携 `production.Layout`；`productionCompositionInputs.PortableRoot string`、`productionCompositionInputs.WebViewDataPath string`；`Backend.webViewDataPath string`。

- [ ] **Step 1: 写普通入口和 UAC 入口失败测试**

把 `TestResolveWindowsLayoutUsesKnownFoldersOnly` 替换为：

```go
func TestWindowsProductionCompositionUsesInspectedPortableExecutable(t *testing.T)
```

通过注入 Inspector 返回 `D:\便携 工具\Compute\nodetray.exe`，断言构造得到的 Store、Agent、Helper、登录启动、任务定义和 WebView2 路径全部在该根下，并断言 known-folder API 不再是依赖。

在 `elevated_windows_test.go` 增加：

```go
func TestElevatedEntryDerivesHelperPathsFromItsPortableTrayExecutable(t *testing.T)
```

断言管理员一次性进程使用同一根下的 `helper.exe` 和 `data\helper\helper.json`，而不是 Program Files/ProgramData。

在 `app_test.go` 增加：

```go
func TestRunNormalWailsUsesBackendPortableWebViewData(t *testing.T)
```

断言 `newWailsOptions` 收到 `D:\便携 工具\Compute\data\nodetray\webview2`，且不会调用 `os.UserConfigDir`。

- [ ] **Step 2: 运行测试并确认 RED**

Run:

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 .\nodetray -run 'Portable|ElevatedEntry|RunNormalWails'
```

Expected: FAIL，当前普通和管理员组合仍从 Windows Known Folders 生成固定布局，Wails 仍使用用户配置目录。

- [ ] **Step 3: 改造普通生产组合**

在 `composeWindowsProductionBackend` 中先调用 Inspector 获取当前进程身份，再执行：

```go
layout, err := production.ResolvePortableLayout(self.ExecutablePath)
```

删除 `FOLDERID_ProgramFiles`、`FOLDERID_ProgramData`、`FOLDERID_LocalAppData` 查询和“outside fixed deployment”固定目录比较。仍使用 FinalPathResolver 验证当前 `nodetray.exe` 的最终路径与 `layout.TrayExecutable` 相同，保留父子进程、可执行身份和协议验证。

把 `layout.Root` 和 `layout.WebViewData` 写入新的 `productionCompositionInputs.PortableRoot`、`productionCompositionInputs.WebViewDataPath`。`composeProductionBackendWith` 校验二者为绝对路径且 WebView 数据严格位于计算根后，写入 `Backend.webViewDataPath`。

- [ ] **Step 4: 改造管理员入口和 Wails 用户目录**

`runWindowsElevatedOnce` 不再预先调用 fixed layout resolver。`runElevatedOnceWith` 获取并验证当前进程身份后，从 `self.ExecutablePath` 调用 `ResolvePortableLayout`，再冻结 Helper 路径和任务定义。

`runNormalWails` 改为读取 `backend.webViewDataPath`；空值或非绝对路径返回稳定错误 `portable_data_unavailable`。删除 `userConfigDirAdapter` 和 `user_config_unavailable` 分支，更新稳定错误码白名单。

保持登录启动值和计划任务注册逻辑不变；它们已接收 layout 中的绝对路径，现有 drift 比较会把旧目录视为不匹配并允许重新保存/修复。

- [ ] **Step 5: 运行 GREEN 与相关回归**

Run:

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 .\nodetray .\internal\nodetray\production .\internal\nodetray\config .\internal\nodetray\windows\loginstart .\internal\nodetray\windows\task
```

Expected: PASS。Windows ACL 测试继续证明 Helper 配置写入仍受现有 UAC/ACL 机制保护；本任务不增加“可写根目录禁止 Helper”的门禁。

- [ ] **Step 6: 提交 NodeTray 接入**

```powershell
git add -- nodetray/composition_windows.go nodetray/composition_windows_test.go nodetray/elevated_windows.go nodetray/elevated_windows_test.go nodetray/composition.go nodetray/composition_test.go nodetray/app.go nodetray/main.go nodetray/app_test.go
git commit -m "feat: run compute node from portable directory"
```

---

### Task 3: 让 GUI 使用便携配置、文件日志和自动浏览器

**Files:**
- Create: `cmd/gui/runtime_paths.go`
- Create: `cmd/gui/runtime_paths_test.go`
- Create: `cmd/gui/platform_windows.go`
- Create: `cmd/gui/platform_other.go`
- Modify: `cmd/gui/main.go`
- Modify: `cmd/gui/main_test.go`

**Interfaces:**
- Produces: `resolveGUIRuntimePaths(executable, requestedConfig string) (guiRuntimePaths, error)`；`localBrowserURL(listenAddr string) (string, error)`；`newGUIRuntimeLogger(logPath string, console io.Writer) (*slog.Logger, func() error, error)`。
- Runtime adapters: `guiExecutablePath func() (string, error)`、`guiListen func(network, address string) (net.Listener, error)`、`guiOpenBrowser func(string) error`、`guiShowStartupError func(string)`。

- [ ] **Step 1: 写路径、日志和浏览器 URL 失败测试**

新增：

```go
func TestResolveGUIRuntimePathsUsesExecutableDirectoryInsteadOfWorkingDirectory(t *testing.T)
func TestResolveGUIRuntimePathsKeepsExplicitConfigOverride(t *testing.T)
func TestGUIRuntimeLoggerWritesPortableLogAndConsole(t *testing.T)
func TestLocalBrowserURLMapsWildcardListenersToLoopback(t *testing.T)
```

核心断言：`D:\管理 工具\gui.exe` 默认配置为 `D:\管理 工具\gui.json`，日志为 `D:\管理 工具\data\logs\gui.log`；`0.0.0.0:8080` 映射为 `http://127.0.0.1:8080/`，`[::]:8080` 映射为 `http://[::1]:8080/`。

- [ ] **Step 2: 写启动时序和参数失败测试**

在 `main_test.go` 增加：

```go
func TestGUIOpensBrowserOnlyAfterListenerIsBound(t *testing.T)
func TestGUINoBrowserFlagSuppressesBrowserLaunch(t *testing.T)
func TestGUIStartupFailureIsLoggedBeforeInteractiveNotification(t *testing.T)
```

通过注入 listener、browser 和 notification adapter 记录事件顺序，必须得到 `listen -> browser`；`-no-browser` 只有 `listen`；配置缺失时必须先在便携日志留下记录，再调用稳定中文错误通知，通知内容不得包含 DSN 密码或完整私有路径。

- [ ] **Step 3: 运行测试并确认 RED**

Run:

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 .\cmd\gui -run 'RuntimePaths|RuntimeLogger|Browser|StartupFailure'
```

Expected: FAIL，因为 GUI 当前默认使用当前工作目录、只写 stdout、没有浏览器启动器，HTTP server 也没有预先绑定 listener。

- [ ] **Step 4: 实现便携路径和文件日志**

`guiRuntimePaths` 固定字段：

```go
type guiRuntimePaths struct {
    Root       string
    ConfigPath string
    LogPath    string
}
```

无显式配置时使用 `<exe-root>\gui.json`；显式配置按现有语义转为绝对路径。拒绝 UNC EXE 根。`newGUIRuntimeLogger` 创建 `data\logs`，用现有 lumberjack 依赖写 `gui.log`，并用 `io.MultiWriter` 同时输出控制台。

把 `-config` flag 默认值从字符串 `gui.json` 改为空字符串，使解析器能够区分“采用 EXE 同目录默认值”和“用户显式覆盖”；帮助文本仍说明默认文件为 EXE 同目录 `gui.json`。

`run` 在读取配置前初始化 logger，并在关闭 logger 前记录启动失败。`main` 只把经过稳定映射的中文摘要传给 `guiShowStartupError`，不得把原始错误或 DSN 直接送入对话框。

- [ ] **Step 5: 实现监听后打开浏览器**

flag 增加：

```go
noBrowser := flags.Bool("no-browser", false, "不自动打开浏览器")
```

把 server 接口从 `ListenAndServe()` 改为 `Serve(net.Listener)`。生产流程先调用 `net.Listen("tcp", cfg.ListenAddr)`；成功后记录实际监听，若未设置 `-no-browser`，调用 `guiOpenBrowser(localBrowserURL(cfg.ListenAddr))`，浏览器启动失败只记录 warning，不停止已绑定的管理服务。

`platform_windows.go` 使用单参数 Windows Shell 打开经过 URL 构造器验证的 `http://` URL，并用 MessageBox 显示稳定的交互错误摘要；`platform_other.go` 的浏览器入口返回平台不支持错误，错误通知为空实现，保持包可测试。

- [ ] **Step 6: 运行 GREEN 与完整 GUI 回归**

Run:

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 .\cmd\gui .\internal\gui .\internal\config
& 'C:\tmp\go1.26.5\go\bin\go.exe' vet .\cmd\gui .\internal\gui .\internal\config
```

Expected: PASS。

- [ ] **Step 7: 提交 GUI 便携运行时**

```powershell
git add -- cmd/gui/runtime_paths.go cmd/gui/runtime_paths_test.go cmd/gui/platform_windows.go cmd/gui/platform_other.go cmd/gui/main.go cmd/gui/main_test.go
git commit -m "feat: run manager from portable directory"
```

---

### Task 4: 新增独立 Manager 发布包

**Files:**
- Create: `scripts/package-manager-release.ps1`
- Create: `scripts/test-package-manager-release.ps1`
- Create: `deploy/Start-Manager.ps1`
- Create: `deploy/README-管理端部署.md`
- Modify: `deploy/gui.example.json`
- Modify: `scripts/test-node-tray-supply-chain.ps1`

**Interfaces:**
- Consumes: 阶段目录中的 `gui.exe` 和仓库 `deploy` 中三个管理端文件。
- Produces: `MySingerServer-manager-win-x64-<ReleaseId>.zip`、`.zip.sha256`，顶层目录为 `MySingerServer-Manager`。

- [ ] **Step 1: 写 Manager 包失败契约**

`test-package-manager-release.ps1` 构造只含 `gui.exe` 的模拟 stage，执行打包脚本并断言 ZIP 精确包含：

```text
MySingerServer-Manager/gui.exe
MySingerServer-Manager/gui.example.json
MySingerServer-Manager/Start-Manager.ps1
MySingerServer-Manager/README-管理端部署.md
MySingerServer-Manager/release-manifest.json
```

断言不存在 `agent.exe`、`worker.exe`、`helper.exe`、`nodetray.exe`、Everything、FFmpeg、VideoCore、WebView2 和 `gui.json`。断言 manifest `release_kind=remote-manager-portable`、`portable_root=.`，并验证文件哈希与 sidecar。

另用含密码 DSN、token query 或第二个真实 LAN Agent 地址的临时模板证明脚本 fail closed。

- [ ] **Step 2: 运行测试并确认 RED**

Run:

```powershell
& 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -File .\scripts\test-package-manager-release.ps1
```

Expected: FAIL，因为管理端打包脚本和部署文件尚不存在。

- [ ] **Step 3: 实现模板和启动脚本**

`gui.example.json` 保留无密码 DSN `postgres://dedup@127.0.0.1:5432/dedup`，Agent 示例只保留 `127.0.0.1:9101`，不写真实 LAN 地址。

`Start-Manager.ps1` 核心行为：

```powershell
$root = $PSScriptRoot
$exe = Join-Path $root 'gui.exe'
$config = Join-Path $root 'gui.json'
if (-not (Test-Path -LiteralPath $config -PathType Leaf)) {
    throw '请先把 gui.example.json 复制为 gui.json 并编辑 PostgreSQL 与 Agent 地址。'
}
& $exe -config $config @args
exit $LASTEXITCODE
```

中文 README 说明手工复制配置、双击 `gui.exe`、脚本启动、`-no-browser`、外部 PostgreSQL 和便携日志位置。

- [ ] **Step 4: 实现 Manager 打包器**

沿用节点打包器的路径规范化、目标冲突、UTF-8、候选目录、解压复核、逐文件 SHA-256 和 sidecar 模式。配置检查必须解析 JSON/URI，拒绝 DSN UserInfo 中的密码以及 query 中的 `password|passwd|pwd|token|secret`。

发布 manifest 不写安装目录，写：

```powershell
release_kind = 'remote-manager-portable'
portable_root = '.'
```

- [ ] **Step 5: 运行 GREEN 和供应链回归**

Run:

```powershell
& 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -File .\scripts\test-package-manager-release.ps1
& 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -File .\scripts\test-node-tray-supply-chain.ps1
```

Expected: PASS。

- [ ] **Step 6: 提交 Manager 发布包**

```powershell
git add -- scripts/package-manager-release.ps1 scripts/test-package-manager-release.ps1 deploy/Start-Manager.ps1 deploy/README-管理端部署.md deploy/gui.example.json scripts/test-node-tray-supply-chain.ps1
git commit -m "build: add portable manager release"
```

---

### Task 5: 将现有 Node ZIP 改为 Compute 便携包

**Files:**
- Modify: `scripts/package-node-release.ps1`
- Modify: `scripts/test-package-node-release.ps1`
- Modify: `deploy/README-节点部署.md`

**Interfaces:**
- Consumes: 现有完整 stage、计算端示例配置和许可证。
- Produces: `MySingerServer-compute-win-x64-<ReleaseId>.zip`、`.zip.sha256`，顶层目录为 `MySingerServer-Compute`。

- [ ] **Step 1: 修改测试期望并确认 RED**

测试要求：

- ZIP 和 sidecar 使用 `compute` 文件名；
- 顶层仅为 `MySingerServer-Compute`；
- manifest `release_kind=compute-node-portable`、`portable_root=.`；
- 不再存在 `install_root=C:\Program Files\MySingerServer\`；
- 不包含 `gui.exe`、`gui.json`、`gui.example.json`、`Start-Manager.ps1`；
- 保持 Everything 四文件、NodeTray、Agent、Worker、Helper、WebView2、原生依赖和许可证完整。

Run:

```powershell
& 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -File .\scripts\test-package-node-release.ps1
```

Expected: FAIL，当前输出仍名为 node、顶层仍为 `MySingerServer`，manifest 仍固定 Program Files。

- [ ] **Step 2: 实现 Compute 包命名和 manifest**

修改：

```powershell
$baseName = "MySingerServer-compute-win-x64-$ReleaseId"
$payload = Join-Path $work 'MySingerServer-Compute'
```

manifest 使用 `compute-node-portable` 和 `portable_root='.'`。保持当前所有 fail-closed 哈希、配置脱敏、原生闭包和不覆盖目标行为。

- [ ] **Step 3: 更新中文节点部署说明**

删除“必须复制到 Program Files”的要求，改为完整解压到任意本地可写目录。记录 `data` 子目录、不可使用 UNC、移动后修复登录启动/Helper 任务，以及普通用户可写根下启用 Helper 的提权风险。

- [ ] **Step 4: 运行 GREEN**

Run:

```powershell
& 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -File .\scripts\test-package-node-release.ps1
```

Expected: PASS，输出文件数和 manifest 哈希一致。

- [ ] **Step 5: 提交 Compute 包更新**

```powershell
git add -- scripts/package-node-release.ps1 scripts/test-package-node-release.ps1 deploy/README-节点部署.md
git commit -m "build: make compute release portable"
```

---

### Task 6: 增加一次生成双包的发布入口

**Files:**
- Create: `scripts/package-portable-release.ps1`
- Create: `scripts/test-package-portable-release.ps1`
- Modify: `README.md`

**Interfaces:**
- Consumes: `package-node-release.ps1` 和 `package-manager-release.ps1`。
- Produces: 同一 ReleaseId 的 Compute ZIP/sidecar 与 Manager ZIP/sidecar；候选全部通过后才进入最终输出目录。

- [ ] **Step 1: 写双包发布失败测试**

测试先创建同时满足两个子打包器的 stage，运行总入口并验证四个最终文件。第二场景故意移除 `gui.exe`，断言命令失败且最终输出目录中没有任何本 ReleaseId 文件。第三场景预置任一目标文件，断言开始候选构建前就 fail closed 且不覆盖。

- [ ] **Step 2: 运行测试并确认 RED**

Run:

```powershell
& 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -File .\scripts\test-package-portable-release.ps1
```

Expected: FAIL，因为总发布入口尚不存在。

- [ ] **Step 3: 实现候选构建、复核和发布回滚**

总入口参数与子脚本统一：

```powershell
param(
    [Parameter(Mandatory)][string]$StageDir,
    [string]$OutputDir = 'artifacts\releases',
    [string]$ReleaseId = (Get-Date -Format 'yyyyMMdd'),
    [string]$BuildDate = (Get-Date -Format 'yyyy-MM-dd'),
    [string]$SourceRevision = 'N/A_NO_GIT_METADATA'
)
```

先计算四个最终路径并拒绝任何冲突。两个子打包器只写入任务专用候选目录；总入口再次验证 ZIP、sidecar 和 manifest product/release_kind。发布时记录已移动文件，若后续移动失败，只删除本次已移动且哈希仍等于候选哈希的文件，然后抛出 `PORTABLE_RELEASE_PUBLISH_FAILED`。

- [ ] **Step 4: 更新根 README 发布说明**

增加两个产品职责、便携目录、Manager 手工配置、双击自动浏览器、总发布命令和四个输出文件名。删除把 GUI 与计算端描述为同一发布目录的陈旧命令。

- [ ] **Step 5: 运行 GREEN 与三个打包契约**

Run:

```powershell
& 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -File .\scripts\test-package-node-release.ps1
& 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -File .\scripts\test-package-manager-release.ps1
& 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -File .\scripts\test-package-portable-release.ps1
```

Expected: 三项 PASS。

- [ ] **Step 6: 提交双包入口**

```powershell
git add -- scripts/package-portable-release.ps1 scripts/test-package-portable-release.ps1 README.md
git commit -m "build: publish compute and manager packages"
```

---

### Task 7: 全面验证并生成新鲜双发布包

**Files:**
- Verify: Tasks 1-6 所有修改文件
- Generate: `artifacts/portable-dual-stage-20260811/`
- Generate: `artifacts/releases/MySingerServer-compute-win-x64-portable-20260811.zip`
- Generate: `artifacts/releases/MySingerServer-manager-win-x64-portable-20260811.zip`

**Interfaces:**
- Consumes: 完整构建脚本和双包总发布入口。
- Produces: 新鲜 stage、两个 ZIP、两个 sidecar、验证证据和明确的动态验收边界。

- [ ] **Step 1: 格式化并检查差异**

```powershell
& 'C:\tmp\go1.26.5\go\bin\gofmt.exe' -w internal\nodetray\production\layout.go internal\nodetray\production\layout_test.go nodetray\composition_windows.go nodetray\composition_windows_test.go nodetray\elevated_windows.go nodetray\elevated_windows_test.go nodetray\composition.go nodetray\composition_test.go nodetray\app.go nodetray\main.go nodetray\app_test.go cmd\gui\runtime_paths.go cmd\gui\runtime_paths_test.go cmd\gui\platform_windows.go cmd\gui\platform_other.go cmd\gui\main.go cmd\gui\main_test.go
git diff --check
```

- [ ] **Step 2: 运行聚焦 Go 门禁**

```powershell
New-Item -ItemType Directory -Force -Path '.codex-temp\portable-dual-package\gocache','.codex-temp\portable-dual-package\gotmp' | Out-Null
$env:GOCACHE=(Resolve-Path '.codex-temp\portable-dual-package\gocache').Path
$env:GOTMPDIR=(Resolve-Path '.codex-temp\portable-dual-package\gotmp').Path
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 .\internal\nodetray\production .\internal\nodetray\config .\internal\nodetray\windows\loginstart .\internal\nodetray\windows\task .\nodetray .\cmd\gui .\internal\gui .\internal\config
& 'C:\tmp\go1.26.5\go\bin\go.exe' vet .\internal\nodetray\production .\nodetray .\cmd\gui .\internal\gui
```

- [ ] **Step 3: 运行发布契约和全仓串行测试**

```powershell
& 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -File .\scripts\test-package-node-release.ps1
& 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -File .\scripts\test-package-manager-release.ps1
& 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -File .\scripts\test-package-portable-release.ps1
& 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -File .\scripts\test-node-tray-supply-chain.ps1
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 .\...
```

全仓门禁若仍被现有 `artifacts` 混合包、PowerShell 5 策略或外部工具链阻塞，必须逐项报告，不能据此否定已通过的聚焦门禁，也不能声称全仓通过。

- [ ] **Step 4: 生成新阶段目录**

确保目标不存在后执行：

```powershell
& 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -File .\scripts\build.ps1 `
  -Go 'C:\tmp\go1.26.5\go\bin\go.exe' `
  -CC 'C:\Users\Administrator\AppData\Local\Temp\winlibs-gcc\mingw64\bin\gcc.exe' `
  -Windres 'C:\Users\Administrator\AppData\Local\Temp\winlibs-gcc\mingw64\bin\windres.exe' `
  -Dlltool 'C:\Users\Administrator\AppData\Local\Temp\winlibs-gcc\mingw64\bin\dlltool.exe' `
  -CMake 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\cmake.exe' `
  -VcpkgRoot 'C:\vcpkg' `
  -StageDir '.\artifacts\portable-dual-stage-20260811'
```

固定 MinGW 路径缺失时报告 `BLOCKED_TOOLCHAIN_MISSING`，不得复用旧 stage 或下载未固定工具链冒充新构建。

- [ ] **Step 5: 生成并复核两个 ZIP**

```powershell
& 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -File .\scripts\package-portable-release.ps1 `
  -StageDir '.\artifacts\portable-dual-stage-20260811' `
  -OutputDir '.\artifacts\releases' `
  -ReleaseId 'portable-20260811' `
  -BuildDate '2026-08-11' `
  -SourceRevision (git rev-parse HEAD)
```

分别解压两个 ZIP，验证精确文件集合、manifest、逐文件哈希和 sidecar；记录两个 ZIP 的绝对路径、大小和 SHA-256。

- [ ] **Step 6: 记录动态验收边界和最终状态**

如果未实际从含空格/中文路径启动计算端、完成 UAC/计划任务漂移修复、连接外部 PostgreSQL/远端 Agent 并扫描媒体，则将相应项明确标记为 `PARTIAL` 或 `BLOCKED`。确认任务文件无未提交修改，现有用户文档和 `.codex-temp` 未被暂存。
