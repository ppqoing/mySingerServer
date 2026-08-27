<#
.SYNOPSIS
聚合 Rust V2 六轮 CPU/I/O A/B 证据并裁决 PASS、FAIL 或 INCONCLUSIVE。

.DESCRIPTION
报告器只读取 immutable run roots 和 benchmark evidence。确认的业务/性能失败优先于
缺证；所有时间积分使用 runtime 的真实时间戳和 sample_interval_ms，系统样本只匹配
不晚于它的最近 runtime snapshot。
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string] $AbRoot,
    [string] $BenchmarkRoot = '',
    [string] $OutputPath = ''
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$fixedOrder = @('A-1', 'B-1', 'B-2', 'A-2', 'A-3', 'B-3')
$requiredOwnershipFields = @(
    'hash_waiting_permit', 'hash_reading', 'hash_completed_unjoined',
    'media_permit_waiting', 'media_acquire_ready', 'media_permit_ready',
    'worker_dispatching', 'worker_start_pending', 'worker_decode',
    'worker_feature', 'worker_result_wait', 'worker_phase_unknown', 'worker_slots'
)
# item_completion_latency 是 field27；hash_refill_token_available 是 field25，只做 telemetry 覆盖而不参与 ownership 守恒。
$requiredTelemetryFields = @($requiredOwnershipFields + 'item_completion_latency')
$candidateCreditFields = @('content_output_credit_owned', 'hash_refill_token_available', 'decode_credit_owned')

function Get-FullPathSafe {
    <# 统一绝对路径，报告输出不得覆盖 evidence 内已有文件。 #>
    param([string] $Path)
    if ([string]::IsNullOrWhiteSpace($Path)) { return '' }
    return [IO.Path]::GetFullPath($Path).TrimEnd('\')
}

function Test-PathWithin {
    <# 判断报告输出是否落入 immutable evidence root，防止覆盖原始证据。 #>
    param([string] $Candidate, [string] $Root)
    $candidatePath = Get-FullPathSafe $Candidate; $rootPath = Get-FullPathSafe $Root
    if (-not $candidatePath -or -not $rootPath) { return $false }
    return $candidatePath.Equals($rootPath, [StringComparison]::OrdinalIgnoreCase) -or
        $candidatePath.StartsWith(($rootPath + '\'), [StringComparison]::OrdinalIgnoreCase)
}

function Get-Prop {
    <# 安全读取 JSON 字段，缺失返回 null 以便归类证据不足。 #>
    param($Object, [string] $Name)
    if ($null -eq $Object) { return $null }
    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property) { return $null }
    return $property.Value
}

function Get-Num {
    <# 将 JSON 数字转换为 double；非法/缺失保持 null，不把零当作缺失。 #>
    param($Value)
    if ($null -eq $Value -or [string]::IsNullOrWhiteSpace([string]$Value)) { return $null }
    $number = 0.0
    if ([double]::TryParse([string]$Value, [Globalization.NumberStyles]::Float, [Globalization.CultureInfo]::InvariantCulture, [ref]$number)) { return $number }
    return $null
}

function Get-ShaOrNull {
    <# 读取实际文件 SHA，发现 canonical 文件变化时形成 confirmed failure。 #>
    param([string] $Path)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $null }
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Get-JsonSemanticSha256 {
    <# 对解析后的 JSON 对象按 Task6 语义序列化后计算 SHA，忽略 pretty/换行差异。 #>
    param($Object)
    if ($null -eq $Object) { return $null }
    $text = $Object | ConvertTo-Json -Depth 32 -Compress
    $bytes = [Text.UTF8Encoding]::new($false).GetBytes($text)
    $sha = [Security.Cryptography.SHA256]::Create()
    try { return ([BitConverter]::ToString($sha.ComputeHash($bytes))).Replace('-', '').ToLowerInvariant() }
    finally { $sha.Dispose() }
}

function Get-NormalizedFileSha256 {
    <# 规范化配置换行后计算 SHA，验证 metadata 与 release 实物一致。 #>
    param([string] $Path)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $null }
    $text = [IO.File]::ReadAllText($Path); $normalized = ($text -replace "`r`n", "`n") -replace "`r", "`n"
    $bytes = [Text.UTF8Encoding]::new($false).GetBytes($normalized); $sha = [Security.Cryptography.SHA256]::Create()
    try { return ([BitConverter]::ToString($sha.ComputeHash($bytes))).Replace('-', '').ToLowerInvariant() } finally { $sha.Dispose() }
}

function Read-JsonLines {
    <# 逐行读取 NDJSON；坏行会进入 evidence missing，而不是跳过。 #>
    param([string] $Path)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return [pscustomobject]@{ Rows = @(); Error = "MISSING:$Path" } }
    $rows = [Collections.Generic.List[object]]::new(); $lineNumber = 0
    foreach ($line in Get-Content -LiteralPath $Path) {
        $lineNumber++
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        try { [void]$rows.Add(($line | ConvertFrom-Json)) } catch { return [pscustomobject]@{ Rows = @($rows); Error = "INVALID_NDJSON:${Path}:$lineNumber" } }
    }
    return [pscustomobject]@{ Rows = @($rows); Error = $null }
}

function Get-TimeMs {
    <# 支持客户端的 utc_unix_ms、timestamp_utc 和旧 fixture 的 utc 字段。 #>
    param($Row)
    $unix = Get-Num (Get-Prop $Row 'utc_unix_ms')
    if ($null -ne $unix) { return $unix }
    foreach ($name in @('applied_ack_utc_ms', 'applied_ack_timestamp_ms', 'ack_utc_unix_ms')) {
        $ack = Get-Num (Get-Prop $Row $name)
        if ($null -ne $ack) { return $ack }
    }
    foreach ($name in @('timestamp_utc', 'utc', 'timestamp')) {
        $text = [string](Get-Prop $Row $name)
        if ($text) {
            try { return ([DateTimeOffset]::Parse($text, [Globalization.CultureInfo]::InvariantCulture, [Globalization.DateTimeStyles]::AssumeUniversal)).ToUnixTimeMilliseconds() } catch { }
        }
    }
    $elapsed = Get-Num (Get-Prop $Row 'elapsed_ms')
    if ($null -ne $elapsed) { return $elapsed }
    $seconds = Get-Num (Get-Prop $Row 'elapsed_seconds')
    if ($null -ne $seconds) { return $seconds * 1000.0 }
    return $null
}

function Get-WeightedRuntime {
    <# 为每个 runtime snapshot 计算真实时间权重；首样本权重严格为零。 #>
    param([object[]] $Samples)
    $ordered = @($Samples | Where-Object { $null -ne (Get-TimeMs $_) } | Sort-Object { Get-TimeMs $_ })
    $result = [Collections.Generic.List[object]]::new()
    for ($i = 0; $i -lt $ordered.Count; $i++) {
        $current = [double](Get-TimeMs $ordered[$i]); $next = $null
        if ($i + 1 -lt $ordered.Count) { $next = [double](Get-TimeMs $ordered[$i + 1]) }
        $declared = Get-Num (Get-Prop $ordered[$i] 'sample_interval_ms')
        $weight = if ($i -eq 0) { 0.0 } elseif ($next -ne $null -and $next -gt $current) { $next - $current } elseif ($declared -ne $null -and $declared -gt 0) { $declared } else { 0.0 }
        [void]$result.Add([pscustomobject]@{ Sample = $ordered[$i]; TimestampMs = $current; WeightMs = $weight })
    }
    return @($result)
}

function Get-StageState {
    <# 按 proto 枚举 WAITING=1、RUNNING=2、COMPLETED=3、FAILED=4、SKIPPED=5 识别生产窗口。 #>
    param($Sample)
    $stages = @((Get-Prop $Sample 'stages'))
    $stage = $stages | Where-Object {
        $id = [string](Get-Prop $_ 'stage_id')
        $id -eq 'ComputeBaseFeatures'
    } | Select-Object -Last 1
    if ($null -eq $stage) { return 'missing' }
    $status = [string](Get-Prop $stage 'status')
    if ($status) {
        $normalized = $status.ToLowerInvariant()
        if ($normalized -eq 'running') { return 'running' }
        if ($normalized -eq 'waiting') { return 'waiting' }
        if ($normalized -in @('completed', 'failed', 'skipped', 'cancelled', 'terminal')) { return 'terminal' }
        return 'other'
    }
    $state = Get-Num (Get-Prop $stage 'state')
    if ($state -eq 2) { return 'running' }
    if ($state -in @(3, 4, 5)) { return 'terminal' }
    if ($state -eq 1) { return 'waiting' }
    return 'other'
}

function Get-StageRunning {
    <# 生产窗口只由显式 running 样本构成；completed 后的样本属于 finalization tail。 #>
    param($Sample)
    return (Get-StageState -Sample $Sample) -eq 'running'
}

function Get-Median {
    <# 计算三轮中位数；无值返回 null，不将证据不足伪装为零。 #>
    param([object[]] $Values)
    $numbers = @($Values | ForEach-Object { Get-Num $_ } | Where-Object { $null -ne $_ } | Sort-Object)
    if ($numbers.Count -eq 0) { return $null }
    $middle = [int][Math]::Floor($numbers.Count / 2)
    if (($numbers.Count % 2) -eq 1) { return [double]$numbers[$middle] }
    return ([double]$numbers[$middle - 1] + [double]$numbers[$middle]) / 2.0
}

function Get-WeightedQuantile {
    <# 按时间权重计算经验分位数，适用于磁盘读取队列和 I/O 分桶阈值。 #>
    param([object[]] $Rows, [string] $ValueProperty, [double] $Quantile)
    $usable = @($Rows | Where-Object { (Get-Num (Get-Prop $_ 'Value')) -ne $null -and (Get-Num (Get-Prop $_ 'Weight')) -gt 0 } | Sort-Object Value)
    if ($usable.Count -eq 0) { return $null }
    $total = ($usable | Measure-Object -Property Weight -Sum).Sum
    if ($total -le 0) { return $null }
    $target = $total * $Quantile; $running = 0.0
    foreach ($row in $usable) { $running += [double]$row.Weight; if ($running -ge $target) { return [double]$row.Value } }
    return [double]$usable[-1].Value
}

function Get-WeightedDiskRows {
    <# 每个 system sample 先合计全部物理盘读取 B/s，保证多盘不会重复放大权重。 #>
    param([object[]] $Samples)
    $ordered = @($Samples | Where-Object { $null -ne (Get-TimeMs $_) } | Sort-Object { Get-TimeMs $_ })
    $rows = [Collections.Generic.List[object]]::new()
    for ($i = 0; $i -lt $ordered.Count; $i++) {
        $time = [double](Get-TimeMs $ordered[$i]); $next = if ($i + 1 -lt $ordered.Count) { [double](Get-TimeMs $ordered[$i + 1]) } else { $null }
        $declared = Get-Num (Get-Prop $ordered[$i] 'sample_interval_ms')
        $explicitWeight = Get-Num (Get-Prop $ordered[$i] '_pair_weight_ms')
        $weight = if ($null -ne $explicitWeight) { $explicitWeight } elseif ($i -eq 0) { 0.0 } elseif ($next -ne $null -and $next -gt $time) { $next - $time } elseif ($declared -ne $null) { $declared } else { 0.0 }
        $readTotal = 0.0; $queueTotal = 0.0; $readSeen = $false; $queueSeen = $false
        foreach ($disk in @((Get-Prop $ordered[$i] 'disks'))) {
            $diskName = [string](Get-Prop $disk 'Name')
            if ($diskName -match '(?i)^\s*_total\s*$') { continue }
            $diskNumber = Get-Num (Get-Prop $disk 'disk_number')
            if ($null -eq $diskNumber) { $diskNumber = Get-Num (Get-Prop $disk 'DiskNumber') }
            $isPhysical = (Get-Prop $disk 'is_physical') -eq $true
            $looksLikeWindowsDisk = $diskName -match '(?i)^\s*\d+\s+[A-Za-z]:|physical|drive'
            if (-not $isPhysical -and $null -eq $diskNumber -and -not $looksLikeWindowsDisk) { continue }
            $read = Get-Num (Get-Prop $disk 'DiskReadBytesPerSec'); if ($null -eq $read) { $read = Get-Num (Get-Prop $disk 'read_bytes_per_sec') }
            $queue = Get-Num (Get-Prop $disk 'AvgDiskQueueLength'); if ($null -eq $queue) { $queue = Get-Num (Get-Prop $disk 'queue_length') }
            if ($null -ne $read) { $readTotal += $read; $readSeen = $true }
            if ($null -ne $queue) { $queueTotal += $queue; $queueSeen = $true }
        }
        if ($readSeen) {
            [void]$rows.Add([pscustomobject]@{ Value = $readTotal; Queue = if ($queueSeen) { $queueTotal } else { $null }; Weight = [Math]::Max(0.0, $weight); TimestampMs = $time })
        }
    }
    return @($rows)
}

