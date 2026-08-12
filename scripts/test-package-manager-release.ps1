param()

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$packageScript = Join-Path $PSScriptRoot 'package-manager-release.ps1'

function Assert-True {
    param([bool]$Condition, [string]$Message)
    if (-not $Condition) { throw "ASSERTION_FAILED: $Message" }
}

function Write-Utf8NoBom {
    param([string]$Path, [string]$Value)
    [IO.File]::WriteAllText($Path, $Value, [Text.UTF8Encoding]::new($false))
}

function Invoke-RejectedPackage {
    param([string]$TemplatePath, [string]$ReleaseId)
    $rejected = $false
    try {
        & $packageScript -StageDir $stage -OutputDir (Join-Path $testRoot $ReleaseId) `
            -ReleaseId $ReleaseId -BuildDate '2026-08-11' `
            -SourceRevision 'N/A_NO_GIT_METADATA' -GuiExamplePath $TemplatePath
    } catch {
        $rejected = $_.Exception.Message -match 'MANAGER_RELEASE_SENSITIVE_CONFIG'
    }
    Assert-True $rejected "unsafe manager template was accepted: $ReleaseId"
}

$testRoot = Join-Path $repo ('.tmp\test-package-manager-release-{0}' -f [Guid]::NewGuid().ToString('N'))
$stage = Join-Path $testRoot 'stage'
$output = Join-Path $testRoot 'release'
$extract = Join-Path $testRoot 'extract'

