<#
.SYNOPSIS
把半小时真实媒体运行原始证据汇总为中文可审计报告。
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string] $EvidenceRoot,
    [string] $OutputPath = ''
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
if (-not $OutputPath) {
    $OutputPath = Join-Path $repositoryRoot 'docs\verification\2026-08-21-node-runtime-half-hour.md'
}

function Read-Ndjson {
    <# 逐行解析NDJSON；任何坏行都让证据不可用。 #>
    param([string] $Path)

    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        throw "RUST_V2_RUNTIME_EVIDENCE_MISSING path=$Path"
    }
    @(
        Get-Content -LiteralPath $Path |
            Where-Object { -not [string]::IsNullOrWhiteSpace($_) } |
            ForEach-Object { $_ | ConvertFrom-Json -Depth 30 }
    )
}

function Get-MaxGapSeconds {
    param([object[]] $Samples)

    $times = @($Samples | ForEach-Object { [double]$_.elapsed_seconds } | Sort-Object)
    $max = 0.0
    for ($index = 1; $index -lt $times.Count; $index++) {
        $max = [Math]::Max($max, $times[$index] - $times[$index - 1])
    }
    $max
}

function Format-Bytes {
    param([double] $Value)
    if ($Value -ge 1GB) { return ('{0:N2} GiB' -f ($Value / 1GB)) }
    if ($Value -ge 1MB) { return ('{0:N2} MiB' -f ($Value / 1MB)) }
    if ($Value -ge 1KB) { return ('{0:N2} KiB' -f ($Value / 1KB)) }
    '{0:N0} B' -f $Value
}

function Json-Equivalent {
    param($Left, $Right)
    (($Left | ConvertTo-Json -Depth 30 -Compress) -ceq ($Right | ConvertTo-Json -Depth 30 -Compress))
}

$runtimePath = Join-Path $EvidenceRoot 'runtime.ndjson'
$systemPath = Join-Path $EvidenceRoot 'system.ndjson'
$beforePath = Join-Path $EvidenceRoot 'media-before.json'
$afterPath = Join-Path $EvidenceRoot 'media-after.json'
$harnessPath = Join-Path $EvidenceRoot 'harness-result.json'

$runtimeRecords = Read-Ndjson -Path $runtimePath
$systemSamples = Read-Ndjson -Path $systemPath
$runtimeSamples = @($runtimeRecords | Where-Object record_type -eq 'runtime_sample')
$runtimeResult = @($runtimeRecords | Where-Object record_type -eq 'runtime_result') | Select-Object -Last 1
if (-not $runtimeResult -or $runtimeSamples.Count -eq 0 -or $systemSamples.Count -eq 0) {
    throw 'RUST_V2_RUNTIME_EVIDENCE_INCOMPLETE'
}
$before = Get-Content -LiteralPath $beforePath -Raw | ConvertFrom-Json -Depth 30
$after = Get-Content -LiteralPath $afterPath -Raw | ConvertFrom-Json -Depth 30
$harness = Get-Content -LiteralPath $harnessPath -Raw | ConvertFrom-Json -Depth 20

$duration = [int64]$runtimeResult.duration_seconds
$maxGap = Get-MaxGapSeconds -Samples $runtimeSamples
$machineIds = @($runtimeSamples.machine_id | Where-Object { $_ } | Sort-Object -Unique)
$taskIds = @($runtimeSamples.runtime_task_id | Where-Object { $_ } | Sort-Object -Unique)
$mediaUnchanged = (Json-Equivalent -Left $before -Right $after) -and [bool]$harness.MediaUnchanged
$failedScans = [int64]$runtimeResult.failed_scans
$unexpectedExit = [bool]$harness.NodeUnexpectedExit
$effectiveWorkers = [int]$harness.EffectiveWorkerCount

$workerSnapshots = @($runtimeSamples | ForEach-Object { @($_.workers) })
$workerCounts = @($runtimeSamples | ForEach-Object { @($_.workers).Count })
$peakWorkers = if ($workerCounts.Count) { [int](($workerCounts | Measure-Object -Maximum).Maximum) } else { 0 }
$averageWorkers = if ($workerCounts.Count) { [double](($workerCounts | Measure-Object -Average).Average) } else { 0 }
$allWorkerDisks = @($workerSnapshots.physical_disk_id | Where-Object { $_ } | Sort-Object -Unique)
$peakConcurrentDisks = 0
foreach ($sample in $runtimeSamples) {
    $count = @($sample.workers.physical_disk_id | Where-Object { $_ } | Sort-Object -Unique).Count
    $peakConcurrentDisks = [Math]::Max($peakConcurrentDisks, $count)
}

