[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$Go,

    [Parameter(Mandatory = $true)]
    [string]$PGDSN,

    [string]$GCC,

    [switch]$Scale,

    [switch]$RunScale
)

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'verify_m3_marker.ps1')
. (Join-Path $PSScriptRoot 'verify_m3_final_gate.ps1')
$runScaleMode = Resolve-M3ScaleMode -Scale:$Scale -RunScale:$RunScale
if ($runScaleMode) {
    . (Join-Path $PSScriptRoot 'verify_m3_scale_marker.ps1')
}
$repoRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$runID = '{0}-{1}' -f (
    [DateTimeOffset]::Now.ToString('yyyyMMdd-HHmmss-fff')
), ([Guid]::NewGuid().ToString('N').Substring(0, 8))
$evidenceDir = [System.IO.Path]::GetFullPath(
    (Join-Path $repoRoot ".superpowers\evidence\m3-$runID")
)
$evidencePrefix = $repoRoot.TrimEnd('\') + '\'
if (-not $evidenceDir.StartsWith(
    $evidencePrefix,
    [System.StringComparison]::OrdinalIgnoreCase
)) {
    throw 'M3 evidence directory must stay inside the workspace.'
}
[System.IO.Directory]::CreateDirectory($evidenceDir) | Out-Null

$gateResults = [ordered]@{}
$commands = [System.Collections.Generic.List[string]]::new()
$acceptanceMarker = $null
$scaleSeedMarker = $null
$scaleReuseMarker = $null
$scaleSchema = ''
$scaleNeedsCleanup = $false
$postgresContracts = $null
$schemaIndexInspection = $null
$cleanupAudit = $null
$negativeMatrices = [ordered]@{}
$gofmtPath = ''
$pwshPath = ''
$failure = $null
$requiredGates = [System.Collections.Generic.List[string]]::new()
foreach ($name in @(
    'format',
    'pure_go',
    'unit',
    'race',
    'vet',
    'postgres_contracts',
    'small_acceptance',
    'marker_task8',
    'marker_scale',
    'marker_cleanup',
    'schema_index_audit',
    'cleanup_audit'
)) {
    $requiredGates.Add($name)
}
if ($runScaleMode) {
    $requiredGates.Insert(10, 'scale_seed')
    $requiredGates.Insert(11, 'scale_reuse')
}

function Resolve-RequiredFile {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Value,
        [Parameter(Mandatory = $true)]
        [string]$Label
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

function Find-GCC {
    param([string]$Explicit)
    if (-not [string]::IsNullOrWhiteSpace($Explicit)) {
        return Resolve-RequiredFile -Value $Explicit -Label '-GCC'
    }

    $command = Get-Command gcc.exe -ErrorAction SilentlyContinue
    if ($null -ne $command -and
        (Test-Path -LiteralPath $command.Source -PathType Leaf)) {
        return [System.IO.Path]::GetFullPath($command.Source)
    }

    $candidates = [System.Collections.Generic.List[string]]::new()
    $known = Join-Path ([System.IO.Path]::GetTempPath()) 'winlibs-gcc\mingw64\bin\gcc.exe'
    $candidates.Add($known)
    foreach ($directory in @(
        Get-ChildItem -LiteralPath ([System.IO.Path]::GetTempPath()) `
            -Directory -Filter 'winlibs-gcc*' -ErrorAction SilentlyContinue |
            Sort-Object LastWriteTime -Descending
    )) {
        $candidates.Add((Join-Path $directory.FullName 'mingw64\bin\gcc.exe'))
    }
    foreach ($candidate in $candidates) {
        if (Test-Path -LiteralPath $candidate -PathType Leaf) {
            return [System.IO.Path]::GetFullPath($candidate)
        }
    }
    throw 'GCC was not supplied and could not be auto-discovered.'
}

function Protect-Secret {
    param([AllowEmptyString()][string]$Text)
    if ($null -eq $Text) {
        return ''
    }
    if (-not [string]::IsNullOrEmpty($PGDSN)) {
        return $Text.Replace($PGDSN, '[REDACTED_DSN]')
    }
    return $Text
}

function Assert-M3EvidenceFilePath {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path
    )
    $fullPath = [System.IO.Path]::GetFullPath($Path)
    $prefix = $evidenceDir.TrimEnd('\') + '\'
    if (-not $fullPath.StartsWith(
        $prefix,
        [System.StringComparison]::OrdinalIgnoreCase
    )) {
        throw 'gate log path must stay inside the M3 evidence directory.'
    }
    return $fullPath
}

function Invoke-M3ExternalGate {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Name,
        [Parameter(Mandatory = $true)]
        [string]$Executable,
        [Parameter(Mandatory = $true)]
        [string[]]$Arguments,
        [Parameter(Mandatory = $true)]
        [string]$Display
    )
    $logPath = Assert-M3EvidenceFilePath (
        Join-Path $evidenceDir "$Name.log"
    )
    $utf8NoBOM = [System.Text.UTF8Encoding]::new($false)
    [System.IO.File]::WriteAllText(
        $logPath,
        [string]::Empty,
        $utf8NoBOM
    )
    $commands.Add($display)
    Write-Host "M3 gate start: $Name"
    $rawLines = @(& $Executable @Arguments 2>&1)
    $exitCode = $LASTEXITCODE
    $safeLines = @($rawLines | ForEach-Object {
        Protect-Secret ([string]$_)
    })
    if ($safeLines.Count -gt 0) {
        [System.IO.File]::WriteAllLines(
            $logPath,
            [string[]]$safeLines,
            $utf8NoBOM
        )
    }
    if (-not (Test-Path -LiteralPath $logPath -PathType Leaf)) {
        throw "$Name gate log was not created."
    }
    [void](Assert-M3EvidenceFilePath -Path $logPath)
    foreach ($line in $safeLines) {
        Write-Host $line
    }
    $gateResults[$Name] = [ordered]@{
        status = if ($exitCode -eq 0) { 'PASS' } else { 'FAIL' }
        exit_code = $exitCode
        log = $logPath
    }
    if ($exitCode -ne 0) {
        throw "$Name failed with exit code $exitCode"
    }
    return ($safeLines -join "`n")
}

function Invoke-GoGate {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Name,
        [Parameter(Mandatory = $true)]
        [string[]]$Arguments
    )
    return Invoke-M3ExternalGate `
        -Name $Name `
        -Executable $script:goPath `
        -Arguments $Arguments `
        -Display ('go ' + ($Arguments -join ' '))
}

function Invoke-M3GateValidation {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Name,
        [Parameter(Mandatory = $true)]
        [scriptblock]$Action
    )
    try {
        return & $Action
    }
    catch {
        if ($gateResults.Contains($Name)) {
            $gateResults[$Name].status = 'FAIL'
        }
        throw
    }
}

function Write-M3AuditGate {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Name,
        [Parameter(Mandatory = $true)]
        [object]$Value
    )
    $logPath = Assert-M3EvidenceFilePath (
        Join-Path $evidenceDir "$Name.log"
    )
    try {
        $Value | ConvertTo-Json -Depth 12 |
            Set-Content -LiteralPath $logPath -Encoding utf8
        if (-not (Test-Path -LiteralPath $logPath -PathType Leaf)) {
            throw "$Name audit log was not created."
        }
        $gateResults[$Name] = [ordered]@{
            status = 'PASS'
            exit_code = 0
            log = $logPath
        }
        $commands.Add("internal audit $Name")
    }
    catch {
        $gateResults[$Name] = [ordered]@{
            status = 'FAIL'
            exit_code = 1
            log = $logPath
        }
        throw
    }
}

function Assert-AcceptanceOutput {
    param([Parameter(Mandatory = $true)][string]$Output)
    if (-not $Output.Contains('=== RUN   TestIntegrationSmallDB')) {
        throw 'PostgreSQL acceptance did not name TestIntegrationSmallDB.'
    }
    if ($Output.Contains('--- SKIP: TestIntegrationSmallDB')) {
        throw 'PostgreSQL acceptance was skipped.'
    }
    if (-not $Output.Contains('--- PASS: TestIntegrationSmallDB')) {
        throw 'PostgreSQL acceptance lacks the named PASS line.'
    }
    $matches = [regex]::Matches(
        $Output,
        'M3_SMALL_ACCEPTANCE\s+(\{[^\r\n]+\})'
    )
    if ($matches.Count -ne 1) {
        throw "expected exactly one M3_SMALL_ACCEPTANCE marker, got $($matches.Count)"
    }
    try {
        $marker = $matches[0].Groups[1].Value | ConvertFrom-Json
    }
    catch {
        throw 'M3_SMALL_ACCEPTANCE marker is not valid JSON.'
    }
    Assert-M3AcceptanceMarker -Marker $marker -ExpectedRunID $runID
    return $marker
}

function Assert-ScaleAcceptanceOutput {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Output,
        [Parameter(Mandatory = $true)]
        [bool]$ExpectedSeeded
    )
    if (-not $Output.Contains('=== RUN   TestAcceptanceM3')) {
        throw 'scale acceptance did not name TestAcceptanceM3.'
    }
    if ($Output.Contains('--- SKIP: TestAcceptanceM3')) {
        throw 'scale acceptance was skipped.'
    }
    if (-not $Output.Contains('--- PASS: TestAcceptanceM3')) {
        throw 'scale acceptance lacks the named PASS line.'
    }
    $matches = [regex]::Matches(
        $Output,
        'M3_SCALE_ACCEPTANCE\s+(\{[^\r\n]+\})'
    )
    if ($matches.Count -ne 1) {
        throw "expected exactly one M3_SCALE_ACCEPTANCE marker, got $($matches.Count)"
    }
    try {
        $marker = $matches[0].Groups[1].Value | ConvertFrom-Json
    }
    catch {
        throw 'M3_SCALE_ACCEPTANCE marker is not valid JSON.'
    }
    Assert-M3ScaleMarker `
        -Marker $marker `
        -ExpectedRunID $runID `
        -ExpectedSchema $scaleSchema `
        -ExpectedSeeded $ExpectedSeeded
    return $marker
}

