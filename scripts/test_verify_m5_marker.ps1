[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$repoRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
. (Join-Path $PSScriptRoot 'verify_m5_marker.ps1')

$requiredGates = @(
    'format',
    'pure_go_full',
    'cgo_full',
    'race_changed',
    'vet',
    'helper_unit',
    'manifest_audit',
    'pipe_acl',
    'agent_forwarder',
    'postgres_contracts',
    'm5_e2e',
    'public_unchanged',
    'secret_scan',
    'marker_negative',
    'cleanup_audit'
)
$testRoot = [System.IO.Path]::GetFullPath((
    Join-Path $repoRoot (
        '.superpowers\tmp\m5-marker-selftest-' +
        [Guid]::NewGuid().ToString('N')
    )
))
$evidenceDir = Join-Path $testRoot 'evidence'
$detailPath = Join-Path $testRoot 'detail.md'
$planPath = Join-Path $testRoot 'plan.md'
$outsideLog = Join-Path $testRoot 'outside.log'

function Write-M5MarkerTestText {
    param(
        [Parameter(Mandatory)][string]$Path,
        [AllowEmptyString()][string]$Text
    )
    $parent = Split-Path -Parent $Path
    if (-not [System.IO.Directory]::Exists($parent)) {
        [System.IO.Directory]::CreateDirectory($parent) | Out-Null
    }
    [System.IO.File]::WriteAllText(
        $Path,
        $Text,
        [System.Text.UTF8Encoding]::new($false)
    )
}

function New-M5ValidEvidence {
    $gates = [ordered]@{}
    foreach ($name in $requiredGates) {
        $log = Join-Path $evidenceDir "$name.log"
        Write-M5MarkerTestText -Path $log -Text "gate=$name"
        $gates[$name] = [ordered]@{
            status = 'PASS'
            exit_code = 0
            command = "test gate $name"
            log = $log
            started_utc = '2026-07-29T00:00:00.0000000Z'
            ended_utc = '2026-07-29T00:00:01.0000000Z'
        }
    }

    $artifacts = [ordered]@{}
    foreach ($name in @('agent', 'gui', 'helper')) {
        $artifactPath = Join-Path $evidenceDir "$name.exe"
        Write-M5MarkerTestText -Path $artifactPath -Text "fresh-$name"
        $artifacts[$name] = [ordered]@{
            path = $artifactPath
            sha256 = (
                Get-FileHash -LiteralPath $artifactPath -Algorithm SHA256
            ).Hash.ToLowerInvariant()
            fresh = $true
        }
    }

    $tc = @(
        for ($index = 1; $index -le 12; $index++) {
            $id = 'TC-{0:D2}' -f $index
            $proof = Join-Path $evidenceDir "$id.json"
            Write-M5MarkerTestText -Path $proof -Text (
                [ordered]@{ id = $id; status = 'PASS' } |
                    ConvertTo-Json -Compress
            )
            [ordered]@{
                id = $id
                status = 'PASS'
                evidence = $proof
            }
        }
    )

    $reviewProof = Join-Path $evidenceDir 'reviews.json'
    Write-M5MarkerTestText -Path $reviewProof -Text (
        [ordered]@{
            critical_open = 0
            important_open = 0
            findings = @()
        } | ConvertTo-Json -Depth 4
    )

    return [ordered]@{
        schema_version = 1
        run_id = '20260729-120000-000-12345678'
        started_utc = '2026-07-29T00:00:00.0000000Z'
        ended_utc = '2026-07-29T00:01:00.0000000Z'
        status = 'PASS'
        git_status = 'NO_REPOSITORY'
        required_gates = $requiredGates
        gates = $gates
        tools = [ordered]@{
            go = [ordered]@{
                path = 'C:\tool\go.exe'
                version = 'go version go1.26.5 windows/amd64'
            }
            gcc = [ordered]@{
                path = 'C:\tool\gcc.exe'
                version = 'gcc 15.1.0'
            }
            gofmt = [ordered]@{
                path = 'C:\tool\gofmt.exe'
                version = 'go1.26.5'
            }
            pwsh = [ordered]@{
                path = 'C:\tool\pwsh.exe'
                version = '7.5.2'
            }
            windres = [ordered]@{
                path = 'C:\tool\windres.exe'
                version = 'windres 2.45'
            }
            mt = [ordered]@{
                path = 'C:\tool\mt.exe'
                version = '10.0.26100.0'
            }
            dlltool = [ordered]@{
                path = 'C:\tool\dlltool.exe'
                version = 'dlltool 2.45'
            }
            cmake = [ordered]@{
                path = 'C:\tool\cmake.exe'
                version = 'cmake 4.2.3'
            }
            ctest = [ordered]@{
                path = 'C:\tool\ctest.exe'
                version = 'ctest 4.2.3'
            }
            docker = [ordered]@{
                path = 'C:\tool\docker.exe'
                version = 'Docker 28.0.0'
            }
            subst = [ordered]@{
                path = 'C:\Windows\System32\subst.exe'
                version = '10.0'
            }
            vcpkg_root = [ordered]@{
                path = 'C:\vcpkg'
                version = 'toolchain-root'
            }
        }
        postgresql = [ordered]@{
            host = '127.0.0.1'
            database = 'dedup'
        }
        artifacts = $artifacts
        second_windows_status = 'VERIFIED_ON_SECOND_WINDOWS'
        second_windows = [ordered]@{
            configured_host = 'codex-192-168-1-6'
            reported_host = 'SECOND-WINDOWS-FIXTURE'
            status = 'VERIFIED_ON_SECOND_WINDOWS'
            evidence = (Join-Path $evidenceDir 'TC-10.json')
            sha256 = (
                Get-FileHash `
                    -LiteralPath (Join-Path $evidenceDir 'TC-10.json') `
                    -Algorithm SHA256
            ).Hash.ToLowerInvariant()
        }
        tc = $tc
        reviews = [ordered]@{
            critical_open = 0
            important_open = 0
            findings = @()
            sources = @($reviewProof)
        }
        protected_media_access_count = 0
        residue = [ordered]@{
            schema = 0
            process = 0
            pipe = 0
            subst = 0
            junction = 0
            handle = 0
            test_root = 0
        }
        failure = $null
    }
}

function Copy-M5MarkerValue {
    param([Parameter(Mandatory)][object]$Value)
    return (($Value | ConvertTo-Json -Depth 24) | ConvertFrom-Json)
}

function Assert-M5MarkerRejected {
    param(
        [Parameter(Mandatory)][string]$Name,
        [Parameter(Mandatory)][scriptblock]$MutateEvidence,
        [scriptblock]$MutateFiles
    )
    Write-M5MarkerTestText -Path $detailPath -Text "- [x] P1 complete`n"
    Write-M5MarkerTestText -Path $planPath -Text "- [x] Step complete`n"
    $evidence = Copy-M5MarkerValue -Value (New-M5ValidEvidence)
    & $MutateEvidence $evidence
    if ($null -ne $MutateFiles) {
        & $MutateFiles
    }
    Write-M5MarkerTestText `
        -Path (Join-Path $evidenceDir 'm5-evidence.json') `
        -Text ($evidence | ConvertTo-Json -Depth 24)
    $markerPath = Join-Path $evidenceDir "negative-$Name-complete.json"
    $rejected = $false
    try {
        Write-M5CompletionMarker `
            -Evidence $evidence `
            -EvidenceDir $evidenceDir `
            -WorkspaceRoot $repoRoot `
            -DetailPath $detailPath `
            -PlanPath $planPath `
            -MarkerPath $markerPath
    }
    catch {
        $rejected = $true
    }
    if (-not $rejected) {
        throw "negative M5 marker case unexpectedly accepted: $Name"
    }
    if (Test-Path -LiteralPath $markerPath) {
        throw "negative M5 marker case wrote a completion marker: $Name"
    }
}

[System.IO.Directory]::CreateDirectory($evidenceDir) | Out-Null
Write-M5MarkerTestText -Path $outsideLog -Text 'outside'

try {
    Write-M5MarkerTestText -Path $detailPath -Text "- [x] P1 complete`n"
    Write-M5MarkerTestText -Path $planPath -Text "- [x] Step complete`n"
    $valid = Copy-M5MarkerValue -Value (New-M5ValidEvidence)
    Write-M5MarkerTestText `
        -Path (Join-Path $evidenceDir 'm5-evidence.json') `
        -Text ($valid | ConvertTo-Json -Depth 24)
    $validMarker = Join-Path $evidenceDir 'valid-complete.json'
    Write-M5CompletionMarker `
        -Evidence $valid `
        -EvidenceDir $evidenceDir `
        -WorkspaceRoot $repoRoot `
        -DetailPath $detailPath `
        -PlanPath $planPath `
        -MarkerPath $validMarker
    if (-not (Test-Path -LiteralPath $validMarker -PathType Leaf)) {
        throw 'valid M5 marker did not create a completion marker'
    }
    $minorValid = Copy-M5MarkerValue -Value (New-M5ValidEvidence)
    $minorValid.reviews.findings = @(
        'Minor: deferred non-blocking review observation'
    )
    Write-M5MarkerTestText `
        -Path (Join-Path $evidenceDir 'm5-evidence.json') `
        -Text ($minorValid | ConvertTo-Json -Depth 24)
    $minorMarker = Join-Path $evidenceDir 'valid-minor-complete.json'
    Write-M5CompletionMarker `
        -Evidence $minorValid `
        -EvidenceDir $evidenceDir `
        -WorkspaceRoot $repoRoot `
        -DetailPath $detailPath `
        -PlanPath $planPath `
        -MarkerPath $minorMarker
    if (-not (Test-Path -LiteralPath $minorMarker -PathType Leaf)) {
        throw 'zero-C/I evidence with a Minor finding was rejected'
    }

    $pathOrderCases = 0
    $protectedRejected = $false
    try {
        ConvertTo-M5LexicalLocalPath `
            -Path (('I:' + '\tmp') + '\missing-tool.exe') `
            -Label 'external tool fixture' | Out-Null
    }
    catch {
        if ($_.Exception.Message -match 'intersects a protected media root') {
            $protectedRejected = $true
        }
    }
    if (-not $protectedRejected) {
        throw 'protected external path was not rejected at the lexical boundary'
    }
    $pathOrderCases++

    $outsideWorkspaceRejected = $false
    try {
        Get-M5WorkspacePath `
            -WorkspaceRoot $testRoot `
            -Path (Join-Path $repoRoot 'scripts\verify_m5.ps1') `
            -Label 'outside workspace fixture' | Out-Null
    }
    catch {
        if ($_.Exception.Message -match 'outside its allowed root') {
            $outsideWorkspaceRejected = $true
        }
    }
    if (-not $outsideWorkspaceRejected) {
        throw 'outside workspace path was accessed before lexical rejection'
    }
    $pathOrderCases++

    $cases = [ordered]@{}
    $cases['unchecked_detail'] = {
        param($evidence)
    }, {
        Write-M5MarkerTestText -Path $detailPath -Text "- [ ] P1 open`n"
    }
    $cases['unchecked_plan'] = {
        param($evidence)
    }, {
        Write-M5MarkerTestText -Path $planPath -Text "- [ ] Step open`n"
    }
    $cases['missing_tc'] = {
        param($evidence)
        $evidence.tc = @($evidence.tc | Select-Object -First 11)
    }, $null
    $cases['non_pass_gate'] = {
        param($evidence)
        $evidence.gates.format.status = 'FAIL'
        $evidence.gates.format.exit_code = [int64]1
    }, $null
    $cases['critical_open'] = {
        param($evidence)
        $evidence.reviews.critical_open = [int64]1
        $evidence.reviews.findings = @('critical finding')
    }, $null
    $cases['important_open'] = {
        param($evidence)
        $evidence.reviews.important_open = [int64]1
        $evidence.reviews.findings = @('important finding')
    }, $null
    $cases['protected_access'] = {
        param($evidence)
        $evidence.protected_media_access_count = [int64]1
    }, $null
    $cases['tc_failed'] = {
        param($evidence)
        $evidence.tc[4].status = 'FAILED'
    }, $null
    $cases['gate_log_outside'] = {
        param($evidence)
        $evidence.gates.vet.log = $outsideLog
    }, $null
    $cases['status_fail'] = {
        param($evidence)
        $evidence.status = 'FAIL'
        $evidence.failure = 'injected'
    }, $null

    foreach ($status in @(
        'USER_WAIVED',
        'PENDING_REMOTE_VALIDATION',
        'PASS',
        ''
    )) {
        $statusCopy = $status
        $caseName = if ($status -eq '') { 'empty' } else { $status }
        $cases["second_windows_$caseName"] = {
            param($evidence)
            $evidence.second_windows_status = $statusCopy
        }.GetNewClosure(), $null
    }
    foreach ($name in @(
        'schema',
        'process',
        'pipe',
        'subst',
        'junction',
        'handle',
        'test_root'
    )) {
        $nameCopy = $name
        $cases["residue_$name"] = {
            param($evidence)
            $evidence.residue.PSObject.Properties[$nameCopy].Value = [int64]1
        }.GetNewClosure(), $null
    }

    foreach ($entry in $cases.GetEnumerator()) {
        $mutators = @($entry.Value)
        Assert-M5MarkerRejected `
            -Name $entry.Key `
            -MutateEvidence $mutators[0] `
            -MutateFiles $mutators[1]
    }

    Write-Host "M5_MARKER_PATH_ORDER_PASS cases=$pathOrderCases"
    Write-Host "M5_MARKER_NEGATIVE_PASS cases=$($cases.Count)"
}
finally {
    $fullRoot = [System.IO.Path]::GetFullPath($testRoot)
    $allowedRoot = [System.IO.Path]::GetFullPath((
        Join-Path $repoRoot '.superpowers\tmp'
    )).TrimEnd('\') + '\'
    if ($fullRoot.StartsWith(
        $allowedRoot,
        [System.StringComparison]::OrdinalIgnoreCase
    ) -and (Split-Path -Leaf $fullRoot).StartsWith(
        'm5-marker-selftest-',
        [System.StringComparison]::Ordinal
    )) {
        Remove-Item -LiteralPath $fullRoot -Recurse -Force
    }
}
