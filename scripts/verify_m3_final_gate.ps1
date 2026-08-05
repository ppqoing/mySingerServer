function Assert-M3PathHasNoReparsePoint {
    param(
        [Parameter(Mandatory = $true)]
        [string]$FullRoot,
        [Parameter(Mandatory = $true)]
        [string]$FullFile,
        [scriptblock]$AttributeReader
    )
    $relative = [System.IO.Path]::GetRelativePath($FullRoot, $FullFile)
    if ([System.IO.Path]::IsPathRooted($relative) -or
        $relative -eq '..' -or
        $relative.StartsWith(
            '..\',
            [System.StringComparison]::Ordinal
        )) {
        throw "repository Go source escaped the repository root: $FullFile"
    }
    $components = @($relative -split '[\\/]')
    $current = $FullRoot
    foreach ($component in @('') + $components) {
        if (-not [string]::IsNullOrEmpty($component)) {
            $current = Join-Path $current $component
        }
        try {
            $attributes = if ($null -ne $AttributeReader) {
                & $AttributeReader $current
            }
            else {
                [System.IO.File]::GetAttributes($current)
            }
        }
        catch {
            throw "repository Go source path component does not exist: $current"
        }
        if (($attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw "repository Go source path contains a reparse point: $current"
        }
    }
}

function Assert-M3RepoOwnedGoFiles {
    param(
        [Parameter(Mandatory = $true)]
        [string]$RepoRoot,
        [AllowEmptyCollection()]
        [string[]]$Files
    )
    if ([string]::IsNullOrWhiteSpace($RepoRoot)) {
        throw 'repository root must be a non-empty path.'
    }
    try {
        $fullRoot = [System.IO.Path]::GetFullPath(
            (Resolve-Path -LiteralPath $RepoRoot -ErrorAction Stop).Path
        )
    }
    catch {
        throw 'repository root does not exist.'
    }
    if (-not (Test-Path -LiteralPath $fullRoot -PathType Container)) {
        throw 'repository root must be a directory.'
    }
    if ($null -eq $Files -or $Files.Count -eq 0) {
        throw 'repository Go source enumeration returned zero files.'
    }

    $rootPrefix = $fullRoot.TrimEnd('\') + '\'
    $seen = [System.Collections.Generic.HashSet[string]]::new(
        [System.StringComparer]::OrdinalIgnoreCase
    )
    $validated = [System.Collections.Generic.List[string]]::new()
    foreach ($file in $Files) {
        if ([string]::IsNullOrWhiteSpace($file)) {
            throw 'repository Go source path must be non-empty.'
        }
        $fullFile = [System.IO.Path]::GetFullPath($file)
        if (-not $fullFile.StartsWith(
            $rootPrefix,
            [System.StringComparison]::OrdinalIgnoreCase
        )) {
            throw "repository Go source escaped the repository root: $file"
        }
        Assert-M3PathHasNoReparsePoint `
            -FullRoot $fullRoot `
            -FullFile $fullFile
        if (-not (Test-Path -LiteralPath $fullFile -PathType Leaf)) {
            throw "repository Go source does not exist: $file"
        }
        if ([System.IO.Path]::GetExtension($fullFile) -cne '.go') {
            throw "repository Go source does not have .go extension: $file"
        }
        if (-not $seen.Add($fullFile)) {
            throw "repository Go source is duplicated: $file"
        }
        $validated.Add($fullFile)
    }
    foreach ($fullFile in $validated) {
        Assert-M3PathHasNoReparsePoint `
            -FullRoot $fullRoot `
            -FullFile $fullFile
    }
    return $validated.ToArray()
}

function Get-M3RepoOwnedGoFiles {
    param(
        [Parameter(Mandatory = $true)]
        [string]$RepoRoot
    )
    if ([string]::IsNullOrWhiteSpace($RepoRoot)) {
        throw 'repository root must be a non-empty path.'
    }
    try {
        $fullRoot = [System.IO.Path]::GetFullPath(
            (Resolve-Path -LiteralPath $RepoRoot -ErrorAction Stop).Path
        )
    }
    catch {
        throw 'repository root does not exist.'
    }
    if (-not (Test-Path -LiteralPath $fullRoot -PathType Container)) {
        throw 'repository root must be a directory.'
    }

    $excludedSegments = [System.Collections.Generic.HashSet[string]]::new(
        [System.StringComparer]::OrdinalIgnoreCase
    )
    foreach ($segment in @(
        '.git',
        '.superpowers',
        '.tmp',
        '.cache',
        'vendor',
        'node_modules',
        'third_party',
        'build',
        'dist',
        'out',
        'bin',
        'obj'
    )) {
        [void]$excludedSegments.Add($segment)
    }

    $files = [System.Collections.Generic.List[string]]::new()
    foreach ($item in @(
        Get-ChildItem `
            -LiteralPath $fullRoot `
            -Recurse `
            -Force `
            -File `
            -Filter '*.go' `
            -ErrorAction Stop
    )) {
        $relative = [System.IO.Path]::GetRelativePath(
            $fullRoot,
            $item.FullName
        )
        if ([System.IO.Path]::IsPathRooted($relative) -or
            $relative -eq '..' -or
            $relative.StartsWith(
                '..\',
                [System.StringComparison]::Ordinal
            )) {
            throw "enumerated Go source escaped the repository root: $($item.FullName)"
        }
        $excluded = $false
        foreach ($segment in @($relative -split '[\\/]')) {
            if ($excludedSegments.Contains($segment)) {
                $excluded = $true
                break
            }
        }
        if (-not $excluded) {
            $files.Add([System.IO.Path]::GetFullPath($item.FullName))
        }
    }
    $sorted = @($files | Sort-Object)
    return Assert-M3RepoOwnedGoFiles -RepoRoot $fullRoot -Files $sorted
}

function Assert-M3FormatOutput {
    param(
        [AllowEmptyString()]
        [string]$Output
    )
    if ($null -ne $Output -and $Output.Length -gt 0) {
        throw "gofmt reported unformatted repository-owned Go files: $Output"
    }
}

function Assert-M3CommandExitCode {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Name,
        [Parameter(Mandatory = $true)]
        [object]$ExitCode
    )
    Assert-M3NativeInteger -Value $ExitCode -Name "$Name.exit_code"
    if ($ExitCode -ne 0) {
        throw "$Name failed with exit code $ExitCode"
    }
}

function Assert-M3MatrixOutput {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Output,
        [Parameter(Mandatory = $true)]
        [string]$Marker,
        [Parameter(Mandatory = $true)]
        [int]$ExpectedCases
    )
    $pattern = '(?m)^' + [regex]::Escape($Marker) +
        '\s+cases=' + $ExpectedCases + '\r?$'
    $matches = [regex]::Matches($Output, $pattern)
    if ($matches.Count -ne 1) {
        throw "expected exactly one $Marker cases=$ExpectedCases line, got $($matches.Count)"
    }
}

