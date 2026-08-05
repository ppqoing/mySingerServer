. (Join-Path $PSScriptRoot 'verify_m4_helpers.ps1')

function Get-M4RequiredValue {
    param(
        [Parameter(Mandatory = $true)]
        [object]$Object,
        [Parameter(Mandatory = $true)]
        [string]$Name,
        [Parameter(Mandatory = $true)]
        [string]$Path
    )
    if ($null -eq $Object) {
        throw "$Path is null"
    }
    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property) {
        throw "$Path is missing property $Name"
    }
    if ($null -eq $property.Value) {
        throw "$Path.$Name is null"
    }
    return $property.Value
}

function Assert-M4ExactProperties {
    param(
        [Parameter(Mandatory = $true)]
        [object]$Object,
        [Parameter(Mandatory = $true)]
        [string[]]$Names,
        [Parameter(Mandatory = $true)]
        [string]$Path
    )
    if ($Object -isnot [pscustomobject]) {
        throw "$Path must be a JSON object"
    }
    $actual = @($Object.PSObject.Properties.Name | Sort-Object)
    $expected = @($Names | Sort-Object)
    if (($actual -join "`n") -cne ($expected -join "`n")) {
        throw "$Path properties mismatch expected=$($expected -join ',') actual=$($actual -join ',')"
    }
}

function Assert-M4NativeInteger {
    param(
        [Parameter(Mandatory = $true)]
        [object]$Value,
        [Parameter(Mandatory = $true)]
        [string]$Name
    )
    if (@(
        [sbyte], [byte], [int16], [uint16],
        [int32], [uint32], [int64], [uint64]
    ) -notcontains $Value.GetType()) {
        throw "$Name must be a JSON integer"
    }
}

function Assert-M4IntegerValue {
    param(
        [Parameter(Mandatory = $true)]
        [object]$Object,
        [Parameter(Mandatory = $true)]
        [string]$Name,
        [Parameter(Mandatory = $true)]
        [int64]$Expected,
        [Parameter(Mandatory = $true)]
        [string]$Path
    )
    $value = Get-M4RequiredValue -Object $Object -Name $Name -Path $Path
    Assert-M4NativeInteger -Value $value -Name "$Path.$Name"
    if ([int64]$value -ne $Expected) {
        throw "$Path.$Name=$value, want $Expected"
    }
}

function Assert-M4True {
    param(
        [Parameter(Mandatory = $true)]
        [object]$Object,
        [Parameter(Mandatory = $true)]
        [string]$Name,
        [Parameter(Mandatory = $true)]
        [string]$Path
    )
    $value = Get-M4RequiredValue -Object $Object -Name $Name -Path $Path
    if ($value -isnot [bool]) {
        throw "$Path.$Name must be a JSON boolean"
    }
    if (-not $value) {
        throw "$Path.$Name must be true"
    }
}

function Assert-M4False {
    param(
        [Parameter(Mandatory = $true)]
        [object]$Object,
        [Parameter(Mandatory = $true)]
        [string]$Name,
        [Parameter(Mandatory = $true)]
        [string]$Path
    )
    $value = Get-M4RequiredValue -Object $Object -Name $Name -Path $Path
    if ($value -isnot [bool]) {
        throw "$Path.$Name must be a JSON boolean"
    }
    if ($value) {
        throw "$Path.$Name must be false"
    }
}

function Assert-M4ExactString {
    param(
        [Parameter(Mandatory = $true)]
        [object]$Object,
        [Parameter(Mandatory = $true)]
        [string]$Name,
        [Parameter(Mandatory = $true)]
        [string]$Expected,
        [Parameter(Mandatory = $true)]
        [string]$Path
    )
    $value = Get-M4RequiredValue -Object $Object -Name $Name -Path $Path
    if ($value -isnot [string] -or $value -cne $Expected) {
        throw "$Path.$Name must equal $Expected"
    }
}

