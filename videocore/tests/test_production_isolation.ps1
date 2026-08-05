param(
    [Parameter(Mandatory = $true)]
    [string]$Dumpbin,

    [Parameter(Mandatory = $true)]
    [string]$WinFileObject,

    [Parameter(Mandatory = $true)]
    [string]$MediaSessionObject,

    [Parameter(Mandatory = $true)]
    [string]$Compiler,

    [Parameter(Mandatory = $true)]
    [string]$VsDevCmd,

    [Parameter(Mandatory = $true)]
    [string]$CompilerCommandTlog,

    [Parameter(Mandatory = $true)]
    [string]$WinFileSource,

    [Parameter(Mandatory = $true)]
    [string]$MediaSessionSource,

    [Parameter(Mandatory = $true)]
    [string]$MediaSessionHeader
)

$ErrorActionPreference = 'Stop'
$objectPaths = @($WinFileObject, $MediaSessionObject)

function Test-Identifier {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Text,

        [Parameter(Mandatory = $true)]
        [string]$Identifier
    )

    $pattern = '(?<![A-Za-z0-9_])' +
        [regex]::Escape($Identifier) +
        '(?![A-Za-z0-9_])'
    return [regex]::IsMatch($Text, $pattern)
}

function Test-IdentifierPrefix {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Text,

        [Parameter(Mandatory = $true)]
        [string]$Prefix
    )

    $pattern = '(?<![A-Za-z0-9_])' +
        [regex]::Escape($Prefix) +
        '[A-Za-z0-9_]*'
    return [regex]::IsMatch($Text, $pattern)
}

function Get-PreprocessedProjectText {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Text,

        [Parameter(Mandatory = $true)]
        [string[]]$Sources
    )

    $sourcePaths =
        [System.Collections.Generic.HashSet[string]]::new(
            [System.StringComparer]::OrdinalIgnoreCase)
    foreach ($source in $Sources) {
        if (-not (Test-Path -LiteralPath $source -PathType Leaf)) {
            throw "project source is missing: $source"
        }
        [void]$sourcePaths.Add(
            [System.IO.Path]::GetFullPath($source))
    }

    $projectLines = [System.Collections.Generic.List[string]]::new()
    $isProjectSource = $false
    foreach ($line in ($Text -split "`r?`n")) {
        if ($line -match '^\s*#line\s+\d+\s+"(?<path>[^"]+)"') {
            $markerPath = $Matches.path.Replace('\\', '\')
            try {
                $markerPath =
                    [System.IO.Path]::GetFullPath($markerPath)
                $isProjectSource = $sourcePaths.Contains($markerPath)
            } catch {
                $isProjectSource = $false
            }
            continue
        }
        if ($isProjectSource) {
            $projectLines.Add($line)
        }
    }
    return $projectLines -join "`n"
}

function Get-GenericTestIdentifiers {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Text
    )

    $allowedProductionIdentifiers = @(
        'test_and_set'
    )
    return [regex]::Matches(
        $Text,
        '(?<![A-Za-z0-9_])[A-Za-z_][A-Za-z0-9_]*(?![A-Za-z0-9_])') |
        ForEach-Object { $_.Value } |
        Where-Object {
            ($_.Contains('Test') -or $_ -cmatch '(^|_)test_') -and
            $_ -cnotin $allowedProductionIdentifiers
        } |
        Sort-Object -Unique
}

$forbiddenIdentifiers = @(
    'RunHook',
    'SetIoHook',
    'WinFileStats',
    'IoBoundary',
    'IoBoundaryHook',
    'stats_',
    'create_file_calls',
    'read_calls',
    'seek_calls',
    'size_queries',
    'hook_',
    'hook_context_',
    'fail_next_win_file_allocation',
    'MediaSessionTestSnapshot',
    'GetMediaSessionTestSnapshot',
    'SetMediaSessionTestIoHook',
    'test_hash_failure_',
    'session_test_first_context',
    'session_test_second_context',
    'SetMediaSessionTestHashFailure'
)
$forbiddenPrefixes = @(
    'WinFileTest',
    'MediaSessionTest'
)

$hits = [System.Collections.Generic.List[string]]::new()
foreach ($objectPath in $objectPaths) {
    if (-not (Test-Path -LiteralPath $objectPath -PathType Leaf)) {
        throw "production object is missing: $objectPath"
    }
    $symbols = & $Dumpbin /SYMBOLS $objectPath 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "dumpbin failed for production object: $objectPath"
    }
    $symbolText = $symbols -join "`n"
    foreach ($name in $forbiddenIdentifiers) {
        if (Test-Identifier -Text $symbolText -Identifier $name) {
            $hits.Add("$objectPath::$name")
        }
    }
    foreach ($prefix in $forbiddenPrefixes) {
        if (Test-IdentifierPrefix -Text $symbolText -Prefix $prefix) {
            $hits.Add("$objectPath::$prefix*")
        }
    }
}

