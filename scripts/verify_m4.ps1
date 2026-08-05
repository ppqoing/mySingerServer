[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$Go,

    [Parameter(Mandatory = $true)]
    [string]$GCC,

    [Parameter(Mandatory = $true)]
    [string]$PGDSN,

    [string]$CMake = '',

    [string]$VcpkgRoot = '',

    [string]$Dumpbin = ''
)

$ErrorActionPreference = 'Stop'
$repoRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$runID = '{0}-{1}' -f (
    [DateTimeOffset]::Now.ToString('yyyyMMdd-HHmmss-fff')
), ([Guid]::NewGuid().ToString('N').Substring(0, 8))
$requiredGates = @(
    'format',
    'native_ctest',
    'native_exports',
    'binary_staging',
    'pure_go_full',
    'cgo_full',
    'race_changed',
    'vet',
    'postgres_contracts',
    'm4_e2e',
    'marker_negative',
    'schema_index_audit',
    'public_unchanged',
    'cleanup_audit',
    'secret_scan'
)

function Protect-M4BootstrapSecret {
    param([AllowEmptyString()][string]$Text)
    if ($null -eq $Text) {
        return ''
    }
    $safe = $Text
    if (-not [string]::IsNullOrEmpty($PGDSN)) {
        $safe = $safe.Replace($PGDSN, '[REDACTED_DSN]')
    }
    return [regex]::Replace(
        $safe,
        'postgres(?:ql)?://[^/\s:@]+:[^@\s/]+@',
        'postgres://[REDACTED]@',
        [System.Text.RegularExpressions.RegexOptions]::IgnoreCase
    )
}

