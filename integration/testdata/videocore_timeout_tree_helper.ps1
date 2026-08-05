[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$PidFile
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$hostPath = (Get-Process -Id $PID).Path
$child = Start-Process `
    -FilePath $hostPath `
    -ArgumentList @('-NoProfile', '-Command', 'Start-Sleep -Seconds 300') `
    -PassThru `
    -WindowStyle Hidden

[IO.File]::WriteAllLines(
    $PidFile,
    @([string]$PID, [string]$child.Id),
    [Text.UTF8Encoding]::new($false)
)

Start-Sleep -Seconds 300
