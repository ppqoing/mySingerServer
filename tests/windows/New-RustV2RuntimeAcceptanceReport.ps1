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

if ([string]::IsNullOrWhiteSpace($EvidenceRoot)) {
    throw 'RUST_V2_RUNTIME_EVIDENCE_ROOT_INVALID'
}
$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$evidenceAbsolute = [IO.Path]::GetFullPath($EvidenceRoot).TrimEnd('\')
if (-not $OutputPath) {
    $OutputPath = Join-Path $EvidenceRoot 'report.md'
}
$outputAbsolute = [IO.Path]::GetFullPath($OutputPath).TrimEnd('\')
if ($outputAbsolute.Equals($evidenceAbsolute, [StringComparison]::OrdinalIgnoreCase) -or
    -not $outputAbsolute.StartsWith(($evidenceAbsolute + '\'), [StringComparison]::OrdinalIgnoreCase)) {
    throw 'RUST_V2_RUNTIME_REPORT_OUTSIDE_EVIDENCE'
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
            ForEach-Object { $_ | ConvertFrom-Json }
    )
}

function Get-NumberOrZero {
    <# 把空聚合值转换为零，兼容没有空合并运算符的 Windows PowerShell 5.1。 #>
    param($Value)

    if ($null -eq $Value) {
        return 0
    }
    $Value
}

function Get-OptionalProperty {
    <# 安全读取旧证据可能缺失的字段；缺失值保持为空，不用默认值冒充实测。 #>
    param($Object, [string] $Name)

    if ($null -eq $Object) { return $null }
    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property) { return $null }
    $property.Value
}

function Get-WorkerDiskIds {
    <# 从可空或空集合的Worker快照中提取非空物理盘，并返回去重结果。 #>
    param([object[]] $Workers)

    @(
        $Workers | ForEach-Object {
            $diskId = Get-OptionalProperty -Object $_ -Name 'physical_disk_id'
            if (-not [string]::IsNullOrWhiteSpace([string]$diskId)) {
                [string]$diskId
            }
        } | Sort-Object -Unique
    )
}

function Get-NormalizedWindowsEvidencePath {
    <# 规范化 Windows 路径用于大小写不敏感比较；失败时返回空值而不放宽证据门禁。 #>
    param([string] $Path)

    if ([string]::IsNullOrWhiteSpace($Path)) { return '' }
    try {
        [IO.Path]::GetFullPath($Path).TrimEnd('\')
    }
    catch {
        ''
    }
}

function Test-WindowsEvidencePathWithinRoot {
    <# 判断 Worker display_path 是否位于指定媒体根内，根边界后必须是分隔符。 #>
    param(
        [string] $Candidate,
        [string] $Root
    )

    $candidatePath = Get-NormalizedWindowsEvidencePath -Path $Candidate
    $rootPath = Get-NormalizedWindowsEvidencePath -Path $Root
    if ([string]::IsNullOrWhiteSpace($candidatePath) -or [string]::IsNullOrWhiteSpace($rootPath)) {
        return $false
    }
    $candidatePath.Equals($rootPath, [StringComparison]::OrdinalIgnoreCase) -or
        $candidatePath.StartsWith(($rootPath + '\'), [StringComparison]::OrdinalIgnoreCase)
}

function Test-WindowsEvidencePathEqual {
    <# 按规范化 Windows 路径做大小写不敏感相等比较，供多根证据逐项闭合。 #>
    param(
        [string] $Left,
        [string] $Right
    )

    $leftPath = Get-NormalizedWindowsEvidencePath -Path $Left
    $rightPath = Get-NormalizedWindowsEvidencePath -Path $Right
    -not [string]::IsNullOrWhiteSpace($leftPath) -and
        $leftPath.Equals($rightPath, [StringComparison]::OrdinalIgnoreCase)
}

function Get-ActiveWorkers {
    <# 排除显式idle Worker；旧证据缺少phase时保留为活动行，兼容旧协议口径。 #>
    param([object[]] $Workers)

    @(
        $Workers | Where-Object {
            if ($null -eq $_) { return $false }
            $phase = Get-OptionalProperty -Object $_ -Name 'phase'
            [string]::IsNullOrWhiteSpace([string]$phase) -or [string]$phase -ne 'idle'
        }
    )
}

function Format-Optional {
    <# 报告中把未采集值统一显示为破折号。 #>
    param($Value, [string] $Suffix = '')

    if ($null -eq $Value) { return '—' }
    "$Value$Suffix"
}

function Get-MaxOptionalProperty {
    <# 返回一组遥测对象指定字段的最大数值；没有样本时保持为空。 #>
    param([object[]] $Rows, [string] $Name)

    $values = @(
        $Rows | ForEach-Object { Get-OptionalProperty -Object $_ -Name $Name } |
            Where-Object { $null -ne $_ }
    )
    if ($values.Count -eq 0) { return $null }
    ($values | Measure-Object -Maximum).Maximum
}

function Get-PipelineCurrent {
    <# 读取某个 ownership/队列指标的 current；缺字段保持 null，不能用零冒充旧 Node。 #>
    param($Pipeline, [string] $Name)

    $metric = Get-OptionalProperty -Object $Pipeline -Name $Name
    if ($null -eq $metric) { return $null }
    Get-OptionalProperty -Object $metric -Name 'current'
}

function Get-PipelineCapacity {
    <# 读取指标硬容量；调用方再按协议字段回退到 execution_config。 #>
    param($Pipeline, [string] $Name)

    $metric = Get-OptionalProperty -Object $Pipeline -Name $Name
    if ($null -eq $metric) { return $null }
    Get-OptionalProperty -Object $metric -Name 'capacity'
}

function Test-MetricValuesComplete {
    <# 判断守恒公式所需的字段是否全部存在；缺失交给 INCONCLUSIVE 而非伪造计算。 #>
    param([hashtable] $Values)

    foreach ($key in $Values.Keys) {
        if ($null -eq $Values[$key]) { return $false }
    }
    $true
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
    <# 比较两个已解析 JSON 的结构，不依赖文件格式或 BOM 差异。 #>
    param($Left, $Right)
    (($Left | ConvertTo-Json -Depth 30 -Compress) -ceq ($Right | ConvertTo-Json -Depth 30 -Compress))
}

function Get-PhysicalDiskMapEvidence {
    <# 读取可选双盘映射并校验根、DiskNumber 与 SHA；旧单根证据没有该文件时保持兼容。 #>
    param(
        [Parameter(Mandatory)] $Harness,
        [string] $EvidenceRoot = '',
        [switch] $RequireSha
    )

    $path = [string](Get-OptionalProperty -Object $Harness -Name 'physical_disk_map_path')
    if ([string]::IsNullOrWhiteSpace($path)) {
        return [pscustomobject]@{
            Present = $false; Valid = $true; Path = ''; ShaMatches = $true; Entries = @()
            EntriesPropertyPresent = $false; DistinctMatches = $true; Diagnostic = ''
        }
    }
    try {
        if (-not [IO.Path]::IsPathFullyQualified($path)) {
            return [pscustomobject]@{
                Present = $true; Valid = $false; Path = $path; ShaMatches = $false; Entries = @()
                EntriesPropertyPresent = $false; DistinctMatches = $false; Diagnostic = 'RUST_V2_RUNTIME_PHYSICAL_DISK_MAP_PATH_INVALID'
            }
        }
        $absolute = [IO.Path]::GetFullPath($path)
        $evidenceAbsolute = if ([string]::IsNullOrWhiteSpace($EvidenceRoot)) {
            ''
        }
        else {
            [IO.Path]::GetFullPath($EvidenceRoot).TrimEnd('\')
        }
        if (-not [IO.Path]::IsPathFullyQualified($absolute) -or
            ($evidenceAbsolute -and -not $absolute.StartsWith(($evidenceAbsolute + '\'), [StringComparison]::OrdinalIgnoreCase))) {
            return [pscustomobject]@{
                Present = $true; Valid = $false; Path = $absolute; ShaMatches = $false; Entries = @()
                EntriesPropertyPresent = $false; DistinctMatches = $false; Diagnostic = 'RUST_V2_RUNTIME_PHYSICAL_DISK_MAP_PATH_INVALID'
            }
        }
        if (-not (Test-Path -LiteralPath $absolute -PathType Leaf)) {
            return [pscustomobject]@{
                Present = $true; Valid = $false; Path = $absolute; ShaMatches = $false; Entries = @()
                EntriesPropertyPresent = $false; DistinctMatches = $false; Diagnostic = 'RUST_V2_RUNTIME_PHYSICAL_DISK_MAP_MISSING'
            }
        }
        $map = [IO.File]::ReadAllText($absolute) | ConvertFrom-Json
        $entriesProperty = $map.PSObject.Properties['entries']
        $entries = if ($null -ne $entriesProperty) { @($entriesProperty.Value) } else { @() }
        $requiredNames = @('root', 'drive_letter', 'partition_number', 'disk_number', 'friendly_name', 'bus_type')
        $valid = [string](Get-OptionalProperty -Object $map -Name 'schema') -eq 'rust-v2-physical-disk-map/v1' -and
        $null -ne $entriesProperty -and @($entries).Count -gt 0
        foreach ($entry in $entries) {
            foreach ($name in $requiredNames) {
                if ($null -eq $entry.PSObject.Properties[$name]) { $valid = $false }
            }
        }
        $diskNumbers = @($entries | ForEach-Object { [string](Get-OptionalProperty -Object $_ -Name 'disk_number') } | Sort-Object -Unique)
        if (@($diskNumbers).Count -ne @($entries).Count) { $valid = $false }
        $declaredDistinct = @((Get-OptionalProperty -Object $map -Name 'distinct_disk_numbers') | ForEach-Object { [string]$_ })
        $distinctMatches = @($declaredDistinct).Count -eq @($diskNumbers).Count -and
            (($declaredDistinct | Sort-Object) -join ',' -ceq ($diskNumbers | Sort-Object) -join ',')
        if (-not $distinctMatches) { $valid = $false }
        $expectedSha = [string](Get-OptionalProperty -Object $Harness -Name 'physical_disk_map_sha256')
        $shaMatches = -not $RequireSha -and [string]::IsNullOrWhiteSpace($expectedSha)
        if (-not [string]::IsNullOrWhiteSpace($expectedSha)) {
            $actualSha = (Get-FileHash -LiteralPath $absolute -Algorithm SHA256).Hash.ToLowerInvariant()
            $shaMatches = $expectedSha -match '^[0-9a-f]{64}$' -and $actualSha -ceq $expectedSha.ToLowerInvariant()
        }
        if (-not $shaMatches) { $valid = $false }
        [pscustomobject]@{
            Present = $true; Valid = $valid; Path = $absolute; ShaMatches = $shaMatches; Entries = $entries
            EntriesPropertyPresent = ($null -ne $entriesProperty); DistinctMatches = $distinctMatches
            Diagnostic = if ($valid) { '' } else { 'RUST_V2_RUNTIME_PHYSICAL_DISK_MAP_INVALID' }
        }
    }
    catch {
        [pscustomobject]@{
            Present = $true; Valid = $false; Path = $path; ShaMatches = $false; Entries = @()
            EntriesPropertyPresent = $false; DistinctMatches = $false; Diagnostic = 'RUST_V2_RUNTIME_PHYSICAL_DISK_MAP_INVALID'
        }
    }
}

function Format-UnixMillisecondsUtc {
    <# 把采样毫秒时间格式化为稳定 UTC；缺失时间保持破折号。 #>
    param($Value)

    if ($null -eq $Value) { return '—' }
    try {
        [DateTimeOffset]::FromUnixTimeMilliseconds([int64]$Value).UtcDateTime.ToString('O')
    }
    catch {
        '—'
    }
}

function Get-ProductionSampleInterval {
    <# 为同一任务的 BaseCompute running 样本验证向后间隔；任何时间矛盾都返回不可靠证据。 #>
    param(
        [Parameter(Mandatory)] [object[]] $TaskSamples,
        [Parameter(Mandatory)] [int] $SampleIndex
    )

    $sample = $TaskSamples[$SampleIndex]
    $baseStages = @((Get-OptionalProperty -Object $sample -Name 'stages') |
        Where-Object { Get-IsBaseComputeStage -Stage $_ })
    if ($baseStages.Count -eq 0 -or -not (Get-IsRunningStage -Stage $baseStages[-1])) {
        return [pscustomobject]@{ IsProduction = $false; Reliable = $true; Interval = $null }
    }
    # 每个任务的首条生产样本没有同任务上一边界，只是不贡献覆盖区间，不把固定 interval=0 误判为坏证据。
    if ($SampleIndex -eq 0) {
        return [pscustomobject]@{ IsProduction = $true; Reliable = $true; Interval = $null }
    }
    $timestampMs = Get-SampleTimestampMs -Sample $sample
    $previousTimestampMs = Get-SampleTimestampMs -Sample $TaskSamples[$SampleIndex - 1]
    if ($null -eq $timestampMs -or $null -eq $previousTimestampMs) {
        return [pscustomobject]@{ IsProduction = $true; Reliable = $false; Interval = $null }
    }
    $declaredValue = Get-OptionalProperty -Object $sample -Name 'sample_interval_ms'
    try { $intervalMs = [double]$declaredValue }
    catch { return [pscustomobject]@{ IsProduction = $true; Reliable = $false; Interval = $null } }
    $actualGapMs = [double]$timestampMs - [double]$previousTimestampMs
    if ($null -eq $declaredValue -or $intervalMs -le 0 -or $intervalMs -gt 2500 -or
        $actualGapMs -le 0 -or $actualGapMs -gt 2500 -or [Math]::Abs($intervalMs - $actualGapMs) -gt 500) {
        return [pscustomobject]@{ IsProduction = $true; Reliable = $false; Interval = $null }
    }
    [pscustomobject]@{
        IsProduction = $true
        Reliable = $true
        Interval = [pscustomobject]@{ StartMs = [double]$timestampMs - $intervalMs; EndMs = [double]$timestampMs }
    }
}

function Test-RequestIntervalsConnected {
    <# 判断两盘的生产请求区间是否相邻或重叠，用于证明后一盘在前一盘耗尽前已进入调度窗口。 #>
    param(
        [Parameter(Mandatory)] [object[]] $LeftIntervals,
        [Parameter(Mandatory)] [object[]] $RightIntervals
    )

    foreach ($left in $LeftIntervals) {
        foreach ($right in $RightIntervals) {
            if ([double]$left.StartMs -le [double]$right.EndMs -and
                [double]$right.StartMs -le [double]$left.EndMs) {
                return $true
            }
        }
    }
    $false
}

function Get-DiskReadAcceptanceEvidence {
    <# 绑定完整媒体闭包，按 runtime_task_id 检查逐盘许可、单调守恒、任务终态和生产区间可见性。 #>
    param(
        [Parameter(Mandatory)] [object[]] $RuntimeSamples,
        [Parameter(Mandatory)] $RuntimeEvidence,
        [Parameter(Mandatory)] $Before,
        [Parameter(Mandatory)] $PhysicalDiskMapEvidence,
        $MediaEvidenceClosure
    )

    $tableLines = [Collections.Generic.List[string]]::new()
    $missingReasons = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    $hardFailures = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    $mediaClosureValid = $null -eq $MediaEvidenceClosure -or [bool]$MediaEvidenceClosure.Valid
    if (-not $PhysicalDiskMapEvidence.Present -or -not $PhysicalDiskMapEvidence.Valid -or -not $mediaClosureValid) {
        $tableLines.Add('| — | — | — | — | — | — | — | — |')
        return [pscustomobject]@{
            Enabled = $false; TableLines = @($tableLines); MissingReasons = @(); HardFailures = @()
        }
    }

    # 映射条目是预期盘集合的唯一来源；RootIndex 只用于判断该根是否确实有输入文件。
    $expectedDisks = [Collections.Generic.List[object]]::new()
    $entryIndex = 0
    foreach ($entry in @($PhysicalDiskMapEvidence.Entries)) {
        $entryIndex++
        $diskNumber = Get-OptionalProperty -Object $entry -Name 'disk_number'
        $diskId = "PhysicalDisk$diskNumber"
        $rootHasWork = @((Get-OptionalProperty -Object $Before -Name 'Files') | Where-Object {
                $rootIndex = Get-OptionalProperty -Object $_ -Name 'RootIndex'
                $null -ne $rootIndex -and [int]$rootIndex -eq $entryIndex
            }).Count -gt 0
        [void]$expectedDisks.Add([pscustomobject]@{
                PhysicalDiskId = $diskId
                HasWork = $rootHasWork
                Observed = $false
                Capacity = $null
                WaitingPeak = 0.0
                ActivePeak = 0.0
                GrantedTotal = 0.0
                ReleasedTotal = 0.0
                FirstActiveUtcMs = $null
                LastActiveUtcMs = $null
            })
    }
    $expectedDisks = @($expectedDisks | Sort-Object -Property PhysicalDiskId)

    $requiredFields = @(
        'capacity', 'hash_waiting', 'media_waiting', 'hash_active', 'media_active',
        'hash_granted_total', 'media_granted_total', 'hash_released_total', 'media_released_total'
    )
    $sampleGroups = @{}
    foreach ($sample in $RuntimeSamples) {
        $taskId = [string](Get-OptionalProperty -Object $sample -Name 'runtime_task_id')
        if ([string]::IsNullOrWhiteSpace($taskId)) {
            [void]$missingReasons.Add('RUST_V2_RUNTIME_DISK_READ_TASK_ID_MISSING')
            continue
        }
        if (-not $sampleGroups.ContainsKey($taskId)) {
            $sampleGroups[$taskId] = [Collections.Generic.List[object]]::new()
        }
        [void]$sampleGroups[$taskId].Add($sample)
    }

    $workDisks = @($expectedDisks | Where-Object HasWork)
    $taskSummaries = [Collections.Generic.List[object]]::new()
    foreach ($taskId in @($sampleGroups.Keys | Sort-Object)) {
        $taskSamples = @($sampleGroups[$taskId])
        $timestampsComplete = @($taskSamples | Where-Object { $null -eq (Get-SampleTimestampMs -Sample $_) }).Count -eq 0
        if ($timestampsComplete) {
            $taskSamples = @($taskSamples | Sort-Object { Get-SampleTimestampMs -Sample $_ })
        }
        # 缺失证据按任务隔离；B 的不完整性不能取消已经闭合的 A 的硬裁决资格。
        $taskMissingReasons = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
        $matchingScanTasks = @($RuntimeEvidence.ScanTasks | Where-Object {
                [string]::Equals([string](Get-OptionalProperty -Object $_ -Name 'runtime_task_id'), $taskId, [StringComparison]::Ordinal)
            })
        if ($matchingScanTasks.Count -ne 1) {
            [void]$taskMissingReasons.Add("RUST_V2_RUNTIME_TASK_BINDING_INVALID:$taskId")
        }
        elseif ([string](Get-OptionalProperty -Object $matchingScanTasks[0] -Name 'terminal_state') -notin @('completed', 'failed', 'cancelled')) {
            [void]$taskMissingReasons.Add("RUST_V2_RUNTIME_TASK_TERMINAL_MISSING:$taskId")
        }
        $lastState = [string](Get-OptionalProperty -Object $taskSamples[-1] -Name 'state')
        $terminalRuntimeSample = if ($lastState -in @('completed', 'failed', 'cancelled')) { $taskSamples[-1] } else { $null }
        if ($null -eq $terminalRuntimeSample) {
            [void]$taskMissingReasons.Add("RUST_V2_RUNTIME_DISK_READ_TERMINAL_SAMPLE_MISSING:$taskId")
        }

        # 每个任务维护独立累计值；同盘跨任务允许从零重置，任务内禁止回退。
        $taskDisks = @{}
        foreach ($expectedDisk in $expectedDisks) {
            $taskDisks[$expectedDisk.PhysicalDiskId] = [pscustomobject]@{
                PhysicalDiskId = $expectedDisk.PhysicalDiskId
                Observed = $false
                Capacity = $null
                WaitingPeak = 0.0
                ActivePeak = 0.0
                HashGrantedMax = 0.0
                MediaGrantedMax = 0.0
                HashReleasedMax = 0.0
                MediaReleasedMax = 0.0
                FirstActiveUtcMs = $null
                LastActiveUtcMs = $null
                PreviousTotals = @{}
                RequestIntervals = [Collections.Generic.List[object]]::new()
            }
        }
        $usableProductionIntervals = 0
        $taskTimeReliable = $true
        for ($sampleIndex = 0; $sampleIndex -lt $taskSamples.Count; $sampleIndex++) {
            $sample = $taskSamples[$sampleIndex]
            $sampleTimeEvidence = Get-ProductionSampleInterval -TaskSamples $taskSamples -SampleIndex $sampleIndex
            if ($sampleTimeEvidence.IsProduction -and -not $sampleTimeEvidence.Reliable) { $taskTimeReliable = $false }
            $sampleInterval = $sampleTimeEvidence.Interval
            if ($null -ne $sampleInterval) { $usableProductionIntervals++ }
            $pipeline = Get-OptionalProperty -Object $sample -Name 'pipeline_metrics'
            if ($null -eq $pipeline) {
                [void]$taskMissingReasons.Add('RUST_V2_RUNTIME_DISK_READ_METRICS_MISSING')
                continue
            }
            $diskReadsProperty = $pipeline.PSObject.Properties['disk_reads']
            if ($null -eq $diskReadsProperty -or $null -eq $diskReadsProperty.Value) {
                [void]$taskMissingReasons.Add('RUST_V2_RUNTIME_DISK_READ_METRICS_MISSING')
                continue
            }
            $rowsById = @{}
            foreach ($row in @($diskReadsProperty.Value)) {
                if ($null -eq $row) { continue }
                $diskId = [string](Get-OptionalProperty -Object $row -Name 'physical_disk_id')
                if ([string]::IsNullOrWhiteSpace($diskId) -or $rowsById.ContainsKey($diskId)) {
                    [void]$taskMissingReasons.Add('RUST_V2_RUNTIME_DISK_READ_METRICS_INVALID')
                    continue
                }
                $rowsById[$diskId] = $row
            }

            $terminalSample = [string](Get-OptionalProperty -Object $sample -Name 'state') -in @('completed', 'failed', 'cancelled')
            foreach ($expectedDisk in $expectedDisks) {
                $disk = $taskDisks[$expectedDisk.PhysicalDiskId]
                if (-not $rowsById.ContainsKey($disk.PhysicalDiskId)) {
                    if ($terminalSample) {
                        [void]$taskMissingReasons.Add("RUST_V2_RUNTIME_DISK_READ_TERMINAL_ROW_MISSING:${taskId}:$($disk.PhysicalDiskId)")
                    }
                    continue
                }
                $row = $rowsById[$disk.PhysicalDiskId]
                $disk.Observed = $true
                $complete = $true
                foreach ($field in $requiredFields) {
                    if ($null -eq $row.PSObject.Properties[$field] -or
                        $null -eq (Get-OptionalProperty -Object $row -Name $field)) {
                        $complete = $false
                    }
                }
                if (-not $complete) {
                    [void]$taskMissingReasons.Add("RUST_V2_RUNTIME_DISK_READ_FIELDS_MISSING:${taskId}:$($disk.PhysicalDiskId)")
                    continue
                }

                $capacity = [double](Get-OptionalProperty -Object $row -Name 'capacity')
                $hashWaiting = [double](Get-OptionalProperty -Object $row -Name 'hash_waiting')
                $mediaWaiting = [double](Get-OptionalProperty -Object $row -Name 'media_waiting')
                $hashActive = [double](Get-OptionalProperty -Object $row -Name 'hash_active')
                $mediaActive = [double](Get-OptionalProperty -Object $row -Name 'media_active')
                $hashGranted = [double](Get-OptionalProperty -Object $row -Name 'hash_granted_total')
                $mediaGranted = [double](Get-OptionalProperty -Object $row -Name 'media_granted_total')
                $hashReleased = [double](Get-OptionalProperty -Object $row -Name 'hash_released_total')
                $mediaReleased = [double](Get-OptionalProperty -Object $row -Name 'media_released_total')
                $waiting = $hashWaiting + $mediaWaiting
                $active = $hashActive + $mediaActive

                if ($null -eq $disk.Capacity -or $capacity -gt [double]$disk.Capacity) { $disk.Capacity = $capacity }
                $disk.WaitingPeak = [Math]::Max([double]$disk.WaitingPeak, $waiting)
                $disk.ActivePeak = [Math]::Max([double]$disk.ActivePeak, $active)
                $disk.HashGrantedMax = [Math]::Max([double]$disk.HashGrantedMax, $hashGranted)
                $disk.MediaGrantedMax = [Math]::Max([double]$disk.MediaGrantedMax, $mediaGranted)
                $disk.HashReleasedMax = [Math]::Max([double]$disk.HashReleasedMax, $hashReleased)
                $disk.MediaReleasedMax = [Math]::Max([double]$disk.MediaReleasedMax, $mediaReleased)
                if ($waiting -gt $capacity -or $active -gt $capacity) {
                    [void]$hardFailures.Add("DISK_READ_CAPACITY_EXCEEDED:$($disk.PhysicalDiskId)")
                }
                if ($hashReleased -gt $hashGranted -or $mediaReleased -gt $mediaGranted) {
                    [void]$hardFailures.Add("DISK_READ_RELEASED_EXCEEDS_GRANTED:$($disk.PhysicalDiskId)")
                }
                foreach ($totalField in @(
                        [pscustomobject]@{ Name = 'hash_granted_total'; Value = $hashGranted },
                        [pscustomobject]@{ Name = 'media_granted_total'; Value = $mediaGranted },
                        [pscustomobject]@{ Name = 'hash_released_total'; Value = $hashReleased },
                        [pscustomobject]@{ Name = 'media_released_total'; Value = $mediaReleased }
                    )) {
                    if ($disk.PreviousTotals.ContainsKey($totalField.Name) -and
                        [double]$totalField.Value -lt [double]$disk.PreviousTotals[$totalField.Name]) {
                        [void]$hardFailures.Add("DISK_READ_TOTAL_ROLLBACK:${taskId}:$($disk.PhysicalDiskId):$($totalField.Name)")
                    }
                    $disk.PreviousTotals[$totalField.Name] = [double]$totalField.Value
                }
                if ($terminalSample -and ($waiting -ne 0 -or $active -ne 0)) {
                    [void]$hardFailures.Add("DISK_READ_TERMINAL_NOT_ZERO:$($disk.PhysicalDiskId)")
                }
                if ($terminalSample -and ($hashGranted -ne $hashReleased -or $mediaGranted -ne $mediaReleased)) {
                    [void]$hardFailures.Add("DISK_READ_TERMINAL_TOTALS_UNBALANCED:${taskId}:$($disk.PhysicalDiskId)")
                }
                if ($null -ne $sampleInterval -and ($waiting -gt 0 -or $active -gt 0)) {
                    [void]$disk.RequestIntervals.Add($sampleInterval)
                }
                if ($null -ne $sampleInterval -and $active -gt 0) {
                    if ($null -eq $disk.FirstActiveUtcMs -or $sampleInterval.StartMs -lt $disk.FirstActiveUtcMs) {
                        $disk.FirstActiveUtcMs = $sampleInterval.StartMs
                    }
                    if ($null -eq $disk.LastActiveUtcMs -or $sampleInterval.EndMs -gt $disk.LastActiveUtcMs) {
                        $disk.LastActiveUtcMs = $sampleInterval.EndMs
                    }
                }
            }
        }
        if (-not $taskTimeReliable) {
            [void]$taskMissingReasons.Add("RUST_V2_RUNTIME_DISK_READ_TIME_INTERVAL_INVALID:$taskId")
        }
        if ($usableProductionIntervals -eq 0) {
            [void]$taskMissingReasons.Add("RUST_V2_RUNTIME_DISK_READ_TIME_INTERVAL_MISSING:$taskId")
        }
        foreach ($workDisk in $workDisks) {
            if (-not $taskDisks[$workDisk.PhysicalDiskId].Observed) {
                [void]$taskMissingReasons.Add("RUST_V2_RUNTIME_DISK_READ_EXPECTED_DISK_MISSING:${taskId}:$($workDisk.PhysicalDiskId)")
            }
        }
        foreach ($reason in $taskMissingReasons) { [void]$missingReasons.Add($reason) }
        [void]$taskSummaries.Add([pscustomobject]@{
                TaskId = $taskId
                Disks = $taskDisks
                ReadyForVisibility = $taskMissingReasons.Count -eq 0
            })
    }

    # 完整媒体闭包已在入口保证；这里逐任务判断 readiness，避免一个坏任务抑制另一个完整任务的硬失败。
    if ($workDisks.Count -gt 1) {
        foreach ($taskSummary in @($taskSummaries | Where-Object ReadyForVisibility)) {
            for ($leftIndex = 0; $leftIndex -lt $workDisks.Count; $leftIndex++) {
                for ($rightIndex = $leftIndex + 1; $rightIndex -lt $workDisks.Count; $rightIndex++) {
                    $leftDisk = $taskSummary.Disks[$workDisks[$leftIndex].PhysicalDiskId]
                    $rightDisk = $taskSummary.Disks[$workDisks[$rightIndex].PhysicalDiskId]
                    if (-not (Test-RequestIntervalsConnected -LeftIntervals @($leftDisk.RequestIntervals) -RightIntervals @($rightDisk.RequestIntervals))) {
                        $pair = @($leftDisk.PhysicalDiskId, $rightDisk.PhysicalDiskId) | Sort-Object
                        [void]$hardFailures.Add("DISK_REQUEST_VISIBILITY_NOT_MET:$($pair -join ',')")
                    }
                }
            }
        }
    }

    # 报告总数是每个任务自身最大累计值之和；waiting/active 峰值仍取所有任务中的观测峰值。
    foreach ($taskSummary in $taskSummaries) {
        foreach ($disk in $expectedDisks) {
            $taskDisk = $taskSummary.Disks[$disk.PhysicalDiskId]
            if ($taskDisk.Observed) { $disk.Observed = $true }
            if ($null -ne $taskDisk.Capacity -and ($null -eq $disk.Capacity -or $taskDisk.Capacity -gt $disk.Capacity)) {
                $disk.Capacity = $taskDisk.Capacity
            }
            $disk.WaitingPeak = [Math]::Max([double]$disk.WaitingPeak, [double]$taskDisk.WaitingPeak)
            $disk.ActivePeak = [Math]::Max([double]$disk.ActivePeak, [double]$taskDisk.ActivePeak)
            $disk.GrantedTotal += [double]$taskDisk.HashGrantedMax + [double]$taskDisk.MediaGrantedMax
            $disk.ReleasedTotal += [double]$taskDisk.HashReleasedMax + [double]$taskDisk.MediaReleasedMax
            if ($null -ne $taskDisk.FirstActiveUtcMs -and
                ($null -eq $disk.FirstActiveUtcMs -or $taskDisk.FirstActiveUtcMs -lt $disk.FirstActiveUtcMs)) {
                $disk.FirstActiveUtcMs = $taskDisk.FirstActiveUtcMs
            }
            if ($null -ne $taskDisk.LastActiveUtcMs -and
                ($null -eq $disk.LastActiveUtcMs -or $taskDisk.LastActiveUtcMs -gt $disk.LastActiveUtcMs)) {
                $disk.LastActiveUtcMs = $taskDisk.LastActiveUtcMs
            }
        }
    }
    foreach ($disk in $expectedDisks) {
        $tableLines.Add("| $($disk.PhysicalDiskId) | $(Format-Optional $disk.Capacity) | $($disk.WaitingPeak) | $($disk.ActivePeak) | $($disk.GrantedTotal) | $($disk.ReleasedTotal) | $(Format-UnixMillisecondsUtc $disk.FirstActiveUtcMs) | $(Format-UnixMillisecondsUtc $disk.LastActiveUtcMs) |")
    }
    [pscustomobject]@{
        Enabled = $true
        TableLines = @($tableLines)
        MissingReasons = @($missingReasons | Sort-Object)
        HardFailures = @($hardFailures | Sort-Object)
    }
}

function Get-V2MediaEvidenceClosure {
    <# 闭合 v2 的根、物理盘映射和分根清单，任何错绑都只允许 INCONCLUSIVE。 #>
    param(
        [Parameter(Mandatory)] $Before,
        [Parameter(Mandatory)] $After,
        [Parameter(Mandatory)] $Harness,
        [Parameter(Mandatory)] $RuntimeResult,
        [Parameter(Mandatory)] [object[]] $RuntimeSamples,
        [Parameter(Mandatory)] [string] $EvidenceRoot
    )

    $errors = [Collections.Generic.List[string]]::new()
    $evidenceAbsolute = [IO.Path]::GetFullPath($EvidenceRoot).TrimEnd('\')
    $beforeRoots = @((Get-OptionalProperty -Object $Before -Name 'Roots'))
    $afterRoots = @((Get-OptionalProperty -Object $After -Name 'Roots'))
    $harnessRootsProperty = $Harness.PSObject.Properties['media_roots']
    if ($null -eq $harnessRootsProperty) {
        $errors.Add('RUST_V2_RUNTIME_MEDIA_ROOT_BINDING_MISSING')
    }
    $harnessRoots = if ($null -eq $harnessRootsProperty) { @() } else { @($harnessRootsProperty.Value) }
    if (@($beforeRoots).Count -eq 0 -or @($afterRoots).Count -ne @($beforeRoots).Count -or
        @($harnessRoots).Count -ne @($beforeRoots).Count) {
        $errors.Add('RUST_V2_RUNTIME_MEDIA_ROOT_BINDING_INVALID')
    }
    $rootCount = @($beforeRoots).Count
    for ($rootIndex = 0; $rootIndex -lt $rootCount; $rootIndex++) {
        if (-not (Test-WindowsEvidencePathEqual -Left ([string]$harnessRoots[$rootIndex]) -Right ([string]$beforeRoots[$rootIndex])) -or
            -not (Test-WindowsEvidencePathEqual -Left ([string]$afterRoots[$rootIndex]) -Right ([string]$beforeRoots[$rootIndex]))) {
            $errors.Add("RUST_V2_RUNTIME_MEDIA_ROOT_BINDING_INVALID:$($rootIndex + 1)")
        }
    }

    $physicalMap = Get-PhysicalDiskMapEvidence -Harness $Harness -EvidenceRoot $EvidenceRoot -RequireSha
    if (-not $physicalMap.Present) {
        $errors.Add('RUST_V2_RUNTIME_PHYSICAL_DISK_MAP_MISSING')
    }
    elseif (-not $physicalMap.Valid) {
        $errors.Add($physicalMap.Diagnostic)
    }
    else {
        if (@($physicalMap.Entries).Count -ne $rootCount) {
            $errors.Add('RUST_V2_RUNTIME_PHYSICAL_DISK_MAP_ROOT_COUNT_INVALID')
        }
        for ($entryIndex = 0; $entryIndex -lt [Math]::Min(@($physicalMap.Entries).Count, $rootCount); $entryIndex++) {
            if (-not (Test-WindowsEvidencePathEqual -Left ([string]$physicalMap.Entries[$entryIndex].root) -Right ([string]$beforeRoots[$entryIndex]))) {
                $errors.Add("RUST_V2_RUNTIME_PHYSICAL_DISK_MAP_ROOT_ORDER_INVALID:$($entryIndex + 1)")
            }
        }
    }

    $beforePathProperty = $Harness.PSObject.Properties['media_before_root_paths']
    $afterPathProperty = $Harness.PSObject.Properties['media_after_root_paths']
    $beforeShaProperty = $Harness.PSObject.Properties['media_before_root_sha256']
    $afterShaProperty = $Harness.PSObject.Properties['media_after_root_sha256']
    if ($null -eq $beforePathProperty -or $null -eq $afterPathProperty -or
        $null -eq $beforeShaProperty -or $null -eq $afterShaProperty) {
        $errors.Add('RUST_V2_RUNTIME_MEDIA_ROOT_EVIDENCE_FIELDS_MISSING')
    }
    $beforePaths = if ($null -eq $beforePathProperty) { @() } else { @($beforePathProperty.Value) }
    $afterPaths = if ($null -eq $afterPathProperty) { @() } else { @($afterPathProperty.Value) }
    $beforeShas = if ($null -eq $beforeShaProperty) { @() } else { @($beforeShaProperty.Value) }
    $afterShas = if ($null -eq $afterShaProperty) { @() } else { @($afterShaProperty.Value) }
    if (@($beforePaths).Count -ne $rootCount -or @($afterPaths).Count -ne $rootCount -or
        @($beforeShas).Count -ne $rootCount -or @($afterShas).Count -ne $rootCount) {
        $errors.Add('RUST_V2_RUNTIME_MEDIA_ROOT_EVIDENCE_COUNT_INVALID')
    }
    for ($rootIndex = 0; $rootIndex -lt $rootCount; $rootIndex++) {
        $rootBefore = $null
        $rootAfter = $null
        if ($rootIndex -ge @($beforePaths).Count -or $rootIndex -ge @($afterPaths).Count -or
            $rootIndex -ge @($beforeShas).Count -or $rootIndex -ge @($afterShas).Count) {
            continue
        }
        foreach ($pathPair in @(
                [pscustomobject]@{ Path = [string]$beforePaths[$rootIndex]; ExpectedSha = [string]$beforeShas[$rootIndex]; Label = 'before' }
                [pscustomobject]@{ Path = [string]$afterPaths[$rootIndex]; ExpectedSha = [string]$afterShas[$rootIndex]; Label = 'after' }
            )) {
            $absolutePath = ''
            try {
                if ([IO.Path]::IsPathFullyQualified($pathPair.Path)) {
                    $absolutePath = [IO.Path]::GetFullPath($pathPair.Path)
                }
            }
            catch { }
            if ([string]::IsNullOrWhiteSpace($absolutePath) -or
                -not $absolutePath.StartsWith(($evidenceAbsolute + '\'), [StringComparison]::OrdinalIgnoreCase)) {
                $errors.Add("RUST_V2_RUNTIME_MEDIA_ROOT_EVIDENCE_PATH_INVALID:$($pathPair.Label):$($rootIndex + 1)")
                continue
            }
            if (-not (Test-Path -LiteralPath $absolutePath -PathType Leaf)) {
                $errors.Add("RUST_V2_RUNTIME_MEDIA_ROOT_EVIDENCE_MISSING:$($pathPair.Label):$($rootIndex + 1)")
                continue
            }
            $actualSha = (Get-FileHash -LiteralPath $absolutePath -Algorithm SHA256).Hash.ToLowerInvariant()
            if ($pathPair.ExpectedSha -notmatch '^[0-9a-f]{64}$' -or $actualSha -cne $pathPair.ExpectedSha.ToLowerInvariant()) {
                $errors.Add("RUST_V2_RUNTIME_MEDIA_ROOT_EVIDENCE_SHA_INVALID:$($pathPair.Label):$($rootIndex + 1)")
            }
            try {
                $manifest = [IO.File]::ReadAllText($absolutePath) | ConvertFrom-Json
                $manifestRoot = [string](Get-OptionalProperty -Object $manifest -Name 'Root')
                if (-not (Test-WindowsEvidencePathEqual -Left $manifestRoot -Right ([string]$beforeRoots[$rootIndex]))) {
                    $errors.Add("RUST_V2_RUNTIME_MEDIA_ROOT_MANIFEST_ROOT_INVALID:$($pathPair.Label):$($rootIndex + 1)")
                }
                if ($pathPair.Label -eq 'before') { $rootBefore = $manifest } else { $rootAfter = $manifest }
            }
            catch {
                $errors.Add("RUST_V2_RUNTIME_MEDIA_ROOT_MANIFEST_INVALID:$($pathPair.Label):$($rootIndex + 1)")
            }
        }
        if ($null -ne $rootBefore -and $null -ne $rootAfter -and -not (Json-Equivalent -Left $rootBefore -Right $rootAfter)) {
            $errors.Add("RUST_V2_RUNTIME_MEDIA_ROOT_CHANGED:$($rootIndex + 1)")
        }
    }

    # runtime_result 是客户端最终自报的根集合，必须和静态清单逐项闭合，不能只信 harness 字段。
    $runtimeRootsProperty = $RuntimeResult.PSObject.Properties['media_roots']
    $runtimeRoots = if ($null -eq $runtimeRootsProperty) { @() } else { @($runtimeRootsProperty.Value) }
    if ($null -eq $runtimeRootsProperty) {
        $errors.Add('RUST_V2_RUNTIME_MEDIA_ROOT_RUNTIME_RESULT_BINDING_MISSING')
    }
    elseif ($runtimeRoots.Count -ne $rootCount) {
        $errors.Add('RUST_V2_RUNTIME_MEDIA_ROOT_RUNTIME_RESULT_BINDING_INVALID')
    }
    for ($rootIndex = 0; $rootIndex -lt [Math]::Min($runtimeRoots.Count, $rootCount); $rootIndex++) {
        $runtimeRoot = Get-NormalizedWindowsEvidencePath -Path ([string]$runtimeRoots[$rootIndex])
        $staticRoot = Get-NormalizedWindowsEvidencePath -Path ([string]$beforeRoots[$rootIndex])
        if ([string]::IsNullOrWhiteSpace($runtimeRoot) -or
            -not $runtimeRoot.Equals($staticRoot, [StringComparison]::OrdinalIgnoreCase)) {
            $errors.Add("RUST_V2_RUNTIME_MEDIA_ROOT_RUNTIME_RESULT_BINDING_INVALID:$($rootIndex + 1)")
        }
    }

    # 每个配置根都要在至少一个非空 Worker display_path 中出现，避免单盘工作被错误报告成双盘覆盖。
    $observedWorkerRootIndexes = [Collections.Generic.List[int]]::new()
    for ($rootIndex = 0; $rootIndex -lt $rootCount; $rootIndex++) {
        $observed = $false
        foreach ($sample in $RuntimeSamples) {
            foreach ($worker in @((Get-OptionalProperty -Object $sample -Name 'workers'))) {
                $displayPath = [string](Get-OptionalProperty -Object $worker -Name 'display_path')
                if (Test-WindowsEvidencePathWithinRoot -Candidate $displayPath -Root ([string]$beforeRoots[$rootIndex])) {
                    $observed = $true
                    break
                }
            }
            if ($observed) { break }
        }
        if ($observed) {
            $observedWorkerRootIndexes.Add($rootIndex + 1)
        }
        else {
            $errors.Add("RUST_V2_RUNTIME_MEDIA_ROOT_WORKER_OBSERVATION_MISSING:$($rootIndex + 1)")
        }
    }
    [pscustomobject]@{
        Valid = $errors.Count -eq 0
        Errors = @($errors)
        PhysicalDiskMap = $physicalMap
        Entries = @($physicalMap.Entries)
        RuntimeRoots = [string[]]$runtimeRoots
        ObservedWorkerRootIndexes = @($observedWorkerRootIndexes)
        RuntimeRootsClosed = ($null -ne $runtimeRootsProperty -and $runtimeRoots.Count -eq $rootCount -and
            -not @($errors | Where-Object { [string]$_ -like 'RUST_V2_RUNTIME_MEDIA_ROOT_RUNTIME_RESULT_BINDING_*' }).Count)
        WorkerRootsClosed = ($observedWorkerRootIndexes.Count -eq $rootCount)
    }
}

function Get-SampleTimestampMs {
    <# 只接受 utc_unix_ms；缺失时返回 null，禁止用旧 elapsed_seconds 推断时间。 #>
    param([Parameter(Mandatory)] $Sample)

    $utc = Get-OptionalProperty -Object $Sample -Name 'utc_unix_ms'
    if ($null -ne $utc) { return [double]$utc }
    $null
}

function Get-SampleWeights {
    <# 以相邻 UTC 时间差产生真实权重，首样本固定为零。 #>
    param([Parameter(Mandatory)] [object[]] $Samples)

    $ordered = @($Samples | Sort-Object {
        $timestamp = Get-SampleTimestampMs -Sample $_
        if ($null -eq $timestamp) { [double]::PositiveInfinity } else { $timestamp }
    })
    $previous = $null
    @(
        foreach ($sample in $ordered) {
            $timestamp = Get-SampleTimestampMs -Sample $sample
            $weightMs = if ($null -eq $timestamp -or $null -eq $previous) {
                0.0
            }
            else {
                [Math]::Max(0.0, $timestamp - $previous)
            }
            $declared = Get-OptionalProperty -Object $sample -Name 'sample_interval_ms'
            [pscustomobject]@{
                Sample = $sample
                TimestampMs = $timestamp
                WeightMs = $weightMs
                DeclaredIntervalMs = if ($null -eq $declared) { 0.0 } else { [double]$declared }
            }
            if ($null -ne $timestamp) { $previous = $timestamp }
        }
    )
}

function Get-MaxGapMilliseconds {
    <# 返回相邻真实 UTC 样本间最大 gap；报告按 2.5/6 秒硬阈值解释。 #>
    param([Parameter(Mandatory)] [object[]] $Samples)

    $weights = @(Get-SampleWeights -Samples $Samples)
    if ($weights.Count -eq 0) { return 0.0 }
    [double](($weights.WeightMs | Measure-Object -Maximum).Maximum)
}

function Get-IsRunningStage {
    <# 兼容 protobuf 数值状态与 fixture 文本状态，识别生产阶段 running。 #>
    param($Stage)

    if ($null -eq $Stage) { return $false }
    $state = Get-OptionalProperty -Object $Stage -Name 'state'
    [string]$state -in @('2', 'running', 'RUNNING', 'RuntimeStageRunning')
}

function Get-IsBaseComputeStage {
    <# 识别 ComputeBaseFeatures 阶段，避免把 finalization tail 混入生产窗口。 #>
    param($Stage)

    if ($null -eq $Stage) { return $false }
    $id = [string](Get-OptionalProperty -Object $Stage -Name 'stage_id')
    $id -in @('compute_base_features', 'ComputeBaseFeatures', 'base_compute')
}

function Get-OwnershipDefinitionRows {
    <# 返回字段12-26的顺序映射；refill token 通过 IsControl 排除 ownership 求和。 #>
    @(
        [pscustomobject]@{ Label = 'hash_waiting_permit'; Display = 'Hash等待许可'; IsControl = $false }
        [pscustomobject]@{ Label = 'hash_reading'; Display = 'Hash读取中'; IsControl = $false }
        [pscustomobject]@{ Label = 'hash_completed_unjoined'; Display = 'Hash完成待归并'; IsControl = $false }
        [pscustomobject]@{ Label = 'media_permit_waiting'; Display = '媒体许可等待'; IsControl = $false }
        [pscustomobject]@{ Label = 'media_acquire_ready'; Display = '媒体获取就绪'; IsControl = $false }
        [pscustomobject]@{ Label = 'media_permit_ready'; Display = '媒体许可就绪'; IsControl = $false }
        [pscustomobject]@{ Label = 'worker_dispatching'; Display = 'Worker派发中'; IsControl = $false }
        [pscustomobject]@{ Label = 'worker_start_pending'; Display = 'Worker待Started'; IsControl = $false }
        [pscustomobject]@{ Label = 'worker_decode'; Display = 'Worker解码'; IsControl = $false }
        [pscustomobject]@{ Label = 'worker_feature'; Display = 'Worker特征'; IsControl = $false }
        [pscustomobject]@{ Label = 'worker_result_wait'; Display = 'Worker结果等待'; IsControl = $false }
        [pscustomobject]@{ Label = 'worker_phase_unknown'; Display = 'Worker未知阶段'; IsControl = $false }
        [pscustomobject]@{ Label = 'content_output_credit_owned'; Display = 'Content输出credit'; IsControl = $false }
        [pscustomobject]@{ Label = 'hash_refill_token_available'; Display = 'Hash refill token'; IsControl = $true }
        [pscustomobject]@{ Label = 'decode_credit_owned'; Display = 'Decode credit'; IsControl = $false }
    )
}

function Get-ResultSummaryMeta {
    <# 从 harness-result 读取结果摘要状态；缺失按证据不足处理，不伪造 PASS。 #>
    param([Parameter(Mandatory)] $Harness)

    [pscustomobject]@{
        Status = [string](Get-OptionalProperty -Object $Harness -Name 'result_summary_status')
        Path = [string](Get-OptionalProperty -Object $Harness -Name 'result_summary_path')
        Sha256 = [string](Get-OptionalProperty -Object $Harness -Name 'result_summary_sha256')
        TaskId = [string](Get-OptionalProperty -Object $Harness -Name 'result_summary_task_id')
        RowCount = [int64](Get-NumberOrZero (Get-OptionalProperty -Object $Harness -Name 'result_summary_row_count'))
        MissingCount = [int64](Get-NumberOrZero (Get-OptionalProperty -Object $Harness -Name 'result_summary_missing_count'))
        InconclusiveCount = [int64](Get-NumberOrZero (Get-OptionalProperty -Object $Harness -Name 'result_summary_inconclusive_count'))
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
            throw 'RUST_V2_RUNTIME_RESULT_SUMMARY_ROW_COUNT_INVALID'
        }
        return 0
    }
    if ($bytes[$bytes.Length - 1] -ne [byte]10 -or
        [Array]::IndexOf($bytes, [byte]13) -ge 0 -or
        ($bytes.Length -ge 3 -and $bytes[0] -eq [byte]239 -and $bytes[1] -eq [byte]187 -and $bytes[2] -eq [byte]191)) {
        throw 'RUST_V2_RUNTIME_RESULT_SUMMARY_CANONICAL_FORMAT_INVALID'
    }
    $text = [Text.UTF8Encoding]::new($false, $true).GetString($bytes)
    $rows = @(
        $text -split [char]10 |
            Where-Object { -not [string]::IsNullOrWhiteSpace($_) } |
            ForEach-Object { $_ | ConvertFrom-Json }
    )
    if ($rows.Count -eq 0 -or ($ExpectedRowCount -ge 0 -and $rows.Count -ne $ExpectedRowCount)) {
        throw 'RUST_V2_RUNTIME_RESULT_SUMMARY_ROW_COUNT_INVALID'
    }
    $rows.Count
}

function Get-ResultSummaryArtifacts {
    <# 独立复核摘要、metadata、lease token 的路径、任务、状态、SHA、JSONL 行数绑定。 #>
    param(
        [Parameter(Mandatory)] [string] $SummaryPath,
        [string] $ExpectedTaskId = '',
        [string] $ExpectedStatus = '',
        [string] $ExpectedSha256 = '',
        [long] $ExpectedRowCount = -1
    )

    $summary = [IO.Path]::GetFullPath($SummaryPath).TrimEnd('\')
    $metadataPath = Join-Path (Split-Path -Parent $summary) 'result-summary-meta.json'
    $leasePath = "$summary.pair.lock"
    $summaryExists = Test-Path -LiteralPath $summary -PathType Leaf
    $metadataExists = Test-Path -LiteralPath $metadataPath -PathType Leaf
    $leaseExists = Test-Path -LiteralPath $leasePath -PathType Leaf
    $bindingValid = $false
    $diagnostic = ''
    if (-not $summaryExists -or -not $metadataExists -or -not $leaseExists) {
        $diagnostic = 'RUST_V2_RUNTIME_RESULT_SUMMARY_THREE_PIECE_MISSING'
    }
    else {
        try {
            $metadata = [IO.File]::ReadAllText($metadataPath) | ConvertFrom-Json
            $lease = [IO.File]::ReadAllText($leasePath) | ConvertFrom-Json
            $canonicalRowCount = Assert-CanonicalResultSummary -Path $summary -ExpectedRowCount $ExpectedRowCount
            $actualSha256 = (Get-FileHash -LiteralPath $summary -Algorithm SHA256).Hash.ToLowerInvariant()
            $metaToken = [string]$metadata.lease_token
            $leaseToken = [string]$lease.lease_token
            $metaSha256 = [string]$metadata.canonical_sha256
            $leaseSha256 = [string]$lease.expected_canonical_sha256
            $metaTaskId = [string]$metadata.task_id
            $metaStatus = [string]$metadata.status
            $leaseStatus = [string]$lease.expected_status
            $emptyNonMissing = ($canonicalRowCount -eq 0) -and
                (-not $metaStatus.Equals('MISSING', [StringComparison]::OrdinalIgnoreCase) -or
                    -not $leaseStatus.Equals('MISSING', [StringComparison]::OrdinalIgnoreCase))
            $bindingValid = -not [string]::IsNullOrWhiteSpace($metaToken) -and
                $metaToken -ceq $leaseToken -and
                $actualSha256 -ceq $metaSha256 -and $actualSha256 -ceq $leaseSha256 -and
                ([string]::IsNullOrWhiteSpace($ExpectedSha256) -or $actualSha256 -ceq $ExpectedSha256) -and
                # PairLeaseManifest 不含 expected_task_id；ExpectedTaskId 只绑定 metadata.task_id。
                ([string]::IsNullOrWhiteSpace($ExpectedTaskId) -or $metaTaskId -ceq $ExpectedTaskId) -and
                ([string]::IsNullOrWhiteSpace($ExpectedStatus) -or
                    ($metaStatus.Equals($ExpectedStatus, [StringComparison]::OrdinalIgnoreCase) -and
                        $leaseStatus.Equals($ExpectedStatus, [StringComparison]::OrdinalIgnoreCase))) -and
                [long]$metadata.row_count -eq $canonicalRowCount -and
                [long]$lease.expected_row_count -eq $canonicalRowCount -and
                -not $emptyNonMissing
            if (-not $bindingValid) {
                $diagnostic = if ($emptyNonMissing) {
                    'RUST_V2_RUNTIME_RESULT_SUMMARY_EMPTY_NON_MISSING'
                }
                else {
                    'RUST_V2_RUNTIME_RESULT_SUMMARY_BINDING_INVALID'
                }
            }
        }
        catch {
            $diagnostic = 'RUST_V2_RUNTIME_RESULT_SUMMARY_BINDING_INVALID'
        }
    }
    [pscustomobject]@{
        SummaryExists = $summaryExists
        MetadataExists = $metadataExists
        LeaseExists = $leaseExists
        BindingValid = $bindingValid
        Diagnostic = $diagnostic
    }
}

function Test-HarnessSchema2 {
    <# 强制校验 schema2 的必需元数据、类型和值；失败交给 INCONCLUSIVE。 #>
    param([Parameter(Mandatory)] $Harness)

    $errors = [Collections.Generic.List[string]]::new()
    foreach ($name in @(
            'schema_version', 'variant', 'run_index', 'run_status', 'source_revision',
            'source_tree_sha256', 'package_path', 'package_sha256', 'release_root', 'config_sha256',
            'package_manifest_status', 'media_before_sha256', 'media_after_sha256',
            'result_summary_path', 'result_summary_sha256', 'result_summary_status', 'result_summary_task_id',
            'result_summary_row_count', 'result_summary_missing_count', 'result_summary_inconclusive_count',
            'media_unchanged', 'node_unexpected_exit', 'exporter_exit_code',
            'deadline_cancelled_persistent_task_id', 'effective_worker_count', 'hdd_threads_per_disk',
            'ssd_threads_per_disk', 'unknown_threads_per_disk', 'read_total_threads', 'reserved_cores',
            'contact_sheet_reuse_count', 'disk_full_cleanup_count')) {
        if ($null -eq $Harness.PSObject.Properties[$name]) { $errors.Add($name) }
    }
    $schemaVersion = Get-OptionalProperty -Object $Harness -Name 'schema_version'
    if ($schemaVersion -isnot [byte] -and $schemaVersion -isnot [int16] -and $schemaVersion -isnot [int32] -and $schemaVersion -isnot [int64]) {
        $errors.Add('schema_version:type')
    }
    elseif ([int64]$schemaVersion -ne 2) { $errors.Add('schema_version:value') }
    $variant = [string](Get-OptionalProperty -Object $Harness -Name 'variant')
    if ($variant -notin @('A', 'B')) { $errors.Add('variant') }
    $runIndex = Get-OptionalProperty -Object $Harness -Name 'run_index'
    if (($runIndex -isnot [byte] -and $runIndex -isnot [int16] -and $runIndex -isnot [int32] -and $runIndex -isnot [int64]) -or
        [int64]$runIndex -lt 1 -or [int64]$runIndex -gt 3) { $errors.Add('run_index') }
    $runStatus = [string](Get-OptionalProperty -Object $Harness -Name 'run_status')
    if ($runStatus -notin @('PASS', 'FAIL', 'INCONCLUSIVE')) { $errors.Add('run_status') }
    $summaryStatus = [string](Get-OptionalProperty -Object $Harness -Name 'result_summary_status')
    if ($summaryStatus -notin @('PASS', 'MISSING', 'INCONCLUSIVE')) { $errors.Add('result_summary_status') }
    foreach ($name in @('media_unchanged', 'node_unexpected_exit')) {
        $value = Get-OptionalProperty -Object $Harness -Name $name
        if ($value -isnot [bool]) { $errors.Add("$name:type") }
    }
    $exporterExitCode = Get-OptionalProperty -Object $Harness -Name 'exporter_exit_code'
    if ($exporterExitCode -isnot [byte] -and $exporterExitCode -isnot [int16] -and
        $exporterExitCode -isnot [int32] -and $exporterExitCode -isnot [int64]) {
        $errors.Add('exporter_exit_code:type')
    }
    $sourceRevision = [string](Get-OptionalProperty -Object $Harness -Name 'source_revision')
    if ([string]::IsNullOrWhiteSpace($sourceRevision)) { $errors.Add('source_revision') }
    foreach ($name in @('source_tree_sha256', 'config_sha256', 'media_before_sha256', 'media_after_sha256')) {
        if ([string](Get-OptionalProperty -Object $Harness -Name $name) -notmatch '^[0-9a-f]{64}$') { $errors.Add($name) }
    }
    if ([string](Get-OptionalProperty -Object $Harness -Name 'result_summary_sha256') -notmatch '^[0-9a-f]{64}$') {
        $errors.Add('result_summary_sha256')
    }
    $summaryTaskId = Get-OptionalProperty -Object $Harness -Name 'result_summary_task_id'
    if ($summaryStatus -eq 'PASS' -and [string]::IsNullOrWhiteSpace([string]$summaryTaskId)) {
        $errors.Add('result_summary_task_id')
    }
    if ([string](Get-OptionalProperty -Object $Harness -Name 'package_sha256') -notmatch '^[0-9a-f]{64}$') {
        $errors.Add('package_sha256')
    }
    foreach ($name in @('package_path', 'release_root', 'result_summary_path')) {
        $value = [string](Get-OptionalProperty -Object $Harness -Name $name)
        if ([string]::IsNullOrWhiteSpace($value) -or -not [IO.Path]::IsPathFullyQualified($value)) { $errors.Add($name) }
    }
    $manifestStatus = [string](Get-OptionalProperty -Object $Harness -Name 'package_manifest_status')
    if ($manifestStatus -notin @('PRESENT', 'MISSING')) { $errors.Add('package_manifest_status') }
    if ($manifestStatus -eq 'PRESENT' -and [string](Get-OptionalProperty -Object $Harness -Name 'package_manifest_sha256') -notmatch '^[0-9a-f]{64}$') {
        $errors.Add('package_manifest_sha256')
    }
    # Task 16 字段是追加字段；旧 schema2 harness 缺失时仍按 v1 规则继续裁决。
    $mediaRootsProperty = $Harness.PSObject.Properties['media_roots']
    if ($null -ne $mediaRootsProperty) {
        $mediaRoots = @($mediaRootsProperty.Value)
        if ($mediaRoots.Count -eq 0 -or @($mediaRoots | Where-Object {
                    [string]::IsNullOrWhiteSpace([string]$_) -or -not [IO.Path]::IsPathFullyQualified([string]$_)
                }).Count -gt 0) {
            $errors.Add('media_roots')
        }
    }
    $singleRunProperty = $Harness.PSObject.Properties['single_run']
    if ($null -ne $singleRunProperty -and $singleRunProperty.Value -isnot [bool]) {
        $errors.Add('single_run:type')
    }
    foreach ($name in @('physical_disk_map_path', 'media_before_root_paths', 'media_after_root_paths')) {
        $property = $Harness.PSObject.Properties[$name]
        if ($null -eq $property) { continue }
        foreach ($path in @($property.Value)) {
            if (-not [string]::IsNullOrWhiteSpace([string]$path) -and -not [IO.Path]::IsPathFullyQualified([string]$path)) {
                $errors.Add("$name:path")
            }
        }
    }
    $physicalMapSha = [string](Get-OptionalProperty -Object $Harness -Name 'physical_disk_map_sha256')
    if (-not [string]::IsNullOrWhiteSpace($physicalMapSha) -and $physicalMapSha -notmatch '^[0-9a-f]{64}$') {
        $errors.Add('physical_disk_map_sha256')
    }
    foreach ($name in @('result_summary_row_count', 'result_summary_missing_count', 'result_summary_inconclusive_count',
            'effective_worker_count', 'hdd_threads_per_disk',
            'ssd_threads_per_disk', 'unknown_threads_per_disk', 'read_total_threads', 'reserved_cores',
            'contact_sheet_reuse_count', 'disk_full_cleanup_count')) {
        $value = Get-OptionalProperty -Object $Harness -Name $name
        if (($value -isnot [byte] -and $value -isnot [int16] -and $value -isnot [int32] -and $value -isnot [int64]) -or
            [int64]$value -lt 0) { $errors.Add($name) }
    }
    [pscustomobject]@{ Valid = $errors.Count -eq 0; Errors = @($errors) }
}

function Test-RuntimeResultEvidence {
    <# 校验 runtime_result 的失败计数与任务数组，避免缺失证据被默认成零或空数组。 #>
    param([Parameter(Mandatory)] $RuntimeResult)

    $errors = [Collections.Generic.List[string]]::new()
    $failedScans = $null
    $failedScansProperty = $RuntimeResult.PSObject.Properties['failed_scans']
    if ($null -eq $failedScansProperty) {
        $errors.Add('failed_scans:missing')
    }
    else {
        $value = $failedScansProperty.Value
        $isInteger = $value -is [byte] -or $value -is [int16] -or $value -is [int32] -or $value -is [int64]
        if (-not $isInteger) {
            $errors.Add('failed_scans:type')
        }
        elseif ([int64]$value -lt 0) {
            $errors.Add('failed_scans:value')
        }
        else {
            $failedScans = [int64]$value
        }
    }

    $scanTasks = $null
    $scanTasksProperty = $RuntimeResult.PSObject.Properties['scan_tasks']
    if ($null -eq $scanTasksProperty) {
        $errors.Add('scan_tasks:missing')
    }
    else {
        $value = $scanTasksProperty.Value
        if ($value -isnot [Array]) {
            $errors.Add('scan_tasks:type')
        }
        else {
            $scanTasks = @($value)
            foreach ($task in $scanTasks) {
                if ($null -eq $task -or $task -is [string] -or
                    $null -eq $task.PSObject.Properties['terminal_state']) {
                    $errors.Add('scan_tasks:item_type')
                    break
                }
            }
        }
    }

    [pscustomobject]@{
        Valid = $errors.Count -eq 0
        Errors = @($errors)
        FailedScans = $failedScans
        ScanTasks = if ($null -eq $scanTasks) { @() } else { @($scanTasks) }
    }
}

$runtimePath = Join-Path $EvidenceRoot 'runtime.ndjson'
$systemPath = Join-Path $EvidenceRoot 'system.ndjson'
$beforePath = Join-Path $EvidenceRoot 'media-before.json'
$afterPath = Join-Path $EvidenceRoot 'media-after.json'
$harnessPath = Join-Path $EvidenceRoot 'harness-result.json'
$summaryPath = Join-Path $EvidenceRoot 'result-summary.jsonl'
$summaryMetaPath = Join-Path $EvidenceRoot 'result-summary-meta.json'
$summaryLeasePath = "$summaryPath.pair.lock"

$runtimeRecords = Read-Ndjson -Path $runtimePath
$systemSamples = Read-Ndjson -Path $systemPath
$runtimeSamples = @($runtimeRecords | Where-Object record_type -eq 'runtime_sample')
$runtimeResult = @($runtimeRecords | Where-Object record_type -eq 'runtime_result') | Select-Object -Last 1
if (-not $runtimeResult -or $runtimeSamples.Count -eq 0 -or $systemSamples.Count -eq 0) {
    throw 'RUST_V2_RUNTIME_EVIDENCE_INCOMPLETE'
}
$runtimeEvidence = Test-RuntimeResultEvidence -RuntimeResult $runtimeResult
$before = Get-Content -LiteralPath $beforePath -Raw | ConvertFrom-Json
$after = Get-Content -LiteralPath $afterPath -Raw | ConvertFrom-Json
$harness = Get-Content -LiteralPath $harnessPath -Raw | ConvertFrom-Json
$schemaCheck = Test-HarnessSchema2 -Harness $harness
$summaryMeta = Get-ResultSummaryMeta -Harness $harness
$variant = [string](Get-OptionalProperty -Object $harness -Name 'variant')
$harnessSingleRunProperty = $harness.PSObject.Properties['single_run']
$runtimeSingleRunProperty = $runtimeResult.PSObject.Properties['single_run']
$singleRun = [bool](Get-OptionalProperty -Object $harness -Name 'single_run')
$runtimeSingleRun = if ($null -eq $runtimeSingleRunProperty) { $null } else { $runtimeSingleRunProperty.Value }
$configuredDeadlineSeconds = 1800
$lastRuntimeElapsed = [double](($runtimeSamples.elapsed_seconds | Measure-Object -Maximum).Maximum)
$lastRuntimeElapsedText = if ($lastRuntimeElapsed -eq [Math]::Truncate($lastRuntimeElapsed)) {
    [string]([int64]$lastRuntimeElapsed)
}
else {
    '{0:N3}' -f $lastRuntimeElapsed
}
$manifestSchema = [string](Get-OptionalProperty -Object $before -Name 'Schema')
$manifestRoots = @((Get-OptionalProperty -Object $before -Name 'Roots'))
$mediaEvidenceClosure = $null
$physicalDiskMapEvidence = if ($manifestSchema -eq 'rust-v2-media-manifest/v2') {
    $mediaEvidenceClosure = Get-V2MediaEvidenceClosure -Before $before -After $after -Harness $harness `
        -RuntimeResult $runtimeResult -RuntimeSamples $runtimeSamples -EvidenceRoot $EvidenceRoot
    $mediaEvidenceClosure.PhysicalDiskMap
}
else {
    Get-PhysicalDiskMapEvidence -Harness $harness
}
# 逐盘许可只绑定已经通过路径、结构和 SHA 校验的物理盘映射。
$diskReadEvidence = Get-DiskReadAcceptanceEvidence -RuntimeSamples $runtimeSamples `
    -RuntimeEvidence $runtimeEvidence -Before $before -PhysicalDiskMapEvidence $physicalDiskMapEvidence `
    -MediaEvidenceClosure $mediaEvidenceClosure
# Worker 退出 skip 仅作采样诊断；健康 system 序列仍由现有间隔门禁裁决。
$processSampleSkips = @(
    $systemSamples | ForEach-Object {
        $skips = Get-OptionalProperty -Object $_ -Name 'process_sample_skips'
        if ($null -ne $skips) { @($skips) }
    } | Where-Object { $null -ne $_ }
)
$processSampleSkipCount = $processSampleSkips.Count
$summaryFilesComplete = (Test-Path -LiteralPath $summaryPath -PathType Leaf) -and
    (Test-Path -LiteralPath $summaryMetaPath -PathType Leaf) -and
    (Test-Path -LiteralPath $summaryLeasePath -PathType Leaf)
$summaryPathMatches = $false
try {
    $summaryPathMatches = [IO.Path]::GetFullPath($summaryMeta.Path).TrimEnd('\').Equals(
        [IO.Path]::GetFullPath($summaryPath).TrimEnd('\'), [StringComparison]::OrdinalIgnoreCase)
}
catch {
    $summaryPathMatches = $false
}
$completedTaskId = [string](Get-OptionalProperty -Object $runtimeResult -Name 'latest_completed_persistent_task_id')
$summaryTaskMatches = -not [string]::IsNullOrWhiteSpace($completedTaskId) -and
    [string]::Equals($summaryMeta.TaskId, $completedTaskId, [StringComparison]::Ordinal)
$summaryShaMatches = $false
if ($summaryFilesComplete -and $summaryMeta.Sha256 -match '^[0-9a-f]{64}$') {
    $summaryShaMatches = ((Get-FileHash -LiteralPath $summaryPath -Algorithm SHA256).Hash.ToLowerInvariant() -ceq $summaryMeta.Sha256)
}
$summaryBinding = Get-ResultSummaryArtifacts -SummaryPath $summaryPath `
    -ExpectedTaskId $completedTaskId -ExpectedStatus $summaryMeta.Status `
    -ExpectedSha256 $summaryMeta.Sha256 -ExpectedRowCount $summaryMeta.RowCount

$duration = [int64]$runtimeResult.duration_seconds
$runtimeWeights = @(Get-SampleWeights -Samples $runtimeSamples)
$systemWeights = @(Get-SampleWeights -Samples $systemSamples)
$maxRuntimeGapMs = if ($runtimeWeights.Count -eq 0) { 0.0 } else {
    [double](($runtimeWeights.WeightMs | Measure-Object -Maximum).Maximum)
}
$maxSystemGapMs = if ($systemWeights.Count -eq 0) { 0.0 } else {
    [double](($systemWeights.WeightMs | Measure-Object -Maximum).Maximum)
}
$maxGap = [Math]::Round($maxRuntimeGapMs / 1000, 3)
$maxSystemGap = [Math]::Round($maxSystemGapMs / 1000, 3)
$machineIds = @($runtimeSamples.machine_id | Where-Object { $_ } | Sort-Object -Unique)
$taskIds = @($runtimeSamples.runtime_task_id | Where-Object { $_ } | Sort-Object -Unique)
$mediaUnchanged = (Json-Equivalent -Left $before -Right $after) -and [bool](Get-OptionalProperty -Object $harness -Name 'media_unchanged')
$failedScans = $runtimeEvidence.FailedScans
$unexpectedExit = [bool](Get-OptionalProperty -Object $harness -Name 'node_unexpected_exit')
$effectiveWorkers = [int](Get-NumberOrZero (Get-OptionalProperty -Object $harness -Name 'effective_worker_count'))
$hddThreads = [int](Get-NumberOrZero (Get-OptionalProperty -Object $harness -Name 'hdd_threads_per_disk'))
$ssdThreads = [int](Get-NumberOrZero (Get-OptionalProperty -Object $harness -Name 'ssd_threads_per_disk'))
$unknownThreads = [int](Get-NumberOrZero (Get-OptionalProperty -Object $harness -Name 'unknown_threads_per_disk'))
$readTotalThreads = [int](Get-NumberOrZero (Get-OptionalProperty -Object $harness -Name 'read_total_threads'))

$workerSnapshots = @(
    $runtimeSamples | ForEach-Object { @($_.workers) } | Where-Object { $null -ne $_ }
)
$activeWorkerSnapshots = @(Get-ActiveWorkers -Workers $workerSnapshots)
$workerCounts = @(
    $runtimeSamples | ForEach-Object {
        # JSON null 表示该样本没有Worker；显式idle常驻行不能算作有效在途任务。
        $sampleWorkers = Get-OptionalProperty -Object $_ -Name 'workers'
        if ($null -eq $sampleWorkers) {
            0
        }
        else {
            @(Get-ActiveWorkers -Workers @($sampleWorkers)).Count
        }
    }
)
$peakWorkers = if ($workerCounts.Count) { [int](($workerCounts | Measure-Object -Maximum).Maximum) } else { 0 }
$averageWorkers = if ($workerCounts.Count) { [double](($workerCounts | Measure-Object -Average).Average) } else { 0 }
$allWorkerDisks = @(Get-WorkerDiskIds -Workers $activeWorkerSnapshots)
$peakConcurrentDisks = 0
foreach ($sample in $runtimeSamples) {
    # 终态和阶段切换样本允许没有在途Worker，不能对空数组使用成员枚举。
    $sampleWorkers = @(Get-ActiveWorkers -Workers @(Get-OptionalProperty -Object $sample -Name 'workers'))
    $count = @(Get-WorkerDiskIds -Workers $sampleWorkers).Count
    $peakConcurrentDisks = [Math]::Max($peakConcurrentDisks, $count)
}

$failureRows = @(
    $runtimeSamples | ForEach-Object { @($_.failures) } |
        Where-Object { $null -ne $_ -and $null -ne $_.PSObject.Properties['message'] }
)
$failureGroups = @($failureRows | Group-Object -Property stage_id, display_path, message | Sort-Object Count -Descending)
$repeatFailure = $failureGroups | Where-Object {
    $_.Count -gt [Math]::Max(20, [int]($runtimeSamples.Count / 2))
} | Select-Object -First 1
$physicalFaults = @($failureRows | Where-Object { $_.message -match '物理|timeout|超时|读取' }).Count
$workerCrashes = @($failureRows | Where-Object { $_.message -match 'Worker|崩溃|exit' }).Count

$executionConfigs = @(
    $runtimeSamples | ForEach-Object { Get-OptionalProperty -Object $_ -Name 'execution_config' } |
        Where-Object { $null -ne $_ }
)
$executionConfig = $executionConfigs | Select-Object -Last 1
$pipelineSamples = @(
    $runtimeSamples | ForEach-Object { Get-OptionalProperty -Object $_ -Name 'pipeline_metrics' } |
        Where-Object { $null -ne $_ }
)

$metricDefinitions = @(
    [pscustomobject]@{ Label = 'Hash队列'; Field = 'hash_queue'; Kind = '队列' },
    [pscustomobject]@{ Label = '路径缓存队列'; Field = 'path_cache_queue'; Kind = '队列' },
    [pscustomobject]@{ Label = '内容缓存队列'; Field = 'content_cache_queue'; Kind = '队列' },
    [pscustomobject]@{ Label = '待解码队列'; Field = 'decode_queue'; Kind = '队列' },
    [pscustomobject]@{ Label = '持久化队列'; Field = 'persist_queue'; Kind = '队列' },
    [pscustomobject]@{ Label = 'Hash磁盘许可'; Field = 'hash_io'; Kind = '资源' },
    [pscustomobject]@{ Label = '媒体磁盘许可'; Field = 'media_io'; Kind = '资源' },
    [pscustomobject]@{ Label = 'CPU权重'; Field = 'cpu_weight'; Kind = '资源' },
    [pscustomobject]@{ Label = 'Worker槽'; Field = 'worker_slots'; Kind = '资源' }
)
$metricLines = [Collections.Generic.List[string]]::new()
$capacityViolations = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
foreach ($definition in $metricDefinitions) {
    $rows = @(
        $pipelineSamples | ForEach-Object {
            Get-OptionalProperty -Object $_ -Name $definition.Field
        } | Where-Object { $null -ne $_ }
    )
    $currentMax = Get-MaxOptionalProperty -Rows $rows -Name 'current'
    $peakMax = Get-MaxOptionalProperty -Rows $rows -Name 'peak'
    $capacity = Get-MaxOptionalProperty -Rows $rows -Name 'capacity'
    $waitP95 = Get-MaxOptionalProperty -Rows @(
        $rows | ForEach-Object { Get-OptionalProperty -Object $_ -Name 'wait_latency' } |
            Where-Object { $null -ne $_ }
    ) -Name 'p95_ms'
    $serviceP95 = Get-MaxOptionalProperty -Rows @(
        $rows | ForEach-Object { Get-OptionalProperty -Object $_ -Name 'service_latency' } |
            Where-Object { $null -ne $_ }
    ) -Name 'p95_ms'
    foreach ($row in $rows) {
        $rowCurrent = Get-OptionalProperty -Object $row -Name 'current'
        $rowPeak = Get-OptionalProperty -Object $row -Name 'peak'
        $rowCapacity = Get-OptionalProperty -Object $row -Name 'capacity'
        if ($null -ne $rowCapacity -and
            (($null -ne $rowCurrent -and [double]$rowCurrent -gt [double]$rowCapacity) -or
             ($null -ne $rowPeak -and [double]$rowPeak -gt [double]$rowCapacity))) {
            $null = $capacityViolations.Add($definition.Label)
        }
    }
    $metricLines.Add("| $($definition.Label) | $($definition.Kind) | $(Format-Optional $currentMax) | $(Format-Optional $peakMax) | $(Format-Optional $capacity) | $(Format-Optional $waitP95 ' ms') | $(Format-Optional $serviceP95 ' ms') |")
}

$ownershipDefinitions = @(Get-OwnershipDefinitionRows)
$ownershipMissing = [Collections.Generic.List[string]]::new()
$ownershipCapacityViolations = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
$ownershipInvariantViolations = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
$ownershipCurrentSums = @{}
$ownershipPeakSums = @{}
foreach ($definition in $ownershipDefinitions) {
    $rows = @(
        $pipelineSamples | ForEach-Object { Get-OptionalProperty -Object $_ -Name $definition.Label } |
            Where-Object { $null -ne $_ }
    )
    $required = ($definition.Label -ne 'content_output_credit_owned' -and
        $definition.Label -ne 'hash_refill_token_available' -and $definition.Label -ne 'decode_credit_owned')
    if ($variant -eq 'B') { $required = $true }
    if ($required -and $rows.Count -eq 0) { $ownershipMissing.Add($definition.Label) }
    foreach ($row in $rows) {
        $rowCurrent = Get-OptionalProperty -Object $row -Name 'current'
        $rowPeak = Get-OptionalProperty -Object $row -Name 'peak'
        $rowCapacity = Get-OptionalProperty -Object $row -Name 'capacity'
        if ($required -and ($null -eq $rowCurrent -or $null -eq $rowPeak -or $null -eq $rowCapacity)) {
            if (-not $ownershipMissing.Contains($definition.Label)) { $ownershipMissing.Add($definition.Label) }
        }
        if ($null -ne $rowCapacity -and (($null -ne $rowCurrent -and [double]$rowCurrent -gt [double]$rowCapacity) -or
            ($null -ne $rowPeak -and [double]$rowPeak -gt [double]$rowCapacity))) {
            [void]$ownershipCapacityViolations.Add($definition.Label)
        }
    }
    # A 基线允许 credit/control 字段缺失；保留 null，报告显示 —，不从聚合队列反推零。
    $ownershipCurrentSums[$definition.Label] = Get-MaxOptionalProperty -Rows $rows -Name 'current'
    $ownershipPeakSums[$definition.Label] = Get-MaxOptionalProperty -Rows $rows -Name 'peak'
}
$latencyRows = @(
    $pipelineSamples | ForEach-Object { Get-OptionalProperty -Object $_ -Name 'item_completion_latency' } |
        Where-Object { $null -ne $_ }
)
if ($latencyRows.Count -eq 0) { $ownershipMissing.Add('item_completion_latency') }
$nonControlOwnershipFields = @($ownershipDefinitions | Where-Object { -not $_.IsControl } | Select-Object -ExpandProperty Label)
$ownershipCurrentSum = 0.0
$ownershipPeakSum = 0.0
foreach ($name in $nonControlOwnershipFields) {
    if ($null -ne $ownershipCurrentSums[$name]) { $ownershipCurrentSum += [double]$ownershipCurrentSums[$name] }
    if ($null -ne $ownershipPeakSums[$name]) { $ownershipPeakSum += [double]$ownershipPeakSums[$name] }
}
$refillTokenCurrent = $ownershipCurrentSums['hash_refill_token_available']
$creditViolations = [Collections.Generic.List[string]]::new()
if ($variant -eq 'B') {
    foreach ($name in @('content_output_credit_owned', 'decode_credit_owned')) {
        if ($ownershipMissing -contains $name) { continue }
        if ($ownershipCapacityViolations.Contains($name)) { $creditViolations.Add($name) }
    }
    foreach ($name in @('content_output_credit_capacity', 'decode_credit_balance', 'decode_credit_capacity')) {
        if ($ownershipInvariantViolations.Contains($name)) { $creditViolations.Add($name) }
    }
}
$hashOwnershipFields = @('hash_waiting_permit', 'hash_reading', 'hash_completed_unjoined')
$mediaOwnershipFields = @('media_permit_waiting', 'media_acquire_ready')
$workerAdmissionFields = @('worker_dispatching', 'worker_start_pending', 'worker_decode', 'worker_feature', 'worker_result_wait', 'worker_phase_unknown', 'media_permit_waiting', 'media_acquire_ready')
foreach ($sample in $runtimeSamples) {
    $samplePipeline = Get-OptionalProperty -Object $sample -Name 'pipeline_metrics'
    if ($null -eq $samplePipeline) { continue }
    foreach ($group in @(
        [pscustomobject]@{ Label = 'hash_ownership'; Fields = $hashOwnershipFields; CapacityField = 'hash_queue'; Fallback = 'hash_tasks' }
        [pscustomobject]@{ Label = 'media_ownership'; Fields = $mediaOwnershipFields; CapacityField = 'media_io'; Fallback = 'global_disk_permits' }
        [pscustomobject]@{ Label = 'worker_admission'; Fields = $workerAdmissionFields; CapacityField = 'worker_slots'; Fallback = 'worker_slots' }
    )) {
        $sum = 0.0
        foreach ($field in $group.Fields) {
            $metric = Get-OptionalProperty -Object $samplePipeline -Name $field
            $sum += [double](Get-NumberOrZero (Get-OptionalProperty -Object $metric -Name 'current'))
        }
        $capacityMetric = Get-OptionalProperty -Object $samplePipeline -Name $group.CapacityField
        $capacity = Get-OptionalProperty -Object $capacityMetric -Name 'capacity'
        if ($null -eq $capacity) {
            $execution = Get-OptionalProperty -Object $sample -Name 'execution_config'
            $capacity = Get-OptionalProperty -Object $execution -Name $group.Fallback
        }
        if ($null -ne $capacity -and $sum -gt [double]$capacity) {
            [void]$ownershipCapacityViolations.Add($group.Label)
        }
    }

    # 对协议定义的精确关系做逐样本检查；缺字段时由字段覆盖门禁标记 INCONCLUSIVE。
    $hashValues = @{
        waiting = Get-PipelineCurrent -Pipeline $samplePipeline -Name 'hash_waiting_permit'
        reading = Get-PipelineCurrent -Pipeline $samplePipeline -Name 'hash_reading'
        completed = Get-PipelineCurrent -Pipeline $samplePipeline -Name 'hash_completed_unjoined'
    }
    $hashQueueCurrent = Get-PipelineCurrent -Pipeline $samplePipeline -Name 'hash_queue'
    if ((Test-MetricValuesComplete -Values $hashValues) -and $null -ne $hashQueueCurrent -and
        (($hashValues.Values | ForEach-Object { [double]$_ } | Measure-Object -Sum).Sum -ne [double]$hashQueueCurrent)) {
        [void]$ownershipInvariantViolations.Add('hashing_balance')
    }

    $mediaValues = @{
        waiting = Get-PipelineCurrent -Pipeline $samplePipeline -Name 'media_permit_waiting'
        ready = Get-PipelineCurrent -Pipeline $samplePipeline -Name 'media_acquire_ready'
        permitReady = Get-PipelineCurrent -Pipeline $samplePipeline -Name 'media_permit_ready'
    }
    if (Test-MetricValuesComplete -Values $mediaValues) {
        if ([double]$mediaValues.permitReady -gt [double]$mediaValues.ready) {
            [void]$ownershipInvariantViolations.Add('media_permit_ready_subset')
        }
    }

    $activeValues = @{
        startPending = Get-PipelineCurrent -Pipeline $samplePipeline -Name 'worker_start_pending'
        decode = Get-PipelineCurrent -Pipeline $samplePipeline -Name 'worker_decode'
        feature = Get-PipelineCurrent -Pipeline $samplePipeline -Name 'worker_feature'
        resultWait = Get-PipelineCurrent -Pipeline $samplePipeline -Name 'worker_result_wait'
        unknown = Get-PipelineCurrent -Pipeline $samplePipeline -Name 'worker_phase_unknown'
    }
    if (Test-MetricValuesComplete -Values $activeValues) {
        $activeSum = ($activeValues.Values | ForEach-Object { [double]$_ } | Measure-Object -Sum).Sum
        if ($null -ne $sample.PSObject.Properties['workers']) {
            $sampleWorkers = @(Get-ActiveWorkers -Workers @(Get-OptionalProperty -Object $sample -Name 'workers'))
            if ($sampleWorkers.Count -ne [int]$activeSum) {
                [void]$ownershipInvariantViolations.Add('active_balance')
            }
        }
        $mediaAcquiring = [double]$mediaValues.waiting + [double]$mediaValues.ready
        $dispatching = [double](Get-PipelineCurrent -Pipeline $samplePipeline -Name 'worker_dispatching')
        if ($null -ne $dispatching) {
            $workerAdmission = $activeSum + $mediaAcquiring + $dispatching
            $workerCapacityMetric = Get-OptionalProperty -Object $samplePipeline -Name 'worker_slots'
            $workerCapacity = Get-OptionalProperty -Object $workerCapacityMetric -Name 'capacity'
            if ($null -eq $workerCapacity) {
                $workerCapacity = Get-OptionalProperty -Object (Get-OptionalProperty -Object $sample -Name 'execution_config') -Name 'worker_slots'
            }
            if ($null -ne $workerCapacity -and $workerAdmission -gt [double]$workerCapacity) {
                [void]$ownershipInvariantViolations.Add('worker_admission_capacity')
            }
        }
    }

    $decodeCreditCurrent = Get-PipelineCurrent -Pipeline $samplePipeline -Name 'decode_credit_owned'
    $decodeQueueCurrent = Get-PipelineCurrent -Pipeline $samplePipeline -Name 'decode_queue'
    $dispatchCurrent = Get-PipelineCurrent -Pipeline $samplePipeline -Name 'worker_dispatching'
    if ($null -ne $decodeCreditCurrent -and $null -ne $decodeQueueCurrent -and
        (Test-MetricValuesComplete -Values $mediaValues) -and $null -ne $dispatchCurrent -and
        $null -ne $activeValues.startPending) {
        $expectedDecodeCredit = [double]$decodeQueueCurrent + [double]$mediaValues.waiting +
            [double]$mediaValues.ready + [double]$dispatchCurrent + [double]$activeValues.startPending
        if ([double]$decodeCreditCurrent -ne $expectedDecodeCredit) {
            [void]$ownershipInvariantViolations.Add('decode_credit_balance')
        }
    }

    $executionForSample = Get-OptionalProperty -Object $sample -Name 'execution_config'
    $contentCreditCurrent = Get-PipelineCurrent -Pipeline $samplePipeline -Name 'content_output_credit_owned'
    $contentCapacity = Get-PipelineCapacity -Pipeline $samplePipeline -Name 'content_cache_queue'
    if ($null -eq $contentCapacity) {
        $contentCapacity = Get-OptionalProperty -Object $executionForSample -Name 'content_cache_queue_capacity'
    }
    if ($null -ne $contentCreditCurrent -and $null -ne $contentCapacity -and
        [double]$contentCreditCurrent -gt [double]$contentCapacity) {
        [void]$ownershipInvariantViolations.Add('content_output_credit_capacity')
    }

    $refillTokenCurrent = Get-PipelineCurrent -Pipeline $samplePipeline -Name 'hash_refill_token_available'
    $hashCapacity = Get-PipelineCapacity -Pipeline $samplePipeline -Name 'hash_queue'
    if ($null -eq $hashCapacity) {
        $hashCapacity = Get-OptionalProperty -Object $executionForSample -Name 'hash_tasks'
    }
    if ($null -ne $refillTokenCurrent -and $null -ne $hashCapacity -and
        [double]$refillTokenCurrent -gt [double]$hashCapacity) {
        [void]$ownershipInvariantViolations.Add('hash_refill_token_capacity')
    }

    if ($null -ne $decodeCreditCurrent) {
        $workerLimit = Get-OptionalProperty -Object $executionForSample -Name 'worker_slots'
        if ($null -eq $workerLimit) { $workerLimit = $effectiveWorkers }
        if ([double]$decodeCreditCurrent -gt (2.0 * [double]$workerLimit)) {
            [void]$ownershipInvariantViolations.Add('decode_credit_capacity')
        }
    }
}
if ($variant -eq 'B') {
    # 守恒关系在逐样本扫描后才确定，单独回填 B credit 门禁展示，避免只显示 PASS。
    foreach ($name in @('content_output_credit_capacity', 'decode_credit_balance', 'decode_credit_capacity')) {
        if ($ownershipInvariantViolations.Contains($name) -and -not $creditViolations.Contains($name)) {
            $creditViolations.Add($name)
        }
    }
}
$terminalPipeline = Get-OptionalProperty -Object $runtimeSamples[-1] -Name 'pipeline_metrics'
$terminalNonZeroOwnership = [Collections.Generic.List[string]]::new()
foreach ($definition in $ownershipDefinitions | Where-Object { -not $_.IsControl }) {
    $terminalMetric = if ($null -eq $terminalPipeline) { $null } else { Get-OptionalProperty -Object $terminalPipeline -Name $definition.Label }
    $terminalCurrent = if ($null -eq $terminalMetric) { $null } else { Get-OptionalProperty -Object $terminalMetric -Name 'current' }
    if ($null -ne $terminalCurrent -and [double]$terminalCurrent -ne 0) {
        $terminalNonZeroOwnership.Add($definition.Label)
    }
}
$terminalTokenMetric = if ($null -eq $terminalPipeline) { $null } else { Get-OptionalProperty -Object $terminalPipeline -Name 'hash_refill_token_available' }
$terminalTokenCurrent = if ($null -eq $terminalTokenMetric) { $null } else { Get-OptionalProperty -Object $terminalTokenMetric -Name 'current' }

$workerPhaseLines = [Collections.Generic.List[string]]::new()
$phaseWorkers = @(
    $workerSnapshots | Where-Object {
        $phase = Get-OptionalProperty -Object $_ -Name 'phase'
        -not [string]::IsNullOrWhiteSpace([string]$phase)
    }
)
foreach ($group in $phaseWorkers | Group-Object -Property phase | Sort-Object Name) {
    $maxCpu = Get-MaxOptionalProperty -Rows @($group.Group) -Name 'cpu_weight'
    $maxDecoder = Get-MaxOptionalProperty -Rows @($group.Group) -Name 'decoder_threads'
    $workerPhaseLines.Add("| $($group.Name) | $($group.Count) | $(Format-Optional $maxCpu) | $(Format-Optional $maxDecoder) |")
}
if ($workerPhaseLines.Count -eq 0) {
    $workerPhaseLines.Add('| — | 0 | — | — |')
}

$deadlineCancelledId = [string](Get-OptionalProperty -Object $runtimeResult -Name 'deadline_cancelled_persistent_task_id')
$scanTasks = if ($runtimeEvidence.Valid) { @($runtimeEvidence.ScanTasks) } else { @() }
$nonDeadlineCancelledScans = @(
    $scanTasks | Where-Object {
        [string](Get-OptionalProperty -Object $_ -Name 'terminal_state') -eq 'cancelled' -and
        ([string]::IsNullOrWhiteSpace($deadlineCancelledId) -or
            [string](Get-OptionalProperty -Object $_ -Name 'persistent_task_id') -cne $deadlineCancelledId)
    }
)
$failedScanTasks = @($scanTasks | Where-Object { [string](Get-OptionalProperty -Object $_ -Name 'terminal_state') -eq 'failed' })
$cacheWaitResourceOwnershipViolations = @(
    $failureRows | Where-Object { [string]$_.message -like '*CACHE_WAIT_RESOURCE_OWNERSHIP_VIOLATION*' }
).Count
$declaredIntervalMismatches = @(
    $runtimeWeights | Where-Object {
        $_.WeightMs -gt 0 -and $_.DeclaredIntervalMs -gt 0 -and [Math]::Abs($_.WeightMs - $_.DeclaredIntervalMs) -gt 500
    }
).Count
$systemIntervalMismatches = @(
    $systemWeights | Where-Object {
        $_.WeightMs -gt 0 -and $_.DeclaredIntervalMs -gt 0 -and [Math]::Abs($_.WeightMs - $_.DeclaredIntervalMs) -gt 500
    }
).Count

$failReasons = [Collections.Generic.List[string]]::new()
$inconclusiveReasons = [Collections.Generic.List[string]]::new()
$diskReadEvidence.HardFailures | ForEach-Object { $failReasons.Add([string]$_) }
$diskReadEvidence.MissingReasons | ForEach-Object { $inconclusiveReasons.Add([string]$_) }
$schemaCheck.Errors | ForEach-Object { $inconclusiveReasons.Add("harness-result schema2 无效：$_") }
$runtimeEvidence.Errors | ForEach-Object { $inconclusiveReasons.Add("RUST_V2_RUNTIME_RESULT_EVIDENCE_INVALID:$_") }
if (-not $physicalDiskMapEvidence.Valid) { $inconclusiveReasons.Add($physicalDiskMapEvidence.Diagnostic) }
if ($null -ne $mediaEvidenceClosure) {
    $mediaEvidenceClosure.Errors | ForEach-Object { $inconclusiveReasons.Add([string]$_) }
}
if (-not [string]::IsNullOrWhiteSpace($manifestSchema)) {
    if ($manifestSchema -ne 'rust-v2-media-manifest/v2' -or $manifestRoots.Count -lt 2) {
        $inconclusiveReasons.Add('RUST_V2_RUNTIME_MEDIA_MANIFEST_V2_INVALID')
    }
    $harnessRoots = @((Get-OptionalProperty -Object $harness -Name 'media_roots'))
    if ($harnessRoots.Count -gt 0 -and $harnessRoots.Count -ne $manifestRoots.Count) {
        $inconclusiveReasons.Add('RUST_V2_RUNTIME_MEDIA_ROOT_BINDING_INVALID')
    }
}
if (-not $summaryBinding.BindingValid) { $inconclusiveReasons.Add($summaryBinding.Diagnostic) }
$harnessRunStatusProperty = $harness.PSObject.Properties['run_status']
$harnessRunStatus = [string](Get-OptionalProperty -Object $harness -Name 'run_status')
if ($null -eq $harnessRunStatusProperty -or [string]::IsNullOrWhiteSpace($harnessRunStatus)) {
    $inconclusiveReasons.Add('RUST_V2_RUNTIME_HARNESS_RUN_STATUS_MISSING')
}
elseif ($harnessRunStatus -eq 'PASS') {
    # 只有 harness 明确 PASS 才允许继续争取最终 PASS。
}
elseif ($harnessRunStatus -eq 'FAIL') {
    $failReasons.Add('RUST_V2_RUNTIME_HARNESS_RUN_STATUS_FAIL')
}
elseif ($harnessRunStatus -eq 'INCONCLUSIVE') {
    $inconclusiveReasons.Add('RUST_V2_RUNTIME_HARNESS_RUN_STATUS_INCONCLUSIVE')
}
else {
    $inconclusiveReasons.Add("RUST_V2_RUNTIME_HARNESS_RUN_STATUS_INVALID:$harnessRunStatus")
}
$missingRuntimeUtc = @($runtimeSamples | Where-Object {
    $null -eq $_.PSObject.Properties['utc_unix_ms'] -or $null -eq (Get-OptionalProperty -Object $_ -Name 'utc_unix_ms')
}).Count
$missingSystemUtc = @($systemSamples | Where-Object {
    $null -eq $_.PSObject.Properties['utc_unix_ms'] -or $null -eq (Get-OptionalProperty -Object $_ -Name 'utc_unix_ms')
}).Count
if ($singleRun) {
    # 单轮必须由 runtime_result 明确确认，并且只允许一项扫描及有限终态，防止截断或额外任务伪装完成。
    if ($null -eq $runtimeSingleRunProperty) {
        $inconclusiveReasons.Add('single_run runtime_result.single_run missing')
    }
    elseif ($runtimeSingleRun -isnot [bool]) {
        $inconclusiveReasons.Add('single_run runtime_result.single_run type invalid')
    }
    elseif (-not $runtimeSingleRun) {
        $failReasons.Add('single_run runtime_result.single_run must be true')
    }
    $scansStarted = Get-OptionalProperty -Object $runtimeResult -Name 'scans_started'
    if ($null -eq $scansStarted) {
        $inconclusiveReasons.Add('single_run 缺少 scans_started')
    }
    elseif ($scansStarted -isnot [byte] -and $scansStarted -isnot [int16] -and
        $scansStarted -isnot [int32] -and $scansStarted -isnot [int64]) {
        $inconclusiveReasons.Add('single_run scans_started type invalid')
    }
    elseif ([int64]$scansStarted -ne 1) {
        $failReasons.Add("single_run 必须恰好启动一次扫描，实际=$scansStarted")
    }
    if ($runtimeEvidence.Valid) {
        if ($scanTasks.Count -ne 1) {
            $failReasons.Add("single_run must contain exactly one scan task, actual=$($scanTasks.Count)")
        }
        else {
            $terminalState = [string](Get-OptionalProperty -Object $scanTasks[0] -Name 'terminal_state')
            if ($terminalState -notin @('completed', 'failed', 'cancelled')) {
                $failReasons.Add("single_run terminal_state invalid: $terminalState")
            }
        }
    }
}
else {
    if ($null -ne $runtimeSingleRunProperty) {
        if ($runtimeSingleRun -isnot [bool]) {
            $inconclusiveReasons.Add('runtime_result.single_run type invalid')
        }
        elseif ($runtimeSingleRun) {
            $inconclusiveReasons.Add('single_run harness/runtime binding mismatch')
        }
    }
    if ($duration -lt $configuredDeadlineSeconds) {
        $failReasons.Add("实际计算窗口仅 $duration 秒，少于$configuredDeadlineSeconds 秒")
    }
}
if (-not $physicalDiskMapEvidence.Valid -and $physicalDiskMapEvidence.Present) {
    $inconclusiveReasons.Add('物理盘映射文件存在但未通过结构或 SHA 校验')
}
if ($missingRuntimeUtc -gt 0 -or $missingSystemUtc -gt 0) {
    $inconclusiveReasons.Add("缺少 utc_unix_ms 时间证据：runtime=$missingRuntimeUtc、system=$missingSystemUtc；禁止回退 elapsed_seconds")
}
if ($maxRuntimeGapMs -gt 2500) { $inconclusiveReasons.Add("runtime 相邻采样最大间隔为 $maxGap 秒，超过2.5秒") }
if ($maxSystemGapMs -gt 6000) { $inconclusiveReasons.Add("system 相邻采样最大间隔为 $maxSystemGap 秒，超过6秒") }
if ($declaredIntervalMismatches -gt 0 -or $systemIntervalMismatches -gt 0) {
    $inconclusiveReasons.Add("采样 utc 间隔与 sample_interval_ms 交叉校验不一致：runtime=$declaredIntervalMismatches、system=$systemIntervalMismatches")
}
if (-not $mediaUnchanged) { $failReasons.Add('真实媒体清单发生变化') }
if ($unexpectedExit) { $failReasons.Add('Node或Worker发生非预期退出') }
if ($failedScans -gt 0 -or $failedScanTasks.Count -gt 0 -or @($runtimeSamples | Where-Object state -eq 'failed').Count -gt 0) {
    $failReasons.Add("发生任务级失败，failed_scans=$failedScans")
}
if ($nonDeadlineCancelledScans.Count -gt 0) { $failReasons.Add('存在非到期主动取消的扫描任务') }
if ($effectiveWorkers -gt 1 -and $runtimeSamples.Count -ge 30 -and $peakWorkers -lt 2) {
    $failReasons.Add("有效Worker为 $effectiveWorkers，但峰值非空闲Worker仅 $peakWorkers")
}
if ($allWorkerDisks.Count -gt 1 -and $peakConcurrentDisks -lt 2) {
    $failReasons.Add('多个物理盘均有工作，但从未观察到重叠读取')
}
if ($repeatFailure) {
    $failReasons.Add("同一运行任务/文件失败疑似无限重复：$($repeatFailure.Name) × $($repeatFailure.Count)")
}
if ([int64](Get-NumberOrZero (($runtimeSamples.overall_completed | Measure-Object -Maximum).Maximum)) -eq 0) {
    $failReasons.Add('30分钟内没有观察到任何文件完成，流水线无有效进度')
}
if ($executionConfigs.Count -eq 0) {
    $failReasons.Add('缺少Node实际执行配置遥测')
}
else {
    $actualWorkerSlots = Get-OptionalProperty -Object $executionConfig -Name 'worker_slots'
    $actualGlobalPermits = Get-OptionalProperty -Object $executionConfig -Name 'global_disk_permits'
    $actualSsdPermits = Get-OptionalProperty -Object $executionConfig -Name 'ssd_per_disk_permits'
    if ($actualWorkerSlots -ne $effectiveWorkers) {
        $failReasons.Add("实际Worker槽位与验收配置不一致：actual=$actualWorkerSlots expected=$effectiveWorkers")
    }
    if ($actualGlobalPermits -ne $readTotalThreads) {
        $failReasons.Add("全局磁盘许可与验收配置不一致：actual=$actualGlobalPermits expected=$readTotalThreads")
    }
    if ($actualSsdPermits -ne $ssdThreads) {
        $failReasons.Add("SSD每盘许可与验收配置不一致：actual=$actualSsdPermits expected=$ssdThreads")
    }
}
if ($pipelineSamples.Count -eq 0) {
    $failReasons.Add('缺少流水线队列与资源遥测')
}
foreach ($name in $capacityViolations) {
    $failReasons.Add("队列峰值超过容量或资源占用越界：$name")
}
foreach ($name in $ownershipCapacityViolations) {
    $failReasons.Add("ownership 峰值超过容量：$name")
}
foreach ($name in $creditViolations) {
    $failReasons.Add("B credit 容量或守恒失败：$name")
}
foreach ($name in $ownershipInvariantViolations) {
    $failReasons.Add("ownership 守恒关系失败：$name")
}
if ($cacheWaitResourceOwnershipViolations -gt 0) {
    $failReasons.Add("cache_wait_resource_ownership_violations=$cacheWaitResourceOwnershipViolations")
}
if ($ownershipMissing.Count -gt 0) {
    $inconclusiveReasons.Add("缺少 ownership 字段：$($ownershipMissing -join ', ')")
}
if (-not $summaryFilesComplete -or -not $summaryPathMatches -or -not $summaryTaskMatches -or
    -not $summaryShaMatches -or -not $summaryBinding.BindingValid -or
    [string]::IsNullOrWhiteSpace($summaryMeta.Status)) {
    $inconclusiveReasons.Add('结果摘要三件套、路径/Task ID、状态或 SHA 缺失/不一致')
}
elseif ($summaryMeta.Status -in @('MISSING', 'INCONCLUSIVE')) {
    $inconclusiveReasons.Add("结果摘要状态为 $($summaryMeta.Status)")
}
if ($terminalNonZeroOwnership.Count -gt 0) {
    $failReasons.Add("终态 ownership 未归零：$($terminalNonZeroOwnership -join ', ')")
}
if ($null -ne $terminalTokenCurrent -and [double]$terminalTokenCurrent -ne 0) {
    $failReasons.Add('终态 hash_refill_token_available 未清零')
}
$runtimeCorrectnessProperty = $runtimeResult.PSObject.Properties['correctness']
$runtimeCorrectness = [string](Get-OptionalProperty -Object $runtimeResult -Name 'correctness')
if ($null -eq $runtimeCorrectnessProperty -or [string]::IsNullOrWhiteSpace($runtimeCorrectness)) {
    $inconclusiveReasons.Add('RUST_V2_RUNTIME_CORRECTNESS_MISSING')
}
elseif ($runtimeCorrectness -eq 'PASS') {
    # runtime_result 正确性明确 PASS 才能进入最终 PASS。
}
elseif ($runtimeCorrectness -eq 'FAIL') {
    $failReasons.Add('RUST_V2_RUNTIME_CORRECTNESS_FAIL')
}
elseif ($runtimeCorrectness -eq 'MISSING' -or $runtimeCorrectness -eq 'INCONCLUSIVE') {
    $inconclusiveReasons.Add("RUST_V2_RUNTIME_CORRECTNESS_$runtimeCorrectness")
}
else {
    $inconclusiveReasons.Add("RUST_V2_RUNTIME_CORRECTNESS_INVALID:$runtimeCorrectness")
}
$verdict = if ($failReasons.Count -gt 0) { 'FAIL' } elseif ($inconclusiveReasons.Count -gt 0) { 'INCONCLUSIVE' } else { 'PASS' }

$stageLines = [Collections.Generic.List[string]]::new()
$stageRows = @($runtimeSamples | ForEach-Object { @($_.stages) })
foreach ($group in $stageRows | Group-Object stage_id | Sort-Object Name) {
    $last = $group.Group | Sort-Object elapsed_ms | Select-Object -Last 1
    $maxSpeed = [double](Get-NumberOrZero (($group.Group.speed_per_second | Measure-Object -Maximum).Maximum))
    $stageLines.Add("| $($last.display_name) ($($last.stage_id)) | $($last.completed) / $($last.total) | $($last.elapsed_ms) | $('{0:N2}' -f $maxSpeed) | $($last.failed) |")
}

$processLines = [Collections.Generic.List[string]]::new()
$processRows = @($systemSamples | ForEach-Object { @($_.processes) })
foreach ($group in $processRows | Group-Object Name | Sort-Object Name) {
    $cpuAverage = [double](Get-NumberOrZero (($group.Group.CpuDeltaMs | Measure-Object -Average).Average))
    $workingPeak = [double](Get-NumberOrZero (($group.Group.WorkingSetBytes | Measure-Object -Maximum).Maximum))
    $privatePeak = [double](Get-NumberOrZero (($group.Group.PrivateMemoryBytes | Measure-Object -Maximum).Maximum))
    $processLines.Add("| $($group.Name) | $('{0:N2}' -f $cpuAverage) | $(Format-Bytes $workingPeak) | $(Format-Bytes $privatePeak) |")
}

$diskLines = [Collections.Generic.List[string]]::new()
$diskRows = @($systemSamples | ForEach-Object { @($_.disks) })
foreach ($group in $diskRows | Group-Object Name | Sort-Object Name) {
    $readAverage = [double](Get-NumberOrZero (($group.Group.DiskReadBytesPerSec | Measure-Object -Average).Average))
    $readPeak = [double](Get-NumberOrZero (($group.Group.DiskReadBytesPerSec | Measure-Object -Maximum).Maximum))
    $queuePeak = [double](Get-NumberOrZero (($group.Group.AvgDiskQueueLength | Measure-Object -Maximum).Maximum))
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
$hardFailureText = if ($failReasons.Count -eq 0) { '- 无。' } else { $failureReasonLines }
$inconclusiveReasonText = if ($inconclusiveReasons.Count -eq 0) {
    '- 无。'
}
else {
    @($inconclusiveReasons | ForEach-Object { "- $_" }) -join "`n"
}
$cleanupCount = [int64](Get-NumberOrZero (Get-OptionalProperty -Object $harness -Name 'disk_full_cleanup_count'))
$contactSheetReuseCount = Get-OptionalProperty -Object $harness -Name 'contact_sheet_reuse_count'
$cleanupText = if ($cleanupCount -eq 0) {
    '本次未触发，不能从本次实测证明清理路径。'
}
else {
    "本次触发 $cleanupCount 次；原始日志需结合删除清单审计。"
}

$hashBytes = Get-MaxOptionalProperty -Rows $pipelineSamples -Name 'hash_bytes'
$workerSlotsText = Format-Optional (Get-OptionalProperty -Object $executionConfig -Name 'worker_slots')
$cpuBudgetText = Format-Optional (Get-OptionalProperty -Object $executionConfig -Name 'cpu_budget')
$hashTasksText = Format-Optional (Get-OptionalProperty -Object $executionConfig -Name 'hash_tasks')
$globalPermitsText = Format-Optional (Get-OptionalProperty -Object $executionConfig -Name 'global_disk_permits')
$hddPermitsText = Format-Optional (Get-OptionalProperty -Object $executionConfig -Name 'hdd_per_disk_permits')
$ssdPermitsText = Format-Optional (Get-OptionalProperty -Object $executionConfig -Name 'ssd_per_disk_permits')
$unknownPermitsText = Format-Optional (Get-OptionalProperty -Object $executionConfig -Name 'unknown_per_disk_permits')
$capacityVerdict = if ($capacityViolations.Count -eq 0 -and $pipelineSamples.Count -gt 0) { 'PASS' } else { 'FAIL' }
$physicalDiskMapLines = if (-not $physicalDiskMapEvidence.Present) {
    @('- 未提供双盘映射（单根 v1 证据）。')
}
elseif (-not $physicalDiskMapEvidence.Valid) {
    @("- 物理盘映射不可用：$($physicalDiskMapEvidence.Diagnostic)")
}
else {
    @($physicalDiskMapEvidence.Entries | ForEach-Object {
        "- $($_.root) → DiskNumber=$($_.disk_number)；盘符=$($_.drive_letter)；分区=$($_.partition_number)；设备=$($_.friendly_name)；总线=$($_.bus_type)"
    })
}
$mediaRootLines = if ($manifestRoots.Count -gt 0) {
    @($manifestRoots | ForEach-Object { "- 媒体根：$_" })
}
else {
    @('- 媒体清单：schema v1 单根。')
}
$runtimeRootClosureText = if ($null -eq $mediaEvidenceClosure) {
    '—（v1 单根证据）'
}
elseif ($mediaEvidenceClosure.RuntimeRootsClosed) {
    "PASS（$(@($mediaEvidenceClosure.RuntimeRoots).Count) 根，runtime_result 与静态清单顺序/路径一致）"
}
else {
    'INCONCLUSIVE（runtime_result.media_roots 与静态清单未闭合）'
}
$workerRootClosureText = if ($null -eq $mediaEvidenceClosure) {
    '—（v1 单根证据）'
}
elseif ($mediaEvidenceClosure.WorkerRootsClosed) {
    "PASS（已观察根：$(@($mediaEvidenceClosure.ObservedWorkerRootIndexes) -join '、')）"
}
else {
    "INCONCLUSIVE（已观察根：$(@($mediaEvidenceClosure.ObservedWorkerRootIndexes) -join '、')）"
}

$productionSeconds = 0.0
$finalizationSeconds = 0.0
$weightedActiveWorkerSeconds = 0.0
$idleWorkerSecondsWhileMediaWaits = 0.0
$resourceBubbleSeconds = 0.0
$workerCpuCores = 0.0
$productionDiskReadBytes = 0.0
# 先按 UTC 将 system 进程/磁盘增量压成一行；报告循环只向前移动游标，避免每个 runtime 样本重复扫描全部 system 样本。
$orderedSystemMetrics = @(
    $systemSamples |
        Sort-Object { Get-SampleTimestampMs -Sample $_ } |
        ForEach-Object {
            $workerCpuDeltaMs = 0.0
            foreach ($process in @((Get-OptionalProperty -Object $_ -Name 'processes'))) {
                if ([string](Get-OptionalProperty -Object $process -Name 'Name') -in @('worker', 'worker.exe')) {
                    $workerCpuDeltaMs += [double](Get-NumberOrZero (Get-OptionalProperty -Object $process -Name 'CpuDeltaMs'))
                }
            }
            $diskReadBytesPerSec = 0.0
            foreach ($disk in @((Get-OptionalProperty -Object $_ -Name 'disks'))) {
                $diskReadBytesPerSec += [double](Get-NumberOrZero (Get-OptionalProperty -Object $disk -Name 'DiskReadBytesPerSec'))
            }
            $timestamp = Get-SampleTimestampMs -Sample $_
            [pscustomobject]@{
                TimestampMs = if ($null -eq $timestamp) { 0.0 } else { $timestamp }
                SampleIntervalMs = [double](Get-NumberOrZero (Get-OptionalProperty -Object $_ -Name 'sample_interval_ms'))
                WorkerCpuDeltaMs = $workerCpuDeltaMs
                DiskReadBytesPerSec = $diskReadBytesPerSec
            }
        }
)
$systemMetricIndex = 0
foreach ($weighted in $runtimeWeights) {
    $dtSeconds = $weighted.WeightMs / 1000.0
    if ($dtSeconds -le 0) { continue }
    $sample = $weighted.Sample
    $stagesForSample = @($sample.stages)
    $baseStage = $stagesForSample | Where-Object { Get-IsBaseComputeStage -Stage $_ } | Select-Object -Last 1
    $isProduction = if ($null -eq $baseStage) { $true } else { Get-IsRunningStage -Stage $baseStage }
    if ($isProduction) { $productionSeconds += $dtSeconds } else { $finalizationSeconds += $dtSeconds }
    $sampleWorkers = @(Get-ActiveWorkers -Workers @(Get-OptionalProperty -Object $sample -Name 'workers'))
    $activeCount = $sampleWorkers.Count
    $weightedActiveWorkerSeconds += $activeCount * $dtSeconds
    $executionForSample = Get-OptionalProperty -Object $sample -Name 'execution_config'
    $workerCapacity = Get-OptionalProperty -Object $executionForSample -Name 'worker_slots'
    if ($null -eq $workerCapacity) { $workerCapacity = $effectiveWorkers }
    $samplePipeline = Get-OptionalProperty -Object $sample -Name 'pipeline_metrics'
    $mediaWaitingMetric = Get-OptionalProperty -Object $samplePipeline -Name 'media_permit_waiting'
    $mediaWaitingCurrent = Get-OptionalProperty -Object $mediaWaitingMetric -Name 'current'
    if ($null -ne $mediaWaitingCurrent -and [double]$mediaWaitingCurrent -gt 0) {
        $idleWorkerSecondsWhileMediaWaits += [Math]::Max(0.0, [double]$workerCapacity - $activeCount) * $dtSeconds
    }
    $hashIo = Get-OptionalProperty -Object (Get-OptionalProperty -Object $samplePipeline -Name 'hash_io') -Name 'current'
    $mediaIo = Get-OptionalProperty -Object (Get-OptionalProperty -Object $samplePipeline -Name 'media_io') -Name 'current'
    $pending = Get-OptionalProperty -Object (Get-OptionalProperty -Object $samplePipeline -Name 'decode_queue') -Name 'current'
    if ($null -ne $pending -and [double]$pending -gt 0 -and $activeCount -eq 0 -and
        [double](Get-NumberOrZero $hashIo) -eq 0 -and [double](Get-NumberOrZero $mediaIo) -eq 0) {
        $resourceBubbleSeconds += $dtSeconds
    }
    while ($systemMetricIndex + 1 -lt $orderedSystemMetrics.Count -and
        [double]$orderedSystemMetrics[$systemMetricIndex + 1].TimestampMs -le [double]$weighted.TimestampMs) {
        $systemMetricIndex++
    }
    if ($orderedSystemMetrics.Count -gt 0) {
        $systemMetric = $orderedSystemMetrics[$systemMetricIndex]
        $systemIntervalMs = [Math]::Max(1.0, [double]$systemMetric.SampleIntervalMs)
        $workerCpuCores += ([double]$systemMetric.WorkerCpuDeltaMs / $systemIntervalMs) * $dtSeconds
        $productionDiskReadBytes += [double]$systemMetric.DiskReadBytesPerSec * $dtSeconds
    }
}
$throughputFilesPerSecond = if ($productionSeconds -gt 0) {
    [double](Get-NumberOrZero (($runtimeSamples.overall_completed | Measure-Object -Maximum).Maximum)) / $productionSeconds
} else { $null }

$report = @"
# Rust V2 Node 单轮真实媒体运行验收

结论：$verdict

## 自动化门禁

- 配置最大截止时间：$configuredDeadlineSeconds 秒
- 实际最后 runtime sample elapsed：$lastRuntimeElapsedText 秒
- 运行模式：$(if ($singleRun) { 'single_run（以一次扫描终态结束）' } else { '持续模式（必须达到最大截止时间）' })
- 实际计算窗口：$duration 秒
- 运行样本：$($runtimeSamples.Count) 条；系统样本：$($systemSamples.Count) 条
- runtime 最大相邻间隔：$maxGap 秒；system 最大相邻间隔：$maxSystemGap 秒
- 首样本权重：0 ms；runtime 加权生产窗口：$('{0:N3}' -f $productionSeconds) 秒；finalization tail：$('{0:N3}' -f $finalizationSeconds) 秒
- 机器 ID：$($machineIds -join '、')
- 运行任务 ID：$($taskIds -join '、')
- 有效 Worker 配置：$effectiveWorkers；非空闲峰值：$peakWorkers；非空闲平均：$('{0:N2}' -f $averageWorkers)

### 已证实硬失败
$hardFailureText

### 证据缺失或不确定
$inconclusiveReasonText

## Node 配置摘要

- 枚举器：Walker（验收客户端专用，只受显式媒体根约束）
- 单块读取超时：3 秒；重试：2 次；块大小：4 MiB
- 请求配置：Worker $effectiveWorkers；HDD $hddThreads/盘、SSD $ssdThreads/盘、未知盘 $unknownThreads/盘、总读取 $readTotalThreads
- 配置与运行数据位于本次隔离目录，重启后运行详情不持久化。

## 实际执行配置

- Worker 槽：$workerSlotsText；CPU 权重预算：$cpuBudgetText；Hash 并发：$hashTasksText
- 全局磁盘许可：$globalPermitsText；HDD/盘：$hddPermitsText；SSD/盘：$ssdPermitsText；未知盘/盘：$unknownPermitsText
- 以上值直接来自 Node 运行详情；缺失字段显示 `—`，不从进程数或默认配置估算。

## 总文件与字节

- 源媒体文件：$($before.FileCount)
- 源媒体字节：$(Format-Bytes ([double]$before.TotalBytes))
- 运行任务最终计数：$($runtimeSamples[-1].overall_completed) / $($runtimeSamples[-1].overall_total)

## 各阶段耗时与吞吐

| 阶段 | 完成/总计 | 已运行毫秒 | 峰值速度/秒 | 失败 |
| --- | ---: | ---: | ---: | ---: |
$($stageLines -join "`n")

## Worker 并行

- 峰值非空闲 Worker：$peakWorkers
- 平均非空闲 Worker：$('{0:N2}' -f $averageWorkers)
- 观察到的物理盘：$($allWorkerDisks -join '、')
- 同时工作的物理盘峰值：$peakConcurrentDisks
- system process_sample_skips：$processSampleSkipCount（仅诊断，不中止连续系统样本裁决）

## 媒体根与物理盘映射

$($mediaRootLines -join "`n")
$($physicalDiskMapLines -join "`n")
- 映射 SHA 校验：$(if (-not $physicalDiskMapEvidence.Present) { '—' } else { $physicalDiskMapEvidence.ShaMatches })
- runtime_result.media_roots 闭合：$runtimeRootClosureText
- Worker display_path 按根覆盖：$workerRootClosureText

## Worker 子阶段

| 显式阶段 | 采样行数 | 最大 CPU 权重 | 最大解码线程 |
| --- | ---: | ---: | ---: |
$($workerPhaseLines -join "`n")

## 流水线运行指标

| 项目 | 类型 | 当前峰值 | 历史峰值 | 硬容量 | 等待 P95 | 服务/持有 P95 |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
$($metricLines -join "`n")

- Hash累计读取：$(if ($null -eq $hashBytes) { '—' } else { Format-Bytes ([double]$hashBytes) })
- 队列容量门禁：$capacityVerdict
- 真实时间加权吞吐：$(Format-Optional $throughputFilesPerSecond ' files/s')
- Worker CPU 核当量：$('{0:N3}' -f $workerCpuCores)；媒体等待空闲 Worker-seconds：$('{0:N3}' -f $idleWorkerSecondsWhileMediaWaits)
- resource bubble seconds：$('{0:N3}' -f $resourceBubbleSeconds)；加权磁盘读取字节：$(Format-Bytes $productionDiskReadBytes)

## 逐物理盘读取许可

| 物理盘 | 容量 | waiting 峰值 | active 峰值 | grant 总数 | release 总数 | 首次 active UTC | 末次 active UTC |
| --- | ---: | ---: | ---: | ---: | ---: | --- | --- |
$($diskReadEvidence.TableLines -join "`n")

- 物理盘映射路径：$(Format-Optional $physicalDiskMapEvidence.Path)
- 物理盘映射 SHA 校验：$(if (-not $physicalDiskMapEvidence.Present) { '—' } else { $physicalDiskMapEvidence.ShaMatches })

## Ownership / credit 守恒

- 非 control-state ownership 当前峰值和：$('{0:N0}' -f $ownershipCurrentSum)；历史峰值和：$('{0:N0}' -f $ownershipPeakSum)
- `hash_refill_token_available`（refill token）当前峰值（不计入 ownership 和）：$(Format-Optional $refillTokenCurrent)
- 字段缺失：$(if ($ownershipMissing.Count -eq 0) { '无' } else { $ownershipMissing -join '、' })
- 容量越界：$(if ($ownershipCapacityViolations.Count -eq 0) { '无' } else { $ownershipCapacityViolations -join '、' })
- B credit 守恒：$(if ($creditViolations.Count -eq 0) { 'PASS' } else { 'FAIL' })
- cache_wait_resource_ownership_violations：$cacheWaitResourceOwnershipViolations
- 终态 ownership 归零：$(if ($terminalNonZeroOwnership.Count -eq 0) { 'PASS' } else { "FAIL（$($terminalNonZeroOwnership -join '、')）" })

## Result summary

- 状态：$(Format-Optional $summaryMeta.Status)
- Task ID：$(Format-Optional $summaryMeta.TaskId)
- canonical：$(Format-Optional $summaryMeta.Path)
- canonical 路径绑定：$summaryPathMatches；Task ID 绑定：$summaryTaskMatches；canonical SHA 校验：$summaryShaMatches；三件套：$summaryFilesComplete
- MISSING：$($summaryMeta.MissingCount)；INCONCLUSIVE：$($summaryMeta.InconclusiveCount)

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
- Worker崩溃观察样本：$workerCrashes（本轮仅记录，不单独作为 CPU/I/O 架构 FAIL 条件）

## 联系表复用

- 本次记录的 MD5 联系表复用数：$(Format-Optional $contactSheetReuseCount)

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
- SSD/HDD 识别结果仅作观察，不属于本轮 CPU/I/O 架构验收门禁。
- 没有发生的故障、磁盘满或崩溃路径不会被写成“已通过实测”。
- 原始证据目录：$EvidenceRoot
"@

$parent = Split-Path -Parent $OutputPath
if ($parent) { New-Item -ItemType Directory -Path $parent -Force | Out-Null }
# 写入 BOM，让远端 Windows PowerShell 5.1 也能按 UTF-8 正确读取中文报告。
[IO.File]::WriteAllText($OutputPath, $report, [Text.UTF8Encoding]::new($true))
Write-Output "RUST_V2_RUNTIME_ACCEPTANCE_REPORT_$verdict"
Write-Output "REPORT_PATH=$OutputPath"
