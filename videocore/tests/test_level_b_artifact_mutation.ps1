param(
    [Parameter(Mandatory = $true)] [string]$Verifier,
    [Parameter(Mandatory = $true)] [string]$ArtifactDir,
    [Parameter(Mandatory = $true)] [string]$InputRoot,
    [Parameter(Mandatory = $true)] [string]$ExpectedManifestSha256,
    [Parameter(Mandatory = $true)] [string]$ExpectedGoldenSha256
)

$ErrorActionPreference = 'Stop'
$tempBase = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\') + '\'
$caseRoot = Join-Path $tempBase ("videocore-levelb-mutation-" + [guid]::NewGuid().ToString('N'))
$caseFull = [IO.Path]::GetFullPath($caseRoot)
if (-not $caseFull.StartsWith($tempBase, [StringComparison]::OrdinalIgnoreCase)) {
    throw "refusing unsafe mutation root: $caseFull"
}

function Sha256-File([string]$Path) {
    return (Get-FileHash -Algorithm SHA256 -LiteralPath $Path).Hash.ToLowerInvariant()
}

function Sha256-Text([string]$Text) {
    $bytes = [Text.UTF8Encoding]::new($false).GetBytes($Text)
    return [Convert]::ToHexString(
        [Security.Cryptography.SHA256]::HashData($bytes)).ToLowerInvariant()
}

function Copy-LevelBCase([string]$Destination) {
    $artifactCopy = Join-Path $Destination 'artifact'
    $inputCopy = Join-Path $Destination 'input'
    New-Item -ItemType Directory -Force -Path $artifactCopy,(Join-Path $inputCopy 'images') | Out-Null
    Copy-Item -LiteralPath (Join-Path $ArtifactDir 'manifest.json'),(Join-Path $ArtifactDir 'legacy-golden.tsv') -Destination $artifactCopy
    Copy-Item -LiteralPath (Join-Path $InputRoot 'level_b.tsv') -Destination $inputCopy
    Get-ChildItem -LiteralPath (Join-Path $InputRoot 'images') -File |
        ForEach-Object {
            Copy-Item -LiteralPath $_.FullName -Destination (Join-Path $inputCopy 'images')
        }
    return @($artifactCopy, $inputCopy)
}

function Invoke-ArtifactVerifier(
    [string]$Artifact,
    [string]$InputDirectory,
    [string]$ManifestPin = $ExpectedManifestSha256,
    [string]$GoldenPin = $ExpectedGoldenSha256) {
    $arguments = @('-NoProfile', '-File', $Verifier, '-ArtifactDir', $Artifact, '-InputRoot', $InputDirectory)
    $arguments += @(
        '-ExpectedManifestSha256', $ManifestPin,
        '-ExpectedGoldenSha256', $GoldenPin)
    return (& 'C:\Program Files\PowerShell\7\pwsh.exe' @arguments 2>&1 | Out-String)
}

