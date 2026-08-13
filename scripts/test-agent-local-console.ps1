[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string]$StageDir,
    [string]$PostgresDSN = ''
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$stage = if ([IO.Path]::IsPathRooted($StageDir)) { $StageDir } else { Join-Path $repo $StageDir }
if (-not (Test-Path -LiteralPath $stage -PathType Container)) { throw "LOCAL_CONSOLE_STAGE_NOT_FOUND path=$stage" }

foreach ($name in @('nodetray.exe','agent.exe','worker.exe','helper.exe','Everything.exe','Everything64.dll','videocore.dll','agent.example.json')) {
    if (-not (Test-Path -LiteralPath (Join-Path $stage $name) -PathType Leaf)) { throw "LOCAL_CONSOLE_REQUIRED_FILE_MISSING name=$name" }
}

$agent = Get-Content -Raw -LiteralPath (Join-Path $stage 'agent.example.json') | ConvertFrom-Json
if ([string]$agent.pg_dsn) { throw 'LOCAL_CONSOLE_TEMPLATE_POSTGRES_MUST_BE_OPTIONAL' }
if ([string]$agent.listen_addr -notmatch ':\d+$') { throw 'LOCAL_CONSOLE_LISTEN_PORT_REQUIRED' }

if ($PostgresDSN) {
    try { $uri = [Uri]::new($PostgresDSN) } catch { throw 'LOCAL_CONSOLE_TEST_POSTGRES_DSN_INVALID' }
    if ($uri.Scheme -notin @('postgres','postgresql') -or $uri.AbsolutePath -notmatch '(?i)test') { throw 'LOCAL_CONSOLE_TEST_POSTGRES_DSN_MUST_NAME_TEST_DATABASE' }
}

$sourceMatches = @(rg -n --glob '*.go' 'AgentPipeName|internal/agentcontrol' (Join-Path $repo 'cmd') (Join-Path $repo 'internal') (Join-Path $repo 'nodetray'))
if ($sourceMatches.Count) { throw "LOCAL_CONSOLE_AGENT_PIPE_REMAINS`n$($sourceMatches -join "`n")" }

Write-Host "AGENT LOCAL CONSOLE STATIC PASS stage=$stage postgres=$([bool]$PostgresDSN)"
Write-Host 'RUNTIME PARTIAL: destructive media deletion and long Everything indexing require an operator-provided disposable media fixture; this script does not delete user files.'
