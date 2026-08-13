# MinGW 系统级安装脚本 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 生成一个由管理员执行的 PowerShell 脚本，从官方固定地址安装 WinLibs MinGW-w64，并配置、验证项目所需的系统级环境变量。

**Architecture:** 安装脚本把纯计算和文件检查拆成可独立测试的函数，只有顶层 `Invoke-MingwSystemInstall` 执行下载、系统环境变量写入和编译验证。测试脚本通过点源方式加载函数，使用临时目录和假工具文件验证路径去重、哈希及完整性判断，不联网、不写 Machine 环境变量。

**Tech Stack:** Windows PowerShell 5.1+、WinLibs x86_64 POSIX/UCRT、.NET `System.Environment`、`System.IO.Compression`、Win32 `WM_SETTINGCHANGE`。

## Global Constraints

- 安装目录固定为 `C:\Tools\WinLibs\mingw64`。
- 发行包固定为 WinLibs GCC 16.1.0、MinGW-w64 14.0.0、release 4。
- 下载地址固定为 `https://github.com/brechtsanders/winlibs_mingw/releases/download/16.1.0posix-14.0.0-ucrt-r4/winlibs-x86_64-posix-seh-gcc-16.1.0-mingw-w64ucrt-14.0.0-r4.zip`。
- SHA-256 固定为 `c406a22f8cac82559a3a1d96b62ff603f666499fb5ff4784e87b4eb6fa37dede`。
- 只写 Machine 范围的 `Path`、`CC`、`CXX`、`M5_CC`、`M5_WINDRES`、`M5_DLLTOOL`。
- 不覆盖不完整的已有安装目录，不删除脚本未创建的文件。
- 安装前至少要求 C 盘有 4 GiB 可用空间。

---

### Task 1: 编写并验证系统级 MinGW 安装脚本

**Files:**
- Create: `scripts/install-mingw-system.ps1`
- Create: `scripts/test-install-mingw-system.ps1`

**Interfaces:**
- Consumes: 官方 ZIP 地址和 SHA-256；Windows 管理员令牌；`C:\Tools` 可用空间。
- Produces: `Get-PathWithEntry([string]$CurrentPath, [string]$Entry) -> string`，`Test-RequiredToolchain([string]$MingwRoot) -> bool`，`Test-FileSha256([string]$Path, [string]$ExpectedSha256) -> bool`，以及顶层 `Invoke-MingwSystemInstall`。

- [ ] **Step 1: 写失败测试**

创建 `scripts/test-install-mingw-system.ps1`，先断言安装脚本存在，然后点源脚本并验证三个纯函数：

```powershell
$ErrorActionPreference = 'Stop'
$scriptPath = Join-Path $PSScriptRoot 'install-mingw-system.ps1'
if (-not (Test-Path -LiteralPath $scriptPath -PathType Leaf)) {
    throw "安装脚本不存在: $scriptPath"
}

. $scriptPath

$entry = 'C:\Tools\WinLibs\mingw64\bin'
$once = Get-PathWithEntry -CurrentPath 'C:\Windows\System32' -Entry $entry
$twice = Get-PathWithEntry -CurrentPath $once -Entry $entry
if (@($twice -split ';' | Where-Object {
    $_.TrimEnd([char]'\') -ieq $entry.TrimEnd([char]'\')
}).Count -ne 1) {
    throw 'Path 去重测试失败'
}

$fixture = Join-Path ([IO.Path]::GetTempPath()) ('m5-mingw-test-' + [guid]::NewGuid().ToString('N'))
try {
    $bin = Join-Path $fixture 'bin'
    [void](New-Item -ItemType Directory -Path $bin -Force)
    foreach ($name in 'gcc.exe','g++.exe','windres.exe','dlltool.exe') {
        [IO.File]::WriteAllText((Join-Path $bin $name), $name)
    }
    if (-not (Test-RequiredToolchain -MingwRoot $fixture)) {
        throw '完整工具链判断失败'
    }
    Remove-Item -LiteralPath (Join-Path $bin 'dlltool.exe')
    if (Test-RequiredToolchain -MingwRoot $fixture) {
        throw '不完整工具链判断失败'
    }

    $hashFile = Join-Path $fixture 'hash.txt'
    [IO.File]::WriteAllText($hashFile, 'm5')
    $hash = (Get-FileHash -LiteralPath $hashFile -Algorithm SHA256).Hash
    if (-not (Test-FileSha256 -Path $hashFile -ExpectedSha256 $hash.ToLowerInvariant())) {
        throw 'SHA-256 大小写兼容测试失败'
    }
} finally {
    if (Test-Path -LiteralPath $fixture) {
        Remove-Item -LiteralPath $fixture -Recurse -Force
    }
}

Write-Host 'PASS: install-mingw-system.ps1'
```

