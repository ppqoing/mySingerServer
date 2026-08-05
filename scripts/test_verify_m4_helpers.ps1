[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'verify_m4_helpers.ps1')

function Assert-M4HelperRejected {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][scriptblock]$Action
    )
    $rejected = $false
    try {
        & $Action
    }
    catch {
        $rejected = $true
    }
    if (-not $rejected) {
        throw "M4 helper negative case unexpectedly accepted: $Name"
    }
}

$root = 'C:\fixture-repo'
$stage = 'C:\fixture-repo\.superpowers\evidence\m4-run\fresh-bin'
$relative = Get-M4BuildOutDir -RepoRoot $root -StageDir $stage
if ([System.IO.Path]::IsPathRooted($relative) -or
    [System.IO.Path]::GetFullPath((Join-Path $root $relative)) -cne $stage) {
    throw "M4 build OutDir was not repo-relative: $relative"
}
Assert-M4HelperRejected -Name 'build_outside_repo' -Action {
    Get-M4BuildOutDir `
        -RepoRoot $root `
        -StageDir 'C:\outside\fresh-bin' | Out-Null
}

$namedPass = "=== RUN   TestAcceptance`n--- PASS: TestAcceptance (0.01s)"
Assert-M4NamedPasses -Output $namedPass -Names @('TestAcceptance')
Assert-M4HelperRejected -Name 'top_level_skip' -Action {
    Assert-M4NamedPasses `
        -Output "=== RUN   TestAcceptance`n--- SKIP: TestAcceptance (0.01s)" `
        -Names @('TestAcceptance')
}
Assert-M4HelperRejected -Name 'indented_skip' -Action {
    Assert-M4NamedPasses `
        -Output "=== RUN   TestAcceptance`n    --- SKIP: TestAcceptance/sub (0.01s)" `
        -Names @('TestAcceptance')
}

Assert-M4ExactExports -Expected @('mc_one', 'mc_two') `
    -Actual @('mc_one', 'mc_two')
Assert-M4HelperRejected -Name 'wrong_case_export' -Action {
    Assert-M4ExactExports -Expected @('mc_one') -Actual @('MC_ONE')
}
Assert-M4HelperRejected -Name 'duplicate_exact_export' -Action {
    Assert-M4ExactExports -Expected @('mc_one') `
        -Actual @('mc_one', 'mc_one')
}
Assert-M4HelperRejected -Name 'duplicate_case_variant_export' -Action {
    Assert-M4ExactExports -Expected @('mc_one') `
        -Actual @('mc_one', 'MC_ONE')
}

$orderedGateMap = [ordered]@{
    format = [ordered]@{
        status = 'PASS'
        exit_code = 0
        log = 'format.log'
    }
}
$strictGateMap = ConvertTo-M4StrictJSONObject -Value $orderedGateMap
if ($strictGateMap -is [System.Collections.IDictionary] -or
    $strictGateMap.format.status -cne 'PASS' -or
    $strictGateMap.format.exit_code -isnot [int64]) {
    throw 'ordered gate map did not round-trip to strict JSON object shape'
}

$attributeReader = {
    param([string]$Path)
    if ($Path -ceq 'C:\fixture-repo\nested') {
        return [System.IO.FileAttributes]::Directory -bor
            [System.IO.FileAttributes]::ReparsePoint
    }
    return [System.IO.FileAttributes]::Directory
}
Assert-M4HelperRejected -Name 'nested_reparse_attribute' -Action {
    Assert-M4PathHasNoReparsePoint `
        -FullRoot 'C:\fixture-repo' `
        -FullPath 'C:\fixture-repo\nested\leaf.log' `
        -AttributeReader $attributeReader
}

Write-Host 'M4_HELPERS_NEGATIVE_PASS cases=8'
