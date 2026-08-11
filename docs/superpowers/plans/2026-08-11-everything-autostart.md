# Everything 自动启动与就绪等待实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 发布包携带官方 Everything 运行文件，并让启用 Everything 的扫描在客户端未运行时自动启动它、无限等待数据库就绪后再枚举路径。

**Architecture:** 供应链固定 Everything 1.4.1.1032 x64 非 Lite 便携版并将 EXE、SDK DLL 和许可证纳入阶段目录与节点 ZIP。Agent 使用异步自动启动枚举器：监听与控制端点不等待 Everything，扫描调用等待一个可取消的就绪门控；不可恢复的文件或启动错误才选择 Walker。

**Tech Stack:** Go 1.26.5、`golang.org/x/sys/windows`、PowerShell 7、voidtools Everything SDK 1.4、现有节点发布 ZIP/manifest 流程。

## Global Constraints

- 不安装、启动、停止或修改 Windows `Everything` 服务，不触发 UAC。
- `use_everything=false` 时保持现有 Walker 行为。
- Everything 已成功启动但数据库未加载时不设置总超时、不回退 Walker。
- Agent/NodeTray 仍须在现有 30 秒就绪预算内启动；无限等待只门控扫描。
- Agent 关闭或重启必须取消等待。
- 只提交本计划涉及的文件，不暂存现有未提交文档和 `.codex-temp/`。

---

### Task 1: 固定 Everything 运行文件并扩展发布契约

**Files:**
- Create: `third_party/everything/Everything.exe`
- Create: `third_party/everything/LICENSE.txt`
- Create: `third_party/everything/NOTICE.md`
- Create: `third_party/everything/manifest.json`
- Modify: `scripts/test-package-node-release.ps1`
- Modify: `scripts/test-node-tray-supply-chain.ps1`
- Modify: `scripts/build.ps1`
- Modify: `scripts/package-node-release.ps1`
- Modify: `deploy/README-节点部署.md`

**Interfaces:**
- Consumes: 官方 `https://www.voidtools.com/Everything-1.4.1.1032.x64.zip`，固定 ZIP SHA-256 `698df475ec44e638f66f1b6a32d28fea613cec78d3b6310e6abe53431eeb940c`。
- Produces: 阶段目录和节点 ZIP 根目录中的 `Everything.exe`、`Everything64.dll`，以及 `licenses/everything-LICENSE.txt`、`licenses/everything-NOTICE.md`。

- [ ] **Step 1: 写发布契约失败测试**

在 `scripts/test-package-node-release.ps1` 的模拟阶段目录中增加 `Everything.exe` 和 `licenses` 两个文件，并把它们加入 `$expectedFiles`：

```powershell
'Everything.exe',
'licenses/everything-LICENSE.txt',
'licenses/everything-NOTICE.md',
```

在 `scripts/test-node-tray-supply-chain.ps1` 的完整构建脚本文本契约中要求同样三个名称，并要求 `third_party\everything\manifest.json`、`NOTICE.md`、`LICENSE.txt`、`Everything.exe` 存在。

- [ ] **Step 2: 运行测试并确认 RED**

Run:

```powershell
& 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -File .\scripts\test-package-node-release.ps1
& 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -File .\scripts\test-node-tray-supply-chain.ps1
```

Expected: 首个测试因 ZIP 缺少 `Everything.exe`/许可证文件失败；第二个测试因源文件或构建复制契约缺失失败。

- [ ] **Step 3: 获取并固定官方文件**

下载到任务专用临时目录，先验证 ZIP SHA-256，再解压 `Everything.exe`；从 `https://www.voidtools.com/License.txt` 保存许可证。`manifest.json` 记录版本、架构、官方 URL、ZIP 固定哈希、解压后 EXE 的实际 SHA-256、大小和许可证路径；`NOTICE.md` 用中文说明来源、版本、非 Lite/IPC 要求和再分发许可证。

```powershell
$zipHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $zipPath).Hash.ToLowerInvariant()
if ($zipHash -cne '698df475ec44e638f66f1b6a32d28fea613cec78d3b6310e6abe53431eeb940c') {
    throw "EVERYTHING_ARCHIVE_SHA256_MISMATCH actual=$zipHash"
}
```

- [ ] **Step 4: 实现构建和节点 ZIP 复制**

`scripts/build.ps1` 从固定 `third_party` 路径复制四个运行/许可文件，并把它们加入 `$requiredStageFiles`。`scripts/package-node-release.ps1` 从阶段目录复制：

```powershell
'Everything.exe',
'Everything64.dll',
'licenses\everything-LICENSE.txt',
'licenses\everything-NOTICE.md'
```

保持既有目标冲突、缺失文件和发布 manifest 的 fail-closed 行为。

- [ ] **Step 5: 更新部署说明并验证 GREEN**

说明 `Everything.exe -startup` 由 Agent 自动调用，SDK 仍要求 `Everything64.dll` 同目录，Windows 服务不由本程序管理。重新运行 Step 2 两条命令，Expected: PASS。

