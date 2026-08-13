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

    git -C $fixture init --quiet
    if ($LASTEXITCODE -ne 0) { throw 'FIXTURE_GIT_INIT_FAILED' }
    Set-Content -LiteralPath (Join-Path $fixture '.gitignore') -Value @(
        '/.tmp/'
        '/.tmp-*/'
        '/.codex-temp/'
        '/.superpowers/'
        '/artifacts/'
        '/videocore/build/'
        '/mediacore/build/'
    )

    $dryRunOutput = @(
        & (Join-Path $PSScriptRoot 'clean-build-cache.ps1') `
            -RepositoryRoot $fixture
    )
    $expectedDryRunTargets = @(
        '.tmp', '.superpowers\tmp', '.superpowers\runtime',
        'artifacts\.gocache-live-status',
        'artifacts\.gocache-live-workerpool',
        'videocore\build', 'mediacore\build', '.tmp-review',
        '.codex-temp\gocache', '.codex-temp\stage-old'
    ) | ForEach-Object {
        'DRY-RUN target={0} files=1 logical-bytes=7' -f
            (Join-Path $fixture $_)
    }
    $expectedDryRunTargets += (
        'DRY-RUN target={0} files=1 logical-bytes=5' -f
        (Join-Path $fixture '.codex-temp\old.stdout.log')
    )
    $actualDryRunTargets = @($dryRunOutput | Select-Object -SkipLast 1)
    $dryRunDifference = @(Compare-Object -ReferenceObject $expectedDryRunTargets `
        -DifferenceObject $actualDryRunTargets)
    if ($dryRunDifference.Count -ne 0) {
        throw "DRY_RUN_TARGET_OUTPUT_MISMATCH detail=$($dryRunDifference -join ';')"
    }
    if ($dryRunOutput[-1] -ne
        'DRY-RUN TOTAL targets=11 files=11 logical-bytes=75') {
        throw "DRY_RUN_TOTAL_MISMATCH actual=$($dryRunOutput[-1])"
    }
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

    $applyOutput = @(
        & (Join-Path $PSScriptRoot 'clean-build-cache.ps1') `
            -RepositoryRoot $fixture -Apply
    )
    if ($applyOutput[-1] -ne
        'APPLY COMPLETE targets=11 released-logical-bytes=75') {
        throw "APPLY_TOTAL_MISMATCH actual=$($applyOutput[-1])"
    }
    $applyTargets = @($applyOutput | Where-Object {
        $_ -match '^APPLY target=.+ files=1 logical-bytes=(7|5)$'
    })
    if ($applyTargets.Count -ne 11) {
        throw "APPLY_TARGET_COUNT_MISMATCH actual=$($applyTargets.Count)"
    }
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
