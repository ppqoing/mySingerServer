# Helper 运行时隐藏 CMD 窗口实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**目标：** 将 Helper 构建为 Windows GUI 子系统程序，使所有启动方式都不创建 CMD 窗口，同时保留 UAC、管理员 manifest、控制握手和既有配置指纹修复。

**架构：** 只在 Helper 的官方构建链路中加入 Go 链接器参数 `-H=windowsgui`，不修改 Helper 服务代码或 NodeTray 启动器。构建合同测试固定参数存在，实际二进制测试解析 PE Header 固定 Machine=`0x8664`、Subsystem=`2`，并继续验证 `requireAdministrator` manifest。

**技术栈：** Go 1.26.5、Go `testing`、PowerShell 7、GNU `windres`、Windows PE/COFF、Windows Manifest Tool (`mt.exe`)

## Global Constraints

- 所有 Helper 启动方式都不显示 CMD；UAC 确认弹窗必须保留。
- 只对 Helper 使用 `-ldflags=-H=windowsgui`；Agent、GUI、Worker、NodeTray 构建参数不变。
- 不修改 `cmd/helper/main.go`、NodeTray `ShellExecuteExW`、控制握手、配置字段、配置指纹、日志路径或生命周期。
- 不修改两个既有 `OutDir` 构建测试夹具，也不为其新增生产构建模式或重构 `scripts/build.ps1`；其既有失败按 `KNOWN_FAIL_OUT_OF_SCOPE` 记录。
- 候选产物只写入 `artifacts/helper-hidden-console/helper.exe`，不覆盖既有产物或 `C:\Program Files\MySingerServer\helper.exe`。
- 不部署、不启停任何已安装进程；动态验收保持 `BLOCKED_NOT_RUN_DYNAMIC`。
- 当前 checkout 没有 Git 元数据，不初始化 Git；所有版本/提交状态记录为 `N/A_NO_GIT_METADATA`。
- 所有测试使用 `-count=1`。任一必需静态门禁失败时如实记录，不得写成 PASS。

## 文件映射

- 修改 `integration/videocore_build_test.go`
  - 把 Helper GUI 子系统链接参数加入官方构建静态合同及破坏性 mutation 测试。
- 修改 `scripts/build.ps1`
  - 只为 Helper 构建命令增加 `"-ldflags=-H=windowsgui"`。
- 修改 `cmd/helper/main_test.go`
  - 增加 PE Machine/Subsystem 解析辅助函数。
  - 让 manifest 合同产物使用 GUI 链接参数并断言 Machine=`0x8664`、Subsystem=`2`。
- 生成 `artifacts/helper-hidden-console/helper.exe`
  - 新的 Windows x64、`WINDOWS_GUI`、`requireAdministrator` 候选产物。

---

### Task 1: 固定并实施官方 Helper GUI 子系统构建合同

**Files:**

- Modify: `integration/videocore_build_test.go:277-392`
- Modify: `scripts/build.ps1:329-339`

**Interfaces:**

- Consumes: `validateVideoCoreBuildContract(source string) error` 对官方构建脚本执行唯一标记和顺序检查。
- Produces: 官方 Helper 构建命令包含唯一的 `"-ldflags=-H=windowsgui"`，供 Task 2 的实际 PE 断言和 Task 4 的候选产物构建采用。

- [ ] **Step 1: 先扩展构建静态合同测试**

在 `TestVideoCoreBuildStaticContract` 的 `mutations` 中加入：

```go
{
	name: "helper GUI subsystem linker flag removed",
	source: strings.Replace(
		source,
		`"-ldflags=-H=windowsgui"`,
		`"-ldflags="`,
		1,
	),
},
```

在 `validateVideoCoreBuildContract` 的 `ordered` 列表中，将以下检查放在 `Helper target` 之前：

```go
{label: "Helper GUI subsystem linker flag", marker: `"-ldflags=-H=windowsgui"`},
{label: "Helper target", marker: `./cmd/helper`},
```

该合同要求链接参数在脚本中恰好出现一次，且位于 Helper target 之前。

