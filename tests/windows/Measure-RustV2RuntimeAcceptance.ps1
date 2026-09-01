<#
.SYNOPSIS
在隔离 Node 状态下运行一次真实媒体计算并每两秒采样，任务终态后结束。

.DESCRIPTION
媒体目录只用于递归清单和传给 Node 的扫描根；脚本不会写入、复制、删除、重命名媒体，
也不会在媒体目录生成旁车文件。数据库、配置、日志、缓存和证据全部位于 C:\tmp。
#>
[CmdletBinding()]
param(
    [string[]] $MediaRoot = @($env:RUST_V2_REAL_MEDIA_ROOT),
    [string[]] $MediaRoots = @(),
    [int] $DurationSeconds = 1800,
    [int] $SampleSeconds = 2,
    [string] $CargoTargetDir = 'C:\tmp\rust-v2-node-runtime-target',
    [string] $ReleaseRoot = '',
    [string] $AcceptanceClientPath = '',
    [string] $ResultExporterPath = '',
    [string] $EvidenceRoot = '',
    [string] $ReportPath = '',
    [ValidateSet('A', 'B')]
    [string] $Variant = 'A',
    [ValidateRange(1, 3)]
    [int] $RunIndex = 1,
    [string] $SourceRevision = '',
    [string] $SourceTreeSha256 = '',
    [string] $PackagePath = '',
    [string] $PackageSha256 = '',
    [int] $WorkerCount = 20,
    [int] $HddThreadsPerDisk = 1,
    [int] $SsdThreadsPerDisk = 16,
    [int] $UnknownThreadsPerDisk = 1,
    [int] $TotalReadThreads = 12,
    [int] $ReservedCores = 1,
    [ValidateSet('everything', 'windows_walker')]
    [string] $Enumerator = 'everything',
    [switch] $SingleRun,
    [switch] $CompleteWhenTaskTerminal = $true,
    [switch] $RequireDistinctPhysicalDisks,
    [switch] $LibraryOnly
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$script:RepositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$script:AcceptanceRoot = 'C:\tmp\rust-v2-runtime-acceptance'
$script:TargetTriple = 'x86_64-pc-windows-msvc'
# 外层监督在客户端自有运行窗口之外预留的固定收尾宽限，避免 RPC 卡住时无限等待。
$script:RuntimeAcceptanceTeardownAllowanceSeconds = 120
$script:FfmpegFiles = @(
    'avutil-60.dll',
    'swresample-6.dll',
    'swscale-9.dll',
    'avcodec-62.dll',
    'avformat-62.dll'
)

function New-ValidationResult {
    <# 返回稳定的输入校验结果；调用方可选择抛出或保留诊断证据。 #>
    param([bool] $Valid, [string] $Code)
    [pscustomobject]@{ Valid = $Valid; Code = $Code }
}

function Get-RuntimeAcceptanceSupervisorDeadlineSeconds {
    <# 返回客户端外层硬截止；期限等于配置运行窗口加固定收尾宽限。 #>
    param([Parameter(Mandatory)] [int] $DurationSeconds)

    if ($DurationSeconds -lt 1) {
        throw 'RUST_V2_ACCEPTANCE_SUPERVISOR_DURATION_INVALID'
    }
    [int64]$DurationSeconds + [int64]$script:RuntimeAcceptanceTeardownAllowanceSeconds
}

function Get-RuntimeAcceptanceElapsedSeconds {
    <# 返回外层监督使用的单调秒数；独立函数便于行为夹具无等待地推进时钟。 #>
    param([Parameter(Mandatory)] [Diagnostics.Stopwatch] $Stopwatch)

    $Stopwatch.Elapsed.TotalSeconds
}

function Start-RuntimeAcceptanceSupervisor {
    <# 启动独立 pwsh 监督进程；主采样阻塞时仍按绝对截止校验双进程身份并 Kill(true)。 #>
    param(
        [Parameter(Mandatory)] [int] $ClientId,
        [Parameter(Mandatory)] [string] $ClientPath,
        [Parameter(Mandatory)] [string] $ClientStartTimeUtc,
        [Parameter(Mandatory)] [int] $NodeId,
        [Parameter(Mandatory)] [string] $NodePath,
        [Parameter(Mandatory)] [string] $NodeStartTimeUtc,
        [Parameter(Mandatory)] [DateTime] $DeadlineUtc,
        [Parameter(Mandatory)] [string] $StatusPath
    )

    $status = Get-NormalizedAbsolutePath -Path $StatusPath
    $statusParent = Split-Path -Parent $status
    if (-not (Test-Path -LiteralPath $statusParent -PathType Container)) {
        throw 'RUST_V2_ACCEPTANCE_SUPERVISOR_STATUS_PARENT_MISSING'
    }
    if (Test-Path -LiteralPath $status -PathType Leaf) {
        throw 'RUST_V2_ACCEPTANCE_SUPERVISOR_STATUS_EXISTS'
    }

    # 将受信任的参数编码进只读脚本；EncodedCommand 避免路径空格/引号被参数拼接拆分。
    $supervisorScript = @'
$ErrorActionPreference = 'Stop'
$clientId = __CLIENT_ID__
$clientPath = __CLIENT_PATH__
$clientStartTimeUtc = __CLIENT_START__
$nodeId = __NODE_ID__
$nodePath = __NODE_PATH__
$nodeStartTimeUtc = __NODE_START__
$deadlineUtc = [DateTime]::Parse(__DEADLINE__, [Globalization.CultureInfo]::InvariantCulture, [Globalization.DateTimeStyles]::RoundtripKind).ToUniversalTime()
$statusPath = __STATUS_PATH__
$utf8 = [Text.UTF8Encoding]::new($false)

function Write-SupervisorStatus {
    param([Parameter(Mandatory)] $Value)
    $tempPath = "$statusPath.$PID.tmp"
    [IO.File]::WriteAllText($tempPath, ($Value | ConvertTo-Json -Depth 8), $utf8)
    [IO.File]::Move($tempPath, $statusPath, $true)
}

function Get-VerifiedProcess {
    param(
        [Parameter(Mandatory)] [int] $Id,
        [Parameter(Mandatory)] [string] $ExpectedPath,
        [Parameter(Mandatory)] [string] $ExpectedStartTimeUtc,
        [Parameter(Mandatory)] [string] $Role
    )
    $process = Get-Process -Id $Id -ErrorAction SilentlyContinue
    if ($null -eq $process) {
        return [pscustomobject]@{ Kind = 'Missing'; Process = $null; Diagnostic = '' }
    }
    try {
        $actualPath = [string]$process.Path
        if ([string]::IsNullOrWhiteSpace($actualPath)) {
            $actualPath = [string]$process.MainModule.FileName
        }
        if ([string]::IsNullOrWhiteSpace($actualPath)) {
            return [pscustomobject]@{ Kind = 'Invalid'; Process = $process; Diagnostic = ('RUST_V2_ACCEPTANCE_SUPERVISOR_' + $Role + '_IDENTITY_UNAVAILABLE') }
        }
        $actualPath = [IO.Path]::GetFullPath($actualPath).TrimEnd('\')
        $expected = [IO.Path]::GetFullPath($ExpectedPath).TrimEnd('\')
        if (-not $actualPath.Equals($expected, [StringComparison]::OrdinalIgnoreCase)) {
            return [pscustomobject]@{ Kind = 'Invalid'; Process = $process; Diagnostic = ('RUST_V2_ACCEPTANCE_SUPERVISOR_' + $Role + '_PID_REUSED') }
        }
        $expectedStart = [DateTime]::Parse($ExpectedStartTimeUtc, [Globalization.CultureInfo]::InvariantCulture, [Globalization.DateTimeStyles]::RoundtripKind).ToUniversalTime()
        $actualStart = $process.StartTime.ToUniversalTime()
        if ($actualStart -ne $expectedStart) {
            return [pscustomobject]@{ Kind = 'Invalid'; Process = $process; Diagnostic = ('RUST_V2_ACCEPTANCE_SUPERVISOR_' + $Role + '_PID_REUSED') }
        }
        [pscustomobject]@{ Kind = 'Valid'; Process = $process; Diagnostic = '' }
    }
    catch {
        [pscustomobject]@{ Kind = 'Invalid'; Process = $process; Diagnostic = ('RUST_V2_ACCEPTANCE_SUPERVISOR_' + $Role + '_IDENTITY_UNAVAILABLE') }
    }
}

function Stop-VerifiedProcessTree {
    param([Parameter(Mandatory)] $State)
    if ($State.Kind -eq 'Missing') {
        return [pscustomobject]@{ Attempted = $false; Confirmed = $true; Process = $null; Diagnostic = '' }
    }
    if ($State.Kind -ne 'Valid') {
        return [pscustomobject]@{ Attempted = $false; Confirmed = $false; Process = $State.Process; Diagnostic = $State.Diagnostic }
    }
    try {
        # .Kill(true) 递归终止 Worker 子树；本函数只发出终止请求，确保双树先同时收到 Kill。
        $State.Process.Kill($true)
        [pscustomobject]@{ Attempted = $true; Confirmed = $null; Process = $State.Process; Diagnostic = '' }
    }
    catch {
        [pscustomobject]@{ Attempted = $true; Confirmed = $false; Process = $State.Process; Diagnostic = 'RUST_V2_ACCEPTANCE_SUPERVISOR_STOP_FAILED' }
    }
}

function Confirm-VerifiedProcessTreeExit {
    param([Parameter(Mandatory)] $StopResult)
    if ($null -ne $StopResult.Confirmed -and -not $StopResult.Confirmed) {
        return $StopResult
    }
    if ($null -eq $StopResult.Process) {
        return $StopResult
    }
    try {
        # 两棵树都已发出 Kill 后才进入各自 WaitForExit(5000)，避免 Node 继续运行5秒。
        $StopResult.Confirmed = [bool]$StopResult.Process.WaitForExit(5000)
        if (-not $StopResult.Confirmed) {
            $StopResult.Diagnostic = 'RUST_V2_ACCEPTANCE_CLIENT_EXIT_UNCONFIRMED'
        }
    }
    catch {
        $StopResult.Confirmed = $false
        $StopResult.Diagnostic = 'RUST_V2_ACCEPTANCE_CLIENT_EXIT_UNCONFIRMED'
    }
    $StopResult
}

try {
    while ($true) {
        $client = Get-VerifiedProcess -Id $clientId -ExpectedPath $clientPath -ExpectedStartTimeUtc $clientStartTimeUtc -Role 'CLIENT'
        if ($client.Kind -eq 'Missing') {
            Write-SupervisorStatus ([pscustomobject]@{
                TimedOut = $false; StopAttempted = $false; ExitConfirmed = $true; Diagnostic = ''
                ClientIdentityValid = $true; NodeIdentityValid = $true; Phase = 'complete'
            })
            exit 0
        }
        if ($client.Kind -ne 'Valid') {
            Write-SupervisorStatus ([pscustomobject]@{
                TimedOut = $false; StopAttempted = $false; ExitConfirmed = $false; Diagnostic = $client.Diagnostic
                ClientIdentityValid = $false; NodeIdentityValid = $true; Phase = 'complete'
            })
            exit 0
        }
        $node = Get-VerifiedProcess -Id $nodeId -ExpectedPath $nodePath -ExpectedStartTimeUtc $nodeStartTimeUtc -Role 'NODE'
        if ($node.Kind -eq 'Invalid') {
            Write-SupervisorStatus ([pscustomobject]@{
                TimedOut = $false; StopAttempted = $false; ExitConfirmed = $false; Diagnostic = $node.Diagnostic
                ClientIdentityValid = $true; NodeIdentityValid = $false; Phase = 'complete'
            })
            exit 0
        }
        $remaining = ($deadlineUtc - [DateTime]::UtcNow).TotalMilliseconds
        if ($remaining -le 0) {
            # 先落原子 stopping 状态，再执行 Kill(true)，避免主线程看到客户端退出却丢掉超时原因。
            try {
                Write-SupervisorStatus ([pscustomobject]@{
                    TimedOut = $true; StopAttempted = $true; ExitConfirmed = $false
                    Diagnostic = 'RUST_V2_ACCEPTANCE_SUPERVISOR_TIMEOUT'; Phase = 'stopping'
                    ClientIdentityValid = $true; NodeIdentityValid = $true
                })
            }
            catch { }
            # 先对 client、Node 两棵树同时发 Kill(true)，再分别 WaitForExit(5000) 确认。
            $clientStopRequest = Stop-VerifiedProcessTree -State $client
            $nodeStopRequest = Stop-VerifiedProcessTree -State $node
            $clientStop = Confirm-VerifiedProcessTreeExit -StopResult $clientStopRequest
            $nodeStop = Confirm-VerifiedProcessTreeExit -StopResult $nodeStopRequest
            $confirmed = $clientStop.Confirmed -and $nodeStop.Confirmed
            $diagnostic = if (-not $confirmed) { 'RUST_V2_ACCEPTANCE_CLIENT_EXIT_UNCONFIRMED' } else { 'RUST_V2_ACCEPTANCE_SUPERVISOR_TIMEOUT' }
            if (-not [string]::IsNullOrWhiteSpace([string]$clientStop.Diagnostic) -and $clientStop.Diagnostic -ne 'RUST_V2_ACCEPTANCE_CLIENT_EXIT_UNCONFIRMED') { $diagnostic = $clientStop.Diagnostic }
            elseif (-not [string]::IsNullOrWhiteSpace([string]$nodeStop.Diagnostic) -and $nodeStop.Diagnostic -ne 'RUST_V2_ACCEPTANCE_CLIENT_EXIT_UNCONFIRMED') { $diagnostic = $nodeStop.Diagnostic }
            Write-SupervisorStatus ([pscustomobject]@{
                TimedOut = $true; StopAttempted = $clientStop.Attempted -or $nodeStop.Attempted; ExitConfirmed = $confirmed; Diagnostic = $diagnostic
                ClientIdentityValid = $true; NodeIdentityValid = $true; Phase = 'complete'
            })
            exit 0
        }
        Start-Sleep -Milliseconds ([Math]::Min(250, [Math]::Max(1, [int][Math]::Ceiling($remaining))))
    }
}
catch {
    try {
        Write-SupervisorStatus ([pscustomobject]@{
            TimedOut = $false; StopAttempted = $false; ExitConfirmed = $false; Diagnostic = 'RUST_V2_ACCEPTANCE_SUPERVISOR_FAILED'
            ClientIdentityValid = $false; NodeIdentityValid = $false; Phase = 'failed'
        })
    }
    catch { }
    exit 1
}
'@
    $literalValues = @{
        '__CLIENT_ID__' = $ClientId
        '__CLIENT_PATH__' = $ClientPath
        '__CLIENT_START__' = $ClientStartTimeUtc
        '__NODE_ID__' = $NodeId
        '__NODE_PATH__' = $NodePath
        '__NODE_START__' = $NodeStartTimeUtc
        '__DEADLINE__' = $DeadlineUtc.ToUniversalTime().ToString('O')
        '__STATUS_PATH__' = $status
    }
    foreach ($placeholder in $literalValues.Keys) {
        $literal = $literalValues[$placeholder] | ConvertTo-Json -Compress
        $supervisorScript = $supervisorScript.Replace($placeholder, $literal)
    }
    $encoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($supervisorScript))
    $pwshPath = Join-Path $PSHOME 'pwsh.exe'
    $stdoutPath = Join-Path $statusParent 'supervisor.stdout.log'
    $stderrPath = Join-Path $statusParent 'supervisor.stderr.log'
    try {
        $process = Start-Process -FilePath $pwshPath -ArgumentList @('-NoLogo', '-NoProfile', '-NonInteractive', '-EncodedCommand', $encoded) -WorkingDirectory $statusParent -PassThru -WindowStyle Hidden -RedirectStandardOutput $stdoutPath -RedirectStandardError $stderrPath
        [pscustomobject]@{ Process = $process; StatusPath = $status; ClientId = $ClientId; NodeId = $NodeId }
    }
    catch {
        throw 'RUST_V2_ACCEPTANCE_SUPERVISOR_START_FAILED'
    }
}

function Get-RuntimeAcceptanceSupervisorStatus {
    <# 非阻塞读取独立监督进程写入的原子状态；监督进程异常退出且无状态时立即判 INCONCLUSIVE。 #>
    param([Parameter(Mandatory)] $Supervisor)

    try {
        if (Test-Path -LiteralPath $Supervisor.StatusPath -PathType Leaf) {
            $status = [IO.File]::ReadAllText($Supervisor.StatusPath) | ConvertFrom-Json
            if ([string]::IsNullOrWhiteSpace([string]$status.Diagnostic) -and $status.TimedOut -and $status.ExitConfirmed) {
                $status.Diagnostic = 'RUST_V2_ACCEPTANCE_SUPERVISOR_STATUS_INVALID'
            }
            return $status
        }
        if ($null -eq $Supervisor.Process) {
            return [pscustomobject]@{
                TimedOut = $false; StopAttempted = $false; ExitConfirmed = $false
                Diagnostic = 'RUST_V2_ACCEPTANCE_SUPERVISOR_STATUS_FAILED'
            }
        }
        if (-not [bool]$Supervisor.Process.HasExited) {
            return $null
        }
        [pscustomobject]@{
            TimedOut = $false; StopAttempted = $false; ExitConfirmed = $false
            Diagnostic = 'RUST_V2_ACCEPTANCE_SUPERVISOR_STATUS_MISSING'
        }
    }
    catch {
        [pscustomobject]@{
            TimedOut = $false; StopAttempted = $false; ExitConfirmed = $false
            Diagnostic = 'RUST_V2_ACCEPTANCE_SUPERVISOR_STATUS_INVALID'
        }
    }
}

function Wait-RuntimeAcceptanceSupervisorFinalStatus {
    <# 超时已进入 stopping 时等待监督器写完最终状态；等待有界，禁止 finally 提前取消监督器。 #>
    param(
        [Parameter(Mandatory)] $Supervisor,
        [int] $TimeoutMilliseconds = 6000
    )

    $deadline = [DateTime]::UtcNow.AddMilliseconds([Math]::Max(1, $TimeoutMilliseconds))
    do {
        $status = Get-RuntimeAcceptanceSupervisorStatus -Supervisor $Supervisor
        if ($null -ne $status) {
            $phase = if ($status.PSObject.Properties['Phase']) { [string]$status.Phase } else { '' }
            if ($phase -eq 'complete' -or $phase -eq 'failed' -or
                (-not [bool]$status.TimedOut -and $phase -ne 'stopping')) {
                return $status
            }
        }
        if ($null -ne $Supervisor.Process) {
            try {
                if ([bool]$Supervisor.Process.HasExited -and $null -eq $status) { break }
            }
            catch { break }
        }
        Start-Sleep -Milliseconds 50
    } while ([DateTime]::UtcNow -lt $deadline)
    Get-RuntimeAcceptanceSupervisorStatus -Supervisor $Supervisor
}

function Stop-RuntimeAcceptanceSupervisor {
    <# 提前完成或异常收尾时终止独立监督进程，并在5秒内确认监督器退出。 #>
    param([Parameter(Mandatory)] $Supervisor)

    try {
        if ($null -eq $Supervisor.Process) {
            return [pscustomobject]@{ ExitConfirmed = $true; Diagnostic = '' }
        }
        if (-not [bool]$Supervisor.Process.HasExited) {
            $Supervisor.Process.Kill($true)
        }
        $confirmed = Wait-RuntimeAcceptanceProcessExit -Process $Supervisor.Process -TimeoutMilliseconds 5000
        [pscustomobject]@{
            ExitConfirmed = $confirmed
            Diagnostic = if ($confirmed) { '' } else { 'RUST_V2_ACCEPTANCE_SUPERVISOR_EXIT_UNCONFIRMED' }
        }
    }
    catch {
        [pscustomobject]@{ ExitConfirmed = $false; Diagnostic = 'RUST_V2_ACCEPTANCE_SUPERVISOR_STOP_FAILED' }
    }
}

function Wait-RuntimeAcceptanceProcessExit {
    <# 在 Stop-Process 后用有界 WaitForExit 确认目标已退出，禁止仅凭调用成功继续收尾。 #>
    param(
        [Parameter(Mandatory)] $Process,
        [int] $TimeoutMilliseconds = 5000
    )

    try {
        if ([bool]$Process.HasExited) { return $true }
        [bool]$Process.WaitForExit($TimeoutMilliseconds)
    }
    catch {
        $false
    }
}

function Get-NormalizedAbsolutePath {
    <# 统一绝对路径大小写无关比较，避免路径别名绕过隔离边界。 #>
    param([Parameter(Mandatory)] [string] $Path)

    if ([string]::IsNullOrWhiteSpace($Path)) {
        return ''
    }
    [IO.Path]::GetFullPath($Path).TrimEnd('\')
}

function Test-PathWithin {
    <# 判断 candidate 是否位于 root 内（包含 root 本身），供正式包和工具隔离校验使用。 #>
    param(
        [Parameter(Mandatory)] [string] $Candidate,
        [Parameter(Mandatory)] [string] $Root
    )

    $candidatePath = Get-NormalizedAbsolutePath -Path $Candidate
    $rootPath = Get-NormalizedAbsolutePath -Path $Root
    if (-not $candidatePath -or -not $rootPath) {
        return $false
    }
    $candidatePath.Equals($rootPath, [StringComparison]::OrdinalIgnoreCase) -or
        $candidatePath.StartsWith(($rootPath + '\'), [StringComparison]::OrdinalIgnoreCase)
}

function Get-FileSha256OrNull {
    <# 计算真实文件 SHA-256；文件缺失时保持 null，禁止伪造 manifest hash。 #>
    param([string] $Path)

    if ([string]::IsNullOrWhiteSpace($Path) -or -not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        return $null
    }
    (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Get-DatabaseSnapshotFileState {
    <# 读取数据库三件套的长度和 SHA；缺失旁车文件以 null 表示，供前后稳定性比较。 #>
    param(
        [Parameter(Mandatory)] [string] $Path,
        [Parameter(Mandatory)] [string] $Role
    )

    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        return $null
    }
    $file = Get-Item -LiteralPath $Path -ErrorAction Stop
    [pscustomobject]@{
        Role = $Role
        Name = $file.Name
        Path = Get-NormalizedAbsolutePath -Path $file.FullName
        Length = [long]$file.Length
        Sha256 = Get-FileSha256OrNull -Path $file.FullName
    }
}

function Write-DatabaseSnapshotMetadata {
    <# 使用 CreateNew 原子写入快照 metadata，避免并发或重试覆盖既有证据。 #>
    param(
        [Parameter(Mandatory)] [string] $Path,
        [Parameter(Mandatory)] $Metadata
    )

    $metadataPath = Get-NormalizedAbsolutePath -Path $Path
    $bytes = [Text.UTF8Encoding]::new($false).GetBytes(($Metadata | ConvertTo-Json -Depth 8))
    $stream = $null
    try {
        # FileMode.CreateNew 在内核层保证目标已存在时失败，FileShare.Read 允许审计读取。
        $stream = [IO.File]::Open(
            $metadataPath,
            [IO.FileMode]::CreateNew,
            [IO.FileAccess]::Write,
            [IO.FileShare]::Read)
        $stream.Write($bytes, 0, $bytes.Length)
        $stream.Flush($true)
    }
    catch [IO.IOException] {
        if (Test-Path -LiteralPath $metadataPath -PathType Leaf) {
            throw 'RUST_V2_ACCEPTANCE_DATABASE_SNAPSHOT_METADATA_EXISTS'
        }
        throw
    }
    finally {
        if ($null -ne $stream) {
            $stream.Dispose()
        }
    }
}

function New-ReadOnlyDatabaseSnapshot {
    <# Node/Worker 全停后复制 node.db/WAL/SHM，验证源稳定性并生成不可覆盖的只读快照证据。 #>
    param(
        [Parameter(Mandatory)] [string] $DatabasePath,
        [Parameter(Mandatory)] [string] $EvidenceRoot
    )

    $database = Get-NormalizedAbsolutePath -Path $DatabasePath
    $evidence = Get-NormalizedAbsolutePath -Path $EvidenceRoot
    if (-not (Test-Path -LiteralPath $database -PathType Leaf)) {
        throw 'RUST_V2_ACCEPTANCE_DATABASE_SNAPSHOT_SOURCE_MISSING:node.db'
    }
    if (-not (Test-Path -LiteralPath $evidence -PathType Container)) {
        throw 'RUST_V2_ACCEPTANCE_DATABASE_SNAPSHOT_EVIDENCE_MISSING'
    }

    # 仅在快照目录为空时创建；碰撞即失败，保留既有目录作为历史旁证。
    $snapshotRoot = Join-Path $evidence ("database-snapshot-" + [Guid]::NewGuid().ToString('N'))
    if (Test-Path -LiteralPath $snapshotRoot) {
        throw 'RUST_V2_ACCEPTANCE_DATABASE_SNAPSHOT_TARGET_EXISTS'
    }
    New-Item -ItemType Directory -Path $snapshotRoot -ErrorAction Stop | Out-Null

    $definitions = @(
        [pscustomobject]@{ Role = 'database'; Path = $database }
        [pscustomobject]@{ Role = 'wal'; Path = "$database-wal" }
        [pscustomobject]@{ Role = 'shm'; Path = "$database-shm" }
    )
    foreach ($definition in $definitions) {
        if (-not (Test-Path -LiteralPath $definition.Path -PathType Leaf)) {
            throw "RUST_V2_ACCEPTANCE_DATABASE_SNAPSHOT_SIDECAR_MISSING:$([IO.Path]::GetFileName($definition.Path))"
        }
    }
    $before = @(
        foreach ($definition in $definitions) {
            $state = Get-DatabaseSnapshotFileState -Path $definition.Path -Role $definition.Role
            if ($null -ne $state) { $state }
        }
    )
    if (@($before | Where-Object { $_.Role -eq 'database' }).Count -ne 1) {
        throw 'RUST_V2_ACCEPTANCE_DATABASE_SNAPSHOT_SOURCE_MISSING:node.db'
    }

    $snapshotFiles = @(
        foreach ($source in $before) {
            $target = Join-Path $snapshotRoot $source.Name
            if (Test-Path -LiteralPath $target) {
                throw "RUST_V2_ACCEPTANCE_DATABASE_SNAPSHOT_TARGET_EXISTS:$($source.Name)"
            }
            # File.Copy 的 overwrite=false 保证不会覆盖任何已有旁证。
            [IO.File]::Copy($source.Path, $target, $false)
            [IO.File]::SetAttributes(
                $target,
                ([IO.File]::GetAttributes($target) -bor [IO.FileAttributes]::ReadOnly))
            $copy = Get-DatabaseSnapshotFileState -Path $target -Role $source.Role
            if ($copy.Length -ne $source.Length -or $copy.Sha256 -cne $source.Sha256) {
                throw "RUST_V2_ACCEPTANCE_DATABASE_SNAPSHOT_COPY_INVALID:$($source.Name)"
            }
            [pscustomobject]@{
                role = $source.Role
                name = $source.Name
                source_path = $source.Path
                snapshot_path = $copy.Path
                source_length_before = $source.Length
                source_sha256_before = $source.Sha256
                snapshot_length = $copy.Length
                snapshot_sha256 = $copy.Sha256
                snapshot_read_only = $true
            }
        }
    )

    $after = @(
        foreach ($definition in $definitions) {
            $state = Get-DatabaseSnapshotFileState -Path $definition.Path -Role $definition.Role
            if ($null -ne $state) { $state }
        }
    )
    $beforeNames = @($before | ForEach-Object { $_.Role }) -join ','
    $afterNames = @($after | ForEach-Object { $_.Role }) -join ','
    if ($beforeNames -cne $afterNames) {
        throw 'RUST_V2_ACCEPTANCE_DATABASE_SNAPSHOT_SOURCE_SET_CHANGED'
    }
    foreach ($sourceBefore in $before) {
        $sourceAfter = @($after | Where-Object { $_.Role -eq $sourceBefore.Role }) | Select-Object -First 1
        if ($null -eq $sourceAfter -or $sourceBefore.Length -ne $sourceAfter.Length -or
            $sourceBefore.Sha256 -cne $sourceAfter.Sha256) {
            throw "RUST_V2_ACCEPTANCE_DATABASE_SNAPSHOT_SOURCE_CHANGED:$($sourceBefore.Name)"
        }
        $snapshotFile = @($snapshotFiles | Where-Object { $_.role -eq $sourceBefore.Role }) | Select-Object -First 1
        $snapshotFile | Add-Member -NotePropertyName source_length_after -NotePropertyValue $sourceAfter.Length
        $snapshotFile | Add-Member -NotePropertyName source_sha256_after -NotePropertyValue $sourceAfter.Sha256
    }

    $metadataPath = Join-Path $snapshotRoot 'snapshot-metadata.json'
    $metadata = [ordered]@{
        schema_version = 1
        status = 'PASS'
        source_database_path = $database
        snapshot_root = Get-NormalizedAbsolutePath -Path $snapshotRoot
        source_stability_verified = $true
        created_utc = [DateTime]::UtcNow.ToString('O')
        files = @($snapshotFiles)
    }
    Write-DatabaseSnapshotMetadata -Path $metadataPath -Metadata $metadata

    $databaseSnapshot = @($snapshotFiles | Where-Object { $_.role -eq 'database' }) | Select-Object -First 1
    [pscustomobject]@{
        SnapshotRoot = Get-NormalizedAbsolutePath -Path $snapshotRoot
        DatabasePath = [string]$databaseSnapshot.snapshot_path
        MetadataPath = Get-NormalizedAbsolutePath -Path $metadataPath
        Files = @($snapshotFiles)
    }
}

function Get-ManifestSha256OrNull {
    <# 读取正式包 manifest/files.sha256 的实际内容 SHA；缺失显式返回 null。 #>
    param([string] $ReleaseRoot)

    Get-FileSha256OrNull -Path (Join-Path $ReleaseRoot 'manifest\files.sha256')
}

function Get-TextSha256 {
    <# 对 UTF-8 文本计算稳定 SHA-256，用于规范化媒体清单和配置正文。 #>
    param([Parameter(Mandatory)] [string] $Text)

    $bytes = [Text.UTF8Encoding]::new($false).GetBytes($Text)
    $sha = [Security.Cryptography.SHA256]::Create()
    try {
        ([BitConverter]::ToString($sha.ComputeHash($bytes))).Replace('-', '').ToLowerInvariant()
    }
    finally {
        $sha.Dispose()
    }
}

function Get-CanonicalMediaRootPath {
    <# 将已验证媒体目录转换为稳定绝对路径；盘符根保留结尾反斜杠。 #>
    param([Parameter(Mandatory)] [string] $Path)

    $fullPath = [IO.Path]::GetFullPath($Path)
    $trimmed = $fullPath.TrimEnd('\')
    if ($trimmed -match '^[A-Za-z]:$') {
        return $trimmed + '\'
    }
    $trimmed
}

function Resolve-RuntimeMediaRoots {
    <# 校验并按调用顺序解析媒体根，拒绝空值、重复、嵌套和根目录重解析点。 #>
    param(
        [string[]] $MediaRoot = @(),
        [string[]] $MediaRoots = @()
    )

    $primaryRoots = @($MediaRoot | Where-Object { -not [string]::IsNullOrWhiteSpace([string]$_ ) })
    $legacyRoots = @($MediaRoots | Where-Object { -not [string]::IsNullOrWhiteSpace([string]$_ ) })
    if ($primaryRoots.Count -gt 0 -and $legacyRoots.Count -gt 0) {
        throw 'RUST_V2_REAL_MEDIA_ROOTS_BOTH_ARGUMENTS'
    }
    if ($primaryRoots.Count -eq 0 -and $legacyRoots.Count -eq 0) {
        throw 'RUST_V2_REAL_MEDIA_ROOT_MISSING'
    }

    # MediaRoot 现在直接支持多根；MediaRoots 只作为旧调用方的过渡别名。
    $candidates = if ($legacyRoots.Count -gt 0) { $legacyRoots } else { $primaryRoots }
    $resolved = [Collections.Generic.List[string]]::new()
    $seen = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    foreach ($candidate in $candidates) {
        if ([string]::IsNullOrWhiteSpace([string]$candidate)) {
            throw 'RUST_V2_REAL_MEDIA_ROOT_INVALID'
        }
        # IsPathRooted 会把 C:relative 与 \relative 误认为绝对；这里要求盘符和根分隔符都完整。
        if (-not [IO.Path]::IsPathFullyQualified([string]$candidate)) {
            throw 'RUST_V2_REAL_MEDIA_ROOT_NOT_ABSOLUTE'
        }
        try {
            $item = Get-Item -LiteralPath ([string]$candidate) -ErrorAction Stop
        }
        catch {
            throw 'RUST_V2_REAL_MEDIA_ROOT_INVALID'
        }
        if (-not $item.PSIsContainer) {
            throw 'RUST_V2_REAL_MEDIA_ROOT_INVALID'
        }
        if (([IO.File]::GetAttributes($item.FullName) -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw 'RUST_V2_REAL_MEDIA_ROOT_REPARSE_POINT'
        }
        $canonical = Get-CanonicalMediaRootPath -Path $item.FullName
        if (-not $seen.Add($canonical)) {
            throw 'RUST_V2_REAL_MEDIA_ROOTS_DUPLICATE'
        }
        [void]$resolved.Add($canonical)
    }

    for ($leftIndex = 0; $leftIndex -lt $resolved.Count; $leftIndex++) {
        for ($rightIndex = $leftIndex + 1; $rightIndex -lt $resolved.Count; $rightIndex++) {
            $left = $resolved[$leftIndex].TrimEnd('\')
            $right = $resolved[$rightIndex].TrimEnd('\')
            if ($left.Equals($right, [StringComparison]::OrdinalIgnoreCase) -or
                $left.StartsWith(($right + '\'), [StringComparison]::OrdinalIgnoreCase) -or
                $right.StartsWith(($left + '\'), [StringComparison]::OrdinalIgnoreCase)) {
                throw 'RUST_V2_REAL_MEDIA_ROOTS_NESTED'
            }
        }
    }
    [string[]]$resolved.ToArray()
}

function Assert-RuntimeAcceptanceMediaRoots {
    <# 验收入口只允许本次双物理盘的两个规范化媒体根，避免只按盘符误绑定其它目录。 #>
    param([Parameter(Mandatory)] [string[]] $MediaRoots)

    $expected = @('H:\pik\00000000000', 'I:\tmp')
    $actual = @($MediaRoots | ForEach-Object { Get-CanonicalMediaRootPath -Path ([string]$_) })
    if ($actual.Count -ne $expected.Count) {
        throw 'RUST_V2_REAL_MEDIA_ROOTS_NOT_APPROVED'
    }
    for ($index = 0; $index -lt $expected.Count; $index++) {
        if (-not $actual[$index].Equals($expected[$index], [StringComparison]::OrdinalIgnoreCase)) {
            throw 'RUST_V2_REAL_MEDIA_ROOTS_NOT_APPROVED'
        }
    }
    [string[]]$actual
}

function Get-RuntimePhysicalDiskMap {
    <# 通过 Storage API 将每个本地盘符根绑定到物理盘；重复 DiskNumber 立即拒绝。 #>
    param(
        [Parameter(Mandatory)] [string[]] $MediaRoots,
        [switch] $RequireDistinctPhysicalDisks,
        [scriptblock] $PartitionResolver,
        [scriptblock] $DiskResolver
    )

    $entries = [Collections.Generic.List[object]]::new()
    foreach ($root in @($MediaRoots)) {
        $absoluteRoot = [IO.Path]::GetFullPath([string]$root)
        if ($absoluteRoot -notmatch '^[A-Za-z]:\\') {
            throw 'RUST_V2_ACCEPTANCE_PHYSICAL_DISK_ROOT_NOT_LOCAL'
        }
        $driveLetter = $absoluteRoot.Substring(0, 1).ToUpperInvariant()
        try {
            $partition = if ($PartitionResolver) {
                @(& $PartitionResolver $driveLetter) | Select-Object -First 1
            }
            else {
                @(Get-Partition -DriveLetter $driveLetter -ErrorAction Stop) | Select-Object -First 1
            }
        }
        catch {
            throw "RUST_V2_ACCEPTANCE_PHYSICAL_DISK_MAP_FAILED:$driveLetter"
        }
        if ($null -eq $partition -or $null -eq $partition.PSObject.Properties['DiskNumber'] -or
            $null -eq $partition.DiskNumber) {
            throw "RUST_V2_ACCEPTANCE_PHYSICAL_DISK_MAP_FAILED:$driveLetter"
        }
        $diskNumber = [int]$partition.DiskNumber
        try {
            $disk = if ($DiskResolver) {
                @(& $DiskResolver $diskNumber) | Select-Object -First 1
            }
            else {
                Get-Disk -Number $diskNumber -ErrorAction Stop
            }
        }
        catch {
            throw "RUST_V2_ACCEPTANCE_PHYSICAL_DISK_MAP_FAILED:$driveLetter"
        }
        if ($null -eq $disk) {
            throw "RUST_V2_ACCEPTANCE_PHYSICAL_DISK_MAP_FAILED:$driveLetter"
        }
        [void]$entries.Add([ordered]@{
                root = $absoluteRoot.TrimEnd('\')
                drive_letter = $driveLetter
                partition_number = if ($null -ne $partition.PSObject.Properties['PartitionNumber']) { [int]$partition.PartitionNumber } else { $null }
                disk_number = $diskNumber
                friendly_name = if ($null -ne $disk.PSObject.Properties['FriendlyName']) { [string]$disk.FriendlyName } else { '' }
                bus_type = if ($null -ne $disk.PSObject.Properties['BusType']) { [string]$disk.BusType } else { '' }
            })
    }
    $distinct = @($entries | ForEach-Object { [int]$_.disk_number } | Sort-Object -Unique)
    if ($RequireDistinctPhysicalDisks -and $distinct.Count -ne @($entries).Count) {
        throw 'RUST_V2_ACCEPTANCE_PHYSICAL_DISK_NOT_DISTINCT'
    }
    [ordered]@{
        schema = 'rust-v2-physical-disk-map/v1'
        roots = @($entries)
        entries = @($entries)
        distinct_disk_numbers = $distinct
    }
}

function Assert-RuntimeAcceptanceInputs {
    <# 验证所有外部输入；失败时不创建目录、不启动进程。 #>
    param(
        [string[]] $MediaRoot,
        [string[]] $MediaRoots = @(),
        [int] $DurationSeconds,
        [int] $SampleSeconds,
        [string] $ReleaseRoot,
        [string] $AcceptanceClientPath,
        [string] $ResultExporterPath,
        [string] $EvidenceRoot,
        [string] $ReportPath,
        [string] $Variant = 'A',
        [int] $RunIndex = 1,
        [int] $WorkerCount = 20,
        [int] $HddThreadsPerDisk = 1,
        [int] $SsdThreadsPerDisk = 16,
        [int] $UnknownThreadsPerDisk = 1,
        [int] $TotalReadThreads = 12,
        [int] $ReservedCores = 1,
        [string] $Enumerator = 'everything',
        [switch] $SingleRun,
        [switch] $CompleteWhenTaskTerminal,
        [switch] $RequireDistinctPhysicalDisks,
        [switch] $ThrowOnError = $true
    )

    $code = ''
    try {
        $null = Resolve-RuntimeMediaRoots -MediaRoot $MediaRoot -MediaRoots $MediaRoots
    }
    catch {
        $code = $_.Exception.Message
    }
    if (-not $code -and $DurationSeconds -lt 1800) {
        $code = 'RUST_V2_ACCEPTANCE_DURATION_INVALID'
    }
    elseif (-not $code -and $SampleSeconds -ne 2) {
        $code = 'RUST_V2_ACCEPTANCE_SAMPLE_INVALID'
    }
    elseif (-not $code -and ([string]$Enumerator).Trim().ToLowerInvariant() -notin @('everything', 'windows_walker')) {
        $code = 'RUST_V2_ACCEPTANCE_ENUMERATOR_INVALID'
    }
    elseif (-not $code -and $Variant -notin @('A', 'B')) {
        $code = 'RUST_V2_ACCEPTANCE_VARIANT_INVALID'
    }
    elseif (-not $code -and ($RunIndex -lt 1 -or $RunIndex -gt 3)) {
        $code = 'RUST_V2_ACCEPTANCE_RUN_INDEX_INVALID'
    }
    elseif (-not $code -and ($WorkerCount -lt 1 -or $WorkerCount -gt 256)) {
        $code = 'RUST_V2_ACCEPTANCE_WORKER_COUNT_INVALID'
    }
    elseif (-not $code -and ($HddThreadsPerDisk -lt 1 -or $HddThreadsPerDisk -gt 64 -or
        $SsdThreadsPerDisk -lt 1 -or $SsdThreadsPerDisk -gt 64 -or
        $UnknownThreadsPerDisk -lt 1 -or $UnknownThreadsPerDisk -gt 64 -or
        $TotalReadThreads -lt 1 -or $TotalReadThreads -gt 256)) {
        $code = 'RUST_V2_ACCEPTANCE_READ_THREADS_INVALID'
    }
    elseif (-not $code -and ($ReservedCores -lt 0 -or $ReservedCores -gt 255)) {
        $code = 'RUST_V2_ACCEPTANCE_RESERVED_CORES_INVALID'
    }
    elseif (-not $code -and ([string]::IsNullOrWhiteSpace($ReleaseRoot) -or
        -not (Test-Path -LiteralPath $ReleaseRoot -PathType Container))) {
        $code = 'RUST_V2_ACCEPTANCE_RELEASE_ROOT_INVALID'
    }
    elseif (-not $code -and ([string]::IsNullOrWhiteSpace($AcceptanceClientPath) -or
        [string]::IsNullOrWhiteSpace($ResultExporterPath))) {
        $code = 'RUST_V2_ACCEPTANCE_TOOLS_MISSING'
    }
    elseif (-not $code -and (-not [IO.Path]::IsPathFullyQualified($AcceptanceClientPath) -or
        -not [IO.Path]::IsPathFullyQualified($ResultExporterPath))) {
        $code = 'RUST_V2_ACCEPTANCE_TOOLS_PATH_INVALID'
    }
    elseif (-not $code -and ((Test-PathWithin -Candidate $AcceptanceClientPath -Root $ReleaseRoot) -or
        (Test-PathWithin -Candidate $ResultExporterPath -Root $ReleaseRoot))) {
        $code = 'RUST_V2_ACCEPTANCE_TOOL_INSIDE_RELEASE'
    }
    elseif (-not $code -and -not (Test-Path -LiteralPath $AcceptanceClientPath -PathType Leaf)) {
        $code = 'RUST_V2_ACCEPTANCE_CLIENT_MISSING'
    }
    elseif (-not $code -and -not (Test-Path -LiteralPath $ResultExporterPath -PathType Leaf)) {
        $code = 'RUST_V2_ACCEPTANCE_EXPORTER_MISSING'
    }
    elseif (-not $code -and ([string]::IsNullOrWhiteSpace($EvidenceRoot) -or
        [string]::IsNullOrWhiteSpace($ReportPath))) {
        $code = 'RUST_V2_ACCEPTANCE_EVIDENCE_PATH_INVALID'
    }
    elseif (-not $code -and (-not [IO.Path]::IsPathFullyQualified($EvidenceRoot) -or
        -not [IO.Path]::IsPathFullyQualified($ReportPath))) {
        $code = 'RUST_V2_ACCEPTANCE_EVIDENCE_PATH_INVALID'
    }
    elseif (-not $code) {
        $releasePath = Get-NormalizedAbsolutePath -Path $ReleaseRoot
        $evidencePath = Get-NormalizedAbsolutePath -Path $EvidenceRoot
        $reportPath = Get-NormalizedAbsolutePath -Path $ReportPath
        if ((Test-PathWithin -Candidate $evidencePath -Root $releasePath) -or
            (Test-PathWithin -Candidate $reportPath -Root $releasePath)) {
            $code = 'RUST_V2_ACCEPTANCE_EVIDENCE_PATH_INVALID'
        }
        elseif ($reportPath.Equals($evidencePath, [StringComparison]::OrdinalIgnoreCase)) {
            $code = 'RUST_V2_ACCEPTANCE_REPORT_PATH_INVALID'
        }
        elseif (-not (Test-PathWithin -Candidate $reportPath -Root $evidencePath)) {
            $code = 'RUST_V2_ACCEPTANCE_REPORT_OUTSIDE_EVIDENCE'
        }
        elseif (Test-Path -LiteralPath $EvidenceRoot) {
            $code = 'RUST_V2_ACCEPTANCE_EVIDENCE_EXISTS'
        }
        elseif (Test-Path -LiteralPath $ReportPath) {
            $code = 'RUST_V2_ACCEPTANCE_REPORT_EXISTS'
        }
    }
    if (-not $code -and $ReleaseRoot) {
        foreach ($name in @('desktop.exe', 'node.exe', 'worker.exe', 'Everything.exe')) {
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
    param(
        [string] $RunId = [Guid]::NewGuid().ToString('N'),
        [string] $RequestedEvidenceRoot = '',
        [string] $RequestedReportPath = ''
    )

    if ($RunId -notmatch '^[A-Za-z0-9-]+$') {
        throw 'RUST_V2_ACCEPTANCE_RUN_ID_INVALID'
    }
    $root = if ([string]::IsNullOrWhiteSpace($RequestedEvidenceRoot)) {
        [IO.Path]::GetFullPath((Join-Path $script:AcceptanceRoot $RunId))
    }
    else {
        $evidence = Get-NormalizedAbsolutePath -Path $RequestedEvidenceRoot
        if (-not $evidence.EndsWith('\evidence', [StringComparison]::OrdinalIgnoreCase)) {
            throw 'RUST_V2_ACCEPTANCE_EVIDENCE_PATH_INVALID'
        }
        Split-Path -Parent $evidence
    }
    if ([string]::IsNullOrWhiteSpace($root)) {
        throw 'RUST_V2_ACCEPTANCE_LAYOUT_INVALID'
    }
    $evidencePath = if ([string]::IsNullOrWhiteSpace($RequestedEvidenceRoot)) {
        Join-Path $root 'evidence'
    }
    else {
        Get-NormalizedAbsolutePath -Path $RequestedEvidenceRoot
    }
    $reportPath = if ([string]::IsNullOrWhiteSpace($RequestedReportPath)) {
        Join-Path $evidencePath 'report.md'
    }
    else {
        Get-NormalizedAbsolutePath -Path $RequestedReportPath
    }
    [pscustomobject]@{
        Root = $root
        Release = Join-Path $root 'release'
        # Node artifact registry 要求 cache 位于安装根内；数据、日志和缓存随 Release 一起隔离。
        Data = Join-Path $root 'release\data\node'
        Logs = Join-Path $root 'release\data\node\logs'
        Cache = Join-Path $root 'release\data\node\cache'
        Temp = Join-Path $root 'temp'
        Evidence = $evidencePath
        Report = $reportPath
        Tools = Join-Path $root 'tools'
    }
}

function ConvertTo-TomlBasicString {
    <# 将路径编码为 TOML basic string，先转义反斜杠再转义引号。 #>
    param([Parameter(Mandatory)] [string] $Value)

    $escaped = $Value.Replace('\', '\\').Replace('"', '\"')
    '"' + $escaped + '"'
}

function New-IsolatedNodeConfig {
    <# 写入相对 node.exe 根解释的完整配置正文；枚举器默认使用 Everything。 #>
    param(
        [int] $Port,
        [int] $WorkerCount = 20,
        [int] $HddThreadsPerDisk = 1,
        [int] $SsdThreadsPerDisk = 16,
        [int] $UnknownThreadsPerDisk = 1,
        [int] $TotalReadThreads = 12,
        [int] $ReservedCores = 1,
        [ValidateSet('everything', 'windows_walker')]
        [string] $Enumerator = 'everything',
        [string] $DataRoot = ''
    )

    $dataPath = if ([string]::IsNullOrWhiteSpace($DataRoot)) { 'data/node' } else { Get-NormalizedAbsolutePath -Path $DataRoot }
    $configPath = if ([string]::IsNullOrWhiteSpace($DataRoot)) { 'data/node/config.toml' } else { Join-Path $dataPath 'config.toml' }
    $logPath = if ([string]::IsNullOrWhiteSpace($DataRoot)) { 'data/node/logs' } else { Join-Path $dataPath 'logs' }
    $cachePath = if ([string]::IsNullOrWhiteSpace($DataRoot)) { 'data/node/cache' } else { Join-Path $dataPath 'cache' }
    $tomlDataPath = ConvertTo-TomlBasicString -Value $dataPath
    $tomlConfigPath = ConvertTo-TomlBasicString -Value $configPath
    $tomlLogPath = ConvertTo-TomlBasicString -Value $logPath
    $tomlCachePath = ConvertTo-TomlBasicString -Value $cachePath
    $enum = ([string]$Enumerator).Trim().ToLowerInvariant()
    if ($enum -notin @('everything', 'windows_walker')) {
        throw 'RUST_V2_ACCEPTANCE_ENUMERATOR_INVALID'
    }

@"
listen_ip = "127.0.0.1"
port = $Port
worker_count = $WorkerCount
# 默认使用正式产品的 Everything 枚举器；调用方仍可显式选择 Windows Walker。
enumerator = "$enum"

[paths]
data_path = $tomlDataPath
config_path = $tomlConfigPath
log_path = $tomlLogPath
cache_path = $tomlCachePath

[read]
hdd_threads_per_disk = $HddThreadsPerDisk
ssd_threads_per_disk = $SsdThreadsPerDisk
unknown_threads_per_disk = $UnknownThreadsPerDisk
total_threads = $TotalReadThreads
block_size_bytes = 4194304
block_timeout_seconds = 3
block_retries = 2

[worker]
mode = "manual"
reserved_cores = $ReservedCores
manual_worker_count = $WorkerCount
"@
}

function Get-RuntimeSingleMediaManifest {
    <# 读取一个媒体根的相对路径清单；不读取正文、不计算媒体哈希。 #>
    param([Parameter(Mandatory)] [string] $MediaRoot)

    $root = Get-CanonicalMediaRootPath -Path $MediaRoot
    $rootPrefixLength = $root.TrimEnd('\').Length
    $files = @(
        Get-ChildItem -LiteralPath $root -Recurse -File -Force -ErrorAction Stop |
            ForEach-Object {
                # PowerShell 5.1 没有 Path.GetRelativePath；直接截取已验证根目录后的相对部分。
                $relativePath = $_.FullName.Substring($rootPrefixLength).TrimStart('\').Replace('\', '/')
                [pscustomobject]@{
                    Path = $relativePath
                    Length = [long]$_.Length
                    LastWriteTimeUtc = $_.LastWriteTimeUtc.ToString('O')
                }
            } |
            Sort-Object -Property Path
    )
    $totalBytes = ($files | Measure-Object -Property Length -Sum).Sum
    if ($null -eq $totalBytes) { $totalBytes = 0 }
    [pscustomobject]@{
        Root = $root
        FileCount = $files.Count
        TotalBytes = [long]$totalBytes
        Files = $files
    }
}

function Get-RuntimeMediaManifest {
    <# 单根保持 v1；多根生成按根序号和相对路径排序的 canonical v2 清单。 #>
    param(
        [string] $MediaRoot = '',
        [string[]] $MediaRoots = @()
    )

    $roots = @(Resolve-RuntimeMediaRoots -MediaRoot $MediaRoot -MediaRoots $MediaRoots)
    if (@($roots).Count -eq 1) {
        return Get-RuntimeSingleMediaManifest -MediaRoot $roots[0]
    }

    $singleManifests = @(
        foreach ($root in @($roots)) {
            Get-RuntimeSingleMediaManifest -MediaRoot $root
        }
    )
    $flattenedFiles = [Collections.Generic.List[object]]::new()
    for ($rootIndex = 0; $rootIndex -lt $singleManifests.Count; $rootIndex++) {
        foreach ($file in @($singleManifests[$rootIndex].Files)) {
            [void]$flattenedFiles.Add([pscustomobject]@{
                    RootIndex = $rootIndex + 1
                    Root = [string]$singleManifests[$rootIndex].Root
                    Path = [string]$file.Path
                    Length = [long]$file.Length
                    LastWriteTimeUtc = [string]$file.LastWriteTimeUtc
                })
        }
    }
    $sortedFiles = @($flattenedFiles | Sort-Object -Property RootIndex, Path)
    $totalBytes = ($singleManifests.TotalBytes | Measure-Object -Sum).Sum
    if ($null -eq $totalBytes) { $totalBytes = 0 }
    [pscustomobject]@{
        Schema = 'rust-v2-media-manifest/v2'
        Roots = [string[]]$roots
        FileCount = $sortedFiles.Count
        TotalBytes = [long]$totalBytes
        Files = $sortedFiles
    }
}

function Get-CanonicalMediaFilesJson {
    <# 将清单字段按固定顺序和时间精度序列化，避免 JSON 解析造成时间字符串格式漂移。 #>
    param([Parameter(Mandatory)] [object[]] $Files)

    $normalized = @(
        foreach ($file in @($Files)) {
            $rawTimestamp = $file.LastWriteTimeUtc
            $timestamp = [string]$rawTimestamp
            try {
                $timestamp = if ($rawTimestamp -is [DateTime]) {
                    $rawTimestamp.ToUniversalTime().ToString('O')
                }
                elseif ($rawTimestamp -is [DateTimeOffset]) {
                    $rawTimestamp.ToUniversalTime().ToString('O')
                }
                else {
                    ([DateTime]::Parse(
                            [string]$rawTimestamp,
                            [Globalization.CultureInfo]::InvariantCulture,
                            [Globalization.DateTimeStyles]::RoundtripKind)).ToUniversalTime().ToString('O')
                }
            }
            catch {
                # 非标准时间交给后续比较判定变化，不悄悄改成当前时间。
            }
            $row = [ordered]@{
                Path = [string]$file.Path
                Length = [long]$file.Length
                LastWriteTimeUtc = $timestamp
            }
            if ($null -ne $file.PSObject.Properties['RootIndex']) {
                $row = [ordered]@{
                    RootIndex = [int]$file.RootIndex
                    Root = [string]$file.Root
                    Path = [string]$file.Path
                    Length = [long]$file.Length
                    LastWriteTimeUtc = $timestamp
                }
            }
            $row
        }
    )
    if ($normalized.Count -eq 0) { return '[]' }
    ConvertTo-Json -InputObject ([object[]]$normalized) -Depth 8 -Compress
}

function Get-CanonicalMediaTimestamp {
    <# 将文件时间统一为 UTC 七位小数；兼容 ConvertFrom-Json 产生的 DateTime 值。 #>
    param([Parameter(Mandatory)] $Value)

    try {
        if ($Value -is [DateTime]) { return $Value.ToUniversalTime().ToString('O') }
        if ($Value -is [DateTimeOffset]) { return $Value.ToUniversalTime().ToString('O') }
        return ([DateTime]::Parse(
                [string]$Value,
                [Globalization.CultureInfo]::InvariantCulture,
                [Globalization.DateTimeStyles]::RoundtripKind)).ToUniversalTime().ToString('O')
    }
    catch {
        [string]$Value
    }
}

function Test-CanonicalMediaFilesEqual {
    <# 逐项比较根序号、路径、长度和 UTC 修改时间，避免 JSON 格式差异影响只读证明。 #>
    param(
        [Parameter(Mandatory)] [object[]] $BeforeFiles,
        [Parameter(Mandatory)] [object[]] $AfterFiles
    )

    if ($BeforeFiles.Count -ne $AfterFiles.Count) { return $false }
    for ($index = 0; $index -lt $BeforeFiles.Count; $index++) {
        $beforeFile = $BeforeFiles[$index]
        $afterFile = $AfterFiles[$index]
        $beforeRootIndex = if ($null -eq $beforeFile.PSObject.Properties['RootIndex']) { $null } else { [int]$beforeFile.RootIndex }
        $afterRootIndex = if ($null -eq $afterFile.PSObject.Properties['RootIndex']) { $null } else { [int]$afterFile.RootIndex }
        $beforeRoot = if ($null -eq $beforeFile.PSObject.Properties['Root']) { '' } else { [string]$beforeFile.Root }
        $afterRoot = if ($null -eq $afterFile.PSObject.Properties['Root']) { '' } else { [string]$afterFile.Root }
        if ($beforeRootIndex -ne $afterRootIndex -or $beforeRoot -cne $afterRoot -or
            [string]$beforeFile.Path -cne [string]$afterFile.Path -or
            [long]$beforeFile.Length -ne [long]$afterFile.Length -or
            (Get-CanonicalMediaTimestamp -Value $beforeFile.LastWriteTimeUtc) -cne
                (Get-CanonicalMediaTimestamp -Value $afterFile.LastWriteTimeUtc)) {
            return $false
        }
    }
    $true
}

function Assert-RuntimeMediaUnchanged {
    <# 按相对路径、长度和UTC修改时间逐项证明源媒体未变化。 #>
    param(
        [Parameter(Mandatory)] $Before,
        [Parameter(Mandatory)] $After
    )

    $beforeSchema = if ($null -eq $Before.PSObject.Properties['Schema']) { '' } else { [string]$Before.Schema }
    $afterSchema = if ($null -eq $After.PSObject.Properties['Schema']) { '' } else { [string]$After.Schema }
    $filesEqual = Test-CanonicalMediaFilesEqual -BeforeFiles @($Before.Files) -AfterFiles @($After.Files)
    $rootsEqual = if ($beforeSchema -eq 'rust-v2-media-manifest/v2' -or $afterSchema -eq 'rust-v2-media-manifest/v2') {
        $beforeSchema -ceq $afterSchema -and
            (($Before.Roots | ConvertTo-Json -Compress) -ceq ($After.Roots | ConvertTo-Json -Compress))
    }
    else {
        $Before.Root -ceq $After.Root
    }
    if (-not $rootsEqual -or $Before.FileCount -ne $After.FileCount -or
        $Before.TotalBytes -ne $After.TotalBytes -or -not $filesEqual) {
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
    <# 仅复制正式发布白名单；验收客户端和 exporter 永远留在外置 tools。 #>
    param([string] $Source, [string] $Destination)

    New-Item -ItemType Directory -Path $Destination -Force | Out-Null
    foreach ($name in @('desktop.exe', 'node.exe', 'worker.exe', 'Everything.exe')) {
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
    <# 枚举隔离进程；失败必须显式上报，不能静默当成已退出。 #>
    param(
        [string] $Root,
        [scriptblock] $ProcessEnumerator
    )

    try {
        $rows = if ($ProcessEnumerator) {
            @(& $ProcessEnumerator)
        }
        else {
            @(Get-CimInstance Win32_Process -ErrorAction Stop)
        }
    }
    catch {
        throw 'RUST_V2_ACCEPTANCE_PROCESS_ENUMERATION_FAILED'
    }
    @(
        $rows | Where-Object {
            $_.ExecutablePath -and
            $_.ExecutablePath.StartsWith(($Root.TrimEnd('\') + '\'), [StringComparison]::OrdinalIgnoreCase)
        }
    )
}

function Get-ProcessStartTimeUtc {
    <# 读取进程世代时间；优先使用实时 Process，失败时回退 CIM CreationDate。 #>
    param(
        [Parameter(Mandatory)] $Row,
        [Parameter(Mandatory)] $Process
    )

    $startTime = $null
    $processStartProperty = $Process.PSObject.Properties['StartTime']
    if ($null -ne $processStartProperty) {
        try {
            $startTime = ([DateTime]$processStartProperty.Value).ToUniversalTime()
        }
        catch {
            $startTime = $null
        }
    }
    if ($null -eq $startTime) {
        $creationProperty = $Row.PSObject.Properties['CreationDate']
        if ($null -ne $creationProperty -and $null -ne $creationProperty.Value) {
            try {
                $startTime = ([DateTime]$creationProperty.Value).ToUniversalTime()
            }
            catch {
                $startTime = $null
            }
        }
    }
    if ($null -eq $startTime) {
        return ''
    }
    $startTime.ToString('O')
}

function Remove-IsolatedProcessBaselines {
    <# 删除指定 PID 的全部进程世代基线，避免退出进程的累计值残留。 #>
    param(
        [Parameter(Mandatory)] [int] $ProcessId,
        [Parameter(Mandatory)] [hashtable] $PreviousCpu,
        [Parameter(Mandatory)] [hashtable] $PreviousIo
    )

    # PID 前缀覆盖该 PID 的全部启动世代。
    $generationPrefix = "$ProcessId|"
    foreach ($key in @($PreviousCpu.Keys)) {
        if (([string]$key).StartsWith($generationPrefix)) {
            $PreviousCpu.Remove($key)
        }
    }
    foreach ($key in @($PreviousIo.Keys)) {
        if (([string]$key).StartsWith($generationPrefix)) {
            $PreviousIo.Remove($key)
        }
    }
}

function Try-NewIsolatedProcessSample {
    <# 先快照进程属性再更新基线；进程退出时返回稳定 skip 而不中止整轮采样。 #>
    param(
        [Parameter(Mandatory)] $Row,
        [Parameter(Mandatory)] $Process,
        [Parameter(Mandatory)] [hashtable] $PreviousCpu,
        [Parameter(Mandatory)] [hashtable] $PreviousIo,
        [Parameter(Mandatory)] [double] $SampleIntervalMs
    )

    # Row.ProcessId 是本轮 CIM 行的稳定身份，失败时用它清理所有旧世代基线。
    $rowProcessId = [int]$Row.ProcessId
    try {
        # 进程可能在 resolver 返回后立刻退出；先读取原值，禁止 null 被强转为 0 或空字符串。
        $rawProcessId = $Process.Id
        if ($null -eq $rawProcessId) {
            throw 'PROCESS_EXITED_DURING_SAMPLE'
        }
        $processId = [int]$rawProcessId
        if ($rowProcessId -le 0 -or $processId -le 0 -or $processId -ne $rowProcessId) {
            throw 'PROCESS_EXITED_DURING_SAMPLE'
        }

        $rawName = $Process.ProcessName
        if ($null -eq $rawName) {
            throw 'PROCESS_EXITED_DURING_SAMPLE'
        }
        $name = [string]$rawName
        if ([string]::IsNullOrWhiteSpace($name)) {
            throw 'PROCESS_EXITED_DURING_SAMPLE'
        }

        $processStartTimeUtc = Get-ProcessStartTimeUtc -Row $Row -Process $Process
        # 空值或无法解析的世代时间不能退化为 unknown，否则 PID 复用会污染累计基线。
        if ([string]::IsNullOrWhiteSpace($processStartTimeUtc)) {
            throw 'PROCESS_EXITED_DURING_SAMPLE'
        }
        $parsedStartTimeUtc = [DateTime]::MinValue
        if (-not [DateTime]::TryParse(
                $processStartTimeUtc,
                [Globalization.CultureInfo]::InvariantCulture,
                [Globalization.DateTimeStyles]::RoundtripKind,
                [ref]$parsedStartTimeUtc)) {
            throw 'PROCESS_EXITED_DURING_SAMPLE'
        }
        $processStartTimeUtc = $parsedStartTimeUtc.ToUniversalTime().ToString('O')
        $cpuTime = $Process.TotalProcessorTime
        if ($null -eq $cpuTime) {
            throw 'PROCESS_EXITED_DURING_SAMPLE'
        }
        $totalCpuMs = [double]$cpuTime.TotalMilliseconds
        # null 代表读取过程已失效；数值零仍是合法快照，不能使用真假判断。
        $workingSet = $Process.WorkingSet64
        if ($null -eq $workingSet) {
            throw 'PROCESS_EXITED_DURING_SAMPLE'
        }
        $privateMemory = $Process.PrivateMemorySize64
        if ($null -eq $privateMemory) {
            throw 'PROCESS_EXITED_DURING_SAMPLE'
        }
        $workingSetBytes = [long]$workingSet
        $privateMemoryBytes = [long]$privateMemory
        # CIM 累计 I/O 任一字段不可读都跳过本 PID，不能把 null 当作零参与差分。
        $rawReadOperationCount = $Row.ReadOperationCount
        $rawWriteOperationCount = $Row.WriteOperationCount
        $rawReadTransferCount = $Row.ReadTransferCount
        $rawWriteTransferCount = $Row.WriteTransferCount
        $rawOtherTransferCount = $Row.OtherTransferCount
        if ($null -eq $rawReadOperationCount -or
            $null -eq $rawWriteOperationCount -or
            $null -eq $rawReadTransferCount -or
            $null -eq $rawWriteTransferCount -or
            $null -eq $rawOtherTransferCount) {
            throw 'PROCESS_EXITED_DURING_SAMPLE'
        }
        $currentIo = [pscustomobject]@{
            ReadOperationCount = [double]$rawReadOperationCount
            WriteOperationCount = [double]$rawWriteOperationCount
            ReadTransferCount = [double]$rawReadTransferCount
            WriteTransferCount = [double]$rawWriteTransferCount
            OtherTransferCount = [double]$rawOtherTransferCount
        }
    }
    catch {
        Remove-IsolatedProcessBaselines -ProcessId $rowProcessId `
            -PreviousCpu $PreviousCpu -PreviousIo $PreviousIo
        return [pscustomobject]@{
            Sample = $null
            Skip = [pscustomobject]@{
                process_id = $rowProcessId
                reason = 'PROCESS_EXITED_DURING_SAMPLE'
            }
        }
    }

    # Windows 会复用 PID；累计计数只能与同一 PID、同一启动时间的进程世代做差。
    $generationSuffix = if ([string]::IsNullOrWhiteSpace($processStartTimeUtc)) {
        'unknown'
    }
    else {
        $processStartTimeUtc
    }
    $processGenerationKey = "$processId|$generationSuffix"
    $generationPrefix = "$processId|"
    foreach ($key in @($PreviousCpu.Keys)) {
        if ([string]$key -ne $processGenerationKey -and ([string]$key).StartsWith($generationPrefix)) {
            $PreviousCpu.Remove($key)
        }
    }
    foreach ($key in @($PreviousIo.Keys)) {
        if ([string]$key -ne $processGenerationKey -and ([string]$key).StartsWith($generationPrefix)) {
            $PreviousIo.Remove($key)
        }
    }
    $lastCpuMs = if ($PreviousCpu.ContainsKey($processGenerationKey)) {
        [double]$PreviousCpu[$processGenerationKey]
    }
    else {
        $totalCpuMs
    }
    $PreviousCpu[$processGenerationKey] = $totalCpuMs
    $cpuDeltaMs = [Math]::Max([double]0, $totalCpuMs - $lastCpuMs)

    $lastIo = if ($PreviousIo.ContainsKey($processGenerationKey)) {
        $PreviousIo[$processGenerationKey]
    }
    else {
        $currentIo
    }
    $PreviousIo[$processGenerationKey] = $currentIo

    return [pscustomobject]@{
        Sample = [pscustomobject]@{
            Name = $name
            ProcessId = $processId
            ProcessStartTimeUtc = $processStartTimeUtc
            ProcessGenerationKey = $processGenerationKey
            CpuDeltaMs = $cpuDeltaMs
            CpuPercentOfOneCore = if ($SampleIntervalMs -gt 0) {
                [Math]::Round(($cpuDeltaMs / $SampleIntervalMs) * 100, 4)
            }
            else {
                0
            }
            ReadOperationDelta = [long][Math]::Max([double]0, $currentIo.ReadOperationCount - $lastIo.ReadOperationCount)
            WriteOperationDelta = [long][Math]::Max([double]0, $currentIo.WriteOperationCount - $lastIo.WriteOperationCount)
            ReadTransferDeltaBytes = [long][Math]::Max([double]0, $currentIo.ReadTransferCount - $lastIo.ReadTransferCount)
            WriteTransferDeltaBytes = [long][Math]::Max([double]0, $currentIo.WriteTransferCount - $lastIo.WriteTransferCount)
            OtherTransferDeltaBytes = [long][Math]::Max([double]0, $currentIo.OtherTransferCount - $lastIo.OtherTransferCount)
            WorkingSetBytes = $workingSetBytes
            PrivateMemoryBytes = $privateMemoryBytes
        }
        Skip = $null
    }
}

function New-IsolatedProcessSample {
    <# 保持旧调用方的健康进程样本形状，退出进程由 Write-SystemSample 记录 skip。 #>
    param(
        [Parameter(Mandatory)] $Row,
        [Parameter(Mandatory)] $Process,
        [Parameter(Mandatory)] [hashtable] $PreviousCpu,
        [Parameter(Mandatory)] [hashtable] $PreviousIo,
        [Parameter(Mandatory)] [double] $SampleIntervalMs
    )

    # 兼容调用方仍只接收健康进程样本，退出状态维持为异常而非改变返回类型。
    $result = Try-NewIsolatedProcessSample -Row $Row -Process $Process -PreviousCpu $PreviousCpu `
        -PreviousIo $PreviousIo -SampleIntervalMs $SampleIntervalMs
    if ($null -eq $result.Sample) {
        throw 'PROCESS_EXITED_DURING_SAMPLE'
    }
    $result.Sample
}

function New-LogicalProcessorSample {
    <# 保留每个逻辑处理器的忙碌、用户态、内核态、中断与 DPC 百分比。 #>
    param([Parameter(Mandatory)] $Row)

    [pscustomobject]@{
        Name = [string]$Row.Name
        PercentProcessorTime = [double]$Row.PercentProcessorTime
        PercentUserTime = [double]$Row.PercentUserTime
        PercentPrivilegedTime = [double]$Row.PercentPrivilegedTime
        PercentInterruptTime = [double]$Row.PercentInterruptTime
        PercentDPCTime = [double]$Row.PercentDPCTime
    }
}

function New-PhysicalDiskSample {
    <# 保留物理盘读写、队列及可用延迟；格式化 CIM 的伪零延迟显式标记为不可用。 #>
    param([Parameter(Mandatory)] $Row)

    # PhysicalDisk 性能实例通常以“盘号 盘符”开头；不能解析时保留空值供报告显式标注。
    $instanceName = [string]$Row.Name
    $diskNumber = $null
    if ($instanceName -match '^\s*(\d+)(?:\s|$)') {
        $diskNumber = [int]$Matches[1]
    }
    $readLatency = [double]$Row.AvgDisksecPerRead
    $writeLatency = [double]$Row.AvgDisksecPerWrite
    $readLatencyAvailable = $readLatency -gt 0
    $writeLatencyAvailable = $writeLatency -gt 0
    [pscustomobject]@{
        Name = $instanceName
        DiskNumber = $diskNumber
        DiskReadBytesPerSec = [double]$Row.DiskReadBytesPersec
        DiskWriteBytesPerSec = [double]$Row.DiskWriteBytesPersec
        DiskReadsPerSec = [double]$Row.DiskReadsPersec
        DiskWritesPerSec = [double]$Row.DiskWritesPersec
        AvgDiskQueueLength = [double]$Row.AvgDiskQueueLength
        AvgDiskReadQueueLength = [double]$Row.AvgDiskReadQueueLength
        AvgDiskWriteQueueLength = [double]$Row.AvgDiskWriteQueueLength
        CurrentDiskQueueLength = [double]$Row.CurrentDiskQueueLength
        PercentDiskTime = [double]$Row.PercentDiskTime
        ReadLatencyAvailable = $readLatencyAvailable
        WriteLatencyAvailable = $writeLatencyAvailable
        AvgDiskSecPerRead = if ($readLatencyAvailable) { $readLatency } else { $null }
        AvgDiskSecPerWrite = if ($writeLatencyAvailable) { $writeLatency } else { $null }
        SplitIoPerSec = [double]$Row.SplitIOPerSec
    }
}

function New-SystemSampleRecord {
    <# 组装可序列化的系统样本，并记录单调时钟得到的真实采样间隔与采集开销。 #>
    param(
        [Parameter(Mandatory)] [DateTime] $Utc,
        [Parameter(Mandatory)] [double] $ElapsedMilliseconds,
        [Parameter(Mandatory)] [double] $PreviousSampleElapsedMilliseconds,
        [Parameter(Mandatory)] [double] $CollectionDurationMs,
        [Parameter(Mandatory)] [int] $LogicalProcessorCount,
        [Parameter(Mandatory)] [object[]] $Processes,
        [object[]] $ProcessSampleSkips = @(),
        [Parameter(Mandatory)] [object[]] $CpuCores,
        [Parameter(Mandatory)] [object[]] $Disks
    )

    # 首个样本没有前一 tick，间隔明确记 0；后续样本保留毫秒精度，避免把采集开销误算成固定 2 秒。
    $sampleIntervalMs = if ($PreviousSampleElapsedMilliseconds -ge 0) {
        [Math]::Max(0, $ElapsedMilliseconds - $PreviousSampleElapsedMilliseconds)
    }
    else {
        0
    }
    [pscustomobject]@{
        record_type = 'system_sample'
        utc = $Utc.ToUniversalTime().ToString('O')
        utc_unix_ms = ([DateTimeOffset]$Utc.ToUniversalTime()).ToUnixTimeMilliseconds()
        elapsed_seconds = [int][Math]::Floor($ElapsedMilliseconds / 1000)
        sample_interval_ms = [Math]::Round($sampleIntervalMs, 3)
        collection_duration_ms = [Math]::Round([Math]::Max(0, $CollectionDurationMs), 3)
        logical_processor_count = $LogicalProcessorCount
        processes = @($Processes)
        process_sample_skips = @($ProcessSampleSkips)
        cpu_cores = @($CpuCores)
        disks = @($Disks)
    }
}

function Write-SystemSample {
    <# 采集隔离进程、逐逻辑核与逐物理盘指标，并追加一条可按 PID/时间关联的 NDJSON。 #>
    param(
        [string] $Path,
        [string] $Root,
        [double] $ElapsedMilliseconds,
        [double] $PreviousSampleElapsedMilliseconds,
        [hashtable] $PreviousCpu,
        [hashtable] $PreviousIo,
        [object[]] $IsolatedProcessRows,
        [scriptblock] $ProcessResolver,
        [object[]] $ProcessorRows,
        [object[]] $DiskRows
    )

    # 可注入的计数器行仅用于受控行为测试；真实运行默认读取 Windows CIM 性能类。
    $sampleUtc = [DateTime]::UtcNow
    $collectionWatch = [Diagnostics.Stopwatch]::StartNew()
    $effectiveProcessRows = if ($PSBoundParameters.ContainsKey('IsolatedProcessRows')) {
        @($IsolatedProcessRows)
    }
    else {
        @(Get-IsolatedProcesses -Root $Root)
    }
    $effectiveProcessResolver = if ($ProcessResolver) {
        $ProcessResolver
    }
    else {
        { param($processId) Get-Process -Id $processId -ErrorAction SilentlyContinue }
    }
    $sampleIntervalMs = if ($PreviousSampleElapsedMilliseconds -ge 0) {
        [Math]::Max(0, $ElapsedMilliseconds - $PreviousSampleElapsedMilliseconds)
    }
    else {
        0
    }
    # 健康进程样本与退出跳过记录分开收集，保证单个 PID 不影响其余系统指标。
    $processes = [Collections.Generic.List[object]]::new()
    $processSampleSkips = [Collections.Generic.List[object]]::new()
    foreach ($row in $effectiveProcessRows) {
        $process = & $effectiveProcessResolver ([int]$row.ProcessId)
        if (-not $process) { continue }
        $result = Try-NewIsolatedProcessSample -Row $row -Process $process `
            -PreviousCpu $PreviousCpu -PreviousIo $PreviousIo `
            -SampleIntervalMs $sampleIntervalMs
        if ($null -ne $result.Sample) {
            [void]$processes.Add($result.Sample)
        }
        if ($null -ne $result.Skip) {
            [void]$processSampleSkips.Add($result.Skip)
        }
    }
    # 转为固定数组后供基线清理和 NDJSON 序列化复用。
    $processSamples = @($processes.ToArray())
    $processSkips = @($processSampleSkips.ToArray())
    # 只保留当前仍存在的进程世代，避免长跑中已退出 PID 的累计基线无限残留。
    $activeProcessGenerations = [Collections.Generic.HashSet[string]]::new(
        [StringComparer]::OrdinalIgnoreCase)
    foreach ($processSample in $processSamples) {
        [void]$activeProcessGenerations.Add([string]$processSample.ProcessGenerationKey)
    }
    foreach ($key in @($PreviousCpu.Keys)) {
        if (-not $activeProcessGenerations.Contains([string]$key)) {
            $PreviousCpu.Remove($key)
        }
    }
    foreach ($key in @($PreviousIo.Keys)) {
        if (-not $activeProcessGenerations.Contains([string]$key)) {
            $PreviousIo.Remove($key)
        }
    }
    $effectiveProcessorRows = if ($PSBoundParameters.ContainsKey('ProcessorRows')) {
        @($ProcessorRows)
    }
    else {
        @(
            Get-CimInstance Win32_PerfFormattedData_PerfOS_Processor -ErrorAction SilentlyContinue |
                Where-Object Name -ne '_Total'
        )
    }
    $cpuCores = @(
        foreach ($row in $effectiveProcessorRows) {
            New-LogicalProcessorSample -Row $row
        }
    )
    $effectiveDiskRows = if ($PSBoundParameters.ContainsKey('DiskRows')) {
        @($DiskRows)
    }
    else {
        @(
            Get-CimInstance Win32_PerfFormattedData_PerfDisk_PhysicalDisk -ErrorAction SilentlyContinue |
                Where-Object Name -ne '_Total'
        )
    }
    $disks = @(
        foreach ($row in $effectiveDiskRows) {
            New-PhysicalDiskSample -Row $row
        }
    )
    $collectionWatch.Stop()
    $sample = New-SystemSampleRecord -Utc $sampleUtc `
        -ElapsedMilliseconds $ElapsedMilliseconds `
        -PreviousSampleElapsedMilliseconds $PreviousSampleElapsedMilliseconds `
        -CollectionDurationMs $collectionWatch.Elapsed.TotalMilliseconds `
        -LogicalProcessorCount ([Environment]::ProcessorCount) `
        -Processes $processSamples -ProcessSampleSkips $processSkips `
        -CpuCores $cpuCores -Disks $disks
    Add-Content -LiteralPath $Path -Value ($sample | ConvertTo-Json -Depth 8 -Compress) -Encoding utf8
}

function Stop-IsolatedProcesses {
    <# 只终止本次 staging 绝对路径下的 Node、Worker 和客户端；失败不被吞掉。 #>
    param(
        [string] $Root,
        [scriptblock] $ProcessEnumerator,
        [scriptblock] $ProcessTerminator
    )

    try {
        $rows = @(Get-IsolatedProcesses -Root $Root -ProcessEnumerator $ProcessEnumerator) |
            Sort-Object -Property ProcessId -Descending
        foreach ($row in $rows) {
            if ($ProcessTerminator) {
                & $ProcessTerminator ([int]$row.ProcessId)
            }
            else {
                Stop-Process -Id ([int]$row.ProcessId) -Force -ErrorAction Stop
            }
        }
    }
    catch {
        throw 'RUST_V2_ACCEPTANCE_PROCESS_STOP_FAILED'
    }
}

function Resolve-ReleaseRoot {
    <# 解析正式发布根；自动组装时也只包含正式四项 EXE 和 FFmpeg DLL。 #>
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
    foreach ($name in @('desktop.exe', 'node.exe', 'worker.exe')) {
        Copy-Item -LiteralPath (Join-Path $binaryRoot $name) -Destination (Join-Path $assembled $name)
    }
    Copy-Item -LiteralPath (Join-Path $script:RepositoryRoot 'third_party\everything\Everything.exe') `
        -Destination (Join-Path $assembled 'Everything.exe')
    $assembledFfmpeg = Join-Path $assembled 'runtime\ffmpeg'
    New-Item -ItemType Directory -Path $assembledFfmpeg -Force | Out-Null
    foreach ($name in $script:FfmpegFiles) {
        Copy-Item -LiteralPath (Join-Path $stagedRuntime "runtime\ffmpeg\$name") `
            -Destination (Join-Path $assembledFfmpeg $name)
    }
    $assembled
}

function Parse-ResultSummaryOutput {
    <# 解析 exporter 固定 stdout，并验证 TSV 路径、状态、SHA 与计数的互相绑定。 #>
    param(
        [Parameter(Mandatory)] [string] $Text,
        [Parameter(Mandatory)] [string] $ExpectedPath,
        [string] $ExpectedTaskId = ''
    )

    $values = [ordered]@{}
    foreach ($line in ($Text -split "`r?`n")) {
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        $separator = $line.IndexOf('=')
        if ($separator -le 0) { throw "RUST_V2_RESULT_SUMMARY_STDOUT_INVALID line=$line" }
        $name = $line.Substring(0, $separator)
        $value = $line.Substring($separator + 1)
        if (-not $name.StartsWith('RESULT_SUMMARY_', [StringComparison]::Ordinal) -or
            $values.Contains($name)) {
            throw "RUST_V2_RESULT_SUMMARY_STDOUT_INVALID field=$name"
        }
        $values[$name] = $value
    }
    $required = @(
        'RESULT_SUMMARY_STATUS', 'RESULT_SUMMARY_PATH', 'RESULT_SUMMARY_SHA256',
        'RESULT_SUMMARY_ROW_COUNT', 'RESULT_SUMMARY_MISSING_COUNT',
        'RESULT_SUMMARY_INCONCLUSIVE_COUNT'
    )
    foreach ($name in $required) {
        if (-not $values.Contains($name) -or [string]::IsNullOrWhiteSpace([string]$values[$name])) {
            throw "RUST_V2_RESULT_SUMMARY_FIELD_MISSING field=$name"
        }
    }
    $status = [string]$values['RESULT_SUMMARY_STATUS']
    if ($status -notin @('PASS', 'MISSING', 'INCONCLUSIVE')) {
        throw "RUST_V2_RESULT_SUMMARY_STATUS_INVALID value=$status"
    }
    $summaryPath = Get-NormalizedAbsolutePath -Path ([string]$values['RESULT_SUMMARY_PATH'])
    $expectedPath = Get-NormalizedAbsolutePath -Path $ExpectedPath
    if (-not $summaryPath -or $summaryPath -cne $expectedPath) {
        throw 'RUST_V2_RESULT_SUMMARY_PATH_MISMATCH'
    }
    $sha256 = [string]$values['RESULT_SUMMARY_SHA256']
    if ($sha256 -notmatch '^[0-9a-f]{64}$') {
        throw 'RUST_V2_RESULT_SUMMARY_SHA_INVALID'
    }
    $rowCount = 0L
    $missingCount = 0L
    $inconclusiveCount = 0L
    if (-not [long]::TryParse([string]$values['RESULT_SUMMARY_ROW_COUNT'], [Globalization.NumberStyles]::Integer, [Globalization.CultureInfo]::InvariantCulture, [ref]$rowCount) -or $rowCount -lt 0 -or
        -not [long]::TryParse([string]$values['RESULT_SUMMARY_MISSING_COUNT'], [Globalization.NumberStyles]::Integer, [Globalization.CultureInfo]::InvariantCulture, [ref]$missingCount) -or $missingCount -lt 0 -or
        -not [long]::TryParse([string]$values['RESULT_SUMMARY_INCONCLUSIVE_COUNT'], [Globalization.NumberStyles]::Integer, [Globalization.CultureInfo]::InvariantCulture, [ref]$inconclusiveCount) -or $inconclusiveCount -lt 0) {
        throw 'RUST_V2_RESULT_SUMMARY_COUNT_INVALID'
    }
    $taskId = if ($values.Contains('RESULT_SUMMARY_TASK_ID')) {
        [string]$values['RESULT_SUMMARY_TASK_ID']
    }
    else {
        ''
    }
    if (-not [string]::IsNullOrWhiteSpace($ExpectedTaskId) -and $taskId -cne $ExpectedTaskId) {
        throw 'RUST_V2_RESULT_SUMMARY_TASK_ID_MISMATCH'
    }
    [pscustomobject]@{
        Status = $status
        Path = $summaryPath
        Sha256 = $sha256
        RowCount = $rowCount
        MissingCount = $missingCount
        InconclusiveCount = $inconclusiveCount
        TaskId = $taskId
    }
}

function Assert-CanonicalResultSummary {
    <# 校验 canonical JSONL 的 UTF-8 无 BOM、单字节 LF、合法 JSON 与行数。 #>
    param(
        [Parameter(Mandatory)] [string] $Path,
        [long] $ExpectedRowCount = -1
    )

    $bytes = [IO.File]::ReadAllBytes($Path)
    if ($bytes.Length -eq 0) {
        if ($ExpectedRowCount -ge 0 -and $ExpectedRowCount -ne 0) {
            throw 'RUST_V2_ACCEPTANCE_RESULT_SUMMARY_ROW_COUNT_INVALID'
        }
        return 0
    }
    if ($bytes[$bytes.Length - 1] -ne [byte]10 -or
        [Array]::IndexOf($bytes, [byte]13) -ge 0 -or
        ($bytes.Length -ge 3 -and $bytes[0] -eq [byte]239 -and $bytes[1] -eq [byte]187 -and $bytes[2] -eq [byte]191)) {
        throw 'RUST_V2_ACCEPTANCE_RESULT_SUMMARY_CANONICAL_FORMAT_INVALID'
    }
    $text = [Text.UTF8Encoding]::new($false, $true).GetString($bytes)
    $rows = @(
        $text -split [char]10 |
            Where-Object { -not [string]::IsNullOrWhiteSpace($_) } |
            ForEach-Object { $_ | ConvertFrom-Json }
    )
    if ($rows.Count -eq 0 -or ($ExpectedRowCount -ge 0 -and $rows.Count -ne $ExpectedRowCount)) {
        throw 'RUST_V2_ACCEPTANCE_RESULT_SUMMARY_ROW_COUNT_INVALID'
    }
    $rows.Count
}

function Assert-ResultSummaryTsv {
    <# 流程级校验固定 TSV 的 UTF-8/LF、列数、footer、行数和数据区 SHA。 #>
    param(
        [Parameter(Mandatory)] [string] $Path,
        [long] $ExpectedRowCount = -1
    )

    $bytes = [IO.File]::ReadAllBytes($Path)
    if ($bytes.Length -eq 0 -or $bytes[$bytes.Length - 1] -ne [byte]10 -or
        [Array]::IndexOf($bytes, [byte]13) -ge 0 -or
        ($bytes.Length -ge 3 -and $bytes[0] -eq [byte]239 -and
            $bytes[1] -eq [byte]187 -and $bytes[2] -eq [byte]191)) {
        throw 'RUST_V2_ACCEPTANCE_RESULT_SUMMARY_TSV_FORMAT_INVALID'
    }
    try {
        $text = [Text.UTF8Encoding]::new($false, $true).GetString($bytes)
    }
    catch {
        throw 'RUST_V2_ACCEPTANCE_RESULT_SUMMARY_TSV_UTF8_INVALID'
    }
    $lines = @($text -split "`n")
    if ($lines.Count -lt 3) {
        throw 'RUST_V2_ACCEPTANCE_RESULT_SUMMARY_TSV_FOOTER_MISSING'
    }
    $expectedColumns = @(
        'record_type', 'status', 'machine_id', 'normalized_path', 'display_path',
        'file_size', 'md5', 'media_type', 'base_complete', 'feature_payload_sha256',
        'image_stage1_sha256', 'image_stage2_sha256', 'video_metadata_sha256',
        'video_frame_stage1_0_sha256', 'video_frame_stage1_1_sha256',
        'video_frame_stage1_2_sha256', 'video_frame_stage1_3_sha256',
        'video_frame_stage1_4_sha256', 'video_frame_stage1_5_sha256',
        'video_frame_stage2_0_sha256', 'video_frame_stage2_1_sha256',
        'video_frame_stage2_2_sha256', 'video_frame_stage2_3_sha256',
        'video_frame_stage2_4_sha256', 'video_frame_stage2_5_sha256',
        'thumbnail_sha256', 'thumbnail_state', 'contact_sheet_sha256', 'status_reason'
    ) -join "`t"
    if ($lines[0] -cne $expectedColumns) {
        throw 'RUST_V2_ACCEPTANCE_RESULT_SUMMARY_TSV_HEADER_INVALID'
    }
    $footerIndex = $lines.Count - 2
    $footer = @($lines[$footerIndex].Split([char]9))
    if ($footer.Count -ne 3 -or $footer[0] -cne 'F' -or
        $footer[1] -notmatch '^[0-9]+$' -or $footer[2] -notmatch '^[0-9a-f]{64}$') {
        throw 'RUST_V2_ACCEPTANCE_RESULT_SUMMARY_TSV_FOOTER_INVALID'
    }
    $rowCount = [long]$footer[1]
    if ($ExpectedRowCount -ge 0 -and $rowCount -ne $ExpectedRowCount) {
        throw 'RUST_V2_ACCEPTANCE_RESULT_SUMMARY_ROW_COUNT_INVALID'
    }
    $dataText = $lines[0] + "`n"
    $missingCount = 0L
    $inconclusiveCount = 0L
    $actualRows = 0L
    for ($lineIndex = 1; $lineIndex -lt $footerIndex; $lineIndex++) {
        $line = $lines[$lineIndex]
        $columns = @($line.Split([char]9))
        if ($columns.Count -ne 29 -or $columns[0] -cne 'R' -or
            [string]::IsNullOrWhiteSpace($columns[1])) {
            throw "RUST_V2_ACCEPTANCE_RESULT_SUMMARY_TSV_ROW_INVALID line=$($lineIndex + 1)"
        }
        if ($columns[1] -ceq 'MISSING') { $missingCount++ }
        if ($columns[1] -ceq 'INCONCLUSIVE') { $inconclusiveCount++ }
        $dataText += $line + "`n"
        $actualRows++
    }
    if ($actualRows -ne $rowCount) {
        throw 'RUST_V2_ACCEPTANCE_RESULT_SUMMARY_TSV_ROW_COUNT_INVALID'
    }
    $actualDataHash = Get-TextSha256 -Text $dataText
    if ($actualDataHash -cne [string]$footer[2]) {
        throw 'RUST_V2_ACCEPTANCE_RESULT_SUMMARY_TSV_DATA_HASH_INVALID'
    }
    [pscustomobject]@{
        RowCount = $rowCount
        MissingCount = $missingCount
        InconclusiveCount = $inconclusiveCount
        DataSha256 = [string]$footer[2]
    }
}

function Get-ResultSummaryArtifacts {
    <# 校验单个固定 TSV；不读取、不创建 JSON metadata、pair lock 或其他旁车文件。 #>
    param(
        [Parameter(Mandatory)] [string] $SummaryPath,
        [string] $ExpectedTaskId = '',
        [string] $ExpectedStatus = '',
        [string] $ExpectedSha256 = '',
        [long] $ExpectedRowCount = -1
    )

    $summary = Get-NormalizedAbsolutePath -Path $SummaryPath
    $summaryExists = Test-Path -LiteralPath $summary -PathType Leaf
    $bindingValid = $false
    $diagnostic = ''
    $tsv = $null
    $actualSha256 = $null
    if (-not $summaryExists) {
        $diagnostic = 'RUST_V2_ACCEPTANCE_RESULT_SUMMARY_MISSING'
    }
    else {
        try {
            if ([IO.Path]::GetFileName($summary) -cne 'result-summary.tsv') {
                throw 'RUST_V2_ACCEPTANCE_RESULT_SUMMARY_FILENAME_INVALID'
            }
            $tsv = Assert-ResultSummaryTsv -Path $summary -ExpectedRowCount $ExpectedRowCount
            $actualSha256 = Get-FileSha256OrNull -Path $summary
            $summaryStatus = if ($tsv.InconclusiveCount -gt 0) {
                'INCONCLUSIVE'
            }
            elseif ($tsv.MissingCount -gt 0 -or $tsv.RowCount -eq 0) {
                'MISSING'
            }
            else {
                'PASS'
            }
            $bindingValid = $actualSha256 -and
                ([string]::IsNullOrWhiteSpace($ExpectedSha256) -or $actualSha256 -ceq $ExpectedSha256) -and
                ([string]::IsNullOrWhiteSpace($ExpectedTaskId)) -and
                ([string]::IsNullOrWhiteSpace($ExpectedStatus) -or
                    $ExpectedStatus.Equals($summaryStatus, [StringComparison]::OrdinalIgnoreCase))
            if (-not $bindingValid) {
                $diagnostic = 'RUST_V2_ACCEPTANCE_RESULT_SUMMARY_BINDING_INVALID'
            }
        }
        catch {
            $diagnostic = $_.Exception.Message
        }
    }
    [pscustomobject]@{
        SummaryPath = $summary
        MetadataPath = $null
        LeasePath = $null
        SummaryExists = $summaryExists
        MetadataExists = $false
        LeaseExists = $false
        BindingValid = $bindingValid
        Diagnostic = $diagnostic
        Metadata = $null
        Lease = $null
        RowCount = if ($tsv) { $tsv.RowCount } else { 0 }
        MissingCount = if ($tsv) { $tsv.MissingCount } else { 0 }
        InconclusiveCount = if ($tsv) { $tsv.InconclusiveCount } else { 0 }
    }
}

function Get-ResultSummaryRunStatus {
    <# 纯分类摘要结果：基础设施证据不完整为 INCONCLUSIVE，摘要业务状态失败为 FAIL。 #>
    param(
        [bool] $RuntimeEvidenceComplete = $true,
        [bool] $NodeStartupFailed = $false,
        [bool] $NodeUnexpectedExit = $false,
        [bool] $ClientExitFailed = $false,
        [bool] $MediaUnchanged = $true,
        [bool] $ScanFailed = $false,
        [bool] $ClientExitUnconfirmed = $false,
        [bool] $RuntimeTaskTerminalObserved = $false,
        [bool] $CompletedTaskIdPresent = $false,
        [bool] $ExporterSucceeded = $false,
        [bool] $SummaryBindingValid = $false,
        [string] $SummaryStatus = '',
        [string] $RunDiagnostic = ''
    )

    if (-not $RuntimeEvidenceComplete -or $NodeStartupFailed) {
        return 'INCONCLUSIVE'
    }
    # 监督器超时、身份失配、状态缺失或退出未确认都是基础设施不完整，优先于客户端非零/Node 退出判 INCONCLUSIVE。
    if ($RunDiagnostic -match 'RUST_V2_ACCEPTANCE_(SUPERVISOR_|CLIENT_EXIT_UNCONFIRMED)') {
        return 'INCONCLUSIVE'
    }
    $terminalObserved = $RuntimeTaskTerminalObserved -or $CompletedTaskIdPresent
    if ($NodeUnexpectedExit -or $ClientExitFailed -or -not $MediaUnchanged -or $ScanFailed) {
        return 'FAIL'
    }
    if ($ClientExitUnconfirmed -or -not $terminalObserved -or -not $ExporterSucceeded -or
        -not $SummaryBindingValid -or -not [string]::IsNullOrWhiteSpace($RunDiagnostic)) {
        return 'INCONCLUSIVE'
    }
    if ($SummaryStatus -in @('MISSING', 'INCONCLUSIVE')) {
        return 'FAIL'
    }
    if ($SummaryStatus -eq 'PASS') {
        return 'PASS'
    }
    'INCONCLUSIVE'
}

function New-HarnessResult {
    <# 构造有序 schema2 harness-result；所有路径规范化，缺失 manifest 保持 null。 #>
    param(
        [string] $Variant,
        [int] $RunIndex,
        [string] $SourceRevision,
        [string] $SourceTreeSha256,
        [string] $PackagePath,
        [string] $PackageSha256,
        [string] $ReleaseRoot,
        [string] $DatabaseSnapshotRoot,
        [string] $DatabaseSnapshotPath,
        [string] $DatabaseSnapshotMetadataPath,
        [string] $ConfigSha256,
        [string] $PackageManifestSha256,
        [string] $MediaBeforeSha256,
        [string] $MediaAfterSha256,
        [string[]] $MediaRoots = @(),
        [switch] $SingleRun,
        [string] $PhysicalDiskMapPath = '',
        [string] $PhysicalDiskMapSha256 = '',
        [string[]] $MediaBeforeRootPaths = @(),
        [string[]] $MediaAfterRootPaths = @(),
        [string[]] $MediaBeforeRootSha256 = @(),
        [string[]] $MediaAfterRootSha256 = @(),
        [Parameter(Mandatory)] $ResultSummary,
        [string] $ResultSummaryStatus,
        [string] $ResultSummaryPath,
        [string] $ResultSummarySha256,
        [string] $ResultSummaryTaskId,
        [long] $ResultSummaryMissingCount = 0,
        [long] $ResultSummaryInconclusiveCount = 0,
        [long] $ResultSummaryRowCount = 0,
        [string] $RunStatus = 'INCONCLUSIVE',
        [string] $RunDiagnostic = '',
        [bool] $MediaUnchanged = $false,
        [bool] $NodeUnexpectedExit = $false,
        [int] $ExporterExitCode = -1,
        [string] $DeadlineCancelledPersistentTaskId = '',
        [int] $EffectiveWorkerCount = 0,
        [int] $HddThreadsPerDisk = 0,
        [int] $SsdThreadsPerDisk = 0,
        [int] $UnknownThreadsPerDisk = 0,
        [int] $ReadTotalThreads = 0,
        [int] $ReservedCores = 0,
        [int] $ContactSheetReuseCount = 0,
        [int] $DiskFullCleanupCount = 0
    )

    $summaryPathValue = if ($ResultSummaryPath) { Get-NormalizedAbsolutePath -Path $ResultSummaryPath } else { $ResultSummary.Path }
    $summaryShaValue = if ($ResultSummarySha256) { $ResultSummarySha256 } else { $ResultSummary.Sha256 }
    $summaryStatusValue = if ($ResultSummaryStatus) { $ResultSummaryStatus } else { $ResultSummary.Status }
    $summaryTaskValue = if ($ResultSummaryTaskId) { $ResultSummaryTaskId } else { $ResultSummary.TaskId }
    $summaryRowValue = if ($ResultSummary.PSObject.Properties['RowCount']) { $ResultSummary.RowCount } else { $ResultSummaryRowCount }
    $summaryMissingValue = if ($ResultSummary.PSObject.Properties['MissingCount']) { $ResultSummary.MissingCount } else { $ResultSummaryMissingCount }
    $summaryInconclusiveValue = if ($ResultSummary.PSObject.Properties['InconclusiveCount']) { $ResultSummary.InconclusiveCount } else { $ResultSummaryInconclusiveCount }
    [ordered]@{
        schema_version = 2
        variant = $Variant
        run_index = $RunIndex
        source_revision = $SourceRevision
        source_tree_sha256 = $SourceTreeSha256
        package_path = if ($PackagePath) { Get-NormalizedAbsolutePath -Path $PackagePath } else { $null }
        package_sha256 = $PackageSha256
        release_root = if ($ReleaseRoot) { Get-NormalizedAbsolutePath -Path $ReleaseRoot } else { $null }
        database_snapshot_root = if ($DatabaseSnapshotRoot) { Get-NormalizedAbsolutePath -Path $DatabaseSnapshotRoot } else { $null }
        database_snapshot_path = if ($DatabaseSnapshotPath) { Get-NormalizedAbsolutePath -Path $DatabaseSnapshotPath } else { $null }
        database_snapshot_metadata_path = if ($DatabaseSnapshotMetadataPath) { Get-NormalizedAbsolutePath -Path $DatabaseSnapshotMetadataPath } else { $null }
        database_snapshot_status = if ($DatabaseSnapshotMetadataPath) { 'PRESENT' } else { 'MISSING' }
        config_sha256 = $ConfigSha256
        package_manifest_sha256 = if ([string]::IsNullOrWhiteSpace($PackageManifestSha256)) { $null } else { $PackageManifestSha256 }
        package_manifest_status = if ([string]::IsNullOrWhiteSpace($PackageManifestSha256)) { 'MISSING' } else { 'PRESENT' }
        media_before_sha256 = $MediaBeforeSha256
        media_after_sha256 = $MediaAfterSha256
        media_roots = @($MediaRoots)
        single_run = [bool]$SingleRun
        physical_disk_map_path = if ($PhysicalDiskMapPath) { Get-NormalizedAbsolutePath -Path $PhysicalDiskMapPath } else { $null }
        physical_disk_map_sha256 = if ($PhysicalDiskMapSha256) { $PhysicalDiskMapSha256 } else { $null }
        media_before_root_paths = @($MediaBeforeRootPaths | ForEach-Object { Get-NormalizedAbsolutePath -Path $_ })
        media_after_root_paths = @($MediaAfterRootPaths | ForEach-Object { Get-NormalizedAbsolutePath -Path $_ })
        media_before_root_sha256 = @($MediaBeforeRootSha256)
        media_after_root_sha256 = @($MediaAfterRootSha256)
        result_summary_path = $summaryPathValue
        result_summary_sha256 = $summaryShaValue
        result_summary_status = $summaryStatusValue
        result_summary_task_id = if ([string]::IsNullOrWhiteSpace($summaryTaskValue)) { $null } else { $summaryTaskValue }
        result_summary_row_count = $summaryRowValue
        result_summary_missing_count = $summaryMissingValue
        result_summary_inconclusive_count = $summaryInconclusiveValue
        run_status = $RunStatus
        run_diagnostic = if ([string]::IsNullOrWhiteSpace($RunDiagnostic)) { $null } else { $RunDiagnostic }
        media_unchanged = $MediaUnchanged
        node_unexpected_exit = $NodeUnexpectedExit
        exporter_exit_code = $ExporterExitCode
        deadline_cancelled_persistent_task_id = if ($DeadlineCancelledPersistentTaskId) { $DeadlineCancelledPersistentTaskId } else { $null }
        effective_worker_count = $EffectiveWorkerCount
        hdd_threads_per_disk = $HddThreadsPerDisk
        ssd_threads_per_disk = $SsdThreadsPerDisk
        unknown_threads_per_disk = $UnknownThreadsPerDisk
        read_total_threads = $ReadTotalThreads
        reserved_cores = $ReservedCores
        contact_sheet_reuse_count = $ContactSheetReuseCount
        disk_full_cleanup_count = $DiskFullCleanupCount
    }
}

function Read-RuntimeEvidenceRecords {
    <# 读取已落盘 NDJSON；坏行保持异常，调用方会将原始证据标成 INCONCLUSIVE。 #>
    param([Parameter(Mandatory)] [string] $Path)

    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        return @()
    }
    $lineNumber = 0
    $records = [Collections.Generic.List[object]]::new()
    # 一次性读入并立即释放文件句柄；坏行抛错时 finally 仍能清理 fixture/evidence。
    foreach ($line in [IO.File]::ReadAllLines($Path)) {
        $lineNumber++
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        try {
            $records.Add(($line | ConvertFrom-Json))
        }
        catch {
            throw "RUST_V2_ACCEPTANCE_RUNTIME_NDJSON_INVALID line=$lineNumber"
        }
    }
    @($records)
}

function Get-LastRuntimeResult {
    <# 从最后一条 runtime_result 读取 completed 与 deadline-cancelled 两个独立身份。 #>
    param([Parameter(Mandatory)] [string] $Path)

    @(
        Read-RuntimeEvidenceRecords -Path $Path |
            Where-Object { $_.record_type -eq 'runtime_result' }
    ) | Select-Object -Last 1
}

function Request-IsolatedNodeExit {
    <# 在所有证据写完后请求 Node 退出；无主窗口时立即受控终止并确认 Node/Worker 已清理。 #>
    param(
        [Parameter(Mandatory)] $Node,
        [Parameter(Mandatory)] [string] $Root,
        [int] $TimeoutSeconds = 20
    )

    $diagnostic = ''
    $needsForcedStop = $false
    # 仅记录优雅等待的正常超时；最终确认隔离进程归零后可清空该诊断，异常诊断不可覆盖。
    $gracefulWaitTimedOut = $false
    try {
        if ($null -ne $Node -and -not $Node.HasExited) {
            # headless Node 没有主窗口；CloseMainWindow=false 不是异常，应直接进入受控清理路径。
            $closeRequested = [bool]$Node.CloseMainWindow()
            if (-not $closeRequested) {
                $needsForcedStop = $true
            }
            elseif (-not $Node.WaitForExit($TimeoutSeconds * 1000)) {
                $diagnostic = 'RUST_V2_ACCEPTANCE_NODE_EXIT_TIMEOUT'
                $gracefulWaitTimedOut = $true
                $needsForcedStop = $true
            }
        }
    }
    catch {
        $diagnostic = 'RUST_V2_ACCEPTANCE_NODE_EXIT_REQUEST_FAILED'
        $needsForcedStop = $true
    }
    if ($needsForcedStop) {
        try {
            Stop-IsolatedProcesses -Root $Root
        }
        catch {
            return 'RUST_V2_ACCEPTANCE_PROCESS_STOP_FAILED'
        }
    }
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        try {
            $remaining = @(Get-IsolatedProcesses -Root $Root)
        }
        catch {
            return 'RUST_V2_ACCEPTANCE_PROCESS_ENUMERATION_FAILED'
        }
        if ($remaining.Count -eq 0) { break }
        Start-Sleep -Milliseconds 100
    }
    try {
        $remaining = @(Get-IsolatedProcesses -Root $Root)
    }
    catch {
        return 'RUST_V2_ACCEPTANCE_PROCESS_ENUMERATION_FAILED'
    }
    if ($remaining.Count -gt 0) {
        try {
            Stop-IsolatedProcesses -Root $Root
        }
        catch {
            return 'RUST_V2_ACCEPTANCE_PROCESS_STOP_FAILED'
        }
        if (-not $diagnostic) { $diagnostic = 'RUST_V2_ACCEPTANCE_WORKER_EXIT_TIMEOUT' }
    }
    elseif ($gracefulWaitTimedOut) {
        # CloseMainWindow/WaitForExit 的受控超时已由隔离进程归零证明清理成功，允许继续 exporter。
        $diagnostic = ''
    }
    $diagnostic
}

function Invoke-ResultExporter {
    <# Node/Worker 全停后运行外置 exporter；重复传递媒体根，只生成固定 TSV。 #>
    param(
        [Parameter(Mandatory)] [string] $ExporterPath,
        [Parameter(Mandatory)] [string] $DatabasePath,
        [Parameter(Mandatory)] [string] $CacheRoot,
        [string[]] $MediaRoots = @(),
        [Parameter(Mandatory)] [string] $OutputPath,
        [Parameter(Mandatory)] [string] $EvidenceRoot,
        [int] $TimeoutSeconds = 120,
        [scriptblock] $ProcessKiller,
        [scriptblock] $ProcessWaiter
    )

    $stdoutPath = Join-Path $EvidenceRoot 'result-summary.stdout.log'
    $stderrPath = Join-Path $EvidenceRoot 'result-summary.stderr.log'
    $stdout = ''
    $stderr = ''
    $exitCode = -1
    $timedOut = $false
    $diagnostic = ''
    $killFailed = $false
    $exitConfirmed = $false
    $process = $null
    $stdoutTask = $null
    $stderrTask = $null
    try {
        if (@($MediaRoots).Count -eq 0) {
            throw 'RUST_V2_ACCEPTANCE_EXPORTER_MEDIA_ROOT_MISSING'
        }
        if ([IO.Path]::GetFileName($OutputPath) -cne 'result-summary.tsv') {
            throw 'RUST_V2_ACCEPTANCE_RESULT_SUMMARY_FILENAME_INVALID'
        }
        # ArgumentList 逐项传值，Windows 路径中的空格不会被拆成多个参数。
        $startInfo = [Diagnostics.ProcessStartInfo]::new()
        $startInfo.FileName = $ExporterPath
        $startInfo.WorkingDirectory = $EvidenceRoot
        $startInfo.UseShellExecute = $false
        $startInfo.CreateNoWindow = $true
        $startInfo.RedirectStandardOutput = $true
        $startInfo.RedirectStandardError = $true
        $argumentList = [Collections.Generic.List[string]]::new()
        foreach ($argument in @('--database', $DatabasePath, '--cache-root', $CacheRoot)) {
            [void]$argumentList.Add([string]$argument)
        }
        foreach ($mediaRoot in @($MediaRoots)) {
            [void]$argumentList.Add('--media-root')
            [void]$argumentList.Add([string]$mediaRoot)
        }
        foreach ($argument in @('--output', $OutputPath)) {
            [void]$argumentList.Add([string]$argument)
        }
        foreach ($argument in $argumentList) {
            [void]$startInfo.ArgumentList.Add($argument)
        }
        $process = [Diagnostics.Process]::new()
        $process.StartInfo = $startInfo
        if (-not $process.Start()) {
            $diagnostic = 'RUST_V2_ACCEPTANCE_EXPORTER_START_FAILED'
        }
        else {
            $stdoutTask = $process.StandardOutput.ReadToEndAsync()
            $stderrTask = $process.StandardError.ReadToEndAsync()
            $waitMilliseconds = [Math]::Max(1, $TimeoutSeconds) * 1000
            $initialWait = if ($ProcessWaiter) {
                [bool](& $ProcessWaiter $process $waitMilliseconds)
            }
            else {
                [bool]$process.WaitForExit($waitMilliseconds)
            }
            if (-not $initialWait) {
                $timedOut = $true
                $diagnostic = 'RUST_V2_ACCEPTANCE_EXPORTER_TIMEOUT'
                try {
                    if ($ProcessKiller) { & $ProcessKiller $process } else { $process.Kill($true) }
                }
                catch {
                    $killFailed = $true
                    $diagnostic = 'RUST_V2_ACCEPTANCE_EXPORTER_KILL_FAILED'
                }
                try {
                    $postKillWait = if ($ProcessWaiter) {
                        [bool](& $ProcessWaiter $process 5000)
                    }
                    else {
                        [bool]$process.WaitForExit(5000)
                    }
                    $exitConfirmed = $postKillWait
                }
                catch {
                    $exitConfirmed = $false
                }
                if (-not $exitConfirmed -and -not $killFailed) {
                    $diagnostic = 'RUST_V2_ACCEPTANCE_EXPORTER_EXIT_UNCONFIRMED'
                }
            }
            else {
                [void]$process.WaitForExit()
                $exitConfirmed = $true
            }
            if ($exitConfirmed) {
                $stdoutReady = $stdoutTask.Wait(5000)
                $stderrReady = $stderrTask.Wait(5000)
                if ($stdoutReady) { $stdout = $stdoutTask.GetAwaiter().GetResult() }
                if ($stderrReady) { $stderr = $stderrTask.GetAwaiter().GetResult() }
                if (-not $stdoutReady -or -not $stderrReady) {
                    if (-not $diagnostic) { $diagnostic = 'RUST_V2_ACCEPTANCE_EXPORTER_DRAIN_TIMEOUT' }
                    $exitConfirmed = $false
                }
                elseif (-not $timedOut) {
                    $exitCode = [int]$process.ExitCode
                }
            }
        }
    }
    catch {
        if (-not $diagnostic) { $diagnostic = 'RUST_V2_ACCEPTANCE_EXPORTER_START_FAILED' }
        if ($null -ne $process) {
            try {
                if (-not $process.HasExited) {
                    if ($ProcessKiller) { & $ProcessKiller $process } else { $process.Kill($true) }
                }
            }
            catch {
                $killFailed = $true
                if (-not $diagnostic) { $diagnostic = 'RUST_V2_ACCEPTANCE_EXPORTER_KILL_FAILED' }
            }
            try {
                $exitConfirmed = if ($ProcessWaiter) { [bool](& $ProcessWaiter $process 5000) } else { [bool]$process.WaitForExit(5000) }
            }
            catch { $exitConfirmed = $false }
        }
    }
    finally {
        if ($null -ne $process) { $process.Dispose() }
        [IO.File]::WriteAllText($stdoutPath, $stdout, [Text.UTF8Encoding]::new($false))
        [IO.File]::WriteAllText($stderrPath, $stderr, [Text.UTF8Encoding]::new($false))
    }
    [pscustomobject]@{
        ExitCode = $exitCode
        StdoutPath = $stdoutPath
        StderrPath = $stderrPath
        Stdout = $stdout
        TimedOut = $timedOut
        Diagnostic = $diagnostic
        KillFailed = $killFailed
        ExitConfirmed = $exitConfirmed
    }
}

function Get-CompletedProcessExitCode {
    <# 仅在 HasExited 权威确认后读取退出码；运行中的进程返回 null。 #>
    param(
        [Parameter(Mandatory)] $Process
    )
    try {
        if (-not [bool]$Process.HasExited) { return $null }
        [int]$Process.ExitCode
    }
    catch {
        return $null
    }
}

function Get-MediaEvidenceSha256 {
    <# 对规范化媒体清单计算 SHA-256，前后清单分别绑定到 harness-result。 #>
    param([Parameter(Mandatory)] $Manifest)

    Get-TextSha256 -Text ($Manifest | ConvertTo-Json -Depth 8 -Compress)
}

function Get-RuntimeTaskFileStatistics {
    <# 仅在任务 TSV 尚未被终态清理时统计真实 P/C/F；缺失时明确标记不可用。 #>
    param([Parameter(Mandatory)] [string] $RuntimeRoot)

    $unavailable = [ordered]@{
        status = 'UNAVAILABLE'
        source = 'runtime_protocol_not_exposed'
        pending = $null
        completed = $null
        failed = $null
        row_count = $null
        files = @()
        diagnostic = '任务终态后运行目录已清理，协议未暴露 P/C/F'
    }
    if (-not (Test-Path -LiteralPath $RuntimeRoot -PathType Container)) {
        return $unavailable
    }
    try {
        $taskFiles = @(Get-ChildItem -LiteralPath $RuntimeRoot -Recurse -File -Filter '*.tasks.tsv' -ErrorAction Stop)
    }
    catch {
        $unavailable.status = 'INCONCLUSIVE'
        $unavailable.source = 'runtime_tsv'
        $unavailable.diagnostic = '任务 TSV 枚举失败'
        return $unavailable
    }
    if ($taskFiles.Count -eq 0) {
        return $unavailable
    }
    $pending = 0L
    $completed = 0L
    $failed = 0L
    $rowCount = 0L
    try {
        foreach ($taskFile in $taskFiles) {
            foreach ($line in [IO.File]::ReadLines($taskFile.FullName)) {
                if ([string]::IsNullOrEmpty($line)) { continue }
                $rowCount++
                switch ($line.Substring(0, 1)) {
                    'P' { $pending++ }
                    'C' { $completed++ }
                    'F' { $failed++ }
                    default { throw 'invalid task file state' }
                }
            }
        }
    }
    catch {
        $unavailable.status = 'INCONCLUSIVE'
        $unavailable.source = 'runtime_tsv'
        $unavailable.files = @($taskFiles | ForEach-Object { Get-NormalizedAbsolutePath -Path $_.FullName })
        $unavailable.diagnostic = '任务 TSV 行格式无效'
        return $unavailable
    }
    [ordered]@{
        status = 'PRESENT'
        source = 'runtime_tsv'
        pending = $pending
        completed = $completed
        failed = $failed
        row_count = $rowCount
        files = @($taskFiles | Sort-Object FullName | ForEach-Object { Get-NormalizedAbsolutePath -Path $_.FullName })
        diagnostic = $null
    }
}

function Invoke-RustV2RuntimeAcceptance {
    <# 执行单轮验收；采样、停机、导出和报告严格按固定证据顺序串行完成。 #>
    param(
        [string[]] $MediaRoot,
        [string[]] $MediaRoots = @(),
        [int] $DurationSeconds,
        [int] $SampleSeconds,
        [string] $CargoTargetDir,
        [string] $ReleaseRoot,
        [string] $AcceptanceClientPath,
        [string] $ResultExporterPath,
        [string] $EvidenceRoot,
        [string] $ReportPath,
        [string] $Variant,
        [int] $RunIndex,
        [string] $SourceRevision,
        [string] $SourceTreeSha256,
        [string] $PackagePath,
        [string] $PackageSha256,
        [int] $WorkerCount,
        [int] $HddThreadsPerDisk,
        [int] $SsdThreadsPerDisk,
        [int] $UnknownThreadsPerDisk,
        [int] $TotalReadThreads,
        [int] $ReservedCores,
        [ValidateSet('everything', 'windows_walker')]
        [string] $Enumerator = 'everything',
        [switch] $SingleRun,
        [switch] $CompleteWhenTaskTerminal,
        [switch] $RequireDistinctPhysicalDisks,
        [switch] $RequireApprovedMediaRoots
    )

    $resolvedRelease = Resolve-ReleaseRoot -CargoTargetDir $CargoTargetDir -ReleaseRoot $ReleaseRoot
    $layout = New-RuntimeAcceptanceLayout -RequestedEvidenceRoot $EvidenceRoot -RequestedReportPath $ReportPath
    $resolvedMediaRoots = @(Resolve-RuntimeMediaRoots -MediaRoot $MediaRoot -MediaRoots $MediaRoots)
    if ($RequireApprovedMediaRoots) {
        $null = Assert-RuntimeAcceptanceMediaRoots -MediaRoots $resolvedMediaRoots
    }
    $validation = Assert-RuntimeAcceptanceInputs -MediaRoot $MediaRoot -MediaRoots $MediaRoots -DurationSeconds $DurationSeconds `
        -SampleSeconds $SampleSeconds -ReleaseRoot $resolvedRelease `
        -AcceptanceClientPath $AcceptanceClientPath -ResultExporterPath $ResultExporterPath `
        -EvidenceRoot $layout.Evidence -ReportPath $layout.Report -Variant $Variant -RunIndex $RunIndex `
        -WorkerCount $WorkerCount -HddThreadsPerDisk $HddThreadsPerDisk `
        -SsdThreadsPerDisk $SsdThreadsPerDisk -UnknownThreadsPerDisk $UnknownThreadsPerDisk `
        -TotalReadThreads $TotalReadThreads -ReservedCores $ReservedCores `
        -Enumerator $Enumerator `
        -SingleRun:$SingleRun -CompleteWhenTaskTerminal:$CompleteWhenTaskTerminal `
        -RequireDistinctPhysicalDisks:$RequireDistinctPhysicalDisks -ThrowOnError:$false
    if (-not $validation.Valid) {
        throw $validation.Code
    }
    if (-not (Test-IsAdministrator)) {
        throw 'RUST_V2_ACCEPTANCE_ADMIN_REQUIRED'
    }
    $media = [string]$resolvedMediaRoots[0]
    foreach ($mediaRoot in @($resolvedMediaRoots)) {
        if ($mediaRoot.StartsWith(($layout.Root + '\'), [StringComparison]::OrdinalIgnoreCase) -or
            $layout.Root.StartsWith(($mediaRoot + '\'), [StringComparison]::OrdinalIgnoreCase)) {
            throw 'RUST_V2_REAL_MEDIA_STAGING_OVERLAP'
        }
    }

    New-Item -ItemType Directory -Path $layout.Root, $layout.Release, $layout.Data, $layout.Logs, `
        $layout.Cache, $layout.Temp, $layout.Evidence, $layout.Tools -Force | Out-Null
    Copy-RuntimeAcceptanceRelease -Source $resolvedRelease -Destination $layout.Release
    $physicalDiskMap = $null
    $physicalDiskMapPath = Join-Path $layout.Evidence 'physical-disk-map.json'
    $physicalDiskMapSha256 = ''
    if ($RequireDistinctPhysicalDisks) {
        # 显式双物理盘验收在启动 Node 前记录映射；相同 DiskNumber 直接拒绝。
        $physicalDiskMap = Get-RuntimePhysicalDiskMap -MediaRoots $resolvedMediaRoots `
            -RequireDistinctPhysicalDisks
        [IO.File]::WriteAllText($physicalDiskMapPath,
            ($physicalDiskMap | ConvertTo-Json -Depth 12), [Text.UTF8Encoding]::new($false))
        $physicalDiskMapSha256 = Get-FileSha256OrNull -Path $physicalDiskMapPath
    }
    $port = Get-FreeTcpPort
    # bootstrap 与 node 配置必须使用同一 TOML 解码后的绝对路径，避免启动前精确比较失败。
    $bootstrapConfigPath = Join-Path $layout.Data 'config.toml'
    [IO.File]::WriteAllText(
        (Join-Path $layout.Release 'bootstrap.toml'),
        "config_path = $(ConvertTo-TomlBasicString -Value $bootstrapConfigPath)`n",
        [Text.UTF8Encoding]::new($false))
    $configText = New-IsolatedNodeConfig -Port $port -WorkerCount $WorkerCount `
        -HddThreadsPerDisk $HddThreadsPerDisk -SsdThreadsPerDisk $SsdThreadsPerDisk `
        -UnknownThreadsPerDisk $UnknownThreadsPerDisk -TotalReadThreads $TotalReadThreads `
        -ReservedCores $ReservedCores -DataRoot $layout.Data -Enumerator $Enumerator
    [IO.File]::WriteAllText(
        (Join-Path $layout.Data 'config.toml'),
        $configText,
        [Text.UTF8Encoding]::new($false))
    $configSha256 = Get-TextSha256 -Text $configText

    $before = if (@($resolvedMediaRoots).Count -eq 1) {
        Get-RuntimeMediaManifest -MediaRoot $media
    }
    else {
        Get-RuntimeMediaManifest -MediaRoots $resolvedMediaRoots
    }
    [IO.File]::WriteAllText((Join-Path $layout.Evidence 'media-before.json'),
        ($before | ConvertTo-Json -Depth 8), [Text.UTF8Encoding]::new($false))
    $mediaBeforeSha256 = Get-MediaEvidenceSha256 -Manifest $before
    $mediaBeforeRootPaths = @()
    $mediaBeforeRootSha256 = @()
    if (@($resolvedMediaRoots).Count -gt 1) {
        for ($rootIndex = 0; $rootIndex -lt @($resolvedMediaRoots).Count; $rootIndex++) {
            $rootManifest = Get-RuntimeMediaManifest -MediaRoot $resolvedMediaRoots[$rootIndex]
            $rootPath = Join-Path $layout.Evidence ('media-before-root-{0:d2}.json' -f ($rootIndex + 1))
            [IO.File]::WriteAllText($rootPath, ($rootManifest | ConvertTo-Json -Depth 8), [Text.UTF8Encoding]::new($false))
            $mediaBeforeRootPaths += Get-NormalizedAbsolutePath -Path $rootPath
            $mediaBeforeRootSha256 += Get-FileSha256OrNull -Path $rootPath
        }
    }
    $runtimeOutput = Join-Path $layout.Evidence 'runtime.ndjson'
    $systemOutput = Join-Path $layout.Evidence 'system.ndjson'
    $stdout = Join-Path $layout.Evidence 'client.stdout.log'
    $stderr = Join-Path $layout.Evidence 'client.stderr.log'
    # Node 的启动日志与客户端日志同属本次 evidence，便于定位 preflight 退出。
    $nodeStdout = Join-Path $layout.Evidence 'node.stdout.log'
    $nodeStderr = Join-Path $layout.Evidence 'node.stderr.log'
    $node = $null
    $client = $null
    $supervisor = $null
    $after = $null
    $runtimeResult = $null
    $taskFileStats = $null
    $scanRecords = @()
    $terminalScanObserved = $false
    $completedScanObserved = $false
    $databaseSnapshot = $null
    $resultSummary = [pscustomobject]@{
        Status = 'INCONCLUSIVE'; Path = Join-Path $layout.Evidence 'result-summary.tsv'
        Sha256 = $null; RowCount = 0; MissingCount = 0; InconclusiveCount = 0; TaskId = $null
    }
    $exporterExitCode = -1
    $exporterSucceeded = $false
    $resultSummaryBindingValid = $false
    $completedTaskIdPresent = $false
    $nodeUnexpectedExit = $false
    # 标记 Node 是否在客户端启动前退出；该状态属于基础设施不完整，不应伪装成业务 FAIL。
    $nodeStartupFailed = $false
    $runDiagnostic = ''
    $mediaUnchanged = $false
    $mediaBeforeRootPaths = @($mediaBeforeRootPaths)
    $mediaAfterRootPaths = @()
    $mediaBeforeRootSha256 = @($mediaBeforeRootSha256)
    $mediaAfterRootSha256 = @()
    $nodeExecutablePath = Get-NormalizedAbsolutePath -Path (Join-Path $layout.Release 'node.exe')
    $clientExecutablePath = Get-NormalizedAbsolutePath -Path $AcceptanceClientPath
    $supervisorStatusPath = Join-Path $layout.Evidence 'supervisor-status.json'
    $completionOnTerminal = [bool]$SingleRun -or [bool]$CompleteWhenTaskTerminal
    $savedEnvironment = @{}
    foreach ($name in @(
        'RUST_V2_ACCEPTANCE_ENDPOINT',
        'RUST_V2_REAL_MEDIA_ROOT',
        'RUST_V2_REAL_MEDIA_ROOTS_JSON',
        'RUST_V2_ACCEPTANCE_DURATION_SECONDS',
        'RUST_V2_ACCEPTANCE_OUTPUT',
        'RUST_V2_ACCEPTANCE_ENUMERATOR',
        'RUST_V2_ACCEPTANCE_SINGLE_RUN')) {
        $savedEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
    }

    try {
        $nodeArgs = @{
            FilePath = (Join-Path $layout.Release 'node.exe')
            WorkingDirectory = $layout.Release
            PassThru = $true
            WindowStyle = 'Hidden'
            RedirectStandardOutput = $nodeStdout
            RedirectStandardError = $nodeStderr
        }
        $node = Start-Process @nodeArgs
        $nodeStartTimeUtc = ''
        try {
            $nodeStartTimeUtc = $node.StartTime.ToUniversalTime().ToString('O')
        }
        catch {
            throw 'RUST_V2_ACCEPTANCE_SUPERVISOR_NODE_IDENTITY_UNAVAILABLE'
        }
        Wait-TcpEndpoint -Port $port -Process $node

        $env:RUST_V2_ACCEPTANCE_ENDPOINT = "127.0.0.1:$port"
        $env:RUST_V2_REAL_MEDIA_ROOT = $media
        if (@($resolvedMediaRoots).Count -gt 1) {
            $env:RUST_V2_REAL_MEDIA_ROOTS_JSON = ConvertTo-Json -InputObject ([string[]]$resolvedMediaRoots) -Compress
        }
        else {
            [Environment]::SetEnvironmentVariable('RUST_V2_REAL_MEDIA_ROOTS_JSON', $null, 'Process')
        }
        $env:RUST_V2_ACCEPTANCE_DURATION_SECONDS = [string]$DurationSeconds
        $env:RUST_V2_ACCEPTANCE_OUTPUT = $runtimeOutput
        $env:RUST_V2_ACCEPTANCE_ENUMERATOR = $Enumerator
        if ($completionOnTerminal) {
            $env:RUST_V2_ACCEPTANCE_SINGLE_RUN = '1'
        }
        else {
            [Environment]::SetEnvironmentVariable('RUST_V2_ACCEPTANCE_SINGLE_RUN', $null, 'Process')
        }
        # 监督时钟从客户端启动前开始；客户端即使卡在 TCP/RPC 等待中也受外层硬截止约束。
        $started = [Diagnostics.Stopwatch]::StartNew()
        $supervisorDeadlineSeconds = Get-RuntimeAcceptanceSupervisorDeadlineSeconds -DurationSeconds $DurationSeconds
        $supervisorDeadlineUtc = [DateTime]::UtcNow.AddSeconds($supervisorDeadlineSeconds)
        $client = Start-Process -FilePath $AcceptanceClientPath `
            -WorkingDirectory $layout.Release -PassThru -WindowStyle Hidden `
            -RedirectStandardOutput $stdout -RedirectStandardError $stderr
        $clientStartTimeUtc = ''
        try {
            $clientStartTimeUtc = $client.StartTime.ToUniversalTime().ToString('O')
        }
        catch {
            throw 'RUST_V2_ACCEPTANCE_SUPERVISOR_CLIENT_IDENTITY_UNAVAILABLE'
        }
        $supervisor = Start-RuntimeAcceptanceSupervisor -ClientId ([int]$client.Id) `
            -ClientPath $clientExecutablePath -ClientStartTimeUtc $clientStartTimeUtc `
            -NodeId ([int]$node.Id) -NodePath $nodeExecutablePath -NodeStartTimeUtc $nodeStartTimeUtc `
            -DeadlineUtc $supervisorDeadlineUtc -StatusPath $supervisorStatusPath
        if ($null -eq $supervisor) {
            throw 'RUST_V2_ACCEPTANCE_SUPERVISOR_START_FAILED'
        }

        $previousCpu = @{}
        $previousIo = @{}
        $previousSampleElapsedMs = -1.0
        while (-not $client.HasExited) {
            $supervisorElapsedSeconds = Get-RuntimeAcceptanceElapsedSeconds -Stopwatch $started
            $supervisorStatus = Get-RuntimeAcceptanceSupervisorStatus -Supervisor $supervisor
            if ($null -ne $supervisorStatus -and -not [string]::IsNullOrWhiteSpace([string]$supervisorStatus.Diagnostic)) {
                $runDiagnostic = [string]$supervisorStatus.Diagnostic
                throw $runDiagnostic
            }
            if ($supervisorElapsedSeconds -ge $supervisorDeadlineSeconds) {
                # 稳定诊断会在 catch/finally 中保留，同时触发客户端、Node 和 Worker 清理。
                $runDiagnostic = 'RUST_V2_ACCEPTANCE_SUPERVISOR_TIMEOUT'
                throw $runDiagnostic
            }
            $sampleElapsedMs = $started.Elapsed.TotalMilliseconds
            Write-SystemSample -Path $systemOutput -Root $layout.Root `
                -ElapsedMilliseconds $sampleElapsedMs `
                -PreviousSampleElapsedMilliseconds $previousSampleElapsedMs `
                -PreviousCpu $previousCpu -PreviousIo $previousIo
            $previousSampleElapsedMs = $sampleElapsedMs
            # 同步采样可能耗时；采样后再次检查并把睡眠裁剪到 hard deadline 剩余时间。
            $supervisorStatus = Get-RuntimeAcceptanceSupervisorStatus -Supervisor $supervisor
            if ($null -ne $supervisorStatus -and -not [string]::IsNullOrWhiteSpace([string]$supervisorStatus.Diagnostic)) {
                $runDiagnostic = [string]$supervisorStatus.Diagnostic
                throw $runDiagnostic
            }
            $supervisorElapsedSeconds = Get-RuntimeAcceptanceElapsedSeconds -Stopwatch $started
            $remainingMilliseconds = ($supervisorDeadlineSeconds - $supervisorElapsedSeconds) * 1000
            if ($remainingMilliseconds -le 0) {
                $runDiagnostic = 'RUST_V2_ACCEPTANCE_SUPERVISOR_TIMEOUT'
                throw $runDiagnostic
            }
            $sleepMilliseconds = [Math]::Min(
                [double]$SampleSeconds * 1000,
                [double][Math]::Ceiling($remainingMilliseconds))
            if ($sleepMilliseconds -gt 0) {
                Start-Sleep -Milliseconds ([int]$sleepMilliseconds)
            }
            $supervisorStatus = Get-RuntimeAcceptanceSupervisorStatus -Supervisor $supervisor
            if ($null -ne $supervisorStatus -and -not [string]::IsNullOrWhiteSpace([string]$supervisorStatus.Diagnostic)) {
                $runDiagnostic = [string]$supervisorStatus.Diagnostic
                throw $runDiagnostic
            }
            if ((Get-RuntimeAcceptanceElapsedSeconds -Stopwatch $started) -ge $supervisorDeadlineSeconds) {
                $runDiagnostic = 'RUST_V2_ACCEPTANCE_SUPERVISOR_TIMEOUT'
                throw $runDiagnostic
            }
            $client.Refresh()
            if ($node.HasExited) {
                $nodeUnexpectedExit = $true
                throw "RUST_V2_ACCEPTANCE_NODE_EXITED code=$($node.ExitCode)"
            }
        }
        $clientExitCode = Get-CompletedProcessExitCode -Process $client
        if ($null -eq $clientExitCode) {
            throw 'RUST_V2_ACCEPTANCE_CLIENT_EXIT_UNCONFIRMED'
        }
        if ($clientExitCode -ne 0) {
            throw "RUST_V2_ACCEPTANCE_CLIENT_FAILED code=$clientExitCode"
        }

        $after = if (@($resolvedMediaRoots).Count -eq 1) {
            Get-RuntimeMediaManifest -MediaRoot $media
        }
        else {
            Get-RuntimeMediaManifest -MediaRoots $resolvedMediaRoots
        }
        [IO.File]::WriteAllText((Join-Path $layout.Evidence 'media-after.json'),
            ($after | ConvertTo-Json -Depth 8), [Text.UTF8Encoding]::new($false))
        Assert-RuntimeMediaUnchanged -Before $before -After $after
        $mediaUnchanged = $true
        $mediaAfterRootPaths = @()
        $mediaAfterRootSha256 = @()
        if (@($resolvedMediaRoots).Count -gt 1) {
            for ($rootIndex = 0; $rootIndex -lt @($resolvedMediaRoots).Count; $rootIndex++) {
                $rootManifest = Get-RuntimeMediaManifest -MediaRoot $resolvedMediaRoots[$rootIndex]
                $rootPath = Join-Path $layout.Evidence ('media-after-root-{0:d2}.json' -f ($rootIndex + 1))
                [IO.File]::WriteAllText($rootPath, ($rootManifest | ConvertTo-Json -Depth 8), [Text.UTF8Encoding]::new($false))
                $mediaAfterRootPaths += Get-NormalizedAbsolutePath -Path $rootPath
                $mediaAfterRootSha256 += Get-FileSha256OrNull -Path $rootPath
            }
        }

        # 终态清理 runtime 前尽力读取任务 TSV；若已清理则写入明确的不可用状态。
        $taskFileStats = Get-RuntimeTaskFileStatistics -RuntimeRoot (Join-Path $layout.Data 'runtime')
        # 客户端终态和 media-after 已保存后，才请求 Node 及所有 Worker 退出。
        $shutdownDiagnostic = Request-IsolatedNodeExit -Node $node -Root $layout.Release
        if ($shutdownDiagnostic) { $runDiagnostic = $shutdownDiagnostic }
        try {
            $runtimeResult = Get-LastRuntimeResult -Path $runtimeOutput
        }
        catch {
            $runDiagnostic = 'RUST_V2_ACCEPTANCE_RUNTIME_NDJSON_INVALID'
            $runtimeResult = $null
        }
        $completedTaskId = if ($runtimeResult) { [string]$runtimeResult.latest_completed_persistent_task_id } else { '' }
        $completedTaskIdPresent = -not [string]::IsNullOrWhiteSpace($completedTaskId)
        $scanRecords = if ($runtimeResult) { @($runtimeResult.scan_tasks) } else { @() }
        $completedScanObserved = @($scanRecords | Where-Object { [string]$_.terminal_state -eq 'completed' }).Count -gt 0
        $terminalScanObserved = @($scanRecords | Where-Object {
                [string]$_.terminal_state -in @('completed', 'failed', 'cancelled')
            }).Count -gt 0
        if (-not $terminalScanObserved) {
            if (-not $runDiagnostic) { $runDiagnostic = 'RUST_V2_ACCEPTANCE_TERMINAL_STATE_MISSING' }
        }
        elseif ($completedScanObserved -and -not $shutdownDiagnostic) {
            $databasePath = Join-Path $layout.Data 'node.db'
            # 只读快照必须在受控停机并确认隔离进程归零后创建，避免 SQLite 首次打开自变更 WAL/SHM。
            $databaseSnapshot = New-ReadOnlyDatabaseSnapshot -DatabasePath $databasePath `
                -EvidenceRoot $layout.Evidence
            $export = Invoke-ResultExporter -ExporterPath $ResultExporterPath `
                -DatabasePath $databaseSnapshot.DatabasePath `
                -CacheRoot $layout.Cache -MediaRoots $resolvedMediaRoots `
                -OutputPath $resultSummary.Path -EvidenceRoot $layout.Evidence
            $exporterExitCode = $export.ExitCode
            if ($export.Diagnostic) {
                $runDiagnostic = $export.Diagnostic
            }
            elseif ($exporterExitCode -ne 0) {
                $runDiagnostic = 'RUST_V2_ACCEPTANCE_EXPORTER_FAILED'
            }
            else {
                $exporterSucceeded = $true
                try {
                    $resultSummary = Parse-ResultSummaryOutput -Text $export.Stdout `
                        -ExpectedPath $resultSummary.Path
                    $artifacts = Get-ResultSummaryArtifacts -SummaryPath $resultSummary.Path `
                        -ExpectedStatus $resultSummary.Status `
                        -ExpectedSha256 $resultSummary.Sha256 -ExpectedRowCount $resultSummary.RowCount
                    if (-not $artifacts.BindingValid) {
                        throw $artifacts.Diagnostic
                    }
                    $resultSummaryBindingValid = $true
                }
                catch {
                    $runDiagnostic = $_.Exception.Message
                    $resultSummary = [pscustomobject]@{
                        Status = 'INCONCLUSIVE'; Path = $resultSummary.Path; Sha256 = $null
                        RowCount = 0; MissingCount = 0; InconclusiveCount = 0; TaskId = $completedTaskId
                    }
                }
            }
        }
    }
    catch {
        if (-not $runDiagnostic) { $runDiagnostic = $_.Exception.Message }
        if ($_.Exception.Message -match 'NODE_EXITED') {
            $nodeUnexpectedExit = $true
            $nodeStartupFailed = $null -eq $client
        }
    }
    finally {
        foreach ($name in $savedEnvironment.Keys) {
            [Environment]::SetEnvironmentVariable($name, $savedEnvironment[$name], 'Process')
        }
        $supervisorStatusAtFinally = $null
        try {
            if ($null -ne $supervisor) {
                $supervisorStatusAtFinally = Get-RuntimeAcceptanceSupervisorStatus -Supervisor $supervisor
                if ($null -ne $supervisorStatusAtFinally -and [bool]$supervisorStatusAtFinally.TimedOut) {
                    # 超时监督器正在 stopping 时不能被 finally 先取消；有界等待其写出 Kill/Wait 结果。
                    $supervisorStatusAtFinally = Wait-RuntimeAcceptanceSupervisorFinalStatus -Supervisor $supervisor -TimeoutMilliseconds 12000
                }
                if ($null -ne $supervisorStatusAtFinally -and
                    -not [string]::IsNullOrWhiteSpace([string]$supervisorStatusAtFinally.Diagnostic)) {
                    if ([string]::IsNullOrWhiteSpace($runDiagnostic)) {
                        $runDiagnostic = [string]$supervisorStatusAtFinally.Diagnostic
                    }
                    elseif ($runDiagnostic -notlike "*$($supervisorStatusAtFinally.Diagnostic)*") {
                        $runDiagnostic = "$runDiagnostic;$($supervisorStatusAtFinally.Diagnostic)"
                    }
                }
            }
        }
        catch {
            if ([string]::IsNullOrWhiteSpace($runDiagnostic)) {
                $runDiagnostic = 'RUST_V2_ACCEPTANCE_SUPERVISOR_STATUS_FAILED'
            }
        }
        # 身份校验失败时不按旧 PID 盲杀，避免 PID 复用误伤；证据会保持 INCONCLUSIVE。
        $clientIdentityTrusted = $true
        if ($null -ne $supervisorStatusAtFinally -and
            $supervisorStatusAtFinally.PSObject.Properties['ClientIdentityValid'] -and
            -not [bool]$supervisorStatusAtFinally.ClientIdentityValid) {
            $clientIdentityTrusted = $false
        }
        try {
            if ($null -ne $client -and $clientIdentityTrusted -and -not $client.HasExited) {
                Stop-Process -Id $client.Id -Force -ErrorAction Stop
            }
            if ($null -ne $client) {
                $clientExitConfirmed = Wait-RuntimeAcceptanceProcessExit -Process $client -TimeoutMilliseconds 5000
                if (-not $clientExitConfirmed) {
                    $unconfirmed = 'RUST_V2_ACCEPTANCE_CLIENT_EXIT_UNCONFIRMED'
                    if ([string]::IsNullOrWhiteSpace($runDiagnostic)) { $runDiagnostic = $unconfirmed }
                    elseif ($runDiagnostic -notlike "*$unconfirmed*") { $runDiagnostic = "$runDiagnostic;$unconfirmed" }
                }
            }
        }
        catch {
            if ([string]::IsNullOrWhiteSpace($runDiagnostic)) { $runDiagnostic = 'RUST_V2_ACCEPTANCE_CLIENT_STOP_FAILED' }
        }
        try {
            if ($null -ne $node -and -not $node.HasExited) {
                $cleanupDiagnostic = Request-IsolatedNodeExit -Node $node -Root $layout.Release
                if ($cleanupDiagnostic -and -not $runDiagnostic) { $runDiagnostic = $cleanupDiagnostic }
            }
        }
        catch {
            if (-not $runDiagnostic) { $runDiagnostic = 'RUST_V2_ACCEPTANCE_PROCESS_STOP_FAILED' }
        }
        try {
            if ($null -ne $supervisor) {
                $supervisorStop = Stop-RuntimeAcceptanceSupervisor -Supervisor $supervisor
                if ($null -eq $supervisorStop -or -not [bool]$supervisorStop.ExitConfirmed) {
                    $supervisorDiagnostic = if ($supervisorStop -and $supervisorStop.Diagnostic) {
                        [string]$supervisorStop.Diagnostic
                    }
                    else {
                        'RUST_V2_ACCEPTANCE_SUPERVISOR_EXIT_UNCONFIRMED'
                    }
                    if ([string]::IsNullOrWhiteSpace($runDiagnostic)) { $runDiagnostic = $supervisorDiagnostic }
                    elseif ($runDiagnostic -notlike "*$supervisorDiagnostic*") { $runDiagnostic = "$runDiagnostic;$supervisorDiagnostic" }
                }
            }
        }
        catch {
            if ([string]::IsNullOrWhiteSpace($runDiagnostic)) { $runDiagnostic = 'RUST_V2_ACCEPTANCE_SUPERVISOR_STOP_FAILED' }
        }
        if ($null -eq $after) {
            try {
                $after = if (@($resolvedMediaRoots).Count -eq 1) {
                    Get-RuntimeMediaManifest -MediaRoot $media
                }
                else {
                    Get-RuntimeMediaManifest -MediaRoots $resolvedMediaRoots
                }
                [IO.File]::WriteAllText((Join-Path $layout.Evidence 'media-after.json'),
                    ($after | ConvertTo-Json -Depth 8), [Text.UTF8Encoding]::new($false))
                Assert-RuntimeMediaUnchanged -Before $before -After $after
                $mediaUnchanged = $true
                if (@($resolvedMediaRoots).Count -gt 1) {
                    for ($rootIndex = 0; $rootIndex -lt @($resolvedMediaRoots).Count; $rootIndex++) {
                        $rootManifest = Get-RuntimeMediaManifest -MediaRoot $resolvedMediaRoots[$rootIndex]
                        $rootPath = Join-Path $layout.Evidence ('media-after-root-{0:d2}.json' -f ($rootIndex + 1))
                        [IO.File]::WriteAllText($rootPath, ($rootManifest | ConvertTo-Json -Depth 8), [Text.UTF8Encoding]::new($false))
                        $mediaAfterRootPaths += Get-NormalizedAbsolutePath -Path $rootPath
                        $mediaAfterRootSha256 += Get-FileSha256OrNull -Path $rootPath
                    }
                }
            }
            catch {
                if (-not $runDiagnostic) { $runDiagnostic = $_.Exception.Message }
            }
        }
        if ($null -eq $runtimeResult) {
            try {
                $runtimeResult = Get-LastRuntimeResult -Path $runtimeOutput
            }
            catch {
                if (-not $runDiagnostic) { $runDiagnostic = 'RUST_V2_ACCEPTANCE_RUNTIME_NDJSON_INVALID' }
                $runtimeResult = $null
            }
        }
    }

    $mediaAfterSha256 = if ($after) { Get-MediaEvidenceSha256 -Manifest $after } else { $null }
    $packageManifestSha256 = Get-ManifestSha256OrNull -ReleaseRoot $resolvedRelease
    $packageShaValue = if ($PackageSha256) { $PackageSha256 } elseif ($PackagePath) { Get-FileSha256OrNull -Path $PackagePath } else { $null }
    $databaseSnapshotRoot = if ($databaseSnapshot) { $databaseSnapshot.SnapshotRoot } else { '' }
    $databaseSnapshotPath = if ($databaseSnapshot) { $databaseSnapshot.DatabasePath } else { '' }
    $databaseSnapshotMetadataPath = if ($databaseSnapshot) { $databaseSnapshot.MetadataPath } else { '' }
    # 摘要三件套已绑定时，MISSING/INCONCLUSIVE 是业务 FAIL；ID、exporter 或绑定缺失仍为 INCONCLUSIVE。
    $deadlineTaskId = if ($runtimeResult) { [string]$runtimeResult.deadline_cancelled_persistent_task_id } else { '' }
    $scanTasks = if ($runtimeResult) { @($runtimeResult.scan_tasks) } else { @() }
    # failed 永远是业务失败；只有 cancelled 且匹配 deadline ID 才能豁免。
    $nonDeadlineFailedScan = @($scanTasks | Where-Object {
        [string]$_.terminal_state -eq 'failed'
    }).Count -gt 0
    $nonDeadlineCancelledScan = @($scanTasks | Where-Object {
        [string]$_.terminal_state -eq 'cancelled' -and
        ([string]::IsNullOrWhiteSpace($deadlineTaskId) -or [string]$_.persistent_task_id -ne $deadlineTaskId)
    }).Count -gt 0
    $failedScanCount = if ($runtimeResult) { [int64]$runtimeResult.failed_scans } else { 0 }
    $clientExitCode = if ($client) { Get-CompletedProcessExitCode -Process $client } else { $null }
    $clientExitFailed = $null -ne $clientExitCode -and $clientExitCode -ne 0
    $clientExitUnconfirmed = $null -ne $client -and $null -eq $clientExitCode
    # 任一核心采样文件缺失都表示本次运行证据不完整。
    $runtimeEvidenceMissing = -not (Test-Path -LiteralPath $runtimeOutput -PathType Leaf) -or
        -not (Test-Path -LiteralPath $systemOutput -PathType Leaf)
    $runStatus = Get-ResultSummaryRunStatus `
        -RuntimeEvidenceComplete:(-not $runtimeEvidenceMissing) `
        -NodeStartupFailed:$nodeStartupFailed -NodeUnexpectedExit:$nodeUnexpectedExit `
        -ClientExitFailed:$clientExitFailed -MediaUnchanged:$mediaUnchanged `
        -ScanFailed:($failedScanCount -gt 0 -or $nonDeadlineFailedScan -or $nonDeadlineCancelledScan) `
        -ClientExitUnconfirmed:$clientExitUnconfirmed `
        -RuntimeTaskTerminalObserved:$terminalScanObserved `
        -CompletedTaskIdPresent:$completedTaskIdPresent -ExporterSucceeded:$exporterSucceeded `
        -SummaryBindingValid:$resultSummaryBindingValid -SummaryStatus $resultSummary.Status `
        -RunDiagnostic $runDiagnostic
    # 复用前面已解析的截止取消任务 ID，避免 PowerShell 将 if 误解析为命令。
    $harnessResult = New-HarnessResult -Variant $Variant -RunIndex $RunIndex `
        -SourceRevision $SourceRevision -SourceTreeSha256 $SourceTreeSha256 `
        -PackagePath $PackagePath -PackageSha256 $packageShaValue -ReleaseRoot $layout.Release `
        -DatabaseSnapshotRoot $databaseSnapshotRoot -DatabaseSnapshotPath $databaseSnapshotPath `
        -DatabaseSnapshotMetadataPath $databaseSnapshotMetadataPath `
        -ConfigSha256 $configSha256 -PackageManifestSha256 $packageManifestSha256 `
        -MediaBeforeSha256 $mediaBeforeSha256 -MediaAfterSha256 $mediaAfterSha256 `
        -MediaRoots $resolvedMediaRoots -SingleRun:$completionOnTerminal `
        -PhysicalDiskMapPath $(if ($physicalDiskMap) { $physicalDiskMapPath } else { '' }) `
        -PhysicalDiskMapSha256 $physicalDiskMapSha256 `
        -MediaBeforeRootPaths $mediaBeforeRootPaths -MediaAfterRootPaths $mediaAfterRootPaths `
        -MediaBeforeRootSha256 $mediaBeforeRootSha256 -MediaAfterRootSha256 $mediaAfterRootSha256 `
        -ResultSummary $resultSummary -ResultSummaryStatus $resultSummary.Status `
        -ResultSummaryPath $resultSummary.Path -ResultSummarySha256 $resultSummary.Sha256 `
        -ResultSummaryTaskId $resultSummary.TaskId -ResultSummaryMissingCount $resultSummary.MissingCount `
        -ResultSummaryInconclusiveCount $resultSummary.InconclusiveCount -ResultSummaryRowCount $resultSummary.RowCount `
        -RunStatus $runStatus -RunDiagnostic $runDiagnostic -MediaUnchanged $mediaUnchanged -NodeUnexpectedExit $nodeUnexpectedExit `
        -ExporterExitCode $exporterExitCode `
        -DeadlineCancelledPersistentTaskId $deadlineTaskId `
        -EffectiveWorkerCount $WorkerCount -HddThreadsPerDisk $HddThreadsPerDisk `
        -SsdThreadsPerDisk $SsdThreadsPerDisk -UnknownThreadsPerDisk $UnknownThreadsPerDisk `
        -ReadTotalThreads $TotalReadThreads -ReservedCores $ReservedCores
    # 这些统计只在任务 TSV 尚未被终态清理时读取；协议没有暴露的计数保持 null，禁止伪造。
    $harnessResult = [pscustomobject]$harnessResult
    $harnessResult | Add-Member -NotePropertyName enumerator -NotePropertyValue ([string]$Enumerator).Trim().ToLowerInvariant()
    $harnessResult | Add-Member -NotePropertyName complete_when_task_terminal -NotePropertyValue $completionOnTerminal
    $harnessResult | Add-Member -NotePropertyName task_file_stats -NotePropertyValue $taskFileStats
    $harnessResult | Add-Member -NotePropertyName cache_hits_not_in_task_file -NotePropertyValue ([ordered]@{
            status = 'UNAVAILABLE'; source = 'runtime_protocol_not_exposed'; count = $null
        })
    $harnessResult | Add-Member -NotePropertyName sqlite_runtime_write_counts -NotePropertyValue ([ordered]@{
            status = 'UNAVAILABLE'; source = 'runtime_protocol_not_exposed'; task = $null; analysis = $null; delete = $null
        })
    [IO.File]::WriteAllText((Join-Path $layout.Evidence 'harness-result.json'),
        ($harnessResult | ConvertTo-Json -Depth 12), [Text.UTF8Encoding]::new($false))

    $reporter = Join-Path $script:RepositoryRoot 'tests\windows\New-RustV2RuntimeAcceptanceReport.ps1'
    try {
        if (Test-Path -LiteralPath $reporter -PathType Leaf) {
            & $reporter -EvidenceRoot $layout.Evidence -OutputPath $layout.Report | Out-Null
        }
    }
    catch {
        $fallback = "# Rust V2 单轮运行验收`n`n结论：INCONCLUSIVE`n`n- 报告生成失败：$($_.Exception.Message)`n- 原始证据目录：$($layout.Evidence)`n"
        [IO.File]::WriteAllText($layout.Report, $fallback, [Text.UTF8Encoding]::new($true))
    }
    Write-Output "RUST_V2_RUNTIME_ACCEPTANCE_MEASURE_$runStatus"
    Write-Output "RUN_ROOT=$($layout.Root)"
    Write-Output "EVIDENCE_ROOT=$($layout.Evidence)"
    Write-Output "REPORT_PATH=$($layout.Report)"
}

if (-not $LibraryOnly) {
    $invokeMediaRoot = if ($PSBoundParameters.ContainsKey('MediaRoots') -and
        -not $PSBoundParameters.ContainsKey('MediaRoot')) { '' } else { $MediaRoot }
    Invoke-RustV2RuntimeAcceptance -MediaRoot $invokeMediaRoot -MediaRoots $MediaRoots -DurationSeconds $DurationSeconds `
        -SampleSeconds $SampleSeconds -CargoTargetDir $CargoTargetDir -ReleaseRoot $ReleaseRoot `
        -AcceptanceClientPath $AcceptanceClientPath -ResultExporterPath $ResultExporterPath `
        -EvidenceRoot $EvidenceRoot -ReportPath $ReportPath -Variant $Variant -RunIndex $RunIndex `
        -SourceRevision $SourceRevision -SourceTreeSha256 $SourceTreeSha256 `
        -PackagePath $PackagePath -PackageSha256 $PackageSha256 `
        -Enumerator $Enumerator -CompleteWhenTaskTerminal:$CompleteWhenTaskTerminal `
        -WorkerCount $WorkerCount -HddThreadsPerDisk $HddThreadsPerDisk `
        -SsdThreadsPerDisk $SsdThreadsPerDisk -UnknownThreadsPerDisk $UnknownThreadsPerDisk `
        -TotalReadThreads $TotalReadThreads -ReservedCores $ReservedCores `
        -SingleRun:$SingleRun -RequireDistinctPhysicalDisks:$RequireDistinctPhysicalDisks `
        -RequireApprovedMediaRoots
}
