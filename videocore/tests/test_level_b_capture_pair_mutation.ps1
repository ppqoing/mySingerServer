param(
    [Parameter(Mandatory = $true)] [string]$CaptureScript,
    [Parameter(Mandatory = $true)] [string]$Runner,
    [Parameter(Mandatory = $true)] [string]$CorrectDll,
    [Parameter(Mandatory = $true)] [string]$WrongDll,
    [Parameter(Mandatory = $true)] [string]$InputRoot,
    [Parameter(Mandatory = $true)] [string]$RepoRoot,
    [Parameter(Mandatory = $true)] [string]$Dumpbin,
    [Parameter(Mandatory = $true)] [string]$ExpectedManifestSha256,
    [Parameter(Mandatory = $true)] [string]$ExpectedGoldenSha256
)

$ErrorActionPreference = 'Stop'
$repo = [IO.Path]::GetFullPath($RepoRoot).TrimEnd('\')
$safeBase = Join-Path $repo '.tmp\videocore-levelb-capture-tests'
$caseRoot = Join-Path $safeBase ([guid]::NewGuid().ToString('N'))
$caseFull = [IO.Path]::GetFullPath($caseRoot)
$safePrefix = [IO.Path]::GetFullPath($safeBase).TrimEnd('\') + '\'
if (-not $caseFull.StartsWith($safePrefix, [StringComparison]::OrdinalIgnoreCase)) {
    throw "refusing unsafe capture mutation root: $caseFull"
}

function Require-X64PeDll([string]$Path, [string]$Role) {
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { throw "$Role is missing: $Path" }
    $headers = (& $Dumpbin /nologo /headers $Path 2>&1 | Out-String)
    if ($LASTEXITCODE -ne 0 -or $headers -notmatch '(?im)8664 machine \(x64\)' -or
        $headers -notmatch '(?im)\bDLL\b') {
        throw "$Role is not a valid x64 PE DLL: $Path"
    }
    return (& $Dumpbin /nologo /exports $Path 2>&1 | Out-String)
}

function Invoke-Capture([string]$DllPath, [string]$OutDir) {
    $text = (& 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -File $CaptureScript `
        -Runner $Runner -Dll $DllPath -InputRoot $InputRoot -OutDir $OutDir 2>&1 | Out-String)
    return [ordered]@{ ExitCode=$LASTEXITCODE; Output=$text }
}

try {
    New-Item -ItemType Directory -Force -Path $caseFull | Out-Null
    $runnerImportsText = (& $Dumpbin /nologo /imports $Runner 2>&1 | Out-String)
    if ($LASTEXITCODE -ne 0) { throw "cannot inspect legacy runner imports: $runnerImportsText" }
    $requiredExports = @([regex]::Matches($runnerImportsText, '(?im)^\s+[0-9A-F]+\s+(mc_[a-z0-9_]+)\s*$') |
        ForEach-Object { $_.Groups[1].Value } | Sort-Object -Unique)
    if ($requiredExports.Count -eq 0) { throw 'legacy runner has no imported mc_* entry points' }
    $correctExports = Require-X64PeDll $CorrectDll 'correct legacy DLL'
    foreach ($name in $requiredExports) {
        if ($correctExports -notmatch "(?im)\b$([regex]::Escape($name))\b") {
            throw "correct legacy DLL is missing required export: $name"
        }
    }
    $wrongExports = Require-X64PeDll $WrongDll 'wrong DLL fixture'
    foreach ($name in $requiredExports) {
        if ($wrongExports -match "(?im)\b$([regex]::Escape($name))\b") {
            throw "wrong DLL fixture unexpectedly exports $name"
        }
    }

    $positiveOut = Join-Path $caseFull 'positive-artifact'
    $positive = Invoke-Capture $CorrectDll $positiveOut
    if ($positive.ExitCode -ne 0) {
        throw "declared valid runner/DLL pair failed positive control: $($positive.Output)"
    }
    $manifestHash = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $positiveOut 'manifest.json')).Hash
    $goldenHash = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $positiveOut 'legacy-golden.tsv')).Hash
    if ($manifestHash -ne $ExpectedManifestSha256 -or $goldenHash -ne $ExpectedGoldenSha256) {
        throw "valid pair did not reproduce frozen hashes: manifest=$manifestHash golden=$goldenHash"
    }

    $wrongOut = Join-Path $caseFull 'wrong-artifact'
    $wrong = Invoke-Capture $WrongDll $wrongOut
    if ($wrong.ExitCode -eq 0) {
        throw 'capture incorrectly accepted a runner paired with the wrong DLL'
    }
    # STATUS_ENTRYPOINT_NOT_FOUND (0xC0000139) proves the isolated x64 DLL was
    # found but did not provide the runner's imported mc_* entry points.
    if ($wrong.Output -notmatch 'isolated legacy runner failed for .+: exit=-1073741511\b') {
        throw "wrong pair did not fail with STATUS_ENTRYPOINT_NOT_FOUND: $($wrong.Output)"
    }
    if (Test-Path -LiteralPath $wrongOut) {
        throw 'failed wrong-pair capture left an output directory'
    }
    Write-Output 'LEVEL_B_CAPTURE_PAIR_MUTATION PASS positive_pair=frozen_hashes wrong_dll=x64_pe_no_mc_exports expected_status=0xC0000139 partial_artifact=false'
} finally {
    if ((Test-Path -LiteralPath $caseFull) -and
        $caseFull.StartsWith($safePrefix, [StringComparison]::OrdinalIgnoreCase)) {
        Remove-Item -LiteralPath $caseFull -Recurse -Force
    }
}
