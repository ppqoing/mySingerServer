param()
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'standard-dependency-paths.ps1')

function Assert-Equal($Actual, $Expected, $Label) {
    if ($Actual -cne $Expected) {
        throw "ASSERT_EQUAL_FAILED label=$Label actual=$Actual expected=$Expected"
    }
}

function Assert-EntryRejectsRepositoryCache {
    param(
        [string]$Entry,
        [string[]]$EntryArguments,
        [string]$ExpectedLabel,
        [string]$FakeToolDirectory = ''
    )

    $oldPath = $env:PATH
    try {
        if ($FakeToolDirectory) {
            $env:PATH = "$FakeToolDirectory;$oldPath"
        }
        $output = (& (Join-Path $PSHOME 'pwsh.exe') -NoProfile -File $Entry `
            @EntryArguments 2>&1 | Out-String)
        $exitCode = $LASTEXITCODE
    } finally {
        $env:PATH = $oldPath
    }

    if ($exitCode -eq 0) {
        throw "STANDARD_PATH_ENTRY_REJECTION_MISSING entry=$Entry exit=0"
    }
    if ($output -notmatch "DEPENDENCY_PATH_INSIDE_REPOSITORY label=$ExpectedLabel") {
        throw "STANDARD_PATH_ENTRY_REJECTION_MISSING entry=$Entry label=$ExpectedLabel output=$output"
    }
    if ($output -match 'COSTLY_FAKE_(GO|NPM|NODE|CMAKE)_CALLED') {
        throw "STANDARD_PATH_ENTRY_DID_NOT_FAIL_EARLY entry=$Entry output=$output"
    }
}

$fixture = Join-Path ([IO.Path]::GetTempPath()) `
    ('mysinger-standard-paths-' + [Guid]::NewGuid().ToString('N'))
$repo = Join-Path $fixture 'repository'
$shared = Join-Path $fixture 'shared'
$vcpkg = Join-Path $shared 'vcpkg'
try {
    New-Item -ItemType Directory -Force -Path $repo | Out-Null
    New-Item -ItemType Directory -Force -Path `
        (Join-Path $shared 'go'), `
        (Join-Path $shared 'go-mod'), `
        (Join-Path $shared 'go-build'), `
        (Join-Path $shared 'npm-cache'), `
        (Join-Path $vcpkg 'installed'), `
        (Join-Path $vcpkg 'downloads'), `
        (Join-Path $vcpkg 'scripts\buildsystems') | Out-Null
    Set-Content -LiteralPath (Join-Path $vcpkg 'vcpkg.exe') -Value 'fixture'
    Set-Content -LiteralPath `
        (Join-Path $vcpkg 'scripts\buildsystems\vcpkg.cmake') -Value 'fixture'

    $go = Join-Path $fixture 'go.cmd'
    $npm = Join-Path $fixture 'npm.cmd'
    Set-Content -LiteralPath $go -Value @(
        '@echo off',
        ('echo ' + (Join-Path $shared 'go')),
        ('echo ' + (Join-Path $shared 'go-mod')),
        ('echo ' + (Join-Path $shared 'go-build'))
    )
    Set-Content -LiteralPath $npm -Value @(
        '@echo off',
        ('echo ' + (Join-Path $shared 'npm-cache'))
    )

    $actual = Resolve-StandardDependencyPaths -RepositoryRoot $repo `
        -GoExecutable $go -NpmExecutable $npm -VcpkgRoot $vcpkg
    Assert-Equal $actual.GoModCache (Join-Path $shared 'go-mod') 'GOMODCACHE'
    Assert-Equal $actual.NpmCache (Join-Path $shared 'npm-cache') 'npm cache'
    Assert-Equal $actual.VcpkgInstalled (Join-Path $vcpkg 'installed') 'vcpkg installed'

    Set-Content -LiteralPath $go -Value @(
        '@echo off',
        ('echo ' + (Join-Path $shared 'go')),
        ('echo ' + (Join-Path $repo '.tmp\gomodcache')),
        ('echo ' + (Join-Path $shared 'go-build'))
    )
    try {
        Resolve-StandardDependencyPaths -RepositoryRoot $repo `
            -GoExecutable $go | Out-Null
        throw 'EXPECTED_REPOSITORY_CACHE_REJECTION'
    } catch {
        if ($_.Exception.Message -notmatch 'DEPENDENCY_PATH_INSIDE_REPOSITORY') {
            throw
        }
    }

    # These entry tests fail if an entry point stops invoking
    # Resolve-StandardDependencyPaths before it starts its build work.
    $projectRepo = Split-Path -Parent $PSScriptRoot
    $projectCache = Join-Path $projectRepo '.tmp\standard-path-entry-cache'
    $fakeTools = Join-Path $fixture 'entry-tools'
    New-Item -ItemType Directory -Force -Path $fakeTools | Out-Null
    $entryGo = Join-Path $fakeTools 'go.cmd'
    $entryNpm = Join-Path $fakeTools 'npm.cmd'
    $entryNode = Join-Path $fakeTools 'node.cmd'
    $fakeCmake = Join-Path $fakeTools 'cmake.cmd'
    Set-Content -LiteralPath $entryGo -Value @(
        '@echo off',
        'if /I "%1"=="env" (',
        ('  echo ' + (Join-Path $shared 'go')),
        ('  echo ' + $projectCache),
        ('  echo ' + (Join-Path $shared 'go-build')),
        '  exit /b 0',
        ')',
        'echo COSTLY_FAKE_GO_CALLED',
        'exit /b 17'
    )
    Set-Content -LiteralPath $entryNpm -Value @(
        '@echo off',
        'if /I "%1"=="config" (',
        ('  echo ' + $projectCache),
        '  exit /b 0',
        ')',
        'echo COSTLY_FAKE_NPM_CALLED',
        'exit /b 17'
    )
    Set-Content -LiteralPath $entryNode -Value @(
        '@echo off',
        'echo COSTLY_FAKE_NODE_CALLED',
        'exit /b 17'
    )
    Set-Content -LiteralPath $fakeCmake -Value @(
        '@echo off',
        'echo COSTLY_FAKE_CMAKE_CALLED',
        'exit /b 0'
    )
    Set-Content -LiteralPath (Join-Path $fakeTools 'ctest.exe') -Value 'fixture'

    Assert-EntryRejectsRepositoryCache `
        -Entry (Join-Path $PSScriptRoot 'build.ps1') `
        -EntryArguments @('-Go', $entryGo, '-VcpkgRoot', $vcpkg, '-Cmake', $fakeCmake, '-MediacoreOnly', '-OutDir', '.') `
        -ExpectedLabel 'GOMODCACHE'
    Assert-EntryRejectsRepositoryCache `
        -Entry (Join-Path $PSScriptRoot 'build-web.ps1') `
        -EntryArguments @() -ExpectedLabel 'npm-cache' -FakeToolDirectory $fakeTools
    Assert-EntryRejectsRepositoryCache `
        -Entry (Join-Path $PSScriptRoot 'build-nodetray.ps1') `
        -EntryArguments @('-Go', $entryGo, '-Npm', $entryNpm, '-OutDir', (Join-Path $fixture 'nodetray-stage')) `
        -ExpectedLabel 'GOMODCACHE'
    Assert-EntryRejectsRepositoryCache `
        -Entry (Join-Path $PSScriptRoot 'test-cgo.ps1') `
        -EntryArguments @('-Go', $entryGo) -ExpectedLabel 'GOMODCACHE'
} finally {
    if (Test-Path -LiteralPath $fixture) {
        Remove-Item -LiteralPath $fixture -Recurse -Force
    }
}
Write-Output 'STANDARD DEPENDENCY PATH TEST PASS'
