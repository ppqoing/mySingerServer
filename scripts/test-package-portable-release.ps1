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

    $computeExtract = Join-Path $testRoot 'compute-extract'
    Expand-Archive -LiteralPath (Join-Path $output $expected[0]) -DestinationPath $computeExtract
    $computeRoot = Join-Path $computeExtract 'MySingerServer-Compute'
    $computeFiles = @(Get-ChildItem -LiteralPath $computeRoot -Recurse -File | ForEach-Object {
        [IO.Path]::GetRelativePath($computeRoot, $_.FullName).Replace('\', '/')
    })
    Assert-True ($computeFiles -contains 'Start-Compute.ps1') 'Compute start script missing'
    foreach ($forbidden in @('gui.exe','agent.json','data/agent/agent.db','data/agent/local-control.token')) {
        Assert-True (-not ($computeFiles -contains $forbidden)) "Compute package leaked runtime file: $forbidden"
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

    $lateCollisionOutput = Join-Path $testRoot 'late-collision-release'
    $lateCollisionTarget = Join-Path $lateCollisionOutput 'MySingerServer-compute-win-x64-late-collision.zip'
    $lateCollisionRejected = $false
    try {
        & $packageScript -StageDir $stage -OutputDir $lateCollisionOutput -ReleaseId 'late-collision' -BuildDate '2026-08-11' -TestPublishHook {
            param($context)
            if ($context.Phase -ceq 'BeforeSecondPreflight') {
                New-Item -ItemType Directory -Path $context.FinalPaths[0] -Force | Out-Null
            }
        }
    } catch {
        $lateCollisionRejected = $_.Exception.Message -match 'PORTABLE_RELEASE_OUTPUT_EXISTS'
    }
    Assert-True $lateCollisionRejected 'post-candidate directory collision was accepted'
    Assert-True (Test-Path -LiteralPath $lateCollisionTarget -PathType Container) 'post-candidate collision directory was changed'
    Assert-True (@(Get-ChildItem -LiteralPath $lateCollisionOutput -Force).Count -eq 1) 'post-candidate collision published another artifact'

    $raceCollisionOutput = Join-Path $testRoot 'race-collision-release'
    $raceCollisionTarget = Join-Path $raceCollisionOutput 'MySingerServer-compute-win-x64-race-collision.zip'
    $raceCollisionRejected = $false
    try {
        & $packageScript -StageDir $stage -OutputDir $raceCollisionOutput -ReleaseId 'race-collision' -BuildDate '2026-08-11' -TestPublishHook {
            param($context)
            if ($context.Phase -ceq 'BeforeMove' -and $context.MoveIndex -eq 1) {
                New-Item -ItemType Directory -Path $context.Destination -Force | Out-Null
            }
        }
    } catch {
        $raceCollisionRejected = $_.Exception.Message -match 'PORTABLE_RELEASE_PUBLISH_FAILED'
    }
    Assert-True $raceCollisionRejected 'atomic move race collision did not return the stable publish error'
    Assert-True (Test-Path -LiteralPath $raceCollisionTarget -PathType Container) 'atomic move race collision directory was changed'
    Assert-True (@(Get-ChildItem -LiteralPath $raceCollisionOutput -File -ErrorAction SilentlyContinue).Count -eq 0) 'atomic move race collision published a file'

    $rollbackOutput = Join-Path $testRoot 'rollback-release'
    $rollbackState = [pscustomobject]@{ FirstMovedPath = $null }
    $rollbackRejected = $false
    $rollbackError = ''
    try {
        & $packageScript -StageDir $stage -OutputDir $rollbackOutput -ReleaseId 'rollback' -BuildDate '2026-08-11' -TestPublishHook {
            param($context)
            if ($context.Phase -ceq 'AfterMove' -and $context.MoveIndex -eq 1) {
                $rollbackState.FirstMovedPath = $context.Destination
                Write-Utf8NoBom -Path $context.Destination -Value 'user-replaced-after-publish'
            }
            if ($context.Phase -ceq 'AfterMove' -and $context.MoveIndex -eq 3) {
                throw 'INJECTED_PUBLISH_FAILURE'
            }
        } -TestRollbackHook {
            param($context)
            if ($context.Path -like '*.zip.sha256') { throw 'INJECTED_CLEANUP_FAILURE' }
        }
    } catch {
        $rollbackError = $_.Exception.Message
        $rollbackRejected = $_.Exception.Message -match 'PORTABLE_RELEASE_PUBLISH_FAILED.*INJECTED_PUBLISH_FAILURE'
    }
    Assert-True $rollbackRejected 'partial publish failure did not keep the stable original error code'
    Assert-True ($rollbackError -match 'cleanup_warnings=.*INJECTED_CLEANUP_FAILURE') 'cleanup warning did not preserve the original publish failure'
    Assert-True (Test-Path -LiteralPath $rollbackState.FirstMovedPath -PathType Leaf) 'user-modified published file was deleted'
    Assert-True ((Get-Content -Raw -LiteralPath $rollbackState.FirstMovedPath) -ceq 'user-replaced-after-publish') 'user-modified published file was changed during rollback'
    Assert-True (Test-Path -LiteralPath (Join-Path $rollbackOutput 'MySingerServer-compute-win-x64-rollback.zip.sha256') -PathType Leaf) 'cleanup-injected file was unexpectedly deleted'
    Assert-True (-not (Test-Path -LiteralPath (Join-Path $rollbackOutput 'MySingerServer-manager-win-x64-rollback.zip') -PathType Leaf)) 'rollback stopped after one cleanup item failed'

    $rollbackRaceOutput = Join-Path $testRoot 'rollback-race-release'
    $rollbackRaceTarget = Join-Path $rollbackRaceOutput 'MySingerServer-compute-win-x64-rollback-race.zip'
    $rollbackRaceOriginal = "$rollbackRaceTarget.published-original"
    $rollbackRaceState = [pscustomobject]@{ HookRan = $false }
    $rollbackRaceRejected = $false
    try {
        & $packageScript -StageDir $stage -OutputDir $rollbackRaceOutput -ReleaseId 'rollback-race' -BuildDate '2026-08-11' -TestPublishHook {
            param($context)
            if ($context.Phase -ceq 'AfterMove' -and $context.MoveIndex -eq 3) {
                throw 'INJECTED_ROLLBACK_RACE_PUBLISH_FAILURE'
            }
        } -TestRollbackHook {
            param($context)
            if ($context.Path -ceq $rollbackRaceTarget) {
                [IO.File]::Move($context.Path, $rollbackRaceOriginal, $false)
                Write-Utf8NoBom -Path $context.Path -Value 'user-replacement-after-verified-hash'
                $rollbackRaceState.HookRan = $true
            }
        }
    } catch {
        $rollbackRaceRejected = $_.Exception.Message -match `
            'PORTABLE_RELEASE_PUBLISH_FAILED.*INJECTED_ROLLBACK_RACE_PUBLISH_FAILURE'
    }
    Assert-True $rollbackRaceRejected 'hash/delete race did not keep the stable publish failure'
    Assert-True $rollbackRaceState.HookRan 'hash/delete race hook did not replace the verified path'
    Assert-True (Test-Path -LiteralPath $rollbackRaceTarget -PathType Leaf) `
        'rollback deleted the user file that replaced the verified path'
    Assert-True ((Get-Content -Raw -LiteralPath $rollbackRaceTarget) -ceq `
            'user-replacement-after-verified-hash') `
        'rollback changed the user file that replaced the verified path'
    Assert-True (-not (Test-Path -LiteralPath $rollbackRaceOriginal)) `
        'rollback did not delete the original verified file object'

    $candidateCleanupOutput = Join-Path $testRoot 'candidate-cleanup-release'
    $candidateCleanupState = [pscustomobject]@{ HookRan = $false }
    $candidateCleanupError = ''
    $candidateCleanupWarnings = @()
    try {
        & $packageScript -StageDir $stage -OutputDir $candidateCleanupOutput -ReleaseId 'candidate-cleanup' -BuildDate '2026-08-11' -TestPublishHook {
            param($context)
            if ($context.Phase -ceq 'AfterMove' -and $context.MoveIndex -eq 1) {
                throw 'INJECTED_PUBLISH_FAILURE_FOR_CANDIDATE_CLEANUP'
            }
            if ($context.Phase -ceq 'BeforeCandidateCleanup') {
                $candidateCleanupState.HookRan = $true
                throw 'INJECTED_CANDIDATE_CLEANUP_FAILURE'
            }
        } -WarningAction Stop -WarningVariable +candidateCleanupWarnings
    } catch {
        $candidateCleanupError = $_.Exception.Message
    }
    Assert-True $candidateCleanupState.HookRan 'candidate cleanup failure injection did not run'
    Assert-True ($candidateCleanupError -match 'PORTABLE_RELEASE_PUBLISH_FAILED.*INJECTED_PUBLISH_FAILURE_FOR_CANDIDATE_CLEANUP') 'candidate cleanup failure replaced the original publish error'
    Assert-True (@($candidateCleanupWarnings | Where-Object Message -Match 'PORTABLE_RELEASE_CANDIDATE_CLEANUP_WARNING').Count -eq 1) 'candidate cleanup failure did not emit the stable warning'

    Write-Host 'PORTABLE RELEASE PACKAGE CONTRACT PASS'
}
finally {
    if (Test-Path -LiteralPath $testRoot) { Remove-Item -LiteralPath $testRoot -Recurse -Force }
}
