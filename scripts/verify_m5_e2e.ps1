param(
    [Parameter(Mandatory)][string]$Go,
    [Parameter(Mandatory)][string]$PGDSN,
    [Parameter(Mandatory)][string]$HelperExe,
    [Parameter(Mandatory)][string]$AgentExe,
    [Parameter(Mandatory)][string]$GUIExe,
    [Parameter(Mandatory)][string]$EvidenceDir
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$script:M5ExternalPathProbeCount = 0
$script:M5PathProbeAudit = (
    [Environment]::GetEnvironmentVariable(
        'M5_E2E_PATH_PROBE_AUDIT',
        [EnvironmentVariableTarget]::Process
    ) -ceq '1'
)

function Throw-M5 {
    param(
        [Parameter(Mandatory)][string]$Code,
        [Parameter(Mandatory)][string]$Message
    )
    throw "$Code`: $Message"
}

function Get-M5SecondWindowsStatus {
    $value = [Environment]::GetEnvironmentVariable(
        'M5_E2E_SECOND_WINDOWS_STATUS',
        [EnvironmentVariableTarget]::Process
    )
    if ($value -cne 'VERIFIED_ON_SECOND_WINDOWS') {
        Throw-M5 `
            'SECOND_WINDOWS_STATUS_INVALID' `
            'second-Windows status must be exact VERIFIED_ON_SECOND_WINDOWS'
    }
    return $value
}

$secondWindowsStatus = Get-M5SecondWindowsStatus

function Get-M5AbsolutePath {
    param([Parameter(Mandatory)][string]$LiteralPath)
    try {
        return [System.IO.Path]::GetFullPath($LiteralPath)
    }
    catch {
        Throw-M5 'PATH_INVALID' 'a supplied path is not an absolute Windows path'
    }
}

function Test-M5PathEqual {
    param(
        [Parameter(Mandatory)][string]$Left,
        [Parameter(Mandatory)][string]$Right
    )
    return [string]::Equals(
        $Left,
        $Right,
        [System.StringComparison]::OrdinalIgnoreCase
    )
}

function Test-M5StrictChild {
    param(
        [Parameter(Mandatory)][string]$Parent,
        [Parameter(Mandatory)][string]$Child
    )
    $parentFull = Get-M5AbsolutePath $Parent
    $childFull = Get-M5AbsolutePath $Child
    $prefix = $parentFull.TrimEnd(
        [System.IO.Path]::DirectorySeparatorChar,
        [System.IO.Path]::AltDirectorySeparatorChar
    ) + [System.IO.Path]::DirectorySeparatorChar
    return (
        $childFull.StartsWith(
            $prefix,
            [System.StringComparison]::OrdinalIgnoreCase
        ) -and
        -not (Test-M5PathEqual $parentFull $childFull)
    )
}

function Throw-M5PathBoundary {
    param(
        [Parameter(Mandatory)][string]$Label,
        [Parameter(Mandatory)][string]$Reason
    )
    $message = "$Label path $Reason"
    if ($script:M5PathProbeAudit) {
        $message += "; probe_count=$script:M5ExternalPathProbeCount"
    }
    Throw-M5 'PATH_BOUNDARY_INVALID' $message
}

function Assert-M5ExternalPathLexical {
    param(
        [Parameter(Mandatory)][string]$LiteralPath,
        [Parameter(Mandatory)][string]$Label
    )
    if (
        [string]::IsNullOrWhiteSpace($LiteralPath) -or
        $LiteralPath -cnotmatch '^[A-Za-z]:[\\/]'
    ) {
        Throw-M5PathBoundary $Label 'must be a drive-qualified local absolute path'
    }
    try {
        $absolute = [System.IO.Path]::GetFullPath($LiteralPath)
        $root = [System.IO.Path]::GetPathRoot($absolute)
    }
    catch {
        Throw-M5PathBoundary $Label 'is not a valid local absolute path'
    }
    if (
        [string]::IsNullOrWhiteSpace($root) -or
        (Test-M5PathEqual $absolute $root)
    ) {
        Throw-M5PathBoundary $Label 'must not be a drive root'
    }
    foreach ($protected in @(
        'I:\tmp',
        'H:\pik\00000000000'
    )) {
        $protectedAbsolute = [System.IO.Path]::GetFullPath($protected)
        if (
            (Test-M5PathEqual $absolute $protectedAbsolute) -or
            (Test-M5StrictChild $protectedAbsolute $absolute) -or
            (Test-M5StrictChild $absolute $protectedAbsolute)
        ) {
            Throw-M5PathBoundary $Label 'overlaps a protected media boundary'
        }
    }
    return $absolute
}

function Test-M5ExternalLeaf {
    param([Parameter(Mandatory)][string]$LiteralPath)
    $script:M5ExternalPathProbeCount++
    return Test-Path -LiteralPath $LiteralPath -PathType Leaf
}

function Test-M5ExternalContainer {
    param([Parameter(Mandatory)][string]$LiteralPath)
    $script:M5ExternalPathProbeCount++
    return Test-Path -LiteralPath $LiteralPath -PathType Container
}

function Resolve-M5ExternalLiteral {
    param([Parameter(Mandatory)][string]$LiteralPath)
    $script:M5ExternalPathProbeCount++
    return (Resolve-Path -LiteralPath $LiteralPath).Path
}

function Get-M5ExternalItem {
    param([Parameter(Mandatory)][string]$LiteralPath)
    $script:M5ExternalPathProbeCount++
    return Get-Item -LiteralPath $LiteralPath -Force
}

function Assert-M5NoReparseAncestor {
    param(
        [Parameter(Mandatory)][string]$LiteralPath,
        [Parameter(Mandatory)][string]$StopAt
    )

    $absolute = Get-M5AbsolutePath $LiteralPath
    $stop = Get-M5AbsolutePath $StopAt
    if (
        -not (Test-M5PathEqual $absolute $stop) -and
        -not (Test-M5StrictChild $stop $absolute)
    ) {
        Throw-M5 'PATH_OUTSIDE_WORKSPACE' 'path is outside the verified workspace'
    }

    $cursor = $absolute
    while ($true) {
        if (Test-Path -LiteralPath $cursor) {
            $item = Get-Item -LiteralPath $cursor -Force
            if (
                ($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0
            ) {
                Throw-M5 'REPARSE_ANCESTOR' 'a verified path has a reparse-point ancestor'
            }
        }
        if (Test-M5PathEqual $cursor $stop) {
            break
        }
        $parent = [System.IO.Path]::GetDirectoryName($cursor)
        if ([string]::IsNullOrWhiteSpace($parent) -or (Test-M5PathEqual $parent $cursor)) {
            Throw-M5 'PATH_OUTSIDE_WORKSPACE' 'verified workspace ancestor was not reached'
        }
        $cursor = $parent
    }
}

function Resolve-M5Executable {
    param(
        [Parameter(Mandatory)][string]$LiteralPath,
        [Parameter(Mandatory)][string]$Label
    )
    $lexical = Assert-M5ExternalPathLexical -LiteralPath $LiteralPath -Label $Label
    if (-not (Test-M5ExternalLeaf -LiteralPath $lexical)) {
        Throw-M5 'EXECUTABLE_INVALID' "$Label executable is not an existing leaf"
    }
    $resolved = Resolve-M5ExternalLiteral -LiteralPath $lexical
    $resolved = Assert-M5ExternalPathLexical -LiteralPath $resolved -Label $Label
    $item = Get-M5ExternalItem -LiteralPath $resolved
    if (
        $item.PSIsContainer -or
        (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0)
    ) {
        Throw-M5 'EXECUTABLE_INVALID' "$Label executable is not a plain leaf"
    }
    return [System.IO.Path]::GetFullPath($resolved)
}

function Resolve-M5EvidenceDirectory {
    param(
        [Parameter(Mandatory)][string]$LiteralPath,
        [Parameter(Mandatory)][string]$Workspace
    )
    $lexical = Assert-M5ExternalPathLexical `
        -LiteralPath $LiteralPath `
        -Label 'EvidenceDir'
    if (-not (Test-M5StrictChild $Workspace $lexical)) {
        Throw-M5 'EVIDENCE_INVALID' 'evidence directory must be inside the workspace'
    }
    if (-not (Test-M5ExternalContainer -LiteralPath $lexical)) {
        Throw-M5 'EVIDENCE_INVALID' 'evidence directory is not an existing directory'
    }
    $resolved = [System.IO.Path]::GetFullPath(
        (Resolve-M5ExternalLiteral -LiteralPath $lexical)
    )
    $resolved = Assert-M5ExternalPathLexical `
        -LiteralPath $resolved `
        -Label 'EvidenceDir'
    if (-not (Test-M5StrictChild $Workspace $resolved)) {
        Throw-M5 'EVIDENCE_INVALID' 'evidence directory must be inside the workspace'
    }
    Assert-M5NoReparseAncestor -LiteralPath $resolved -StopAt $Workspace
    return $resolved
}

function Assert-M5RunRoot {
    param(
        [Parameter(Mandatory)][string]$RunRoot,
        [Parameter(Mandatory)][string]$TmpRoot,
        [Parameter(Mandatory)][string]$Workspace
    )
    $absolute = Get-M5AbsolutePath $RunRoot
    $parent = [System.IO.Path]::GetDirectoryName($absolute)
    $leaf = [System.IO.Path]::GetFileName($absolute)
    if (
        -not (Test-M5PathEqual $parent $TmpRoot) -or
        -not $leaf.StartsWith(
            'm5-delete-',
            [System.StringComparison]::Ordinal
        ) -or
        $leaf.Length -le 'm5-delete-'.Length
    ) {
        Throw-M5 'RUN_ROOT_INVALID' 'run root is not the expected direct child of .superpowers\tmp'
    }
    Assert-M5NoReparseAncestor -LiteralPath $parent -StopAt $Workspace
    if (Test-Path -LiteralPath $absolute) {
        $item = Get-Item -LiteralPath $absolute -Force
        if (
            -not $item.PSIsContainer -or
            (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0)
        ) {
            Throw-M5 'RUN_ROOT_INVALID' 'existing run root is not a plain directory'
        }
    }
    return $absolute
}

function Assert-M5DriveLetter {
    param(
        [Parameter(Mandatory)][string]$DriveLetter,
        [Parameter(Mandatory)]$OccupiedDriveLetters
    )
    if ($DriveLetter -cnotmatch '^[D-Z]:$') {
        Throw-M5 'DRIVE_INVALID' 'drive letter is outside the selectable range'
    }
    foreach ($occupied in @($OccupiedDriveLetters)) {
        if (Test-M5PathEqual ([string]$occupied) $DriveLetter) {
            Throw-M5 'DRIVE_NOT_FREE' 'selected drive letter is already mapped or mounted'
        }
    }
}

function Assert-M5HelperRoots {
    param(
        [Parameter(Mandatory)]$HelperRoots,
        [Parameter(Mandatory)][string]$DriveLetter
    )
    $roots = @($HelperRoots)
    $expected = $DriveLetter + '\generated'
    if ($roots.Count -ne 1) {
        Throw-M5 'HELPER_ROOT_INVALID' 'Helper must have exactly one allowed root'
    }
    $candidate = [string]$roots[0]
    if (-not (Test-M5PathEqual $candidate $expected)) {
        Throw-M5 'HELPER_ROOT_INVALID' 'Helper root is not the exact generated child of the mapped run'
    }
}

function Assert-M5CleanupPID {
    param(
        [Parameter(Mandatory)]$RecordedPIDs,
        [Parameter(Mandatory)][int]$CleanupPID,
        [Parameter(Mandatory)][string]$CleanupIdentity
    )
    if ($CleanupPID -le 0) {
        Throw-M5 'CLEANUP_PID_UNVERIFIED' 'cleanup PID is not positive'
    }
    $matches = @(
        @($RecordedPIDs) | Where-Object {
            [int]$_.pid -eq $CleanupPID -and
            [string]$_.identity -ceq $CleanupIdentity
        }
    )
    if ($matches.Count -ne 1) {
        Throw-M5 'CLEANUP_PID_UNVERIFIED' 'cleanup PID/identity does not match one record'
    }
}

function Assert-M5SchemaRecord {
    param(
        [Parameter(Mandatory)][string]$RecordedSchema,
        [Parameter(Mandatory)][string]$CleanupSchema
    )
    if (
        $RecordedSchema -cnotmatch '^m5_e2e_[a-z0-9_]{8,80}$' -or
        $CleanupSchema -cne $RecordedSchema
    ) {
        Throw-M5 'CLEANUP_SCHEMA_UNVERIFIED' 'cleanup schema does not match its validated record'
    }
}

function Assert-M5DirectoryRecord {
    param(
        [Parameter(Mandatory)][string]$RecordedDirectory,
        [Parameter(Mandatory)][string]$CleanupDirectory,
        [Parameter(Mandatory)][string]$TmpRoot,
        [Parameter(Mandatory)][string]$Workspace
    )
    $recorded = Assert-M5RunRoot `
        -RunRoot $RecordedDirectory `
        -TmpRoot $TmpRoot `
        -Workspace $Workspace
    $cleanup = Get-M5AbsolutePath $CleanupDirectory
    if (-not (Test-M5PathEqual $recorded $cleanup)) {
        Throw-M5 'CLEANUP_DIRECTORY_UNVERIFIED' 'cleanup directory does not match its verified record'
    }
    return $recorded
}

function Assert-M5NoSyntheticResidue {
    param([Parameter(Mandatory)]$Residues)
    foreach ($name in @(
        'process', 'pipe', 'subst', 'schema',
        'junction', 'handle', 'directory'
    )) {
        if ([bool]$Residues.$name) {
            Throw-M5 ("RESIDUE_" + $name.ToUpperInvariant()) "synthetic $name residue remains"
        }
    }
}

function Remove-M5VerifiedRunRoot {
    param(
        [Parameter(Mandatory)][string]$RunRoot,
        [Parameter(Mandatory)][string]$TmpRoot,
        [Parameter(Mandatory)][string]$Workspace
    )
    $absolute = Assert-M5RunRoot `
        -RunRoot $RunRoot `
        -TmpRoot $TmpRoot `
        -Workspace $Workspace
    if (-not (Test-Path -LiteralPath $absolute)) {
        return
    }
    $item = Get-Item -LiteralPath $absolute -Force
    if (
        -not $item.PSIsContainer -or
        (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0)
    ) {
        Throw-M5 'CLEANUP_DIRECTORY_UNVERIFIED' 'run root changed before cleanup'
    }
    Remove-Item -LiteralPath $absolute -Recurse -Force
    if (Test-Path -LiteralPath $absolute) {
        Throw-M5 'RESIDUE_DIRECTORY' 'verified run root remains after cleanup'
    }
}

function Invoke-M5SafetySeam {
    param(
        [Parameter(Mandatory)]$Seam,
        [Parameter(Mandatory)][string]$Workspace,
        [Parameter(Mandatory)][string]$TmpRoot
    )
    if (
        [int]$Seam.schema_version -ne 1 -or
        [string]$Seam.mode -cne 'safety'
    ) {
        Throw-M5 'SAFETY_SEAM_INVALID' 'unsupported deterministic safety seam'
    }
    $runRoot = Assert-M5RunRoot `
        -RunRoot ([string]$Seam.proposed_run_root) `
        -TmpRoot $TmpRoot `
        -Workspace $Workspace
    Assert-M5DriveLetter `
        -DriveLetter ([string]$Seam.drive_letter) `
        -OccupiedDriveLetters $Seam.occupied_drive_letters
    Assert-M5HelperRoots `
        -HelperRoots $Seam.helper_roots `
        -DriveLetter ([string]$Seam.drive_letter)
    Assert-M5CleanupPID `
        -RecordedPIDs $Seam.recorded_pids `
        -CleanupPID ([int]$Seam.cleanup_pid) `
        -CleanupIdentity ([string]$Seam.cleanup_pid_identity)
    Assert-M5SchemaRecord `
        -RecordedSchema ([string]$Seam.recorded_schema) `
        -CleanupSchema ([string]$Seam.cleanup_schema)
    $cleanupRoot = Assert-M5DirectoryRecord `
        -RecordedDirectory ([string]$Seam.recorded_directory) `
        -CleanupDirectory ([string]$Seam.cleanup_directory) `
        -TmpRoot $TmpRoot `
        -Workspace $Workspace
    if (-not (Test-M5PathEqual $cleanupRoot $runRoot)) {
        Throw-M5 'CLEANUP_DIRECTORY_UNVERIFIED' 'cleanup record does not identify the proposed run'
    }
    Assert-M5NoSyntheticResidue -Residues $Seam.residues

    if ([bool]$Seam.perform_exact_path_cleanup) {
        if (Test-Path -LiteralPath $runRoot) {
            Throw-M5 'RUN_ROOT_INVALID' 'synthetic exact-cleanup target already exists'
        }
        New-Item -ItemType Directory -Path $runRoot | Out-Null
        $created = Get-Item -LiteralPath $runRoot -Force
        if (
            -not $created.PSIsContainer -or
            (($created.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0)
        ) {
            Throw-M5 'RUN_ROOT_INVALID' 'synthetic run root is not a plain directory'
        }
        Set-Content -LiteralPath (Join-Path $runRoot 'synthetic.txt') -Value 'generated'
        Remove-M5VerifiedRunRoot `
            -RunRoot $runRoot `
            -TmpRoot $TmpRoot `
            -Workspace $Workspace
    }

    Write-Output (
        'SAFETY_SCENARIO_OK scenario={0}' -f [string]$Seam.scenario
    )
}

function Protect-M5Text {
    param([AllowNull()][string]$Text)
    if ($null -eq $Text) {
        return ''
    }
    return [regex]::Replace(
        $Text,
        '(?i)postgres(?:ql)?://[^\s"''<>]+',
        'postgres://[REDACTED]'
    )
}

function Assert-M5SchemaIdentifier {
    param([Parameter(Mandatory)][string]$Schema)
    if ($Schema -cnotmatch '^m5_e2e_[a-z0-9_]{8,72}$') {
        Throw-M5 'SCHEMA_INVALID' 'generated PostgreSQL schema identifier is invalid'
    }
}

function Add-M5SearchPath {
    param(
        [Parameter(Mandatory)][string]$DSN,
        [Parameter(Mandatory)][string]$Schema
    )
    Assert-M5SchemaIdentifier $Schema
    try {
        $uri = [uri]$DSN
        if (
            -not $uri.IsAbsoluteUri -or
            ($uri.Scheme -cne 'postgres' -and $uri.Scheme -cne 'postgresql')
        ) {
            throw 'unsupported DSN form'
        }
        $builder = [System.UriBuilder]::new($uri)
        $query = $builder.Query.TrimStart('?')
        $parameter = 'search_path=' + [uri]::EscapeDataString($Schema)
        if ([string]::IsNullOrWhiteSpace($query)) {
            $builder.Query = $parameter
        }
        else {
            $builder.Query = $query + '&' + $parameter
        }
        return $builder.Uri.AbsoluteUri
    }
    catch {
        Throw-M5 'PG_DSN_INVALID' 'PostgreSQL DSN cannot be safely scoped'
    }
}

function Get-M5FreePort {
    $listener = [System.Net.Sockets.TcpListener]::new(
        [System.Net.IPAddress]::Loopback,
        0
    )
    try {
        $listener.Start()
        return ([System.Net.IPEndPoint]$listener.LocalEndpoint).Port
    }
    finally {
        $listener.Stop()
    }
}

function Get-M5FreeDriveLetter {
    $occupied = @{}
    foreach ($drive in [System.IO.DriveInfo]::GetDrives()) {
        if ($drive.Name -match '^([A-Za-z]):\\$') {
            $occupied[$matches[1].ToUpperInvariant() + ':'] = $true
        }
    }
    $substOutput = @(& subst 2>$null)
    foreach ($line in $substOutput) {
        if ([string]$line -match '^\s*([A-Za-z]):\\: => ') {
            $occupied[$matches[1].ToUpperInvariant() + ':'] = $true
        }
    }
    for ($code = [int][char]'Z'; $code -ge [int][char]'D'; $code--) {
        $candidate = ([char]$code).ToString() + ':'
        if (-not $occupied.ContainsKey($candidate)) {
            return $candidate
        }
    }
    Throw-M5 'DRIVE_NOT_FREE' 'no free drive letter is available from Z: downward'
}

function Get-M5SubstTarget {
    param([Parameter(Mandatory)][string]$DriveLetter)
    $output = @(& subst 2>$null)
    if ($LASTEXITCODE -ne 0) {
        return ''
    }
    $pattern = (
        '^\s*' +
        [regex]::Escape($DriveLetter) +
        '\\: => (.+?)\s*$'
    )
    foreach ($entry in $output) {
        $line = [string]$entry
        if ($line -match $pattern) {
            return [System.IO.Path]::GetFullPath($matches[1])
        }
    }
    return ''
}

function Write-M5JSON {
    param(
        [Parameter(Mandatory)][string]$LiteralPath,
        [Parameter(Mandatory)]$Value
    )
    $json = $Value | ConvertTo-Json -Depth 12
    [System.IO.File]::WriteAllText(
        $LiteralPath,
        $json,
        [System.Text.UTF8Encoding]::new($false)
    )
}

function Assert-M5ProcessIdentity {
    param(
        [Parameter(Mandatory)][int]$PIDValue,
        [Parameter(Mandatory)][string]$ExpectedExecutable
    )
    if ($PIDValue -le 0) {
        Throw-M5 'CLEANUP_PID_UNVERIFIED' 'recorded process has a non-positive PID'
    }
    $process = Get-Process -Id $PIDValue -ErrorAction Stop
    $actual = [System.IO.Path]::GetFullPath($process.Path)
    $expected = [System.IO.Path]::GetFullPath($ExpectedExecutable)
    if (-not (Test-M5PathEqual $actual $expected)) {
        Throw-M5 'CLEANUP_PID_UNVERIFIED' 'recorded PID identity changed'
    }
    return $process
}

function Start-M5RecordedProcess {
    param(
        [Parameter(Mandatory)][string]$Identity,
        [Parameter(Mandatory)][string]$Executable,
        [Parameter(Mandatory)][string[]]$Arguments,
        [Parameter(Mandatory)][string]$StdoutPath,
        [Parameter(Mandatory)][string]$StderrPath
    )
    $process = Start-Process `
        -FilePath $Executable `
        -ArgumentList $Arguments `
        -PassThru `
        -WindowStyle Hidden `
        -RedirectStandardOutput $StdoutPath `
        -RedirectStandardError $StderrPath
    if ($null -eq $process -or $process.Id -le 0) {
        Throw-M5 'PROCESS_START_FAILED' "$Identity launch returned no positive PID"
    }
    Start-Sleep -Milliseconds 100
    $verified = Assert-M5ProcessIdentity `
        -PIDValue $process.Id `
        -ExpectedExecutable $Executable
    return [ordered]@{
        identity = $Identity
        pid = [int]$process.Id
        executable = [System.IO.Path]::GetFullPath($Executable)
        process = $verified
    }
}

function Stop-M5RecordedProcess {
    param([Parameter(Mandatory)]$Record)
    $pidValue = [int]$Record.pid
    $existing = Get-Process -Id $pidValue -ErrorAction SilentlyContinue
    if ($null -eq $existing) {
        return
    }
    $verified = Assert-M5ProcessIdentity `
        -PIDValue $pidValue `
        -ExpectedExecutable ([string]$Record.executable)
    Stop-Process -Id $pidValue -Force -ErrorAction Stop
    if (-not $verified.WaitForExit(10000)) {
        Throw-M5 'RESIDUE_PROCESS' 'recorded process did not exit'
    }
}

function Invoke-M5GoAction {
    param(
        [Parameter(Mandatory)][string]$Action,
        [Parameter(Mandatory)][string]$TestName,
        [Parameter(Mandatory)][string]$GoExecutable,
        [Parameter(Mandatory)][string]$Workspace,
        [Parameter(Mandatory)][string]$OutputPath
    )
    $env:M5_E2E_ACTION = $Action
    $output = @(
        & $GoExecutable `
            -C $Workspace `
            test `
            -count=1 `
            ./internal/gui `
            -run ("^" + $TestName + "$") 2>&1
    )
    $status = $LASTEXITCODE
    $safe = Protect-M5Text ($output -join [Environment]::NewLine)
    [System.IO.File]::WriteAllText(
        $OutputPath,
        $safe,
        [System.Text.UTF8Encoding]::new($false)
    )
    if ($safe -ne '') {
        Write-Output $safe
    }
    if ($status -ne 0) {
        Throw-M5 'GO_ACTION_FAILED' "$Action Go action failed"
    }
}

function Test-M5PipeAvailable {
    param(
        [Parameter(Mandatory)][string]$PipeName,
        [int]$TimeoutMS = 75
    )
    $prefix = '\\.\pipe\'
    if (-not $PipeName.StartsWith($prefix, [System.StringComparison]::Ordinal)) {
        Throw-M5 'PIPE_INVALID' 'recorded pipe name is invalid'
    }
    $leaf = $PipeName.Substring($prefix.Length)
    $client = [System.IO.Pipes.NamedPipeClientStream]::new(
        '.',
        $leaf,
        [System.IO.Pipes.PipeDirection]::InOut,
        [System.IO.Pipes.PipeOptions]::Asynchronous
    )
    try {
        try {
            $client.Connect($TimeoutMS)
            return $client.IsConnected
        }
        catch [System.TimeoutException] {
            return $false
        }
        catch [System.IO.IOException] {
            return $false
        }
    }
    finally {
        $client.Dispose()
    }
}

function Assert-M5RunTreePlain {
    param([Parameter(Mandatory)][string]$RunRoot)
    $root = Get-Item -LiteralPath $RunRoot -Force
    if (
        -not $root.PSIsContainer -or
        (($root.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0)
    ) {
        Throw-M5 'CLEANUP_DIRECTORY_UNVERIFIED' 'run root is not a plain directory'
    }
    foreach ($item in Get-ChildItem -LiteralPath $RunRoot -Force -Recurse) {
        if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
            Throw-M5 'RESIDUE_JUNCTION' 'run tree contains a reparse point'
        }
    }
}

function Invoke-M5RealRun {
    param(
        [Parameter(Mandatory)][string]$Workspace,
        [Parameter(Mandatory)][string]$TmpRoot,
        [Parameter(Mandatory)][string]$GoExecutable,
        [Parameter(Mandatory)][string]$AdminDSN,
        [Parameter(Mandatory)][string]$HelperExecutable,
        [Parameter(Mandatory)][string]$AgentExecutable,
        [Parameter(Mandatory)][string]$WorkerExecutable,
        [Parameter(Mandatory)][string]$GUIExecutable,
        [Parameter(Mandatory)][string]$Evidence
    )

    $runID = [guid]::NewGuid().ToString('N')
    $runRoot = Assert-M5RunRoot `
        -RunRoot (Join-Path $TmpRoot ("m5-delete-$runID")) `
        -TmpRoot $TmpRoot `
        -Workspace $Workspace
    $schema = "m5_e2e_$runID"
    Assert-M5SchemaIdentifier $schema
    $scopedDSN = Add-M5SearchPath -DSN $AdminDSN -Schema $schema
    $driveLetter = Get-M5FreeDriveLetter
    $machineID = "m5-agent-" + $runID.Substring(0, 12)
    $pipeName = "\\.\pipe\dedup-m5-$runID"
    $agentPort = Get-M5FreePort
    do {
        $guiPort = Get-M5FreePort
    } while ($guiPort -eq $agentPort)

    $mappingRecorded = $false
    $schemaRecorded = $false
    $runCreated = $false
    $processRecords = [System.Collections.ArrayList]::new()
    $cleanupFailures = [System.Collections.Generic.List[string]]::new()
    $primaryFailure = $null
    $acceptancePassed = $false
    $workerExecutable = $WorkerExecutable

    try {
        New-Item -ItemType Directory -Path $runRoot | Out-Null
        $runCreated = $true
        $created = Get-Item -LiteralPath $runRoot -Force
        if (
            -not $created.PSIsContainer -or
            (($created.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0)
        ) {
            Throw-M5 'RUN_ROOT_INVALID' 'created run root is not a plain directory'
        }

        & subst $driveLetter $runRoot
        if ($LASTEXITCODE -ne 0) {
            Throw-M5 'SUBST_FAILED' 'subst mapping failed'
        }
        $mappingRecorded = $true
        $mappedTarget = Get-M5SubstTarget $driveLetter
        if (
            [string]::IsNullOrWhiteSpace($mappedTarget) -or
            -not (Test-M5PathEqual $mappedTarget $runRoot)
        ) {
            Throw-M5 'SUBST_FAILED' 'subst mapping does not match the verified run root'
        }

        $generatedRoot = $driveLetter + '\generated'
        $outsideRoot = $driveLetter + '\outside'
        New-Item -ItemType Directory -Path $generatedRoot | Out-Null
        New-Item -ItemType Directory -Path $outsideRoot | Out-Null
        $runtime = Join-Path $runRoot 'runtime'
        $agentData = Join-Path $runtime 'agent-data'
        $helperLogs = Join-Path $runtime 'helper-logs'
        foreach ($directory in @($runtime, $agentData, $helperLogs)) {
            New-Item -ItemType Directory -Path $directory -Force | Out-Null
        }

        $helperConfig = Join-Path $runtime 'helper.json'
        $agentConfig = Join-Path $runtime 'agent.json'
        $guiConfig = Join-Path $runtime 'gui.json'
        Write-M5JSON -LiteralPath $helperConfig -Value ([ordered]@{
            pipe_name = $pipeName
            allowed_roots = @($generatedRoot)
            denied_roots = @(
                (Join-Path $generatedRoot 'system'),
                (Join-Path $generatedRoot 'denied')
            )
            default_mode = 'soft'
            allow_hard_delete = $true
            recycle_dir_name = '$DedupRecycle'
            max_entries_per_frame = 2000
            frame_read_timeout_sec = 120
            frame_write_timeout_sec = 60
            log_dir = $helperLogs
        })
        Write-M5JSON -LiteralPath $agentConfig -Value ([ordered]@{
            machine_id = $machineID
            listen_addr = "127.0.0.1:$agentPort"
            data_dir = $agentData
            pg_dsn = $scopedDSN
            use_everything = $false
            sync = [ordered]@{
                interval_s = 2
                trigger_rows = 50000
                upsert_batch = 5000
            }
            proto = [ordered]@{ heartbeat_s = 1 }
            worker = [ordered]@{
                count = 1
                exe_path = $workerExecutable
                image_timeout_s = 30
                video_timeout_s = 120
                image_memory_mb = 64
                respawn_delay_ms = 500
                crash_injection = $false
            }
            delete = [ordered]@{
                pipe_name = $pipeName
                max_entries_per_frame = 2000
                dial_timeout_ms = 500
                hello_timeout_s = 5
                report_timeout_s = 120
            }
        })
        Write-M5JSON -LiteralPath $guiConfig -Value ([ordered]@{
            listen_addr = "127.0.0.1:$guiPort"
            pg_dsn = $scopedDSN
            heartbeat_s = 1
            agents = @(
                [ordered]@{
                    machine_id = $machineID
                    addr = "127.0.0.1:$agentPort"
                }
            )
        })

        $env:M5_E2E_ACTIVE = '1'
        $env:M5_E2E_WORKSPACE = $Workspace
        $env:M5_E2E_RUN_ID = $runID
        $env:M5_E2E_RUN_ROOT = $runRoot
        $env:M5_E2E_GENERATED_ROOT = $generatedRoot
        $env:M5_E2E_DRIVE = $driveLetter
        $env:M5_E2E_PIPE = $pipeName
        $env:M5_E2E_SCHEMA = $schema
        $env:M5_E2E_RECORDED_SCHEMA = $schema
        $env:M5_E2E_ADMIN_DSN = $AdminDSN
        $env:M5_E2E_SCOPED_DSN = $scopedDSN
        $env:M5_E2E_HELPER_EXE = $HelperExecutable
        $env:M5_E2E_AGENT_EXE = $AgentExecutable
        $env:M5_E2E_GUI_EXE = $GUIExecutable
        $env:M5_E2E_HELPER_CONFIG = $helperConfig
        $env:M5_E2E_AGENT_CONFIG = $agentConfig
        $env:M5_E2E_GUI_CONFIG = $guiConfig
        $env:M5_E2E_AGENT_DATA = $agentData
        $env:M5_E2E_EVIDENCE_DIR = $Evidence
        $env:M5_E2E_MACHINE_ID = $machineID
        $env:M5_E2E_SECOND_WINDOWS_STATUS = $secondWindowsStatus
        $env:M5_E2E_GUI_URL = "http://127.0.0.1:$guiPort"

        Invoke-M5GoAction `
            -Action 'schema-setup' `
            -TestName 'TestM5E2ESchemaSetup' `
            -GoExecutable $GoExecutable `
            -Workspace $Workspace `
            -OutputPath (Join-Path $Evidence "m5-$runID-schema-setup.log")
        $schemaRecorded = $true

        $helperRecord = Start-M5RecordedProcess `
            -Identity 'helper' `
            -Executable $HelperExecutable `
            -Arguments @('-config', $helperConfig) `
            -StdoutPath (Join-Path $runtime 'helper.stdout.log') `
            -StderrPath (Join-Path $runtime 'helper.stderr.log')
        $null = $processRecords.Add($helperRecord)
        $agentRecord = Start-M5RecordedProcess `
            -Identity 'agent' `
            -Executable $AgentExecutable `
            -Arguments @('-config', $agentConfig) `
            -StdoutPath (Join-Path $runtime 'agent.stdout.log') `
            -StderrPath (Join-Path $runtime 'agent.stderr.log')
        $null = $processRecords.Add($agentRecord)
        $guiRecord = Start-M5RecordedProcess `
            -Identity 'gui' `
            -Executable $GUIExecutable `
            -Arguments @('-config', $guiConfig) `
            -StdoutPath (Join-Path $runtime 'gui.stdout.log') `
            -StderrPath (Join-Path $runtime 'gui.stderr.log')
        $null = $processRecords.Add($guiRecord)
        $env:M5_E2E_HELPER_PID = [string]$helperRecord.pid
        $env:M5_E2E_AGENT_PID = [string]$agentRecord.pid
        $env:M5_E2E_GUI_PID = [string]$guiRecord.pid

        Invoke-M5GoAction `
            -Action 'acceptance' `
            -TestName 'TestM5E2EWindows' `
            -GoExecutable $GoExecutable `
            -Workspace $Workspace `
            -OutputPath (Join-Path $Evidence "m5-$runID-acceptance.log")
        $acceptancePassed = $true
    }
    catch {
        $primaryFailure = Protect-M5Text $_.Exception.Message
    }
    finally {
        try {
            if ($processRecords.Count -gt 1) {
                $agentRecord = @($processRecords | Where-Object {
                    $_.identity -eq 'agent'
                })[0]
                if ($null -ne $agentRecord) {
                    $children = @(
                        Get-CimInstance Win32_Process -Filter (
                            "ParentProcessId=" + [int]$agentRecord.pid
                        ) -ErrorAction SilentlyContinue
                    )
                    foreach ($child in $children) {
                        if ($null -eq $child -or [int]$child.ProcessId -le 0) {
                            continue
                        }
                        $childPath = [string]$child.ExecutablePath
                        if (
                            -not [string]::IsNullOrWhiteSpace($childPath) -and
                            (Test-M5PathEqual $childPath $workerExecutable)
                        ) {
                            $null = $processRecords.Add([ordered]@{
                                identity = 'worker'
                                pid = [int]$child.ProcessId
                                executable = $workerExecutable
                                process = $null
                            })
                        }
                    }
                }
            }
        }
        catch {
            $cleanupFailures.Add(
                'record worker: ' + (Protect-M5Text $_.Exception.Message)
            )
        }

        for ($recordIndex = $processRecords.Count - 1; $recordIndex -ge 0; $recordIndex--) {
            $record = $processRecords[$recordIndex]
            try {
                Stop-M5RecordedProcess -Record $record
            }
            catch {
                $cleanupFailures.Add(
                    "stop $($record.identity)/$($record.pid): " +
                    (Protect-M5Text $_.Exception.Message)
                )
            }
        }

        if ($schemaRecorded) {
            try {
                Invoke-M5GoAction `
                    -Action 'schema-cleanup' `
                    -TestName 'TestM5E2ESchemaCleanup' `
                    -GoExecutable $GoExecutable `
                    -Workspace $Workspace `
                    -OutputPath (Join-Path $Evidence "m5-$runID-schema-cleanup.log")
                $schemaRecorded = $false
            }
            catch {
                $cleanupFailures.Add(
                    'schema cleanup: ' + (Protect-M5Text $_.Exception.Message)
                )
            }
        }

        if (Test-M5PipeAvailable -PipeName $pipeName) {
            $cleanupFailures.Add('RESIDUE_PIPE: recorded pipe remains connectable')
        }

        if ($mappingRecorded) {
            try {
                $target = Get-M5SubstTarget $driveLetter
                if (-not (Test-M5PathEqual $target $runRoot)) {
                    Throw-M5 'CLEANUP_SUBST_UNVERIFIED' 'subst target changed before cleanup'
                }
                & subst $driveLetter /d
                if ($LASTEXITCODE -ne 0) {
                    Throw-M5 'RESIDUE_SUBST' 'subst removal failed'
                }
                if (-not [string]::IsNullOrWhiteSpace(
                    (Get-M5SubstTarget $driveLetter)
                )) {
                    Throw-M5 'RESIDUE_SUBST' 'subst mapping remains'
                }
                $mappingRecorded = $false
            }
            catch {
                $cleanupFailures.Add(
                    'subst cleanup: ' + (Protect-M5Text $_.Exception.Message)
                )
            }
        }

        if ($runCreated) {
            try {
                $verified = Assert-M5RunRoot `
                    -RunRoot $runRoot `
                    -TmpRoot $TmpRoot `
                    -Workspace $Workspace
                Assert-M5RunTreePlain -RunRoot $verified
                Remove-M5VerifiedRunRoot `
                    -RunRoot $verified `
                    -TmpRoot $TmpRoot `
                    -Workspace $Workspace
                $runCreated = $false
            }
            catch {
                $cleanupFailures.Add(
                    'run cleanup: ' + (Protect-M5Text $_.Exception.Message)
                )
            }
        }

        foreach ($record in $processRecords) {
            if (Get-Process -Id ([int]$record.pid) -ErrorAction SilentlyContinue) {
                $cleanupFailures.Add(
                    "RESIDUE_PROCESS: recorded PID $($record.pid) remains"
                )
            }
        }
        if ($schemaRecorded) {
            $cleanupFailures.Add('RESIDUE_SCHEMA: recorded schema cleanup failed')
        }
        if ($mappingRecorded) {
            $cleanupFailures.Add('RESIDUE_SUBST: recorded mapping cleanup failed')
        }
        if ($runCreated) {
            $cleanupFailures.Add('RESIDUE_DIRECTORY: recorded run root cleanup failed')
        }

        $cleanupAudit = [ordered]@{
            schema_version = 1
            run_id = $runID
            run_root_removed = (-not $runCreated)
            drive_letter = $driveLetter
            subst_removed = (-not $mappingRecorded)
            schema = $schema
            schema_removed = (-not $schemaRecorded)
            pipe_removed = (-not (Test-M5PipeAvailable -PipeName $pipeName))
            process_residue = @(
                $processRecords | Where-Object {
                    Get-Process -Id ([int]$_.pid) -ErrorAction SilentlyContinue
                } | ForEach-Object { [int]$_.pid }
            )
            junction_residue = 0
            handle_residue = 0
            directory_residue = $(if ($runCreated) { 1 } else { 0 })
            failures = @($cleanupFailures)
        }
        Write-M5JSON `
            -LiteralPath (Join-Path $Evidence "m5-$runID-cleanup.json") `
            -Value $cleanupAudit
    }

    if ($null -ne $primaryFailure) {
        Throw-M5 'M5_E2E_FAILED' $primaryFailure
    }
    if ($cleanupFailures.Count -ne 0) {
        Throw-M5 'M5_E2E_CLEANUP_FAILED' ($cleanupFailures -join '; ')
    }
    if (-not $acceptancePassed) {
        Throw-M5 'M5_E2E_FAILED' 'acceptance action did not complete'
    }
    Write-Output (
        "M5_E2E_OK run_id=$runID drive=$driveLetter schema=$schema"
    )
}

$workspace = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$tmpRoot = [System.IO.Path]::GetFullPath(
    (Join-Path $workspace '.superpowers\tmp')
)
$lexicalGo = Assert-M5ExternalPathLexical -LiteralPath $Go -Label 'Go'
$lexicalHelper = Assert-M5ExternalPathLexical -LiteralPath $HelperExe -Label 'Helper'
$lexicalAgent = Assert-M5ExternalPathLexical -LiteralPath $AgentExe -Label 'Agent'
$lexicalGUI = Assert-M5ExternalPathLexical -LiteralPath $GUIExe -Label 'GUI'
$lexicalEvidence = Assert-M5ExternalPathLexical `
    -LiteralPath $EvidenceDir `
    -Label 'EvidenceDir'
$lexicalWorker = Assert-M5ExternalPathLexical `
    -LiteralPath (
        Join-Path ([System.IO.Path]::GetDirectoryName($lexicalAgent)) 'worker.exe'
    ) `
    -Label 'Agent worker'
if (-not (Test-Path -LiteralPath $tmpRoot -PathType Container)) {
    New-Item -ItemType Directory -Path $tmpRoot | Out-Null
}
Assert-M5NoReparseAncestor -LiteralPath $tmpRoot -StopAt $workspace

$resolvedGo = Resolve-M5Executable -LiteralPath $lexicalGo -Label 'Go'
$resolvedHelper = Resolve-M5Executable -LiteralPath $lexicalHelper -Label 'Helper'
$resolvedAgent = Resolve-M5Executable -LiteralPath $lexicalAgent -Label 'Agent'
$resolvedWorker = Resolve-M5Executable `
    -LiteralPath $lexicalWorker `
    -Label 'Agent worker'
$resolvedGUI = Resolve-M5Executable -LiteralPath $lexicalGUI -Label 'GUI'
$resolvedEvidence = Resolve-M5EvidenceDirectory `
    -LiteralPath $lexicalEvidence `
    -Workspace $workspace

# Resolve all untrusted inputs before selecting a mode. Deliberately do not
# write or echo the PostgreSQL DSN; only the real-run branch passes it through
# an environment variable to the bounded Go acceptance test.
$null = @(
    $resolvedGo,
    $resolvedHelper,
    $resolvedAgent,
    $resolvedGUI,
    $resolvedEvidence
)
if ([string]::IsNullOrWhiteSpace($PGDSN)) {
    Throw-M5 'PG_DSN_INVALID' 'PostgreSQL DSN is empty'
}

$safetyJSON = [Environment]::GetEnvironmentVariable(
    'M5_E2E_TEST_SEAM',
    [EnvironmentVariableTarget]::Process
)
if (-not [string]::IsNullOrWhiteSpace($safetyJSON)) {
    try {
        $seam = $safetyJSON | ConvertFrom-Json
    }
    catch {
        Throw-M5 'SAFETY_SEAM_INVALID' 'deterministic safety seam is not valid JSON'
    }
    Invoke-M5SafetySeam -Seam $seam -Workspace $workspace -TmpRoot $tmpRoot
    exit 0
}

Invoke-M5RealRun `
    -Workspace $workspace `
    -TmpRoot $tmpRoot `
    -GoExecutable $resolvedGo `
    -AdminDSN $PGDSN `
    -HelperExecutable $resolvedHelper `
    -AgentExecutable $resolvedAgent `
    -WorkerExecutable $resolvedWorker `
    -GUIExecutable $resolvedGUI `
    -Evidence $resolvedEvidence
