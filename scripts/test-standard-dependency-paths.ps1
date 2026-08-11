param()
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'standard-dependency-paths.ps1')

function Assert-Equal($Actual, $Expected, $Label) {
    if ($Actual -cne $Expected) {
        throw "ASSERT_EQUAL_FAILED label=$Label actual=$Actual expected=$Expected"
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
} finally {
    if (Test-Path -LiteralPath $fixture) {
        Remove-Item -LiteralPath $fixture -Recurse -Force
    }
}
Write-Output 'STANDARD DEPENDENCY PATH TEST PASS'
