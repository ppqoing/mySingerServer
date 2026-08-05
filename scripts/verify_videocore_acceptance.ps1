param(
    [Parameter(Mandatory=$true)][string]$StageDir,
    [Parameter(Mandatory=$true)][string]$CorpusDir,
    [Parameter(Mandatory=$true)][string]$EvidenceDir,
    [string]$Runner = ""
)

$ErrorActionPreference = "Stop"

function Resolve-FullPath {
    param([string]$Path)
    if ([IO.Path]::IsPathFullyQualified($Path)) {
        return [IO.Path]::GetFullPath($Path)
    }
    return [IO.Path]::GetFullPath((Join-Path (Get-Location) $Path))
}

function Read-RequiredJson {
    param([string]$Path, [string]$Label)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        throw "VIDEOCORE_ACCEPTANCE_EVIDENCE_MISSING label=$Label"
    }
    try {
        return Get-Content -Raw -LiteralPath $Path | ConvertFrom-Json
    }
    catch {
        throw "VIDEOCORE_ACCEPTANCE_JSON_INVALID label=$Label"
    }
}

function Read-JsonLines {
    param([string]$Path, [string]$Label)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        throw "VIDEOCORE_ACCEPTANCE_EVIDENCE_MISSING label=$Label"
    }
    $items = [Collections.Generic.List[object]]::new()
    foreach ($line in [IO.File]::ReadLines($Path)) {
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        try { $items.Add(($line | ConvertFrom-Json)) }
        catch { throw "VIDEOCORE_ACCEPTANCE_JSONL_INVALID label=$Label line=$($items.Count + 1)" }
    }
    if ($items.Count -eq 0) {
        throw "VIDEOCORE_ACCEPTANCE_JSONL_EMPTY label=$Label"
    }
    return $items.ToArray()
}

function Assert-ProcessTree {
    param([object[]]$Rows, [int]$AgentPID, [string]$StagePath)
    $agentImage = [IO.Path]::GetFullPath((Join-Path $StagePath "agent.exe"))
    $workerImage = [IO.Path]::GetFullPath((Join-Path $StagePath "worker.exe"))
    $parents = @{}
    $agentFound = $false
    foreach ($row in $Rows) {
        $processID = [int]$row.pid
        $parentPID = [int]$row.parent_pid
        $imagePath = [string]$row.image_path
        if ($processID -le 0 -or $parentPID -lt 0 -or [string]::IsNullOrWhiteSpace($imagePath) -or
            [string]::IsNullOrWhiteSpace([string]$row.creation_time_utc) -or
            -not [IO.Path]::IsPathFullyQualified($imagePath) -or $row.path_empty -ne $true) {
            throw "VIDEOCORE_ACCEPTANCE_PROCESS_ROW_INVALID"
        }
        $fullImage = [IO.Path]::GetFullPath($imagePath)
        if ($processID -eq $AgentPID) {
            if (-not [string]::Equals($fullImage, $agentImage, [StringComparison]::OrdinalIgnoreCase)) {
                throw "VIDEOCORE_ACCEPTANCE_AGENT_IMAGE_INVALID"
            }
            $agentFound = $true
        }
        elseif (-not [string]::Equals($fullImage, $workerImage, [StringComparison]::OrdinalIgnoreCase)) {
            throw "VIDEOCORE_ACCEPTANCE_DECODER_CHILD_FOUND"
        }
        $parents[$processID] = $parentPID
    }
    if (-not $agentFound) {
        throw "VIDEOCORE_ACCEPTANCE_AGENT_PROCESS_MISSING"
    }
    foreach ($pidValue in @($parents.Keys)) {
        $processID = [int]$pidValue
        if ($processID -eq $AgentPID) { continue }
        $visited = @{}
        $cursor = $processID
        while ($cursor -ne $AgentPID) {
            if ($visited.ContainsKey($cursor) -or -not $parents.ContainsKey($cursor)) {
                throw "VIDEOCORE_ACCEPTANCE_PROCESS_ANCESTRY_INVALID"
            }
            $visited[$cursor] = $true
            $cursor = [int]$parents[$cursor]
        }
    }
}

