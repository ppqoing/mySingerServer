<#
.SYNOPSIS
构建并验证 mySingerServer Rust V2 Windows x64 便携发布包。

.DESCRIPTION
默认执行固定的 Cargo Release 构建，然后从一个全新的 staging 目录开始组装发布包。
发布内容采用白名单：三个 Rust 可执行文件、固定哈希的 Everything.exe、五个固定 FFmpeg
运行 DLL、中心建库脚本和许可证闭包。脚本不会复制 data、config、旧 Go/C++ 产物或任何
FFmpeg EXE。

.PARAMETER CargoTargetDir
可选的 Cargo target 根目录。相对路径按仓库根目录解析；未指定时固定使用仓库 target。
该参数通过 CARGO_TARGET_DIR 传递，不改变计划约定的 Cargo 命令行。

.PARAMETER SkipBuild
仅供已经完成同一目标 Release 构建后的打包复测使用。省略此参数时始终执行固定构建命令；
使用后仍会严格检查三个目标 EXE 是否存在，并完整执行依赖获取、打包和发布验证。
#>
[CmdletBinding()]
param(
    [string] $CargoTargetDir = '',
    [switch] $SkipBuild
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repositoryRoot = Split-Path -Parent $PSScriptRoot
$targetTriple = 'x86_64-pc-windows-msvc'
$distributionRoot = [IO.Path]::GetFullPath((Join-Path $repositoryRoot 'dist-rust-v2'))
$stagingDirectory = [IO.Path]::GetFullPath((Join-Path $distributionRoot 'staging'))
$archiveName = 'mySingerServer-rust-v2-win-x64.zip'
$archivePath = Join-Path $distributionRoot $archiveName
$archiveSidecarPath = "$archivePath.sha256"
$ffmpegFetcher = Join-Path $PSScriptRoot 'fetch-ffmpeg.ps1'
$noticeGenerator = Join-Path $PSScriptRoot 'generate-third-party-notices.ps1'
$releaseVerifier = Join-Path $PSScriptRoot 'verify-release.ps1'
$everythingRoot = Join-Path $repositoryRoot 'third_party\everything'
$everythingManifestPath = Join-Path $everythingRoot 'manifest.json'
$everythingNoticePath = Join-Path $everythingRoot 'NOTICE.md'
$defaultBootstrap = "config_path = 'config/node.toml'$([Environment]::NewLine)"
$defaultNodeConfig = @'
listen_ip = "127.0.0.1"
port = 39091
worker_count = 4
enumerator = "everything"
image_extensions = [
  "apng", "avif", "bmp", "cur", "dds", "dib", "dpx", "exr", "fits", "gif",
  "hdr", "heic", "heif", "ico", "j2c", "j2k", "jfif", "jls", "jp2", "jpc",
  "jpe", "jpeg", "jpg", "jxl", "pam", "pbm", "pcd", "pcx", "pfm", "pgm",
  "pgx", "png", "pnm", "ppm", "psd", "qoi", "ras", "sgi", "svg", "tga",
  "tif", "tiff", "webp", "xbm", "xpm", "xwd"
]
video_extensions = [
  "264", "265", "266", "3g2", "3gp", "amv", "apv", "asf", "av1", "avc",
  "avi", "bik", "bink", "cdxl", "dav", "dif", "divx", "dv", "evc", "evo",
  "f4v", "flm", "flv", "gxf", "h261", "h263", "h264", "h265", "h266", "hevc",
  "ifv", "ismv", "ivf", "kux", "lvf", "m1v", "m2t", "m2ts", "m2v", "m4v",
  "mj2", "mjpeg", "mjpg", "mk3d", "mkv", "moflex", "mov", "mp4", "mpe", "mpeg",
  "mpg", "mts", "mxf", "nsv", "nut", "nuv", "obu", "ogm", "ogv", "pdv",
  "qt", "r3d", "rm", "rmvb", "roq", "rpl", "ser", "smjpeg", "smk", "str",
  "swf", "ts", "ty", "usm", "vc1", "viv", "vivo", "vob", "vvc", "webm",
  "wmv", "wtv", "xmv", "y4m", "yop"
]

[paths]
data_path = "data/node"
config_path = "config/node.toml"
log_path = "data/node/logs"
cache_path = "data/node/cache"

[read]
hdd_threads_per_disk = 1
ssd_threads_per_disk = 2
unknown_threads_per_disk = 1
total_threads = 4
block_size_bytes = 4194304
block_timeout_seconds = 3
block_retries = 2

[worker]
mode = "automatic"
reserved_cores = 1
manual_worker_count = 4

[postgres]
enabled = false
host = "127.0.0.1"
port = 5432
database = "media_dedup"
username = "postgres"
password = ""
connect_timeout_seconds = 3
'@
$cargoCommand = Get-Command cargo -ErrorAction SilentlyContinue
$cargoExecutable = if ($cargoCommand) {
    $cargoCommand.Source
}
else {
    Join-Path $env:USERPROFILE '.cargo\bin\cargo.exe'
}

function Resolve-RepositoryDirectory {
    param([string] $Path)

    if ([string]::IsNullOrWhiteSpace($Path)) {
        return [IO.Path]::GetFullPath((Join-Path $repositoryRoot 'target'))
    }
    if ([IO.Path]::IsPathRooted($Path)) {
        return [IO.Path]::GetFullPath($Path)
    }
    return [IO.Path]::GetFullPath((Join-Path $repositoryRoot $Path))
}

function Assert-RequiredFile {
    param(
        [Parameter(Mandatory)] [string] $Path,
        [Parameter(Mandatory)] [string] $Code
    )

    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        throw "$Code path=$Path"
    }
}

