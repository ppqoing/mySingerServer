# Go、npm、CMake 标准依赖与缓存目录 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 清理仓库内可再生成的构建缓存，并让所有主要构建入口验证和复用 Go、npm、CMake/vcpkg 的 Windows 标准依赖路径。

**Architecture:** 新增一个只负责解析和校验工具链标准路径的 PowerShell 函数文件，由 Go、前端和原生构建入口按需调用；新增一个具备明确白名单、试运行模式和路径边界校验的清理脚本。先以测试锁定路径解析、入口接线和删除边界，再填充标准缓存、运行代表性构建，最后执行清理并记录空间变化。

**Tech Stack:** PowerShell 7、Go 1.26.5、npm、CMake、vcpkg、Git、Windows NTFS

## Global Constraints

- 不建立 `mySingerServer` 专属公共缓存根目录，不修改系统级环境变量。
- Go 路径必须来自 `go env GOPATH GOMODCACHE GOCACHE`。
- npm 下载缓存必须来自 `npm config get cache`；应用依赖继续使用项目级 `node_modules`。
- vcpkg 默认根目录保持 `C:\vcpkg`，允许现有 `-VcpkgRoot` 参数覆盖。
- 不安装、升级或修改 Go、npm、CMake、vcpkg 以及系统 `PATH`。
- FFmpeg、Everything、Everything SDK、WebView2 固定版本二进制及其供应链语义保持不变。
- 不删除 `artifacts/releases`、`.superpowers/evidence`、`.worktrees`、Docker/PostgreSQL 数据、`third_party`、Git 已跟踪文件或用户未提交文件。
- 当前 `D:\code\mySingerServer` 含用户未提交文档；只暂存计划明确列出的文件，不使用 `git add -A`。
- Windows 下 Go 验证顺序执行，完整仓库门禁使用 `go test -p=1 -count=1 ./...`。

---

### Task 1: 标准依赖路径解析器

**Files:**
- Create: `scripts/standard-dependency-paths.ps1`
- Create: `scripts/test-standard-dependency-paths.ps1`

**Interfaces:**
- Produces: `Resolve-StandardDependencyPaths -RepositoryRoot <string> [-GoExecutable <string>] [-NpmExecutable <string>] [-VcpkgRoot <string>] -> PSCustomObject`
- Produces properties: `GoPath`, `GoModCache`, `GoBuildCache`, `NpmCache`, `VcpkgRoot`, `VcpkgInstalled`, `VcpkgDownloads`
- Throws: `DEPENDENCY_PATH_NOT_ABSOLUTE`、`DEPENDENCY_PATH_INSIDE_REPOSITORY`、`GO_STANDARD_PATH_RESOLVE_FAILED`、`NPM_STANDARD_PATH_RESOLVE_FAILED`、`VCPKG_STANDARD_PATH_MISSING`

- [ ] **Step 1: 写出标准路径与仓库内路径拒绝测试**

在 `scripts/test-standard-dependency-paths.ps1` 中创建独立临时仓库、假的 Go/npm 可执行文件以及假的 vcpkg 标准目录。测试必须覆盖正常解析与仓库内缓存拒绝：

```powershell
param()
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'standard-dependency-paths.ps1')

function Assert-Equal($Actual, $Expected, $Label) {
    if ($Actual -cne $Expected) {
        throw "ASSERT_EQUAL_FAILED label=$Label actual=$Actual expected=$Expected"
    }
}

$fixture = Join-Path ([IO.Path]::GetTempPath()) `
    ('mysinger-standard-paths-' + [Guid]::NewGuid().ToString('N'))