function Test-RecoveryCase {
    param($Case, [string]$Label, [switch]$RequireWatchdog)
    if ($null -eq $Case -or $Case.status -cne "pass" -or $Case.pass -ne $true) {
        throw "VIDEOCORE_ACCEPTANCE_CASE_FAILED label=$Label"
    }
    $before = [int]$Case.agent_pid_before
    $after = [int]$Case.agent_pid_after
    $oldWorker = [int]$Case.old_worker_pid
    $replacement = [int]$Case.replacement_worker_pid
    if ($before -le 0 -or $before -ne $after -or $oldWorker -le 0 -or
        $replacement -le 0 -or $oldWorker -eq $replacement -or
        $Case.old_worker_exited -ne $true -or $Case.replacement_ready -ne $true -or
        $Case.fault_task_failed -ne $true -or $Case.followup_done -ne $true) {
        throw "VIDEOCORE_ACCEPTANCE_RECOVERY_INVALID label=$Label"
    }
    if ($RequireWatchdog -and [int64]$Case.watchdog_ms -lt 120000) {
        throw "VIDEOCORE_ACCEPTANCE_WATCHDOG_TOO_SHORT"
    }
}

function Assert-NoSecrets {
    param([string]$Root)
    $dsn = [Environment]::GetEnvironmentVariable("FS_PG_DSN")
    foreach ($file in Get-ChildItem -LiteralPath $Root -File -Recurse) {
        $text = [IO.File]::ReadAllText($file.FullName)
        if ((-not [string]::IsNullOrEmpty($dsn) -and $text.Contains($dsn)) -or
            $text -match '(?i)postgres(?:ql)?://[^\s"'']+' -or
            $text -match '(?i)(password|passwd|pwd)\s*[:=]') {
            throw "VIDEOCORE_ACCEPTANCE_SECRET_FOUND file=$($file.Name)"
        }
    }
}

$stagePath = Resolve-FullPath $StageDir
$corpusPath = Resolve-FullPath $CorpusDir
$evidencePath = Resolve-FullPath $EvidenceDir

