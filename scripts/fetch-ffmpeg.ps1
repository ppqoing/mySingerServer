<#
.SYNOPSIS
下载并筛选 Rust V2 固定使用的 FFmpeg 8.0.1 x64 LGPL shared 运行库。

.DESCRIPTION
清单固定供应方、归档 URL、归档 SHA、五个 DLL 及许可证 SHA。本脚本只把五个
运行 DLL 写到 runtime/ffmpeg，把许可证写到 licenses；不会复制 FFmpeg EXE、
avdevice 或 avfilter。应用运行时不调用本脚本，也不联网下载依赖。
#>
[CmdletBinding(SupportsShouldProcess)]
param(
    [Parameter(Mandatory)]
    [string] $Destination
)

$ErrorActionPreference = 'Stop'
$repositoryRoot = Split-Path -Parent $PSScriptRoot
$manifestPath = Join-Path $repositoryRoot 'third_party\ffmpeg-dependency.json'
$manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
$runtimeNames = @($manifest.runtime_files | ForEach-Object { $_.name })

Write-Output "FFmpeg source: $($manifest.archive_url)"
Write-Output "Archive SHA-256: $($manifest.archive_sha256)"
Write-Output "Runtime whitelist: $($runtimeNames -join ', ')"
Write-Output "License: licenses\FFmpeg-LGPL-3.0.txt"

if ($WhatIfPreference) {
    return
}

$destinationRoot = [IO.Path]::GetFullPath($Destination)
$runtimeDirectory = Join-Path $destinationRoot 'runtime\ffmpeg'
$licenseDirectory = Join-Path $destinationRoot 'licenses'
$temporaryRoot = Join-Path ([IO.Path]::GetTempPath()) ("mysingerserver-ffmpeg-" + [Guid]::NewGuid().ToString('N'))
$archivePath = Join-Path $temporaryRoot $manifest.archive_name
$expandedDirectory = Join-Path $temporaryRoot 'expanded'

try {
    New-Item -ItemType Directory -Path $temporaryRoot, $expandedDirectory -Force | Out-Null
    Invoke-WebRequest -Uri $manifest.archive_url -OutFile $archivePath

    $archiveHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $archivePath).Hash.ToLowerInvariant()
    if ($archiveHash -ne $manifest.archive_sha256) {
        throw "FFmpeg archive SHA-256 mismatch: expected $($manifest.archive_sha256), got $archiveHash"
    }
    Expand-Archive -LiteralPath $archivePath -DestinationPath $expandedDirectory
    $archiveRoot = Join-Path $expandedDirectory $manifest.archive_root

    New-Item -ItemType Directory -Path $runtimeDirectory, $licenseDirectory -Force | Out-Null
    $unexpected = @(Get-ChildItem -LiteralPath $runtimeDirectory -File -ErrorAction SilentlyContinue |
        Where-Object { $_.Name -notin $runtimeNames })
    if ($unexpected.Count -gt 0) {
        throw "Runtime directory contains non-whitelisted file: $($unexpected[0].Name)"
    }

    foreach ($file in $manifest.runtime_files) {
        $source = Join-Path $archiveRoot ("bin\" + $file.name)
        $sourceHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $source).Hash.ToLowerInvariant()
        if ($sourceHash -ne $file.sha256) {
            throw "FFmpeg runtime SHA-256 mismatch for $($file.name)"
        }
        Copy-Item -LiteralPath $source -Destination (Join-Path $runtimeDirectory $file.name) -Force
    }

    $licenseSource = Join-Path $archiveRoot $manifest.license_source
    $licenseHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $licenseSource).Hash.ToLowerInvariant()
    if ($licenseHash -ne $manifest.license_sha256) {
        throw 'FFmpeg license SHA-256 mismatch'
    }
    Copy-Item -LiteralPath $licenseSource -Destination (Join-Path $licenseDirectory 'FFmpeg-LGPL-3.0.txt') -Force

    $publishedNames = @(Get-ChildItem -LiteralPath $runtimeDirectory -File | ForEach-Object { $_.Name })
    $forbidden = @($publishedNames | Where-Object { $_ -in $manifest.forbidden_runtime_files })
    if ($publishedNames.Count -ne $runtimeNames.Count -or $forbidden.Count -ne 0) {
        throw 'Published FFmpeg runtime does not match the five-file whitelist'
    }
    Write-Output "Published FFmpeg runtime to $runtimeDirectory"
}
finally {
    if (Test-Path -LiteralPath $temporaryRoot) {
        Remove-Item -LiteralPath $temporaryRoot -Recurse -Force
    }
}