$repo = Join-Path $fixture 'repository'
$shared = Join-Path $fixture 'shared'
$vcpkg = Join-Path $shared 'vcpkg'
try {
    New-Item -ItemType Directory -Force -Path $repo | Out-Null
    New-Item -ItemType Directory -Force -Path `
        (Join-Path $shared 'go'), `
        (Join-Path $shared 'go-mod'), `
        (Join-Path $shared 'go-build'), `
        (Join-Path $shared 'npm-cache'), `
        (Join-Path $vcpkg 'installed'), `
        (Join-Path $vcpkg 'downloads'), `
        (Join-Path $vcpkg 'scripts\buildsystems') | Out-Null
    Set-Content -LiteralPath (Join-Path $vcpkg 'vcpkg.exe') -Value 'fixture'
    Set-Content -LiteralPath `
        (Join-Path $vcpkg 'scripts\buildsystems\vcpkg.cmake') -Value 'fixture'

    $go = Join-Path $fixture 'go.cmd'
    $npm = Join-Path $fixture 'npm.cmd'
    Set-Content -LiteralPath $go -Value @(
        '@echo off',
        ('echo ' + (Join-Path $shared 'go')),
        ('echo ' + (Join-Path $shared 'go-mod')),
        ('echo ' + (Join-Path $shared 'go-build'))
    )
    Set-Content -LiteralPath $npm -Value @(
        '@echo off',
        ('echo ' + (Join-Path $shared 'npm-cache'))
    )

    $actual = Resolve-StandardDependencyPaths -RepositoryRoot $repo `
        -GoExecutable $go -NpmExecutable $npm -VcpkgRoot $vcpkg
    Assert-Equal $actual.GoModCache (Join-Path $shared 'go-mod') 'GOMODCACHE'
    Assert-Equal $actual.NpmCache (Join-Path $shared 'npm-cache') 'npm cache'
    Assert-Equal $actual.VcpkgInstalled (Join-Path $vcpkg 'installed') 'vcpkg installed'

    Set-Content -LiteralPath $go -Value @(
        '@echo off',
        ('echo ' + (Join-Path $shared 'go')),
        ('echo ' + (Join-Path $repo '.tmp\gomodcache')),
        ('echo ' + (Join-Path $shared 'go-build'))
    )
    try {
        Resolve-StandardDependencyPaths -RepositoryRoot $repo `
            -GoExecutable $go | Out-Null
        throw 'EXPECTED_REPOSITORY_CACHE_REJECTION'
    } catch {
        if ($_.Exception.Message -notmatch 'DEPENDENCY_PATH_INSIDE_REPOSITORY') {
            throw
        }
    }
} finally {
    if (Test-Path -LiteralPath $fixture) {
        Remove-Item -LiteralPath $fixture -Recurse -Force
    }
}
Write-Output 'STANDARD DEPENDENCY PATH TEST PASS'
```

- [ ] **Step 2: 运行测试并确认红灯**

Run:

```powershell
pwsh -NoProfile -File scripts/test-standard-dependency-paths.ps1
```

Expected: FAIL，错误指出 `scripts/standard-dependency-paths.ps1` 不存在。

- [ ] **Step 3: 实现最小路径解析与边界校验**

在 `scripts/standard-dependency-paths.ps1` 中实现：

```powershell
$ErrorActionPreference = 'Stop'

