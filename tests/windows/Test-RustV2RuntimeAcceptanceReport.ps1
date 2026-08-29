<#
.SYNOPSIS
使用小型真实结构夹具验证单次双物理盘 TSV 验收报告。

.DESCRIPTION
夹具保留 runtime/system NDJSON 作为遥测载体，但结果摘要只使用固定 TSV。
它刻意不创建旧 JSONL、metadata、lease 或结果 Task ID，验证报告不会依赖这些旧边界。
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$reporter = Join-Path $repositoryRoot 'tests\windows\New-RustV2RuntimeAcceptanceReport.ps1'
$fixtureRoot = Join-Path ([IO.Path]::GetTempPath()) ("rust-v2-runtime-report-tsv-" + [Guid]::NewGuid().ToString('N'))

function Get-Sha256Bytes {
    <# 计算字节数组 SHA-256，供 TSV footer 和完整文件绑定使用。 #>
    param([byte[]] $Bytes)

    $sha = [Security.Cryptography.SHA256]::Create()
    try {
        ([BitConverter]::ToString($sha.ComputeHash($Bytes)) -replace '-', '').ToLowerInvariant()
    }
    finally {
        $sha.Dispose()
    }
}

function Write-Utf8NoBom {
    <# 写 UTF-8 无 BOM 文件，固定使用 LF 以匹配结果协议。 #>
    param([string] $Path, [string] $Text)

    [IO.File]::WriteAllText($Path, $Text, [Text.UTF8Encoding]::new($false))
}

function Get-SummaryColumns {
    <# 返回 exporter 固定的 29 个 TSV 列名。 #>
    @(
        'record_type', 'status', 'machine_id', 'normalized_path', 'display_path', 'file_size', 'md5',
        'media_type', 'base_complete', 'feature_payload_sha256', 'image_stage1_sha256', 'image_stage2_sha256',
        'video_metadata_sha256', 'video_frame_stage1_0_sha256', 'video_frame_stage1_1_sha256',
        'video_frame_stage1_2_sha256', 'video_frame_stage1_3_sha256', 'video_frame_stage1_4_sha256',
        'video_frame_stage1_5_sha256', 'video_frame_stage2_0_sha256', 'video_frame_stage2_1_sha256',
        'video_frame_stage2_2_sha256', 'video_frame_stage2_3_sha256', 'video_frame_stage2_4_sha256',
        'video_frame_stage2_5_sha256', 'thumbnail_sha256', 'thumbnail_state', 'contact_sheet_sha256',
        'status_reason'
    )
}

function Write-ResultSummaryTsv {
    <# 写一个完整 TSV，footer hash 覆盖表头和所有 R 行但不覆盖 footer。 #>
    param(
        [Parameter(Mandatory)] [string] $Root,
        [ValidateSet('PASS', 'MISSING', 'INCONCLUSIVE')] [string] $Status = 'PASS',
        [switch] $BrokenFooter
    )

    $columns = @(Get-SummaryColumns)
    $row = @(
        'R', $Status, ('1' * 64), 'H:\pik\00000000000\a.mp4', 'H:\pik\00000000000\a.mp4', '1024',
        ('b' * 32), 'video', 'true', ('c' * 64), '', '', ('d' * 64),
        ('e' * 64), ('e' * 64), ('e' * 64), ('e' * 64), ('e' * 64), ('e' * 64),
        ('f' * 64), ('f' * 64), ('f' * 64), ('f' * 64), ('f' * 64), ('f' * 64),
        '', '', ('1' * 64), ''
    )
    $dataText = (($columns -join "`t") + "`n" + ($row -join "`t") + "`n")
    $dataBytes = [Text.UTF8Encoding]::new($false).GetBytes($dataText)
    $dataSha = Get-Sha256Bytes -Bytes $dataBytes
    if ($BrokenFooter) { $dataSha = ('0' * 64) }
    $text = $dataText + "F`t1`t$dataSha`n"
    $path = Join-Path $Root 'result-summary.tsv'
    Write-Utf8NoBom -Path $path -Text $text
    [pscustomobject]@{
        Path = $path
        Sha256 = Get-Sha256Bytes -Bytes ([Text.UTF8Encoding]::new($false).GetBytes($text))
        RowCount = 1
        Status = $Status
    }
}