- [ ] **Step 2: 运行测试并确认 RED**

Run:

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\test-install-mingw-system.ps1
```

Expected: FAIL，错误包含 `安装脚本不存在`。

- [ ] **Step 3: 实现最小且完整的安装脚本**

创建 `scripts/install-mingw-system.ps1`，内容如下：

```powershell
$script:InstallRoot = 'C:\Tools\WinLibs'
$script:MingwRoot = Join-Path $script:InstallRoot 'mingw64'
$script:BinPath = Join-Path $script:MingwRoot 'bin'
$script:ArchiveUri = 'https://github.com/brechtsanders/winlibs_mingw/releases/download/16.1.0posix-14.0.0-ucrt-r4/winlibs-x86_64-posix-seh-gcc-16.1.0-mingw-w64ucrt-14.0.0-r4.zip'
$script:ExpectedSha256 = 'c406a22f8cac82559a3a1d96b62ff603f666499fb5ff4784e87b4eb6fa37dede'
$script:MinimumFreeBytes = 4GB

function Get-PathWithEntry {
    param([string]$CurrentPath, [Parameter(Mandatory)][string]$Entry)
    $entryValue = $Entry.Trim().TrimEnd([char]'\')
    $items = @($CurrentPath -split ';' | ForEach-Object { $_.Trim() } | Where-Object { $_ })
    if (@($items | Where-Object { $_.TrimEnd([char]'\') -ieq $entryValue }).Count -eq 0) {
        $items += $entryValue
    }
    return ($items -join ';')
}

function Test-RequiredToolchain {
    param([Parameter(Mandatory)][string]$MingwRoot)
    $bin = Join-Path $MingwRoot 'bin'
    foreach ($name in 'gcc.exe','g++.exe','windres.exe','dlltool.exe') {
        if (-not (Test-Path -LiteralPath (Join-Path $bin $name) -PathType Leaf)) { return $false }
    }
    return $true
}

function Test-FileSha256 {
    param([Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)][string]$ExpectedSha256)
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash -ieq $ExpectedSha256
}

function Assert-Administrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw '请以管理员身份运行 PowerShell，然后重新执行本脚本。'
    }
}

function Assert-SystemAndDiskSpace {
    if (-not [Environment]::Is64BitOperatingSystem) { throw '只支持 Windows x64。' }
    $driveName = (Split-Path -Qualifier $script:InstallRoot).TrimEnd([char]'\').TrimEnd([char]':')
    $drive = Get-PSDrive -Name $driveName -ErrorAction Stop
    if ($drive.Free -lt $script:MinimumFreeBytes) {
        throw ('{0}: 可用空间不足，需要至少 4 GiB，当前为 {1:N2} GiB。' -f $driveName, ($drive.Free / 1GB))
    }
}

function Save-OfficialArchive {
    param([Parameter(Mandatory)][string]$Destination)
    if (Get-Command Start-BitsTransfer -ErrorAction SilentlyContinue) {
        Start-BitsTransfer -Source $script:ArchiveUri -Destination $Destination -ErrorAction Stop
    } else {
        Invoke-WebRequest -Uri $script:ArchiveUri -OutFile $Destination -UseBasicParsing
    }
}

function Set-MachineToolchainEnvironment {
    $values = [ordered]@{
        CC = Join-Path $script:BinPath 'gcc.exe'
        CXX = Join-Path $script:BinPath 'g++.exe'
        M5_CC = Join-Path $script:BinPath 'gcc.exe'
        M5_WINDRES = Join-Path $script:BinPath 'windres.exe'
        M5_DLLTOOL = Join-Path $script:BinPath 'dlltool.exe'
    }
    $machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    $machinePath = Get-PathWithEntry -CurrentPath $machinePath -Entry $script:BinPath
    [Environment]::SetEnvironmentVariable('Path', $machinePath, 'Machine')
    $env:Path = Get-PathWithEntry -CurrentPath $env:Path -Entry $script:BinPath
    foreach ($item in $values.GetEnumerator()) {
        [Environment]::SetEnvironmentVariable($item.Key, $item.Value, 'Machine')
        Set-Item -Path ('Env:' + $item.Key) -Value $item.Value
    }
}

function Send-EnvironmentChanged {
    if (-not ('M5Environment.NativeMethods' -as [type])) {
        Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
namespace M5Environment {
    public static class NativeMethods {
        [DllImport("user32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        public static extern IntPtr SendMessageTimeout(
            IntPtr hWnd, uint msg, IntPtr wParam, string lParam,
            uint flags, uint timeout, out IntPtr result);
    }
}
'@
    }
    $result = [IntPtr]::Zero
    [void][M5Environment.NativeMethods]::SendMessageTimeout(
        [IntPtr]0xffff, 0x001A, [IntPtr]::Zero, 'Environment', 2, 5000, [ref]$result)
}

function Invoke-CompilerSmokeTest {
    foreach ($tool in 'gcc.exe','windres.exe','dlltool.exe') {
        & (Join-Path $script:BinPath $tool) --version | Select-Object -First 1 | Write-Host
        if ($LASTEXITCODE -ne 0) { throw "$tool 版本检查失败，退出码 $LASTEXITCODE。" }
    }
    $work = Join-Path ([IO.Path]::GetTempPath()) ('m5-mingw-smoke-' + [guid]::NewGuid().ToString('N'))
    try {
        [void](New-Item -ItemType Directory -Path $work)
        $source = Join-Path $work 'main.c'
        $exe = Join-Path $work 'main.exe'
        [IO.File]::WriteAllText($source, 'int main(void) { return 0; }')
        & (Join-Path $script:BinPath 'gcc.exe') $source '-o' $exe
        if ($LASTEXITCODE -ne 0) { throw "C 编译测试失败，退出码 $LASTEXITCODE。" }
        & $exe
        if ($LASTEXITCODE -ne 0) { throw "C 程序运行测试失败，退出码 $LASTEXITCODE。" }
    } finally {
        if (Test-Path -LiteralPath $work) { Remove-Item -LiteralPath $work -Recurse -Force }
    }
}

function Invoke-MingwSystemInstall {
    Assert-Administrator
    Assert-SystemAndDiskSpace

    $needsInstall = -not (Test-Path -LiteralPath $script:InstallRoot)
    if (-not $needsInstall -and -not (Test-RequiredToolchain -MingwRoot $script:MingwRoot)) {
        throw "安装目录已存在但工具不完整，请手工处理后重试: $script:InstallRoot"
    }

    if ($needsInstall) {
        $parent = Split-Path -Parent $script:InstallRoot
        [void](New-Item -ItemType Directory -Path $parent -Force)
        $work = Join-Path $parent ('.winlibs-install-' + [guid]::NewGuid().ToString('N'))
        try {
            [void](New-Item -ItemType Directory -Path $work)
            $archive = Join-Path $work 'winlibs.zip'
            $extract = Join-Path $work 'extract'
            $installStage = Join-Path $work 'WinLibs'
            Write-Host "正在下载 $script:ArchiveUri"
            Save-OfficialArchive -Destination $archive
            if (-not (Test-FileSha256 -Path $archive -ExpectedSha256 $script:ExpectedSha256)) {
                throw 'WinLibs ZIP 的 SHA-256 不匹配，已拒绝安装。'
            }
            Expand-Archive -LiteralPath $archive -DestinationPath $extract
            $extractedMingw = Join-Path $extract 'mingw64'
            if (-not (Test-RequiredToolchain -MingwRoot $extractedMingw)) {
                throw '解压后的 WinLibs 缺少 gcc、g++、windres 或 dlltool。'
            }
            [void](New-Item -ItemType Directory -Path $installStage)
            Move-Item -LiteralPath $extractedMingw -Destination $installStage
            Move-Item -LiteralPath $installStage -Destination $script:InstallRoot
        } finally {
            if (Test-Path -LiteralPath $work) { Remove-Item -LiteralPath $work -Recurse -Force }
        }
    } else {
        Write-Host "检测到完整安装，跳过下载: $script:MingwRoot"
    }

    Set-MachineToolchainEnvironment
    Send-EnvironmentChanged
    Invoke-CompilerSmokeTest
    Write-Host "MinGW 安装和系统环境变量配置完成: $script:MingwRoot" -ForegroundColor Green
}

if ($MyInvocation.InvocationName -ne '.') {
    Invoke-MingwSystemInstall
}
```

- [ ] **Step 4: 运行测试并确认 GREEN**

Run:

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\test-install-mingw-system.ps1
```

Expected: PASS，输出 `PASS: install-mingw-system.ps1`，且没有网络请求或 Machine 环境变量变更。

- [ ] **Step 5: 执行静态语法和差异检查**

Run:

```powershell
$tokens = $null
$errors = $null
[void][System.Management.Automation.Language.Parser]::ParseFile((Resolve-Path '.\scripts\install-mingw-system.ps1'), [ref]$tokens, [ref]$errors)
if ($errors.Count -ne 0) { $errors | Format-List; exit 1 }
git diff --check -- scripts/install-mingw-system.ps1 scripts/test-install-mingw-system.ps1
```

Expected: parser errors 为 0，`git diff --check` 返回 0。

- [ ] **Step 6: 提交脚本和测试**

```powershell
git add -- scripts/install-mingw-system.ps1 scripts/test-install-mingw-system.ps1
git commit -m "build: add system MinGW installer script"
```