try {
    $copies = Copy-LevelBCase (Join-Path $caseFull 'tenth-delta')
    $artifactCopy = $copies[0]
    $inputCopy = $copies[1]

    $manifestPath = Join-Path $artifactCopy 'manifest.json'
    $manifest = Get-Content -Raw -LiteralPath $manifestPath | ConvertFrom-Json
    $approved = @($manifest.approvedCopiedTsvDeltas.filename)
    $lines = [Collections.Generic.List[string]]::new()
    $mutated = $false
    foreach ($line in [IO.File]::ReadAllLines((Join-Path $inputCopy 'level_b.tsv'))) {
        $fields = $line -split "`t"
        $filename = [IO.Path]::GetFileName($fields[0])
        if (-not $mutated -and $filename -notin $approved) {
            $fields[1] = '0' * 64
            $mutated = $true
        }
        $lines.Add($fields -join "`t")
    }
    if (-not $mutated) { throw 'no non-approved copied TSV row available for mutation' }
    $tsvPath = Join-Path $inputCopy 'level_b.tsv'
    [IO.File]::WriteAllText($tsvPath, ($lines -join "`n") + "`n", [Text.UTF8Encoding]::new($false))
    $manifest.copiedTsvSha256 = (Get-FileHash -Algorithm SHA256 -LiteralPath $tsvPath).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText(
        $manifestPath,
        ($manifest | ConvertTo-Json -Depth 8) + "`n",
        [Text.UTF8Encoding]::new($false))

    # Pin the deliberately mutated manifest for this subcase so the verifier
    # reaches the independent approved-delta validation instead of stopping at
    # the outer immutable-artifact anchor.
    $output = Invoke-ArtifactVerifier $artifactCopy $inputCopy (Sha256-File $manifestPath) $ExpectedGoldenSha256
    if ($LASTEXITCODE -eq 0) {
        throw 'mutated tenth copied-TSV delta was incorrectly accepted'
    }
    if ($output -notmatch 'approved copied-TSV delta details mismatch') {
        throw "mutation failed for an unexpected reason: $output"
    }
    $copies = Copy-LevelBCase (Join-Path $caseFull 'self-resign')
    $artifactCopy = $copies[0]
    $inputCopy = $copies[1]
    $manifestPath = Join-Path $artifactCopy 'manifest.json'
    $goldenPath = Join-Path $artifactCopy 'legacy-golden.tsv'
    $manifest = Get-Content -Raw -LiteralPath $manifestPath | ConvertFrom-Json
    $goldenLines = [Collections.Generic.List[string]]::new()
    $mutatedFilename = @($manifest.approvedCopiedTsvDeltas)[0].filename
    $mutatedFrozenPdq = $null
    foreach ($line in [IO.File]::ReadAllLines($goldenPath)) {
        $fields = $line -split "`t"
        if ($fields[0] -eq $mutatedFilename) {
            $fields[1] = '0' + $fields[1].Substring(1)
            $mutatedFrozenPdq = $fields[1]
        }
        $goldenLines.Add($fields -join "`t")
    }
    if ($null -eq $mutatedFrozenPdq) { throw 'approved row missing from self-resign golden copy' }
    [IO.File]::WriteAllText(
        $goldenPath,
        ($goldenLines -join "`n") + "`n",
        [Text.UTF8Encoding]::new($false))
    $mutatedLine = @($goldenLines | Where-Object { ($_ -split "`t")[0] -eq $mutatedFilename })[0]
    $fields = $mutatedLine -split "`t"
    $canonical = "$($fields[1])`t$($fields[2])`t$($fields[3])`t$($fields[4])"
    $manifestRow = @($manifest.rows | Where-Object filename -eq $mutatedFilename)[0]
    $manifestRow.resultSha256 = Sha256-Text $canonical
    $manifest.golden.sha256 = Sha256-File $goldenPath

    $copiedPath = Join-Path $inputCopy 'level_b.tsv'
    $copiedLines = [Collections.Generic.List[string]]::new()
    $mutatedCopiedPdq = $null
    foreach ($line in [IO.File]::ReadAllLines($copiedPath)) {
        $copiedFields = $line -split "`t"
        if ([IO.Path]::GetFileName($copiedFields[0]) -eq $mutatedFilename) {
            $copiedFields[1] = '1' + $copiedFields[1].Substring(1)
            $mutatedCopiedPdq = $copiedFields[1]
        }
        $copiedLines.Add($copiedFields -join "`t")
    }
    if ($null -eq $mutatedCopiedPdq) { throw 'approved row missing from self-resign copied TSV' }
    [IO.File]::WriteAllText(
        $copiedPath,
        ($copiedLines -join "`n") + "`n",
        [Text.UTF8Encoding]::new($false))
    $manifest.copiedTsvSha256 = Sha256-File $copiedPath
    $approvedDelta = @($manifest.approvedCopiedTsvDeltas | Where-Object filename -eq $mutatedFilename)[0]
    $approvedDelta.copiedPdq = $mutatedCopiedPdq
    $approvedDelta.frozenPdq = $mutatedFrozenPdq
    [IO.File]::WriteAllText(
        $manifestPath,
        ($manifest | ConvertTo-Json -Depth 8) + "`n",
        [Text.UTF8Encoding]::new($false))

    $output = Invoke-ArtifactVerifier $artifactCopy $inputCopy
    if ($LASTEXITCODE -eq 0) {
        throw 'self-consistently re-signed legacy artifact was incorrectly accepted'
    }
    if ($output -notmatch 'external manifest hash mismatch') {
        throw "self-resign mutation failed for an unexpected reason: $output"
    }
    Write-Output 'LEVEL_B_MUTATION PASS tenth_delta=RED self_resign=RED artifact_read_only=true'
} finally {
    if ((Test-Path -LiteralPath $caseFull) -and
        $caseFull.StartsWith($tempBase, [StringComparison]::OrdinalIgnoreCase)) {
        Remove-Item -LiteralPath $caseFull -Recurse -Force
    }
}
