<#
.SYNOPSIS
按固定 A,B,B,A,A,B 顺序运行六轮 Rust V2 CPU/I/O 真实媒体验收。

.DESCRIPTION
每轮使用独立 evidence/run root，并把 Node/Worker、配置、包和外置工具绑定到该轮。
基础设施失败立即停止并保留原始日志；业务 FAIL 继续完成六轮，由聚合报告最终裁决。
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string] $MediaRoot,
    [ValidateRange(1800, 86400)][int] $DurationSeconds = 1800,
    [ValidateSet(2)][int] $SampleSeconds = 2,
    [Parameter(Mandatory)][string] $BaselineReleaseRoot,
    [Parameter(Mandatory)][string] $CandidateReleaseRoot,
    [Parameter(Mandatory)][string] $BaselineMetadataPath,
    [Parameter(Mandatory)][string] $CandidateMetadataPath,
    [Parameter(Mandatory)][string] $AcceptanceClientPath,
    [Parameter(Mandatory)][string] $ResultExporterPath,
    [Parameter(Mandatory)][string] $OutputRoot,
    [ValidateRange(1, 256)][int] $WorkerCount = 12,
    [ValidateRange(1, 256)][int] $HddThreadsPerDisk = 1,
    [ValidateRange(1, 256)][int] $SsdThreadsPerDisk = 16,
    [ValidateRange(1, 256)][int] $UnknownThreadsPerDisk = 1,
    [ValidateRange(1, 256)][int] $TotalReadThreads = 16,
    [ValidateRange(0, 255)][int] $ReservedCores = 1,
    [bool] $LibraryOnly = $true
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$singleRunScript = Join-Path $PSScriptRoot 'Measure-RustV2RuntimeAcceptance.ps1'
$pwsh = (Get-Command pwsh -ErrorAction Stop).Source
$fixedOrder = @(
    [ordered]@{ variant = 'A'; run_index = 1; name = 'A-1' }
    [ordered]@{ variant = 'B'; run_index = 1; name = 'B-1' }
    [ordered]@{ variant = 'B'; run_index = 2; name = 'B-2' }
    [ordered]@{ variant = 'A'; run_index = 2; name = 'A-2' }
    [ordered]@{ variant = 'A'; run_index = 3; name = 'A-3' }
    [ordered]@{ variant = 'B'; run_index = 3; name = 'B-3' }
)

