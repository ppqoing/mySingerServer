function Get-M3ScaleProperty {
    param(
        [Parameter(Mandatory = $true)]
        [object]$Object,
        [Parameter(Mandatory = $true)]
        [string]$Name
    )
    if ($Object -isnot [pscustomobject]) {
        throw "scale marker parent for $Name must be a JSON object"
    }
    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property) {
        throw "scale marker missing property $Name"
    }
    if ($null -eq $property.Value) {
        throw "scale marker property $Name is null"
    }
    return $property
}

function Assert-M3ScaleBooleanValue {
    param(
        [object]$Object,
        [string]$Name,
        [bool]$Expected
    )
    $value = (Get-M3ScaleProperty -Object $Object -Name $Name).Value
    Assert-M3Boolean -Value $value -Name $Name
    if ($value -ne $Expected) {
        throw "scale marker $Name=$value, want $Expected"
    }
}

function Assert-M3ScaleIntegerValue {
    param(
        [object]$Object,
        [string]$Name,
        [int64]$Expected
    )
    $value = (Get-M3ScaleProperty -Object $Object -Name $Name).Value
    Assert-M3NativeInteger -Value $value -Name $Name
    if ([int64]$value -ne $Expected) {
        throw "scale marker $Name=$value, want $Expected"
    }
}

function Assert-M3ScaleNonNegativeInteger {
    param(
        [object]$Object,
        [string]$Name
    )
    $value = (Get-M3ScaleProperty -Object $Object -Name $Name).Value
    Assert-M3NativeInteger -Value $value -Name $Name
    if ($value -lt 0) {
        throw "scale marker $Name must be non-negative"
    }
    return $value
}

function Assert-M3ScaleNumber {
    param(
        [object]$Object,
        [string]$Name
    )
    $value = (Get-M3ScaleProperty -Object $Object -Name $Name).Value
    $numberTypes = @(
        [sbyte], [byte], [int16], [uint16], [int32], [uint32],
        [int64], [uint64], [single], [double], [decimal]
    )
    if ($numberTypes -notcontains $value.GetType() -or $value -lt 0) {
        throw "scale marker $Name must be a non-negative JSON number"
    }
    return $value
}

function Assert-M3ScaleExactIntegerObject {
    param(
        [object]$Object,
        [System.Collections.IDictionary]$Expected,
        [string]$Prefix
    )
    if ($Object -isnot [pscustomobject]) {
        throw "scale marker $Prefix must be a JSON object"
    }
    foreach ($entry in $Expected.GetEnumerator()) {
        $name = "$Prefix.$($entry.Key)"
        $value = (Get-M3ScaleProperty `
            -Object $Object `
            -Name ([string]$entry.Key)).Value
        Assert-M3NativeInteger -Value $value -Name $name
        if ([int64]$value -ne [int64]$entry.Value) {
            throw "scale marker $name=$value, want $($entry.Value)"
        }
    }
}

function Assert-M3ScaleStages {
    param(
        [object]$Run,
        [int]$Ordinal
    )
    $wantStages = @(
        'db_write',
        'exact_group',
        'image_load',
        'image_screen',
        'video_load',
        'video_screen'
    )
    $stageKeys = (Get-M3ScaleProperty `
        -Object $Run `
        -Name 'stage_keys').Value
    if ($stageKeys -isnot [System.Array] -or $stageKeys.Count -ne 6) {
        throw "scale run $Ordinal stage_keys must be a six-element JSON array"
    }
    $seen = [System.Collections.Generic.HashSet[string]]::new(
        [System.StringComparer]::Ordinal
    )
    foreach ($stage in $stageKeys) {
        if ($stage -isnot [string] -or [string]::IsNullOrWhiteSpace($stage)) {
            throw "scale run $Ordinal stage key must be a non-empty string"
        }
        if (-not $seen.Add($stage)) {
            throw "scale run $Ordinal contains duplicate stage $stage"
        }
    }
    $sorted = @($stageKeys | Sort-Object)
    if (($sorted -join ',') -ne ($wantStages -join ',')) {
        throw "scale run $Ordinal stage keys are incomplete"
    }

    $stageMS = (Get-M3ScaleProperty -Object $Run -Name 'stage_ms').Value
    if ($stageMS -isnot [pscustomobject]) {
        throw "scale run $Ordinal stage_ms must be a JSON object"
    }
    foreach ($stage in $wantStages) {
        [void](Assert-M3ScaleNonNegativeInteger `
            -Object $stageMS `
            -Name $stage)
    }
    if ($stageMS.image_screen -gt 5000) {
        throw "scale run $Ordinal image_screen exceeds 5000ms"
    }
    if ($stageMS.video_screen -gt 3000) {
        throw "scale run $Ordinal video_screen exceeds 3000ms"
    }
}

