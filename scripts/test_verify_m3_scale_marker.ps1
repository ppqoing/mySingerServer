[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'verify_m3_marker.ps1')
. (Join-Path $PSScriptRoot 'verify_m3_scale_marker.ps1')

function New-ScaleRun {
    return [ordered]@{
        ordinal = 1
        counts = [ordered]@{
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
        }
        stage_ms = [ordered]@{
            db_write = 5000
            exact_group = 4500
            image_load = 1600
            image_screen = 900
            video_load = 300
            video_screen = 200
        }
        stage_keys = @(
            'db_write',
            'exact_group',
            'image_load',
            'image_screen',
            'video_load',
            'video_screen'
        )
        total_ms = 13000
        peak_heap_bytes = 1000000000
    }
}

function New-ScaleMarker {
    param([bool]$Seeded)
    $runCount = if ($Seeded) { 2 } else { 1 }
    $runs = @()
    for ($index = 1; $index -le $runCount; $index++) {
        $run = New-ScaleRun
        $run.ordinal = $index
        $runs += $run
    }
    $marker = [ordered]@{
        run_id = 'scale-marker-test'
        schema = 'm3_scale_scale_marker_test'
        seed = 1
        seeded = $Seeded
        reused = -not $Seeded
        postgresql_version = '160014'
        public_unchanged = $true
        central_sql_runs = if ($Seeded) { 2 } else { 0 }
        copy_chunk_rows = 50000
        seed_duration_ms = if ($Seeded) { 44000 } else { 0 }
        seed_chunks = [ordered]@{
            files = if ($Seeded) { 27 } else { 0 }
            image_features = if ($Seeded) { 20 } else { 0 }
            video_features = if ($Seeded) { 4 } else { 0 }
        }
        physical_rows = [ordered]@{
            files = 1350000
            image_features = 1000000
            video_features = 200000
        }
        plans = [ordered]@{}
        runs = $runs
        db_totals = [ordered]@{
            groups_exact = 50000
            groups_image_candidate = 60000
            groups_total = 125000
            groups_video_candidate = 15000
            members_exact = 150000
            members_image_candidate = 120000
            members_total = 300000
            members_video_candidate = 30000
        }
        semantic_idempotent = $true
        performance_pass = $true
        physical_verified = $true
        schema_preserved = $Seeded
        cleanup_performed = -not $Seeded
        cleanup_residual = if ($Seeded) { -1 } else { 0 }
        second_windows_status = 'USER_WAIVED'
    }
    foreach ($name in @('files', 'image_features', 'video_features')) {
        $marker.plans[$name] = [ordered]@{
            root_node_type = 'Limit'
            index_names = @('expected_index')
            actual_rows = 50000
            planning_ms = 0.1
            execution_ms = 20.5
            shared_hit_blocks = 1
            shared_read_blocks = 10
            actual = $true
        }
    }
    return (($marker | ConvertTo-Json -Depth 12) | ConvertFrom-Json)
}

function Copy-ScaleMarker {
    param([bool]$Seeded)
    return ((New-ScaleMarker -Seeded $Seeded | ConvertTo-Json -Depth 12) |
        ConvertFrom-Json)
}

function Assert-ScaleRejected {
    param(
        [string]$Name,
        [bool]$Seeded,
        [scriptblock]$Mutate
    )
    $marker = Copy-ScaleMarker -Seeded $Seeded
    & $Mutate $marker
    $rejected = $false
    try {
        Assert-M3ScaleMarker `
            -Marker $marker `
            -ExpectedRunID 'scale-marker-test' `
            -ExpectedSchema 'm3_scale_scale_marker_test' `
            -ExpectedSeeded $Seeded
    }
    catch {
        $rejected = $true
    }
    if (-not $rejected) {
        throw "negative scale marker unexpectedly accepted: $Name"
    }
}

Assert-M3ScaleMarker `
    -Marker (New-ScaleMarker -Seeded $true) `
    -ExpectedRunID 'scale-marker-test' `
    -ExpectedSchema 'm3_scale_scale_marker_test' `
    -ExpectedSeeded $true
Assert-M3ScaleMarker `
    -Marker (New-ScaleMarker -Seeded $false) `
    -ExpectedRunID 'scale-marker-test' `
    -ExpectedSchema 'm3_scale_scale_marker_test' `
    -ExpectedSeeded $false

