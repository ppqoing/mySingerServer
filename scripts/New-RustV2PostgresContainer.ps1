<#
.SYNOPSIS
创建并验证 mySingerServer Rust V2 的持久化 PostgreSQL 容器。

.DESCRIPTION
脚本只面向新的空数据卷：创建命名卷与 PostgreSQL 16 Alpine 容器，并通过官方
docker-entrypoint-initdb.d 入口执行 deploy/central-v2.sql。脚本不会迁移、覆盖或删除已有数据。

.PARAMETER HostPort
宿主机回环地址端口，默认使用 15439，避免占用常见的本机 PostgreSQL 5432 端口。

.PARAMETER DockerExecutable
Docker CLI 路径。默认从 PATH 使用 docker；该参数也用于隔离行为测试。
#>
[CmdletBinding()]
param(
    [ValidatePattern('^[A-Za-z0-9][A-Za-z0-9_.-]*$')]
    [string] $ContainerName = 'mysingerserver-rust-v2-postgres',

    [ValidatePattern('^[A-Za-z0-9][A-Za-z0-9_.-]*$')]
    [string] $VolumeName = 'mysingerserver-rust-v2-postgres-data',

    [ValidateRange(1, 65535)]
    [int] $HostPort = 15439,

    [ValidatePattern('^[A-Za-z_][A-Za-z0-9_]*$')]
    [string] $DatabaseName = 'dedup_v2',

    [ValidatePattern('^[A-Za-z_][A-Za-z0-9_]*$')]
    [string] $DatabaseUser = 'dedup',

    [ValidateNotNullOrEmpty()]
    [string] $DatabasePassword = 'dedup',

    [ValidateNotNullOrEmpty()]
    [string] $Image = 'postgres:16-alpine',

    [ValidateNotNullOrEmpty()]
    [string] $DockerExecutable = 'docker'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

# 固定仓库根与 schema 路径，避免从当前工作目录读取错误的初始化文件。
$repositoryRoot = Split-Path -Parent $PSScriptRoot
$schemaPath = [IO.Path]::GetFullPath((Join-Path $repositoryRoot 'deploy\central-v2.sql'))
$expectedSchemaSummary = 'mysingerserver-rust-v2-central-schema-3|3|22'

function Invoke-DockerCommand {
    <# 调用 Docker CLI，统一收集退出码和输出，并为允许失败的存在性探测保留结果。 #>
    param(
        [Parameter(Mandatory)] [string[]] $Arguments,
        [switch] $AllowFailure
    )

    $previousNativeErrorPreference = $PSNativeCommandUseErrorActionPreference
    try {
        $PSNativeCommandUseErrorActionPreference = $false
        $commandOutput = @(& $DockerExecutable @Arguments 2>&1)
        $commandExitCode = $LASTEXITCODE
    }
    finally {
        $PSNativeCommandUseErrorActionPreference = $previousNativeErrorPreference
    }

    if ($commandExitCode -ne 0 -and -not $AllowFailure) {
        $displayArguments = ($Arguments -join ' ')
        $displayOutput = ($commandOutput -join ' | ')
        throw "RUST_V2_POSTGRES_DOCKER_COMMAND_FAILED exit=$commandExitCode arguments=$displayArguments output=$displayOutput"
    }

    return [pscustomobject]@{
        ExitCode = $commandExitCode
        Output = @($commandOutput | ForEach-Object { [string] $_ })
    }
}

function Test-DockerObjectExists {
    <# 使用 Docker inspect 的退出码判断命名对象是否存在，不解析本地化错误文本。 #>
    param(
        [Parameter(Mandatory)] [ValidateSet('container', 'volume')] [string] $Kind,
        [Parameter(Mandatory)] [string] $Name
    )

    $inspectResult = Invoke-DockerCommand -Arguments @($Kind, 'inspect', $Name) -AllowFailure
    return $inspectResult.ExitCode -eq 0
}

function Wait-PostgresHealthy {
    <# 等待容器健康检查完成；明确 unhealthy 或 60 秒超时都会终止后续 schema 验证。 #>
    param([Parameter(Mandatory)] [string] $Name)

    for ($attempt = 1; $attempt -le 60; $attempt++) {
        $healthResult = Invoke-DockerCommand `
            -Arguments @('inspect', '--format', '{{.State.Health.Status}}', $Name) `
            -AllowFailure
        $health = ($healthResult.Output -join '').Trim()
        if ($healthResult.ExitCode -eq 0 -and $health -ceq 'healthy') {
            return
        }
        if ($health -ceq 'unhealthy') {
            throw "RUST_V2_POSTGRES_UNHEALTHY container=$Name"
        }
        Start-Sleep -Seconds 1
    }

    throw "RUST_V2_POSTGRES_HEALTH_TIMEOUT container=$Name"
}

function Assert-RustV2Schema {
    <# 在容器内用 psql 校验 schema 3 身份、版本和当前 22 张业务表。 #>
    param([Parameter(Mandatory)] [string] $Name)

    $schemaQuery = "SELECT concat((SELECT value FROM schema_metadata WHERE key='schema_id'), chr(124), (SELECT value FROM schema_metadata WHERE key='schema_version'), chr(124), (SELECT count(*)::text FROM information_schema.tables WHERE table_schema='public' AND table_type='BASE TABLE'));"
    $schemaResult = Invoke-DockerCommand -Arguments @(
        'exec', '--env', "PGPASSWORD=$DatabasePassword", $Name,
        'psql', '--tuples-only', '--no-align', '--set', 'ON_ERROR_STOP=1',
        '--username', $DatabaseUser, '--dbname', $DatabaseName,
        '--command', $schemaQuery
    )
    $schemaSummary = @(
        $schemaResult.Output |
            ForEach-Object { $_.Trim() } |
            Where-Object { -not [string]::IsNullOrWhiteSpace($_) }
    ) | Select-Object -Last 1
    if ($schemaSummary -cne $expectedSchemaSummary) {
        throw "RUST_V2_POSTGRES_SCHEMA_INVALID expected=$expectedSchemaSummary actual=$schemaSummary"
    }
}

if (-not (Test-Path -LiteralPath $schemaPath -PathType Leaf)) {
    throw "RUST_V2_POSTGRES_SCHEMA_MISSING path=$schemaPath"
}
if (-not (Get-Command $DockerExecutable -ErrorAction SilentlyContinue)) {
    throw "RUST_V2_POSTGRES_DOCKER_MISSING executable=$DockerExecutable"
}

# 先验证 Docker 服务，再检查对象冲突，防止把服务不可用误判成对象不存在。
Invoke-DockerCommand -Arguments @('version', '--format', '{{.Server.Version}}') | Out-Null
if (Test-DockerObjectExists -Kind 'container' -Name $ContainerName) {
    throw "RUST_V2_POSTGRES_CONTAINER_EXISTS name=$ContainerName"
}
if (Test-DockerObjectExists -Kind 'volume' -Name $VolumeName) {
    throw "RUST_V2_POSTGRES_VOLUME_EXISTS name=$VolumeName"
}

Invoke-DockerCommand -Arguments @('volume', 'create', $VolumeName) | Out-Null
$dataMount = "type=volume,source=$VolumeName,target=/var/lib/postgresql/data"
$schemaMount = "type=bind,source=$schemaPath,target=/docker-entrypoint-initdb.d/001-central-v2.sql,readonly"
$healthCommand = "pg_isready -U $DatabaseUser -d $DatabaseName"
Invoke-DockerCommand -Arguments @(
    'run', '--detach', '--name', $ContainerName,
    '--restart', 'unless-stopped',
    '--publish', "127.0.0.1:${HostPort}:5432",
    '--env', "POSTGRES_DB=$DatabaseName",
    '--env', "POSTGRES_USER=$DatabaseUser",
    '--env', "POSTGRES_PASSWORD=$DatabasePassword",
    '--mount', $dataMount,
    '--mount', $schemaMount,
    '--health-cmd', $healthCommand,
    '--health-interval', '2s',
    '--health-timeout', '2s',
    '--health-retries', '30',
    $Image
) | Out-Null

Wait-PostgresHealthy -Name $ContainerName
Assert-RustV2Schema -Name $ContainerName

# 输出适合直接写入 desktop 配置的连接地址；特殊字符使用 URI 转义。
$encodedUser = [Uri]::EscapeDataString($DatabaseUser)
$encodedPassword = [Uri]::EscapeDataString($DatabasePassword)
$encodedDatabase = [Uri]::EscapeDataString($DatabaseName)
Write-Output "RUST_V2_POSTGRES_CONTAINER_PASS name=$ContainerName volume=$VolumeName"
Write-Output "postgresql://${encodedUser}:${encodedPassword}@127.0.0.1:$HostPort/$encodedDatabase"
