$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
& (Join-Path $root 'gui.exe') -config (Join-Path $root 'gui.json') @args
exit $LASTEXITCODE