- [ ] **Step 2: 运行静态合同测试并确认 RED**

```powershell
$env:GOTOOLCHAIN = 'local'
$env:GOCACHE = Join-Path $env:TEMP 'helper-hidden-console-gocache'
$env:GOTMPDIR = Join-Path $env:TEMP 'helper-hidden-console-gotmp'
New-Item -ItemType Directory -Force -Path $env:GOCACHE, $env:GOTMPDIR | Out-Null
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./integration -run '^TestVideoCoreBuildStaticContract$'
```

Expected: FAIL，错误包含 `Helper GUI subsystem linker flag marker count=0, want 1`；生产脚本此时尚未包含该参数。

- [ ] **Step 3: 对官方 Helper 构建实施最小修改**

将 `scripts/build.ps1` 中：

```powershell
& $Go -C $repo build -trimpath -o (Join-Path $out "helper.exe") ./cmd/helper
```

替换为：

```powershell
& $Go -C $repo build -trimpath "-ldflags=-H=windowsgui" `
    -o (Join-Path $out "helper.exe") ./cmd/helper
```

保留现有 `$LASTEXITCODE` 判断、manifest 资源生成和 `.syso` 清理逻辑。

- [ ] **Step 4: 格式化 Go 测试并确认 GREEN**

```powershell
& 'C:\tmp\go1.26.5\go\bin\gofmt.exe' -w 'integration\videocore_build_test.go'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./integration -run '^TestVideoCoreBuildStaticContract$'
```

Expected: PASS；包括“删除 GUI linker flag”的 mutation 在内，所有 mutation 都被合同拒绝。

- [ ] **Step 5: 只读复核修改范围**

```powershell
rg -n -C 6 'ldflags=-H=windowsgui|Helper GUI subsystem linker flag|./cmd/helper' `
    'scripts\build.ps1' 'integration\videocore_build_test.go'
```

Expected: 链接参数只存在于 Helper 构建和对应测试合同；Agent、GUI、Worker 命令没有该参数。版本记录：`N/A_NO_GIT_METADATA`。

---

### Task 2: 用实际 PE Header 固定 x64 GUI 子系统与管理员 manifest

**Files:**

- Modify: `cmd/helper/main_test.go:3-24`
- Modify: `cmd/helper/main_test.go:378-444`

**Interfaces:**

- Consumes: Task 1 确定的 `-ldflags=-H=windowsgui` 构建合同和现有 `helper.rc`/`helper.manifest`。
- Produces: `readPEMachineAndSubsystem(path string) (machine uint16, subsystem uint16, err error)`，以及对真实 Helper PE 的 Machine=`0x8664`、Subsystem=`2` 断言。

- [ ] **Step 1: 引入二进制小端读取依赖**

在 `cmd/helper/main_test.go` import 中加入：

```go
"encoding/binary"
```

- [ ] **Step 2: 增加安全的 PE Machine/Subsystem 解析辅助函数**

在 `TestManifestContract` 之前加入：

```go
func readPEMachineAndSubsystem(path string) (uint16, uint16, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	if len(data) < 0x40 || binary.LittleEndian.Uint16(data[0:2]) != 0x5a4d {
		return 0, 0, errors.New("invalid PE DOS header")
	}
	peOffset := int(binary.LittleEndian.Uint32(data[0x3c:0x40]))
	const optionalHeaderDelta = 24
	const subsystemDelta = 68
	if peOffset < 0x40 || peOffset > len(data)-(optionalHeaderDelta+subsystemDelta+2) {
		return 0, 0, errors.New("invalid PE header offset")
	}
	if binary.LittleEndian.Uint32(data[peOffset:peOffset+4]) != 0x00004550 {
		return 0, 0, errors.New("invalid PE signature")
	}
	optionalHeader := peOffset + optionalHeaderDelta
	if binary.LittleEndian.Uint16(data[optionalHeader:optionalHeader+2]) != 0x020b {
		return 0, 0, errors.New("Helper is not PE32+")
	}
	machine := binary.LittleEndian.Uint16(data[peOffset+4 : peOffset+6])
	subsystem := binary.LittleEndian.Uint16(
		data[optionalHeader+subsystemDelta : optionalHeader+subsystemDelta+2],
	)
	return machine, subsystem, nil
}
```