function Resolve-M3ScaleMode {
    param(
        [switch]$Scale,
        [switch]$RunScale
    )
    return [bool]($Scale -or $RunScale)
}

function Assert-M3PGContractOutput {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Output,
        [Parameter(Mandatory = $true)]
        [string[]]$ExpectedTests
    )
    if ($ExpectedTests.Count -eq 0) {
        throw 'PostgreSQL contract expected-test list is empty.'
    }
    $uniqueExpected = [System.Collections.Generic.HashSet[string]]::new(
        [System.StringComparer]::Ordinal
    )
    foreach ($test in $ExpectedTests) {
        if ([string]::IsNullOrWhiteSpace($test) -or
            -not $uniqueExpected.Add($test)) {
            throw 'PostgreSQL contract expected-test list is invalid.'
        }
        $escaped = [regex]::Escape($test)
        $runMatches = [regex]::Matches(
            $Output,
            "(?m)^=== RUN   $escaped`r?$"
        )
        if ($runMatches.Count -ne 1) {
            throw "PostgreSQL contract $test must have exactly one top-level RUN."
        }
        $skipMatches = [regex]::Matches(
            $Output,
            "(?m)^--- SKIP: $escaped(?:\s|\()"
        )
        if ($skipMatches.Count -ne 0) {
            throw "PostgreSQL contract $test was skipped."
        }
        $passMatches = [regex]::Matches(
            $Output,
            "(?m)^--- PASS: $escaped \([^\r\n]+\)\r?$"
        )
        if ($passMatches.Count -ne 1) {
            throw "PostgreSQL contract $test must have exactly one top-level PASS."
        }
    }

    $namedMatches = [regex]::Matches(
        $Output,
        '(?m)^=== RUN   (TestPG(?:Keyset|ReplaceResults)[^\s/]+)\r?$'
    )
    $named = @($namedMatches | ForEach-Object {
        $_.Groups[1].Value
    })
    if ($named.Count -ne $ExpectedTests.Count) {
        throw "PostgreSQL contract top-level RUN count=$($named.Count), want $($ExpectedTests.Count)."
    }
    foreach ($test in $named) {
        if (-not $uniqueExpected.Contains($test)) {
            throw "unexpected PostgreSQL contract test ran: $test"
        }
    }

    $cleanupMatches = [regex]::Matches(
        $Output,
        'Task4 cleanup schema=(fs_t4_[a-z0-9]+) residual=([0-9]+)'
    )
    if ($cleanupMatches.Count -lt $ExpectedTests.Count) {
        throw "PostgreSQL contract cleanup count=$($cleanupMatches.Count), want at least $($ExpectedTests.Count)."
    }
    $cleanupSchemas = [System.Collections.Generic.HashSet[string]]::new(
        [System.StringComparer]::Ordinal
    )
    $cleanup = @()
    foreach ($match in $cleanupMatches) {
        $schema = $match.Groups[1].Value
        $residual = [int]$match.Groups[2].Value
        if (-not $cleanupSchemas.Add($schema)) {
            throw "PostgreSQL contract cleanup schema is duplicated: $schema"
        }
        if ($residual -ne 0) {
            throw "PostgreSQL contract cleanup residual for $schema is $residual."
        }
        $cleanup += [ordered]@{
            schema = $schema
            residual = $residual
        }
    }

    $schemaTest = 'TestPGKeysetSchemaTwiceIndexesAndExplainEligibility'
    $rollbackTests = @(
        'TestPGReplaceResultsRemoteFailuresRollbackAndKeepConnectionUsable',
        'TestPGReplaceResultsAbortedCommitIsDefiniteRollback',
        'TestPGReplaceResultsClosedConnectionDoesNotChangeResults'
    )
    $m4Tests = @(
        'TestPGReplaceResultsSuccessIdempotencyAndM4Preservation',
        'TestPGReplaceResultsConcurrentM4InsertAfterDeleteIsPreserved'
    )
    return [ordered]@{
        expected_tests = @($ExpectedTests)
        tests_passed = @($ExpectedTests)
        cleanup = $cleanup
        schema_twice = $uniqueExpected.Contains($schemaTest)
        exact_indexes = $uniqueExpected.Contains($schemaTest)
        expected_indexes = @(
            'idx_files_sha512_id',
            'idx_dup_groups_kind',
            'idx_dup_members_file'
        )
        actual_explain = $uniqueExpected.Contains($schemaTest)
        failure_rollback = @($rollbackTests | Where-Object {
            $uniqueExpected.Contains($_)
        }).Count -eq $rollbackTests.Count
        unknown_commit = $uniqueExpected.Contains(
            'TestPGReplaceResultsLostCommitAckIsUnknownAndRetryConverges'
        )
        m4_preservation = @($m4Tests | Where-Object {
            $uniqueExpected.Contains($_)
        }).Count -eq $m4Tests.Count
    }
}