function Get-SnapshotPairing {
    <# 将每个 system sample 配对到不晚于它的最近 runtime snapshot，并计算生产覆盖/年龄。 #>
    param([object[]] $RuntimeWeights, [object[]] $SystemSamples)
    $systems = @($SystemSamples | Where-Object { $null -ne (Get-TimeMs $_) } | Sort-Object { Get-TimeMs $_ })
    $runtime = @($RuntimeWeights | Sort-Object TimestampMs)
    $productionWeight = 0.0; $pairedWeight = 0.0; $maxAge = 0.0; $pairedRows = [Collections.Generic.List[object]]::new()
    for ($systemIndex = 0; $systemIndex -lt $systems.Count; $systemIndex++) {
        $system = $systems[$systemIndex]; $systemTime = [double](Get-TimeMs $system)
        $next = if ($systemIndex + 1 -lt $systems.Count) { [double](Get-TimeMs $systems[$systemIndex + 1]) } else { $null }
        $declared = Get-Num (Get-Prop $system 'sample_interval_ms')
        $weight = if ($next -ne $null -and $next -gt $systemTime) { $next - $systemTime } elseif ($declared -ne $null -and $declared -gt 0) { $declared } else { 0.0 }
        # 关键方向：system 只能回看 runtime，未来 runtime 不得覆盖当前样本。
        $candidate = $runtime | Where-Object { [double]$_.TimestampMs -le $systemTime } | Select-Object -Last 1
        $age = if ($null -ne $candidate) { $systemTime - [double]$candidate.TimestampMs } else { $null }
        $production = $null -ne $candidate -and (Get-StageRunning $candidate.Sample)
        if ($production -and $weight -gt 0) { $productionWeight += $weight }
        if ($production -and $age -ne $null -and $age -le 2500 -and $weight -gt 0) { $pairedWeight += $weight; $maxAge = [Math]::Max($maxAge, $age) }
        elseif ($production -and $age -ne $null) { $maxAge = [Math]::Max($maxAge, $age) }
        [void]$pairedRows.Add([pscustomobject]@{ System = $system; Runtime = if ($candidate) { $candidate.Sample } else { $null }; WeightMs = [Math]::Max(0.0, [double]$weight); AgeMs = $age; Production = $production; Valid = $production -and $age -ne $null -and $age -le 2500 })
    }
    $coverage = if ($productionWeight -gt 0) { $pairedWeight / $productionWeight } else { $null }
    [pscustomobject]@{ Rows = @($pairedRows); ProductionWeightMs = $productionWeight; PairedWeightMs = $pairedWeight; Coverage = $coverage; MaxAgeMs = $maxAge }
}

function Get-RequiredTelemetry {
    <# 校验 A/B 新遥测覆盖；A 允许候选 credit 三项为空，B 必须提供。 #>
    param($RuntimeSamples, [string] $Variant)
    $missing = [Collections.Generic.List[string]]::new()
    $required = @($requiredTelemetryFields)
    if ($Variant -eq 'B') { $required += $candidateCreditFields }
    $samples = @($RuntimeSamples | Where-Object { [string](Get-Prop $_ 'record_type') -eq 'runtime_sample' })
    if ($samples.Count -eq 0) { [void]$missing.Add('runtime_sample'); return @($missing) }
    foreach ($name in $required) {
        $found = $false
        foreach ($sample in $samples) {
            $pipeline = Get-Prop $sample 'pipeline_metrics'
            if ($null -ne (Get-Prop $pipeline $name)) { $found = $true; break }
        }
        if (-not $found) { [void]$missing.Add($name) }
    }
    return @($missing)
}

function Get-ExecutionConfigDiagnostics {
    <# 校验 runtime sample 的实际容量映射；不把每轮动态 raw config SHA 当作跨轮指纹。 #>
    param([object[]] $RuntimeSamples, $LogicalConfig)
    $failures = [Collections.Generic.List[string]]::new(); $missing = [Collections.Generic.List[string]]::new()
    $fields = @('hash_tasks','path_cache_queue_capacity','content_cache_queue_capacity','decode_queue_capacity','persist_queue_capacity','worker_slots','cpu_budget','global_disk_permits','hdd_per_disk_permits','ssd_per_disk_permits','unknown_per_disk_permits')
    if ($null -eq $LogicalConfig) { [void]$missing.Add('LOGICAL_CONFIG_MISSING') }
    $samples = @($RuntimeSamples)
    if ($samples.Count -eq 0) { [void]$missing.Add('EXECUTION_CONFIG_RUNTIME_SAMPLE_MISSING'); return [pscustomobject]@{ Failures=@($failures); Missing=@($missing); Fingerprint=$null } }
    $fingerprints = [Collections.Generic.List[string]]::new(); $firstConfig = $null
    foreach ($sample in $samples) {
        $config = Get-Prop $sample 'execution_config'
        if ($null -eq $config) { [void]$missing.Add('EXECUTION_CONFIG_MISSING'); continue }
        if ($null -eq $firstConfig) { $firstConfig = $config }
        $values = [ordered]@{}
        foreach ($field in $fields) {
            $value = Get-Num (Get-Prop $config $field)
            if ($null -eq $value) { [void]$missing.Add("EXECUTION_CONFIG_FIELD_MISSING:$field") } else { $values[$field] = [long]$value }
        }
        if ($values.Count -eq $fields.Count) { [void]$fingerprints.Add((Get-JsonSemanticSha256 -Object $values)) }
    }
    $uniqueFingerprints = @($fingerprints | Select-Object -Unique)
    if ($uniqueFingerprints.Count -gt 1) { [void]$failures.Add('EXECUTION_CONFIG_DRIFT_WITHIN_RUN') }
    if ($null -ne $firstConfig -and $null -ne $LogicalConfig) {
        $mapping = @{
            worker_slots = 'worker_count'; global_disk_permits = 'total_read_threads';
            hdd_per_disk_permits = 'hdd_threads_per_disk'; ssd_per_disk_permits = 'ssd_threads_per_disk'; unknown_per_disk_permits = 'unknown_threads_per_disk'
        }
        foreach ($field in $mapping.Keys) {
            $actual = Get-Num (Get-Prop $firstConfig $field); $expected = Get-Num (Get-Prop $LogicalConfig $mapping[$field])
            if ($null -eq $actual -or $null -eq $expected) { [void]$missing.Add("EXECUTION_CONFIG_MAPPING_MISSING:$field") }
            elseif ($actual -ne $expected) { [void]$failures.Add("EXECUTION_CONFIG_MAPPING_MISMATCH:$field") }
        }
    }
    [pscustomobject]@{ Failures=@($failures | Select-Object -Unique); Missing=@($missing | Select-Object -Unique); Fingerprint=if($uniqueFingerprints.Count -eq 1){$uniqueFingerprints[0]}else{$null} }
}

function Get-PipelineNumber {
    <# 读取 pipeline metric 的 current/peak/capacity 数值。 #>
    param($Sample, [string] $Name, [string] $Field = 'current')
    $pipeline = Get-Prop $Sample 'pipeline_metrics'; $metric = Get-Prop $pipeline $Name
    return Get-Num (Get-Prop $metric $Field)
}

function Get-OwnershipDiagnostics {
    <# 执行 Task6 同源的 ownership/credit 守恒检查，token 明确排除 ownership 总和。 #>
    param($RuntimeSamples, [string] $Variant)
    $failures = [Collections.Generic.List[string]]::new()
    $missing = [Collections.Generic.List[string]]::new()
    # hash_refill_token_available 是补充 token，不属于 ownership 守恒总和。
    $definitions = @($requiredOwnershipFields + $(if ($Variant -eq 'B') { @('content_output_credit_owned', 'decode_credit_owned') } else { @() }))
    foreach ($sample in $RuntimeSamples) {
        $pipeline = Get-Prop $sample 'pipeline_metrics'
        if ($null -eq $pipeline) { [void]$missing.Add('pipeline_metrics'); continue }
        foreach ($name in $definitions) {
            $metric = Get-Prop $pipeline $name
            if ($null -eq $metric) { [void]$missing.Add($name); continue }
            $current = Get-Num (Get-Prop $metric 'current'); $peak = Get-Num (Get-Prop $metric 'peak'); $capacity = Get-Num (Get-Prop $metric 'capacity')
            if ($null -eq $current -or $current -lt 0) { [void]$failures.Add("OWNERSHIP_NEGATIVE:$name") }
            if ($null -ne $peak -and $null -ne $capacity -and $peak -gt $capacity) { [void]$failures.Add("OWNERSHIP_CAPACITY:$name") }
            if ($null -ne $current -and $null -ne $capacity -and $current -gt $capacity) { [void]$failures.Add("OWNERSHIP_CURRENT_CAPACITY:$name") }
        }
        $hashSum = 0.0
        foreach ($name in @('hash_waiting_permit','hash_reading','hash_completed_unjoined')) { $v = Get-PipelineNumber $sample $name; if ($null -ne $v) { $hashSum += $v } }
        $hashQueue = Get-PipelineNumber $sample 'hash_queue'
        if ($null -ne $hashQueue -and $hashSum -ne $hashQueue) { [void]$failures.Add('OWNERSHIP_HASH_BALANCE') }
        $permitReady = Get-PipelineNumber $sample 'media_permit_ready'; $acquireReady = Get-PipelineNumber $sample 'media_acquire_ready'
        if ($null -ne $permitReady -and $null -ne $acquireReady -and $permitReady -gt $acquireReady) { [void]$failures.Add('OWNERSHIP_MEDIA_PERMIT_SUBSET') }
        $activeSum = 0.0
        foreach ($name in @('worker_start_pending','worker_decode','worker_feature','worker_result_wait','worker_phase_unknown')) { $v = Get-PipelineNumber $sample $name; if ($null -ne $v) { $activeSum += $v } }
        $workerCapacity = Get-PipelineNumber $sample 'worker_slots' 'capacity'; $dispatch = Get-PipelineNumber $sample 'worker_dispatching'; $waiting = Get-PipelineNumber $sample 'media_permit_waiting'; $ready = Get-PipelineNumber $sample 'media_acquire_ready'
        if ($null -ne $workerCapacity -and $null -ne $dispatch -and $null -ne $waiting -and $null -ne $ready -and $activeSum + $waiting + $ready + $dispatch -gt $workerCapacity) { [void]$failures.Add('OWNERSHIP_WORKER_ADMISSION_CAPACITY') }
        $decodeCredit = Get-PipelineNumber $sample 'decode_credit_owned'; $decodeQueue = Get-PipelineNumber $sample 'decode_queue'
        if ($Variant -eq 'B' -and $null -ne $decodeCredit -and $null -ne $decodeQueue -and $null -ne $waiting -and $null -ne $ready -and $null -ne $dispatch) {
            $expected = $decodeQueue + $waiting + $ready + $dispatch + (Get-PipelineNumber $sample 'worker_start_pending')
            if ($decodeCredit -ne $expected) { [void]$failures.Add('OWNERSHIP_DECODE_CREDIT_BALANCE') }
        }
    }
    $last = $RuntimeSamples | Select-Object -Last 1; $terminalPipeline = Get-Prop $last 'pipeline_metrics'
    if ($null -ne $terminalPipeline) {
        foreach ($name in ($definitions | Where-Object { $_ -ne 'hash_refill_token_available' })) {
            $current = Get-PipelineNumber $last $name
            if ($null -ne $current -and $current -ne 0) { [void]$failures.Add("OWNERSHIP_TERMINAL_NONZERO:$name") }
        }
    }
    [pscustomobject]@{ Failures = @($failures | Select-Object -Unique); Missing = @($missing | Select-Object -Unique) }
}

