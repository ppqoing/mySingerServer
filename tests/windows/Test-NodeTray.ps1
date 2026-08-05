[CmdletBinding()]
param(
    [switch]$WhatIf,
    [string]$StageDir = 'C:\tmp\mysingerserver-nodetray-stage',
    [string]$TestRoot,
    [ValidateRange(1024, 65535)]
    [int]$CentralTestPort = 39281,
    [switch]$AllowProcessControl,
    [switch]$AllowUAC,
    [switch]$AllowTaskScheduler,
    [switch]$AllowHKCUStartup,
    [string]$DynamicEvidenceFile,
    [switch]$ValidateEvidenceOnly,
    [string]$BackendExecutorScript
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$script:Blocked = 'BLOCKED_NOT_RUN_DYNAMIC'
$script:ImplementationDependency = 'BLOCKED_IMPLEMENTATION_DEPENDENCY'
$script:FixedTaskPath = '\MySingerServer\DeleteHelper'
$script:RepositoryRoot = [IO.Path]::GetFullPath(
    (Split-Path -Parent (Split-Path -Parent $PSScriptRoot))
).TrimEnd('\')
$script:MediaRoots = @(
    'I:\tmp',
    'H:\pik\00000000000',
    'G:\pik',
    'D:\webdev',
    'D:\m6-generated-corpus'
)

function Assert-Condition {
    param([bool]$Condition, [string]$Code)
    if (-not $Condition) { throw $Code }
}

function Test-SameOrBelow {
    param([string]$Path, [string]$Root)
    $fullPath = [IO.Path]::GetFullPath($Path).TrimEnd('\')
    $fullRoot = [IO.Path]::GetFullPath($Root).TrimEnd('\')
    return $fullPath.Equals($fullRoot, [StringComparison]::OrdinalIgnoreCase) -or
        $fullPath.StartsWith($fullRoot + '\', [StringComparison]::OrdinalIgnoreCase)
}

function Test-ReparsePointInExistingChain {
    param([string]$Path)
    $cursor = [IO.Path]::GetFullPath($Path)
    while (-not [string]::IsNullOrWhiteSpace($cursor)) {
        if (Test-Path -LiteralPath $cursor) {
            $item = Get-Item -LiteralPath $cursor -Force
            if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
                return $true
            }
        }
        $parent = [IO.Directory]::GetParent($cursor)
        if ($null -eq $parent) { break }
        $cursor = $parent.FullName
    }
    return $false
}

function Resolve-GuidTestRoot {
    param([string]$Candidate)
    if ([string]::IsNullOrWhiteSpace($Candidate)) {
        $Candidate = 'C:\tmp\mysingerserver-node-tray-' +
            [Guid]::NewGuid().ToString('N')
    }
    Assert-Condition ([IO.Path]::IsPathRooted($Candidate)) `
        'TEST_ROOT_MUST_BE_ABSOLUTE'
    $full = [IO.Path]::GetFullPath($Candidate).TrimEnd('\')
    Assert-Condition ($Candidate.TrimEnd('\').Equals(
            $full, [StringComparison]::OrdinalIgnoreCase)) `
        'TEST_ROOT_MUST_BE_CANONICAL'
    Assert-Condition ($full -match '(?i)^C:\\tmp\\mysingerserver-node-tray-(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$') `
        'TEST_ROOT_NAME_NOT_GUID_SCOPED'
    Assert-Condition (-not (Test-ReparsePointInExistingChain $full)) `
        'TEST_ROOT_REPARSE_POINT_REJECTED'
    foreach ($mediaRoot in $script:MediaRoots) {
        Assert-Condition (-not (Test-SameOrBelow $full $mediaRoot)) `
            'TEST_ROOT_MEDIA_PATH_REJECTED'
    }
    return $full
}

function Resolve-StageRoot {
    param([string]$Candidate, [string]$ResolvedTestRoot)
    Assert-Condition (-not [string]::IsNullOrWhiteSpace($Candidate)) `
        'STAGE_DIR_REQUIRED'
    Assert-Condition ([IO.Path]::IsPathRooted($Candidate)) `
        'STAGE_DIR_MUST_BE_ABSOLUTE'
    $full = [IO.Path]::GetFullPath($Candidate).TrimEnd('\')
    Assert-Condition ($Candidate.TrimEnd('\').Equals(
            $full, [StringComparison]::OrdinalIgnoreCase)) `
        'STAGE_DIR_MUST_BE_CANONICAL'
    $leaf = Split-Path -Leaf $full
    $cTmp = [IO.Path]::GetFullPath('C:\tmp').TrimEnd('\')
    $workspaceTmp = [IO.Path]::GetFullPath(
        (Join-Path $script:RepositoryRoot '.tmp')
    ).TrimEnd('\')
    $dedicatedStage = (
        (Test-SameOrBelow $full $cTmp) -and
        -not $full.Equals($cTmp, [StringComparison]::OrdinalIgnoreCase) -and
        $leaf -match '(?i)^mysingerserver-nodetray-stage(?:-[a-z0-9][a-z0-9_-]{0,63})?$'
    ) -or (
        (Test-SameOrBelow $full $workspaceTmp) -and
        -not $full.Equals($workspaceTmp, [StringComparison]::OrdinalIgnoreCase) -and
        $leaf -match '(?i)^nodetray-stage(?:-[a-z0-9][a-z0-9_-]{0,63})?$'
    )
    $nestedStage = (Split-Path -Leaf $full).Equals(
        'stage', [StringComparison]::OrdinalIgnoreCase) -and
        (Test-SameOrBelow $full $ResolvedTestRoot) -and
        -not $full.Equals($ResolvedTestRoot, [StringComparison]::OrdinalIgnoreCase)
    Assert-Condition ($dedicatedStage -or $nestedStage) `
        'STAGE_DIR_NOT_DEDICATED'
    Assert-Condition (-not (Test-ReparsePointInExistingChain $full)) `
        'STAGE_DIR_REPARSE_POINT_REJECTED'
    return $full
}

function Get-FileAssessment {
    param([string]$Path)
    $exists = Test-Path -LiteralPath $Path -PathType Leaf
    $length = if ($exists) { (Get-Item -LiteralPath $Path).Length } else { 0 }
    return [ordered]@{
        path = $Path
        exists = $exists
        nonempty = $exists -and $length -gt 0
        length = $length
    }
}

function Get-StageAssessment {
    param([string]$ResolvedStage)
    $files = [ordered]@{
        nodetray = Get-FileAssessment (Join-Path $ResolvedStage 'nodetray.exe')
        webview2_bootstrapper = Get-FileAssessment `
            (Join-Path $ResolvedStage 'MicrosoftEdgeWebview2Setup.exe')
        agent = Get-FileAssessment (Join-Path $ResolvedStage 'agent.exe')
        worker = Get-FileAssessment (Join-Path $ResolvedStage 'worker.exe')
        helper = Get-FileAssessment (Join-Path $ResolvedStage 'helper.exe')
    }
    $blockers = [Collections.Generic.List[string]]::new()
    foreach ($entry in $files.GetEnumerator()) {
        if (-not $entry.Value.exists) {
            $blockers.Add(('STAGE_FILE_MISSING name={0}' -f $entry.Key))
        } elseif (-not $entry.Value.nonempty) {
            $blockers.Add(('STAGE_FILE_EMPTY name={0}' -f $entry.Key))
        }
    }
    return [ordered]@{
        path = $ResolvedStage
        exists = Test-Path -LiteralPath $ResolvedStage -PathType Container
        canonical_absolute = $true
        narrow_path = $true
        read_only_action = '只读检查目录和文件元数据；不执行任何产物'
        files = $files
        blockers = @($blockers)
    }
}

function Get-CurrentUserAssessment {
    $interactive = [Environment]::UserInteractive
    $sessionId = try { (Get-Process -Id $PID).SessionId } catch { -1 }
    $sid = $null
    $isSystem = $false
    $isAdmin = $false
    try {
        $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
        $sid = $identity.User.Value
        $isSystem = $sid -eq 'S-1-5-18'
        $principal = [Security.Principal.WindowsPrincipal]::new($identity)
        $isAdmin = $principal.IsInRole(
            [Security.Principal.WindowsBuiltInRole]::Administrator)
    } catch {
        $sid = $null
    }
    $sidHash = $null
    if (-not [string]::IsNullOrWhiteSpace($sid)) {
        $bytes = [Text.Encoding]::UTF8.GetBytes($sid)
        $sidHash = [Convert]::ToHexString(
            [Security.Cryptography.SHA256]::HashData($bytes)
        ).ToLowerInvariant()
    }
    return [ordered]@{
        interactive = $interactive -and $sessionId -ne 0
        session_id = $sessionId
        sid_present = -not [string]::IsNullOrWhiteSpace($sid)
        sid_sha256 = $sidHash
        is_system = $isSystem
        is_administrator = $isAdmin
        raw_account_name_emitted = $false
    }
}

function Get-CentralPortAssessment {
    param([int]$Port)
    $listeners = [Net.NetworkInformation.IPGlobalProperties]::GetIPGlobalProperties().GetActiveTcpListeners()
    $inUse = @($listeners | Where-Object Port -eq $Port).Count -gt 0
    return [ordered]@{
        address = '127.0.0.1'
        port = $Port
        available = -not $inUse
        method = '只读查询当前 TCP listener；未绑定端口'
        race_note = '实际绑定前仍可能发生竞争，仅作为预检'
    }
}

function Get-ScenarioPlans {
    return @(
        [ordered]@{ id=1; name='单实例与第二实例唤醒'; required_authorization=@('AllowProcessControl') },
        [ordered]@{ id=2; name='Agent 启停、重启、认领与 Worker Ready'; required_authorization=@('AllowProcessControl') },
        [ordered]@{ id=3; name='非目标同名进程拒绝认领或结束'; required_authorization=@('AllowProcessControl') },
        [ordered]@{ id=4; name='Helper 手动 UAC 取消与同意'; required_authorization=@('AllowProcessControl','AllowUAC') },
        [ordered]@{ id=5; name='固定计划任务安装、校验、运行、停止与删除'; required_authorization=@('AllowProcessControl','AllowUAC','AllowTaskScheduler') },
        [ordered]@{ id=6; name='当前用户登录启动启用、禁用与路径漂移'; required_authorization=@('AllowHKCUStartup') },
        [ordered]@{ id=7; name='关闭隐藏与两种退出语义'; required_authorization=@('AllowProcessControl') },
        [ordered]@{ id=8; name='停止超时不自动强杀'; required_authorization=@('AllowProcessControl') },
        [ordered]@{ id=9; name='WebView2 缺失与初始化失败隔离模拟'; required_authorization=@('AllowProcessControl') },
        [ordered]@{ id=10; name='ACL、备份、通知、日志与凭据扫描'; required_authorization=@('AllowProcessControl','AllowUAC') }
    )
}

function Get-AuthorizationValue {
    param([string]$Name)
    switch ($Name) {
        'AllowProcessControl' { return [bool]$AllowProcessControl }
        'AllowUAC' { return [bool]$AllowUAC }
        'AllowTaskScheduler' { return [bool]$AllowTaskScheduler }
        'AllowHKCUStartup' { return [bool]$AllowHKCUStartup }
        default { return $false }
    }
}

function Get-BackendExecutorAssessment {
    param([string]$Candidate)
    $expected = [IO.Path]::GetFullPath(
        (Join-Path $script:RepositoryRoot `
            'tests\windows\Test-NodeTrayBackend.ps1')
    )
    $selected = if ([string]::IsNullOrWhiteSpace($Candidate)) {
        $expected
    } else {
        Assert-Condition ([IO.Path]::IsPathRooted($Candidate)) `
            'BACKEND_EXECUTOR_MUST_BE_ABSOLUTE'
        [IO.Path]::GetFullPath($Candidate)
    }
    Assert-Condition ($selected.Equals(
            $expected, [StringComparison]::OrdinalIgnoreCase)) `
        'BACKEND_EXECUTOR_NOT_REPOSITORY_OWNED'
    $exists = Test-Path -LiteralPath $selected -PathType Leaf
    $capability = $script:ImplementationDependency
    $blockers = [Collections.Generic.List[string]]::new()
    $sha256 = $null
    $parseErrorCount = $null
    if (-not $exists) {
        $blockers.Add('BACKEND_EXECUTOR_SCRIPT_MISSING')
    } else {
        Assert-Condition (-not (Test-ReparsePointInExistingChain $selected)) `
            'BACKEND_EXECUTOR_REPARSE_POINT_REJECTED'
        $source = Get-Content -Raw -LiteralPath $selected
        $sha256 = (Get-FileHash -LiteralPath $selected `
            -Algorithm SHA256).Hash.ToLowerInvariant()
        $parseErrors = @()
        [Management.Automation.Language.Parser]::ParseFile(
            $selected, [ref]$null, [ref]$parseErrors) | Out-Null
        $parseErrorCount = $parseErrors.Count
        if ($parseErrorCount -ne 0) {
            $blockers.Add('BACKEND_EXECUTOR_PARSE_FAILED')
        }
        $marker = [regex]::Match(
            $source,
            "(?m)^\s*\`$script:ExecutorCapability\s*=\s*'([^']+)'\s*`$"
        )
        if (-not $marker.Success) {
            $blockers.Add('BACKEND_EXECUTOR_CAPABILITY_MARKER_MISSING')
        } else {
            $capability = $marker.Groups[1].Value
        }
        foreach ($parameterName in @(
            'DynamicEvidenceFile',
            'AllowProcessControl',
            'AllowUAC',
            'AllowTaskScheduler',
            'AllowHKCUStartup'
        )) {
            if ($source -notmatch ('(?i)\$' + [regex]::Escape($parameterName) + '\b')) {
                $blockers.Add(('BACKEND_EXECUTOR_PARAMETER_MISSING name={0}' -f `
                    $parameterName))
            }
        }
    }
    if ($capability -eq $script:ImplementationDependency) {
        foreach ($code in @(
            'NODETRAY_ACCEPTANCE_CONTROL_CHANNEL_MISSING',
            'BACKEND_SCENARIOS_ARE_FAIL_CLOSED_SKELETONS',
            'DYNAMIC_EVIDENCE_WRITER_NOT_IMPLEMENTED'
        )) { $blockers.Add($code) }
    } elseif ($capability -ne 'READY_FOR_AUTHORIZED_DYNAMIC') {
        $blockers.Add('BACKEND_EXECUTOR_CAPABILITY_INVALID')
        $capability = $script:ImplementationDependency
    }
    if ($blockers.Count -gt 0 -and $capability -eq 'READY_FOR_AUTHORIZED_DYNAMIC') {
        $capability = $script:ImplementationDependency
    }
    return [ordered]@{
        path = $selected
        repository_owned = $true
        exists = $exists
        sha256 = $sha256
        parse_error_count = $parseErrorCount
        capability = $capability
        can_generate_dynamic_evidence = $capability -eq `
            'READY_FOR_AUTHORIZED_DYNAMIC'
        blockers = @($blockers | Sort-Object -Unique)
        code_evidence = @(
            'nodetray/main.go: parseLaunchMode 仅提供 GUI/background/elevated-once',
            'Test-NodeTrayBackend.ps1: 七个场景函数均为 fail-closed 骨架',
            'Test-NodeTrayBackend.ps1: 尚无动态证据 writer'
        )
        action = '只读检查仓库脚本、AST、能力标记和 SHA-256；未执行 executor'
    }
}

function Invoke-RepositoryBackendExecutor {
    param(
        [object]$Executor,
        [object]$Stage,
        [string]$ResolvedTestRoot
    )
    Assert-Condition ($Executor.capability -eq `
            'READY_FOR_AUTHORIZED_DYNAMIC') `
        'BACKEND_EXECUTOR_NOT_READY'
    foreach ($name in @(
        'AllowProcessControl',
        'AllowUAC',
        'AllowTaskScheduler',
        'AllowHKCUStartup'
    )) {
        Assert-Condition (Get-AuthorizationValue $name) `
            ('BACKEND_EXECUTOR_AUTHORIZATION_MISSING switch={0}' -f $name)
    }
    $evidenceFile = Join-Path $ResolvedTestRoot `
        'evidence\dynamic-evidence.json'
    $pwsh = (Get-Process -Id $PID).Path
    $arguments = @(
        '-NoLogo', '-NoProfile', '-File', [string]$Executor.path,
        '-NodeTrayExe', [string]$Stage.files.nodetray.path,
        '-AgentExe', [string]$Stage.files.agent.path,
        '-HelperExe', [string]$Stage.files.helper.path,
        '-TestRoot', $ResolvedTestRoot,
        '-DynamicEvidenceFile', $evidenceFile,
        '-AllowProcessControl',
        '-AllowUAC',
        '-AllowTaskScheduler',
        '-AllowHKCUStartup'
    )
    $raw = @(& $pwsh @arguments 2>&1)
    $exitCode = $LASTEXITCODE
    Assert-Condition ($exitCode -eq 0) `
        'BACKEND_EXECUTOR_DID_NOT_COMPLETE'
    Assert-Condition (Test-Path -LiteralPath $evidenceFile -PathType Leaf) `
        'BACKEND_EXECUTOR_EVIDENCE_MISSING'
    return [ordered]@{
        evidence_file = $evidenceFile
        exit_code = $exitCode
        output_captured_and_not_reemitted = $raw.Count -ge 0
    }
}

function Get-TestRunId {
    param([string]$ResolvedTestRoot)
    $leaf = Split-Path -Leaf $ResolvedTestRoot
    return $leaf.Substring('mysingerserver-node-tray-'.Length).Replace('-', '').ToLowerInvariant()
}

function Assert-LogicalEvidencePath {
    param([string]$Candidate, [string]$ResolvedTestRoot)
    Assert-Condition (-not [string]::IsNullOrWhiteSpace($Candidate)) `
        'EVIDENCE_PATH_REQUIRED'
    Assert-Condition ([IO.Path]::IsPathRooted($Candidate)) `
        'EVIDENCE_PATH_MUST_BE_ABSOLUTE'
    $full = [IO.Path]::GetFullPath($Candidate)
    Assert-Condition ($Candidate.Equals($full, [StringComparison]::OrdinalIgnoreCase)) `
        'EVIDENCE_PATH_MUST_BE_CANONICAL'
    Assert-Condition ((Test-SameOrBelow $full $ResolvedTestRoot) -and
        -not $full.TrimEnd('\').Equals(
            $ResolvedTestRoot, [StringComparison]::OrdinalIgnoreCase)) `
        'EVIDENCE_PATH_OUTSIDE_TEST_ROOT'
    foreach ($mediaRoot in $script:MediaRoots) {
        Assert-Condition (-not (Test-SameOrBelow $full $mediaRoot)) `
            'EVIDENCE_MEDIA_PATH_REJECTED'
    }
    return $full
}

function Assert-Sha256Text {
    param([string]$Value)
    Assert-Condition ($Value -cmatch '^[0-9a-f]{64}$') `
        'EVIDENCE_SHA256_INVALID'
}

function Assert-PhysicalEvidenceFile {
    param([string]$Path, [string]$ExpectedSha256)
    Assert-Condition (Test-Path -LiteralPath $Path -PathType Leaf) `
        'EVIDENCE_FILE_MISSING'
    Assert-Condition (-not (Test-ReparsePointInExistingChain $Path)) `
        'EVIDENCE_FILE_REPARSE_POINT_REJECTED'
    $actual = (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
    Assert-Condition ($actual -ceq $ExpectedSha256) `
        'EVIDENCE_FILE_HASH_MISMATCH'
}

function Get-EvidenceSummaryStatus {
    param([object[]]$Scenarios)
    if (@($Scenarios | Where-Object status -eq 'FAIL').Count -gt 0) {
        return 'FAIL'
    }
    if (@($Scenarios | Where-Object status -eq $script:Blocked).Count -gt 0) {
        return $script:Blocked
    }
    return 'PASS'
}

function Read-AndValidateDynamicEvidence {
    param(
        [string]$EvidenceFile,
        [string]$ResolvedTestRoot,
        [string]$ResolvedStage,
        [object]$CurrentUser,
        [switch]$RequirePhysical
    )
    Assert-Condition (-not [string]::IsNullOrWhiteSpace($EvidenceFile)) `
        'DYNAMIC_EVIDENCE_FILE_REQUIRED'
    Assert-Condition ([IO.Path]::IsPathRooted($EvidenceFile)) `
        'DYNAMIC_EVIDENCE_FILE_MUST_BE_ABSOLUTE'
    $resolvedInput = [IO.Path]::GetFullPath($EvidenceFile)
    Assert-Condition ($EvidenceFile.Equals(
            $resolvedInput, [StringComparison]::OrdinalIgnoreCase)) `
        'DYNAMIC_EVIDENCE_FILE_MUST_BE_CANONICAL'
    Assert-Condition (Test-Path -LiteralPath $resolvedInput -PathType Leaf) `
        'DYNAMIC_EVIDENCE_FILE_MISSING'
    Assert-Condition (-not (Test-ReparsePointInExistingChain $resolvedInput)) `
        'DYNAMIC_EVIDENCE_FILE_REPARSE_POINT_REJECTED'
    if ($RequirePhysical) {
        [void](Assert-LogicalEvidencePath $resolvedInput $ResolvedTestRoot)
    }
    $document = Get-Content -Raw -LiteralPath $resolvedInput |
        ConvertFrom-Json -Depth 24
    Assert-Condition ([int]$document.schema_version -eq 1) `
        'DYNAMIC_EVIDENCE_SCHEMA_UNSUPPORTED'
    Assert-Condition ([string]$document.run_id -ceq (Get-TestRunId $ResolvedTestRoot)) `
        'DYNAMIC_EVIDENCE_RUN_ID_MISMATCH'
    Assert-Condition ([string]$document.test_root -ceq $ResolvedTestRoot) `
        'DYNAMIC_EVIDENCE_TEST_ROOT_MISMATCH'
    Assert-Condition ([string]$document.stage_dir -ceq $ResolvedStage) `
        'DYNAMIC_EVIDENCE_STAGE_DIR_MISMATCH'
    Assert-Condition ([string]$document.current_user_sid_sha256 -ceq
        [string]$CurrentUser.sid_sha256) 'DYNAMIC_EVIDENCE_USER_MISMATCH'

    $authorizationMap = [ordered]@{
        process_control = [bool]$AllowProcessControl
        uac = [bool]$AllowUAC
        task_scheduler = [bool]$AllowTaskScheduler
        hkcu_startup = [bool]$AllowHKCUStartup
    }
    foreach ($name in $authorizationMap.Keys) {
        $property = $document.authorizations.PSObject.Properties[$name]
        Assert-Condition ($null -ne $property -and $property.Value -is [bool]) `
            'DYNAMIC_EVIDENCE_AUTHORIZATION_INVALID'
        if ($RequirePhysical) {
            Assert-Condition ([bool]$property.Value -eq [bool]$authorizationMap[$name]) `
                'DYNAMIC_EVIDENCE_AUTHORIZATION_MISMATCH'
        }
    }

    $expectedStageFiles = @(
        'nodetray.exe',
        'MicrosoftEdgeWebview2Setup.exe',
        'agent.exe',
        'worker.exe',
        'helper.exe'
    )
    $actualStageNames = @($document.stage_files.PSObject.Properties.Name)
    Assert-Condition ($actualStageNames.Count -eq $expectedStageFiles.Count) `
        'DYNAMIC_EVIDENCE_STAGE_FILE_SET_INVALID'
    foreach ($name in $expectedStageFiles) {
        $property = $document.stage_files.PSObject.Properties[$name]
        Assert-Condition ($null -ne $property) `
            'DYNAMIC_EVIDENCE_STAGE_FILE_SET_INVALID'
        $sha = [string]$property.Value
        Assert-Sha256Text $sha
        if ($RequirePhysical) {
            $stagePath = Join-Path $ResolvedStage $name
            Assert-PhysicalEvidenceFile $stagePath $sha
        }
    }

    Assert-Condition ([string]$document.credential_scan_status -ceq 'PASS') `
        'DYNAMIC_EVIDENCE_CREDENTIAL_SCAN_NOT_PASS'
    $scanPath = Assert-LogicalEvidencePath `
        ([string]$document.credential_scan_evidence.path) $ResolvedTestRoot
    $scanSha = [string]$document.credential_scan_evidence.sha256
    Assert-Sha256Text $scanSha
    if ($RequirePhysical) { Assert-PhysicalEvidenceFile $scanPath $scanSha }

    $scenarios = @($document.scenarios)
    Assert-Condition ($scenarios.Count -eq 10) `
        'DYNAMIC_EVIDENCE_SCENARIO_COUNT_INVALID'
    $ids = @($scenarios | ForEach-Object { [int]$_.id } | Sort-Object)
    Assert-Condition (($ids -join ',') -ceq '1,2,3,4,5,6,7,8,9,10') `
        'DYNAMIC_EVIDENCE_SCENARIO_IDS_INVALID'
    foreach ($scenario in $scenarios) {
        $scenarioStatus = [string]$scenario.status
        Assert-Condition ($scenarioStatus -in @('PASS','FAIL',$script:Blocked)) `
            'DYNAMIC_EVIDENCE_SCENARIO_STATUS_INVALID'
        if ($scenarioStatus -ne $script:Blocked) {
            $started = [datetimeoffset]::MinValue
            $ended = [datetimeoffset]::MinValue
            Assert-Condition ([datetimeoffset]::TryParse(
                    [string]$scenario.started_utc, [ref]$started)) `
                'DYNAMIC_EVIDENCE_STARTED_UTC_INVALID'
            Assert-Condition ([datetimeoffset]::TryParse(
                    [string]$scenario.ended_utc, [ref]$ended) -and
                $ended -ge $started) 'DYNAMIC_EVIDENCE_ENDED_UTC_INVALID'
            Assert-Condition (-not [string]::IsNullOrWhiteSpace(
                    [string]$scenario.command)) `
                'DYNAMIC_EVIDENCE_COMMAND_REQUIRED'
            Assert-Condition ($null -ne $scenario.exit_code) `
                'DYNAMIC_EVIDENCE_EXIT_CODE_REQUIRED'
            $plan = @(Get-ScenarioPlans | Where-Object id -eq ([int]$scenario.id))[0]
            $authorizationNames = @{
                AllowProcessControl = 'process_control'
                AllowUAC = 'uac'
                AllowTaskScheduler = 'task_scheduler'
                AllowHKCUStartup = 'hkcu_startup'
            }
            foreach ($required in $plan.required_authorization) {
                $recordedName = $authorizationNames[[string]$required]
                Assert-Condition ([bool]$document.authorizations.$recordedName) `
                    'DYNAMIC_EVIDENCE_REQUIRED_AUTHORIZATION_FALSE'
            }
        }
        if ([bool]$scenario.modified) {
            Assert-Condition ([bool]$scenario.restored) `
                'DYNAMIC_EVIDENCE_RESTORE_REQUIRED'
            $restorationProperty = $scenario.PSObject.Properties[
                'restoration_evidence_files']
            Assert-Condition ($null -ne $restorationProperty -and
                @($restorationProperty.Value).Count -gt 0) `
                'DYNAMIC_EVIDENCE_RESTORE_FILE_REQUIRED'
            foreach ($restorationEvidence in @($restorationProperty.Value)) {
                $restorePath = Assert-LogicalEvidencePath `
                    ([string]$restorationEvidence.path) $ResolvedTestRoot
                $restoreSha = [string]$restorationEvidence.sha256
                Assert-Sha256Text $restoreSha
                if ($RequirePhysical) {
                    Assert-PhysicalEvidenceFile $restorePath $restoreSha
                }
            }
        }
        foreach ($evidence in @($scenario.evidence_files)) {
            $path = Assert-LogicalEvidencePath `
                ([string]$evidence.path) $ResolvedTestRoot
            $sha = [string]$evidence.sha256
            Assert-Sha256Text $sha
            if ($RequirePhysical) { Assert-PhysicalEvidenceFile $path $sha }
        }
        if ($scenarioStatus -ne $script:Blocked) {
            Assert-Condition (@($scenario.evidence_files).Count -gt 0) `
                'DYNAMIC_EVIDENCE_SCENARIO_FILE_REQUIRED'
        }
    }
    return [ordered]@{
        input_file = $resolvedInput
        scenarios = $scenarios
        summary_status = Get-EvidenceSummaryStatus $scenarios
    }
}

function Get-BlockedScenarioResults {
    param(
        [object[]]$Plans,
        [object]$Stage,
        [object]$User,
        [object]$Port,
        [object]$Task
    )
    foreach ($plan in $Plans) {
        $blockers = [Collections.Generic.List[string]]::new()
        foreach ($authorization in $plan.required_authorization) {
            if (-not (Get-AuthorizationValue $authorization)) {
                $blockers.Add(('AUTHORIZATION_MISSING switch={0}' -f $authorization))
            }
        }
        foreach ($blocker in $Stage.blockers) { $blockers.Add($blocker) }
        if (-not $User.interactive) { $blockers.Add('INTERACTIVE_SESSION_REQUIRED') }
        if ($User.is_system) { $blockers.Add('SYSTEM_ACCOUNT_REJECTED') }
        if (-not $Port.available -and $plan.id -in @(1,2,7,9)) {
            $blockers.Add('CENTRAL_TCP_TEST_PORT_UNAVAILABLE')
        }
        if ($plan.id -eq 5 -and -not $Task.command_available) {
            $blockers.Add('TASK_SCHEDULER_CMDLET_UNAVAILABLE')
        }
        $blockers.Add('DYNAMIC_DRIVER_NOT_EXECUTED_IN_CURRENT_AUTHORIZATION_SCOPE')
        [ordered]@{
            id = $plan.id
            name = $plan.name
            required_authorization = $plan.required_authorization
            status = $script:Blocked
            blockers = @($blockers | Sort-Object -Unique)
            modified = $false
            evidence = @()
        }
    }
}

try {
    $resolvedTestRoot = Resolve-GuidTestRoot $TestRoot
    $resolvedStage = Resolve-StageRoot $StageDir $resolvedTestRoot
    $stage = Get-StageAssessment $resolvedStage
    $currentUser = Get-CurrentUserAssessment
    $backendExecutor = Get-BackendExecutorAssessment $BackendExecutorScript
    $executorInvoked = $false
    $centralPort = Get-CentralPortAssessment $CentralTestPort
    $taskAssessment = [ordered]@{
        path = $script:FixedTaskPath
        exact_path = $true
        command_available = $null -ne (Get-Command Get-ScheduledTask -ErrorAction SilentlyContinue)
        action = '只检查 cmdlet 可用性；未枚举、创建、运行、停止或删除任务'
    }
    $plans = Get-ScenarioPlans
    if ($ValidateEvidenceOnly -and -not $WhatIf) {
        throw 'VALIDATE_EVIDENCE_ONLY_REQUIRES_WHATIF'
    }
    if ($ValidateEvidenceOnly) {
        $validatedEvidence = Read-AndValidateDynamicEvidence `
            $DynamicEvidenceFile $resolvedTestRoot $resolvedStage $currentUser
        [ordered]@{
            schema_version = 1
            mode = 'evidence-validation-only'
            evidence_validation_status = 'PASS'
            would_summarize_status = $validatedEvidence.summary_status
            dynamic_acceptance = $script:Blocked
            executor_capability = [string]$backendExecutor.capability
            executor_invoked = $false
            evidence_file = $validatedEvidence.input_file
            scenario_count = @($validatedEvidence.scenarios).Count
            side_effects_performed = $false
        } | ConvertTo-Json -Depth 12
        exit 0
    }
    $preflightBlockers = [Collections.Generic.List[string]]::new()
    foreach ($blocker in $stage.blockers) { $preflightBlockers.Add($blocker) }
    if (-not $currentUser.interactive) { $preflightBlockers.Add('INTERACTIVE_SESSION_REQUIRED_FOR_DYNAMIC') }
    if ($currentUser.is_system) { $preflightBlockers.Add('SYSTEM_ACCOUNT_REJECTED') }
    if (-not $centralPort.available) { $preflightBlockers.Add('CENTRAL_TCP_TEST_PORT_UNAVAILABLE') }
    if (-not $taskAssessment.command_available) { $preflightBlockers.Add('TASK_SCHEDULER_CMDLET_UNAVAILABLE') }
    if ($backendExecutor.capability -ne 'READY_FOR_AUTHORIZED_DYNAMIC') {
        $preflightBlockers.Add('BACKEND_EXECUTOR_BLOCKED_IMPLEMENTATION_DEPENDENCY')
    }

    $preflight = [ordered]@{
        schema_version = 1
        mode = if ($WhatIf) { 'what-if-read-only' } else { 'dynamic-request-fail-closed' }
        preflight_status = if ($preflightBlockers.Count -eq 0) { 'PASS' } else { 'FAIL' }
        dynamic_readiness = if ($preflightBlockers.Count -eq 0) {
            'READY_FOR_EXPLICIT_AUTHORIZATION'
        } else {
            $script:Blocked
        }
        blockers = @($preflightBlockers | Sort-Object -Unique)
        stage = $stage
        test_root = [ordered]@{
            path = $resolvedTestRoot
            exists = Test-Path -LiteralPath $resolvedTestRoot -PathType Container
            canonical_absolute = $true
            guid_scoped = $true
            reparse_chain = 'NONE_DETECTED'
            action = '只读检查；本脚本不会创建或删除测试根'
        }
        current_user = $currentUser
        fixed_task = $taskAssessment
        central_tcp_port = $centralPort
        authorization = [ordered]@{
            process_control = [bool]$AllowProcessControl
            uac = [bool]$AllowUAC
            task_scheduler = [bool]$AllowTaskScheduler
            hkcu_startup = [bool]$AllowHKCUStartup
        }
        backend_executor = $backendExecutor
        executor_capability = [string]$backendExecutor.capability
        executor_invoked = $executorInvoked
        scenarios = $plans
        side_effects_performed = $false
    }

    if ($WhatIf) {
        $preflight | ConvertTo-Json -Depth 16
        exit 0
    }

    $evidenceToAggregate = $DynamicEvidenceFile
    if ([string]::IsNullOrWhiteSpace($evidenceToAggregate) -and
        $backendExecutor.capability -eq 'READY_FOR_AUTHORIZED_DYNAMIC' -and
        [bool]$AllowProcessControl -and [bool]$AllowUAC -and
        [bool]$AllowTaskScheduler -and [bool]$AllowHKCUStartup -and
        @($preflightBlockers | Where-Object {
            $_ -ne 'BACKEND_EXECUTOR_BLOCKED_IMPLEMENTATION_DEPENDENCY'
        }).Count -eq 0) {
        $executorResult = Invoke-RepositoryBackendExecutor `
            $backendExecutor $stage $resolvedTestRoot
        $executorInvoked = $true
        $evidenceToAggregate = [string]$executorResult.evidence_file
    }

    if (-not [string]::IsNullOrWhiteSpace($evidenceToAggregate)) {
        $validatedEvidence = Read-AndValidateDynamicEvidence `
            $evidenceToAggregate $resolvedTestRoot $resolvedStage $currentUser `
            -RequirePhysical
        $summaryStatus = [string]$validatedEvidence.summary_status
        $validatedScenarios = @($validatedEvidence.scenarios)
        $passCount = @($validatedScenarios | Where-Object status -eq 'PASS').Count
        $failCount = @($validatedScenarios | Where-Object status -eq 'FAIL').Count
        $blockedCount = @($validatedScenarios |
            Where-Object status -eq $script:Blocked).Count
        [ordered]@{
            schema_version = 1
            mode = 'authorized-evidence-aggregation'
            status = $summaryStatus
            dynamic_acceptance = $summaryStatus
            preflight = $preflight
            evidence_file = $validatedEvidence.input_file
            executor_capability = [string]$backendExecutor.capability
            executor_invoked = $executorInvoked
            scenarios = $validatedScenarios
            summary = [ordered]@{
                pass = $passCount
                fail = $failCount
                blocked = $blockedCount
                blocked_is_pass = $false
            }
            dynamic_operations_performed_by_script = $false
            side_effects_performed = $false
        } | ConvertTo-Json -Depth 18
        if ($summaryStatus -eq 'PASS') { exit 0 }
        if ($summaryStatus -eq 'FAIL') { exit 1 }
        exit 2
    }

    $scenarioResults = @(Get-BlockedScenarioResults `
        $plans $stage $currentUser $centralPort $taskAssessment)
    $allAuthorizations = [bool]$AllowProcessControl -and [bool]$AllowUAC -and
        [bool]$AllowTaskScheduler -and [bool]$AllowHKCUStartup
    $environmentBlockers = @($preflightBlockers | Where-Object {
        $_ -ne 'BACKEND_EXECUTOR_BLOCKED_IMPLEMENTATION_DEPENDENCY'
    })
    $blockedStatus = if ($allAuthorizations -and
        $environmentBlockers.Count -eq 0 -and
        $backendExecutor.capability -eq $script:ImplementationDependency) {
        $script:ImplementationDependency
    } else { $script:Blocked }
    [ordered]@{
        schema_version = 1
        mode = 'dynamic-request-fail-closed'
        status = $blockedStatus
        dynamic_acceptance = $script:Blocked
        implementation_dependency = $backendExecutor
        executor_capability = [string]$backendExecutor.capability
        executor_invoked = $executorInvoked
        preflight = $preflight
        scenarios = $scenarioResults
        summary = [ordered]@{
            pass = 0
            fail = 0
            blocked = $scenarioResults.Count
            blocked_is_pass = $false
        }
        side_effects_performed = $false
    } | ConvertTo-Json -Depth 18
    exit 2
} catch {
    [ordered]@{
        schema_version = 1
        mode = if ($WhatIf) { 'what-if-read-only' } else { 'dynamic-request-fail-closed' }
        status = 'FAIL'
        error_code = [string]$_.Exception.Message
        dynamic_acceptance = $script:Blocked
        side_effects_performed = $false
    } | ConvertTo-Json -Depth 8
    exit 1
}