- [ ] **Step 3: 先在现有 manifest 产物上加入 PE 断言**

在 `TestManifestContract` 完成 `helper.exe` 构建后、提取 manifest 前加入：

```go
machine, subsystem, err := readPEMachineAndSubsystem(exe)
if err != nil {
	t.Fatalf("read Helper PE contract: %v", err)
}
if machine != 0x8664 {
	t.Fatalf("Helper PE machine = %#x, want AMD64 0x8664", machine)
}
if subsystem != 2 {
	t.Fatalf("Helper PE subsystem = %d, want WINDOWS_GUI 2", subsystem)
}
```

此时暂时不要修改该测试中的 `go build` 参数。

- [ ] **Step 4: 运行 manifest 合同测试并确认 RED**

```powershell
$env:M5_WINDRES = 'C:\Users\Administrator\AppData\Local\Temp\winlibs-gcc\mingw64\bin\windres.exe'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./cmd/helper -run '^TestManifestContract$'
```

Expected: FAIL，错误为 `Helper PE subsystem = 3, want WINDOWS_GUI 2`；证明旧直接构建确实生成 `WINDOWS_CUI`。

- [ ] **Step 5: 让实际 manifest 合同产物使用 GUI 链接参数**

将测试中的直接构建命令：

```go
command := exec.Command(goExe, "-C", root, "build", "-trimpath", "-o", exe, "./cmd/helper")
```

替换为：

```go
command := exec.Command(
	goExe,
	"-C", root,
	"build", "-trimpath", "-ldflags=-H=windowsgui",
	"-o", exe,
	"./cmd/helper",
)
```

- [ ] **Step 6: 格式化并确认 PE 与 manifest 同时 GREEN**

```powershell
& 'C:\tmp\go1.26.5\go\bin\gofmt.exe' -w 'cmd\helper\main_test.go'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./cmd/helper -run '^TestManifestContract$'
```

Expected: PASS；同一个实际产物同时满足 AMD64、`WINDOWS_GUI` 和 `requireAdministrator/false`，且测试后没有遗留 `.syso`。

- [ ] **Step 7: 记录任务状态**

记录 RED/GREEN 命令及关键输出；不修改 `cmd/helper/main.go`。版本记录：`N/A_NO_GIT_METADATA`。

---

### Task 3: 记录既有构建夹具阻塞并确认零源码改动

**Files:**

- Verify: `cmd/helper/main_test.go`
- Verify: `scripts/build.ps1`
- Generate: `.superpowers/sdd/2026-08-04-helper-hidden-console/task-3-report.md`

**Interfaces:**

- Consumes: 两个既有夹具的 RED 输出、`scripts/build.ps1` 的 `-MediacoreOnly` 早退控制流和 Task 2 后源码快照。
- Produces: 用户确认的范围修订证据；Task 3 不保留源码改动，不阻塞本功能的直接合同测试和候选产物。

- [ ] **Step 1: 记录两个既有 RED**

```powershell
$env:M5_CC = 'C:\Users\Administrator\AppData\Local\Temp\winlibs-gcc\mingw64\bin\gcc.exe'
$env:M5_WINDRES = 'C:\Users\Administrator\AppData\Local\Temp\winlibs-gcc\mingw64\bin\windres.exe'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./cmd/helper `
    -run '^(TestBuildScriptPackagesHelperWithoutOverwritingOperatorConfig|TestBuildScriptFailsClosedWhenExactResourceCleanupFails)$'
