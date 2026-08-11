param()

$ErrorActionPreference = 'Stop'
$fixture = Join-Path ([IO.Path]::GetTempPath()) `
    ('mysinger-clean-cache-' + [Guid]::NewGuid().ToString('N'))

try {
    $removeDirs = @(
        '.tmp\go-cache', '.tmp-review\cache',
        '.codex-temp\gocache', '.codex-temp\stage-old',
        '.superpowers\tmp\cache', '.superpowers\runtime\old',
        'artifacts\.gocache-live-status\cache',
        'artifacts\.gocache-live-workerpool\cache',
        'videocore\build\Release', 'mediacore\build\Release'
    )
    $removeFiles = @('.codex-temp\old.stdout.log')
    $keepFiles = @(
        '.codex-temp\keep.ps1',
        '.codex-temp\internal\keep.go',
        '.codex-temp\protobuf-tools\keep.exe',
        '.codex-temp\fixture-probe\keep.txt',
        '.superpowers\evidence\keep.bin',
        '.worktrees\keep\source.go',
        'artifacts\releases\keep.zip',
        'webui\node_modules\keep.js',
        'nodetray\frontend\node_modules\keep.js',
        'third_party\ffmpeg\keep.dll'
    )

    foreach ($dir in $removeDirs) {
        New-Item -ItemType Directory -Force -Path (Join-Path $fixture $dir) |
            Out-Null
        Set-Content -LiteralPath (Join-Path $fixture $dir 'cache.bin') `
            -Value 'cache'
    }
    foreach ($file in $keepFiles) {
        New-Item -ItemType Directory -Force `
            -Path (Split-Path (Join-Path $fixture $file)) |
            Out-Null
        Set-Content -LiteralPath (Join-Path $fixture $file) -Value 'keep'
    }
    foreach ($file in $removeFiles) {
        New-Item -ItemType Directory -Force `
            -Path (Split-Path (Join-Path $fixture $file)) |
            Out-Null
        Set-Content -LiteralPath (Join-Path $fixture $file) -Value 'log'
    }

    & (Join-Path $PSScriptRoot 'clean-build-cache.ps1') `
        -RepositoryRoot $fixture
    foreach ($dir in $removeDirs) {
        if (-not (Test-Path -LiteralPath (Join-Path $fixture $dir))) {
            throw "DRY_RUN_REMOVED_TARGET path=$dir"
        }
    }
    foreach ($file in $removeFiles) {
        if (-not (Test-Path -LiteralPath (Join-Path $fixture $file))) {
            throw "DRY_RUN_REMOVED_TARGET path=$file"
        }
    }

    & (Join-Path $PSScriptRoot 'clean-build-cache.ps1') `
        -RepositoryRoot $fixture -Apply
    foreach ($file in $keepFiles) {
        if (-not (Test-Path -LiteralPath (Join-Path $fixture $file))) {
            throw "PROTECTED_FILE_REMOVED path=$file"
        }
    }
    foreach ($dir in $removeDirs) {
        if (Test-Path -LiteralPath (Join-Path $fixture $dir)) {
            throw "CACHE_TARGET_REMAINS path=$dir"
        }
    }
    foreach ($file in $removeFiles) {
        if (Test-Path -LiteralPath (Join-Path $fixture $file)) {
            throw "CACHE_TARGET_REMAINS path=$file"
        }
    }
} finally {
    if (Test-Path -LiteralPath $fixture) {
        Remove-Item -LiteralPath $fixture -Recurse -Force
    }
}

Write-Output 'BUILD CACHE CLEANER TEST PASS'
