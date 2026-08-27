[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string]$Root,

    [Parameter(Mandatory)]
    [ValidateSet('Baseline', 'Adaptive')]
    [string]$Mode,

    [Parameter(Mandatory)]
    [string]$OutputDir,

    [string]$RunnerPath = $env:MYSINGER_SCAN_BENCHMARK_RUNNER,

    [uint32]$FieldsMask = 2043,

    [ValidateRange(1, 1024)]
    [int]$Workers = [Math]::Max(1, [Environment]::ProcessorCount),

    [ValidateRange(0, [long]::MaxValue)]
    [long]$MinimumDFreeBytes = 5GB,

    [string]$BaselineSummaryPath = '',

    [Parameter(DontShow)]
    [switch]$AllowFixtureRoot
)

$ErrorActionPreference = 'Stop'
$approvedRoot = 'I:\MiddleDir\11111111'
$repo = Split-Path -Parent $PSScriptRoot
$output = [IO.Path]::GetFullPath(
    $(if ([IO.Path]::IsPathRooted($OutputDir)) { $OutputDir } else { Join-Path $repo $OutputDir }))
$summaryPath = Join-Path $output 'benchmark-summary.json'

function Write-BenchmarkJson {
    param([string]$Path, [object]$Value)
    $parent = Split-Path -Parent $Path
    New-Item -ItemType Directory -Force -Path $parent | Out-Null
    [IO.File]::WriteAllText(
        $Path,
        (($Value | ConvertTo-Json -Depth 16) + [Environment]::NewLine),
        [Text.UTF8Encoding]::new($false))
}

function Get-ObjectProperty {
    param([object]$Value, [string]$Name)
    if ($null -eq $Value) { return $null }
    $property = $Value.PSObject.Properties[$Name]
    if ($null -eq $property) { return $null }
    return $property.Value
}

function Get-CanonicalResult {
    param([object]$Result)
    $files = @(
        @(Get-ObjectProperty -Value $Result -Name 'files') |
            ForEach-Object {
                [ordered]@{
                    path = [string](Get-ObjectProperty -Value $_ -Name 'path')
                    sha256 = ([string](Get-ObjectProperty -Value $_ -Name 'sha256')).ToLowerInvariant()
                    image_feature = [string](Get-ObjectProperty -Value $_ -Name 'image_feature')
                    six_frame_feature = [string](Get-ObjectProperty -Value $_ -Name 'six_frame_feature')
                }
            } |
            Sort-Object path
    )
    $failures = @(
        @(Get-ObjectProperty -Value $Result -Name 'failures') |
            ForEach-Object {
                if ($_ -is [string]) { [string]$_ } else { $_ | ConvertTo-Json -Depth 8 -Compress }
            } |
            Sort-Object
    )
    return [ordered]@{ files = $files; failures = $failures }
}

function Get-JsonSha256 {
    param([object]$Value)
    $json = $Value | ConvertTo-Json -Depth 16 -Compress
    $bytes = [Text.Encoding]::UTF8.GetBytes($json)
    $hash = [Security.Cryptography.SHA256]::HashData($bytes)
    return [Convert]::ToHexString($hash).ToLowerInvariant()
}

function Invoke-BenchmarkRunner {
    param([string]$Executable, [string[]]$Arguments, [string]$StdoutPath, [string]$StderrPath)
    $startInfo = [Diagnostics.ProcessStartInfo]::new()
    if ([IO.Path]::GetExtension($Executable) -ieq '.ps1') {
        $startInfo.FileName = Join-Path $PSHOME 'pwsh.exe'
        $startInfo.ArgumentList.Add('-NoProfile')
        $startInfo.ArgumentList.Add('-File')
        $startInfo.ArgumentList.Add($Executable)
    } else {
        $startInfo.FileName = $Executable
    }
    foreach ($argument in $Arguments) { $startInfo.ArgumentList.Add([string]$argument) }
    $startInfo.UseShellExecute = $false
    $startInfo.CreateNoWindow = $true
    $startInfo.RedirectStandardOutput = $true
    $startInfo.RedirectStandardError = $true
    $process = [Diagnostics.Process]::new()
    $process.StartInfo = $startInfo
    if (-not $process.Start()) { throw 'BENCHMARK_RUNNER_START_FAILED' }
    $stdout = $process.StandardOutput.ReadToEndAsync()
    $stderr = $process.StandardError.ReadToEndAsync()
    $process.WaitForExit()
    [IO.File]::WriteAllText($StdoutPath, $stdout.GetAwaiter().GetResult(), [Text.UTF8Encoding]::new($false))
    [IO.File]::WriteAllText($StderrPath, $stderr.GetAwaiter().GetResult(), [Text.UTF8Encoding]::new($false))
    return $process.ExitCode
}

