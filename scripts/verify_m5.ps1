[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$Go,
    [Parameter(Mandatory)][string]$GCC,
    [Parameter(Mandatory)][string]$PGDSN
)

$ErrorActionPreference = 'Stop'
$repoRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
. (Join-Path $PSScriptRoot 'verify_m5_marker.ps1')

$requiredGates = @(Get-M5RequiredGates)
$runID = '{0}-{1}' -f (
    [DateTimeOffset]::Now.ToString('yyyyMMdd-HHmmss-fff')
), ([Guid]::NewGuid().ToString('N').Substring(0, 8))
$startedUTC = [DateTimeOffset]::UtcNow
$testMode = [string]$env:M5_TEST_MODE -ceq '1'
$testFailGate = [string]$env:M5_TEST_FAIL_GATE
$defaultEvidenceRoot = Join-Path $repoRoot '.superpowers\evidence'
$requestedEvidenceRoot = if (
    $testMode -and
    -not [string]::IsNullOrWhiteSpace([string]$env:M5_TEST_EVIDENCE_ROOT)
) {
    [string]$env:M5_TEST_EVIDENCE_ROOT
}
else {
    $defaultEvidenceRoot
}

function Protect-M5Secret {
    param([AllowEmptyString()][string]$Text)
    if ($null -eq $Text) {
        return ''
    }
    $safe = $Text
    if (-not [string]::IsNullOrEmpty($PGDSN)) {
        $safe = $safe.Replace($PGDSN, '[REDACTED_DSN]')
    }
    $safe = [regex]::Replace(
        $safe,
        'postgres(?:ql)?://[^/\s:@"'']+:[^@\s/"'']+@',
        'postgres://[REDACTED]@',
        [System.Text.RegularExpressions.RegexOptions]::IgnoreCase
    )
    return [regex]::Replace(
        $safe,
        '(?i)(confirm_token\s*["=:]\s*)[A-Za-z0-9_-]{16,}',
        '$1[REDACTED_TOKEN]'
    )
}

