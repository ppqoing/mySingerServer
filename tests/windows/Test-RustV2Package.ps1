<#
.SYNOPSIS
验证 Rust V2 便携包检查器会接受完整包，并拒绝已确认的供应链缺陷。

.DESCRIPTION
测试只在临时目录创建最小 PE fixture，不启动产品进程，也不读取旧版发布目录。每个失败场景
从同一完整基线复制，确保错误来自单一缺陷：禁带 FFmpeg EXE、缺 worker、非 x64 或许可证缺失。
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$verifier = Join-Path $repositoryRoot 'scripts\verify-release.ps1'
$pwsh = (Get-Command pwsh -ErrorAction Stop).Source
$fixtureRoot = Join-Path ([IO.Path]::GetTempPath()) ("rust-v2-package-test-" + [Guid]::NewGuid().ToString('N'))

function Write-MinimalPe {
    param(
        [Parameter(Mandatory)] [string] $Path,
        [Parameter(Mandatory)] [UInt16] $Machine
    )

    $bytes = [byte[]]::new(512)
    $bytes[0] = 0x4d
    $bytes[1] = 0x5a
    [BitConverter]::GetBytes([Int32]128).CopyTo($bytes, 0x3c)
    $bytes[128] = 0x50
    $bytes[129] = 0x45
    [BitConverter]::GetBytes($Machine).CopyTo($bytes, 132)
    [IO.File]::WriteAllBytes($Path, $bytes)
}

function Write-Utf8 {
    param([string] $Path, [string] $Text)
    [IO.File]::WriteAllText($Path, $Text, [Text.UTF8Encoding]::new($false))
}

function Write-FileManifest {
    param([string] $Root)

    $manifestDirectory = Join-Path $Root 'manifest'
    New-Item -ItemType Directory -Path $manifestDirectory -Force | Out-Null
    $manifestPath = Join-Path $manifestDirectory 'files.sha256'
    $lines = Get-ChildItem -LiteralPath $Root -Recurse -File |
        Where-Object { $_.FullName -ne $manifestPath } |
        ForEach-Object {
            $relative = [IO.Path]::GetRelativePath($Root, $_.FullName).Replace('\', '/')
            $hash = (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
            "$hash  $relative"
        } |
        Sort-Object
    Write-Utf8 -Path $manifestPath -Text (($lines -join "`n") + "`n")
}

function New-GoodFixture {
    param([string] $Path)

    New-Item -ItemType Directory -Path $Path -Force | Out-Null
    foreach ($name in @('desktop.exe', 'node.exe', 'worker.exe')) {
        Write-MinimalPe -Path (Join-Path $Path $name) -Machine 0x8664
    }
    $runtime = Join-Path $Path 'runtime\ffmpeg'
    $licenses = Join-Path $Path 'licenses'
    $schema = Join-Path $Path 'schema'
    New-Item -ItemType Directory -Path $runtime, $licenses, $schema -Force | Out-Null
    foreach ($name in @('avutil-60.dll', 'swresample-6.dll', 'swscale-9.dll', 'avcodec-62.dll', 'avformat-62.dll')) {
        Write-Utf8 -Path (Join-Path $runtime $name) -Text "fixture $name"
    }
    foreach ($name in @(
        'Project-MIT.txt',
        'Rust-Third-Party-Licenses.html',
        'Slint-Royalty-Free-2.0.txt',
        'PDQ-BSD-3-Clause.txt',
        'FFmpeg-LGPL-3.0.txt'
    )) {
        Write-Utf8 -Path (Join-Path $licenses $name) -Text "fixture $name"
    }
    Write-Utf8 -Path (Join-Path $schema 'central-v2.sql') -Text '-- fixture schema'
    Write-FileManifest -Root $Path
}

function Invoke-Verify {
    param([string] $Package)

    $output = & $pwsh -NoProfile -File $verifier -Package $Package 2>&1
    [pscustomobject]@{
        ExitCode = $LASTEXITCODE
        Output = ($output | Out-String)
    }
}

function Assert-FailsWith {
    param([string] $Package, [string] $Code)

    $result = Invoke-Verify -Package $Package
    if ($result.ExitCode -eq 0 -or $result.Output -notmatch [regex]::Escape($Code)) {
        throw "期望验证失败并包含 $Code，实际 exit=$($result.ExitCode)：$($result.Output)"
    }
}

try {
    if (-not (Test-Path -LiteralPath $verifier -PathType Leaf)) {
        throw "发布验证器尚未实现: $verifier"
    }

    $good = Join-Path $fixtureRoot 'good'
    New-GoodFixture -Path $good
    $goodResult = Invoke-Verify -Package $good
    if ($goodResult.ExitCode -ne 0 -or $goodResult.Output -notmatch 'PACKAGE_PASS') {
        throw "完整目录 fixture 未通过：$($goodResult.Output)"
    }

    $zipPath = Join-Path $fixtureRoot 'good.zip'
    Compress-Archive -Path (Join-Path $good '*') -DestinationPath $zipPath
    $zipResult = Invoke-Verify -Package $zipPath
    if ($zipResult.ExitCode -ne 0 -or $zipResult.Output -notmatch 'PACKAGE_PASS') {
        throw "完整 ZIP fixture 未通过：$($zipResult.Output)"
    }
    Write-Utf8 -Path "$zipPath.sha256" -Text (("0" * 64) + "  good.zip`n")
    Assert-FailsWith -Package $zipPath -Code 'ARCHIVE_HASH_MISMATCH'
    Remove-Item -LiteralPath "$zipPath.sha256"

    $forbidden = Join-Path $fixtureRoot 'forbidden-ffmpeg'
    Copy-Item -LiteralPath $good -Destination $forbidden -Recurse
    Write-MinimalPe -Path (Join-Path $forbidden 'runtime\ffmpeg\ffmpeg.exe') -Machine 0x8664
    Assert-FailsWith -Package $forbidden -Code 'FORBIDDEN_FFMPEG_EXE'

    $missingWorker = Join-Path $fixtureRoot 'missing-worker'
    Copy-Item -LiteralPath $good -Destination $missingWorker -Recurse
    Remove-Item -LiteralPath (Join-Path $missingWorker 'worker.exe')
    Assert-FailsWith -Package $missingWorker -Code 'MISSING_REQUIRED_EXE'

    $wrongMachine = Join-Path $fixtureRoot 'wrong-machine'
    Copy-Item -LiteralPath $good -Destination $wrongMachine -Recurse
    Write-MinimalPe -Path (Join-Path $wrongMachine 'desktop.exe') -Machine 0x014c
    Assert-FailsWith -Package $wrongMachine -Code 'NOT_X64_PE'

    $missingLicense = Join-Path $fixtureRoot 'missing-license'
    Copy-Item -LiteralPath $good -Destination $missingLicense -Recurse
    Remove-Item -LiteralPath (Join-Path $missingLicense 'licenses\Slint-Royalty-Free-2.0.txt')
    Assert-FailsWith -Package $missingLicense -Code 'MISSING_LICENSE'

    Write-Output 'RUST_V2_PACKAGE_TEST_PASS'
}
finally {
    if (Test-Path -LiteralPath $fixtureRoot) {
        Remove-Item -LiteralPath $fixtureRoot -Recurse -Force
    }
}
