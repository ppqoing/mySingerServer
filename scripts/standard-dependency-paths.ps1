$ErrorActionPreference = 'Stop'

function Resolve-AbsoluteDependencyPath {
    param([string]$Path, [string]$Label, [string]$RepositoryRoot)
    if ([string]::IsNullOrWhiteSpace($Path) -or
        -not [IO.Path]::IsPathRooted($Path)) {
        throw "DEPENDENCY_PATH_NOT_ABSOLUTE label=$Label path=$Path"
    }
    $resolved = [IO.Path]::GetFullPath($Path).TrimEnd('\')
    $repo = [IO.Path]::GetFullPath($RepositoryRoot).TrimEnd('\')
    if ($resolved -eq $repo -or $resolved.StartsWith(
            $repo + '\', [StringComparison]::OrdinalIgnoreCase)) {
        throw "DEPENDENCY_PATH_INSIDE_REPOSITORY label=$Label path=$resolved"
    }
    return $resolved
}

function Resolve-StandardDependencyPaths {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$RepositoryRoot,
        [string]$GoExecutable = '',
        [string]$NpmExecutable = '',
        [string]$VcpkgRoot = ''
    )
    $result = [ordered]@{
        GoPath = $null; GoModCache = $null; GoBuildCache = $null
        NpmCache = $null
        VcpkgRoot = $null; VcpkgInstalled = $null; VcpkgDownloads = $null
    }
    if ($GoExecutable) {
        $goPaths = @(& $GoExecutable env GOPATH GOMODCACHE GOCACHE 2>&1 |
            ForEach-Object { ([string]$_).Trim() } | Where-Object { $_ })
        if ($LASTEXITCODE -ne 0 -or $goPaths.Count -ne 3) {
            throw 'GO_STANDARD_PATH_RESOLVE_FAILED'
        }
        $result.GoPath = Resolve-AbsoluteDependencyPath $goPaths[0] 'GOPATH' $RepositoryRoot
        $result.GoModCache = Resolve-AbsoluteDependencyPath $goPaths[1] 'GOMODCACHE' $RepositoryRoot
        $result.GoBuildCache = Resolve-AbsoluteDependencyPath $goPaths[2] 'GOCACHE' $RepositoryRoot
    }
    if ($NpmExecutable) {
        $npmCache = (& $NpmExecutable config get cache 2>&1 | Out-String).Trim()
        if ($LASTEXITCODE -ne 0 -or -not $npmCache) {
            throw 'NPM_STANDARD_PATH_RESOLVE_FAILED'
        }
        $result.NpmCache = Resolve-AbsoluteDependencyPath $npmCache 'npm-cache' $RepositoryRoot
    }
    if ($VcpkgRoot) {
        $root = Resolve-AbsoluteDependencyPath $VcpkgRoot 'vcpkg-root' $RepositoryRoot
        $required = @(
            (Join-Path $root 'vcpkg.exe'),
            (Join-Path $root 'scripts\buildsystems\vcpkg.cmake'),
            (Join-Path $root 'installed'),
            (Join-Path $root 'downloads')
        )
        foreach ($path in $required) {
            if (-not (Test-Path -LiteralPath $path)) {
                throw "VCPKG_STANDARD_PATH_MISSING path=$path"
            }
        }
        $result.VcpkgRoot = $root
        $result.VcpkgInstalled = Join-Path $root 'installed'
        $result.VcpkgDownloads = Join-Path $root 'downloads'
    }
    return [pscustomobject]$result
}
