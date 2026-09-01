<#
.SYNOPSIS
验证单轮 harness 的结果摘要固定 stdout、单一 TSV 校验与结果接线。
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$measureScript = Join-Path $repositoryRoot 'tests\windows\Measure-RustV2RuntimeAcceptance.ps1'
$fixtureRoot = Join-Path ([IO.Path]::GetTempPath()) ("rust-v2-result-summary-" + [Guid]::NewGuid().ToString('N'))

function Get-TestSummaryColumns {
    <# 返回结果摘要固定的 29 个 TSV 列名。 #>
    @(
        'record_type', 'status', 'machine_id', 'normalized_path', 'display_path',
        'file_size', 'md5', 'media_type', 'base_complete', 'feature_payload_sha256',
        'image_stage1_sha256', 'image_stage2_sha256', 'video_metadata_sha256',
        'video_frame_stage1_0_sha256', 'video_frame_stage1_1_sha256',
        'video_frame_stage1_2_sha256', 'video_frame_stage1_3_sha256',
        'video_frame_stage1_4_sha256', 'video_frame_stage1_5_sha256',
        'video_frame_stage2_0_sha256', 'video_frame_stage2_1_sha256',
        'video_frame_stage2_2_sha256', 'video_frame_stage2_3_sha256',
        'video_frame_stage2_4_sha256', 'video_frame_stage2_5_sha256',
        'thumbnail_sha256', 'thumbnail_state', 'contact_sheet_sha256', 'status_reason'
    )
}

