. (Join-Path $PSScriptRoot 'verify_m3_final_gate.ps1')

function Get-M4RepoOwnedGoFiles {
    param([Parameter(Mandatory = $true)][string]$RepoRoot)
    return Get-M3RepoOwnedGoFiles -RepoRoot $RepoRoot
}

function Get-M4BuildOutDir {
    param(
        [Parameter(Mandatory = $true)][string]$RepoRoot,
        [Parameter(Mandatory = $true)][string]$StageDir
    )
    $fullRoot = [System.IO.Path]::GetFullPath($RepoRoot)
    $fullStage = [System.IO.Path]::GetFullPath($StageDir)
    $relative = [System.IO.Path]::GetRelativePath($fullRoot, $fullStage)
    if ([System.IO.Path]::IsPathRooted($relative) -or
        $relative -eq '..' -or
        $relative.StartsWith('..\', [System.StringComparison]::Ordinal)) {
        throw 'M4 build output directory must stay inside the workspace.'
    }
    if ([string]::IsNullOrWhiteSpace($relative) -or $relative -eq '.') {
        throw 'M4 build output directory must not be the workspace root.'
    }
    return $relative
}

function Assert-M4NamedPasses {
    param(
        [Parameter(Mandatory = $true)][string]$Output,
        [Parameter(Mandatory = $true)][string[]]$Names
    )
    if ($Output -match '(?m)^\s*--- SKIP:') {
        throw 'named acceptance output contains a skipped test.'
    }
    foreach ($name in $Names) {
        if (-not $Output.Contains("=== RUN   $name") -or
            -not $Output.Contains("--- PASS: $name")) {
            throw "named acceptance output lacks a RUN/PASS proof for $name"
        }
    }
}

function Assert-M4ExactExports {
    param(
        [Parameter(Mandatory = $true)][string[]]$Expected,
        [Parameter(Mandatory = $true)][string[]]$Actual
    )
    $expectedSet = [System.Collections.Generic.HashSet[string]]::new(
        [System.StringComparer]::Ordinal
    )
    foreach ($name in $Expected) {
        if ([string]::IsNullOrWhiteSpace($name) -or
            -not $expectedSet.Add($name)) {
            throw "expected native export is empty or duplicated: $name"
        }
    }
    $actualExact = [System.Collections.Generic.HashSet[string]]::new(
        [System.StringComparer]::Ordinal
    )
    $actualFolded = [System.Collections.Generic.HashSet[string]]::new(
        [System.StringComparer]::OrdinalIgnoreCase
    )
    foreach ($name in $Actual) {
        if ([string]::IsNullOrWhiteSpace($name) -or
            -not $actualExact.Add($name) -or
            -not $actualFolded.Add($name)) {
            throw "native export is empty, duplicated, or differs only by case: $name"
        }
    }
    if (-not $expectedSet.SetEquals($actualExact)) {
        throw "native exports differ expected=$($Expected -join ',') actual=$($Actual -join ',')"
    }
}

function ConvertTo-M4StrictJSONObject {
    param([Parameter(Mandatory = $true)][object]$Value)
    return (($Value | ConvertTo-Json -Depth 16) | ConvertFrom-Json)
}

function Assert-M4PathHasNoReparsePoint {
    param(
        [Parameter(Mandatory = $true)][string]$FullRoot,
        [Parameter(Mandatory = $true)][string]$FullPath,
        [scriptblock]$AttributeReader
    )
    Assert-M3PathHasNoReparsePoint `
        -FullRoot ([System.IO.Path]::GetFullPath($FullRoot)) `
        -FullFile ([System.IO.Path]::GetFullPath($FullPath)) `
        -AttributeReader $AttributeReader
}
