<#
.SYNOPSIS
验证 Rust V2 Windows x64 便携目录或 ZIP 的内容、架构、许可证和文件哈希。

.DESCRIPTION
验证器只接受三个顶层 x64 EXE、固定五个 FFmpeg DLL、中心建库脚本、五类许可证和完整
files.sha256。它不启动程序；运行时冒烟和 GUI 验收由最终验收阶段单独完成。
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string] $Package
)

$ErrorActionPreference = 'Stop'
$requiredExecutables = @('desktop.exe', 'node.exe', 'worker.exe')
$requiredFfmpeg = @('avutil-60.dll', 'swresample-6.dll', 'swscale-9.dll', 'avcodec-62.dll', 'avformat-62.dll')
$requiredLicenses = @(
    'Project-MIT.txt',
    'Rust-Third-Party-Licenses.html',
    'Slint-Royalty-Free-2.0.txt',
    'PDQ-BSD-3-Clause.txt',
    'FFmpeg-LGPL-3.0.txt'
)
$temporaryRoot = $null

function Stop-PackageValidation {
    param([string] $Code, [string] $Message)
    throw "$Code`: $Message"
}

function Get-PeMachine {
    param([string] $Path)

    $stream = [IO.File]::OpenRead($Path)
    $reader = [IO.BinaryReader]::new($stream)
    try {
        if ($stream.Length -lt 136 -or $reader.ReadUInt16() -ne 0x5a4d) {
            Stop-PackageValidation -Code 'INVALID_PE' -Message $Path
        }
        $stream.Position = 0x3c
        $peOffset = $reader.ReadInt32()
        if ($peOffset -lt 64 -or $peOffset + 6 -gt $stream.Length) {
            Stop-PackageValidation -Code 'INVALID_PE' -Message $Path
        }
        $stream.Position = $peOffset
        if ($reader.ReadUInt32() -ne 0x00004550) {
            Stop-PackageValidation -Code 'INVALID_PE' -Message $Path
        }
        return $reader.ReadUInt16()
    }
    finally {
        $reader.Dispose()
        $stream.Dispose()
    }
}

