param(
    [Parameter(Mandatory = $true)]
    [string]$Tool,
    [Parameter(Mandatory = $true)]
    [string]$TargetFile,
    [int]$DurationSeconds = 60,
    [string]$Output = ''
)

$ErrorActionPreference = 'Stop'
$toolPath = (Resolve-Path -LiteralPath $Tool).Path
$targetPath = (Resolve-Path -LiteralPath $TargetFile).Path
if (-not (Test-Path -LiteralPath $targetPath -PathType Leaf)) {
    throw "Read-only target file not found: $targetPath"
}
if ($DurationSeconds -lt 1 -or $DurationSeconds -gt 3600) {
    throw 'DurationSeconds must be in 1..3600'
}
$name = [System.IO.Path]::GetFileName($toolPath).ToLowerInvariant()
switch -Regex ($name) {
    '^diskspd(?:\.exe)?$' {
        $arguments = @(
            '-b1M',
            "-d$DurationSeconds",
            '-o1',
            '-t1',
            '-Sh',
            '-w0',
            $targetPath
        )
    }
    '^fio(?:\.exe)?$' {
        $arguments = @(
            '--readonly',
            '--name=m6-read-baseline',
            "--filename=$targetPath",
            '--rw=read',
            '--bs=1M',
            '--iodepth=1',
            "--runtime=$DurationSeconds",
            '--time_based',
            '--output-format=json'
        )
    }
    default {
        throw 'Tool must be an existing DiskSpd or fio executable; no installer is run.'
    }
}
if ($Output) {
    $outputPath = [System.IO.Path]::GetFullPath($Output)
    $parent = Split-Path -Parent $outputPath
    New-Item -ItemType Directory -Force -Path $parent | Out-Null
    & $toolPath @arguments 2>&1 | Set-Content -LiteralPath $outputPath -Encoding utf8NoBOM
} else {
    & $toolPath @arguments
}
if ($LASTEXITCODE -ne 0) {
    throw "Disk baseline tool exited with code $LASTEXITCODE"
}
