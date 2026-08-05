[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string]$NodeTrayExe,
    [ValidateRange(0, 3600)]
    [int]$WarmupSec = 120,
    [ValidateRange(5, 3600)]
    [int]$DurationSec = 300,
    [Parameter(Mandatory)]
    [string]$OutFile,
    [int]$NodeTrayPid = 0,
    [switch]$AllowProcessControl,
    [switch]$ValidateResultOnly,
    [string]$SamplesFile,
    [string]$ThroughputEvidenceFile
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$script:Blocked = 'BLOCKED_NOT_RUN_DYNAMIC'
$script:MemoryLimitBytes = 256L * 1024L * 1024L
$script:AverageCPUThreshold = 1.0

function Test-SameOrBelow {
    param([string]$Path, [string]$Root)
    $fullPath = [IO.Path]::GetFullPath($Path).TrimEnd('\')
    $fullRoot = [IO.Path]::GetFullPath($Root).TrimEnd('\')
    return $fullPath.Equals($fullRoot, [StringComparison]::OrdinalIgnoreCase) -or
        $fullPath.StartsWith($fullRoot + '\', [StringComparison]::OrdinalIgnoreCase)
}

function Test-ReparsePointInExistingChain {
    param([string]$Path)
    $cursor = [IO.Path]::GetFullPath($Path)
    while (-not [string]::IsNullOrWhiteSpace($cursor)) {
        if (Test-Path -LiteralPath $cursor) {
            $item = Get-Item -LiteralPath $cursor -Force
            if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
                return $true
            }
        }
        $parent = [IO.Directory]::GetParent($cursor)
        if ($null -eq $parent) { break }
        $cursor = $parent.FullName
    }
    return $false
}

function Get-ValidatedStageExecutable {
    param([string]$Candidate)
    if (-not [IO.Path]::IsPathRooted($Candidate)) {
        throw 'NODETRAY_EXE_MUST_BE_ABSOLUTE'
    }
    $full = [IO.Path]::GetFullPath($Candidate)
    if (-not $Candidate.Equals($full, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'NODETRAY_EXE_MUST_BE_CANONICAL'
    }
    if (-not ([IO.Path]::GetFileName($full)).Equals(
            'nodetray.exe', [StringComparison]::OrdinalIgnoreCase)) {
        throw 'NODETRAY_EXE_BASENAME_INVALID'
    }
    $parent = [IO.Path]::GetDirectoryName($full).TrimEnd('\')
    $dedicated = $parent -match '(?i)^C:\\tmp\\mysingerserver-nodetray-stage(?:-[0-9a-f]{32})?$'
    $isolated = $parent -match '(?i)^C:\\tmp\\mysingerserver-node-tray-(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\\stage$'
    if (-not ($dedicated -or $isolated)) {
        throw 'NODETRAY_EXE_NOT_IN_DEDICATED_STAGE'
    }
    if (Test-ReparsePointInExistingChain $full) {
        throw 'NODETRAY_EXE_REPARSE_POINT_REJECTED'
    }
    return $full
}

function Get-ValidatedOutFile {
    param([string]$Candidate)
    if (-not [IO.Path]::IsPathRooted($Candidate)) {
        throw 'OUT_FILE_MUST_BE_ABSOLUTE'
    }
    $full = [IO.Path]::GetFullPath($Candidate)
    if (-not $Candidate.Equals($full, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'OUT_FILE_MUST_BE_CANONICAL'
    }
    if (-not $full.EndsWith('.json', [StringComparison]::OrdinalIgnoreCase)) {
        throw 'OUT_FILE_MUST_BE_JSON'
    }
    $rootMatch = [regex]::Match($full,
        '(?i)^(C:\\tmp\\mysingerserver-node-tray-(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}))(?:\\.+)$')
    if (-not $rootMatch.Success) { throw 'OUT_FILE_NOT_IN_GUID_TEST_ROOT' }
    $root = $rootMatch.Groups[1].Value
    if (-not (Test-SameOrBelow $full $root) -or
        $full.Equals($root, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'OUT_FILE_ESCAPED_GUID_TEST_ROOT'
    }
    if (Test-ReparsePointInExistingChain $full) {
        throw 'OUT_FILE_REPARSE_POINT_REJECTED'
    }
    return $full
}

function Get-ProcessPath {
    param([Diagnostics.Process]$Process)
    try { return $Process.Path } catch { return $null }
}

function Get-TargetProcessTree {
    param([int]$RootPid)
    $rows = @(Get-CimInstance Win32_Process -ErrorAction Stop |
        Select-Object ProcessId, ParentProcessId, Name)
    $selected = [Collections.Generic.HashSet[int]]::new()
    [void]$selected.Add($RootPid)
    do {
        $changed = $false
        foreach ($row in $rows) {
            if ($selected.Contains([int]$row.ParentProcessId) -and
                -not $selected.Contains([int]$row.ProcessId)) {
                [void]$selected.Add([int]$row.ProcessId)
                $changed = $true
            }
        }
    } while ($changed)
    return @($rows | Where-Object {
        $selected.Contains([int]$_.ProcessId) -and
        ([int]$_.ProcessId -eq $RootPid -or
            $_.Name -ieq 'msedgewebview2.exe')
    })
}

function Get-ResourceSample {
    param([int]$RootPid, [datetime]$StartedUtc, [double]$ElapsedSeconds)
    $tree = @(Get-TargetProcessTree $RootPid)
    $processSamples = [Collections.Generic.List[object]]::new()
    foreach ($row in $tree) {
        $process = Get-Process -Id ([int]$row.ProcessId) -ErrorAction Stop
        $processSamples.Add([ordered]@{
            pid = $process.Id
            name = $process.ProcessName
            private_working_set_bytes = [long]$process.PrivateMemorySize64
            cpu_total_seconds = [double]$process.CPU
            handles = [int]$process.HandleCount
        })
    }
    return [ordered]@{
        timestamp_utc = [datetime]::UtcNow.ToString('o')
        elapsed_seconds = [math]::Round($ElapsedSeconds, 3)
        process_count = $processSamples.Count
        processes = @($processSamples)
        private_working_set_bytes = [long](($processSamples |
            Measure-Object private_working_set_bytes -Sum).Sum)
        cpu_total_seconds = [double](($processSamples |
            Measure-Object cpu_total_seconds -Sum).Sum)
        handles = [int](($processSamples | Measure-Object handles -Sum).Sum)
        process_started_utc = $StartedUtc.ToString('o')
    }
}

function Get-ResourceSummary {
    param([object[]]$Samples)
    $intervals = [Collections.Generic.List[double]]::new()
    for ($index = 1; $index -lt $Samples.Count; $index++) {
        $wall = [double]$Samples[$index].elapsed_seconds -
            [double]$Samples[$index - 1].elapsed_seconds
        $cpu = [double]$Samples[$index].cpu_total_seconds -
            [double]$Samples[$index - 1].cpu_total_seconds
        if ($wall -gt 0 -and $cpu -ge 0) {
            $intervals.Add(($cpu / $wall) * 100.0)
        }
    }
    $peakPws = [long](($Samples | Measure-Object private_working_set_bytes -Maximum).Maximum)
    $averageCPU = if ($intervals.Count -gt 0) {
        [double](($intervals | Measure-Object -Average).Average)
    } else { 0.0 }
    $peakCPU = if ($intervals.Count -gt 0) {
        [double](($intervals | Measure-Object -Maximum).Maximum)
    } else { 0.0 }
    $peakHandles = [int](($Samples | Measure-Object handles -Maximum).Maximum)
    return [ordered]@{
        peak_private_working_set_bytes = $peakPws
        peak_private_working_set_mib = [math]::Round($peakPws / 1MB, 3)
        average_single_core_cpu_percent = [math]::Round($averageCPU, 4)
        peak_single_core_cpu_percent = [math]::Round($peakCPU, 4)
        peak_handles = $peakHandles
        memory_pass = $peakPws -le $script:MemoryLimitBytes
        cpu_pass = $averageCPU -lt $script:AverageCPUThreshold
    }
}

function Get-TestRootFromOutFile {
    param([string]$ResolvedOut)
    $match = [regex]::Match($ResolvedOut,
        '(?i)^(C:\\tmp\\mysingerserver-node-tray-(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}))(?:\\.+)$')
    if (-not $match.Success) { throw 'OUT_FILE_NOT_IN_GUID_TEST_ROOT' }
    return $match.Groups[1].Value
}

function Get-RunIdFromTestRoot {
    param([string]$TestRoot)
    return (Split-Path -Leaf $TestRoot).Substring(
        'mysingerserver-node-tray-'.Length).Replace('-', '').ToLowerInvariant()
}

function Get-CurrentUserSidHash {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $bytes = [Text.Encoding]::UTF8.GetBytes($identity.User.Value)
    return [Convert]::ToHexString(
        [Security.Cryptography.SHA256]::HashData($bytes)
    ).ToLowerInvariant()
}

function Read-ResourceSamples {
    param([string]$Path)
    if ([string]::IsNullOrWhiteSpace($Path)) {
        throw 'SAMPLES_FILE_REQUIRED'
    }
    if (-not [IO.Path]::IsPathRooted($Path)) {
        throw 'SAMPLES_FILE_MUST_BE_ABSOLUTE'
    }
    $full = [IO.Path]::GetFullPath($Path)
    if (-not $Path.Equals($full, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'SAMPLES_FILE_MUST_BE_CANONICAL'
    }
    if (-not (Test-Path -LiteralPath $full -PathType Leaf)) {
        throw 'SAMPLES_FILE_MISSING'
    }
    if (Test-ReparsePointInExistingChain $full) {
        throw 'SAMPLES_FILE_REPARSE_POINT_REJECTED'
    }
    $samples = @(Get-Content -Raw -LiteralPath $full | ConvertFrom-Json -Depth 12)
    if ($samples.Count -lt 2) { throw 'RESOURCE_SAMPLES_INSUFFICIENT' }
    for ($index = 0; $index -lt $samples.Count; $index++) {
        $sample = $samples[$index]
        if ([double]$sample.elapsed_seconds -lt 0 -or
            [long]$sample.private_working_set_bytes -lt 0 -or
            [double]$sample.cpu_total_seconds -lt 0 -or
            [int]$sample.handles -lt 0) {
            throw 'RESOURCE_SAMPLE_VALUE_INVALID'
        }
        if ($index -gt 0 -and
            [double]$sample.elapsed_seconds -le
                [double]$samples[$index - 1].elapsed_seconds) {
            throw 'RESOURCE_SAMPLE_TIME_NOT_INCREASING'
        }
    }
    return $samples
}

function Get-ResourceStatus {
    param([object]$Summary)
    if ($Summary.memory_pass -and $Summary.cpu_pass) { return 'PASS' }
    return 'FAIL'
}

function Get-CombinedStatus {
    param([string]$ResourceStatus, [string]$ThroughputStatus)
    if ($ResourceStatus -eq 'FAIL' -or $ThroughputStatus -eq 'FAIL') {
        return 'FAIL'
    }
    if ($ResourceStatus -eq $script:Blocked -or
        $ThroughputStatus -eq $script:Blocked) {
        return $script:Blocked
    }
    if ($ResourceStatus -eq 'PASS' -and $ThroughputStatus -eq 'PASS') {
        return 'PASS'
    }
    throw 'COMBINED_STATUS_INPUT_INVALID'
}

function Read-ThroughputEvidence {
    param(
        [string]$Path,
        [string]$ResolvedOut,
        [string]$ResolvedExe,
        [switch]$RequireInsideTestRoot
    )
    if ([string]::IsNullOrWhiteSpace($Path)) {
        return [ordered]@{ status = $script:Blocked; file = $null }
    }
    if (-not [IO.Path]::IsPathRooted($Path)) {
        throw 'THROUGHPUT_EVIDENCE_MUST_BE_ABSOLUTE'
    }
    $full = [IO.Path]::GetFullPath($Path)
    if (-not $Path.Equals($full, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'THROUGHPUT_EVIDENCE_MUST_BE_CANONICAL'
    }
    if (-not (Test-Path -LiteralPath $full -PathType Leaf)) {
        throw 'THROUGHPUT_EVIDENCE_MISSING'
    }
    if (Test-ReparsePointInExistingChain $full) {
        throw 'THROUGHPUT_EVIDENCE_REPARSE_POINT_REJECTED'
    }
    $testRoot = Get-TestRootFromOutFile $ResolvedOut
    if ($RequireInsideTestRoot -and
        (-not (Test-SameOrBelow $full $testRoot) -or
            $full.TrimEnd('\').Equals(
                $testRoot, [StringComparison]::OrdinalIgnoreCase))) {
        throw 'THROUGHPUT_EVIDENCE_OUTSIDE_TEST_ROOT'
    }
    $document = Get-Content -Raw -LiteralPath $full |
        ConvertFrom-Json -Depth 12
    if ([int]$document.schema_version -ne 1) {
        throw 'THROUGHPUT_EVIDENCE_SCHEMA_UNSUPPORTED'
    }
    if ([string]$document.run_id -cne (Get-RunIdFromTestRoot $testRoot)) {
        throw 'THROUGHPUT_EVIDENCE_RUN_ID_MISMATCH'
    }
    if ([string]$document.test_root -cne $testRoot) {
        throw 'THROUGHPUT_EVIDENCE_TEST_ROOT_MISMATCH'
    }
    $stage = [IO.Path]::GetDirectoryName($ResolvedExe).TrimEnd('\')
    if ([string]$document.stage_dir -cne $stage) {
        throw 'THROUGHPUT_EVIDENCE_STAGE_DIR_MISMATCH'
    }
    if ([string]$document.current_user_sid_sha256 -cne
        (Get-CurrentUserSidHash)) {
        throw 'THROUGHPUT_EVIDENCE_USER_MISMATCH'
    }
    $baseline = [double]$document.baseline_items_per_second
    $withTray = [double]$document.with_tray_items_per_second
    $reportedRegression = [double]$document.regression_percent
    $maximumRegression = [double]$document.maximum_regression_percent
    if ($baseline -le 0 -or $withTray -lt 0 -or $maximumRegression -lt 0) {
        throw 'THROUGHPUT_EVIDENCE_RATE_INVALID'
    }
    $calculated = [math]::Max(0.0,
        (($baseline - $withTray) / $baseline) * 100.0)
    if ([math]::Abs($calculated - $reportedRegression) -gt 0.001) {
        throw 'THROUGHPUT_EVIDENCE_REGRESSION_MISMATCH'
    }
    $expectedStatus = if ($calculated -le $maximumRegression) {
        'PASS'
    } else { 'FAIL' }
    if ([string]$document.status -cne $expectedStatus) {
        throw 'THROUGHPUT_EVIDENCE_STATUS_MISMATCH'
    }
    $started = [datetimeoffset]::MinValue
    $ended = [datetimeoffset]::MinValue
    if (-not [datetimeoffset]::TryParse(
            [string]$document.started_utc, [ref]$started) -or
        -not [datetimeoffset]::TryParse(
            [string]$document.ended_utc, [ref]$ended) -or
        $ended -lt $started) {
        throw 'THROUGHPUT_EVIDENCE_TIME_INVALID'
    }
    if ([string]::IsNullOrWhiteSpace([string]$document.command)) {
        throw 'THROUGHPUT_EVIDENCE_COMMAND_REQUIRED'
    }
    if ([string]$document.credential_scan_status -cne 'PASS') {
        throw 'THROUGHPUT_EVIDENCE_CREDENTIAL_SCAN_NOT_PASS'
    }
    return [ordered]@{
        status = $expectedStatus
        file = $full
        baseline_items_per_second = $baseline
        with_tray_items_per_second = $withTray
        regression_percent = [math]::Round($calculated, 4)
        maximum_regression_percent = $maximumRegression
    }
}

$baseResult = [ordered]@{
    schema_version = 1
    status = $script:Blocked
    authorization = [ordered]@{
        process_control = [bool]$AllowProcessControl
    }
    requested = [ordered]@{
        warmup_seconds = $WarmupSec
        duration_seconds = $DurationSec
        sample_interval_seconds = 1
        node_tray_pid = $NodeTrayPid
    }
    thresholds = [ordered]@{
        private_working_set_mib_max = 256
        average_single_core_cpu_percent_max_exclusive = 1.0
    }
    raw_samples = @()
    side_effects_performed = $false
}

if ($ValidateResultOnly) {
    try {
        $resolvedExe = Get-ValidatedStageExecutable $NodeTrayExe
        $resolvedOut = Get-ValidatedOutFile $OutFile
        $offlineSamples = @(Read-ResourceSamples $SamplesFile)
        $offlineSummary = Get-ResourceSummary $offlineSamples
        $resourceStatus = Get-ResourceStatus $offlineSummary
        $throughput = Read-ThroughputEvidence `
            $ThroughputEvidenceFile $resolvedOut $resolvedExe
        $combinedStatus = Get-CombinedStatus `
            $resourceStatus ([string]$throughput.status)
        [ordered]@{
            schema_version = 1
            mode = 'resource-result-validation-only'
            validation_status = 'PASS'
            resource_status = $resourceStatus
            throughput_impact = [string]$throughput.status
            would_summarize_status = $combinedStatus
            dynamic_acceptance = $script:Blocked
            thresholds = $baseResult.thresholds
            summary = $offlineSummary
            samples_file = [IO.Path]::GetFullPath($SamplesFile)
            throughput_evidence_file = $throughput.file
            out_file_written = $false
            side_effects_performed = $false
        } | ConvertTo-Json -Depth 12
        exit 0
    } catch {
        [ordered]@{
            schema_version = 1
            mode = 'resource-result-validation-only'
            validation_status = 'FAIL'
            error_code = [string]$_.Exception.Message
            dynamic_acceptance = $script:Blocked
            out_file_written = $false
            side_effects_performed = $false
        } | ConvertTo-Json -Depth 8
        exit 1
    }
}

if (-not $AllowProcessControl) {
    $baseResult.blockers = @('AUTHORIZATION_MISSING switch=AllowProcessControl')
    $baseResult.out_file_written = $false
    $baseResult | ConvertTo-Json -Depth 10
    exit 2
}

try {
    $resolvedExe = Get-ValidatedStageExecutable $NodeTrayExe
    $resolvedOut = Get-ValidatedOutFile $OutFile
    if (-not (Test-Path -LiteralPath $resolvedExe -PathType Leaf)) {
        throw 'NODETRAY_EXE_MISSING'
    }
    if ($NodeTrayPid -le 0) {
        throw 'EXISTING_NODETRAY_PID_REQUIRED_NO_PROCESS_WILL_BE_STARTED'
    }
    $rootProcess = Get-Process -Id $NodeTrayPid -ErrorAction Stop
    $actualPath = Get-ProcessPath $rootProcess
    if ([string]::IsNullOrWhiteSpace($actualPath) -or
        -not $actualPath.Equals($resolvedExe, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'PROCESS_PATH_DOES_NOT_MATCH_STAGE_NODETRAY'
    }
    $startedUtc = $rootProcess.StartTime.ToUniversalTime()

    if ($WarmupSec -gt 0) { Start-Sleep -Seconds $WarmupSec }
    $timer = [Diagnostics.Stopwatch]::StartNew()
    $samples = [Collections.Generic.List[object]]::new()
    while ($timer.Elapsed.TotalSeconds -lt $DurationSec) {
        $current = Get-Process -Id $NodeTrayPid -ErrorAction Stop
        if ($current.StartTime.ToUniversalTime() -ne $startedUtc -or
            -not (Get-ProcessPath $current).Equals(
                $resolvedExe, [StringComparison]::OrdinalIgnoreCase)) {
            throw 'NODETRAY_PROCESS_IDENTITY_CHANGED'
        }
        $samples.Add((Get-ResourceSample $NodeTrayPid $startedUtc `
            $timer.Elapsed.TotalSeconds))
        Start-Sleep -Seconds 1
    }
    $summary = Get-ResourceSummary @($samples)
    $resourceStatus = Get-ResourceStatus $summary
    $throughput = Read-ThroughputEvidence `
        $ThroughputEvidenceFile $resolvedOut $resolvedExe -RequireInsideTestRoot
    $status = Get-CombinedStatus `
        $resourceStatus ([string]$throughput.status)
    $result = [ordered]@{
        schema_version = 1
        status = $status
        resource_status = $resourceStatus
        authorization = [ordered]@{ process_control = $true }
        target = [ordered]@{
            path = $resolvedExe
            pid = $NodeTrayPid
            started_utc = $startedUtc.ToString('o')
            process_started_by_script = $false
        }
        requested = $baseResult.requested
        thresholds = $baseResult.thresholds
        summary = $summary
        raw_samples = @($samples)
        throughput_impact = [string]$throughput.status
        throughput_evidence = $throughput
        side_effects_performed = $true
        side_effects = @('只读采样已由外部授权启动的独立 stage 进程',
            '写入隔离测试根内 JSON；脚本未启动、停止或结束任何进程')
    }
    $outParent = Split-Path -Parent $resolvedOut
    if (-not (Test-Path -LiteralPath $outParent -PathType Container)) {
        New-Item -ItemType Directory -Path $outParent | Out-Null
    }
    $result | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $resolvedOut -Encoding utf8
    $result.out_file = $resolvedOut
    $result | ConvertTo-Json -Depth 12
    if ($status -eq 'PASS') { exit 0 }
    if ($status -eq $script:Blocked) { exit 2 }
    exit 1
} catch {
    $baseResult.status = 'FAIL'
    $baseResult.blockers = @([string]$_.Exception.Message)
    $baseResult.out_file_written = $false
    $baseResult.side_effects_performed = $false
    $baseResult | ConvertTo-Json -Depth 10
    exit 1
}
