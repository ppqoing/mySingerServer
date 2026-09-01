<#
.SYNOPSIS
验证 Rust V2 第三方许可证生成器能处理 Windows CRLF Cargo.lock。

.DESCRIPTION
测试直接运行真实生成脚本，不替换 Cargo、cargo-about 或 metadata。当前 Windows 工作树中的
Cargo.lock 必须包含 CRLF；生成器需要成功输出许可证正文与固定许可证文件。
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

# 定位真实仓库、生成脚本与当前锁文件，确保测试覆盖正式打包路径。
$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$generator = Join-Path $repositoryRoot 'scripts\generate-third-party-notices.ps1'
$cargoLockPath = Join-Path $repositoryRoot 'Cargo.lock'
$pwsh = (Get-Command pwsh -ErrorAction Stop).Source

# 每次测试使用独立临时输出目录，避免修改正式 dist 与已有发布证据。
$fixtureRoot = Join-Path ([IO.Path]::GetTempPath()) ("rust-v2-notices-test-" + [Guid]::NewGuid().ToString('N'))
$destination = Join-Path $fixtureRoot 'licenses'

try {
    $lockText = [IO.File]::ReadAllText($cargoLockPath)
    if (-not $lockText.Contains("`r`n")) {
        throw '测试前置条件失败：当前 Cargo.lock 不包含 CRLF，无法覆盖 Windows 行尾回归。'
    }

    $output = & $pwsh -NoProfile -File $generator -Destination $destination 2>&1
    $exitCode = $LASTEXITCODE
    $outputText = $output | Out-String
    if ($exitCode -ne 0 -or $outputText -notmatch 'RUST_V2_NOTICES_PASS') {
        throw "CRLF Cargo.lock 许可证生成失败，exit=$exitCode：$outputText"
    }

    foreach ($name in @(
        'Rust-Third-Party-Licenses.html',
        'Slint-Royalty-Free-2.0.txt',
        'PDQ-BSD-3-Clause.txt'
    )) {
        $path = Join-Path $destination $name
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
            throw "许可证生成结果缺少文件：$name"
        }
    }

    Write-Output 'RUST_V2_THIRD_PARTY_NOTICES_TEST_PASS'
}
finally {
    if (Test-Path -LiteralPath $fixtureRoot) {
        Remove-Item -LiteralPath $fixtureRoot -Recurse -Force
    }
}