function Resolve-AbsoluteDependencyPath {
    param([string]$Path, [string]$Label, [string]$RepositoryRoot)
    if ([string]::IsNullOrWhiteSpace($Path) -or
        -not [IO.Path]::IsPathRooted($Path)) {
        throw "DEPENDENCY_PATH_NOT_ABSOLUTE label=$Label path=$Path"
    }
    $resolved = [IO.Path]::GetFullPath($Path).TrimEnd('\')
    $repo = [IO.Path]::GetFullPath($RepositoryRoot).TrimEnd('\')
    if ($resolved -eq $repo -or $resolved.StartsWith(
            $repo + '\', [StringComparison]::OrdinalIgnoreCase)) {
        throw "DEPENDENCY_PATH_INSIDE_REPOSITORY label=$Label path=$resolved"
    }
    return $resolved
}

function Resolve-StandardDependencyPaths {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$RepositoryRoot,
        [string]$GoExecutable = '',
        [string]$NpmExecutable = '',
        [string]$VcpkgRoot = ''
    )
    $result = [ordered]@{
        GoPath = $null; GoModCache = $null; GoBuildCache = $null
        NpmCache = $null
        VcpkgRoot = $null; VcpkgInstalled = $null; VcpkgDownloads = $null
    }
    if ($GoExecutable) {
        $goPaths = @(& $GoExecutable env GOPATH GOMODCACHE GOCACHE 2>&1 |
            ForEach-Object { ([string]$_).Trim() } | Where-Object { $_ })
        if ($LASTEXITCODE -ne 0 -or $goPaths.Count -ne 3) {
            throw 'GO_STANDARD_PATH_RESOLVE_FAILED'
        }
        $result.GoPath = Resolve-AbsoluteDependencyPath $goPaths[0] 'GOPATH' $RepositoryRoot
        $result.GoModCache = Resolve-AbsoluteDependencyPath $goPaths[1] 'GOMODCACHE' $RepositoryRoot
        $result.GoBuildCache = Resolve-AbsoluteDependencyPath $goPaths[2] 'GOCACHE' $RepositoryRoot
    }
    if ($NpmExecutable) {
        $npmCache = (& $NpmExecutable config get cache 2>&1 | Out-String).Trim()
        if ($LASTEXITCODE -ne 0 -or -not $npmCache) {
            throw 'NPM_STANDARD_PATH_RESOLVE_FAILED'
        }
        $result.NpmCache = Resolve-AbsoluteDependencyPath $npmCache 'npm-cache' $RepositoryRoot
    }
    if ($VcpkgRoot) {
        $root = Resolve-AbsoluteDependencyPath $VcpkgRoot 'vcpkg-root' $RepositoryRoot
        $required = @(
            (Join-Path $root 'vcpkg.exe'),
            (Join-Path $root 'scripts\buildsystems\vcpkg.cmake'),
            (Join-Path $root 'installed'),
            (Join-Path $root 'downloads')
        )
        foreach ($path in $required) {
            if (-not (Test-Path -LiteralPath $path)) {
                throw "VCPKG_STANDARD_PATH_MISSING path=$path"
            }
        }
        $result.VcpkgRoot = $root
        $result.VcpkgInstalled = Join-Path $root 'installed'
        $result.VcpkgDownloads = Join-Path $root 'downloads'
    }
    return [pscustomobject]$result
}
```

- [ ] **Step 4: 运行测试并确认绿灯**

Run:

```powershell
pwsh -NoProfile -File scripts/test-standard-dependency-paths.ps1
```

Expected: `STANDARD DEPENDENCY PATH TEST PASS`。

- [ ] **Step 5: 提交路径解析器**

```powershell
git add -- scripts/standard-dependency-paths.ps1 scripts/test-standard-dependency-paths.ps1
git diff --cached --check
git commit -m "build: validate standard dependency paths"
```

---

### Task 2: 将标准路径校验接入主要构建入口

**Files:**
- Modify: `scripts/test-standard-dependency-paths.ps1`
- Modify: `scripts/build.ps1:16-18`
- Modify: `scripts/build-web.ps1:283-303`
- Modify: `scripts/build-nodetray.ps1:299-340`
- Modify: `scripts/test-cgo.ps1:12-18`

**Interfaces:**
- Consumes: Task 1 的 `Resolve-StandardDependencyPaths`
- Produces: 四个构建入口在执行构建前验证所使用的标准依赖路径
- Preserves: `scripts/build-nodetray.ps1` 继续从已验证的 `GoModCache` 定位 Wails 内置 WebView2 文件

- [ ] **Step 1: 为四个入口写静态接线失败测试**

在 `scripts/test-standard-dependency-paths.ps1` 的成功路径测试之后追加：

```powershell
$repoRoot = Split-Path -Parent $PSScriptRoot
$contracts = @(
    @{ File = 'build.ps1'; Tokens = @(
        "standard-dependency-paths.ps1",
        'Resolve-StandardDependencyPaths',
        '-GoExecutable $Go',
        '-VcpkgRoot $VcpkgRoot'
    ) },
    @{ File = 'build-web.ps1'; Tokens = @(
        "standard-dependency-paths.ps1",
        'Resolve-StandardDependencyPaths',
        '-NpmExecutable $npm'
    ) },
    @{ File = 'build-nodetray.ps1'; Tokens = @(
        "standard-dependency-paths.ps1",
        '-GoExecutable $goExe',
        '-NpmExecutable $npmExe',
        '$dependencyPaths.GoModCache'
    ) },
    @{ File = 'test-cgo.ps1'; Tokens = @(
        "standard-dependency-paths.ps1",
        'Resolve-StandardDependencyPaths',
        '-GoExecutable $Go'
    ) }
)
foreach ($contract in $contracts) {
    $source = Get-Content -LiteralPath `
        (Join-Path $PSScriptRoot $contract.File) -Raw
    foreach ($token in $contract.Tokens) {
        if (-not $source.Contains($token)) {
            throw "STANDARD_PATH_WIRING_MISSING file=$($contract.File) token=$token"
        }
    }
}
```

- [ ] **Step 2: 运行接线测试并确认红灯**

Run:

```powershell
pwsh -NoProfile -File scripts/test-standard-dependency-paths.ps1
```

Expected: FAIL，首个错误为 `STANDARD_PATH_WIRING_MISSING`。

- [ ] **Step 3: 在构建入口解析工具后调用路径校验器**

四个脚本都使用以下点入方式，不复制解析逻辑：

```powershell
. (Join-Path $PSScriptRoot 'standard-dependency-paths.ps1')
```

在 `scripts/build.ps1` 中调用 Go 和 vcpkg 校验：

```powershell
$dependencyPaths = Resolve-StandardDependencyPaths `
    -RepositoryRoot $repo -GoExecutable $Go -VcpkgRoot $VcpkgRoot
Write-Host "Go module cache: $($dependencyPaths.GoModCache)"
Write-Host "Go build cache: $($dependencyPaths.GoBuildCache)"
Write-Host "vcpkg installed: $($dependencyPaths.VcpkgInstalled)"
```

在 `scripts/build-web.ps1` 解析 `$npm` 后调用：

```powershell
$dependencyPaths = Resolve-StandardDependencyPaths `
    -RepositoryRoot $repo -NpmExecutable $npm
Write-Host "npm cache: $($dependencyPaths.NpmCache)"
```

在 `scripts/build-nodetray.ps1` 解析 `$goExe`、`$npmExe` 后调用，并替换原有直接查询：

```powershell
$dependencyPaths = Resolve-StandardDependencyPaths `
    -RepositoryRoot $repo -GoExecutable $goExe -NpmExecutable $npmExe
$goModuleCache = $dependencyPaths.GoModCache
Write-Host "Go module cache: $goModuleCache"
Write-Host "Go build cache: $($dependencyPaths.GoBuildCache)"
Write-Host "npm cache: $($dependencyPaths.NpmCache)"
```

在 `scripts/test-cgo.ps1` 中调用 Go 校验，并继续继承调用者环境而不设置 `GOCACHE`、`GOMODCACHE` 或 `GOPATH`：

```powershell
$dependencyPaths = Resolve-StandardDependencyPaths `
    -RepositoryRoot $repo -GoExecutable $Go
Write-Host "Go module cache: $($dependencyPaths.GoModCache)"
Write-Host "Go build cache: $($dependencyPaths.GoBuildCache)"
```

- [ ] **Step 4: 运行接线测试和现有静态构建契约**

Run:

```powershell
pwsh -NoProfile -File scripts/test-standard-dependency-paths.ps1
$go = 'C:\tmp\go1.26.5\go\bin\go.exe'
& $go test -p=1 -count=1 ./integration -run 'TestVideoCoreBuildStaticContract|TestBuildScript'
```

Expected: PowerShell 测试打印 PASS；Go 目标测试 PASS。

- [ ] **Step 5: 确认仓库没有持久项目级工具缓存覆盖**

Run:

```powershell
git grep -n -I -E 'GOCACHE|GOMODCACHE|GOPATH|NPM_CONFIG_CACHE' -- `
    ':!docs/**' ':!scripts/test-standard-dependency-paths.ps1' `
    ':!scripts/standard-dependency-paths.ps1'
```

Expected: 只允许读取标准路径的业务代码；不得出现将这些变量赋值为 `.tmp`、`.codex-temp`、`.superpowers` 或 `artifacts` 的结果。

- [ ] **Step 6: 提交构建入口接线**

```powershell
git add -- scripts/build.ps1 scripts/build-web.ps1 `
    scripts/build-nodetray.ps1 scripts/test-cgo.ps1 `
    scripts/test-standard-dependency-paths.ps1
git diff --cached --check
git commit -m "build: use toolchain standard dependency caches"
```

---

### Task 3: 安全、可重复的仓库构建缓存清理器

**Files:**
- Create: `scripts/clean-build-cache.ps1`
- Create: `scripts/test-clean-build-cache.ps1`
- Modify: `.gitignore:1-8`

**Interfaces:**
- Produces: `scripts/clean-build-cache.ps1 [-RepositoryRoot <string>] [-Apply]`
- Default behavior: 只列出目标、文件数和逻辑字节数，不删除
- `-Apply`: 仅删除白名单目标，保留 `.codex-temp` 中未列入白名单的脚本、源码和工具目录
- Throws: `CACHE_TARGET_OUTSIDE_REPOSITORY`、`CACHE_TARGET_IS_REPOSITORY_ROOT`、`CACHE_TARGET_IN_USE`

- [ ] **Step 1: 写出试运行、删除与保护边界测试**

创建 `scripts/test-clean-build-cache.ps1`，构造以下夹具并验证行为：

```powershell
param()
$ErrorActionPreference = 'Stop'
$fixture = Join-Path ([IO.Path]::GetTempPath()) `
    ('mysinger-clean-cache-' + [Guid]::NewGuid().ToString('N'))
try {
    $removeDirs = @(
        '.tmp\go-cache', '.tmp-review\cache',
        '.codex-temp\gocache', '.codex-temp\stage-old',
        '.superpowers\tmp\cache', '.superpowers\runtime\old',
        'artifacts\.gocache-live-status\cache',
        'artifacts\.gocache-live-workerpool\cache',
        'videocore\build\Release', 'mediacore\build\Release'
    )
    $removeFiles = @('.codex-temp\old.stdout.log')
    $keepFiles = @(
        '.codex-temp\keep.ps1',
        '.codex-temp\internal\keep.go',
        '.codex-temp\protobuf-tools\keep.exe',
        '.superpowers\evidence\keep.bin',
        '.worktrees\keep\source.go',
        'artifacts\releases\keep.zip',
        'webui\node_modules\keep.js',
        'nodetray\frontend\node_modules\keep.js',
        'third_party\ffmpeg\keep.dll'
    )
    foreach ($dir in $removeDirs) {
        New-Item -ItemType Directory -Force -Path (Join-Path $fixture $dir) |
            Out-Null
        Set-Content -LiteralPath (Join-Path $fixture $dir 'cache.bin') `
            -Value 'cache'
    }
    foreach ($file in $keepFiles) {
        New-Item -ItemType Directory -Force -Path (Split-Path (Join-Path $fixture $file)) |
            Out-Null
        Set-Content -LiteralPath (Join-Path $fixture $file) -Value 'keep'
    }
    foreach ($file in $removeFiles) {
        New-Item -ItemType Directory -Force -Path (Split-Path (Join-Path $fixture $file)) |
            Out-Null
        Set-Content -LiteralPath (Join-Path $fixture $file) -Value 'log'
    }

    & (Join-Path $PSScriptRoot 'clean-build-cache.ps1') `
        -RepositoryRoot $fixture
    foreach ($dir in $removeDirs) {
        if (-not (Test-Path -LiteralPath (Join-Path $fixture $dir))) {
            throw "DRY_RUN_REMOVED_TARGET path=$dir"
        }
    }
    foreach ($file in $removeFiles) {
        if (-not (Test-Path -LiteralPath (Join-Path $fixture $file))) {
            throw "DRY_RUN_REMOVED_TARGET path=$file"
        }
    }

    & (Join-Path $PSScriptRoot 'clean-build-cache.ps1') `
        -RepositoryRoot $fixture -Apply
    foreach ($file in $keepFiles) {
        if (-not (Test-Path -LiteralPath (Join-Path $fixture $file))) {
            throw "PROTECTED_FILE_REMOVED path=$file"
        }
    }
    foreach ($dir in $removeDirs) {
        if (Test-Path -LiteralPath (Join-Path $fixture $dir)) {
            throw "CACHE_TARGET_REMAINS path=$dir"
        }
    }
    foreach ($file in $removeFiles) {
        if (Test-Path -LiteralPath (Join-Path $fixture $file)) {
            throw "CACHE_TARGET_REMAINS path=$file"
        }
    }
} finally {
    if (Test-Path -LiteralPath $fixture) {
        Remove-Item -LiteralPath $fixture -Recurse -Force
    }
}
Write-Output 'BUILD CACHE CLEANER TEST PASS'
```

- [ ] **Step 2: 运行清理测试并确认红灯**

Run:

```powershell
pwsh -NoProfile -File scripts/test-clean-build-cache.ps1
```

Expected: FAIL，错误指出 `scripts/clean-build-cache.ps1` 不存在。

- [ ] **Step 3: 实现白名单清理器**

`scripts/clean-build-cache.ps1` 必须使用已解析绝对路径和 `-LiteralPath`。固定目标为：

```powershell
$fixedRelativeTargets = @(
    '.tmp',
    '.superpowers\tmp',
    '.superpowers\runtime',
    'artifacts\.gocache-live-status',
    'artifacts\.gocache-live-workerpool',
    'videocore\build',
    'mediacore\build'
)
```

再加入两类经过相同边界检查的动态目标：

```powershell
# 仓库根目录下名字以 .tmp- 开头的直接子目录。
Get-ChildItem -LiteralPath $repo -Force -Directory |
    Where-Object { $_.Name.StartsWith('.tmp-', [StringComparison]::Ordinal) }

# .codex-temp 中名称明确属于缓存、测试或构建暂存的直接子目录。
$codexTempNames = @(
    'gocache-runtime-logs', 'gocache', 'go-cache',
    'everything-autostart', 'scan-progress-gocache',
    'gocache-cgo', 'gocache-cgo-final',
    'scan-progress-webui', 'testtmp'
)
$codexTempPrefixes = @(
    'stage-', 'central-cache-control-', 'scan-stop-control-',
    'scan-progress-agent-hotfix', 'everything-download-'
)
Get-ChildItem -LiteralPath (Join-Path $repo '.codex-temp') `
    -Force -Directory -ErrorAction SilentlyContinue |
    Where-Object {
        $codexTempNames -contains $_.Name -or
        @($codexTempPrefixes | Where-Object {
            $_.Name.StartsWith($_, [StringComparison]::OrdinalIgnoreCase)
        }).Count -gt 0
    }

# .codex-temp 根部已结束任务留下的日志文件。
Get-ChildItem -LiteralPath (Join-Path $repo '.codex-temp') `
    -Force -File -ErrorAction SilentlyContinue |
    Where-Object { $_.Extension -ieq '.log' }
```

每个候选目标必须通过：

```powershell
$repoPrefix = $repo.TrimEnd('\') + '\'
$target = [IO.Path]::GetFullPath($candidate).TrimEnd('\')
if ($target -eq $repo) { throw 'CACHE_TARGET_IS_REPOSITORY_ROOT' }
if (-not $target.StartsWith($repoPrefix,
        [StringComparison]::OrdinalIgnoreCase)) {
    throw "CACHE_TARGET_OUTSIDE_REPOSITORY path=$target"
}
```

删除前读取 `Win32_Process.CommandLine`，命令行包含具体候选目录时抛出 `CACHE_TARGET_IN_USE`；查询失败时停止，不跳过安全检查。试运行输出 `DRY-RUN`，`-Apply` 输出每个删除目标及最终释放的逻辑字节数。

- [ ] **Step 4: 将 `.codex-temp/` 加入忽略规则并运行测试**

在根 `.gitignore` 的临时目录区域加入：

```gitignore
/.codex-temp/
```

Run:

```powershell
pwsh -NoProfile -File scripts/test-clean-build-cache.ps1
git check-ignore -v .codex-temp
```

Expected: 测试打印 `BUILD CACHE CLEANER TEST PASS`；Git 输出根 `.gitignore` 中的对应规则。

- [ ] **Step 5: 对真实仓库执行试运行并核对保护目录**

Run:

```powershell
pwsh -NoProfile -File scripts/clean-build-cache.ps1
git status --short
```

Expected: 只列出允许清理的目标；`artifacts/releases`、`.superpowers/evidence`、`.worktrees`、两个 `node_modules` 和 `third_party` 不在列表中，Git 状态没有发生变化。

- [ ] **Step 6: 提交清理器与忽略规则**

```powershell
git add -- .gitignore scripts/clean-build-cache.ps1 `
    scripts/test-clean-build-cache.ps1
git diff --cached --check
git commit -m "build: add safe repository cache cleanup"
```

---

### Task 4: 填充标准依赖缓存并运行代表性构建

**Files:**
- Verify only: `go.mod`
- Verify only: `webui/package-lock.json`
- Verify only: `nodetray/frontend/package-lock.json`
- Verify only: `videocore/CMakeLists.txt`
- Verify only: `C:\vcpkg\installed`
- Verify only: `C:\vcpkg\downloads`

**Interfaces:**
- Consumes: Task 1 的路径解析器和 Task 2 的构建入口
- Produces: 标准 Go/npm/vcpkg 路径已实际使用的构建证据

- [ ] **Step 1: 记录清理前占用和当前 Git 边界**

Run:

```powershell
$root = (Resolve-Path '.').Path
$files = Get-ChildItem -LiteralPath $root -Force -File -Recurse `
    -ErrorAction SilentlyContinue
$bytes = ($files | Measure-Object Length -Sum).Sum
"BEFORE files=$($files.Count) gib=$([math]::Round($bytes / 1GB, 3))"
git status --short
```

Expected: 记录约 30 GiB 的当前基线；现有用户文档修改保持原样。

- [ ] **Step 2: 实时解析标准路径**

Run:

```powershell
$go = 'C:\tmp\go1.26.5\go\bin\go.exe'
. .\scripts\standard-dependency-paths.ps1
Resolve-StandardDependencyPaths -RepositoryRoot (Resolve-Path '.').Path `
    -GoExecutable $go -NpmExecutable 'npm' -VcpkgRoot 'C:\vcpkg' |
    Format-List
```

Expected:

```text
GoPath          : C:\Users\Administrator\go
GoModCache      : C:\Users\Administrator\go\pkg\mod
GoBuildCache    : C:\Users\Administrator\AppData\Local\go-build
NpmCache        : C:\Users\Administrator\AppData\Local\npm-cache
VcpkgRoot       : C:\vcpkg
VcpkgInstalled  : C:\vcpkg\installed
VcpkgDownloads  : C:\vcpkg\downloads
```

- [ ] **Step 3: 使用默认 Go 环境填充标准模块缓存**

Run:

```powershell
$go = 'C:\tmp\go1.26.5\go\bin\go.exe'
& $go -C (Resolve-Path '.').Path mod download
& $go env GOPATH GOMODCACHE GOCACHE
```

Expected: `go mod download` 退出码为 0；输出与 Step 2 的标准 Go 路径一致。不得设置 `GOCACHE`、`GOMODCACHE` 或 `GOPATH` 环境变量。

- [ ] **Step 4: 使用 npm 标准下载缓存验证两个锁文件**

Run:

```powershell
npm cache verify
npm --prefix webui ci --ignore-scripts --no-audit --no-fund
npm --prefix nodetray/frontend ci --ignore-scripts --no-audit --no-fund
npm --prefix webui test
npm --prefix nodetray/frontend test
```

Expected: npm cache 路径为 `C:\Users\Administrator\AppData\Local\npm-cache`；两个 `npm ci` 和测试退出码均为 0。

- [ ] **Step 5: 运行 Go 串行门禁**

Run:

```powershell
$go = 'C:\tmp\go1.26.5\go\bin\go.exe'
& $go test -p=1 -count=1 ./...
```

Expected: 所有 Go 包 PASS；仓库中不出现新的 `gocache`、`gomodcache` 或 `go-cache` 目录。

- [ ] **Step 6: 通过标准 vcpkg 工具链执行代表性原生构建**

先确保验证阶段目录不存在，再运行：

```powershell
$stage = '.tmp\standard-dependency-verification-stage'
if (Test-Path -LiteralPath $stage) {
    throw "VERIFICATION_STAGE_ALREADY_EXISTS path=$stage"
}
pwsh -NoProfile -File scripts/build.ps1 `
    -Go 'C:\tmp\go1.26.5\go\bin\go.exe' `
    -VideoCoreOnly -StageDir $stage -SkipWebBuild -SkipNodeTrayBuild `
    -VcpkgRoot 'C:\vcpkg'
```

Expected: CMake 配置、VideoCore 构建、CTest 和 Go 构建全部通过；日志明确显示 `C:\vcpkg\installed`，没有创建项目级 vcpkg 安装或下载目录。

---

### Task 5: 执行清理并验证空间回收

**Files:**
- Execute: `scripts/clean-build-cache.ps1`
- Inspect: `D:\code\mySingerServer`

**Interfaces:**
- Consumes: Task 3 的白名单清理器和 Task 4 的构建证据
- Produces: 实际回收空间记录、清理后目录扫描和最终 Git 状态

- [ ] **Step 1: 最后检查运行进程并执行真实清理**

Run:

```powershell
pwsh -NoProfile -File scripts/clean-build-cache.ps1 -Apply
```

Expected: 脚本逐个输出删除目标和逻辑字节数；若任一目标被占用则停止并报告 `CACHE_TARGET_IN_USE`，不得强制终止未知进程。

- [ ] **Step 2: 验证受保护内容仍存在**

Run:

```powershell
$protected = @(
    'artifacts\releases',
    '.superpowers\evidence',
    '.worktrees',
    'webui\node_modules',
    'nodetray\frontend\node_modules',
    'third_party\ffmpeg'
)
foreach ($path in $protected) {
    if (-not (Test-Path -LiteralPath $path)) {
        throw "PROTECTED_PATH_MISSING path=$path"
    }
}
```

Expected: 所有受保护路径存在。

- [ ] **Step 3: 验证项目级构建缓存已移除**

Run:

```powershell
$forbidden = @(
    '.tmp', '.superpowers\tmp', '.superpowers\runtime',
    'artifacts\.gocache-live-status',
    'artifacts\.gocache-live-workerpool',
    'videocore\build', 'mediacore\build'
)
foreach ($path in $forbidden) {
    if (Test-Path -LiteralPath $path) {
        throw "PROJECT_CACHE_REMAINS path=$path"
    }
}
Get-ChildItem -LiteralPath '.codex-temp' -Force -Directory `
    -ErrorAction SilentlyContinue
```

Expected: 固定缓存目标均不存在；`.codex-temp` 中已确认的缓存/暂存目录消失，`internal`、`protobuf-tools`、`fixture-probe` 及根部普通文件不受影响。

- [ ] **Step 4: 记录清理后占用**

Run:

```powershell
$root = (Resolve-Path '.').Path
$files = Get-ChildItem -LiteralPath $root -Force -File -Recurse `
    -ErrorAction SilentlyContinue
$bytes = ($files | Measure-Object Length -Sum).Sum
"AFTER files=$($files.Count) gib=$([math]::Round($bytes / 1GB, 3))"
```

Expected: 文件数和逻辑 GiB 明显低于清理前；报告清理器输出的精确释放字节数。

- [ ] **Step 5: 运行清理后的轻量门禁**

Run:

```powershell
pwsh -NoProfile -File scripts/test-standard-dependency-paths.ps1
pwsh -NoProfile -File scripts/test-clean-build-cache.ps1
$go = 'C:\tmp\go1.26.5\go\bin\go.exe'
& $go test -p=1 -count=1 ./integration `
    -run 'TestVideoCoreBuildStaticContract|TestBuildScript'
git diff --check
git status --short
```

Expected: 所有轻量门禁 PASS；Git 只显示计划内实现文件及原有用户修改，正式发布物和依赖清单不变。
