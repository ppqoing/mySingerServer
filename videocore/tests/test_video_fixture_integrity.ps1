param(
    [Parameter(Mandatory = $true)][string]$SourceRoot,
    [Parameter(Mandatory = $true)][string]$CopyRoot
)

$ErrorActionPreference = 'Stop'

$expectedNames = @(
    'audio-only.m4a',
    'corrupt-packet.ts',
    'h264-bframes.mp4',
    'h264-rotate90.mp4',
    'h264-sar-4x3.mp4',
    'h264-short.mp4',
    'h264-standard.mp4',
    'truncated-container.mp4'
)

function Get-Sha256([string]$Path) {
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Test-FixtureIntegrity([string]$CandidateRoot) {
    $manifestPath = Join-Path $CandidateRoot 'manifest.json'
    $manifest = Get-Content -Raw -LiteralPath $manifestPath | ConvertFrom-Json
    if ($manifest.schemaVersion -ne 1) { throw 'unexpected manifest schemaVersion' }
    if ($manifest.sourceRoot -cne 'testdata/videocore/compat/videos') {
        throw 'manifest sourceRoot is outside the one allowed repository root'
    }
    $manifestNames = @($manifest.fixtures | ForEach-Object { $_.path } | Sort-Object)
    if (($manifestNames -join '|') -cne ($expectedNames -join '|')) {
        throw "manifest fixture set is not exact: $($manifestNames -join ',')"
    }
    $copyNames = @(Get-ChildItem -LiteralPath $CandidateRoot -File |
        Where-Object Name -ne 'manifest.json' |
        ForEach-Object Name | Sort-Object)
    if (($copyNames -join '|') -cne ($expectedNames -join '|')) {
        throw "copied fixture set is not exact: $($copyNames -join ',')"
    }
    foreach ($entry in $manifest.fixtures) {
        $source = Join-Path $SourceRoot $entry.path
        $target = Join-Path $CandidateRoot $entry.path
        $sourceHash = Get-Sha256 $source
        $targetHash = Get-Sha256 $target
        $manifestHash = [string]$entry.sha256
        if ($sourceHash -cne $targetHash -or $sourceHash -cne $manifestHash) {
            throw "$($entry.path) SHA-256 mismatch source=$sourceHash target=$targetHash manifest=$manifestHash"
        }
    }
}

$resolvedSource = (Resolve-Path -LiteralPath $SourceRoot).Path
$allowedSuffix = [IO.Path]::Combine('testdata', 'videocore', 'compat', 'videos')
if (-not $resolvedSource.EndsWith($allowedSuffix, [StringComparison]::OrdinalIgnoreCase)) {
    throw "source root is not the allowed repository fixture root: $resolvedSource"
}
Test-FixtureIntegrity (Resolve-Path -LiteralPath $CopyRoot).Path

$mutationRoot = Join-Path ([IO.Path]::GetTempPath()) ('videocore-fixture-mutation-' + [Guid]::NewGuid().ToString('N'))
try {
    New-Item -ItemType Directory -Path $mutationRoot | Out-Null
    Copy-Item -LiteralPath (Join-Path $CopyRoot 'manifest.json') -Destination $mutationRoot
    foreach ($name in $expectedNames) {
        Copy-Item -LiteralPath (Join-Path $CopyRoot $name) -Destination $mutationRoot
    }
    $mutatedPath = Join-Path $mutationRoot 'h264-standard.mp4'
    $bytes = [IO.File]::ReadAllBytes($mutatedPath)
    $bytes[[Math]::Floor($bytes.Length / 2)] = $bytes[[Math]::Floor($bytes.Length / 2)] -bxor 0x01
    [IO.File]::WriteAllBytes($mutatedPath, $bytes)
    $mutationDetected = $false
    try {
        Test-FixtureIntegrity $mutationRoot
    } catch {
        $mutationDetected = $true
        Write-Host "fixture byte mutation RED detected: $($_.Exception.Message)"
    }
    if (-not $mutationDetected) { throw 'fixture byte mutation was not detected' }
} finally {
    if (Test-Path -LiteralPath $mutationRoot) {
        Remove-Item -LiteralPath $mutationRoot -Recurse -Force
    }
}

Write-Host 'fixture integrity passed: exact set and source=target=manifest SHA-256'
