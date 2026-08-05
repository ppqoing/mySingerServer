param(
    [string]$Go = "go",
    [string]$CC = "gcc",
    [string]$DllDir = "bin",
    [ValidateSet("MediaCore", "VideoCore")]
    [string]$Mode = "VideoCore",
    [switch]$Race,
    [switch]$VetOnly,
    [string[]]$Packages = @("./...")
)

$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
if ([System.IO.Path]::IsPathRooted($DllDir)) {
    $dllPath = $DllDir
} else {
    $dllPath = Join-Path $repo $DllDir
}
if ($Mode -eq "VideoCore") {
    $dllName = "videocore.dll"
    $importLibrary = Join-Path $repo "internal\wproc\videocore\libvideocore.a"
} else {
    $dllName = "mediacore.dll"
    $importLibrary = Join-Path $repo "internal\wproc\mediacore\libmediacore.a"
}
$nativeDll = Join-Path $dllPath $dllName

if (-not (Test-Path -LiteralPath $nativeDll -PathType Leaf)) {
    throw "$dllName not found: $nativeDll."
}
if (-not (Test-Path -LiteralPath $importLibrary -PathType Leaf)) {
    throw "MinGW import library not found: $importLibrary."
}

$resolvedDllPath = (Resolve-Path -LiteralPath $dllPath).Path
$oldCGO = $env:CGO_ENABLED
$oldCC = $env:CC
$oldPath = $env:PATH
try {
    $env:CGO_ENABLED = "1"
    $env:CC = $CC
    $env:PATH = "$resolvedDllPath;$oldPath"

    if ($VetOnly -and $Race) {
        throw '-VetOnly and -Race cannot be combined'
    }
    if ($VetOnly) {
        $arguments = @("-C", $repo, "vet")
    } else {
        $arguments = @("-C", $repo, "test")
        if ($Race) {
            $arguments += "-race"
        }
        $arguments += "-count=1"
    }
    $arguments += $Packages
    & $Go @arguments
    if ($LASTEXITCODE -ne 0) {
        throw "CGO Go command failed with exit code $LASTEXITCODE"
    }
}
finally {
    if ($null -eq $oldCGO) {
        Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue
    } else {
        $env:CGO_ENABLED = $oldCGO
    }
    if ($null -eq $oldCC) {
        Remove-Item Env:CC -ErrorAction SilentlyContinue
    } else {
        $env:CC = $oldCC
    }
    if ($null -eq $oldPath) {
        Remove-Item Env:PATH -ErrorAction SilentlyContinue
    } else {
        $env:PATH = $oldPath
    }
}