- [ ] **Step 6: 提交供应链修改**

```powershell
git add -- third_party/everything scripts/build.ps1 scripts/package-node-release.ps1 scripts/test-package-node-release.ps1 scripts/test-node-tray-supply-chain.ps1 deploy/README-节点部署.md
git commit -m "build: package Everything runtime"
```

---

### Task 2: 增加可取消的无限就绪等待状态机

**Files:**
- Create: `internal/enum/autostart.go`
- Create: `internal/enum/autostart_test.go`
- Modify: `internal/enum/everything_windows.go`
- Modify: `internal/enum/enumerator_test.go`

**Interfaces:**
- Consumes: 现有 `Enumerator`、`ErrIPC`、`ResilientEnumerator`。
- Produces: `ErrIndexNotReady`、`AutoStartOptions`、`NewAutoStartEnumerator(options AutoStartOptions) *AutoStartEnumerator`、`(*AutoStartEnumerator).Start()`。

- [ ] **Step 1: 写状态机失败测试**

用可脚本化的 fake Enumerator 和 `Poll func(context.Context) error` 覆盖：

```go
func TestAutoStartEnumeratorStartsOnceAndWaitsUntilReady(t *testing.T)
func TestAutoStartEnumeratorDoesNotStartWhenAlreadyReady(t *testing.T)
func TestAutoStartEnumeratorWaitsWithoutTimeoutWhileIndexLoads(t *testing.T)
func TestAutoStartEnumeratorCancelsWaitingWithContext(t *testing.T)
func TestAutoStartEnumeratorFallsBackOnExecutableStartFailure(t *testing.T)
func TestAutoStartEnumeratorFallsBackOnPermanentSDKFailure(t *testing.T)
```

关键断言是连续多次返回 `ErrIndexNotReady` 后 fallback 调用数仍为 0；只有 fake 最终返回 `nil` 才释放等待中的 `Enum`。

- [ ] **Step 2: 运行测试并确认 RED**

Run:

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 .\internal\enum -run '^TestAutoStartEnumerator'
```

Expected: FAIL，因为自动启动枚举器类型和构造函数尚不存在。

- [ ] **Step 3: 实现最小状态机**

`AutoStartOptions` 使用以下明确依赖：

```go
type AutoStartOptions struct {
    Context        context.Context
    Primary        Enumerator
    Fallback       Enumerator
    StartClient    func() error
    Poll           func(context.Context) error
    OnWaiting      func(error)
    OnFallback     func(error)
    OnRootFallback func(string, error)
}
```

`Start()` 通过 `sync.Once` 启动一个 goroutine。首次 `Available()` 返回 `ErrIPC` 时调用 `StartClient` 一次；`ErrIPC` 或 `ErrIndexNotReady` 进入无限条件轮询；其他错误或启动失败选择 Walker。`Enum` 等待关闭的 ready channel，Context 取消时返回 `context.Canceled`；成功后委托 `ResilientEnumerator` 保留单根兜底。

- [ ] **Step 4: 用数据库状态修正 Everything 就绪判断**

在 `everythingProcs` 中解析 `Everything_IsDBLoaded`。`Available()` 先加载 DLL，再调用数据库状态：IPC 不可用返回 `ErrIPC`，IPC 已建立但数据库未完成加载返回 `ErrIndexNotReady`，已加载返回 `nil`。移除“索引至少有一条结果才算可用”的 `ErrEmptyIndex` 判定，并同步集成测试预期。

- [ ] **Step 5: 运行 GREEN 与包回归测试**

Run:

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 .\internal\enum
```

Expected: PASS，且原 Walker/Resilient 测试保持通过。

- [ ] **Step 6: 提交状态机**

```powershell
git add -- internal/enum/autostart.go internal/enum/autostart_test.go internal/enum/everything_windows.go internal/enum/enumerator_test.go
git commit -m "feat: wait for Everything index readiness"
```

---

### Task 3: 启动后台客户端并接入 Agent 扫描门控

**Files:**
- Create: `internal/enum/everything_process_windows.go`
- Create: `internal/enum/everything_process_windows_test.go`
- Modify: `cmd/agent/main.go`
- Modify: `cmd/agent/main_test.go`

**Interfaces:**
- Consumes: `NewEverythingEnumerator()`、`NewAutoStartEnumerator()` 和 Agent 已解析的绝对可执行路径。
- Produces: `StartEverythingClientAt(path string) error`，固定参数为 `-startup`；Agent 的 `newAgentEnumerator(...) Enumerator` 组合函数。

- [ ] **Step 1: 写进程与 Agent 组合失败测试**

测试 `StartEverythingClientAt` 对缺失文件返回带路径的错误；通过可注入 starter 测试 `newAgentEnumerator`：关闭配置立即返回 Walker，开启配置立即返回且后台开始探测，不阻塞 Agent 监听初始化。

```go
func TestStartEverythingClientAtRejectsMissingExecutable(t *testing.T)
func TestNewAgentEnumeratorDisabledUsesWalker(t *testing.T)
func TestNewAgentEnumeratorEnabledDoesNotBlockStartup(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run:

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 .\internal\enum .\cmd\agent -run 'Everything|AgentEnumerator'
```

