param(
    [Parameter(Mandatory = $true)] [string]$ArtifactDir,
    [Parameter(Mandatory = $true)] [string]$InputRoot,
    [Parameter(Mandatory = $true)] [string]$ExpectedManifestSha256,
    [Parameter(Mandatory = $true)] [string]$ExpectedGoldenSha256
)

$ErrorActionPreference = 'Stop'

function Sha256-File([string]$Path) {
    return (Get-FileHash -Algorithm SHA256 -LiteralPath $Path).Hash.ToLowerInvariant()
}

function Sha256-Text([string]$Text) {
    $bytes = [Text.UTF8Encoding]::new($false).GetBytes($Text)
    return [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData($bytes)).ToLowerInvariant()
}

$artifact = [IO.Path]::GetFullPath($ArtifactDir)
$input = [IO.Path]::GetFullPath($InputRoot)
$manifestPath = Join-Path $artifact 'manifest.json'
$goldenPath = Join-Path $artifact 'legacy-golden.tsv'
if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf) -or
    -not (Test-Path -LiteralPath $goldenPath -PathType Leaf)) {
    throw 'legacy Level B manifest/golden artifact is missing'
}
if ($ExpectedManifestSha256 -notmatch '^[0-9a-fA-F]{64}$' -or
    $ExpectedGoldenSha256 -notmatch '^[0-9a-fA-F]{64}$') {
    throw 'external Level B pins must be exact SHA-256 values'
}
if ((Sha256-File $manifestPath) -ne $ExpectedManifestSha256.ToLowerInvariant()) {
    throw 'external manifest hash mismatch'
}
if ((Sha256-File $goldenPath) -ne $ExpectedGoldenSha256.ToLowerInvariant()) {
    throw 'external golden hash mismatch'
}
$manifest = Get-Content -Raw -LiteralPath $manifestPath | ConvertFrom-Json
if ($manifest.schemaVersion -ne 1 -or $manifest.golden.rowCount -ne 20) {
    throw 'legacy Level B manifest schema/count is invalid'
}
if ((Sha256-File $goldenPath) -ne $manifest.golden.sha256) {
    throw 'legacy Level B golden hash mismatch'
}
if ((Sha256-File (Join-Path $input 'level_b.tsv')) -ne $manifest.copiedTsvSha256) {
    throw 'copied Level B TSV hash mismatch'
}
$golden = @{}
foreach ($line in [IO.File]::ReadAllLines($goldenPath)) {
    $fields = $line -split "`t"
    if ($fields.Count -ne 5 -or $fields[1] -notmatch '^[0-9a-f]{64}$') {
        throw "malformed frozen golden row: $line"
    }
    if ($golden.ContainsKey($fields[0])) { throw "duplicate frozen row: $($fields[0])" }
    $golden[$fields[0]] = [ordered]@{
        pdq = $fields[1]
        quality = [int]$fields[2]
        width = [int]$fields[3]
        height = [int]$fields[4]
    }
}
if ($golden.Count -ne 20 -or @($manifest.rows).Count -ne 20) {
    throw 'legacy Level B artifact must contain exactly 20 rows'
}

foreach ($row in $manifest.rows) {
    if (-not $golden.ContainsKey($row.filename)) { throw "manifest row missing from golden: $($row.filename)" }
    $imagePath = Join-Path (Join-Path $input 'images') $row.filename
    if ((Sha256-File $imagePath) -ne $row.inputSha256) { throw "input hash mismatch: $($row.filename)" }
    $value = $golden[$row.filename]
    $canonical = "$($value.pdq)`t$($value.quality)`t$($value.width)`t$($value.height)"
    if ((Sha256-Text $canonical) -ne $row.resultSha256) { throw "result hash mismatch: $($row.filename)" }
}

$copied = @{}
foreach ($line in [IO.File]::ReadAllLines((Join-Path $input 'level_b.tsv'))) {
    $fields = $line -split "`t"
    if ($fields.Count -ne 3) { throw "malformed copied TSV row: $line" }
    $copied[[IO.Path]::GetFileName($fields[0])] = [ordered]@{ pdq = $fields[1]; quality = [int]$fields[2] }
}
$actualDeltas = @()
foreach ($filename in @($golden.Keys | Sort-Object)) {
    $old = $copied[$filename]
    $frozen = $golden[$filename]
    if ($null -eq $old) { throw "copied TSV row missing: $filename" }
    if ($old.pdq -ne $frozen.pdq -or $old.quality -ne $frozen.quality) {
        $actualDeltas += $filename
        $approved = @($manifest.approvedCopiedTsvDeltas | Where-Object filename -eq $filename)
        if ($approved.Count -ne 1 -or
            $approved[0].copiedPdq -ne $old.pdq -or
            $approved[0].copiedQuality -ne $old.quality -or
            $approved[0].frozenPdq -ne $frozen.pdq -or
            $approved[0].frozenQuality -ne $frozen.quality) {
            throw "approved copied-TSV delta details mismatch: $filename"
        }
    }
}
$approvedNames = @($manifest.approvedCopiedTsvDeltas.filename | Sort-Object)
if ($actualDeltas.Count -ne 9 -or $approvedNames.Count -ne 9 -or
    (Compare-Object $actualDeltas $approvedNames)) {
    throw "copied TSV must differ from frozen legacy golden in exactly nine approved rows"
}

Write-Output "LEVEL_B_VERIFY PASS rows=20 approved_deltas=9 anchors=manifest,golden verified=artifact,input,result,golden,approved-delta"