```

Expected: FAIL；第一个错误包含 `VIDEOCORE_STAGE_REQUIRED`，第二个因同一提前退出而报告 `.syso` 不存在。

- [ ] **Step 2: 验证原计划假设无效**

只读确认 `scripts/build.ps1` 的 `-MediacoreOnly` 分支在 Helper 构建前执行 `return`。因此给两个测试追加该参数无法满足其 Helper 打包或 fake `windres` 断言。不得为本功能新增 `ApplicationsOnly` 等生产接口，也不得重构构建脚本。

- [ ] **Step 3: 确认临时修改已完全回滚**

```powershell
rg -n -C 8 'TestBuildScriptPackagesHelperWithoutOverwritingOperatorConfig|TestBuildScriptFailsClosedWhenExactResourceCleanupFails|-MediacoreOnly' `
    'cmd\helper\main_test.go' 'scripts\build.ps1'
Get-FileHash -Algorithm SHA256 -LiteralPath 'cmd\helper\main_test.go'
Test-Path -LiteralPath 'cmd\helper\rsrc_windows_amd64.syso'
```

Expected: `main_test.go` 未给两个测试新增 `-MediacoreOnly`；SHA-256 与 Task 2 快照 `6B8CBA4B0E6D04B37829679FD92F5D960F3D28F77F158ED8275F03BAA7A057E8` 一致；无残留 `.syso`。生产默认仍为 `$VideoCoreOnly -or (-not $MediacoreOnly)`。

- [ ] **Step 4: 记录范围修订**

将两个既有失败标记为 `KNOWN_FAIL_OUT_OF_SCOPE`。Task 3 经独立复核后以“证据和回滚完成、零源码改动”结束；版本记录为 `N/A_NO_GIT_METADATA`。

---

### Task 4: 完整回归、独立候选产物与最终交付边界

**Files:**

- Verify: `scripts/build.ps1`
- Verify: `integration/videocore_build_test.go`
- Verify: `cmd/helper/main_test.go`
- Generate: `artifacts/helper-hidden-console/helper.exe`
- Temporary then remove: `cmd/helper/rsrc_windows_amd64.syso`

**Interfaces:**

- Consumes: Task 1 的官方构建合同、Task 2 的 PE/manifest 合同、Task 3 确认的既有夹具范围边界。
- Produces: 一个未部署的 Windows x64 `WINDOWS_GUI` Helper 候选产物和可审计的静态验收记录。

- [ ] **Step 1: 固定最终验证环境**

```powershell
$repo = (Resolve-Path '.').Path
$go = 'C:\tmp\go1.26.5\go\bin\go.exe'
$windres = 'C:\Users\Administrator\AppData\Local\Temp\winlibs-gcc\mingw64\bin\windres.exe'
$env:GOTOOLCHAIN = 'local'
$env:GOCACHE = Join-Path $env:TEMP 'helper-hidden-console-gocache'
$env:GOTMPDIR = Join-Path $env:TEMP 'helper-hidden-console-gotmp'
$env:M5_CC = 'C:\Users\Administrator\AppData\Local\Temp\winlibs-gcc\mingw64\bin\gcc.exe'
$env:M5_WINDRES = $windres
New-Item -ItemType Directory -Force -Path $env:GOCACHE, $env:GOTMPDIR | Out-Null
```

- [ ] **Step 2: 运行直接相关测试集**

```powershell
& $go test -count=1 ./integration -run '^TestVideoCoreBuildStaticContract$'
if ($LASTEXITCODE -ne 0) { throw 'build contract test failed' }
& $go test -count=1 ./cmd/helper `
    -run '^(TestManifestContract|TestEffectiveHelperConfigSHA256MatchesNodeTrayCanonicalJSON|TestRunWithControlShutdownCancelsDeleteServerAndPublishesIdentity)$'
