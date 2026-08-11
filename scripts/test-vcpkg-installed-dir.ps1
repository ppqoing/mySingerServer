param()

$ErrorActionPreference = 'Stop'

function Assert-Contains {
    param([string[]]$Actual, [string]$Expected, [string]$Label)

    if (($Actual -join "`n") -notmatch [regex]::Escape($Expected)) {
        throw "ASSERT_VCPKG_INSTALLED_DIR_FAILED label=$Label expected=$Expected actual=$($Actual -join '|')"
    }
}

function Assert-ConfigurePassesStandardInstalledDir {
    param(
        [string]$Entry,
        [string[]]$EntryArguments,
        [string]$Label,
        [string]$CaptureFile,
        [string]$ExpectedInstalledDir
    )

    Remove-Item -LiteralPath $CaptureFile -Force -ErrorAction SilentlyContinue
    $oldCapture = $env:MY_SINGER_FAKE_CMAKE_ARGS
    try {
        $env:MY_SINGER_FAKE_CMAKE_ARGS = $CaptureFile
        $output = (& (Join-Path $PSHOME 'pwsh.exe') -NoProfile -File $Entry `
            @EntryArguments 2>&1 | Out-String)
        $exitCode = $LASTEXITCODE
    } finally {
        if ($null -eq $oldCapture) {
            Remove-Item Env:MY_SINGER_FAKE_CMAKE_ARGS -ErrorAction SilentlyContinue
        } else {
            $env:MY_SINGER_FAKE_CMAKE_ARGS = $oldCapture
        }
    }

    if ($exitCode -eq 0) {
        throw "EXPECTED_FAKE_CMAKE_CONFIGURE_FAILURE label=$Label"
    }
    if (-not (Test-Path -LiteralPath $CaptureFile -PathType Leaf)) {
        throw "FAKE_CMAKE_ARGUMENT_CAPTURE_MISSING label=$Label output=$output"
    }
    Assert-Contains -Actual @(Get-Content -LiteralPath $CaptureFile) `
        -Expected ("-DVCPKG_INSTALLED_DIR=" + $ExpectedInstalledDir) `
        -Label $Label
}

$fixture = Join-Path ([IO.Path]::GetTempPath()) `
    ('mysinger-vcpkg-installed-dir-' + [Guid]::NewGuid().ToString('N'))
try {
    $shared = Join-Path $fixture 'shared'
    $vcpkg = Join-Path $shared 'vcpkg'
    $fakeTools = Join-Path $fixture 'tools'
    New-Item -ItemType Directory -Force -Path `
        $fakeTools, `
        (Join-Path $shared 'go'), `
        (Join-Path $shared 'go-mod'), `
        (Join-Path $shared 'go-build'), `
        (Join-Path $vcpkg 'installed'), `
        (Join-Path $vcpkg 'downloads'), `
        (Join-Path $vcpkg 'scripts\buildsystems') | Out-Null
    Set-Content -LiteralPath (Join-Path $vcpkg 'vcpkg.exe') -Value 'fixture'
    Set-Content -LiteralPath `
        (Join-Path $vcpkg 'scripts\buildsystems\vcpkg.cmake') -Value 'fixture'

    $go = Join-Path $fakeTools 'go.cmd'
    Set-Content -LiteralPath $go -Value @(
        '@echo off',
        ('echo ' + (Join-Path $shared 'go')),
        ('echo ' + (Join-Path $shared 'go-mod')),
        ('echo ' + (Join-Path $shared 'go-build')),
        'exit /b 0'
    )
    $cmake = Join-Path $fakeTools 'cmake.cmd'
    Set-Content -LiteralPath $cmake -Value @(
        '@echo off',
        'if "%MY_SINGER_FAKE_CMAKE_ARGS%"=="" exit /b 98',
        '> "%MY_SINGER_FAKE_CMAKE_ARGS%" echo %*',
        'exit /b 23'
    )
    Set-Content -LiteralPath (Join-Path $fakeTools 'ctest.exe') -Value 'fixture'

    $root = Split-Path -Parent $PSScriptRoot
    $installed = Join-Path $vcpkg 'installed'
    $capture = Join-Path $fixture 'cmake-args.txt'

    Assert-ConfigurePassesStandardInstalledDir `
        -Entry (Join-Path $PSScriptRoot 'build.ps1') `
        -EntryArguments @('-Go', $go, '-Cmake', $cmake, '-VcpkgRoot', $vcpkg, '-VideoCoreOnly', '-StageDir', (Join-Path $fixture 'video-stage'), '-SkipWebBuild', '-SkipNodeTrayBuild') `
        -Label 'build-videocore' -CaptureFile $capture -ExpectedInstalledDir $installed
    Assert-ConfigurePassesStandardInstalledDir `
        -Entry (Join-Path $PSScriptRoot 'build.ps1') `
        -EntryArguments @('-Go', $go, '-Cmake', $cmake, '-VcpkgRoot', $vcpkg, '-MediacoreOnly', '-OutDir', '.') `
        -Label 'build-mediacore' -CaptureFile $capture -ExpectedInstalledDir $installed
    Assert-ConfigurePassesStandardInstalledDir `
        -Entry (Join-Path $PSScriptRoot 'verify_videocore_native.ps1') `
        -EntryArguments @('-CMake', $cmake, '-VcpkgRoot', $vcpkg, '-EvidenceDir', (Join-Path $fixture 'verify-evidence')) `
        -Label 'verify-videocore-native' -CaptureFile $capture -ExpectedInstalledDir $installed
} finally {
    if (Test-Path -LiteralPath $fixture) {
        Remove-Item -LiteralPath $fixture -Recurse -Force
    }
}

Write-Output 'VCPKG INSTALLED DIR BEHAVIOR TEST PASS'