function Assert-M4SafeEvidencePath {
    param(
        [Parameter(Mandatory = $true)]
        [object]$Value,
        [Parameter(Mandatory = $true)]
        [string]$ExpectedEvidenceDir
    )
    if ($Value -isnot [string] -or [string]::IsNullOrWhiteSpace($Value)) {
        throw 'marker.evidence_path must be a non-empty JSON string'
    }
    try {
        $expected = [System.IO.Path]::GetFullPath(
            (Resolve-Path -LiteralPath $ExpectedEvidenceDir -ErrorAction Stop).Path
        )
        $actual = [System.IO.Path]::GetFullPath(
            (Resolve-Path -LiteralPath $Value -ErrorAction Stop).Path
        )
    }
    catch {
        throw 'marker evidence path must resolve to an existing path'
    }
    if (-not (Test-Path -LiteralPath $actual -PathType Container)) {
        throw 'marker evidence path must be a directory'
    }
    if (-not [string]::Equals(
        $actual,
        $expected,
        [System.StringComparison]::OrdinalIgnoreCase
    )) {
        throw 'marker evidence path does not match this verifier run'
    }
    Assert-M4PathHasNoReparsePoint `
        -FullRoot ([System.IO.Path]::GetPathRoot($actual)) `
        -FullPath $actual
}