function Get-CanonicalSummaryDiagnostics {
    <# 校验 canonical JSONL、metadata、pair lock 三件套及结果字段，不能用 overall counters 代替。 #>
    param([string] $Path, [string] $ExpectedSha = '', [string] $ExpectedTaskId = '', [string] $ExpectedStatus = '', [long] $ExpectedRowCount = -1)
    $failures = [Collections.Generic.List[string]]::new(); $missing = [Collections.Generic.List[string]]::new(); $paths = [Collections.Generic.List[object]]::new(); $pathNames = [Collections.Generic.List[string]]::new()
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { [void]$missing.Add('CANONICAL_FILE_MISSING'); return [pscustomobject]@{ Failures = @($failures); Missing = @($missing); RowCount = 0; Rows = @(); FailedCount = 0; CompletedCount = 0; Sha = $null } }
    $metadataPath = Join-Path (Split-Path -Parent $Path) 'result-summary-meta.json'
    $leasePath = "$Path.pair.lock"
    foreach ($piece in @($metadataPath, $leasePath)) {
        if (-not (Test-Path -LiteralPath $piece -PathType Leaf)) { [void]$missing.Add("RESULT_SUMMARY_THREE_PIECE_MISSING:$piece") }
    }
    $metadataObject = $null; $leaseObject = $null
    if ((Test-Path -LiteralPath $metadataPath -PathType Leaf) -and (Test-Path -LiteralPath $leasePath -PathType Leaf)) {
        try { $metadataObject = Get-Content -LiteralPath $metadataPath -Raw | ConvertFrom-Json } catch { [void]$failures.Add("RESULT_SUMMARY_BINDING_INVALID:$metadataPath") }
        try { $leaseObject = Get-Content -LiteralPath $leasePath -Raw | ConvertFrom-Json } catch { [void]$failures.Add("RESULT_SUMMARY_BINDING_INVALID:$leasePath") }
    }
    $previous = $null; $lineNumber = 0
    foreach ($line in Get-Content -LiteralPath $Path) {
        $lineNumber++; if ([string]::IsNullOrWhiteSpace($line)) { continue }
        try { $row = $line | ConvertFrom-Json } catch { [void]$failures.Add("CANONICAL_INVALID_JSON:$lineNumber"); continue }
        $pathValue = [string](Get-Prop $row 'normalized_path')
        if (-not $pathValue) { [void]$failures.Add("CANONICAL_PATH_MISSING:$lineNumber"); continue }
        if ($pathNames -contains $pathValue) { [void]$failures.Add("CANONICAL_PATH_DUPLICATE:$pathValue") } else { [void]$pathNames.Add($pathValue) }
        if ($null -ne $previous -and [StringComparer]::Ordinal.Compare($previous, $pathValue) -ge 0) { [void]$failures.Add('CANONICAL_PATH_NOT_SORTED') }
        $previous = $pathValue
        foreach ($field in @('md5','media_type','thumbnail_sha256','contact_sheet_sha256')) {
            if ($null -eq $row.PSObject.Properties[$field]) { [void]$missing.Add("CANONICAL_FIELD_MISSING:$field") }
        }
        if ($null -eq $row.PSObject.Properties['terminal_state'] -and $null -eq $row.PSObject.Properties['status']) { [void]$missing.Add('CANONICAL_FIELD_MISSING:status') }
        if ($null -eq $row.PSObject.Properties['feature_payload_sha256'] -and $null -eq $row.PSObject.Properties['feature_payloads']) { [void]$missing.Add('CANONICAL_FIELD_MISSING:feature_payload') }
        [void]$paths.Add($row)
    }
    if ($paths.Count -eq 0) { [void]$missing.Add('CANONICAL_EMPTY') }
    $actualSha = Get-ShaOrNull $Path
    $metadataSha = [string](Get-Prop $metadataObject 'canonical_sha256')
    $leaseSha = [string](Get-Prop $leaseObject 'expected_canonical_sha256')
    if (-not $metadataSha) { [void]$missing.Add('RESULT_SUMMARY_METADATA_CANONICAL_SHA_MISSING') }
    if (-not $leaseSha) { [void]$missing.Add('RESULT_SUMMARY_LEASE_EXPECTED_CANONICAL_SHA_MISSING') }
    if ($actualSha -and $metadataSha -and $actualSha -ne $metadataSha.ToLowerInvariant()) { [void]$failures.Add('RESULT_SUMMARY_METADATA_CANONICAL_SHA_MISMATCH') }
    if ($actualSha -and $leaseSha -and $actualSha -ne $leaseSha.ToLowerInvariant()) { [void]$failures.Add('RESULT_SUMMARY_LEASE_CANONICAL_SHA_MISMATCH') }
    if ($ExpectedSha -and $actualSha -ne $ExpectedSha.ToLowerInvariant()) { [void]$failures.Add('RESULT_SUMMARY_HARNESS_SHA_MISMATCH') }
    # PairLeaseManifest 的真实 schema 没有 expected_task_id；task_id 只从 metadata 与 harness 绑定。
    $metadataTask = [string](Get-Prop $metadataObject 'task_id')
    $metadataStatus = [string](Get-Prop $metadataObject 'status'); $leaseStatus = [string](Get-Prop $leaseObject 'expected_status')
    $metadataRows = Get-Num (Get-Prop $metadataObject 'row_count'); $leaseRows = Get-Num (Get-Prop $leaseObject 'expected_row_count')
    $metadataToken = [string](Get-Prop $metadataObject 'lease_token'); $leaseToken = [string](Get-Prop $leaseObject 'lease_token')
    if (-not $metadataToken -or -not $leaseToken -or $metadataToken -cne $leaseToken) { [void]$failures.Add('RESULT_SUMMARY_LEASE_TOKEN_MISMATCH') }
    if ($ExpectedTaskId -and $metadataTask -cne $ExpectedTaskId) { [void]$failures.Add('RESULT_SUMMARY_TASK_ID_MISMATCH') }
    if ($ExpectedStatus -and ($metadataStatus -ine $ExpectedStatus -or $leaseStatus -ine $ExpectedStatus)) { [void]$failures.Add('RESULT_SUMMARY_STATUS_MISMATCH') }
    if ($null -eq $metadataRows -or $null -eq $leaseRows) { [void]$missing.Add('RESULT_SUMMARY_ROW_COUNT_MISSING') }
    elseif ($metadataRows -ne $paths.Count -or $leaseRows -ne $paths.Count -or ($ExpectedRowCount -ge 0 -and $paths.Count -ne $ExpectedRowCount)) { [void]$failures.Add('RESULT_SUMMARY_ROW_COUNT_MISMATCH') }
    $failed = @($paths | Where-Object { [string](Get-Prop $_ 'status') -match '(?i)fail|error' -or [string](Get-Prop $_ 'terminal_state') -match '(?i)fail|error' }).Count
    $completed = @($paths | Where-Object { [string](Get-Prop $_ 'status') -match '(?i)complete|success|pass' -or [string](Get-Prop $_ 'terminal_state') -match '(?i)complete|success|pass' }).Count
    [pscustomobject]@{ Failures = @($failures | Select-Object -Unique); Missing = @($missing | Select-Object -Unique); RowCount = $paths.Count; Rows = @($paths); FailedCount = $failed; CompletedCount = $completed; Sha = $actualSha }
}

function Get-RawFailureDiagnostics {
    <# 解析 runtime failure details；缺字段不能默认 ownership violation 为零。 #>
    param([object[]] $Runtime)
    $failures = [Collections.Generic.List[string]]::new(); $missing = [Collections.Generic.List[string]]::new(); $seen = $false
    foreach ($row in $Runtime) {
        if ($null -ne $row.PSObject.Properties['failures']) { $seen = $true }
        foreach ($detail in @((Get-Prop $row 'failures'))) {
            $text = (($detail | ConvertTo-Json -Depth 8 -Compress) + ' ' + [string](Get-Prop $detail 'message') + ' ' + [string](Get-Prop $detail 'code'))
            if ($text -match 'CACHE_WAIT_RESOURCE_OWNERSHIP_VIOLATION') { [void]$failures.Add('CACHE_WAIT_RESOURCE_OWNERSHIP_VIOLATION') }
        }
    }
    if (-not $seen) { [void]$missing.Add('FAILURE_DETAILS_MISSING') }
    [pscustomobject]@{ Failures=@($failures | Select-Object -Unique); Missing=@($missing | Select-Object -Unique) }
}

function Get-MediaManifestDiagnostics {
    <# 解析 before/after 并按 Task6 语义 SHA 比较；harness 只作为额外绑定证据。 #>
    param([string] $BeforePath, [string] $AfterPath, [string] $ExpectedPath = '', [string] $ExpectedSha = '')
    $failures=[Collections.Generic.List[string]]::new();$missing=[Collections.Generic.List[string]]::new();$before=$null;$after=$null;$expected=$null
    try { $before=Get-Content -LiteralPath $BeforePath -Raw|ConvertFrom-Json } catch { [void]$missing.Add('MEDIA_BEFORE_INVALID') }
    try { $after=Get-Content -LiteralPath $AfterPath -Raw|ConvertFrom-Json } catch { [void]$missing.Add('MEDIA_AFTER_INVALID') }
    if ($ExpectedPath) { try { $expected=Get-Content -LiteralPath $ExpectedPath -Raw|ConvertFrom-Json } catch { [void]$missing.Add('MEDIA_INPUT_MANIFEST_INVALID') } }
    $beforeSha = Get-JsonSemanticSha256 -Object $before; $afterSha = Get-JsonSemanticSha256 -Object $after; $expectedSemanticSha = Get-JsonSemanticSha256 -Object $expected
    if ($null -eq $beforeSha -or $null -eq $afterSha) { [void]$missing.Add('MEDIA_MANIFEST_SEMANTIC_SHA_MISSING') }
    if ($beforeSha -and $afterSha -and $beforeSha -ne $afterSha) { [void]$failures.Add('MEDIA_MANIFEST_CHANGED') }
    if ($ExpectedPath -and $null -eq $expected) { [void]$missing.Add('MEDIA_INPUT_MANIFEST_MISSING') }
    if ($ExpectedPath -and $expectedSemanticSha -and $beforeSha -and $beforeSha -ne $expectedSemanticSha) { [void]$failures.Add('MEDIA_BEFORE_INPUT_SEMANTIC_MISMATCH') }
    if ($ExpectedPath -and $expectedSemanticSha -and $afterSha -and $afterSha -ne $expectedSemanticSha) { [void]$failures.Add('MEDIA_AFTER_INPUT_SEMANTIC_MISMATCH') }
    if ($ExpectedSha -and ($beforeSha -ne $ExpectedSha.ToLowerInvariant() -or $afterSha -ne $ExpectedSha.ToLowerInvariant())) { [void]$failures.Add('MEDIA_INPUT_SEMANTIC_SHA_MISMATCH') }
    [pscustomobject]@{
        Failures=@($failures | Select-Object -Unique); Missing=@($missing | Select-Object -Unique)
        BeforeSha=$beforeSha; AfterSha=$afterSha; BeforeFileSha=Get-ShaOrNull $BeforePath; AfterFileSha=Get-ShaOrNull $AfterPath; ExpectedSha=$expectedSemanticSha
    }
}

function Get-HistogramP95 {
    <# 合并最后 completed scan 的 histogram bucket counts 后计算 P95。 #>
    param([object[]] $Histograms)
    $buckets = [Collections.Generic.List[object]]::new(); $total = 0.0
    foreach ($histogram in $Histograms) {
        foreach ($bucket in @((Get-Prop $histogram 'buckets'))) {
            $count = Get-Num (Get-Prop $bucket 'count'); if ($null -eq $count) { $count = Get-Num (Get-Prop $bucket 'bucket_count') }
            $upper = Get-Num (Get-Prop $bucket 'upper_bound_ms'); if ($null -eq $upper) { $upper = Get-Num (Get-Prop $bucket 'le_ms') }
            if ($null -ne $count -and $null -ne $upper -and $count -ge 0) { [void]$buckets.Add([pscustomobject]@{ Upper=$upper; Count=$count }); $total += $count }
        }
    }
    if ($buckets.Count -eq 0 -or $total -le 0) { return $null }
    $groups = @($buckets | Group-Object Upper | ForEach-Object { [pscustomobject]@{ Upper=[double]$_.Name; Count=($_.Group | Measure-Object Count -Sum).Sum } } | Sort-Object Upper)
    $target = [Math]::Ceiling($total * .95); $running = 0.0
    foreach ($bucket in $groups) { $running += $bucket.Count; if ($running -ge $target) { return $bucket.Upper } }
    return $groups[-1].Upper
}

function Get-LastItemLatencyP95 {
    <# 每个 runtime task 只取最后一条 completed snapshot，避免每秒重复累计。 #>
    param([object[]] $Samples)
    $histograms = [Collections.Generic.List[object]]::new()
    $missing = [Collections.Generic.List[string]]::new()
    $groups = @($Samples | Group-Object { $id=[string](Get-Prop $_ 'runtime_task_id'); if(-not $id){$id=[string](Get-Prop $_ 'scan_id')}; $id })
    foreach ($group in $groups) {
        $last = @($group.Group | Sort-Object { Get-TimeMs $_ })[-1]
        if ((Get-StageState $last) -eq 'missing') { [void]$missing.Add("ITEM_LATENCY_STAGE_MISSING:$($group.Name)"); continue }
        $histogram = Get-Prop (Get-Prop $last 'pipeline_metrics') 'item_completion_latency'
        if ($null -eq $histogram) { [void]$missing.Add("ITEM_LATENCY_LAST_SNAPSHOT_MISSING:$($group.Name)"); continue }
        [void]$histograms.Add($histogram)
    }
    [pscustomobject]@{ Value = Get-HistogramP95 -Histograms @($histograms); Missing = @($missing | Select-Object -Unique) }
}

function Get-CompletionTailSeconds {
    <# 按每个 runtime_task_id 的 Applied 累计完成样本计算 90% 到最终完成的尾部，再取本轮中位数。 #>
    param([object[]] $Samples)
    $missing = [Collections.Generic.List[string]]::new(); $values = [Collections.Generic.List[double]]::new()
    $groups = @($Samples | Group-Object {
            $id = [string](Get-Prop $_ 'runtime_task_id')
            if (-not $id) { $id = [string](Get-Prop $_ 'scan_id') }
            $id
        })
    if ($groups.Count -eq 0) { [void]$missing.Add('COMPLETION_RUNTIME_TASK_MISSING') }
    foreach ($group in $groups) {
        $id = [string]$group.Name
        if ([string]::IsNullOrWhiteSpace($id)) { [void]$missing.Add('COMPLETION_RUNTIME_TASK_ID_MISSING'); continue }
        $rows = @($group.Group | Sort-Object { Get-TimeMs $_ })
        $knownTotals = @($rows | Where-Object {
                $known = Get-Prop $_ 'overall_total_known'
                ($null -eq $known -or [bool]$known) -and (Get-Num (Get-Prop $_ 'overall_total')) -gt 0
            } | ForEach-Object { Get-Num (Get-Prop $_ 'overall_total') })
        if ($knownTotals.Count -eq 0) { [void]$missing.Add("COMPLETION_TOTAL_MISSING:$id"); continue }
        $total = ($knownTotals | Measure-Object -Maximum).Maximum
        $threshold = [Math]::Ceiling([double]$total * 0.9)
        $first = @($rows | Where-Object {
                $time = Get-TimeMs $_; $completed = Get-Num (Get-Prop $_ 'overall_completed')
                $null -ne $time -and $null -ne $completed -and $completed -ge $threshold
            } | Select-Object -First 1)
        $last = @($rows | Where-Object {
                $time = Get-TimeMs $_; $completed = Get-Num (Get-Prop $_ 'overall_completed')
                $null -ne $time -and $null -ne $completed -and $completed -ge $total
            } | Select-Object -Last 1)
        if ($first.Count -eq 0 -or $last.Count -eq 0) { [void]$missing.Add("COMPLETION_APPLIED_TAIL_MISSING:$id"); continue }
        $firstTime = Get-TimeMs $first[0]; $lastTime = Get-TimeMs $last[0]
        if ($lastTime -lt $firstTime) { [void]$missing.Add("COMPLETION_APPLIED_TIME_ORDER:$id"); continue }
        [void]$values.Add(($lastTime - $firstTime) / 1000.0)
    }
    [pscustomobject]@{ Value = if ($values.Count) { Get-Median -Values @($values) } else { $null }; Missing = @($missing | Select-Object -Unique) }
}