function Assert-M3RequiredGateEvidence {
    param(
        [Parameter(Mandatory = $true)]
        [System.Collections.IDictionary]$GateResults,
        [Parameter(Mandatory = $true)]
        [string[]]$RequiredGates,
        [Parameter(Mandatory = $true)]
        [string]$EvidenceDir
    )
    $fullEvidenceDir = [System.IO.Path]::GetFullPath($EvidenceDir)
    $evidencePrefix = $fullEvidenceDir.TrimEnd('\') + '\'
    foreach ($name in $RequiredGates) {
        if (-not $GateResults.Contains($name)) {
            throw "required gate result is missing: $name"
        }
        $gate = $GateResults[$name]
        if ($null -eq $gate -or
            $gate.status -isnot [string] -or
            $gate.status -cne 'PASS') {
            throw "required gate is not PASS: $name"
        }
        Assert-M3NativeInteger `
            -Value $gate.exit_code `
            -Name "gates.$name.exit_code"
        if ($gate.exit_code -ne 0) {
            throw "required gate exit code is non-zero: $name"
        }
        if ($gate.log -isnot [string] -or
            [string]::IsNullOrWhiteSpace($gate.log)) {
            throw "required gate log path is missing: $name"
        }
        $fullLog = [System.IO.Path]::GetFullPath($gate.log)
        if (-not $fullLog.StartsWith(
            $evidencePrefix,
            [System.StringComparison]::OrdinalIgnoreCase
        )) {
            throw "required gate log is outside evidence directory: $name"
        }
        if (-not (Test-Path -LiteralPath $fullLog -PathType Leaf)) {
            throw "required gate log does not exist: $name"
        }
    }
}

function Add-M3NotRunGates {
    param(
        [Parameter(Mandatory = $true)]
        [System.Collections.IDictionary]$GateResults,
        [Parameter(Mandatory = $true)]
        [string[]]$RequiredGates
    )
    foreach ($name in $RequiredGates) {
        if (-not $GateResults.Contains($name)) {
            $GateResults[$name] = [ordered]@{
                status = 'NOT_RUN'
                exit_code = $null
                log = ''
            }
        }
    }
}

function Get-M3GateSummaryLines {
    param(
        [Parameter(Mandatory = $true)]
        [System.Collections.IDictionary]$GateResults,
        [Parameter(Mandatory = $true)]
        [string[]]$RequiredGates
    )
    foreach ($name in $RequiredGates) {
        $gate = if ($GateResults.Contains($name)) {
            $GateResults[$name]
        }
        else {
            [ordered]@{
                status = 'NOT_RUN'
                exit_code = $null
                log = ''
            }
        }
        $status = if ($gate.status -in @('PASS', 'FAIL', 'NOT_RUN')) {
            [string]$gate.status
        }
        else {
            'FAIL'
        }
        $exitText = if ($null -eq $gate.exit_code) {
            '-'
        }
        else {
            [string]$gate.exit_code
        }
        $logText = if ([string]::IsNullOrWhiteSpace([string]$gate.log)) {
            '-'
        }
        else {
            [string]$gate.log
        }
        "M3 GATE $name $status exit=$exitText log=$logText"
    }
}
