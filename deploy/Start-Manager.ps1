$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
$exe = Join-Path $root 'gui.exe'
$config = Join-Path $root 'gui.json'
if (-not (Test-Path -LiteralPath $config -PathType Leaf)) {
    throw '请先把 gui.example.json 复制为 gui.json 并编辑 PostgreSQL 与 Agent 地址。'
}
& $exe -config $config @args
exit $LASTEXITCODE