function Get-FullPathSafe {
    <# 统一绝对路径表示，避免相对路径或尾反斜杠绕过重复检查。 #>
    param([string] $Path)
    if ([string]::IsNullOrWhiteSpace($Path)) { return '' }
    return [IO.Path]::GetFullPath($Path).TrimEnd('\')
}

function Test-PathWithin {
    <# 判断路径是否在根目录内，防止媒体根和输出根重叠。 #>
    param([string] $Candidate, [string] $Root)
    $candidatePath = Get-FullPathSafe $Candidate
    $rootPath = Get-FullPathSafe $Root
    if (-not $candidatePath -or -not $rootPath) { return $false }
    return $candidatePath.Equals($rootPath, [StringComparison]::OrdinalIgnoreCase) -or
        $candidatePath.StartsWith(($rootPath + '\'), [StringComparison]::OrdinalIgnoreCase)
}

function Get-FileSha256 {
    <# 计算并返回小写 SHA-256；metadata 绑定必须以实物哈希为准。 #>
    param([string] $Path)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { throw "AB_FILE_MISSING:$Path" }
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Get-TextSha256 {
    <# 对确定性 UTF-8 文本计算 SHA，供 logical config 和媒体语义清单复用。 #>
    param([AllowNull()][string] $Text)
    $bytes = [Text.UTF8Encoding]::new($false).GetBytes([string]$Text)
    $sha = [Security.Cryptography.SHA256]::Create()
    try { return ([BitConverter]::ToString($sha.ComputeHash($bytes))).Replace('-', '').ToLowerInvariant() }
    finally { $sha.Dispose() }
}

function Get-NormalizedFileSha256 {
    <# 规范化配置换行后计算 SHA，验证 metadata 绑定的是实物配置。 #>
    param([string] $Path)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { throw "AB_FILE_MISSING:$Path" }
    $text = [IO.File]::ReadAllText($Path)
    $normalized = ($text -replace "`r`n", "`n") -replace "`r", "`n"
    $bytes = [Text.UTF8Encoding]::new($false).GetBytes($normalized)
    $sha = [Security.Cryptography.SHA256]::Create()
    try { return ([BitConverter]::ToString($sha.ComputeHash($bytes))).Replace('-', '').ToLowerInvariant() } finally { $sha.Dispose() }
}

function Get-OptionalSha256 {
    <# 文件不存在时返回 null，供校验函数形成明确错误，而非伪造零值。 #>
    param([string] $Path)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $null }
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Get-JsonProperty {
    <# 从 PSCustomObject 安全读取字段，兼容 metadata 的 null。 #>
    param($Object, [string] $Name)
    if ($null -eq $Object) { return $null }
    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property) { return $null }
    return $property.Value
}

function Read-Metadata {
    <# 解析并验证 test-package metadata 与 release/formal package 的绑定。 #>
    param([string] $Path, [ValidateSet('A', 'B')][string] $ExpectedVariant, [string] $ExpectedRelease)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { throw "AB_METADATA_MISSING:$Path" }
    try { $metadata = Get-Content -LiteralPath $Path -Raw | ConvertFrom-Json } catch { throw "AB_METADATA_INVALID:$Path" }
    if ([string](Get-JsonProperty $metadata 'schema') -ne 'rust-v2-cpu-io-test-package/v1') { throw "AB_METADATA_SCHEMA:$Path" }
    if ([string](Get-JsonProperty $metadata 'variant') -ne $ExpectedVariant) { throw "AB_METADATA_VARIANT:$Path" }
    if ((Get-JsonProperty $metadata 'test_only') -ne $true -or (Get-JsonProperty $metadata 'deployable') -ne $false) { throw "AB_METADATA_DEPLOY_FLAG:$Path" }
    foreach ($name in @('source_revision', 'source_tree_sha256', 'cargo_lock_sha256', 'rustc', 'cargo', 'target_triple', 'build_config_sha256', 'runtime_config_sha256', 'formal_zip_path', 'formal_zip_sha256', 'formal_manifest_sha256', 'media_manifest_binding')) {
        if ([string]::IsNullOrWhiteSpace([string](Get-JsonProperty $metadata $name))) { throw "AB_METADATA_FIELD_MISSING:$name" }
    }
    if ([string](Get-JsonProperty $metadata 'media_manifest_binding') -ne 'orchestrator_runtime') { throw "AB_METADATA_MEDIA_BINDING:$ExpectedVariant" }
    if ([string](Get-JsonProperty $metadata 'media_manifest_sha256') -ne 'BOUND_AT_RUN') { throw "AB_METADATA_MEDIA_SHA_POLICY:$ExpectedVariant" }
    $release = Get-FullPathSafe ([string](Get-JsonProperty $metadata 'release_root'))
    if (-not $release -or $release -ne (Get-FullPathSafe $ExpectedRelease)) { throw "AB_METADATA_RELEASE_MISMATCH:$ExpectedVariant" }
    $package = Get-FullPathSafe ([string](Get-JsonProperty $metadata 'formal_zip_path'))
    if (-not (Test-Path -LiteralPath $package -PathType Leaf)) { throw "AB_METADATA_PACKAGE_MISSING:$ExpectedVariant" }
    if ((Get-FileSha256 $package) -ne ([string](Get-JsonProperty $metadata 'formal_zip_sha256')).ToLowerInvariant()) { throw "AB_METADATA_PACKAGE_SHA:$ExpectedVariant" }
    $sidecar = Get-FullPathSafe ([string](Get-JsonProperty $metadata 'formal_zip_sidecar_path'))
    if (-not $sidecar -or -not (Test-Path -LiteralPath $sidecar -PathType Leaf)) { throw "AB_METADATA_SIDECAR_MISSING:$ExpectedVariant" }
    $sidecarLine = @(Get-Content -LiteralPath $sidecar | Where-Object { -not [string]::IsNullOrWhiteSpace($_) } | Select-Object -First 1)
    if ($sidecarLine.Count -ne 1 -or $sidecarLine[0] -notmatch '^([0-9a-fA-F]{64})\s+(.+)$' -or $Matches[1].ToLowerInvariant() -ne (Get-FileSha256 $package)) { throw "AB_METADATA_SIDECAR_MISMATCH:$ExpectedVariant" }
    $manifest = Join-Path $release 'manifest\files.sha256'
    if ((Get-FileSha256 $manifest) -ne ([string](Get-JsonProperty $metadata 'formal_manifest_sha256')).ToLowerInvariant()) { throw "AB_METADATA_MANIFEST_SHA:$ExpectedVariant" }
    $configPath = Get-FullPathSafe ([string](Get-JsonProperty $metadata 'runtime_config_path'))
    if (-not $configPath -or (Get-NormalizedFileSha256 $configPath) -ne ([string](Get-JsonProperty $metadata 'runtime_config_sha256')).ToLowerInvariant()) { throw "AB_METADATA_CONFIG_SHA:$ExpectedVariant" }
    foreach ($toolName in @('acceptance_client', 'result_exporter')) {
        $pathName = "${toolName}_path"; $shaName = "${toolName}_sha256"
        $toolPath = [string](Get-JsonProperty $metadata $pathName)
        if ([string]::IsNullOrWhiteSpace($toolPath) -or -not (Test-Path -LiteralPath $toolPath -PathType Leaf)) { throw "AB_METADATA_TOOL_MISSING:$toolName" }
        if (Test-PathWithin -Candidate $toolPath -Root $release) { throw "AB_METADATA_TOOL_INSIDE_RELEASE:$toolName" }
        if ((Get-FileSha256 $toolPath) -ne ([string](Get-JsonProperty $metadata $shaName)).ToLowerInvariant()) { throw "AB_METADATA_TOOL_SHA:$toolName" }
    }
    return $metadata
}

function Assert-MetadataPair {
    <# 校验 A/B 的可比环境；源码可以不同，其余运行条件必须逐字一致。 #>
    param($Baseline, $Candidate)
    $sameFields = @('cargo_lock_sha256', 'rustc', 'cargo', 'target_triple', 'build_config_sha256', 'runtime_config_sha256', 'acceptance_client_sha256', 'result_exporter_sha256')
    foreach ($field in $sameFields) {
        if ([string](Get-JsonProperty $Baseline $field) -ne [string](Get-JsonProperty $Candidate $field)) { throw "AB_METADATA_ENV_MISMATCH:$field" }
    }
    $baselineMediaBinding = [string](Get-JsonProperty $Baseline 'media_manifest_binding')
    $candidateMediaBinding = [string](Get-JsonProperty $Candidate 'media_manifest_binding')
    if (-not $baselineMediaBinding -or $baselineMediaBinding -ne $candidateMediaBinding) { throw 'AB_METADATA_ENV_MISMATCH:media_manifest_binding' }
    if ([string](Get-JsonProperty $Baseline 'source_tree_sha256') -eq [string](Get-JsonProperty $Candidate 'source_tree_sha256')) {
        # A/B 允许偶然相同，但在同一轮测试中应至少能绑定各自 variant；不把源码相同当作失败。
    }
}

function Write-JsonUtf8 {
    <# 以固定深度写入 manifest/result，避免默认编码差异。 #>
    param([string] $Path, $Object)
    [IO.File]::WriteAllText($Path, (($Object | ConvertTo-Json -Depth 16) + "`n"), [Text.UTF8Encoding]::new($false))
}

function Write-JsonAtomic {
    <# 原子更新顶层清单；先写同目录临时文件，崩溃时保留上一份完整 manifest。 #>
    param([string] $Path, $Object)
    $temporary = "$Path.$([Guid]::NewGuid().ToString('N')).tmp"
    try {
        Write-JsonUtf8 -Path $temporary -Object $Object
        Move-Item -LiteralPath $temporary -Destination $Path -Force
    } finally {
        if (Test-Path -LiteralPath $temporary) { Remove-Item -LiteralPath $temporary -Force }
    }
}

function Get-MediaManifestSnapshot {
    <# 使用 Task6 的 Root/FileCount/TotalBytes/Files schema 固定媒体输入语义。 #>
    param([string] $Root)
    $rootAbsolute = (Get-Item -LiteralPath $Root -ErrorAction Stop).FullName
    $files = @(
        Get-ChildItem -LiteralPath $rootAbsolute -Recurse -File -Force | ForEach-Object {
            $relative = [IO.Path]::GetRelativePath($rootAbsolute, $_.FullName).Replace('\', '/')
            [ordered]@{ Path = $relative; Length = [long]$_.Length; LastWriteTimeUtc = $_.LastWriteTimeUtc.ToString('O') }
        } | Sort-Object Path
    )
    $totalBytes = 0L
    foreach ($file in @($files)) { $totalBytes += [long]$file['Length'] }
    $manifest = [ordered]@{ Root = $rootAbsolute; FileCount = $files.Count; TotalBytes = [long]$totalBytes; Files = @($files) }
    # Task6 的绑定是解析对象的压缩 JSON 语义 SHA；pretty 文件 SHA 只作为旁证保留。
    $semanticText = $manifest | ConvertTo-Json -Depth 32 -Compress
    $fileText = $manifest | ConvertTo-Json -Depth 32
    [pscustomobject]@{
        Manifest = $manifest
        Text = $fileText
        Sha256 = Get-TextSha256 -Text $semanticText
        SemanticSha256 = Get-TextSha256 -Text $semanticText
        FileSha256 = Get-TextSha256 -Text $fileText
    }
}

function Get-MediaManifestSha256 {
    <# 返回媒体规范化清单 SHA；仅作兼容调用，实际清单由快照对象持久化。 #>
    param([string] $Root)
    return (Get-MediaManifestSnapshot -Root $Root).SemanticSha256
}

function Write-Utf8NoBom {
    <# 将顶层媒体清单写成不可变证据，避免六轮只绑定到一个环境变量。 #>
    param([string] $Path, [string] $Text)
    [IO.File]::WriteAllText($Path, $Text, [Text.UTF8Encoding]::new($false))
}

function Invoke-Runner {
    <# 运行单轮 Task6；测试时可注入受控 runner，但生产仍调用 Task6 正式脚本。 #>
    param([string] $ScriptPath, [string[]] $Arguments, [string] $WorkingDirectory, [string] $StdoutPath, [string] $StderrPath)
    $startInfo = [Diagnostics.ProcessStartInfo]::new()
    $startInfo.FileName = $pwsh
    $startInfo.WorkingDirectory = $WorkingDirectory
    $startInfo.UseShellExecute = $false
    $startInfo.CreateNoWindow = $true
    $startInfo.RedirectStandardOutput = $true
    $startInfo.RedirectStandardError = $true
    $startInfo.Arguments = ((@('-NoProfile', '-File', $ScriptPath) + $Arguments) | ForEach-Object {
        if ($_ -match '[\s"]') { '"' + ($_ -replace '(?<!\\)"', '\"') + '"' } else { $_ }
    }) -join ' '
    $process = [Diagnostics.Process]::new(); $process.StartInfo = $startInfo
    if (-not $process.Start()) { throw 'AB_RUNNER_START_FAILED' }
    $stdoutTask = $process.StandardOutput.ReadToEndAsync(); $stderrTask = $process.StandardError.ReadToEndAsync()
    $process.WaitForExit(); $stdout = $stdoutTask.Result; $stderr = $stderrTask.Result
    [IO.File]::WriteAllText($StdoutPath, $stdout, [Text.UTF8Encoding]::new($false))
    [IO.File]::WriteAllText($StderrPath, $stderr, [Text.UTF8Encoding]::new($false))
    [pscustomobject]@{ ExitCode = $process.ExitCode; Stdout = $stdout; Stderr = $stderr }
}

function New-RunEntry {
    <# 构造有序 run manifest 条目，开始前写 STARTED，结束后只更新同一条记录。 #>
    param($Spec, [string] $RunRoot, $Metadata, [string] $Status, [string] $Diagnostic = '', [string] $MediaBefore = '', [string] $MediaAfter = '', [string] $StartedUtc = '', $LogicalConfig = $null, [string] $LogicalConfigSha = '')
    [ordered]@{
        intended = [ordered]@{ variant = $Spec.variant; run_index = $Spec.run_index; name = $Spec.name }
        started_utc = if ($StartedUtc) { $StartedUtc } else { (Get-Date).ToUniversalTime().ToString('o') }
        completed_utc = if ($Status -in @('PASS', 'FAIL', 'INCONCLUSIVE')) { (Get-Date).ToUniversalTime().ToString('o') } else { $null }
        status = $Status
        diagnostic = $Diagnostic
        evidence_root = Join-Path $RunRoot 'evidence'
        report_path = Join-Path $RunRoot 'evidence\report.md'
        package_path = [string](Get-JsonProperty $Metadata 'formal_zip_path')
        package_sha256 = [string](Get-JsonProperty $Metadata 'formal_zip_sha256')
        source_revision = [string](Get-JsonProperty $Metadata 'source_revision')
        source_tree_sha256 = [string](Get-JsonProperty $Metadata 'source_tree_sha256')
        config_sha256 = [string](Get-JsonProperty $Metadata 'runtime_config_sha256')
        metadata_path = [string](Get-JsonProperty $Metadata '_metadata_path')
        acceptance_client_path = [string](Get-JsonProperty $Metadata 'acceptance_client_path')
        acceptance_client_sha256 = [string](Get-JsonProperty $Metadata 'acceptance_client_sha256')
        result_exporter_path = [string](Get-JsonProperty $Metadata 'result_exporter_path')
        result_exporter_sha256 = [string](Get-JsonProperty $Metadata 'result_exporter_sha256')
        media_manifest_binding = [string](Get-JsonProperty $Metadata 'media_manifest_binding')
        media_manifest_before_sha256 = $MediaBefore
        media_manifest_after_sha256 = $MediaAfter
        logical_run_config = $LogicalConfig
        logical_config_sha256 = $LogicalConfigSha
        run_root = $RunRoot
    }
}

try {
    $outputAbsolute = ''
    $manifestPath = ''
    # 当前 STARTED 条目和日志路径跨越 runner 调用保留，异常时可原子收口。
    $currentEntry = $null
    $currentRunRoot = ''
    $currentEvidenceRoot = ''
    $currentStdoutPath = ''
    $currentStderrPath = ''
    $currentMediaBefore = ''
    $currentSpec = $null
    $mediaAbsolute = Get-FullPathSafe $MediaRoot
    $outputAbsolute = Get-FullPathSafe $OutputRoot
    if (-not (Test-Path -LiteralPath $mediaAbsolute -PathType Container)) { throw 'AB_MEDIA_ROOT_MISSING' }
    if ((Test-PathWithin -Candidate $outputAbsolute -Root $mediaAbsolute) -or
        (Test-PathWithin -Candidate $mediaAbsolute -Root $outputAbsolute)) { throw 'AB_OUTPUT_MEDIA_OVERLAP' }
    if ($outputAbsolute -match '^(?i:I:\\Tool)(?:\\|$)' -or $mediaAbsolute -match '^(?i:I:\\Tool)(?:\\|$)') { throw 'AB_PRODUCTION_PATH_FORBIDDEN' }
    if (Test-Path -LiteralPath $outputAbsolute) { throw 'AB_OUTPUT_ROOT_NOT_FRESH' }
    New-Item -ItemType Directory -Path $outputAbsolute -Force | Out-Null
    $baseline = Read-Metadata -Path $BaselineMetadataPath -ExpectedVariant A -ExpectedRelease $BaselineReleaseRoot
    $candidate = Read-Metadata -Path $CandidateMetadataPath -ExpectedVariant B -ExpectedRelease $CandidateReleaseRoot
    if ((Get-FullPathSafe $BaselineReleaseRoot) -eq (Get-FullPathSafe $CandidateReleaseRoot)) { throw 'AB_RELEASE_ROOT_MUST_DIFFER' }
    if ((Test-PathWithin -Candidate $BaselineReleaseRoot -Root $CandidateReleaseRoot) -or (Test-PathWithin -Candidate $CandidateReleaseRoot -Root $BaselineReleaseRoot)) { throw 'AB_RELEASE_ROOTS_OVERLAP' }
    foreach ($release in @($BaselineReleaseRoot, $CandidateReleaseRoot)) { if ((Get-FullPathSafe $release) -match '^(?i:I:\\Tool)(?:\\|$)') { throw 'AB_PRODUCTION_PATH_FORBIDDEN' } }
    foreach ($release in @($BaselineReleaseRoot, $CandidateReleaseRoot)) {
        if ((Test-PathWithin -Candidate $outputAbsolute -Root $release) -or (Test-PathWithin -Candidate $mediaAbsolute -Root $release) -or
            (Test-PathWithin -Candidate $release -Root $outputAbsolute) -or (Test-PathWithin -Candidate $release -Root $mediaAbsolute)) { throw 'AB_RELEASE_MEDIA_BOUNDARY' }
    }
    Assert-MetadataPair -Baseline $baseline -Candidate $candidate
    if ((Get-FullPathSafe $AcceptanceClientPath) -ne (Get-FullPathSafe ([string](Get-JsonProperty $baseline 'acceptance_client_path'))) -or
        (Get-FullPathSafe $ResultExporterPath) -ne (Get-FullPathSafe ([string](Get-JsonProperty $baseline 'result_exporter_path')))) {
        throw 'AB_EXTERNAL_TOOL_BINDING_MISMATCH'
    }
    foreach ($tool in @($AcceptanceClientPath, $ResultExporterPath)) {
        if ((Test-PathWithin -Candidate $tool -Root $outputAbsolute) -or (Test-PathWithin -Candidate $tool -Root $mediaAbsolute) -or
            (Test-PathWithin -Candidate $tool -Root $BaselineReleaseRoot) -or (Test-PathWithin -Candidate $tool -Root $CandidateReleaseRoot)) {
            throw 'AB_EXTERNAL_TOOL_PATH_FORBIDDEN'
        }
        if ((Test-PathWithin -Candidate $outputAbsolute -Root $tool) -or (Test-PathWithin -Candidate $mediaAbsolute -Root $tool) -or
            (Test-PathWithin -Candidate $BaselineReleaseRoot -Root $tool) -or (Test-PathWithin -Candidate $CandidateReleaseRoot -Root $tool)) {
            throw 'AB_EXTERNAL_TOOL_PATH_INTERSECTS'
        }
        if ($tool -match '^(?i:I:\\Tool)(?:\\|$)') { throw 'AB_PRODUCTION_PATH_FORBIDDEN' }
    }
    # 只包含跨轮稳定参数；端口、隔离 data/cache 路径留在 Task6 本轮 config SHA 中。
    $logicalRunConfig = [ordered]@{
        duration_seconds = [int]$DurationSeconds
        sample_seconds = [int]$SampleSeconds
        worker_count = [int]$WorkerCount
        hdd_threads_per_disk = [int]$HddThreadsPerDisk
        ssd_threads_per_disk = [int]$SsdThreadsPerDisk
        unknown_threads_per_disk = [int]$UnknownThreadsPerDisk
        total_read_threads = [int]$TotalReadThreads
        reserved_cores = [int]$ReservedCores
        library_only = [bool]$LibraryOnly
    }
    $logicalConfigSha = Get-TextSha256 -Text ($logicalRunConfig | ConvertTo-Json -Depth 8 -Compress)
    $manifestPath = Join-Path $outputAbsolute 'ab-run-manifest.json'
    # 首次快照固定实际媒体输入；每轮 before/after 必须和这份语义清单一致。
    $mediaManifestInitial = Get-MediaManifestSnapshot -Root $mediaAbsolute
    $mediaManifestPath = Join-Path $outputAbsolute 'media-input-manifest.json'
    Write-Utf8NoBom -Path $mediaManifestPath -Text $mediaManifestInitial.Text
    $manifest = [ordered]@{
        schema = 'rust-v2-cpu-io-ab-run/v1'; order = @($fixedOrder | ForEach-Object { $_.name });
        status = 'RUNNING'; intended = @(); runs = @(); created_utc = (Get-Date).ToUniversalTime().ToString('o')
        output_root = $outputAbsolute; media_root = $mediaAbsolute; media_manifest_path = $mediaManifestPath; media_manifest_sha256 = $mediaManifestInitial.SemanticSha256; media_manifest_file_sha256 = Get-FileSha256 $mediaManifestPath; duration_seconds = $DurationSeconds; sample_seconds = $SampleSeconds
        worker_count = $WorkerCount; hdd_threads_per_disk = $HddThreadsPerDisk; ssd_threads_per_disk = $SsdThreadsPerDisk
        unknown_threads_per_disk = $UnknownThreadsPerDisk; total_read_threads = $TotalReadThreads; reserved_cores = $ReservedCores; library_only = [bool]$LibraryOnly
        logical_run_config = $logicalRunConfig; logical_config_sha256 = $logicalConfigSha
        variants = [ordered]@{ A = [ordered]@{ metadata_path = (Get-FullPathSafe $BaselineMetadataPath); release_root = (Get-FullPathSafe $BaselineReleaseRoot) }; B = [ordered]@{ metadata_path = (Get-FullPathSafe $CandidateMetadataPath); release_root = (Get-FullPathSafe $CandidateReleaseRoot) } }
    }
    Write-JsonAtomic -Path $manifestPath -Object $manifest
    $runnerOverride = [Environment]::GetEnvironmentVariable('RUST_V2_TEST_AB_MEASURE_RUNNER', 'Process')
    $runnerScript = if ($runnerOverride) { Get-FullPathSafe $runnerOverride } else { $singleRunScript }
    if (-not (Test-Path -LiteralPath $runnerScript -PathType Leaf)) { throw 'AB_SINGLE_RUN_SCRIPT_MISSING' }

    $completedBusinessFailures = 0
    foreach ($spec in $fixedOrder) {
        $runRoot = Join-Path $outputAbsolute $spec.name
        $evidenceRoot = Join-Path $runRoot 'evidence'
        $metadata = if ($spec.variant -eq 'A') { $baseline } else { $candidate }
        $metadataPath = if ($spec.variant -eq 'A') { Get-FullPathSafe $BaselineMetadataPath } else { Get-FullPathSafe $CandidateMetadataPath }
        $metadata | Add-Member -NotePropertyName _metadata_path -NotePropertyValue $metadataPath -Force
        $release = if ($spec.variant -eq 'A') { $BaselineReleaseRoot } else { $CandidateReleaseRoot }
        if (Test-Path -LiteralPath $runRoot) { throw "AB_RUN_ROOT_NOT_FRESH:$($spec.name)" }
        # runRoot 仅承载 runner stdout/stderr 与顶层结果；evidenceRoot 由单轮 collector 独占创建。
        New-Item -ItemType Directory -Path $runRoot -Force | Out-Null
        $startedUtc = (Get-Date).ToUniversalTime().ToString('o')
        $mediaBefore = Get-MediaManifestSha256 -Root $mediaAbsolute
        $entry = New-RunEntry -Spec $spec -RunRoot $runRoot -Metadata $metadata -Status 'STARTED' -MediaBefore $mediaBefore -StartedUtc $startedUtc -LogicalConfig $logicalRunConfig -LogicalConfigSha $logicalConfigSha
        $manifest.intended += [ordered]@{ variant = $spec.variant; run_index = $spec.run_index; name = $spec.name; intended_utc = $startedUtc }
        $manifest.runs += $entry
        Write-JsonAtomic -Path $manifestPath -Object $manifest
        $currentEntry = $entry
        $currentRunRoot = $runRoot
        $currentEvidenceRoot = $evidenceRoot
        $args = @(
            '-MediaRoot', $mediaAbsolute, '-DurationSeconds', [string]$DurationSeconds, '-SampleSeconds', [string]$SampleSeconds,
            '-ReleaseRoot', (Get-FullPathSafe $release), '-AcceptanceClientPath', (Get-FullPathSafe $AcceptanceClientPath),
            '-ResultExporterPath', (Get-FullPathSafe $ResultExporterPath), '-EvidenceRoot', $evidenceRoot,
            '-ReportPath', (Join-Path $evidenceRoot 'report.md'), '-Variant', $spec.variant, '-RunIndex', [string]$spec.run_index,
            '-SourceRevision', [string](Get-JsonProperty $metadata 'source_revision'), '-SourceTreeSha256', [string](Get-JsonProperty $metadata 'source_tree_sha256'),
            '-PackagePath', [string](Get-JsonProperty $metadata 'formal_zip_path'), '-PackageSha256', [string](Get-JsonProperty $metadata 'formal_zip_sha256'),
            '-WorkerCount', [string]$WorkerCount, '-HddThreadsPerDisk', [string]$HddThreadsPerDisk, '-SsdThreadsPerDisk', [string]$SsdThreadsPerDisk,
            '-UnknownThreadsPerDisk', [string]$UnknownThreadsPerDisk, '-TotalReadThreads', [string]$TotalReadThreads, '-ReservedCores', [string]$ReservedCores
        )
        $stdoutPath = Join-Path $runRoot 'runner.stdout.log'; $stderrPath = Join-Path $runRoot 'runner.stderr.log'
        $currentStdoutPath = $stdoutPath
        $currentStderrPath = $stderrPath
        $currentMediaBefore = $mediaBefore
        $currentSpec = $spec
        $run = $null; $status = 'INCONCLUSIVE'; $diagnostic = ''
        try {
            $run = Invoke-Runner -ScriptPath $runnerScript -Arguments $args -WorkingDirectory $repositoryRoot -StdoutPath $stdoutPath -StderrPath $stderrPath
            if ($run.ExitCode -eq 0 -and $run.Stdout -match 'RUST_V2_RUNTIME_ACCEPTANCE_MEASURE_(PASS|FAIL)') {
                $status = $Matches[1]
                if ($status -eq 'FAIL') { $completedBusinessFailures++ }
            }
            elseif ($run.Stdout -match 'RUST_V2_RUNTIME_ACCEPTANCE_MEASURE_INCONCLUSIVE') {
                $status = 'INCONCLUSIVE'; $diagnostic = 'SINGLE_RUN_INCONCLUSIVE'
            }
            else {
                $diagnostic = "SINGLE_RUN_INFRASTRUCTURE_FAILED:exit=$($run.ExitCode)"
            }
        }
        catch { $diagnostic = $_.Exception.Message }
        $mediaAfter = Get-MediaManifestSha256 -Root $mediaAbsolute
        if ($mediaBefore -ne $mediaManifestInitial.Sha256) { $status = 'FAIL'; $diagnostic = if ($diagnostic) { "$diagnostic;MEDIA_INPUT_MANIFEST_CHANGED" } else { 'MEDIA_INPUT_MANIFEST_CHANGED' }; $completedBusinessFailures++ }
        if ($mediaAfter -ne $mediaManifestInitial.Sha256) { $status = 'FAIL'; $diagnostic = if ($diagnostic) { "$diagnostic;MEDIA_MANIFEST_CHANGED" } else { 'MEDIA_MANIFEST_CHANGED' }; $completedBusinessFailures++ }
        $entry.status = $status
        $entry.diagnostic = $diagnostic
        $entry.completed_utc = if ($status -in @('PASS', 'FAIL', 'INCONCLUSIVE')) { (Get-Date).ToUniversalTime().ToString('o') } else { $null }
        $entry.media_manifest_after_sha256 = $mediaAfter
        $manifest.status = if ($status -eq 'INCONCLUSIVE') { 'INCONCLUSIVE' } else { 'RUNNING' }
        Write-JsonAtomic -Path (Join-Path $runRoot 'ab-run-result.json') -Object ([ordered]@{ schema = 'rust-v2-cpu-io-ab-run-result/v1'; status = $status; variant = $spec.variant; run_index = $spec.run_index; diagnostic = $diagnostic; stdout_path = $stdoutPath; stderr_path = $stderrPath; evidence_root = $evidenceRoot; report_path = (Join-Path $evidenceRoot 'report.md'); media_manifest_before_sha256 = $mediaBefore; media_manifest_after_sha256 = $mediaAfter })
        Write-JsonAtomic -Path $manifestPath -Object $manifest
        if ($status -eq 'INCONCLUSIVE') {
            $manifest.status = 'INCONCLUSIVE'; Write-JsonAtomic -Path $manifestPath -Object $manifest
            Write-Output 'RUST_V2_CPU_IO_AB_RUN_INCONCLUSIVE'
            Write-Output "AB_ROOT=$outputAbsolute"; Write-Output "AB_MANIFEST=$manifestPath"
            exit 1
        }
        $currentEntry = $null
        $currentRunRoot = ''
        $currentEvidenceRoot = ''
        $currentStdoutPath = ''
        $currentStderrPath = ''
        $currentMediaBefore = ''
        $currentSpec = $null
    }
    $manifest.status = if ($completedBusinessFailures -gt 0) { 'COMPLETE_WITH_BUSINESS_FAILURES' } else { 'COMPLETE' }
    Write-JsonAtomic -Path $manifestPath -Object $manifest
    Write-Output 'RUST_V2_CPU_IO_AB_RUN_COMPLETE'
    Write-Output "AB_ROOT=$outputAbsolute"
    Write-Output "AB_MANIFEST=$manifestPath"
}
catch {
    $diagnostic = "{0} at {1}:{2}" -f $_.Exception.Message, $_.InvocationInfo.ScriptName, $_.InvocationInfo.ScriptLineNumber
    if ($outputAbsolute) {
        try {
            $manifestPath = Join-Path $outputAbsolute 'ab-run-manifest.json'
            if ($null -ne $currentEntry) {
                $currentEntry.status = 'INCONCLUSIVE'
                $currentEntry.completed_utc = (Get-Date).ToUniversalTime().ToString('o')
                $currentEntry.diagnostic = $diagnostic
                if (Test-Path -LiteralPath $manifestPath -PathType Leaf) {
                    $manifest.status = 'INCONCLUSIVE'
                    Write-JsonAtomic -Path $manifestPath -Object $manifest
                }
                $resultPath = if ($currentRunRoot) { Join-Path $currentRunRoot 'ab-run-result.json' } else { '' }
                if ($resultPath) {
                    Write-JsonAtomic -Path $resultPath -Object ([ordered]@{
                        schema = 'rust-v2-cpu-io-ab-run-result/v1'; status = 'INCONCLUSIVE';
                        variant = if ($currentSpec) { $currentSpec.variant } else { $null };
                        run_index = if ($currentSpec) { $currentSpec.run_index } else { $null };
                        diagnostic = $diagnostic; stdout_path = $currentStdoutPath; stderr_path = $currentStderrPath;
                        evidence_root = $currentEvidenceRoot; report_path = if ($currentEvidenceRoot) { Join-Path $currentEvidenceRoot 'report.md' } else { $null };
                        media_manifest_before_sha256 = $currentMediaBefore; media_manifest_after_sha256 = $null
                    })
                }
            }
            elseif (-not (Test-Path $manifestPath)) {
                Write-JsonUtf8 -Path $manifestPath -Object ([ordered]@{ schema = 'rust-v2-cpu-io-ab-run/v1'; status = 'INCONCLUSIVE'; order = @($fixedOrder.name); diagnostic = $diagnostic })
            }
        } catch { }
    }
    [Console]::Error.WriteLine($diagnostic)
    Write-Output 'RUST_V2_CPU_IO_AB_RUN_INCONCLUSIVE'
    exit 1
}
