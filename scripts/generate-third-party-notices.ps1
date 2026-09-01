<#
.SYNOPSIS
为 Rust V2 x64 发布包生成 Rust 依赖许可证正文，并复制 Slint 与 PDQ 固定许可证。

.DESCRIPTION
脚本固定要求 cargo-about 0.9.1。它先用 Cargo 的锁定离线 metadata 核对当前依赖闭包中的
name/version 均存在于 Cargo.lock，再在系统临时目录生成 cargo-about 配置与 HTML 模板。
最终目录只写三个文件；FFmpeg-LGPL-3.0.txt 由 fetch-ffmpeg.ps1 从锁定归档提供。
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string] $Destination
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repositoryRoot = Split-Path -Parent $PSScriptRoot
$cargoLockPath = Join-Path $repositoryRoot 'Cargo.lock'
$slintLicense = Join-Path $repositoryRoot 'licenses\Slint-Royalty-Free-2.0.txt'
$pdqLicense = Join-Path $repositoryRoot 'licenses\PDQ-BSD-3-Clause.txt'
$destinationRoot = [IO.Path]::GetFullPath($Destination)
$temporaryRoot = Join-Path ([IO.Path]::GetTempPath()) ("rust-v2-notices-" + [Guid]::NewGuid().ToString('N'))
$aboutConfig = Join-Path $temporaryRoot 'about.toml'
$aboutTemplate = Join-Path $temporaryRoot 'about.hbs'
$outputPath = Join-Path $destinationRoot 'Rust-Third-Party-Licenses.html'

function Write-Utf8NoBom {
    param([string] $Path, [string] $Value)
    [IO.File]::WriteAllText($Path, $Value, [Text.UTF8Encoding]::new($false))
}

function Assert-SourceFile {
    param([string] $Path, [string] $Code)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        throw "$Code path=$Path"
    }
}

Assert-SourceFile -Path $cargoLockPath -Code 'RUST_V2_CARGO_LOCK_MISSING'
Assert-SourceFile -Path $slintLicense -Code 'RUST_V2_SLINT_LICENSE_MISSING'
Assert-SourceFile -Path $pdqLicense -Code 'RUST_V2_PDQ_LICENSE_MISSING'

$cargoAboutCommand = Get-Command cargo-about -ErrorAction SilentlyContinue
$cargoAboutPath = if ($cargoAboutCommand) {
    $cargoAboutCommand.Source
}
else {
    Join-Path $env:USERPROFILE '.cargo\bin\cargo-about.exe'
}
if (-not (Test-Path -LiteralPath $cargoAboutPath -PathType Leaf)) {
    throw 'RUST_V2_CARGO_ABOUT_MISSING: 安装 cargo-about 0.9.1 --locked --features cli'
}
$cargoCommand = Get-Command cargo -ErrorAction SilentlyContinue
$cargoPath = if ($cargoCommand) {
    $cargoCommand.Source
}
else {
    Join-Path $env:USERPROFILE '.cargo\bin\cargo.exe'
}
if (-not (Test-Path -LiteralPath $cargoPath -PathType Leaf)) {
    throw 'RUST_V2_CARGO_MISSING: 未找到标准 Rust Cargo 工具'
}
$aboutVersion = (& $cargoAboutPath --version).Trim()
if ($LASTEXITCODE -ne 0 -or $aboutVersion -ne 'cargo-about 0.9.1') {
    throw "RUST_V2_CARGO_ABOUT_VERSION: expected cargo-about 0.9.1, got $aboutVersion"
}

$originalCc = [Environment]::GetEnvironmentVariable('CC', [EnvironmentVariableTarget]::Process)
$originalCxx = [Environment]::GetEnvironmentVariable('CXX', [EnvironmentVariableTarget]::Process)
$originalPath = [Environment]::GetEnvironmentVariable('PATH', [EnvironmentVariableTarget]::Process)
$locationPushed = $false