function Write-TestResultSummary {
    <# 写入一个独立 TSV 夹具；可选择破坏 footer、行宽或换行格式。 #>
    param(
        [Parameter(Mandatory)] [string] $Path,
        [ValidateSet('PASS', 'MISSING', 'INCONCLUSIVE')] [string] $Status = 'PASS',
        [switch] $BrokenFooter,
        [switch] $MalformedRow,
        [switch] $UseCrLf
    )

    $columns = @(Get-TestSummaryColumns)
    $row = @(
        'R', $Status, ('1' * 64), 'H:\media\a.mp4', 'H:\media\a.mp4', '1024',
        ('b' * 32), 'video', 'true', ('c' * 64), '', '', ('d' * 64),
        ('e' * 64), ('e' * 64), ('e' * 64), ('e' * 64), ('e' * 64), ('e' * 64),
        ('f' * 64), ('f' * 64), ('f' * 64), ('f' * 64), ('f' * 64), ('f' * 64),
        '', '', ('1' * 64), ''
    )
    if ($MalformedRow) {
        $row = @($row | Select-Object -First 28)
    }
    $newline = if ($UseCrLf) { "`r`n" } else { "`n" }
    $dataText = (($columns -join "`t") + $newline + ($row -join "`t") + $newline)
    $dataSha256 = Get-TextSha256 -Text $dataText
    if ($BrokenFooter) {
        $dataSha256 = '0' * 64
    }
    $text = $dataText + "F`t1`t$dataSha256$newline"
    [IO.File]::WriteAllText($Path, $text, [Text.UTF8Encoding]::new($false))
    [pscustomobject]@{
        Path = [IO.Path]::GetFullPath($Path)
        Sha256 = (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
        RowCount = 1L
        Status = $Status
    }
}

try {
    if (-not (Test-Path -LiteralPath $measureScript -PathType Leaf)) {
        throw "RUST_V2_RESULT_SUMMARY_HARNESS_MISSING path=$measureScript"
    }
    . $measureScript -LibraryOnly

    $evidence = Join-Path $fixtureRoot 'runs\A-1\evidence'
    New-Item -ItemType Directory -Path $evidence -Force | Out-Null
    $summaryPath = Join-Path $evidence 'result-summary.tsv'
    $summary = Write-TestResultSummary -Path $summaryPath

    $stdout = @(
        'RESULT_SUMMARY_STATUS=PASS'
        "RESULT_SUMMARY_PATH=$summaryPath"
        "RESULT_SUMMARY_SHA256=$($summary.Sha256)"
        'RESULT_SUMMARY_ROW_COUNT=1'
        'RESULT_SUMMARY_MISSING_COUNT=0'
        'RESULT_SUMMARY_INCONCLUSIVE_COUNT=0'
    ) -join "`n"
    $parsed = Parse-ResultSummaryOutput -Text $stdout -ExpectedPath $summaryPath
    if ($parsed.Status -ne 'PASS' -or $parsed.Path -cne ([IO.Path]::GetFullPath($summaryPath)) -or
        $parsed.RowCount -ne 1 -or $parsed.MissingCount -ne 0 -or
        $parsed.InconclusiveCount -ne 0 -or $parsed.TaskId) {
        throw "固定 exporter stdout 解析错误：$($parsed | ConvertTo-Json -Compress)"
    }

    $missingFieldRejected = $false
    try {
        Parse-ResultSummaryOutput -Text ($stdout -replace 'RESULT_SUMMARY_SHA256=.*', '') `
            -ExpectedPath $summaryPath | Out-Null
    }
    catch {
        $missingFieldRejected = $_.Exception.Message -like '*RUST_V2_RESULT_SUMMARY_FIELD_MISSING*'
    }
    if (-not $missingFieldRejected) {
        throw '缺少固定 stdout 字段必须被拒绝'
    }

    $artifacts = Get-ResultSummaryArtifacts -SummaryPath $summaryPath `
        -ExpectedStatus $parsed.Status -ExpectedSha256 $parsed.Sha256 -ExpectedRowCount $parsed.RowCount
    if (-not $artifacts.SummaryExists -or -not $artifacts.BindingValid -or
        $artifacts.MetadataExists -or $artifacts.LeaseExists -or
        $null -ne $artifacts.MetadataPath -or $null -ne $artifacts.LeasePath) {
        throw "单一 TSV 校验错误：$($artifacts | ConvertTo-Json -Compress)"
    }
    foreach ($legacyName in @('result-summary.jsonl', 'result-summary-meta.json', 'result-summary.tsv.pair.lock')) {
        if (Test-Path -LiteralPath (Join-Path $evidence $legacyName)) {
            throw "结果摘要不得创建旧旁车文件：$legacyName"
        }
    }

    $wrongSha = Get-ResultSummaryArtifacts -SummaryPath $summaryPath `
        -ExpectedStatus PASS -ExpectedSha256 ('0' * 64) -ExpectedRowCount 1
    if ($wrongSha.BindingValid -or $wrongSha.Diagnostic -ne 'RUST_V2_ACCEPTANCE_RESULT_SUMMARY_BINDING_INVALID') {
        throw '完整 TSV SHA 不一致时必须拒绝绑定'
    }

    $legacyTaskBinding = Get-ResultSummaryArtifacts -SummaryPath $summaryPath `
        -ExpectedTaskId 'legacy-task-id' -ExpectedStatus PASS `
        -ExpectedSha256 $summary.Sha256 -ExpectedRowCount 1
    if ($legacyTaskBinding.BindingValid) {
        throw '瞬态扫描结果不得继续依赖旧 Task ID 绑定'
    }

    $broken = Write-TestResultSummary -Path $summaryPath -BrokenFooter
    $brokenArtifacts = Get-ResultSummaryArtifacts -SummaryPath $summaryPath `
        -ExpectedStatus PASS -ExpectedSha256 $broken.Sha256 -ExpectedRowCount 1
    if ($brokenArtifacts.BindingValid -or
        $brokenArtifacts.Diagnostic -ne 'RUST_V2_ACCEPTANCE_RESULT_SUMMARY_TSV_DATA_HASH_INVALID') {
        throw 'footer 数据区 SHA 被破坏时必须拒绝 TSV'
    }

    $malformed = Write-TestResultSummary -Path $summaryPath -MalformedRow
    $malformedArtifacts = Get-ResultSummaryArtifacts -SummaryPath $summaryPath `
        -ExpectedStatus PASS -ExpectedSha256 $malformed.Sha256 -ExpectedRowCount 1
    if ($malformedArtifacts.BindingValid -or
        $malformedArtifacts.Diagnostic -notlike 'RUST_V2_ACCEPTANCE_RESULT_SUMMARY_TSV_ROW_INVALID*') {
        throw 'TSV 行列数错误时必须拒绝摘要'
    }

    $crlf = Write-TestResultSummary -Path $summaryPath -UseCrLf
    $crlfArtifacts = Get-ResultSummaryArtifacts -SummaryPath $summaryPath `
        -ExpectedStatus PASS -ExpectedSha256 $crlf.Sha256 -ExpectedRowCount 1
    if ($crlfArtifacts.BindingValid -or
        $crlfArtifacts.Diagnostic -ne 'RUST_V2_ACCEPTANCE_RESULT_SUMMARY_TSV_FORMAT_INVALID') {
        throw 'TSV 必须固定为 UTF-8 无 BOM 和单字节 LF'
    }

    $wrongName = Join-Path $evidence 'summary.tsv'
    $valid = Write-TestResultSummary -Path $summaryPath
    Copy-Item -LiteralPath $summaryPath -Destination $wrongName
    $wrongNameArtifacts = Get-ResultSummaryArtifacts -SummaryPath $wrongName `
        -ExpectedStatus PASS -ExpectedSha256 $valid.Sha256 -ExpectedRowCount 1
    if ($wrongNameArtifacts.BindingValid -or
        $wrongNameArtifacts.Diagnostic -ne 'RUST_V2_ACCEPTANCE_RESULT_SUMMARY_FILENAME_INVALID') {
        throw '结果摘要必须使用固定 result-summary.tsv 文件名'
    }

    foreach ($status in @('MISSING', 'INCONCLUSIVE')) {
        $statusSummary = Write-TestResultSummary -Path $summaryPath -Status $status
        $statusArtifacts = Get-ResultSummaryArtifacts -SummaryPath $summaryPath `
            -ExpectedStatus $status -ExpectedSha256 $statusSummary.Sha256 -ExpectedRowCount 1
        if (-not $statusArtifacts.BindingValid -or
            ($status -eq 'MISSING' -and $statusArtifacts.MissingCount -ne 1) -or
            ($status -eq 'INCONCLUSIVE' -and $statusArtifacts.InconclusiveCount -ne 1)) {
            throw "TSV 状态计数错误：status=$status artifacts=$($statusArtifacts | ConvertTo-Json -Compress)"
        }
    }

    $valid = Write-TestResultSummary -Path $summaryPath
    $validStdout = $stdout -replace [regex]::Escape($summary.Sha256), $valid.Sha256
    $parsed = Parse-ResultSummaryOutput -Text $validStdout -ExpectedPath $summaryPath
    $harness = New-HarnessResult -Variant A -RunIndex 1 -SourceRevision 'rev' `
        -SourceTreeSha256 ('a' * 64) -PackagePath 'C:\tmp\package.zip' -PackageSha256 ('b' * 64) `
        -ReleaseRoot 'C:\tmp\release' -ConfigSha256 ('c' * 64) -PackageManifestSha256 $null `
        -MediaBeforeSha256 ('d' * 64) -MediaAfterSha256 ('e' * 64) -ResultSummary $parsed `
        -ResultSummaryStatus PASS -RunStatus INCONCLUSIVE `
        -RunDiagnostic 'RUST_V2_ACCEPTANCE_NODE_EXIT_TIMEOUT' -ExporterExitCode 0
    if ($harness.schema_version -ne 2 -or $harness.variant -ne 'A' -or
        $harness.result_summary_status -ne 'PASS' -or $harness.result_summary_path -cne $parsed.Path -or
        $harness.run_status -ne 'INCONCLUSIVE' -or
        $harness.run_diagnostic -ne 'RUST_V2_ACCEPTANCE_NODE_EXIT_TIMEOUT' -or
        $harness.exporter_exit_code -ne 0 -or $null -ne $harness.package_manifest_sha256) {
        throw "harness-result schema2 接线错误：$($harness | ConvertTo-Json -Compress)"
    }

    Write-Output 'RUST_V2_RESULT_SUMMARY_WIRING_PASS'
}
finally {
    if (Test-Path -LiteralPath $fixtureRoot) {
        Remove-Item -LiteralPath $fixtureRoot -Recurse -Force
    }
}