function Get-TaskTerminalDiagnostics {
    <# runtime_result 的每个 scan_tasks 必须有终态；queued/running/空值不能被 harness 计数掩盖。 #>
    param($RuntimeResult)
    $failures = [Collections.Generic.List[string]]::new(); $missing = [Collections.Generic.List[string]]::new()
    $tasksProperty = Get-Prop $RuntimeResult 'scan_tasks'
    if ($null -eq $tasksProperty) { [void]$missing.Add('SCAN_TASKS_MISSING'); return [pscustomobject]@{ Failures=@($failures); Missing=@($missing) } }
    $tasks = @($tasksProperty)
    if ($tasks.Count -eq 0) { [void]$missing.Add('SCAN_TASKS_EMPTY') }
    $deadlineId = [string](Get-Prop $RuntimeResult 'deadline_cancelled_persistent_task_id')
    foreach ($task in $tasks) {
        $state = [string](Get-Prop $task 'terminal_state')
        $persistentId = [string](Get-Prop $task 'persistent_task_id')
        if ([string]::IsNullOrWhiteSpace($state)) { [void]$missing.Add("SCAN_TERMINAL_STATE_MISSING:$persistentId"); continue }
        $normalized = $state.ToLowerInvariant()
        if ($normalized -in @('queued', 'running')) { [void]$failures.Add("QUEUED_OR_RUNNING_REMAINS:$persistentId") }
        elseif ($normalized -eq 'deadline-cancelled') {
            if (-not $deadlineId -or $deadlineId -ne $persistentId) { [void]$failures.Add("UNBOUND_DEADLINE_CANCEL:$persistentId") }
        }
        elseif ($normalized -in @('failed', 'cancelled')) {
            if (-not $deadlineId -or $deadlineId -ne $persistentId) { [void]$failures.Add("NON_DEADLINE_TERMINAL_FAILURE:$persistentId") }
        }
    }
    [pscustomobject]@{ Failures=@($failures | Select-Object -Unique); Missing=@($missing | Select-Object -Unique) }
}

function Get-RuntimeResultDiagnostics {
    <# 以最后一条 runtime_result 为终态事实；fatal/diagnostic/计数不能被 harness 静默覆盖。 #>
    param($RuntimeResult, $Harness)
    $failures = [Collections.Generic.List[string]]::new(); $missing = [Collections.Generic.List[string]]::new()
    if ($null -eq $RuntimeResult) { [void]$missing.Add('RUNTIME_RESULT_MISSING'); return [pscustomobject]@{ Failures=@($failures); Missing=@($missing) } }
    $correctness = [string](Get-Prop $RuntimeResult 'correctness')
    if ([string]::IsNullOrWhiteSpace($correctness)) { [void]$missing.Add('RUNTIME_CORRECTNESS_MISSING') }
    elseif ($correctness -ine 'PASS') {
        if ($correctness -ieq 'FAIL') { [void]$failures.Add('RUNTIME_CORRECTNESS_FAIL') }
        else { [void]$missing.Add("RUNTIME_CORRECTNESS_$($correctness.ToUpperInvariant())") }
    }
    $fatalError = [string](Get-Prop $RuntimeResult 'fatal_error')
    if ($fatalError) {
        if ($fatalError -match '(?i)fail|error|timeout|crash|panic|invalid') { [void]$failures.Add("RUNTIME_FATAL_ERROR:$fatalError") }
        else { [void]$missing.Add("RUNTIME_FATAL_ERROR_PRESENT:$fatalError") }
    }
    $diagnostic = [string](Get-Prop $RuntimeResult 'diagnostic')
    if ($diagnostic) { [void]$missing.Add("RUNTIME_DIAGNOSTIC_PRESENT:$diagnostic") }
    $failedScansValue = Get-Num (Get-Prop $RuntimeResult 'failed_scans')
    if ($null -eq $failedScansValue) { [void]$missing.Add('RUNTIME_FAILED_SCANS_MISSING_OR_INVALID') }
    elseif ($failedScansValue -gt 0) { [void]$failures.Add('FAILED_SCAN') }
    # 新旧 harness 均允许不携带这些字段；一旦携带则必须与 runtime_result 一致。
    foreach ($field in @('failed_scans', 'failed_count')) {
        if ($null -ne $Harness -and $null -ne $Harness.PSObject.Properties[$field]) {
            $harnessFailed = Get-Num (Get-Prop $Harness $field)
            if ($null -eq $harnessFailed -or $null -eq $failedScansValue -or $harnessFailed -ne $failedScansValue) { [void]$failures.Add("RUNTIME_HARNESS_$($field.ToUpperInvariant())_MISMATCH") }
        }
    }
    $runtimeTasks = @((Get-Prop $RuntimeResult 'scan_tasks')); $harnessTasksProperty = if ($null -ne $Harness) { $Harness.PSObject.Properties['scan_tasks'] } else { $null }
    if ($null -ne $harnessTasksProperty) {
        $harnessTasks = @($harnessTasksProperty.Value)
        if ($harnessTasks.Count -ne $runtimeTasks.Count) { [void]$failures.Add('RUNTIME_HARNESS_SCAN_TASK_COUNT_MISMATCH') }
        foreach ($task in $runtimeTasks) {
            $id = [string](Get-Prop $task 'persistent_task_id'); $state = [string](Get-Prop $task 'terminal_state')
            $matching = @($harnessTasks | Where-Object { [string](Get-Prop $_ 'persistent_task_id') -eq $id } | Select-Object -First 1)
            if ($matching.Count -eq 0 -or [string](Get-Prop $matching[0] 'terminal_state') -ine $state) { [void]$failures.Add("RUNTIME_HARNESS_SCAN_TASK_MISMATCH:$id") }
        }
    }
    [pscustomobject]@{ Failures=@($failures | Select-Object -Unique); Missing=@($missing | Select-Object -Unique) }
}

