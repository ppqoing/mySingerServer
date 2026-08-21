<#
.SYNOPSIS
验证半小时真实媒体 harness 的输入保护、隔离路径和媒体清单行为。
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$measureScript = Join-Path $repositoryRoot 'tests\windows\Measure-RustV2RuntimeAcceptance.ps1'
$fixtureRoot = Join-Path ([IO.Path]::GetTempPath()) ("rust-v2-runtime-harness-" + [Guid]::NewGuid().ToString('N'))

try {
    if (-not (Test-Path -LiteralPath $measureScript -PathType Leaf)) {
        throw "RUST_V2_RUNTIME_ACCEPTANCE_HARNESS_MISSING path=$measureScript"
    }
    . $measureScript -LibraryOnly

    $media = Join-Path $fixtureRoot 'media'
    $release = Join-Path $fixtureRoot 'release'
    New-Item -ItemType Directory -Path $media, $release -Force | Out-Null
    foreach ($name in @('node.exe', 'worker.exe', 'runtime_acceptance.exe', 'Everything.exe')) {
        [IO.File]::WriteAllText((Join-Path $release $name), "fixture $name")
    }
    $runtime = Join-Path $release 'runtime\ffmpeg'
    New-Item -ItemType Directory -Path $runtime -Force | Out-Null
    foreach ($name in @('avutil-60.dll', 'swresample-6.dll', 'swscale-9.dll', 'avcodec-62.dll', 'avformat-62.dll')) {
        [IO.File]::WriteAllText((Join-Path $runtime $name), "fixture $name")
    }
    1..3 | ForEach-Object {
        [IO.File]::WriteAllText((Join-Path $media "fixture-$_.bin"), "media $_")
    }

    $missing = Assert-RuntimeAcceptanceInputs `
        -MediaRoot '' -DurationSeconds 1800 -SampleSeconds 2 -ReleaseRoot $release `
        -ThrowOnError:$false
    if ($missing.Valid -or $missing.Code -ne 'RUST_V2_REAL_MEDIA_ROOT_MISSING') {
        throw "缺媒体根必须在启动前拒绝，实际=$($missing | ConvertTo-Json -Compress)"
    }

    $short = Assert-RuntimeAcceptanceInputs `
        -MediaRoot $media -DurationSeconds 1799 -SampleSeconds 2 -ReleaseRoot $release `
        -ThrowOnError:$false
    if ($short.Valid -or $short.Code -ne 'RUST_V2_ACCEPTANCE_DURATION_INVALID') {
        throw '少于1800秒必须拒绝'
    }

    $wrongTick = Assert-RuntimeAcceptanceInputs `
        -MediaRoot $media -DurationSeconds 1800 -SampleSeconds 3 -ReleaseRoot $release `
        -ThrowOnError:$false
    if ($wrongTick.Valid -or $wrongTick.Code -ne 'RUST_V2_ACCEPTANCE_SAMPLE_INVALID') {
        throw '采样间隔必须固定2秒'
    }

    $layout = New-RuntimeAcceptanceLayout -RunId 'fixture-run'
    if (-not $layout.Root.StartsWith('C:\tmp\rust-v2-runtime-acceptance\', [StringComparison]::OrdinalIgnoreCase)) {
        throw "staging必须位于C:\tmp，实际=$($layout.Root)"
    }
    if (-not $layout.Data.StartsWith($layout.Root) -or -not $layout.Evidence.StartsWith($layout.Root)) {
        throw 'data/evidence必须位于同一隔离根'
    }

    $config = New-IsolatedNodeConfig -Port 39123
    if ($config -notmatch 'config_path = "data/node/config.toml"' -or
        $config -notmatch 'data_path = "data/node"' -or
        $config -notmatch 'enumerator = "everything"') {
        throw "相对路径配置或默认Everything错误：$config"
    }

    $before = Get-RuntimeMediaManifest -MediaRoot $media
    $same = Get-RuntimeMediaManifest -MediaRoot $media
    Assert-RuntimeMediaUnchanged -Before $before -After $same

    [IO.File]::WriteAllText((Join-Path $media 'added.bin'), 'new')
    $changed = Get-RuntimeMediaManifest -MediaRoot $media
    $detected = $false
    try {
        Assert-RuntimeMediaUnchanged -Before $before -After $changed
    }
    catch {
        $detected = $_.Exception.Message -match 'RUST_V2_REAL_MEDIA_CHANGED'
    }
    if (-not $detected) {
        throw '新增媒体文件必须被清单比较检测'
    }

    $serialized = $before | ConvertTo-Json -Depth 8 -Compress
    if ($serialized -match '(?i)password|postgresql://') {
        throw '媒体清单不得泄露PostgreSQL密码'
    }

    Write-Output 'RUST_V2_RUNTIME_ACCEPTANCE_HARNESS_PASS'
}
finally {
    if (Test-Path -LiteralPath $fixtureRoot) {
        Remove-Item -LiteralPath $fixtureRoot -Recurse -Force
    }
}