function New-PipelineMetrics {
    <# 构造完整的调度遥测；数值遵守双盘并行和终态归零约束。 #>
    param(
        [bool] $Terminal,
        [switch] $CapacityExceeded,
        [switch] $MissingOwnership
    )

    $active = if ($Terminal) { 0 } else { 1 }
    $metrics = [ordered]@{
        # Hash 队列 current 覆盖 waiting 与 reading 两类 ownership，保持守恒关系。
        hash_queue = [ordered]@{ current = $active * 2; peak = 4; capacity = 12; wait_latency = $null; service_latency = $null }
        path_cache_queue = [ordered]@{ current = $active; peak = 2; capacity = 24; wait_latency = $null; service_latency = $null }
        content_cache_queue = [ordered]@{ current = $active; peak = 2; capacity = 48; wait_latency = $null; service_latency = $null }
        decode_queue = [ordered]@{ current = $active; peak = 4; capacity = 24; wait_latency = $null; service_latency = $null }
        persist_queue = [ordered]@{ current = $active; peak = 2; capacity = 48; wait_latency = $null; service_latency = $null }
        hash_io = [ordered]@{ current = $active; peak = 2; capacity = 12; wait_latency = $null; service_latency = $null }
        media_io = [ordered]@{ current = $active * 2; peak = 4; capacity = 12; wait_latency = $null; service_latency = $null }
        cpu_weight = [ordered]@{ current = if ($Terminal) { 0 } else { 8 }; peak = 16; capacity = 20; wait_latency = $null; service_latency = $null }
        worker_slots = [ordered]@{ current = if ($Terminal) { 0 } else { 2 }; peak = 2; capacity = 20; wait_latency = $null; service_latency = $null }
        hash_bytes = 4096
        hash_waiting_permit = [ordered]@{ current = $active; peak = 2; capacity = 12 }
        hash_reading = [ordered]@{ current = $active; peak = 2; capacity = 12 }
        hash_completed_unjoined = [ordered]@{ current = 0; peak = 1; capacity = 12 }
        media_permit_waiting = [ordered]@{ current = $active; peak = 2; capacity = 12 }
        media_acquire_ready = [ordered]@{ current = 0; peak = 1; capacity = 12 }
        media_permit_ready = [ordered]@{ current = 0; peak = 1; capacity = 12 }
        worker_dispatching = [ordered]@{ current = 0; peak = 1; capacity = 20 }
        worker_start_pending = [ordered]@{ current = 0; peak = 1; capacity = 20 }
        worker_decode = [ordered]@{ current = $active; peak = 2; capacity = 20 }
        worker_feature = [ordered]@{ current = $active; peak = 2; capacity = 20 }
        worker_result_wait = [ordered]@{ current = 0; peak = 1; capacity = 20 }
        worker_phase_unknown = [ordered]@{ current = 0; peak = 0; capacity = 20 }
        content_output_credit_owned = [ordered]@{ current = $active; peak = 2; capacity = 48 }
        hash_refill_token_available = [ordered]@{ current = $active; peak = 2; capacity = 12 }
        decode_credit_owned = [ordered]@{ current = $active * 2; peak = 2; capacity = 20 }
        item_completion_latency = [ordered]@{ buckets = @(); count = 2; p50_ms = 20; p95_ms = 30; p99_ms = 40; max_ms = 50 }
        disk_reads = @(
            [ordered]@{
                physical_disk_id = 'PhysicalDisk10'; capacity = 4; hash_waiting = $active; media_waiting = 0
                hash_active = $active; media_active = 0; hash_granted_total = 3; media_granted_total = 0
                hash_released_total = if ($Terminal) { 3 } else { 2 }; media_released_total = 0
            }
            [ordered]@{
                physical_disk_id = 'PhysicalDisk11'; capacity = 4; hash_waiting = 0; media_waiting = $active
                hash_active = 0; media_active = $active; hash_granted_total = 0; media_granted_total = 3
                hash_released_total = 0; media_released_total = if ($Terminal) { 3 } else { 2 }
            }
        )
    }
    if ($CapacityExceeded) {
        # 真实容量越界应由报告裁为 FAIL，而不是只检查输出是否存在。
        $metrics.hash_queue.peak = 20
    }
    if ($MissingOwnership) {
        # 缺字段只能得到 INCONCLUSIVE，不能从其它队列反推该 ownership。
        [void]$metrics.Remove('worker_feature')
    }
    $metrics
}