function Get-RunMetrics {
    <# 读取单轮全部原始证据并计算时间窗口、守恒、I/O 和资源指标。 #>
    param([string] $RunRoot, [string] $ExpectedName, $ManifestEntry = $null, [string] $TopLevelMediaPath = '', [string] $TopLevelMediaSha = '', $LogicalConfig = $null)
    $evidence = Join-Path $RunRoot 'evidence'
    $harnessPath = Join-Path $evidence 'harness-result.json'
    $runtimePath = Join-Path $evidence 'runtime.ndjson'
    $systemPath = Join-Path $evidence 'system.ndjson'
    $requiredPaths = @($harnessPath, $runtimePath, $systemPath, (Join-Path $evidence 'media-before.json'), (Join-Path $evidence 'media-after.json'), (Join-Path $evidence 'report.md'))
    $missing = [Collections.Generic.List[string]]::new(); $fail = [Collections.Generic.List[string]]::new()
    $bindingMetadata = $null
    if ($null -ne $ManifestEntry) {
        $metadataPath = [string](Get-Prop $ManifestEntry 'metadata_path')
        if ($metadataPath -and (Test-Path -LiteralPath $metadataPath -PathType Leaf)) { try { $bindingMetadata = Get-Content -LiteralPath $metadataPath -Raw | ConvertFrom-Json } catch { [void]$missing.Add('METADATA_INVALID') } } else { [void]$missing.Add('METADATA_MISSING') }
    }
    if ($null -ne $bindingMetadata) {
        $packagePath = Get-FullPathSafe ([string](Get-Prop $bindingMetadata 'formal_zip_path'))
        if (-not (Test-Path -LiteralPath $packagePath -PathType Leaf)) { [void]$missing.Add('PACKAGE_MISSING') } elseif ((Get-ShaOrNull $packagePath) -ne ([string](Get-Prop $bindingMetadata 'formal_zip_sha256')).ToLowerInvariant()) { [void]$fail.Add('PACKAGE_SHA_MISMATCH') }
        $sidecarPath = Get-FullPathSafe ([string](Get-Prop $bindingMetadata 'formal_zip_sidecar_path')); if (-not (Test-Path -LiteralPath $sidecarPath -PathType Leaf)) { [void]$missing.Add('PACKAGE_SIDECAR_MISSING') } else { $sidecarLine = @(Get-Content -LiteralPath $sidecarPath | Where-Object { $_ -match '^([0-9a-fA-F]{64})\s+' } | Select-Object -First 1); if ($sidecarLine.Count -ne 1 -or $Matches[1].ToLowerInvariant() -ne ([string](Get-Prop $bindingMetadata 'formal_zip_sha256')).ToLowerInvariant()) { [void]$fail.Add('PACKAGE_SIDECAR_MISMATCH') } }
        $releaseRoot = Get-FullPathSafe ([string](Get-Prop $bindingMetadata 'release_root')); $manifestFile = Join-Path $releaseRoot 'manifest\files.sha256'
        if (-not (Test-Path -LiteralPath $manifestFile -PathType Leaf)) { [void]$missing.Add('FORMAL_MANIFEST_MISSING') } elseif ((Get-ShaOrNull $manifestFile) -ne ([string](Get-Prop $bindingMetadata 'formal_manifest_sha256')).ToLowerInvariant()) { [void]$fail.Add('FORMAL_MANIFEST_SHA_MISMATCH') }
        $configPath = Get-FullPathSafe ([string](Get-Prop $bindingMetadata 'runtime_config_path')); if ((Get-NormalizedFileSha256 $configPath) -ne ([string](Get-Prop $bindingMetadata 'runtime_config_sha256')).ToLowerInvariant()) { [void]$fail.Add('RUNTIME_CONFIG_SHA_MISMATCH') }
        foreach ($toolName in @('acceptance_client','result_exporter')) { $toolPath = Get-FullPathSafe ([string](Get-Prop $bindingMetadata "${toolName}_path")); if (-not (Test-Path -LiteralPath $toolPath -PathType Leaf)) { [void]$missing.Add("TOOL_MISSING:$toolName") } elseif ((Get-ShaOrNull $toolPath) -ne ([string](Get-Prop $bindingMetadata "${toolName}_sha256")).ToLowerInvariant()) { [void]$fail.Add("TOOL_SHA_MISMATCH:$toolName") } elseif (Test-PathWithin -Candidate $toolPath -Root $releaseRoot) { [void]$fail.Add("TOOL_INSIDE_RELEASE:$toolName") } }
        foreach ($field in @('package_path','package_sha256','source_revision','source_tree_sha256','config_sha256')) {
            $entryValue = [string](Get-Prop $ManifestEntry $field); $metadataValue = if ($field -eq 'package_path') { [string](Get-Prop $bindingMetadata 'formal_zip_path') } elseif ($field -eq 'package_sha256') { [string](Get-Prop $bindingMetadata 'formal_zip_sha256') } elseif ($field -eq 'config_sha256') { [string](Get-Prop $bindingMetadata 'runtime_config_sha256') } else { [string](Get-Prop $bindingMetadata $field) }
            if ($entryValue -and $entryValue -ne $metadataValue) { [void]$fail.Add("MANIFEST_BINDING_MISMATCH:$field") }
        }
        foreach ($toolName in @('acceptance_client', 'result_exporter')) {
            $entryPath = [string](Get-Prop $ManifestEntry "${toolName}_path")
            $entrySha = [string](Get-Prop $ManifestEntry "${toolName}_sha256")
            $metadataToolPath = [string](Get-Prop $bindingMetadata "${toolName}_path")
            $metadataToolSha = [string](Get-Prop $bindingMetadata "${toolName}_sha256")
            if ($entryPath -and (Get-FullPathSafe $entryPath) -ne (Get-FullPathSafe $metadataToolPath)) { [void]$fail.Add("MANIFEST_BINDING_MISMATCH:${toolName}_path") }
            if ($entrySha -and $entrySha -ne $metadataToolSha) { [void]$fail.Add("MANIFEST_BINDING_MISMATCH:${toolName}_sha256") }
        }
    }
    foreach ($path in $requiredPaths) { if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { [void]$missing.Add("MISSING:$path") } }
    if ($missing.Count -gt 0) { return [pscustomobject]@{ Name = $ExpectedName; Missing = @($missing); Fail = @($fail); Harness = $null; Runtime = @(); RuntimeSamples = @(); RuntimeWeights = @(); System = @(); DiskRows = @(); IoRows = @(); NonzeroDiskRows = @(); Metrics = @{}; Metadata = $bindingMetadata; RuntimeGapMs = 0; SystemGapMs = 0; SnapshotCoverage = $null; SnapshotMaxAgeMs = $null; SummarySha = $null; SummaryRowCount = 0; MediaBeforeSha = $null; MediaAfterSha = $null; HarnessConfigSha = $null; MetadataConfigSha = [string](Get-Prop $bindingMetadata 'runtime_config_sha256'); ExecutionConfigFingerprint = $null; RunStatus = 'INCONCLUSIVE' } }
    try { $harness = Get-Content -LiteralPath $harnessPath -Raw | ConvertFrom-Json } catch { [void]$missing.Add('HARNESS_INVALID'); return [pscustomobject]@{ Name = $ExpectedName; Missing = @($missing); Fail = @($fail); Harness = $null; Runtime = @(); RuntimeSamples = @(); RuntimeWeights = @(); System = @(); DiskRows = @(); IoRows = @(); NonzeroDiskRows = @(); Metrics = @{}; Metadata = $bindingMetadata; RuntimeGapMs = 0; SystemGapMs = $null; SnapshotCoverage = $null; SnapshotMaxAgeMs = $null; SummarySha = $null; SummaryRowCount = 0; MediaBeforeSha = $null; MediaAfterSha = $null; HarnessConfigSha = $null; MetadataConfigSha = [string](Get-Prop $bindingMetadata 'runtime_config_sha256'); ExecutionConfigFingerprint = $null; RunStatus = 'INCONCLUSIVE' } }
    $harnessConfigSha = [string](Get-Prop $harness 'config_sha256')
    $effectiveLogicalConfig = if ($null -ne $LogicalConfig) { $LogicalConfig } else { Get-Prop $ManifestEntry 'logical_run_config' }
    if ([string]::IsNullOrWhiteSpace($harnessConfigSha)) { [void]$missing.Add('HARNESS_CONFIG_SHA_MISSING') }
    elseif ($harnessConfigSha -notmatch '^[0-9a-fA-F]{64}$') { [void]$fail.Add('HARNESS_CONFIG_SHA_INVALID') }
    if ($null -ne $bindingMetadata) {
        $harnessBindings = [ordered]@{
            package_path = [string](Get-Prop $bindingMetadata 'formal_zip_path')
            package_sha256 = [string](Get-Prop $bindingMetadata 'formal_zip_sha256')
            source_revision = [string](Get-Prop $bindingMetadata 'source_revision')
            source_tree_sha256 = [string](Get-Prop $bindingMetadata 'source_tree_sha256')
            release_root = [string](Get-Prop $bindingMetadata 'release_root')
            package_manifest_sha256 = [string](Get-Prop $bindingMetadata 'formal_manifest_sha256')
        }
        foreach ($field in $harnessBindings.Keys) {
            $actualValue = [string](Get-Prop $harness $field); $expectedValue = [string]$harnessBindings[$field]
            if ([string]::IsNullOrWhiteSpace($actualValue)) { [void]$missing.Add("HARNESS_BINDING_MISSING:$field") }
            elseif ($field -in @('package_path','release_root')) {
                if ((Get-FullPathSafe $actualValue) -ne (Get-FullPathSafe $expectedValue)) { [void]$fail.Add("HARNESS_BINDING_MISMATCH:$field") }
            }
            elseif ($actualValue -ne $expectedValue) { [void]$fail.Add("HARNESS_BINDING_MISMATCH:$field") }
        }
    }
    $runtimeResult = Read-JsonLines -Path $runtimePath; $systemResult = Read-JsonLines -Path $systemPath
    if ($runtimeResult.Error) { [void]$missing.Add($runtimeResult.Error) }; if ($systemResult.Error) { [void]$missing.Add($systemResult.Error) }
    $runtime = @($runtimeResult.Rows); $system = @($systemResult.Rows)
    $runtimeSamples = @($runtime | Where-Object { [string](Get-Prop $_ 'record_type') -eq 'runtime_sample' })
    $executionConfigDiagnostics = Get-ExecutionConfigDiagnostics -RuntimeSamples $runtimeSamples -LogicalConfig $effectiveLogicalConfig
    foreach ($reason in @($executionConfigDiagnostics.Failures)) { [void]$fail.Add($reason) }
    foreach ($reason in @($executionConfigDiagnostics.Missing)) { [void]$missing.Add($reason) }
    $runtimeWeights = @(Get-WeightedRuntime $runtimeSamples)
    # 先建立 system->runtime 的反向配对，后续所有磁盘、CPU、内存指标只使用生产配对样本。
    $pairing = Get-SnapshotPairing -RuntimeWeights $runtimeWeights -SystemSamples $system
    $snapshotCoverage = $pairing.Coverage
    if ($null -eq $snapshotCoverage -or $snapshotCoverage -lt 0.95) { [void]$missing.Add('SNAPSHOT_COVERAGE_LT_95_PERCENT') }
    if ($pairing.MaxAgeMs -gt 2500) { [void]$missing.Add('SNAPSHOT_AGE_GT_2500_MS') }
    $productionSystem = @($pairing.Rows | Where-Object { $_.Production -and $_.Valid } | ForEach-Object { $_.System })
    $productionSeconds = 0.0; $finalizationSeconds = 0.0; $throughput = $null; $idleWait = 0.0; $bubble = 0.0; $activeCpu = $null
    # worker capacity 从 runtime pipeline 取；harness 的 effective_worker_count 不作为性能输入。
    $workerCount = 12
    foreach ($weighted in $runtimeWeights) {
        $stageState = Get-StageState $weighted.Sample
        if ($stageState -in @('missing', 'other')) { [void]$missing.Add("COMPUTE_BASE_STAGE_$($stageState.ToUpperInvariant())") }
        $dt = [double]$weighted.WeightMs / 1000.0; if ($dt -le 0) { continue }
        $production = Get-StageRunning $weighted.Sample
        if ($production) { $productionSeconds += $dt } else { $finalizationSeconds += $dt }
        $waiting = Get-PipelineNumber $weighted.Sample 'media_permit_waiting'; $slots = Get-PipelineNumber $weighted.Sample 'worker_slots'; if ($null -eq $slots) { $slots = $workerCount }
        if ($production -and $waiting -ne $null -and $waiting -gt 0) { $idleWait += [Math]::Max(0.0, $workerCount - $slots) * $dt }
        $pending = Get-PipelineNumber $weighted.Sample 'decode_queue'; $hashIo = Get-PipelineNumber $weighted.Sample 'hash_io'; $mediaIo = Get-PipelineNumber $weighted.Sample 'media_io'
        $hashIoValue = if ($null -eq $hashIo) { 0.0 } else { [double]$hashIo }
        $mediaIoValue = if ($null -eq $mediaIo) { 0.0 } else { [double]$mediaIo }
        if ($production -and $pending -ne $null -and $pending -gt 0 -and $slots -eq 0 -and $hashIoValue -eq 0 -and $mediaIoValue -eq 0) { $bubble += $dt }
    }
    $maxCompleted = @($runtimeSamples | ForEach-Object { Get-Num (Get-Prop $_ 'overall_completed') } | Where-Object { $null -ne $_ } | Measure-Object -Maximum).Maximum
    if ($productionSeconds -gt 0 -and $null -ne $maxCompleted) { $throughput = [double]$maxCompleted / $productionSeconds }
    foreach ($pairRow in @($pairing.Rows | Where-Object { $_.Production -and $_.Valid })) {
        $pairRow.System | Add-Member -NotePropertyName _pair_weight_ms -NotePropertyValue $pairRow.WeightMs -Force
    }
    $diskRows = @(Get-WeightedDiskRows -Samples $productionSystem)
    $nonzeroDisk = @($diskRows | Where-Object { [double]$_.Value -gt 0 })
    $diskQueueP95 = Get-WeightedQuantile -Rows @($diskRows | ForEach-Object { [pscustomobject]@{ Value = $_.Queue; Weight = $_.Weight } }) -ValueProperty Value -Quantile 0.95
    $privatePeak = $null
    $privateSums = @($productionSystem | ForEach-Object { $sum = 0.0; foreach ($p in @((Get-Prop $_ 'processes'))) { $v = Get-Num (Get-Prop $p 'PrivateMemoryBytes'); if ($null -ne $v) { $sum += $v } }; $sum } | Where-Object { $_ -gt 0 })
    if ($privateSums.Count -gt 0) { $privatePeak = ($privateSums | Measure-Object -Maximum).Maximum }
    $workerCpuDelta = 0.0; $workerCpuInterval = 0.0
    foreach ($sample in $productionSystem) {
        $interval = Get-Num (Get-Prop $sample 'sample_interval_ms'); if ($null -eq $interval -or $interval -le 0) { continue }
        foreach ($p in @((Get-Prop $sample 'processes'))) { if ([string](Get-Prop $p 'Name') -in @('worker', 'worker.exe')) { $cpu = Get-Num (Get-Prop $p 'CpuDeltaMs'); if ($null -ne $cpu) { $workerCpuDelta += $cpu; $workerCpuInterval += $interval } } }
    }
    if ($workerCpuInterval -gt 0) { $activeCpu = $workerCpuDelta / $workerCpuInterval }
    $ioAnalysisRows = [Collections.Generic.List[object]]::new()
    foreach ($pairRow in @($pairing.Rows | Where-Object { $_.Production -and $_.Valid })) {
        $workerActive = Get-PipelineNumber $pairRow.Runtime 'worker_slots'
        $workerCapacity = Get-PipelineNumber $pairRow.Runtime 'worker_slots' 'capacity'; if ($null -eq $workerCapacity) { $workerCapacity = $workerCount }
        $idleWorkers = if ($null -ne $workerActive) { [Math]::Max(0.0, $workerCapacity - $workerActive) } else { $null }
        $interval = Get-Num (Get-Prop $pairRow.System 'sample_interval_ms'); if ($null -eq $interval -or $interval -le 0) { $interval = $pairRow.WeightMs }
        $cpuMs = 0.0; $cpuSeen = $false
        foreach ($process in @((Get-Prop $pairRow.System 'processes'))) {
            if ([string](Get-Prop $process 'Name') -in @('worker', 'worker.exe')) {
                $value = Get-Num (Get-Prop $process 'CpuDeltaMs'); if ($null -eq $value) { $value = Get-Num (Get-Prop $process 'cpu_delta_ms') }
                if ($null -ne $value) { $cpuMs += $value; $cpuSeen = $true }
            }
        }
        $cpuCores = if ($cpuSeen -and $interval -gt 0) { $cpuMs / $interval } else { $null }
        $diskRow = @($diskRows | Where-Object { $_.TimestampMs -eq (Get-TimeMs $pairRow.System) } | Select-Object -First 1)
        [void]$ioAnalysisRows.Add([pscustomobject]@{ Value = if ($diskRow.Count) { $diskRow[0].Value } else { $null }; Queue = if ($diskRow.Count) { $diskRow[0].Queue } else { $null }; IdleWorkers = $idleWorkers; WorkerCpuCores = $cpuCores; Weight = $pairRow.WeightMs })
    }
    # harness 的性能数字全部忽略；这里只保留 raw runtime/system 派生值。
    $runtimeResultObject = @($runtime | Where-Object { [string](Get-Prop $_ 'record_type') -eq 'runtime_result' } | Select-Object -Last 1)
    $runtimeResultObject = if ($runtimeResultObject.Count) { $runtimeResultObject[0] } else { $null }
    $runtimeResultDiagnostics = Get-RuntimeResultDiagnostics -RuntimeResult $runtimeResultObject -Harness $harness
    foreach ($reason in @($runtimeResultDiagnostics.Failures)) { [void]$fail.Add($reason) }
    foreach ($reason in @($runtimeResultDiagnostics.Missing)) { [void]$missing.Add($reason) }
    $taskTerminal = Get-TaskTerminalDiagnostics -RuntimeResult $runtimeResultObject
    foreach ($reason in @($taskTerminal.Failures)) { [void]$fail.Add($reason) }; foreach ($reason in @($taskTerminal.Missing)) { [void]$missing.Add($reason) }
    $completionTailResult = Get-CompletionTailSeconds -Samples $runtimeSamples
    foreach ($reason in @($completionTailResult.Missing)) { [void]$missing.Add($reason) }
    $completionTail = $completionTailResult.Value
    if ($null -eq $completionTail) { [void]$missing.Add('COMPLETION_APPLIED_TAIL_MISSING') }
    $itemLatencyResult = Get-LastItemLatencyP95 -Samples $runtimeSamples
    foreach ($reason in @($itemLatencyResult.Missing)) { [void]$missing.Add($reason) }
    $itemLatency = $itemLatencyResult.Value
    $rawFailures = Get-RawFailureDiagnostics -Runtime $runtime
    foreach ($reason in @($rawFailures.Failures)) { [void]$fail.Add($reason) }
    foreach ($reason in @($rawFailures.Missing)) { [void]$missing.Add($reason) }
    $rawMetrics = @{
        throughput_files_per_second = $throughput
        idle_worker_seconds_while_media_waits = $idleWait
        disk_read_queue_p95 = $diskQueueP95
        private_bytes_peak = $privatePeak
        resource_bubble_seconds = $bubble
        completion_tail_seconds = $completionTail
        item_completion_latency_p95 = $itemLatency
    }
    $failCount = @($runtime | ForEach-Object { Get-Num (Get-Prop $_ 'failed_scans') } | Where-Object { $null -ne $_ } | Measure-Object -Maximum).Maximum
    if ($null -ne $failCount -and $failCount -gt 0) { [void]$fail.Add('FAILED_SCAN') }
    if ([string](Get-Prop $harness 'run_status') -eq 'FAIL') { [void]$fail.Add('RUN_STATUS_FAIL') }
    $telemetryMissing = @(Get-RequiredTelemetry -RuntimeSamples $runtimeSamples -Variant ([string](Get-Prop $harness 'variant')))
    foreach ($name in $telemetryMissing) { [void]$missing.Add("TELEMETRY:$name") }
    $ownership = Get-OwnershipDiagnostics -RuntimeSamples $runtimeSamples -Variant ([string](Get-Prop $harness 'variant'))
    foreach ($name in @($ownership.Failures)) { [void]$fail.Add($name) }
    foreach ($name in @($ownership.Missing)) { [void]$missing.Add("OWNERSHIP_EVIDENCE:$name") }
    if ([string](Get-Prop $harness 'credit_conservation') -eq 'FAIL') { [void]$fail.Add('CREDIT_CONSERVATION_FAIL') }
    $mediaDiagnostics = Get-MediaManifestDiagnostics -BeforePath (Join-Path $evidence 'media-before.json') -AfterPath (Join-Path $evidence 'media-after.json') -ExpectedPath $TopLevelMediaPath -ExpectedSha $TopLevelMediaSha
    foreach ($reason in @($mediaDiagnostics.Failures)) { [void]$fail.Add($reason) }
    foreach ($reason in @($mediaDiagnostics.Missing)) { [void]$missing.Add($reason) }
    $harnessMediaBefore = [string](Get-Prop $harness 'media_before_sha256'); $harnessMediaAfter = [string](Get-Prop $harness 'media_after_sha256')
    if (-not $harnessMediaBefore) { [void]$missing.Add('HARNESS_MEDIA_BEFORE_SHA_MISSING') } elseif ($harnessMediaBefore -ne [string]$mediaDiagnostics.BeforeSha) { [void]$fail.Add('HARNESS_MEDIA_BEFORE_SHA_MISMATCH') }
    if (-not $harnessMediaAfter) { [void]$missing.Add('HARNESS_MEDIA_AFTER_SHA_MISSING') } elseif ($harnessMediaAfter -ne [string]$mediaDiagnostics.AfterSha) { [void]$fail.Add('HARNESS_MEDIA_AFTER_SHA_MISMATCH') }
    $manifestMediaBefore = [string](Get-Prop $ManifestEntry 'media_manifest_before_sha256'); $manifestMediaAfter = [string](Get-Prop $ManifestEntry 'media_manifest_after_sha256')
    if (-not $manifestMediaBefore) { [void]$missing.Add('MANIFEST_MEDIA_BEFORE_SHA_MISSING') } elseif ($manifestMediaBefore -ne [string]$mediaDiagnostics.BeforeSha) { [void]$fail.Add('MEDIA_BEFORE_SHA_BINDING_MISMATCH') }
    if (-not $manifestMediaAfter) { [void]$missing.Add('MANIFEST_MEDIA_AFTER_SHA_MISSING') } elseif ($manifestMediaAfter -ne [string]$mediaDiagnostics.AfterSha) { [void]$fail.Add('MEDIA_AFTER_SHA_BINDING_MISMATCH') }
    $timestamps = @($runtimeSamples | ForEach-Object { Get-TimeMs $_ } | Where-Object { $null -ne $_ } | Sort-Object)
    $runtimeGap = 0.0; for ($i = 1; $i -lt $timestamps.Count; $i++) { $runtimeGap = [Math]::Max($runtimeGap, [double]$timestamps[$i] - [double]$timestamps[$i-1]) }
    $systemTimes = @($system | ForEach-Object { Get-TimeMs $_ } | Where-Object { $null -ne $_ } | Sort-Object); $systemGap = 0.0; for ($i = 1; $i -lt $systemTimes.Count; $i++) { $systemGap = [Math]::Max($systemGap, [double]$systemTimes[$i] - [double]$systemTimes[$i-1]) }
    if ($runtimeGap -gt 2500) { [void]$missing.Add('RUNTIME_GAP_GT_2500_MS') }; if ($systemGap -gt 6000) { [void]$missing.Add('SYSTEM_GAP_GT_6000_MS') }
    $summaryPath = [string](Get-Prop $harness 'result_summary_path'); $summarySha = [string](Get-Prop $harness 'result_summary_sha256'); $summaryStatus = [string](Get-Prop $harness 'result_summary_status')
    if ($summaryPath -and -not [IO.Path]::IsPathRooted($summaryPath)) { $summaryPath = Join-Path $evidence $summaryPath }
    if ($summaryPath -and -not (Test-PathWithin -Candidate $summaryPath -Root $evidence)) { [void]$fail.Add('RESULT_SUMMARY_OUTSIDE_EVIDENCE'); $summaryPath = '' }
    $summaryDiagnostics = $null
    if ([string]::IsNullOrWhiteSpace($summaryPath) -or $summaryStatus -in @('MISSING', 'INCONCLUSIVE') -or -not (Test-Path -LiteralPath $summaryPath -PathType Leaf)) { [void]$missing.Add('RESULT_SUMMARY_MISSING') }
    else {
        $summaryDiagnostics = Get-CanonicalSummaryDiagnostics -Path $summaryPath -ExpectedSha $summarySha `
            -ExpectedTaskId ([string](Get-Prop $harness 'result_summary_task_id')) `
            -ExpectedStatus $summaryStatus `
            -ExpectedRowCount ([long](Get-Num (Get-Prop $harness 'result_summary_row_count')))
        foreach ($reason in @($summaryDiagnostics.Failures)) { [void]$fail.Add($reason) }; foreach ($reason in @($summaryDiagnostics.Missing)) { [void]$missing.Add($reason) }
        if ($summaryDiagnostics.FailedCount -gt 0) { [void]$fail.Add('FAILED_SCAN') }
    }
    if ([string](Get-Prop $harness 'media_before_sha256') -and [string](Get-Prop $harness 'media_after_sha256') -and
        [string](Get-Prop $harness 'media_before_sha256') -ne [string](Get-Prop $harness 'media_after_sha256')) { [void]$fail.Add('MEDIA_MANIFEST_CHANGED') }
    $metrics = $rawMetrics
    $metrics['production_seconds'] = $productionSeconds
    # 低/高 I/O 均在聚合阶段使用 A 的 raw P25/P75 重算，不能从 harness 取值。
    $metrics['low_io_idle_worker_mean'] = $null; $metrics['high_io_idle_worker_mean'] = $null
    $metrics['low_io_worker_cpu_cores'] = $null; $metrics['high_io_worker_cpu_cores'] = $null
    $metrics['low_high_idle_difference'] = $null; $metrics['low_high_cpu_difference'] = $null
    return [pscustomobject]@{
        Name = $ExpectedName; Harness = $harness; Runtime = $runtime; System = $system; RuntimeSamples = $runtimeSamples; RuntimeWeights = $runtimeWeights; DiskRows = $diskRows; IoRows = @($ioAnalysisRows)
        Missing = @($missing | Select-Object -Unique); Fail = @($fail | Select-Object -Unique); Metrics = $metrics; NonzeroDiskRows = $nonzeroDisk
        RuntimeGapMs = $runtimeGap; SystemGapMs = $systemGap; SnapshotCoverage = $snapshotCoverage; SnapshotMaxAgeMs = $pairing.MaxAgeMs; SummarySha = if ($summaryDiagnostics) { $summaryDiagnostics.Sha } else { $null }; SummaryRowCount = if ($summaryDiagnostics) { $summaryDiagnostics.RowCount } else { 0 }; MediaBeforeSha = $mediaDiagnostics.BeforeSha; MediaAfterSha = $mediaDiagnostics.AfterSha; Metadata = $bindingMetadata; HarnessConfigSha = $harnessConfigSha; MetadataConfigSha = [string](Get-Prop $bindingMetadata 'runtime_config_sha256'); ExecutionConfigFingerprint = $executionConfigDiagnostics.Fingerprint; RunStatus = [string](Get-Prop $harness 'run_status')
    }
}

function Test-AtLeastRatio {
    <# 用整数交叉相乘判断 candidate >= baseline * numerator/denominator。 #>
    param($Candidate, $Baseline, [long] $Numerator, [long] $Denominator)
    if ($null -eq $Candidate -or $null -eq $Baseline) { return $null }
    if ([double]$Baseline -eq 0) { return [double]$Candidate -eq 0 }
    $scale = 1000000L; $c = [long][Math]::Round([double]$Candidate * $scale); $b = [long][Math]::Round([double]$Baseline * $scale)
    return ($c * $Denominator) -ge ($b * $Numerator)
}

function Test-AtMostRatio {
    <# 用整数交叉相乘判断 candidate <= baseline * numerator/denominator。 #>
    param($Candidate, $Baseline, [long] $Numerator, [long] $Denominator)
    if ($null -eq $Candidate -or $null -eq $Baseline) { return $null }
    if ([double]$Baseline -eq 0) { return [double]$Candidate -eq 0 }
    $scale = 1000000L; $c = [long][Math]::Round([double]$Candidate * $scale); $b = [long][Math]::Round([double]$Baseline * $scale)
    return ($c * $Denominator) -le ($b * $Numerator)
}

function Get-Gate {
    <# 生成统一门禁行；缺值归 INCONCLUSIVE，已知比较结果保留 PASS/FAIL。 #>
    param([string] $Name, $Baseline, $Candidate, [string] $Rule, $Result)
    [pscustomobject]@{ Name = $Name; Baseline = $Baseline; Candidate = $Candidate; Rule = $Rule; Result = if ($null -eq $Result) { 'INCONCLUSIVE' } elseif ($Result) { 'PASS' } else { 'FAIL' } }
}

function Read-BenchmarkMedian {
    <# 只读取显式 A-1..A-3/B-1..B-3 目录，拒绝递归随意取前三个。 #>
    param([string] $Root, [string] $Variant)
    if ([string]::IsNullOrWhiteSpace($Root) -or -not (Test-Path -LiteralPath $Root -PathType Container)) { return [pscustomobject]@{ Value=$null; Missing=@("BENCHMARK_ROOT_MISSING:$Variant"); Failures=@(); Bindings=@() } }
    $rootAbsolute = Get-FullPathSafe $Root
    $values = [Collections.Generic.List[double]]::new(); $bindingRows = [Collections.Generic.List[object]]::new()
    $missing=[Collections.Generic.List[string]]::new();$failures=[Collections.Generic.List[string]]::new()
    $expectedNames = @('A-1','A-2','A-3','B-1','B-2','B-3')
    $rootDirs = @(Get-ChildItem -LiteralPath $rootAbsolute -Directory | Sort-Object Name)
    $unexpectedDirs = @($rootDirs | Where-Object { $_.Name -notin $expectedNames })
    if ($unexpectedDirs.Count -gt 0) { [void]$failures.Add('BENCHMARK_UNEXPECTED_DIRECTORY') }
    $rootFiles = @(Get-ChildItem -LiteralPath $rootAbsolute -File | Where-Object { $_.Name -ne 'benchmark-manifest.json' })
    if ($rootFiles.Count -gt 0) { [void]$failures.Add('BENCHMARK_UNEXPECTED_ROOT_FILE') }
    $manifestFile = Join-Path $rootAbsolute 'benchmark-manifest.json'
    if (-not (Test-Path -LiteralPath $manifestFile -PathType Leaf)) { [void]$missing.Add('BENCHMARK_MANIFEST_MISSING') }
    else {
        try {
            $benchmarkManifest = Get-Content -LiteralPath $manifestFile -Raw | ConvertFrom-Json
            $manifestNames = @((Get-Prop $benchmarkManifest 'order') | ForEach-Object { [string]$_ })
            if (($manifestNames -join ',') -ne ($expectedNames -join ',')) { [void]$failures.Add('BENCHMARK_MANIFEST_ORDER_INVALID') }
            if (@($manifestNames | Select-Object -Unique).Count -ne 6) { [void]$failures.Add('BENCHMARK_MANIFEST_DUPLICATE') }
        } catch { [void]$failures.Add('BENCHMARK_MANIFEST_INVALID') }
    }
    $dirs=@($rootDirs | Where-Object { $_.Name -in @("$Variant-1","$Variant-2","$Variant-3") } | Sort-Object Name)
    if($dirs.Count -ne 3){[void]$missing.Add("BENCHMARK_EXACT_THREE_REQUIRED:$Variant")}
    foreach($dir in $dirs){$file=Join-Path $dir.FullName 'benchmark.json';$extraFiles=@(Get-ChildItem -LiteralPath $dir.FullName -File | Where-Object Name -ne 'benchmark.json');$extraDirs=@(Get-ChildItem -LiteralPath $dir.FullName -Directory);if($extraFiles.Count -gt 0 -or $extraDirs.Count -gt 0){[void]$failures.Add("BENCHMARK_RUN_EXTRA_CONTENT:$dir.Name")};if(-not(Test-Path -LiteralPath $file -PathType Leaf)){[void]$missing.Add("BENCHMARK_FILE_MISSING:$dir.Name");continue};try{$obj=Get-Content -LiteralPath $file -Raw|ConvertFrom-Json}catch{[void]$failures.Add("BENCHMARK_INVALID:$dir.Name");continue};$expected=[int]($dir.Name -replace "^$Variant-",'');if([string](Get-Prop $obj 'variant') -ne $Variant -or (Get-Num (Get-Prop $obj 'run_index')) -ne $expected){[void]$failures.Add("BENCHMARK_IDENTITY:$dir.Name")};$elapsed=Get-Num(Get-Prop $obj 'elapsed_ms');if($null -eq $elapsed){[void]$missing.Add("BENCHMARK_ELAPSED_MISSING:$dir.Name")}else{[void]$values.Add($elapsed)};foreach($field in @('package_sha256','source_revision','cargo_lock_sha256','rustc','cargo','target_triple','build_config_sha256','config_sha256','exe_sha256')){if([string]::IsNullOrWhiteSpace([string](Get-Prop $obj $field))){[void]$missing.Add("BENCHMARK_BINDING_MISSING:${field}:$($dir.Name)")}};[void]$bindingRows.Add([pscustomobject]@{package_sha256=[string](Get-Prop $obj 'package_sha256');source_revision=[string](Get-Prop $obj 'source_revision');cargo_lock_sha256=[string](Get-Prop $obj 'cargo_lock_sha256');rustc=[string](Get-Prop $obj 'rustc');cargo=[string](Get-Prop $obj 'cargo');target_triple=[string](Get-Prop $obj 'target_triple');build_config_sha256=[string](Get-Prop $obj 'build_config_sha256');config_sha256=[string](Get-Prop $obj 'config_sha256');exe_sha256=[string](Get-Prop $obj 'exe_sha256')})}
    if($values.Count -ne 3){return [pscustomobject]@{Value=$null;Missing=@($missing);Failures=@($failures);Bindings=@($bindingRows)}}
    return [pscustomobject]@{Value=(Get-Median -Values @($values));Missing=@($missing);Failures=@($failures);Bindings=@($bindingRows)}
}

function Write-Utf8Bom {
    <# 报告使用 BOM，兼容 Windows PowerShell 对中文的读取。 #>
    param([string] $Path, [string] $Text)
    $parent = Split-Path -Parent $Path; if ($parent) { New-Item -ItemType Directory -Path $parent -Force | Out-Null }
    [IO.File]::WriteAllText($Path, $Text, [Text.UTF8Encoding]::new($true))
}

try {
    $abAbsolute = Get-FullPathSafe $AbRoot
    if (-not (Test-Path -LiteralPath $abAbsolute -PathType Container)) { throw 'AB_ROOT_MISSING' }
    $manifestPath = Join-Path $abAbsolute 'ab-run-manifest.json'
    if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) { throw 'AB_RUN_MANIFEST_MISSING' }
    $manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
    $manifestOrder = @((Get-Prop $manifest 'order') | ForEach-Object { [string]$_ })
    if (($manifestOrder -join ',') -ne ($fixedOrder -join ',')) { throw 'AB_RUN_ORDER_INVALID' }
    $hardFailures = [Collections.Generic.List[string]]::new(); $missingEvidence = [Collections.Generic.List[string]]::new()
    $topLevelMediaPath = Get-FullPathSafe ([string](Get-Prop $manifest 'media_manifest_path'))
    $topLevelMediaSha = [string](Get-Prop $manifest 'media_manifest_sha256')
    $expectedTopLevelMediaPath = Get-FullPathSafe (Join-Path $abAbsolute 'media-input-manifest.json')
    if (-not $topLevelMediaPath -or $topLevelMediaPath -ne $expectedTopLevelMediaPath -or -not (Test-PathWithin -Candidate $topLevelMediaPath -Root $abAbsolute)) { [void]$hardFailures.Add('MEDIA_INPUT_MANIFEST_PATH_INVALID') }
    $topLevelMediaObject = $null
    if (-not $topLevelMediaPath -or -not (Test-Path -LiteralPath $topLevelMediaPath -PathType Leaf)) { [void]$missingEvidence.Add('MEDIA_INPUT_MANIFEST_MISSING') }
    else {
        try { $topLevelMediaObject = Get-Content -LiteralPath $topLevelMediaPath -Raw | ConvertFrom-Json } catch { [void]$missingEvidence.Add('MEDIA_INPUT_MANIFEST_INVALID') }
        $semanticSha = Get-JsonSemanticSha256 -Object $topLevelMediaObject
        if (-not $topLevelMediaSha -or -not $semanticSha) { [void]$missingEvidence.Add('MEDIA_INPUT_MANIFEST_SEMANTIC_SHA_MISSING') }
        elseif ($semanticSha -ne $topLevelMediaSha.ToLowerInvariant()) { [void]$hardFailures.Add('MEDIA_INPUT_MANIFEST_SHA_MISMATCH') }
        $declaredFileSha = [string](Get-Prop $manifest 'media_manifest_file_sha256')
        if ($declaredFileSha -and (Get-ShaOrNull $topLevelMediaPath) -ne $declaredFileSha.ToLowerInvariant()) { [void]$hardFailures.Add('MEDIA_INPUT_MANIFEST_FILE_SHA_MISMATCH') }
    }
    $logicalConfig = Get-Prop $manifest 'logical_run_config'; $logicalConfigSha = [string](Get-Prop $manifest 'logical_config_sha256')
    $calculatedLogicalSha = Get-JsonSemanticSha256 -Object $logicalConfig
    if ($null -eq $logicalConfig -or -not $logicalConfigSha -or -not $calculatedLogicalSha) { [void]$missingEvidence.Add('LOGICAL_CONFIG_BINDING_MISSING') }
    elseif ($calculatedLogicalSha -ne $logicalConfigSha.ToLowerInvariant()) { [void]$hardFailures.Add('LOGICAL_CONFIG_SHA_MISMATCH') }
    $runs = [Collections.Generic.List[object]]::new(); $seenPaths = @{}
    $manifestRuns = @((Get-Prop $manifest 'runs'))
    foreach ($name in $fixedOrder) {
        $runRoot = Join-Path $abAbsolute $name; $key = (Get-FullPathSafe $runRoot).ToLowerInvariant()
        if ($seenPaths.ContainsKey($key)) { [void]$hardFailures.Add('DUPLICATE_RUN_PATH') } else { $seenPaths[$key] = $true }
        if (-not (Test-Path -LiteralPath $runRoot -PathType Container)) { [void]$missingEvidence.Add("RUN_MISSING:$name"); continue }
        $entry = @($manifestRuns | Where-Object { [string](Get-Prop (Get-Prop $_ 'intended') 'name') -eq $name } | Select-Object -First 1)
        if ($entry.Count -eq 0) { [void]$missingEvidence.Add("RUN_MANIFEST_MISSING:$name"); continue }
        $entry = $entry[0]
        $run = Get-RunMetrics -RunRoot $runRoot -ExpectedName $name -ManifestEntry $entry -TopLevelMediaPath $topLevelMediaPath -TopLevelMediaSha $topLevelMediaSha -LogicalConfig $logicalConfig; [void]$runs.Add($run)
        foreach ($reason in @($run.Fail)) { [void]$hardFailures.Add("$name`:$reason") }; foreach ($reason in @($run.Missing)) { [void]$missingEvidence.Add("$name`:$reason") }
        $declaredVariant = [string](Get-Prop (Get-Prop $entry 'intended') 'variant'); $declaredIndex = Get-Num (Get-Prop (Get-Prop $entry 'intended') 'run_index')
        if ($declaredVariant -and "$declaredVariant-$([int]$declaredIndex)" -ne $name) { [void]$hardFailures.Add("$name`:RUN_IDENTITY_MISMATCH") }
        $entryLogicalSha = [string](Get-Prop $entry 'logical_config_sha256'); $entryLogical = Get-Prop $entry 'logical_run_config'
        if (-not $entryLogicalSha -or -not $entryLogical) { [void]$missingEvidence.Add("$name`:LOGICAL_CONFIG_BINDING_MISSING") }
        elseif ($entryLogicalSha -ne $logicalConfigSha -or (Get-JsonSemanticSha256 -Object $entryLogical) -ne $logicalConfigSha) { [void]$hardFailures.Add("$name`:LOGICAL_CONFIG_BINDING_MISMATCH") }
    }
    if ($runs.Count -ne 6) { [void]$missingEvidence.Add('SIX_RUNS_REQUIRED') }
    $byVariant = @{ A = @($runs | Where-Object { $_.Name -like 'A-*' }); B = @($runs | Where-Object { $_.Name -like 'B-*' }) }
    foreach ($variant in @('A', 'B')) {
        $packageHashes = @(@($byVariant[$variant] | ForEach-Object { [string](Get-Prop $_.Metadata 'formal_zip_sha256') } | Where-Object { $_ }) | Select-Object -Unique)
        $sourceHashes = @(@($byVariant[$variant] | ForEach-Object { [string](Get-Prop $_.Metadata 'source_tree_sha256') } | Where-Object { $_ }) | Select-Object -Unique)
        if ($packageHashes.Count -ne 1) { if ($packageHashes.Count -gt 1) { [void]$hardFailures.Add("$variant`_PACKAGE_DRIFT") } else { [void]$missingEvidence.Add("$variant`_PACKAGE_SHA_MISSING") } }
        if ($sourceHashes.Count -ne 1) { if ($sourceHashes.Count -gt 1) { [void]$hardFailures.Add("$variant`_SOURCE_DRIFT") } else { [void]$missingEvidence.Add("$variant`_SOURCE_SHA_MISSING") } }
    }
    $allRuns = @($runs)
    $summaryShas = @(@($allRuns | ForEach-Object { $_.SummarySha } | Where-Object { $_ }) | Select-Object -Unique)
    if ($summaryShas.Count -ne 1) { if ($summaryShas.Count -gt 1) { [void]$hardFailures.Add('RESULT_SUMMARY_SHA_MISMATCH_ACROSS_RUNS') } else { [void]$missingEvidence.Add('RESULT_SUMMARY_SHA_MISSING') } }
    $commonFields = @('cargo_lock_sha256','rustc','cargo','target_triple','build_config_sha256','runtime_config_sha256')
    foreach ($field in $commonFields) {
        $values = @(@($allRuns | ForEach-Object { [string](Get-Prop $_.Metadata $field) } | Where-Object { $_ }) | Select-Object -Unique)
        if ($values.Count -gt 1) { [void]$hardFailures.Add("ENV_DRIFT:$field") } elseif ($values.Count -eq 0) { [void]$missingEvidence.Add("ENV_MISSING:$field") }
    }
    # Task6 config_sha256 包含隔离 data/cache/端口，允许每轮不同；只要求每轮自身为合法绑定，跨轮比较 logical config。
    $harnessConfigHashes = @(@($allRuns | ForEach-Object { [string]$_.HarnessConfigSha } | Where-Object { $_ }) | Select-Object -Unique)
    if ($harnessConfigHashes.Count -eq 0) { [void]$missingEvidence.Add('HARNESS_CONFIG_SHA_MISSING') }
    $metadataConfigHashes = @(@($allRuns | ForEach-Object { [string]$_.MetadataConfigSha } | Where-Object { $_ }) | Select-Object -Unique)
    if ($metadataConfigHashes.Count -ne 1) { if ($metadataConfigHashes.Count -gt 1) { [void]$hardFailures.Add('METADATA_RUNTIME_CONFIG_SHA_DRIFT') } else { [void]$missingEvidence.Add('METADATA_RUNTIME_CONFIG_SHA_MISSING') } }
    $executionConfigHashes = @(@($allRuns | ForEach-Object { [string]$_.ExecutionConfigFingerprint } | Where-Object { $_ }) | Select-Object -Unique)
    if ($executionConfigHashes.Count -gt 1) { [void]$hardFailures.Add('EXECUTION_CONFIG_DRIFT_ACROSS_RUNS') } elseif ($executionConfigHashes.Count -eq 0) { [void]$missingEvidence.Add('EXECUTION_CONFIG_FINGERPRINT_MISSING') }
    $mediaBefore = @($allRuns | ForEach-Object { $_.MediaBeforeSha } | Where-Object { $_ } | Select-Object -Unique); $mediaAfter = @($allRuns | ForEach-Object { $_.MediaAfterSha } | Where-Object { $_ } | Select-Object -Unique)
    if ($mediaBefore.Count -ne 1 -or $mediaAfter.Count -ne 1 -or $mediaBefore[0] -ne $topLevelMediaSha.ToLowerInvariant() -or $mediaAfter[0] -ne $topLevelMediaSha.ToLowerInvariant()) { [void]$hardFailures.Add('CORRECTNESS_MEDIA_MANIFEST_DRIFT') }
    $baselineNonzero = @($byVariant.A | ForEach-Object { $_.IoRows } | Where-Object { $_.Value -gt 0 })
    if ($baselineNonzero.Count -eq 0) { [void]$missingEvidence.Add('BASELINE_NONZERO_IO_SAMPLE_MISSING') }
    $baselineIoRows = @($baselineNonzero | ForEach-Object { [pscustomobject]@{ Value = $_.Value; Weight = $_.Weight } })
    $baselineIoP25 = Get-WeightedQuantile -Rows $baselineIoRows -ValueProperty Value -Quantile 0.25
    $baselineIoP75 = Get-WeightedQuantile -Rows $baselineIoRows -ValueProperty Value -Quantile 0.75
    if ($null -eq $baselineIoP25 -or $null -eq $baselineIoP75) { [void]$missingEvidence.Add('BASELINE_IO_BUCKET_THRESHOLD_MISSING') }
    foreach ($run in $allRuns) {
        $low = @($run.IoRows | Where-Object { $null -ne $baselineIoP25 -and $null -ne $_.Value -and $_.Value -le $baselineIoP25 })
        $high = @($run.IoRows | Where-Object { $null -ne $baselineIoP75 -and $null -ne $_.Value -and $_.Value -ge $baselineIoP75 })
        $mean = {
            param($rows, $field)
            $sum=0.0; $weight=0.0
            foreach($row in $rows){$v=Get-Num (Get-Prop $row $field);$w=Get-Num (Get-Prop $row 'Weight');if($null -ne $v -and $w -gt 0){$sum+=$v*$w;$weight+=$w}}
            if($weight -gt 0){return $sum/$weight};return $null
        }
        $run.Metrics['low_io_idle_worker_mean'] = & $mean $low 'IdleWorkers'; $run.Metrics['high_io_idle_worker_mean'] = & $mean $high 'IdleWorkers'
        $run.Metrics['low_io_worker_cpu_cores'] = & $mean $low 'WorkerCpuCores'; $run.Metrics['high_io_worker_cpu_cores'] = & $mean $high 'WorkerCpuCores'
        $run.Metrics['low_high_idle_difference'] = if($null -ne $run.Metrics['low_io_idle_worker_mean'] -and $null -ne $run.Metrics['high_io_idle_worker_mean']){[Math]::Abs($run.Metrics['high_io_idle_worker_mean']-$run.Metrics['low_io_idle_worker_mean'])}else{$null}
        $run.Metrics['low_high_cpu_difference'] = if($null -ne $run.Metrics['low_io_worker_cpu_cores'] -and $null -ne $run.Metrics['high_io_worker_cpu_cores']){[Math]::Abs($run.Metrics['high_io_worker_cpu_cores']-$run.Metrics['low_io_worker_cpu_cores'])}else{$null}
    }
    foreach ($run in $allRuns) { if ([string](Get-Prop $run.Harness 'run_status') -eq 'INCONCLUSIVE') { [void]$missingEvidence.Add("$($run.Name):RUN_INCONCLUSIVE") } }

    $aMetrics = @{}; $bMetrics = @{}
    $metricNames = @('throughput_files_per_second','idle_worker_seconds_while_media_waits','low_high_idle_difference','low_high_cpu_difference','disk_read_queue_p95','private_bytes_peak','resource_bubble_seconds','completion_tail_seconds','item_completion_latency_p95')
    foreach ($name in $metricNames) {
        $aMetrics[$name] = Get-Median -Values @($byVariant.A | ForEach-Object { $_.Metrics[$name] }); $bMetrics[$name] = Get-Median -Values @($byVariant.B | ForEach-Object { $_.Metrics[$name] })
    }
    $gates = [Collections.Generic.List[object]]::new()
    [void]$gates.Add((Get-Gate 'production throughput' $aMetrics.throughput_files_per_second $bMetrics.throughput_files_per_second 'B >= A * 95%' (Test-AtLeastRatio $bMetrics.throughput_files_per_second $aMetrics.throughput_files_per_second 95 100)))
    [void]$gates.Add((Get-Gate 'media wait idle Worker-seconds' $aMetrics.idle_worker_seconds_while_media_waits $bMetrics.idle_worker_seconds_while_media_waits 'B <= A * 50%' (Test-AtMostRatio $bMetrics.idle_worker_seconds_while_media_waits $aMetrics.idle_worker_seconds_while_media_waits 50 100)))
    [void]$gates.Add((Get-Gate 'low/high I/O idle difference' $aMetrics.low_high_idle_difference $bMetrics.low_high_idle_difference 'B <= A * 50%' (Test-AtMostRatio $bMetrics.low_high_idle_difference $aMetrics.low_high_idle_difference 50 100)))
    [void]$gates.Add((Get-Gate 'low/high I/O Worker CPU difference' $aMetrics.low_high_cpu_difference $bMetrics.low_high_cpu_difference 'B <= A * 50%' (Test-AtMostRatio $bMetrics.low_high_cpu_difference $aMetrics.low_high_cpu_difference 50 100)))
    [void]$gates.Add((Get-Gate 'disk read queue weighted P95' $aMetrics.disk_read_queue_p95 $bMetrics.disk_read_queue_p95 'B <= A * 110%' (Test-AtMostRatio $bMetrics.disk_read_queue_p95 $aMetrics.disk_read_queue_p95 110 100)))
    [void]$gates.Add((Get-Gate 'private bytes peak' $aMetrics.private_bytes_peak $bMetrics.private_bytes_peak 'B <= A * 125%' (Test-AtMostRatio $bMetrics.private_bytes_peak $aMetrics.private_bytes_peak 125 100)))
    [void]$gates.Add((Get-Gate 'resource bubble seconds' $aMetrics.resource_bubble_seconds $bMetrics.resource_bubble_seconds 'B <= A * 50%' (Test-AtMostRatio $bMetrics.resource_bubble_seconds $aMetrics.resource_bubble_seconds 50 100)))
    $ownershipViolations = @($allRuns | ForEach-Object { $_.Fail } | Where-Object { $_ -match 'CACHE_WAIT_RESOURCE_OWNERSHIP_VIOLATION' }).Count
    [void]$gates.Add((Get-Gate 'cache wait ownership violation' 0 $ownershipViolations 'must equal 0' ($ownershipViolations -eq 0)))
    [void]$gates.Add((Get-Gate '90 percent to final ACK tail' $aMetrics.completion_tail_seconds $bMetrics.completion_tail_seconds 'B <= A' (Test-AtMostRatio $bMetrics.completion_tail_seconds $aMetrics.completion_tail_seconds 100 100)))
    [void]$gates.Add((Get-Gate 'item completion latency P95' $aMetrics.item_completion_latency_p95 $bMetrics.item_completion_latency_p95 'B <= A * 110%' (Test-AtMostRatio $bMetrics.item_completion_latency_p95 $aMetrics.item_completion_latency_p95 110 100)))
    foreach ($gate in $gates) { if ($gate.Result -eq 'FAIL') { [void]$hardFailures.Add("PERFORMANCE:$($gate.Name)") }; if ($gate.Result -eq 'INCONCLUSIVE') { [void]$missingEvidence.Add("PERFORMANCE_EVIDENCE:$($gate.Name)") } }

    $benchmarkA = Read-BenchmarkMedian -Root $BenchmarkRoot -Variant 'A'; $benchmarkB = Read-BenchmarkMedian -Root $BenchmarkRoot -Variant 'B'
    foreach($reason in @($benchmarkA.Failures+$benchmarkB.Failures)){[void]$hardFailures.Add($reason)};foreach($reason in @($benchmarkA.Missing+$benchmarkB.Missing)){[void]$missingEvidence.Add($reason)}
    # 变体运行证据缺失只记录 missing；不能在 benchmark 已明确 FAIL 时因 [0] 越界中断。
    foreach($pair in @([pscustomobject]@{Variant='A';Data=$benchmarkA;Runs=@($byVariant.A)},[pscustomobject]@{Variant='B';Data=$benchmarkB;Runs=@($byVariant.B)})){
        $representative = @($pair.Runs | Where-Object { $null -ne (Get-Prop $_ 'Metadata') } | Select-Object -First 1)
        if ($representative.Count -eq 0) {
            [void]$missingEvidence.Add("BENCHMARK_METADATA_MISSING:$($pair.Variant)")
            continue
        }
        $expectedMetadata = Get-Prop $representative[0] 'Metadata'
        foreach($binding in @($pair.Data.Bindings)){
            foreach($field in @('package_sha256','source_revision','cargo_lock_sha256','rustc','cargo','target_triple','build_config_sha256','config_sha256','exe_sha256')){
                $expectedValue = switch ($field) {
                    'package_sha256' { [string](Get-Prop $expectedMetadata 'formal_zip_sha256'); break }
                    'config_sha256' { [string](Get-Prop $expectedMetadata 'runtime_config_sha256'); break }
                    'exe_sha256' { [string](Get-Prop $expectedMetadata 'node_exe_sha256'); break }
                    default { [string](Get-Prop $expectedMetadata $field) }
                }
                if([string]$binding.$field -ne $expectedValue){[void]$hardFailures.Add("BENCHMARK_BINDING_DRIFT:$($pair.Variant):$field")}
            }
        }
    }
    $benchmarkImprovement = if ($benchmarkA.Value -ne $null -and $benchmarkA.Value -ne 0 -and $benchmarkB.Value -ne $null) { (($benchmarkA.Value - $benchmarkB.Value) / $benchmarkA.Value) * 100.0 } else { $null }
    $benchmarkResult = if ($benchmarkA.Value -eq 0) { 'INCONCLUSIVE' } elseif ($benchmarkA.Value -eq $null -or $benchmarkB.Value -eq $null -or $benchmarkImprovement -eq $null) { 'INCONCLUSIVE' } elseif ($benchmarkImprovement -ge 15.0) { 'PASS' } else { 'FAIL' }
    if ($benchmarkResult -eq 'FAIL') { [void]$hardFailures.Add('PERFORMANCE:fixed benchmark improvement < 15%') } elseif ($benchmarkResult -eq 'INCONCLUSIVE') { [void]$missingEvidence.Add('BENCHMARK_ELAPSED_MS_MISSING') }

    $verdict = if ($hardFailures.Count -gt 0) { 'FAIL' } elseif ($missingEvidence.Count -gt 0) { 'INCONCLUSIVE' } else { 'PASS' }
    $out = if ($OutputPath) { Get-FullPathSafe $OutputPath } else { Join-Path (Split-Path -Parent $abAbsolute) 'cpu-io-ab-report.md' }
    if (Test-PathWithin -Candidate $out -Root $abAbsolute) { throw 'AB_REPORT_OUTPUT_INSIDE_IMMUTABLE_EVIDENCE' }
    $rawRows = @($fixedOrder | ForEach-Object { $run = $allRuns | Where-Object Name -eq $_ | Select-Object -First 1; if ($null -eq $run) { "| $_ | — | — | — | — | — |" } else { "| $($_) | $([string]$run.RunStatus) | $([string]$run.SnapshotCoverage) | $($run.SummarySha) | $([string](Get-Prop $run.Metadata 'formal_zip_sha256')) | $($run.SummaryRowCount) |" } }) -join "`n"
    $gateRows = @($gates | ForEach-Object { "| $($_.Name) | $($_.Baseline) | $($_.Candidate) | $($_.Rule) | $($_.Result) |" }) -join "`n"
    $failText = if ($hardFailures.Count) { ($hardFailures | Select-Object -Unique | ForEach-Object { "- $_" }) -join "`n" } else { '- 无。' }
    $missingText = if ($missingEvidence.Count) { ($missingEvidence | Select-Object -Unique | ForEach-Object { "- $_" }) -join "`n" } else { '- 无。' }
    $report = @"
# Rust V2 CPU/I/O 六轮 A/B 聚合验收

结论：$verdict

## 固定顺序与逐轮绑定

| 轮次 | run status | snapshot coverage | canonical result SHA-256 | package SHA-256 | canonical rows |
| --- | --- | ---: | --- | --- | ---: |
$rawRows

固定顺序：A-1, B-1, B-2, A-2, A-3, B-3。生产阶段以 `ComputeBaseFeatures=running` 的真实时间积分，终态后的 finalization tail 单列，不采用固定 120 秒切割。proto 阶段数值按 WAITING=1、RUNNING=2、COMPLETED=3、FAILED=4、SKIPPED=5 解析，只有字符串 `running` 或数值 2 进入生产窗口。

## 三轮中位数与性能硬门禁

| 门禁 | A 三轮中位数 | B 三轮中位数 | 公式/阈值 | 结果 |
| --- | ---: | ---: | --- | --- |
$gateRows

    - 固定基准 A median elapsed_ms：$(if($null -eq $benchmarkA.Value){'—'}else{'{0:N3}' -f $benchmarkA.Value})
    - 固定基准 B median elapsed_ms：$(if($null -eq $benchmarkB.Value){'—'}else{'{0:N3}' -f $benchmarkB.Value})
- 固定基准改善：$(if($null -eq $benchmarkImprovement){'—'}else{'{0:N3}%' -f $benchmarkImprovement})；要求至少 15%
- 基线非零物理盘读取样本：$($baselineNonzero.Count)；P25/P75 采用真实时间加权经验分位数
- 基线非零物理盘读取加权 P25：$(if($null -eq $baselineIoP25){'—'}else{'{0:N3}' -f $baselineIoP25})；P75：$(if($null -eq $baselineIoP75){'—'}else{'{0:N3}' -f $baselineIoP75})；两阈值原样用于 B

## 已证实硬失败（优先级高于缺证）

$failText

## 证据缺失或 INCONCLUSIVE

$missingText

## 证据规则

- runtime 采样目标为 1,000 ms，system 采样目标为 2,000 ms；统计使用实际 `sample_interval_ms`/时间戳权重。
- system sample 只匹配时间戳不晚于它的最近 runtime snapshot；快照年龄上限 2,500 ms，生产积分覆盖必须至少 95%。
- A/B canonical JSONL 按 normalized path 排序，摘要 SHA 必须在六轮完全一致；overall counters 不能替代摘要。
- 媒体清单绑定使用解析对象 `ConvertTo-Json -Depth 32 -Compress` 的语义 SHA；顶层清单必须是本轮 `media-input-manifest.json`，每轮 harness before/after SHA 必须与实际重算值一致。
- Task6 的 `config_sha256` 含隔离 data/cache/端口路径，因此允许每轮 raw SHA 不同；报告只比较顶层/每轮的稳定 `logical_run_config` 指纹，并用 runtime `execution_config` 容量字段核对 Worker 与磁盘读取配置。
- 比例门禁使用整数交叉相乘；基线为零时，候选必须同为零，否则不能通过；无有效分母为 INCONCLUSIVE。
- 外置 acceptance client/result exporter 不进入 formal ZIP；本报告不允许作为正式部署许可。

原始 A/B 根：$abAbsolute
"@
    Write-Utf8Bom -Path $out -Text $report
    Write-Output "RUST_V2_CPU_IO_AB_REPORT_$verdict"
    Write-Output "REPORT_PATH=$out"
    if ($verdict -eq 'FAIL') { exit 2 }
    if ($verdict -eq 'INCONCLUSIVE') { exit 3 }
}
catch {
    [Console]::Error.WriteLine([string]$_)
    Write-Output 'RUST_V2_CPU_IO_AB_REPORT_INCONCLUSIVE'
    exit 3
}
