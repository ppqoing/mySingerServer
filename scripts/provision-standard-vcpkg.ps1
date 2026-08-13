param(
    [string]$VcpkgRoot = "C:\vcpkg",
    [string]$VcpkgExecutable = ""
)

$ErrorActionPreference = 'Stop'

function Resolve-VcpkgExecutable {
    param([string]$Requested, [string]$Root)

    $candidate = if ($Requested) { $Requested } else {
        Join-Path $Root 'vcpkg.exe'
    }
    if (-not (Test-Path -LiteralPath $candidate -PathType Leaf)) {
        throw "STANDARD_VCPKG_EXECUTABLE_NOT_FOUND path=$candidate"
    }
    return (Resolve-Path -LiteralPath $candidate).Path
}

function Invoke-VcpkgCaptured {
    param([string]$Executable, [string[]]$Arguments, [string]$Label)

    $output = @(& $Executable @Arguments 2>&1)
    if ($LASTEXITCODE -ne 0) {
        throw "STANDARD_VCPKG_COMMAND_FAILED label=$Label exit=$LASTEXITCODE output=$($output -join ' ')"
    }
    return ($output -join [Environment]::NewLine)
}

$installed = Join-Path $VcpkgRoot 'installed'
if (-not (Test-Path -LiteralPath $installed -PathType Container)) {
    throw "STANDARD_VCPKG_INSTALLED_NOT_FOUND path=$installed"
}
$vcpkg = Resolve-VcpkgExecutable -Requested $VcpkgExecutable -Root $VcpkgRoot
$packages = @(
    'libmysql:x64-windows',
    'nlohmann-json:x64-windows',
    'rocksdb[core]:x64-windows',
    'libjpeg-turbo:x64-windows-static',
    'libpng:x64-windows-static',
    'libwebp[core,libwebpmux,nearlossless,simd,unicode]:x64-windows-static'
)

$snapshot = Invoke-VcpkgCaptured -Executable $vcpkg -Arguments @('list') -Label 'list-before'
Write-Host "STANDARD_VCPKG_LIST_BEFORE"
Write-Host $snapshot
$dryRun = Invoke-VcpkgCaptured -Executable $vcpkg `
    -Arguments (@('install', '--classic', '--dry-run') + $packages) `
    -Label 'classic-dry-run'
Write-Host "STANDARD_VCPKG_CLASSIC_DRY_RUN"
Write-Host $dryRun
if ($dryRun -match '(?im)^\s*The following packages will be removed:') {
    throw 'STANDARD_VCPKG_CLASSIC_DRY_RUN_REMOVALS_DETECTED'
}
Invoke-VcpkgCaptured -Executable $vcpkg `
    -Arguments (@('install', '--classic') + $packages) `
    -Label 'classic-install' | Out-Null
Write-Host "STANDARD_VCPKG_CLASSIC_PROVISIONED installed=$installed"
