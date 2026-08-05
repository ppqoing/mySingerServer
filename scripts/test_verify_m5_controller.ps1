[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$repoRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$controllerPath = Join-Path $PSScriptRoot 'verify_m5.ps1'
$pwshPath = (Get-Process -Id $PID).Path
$requiredGates = @(
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
$testRoot = [System.IO.Path]::GetFullPath((
    Join-Path $repoRoot (
        '.superpowers\tmp\m5-controller-selftest-' +
        [Guid]::NewGuid().ToString('N')
    )
))
$evidenceRoot = Join-Path $testRoot 'evidence'
$goPath = Join-Path $testRoot 'go.exe'
$gccPath = Join-Path $testRoot 'gcc.exe'
$detailPath = Join-Path $testRoot 'detail.md'
$planPath = Join-Path $testRoot 'plan.md'
$reviewPath = Join-Path $testRoot 'review.json'
$formatFixtureRoot = Join-Path $testRoot 'format-fixture'
$nonSecretDSN = 'postgresql://127.0.0.1/m5_controller_selftest'

function Write-M5ControllerTestText {
    param(
        [Parameter(Mandatory)][string]$Path,
        [AllowEmptyString()][string]$Text
    )
    $parent = Split-Path -Parent $Path
    if (-not [System.IO.Directory]::Exists($parent)) {
        [System.IO.Directory]::CreateDirectory($parent) | Out-Null
    }
    [System.IO.File]::WriteAllText(
        $Path,
        $Text,
        [System.Text.UTF8Encoding]::new($false)
    )
}

function Invoke-M5ControllerChild {
    param([AllowEmptyString()][string]$FailGate)

    $before = @(
        if (Test-Path -LiteralPath $evidenceRoot -PathType Container) {
            Get-ChildItem -LiteralPath $evidenceRoot -Directory |
                Select-Object -ExpandProperty FullName
        }
    )
    $start = [System.Diagnostics.ProcessStartInfo]::new()
    $start.FileName = $pwshPath
    $start.UseShellExecute = $false
    $start.RedirectStandardOutput = $true
    $start.RedirectStandardError = $true
    $arguments = @(
        '-NoLogo',
        '-NoProfile',
        '-Command',
        (
            'Set-StrictMode -Version 3.0; ' +
            '& $env:M5_TEST_CONTROLLER_PATH ' +
            '-Go $env:M5_TEST_GO_PATH ' +
            '-GCC $env:M5_TEST_GCC_PATH ' +
            '-PGDSN $env:M5_TEST_PG_INPUT; ' +
            'exit $LASTEXITCODE'
        )
    )
    if ($null -ne $start.PSObject.Properties['ArgumentList']) {
        foreach ($argument in $arguments) {
            [void]$start.ArgumentList.Add($argument)
        }
    }
    else {
        $start.Arguments = (($arguments | ForEach-Object {
            '"' + ([string]$_).Replace('"', '\"') + '"'
        }) -join ' ')
    }
    $childCommandText = if (
        $null -ne $start.PSObject.Properties['ArgumentList']
    ) {
        (@($start.ArgumentList) -join ' ')
    }
    else {
        [string]$start.Arguments
    }
    if ($childCommandText.Contains($nonSecretDSN)) {
        throw 'controller self-test put a PostgreSQL DSN on a process command line'
    }
    [object]$childEnvironment = if (
        $null -ne $start.PSObject.Properties['Environment']
    ) {
        Write-Output -NoEnumerate $start.Environment
    }
    else {
        Write-Output -NoEnumerate $start.EnvironmentVariables
    }
    foreach ($name in @($childEnvironment.Keys)) {
        if ($name -match '(?i)(DSN|TOKEN|SECRET|PASSWORD)') {
            [void]$childEnvironment.Remove($name)
        }
    }
    $childEnvironment['M5_TEST_MODE'] = '1'
    $childEnvironment['M5_TEST_CONTROLLER_PATH'] = $controllerPath
    $childEnvironment['M5_TEST_GO_PATH'] = $goPath
    $childEnvironment['M5_TEST_GCC_PATH'] = $gccPath
    $childEnvironment['M5_TEST_PG_INPUT'] = $nonSecretDSN
    $childEnvironment['M5_TEST_EVIDENCE_ROOT'] = $evidenceRoot
    $childEnvironment['M5_TEST_DETAIL_PATH'] = $detailPath
    $childEnvironment['M5_TEST_PLAN_PATH'] = $planPath
    $childEnvironment['M5_REVIEW_EVIDENCE_PATH'] = $reviewPath
    $childEnvironment['M5_TEST_FORMAT_ROOT'] = $formatFixtureRoot
    if ([string]::IsNullOrEmpty($FailGate)) {
        [void]$childEnvironment.Remove('M5_TEST_FAIL_GATE')
    }
    else {
        $childEnvironment['M5_TEST_FAIL_GATE'] = $FailGate
    }

    $process = [System.Diagnostics.Process]::new()
    $process.StartInfo = $start
    if (-not $process.Start()) {
        throw 'failed to start M5 controller child'
    }
    $stdout = $process.StandardOutput.ReadToEnd()
    $stderr = $process.StandardError.ReadToEnd()
    $process.WaitForExit()
    $exitCode = $process.ExitCode
    $process.Dispose()

    $after = @(
        Get-ChildItem -LiteralPath $evidenceRoot -Directory |
            Select-Object -ExpandProperty FullName
    )
    $created = @($after | Where-Object { $before -notcontains $_ })
    if ($created.Count -ne 1) {
        throw (
            "controller invocation created $($created.Count) evidence dirs; " +
            "want exactly 1; stdout=$stdout stderr=$stderr"
        )
    }
    $terminalPath = Join-Path $created[0] 'm5-evidence.json'
    if (-not (Test-Path -LiteralPath $terminalPath -PathType Leaf)) {
        throw "controller invocation lacks terminal JSON: $terminalPath"
    }
    $terminal = Get-Content -LiteralPath $terminalPath -Raw | ConvertFrom-Json
    $persistedText = @(
        Get-ChildItem -LiteralPath $created[0] -Recurse -File |
            Where-Object {
                $_.Extension -in @('.json', '.log', '.txt', '.md')
            } |
            ForEach-Object {
                Get-Content -LiteralPath $_.FullName -Raw
            }
    ) -join "`n"
    if ($persistedText.Contains($nonSecretDSN) -or
        $persistedText -match
            'postgres(?:ql)?://[^/\s:@"]+:[^@\s/"]+@' -or
        $persistedText -match
            '(?i)"?confirm_token"?\s*[:=]\s*"[A-Za-z0-9_-]{16,}"') {
        throw 'controller persisted a DSN, credential URI, or confirmation token'
    }
    return [pscustomobject]@{
        exit_code = $exitCode
        stdout = $stdout
        stderr = $stderr
        evidence_dir = $created[0]
        terminal_path = $terminalPath
        terminal = $terminal
        marker_path = Join-Path $created[0] 'm5-complete.json'
    }
}

[System.IO.Directory]::CreateDirectory($evidenceRoot) | Out-Null
Write-M5ControllerTestText -Path $goPath -Text 'fixture-go'
Write-M5ControllerTestText -Path $gccPath -Text 'fixture-gcc'
Write-M5ControllerTestText -Path $detailPath -Text "- [x] P1 complete`n"
Write-M5ControllerTestText -Path $planPath -Text "- [x] Step complete`n"
Write-M5ControllerTestText `
    -Path (Join-Path $formatFixtureRoot 'owned\owned.go') `
    -Text "package owned`n"
Write-M5ControllerTestText `
    -Path (Join-Path $formatFixtureRoot 'third_party\ignored.go') `
    -Text "package ignored`n"
Write-M5ControllerTestText -Path $reviewPath -Text (
    [ordered]@{
        critical_open = 0
        important_open = 0
        findings = @()
    } | ConvertTo-Json -Depth 4
)

try {
    $tokens = $null
    $errors = $null
    $ast = [System.Management.Automation.Language.Parser]::ParseFile(
        $controllerPath,
        [ref]$tokens,
        [ref]$errors
    )
    if ($errors.Count -ne 0) {
        throw "verify_m5.ps1 has parser errors: $($errors -join '; ')"
    }
    $parameterNames = @($ast.ParamBlock.Parameters.Name.VariablePath.UserPath)
    if (($parameterNames -join ',') -cne 'Go,GCC,PGDSN') {
        throw (
            'verify_m5.ps1 public parameters must be exactly Go,GCC,PGDSN; ' +
            "got $($parameterNames -join ',')"
        )
    }

    $seenRunIDs = [System.Collections.Generic.HashSet[string]]::new(
        [System.StringComparer]::Ordinal
    )
    $lateFailure = $null
    foreach ($gate in $requiredGates) {
        $result = Invoke-M5ControllerChild -FailGate $gate
        if ($result.exit_code -eq 0) {
            throw "injected gate failure returned zero: $gate"
        }
        if ($result.terminal.status -cne 'FAIL') {
            throw "injected gate failure did not write terminal FAIL: $gate"
        }
        if ($result.terminal.gates.$gate.status -cne 'FAIL') {
            $markerDiagnostic = if (
                -not [string]::IsNullOrWhiteSpace(
                    [string]$result.terminal.gates.marker_negative.log
                ) -and
                (Test-Path -LiteralPath (
                    [string]$result.terminal.gates.marker_negative.log
                ) -PathType Leaf)
            ) {
                Get-Content `
                    -LiteralPath $result.terminal.gates.marker_negative.log `
                    -Raw
            }
            else {
                '<no marker log>'
            }
            throw (
                "injected gate was not recorded FAIL: $gate; " +
                "terminal_failure=$($result.terminal.failure); " +
                "marker_log=$markerDiagnostic"
            )
        }
        if (Test-Path -LiteralPath $result.marker_path) {
            throw "injected gate failure wrote completion marker: $gate"
        }
        if (-not $seenRunIDs.Add([string]$result.terminal.run_id)) {
            throw "controller reused a run ID: $($result.terminal.run_id)"
        }
        $allText = (
            $result.stdout,
            $result.stderr,
            (Get-Content -LiteralPath $result.terminal_path -Raw)
        ) -join "`n"
        if ($allText.Contains($nonSecretDSN)) {
            throw "controller leaked even the non-secret test DSN: $gate"
        }
        if ($gate -ceq 'cleanup_audit') {
            $lateFailure = $result
        }
    }

    if ($null -eq $lateFailure) {
        throw 'controller self-test did not retain the late-gate failure'
    }
    $gateNames = @(
        $lateFailure.terminal.gates.PSObject.Properties.Name | Sort-Object
    )
    $wantGateNames = @($requiredGates | Sort-Object)
    if (($gateNames -join "`n") -cne ($wantGateNames -join "`n")) {
        throw 'test-mode terminal gate set is not exact'
    }
    foreach ($gate in $requiredGates | Where-Object {
        $_ -cne 'cleanup_audit'
    }) {
        if ($lateFailure.terminal.gates.$gate.status -cne 'PASS') {
            throw "late-failure prerequisite gate is not PASS: $gate"
        }
    }
    $formatLog = Get-Content `
        -LiteralPath $lateFailure.terminal.gates.format.log `
        -Raw
    if ($formatLog.Trim() -cne 'formatted_files=1') {
        throw (
            'format ownership behavior included an excluded dependency or ' +
            "omitted the owned file: $formatLog"
        )
    }
    $markerLog = Get-Content `
        -LiteralPath $lateFailure.terminal.gates.marker_negative.log `
        -Raw
    if ($markerLog -notmatch
        '(?m)^M5_MARKER_PATH_ORDER_PASS cases=2\r?$' -or
        $markerLog -notmatch
        '(?m)^M5_MARKER_NEGATIVE_PASS cases=21\r?$') {
        throw 'controller marker gate did not run both real marker matrices'
    }

    $noFailGate = Invoke-M5ControllerChild -FailGate ''
    if ($noFailGate.exit_code -eq 0) {
        throw 'test mode without an exact fail gate returned zero'
    }
    if ($noFailGate.terminal.status -cne 'FAIL') {
        throw 'test mode without an exact fail gate did not fail closed'
    }
    if (Test-Path -LiteralPath $noFailGate.marker_path) {
        throw 'test mode without an exact fail gate wrote a completion marker'
    }
    if ([string]$noFailGate.terminal.failure -notmatch
        '(?i)test mode.*fail gate') {
        throw 'test-mode fail-closed terminal diagnostic is not explicit'
    }
    if (-not $seenRunIDs.Add([string]$noFailGate.terminal.run_id)) {
        throw 'test-mode fail-closed run reused a run ID'
    }

    Write-Host (
        "M5_CONTROLLER_NEGATIVE_PASS gates=$($requiredGates.Count) " +
        "unique_runs=$($seenRunIDs.Count) test_mode_no_pass=1"
    )
}
finally {
    $fullRoot = [System.IO.Path]::GetFullPath($testRoot)
    $allowedRoot = [System.IO.Path]::GetFullPath((
        Join-Path $repoRoot '.superpowers\tmp'
    )).TrimEnd('\') + '\'
    if ($fullRoot.StartsWith(
        $allowedRoot,
        [System.StringComparison]::OrdinalIgnoreCase
    ) -and (Split-Path -Leaf $fullRoot).StartsWith(
        'm5-controller-selftest-',
        [System.StringComparison]::Ordinal
    )) {
        Remove-Item -LiteralPath $fullRoot -Recurse -Force
    }
}
