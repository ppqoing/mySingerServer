[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string]$StageDir,

    [string]$OutputDir = 'artifacts\releases',

    [ValidatePattern('^[A-Za-z0-9][A-Za-z0-9._-]*$')]
    [string]$ReleaseId = (Get-Date -Format 'yyyyMMdd'),

    [ValidatePattern('^\d{4}-\d{2}-\d{2}$')]
    [string]$BuildDate = (Get-Date -Format 'yyyy-MM-dd'),

    [string]$SourceRevision = 'N/A_NO_GIT_METADATA'
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot

function Resolve-InputDirectory {
    param(
        [string]$Path,
        [string]$Label
    )
    $candidate = if ([IO.Path]::IsPathRooted($Path)) {
        $Path
    } else {
        Join-Path $repo $Path
    }
    if (-not (Test-Path -LiteralPath $candidate -PathType Container)) {
        throw "${Label}_NOT_FOUND path=$candidate"
    }
    return (Resolve-Path -LiteralPath $candidate).Path.TrimEnd('\')
}

function Resolve-OutputDirectory {
    param([string]$Path)
    $candidate = if ([IO.Path]::IsPathRooted($Path)) {
        [IO.Path]::GetFullPath($Path)
    } else {
        [IO.Path]::GetFullPath((Join-Path $repo $Path))
    }
    New-Item -ItemType Directory -Path $candidate -Force | Out-Null
    return (Resolve-Path -LiteralPath $candidate).Path.TrimEnd('\')
}

function Copy-RequiredFile {
    param(
        [string]$SourceRoot,
        [string]$RelativeSource,
        [string]$DestinationRoot,
        [string]$RelativeDestination = $RelativeSource
    )
    $source = Join-Path $SourceRoot $RelativeSource
    if (-not (Test-Path -LiteralPath $source -PathType Leaf)) {
        throw "NODE_RELEASE_REQUIRED_FILE_MISSING path=$source"
    }
    $destination = Join-Path $DestinationRoot $RelativeDestination
    $parent = Split-Path -Parent $destination
    if (-not (Test-Path -LiteralPath $parent -PathType Container)) {
        New-Item -ItemType Directory -Path $parent -Force | Out-Null
    }
    if (Test-Path -LiteralPath $destination) {
        throw "NODE_RELEASE_DESTINATION_COLLISION path=$destination"
    }
    Copy-Item -LiteralPath $source -Destination $destination
}

function Get-ManifestFiles {
    param([string]$Root)
    return @(
        Get-ChildItem -LiteralPath $Root -Recurse -File |
            Where-Object Name -CNE 'release-manifest.json' |
            Sort-Object FullName |
            ForEach-Object {
                [ordered]@{
                    path = [IO.Path]::GetRelativePath($Root, $_.FullName).Replace('\', '/')
                    size = $_.Length
                    sha256 = (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
                }
            }
    )
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

function Assert-SanitizedAgentExample {
    param([string]$Path)
    try {
        $config = Get-Content -Raw -LiteralPath $Path | ConvertFrom-Json
        $dsn = [string]$config.pg_dsn
        $uri = [Uri]::new($dsn)
    }
    catch {
        throw "NODE_RELEASE_SENSITIVE_CONFIG invalid_agent_example path=$Path"
    }
    if ($uri.UserInfo -match ':' -or
        $uri.Query -match '(?i)(password|passwd|pwd|token|secret)=') {
        throw "NODE_RELEASE_SENSITIVE_CONFIG password_in_pg_dsn path=$Path"
    }
}

function Assert-SanitizedHelperExample {
    param([string]$Path)
    try {
        $config = Get-Content -Raw -LiteralPath $Path | ConvertFrom-Json
    }
    catch {
        throw "NODE_RELEASE_SENSITIVE_CONFIG invalid_helper_example path=$Path"
    }
    if (@($config.allowed_roots).Count -ne 0) {
        throw "NODE_RELEASE_SENSITIVE_CONFIG helper_allowed_roots_not_empty path=$Path"
    }
}

$stage = Resolve-InputDirectory -Path $StageDir -Label 'NODE_RELEASE_STAGE'
$output = Resolve-OutputDirectory -Path $OutputDir
$baseName = "MySingerServer-compute-win-x64-$ReleaseId"
$zipPath = Join-Path $output "$baseName.zip"
$sidecarPath = "$zipPath.sha256"
foreach ($finalPath in @($zipPath, $sidecarPath)) {
    if (Test-Path -LiteralPath $finalPath) {
        throw "NODE_RELEASE_OUTPUT_EXISTS path=$finalPath"
    }
}

$work = Join-Path $output (
    '.node-release-work-{0}' -f [Guid]::NewGuid().ToString('N'))
$payload = Join-Path $work 'MySingerServer-Compute'
$verifyRoot = Join-Path $work 'verify'
$temporaryZip = Join-Path $work "$baseName.zip"
$temporarySidecar = "$temporaryZip.sha256"
$complete = $false

try {
    New-Item -ItemType Directory -Path $payload | Out-Null
    foreach ($relativeDirectory in @('data\\agent', 'data\\nodetray')) {
        $directory = Join-Path $payload $relativeDirectory
        New-Item -ItemType Directory -Path $directory -Force | Out-Null
        Write-Utf8NoBom -Path (Join-Path $directory '.gitkeep') -Value ''
    }

    $nativeManifestPath = Join-Path $stage 'native-dependencies.json'
    if (-not (Test-Path -LiteralPath $nativeManifestPath -PathType Leaf)) {
        throw "NODE_RELEASE_NATIVE_MANIFEST_MISSING path=$nativeManifestPath"
    }
    $nativeManifest = Get-Content -Raw -LiteralPath $nativeManifestPath |
        ConvertFrom-Json
    if ($nativeManifest.schema_version -ne 1 -or
        @($nativeManifest.files).Count -eq 0) {
        throw 'NODE_RELEASE_NATIVE_MANIFEST_INVALID'
    }

    $nativeNames = [Collections.Generic.HashSet[string]]::new(
        [StringComparer]::OrdinalIgnoreCase)
    foreach ($file in @($nativeManifest.files)) {
        $name = [string]$file.name
        if ([string]::IsNullOrWhiteSpace($name) -or
            $name -cne [IO.Path]::GetFileName($name) -or
            [IO.Path]::GetExtension($name) -ine '.dll' -or
            -not $nativeNames.Add($name)) {
            throw "NODE_RELEASE_NATIVE_NAME_INVALID name=$name"
        }
        $source = Join-Path $stage $name
        if (-not (Test-Path -LiteralPath $source -PathType Leaf)) {
            throw "NODE_RELEASE_REQUIRED_FILE_MISSING path=$source"
        }
        $expectedHash = ([string]$file.sha256).ToLowerInvariant()
        $actualHash = (Get-FileHash -LiteralPath $source -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($expectedHash -notmatch '^[0-9a-f]{64}$' -or
            $expectedHash -cne $actualHash) {
            throw "NODE_RELEASE_NATIVE_HASH_MISMATCH name=$name"
        }
        Copy-RequiredFile -SourceRoot $stage -RelativeSource $name `
            -DestinationRoot $payload
    }
    if (-not $nativeNames.Contains('videocore.dll')) {
        throw 'NODE_RELEASE_VIDEOCORE_NOT_IN_NATIVE_MANIFEST'
    }

    Assert-SanitizedAgentExample -Path (
        Join-Path $stage 'agent.example.json')
    Assert-SanitizedHelperExample -Path (
        Join-Path $stage 'helper.example.json')

    foreach ($name in @(
            'nodetray.exe',
            'agent.exe',
            'worker.exe',
            'helper.exe',
            'Everything.exe',
            'Everything64.dll',
            'licenses\everything-LICENSE.txt',
            'licenses\everything-NOTICE.md',
            'MicrosoftEdgeWebview2Setup.exe',
            'agent.example.json',
            'helper.example.json')) {
        Copy-RequiredFile -SourceRoot $stage -RelativeSource $name `
            -DestinationRoot $payload
    }
    Copy-RequiredFile -SourceRoot $stage `
        -RelativeSource 'native-dependencies.json' `
        -DestinationRoot $payload
    Copy-RequiredFile -SourceRoot (Join-Path $repo 'deploy') `
        -RelativeSource 'README-节点部署.md' `
        -DestinationRoot $payload
    Copy-RequiredFile -SourceRoot (Join-Path $repo 'third_party\ffmpeg') `
        -RelativeSource 'LICENSE.txt' `
        -DestinationRoot $payload `
        -RelativeDestination 'licenses\ffmpeg-LICENSE.txt'
    Copy-RequiredFile -SourceRoot (Join-Path $repo 'third_party\ffmpeg') `
        -RelativeSource 'NOTICE.md' `
        -DestinationRoot $payload `
        -RelativeDestination 'licenses\ffmpeg-NOTICE.md'
    Copy-RequiredFile -SourceRoot (Join-Path $repo 'third_party\webview2') `
        -RelativeSource 'NOTICE.md' `
        -DestinationRoot $payload `
        -RelativeDestination 'licenses\webview2-NOTICE.md'

    $manifest = [ordered]@{
        schema_version = 1
        product = 'mySingerServer'
        release_kind = 'compute-node-portable'
        target = 'windows/amd64'
        build_date = $BuildDate
        source_revision = $SourceRevision
        portable_root = '.'
        helper = [ordered]@{
            included = $true
            default_enabled = $false
            requires_administrator = $true
        }
        native_dependency_manifest = 'native-dependencies.json'
        files = @(Get-ManifestFiles -Root $payload)
    }
    Write-Utf8NoBom -Path (Join-Path $payload 'release-manifest.json') `
        -Value (($manifest | ConvertTo-Json -Depth 8) + [Environment]::NewLine)

    Compress-Archive -LiteralPath $payload -DestinationPath $temporaryZip `
        -CompressionLevel Optimal
    Expand-Archive -LiteralPath $temporaryZip -DestinationPath $verifyRoot
    $verifiedPayload = Join-Path $verifyRoot 'MySingerServer-Compute'
    $topLevel = @(Get-ChildItem -LiteralPath $verifyRoot -Force)
    if ($topLevel.Count -ne 1 -or -not $topLevel[0].PSIsContainer -or
        $topLevel[0].Name -cne 'MySingerServer-Compute') {
        throw 'NODE_RELEASE_ZIP_TOP_LEVEL_INVALID'
    }

    $verifiedManifest = Get-Content -Raw -LiteralPath (
        Join-Path $verifiedPayload 'release-manifest.json') | ConvertFrom-Json
    $verifiedFiles = @(Get-ManifestFiles -Root $verifiedPayload)
    $expectedByPath = @{}
    foreach ($file in @($verifiedManifest.files)) {
        $expectedByPath[[string]$file.path] = $file
    }
    if ($expectedByPath.Count -ne $verifiedFiles.Count) {
        throw 'NODE_RELEASE_ZIP_FILE_COUNT_MISMATCH'
    }
    foreach ($file in $verifiedFiles) {
        if (-not $expectedByPath.ContainsKey([string]$file.path)) {
            throw "NODE_RELEASE_ZIP_UNLISTED_FILE path=$($file.path)"
        }
        $expected = $expectedByPath[[string]$file.path]
        if ([long]$expected.size -ne [long]$file.size -or
            [string]$expected.sha256 -cne [string]$file.sha256) {
            throw "NODE_RELEASE_ZIP_HASH_MISMATCH path=$($file.path)"
        }
    }

    $zipHash = (Get-FileHash -LiteralPath $temporaryZip -Algorithm SHA256).Hash.ToLowerInvariant()
    Write-Utf8NoBom -Path $temporarySidecar `
        -Value "$zipHash  $baseName.zip$([Environment]::NewLine)"
    Move-Item -LiteralPath $temporaryZip -Destination $zipPath
    Move-Item -LiteralPath $temporarySidecar -Destination $sidecarPath
    $complete = $true
    Write-Host "NODE RELEASE PACKAGE PASS zip=$zipPath sha256=$zipHash files=$($verifiedFiles.Count + 1)"
}
catch {
    Write-Error ("NODE RELEASE PACKAGE FAILED work_dir={0}: {1}" -f $work, $_.Exception.Message)
    throw
}
finally {
    if ($complete -and (Test-Path -LiteralPath $work)) {
        Remove-Item -LiteralPath $work -Recurse -Force
    } elseif (-not $complete -and (Test-Path -LiteralPath $work)) {
        Write-Warning "NODE_RELEASE_WORK_DIR_RETAINED path=$work"
    }
}
