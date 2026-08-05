[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'verify_m4_marker.ps1')
. (Join-Path $PSScriptRoot 'test_verify_m4_helpers.ps1')

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
$testRoot = Join-Path (
    [System.IO.Path]::GetTempPath()
) ('m4-marker-negative-' + [Guid]::NewGuid().ToString('N'))
$evidenceDir = Join-Path $testRoot 'evidence'
[System.IO.Directory]::CreateDirectory($evidenceDir) | Out-Null
$outsideLog = Join-Path $testRoot 'outside.log'
[System.IO.File]::WriteAllText($outsideLog, 'outside')

function New-ValidMarker {
    return [pscustomobject]@{
        schema_version = [int64]1
        run_id = 'marker-test-run'
        schema = 'm4_e2e_marker_test'
        evidence_path = $evidenceDir
        topology = 'SINGLE_WINDOWS_TWO_LOCAL_AGENT_IDENTITIES'
        second_windows_status = 'USER_WAIVED'
        e1 = [pscustomobject]@{
            passed = $true
            agent_identities = [object[]]@(
                'm4-local-agent-a-12345678',
                'm4-local-agent-b-12345678'
            )
            automatic_dispatch = $true
            actual_native_features = $true
            phash_blob_bytes = [int64]76
            sobel_blob_bytes = [int64]516
        }
        e2 = [pscustomobject]@{
            passed = $true
            video_frames_a = [int64]6
            video_frames_b = [int64]6
            image_verdict = 'yes'
            video_verdict = 'yes'
            group_detail_api = $true
        }
        e3 = [pscustomobject]@{
            passed = $true
            gui_restart_recovery = $true
            corrupt_survived = $true
            timeout_survived = $true
            worker_crash_survived = $true
            remaining_samples_completed = $true
        }
        e4 = [pscustomobject]@{
            passed = $true
            idempotent_rerun = $true
            public_unchanged = $true
            cleanup_residual = [int64]0
            central_sql_runs = [int64]2
            user_media_modified = $false
        }
    }
}

function New-ValidGates {
    $results = [pscustomobject]@{}
    foreach ($name in $requiredGates) {
        $log = Join-Path $evidenceDir "$name.log"
        [System.IO.File]::WriteAllText($log, "gate=$name")
        $results | Add-Member -NotePropertyName $name -NotePropertyValue (
            [pscustomobject]@{
                status = 'PASS'
                exit_code = [int64]0
                log = $log
            }
        )
    }
    return $results
}

function Copy-JSON {
    param([Parameter(Mandatory = $true)][object]$Value)
    return (($Value | ConvertTo-Json -Depth 12) | ConvertFrom-Json)
}

function Get-TestObject {
    param(
        [Parameter(Mandatory = $true)][object]$Root,
        [Parameter(Mandatory = $true)][string[]]$Parts
    )
    $current = $Root
    for ($index = 0; $index -lt $Parts.Count - 1; $index++) {
        $current = $current.PSObject.Properties[$Parts[$index]].Value
    }
    return $current
}

function Set-TestProperty {
    param(
        [Parameter(Mandatory = $true)][object]$Root,
        [Parameter(Mandatory = $true)][string]$Path,
        [AllowNull()][object]$Value
    )
    $parts = @($Path -split '\.')
    $owner = Get-TestObject -Root $Root -Parts $parts
    $owner.PSObject.Properties[$parts[-1]].Value = $Value
}

function Remove-TestProperty {
    param(
        [Parameter(Mandatory = $true)][object]$Root,
        [Parameter(Mandatory = $true)][string]$Path
    )
    $parts = @($Path -split '\.')
    $owner = Get-TestObject -Root $Root -Parts $parts
    $owner.PSObject.Properties.Remove($parts[-1])
}

