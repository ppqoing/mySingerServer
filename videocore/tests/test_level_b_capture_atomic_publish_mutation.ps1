param(
    [Parameter(Mandatory = $true)] [string]$CaptureScript,
    [Parameter(Mandatory = $true)] [string]$Runner,
    [Parameter(Mandatory = $true)] [string]$CorrectDll,
    [Parameter(Mandatory = $true)] [string]$InputRoot,
    [Parameter(Mandatory = $true)] [string]$RepoRoot
)

$ErrorActionPreference = 'Stop'
$repo = [IO.Path]::GetFullPath($RepoRoot).TrimEnd('\')
$safeBase = Join-Path $repo '.tmp\videocore-levelb-atomic-publish-tests'
$caseRoot = Join-Path $safeBase ([guid]::NewGuid().ToString('N'))
$caseFull = [IO.Path]::GetFullPath($caseRoot)
$safePrefix = [IO.Path]::GetFullPath($safeBase).TrimEnd('\') + '\'
if (-not $caseFull.StartsWith($safePrefix, [StringComparison]::OrdinalIgnoreCase)) {
    throw "refusing unsafe atomic-publish mutation root: $caseFull"
}

$hadProcessFaultEnv = Test-Path Env:VC_LEVEL_B_CAPTURE_TEST_FAIL_BEFORE_PUBLISH
$processFaultEnv = $env:VC_LEVEL_B_CAPTURE_TEST_FAIL_BEFORE_PUBLISH
$env:VC_LEVEL_B_CAPTURE_TEST_FAIL_BEFORE_PUBLISH = 'preexisting-sentinel'
try {
    try {
        New-Item -ItemType Directory -Force -Path $caseFull | Out-Null
        $outDir = Join-Path $caseFull 'artifact'
        $env:VC_LEVEL_B_CAPTURE_TEST_FAIL_BEFORE_PUBLISH = '1'
        $output = (& 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -File $CaptureScript `
            -Runner $Runner -Dll $CorrectDll -InputRoot $InputRoot -OutDir $outDir 2>&1 | Out-String)
        if ($LASTEXITCODE -eq 0) {
            throw 'capture ignored the pre-publish fault injection'
        }
        if ($output -notmatch 'injected failure before atomic publish') {
            throw "capture failed for an unexpected reason: $output"
        }
        if (Test-Path -LiteralPath $outDir) {
            throw 'failed atomic publish left a final output directory'
        }
        $staging = @(Get-ChildItem -LiteralPath $caseFull -Directory -Filter '.artifact.stage-*' -ErrorAction SilentlyContinue)
        if ($staging.Count -ne 0) {
            throw "failed atomic publish left staging directories: $($staging.FullName -join ',')"
        }
    } finally {
        $env:VC_LEVEL_B_CAPTURE_TEST_FAIL_BEFORE_PUBLISH = 'preexisting-sentinel'
        if ((Test-Path -LiteralPath $caseFull) -and
            $caseFull.StartsWith($safePrefix, [StringComparison]::OrdinalIgnoreCase)) {
            Remove-Item -LiteralPath $caseFull -Recurse -Force
        }
    }
    if ($env:VC_LEVEL_B_CAPTURE_TEST_FAIL_BEFORE_PUBLISH -cne 'preexisting-sentinel') {
        throw 'atomic mutation did not restore the preexisting fault-injection environment sentinel'
    }
    Write-Output 'LEVEL_B_CAPTURE_ATOMIC_PUBLISH_MUTATION PASS injected_before_rename=RED final_absent=true staging_residue=0 preexisting_sentinel=restored'
} finally {
    if ($hadProcessFaultEnv) {
        $env:VC_LEVEL_B_CAPTURE_TEST_FAIL_BEFORE_PUBLISH = $processFaultEnv
    } else {
        Remove-Item Env:VC_LEVEL_B_CAPTURE_TEST_FAIL_BEFORE_PUBLISH -ErrorAction SilentlyContinue
    }
}
