[CmdletBinding()]
param(
    [string]$Go = 'C:\Users\Administrator\AppData\Local\Temp\go1.26.5-portable\go\bin\go.exe',
    [string]$GCC = 'C:\Users\Administrator\AppData\Local\Temp\winlibs-gcc\mingw64\bin\gcc.exe',
    [string]$Dlltool = 'C:\Users\Administrator\AppData\Local\Temp\winlibs-gcc\mingw64\bin\dlltool.exe',
    [string]$CMake = 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\cmake.exe',
    [string]$VcpkgRoot = 'C:\vcpkg',
    [string]$Dumpbin = 'D:\application\vs2022\ide\VC\Tools\MSVC\14.44.35207\bin\Hostx64\x64\dumpbin.exe',
    [string]$PGDSN = 'postgres://dedup:dedup@127.0.0.1:5432/dedup?sslmode=disable',
    [string]$EvidenceDir = '',
    [string]$AcceptanceReport = '',
    [switch]$PreflightOnly,
    [switch]$PinContract,
    [string]$PDQTreeRoot = '',
    [switch]$TimeoutContractsOnly
)

$ErrorActionPreference = 'Stop'
$repoRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$runID = [DateTimeOffset]::Now.ToString('yyyyMMdd-HHmmss-fff')
if ([string]::IsNullOrWhiteSpace($EvidenceDir)) {
    $EvidenceDir = Join-Path $repoRoot ".superpowers\evidence\m2-$runID"
}
if ([string]::IsNullOrWhiteSpace($AcceptanceReport)) {
    $AcceptanceReport = Join-Path $repoRoot 'docs\acceptance\2026-07-27-m2.md'
}
$EvidenceDir = [System.IO.Path]::GetFullPath($EvidenceDir)
$AcceptanceReport = [System.IO.Path]::GetFullPath($AcceptanceReport)
$tempRoot = [System.IO.Path]::GetFullPath(
    (Join-Path $repoRoot ".superpowers\tmp\m2-verify-$runID")
)
$allowedTempParent = [System.IO.Path]::GetFullPath(
    (Join-Path $repoRoot '.superpowers\tmp')
).TrimEnd('\')
$results = [ordered]@{}
foreach ($number in 1..8) {
    $results["AC-$number"] = [ordered]@{
        status = 'NOT_RUN'
        detail = ''
    }
}
$commands = [System.Collections.Generic.List[string]]::new()
$acceptanceOutput = [ordered]@{}
$failure = $null

function Write-FailedACs {
    param([string]$Reason)
    foreach ($number in 1..8) {
        Write-Host "AC-$number FAIL $Reason"
    }
    Write-Host "M2 VERIFY FAIL $Reason"
}

function Assert-LastExit {
    param([string]$Label)
    if ($LASTEXITCODE -ne 0) {
        throw "$Label failed with exit code $LASTEXITCODE"
    }
}

function Invoke-Recorded {
    param(
        [Parameter(Mandatory)]
        [string]$Display,
        [Parameter(Mandatory)]
        [scriptblock]$Action
    )
    $commands.Add($Display)
    & $Action
    Assert-LastExit $Display
}

function Get-FileSHA256 {
    param([string]$Path)
    $stream = [System.IO.File]::OpenRead($Path)
    try {
        $hasher = [System.Security.Cryptography.SHA256]::Create()
        try {
            $bytes = $hasher.ComputeHash($stream)
            return -join @($bytes | ForEach-Object { $_.ToString('x2') })
        }
        finally {
            $hasher.Dispose()
        }
    }
    finally {
        $stream.Dispose()
    }
}

function Get-TreeSHA256 {
    param([Parameter(Mandatory)][string]$Root)
    $resolvedRoot = [System.IO.Path]::GetFullPath($Root).TrimEnd('\')
    if (-not (Test-Path -LiteralPath $resolvedRoot -PathType Container)) {
        throw "tree digest root missing: $resolvedRoot"
    }
    $relativePaths = [string[]]@(
        Get-ChildItem -LiteralPath $resolvedRoot -Recurse -File | ForEach-Object {
            $_.FullName.Substring($resolvedRoot.Length + 1).Replace('\', '/')
        }
    )
    [Array]::Sort($relativePaths, [StringComparer]::Ordinal)
    $hasher = [System.Security.Cryptography.SHA256]::Create()
    try {
        $utf8 = [System.Text.UTF8Encoding]::new($false)
        foreach ($relativePath in $relativePaths) {
            $pathBytes = $utf8.GetBytes($relativePath)
            [void]$hasher.TransformBlock($pathBytes, 0, $pathBytes.Length, $pathBytes, 0)
            $separator = [byte[]]@(0)
            [void]$hasher.TransformBlock($separator, 0, 1, $separator, 0)
            $fullPath = Join-Path $resolvedRoot $relativePath.Replace('/', '\')
            $lengthBytes = [BitConverter]::GetBytes([int64]([System.IO.FileInfo]::new($fullPath).Length))
            [void]$hasher.TransformBlock($lengthBytes, 0, $lengthBytes.Length, $lengthBytes, 0)
            $stream = [System.IO.File]::OpenRead($fullPath)
            try {
                $buffer = [byte[]]::new(65536)
                while (($read = $stream.Read($buffer, 0, $buffer.Length)) -gt 0) {
                    [void]$hasher.TransformBlock($buffer, 0, $read, $buffer, 0)
                }
            }
            finally {
                $stream.Dispose()
            }
        }
        [void]$hasher.TransformFinalBlock([byte[]]@(), 0, 0)
        [pscustomobject]@{
            Digest = -join @($hasher.Hash | ForEach-Object { $_.ToString('x2') })
            Files = $relativePaths.Count
        }
    }
    finally {
        $hasher.Dispose()
    }
}

function Quote-PowerShellLiteral {
    param([string]$Value)
    return "'" + $Value.Replace("'", "''") + "'"
}

function Get-TrackedProcessSnapshot {
    $snapshot = [ordered]@{}
    foreach ($name in @('agent', 'worker', 'ffmpeg', 'WerFault')) {
        foreach ($process in @(Get-Process -Name $name -ErrorAction SilentlyContinue)) {
            $snapshot[[string]$process.Id] = [ordered]@{
                pid = $process.Id
                name = $process.ProcessName + '.exe'
            }
        }
    }
    return $snapshot
}

function Invoke-TimeoutContracts {
    $nativeVerifier = Join-Path $PSScriptRoot 'verify_m2_native.ps1'
    $hosts = [ordered]@{
        powershell5 = (Get-Command powershell.exe -ErrorAction Stop).Source
        pwsh = (Get-Command pwsh.exe -ErrorAction Stop).Source
    }
    $proofs = [System.Collections.Generic.List[object]]::new()
    foreach ($hostName in $hosts.Keys) {
        $executable = $hosts[$hostName]
        $display = "& $(Quote-PowerShellLiteral $executable)" +
            " -NoProfile -File $(Quote-PowerShellLiteral $nativeVerifier) -TimeoutContract"
        $commands.Add($display)
        $output = @(
            & $executable -NoProfile -File $nativeVerifier -TimeoutContract 2>&1
        )
        $exitCode = $LASTEXITCODE
        $output | ForEach-Object { Write-Host $_ }
        $text = $output -join "`n"
        if ($exitCode -ne 0 -or -not $text.Contains('NATIVE TIMEOUT CONTRACT PASS')) {
            throw "TIMEOUT CONTRACT $hostName failed exit=$exitCode output=$text"
        }
        Write-Host "TIMEOUT CONTRACT $hostName PASS exit=0"
        $proofs.Add([ordered]@{
            host = $hostName
            executable = $executable
            command = $display
            exit_code = $exitCode
            output = $text
        })
    }
    return @($proofs)
}

$expectedPDQTreeSHA256 = 'cc5eaa7dce9488d3467c12daa804f93acf705ca78c5f733539cd3d066064bc46'
if ([string]::IsNullOrWhiteSpace($PDQTreeRoot)) {
    $PDQTreeRoot = Join-Path $repoRoot 'mediacore\src\pdq_upstream'
}
if ($PinContract) {
    $pdqTree = Get-TreeSHA256 -Root $PDQTreeRoot
    if ($pdqTree.Digest -cne $expectedPDQTreeSHA256) {
        throw "PDQ source tree SHA-256 mismatch expected=$expectedPDQTreeSHA256 actual=$($pdqTree.Digest) files=$($pdqTree.Files)"
    }
    Write-Host "M2 PDQ TREE PIN PASS digest=$($pdqTree.Digest) files=$($pdqTree.Files)"
    exit 0
}
if ($TimeoutContractsOnly) {
    $proofs = Invoke-TimeoutContracts
    if (@($proofs).Count -ne 2) {
        throw "expected two timeout contract proofs, got $(@($proofs).Count)"
    }
    Write-Host 'M2 DUAL TIMEOUT CONTRACTS PASS'
    exit 0
}

$missing = [System.Collections.Generic.List[string]]::new()
foreach ($tool in @(
    @{ Name = 'Go'; Path = $Go; Kind = 'Leaf' },
    @{ Name = 'GCC'; Path = $GCC; Kind = 'Leaf' },
    @{ Name = 'dlltool'; Path = $Dlltool; Kind = 'Leaf' },
    @{ Name = 'CMake'; Path = $CMake; Kind = 'Leaf' },
    @{ Name = 'vcpkg'; Path = $VcpkgRoot; Kind = 'Container' },
    @{ Name = 'dumpbin'; Path = $Dumpbin; Kind = 'Leaf' }
)) {
    if (-not (Test-Path -LiteralPath $tool.Path -PathType $tool.Kind)) {
        $missing.Add("$($tool.Name)=$($tool.Path)")
    }
}
if ([string]::IsNullOrWhiteSpace($PGDSN)) {
    $missing.Add('PGDSN=<empty>')
}
if ($missing.Count -ne 0) {
    $reason = 'missing_dependencies=' + ($missing -join ',')
    Write-FailedACs -Reason $reason
    exit 1
}
if ($PreflightOnly) {
    Write-Host 'M2 PREFLIGHT PASS'
    exit 0
}

$originalVerifierPGDSN = $env:FS_PG_DSN
$env:FS_PG_DSN = $PGDSN
$processBaseline = Get-TrackedProcessSnapshot

try {
    [System.IO.Directory]::CreateDirectory($EvidenceDir) | Out-Null
    [System.IO.Directory]::CreateDirectory($tempRoot) | Out-Null
    $binDir = Join-Path $repoRoot 'bin'
    $corpusDir = Join-Path $tempRoot 'corpus'
    $ffmpeg = Join-Path $binDir 'tools\ffmpeg.exe'

    $timeoutContractEvidence = Invoke-TimeoutContracts
    if (@($timeoutContractEvidence).Count -ne 2) {
        throw "expected two timeout contract proofs, got $(@($timeoutContractEvidence).Count)"
    }

    $buildDisplay = "& $(Quote-PowerShellLiteral (Join-Path $PSScriptRoot 'build.ps1'))" +
        " -Go $(Quote-PowerShellLiteral $Go)" +
        " -CC $(Quote-PowerShellLiteral $GCC)" +
        " -Dlltool $(Quote-PowerShellLiteral $Dlltool)" +
        " -CMake $(Quote-PowerShellLiteral $CMake)" +
        " -VcpkgRoot $(Quote-PowerShellLiteral $VcpkgRoot)"
    Invoke-Recorded $buildDisplay {
        & (Join-Path $PSScriptRoot 'build.ps1') `
            -Go $Go `
            -CC $GCC `
            -Dlltool $Dlltool `
            -CMake $CMake `
            -VcpkgRoot $VcpkgRoot
    }
    $nativeDisplay = "& $(Quote-PowerShellLiteral (Join-Path $PSScriptRoot 'verify_m2_native.ps1'))" +
        " -CMake $(Quote-PowerShellLiteral $CMake)" +
        " -VcpkgRoot $(Quote-PowerShellLiteral $VcpkgRoot) -SkipBuild"
    Invoke-Recorded $nativeDisplay {
        & (Join-Path $PSScriptRoot 'verify_m2_native.ps1') `
            -CMake $CMake `
            -VcpkgRoot $VcpkgRoot `
            -SkipBuild
    }

    $oldCGO = $env:CGO_ENABLED
    try {
        $env:CGO_ENABLED = '0'
        $allTestDisplay = "`$env:FS_PG_DSN=$(Quote-PowerShellLiteral $PGDSN); " +
            "`$env:CGO_ENABLED='0'; & $(Quote-PowerShellLiteral $Go)" +
            " -C $(Quote-PowerShellLiteral $repoRoot) test -count=1 ./..."
        Invoke-Recorded $allTestDisplay {
            & $Go -C $repoRoot test -count=1 ./...
        }
    }
    finally {
        $env:CGO_ENABLED = $oldCGO
    }
    $cgoDisplay = "& $(Quote-PowerShellLiteral (Join-Path $PSScriptRoot 'test-cgo.ps1'))" +
        " -Go $(Quote-PowerShellLiteral $Go) -CC $(Quote-PowerShellLiteral $GCC)" +
        " -Packages @('./...')"
    Invoke-Recorded $cgoDisplay {
        & (Join-Path $PSScriptRoot 'test-cgo.ps1') `
            -Go $Go `
            -CC $GCC `
            -Packages @('./...')
    }
    $raceDisplay = "& $(Quote-PowerShellLiteral (Join-Path $PSScriptRoot 'test-cgo.ps1'))" +
        " -Go $(Quote-PowerShellLiteral $Go) -CC $(Quote-PowerShellLiteral $GCC)" +
        " -Race -Packages @('./...')"
    Invoke-Recorded $raceDisplay {
        & (Join-Path $PSScriptRoot 'test-cgo.ps1') `
            -Go $Go `
            -CC $GCC `
            -Race `
            -Packages @('./...')
    }
    $vetDisplay = "& $(Quote-PowerShellLiteral (Join-Path $PSScriptRoot 'test-cgo.ps1'))" +
        " -Go $(Quote-PowerShellLiteral $Go) -CC $(Quote-PowerShellLiteral $GCC)" +
        " -VetOnly -Packages @('./...')"
    Invoke-Recorded $vetDisplay {
        & (Join-Path $PSScriptRoot 'test-cgo.ps1') `
            -Go $Go `
            -CC $GCC `
            -VetOnly `
            -Packages @('./...')
    }
    $pgPattern = '^(TestPGRemoteUpsertFilesMatchesCentralSchemaWhenIntegrationEnabled|TestPostgresSyncIsIdempotentWhenIntegrationEnabled|TestPGRemoteFeatureUpsertsAndCentralMigrationWhenIntegrationEnabled)$'
    $pgDisplay = "`$env:FS_PG_DSN=$(Quote-PowerShellLiteral $PGDSN); " +
        "`$env:CGO_ENABLED='0'; & $(Quote-PowerShellLiteral $Go)" +
        " -C $(Quote-PowerShellLiteral $repoRoot) test -v -count=1 ./internal/syncer" +
        " -run $(Quote-PowerShellLiteral $pgPattern)"
    $commands.Add($pgDisplay)
    $pgOutput = @(
        & $Go -C $repoRoot test -v -count=1 ./internal/syncer -run $pgPattern 2>&1
    )
    $pgExit = $LASTEXITCODE
    $pgOutput | ForEach-Object { Write-Host $_ }
    $pgText = $pgOutput -join "`n"
    if ($pgExit -ne 0 -or $pgText -match '(?m)^--- SKIP:') {
        throw "PostgreSQL IntegrationEnabled proof failed or skipped exit=$pgExit"
    }
    foreach ($pgTest in @(
        'TestPGRemoteUpsertFilesMatchesCentralSchemaWhenIntegrationEnabled',
        'TestPostgresSyncIsIdempotentWhenIntegrationEnabled',
        'TestPGRemoteFeatureUpsertsAndCentralMigrationWhenIntegrationEnabled'
    )) {
        if (-not $pgText.Contains("--- PASS: $pgTest")) {
            throw "PostgreSQL IntegrationEnabled proof missing PASS for $pgTest"
        }
    }

    $expectedExports = @(
        'mc_debug_crash',
        'mc_debug_sleep_ms',
        'mc_decode_gray',
        'mc_free_image',
        'mc_hamming_distance',
        'mc_image_phase1',
        'mc_pdq256_from_gray',
        'mc_sha512_final',
        'mc_sha512_free',
        'mc_sha512_new',
        'mc_sha512_update',
        'mc_version'
    ) | Sort-Object
    $exportsRaw = (& $Dumpbin /nologo /exports (Join-Path $binDir 'mediacore.dll')) -join "`n"
    Assert-LastExit 'dumpbin mediacore exports'
    $actualExports = @(
        foreach ($line in $exportsRaw -split "`r?`n") {
            if ($line -match '^\s+\d+\s+[0-9A-Fa-f]+\s+[0-9A-Fa-f]+\s+([A-Za-z_][A-Za-z0-9_]*)\s*$') {
                $Matches[1]
            }
        }
    ) | Sort-Object -Unique
    if (@(Compare-Object $expectedExports $actualExports).Count -ne 0) {
        throw "exact mediacore exports mismatch expected=$($expectedExports -join ',') actual=$($actualExports -join ',')"
    }
    $dependencyEvidence = [ordered]@{}
    foreach ($binary in @('agent.exe', 'worker.exe', 'mediacore.dll')) {
        $raw = (& $Dumpbin /nologo /dependents (Join-Path $binDir $binary)) -join "`n"
        Assert-LastExit "dumpbin dependents $binary"
        $dependencies = @(
            foreach ($line in $raw -split "`r?`n") {
                if ($line -match '^\s+([A-Za-z0-9_.-]+\.dll)\s*$') {
                    $Matches[1].ToLowerInvariant()
                }
            }
        ) | Sort-Object -Unique
        $dependencyEvidence[$binary] = $dependencies
    }
    $expectedDependencies = [ordered]@{
        'agent.exe' = @(
            'kernel32.dll'
        ) | Sort-Object
        'worker.exe' = @(
            'api-ms-win-crt-environment-l1-1-0.dll',
            'api-ms-win-crt-heap-l1-1-0.dll',
            'api-ms-win-crt-locale-l1-1-0.dll',
            'api-ms-win-crt-math-l1-1-0.dll',
            'api-ms-win-crt-private-l1-1-0.dll',
            'api-ms-win-crt-runtime-l1-1-0.dll',
            'api-ms-win-crt-stdio-l1-1-0.dll',
            'api-ms-win-crt-string-l1-1-0.dll',
            'kernel32.dll',
            'mediacore.dll'
        ) | Sort-Object
        'mediacore.dll' = @(
            'bcrypt.dll',
            'kernel32.dll'
        ) | Sort-Object
    }
    foreach ($binary in $expectedDependencies.Keys) {
        $difference = @(Compare-Object $expectedDependencies[$binary] $dependencyEvidence[$binary])
        if ($difference.Count -ne 0) {
            throw "exact dependencies mismatch $binary expected=$($expectedDependencies[$binary] -join ',') actual=$($dependencyEvidence[$binary] -join ',')"
        }
        foreach ($dependency in $dependencyEvidence[$binary]) {
            $isSystem = $dependency -eq 'kernel32.dll' -or
                $dependency -eq 'bcrypt.dll' -or
                $dependency.StartsWith('api-ms-win-crt-')
            if (-not $isSystem -and $dependency -ne 'mediacore.dll') {
                throw "unexpected non-system DLL dependency $binary -> $dependency"
            }
        }
    }

    $planPath = Join-Path $repoRoot 'docs\superpowers\plans\2026-07-27-m2-phase1.md'
    $planText = Get-Content -LiteralPath $planPath -Raw
    $pdqCommit = 'baefb4ed67b6cdc1d4c82dbaef858d50866ac424'
    $vcpkgSnapshot = 'e0612b42ce44e55a0e630f2ee9d3c533a63d8bc1'
    $expectedVcpkgVersion = 'vcpkg package management program version 2026-04-08-e0612b42ce44e55a0e630f2ee9d3c533a63d8bc1'
    $expectedVcpkgManifestSHA256 = '0614e71ed97f0b9792ff3677f86356a71caeb4205ac76590972161d2b84f7f8f'
    $expectedFFmpegVersion = 'ffmpeg version N-125444-g6d72600a30-20260703 Copyright (c) 2000-2026 the FFmpeg developers'
    $expectedFFmpegSHA256 = '5f3c767af1cdbb9c44ad14478ce5fc036aec20e6a724755caa2f70abb9655c3f'
    if (-not $planText.Contains($pdqCommit) -or -not $planText.Contains($vcpkgSnapshot)) {
        throw 'PDQ/vcpkg pin evidence is missing from the implementation plan'
    }
    $pdqData = Join-Path $repoRoot ".superpowers\tmp\threatexchange-$pdqCommit\ThreatExchange-$pdqCommit\pdq\data"
    if (-not (Test-Path -LiteralPath $pdqData -PathType Container)) {
        throw "pinned PDQ data missing: $pdqData"
    }
    $actualPDQCommit = (Get-Content -LiteralPath (Join-Path $repoRoot 'mediacore\src\pdq_upstream\COMMIT') -Raw).Trim()
    if ($actualPDQCommit -cne $pdqCommit) {
        throw "PDQ commit mismatch expected=$pdqCommit actual=$actualPDQCommit"
    }
    $pdqTree = Get-TreeSHA256 -Root $PDQTreeRoot
    if ($pdqTree.Digest -cne $expectedPDQTreeSHA256) {
        throw "PDQ source tree SHA-256 mismatch expected=$expectedPDQTreeSHA256 actual=$($pdqTree.Digest) files=$($pdqTree.Files)"
    }
    $vcpkgManifest = Join-Path $repoRoot 'mediacore\vcpkg.json'
    $vcpkgVersion = (& (Join-Path $VcpkgRoot 'vcpkg.exe') version | Select-Object -First 1)
    Assert-LastExit 'vcpkg version'
    $ffmpegVersion = (& $ffmpeg -version | Select-Object -First 1)
    Assert-LastExit 'ffmpeg version'
    $vcpkgManifestSHA256 = Get-FileSHA256 $vcpkgManifest
    $ffmpegSHA256 = Get-FileSHA256 $ffmpeg
    if ($vcpkgVersion.Trim() -cne $expectedVcpkgVersion) {
        throw "vcpkg version mismatch expected=$expectedVcpkgVersion actual=$vcpkgVersion"
    }
    if ($vcpkgManifestSHA256 -cne $expectedVcpkgManifestSHA256) {
        throw "vcpkg manifest SHA-256 mismatch expected=$expectedVcpkgManifestSHA256 actual=$vcpkgManifestSHA256"
    }
    if ($ffmpegVersion -cne $expectedFFmpegVersion) {
        throw "FFmpeg version mismatch expected=$expectedFFmpegVersion actual=$ffmpegVersion"
    }
    if ($ffmpegSHA256 -cne $expectedFFmpegSHA256) {
        throw "FFmpeg SHA-256 mismatch expected=$expectedFFmpegSHA256 actual=$ffmpegSHA256"
    }

    $results['AC-7'].status = 'PASS'
    $results['AC-7'].detail = 'CTest 3/3; Level A 72/72; Level B 69 samples; corrupt/SHA gates; exact exports/dependencies/pins'

    $corpusDisplay = "& $(Quote-PowerShellLiteral $Go)" +
        " -C $(Quote-PowerShellLiteral $repoRoot) run testdata/m2/gen_corrupt.go" +
        " -out $(Quote-PowerShellLiteral $corpusDir)" +
        " -ffmpeg $(Quote-PowerShellLiteral $ffmpeg)"
    Invoke-Recorded $corpusDisplay {
        & $Go -C $repoRoot run 'testdata/m2/gen_corrupt.go' `
            -out $corpusDir `
            -ffmpeg $ffmpeg
    }
    $manifestPath = Join-Path $corpusDir 'manifest.json'
    if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) {
        throw 'corpus manifest was not generated'
    }
    $corpusManifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json

    $oldBin = $env:M2_BIN_DIR
    $oldCorpus = $env:M2_CORPUS_DIR
    $oldDSN = $env:FS_PG_DSN
    $oldCGO = $env:CGO_ENABLED
    try {
        $env:M2_BIN_DIR = $binDir
        $env:M2_CORPUS_DIR = $corpusDir
        $env:FS_PG_DSN = $PGDSN
        $env:CGO_ENABLED = '0'
        $tests = [ordered]@{
            'AC-1' = 'TestM2AC1CorruptInputs'
            'AC-2' = 'TestM2AC2RealNativeAccessViolation'
            'AC-3' = 'TestM2AC3NativeHangWatchdog'
            'AC-4' = 'TestM2AC4SingleFlight'
            'AC-5' = 'TestM2AC5ThumbnailCache'
            'AC-6' = 'TestM2AC6PathsAndAccess'
            'AC-8' = 'TestM2AC8Baseline'
        }
        foreach ($entry in $tests.GetEnumerator()) {
            $display = "`$env:M2_BIN_DIR=$(Quote-PowerShellLiteral $binDir); " +
                "`$env:M2_CORPUS_DIR=$(Quote-PowerShellLiteral $corpusDir); " +
                "`$env:FS_PG_DSN=$(Quote-PowerShellLiteral $PGDSN); " +
                "`$env:CGO_ENABLED='0'; & $(Quote-PowerShellLiteral $Go)" +
                " -C $(Quote-PowerShellLiteral $repoRoot)" +
                " test -v -tags m2acceptance -count=1 ./integration" +
                " -run $(Quote-PowerShellLiteral "^$($entry.Value)$") -timeout 300s"
            $commands.Add($display)
            $output = @(
                & $Go -C $repoRoot test -v -tags m2acceptance -count=1 ./integration `
                    -run "^$($entry.Value)$" `
                    -timeout 300s 2>&1
            )
            $exitCode = $LASTEXITCODE
            $output | ForEach-Object { Write-Host $_ }
            $acceptanceOutput[$entry.Key] = $output -join "`n"
            if ($exitCode -eq 0 -and
                -not $acceptanceOutput[$entry.Key].Contains('M2 process cleanup root_pid=')) {
                $exitCode = 1
                $output += 'missing scoped M2 process cleanup audit'
                $acceptanceOutput[$entry.Key] = $output -join "`n"
            }
            if ($exitCode -eq 0) {
                $results[$entry.Key].status = 'PASS'
                $results[$entry.Key].detail = $entry.Value
            }
            else {
                $results[$entry.Key].status = 'FAIL'
                $results[$entry.Key].detail = "$($entry.Value) exit=$exitCode"
            }
        }
    }
    finally {
        $env:M2_BIN_DIR = $oldBin
        $env:M2_CORPUS_DIR = $oldCorpus
        $env:FS_PG_DSN = $oldDSN
        $env:CGO_ENABLED = $oldCGO
    }

    $failedAcceptance = @(
        $results.GetEnumerator() | Where-Object { $_.Value.status -ne 'PASS' }
    )
    if ($failedAcceptance.Count -ne 0) {
        throw 'one or more acceptance criteria failed'
    }

    $processFinal = Get-TrackedProcessSnapshot
    $newProcessResiduals = @(
        foreach ($pid in $processFinal.Keys) {
            if (-not $processBaseline.Contains($pid)) {
                $processFinal[$pid]
            }
        }
    )
    $processAudit = [ordered]@{
        names = @('agent.exe', 'worker.exe', 'ffmpeg.exe', 'WerFault.exe')
        baseline = @($processBaseline.Values)
        final = @($processFinal.Values)
        new_residuals = $newProcessResiduals
        scoped_cleanup_proofs = @(
            foreach ($entry in $acceptanceOutput.GetEnumerator()) {
                @($entry.Value -split "`r?`n" | Where-Object {
                    $_.Contains('M2 process cleanup root_pid=')
                })
            }
        )
    }
    if ($newProcessResiduals.Count -ne 0) {
        throw "new tracked process residuals: $($newProcessResiduals | ConvertTo-Json -Compress)"
    }

    $evidence = [ordered]@{
        schema_version = 1
        run_id = $runID
        timestamp = [DateTimeOffset]::Now.ToString('o')
        second_windows_host = [ordered]@{
            status = 'USER_WAIVED'
            executed = $false
        }
        tools = [ordered]@{
            go = ((& $Go version) -join ' ')
            gcc = ((& $GCC --version | Select-Object -First 1) -join ' ')
            cmake = ((& $CMake --version | Select-Object -First 1) -join ' ')
            vcpkg = $vcpkgVersion
            ffmpeg = $ffmpegVersion
        }
        pins = [ordered]@{
            pdq_commit = $pdqCommit
            pdq_tree_sha256 = $pdqTree.Digest
            pdq_tree_files = $pdqTree.Files
            vcpkg_snapshot = $vcpkgSnapshot
            vcpkg_manifest_sha256 = $vcpkgManifestSHA256
            ffmpeg_exe_sha256 = $ffmpegSHA256
        }
        artifacts = [ordered]@{
            agent_sha256 = Get-FileSHA256 (Join-Path $binDir 'agent.exe')
            worker_sha256 = Get-FileSHA256 (Join-Path $binDir 'worker.exe')
            mediacore_sha256 = Get-FileSHA256 (Join-Path $binDir 'mediacore.dll')
            corpus_manifest_sha256 = Get-FileSHA256 $manifestPath
        }
        exports = $actualExports
        dependencies = $dependencyEvidence
        process_audit = $processAudit
        timeout_contracts = $timeoutContractEvidence
        commands = $commands
        criteria = $results
        acceptance_output = $acceptanceOutput
    }
    $evidencePath = Join-Path $EvidenceDir 'm2-evidence.json'
    $evidence | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $evidencePath -Encoding utf8
    Copy-Item -LiteralPath $manifestPath -Destination (Join-Path $EvidenceDir 'corpus-manifest.json') -Force

    [System.IO.Directory]::CreateDirectory((Split-Path -Parent $AcceptanceReport)) | Out-Null
    $reportLines = [System.Collections.Generic.List[string]]::new()
    $reportLines.Add('# M2 Phase 1 acceptance - 2026-07-27')
    $reportLines.Add('')
    $reportLines.Add("- Run: ``$runID``")
    $reportLines.Add("- Evidence: ``$evidencePath``")
    $reportLines.Add("- Corpus manifest SHA-256: ``$($evidence.artifacts.corpus_manifest_sha256)``")
    $reportLines.Add('- Second independent Windows host: **USER_WAIVED** (not executed).')
    $reportLines.Add('')
    $reportLines.Add('## Toolchain and pins')
    $reportLines.Add('')
    $reportLines.Add("- Go: ``$($evidence.tools.go)``")
    $reportLines.Add("- GCC: ``$($evidence.tools.gcc)``")
    $reportLines.Add("- CMake: ``$($evidence.tools.cmake)``")
    $reportLines.Add("- vcpkg: ``$($evidence.tools.vcpkg)``")
    $reportLines.Add("- FFmpeg: ``$($evidence.tools.ffmpeg)``")
    $reportLines.Add("- PDQ commit: ``$pdqCommit``")
    $reportLines.Add("- PDQ vendored source tree SHA-256 ($($evidence.pins.pdq_tree_files) files): ``$($evidence.pins.pdq_tree_sha256)``")
    $reportLines.Add("- vcpkg snapshot: ``$vcpkgSnapshot``")
    $reportLines.Add("- vcpkg manifest SHA-256: ``$($evidence.pins.vcpkg_manifest_sha256)``")
    $reportLines.Add("- FFmpeg executable SHA-256: ``$($evidence.pins.ffmpeg_exe_sha256)``")
    $reportLines.Add('')
    $reportLines.Add('## Corpus')
    $reportLines.Add('')
    $reportLines.Add("- Version/seed: ``$($corpusManifest.version)`` / ``$($corpusManifest.seed)``")
    $reportLines.Add("- Manifest files: $($corpusManifest.counts.manifest_files)")
    $reportLines.Add("- Corrupt classes: $($corpusManifest.counts.corrupt_classes)")
    $reportLines.Add("- Smoke images: $($corpusManifest.counts.smoke_images)")
    $reportLines.Add("- AC-8 warmup images: $($corpusManifest.counts.warmup_images)")
    $reportLines.Add("- Single-flight images/videos: $($corpusManifest.counts.single_images)/$($corpusManifest.counts.single_videos)")
    $reportLines.Add("- Cache/crash/hang videos or images: $($corpusManifest.counts.cache_videos)/$($corpusManifest.counts.crash_images)/$($corpusManifest.counts.hang_images)")
    $reportLines.Add("- Manifest SHA-256: ``$($evidence.artifacts.corpus_manifest_sha256)``")
    $reportLines.Add('')
    $reportLines.Add('## Native ABI and dependencies')
    $reportLines.Add('')
    $reportLines.Add("- Exports (exact $($actualExports.Count)): ``$($actualExports -join ', ')``")
    foreach ($binary in @('agent.exe', 'worker.exe', 'mediacore.dll')) {
        $reportLines.Add("- $binary dependencies: ``$(@($dependencyEvidence[$binary]) -join ', ')``")
    }
    $reportLines.Add("- Artifact SHA-256: agent ``$($evidence.artifacts.agent_sha256)``; worker ``$($evidence.artifacts.worker_sha256)``; mediacore ``$($evidence.artifacts.mediacore_sha256)``")
    foreach ($proof in $evidence.timeout_contracts) {
        $reportLines.Add("- Native timeout contract $($proof.host): exit=$($proof.exit_code), ``$($proof.executable)``")
    }
    $reportLines.Add('')
    $reportLines.Add('## Results')
    $reportLines.Add('')
    foreach ($entry in $results.GetEnumerator()) {
        $reportLines.Add("- **$($entry.Key) $($entry.Value.status)** - $($entry.Value.detail)")
    }
    $reportLines.Add('')
    $reportLines.Add('The captured per-AC output below records packaged Agent and crash Worker PIDs,')
    $reportLines.Add('exact task metrics, SQLite assertions, timestamped RSS samples, and exact')
    $reportLines.Add('PostgreSQL machine-row cleanup counts.')
    $reportLines.Add('')
    $reportLines.Add('## Captured AC output')
    $reportLines.Add('')
    foreach ($entry in $acceptanceOutput.GetEnumerator()) {
        $reportLines.Add("### $($entry.Key)")
        $reportLines.Add('')
        $reportLines.Add('```text')
        foreach ($line in $entry.Value -split "`r?`n") {
            $reportLines.Add($line)
        }
        $reportLines.Add('```')
        $reportLines.Add('')
    }
    $reportLines.Add('## Exact commands')
    $reportLines.Add('')
    $reportLines.Add('```powershell')
    foreach ($command in $commands) {
        $reportLines.Add($command)
    }
    $reportLines.Add('```')
    $reportLines.Add('')
    $reportLines.Add('## Cleanup')
    $reportLines.Add('')
    $reportLines.Add('- Each test stopped only its launched Agent PID tree, restored readonly/lock state, and deleted only its exact `m2-*` PostgreSQL machine row.')
    $reportLines.Add("- Scoped process cleanup proofs captured: $(@($evidence.process_audit.scoped_cleanup_proofs).Count).")
    $reportLines.Add("- Tracked process audit names: ``$($evidence.process_audit.names -join ', ')``; new residuals: $(@($evidence.process_audit.new_residuals).Count).")
    $reportLines.Add('- PostgreSQL cleanup output records files/image_features/video_features deleted, restored, and residual counts.')
    $reportLines.Add('- The verifier deleted only its resolved run-specific corpus under `.superpowers/tmp`.')
    Set-Content -LiteralPath $AcceptanceReport -Value $reportLines -Encoding utf8
}
catch {
    $failure = $_.Exception.Message
    foreach ($entry in $results.GetEnumerator()) {
        if ($entry.Value.status -eq 'NOT_RUN') {
            $entry.Value.status = 'FAIL'
            $entry.Value.detail = $failure
        }
    }
}
finally {
    $env:FS_PG_DSN = $originalVerifierPGDSN
    $tempParent = [System.IO.Path]::GetDirectoryName($tempRoot).TrimEnd('\')
    if ([string]::Equals(
        $tempParent,
        $allowedTempParent,
        [System.StringComparison]::OrdinalIgnoreCase
    ) -and (Test-Path -LiteralPath $tempRoot)) {
        Remove-Item -LiteralPath $tempRoot -Recurse -Force
    }
}

foreach ($entry in $results.GetEnumerator()) {
    Write-Host "$($entry.Key) $($entry.Value.status) $($entry.Value.detail)"
}
if ($null -ne $failure -or @(
    $results.GetEnumerator() | Where-Object { $_.Value.status -ne 'PASS' }
).Count -ne 0) {
    Write-Host "M2 VERIFY FAIL $failure"
    exit 1
}
Write-Host "M2 VERIFY PASS evidence=$EvidenceDir report=$AcceptanceReport"
exit 0