function Write-JsonLines {
    <# 写 runtime/system NDJSON，时间轴使用 runtime 1 秒、system 2 秒。 #>
    param(
        [Parameter(Mandatory)] [string] $Root,
        [ValidateSet('completed', 'failed', 'cancelled')] [string] $TerminalState = 'completed',
        [int] $TerminalAtSeconds = 2,
        [switch] $MissingSecondDisk,
        [switch] $CapacityExceeded,
        [switch] $MissingOwnership,
        [switch] $RuntimeGap
    )

    $runtime = [Collections.Generic.List[string]]::new()
    $machine = '1' * 64
    for ($elapsed = 0; $elapsed -le $TerminalAtSeconds; $elapsed++) {
        $terminal = $elapsed -eq $TerminalAtSeconds
        $workers = if ($terminal) {
            @()
        }
        else {
            @(
                [ordered]@{ slot = 0; process_id = 1001; phase = 'decode'; display_path = 'H:\pik\00000000000\a.mp4'; physical_disk_id = 'PhysicalDisk10'; cpu_weight = 4; decoder_threads = 2 }
                [ordered]@{ slot = 1; process_id = 1002; phase = 'feature'; display_path = 'I:\tmp\b.mp4'; physical_disk_id = 'PhysicalDisk11'; cpu_weight = 4; decoder_threads = 2 }
            )
        }
        $metrics = New-PipelineMetrics -Terminal:$terminal -CapacityExceeded:$CapacityExceeded -MissingOwnership:$MissingOwnership
        if ($MissingSecondDisk) { $metrics.disk_reads = @($metrics.disk_reads[0]) }
        $runtimeObject = [ordered]@{
            record_type = 'runtime_sample'; utc_unix_ms = 1787356800000 + ($elapsed * 1000)
            elapsed_seconds = $elapsed; sample_interval_ms = if ($elapsed -eq 0) { 0 } else { 1000 }
            runtime_task_id = 'runtime-transient-1'; machine_id = $machine
            state = if ($terminal) { $TerminalState } else { 'running' }
            overall_completed = if ($terminal) { 2 } else { $elapsed }
            overall_total = 2; overall_total_known = $true; overall_failed = if ($TerminalState -eq 'failed') { 1 } else { 0 }
            overall_skipped = 0; stale = $false; workers = $workers; failures = @()
            execution_config = [ordered]@{
                hash_tasks = 12; path_cache_queue_capacity = 24; content_cache_queue_capacity = 48
                decode_queue_capacity = 24; persist_queue_capacity = 48; worker_slots = 20; cpu_budget = 20
                global_disk_permits = 12; hdd_per_disk_permits = 1; ssd_per_disk_permits = 5; unknown_per_disk_permits = 1
            }
            pipeline_metrics = $metrics
            stages = @(
                [ordered]@{ stage_id = 'base_compute'; display_name = '基础计算'; state = if ($terminal) { 3 } else { 2 }; unit = 'items'; completed = if ($terminal) { 2 } else { $elapsed }; total = 2; total_known = $true; failed = if ($TerminalState -eq 'failed') { 1 } else { 0 }; skipped = 0; speed_per_second = 1; elapsed_ms = $elapsed * 1000; eta_ms = 0 }
            )
        }
        if ($RuntimeGap -and $elapsed -eq 1) {
            # 仅破坏真实 UTC 间隔，保留声明间隔，验证报告的时间轴交叉门禁。
            $runtimeObject.utc_unix_ms += 2000
        }
        [void]$runtime.Add(($runtimeObject | ConvertTo-Json -Depth 12 -Compress))
    }
    $result = [ordered]@{
        record_type = 'runtime_result'; duration_seconds = $TerminalAtSeconds; sample_count = $TerminalAtSeconds + 1
        scans_started = 1; failed_scans = if ($TerminalState -eq 'failed') { 1 } else { 0 }
        cancelled_at_deadline = $false; correctness = if ($TerminalState -eq 'completed') { 'PASS' } else { 'FAIL' }
        media_roots = @('H:\pik\00000000000', 'I:\tmp'); single_run = $true
        scan_tasks = @([ordered]@{ persistent_task_id = 'runtime-only-task'; runtime_task_id = 'runtime-transient-1'; terminal_state = $TerminalState })
    }
    [void]$runtime.Add(($result | ConvertTo-Json -Depth 12 -Compress))
    Write-Utf8NoBom -Path (Join-Path $Root 'runtime.ndjson') -Text (($runtime -join "`n") + "`n")

    $system = [Collections.Generic.List[string]]::new()
    for ($elapsed = 0; $elapsed -le $TerminalAtSeconds; $elapsed += 2) {
        $systemObject = [ordered]@{
            record_type = 'system_sample'; utc_unix_ms = 1787356800000 + ($elapsed * 1000)
            elapsed_seconds = $elapsed; sample_interval_ms = if ($elapsed -eq 0) { 0 } else { 2000 }
            processes = @(
                [ordered]@{ Name = 'node'; ProcessId = 900; CpuDeltaMs = 20; WorkingSetBytes = 134217728; PrivateMemoryBytes = 100663296 }
                [ordered]@{ Name = 'worker'; ProcessId = 1001; CpuDeltaMs = 220; WorkingSetBytes = 268435456; PrivateMemoryBytes = 234881024 }
                [ordered]@{ Name = 'worker'; ProcessId = 1002; CpuDeltaMs = 210; WorkingSetBytes = 251658240; PrivateMemoryBytes = 218103808 }
            )
            process_sample_skips = @()
            disks = @(
                [ordered]@{ Name = '10 H:'; DiskReadBytesPerSec = 5242880; AvgDiskQueueLength = 0.8 }
                [ordered]@{ Name = '11 I:'; DiskReadBytesPerSec = 7340032; AvgDiskQueueLength = 0.9 }
            )
        }
        [void]$system.Add(($systemObject | ConvertTo-Json -Depth 10 -Compress))
    }
    Write-Utf8NoBom -Path (Join-Path $Root 'system.ndjson') -Text (($system -join "`n") + "`n")
}

