<#
.SYNOPSIS
验证单轮 harness 的结果摘要固定 stdout、三件套校验与 metadata 接线。
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$measureScript = Join-Path $repositoryRoot 'tests\windows\Measure-RustV2RuntimeAcceptance.ps1'
$fixtureRoot = Join-Path ([IO.Path]::GetTempPath()) ("rust-v2-result-summary-" + [Guid]::NewGuid().ToString('N'))

try {
    if (-not (Test-Path -LiteralPath $measureScript -PathType Leaf)) {
        throw "RUST_V2_RESULT_SUMMARY_HARNESS_MISSING path=$measureScript"
    }
    . $measureScript -LibraryOnly

    $evidence = Join-Path $fixtureRoot 'runs\A-1\evidence'
    New-Item -ItemType Directory -Path $evidence -Force | Out-Null
    $summaryPath = Join-Path $evidence 'result-summary.jsonl'
    $metaPath = Join-Path $evidence 'result-summary-meta.json'
    $leasePath = "$summaryPath.pair.lock"
    # 用真实 LF 写入摘要，避免把 PowerShell 字面量 ``n 当成内容。
    # canonical JSONL 固定使用单字节 LF，避免 Windows 环境隐式写入 CRLF。
    $summaryText = '{"status":"succeeded"}' + [char]10
    [IO.File]::WriteAllText($summaryPath, $summaryText, [Text.UTF8Encoding]::new($false))
    $summarySha256 = (Get-FileHash -LiteralPath $summaryPath -Algorithm SHA256).Hash.ToLowerInvariant()
    $metadata = [ordered]@{
        schema_version = 1
        lease_token = 'fixture-lease-token'
        canonical_sha256 = $summarySha256
        task_id = '00000000-0000-0000-0000-000000000001'
        task_status = 'succeeded'
        status = 'PASS'
        row_count = 1
        missing_count = 0
        inconclusive_count = 0
    }
    $lease = [ordered]@{
        schema_version = 1
        lease_token = 'fixture-lease-token'
        expected_canonical_identity = [ordered]@{ first = 1; second = 2 }
        expected_metadata_identity = [ordered]@{ first = 1; second = 3 }
        expected_canonical_sha256 = $summarySha256
        expected_status = 'PASS'
        expected_row_count = 1
        run_evidence_dir = 'A-1'
    }
    [IO.File]::WriteAllText($metaPath, ($metadata | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
    [IO.File]::WriteAllText($leasePath, ($lease | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))

    $stdout = @(
        "RESULT_SUMMARY_STATUS=PASS"
        "RESULT_SUMMARY_PATH=$summaryPath"
        "RESULT_SUMMARY_SHA256=$summarySha256"
        'RESULT_SUMMARY_ROW_COUNT=1'
        'RESULT_SUMMARY_MISSING_COUNT=0'
        'RESULT_SUMMARY_INCONCLUSIVE_COUNT=0'
        'RESULT_SUMMARY_TASK_ID=00000000-0000-0000-0000-000000000001'
    ) -join "`n"
    $parsed = Parse-ResultSummaryOutput -Text $stdout -ExpectedPath $summaryPath `
        -ExpectedTaskId '00000000-0000-0000-0000-000000000001'
    if ($parsed.Status -ne 'PASS' -or $parsed.Path -ne ([IO.Path]::GetFullPath($summaryPath)) -or
        $parsed.RowCount -ne 1 -or $parsed.MissingCount -ne 0 -or $parsed.InconclusiveCount -ne 0) {
        throw "固定 exporter stdout 解析错误：$($parsed | ConvertTo-Json -Compress)"
    }

    $invalid = $false
    try {
        Parse-ResultSummaryOutput -Text ($stdout -replace 'RESULT_SUMMARY_SHA256=.*', '') `
            -ExpectedPath $summaryPath -ExpectedTaskId '00000000-0000-0000-0000-000000000001' | Out-Null
    }
    catch {
        $invalid = $_.Exception.Message -like '*RUST_V2_RESULT_SUMMARY_FIELD_MISSING*'
    }
    if (-not $invalid) {
        throw '缺少固定 stdout 字段必须被拒绝'
    }

    $artifacts = Get-ResultSummaryArtifacts -SummaryPath $summaryPath `
        -ExpectedTaskId $parsed.TaskId -ExpectedStatus $parsed.Status `
        -ExpectedSha256 $parsed.Sha256 -ExpectedRowCount $parsed.RowCount
    if (-not $artifacts.SummaryExists -or -not $artifacts.MetadataExists -or -not $artifacts.LeaseExists -or
        -not $artifacts.BindingValid) {
        throw "结果摘要三件套校验错误：$($artifacts | ConvertTo-Json -Compress)"
    }
    $summaryBytes = [IO.File]::ReadAllBytes($summaryPath)
    if ([Array]::IndexOf($summaryBytes, [byte]13) -ge 0 -or $summaryBytes.Length -eq 0 -or
        $summaryBytes[$summaryBytes.Length - 1] -ne [byte]10) {
        throw 'canonical JSONL 必须是 UTF-8 无 BOM、每行单字节 LF 且不得含 CR'
    }

    $lease.lease_token = 'tampered-lease-token'
    [IO.File]::WriteAllText($leasePath, ($lease | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
    $tamperedLease = Get-ResultSummaryArtifacts -SummaryPath $summaryPath `
        -ExpectedTaskId $parsed.TaskId -ExpectedStatus $parsed.Status `
        -ExpectedSha256 $parsed.Sha256 -ExpectedRowCount $parsed.RowCount
    if ($tamperedLease.BindingValid) {
        throw 'lease token 被篡改时必须拒绝三件套绑定'
    }
    $lease.lease_token = 'fixture-lease-token'
    [IO.File]::WriteAllText($leasePath, ($lease | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))

    $metadata.canonical_sha256 = ('f' * 64)
    [IO.File]::WriteAllText($metaPath, ($metadata | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
    $tamperedMetadata = Get-ResultSummaryArtifacts -SummaryPath $summaryPath `
        -ExpectedTaskId $parsed.TaskId -ExpectedStatus $parsed.Status `
        -ExpectedSha256 $parsed.Sha256 -ExpectedRowCount $parsed.RowCount
    if ($tamperedMetadata.BindingValid) {
        throw 'metadata canonical SHA 被篡改时必须拒绝三件套绑定'
    }
    $metadata.canonical_sha256 = $summarySha256
    [IO.File]::WriteAllText($metaPath, ($metadata | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))

    $metadata.task_id = 'tampered-task-id'
    [IO.File]::WriteAllText($metaPath, ($metadata | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
    $tamperedMetadataTask = Get-ResultSummaryArtifacts -SummaryPath $summaryPath `
        -ExpectedTaskId $parsed.TaskId -ExpectedStatus $parsed.Status `
        -ExpectedSha256 $parsed.Sha256 -ExpectedRowCount $parsed.RowCount
    if ($tamperedMetadataTask.BindingValid) {
        throw 'metadata task_id 被篡改时必须拒绝三件套绑定'
    }
    $metadata.task_id = $parsed.TaskId
    [IO.File]::WriteAllText($metaPath, ($metadata | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))

    # MISSING 摘要允许零行 canonical；空文件的 SHA/row_count 仍必须与 metadata/lease 绑定。
    [IO.File]::WriteAllBytes($summaryPath, [byte[]]@())
    $emptySha256 = (Get-FileHash -LiteralPath $summaryPath -Algorithm SHA256).Hash.ToLowerInvariant()
    $metadata.canonical_sha256 = $emptySha256
    $metadata.status = 'MISSING'
    $metadata.row_count = 0
    $lease.expected_canonical_sha256 = $emptySha256
    $lease.expected_status = 'MISSING'
    $lease.expected_row_count = 0
    [IO.File]::WriteAllText($metaPath, ($metadata | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
    [IO.File]::WriteAllText($leasePath, ($lease | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
    $emptySummary = Get-ResultSummaryArtifacts -SummaryPath $summaryPath `
        -ExpectedTaskId $parsed.TaskId -ExpectedStatus 'MISSING' `
        -ExpectedSha256 $emptySha256 -ExpectedRowCount 0
    if (-not $emptySummary.BindingValid) {
        throw "零行 MISSING canonical 仍必须通过三件套绑定：$($emptySummary | ConvertTo-Json -Compress)"
    }
    [IO.File]::WriteAllText($summaryPath, $summaryText, [Text.UTF8Encoding]::new($false))
    $metadata.canonical_sha256 = $summarySha256
    $metadata.status = 'PASS'
    $metadata.row_count = 1
    $lease.expected_canonical_sha256 = $summarySha256
    $lease.expected_status = 'PASS'
    $lease.expected_row_count = 1
    [IO.File]::WriteAllText($metaPath, ($metadata | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
    [IO.File]::WriteAllText($leasePath, ($lease | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))

    # PASS 不能借零行空 canonical 伪造成功；三件套即使完全自洽也必须被拒绝。
    [IO.File]::WriteAllBytes($summaryPath, [byte[]]@())
    $metadata.canonical_sha256 = $emptySha256
    $metadata.status = 'PASS'
    $metadata.row_count = 0
    $lease.expected_canonical_sha256 = $emptySha256
    $lease.expected_status = 'PASS'
    $lease.expected_row_count = 0
    [IO.File]::WriteAllText($metaPath, ($metadata | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
    [IO.File]::WriteAllText($leasePath, ($lease | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
    $emptyPass = Get-ResultSummaryArtifacts -SummaryPath $summaryPath `
        -ExpectedTaskId $parsed.TaskId -ExpectedStatus 'PASS' `
        -ExpectedSha256 $emptySha256 -ExpectedRowCount 0
    if ($emptyPass.BindingValid) {
        throw 'PASS + row_count=0 + 空 canonical 必须拒绝三件套绑定'
    }
    # INCONCLUSIVE 也不能用零行空 canonical 获得绑定；只有 MISSING 允许空结果。
    $metadata.status = 'INCONCLUSIVE'
    $lease.expected_status = 'INCONCLUSIVE'
    [IO.File]::WriteAllText($metaPath, ($metadata | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
    [IO.File]::WriteAllText($leasePath, ($lease | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
    $emptyInconclusive = Get-ResultSummaryArtifacts -SummaryPath $summaryPath `
        -ExpectedTaskId $parsed.TaskId -ExpectedStatus 'INCONCLUSIVE' `
        -ExpectedSha256 $emptySha256 -ExpectedRowCount 0
    if ($emptyInconclusive.BindingValid) {
        throw 'INCONCLUSIVE + row_count=0 + 空 canonical 必须拒绝三件套绑定'
    }
    [IO.File]::WriteAllText($summaryPath, $summaryText, [Text.UTF8Encoding]::new($false))
    $metadata.canonical_sha256 = $summarySha256
    $metadata.status = 'PASS'
    $metadata.row_count = 1
    $lease.expected_canonical_sha256 = $summarySha256
    $lease.expected_status = 'PASS'
    $lease.expected_row_count = 1
    [IO.File]::WriteAllText($metaPath, ($metadata | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
    [IO.File]::WriteAllText($leasePath, ($lease | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))

    $invalidJsonText = '{not-json}' + [char]10
    [IO.File]::WriteAllText($summaryPath, $invalidJsonText, [Text.UTF8Encoding]::new($false))
    $invalidJsonSha256 = (Get-FileHash -LiteralPath $summaryPath -Algorithm SHA256).Hash.ToLowerInvariant()
    $metadata.canonical_sha256 = $invalidJsonSha256
    $lease.expected_canonical_sha256 = $invalidJsonSha256
    [IO.File]::WriteAllText($metaPath, ($metadata | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
    [IO.File]::WriteAllText($leasePath, ($lease | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
    $invalidJson = Get-ResultSummaryArtifacts -SummaryPath $summaryPath `
        -ExpectedTaskId $parsed.TaskId -ExpectedStatus $parsed.Status `
        -ExpectedSha256 $invalidJsonSha256 -ExpectedRowCount $parsed.RowCount
    if ($invalidJson.BindingValid) {
        throw 'canonical JSONL 非法 JSON 时必须拒绝三件套绑定'
    }

    $harness = New-HarnessResult -Variant A -RunIndex 1 -SourceRevision 'rev' `
        -SourceTreeSha256 ('a' * 64) -PackagePath 'C:\tmp\package.zip' -PackageSha256 ('b' * 64) `
        -ReleaseRoot 'C:\tmp\release' -ConfigSha256 ('c' * 64) -PackageManifestSha256 $null `
        -MediaBeforeSha256 ('d' * 64) -MediaAfterSha256 ('e' * 64) -ResultSummary $parsed `
        -ResultSummaryStatus 'PASS' -RunStatus 'INCONCLUSIVE' `
        -RunDiagnostic 'RUST_V2_ACCEPTANCE_NODE_EXIT_TIMEOUT' -ExporterExitCode 0
    if ($harness.schema_version -ne 2 -or $harness.variant -ne 'A' -or
        $harness.result_summary_status -ne 'PASS' -or $harness.run_status -ne 'INCONCLUSIVE' -or
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