$failureRows = @($runtimeSamples | ForEach-Object { @($_.failures) })
$failureGroups = @($failureRows | Group-Object -Property stage_id, display_path, message | Sort-Object Count -Descending)
$repeatFailure = $failureGroups | Where-Object {
    $_.Count -gt [Math]::Max(20, [int]($runtimeSamples.Count / 2))
} | Select-Object -First 1
$physicalFaults = @($failureRows | Where-Object { $_.message -match '物理|timeout|超时|读取' }).Count
$workerCrashes = @($failureRows | Where-Object { $_.message -match 'Worker|崩溃|exit' }).Count

$failReasons = [Collections.Generic.List[string]]::new()
if ($duration -lt 1800) { $failReasons.Add("实际计算窗口仅 $duration 秒，少于1800秒") }
if ($maxGap -gt 6) { $failReasons.Add("采样最大间隔为 $maxGap 秒，超过6秒") }
if (-not $mediaUnchanged) { $failReasons.Add('真实媒体清单发生变化') }
if ($unexpectedExit) { $failReasons.Add('Node或Worker发生非预期退出') }
if ($failedScans -gt 0 -or @($runtimeSamples | Where-Object state -eq 'failed').Count -gt 0) {
    $failReasons.Add("发生任务级失败，failed_scans=$failedScans")
}
if ($workerCrashes -gt 0) {
    $failReasons.Add("观察到 Worker 崩溃样本：$workerCrashes")
}
if ($effectiveWorkers -gt 1 -and $runtimeSamples.Count -ge 30 -and $peakWorkers -lt 2) {
    $failReasons.Add("有效Worker为 $effectiveWorkers，但峰值在途Worker仅 $peakWorkers")
}
if ($allWorkerDisks.Count -gt 1 -and $peakConcurrentDisks -lt 2) {
    $failReasons.Add('多个物理盘均有工作，但从未观察到重叠读取')
}
if ($repeatFailure) {
    $failReasons.Add("同一运行任务/文件失败疑似无限重复：$($repeatFailure.Name) × $($repeatFailure.Count)")
}
$verdict = if ($failReasons.Count -eq 0) { 'PASS' } else { 'FAIL' }

$stageLines = [Collections.Generic.List[string]]::new()
$stageRows = @($runtimeSamples | ForEach-Object { @($_.stages) })
foreach ($group in $stageRows | Group-Object stage_id | Sort-Object Name) {
    $last = $group.Group | Sort-Object elapsed_ms | Select-Object -Last 1
    $maxSpeed = [double](($group.Group.speed_per_second | Measure-Object -Maximum).Maximum ?? 0)
    $stageLines.Add("| $($last.display_name) ($($last.stage_id)) | $($last.completed) / $($last.total) | $($last.elapsed_ms) | $('{0:N2}' -f $maxSpeed) | $($last.failed) |")
}

$processLines = [Collections.Generic.List[string]]::new()
$processRows = @($systemSamples | ForEach-Object { @($_.processes) })
foreach ($group in $processRows | Group-Object Name | Sort-Object Name) {
    $cpuAverage = [double](($group.Group.CpuDeltaMs | Measure-Object -Average).Average ?? 0)
    $workingPeak = [double](($group.Group.WorkingSetBytes | Measure-Object -Maximum).Maximum ?? 0)
    $privatePeak = [double](($group.Group.PrivateMemoryBytes | Measure-Object -Maximum).Maximum ?? 0)
    $processLines.Add("| $($group.Name) | $('{0:N2}' -f $cpuAverage) | $(Format-Bytes $workingPeak) | $(Format-Bytes $privatePeak) |")
}

$diskLines = [Collections.Generic.List[string]]::new()
$diskRows = @($systemSamples | ForEach-Object { @($_.disks) })
foreach ($group in $diskRows | Group-Object Name | Sort-Object Name) {
    $readAverage = [double](($group.Group.DiskReadBytesPerSec | Measure-Object -Average).Average ?? 0)
    $readPeak = [double](($group.Group.DiskReadBytesPerSec | Measure-Object -Maximum).Maximum ?? 0)
    $queuePeak = [double](($group.Group.AvgDiskQueueLength | Measure-Object -Maximum).Maximum ?? 0)
    $diskLines.Add("| $($group.Name) | $(Format-Bytes $readAverage)/s | $(Format-Bytes $readPeak)/s | $('{0:N2}' -f $queuePeak) |")
}