function Write-Fixture {
    <# 生成双盘、提前终态、Everything、Worker20/read12 的完整证据目录。 #>
    param(
        [Parameter(Mandatory)] [string] $Root,
        [ValidateSet('completed', 'failed', 'cancelled')] [string] $TerminalState = 'completed',
        [int] $TerminalAtSeconds = 2,
        [switch] $MissingSummary,
        [switch] $BrokenFooter,
        [switch] $MissingSecondDisk,
        [switch] $ConfigMismatch,
        [switch] $CapacityExceeded,
        [switch] $MissingOwnership,
        [switch] $RuntimeGap,
        [switch] $MediaChanged,
        [switch] $NodeUnexpectedExit
    )

    New-Item -ItemType Directory -Path $Root -Force | Out-Null
    Write-JsonLines -Root $Root -TerminalState $TerminalState -TerminalAtSeconds $TerminalAtSeconds `
        -MissingSecondDisk:$MissingSecondDisk -CapacityExceeded:$CapacityExceeded `
        -MissingOwnership:$MissingOwnership -RuntimeGap:$RuntimeGap
    $before = [ordered]@{
        Schema = 'rust-v2-media-manifest/v2'; Roots = @('H:\pik\00000000000', 'I:\tmp'); FileCount = 2; TotalBytes = 2048
        Files = @(
            [ordered]@{ RootIndex = 1; Root = 'H:\pik\00000000000'; Path = 'a.mp4'; Length = 1024; LastWriteTimeUtc = '2026-08-22T00:00:00.0000000Z' }
            [ordered]@{ RootIndex = 2; Root = 'I:\tmp'; Path = 'b.mp4'; Length = 1024; LastWriteTimeUtc = '2026-08-22T00:00:00.0000000Z' }
        )
    }
    Write-Utf8NoBom -Path (Join-Path $Root 'media-before.json') -Text ($before | ConvertTo-Json -Depth 10)
    $after = $before | ConvertTo-Json -Depth 10 | ConvertFrom-Json
    if ($MediaChanged) {
        $after.TotalBytes = 3072
        $after.FileCount = 3
    }
    Write-Utf8NoBom -Path (Join-Path $Root 'media-after.json') -Text ($after | ConvertTo-Json -Depth 10)
    $map = [ordered]@{
        schema = 'rust-v2-physical-disk-map/v1'
        entries = @(
            [ordered]@{ root = 'H:\pik\00000000000'; drive_letter = 'H'; partition_number = 4; disk_number = 10; friendly_name = 'Fixture HDD'; bus_type = 'SATA' }
            [ordered]@{ root = 'I:\tmp'; drive_letter = 'I'; partition_number = 8; disk_number = 11; friendly_name = 'Fixture SSD'; bus_type = 'NVMe' }
        )
        distinct_disk_numbers = @(10, 11)
    }
    $mapPath = Join-Path $Root 'physical-disk-map.json'
    Write-Utf8NoBom -Path $mapPath -Text ($map | ConvertTo-Json -Depth 10)
    $summary = if ($MissingSummary) { $null } else { Write-ResultSummaryTsv -Root $Root -BrokenFooter:$BrokenFooter }
    $harness = [ordered]@{
        schema_version = 3; run_status = if ($TerminalState -eq 'completed') { 'PASS' } else { 'FAIL' }
        media_unchanged = [bool](-not $MediaChanged.IsPresent); node_unexpected_exit = [bool]$NodeUnexpectedExit.IsPresent; exporter_exit_code = if ($MissingSummary) { 2 } else { 0 }
        media_roots = @('H:\pik\00000000000', 'I:\tmp'); enumerator = 'everything'; complete_when_task_terminal = $true
        effective_worker_count = if ($ConfigMismatch) { 19 } else { 20 }
        read_total_threads = if ($ConfigMismatch) { 11 } else { 12 }
        hdd_threads_per_disk = 1; ssd_threads_per_disk = 5; unknown_threads_per_disk = 1; reserved_cores = 1
        physical_disk_map_path = $mapPath; physical_disk_map_sha256 = (Get-FileHash -LiteralPath $mapPath -Algorithm SHA256).Hash.ToLowerInvariant()
        result_summary_path = Join-Path $Root 'result-summary.tsv'
        result_summary_sha256 = if ($summary) { $summary.Sha256 } else { ('0' * 64) }
        result_summary_status = if ($summary) { $summary.Status } else { 'MISSING' }
        result_summary_row_count = if ($summary) { $summary.RowCount } else { 0 }
        result_summary_missing_count = 0; result_summary_inconclusive_count = 0
        source_revision = 'fixture-revision'; source_tree_sha256 = ('a' * 64); package_path = 'C:\tmp\fixture-package.zip'; package_sha256 = ('b' * 64)
        release_root = 'C:\tmp\fixture-release'; config_sha256 = ('c' * 64); package_manifest_sha256 = ('d' * 64); package_manifest_status = 'PRESENT'
        media_before_sha256 = ('e' * 64); media_after_sha256 = ('e' * 64); run_diagnostic = $null
    }
    Write-Utf8NoBom -Path (Join-Path $Root 'harness-result.json') -Text ($harness | ConvertTo-Json -Depth 12)
    if ($MissingSummary) {
        # 旧三件套必须不存在；该分支专门验证报告能识别缺失 TSV。
        foreach ($old in @('result-summary.jsonl', 'result-summary-meta.json', 'result-summary.tsv.pair.lock')) {
            $oldPath = Join-Path $Root $old
            if (Test-Path -LiteralPath $oldPath) { Remove-Item -LiteralPath $oldPath -Force }
        }
    }
}

