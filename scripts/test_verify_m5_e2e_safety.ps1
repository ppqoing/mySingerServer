[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$script:Workspace = [System.IO.Path]::GetFullPath(
    (Join-Path $PSScriptRoot '..')
)
$script:Harness = Join-Path $PSScriptRoot 'verify_m5_e2e.ps1'
$script:TmpRoot = [System.IO.Path]::GetFullPath(
    (Join-Path $script:Workspace '.superpowers\tmp')
)
$script:FixtureLeaf = 'm5-e2e-safety-fixture-' + [guid]::NewGuid().ToString('N')
$script:FixtureRoot = Join-Path $script:TmpRoot $script:FixtureLeaf
$script:Evidence = Join-Path $script:FixtureRoot 'evidence'
$script:FakeBin = Join-Path $script:FixtureRoot 'fake-bin'

function Assert-Safety {
    param(
        [Parameter(Mandatory)][bool]$Condition,
        [Parameter(Mandatory)][string]$Message
    )
    if (-not $Condition) {
        throw "SAFETY_SELF_TEST_ASSERTION: $Message"
    }
}

function Assert-HarnessParameterContract {
    $tokens = $null
    $parseErrors = $null
    $ast = [System.Management.Automation.Language.Parser]::ParseFile(
        $script:Harness,
        [ref]$tokens,
        [ref]$parseErrors
    )
    Assert-Safety `
        -Condition (@($parseErrors).Count -eq 0) `
        -Message 'harness parameter contract could not be parsed'
    $actual = @(
        $ast.ParamBlock.Parameters |
            ForEach-Object { $_.Name.VariablePath.UserPath }
    )
    $expected = @(
        'Go',
        'PGDSN',
        'HelperExe',
        'AgentExe',
        'GUIExe',
        'EvidenceDir'
    )
    Assert-Safety `
        -Condition (
            $actual.Count -eq $expected.Count -and
            (($actual -join "`n") -ceq ($expected -join "`n"))
        ) `
        -Message (
            'harness must expose exactly the six binding-brief parameters; ' +
            "actual=[$($actual -join ',')]"
        )
}

function New-SafeScenario {
    param([Parameter(Mandatory)][string]$Name)

    $suffix = [guid]::NewGuid().ToString('N')
    $runRoot = Join-Path $script:TmpRoot ("m5-delete-safety-$suffix")
    return [ordered]@{
        schema_version = 1
        mode = 'safety'
        scenario = $Name
        run_id = "safety-$suffix"
        proposed_run_root = $runRoot
        drive_letter = 'Z:'
        occupied_drive_letters = @()
        helper_roots = @('Z:\generated')
        recorded_pids = @(
            [ordered]@{ pid = 424201; identity = 'helper-safety' }
        )
        cleanup_pid = 424201
        cleanup_pid_identity = 'helper-safety'
        recorded_schema = "m5_e2e_$suffix"
        cleanup_schema = "m5_e2e_$suffix"
        recorded_directory = $runRoot
        cleanup_directory = $runRoot
        residues = [ordered]@{
            process = $false
            pipe = $false
            subst = $false
            schema = $false
            junction = $false
            handle = $false
            directory = $false
        }
        perform_exact_path_cleanup = $false
    }
}

function Invoke-SafetyScenario {
    param(
        [Parameter(Mandatory)][hashtable]$Scenario,
        [Parameter(Mandatory)][bool]$ShouldPass,
        [string]$ExpectedCode = ''
    )

    $previous = [Environment]::GetEnvironmentVariable(
        'M5_E2E_TEST_SEAM',
        [EnvironmentVariableTarget]::Process
    )
    try {
        $json = $Scenario | ConvertTo-Json -Depth 8 -Compress
        [Environment]::SetEnvironmentVariable(
            'M5_E2E_TEST_SEAM',
            $json,
            [EnvironmentVariableTarget]::Process
        )
        $output = @(
            & $script:Harness `
                -Go (Join-Path $script:FakeBin 'go.exe') `
                -PGDSN 'host=127.0.0.1 dbname=safety' `
                -HelperExe (Join-Path $script:FakeBin 'helper.exe') `
                -AgentExe (Join-Path $script:FakeBin 'agent.exe') `
                -GUIExe (Join-Path $script:FakeBin 'gui.exe') `
                -EvidenceDir $script:Evidence 2>&1
        )
        if (-not $ShouldPass) {
            throw "SAFETY_SELF_TEST_ASSERTION: scenario '$($Scenario.scenario)' unexpectedly passed"
        }
        Assert-Safety `
            -Condition (($output -join "`n") -match 'SAFETY_SCENARIO_OK') `
            -Message "scenario '$($Scenario.scenario)' omitted success marker"
    }
    catch {
        if ($ShouldPass) {
            throw
        }
        $message = $_.Exception.Message
        Assert-Safety `
            -Condition ($message -match [regex]::Escape($ExpectedCode)) `
            -Message (
                "scenario '{0}' failed with '{1}', expected code '{2}'" -f
                $Scenario.scenario, $message, $ExpectedCode
            )
    }
    finally {
        [Environment]::SetEnvironmentVariable(
            'M5_E2E_TEST_SEAM',
            $previous,
            [EnvironmentVariableTarget]::Process
        )
    }
}

function Invoke-ExternalPathBoundaryScenario {
    param(
        [Parameter(Mandatory)][string]$Name,
        [Parameter(Mandatory)][string]$ParameterName,
        [Parameter(Mandatory)][string]$LiteralPath
    )

    $scenario = New-SafeScenario -Name $Name
    $parameters = @{
        Go = Join-Path $script:FakeBin 'go.exe'
        PGDSN = 'host=127.0.0.1 dbname=safety'
        HelperExe = Join-Path $script:FakeBin 'helper.exe'
        AgentExe = Join-Path $script:FakeBin 'agent.exe'
        GUIExe = Join-Path $script:FakeBin 'gui.exe'
        EvidenceDir = $script:Evidence
    }
    $parameters[$ParameterName] = $LiteralPath
    $previousSeam = [Environment]::GetEnvironmentVariable(
        'M5_E2E_TEST_SEAM',
        [EnvironmentVariableTarget]::Process
    )
    $previousProbeAudit = [Environment]::GetEnvironmentVariable(
        'M5_E2E_PATH_PROBE_AUDIT',
        [EnvironmentVariableTarget]::Process
    )
    $previousSecondWindowsStatus = [Environment]::GetEnvironmentVariable(
        'M5_E2E_SECOND_WINDOWS_STATUS',
        [EnvironmentVariableTarget]::Process
    )
    try {
        [Environment]::SetEnvironmentVariable(
            'M5_E2E_TEST_SEAM',
            ($scenario | ConvertTo-Json -Depth 8 -Compress),
            [EnvironmentVariableTarget]::Process
        )
        [Environment]::SetEnvironmentVariable(
            'M5_E2E_PATH_PROBE_AUDIT',
            '1',
            [EnvironmentVariableTarget]::Process
        )
        [Environment]::SetEnvironmentVariable(
            'M5_E2E_SECOND_WINDOWS_STATUS',
            'VERIFIED_ON_SECOND_WINDOWS',
            [EnvironmentVariableTarget]::Process
        )
        $failed = $false
        $message = ''
        try {
            $null = & $script:Harness @parameters 2>&1
        }
        catch {
            $failed = $true
            $message = $_.Exception.Message
        }
        Assert-Safety `
            -Condition $failed `
            -Message "external path scenario '$Name' unexpectedly passed"
        Assert-Safety `
            -Condition ($message -match 'PATH_BOUNDARY_INVALID') `
            -Message "external path scenario '$Name' failed with '$message'"
        Assert-Safety `
            -Condition ($message -match 'probe_count=0') `
            -Message "external path scenario '$Name' probed before rejection: '$message'"
    }
    finally {
        [Environment]::SetEnvironmentVariable(
            'M5_E2E_TEST_SEAM',
            $previousSeam,
            [EnvironmentVariableTarget]::Process
        )
        [Environment]::SetEnvironmentVariable(
            'M5_E2E_PATH_PROBE_AUDIT',
            $previousProbeAudit,
            [EnvironmentVariableTarget]::Process
        )
        [Environment]::SetEnvironmentVariable(
            'M5_E2E_SECOND_WINDOWS_STATUS',
            $previousSecondWindowsStatus,
            [EnvironmentVariableTarget]::Process
        )
    }
}

function Remove-SelfTestFixture {
    if (-not (Test-Path -LiteralPath $script:FixtureRoot)) {
        return
    }
    $absolute = [System.IO.Path]::GetFullPath($script:FixtureRoot)
    $parent = [System.IO.Path]::GetDirectoryName($absolute)
    $leaf = [System.IO.Path]::GetFileName($absolute)
    Assert-Safety `
        -Condition (
            [string]::Equals(
                $parent,
                $script:TmpRoot,
                [System.StringComparison]::OrdinalIgnoreCase
            )
        ) `
        -Message 'self-test fixture parent changed before cleanup'
    Assert-Safety `
        -Condition ($leaf -eq $script:FixtureLeaf -and $leaf.StartsWith('m5-e2e-safety-fixture-')) `
        -Message 'self-test fixture leaf changed before cleanup'
    $item = Get-Item -LiteralPath $absolute -Force
    Assert-Safety `
        -Condition (
            $item.PSIsContainer -and
            (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -eq 0)
        ) `
        -Message 'self-test fixture is not a plain directory'
    Remove-Item -LiteralPath $absolute -Recurse -Force
}

New-Item -ItemType Directory -Path $script:FakeBin -Force | Out-Null
New-Item -ItemType Directory -Path $script:Evidence -Force | Out-Null
foreach ($name in @('go.exe', 'helper.exe', 'agent.exe', 'worker.exe', 'gui.exe')) {
    Set-Content -LiteralPath (Join-Path $script:FakeBin $name) -Value 'synthetic'
}

$previousSecondWindowsStatus = [Environment]::GetEnvironmentVariable(
    'M5_E2E_SECOND_WINDOWS_STATUS',
    [EnvironmentVariableTarget]::Process
)
try {
    Assert-HarnessParameterContract

    foreach ($case in @(
        [ordered]@{
            name = 'go-protected-descendant'
            parameter = 'Go'
            path = 'I:\tmp\synthetic-go.exe'
        },
        [ordered]@{
            name = 'helper-protected-descendant'
            parameter = 'HelperExe'
            path = 'H:\pik\00000000000\synthetic-helper.exe'
        },
        [ordered]@{
            name = 'agent-protected-root'
            parameter = 'AgentExe'
            path = 'I:\tmp'
        },
        [ordered]@{
            name = 'gui-protected-ancestor'
            parameter = 'GUIExe'
            path = 'H:\pik'
        },
        [ordered]@{
            name = 'evidence-protected-drive-root'
            parameter = 'EvidenceDir'
            path = 'I:\'
        },
        [ordered]@{
            name = 'go-relative-path'
            parameter = 'Go'
            path = 'relative\go.exe'
        }
    )) {
        Invoke-ExternalPathBoundaryScenario `
            -Name $case.name `
            -ParameterName $case.parameter `
            -LiteralPath $case.path
    }

    foreach ($statusCase in @(
        [ordered]@{ name = 'second-windows-unset'; value = $null },
        [ordered]@{
            name = 'second-windows-pending'
            value = 'PENDING_REMOTE_VALIDATION'
        },
        [ordered]@{ name = 'second-windows-waiver'; value = 'USER_WAIVED' },
        [ordered]@{
            name = 'second-windows-arbitrary'
            value = 'OTHER_REMOTE_STATUS'
        }
    )) {
        [Environment]::SetEnvironmentVariable(
            'M5_E2E_SECOND_WINDOWS_STATUS',
            $statusCase.value,
            [EnvironmentVariableTarget]::Process
        )
        $scenario = New-SafeScenario -Name $statusCase.name
        Invoke-SafetyScenario `
            $scenario `
            $false `
            'SECOND_WINDOWS_STATUS_INVALID'
    }
    [Environment]::SetEnvironmentVariable(
        'M5_E2E_SECOND_WINDOWS_STATUS',
        'VERIFIED_ON_SECOND_WINDOWS',
        [EnvironmentVariableTarget]::Process
    )

    $scenario = New-SafeScenario -Name 'run-root-outside'
    $scenario.proposed_run_root = Join-Path $script:Workspace 'm5-delete-outside'
    $scenario.recorded_directory = $scenario.proposed_run_root
    $scenario.cleanup_directory = $scenario.proposed_run_root
    Invoke-SafetyScenario $scenario $false 'RUN_ROOT_INVALID'

    $scenario = New-SafeScenario -Name 'drive-already-occupied'
    $scenario.occupied_drive_letters = @('Z:')
    Invoke-SafetyScenario $scenario $false 'DRIVE_NOT_FREE'

    foreach ($case in @(
        [ordered]@{ name = 'protected-root-a'; root = 'I:\tmp' },
        [ordered]@{ name = 'protected-root-b'; root = 'H:\pik\00000000000' },
        [ordered]@{ name = 'system-root'; root = 'C:\Windows' },
        [ordered]@{ name = 'outside-mapped-run'; root = 'Y:\outside' }
    )) {
        $scenario = New-SafeScenario -Name $case.name
        $scenario.helper_roots = @($case.root)
        Invoke-SafetyScenario $scenario $false 'HELPER_ROOT_INVALID'
    }

    $scenario = New-SafeScenario -Name 'unknown-pid'
    $scenario.cleanup_pid = 424202
    Invoke-SafetyScenario $scenario $false 'CLEANUP_PID_UNVERIFIED'

    $scenario = New-SafeScenario -Name 'wrong-pid-identity'
    $scenario.cleanup_pid_identity = 'different-process'
    Invoke-SafetyScenario $scenario $false 'CLEANUP_PID_UNVERIFIED'

    $scenario = New-SafeScenario -Name 'unknown-schema'
    $scenario.cleanup_schema = 'm5_e2e_unrecorded'
    Invoke-SafetyScenario $scenario $false 'CLEANUP_SCHEMA_UNVERIFIED'

    $scenario = New-SafeScenario -Name 'unknown-directory'
    $scenario.cleanup_directory = Join-Path $script:TmpRoot 'm5-delete-unrecorded'
    Invoke-SafetyScenario $scenario $false 'CLEANUP_DIRECTORY_UNVERIFIED'

    foreach ($residue in @(
        'process', 'pipe', 'subst', 'schema',
        'junction', 'handle', 'directory'
    )) {
        $scenario = New-SafeScenario -Name "residue-$residue"
        $scenario.residues[$residue] = $true
        Invoke-SafetyScenario $scenario $false ('RESIDUE_' + $residue.ToUpperInvariant())
    }

    $scenario = New-SafeScenario -Name 'exact-safe-cleanup'
    $scenario.perform_exact_path_cleanup = $true
    Invoke-SafetyScenario $scenario $true
    Assert-Safety `
        -Condition (-not (Test-Path -LiteralPath $scenario.proposed_run_root)) `
        -Message 'exact synthetic run root remained after successful cleanup'

    Write-Output 'M5_E2E_SAFETY_SELF_TEST_OK scenarios=28'
}
finally {
    [Environment]::SetEnvironmentVariable(
        'M5_E2E_SECOND_WINDOWS_STATUS',
        $previousSecondWindowsStatus,
        [EnvironmentVariableTarget]::Process
    )
    Remove-SelfTestFixture
}
