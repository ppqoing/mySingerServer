[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$pwsh = Join-Path $PSHOME 'pwsh.exe'
$controllerSource = Join-Path $PSScriptRoot 'verify_m4.ps1'
$testRoot = Join-Path (
    [System.IO.Path]::GetTempPath()
) ('m4-bootstrap-negative-' + [Guid]::NewGuid().ToString('N'))
[System.IO.Directory]::CreateDirectory($testRoot) | Out-Null
$junctions = [System.Collections.Generic.List[string]]::new()

function New-M4BootstrapFixture {
    param([Parameter(Mandatory = $true)][string]$Name)
    $repo = Join-Path $testRoot $Name
    $scripts = Join-Path $repo 'scripts'
    [System.IO.Directory]::CreateDirectory($scripts) | Out-Null
    Copy-Item -LiteralPath $controllerSource `
        -Destination (Join-Path $scripts 'verify_m4.ps1')
    return [pscustomobject]@{
        Repo = $repo
        Controller = Join-Path $scripts 'verify_m4.ps1'
    }
}

function Invoke-M4BootstrapFixture {
    param(
        [Parameter(Mandatory = $true)][object]$Fixture,
        [AllowEmptyString()][string]$MarkerPath,
        [bool]$FailEvidenceCreate,
        [Parameter(Mandatory = $true)][string]$DSN,
        [bool]$ExpectSummary,
        [bool]$RawResult = $false
    )
    $oldMarker = [string]$env:M4_TEST_MARKER_PATH
    $oldFailure = [string]$env:M4_TEST_FAIL_EVIDENCE_CREATE
    try {
        if ([string]::IsNullOrWhiteSpace($MarkerPath)) {
            Remove-Item Env:M4_TEST_MARKER_PATH -ErrorAction SilentlyContinue
        }
        else {
            $env:M4_TEST_MARKER_PATH = $MarkerPath
        }
        if ($FailEvidenceCreate) {
            $env:M4_TEST_FAIL_EVIDENCE_CREATE = '1'
        }
        else {
            Remove-Item Env:M4_TEST_FAIL_EVIDENCE_CREATE `
                -ErrorAction SilentlyContinue
        }
        $output = @(
            & $pwsh -NoLogo -NoProfile -File $Fixture.Controller `
                -Go 'unused-go' `
                -GCC 'unused-gcc' `
                -PGDSN $DSN 2>&1
        )
        $exitCode = $LASTEXITCODE
    }
    finally {
        if ([string]::IsNullOrEmpty($oldMarker)) {
            Remove-Item Env:M4_TEST_MARKER_PATH -ErrorAction SilentlyContinue
        }
        else {
            $env:M4_TEST_MARKER_PATH = $oldMarker
        }
        if ([string]::IsNullOrEmpty($oldFailure)) {
            Remove-Item Env:M4_TEST_FAIL_EVIDENCE_CREATE `
                -ErrorAction SilentlyContinue
        }
        else {
            $env:M4_TEST_FAIL_EVIDENCE_CREATE = $oldFailure
        }
    }
    $text = $output -join "`n"
    if ($RawResult) {
        return [pscustomobject]@{
            ExitCode = $exitCode
            Text = $text
        }
    }
    if ($exitCode -eq 0) {
        throw 'bootstrap fixture unexpectedly exited zero'
    }
    $gateMatches = [regex]::Matches(
        $text,
        '(?m)^M4 GATE \S+ NOT_RUN exit=- log=-\r?$'
    )
    if ($gateMatches.Count -ne 15) {
        throw "bootstrap fixture emitted $($gateMatches.Count) NOT_RUN gates"
    }
    $final = [regex]::Match(
        $text,
        '(?m)^M4 FINAL RESULT FAIL run_id=\S+ evidence=(.+?) reason=.+\r?$'
    )
    if (-not $final.Success) {
        throw 'bootstrap fixture lacks a final FAIL line'
    }
    $summaryPath = $final.Groups[1].Value.Trim()
    if ($ExpectSummary) {
        if ($summaryPath -eq '-' -or
            -not (Test-Path -LiteralPath $summaryPath -PathType Leaf)) {
            throw 'bootstrap fixture lacks its required safe summary'
        }
        $summary = Get-Content -LiteralPath $summaryPath -Raw |
            ConvertFrom-Json
        if ($summary.status -cne 'FAIL' -or
            @($summary.required_gates).Count -ne 15 -or
            @($summary.gates.PSObject.Properties).Count -ne 15 -or
            @($summary.gates.PSObject.Properties.Value |
                Where-Object status -cne 'NOT_RUN').Count -ne 0) {
            throw 'bootstrap fixture summary is incomplete'
        }
    }
    elseif ($summaryPath -cne '-') {
        throw 'unsafe bootstrap fixture did not use evidence=-'
    }
    return [pscustomobject]@{
        Text = $text
        SummaryPath = $summaryPath
    }
}

