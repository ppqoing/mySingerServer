function Get-M3RequiredValue {
    param(
        [Parameter(Mandatory = $true)]
        [object]$Object,
        [Parameter(Mandatory = $true)]
        [string]$Name
    )
    if ($null -eq $Object) {
        throw "marker object for $Name is null"
    }
    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property) {
        throw "acceptance marker missing property $Name"
    }
    if ($null -eq $property.Value) {
        throw "acceptance marker property $Name is null"
    }
    return $property.Value
}

function Assert-M3NativeInteger {
    param(
        [Parameter(Mandatory = $true)]
        [object]$Value,
        [Parameter(Mandatory = $true)]
        [string]$Name
    )
    $integerTypes = @(
        [sbyte],
        [byte],
        [int16],
        [uint16],
        [int32],
        [uint32],
        [int64],
        [uint64]
    )
    if ($integerTypes -notcontains $Value.GetType()) {
        throw "acceptance marker property $Name must be a JSON integer"
    }
}

function Assert-M3Boolean {
    param(
        [Parameter(Mandatory = $true)]
        [object]$Value,
        [Parameter(Mandatory = $true)]
        [string]$Name
    )
    if ($Value -isnot [bool]) {
        throw "acceptance marker property $Name must be a JSON boolean"
    }
}

function Assert-M3AcceptanceMarker {
    param(
        [Parameter(Mandatory = $true)]
        [object]$Marker,
        [Parameter(Mandatory = $true)]
        [string]$ExpectedRunID
    )
    if ($Marker -isnot [pscustomobject]) {
        throw 'acceptance marker root must be a JSON object'
    }

    $runID = Get-M3RequiredValue -Object $Marker -Name 'run_id'
    if ($runID -isnot [string] -or [string]::IsNullOrWhiteSpace($runID)) {
        throw 'acceptance marker run_id must be a non-empty JSON string'
    }
    if ($runID -ne $ExpectedRunID) {
        throw 'acceptance marker run_id does not match this verifier run.'
    }

    $counts = Get-M3RequiredValue -Object $Marker -Name 'counts'
    if ($counts -isnot [pscustomobject]) {
        throw 'acceptance marker counts must be a JSON object'
    }
    $wantCounts = [ordered]@{
        files_scanned = 13
        image_features = 4
        video_features = 4
        exact_groups = 2
        exact_members = 5
        image_pairs = 1
        video_pairs = 2
        groups_written = 5
        members_written = 12
        skipped_pairs = 0
        bad_rows = 0
    }
    foreach ($entry in $wantCounts.GetEnumerator()) {
        $value = Get-M3RequiredValue -Object $counts -Name $entry.Key
        Assert-M3NativeInteger -Value $value -Name "counts.$($entry.Key)"
        if ($value -ne $entry.Value) {
            throw "acceptance count $($entry.Key)=$value, want $($entry.Value)"
        }
    }

    $stages = Get-M3RequiredValue -Object $Marker -Name 'stage_keys'
    if ($stages -isnot [System.Array] -or $stages.Count -ne 6) {
        throw 'acceptance stage_keys must be a six-element JSON array'
    }
    $seen = [System.Collections.Generic.HashSet[string]]::new(
        [System.StringComparer]::Ordinal
    )
    foreach ($stage in $stages) {
        if ($stage -isnot [string] -or [string]::IsNullOrWhiteSpace($stage)) {
            throw 'acceptance stage_keys entries must be non-empty JSON strings'
        }
        if (-not $seen.Add($stage)) {
            throw "acceptance stage_keys contains duplicate $stage"
        }
    }
    $wantStages = @(
        'db_write',
        'exact_group',
        'image_load',
        'image_screen',
        'video_load',
        'video_screen'
    )
    $sortedStages = @($stages | Sort-Object)
    if (($sortedStages -join ',') -ne ($wantStages -join ',')) {
        throw "acceptance stage keys are incomplete: $($sortedStages -join ',')"
    }

    foreach ($integerContract in @(
        @('cleanup_residual', 0),
        @('central_sql_runs', 2),
        @('read_page_size', 3)
    )) {
        $name = [string]$integerContract[0]
        $expected = [int]$integerContract[1]
        $value = Get-M3RequiredValue -Object $Marker -Name $name
        Assert-M3NativeInteger -Value $value -Name $name
        if ($value -ne $expected) {
            throw "acceptance marker $name=$value, want $expected"
        }
    }

    foreach ($booleanName in @(
        'rerun',
        'sentinel_preserved',
        'public_unchanged'
    )) {
        $value = Get-M3RequiredValue -Object $Marker -Name $booleanName
        Assert-M3Boolean -Value $value -Name $booleanName
        if (-not $value) {
            throw "acceptance marker $booleanName must be true"
        }
    }
}
