[CmdletBinding()]
param(
    [string]$RepositoryRoot = (Split-Path -Parent $PSScriptRoot),
    [switch]$Apply
)

$ErrorActionPreference = 'Stop'

$repo = [IO.Path]::GetFullPath($RepositoryRoot).TrimEnd('\')
if (-not (Test-Path -LiteralPath $repo -PathType Container)) {
    throw "CACHE_REPOSITORY_NOT_FOUND path=$repo"
}
$repoPrefix = $repo.TrimEnd('\') + '\'

function Resolve-CacheTarget {
    param(
        [Parameter(Mandatory)]
        [string]$Candidate
    )

    $target = [IO.Path]::GetFullPath($Candidate).TrimEnd('\')
    if ($target -eq $repo) {
        throw 'CACHE_TARGET_IS_REPOSITORY_ROOT'
    }
    if (-not $target.StartsWith(
            $repoPrefix,
            [StringComparison]::OrdinalIgnoreCase)) {
        throw "CACHE_TARGET_OUTSIDE_REPOSITORY path=$target"
    }
    return $target
}

function Get-CacheTargetInfo {
    param(
        [Parameter(Mandatory)]
        [string]$Target
    )

    $targetItem = Get-Item -LiteralPath $Target -Force
    if ($targetItem.PSIsContainer) {
        $files = @(Get-ChildItem -LiteralPath $Target -Force -File -Recurse)
        $bytes = [long](($files | Measure-Object -Property Length -Sum).Sum)
    } else {
        $files = @($targetItem)
        $bytes = [long]$targetItem.Length
    }
    return [pscustomobject]@{
        Path = $Target
        IsDirectory = [bool]$targetItem.PSIsContainer
        FileCount = $files.Count
        LogicalBytes = $bytes
    }
}

function Invoke-CacheGit {
    param(
        [Parameter(Mandatory)]
        [string[]]$Arguments,
        [Parameter(Mandatory)]
        [string]$Operation
    )

    try {
        $output = @(& git -C $repo @Arguments 2>&1)
        $exitCode = $LASTEXITCODE
    } catch {
        throw (
            'CACHE_GIT_PREFLIGHT_FAILED operation={0} detail={1}' -f
            $Operation, $_.Exception.Message
        )
    }
    if ($exitCode -ne 0) {
        throw (
            'CACHE_GIT_PREFLIGHT_FAILED operation={0} exit={1} detail={2}' -f
            $Operation, $exitCode, ($output -join ' ')
        )
    }
    return $output
}

function Assert-CacheRepositoryGitRoot {
    $topLevelOutput = @(
        Invoke-CacheGit -Operation 'resolve-root' `
            -Arguments @('rev-parse', '--show-toplevel')
    )
    if ($topLevelOutput.Count -ne 1) {
        throw 'CACHE_GIT_PREFLIGHT_FAILED operation=resolve-root output-count'
    }
    $gitRoot = [IO.Path]::GetFullPath($topLevelOutput[0]).TrimEnd('\')
    if (-not $gitRoot.Equals($repo, [StringComparison]::OrdinalIgnoreCase)) {
        throw (
            'CACHE_GIT_PREFLIGHT_FAILED operation=resolve-root ' +
            "expected=$repo actual=$gitRoot"
        )
    }
}

function Assert-CacheTargetGitSafe {
    param(
        [Parameter(Mandatory)]
        [string]$Target
    )

    $relativeTarget = [IO.Path]::GetRelativePath($repo, $Target)
    $relativeTarget = $relativeTarget.Replace('\', '/')
    $tracked = @(
        Invoke-CacheGit -Operation 'tracked-content' -Arguments @(
            '--literal-pathspecs', 'ls-files', '--', $relativeTarget
        )
    )
    if ($tracked.Count -gt 0) {
        throw "CACHE_TARGET_HAS_TRACKED_CONTENT path=$Target"
    }

    $userContent = @(
        Invoke-CacheGit -Operation 'user-content' -Arguments @(
            '--literal-pathspecs', 'status', '--porcelain=v1',
            '--untracked-files=all', '--', $relativeTarget
        )
    )
    if ($userContent.Count -gt 0) {
        throw "CACHE_TARGET_HAS_USER_CONTENT path=$Target"
    }
}

function Assert-CacheTargetHasNoReparsePoint {
    param(
        [Parameter(Mandatory)]
        [string]$Target
    )

    $targetItem = Get-Item -LiteralPath $Target -Force
    if ($targetItem.Attributes -band [IO.FileAttributes]::ReparsePoint) {
        throw "CACHE_TARGET_REPARSE_POINT path=$Target"
    }
    if ($targetItem.PSIsContainer) {
        $nestedReparsePoint = Get-ChildItem -LiteralPath $Target -Force `
            -Recurse -Attributes ReparsePoint | Select-Object -First 1
        if ($null -ne $nestedReparsePoint) {
            throw "CACHE_TARGET_REPARSE_POINT path=$($nestedReparsePoint.FullName)"
        }
    }
}

$fixedRelativeTargets = @(
    '.tmp',
    '.superpowers\tmp',
    '.superpowers\runtime',
    'artifacts\.gocache-live-status',
    'artifacts\.gocache-live-workerpool',
    'videocore\build',
    'mediacore\build'
)
$codexTempNames = @(
    'gocache-runtime-logs', 'gocache', 'go-cache',
    'everything-autostart', 'scan-progress-gocache',
    'gocache-cgo', 'gocache-cgo-final',
    'scan-progress-webui', 'testtmp'
)
$codexTempPrefixes = @(
    'stage-', 'central-cache-control-', 'scan-stop-control-',
    'scan-progress-agent-hotfix', 'everything-download-'
)

$candidates = [Collections.Generic.List[string]]::new()
foreach ($relativeTarget in $fixedRelativeTargets) {
    $candidates.Add((Join-Path $repo $relativeTarget))
}

Get-ChildItem -LiteralPath $repo -Force -Directory |
    Where-Object { $_.Name.StartsWith('.tmp-', [StringComparison]::Ordinal) } |
    ForEach-Object { $candidates.Add($_.FullName) }

$codexTemp = Join-Path $repo '.codex-temp'
Get-ChildItem -LiteralPath $codexTemp -Force -Directory `
    -ErrorAction SilentlyContinue |
    Where-Object {
        $candidateName = $_.Name
        $codexTempNames -contains $candidateName -or
        @($codexTempPrefixes | Where-Object {
            $candidateName.StartsWith(
                $_,
                [StringComparison]::OrdinalIgnoreCase)
        }).Count -gt 0
    } |
    ForEach-Object { $candidates.Add($_.FullName) }

Get-ChildItem -LiteralPath $codexTemp -Force -File `
    -ErrorAction SilentlyContinue |
    Where-Object { $_.Extension -ieq '.log' } |
    ForEach-Object { $candidates.Add($_.FullName) }

$seen = [Collections.Generic.HashSet[string]]::new(
    [StringComparer]::OrdinalIgnoreCase
)
$targets = @(
    foreach ($candidate in $candidates) {
        $target = Resolve-CacheTarget -Candidate $candidate
        if ($seen.Add($target) -and (Test-Path -LiteralPath $target)) {
            Assert-CacheTargetHasNoReparsePoint -Target $target
            Get-CacheTargetInfo -Target $target
        }
    }
)

$mode = if ($Apply) { 'APPLY' } else { 'DRY-RUN' }
foreach ($target in $targets) {
    Write-Output (
        '{0} target={1} files={2} logical-bytes={3}' -f
        $mode, $target.Path, $target.FileCount, $target.LogicalBytes
    )
}

$totalFiles = [long](($targets | Measure-Object -Property FileCount -Sum).Sum)
$totalBytes = [long](($targets | Measure-Object -Property LogicalBytes -Sum).Sum)
if (-not $Apply) {
    Write-Output (
        'DRY-RUN TOTAL targets={0} files={1} logical-bytes={2}' -f
        $targets.Count, $totalFiles, $totalBytes
    )
    return
}

Assert-CacheRepositoryGitRoot
foreach ($target in $targets) {
    Assert-CacheTargetGitSafe -Target $target.Path
}

$processes = @(Get-CimInstance -ClassName Win32_Process `
    -Property ProcessId, CommandLine -ErrorAction Stop)
foreach ($target in $targets) {
    foreach ($process in $processes) {
        if ($process.CommandLine -and
            $process.CommandLine.IndexOf(
                $target.Path,
                [StringComparison]::OrdinalIgnoreCase) -ge 0) {
            throw (
                'CACHE_TARGET_IN_USE path={0} processId={1}' -f
                $target.Path, $process.ProcessId
            )
        }
    }
}

foreach ($target in $targets) {
    if ($target.IsDirectory) {
        Remove-Item -LiteralPath $target.Path -Recurse -Force
    } else {
        Remove-Item -LiteralPath $target.Path -Force
    }
}

Write-Output (
    'APPLY COMPLETE targets={0} released-logical-bytes={1}' -f
    $targets.Count, $totalBytes
)
