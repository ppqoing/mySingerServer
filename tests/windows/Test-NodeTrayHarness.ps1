[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repo = [IO.Path]::GetFullPath(
    (Split-Path -Parent (Split-Path -Parent $PSScriptRoot))
).TrimEnd('\')
$testScript = Join-Path $repo 'tests\windows\Test-NodeTray.ps1'
$measureScript = Join-Path $repo 'tests\windows\Measure-NodeTrayResources.ps1'
$failures = [Collections.Generic.List[string]]::new()
$guidLeaf = '11111111111111111111111111111111'
$testRoot = "C:\tmp\mysingerserver-node-tray-$guidLeaf"
$missingStage = "C:\tmp\mysingerserver-nodetray-stage-$guidLeaf"
$workspaceStage = Join-Path $repo '.tmp\nodetray-stage-contract'
$resourceOut = Join-Path $testRoot 'resources.json'

function Add-Failure {
    param([string]$Code)
    $failures.Add($Code)
}

function Assert-True {
    param([bool]$Condition, [string]$Code)
    if (-not $Condition) { Add-Failure $Code }
}

function Invoke-JsonScript {
    param([string]$Path, [string[]]$Arguments)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        return [pscustomobject]@{
            ExitCode = 127
            Document = $null
            Raw = "SCRIPT_MISSING path=$Path"
        }
    }
    $pwsh = (Get-Process -Id $PID).Path
    $rawLines = @(& $pwsh -NoLogo -NoProfile -File $Path @Arguments 2>&1)
    $exitCode = $LASTEXITCODE
    $raw = ($rawLines | ForEach-Object { [string]$_ }) -join [Environment]::NewLine
    $document = $null
    try {
        $document = $raw | ConvertFrom-Json -Depth 24
    } catch {
        Add-Failure ("INVALID_JSON path={0} exit={1}" -f $Path, $exitCode)
    }
    return [pscustomobject]@{
        ExitCode = $exitCode
        Document = $document
        Raw = $raw
    }
}

Assert-True (-not (Test-Path -LiteralPath $testRoot)) `
    'TEST_PRECONDITION_ROOT_ALREADY_EXISTS'
Assert-True (-not (Test-Path -LiteralPath $missingStage)) `
    'TEST_PRECONDITION_STAGE_ALREADY_EXISTS'

$whatIf = Invoke-JsonScript $testScript @(
    '-WhatIf',
    '-StageDir', $missingStage,
    '-TestRoot', $testRoot,
    '-CentralTestPort', '39281'
)
Assert-True ($whatIf.ExitCode -eq 0) 'WHATIF_EXIT_NOT_ZERO'
if ($null -ne $whatIf.Document) {
    Assert-True ($whatIf.Document.mode -eq 'what-if-read-only') `
        'WHATIF_MODE_INCORRECT'
    Assert-True ($whatIf.Document.preflight_status -eq 'FAIL') `
        'WHATIF_MISSING_STAGE_FALSE_PASS'
    Assert-True ($whatIf.Document.dynamic_readiness -eq 'BLOCKED_NOT_RUN_DYNAMIC') `
        'WHATIF_MISSING_STAGE_NOT_BLOCKED'
    Assert-True (@($whatIf.Document.blockers).Count -ge 2) `
        'WHATIF_MISSING_STAGE_BLOCKERS_MISSING'
    Assert-True (-not [bool]$whatIf.Document.stage.files.nodetray.exists) `
        'WHATIF_NODETRAY_FALSE_PRESENT'
    Assert-True (-not [bool]$whatIf.Document.stage.files.webview2_bootstrapper.exists) `
        'WHATIF_WEBVIEW2_FALSE_PRESENT'
    Assert-True ($whatIf.Document.test_root.path -eq $testRoot) `
        'WHATIF_TEST_ROOT_CHANGED'
    Assert-True ($whatIf.Document.fixed_task.path -eq '\MySingerServer\DeleteHelper') `
        'WHATIF_FIXED_TASK_PATH_CHANGED'
    Assert-True ($null -ne $whatIf.Document.central_tcp_port.available) `
        'WHATIF_TCP_AVAILABILITY_MISSING'
    Assert-True ($null -ne $whatIf.Document.current_user.interactive) `
        'WHATIF_CURRENT_USER_ASSESSMENT_MISSING'
    $executorCapabilityProperty = $whatIf.Document.PSObject.Properties[
        'executor_capability']
    $executorInvokedProperty = $whatIf.Document.PSObject.Properties[
        'executor_invoked']
    Assert-True ($null -ne $executorCapabilityProperty -and
        $executorCapabilityProperty.Value -eq `
            'BLOCKED_IMPLEMENTATION_DEPENDENCY') `
        'WHATIF_EXECUTOR_CAPABILITY_FIELD_MISSING'
    Assert-True ($null -ne $executorInvokedProperty -and
        [bool]$executorInvokedProperty.Value -eq $false) `
        'WHATIF_EXECUTOR_INVOKED_FALSE_MISSING'
    $backendExecutorProperty = $whatIf.Document.PSObject.Properties[
        'backend_executor']
    Assert-True ($null -ne $backendExecutorProperty) `
        'WHATIF_BACKEND_EXECUTOR_ASSESSMENT_MISSING'
    if ($null -ne $backendExecutorProperty) {
        Assert-True ([bool]$backendExecutorProperty.Value.repository_owned) `
            'WHATIF_BACKEND_EXECUTOR_NOT_REPOSITORY_OWNED'
        Assert-True ($backendExecutorProperty.Value.capability -eq `
            'BLOCKED_IMPLEMENTATION_DEPENDENCY') `
            'WHATIF_BACKEND_EXECUTOR_DEPENDENCY_NOT_EXPLICIT'
        Assert-True (@($backendExecutorProperty.Value.blockers).Count -ge 3) `
            'WHATIF_BACKEND_EXECUTOR_CODE_EVIDENCE_MISSING'
    }
}
Assert-True (-not (Test-Path -LiteralPath $testRoot)) `
    'WHATIF_CREATED_TEST_ROOT'
Assert-True (-not (Test-Path -LiteralPath $missingStage)) `
    'WHATIF_CREATED_STAGE_ROOT'

$workspaceWhatIf = Invoke-JsonScript $testScript @(
    '-WhatIf',
    '-StageDir', $workspaceStage,
    '-TestRoot', $testRoot,
    '-CentralTestPort', '39281'
)
Assert-True ($workspaceWhatIf.ExitCode -eq 0) `
    'WORKSPACE_STAGE_WHATIF_EXIT_NOT_ZERO'
if ($workspaceWhatIf.ExitCode -eq 0 -and $null -ne $workspaceWhatIf.Document) {
    Assert-True ($workspaceWhatIf.Document.stage.path -eq $workspaceStage) `
        'WORKSPACE_STAGE_PATH_CHANGED'
    Assert-True ($workspaceWhatIf.Document.preflight_status -eq 'FAIL') `
        'WORKSPACE_MISSING_STAGE_FALSE_PASS'
}
Assert-True (-not (Test-Path -LiteralPath $workspaceStage)) `
    'WORKSPACE_WHATIF_CREATED_STAGE_ROOT'

$invalidRoot = Invoke-JsonScript $testScript @(
    '-WhatIf',
    '-StageDir', $missingStage,
    '-TestRoot', 'C:\tmp\mysingerserver-node-tray-not-a-guid'
)
Assert-True ($invalidRoot.ExitCode -eq 1) 'INVALID_ROOT_EXIT_NOT_ONE'
if ($null -ne $invalidRoot.Document) {
    Assert-True ($invalidRoot.Document.status -eq 'FAIL') `
        'INVALID_ROOT_NOT_FAIL'
    Assert-True ($invalidRoot.Document.error_code -eq 'TEST_ROOT_NAME_NOT_GUID_SCOPED') `
        'INVALID_ROOT_ERROR_NOT_FAIL_CLOSED'
}

$dynamic = Invoke-JsonScript $testScript @(
    '-StageDir', $missingStage,
    '-TestRoot', $testRoot,
    '-CentralTestPort', '39281'
)
Assert-True ($dynamic.ExitCode -eq 2) 'UNAUTHORIZED_DYNAMIC_EXIT_NOT_TWO'
if ($null -ne $dynamic.Document) {
    Assert-True ($dynamic.Document.status -eq 'BLOCKED_NOT_RUN_DYNAMIC') `
        'UNAUTHORIZED_DYNAMIC_NOT_BLOCKED'
    Assert-True (@($dynamic.Document.scenarios).Count -eq 10) `
        'DYNAMIC_SCENARIO_COUNT_INCORRECT'
    Assert-True ([int]$dynamic.Document.summary.pass -eq 0) `
        'BLOCKED_DYNAMIC_COUNTED_AS_PASS'
    Assert-True ([int]$dynamic.Document.summary.fail -eq 0) `
        'UNRUN_DYNAMIC_COUNTED_AS_FAIL'
    Assert-True ([int]$dynamic.Document.summary.blocked -eq 10) `
        'DYNAMIC_BLOCKED_COUNT_INCORRECT'
    $required = @($dynamic.Document.scenarios.required_authorization | ForEach-Object { @($_) })
    foreach ($switchName in @(
        'AllowProcessControl',
        'AllowUAC',
        'AllowTaskScheduler',
        'AllowHKCUStartup'
    )) {
        Assert-True ($required -contains $switchName) `
            ("AUTHORIZATION_MATRIX_MISSING switch={0}" -f $switchName)
    }
    Assert-True (@($dynamic.Document.scenarios | Where-Object {
        $_.status -ne 'BLOCKED_NOT_RUN_DYNAMIC' -or $_.modified -ne $false
    }).Count -eq 0) 'UNAUTHORIZED_DYNAMIC_MUTATED_OR_UNBLOCKED'
}
Assert-True (-not (Test-Path -LiteralPath $testRoot)) `
    'UNAUTHORIZED_DYNAMIC_CREATED_TEST_ROOT'

$resource = Invoke-JsonScript $measureScript @(
    '-NodeTrayExe', (Join-Path $missingStage 'nodetray.exe'),
    '-WarmupSec', '120',
    '-DurationSec', '300',
    '-OutFile', $resourceOut
)
Assert-True ($resource.ExitCode -eq 2) 'RESOURCE_UNAUTHORIZED_EXIT_NOT_TWO'
if ($null -ne $resource.Document) {
    Assert-True ($resource.Document.status -eq 'BLOCKED_NOT_RUN_DYNAMIC') `
        'RESOURCE_UNAUTHORIZED_NOT_BLOCKED'
    Assert-True ([bool]$resource.Document.authorization.process_control -eq $false) `
        'RESOURCE_PROCESS_CONTROL_FALSE_NOT_RECORDED'
    Assert-True ([int]$resource.Document.thresholds.private_working_set_mib_max -eq 256) `
        'RESOURCE_MEMORY_THRESHOLD_INCORRECT'
    Assert-True ([double]$resource.Document.thresholds.average_single_core_cpu_percent_max_exclusive -eq 1.0) `
        'RESOURCE_CPU_THRESHOLD_INCORRECT'
    Assert-True (@($resource.Document.raw_samples).Count -eq 0) `
        'RESOURCE_BLOCKED_HAS_FAKE_SAMPLES'
}
Assert-True (-not (Test-Path -LiteralPath $resourceOut)) `
    'RESOURCE_BLOCKED_WROTE_OUTPUT'
Assert-True (-not (Test-Path -LiteralPath $testRoot)) `
    'RESOURCE_BLOCKED_CREATED_TEST_ROOT'

