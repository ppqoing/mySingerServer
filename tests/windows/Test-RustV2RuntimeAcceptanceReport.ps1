<#
.SYNOPSIS
使用901个两秒合成样本验证半小时中文报告和失败条件。
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$reporter = Join-Path $repositoryRoot 'tests\windows\New-RustV2RuntimeAcceptanceReport.ps1'
$fixtureRoot = Join-Path ([IO.Path]::GetTempPath()) ("rust-v2-runtime-report-" + [Guid]::NewGuid().ToString('N'))

function Write-Fixture {
    param([string] $Root, [int] $DurationSeconds, [switch] $MediaChanged)

    New-Item -ItemType Directory -Path $Root -Force | Out-Null
    $runtimePath = Join-Path $Root 'runtime.ndjson'
    $systemPath = Join-Path $Root 'system.ndjson'
    $machine = 'a' * 64
    for ($elapsed = 0; $elapsed -le $DurationSeconds; $elapsed += 2) {
        $runtime = [ordered]@{
            record_type = 'runtime_sample'
            utc_unix_ms = 1787356800000 + ($elapsed * 1000)
            elapsed_seconds = $elapsed
            runtime_task_id = 'runtime-fixture'
            machine_id = $machine
            state = if ($elapsed -eq $DurationSeconds) { 'cancelled' } else { 'running' }
            overall_completed = [Math]::Min(900, [int]($elapsed / 2))
            overall_total = 900
            overall_total_known = $true
            overall_failed = 0
            overall_skipped = 0
            stale = $false
            stages = @(
                [ordered]@{
                    stage_id = 'read_md5'; display_name = '读取与 MD5'; state = 2; unit = 'bytes'
                    completed = $elapsed * 1048576; total = 1887436800; total_known = $true
                    failed = 0; skipped = 0; speed_per_second = 524288; elapsed_ms = $elapsed * 1000; eta_ms = 0
                }
            )
            workers = @(
                [ordered]@{ slot = 0; process_id = 1001; stage_id = 'probe_stage1'; display_path = 'D:\Media\a.mp4'; physical_disk_id = 'PhysicalDrive0'; completed_files = [int]($elapsed / 4); speed_per_second = 0.5 },
                [ordered]@{ slot = 1; process_id = 1002; stage_id = 'probe_stage1'; display_path = 'E:\Media\b.mp4'; physical_disk_id = 'PhysicalDrive1'; completed_files = [int]($elapsed / 4); speed_per_second = 0.5 }
            )
            failures = @()
        }
        Add-Content -LiteralPath $runtimePath -Value ($runtime | ConvertTo-Json -Depth 10 -Compress) -Encoding utf8

        $system = [ordered]@{
            record_type = 'system_sample'
            utc = ([DateTime]'2026-08-22T00:00:00Z').AddSeconds($elapsed).ToString('O')
            elapsed_seconds = $elapsed
            processes = @(
                [ordered]@{ Name = 'node'; ProcessId = 900; CpuDeltaMs = 40; WorkingSetBytes = 134217728; PrivateMemoryBytes = 100663296 },
                [ordered]@{ Name = 'worker'; ProcessId = 1001; CpuDeltaMs = 500; WorkingSetBytes = 268435456; PrivateMemoryBytes = 234881024 },
                [ordered]@{ Name = 'worker'; ProcessId = 1002; CpuDeltaMs = 450; WorkingSetBytes = 251658240; PrivateMemoryBytes = 218103808 }
            )
            disks = @(
                [ordered]@{ Name = '0 C:'; DiskReadBytesPerSec = 1048576; AvgDiskQueueLength = 0.4 },
                [ordered]@{ Name = '1 D:'; DiskReadBytesPerSec = 2097152; AvgDiskQueueLength = 0.8 }
            )
        }
        Add-Content -LiteralPath $systemPath -Value ($system | ConvertTo-Json -Depth 8 -Compress) -Encoding utf8
    }
    Add-Content -LiteralPath $runtimePath -Value (([ordered]@{
        record_type = 'runtime_result'; duration_seconds = $DurationSeconds
        sample_count = [int]($DurationSeconds / 2); scans_started = 1; failed_scans = 0
        cancelled_at_deadline = $true
    }) | ConvertTo-Json -Compress) -Encoding utf8

    $before = [ordered]@{
        Root = 'D:\Media'; FileCount = 2; TotalBytes = 300
        Files = @(
            [ordered]@{ Path = 'a.mp4'; Length = 100; LastWriteTimeUtc = '2026-08-22T00:00:00.0000000Z' },
            [ordered]@{ Path = 'b.mp4'; Length = 200; LastWriteTimeUtc = '2026-08-22T00:00:00.0000000Z' }
        )
    }
    $after = $before | ConvertTo-Json -Depth 8 | ConvertFrom-Json -Depth 8
    if ($MediaChanged) {
        $after.TotalBytes = 301
        $after.Files = @(
            [ordered]@{ Path = 'a.mp4'; Length = 101; LastWriteTimeUtc = '2026-08-22T00:00:01.0000000Z' },
            $before.Files[1]
        )
    }
    $before | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath (Join-Path $Root 'media-before.json') -Encoding utf8
    $after | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath (Join-Path $Root 'media-after.json') -Encoding utf8
    [ordered]@{
        RunRoot = (Split-Path -Parent $Root)
        EvidenceRoot = $Root
        DurationSeconds = $DurationSeconds
        MediaUnchanged = (-not $MediaChanged)
        EffectiveWorkerCount = 2
        NodeUnexpectedExit = $false
        ContactSheetReuseCount = 3
        DiskFullCleanupCount = 0
    } | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $Root 'harness-result.json') -Encoding utf8
}

