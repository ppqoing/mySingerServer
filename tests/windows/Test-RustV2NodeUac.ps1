<#
.SYNOPSIS
验证 Rust V2 node.exe 启动时请求管理员权限。
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string] $NodeExe
)

$ErrorActionPreference = 'Stop'
$resolvedNode = (Resolve-Path -LiteralPath $NodeExe -ErrorAction Stop).Path
$kitRoot = 'C:\Program Files (x86)\Windows Kits\10\bin'
$mt = Get-ChildItem -LiteralPath $kitRoot -Directory -ErrorAction Stop |
    Sort-Object -Property Name -Descending |
    ForEach-Object { Join-Path $_.FullName 'x64\mt.exe' } |
    Where-Object { Test-Path -LiteralPath $_ -PathType Leaf } |
    Select-Object -First 1
if (-not $mt) {
    throw 'RUST_V2_MT_EXE_MISSING'
}

$temporary = Join-Path ([IO.Path]::GetTempPath()) (
    'rust-v2-node-manifest-' + [Guid]::NewGuid().ToString('N') + '.xml')
try {
    & $mt "-inputresource:$resolvedNode;#1" "-out:$temporary" | Out-Null
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $temporary -PathType Leaf)) {
        throw "RUST_V2_NODE_MANIFEST_EXTRACT_FAILED exit=$LASTEXITCODE"
    }
    $manifest = [IO.File]::ReadAllText($temporary)
    if ($manifest -notmatch 'requestedExecutionLevel\s+level="requireAdministrator"\s+uiAccess="false"') {
        throw 'RUST_V2_NODE_UAC_MANIFEST_INVALID'
    }
    Write-Output 'RUST_V2_NODE_UAC_TEST_PASS'
}
finally {
    if (Test-Path -LiteralPath $temporary) {
        Remove-Item -LiteralPath $temporary -Force
    }
}