$evidenceGuid = '55555555555555555555555555555555'
$evidenceRoot = "C:\tmp\mysingerserver-node-tray-$evidenceGuid"
$evidenceStage = "C:\tmp\mysingerserver-nodetray-stage-$evidenceGuid"
$fixtureRoot = Join-Path $repo ".tmp\task11-contract-$evidenceGuid"
try {
    Assert-True (-not (Test-Path -LiteralPath $evidenceRoot)) `
        'EVIDENCE_TEST_ROOT_ALREADY_EXISTS'
    Assert-True (-not (Test-Path -LiteralPath $evidenceStage)) `
        'EVIDENCE_TEST_STAGE_ALREADY_EXISTS'
    Assert-True (-not (Test-Path -LiteralPath $fixtureRoot)) `
        'EVIDENCE_FIXTURE_ROOT_ALREADY_EXISTS'
    New-Item -ItemType Directory -Path $fixtureRoot -Force | Out-Null
    $stageFiles = [ordered]@{}
    foreach ($name in @(
        'nodetray.exe',
        'MicrosoftEdgeWebview2Setup.exe',
        'agent.exe',
        'worker.exe',
        'helper.exe'
    )) {
        $stageFiles[$name] = [Convert]::ToHexString(
            [Security.Cryptography.SHA256]::HashData(
                [Text.Encoding]::UTF8.GetBytes("fixture-$name")
            )
        ).ToLowerInvariant()
    }
    $sid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    $sidHash = [Convert]::ToHexString(
        [Security.Cryptography.SHA256]::HashData(
            [Text.Encoding]::UTF8.GetBytes($sid)
        )
    ).ToLowerInvariant()
    $scenarioEvidence = @()
    for ($id = 1; $id -le 10; $id++) {
        $path = Join-Path $evidenceRoot ("evidence\scenario-{0}.json" -f $id)
        $scenarioEvidence += [ordered]@{
            id = $id
            status = 'PASS'
            started_utc = '2026-08-03T00:00:00Z'
            ended_utc = '2026-08-03T00:00:01Z'
            command = ("authorized-scenario-{0} [REDACTED]" -f $id)
            exit_code = 0
            modified = $false
            restored = $false
            evidence_files = @([ordered]@{
                path = $path
                sha256 = [Convert]::ToHexString(
                    [Security.Cryptography.SHA256]::HashData(
                        [Text.Encoding]::UTF8.GetBytes("scenario-$id")
                    )
                ).ToLowerInvariant()
            })
        }
    }
    $scanPath = Join-Path $evidenceRoot 'evidence\credential-scan.json'
    $scanHash = [Convert]::ToHexString(
        [Security.Cryptography.SHA256]::HashData(
            [Text.Encoding]::UTF8.GetBytes('credential-scan')
        )
    ).ToLowerInvariant()
    $dynamicEvidencePath = Join-Path $fixtureRoot 'dynamic-evidence.json'
    [ordered]@{
        schema_version = 1
        run_id = $evidenceGuid
        test_root = $evidenceRoot
        stage_dir = $evidenceStage
        current_user_sid_sha256 = $sidHash
        authorizations = [ordered]@{
            process_control = $true
            uac = $true
            task_scheduler = $true
            hkcu_startup = $true
        }
        stage_files = $stageFiles
        credential_scan_status = 'PASS'
        credential_scan_evidence = [ordered]@{
            path = $scanPath
            sha256 = $scanHash
        }
        scenarios = $scenarioEvidence
    } | ConvertTo-Json -Depth 10 |
        Set-Content -LiteralPath $dynamicEvidencePath -Encoding utf8NoBOM

    $evidenceValidation = Invoke-JsonScript $testScript @(
        '-WhatIf',
        '-ValidateEvidenceOnly',
        '-DynamicEvidenceFile', $dynamicEvidencePath,
        '-StageDir', $evidenceStage,
        '-TestRoot', $evidenceRoot,
        '-CentralTestPort', '39281'
    )
    Assert-True ($evidenceValidation.ExitCode -eq 0) `
        'EVIDENCE_VALIDATION_EXIT_NOT_ZERO'
    if ($evidenceValidation.ExitCode -eq 0 -and
        $null -ne $evidenceValidation.Document) {
        Assert-True ($evidenceValidation.Document.evidence_validation_status -eq 'PASS') `
            'EVIDENCE_VALIDATION_NOT_PASS'
        Assert-True ($evidenceValidation.Document.would_summarize_status -eq 'PASS') `
            'EVIDENCE_SUMMARY_CANNOT_REACH_PASS'
        Assert-True ($evidenceValidation.Document.dynamic_acceptance -eq `
            'BLOCKED_NOT_RUN_DYNAMIC') 'EVIDENCE_VALIDATION_FALSE_DYNAMIC_PASS'
    }
    $invalidEvidencePath = Join-Path $fixtureRoot 'dynamic-evidence-invalid.json'
    $invalidEvidence = Get-Content -Raw -LiteralPath $dynamicEvidencePath |
        ConvertFrom-Json -Depth 24
    $invalidEvidence.run_id = '00000000000000000000000000000000'
    $invalidEvidence | ConvertTo-Json -Depth 24 |
        Set-Content -LiteralPath $invalidEvidencePath -Encoding utf8NoBOM
    $invalidEvidenceValidation = Invoke-JsonScript $testScript @(
        '-WhatIf',
        '-ValidateEvidenceOnly',
        '-DynamicEvidenceFile', $invalidEvidencePath,
        '-StageDir', $evidenceStage,
        '-TestRoot', $evidenceRoot,
        '-CentralTestPort', '39281'
    )
    Assert-True ($invalidEvidenceValidation.ExitCode -eq 1) `
        'EVIDENCE_BINDING_MISMATCH_NOT_REJECTED'
    if ($null -ne $invalidEvidenceValidation.Document) {
        Assert-True ($invalidEvidenceValidation.Document.dynamic_acceptance -eq `
            'BLOCKED_NOT_RUN_DYNAMIC') 'INVALID_EVIDENCE_FALSE_DYNAMIC_PASS'
    }

    $samplesPath = Join-Path $fixtureRoot 'resource-samples.json'
    @(
        [ordered]@{ elapsed_seconds=0.0; private_working_set_bytes=100MB; cpu_total_seconds=10.000; handles=100 },
        [ordered]@{ elapsed_seconds=1.0; private_working_set_bytes=110MB; cpu_total_seconds=10.005; handles=102 },
        [ordered]@{ elapsed_seconds=2.0; private_working_set_bytes=120MB; cpu_total_seconds=10.010; handles=104 }
    ) | ConvertTo-Json -Depth 5 |
        Set-Content -LiteralPath $samplesPath -Encoding utf8NoBOM
    $offlineOut = Join-Path $evidenceRoot 'resource-result.json'
    $resourceWithoutThroughput = Invoke-JsonScript $measureScript @(
        '-NodeTrayExe', (Join-Path $evidenceStage 'nodetray.exe'),
        '-WarmupSec', '120',
        '-DurationSec', '300',
        '-OutFile', $offlineOut,
        '-ValidateResultOnly',
        '-SamplesFile', $samplesPath
    )
    Assert-True ($resourceWithoutThroughput.ExitCode -eq 0) `
        'RESOURCE_OFFLINE_VALIDATION_EXIT_NOT_ZERO'
    if ($resourceWithoutThroughput.ExitCode -eq 0 -and
        $null -ne $resourceWithoutThroughput.Document) {
        Assert-True ($resourceWithoutThroughput.Document.resource_status -eq 'PASS') `
            'RESOURCE_OFFLINE_RESOURCE_NOT_PASS'
        Assert-True ($resourceWithoutThroughput.Document.throughput_impact -eq `
            'BLOCKED_NOT_RUN_DYNAMIC') 'RESOURCE_OFFLINE_THROUGHPUT_NOT_BLOCKED'
        Assert-True ($resourceWithoutThroughput.Document.would_summarize_status -eq `
            'BLOCKED_NOT_RUN_DYNAMIC') 'RESOURCE_OFFLINE_FALSE_OVERALL_PASS'
    }
    Assert-True (-not (Test-Path -LiteralPath $offlineOut)) `
        'RESOURCE_OFFLINE_VALIDATION_WROTE_OUTPUT'

    $throughputPath = Join-Path $fixtureRoot 'throughput.json'
    [ordered]@{
        schema_version = 1
        run_id = $evidenceGuid
        test_root = $evidenceRoot
        stage_dir = $evidenceStage
        current_user_sid_sha256 = $sidHash
        baseline_items_per_second = 100.0
        with_tray_items_per_second = 99.0
        regression_percent = 1.0
        maximum_regression_percent = 5.0
        status = 'PASS'
        started_utc = '2026-08-03T00:00:00Z'
        ended_utc = '2026-08-03T00:05:00Z'
        command = 'authorized-throughput-benchmark [REDACTED]'
        credential_scan_status = 'PASS'
    } | ConvertTo-Json -Depth 6 |
        Set-Content -LiteralPath $throughputPath -Encoding utf8NoBOM
    $resourceWithThroughput = Invoke-JsonScript $measureScript @(
        '-NodeTrayExe', (Join-Path $evidenceStage 'nodetray.exe'),
        '-WarmupSec', '120',
        '-DurationSec', '300',
        '-OutFile', $offlineOut,
        '-ValidateResultOnly',
        '-SamplesFile', $samplesPath,
        '-ThroughputEvidenceFile', $throughputPath
    )
    Assert-True ($resourceWithThroughput.ExitCode -eq 0) `
        'RESOURCE_THROUGHPUT_VALIDATION_EXIT_NOT_ZERO'
    if ($resourceWithThroughput.ExitCode -eq 0 -and
        $null -ne $resourceWithThroughput.Document) {
        Assert-True ($resourceWithThroughput.Document.throughput_impact -eq 'PASS') `
            'RESOURCE_VALID_THROUGHPUT_NOT_PASS'
        Assert-True ($resourceWithThroughput.Document.would_summarize_status -eq 'PASS') `
            'RESOURCE_VALID_THROUGHPUT_CANNOT_REACH_PASS'
        Assert-True ($resourceWithThroughput.Document.dynamic_acceptance -eq `
            'BLOCKED_NOT_RUN_DYNAMIC') 'RESOURCE_VALIDATION_FALSE_DYNAMIC_PASS'
    }
    $invalidThroughputPath = Join-Path $fixtureRoot 'throughput-invalid.json'
    $invalidThroughput = Get-Content -Raw -LiteralPath $throughputPath |
        ConvertFrom-Json -Depth 12
    $invalidThroughput.credential_scan_status = 'FAIL'
    $invalidThroughput | ConvertTo-Json -Depth 12 |
        Set-Content -LiteralPath $invalidThroughputPath -Encoding utf8NoBOM
    $invalidThroughputValidation = Invoke-JsonScript $measureScript @(
        '-NodeTrayExe', (Join-Path $evidenceStage 'nodetray.exe'),
        '-WarmupSec', '120',
        '-DurationSec', '300',
        '-OutFile', $offlineOut,
        '-ValidateResultOnly',
        '-SamplesFile', $samplesPath,
        '-ThroughputEvidenceFile', $invalidThroughputPath
    )
    Assert-True ($invalidThroughputValidation.ExitCode -eq 1) `
        'INVALID_THROUGHPUT_NOT_REJECTED'
    if ($null -ne $invalidThroughputValidation.Document) {
        Assert-True ($invalidThroughputValidation.Document.dynamic_acceptance -eq `
            'BLOCKED_NOT_RUN_DYNAMIC') 'INVALID_THROUGHPUT_FALSE_DYNAMIC_PASS'
    }
} finally {
    $resolvedOwned = [IO.Path]::GetFullPath($fixtureRoot).TrimEnd('\')
    $fixtureParent = [IO.Path]::GetFullPath((Join-Path $repo '.tmp')).TrimEnd('\')
    if ($resolvedOwned.StartsWith("$fixtureParent\", `
            [StringComparison]::OrdinalIgnoreCase) -and
        (Split-Path -Leaf $resolvedOwned) -eq `
            "task11-contract-$evidenceGuid" -and
        (Test-Path -LiteralPath $resolvedOwned)) {
        $fixtureItem = Get-Item -LiteralPath $resolvedOwned -Force
        if (($fixtureItem.Attributes -band `
                [IO.FileAttributes]::ReparsePoint) -eq 0) {
            Remove-Item -LiteralPath $resolvedOwned -Recurse -Force
        }
    }
    Assert-True (-not (Test-Path -LiteralPath $evidenceRoot)) `
        'EVIDENCE_VALIDATION_CREATED_LOGICAL_TEST_ROOT'
    Assert-True (-not (Test-Path -LiteralPath $evidenceStage)) `
        'EVIDENCE_VALIDATION_CREATED_LOGICAL_STAGE'
}

if ($failures.Count -gt 0) {
    $failures | Sort-Object -Unique | ForEach-Object {
        Write-Error $_ -ErrorAction Continue
    }
    throw ("NODETRAY_HARNESS_CONTRACT_FAILED count={0}" -f `
        @($failures | Sort-Object -Unique).Count)
}

Write-Host 'NODETRAY_HARNESS_CONTRACT_PASS'
