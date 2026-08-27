<#
.SYNOPSIS
验证 Rust V2 CPU/I/O A/B test-only 工具的行为契约。

.DESCRIPTION
测试只在临时目录构造可控的包、元数据和六轮证据，不构建 workspace、不启动 Node、
不读取真实媒体。通过真实调用脚本验证边界、顺序、证据绑定和门禁状态。
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$builder = Join-Path $repositoryRoot 'scripts\build-rust-v2-cpu-io-test-package.ps1'
$orchestrator = Join-Path $PSScriptRoot 'Measure-RustV2CpuIoAb.ps1'
$reporter = Join-Path $PSScriptRoot 'New-RustV2CpuIoAbReport.ps1'
$packageVerifier = Join-Path $repositoryRoot 'scripts\verify-release.ps1'
$fixtureRoot = Join-Path ([IO.Path]::GetTempPath()) ('rust-v2-cpu-io-ab-test-' + [Guid]::NewGuid().ToString('N'))

function Write-Utf8NoBom {
    <# 以无 BOM UTF-8 写入夹具，和生产证据编码保持一致。 #>
    param([string] $Path, [string] $Text)
    $parent = Split-Path -Parent $Path
    if ($parent) { New-Item -ItemType Directory -Path $parent -Force | Out-Null }
    [IO.File]::WriteAllText($Path, $Text, [Text.UTF8Encoding]::new($false))
}

function Get-FileSha256OrNull {
    <# 读取夹具文件 SHA；不存在时返回 null，便于验证归档是否被覆盖。 #>
    param([string] $Path)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $null }
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Assert-ArchiveSidecarBinding {
    <# 解析归档 sidecar，确认文件名和实际归档 ZIP SHA 都已重新绑定。 #>
    param([string] $PackagePath)
    $sidecarPath = "$PackagePath.sha256"
    if (-not (Test-Path -LiteralPath $sidecarPath -PathType Leaf)) { throw 'archive sidecar missing' }
    $line = @(Get-Content -LiteralPath $sidecarPath | Where-Object { -not [string]::IsNullOrWhiteSpace($_) } | Select-Object -First 1)
    if ($line.Count -ne 1 -or $line[0] -notmatch '^([0-9a-fA-F]{64})\s+(.+)$') { throw 'archive sidecar malformed' }
    $expectedHash = Get-FileSha256OrNull -Path $PackagePath
    $expectedName = [IO.Path]::GetFileName($PackagePath)
    if ($Matches[1].ToLowerInvariant() -ne $expectedHash -or [IO.Path]::GetFileName([string]$Matches[2]) -ne $expectedName) {
        throw "archive sidecar is not bound to $expectedName"
    }
}

function Get-JsonSemanticSha256 {
    <# 按 Task6 语义对象的压缩 JSON 计算 SHA，不把 pretty 文件布局当作媒体绑定。 #>
    param($Object)
    $text = $Object | ConvertTo-Json -Depth 32 -Compress
    $bytes = [Text.UTF8Encoding]::new($false).GetBytes($text)
    $sha = [Security.Cryptography.SHA256]::Create()
    try { return ([BitConverter]::ToString($sha.ComputeHash($bytes))).Replace('-', '').ToLowerInvariant() }
    finally { $sha.Dispose() }
}

function Write-MinimalPe {
    <# 创建只用于 verifier fixture 的最小 x64 PE 头；不启动该文件。 #>
    param([string] $Path, [UInt16] $Machine = 0x8664)
    $bytes = [byte[]]::new(512)
    $bytes[0] = 0x4d; $bytes[1] = 0x5a
    [BitConverter]::GetBytes([Int32]128).CopyTo($bytes, 0x3c)
    $bytes[128] = 0x50; $bytes[129] = 0x45
    [BitConverter]::GetBytes($Machine).CopyTo($bytes, 132)
    [IO.File]::WriteAllBytes($Path, $bytes)
}

