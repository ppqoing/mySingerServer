<#
.SYNOPSIS
使用901个两秒合成样本验证半小时中文报告和失败条件。
#>
[CmdletBinding()]
param(
    [switch] $ZeroPassOnly,
    [switch] $ZeroInconclusiveOnly,
    [switch] $StateEvidenceOnly,
    [switch] $Task8ReviewOnly
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$reporter = Join-Path $repositoryRoot 'tests\windows\New-RustV2RuntimeAcceptanceReport.ps1'
$fixtureRoot = Join-Path ([IO.Path]::GetTempPath()) ("rust-v2-runtime-report-" + [Guid]::NewGuid().ToString('N'))

function Write-Fixture {
    param(
        [string] $Root,
        [int] $DurationSeconds,
        [ValidateRange(1, 2)] [int] $RuntimeSampleSeconds = 2,
        [ValidateSet('A', 'B')] [string] $Variant = 'A',
        [ValidateSet('PASS', 'MISSING', 'INCONCLUSIVE')] [string] $SummaryStatus = 'PASS',
        [int] $RuntimeGapAtSeconds = -1,
        [int] $SystemGapAtSeconds = -1,
        [switch] $MediaChanged,
        [switch] $CapacityExceeded,
        [switch] $WorkerCrashObserved,
        [switch] $AllWorkerSamplesEmpty,
        [switch] $CacheWaitOwnershipViolation,
        [switch] $NodeUnexpectedExit,
        [switch] $NonDeadlineCancelled,
        [switch] $OwnershipExceeded,
        [switch] $TaskFailed,
        [switch] $CreditBroken,
        [switch] $FinalizationTail,
        [switch] $MissingCompletedTaskId,
        [switch] $MissingUtcTimestamp,
        [switch] $MissingOwnershipField,
        [switch] $IrregularIntervals,
        [switch] $SingleRun,
        [ValidateSet('completed', 'failed', 'cancelled')]
        [string] $SingleRunTerminalState = 'completed',
        [switch] $ManifestV2,
        [switch] $RuntimeRootsMismatch,
        [switch] $OnlyFirstRootObserved,
        [switch] $SequentialDiskRequests,
        [switch] $DelayedDiskRequests,
        [switch] $TailOnlyDiskOverlap,
        [switch] $MissingDiskReads,
        [switch] $DiskCapacityExceeded,
        [switch] $DiskReleasedExceeded,
        [switch] $DiskTerminalNonZero,
        [switch] $NoTaskTerminal,
        [switch] $ProcessSampleSkip
    )

    New-Item -ItemType Directory -Path $Root -Force | Out-Null
    $runtimePath = Join-Path $Root 'runtime.ndjson'
    $systemPath = Join-Path $Root 'system.ndjson'
    # 先在内存中组装固定样本，再一次性写 NDJSON；保持901个样本和所有门禁，避免逐行打开文件拖慢 fixture。
    $runtimeLines = [Collections.Generic.List[string]]::new()
    $systemLines = [Collections.Generic.List[string]]::new()
    $machine = 'a' * 64
    $baseUnixMs = 1787356800000
    $runtimeStepSeconds = [Math]::Max(1, $RuntimeSampleSeconds)
    $previousSampleUnixMs = $null
    for ($elapsed = 0; $elapsed -le $DurationSeconds; $elapsed += $runtimeStepSeconds) {
        $runtimeGapMs = if ($RuntimeGapAtSeconds -ge 0 -and $elapsed -ge $RuntimeGapAtSeconds) { 3000 } else { 0 }
        $irregularOffsetMs = if ($IrregularIntervals -and $elapsed -ge 100 -and $elapsed -lt 110) { 250 } elseif ($IrregularIntervals -and $elapsed -ge 110 -and $elapsed -lt 120) { -150 } else { 0 }
        $sampleUnixMs = $baseUnixMs + ($elapsed * 1000) + $runtimeGapMs + $irregularOffsetMs
        $sampleIntervalMs = if ($null -eq $previousSampleUnixMs) { 0 } else { $sampleUnixMs - $previousSampleUnixMs }
        $previousSampleUnixMs = $sampleUnixMs
        $terminal = $elapsed -eq $DurationSeconds
        $ownershipPeak = if ($OwnershipExceeded) { 30 } else { 2 }
        # 让 fixture 的 current 服从设计中的逐项守恒公式，终态统一归零；peak 仍可单独制造越界。
        $hashWaitingCurrent = if ($terminal) { 0 } else { 1 }
        $hashReadingCurrent = if ($terminal) { 0 } else { 1 }
        $hashCompletedCurrent = 0
        $mediaWaitingCurrent = if ($terminal) { 0 } else { 1 }
        $mediaReadyCurrent = 0
        $mediaPermitReadyCurrent = 0
        $workerDispatchCurrent = 0
        $workerStartPendingCurrent = 0
        $workerDecodeCurrent = if ($terminal) { 0 } else { 1 }
        $workerFeatureCurrent = if ($terminal) { 0 } else { 1 }
        $workerResultWaitCurrent = 0
        $workerUnknownCurrent = 0
        $contentCreditCurrent = if ($terminal) { 0 } else { 1 }
        $refillTokenCurrent = if ($terminal) { 0 } else { 1 }
        $decodeCreditCurrent = if ($terminal) { 0 } elseif ($CreditBroken) { 4 } else { 3 }
        # 逐盘 fixture 直接构造协议值；不使用 Worker 路径反推许可状态。
        $requestSequence = [int]($elapsed / $runtimeStepSeconds) + 1
        $diskSplitSeconds = [int][Math]::Floor($DurationSeconds / 2)
        $staggeredDiskRequests = $SequentialDiskRequests -or $DelayedDiskRequests -or $TailOnlyDiskOverlap
        $disk11StartSeconds = if ($SequentialDiskRequests) { $diskSplitSeconds } else { $diskSplitSeconds + (2 * $runtimeStepSeconds) }
        $tailOverlapVisible = $TailOnlyDiskOverlap -and $elapsed -ge ($DurationSeconds - 10)
        $disk10Visible = -not $terminal -and (-not $staggeredDiskRequests -or $elapsed -lt $diskSplitSeconds -or $tailOverlapVisible)
        $disk11Visible = -not $terminal -and (-not $staggeredDiskRequests -or $elapsed -ge $disk11StartSeconds)
        $disk10Granted = if ($staggeredDiskRequests) {
            $disk10BaseGranted = [Math]::Min($requestSequence, [int][Math]::Ceiling($diskSplitSeconds / $runtimeStepSeconds))
            if ($TailOnlyDiskOverlap -and $elapsed -ge ($DurationSeconds - 10)) { $disk10BaseGranted + 1 } else { $disk10BaseGranted }
        }
        else { $requestSequence }
        $disk11Granted = if ($staggeredDiskRequests -and $elapsed -lt $disk11StartSeconds) {
            0
        }
        elseif ($staggeredDiskRequests) {
            [int](($elapsed - $disk11StartSeconds) / $runtimeStepSeconds) + 1
        }
        else { $requestSequence }
        $runtime = [ordered]@{
            record_type = 'runtime_sample'
            utc_unix_ms = $sampleUnixMs
            elapsed_seconds = $elapsed
            sample_interval_ms = $sampleIntervalMs
            runtime_task_id = 'runtime-fixture'
            machine_id = $machine
            state = if ($terminal) { if ($SingleRun) { $SingleRunTerminalState } else { 'cancelled' } } else { 'running' }
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
                },
                [ordered]@{
                    stage_id = 'base_compute'; display_name = '基础计算'; state = if ($terminal) { 3 } else { 2 }; unit = 'items'
                    completed = [Math]::Min(900, [int]($elapsed / 2)); total = 900; total_known = $true
                    failed = 0; skipped = 0; speed_per_second = 0.5; elapsed_ms = $elapsed * 1000; eta_ms = 0
                }
            )
            # 真实终态样本没有在途 Worker，用空数组覆盖报告器的严格模式兼容性。
            workers = if ($AllWorkerSamplesEmpty -or $elapsed -eq $DurationSeconds) {
                @()
            }
            else {
                @(
                    [ordered]@{ slot = 0; process_id = 1001; stage_id = 'base_compute'; current_step = '媒体解码'; cache_detail = ''; display_path = if ($ManifestV2) { 'H:\pik\00000000000\a.mp4' } else { 'D:\Media\a.mp4' }; physical_disk_id = 'PhysicalDrive0'; completed_files = [int]($elapsed / 4); speed_per_second = 0.5; phase = 'decode'; cpu_weight = 2; decoder_threads = 2 },
                    [ordered]@{ slot = 1; process_id = 1002; stage_id = 'base_compute'; current_step = '特征计算'; cache_detail = ''; display_path = if ($ManifestV2 -and $OnlyFirstRootObserved) { '' } elseif ($ManifestV2) { 'I:\tmp\b.mp4' } else { 'E:\Media\b.mp4' }; physical_disk_id = 'PhysicalDrive1'; completed_files = [int]($elapsed / 4); speed_per_second = 0.5; phase = 'feature'; cpu_weight = 3; decoder_threads = 3 },
                    [ordered]@{ slot = 2; process_id = 1003; stage_id = ''; current_step = '空闲'; cache_detail = ''; display_path = ''; physical_disk_id = ''; completed_files = 0; speed_per_second = 0; phase = 'idle'; cpu_weight = $null; decoder_threads = $null }
                )
            }
            failures = if ($WorkerCrashObserved -and $elapsed -eq 100) {
                @([ordered]@{ stage_id = 'base_compute'; display_path = 'D:\Media\a.mp4'; message = 'Worker 崩溃后文件级失败' })
            } elseif ($CacheWaitOwnershipViolation -and $elapsed -eq 100) {
                @([ordered]@{ stage_id = 'base_compute'; display_path = 'D:\Media\a.mp4'; message = 'CACHE_WAIT_RESOURCE_OWNERSHIP_VIOLATION' })
            } elseif ($NonDeadlineCancelled -and $elapsed -eq 100) {
                @([ordered]@{ stage_id = 'base_compute'; display_path = 'D:\Media\a.mp4'; message = 'scan cancelled by operator' })
            } else { @() }
            execution_config = [ordered]@{
                hash_tasks = 16; path_cache_queue_capacity = 24; content_cache_queue_capacity = 48
                decode_queue_capacity = 24; persist_queue_capacity = 1012; worker_slots = 12
                cpu_budget = 23; global_disk_permits = 16; hdd_per_disk_permits = 1
                ssd_per_disk_permits = 16; unknown_per_disk_permits = 1
            }
            pipeline_metrics = [ordered]@{
                hash_queue = [ordered]@{ current = if ($terminal) { 0 } else { 2 }; peak = 8; capacity = 16; wait_latency = [ordered]@{ count = 2; p50_ms = 1; p95_ms = 4; p99_ms = 4; max_ms = 4; buckets = @() }; service_latency = [ordered]@{ count = 2; p50_ms = 2; p95_ms = 5; p99_ms = 5; max_ms = 5; buckets = @() } }
                path_cache_queue = [ordered]@{ current = 1; peak = 4; capacity = 24; wait_latency = $null; service_latency = $null }
                content_cache_queue = [ordered]@{ current = 1; peak = 8; capacity = 48; wait_latency = $null; service_latency = $null }
                decode_queue = [ordered]@{ current = if ($terminal) { 0 } else { 2 }; peak = $(if ($CapacityExceeded) { 25 } else { 12 }); capacity = 24; wait_latency = [ordered]@{ count = 3; p50_ms = 2; p95_ms = 7; p99_ms = 9; max_ms = 11; buckets = @() }; service_latency = $null }
                persist_queue = [ordered]@{ current = 1; peak = 9; capacity = 1012; wait_latency = $null; service_latency = [ordered]@{ count = 3; p50_ms = 2; p95_ms = 6; p99_ms = 8; max_ms = 10; buckets = @() } }
                hash_io = [ordered]@{ current = 1; peak = 8; capacity = 16; wait_latency = $null; service_latency = $null }
                media_io = [ordered]@{ current = 2; peak = 10; capacity = 16; wait_latency = $null; service_latency = $null }
                cpu_weight = [ordered]@{ current = 5; peak = 18; capacity = 23; wait_latency = $null; service_latency = $null }
                worker_slots = [ordered]@{ current = if ($terminal) { 0 } else { 2 }; peak = 12; capacity = 12; wait_latency = $null; service_latency = $null }
                hash_bytes = 1073741824
                media_throughput = @([ordered]@{ media_kind = 2; size_bucket = 'large'; files = 6; bytes = 2147483648 })
                hash_waiting_permit = [ordered]@{ current = $hashWaitingCurrent; peak = $ownershipPeak; capacity = 16 }
                hash_reading = [ordered]@{ current = $hashReadingCurrent; peak = $ownershipPeak; capacity = 16 }
                hash_completed_unjoined = [ordered]@{ current = $hashCompletedCurrent; peak = $ownershipPeak; capacity = 16 }
                media_permit_waiting = [ordered]@{ current = $mediaWaitingCurrent; peak = $ownershipPeak; capacity = 16 }
                media_acquire_ready = [ordered]@{ current = $mediaReadyCurrent; peak = $ownershipPeak; capacity = 16 }
                media_permit_ready = [ordered]@{ current = $mediaPermitReadyCurrent; peak = $ownershipPeak; capacity = 16 }
                worker_dispatching = [ordered]@{ current = $workerDispatchCurrent; peak = $ownershipPeak; capacity = 12 }
                worker_start_pending = [ordered]@{ current = $workerStartPendingCurrent; peak = $ownershipPeak; capacity = 24 }
                worker_decode = [ordered]@{ current = $workerDecodeCurrent; peak = $ownershipPeak; capacity = 24 }
                worker_feature = [ordered]@{ current = $workerFeatureCurrent; peak = $ownershipPeak; capacity = 12 }
                worker_result_wait = [ordered]@{ current = $workerResultWaitCurrent; peak = $ownershipPeak; capacity = 12 }
                worker_phase_unknown = [ordered]@{ current = $workerUnknownCurrent; peak = $ownershipPeak; capacity = 12 }
                content_output_credit_owned = if ($Variant -eq 'B') { [ordered]@{ current = $contentCreditCurrent; peak = $ownershipPeak; capacity = 48 } } else { $null }
                hash_refill_token_available = if ($Variant -eq 'B') { [ordered]@{ current = $refillTokenCurrent; peak = $ownershipPeak; capacity = 16 } } else { $null }
                decode_credit_owned = if ($Variant -eq 'B') { [ordered]@{ current = $decodeCreditCurrent; peak = $ownershipPeak; capacity = 24 } } else { $null }
                item_completion_latency = [ordered]@{ count = 2; p50_ms = 10; p95_ms = 20; p99_ms = 30; max_ms = 40; buckets = @() }
                disk_reads = @(
                    [ordered]@{
                        physical_disk_id = 'PhysicalDisk10'; capacity = 4
                        hash_waiting = if ($DiskCapacityExceeded -and -not $terminal) { 5 } elseif ($disk10Visible) { 1 } else { 0 }
                        media_waiting = 0; hash_active = if ($disk10Visible) { 1 } else { 0 }; media_active = 0
                        hash_granted_total = $disk10Granted; media_granted_total = 0
                        hash_released_total = if ($DiskReleasedExceeded) { $disk10Granted + 1 } else { [Math]::Max(0, $disk10Granted - $(if ($disk10Visible) { 1 } else { 0 })) }
                        media_released_total = 0
                    },
                    [ordered]@{
                        physical_disk_id = 'PhysicalDisk11'; capacity = 4
                        hash_waiting = 0; media_waiting = if ($disk11Visible) { 1 } else { 0 }; hash_active = 0
                        media_active = if ($DiskTerminalNonZero -and $terminal) { 1 } elseif ($disk11Visible) { 1 } else { 0 }
                        hash_granted_total = 0; media_granted_total = $disk11Granted; hash_released_total = 0
                        media_released_total = [Math]::Max(0, $disk11Granted - $(if ($disk11Visible) { 1 } else { 0 }))
                    }
                )
            }
        }
        if ($FinalizationTail -and $elapsed -ge ($DurationSeconds - 10)) {
            # 终结阶段必须存在于 fixture，报告器应把它单列而不计入生产窗口。
            $baseComputeStage = @($runtime.stages | Where-Object stage_id -eq 'base_compute')[-1]
            $baseComputeStage.display_name = 'Finalize'
            $baseComputeStage.state = 3
            $baseComputeStage.completed = 900
            $baseComputeStage.speed_per_second = 0
        }
        if ($MissingUtcTimestamp) {
            [void]$runtime.Remove('utc_unix_ms')
        }
        if ($MissingOwnershipField) {
            [void]$runtime.pipeline_metrics.Remove('worker_feature')
        }
        if ($MissingDiskReads) {
            [void]$runtime.pipeline_metrics.Remove('disk_reads')
        }
        if ($NoTaskTerminal -and $terminal) {
            $runtime.state = 'running'
        }
        [void]$runtimeLines.Add(($runtime | ConvertTo-Json -Depth 10 -Compress))
        if (($elapsed % 2) -ne 0) {
            continue
        }

        $system = [ordered]@{
            record_type = 'system_sample'
            utc = ([DateTime]'2026-08-22T00:00:00Z').AddSeconds($elapsed).ToString('O')
            utc_unix_ms = $baseUnixMs + ($elapsed * 1000)
            elapsed_seconds = $elapsed
            sample_interval_ms = if ($elapsed -eq 0) { 0 } else { 2000 }
            processes = @(
                [ordered]@{ Name = 'node'; ProcessId = 900; CpuDeltaMs = 40; WorkingSetBytes = 134217728; PrivateMemoryBytes = 100663296 },
                [ordered]@{ Name = 'worker'; ProcessId = 1001; CpuDeltaMs = 500; WorkingSetBytes = 268435456; PrivateMemoryBytes = 234881024 },
                [ordered]@{ Name = 'worker'; ProcessId = 1002; CpuDeltaMs = 450; WorkingSetBytes = 251658240; PrivateMemoryBytes = 218103808 }
            )
            process_sample_skips = if ($ProcessSampleSkip -and $elapsed -eq 100) {
                @([ordered]@{ process_id = 1002; reason = 'PROCESS_EXITED_DURING_SAMPLE' })
            }
            else { @() }
            disks = @(
                [ordered]@{ Name = '0 C:'; DiskReadBytesPerSec = 1048576; AvgDiskQueueLength = 0.4 },
                [ordered]@{ Name = '1 D:'; DiskReadBytesPerSec = 2097152; AvgDiskQueueLength = 0.8 }
            )
        }
        if ($MissingUtcTimestamp) {
            [void]$system.Remove('utc_unix_ms')
        }
        [void]$systemLines.Add(($system | ConvertTo-Json -Depth 8 -Compress))
    }
    [void]$runtimeLines.Add((([ordered]@{
        record_type = 'runtime_result'; duration_seconds = $DurationSeconds
            sample_count = [int]($DurationSeconds / $runtimeStepSeconds); scans_started = 1; failed_scans = if ($TaskFailed) { 1 } else { 0 }
        cancelled_at_deadline = $true
        latest_completed_persistent_task_id = if ($MissingCompletedTaskId -or ($SingleRun -and $SingleRunTerminalState -ne 'completed')) { '' } else { 'task-completed' }
        deadline_cancelled_persistent_task_id = if ($NonDeadlineCancelled) { '' } else { 'task-deadline' }
        correctness = if ($SingleRun -and $SingleRunTerminalState -eq 'failed') { 'FAIL' } else { $SummaryStatus }
        media_roots = if ($ManifestV2 -and $RuntimeRootsMismatch) { @('H:\pik\00000000000', 'I:\wrong-root') } elseif ($ManifestV2) { @('H:\pik\00000000000', 'I:\tmp') } else { @('D:\Media') }
        single_run = [bool]$SingleRun
        scan_tasks = @(
            if (-not $SingleRun -and -not $NoTaskTerminal) { [ordered]@{ persistent_task_id = 'task-completed'; runtime_task_id = 'runtime-completed'; terminal_state = 'completed' } }
            [ordered]@{ persistent_task_id = if ($SingleRun) { 'task-completed' } elseif ($NonDeadlineCancelled) { 'task-other' } else { 'task-deadline' }; runtime_task_id = 'runtime-fixture'; terminal_state = if ($NoTaskTerminal) { $null } elseif ($SingleRun) { $SingleRunTerminalState } elseif ($TaskFailed) { 'failed' } else { 'cancelled' } }
        )
    }) | ConvertTo-Json -Compress))
    [IO.File]::WriteAllLines($runtimePath, $runtimeLines, [Text.UTF8Encoding]::new($false))
    [IO.File]::WriteAllLines($systemPath, $systemLines, [Text.UTF8Encoding]::new($false))

    $before = if ($ManifestV2) {
        [ordered]@{
            Schema = 'rust-v2-media-manifest/v2'; Roots = @('H:\pik\00000000000', 'I:\tmp'); FileCount = 2; TotalBytes = 300
            Files = @(
                [ordered]@{ RootIndex = 1; Root = 'H:\pik\00000000000'; Path = 'a.mp4'; Length = 100; LastWriteTimeUtc = '2026-08-22T00:00:00.0000000Z' },
                [ordered]@{ RootIndex = 2; Root = 'I:\tmp'; Path = 'b.mp4'; Length = 200; LastWriteTimeUtc = '2026-08-22T00:00:00.0000000Z' }
            )
        }
    } else {
        [ordered]@{
            Root = 'D:\Media'; FileCount = 2; TotalBytes = 300
            Files = @(
                [ordered]@{ Path = 'a.mp4'; Length = 100; LastWriteTimeUtc = '2026-08-22T00:00:00.0000000Z' },
                [ordered]@{ Path = 'b.mp4'; Length = 200; LastWriteTimeUtc = '2026-08-22T00:00:00.0000000Z' }
            )
        }
    }
    $after = $before | ConvertTo-Json -Depth 8 | ConvertFrom-Json
    if ($MediaChanged) {
        $after.TotalBytes = 301
        $after.Files = @(
            [ordered]@{ Path = 'a.mp4'; Length = 101; LastWriteTimeUtc = '2026-08-22T00:00:01.0000000Z' },
            $before.Files[1]
        )
    }
    $before | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath (Join-Path $Root 'media-before.json') -Encoding utf8
    $after | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath (Join-Path $Root 'media-after.json') -Encoding utf8
    $rootBeforePaths = @()
    $rootAfterPaths = @()
    $rootBeforeSha256 = @()
    $rootAfterSha256 = @()
    if ($ManifestV2) {
        $rootIndex = 0
        foreach ($manifestRoot in @($before.Roots)) {
            $rootIndex++
            $rootBefore = [ordered]@{ Root = $manifestRoot; FileCount = 1; TotalBytes = if ($rootIndex -eq 1) { 100 } else { 200 }; Files = @($before.Files[$rootIndex - 1]) }
            $rootAfter = [ordered]@{ Root = $manifestRoot; FileCount = 1; TotalBytes = $rootBefore.TotalBytes; Files = @($after.Files[$rootIndex - 1]) }
            $rootBeforePath = Join-Path $Root ('media-before-root-{0:d2}.json' -f $rootIndex)
            $rootAfterPath = Join-Path $Root ('media-after-root-{0:d2}.json' -f $rootIndex)
            $rootBefore | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $rootBeforePath -Encoding utf8
            $rootAfter | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $rootAfterPath -Encoding utf8
            $rootBeforePaths += $rootBeforePath
            $rootAfterPaths += $rootAfterPath
            $rootBeforeSha256 += (Get-FileHash -LiteralPath $rootBeforePath -Algorithm SHA256).Hash.ToLowerInvariant()
            $rootAfterSha256 += (Get-FileHash -LiteralPath $rootAfterPath -Algorithm SHA256).Hash.ToLowerInvariant()
        }
        $physicalDiskEntries = @(
            [ordered]@{ root = 'H:\pik\00000000000'; drive_letter = 'H'; partition_number = 4; disk_number = 10; friendly_name = 'Fixture HDD'; bus_type = 'SATA' }
            [ordered]@{ root = 'I:\tmp'; drive_letter = 'I'; partition_number = 8; disk_number = 11; friendly_name = 'Fixture SSD'; bus_type = 'NVMe' }
        )
        $physicalDiskMap = [ordered]@{
            schema = 'rust-v2-physical-disk-map/v1'; roots = $physicalDiskEntries; entries = $physicalDiskEntries
            distinct_disk_numbers = @(10, 11)
        }
        $physicalDiskMap | ConvertTo-Json -Depth 10 | Set-Content -LiteralPath (Join-Path $Root 'physical-disk-map.json') -Encoding utf8
    }
    [ordered]@{
        run_root = (Split-Path -Parent $Root)
        evidence_root = $Root
        duration_seconds = $DurationSeconds
        media_unchanged = (-not $MediaChanged)
        effective_worker_count = 12
        hdd_threads_per_disk = 1
        read_total_threads = 16
        ssd_threads_per_disk = 16
        unknown_threads_per_disk = 1
        reserved_cores = 1
        node_unexpected_exit = [bool]$NodeUnexpectedExit
        contact_sheet_reuse_count = 3
        disk_full_cleanup_count = 0
        schema_version = 2
        variant = $Variant
        run_index = 1
        source_revision = 'fixture-revision'
        source_tree_sha256 = ('a' * 64)
        package_path = 'C:\tmp\fixture-package.zip'
        package_sha256 = ('b' * 64)
        release_root = 'C:\tmp\fixture-release'
        config_sha256 = ('c' * 64)
        package_manifest_sha256 = ('d' * 64)
        package_manifest_status = 'PRESENT'
        media_before_sha256 = ('e' * 64)
        media_after_sha256 = ('e' * 64)
        result_summary_path = (Join-Path $Root 'result-summary.jsonl')
        result_summary_sha256 = ''
        result_summary_status = $SummaryStatus
        result_summary_task_id = if ($MissingCompletedTaskId -or ($SingleRun -and $SingleRunTerminalState -ne 'completed')) { '' } else { 'task-completed' }
        result_summary_row_count = 1
        result_summary_missing_count = if ($SummaryStatus -eq 'MISSING') { 1 } else { 0 }
        result_summary_inconclusive_count = if ($SummaryStatus -eq 'INCONCLUSIVE') { 1 } else { 0 }
        run_status = if ($SingleRun -and $SingleRunTerminalState -eq 'failed') { 'FAIL' } else { $SummaryStatus }
        media_roots = if ($ManifestV2) { @('H:\pik\00000000000', 'I:\tmp') } else { @('D:\Media') }
        single_run = [bool]$SingleRun
        physical_disk_map_path = if ($ManifestV2) { Join-Path $Root 'physical-disk-map.json' } else { $null }
        physical_disk_map_sha256 = if ($ManifestV2) { ('f' * 64) } else { $null }
         media_before_root_paths = $rootBeforePaths
         media_after_root_paths = $rootAfterPaths
         media_before_root_sha256 = $rootBeforeSha256
         media_after_root_sha256 = $rootAfterSha256
        exporter_exit_code = if ($SummaryStatus -eq 'PASS') { 0 } else { -1 }
        deadline_cancelled_persistent_task_id = 'task-deadline'
        cache_wait_resource_ownership_violations = if ($CacheWaitOwnershipViolation) { 1 } else { 0 }
    } | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $Root 'harness-result.json') -Encoding utf8
    $summaryPath = Join-Path $Root 'result-summary.jsonl'
    # 真实三件套：摘要、metadata、lease token 互相绑定，正文使用真实 LF。
    [IO.File]::WriteAllText($summaryPath, "{`"status`":`"$SummaryStatus`"}" + [char]10, [Text.UTF8Encoding]::new($false))
    $summarySha256 = (Get-FileHash -LiteralPath $summaryPath -Algorithm SHA256).Hash.ToLowerInvariant()
    $metadata = [ordered]@{
        schema_version = 1; lease_token = 'fixture-lease-token'; canonical_sha256 = $summarySha256
        task_id = if ($MissingCompletedTaskId) { '' } else { 'task-completed' }
        task_status = $SummaryStatus; status = $SummaryStatus; row_count = 1
        missing_count = if ($SummaryStatus -eq 'MISSING') { 1 } else { 0 }
        inconclusive_count = if ($SummaryStatus -eq 'INCONCLUSIVE') { 1 } else { 0 }
    }
    $lease = [ordered]@{
        schema_version = 1; lease_token = 'fixture-lease-token'
        expected_canonical_identity = [ordered]@{ first = 1; second = 2 }
        expected_metadata_identity = [ordered]@{ first = 1; second = 3 }
        expected_canonical_sha256 = $summarySha256
        expected_status = $SummaryStatus; expected_row_count = 1
        run_evidence_dir = 'fixture-evidence'
    }
    [IO.File]::WriteAllText((Join-Path $Root 'result-summary-meta.json'), ($metadata | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
    [IO.File]::WriteAllText("$summaryPath.pair.lock", ($lease | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
    $harnessObject = Get-Content -LiteralPath (Join-Path $Root 'harness-result.json') -Raw | ConvertFrom-Json
    $harnessObject.result_summary_sha256 = (Get-FileHash -LiteralPath $summaryPath -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($ManifestV2) {
        $harnessObject.physical_disk_map_sha256 = (Get-FileHash -LiteralPath (Join-Path $Root 'physical-disk-map.json') -Algorithm SHA256).Hash.ToLowerInvariant()
    }
    $harnessObject | ConvertTo-Json -Depth 10 | Set-Content -LiteralPath (Join-Path $Root 'harness-result.json') -Encoding utf8
    if ($SystemGapAtSeconds -ge 0) {
        $systemLines = @(Get-Content -LiteralPath $systemPath)
        for ($index = 0; $index -lt $systemLines.Count; $index++) {
            $system = $systemLines[$index] | ConvertFrom-Json
            if ([int]$system.elapsed_seconds -ge $SystemGapAtSeconds) {
                $system.utc_unix_ms = [int64]$system.utc_unix_ms + 7000
                $system.utc = ([DateTimeOffset]::FromUnixTimeMilliseconds([int64]$system.utc_unix_ms)).UtcDateTime.ToString('O')
                $systemLines[$index] = $system | ConvertTo-Json -Depth 10 -Compress
            }
        }
        [IO.File]::WriteAllLines($systemPath, $systemLines, [Text.UTF8Encoding]::new($false))
    }
}

function Set-JsonPropertyForTest {
    <# 修改单轮 fixture 的 JSON 字段，模拟真实证据缺失或状态变化。 #>
    param(
        [Parameter(Mandatory)] [string] $Path,
        [Parameter(Mandatory)] [string] $PropertyName,
        [object] $Value,
        [switch] $Remove
    )

    $document = [IO.File]::ReadAllText($Path) | ConvertFrom-Json
    if ($Remove) {
        if ($null -eq $document.PSObject.Properties[$PropertyName]) {
            throw "测试字段不存在，不能删除：$PropertyName"
        }
        $null = $document.PSObject.Properties.Remove($PropertyName)
    }
    else {
        $property = $document.PSObject.Properties[$PropertyName]
        if ($null -eq $property) {
            $document | Add-Member -MemberType NoteProperty -Name $PropertyName -Value $Value
        }
        else {
            $property.Value = $Value
        }
    }
    [IO.File]::WriteAllText($Path, ($document | ConvertTo-Json -Depth 12), [Text.UTF8Encoding]::new($false))
}

function Set-RuntimeResultPropertyForTest {
    <# 只修改最后一条 runtime_result，保持每行 JSONL 与 UTF-8 无 BOM。 #>
    param(
        [Parameter(Mandatory)] [string] $Root,
        [Parameter(Mandatory)] [string] $PropertyName,
        [object] $Value,
        [switch] $Remove
    )

    $path = Join-Path $Root 'runtime.ndjson'
    $lines = [Collections.Generic.List[string]]::new()
    $found = $false
    foreach ($line in [IO.File]::ReadAllLines($path)) {
        $record = $line | ConvertFrom-Json
        if ([string]$record.record_type -eq 'runtime_result') {
            if ($Remove) {
                if ($null -eq $record.PSObject.Properties[$PropertyName]) {
                    throw "测试字段不存在，不能删除：$PropertyName"
                }
                $null = $record.PSObject.Properties.Remove($PropertyName)
            }
            else {
                $property = $record.PSObject.Properties[$PropertyName]
                if ($null -eq $property) {
                    $record | Add-Member -MemberType NoteProperty -Name $PropertyName -Value $Value
                }
                else {
                    $property.Value = $Value
                }
            }
            $found = $true
        }
        [void]$lines.Add(($record | ConvertTo-Json -Depth 12 -Compress))
    }
    if (-not $found) { throw '测试 fixture 缺少 runtime_result' }
    [IO.File]::WriteAllLines($path, $lines, [Text.UTF8Encoding]::new($false))
}

function Convert-ToTwoTaskDiskFixture {
    <# 把持续运行 fixture 切成两个任务；每个任务的逐盘累计值独立从零开始并拥有自己的终态。 #>
    param(
        [Parameter(Mandatory)] [string] $Root,
        [switch] $SecondTaskNotTerminal,
        [switch] $FirstTaskDelayedRequests
    )

    $path = Join-Path $Root 'runtime.ndjson'
    $records = @([IO.File]::ReadAllLines($path) | Where-Object { $_ } | ForEach-Object { $_ | ConvertFrom-Json })
    $runtimeSamples = @($records | Where-Object record_type -eq 'runtime_sample')
    $splitIndex = [int][Math]::Floor(($runtimeSamples.Count - 1) / 2)
    for ($sampleIndex = 0; $sampleIndex -lt $runtimeSamples.Count; $sampleIndex++) {
        $sample = $runtimeSamples[$sampleIndex]
        $isFirstTask = $sampleIndex -le $splitIndex
        $localIndex = if ($isFirstTask) { $sampleIndex } else { $sampleIndex - $splitIndex - 1 }
        $isTaskTerminal = $sampleIndex -eq $splitIndex -or $sampleIndex -eq ($runtimeSamples.Count - 1)
        $taskId = if ($isFirstTask) { 'runtime-task-a' } else { 'runtime-task-b' }
        $sample.runtime_task_id = $taskId
        $sample.state = if ($isTaskTerminal -and $isFirstTask) {
            'completed'
        }
        elseif ($isTaskTerminal -and -not $SecondTaskNotTerminal) {
            'cancelled'
        }
        else {
            'running'
        }
        $baseComputeStage = @($sample.stages | Where-Object stage_id -eq 'base_compute')[-1]
        $baseComputeStage.state = if ($isTaskTerminal -and -not ($SecondTaskNotTerminal -and -not $isFirstTask)) { 3 } else { 2 }

        # 两个任务都从 1 开始累计；可选让 A 的双盘请求留出明确生产间隙，验证 B 的缺证据不能抑制 A 硬失败。
        $taskSequence = $localIndex + 1
        $diskRows = @($sample.pipeline_metrics.disk_reads)
        $taskHasRequest = -not $isTaskTerminal -or ($SecondTaskNotTerminal -and -not $isFirstTask)
        $firstTaskDiskSplitIndex = [int][Math]::Floor($splitIndex / 2)
        $firstTaskDisk11StartIndex = $firstTaskDiskSplitIndex + 2
        $disk10HasRequest = $taskHasRequest -and (-not ($isFirstTask -and $FirstTaskDelayedRequests) -or $localIndex -lt $firstTaskDiskSplitIndex)
        $disk11HasRequest = $taskHasRequest -and (-not ($isFirstTask -and $FirstTaskDelayedRequests) -or $localIndex -ge $firstTaskDisk11StartIndex)
        $disk10Granted = if ($isFirstTask -and $FirstTaskDelayedRequests) {
            [Math]::Min($taskSequence, $firstTaskDiskSplitIndex)
        }
        else { $taskSequence }
        $disk11Granted = if ($isFirstTask -and $FirstTaskDelayedRequests) {
            if ($localIndex -lt $firstTaskDisk11StartIndex) { 0 } else { $localIndex - $firstTaskDisk11StartIndex + 1 }
        }
        else { $taskSequence }
        $diskRows[0].hash_waiting = if ($disk10HasRequest) { 1 } else { 0 }
        $diskRows[0].hash_active = if ($disk10HasRequest) { 1 } else { 0 }
        $diskRows[0].hash_granted_total = $disk10Granted
        $diskRows[0].hash_released_total = if ($disk10HasRequest) { $disk10Granted - 1 } else { $disk10Granted }
        $diskRows[1].media_waiting = if ($disk11HasRequest) { 1 } else { 0 }
        $diskRows[1].media_active = if ($disk11HasRequest) { 1 } else { 0 }
        $diskRows[1].media_granted_total = $disk11Granted
        $diskRows[1].media_released_total = if ($disk11HasRequest) { $disk11Granted - 1 } else { $disk11Granted }
    }

    $runtimeResult = @($records | Where-Object record_type -eq 'runtime_result')[-1]
    $runtimeResult.scans_started = 2
    $runtimeResult.scan_tasks = @(
        [pscustomobject]@{ persistent_task_id = 'task-completed'; runtime_task_id = 'runtime-task-a'; terminal_state = 'completed' },
        [pscustomobject]@{ persistent_task_id = 'task-deadline'; runtime_task_id = 'runtime-task-b'; terminal_state = if ($SecondTaskNotTerminal) { $null } else { 'cancelled' } }
    )
    $lines = @($records | ForEach-Object { $_ | ConvertTo-Json -Depth 12 -Compress })
    [IO.File]::WriteAllLines($path, $lines, [Text.UTF8Encoding]::new($false))
}

function Set-Task8RuntimeTimingIssue {
    <# 只破坏一个 BaseCompute running 样本的时间证据，验证逐任务可见性不会使用不可靠区间。 #>
    param(
        [Parameter(Mandatory)] [string] $Root,
        [Parameter(Mandatory)] [ValidateSet('MissingUtc', 'IntervalMismatch')] [string] $Issue
    )

    $path = Join-Path $Root 'runtime.ndjson'
    $records = @([IO.File]::ReadAllLines($path) | Where-Object { $_ } | ForEach-Object { $_ | ConvertFrom-Json })
    $runtimeSamples = @($records | Where-Object record_type -eq 'runtime_sample')
    $targetSample = $runtimeSamples[100]
    if ($Issue -eq 'MissingUtc') {
        [void]$targetSample.PSObject.Properties.Remove('utc_unix_ms')
    }
    else {
        $targetSample.sample_interval_ms = 1000
    }
    $lines = @($records | ForEach-Object { $_ | ConvertTo-Json -Depth 12 -Compress })
    [IO.File]::WriteAllLines($path, $lines, [Text.UTF8Encoding]::new($false))
}

function Set-Task8DiskTotalRollback {
    <# 在单一任务中制造一次累计授权回退，其他样本保持容量和 released<=granted 合法。 #>
    param([Parameter(Mandatory)] [string] $Root)

    $path = Join-Path $Root 'runtime.ndjson'
    $records = @([IO.File]::ReadAllLines($path) | Where-Object { $_ } | ForEach-Object { $_ | ConvertFrom-Json })
    $runtimeSamples = @($records | Where-Object record_type -eq 'runtime_sample')
    $rollbackSample = $runtimeSamples[100]
    $disk10 = @($rollbackSample.pipeline_metrics.disk_reads | Where-Object physical_disk_id -eq 'PhysicalDisk10')[0]
    $disk10.hash_granted_total = 1
    $disk10.hash_released_total = 0
    $lines = @($records | ForEach-Object { $_ | ConvertTo-Json -Depth 12 -Compress })
    [IO.File]::WriteAllLines($path, $lines, [Text.UTF8Encoding]::new($false))
}

function Assert-Task8ReviewFixtures {
    <# 定向覆盖 Task 8 审查缺口；快速入口和完整套件共用同一组行为断言。 #>
    $invalidBindingRoot = Join-Path $fixtureRoot 'task8-review-invalid-binding'
    Write-Fixture -Root $invalidBindingRoot -DurationSeconds 760 -RuntimeSampleSeconds 2 `
        -SingleRun -SingleRunTerminalState completed -ManifestV2 -DelayedDiskRequests
    $physicalMapPath = Join-Path $invalidBindingRoot 'physical-disk-map.json'
    $physicalMap = [IO.File]::ReadAllText($physicalMapPath) | ConvertFrom-Json
    $physicalMap.entries = @($physicalMap.entries[1], $physicalMap.entries[0])
    [IO.File]::WriteAllText($physicalMapPath, ($physicalMap | ConvertTo-Json -Depth 10), [Text.UTF8Encoding]::new($false))
    $invalidHarnessPath = Join-Path $invalidBindingRoot 'harness-result.json'
    $invalidHarness = [IO.File]::ReadAllText($invalidHarnessPath) | ConvertFrom-Json
    $invalidHarness.physical_disk_map_sha256 = (Get-FileHash -LiteralPath $physicalMapPath -Algorithm SHA256).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText($invalidHarnessPath, ($invalidHarness | ConvertTo-Json -Depth 12), [Text.UTF8Encoding]::new($false))
    $invalidBindingReport = Join-Path $invalidBindingRoot 'report.md'
    & $reporter -EvidenceRoot $invalidBindingRoot -OutputPath $invalidBindingReport | Out-Null
    $invalidBindingText = [IO.File]::ReadAllText($invalidBindingReport)
    if ($invalidBindingText -notmatch '结论：INCONCLUSIVE' -or
        $invalidBindingText -notmatch 'RUST_V2_RUNTIME_PHYSICAL_DISK_MAP_ROOT_ORDER_INVALID' -or
        $invalidBindingText -match 'DISK_(READ|REQUEST)_') {
        throw "RED: 完整媒体/映射绑定闭包无效时必须 INCONCLUSIVE，且不得产生逐盘硬失败：$invalidBindingText"
    }

    $adjacentRoot = Join-Path $fixtureRoot 'task8-review-adjacent-intervals'
    Write-Fixture -Root $adjacentRoot -DurationSeconds 760 -RuntimeSampleSeconds 2 `
        -SingleRun -SingleRunTerminalState completed -ManifestV2 -SequentialDiskRequests
    $adjacentReport = Join-Path $adjacentRoot 'report.md'
    & $reporter -EvidenceRoot $adjacentRoot -OutputPath $adjacentReport | Out-Null
    $adjacentText = [IO.File]::ReadAllText($adjacentReport)
    if ($adjacentText -notmatch '结论：PASS' -or $adjacentText -match 'DISK_REQUEST_VISIBILITY_NOT_MET') {
        throw "RED: 相邻生产采样区间必须证明第二盘在第一盘耗尽前进入：$adjacentText"
    }

    $tailOnlyRoot = Join-Path $fixtureRoot 'task8-review-tail-only-overlap'
    Write-Fixture -Root $tailOnlyRoot -DurationSeconds 760 -RuntimeSampleSeconds 2 `
        -SingleRun -SingleRunTerminalState completed -ManifestV2 -TailOnlyDiskOverlap -FinalizationTail
    $tailOnlyReport = Join-Path $tailOnlyRoot 'report.md'
    & $reporter -EvidenceRoot $tailOnlyRoot -OutputPath $tailOnlyReport | Out-Null
    $tailOnlyText = [IO.File]::ReadAllText($tailOnlyReport)
    if ($tailOnlyText -notmatch '结论：FAIL' -or $tailOnlyText -notmatch 'DISK_REQUEST_VISIBILITY_NOT_MET') {
        throw "RED: 仅 finalization tail 重叠不能证明生产阶段逐盘请求可见性：$tailOnlyText"
    }

    $noTerminalRoot = Join-Path $fixtureRoot 'task8-review-no-terminal'
    Write-Fixture -Root $noTerminalRoot -DurationSeconds 1800 -RuntimeSampleSeconds 2 -ManifestV2 -NoTaskTerminal
    $noTerminalReport = Join-Path $noTerminalRoot 'report.md'
    & $reporter -EvidenceRoot $noTerminalRoot -OutputPath $noTerminalReport | Out-Null
    $noTerminalText = [IO.File]::ReadAllText($noTerminalReport)
    if ($noTerminalText -notmatch '结论：INCONCLUSIVE' -or $noTerminalText -match 'DISK_REQUEST_VISIBILITY_NOT_MET') {
        throw "RED: 被采样任务没有自己的 scan/runtime 终态时只能 INCONCLUSIVE：$noTerminalText"
    }

    $noIntervalRoot = Join-Path $fixtureRoot 'task8-review-no-time-interval'
    Write-Fixture -Root $noIntervalRoot -DurationSeconds 760 -RuntimeSampleSeconds 2 `
        -SingleRun -SingleRunTerminalState completed -ManifestV2 -DelayedDiskRequests -MissingUtcTimestamp
    $noIntervalReport = Join-Path $noIntervalRoot 'report.md'
    & $reporter -EvidenceRoot $noIntervalRoot -OutputPath $noIntervalReport | Out-Null
    $noIntervalText = [IO.File]::ReadAllText($noIntervalReport)
    if ($noIntervalText -notmatch '结论：INCONCLUSIVE' -or $noIntervalText -match 'DISK_REQUEST_VISIBILITY_NOT_MET') {
        throw "RED: 没有可用生产时间区间时只能 INCONCLUSIVE：$noIntervalText"
    }

    foreach ($timingCase in @(
            [pscustomobject]@{ Name = 'single-missing-utc'; Issue = 'MissingUtc'; Gap = $false; Reason = 'RUST_V2_RUNTIME_DISK_READ_TIME_INTERVAL_INVALID:runtime-fixture' },
            [pscustomobject]@{ Name = 'interval-mismatch'; Issue = 'IntervalMismatch'; Gap = $false; Reason = 'RUST_V2_RUNTIME_DISK_READ_TIME_INTERVAL_INVALID:runtime-fixture' },
            [pscustomobject]@{ Name = 'gap-exceeded'; Issue = ''; Gap = $true; Reason = 'RUST_V2_RUNTIME_DISK_READ_TIME_INTERVAL_INVALID:runtime-fixture' }
        )) {
        $timingRoot = Join-Path $fixtureRoot ("task8-review-" + $timingCase.Name)
        $timingArguments = @{
            Root = $timingRoot; DurationSeconds = 760; RuntimeSampleSeconds = 2
            SingleRun = $true; SingleRunTerminalState = 'completed'; ManifestV2 = $true
            DelayedDiskRequests = $true
        }
        if ($timingCase.Gap) { $timingArguments.RuntimeGapAtSeconds = 100 }
        Write-Fixture @timingArguments
        if (-not $timingCase.Gap) { Set-Task8RuntimeTimingIssue -Root $timingRoot -Issue $timingCase.Issue }
        $timingReport = Join-Path $timingRoot 'report.md'
        & $reporter -EvidenceRoot $timingRoot -OutputPath $timingReport | Out-Null
        $timingText = [IO.File]::ReadAllText($timingReport)
        if ($timingText -notmatch '结论：INCONCLUSIVE' -or
            $timingText -notmatch [regex]::Escape($timingCase.Reason) -or
            $timingText -match 'DISK_REQUEST_VISIBILITY_NOT_MET') {
            throw "RED: 单任务生产时间证据不可靠时必须 INCONCLUSIVE 且禁止可见性硬失败：$($timingCase.Name) / $timingText"
        }
    }

    $twoTaskRoot = Join-Path $fixtureRoot 'task8-review-two-task-reset'
    Write-Fixture -Root $twoTaskRoot -DurationSeconds 1800 -RuntimeSampleSeconds 2 -ManifestV2
    Convert-ToTwoTaskDiskFixture -Root $twoTaskRoot
    $twoTaskReport = Join-Path $twoTaskRoot 'report.md'
    & $reporter -EvidenceRoot $twoTaskRoot -OutputPath $twoTaskReport | Out-Null
    $twoTaskText = [IO.File]::ReadAllText($twoTaskReport)
    if ($twoTaskText -notmatch '结论：PASS' -or
        $twoTaskText -notmatch 'PhysicalDisk10.*\| 901 \| 901 \|' -or
        $twoTaskText -notmatch 'PhysicalDisk11.*\| 901 \| 901 \|') {
        throw "RED: 两任务累计值可分别重置，逐盘报告必须聚合各任务峰值之和：$twoTaskText"
    }

    $openSecondTaskRoot = Join-Path $fixtureRoot 'task8-review-a-terminal-not-b'
    Write-Fixture -Root $openSecondTaskRoot -DurationSeconds 1800 -RuntimeSampleSeconds 2 -ManifestV2
    Convert-ToTwoTaskDiskFixture -Root $openSecondTaskRoot -SecondTaskNotTerminal
    $openSecondTaskReport = Join-Path $openSecondTaskRoot 'report.md'
    & $reporter -EvidenceRoot $openSecondTaskRoot -OutputPath $openSecondTaskReport | Out-Null
    $openSecondTaskText = [IO.File]::ReadAllText($openSecondTaskReport)
    if ($openSecondTaskText -notmatch '结论：INCONCLUSIVE' -or
        $openSecondTaskText -notmatch 'RUST_V2_RUNTIME_TASK_TERMINAL_MISSING:runtime-task-b' -or
        $openSecondTaskText -match 'DISK_REQUEST_VISIBILITY_NOT_MET') {
        throw "RED: A 的终态不得替 B 关账：$openSecondTaskText"
    }

    $mixedReadinessRoot = Join-Path $fixtureRoot 'task8-review-mixed-task-readiness'
    Write-Fixture -Root $mixedReadinessRoot -DurationSeconds 1800 -RuntimeSampleSeconds 2 -ManifestV2
    Convert-ToTwoTaskDiskFixture -Root $mixedReadinessRoot -FirstTaskDelayedRequests -SecondTaskNotTerminal
    $mixedReadinessReport = Join-Path $mixedReadinessRoot 'report.md'
    & $reporter -EvidenceRoot $mixedReadinessRoot -OutputPath $mixedReadinessReport | Out-Null
    $mixedReadinessText = [IO.File]::ReadAllText($mixedReadinessReport)
    if ($mixedReadinessText -notmatch '结论：FAIL' -or
        $mixedReadinessText -notmatch 'DISK_REQUEST_VISIBILITY_NOT_MET' -or
        $mixedReadinessText -notmatch 'RUST_V2_RUNTIME_TASK_TERMINAL_MISSING:runtime-task-b') {
        throw "RED: B 缺终态只能抑制 B 裁决，不能抑制完整任务 A 的逐盘可见性硬失败：$mixedReadinessText"
    }

    $rollbackRoot = Join-Path $fixtureRoot 'task8-review-single-task-rollback'
    Write-Fixture -Root $rollbackRoot -DurationSeconds 760 -RuntimeSampleSeconds 2 `
        -SingleRun -SingleRunTerminalState completed -ManifestV2
    Set-Task8DiskTotalRollback -Root $rollbackRoot
    $rollbackReport = Join-Path $rollbackRoot 'report.md'
    & $reporter -EvidenceRoot $rollbackRoot -OutputPath $rollbackReport | Out-Null
    $rollbackText = [IO.File]::ReadAllText($rollbackReport)
    if ($rollbackText -notmatch '结论：FAIL' -or
        $rollbackText -notmatch 'DISK_READ_TOTAL_ROLLBACK:runtime-fixture:PhysicalDisk10:hash_granted_total') {
        throw "RED: 同一任务逐盘累计值回退必须硬失败：$rollbackText"
    }
}

function Assert-V2ReportEvidenceRejected {
    <# 为一个 v2 证据变体应用单一破坏并确认报告保持 INCONCLUSIVE，防止错绑证据升级为 PASS。 #>
    param(
        [Parameter(Mandatory)] [string] $Name,
        [Parameter(Mandatory)] [scriptblock] $Mutate,
        [Parameter(Mandatory)] [string] $Reason
    )

    $root = Join-Path $fixtureRoot ("task16-v2-" + $Name)
    Write-Fixture -Root $root -DurationSeconds 1800 -RuntimeSampleSeconds 2 -ManifestV2
    & $Mutate $root
    $report = Join-Path $root 'report.md'
    & $reporter -EvidenceRoot $root -OutputPath $report | Out-Null
    $text = [IO.File]::ReadAllText($report)
    if ($text -match '结论：PASS' -or $text -notmatch [regex]::Escape($Reason)) {
        throw "v2 错绑证据必须 INCONCLUSIVE：$Name / $Reason / $text"
    }
}

function Assert-SingleRunReportRejected {
    <# 为 single-run 变体应用单一破坏，确认运行终态和任务数量不能被报告器放宽。 #>
    param(
        [Parameter(Mandatory)] [string] $Name,
        [Parameter(Mandatory)] [scriptblock] $Mutate,
        [Parameter(Mandatory)] [string] $Reason
    )

    $root = Join-Path $fixtureRoot ("task16-single-run-" + $Name)
    Write-Fixture -Root $root -DurationSeconds 760 -RuntimeSampleSeconds 1 `
        -SingleRun -SingleRunTerminalState completed -ManifestV2
    & $Mutate $root
    $report = Join-Path $root 'report.md'
    & $reporter -EvidenceRoot $root -OutputPath $report | Out-Null
    $text = [IO.File]::ReadAllText($report)
    if ($text -match '结论：PASS' -or $text -notmatch [regex]::Escape($Reason)) {
        throw "single-run 非法证据必须拒绝：$Name / $Reason / $text"
    }
}

try {
    if (-not (Test-Path -LiteralPath $reporter -PathType Leaf)) {
        throw "RUST_V2_RUNTIME_ACCEPTANCE_REPORTER_MISSING path=$reporter"
    }
    if ($Task8ReviewOnly) {
        Assert-Task8ReviewFixtures
        Write-Output 'RUST_V2_RUNTIME_ACCEPTANCE_REPORT_TASK8_REVIEW_PASS'
        return
    }
    if ($ZeroPassOnly -or $ZeroInconclusiveOnly) {
        $zeroStatus = if ($ZeroPassOnly) { 'PASS' } else { 'INCONCLUSIVE' }
        $zeroPassRoot = Join-Path $fixtureRoot 'zero-pass-empty'
        Write-Fixture -Root $zeroPassRoot -DurationSeconds 1800
        $zeroSummaryPath = Join-Path $zeroPassRoot 'result-summary.jsonl'
        $zeroMetaPath = Join-Path $zeroPassRoot 'result-summary-meta.json'
        $zeroLeasePath = "$zeroSummaryPath.pair.lock"
        [IO.File]::WriteAllBytes($zeroSummaryPath, [byte[]]@())
        $zeroSha256 = (Get-FileHash -LiteralPath $zeroSummaryPath -Algorithm SHA256).Hash.ToLowerInvariant()
        $zeroHarness = [IO.File]::ReadAllText((Join-Path $zeroPassRoot 'harness-result.json')) | ConvertFrom-Json
        $zeroHarness.result_summary_sha256 = $zeroSha256
        $zeroHarness.result_summary_status = $zeroStatus
        $zeroHarness.result_summary_row_count = 0
        $zeroHarness.result_summary_missing_count = 0
        $zeroHarness.result_summary_inconclusive_count = 0
        $zeroHarness.run_status = 'PASS'
        [IO.File]::WriteAllText((Join-Path $zeroPassRoot 'harness-result.json'),
            ($zeroHarness | ConvertTo-Json -Depth 12), [Text.UTF8Encoding]::new($false))
        $zeroMetadata = [IO.File]::ReadAllText($zeroMetaPath) | ConvertFrom-Json
        $zeroMetadata.canonical_sha256 = $zeroSha256
        $zeroMetadata.status = $zeroStatus
        $zeroMetadata.row_count = 0
        [IO.File]::WriteAllText($zeroMetaPath, ($zeroMetadata | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
        $zeroLease = [IO.File]::ReadAllText($zeroLeasePath) | ConvertFrom-Json
        $zeroLease.expected_canonical_sha256 = $zeroSha256
        $zeroLease.expected_status = $zeroStatus
        $zeroLease.expected_row_count = 0
        [IO.File]::WriteAllText($zeroLeasePath, ($zeroLease | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
        $zeroReport = Join-Path $zeroPassRoot 'report.md'
        & $reporter -EvidenceRoot $zeroPassRoot -OutputPath $zeroReport | Out-Null
        $zeroText = [IO.File]::ReadAllText($zeroReport)
        if ($zeroText -match '结论：PASS' -or $zeroText -notmatch 'RUST_V2_RUNTIME_RESULT_SUMMARY_EMPTY_NON_MISSING') {
            throw "$zeroStatus + row_count=0 + 空 canonical 必须拒绝三件套绑定并保留 INCONCLUSIVE evidence"
        }
        $zeroMarker = if ($ZeroPassOnly) {
            'RUST_V2_RUNTIME_ACCEPTANCE_REPORT_ZERO_PASS_BINDING_PASS'
        }
        else {
            'RUST_V2_RUNTIME_ACCEPTANCE_REPORT_ZERO_INCONCLUSIVE_BINDING_PASS'
        }
        Write-Output $zeroMarker
        return
    }
    if ($StateEvidenceOnly) {
        # 每个状态单独使用 evidence 根，验证报告器不会把缺失证据折叠成 PASS。
        $stateCases = @(
            [pscustomobject]@{ Name = 'harness-run-fail'; Target = 'harness'; Property = 'run_status'; Value = 'FAIL'; Remove = $false; Verdict = 'FAIL'; Reason = 'RUST_V2_RUNTIME_HARNESS_RUN_STATUS_FAIL' }
            [pscustomobject]@{ Name = 'harness-run-inconclusive'; Target = 'harness'; Property = 'run_status'; Value = 'INCONCLUSIVE'; Remove = $false; Verdict = 'INCONCLUSIVE'; Reason = 'RUST_V2_RUNTIME_HARNESS_RUN_STATUS_INCONCLUSIVE' }
            [pscustomobject]@{ Name = 'runtime-correctness-fail'; Target = 'runtime'; Property = 'correctness'; Value = 'FAIL'; Remove = $false; Verdict = 'FAIL'; Reason = 'RUST_V2_RUNTIME_CORRECTNESS_FAIL' }
            [pscustomobject]@{ Name = 'runtime-correctness-missing'; Target = 'runtime'; Property = 'correctness'; Value = $null; Remove = $true; Verdict = 'INCONCLUSIVE'; Reason = 'RUST_V2_RUNTIME_CORRECTNESS_MISSING' }
            [pscustomobject]@{ Name = 'runtime-correctness-inconclusive'; Target = 'runtime'; Property = 'correctness'; Value = 'INCONCLUSIVE'; Remove = $false; Verdict = 'INCONCLUSIVE'; Reason = 'RUST_V2_RUNTIME_CORRECTNESS_INCONCLUSIVE' }
            [pscustomobject]@{ Name = 'runtime-correctness-invalid'; Target = 'runtime'; Property = 'correctness'; Value = 'BROKEN'; Remove = $false; Verdict = 'INCONCLUSIVE'; Reason = 'RUST_V2_RUNTIME_CORRECTNESS_INVALID' }
            [pscustomobject]@{ Name = 'runtime-failed-scans-missing'; Target = 'runtime'; Property = 'failed_scans'; Value = $null; Remove = $true; Verdict = 'INCONCLUSIVE'; Reason = 'RUST_V2_RUNTIME_RESULT_EVIDENCE_INVALID:failed_scans:missing' }
            [pscustomobject]@{ Name = 'runtime-scan-tasks-missing'; Target = 'runtime'; Property = 'scan_tasks'; Value = $null; Remove = $true; Verdict = 'INCONCLUSIVE'; Reason = 'RUST_V2_RUNTIME_RESULT_EVIDENCE_INVALID:scan_tasks:missing' }
            [pscustomobject]@{ Name = 'runtime-failed-scans-type'; Target = 'runtime'; Property = 'failed_scans'; Value = 'not-an-integer'; Remove = $false; Verdict = 'INCONCLUSIVE'; Reason = 'RUST_V2_RUNTIME_RESULT_EVIDENCE_INVALID:failed_scans:type' }
            [pscustomobject]@{ Name = 'runtime-scan-tasks-type'; Target = 'runtime'; Property = 'scan_tasks'; Value = 'not-an-array'; Remove = $false; Verdict = 'INCONCLUSIVE'; Reason = 'RUST_V2_RUNTIME_RESULT_EVIDENCE_INVALID:scan_tasks:type' }
        )
        foreach ($stateCase in $stateCases) {
            $stateRoot = Join-Path $fixtureRoot $stateCase.Name
            Write-Fixture -Root $stateRoot -DurationSeconds 1800 -RuntimeSampleSeconds 1 -FinalizationTail -IrregularIntervals
            if ($stateCase.Target -eq 'harness') {
                Set-JsonPropertyForTest -Path (Join-Path $stateRoot 'harness-result.json') `
                    -PropertyName $stateCase.Property -Value $stateCase.Value -Remove:$stateCase.Remove
            }
            else {
                Set-RuntimeResultPropertyForTest -Root $stateRoot `
                    -PropertyName $stateCase.Property -Value $stateCase.Value -Remove:$stateCase.Remove
            }
            $stateReport = Join-Path $stateRoot 'report.md'
            & $reporter -EvidenceRoot $stateRoot -OutputPath $stateReport | Out-Null
            $stateText = [IO.File]::ReadAllText($stateReport)
            if ($stateText -notmatch [regex]::Escape("结论：$($stateCase.Verdict)")) {
                throw "状态证据门禁错误：$($stateCase.Name) 未得到 $($stateCase.Verdict)"
            }
            if ($stateText -notmatch [regex]::Escape($stateCase.Reason)) {
                throw "状态证据门禁缺少稳定原因：$($stateCase.Name) / $($stateCase.Reason)"
            }
        }
        Write-Output 'RUST_V2_RUNTIME_ACCEPTANCE_REPORT_STATE_EVIDENCE_PASS'
        return
    }

    # 收尾超时但 exporter 已在 Node/Worker 退出后完成时，摘要可为 PASS，单轮仍须保留 harness INCONCLUSIVE。
    $shutdownWarningRoot = Join-Path $fixtureRoot 'shutdown-warning'
    Write-Fixture -Root $shutdownWarningRoot -DurationSeconds 1800 -RuntimeSampleSeconds 1 -FinalizationTail -IrregularIntervals
    $shutdownWarningHarnessPath = Join-Path $shutdownWarningRoot 'harness-result.json'
    # 通过统一 helper 追加缺失字段，保持 fixture 与真实 schema2 的可选字段演进兼容。
    Set-JsonPropertyForTest -Path $shutdownWarningHarnessPath -PropertyName 'run_status' -Value 'INCONCLUSIVE'
    Set-JsonPropertyForTest -Path $shutdownWarningHarnessPath -PropertyName 'run_diagnostic' -Value 'RUST_V2_ACCEPTANCE_NODE_EXIT_TIMEOUT'
    Set-JsonPropertyForTest -Path $shutdownWarningHarnessPath -PropertyName 'exporter_exit_code' -Value 0
    $shutdownWarningReport = Join-Path $shutdownWarningRoot 'report.md'
    & $reporter -EvidenceRoot $shutdownWarningRoot -OutputPath $shutdownWarningReport | Out-Null
    $shutdownWarningText = [IO.File]::ReadAllText($shutdownWarningReport)
    if ($shutdownWarningText -notmatch '结论：INCONCLUSIVE' -or
        $shutdownWarningText -notmatch 'RUST_V2_RUNTIME_HARNESS_RUN_STATUS_INCONCLUSIVE') {
        throw 'Node shutdown timeout 即使 exporter 已返回也必须保留单轮 INCONCLUSIVE'
    }

    $passRoot = Join-Path $fixtureRoot 'pass'
    Write-Fixture -Root $passRoot -DurationSeconds 1800 -RuntimeSampleSeconds 1 -FinalizationTail -IrregularIntervals
    $passReport = Join-Path $passRoot 'report.md'
    & $reporter -EvidenceRoot $passRoot -OutputPath $passReport | Out-Null
    $text = Get-Content -LiteralPath $passReport -Raw
    foreach ($required in @(
        '结论：PASS', '实际计算窗口', '机器 ID', '各阶段耗时与吞吐',
        'Worker 并行', 'Node/Worker CPU 与内存', '物理磁盘读取', '最近失败',
        '实际执行配置', '流水线运行指标', 'Worker 子阶段', '队列容量门禁',
        '峰值非空闲 Worker：2', '平均非空闲 Worker',
        '文件故障分类', '联系表复用', '磁盘满清理', '真实媒体未修改证明',
        '本次未触发，不能从本次实测证明清理路径')) {
        if ($text -notmatch [regex]::Escape($required)) {
            throw "PASS报告缺少字段：$required"
        }
    }
    if ($text -match 'finalization tail：0\.000') {
        throw 'finalization tail 必须单列且不计入生产窗口'
    }
    $runtimeFixture = @([IO.File]::ReadAllLines((Join-Path $passRoot 'runtime.ndjson')) |
        Where-Object { $_ } | ForEach-Object { $_ | ConvertFrom-Json })
    $systemFixture = @([IO.File]::ReadAllLines((Join-Path $passRoot 'system.ndjson')) |
        Where-Object { $_ } | ForEach-Object { $_ | ConvertFrom-Json })
    $runtimeSampleFixture = @($runtimeFixture | Where-Object record_type -eq 'runtime_sample')
    $systemSampleFixture = @($systemFixture | Where-Object record_type -eq 'system_sample')
    $irregularIntervals = @($runtimeSampleFixture | Where-Object { [double]$_.sample_interval_ms -notin @(0, 1000) })
    if ($runtimeSampleFixture.Count -le $systemSampleFixture.Count -or $irregularIntervals.Count -eq 0) {
        throw 'fixture 必须使用 runtime 1 秒、system 2 秒并保留实际不规则 sample_interval_ms'
    }
    $summaryBytes = [IO.File]::ReadAllBytes((Join-Path $passRoot 'result-summary.jsonl'))
    if ([Array]::IndexOf($summaryBytes, [byte]13) -ge 0 -or $summaryBytes.Length -eq 0 -or
        $summaryBytes[$summaryBytes.Length - 1] -ne [byte]10) {
        throw 'Report fixture canonical JSONL 必须是 UTF-8 无 BOM、单字节 LF、无 CR'
    }

    # Task 8 审查回归与完整套件共用，防止快速定向门禁通过但完整入口漏跑。
    Assert-Task8ReviewFixtures

    # Task 8 RED：逐盘协议值必须直接裁决双盘可见性和守恒，不能从 Worker 路径猜许可状态。
    $diskOverlapRoot = Join-Path $fixtureRoot 'task8-disk-overlap'
    Write-Fixture -Root $diskOverlapRoot -DurationSeconds 760 -RuntimeSampleSeconds 2 `
        -SingleRun -SingleRunTerminalState completed -ManifestV2 -ProcessSampleSkip
    $diskOverlapReport = Join-Path $diskOverlapRoot 'report.md'
    & $reporter -EvidenceRoot $diskOverlapRoot -OutputPath $diskOverlapReport | Out-Null
    $diskOverlapText = [IO.File]::ReadAllText($diskOverlapReport)
    if ($diskOverlapText -notmatch '结论：PASS' -or
        $diskOverlapText -notmatch '逐物理盘读取许可' -or
        $diskOverlapText -notmatch 'PhysicalDisk10.*\| 4 \|' -or
        $diskOverlapText -notmatch 'PhysicalDisk11.*\| 4 \|' -or
        $diskOverlapText -notmatch 'process_sample_skips：1') {
        throw "RED: 双盘窗口重叠且守恒时必须 PASS，并报告逐盘表和 Worker skip 次数：$diskOverlapText"
    }

    $diskSequentialRoot = Join-Path $fixtureRoot 'task8-disk-delayed'
    Write-Fixture -Root $diskSequentialRoot -DurationSeconds 760 -RuntimeSampleSeconds 2 `
        -SingleRun -SingleRunTerminalState completed -ManifestV2 -DelayedDiskRequests
    $diskSequentialReport = Join-Path $diskSequentialRoot 'report.md'
    & $reporter -EvidenceRoot $diskSequentialRoot -OutputPath $diskSequentialReport | Out-Null
    $diskSequentialText = [IO.File]::ReadAllText($diskSequentialReport)
    if ($diskSequentialText -notmatch '结论：FAIL' -or
        $diskSequentialText -notmatch 'DISK_REQUEST_VISIBILITY_NOT_MET') {
        throw "RED: 第二盘在第一盘耗尽前从未 waiting/active 必须硬失败：$diskSequentialText"
    }

    $diskMissingRoot = Join-Path $fixtureRoot 'task8-disk-missing'
    Write-Fixture -Root $diskMissingRoot -DurationSeconds 760 -RuntimeSampleSeconds 2 `
        -SingleRun -SingleRunTerminalState completed -ManifestV2 -MissingDiskReads
    $diskMissingReport = Join-Path $diskMissingRoot 'report.md'
    & $reporter -EvidenceRoot $diskMissingRoot -OutputPath $diskMissingReport | Out-Null
    $diskMissingText = [IO.File]::ReadAllText($diskMissingReport)
    if ($diskMissingText -notmatch '结论：INCONCLUSIVE' -or
        $diskMissingText -notmatch 'RUST_V2_RUNTIME_DISK_READ_METRICS_MISSING' -or
        $diskMissingText -match 'DISK_REQUEST_VISIBILITY_NOT_MET') {
        throw "RED: disk_reads 缺失只能 INCONCLUSIVE，且不得用 Worker 路径推导请求可见性：$diskMissingText"
    }

    $missingWithFailureRoot = Join-Path $fixtureRoot 'task8-missing-with-hard-failure'
    Write-Fixture -Root $missingWithFailureRoot -DurationSeconds 760 -RuntimeSampleSeconds 2 `
        -SingleRun -SingleRunTerminalState failed -ManifestV2 -MissingDiskReads
    $missingWithFailureReport = Join-Path $missingWithFailureRoot 'report.md'
    & $reporter -EvidenceRoot $missingWithFailureRoot -OutputPath $missingWithFailureReport | Out-Null
    $missingWithFailureText = [IO.File]::ReadAllText($missingWithFailureReport)
    if ($missingWithFailureText -notmatch '结论：FAIL' -or
        $missingWithFailureText -notmatch 'RUST_V2_RUNTIME_CORRECTNESS_FAIL' -or
        $missingWithFailureText -notmatch 'RUST_V2_RUNTIME_DISK_READ_METRICS_MISSING') {
        throw "新增缺字段不得遮蔽既有 hard failure：$missingWithFailureText"
    }

    foreach ($diskFailureCase in @(
            [pscustomobject]@{ Name = 'capacity'; Switch = 'DiskCapacityExceeded'; Reason = 'DISK_READ_CAPACITY_EXCEEDED' }
            [pscustomobject]@{ Name = 'released'; Switch = 'DiskReleasedExceeded'; Reason = 'DISK_READ_RELEASED_EXCEEDS_GRANTED' }
            [pscustomobject]@{ Name = 'terminal'; Switch = 'DiskTerminalNonZero'; Reason = 'DISK_READ_TERMINAL_NOT_ZERO' }
        )) {
        $diskFailureRoot = Join-Path $fixtureRoot ("task8-disk-" + $diskFailureCase.Name)
        $fixtureArguments = @{
            Root = $diskFailureRoot; DurationSeconds = 760; RuntimeSampleSeconds = 2
            SingleRun = $true; SingleRunTerminalState = 'completed'; ManifestV2 = $true
        }
        $fixtureArguments[$diskFailureCase.Switch] = $true
        Write-Fixture @fixtureArguments
        $diskFailureReport = Join-Path $diskFailureRoot 'report.md'
        & $reporter -EvidenceRoot $diskFailureRoot -OutputPath $diskFailureReport | Out-Null
        $diskFailureText = [IO.File]::ReadAllText($diskFailureReport)
        if ($diskFailureText -notmatch '结论：FAIL' -or
            $diskFailureText -notmatch [regex]::Escape($diskFailureCase.Reason)) {
            throw "RED: 逐盘硬失败门禁未触发：$($diskFailureCase.Name) / $diskFailureText"
        }
    }

    $noTerminalRoot = Join-Path $fixtureRoot 'task8-no-terminal'
    Write-Fixture -Root $noTerminalRoot -DurationSeconds 1800 -RuntimeSampleSeconds 2 `
        -ManifestV2 -NoTaskTerminal
    $noTerminalReport = Join-Path $noTerminalRoot 'report.md'
    & $reporter -EvidenceRoot $noTerminalRoot -OutputPath $noTerminalReport | Out-Null
    $noTerminalText = [IO.File]::ReadAllText($noTerminalReport)
    if ($noTerminalText -notmatch '结论：INCONCLUSIVE' -or
        $noTerminalText -notmatch 'RUST_V2_RUNTIME_TASK_TERMINAL_MISSING') {
        throw "RED: 无任务终态只能 INCONCLUSIVE：$noTerminalText"
    }

    $skipInterruptedRoot = Join-Path $fixtureRoot 'task8-skip-system-interrupted'
    Write-Fixture -Root $skipInterruptedRoot -DurationSeconds 760 -RuntimeSampleSeconds 2 `
        -SingleRun -SingleRunTerminalState completed -ManifestV2 -ProcessSampleSkip -SystemGapAtSeconds 100
    $skipInterruptedReport = Join-Path $skipInterruptedRoot 'report.md'
    & $reporter -EvidenceRoot $skipInterruptedRoot -OutputPath $skipInterruptedReport | Out-Null
    $skipInterruptedText = [IO.File]::ReadAllText($skipInterruptedReport)
    if ($skipInterruptedText -notmatch '结论：INCONCLUSIVE' -or
        $skipInterruptedText -notmatch 'system 相邻采样最大间隔' -or
        $skipInterruptedText -notmatch 'process_sample_skips：1') {
        throw "RED: Worker skip 可继续裁决，但 system 采样中断必须 INCONCLUSIVE：$skipInterruptedText"
    }

    # Task 17 RED：runtime_result 的媒体根必须与静态 v2 清单闭合，且两个配置根都要被 Worker display_path 观察到。
    $runtimeRootsMismatchRoot = Join-Path $fixtureRoot 'task17-runtime-roots-mismatch'
    Write-Fixture -Root $runtimeRootsMismatchRoot -DurationSeconds 1800 -RuntimeSampleSeconds 1 -ManifestV2 -RuntimeRootsMismatch
    $runtimeRootsMismatchReport = Join-Path $runtimeRootsMismatchRoot 'report.md'
    & $reporter -EvidenceRoot $runtimeRootsMismatchRoot -OutputPath $runtimeRootsMismatchReport | Out-Null
    $runtimeRootsMismatchText = Get-Content -LiteralPath $runtimeRootsMismatchReport -Raw
    if ($runtimeRootsMismatchText -match '结论：PASS' -or
        $runtimeRootsMismatchText -notmatch 'RUST_V2_RUNTIME_MEDIA_ROOT_RUNTIME_RESULT_BINDING_INVALID') {
        throw "RED: runtime_result.media_roots 错绑必须 INCONCLUSIVE：$runtimeRootsMismatchText"
    }

    $onlyFirstRootObservedRoot = Join-Path $fixtureRoot 'task17-only-first-root-observed'
    Write-Fixture -Root $onlyFirstRootObservedRoot -DurationSeconds 1800 -RuntimeSampleSeconds 1 -ManifestV2 -OnlyFirstRootObserved
    $onlyFirstRootObservedReport = Join-Path $onlyFirstRootObservedRoot 'report.md'
    & $reporter -EvidenceRoot $onlyFirstRootObservedRoot -OutputPath $onlyFirstRootObservedReport | Out-Null
    $onlyFirstRootObservedText = Get-Content -LiteralPath $onlyFirstRootObservedReport -Raw
    if ($onlyFirstRootObservedText -match '结论：PASS' -or
        $onlyFirstRootObservedText -notmatch 'RUST_V2_RUNTIME_MEDIA_ROOT_WORKER_OBSERVATION_MISSING') {
        throw "RED: 只观察一个媒体根必须 INCONCLUSIVE：$onlyFirstRootObservedText"
    }

    $bothRootsObservedRoot = Join-Path $fixtureRoot 'task17-both-roots-observed'
    Write-Fixture -Root $bothRootsObservedRoot -DurationSeconds 1800 -RuntimeSampleSeconds 1 -ManifestV2
    $bothRootsObservedReport = Join-Path $bothRootsObservedRoot 'report.md'
    & $reporter -EvidenceRoot $bothRootsObservedRoot -OutputPath $bothRootsObservedReport | Out-Null
    $bothRootsObservedText = Get-Content -LiteralPath $bothRootsObservedReport -Raw
    if ($bothRootsObservedText -notmatch '结论：PASS' -or
        $bothRootsObservedText -match 'RUST_V2_RUNTIME_MEDIA_ROOT_WORKER_OBSERVATION_MISSING') {
        throw "GREEN 目标：两个媒体根均被观察时应保持 PASS：$bothRootsObservedText"
    }

    # Task 16 RED：single-run 的实际终态在760秒完成时不应被1800秒最大截止门禁误判。
    $singleRunRoot = Join-Path $fixtureRoot 'single-run-v2'
    Write-Fixture -Root $singleRunRoot -DurationSeconds 760 -RuntimeSampleSeconds 1 `
        -SingleRun -SingleRunTerminalState completed -ManifestV2
    $singleRunReport = Join-Path $singleRunRoot 'report.md'
    & $reporter -EvidenceRoot $singleRunRoot -OutputPath $singleRunReport | Out-Null
    $singleRunText = Get-Content -LiteralPath $singleRunReport -Raw
    if ($singleRunText -notmatch '结论：PASS' -or
        $singleRunText -notmatch '配置最大截止时间：1800 秒' -or
        $singleRunText -notmatch '实际最后 runtime sample elapsed：760 秒' -or
        $singleRunText -notmatch 'physical.?disk.?map|物理盘映射' -or
        $singleRunText -notmatch 'H:\\pik\\00000000000.*DiskNumber.?10' -or
        $singleRunText -notmatch 'I:\\tmp.*DiskNumber.?11') {
        throw "single-run v2 completed 必须按首个终态裁决并列出双盘绑定：$singleRunText"
    }

    # failed/cancelled 仍按业务与证据语义裁决，不能因为 single-run 提前结束而升级 PASS。
    $singleFailedRoot = Join-Path $fixtureRoot 'single-run-failed-v2'
    Write-Fixture -Root $singleFailedRoot -DurationSeconds 760 -RuntimeSampleSeconds 1 `
        -SingleRun -SingleRunTerminalState failed -ManifestV2
    $singleFailedReport = Join-Path $singleFailedRoot 'report.md'
    & $reporter -EvidenceRoot $singleFailedRoot -OutputPath $singleFailedReport | Out-Null
    $singleFailedText = Get-Content -LiteralPath $singleFailedReport -Raw
    if ($singleFailedText -match '结论：PASS' -or
        $singleFailedText -notmatch '结论：FAIL' -or
        $singleFailedText -notmatch 'RUST_V2_RUNTIME_CORRECTNESS_FAIL') {
        throw "single-run failed 不得升级 PASS：$singleFailedText"
    }
    $singleCancelledRoot = Join-Path $fixtureRoot 'single-run-cancelled-v2'
    Write-Fixture -Root $singleCancelledRoot -DurationSeconds 760 -RuntimeSampleSeconds 1 `
        -SingleRun -SingleRunTerminalState cancelled -ManifestV2
    $singleCancelledReport = Join-Path $singleCancelledRoot 'report.md'
    & $reporter -EvidenceRoot $singleCancelledRoot -OutputPath $singleCancelledReport | Out-Null
    $singleCancelledText = Get-Content -LiteralPath $singleCancelledReport -Raw
    if ($singleCancelledText -match '结论：PASS' -or
        $singleCancelledText -notmatch '结论：(FAIL|INCONCLUSIVE)') {
        throw "single-run cancelled 不得升级 PASS：$singleCancelledText"
    }

    # Task 16 REVIEW RED：harness 根顺序与 v2 media manifest 根顺序不一致时不得升级 PASS。
    Assert-V2ReportEvidenceRejected -Name 'root-binding' `
        -Reason 'RUST_V2_RUNTIME_MEDIA_ROOT_BINDING_INVALID' -Mutate {
        param($root)
        $harnessPath = Join-Path $root 'harness-result.json'
        $harness = [IO.File]::ReadAllText($harnessPath) | ConvertFrom-Json
        $harness.media_roots = @('I:\tmp', 'H:\pik\00000000000')
        $harness | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $harnessPath -Encoding utf8
    }
    Assert-V2ReportEvidenceRejected -Name 'physical-map-path' `
        -Reason 'RUST_V2_RUNTIME_PHYSICAL_DISK_MAP_PATH_INVALID' -Mutate {
        param($root)
        $sourceMap = Join-Path $root 'physical-disk-map.json'
        $outsideMap = Join-Path (Split-Path -Parent $root) 'task16-physical-map-outside.json'
        Copy-Item -LiteralPath $sourceMap -Destination $outsideMap -Force
        $harnessPath = Join-Path $root 'harness-result.json'
        $harness = [IO.File]::ReadAllText($harnessPath) | ConvertFrom-Json
        $harness.physical_disk_map_path = $outsideMap
        $harness.physical_disk_map_sha256 = (Get-FileHash -LiteralPath $outsideMap -Algorithm SHA256).Hash.ToLowerInvariant()
        $harness | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $harnessPath -Encoding utf8
    }
    Assert-V2ReportEvidenceRejected -Name 'physical-map-sha-missing' `
        -Reason 'RUST_V2_RUNTIME_PHYSICAL_DISK_MAP_INVALID' -Mutate {
        param($root)
        $harnessPath = Join-Path $root 'harness-result.json'
        $harness = [IO.File]::ReadAllText($harnessPath) | ConvertFrom-Json
        $harness.physical_disk_map_sha256 = ''
        $harness | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $harnessPath -Encoding utf8
    }
    Assert-V2ReportEvidenceRejected -Name 'physical-map-root-order' `
        -Reason 'RUST_V2_RUNTIME_PHYSICAL_DISK_MAP_ROOT_ORDER_INVALID' -Mutate {
        param($root)
        $mapPath = Join-Path $root 'physical-disk-map.json'
        $map = [IO.File]::ReadAllText($mapPath) | ConvertFrom-Json
        $entries = @($map.entries)
        $roots = @($map.roots)
        $map.entries = @($entries[1], $entries[0])
        $map.roots = @($roots[1], $roots[0])
        $map | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $mapPath -Encoding utf8
        $harnessPath = Join-Path $root 'harness-result.json'
        $harness = [IO.File]::ReadAllText($harnessPath) | ConvertFrom-Json
        $harness.physical_disk_map_sha256 = (Get-FileHash -LiteralPath $mapPath -Algorithm SHA256).Hash.ToLowerInvariant()
        $harness | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $harnessPath -Encoding utf8
    }
    Assert-V2ReportEvidenceRejected -Name 'physical-map-distinct' `
        -Reason 'RUST_V2_RUNTIME_PHYSICAL_DISK_MAP_INVALID' -Mutate {
        param($root)
        $mapPath = Join-Path $root 'physical-disk-map.json'
        $map = [IO.File]::ReadAllText($mapPath) | ConvertFrom-Json
        $map.distinct_disk_numbers = @(10)
        $map | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $mapPath -Encoding utf8
        $harnessPath = Join-Path $root 'harness-result.json'
        $harness = [IO.File]::ReadAllText($harnessPath) | ConvertFrom-Json
        $harness.physical_disk_map_sha256 = (Get-FileHash -LiteralPath $mapPath -Algorithm SHA256).Hash.ToLowerInvariant()
        $harness | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $harnessPath -Encoding utf8
    }
    Assert-V2ReportEvidenceRejected -Name 'root-evidence-count' `
        -Reason 'RUST_V2_RUNTIME_MEDIA_ROOT_EVIDENCE_COUNT_INVALID' -Mutate {
        param($root)
        $harnessPath = Join-Path $root 'harness-result.json'
        $harness = [IO.File]::ReadAllText($harnessPath) | ConvertFrom-Json
        $harness.media_before_root_paths = @($harness.media_before_root_paths[0])
        $harness.media_before_root_sha256 = @($harness.media_before_root_sha256[0])
        $harness | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $harnessPath -Encoding utf8
    }
    Assert-V2ReportEvidenceRejected -Name 'root-manifest-binding' `
        -Reason 'RUST_V2_RUNTIME_MEDIA_ROOT_MANIFEST_ROOT_INVALID' -Mutate {
        param($root)
        $harnessPath = Join-Path $root 'harness-result.json'
        $harness = [IO.File]::ReadAllText($harnessPath) | ConvertFrom-Json
        $manifestPath = [string]$harness.media_before_root_paths[0]
        $manifest = [IO.File]::ReadAllText($manifestPath) | ConvertFrom-Json
        $manifest.Root = 'I:\tmp'
        $manifest | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $manifestPath -Encoding utf8
        $shas = @($harness.media_before_root_sha256)
        $shas[0] = (Get-FileHash -LiteralPath $manifestPath -Algorithm SHA256).Hash.ToLowerInvariant()
        $harness.media_before_root_sha256 = $shas
        $harness | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $harnessPath -Encoding utf8
    }
    Assert-V2ReportEvidenceRejected -Name 'root-manifest-changed' `
        -Reason 'RUST_V2_RUNTIME_MEDIA_ROOT_CHANGED' -Mutate {
        param($root)
        $harnessPath = Join-Path $root 'harness-result.json'
        $harness = [IO.File]::ReadAllText($harnessPath) | ConvertFrom-Json
        $manifestPath = [string]$harness.media_after_root_paths[0]
        $manifest = [IO.File]::ReadAllText($manifestPath) | ConvertFrom-Json
        $manifest.TotalBytes = 101
        $manifest | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $manifestPath -Encoding utf8
        $shas = @($harness.media_after_root_sha256)
        $shas[0] = (Get-FileHash -LiteralPath $manifestPath -Algorithm SHA256).Hash.ToLowerInvariant()
        $harness.media_after_root_sha256 = $shas
        $harness | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $harnessPath -Encoding utf8
    }
    Assert-SingleRunReportRejected -Name 'runtime-single-run-missing' `
        -Reason 'single_run runtime_result.single_run missing' -Mutate {
        param($root)
        Set-RuntimeResultPropertyForTest -Root $root -PropertyName 'single_run' -Remove
    }
    Assert-SingleRunReportRejected -Name 'runtime-single-run-false' `
        -Reason 'single_run runtime_result.single_run must be true' -Mutate {
        param($root)
        Set-RuntimeResultPropertyForTest -Root $root -PropertyName 'single_run' -Value $false
    }
    Assert-SingleRunReportRejected -Name 'scans-started-two' `
        -Reason 'single_run 必须恰好启动一次扫描' -Mutate {
        param($root)
        Set-RuntimeResultPropertyForTest -Root $root -PropertyName 'scans_started' -Value 2
    }
    Assert-SingleRunReportRejected -Name 'scan-task-extra' `
        -Reason 'single_run must contain exactly one scan task' -Mutate {
        param($root)
        $extraTask = [pscustomobject]@{
            persistent_task_id = 'task-extra'; runtime_task_id = 'runtime-extra'; terminal_state = 'completed'
        }
        $tasks = @([pscustomobject]@{
                persistent_task_id = 'task-completed'; runtime_task_id = 'runtime-fixture'; terminal_state = 'completed'
            }, $extraTask)
        Set-RuntimeResultPropertyForTest -Root $root -PropertyName 'scan_tasks' -Value $tasks
    }
    Assert-SingleRunReportRejected -Name 'terminal-invalid' `
        -Reason 'single_run terminal_state invalid' -Mutate {
        param($root)
        $invalidTask = [pscustomobject]@{
            persistent_task_id = 'task-completed'; runtime_task_id = 'runtime-fixture'; terminal_state = 'unknown'
        }
        Set-RuntimeResultPropertyForTest -Root $root -PropertyName 'scan_tasks' -Value @($invalidTask)
    }

    $passHarnessPath = Join-Path $passRoot 'harness-result.json'
    $validHarnessText = [IO.File]::ReadAllText($passHarnessPath)
    $invalidSchemaHarness = $validHarnessText | ConvertFrom-Json
    $invalidSchemaHarness.schema_version = 1
    [IO.File]::WriteAllText($passHarnessPath, ($invalidSchemaHarness | ConvertTo-Json -Depth 12), [Text.UTF8Encoding]::new($false))
    & $reporter -EvidenceRoot $passRoot -OutputPath $passReport | Out-Null
    $invalidSchemaText = Get-Content -LiteralPath $passReport -Raw
    if ($invalidSchemaText -notmatch '结论：INCONCLUSIVE' -or $invalidSchemaText -notmatch 'schema2') {
        throw 'schema_version 非2必须判INCONCLUSIVE'
    }
    [IO.File]::WriteAllText($passHarnessPath, $validHarnessText, [Text.UTF8Encoding]::new($false))

    $missingFieldHarness = $validHarnessText | ConvertFrom-Json
    [void]$missingFieldHarness.PSObject.Properties.Remove('run_status')
    [IO.File]::WriteAllText($passHarnessPath, ($missingFieldHarness | ConvertTo-Json -Depth 12), [Text.UTF8Encoding]::new($false))
    & $reporter -EvidenceRoot $passRoot -OutputPath $passReport | Out-Null
    $missingFieldText = Get-Content -LiteralPath $passReport -Raw
    if ($missingFieldText -notmatch '结论：INCONCLUSIVE' -or $missingFieldText -notmatch 'run_status') {
        throw 'schema2 必需字段缺失必须判INCONCLUSIVE'
    }
    [IO.File]::WriteAllText($passHarnessPath, $validHarnessText, [Text.UTF8Encoding]::new($false))

    $wrongTypeHarness = $validHarnessText | ConvertFrom-Json
    $wrongTypeHarness.run_index = 'not-an-integer'
    [IO.File]::WriteAllText($passHarnessPath, ($wrongTypeHarness | ConvertTo-Json -Depth 12), [Text.UTF8Encoding]::new($false))
    & $reporter -EvidenceRoot $passRoot -OutputPath $passReport | Out-Null
    $wrongTypeText = Get-Content -LiteralPath $passReport -Raw
    if ($wrongTypeText -notmatch '结论：INCONCLUSIVE' -or $wrongTypeText -notmatch 'run_index') {
        throw 'schema2 元数据类型错误必须判INCONCLUSIVE'
    }
    [IO.File]::WriteAllText($passHarnessPath, $validHarnessText, [Text.UTF8Encoding]::new($false))

    $passLeasePath = Join-Path $passRoot 'result-summary.jsonl.pair.lock'
    $passMetadataPath = Join-Path $passRoot 'result-summary-meta.json'
    $validLeaseText = [IO.File]::ReadAllText($passLeasePath)
    $validMetadataText = [IO.File]::ReadAllText($passMetadataPath)
    $tamperedLease = [IO.File]::ReadAllText($passLeasePath) | ConvertFrom-Json
    $tamperedLease.lease_token = 'tampered-lease-token'
    [IO.File]::WriteAllText($passLeasePath, ($tamperedLease | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
    & $reporter -EvidenceRoot $passRoot -OutputPath $passReport | Out-Null
    $tamperedLeaseText = Get-Content -LiteralPath $passReport -Raw
    if ($tamperedLeaseText -notmatch '结论：INCONCLUSIVE' -or $tamperedLeaseText -notmatch '三件套') {
        throw 'Report lease token 被篡改必须判INCONCLUSIVE'
    }
    [IO.File]::WriteAllText($passLeasePath, $validLeaseText, [Text.UTF8Encoding]::new($false))

    $tamperedMetadataTask = $validMetadataText | ConvertFrom-Json
    $tamperedMetadataTask.task_id = 'tampered-task-id'
    [IO.File]::WriteAllText($passMetadataPath, ($tamperedMetadataTask | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
    & $reporter -EvidenceRoot $passRoot -OutputPath $passReport | Out-Null
    $tamperedMetadataTaskText = Get-Content -LiteralPath $passReport -Raw
    if ($tamperedMetadataTaskText -notmatch '结论：INCONCLUSIVE' -or $tamperedMetadataTaskText -notmatch '三件套') {
        throw 'Report metadata task_id 被篡改必须判INCONCLUSIVE'
    }
    [IO.File]::WriteAllText($passMetadataPath, $validMetadataText, [Text.UTF8Encoding]::new($false))

    $tamperedMetadata = $validMetadataText | ConvertFrom-Json
    $tamperedMetadata.canonical_sha256 = ('f' * 64)
    [IO.File]::WriteAllText($passMetadataPath, ($tamperedMetadata | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
    & $reporter -EvidenceRoot $passRoot -OutputPath $passReport | Out-Null
    $tamperedMetadataText = Get-Content -LiteralPath $passReport -Raw
    if ($tamperedMetadataText -notmatch '结论：INCONCLUSIVE' -or $tamperedMetadataText -notmatch '三件套') {
        throw 'Report metadata 被篡改必须判INCONCLUSIVE'
    }
    [IO.File]::WriteAllText($passMetadataPath, $validMetadataText, [Text.UTF8Encoding]::new($false))
    $outsideReportRejected = $false
    try {
        & $reporter -EvidenceRoot $passRoot -OutputPath (Join-Path $fixtureRoot 'shared-report.md') | Out-Null
    }
    catch {
        $outsideReportRejected = $_.Exception.Message -like '*RUST_V2_RUNTIME_REPORT_OUTSIDE_EVIDENCE*'
    }
    if (-not $outsideReportRejected) {
        throw '报告输出路径越出本轮 evidence 必须拒绝'
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

    $capacityRoot = Join-Path $fixtureRoot 'capacity'
    Write-Fixture -Root $capacityRoot -DurationSeconds 1800 -CapacityExceeded
    $capacityReport = Join-Path $capacityRoot 'report.md'
    & $reporter -EvidenceRoot $capacityRoot -OutputPath $capacityReport | Out-Null
    if ((Get-Content -LiteralPath $capacityReport -Raw) -notmatch '队列峰值超过容量') {
        throw '队列峰值超过硬容量必须进入FAIL原因'
    }

    $emptyWorkersRoot = Join-Path $fixtureRoot 'empty-workers'
    Write-Fixture -Root $emptyWorkersRoot -DurationSeconds 1800 -AllWorkerSamplesEmpty
    $emptyWorkersReport = Join-Path $emptyWorkersRoot 'report.md'
    & $reporter -EvidenceRoot $emptyWorkersRoot -OutputPath $emptyWorkersReport | Out-Null
    $emptyWorkersText = Get-Content -LiteralPath $emptyWorkersReport -Raw
    if ($emptyWorkersText -notmatch '结论：FAIL' -or $emptyWorkersText -notmatch '峰值非空闲Worker仅 0') {
        $emptyWorkerLines = @($emptyWorkersText -split "`r?`n" | Where-Object { $_ -match 'Worker' }) -join ' | '
        throw "全空Worker样本必须生成可审计FAIL，不能抛出字段访问错误：fail=$($emptyWorkersText -match '结论：FAIL') workerGate=$($emptyWorkersText -match '峰值非空闲Worker仅 0') lines=$emptyWorkerLines"
    }

    $crashRoot = Join-Path $fixtureRoot 'crash-observed'
    Write-Fixture -Root $crashRoot -DurationSeconds 1800 -WorkerCrashObserved
    $crashReport = Join-Path $crashRoot 'report.md'
    & $reporter -EvidenceRoot $crashRoot -OutputPath $crashReport | Out-Null
    $crashText = Get-Content -LiteralPath $crashReport -Raw
    if ($crashText -notmatch '结论：PASS' -or $crashText -notmatch 'Worker崩溃观察样本') {
        throw '本轮CPU/I/O验收应记录Worker崩溃，但不把它单独升级为架构FAIL'
    }

    $baselineBRoot = Join-Path $fixtureRoot 'baseline-b'
    Write-Fixture -Root $baselineBRoot -DurationSeconds 1800 -Variant B
    $baselineBReport = Join-Path $baselineBRoot 'report.md'
    & $reporter -EvidenceRoot $baselineBRoot -OutputPath $baselineBReport | Out-Null
    $baselineBText = Get-Content -LiteralPath $baselineBReport -Raw
    if ($baselineBText -notmatch '结论：PASS' -or $baselineBText -notmatch 'B credit 守恒：PASS' -or
        $baselineBText -notmatch 'refill token' -or $baselineBText -notmatch '首样本权重：0 ms') {
        throw 'B 变体必须要求 credit 字段并排除 refill token ownership 求和'
    }

    $creditBrokenRoot = Join-Path $fixtureRoot 'credit-broken'
    Write-Fixture -Root $creditBrokenRoot -DurationSeconds 1800 -Variant B -CreditBroken
    $creditBrokenReport = Join-Path $creditBrokenRoot 'report.md'
    & $reporter -EvidenceRoot $creditBrokenRoot -OutputPath $creditBrokenReport | Out-Null
    $creditBrokenText = Get-Content -LiteralPath $creditBrokenReport -Raw
    if ($creditBrokenText -notmatch '结论：FAIL' -or $creditBrokenText -notmatch 'B credit 守恒：FAIL') {
        throw 'B decode credit 不守恒必须判FAIL并展示守恒门禁'
    }

    $missingSummaryRoot = Join-Path $fixtureRoot 'summary-missing'
    Write-Fixture -Root $missingSummaryRoot -DurationSeconds 1800 -SummaryStatus MISSING
    $missingSummaryReport = Join-Path $missingSummaryRoot 'report.md'
    & $reporter -EvidenceRoot $missingSummaryRoot -OutputPath $missingSummaryReport | Out-Null
    $missingSummaryText = Get-Content -LiteralPath $missingSummaryReport -Raw
    if ($missingSummaryText -notmatch '结论：INCONCLUSIVE' -or $missingSummaryText -notmatch '结果摘要状态为 MISSING') {
        throw 'MISSING 结果摘要只能将单轮判为INCONCLUSIVE'
    }

    $missingCompletedRoot = Join-Path $fixtureRoot 'missing-completed-task-id'
    Write-Fixture -Root $missingCompletedRoot -DurationSeconds 1800 -MissingCompletedTaskId
    $missingCompletedReport = Join-Path $missingCompletedRoot 'report.md'
    & $reporter -EvidenceRoot $missingCompletedRoot -OutputPath $missingCompletedReport | Out-Null
    $missingCompletedText = Get-Content -LiteralPath $missingCompletedReport -Raw
    if ($missingCompletedText -notmatch '结论：INCONCLUSIVE' -or $missingCompletedText -notmatch 'Task ID 绑定：False') {
        throw 'completed persistent task ID 缺失必须让摘要 Task ID 绑定失败并标记INCONCLUSIVE'
    }

    $inconclusiveSummaryRoot = Join-Path $fixtureRoot 'summary-inconclusive'
    Write-Fixture -Root $inconclusiveSummaryRoot -DurationSeconds 1800 -SummaryStatus INCONCLUSIVE
    $inconclusiveSummaryReport = Join-Path $inconclusiveSummaryRoot 'report.md'
    & $reporter -EvidenceRoot $inconclusiveSummaryRoot -OutputPath $inconclusiveSummaryReport | Out-Null
    $inconclusiveSummaryText = Get-Content -LiteralPath $inconclusiveSummaryReport -Raw
    if ($inconclusiveSummaryText -notmatch '结论：INCONCLUSIVE' -or $inconclusiveSummaryText -notmatch '结果摘要状态为 INCONCLUSIVE') {
        throw 'INCONCLUSIVE 结果摘要必须保留证据并标记不确定'
    }

    $runtimeGapRoot = Join-Path $fixtureRoot 'runtime-gap'
    Write-Fixture -Root $runtimeGapRoot -DurationSeconds 1800 -RuntimeGapAtSeconds 100
    $runtimeGapReport = Join-Path $runtimeGapRoot 'report.md'
    & $reporter -EvidenceRoot $runtimeGapRoot -OutputPath $runtimeGapReport | Out-Null
    $runtimeGapText = Get-Content -LiteralPath $runtimeGapReport -Raw
    if ($runtimeGapText -notmatch '结论：INCONCLUSIVE' -or $runtimeGapText -notmatch 'runtime 相邻采样最大间隔') {
        throw 'runtime gap 超过2500ms必须标记INCONCLUSIVE'
    }

    $systemGapRoot = Join-Path $fixtureRoot 'system-gap'
    Write-Fixture -Root $systemGapRoot -DurationSeconds 1800 -SystemGapAtSeconds 100
    $systemGapReport = Join-Path $systemGapRoot 'report.md'
    & $reporter -EvidenceRoot $systemGapRoot -OutputPath $systemGapReport | Out-Null
    $systemGapText = Get-Content -LiteralPath $systemGapReport -Raw
    if ($systemGapText -notmatch '结论：INCONCLUSIVE' -or $systemGapText -notmatch 'system 相邻采样最大间隔') {
        throw 'system gap 超过6000ms必须标记INCONCLUSIVE'
    }

    $missingUtcRoot = Join-Path $fixtureRoot 'missing-utc'
    Write-Fixture -Root $missingUtcRoot -DurationSeconds 1800 -MissingUtcTimestamp
    $missingUtcReport = Join-Path $missingUtcRoot 'report.md'
    & $reporter -EvidenceRoot $missingUtcRoot -OutputPath $missingUtcReport | Out-Null
    $missingUtcText = Get-Content -LiteralPath $missingUtcReport -Raw
    if ($missingUtcText -notmatch '结论：INCONCLUSIVE' -or $missingUtcText -notmatch 'utc_unix_ms') {
        throw '缺少 utc_unix_ms 不得回退 elapsed_seconds，必须标记INCONCLUSIVE'
    }

    $cacheViolationRoot = Join-Path $fixtureRoot 'cache-violation'
    Write-Fixture -Root $cacheViolationRoot -DurationSeconds 1800 -CacheWaitOwnershipViolation
    $cacheViolationReport = Join-Path $cacheViolationRoot 'report.md'
    & $reporter -EvidenceRoot $cacheViolationRoot -OutputPath $cacheViolationReport | Out-Null
    $cacheViolationText = Get-Content -LiteralPath $cacheViolationReport -Raw
    if ($cacheViolationText -notmatch '结论：FAIL' -or $cacheViolationText -notmatch 'cache_wait_resource_ownership_violations=1') {
        throw 'CACHE_WAIT_RESOURCE_OWNERSHIP_VIOLATION 必须判FAIL并展示计数'
    }

    $ownershipExceededRoot = Join-Path $fixtureRoot 'ownership-exceeded'
    Write-Fixture -Root $ownershipExceededRoot -DurationSeconds 1800 -OwnershipExceeded
    $ownershipExceededReport = Join-Path $ownershipExceededRoot 'report.md'
    & $reporter -EvidenceRoot $ownershipExceededRoot -OutputPath $ownershipExceededReport | Out-Null
    if ((Get-Content -LiteralPath $ownershipExceededReport -Raw) -notmatch '结论：FAIL') {
        throw 'ownership 越界必须判FAIL'
    }

    $missingOwnershipRoot = Join-Path $fixtureRoot 'missing-ownership'
    Write-Fixture -Root $missingOwnershipRoot -DurationSeconds 1800 -MissingOwnershipField
    $missingOwnershipReport = Join-Path $missingOwnershipRoot 'report.md'
    & $reporter -EvidenceRoot $missingOwnershipRoot -OutputPath $missingOwnershipReport | Out-Null
    $missingOwnershipText = Get-Content -LiteralPath $missingOwnershipReport -Raw
    if ($missingOwnershipText -notmatch '结论：INCONCLUSIVE' -or $missingOwnershipText -notmatch 'worker_feature') {
        throw '缺少 ownership 字段必须保留证据并标记INCONCLUSIVE'
    }

    $nonDeadlineRoot = Join-Path $fixtureRoot 'non-deadline-cancelled'
    Write-Fixture -Root $nonDeadlineRoot -DurationSeconds 1800 -NonDeadlineCancelled
    $nonDeadlineReport = Join-Path $nonDeadlineRoot 'report.md'
    & $reporter -EvidenceRoot $nonDeadlineRoot -OutputPath $nonDeadlineReport | Out-Null
    if ((Get-Content -LiteralPath $nonDeadlineReport -Raw) -notmatch '结论：FAIL') {
        throw '非到期主动取消扫描必须判FAIL'
    }

    $taskFailedRoot = Join-Path $fixtureRoot 'task-failed'
    Write-Fixture -Root $taskFailedRoot -DurationSeconds 1800 -TaskFailed
    $taskFailedReport = Join-Path $taskFailedRoot 'report.md'
    & $reporter -EvidenceRoot $taskFailedRoot -OutputPath $taskFailedReport | Out-Null
    if ((Get-Content -LiteralPath $taskFailedReport -Raw) -notmatch '结论：FAIL') {
        throw '非到期任务失败必须判FAIL'
    }

    Write-Output 'RUST_V2_RUNTIME_ACCEPTANCE_REPORT_PASS'
}
finally {
    if (Test-Path -LiteralPath $fixtureRoot) {
        Remove-Item -LiteralPath $fixtureRoot -Recurse -Force
    }
}
