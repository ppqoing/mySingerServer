param()

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$packageScript = Join-Path $PSScriptRoot 'package-portable-release.ps1'

function Assert-True {
    param([bool]$Condition, [string]$Message)
    if (-not $Condition) { throw "ASSERTION_FAILED: $Message" }
}

function Write-Utf8NoBom {
    param([string]$Path, [string]$Value)
    [IO.File]::WriteAllText($Path, $Value, [Text.UTF8Encoding]::new($false))
}

function New-CompleteStage {
    param([string]$Path)
    New-Item -ItemType Directory -Path $Path -Force | Out-Null
    foreach ($name in @(
            'nodetray.exe', 'agent.exe', 'worker.exe', 'helper.exe', 'gui.exe',
            'Everything.exe', 'Everything64.dll', 'MicrosoftEdgeWebview2Setup.exe',
            'videocore.dll', 'avcodec-fixture.dll')) {
        Write-Utf8NoBom -Path (Join-Path $Path $name) -Value "fixture:$name"
    }
    $licenses = Join-Path $Path 'licenses'
    New-Item -ItemType Directory -Path $licenses -Force | Out-Null
    Write-Utf8NoBom -Path (Join-Path $licenses 'everything-LICENSE.txt') -Value 'fixture:license'
    Write-Utf8NoBom -Path (Join-Path $licenses 'everything-NOTICE.md') -Value 'fixture:notice'
    Copy-Item -LiteralPath (Join-Path $repo 'deploy\agent.example.json') -Destination (Join-Path $Path 'agent.example.json')
    Copy-Item -LiteralPath (Join-Path $repo 'deploy\helper.example.json') -Destination (Join-Path $Path 'helper.example.json')
    $nativeFiles = @(
        foreach ($name in @('videocore.dll', 'avcodec-fixture.dll')) {
            $file = Join-Path $Path $name
            [ordered]@{ name = $name; path = "fixture/$name"; sha256 = (Get-FileHash -LiteralPath $file -Algorithm SHA256).Hash.ToLowerInvariant(); imports = @() }
        }
    )
    Write-Utf8NoBom -Path (Join-Path $Path 'native-dependencies.json') -Value (([ordered]@{ schema_version = 1; files = $nativeFiles } | ConvertTo-Json -Depth 8) + [Environment]::NewLine)
}

$testRoot = Join-Path $repo ('.tmp\test-package-portable-release-{0}' -f [Guid]::NewGuid().ToString('N'))
try {
    Assert-True (Test-Path -LiteralPath $packageScript -PathType Leaf) 'portable release entrypoint is missing'

    $stage = Join-Path $testRoot 'stage'
    $output = Join-Path $testRoot 'release'
    New-CompleteStage -Path $stage
    & $packageScript -StageDir $stage -OutputDir $output -ReleaseId 'contract-test' -BuildDate '2026-08-11' -SourceRevision 'N/A_NO_GIT_METADATA'
    $expected = @(
        'MySingerServer-compute-win-x64-contract-test.zip',
        'MySingerServer-compute-win-x64-contract-test.zip.sha256',
        'MySingerServer-manager-win-x64-contract-test.zip',
        'MySingerServer-manager-win-x64-contract-test.zip.sha256')
    foreach ($name in $expected) {
        Assert-True (Test-Path -LiteralPath (Join-Path $output $name) -PathType Leaf) "missing published artifact: $name"
    }

    $missingGuiStage = Join-Path $testRoot 'missing-gui-stage'
    Copy-Item -LiteralPath $stage -Destination $missingGuiStage -Recurse
    Remove-Item -LiteralPath (Join-Path $missingGuiStage 'gui.exe')
    $missingGuiOutput = Join-Path $testRoot 'missing-gui-release'
    $missingGuiRejected = $false
    try {
        & $packageScript -StageDir $missingGuiStage -OutputDir $missingGuiOutput -ReleaseId 'missing-gui' -BuildDate '2026-08-11'
    } catch {
        $missingGuiRejected = $true
    }
    Assert-True $missingGuiRejected 'missing gui.exe was accepted'
    $missingGuiPublished = @(Get-ChildItem -LiteralPath $missingGuiOutput -File -ErrorAction SilentlyContinue | Where-Object Name -Match 'missing-gui')
    Assert-True ($missingGuiPublished.Count -eq 0) 'failed candidate build published a partial release'

    $collisionOutput = Join-Path $testRoot 'collision-release'
    New-Item -ItemType Directory -Path $collisionOutput -Force | Out-Null
    $collisionPath = Join-Path $collisionOutput 'MySingerServer-compute-win-x64-collision.zip'
    Write-Utf8NoBom -Path $collisionPath -Value 'user-owned-existing-file'
    $collisionRejected = $false
    try {
        & $packageScript -StageDir $stage -OutputDir $collisionOutput -ReleaseId 'collision' -BuildDate '2026-08-11'
    } catch {
        $collisionRejected = $_.Exception.Message -match 'PORTABLE_RELEASE_OUTPUT_EXISTS'
    }
    Assert-True $collisionRejected 'existing target did not fail closed'
    Assert-True ((Get-Content -Raw -LiteralPath $collisionPath) -ceq 'user-owned-existing-file') 'existing target was overwritten'
    Assert-True (@(Get-ChildItem -LiteralPath $collisionOutput -Force).Count -eq 1) 'collision started candidate construction or created extra output'

    Write-Host 'PORTABLE RELEASE PACKAGE CONTRACT PASS'
}
finally {
    if (Test-Path -LiteralPath $testRoot) { Remove-Item -LiteralPath $testRoot -Recurse -Force }
}