if ($hits.Count -ne 0) {
    throw "production object contains test instrumentation: $($hits -join ', ')"
}

if (-not (Test-Path -LiteralPath $CompilerCommandTlog -PathType Leaf)) {
    throw "production compiler command tlog is missing: $CompilerCommandTlog"
}
$compilerCommands = Get-Content `
    -LiteralPath $CompilerCommandTlog `
    -Encoding Unicode

function Get-ProductionCompileArguments {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Source
    )

    $sourcePath = [System.IO.Path]::GetFullPath($Source)
    $marker = "^$($sourcePath.ToUpperInvariant())"
    for ($index = 0;
         $index + 1 -lt $compilerCommands.Count;
         ++$index) {
        if ($compilerCommands[$index] -ieq $marker) {
            return $compilerCommands[$index + 1]
        }
    }
    throw "production compiler command is missing for source: $sourcePath"
}

$preprocessSpecs = @(
    @{
        Name = 'win_file'
        Source = $WinFileSource
        Forbidden = @(
            'WinFileStats',
            'stats_',
            'create_file_calls',
            'read_calls',
            'seek_calls',
            'size_queries',
            'IoBoundary',
            'IoBoundaryHook',
            'RunHook',
            'SetIoHook',
            'hook_',
            'hook_context_',
            'fail_next_win_file_allocation',
            'WinFileTestFailNextAllocation'
        )
        ForbiddenPrefixes = @(
            'WinFileTest'
        )
    },
    @{
        Name = 'media_session'
        Source = $MediaSessionSource
        Forbidden = @(
            'hash_runs_',
            'hash_cached_',
            'test_hash_failure_',
            'MediaSessionTest',
            'SetMediaSessionTestHashFailure',
            'RunSessionTestProtocolHooks',
            'session_test_first_hook',
            'session_test_first_context',
            'session_test_second_hook',
            'session_test_second_context',
            'fail_next_post_claim'
        )
        ForbiddenPrefixes = @(
            'MediaSessionTest'
        )
        GenericTestIdentifierSources = @(
            $MediaSessionSource,
            $MediaSessionHeader
        )
    }
)

$preprocessedCount = 0
foreach ($spec in $preprocessSpecs) {
    $preprocessedPath = Join-Path `
        ([System.IO.Path]::GetTempPath()) `
        ("videocore-production-{0}-{1}.i" -f `
            $spec.Name, `
            [guid]::NewGuid().ToString('N'))
    try {
        $compileArguments =
            Get-ProductionCompileArguments -Source $spec.Source
        $command = @(
            "call `"$VsDevCmd`" -no_logo -arch=x64 -host_arch=x64 >nul",
            "&& `"$Compiler`"",
            $compileArguments,
            '/P',
            "/Fi`"$preprocessedPath`""
        ) -join ' '
        & $env:ComSpec /d /s /c $command
        if ($LASTEXITCODE -ne 0) {
            throw "production preprocessing failed for $($spec.Name) with exit code $LASTEXITCODE"
        }
        if (-not (Test-Path -LiteralPath $preprocessedPath -PathType Leaf)) {
            throw "production preprocessing did not create output for $($spec.Name)"
        }
        $preprocessed = Get-Content -Raw -LiteralPath $preprocessedPath
        $preprocessedHits =
            [System.Collections.Generic.HashSet[string]]::new(
                [System.StringComparer]::Ordinal)
        foreach ($name in $spec.Forbidden) {
            if (Test-Identifier `
                    -Text $preprocessed `
                    -Identifier $name) {
                [void]$preprocessedHits.Add($name)
            }
        }
        foreach ($prefix in $spec.ForbiddenPrefixes) {
            if (Test-IdentifierPrefix `
                    -Text $preprocessed `
                    -Prefix $prefix) {
                [void]$preprocessedHits.Add("$prefix*")
            }
        }
        if ($spec.GenericTestIdentifierSources.Count -ne 0) {
            $projectText = Get-PreprocessedProjectText `
                -Text $preprocessed `
                -Sources $spec.GenericTestIdentifierSources
            foreach ($name in (Get-GenericTestIdentifiers `
                    -Text $projectText)) {
                [void]$preprocessedHits.Add($name)
            }
        }
        if ($preprocessedHits.Count -ne 0) {
            $sortedHits = $preprocessedHits | Sort-Object
            throw "production preprocessed source contains $($spec.Name) test instrumentation: $($sortedHits -join ', ')"
        }
        ++$preprocessedCount
    } finally {
        if (Test-Path -LiteralPath $preprocessedPath) {
            Remove-Item -LiteralPath $preprocessedPath -Force
        }
    }
}

Write-Output `
    "PRODUCTION_ISOLATION PASS objects=$($objectPaths.Count) symbols=0 preprocessed=$preprocessedCount parameters=tlog-exact generic=source-local"