function Copy-RequiredFile {
    param(
        [Parameter(Mandatory)] [string] $Source,
        [Parameter(Mandatory)] [string] $Destination,
        [Parameter(Mandatory)] [string] $Code
    )

    Assert-RequiredFile -Path $Source -Code $Code
    $destinationParent = Split-Path -Parent $Destination
    if (-not (Test-Path -LiteralPath $destinationParent -PathType Container)) {
        New-Item -ItemType Directory -Path $destinationParent -Force | Out-Null
    }
    Copy-Item -LiteralPath $Source -Destination $Destination -Force
}

function Write-Utf8NoBom {
    param(
        [Parameter(Mandatory)] [string] $Path,
        [Parameter(Mandatory)] [string] $Value
    )

    [IO.File]::WriteAllText($Path, $Value, [Text.UTF8Encoding]::new($false))
}

function Write-FileManifest {
    param([Parameter(Mandatory)] [string] $Root)

    $manifestDirectory = Join-Path $Root 'manifest'
    $manifestPath = Join-Path $manifestDirectory 'files.sha256'
    New-Item -ItemType Directory -Path $manifestDirectory -Force | Out-Null

    # 清单覆盖 staging 中当时存在的每一个普通文件，但按约定不递归记录清单自身。
    # 路径统一写成正斜杠，哈希统一小写，并使用 sha256sum 兼容的“双空格”格式。
    $lines = @(
        Get-ChildItem -LiteralPath $Root -Recurse -File |
            Where-Object {
                -not [StringComparer]::OrdinalIgnoreCase.Equals($_.FullName, $manifestPath)
            } |
            ForEach-Object {
                $relativePath = [IO.Path]::GetRelativePath($Root, $_.FullName).Replace('\', '/')
                $hash = (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
                [pscustomobject]@{
                    RelativePath = $relativePath
                    Line = "$hash  $relativePath"
                }
            } |
            Sort-Object -Property RelativePath |
            ForEach-Object { $_.Line }
    )
    Write-Utf8NoBom -Path $manifestPath -Value (($lines -join "`n") + "`n")
}

$cargoTargetRoot = Resolve-RepositoryDirectory -Path $CargoTargetDir
$releaseBinaryDirectory = Join-Path $cargoTargetRoot "$targetTriple\release"
$requiredSources = @(
    [pscustomobject]@{ Path = $ffmpegFetcher; Code = 'RUST_V2_FFMPEG_FETCHER_MISSING' },
    [pscustomobject]@{ Path = $noticeGenerator; Code = 'RUST_V2_NOTICE_GENERATOR_MISSING' },
    [pscustomobject]@{ Path = $releaseVerifier; Code = 'RUST_V2_RELEASE_VERIFIER_MISSING' },
    [pscustomobject]@{ Path = $everythingManifestPath; Code = 'RUST_V2_EVERYTHING_MANIFEST_MISSING' },
    [pscustomobject]@{ Path = $everythingNoticePath; Code = 'RUST_V2_EVERYTHING_NOTICE_MISSING' },
    [pscustomobject]@{ Path = (Join-Path $repositoryRoot 'deploy\central-v2.sql'); Code = 'RUST_V2_SCHEMA_MISSING' },
    [pscustomobject]@{ Path = (Join-Path $repositoryRoot 'LICENSE'); Code = 'RUST_V2_PROJECT_LICENSE_MISSING' }
)
foreach ($requiredSource in $requiredSources) {
    Assert-RequiredFile -Path $requiredSource.Path -Code $requiredSource.Code
}
Assert-RequiredFile -Path $cargoExecutable -Code 'RUST_V2_CARGO_MISSING'

$everythingManifest = Get-Content -LiteralPath $everythingManifestPath -Raw | ConvertFrom-Json
if ($everythingManifest.schema_version -ne 1 -or
    [string]$everythingManifest.version -cne '1.4.1.1032' -or
    [string]$everythingManifest.architecture -cne 'x64') {
    throw 'RUST_V2_EVERYTHING_MANIFEST_INVALID'
}
foreach ($name in @('Everything.exe', 'LICENSE.txt')) {
    $entries = @($everythingManifest.files | Where-Object path -CEQ $name)
    if ($entries.Count -ne 1) {
        throw "RUST_V2_EVERYTHING_MANIFEST_FILE_INVALID name=$name"
    }
    $source = Join-Path $everythingRoot $name
    Assert-RequiredFile -Path $source -Code 'RUST_V2_EVERYTHING_SOURCE_MISSING'
    $item = Get-Item -LiteralPath $source
    $hash = (Get-FileHash -LiteralPath $source -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($item.Length -ne [long]$entries[0].size -or
        $hash -cne [string]$entries[0].sha256) {
        throw "RUST_V2_EVERYTHING_SOURCE_HASH_MISMATCH name=$name"
    }
}

# 仅在本脚本进程内清除泛化编译器覆盖，防止 cc-rs 把 MinGW GCC 用于 MSVC 目标。
# 同时固定 Cargo target 根目录，确保构建位置与后续白名单复制位置完全一致。
$originalCc = [Environment]::GetEnvironmentVariable('CC', [EnvironmentVariableTarget]::Process)
$originalCxx = [Environment]::GetEnvironmentVariable('CXX', [EnvironmentVariableTarget]::Process)
$originalCargoTargetDir = [Environment]::GetEnvironmentVariable('CARGO_TARGET_DIR', [EnvironmentVariableTarget]::Process)
$locationPushed = $false

try {
    Remove-Item Env:CC -ErrorAction SilentlyContinue
    Remove-Item Env:CXX -ErrorAction SilentlyContinue
    $env:CARGO_TARGET_DIR = $cargoTargetRoot

    if ($SkipBuild) {
        Write-Warning "已按请求跳过 Cargo 构建；将复用 $releaseBinaryDirectory 中的现有产物。"
    }
    else {
        Push-Location $repositoryRoot
        $locationPushed = $true
        try {
            & $cargoExecutable build --workspace --release --locked --target x86_64-pc-windows-msvc
            if ($LASTEXITCODE -ne 0) {
                throw "RUST_V2_CARGO_BUILD_FAILED exit_code=$LASTEXITCODE"
            }
        }
        finally {
            Pop-Location
            $locationPushed = $false
        }
    }

    $executables = @('desktop.exe', 'node.exe', 'worker.exe')
    foreach ($name in $executables) {
        Assert-RequiredFile -Path (Join-Path $releaseBinaryDirectory $name) `
            -Code 'RUST_V2_RELEASE_BINARY_MISSING'
    }
    # staging 是唯一允许递归清理的目录。先核对它确实是固定 dist 根的直接子目录，
    # 再删除旧内容，避免参数或当前目录变化把递归删除扩大到仓库其他位置。
    $expectedStagingDirectory = [IO.Path]::GetFullPath((Join-Path $distributionRoot 'staging'))
    if (-not [StringComparer]::OrdinalIgnoreCase.Equals($stagingDirectory, $expectedStagingDirectory) -or
        -not [StringComparer]::OrdinalIgnoreCase.Equals(
            [IO.Directory]::GetParent($stagingDirectory).FullName,
            $distributionRoot)) {
        throw "RUST_V2_STAGING_PATH_INVALID path=$stagingDirectory"
    }
    if (Test-Path -LiteralPath $stagingDirectory) {
        Remove-Item -LiteralPath $stagingDirectory -Recurse -Force
    }
    New-Item -ItemType Directory -Path $stagingDirectory -Force | Out-Null

    # 固定名称的旧归档不得在本轮失败时继续冒充新产物，因此组装开始前精确删除它们。
    foreach ($oldArtifact in @($archivePath, $archiveSidecarPath)) {
        if (Test-Path -LiteralPath $oldArtifact) {
            Remove-Item -LiteralPath $oldArtifact -Force
        }
    }

    foreach ($name in $executables) {
        Copy-RequiredFile -Source (Join-Path $releaseBinaryDirectory $name) `
            -Destination (Join-Path $stagingDirectory $name) `
            -Code 'RUST_V2_RELEASE_BINARY_MISSING'
    }
    Copy-RequiredFile -Source (Join-Path $everythingRoot 'Everything.exe') `
        -Destination (Join-Path $stagingDirectory 'Everything.exe') `
        -Code 'RUST_V2_EVERYTHING_SOURCE_MISSING'
    Copy-RequiredFile -Source (Join-Path $repositoryRoot 'deploy\central-v2.sql') `
        -Destination (Join-Path $stagingDirectory 'schema\central-v2.sql') `
        -Code 'RUST_V2_SCHEMA_MISSING'
    Copy-RequiredFile -Source (Join-Path $repositoryRoot 'LICENSE') `
        -Destination (Join-Path $stagingDirectory 'licenses\Project-MIT.txt') `
        -Code 'RUST_V2_PROJECT_LICENSE_MISSING'
    Copy-RequiredFile -Source (Join-Path $everythingRoot 'LICENSE.txt') `
        -Destination (Join-Path $stagingDirectory 'licenses\Everything-License.txt') `
        -Code 'RUST_V2_EVERYTHING_SOURCE_MISSING'
    Copy-RequiredFile -Source $everythingNoticePath `
        -Destination (Join-Path $stagingDirectory 'licenses\Everything-NOTICE.md') `
        -Code 'RUST_V2_EVERYTHING_NOTICE_MISSING'
    Write-Utf8NoBom -Path (Join-Path $stagingDirectory 'bootstrap.toml') -Value $defaultBootstrap
    $defaultConfigPath = Join-Path $stagingDirectory 'config\node.toml'
    New-Item -ItemType Directory -Path (Split-Path -Parent $defaultConfigPath) -Force | Out-Null
    Write-Utf8NoBom -Path $defaultConfigPath -Value $defaultNodeConfig

    # FFmpeg 获取脚本内部按锁定清单校验归档、五个 DLL 和 LGPL 文本；Destination
    # 指向 staging 根，使运行库只能落在 runtime/ffmpeg，不把 ffmpeg.exe 等工具带入包。
    & $ffmpegFetcher -Destination $stagingDirectory

    # 第三方 notices 生成器只接收 licenses 目录。它负责 Rust、Slint 与 PDQ 的许可证闭包，
    # 并与 fetch-ffmpeg 已写入的 FFmpeg LGPL 文本共同构成最终发布许可证集合。
    $licenseDirectory = Join-Path $stagingDirectory 'licenses'
    & $noticeGenerator -Destination $licenseDirectory

    Write-FileManifest -Root $stagingDirectory

    # 使用 staging 的内容作为 ZIP 根目录，归档解压后直接得到四个 EXE、runtime、schema、
    # licenses 和 manifest，而不会额外嵌套一层 staging 目录。
    Compress-Archive -Path (Join-Path $stagingDirectory '*') `
        -DestinationPath $archivePath -CompressionLevel Optimal
    $archiveHash = (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
    Write-Utf8NoBom -Path $archiveSidecarPath `
        -Value "$archiveHash  $archiveName$([Environment]::NewLine)"

    # 最终验证直接针对 ZIP；验证器会重新解压并核对 PE x64、文件集合、许可证、清单和 sidecar。
    & $releaseVerifier -Package $archivePath

    $archiveSize = (Get-Item -LiteralPath $archivePath).Length
    Write-Output 'RUST_V2_RELEASE_BUILD_PASS'
    Write-Output "PACKAGE_PATH=$archivePath"
    Write-Output "PACKAGE_SIZE=$archiveSize"
    Write-Output "PACKAGE_SHA256=$archiveHash"
}
finally {
    if ($locationPushed) {
        Pop-Location
    }
    [Environment]::SetEnvironmentVariable('CC', $originalCc, [EnvironmentVariableTarget]::Process)
    [Environment]::SetEnvironmentVariable('CXX', $originalCxx, [EnvironmentVariableTarget]::Process)
    [Environment]::SetEnvironmentVariable(
        'CARGO_TARGET_DIR',
        $originalCargoTargetDir,
        [EnvironmentVariableTarget]::Process)
}
