param(
    [Parameter(Mandatory = $true)] [string]$Runner,
    [Parameter(Mandatory = $true)] [string]$Dll,
    [Parameter(Mandatory = $true)] [string]$InputRoot,
    [Parameter(Mandatory = $true)] [string]$OutDir
)

$ErrorActionPreference = 'Stop'
$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))

if (-not ('VideoCoreCapture.NativeMethods' -as [type])) {
    Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
namespace VideoCoreCapture {
    public static class NativeMethods {
        [DllImport("kernel32.dll")]
        public static extern uint SetErrorMode(uint mode);
    }
}
'@
}

function Require-File([string]$Path, [string]$Role) {
    $full = [IO.Path]::GetFullPath($Path)
    if (-not (Test-Path -LiteralPath $full -PathType Leaf)) {
        throw "$Role is missing: $full"
    }
    return $full
}

function Require-RepoPath([string]$Path, [string]$Role) {
    $full = [IO.Path]::GetFullPath($Path)
    $prefix = $repoRoot.TrimEnd('\') + '\'
    if (-not $full.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) {
        throw "$Role must stay under repository root: $full"
    }
    return $full
}

function Sha256-File([string]$Path) {
    return (Get-FileHash -Algorithm SHA256 -LiteralPath $Path).Hash.ToLowerInvariant()
}

function Sha256-Text([string]$Text) {
    $bytes = [Text.UTF8Encoding]::new($false).GetBytes($Text)
    $hash = [Security.Cryptography.SHA256]::HashData($bytes)
    return [Convert]::ToHexString($hash).ToLowerInvariant()
}

function Require-PeDll([string]$Path) {
    $stream = [IO.File]::Open($Path, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read)
    $reader = [IO.BinaryReader]::new($stream)
    try {
        if ($stream.Length -lt 64 -or $reader.ReadUInt16() -ne 0x5A4D) {
            throw 'declared legacy DLL is not a PE DLL'
        }
        $stream.Position = 0x3C
        $peOffset = $reader.ReadUInt32()
        if ($peOffset -gt ($stream.Length - 24)) {
            throw 'declared legacy DLL is not a PE DLL'
        }
        $stream.Position = $peOffset
        if ($reader.ReadUInt32() -ne 0x00004550) {
            throw 'declared legacy DLL is not a PE DLL'
        }
        $stream.Position = $peOffset + 22
        $characteristics = $reader.ReadUInt16()
        if (($characteristics -band 0x2000) -eq 0) {
            throw 'declared legacy DLL is not a PE DLL'
        }
    } finally {
        $reader.Dispose()
        $stream.Dispose()
    }
}

function Invoke-IsolatedLegacyRunner([string]$RunnerPath, [string]$ImagePath, [string]$WorkingDirectory) {
    $startInfo = [Diagnostics.ProcessStartInfo]::new()
    $startInfo.FileName = $RunnerPath
    $startInfo.ArgumentList.Add('hash')
    $startInfo.ArgumentList.Add($ImagePath)
    $startInfo.WorkingDirectory = $WorkingDirectory
    $startInfo.UseShellExecute = $false
    $startInfo.CreateNoWindow = $true
    $startInfo.RedirectStandardOutput = $true
    $startInfo.RedirectStandardError = $true
    $startInfo.Environment['PATH'] = "$env:SystemRoot\System32;$env:SystemRoot"

    $process = [Diagnostics.Process]::new()
    $process.StartInfo = $startInfo
    # Suppress Windows loader/error-reporting UI for deliberately mismatched
    # pair tests.  The child inherits this mode at CreateProcess time.
    $oldErrorMode = [VideoCoreCapture.NativeMethods]::SetErrorMode(0x8003)
    try {
        try {
            if (-not $process.Start()) {
                throw 'CreateProcess returned false'
            }
        } catch {
            throw "isolated legacy runner failed to start: $($_.Exception.Message)"
        }
    } finally {
        [void][VideoCoreCapture.NativeMethods]::SetErrorMode($oldErrorMode)
    }

    try {
        $stdout = $process.StandardOutput.ReadToEndAsync()
        $stderr = $process.StandardError.ReadToEndAsync()
        if (-not $process.WaitForExit(10000)) {
            $process.Kill($true)
            $process.WaitForExit()
            throw 'isolated legacy runner failed: timed out after 10 seconds'
        }
        $text = (($stdout.GetAwaiter().GetResult()) + ($stderr.GetAwaiter().GetResult())).Trim()
        return [ordered]@{ ExitCode = $process.ExitCode; Output = $text }
    } finally {
        $process.Dispose()
    }
}

$runnerPath = Require-File $Runner 'legacy runner'
$dllPath = Require-File $Dll 'legacy DLL'
Require-PeDll $dllPath
$inputRootPath = Require-RepoPath $InputRoot 'input root'
if (-not (Test-Path -LiteralPath $inputRootPath -PathType Container)) {
    throw "input root is missing: $inputRootPath"
}
$outDirPath = Require-RepoPath $OutDir 'output directory'
$goldenPath = Join-Path $outDirPath 'legacy-golden.tsv'
$manifestPath = Join-Path $outDirPath 'manifest.json'
if (Test-Path -LiteralPath $outDirPath) {
    throw 'refusing to overwrite frozen Level B artifact'
}

$imagesDir = Join-Path $inputRootPath 'images'
$images = @(Get-ChildItem -LiteralPath $imagesDir -File | Sort-Object Name)
if ($images.Count -ne 20) {
    throw "expected exactly 20 Level B inputs, got $($images.Count)"
}

$rows = @()
$goldenLines = [Collections.Generic.List[string]]::new()
$isolationBase = Require-RepoPath (Join-Path $repoRoot '.tmp\videocore-levelb-capture') 'capture isolation base'
$isolationRoot = Join-Path $isolationBase ([guid]::NewGuid().ToString('N'))
$isolationFull = [IO.Path]::GetFullPath($isolationRoot)
$isolationPrefix = [IO.Path]::GetFullPath($isolationBase).TrimEnd('\') + '\'
if (-not $isolationFull.StartsWith($isolationPrefix, [StringComparison]::OrdinalIgnoreCase)) {
    throw "refusing unsafe capture isolation root: $isolationFull"
}

try {
    New-Item -ItemType Directory -Path $isolationFull | Out-Null
    $isolatedRunner = Join-Path $isolationFull 'legacy-level-b-runner.exe'
    $isolatedDll = Join-Path $isolationFull 'mediacore.dll'
    Copy-Item -LiteralPath $runnerPath -Destination $isolatedRunner
    Copy-Item -LiteralPath $dllPath -Destination $isolatedDll
    if ((Sha256-File $isolatedRunner) -ne (Sha256-File $runnerPath) -or
        (Sha256-File $isolatedDll) -ne (Sha256-File $dllPath)) {
        throw 'isolated legacy runner/DLL copy hash mismatch'
    }

    foreach ($image in $images) {
        $run = Invoke-IsolatedLegacyRunner $isolatedRunner $image.FullName $isolationFull
        if ($run.ExitCode -ne 0) {
            throw "isolated legacy runner failed for $($image.Name): exit=$($run.ExitCode) $($run.Output)"
        }
        $output = $run.Output
        if ($output -notmatch '^([0-9a-f]{64})\s+([0-9]+)\s+([0-9]+)\s+([0-9]+)$') {
            throw "isolated legacy runner output is malformed for $($image.Name): $output"
        }
        $pdq = $Matches[1]
        $quality = [int]$Matches[2]
        $width = [int]$Matches[3]
        $height = [int]$Matches[4]
        $canonicalResult = "$pdq`t$quality`t$width`t$height"
        $goldenLines.Add("$($image.Name)`t$canonicalResult")
        $rows += [ordered]@{
            filename = $image.Name
            inputSha256 = Sha256-File $image.FullName
            resultSha256 = Sha256-Text $canonicalResult
        }
    }
} finally {
    if ((Test-Path -LiteralPath $isolationFull) -and
        $isolationFull.StartsWith($isolationPrefix, [StringComparison]::OrdinalIgnoreCase)) {
        Remove-Item -LiteralPath $isolationFull -Recurse -Force
    }
}

$goldenText = ($goldenLines -join "`n") + "`n"
$copiedTsvPath = Require-File (Join-Path $inputRootPath 'level_b.tsv') 'copied legacy TSV'
$copied = @{}
foreach ($line in [IO.File]::ReadAllLines($copiedTsvPath)) {
    $fields = $line -split "`t"
    if ($fields.Count -ne 3) { throw "malformed copied TSV row: $line" }
    $filename = [IO.Path]::GetFileName($fields[0])
    $copied[$filename] = [ordered]@{ pdq = $fields[1]; quality = [int]$fields[2] }
}

$deltas = @()
foreach ($line in $goldenLines) {
    $fields = $line -split "`t"
    $filename = $fields[0]
    if (-not $copied.ContainsKey($filename)) {
        throw "copied TSV is missing $filename"
    }
    $old = $copied[$filename]
    if ($old.pdq -ne $fields[1] -or $old.quality -ne [int]$fields[2]) {
        $deltas += [ordered]@{
            filename = $filename
            copiedPdq = $old.pdq
            copiedQuality = $old.quality
            frozenPdq = $fields[1]
            frozenQuality = [int]$fields[2]
        }
    }
}
if ($deltas.Count -ne 9) {
    throw "expected exactly nine approved copied-TSV deltas, got $($deltas.Count)"
}

$outParent = Split-Path -Parent $outDirPath
$outLeaf = Split-Path -Leaf $outDirPath
New-Item -ItemType Directory -Force -Path $outParent | Out-Null
$stagePath = Join-Path $outParent (".$outLeaf.stage-" + [guid]::NewGuid().ToString('N'))
$stageFull = [IO.Path]::GetFullPath($stagePath)
$parentPrefix = [IO.Path]::GetFullPath($outParent).TrimEnd('\') + '\'
if (-not $stageFull.StartsWith($parentPrefix, [StringComparison]::OrdinalIgnoreCase)) {
    throw "refusing unsafe capture staging directory: $stageFull"
}
$stageGolden = Join-Path $stageFull 'legacy-golden.tsv'
$stageManifest = Join-Path $stageFull 'manifest.json'
try {
    New-Item -ItemType Directory -Path $stageFull | Out-Null
    [IO.File]::WriteAllText($stageGolden, $goldenText, [Text.UTF8Encoding]::new($false))
    $manifest = [ordered]@{
        schemaVersion = 1
        provenance = [ordered]@{
            runner = [ordered]@{
                path = [IO.Path]::GetRelativePath($repoRoot, $runnerPath).Replace('\', '/')
                sha256 = Sha256-File $runnerPath
            }
            dll = [ordered]@{
                path = [IO.Path]::GetRelativePath($repoRoot, $dllPath).Replace('\', '/')
                sha256 = Sha256-File $dllPath
            }
        }
        inputRoot = [IO.Path]::GetRelativePath($repoRoot, $inputRootPath).Replace('\', '/')
        copiedTsvSha256 = Sha256-File $copiedTsvPath
        golden = [ordered]@{
            path = 'legacy-golden.tsv'
            sha256 = Sha256-File $stageGolden
            rowCount = 20
        }
        rows = $rows
        approvedCopiedTsvDeltas = $deltas
    }
    $json = ($manifest | ConvertTo-Json -Depth 8) + "`n"
    [IO.File]::WriteAllText($stageManifest, $json, [Text.UTF8Encoding]::new($false))

    $stageFiles = @(Get-ChildItem -LiteralPath $stageFull -File | Sort-Object Name)
    if ($stageFiles.Count -ne 2 -or
        $stageFiles[0].Name -ne 'legacy-golden.tsv' -or
        $stageFiles[1].Name -ne 'manifest.json') {
        throw 'capture staging directory is incomplete'
    }
    $stagedManifest = Get-Content -Raw -LiteralPath $stageManifest | ConvertFrom-Json
    if ($stagedManifest.golden.rowCount -ne 20 -or
        $stagedManifest.rows.Count -ne 20 -or
        $stagedManifest.golden.sha256 -ne (Sha256-File $stageGolden)) {
        throw 'capture staging artifact failed internal validation'
    }
    if ($env:VC_LEVEL_B_CAPTURE_TEST_FAIL_BEFORE_PUBLISH -eq '1') {
        throw 'injected failure before atomic publish'
    }
    # Both artifact files become visible together.  Directory.Move is one
    # same-parent rename and refuses an existing destination.
    [IO.Directory]::Move($stageFull, $outDirPath)
} finally {
    if ((Test-Path -LiteralPath $stageFull) -and
        $stageFull.StartsWith($parentPrefix, [StringComparison]::OrdinalIgnoreCase)) {
        Remove-Item -LiteralPath $stageFull -Recurse -Force
    }
}

Write-Output "LEVEL_B_CAPTURE PASS rows=20 approved_deltas=9 golden_sha256=$(Sha256-File $goldenPath)"
