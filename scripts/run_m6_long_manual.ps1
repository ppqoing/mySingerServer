param(
    [Parameter(Mandatory = $true)]
    [string]$CorpusRoot,
    [Parameter(Mandatory = $true)]
    [switch]$ConfirmGeneratedCorpus,
    [Parameter(Mandatory = $true)]
    [string]$CommandJson,
    [string]$Go = 'C:\Users\Administrator\AppData\Local\Temp\go1.26.5-portable\go\bin\go.exe',
    [ValidateRange(1, 2000000)]
    [int]$Files = 1000000,
    [ValidateRange(1, 168)]
    [int]$DurationHours = 24,
    [string]$EvidenceDir = '',
    [switch]$CleanAfter
)

$ErrorActionPreference = 'Stop'
if (-not $ConfirmGeneratedCorpus) {
    throw 'Pass -ConfirmGeneratedCorpus to acknowledge that only the generated corpus may be mutated.'
}
$repo = Split-Path -Parent $PSScriptRoot
$root = [System.IO.Path]::GetFullPath($CorpusRoot).TrimEnd('\')
foreach ($protected in @('I:\tmp', 'H:\pik\00000000000')) {
    if ($root.Equals($protected, [System.StringComparison]::OrdinalIgnoreCase) -or
        $root.StartsWith($protected + '\', [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "Protected media root is read-only: $protected"
    }
}
if (-not $EvidenceDir) {
    $runID = 'm6-long-{0}-{1}' -f ([DateTime]::UtcNow.ToString('yyyyMMdd-HHmmss-fff')), $PID
    $EvidenceDir = Join-Path $repo ".superpowers\evidence\$runID"
} else {
    $runID = Split-Path -Leaf $EvidenceDir
}
$evidence = [System.IO.Path]::GetFullPath($EvidenceDir)
$bin = Join-Path $evidence 'bin'
New-Item -ItemType Directory -Force -Path $bin | Out-Null
foreach ($command in @('corpusgen', 'benchscreen', 'benchsync', 'soakrun', 'perfreport')) {
    & $Go -C $repo build -trimpath -o (Join-Path $bin "$command.exe") "./cmd/$command"
    if ($LASTEXITCODE -ne 0) {
        throw "$command build failed"
    }
}

$marker = Join-Path $root '.m6-corpus-owner.json'
if (-not (Test-Path -LiteralPath $marker -PathType Leaf)) {
    & (Join-Path $bin 'corpusgen.exe') `
        -root $root `
        -files $Files `
        -duplicates ([Math]::Min(10000, [Math]::Floor($Files / 2))) `
        -sparse 10 `
        -seed 20260729 `
        -run-id $runID |
        Set-Content -LiteralPath (Join-Path $evidence 'corpusgen.stdout.json') -Encoding utf8NoBOM
    if ($LASTEXITCODE -ne 0) {
        throw 'corpus generation failed'
    }
}
& (Join-Path $bin 'benchscreen.exe') `
    -rows 1000000 `
    -timeout 30m `
    -out (Join-Path $evidence 'screen-million.json') |
    Set-Content -LiteralPath (Join-Path $evidence 'screen-million.stdout.log') -Encoding utf8NoBOM
if ($LASTEXITCODE -ne 0) {
    throw 'million-row screen benchmark failed'
}
if ($env:M6_PG_DSN) {
    & (Join-Path $bin 'benchsync.exe') `
        -rows 1000000 `
        -batches '1000,5000,10000,50000' `
        -run-id $runID `
        -timeout 2h `
        -out (Join-Path $evidence 'sync-million.json') |
        Set-Content -LiteralPath (Join-Path $evidence 'sync-million.stdout.log') -Encoding utf8NoBOM
    if ($LASTEXITCODE -ne 0) {
        throw 'million-row sync benchmark failed'
    }
}
& (Join-Path $bin 'soakrun.exe') `
    -corpus-root $root `
    -duration "${DurationHours}h" `
    -output (Join-Path $evidence 'soak-children') `
    -command $CommandJson `
    -out (Join-Path $evidence 'soak.json') |
    Set-Content -LiteralPath (Join-Path $evidence 'soak.stdout.log') -Encoding utf8NoBOM
if ($LASTEXITCODE -ne 0) {
    throw 'long soak failed'
}
if ($CleanAfter) {
    $owner = Get-Content -Raw -LiteralPath $marker | ConvertFrom-Json
    & (Join-Path $bin 'corpusgen.exe') -root $root -run-id $owner.run_id -clean
    if ($LASTEXITCODE -ne 0) {
        throw 'owned corpus cleanup failed'
    }
}
Write-Output $evidence
