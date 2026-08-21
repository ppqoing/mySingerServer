<#
.SYNOPSIS
在隔离 Node 状态下连续运行半小时真实媒体计算并每两秒采样。

.DESCRIPTION
媒体目录只用于递归清单和传给 Node 的扫描根；脚本不会写入、复制、删除、重命名媒体，
也不会在媒体目录生成旁车文件。数据库、配置、日志、缓存和证据全部位于 C:\tmp。
#>
[CmdletBinding()]
param(
    [string] $MediaRoot = $env:RUST_V2_REAL_MEDIA_ROOT,
    [int] $DurationSeconds = 1800,
    [int] $SampleSeconds = 2,
    [string] $CargoTargetDir = 'C:\tmp\rust-v2-node-runtime-target',
    [string] $ReleaseRoot = '',
    [switch] $LibraryOnly
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$script:RepositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$script:AcceptanceRoot = 'C:\tmp\rust-v2-runtime-acceptance'
$script:TargetTriple = 'x86_64-pc-windows-msvc'
$script:FfmpegFiles = @(
    'avutil-60.dll',
    'swresample-6.dll',
    'swscale-9.dll',
    'avcodec-62.dll',
    'avformat-62.dll'
)

function New-ValidationResult {
    param([bool] $Valid, [string] $Code)
    [pscustomobject]@{ Valid = $Valid; Code = $Code }
}

function Assert-RuntimeAcceptanceInputs {
    <# 验证所有外部输入；失败时不创建目录、不启动进程。 #>
    param(
        [string] $MediaRoot,
        [int] $DurationSeconds,
        [int] $SampleSeconds,
        [string] $ReleaseRoot,
        [switch] $ThrowOnError = $true
    )

    $code = ''
    if ([string]::IsNullOrWhiteSpace($MediaRoot)) {
        $code = 'RUST_V2_REAL_MEDIA_ROOT_MISSING'
    }
    elseif (-not (Test-Path -LiteralPath $MediaRoot -PathType Container)) {
        $code = 'RUST_V2_REAL_MEDIA_ROOT_INVALID'
    }
    elseif ($DurationSeconds -lt 1800) {
        $code = 'RUST_V2_ACCEPTANCE_DURATION_INVALID'
    }
    elseif ($SampleSeconds -ne 2) {
        $code = 'RUST_V2_ACCEPTANCE_SAMPLE_INVALID'
    }
    elseif ([string]::IsNullOrWhiteSpace($ReleaseRoot) -or
        -not (Test-Path -LiteralPath $ReleaseRoot -PathType Container)) {
        $code = 'RUST_V2_ACCEPTANCE_RELEASE_ROOT_INVALID'
    }
    else {
        foreach ($name in @('node.exe', 'worker.exe', 'runtime_acceptance.exe', 'Everything.exe')) {
            if (-not (Test-Path -LiteralPath (Join-Path $ReleaseRoot $name) -PathType Leaf)) {
                $code = "RUST_V2_ACCEPTANCE_BINARY_MISSING:$name"
                break
            }
        }
        if (-not $code) {
            foreach ($name in $script:FfmpegFiles) {
                if (-not (Test-Path -LiteralPath (Join-Path $ReleaseRoot "runtime\ffmpeg\$name") -PathType Leaf)) {
                    $code = "RUST_V2_ACCEPTANCE_FFMPEG_MISSING:$name"
                    break
                }
            }
        }
    }

    if ($code) {
        if ($ThrowOnError) {
            throw $code
        }
        return New-ValidationResult -Valid $false -Code $code
    }
    New-ValidationResult -Valid $true -Code ''
}

function New-RuntimeAcceptanceLayout {
    <# 计算本次唯一隔离根；调用者负责按需创建目录。 #>
    param([string] $RunId = [Guid]::NewGuid().ToString('N'))

    if ($RunId -notmatch '^[A-Za-z0-9-]+$') {
        throw 'RUST_V2_ACCEPTANCE_RUN_ID_INVALID'
    }
    $root = [IO.Path]::GetFullPath((Join-Path $script:AcceptanceRoot $RunId))
    if (-not $root.StartsWith(($script:AcceptanceRoot + '\'), [StringComparison]::OrdinalIgnoreCase)) {
        throw 'RUST_V2_ACCEPTANCE_LAYOUT_INVALID'
    }
    [pscustomobject]@{
        Root = $root
        Data = Join-Path $root 'data\node'
        Logs = Join-Path $root 'data\node\logs'
        Cache = Join-Path $root 'data\node\cache'
        Evidence = Join-Path $root 'evidence'
    }
}

function New-IsolatedNodeConfig {
    <# 写入相对 node.exe 根解释的完整配置正文。 #>
    param([int] $Port)

    @"
listen_ip = "127.0.0.1"
port = $Port
worker_count = 4
enumerator = "everything"

[paths]
data_path = "data/node"
config_path = "data/node/config.toml"
log_path = "data/node/logs"
cache_path = "data/node/cache"

[read]
hdd_threads_per_disk = 1
ssd_threads_per_disk = 2
unknown_threads_per_disk = 1
total_threads = 4
block_size_bytes = 4194304
block_timeout_seconds = 3
block_retries = 2

[worker]
mode = "automatic"
reserved_cores = 1
manual_worker_count = 4
"@
}

function Get-RuntimeMediaManifest {
    <# 只读枚举媒体；不读取正文，不计算哈希，不改变时间戳。 #>
    param([Parameter(Mandatory)] [string] $MediaRoot)

    $root = (Get-Item -LiteralPath $MediaRoot -ErrorAction Stop).FullName
    $files = @(
        Get-ChildItem -LiteralPath $root -Recurse -File -Force -ErrorAction Stop |
            ForEach-Object {
                [pscustomobject]@{
                    Path = [IO.Path]::GetRelativePath($root, $_.FullName).Replace('\', '/')
                    Length = [long]$_.Length
                    LastWriteTimeUtc = $_.LastWriteTimeUtc.ToString('O')
                }
            } |
            Sort-Object -Property Path
    )
    [pscustomobject]@{
        Root = $root
        FileCount = $files.Count
        TotalBytes = [long](($files | Measure-Object -Property Length -Sum).Sum ?? 0)
        Files = $files
    }
}

function Assert-RuntimeMediaUnchanged {
    <# 按相对路径、长度和UTC修改时间逐项证明源媒体未变化。 #>
    param(
        [Parameter(Mandatory)] $Before,
        [Parameter(Mandatory)] $After
    )

    $beforeJson = $Before.Files | ConvertTo-Json -Depth 5 -Compress
    $afterJson = $After.Files | ConvertTo-Json -Depth 5 -Compress
    if ($Before.Root -cne $After.Root -or
        $Before.FileCount -ne $After.FileCount -or
        $Before.TotalBytes -ne $After.TotalBytes -or
        $beforeJson -cne $afterJson) {
        throw 'RUST_V2_REAL_MEDIA_CHANGED'
    }
}

function Get-FreeTcpPort {
    $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
    $listener.Start()
    try {
        ([Net.IPEndPoint]$listener.LocalEndpoint).Port
    }
    finally {
        $listener.Stop()
    }
}

function Test-IsAdministrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = [Security.Principal.WindowsPrincipal]::new($identity)
    $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Wait-TcpEndpoint {
    param([int] $Port, [Diagnostics.Process] $Process, [int] $TimeoutSeconds = 60)

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        if ($Process.HasExited) {
            throw "RUST_V2_ACCEPTANCE_NODE_EXITED code=$($Process.ExitCode)"
        }
        $client = [Net.Sockets.TcpClient]::new()
        try {
            $pending = $client.ConnectAsync([Net.IPAddress]::Loopback, $Port)
            if ($pending.Wait(250) -and $client.Connected) {
                return
            }
        }
        catch {
            # listener 尚未就绪时继续固定短轮询。
        }
        finally {
            $client.Dispose()
        }
        Start-Sleep -Milliseconds 250
    }
    throw "RUST_V2_ACCEPTANCE_NODE_TIMEOUT port=$Port"
}

function Copy-RuntimeAcceptanceRelease {
    param([string] $Source, [string] $Destination)

    foreach ($name in @('node.exe', 'worker.exe', 'runtime_acceptance.exe', 'Everything.exe')) {
        Copy-Item -LiteralPath (Join-Path $Source $name) -Destination (Join-Path $Destination $name)
    }
    $runtimeDestination = Join-Path $Destination 'runtime\ffmpeg'
    New-Item -ItemType Directory -Path $runtimeDestination -Force | Out-Null
    foreach ($name in $script:FfmpegFiles) {
        Copy-Item -LiteralPath (Join-Path $Source "runtime\ffmpeg\$name") `
            -Destination (Join-Path $runtimeDestination $name)
    }
}

function Get-IsolatedProcesses {
    param([string] $Root)

    @(
        Get-CimInstance Win32_Process -ErrorAction SilentlyContinue |
            Where-Object {
                $_.ExecutablePath -and
                $_.ExecutablePath.StartsWith(($Root + '\'), [StringComparison]::OrdinalIgnoreCase)
            }
    )
}

function Write-SystemSample {
    param(
        [string] $Path,
        [string] $Root,
        [int] $ElapsedSeconds,
        [hashtable] $PreviousCpu
    )

    $processes = @(
        foreach ($row in Get-IsolatedProcesses -Root $Root) {
            $process = Get-Process -Id $row.ProcessId -ErrorAction SilentlyContinue
            if (-not $process) { continue }
            $totalCpuMs = [double]$process.TotalProcessorTime.TotalMilliseconds
            $last = if ($PreviousCpu.ContainsKey($process.Id)) { [double]$PreviousCpu[$process.Id] } else { $totalCpuMs }
            $PreviousCpu[$process.Id] = $totalCpuMs
            [pscustomobject]@{
                Name = $process.ProcessName
                ProcessId = $process.Id
                CpuDeltaMs = [Math]::Max(0, $totalCpuMs - $last)
                WorkingSetBytes = [long]$process.WorkingSet64
                PrivateMemoryBytes = [long]$process.PrivateMemorySize64
            }
        }
    )
    $disks = @(
        Get-CimInstance Win32_PerfFormattedData_PerfDisk_PhysicalDisk -ErrorAction SilentlyContinue |
            Where-Object Name -ne '_Total' |
            ForEach-Object {
                [pscustomobject]@{
                    Name = [string]$_.Name
                    DiskReadBytesPerSec = [double]$_.DiskReadBytesPersec
                    AvgDiskQueueLength = [double]$_.AvgDiskQueueLength
                }
            }
    )
    $sample = [pscustomobject]@{
        record_type = 'system_sample'
        utc = [DateTime]::UtcNow.ToString('O')
        elapsed_seconds = $ElapsedSeconds
        processes = $processes
        disks = $disks
    }
    Add-Content -LiteralPath $Path -Value ($sample | ConvertTo-Json -Depth 8 -Compress) -Encoding utf8
}

function Stop-IsolatedProcesses {
    <# 只终止本次 staging 绝对路径下的 Node、Worker 和客户端。 #>
    param([string] $Root)

    $rows = Get-IsolatedProcesses -Root $Root | Sort-Object -Property ProcessId -Descending
    foreach ($row in $rows) {
        Stop-Process -Id $row.ProcessId -Force -ErrorAction SilentlyContinue
    }
}

function Resolve-ReleaseRoot {
    param([string] $CargoTargetDir, [string] $ReleaseRoot)

    if ($ReleaseRoot) {
        return [IO.Path]::GetFullPath($ReleaseRoot)
    }
    $binaryRoot = Join-Path ([IO.Path]::GetFullPath($CargoTargetDir)) "$script:TargetTriple\release"
    $stagedRuntime = Join-Path $script:RepositoryRoot 'dist-rust-v2\staging'
    $assembled = Join-Path ([IO.Path]::GetTempPath()) 'rust-v2-runtime-acceptance-release'
    if (Test-Path -LiteralPath $assembled) {
        Remove-Item -LiteralPath $assembled -Recurse -Force
    }
    New-Item -ItemType Directory -Path $assembled -Force | Out-Null
    foreach ($name in @('node.exe', 'worker.exe')) {
        Copy-Item -LiteralPath (Join-Path $binaryRoot $name) -Destination (Join-Path $assembled $name)
    }
    Copy-Item -LiteralPath (Join-Path $binaryRoot 'examples\runtime_acceptance.exe') `
        -Destination (Join-Path $assembled 'runtime_acceptance.exe')
    Copy-Item -LiteralPath (Join-Path $script:RepositoryRoot 'third_party\everything\Everything.exe') `
        -Destination (Join-Path $assembled 'Everything.exe')
    Copy-Item -LiteralPath (Join-Path $stagedRuntime 'runtime') -Destination $assembled -Recurse
    $assembled
}

function Invoke-RustV2RuntimeAcceptance {
    param(
        [string] $MediaRoot,
        [int] $DurationSeconds,
        [int] $SampleSeconds,
        [string] $CargoTargetDir,
        [string] $ReleaseRoot
    )

    $resolvedRelease = Resolve-ReleaseRoot -CargoTargetDir $CargoTargetDir -ReleaseRoot $ReleaseRoot
    Assert-RuntimeAcceptanceInputs -MediaRoot $MediaRoot -DurationSeconds $DurationSeconds `
        -SampleSeconds $SampleSeconds -ReleaseRoot $resolvedRelease | Out-Null
    $media = (Get-Item -LiteralPath $MediaRoot).FullName
    $layout = New-RuntimeAcceptanceLayout
    if ($media.StartsWith(($layout.Root + '\'), [StringComparison]::OrdinalIgnoreCase) -or
        $layout.Root.StartsWith(($media + '\'), [StringComparison]::OrdinalIgnoreCase)) {
        throw 'RUST_V2_REAL_MEDIA_STAGING_OVERLAP'
    }

    New-Item -ItemType Directory -Path $layout.Root, $layout.Data, $layout.Logs, $layout.Cache, $layout.Evidence -Force | Out-Null
    Copy-RuntimeAcceptanceRelease -Source $resolvedRelease -Destination $layout.Root
    $port = Get-FreeTcpPort
    [IO.File]::WriteAllText(
        (Join-Path $layout.Root 'bootstrap.toml'),
        "config_path = 'data/node/config.toml'`n",
        [Text.UTF8Encoding]::new($false))
    [IO.File]::WriteAllText(
        (Join-Path $layout.Data 'config.toml'),
        (New-IsolatedNodeConfig -Port $port),
        [Text.UTF8Encoding]::new($false))

    $before = Get-RuntimeMediaManifest -MediaRoot $media
    $before | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath (Join-Path $layout.Evidence 'media-before.json') -Encoding utf8
    $runtimeOutput = Join-Path $layout.Evidence 'runtime.ndjson'
    $systemOutput = Join-Path $layout.Evidence 'system.ndjson'
    $stdout = Join-Path $layout.Evidence 'client.stdout.log'
    $stderr = Join-Path $layout.Evidence 'client.stderr.log'
    $node = $null
    $client = $null
    $savedEnvironment = @{}
    foreach ($name in @(
        'RUST_V2_ACCEPTANCE_ENDPOINT',
        'RUST_V2_REAL_MEDIA_ROOT',
        'RUST_V2_ACCEPTANCE_DURATION_SECONDS',
        'RUST_V2_ACCEPTANCE_OUTPUT')) {
        $savedEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
    }

    try {
        $nodeArgs = @{
            FilePath = (Join-Path $layout.Root 'node.exe')
            WorkingDirectory = $layout.Root
            PassThru = $true
            WindowStyle = 'Hidden'
        }
        if (-not (Test-IsAdministrator)) {
            $nodeArgs.Verb = 'RunAs'
        }
        $node = Start-Process @nodeArgs
        Wait-TcpEndpoint -Port $port -Process $node

        $env:RUST_V2_ACCEPTANCE_ENDPOINT = "127.0.0.1:$port"
        $env:RUST_V2_REAL_MEDIA_ROOT = $media
        $env:RUST_V2_ACCEPTANCE_DURATION_SECONDS = [string]$DurationSeconds
        $env:RUST_V2_ACCEPTANCE_OUTPUT = $runtimeOutput
        $client = Start-Process -FilePath (Join-Path $layout.Root 'runtime_acceptance.exe') `
            -WorkingDirectory $layout.Root -PassThru -WindowStyle Hidden `
            -RedirectStandardOutput $stdout -RedirectStandardError $stderr

        $started = [Diagnostics.Stopwatch]::StartNew()
        $previousCpu = @{}
        while (-not $client.HasExited) {
            Write-SystemSample -Path $systemOutput -Root $layout.Root `
                -ElapsedSeconds ([int]$started.Elapsed.TotalSeconds) -PreviousCpu $previousCpu
            Start-Sleep -Seconds $SampleSeconds
            $client.Refresh()
            if ($node.HasExited) {
                throw "RUST_V2_ACCEPTANCE_NODE_EXITED code=$($node.ExitCode)"
            }
        }
        if ($client.ExitCode -ne 0) {
            throw "RUST_V2_ACCEPTANCE_CLIENT_FAILED code=$($client.ExitCode)"
        }

        $after = Get-RuntimeMediaManifest -MediaRoot $media
        $after | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath (Join-Path $layout.Evidence 'media-after.json') -Encoding utf8
        Assert-RuntimeMediaUnchanged -Before $before -After $after
        [pscustomobject]@{
            RunRoot = $layout.Root
            EvidenceRoot = $layout.Evidence
            RuntimeOutput = $runtimeOutput
            SystemOutput = $systemOutput
            DurationSeconds = $DurationSeconds
            MediaUnchanged = $true
            EffectiveWorkerCount = 4
            NodeUnexpectedExit = $false
            ContactSheetReuseCount = 0
            DiskFullCleanupCount = 0
        } | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath (Join-Path $layout.Evidence 'harness-result.json') -Encoding utf8

        $reporter = Join-Path $script:RepositoryRoot 'tests\windows\New-RustV2RuntimeAcceptanceReport.ps1'
        if (Test-Path -LiteralPath $reporter -PathType Leaf) {
            & $reporter -EvidenceRoot $layout.Evidence
        }
        Write-Output 'RUST_V2_RUNTIME_ACCEPTANCE_MEASURE_PASS'
        Write-Output "RUN_ROOT=$($layout.Root)"
        Write-Output "EVIDENCE_ROOT=$($layout.Evidence)"
    }
    finally {
        foreach ($name in $savedEnvironment.Keys) {
            [Environment]::SetEnvironmentVariable($name, $savedEnvironment[$name], 'Process')
        }
        Stop-IsolatedProcesses -Root $layout.Root
    }
}

if (-not $LibraryOnly) {
    Invoke-RustV2RuntimeAcceptance -MediaRoot $MediaRoot -DurationSeconds $DurationSeconds `
        -SampleSeconds $SampleSeconds -CargoTargetDir $CargoTargetDir -ReleaseRoot $ReleaseRoot
}
