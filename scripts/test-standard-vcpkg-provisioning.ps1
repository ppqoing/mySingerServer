param()

$ErrorActionPreference = 'Stop'

function Assert-Contains {
    param([string]$Actual, [string]$Expected, [string]$Label)

    if ($Actual -notmatch [regex]::Escape($Expected)) {
        throw "ASSERT_STANDARD_VCPKG_PROVISIONING_FAILED label=$Label expected=$Expected actual=$Actual"
    }
}

$fixture = Join-Path ([IO.Path]::GetTempPath()) `
    ('mysinger-standard-vcpkg-provisioning-' + [Guid]::NewGuid().ToString('N'))
try {
    $vcpkg = Join-Path $fixture 'vcpkg'
    $tools = Join-Path $fixture 'tools'
    New-Item -ItemType Directory -Force -Path `
        $tools, (Join-Path $vcpkg 'installed'), (Join-Path $vcpkg 'downloads') | Out-Null
    $capture = Join-Path $fixture 'vcpkg-args.txt'
    $fakeVcpkg = Join-Path $tools 'vcpkg.cmd'
    Set-Content -LiteralPath $fakeVcpkg -Value @(
        '@echo off',
        '>> "%MY_SINGER_FAKE_VCPKG_ARGS%" echo %*',
        'if /I "%1"=="list" (echo fixture:x64-windows 1.0.0 & exit /b 0)',
        'exit /b 0'
    )

    $oldCapture = $env:MY_SINGER_FAKE_VCPKG_ARGS
    try {
        $env:MY_SINGER_FAKE_VCPKG_ARGS = $capture
        & (Join-Path $PSScriptRoot 'provision-standard-vcpkg.ps1') `
            -VcpkgRoot $vcpkg -VcpkgExecutable $fakeVcpkg
        $exitCode = $LASTEXITCODE
    } finally {
        if ($null -eq $oldCapture) {
            Remove-Item Env:MY_SINGER_FAKE_VCPKG_ARGS -ErrorAction SilentlyContinue
        } else {
            $env:MY_SINGER_FAKE_VCPKG_ARGS = $oldCapture
        }
    }

    if ($exitCode -ne 0) {
        throw "FAKE_VCPKG_PROVISIONING_EXIT=$exitCode"
    }
    $actual = Get-Content -Raw -LiteralPath $capture
    Assert-Contains -Actual $actual -Expected 'list' -Label 'snapshot'
    Assert-Contains -Actual $actual -Expected 'install --classic --dry-run libmysql:x64-windows nlohmann-json:x64-windows rocksdb[core]:x64-windows libjpeg-turbo:x64-windows-static libpng:x64-windows-static libwebp[core,libwebpmux,nearlossless,simd,unicode]:x64-windows-static' -Label 'classic-dry-run'
    Assert-Contains -Actual $actual -Expected 'install --classic libmysql:x64-windows nlohmann-json:x64-windows rocksdb[core]:x64-windows libjpeg-turbo:x64-windows-static libpng:x64-windows-static libwebp[core,libwebpmux,nearlossless,simd,unicode]:x64-windows-static' -Label 'classic-install'

    Set-Content -LiteralPath $fakeVcpkg -Value @(
        '@echo off',
        '>> "%MY_SINGER_FAKE_VCPKG_ARGS%" echo %*',
        'if /I "%1"=="list" (echo fixture:x64-windows 1.0.0 & exit /b 0)',
        'if /I "%3"=="--dry-run" (echo The following packages will be removed: & exit /b 0)',
        'exit /b 0'
    )
    Remove-Item -LiteralPath $capture -Force
    $oldCapture = $env:MY_SINGER_FAKE_VCPKG_ARGS
    try {
        $env:MY_SINGER_FAKE_VCPKG_ARGS = $capture
        try {
            & (Join-Path $PSScriptRoot 'provision-standard-vcpkg.ps1') `
                -VcpkgRoot $vcpkg -VcpkgExecutable $fakeVcpkg
            throw 'EXPECTED_CLASSIC_DRY_RUN_REMOVAL_REJECTION'
        } catch {
            if ($_.Exception.Message -notmatch 'STANDARD_VCPKG_CLASSIC_DRY_RUN_REMOVALS_DETECTED') {
                throw
            }
        }
    } finally {
        if ($null -eq $oldCapture) {
            Remove-Item Env:MY_SINGER_FAKE_VCPKG_ARGS -ErrorAction SilentlyContinue
        } else {
            $env:MY_SINGER_FAKE_VCPKG_ARGS = $oldCapture
        }
    }
    $calls = @(Get-Content -LiteralPath $capture)
    if ($calls | Where-Object { $_ -match '^install --classic libmysql:x64-windows' }) {
        throw 'CLASSIC_INSTALL_RAN_AFTER_REMOVAL_DRY_RUN'
    }
} finally {
    if (Test-Path -LiteralPath $fixture) {
        Remove-Item -LiteralPath $fixture -Recurse -Force
    }
}

Write-Output 'STANDARD VCPKG PROVISIONING TEST PASS'
