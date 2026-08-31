<#
.SYNOPSIS
验证 Rust V2 PostgreSQL 持久化容器创建脚本的 Docker 边界行为。
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

# 仓库与脚本路径用于执行真实 PowerShell 入口，Docker 本身由隔离替身接管。
$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$containerScript = Join-Path $repositoryRoot 'scripts\New-RustV2PostgresContainer.ps1'
$fixtureRoot = Join-Path ([IO.Path]::GetTempPath()) ("rust-v2-postgres-container-" + [Guid]::NewGuid().ToString('N'))
$fakeDocker = Join-Path $fixtureRoot 'docker.cmd'
$dockerLog = Join-Path $fixtureRoot 'docker.log'

try {
    if (-not (Test-Path -LiteralPath $containerScript -PathType Leaf)) {
        throw "RUST_V2_POSTGRES_SCRIPT_MISSING path=$containerScript"
    }

    New-Item -ItemType Directory -Path $fixtureRoot -Force | Out-Null
    # Docker 替身仅模拟脚本依赖的 CLI 边界，并将每次调用完整记录供行为断言。
    $fakeDockerSource = @'
@echo off
echo %*>>"%RUST_V2_POSTGRES_DOCKER_LOG%"
if "%1"=="version" (
  echo 26.1.0
  exit /b 0
)
if "%1"=="container" if "%2"=="inspect" exit /b 1
if "%1"=="volume" if "%2"=="inspect" (
  if "%RUST_V2_POSTGRES_FAKE_MODE%"=="existing-volume" (
    echo mysingerserver-rust-v2-postgres-data
    exit /b 0
  )
  exit /b 1
)
if "%1"=="volume" if "%2"=="create" (
  echo mysingerserver-rust-v2-postgres-data
  exit /b 0
)
if "%1"=="run" (
  echo fixture-container-id
  exit /b 0
)
if "%1"=="inspect" (
  echo healthy
  exit /b 0
)
if "%1"=="exec" (
  echo mysingerserver-rust-v2-central-schema-3^|3^|22
  exit /b 0
)
exit /b 2
'@
    [IO.File]::WriteAllText($fakeDocker, $fakeDockerSource, [Text.Encoding]::ASCII)

    $env:RUST_V2_POSTGRES_DOCKER_LOG = $dockerLog
    $env:RUST_V2_POSTGRES_FAKE_MODE = 'new'
    $output = @(& $containerScript -DockerExecutable $fakeDocker)
    $calls = @(Get-Content -LiteralPath $dockerLog)
    $joinedCalls = $calls -join "`n"

    if ($joinedCalls -notmatch 'volume create mysingerserver-rust-v2-postgres-data') {
        throw '新建容器前必须创建专用命名卷'
    }
    if ($joinedCalls -notmatch '--mount type=volume,source=mysingerserver-rust-v2-postgres-data,target=/var/lib/postgresql/data') {
        throw "容器必须把命名卷挂载到PostgreSQL数据目录：$joinedCalls"
    }
    if ($joinedCalls -notmatch '--mount type=bind,source=.*central-v2\.sql,target=/docker-entrypoint-initdb\.d/001-central-v2\.sql,readonly') {
        throw "空卷初始化必须挂载当前central-v2.sql：$joinedCalls"
    }
    if ($joinedCalls -notmatch '--publish 127\.0\.0\.1:15439:5432') {
        throw '默认端口必须只绑定本机15439'
    }
    if ($joinedCalls -notmatch '--restart unless-stopped') {
        throw '持久化容器必须启用unless-stopped重启策略'
    }
    if ($joinedCalls -notmatch 'exec .*psql') {
        throw "健康后必须在容器内验证Rust V2 schema：$joinedCalls"
    }
    if (($output -join "`n") -notmatch 'postgresql://dedup:dedup@127\.0\.0\.1:15439/dedup_v2') {
        throw "脚本必须输出可直接使用的连接地址：$($output -join ' | ')"
    }

    Remove-Item -LiteralPath $dockerLog -Force
    $env:RUST_V2_POSTGRES_FAKE_MODE = 'new'
    $lanOutput = @(& $containerScript -DockerExecutable $fakeDocker -HostAddress '192.168.1.17')
    $lanCalls = @(Get-Content -LiteralPath $dockerLog)
    if (($lanCalls -join "`n") -notmatch '--publish 192\.168\.1\.17:15439:5432') {
        throw "显式主机地址必须发布到指定 LAN 地址：$($lanCalls -join ' | ')"
    }
    if (($lanOutput -join "`n") -notmatch 'postgresql://dedup:dedup@192\.168\.1\.17:15439/dedup_v2') {
        throw "脚本必须输出指定主机地址的连接串：$($lanOutput -join ' | ')"
    }

    foreach ($invalidHostAddress in @('db.example.test', '0.0.0.0', '*', '192.168.1.17;--privileged')) {
        Remove-Item -LiteralPath $dockerLog -Force -ErrorAction SilentlyContinue
        $rejectedHostAddress = $false
        try {
            & $containerScript -DockerExecutable $fakeDocker -HostAddress $invalidHostAddress | Out-Null
        }
        catch {
            $rejectedHostAddress = $true
        }
        if (-not $rejectedHostAddress) {
            throw "非法主机地址必须拒绝：$invalidHostAddress"
        }
        if (Test-Path -LiteralPath $dockerLog) {
            $invalidCalls = @(Get-Content -LiteralPath $dockerLog)
            if ($invalidCalls.Count -gt 0) {
                throw "非法主机地址不得调用 Docker：$invalidHostAddress"
            }
        }
    }

    Remove-Item -LiteralPath $dockerLog -Force -ErrorAction SilentlyContinue
    $env:RUST_V2_POSTGRES_FAKE_MODE = 'existing-volume'
    $rejectedExistingVolume = $false
    try {
        & $containerScript -DockerExecutable $fakeDocker | Out-Null
    }
    catch {
        $rejectedExistingVolume = $_.Exception.Message -match '^RUST_V2_POSTGRES_VOLUME_EXISTS'
    }
    if (-not $rejectedExistingVolume) {
        throw '同名数据卷已存在时必须拒绝覆盖'
    }
    $reuseCalls = @(Get-Content -LiteralPath $dockerLog)
    if (($reuseCalls -join "`n") -match '(^|\n)run ') {
        throw '拒绝已有数据卷后不得继续创建容器'
    }

    Write-Output 'RUST_V2_POSTGRES_CONTAINER_TEST_PASS'
}
finally {
    Remove-Item Env:\RUST_V2_POSTGRES_DOCKER_LOG -ErrorAction SilentlyContinue
    Remove-Item Env:\RUST_V2_POSTGRES_FAKE_MODE -ErrorAction SilentlyContinue
    if (Test-Path -LiteralPath $fixtureRoot) {
        Remove-Item -LiteralPath $fixtureRoot -Recurse -Force
    }
}
