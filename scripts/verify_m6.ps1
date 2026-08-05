param(
    [string]$Go = 'C:\Users\Administrator\AppData\Local\Temp\go1.26.5-portable\go\bin\go.exe',
    [string]$CC = 'C:\Users\Administrator\AppData\Local\Temp\winlibs-gcc\mingw64\bin\gcc.exe',
    [string]$Windres = 'C:\Users\Administrator\AppData\Local\Temp\winlibs-gcc\mingw64\bin\windres.exe',
    [string]$EvidenceDir = ''
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
if (-not (Test-Path -LiteralPath $Go -PathType Leaf)) {
    throw "Go executable not found: $Go"
}
if (-not (Test-Path -LiteralPath $CC -PathType Leaf)) {
    throw "C compiler not found: $CC"
}
if (-not (Test-Path -LiteralPath $Windres -PathType Leaf)) {
    throw "windres not found: $Windres"
}

$packages = @(
    './internal/stats',
    './internal/m6bench',
    './internal/agent',
    './internal/config',
    './internal/proto',
    './internal/firstscreen',
    './cmd/agent',
    './cmd/benchio',
    './cmd/benchsync',
    './cmd/benchscreen',
    './cmd/corpusgen',
    './cmd/soakrun',
    './cmd/perfreport'
)

& $Go -C $repo test -count=1 @packages
if ($LASTEXITCODE -ne 0) {
    throw 'M6 focused Go tests failed'
}
$oldM5CC = $env:M5_CC
$oldM5Windres = $env:M5_WINDRES
try {
    $env:M5_CC = $CC
    $env:M5_WINDRES = $Windres
    & $Go -C $repo test -count=1 ./...
    if ($LASTEXITCODE -ne 0) {
        throw 'Full Go tests failed'
    }
} finally {
    if ($null -eq $oldM5CC) {
        Remove-Item Env:M5_CC -ErrorAction SilentlyContinue
    } else {
        $env:M5_CC = $oldM5CC
    }
    if ($null -eq $oldM5Windres) {
        Remove-Item Env:M5_WINDRES -ErrorAction SilentlyContinue
    } else {
        $env:M5_WINDRES = $oldM5Windres
    }
}
& $Go -C $repo build ./cmd/agent ./cmd/benchio ./cmd/benchsync ./cmd/benchscreen ./cmd/corpusgen ./cmd/soakrun ./cmd/perfreport
if ($LASTEXITCODE -ne 0) {
    throw 'M6 command build failed'
}

$parseErrors = @()
$scripts = @(
    'audit_m6_logs.ps1',
    'disk_baseline.ps1',
    'run_m6_short_benchmark.ps1',
    'run_m6_long_manual.ps1',
    'verify_m6.ps1'
)
foreach ($name in $scripts) {
    $path = Join-Path $PSScriptRoot $name
    [System.Management.Automation.Language.Parser]::ParseFile(
        $path,
        [ref]$null,
        [ref]$parseErrors
    ) | Out-Null
}
if ($parseErrors.Count -ne 0) {
    $parseErrors | ForEach-Object { Write-Error $_.ToString() }
    throw 'M6 PowerShell parser check failed'
}

if ($EvidenceDir) {
    $evidence = [System.IO.Path]::GetFullPath($EvidenceDir)
    New-Item -ItemType Directory -Force -Path $evidence | Out-Null
    $tooling = [ordered]@{
        schema_version = 1
        kind = 'tooling'
        passed = $true
        go_version = ((& $Go version) -join ' ')
        focused_packages = $packages.Count
        scripts_parsed = $scripts.Count
        timestamp = [DateTime]::UtcNow.ToString('o')
    }
    $tooling |
        ConvertTo-Json -Depth 8 |
        Set-Content -LiteralPath (Join-Path $evidence 'tooling.json') -Encoding utf8NoBOM
}

Write-Host 'M6 focused tests, full tests, command builds, and script parser checks passed.'
