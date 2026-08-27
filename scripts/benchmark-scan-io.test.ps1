[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$benchmark = Join-Path $PSScriptRoot 'benchmark-scan-io.ps1'

function Assert-True {
    param([bool]$Condition, [string]$Message)
    if (-not $Condition) { throw "ASSERTION_FAILED: $Message" }
}

function Write-Utf8NoBom {
    param([string]$Path, [string]$Value)
    [IO.File]::WriteAllText($Path, $Value, [Text.UTF8Encoding]::new($false))
}

if (-not (Test-Path -LiteralPath $benchmark -PathType Leaf)) {
    throw 'BENCHMARK_SCAN_IO_SCRIPT_MISSING'
}

$fixture = Join-Path ([IO.Path]::GetTempPath()) ('mysinger-benchmark-scan-io-' + [Guid]::NewGuid().ToString('N'))
$root = Join-Path $fixture 'readonly-media'
$baseline = Join-Path $fixture 'io-baseline'
$adaptive = Join-Path $fixture 'io-adaptive'
$blocked = Join-Path $fixture 'blocked'
$runner = Join-Path $fixture 'fixture-runner.ps1'
$failingRunner = Join-Path $fixture 'failing-runner.ps1'

try {
    New-Item -ItemType Directory -Force -Path $root | Out-Null
    Write-Utf8NoBom -Path (Join-Path $root 'source.mp4') -Value 'read-only-fixture'
    Write-Utf8NoBom -Path $runner -Value @'
param(
    [string]$Root,
    [string]$Mode,
    [uint32]$FieldsMask,
    [int]$Workers,
    [string]$OutputPath,
    [string]$TracePath,
    [string]$LifecyclePath,
    [string]$ResourcePath
)
if ($Mode -eq 'Baseline') { Start-Sleep -Milliseconds 300 } else { Start-Sleep -Milliseconds 40 }
$result = [ordered]@{
    schema_version = 1
    root = $Root
    mode = $Mode
    fields_mask = $FieldsMask
    workers = $Workers
    files = @(
        [ordered]@{ path = 'clip-h264.mp4'; sha256 = ('a' * 64); image_feature = 'image-a'; six_frame_feature = 'frames-a' },
        [ordered]@{ path = 'clip-hevc.mkv'; sha256 = ('b' * 64); image_feature = 'image-b'; six_frame_feature = 'frames-b' }
    )
    failures = @()
}
$result | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $OutputPath -Encoding utf8NoBOM
@([ordered]@{ disk_key = 'fixture:disk0'; concurrency = 2; effective_read_bps = 1048576; lease_wait_ms = 4 }) |
    ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $TracePath -Encoding utf8NoBOM
[ordered]@{ pause = 'passed'; resume = 'passed'; stop = 'passed'; inflight_drained = $true; progress_not_ahead = $true } |
    ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $LifecyclePath -Encoding utf8NoBOM
@([ordered]@{ cpu_percent = 12.5; disk_read_bps = 1048576 }) |
    ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $ResourcePath -Encoding utf8NoBOM
'fixture runner completed'
'fixture runner diagnostic' | Write-Error -ErrorAction Continue
'@
    Write-Utf8NoBom -Path $failingRunner -Value @'
param([string]$Root, [string]$Mode, [uint32]$FieldsMask, [int]$Workers, [string]$OutputPath, [string]$TracePath, [string]$LifecyclePath, [string]$ResourcePath)
throw 'fixture scan failed'
'@

    $wrongRootRejected = $false
    try {
        & $benchmark -Root $root -Mode Baseline -OutputDir (Join-Path $fixture 'wrong-root') `
            -RunnerPath $runner -MinimumDFreeBytes 0
    } catch {
        $wrongRootRejected = $_.Exception.Message -match 'BENCHMARK_ROOT_NOT_APPROVED'
    }
    Assert-True $wrongRootRejected 'production mode accepted a root other than I:\MiddleDir\11111111'

    & $benchmark -Root $root -Mode Baseline -OutputDir $blocked -RunnerPath $runner `
        -MinimumDFreeBytes ([long]::MaxValue) -AllowFixtureRoot | Out-Null
    $blockedSummary = Get-Content -Raw -LiteralPath (Join-Path $blocked 'benchmark-summary.json') | ConvertFrom-Json
    Assert-True ($blockedSummary.status -ceq 'BLOCKED') 'low D free space did not produce BLOCKED'
    Assert-True (-not (Test-Path -LiteralPath (Join-Path $blocked 'scan-result.json'))) 'runner executed after D space BLOCKED'
    Assert-True (@(Get-ChildItem -LiteralPath $root -Force).Count -eq 1) 'source data changed during BLOCKED preflight'

    & $benchmark -Root $root -Mode Baseline -OutputDir $baseline -RunnerPath $runner `
        -MinimumDFreeBytes 0 -AllowFixtureRoot -FieldsMask 2043 -Workers 7 | Out-Null
    $baselineSummary = Get-Content -Raw -LiteralPath (Join-Path $baseline 'benchmark-summary.json') | ConvertFrom-Json
    Assert-True ($baselineSummary.status -ceq 'PASS') 'baseline fixture did not pass'
    Assert-True ($baselineSummary.build_sha -match '^[0-9a-f]{40}$') 'build SHA was not recorded'
    Assert-True ([uint32]$baselineSummary.config.fields_mask -eq 2043) 'fields mask was not recorded'
    Assert-True ([int]$baselineSummary.config.workers -eq 7) 'worker count was not recorded'
    Assert-True (@($baselineSummary.disk_trace).Count -eq 1) 'per-disk trace was not recorded'
    Assert-True ($null -ne $baselineSummary.started_utc -and $null -ne $baselineSummary.finished_utc) 'run boundaries were not recorded'
    Assert-True ([int]$baselineSummary.result_summary.file_count -eq 2) 'result-set summary was not recorded'
    Assert-True ($baselineSummary.performance_status -ceq 'NOT_APPLICABLE') 'baseline declared a performance result'

    & $benchmark -Root $root -Mode Adaptive -OutputDir $adaptive -RunnerPath $runner `
        -MinimumDFreeBytes 0 -AllowFixtureRoot -FieldsMask 2043 -Workers 7 `
        -BaselineSummaryPath (Join-Path $baseline 'benchmark-summary.json') | Out-Null
    $adaptiveSummary = Get-Content -Raw -LiteralPath (Join-Path $adaptive 'benchmark-summary.json') | ConvertFrom-Json
    Assert-True ($adaptiveSummary.correctness_status -ceq 'PASS') 'identical result set was not accepted'
    Assert-True ($adaptiveSummary.performance_status -ceq 'PASS') '20 percent fixture target was not accepted'
    Assert-True ($adaptiveSummary.lifecycle.pause -ceq 'passed' -and $adaptiveSummary.lifecycle.stop -ceq 'passed') 'lifecycle evidence missing'

    $failedOutput = Join-Path $fixture 'adaptive-failed'
    $failed = $false
    try {
        & $benchmark -Root $root -Mode Adaptive -OutputDir $failedOutput -RunnerPath $failingRunner `
            -MinimumDFreeBytes 0 -AllowFixtureRoot -FieldsMask 2043 -Workers 7 `
            -BaselineSummaryPath (Join-Path $baseline 'benchmark-summary.json') | Out-Null
    } catch {
        $failed = $true
    }
    Assert-True $failed 'runner failure was swallowed'
    $failedSummary = Get-Content -Raw -LiteralPath (Join-Path $failedOutput 'benchmark-summary.json') | ConvertFrom-Json
    Assert-True ($failedSummary.status -ceq 'FAIL') 'runner failure was not recorded'
    Assert-True ($failedSummary.performance_status -cne 'PASS') 'failed run emitted performance PASS'
    Assert-True (@(Get-ChildItem -LiteralPath $root -Force).Count -eq 1) 'benchmark deleted source data or cache'

    $fakeGo = Join-Path $fixture 'fake-go.ps1'
    $fakeCC = Join-Path $fixture 'gcc.exe'
    $capture = Join-Path $fixture 'agent-build-capture.json'
    Write-Utf8NoBom -Path $fakeCC -Value 'fixture compiler'
    Write-Utf8NoBom -Path $fakeGo -Value @'
[ordered]@{
    cgo_enabled = $env:CGO_ENABLED
    cc = $env:CC
    arguments = @($args)
} | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath $env:TASK12_AGENT_BUILD_CAPTURE -Encoding utf8NoBOM
'@
    . (Join-Path $PSScriptRoot 'build.ps1')
    if (-not (Get-Command Invoke-AgentBuild -ErrorAction SilentlyContinue)) {
        throw 'INVOKE_AGENT_BUILD_MISSING'
    }
    $oldCapture = $env:TASK12_AGENT_BUILD_CAPTURE
    $oldCGO = $env:CGO_ENABLED
    $oldCC = $env:CC
    try {
        $env:TASK12_AGENT_BUILD_CAPTURE = $capture
        $env:CGO_ENABLED = 'fixture-old-cgo'
        $env:CC = 'fixture-old-cc'
        Invoke-AgentBuild -Go $fakeGo -RepositoryRoot $repo `
            -OutputPath (Join-Path $fixture 'agent.exe') -CCompiler $fakeCC `
            -Package './cmd/agent'
        $agentBuild = Get-Content -Raw -LiteralPath $capture | ConvertFrom-Json
        Assert-True ([string]$agentBuild.cgo_enabled -ceq '1') 'Agent build did not enable CGO'
        Assert-True ([string]$agentBuild.cc -ceq $fakeCC) 'Agent build did not use the verified compiler'
        Assert-True (@($agentBuild.arguments) -contains './cmd/agent') 'Agent build targeted the wrong package'
        Assert-True ($env:CGO_ENABLED -ceq 'fixture-old-cgo' -and $env:CC -ceq 'fixture-old-cc') 'Agent build leaked environment changes'
    } finally {
        $env:TASK12_AGENT_BUILD_CAPTURE = $oldCapture
        $env:CGO_ENABLED = $oldCGO
        $env:CC = $oldCC
    }

    $packageStage = Join-Path $fixture 'package-stage'
    $packageOutput = Join-Path $fixture 'package-output'
    New-Item -ItemType Directory -Force -Path (Join-Path $packageStage 'licenses'),$packageOutput | Out-Null
    foreach ($name in @('nodetray.exe','agent.exe','worker.exe','helper.exe','Everything.exe','Everything64.dll','MicrosoftEdgeWebview2Setup.exe','gui.exe','videocore.dll')) {
        Write-Utf8NoBom -Path (Join-Path $packageStage $name) -Value "fixture:$name"
    }
    Write-Utf8NoBom -Path (Join-Path $packageStage 'licenses\everything-LICENSE.txt') -Value 'fixture license'
    Write-Utf8NoBom -Path (Join-Path $packageStage 'licenses\everything-NOTICE.md') -Value 'fixture notice'
    foreach ($name in @('agent.default.json','nodetray.default.json','helper.default.json')) {
        Copy-Item -LiteralPath (Join-Path $repo "deploy\$name") -Destination (Join-Path $packageStage $name)
    }
    $nativeHash = (Get-FileHash -LiteralPath (Join-Path $packageStage 'videocore.dll') -Algorithm SHA256).Hash.ToLowerInvariant()
    Write-Utf8NoBom -Path (Join-Path $packageStage 'native-dependencies.json') -Value (([ordered]@{
        schema_version = 1
        files = @([ordered]@{name='videocore.dll';path='fixture/videocore.dll';sha256=$nativeHash;imports=@()})
    } | ConvertTo-Json -Depth 8) + [Environment]::NewLine)
    & (Join-Path $PSScriptRoot 'package-node-release.ps1') -StageDir $packageStage -OutputDir $packageOutput `
        -ReleaseId 'task12-contract' -BuildDate '2026-08-17' -SourceRevision ('a' * 40) | Out-Null
    & (Join-Path $PSScriptRoot 'package-manager-release.ps1') -StageDir $packageStage -OutputDir $packageOutput `
        -ReleaseId 'task12-contract' -BuildDate '2026-08-17' -SourceRevision ('a' * 40) | Out-Null
    $computeExtract = Join-Path $fixture 'compute-extract'
    $managerExtract = Join-Path $fixture 'manager-extract'
    Expand-Archive -LiteralPath (Join-Path $packageOutput 'MySingerServer-compute-win-x64-task12-contract.zip') -DestinationPath $computeExtract
    Expand-Archive -LiteralPath (Join-Path $packageOutput 'MySingerServer-manager-win-x64-task12-contract.zip') -DestinationPath $managerExtract
    $computeManifest = Get-Content -Raw -LiteralPath (Join-Path $computeExtract 'MySingerServer-Compute\release-manifest.json') | ConvertFrom-Json
    $managerManifest = Get-Content -Raw -LiteralPath (Join-Path $managerExtract 'MySingerServer-Manager\release-manifest.json') | ConvertFrom-Json
    Assert-True ([int]$computeManifest.compatibility.agent_worker_ipc_version -eq 2) 'compute manifest lacks Agent/Worker IPC ABI 2'
    Assert-True ([int]$computeManifest.compatibility.videocore_abi_version -eq 2) 'compute manifest lacks VideoCore ABI 2'
    Assert-True ([int]$computeManifest.compatibility.media_metadata_schema_version -eq 5) 'compute manifest lacks SQLite schema 5'
    Assert-True ([int]$managerManifest.compatibility.media_metadata_schema_version -eq 5) 'manager manifest lacks metadata schema 5'
} finally {
    if (Test-Path -LiteralPath $fixture) {
        Remove-Item -LiteralPath $fixture -Recurse -Force
    }
}

Write-Output 'BENCHMARK SCAN IO CONTRACT TEST PASS'