try {
    New-Item -ItemType Directory -Path $stage -Force | Out-Null
    Write-Utf8NoBom -Path (Join-Path $stage 'gui.exe') -Value 'fixture:gui.exe'
    foreach ($name in @('agent.exe', 'worker.exe', 'helper.exe', 'nodetray.exe',
            'Everything.exe', 'ffmpeg.exe', 'videocore.dll', 'WebView2Loader.dll', 'gui.json')) {
        Write-Utf8NoBom -Path (Join-Path $stage $name) -Value "must-not-ship:$name"
    }

    if (-not (Test-Path -LiteralPath $packageScript -PathType Leaf)) {
        throw 'PACKAGE_SCRIPT_MISSING'
    }
    & $packageScript -StageDir $stage -OutputDir $output -ReleaseId 'contract-test' `
        -BuildDate '2026-08-11' -SourceRevision 'N/A_NO_GIT_METADATA'

    $zipName = 'MySingerServer-manager-win-x64-contract-test.zip'
    $zipPath = Join-Path $output $zipName
    $sidecarPath = "$zipPath.sha256"
    Assert-True (Test-Path -LiteralPath $zipPath -PathType Leaf) 'ZIP was not created'
    Assert-True (Test-Path -LiteralPath $sidecarPath -PathType Leaf) 'ZIP SHA-256 sidecar was not created'

    Expand-Archive -LiteralPath $zipPath -DestinationPath $extract
    $payloadRoot = Join-Path $extract 'MySingerServer-Manager'
    Assert-True (Test-Path -LiteralPath $payloadRoot -PathType Container) 'ZIP lacks manager top-level directory'
    $topLevel = @(Get-ChildItem -LiteralPath $extract -Force)
    Assert-True ($topLevel.Count -eq 1 -and $topLevel[0].PSIsContainer -and
        $topLevel[0].Name -ceq 'MySingerServer-Manager') 'ZIP must contain exactly one manager top-level directory'

    $actualFiles = @(Get-ChildItem -LiteralPath $payloadRoot -Recurse -File | ForEach-Object {
        [IO.Path]::GetRelativePath($payloadRoot, $_.FullName).Replace('\', '/')
    } | Sort-Object)
    $expectedFiles = @('gui.exe', 'gui.example.json', 'Start-Manager.ps1',
        'README-管理端部署.md', 'release-manifest.json') | Sort-Object
    Assert-True (@(Compare-Object -ReferenceObject $expectedFiles -DifferenceObject $actualFiles).Count -eq 0) `
        'ZIP file list differs from the portable manager contract'
    foreach ($forbidden in @('agent.exe', 'worker.exe', 'helper.exe', 'nodetray.exe',
            'Everything.exe', 'ffmpeg.exe', 'videocore.dll', 'WebView2Loader.dll', 'gui.json')) {
        Assert-True (-not ($actualFiles -contains $forbidden)) "forbidden file shipped: $forbidden"
    }
    $guiExample = Get-Content -Raw -LiteralPath (Join-Path $payloadRoot 'gui.example.json') |
        ConvertFrom-Json
    Assert-True ([string]$guiExample.listen_addr -ceq '127.0.0.1:18081') `
        'manager template must use the dedicated loopback port 18081'

    $startScript = Get-Content -Raw -LiteralPath (Join-Path $payloadRoot 'Start-Manager.ps1')
    Assert-True ($startScript -match `
        '& \(Join-Path \$root ''gui\.exe''\) -config \(Join-Path \$root ''gui\.json''\) @args') `
        'manager launch script must invoke gui.exe with the absolute sibling gui.json path'
    Assert-True ($startScript -notmatch '(?im)^\s*throw\b') `
        'manager launch script must not reject a missing gui.json'

    $readme = Get-Content -Raw -LiteralPath (Join-Path $payloadRoot 'README-管理端部署.md')
    Assert-True ($readme -match '首次双击.*自动生成.*gui\.json') `
        'manager README must explain first-run gui.json creation'
    Assert-True ($readme -match 'PostgreSQL.*Agent.*不可用.*设置页') `
        'manager README must explain degraded startup into settings'
    Assert-True ($readme -match '保存.*自动重启') `
        'manager README must explain automatic restart after saving settings'

    $manifest = Get-Content -Raw -LiteralPath (Join-Path $payloadRoot 'release-manifest.json') | ConvertFrom-Json
    Assert-True ($manifest.release_kind -ceq 'remote-manager-portable') 'wrong release kind'
    Assert-True ($manifest.portable_root -ceq '.') 'wrong portable root'
    $manifestFiles = @($manifest.files)
    Assert-True ($manifestFiles.Count -eq 4) 'manifest must list every payload file except itself'
    foreach ($file in $manifestFiles) {
        $path = Join-Path $payloadRoot ([string]$file.path).Replace('/', '\\')
        Assert-True (Test-Path -LiteralPath $path -PathType Leaf) "manifest file missing: $($file.path)"
        Assert-True ([long]$file.size -eq (Get-Item -LiteralPath $path).Length) "manifest size mismatch: $($file.path)"
        Assert-True ([string]$file.sha256 -ceq (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()) "manifest hash mismatch: $($file.path)"
    }
    $zipHash = (Get-FileHash -LiteralPath $zipPath -Algorithm SHA256).Hash.ToLowerInvariant()
    Assert-True ((Get-Content -Raw -LiteralPath $sidecarPath).Trim() -ceq "$zipHash  $zipName") 'ZIP SHA-256 sidecar mismatch'

    $passwordTemplate = Join-Path $testRoot 'password.json'
    Write-Utf8NoBom -Path $passwordTemplate -Value '{"pg_dsn":"postgres://dedup:secret@127.0.0.1:5432/dedup","agents":[{"addr":"127.0.0.1:9101"}]}'
    Invoke-RejectedPackage -TemplatePath $passwordTemplate -ReleaseId 'password'
    $tokenTemplate = Join-Path $testRoot 'token.json'
    Write-Utf8NoBom -Path $tokenTemplate -Value '{"pg_dsn":"postgres://dedup@127.0.0.1:5432/dedup?token=secret","agents":[{"addr":"127.0.0.1:9101"}]}'
    Invoke-RejectedPackage -TemplatePath $tokenTemplate -ReleaseId 'token'
    foreach ($queryCase in @(
            [ordered]@{ name = 'token-no-value'; query = 'token' },
            [ordered]@{ name = 'encoded-token'; query = 'to%6ben=secret' },
            [ordered]@{ name = 'mixed-case-access-token'; query = 'AcCeSs_ToKeN=secret' },
            [ordered]@{ name = 'encoded-api-key-no-value'; query = 'api%5Fkey' })) {
        $queryTemplate = Join-Path $testRoot ("query-{0}.json" -f $queryCase.name)
        Write-Utf8NoBom -Path $queryTemplate -Value (
            '{"pg_dsn":"postgres://dedup@127.0.0.1:5432/dedup?' + $queryCase.query +
            '","agents":[{"addr":"127.0.0.1:9101"}]}')
        Invoke-RejectedPackage -TemplatePath $queryTemplate -ReleaseId $queryCase.name
    }
    $lanTemplate = Join-Path $testRoot 'lan.json'
    Write-Utf8NoBom -Path $lanTemplate -Value '{"pg_dsn":"postgres://dedup@127.0.0.1:5432/dedup","agents":[{"addr":"127.0.0.1:9101"},{"addr":"192.168.1.20:9101"}]}'
    Invoke-RejectedPackage -TemplatePath $lanTemplate -ReleaseId 'lan'
    foreach ($dsnCase in @(
            [ordered]@{ name = 'postgres-lan-host'; dsn = 'postgres://dedup@192.168.1.20:5432/dedup' },
            [ordered]@{ name = 'postgres-internal-dns'; dsn = 'postgresql://dedup@db.internal.example:5432/dedup' },
            [ordered]@{ name = 'wrong-dsn-scheme'; dsn = 'https://dedup@127.0.0.1:5432/dedup' })) {
        $dsnTemplate = Join-Path $testRoot ("{0}.json" -f $dsnCase.name)
        Write-Utf8NoBom -Path $dsnTemplate -Value (
            '{"pg_dsn":"' + $dsnCase.dsn +
            '","agents":[{"addr":"127.0.0.1:9101"}]}')
        Invoke-RejectedPackage -TemplatePath $dsnTemplate -ReleaseId $dsnCase.name
    }
    $safePlaceholderTemplate = Join-Path $testRoot 'safe-placeholder.json'
    Write-Utf8NoBom -Path $safePlaceholderTemplate -Value `
        '{"pg_dsn":"postgresql://dedup@localhost:5432/dedup","agents":[{"addr":"127.0.0.1:9101"}]}'
    $safePlaceholderOutput = Join-Path $testRoot 'safe-placeholder-release'
    & $packageScript -StageDir $stage -OutputDir $safePlaceholderOutput `
        -ReleaseId 'safe-placeholder' -BuildDate '2026-08-11' `
        -SourceRevision 'N/A_NO_GIT_METADATA' `
        -GuiExamplePath $safePlaceholderTemplate
    Assert-True (Test-Path -LiteralPath (Join-Path $safePlaceholderOutput `
            'MySingerServer-manager-win-x64-safe-placeholder.zip') -PathType Leaf) `
        'postgresql localhost placeholder was rejected'

    Write-Host "MANAGER RELEASE PACKAGE CONTRACT PASS files=$($actualFiles.Count)"
}
finally {
    if (Test-Path -LiteralPath $testRoot) { Remove-Item -LiteralPath $testRoot -Recurse -Force }
}
