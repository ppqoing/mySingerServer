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

    [scriptblock]$TestPublishHook = $null,

    [scriptblock]$TestRollbackHook = $null
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$repo = Split-Path -Parent $PSScriptRoot
$nodePackageScript = Join-Path $PSScriptRoot 'package-node-release.ps1'
$managerPackageScript = Join-Path $PSScriptRoot 'package-manager-release.ps1'

function Resolve-InputDirectory {
    param([string]$Path)
    $candidate = if ([IO.Path]::IsPathRooted($Path)) { $Path } else { Join-Path $repo $Path }
    if (-not (Test-Path -LiteralPath $candidate -PathType Container)) {
        throw "PORTABLE_RELEASE_STAGE_NOT_FOUND path=$candidate"
    }
    (Resolve-Path -LiteralPath $candidate).Path.TrimEnd('\')
}

function Resolve-OutputDirectory {
    param([string]$Path)
    $candidate = if ([IO.Path]::IsPathRooted($Path)) { [IO.Path]::GetFullPath($Path) } else { [IO.Path]::GetFullPath((Join-Path $repo $Path)) }
    New-Item -ItemType Directory -Path $candidate -Force | Out-Null
    (Resolve-Path -LiteralPath $candidate).Path.TrimEnd('\')
}

function Get-Sha256 {
    param([string]$Path)
    (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Assert-FinalPathsAvailable {
    param([string[]]$Paths)
    foreach ($path in $Paths) {
        if (Test-Path -LiteralPath $path) {
            throw "PORTABLE_RELEASE_OUTPUT_EXISTS path=$path"
        }
    }
}

function Invoke-TestPublishHook {
    param([object]$Context)
    if ($null -ne $TestPublishHook) { & $TestPublishHook $Context }
}

function Invoke-TestRollbackHook {
    param([object]$Context)
    if ($null -ne $TestRollbackHook) { & $TestRollbackHook $Context }
}

function Assert-CandidatePackage {
    param(
        [string]$ZipPath,
        [string]$SidecarPath,
        [string]$ExpectedReleaseKind,
        [string]$VerificationRoot
    )
    if (-not (Test-Path -LiteralPath $ZipPath -PathType Leaf) -or
        -not (Test-Path -LiteralPath $SidecarPath -PathType Leaf)) {
        throw "PORTABLE_RELEASE_CANDIDATE_MISSING zip=$ZipPath"
    }
    $zipName = [IO.Path]::GetFileName($ZipPath)
    $zipHash = Get-Sha256 -Path $ZipPath
    if ((Get-Content -Raw -LiteralPath $SidecarPath).Trim() -cne "$zipHash  $zipName") {
        throw "PORTABLE_RELEASE_CANDIDATE_SIDECAR_INVALID zip=$ZipPath"
    }
    Expand-Archive -LiteralPath $ZipPath -DestinationPath $VerificationRoot
    $roots = @(Get-ChildItem -LiteralPath $VerificationRoot -Force)
    if ($roots.Count -ne 1 -or -not $roots[0].PSIsContainer) {
        throw "PORTABLE_RELEASE_CANDIDATE_ZIP_INVALID zip=$ZipPath"
    }
    $manifestPath = Join-Path $roots[0].FullName 'release-manifest.json'
    if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) {
        throw "PORTABLE_RELEASE_CANDIDATE_MANIFEST_MISSING zip=$ZipPath"
    }
    $manifest = Get-Content -Raw -LiteralPath $manifestPath | ConvertFrom-Json
    if ([string]$manifest.product -cne 'mySingerServer' -or
        [string]$manifest.release_kind -cne $ExpectedReleaseKind) {
        throw "PORTABLE_RELEASE_CANDIDATE_MANIFEST_INVALID zip=$ZipPath"
    }
}

$stage = Resolve-InputDirectory -Path $StageDir
$output = Resolve-OutputDirectory -Path $OutputDir
$artifacts = @(
    [ordered]@{ Name = "MySingerServer-compute-win-x64-$ReleaseId.zip"; Kind = 'compute-node-portable' },
    [ordered]@{ Name = "MySingerServer-manager-win-x64-$ReleaseId.zip"; Kind = 'remote-manager-portable' }
)
$finalPaths = @(
    foreach ($artifact in $artifacts) {
        Join-Path $output $artifact.Name
        Join-Path $output ($artifact.Name + '.sha256')
    }
)
Assert-FinalPathsAvailable -Paths $finalPaths

$candidateRoot = Join-Path $repo ('.tmp\.portable-release-work-{0}' -f [Guid]::NewGuid().ToString('N'))
$verifyRoot = Join-Path $candidateRoot 'verify'
$published = @()
$publishStarted = $false
$moveIndex = 0

try {
    New-Item -ItemType Directory -Path $candidateRoot -Force | Out-Null
    & $nodePackageScript -StageDir $stage -OutputDir $candidateRoot -ReleaseId $ReleaseId -BuildDate $BuildDate -SourceRevision $SourceRevision
    & $managerPackageScript -StageDir $stage -OutputDir $candidateRoot -ReleaseId $ReleaseId -BuildDate $BuildDate -SourceRevision $SourceRevision

    foreach ($artifact in $artifacts) {
        $zip = Join-Path $candidateRoot $artifact.Name
        $sidecar = "$zip.sha256"
        Assert-CandidatePackage -ZipPath $zip -SidecarPath $sidecar -ExpectedReleaseKind $artifact.Kind -VerificationRoot (Join-Path $verifyRoot $artifact.Kind)
    }

    Invoke-TestPublishHook -Context ([pscustomobject]@{
            Phase = 'BeforeSecondPreflight'
            FinalPaths = $finalPaths
        })
    Assert-FinalPathsAvailable -Paths $finalPaths
    $publishStarted = $true
    foreach ($artifact in $artifacts) {
        $candidatePaths = @()
        $candidatePaths += Join-Path $candidateRoot $artifact.Name
        $candidatePaths += Join-Path $candidateRoot ($artifact.Name + '.sha256')
        foreach ($candidate in $candidatePaths) {
            $moveIndex++
            $destination = Join-Path $output ([IO.Path]::GetFileName($candidate))
            $hash = Get-Sha256 -Path $candidate
            Invoke-TestPublishHook -Context ([pscustomobject]@{
                    Phase = 'BeforeMove'
                    MoveIndex = $moveIndex
                    Candidate = $candidate
                    Destination = $destination
                })
            [IO.File]::Move($candidate, $destination, $false)
            $published += [ordered]@{ Path = $destination; Hash = $hash }
            Invoke-TestPublishHook -Context ([pscustomobject]@{
                    Phase = 'AfterMove'
                    MoveIndex = $moveIndex
                    Candidate = $candidate
                    Destination = $destination
                })
        }
    }
    Write-Host "PORTABLE RELEASE PACKAGE PASS output=$output release_id=$ReleaseId"
}
catch {
    $failure = $_
    if ($publishStarted) {
        $cleanupWarnings = @()
        foreach ($artifact in @($published)) {
            try {
                $exists = Test-Path -LiteralPath $artifact.Path -PathType Leaf
            }
            catch {
                $cleanupWarnings += "exists path=$($artifact.Path) error=$($_.Exception.Message)"
                continue
            }
            if (-not $exists) { continue }
            try {
                $actualHash = Get-Sha256 -Path $artifact.Path
            }
            catch {
                $cleanupWarnings += "hash path=$($artifact.Path) error=$($_.Exception.Message)"
                continue
            }
            if ($actualHash -cne $artifact.Hash) { continue }
            try {
                Invoke-TestRollbackHook -Context ([pscustomobject]@{
                        Path = $artifact.Path
                        ExpectedHash = $artifact.Hash
                    })
                Remove-Item -LiteralPath $artifact.Path -Force
            }
            catch {
                $cleanupWarnings += "remove path=$($artifact.Path) error=$($_.Exception.Message)"
            }
        }
        $cleanupSuffix = if ($cleanupWarnings.Count -eq 0) { '' } else { " cleanup_warnings=$($cleanupWarnings -join ' | ')" }
        throw "PORTABLE_RELEASE_PUBLISH_FAILED: $($failure.Exception.Message)$cleanupSuffix"
    }
    throw
}
finally {
    try {
        if (Test-Path -LiteralPath $candidateRoot) {
            Invoke-TestPublishHook -Context ([pscustomobject]@{
                    Phase = 'BeforeCandidateCleanup'
                    CandidateRoot = $candidateRoot
                })
            Remove-Item -LiteralPath $candidateRoot -Recurse -Force
        }
    }
    catch {
        Write-Warning 'PORTABLE_RELEASE_CANDIDATE_CLEANUP_WARNING'
    }
}