if ($LASTEXITCODE -ne 0) { throw 'targeted Helper tests failed' }
```

Expected: 两条命令均退出 `0`，无残留 `.syso`。

- [ ] **Step 3: 运行设计要求的完整相关门禁**

```powershell
& $go test -count=1 ./cmd/helper ./integration ./internal/nodetray/... ./nodetray
```

Expected: 执行并保留完整输出。两个既有 `OutDir` 夹具以已确认方式失败时标记为 `KNOWN_FAIL_OUT_OF_SCOPE`，不是本功能直接合同失败；任何其他失败必须按实际状态标记 `FAIL`/`BLOCKED`，不得写成 PASS。

- [ ] **Step 4: 拒绝覆盖并准备独立产物目录**

```powershell
$helperDir = (Resolve-Path 'cmd\helper').Path
$helperResource = Join-Path $helperDir 'rsrc_windows_amd64.syso'
$artifactDir = Join-Path $repo 'artifacts\helper-hidden-console'
$helperExe = Join-Path $artifactDir 'helper.exe'
if (Test-Path -LiteralPath $helperResource) { throw "refusing to overwrite existing resource: $helperResource" }
if (Test-Path -LiteralPath $artifactDir) { throw "refusing to overwrite existing artifact directory: $artifactDir" }
New-Item -ItemType Directory -Path $artifactDir | Out-Null
```

- [ ] **Step 5: 构建带管理员 manifest 的 x64 GUI Helper**

```powershell
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'
$env:CGO_ENABLED = '0'
try {
    Push-Location -LiteralPath $helperDir
    try {
        & $windres -i 'helper.rc' -O coff -o $helperResource
        if ($LASTEXITCODE -ne 0) { throw 'helper manifest resource generation failed' }
    }
    finally {
        Pop-Location
    }
    & $go -C $repo build -trimpath '-ldflags=-H=windowsgui' -o $helperExe ./cmd/helper
    if ($LASTEXITCODE -ne 0) { throw 'helper build failed' }
}
finally {
    if (Test-Path -LiteralPath $helperResource) {
        Remove-Item -LiteralPath $helperResource -Force
    }
}
```

- [ ] **Step 6: 验证 PE Machine、PE32+ 和 WINDOWS_GUI Subsystem**

```powershell
$stream = [System.IO.File]::OpenRead($helperExe)
$reader = [System.IO.BinaryReader]::new($stream)
try {
    if ($reader.ReadUInt16() -ne 0x5A4D) { throw 'invalid DOS signature' }
    $stream.Position = 0x3c
    $peOffset = $reader.ReadInt32()
    $stream.Position = $peOffset
    if ($reader.ReadUInt32() -ne 0x00004550) { throw 'invalid PE signature' }
    $machine = $reader.ReadUInt16()
    if ($machine -ne 0x8664) { throw ('unexpected PE machine: 0x{0:X4}' -f $machine) }
    $optionalHeader = $peOffset + 24
    $stream.Position = $optionalHeader
    if ($reader.ReadUInt16() -ne 0x020B) { throw 'Helper is not PE32+' }
    $stream.Position = $optionalHeader + 68
    $subsystem = $reader.ReadUInt16()
    if ($subsystem -ne 2) { throw "unexpected PE subsystem: $subsystem" }
}
finally {
    $reader.Dispose()
    $stream.Dispose()
}
```

Expected: Machine=`0x8664`，Optional Header=`0x020B`，Subsystem=`2`。

- [ ] **Step 7: 验证管理员 manifest 并记录产物证据**

```powershell
$mt = 'C:\Program Files (x86)\Windows Kits\10\bin\10.0.26100.0\x64\mt.exe'
$manifestOut = Join-Path $env:TEMP 'helper-hidden-console.manifest'
try {
    & $mt '-nologo' "-inputresource:$helperExe;#1" "-out:$manifestOut"
    if ($LASTEXITCODE -ne 0) { throw 'extract helper manifest failed' }
    if (-not (Select-String -LiteralPath $manifestOut `
            -Pattern 'requestedExecutionLevel\s+level="requireAdministrator"' -Quiet)) {
        throw 'requireAdministrator manifest not found'
    }
}
finally {
    if (Test-Path -LiteralPath $manifestOut) {
        Remove-Item -LiteralPath $manifestOut -Force
    }
}
Get-Item -LiteralPath $helperExe | Select-Object FullName, Length, LastWriteTime
Get-FileHash -Algorithm SHA256 -LiteralPath $helperExe
Get-AuthenticodeSignature -LiteralPath $helperExe | Select-Object Status, StatusMessage
```

记录绝对路径、大小、时间、SHA-256 和实际签名状态；`NotSigned` 不得描述为已签名。

- [ ] **Step 8: 最终边界报告**

最终报告必须分开列出：

- `PASS`：实际通过的构建合同、PE、manifest、指纹和 NodeTray 静态测试。
- `KNOWN_FAIL_OUT_OF_SCOPE`：两个既有 `OutDir` 构建夹具的已确认失败；不得描述为已修复或已通过。
- `FAIL`/`BLOCKED`：任何未通过的必需静态门禁。
- `BLOCKED_NOT_RUN_DYNAMIC`：没有替换安装目录、没有从 NodeTray 或手动启动候选 Helper，不能声称真实窗口行为和握手已动态通过。
- `N/A_NO_GIT_METADATA`：无提交、分支或合并状态。

不得复制到 `C:\Program Files\MySingerServer`，不得启停 `nodetray`、`agent`、`worker` 或 `helper`。

---

### Task 5: 修复终审发现的构建合同绕过与 PE 格式校验

**Files:**

- Modify: `integration/videocore_build_test.go`
- Modify: `cmd/helper/main_test.go`
- Verify: `artifacts/helper-hidden-console/helper.exe`

**Interfaces:**

- Consumes: 终审报告 `.superpowers/sdd/2026-08-04-helper-hidden-console/final-review.md` 的 Important 1 和 Minor 1。
- Produces: 静态合同把 `-H=windowsgui` 绑定到实际 Helper `go build` 逻辑命令；PE 解析器拒绝声明过短或越过文件边界的 Optional Header。

- [ ] **Step 1: 先加入可复现构建合同绕过的 mutation**

新增 mutation：把 `"-ldflags=-H=windowsgui"` 移到未使用的 `$helperGUIFlag` 变量，同时从实际 Helper 构建命令删除该参数。修改生产校验前运行：

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./integration -run '^TestVideoCoreBuildStaticContract$'
```

Expected: FAIL；新 mutation 被当前 validator 错误接受。

- [ ] **Step 2: 把 flag 绑定到完整 Helper 构建逻辑命令**

让 `validateVideoCoreBuildContract` 验证同一 PowerShell 逻辑命令同时包含：

```text
& $Go -C $repo build -trimpath "-ldflags=-H=windowsgui"
-o (Join-Path $out "helper.exe") ./cmd/helper
```

只保留字符串或把 flag 移入注释/未使用变量必须被拒绝。生产 `scripts/build.ps1` 不再修改。

- [ ] **Step 3: 先加入 Optional Header 大小 RED 测试**

构造声明 `SizeOfOptionalHeader=0` 和声明大小越过文件尾的畸形 PE；当前解析器会错误接受固定偏移字段，测试应先失败。

- [ ] **Step 4: 校验 COFF SizeOfOptionalHeader**

`readPEMachineAndSubsystem` 必须要求声明大小至少覆盖 Subsystem 字段（`68+2`），且整个声明的 Optional Header 位于文件范围内；保留现有 DOS、PE、PE32+、Machine 和 Subsystem 断言。

- [ ] **Step 5: 格式化并运行修复验证**

```powershell
& 'C:\tmp\go1.26.5\go\bin\gofmt.exe' -w 'integration\videocore_build_test.go' 'cmd\helper\main_test.go'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./integration -run '^TestVideoCoreBuildStaticContract$'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./cmd/helper -run '^(TestManifestContract|TestReadPEMachineAndSubsystemRejectsInvalidOptionalHeaderSize)$'
```

Expected: 两条测试命令均退出 `0`，无 `.syso` 残留。

- [ ] **Step 6: 保持候选和交付边界**

Task 5 只修改测试合同，不重建或覆盖现有候选。确认候选 SHA-256 仍为 `FC19E5B3C9ADF58234723D93389929D8873A328A3D8808CBC9B14FE3C57EF564`；动态验收仍为 `BLOCKED_NOT_RUN_DYNAMIC`，版本为 `N/A_NO_GIT_METADATA`。
