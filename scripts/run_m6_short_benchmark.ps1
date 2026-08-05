param(
    [string]$Go = 'C:\Users\Administrator\AppData\Local\Temp\go1.26.5-portable\go\bin\go.exe',
    [string]$RootA = 'I:\tmp',
    [string]$RootB = 'H:\pik\00000000000',
    [ValidateRange(1, 10000)]
    [int]$MaxFilesPerRoot = 10000,
    [ValidateRange(1, 30)]
    [int]$MaximumMinutes = 30,
    [string]$EvidenceDir = ''
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
if (-not (Test-Path -LiteralPath $Go -PathType Leaf)) {
    throw "Go executable not found: $Go"
}
$resolvedA = (Resolve-Path -LiteralPath $RootA).Path.TrimEnd('\')
$resolvedB = (Resolve-Path -LiteralPath $RootB).Path.TrimEnd('\')
if (-not [string]::Equals($resolvedA, 'I:\tmp', [System.StringComparison]::OrdinalIgnoreCase)) {
    throw "RootA must resolve exactly to the approved read-only root I:\tmp"
}
if (-not [string]::Equals($resolvedB, 'H:\pik\00000000000', [System.StringComparison]::OrdinalIgnoreCase)) {
    throw "RootB must resolve exactly to the approved read-only root H:\pik\00000000000"
}
if ([string]::Equals(
    [System.IO.Path]::GetPathRoot($resolvedA),
    [System.IO.Path]::GetPathRoot($resolvedB),
    [System.StringComparison]::OrdinalIgnoreCase
)) {
    throw 'The two benchmark roots must be on different volumes.'
}

if (-not $EvidenceDir) {
    $runID = 'm6-short-{0}-{1}' -f ([DateTime]::UtcNow.ToString('yyyyMMdd-HHmmss-fff')), $PID
    $EvidenceDir = Join-Path $repo ".superpowers\evidence\$runID"
}
$evidence = [System.IO.Path]::GetFullPath($EvidenceDir)
New-Item -ItemType Directory -Force -Path $evidence | Out-Null
$bin = Join-Path $evidence 'benchio.exe'
& $Go -C $repo build -trimpath -o $bin ./cmd/benchio
if ($LASTEXITCODE -ne 0) {
    throw 'benchio build failed'
}

$before = [ordered]@{
    root_a_last_write_utc = (Get-Item -LiteralPath $resolvedA).LastWriteTimeUtc.ToString('o')
    root_b_last_write_utc = (Get-Item -LiteralPath $resolvedB).LastWriteTimeUtc.ToString('o')
}
$totalStarted = [DateTime]::UtcNow
$perRootSeconds = [Math]::Max(1, [Math]::Floor(($MaximumMinutes * 60 - 10) / 2))
$extensions = '.jpg,.jpeg,.png,.webp,.bmp,.gif,.tif,.tiff,.mp4,.mkv,.mov,.avi,.webm,.m4v'
$runs = @(
    [ordered]@{ Name = 'ssd-i'; Root = $resolvedA },
    [ordered]@{ Name = 'ssd-h'; Root = $resolvedB }
)
foreach ($run in $runs) {
    $json = Join-Path $evidence "$($run.Name).json"
    $stdout = Join-Path $evidence "$($run.Name).stdout.log"
    $stderr = Join-Path $evidence "$($run.Name).stderr.log"
    $arguments = @(
        '-root', $run.Root,
        '-ext', $extensions,
        '-max-files', $MaxFilesPerRoot,
        '-duration', "${perRootSeconds}s",
        '-streams', 6,
        '-block-kb', 4096,
        '-out', $json
    )
    $process = Start-Process -FilePath $bin -ArgumentList $arguments -NoNewWindow -Wait -PassThru `
        -RedirectStandardOutput $stdout -RedirectStandardError $stderr
    if ($process.ExitCode -ne 0) {
        throw "$($run.Name) benchio failed with exit code $($process.ExitCode)"
    }
}
$elapsed = [DateTime]::UtcNow - $totalStarted
if ($elapsed.TotalMinutes -gt $MaximumMinutes) {
    throw "Aggregate benchmark exceeded $MaximumMinutes minutes"
}
$after = [ordered]@{
    root_a_last_write_utc = (Get-Item -LiteralPath $resolvedA).LastWriteTimeUtc.ToString('o')
    root_b_last_write_utc = (Get-Item -LiteralPath $resolvedB).LastWriteTimeUtc.ToString('o')
}
$guard = [ordered]@{
    schema_version = 1
    kind = 'source_guard'
    passed = (
        $before.root_a_last_write_utc -eq $after.root_a_last_write_utc -and
        $before.root_b_last_write_utc -eq $after.root_b_last_write_utc
    )
    roots = @($resolvedA, $resolvedB)
    max_files_per_root = $MaxFilesPerRoot
    aggregate_elapsed_ms = [int64]$elapsed.TotalMilliseconds
    before = $before
    after = $after
}
$guard |
    ConvertTo-Json -Depth 8 |
    Set-Content -LiteralPath (Join-Path $evidence 'source-guard.json') -Encoding utf8NoBOM
if (-not $guard.passed) {
    throw 'A protected source root last-write timestamp changed during the read-only benchmark.'
}
Write-Output $evidence
