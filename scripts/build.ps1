param(
    [string]$Go = "go",
    [string]$CC = "gcc",
    [string]$Windres = "",
    [string]$Dlltool = "dlltool",
    [string]$OutDir = "bin",
    [string]$CMake = "",
    [string]$VcpkgRoot = "C:\vcpkg",
    [string]$Vcpkg = "",
    [switch]$MediacoreOnly,
    [switch]$VideoCoreOnly,
    [string]$StageDir = "",
    [switch]$SkipWebBuild,
    [switch]$SkipNodeTrayBuild
)

$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
. (Join-Path $PSScriptRoot 'standard-dependency-paths.ps1')

function Resolve-Application {
    param(
        [string]$Requested,
        [string]$Label,
        [switch]$AllowMissing
    )

    if (Test-Path -LiteralPath $Requested -PathType Leaf) {
        return [string](Resolve-Path -LiteralPath $Requested).Path
    }
    $selected = Get-Command $Requested -CommandType Application `
        -ErrorAction SilentlyContinue |
        Where-Object {
            $_.Source -and
            (Test-Path -LiteralPath $_.Source -PathType Leaf)
        } |
        Select-Object -First 1
    if ($null -eq $selected) {
        if ($AllowMissing) { return $null }
        throw "$Label executable not found: $Requested"
    }
    $source = [string]$selected.Source
    if (-not (Test-Path -LiteralPath $source -PathType Leaf)) {
        if ($AllowMissing) { return $null }
        throw "$Label executable not found: $Requested"
    }
    return $source
}

function Resolve-CMakeExecutable {
    param(
        [string]$Requested,
        [string]$Root
    )

    if ($Requested) {
        $requested = Resolve-Application -Requested $Requested `
            -Label "CMake" -AllowMissing
        if ($null -ne $requested) {
            return $requested
        }
        throw "CMake executable not found: $Requested"
    }

    $pathCMake = Resolve-Application -Requested "cmake" `
        -Label "CMake" -AllowMissing
    if ($null -ne $pathCMake) {
        return $pathCMake
    }

    $cached = Join-Path $Root "downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\cmake.exe"
    if (Test-Path -LiteralPath $cached -PathType Leaf) {
        return (Resolve-Path -LiteralPath $cached).Path
    }
    throw "CMake was not found on PATH or in the vcpkg tools cache: $cached"
}

if ($MyInvocation.InvocationName -eq '.') {
    return
}

$dependencyPaths = Resolve-StandardDependencyPaths `
    -RepositoryRoot $repo -GoExecutable $Go -VcpkgRoot $VcpkgRoot
Write-Host "Go module cache: $($dependencyPaths.GoModCache)"
Write-Host "Go build cache: $($dependencyPaths.GoBuildCache)"
Write-Host "vcpkg installed: $($dependencyPaths.VcpkgInstalled)"