$recentFailureLines = if ($failureGroups.Count -eq 0) {
    '- 本次未记录文件级失败。'
}
else {
    @($failureGroups | Select-Object -First 20 | ForEach-Object { "- $($_.Name)（$($_.Count) 次）" }) -join "`n"
}
$failureReasonLines = if ($failReasons.Count -eq 0) {
    '- 无自动化失败条件。'
}
else {
    @($failReasons | ForEach-Object { "- $_" }) -join "`n"
}
$cleanupCount = [int64]$harness.DiskFullCleanupCount
$cleanupText = if ($cleanupCount -eq 0) {
    '本次未触发，不能从本次实测证明清理路径。'
}
else {
    "本次触发 $cleanupCount 次；原始日志需结合删除清单审计。"
}

$report = @"
# Rust V2 Node 真实媒体半小时运行验收

结论：$verdict

## 自动化门禁

- 实际计算窗口：$duration 秒
- 运行样本：$($runtimeSamples.Count) 条；系统样本：$($systemSamples.Count) 条
- 最大采样间隔：$maxGap 秒
- 机器 ID：$($machineIds -join '、')
- 运行任务 ID：$($taskIds -join '、')
- 有效 Worker 配置：$effectiveWorkers；峰值：$peakWorkers；平均在途：$('{0:N2}' -f $averageWorkers)

$failureReasonLines

## Node 配置摘要

- 枚举器：Everything（不可用时由 Node 回退 Windows Walker）
- 单块读取超时：3 秒；重试：2 次；块大小：4 MiB
- 读取并发：HDD 1/盘、SSD 2/盘、未知盘 1/盘、总计 4
- 配置与运行数据位于本次隔离目录，重启后运行详情不持久化。

## 总文件与字节

- 源媒体文件：$($before.FileCount)
- 源媒体字节：$(Format-Bytes ([double]$before.TotalBytes))
- 运行任务最终计数：$($runtimeSamples[-1].overall_completed) / $($runtimeSamples[-1].overall_total)

## 各阶段耗时与吞吐

| 阶段 | 完成/总计 | 已运行毫秒 | 峰值速度/秒 | 失败 |
| --- | ---: | ---: | ---: | ---: |
$($stageLines -join "`n")

## Worker 并行

- 峰值在途 Worker：$peakWorkers
- 平均在途 Worker：$('{0:N2}' -f $averageWorkers)
- 观察到的物理盘：$($allWorkerDisks -join '、')
- 同时工作的物理盘峰值：$peakConcurrentDisks

## Node/Worker CPU 与内存

| 进程 | 平均每 tick CPU 毫秒 | Working Set 峰值 | Private 峰值 |
| --- | ---: | ---: | ---: |
$($processLines -join "`n")

## 物理磁盘读取

| 物理盘实例 | 平均读吞吐 | 峰值读吞吐 | 队列峰值 |
| --- | ---: | ---: | ---: |
$($diskLines -join "`n")

## 最近失败

$recentFailureLines

## 文件故障分类

- 疑似物理读取故障样本：$physicalFaults
- Worker 崩溃样本：$workerCrashes

## 联系表复用

- 本次记录的 MD5 联系表复用数：$($harness.ContactSheetReuseCount)

## 磁盘满清理

- 触发次数：$cleanupCount
- $cleanupText

## 真实媒体未修改证明

- 验收前：$($before.FileCount) 个文件，$(Format-Bytes ([double]$before.TotalBytes))
- 验收后：$($after.FileCount) 个文件，$(Format-Bytes ([double]$after.TotalBytes))
- 路径、长度、LastWriteTimeUtc 逐项一致：$mediaUnchanged

## 实测与未触发边界

- 自动化门禁来自 runtime/system NDJSON 与前后媒体清单。
- OS 采样只说明本轮实际观察到的 CPU、内存和物理盘吞吐。
- 没有发生的故障、磁盘满或崩溃路径不会被写成“已通过实测”。
- 原始证据目录：$EvidenceRoot
"@

$parent = Split-Path -Parent $OutputPath
if ($parent) { New-Item -ItemType Directory -Path $parent -Force | Out-Null }
[IO.File]::WriteAllText($OutputPath, $report, [Text.UTF8Encoding]::new($false))
Write-Output "RUST_V2_RUNTIME_ACCEPTANCE_REPORT_$verdict"
Write-Output "REPORT_PATH=$OutputPath"
