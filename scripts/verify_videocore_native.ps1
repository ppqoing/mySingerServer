param(
    [string]$CMake = "",
    [string]$VcpkgRoot = "C:\vcpkg",
    [string]$EvidenceDir = ""
)

$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot

function Resolve-VerificationCMake {
    param([string]$Requested, [string]$Root)
    if ($Requested) {
        if (Test-Path -LiteralPath $Requested -PathType Leaf) {
            return (Resolve-Path -LiteralPath $Requested).Path
        }
        $command = Get-Command $Requested -CommandType Application `
            -ErrorAction SilentlyContinue
        if ($null -ne $command) { return $command.Source }
        throw "VIDEOCORE_VERIFY_CMAKE_NOT_FOUND path=$Requested"
    }
    $command = Get-Command "cmake" -CommandType Application `
        -ErrorAction SilentlyContinue
    if ($null -ne $command) { return $command.Source }
    $cached = Join-Path $Root `
        "downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\cmake.exe"
    if (Test-Path -LiteralPath $cached -PathType Leaf) {
        return (Resolve-Path -LiteralPath $cached).Path
    }
    throw "VIDEOCORE_VERIFY_CMAKE_NOT_FOUND path=$cached"
}

function Resolve-VerificationApplication {
    param([Parameter(Mandatory = $true)][string]$Name)

    $selected = Get-Command $Name -CommandType Application `
        -ErrorAction SilentlyContinue |
        Where-Object {
            $_.Source -and
            (Test-Path -LiteralPath $_.Source -PathType Leaf)
        } |
        Select-Object -First 1
    if ($null -eq $selected) {
        throw "VIDEOCORE_VERIFY_APPLICATION_NOT_FOUND name=$Name"
    }
    $source = [string]$selected.Source
    if (-not (Test-Path -LiteralPath $source -PathType Leaf)) {
        throw "VIDEOCORE_VERIFY_APPLICATION_NOT_FOUND name=$Name path=$source"
    }
    return $source
}

function Invoke-ExpectedClosureFailure {
    param(
        [string]$Resolver,
        [string]$Pwsh,
        [string]$RootDll,
        [string]$SearchRoot,
        [string]$RepositoryRoot,
        [string]$Dumpbin,
        [string]$OutFile,
        [string]$ExpectedCode
    )
    $output = @(& $Pwsh -NoProfile -File $Resolver `
        -RootDll $RootDll `
        -SearchRoot $SearchRoot `
        -RepositoryRoot $RepositoryRoot `
        -Dumpbin $Dumpbin `
        -OutFile $OutFile 2>&1)
    if ($LASTEXITCODE -eq 0 -or
        -not (($output -join "`n").Contains($ExpectedCode)) -or
        (Test-Path -LiteralPath $OutFile)) {
        throw "VIDEOCORE_SYNTHETIC_CLOSURE_NEGATIVE_FAILED code=$ExpectedCode output=$($output -join ' ')"
    }
}

if ($MyInvocation.InvocationName -eq '.') {
    return
}

$cmakeExe = Resolve-VerificationCMake -Requested $CMake -Root $VcpkgRoot
$ctestExe = Join-Path (Split-Path -Parent $cmakeExe) "ctest.exe"
if (-not (Test-Path -LiteralPath $ctestExe -PathType Leaf)) {
    throw "VIDEOCORE_VERIFY_CTEST_NOT_FOUND path=$ctestExe"
}
$pwsh = Resolve-VerificationApplication -Name "pwsh"
$videoCoreSource = Join-Path $repo "videocore"
$videoCoreBuild = Join-Path $videoCoreSource "build"
$toolchain = Join-Path $VcpkgRoot "scripts\buildsystems\vcpkg.cmake"
$ffmpegRoot = Join-Path $repo "third_party\ffmpeg"
if (-not (Test-Path -LiteralPath $toolchain -PathType Leaf)) {
    throw "VIDEOCORE_VERIFY_TOOLCHAIN_NOT_FOUND path=$toolchain"
}
if (-not $EvidenceDir) {
    $EvidenceDir = Join-Path $repo (
        "artifacts\verification\videocore-native-{0}-{1}" -f `
            (Get-Date -Format "yyyyMMdd-HHmmss"),
            [Guid]::NewGuid().ToString("N").Substring(0, 8))
} elseif (-not [IO.Path]::IsPathRooted($EvidenceDir)) {
    $EvidenceDir = Join-Path $repo $EvidenceDir
}
$evidence = [IO.Path]::GetFullPath($EvidenceDir).TrimEnd('\')
if (Test-Path -LiteralPath $evidence) {
    throw "VIDEOCORE_VERIFY_EVIDENCE_EXISTS path=$evidence"
}
New-Item -ItemType Directory -Path $evidence | Out-Null

$syntheticRoot = Join-Path $repo `
    (".tmp\task10-native-{0}" -f [Guid]::NewGuid().ToString("N"))
try {
    & $cmakeExe `
        -S $videoCoreSource `
        -B $videoCoreBuild `
        -G "Visual Studio 17 2022" `
        -A x64 `
        "-DCMAKE_TOOLCHAIN_FILE=$($toolchain -replace '\\', '/')" `
        -DVCPKG_TARGET_TRIPLET=x64-windows-static `
        "-DVC_FFMPEG_ROOT=$($ffmpegRoot -replace '\\', '/')"
    if ($LASTEXITCODE -ne 0) { throw "VIDEOCORE_VERIFY_CONFIGURE_FAILED" }

    & $cmakeExe --build $videoCoreBuild --config Release
    if ($LASTEXITCODE -ne 0) { throw "VIDEOCORE_VERIFY_BUILD_FAILED" }

    $ctestXml = Join-Path $evidence "ctest.xml"
    & $ctestExe --test-dir $videoCoreBuild -C Release `
        -R '^videocore_' --output-on-failure --output-junit $ctestXml
    if ($LASTEXITCODE -ne 0) { throw "VIDEOCORE_VERIFY_CTEST_FAILED" }

    $release = Join-Path $videoCoreBuild "Release"
    $videoCoreDll = Join-Path $release "videocore.dll"
    $exportList = Join-Path $evidence "exports.txt"
    & $pwsh -NoProfile -File `
        (Join-Path $PSScriptRoot "test-videocore-exports.ps1") `
        -Dll $videoCoreDll `
        -Def (Join-Path $videoCoreSource "exports.def") `
        -OutFile $exportList
    if ($LASTEXITCODE -ne 0) { throw "VIDEOCORE_VERIFY_EXPORTS_FAILED" }

    $runtimeOutput = @(& (Join-Path $release "test_vc_runtime.exe") 2>&1)
    if ($LASTEXITCODE -ne 0) { throw "VIDEOCORE_VERIFY_RUNTIME_FAILED" }
    $runtimeText = $runtimeOutput -join "`n"
    if ($runtimeText -notmatch 'RUNTIME_VERSIONS .*avformat=(\d+)/(\d+) avcodec=(\d+)/(\d+) avutil=(\d+)/(\d+) swscale=(\d+)/(\d+)') {
        throw "VIDEOCORE_VERIFY_RUNTIME_OUTPUT_INVALID"
    }
    for ($index = 1; $index -le 7; $index += 2) {
        $header = [uint32]$Matches[$index]
        $runtime = [uint32]$Matches[$index + 1]
        if (($header -shr 16) -ne ($runtime -shr 16)) {
            throw "VIDEOCORE_VERIFY_RUNTIME_MAJOR_MISMATCH header=$header runtime=$runtime"
        }
    }
    [IO.File]::WriteAllLines(
        (Join-Path $evidence "runtime.txt"),
        $runtimeOutput,
        [Text.UTF8Encoding]::new($false))

    $resilienceOutput = @(& (Join-Path $release "test_vc_resilience.exe") 2>&1)
    if ($LASTEXITCODE -ne 0 -or
        -not (($resilienceOutput -join "`n").Contains("iterations=500")) -or
        -not (($resilienceOutput -join "`n").Contains("live_native=0"))) {
        throw "VIDEOCORE_VERIFY_LIVE_RESOURCES_FAILED output=$($resilienceOutput -join ' ')"
    }
    [IO.File]::WriteAllLines(
        (Join-Path $evidence "resilience.txt"),
        $resilienceOutput,
        [Text.UTF8Encoding]::new($false))

    $syntheticRepository = Join-Path $syntheticRoot "repository"
    $cycle = Join-Path $syntheticRepository "cycle"
    $unresolved = Join-Path $syntheticRepository "unresolved"
    $ambiguous = Join-Path $syntheticRepository "ambiguous"
    $outside = Join-Path $syntheticRoot "outside"
    foreach ($directory in @(
        $cycle, $unresolved,
        (Join-Path $ambiguous "one"),
        (Join-Path $ambiguous "two"),
        $outside)) {
        New-Item -ItemType Directory -Force -Path $directory | Out-Null
    }
    $files = @(
        (Join-Path $cycle "Root.DLL"),
        (Join-Path $cycle "a.dll"),
        (Join-Path $cycle "B.DlL"),
        (Join-Path $unresolved "unresolved-root.dll"),
        (Join-Path $ambiguous "ambiguous-root.dll"),
        (Join-Path $ambiguous "one\same.dll"),
        (Join-Path $ambiguous "two\SAME.DLL"),
        (Join-Path $outside "outside-root.dll"))
    foreach ($file in $files) {
        [IO.File]::WriteAllText(
            $file, "synthetic $([IO.Path]::GetFileName($file))",
            [Text.UTF8Encoding]::new($false))
    }
    $fakeDumpbin = Join-Path $syntheticRoot "fake-dumpbin.ps1"
    $fakeSource = @'
param([string]$Mode, [string]$Path)
$leaf = [IO.Path]::GetFileName($Path).ToLowerInvariant()
switch ($leaf) {
  'root.dll' { @('Image has the following dependencies:', 'a.DLL', 'B.dll', 'A.dll', 'KERNEL32.dll'); break }
  'a.dll' { @('Image has the following dependencies:', 'b.DLL'); break }
  'b.dll' { @('Image has the following dependencies:', 'ROOT.dll', 'api-ms-win-core-file-l1-1-0.dll'); break }
  'unresolved-root.dll' { @('Image has the following dependencies:', 'missing-runtime.dll'); break }
  'ambiguous-root.dll' { @('Image has the following dependencies:', 'same.dll'); break }
  default { @('Image has the following dependencies:') }
}
'@
    [IO.File]::WriteAllText(
        $fakeDumpbin, $fakeSource, [Text.UTF8Encoding]::new($false))
    $resolver = Join-Path $PSScriptRoot "resolve_native_dependencies.ps1"
    $syntheticManifest = Join-Path $evidence "native-dependencies.synthetic.json"
    & $pwsh -NoProfile -File $resolver `
        -RootDll (Join-Path $cycle "Root.DLL") `
        -SearchRoot $cycle `
        -RepositoryRoot $syntheticRepository `
        -Dumpbin $fakeDumpbin `
        -OutFile $syntheticManifest
    if ($LASTEXITCODE -ne 0) { throw "VIDEOCORE_SYNTHETIC_CLOSURE_FAILED" }
    $synthetic = Get-Content -Raw -LiteralPath $syntheticManifest |
        ConvertFrom-Json
    if (@($synthetic.files).Count -ne 3) {
        throw "VIDEOCORE_SYNTHETIC_CLOSURE_COUNT actual=$(@($synthetic.files).Count)"
    }
    Invoke-ExpectedClosureFailure `
        -Resolver $resolver -Pwsh $pwsh `
        -RootDll (Join-Path $unresolved "unresolved-root.dll") `
        -SearchRoot $unresolved -RepositoryRoot $syntheticRepository `
        -Dumpbin $fakeDumpbin `
        -OutFile (Join-Path $syntheticRoot "unresolved.json") `
        -ExpectedCode "NATIVE_DEPENDENCY_UNRESOLVED"
    Invoke-ExpectedClosureFailure `
        -Resolver $resolver -Pwsh $pwsh `
        -RootDll (Join-Path $ambiguous "ambiguous-root.dll") `
        -SearchRoot $ambiguous -RepositoryRoot $syntheticRepository `
        -Dumpbin $fakeDumpbin `
        -OutFile (Join-Path $syntheticRoot "ambiguous.json") `
        -ExpectedCode "NATIVE_DEPENDENCY_AMBIGUOUS"
    Invoke-ExpectedClosureFailure `
        -Resolver $resolver -Pwsh $pwsh `
        -RootDll (Join-Path $outside "outside-root.dll") `
        -SearchRoot $outside -RepositoryRoot $syntheticRepository `
        -Dumpbin $fakeDumpbin `
        -OutFile (Join-Path $syntheticRoot "outside.json") `
        -ExpectedCode "NATIVE_DEPENDENCY_OUTSIDE_REPOSITORY"

    $ledger = [ordered]@{
        schema_version = 1
        status = "pass"
        commit = "N/A"
        abi = 1
        exact_exports = 10
        ctest = "pass"
        runtime_major = "pass"
        resilience_iterations = 500
        live_resources = 0
        synthetic_dependency_closure = "pass"
        real_ffmpeg_staging = "BLOCKED_NOT_RUN"
        package_release = "BLOCKED_NOT_RUN"
    }
    [IO.File]::WriteAllText(
        (Join-Path $evidence "verification-ledger.json"),
        ($ledger | ConvertTo-Json -Depth 4) + [Environment]::NewLine,
        [Text.UTF8Encoding]::new($false))
    Write-Host "VIDEOCORE NATIVE VERIFY PASS evidence=$evidence"
}
finally {
    if (Test-Path -LiteralPath $syntheticRoot) {
        Remove-Item -LiteralPath $syntheticRoot -Recurse -Force
    }
}