try {
    if ([string]::IsNullOrWhiteSpace($Runner)) {
        throw "BLOCKED_NOT_RUN: real VideoCore acceptance runner is not configured"
    }
    $runnerPath = Resolve-FullPath $Runner
    if (-not (Test-Path -LiteralPath $runnerPath -PathType Leaf)) {
        throw "BLOCKED_NOT_RUN: acceptance runner is missing"
    }
    if (-not (Test-Path -LiteralPath $stagePath -PathType Container)) {
        throw "VIDEOCORE_ACCEPTANCE_STAGE_MISSING"
    }
    if (-not (Test-Path -LiteralPath $corpusPath -PathType Container)) {
        throw "VIDEOCORE_ACCEPTANCE_CORPUS_MISSING"
    }
    $stageManifestPath = Join-Path $stagePath "release-manifest.json"
    [void](Read-RequiredJson $stageManifestPath "stage_manifest")
    foreach ($requiredBinary in @("agent.exe", "worker.exe")) {
        if (-not (Test-Path -LiteralPath (Join-Path $stagePath $requiredBinary) -PathType Leaf)) {
            throw "VIDEOCORE_ACCEPTANCE_STAGE_BINARY_MISSING name=$requiredBinary"
        }
    }
    $stageHash = (Get-FileHash -LiteralPath $stageManifestPath -Algorithm SHA256).Hash.ToLowerInvariant()

    [IO.Directory]::CreateDirectory($evidencePath) | Out-Null
    $pwsh = Join-Path $PSHOME "pwsh.exe"
    $startInfo = [Diagnostics.ProcessStartInfo]::new()
    $startInfo.FileName = $pwsh
    $startInfo.UseShellExecute = $false
    $startInfo.CreateNoWindow = $true
    $startInfo.RedirectStandardOutput = $true
    $startInfo.RedirectStandardError = $true
    foreach ($argument in @(
        "-NoProfile", "-File", $runnerPath,
        "-StageDir", $stagePath,
        "-CorpusDir", $corpusPath,
        "-EvidenceDir", $evidencePath
    )) {
        [void]$startInfo.ArgumentList.Add($argument)
    }
    $startInfo.Environment["PATH"] = ""
    $startInfo.Environment["VIDEOCORE_ACCEPTANCE_FORCE_EMPTY_PATH"] = "1"
    $process = [Diagnostics.Process]::new()
    $process.StartInfo = $startInfo
    if (-not $process.Start()) {
        throw "VIDEOCORE_ACCEPTANCE_RUNNER_START_FAILED"
    }
    $stdoutTask = $process.StandardOutput.ReadToEndAsync()
    $stderrTask = $process.StandardError.ReadToEndAsync()
    $process.WaitForExit()
    $runnerExitCode = $process.ExitCode
    $runnerOutput = $stdoutTask.Result + $stderrTask.Result
    $process.Dispose()
    $runnerLog = Join-Path $evidencePath "runner.log"
    [IO.File]::WriteAllText($runnerLog,
        "runner_exit_code=$runnerExitCode output_redacted=true$([Environment]::NewLine)",
        [Text.UTF8Encoding]::new($false))
    $runnerOutput = $null
    if ($runnerExitCode -ne 0) {
        throw "VIDEOCORE_ACCEPTANCE_RUNNER_FAILED exit_code=$runnerExitCode"
    }

    $acceptancePath = Join-Path $evidencePath "acceptance.json"
    $processTreePath = Join-Path $evidencePath "process-tree.jsonl"
    $readyPath = Join-Path $evidencePath "ready.jsonl"
    $document = Read-RequiredJson $acceptancePath "acceptance"
    $processRows = @(Read-JsonLines $processTreePath "process_tree")
    [void](Read-JsonLines $readyPath "ready")

    if ([int]$document.schema_version -ne 1 -or [string]::IsNullOrWhiteSpace([string]$document.run_id) -or
        $document.status -cne "pass" -or $document.pass -ne $true -or
        $document.stage_manifest_sha256 -cne $stageHash) {
        throw "VIDEOCORE_ACCEPTANCE_SUMMARY_INVALID"
    }
    if ($null -eq $document.ac1 -or $document.ac1.status -cne "pass" -or
        $document.ac1.pass -ne $true -or $document.ac1.empty_path -ne $true -or
        [int]$document.ac1.agent_pid -le 0 -or [int]$document.ac1.decoder_children -ne 0) {
        throw "VIDEOCORE_ACCEPTANCE_AC1_INVALID"
    }
    Assert-ProcessTree $processRows ([int]$document.ac1.agent_pid) $stagePath
    Test-RecoveryCase $document.ac2 "AC2"
    Test-RecoveryCase $document.ac3 "AC3" -RequireWatchdog
    if ($null -eq $document.cleanup -or @($document.cleanup.residual_pids).Count -ne 0) {
        throw "VIDEOCORE_ACCEPTANCE_RESIDUAL_PROCESS"
    }
    Assert-NoSecrets $evidencePath

    Write-Host "AC-1 PASS empty_path=true decoder_children=0"
    Write-Host "AC-2 PASS agent_pid_unchanged=true replacement_ready=true followup_done=true"
    Write-Host "AC-3 PASS watchdog_ms>=120000 agent_pid_unchanged=true followup_done=true"
    Write-Host "VIDEOCORE ACCEPTANCE PASS"
    exit 0
}
catch {
    $message = [string]$_.Exception.Message
    if ($message -like "BLOCKED_NOT_RUN*") {
        Write-Host $message
    }
    else {
        Write-Host "VIDEOCORE ACCEPTANCE FAIL"
        Write-Host $message
    }
    exit 1
}