function Write-Manifest {
    <# 生成正式 verifier 需要的 files.sha256，排序保证哈希稳定。 #>
    param([string] $Root)
    $manifest = Join-Path $Root 'manifest\files.sha256'
    $lines = Get-ChildItem -LiteralPath $Root -Recurse -File |
        Where-Object { $_.FullName -ne $manifest } |
        ForEach-Object {
            $relative = [IO.Path]::GetRelativePath($Root, $_.FullName).Replace('\', '/')
            $hash = (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
            "$hash  $relative"
        } | Sort-Object
    Write-Utf8NoBom -Path $manifest -Text (($lines -join "`n") + "`n")
}

function New-FormalFixture {
    <# 创建可被正式 verifier 接受的最小目录和 ZIP，供 test-only builder 隔离验证。 #>
    param([string] $Root, [string] $ZipPath)
    New-Item -ItemType Directory -Path $Root -Force | Out-Null
    foreach ($name in @('desktop.exe', 'node.exe', 'worker.exe', 'Everything.exe')) {
        Write-MinimalPe -Path (Join-Path $Root $name)
    }
    $dirs = @('runtime\ffmpeg', 'licenses', 'schema', 'config') | ForEach-Object { Join-Path $Root $_ }
    New-Item -ItemType Directory -Path $dirs -Force | Out-Null
    foreach ($name in @('avutil-60.dll', 'swresample-6.dll', 'swscale-9.dll', 'avcodec-62.dll', 'avformat-62.dll')) {
        Write-Utf8NoBom -Path (Join-Path $Root "runtime\ffmpeg\$name") -Text $name
    }
    foreach ($name in @('Project-MIT.txt', 'Rust-Third-Party-Licenses.html', 'Slint-Royalty-Free-2.0.txt', 'PDQ-BSD-3-Clause.txt', 'FFmpeg-LGPL-3.0.txt', 'Everything-License.txt', 'Everything-NOTICE.md')) {
        Write-Utf8NoBom -Path (Join-Path $Root "licenses\$name") -Text $name
    }
    Write-Utf8NoBom -Path (Join-Path $Root 'schema\central-v2.sql') -Text '-- fixture'
    Write-Utf8NoBom -Path (Join-Path $Root 'bootstrap.toml') -Text "config_path = 'config/node.toml'`n"
    Write-Utf8NoBom -Path (Join-Path $Root 'config\node.toml') -Text @'
enumerator = "everything"
[paths]
config_path = "config/node.toml"
cache_path = "data/node/cache"
[read]
block_size_bytes = 4194304
block_timeout_seconds = 3
block_retries = 2
'@
    Write-Manifest -Root $Root
    Compress-Archive -Path (Join-Path $Root '*') -DestinationPath $ZipPath -Force
    $zipHash = (Get-FileHash -LiteralPath $ZipPath -Algorithm SHA256).Hash.ToLowerInvariant()
    Write-Utf8NoBom -Path "$ZipPath.sha256" -Text "$zipHash  $(Split-Path -Leaf $ZipPath)`n"
}

function New-FakeFormalScripts {
    <# 提供隔离的同名 build/verify 脚本；测试仍验证 builder 真实调用两者及参数顺序。 #>
    param([string] $Root, [string] $LogPath)
    New-Item -ItemType Directory -Path $Root -Force | Out-Null
    $build = @'
param([string]$CargoTargetDir)
Add-Content -LiteralPath $env:RUST_V2_TEST_FORMAL_CALL_LOG -Value ("BUILD|" + $CargoTargetDir)
Write-Output 'RUST_V2_RELEASE_BUILD_PASS'
Write-Output ("PACKAGE_PATH=" + $env:RUST_V2_TEST_FORMAL_PACKAGE)
'@
    $verify = @'
param([string]$Package)
Add-Content -LiteralPath $env:RUST_V2_TEST_FORMAL_CALL_LOG -Value ("VERIFY|" + $Package)
Write-Output 'PACKAGE_PASS'
'@
    Write-Utf8NoBom -Path (Join-Path $Root 'build-release.ps1') -Text $build
    Write-Utf8NoBom -Path (Join-Path $Root 'verify-release.ps1') -Text $verify
    Write-Utf8NoBom -Path $LogPath -Text ''
}

function Invoke-Script {
    <# 用 pwsh 子进程执行脚本，保留 stdout、stderr 和退出码供断言。 #>
    param([string] $Path, [string[]] $Arguments)
    $stdout = Join-Path $fixtureRoot ('stdout-' + [Guid]::NewGuid().ToString('N') + '.log')
    $stderr = Join-Path $fixtureRoot ('stderr-' + [Guid]::NewGuid().ToString('N') + '.log')
    $p = Start-Process -FilePath ((Get-Command pwsh).Source) -ArgumentList (@('-NoProfile', '-File', $Path) + $Arguments) -Wait -PassThru -WindowStyle Hidden -RedirectStandardOutput $stdout -RedirectStandardError $stderr
    [pscustomobject]@{ ExitCode = $p.ExitCode; Stdout = if (Test-Path $stdout) { Get-Content $stdout -Raw } else { '' }; Stderr = if (Test-Path $stderr) { Get-Content $stderr -Raw } else { '' } }
}

function Update-NdjsonRows {
    <# 仅修改临时证据副本的指定行，保留原始 NDJSON 结构供行为门禁使用。 #>
    param([string] $Path, [scriptblock] $Mutation)
    $rows = foreach ($line in Get-Content -LiteralPath $Path) {
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        $row = $line | ConvertFrom-Json
        & $Mutation $row
        $row | ConvertTo-Json -Depth 20 -Compress
    }
    Write-Utf8NoBom -Path $Path -Text ($rows -join "`n")
}

function Repair-CopiedSummaryPaths {
    <# 复制 evidence 后把 summary 三件套重新绑定到副本，防止测试意外读取原始根。 #>
    param([string] $Root)
    foreach ($name in @('A-1','B-1','B-2','A-2','A-3','B-3')) {
        $harnessPath = Join-Path $Root "$name\evidence\harness-result.json"
        if (-not (Test-Path -LiteralPath $harnessPath -PathType Leaf)) { continue }
        $harness = Get-Content -LiteralPath $harnessPath -Raw | ConvertFrom-Json
        $harness.result_summary_path = Join-Path $Root "$name\evidence\result-summary.jsonl"
        Write-Utf8NoBom -Path $harnessPath -Text ($harness | ConvertTo-Json -Depth 20)
    }
    $manifestPath = Join-Path $Root 'ab-run-manifest.json'
    $mediaManifestPath = Join-Path $Root 'media-input-manifest.json'
    if ((Test-Path -LiteralPath $manifestPath -PathType Leaf) -and (Test-Path -LiteralPath $mediaManifestPath -PathType Leaf)) {
        $manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
        $mediaObject = Get-Content -LiteralPath $mediaManifestPath -Raw | ConvertFrom-Json
        $manifest.media_manifest_path = $mediaManifestPath
        $manifest.media_manifest_sha256 = Get-JsonSemanticSha256 -Object $mediaObject
        Write-Utf8NoBom -Path $manifestPath -Text ($manifest | ConvertTo-Json -Depth 24)
    }
}

function Set-JsonPropertyForTest {
    <# 修改复制证据的 JSON 字段；字段缺失时追加，兼容不同 fixture schema2 版本。 #>
    param(
        [Parameter(Mandatory)] [string] $Path,
        [Parameter(Mandatory)] [string] $PropertyName,
        [object] $Value
    )

    $document = Get-Content -LiteralPath $Path -Raw | ConvertFrom-Json
    $property = $document.PSObject.Properties[$PropertyName]
    if ($null -eq $property) {
        $document | Add-Member -MemberType NoteProperty -Name $PropertyName -Value $Value
    }
    else {
        $property.Value = $Value
    }
    Write-Utf8NoBom -Path $Path -Text ($document | ConvertTo-Json -Depth 24)
}

function Invoke-GateFailureCase {
    <# 每个性能门禁使用独立 evidence 副本，验证聚合器确实从 raw 证据裁决。 #>
    param([string] $Name, [scriptblock] $Mutation, [int] $ExpectedExit = 2)
    $root = Join-Path $fixtureRoot ('gate-' + $Name)
    Copy-Item -LiteralPath $runsRoot -Destination $root -Recurse
    Repair-CopiedSummaryPaths -Root $root
    try {
        & $Mutation $root
        $result = Invoke-Script -Path $reporter -Arguments @('-AbRoot',$root,'-BenchmarkRoot',$benchmark,'-OutputPath',(Join-Path $fixtureRoot "gate-$Name.md"))
        if ($result.ExitCode -ne $ExpectedExit -or $result.Stdout -notmatch "RUST_V2_CPU_IO_AB_REPORT_(FAIL|INCONCLUSIVE)") {
            throw "gate $Name expected exit $ExpectedExit, got $($result.ExitCode): $($result.Stdout) $($result.Stderr)"
        }
    }
    finally {
        if (Test-Path -LiteralPath $root) { Remove-Item -LiteralPath $root -Recurse -Force }
    }
}

function Write-ControlledRunner {
    <# 写入不启动产品的受控单轮 runner，用于验证 orchestrator 的顺序与参数绑定。 #>
    param([string] $Path, [string] $AMetadataPath, [string] $BMetadataPath)
    $text = @'
param(
    [string]$MediaRoot,[int]$DurationSeconds,[int]$SampleSeconds,[string]$ReleaseRoot,
    [string]$AcceptanceClientPath,[string]$ResultExporterPath,[string]$EvidenceRoot,[string]$ReportPath,
    [ValidateSet('A','B')][string]$Variant,[int]$RunIndex,[string]$SourceRevision,[string]$SourceTreeSha256,
    [string]$PackagePath,[string]$PackageSha256,[int]$WorkerCount,[int]$HddThreadsPerDisk,
    [int]$SsdThreadsPerDisk,[int]$UnknownThreadsPerDisk,[int]$TotalReadThreads,[int]$ReservedCores
)
$ErrorActionPreference = 'Stop'
# 受控 runner 是 evidence 的实际 owner；若编排器提前创建目录，立即以稳定 marker 失败。
if (Test-Path -LiteralPath $EvidenceRoot -PathType Container) {
    $marker = 'RUST_V2_CPU_IO_AB_EVIDENCE_PREEXISTS'
    if (-not [string]::IsNullOrWhiteSpace($env:RUST_V2_TEST_AB_RED_EVIDENCE_PATH)) {
        [IO.File]::WriteAllText($env:RUST_V2_TEST_AB_RED_EVIDENCE_PATH, $marker + "`n", [Text.UTF8Encoding]::new($false))
    }
    Write-Output $marker
    exit 17
}
New-Item -ItemType Directory -Path $EvidenceRoot -Force | Out-Null
$metaPath = if ($Variant -eq 'A') { '__A_META__' } else { '__B_META__' }
$meta = Get-Content -LiteralPath $metaPath -Raw | ConvertFrom-Json
$summary = Join-Path $EvidenceRoot 'result-summary.jsonl'
$line = '{"normalized_path":"x","status":"completed","md5":"aa","media_type":"image","feature_payload_sha256":"bb","thumbnail_sha256":"cc","contact_sheet_sha256":"dd"}'
    [IO.File]::WriteAllText($summary, $line, [Text.UTF8Encoding]::new($false))
    $summarySha = (Get-FileHash -LiteralPath $summary -Algorithm SHA256).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText((Join-Path $EvidenceRoot 'result-summary-meta.json'), (@{task_id='task-1';lease_token='lease-1';canonical_sha256=$summarySha;row_count=1;status='PASS'} | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
    [IO.File]::WriteAllText("$summary.pair.lock", (@{schema_version=1;lease_token='lease-1';expected_canonical_sha256=$summarySha;expected_row_count=1;expected_status='PASS'} | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
$mediaRootAbsolute = (Get-Item -LiteralPath $MediaRoot).FullName
$mediaRows = @(Get-ChildItem -LiteralPath $mediaRootAbsolute -Recurse -File -Force | ForEach-Object { [ordered]@{ Path = [IO.Path]::GetRelativePath($mediaRootAbsolute, $_.FullName).Replace('\','/'); Length = [long]$_.Length; LastWriteTimeUtc = $_.LastWriteTimeUtc.ToString('O') } } | Sort-Object Path)
    $mediaTotalBytes = 0L; foreach ($mediaRow in @($mediaRows)) { $mediaTotalBytes += [long]$mediaRow['Length'] }
    $mediaObject = [ordered]@{ Root = $mediaRootAbsolute; FileCount = $mediaRows.Count; TotalBytes = $mediaTotalBytes; Files = @($mediaRows) }
    $media = $mediaObject | ConvertTo-Json -Depth 8
    $mediaSemanticText = $mediaObject | ConvertTo-Json -Depth 32 -Compress
    $mediaSemanticBytes = [Text.UTF8Encoding]::new($false).GetBytes($mediaSemanticText)
    $mediaSemanticShaAlgorithm = [Security.Cryptography.SHA256]::Create()
    try { $mediaSemanticSha = ([BitConverter]::ToString($mediaSemanticShaAlgorithm.ComputeHash($mediaSemanticBytes))).Replace('-', '').ToLowerInvariant() } finally { $mediaSemanticShaAlgorithm.Dispose() }
    [IO.File]::WriteAllText((Join-Path $EvidenceRoot 'media-before.json'), $media, [Text.UTF8Encoding]::new($false))
    [IO.File]::WriteAllText((Join-Path $EvidenceRoot 'media-after.json'), $media, [Text.UTF8Encoding]::new($false))
$metric = { param([int]$Current) [ordered]@{ current = $Current; peak = 4; capacity = 24 } }
$activeSlots = if ($Variant -eq 'B') { 10 } else { 6 }
$waitCurrent = if ($Variant -eq 'B') { 0 } else { 1 }
$decodeCredit = if ($Variant -eq 'B') { 1 } else { 0 }
$pipeline = [ordered]@{
    hash_waiting_permit = (&$metric 0); hash_reading = (&$metric 1); hash_completed_unjoined = (&$metric 0)
    media_permit_waiting = (&$metric $waitCurrent); media_acquire_ready = (&$metric 0); media_permit_ready = (&$metric 0)
    worker_dispatching = (&$metric 0); worker_start_pending = (&$metric 0); worker_decode = (&$metric 1)
    worker_feature = (&$metric 1); worker_result_wait = (&$metric 0); worker_phase_unknown = (&$metric 0)
    worker_slots = [ordered]@{ current = $activeSlots; peak = 12; capacity = 12 }
    content_output_credit_owned = (&$metric 0); hash_refill_token_available = (&$metric 0)
    decode_queue = (&$metric 1); hash_io = (&$metric 1); media_io = (&$metric 1); decode_credit_owned = [ordered]@{ current = $decodeCredit; peak = 4; capacity = 24 }
    item_completion_latency = [ordered]@{ count = 100; buckets = @([ordered]@{ upper_bound_ms = 10; count = 80 }, [ordered]@{ upper_bound_ms = 20; count = 20 }) }
 }
    $executionConfig = [ordered]@{ hash_tasks = 16; path_cache_queue_capacity = 24; content_cache_queue_capacity = 48; decode_queue_capacity = 24; persist_queue_capacity = 1012; worker_slots = 12; cpu_budget = 23; global_disk_permits = 16; hdd_per_disk_permits = 1; ssd_per_disk_permits = 16; unknown_per_disk_permits = 1 }
    $sample1 = [ordered]@{ record_type='runtime_sample'; utc_unix_ms=0; sample_interval_ms=0; runtime_task_id='task-1'; state='running'; overall_total=100; overall_total_known=$true; overall_completed=0; failures=@(); stages=@([ordered]@{stage_id='ComputeBaseFeatures';state=2}); execution_config=$executionConfig; pipeline_metrics=$pipeline }
$terminalPipeline = $pipeline | ConvertTo-Json -Depth 10 | ConvertFrom-Json
foreach ($name in @('hash_waiting_permit','hash_reading','hash_completed_unjoined','media_permit_waiting','media_acquire_ready','media_permit_ready','worker_dispatching','worker_start_pending','worker_decode','worker_feature','worker_result_wait','worker_phase_unknown','worker_slots','content_output_credit_owned','hash_refill_token_available','decode_credit_owned','decode_queue','hash_io','media_io')) { $terminalPipeline.$name.current = 0 }
    $sample2 = [ordered]@{ record_type='runtime_sample'; utc_unix_ms=1000; sample_interval_ms=1000; runtime_task_id='task-1'; state='running'; stages=@([ordered]@{stage_id='ComputeBaseFeatures';state=2}); overall_total=100; overall_total_known=$true; overall_completed=50; failures=@(); execution_config=$executionConfig; pipeline_metrics=$pipeline }
    $sample3 = [ordered]@{ record_type='runtime_sample'; utc_unix_ms=2000; sample_interval_ms=1000; runtime_task_id='task-1'; state='running'; stages=@([ordered]@{stage_id='ComputeBaseFeatures';state=2}); overall_total=100; overall_total_known=$true; overall_completed=100; failures=@(); execution_config=$executionConfig; pipeline_metrics=$pipeline }
$sample3.pipeline_metrics.item_completion_latency = [ordered]@{ count = 100; buckets = @([ordered]@{ upper_bound_ms = 10; count = 90 }, [ordered]@{ upper_bound_ms = 20; count = 10 }) }
$sample1.pipeline_metrics = $pipeline | ConvertTo-Json -Depth 12 | ConvertFrom-Json
$sample2.pipeline_metrics = $pipeline | ConvertTo-Json -Depth 12 | ConvertFrom-Json
$sample3.pipeline_metrics = $pipeline | ConvertTo-Json -Depth 12 | ConvertFrom-Json
$sample1.pipeline_metrics.worker_slots.current = if($Variant -eq 'B'){11}else{10}
$sample2.pipeline_metrics.worker_slots.current = if($Variant -eq 'B'){10}else{8}
$sample3.pipeline_metrics.worker_slots.current = if($Variant -eq 'B'){8}else{4}
$sample3.pipeline_metrics.item_completion_latency = [ordered]@{ count = 100; buckets = @([ordered]@{ upper_bound_ms = 10; count = 90 }, [ordered]@{ upper_bound_ms = 20; count = 10 }) }
    $sample4 = [ordered]@{ record_type='runtime_sample'; utc_unix_ms=3000; sample_interval_ms=1000; runtime_task_id='task-1'; state='completed'; stages=@([ordered]@{stage_id='ComputeBaseFeatures';state=3}); overall_total=100; overall_total_known=$true; overall_completed=100; failures=@(); execution_config=$executionConfig; pipeline_metrics=$terminalPipeline }
    $result = [ordered]@{ record_type='runtime_result'; utc_unix_ms=4000; failed_scans=0; correctness='PASS'; fatal_error=''; diagnostic=''; scan_tasks=@([ordered]@{persistent_task_id='task-1';runtime_task_id='task-1';terminal_state='completed'}) }
[IO.File]::WriteAllText((Join-Path $EvidenceRoot 'runtime.ndjson'), (($sample1,$sample2,$sample3,$sample4,$result | ForEach-Object { $_ | ConvertTo-Json -Depth 12 -Compress }) -join "`n"), [Text.UTF8Encoding]::new($false))
$cpu1 = if ($Variant -eq 'B') { 10 } else { 20 }; $cpu2 = if ($Variant -eq 'B') { 20 } else { 40 }; $cpu3 = if ($Variant -eq 'B') { 40 } else { 80 }
$system1 = [ordered]@{ record_type='system_sample'; utc_unix_ms=500; sample_interval_ms=2000; processes=@([ordered]@{Name='worker';CpuDeltaMs=$cpu1;PrivateMemoryBytes=100}); disks=@([ordered]@{Name='0 C:';disk_number=0;DiskReadBytesPerSec=$(if($Variant -eq 'B'){50}else{100});AvgDiskQueueLength=2},[ordered]@{Name='1 D:';disk_number=1;DiskReadBytesPerSec=$(if($Variant -eq 'B'){25}else{50});AvgDiskQueueLength=1},[ordered]@{Name='_Total';DiskReadBytesPerSec=99999;AvgDiskQueueLength=99}) }
$system2 = [ordered]@{ record_type='system_sample'; utc_unix_ms=2500; sample_interval_ms=2000; processes=@([ordered]@{Name='worker';CpuDeltaMs=$cpu2;PrivateMemoryBytes=100}); disks=@([ordered]@{Name='0 C:';disk_number=0;DiskReadBytesPerSec=$(if($Variant -eq 'B'){400}else{200});AvgDiskQueueLength=2},[ordered]@{Name='1 D:';disk_number=1;DiskReadBytesPerSec=$(if($Variant -eq 'B'){200}else{100});AvgDiskQueueLength=1},[ordered]@{Name='_Total';DiskReadBytesPerSec=99999;AvgDiskQueueLength=99}) }
$system3 = [ordered]@{ record_type='system_sample'; utc_unix_ms=4500; sample_interval_ms=2000; processes=@([ordered]@{Name='worker';CpuDeltaMs=$cpu3;PrivateMemoryBytes=100}); disks=@([ordered]@{Name='0 C:';disk_number=0;DiskReadBytesPerSec=$(if($Variant -eq 'B'){400}else{400});AvgDiskQueueLength=2},[ordered]@{Name='1 D:';disk_number=1;DiskReadBytesPerSec=$(if($Variant -eq 'B'){200}else{200});AvgDiskQueueLength=1},[ordered]@{Name='_Total';DiskReadBytesPerSec=99999;AvgDiskQueueLength=99}) }
$system4 = [ordered]@{ record_type='system_sample'; utc_unix_ms=6500; sample_interval_ms=2000; processes=@([ordered]@{Name='worker';CpuDeltaMs=$cpu3;PrivateMemoryBytes=100}); disks=@([ordered]@{Name='0 C:';disk_number=0;DiskReadBytesPerSec=0;AvgDiskQueueLength=0}) }
[IO.File]::WriteAllText((Join-Path $EvidenceRoot 'system.ndjson'), (($system1,$system2,$system3,$system4 | ForEach-Object { $_ | ConvertTo-Json -Depth 12 -Compress }) -join "`n"), [Text.UTF8Encoding]::new($false))
    $harnessConfigSha = ('f' * 63) + ('{0:x}' -f (($RunIndex - 1) % 16 + 1))
    $harness = [ordered]@{
    schema='rust-v2-runtime-acceptance/v2'; variant=$Variant; run_index=$RunIndex; source_revision=$SourceRevision; source_tree_sha256=$SourceTreeSha256
    package_path=$PackagePath; package_sha256=$PackageSha256; release_root=$ReleaseRoot; package_manifest_sha256=$meta.formal_manifest_sha256
    cargo_lock_sha256=$meta.cargo_lock_sha256; rustc=$meta.rustc; cargo=$meta.cargo; target_triple=$meta.target_triple
    build_config_sha256=$meta.build_config_sha256; runtime_config_sha256=$meta.runtime_config_sha256; media_manifest_sha256=('f'*64)
    media_before_sha256=$mediaSemanticSha; media_after_sha256=$mediaSemanticSha; result_summary_path=$summary; result_summary_sha256=$summarySha
    result_summary_status='PASS'; result_summary_task_id='task-1'; result_summary_row_count=1; result_summary_missing_count=0; result_summary_inconclusive_count=0
    run_status='PASS'; media_unchanged=$true; node_unexpected_exit=$false; exporter_exit_code=0; effective_worker_count=12; failed_scans=0; scan_tasks=@([ordered]@{persistent_task_id='task-1';terminal_state='completed'}); config_sha256=$harnessConfigSha
    hdd_threads_per_disk=1; ssd_threads_per_disk=16; unknown_threads_per_disk=1; read_total_threads=16; reserved_cores=1
    failed_count=0; skipped_count=0
    # 故意写入错误的派生性能值，测试必须仍以 raw runtime/system 通过。
    snapshot_coverage=0.01; throughput_files_per_second=$(if($Variant -eq 'B'){0.01}else{999.0}); idle_worker_seconds_while_media_waits=$(if($Variant -eq 'B'){999.0}else{0.01}); low_io_idle_worker_mean=999.0; high_io_idle_worker_mean=0.0
    low_io_worker_cpu_cores=999.0; high_io_worker_cpu_cores=0.0; disk_read_queue_p95=999.0; private_bytes_peak=1.0; resource_bubble_seconds=$(if($Variant -eq 'B'){999.0}else{0.01})
    completion_tail_seconds=999.0; item_completion_latency_p95=999.0; ownership_current_peak=4; ownership_capacity=24; credit_conservation='PASS'
}
[IO.File]::WriteAllText((Join-Path $EvidenceRoot 'harness-result.json'), ($harness | ConvertTo-Json -Depth 16), [Text.UTF8Encoding]::new($false))
[IO.File]::WriteAllText($ReportPath, "# controlled`n结论：PASS`n", [Text.UTF8Encoding]::new($true))
Write-Output 'RUST_V2_RUNTIME_ACCEPTANCE_MEASURE_PASS'
'@
    $text = $text.Replace('__A_META__', $AMetadataPath).Replace('__B_META__', $BMetadataPath)
    Write-Utf8NoBom -Path $Path -Text $text
}

try {
    New-Item -ItemType Directory -Path $fixtureRoot -Force | Out-Null
    if (-not (Test-Path -LiteralPath $builder)) { throw 'builder missing' }
    if (-not (Test-Path -LiteralPath $reporter)) { throw 'reporter missing' }
    if (-not (Test-Path -LiteralPath $orchestrator)) { throw 'orchestrator missing' }
    # 1. test-only builder：通过正式 verifier 检验 ZIP，且 A 归档后不可被下一次调用覆盖。
    $packages = Join-Path $fixtureRoot 'packages'
    New-Item -ItemType Directory -Path $packages -Force | Out-Null
    $aZip = Join-Path $packages 'source-A-release.zip'; $bZip = Join-Path $packages 'source-B-release.zip'
    New-FormalFixture -Root (Join-Path $packages 'A-formal') -ZipPath $aZip
    New-FormalFixture -Root (Join-Path $packages 'B-formal') -ZipPath $bZip
    $toolClient = Join-Path $fixtureRoot 'runtime_acceptance.exe'; $toolExporter = Join-Path $fixtureRoot 'export_scan_result_summary.exe'
    Write-Utf8NoBom -Path $toolClient -Text 'client fixture'; Write-Utf8NoBom -Path $toolExporter -Text 'exporter fixture'
    $formalScriptsRoot = Join-Path $fixtureRoot 'formal-scripts'; $formalCallLog = Join-Path $fixtureRoot 'formal-calls.log'
    New-FakeFormalScripts -Root $formalScriptsRoot -LogPath $formalCallLog
    $env:RUST_V2_TEST_FORMAL_SCRIPTS_ROOT = $formalScriptsRoot
    $env:RUST_V2_TEST_FORMAL_CALL_LOG = $formalCallLog
    $env:RUST_V2_TEST_FORMAL_PACKAGE = $aZip
    # builder 的 source-tree metadata 排除自身 OutputRoot；A/B 各用独立根避免交叉污染。
    $builtRoot = Join-Path $packages 'built-A'
    $buildARoot = $builtRoot
    $buildBRoot = Join-Path $packages 'built-B'
    $buildA = Invoke-Script -Path $builder -Arguments @('-Variant','A','-OutputRoot',$buildARoot,'-AcceptanceClientPath',$toolClient,'-ResultExporterPath',$toolExporter)
    if ($buildA.ExitCode -ne 0 -or $buildA.Stdout -notmatch 'RUST_V2_CPU_IO_TEST_PACKAGE_PASS') { throw "A test package failed: $($buildA.Stdout)`n$($buildA.Stderr)" }
    $aMeta = Join-Path $builtRoot 'A\test-package.json'; $aRelease = Join-Path $builtRoot 'A\release'; $aArchive = Join-Path $builtRoot 'A\formal\A-formal.zip'
    $aFrozenHash = Get-FileSha256OrNull -Path $aArchive
    Assert-ArchiveSidecarBinding -PackagePath $aArchive
    $env:RUST_V2_TEST_FORMAL_PACKAGE = $bZip
    $buildB = Invoke-Script -Path $builder -Arguments @('-Variant','B','-OutputRoot',$buildBRoot,'-AcceptanceClientPath',$toolClient,'-ResultExporterPath',$toolExporter)
    if ($buildB.ExitCode -ne 0 -or $buildB.Stdout -notmatch 'RUST_V2_CPU_IO_TEST_PACKAGE_PASS') { throw "B test package failed: $($buildB.Stdout)`n$($buildB.Stderr)" }
    $bMeta = Join-Path $buildBRoot 'B\test-package.json'; $bRelease = Join-Path $buildBRoot 'B\release'; $bArchive = Join-Path $buildBRoot 'B\formal\B-formal.zip'
    Assert-ArchiveSidecarBinding -PackagePath $bArchive
    if ((Get-FileSha256OrNull -Path $aArchive) -ne $aFrozenHash) { throw 'A archive changed after B build' }
    $formalCallLines = @(Get-Content -LiteralPath $formalCallLog | Where-Object { $_ })
    if ($formalCallLines.Count -lt 6 -or $formalCallLines[0] -notmatch '^BUILD\|' -or ($formalCallLines | Where-Object { $_ -match '^VERIFY\|' }).Count -lt 4) { throw 'formal build/verify call contract missing' }
    $buildAgain = Invoke-Script -Path $builder -Arguments @('-Variant','A','-OutputRoot',$builtRoot,'-AcceptanceClientPath',$toolClient,'-ResultExporterPath',$toolExporter)
    if ($buildAgain.ExitCode -eq 0 -or $buildAgain.Stderr -notmatch 'VARIANT_ALREADY_EXISTS') { throw 'duplicate A package was accepted' }
    $badSourceBuild = Invoke-Script -Path $builder -Arguments @('-Variant','B','-OutputRoot',(Join-Path $packages 'bad-source'),'-SourceRevision','rev-not-head','-AcceptanceClientPath',$toolClient,'-ResultExporterPath',$toolExporter)
    if ($badSourceBuild.ExitCode -eq 0 -or $badSourceBuild.Stderr -notmatch 'SOURCE_REVISION_MISMATCH') { throw 'caller source revision drift was accepted' }
    foreach ($release in @($aRelease, $bRelease)) {
        if (Get-ChildItem -LiteralPath $release -Recurse -File -Filter 'runtime_acceptance.exe' -ErrorAction SilentlyContinue) { throw 'acceptance client entered release' }
    }

    # 2. 六轮 orchestrator：注入受控 runner，实际验证固定顺序、唯一证据根和 metadata 传递。
    $runner = Join-Path $fixtureRoot 'controlled-runner.ps1'
    Write-ControlledRunner -Path $runner -AMetadataPath $aMeta -BMetadataPath $bMeta
    $media = Join-Path $fixtureRoot 'media'; New-Item -ItemType Directory -Path $media -Force | Out-Null
    Write-Utf8NoBom -Path (Join-Path $media 'read-only.bin') -Text 'media fixture'
    $runsRoot = Join-Path $fixtureRoot 'runs'
    $env:RUST_V2_TEST_AB_MEASURE_RUNNER = $runner
    $orchestratorArgs = @('-MediaRoot',$media,'-BaselineReleaseRoot',$aRelease,'-CandidateReleaseRoot',$bRelease,'-BaselineMetadataPath',$aMeta,'-CandidateMetadataPath',$bMeta,'-AcceptanceClientPath',$toolClient,'-ResultExporterPath',$toolExporter,'-OutputRoot',$runsRoot)
    $ab = Invoke-Script -Path $orchestrator -Arguments $orchestratorArgs
    if ($ab.ExitCode -ne 0 -or $ab.Stdout -notmatch 'RUST_V2_CPU_IO_AB_RUN_COMPLETE') { throw "orchestrator failed: $($ab.Stdout)`n$($ab.Stderr)" }
    $manifest = Get-Content -LiteralPath (Join-Path $runsRoot 'ab-run-manifest.json') -Raw | ConvertFrom-Json
    if ((@($manifest.order) -join ',') -ne 'A-1,B-1,B-2,A-2,A-3,B-3' -or @($manifest.runs).Count -ne 6) { throw 'fixed order or run count mismatch' }
    foreach ($name in @('A-1','B-1','B-2','A-2','A-3','B-3')) { if (-not (Test-Path -LiteralPath (Join-Path $runsRoot "$name\evidence\harness-result.json") -PathType Leaf)) { throw "missing evidence $name" } }

    # 编排器行为：业务 FAIL 必须继续六轮，基础设施失败必须停止且保留已 STARTED/INCONCLUSIVE 记录。
    $businessRunner = Join-Path $fixtureRoot 'business-fail-runner.ps1'; $businessText = [IO.File]::ReadAllText($runner)
    $businessText = $businessText.Replace("Write-Output 'RUST_V2_RUNTIME_ACCEPTANCE_MEASURE_PASS'", "if(`$Variant -eq 'B' -and `$RunIndex -eq 1){Write-Output 'RUST_V2_RUNTIME_ACCEPTANCE_MEASURE_FAIL'}else{Write-Output 'RUST_V2_RUNTIME_ACCEPTANCE_MEASURE_PASS'}")
    Write-Utf8NoBom -Path $businessRunner -Text $businessText
    $env:RUST_V2_TEST_AB_MEASURE_RUNNER = $businessRunner
    $businessRoot = Join-Path $fixtureRoot 'business-fail-runs'; $businessArgs = @($orchestratorArgs); $businessArgs[([Array]::IndexOf($businessArgs, '-OutputRoot') + 1)] = $businessRoot
    $businessResult = Invoke-Script -Path $orchestrator -Arguments $businessArgs
    if ($businessResult.ExitCode -ne 0 -or $businessResult.Stdout -notmatch 'RUST_V2_CPU_IO_AB_RUN_COMPLETE') { throw "business FAIL stopped orchestration: $($businessResult.Stdout)`n$($businessResult.Stderr)" }
    $businessManifest = Get-Content -LiteralPath (Join-Path $businessRoot 'ab-run-manifest.json') -Raw | ConvertFrom-Json
    if (@($businessManifest.runs).Count -ne 6 -or ([string](@($businessManifest.runs)[1].status) -ne 'FAIL')) { throw 'business FAIL did not continue six rounds' }
    $infraRunner = Join-Path $fixtureRoot 'infra-fail-runner.ps1'; $infraText = [IO.File]::ReadAllText($runner)
    $infraText = $infraText.Replace("Write-Output 'RUST_V2_RUNTIME_ACCEPTANCE_MEASURE_PASS'", "if(`$Variant -eq 'B' -and `$RunIndex -eq 1){exit 17}else{Write-Output 'RUST_V2_RUNTIME_ACCEPTANCE_MEASURE_PASS'}")
    Write-Utf8NoBom -Path $infraRunner -Text $infraText
    $env:RUST_V2_TEST_AB_MEASURE_RUNNER = $infraRunner
    $infraRoot = Join-Path $fixtureRoot 'infra-fail-runs'; $infraArgs = @($orchestratorArgs); $infraArgs[([Array]::IndexOf($infraArgs, '-OutputRoot') + 1)] = $infraRoot
    $infraResult = Invoke-Script -Path $orchestrator -Arguments $infraArgs
    if ($infraResult.ExitCode -eq 0 -or $infraResult.Stdout -notmatch 'RUST_V2_CPU_IO_AB_RUN_INCONCLUSIVE') { throw 'infrastructure failure did not stop orchestration' }
    $infraManifest = Get-Content -LiteralPath (Join-Path $infraRoot 'ab-run-manifest.json') -Raw | ConvertFrom-Json
    if (@($infraManifest.runs).Count -ne 2 -or ([string](@($infraManifest.runs)[1].status) -ne 'INCONCLUSIVE')) { throw 'infrastructure failure did not preserve stopped run evidence' }
    $env:RUST_V2_TEST_AB_MEASURE_RUNNER = $runner
    # STARTED 之后的采样异常也必须原子收口，不能留下 RUNNING/STARTED。
    $crashRunner = Join-Path $fixtureRoot 'post-start-crash-runner.ps1'; $crashText = [IO.File]::ReadAllText($runner)
    $crashText = $crashText.Replace("Write-Output 'RUST_V2_RUNTIME_ACCEPTANCE_MEASURE_PASS'", "if(`$Variant -eq 'B' -and `$RunIndex -eq 1){Remove-Item -LiteralPath `$MediaRoot -Recurse -Force}; Write-Output 'RUST_V2_RUNTIME_ACCEPTANCE_MEASURE_PASS'")
    Write-Utf8NoBom -Path $crashRunner -Text $crashText
    $crashMedia = Join-Path $fixtureRoot 'crash-media'; New-Item -ItemType Directory -Path $crashMedia -Force | Out-Null; Write-Utf8NoBom -Path (Join-Path $crashMedia 'read-only.bin') -Text 'crash fixture'
    $env:RUST_V2_TEST_AB_MEASURE_RUNNER = $crashRunner
    $crashRoot = Join-Path $fixtureRoot 'post-start-crash-runs'; $crashArgs = @($orchestratorArgs); $crashArgs[([Array]::IndexOf($crashArgs, '-MediaRoot') + 1)] = $crashMedia; $crashArgs[([Array]::IndexOf($crashArgs, '-OutputRoot') + 1)] = $crashRoot
    $crashResult = Invoke-Script -Path $orchestrator -Arguments $crashArgs
    if ($crashResult.ExitCode -eq 0 -or $crashResult.Stdout -notmatch 'RUST_V2_CPU_IO_AB_RUN_INCONCLUSIVE') { throw 'post-start crash was not inconclusive' }
    $crashManifest = Get-Content -LiteralPath (Join-Path $crashRoot 'ab-run-manifest.json') -Raw | ConvertFrom-Json
    $crashEntry = @($crashManifest.runs)[1]
    if (@($crashManifest.runs).Count -ne 2 -or [string]$crashEntry.status -ne 'INCONCLUSIVE' -or [string]::IsNullOrWhiteSpace([string]$crashEntry.completed_utc) -or -not (Test-Path -LiteralPath (Join-Path $crashRoot 'B-1\ab-run-result.json') -PathType Leaf)) { throw 'post-start crash left incomplete STARTED evidence' }
    $env:RUST_V2_TEST_AB_MEASURE_RUNNER = $runner

    # 3. 聚合器：A/B benchmark 使用固定三轮 median；完整 fixture 必须 PASS。
    $benchmark = Join-Path $fixtureRoot 'benchmark'
    foreach($variant in @('A','B')){
        foreach($index in 1..3){
            $dir=Join-Path $benchmark "$variant-$index";New-Item -ItemType Directory -Path $dir -Force|Out-Null
            $meta=if($variant -eq 'A'){$aMeta}else{$bMeta};$metaObject=Get-Content -LiteralPath $meta -Raw|ConvertFrom-Json
            $elapsed=if($variant -eq 'A'){100}else{80};$benchObject=[ordered]@{variant=$variant;run_index=$index;elapsed_ms=$elapsed;package_sha256=$metaObject.formal_zip_sha256;source_revision=$metaObject.source_revision;cargo_lock_sha256=$metaObject.cargo_lock_sha256;rustc=$metaObject.rustc;cargo=$metaObject.cargo;target_triple=$metaObject.target_triple;build_config_sha256=$metaObject.build_config_sha256;config_sha256=$metaObject.runtime_config_sha256;exe_sha256=$metaObject.node_exe_sha256}
            Write-Utf8NoBom -Path (Join-Path $dir 'benchmark.json') -Text ($benchObject|ConvertTo-Json -Depth 8)
        }
    }
    Write-Utf8NoBom -Path (Join-Path $benchmark 'benchmark-manifest.json') -Text (([ordered]@{ order = @('A-1','A-2','A-3','B-1','B-2','B-3') } | ConvertTo-Json -Depth 8))
    $reportPath = Join-Path $fixtureRoot 'ab-report.md'
    $reportResult = Invoke-Script -Path $reporter -Arguments @('-AbRoot',$runsRoot,'-BenchmarkRoot',$benchmark,'-OutputPath',$reportPath)
    if ($reportResult.ExitCode -ne 0 -or $reportResult.Stdout -notmatch 'RUST_V2_CPU_IO_AB_REPORT_PASS' -or -not (Test-Path -LiteralPath $reportPath -PathType Leaf)) { $reportText = if (Test-Path -LiteralPath $reportPath) { Get-Content -LiteralPath $reportPath -Raw } else { '' }; throw "aggregate PASS fixture failed: $($reportResult.Stdout)`n$($reportResult.Stderr)`n$reportText" }

    # RED：缺少完整 A 变体时，固定 benchmark 已明确低于 15% 仍必须优先输出 FAIL。
    $missingVariantRoot = Join-Path $fixtureRoot 'missing-a-runs'
    Copy-Item -LiteralPath $runsRoot -Destination $missingVariantRoot -Recurse
    Repair-CopiedSummaryPaths -Root $missingVariantRoot
    foreach ($name in @('A-1', 'A-2', 'A-3')) {
        Remove-Item -LiteralPath (Join-Path $missingVariantRoot $name) -Recurse -Force
    }
    $lowBenchmark = Join-Path $fixtureRoot 'low-benchmark'
    Copy-Item -LiteralPath $benchmark -Destination $lowBenchmark -Recurse
    foreach ($index in 1..3) {
        Set-JsonPropertyForTest -Path (Join-Path $lowBenchmark "B-$index\benchmark.json") `
            -PropertyName 'elapsed_ms' -Value 90
    }
    $missingVariantResult = Invoke-Script -Path $reporter -Arguments @(
        '-AbRoot', $missingVariantRoot, '-BenchmarkRoot', $lowBenchmark,
        '-OutputPath', (Join-Path $fixtureRoot 'missing-a-report.md'))
    if ($missingVariantResult.ExitCode -ne 2 -or
        $missingVariantResult.Stdout -notmatch 'RUST_V2_CPU_IO_AB_REPORT_FAIL' -or
        $missingVariantResult.Stdout -match 'RUST_V2_CPU_IO_AB_REPORT_INCONCLUSIVE') {
        throw "RED: 缺完整 A 且 benchmark 明确 FAIL 时必须保留 FAIL，实际 exit=$($missingVariantResult.ExitCode) stdout=$($missingVariantResult.Stdout) stderr=$($missingVariantResult.Stderr)"
    }

    # 单轮 Node 收尾异常必须阻止 A/B 聚合器宣称 PASS，即使 exporter 已返回 0。
    $shutdownWarningRoot = Join-Path $fixtureRoot 'shutdown-warning-runs'
    Copy-Item -LiteralPath $runsRoot -Destination $shutdownWarningRoot -Recurse
    Repair-CopiedSummaryPaths -Root $shutdownWarningRoot
    $shutdownWarningHarnessPath = Join-Path $shutdownWarningRoot 'A-1\evidence\harness-result.json'
    Set-JsonPropertyForTest -Path $shutdownWarningHarnessPath -PropertyName 'run_status' -Value 'INCONCLUSIVE'
    Set-JsonPropertyForTest -Path $shutdownWarningHarnessPath -PropertyName 'run_diagnostic' -Value 'RUST_V2_ACCEPTANCE_NODE_EXIT_TIMEOUT'
    Set-JsonPropertyForTest -Path $shutdownWarningHarnessPath -PropertyName 'exporter_exit_code' -Value 0
    $shutdownWarningReportPath = Join-Path $fixtureRoot 'shutdown-warning-ab-report.md'
    $shutdownWarningResult = Invoke-Script -Path $reporter -Arguments @('-AbRoot',$shutdownWarningRoot,'-BenchmarkRoot',$benchmark,'-OutputPath',$shutdownWarningReportPath)
    $shutdownWarningReport = if (Test-Path -LiteralPath $shutdownWarningReportPath -PathType Leaf) { Get-Content -LiteralPath $shutdownWarningReportPath -Raw } else { '' }
    if ($shutdownWarningResult.ExitCode -ne 3 -or
        $shutdownWarningResult.Stdout -notmatch 'RUST_V2_CPU_IO_AB_REPORT_INCONCLUSIVE' -or
        $shutdownWarningReport -notmatch 'A-1:RUN_INCONCLUSIVE') {
        throw "Node shutdown INCONCLUSIVE 未阻断 A/B 聚合：exit=$($shutdownWarningResult.ExitCode) stdout=$($shutdownWarningResult.Stdout) report=$shutdownWarningReport"
    }

    $zeroBenchmark = Join-Path $fixtureRoot 'zero-benchmark'; Copy-Item -LiteralPath $benchmark -Destination $zeroBenchmark -Recurse
    foreach ($index in 1..3) { $zeroPath = Join-Path $zeroBenchmark "A-$index\benchmark.json"; $zeroObj = Get-Content -LiteralPath $zeroPath -Raw | ConvertFrom-Json; $zeroObj.elapsed_ms = 0; Write-Utf8NoBom -Path $zeroPath -Text ($zeroObj | ConvertTo-Json -Depth 10) }
    $zeroResult = Invoke-Script -Path $reporter -Arguments @('-AbRoot',$runsRoot,'-BenchmarkRoot',$zeroBenchmark,'-OutputPath',(Join-Path $fixtureRoot 'zero-benchmark.md'))
    if ($zeroResult.ExitCode -ne 3 -or $zeroResult.Stdout -notmatch 'RUST_V2_CPU_IO_AB_REPORT_INCONCLUSIVE') { throw 'zero baseline benchmark was not inconclusive' }
    $finalizationRoot = Join-Path $fixtureRoot 'finalization-tail'; Copy-Item -LiteralPath $runsRoot -Destination $finalizationRoot -Recurse; Repair-CopiedSummaryPaths -Root $finalizationRoot
    $finalizationSystem = Join-Path $finalizationRoot 'A-1\evidence\system.ndjson'; Update-NdjsonRows -Path $finalizationSystem -Mutation { param($row); if ([double]$row.utc_unix_ms -eq 6500) { foreach ($process in @($row.processes)) { $process.CpuDeltaMs = 999999; $process.PrivateMemoryBytes = 999999 } foreach ($disk in @($row.disks)) { $disk.DiskReadBytesPerSec = 999999; $disk.AvgDiskQueueLength = 999 } } }
    $finalizationResult = Invoke-Script -Path $reporter -Arguments @('-AbRoot',$finalizationRoot,'-BenchmarkRoot',$benchmark,'-OutputPath',(Join-Path $fixtureRoot 'finalization-tail.md'))
    if ($finalizationResult.ExitCode -ne 0 -or $finalizationResult.Stdout -notmatch 'RUST_V2_CPU_IO_AB_REPORT_PASS') { throw 'finalization tail leaked into production metrics' }
    $bRuns = @('B-1','B-2','B-3')
    Invoke-GateFailureCase -Name 'queued-running' -Mutation {
        param($root)
        $path = Join-Path $root 'B-1\evidence\runtime.ndjson'; Update-NdjsonRows -Path $path -Mutation { param($row); if ([string]$row.record_type -eq 'runtime_result') { $row.scan_tasks[0].terminal_state = 'running' } }
    }
    Invoke-GateFailureCase -Name 'waiting-stage' -ExpectedExit 3 -Mutation {
        param($root)
        foreach ($name in $bRuns) { Update-NdjsonRows -Path (Join-Path $root "$name\evidence\runtime.ndjson") -Mutation { param($row); if ([string]$row.record_type -eq 'runtime_sample') { $row.stages[0].state = 1 } } }
    }
    Invoke-GateFailureCase -Name 'canonical-evidence' -ExpectedExit 3 -Mutation {
        param($root)
        $path = Join-Path $root 'B-1\evidence\result-summary-meta.json'; $obj = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json; $obj.PSObject.Properties.Remove('canonical_sha256'); Write-Utf8NoBom -Path $path -Text ($obj | ConvertTo-Json -Depth 10)
    }

    # 11 个性能门禁分别制造 raw 证据反例；每次只复制临时根，不修改 PASS 基准。
    Invoke-GateFailureCase -Name 'throughput' -Mutation {
        param($root)
        foreach ($name in $bRuns) { Update-NdjsonRows -Path (Join-Path $root "$name\evidence\runtime.ndjson") -Mutation { param($row); if ([string]$row.record_type -eq 'runtime_sample') { $row.overall_total = 1; $row.overall_total_known = $true; $row.overall_completed = if ([double]$row.utc_unix_ms -ge 3000) { 1 } else { 0 } } } }
    }
    Invoke-GateFailureCase -Name 'idle-wait' -Mutation {
        param($root)
        foreach ($name in $bRuns) { Update-NdjsonRows -Path (Join-Path $root "$name\evidence\runtime.ndjson") -Mutation { param($row); if ([string]$row.record_type -eq 'runtime_sample' -and $row.stages[0].state -eq 2) { $row.pipeline_metrics.media_permit_waiting.current = 1 } } }
    }
    Invoke-GateFailureCase -Name 'io-idle-difference' -Mutation {
        param($root)
        foreach ($name in $bRuns) { Update-NdjsonRows -Path (Join-Path $root "$name\evidence\runtime.ndjson") -Mutation { param($row); if ([string]$row.record_type -eq 'runtime_sample' -and [double]$row.utc_unix_ms -eq 2000) { $row.pipeline_metrics.worker_slots.current = 0 } } }
    }
    Invoke-GateFailureCase -Name 'io-cpu-difference' -Mutation {
        param($root)
        foreach ($name in $bRuns) { Update-NdjsonRows -Path (Join-Path $root "$name\evidence\system.ndjson") -Mutation { param($row); if ([double]$row.utc_unix_ms -eq 2500) { foreach ($process in @($row.processes)) { if ([string]$process.Name -eq 'worker') { $process.CpuDeltaMs = 5000 } } } } }
    }
    Invoke-GateFailureCase -Name 'disk-queue' -Mutation {
        param($root)
        foreach ($name in $bRuns) { Update-NdjsonRows -Path (Join-Path $root "$name\evidence\system.ndjson") -Mutation { param($row); if ([double]$row.utc_unix_ms -eq 2500) { foreach ($disk in @($row.disks)) { $disk.AvgDiskQueueLength = 100 } } } }
    }
    Invoke-GateFailureCase -Name 'private-bytes' -Mutation {
        param($root)
        foreach ($name in $bRuns) { Update-NdjsonRows -Path (Join-Path $root "$name\evidence\system.ndjson") -Mutation { param($row); if ([double]$row.utc_unix_ms -eq 2500) { foreach ($process in @($row.processes)) { $process.PrivateMemoryBytes = 10000 } } } }
    }
    Invoke-GateFailureCase -Name 'resource-bubble' -Mutation {
        param($root)
        foreach ($name in $bRuns) { Update-NdjsonRows -Path (Join-Path $root "$name\evidence\runtime.ndjson") -Mutation { param($row); if ([string]$row.record_type -eq 'runtime_sample' -and $row.stages[0].state -eq 2) { $row.pipeline_metrics.worker_slots.current = 0; $row.pipeline_metrics.hash_io.current = 0; $row.pipeline_metrics.media_io.current = 0 } } }
    }
    Invoke-GateFailureCase -Name 'ownership' -Mutation {
        param($root)
        foreach ($name in $bRuns) { Update-NdjsonRows -Path (Join-Path $root "$name\evidence\runtime.ndjson") -Mutation { param($row); if ([string]$row.record_type -eq 'runtime_sample' -and $row.stages[0].state -eq 2) { $row.pipeline_metrics.worker_decode.current = 12 } } }
    }
    Invoke-GateFailureCase -Name 'completion-tail' -ExpectedExit 3 -Mutation {
        param($root)
        foreach ($name in $bRuns) { Update-NdjsonRows -Path (Join-Path $root "$name\evidence\runtime.ndjson") -Mutation { param($row); if ([string]$row.record_type -eq 'runtime_sample' -and [double]$row.utc_unix_ms -ge 2000) { $row.overall_completed = 95; $row.overall_total = 100; $row.overall_total_known = $true } } }
    }
    Invoke-GateFailureCase -Name 'item-latency' -Mutation {
        param($root)
        foreach ($name in $bRuns) { Update-NdjsonRows -Path (Join-Path $root "$name\evidence\runtime.ndjson") -Mutation { param($row); if ([string]$row.record_type -eq 'runtime_sample' -and [double]$row.utc_unix_ms -eq 3000) { $row.pipeline_metrics.item_completion_latency.buckets = @([ordered]@{ upper_bound_ms = 1000; count = 100 }) } } }
    }
    Invoke-GateFailureCase -Name 'zero-denominator' -ExpectedExit 3 -Mutation {
        param($root)
        foreach ($name in $bRuns) { Update-NdjsonRows -Path (Join-Path $root "$name\evidence\system.ndjson") -Mutation { param($row); foreach ($disk in @($row.disks)) { $disk.DiskReadBytesPerSec = 0 } } }
    }
    Invoke-GateFailureCase -Name 'package-drift' -Mutation {
        param($root)
        $path = Join-Path $root 'B-2\evidence\harness-result.json'; $obj = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json; $obj.package_sha256 = ('0' * 64); Write-Utf8NoBom -Path $path -Text ($obj | ConvertTo-Json -Depth 20)
    }
    Invoke-GateFailureCase -Name 'source-drift' -Mutation {
        param($root)
        $path = Join-Path $root 'B-2\evidence\harness-result.json'; $obj = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json; $obj.source_tree_sha256 = ('0' * 64); Write-Utf8NoBom -Path $path -Text ($obj | ConvertTo-Json -Depth 20)
    }
    Invoke-GateFailureCase -Name 'tool-drift' -Mutation {
        param($root)
        $path = Join-Path $root 'ab-run-manifest.json'; $obj = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json; $entry = @($obj.runs | Where-Object { $_.intended.name -eq 'B-2' })[0]; $entry.acceptance_client_sha256 = ('0' * 64); Write-Utf8NoBom -Path $path -Text ($obj | ConvertTo-Json -Depth 20)
    }
    Invoke-GateFailureCase -Name 'config-drift' -Mutation {
        param($root)
        $path = Join-Path $root 'B-2\evidence\runtime.ndjson'; Update-NdjsonRows -Path $path -Mutation { param($row); if ([string]$row.record_type -eq 'runtime_sample') { $row.execution_config.worker_slots = 99 } }
    }
    Invoke-GateFailureCase -Name 'media-drift' -Mutation {
        param($root)
        $path = Join-Path $root 'B-2\evidence\media-after.json'; $obj = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json; $obj.TotalBytes = [long]$obj.TotalBytes + 1; Write-Utf8NoBom -Path $path -Text ($obj | ConvertTo-Json -Depth 20)
    }
    Invoke-GateFailureCase -Name 'runtime-result-failure' -Mutation {
        param($root)
        $path = Join-Path $root 'B-1\evidence\runtime.ndjson'; Update-NdjsonRows -Path $path -Mutation { param($row); if ([string]$row.record_type -eq 'runtime_result') { $row.correctness = 'INCONCLUSIVE'; $row.fatal_error = 'runtime_task_details_failed'; $row.diagnostic = 'runtime_task_details_request_failed'; $row.failed_scans = 1 } }
    }

    # 配对方向反例：全部 runtime 置于 system 之后时，不得把一个未来 snapshot 重复覆盖所有 system。
    $futureRoot = Join-Path $fixtureRoot 'future-runtime'; Copy-Item -LiteralPath $runsRoot -Destination $futureRoot -Recurse; Repair-CopiedSummaryPaths -Root $futureRoot
    $futureRuntime = Join-Path $futureRoot 'A-1\evidence\runtime.ndjson'; $futureRows = foreach($line in Get-Content -LiteralPath $futureRuntime){$row=$line|ConvertFrom-Json;if([string]$row.record_type -eq 'runtime_sample'){$row.utc_unix_ms=[long]$row.utc_unix_ms+10000};$row|ConvertTo-Json -Depth 16 -Compress}; Write-Utf8NoBom -Path $futureRuntime -Text ($futureRows -join "`n")
    $futureResult = Invoke-Script -Path $reporter -Arguments @('-AbRoot',$futureRoot,'-BenchmarkRoot',$benchmark,'-OutputPath',(Join-Path $fixtureRoot 'future-report.md'))
    if ($futureResult.ExitCode -ne 3 -or $futureResult.Stdout -notmatch 'RUST_V2_CPU_IO_AB_REPORT_INCONCLUSIVE') { throw 'future runtime was incorrectly paired to system samples' }

    # 输入 metadata 或固定顺序被篡改时必须拒绝，不得拼接旧证据继续运行。
    $tamperedMeta = Join-Path $fixtureRoot 'tampered-A.json'; Copy-Item -LiteralPath $aMeta -Destination $tamperedMeta
    $tamperedObject = Get-Content -LiteralPath $tamperedMeta -Raw | ConvertFrom-Json; $tamperedObject.formal_zip_sha256 = ('0' * 64); $tamperedObject | ConvertTo-Json -Depth 16 | Set-Content -LiteralPath $tamperedMeta -Encoding utf8
    $tamperedArgs = @($orchestratorArgs)
    $tamperedArgs[([Array]::IndexOf($tamperedArgs, '-BaselineMetadataPath') + 1)] = $tamperedMeta
    $tamperedArgs[([Array]::IndexOf($tamperedArgs, '-OutputRoot') + 1)] = (Join-Path $fixtureRoot 'tampered-runs')
    $tamperedRun = Invoke-Script -Path $orchestrator -Arguments $tamperedArgs
    if ($tamperedRun.ExitCode -eq 0 -or $tamperedRun.Stdout -notmatch 'RUST_V2_CPU_IO_AB_RUN_INCONCLUSIVE') { throw 'tampered metadata was accepted' }
    $badOrderRoot = Join-Path $fixtureRoot 'bad-order'; Copy-Item -LiteralPath $runsRoot -Destination $badOrderRoot -Recurse; Repair-CopiedSummaryPaths -Root $badOrderRoot
    $badManifest = Get-Content -LiteralPath (Join-Path $badOrderRoot 'ab-run-manifest.json') -Raw | ConvertFrom-Json; $badManifest.order[0] = 'B-1'; $badManifest | ConvertTo-Json -Depth 16 | Set-Content -LiteralPath (Join-Path $badOrderRoot 'ab-run-manifest.json') -Encoding utf8
    $badOrder = Invoke-Script -Path $reporter -Arguments @('-AbRoot',$badOrderRoot,'-BenchmarkRoot',$benchmark,'-OutputPath',(Join-Path $fixtureRoot 'bad-order.md'))
    if ($badOrder.ExitCode -ne 3 -or $badOrder.Stdout -notmatch 'RUST_V2_CPU_IO_AB_REPORT_INCONCLUSIVE') { throw "invalid A/B order was accepted: exit=$($badOrder.ExitCode) stdout=$($badOrder.Stdout) stderr=$($badOrder.Stderr)" }
    $externalManifestRoot = Join-Path $fixtureRoot 'external-manifest'; Copy-Item -LiteralPath $runsRoot -Destination $externalManifestRoot -Recurse; Repair-CopiedSummaryPaths -Root $externalManifestRoot
    $externalManifestPath = Join-Path $fixtureRoot 'external-media-input-manifest.json'; Copy-Item -LiteralPath (Join-Path $externalManifestRoot 'media-input-manifest.json') -Destination $externalManifestPath
    $externalManifest = Get-Content -LiteralPath (Join-Path $externalManifestRoot 'ab-run-manifest.json') -Raw | ConvertFrom-Json; $externalManifest.media_manifest_path = $externalManifestPath; Write-Utf8NoBom -Path (Join-Path $externalManifestRoot 'ab-run-manifest.json') -Text ($externalManifest | ConvertTo-Json -Depth 24)
    $externalManifestResult = Invoke-Script -Path $reporter -Arguments @('-AbRoot',$externalManifestRoot,'-BenchmarkRoot',$benchmark,'-OutputPath',(Join-Path $fixtureRoot 'external-manifest.md'))
    if ($externalManifestResult.ExitCode -ne 2 -or $externalManifestResult.Stdout -notmatch 'RUST_V2_CPU_IO_AB_REPORT_FAIL') { throw 'external top-level media manifest was accepted' }

    # confirmed FAIL 必须压过同时存在的缺证，不能被降级成 INCONCLUSIVE。
    $badRoot = Join-Path $fixtureRoot 'bad-runs'; Copy-Item -LiteralPath $runsRoot -Destination $badRoot -Recurse; Repair-CopiedSummaryPaths -Root $badRoot
    $badHarnessPath = Join-Path $badRoot 'B-1\evidence\harness-result.json'
    (Get-Content -LiteralPath $badHarnessPath -Raw | ConvertFrom-Json | ForEach-Object { $_.run_status = 'FAIL'; $_ | ConvertTo-Json -Depth 16 }) | Set-Content -LiteralPath $badHarnessPath -Encoding utf8
    $badRuntimePath = Join-Path $badRoot 'B-1\evidence\runtime.ndjson'; $badRuntimeRows = foreach($line in Get-Content -LiteralPath $badRuntimePath){$row=$line|ConvertFrom-Json;if([string]$row.record_type -eq 'runtime_sample'){$row.failures=@([ordered]@{code='CACHE_WAIT_RESOURCE_OWNERSHIP_VIOLATION';message='fixture'})};$row|ConvertTo-Json -Depth 16 -Compress}; Write-Utf8NoBom -Path $badRuntimePath -Text ($badRuntimeRows -join "`n")
    Remove-Item -LiteralPath (Join-Path $badRoot 'B-2\evidence\system.ndjson')
    $badResult = Invoke-Script -Path $reporter -Arguments @('-AbRoot',$badRoot,'-BenchmarkRoot',$benchmark,'-OutputPath',(Join-Path $fixtureRoot 'bad-report.md'))
    if ($badResult.ExitCode -ne 2 -or $badResult.Stdout -notmatch 'RUST_V2_CPU_IO_AB_REPORT_FAIL') { throw "confirmed FAIL did not take precedence: exit=$($badResult.ExitCode) stdout=$($badResult.Stdout) stderr=$($badResult.Stderr)" }
    Remove-Item Env:RUST_V2_TEST_FORMAL_SCRIPTS_ROOT -ErrorAction SilentlyContinue
    Remove-Item Env:RUST_V2_TEST_FORMAL_CALL_LOG -ErrorAction SilentlyContinue
    Remove-Item Env:RUST_V2_TEST_FORMAL_PACKAGE -ErrorAction SilentlyContinue
    Remove-Item Env:RUST_V2_TEST_AB_MEASURE_RUNNER -ErrorAction SilentlyContinue
    Remove-Item Env:RUST_V2_TEST_AB_RED_EVIDENCE_PATH -ErrorAction SilentlyContinue
    Write-Output 'RUST_V2_CPU_IO_AB_REPORT_PASS'
}
finally {
    Remove-Item Env:RUST_V2_TEST_FORMAL_SCRIPTS_ROOT -ErrorAction SilentlyContinue
    Remove-Item Env:RUST_V2_TEST_FORMAL_CALL_LOG -ErrorAction SilentlyContinue
    Remove-Item Env:RUST_V2_TEST_FORMAL_PACKAGE -ErrorAction SilentlyContinue
    Remove-Item Env:RUST_V2_TEST_AB_MEASURE_RUNNER -ErrorAction SilentlyContinue
    Remove-Item Env:RUST_V2_TEST_AB_RED_EVIDENCE_PATH -ErrorAction SilentlyContinue
    if (Test-Path -LiteralPath $fixtureRoot) { Remove-Item -LiteralPath $fixtureRoot -Recurse -Force }
}
