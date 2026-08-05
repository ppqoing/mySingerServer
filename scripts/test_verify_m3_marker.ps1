[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'verify_m3_marker.ps1')

function New-ValidMarker {
    return [pscustomobject]@{
        run_id = 'marker-test-run'
        counts = [pscustomobject]@{
            files_scanned = [int64]13
            image_features = [int64]4
            video_features = [int64]4
            exact_groups = [int64]2
            exact_members = [int64]5
            image_pairs = [int64]1
            video_pairs = [int64]2
            groups_written = [int64]5
            members_written = [int64]12
            skipped_pairs = [int64]0
            bad_rows = [int64]0
        }
        stage_keys = [object[]]@(
            'db_write',
            'exact_group',
            'image_load',
            'image_screen',
            'video_load',
            'video_screen'
        )
        cleanup_residual = [int64]0
        rerun = $true
        central_sql_runs = [int64]2
        read_page_size = [int64]3
        sentinel_preserved = $true
        public_unchanged = $true
    }
}

function Copy-Marker {
    return ((New-ValidMarker | ConvertTo-Json -Depth 8) | ConvertFrom-Json)
}

function Assert-Rejected {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Name,
        [Parameter(Mandatory = $true)]
        [scriptblock]$Mutate
    )
    $marker = Copy-Marker
    & $Mutate $marker
    $rejected = $false
    try {
        Assert-M3AcceptanceMarker `
            -Marker $marker `
            -ExpectedRunID 'marker-test-run'
    }
    catch {
        $rejected = $true
    }
    if (-not $rejected) {
        throw "negative marker case unexpectedly accepted: $Name"
    }
}

Assert-M3AcceptanceMarker `
    -Marker (New-ValidMarker) `
    -ExpectedRunID 'marker-test-run'

$cases = [ordered]@{
    missing_cleanup = {
        param($marker)
        $marker.PSObject.Properties.Remove('cleanup_residual')
    }
    null_cleanup = {
        param($marker)
        $marker.cleanup_residual = $null
    }
    string_cleanup = {
        param($marker)
        $marker.cleanup_residual = '0'
    }
    null_zero_count = {
        param($marker)
        $marker.counts.skipped_pairs = $null
    }
    string_zero_count = {
        param($marker)
        $marker.counts.bad_rows = '0'
    }
    bool_integer_count = {
        param($marker)
        $marker.counts.files_scanned = $true
    }
    double_integer_count = {
        param($marker)
        $marker.counts.files_scanned = [double]13
    }
    string_bool = {
        param($marker)
        $marker.rerun = 'true'
    }
    duplicate_stages = {
        param($marker)
        $marker.stage_keys[5] = 'video_load'
    }
    nonstring_stage = {
        param($marker)
        $marker.stage_keys[5] = [int64]6
    }
    string_counts_object = {
        param($marker)
        $marker.counts = 'not-an-object'
    }
    null_public_bool = {
        param($marker)
        $marker.public_unchanged = $null
    }
}

foreach ($entry in $cases.GetEnumerator()) {
    Assert-Rejected -Name $entry.Key -Mutate $entry.Value
}

Write-Host "M3_MARKER_NEGATIVE_PASS cases=$($cases.Count)"