function Get-RelativePackagePath {
    param([string] $Root, [string] $Path)
    return [IO.Path]::GetRelativePath($Root, $Path).Replace('\', '/')
}

function Assert-Manifest {
    param([string] $Root)

    $manifestPath = Join-Path $Root 'manifest\files.sha256'
    if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) {
        Stop-PackageValidation -Code 'MISSING_FILE_MANIFEST' -Message 'manifest/files.sha256'
    }
    $entries = @{}
    foreach ($line in Get-Content -LiteralPath $manifestPath) {
        if ([string]::IsNullOrWhiteSpace($line)) {
            continue
        }
        if ($line -notmatch '^([0-9a-fA-F]{64})  (.+)$') {
            Stop-PackageValidation -Code 'INVALID_FILE_MANIFEST' -Message $line
        }
        $relative = $Matches[2].Replace('\', '/')
        if ($relative.StartsWith('/') -or $relative.Split('/') -contains '..' -or $entries.ContainsKey($relative)) {
            Stop-PackageValidation -Code 'INVALID_FILE_MANIFEST' -Message $relative
        }
        $entries[$relative] = $Matches[1].ToLowerInvariant()
    }

    $files = @(Get-ChildItem -LiteralPath $Root -Recurse -File |
        Where-Object { $_.FullName -ne $manifestPath })
    if ($entries.Count -ne $files.Count) {
        Stop-PackageValidation -Code 'FILE_MANIFEST_MISMATCH' -Message "清单 $($entries.Count) 项，实际 $($files.Count) 项"
    }
    foreach ($file in $files) {
        $relative = Get-RelativePackagePath -Root $Root -Path $file.FullName
        if (-not $entries.ContainsKey($relative)) {
            Stop-PackageValidation -Code 'UNLISTED_PACKAGE_FILE' -Message $relative
        }
        $actual = (Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($actual -ne $entries[$relative]) {
            Stop-PackageValidation -Code 'FILE_HASH_MISMATCH' -Message $relative
        }
    }
}

function Assert-PackageRoot {
    param([string] $Root)

    $forbiddenFfmpeg = @(Get-ChildItem -LiteralPath $Root -Recurse -File |
        Where-Object { $_.Name -in @('ffmpeg.exe', 'ffprobe.exe', 'ffplay.exe') })
    if ($forbiddenFfmpeg.Count -gt 0) {
        Stop-PackageValidation -Code 'FORBIDDEN_FFMPEG_EXE' -Message $forbiddenFfmpeg[0].FullName
    }

    foreach ($name in $requiredExecutables) {
        $path = Join-Path $Root $name
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
            Stop-PackageValidation -Code 'MISSING_REQUIRED_EXE' -Message $name
        }
        $machine = Get-PeMachine -Path $path
        if ($machine -ne 0x8664) {
            Stop-PackageValidation -Code 'NOT_X64_PE' -Message "$name machine=0x$($machine.ToString('x4'))"
        }
    }
    $allExecutables = @(Get-ChildItem -LiteralPath $Root -Recurse -File -Filter '*.exe')
    if ($allExecutables.Count -ne $requiredExecutables.Count) {
        Stop-PackageValidation -Code 'UNEXPECTED_EXE' -Message (($allExecutables.Name | Sort-Object) -join ', ')
    }

    $runtime = Join-Path $Root 'runtime\ffmpeg'
    $actualRuntime = if (Test-Path -LiteralPath $runtime -PathType Container) {
        @(Get-ChildItem -LiteralPath $runtime -Recurse -File |
            ForEach-Object { Get-RelativePackagePath -Root $runtime -Path $_.FullName })
    }
    else {
        @()
    }
    if (@(Compare-Object -ReferenceObject $requiredFfmpeg -DifferenceObject $actualRuntime).Count -ne 0) {
        Stop-PackageValidation -Code 'FFMPEG_DLL_SET_MISMATCH' -Message ($actualRuntime -join ', ')
    }

    foreach ($name in $requiredLicenses) {
        $path = Join-Path (Join-Path $Root 'licenses') $name
        if (-not (Test-Path -LiteralPath $path -PathType Leaf) -or (Get-Item -LiteralPath $path).Length -eq 0) {
            Stop-PackageValidation -Code 'MISSING_LICENSE' -Message $name
        }
    }
    $schema = Join-Path $Root 'schema\central-v2.sql'
    if (-not (Test-Path -LiteralPath $schema -PathType Leaf)) {
        Stop-PackageValidation -Code 'MISSING_SCHEMA' -Message 'schema/central-v2.sql'
    }

    $forbiddenData = @(Get-ChildItem -LiteralPath $Root -Recurse -File |
        Where-Object { $_.Extension -in @('.db', '.sqlite', '.sqlite3') -or $_.Name -eq 'config.toml' })
    if ($forbiddenData.Count -gt 0 -or (Test-Path -LiteralPath (Join-Path $Root 'data'))) {
        $name = if ($forbiddenData.Count -gt 0) { $forbiddenData[0].FullName } else { 'data/' }
        Stop-PackageValidation -Code 'FORBIDDEN_RUNTIME_DATA' -Message $name
    }

    Assert-Manifest -Root $Root
}

try {
    $resolvedPackage = [IO.Path]::GetFullPath($Package)
    if (Test-Path -LiteralPath $resolvedPackage -PathType Container) {
        $packageRoot = $resolvedPackage
    }
    elseif (Test-Path -LiteralPath $resolvedPackage -PathType Leaf) {
        if ([IO.Path]::GetExtension($resolvedPackage) -ne '.zip') {
            Stop-PackageValidation -Code 'UNSUPPORTED_PACKAGE' -Message $resolvedPackage
        }
        $temporaryRoot = Join-Path ([IO.Path]::GetTempPath()) ("rust-v2-verify-" + [Guid]::NewGuid().ToString('N'))
        New-Item -ItemType Directory -Path $temporaryRoot | Out-Null
        Expand-Archive -LiteralPath $resolvedPackage -DestinationPath $temporaryRoot
        $packageRoot = $temporaryRoot
    }
    else {
        Stop-PackageValidation -Code 'PACKAGE_NOT_FOUND' -Message $resolvedPackage
    }

    $archiveHash = $null
    if (Test-Path -LiteralPath $resolvedPackage -PathType Leaf) {
        $archiveHash = (Get-FileHash -LiteralPath $resolvedPackage -Algorithm SHA256).Hash.ToLowerInvariant()
        $sidecar = "$resolvedPackage.sha256"
        if (Test-Path -LiteralPath $sidecar -PathType Leaf) {
            $declared = ((Get-Content -LiteralPath $sidecar -Raw).Trim() -split '\s+')[0].ToLowerInvariant()
            if ($declared -ne $archiveHash) {
                Stop-PackageValidation -Code 'ARCHIVE_HASH_MISMATCH' -Message $sidecar
            }
        }
    }
    Assert-PackageRoot -Root $packageRoot
    Write-Output 'PACKAGE_PASS'
    Write-Output "PACKAGE_PATH=$resolvedPackage"
    if ($archiveHash) {
        Write-Output "PACKAGE_SHA256=$archiveHash"
    }
}
finally {
    if ($temporaryRoot -and (Test-Path -LiteralPath $temporaryRoot)) {
        Remove-Item -LiteralPath $temporaryRoot -Recurse -Force
    }
}
