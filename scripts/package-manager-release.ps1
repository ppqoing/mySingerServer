[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string]$StageDir,

    [string]$OutputDir = 'artifacts\releases',

    [ValidatePattern('^[A-Za-z0-9][A-Za-z0-9._-]*$')]
    [string]$ReleaseId = (Get-Date -Format 'yyyyMMdd'),

    [ValidatePattern('^\d{4}-\d{2}-\d{2}$')]
    [string]$BuildDate = (Get-Date -Format 'yyyy-MM-dd'),

    [string]$SourceRevision = 'N/A_NO_GIT_METADATA',

    [string]$GuiConfigPath = ''
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$repo = Split-Path -Parent $PSScriptRoot

function Resolve-InputDirectory {
    param([string]$Path, [string]$Label)
    $candidate = if ([IO.Path]::IsPathRooted($Path)) { $Path } else { Join-Path $repo $Path }
    if (-not (Test-Path -LiteralPath $candidate -PathType Container)) {
        throw "${Label}_NOT_FOUND path=$candidate"
    }
    (Resolve-Path -LiteralPath $candidate).Path.TrimEnd('\')
}

function Resolve-InputFile {
    param([string]$Path, [string]$Label)
    $candidate = if ([IO.Path]::IsPathRooted($Path)) { $Path } else { Join-Path $repo $Path }
    if (-not (Test-Path -LiteralPath $candidate -PathType Leaf)) {
        throw "${Label}_NOT_FOUND path=$candidate"
    }
    (Resolve-Path -LiteralPath $candidate).Path
}

function Resolve-OutputDirectory {
    param([string]$Path)
    $candidate = if ([IO.Path]::IsPathRooted($Path)) { [IO.Path]::GetFullPath($Path) } else { [IO.Path]::GetFullPath((Join-Path $repo $Path)) }
    New-Item -ItemType Directory -Path $candidate -Force | Out-Null
    (Resolve-Path -LiteralPath $candidate).Path.TrimEnd('\')
}

function Write-Utf8NoBom {
    param([string]$Path, [string]$Value)
    [IO.File]::WriteAllText($Path, $Value, [Text.UTF8Encoding]::new($false))
}

function Copy-RequiredFile {
    param([string]$Source, [string]$DestinationRoot, [string]$DestinationName)
    if (-not (Test-Path -LiteralPath $Source -PathType Leaf)) {
        throw "MANAGER_RELEASE_REQUIRED_FILE_MISSING path=$Source"
    }
    $destination = Join-Path $DestinationRoot $DestinationName
    if (Test-Path -LiteralPath $destination) {
        throw "MANAGER_RELEASE_DESTINATION_COLLISION path=$destination"
    }
    Copy-Item -LiteralPath $Source -Destination $destination
}

function Get-ManifestFiles {
    param([string]$Root)
    @(Get-ChildItem -LiteralPath $Root -Recurse -File | Where-Object Name -CNE 'release-manifest.json' |
        Sort-Object FullName | ForEach-Object {
            [ordered]@{
                path = [IO.Path]::GetRelativePath($Root, $_.FullName).Replace('\', '/')
                size = $_.Length
                sha256 = (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
            }
        })
}

function Assert-SanitizedGuiConfig {
    param([string]$Path)
    try {
        $config = Get-Content -Raw -LiteralPath $Path | ConvertFrom-Json
        $uri = if ([string]::IsNullOrWhiteSpace([string]$config.pg_dsn)) { $null } else { [Uri]::new([string]$config.pg_dsn) }
    } catch {
        throw "MANAGER_RELEASE_SENSITIVE_CONFIG invalid_gui_config path=$Path"
    }
    if ($null -ne $uri -and (($uri.Scheme -cne 'postgres' -and $uri.Scheme -cne 'postgresql') -or
        ($uri.Host -cne '127.0.0.1' -and $uri.Host -cne 'localhost'))) {
        throw "MANAGER_RELEASE_SENSITIVE_CONFIG unsafe_pg_dsn_endpoint path=$Path"
    }
    if ($null -ne $uri -and $uri.UserInfo -match ':') {
        throw "MANAGER_RELEASE_SENSITIVE_CONFIG credential_in_pg_dsn path=$Path"
    }
    $sensitiveQueryKeys = [Collections.Generic.HashSet[string]]::new(
        [StringComparer]::OrdinalIgnoreCase)
    foreach ($key in @('password', 'passwd', 'pwd', 'token', 'access_token', 'api_key', 'key', 'secret')) {
        [void]$sensitiveQueryKeys.Add($key)
    }
    try {
        $encodedQuery = if ($null -eq $uri) { '' } else { $uri.GetComponents([UriComponents]::Query, [UriFormat]::UriEscaped) }
        foreach ($segment in $encodedQuery -split '&') {
            if ([string]::IsNullOrEmpty($segment)) { continue }
            $encodedName = ($segment -split '=', 2)[0]
            $name = [Uri]::UnescapeDataString($encodedName)
            if ($sensitiveQueryKeys.Contains($name)) {
                throw 'sensitive_query_key'
            }
        }
    } catch {
        throw "MANAGER_RELEASE_SENSITIVE_CONFIG invalid_or_sensitive_pg_dsn_query path=$Path"
    }
    $agents = @($config.agents)
    if ($agents.Count -ne 1 -or [string]$agents[0].addr -cne '127.0.0.1:9101') {
        throw "MANAGER_RELEASE_SENSITIVE_CONFIG agent_example_not_loopback_only path=$Path"
    }
}

$stage = Resolve-InputDirectory -Path $StageDir -Label 'MANAGER_RELEASE_STAGE'
$output = Resolve-OutputDirectory -Path $OutputDir
$guiConfig = Resolve-InputFile -Path $(if ([string]::IsNullOrWhiteSpace($GuiConfigPath)) { Join-Path $repo 'deploy\gui.default.json' } else { $GuiConfigPath }) -Label 'MANAGER_RELEASE_GUI_CONFIG'
$startScript = Resolve-InputFile -Path (Join-Path $repo 'deploy\Start-Manager.ps1') -Label 'MANAGER_RELEASE_START_SCRIPT'
$readme = Resolve-InputFile -Path (Join-Path $repo 'deploy\README-管理端部署.md') -Label 'MANAGER_RELEASE_README'
Assert-SanitizedGuiConfig -Path $guiConfig

$baseName = "MySingerServer-manager-win-x64-$ReleaseId"
$zipPath = Join-Path $output "$baseName.zip"
$sidecarPath = "$zipPath.sha256"
foreach ($finalPath in @($zipPath, $sidecarPath)) {
    if (Test-Path -LiteralPath $finalPath) { throw "MANAGER_RELEASE_OUTPUT_EXISTS path=$finalPath" }
}

$work = Join-Path $output ('.manager-release-work-{0}' -f [Guid]::NewGuid().ToString('N'))
$payload = Join-Path $work 'MySingerServer-Manager'
$verifyRoot = Join-Path $work 'verify'
$temporaryZip = Join-Path $work "$baseName.zip"
$temporarySidecar = "$temporaryZip.sha256"
$complete = $false
try {
    New-Item -ItemType Directory -Path $payload | Out-Null
    Copy-RequiredFile -Source (Join-Path $stage 'gui.exe') -DestinationRoot $payload -DestinationName 'gui.exe'
    Copy-RequiredFile -Source $guiConfig -DestinationRoot $payload -DestinationName 'gui.json'
    Copy-RequiredFile -Source $startScript -DestinationRoot $payload -DestinationName 'Start-Manager.ps1'
    Copy-RequiredFile -Source $readme -DestinationRoot $payload -DestinationName 'README-管理端部署.md'

    $manifest = [ordered]@{
        schema_version = 1
        product = 'mySingerServer'
        release_kind = 'remote-manager-portable'
        target = 'windows/amd64'
        build_date = $BuildDate
        source_revision = $SourceRevision
        portable_root = '.'
        compatibility = [ordered]@{
            media_metadata_schema_version = 5
        }
        files = @(Get-ManifestFiles -Root $payload)
    }
    Write-Utf8NoBom -Path (Join-Path $payload 'release-manifest.json') -Value (($manifest | ConvertTo-Json -Depth 8) + [Environment]::NewLine)
    Compress-Archive -LiteralPath $payload -DestinationPath $temporaryZip -CompressionLevel Optimal
    Expand-Archive -LiteralPath $temporaryZip -DestinationPath $verifyRoot
    $topLevel = @(Get-ChildItem -LiteralPath $verifyRoot -Force)
    $verifiedPayload = Join-Path $verifyRoot 'MySingerServer-Manager'
    if ($topLevel.Count -ne 1 -or -not $topLevel[0].PSIsContainer -or $topLevel[0].Name -cne 'MySingerServer-Manager') { throw 'MANAGER_RELEASE_ZIP_TOP_LEVEL_INVALID' }
    $verifiedManifest = Get-Content -Raw -LiteralPath (Join-Path $verifiedPayload 'release-manifest.json') | ConvertFrom-Json
    $verifiedFiles = @(Get-ManifestFiles -Root $verifiedPayload)
    $expectedByPath = @{}
    foreach ($file in @($verifiedManifest.files)) { $expectedByPath[[string]$file.path] = $file }
    if ($expectedByPath.Count -ne $verifiedFiles.Count) { throw 'MANAGER_RELEASE_ZIP_FILE_COUNT_MISMATCH' }
    foreach ($file in $verifiedFiles) {
        if (-not $expectedByPath.ContainsKey([string]$file.path)) { throw "MANAGER_RELEASE_ZIP_UNLISTED_FILE path=$($file.path)" }
        $expected = $expectedByPath[[string]$file.path]
        if ([long]$expected.size -ne [long]$file.size -or [string]$expected.sha256 -cne [string]$file.sha256) { throw "MANAGER_RELEASE_ZIP_HASH_MISMATCH path=$($file.path)" }
    }
    $zipHash = (Get-FileHash -LiteralPath $temporaryZip -Algorithm SHA256).Hash.ToLowerInvariant()
    Write-Utf8NoBom -Path $temporarySidecar -Value "$zipHash  $baseName.zip$([Environment]::NewLine)"
    Move-Item -LiteralPath $temporaryZip -Destination $zipPath
    Move-Item -LiteralPath $temporarySidecar -Destination $sidecarPath
    $complete = $true
    Write-Host "MANAGER RELEASE PACKAGE PASS zip=$zipPath sha256=$zipHash files=$($verifiedFiles.Count + 1)"
} catch {
    Write-Error ("MANAGER RELEASE PACKAGE FAILED work_dir={0}: {1}" -f $work, $_.Exception.Message)
    throw
} finally {
    if ($complete -and (Test-Path -LiteralPath $work)) { Remove-Item -LiteralPath $work -Recurse -Force }
    elseif (-not $complete -and (Test-Path -LiteralPath $work)) { Write-Warning "MANAGER_RELEASE_WORK_DIR_RETAINED path=$work" }
}