function Assert-NegativeRejected {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][scriptblock]$Mutate
    )
    $marker = Copy-JSON -Value (New-ValidMarker)
    $gates = Copy-JSON -Value (New-ValidGates)
    $required = @($requiredGates)
    & $Mutate $marker $gates ([ref]$required)
    $rejected = $false
    try {
        Assert-M4AcceptanceMarker `
            -Marker $marker `
            -ExpectedRunID 'marker-test-run' `
            -ExpectedSchema 'm4_e2e_marker_test' `
            -ExpectedEvidenceDir $evidenceDir
        Assert-M4RequiredGateEvidence `
            -GateResults $gates `
            -RequiredGates $required `
            -EvidenceDir $evidenceDir
    }
    catch {
        $rejected = $true
    }
    if (-not $rejected) {
        throw "negative M4 marker/gate case unexpectedly accepted: $Name"
    }
}

$cases = [ordered]@{}
$leafSpecs = @(
    @('schema_version', '1', [int64]2),
    @('run_id', [int64]1, 'other-run'),
    @('schema', [int64]1, 'unsafe-schema'),
    @('evidence_path', [int64]1, $outsideLog),
    @('topology', [int64]1, 'TWO_WINDOWS'),
    @('second_windows_status', [int64]1, 'PASS'),
    @('e1.passed', 'true', $false),
    @('e1.agent_identities', 'agents', [object[]]@('m4-local-agent-a-12345678')),
    @('e1.automatic_dispatch', 'true', $false),
    @('e1.actual_native_features', 'true', $false),
    @('e1.phash_blob_bytes', '76', [int64]75),
    @('e1.sobel_blob_bytes', '516', [int64]515),
    @('e2.passed', 'true', $false),
    @('e2.video_frames_a', '6', [int64]5),
    @('e2.video_frames_b', '6', [int64]5),
    @('e2.image_verdict', [int64]1, 'no'),
    @('e2.video_verdict', [int64]1, 'inconclusive'),
    @('e2.group_detail_api', 'true', $false),
    @('e3.passed', 'true', $false),
    @('e3.gui_restart_recovery', 'true', $false),
    @('e3.corrupt_survived', 'true', $false),
    @('e3.timeout_survived', 'true', $false),
    @('e3.worker_crash_survived', 'true', $false),
    @('e3.remaining_samples_completed', 'true', $false),
    @('e4.passed', 'true', $false),
    @('e4.idempotent_rerun', 'true', $false),
    @('e4.public_unchanged', 'true', $false),
    @('e4.cleanup_residual', '0', [int64]1),
    @('e4.central_sql_runs', '2', [int64]1),
    @('e4.user_media_modified', 'false', $true)
)
foreach ($spec in $leafSpecs) {
    $path = [string]$spec[0]
    $wrongType = $spec[1]
    $wrongValue = $spec[2]
    $caseName = $path -replace '\.', '_'
    $cases["marker_missing_$caseName"] = {
        param($marker, $gates, $required)
        Remove-TestProperty -Root $marker -Path $path
    }.GetNewClosure()
    $cases["marker_type_$caseName"] = {
        param($marker, $gates, $required)
        Set-TestProperty -Root $marker -Path $path -Value $wrongType
    }.GetNewClosure()
    $cases["marker_value_$caseName"] = {
        param($marker, $gates, $required)
        Set-TestProperty -Root $marker -Path $path -Value $wrongValue
    }.GetNewClosure()
    $cases["marker_null_$caseName"] = {
        param($marker, $gates, $required)
        Set-TestProperty -Root $marker -Path $path -Value $null
    }.GetNewClosure()
}
foreach ($objectName in @('e1', 'e2', 'e3', 'e4')) {
    $nameCopy = $objectName
    $cases["marker_missing_$objectName"] = {
        param($marker, $gates, $required)
        Remove-TestProperty -Root $marker -Path $nameCopy
    }.GetNewClosure()
    $cases["marker_null_$objectName"] = {
        param($marker, $gates, $required)
        Set-TestProperty -Root $marker -Path $nameCopy -Value $null
    }.GetNewClosure()
    $cases["marker_type_$objectName"] = {
        param($marker, $gates, $required)
        Set-TestProperty -Root $marker -Path $nameCopy -Value 'not-an-object'
    }.GetNewClosure()
}
$cases['marker_duplicate_agent_identity'] = {
    param($marker, $gates, $required)
    $marker.e1.agent_identities[1] = $marker.e1.agent_identities[0]
}
$cases['marker_extra_top_property'] = {
    param($marker, $gates, $required)
    $marker | Add-Member -NotePropertyName unexpected -NotePropertyValue $true
}
$cases['marker_extra_nested_property'] = {
    param($marker, $gates, $required)
    $marker.e2 | Add-Member -NotePropertyName unexpected -NotePropertyValue $true
}

foreach ($gateName in $requiredGates) {
    $nameCopy = $gateName
    $cases["gate_missing_$gateName"] = {
        param($marker, $gates, $required)
        $gates.PSObject.Properties.Remove($nameCopy)
    }.GetNewClosure()
    $cases["gate_null_$gateName"] = {
        param($marker, $gates, $required)
        $gates.PSObject.Properties[$nameCopy].Value = $null
    }.GetNewClosure()
    foreach ($field in @('status', 'exit_code', 'log')) {
        $fieldCopy = $field
        $cases["gate_${field}_missing_$gateName"] = {
            param($marker, $gates, $required)
            $gates.PSObject.Properties[$nameCopy].Value.PSObject.Properties.Remove(
                $fieldCopy
            )
        }.GetNewClosure()
        $cases["gate_${field}_null_$gateName"] = {
            param($marker, $gates, $required)
            $gates.PSObject.Properties[$nameCopy].Value.
                PSObject.Properties[$fieldCopy].Value = $null
        }.GetNewClosure()
    }
    $cases["gate_status_value_$gateName"] = {
        param($marker, $gates, $required)
        $gates.PSObject.Properties[$nameCopy].Value.status = 'FAIL'
    }.GetNewClosure()
    $cases["gate_status_type_$gateName"] = {
        param($marker, $gates, $required)
        $gates.PSObject.Properties[$nameCopy].Value.status = [int64]1
    }.GetNewClosure()
    $cases["gate_exit_value_$gateName"] = {
        param($marker, $gates, $required)
        $gates.PSObject.Properties[$nameCopy].Value.exit_code = [int64]1
    }.GetNewClosure()
    $cases["gate_exit_type_$gateName"] = {
        param($marker, $gates, $required)
        $gates.PSObject.Properties[$nameCopy].Value.exit_code = '0'
    }.GetNewClosure()
    $cases["gate_log_empty_$gateName"] = {
        param($marker, $gates, $required)
        $gates.PSObject.Properties[$nameCopy].Value.log = ''
    }.GetNewClosure()
    $cases["gate_log_type_$gateName"] = {
        param($marker, $gates, $required)
        $gates.PSObject.Properties[$nameCopy].Value.log = [int64]1
    }.GetNewClosure()
    $cases["gate_log_outside_$gateName"] = {
        param($marker, $gates, $required)
        $gates.PSObject.Properties[$nameCopy].Value.log = $outsideLog
    }.GetNewClosure()
    $cases["gate_log_missing_$gateName"] = {
        param($marker, $gates, $required)
        $gates.PSObject.Properties[$nameCopy].Value.log =
            (Join-Path $evidenceDir "$nameCopy-missing.log")
    }.GetNewClosure()
    $cases["gate_extra_field_$gateName"] = {
        param($marker, $gates, $required)
        $gates.PSObject.Properties[$nameCopy].Value |
            Add-Member -NotePropertyName unexpected -NotePropertyValue $true
    }.GetNewClosure()
}
$cases['gate_extra_result'] = {
    param($marker, $gates, $required)
    $gates | Add-Member -NotePropertyName unexpected -NotePropertyValue (
        $gates.format
    )
}
$cases['gate_required_duplicate'] = {
    param($marker, $gates, $required)
    $required.Value = @($required.Value) + @($required.Value[0])
}
$cases['gate_required_empty'] = {
    param($marker, $gates, $required)
    $required.Value = @($required.Value)
    $required.Value[0] = ''
}

try {
    $validMarker = New-ValidMarker
    $validGates = New-ValidGates
    Assert-M4AcceptanceMarker `
        -Marker $validMarker `
        -ExpectedRunID 'marker-test-run' `
        -ExpectedSchema 'm4_e2e_marker_test' `
        -ExpectedEvidenceDir $evidenceDir
    Assert-M4RequiredGateEvidence `
        -GateResults $validGates `
        -RequiredGates $requiredGates `
        -EvidenceDir $evidenceDir
    foreach ($entry in $cases.GetEnumerator()) {
        Assert-NegativeRejected -Name $entry.Key -Mutate $entry.Value
    }

    $pathCases = 0
    $junctionTarget = Join-Path $testRoot 'junction-evidence-target'
    [System.IO.Directory]::CreateDirectory($junctionTarget) | Out-Null
    $junctionEvidence = Join-Path $testRoot 'junction-evidence'
    New-Item -ItemType Junction -Path $junctionEvidence `
        -Target $junctionTarget -ErrorAction Stop | Out-Null
    $junctionMarker = New-ValidMarker
    $junctionMarker.evidence_path = $junctionEvidence
    $rejected = $false
    try {
        Assert-M4AcceptanceMarker `
            -Marker $junctionMarker `
            -ExpectedRunID 'marker-test-run' `
            -ExpectedSchema 'm4_e2e_marker_test' `
            -ExpectedEvidenceDir $junctionEvidence
    }
    catch {
        $rejected = $true
    }
    if (-not $rejected) {
        throw 'ancestor reparse point evidence directory was accepted'
    }
    $pathCases++
    Remove-Item -LiteralPath $junctionEvidence -Force

    $logTarget = Join-Path $testRoot 'junction-log-target'
    [System.IO.Directory]::CreateDirectory($logTarget) | Out-Null
    $nestedLink = Join-Path $evidenceDir 'nested-link'
    New-Item -ItemType Junction -Path $nestedLink `
        -Target $logTarget -ErrorAction Stop | Out-Null
    $linkedLog = Join-Path $nestedLink 'format.log'
    [System.IO.File]::WriteAllText($linkedLog, 'linked')
    $linkedGates = New-ValidGates
    $linkedGates.format.log = $linkedLog
    $rejected = $false
    try {
        Assert-M4RequiredGateEvidence `
            -GateResults $linkedGates `
            -RequiredGates $requiredGates `
            -EvidenceDir $evidenceDir
    }
    catch {
        $rejected = $true
    }
    if (-not $rejected) {
        throw 'ancestor reparse point gate log was accepted'
    }
    $pathCases++
    Remove-Item -LiteralPath $nestedLink -Force

    $schemaCaseCount = @($cases.Keys | Where-Object {
        $_.StartsWith('marker_', [System.StringComparison]::Ordinal)
    }).Count
    $gateCaseCount = $cases.Count - $schemaCaseCount
    Write-Host (
        "M4_MARKER_NEGATIVE_CATEGORY schema_and_values cases=$schemaCaseCount"
    )
    Write-Host "M4_MARKER_NEGATIVE_CATEGORY gates cases=$gateCaseCount"
    Write-Host "M4_MARKER_NEGATIVE_CATEGORY ancestor_reparse cases=$pathCases"
    Write-Host "M4_MARKER_NEGATIVE_PASS cases=$($cases.Count + $pathCases)"
}
finally {
    $fullTestRoot = [System.IO.Path]::GetFullPath($testRoot)
    $tempPrefix = [System.IO.Path]::GetFullPath(
        [System.IO.Path]::GetTempPath()
    ).TrimEnd('\') + '\'
    if ($fullTestRoot.StartsWith(
        $tempPrefix,
        [System.StringComparison]::OrdinalIgnoreCase
    ) -and (Split-Path -Leaf $fullTestRoot).StartsWith(
        'm4-marker-negative-',
        [System.StringComparison]::Ordinal
    )) {
        Remove-Item -LiteralPath $fullTestRoot -Recurse -Force
    }
}
