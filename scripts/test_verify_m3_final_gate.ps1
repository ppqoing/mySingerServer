[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'verify_m3_marker.ps1')
. (Join-Path $PSScriptRoot 'verify_m3_final_gate.ps1')

function Assert-Rejected {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Name,
        [Parameter(Mandatory = $true)]
        [scriptblock]$Action
    )
    $rejected = $false
    try {
        & $Action
    }
    catch {
        $rejected = $true
    }
    if (-not $rejected) {
        throw "Task10 negative case unexpectedly accepted: $Name"
    }
}

$expectedPGTests = @(
    'TestPGKeysetNullableFirstPageIncludesEmptyKeysAndTerminates',
    'TestPGKeysetReadersPageThreeFilteringOrderingAndBadRows',
    'TestPGKeysetContextCancellationQueryAndCallbackErrors',
    'TestPGKeysetSchemaTwiceIndexesAndExplainEligibility',
    'TestPGReplaceResultsSuccessIdempotencyAndM4Preservation',
    'TestPGReplaceResultsRemoteFailuresRollbackAndKeepConnectionUsable',
    'TestPGReplaceResultsAbortedCommitIsDefiniteRollback',
    'TestPGReplaceResultsLostCommitAckIsUnknownAndRetryConverges',
    'TestPGReplaceResultsClosedConnectionDoesNotChangeResults',
    'TestPGReplaceResultsConcurrentM4InsertAfterDeleteIsPreserved'
)

Assert-M3FormatOutput -Output ''
Assert-Rejected -Name 'gofmt_output' -Action {
    Assert-M3FormatOutput -Output "internal/firstscreen/store.go`n"
}

$pgLines = [System.Collections.Generic.List[string]]::new()
for ($index = 0; $index -lt $expectedPGTests.Count; $index++) {
    $test = $expectedPGTests[$index]
    $schema = 'fs_t4_contract{0:d2}' -f $index
    $pgLines.Add("=== RUN   $test")
    $pgLines.Add(
        "    store_integration_test.go:1: Task4 cleanup schema=$schema residual=0"
    )
    if ($test -eq 'TestPGReplaceResultsRemoteFailuresRollbackAndKeepConnectionUsable') {
        for ($subcase = 1; $subcase -le 8; $subcase++) {
            $subSchema = 'fs_t4_contract{0:d2}sub{1:d2}' -f $index, $subcase
            $pgLines.Add(
                "    store_integration_test.go:1: Task4 cleanup schema=$subSchema residual=0"
            )
        }
    }
    $pgLines.Add("--- PASS: $test (0.01s)")
}
$pgLines.Add('PASS')
$validPGOutput = $pgLines -join "`n"
$pgEvidence = Assert-M3PGContractOutput `
    -Output $validPGOutput `
    -ExpectedTests $expectedPGTests
if ($pgEvidence.tests_passed.Count -ne 10 -or
    $pgEvidence.cleanup.Count -ne 18 -or
    -not $pgEvidence.schema_twice -or
    -not $pgEvidence.exact_indexes -or
    -not $pgEvidence.actual_explain -or
    -not $pgEvidence.failure_rollback -or
    -not $pgEvidence.unknown_commit -or
    -not $pgEvidence.m4_preservation) {
    throw 'valid PostgreSQL contract evidence is incomplete'
}

$skipOutput = $validPGOutput -replace (
    [regex]::Escape("--- PASS: $($expectedPGTests[0]) (0.01s)")
), "--- SKIP: $($expectedPGTests[0]) (0.01s)"
Assert-Rejected -Name 'postgres_skip' -Action {
    Assert-M3PGContractOutput `
        -Output $skipOutput `
        -ExpectedTests $expectedPGTests | Out-Null
}
$missingPassOutput = $validPGOutput -replace (
    [regex]::Escape("--- PASS: $($expectedPGTests[1]) (0.01s)")
), ''
Assert-Rejected -Name 'postgres_missing_pass' -Action {
    Assert-M3PGContractOutput `
        -Output $missingPassOutput `
        -ExpectedTests $expectedPGTests | Out-Null
}

Assert-M3CommandExitCode -Name 'pure_go' -ExitCode 0
Assert-Rejected -Name 'pure_go_failure' -Action {
    Assert-M3CommandExitCode -Name 'pure_go' -ExitCode 1
}