function Assert-M4BootstrapExistingAncestorsSafe {
    param(
        [Parameter(Mandatory = $true)][string]$Root,
        [Parameter(Mandatory = $true)][string]$Target
    )
    $fullRoot = [System.IO.Path]::GetFullPath($Root)
    $fullTarget = [System.IO.Path]::GetFullPath($Target)
    $relative = [System.IO.Path]::GetRelativePath($fullRoot, $fullTarget)
    if ([System.IO.Path]::IsPathRooted($relative) -or
        $relative -eq '..' -or
        $relative.StartsWith('..\', [System.StringComparison]::Ordinal)) {
        throw 'M4 bootstrap path must stay inside the workspace.'
    }
    $current = $fullRoot
    foreach ($component in @('') + @($relative -split '[\\/]')) {
        if (-not [string]::IsNullOrEmpty($component)) {
            $current = Join-Path $current $component
            if (-not [System.IO.Directory]::Exists($current) -and
                -not [System.IO.File]::Exists($current)) {
                break
            }
        }
        try {
            $attributes = [System.IO.File]::GetAttributes($current)
        }
        catch {
            throw "M4 bootstrap path component is unreadable: $current"
        }
        if (($attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw "M4 bootstrap path contains a reparse point: $current"
        }
        if ($current -cne $fullTarget -and
            ($attributes -band [System.IO.FileAttributes]::Directory) -eq 0) {
            throw "M4 bootstrap path ancestor is not a directory: $current"
        }
    }
}

function New-M4SafeBootstrapDirectory {
    param(
        [Parameter(Mandatory = $true)][string]$Root,
        [Parameter(Mandatory = $true)][string]$Target
    )
    $fullRoot = [System.IO.Path]::GetFullPath($Root)
    $fullTarget = [System.IO.Path]::GetFullPath($Target)
    Assert-M4BootstrapExistingAncestorsSafe `
        -Root $fullRoot `
        -Target $fullTarget
    $relative = [System.IO.Path]::GetRelativePath($fullRoot, $fullTarget)
    $current = $fullRoot
    foreach ($component in @($relative -split '[\\/]')) {
        if ([string]::IsNullOrEmpty($component)) {
            continue
        }
        Assert-M4BootstrapExistingAncestorsSafe `
            -Root $fullRoot `
            -Target $current
        $current = Join-Path $current $component
        if (-not [System.IO.Directory]::Exists($current) -and
            -not [System.IO.File]::Exists($current)) {
            [System.IO.Directory]::CreateDirectory($current) | Out-Null
        }
        $attributes = [System.IO.File]::GetAttributes($current)
        if (($attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw "M4 bootstrap path contains a reparse point: $current"
        }
        if (($attributes -band [System.IO.FileAttributes]::Directory) -eq 0) {
            throw "M4 bootstrap path component is not a directory: $current"
        }
    }
    Assert-M4BootstrapExistingAncestorsSafe `
        -Root $fullRoot `
        -Target $fullTarget
    return $fullTarget
}

$evidenceDir = [System.IO.Path]::GetFullPath(
    (Join-Path $repoRoot ".superpowers\evidence\m4-$runID")
)
$summaryPath = ''
$bootstrapFailure = $null
try {
    $markerScript = if ([string]::IsNullOrWhiteSpace(
        [string]$env:M4_TEST_MARKER_PATH
    )) {
        Join-Path $PSScriptRoot 'verify_m4_marker.ps1'
    }
    else {
        [string]$env:M4_TEST_MARKER_PATH
    }
    . $markerScript
    if ($env:M4_TEST_FAIL_EVIDENCE_CREATE -eq '1') {
        throw 'injected M4 evidence directory creation failure'
    }
    [void](New-M4SafeBootstrapDirectory `
        -Root $repoRoot `
        -Target $evidenceDir)
    Assert-M4PathHasNoReparsePoint `
        -FullRoot $repoRoot `
        -FullPath $evidenceDir
    $summaryPath = Join-Path $evidenceDir 'm4-evidence.json'
}
catch {
    $bootstrapFailure = Protect-M4BootstrapSecret (
        [string]$_.Exception.Message
    )
}

if ($null -ne $bootstrapFailure) {
    $bootstrapGates = [ordered]@{}
    foreach ($name in $requiredGates) {
        $bootstrapGates[$name] = [ordered]@{
            status = 'NOT_RUN'
            exit_code = $null
            log = ''
        }
    }
    $bootstrapSummary = [ordered]@{
        schema_version = 1
        run_id = $runID
        timestamp = [DateTimeOffset]::Now.ToString('o')
        status = 'FAIL'
        required_gates = $requiredGates
        gates = $bootstrapGates
        tools = [ordered]@{}
        commands = @()
        acceptance = $null
        failure = $bootstrapFailure
    }
    $summaryPath = '-'
    $fallbackFailures = [System.Collections.Generic.List[string]]::new()
    $fallbackRoots = @(
        (Join-Path $repoRoot '.superpowers\tmp'),
        (Join-Path $repoRoot '.superpowers\bootstrap')
    )
    foreach ($fallbackRoot in $fallbackRoots) {
        try {
            $fallbackDir = [System.IO.Path]::GetFullPath(
                (Join-Path $fallbackRoot "m4-bootstrap-$runID")
            )
            [void](New-M4SafeBootstrapDirectory `
                -Root $repoRoot `
                -Target $fallbackDir)
            $candidateSummary = Join-Path $fallbackDir 'm4-evidence.json'
            $encoding = [System.Text.UTF8Encoding]::new($false)
            [System.IO.File]::WriteAllText(
                $candidateSummary,
                ($bootstrapSummary | ConvertTo-Json -Depth 16),
                $encoding
            )
            Assert-M4BootstrapExistingAncestorsSafe `
                -Root $repoRoot `
                -Target $candidateSummary
            $summaryPath = $candidateSummary
            break
        }
        catch {
            $fallbackFailures.Add(
                (Protect-M4BootstrapSecret ([string]$_.Exception.Message))
            )
        }
    }
    $finalFailure = $bootstrapFailure
    if ($summaryPath -eq '-' -and $fallbackFailures.Count -ne 0) {
        $finalFailure = Protect-M4BootstrapSecret (
            "$bootstrapFailure; no safe bootstrap summary path: " +
            ($fallbackFailures -join '; ')
        )
    }
    foreach ($name in $requiredGates) {
        Write-Host "M4 GATE $name NOT_RUN exit=- log=-"
    }
    Write-Host (
        "M4 FINAL RESULT FAIL run_id=$runID evidence=$summaryPath reason=$finalFailure"
    )
    exit 1
}

$gates = [ordered]@{}
$commands = [System.Collections.Generic.List[string]]::new()
$failure = $null
$goPath = ''
$gccPath = ''
$gofmtPath = ''
$pwshPath = ''
$dlltoolPath = ''
$cmakePath = ''
$ctestPath = ''
$dumpbinPath = ''
$vcpkgRootPath = ''
$stageDir = Join-Path $evidenceDir 'fresh-bin'
$acceptanceMarker = $null
$postgresTests = @(
    'TestPostgres16BuildAndPersistFixtureWhenIntegrationEnabled',
    'TestPostgres16ScopedPendingEnvelopeRestoreWhenIntegrationEnabled',
    'TestPostgresPendingTargetAuditRejectsUnknownDiscriminatorWhenIntegrationEnabled',
    'TestPostgres16ScopedRescreenerRestoreReplayAndBarrierWhenIntegrationEnabled',
    'TestPostgres16ScopedGroupRebuildSchemaTwiceCleanupAndConcurrencyWhenEnabled',
    'TestPostgresGroupsDeletedRepresentativeFallbackAndLiveFilteringWhenEnabled'
)

function Protect-M4Secret {
    param([AllowEmptyString()][string]$Text)
    if ($null -eq $Text) {
        return ''
    }
    $safe = $Text
    if (-not [string]::IsNullOrEmpty($PGDSN)) {
        $safe = $safe.Replace($PGDSN, '[REDACTED_DSN]')
    }
    return [regex]::Replace(
        $safe,
        'postgres(?:ql)?://[^/\s:@]+:[^@\s/]+@',
        'postgres://[REDACTED]@',
        [System.Text.RegularExpressions.RegexOptions]::IgnoreCase
    )
}

function Resolve-M4RequiredFile {
    param(
        [Parameter(Mandatory = $true)][string]$Value,
        [Parameter(Mandatory = $true)][string]$Label
    )
    if ([string]::IsNullOrWhiteSpace($Value)) {
        throw "$Label must be an explicit non-empty path."
    }
    try {
        $resolved = (Resolve-Path -LiteralPath $Value -ErrorAction Stop).Path
    }
    catch {
        throw "$Label path does not exist."
    }
    if (-not (Test-Path -LiteralPath $resolved -PathType Leaf)) {
        throw "$Label must point to a file."
    }
    return [System.IO.Path]::GetFullPath($resolved)
}

function Resolve-M4OptionalTool {
    param(
        [AllowEmptyString()][string]$Requested,
        [Parameter(Mandatory = $true)][string]$Command,
        [Parameter(Mandatory = $true)][string[]]$Candidates,
        [Parameter(Mandatory = $true)][string]$Label
    )
    if (-not [string]::IsNullOrWhiteSpace($Requested)) {
        return Resolve-M4RequiredFile -Value $Requested -Label $Label
    }
    $found = Get-Command $Command -CommandType Application `
        -ErrorAction SilentlyContinue
    if ($null -ne $found) {
        return Resolve-M4RequiredFile -Value $found.Source -Label $Label
    }
    foreach ($candidate in $Candidates) {
        if (-not [string]::IsNullOrWhiteSpace($candidate) -and
            (Test-Path -LiteralPath $candidate -PathType Leaf)) {
            return Resolve-M4RequiredFile -Value $candidate -Label $Label
        }
    }
    throw "$Label executable was not supplied and discovery found no candidate."
}

function Resolve-M4EvidencePath {
    param([Parameter(Mandatory = $true)][string]$Path)
    $full = [System.IO.Path]::GetFullPath($Path)
    $prefix = $evidenceDir.TrimEnd('\') + '\'
    if (-not $full.StartsWith(
        $prefix,
        [System.StringComparison]::OrdinalIgnoreCase
    )) {
        throw 'gate evidence path must stay inside this M4 evidence directory.'
    }
    return $full
}

function Write-M4Text {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [AllowEmptyString()][string]$Text
    )
    $encoding = [System.Text.UTF8Encoding]::new($false)
    $fullPath = Resolve-M4EvidencePath $Path
    [System.IO.File]::WriteAllText(
        $fullPath,
        (Protect-M4Secret $Text),
        $encoding
    )
    Assert-M4PathHasNoReparsePoint `
        -FullRoot $evidenceDir `
        -FullPath $fullPath
}

function Invoke-M4ExternalGate {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][string]$Executable,
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [Parameter(Mandatory = $true)][string]$Display
    )
    $logPath = Resolve-M4EvidencePath (Join-Path $evidenceDir "$Name.log")
    $commands.Add($Display)
    Write-Host "M4 gate start: $Name"
    $raw = @(& $Executable @Arguments 2>&1)
    $exitCode = $LASTEXITCODE
    $safe = @($raw | ForEach-Object { Protect-M4Secret ([string]$_) })
    Write-M4Text -Path $logPath -Text ($safe -join "`r`n")
    foreach ($line in $safe) {
        Write-Host $line
    }
    $gates[$Name] = [ordered]@{
        status = if ($exitCode -eq 0) { 'PASS' } else { 'FAIL' }
        exit_code = [int]$exitCode
        log = $logPath
    }
    if ($exitCode -ne 0) {
        throw "$Name failed with exit code $exitCode"
    }
    return ($safe -join "`n")
}

function Invoke-M4ValidationGate {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][scriptblock]$Action
    )
    $logPath = Resolve-M4EvidencePath (Join-Path $evidenceDir "$Name.log")
    try {
        $value = & $Action
        if ($value -isnot [string]) {
            $value = $value | ConvertTo-Json -Depth 12
        }
        Write-M4Text -Path $logPath -Text ([string]$value)
        $gates[$Name] = [ordered]@{
            status = 'PASS'
            exit_code = 0
            log = $logPath
        }
        $commands.Add("internal audit $Name")
        return $value
    }
    catch {
        Write-M4Text -Path $logPath -Text $_.Exception.Message
        $gates[$Name] = [ordered]@{
            status = 'FAIL'
            exit_code = 1
            log = $logPath
        }
        throw
    }
}

function Invoke-M4GoGate {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][string[]]$Arguments
    )
    return Invoke-M4ExternalGate `
        -Name $Name `
        -Executable $script:goPath `
        -Arguments $Arguments `
        -Display ('go ' + ($Arguments -join ' '))
}