try {
    if (-not (Test-Path -LiteralPath $reporter -PathType Leaf)) {
        throw "RUST_V2_RUNTIME_ACCEPTANCE_REPORTER_MISSING path=$reporter"
    }
    $passRoot = Join-Path $fixtureRoot 'pass'
    Write-Fixture -Root $passRoot -DurationSeconds 1800
    $passReport = Join-Path $passRoot 'report.md'
    & $reporter -EvidenceRoot $passRoot -OutputPath $passReport | Out-Null
    $text = Get-Content -LiteralPath $passReport -Raw
    foreach ($required in @(
        '结论：PASS', '实际计算窗口', '机器 ID', '各阶段耗时与吞吐',
        'Worker 并行', 'Node/Worker CPU 与内存', '物理磁盘读取', '最近失败',
        '文件故障分类', '联系表复用', '磁盘满清理', '真实媒体未修改证明',
        '本次未触发，不能从本次实测证明清理路径')) {
        if ($text -notmatch [regex]::Escape($required)) {
            throw "PASS报告缺少字段：$required"
        }
    }

    $shortRoot = Join-Path $fixtureRoot 'short'
    Write-Fixture -Root $shortRoot -DurationSeconds 1798
    $shortReport = Join-Path $shortRoot 'report.md'
    & $reporter -EvidenceRoot $shortRoot -OutputPath $shortReport | Out-Null
    if ((Get-Content -LiteralPath $shortReport -Raw) -notmatch '结论：FAIL') {
        throw '少于1800秒必须输出FAIL'
    }

    $changedRoot = Join-Path $fixtureRoot 'changed'
    Write-Fixture -Root $changedRoot -DurationSeconds 1800 -MediaChanged
    $changedReport = Join-Path $changedRoot 'report.md'
    & $reporter -EvidenceRoot $changedRoot -OutputPath $changedReport | Out-Null
    if ((Get-Content -LiteralPath $changedReport -Raw) -notmatch '真实媒体清单发生变化') {
        throw '媒体变化必须进入FAIL原因'
    }

    Write-Output 'RUST_V2_RUNTIME_ACCEPTANCE_REPORT_PASS'
}
finally {
    if (Test-Path -LiteralPath $fixtureRoot) {
        Remove-Item -LiteralPath $fixtureRoot -Recurse -Force
    }
}