function Assert-M4AcceptanceMarker {
    param(
        [Parameter(Mandatory = $true)]
        [object]$Marker,
        [Parameter(Mandatory = $true)]
        [string]$ExpectedRunID,
        [Parameter(Mandatory = $true)]
        [string]$ExpectedSchema,
        [Parameter(Mandatory = $true)]
        [string]$ExpectedEvidenceDir
    )
    $topProperties = @(
        'schema_version',
        'run_id',
        'schema',
        'evidence_path',
        'topology',
        'second_windows_status',
        'e1',
        'e2',
        'e3',
        'e4'
    )
    Assert-M4ExactProperties -Object $Marker -Names $topProperties -Path 'marker'
    Assert-M4IntegerValue `
        -Object $Marker -Name 'schema_version' -Expected 1 -Path 'marker'

    if ($ExpectedRunID -notmatch '^[A-Za-z0-9][A-Za-z0-9._-]{7,127}$') {
        throw 'expected run ID is outside the safe naming contract'
    }
    Assert-M4ExactString `
        -Object $Marker -Name 'run_id' -Expected $ExpectedRunID -Path 'marker'
    if ($ExpectedSchema -notmatch '^m4_e2e_[a-z0-9_]{8,96}$') {
        throw 'expected schema is outside the safe naming contract'
    }
    Assert-M4ExactString `
        -Object $Marker -Name 'schema' -Expected $ExpectedSchema -Path 'marker'
    $evidencePath = Get-M4RequiredValue `
        -Object $Marker -Name 'evidence_path' -Path 'marker'
    Assert-M4SafeEvidencePath `
        -Value $evidencePath `
        -ExpectedEvidenceDir $ExpectedEvidenceDir
    Assert-M4ExactString `
        -Object $Marker `
        -Name 'topology' `
        -Expected 'SINGLE_WINDOWS_TWO_LOCAL_AGENT_IDENTITIES' `
        -Path 'marker'
    Assert-M4ExactString `
        -Object $Marker `
        -Name 'second_windows_status' `
        -Expected 'USER_WAIVED' `
        -Path 'marker'

    $e1 = Get-M4RequiredValue -Object $Marker -Name 'e1' -Path 'marker'
    Assert-M4ExactProperties -Object $e1 -Names @(
        'passed',
        'agent_identities',
        'automatic_dispatch',
        'actual_native_features',
        'phash_blob_bytes',
        'sobel_blob_bytes'
    ) -Path 'marker.e1'
    Assert-M4True -Object $e1 -Name 'passed' -Path 'marker.e1'
    $identities = Get-M4RequiredValue `
        -Object $e1 -Name 'agent_identities' -Path 'marker.e1'
    if ($identities -isnot [System.Array] -or $identities.Count -ne 2) {
        throw 'marker.e1.agent_identities must be a two-element JSON array'
    }
    $identitySet = [System.Collections.Generic.HashSet[string]]::new(
        [System.StringComparer]::Ordinal
    )
    foreach ($identity in $identities) {
        if ($identity -isnot [string] -or
            $identity -notmatch '^m4-local-agent-[ab]-[a-z0-9]{8}$' -or
            -not $identitySet.Add($identity)) {
            throw 'marker.e1.agent_identities entries must be two distinct safe local identities'
        }
    }
    Assert-M4True `
        -Object $e1 -Name 'automatic_dispatch' -Path 'marker.e1'
    Assert-M4True `
        -Object $e1 -Name 'actual_native_features' -Path 'marker.e1'
    Assert-M4IntegerValue `
        -Object $e1 -Name 'phash_blob_bytes' -Expected 76 -Path 'marker.e1'
    Assert-M4IntegerValue `
        -Object $e1 -Name 'sobel_blob_bytes' -Expected 516 -Path 'marker.e1'

    $e2 = Get-M4RequiredValue -Object $Marker -Name 'e2' -Path 'marker'
    Assert-M4ExactProperties -Object $e2 -Names @(
        'passed',
        'video_frames_a',
        'video_frames_b',
        'image_verdict',
        'video_verdict',
        'group_detail_api'
    ) -Path 'marker.e2'
    Assert-M4True -Object $e2 -Name 'passed' -Path 'marker.e2'
    Assert-M4IntegerValue `
        -Object $e2 -Name 'video_frames_a' -Expected 6 -Path 'marker.e2'
    Assert-M4IntegerValue `
        -Object $e2 -Name 'video_frames_b' -Expected 6 -Path 'marker.e2'
    Assert-M4ExactString `
        -Object $e2 -Name 'image_verdict' -Expected 'yes' -Path 'marker.e2'
    Assert-M4ExactString `
        -Object $e2 -Name 'video_verdict' -Expected 'yes' -Path 'marker.e2'
    Assert-M4True `
        -Object $e2 -Name 'group_detail_api' -Path 'marker.e2'

    $e3 = Get-M4RequiredValue -Object $Marker -Name 'e3' -Path 'marker'
    Assert-M4ExactProperties -Object $e3 -Names @(
        'passed',
        'gui_restart_recovery',
        'corrupt_survived',
        'timeout_survived',
        'worker_crash_survived',
        'remaining_samples_completed'
    ) -Path 'marker.e3'
    foreach ($name in @(
        'passed',
        'gui_restart_recovery',
        'corrupt_survived',
        'timeout_survived',
        'worker_crash_survived',
        'remaining_samples_completed'
    )) {
        Assert-M4True -Object $e3 -Name $name -Path 'marker.e3'
    }

    $e4 = Get-M4RequiredValue -Object $Marker -Name 'e4' -Path 'marker'
    Assert-M4ExactProperties -Object $e4 -Names @(
        'passed',
        'idempotent_rerun',
        'public_unchanged',
        'cleanup_residual',
        'central_sql_runs',
        'user_media_modified'
    ) -Path 'marker.e4'
    foreach ($name in @('passed', 'idempotent_rerun', 'public_unchanged')) {
        Assert-M4True -Object $e4 -Name $name -Path 'marker.e4'
    }
    Assert-M4IntegerValue `
        -Object $e4 -Name 'cleanup_residual' -Expected 0 -Path 'marker.e4'
    Assert-M4IntegerValue `
        -Object $e4 -Name 'central_sql_runs' -Expected 2 -Path 'marker.e4'
    Assert-M4False `
        -Object $e4 -Name 'user_media_modified' -Path 'marker.e4'
}

function Get-M4GateKeys {
    param([Parameter(Mandatory = $true)][object]$GateResults)
    if ($GateResults -is [System.Collections.IDictionary]) {
        return @($GateResults.Keys)
    }
    if ($GateResults -is [pscustomobject]) {
        return @($GateResults.PSObject.Properties.Name)
    }
    throw 'gate results must be an object'
}

function Get-M4GateValue {
    param(
        [Parameter(Mandatory = $true)]
        [object]$GateResults,
        [Parameter(Mandatory = $true)]
        [string]$Name
    )
    if ($GateResults -is [System.Collections.IDictionary]) {
        if (-not $GateResults.Contains($Name)) {
            throw "required gate is missing: $Name"
        }
        return $GateResults[$Name]
    }
    $property = $GateResults.PSObject.Properties[$Name]
    if ($null -eq $property) {
        throw "required gate is missing: $Name"
    }
    return $property.Value
}

function Assert-M4RequiredGateEvidence {
    param(
        [Parameter(Mandatory = $true)]
        [object]$GateResults,
        [Parameter(Mandatory = $true)]
        [string[]]$RequiredGates,
        [Parameter(Mandatory = $true)]
        [string]$EvidenceDir
    )
    if ($RequiredGates.Count -eq 0) {
        throw 'required gate list must not be empty'
    }
    $unique = [System.Collections.Generic.HashSet[string]]::new(
        [System.StringComparer]::Ordinal
    )
    foreach ($name in $RequiredGates) {
        if ([string]::IsNullOrWhiteSpace($name) -or -not $unique.Add($name)) {
            throw 'required gate list must contain distinct non-empty names'
        }
    }
    $actualKeys = @(Get-M4GateKeys -GateResults $GateResults | Sort-Object)
    $expectedKeys = @($RequiredGates | Sort-Object)
    if (($actualKeys -join "`n") -cne ($expectedKeys -join "`n")) {
        throw 'gate results do not exactly match the required gate set'
    }

    $fullEvidenceDir = [System.IO.Path]::GetFullPath(
        (Resolve-Path -LiteralPath $EvidenceDir -ErrorAction Stop).Path
    )
    Assert-M4PathHasNoReparsePoint `
        -FullRoot ([System.IO.Path]::GetPathRoot($fullEvidenceDir)) `
        -FullPath $fullEvidenceDir
    $prefix = $fullEvidenceDir.TrimEnd('\') + '\'
    foreach ($name in $RequiredGates) {
        $gate = Get-M4GateValue -GateResults $GateResults -Name $name
        Assert-M4ExactProperties `
            -Object $gate `
            -Names @('status', 'exit_code', 'log') `
            -Path "gates.$name"
        Assert-M4ExactString `
            -Object $gate -Name 'status' -Expected 'PASS' -Path "gates.$name"
        Assert-M4IntegerValue `
            -Object $gate -Name 'exit_code' -Expected 0 -Path "gates.$name"
        $logValue = Get-M4RequiredValue `
            -Object $gate -Name 'log' -Path "gates.$name"
        if ($logValue -isnot [string] -or
            [string]::IsNullOrWhiteSpace($logValue)) {
            throw "gates.$name.log must be a non-empty JSON string"
        }
        try {
            $fullLog = [System.IO.Path]::GetFullPath(
                (Resolve-Path -LiteralPath $logValue -ErrorAction Stop).Path
            )
        }
        catch {
            throw "gates.$name.log must resolve to an existing file"
        }
        if (-not $fullLog.StartsWith(
            $prefix,
            [System.StringComparison]::OrdinalIgnoreCase
        )) {
            throw "gates.$name.log is outside the evidence directory"
        }
        if (-not (Test-Path -LiteralPath $fullLog -PathType Leaf)) {
            throw "gates.$name.log is not a file"
        }
        Assert-M4PathHasNoReparsePoint `
            -FullRoot $fullEvidenceDir `
            -FullPath $fullLog
    }
}