try {
    Remove-Item Env:CC -ErrorAction SilentlyContinue
    Remove-Item Env:CXX -ErrorAction SilentlyContinue
    $env:PATH = (Split-Path -Parent $cargoPath) + ';' + $originalPath
    Push-Location $repositoryRoot
    $locationPushed = $true

    $metadataJson = & $cargoPath metadata --locked --offline --format-version 1
    if ($LASTEXITCODE -ne 0) {
        throw "RUST_V2_CARGO_METADATA_FAILED exit_code=$LASTEXITCODE"
    }
    $metadata = $metadataJson | ConvertFrom-Json
    $resolvedIds = @($metadata.resolve.nodes | ForEach-Object { $_.id })
    $resolvedPackages = @($metadata.packages | Where-Object { $_.id -in $resolvedIds })
    $lockText = Get-Content -LiteralPath $cargoLockPath -Raw
    foreach ($package in $resolvedPackages) {
        $name = [regex]::Escape([string]$package.name)
        $version = [regex]::Escape([string]$package.version)
        # 同时接受 LF 与 CRLF，避免 version 行末残留的 \r 被误判为未锁定依赖。
        $pattern = "(?m)^name = `"$name`"\r?\nversion = `"$version`"\r?$"
        if ($lockText -notmatch $pattern) {
            throw "RUST_V2_METADATA_NOT_LOCKED package=$($package.name)@$($package.version)"
        }
    }

    New-Item -ItemType Directory -Path $temporaryRoot, $destinationRoot -Force | Out-Null
    Write-Utf8NoBom -Path $aboutConfig -Value @'
accepted = [
  "0BSD",
  "Apache-2.0",
  "BSD-2-Clause",
  "BSD-3-Clause",
  "BSL-1.0",
  "CC0-1.0",
  "ISC",
  "LGPL-2.1-or-later",
  "LicenseRef-Slint-Royalty-free-2.0",
  "MIT",
  "NCSA",
  "Unicode-3.0",
  "Unlicense",
  "Zlib",
]
targets = ["x86_64-pc-windows-msvc"]
ignore-build-dependencies = false
ignore-dev-dependencies = false
ignore-transitive-dependencies = false
'@
    Write-Utf8NoBom -Path $aboutTemplate -Value @'
<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>Rust Third-Party Licenses</title></head>
<body>
<h1>Rust Third-Party Licenses</h1>
<p>Generated from Cargo.lock with cargo-about 0.9.1 for x86_64-pc-windows-msvc.</p>
{{#each licenses}}
<section>
  <h2>{{name}}</h2>
  <h3>Used by</h3>
  <ul>{{#each used_by}}<li>{{crate.name}} {{crate.version}}</li>{{/each}}</ul>
  <pre>{{text}}</pre>
</section>
{{/each}}
</body>
</html>
'@

    & $cargoAboutPath generate --config $aboutConfig --workspace --locked --offline `
        --target x86_64-pc-windows-msvc --fail --output-file $outputPath $aboutTemplate
    if ($LASTEXITCODE -ne 0) {
        throw "RUST_V2_CARGO_ABOUT_FAILED exit_code=$LASTEXITCODE"
    }

    $html = Get-Content -LiteralPath $outputPath -Raw
    # cargo-about 能解析 Slint 的 LicenseRef 选择，但 Slint 子 crate 没有把自定义正文注册为
    # 工具可识别的 license file。发布包已经单独携带官方正文；这里再把所有对应 crate 和正文
    # 显式写入 HTML，避免依赖清单因工具识别边界而遗漏 Slint。
    $slintPackages = @($resolvedPackages |
        Where-Object { $_.license -like '*LicenseRef-Slint-Royalty-free-2.0*' } |
        Sort-Object -Property name, version)
    $slintItems = $slintPackages | ForEach-Object {
        '<li>{0} {1}</li>' -f `
            [Net.WebUtility]::HtmlEncode([string]$_.name), `
            [Net.WebUtility]::HtmlEncode([string]$_.version)
    }
    $slintText = [Net.WebUtility]::HtmlEncode((Get-Content -LiteralPath $slintLicense -Raw))
    $slintSection = @"
<section>
  <h2>LicenseRef-Slint-Royalty-free-2.0</h2>
  <h3>Used by</h3>
  <ul>$($slintItems -join '')</ul>
  <pre>$slintText</pre>
</section>
"@
    $html = $html.Replace('</body>', "$slintSection`n</body>")
    Write-Utf8NoBom -Path $outputPath -Value $html
    foreach ($requiredCrate in @('slint 1.17.1', 'tokio 1.53.1', 'rusqlite 0.40.1', 'prost 0.14.4', 'windows 0.62.2')) {
        if ($html -notmatch [regex]::Escape($requiredCrate)) {
            throw "RUST_V2_NOTICE_CRATE_MISSING crate=$requiredCrate"
        }
    }
    Copy-Item -LiteralPath $slintLicense -Destination (Join-Path $destinationRoot 'Slint-Royalty-Free-2.0.txt') -Force
    Copy-Item -LiteralPath $pdqLicense -Destination (Join-Path $destinationRoot 'PDQ-BSD-3-Clause.txt') -Force

    Write-Output 'RUST_V2_NOTICES_PASS'
    Write-Output "RUST_DEPENDENCY_COUNT=$($resolvedPackages.Count)"
    Write-Output "RUST_NOTICES=$outputPath"
    Write-Output 'FFMPEG_LICENSE_PROVIDER=scripts/fetch-ffmpeg.ps1'
}
finally {
    if ($locationPushed) {
        Pop-Location
    }
    [Environment]::SetEnvironmentVariable('CC', $originalCc, [EnvironmentVariableTarget]::Process)
    [Environment]::SetEnvironmentVariable('CXX', $originalCxx, [EnvironmentVariableTarget]::Process)
    [Environment]::SetEnvironmentVariable('PATH', $originalPath, [EnvironmentVariableTarget]::Process)
    if (Test-Path -LiteralPath $temporaryRoot) {
        Remove-Item -LiteralPath $temporaryRoot -Recurse -Force
    }
}