function Assert-M3ScalePlan {
    param(
        [object]$Plan,
        [string]$Name
    )
    if ($Plan -isnot [pscustomobject]) {
        throw "scale plan $Name must be a JSON object"
    }
    $root = (Get-M3ScaleProperty `
        -Object $Plan `
        -Name 'root_node_type').Value
    if ($root -isnot [string] -or [string]::IsNullOrWhiteSpace($root)) {
        throw "scale plan $Name root_node_type must be a non-empty string"
    }
    $indexes = (Get-M3ScaleProperty -Object $Plan -Name 'index_names').Value
    if ($indexes -isnot [System.Array] -or $indexes.Count -lt 1) {
        throw "scale plan $Name index_names must be a non-empty JSON array"
    }
    foreach ($index in $indexes) {
        if ($index -isnot [string] -or [string]::IsNullOrWhiteSpace($index)) {
            throw "scale plan $Name index name must be a non-empty string"
        }
    }
    Assert-M3ScaleIntegerValue -Object $Plan -Name 'actual_rows' -Expected 50000
    [void](Assert-M3ScaleNumber -Object $Plan -Name 'planning_ms')
    $executionMS = Assert-M3ScaleNumber -Object $Plan -Name 'execution_ms'
    if ($executionMS -le 0) {
        throw "scale plan $Name execution_ms must be positive"
    }
    [void](Assert-M3ScaleNonNegativeInteger `
        -Object $Plan `
        -Name 'shared_hit_blocks')
    [void](Assert-M3ScaleNonNegativeInteger `
        -Object $Plan `
        -Name 'shared_read_blocks')
    Assert-M3ScaleBooleanValue -Object $Plan -Name 'actual' -Expected $true
}