$cases = @(
    @{ Name = 'missing_total'; Seeded = $true; Mutate = {
        param($m) $m.runs[0].PSObject.Properties.Remove('total_ms')
    }},
    @{ Name = 'null_total'; Seeded = $true; Mutate = {
        param($m) $m.runs[0].total_ms = $null
    }},
    @{ Name = 'string_total'; Seeded = $true; Mutate = {
        param($m) $m.runs[0].total_ms = '13000'
    }},
    @{ Name = 'bool_integer_count'; Seeded = $true; Mutate = {
        param($m) $m.runs[0].counts.bad_rows = $false
    }},
    @{ Name = 'string_zero_count'; Seeded = $true; Mutate = {
        param($m) $m.runs[0].counts.skipped_pairs = '0'
    }},
    @{ Name = 'string_bool'; Seeded = $true; Mutate = {
        param($m) $m.performance_pass = 'true'
    }},
    @{ Name = 'duplicate_stage'; Seeded = $true; Mutate = {
        param($m) $m.runs[0].stage_keys[5] = 'video_load'
    }},
    @{ Name = 'missing_actual_plan'; Seeded = $true; Mutate = {
        param($m) $m.plans.files.PSObject.Properties.Remove('actual')
    }},
    @{ Name = 'string_plan_number'; Seeded = $true; Mutate = {
        param($m) $m.plans.files.execution_ms = '20.5'
    }},
    @{ Name = 'wrong_seeded_state'; Seeded = $true; Mutate = {
        param($m) $m.seeded = $false
    }},
    @{ Name = 'reuse_cleanup_missing'; Seeded = $false; Mutate = {
        param($m) $m.cleanup_performed = $false
    }},
    @{ Name = 'reuse_cleanup_string_zero'; Seeded = $false; Mutate = {
        param($m) $m.cleanup_residual = '0'
    }},
    @{ Name = 'schema_mismatch'; Seeded = $true; Mutate = {
        param($m) $m.schema = 'm3_scale_other_run'
    }}
)

foreach ($case in $cases) {
    Assert-ScaleRejected `
        -Name $case.Name `
        -Seeded $case.Seeded `
        -Mutate $case.Mutate
}

$authorityPath = Join-Path (
    [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
) '.superpowers\evidence\m3-20260728-081525-795-10d619e8\m3-evidence.json'
if (-not (Test-Path -LiteralPath $authorityPath -PathType Leaf)) {
    throw 'Task9 authority evidence for marker mutation is missing.'
}
$authority = Get-Content -Raw -LiteralPath $authorityPath | ConvertFrom-Json
$authorityRunID = [string]$authority.run_id
$authoritySchema = [string]$authority.scale.schema

function Assert-AuthorityRejected {
    param(
        [string]$Name,
        [bool]$Seeded,
        [scriptblock]$Mutate
    )
    $source = if ($Seeded) {
        $authority.scale.seed
    } else {
        $authority.scale.reuse
    }
    $marker = (($source | ConvertTo-Json -Depth 12) | ConvertFrom-Json)
    & $Mutate $marker
    $rejected = $false
    try {
        Assert-M3ScaleMarker `
            -Marker $marker `
            -ExpectedRunID $authorityRunID `
            -ExpectedSchema $authoritySchema `
            -ExpectedSeeded $Seeded
    }
    catch {
        $rejected = $true
    }
    if (-not $rejected) {
        throw "authority marker mutation unexpectedly accepted: $Name"
    }
}

$boundaryCases = @(
    @{ Name = 'total_zero'; Seeded = $true; Mutate = {
        param($m) $m.runs[0].total_ms = 0
    }},
    @{ Name = 'peak_zero'; Seeded = $true; Mutate = {
        param($m) $m.runs[0].peak_heap_bytes = 0
    }},
    @{ Name = 'execution_zero'; Seeded = $true; Mutate = {
        param($m) $m.plans.files.execution_ms = 0
    }},
    @{ Name = 'total_over_90s'; Seeded = $true; Mutate = {
        param($m) $m.runs[0].total_ms = 90001
    }},
    @{ Name = 'image_over_5s'; Seeded = $true; Mutate = {
        param($m) $m.runs[0].stage_ms.image_screen = 5001
    }},
    @{ Name = 'video_over_3s'; Seeded = $true; Mutate = {
        param($m) $m.runs[0].stage_ms.video_screen = 3001
    }},
    @{ Name = 'peak_over_4gib'; Seeded = $true; Mutate = {
        param($m) $m.runs[0].peak_heap_bytes = [int64](4GB + 1)
    }},
    @{ Name = 'wrong_exact_count'; Seeded = $true; Mutate = {
        param($m) $m.runs[0].counts.exact_groups = 49999
    }},
    @{ Name = 'run_id_mismatch'; Seeded = $true; Mutate = {
        param($m) $m.run_id = 'different-authority-run'
    }},
    @{ Name = 'reuse_schema_state_mismatch'; Seeded = $false; Mutate = {
        param($m)
        $m.schema = 'm3_scale_different_authority_run'
        $m.seeded = $true
        $m.reused = $false
    }}
)

foreach ($case in $boundaryCases) {
    Assert-AuthorityRejected `
        -Name $case.Name `
        -Seeded $case.Seeded `
        -Mutate $case.Mutate
}

$totalCases = $cases.Count + $boundaryCases.Count
Write-Host "M3_SCALE_MARKER_NEGATIVE_PASS cases=$totalCases"
