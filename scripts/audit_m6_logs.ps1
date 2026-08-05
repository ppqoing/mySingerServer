param(
    [Parameter(Mandatory = $true)]
    [string]$StatsLog,
    [Parameter(Mandatory = $true)]
    [string]$Output
)

$ErrorActionPreference = 'Stop'
$statsPath = (Resolve-Path -LiteralPath $StatsLog).Path
$records = [System.Collections.Generic.List[object]]::new()
$errors = [System.Collections.Generic.List[string]]::new()
$lineNumber = 0
$previousFilesDone = [int64]0

foreach ($line in [System.IO.File]::ReadLines($statsPath)) {
    $lineNumber++
    if ([string]::IsNullOrWhiteSpace($line)) {
        continue
    }
    if ($line -match '(?i)"(?:dsn|password|passwd|token|secret|credential)[^"]*"\s*:') {
        $errors.Add("line $lineNumber contains a forbidden credential-like key")
        continue
    }
    try {
        $record = $line | ConvertFrom-Json -Depth 32
    } catch {
        $errors.Add("line $lineNumber is invalid JSON")
        continue
    }
    foreach ($field in @('cpu', 'rss_bytes', 'heap_bytes', 'handles', 'pending_bytes', 'files_done', 'files_failed', 'crashes', 'read_p95_ms', 'decode_p95_ms')) {
        if ($null -ne $record.$field -and [double]$record.$field -lt 0) {
            $errors.Add("line $lineNumber has negative $field")
        }
    }
    if ([int64]$record.files_done -lt $previousFilesDone) {
        $errors.Add("line $lineNumber files_done is not monotonic")
    }
    $previousFilesDone = [int64]$record.files_done
    foreach ($disk in @($record.disks)) {
        if ([double]$disk.read_bps -lt 0 -or
            [double]$disk.busy_fraction -lt 0 -or
            [double]$disk.busy_fraction -gt 1 -or
            [int64]$disk.pending_bytes -lt 0) {
            $errors.Add("line $lineNumber has invalid disk metrics")
        }
    }
    $records.Add($record)
}

if ($records.Count -eq 0) {
    $errors.Add('stats log contains no JSON records')
}
$artifact = [ordered]@{
    schema_version = 1
    kind = 'log_audit'
    passed = ($errors.Count -eq 0)
    records = $records.Count
    errors = @($errors)
    source = [System.IO.Path]::GetFileName($statsPath)
}
$outputPath = [System.IO.Path]::GetFullPath($Output)
$parent = Split-Path -Parent $outputPath
New-Item -ItemType Directory -Force -Path $parent | Out-Null
$artifact |
    ConvertTo-Json -Depth 16 |
    Set-Content -LiteralPath $outputPath -Encoding utf8NoBOM
if ($errors.Count -ne 0) {
    $errors | ForEach-Object { Write-Error $_ }
    exit 1
}
Write-Host "M6 stats log audit passed: $($records.Count) records."