function Invoke-Report {
    <# 运行报告脚本并返回报告文本，统一隔离输出路径。 #>
    param([string] $Root)

    $path = Join-Path $Root 'report.md'
    & $reporter -EvidenceRoot $Root -OutputPath $path | Out-Null
    [IO.File]::ReadAllText($path)
}

try {
    if (-not (Test-Path -LiteralPath $reporter -PathType Leaf)) {
        throw "RUST_V2_RUNTIME_ACCEPTANCE_REPORTER_MISSING path=$reporter"
    }

    $passRoot = Join-Path $fixtureRoot 'pass-early-terminal'
    Write-Fixture -Root $passRoot
    $passText = Invoke-Report -Root $passRoot
    if ($passText -notmatch '结论：PASS' -or
        $passText -notmatch 'result-summary\.tsv' -or
        $passText -notmatch 'Everything' -or
        $passText -notmatch 'Worker.*20' -or
        $passText -notmatch '读取线程.*12' -or
        $passText -notmatch 'PhysicalDisk10' -or $passText -notmatch 'PhysicalDisk11' -or
        $passText -notmatch '任务终态') {
        throw "TSV 单次双盘提前终态应 PASS，实际报告：$passText"
    }
    if ($passText -match 'result-summary\.jsonl|result-summary-meta\.json|pair\.lock|结果摘要 Task ID|六轮|中位数') {
        throw '报告不应依赖旧 JSONL/metadata/lease/Task ID，也不应生成六轮中位数结论'
    }

    $missingRoot = Join-Path $fixtureRoot 'missing-tsv'
    Write-Fixture -Root $missingRoot -MissingSummary
    $missingText = Invoke-Report -Root $missingRoot
    if ($missingText -notmatch '结论：INCONCLUSIVE' -or $missingText -notmatch 'TSV') {
        throw '缺少 result-summary.tsv 必须是 INCONCLUSIVE'
    }

    $brokenRoot = Join-Path $fixtureRoot 'broken-footer'
    Write-Fixture -Root $brokenRoot -BrokenFooter
    $brokenText = Invoke-Report -Root $brokenRoot
    if ($brokenText -notmatch '结论：INCONCLUSIVE' -or $brokenText -notmatch 'footer|SHA|校验') {
        throw 'TSV footer/hash 错误必须是 INCONCLUSIVE'
    }

    $failedRoot = Join-Path $fixtureRoot 'task-failed'
    Write-Fixture -Root $failedRoot -TerminalState failed
    $failedText = Invoke-Report -Root $failedRoot
    if ($failedText -notmatch '结论：FAIL' -or $failedText -notmatch '任务.*失败') {
        throw '任务终态 failed 必须是 FAIL'
    }

    $configRoot = Join-Path $fixtureRoot 'config-mismatch'
    Write-Fixture -Root $configRoot -ConfigMismatch
    $configText = Invoke-Report -Root $configRoot
    if ($configText -notmatch '结论：FAIL' -or $configText -notmatch 'Worker.*20|读取线程.*12') {
        throw 'Worker/read 配置不匹配必须是 FAIL'
    }

    $diskRoot = Join-Path $fixtureRoot 'second-disk-missing'
    Write-Fixture -Root $diskRoot -MissingSecondDisk
    $diskText = Invoke-Report -Root $diskRoot
    if ($diskText -notmatch '结论：INCONCLUSIVE' -or $diskText -notmatch 'PhysicalDisk11|双盘|物理盘') {
        throw '第二物理盘采样缺失必须是 INCONCLUSIVE'
    }

    $capacityRoot = Join-Path $fixtureRoot 'capacity-exceeded'
    Write-Fixture -Root $capacityRoot -CapacityExceeded
    $capacityText = Invoke-Report -Root $capacityRoot
    if ($capacityText -notmatch '结论：FAIL' -or $capacityText -notmatch '容量') {
        throw '队列容量越界必须是 FAIL'
    }

    $ownershipRoot = Join-Path $fixtureRoot 'ownership-missing'
    Write-Fixture -Root $ownershipRoot -MissingOwnership
    $ownershipText = Invoke-Report -Root $ownershipRoot
    if ($ownershipText -notmatch '结论：INCONCLUSIVE' -or $ownershipText -notmatch 'worker_feature|缺失字段') {
        throw 'ownership 字段缺失必须是 INCONCLUSIVE'
    }

    $timeRoot = Join-Path $fixtureRoot 'runtime-time-gap'
    Write-Fixture -Root $timeRoot -RuntimeGap
    $timeText = Invoke-Report -Root $timeRoot
    if ($timeText -notmatch '结论：INCONCLUSIVE' -or $timeText -notmatch '时间间隔|sample_interval') {
        throw '任务快照时间轴不连续必须是 INCONCLUSIVE'
    }

    $mediaRoot = Join-Path $fixtureRoot 'media-changed'
    Write-Fixture -Root $mediaRoot -MediaChanged
    $mediaText = Invoke-Report -Root $mediaRoot
    if ($mediaText -notmatch '结论：FAIL' -or $mediaText -notmatch '媒体清单发生变化') {
        throw '媒体清单变化必须是 FAIL'
    }

    $nodeExitRoot = Join-Path $fixtureRoot 'node-unexpected-exit'
    Write-Fixture -Root $nodeExitRoot -NodeUnexpectedExit
    $nodeExitText = Invoke-Report -Root $nodeExitRoot
    if ($nodeExitText -notmatch '结论：FAIL' -or $nodeExitText -notmatch '非预期退出') {
        throw 'Node 非预期退出必须是 FAIL'
    }

    Write-Output 'RUST_V2_RUNTIME_ACCEPTANCE_REPORT_PASS'
}
finally {
    if (Test-Path -LiteralPath $fixtureRoot) {
        Remove-Item -LiteralPath $fixtureRoot -Recurse -Force
    }
}