Expected: FAIL，因为进程启动器和 Agent 组合函数尚不存在。

- [ ] **Step 3: 实现 Windows 后台启动**

`StartEverythingClientAt` 校验文件存在，执行 `Everything.exe -startup`，设置 `HideWindow=true`，成功 `Start` 后调用 `Process.Release()`，不等待长期运行的客户端退出；错误包含操作和绝对路径。

- [ ] **Step 4: 接入 Agent 生命周期**

把 `signal.NotifyContext` 提前到枚举器构造之前。`newAgentEnumerator` 在配置启用时创建自动启动枚举器，调用 `Start()` 后立即返回；生产 Poll 使用 250ms 可取消 timer，`OnWaiting` 每 30 秒写一次日志。扫描调用在 Everything ready channel 上等待，而 Agent 业务监听和控制端点继续正常启动。

不可恢复错误日志使用 `everything unavailable, fallback to walker`；等待日志使用 `waiting for everything index`；就绪日志使用 `everything enumerator ready`。

- [ ] **Step 5: 运行 GREEN 和 Agent 回归测试**

Run:

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 .\internal\enum .\cmd\agent
```

Expected: PASS，无 goroutine 泄漏或固定时长等待。

- [ ] **Step 6: 提交 Agent 接入**

```powershell
git add -- internal/enum/everything_process_windows.go internal/enum/everything_process_windows_test.go cmd/agent/main.go cmd/agent/main_test.go
git commit -m "feat: auto-start Everything for scans"
```

---

### Task 4: 构建、打包和最终验证

**Files:**
- Verify: `scripts/build.ps1`
- Verify: `scripts/package-node-release.ps1`
- Verify: `artifacts/everything-autostart-stage-20260811/`
- Verify: `artifacts/releases/*.zip`

**Interfaces:**
- Consumes: Tasks 1-3 的发布契约和 Agent 行为。
- Produces: 新鲜 Windows x64 阶段目录、节点发布 ZIP、SHA-256 sidecar 和验证结果。

- [ ] **Step 1: 运行格式与静态检查**

```powershell
& 'C:\tmp\go1.26.5\go\bin\gofmt.exe' -w internal\enum\autostart.go internal\enum\autostart_test.go internal\enum\everything_windows.go internal\enum\everything_process_windows.go internal\enum\everything_process_windows_test.go cmd\agent\main.go cmd\agent\main_test.go
git diff --check
```

- [ ] **Step 2: 运行聚焦及全仓串行 Go 门禁**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 .\internal\enum .\cmd\agent
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 .\...
& 'C:\tmp\go1.26.5\go\bin\go.exe' vet .\internal\enum .\cmd\agent
```

- [ ] **Step 3: 运行 PowerShell 发布契约**

```powershell
& 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -File .\scripts\test-package-node-release.ps1
& 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -File .\scripts\test-node-tray-supply-chain.ps1
```

- [ ] **Step 4: 生成新阶段目录和节点 ZIP**

使用下列命令生成固定的新阶段目录；执行前该目录必须不存在。当前已确认 Go、CMake、vcpkg 和 npm 可用；如果固定的 MinGW `gcc.exe`、`windres.exe`、`dlltool.exe` 已被外部清理，则将完整构建明确记录为 `BLOCKED_TOOLCHAIN_MISSING`，不得下载未固定工具链或复用旧阶段目录冒充新构建。

```powershell
& .\scripts\build.ps1 `
  -Go 'C:\tmp\go1.26.5\go\bin\go.exe' `
  -CC 'C:\Users\Administrator\AppData\Local\Temp\winlibs-gcc\mingw64\bin\gcc.exe' `
  -Windres 'C:\Users\Administrator\AppData\Local\Temp\winlibs-gcc\mingw64\bin\windres.exe' `
  -Dlltool 'C:\Users\Administrator\AppData\Local\Temp\winlibs-gcc\mingw64\bin\dlltool.exe' `
  -CMake 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\cmake.exe' `
  -VcpkgRoot 'C:\vcpkg' `
  -StageDir '.\artifacts\everything-autostart-stage-20260811'

& .\scripts\package-node-release.ps1 `
  -StageDir '.\artifacts\everything-autostart-stage-20260811' `
  -OutputDir '.\artifacts\releases' `
  -ReleaseId 'everything-autostart-20260811' `
  -BuildDate '2026-08-11' `
  -SourceRevision (git rev-parse HEAD)
```

检查 ZIP 中四个 Everything 文件、release manifest 哈希和 sidecar。

- [ ] **Step 5: 记录动态验收边界并提交收尾**

如果没有实际运行新发布目录中的 `agent.exe` 与 `Everything.exe` 完成首次索引和真实路径扫描，则把该项标记为 `PARTIAL/BLOCKED`，不得用单元测试代替。只提交本任务新增的验证文档或必要修正。

```powershell
git status --short
git log -4 --oneline
```