$canonicalRoot = [IO.Path]::GetFullPath($Root).TrimEnd('\')
if (-not $AllowFixtureRoot -and
    -not [string]::Equals($canonicalRoot, $approvedRoot, [StringComparison]::OrdinalIgnoreCase)) {
    throw "BENCHMARK_ROOT_NOT_APPROVED expected=$approvedRoot actual=$canonicalRoot"
}

New-Item -ItemType Directory -Force -Path $output | Out-Null
$revision = (& git -C $repo rev-parse HEAD 2>$null | Out-String).Trim().ToLowerInvariant()
if ($LASTEXITCODE -ne 0 -or $revision -notmatch '^[0-9a-f]{40}$') {
    throw 'BENCHMARK_BUILD_SHA_UNAVAILABLE'
}

$driveD = [IO.DriveInfo]::new('D')
$freeBytes = if ($driveD.IsReady) { [long]$driveD.AvailableFreeSpace } else { 0L }
$config = [ordered]@{
    root = $canonicalRoot
    mode = $Mode
    fields_mask = $FieldsMask
    workers = $Workers
    minimum_d_free_bytes = $MinimumDFreeBytes
    observed_d_free_bytes = $freeBytes
}
if ($freeBytes -lt $MinimumDFreeBytes) {
    $blocked = [ordered]@{
        schema_version = 1
        status = 'BLOCKED'
        correctness_status = 'BLOCKED'
        performance_status = 'BLOCKED'
        reason = 'D_DRIVE_SPACE_INSUFFICIENT'
        build_sha = $revision
        config = $config
        started_utc = $null
        finished_utc = $null
        wall_clock_ms = $null
        disk_trace = @()
        lifecycle = $null
        resources = @()
        result_summary = $null
        result_manifest = $null
    }
    Write-BenchmarkJson -Path $summaryPath -Value $blocked
    Write-Output "BENCHMARK BLOCKED reason=D_DRIVE_SPACE_INSUFFICIENT free=$freeBytes required=$MinimumDFreeBytes summary=$summaryPath"
    return
}

if (-not (Test-Path -LiteralPath $canonicalRoot -PathType Container)) {
    throw "BENCHMARK_ROOT_NOT_FOUND path=$canonicalRoot"
}
if (-not $RunnerPath) {
    $missingRunner = [ordered]@{
        schema_version = 1
        status = 'BLOCKED'
        correctness_status = 'BLOCKED'
        performance_status = 'BLOCKED'
        reason = 'BENCHMARK_RUNNER_NOT_CONFIGURED'
        build_sha = $revision
        config = $config
        started_utc = $null
        finished_utc = $null
        wall_clock_ms = $null
        disk_trace = @()
        lifecycle = $null
        resources = @()
        result_summary = $null
        result_manifest = $null
    }
    Write-BenchmarkJson -Path $summaryPath -Value $missingRunner
    Write-Output "BENCHMARK BLOCKED reason=BENCHMARK_RUNNER_NOT_CONFIGURED summary=$summaryPath"
    return
}
$runner = (Resolve-Path -LiteralPath $RunnerPath).Path
$runnerHash = (Get-FileHash -LiteralPath $runner -Algorithm SHA256).Hash.ToLowerInvariant()

$resultPath = Join-Path $output 'scan-result.json'
$tracePath = Join-Path $output 'disk-trace.json'
$lifecyclePath = Join-Path $output 'lifecycle.json'
$resourcePath = Join-Path $output 'resources.json'
$stdoutPath = Join-Path $output 'runner.stdout.log'
$stderrPath = Join-Path $output 'runner.stderr.log'
$arguments = @(
    '-Root', $canonicalRoot,
    '-Mode', $Mode,
    '-FieldsMask', [string]$FieldsMask,
    '-Workers', [string]$Workers,
    '-OutputPath', $resultPath,
    '-TracePath', $tracePath,
    '-LifecyclePath', $lifecyclePath,
    '-ResourcePath', $resourcePath
)
$started = [DateTime]::UtcNow
$exitCode = -1
$runnerError = $null
try {
    $exitCode = Invoke-BenchmarkRunner -Executable $runner -Arguments $arguments `
        -StdoutPath $stdoutPath -StderrPath $stderrPath
} catch {
    $runnerError = $_.Exception.Message
}
$finished = [DateTime]::UtcNow
$wallClockMS = [int64]($finished - $started).TotalMilliseconds

$status = 'FAIL'
$correctnessStatus = 'FAIL'
$performanceStatus = 'BLOCKED'
$reason = $null
$canonical = $null
$diskTrace = @()
$lifecycle = $null
$resources = @()
$resultSummary = $null
$performance = $null

if ($null -ne $runnerError -or $exitCode -ne 0) {
    $reason = if ($runnerError) { "BENCHMARK_RUNNER_EXCEPTION: $runnerError" } else { "BENCHMARK_RUNNER_EXIT_$exitCode" }
} elseif (-not (Test-Path -LiteralPath $resultPath -PathType Leaf) -or
        -not (Test-Path -LiteralPath $tracePath -PathType Leaf) -or
        -not (Test-Path -LiteralPath $lifecyclePath -PathType Leaf)) {
    $reason = 'BENCHMARK_RUNNER_EVIDENCE_INCOMPLETE'
} else {
    try {
        $rawResult = Get-Content -Raw -LiteralPath $resultPath | ConvertFrom-Json
        if ([string](Get-ObjectProperty $rawResult 'root') -cne $canonicalRoot -or
            [string](Get-ObjectProperty $rawResult 'mode') -cne $Mode -or
            [uint32](Get-ObjectProperty $rawResult 'fields_mask') -ne $FieldsMask -or
            [int](Get-ObjectProperty $rawResult 'workers') -ne $Workers) {
            throw 'BENCHMARK_RUNNER_CONFIG_MISMATCH'
        }
        $canonical = Get-CanonicalResult -Result $rawResult
        $diskTrace = @(Get-Content -Raw -LiteralPath $tracePath | ConvertFrom-Json)
        $lifecycle = Get-Content -Raw -LiteralPath $lifecyclePath | ConvertFrom-Json
        if (Test-Path -LiteralPath $resourcePath -PathType Leaf) {
            $resources = @(Get-Content -Raw -LiteralPath $resourcePath | ConvertFrom-Json)
        }
        if ($diskTrace.Count -eq 0) { throw 'BENCHMARK_DISK_TRACE_EMPTY' }
        foreach ($operation in @('pause', 'resume', 'stop')) {
            if ([string](Get-ObjectProperty $lifecycle $operation) -cne 'passed') {
                throw "BENCHMARK_LIFECYCLE_NOT_PASSED operation=$operation"
            }
        }
        if (-not [bool](Get-ObjectProperty $lifecycle 'inflight_drained') -or
            -not [bool](Get-ObjectProperty $lifecycle 'progress_not_ahead')) {
            throw 'BENCHMARK_LIFECYCLE_DRAIN_INVALID'
        }
        $resultDigest = Get-JsonSha256 -Value $canonical
        $resultSummary = [ordered]@{
            file_count = @($canonical.files).Count
            failure_count = @($canonical.failures).Count
            result_sha256 = $resultDigest
        }
        if ($resultSummary.failure_count -ne 0) {
            $reason = 'BENCHMARK_RESULT_CONTAINS_FAILURES'
        } elseif ($Mode -eq 'Baseline') {
            $status = 'PASS'
            $correctnessStatus = 'PASS'
            $performanceStatus = 'NOT_APPLICABLE'
        } else {
            if (-not $BaselineSummaryPath) {
                $BaselineSummaryPath = Join-Path (Split-Path -Parent $output) 'io-baseline\benchmark-summary.json'
            }
            $baselinePath = [IO.Path]::GetFullPath($BaselineSummaryPath)
            if (-not (Test-Path -LiteralPath $baselinePath -PathType Leaf)) {
                throw "BENCHMARK_BASELINE_SUMMARY_NOT_FOUND path=$baselinePath"
            }
            $baseline = Get-Content -Raw -LiteralPath $baselinePath | ConvertFrom-Json
            if ([string]$baseline.status -cne 'PASS' -or [string]$baseline.correctness_status -cne 'PASS') {
                throw 'BENCHMARK_BASELINE_NOT_PASSED'
            }
            if ([uint32]$baseline.config.fields_mask -ne $FieldsMask -or
                [int]$baseline.config.workers -ne $Workers -or
                [string]$baseline.config.root -cne $canonicalRoot) {
                throw 'BENCHMARK_BASELINE_CONFIG_MISMATCH'
            }
            if ([string]$baseline.result_summary.result_sha256 -cne $resultDigest) {
                throw 'BENCHMARK_RESULT_SET_MISMATCH'
            }
            $baselineMS = [double]$baseline.wall_clock_ms
            if ($baselineMS -le 0) { throw 'BENCHMARK_BASELINE_WALL_CLOCK_INVALID' }
            $improvementPercent = (($baselineMS - $wallClockMS) / $baselineMS) * 100.0
            $regressionPercent = (($wallClockMS - $baselineMS) / $baselineMS) * 100.0
            $performance = [ordered]@{
                baseline_wall_clock_ms = [int64]$baselineMS
                adaptive_wall_clock_ms = $wallClockMS
                improvement_percent = [Math]::Round($improvementPercent, 3)
                regression_percent = [Math]::Round($regressionPercent, 3)
                regression_limit_percent = 3.0
                target_improvement_percent = 20.0
            }
            $correctnessStatus = 'PASS'
            if ($regressionPercent -gt 3.0) {
                $performanceStatus = 'FAIL'
                $reason = 'BENCHMARK_PERFORMANCE_REGRESSION'
            } elseif ($improvementPercent -ge 20.0) {
                $performanceStatus = 'PASS'
                $status = 'PASS'
            } else {
                $performanceStatus = 'TARGET_NOT_MET'
                $status = 'PASS'
            }
        }
    } catch {
        $reason = $_.Exception.Message
        $status = 'FAIL'
        $correctnessStatus = 'FAIL'
        $performanceStatus = 'BLOCKED'
    }
}

$summary = [ordered]@{
    schema_version = 1
    status = $status
    correctness_status = $correctnessStatus
    performance_status = $performanceStatus
    reason = $reason
    build_sha = $revision
    runner_path = $runner
    runner_sha256 = $runnerHash
    config = $config
    started_utc = $started.ToString('o')
    finished_utc = $finished.ToString('o')
    wall_clock_ms = $wallClockMS
    runner_exit_code = $exitCode
    disk_trace = $diskTrace
    lifecycle = $lifecycle
    resources = $resources
    result_summary = $resultSummary
    result_manifest = $canonical
    performance = $performance
}
Write-BenchmarkJson -Path $summaryPath -Value $summary

if ($status -ne 'PASS') {
    throw "BENCHMARK_FAILED reason=$reason summary=$summaryPath"
}
Write-Output "BENCHMARK $status mode=$Mode correctness=$correctnessStatus performance=$performanceStatus summary=$summaryPath"