if ($VideoCoreOnly -and $MediacoreOnly) {
    throw "VIDEOCORE_BUILD_MODE_CONFLICT"
}
$freshStageOwned = $false
$useVideoCore = $VideoCoreOnly -or (-not $MediacoreOnly)
if ($useVideoCore) {
    if ([string]::IsNullOrWhiteSpace($StageDir)) {
        throw "VIDEOCORE_STAGE_REQUIRED"
    }
    $stageCandidate = if ([IO.Path]::IsPathRooted($StageDir)) {
        $StageDir
    } else {
        Join-Path $repo $StageDir
    }
    $out = [IO.Path]::GetFullPath($stageCandidate).TrimEnd('\')
    $legacyBin = [IO.Path]::GetFullPath(
        (Join-Path $repo "bin")).TrimEnd('\')
    if ([string]::Equals(
            $out, $legacyBin,
            [StringComparison]::OrdinalIgnoreCase)) {
        throw "VIDEOCORE_STAGE_LEGACY_BIN_FORBIDDEN path=$out"
    }
    if (Test-Path -LiteralPath $out) {
        throw "VIDEOCORE_STAGE_EXISTS path=$out"
    }
} else {
    $out = Join-Path $repo $OutDir
    New-Item -ItemType Directory -Force -Path $out | Out-Null
}

$cmakeExe = Resolve-CMakeExecutable -Requested $CMake -Root $VcpkgRoot
$ctestExe = Join-Path (Split-Path -Parent $cmakeExe) "ctest.exe"
if (-not (Test-Path -LiteralPath $ctestExe -PathType Leaf)) {
    throw "CTest executable not found beside CMake: $ctestExe"
}

$mediacoreSource = Join-Path $repo "mediacore"
$mediacoreBuild = Join-Path $mediacoreSource "build"
$toolchain = Join-Path $VcpkgRoot "scripts\buildsystems\vcpkg.cmake"
if (-not (Test-Path -LiteralPath $toolchain -PathType Leaf)) {
    throw "vcpkg toolchain not found: $toolchain"
}
$provisionVcpkg = Join-Path $PSScriptRoot "provision-standard-vcpkg.ps1"
& $provisionVcpkg -VcpkgRoot $VcpkgRoot -VcpkgExecutable $Vcpkg
if ($LASTEXITCODE -ne 0) { throw "STANDARD_VCPKG_PROVISION_FAILED" }

$ccExe = $null
if ($useVideoCore) {
    $videoCoreSource = Join-Path $repo "videocore"
    $videoCoreBuild = Join-Path $videoCoreSource "build"
    $ffmpegRoot = Join-Path $repo "third_party\ffmpeg"
    $ffmpegRootCMake = $ffmpegRoot -replace '\\', '/'
    $toolchainCMake = $toolchain -replace '\\', '/'

    & $cmakeExe `
        -S $videoCoreSource `
        -B $videoCoreBuild `
        -G "Visual Studio 17 2022" `
        -A x64 `
        "-DCMAKE_EXE_LINKER_FLAGS=" `
        "-DCMAKE_SHARED_LINKER_FLAGS=" `
        "-DCMAKE_MODULE_LINKER_FLAGS=" `
        "-DCMAKE_STATIC_LINKER_FLAGS=" `
        "-DCMAKE_TOOLCHAIN_FILE=$toolchainCMake" `
        -DVCPKG_TARGET_TRIPLET=x64-windows-static `
        "-DVCPKG_INSTALLED_DIR=$($dependencyPaths.VcpkgInstalled)" `
        -DVCPKG_MANIFEST_MODE=OFF `
        "-DVC_FFMPEG_ROOT=$ffmpegRootCMake"
    if ($LASTEXITCODE -ne 0) { throw "VIDEOCORE_CONFIGURE_FAILED" }

    & $cmakeExe --build $videoCoreBuild --config Release
    if ($LASTEXITCODE -ne 0) { throw "VIDEOCORE_BUILD_FAILED" }

    & $ctestExe --test-dir $videoCoreBuild -C Release --output-on-failure
    if ($LASTEXITCODE -ne 0) { throw "VIDEOCORE_TESTS_FAILED" }

    $videoCoreDll = Join-Path $videoCoreBuild "Release\videocore.dll"
    if (-not (Test-Path -LiteralPath $videoCoreDll -PathType Leaf)) {
        throw "VIDEOCORE_DLL_NOT_FOUND path=$videoCoreDll"
    }
    $videoCoreDef = Join-Path $videoCoreSource "exports.def"
    $exportGate = Join-Path $PSScriptRoot "test-videocore-exports.ps1"
    $pwshExe = Resolve-Application -Requested "pwsh" `
        -Label "PowerShell"
    & $pwshExe -NoProfile -File $exportGate `
        -Dll $videoCoreDll -Def $videoCoreDef
    if ($LASTEXITCODE -ne 0) { throw "VIDEOCORE_EXPORT_GATE_FAILED" }

    if ($Dlltool -eq "dlltool") {
        $videoDlltoolExe = Resolve-Application -Requested "dlltool" `
            -Label "dlltool" -AllowMissing
        if ($null -eq $videoDlltoolExe) {
            $videoCcExe = Resolve-Application -Requested $CC -Label "C compiler"
            $dlltoolCandidate = Join-Path `
                (Split-Path -Parent $videoCcExe) "dlltool.exe"
            if (-not (Test-Path -LiteralPath $dlltoolCandidate -PathType Leaf)) {
                throw "VIDEOCORE_DLLTOOL_NOT_FOUND path=$dlltoolCandidate"
            }
            $videoDlltoolExe = (Resolve-Path -LiteralPath $dlltoolCandidate).Path
        }
    } else {
        $videoDlltoolExe = Resolve-Application `
            -Requested $Dlltool -Label "dlltool"
    }
    $videoImportDirectory = Join-Path $repo "internal\wproc\videocore"
    New-Item -ItemType Directory -Force -Path $videoImportDirectory | Out-Null
    $videoImportLibrary = Join-Path $videoImportDirectory "libvideocore.a"
    & $videoDlltoolExe `
        --dllname videocore.dll `
        --def $videoCoreDef `
        --output-lib $videoImportLibrary
    if ($LASTEXITCODE -ne 0 -or
        -not (Test-Path -LiteralPath $videoImportLibrary -PathType Leaf)) {
        throw "VIDEOCORE_IMPORT_LIBRARY_FAILED"
    }

    $stageParent = [IO.Path]::GetDirectoryName($out)
    New-Item -ItemType Directory -Force -Path $stageParent | Out-Null
    $closureTemporary = Join-Path $stageParent `
        (".native-dependencies.json.tmp-{0}" -f [Guid]::NewGuid().ToString("N"))
    $resolver = Join-Path $PSScriptRoot "resolve_native_dependencies.ps1"
    try {
        & $pwshExe -NoProfile -File $resolver `
            -RootDll $videoCoreDll `
            -SearchRoot (Join-Path $videoCoreBuild "Release") `
            -RepositoryRoot $repo `
            -OutFile $closureTemporary
        if ($LASTEXITCODE -ne 0) {
            throw "VIDEOCORE_DEPENDENCY_CLOSURE_FAILED"
        }
        $closure = Get-Content -Raw -LiteralPath $closureTemporary |
            ConvertFrom-Json
        if ($closure.schema_version -ne 1 -or
            @($closure.files).Count -eq 0) {
            throw "VIDEOCORE_DEPENDENCY_MANIFEST_INVALID"
        }
        if (Test-Path -LiteralPath $out) {
            throw "VIDEOCORE_STAGE_EXISTS path=$out"
        }
        New-Item -ItemType Directory -Path $out | Out-Null
        $stageCreated = $true
        foreach ($file in @($closure.files)) {
            $source = Join-Path $repo `
                ([string]$file.path -replace '/', '\\')
            if (-not (Test-Path -LiteralPath $source -PathType Leaf) -or
                [IO.Path]::GetExtension($source) -ine ".dll") {
                throw "VIDEOCORE_STAGE_SOURCE_INVALID path=$source"
            }
            $destination = Join-Path $out ([IO.Path]::GetFileName($source))
            if (Test-Path -LiteralPath $destination) {
                throw "VIDEOCORE_STAGE_NAME_COLLISION path=$destination"
            }
            Copy-Item -LiteralPath $source -Destination $destination
        }
        Copy-Item -LiteralPath $closureTemporary `
            -Destination (Join-Path $out "native-dependencies.json")
        $stageCreated = $false
        $freshStageOwned = $true
    }
    catch {
        if ($stageCreated -and (Test-Path -LiteralPath $out)) {
            Remove-Item -LiteralPath $out -Recurse -Force
        }
        throw
    }
    finally {
        if (Test-Path -LiteralPath $closureTemporary) {
            Remove-Item -LiteralPath $closureTemporary -Force
        }
    }

    if ($VideoCoreOnly) {
        Write-Host ("Built VideoCore, exact exports, import library, and recursive DLL closure in fresh stage: {0}" -f $out)
        return
    }
}

try {
if ($MediacoreOnly) {
    & $cmakeExe `
        -S $mediacoreSource `
        -B $mediacoreBuild `
        -G "Visual Studio 17 2022" `
        -A x64 `
        "-DCMAKE_TOOLCHAIN_FILE=$($toolchain -replace '\\', '/')" `
        -DVCPKG_TARGET_TRIPLET=x64-windows-static `
        "-DVCPKG_INSTALLED_DIR=$($dependencyPaths.VcpkgInstalled)" `
        -DVCPKG_MANIFEST_MODE=OFF
    if ($LASTEXITCODE -ne 0) { throw "mediacore configure failed" }
    & $cmakeExe --build $mediacoreBuild --config Release
    if ($LASTEXITCODE -ne 0) { throw "mediacore Release build failed" }
    & $ctestExe --test-dir $mediacoreBuild -C Release --output-on-failure
    if ($LASTEXITCODE -ne 0) { throw "mediacore tests failed" }

    $mediacoreDll = Join-Path $mediacoreBuild "Release\mediacore.dll"
    $exportsDef = Join-Path $mediacoreSource "exports.def"
    $importLibrary = Join-Path $repo "internal\wproc\mediacore\libmediacore.a"
    $dlltoolExe = Resolve-Application -Requested $Dlltool -Label "dlltool"
    & $dlltoolExe --dllname mediacore.dll --def $exportsDef --output-lib $importLibrary
    if ($LASTEXITCODE -ne 0) { throw "mediacore import-library generation failed" }
    Copy-Item -LiteralPath $mediacoreDll -Destination (Join-Path $out "mediacore.dll") -Force
    Write-Host "Built and tested legacy mediacore.dll"
    return
}

$webBuild = Join-Path $PSScriptRoot "build-web.ps1"
if ($SkipWebBuild) {
    Write-Host "Skipping web rebuild; verifying pre-generated embedded assets for CI."
    & $webBuild -VerifyEmbedded
} else {
    & $webBuild
}
if ($LASTEXITCODE -ne 0) { throw "embedded web build verification failed" }

if ($null -eq $ccExe) {
    $ccExe = Resolve-Application -Requested $CC -Label "C compiler"
}
if ($Windres) {
    $windresExe = Resolve-Application -Requested $Windres -Label "windres"
} else {
    $windresCandidate = Join-Path (Split-Path -Parent $ccExe) "windres.exe"
    if (-not (Test-Path -LiteralPath $windresCandidate -PathType Leaf)) {
        throw "windres executable not found beside C compiler: $windresCandidate"
    }
    $windresExe = (Resolve-Path -LiteralPath $windresCandidate).Path
}

$helperDir = (Resolve-Path -LiteralPath (Join-Path $repo "cmd\helper")).Path.TrimEnd('\')
$helperResource = [System.IO.Path]::GetFullPath((Join-Path $helperDir "rsrc_windows_amd64.syso"))
$helperResourceParent = [System.IO.Path]::GetDirectoryName($helperResource).TrimEnd('\')
if (-not [string]::Equals($helperResourceParent, $helperDir, [System.StringComparison]::OrdinalIgnoreCase)) {
    throw "Refusing to generate Helper resource outside the direct cmd\\helper child: $helperResource"
}
if (Test-Path -LiteralPath $helperResource) {
    throw "Refusing to overwrite an existing Helper resource: $helperResource"
}

$oldCGO = $env:CGO_ENABLED
$oldCC = $env:CC
$oldGOOS = $env:GOOS
$oldGOARCH = $env:GOARCH
try {
    $env:GOOS = "windows"
    $env:GOARCH = "amd64"
    $env:CGO_ENABLED = "0"
    Remove-Item Env:CC -ErrorAction SilentlyContinue
    $controlPackages = @(
        "./internal/nodectl",
        "./internal/nodetray/agentclient",
        "./internal/helpercontrol"
    )
    & $Go -C $repo test @controlPackages -count=1
    if ($LASTEXITCODE -ne 0) { throw "node control package tests failed" }

    & $Go -C $repo build -trimpath -tags nodynamic -o (Join-Path $out "agent.exe") ./cmd/agent
    if ($LASTEXITCODE -ne 0) { throw "agent build failed" }

    & $Go -C $repo build -trimpath -o (Join-Path $out "gui.exe") ./cmd/gui
    if ($LASTEXITCODE -ne 0) { throw "gui build failed" }

    try {
        Push-Location -LiteralPath $helperDir
        try {
            & $windresExe -i "helper.rc" -O coff -o $helperResource
            if ($LASTEXITCODE -ne 0) { throw "helper manifest resource generation failed" }
        }
        finally {
            Pop-Location
        }
        & $Go -C $repo build -trimpath "-ldflags=-H=windowsgui" `
            -o (Join-Path $out "helper.exe") ./cmd/helper
        if ($LASTEXITCODE -ne 0) { throw "helper build failed" }
    }
    finally {
        $resourceCleanupFailure = $null
        try {
            if (Test-Path -LiteralPath $helperResource) {
                Remove-Item -LiteralPath $helperResource -Force -ErrorAction Stop
            }
        }
        catch {
            $resourceCleanupFailure = $_
        }
        if (Test-Path -LiteralPath $helperResource) {
            if ($null -eq $resourceCleanupFailure) {
                $resourceCleanupFailure = "generated Helper resource remains after cleanup"
            }
        }
        if ($null -ne $resourceCleanupFailure) {
            throw ("remove generated Helper resource failed: {0}: {1}" -f $helperResource, $resourceCleanupFailure)
        }
    }

    $env:CGO_ENABLED = "1"
    $env:CC = $ccExe
    & $Go -C $repo build -trimpath -tags nodynamic `
        -o (Join-Path $out "worker.exe") ./cmd/worker
    if ($LASTEXITCODE -ne 0) { throw "worker build failed" }
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
    if ($null -eq $oldGOOS) {
        Remove-Item Env:GOOS -ErrorAction SilentlyContinue
    } else {
        $env:GOOS = $oldGOOS
    }
    if ($null -eq $oldGOARCH) {
        Remove-Item Env:GOARCH -ErrorAction SilentlyContinue
    } else {
        $env:GOARCH = $oldGOARCH
    }
}

if ($SkipNodeTrayBuild) {
    Write-Host "Skipping nodetray build by explicit request."
} else {
    $nodeTrayBuild = Join-Path $PSScriptRoot "build-nodetray.ps1"
    if (-not (Test-Path -LiteralPath $nodeTrayBuild -PathType Leaf)) {
        throw "NODETRAY_BUILD_SCRIPT_MISSING"
    }
    $nodeTrayPackage = Join-Path (Split-Path -Parent $out) `
        (".nodetray-package-{0}" -f [Guid]::NewGuid().ToString("N"))
    try {
        & $nodeTrayBuild -Go $Go -OutDir $nodeTrayPackage
        foreach ($name in @("nodetray.exe", "MicrosoftEdgeWebview2Setup.exe")) {
            $source = Join-Path $nodeTrayPackage $name
            if (-not (Test-Path -LiteralPath $source -PathType Leaf)) {
                throw ("NODETRAY_PACKAGE_ARTIFACT_MISSING name={0}" -f $name)
            }
            Copy-Item -LiteralPath $source -Destination (Join-Path $out $name)
        }
    }
    finally {
        if (Test-Path -LiteralPath $nodeTrayPackage) {
            Remove-Item -LiteralPath $nodeTrayPackage -Recurse -Force
        }
    }
}

$everythingRoot = Join-Path $repo "third_party\everything"
$everythingManifestPath = Join-Path $everythingRoot "manifest.json"
if (-not (Test-Path -LiteralPath $everythingManifestPath -PathType Leaf)) {
    throw "EVERYTHING_MANIFEST_NOT_FOUND path=$everythingManifestPath"
}
$everythingManifest = Get-Content -Raw -LiteralPath $everythingManifestPath |
    ConvertFrom-Json
if ($everythingManifest.schema_version -ne 1 -or
    [string]$everythingManifest.version -cne "1.4.1.1032" -or
    [string]$everythingManifest.architecture -cne "x64") {
    throw "EVERYTHING_MANIFEST_INVALID"
}
foreach ($name in @("Everything.exe", "LICENSE.txt")) {
    $entry = @($everythingManifest.files | Where-Object path -CEQ $name)
    if ($entry.Count -ne 1) {
        throw "EVERYTHING_MANIFEST_FILE_INVALID name=$name"
    }
    $source = Join-Path $everythingRoot $name
    if (-not (Test-Path -LiteralPath $source -PathType Leaf)) {
        throw "EVERYTHING_SOURCE_NOT_FOUND path=$source"
    }
    $item = Get-Item -LiteralPath $source
    $hash = (Get-FileHash -LiteralPath $source -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($item.Length -ne [long]$entry[0].size -or
        $hash -cne [string]$entry[0].sha256) {
        throw "EVERYTHING_SOURCE_HASH_MISMATCH name=$name"
    }
}
$everythingNotice = Join-Path $everythingRoot "NOTICE.md"
if (-not (Test-Path -LiteralPath $everythingNotice -PathType Leaf)) {
    throw "EVERYTHING_NOTICE_NOT_FOUND path=$everythingNotice"
}
$everythingSDK = Join-Path $repo "third_party\everything_sdk\Everything64.dll"
if (-not (Test-Path -LiteralPath $everythingSDK -PathType Leaf)) {
    throw "EVERYTHING_SDK_NOT_FOUND path=$everythingSDK"
}
$everythingLicenses = Join-Path $out "licenses"
New-Item -ItemType Directory -Path $everythingLicenses -Force | Out-Null
Copy-Item -LiteralPath (Join-Path $everythingRoot "Everything.exe") `
    -Destination (Join-Path $out "Everything.exe")
Copy-Item -LiteralPath $everythingSDK `
    -Destination (Join-Path $out "Everything64.dll")
Copy-Item -LiteralPath (Join-Path $everythingRoot "LICENSE.txt") `
    -Destination (Join-Path $everythingLicenses "everything-LICENSE.txt")
Copy-Item -LiteralPath $everythingNotice `
    -Destination (Join-Path $everythingLicenses "everything-NOTICE.md")

foreach ($name in @("agent", "gui")) {
    $example = Join-Path $repo "deploy\$name.example.json"
    $target = Join-Path $out "$name.json"
    if (-not (Test-Path -LiteralPath $target)) {
        Copy-Item -LiteralPath $example -Destination $target
    }
}

$helperExample = Join-Path $repo "deploy\helper.example.json"
$helperConfig = Join-Path $out "helper.json"
if (-not (Test-Path -LiteralPath $helperConfig)) {
    Copy-Item -LiteralPath $helperExample -Destination $helperConfig
}

Copy-Item -LiteralPath (Join-Path $repo "deploy\agent.example.json") `
    -Destination (Join-Path $out "agent.example.json")
Copy-Item -LiteralPath $helperExample `
    -Destination (Join-Path $out "helper.example.json")

$requiredStageFiles = @(
    "agent.exe",
    "gui.exe",
    "helper.exe",
    "worker.exe",
    "videocore.dll",
    "Everything.exe",
    "Everything64.dll",
    "licenses\everything-LICENSE.txt",
    "licenses\everything-NOTICE.md",
    "agent.example.json",
    "helper.example.json"
)
if (-not $SkipNodeTrayBuild) {
    $requiredStageFiles += @("nodetray.exe", "MicrosoftEdgeWebview2Setup.exe")
}
foreach ($name in $requiredStageFiles) {
    if (-not (Test-Path -LiteralPath (Join-Path $out $name) -PathType Leaf)) {
        throw "VIDEOCORE_STAGE_REQUIRED_ARTIFACT_MISSING name=$name"
    }
}
$forbiddenStageNames = @(
    "mediacore.dll", "libmediacore.a", "ffmpeg.exe", "ffprobe.exe", "ffplay.exe"
)
foreach ($name in $forbiddenStageNames) {
    if (Get-ChildItem -LiteralPath $out -Recurse -File |
        Where-Object Name -IEQ $name | Select-Object -First 1) {
        throw "VIDEOCORE_FORBIDDEN_STAGE_ARTIFACT name=$name"
    }
}
if (Test-Path -LiteralPath (Join-Path $out "tools")) {
    throw "VIDEOCORE_FORBIDDEN_STAGE_ARTIFACT name=tools"
}

$manifestFiles = @(
    Get-ChildItem -LiteralPath $out -Recurse -File |
        Where-Object Name -INE "release-manifest.json" |
        Sort-Object FullName |
        ForEach-Object {
            [ordered]@{
                path = [IO.Path]::GetRelativePath($out, $_.FullName).Replace('\', '/')
                size = $_.Length
                sha256 = (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
            }
        }
)
$releaseManifest = [ordered]@{
    schema_version = 1
    native_dependency_manifest = "native-dependencies.json"
    files = $manifestFiles
}
$releaseManifest | ConvertTo-Json -Depth 6 |
    Set-Content -LiteralPath (Join-Path $out "release-manifest.json") -Encoding utf8NoBOM

$freshStageOwned = $false
$nodeTraySummary = if ($SkipNodeTrayBuild) { "nodetray explicitly skipped" } else { "nodetray.exe and verified WebView2 Bootstrapper" }
Write-Host "Built agent.exe, gui.exe, helper.exe, worker.exe, videocore.dll, $nodeTraySummary, recursive FFmpeg DLL closure, and release manifest in $out"
}
catch {
    if ($freshStageOwned -and (Test-Path -LiteralPath $out)) {
        $ownedStage = [IO.Path]::GetFullPath($out).TrimEnd('\')
        if (-not [string]::Equals($ownedStage, $legacyBin,
                [StringComparison]::OrdinalIgnoreCase) -and
            -not [string]::Equals($ownedStage,
                [IO.Path]::GetPathRoot($ownedStage).TrimEnd('\'),
                [StringComparison]::OrdinalIgnoreCase)) {
            Remove-Item -LiteralPath $ownedStage -Recurse -Force
        }
    }
    throw
}