function Get-M3ScaleSchema {
    $suffix = $runID.ToLowerInvariant() -replace '[^a-z0-9]', '_'
    $schema = "m3_scale_$suffix"
    if ($schema -notmatch '^m3_scale_[a-z0-9_]{8,96}$') {
        throw 'generated M3 scale schema is outside the safe naming contract.'
    }
    return $schema
}

function Invoke-M3ScaleCleanup {
    $env:FS_M3_SEED = '0'
    $env:FS_M3_SCHEMA = $scaleSchema
    $env:FS_M3_CLEANUP_ONLY = '1'
    try {
        $cleanupOutput = Invoke-GoGate -Name 'scale_cleanup' -Arguments @(
            'test',
            '-v',
            '-tags',
            'm3scale',
            '-count=1',
            '-run',
            '^TestAcceptanceM3$',
            '-timeout',
            '5m',
            './internal/firstscreen'
        )
        [void](Invoke-M3GateValidation -Name 'scale_cleanup' -Action {
            Assert-M3ScaleCleanupOutput `
                -Output $cleanupOutput `
                -ExpectedRunID $runID `
                -ExpectedSchema $scaleSchema
        })
    }
    finally {
        Remove-Item -LiteralPath 'Env:FS_M3_CLEANUP_ONLY' `
            -ErrorAction SilentlyContinue
    }
}

function Assert-AllM3GatesPassed {
    foreach ($name in $requiredGates) {
        if (-not $gateResults.Contains($name)) {
            throw "required gate result is missing: $name"
        }
        $gate = $gateResults[$name]
        if ($gate.status -isnot [string] -or $gate.status -cne 'PASS') {
            throw "required gate is not PASS: $name"
        }
        Assert-M3NativeInteger -Value $gate.exit_code -Name "gates.$name.exit_code"
        if ($gate.exit_code -ne 0) {
            throw "required gate exit code is non-zero: $name"
        }
        if ($gate.log -isnot [string] -or
            [string]::IsNullOrWhiteSpace($gate.log)) {
            throw "required gate log path is missing: $name"
        }
        $logPath = Assert-M3EvidenceFilePath -Path $gate.log
        if (-not (Test-Path -LiteralPath $logPath -PathType Leaf)) {
            throw "required gate log does not exist: $name"
        }
    }
}

function Restore-ProcessEnvironment {
    param([hashtable]$Snapshot)
    foreach ($name in $Snapshot.Keys) {
        $entry = $Snapshot[$name]
        $path = "Env:$name"
        if (-not $entry.Exists) {
            Remove-Item -LiteralPath $path -ErrorAction SilentlyContinue
        }
        else {
            Set-Item -LiteralPath $path -Value ([string]$entry.Value)
        }
    }
}

if ([string]::IsNullOrWhiteSpace($PGDSN)) {
    $failure = '-PGDSN must be explicit and non-empty.'
}

$environmentNames = @(
    'PATH',
    'CGO_ENABLED',
    'FS_PG_DSN',
    'DEDUP_TEST_PG_DSN',
    'M3_VERIFY_RUN_ID',
    'M3_EVIDENCE_PATH',
    'FS_M3_SEED',
    'FS_M3_SCHEMA',
    'FS_M3_CLEANUP_ONLY'
)
$environmentBefore = @{}
foreach ($name in $environmentNames) {
    $item = Get-Item -LiteralPath "Env:$name" -ErrorAction SilentlyContinue
    $environmentBefore[$name] = [pscustomobject]@{
        Exists = $null -ne $item
        Value = if ($null -ne $item) { [string]$item.Value } else { $null }
    }
}
$locationBefore = Get-Location

try {
    if ($null -ne $failure) {
        throw $failure
    }
    $script:goPath = Resolve-RequiredFile -Value $Go -Label '-Go'
    $gccPath = Find-GCC -Explicit $GCC
    $acceptancePath = Join-Path $repoRoot 'internal\firstscreen\small_acceptance_test.go'
    $centralPath = Join-Path $repoRoot 'deploy\central.sql'
    if (-not (Test-Path -LiteralPath $acceptancePath -PathType Leaf)) {
        throw 'small_acceptance_test.go is missing.'
    }
    $acceptanceSource = Get-Content -Raw -LiteralPath $acceptancePath
    if ($acceptanceSource -notmatch 'func\s+TestIntegrationSmallDB\s*\(') {
        throw 'TestIntegrationSmallDB is missing.'
    }
    if (-not (Test-Path -LiteralPath $centralPath -PathType Leaf)) {
        throw 'deploy/central.sql is missing.'
    }
    if ($runScaleMode) {
        $scalePath = Join-Path $repoRoot 'internal\firstscreen\scale_acceptance_test.go'
        if (-not (Test-Path -LiteralPath $scalePath -PathType Leaf)) {
            throw 'scale_acceptance_test.go is missing.'
        }
        $scaleSource = Get-Content -Raw -LiteralPath $scalePath
        if ($scaleSource -notmatch 'func\s+TestAcceptanceM3\s*\(') {
            throw 'TestAcceptanceM3 is missing.'
        }
        $scaleSchema = Get-M3ScaleSchema
    }
    $gofmtPath = Resolve-RequiredFile `
        -Value (Join-Path (Split-Path -Parent $script:goPath) 'gofmt.exe') `
        -Label 'gofmt'
    $pwshPath = Resolve-RequiredFile `
        -Value (Join-Path $PSHOME 'pwsh.exe') `
        -Label 'pwsh'

    $env:CGO_ENABLED = '1'
    $env:FS_PG_DSN = $PGDSN
    $env:DEDUP_TEST_PG_DSN = $PGDSN
    $env:M3_VERIFY_RUN_ID = $runID
    $env:M3_EVIDENCE_PATH = $evidenceDir
    $env:PATH = (
        (Join-Path $repoRoot 'bin'),
        (Join-Path $repoRoot 'bin\tools'),
        (Split-Path -Parent $gccPath),
        (Split-Path -Parent $script:goPath),
        $environmentBefore['PATH'].Value
    ) -join ';'
    Set-Location -LiteralPath $repoRoot

    $ownedGoFiles = @(Get-M3RepoOwnedGoFiles -RepoRoot $repoRoot)
    $formatOutput = Invoke-M3ExternalGate `
        -Name 'format' `
        -Executable $gofmtPath `
        -Arguments (@('-l') + @($ownedGoFiles)) `
        -Display "gofmt -l <repo-owned Go files count=$($ownedGoFiles.Count)>"
    [void](Invoke-M3GateValidation -Name 'format' -Action {
        Assert-M3FormatOutput -Output $formatOutput
    })

    try {
        $env:CGO_ENABLED = '0'
        [void](Invoke-GoGate -Name 'pure_go' -Arguments @(
            'test',
            '-count=1',
            '-skip',
            '^Test(IntegrationSmallDB|PG(Keyset|ReplaceResults).*|AcceptanceM3)$',
            './...'
        ))
    }
    finally {
        $env:CGO_ENABLED = '1'
    }

    [void](Invoke-GoGate -Name 'unit' -Arguments @(
        'test',
        '-count=1',
        '-skip',
        '(?i)IntegrationSmallDB|Million|Task9',
        './...'
    ))
    [void](Invoke-GoGate -Name 'race' -Arguments @(
        'test',
        '-race',
        '-count=1',
        '-skip',
        '(?i)IntegrationSmallDB|Million|Task9',
        './internal/firstscreen',
        './internal/gui',
        './cmd/gui'
    ))
    [void](Invoke-GoGate -Name 'vet' -Arguments @(
        'vet',
        './...'
    ))

    $expectedPGTests = @(
        'TestPGKeysetNullableFirstPageIncludesEmptyKeysAndTerminates',
        'TestPGKeysetReadersPageThreeFilteringOrderingAndBadRows',
        'TestPGKeysetContextCancellationQueryAndCallbackErrors',
        'TestPGKeysetSchemaTwiceIndexesAndExplainEligibility',
        'TestPGReplaceResultsSuccessIdempotencyAndM4Preservation',
        'TestPGReplaceResultsRemoteFailuresRollbackAndKeepConnectionUsable',
        'TestPGReplaceResultsAbortedCommitIsDefiniteRollback',
        'TestPGReplaceResultsLostCommitAckIsUnknownAndRetryConverges',
        'TestPGReplaceResultsClosedConnectionDoesNotChangeResults',
        'TestPGReplaceResultsConcurrentM4InsertAfterDeleteIsPreserved'
    )
    $pgContractOutput = Invoke-GoGate `
        -Name 'postgres_contracts' `
        -Arguments @(
            'test',
            '-v',
            '-count=1',
            '-run',
            '^TestPG(Keyset|ReplaceResults)',
            './internal/firstscreen'
        )
    $postgresContracts = Invoke-M3GateValidation `
        -Name 'postgres_contracts' `
        -Action {
            Assert-M3PGContractOutput `
                -Output $pgContractOutput `
                -ExpectedTests $expectedPGTests
        }

    $pgOutput = Invoke-GoGate -Name 'small_acceptance' -Arguments @(
        'test',
        '-v',
        '-count=1',
        '-run',
        '^TestIntegrationSmallDB$',
        './internal/firstscreen'
    )
    $acceptanceMarker = Invoke-M3GateValidation `
        -Name 'small_acceptance' `
        -Action {
            Assert-AcceptanceOutput -Output $pgOutput
        }

    foreach ($matrix in @(
        [ordered]@{
            gate = 'marker_task8'
            script = 'test_verify_m3_marker.ps1'
            marker = 'M3_MARKER_NEGATIVE_PASS'
            cases = 12
        },
        [ordered]@{
            gate = 'marker_scale'
            script = 'test_verify_m3_scale_marker.ps1'
            marker = 'M3_SCALE_MARKER_NEGATIVE_PASS'
            cases = 23
        },
        [ordered]@{
            gate = 'marker_cleanup'
            script = 'test_verify_m3_scale_cleanup_marker.ps1'
            marker = 'M3_SCALE_CLEANUP_MARKER_NEGATIVE_PASS'
            cases = 7
        }
    )) {
        $matrixScript = Join-Path $PSScriptRoot $matrix.script
        $matrixOutput = Invoke-M3ExternalGate `
            -Name $matrix.gate `
            -Executable $pwshPath `
            -Arguments @('-NoLogo', '-NoProfile', '-File', $matrixScript) `
            -Display "pwsh -NoProfile -File scripts/$($matrix.script)"
        [void](Invoke-M3GateValidation -Name $matrix.gate -Action {
            Assert-M3MatrixOutput `
                -Output $matrixOutput `
                -Marker $matrix.marker `
                -ExpectedCases $matrix.cases
        })
        $negativeMatrices[$matrix.gate] = [ordered]@{
            marker = $matrix.marker
            cases = $matrix.cases
        }
    }

    if ($runScaleMode) {
        $env:FS_M3_SCHEMA = $scaleSchema
        $env:FS_M3_SEED = '1'
        Remove-Item -LiteralPath 'Env:FS_M3_CLEANUP_ONLY' `
            -ErrorAction SilentlyContinue
        $scaleNeedsCleanup = $true
        $scaleSeedOutput = Invoke-GoGate -Name 'scale_seed' -Arguments @(
            'test',
            '-v',
            '-tags',
            'm3scale',
            '-count=1',
            '-run',
            '^TestAcceptanceM3$',
            '-timeout',
            '30m',
            './internal/firstscreen'
        )
        $scaleSeedMarker = Invoke-M3GateValidation `
            -Name 'scale_seed' `
            -Action {
                Assert-ScaleAcceptanceOutput `
                    -Output $scaleSeedOutput `
                    -ExpectedSeeded $true
            }

        $env:FS_M3_SEED = '0'
        $scaleReuseOutput = Invoke-GoGate -Name 'scale_reuse' -Arguments @(
            'test',
            '-v',
            '-tags',
            'm3scale',
            '-count=1',
            '-run',
            '^TestAcceptanceM3$',
            '-timeout',
            '30m',
            './internal/firstscreen'
        )
        $scaleReuseMarker = Invoke-M3GateValidation `
            -Name 'scale_reuse' `
            -Action {
                Assert-ScaleAcceptanceOutput `
                    -Output $scaleReuseOutput `
                    -ExpectedSeeded $false
            }
        $scaleNeedsCleanup = $false
    }

    $schemaIndexInspection = [ordered]@{
        contract_test = 'TestPGKeysetSchemaTwiceIndexesAndExplainEligibility'
        central_sql_runs = 2
        exact_indexes = @($postgresContracts.expected_indexes)
        actual_explain = [bool]$postgresContracts.actual_explain
        public_unchanged = $true
    }
    if (-not $postgresContracts.schema_twice -or
        -not $postgresContracts.exact_indexes -or
        -not $postgresContracts.actual_explain) {
        throw 'PostgreSQL schema/index inspection contract is incomplete.'
    }
    Write-M3AuditGate `
        -Name 'schema_index_audit' `
        -Value $schemaIndexInspection

    $cleanupAudit = [ordered]@{
        small = [ordered]@{
            cleanup_residual = $acceptanceMarker.cleanup_residual
        }
        postgres_contracts = @($postgresContracts.cleanup)
        scale = if ($runScaleMode) {
            [ordered]@{
                enabled = $true
                schema = $scaleSchema
                cleanup_performed = $scaleReuseMarker.cleanup_performed
                cleanup_residual = $scaleReuseMarker.cleanup_residual
                public_unchanged = $scaleReuseMarker.public_unchanged
            }
        }
        else {
            [ordered]@{
                enabled = $false
            }
        }
    }
    if ($acceptanceMarker.cleanup_residual -ne 0) {
        throw 'small acceptance cleanup residual is non-zero.'
    }
    foreach ($cleanup in $postgresContracts.cleanup) {
        if ($cleanup.residual -ne 0) {
            throw "PostgreSQL contract cleanup residual is non-zero: $($cleanup.schema)"
        }
    }
    if ($runScaleMode -and
        (-not $scaleReuseMarker.cleanup_performed -or
            $scaleReuseMarker.cleanup_residual -ne 0 -or
            -not $scaleReuseMarker.public_unchanged)) {
        throw 'scale reuse cleanup/public-window audit failed.'
    }
    Write-M3AuditGate -Name 'cleanup_audit' -Value $cleanupAudit

    Assert-M3RequiredGateEvidence `
        -GateResults $gateResults `
        -RequiredGates @($requiredGates) `
        -EvidenceDir $evidenceDir
}
catch {
    $primaryFailure = Protect-Secret $_.Exception.Message
    if ($runScaleMode -and $scaleNeedsCleanup -and
        -not [string]::IsNullOrWhiteSpace($scaleSchema)) {
        if (-not $requiredGates.Contains('scale_cleanup')) {
            $requiredGates.Add('scale_cleanup')
        }
        try {
            Invoke-M3ScaleCleanup
            $scaleNeedsCleanup = $false
        }
        catch {
            $cleanupFailure = Protect-Secret $_.Exception.Message
            $primaryFailure = Join-M3ScaleFailures `
                -Primary $primaryFailure `
                -Cleanup $cleanupFailure
        }
    }
    $failure = $primaryFailure
}
finally {
    Set-Location -LiteralPath $locationBefore
    Restore-ProcessEnvironment -Snapshot $environmentBefore
}

Add-M3NotRunGates `
    -GateResults $gateResults `
    -RequiredGates @($requiredGates)

$summary = [ordered]@{
    schema_version = 2
    run_id = $runID
    timestamp = [DateTimeOffset]::Now.ToString('o')
    status = if ($null -eq $failure) { 'PASS' } else { 'FAIL' }
    tools = [ordered]@{
        go = if ($null -ne $script:goPath) { $script:goPath } else { '' }
        gcc = if ($null -ne $gccPath) { $gccPath } else { '' }
        gofmt = $gofmtPath
        pwsh = $pwshPath
    }
    commands = $commands
    required_gates = @($requiredGates)
    gates = $gateResults
    acceptance = $acceptanceMarker
    postgres_contracts = $postgresContracts
    schema_index_inspection = $schemaIndexInspection
    cleanup_audit = $cleanupAudit
    negative_matrices = $negativeMatrices
    scale = [ordered]@{
        enabled = $runScaleMode
        schema = $scaleSchema
        seed = $scaleSeedMarker
        reuse = $scaleReuseMarker
    }
    failure = $failure
}
$summaryPath = Join-Path $evidenceDir 'm3-evidence.json'
$summary | ConvertTo-Json -Depth 12 |
    Set-Content -LiteralPath $summaryPath -Encoding utf8

foreach ($line in @(
    Get-M3GateSummaryLines `
        -GateResults $gateResults `
        -RequiredGates @($requiredGates)
)) {
    Write-Host $line
}

if ($null -ne $failure) {
    throw "M3 VERIFY FAIL run_id=$runID evidence=$summaryPath reason=$failure"
}
Write-Host "M3 VERIFY PASS run_id=$runID evidence=$summaryPath"
