param()

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$packageScript = Join-Path $PSScriptRoot 'package-node-release.ps1'

function Assert-True {
    param(
        [bool]$Condition,
        [string]$Message
    )
    if (-not $Condition) {
        throw "ASSERTION_FAILED: $Message"
    }
}

function Write-Utf8NoBom {
    param(
        [string]$Path,
        [string]$Value
    )
    [IO.File]::WriteAllText(
        $Path,
        $Value,
        [Text.UTF8Encoding]::new($false))
}

$testRoot = Join-Path $repo (
    '.tmp\test-package-node-release-{0}' -f [Guid]::NewGuid().ToString('N'))
$stage = Join-Path $testRoot 'full-stage'
$output = Join-Path $testRoot 'release'
$extract = Join-Path $testRoot 'extract'

try {
    New-Item -ItemType Directory -Path $stage -Force | Out-Null
    foreach ($name in @(
            'nodetray.exe',
            'agent.exe',
            'worker.exe',
            'helper.exe',
            'Everything64.dll',
            'MicrosoftEdgeWebview2Setup.exe',
            'videocore.dll',
            'avcodec-fixture.dll')) {
        Write-Utf8NoBom -Path (Join-Path $stage $name) -Value "fixture:$name"
    }
    Copy-Item -LiteralPath (Join-Path $repo 'deploy\agent.example.json') `
        -Destination (Join-Path $stage 'agent.example.json')
    Copy-Item -LiteralPath (Join-Path $repo 'deploy\helper.example.json') `
        -Destination (Join-Path $stage 'helper.example.json')

    # A full build contains center/config files. The node package must ignore them.
    foreach ($name in @('gui.exe', 'agent.json', 'helper.json', 'gui.json')) {
        Write-Utf8NoBom -Path (Join-Path $stage $name) -Value "must-not-ship:$name"
    }

    $nativeFiles = @(
        foreach ($name in @('videocore.dll', 'avcodec-fixture.dll')) {
            $path = Join-Path $stage $name
            [ordered]@{
                name = $name
                path = "fixture/$name"
                sha256 = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()
                imports = @()
            }
        }
    )
    $nativeManifest = [ordered]@{
        schema_version = 1
        files = $nativeFiles
    }
    Write-Utf8NoBom -Path (Join-Path $stage 'native-dependencies.json') `
        -Value (($nativeManifest | ConvertTo-Json -Depth 8) + [Environment]::NewLine)

    if (-not (Test-Path -LiteralPath $packageScript -PathType Leaf)) {
        throw 'PACKAGE_SCRIPT_MISSING'
    }
    & $packageScript `
        -StageDir $stage `
        -OutputDir $output `
        -ReleaseId 'contract-test' `
        -BuildDate '2026-08-03' `
        -SourceRevision 'N/A_NO_GIT_METADATA'

    $zipName = 'MySingerServer-node-win-x64-contract-test.zip'
    $zipPath = Join-Path $output $zipName
    $sidecarPath = "$zipPath.sha256"
    Assert-True (Test-Path -LiteralPath $zipPath -PathType Leaf) 'ZIP was not created'
    Assert-True (Test-Path -LiteralPath $sidecarPath -PathType Leaf) 'ZIP SHA-256 sidecar was not created'

    Expand-Archive -LiteralPath $zipPath -DestinationPath $extract
    $payloadRoot = Join-Path $extract 'MySingerServer'
    Assert-True (Test-Path -LiteralPath $payloadRoot -PathType Container) 'ZIP lacks MySingerServer top-level directory'
    $topLevel = @(Get-ChildItem -LiteralPath $extract -Force)
    Assert-True ($topLevel.Count -eq 1 -and $topLevel[0].PSIsContainer) 'ZIP must contain exactly one top-level directory'

    $actualFiles = @(
        Get-ChildItem -LiteralPath $payloadRoot -Recurse -File |
            ForEach-Object {
                [IO.Path]::GetRelativePath($payloadRoot, $_.FullName).Replace('\', '/')
            } |
            Sort-Object
    )
    $expectedFiles = @(
        'Everything64.dll',
        'MicrosoftEdgeWebview2Setup.exe',
        'README-节点部署.md',
        'agent.example.json',
        'agent.exe',
        'avcodec-fixture.dll',
        'helper.example.json',
        'helper.exe',
        'licenses/ffmpeg-LICENSE.txt',
        'licenses/ffmpeg-NOTICE.md',
        'licenses/webview2-NOTICE.md',
        'native-dependencies.json',
        'nodetray.exe',
        'release-manifest.json',
        'videocore.dll',
        'worker.exe'
    ) | Sort-Object
    $difference = @(Compare-Object -ReferenceObject $expectedFiles -DifferenceObject $actualFiles)
    Assert-True ($difference.Count -eq 0) (
        'ZIP file list differs: ' + (($difference | Out-String).Trim()))

    foreach ($forbidden in @('gui.exe', 'agent.json', 'helper.json', 'gui.json')) {
        Assert-True (-not ($actualFiles -contains $forbidden)) "forbidden file shipped: $forbidden"
    }

    $releaseManifest = Get-Content -Raw -LiteralPath (
        Join-Path $payloadRoot 'release-manifest.json') | ConvertFrom-Json
    Assert-True ($releaseManifest.release_kind -ceq 'media-node-minimal') 'wrong release kind'
    Assert-True ($releaseManifest.install_root -ceq 'C:\Program Files\MySingerServer\') 'wrong fixed install root'
    Assert-True ($releaseManifest.helper.default_enabled -eq $false) 'Helper must be disabled by default'
    Assert-True ($releaseManifest.helper.requires_administrator -eq $true) 'Helper must record its administrator requirement'
    Assert-True ($releaseManifest.source_revision -ceq 'N/A_NO_GIT_METADATA') 'wrong source revision marker'

    $sidecar = (Get-Content -Raw -LiteralPath $sidecarPath).Trim()
    $zipHash = (Get-FileHash -LiteralPath $zipPath -Algorithm SHA256).Hash.ToLowerInvariant()
    Assert-True ($sidecar -ceq "$zipHash  $zipName") 'ZIP SHA-256 sidecar mismatch'

    $badNativeStage = Join-Path $testRoot 'bad-native-stage'
    Copy-Item -LiteralPath $stage -Destination $badNativeStage -Recurse
    Write-Utf8NoBom -Path (Join-Path $badNativeStage 'avcodec-fixture.dll') `
        -Value 'tampered-after-manifest'
    $badNativeRejected = $false
    try {
        $oldWarningPreference = $WarningPreference
        $WarningPreference = 'SilentlyContinue'
        & $packageScript `
            -StageDir $badNativeStage `
            -OutputDir (Join-Path $testRoot 'bad-native-release') `
            -ReleaseId 'bad-native' `
            -BuildDate '2026-08-03'
    }
    catch {
        $badNativeRejected = $_.Exception.Message -match `
            'NODE_RELEASE_NATIVE_HASH_MISMATCH'
    }
    finally {
        $WarningPreference = $oldWarningPreference
    }
    Assert-True $badNativeRejected `
        'native dependency hash mismatch was accepted'

    $credentialStage = Join-Path $testRoot 'credential-stage'
    Copy-Item -LiteralPath $stage -Destination $credentialStage -Recurse
    $credentialAgent = Get-Content -Raw -LiteralPath (
        Join-Path $credentialStage 'agent.example.json') | ConvertFrom-Json
    $credentialAgent.pg_dsn = 'postgres://dedup:real-secret@127.0.0.1:5432/dedup'
    Write-Utf8NoBom -Path (Join-Path $credentialStage 'agent.example.json') `
        -Value (($credentialAgent | ConvertTo-Json -Depth 8) + [Environment]::NewLine)
    $credentialRejected = $false
    try {
        $oldWarningPreference = $WarningPreference
        $WarningPreference = 'SilentlyContinue'
        & $packageScript `
            -StageDir $credentialStage `
            -OutputDir (Join-Path $testRoot 'credential-release') `
            -ReleaseId 'credential' `
            -BuildDate '2026-08-03'
    }
    catch {
        $credentialRejected = $_.Exception.Message -match `
            'NODE_RELEASE_SENSITIVE_CONFIG'
    }
    finally {
        $WarningPreference = $oldWarningPreference
    }
    Assert-True $credentialRejected `
        'password-bearing PostgreSQL example DSN was accepted'

    $helperRootStage = Join-Path $testRoot 'helper-root-stage'
    Copy-Item -LiteralPath $stage -Destination $helperRootStage -Recurse
    $helperRootConfig = Get-Content -Raw -LiteralPath (
        Join-Path $helperRootStage 'helper.example.json') | ConvertFrom-Json
    $helperRootConfig.allowed_roots = @('I:\tmp')
    Write-Utf8NoBom -Path (Join-Path $helperRootStage 'helper.example.json') `
        -Value (($helperRootConfig | ConvertTo-Json -Depth 8) + [Environment]::NewLine)
    $helperRootRejected = $false
    try {
        $oldWarningPreference = $WarningPreference
        $WarningPreference = 'SilentlyContinue'
        & $packageScript `
            -StageDir $helperRootStage `
            -OutputDir (Join-Path $testRoot 'helper-root-release') `
            -ReleaseId 'helper-root' `
            -BuildDate '2026-08-03'
    }
    catch {
        $helperRootRejected = $_.Exception.Message -match `
            'NODE_RELEASE_SENSITIVE_CONFIG'
    }
    finally {
        $WarningPreference = $oldWarningPreference
    }
    Assert-True $helperRootRejected `
        'Helper example with a live allowed_roots path was accepted'

    Write-Host "NODE RELEASE PACKAGE CONTRACT PASS files=$($actualFiles.Count)"
}
finally {
    if (Test-Path -LiteralPath $testRoot) {
        Remove-Item -LiteralPath $testRoot -Recurse -Force
    }
}