function Confirm-M4Gate {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][scriptblock]$Action
    )
    try {
        return & $Action
    }
    catch {
        if ($gates.Contains($Name)) {
            $gates[$Name].status = 'FAIL'
        }
        throw
    }
}

function Get-M4OwnedGoFiles {
    return [string[]]@(Get-M4RepoOwnedGoFiles -RepoRoot $repoRoot)
}

function Get-M4SHA256 {
    param([Parameter(Mandatory = $true)][string]$Path)
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Assert-M4NamedPasses {
    param(
        [Parameter(Mandatory = $true)][string]$Output,
        [Parameter(Mandatory = $true)][string[]]$Names
    )
    if ($Output -match '(?m)^\s*--- SKIP:') {
        throw 'named acceptance output contains a skipped test.'
    }
    foreach ($name in $Names) {
        if (-not $Output.Contains("=== RUN   $name") -or
            -not $Output.Contains("--- PASS: $name")) {
            throw "named acceptance output lacks a RUN/PASS proof for $name"
        }
    }
}

function Restore-M4Environment {
    param([hashtable]$Snapshot)
    foreach ($name in $Snapshot.Keys) {
        $entry = $Snapshot[$name]
        if ($entry.exists) {
            Set-Item -LiteralPath "Env:$name" -Value $entry.value
        }
        else {
            Remove-Item -LiteralPath "Env:$name" -ErrorAction SilentlyContinue
        }
    }
}

$environmentNames = @(
    'PATH', 'CGO_ENABLED', 'CC', 'GOOS', 'GOARCH',
    'FS_PG_DSN', 'DEDUP_TEST_PG_DSN',
    'M4_VERIFY_RUN_ID', 'M4_EVIDENCE_PATH',
    'M4_E2E_BIN_DIR', 'DEDUP_TEST_M4_BIN_DIR'
)
$environmentBefore = @{}
foreach ($name in $environmentNames) {
    $item = Get-Item -LiteralPath "Env:$name" -ErrorAction SilentlyContinue
    $environmentBefore[$name] = @{
        exists = $null -ne $item
        value = if ($null -ne $item) { [string]$item.Value } else { '' }
    }
}
$locationBefore = Get-Location

try {
    if ([string]::IsNullOrWhiteSpace($PGDSN)) {
        throw '-PGDSN must be explicit and non-empty.'
    }
    $script:goPath = Resolve-M4RequiredFile -Value $Go -Label '-Go'
    $goPath = $script:goPath
    $gccPath = Resolve-M4RequiredFile -Value $GCC -Label '-GCC'
    $gofmtPath = Resolve-M4RequiredFile `
        -Value (Join-Path (Split-Path -Parent $goPath) 'gofmt.exe') `
        -Label 'gofmt'
    $pwshPath = Resolve-M4RequiredFile `
        -Value (Join-Path $PSHOME 'pwsh.exe') `
        -Label 'pwsh'
    $dlltoolPath = Resolve-M4RequiredFile `
        -Value (Join-Path (Split-Path -Parent $gccPath) 'dlltool.exe') `
        -Label 'dlltool'
    $requestedVcpkgRoot = if (-not [string]::IsNullOrWhiteSpace($VcpkgRoot)) {
        $VcpkgRoot
    }
    elseif (-not [string]::IsNullOrWhiteSpace($env:VCPKG_ROOT)) {
        [string]$env:VCPKG_ROOT
    }
    else {
        'C:\vcpkg'
    }
    try {
        $vcpkgRootPath = [System.IO.Path]::GetFullPath(
            (Resolve-Path -LiteralPath $requestedVcpkgRoot `
                -ErrorAction Stop).Path
        )
    }
    catch {
        throw 'vcpkg root does not exist; supply -VcpkgRoot explicitly.'
    }
    if (-not (Test-Path -LiteralPath $vcpkgRootPath -PathType Container)) {
        throw 'vcpkg root must be a directory.'
    }
    $cmakePath = Resolve-M4OptionalTool `
        -Requested $CMake `
        -Command 'cmake.exe' `
        -Candidates @(
            (Join-Path $vcpkgRootPath `
                'downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\cmake.exe')
        ) `
        -Label 'cmake'
    $ctestPath = Resolve-M4RequiredFile `
        -Value (Join-Path (Split-Path -Parent $cmakePath) 'ctest.exe') `
        -Label 'ctest'
    $dumpbinPath = Resolve-M4OptionalTool `
        -Requested $Dumpbin `
        -Command 'dumpbin.exe' `
        -Candidates @(
            'D:\application\vs2022\ide\VC\Tools\MSVC\14.44.35207\bin\Hostx64\x64\dumpbin.exe'
        ) `
        -Label 'dumpbin'

    Set-Location -LiteralPath $repoRoot
    $env:PATH = (
        $stageDir,
        (Join-Path $stageDir 'tools'),
        (Split-Path -Parent $gccPath),
        (Split-Path -Parent $goPath),
        $environmentBefore['PATH'].value
    ) -join ';'
    foreach ($name in @(
        'FS_PG_DSN', 'DEDUP_TEST_PG_DSN',
        'M4_VERIFY_RUN_ID', 'M4_EVIDENCE_PATH',
        'M4_E2E_BIN_DIR', 'DEDUP_TEST_M4_BIN_DIR'
    )) {
        Remove-Item -LiteralPath "Env:$name" -ErrorAction SilentlyContinue
    }

    $ownedGo = @(Get-M4OwnedGoFiles)
    $formatOutput = Invoke-M4ExternalGate `
        -Name 'format' `
        -Executable $gofmtPath `
        -Arguments (@('-l') + $ownedGo) `
        -Display "gofmt -l <repo-owned files=$($ownedGo.Count)>"
    [void](Confirm-M4Gate -Name 'format' -Action {
        if (-not [string]::IsNullOrWhiteSpace($formatOutput)) {
            throw "format gate found unformatted files: $formatOutput"
        }
    })

    $stageOutDir = Get-M4BuildOutDir `
        -RepoRoot $repoRoot `
        -StageDir $stageDir
    $buildOutput = Invoke-M4ExternalGate `
        -Name 'native_ctest' `
        -Executable $pwshPath `
        -Arguments @(
            '-NoLogo', '-NoProfile', '-File',
            (Join-Path $PSScriptRoot 'build.ps1'),
            '-Go', $goPath,
            '-CC', $gccPath,
            '-Dlltool', $dlltoolPath,
            '-OutDir', $stageOutDir,
            '-CMake', $cmakePath,
            '-VcpkgRoot', $vcpkgRootPath
        ) `
        -Display 'scripts/build.ps1 <fresh evidence stage>'
    [void](Confirm-M4Gate -Name 'native_ctest' -Action {
        if ($buildOutput -notmatch '100% tests passed, 0 tests failed out of 4') {
            throw 'native CTest output is not exactly 4/4 passing.'
        }
        Assert-M4PathHasNoReparsePoint `
            -FullRoot $evidenceDir `
            -FullPath $stageDir
        foreach ($relative in @(
            'agent.exe', 'gui.exe', 'worker.exe', 'mediacore.dll',
            'tools\ffmpeg.exe', 'tools\ffprobe.exe'
        )) {
            Assert-M4PathHasNoReparsePoint `
                -FullRoot $evidenceDir `
                -FullPath (Join-Path $stageDir $relative)
        }
    })

    $exportsOutput = Invoke-M4ExternalGate `
        -Name 'native_exports' `
        -Executable $dumpbinPath `
        -Arguments @('/nologo', '/exports', (Join-Path $stageDir 'mediacore.dll')) `
        -Display 'dumpbin /exports fresh-bin/mediacore.dll'
    $expectedExports = @(
        'mc_version',
        'mc_sha512_new',
        'mc_sha512_free',
        'mc_sha512_update',
        'mc_sha512_final',
        'mc_decode_gray',
        'mc_free_image',
        'mc_pdq256_from_gray',
        'mc_hamming_distance',
        'mc_image_phase1',
        'mc_phash_parts',
        'mc_sobel_hist',
        'mc_phase2_image',
        'mc_debug_crash',
        'mc_debug_sleep_ms'
    ) | Sort-Object
    $actualExports = @(
        foreach ($line in $exportsOutput -split "`r?`n") {
            if ($line -match '^\s+\d+\s+[0-9A-Fa-f]+\s+[0-9A-Fa-f]+\s+([A-Za-z_][A-Za-z0-9_]*)\s*$') {
                $Matches[1]
            }
        }
    )
    [void](Confirm-M4Gate -Name 'native_exports' -Action {
        Assert-M4ExactExports `
            -Expected $expectedExports `
            -Actual $actualExports
    })

    [void](Invoke-M4ValidationGate -Name 'binary_staging' -Action {
        $sourceMap = [ordered]@{
            'mediacore.dll' = Join-Path $repoRoot 'mediacore\build\Release\mediacore.dll'
            'ffmpeg.exe' = Join-Path $repoRoot 'third_party\ffmpeg\bin\ffmpeg.exe'
            'ffprobe.exe' = Join-Path $repoRoot 'third_party\ffmpeg\bin\ffprobe.exe'
        }
        $audit = [ordered]@{}
        foreach ($name in @('agent.exe', 'gui.exe', 'worker.exe', 'mediacore.dll')) {
            $path = Join-Path $stageDir $name
            if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
                throw "fresh stage is missing $name"
            }
            $audit[$name] = Get-M4SHA256 $path
        }
        foreach ($name in @('ffmpeg.exe', 'ffprobe.exe')) {
            $path = Join-Path $stageDir "tools\$name"
            if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
                throw "fresh stage is missing tools/$name"
            }
            $audit["tools/$name"] = Get-M4SHA256 $path
        }
        foreach ($name in $sourceMap.Keys) {
            $destination = if ($name -eq 'mediacore.dll') {
                Join-Path $stageDir $name
            } else {
                Join-Path $stageDir "tools\$name"
            }
            if ((Get-M4SHA256 $sourceMap[$name]) -cne
                (Get-M4SHA256 $destination)) {
                throw "fresh stage hash mismatch for $name"
            }
        }
        return $audit
    })

    $env:CGO_ENABLED = '0'
    Remove-Item Env:CC -ErrorAction SilentlyContinue
    [void](Invoke-M4GoGate -Name 'pure_go_full' -Arguments @(
        'test', '-count=1', './...'
    ))

    $env:CGO_ENABLED = '1'
    $env:CC = $gccPath
    [void](Invoke-M4GoGate -Name 'cgo_full' -Arguments @(
        'test', '-count=1', './...'
    ))
    [void](Invoke-M4GoGate -Name 'race_changed' -Arguments @(
        'test', '-race', '-count=1',
        './cmd/gui',
        './internal/agent',
        './internal/gui',
        './internal/phase2',
        './internal/worker',
        './internal/wproc'
    ))
    [void](Invoke-M4GoGate -Name 'vet' -Arguments @('vet', './...'))

    $env:DEDUP_TEST_PG_DSN = $PGDSN
    $env:FS_PG_DSN = $PGDSN
    $pgPattern = '^(' + (($postgresTests | ForEach-Object {
        [regex]::Escape($_)
    }) -join '|') + ')$'
    $pgOutput = Invoke-M4GoGate -Name 'postgres_contracts' -Arguments @(
        'test', '-v', '-count=1', '-run', $pgPattern,
        './internal/phase2', './internal/gui'
    )
    [void](Confirm-M4Gate -Name 'postgres_contracts' -Action {
        Assert-M4NamedPasses -Output $pgOutput -Names $postgresTests
    })

    $env:M4_VERIFY_RUN_ID = $runID
    $env:M4_EVIDENCE_PATH = $evidenceDir
    $env:M4_E2E_BIN_DIR = $stageDir
    Remove-Item Env:DEDUP_TEST_M4_BIN_DIR -ErrorAction SilentlyContinue
    $e2eOutput = Invoke-M4GoGate -Name 'm4_e2e' -Arguments @(
        'test', '-v', '-count=1',
        '-run', '^TestM4E2EWhenEnabled$',
        './integration'
    )
    $e2eNames = @(
        'TestM4E2EWhenEnabled/E1_AutomaticDispatchAndNativeFeatures',
        'TestM4E2EWhenEnabled/E2_SixFramesVerdictsAndGroupDetailAPI',
        'TestM4E2EWhenEnabled/E3_RestartCorruptTimeoutAndWorkerCrashSurvival',
        'TestM4E2EWhenEnabled/E4_IdempotentPublicUnchangedAndCleanup'
    )
    [void](Confirm-M4Gate -Name 'm4_e2e' -Action {
        Assert-M4NamedPasses -Output $e2eOutput -Names $e2eNames
        $markerMatches = [regex]::Matches(
            $e2eOutput,
            'M4_ACCEPTANCE\s+(\{[^\r\n]+\})'
        )
        if ($markerMatches.Count -ne 1) {
            throw "expected exactly one M4_ACCEPTANCE marker, got $($markerMatches.Count)"
        }
        try {
            $script:acceptanceMarker = (
                $markerMatches[0].Groups[1].Value | ConvertFrom-Json
            )
        }
        catch {
            throw 'M4 acceptance marker is not valid JSON.'
        }
        Assert-M4AcceptanceMarker `
            -Marker $script:acceptanceMarker `
            -ExpectedRunID $runID `
            -ExpectedSchema $script:acceptanceMarker.schema `
            -ExpectedEvidenceDir $evidenceDir
    })
    $acceptanceMarker = $script:acceptanceMarker

    $negativeOutput = Invoke-M4ExternalGate `
        -Name 'marker_negative' `
        -Executable $pwshPath `
        -Arguments @(
            '-NoLogo', '-NoProfile', '-File',
            (Join-Path $PSScriptRoot 'test_verify_m4_controller.ps1')
        ) `
        -Display 'scripts/test_verify_m4_controller.ps1'
    [void](Confirm-M4Gate -Name 'marker_negative' -Action {
        if ($negativeOutput -notmatch '(?m)^M4_HELPERS_NEGATIVE_PASS cases=8$') {
            throw 'M4 helper negative matrix did not report exactly 8 passing cases.'
        }
        if ($negativeOutput -notmatch '(?m)^M4_MARKER_NEGATIVE_PASS cases=380$') {
            throw 'marker negative matrix did not report exactly 380 passing cases.'
        }
        if ($negativeOutput -notmatch '(?m)^M4_BOOTSTRAP_SECRET LEAK=false$') {
            throw 'M4 bootstrap secret regression did not report LEAK=false.'
        }
        foreach ($category in @(
            'failure_artifact cases=3',
            'path_boundary cases=3',
            'secret_redaction cases=1'
        )) {
            if ($negativeOutput -notmatch (
                '(?m)^M4_BOOTSTRAP_NEGATIVE_CATEGORY ' +
                [regex]::Escape($category) +
                '$'
            )) {
                throw "M4 bootstrap category is missing: $category"
            }
        }
        if ($negativeOutput -notmatch '(?m)^M4_BOOTSTRAP_NEGATIVE_PASS cases=7$') {
            throw 'M4 bootstrap negative matrix did not report exactly 7 passing cases.'
        }
    })

    [void](Invoke-M4ValidationGate -Name 'schema_index_audit' -Action {
        $central = Get-Content -LiteralPath (
            Join-Path $repoRoot 'deploy\central.sql'
        ) -Raw
        $required = @(
            'CREATE TABLE IF NOT EXISTS pair_scores',
            'UNIQUE (kind, sha_a, sha_b)',
            'CREATE TABLE IF NOT EXISTS video_frames',
            'PRIMARY KEY (sha512, frame_idx)',
            'CREATE TABLE IF NOT EXISTS scan_tasks',
            'idx_dup_groups_kind',
            'idx_dup_members_file'
        )
        foreach ($needle in $required) {
            if (-not $central.Contains($needle)) {
                throw "central.sql schema/index contract is missing: $needle"
            }
        }
        return [ordered]@{
            central_sql_sha256 = Get-M4SHA256 (
                Join-Path $repoRoot 'deploy\central.sql'
            )
            postgres_16_named_tests = $postgresTests
            central_sql_runs = $acceptanceMarker.e4.central_sql_runs
        }
    })
    [void](Invoke-M4ValidationGate -Name 'public_unchanged' -Action {
        if ($acceptanceMarker.e4.public_unchanged -isnot [bool] -or
            -not $acceptanceMarker.e4.public_unchanged) {
            throw 'E4 did not prove the public schema unchanged.'
        }
        return [ordered]@{
            schema = $acceptanceMarker.schema
            public_unchanged = $true
        }
    })
    [void](Invoke-M4ValidationGate -Name 'cleanup_audit' -Action {
        Assert-M4NativeInteger `
            -Value $acceptanceMarker.e4.cleanup_residual `
            -Name 'marker.e4.cleanup_residual'
        if ([int64]$acceptanceMarker.e4.cleanup_residual -ne 0) {
            throw 'E4 cleanup residual is not the native integer zero.'
        }
        return [ordered]@{
            schema = $acceptanceMarker.schema
            cleanup_residual = 0
            user_media_modified = $acceptanceMarker.e4.user_media_modified
        }
    })
    [void](Invoke-M4ValidationGate -Name 'secret_scan' -Action {
        $findings = [System.Collections.Generic.List[string]]::new()
        $textFiles = @(
            Get-ChildItem -LiteralPath $evidenceDir -Recurse -File |
                Where-Object {
                    $_.Name -ne 'secret_scan.log' -and
                    $_.Extension -in @('.log', '.json', '.txt')
                }
        )
        foreach ($file in $textFiles) {
            Assert-M4PathHasNoReparsePoint `
                -FullRoot $evidenceDir `
                -FullPath $file.FullName
            $text = Get-Content -LiteralPath $file.FullName -Raw `
                -ErrorAction SilentlyContinue
            if ($null -eq $text) {
                continue
            }
            if ((-not [string]::IsNullOrEmpty($PGDSN) -and
                $text.Contains($PGDSN)) -or
                $text -match 'postgres(?:ql)?://[^/\s:@]+:[^@\s/]+@') {
                $findings.Add($file.FullName)
            }
        }
        if ($findings.Count -ne 0) {
            throw "credential-bearing PostgreSQL URI found in evidence: $($findings -join ',')"
        }
        return [ordered]@{
            files_scanned = $textFiles.Count
            findings = 0
        }
    })

    $strictGateResults = ConvertTo-M4StrictJSONObject -Value $gates
    Assert-M4RequiredGateEvidence `
        -GateResults $strictGateResults `
        -RequiredGates $requiredGates `
        -EvidenceDir $evidenceDir
}
catch {
    $failure = Protect-M4Secret $_.Exception.Message
}
finally {
    Set-Location -LiteralPath $locationBefore
    Restore-M4Environment -Snapshot $environmentBefore
}

foreach ($name in $requiredGates) {
    if (-not $gates.Contains($name)) {
        $gates[$name] = [ordered]@{
            status = 'NOT_RUN'
            exit_code = $null
            log = ''
        }
    }
}
$status = if ($null -eq $failure) { 'PASS' } else { 'FAIL' }
$summary = [ordered]@{
    schema_version = 1
    run_id = $runID
    timestamp = [DateTimeOffset]::Now.ToString('o')
    status = $status
    required_gates = $requiredGates
    gates = $gates
    tools = [ordered]@{
        go = $goPath
        gcc = $gccPath
        gofmt = $gofmtPath
        pwsh = $pwshPath
        dlltool = $dlltoolPath
        cmake = $cmakePath
        ctest = $ctestPath
        dumpbin = $dumpbinPath
        vcpkg_root = $vcpkgRootPath
    }
    build_out_dir = if ($null -ne $stageOutDir) { $stageOutDir } else { '' }
    commands = $commands
    acceptance = $acceptanceMarker
    failure = $failure
}
$summary | ConvertTo-Json -Depth 16 |
    Set-Content -LiteralPath $summaryPath -Encoding utf8

foreach ($name in $requiredGates) {
    $gate = $gates[$name]
    $exitText = if ($null -eq $gate.exit_code) { '-' } else { $gate.exit_code }
    $logText = if ([string]::IsNullOrEmpty($gate.log)) { '-' } else { $gate.log }
    Write-Host "M4 GATE $name $($gate.status) exit=$exitText log=$logText"
}
if ($null -ne $failure) {
    Write-Host (
        "M4 FINAL RESULT FAIL run_id=$runID evidence=$summaryPath reason=$failure"
    )
    exit 1
}
Write-Host "M4 FINAL RESULT PASS run_id=$runID evidence=$summaryPath"
exit 0
