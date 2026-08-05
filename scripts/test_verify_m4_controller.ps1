[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'test_verify_m4_marker.ps1')
. (Join-Path $PSScriptRoot 'test_verify_m4_bootstrap.ps1')
