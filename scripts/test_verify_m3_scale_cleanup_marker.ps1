[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'verify_m3_marker.ps1')
. (Join-Path $PSScriptRoot 'verify_m3_scale_marker.ps1')

$runID = 'cleanup-marker-test'
$schema = 'm3_scale_cleanup_marker_test'
$validJSON = [ordered]@{
    run_id = $runID
    schema = $schema
    cleanup_residual = 0
} | ConvertTo-Json -Compress
$validOutput = @"
=== RUN   TestAcceptanceM3
    scale_acceptance_test.go:1: M3_SCALE_CLEANUP $validJSON
--- PASS: TestAcceptanceM3 (0.01s)
PASS
"@

Assert-M3ScaleCleanupOutput `
    -Output $validOutput `
    -ExpectedRunID $runID `
    -ExpectedSchema $schema | Out-Null

function Assert-CleanupRejected {
    param(
        [string]$Name,
        [string]$Output,
        [string]$ExpectedRunID = $runID,
        [string]$ExpectedSchema = $schema
    )
    $rejected = $false
    try {
        Assert-M3ScaleCleanupOutput `
            -Output $Output `
            -ExpectedRunID $ExpectedRunID `
            -ExpectedSchema $ExpectedSchema | Out-Null
    }
    catch {
        $rejected = $true
    }
    if (-not $rejected) {
        throw "negative cleanup marker unexpectedly accepted: $Name"
    }
}

$missing = $validOutput -replace 'M3_SCALE_CLEANUP .+', 'cleanup completed'
$duplicate = $validOutput -replace (
    'M3_SCALE_CLEANUP .+'
), "M3_SCALE_CLEANUP $validJSON`nM3_SCALE_CLEANUP $validJSON"
$wrongRunJSON = $validJSON -replace 'cleanup-marker-test', 'other-run'
$wrongSchemaJSON = $validJSON -replace (
    'm3_scale_cleanup_marker_test'
), 'm3_scale_other_run'
$nullJSON = $validJSON -replace '"cleanup_residual":0', '"cleanup_residual":null'
$stringJSON = $validJSON -replace '"cleanup_residual":0', '"cleanup_residual":"0"'
$nonzeroJSON = $validJSON -replace '"cleanup_residual":0', '"cleanup_residual":1'

$cases = @(
    @{ Name = 'missing'; Output = $missing },
    @{ Name = 'duplicate'; Output = $duplicate },
    @{ Name = 'wrong_run'; Output = ($validOutput -replace [regex]::Escape($validJSON), $wrongRunJSON) },
    @{ Name = 'wrong_schema'; Output = ($validOutput -replace [regex]::Escape($validJSON), $wrongSchemaJSON) },
    @{ Name = 'null_residual'; Output = ($validOutput -replace [regex]::Escape($validJSON), $nullJSON) },
    @{ Name = 'string_residual'; Output = ($validOutput -replace [regex]::Escape($validJSON), $stringJSON) },
    @{ Name = 'nonzero_residual'; Output = ($validOutput -replace [regex]::Escape($validJSON), $nonzeroJSON) }
)

foreach ($case in $cases) {
    Assert-CleanupRejected -Name $case.Name -Output $case.Output
}

$combinedFailure = Join-M3ScaleFailures `
    -Primary 'primary gate failed' `
    -Cleanup 'cleanup marker failed'
if (-not $combinedFailure.Contains('primary gate failed') -or
    -not $combinedFailure.Contains('cleanup marker failed')) {
    throw 'combined failure did not preserve primary and cleanup reasons'
}

Write-Host "M3_SCALE_CLEANUP_MARKER_NEGATIVE_PASS cases=$($cases.Count)"
