$ErrorActionPreference = 'Stop'

$script:M5RequiredGates = @(
    'format',
    'pure_go_full',
    'cgo_full',
    'race_changed',
    'vet',
    'helper_unit',
    'manifest_audit',
    'pipe_acl',
    'agent_forwarder',
    'postgres_contracts',
    'm5_e2e',
    'public_unchanged',
    'secret_scan',
    'marker_negative',
    'cleanup_audit'
)

function Get-M5RequiredGates {
    return [string[]]@($script:M5RequiredGates)
}

function Get-M5ProtectedLexicalRoots {
    # Constructed as components so these inert policy values cannot be mistaken
    # for commands or filesystem probes.
    return [string[]]@(
        [System.IO.Path]::GetFullPath(('I:' + '\tmp')),
        [System.IO.Path]::GetFullPath(('H:' + '\pik\00000000000'))
    )
}

function Assert-M5LexicalPathDoesNotIntersectProtectedRoots {
    param(
        [Parameter(Mandatory)][string]$FullPath,
        [Parameter(Mandatory)][string]$Label
    )
    $candidate = $FullPath.TrimEnd('\')
    foreach ($protected in Get-M5ProtectedLexicalRoots) {
        $protectedPath = $protected.TrimEnd('\')
        $candidatePrefix = $candidate + '\'
        $protectedPrefix = $protectedPath + '\'
        if ([string]::Equals(
            $candidate,
            $protectedPath,
            [System.StringComparison]::OrdinalIgnoreCase
        ) -or $candidate.StartsWith(
            $protectedPrefix,
            [System.StringComparison]::OrdinalIgnoreCase
        ) -or $protectedPath.StartsWith(
            $candidatePrefix,
            [System.StringComparison]::OrdinalIgnoreCase
        )) {
            throw "$Label intersects a protected media root."
        }
    }
}

function ConvertTo-M5LexicalLocalPath {
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$Label
    )
    if ([string]::IsNullOrWhiteSpace($Path) -or $Path.Contains([char]0)) {
        throw "$Label must be a non-empty local absolute path."
    }
    if ($Path -notmatch '^[A-Za-z]:[\\/]') {
        throw "$Label must be a local drive-letter absolute path."
    }
    try {
        $full = [System.IO.Path]::GetFullPath($Path)
    }
    catch {
        throw "$Label is not a valid local absolute path."
    }
    if ($full -notmatch '^[A-Za-z]:\\') {
        throw "$Label did not normalize to a local drive-letter path."
    }
    Assert-M5LexicalPathDoesNotIntersectProtectedRoots `
        -FullPath $full `
        -Label $Label
    return $full
}

function Assert-M5LexicalPathWithin {
    param(
        [Parameter(Mandatory)][string]$FullRoot,
        [Parameter(Mandatory)][string]$FullPath,
        [Parameter(Mandatory)][string]$Label,
        [switch]$AllowRoot
    )
    $root = $FullRoot.TrimEnd('\')
    $candidate = $FullPath.TrimEnd('\')
    if ([string]::Equals(
        $candidate,
        $root,
        [System.StringComparison]::OrdinalIgnoreCase
    )) {
        if (-not $AllowRoot) {
            throw "$Label must be below, not equal to, its allowed root."
        }
        return
    }
    if (-not $candidate.StartsWith(
        $root + '\',
        [System.StringComparison]::OrdinalIgnoreCase
    )) {
        throw "$Label is outside its allowed root."
    }
}

function Assert-M5ExistingPathHasNoReparsePoint {
    param(
        [Parameter(Mandatory)][string]$FullRoot,
        [Parameter(Mandatory)][string]$FullPath,
        [Parameter(Mandatory)][string]$Label
    )
    Assert-M5LexicalPathWithin `
        -FullRoot $FullRoot `
        -FullPath $FullPath `
        -Label $Label `
        -AllowRoot

    $root = $FullRoot.TrimEnd('\')
    $candidate = $FullPath.TrimEnd('\')
    $relative = if ([string]::Equals(
        $root,
        $candidate,
        [System.StringComparison]::OrdinalIgnoreCase
    )) {
        ''
    }
    else {
        $candidate.Substring($root.Length).TrimStart('\')
    }
    $current = $root
    foreach ($component in @('') + @($relative -split '\\')) {
        if (-not [string]::IsNullOrEmpty($component)) {
            $current = Join-Path $current $component
        }
        if (-not [System.IO.Directory]::Exists($current) -and
            -not [System.IO.File]::Exists($current)) {
            break
        }
        try {
            $attributes = [System.IO.File]::GetAttributes($current)
        }
        catch {
            throw "$Label contains an unreadable existing component."
        }
        if (($attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw "$Label contains a reparse point."
        }
    }
}

function Get-M5WorkspacePath {
    param(
        [Parameter(Mandatory)][string]$WorkspaceRoot,
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$Label,
        [switch]$AllowRoot
    )
    $root = ConvertTo-M5LexicalLocalPath `
        -Path $WorkspaceRoot `
        -Label 'workspace root'
    $full = ConvertTo-M5LexicalLocalPath -Path $Path -Label $Label
    Assert-M5LexicalPathWithin `
        -FullRoot $root `
        -FullPath $full `
        -Label $Label `
        -AllowRoot:$AllowRoot
    Assert-M5ExistingPathHasNoReparsePoint `
        -FullRoot $root `
        -FullPath $full `
        -Label $Label
    return $full
}

function Get-M5EvidenceFile {
    param(
        [Parameter(Mandatory)][string]$EvidenceDir,
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$Label
    )
    $root = ConvertTo-M5LexicalLocalPath `
        -Path $EvidenceDir `
        -Label 'evidence directory'
    $full = ConvertTo-M5LexicalLocalPath -Path $Path -Label $Label
    Assert-M5LexicalPathWithin `
        -FullRoot $root `
        -FullPath $full `
        -Label $Label
    Assert-M5ExistingPathHasNoReparsePoint `
        -FullRoot $root `
        -FullPath $full `
        -Label $Label
    if (-not [System.IO.File]::Exists($full)) {
        throw "$Label must be an existing file."
    }
    return $full
}

function Get-M5PropertyNames {
    param([Parameter(Mandatory)][object]$Object)
    if ($Object -is [System.Collections.IDictionary]) {
        return [string[]]@($Object.Keys)
    }
    if ($Object -is [pscustomobject]) {
        return [string[]]@($Object.PSObject.Properties.Name)
    }
    throw 'value must be a JSON object'
}

function Get-M5RequiredValue {
    param(
        [Parameter(Mandatory)][object]$Object,
        [Parameter(Mandatory)][string]$Name,
        [Parameter(Mandatory)][string]$Path,
        [switch]$AllowNull,
        [switch]$NoEnumerate
    )
    if ($null -eq $Object) {
        throw "$Path is null."
    }
    if ($Object -is [System.Collections.IDictionary]) {
        if (-not $Object.Contains($Name)) {
            throw "$Path is missing property $Name."
        }
        $value = $Object[$Name]
    }
    else {
        $property = $Object.PSObject.Properties[$Name]
        if ($null -eq $property) {
            throw "$Path is missing property $Name."
        }
        $value = $property.Value
    }
    if ($null -eq $value -and -not $AllowNull) {
        throw "$Path.$Name is null."
    }
    if ($NoEnumerate) {
        Write-Output -NoEnumerate $value
        return
    }
    return $value
}

function Assert-M5ExactProperties {
    param(
        [Parameter(Mandatory)][object]$Object,
        [Parameter(Mandatory)][string[]]$Names,
        [Parameter(Mandatory)][string]$Path
    )
    $actual = @(Get-M5PropertyNames -Object $Object | Sort-Object)
    $expected = @($Names | Sort-Object)
    if (($actual -join "`n") -cne ($expected -join "`n")) {
        throw (
            "$Path properties mismatch expected=$($expected -join ',') " +
            "actual=$($actual -join ',')"
        )
    }
}

function Assert-M5NativeInteger {
    param(
        [Parameter(Mandatory)][object]$Value,
        [Parameter(Mandatory)][string]$Path
    )
    if (@(
        [sbyte], [byte], [int16], [uint16],
        [int32], [uint32], [int64], [uint64]
    ) -notcontains $Value.GetType()) {
        throw "$Path must be a JSON integer."
    }
}

function Assert-M5ZeroInteger {
    param(
        [Parameter(Mandatory)][object]$Object,
        [Parameter(Mandatory)][string]$Name,
        [Parameter(Mandatory)][string]$Path
    )
    $value = Get-M5RequiredValue -Object $Object -Name $Name -Path $Path
    Assert-M5NativeInteger -Value $value -Path "$Path.$Name"
    if ([int64]$value -ne 0) {
        throw "$Path.$Name must be zero."
    }
}

function Assert-M5ExactString {
    param(
        [Parameter(Mandatory)][object]$Object,
        [Parameter(Mandatory)][string]$Name,
        [Parameter(Mandatory)][string]$Expected,
        [Parameter(Mandatory)][string]$Path
    )
    $value = Get-M5RequiredValue -Object $Object -Name $Name -Path $Path
    if ($value -isnot [string] -or $value -cne $Expected) {
        throw "$Path.$Name must equal $Expected."
    }
}

function Assert-M5NonEmptyString {
    param(
        [Parameter(Mandatory)][object]$Object,
        [Parameter(Mandatory)][string]$Name,
        [Parameter(Mandatory)][string]$Path
    )
    $value = Get-M5RequiredValue -Object $Object -Name $Name -Path $Path
    if ($value -isnot [string] -or [string]::IsNullOrWhiteSpace($value)) {
        throw "$Path.$Name must be a non-empty JSON string."
    }
    return $value
}

function Assert-M5True {
    param(
        [Parameter(Mandatory)][object]$Object,
        [Parameter(Mandatory)][string]$Name,
        [Parameter(Mandatory)][string]$Path
    )
    $value = Get-M5RequiredValue -Object $Object -Name $Name -Path $Path
    if ($value -isnot [bool] -or -not $value) {
        throw "$Path.$Name must be true."
    }
}

function Assert-M5TimeString {
    param(
        [Parameter(Mandatory)][object]$Object,
        [Parameter(Mandatory)][string]$Name,
        [Parameter(Mandatory)][string]$Path
    )
    $value = Assert-M5NonEmptyString -Object $Object -Name $Name -Path $Path
    $parsed = [DateTimeOffset]::MinValue
    if (-not [DateTimeOffset]::TryParse(
        $value,
        [System.Globalization.CultureInfo]::InvariantCulture,
        [System.Globalization.DateTimeStyles]::RoundtripKind,
        [ref]$parsed
    )) {
        throw "$Path.$Name must be an ISO-8601 timestamp."
    }
}

function Assert-M5ChecklistComplete {
    param(
        [Parameter(Mandatory)][string]$WorkspaceRoot,
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$Label
    )
    $full = Get-M5WorkspacePath `
        -WorkspaceRoot $WorkspaceRoot `
        -Path $Path `
        -Label $Label
    if (-not [System.IO.File]::Exists($full)) {
        throw "$Label must be an existing file."
    }
    $text = [System.IO.File]::ReadAllText($full)
    if ($text -match '(?m)^\s*-\s+\[\s\]') {
        throw "$Label contains unchecked tasks."
    }
}

function Assert-M5RequiredGateEvidence {
    param(
        [Parameter(Mandatory)][object]$GateResults,
        [Parameter(Mandatory)][string[]]$RequiredGates,
        [Parameter(Mandatory)][string]$EvidenceDir
    )
    if (($RequiredGates -join "`n") -cne
        ($script:M5RequiredGates -join "`n")) {
        throw 'required gate list is not the exact M5 gate contract.'
    }
    $actualKeys = @(Get-M5PropertyNames -Object $GateResults | Sort-Object)
    $expectedKeys = @($RequiredGates | Sort-Object)
    if (($actualKeys -join "`n") -cne ($expectedKeys -join "`n")) {
        throw 'gate results do not exactly match the required M5 gate set.'
    }
    foreach ($name in $RequiredGates) {
        $gate = Get-M5RequiredValue `
            -Object $GateResults `
            -Name $name `
            -Path 'gates'
        Assert-M5ExactProperties -Object $gate -Names @(
            'status',
            'exit_code',
            'command',
            'log',
            'started_utc',
            'ended_utc'
        ) -Path "gates.$name"
        Assert-M5ExactString `
            -Object $gate `
            -Name 'status' `
            -Expected 'PASS' `
            -Path "gates.$name"
        $exitCode = Get-M5RequiredValue `
            -Object $gate `
            -Name 'exit_code' `
            -Path "gates.$name"
        Assert-M5NativeInteger -Value $exitCode -Path "gates.$name.exit_code"
        if ([int64]$exitCode -ne 0) {
            throw "gates.$name.exit_code must be zero."
        }
        [void](Assert-M5NonEmptyString `
            -Object $gate `
            -Name 'command' `
            -Path "gates.$name")
        $log = Assert-M5NonEmptyString `
            -Object $gate `
            -Name 'log' `
            -Path "gates.$name"
        [void](Get-M5EvidenceFile `
            -EvidenceDir $EvidenceDir `
            -Path $log `
            -Label "gates.$name.log")
        Assert-M5TimeString `
            -Object $gate `
            -Name 'started_utc' `
            -Path "gates.$name"
        Assert-M5TimeString `
            -Object $gate `
            -Name 'ended_utc' `
            -Path "gates.$name"
    }
}

function Assert-M5CompletionEvidence {
    param(
        [Parameter(Mandatory)][object]$Evidence,
        [Parameter(Mandatory)][string]$EvidenceDir,
        [Parameter(Mandatory)][string]$WorkspaceRoot,
        [Parameter(Mandatory)][string]$DetailPath,
        [Parameter(Mandatory)][string]$PlanPath
    )
    $topNames = @(
        'schema_version',
        'run_id',
        'started_utc',
        'ended_utc',
        'status',
        'git_status',
        'required_gates',
        'gates',
        'tools',
        'postgresql',
        'artifacts',
        'second_windows_status',
        'second_windows',
        'tc',
        'reviews',
        'protected_media_access_count',
        'residue',
        'failure'
    )
    Assert-M5ExactProperties `
        -Object $Evidence `
        -Names $topNames `
        -Path 'evidence'

    $schemaVersion = Get-M5RequiredValue `
        -Object $Evidence `
        -Name 'schema_version' `
        -Path 'evidence'
    Assert-M5NativeInteger -Value $schemaVersion -Path 'evidence.schema_version'
    if ([int64]$schemaVersion -ne 1) {
        throw 'evidence.schema_version must equal 1.'
    }
    $runID = Assert-M5NonEmptyString `
        -Object $Evidence `
        -Name 'run_id' `
        -Path 'evidence'
    if ($runID -notmatch '^\d{8}-\d{6}-\d{3}-[a-f0-9]{8}$') {
        throw 'evidence.run_id is outside the M5 safe naming contract.'
    }
    Assert-M5TimeString -Object $Evidence -Name 'started_utc' -Path 'evidence'
    Assert-M5TimeString -Object $Evidence -Name 'ended_utc' -Path 'evidence'
    Assert-M5ExactString `
        -Object $Evidence `
        -Name 'status' `
        -Expected 'PASS' `
        -Path 'evidence'
    Assert-M5ExactString `
        -Object $Evidence `
        -Name 'git_status' `
        -Expected 'NO_REPOSITORY' `
        -Path 'evidence'
    $failure = Get-M5RequiredValue `
        -Object $Evidence `
        -Name 'failure' `
        -Path 'evidence' `
        -AllowNull
    if ($null -ne $failure) {
        throw 'evidence.failure must be null for completion.'
    }

    $required = @(
        Get-M5RequiredValue `
            -Object $Evidence `
            -Name 'required_gates' `
            -Path 'evidence'
    )
    if (($required -join "`n") -cne ($script:M5RequiredGates -join "`n")) {
        throw 'evidence.required_gates is not the exact 15-gate contract.'
    }
    $gates = Get-M5RequiredValue `
        -Object $Evidence `
        -Name 'gates' `
        -Path 'evidence'
    Assert-M5RequiredGateEvidence `
        -GateResults $gates `
        -RequiredGates $required `
        -EvidenceDir $EvidenceDir

    $tools = Get-M5RequiredValue `
        -Object $Evidence `
        -Name 'tools' `
        -Path 'evidence'
    $requiredTools = @(
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
    Assert-M5ExactProperties -Object $tools -Names $requiredTools -Path 'tools'
    foreach ($name in $requiredTools) {
        $tool = Get-M5RequiredValue -Object $tools -Name $name -Path 'tools'
        Assert-M5ExactProperties `
            -Object $tool `
            -Names @('path', 'version') `
            -Path "tools.$name"
        $toolPath = Assert-M5NonEmptyString `
            -Object $tool `
            -Name 'path' `
            -Path "tools.$name"
        [void](ConvertTo-M5LexicalLocalPath `
            -Path $toolPath `
            -Label "tools.$name.path")
        [void](Assert-M5NonEmptyString `
            -Object $tool `
            -Name 'version' `
            -Path "tools.$name")
    }

    $postgresql = Get-M5RequiredValue `
        -Object $Evidence `
        -Name 'postgresql' `
        -Path 'evidence'
    Assert-M5ExactProperties `
        -Object $postgresql `
        -Names @('host', 'database') `
        -Path 'postgresql'
    foreach ($name in @('host', 'database')) {
        $value = Assert-M5NonEmptyString `
            -Object $postgresql `
            -Name $name `
            -Path 'postgresql'
        if ($value -match '[:/@?]') {
            throw "postgresql.$name contains URI or credential punctuation."
        }
    }

    $artifacts = Get-M5RequiredValue `
        -Object $Evidence `
        -Name 'artifacts' `
        -Path 'evidence'
    Assert-M5ExactProperties `
        -Object $artifacts `
        -Names @('agent', 'gui', 'helper') `
        -Path 'artifacts'
    foreach ($name in @('agent', 'gui', 'helper')) {
        $artifact = Get-M5RequiredValue `
            -Object $artifacts `
            -Name $name `
            -Path 'artifacts'
        Assert-M5ExactProperties `
            -Object $artifact `
            -Names @('path', 'sha256', 'fresh') `
            -Path "artifacts.$name"
        Assert-M5True -Object $artifact -Name 'fresh' -Path "artifacts.$name"
        $artifactPath = Assert-M5NonEmptyString `
            -Object $artifact `
            -Name 'path' `
            -Path "artifacts.$name"
        $fullArtifact = Get-M5EvidenceFile `
            -EvidenceDir $EvidenceDir `
            -Path $artifactPath `
            -Label "artifacts.$name.path"
        $expectedHash = Assert-M5NonEmptyString `
            -Object $artifact `
            -Name 'sha256' `
            -Path "artifacts.$name"
        if ($expectedHash -notmatch '^[a-f0-9]{64}$') {
            throw "artifacts.$name.sha256 is not lowercase SHA-256."
        }
        $actualHash = (
            Get-FileHash -LiteralPath $fullArtifact -Algorithm SHA256
        ).Hash.ToLowerInvariant()
        if ($actualHash -cne $expectedHash) {
            throw "artifacts.$name hash does not match its fresh file."
        }
    }

    Assert-M5ExactString `
        -Object $Evidence `
        -Name 'second_windows_status' `
        -Expected 'VERIFIED_ON_SECOND_WINDOWS' `
        -Path 'evidence'
    $secondWindows = Get-M5RequiredValue `
        -Object $Evidence `
        -Name 'second_windows' `
        -Path 'evidence'
    Assert-M5ExactProperties -Object $secondWindows -Names @(
        'configured_host',
        'reported_host',
        'status',
        'evidence',
        'sha256'
    ) -Path 'second_windows'
    Assert-M5ExactString `
        -Object $secondWindows `
        -Name 'configured_host' `
        -Expected 'codex-192-168-1-6' `
        -Path 'second_windows'
    [void](Assert-M5NonEmptyString `
        -Object $secondWindows `
        -Name 'reported_host' `
        -Path 'second_windows')
    Assert-M5ExactString `
        -Object $secondWindows `
        -Name 'status' `
        -Expected 'VERIFIED_ON_SECOND_WINDOWS' `
        -Path 'second_windows'
    $remoteEvidence = Assert-M5NonEmptyString `
        -Object $secondWindows `
        -Name 'evidence' `
        -Path 'second_windows'
    $remoteFile = Get-M5EvidenceFile `
        -EvidenceDir $EvidenceDir `
        -Path $remoteEvidence `
        -Label 'second_windows.evidence'
    $remoteHash = Assert-M5NonEmptyString `
        -Object $secondWindows `
        -Name 'sha256' `
        -Path 'second_windows'
    if ($remoteHash -notmatch '^[a-f0-9]{64}$' -or (
        Get-FileHash -LiteralPath $remoteFile -Algorithm SHA256
    ).Hash.ToLowerInvariant() -cne $remoteHash) {
        throw 'second_windows evidence hash is invalid or mismatched.'
    }

    $tc = @(
        Get-M5RequiredValue -Object $Evidence -Name 'tc' -Path 'evidence'
    )
    if ($tc.Count -ne 12) {
        throw 'evidence.tc must contain exactly TC-01 through TC-12.'
    }
    for ($index = 1; $index -le 12; $index++) {
        $entry = $tc[$index - 1]
        $path = "tc[$($index - 1)]"
        Assert-M5ExactProperties `
            -Object $entry `
            -Names @('id', 'status', 'evidence') `
            -Path $path
        Assert-M5ExactString `
            -Object $entry `
            -Name 'id' `
            -Expected ('TC-{0:D2}' -f $index) `
            -Path $path
        Assert-M5ExactString `
            -Object $entry `
            -Name 'status' `
            -Expected 'PASS' `
            -Path $path
        $proof = Assert-M5NonEmptyString `
            -Object $entry `
            -Name 'evidence' `
            -Path $path
        [void](Get-M5EvidenceFile `
            -EvidenceDir $EvidenceDir `
            -Path $proof `
            -Label "$path.evidence")
    }

    $reviews = Get-M5RequiredValue `
        -Object $Evidence `
        -Name 'reviews' `
        -Path 'evidence'
    Assert-M5ExactProperties -Object $reviews -Names @(
        'critical_open',
        'important_open',
        'findings',
        'sources'
    ) -Path 'reviews'
    Assert-M5ZeroInteger `
        -Object $reviews `
        -Name 'critical_open' `
        -Path 'reviews'
    Assert-M5ZeroInteger `
        -Object $reviews `
        -Name 'important_open' `
        -Path 'reviews'
    $findingValue = Get-M5RequiredValue `
        -Object $reviews `
        -Name 'findings' `
        -Path 'reviews' `
        -NoEnumerate
    if ($findingValue -isnot [System.Collections.IList] -or
        $findingValue -is [string]) {
        throw 'reviews.findings must be a JSON array.'
    }
    $findings = @($findingValue)
    foreach ($finding in $findings) {
        if ($finding -isnot [string] -or
            [string]::IsNullOrWhiteSpace($finding)) {
            throw 'reviews.findings entries must be non-empty JSON strings.'
        }
    }
    $sources = @(Get-M5RequiredValue `
        -Object $reviews `
        -Name 'sources' `
        -Path 'reviews')
    if ($sources.Count -eq 0) {
        throw 'reviews.sources must contain review evidence.'
    }
    foreach ($source in $sources) {
        if ($source -isnot [string] -or [string]::IsNullOrWhiteSpace($source)) {
            throw 'reviews.sources entries must be non-empty strings.'
        }
        [void](Get-M5EvidenceFile `
            -EvidenceDir $EvidenceDir `
            -Path $source `
            -Label 'reviews.sources')
    }

    Assert-M5ZeroInteger `
        -Object $Evidence `
        -Name 'protected_media_access_count' `
        -Path 'evidence'
    $residue = Get-M5RequiredValue `
        -Object $Evidence `
        -Name 'residue' `
        -Path 'evidence'
    $residueNames = @(
        'schema',
        'process',
        'pipe',
        'subst',
        'junction',
        'handle',
        'test_root'
    )
    Assert-M5ExactProperties `
        -Object $residue `
        -Names $residueNames `
        -Path 'residue'
    foreach ($name in $residueNames) {
        Assert-M5ZeroInteger -Object $residue -Name $name -Path 'residue'
    }

    Assert-M5ChecklistComplete `
        -WorkspaceRoot $WorkspaceRoot `
        -Path $DetailPath `
        -Label 'M5 detailed checklist'
    Assert-M5ChecklistComplete `
        -WorkspaceRoot $WorkspaceRoot `
        -Path $PlanPath `
        -Label 'M5 implementation plan'

    $serialized = $Evidence | ConvertTo-Json -Depth 32
    if ($serialized -match
        'postgres(?:ql)?://[^/\s:@"]+:[^@\s/"]+@' -or
        $serialized -match '"confirm_token"\s*:\s*"[^"]+"') {
        throw 'completion evidence contains a credential or confirmation token.'
    }
}

function Write-M5CompletionMarker {
    param(
        [Parameter(Mandatory)][object]$Evidence,
        [Parameter(Mandatory)][string]$EvidenceDir,
        [Parameter(Mandatory)][string]$WorkspaceRoot,
        [Parameter(Mandatory)][string]$DetailPath,
        [Parameter(Mandatory)][string]$PlanPath,
        [Parameter(Mandatory)][string]$MarkerPath
    )
    $workspace = ConvertTo-M5LexicalLocalPath `
        -Path $WorkspaceRoot `
        -Label 'workspace root'
    $evidenceRoot = Get-M5WorkspacePath `
        -WorkspaceRoot $workspace `
        -Path $EvidenceDir `
        -Label 'evidence directory'
    if (-not [System.IO.Directory]::Exists($evidenceRoot)) {
        throw 'evidence directory must exist.'
    }
    $marker = Get-M5WorkspacePath `
        -WorkspaceRoot $workspace `
        -Path $MarkerPath `
        -Label 'completion marker'
    Assert-M5LexicalPathWithin `
        -FullRoot $evidenceRoot `
        -FullPath $marker `
        -Label 'completion marker'
    if ([System.IO.File]::Exists($marker) -or
        [System.IO.Directory]::Exists($marker)) {
        throw 'completion marker path already exists.'
    }

    Assert-M5CompletionEvidence `
        -Evidence $Evidence `
        -EvidenceDir $evidenceRoot `
        -WorkspaceRoot $workspace `
        -DetailPath $DetailPath `
        -PlanPath $PlanPath

    $terminal = Get-M5EvidenceFile `
        -EvidenceDir $evidenceRoot `
        -Path (Join-Path $evidenceRoot 'm5-evidence.json') `
        -Label 'terminal evidence'
    $markerValue = [ordered]@{
        schema_version = 1
        run_id = [string]$Evidence.run_id
        status = 'M5_COMPLETE'
        second_windows_status = 'VERIFIED_ON_SECOND_WINDOWS'
        evidence_path = $terminal
        evidence_sha256 = (
            Get-FileHash -LiteralPath $terminal -Algorithm SHA256
        ).Hash.ToLowerInvariant()
        completed_utc = [DateTimeOffset]::UtcNow.ToString('o')
    }
    [System.IO.File]::WriteAllText(
        $marker,
        ($markerValue | ConvertTo-Json -Depth 8),
        [System.Text.UTF8Encoding]::new($false)
    )
    Assert-M5ExistingPathHasNoReparsePoint `
        -FullRoot $evidenceRoot `
        -FullPath $marker `
        -Label 'completion marker'
}