foreach ($matrix in @(
    @('M3_MARKER_NEGATIVE_PASS', 12),
    @('M3_SCALE_MARKER_NEGATIVE_PASS', 23),
    @('M3_SCALE_CLEANUP_MARKER_NEGATIVE_PASS', 7)
)) {
    $marker = [string]$matrix[0]
    $count = [int]$matrix[1]
    Assert-M3MatrixOutput `
        -Output "$marker cases=$count" `
        -Marker $marker `
        -ExpectedCases $count
    Assert-Rejected -Name "matrix_missing_$marker" -Action {
        Assert-M3MatrixOutput `
            -Output 'PASS' `
            -Marker $marker `
            -ExpectedCases $count
    }
    Assert-Rejected -Name "matrix_duplicate_$marker" -Action {
        Assert-M3MatrixOutput `
            -Output "$marker cases=$count`n$marker cases=$count" `
            -Marker $marker `
            -ExpectedCases $count
    }
}

if (-not (Resolve-M3ScaleMode -RunScale)) {
    throw '-RunScale did not enable scale mode'
}
if (-not (Resolve-M3ScaleMode -Scale -RunScale)) {
    throw 'combined -Scale/-RunScale did not enable scale mode'
}
if (Resolve-M3ScaleMode) {
    throw 'quick mode unexpectedly enabled scale mode'
}

$repoRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$testDir = Join-Path (
    Join-Path $repoRoot '.superpowers\tmp'
) ('m3-final-gate-test-' + [Guid]::NewGuid().ToString('N'))
[System.IO.Directory]::CreateDirectory($testDir) | Out-Null
try {
    $repoGoFiles = @(Get-M3RepoOwnedGoFiles -RepoRoot $repoRoot)
    $requiredRepoFile = [System.IO.Path]::GetFullPath(
        (Join-Path $repoRoot 'testdata\m2\gen_corrupt.go')
    )
    if ($repoGoFiles.Count -lt 104 -or
        $repoGoFiles -cnotcontains $requiredRepoFile) {
        throw "repository Go enumeration count=$($repoGoFiles.Count), want at least 104 including testdata/m2/gen_corrupt.go"
    }

    $enumerationRoot = Join-Path $testDir 'enumeration-root'
    $includedPaths = @(
        'keep.go',
        'testdata\m2\included.go'
    )
    $excludedPaths = @(
        '.git\ignored.go',
        '.superpowers\evidence\ignored.go',
        '.superpowers\tmp\ignored.go',
        '.tmp\gomodcache\ignored.go',
        'vendor\ignored.go',
        'node_modules\ignored.go',
        'third_party\ignored.go',
        'build\ignored.go',
        'dist\ignored.go',
        'out\ignored.go',
        'bin\ignored.go',
        'obj\ignored.go',
        '.cache\ignored.go'
    )
    foreach ($relative in @($includedPaths + $excludedPaths)) {
        $path = Join-Path $enumerationRoot $relative
        [System.IO.Directory]::CreateDirectory(
            [System.IO.Path]::GetDirectoryName($path)
        ) | Out-Null
        [System.IO.File]::WriteAllText($path, "package fixture`n")
    }
    $fixtureGoFiles = @(Get-M3RepoOwnedGoFiles -RepoRoot $enumerationRoot)
    $fixtureRelative = @($fixtureGoFiles | ForEach-Object {
        [System.IO.Path]::GetRelativePath($enumerationRoot, $_)
    })
    if ($fixtureRelative.Count -ne 2 -or
        $fixtureRelative -cnotcontains 'keep.go' -or
        $fixtureRelative -cnotcontains 'testdata\m2\included.go') {
        throw "safe fixture enumeration was $($fixtureRelative -join ', ')"
    }

    Assert-Rejected -Name 'go_files_zero' -Action {
        Assert-M3RepoOwnedGoFiles `
            -RepoRoot $enumerationRoot `
            -Files @() | Out-Null
    }
    Assert-Rejected -Name 'go_files_duplicate' -Action {
        Assert-M3RepoOwnedGoFiles `
            -RepoRoot $enumerationRoot `
            -Files @($fixtureGoFiles[0], $fixtureGoFiles[0]) | Out-Null
    }
    $outsideFile = Join-Path $testDir 'outside.go'
    [System.IO.File]::WriteAllText($outsideFile, "package outside`n")
    Assert-Rejected -Name 'go_files_outside_root' -Action {
        Assert-M3RepoOwnedGoFiles `
            -RepoRoot $enumerationRoot `
            -Files @($fixtureGoFiles[0], $outsideFile) | Out-Null
    }
    $nonGoFile = Join-Path $enumerationRoot 'not-go.txt'
    [System.IO.File]::WriteAllText($nonGoFile, "not Go`n")
    Assert-Rejected -Name 'go_files_non_go' -Action {
        Assert-M3RepoOwnedGoFiles `
            -RepoRoot $enumerationRoot `
            -Files @($fixtureGoFiles[0], $nonGoFile) | Out-Null
    }
    Assert-Rejected -Name 'go_files_missing' -Action {
        Assert-M3RepoOwnedGoFiles `
            -RepoRoot $enumerationRoot `
            -Files @(
                $fixtureGoFiles[0],
                (Join-Path $enumerationRoot 'missing.go')
            ) | Out-Null
    }
    $junctionTarget = Join-Path $testDir 'junction-target'
    [System.IO.Directory]::CreateDirectory($junctionTarget) | Out-Null
    $junctionTargetFile = Join-Path $junctionTarget 'outside.go'
    [System.IO.File]::WriteAllText(
        $junctionTargetFile,
        "package outside`n"
    )
    $junction = Join-Path $enumerationRoot 'junction'
    New-Item `
        -ItemType Junction `
        -Path $junction `
        -Target $junctionTarget `
        -ErrorAction Stop | Out-Null
    Assert-Rejected -Name 'go_files_junction_escape' -Action {
        Assert-M3RepoOwnedGoFiles `
            -RepoRoot $enumerationRoot `
            -Files @((Join-Path $junction 'outside.go')) | Out-Null
    }

    $formatLog = Join-Path $testDir 'format.log'
    $pureLog = Join-Path $testDir 'pure_go.log'
    [System.IO.File]::WriteAllText($formatLog, '')
    [System.IO.File]::WriteAllText($pureLog, 'ok')
    $validGates = [ordered]@{
        format = [ordered]@{
            status = 'PASS'
            exit_code = 0
            log = $formatLog
        }
        pure_go = [ordered]@{
            status = 'PASS'
            exit_code = 0
            log = $pureLog
        }
    }
    $required = @('format', 'pure_go')
    Assert-M3RequiredGateEvidence `
        -GateResults $validGates `
        -RequiredGates $required `
        -EvidenceDir $testDir

    $missingGate = [ordered]@{
        format = $validGates.format
    }
    Assert-Rejected -Name 'missing_gate' -Action {
        Assert-M3RequiredGateEvidence `
            -GateResults $missingGate `
            -RequiredGates $required `
            -EvidenceDir $testDir
    }
    $missingLog = [ordered]@{
        format = $validGates.format
        pure_go = [ordered]@{
            status = 'PASS'
            exit_code = 0
            log = (Join-Path $testDir 'absent.log')
        }
    }
    Assert-Rejected -Name 'missing_log' -Action {
        Assert-M3RequiredGateEvidence `
            -GateResults $missingLog `
            -RequiredGates $required `
            -EvidenceDir $testDir
    }

    $summaryGates = [ordered]@{
        format = $validGates.format
        pure_go = [ordered]@{
            status = 'FAIL'
            exit_code = 1
            log = $pureLog
        }
    }
    Add-M3NotRunGates `
        -GateResults $summaryGates `
        -RequiredGates @('format', 'pure_go', 'vet')
    $summaryLines = @(Get-M3GateSummaryLines `
        -GateResults $summaryGates `
        -RequiredGates @('format', 'pure_go', 'vet'))
    if ($summaryLines.Count -ne 3 -or
        $summaryLines[0] -notmatch '^M3 GATE format PASS ' -or
        $summaryLines[1] -notmatch '^M3 GATE pure_go FAIL ' -or
        $summaryLines[2] -notmatch '^M3 GATE vet NOT_RUN ') {
        throw "unexpected dynamic gate summary: $($summaryLines -join ' | ')"
    }
}
finally {
    $resolvedTestDir = [System.IO.Path]::GetFullPath($testDir)
    $safePrefix = [System.IO.Path]::GetFullPath(
        (Join-Path $repoRoot '.superpowers\tmp')
    ).TrimEnd('\') + '\'
    if ($resolvedTestDir.StartsWith(
        $safePrefix,
        [System.StringComparison]::OrdinalIgnoreCase
    )) {
        Remove-Item -LiteralPath $resolvedTestDir -Recurse -Force
    }
}

Write-Host 'M3_FINAL_GATE_NEGATIVE_PASS cases=18'