function Assert-M3ScaleRun {
    param(
        [object]$Run,
        [int]$Ordinal
    )
    if ($Run -isnot [pscustomobject]) {
        throw "scale run $Ordinal must be a JSON object"
    }
    Assert-M3ScaleIntegerValue -Object $Run -Name 'ordinal' -Expected $Ordinal
    $counts = (Get-M3ScaleProperty -Object $Run -Name 'counts').Value
    Assert-M3ScaleExactIntegerObject `
        -Object $counts `
        -Prefix "runs[$Ordinal].counts" `
        -Expected ([ordered]@{
            files_scanned = 1350000
            exact_groups = 50000
            exact_members = 150000
            image_features = 990400
            image_pairs = 60000
            video_features = 200000
            video_pairs = 15000
            groups_written = 125000
            members_written = 300000
            skipped_pairs = 0
            bad_rows = 0
        })
    Assert-M3ScaleStages -Run $Run -Ordinal $Ordinal
    $total = Assert-M3ScaleNonNegativeInteger -Object $Run -Name 'total_ms'
    if ($total -le 0) {
        throw "scale run $Ordinal total_ms must be positive"
    }
    if ($total -gt 90000) {
        throw "scale run $Ordinal total_ms exceeds 90000"
    }
    $peak = Assert-M3ScaleNonNegativeInteger `
        -Object $Run `
        -Name 'peak_heap_bytes'
    if ($peak -le 0) {
        throw "scale run $Ordinal peak_heap_bytes must be positive"
    }
    if ([uint64]$peak -gt [uint64](4GB)) {
        throw "scale run $Ordinal peak_heap_bytes exceeds 4GiB"
    }
}

function Assert-M3ScaleMarker {
    param(
        [Parameter(Mandatory = $true)]
        [object]$Marker,
        [Parameter(Mandatory = $true)]
        [string]$ExpectedRunID,
        [Parameter(Mandatory = $true)]
        [string]$ExpectedSchema,
        [Parameter(Mandatory = $true)]
        [bool]$ExpectedSeeded
    )
    if ($Marker -isnot [pscustomobject]) {
        throw 'scale marker root must be a JSON object'
    }
    foreach ($contract in @(
        @('run_id', $ExpectedRunID),
        @('schema', $ExpectedSchema),
        @('second_windows_status', 'USER_WAIVED')
    )) {
        $value = (Get-M3ScaleProperty `
            -Object $Marker `
            -Name ([string]$contract[0])).Value
        if ($value -isnot [string] -or
            [string]::IsNullOrWhiteSpace($value) -or
            $value -cne [string]$contract[1]) {
            throw "scale marker $($contract[0]) does not match"
        }
    }
    $version = (Get-M3ScaleProperty `
        -Object $Marker `
        -Name 'postgresql_version').Value
    if ($version -isnot [string] -or $version -notmatch '^16[0-9]+$') {
        throw 'scale marker postgresql_version must name PostgreSQL 16'
    }

    Assert-M3ScaleIntegerValue -Object $Marker -Name 'seed' -Expected 1
    Assert-M3ScaleBooleanValue `
        -Object $Marker `
        -Name 'seeded' `
        -Expected $ExpectedSeeded
    Assert-M3ScaleBooleanValue `
        -Object $Marker `
        -Name 'reused' `
        -Expected (-not $ExpectedSeeded)
    Assert-M3ScaleBooleanValue `
        -Object $Marker `
        -Name 'public_unchanged' `
        -Expected $true
    Assert-M3ScaleBooleanValue `
        -Object $Marker `
        -Name 'semantic_idempotent' `
        -Expected $true
    Assert-M3ScaleBooleanValue `
        -Object $Marker `
        -Name 'performance_pass' `
        -Expected $true
    Assert-M3ScaleBooleanValue `
        -Object $Marker `
        -Name 'physical_verified' `
        -Expected $true
    Assert-M3ScaleIntegerValue `
        -Object $Marker `
        -Name 'copy_chunk_rows' `
        -Expected 50000

    $physical = (Get-M3ScaleProperty `
        -Object $Marker `
        -Name 'physical_rows').Value
    Assert-M3ScaleExactIntegerObject `
        -Object $physical `
        -Prefix 'physical_rows' `
        -Expected ([ordered]@{
            files = 1350000
            image_features = 1000000
            video_features = 200000
        })
    $totals = (Get-M3ScaleProperty -Object $Marker -Name 'db_totals').Value
    Assert-M3ScaleExactIntegerObject `
        -Object $totals `
        -Prefix 'db_totals' `
        -Expected ([ordered]@{
            groups_exact = 50000
            groups_image_candidate = 60000
            groups_total = 125000
            groups_video_candidate = 15000
            members_exact = 150000
            members_image_candidate = 120000
            members_total = 300000
            members_video_candidate = 30000
        })

    $plans = (Get-M3ScaleProperty -Object $Marker -Name 'plans').Value
    if ($plans -isnot [pscustomobject]) {
        throw 'scale marker plans must be a JSON object'
    }
    foreach ($planName in @('files', 'image_features', 'video_features')) {
        $plan = (Get-M3ScaleProperty `
            -Object $plans `
            -Name $planName).Value
        Assert-M3ScalePlan -Plan $plan -Name $planName
    }

    $runs = (Get-M3ScaleProperty -Object $Marker -Name 'runs').Value
    $expectedRunCount = if ($ExpectedSeeded) { 2 } else { 1 }
    if ($runs -isnot [System.Array] -or
        $runs.Count -ne $expectedRunCount) {
        throw "scale marker runs must contain $expectedRunCount entries"
    }
    for ($index = 0; $index -lt $runs.Count; $index++) {
        Assert-M3ScaleRun -Run $runs[$index] -Ordinal ($index + 1)
    }

    $chunks = (Get-M3ScaleProperty `
        -Object $Marker `
        -Name 'seed_chunks').Value
    if ($ExpectedSeeded) {
        Assert-M3ScaleIntegerValue `
            -Object $Marker `
            -Name 'central_sql_runs' `
            -Expected 2
        $duration = Assert-M3ScaleNonNegativeInteger `
            -Object $Marker `
            -Name 'seed_duration_ms'
        if ($duration -le 0) {
            throw 'seed marker seed_duration_ms must be positive'
        }
        Assert-M3ScaleExactIntegerObject `
            -Object $chunks `
            -Prefix 'seed_chunks' `
            -Expected ([ordered]@{
                files = 27
                image_features = 20
                video_features = 4
            })
        Assert-M3ScaleBooleanValue `
            -Object $Marker `
            -Name 'schema_preserved' `
            -Expected $true
        Assert-M3ScaleBooleanValue `
            -Object $Marker `
            -Name 'cleanup_performed' `
            -Expected $false
        Assert-M3ScaleIntegerValue `
            -Object $Marker `
            -Name 'cleanup_residual' `
            -Expected -1
    }
    else {
        Assert-M3ScaleIntegerValue `
            -Object $Marker `
            -Name 'central_sql_runs' `
            -Expected 0
        Assert-M3ScaleIntegerValue `
            -Object $Marker `
            -Name 'seed_duration_ms' `
            -Expected 0
        Assert-M3ScaleExactIntegerObject `
            -Object $chunks `
            -Prefix 'seed_chunks' `
            -Expected ([ordered]@{
                files = 0
                image_features = 0
                video_features = 0
            })
        Assert-M3ScaleBooleanValue `
            -Object $Marker `
            -Name 'schema_preserved' `
            -Expected $false
        Assert-M3ScaleBooleanValue `
            -Object $Marker `
            -Name 'cleanup_performed' `
            -Expected $true
        Assert-M3ScaleIntegerValue `
            -Object $Marker `
            -Name 'cleanup_residual' `
            -Expected 0
    }
}

function Assert-M3ScaleCleanupOutput {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Output,
        [Parameter(Mandatory = $true)]
        [string]$ExpectedRunID,
        [Parameter(Mandatory = $true)]
        [string]$ExpectedSchema
    )
    if (-not $Output.Contains('=== RUN   TestAcceptanceM3')) {
        throw 'scale cleanup did not name TestAcceptanceM3.'
    }
    if ($Output.Contains('--- SKIP: TestAcceptanceM3')) {
        throw 'scale cleanup acceptance was skipped.'
    }
    if (-not $Output.Contains('--- PASS: TestAcceptanceM3')) {
        throw 'scale cleanup lacks the named PASS line.'
    }
    $matches = [regex]::Matches(
        $Output,
        'M3_SCALE_CLEANUP\s+(\{[^\r\n]+\})'
    )
    if ($matches.Count -ne 1) {
        throw "expected exactly one M3_SCALE_CLEANUP marker, got $($matches.Count)"
    }
    try {
        $marker = $matches[0].Groups[1].Value | ConvertFrom-Json
    }
    catch {
        throw 'M3_SCALE_CLEANUP marker is not valid JSON.'
    }
    if ($marker -isnot [pscustomobject] -or
        @($marker.PSObject.Properties).Count -ne 3) {
        throw 'M3_SCALE_CLEANUP marker must be an exact three-property JSON object.'
    }
    foreach ($contract in @(
        @('run_id', $ExpectedRunID),
        @('schema', $ExpectedSchema)
    )) {
        $value = (Get-M3ScaleProperty `
            -Object $marker `
            -Name ([string]$contract[0])).Value
        if ($value -isnot [string] -or
            [string]::IsNullOrWhiteSpace($value) -or
            $value -cne [string]$contract[1]) {
            throw "scale cleanup marker $($contract[0]) does not match"
        }
    }
    Assert-M3ScaleIntegerValue `
        -Object $marker `
        -Name 'cleanup_residual' `
        -Expected 0
    return $marker
}

function Join-M3ScaleFailures {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Primary,
        [Parameter(Mandatory = $true)]
        [string]$Cleanup
    )
    return "$Primary; scale cleanup failed: $Cleanup"
}
