param()

$ErrorActionPreference = 'Stop'
$cleaner = Join-Path $PSScriptRoot 'clean-build-cache.ps1'
$testRoot = Join-Path ([IO.Path]::GetTempPath()) `
    ('mysinger-clean-cache-safety-' + [Guid]::NewGuid().ToString('N'))
$knownProcesses = [Collections.Generic.List[Diagnostics.Process]]::new()
$junctions = [Collections.Generic.List[string]]::new()

function New-TestRepository {
    param(
        [Parameter(Mandatory)]
        [string]$Name,
        [string[]]$Ignore = @()
    )

    $root = Join-Path $testRoot $Name
    New-Item -ItemType Directory -Force -Path $root | Out-Null
    git -C $root init --quiet
    if ($LASTEXITCODE -ne 0) { throw "FIXTURE_GIT_INIT_FAILED name=$Name" }
    git -C $root config user.email 'cache-cleaner-test@example.invalid'
    git -C $root config user.name 'Cache Cleaner Test'
    if ($Ignore.Count -gt 0) {
        Set-Content -LiteralPath (Join-Path $root '.gitignore') -Value $Ignore
    }
    return $root
}

function Add-CacheFile {
    param(
        [Parameter(Mandatory)]
        [string]$Repository,
        [string]$RelativePath = '.tmp\cache.bin',
        [string]$Value = 'cache'
    )

    $path = Join-Path $Repository $RelativePath
    New-Item -ItemType Directory -Force -Path (Split-Path $path) | Out-Null
    Set-Content -LiteralPath $path -Value $Value
    return $path
}

function Assert-ThrowsMessage {
    param(
        [Parameter(Mandatory)]
        [scriptblock]$Action,
        [Parameter(Mandatory)]
        [string]$Expected
    )

    try {
        $null = & $Action
    } catch {
        if ($_.Exception.Message -notlike "*$Expected*") {
            throw "WRONG_ERROR expected=$Expected actual=$($_.Exception.Message)"
        }
        return
    }
    throw "EXPECTED_ERROR_NOT_THROWN expected=$Expected"
}

try {
    New-Item -ItemType Directory -Force -Path $testRoot | Out-Null

    $boundaryRepository = New-TestRepository -Name 'boundary'
    $null = . $cleaner -RepositoryRoot $boundaryRepository
    Assert-ThrowsMessage -Expected 'CACHE_TARGET_IS_REPOSITORY_ROOT' -Action {
        Resolve-CacheTarget -Candidate $boundaryRepository
    }
    $outsideCandidate = Join-Path (Split-Path $testRoot -Parent) `
        ('outside-' + [Guid]::NewGuid().ToString('N'))
    Assert-ThrowsMessage -Expected 'CACHE_TARGET_OUTSIDE_REPOSITORY' -Action {
        Resolve-CacheTarget -Candidate $outsideCandidate
    }

    $trackedRepository = New-TestRepository -Name 'tracked' `
        -Ignore @('/.tmp/')
    $trackedFile = Add-CacheFile -Repository $trackedRepository
    git -C $trackedRepository add -f -- '.tmp/cache.bin'
    git -C $trackedRepository commit --quiet -m 'fixture: track cache file'
    Assert-ThrowsMessage -Expected 'CACHE_TARGET_HAS_TRACKED_CONTENT' -Action {
        & $cleaner -RepositoryRoot $trackedRepository -Apply
    }
    if (-not (Test-Path -LiteralPath $trackedFile)) {
        throw 'TRACKED_FILE_REMOVED'
    }

    Set-Content -LiteralPath $trackedFile -Value 'user modification'
    Assert-ThrowsMessage -Expected 'CACHE_TARGET_HAS_TRACKED_CONTENT' -Action {
        & $cleaner -RepositoryRoot $trackedRepository -Apply
    }
    if ((Get-Content -LiteralPath $trackedFile -Raw) -notmatch 'user modification') {
        throw 'MODIFIED_TRACKED_FILE_CHANGED'
    }

    $untrackedRepository = New-TestRepository -Name 'untracked'
    $untrackedFile = Add-CacheFile -Repository $untrackedRepository
    Assert-ThrowsMessage -Expected 'CACHE_TARGET_HAS_USER_CONTENT' -Action {
        & $cleaner -RepositoryRoot $untrackedRepository -Apply
    }
    if (-not (Test-Path -LiteralPath $untrackedFile)) {
        throw 'UNTRACKED_FILE_REMOVED'
    }

    $notRepository = Join-Path $testRoot 'not-a-repository'
    New-Item -ItemType Directory -Force -Path $notRepository | Out-Null
    $noGitFile = Add-CacheFile -Repository $notRepository
    Assert-ThrowsMessage -Expected 'CACHE_GIT_PREFLIGHT_FAILED' -Action {
        & $cleaner -RepositoryRoot $notRepository -Apply
    }
    if (-not (Test-Path -LiteralPath $noGitFile)) {
        throw 'NO_GIT_PREFLIGHT_FILE_REMOVED'
    }

    $cimRepository = New-TestRepository -Name 'cim-failure' `
        -Ignore @('/.tmp/')
    $cimFile = Add-CacheFile -Repository $cimRepository
    function Get-CimInstance { throw 'SYNTHETIC_CIM_FAILURE' }
    try {
        Assert-ThrowsMessage -Expected 'SYNTHETIC_CIM_FAILURE' -Action {
            & $cleaner -RepositoryRoot $cimRepository -Apply
        }
    } finally {
        Remove-Item -LiteralPath Function:\Get-CimInstance
    }
    if (-not (Test-Path -LiteralPath $cimFile)) {
        throw 'CIM_FAILURE_FILE_REMOVED'
    }

    $inUseRepository = New-TestRepository -Name 'in-use' `
        -Ignore @('/.tmp/')
    $inUseFile = Add-CacheFile -Repository $inUseRepository
    $inUseTarget = Join-Path $inUseRepository '.tmp'
    $holdScript = Join-Path $inUseRepository 'hold-cache.ps1'
    Set-Content -LiteralPath $holdScript -Value @(
        'param([string]$Marker)'
        'Start-Sleep -Seconds 30'
    )
    $pwsh = (Get-Process -Id $PID).Path
    $holdProcess = Start-Process -FilePath $pwsh -WindowStyle Hidden `
        -ArgumentList @('-NoProfile', '-File', $holdScript, $inUseTarget) `
        -PassThru
    $knownProcesses.Add($holdProcess)
    $foundMarker = $false
    foreach ($attempt in 1..50) {
        $foundMarker = @(
            CimCmdlets\Get-CimInstance -ClassName Win32_Process `
                -Filter "ProcessId = $($holdProcess.Id)" |
                Where-Object {
                    $_.CommandLine -and $_.CommandLine.Contains($inUseTarget)
                }
        ).Count -gt 0
        if ($foundMarker) { break }
        Start-Sleep -Milliseconds 100
    }
    if (-not $foundMarker) { throw 'IN_USE_PROCESS_MARKER_NOT_VISIBLE' }
    Assert-ThrowsMessage -Expected 'CACHE_TARGET_IN_USE' -Action {
        & $cleaner -RepositoryRoot $inUseRepository -Apply
    }
    if (-not (Test-Path -LiteralPath $inUseFile)) {
        throw 'IN_USE_FILE_REMOVED'
    }

    $reparseRepository = New-TestRepository -Name 'reparse' `
        -Ignore @('/.tmp-*/')
    $outsideDirectory = Join-Path $testRoot 'outside-reparse-target'
    New-Item -ItemType Directory -Force -Path $outsideDirectory | Out-Null
    Set-Content -LiteralPath (Join-Path $outsideDirectory 'user.txt') `
        -Value 'outside user content'
    $junction = Join-Path $reparseRepository '.tmp-link'
    New-Item -ItemType Junction -Path $junction -Target $outsideDirectory |
        Out-Null
    $junctions.Add($junction)
    Assert-ThrowsMessage -Expected 'CACHE_TARGET_REPARSE_POINT' -Action {
        & $cleaner -RepositoryRoot $reparseRepository
    }
    if (-not (Test-Path -LiteralPath (Join-Path $outsideDirectory 'user.txt'))) {
        throw 'REPARSE_TARGET_CONTENT_REMOVED'
    }
} finally {
    foreach ($process in $knownProcesses) {
        if (-not $process.HasExited) {
            Stop-Process -Id $process.Id -Force
            $process.WaitForExit()
        }
    }
    foreach ($junction in $junctions) {
        if (Test-Path -LiteralPath $junction) {
            Remove-Item -LiteralPath $junction -Force
        }
    }
    if (Test-Path -LiteralPath $testRoot) {
        Remove-Item -LiteralPath $testRoot -Recurse -Force
    }
}

Write-Output 'BUILD CACHE CLEANER SAFETY TEST PASS'