function Assert-M4NoOutsideRunDirectory {
    param([Parameter(Mandatory = $true)][string]$Outside)
    $created = @(
        Get-ChildItem -LiteralPath $Outside -Directory -ErrorAction Stop
    )
    if ($created.Count -ne 0) {
        throw 'bootstrap created a run directory through a junction'
    }
}

function New-M4TestJunction {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Target
    )
    [System.IO.Directory]::CreateDirectory($Target) | Out-Null
    New-Item -ItemType Junction -Path $Path -Target $Target `
        -ErrorAction Stop | Out-Null
    $junctions.Add($Path)
}

try {
    $caseCount = 0
    foreach ($case in @(
        [pscustomobject]@{
            Name = 'missing_marker'
            Marker = 'missing-marker.ps1'
            Corrupt = $false
            Minimal = $false
            FailCreate = $false
        },
        [pscustomobject]@{
            Name = 'corrupt_marker'
            Marker = 'corrupt-marker.ps1'
            Corrupt = $true
            Minimal = $false
            FailCreate = $false
        },
        [pscustomobject]@{
            Name = 'evidence_creation_failure'
            Marker = 'minimal-marker.ps1'
            Corrupt = $false
            Minimal = $true
            FailCreate = $true
        }
    )) {
        $fixture = New-M4BootstrapFixture -Name $case.Name
        $marker = if ([string]::IsNullOrEmpty($case.Marker)) {
            ''
        }
        else {
            Join-Path $fixture.Repo $case.Marker
        }
        if ($case.Corrupt) {
            [System.IO.File]::WriteAllText($marker, 'this is not valid @@@')
        }
        elseif ($case.Minimal) {
            [System.IO.File]::WriteAllText(
                $marker,
                'function Assert-M4PathHasNoReparsePoint {}'
            )
        }
        [void](Invoke-M4BootstrapFixture `
            -Fixture $fixture `
            -MarkerPath $marker `
            -FailEvidenceCreate $case.FailCreate `
            -DSN 'unused-dsn' `
            -ExpectSummary $true)
        $caseCount++
    }

    $fallback = New-M4BootstrapFixture -Name 'fallback_junction'
    $fallbackMeta = Join-Path $fallback.Repo '.superpowers'
    [System.IO.Directory]::CreateDirectory($fallbackMeta) | Out-Null
    $fallbackOutside = Join-Path $testRoot 'fallback-outside'
    New-M4TestJunction `
        -Path (Join-Path $fallbackMeta 'tmp') `
        -Target $fallbackOutside
    $fallbackResult = Invoke-M4BootstrapFixture `
        -Fixture $fallback `
        -MarkerPath (Join-Path $fallback.Repo 'missing-marker.ps1') `
        -FailEvidenceCreate $false `
        -DSN 'unused-dsn' `
        -ExpectSummary $true `
        -RawResult $true
    $fallbackOutsideCount = @(
        Get-ChildItem -LiteralPath $fallbackOutside -Directory
    ).Count
    $fallbackSummaryCount = @(
        Get-ChildItem -LiteralPath $fallback.Repo `
            -Recurse -File -Filter 'm4-evidence.json'
    ).Count
    $fallbackGateCount = [regex]::Matches(
        $fallbackResult.Text,
        '(?m)^M4 GATE \S+ NOT_RUN exit=- log=-\r?$'
    ).Count
    $fallbackFinalCount = [regex]::Matches(
        $fallbackResult.Text,
        '(?m)^M4 FINAL RESULT FAIL '
    ).Count
    if ($fallbackResult.ExitCode -eq 0 -or
        $fallbackOutsideCount -ne 0 -or
        $fallbackSummaryCount -ne 1 -or
        $fallbackGateCount -ne 15 -or
        $fallbackFinalCount -ne 1) {
        throw (
            'fallback junction contract ' +
            "outside=$fallbackOutsideCount " +
            "summary=$fallbackSummaryCount " +
            "gates=$fallbackGateCount final=$fallbackFinalCount"
        )
    }
    $caseCount++

    $preferred = New-M4BootstrapFixture -Name 'preferred_junction'
    $preferredMeta = Join-Path $preferred.Repo '.superpowers'
    [System.IO.Directory]::CreateDirectory($preferredMeta) | Out-Null
    $preferredOutside = Join-Path $testRoot 'preferred-outside'
    New-M4TestJunction `
        -Path (Join-Path $preferredMeta 'evidence') `
        -Target $preferredOutside
    $minimalMarker = Join-Path $preferred.Repo 'minimal-marker.ps1'
    [System.IO.File]::WriteAllText(
        $minimalMarker,
        'function Assert-M4PathHasNoReparsePoint {}'
    )
    [void](Invoke-M4BootstrapFixture `
        -Fixture $preferred `
        -MarkerPath $minimalMarker `
        -FailEvidenceCreate $false `
        -DSN 'unused-dsn' `
        -ExpectSummary $true)
    Assert-M4NoOutsideRunDirectory -Outside $preferredOutside
    $caseCount++

    $consoleOnly = New-M4BootstrapFixture -Name 'all_fallbacks_unsafe'
    $consoleMeta = Join-Path $consoleOnly.Repo '.superpowers'
    [System.IO.Directory]::CreateDirectory($consoleMeta) | Out-Null
    $consoleTmpOutside = Join-Path $testRoot 'console-tmp-outside'
    $consoleBootstrapOutside = Join-Path $testRoot 'console-bootstrap-outside'
    New-M4TestJunction `
        -Path (Join-Path $consoleMeta 'tmp') `
        -Target $consoleTmpOutside
    New-M4TestJunction `
        -Path (Join-Path $consoleMeta 'bootstrap') `
        -Target $consoleBootstrapOutside
    [void](Invoke-M4BootstrapFixture `
        -Fixture $consoleOnly `
        -MarkerPath (Join-Path $consoleOnly.Repo 'missing-marker.ps1') `
        -FailEvidenceCreate $false `
        -DSN 'unused-dsn' `
        -ExpectSummary $false)
    Assert-M4NoOutsideRunDirectory -Outside $consoleTmpOutside
    Assert-M4NoOutsideRunDirectory -Outside $consoleBootstrapOutside
    $caseCount++

    $secretFixture = New-M4BootstrapFixture -Name 'keyword_dsn_secret'
    $secretMarker = Join-Path $secretFixture.Repo 'secret-marker.ps1'
    [System.IO.File]::WriteAllText($secretMarker, 'throw $PGDSN')
    $secretPassword = 'UNIQUE_M4_BOOTSTRAP_PASSWORD_7f31'
    $keywordDSN = (
        "host=127.0.0.1 port=5432 user=dedup " +
        "password=$secretPassword dbname=dedup sslmode=disable"
    )
    $secretResult = Invoke-M4BootstrapFixture `
        -Fixture $secretFixture `
        -MarkerPath $secretMarker `
        -FailEvidenceCreate $false `
        -DSN $keywordDSN `
        -ExpectSummary $true
    $summaryText = Get-Content -LiteralPath $secretResult.SummaryPath -Raw
    $leak = $secretResult.Text.Contains($keywordDSN) -or
        $secretResult.Text.Contains($secretPassword) -or
        $summaryText.Contains($keywordDSN) -or
        $summaryText.Contains($secretPassword)
    if ($leak) {
        throw 'bootstrap keyword DSN leak detected'
    }
    Write-Host 'M4_BOOTSTRAP_SECRET LEAK=false'
    $caseCount++

    Write-Host 'M4_BOOTSTRAP_NEGATIVE_CATEGORY failure_artifact cases=3'
    Write-Host 'M4_BOOTSTRAP_NEGATIVE_CATEGORY path_boundary cases=3'
    Write-Host 'M4_BOOTSTRAP_NEGATIVE_CATEGORY secret_redaction cases=1'
    Write-Host "M4_BOOTSTRAP_NEGATIVE_PASS cases=$caseCount"
}
finally {
    foreach ($junction in $junctions) {
        if (Test-Path -LiteralPath $junction) {
            Remove-Item -LiteralPath $junction -Force
        }
    }
    $fullTestRoot = [System.IO.Path]::GetFullPath($testRoot)
    $tempPrefix = [System.IO.Path]::GetFullPath(
        [System.IO.Path]::GetTempPath()
    ).TrimEnd('\') + '\'
    if ($fullTestRoot.StartsWith(
        $tempPrefix,
        [System.StringComparison]::OrdinalIgnoreCase
    )) {
        Remove-Item -LiteralPath $fullTestRoot -Recurse -Force
    }
}
