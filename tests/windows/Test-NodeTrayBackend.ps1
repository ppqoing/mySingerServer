[CmdletBinding()]
param(
    [switch]$WhatIf,
    [string]$NodeTrayExe,
    [string]$AgentExe,
    [string]$HelperExe,
    [string]$TestRoot,
    [string]$DynamicEvidenceFile,
    [switch]$AllowProcessControl,
    [switch]$AllowUAC,
    [switch]$AllowTaskScheduler,
    [switch]$AllowHKCUStartup
)

$ErrorActionPreference = 'Stop'
$script:FixedTaskPath = '\MySingerServer\DeleteHelper'
$script:FixedRunValue = 'MySingerServerNodeTray'
$script:DynamicStatus = 'BLOCKED_NOT_RUN_DYNAMIC'
$script:ExecutorCapability = 'BLOCKED_IMPLEMENTATION_DEPENDENCY'
$script:RepositoryRoot = [IO.Path]::GetFullPath(
    (Split-Path -Parent (Split-Path -Parent $PSScriptRoot))
).TrimEnd('\')

function Assert-NodeTrayCondition {
    param([bool]$Condition, [string]$Code)
    if (-not $Condition) {
        throw $Code
    }
}

function Test-IsSameOrBelowPath {
    param([string]$Path, [string]$Root)
    $fullPath = [IO.Path]::GetFullPath($Path).TrimEnd('\')
    $fullRoot = [IO.Path]::GetFullPath($Root).TrimEnd('\')
    return $fullPath.Equals(
            $fullRoot,
            [StringComparison]::OrdinalIgnoreCase
        ) -or $fullPath.StartsWith(
            $fullRoot + '\',
            [StringComparison]::OrdinalIgnoreCase
        )
}

function Test-ExistingPathChainHasReparsePoint {
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
        if ($null -eq $parent) {
            break
        }
        $cursor = $parent.FullName
    }
    return $false
}

function Resolve-SafeTestRoot {
    param([string]$Candidate)
    if ([string]::IsNullOrWhiteSpace($Candidate)) {
        $Candidate = Join-Path `
            (Join-Path $script:RepositoryRoot '.tmp') `
            'mysingerserver-node-tray-backend'
    }
    Assert-NodeTrayCondition `
        ([IO.Path]::IsPathRooted($Candidate)) `
        'TEST_ROOT_MUST_BE_ABSOLUTE'
    $full = [IO.Path]::GetFullPath($Candidate).TrimEnd('\')
    Assert-NodeTrayCondition `
        ($Candidate.TrimEnd('\').Equals(
            $full,
            [StringComparison]::OrdinalIgnoreCase
        )) `
        'TEST_ROOT_MUST_BE_CANONICAL'

    $driveRoot = [IO.Path]::GetPathRoot($full).TrimEnd('\')
    Assert-NodeTrayCondition `
        (-not $full.Equals($driveRoot, [StringComparison]::OrdinalIgnoreCase)) `
        'TEST_ROOT_MUST_NOT_BE_VOLUME_ROOT'
    Assert-NodeTrayCondition `
        (-not $full.Equals(
            $script:RepositoryRoot,
            [StringComparison]::OrdinalIgnoreCase
        )) `
        'TEST_ROOT_MUST_NOT_BE_REPOSITORY_ROOT'

    $profile = [Environment]::GetFolderPath('UserProfile').TrimEnd('\')
    if (-not [string]::IsNullOrWhiteSpace($profile)) {
        Assert-NodeTrayCondition `
            (-not $full.Equals($profile, [StringComparison]::OrdinalIgnoreCase)) `
            'TEST_ROOT_MUST_NOT_BE_USER_PROFILE_ROOT'
    }

    $safeRoots = @(
        [IO.Path]::GetFullPath('C:\tmp').TrimEnd('\'),
        [IO.Path]::GetFullPath(
            (Join-Path $script:RepositoryRoot '.tmp')
        ).TrimEnd('\')
    )
    $safeRoot = $safeRoots | Where-Object {
        (Test-IsSameOrBelowPath -Path $full -Root $_) -and
        -not $full.Equals($_, [StringComparison]::OrdinalIgnoreCase)
    } | Select-Object -First 1
    Assert-NodeTrayCondition `
        (-not [string]::IsNullOrWhiteSpace($safeRoot)) `
        'TEST_ROOT_OUTSIDE_APPROVED_TEST_ROOTS'

    $leaf = Split-Path -Leaf $full
    Assert-NodeTrayCondition `
        ($leaf.Equals(
            'mysingerserver-node-tray-backend',
            [StringComparison]::OrdinalIgnoreCase
        ) -or $leaf.StartsWith(
            'mysingerserver-node-tray-backend-',
            [StringComparison]::OrdinalIgnoreCase
        ) -or $leaf -match `
            '(?i)^mysingerserver-node-tray-(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$') `
        'TEST_ROOT_NAME_NOT_DEDICATED'
    Assert-NodeTrayCondition `
        (-not (Test-ExistingPathChainHasReparsePoint -Path $full)) `
        'TEST_ROOT_REPARSE_POINT_REJECTED'

    foreach ($mediaRoot in @(
            'I:\tmp',
            'H:\pik\00000000000',
            'G:\pik',
            'D:\webdev',
            'D:\m6-generated-corpus'
        )) {
        Assert-NodeTrayCondition `
            (-not (Test-IsSameOrBelowPath -Path $full -Root $mediaRoot)) `
            'TEST_ROOT_MEDIA_PATH_REJECTED'
    }
    return $full
}

function Get-BinaryAssessment {
    param(
        [string]$InputPath,
        [string]$DefaultRelativePath,
        [string]$ExpectedBaseName
    )
    $usedDefault = [string]::IsNullOrWhiteSpace($InputPath)
    $candidate = if ($usedDefault) {
        Join-Path $script:RepositoryRoot $DefaultRelativePath
    } else {
        $InputPath
    }
    $absolute = [IO.Path]::IsPathRooted($candidate)
    $full = if ($absolute) {
        [IO.Path]::GetFullPath($candidate)
    } else {
        [IO.Path]::GetFullPath((Join-Path $script:RepositoryRoot $candidate))
    }
    $canonical = $absolute -and $candidate.Equals(
        $full,
        [StringComparison]::OrdinalIgnoreCase
    )
    $baseNameValid = ([IO.Path]::GetFileName($full)).Equals(
        $ExpectedBaseName,
        [StringComparison]::OrdinalIgnoreCase
    )
    $exists = Test-Path -LiteralPath $full -PathType Leaf
    $ready = $canonical -and $baseNameValid -and $exists
    $reason = if (-not $absolute) {
        '路径不是绝对路径'
    } elseif (-not $canonical) {
        '路径不是规范绝对路径'
    } elseif (-not $baseNameValid) {
        'basename 不符合固定合同'
    } elseif (-not $exists) {
        '文件不存在'
    } else {
        ''
    }
    return [ordered]@{
        expected_basename = $ExpectedBaseName
        path = $full
        used_default = $usedDefault
        canonical_absolute = $canonical
        basename_valid = $baseNameValid
        exists = $exists
        dynamic_status = if ($ready) { 'READY_FOR_AUTHORIZED_DYNAMIC' } else { $script:DynamicStatus }
        blocked_reason = $reason
    }
}

function Get-TaskSchedulerAssessment {
    try {
        $service = Get-Service -Name 'Schedule' -ErrorAction Stop
        return [ordered]@{
            queryable = $true
            status = [string]$service.Status
            action = '只读查询服务状态；未启动、停止或修改服务'
        }
    } catch {
        return [ordered]@{
            queryable = $false
            status = 'Unavailable'
            action = '只读查询失败；未启动、停止或修改服务'
        }
    }
}

function New-ScenarioPlan {
    param(
        [int]$Id,
        [string]$Name,
        [string[]]$RequiredAuthorization,
        [string]$SingleTarget,
        [string]$SavedState,
        [string]$MissingProtocol
    )
    return [ordered]@{
        id = $Id
        name = $Name
        required_authorization = $RequiredAuthorization
        single_target = $SingleTarget
        state_before_action = $SavedState
        finally_restore = '仅恢复脚本成功修改的上述单一固定测试目标'
        status = $script:DynamicStatus
        blocked_reason = $MissingProtocol
    }
}

function Get-DynamicScenarioPlans {
    param([string]$ResolvedTestRoot)
    return @(
        (New-ScenarioPlan 1 'Agent 认领、启动、受控停止及停止超时不强杀' @('非 WhatIf 动态会话') 'TestRoot 内 Agent 受控测试实例' '保存测试实例初始状态；不扫描其他进程' '缺少 nodetray 受控验收协议，禁止按进程名替代'),
        (New-ScenarioPlan 2 'Helper 手动 UAC 取消与同意' @('AllowUAC') 'TestRoot 内 Helper 受控测试实例' '保存 Helper 受控测试实例状态' '缺少交互式 UAC 验收驱动协议'),
        (New-ScenarioPlan 3 'Helper 固定任务安装、定义验证、运行、停止与删除' @('AllowUAC','AllowTaskScheduler') $script:FixedTaskPath '仅保存固定测试任务原状态' '缺少固定任务安全快照/恢复验收协议'),
        (New-ScenarioPlan 4 '当前用户登录启动启用与禁用' @('AllowHKCUStartup') ('HKCU Run value: ' + $script:FixedRunValue) '仅保存固定 Run value 原状态' '缺少固定 Run value 安全快照/恢复验收协议'),
        (New-ScenarioPlan 5 '临时 Helper 配置 ACL、原子替换及 last-good 恢复' @('AllowUAC') (Join-Path $ResolvedTestRoot 'config\helper.json') '保存测试配置和单份 last-good 原状态' '缺少提权配置写入受控验收协议'),
        (New-ScenarioPlan 6 'ExitTray(false) 后组件保持运行' @('非 WhatIf 动态会话') 'TestRoot 内已认领测试实例' '保存受控实例 identity；不接管其他实例' '缺少 ExitTray 受控验收协议'),
        (New-ScenarioPlan 7 'ExitTray(true) 受控等待和超时人工选择' @('非 WhatIf 动态会话') 'TestRoot 内已认领测试实例' '保存受控实例 identity；超时不自动强杀' '缺少超时交互选择受控验收协议')
    )
}

function New-BlockedScenarioResult {
    param(
        [hashtable]$Plan,
        [string[]]$Blockers
    )
    return [ordered]@{
        id = $Plan.id
        name = $Plan.name
        required_authorization = $Plan.required_authorization
        single_target = $Plan.single_target
        state_before_action = $Plan.state_before_action
        finally_restore = $Plan.finally_restore
        status = $script:DynamicStatus
        blocked_reason = ($Blockers -join '；')
        modified = $false
        restored = $false
    }
}

function Invoke-AgentLifecycleScenario {
    param([hashtable]$Context, [hashtable]$Plan)
    $blockers = @()
    if (-not $Context.BinariesReady) { $blockers += '测试二进制未全部就绪' }
    $blockers += 'nodetray 尚未提供只面向 TestRoot 的受控 Agent 验收协议'
    return New-BlockedScenarioResult $Plan $blockers
}

function Invoke-ManualHelperScenario {
    param([hashtable]$Context, [hashtable]$Plan)
    $blockers = @()
    if (-not $Context.AllowUAC) { $blockers += '缺少 AllowUAC 独立授权' }
    if (-not $Context.BinariesReady) { $blockers += '测试二进制未全部就绪' }
    $blockers += '尚无可审计的 UAC 取消/同意自动验收驱动协议'
    return New-BlockedScenarioResult $Plan $blockers
}

function Invoke-HelperTaskScenario {
    param([hashtable]$Context, [hashtable]$Plan)
    $blockers = @()
    if (-not $Context.AllowUAC) { $blockers += '缺少 AllowUAC 独立授权' }
    if (-not $Context.AllowTaskScheduler) { $blockers += '缺少 AllowTaskScheduler 独立授权' }
    if (-not $Context.TaskSchedulerQueryable) { $blockers += 'Task Scheduler 服务不可只读查询' }
    $blockers += '尚无只保存/恢复固定 TaskPath 的受控验收协议'
    return New-BlockedScenarioResult $Plan $blockers
}

function Invoke-HKCUStartupScenario {
    param([hashtable]$Context, [hashtable]$Plan)
    $blockers = @()
    if (-not $Context.AllowHKCUStartup) { $blockers += '缺少 AllowHKCUStartup 独立授权' }
    $blockers += '尚无只保存/恢复固定 Run value 的受控验收协议'
    return New-BlockedScenarioResult $Plan $blockers
}

function Invoke-HelperConfigScenario {
    param([hashtable]$Context, [hashtable]$Plan)
    $blockers = @()
    if (-not $Context.AllowUAC) { $blockers += '缺少 AllowUAC 独立授权' }
    $blockers += '尚无只写 TestRoot 且可验证 Owner/DACL/replace/last-good 的受控协议'
    return New-BlockedScenarioResult $Plan $blockers
}

function Invoke-ExitKeepRunningScenario {
    param([hashtable]$Context, [hashtable]$Plan)
    $blockers = @()
    if (-not $Context.BinariesReady) { $blockers += '测试二进制未全部就绪' }
    $blockers += '尚无可证明组件 identity 保持不变的 ExitTray(false) 受控协议'
    return New-BlockedScenarioResult $Plan $blockers
}

function Invoke-ExitStopScenario {
    param([hashtable]$Context, [hashtable]$Plan)
    $blockers = @()
    if (-not $Context.BinariesReady) { $blockers += '测试二进制未全部就绪' }
    $blockers += '尚无可注入停止超时和人工选择的 ExitTray(true) 受控协议'
    return New-BlockedScenarioResult $Plan $blockers
}

function Invoke-DynamicAcceptance {
    param(
        [object[]]$Plans,
        [hashtable]$Context
    )
    return @(
        (Invoke-AgentLifecycleScenario $Context $Plans[0]),
        (Invoke-ManualHelperScenario $Context $Plans[1]),
        (Invoke-HelperTaskScenario $Context $Plans[2]),
        (Invoke-HKCUStartupScenario $Context $Plans[3]),
        (Invoke-HelperConfigScenario $Context $Plans[4]),
        (Invoke-ExitKeepRunningScenario $Context $Plans[5]),
        (Invoke-ExitStopScenario $Context $Plans[6])
    )
}

try {
    $resolvedRoot = Resolve-SafeTestRoot $TestRoot
    $binaries = [ordered]@{
        nodetray = Get-BinaryAssessment $NodeTrayExe 'artifacts\stage\nodetray.exe' 'nodetray.exe'
        agent = Get-BinaryAssessment $AgentExe 'artifacts\stage\agent.exe' 'agent.exe'
        helper = Get-BinaryAssessment $HelperExe 'artifacts\stage\helper.exe' 'helper.exe'
    }
    $binaryReady = @($binaries.Values | Where-Object {
            $_.dynamic_status -ne 'READY_FOR_AUTHORIZED_DYNAMIC'
        }).Count -eq 0
    $scheduler = Get-TaskSchedulerAssessment
    $sid = try {
        [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    } catch {
        'UNAVAILABLE'
    }
    $plans = Get-DynamicScenarioPlans $resolvedRoot

    $preflight = [ordered]@{
        schema_version = 1
        mode = if ($WhatIf) { 'what-if-read-only' } else { 'authorized-dynamic-request' }
        status = 'PASS'
        powershell_version = $PSVersionTable.PSVersion.ToString()
        windows_version = [Environment]::OSVersion.VersionString
        current_user_sid = $sid
        task_scheduler = $scheduler
        test_root = [ordered]@{
            path = $resolvedRoot
            approved_roots = @('C:\tmp', (Join-Path $script:RepositoryRoot '.tmp'))
            reparse_chain = 'NONE_DETECTED'
            action = '只显示；WhatIf 不创建目录或证据'
        }
        binaries = $binaries
        fixed_targets = [ordered]@{
            task_path = $script:FixedTaskPath
            hkcu_run_value = $script:FixedRunValue
            task_action = '将使用；WhatIf 不连接、枚举或修改任务'
            run_action = '将使用；WhatIf 不查询或修改注册表值'
        }
        evidence_directory = [ordered]@{
            path = Join-Path $resolvedRoot 'evidence'
            action = '将使用；WhatIf 不创建或写入'
            redaction = '禁止 DSN、password、token 和媒体路径'
        }
        scenario_plans = $plans
        executor_contract = [ordered]@{
            capability = $script:ExecutorCapability
            repository_executor = $true
            can_generate_dynamic_evidence = $false
            requested_evidence_file = $DynamicEvidenceFile
            required_authorization = @(
                'AllowProcessControl',
                'AllowUAC',
                'AllowTaskScheduler',
                'AllowHKCUStartup'
            )
            blockers = @(
                'NODETRAY_ACCEPTANCE_CONTROL_CHANNEL_MISSING',
                'BACKEND_SCENARIOS_ARE_FAIL_CLOSED_SKELETONS',
                'DYNAMIC_EVIDENCE_WRITER_NOT_IMPLEMENTED'
            )
        }
        dynamic_acceptance = $script:DynamicStatus
    }

    if ($WhatIf) {
        $preflight | ConvertTo-Json -Depth 12
        exit 0
    }

    $context = @{
        AllowUAC = [bool]$AllowUAC
        AllowTaskScheduler = [bool]$AllowTaskScheduler
        AllowHKCUStartup = [bool]$AllowHKCUStartup
        BinariesReady = $binaryReady
        TaskSchedulerQueryable = [bool]$scheduler.queryable
    }
    $results = Invoke-DynamicAcceptance $plans $context
    [ordered]@{
        schema_version = 1
        mode = 'authorized-dynamic-request'
        status = $script:ExecutorCapability
        executor_capability = $script:ExecutorCapability
        dynamic_acceptance = $script:DynamicStatus
        preflight = $preflight
        scenarios = $results
        summary = [ordered]@{
            pass = @($results | Where-Object status -eq 'PASS').Count
            fail = @($results | Where-Object status -eq 'FAIL').Count
            blocked = @($results | Where-Object status -eq $script:DynamicStatus).Count
            note = 'blocked 不计为 pass；当前骨架未执行任何系统变更'
        }
    } | ConvertTo-Json -Depth 12
    exit 2
} catch {
    [ordered]@{
        schema_version = 1
        mode = if ($WhatIf) { 'what-if-read-only' } else { 'authorized-dynamic-request' }
        status = 'FAIL'
        error_code = [string]$_.Exception.Message
        dynamic_acceptance = $script:DynamicStatus
    } | ConvertTo-Json -Depth 6
    exit 1
}