function New-M5SafeDirectory {
    param(
        [Parameter(Mandatory)][string]$WorkspaceRoot,
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$Label,
        [switch]$RequireAbsent
    )
    $workspace = ConvertTo-M5LexicalLocalPath `
        -Path $WorkspaceRoot `
        -Label 'workspace root'
    $full = ConvertTo-M5LexicalLocalPath -Path $Path -Label $Label
    Assert-M5LexicalPathWithin `
        -FullRoot $workspace `
        -FullPath $full `
        -Label $Label
    Assert-M5ExistingPathHasNoReparsePoint `
        -FullRoot $workspace `
        -FullPath $full `
        -Label $Label
    if ($RequireAbsent -and (
        [System.IO.Directory]::Exists($full) -or
        [System.IO.File]::Exists($full)
    )) {
        throw "$Label already exists."
    }

    $workspacePrefix = $workspace.TrimEnd('\')
    $relative = $full.Substring($workspacePrefix.Length).TrimStart('\')
    $current = $workspacePrefix
    foreach ($component in @($relative -split '\\')) {
        if ([string]::IsNullOrEmpty($component)) {
            continue
        }
        $current = Join-Path $current $component
        if (-not [System.IO.Directory]::Exists($current) -and
            -not [System.IO.File]::Exists($current)) {
            [System.IO.Directory]::CreateDirectory($current) | Out-Null
        }
        $attributes = [System.IO.File]::GetAttributes($current)
        if (($attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw "$Label contains a reparse point."
        }
        if (($attributes -band [System.IO.FileAttributes]::Directory) -eq 0) {
            throw "$Label contains a non-directory ancestor."
        }
    }
    return $full
}

function Get-M5SafeWorkspaceFile {
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$Label
    )
    $full = Get-M5WorkspacePath `
        -WorkspaceRoot $repoRoot `
        -Path $Path `
        -Label $Label
    if (-not [System.IO.File]::Exists($full)) {
        throw "$Label must be an existing file."
    }
    return $full
}

function Resolve-M5RequiredTool {
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$Label
    )
    $full = ConvertTo-M5LexicalLocalPath -Path $Path -Label $Label
    $driveRoot = [System.IO.Path]::GetPathRoot($full)
    Assert-M5ExistingPathHasNoReparsePoint `
        -FullRoot $driveRoot `
        -FullPath $full `
        -Label $Label
    if (-not [System.IO.File]::Exists($full)) {
        throw "$Label must be an existing file."
    }
    return $full
}

function Resolve-M5RequiredDirectory {
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$Label
    )
    $full = ConvertTo-M5LexicalLocalPath -Path $Path -Label $Label
    $driveRoot = [System.IO.Path]::GetPathRoot($full)
    Assert-M5ExistingPathHasNoReparsePoint `
        -FullRoot $driveRoot `
        -FullPath $full `
        -Label $Label
    if (-not [System.IO.Directory]::Exists($full)) {
        throw "$Label must be an existing directory."
    }
    return $full
}

function Resolve-M5DiscoveredTool {
    param(
        [Parameter(Mandatory)][string]$Command,
        [Parameter(Mandatory)][string]$Label,
        [string[]]$Candidates = @()
    )
    $found = Get-Command $Command -CommandType Application `
        -ErrorAction SilentlyContinue
    if ($null -ne $found) {
        return Resolve-M5RequiredTool -Path $found.Source -Label $Label
    }
    foreach ($candidate in $Candidates) {
        if ([string]::IsNullOrWhiteSpace($candidate)) {
            continue
        }
        $lexical = ConvertTo-M5LexicalLocalPath `
            -Path $candidate `
            -Label $Label
        if ([System.IO.File]::Exists($lexical)) {
            return Resolve-M5RequiredTool -Path $lexical -Label $Label
        }
    }
    throw "$Label was not found."
}

function Get-M5WindowsSDKMT {
    $kitsRoot = ConvertTo-M5LexicalLocalPath `
        -Path 'C:\Program Files (x86)\Windows Kits\10\bin' `
        -Label 'Windows SDK bin root'
    if (-not [System.IO.Directory]::Exists($kitsRoot)) {
        throw 'Windows SDK bin root was not found.'
    }
    $candidates = @(
        Get-ChildItem -LiteralPath $kitsRoot -Directory |
            Where-Object {
                $version = [Version]::new()
                [Version]::TryParse($_.Name, [ref]$version)
            } |
            Sort-Object { [Version]$_.Name } -Descending |
            ForEach-Object { Join-Path $_.FullName 'x64\mt.exe' }
    )
    foreach ($candidate in $candidates) {
        $lexical = ConvertTo-M5LexicalLocalPath `
            -Path $candidate `
            -Label 'mt'
        if ([System.IO.File]::Exists($lexical)) {
            return Resolve-M5RequiredTool -Path $lexical -Label 'mt'
        }
    }
    throw 'the newest installed Windows SDK has no x64 mt.exe.'
}

function Get-M5ToolOutput {
    param(
        [Parameter(Mandatory)][string]$Executable,
        [Parameter(Mandatory)][string[]]$Arguments
    )
    $output = @(& $Executable @Arguments 2>&1)
    $exitCode = $LASTEXITCODE
    if ($exitCode -ne 0) {
        throw 'tool version query failed.'
    }
    return (Protect-M5Secret (($output | ForEach-Object {
        [string]$_
    }) -join "`n")).Trim()
}

function Save-M5Environment {
    $snapshot = @{}
    foreach ($item in Get-ChildItem Env:) {
        $snapshot[$item.Name] = [string]$item.Value
    }
    return $snapshot
}

function Restore-M5Environment {
    param([Parameter(Mandatory)][hashtable]$Snapshot)
    foreach ($item in @(Get-ChildItem Env:)) {
        if (-not $Snapshot.ContainsKey($item.Name)) {
            Remove-Item -LiteralPath "Env:$($item.Name)" `
                -ErrorAction SilentlyContinue
        }
    }
    foreach ($name in $Snapshot.Keys) {
        Set-Item -LiteralPath "Env:$name" -Value $Snapshot[$name]
    }
}

function Get-M5OwnedGoFiles {
    param([Parameter(Mandatory)][string]$Root)
    $rootFull = ConvertTo-M5LexicalLocalPath `
        -Path $Root `
        -Label 'source root'
    $queue = [System.Collections.Generic.Queue[string]]::new()
    $queue.Enqueue($rootFull)
    $files = [System.Collections.Generic.List[string]]::new()
    $excludedNames = [System.Collections.Generic.HashSet[string]]::new(
        [System.StringComparer]::OrdinalIgnoreCase
    )
    foreach ($name in @(
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
        [void]$excludedNames.Add($name)
    }
    while ($queue.Count -ne 0) {
        $directory = $queue.Dequeue()
        Assert-M5ExistingPathHasNoReparsePoint `
            -FullRoot $rootFull `
            -FullPath $directory `
            -Label 'Go source directory'
        foreach ($entry in [System.IO.Directory]::EnumerateFileSystemEntries(
            $directory
        )) {
            $full = ConvertTo-M5LexicalLocalPath `
                -Path $entry `
                -Label 'Go source entry'
            Assert-M5LexicalPathWithin `
                -FullRoot $rootFull `
                -FullPath $full `
                -Label 'Go source entry'
            $attributes = [System.IO.File]::GetAttributes($full)
            if (($attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
                continue
            }
            if (($attributes -band [System.IO.FileAttributes]::Directory) -ne 0) {
                if (-not $excludedNames.Contains(
                    [System.IO.Path]::GetFileName($full)
                )) {
                    $queue.Enqueue($full)
                }
            }
            elseif ([System.IO.Path]::GetExtension($full) -ceq '.go') {
                $files.Add($full)
            }
        }
    }
    return [string[]]@($files | Sort-Object)
}

function Assert-M5NamedPasses {
    param(
        [Parameter(Mandatory)][string]$Output,
        [Parameter(Mandatory)][string[]]$Names
    )
    if ($Output -match '(?m)^\s*--- SKIP:') {
        throw 'named acceptance output contains a skipped test.'
    }
    foreach ($name in $Names) {
        if (-not $Output.Contains("=== RUN   $name") -or
            -not $Output.Contains("--- PASS: $name")) {
            throw "named test lacks RUN/PASS proof: $name"
        }
    }
}

$environmentBefore = Save-M5Environment
$locationBefore = Get-Location
$evidenceDir = ''
$summaryPath = ''
$markerPath = ''
$failure = $null
$gates = [ordered]@{}
foreach ($name in $requiredGates) {
    $gates[$name] = [ordered]@{
        status = 'NOT_RUN'
        exit_code = $null
        command = ''
        log = ''
        started_utc = $null
        ended_utc = $null
    }
}
$toolNames = @(
    'go',
    'gcc',
    'gofmt',
    'pwsh',
    'windres',
    'dlltool',
    'cmake',
    'ctest',
    'mt',
    'docker',
    'subst',
    'vcpkg_root'
)
$tools = [ordered]@{}
foreach ($name in $toolNames) {
    $tools[$name] = [ordered]@{ path = ''; version = '' }
}
$postgresql = [ordered]@{ host = ''; database = '' }
$artifacts = [ordered]@{}
$tcResults = @()
$secondWindowsStatus = ''
$secondWindows = [ordered]@{
    configured_host = 'codex-192-168-1-6'
    reported_host = ''
    status = ''
    evidence = ''
    sha256 = ''
}
$reviews = [ordered]@{
    critical_open = -1
    important_open = -1
    findings = @('review evidence not loaded')
    sources = @()
}
$protectedMediaAccessCount = -1
$residue = [ordered]@{
    schema = -1
    process = -1
    pipe = -1
    subst = -1
    junction = -1
    handle = -1
    test_root = -1
}
$goPath = ''
$gccPath = ''
$gofmtPath = ''
$pwshPath = ''
$windresPath = ''
$dlltoolPath = ''
$cmakePath = ''
$ctestPath = ''
$mtPath = ''
$dockerPath = ''
$substPath = ''
$vcpkgRootPath = ''
$stageDir = ''
$e2eMatrixPath = ''
$e2eCleanupPath = ''

function Resolve-M5EvidenceOutputPath {
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$Label
    )
    $full = ConvertTo-M5LexicalLocalPath -Path $Path -Label $Label
    Assert-M5LexicalPathWithin `
        -FullRoot $evidenceDir `
        -FullPath $full `
        -Label $Label
    Assert-M5ExistingPathHasNoReparsePoint `
        -FullRoot $evidenceDir `
        -FullPath $full `
        -Label $Label
    return $full
}

function Write-M5EvidenceText {
    param(
        [Parameter(Mandatory)][string]$Path,
        [AllowEmptyString()][string]$Text
    )
    $full = Resolve-M5EvidenceOutputPath `
        -Path $Path `
        -Label 'evidence output'
    [System.IO.File]::WriteAllText(
        $full,
        (Protect-M5Secret $Text),
        [System.Text.UTF8Encoding]::new($false)
    )
    Assert-M5ExistingPathHasNoReparsePoint `
        -FullRoot $evidenceDir `
        -FullPath $full `
        -Label 'evidence output'
}

function Invoke-M5Command {
    param(
        [Parameter(Mandatory)][string]$Executable,
        [Parameter(Mandatory)][string[]]$Arguments
    )
    $output = @(& $Executable @Arguments 2>&1)
    $exitCode = $LASTEXITCODE
    $safe = Protect-M5Secret (($output | ForEach-Object {
        [string]$_
    }) -join "`r`n")
    if ($exitCode -ne 0) {
        throw "external command failed with exit code $exitCode`n$safe"
    }
    return $safe
}

function Invoke-M5Gate {
    param(
        [Parameter(Mandatory)][string]$Name,
        [Parameter(Mandatory)][string]$Command,
        [Parameter(Mandatory)][scriptblock]$Action
    )
    $logPath = Resolve-M5EvidenceOutputPath `
        -Path (Join-Path $evidenceDir "$Name.log") `
        -Label "$Name gate log"
    $started = [DateTimeOffset]::UtcNow.ToString('o')
    try {
        if ($testMode -and $testFailGate -ceq $Name) {
            throw "injected M5 gate failure: $Name"
        }
        $value = & $Action
        $text = if ($value -is [string]) {
            $value
        }
        else {
            $value | ConvertTo-Json -Depth 24
        }
        Write-M5EvidenceText -Path $logPath -Text ([string]$text)
        $gates[$Name] = [ordered]@{
            status = 'PASS'
            exit_code = 0
            command = $Command
            log = $logPath
            started_utc = $started
            ended_utc = [DateTimeOffset]::UtcNow.ToString('o')
        }
        return $value
    }
    catch {
        $message = Protect-M5Secret ([string]$_.Exception.Message)
        Write-M5EvidenceText -Path $logPath -Text $message
        $gates[$Name] = [ordered]@{
            status = 'FAIL'
            exit_code = 1
            command = $Command
            log = $logPath
            started_utc = $started
            ended_utc = [DateTimeOffset]::UtcNow.ToString('o')
        }
        throw
    }
}

function Get-M5SHA256 {
    param([Parameter(Mandatory)][string]$Path)
    return (
        Get-FileHash -LiteralPath $Path -Algorithm SHA256
    ).Hash.ToLowerInvariant()
}

function Test-M5ObjectHasFalseBoolean {
    param([AllowNull()][object]$Value)
    if ($null -eq $Value) {
        return $false
    }
    if ($Value -is [bool]) {
        return -not $Value
    }
    if ($Value -is [System.Collections.IDictionary]) {
        foreach ($entry in $Value.GetEnumerator()) {
            if (Test-M5ObjectHasFalseBoolean -Value $entry.Value) {
                return $true
            }
        }
        return $false
    }
    if ($Value -is [pscustomobject]) {
        foreach ($property in $Value.PSObject.Properties) {
            if (Test-M5ObjectHasFalseBoolean -Value $property.Value) {
                return $true
            }
        }
        return $false
    }
    if ($Value -is [System.Collections.IEnumerable] -and
        $Value -isnot [string]) {
        foreach ($item in $Value) {
            if (Test-M5ObjectHasFalseBoolean -Value $item) {
                return $true
            }
        }
    }
    return $false
}

function Get-M5EvidenceTextFiles {
    $queue = [System.Collections.Generic.Queue[string]]::new()
    $queue.Enqueue($evidenceDir)
    $files = [System.Collections.Generic.List[string]]::new()
    while ($queue.Count -ne 0) {
        $directory = $queue.Dequeue()
        Assert-M5ExistingPathHasNoReparsePoint `
            -FullRoot $evidenceDir `
            -FullPath $directory `
            -Label 'secret scan directory'
        foreach ($entry in [System.IO.Directory]::EnumerateFileSystemEntries(
            $directory
        )) {
            $full = Resolve-M5EvidenceOutputPath `
                -Path $entry `
                -Label 'secret scan entry'
            $attributes = [System.IO.File]::GetAttributes($full)
            if (($attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
                throw 'secret scan found a reparse point.'
            }
            if (($attributes -band [System.IO.FileAttributes]::Directory) -ne 0) {
                $queue.Enqueue($full)
            }
            elseif ([System.IO.Path]::GetExtension($full) -in @(
                '.log', '.json', '.txt', '.md'
            ) -and [System.IO.Path]::GetFileName($full) -cne
                'secret_scan.log') {
                $files.Add($full)
            }
        }
    }
    return [string[]]@($files)
}

function Invoke-M5SecretScan {
    $findings = [System.Collections.Generic.List[string]]::new()
    $files = @(Get-M5EvidenceTextFiles)
    $protectedRoots = @(Get-M5ProtectedLexicalRoots)
    foreach ($file in $files) {
        $text = [System.IO.File]::ReadAllText($file)
        $found = (
            (-not [string]::IsNullOrEmpty($PGDSN) -and
                $text.Contains($PGDSN)) -or
            $text -match 'postgres(?:ql)?://[^/\s:@"'']+:[^@\s/"'']+@' -or
            $text -match '(?i)"?confirm_token"?\s*[:=]\s*"[A-Za-z0-9_-]{16,}"'
        )
        if (-not $found) {
            foreach ($protected in $protectedRoots) {
                if ($text.IndexOf(
                    $protected,
                    [System.StringComparison]::OrdinalIgnoreCase
                ) -ge 0) {
                    $found = $true
                    break
                }
            }
        }
        if ($found) {
            $findings.Add($file)
        }
    }
    if ($findings.Count -ne 0) {
        throw (
            'secret/protected-path scan found unsafe evidence files: ' +
            (($findings | ForEach-Object {
                [System.IO.Path]::GetFileName($_)
            }) -join ',')
        )
    }
    return [ordered]@{
        files_scanned = $files.Count
        findings = 0
    }
}

function Write-M5TerminalEvidence {
    param(
        [Parameter(Mandatory)][string]$Status,
        [AllowNull()][object]$FailureText
    )
    $summary = [ordered]@{
        schema_version = 1
        run_id = $runID
        started_utc = $startedUTC.ToString('o')
        ended_utc = [DateTimeOffset]::UtcNow.ToString('o')
        status = $Status
        git_status = 'NO_REPOSITORY'
        required_gates = $requiredGates
        gates = $gates
        tools = $tools
        postgresql = $postgresql
        artifacts = $artifacts
        second_windows_status = $secondWindowsStatus
        second_windows = $secondWindows
        tc = $tcResults
        reviews = $reviews
        protected_media_access_count = $protectedMediaAccessCount
        residue = $residue
        failure = if ($null -eq $FailureText) {
            $null
        }
        else {
            Protect-M5Secret ([string]$FailureText)
        }
    }
    Write-M5EvidenceText `
        -Path $summaryPath `
        -Text ($summary | ConvertTo-Json -Depth 40)
    return $summary
}

function Import-M5ReviewEvidence {
    param([Parameter(Mandatory)][string]$Path)
    $full = Get-M5WorkspacePath `
        -WorkspaceRoot $repoRoot `
        -Path $Path `
        -Label 'review evidence'
    if (-not [System.IO.File]::Exists($full)) {
        throw 'review evidence must be an existing JSON file.'
    }
    try {
        $value = [System.IO.File]::ReadAllText($full) | ConvertFrom-Json
    }
    catch {
        throw 'review evidence is not valid JSON.'
    }
    Assert-M5ExactProperties `
        -Object $value `
        -Names @('critical_open', 'important_open', 'findings') `
        -Path 'review evidence'
    foreach ($name in @('critical_open', 'important_open')) {
        $count = Get-M5RequiredValue `
            -Object $value `
            -Name $name `
            -Path 'review evidence'
        Assert-M5NativeInteger -Value $count -Path "review evidence.$name"
        if ([int64]$count -lt 0) {
            throw "review evidence.$name must be non-negative."
        }
    }
    $findingValues = @(
        Get-M5RequiredValue `
            -Object $value `
            -Name 'findings' `
            -Path 'review evidence'
    )
    foreach ($finding in $findingValues) {
        if ($finding -isnot [string] -or
            [string]::IsNullOrWhiteSpace($finding)) {
            throw 'review findings must be non-empty strings.'
        }
    }
    $copyPath = Join-Path $evidenceDir 'reviews.json'
    Write-M5EvidenceText `
        -Path $copyPath `
        -Text ($value | ConvertTo-Json -Depth 12)
    $script:reviews = [ordered]@{
        critical_open = [int64]$value.critical_open
        important_open = [int64]$value.important_open
        findings = [string[]]@($findingValues)
        sources = @($copyPath)
    }
    if ([int64]$value.critical_open -ne 0 -or
        [int64]$value.important_open -ne 0) {
        throw 'Critical or Important review findings remain open.'
    }
}

function Import-M5SecondWindowsFreezeEvidence {
    $manifestPath = Get-M5SafeWorkspaceFile `
        -Path (Join-Path $repoRoot (
            '.superpowers\sdd\2026-07-29-m5-delete\' +
            'task-12-freeze-manifest.json'
        )) `
        -Label 'Task 12 freeze manifest'
    try {
        $manifest = [System.IO.File]::ReadAllText($manifestPath) |
            ConvertFrom-Json
    }
    catch {
        throw 'Task 12 freeze manifest is not valid JSON.'
    }
    if ($manifest.no_repository -isnot [bool] -or
        -not $manifest.no_repository) {
        throw 'Task 12 freeze manifest is not NO_REPOSITORY evidence.'
    }

    $sourceProperties = @($manifest.source_sha256.PSObject.Properties)
    if ($sourceProperties.Count -ne 3) {
        throw 'Task 12 freeze manifest must bind exactly three source files.'
    }
    foreach ($property in $sourceProperties) {
        $sourcePath = Get-M5WorkspacePath `
            -WorkspaceRoot $repoRoot `
            -Path (Join-Path $repoRoot $property.Name) `
            -Label 'Task 12 frozen source'
        if (-not [System.IO.File]::Exists($sourcePath)) {
            throw 'Task 12 frozen source is missing.'
        }
        if ((Get-M5SHA256 $sourcePath) -cne [string]$property.Value) {
            throw 'Task 12 frozen source hash drifted.'
        }
    }

    $remotePath = ''
    $remoteExpectedHash = ''
    $evidenceEntries = @($manifest.evidence)
    if ($evidenceEntries.Count -eq 0) {
        throw 'Task 12 freeze manifest has no evidence entries.'
    }
    foreach ($entry in $evidenceEntries) {
        $entryPath = Get-M5WorkspacePath `
            -WorkspaceRoot $repoRoot `
            -Path (Join-Path $repoRoot ([string]$entry.path)) `
            -Label 'Task 12 frozen evidence'
        if (-not [System.IO.File]::Exists($entryPath)) {
            throw 'Task 12 frozen evidence is missing.'
        }
        $actualHash = Get-M5SHA256 $entryPath
        if ($actualHash -cne [string]$entry.sha256) {
            throw 'Task 12 frozen evidence hash drifted.'
        }
        if ([System.IO.Path]::GetFileName($entryPath) -match
            '^m5-remote-tc10-[a-f0-9]{32}\.json$') {
            if (-not [string]::IsNullOrEmpty($remotePath)) {
                throw 'Task 12 freeze manifest has duplicate remote TC-10 evidence.'
            }
            $remotePath = $entryPath
            $remoteExpectedHash = $actualHash
        }
    }
    if ([string]::IsNullOrEmpty($remotePath)) {
        throw 'Task 12 freeze manifest lacks remote TC-10 evidence.'
    }

    try {
        $remote = [System.IO.File]::ReadAllText($remotePath) | ConvertFrom-Json
    }
    catch {
        throw 'remote TC-10 evidence is not valid JSON.'
    }
    if ([string]$remote.status -cne 'PASS' -or
        [string]$remote.second_windows_status -cne
            'VERIFIED_ON_SECOND_WINDOWS') {
        throw 'remote TC-10 status is not exact VERIFIED_ON_SECOND_WINDOWS.'
    }
    if ([string]::IsNullOrWhiteSpace([string]$remote.host)) {
        throw 'remote TC-10 evidence lacks the reported Windows host.'
    }
    if ($remote.protected_media_access_count -isnot [int64] -and
        $remote.protected_media_access_count -isnot [int32]) {
        throw 'remote TC-10 protected access count is not an integer.'
    }
    if ([int64]$remote.protected_media_access_count -ne 0) {
        throw 'remote TC-10 protected media access count is nonzero.'
    }
    if (Test-M5ObjectHasFalseBoolean -Value $remote.assertions) {
        throw 'remote TC-10 evidence contains a false assertion.'
    }
    if (@($remote.cleanup.process_residue).Count -ne 0 -or
        [int64]$remote.cleanup.pipe_residue -ne 0 -or
        [int64]$remote.cleanup.run_root_residue -ne 0 -or
        @($remote.cleanup.failures).Count -ne 0) {
        throw 'remote TC-10 cleanup evidence is not zero.'
    }

    $copyPath = Join-Path $evidenceDir 'second-windows-tc10.json'
    Write-M5EvidenceText `
        -Path $copyPath `
        -Text ([System.IO.File]::ReadAllText($remotePath))
    $copyHash = Get-M5SHA256 $copyPath
    if ($copyHash -cne $remoteExpectedHash) {
        throw 'copied remote TC-10 evidence hash changed.'
    }
    $script:secondWindowsStatus = 'VERIFIED_ON_SECOND_WINDOWS'
    $script:secondWindows = [ordered]@{
        configured_host = 'codex-192-168-1-6'
        reported_host = [string]$remote.host
        status = 'VERIFIED_ON_SECOND_WINDOWS'
        evidence = $copyPath
        sha256 = $copyHash
    }
}

function New-M5TestArtifacts {
    $script:stageDir = New-M5SafeDirectory `
        -WorkspaceRoot $repoRoot `
        -Path (Join-Path $evidenceDir 'fresh-bin') `
        -Label 'test fresh artifact directory'
    $script:artifacts = [ordered]@{}
    foreach ($name in @('agent', 'gui', 'helper')) {
        $path = Join-Path $script:stageDir "$name.exe"
        Write-M5EvidenceText -Path $path -Text "fresh-$name-$runID"
        $script:artifacts[$name] = [ordered]@{
            path = $path
            sha256 = Get-M5SHA256 $path
            fresh = $true
        }
    }
}

function New-M5TestE2EEvidence {
    $e2eDir = New-M5SafeDirectory `
        -WorkspaceRoot $repoRoot `
        -Path (Join-Path $evidenceDir 'm5-e2e') `
        -Label 'test E2E evidence directory'
    $script:e2eMatrixPath = Join-Path $e2eDir 'm5-test-tc-matrix.json'
    $script:e2eCleanupPath = Join-Path $e2eDir 'm5-test-cleanup.json'
    $tc = @(
        for ($index = 1; $index -le 12; $index++) {
            [ordered]@{
                id = 'TC-{0:D2}' -f $index
                status = 'PASSED'
                duration_ms = 1
                assertions = [ordered]@{ fixture_assertion = $true }
            }
        }
    )
    $matrix = [ordered]@{
        schema_version = 1
        run_id = 'm5test' + ([Guid]::NewGuid().ToString('N'))
        schema = 'm5_e2e_marker_fixture'
        pipe_name = '\\.\pipe\dedup-m5-marker-fixture'
        drive_letter = 'Z:'
        second_windows_status = 'VERIFIED_ON_SECOND_WINDOWS'
        tc = $tc
        access_ledger = @()
        protected_media_access_count = 0
        task_ids = @('11111111-1111-4111-8111-111111111111')
        component_pids = @(1234)
    }
    $cleanup = [ordered]@{
        schema_version = 1
        run_id = $matrix.run_id
        run_root_removed = $true
        drive_letter = 'Z:'
        subst_removed = $true
        schema = $matrix.schema
        schema_removed = $true
        pipe_removed = $true
        process_residue = @()
        junction_residue = 0
        handle_residue = 0
        directory_residue = 0
        failures = @()
    }
    Write-M5EvidenceText `
        -Path $script:e2eMatrixPath `
        -Text ($matrix | ConvertTo-Json -Depth 16)
    Write-M5EvidenceText `
        -Path $script:e2eCleanupPath `
        -Text ($cleanup | ConvertTo-Json -Depth 16)
    $remotePath = Join-Path $evidenceDir 'second-windows-tc10.json'
    Write-M5EvidenceText -Path $remotePath -Text (
        [ordered]@{
            status = 'PASS'
            second_windows_status = 'VERIFIED_ON_SECOND_WINDOWS'
            host = 'SECOND-WINDOWS-FIXTURE'
            protected_media_access_count = 0
        } | ConvertTo-Json -Depth 8
    )
    $script:secondWindowsStatus = 'VERIFIED_ON_SECOND_WINDOWS'
    $script:secondWindows = [ordered]@{
        configured_host = 'codex-192-168-1-6'
        reported_host = 'SECOND-WINDOWS-FIXTURE'
        status = 'VERIFIED_ON_SECOND_WINDOWS'
        evidence = $remotePath
        sha256 = Get-M5SHA256 $remotePath
    }
    $script:tcResults = @(
        for ($index = 1; $index -le 12; $index++) {
            [ordered]@{
                id = 'TC-{0:D2}' -f $index
                status = 'PASS'
                evidence = $script:e2eMatrixPath
            }
        }
    )
    $script:protectedMediaAccessCount = 0
}

function Set-M5ResidueFromCleanup {
    param([Parameter(Mandatory)][object]$Cleanup)
    if ($Cleanup.run_root_removed -isnot [bool] -or
        -not $Cleanup.run_root_removed -or
        $Cleanup.subst_removed -isnot [bool] -or
        -not $Cleanup.subst_removed -or
        $Cleanup.schema_removed -isnot [bool] -or
        -not $Cleanup.schema_removed -or
        $Cleanup.pipe_removed -isnot [bool] -or
        -not $Cleanup.pipe_removed) {
        throw 'E2E cleanup did not prove root/subst/schema/pipe removal.'
    }
    $values = [ordered]@{
        schema = 0
        process = @($Cleanup.process_residue).Count
        pipe = 0
        subst = 0
        junction = [int64]$Cleanup.junction_residue
        handle = [int64]$Cleanup.handle_residue
        test_root = [int64]$Cleanup.directory_residue
    }
    if (@($Cleanup.failures).Count -ne 0) {
        throw 'E2E cleanup recorded failures.'
    }
    foreach ($name in $values.Keys) {
        if ([int64]$values[$name] -ne 0) {
            throw "E2E cleanup residue is nonzero: $name"
        }
    }
    $script:residue = $values
}

function Invoke-M5RealE2E {
    $e2eDir = New-M5SafeDirectory `
        -WorkspaceRoot $repoRoot `
        -Path (Join-Path $evidenceDir 'm5-e2e') `
        -Label 'M5 E2E evidence directory'
    $e2eScript = Get-M5SafeWorkspaceFile `
        -Path (Join-Path $PSScriptRoot 'verify_m5_e2e.ps1') `
        -Label 'M5 E2E script'
    $oldStatus = Get-Item Env:M5_E2E_SECOND_WINDOWS_STATUS `
        -ErrorAction SilentlyContinue
    try {
        $env:M5_E2E_SECOND_WINDOWS_STATUS = 'VERIFIED_ON_SECOND_WINDOWS'
        $output = @(
            & $e2eScript `
                -Go $goPath `
                -PGDSN $PGDSN `
                -HelperExe $artifacts.helper.path `
                -AgentExe $artifacts.agent.path `
                -GUIExe $artifacts.gui.path `
                -EvidenceDir $e2eDir 2>&1
        )
    }
    finally {
        if ($null -eq $oldStatus) {
            Remove-Item Env:M5_E2E_SECOND_WINDOWS_STATUS `
                -ErrorAction SilentlyContinue
        }
        else {
            $env:M5_E2E_SECOND_WINDOWS_STATUS = [string]$oldStatus.Value
        }
    }
    $safeOutput = Protect-M5Secret (($output | ForEach-Object {
        [string]$_
    }) -join "`r`n")

    $matrixFiles = @(
        Get-ChildItem -LiteralPath $e2eDir -File |
            Where-Object { $_.Name -match '-tc-matrix\.json$' }
    )
    $cleanupFiles = @(
        Get-ChildItem -LiteralPath $e2eDir -File |
            Where-Object { $_.Name -match '-cleanup\.json$' }
    )
    if ($matrixFiles.Count -ne 1 -or $cleanupFiles.Count -ne 1) {
        throw 'M5 E2E did not produce exactly one matrix and cleanup JSON.'
    }
    $script:e2eMatrixPath = Get-M5EvidenceFile `
        -EvidenceDir $evidenceDir `
        -Path $matrixFiles[0].FullName `
        -Label 'M5 E2E matrix'
    $script:e2eCleanupPath = Get-M5EvidenceFile `
        -EvidenceDir $evidenceDir `
        -Path $cleanupFiles[0].FullName `
        -Label 'M5 E2E cleanup'
    try {
        $matrix = [System.IO.File]::ReadAllText($script:e2eMatrixPath) |
            ConvertFrom-Json
        $cleanup = [System.IO.File]::ReadAllText($script:e2eCleanupPath) |
            ConvertFrom-Json
    }
    catch {
        throw 'M5 E2E matrix or cleanup is not valid JSON.'
    }
    if ([string]$matrix.second_windows_status -cne
        'VERIFIED_ON_SECOND_WINDOWS') {
        throw 'M5 E2E matrix did not prove exact second-Windows verification.'
    }
    if ([int64]$matrix.protected_media_access_count -ne 0) {
        throw 'M5 E2E protected media access count is nonzero.'
    }
    $matrixTC = @($matrix.tc)
    if ($matrixTC.Count -ne 12) {
        throw 'M5 E2E matrix does not contain exactly 12 cases.'
    }
    $normalizedTC = [System.Collections.Generic.List[object]]::new()
    for ($index = 1; $index -le 12; $index++) {
        $entry = $matrixTC[$index - 1]
        $expectedID = 'TC-{0:D2}' -f $index
        if ([string]$entry.id -cne $expectedID -or
            [string]$entry.status -cne 'PASSED' -or
            (Test-M5ObjectHasFalseBoolean -Value $entry.assertions)) {
            throw "M5 E2E case is not fully passing: $expectedID"
        }
        $normalizedTC.Add([ordered]@{
            id = $expectedID
            status = 'PASS'
            evidence = $script:e2eMatrixPath
        })
    }
    Set-M5ResidueFromCleanup -Cleanup $cleanup
    Import-M5SecondWindowsFreezeEvidence
    $script:tcResults = @($normalizedTC)
    $script:protectedMediaAccessCount = 0
    return $safeOutput
}

$bootstrapFailure = $null
try {
    $evidenceRoot = New-M5SafeDirectory `
        -WorkspaceRoot $repoRoot `
        -Path $requestedEvidenceRoot `
        -Label 'M5 evidence root'
    $evidenceDir = New-M5SafeDirectory `
        -WorkspaceRoot $repoRoot `
        -Path (Join-Path $evidenceRoot "m5-$runID") `
        -Label 'M5 invocation evidence directory' `
        -RequireAbsent
    $summaryPath = Resolve-M5EvidenceOutputPath `
        -Path (Join-Path $evidenceDir 'm5-evidence.json') `
        -Label 'M5 terminal evidence'
    $markerPath = Resolve-M5EvidenceOutputPath `
        -Path (Join-Path $evidenceDir 'm5-complete.json') `
        -Label 'M5 completion marker'
}
catch {
    $bootstrapFailure = Protect-M5Secret ([string]$_.Exception.Message)
}

if ($null -ne $bootstrapFailure) {
    try {
        $fallbackRoot = New-M5SafeDirectory `
            -WorkspaceRoot $repoRoot `
            -Path (Join-Path $repoRoot '.superpowers\tmp') `
            -Label 'M5 bootstrap fallback root'
        $evidenceDir = New-M5SafeDirectory `
            -WorkspaceRoot $repoRoot `
            -Path (Join-Path $fallbackRoot "m5-bootstrap-$runID") `
            -Label 'M5 bootstrap fallback directory' `
            -RequireAbsent
        $summaryPath = Resolve-M5EvidenceOutputPath `
            -Path (Join-Path $evidenceDir 'm5-evidence.json') `
            -Label 'M5 fallback terminal evidence'
        $markerPath = Resolve-M5EvidenceOutputPath `
            -Path (Join-Path $evidenceDir 'm5-complete.json') `
            -Label 'M5 fallback completion marker'
        $failure = $bootstrapFailure
        [void](Write-M5TerminalEvidence `
            -Status 'FAIL' `
            -FailureText $failure)
        Write-Host (
            "M5 FINAL RESULT FAIL run_id=$runID " +
            "evidence=$summaryPath reason=$failure"
        )
    }
    catch {
        Write-Host (
            "M5 FINAL RESULT FAIL run_id=$runID evidence=- " +
            'reason=no safe terminal evidence path'
        )
    }
    exit 1
}

try {
    if ($testMode -and (
        [string]::IsNullOrWhiteSpace($testFailGate) -or
        $requiredGates -cnotcontains $testFailGate
    )) {
        throw (
            'M5 test mode requires exactly one named fail gate and ' +
            'cannot produce completion evidence.'
        )
    }
    if ([string]::IsNullOrWhiteSpace($PGDSN)) {
        throw '-PGDSN must be explicit and non-empty.'
    }
    try {
        $dsnURI = [Uri]$PGDSN
    }
    catch {
        throw '-PGDSN must be a PostgreSQL URI.'
    }
    if ($dsnURI.Scheme -cnotin @('postgres', 'postgresql') -or
        [string]::IsNullOrWhiteSpace($dsnURI.Host)) {
        throw '-PGDSN must be a PostgreSQL URI with a host.'
    }
    $database = [Uri]::UnescapeDataString($dsnURI.AbsolutePath.TrimStart('/'))
    if ([string]::IsNullOrWhiteSpace($database) -or
        $database -match '[:/@?]') {
        throw '-PGDSN must name one non-secret database.'
    }
    $postgresql = [ordered]@{
        host = $dsnURI.Host
        database = $database
    }
    $repositoryMarker = Get-M5WorkspacePath `
        -WorkspaceRoot $repoRoot `
        -Path (Join-Path $repoRoot '.git') `
        -Label 'repository marker'
    if ([System.IO.File]::Exists($repositoryMarker) -or
        [System.IO.Directory]::Exists($repositoryMarker)) {
        throw 'M5 controller requires the documented NO_REPOSITORY workspace.'
    }

    if ($testMode) {
        $goPath = Resolve-M5RequiredTool -Path $Go -Label '-Go'
        $gccPath = Resolve-M5RequiredTool -Path $GCC -Label '-GCC'
        $fixtureToolRoot = Split-Path -Parent $goPath
        $gofmtPath = Join-Path $fixtureToolRoot 'gofmt.exe'
        $pwshPath = (Get-Process -Id $PID).Path
        $windresPath = Join-Path $fixtureToolRoot 'windres.exe'
        $dlltoolPath = Join-Path $fixtureToolRoot 'dlltool.exe'
        $cmakePath = Join-Path $fixtureToolRoot 'cmake.exe'
        $ctestPath = Join-Path $fixtureToolRoot 'ctest.exe'
        $mtPath = Join-Path $fixtureToolRoot 'mt.exe'
        $dockerPath = Join-Path $fixtureToolRoot 'docker.exe'
        $substPath = Join-Path $fixtureToolRoot 'subst.exe'
        $vcpkgRootPath = Join-Path $fixtureToolRoot 'vcpkg'
        foreach ($entry in $tools.GetEnumerator()) {
            $path = switch ($entry.Key) {
                'go' { $goPath }
                'gcc' { $gccPath }
                'gofmt' { $gofmtPath }
                'pwsh' { $pwshPath }
                'windres' { $windresPath }
                'dlltool' { $dlltoolPath }
                'cmake' { $cmakePath }
                'ctest' { $ctestPath }
                'mt' { $mtPath }
                'docker' { $dockerPath }
                'subst' { $substPath }
                'vcpkg_root' { $vcpkgRootPath }
            }
            $entry.Value.path = $path
            $entry.Value.version = "M5_TEST_VERSION_$($entry.Key)"
        }
    }
    else {
        $goPath = Resolve-M5RequiredTool -Path $Go -Label '-Go'
        $gccPath = Resolve-M5RequiredTool -Path $GCC -Label '-GCC'
        $gofmtPath = Resolve-M5RequiredTool `
            -Path (Join-Path (Split-Path -Parent $goPath) 'gofmt.exe') `
            -Label 'gofmt'
        $windresPath = Resolve-M5RequiredTool `
            -Path (Join-Path (Split-Path -Parent $gccPath) 'windres.exe') `
            -Label 'windres'
        $dlltoolPath = Resolve-M5RequiredTool `
            -Path (Join-Path (Split-Path -Parent $gccPath) 'dlltool.exe') `
            -Label 'dlltool'
        $pwshPath = Resolve-M5RequiredTool `
            -Path (Get-Process -Id $PID).Path `
            -Label 'PowerShell host'
        $vcpkgRequested = if (
            -not [string]::IsNullOrWhiteSpace([string]$env:VCPKG_ROOT)
        ) {
            [string]$env:VCPKG_ROOT
        }
        else {
            'C:\vcpkg'
        }
        $vcpkgRootPath = Resolve-M5RequiredDirectory `
            -Path $vcpkgRequested `
            -Label 'vcpkg root'
        $cmakePath = Resolve-M5DiscoveredTool `
            -Command 'cmake.exe' `
            -Label 'cmake' `
            -Candidates @(
                (Join-Path $vcpkgRootPath (
                    'downloads\tools\cmake-4.2.3-windows\' +
                    'cmake-4.2.3-windows-x86_64\bin\cmake.exe'
                ))
            )
        $ctestPath = Resolve-M5RequiredTool `
            -Path (Join-Path (Split-Path -Parent $cmakePath) 'ctest.exe') `
            -Label 'ctest'
        $mtPath = Get-M5WindowsSDKMT
        $dockerPath = Resolve-M5DiscoveredTool `
            -Command 'docker.exe' `
            -Label 'docker' `
            -Candidates @(
                'C:\Program Files\Docker\Docker\resources\bin\docker.exe'
            )
        $substPath = Resolve-M5RequiredTool `
            -Path (Join-Path $env:SystemRoot 'System32\subst.exe') `
            -Label 'subst'

        $goVersion = Get-M5ToolOutput `
            -Executable $goPath `
            -Arguments @('version')
        $tools.go = [ordered]@{ path = $goPath; version = $goVersion }
        $tools.gcc = [ordered]@{
            path = $gccPath
            version = Get-M5ToolOutput `
                -Executable $gccPath `
                -Arguments @('--version')
        }
        $tools.gofmt = [ordered]@{
            path = $gofmtPath
            version = $goVersion
        }
        $tools.pwsh = [ordered]@{
            path = $pwshPath
            version = $PSVersionTable.PSVersion.ToString()
        }
        foreach ($spec in @(
            @('windres', $windresPath),
            @('dlltool', $dlltoolPath),
            @('cmake', $cmakePath),
            @('ctest', $ctestPath),
            @('docker', $dockerPath)
        )) {
            $tools[$spec[0]] = [ordered]@{
                path = $spec[1]
                version = Get-M5ToolOutput `
                    -Executable $spec[1] `
                    -Arguments @('--version')
            }
        }
        $tools.mt = [ordered]@{
            path = $mtPath
            version = [System.Diagnostics.FileVersionInfo]::GetVersionInfo(
                $mtPath
            ).FileVersion
        }
        $tools.subst = [ordered]@{
            path = $substPath
            version = [System.Diagnostics.FileVersionInfo]::GetVersionInfo(
                $substPath
            ).FileVersion
        }
        $toolchainPath = Get-M5WorkspacePath `
            -WorkspaceRoot $vcpkgRootPath `
            -Path (Join-Path $vcpkgRootPath (
                'scripts\buildsystems\vcpkg.cmake'
            )) `
            -Label 'vcpkg toolchain'
        if (-not [System.IO.File]::Exists($toolchainPath)) {
            throw 'vcpkg toolchain is missing.'
        }
        $tools.vcpkg_root = [ordered]@{
            path = $vcpkgRootPath
            version = 'toolchain_sha256=' + (Get-M5SHA256 $toolchainPath)
        }
    }

    $reviewPath = if (
        $testMode -and
        -not [string]::IsNullOrWhiteSpace(
            [string]$env:M5_REVIEW_EVIDENCE_PATH
        )
    ) {
        [string]$env:M5_REVIEW_EVIDENCE_PATH
    }
    else {
        Join-Path $repoRoot (
            '.superpowers\sdd\2026-07-29-m5-delete\' +
            'task-13-final-review.json'
        )
    }
    Import-M5ReviewEvidence -Path $reviewPath

    $env:PATH = (
        (Join-Path $repoRoot 'bin'),
        (Split-Path -Parent $gccPath),
        (Split-Path -Parent $goPath),
        [string]$environmentBefore['PATH']
    ) -join ';'
    $env:M5_CC = $gccPath
    $env:M5_WINDRES = $windresPath
    $env:M5_POWERSHELL = $pwshPath
    Set-Location -LiteralPath $repoRoot

    [void](Invoke-M5Gate `
        -Name 'format' `
        -Command 'gofmt -l <repo-owned Go files>' `
        -Action {
            if ($testMode) {
                $formatRoot = Get-M5WorkspacePath `
                    -WorkspaceRoot $repoRoot `
                    -Path ([string]$env:M5_TEST_FORMAT_ROOT) `
                    -Label 'test format root'
                if (-not [System.IO.Directory]::Exists($formatRoot)) {
                    throw 'test format root must be an existing directory.'
                }
                $files = @(Get-M5OwnedGoFiles -Root $formatRoot)
                return "formatted_files=$($files.Count)"
            }
            $files = @(Get-M5OwnedGoFiles -Root $repoRoot)
            $output = Invoke-M5Command `
                -Executable $gofmtPath `
                -Arguments (@('-l') + $files)
            if (-not [string]::IsNullOrWhiteSpace($output)) {
                throw "gofmt found unformatted files:`n$output"
            }
            return "formatted_files=$($files.Count)"
        })

    [void](Invoke-M5Gate `
        -Name 'pure_go_full' `
        -Command 'CGO_ENABLED=0 go test -count=1 ./...' `
        -Action {
            if ($testMode) {
                return 'M5 test pure_go_full PASS'
            }
            $env:CGO_ENABLED = '0'
            Remove-Item Env:CC -ErrorAction SilentlyContinue
            return Invoke-M5Command `
                -Executable $goPath `
                -Arguments @('-C', $repoRoot, 'test', '-count=1', './...')
        })

    [void](Invoke-M5Gate `
        -Name 'cgo_full' `
        -Command 'CGO_ENABLED=1 go test -count=1 ./...' `
        -Action {
            if ($testMode) {
                return 'M5 test cgo_full PASS'
            }
            $env:CGO_ENABLED = '1'
            $env:CC = $gccPath
            return Invoke-M5Command `
                -Executable $goPath `
                -Arguments @('-C', $repoRoot, 'test', '-count=1', './...')
        })

    [void](Invoke-M5Gate `
        -Name 'race_changed' `
        -Command 'go test -race -count=1 <M5 changed packages>' `
        -Action {
            if ($testMode) {
                return 'M5 test race_changed PASS'
            }
            $env:CGO_ENABLED = '1'
            $env:CC = $gccPath
            return Invoke-M5Command `
                -Executable $goPath `
                -Arguments @(
                    '-C', $repoRoot,
                    'test', '-race', '-count=1',
                    './internal/proto',
                    './internal/config',
                    './internal/helper',
                    './internal/store',
                    './internal/agent',
                    './internal/agent/delete',
                    './cmd/agent',
                    './internal/gui',
                    './cmd/gui',
                    './cmd/helper'
                )
        })

    [void](Invoke-M5Gate `
        -Name 'vet' `
        -Command 'go vet ./...' `
        -Action {
            if ($testMode) {
                return 'M5 test vet PASS'
            }
            $env:CGO_ENABLED = '1'
            $env:CC = $gccPath
            return Invoke-M5Command `
                -Executable $goPath `
                -Arguments @('-C', $repoRoot, 'vet', './...')
        })

    [void](Invoke-M5Gate `
        -Name 'helper_unit' `
        -Command 'go test -count=1 ./internal/helper ./cmd/helper' `
        -Action {
            if ($testMode) {
                return 'M5 test helper_unit PASS'
            }
            $env:CGO_ENABLED = '0'
            Remove-Item Env:CC -ErrorAction SilentlyContinue
            return Invoke-M5Command `
                -Executable $goPath `
                -Arguments @(
                    '-C', $repoRoot,
                    'test', '-count=1',
                    './internal/helper',
                    './cmd/helper'
                )
        })

    [void](Invoke-M5Gate `
        -Name 'manifest_audit' `
        -Command 'fresh build + mt.exe extracted Helper manifest audit' `
        -Action {
            if ($testMode) {
                New-M5TestArtifacts
                return [ordered]@{
                    fresh_artifacts = @($artifacts.Keys)
                    manifest_level = 'requireAdministrator'
                }
            }
            $buildStarted = [DateTimeOffset]::UtcNow
            $script:stageDir = Resolve-M5EvidenceOutputPath `
                -Path (Join-Path $evidenceDir 'fresh-bin') `
                -Label 'fresh build directory'
            if ([System.IO.Directory]::Exists($script:stageDir) -or
                [System.IO.File]::Exists($script:stageDir)) {
                throw 'fresh build directory already exists.'
            }
            $relativeOut = $script:stageDir.Substring(
                $repoRoot.TrimEnd('\').Length
            ).TrimStart('\')
            if ([string]::IsNullOrWhiteSpace($relativeOut)) {
                throw 'fresh build output cannot be the workspace root.'
            }
            $buildScript = Get-M5SafeWorkspaceFile `
                -Path (Join-Path $PSScriptRoot 'build.ps1') `
                -Label 'build script'
            $buildOutput = @(
                & $buildScript `
                    -Go $goPath `
                    -CC $gccPath `
                    -Windres $windresPath `
                    -Dlltool $dlltoolPath `
                    -OutDir $relativeOut `
                    -CMake $cmakePath `
                    -VcpkgRoot $vcpkgRootPath 2>&1
            )
            $safeBuildOutput = Protect-M5Secret (($buildOutput |
                ForEach-Object { [string]$_ }) -join "`r`n")
            Assert-M5ExistingPathHasNoReparsePoint `
                -FullRoot $evidenceDir `
                -FullPath $script:stageDir `
                -Label 'fresh build directory'

            $helperPath = Get-M5EvidenceFile `
                -EvidenceDir $evidenceDir `
                -Path (Join-Path $script:stageDir 'helper.exe') `
                -Label 'fresh helper'
            $manifestPath = Resolve-M5EvidenceOutputPath `
                -Path (Join-Path $evidenceDir (
                    'helper.extracted.manifest'
                )) `
                -Label 'extracted Helper manifest'
            $mtOutput = Invoke-M5Command `
                -Executable $mtPath `
                -Arguments @(
                    "-inputresource:$helperPath;#1",
                    "-out:$manifestPath"
                )
            $manifestFile = Get-M5EvidenceFile `
                -EvidenceDir $evidenceDir `
                -Path $manifestPath `
                -Label 'extracted Helper manifest'
            $manifestText = [System.IO.File]::ReadAllText($manifestFile)
            if ($manifestText -notmatch
                'requestedExecutionLevel\s+level="requireAdministrator"\s+uiAccess="false"') {
                throw 'fresh Helper manifest contract is not exact.'
            }
            $sysoPath = Get-M5WorkspacePath `
                -WorkspaceRoot $repoRoot `
                -Path (Join-Path $repoRoot (
                    'cmd\helper\rsrc_windows_amd64.syso'
                )) `
                -Label 'generated Helper resource'
            if ([System.IO.File]::Exists($sysoPath) -or
                [System.IO.Directory]::Exists($sysoPath)) {
                throw 'generated Helper .syso remains after the fresh build.'
            }

            $script:artifacts = [ordered]@{}
            foreach ($name in @('agent', 'gui', 'helper')) {
                $path = Get-M5EvidenceFile `
                    -EvidenceDir $evidenceDir `
                    -Path (Join-Path $script:stageDir "$name.exe") `
                    -Label "fresh $name artifact"
                $lastWrite = [System.IO.File]::GetLastWriteTimeUtc($path)
                if ($lastWrite -lt $buildStarted.UtcDateTime.AddSeconds(-2)) {
                    throw "fresh $name artifact predates this build."
                }
                $script:artifacts[$name] = [ordered]@{
                    path = $path
                    sha256 = Get-M5SHA256 $path
                    fresh = $true
                }
            }
            return (
                $safeBuildOutput,
                $mtOutput,
                'manifest_level=requireAdministrator',
                'manifest_uiAccess=false'
            ) -join "`r`n"
        })

    [void](Invoke-M5Gate `
        -Name 'pipe_acl' `
        -Command 'go test -v -count=1 -run ^TestPipe ./internal/helper' `
        -Action {
            if ($testMode) {
                return 'M5 test pipe_acl PASS'
            }
            $env:CGO_ENABLED = '0'
            Remove-Item Env:CC -ErrorAction SilentlyContinue
            return Invoke-M5Command `
                -Executable $goPath `
                -Arguments @(
                    '-C', $repoRoot,
                    'test', '-v', '-count=1',
                    '-run', '^TestPipe',
                    './internal/helper'
                )
        })

    [void](Invoke-M5Gate `
        -Name 'agent_forwarder' `
        -Command 'go test -race -count=1 ./internal/agent/delete' `
        -Action {
            if ($testMode) {
                return 'M5 test agent_forwarder PASS'
            }
            $env:CGO_ENABLED = '1'
            $env:CC = $gccPath
            return Invoke-M5Command `
                -Executable $goPath `
                -Arguments @(
                    '-C', $repoRoot,
                    'test', '-race', '-count=1',
                    './internal/agent/delete'
                )
        })

    [void](Invoke-M5Gate `
        -Name 'postgres_contracts' `
        -Command 'PG_DSN=<in-memory> go test -v M5 PostgreSQL contracts' `
        -Action {
            if ($testMode) {
                return 'M5 test postgres_contracts PASS'
            }
            $oldPG = Get-Item Env:PG_DSN -ErrorAction SilentlyContinue
            try {
                $env:PG_DSN = $PGDSN
                $output = Invoke-M5Command `
                    -Executable $goPath `
                    -Arguments @(
                        '-C', $repoRoot,
                        'test', '-v', '-count=1',
                        '-run',
                        '^TestDeletePrepare(ResolvesCanonicalMembersFromPostgres|RejectsWholePostgresSelectionOnAnyConflict)$',
                        './internal/gui'
                    )
            }
            finally {
                if ($null -eq $oldPG) {
                    Remove-Item Env:PG_DSN -ErrorAction SilentlyContinue
                }
                else {
                    $env:PG_DSN = [string]$oldPG.Value
                }
            }
            Assert-M5NamedPasses -Output $output -Names @(
                'TestDeletePrepareResolvesCanonicalMembersFromPostgres',
                'TestDeletePrepareRejectsWholePostgresSelectionOnAnyConflict'
            )
            return $output
        })

    [void](Invoke-M5Gate `
        -Name 'm5_e2e' `
        -Command (
            'scripts/verify_m5_e2e.ps1 ' +
            '-PGDSN <in-memory> -SECOND_WINDOWS VERIFIED'
        ) `
        -Action {
            if ($testMode) {
                New-M5TestE2EEvidence
                return 'M5 test m5_e2e PASS'
            }
            return Invoke-M5RealE2E
        })

    [void](Invoke-M5Gate `
        -Name 'public_unchanged' `
        -Command (
            'DEDUP_TEST_PG_DSN=<in-memory> go test -v ' +
            'public-schema snapshot contract'
        ) `
        -Action {
            if ($testMode) {
                return 'M5 test public_unchanged PASS'
            }
            $testName = (
                'TestPostgres16ScopedGroupRebuildSchemaTwiceCleanup' +
                'AndConcurrencyWhenEnabled'
            )
            $oldPG = Get-Item Env:DEDUP_TEST_PG_DSN `
                -ErrorAction SilentlyContinue
            try {
                $env:DEDUP_TEST_PG_DSN = $PGDSN
                $output = Invoke-M5Command `
                    -Executable $goPath `
                    -Arguments @(
                        '-C', $repoRoot,
                        'test', '-v', '-count=1',
                        '-run', "^$testName$",
                        './internal/phase2'
                    )
            }
            finally {
                if ($null -eq $oldPG) {
                    Remove-Item Env:DEDUP_TEST_PG_DSN `
                        -ErrorAction SilentlyContinue
                }
                else {
                    $env:DEDUP_TEST_PG_DSN = [string]$oldPG.Value
                }
            }
            Assert-M5NamedPasses -Output $output -Names @($testName)
            return $output
        })

    [void](Invoke-M5Gate `
        -Name 'secret_scan' `
        -Command 'internal evidence credential/token/protected-path scan' `
        -Action {
            return Invoke-M5SecretScan
        })

    [void](Invoke-M5Gate `
        -Name 'marker_negative' `
        -Command 'scripts/test_verify_m5_marker.ps1' `
        -Action {
            $markerTest = Get-M5SafeWorkspaceFile `
                -Path (Join-Path $PSScriptRoot (
                    'test_verify_m5_marker.ps1'
                )) `
                -Label 'M5 marker self-test'
            $safe = Invoke-M5Command `
                -Executable $pwshPath `
                -Arguments @(
                    '-NoLogo',
                    '-NoProfile',
                    '-File',
                    $markerTest
                )
            if ($safe -notmatch
                '(?m)^M5_MARKER_PATH_ORDER_PASS cases=2\r?$' -or
                $safe -notmatch
                '(?m)^M5_MARKER_NEGATIVE_PASS cases=21\r?$') {
                throw 'M5 marker path/order or negative matrix did not pass.'
            }
            return $safe
        })

    [void](Invoke-M5Gate `
        -Name 'cleanup_audit' `
        -Command 'internal exact M5 residue audit' `
        -Action {
            if ([string]::IsNullOrWhiteSpace($e2eCleanupPath)) {
                throw 'cleanup audit has no E2E cleanup evidence.'
            }
            $cleanupFile = Get-M5EvidenceFile `
                -EvidenceDir $evidenceDir `
                -Path $e2eCleanupPath `
                -Label 'cleanup audit JSON'
            try {
                $cleanup = [System.IO.File]::ReadAllText($cleanupFile) |
                    ConvertFrom-Json
            }
            catch {
                throw 'cleanup audit JSON is malformed.'
            }
            Set-M5ResidueFromCleanup -Cleanup $cleanup
            $sysoPath = Get-M5WorkspacePath `
                -WorkspaceRoot $repoRoot `
                -Path (Join-Path $repoRoot (
                    'cmd\helper\rsrc_windows_amd64.syso'
                )) `
                -Label 'generated Helper resource'
            if ([System.IO.File]::Exists($sysoPath) -or
                [System.IO.Directory]::Exists($sysoPath)) {
                throw 'cleanup audit found a generated Helper .syso.'
            }
            return $residue
        })
}
catch {
    $failure = Protect-M5Secret ([string]$_.Exception.Message)
}
finally {
    Set-Location -LiteralPath $locationBefore
    Restore-M5Environment -Snapshot $environmentBefore
}

if ($testMode -and $null -eq $failure) {
    $failure = 'M5 test mode can never produce completion evidence.'
}

$status = if ($null -eq $failure) { 'PASS' } else { 'FAIL' }
$summary = Write-M5TerminalEvidence `
    -Status $status `
    -FailureText $failure

if ($null -eq $failure) {
    try {
        $finalSecretResult = Invoke-M5SecretScan
        Write-M5EvidenceText `
            -Path $gates.secret_scan.log `
            -Text ($finalSecretResult | ConvertTo-Json -Depth 8)
        $summary = Write-M5TerminalEvidence `
            -Status 'PASS' `
            -FailureText $null
    }
    catch {
        $failure = Protect-M5Secret ([string]$_.Exception.Message)
        $gates.secret_scan.status = 'FAIL'
        $gates.secret_scan.exit_code = 1
        $gates.secret_scan.ended_utc = [DateTimeOffset]::UtcNow.ToString('o')
        Write-M5EvidenceText `
            -Path $gates.secret_scan.log `
            -Text $failure
        $summary = Write-M5TerminalEvidence `
            -Status 'FAIL' `
            -FailureText $failure
    }
}

if ($null -eq $failure) {
    $detailPath = if (
        $testMode -and
        -not [string]::IsNullOrWhiteSpace([string]$env:M5_TEST_DETAIL_PATH)
    ) {
        [string]$env:M5_TEST_DETAIL_PATH
    }
    else {
        Join-Path $repoRoot 'docs\details\M5-delete.md'
    }
    $planPath = if (
        $testMode -and
        -not [string]::IsNullOrWhiteSpace([string]$env:M5_TEST_PLAN_PATH)
    ) {
        [string]$env:M5_TEST_PLAN_PATH
    }
    else {
        Join-Path $repoRoot (
            'docs\superpowers\plans\2026-07-29-m5-delete.md'
        )
    }
    try {
        Write-M5CompletionMarker `
            -Evidence $summary `
            -EvidenceDir $evidenceDir `
            -WorkspaceRoot $repoRoot `
            -DetailPath $detailPath `
            -PlanPath $planPath `
            -MarkerPath $markerPath
    }
    catch {
        $failure = Protect-M5Secret ([string]$_.Exception.Message)
        if ([System.IO.File]::Exists($markerPath)) {
            try {
                [System.IO.File]::Delete($markerPath)
            }
            catch {
                $failure = (
                    "$failure; failed to remove the exact partial " +
                    'completion marker'
                )
            }
        }
        $summary = Write-M5TerminalEvidence `
            -Status 'FAIL' `
            -FailureText $failure
    }
}

foreach ($name in $requiredGates) {
    $gate = $gates[$name]
    $exitText = if ($null -eq $gate.exit_code) { '-' } else {
        [string]$gate.exit_code
    }
    $logText = if ([string]::IsNullOrEmpty([string]$gate.log)) {
        '-'
    }
    else {
        [string]$gate.log
    }
    Write-Host (
        "M5 GATE $name $($gate.status) exit=$exitText log=$logText"
    )
}
if ($null -ne $failure) {
    Write-Host (
        "M5 FINAL RESULT FAIL run_id=$runID " +
        "evidence=$summaryPath reason=$failure"
    )
    exit 1
}
Write-Host (
    "M5 FINAL RESULT PASS run_id=$runID evidence=$summaryPath"
)
exit 0
