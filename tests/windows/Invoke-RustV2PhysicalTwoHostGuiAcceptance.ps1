<#
.SYNOPSIS
编排一次双实体机 Rust V2 GUI 验收，默认只允许显式注入的执行提供者。

.DESCRIPTION
本脚本不自动操作 GUI。它先完成只读预检与安全边界检查，随后通过提供者执行隔离操作，
等待 desktop.exe 正常退出后才调用只读观察器，并把完整事实交给独立报告器裁决。
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

function Invoke-RustV2PhysicalGuiProvider {
    <# 调用显式注入的外部边界；测试替身据此记录每个真实 argv/调用顺序。 #>
    param(
        [Parameter(Mandatory)] [hashtable] $Provider,
        [Parameter(Mandatory)] [string] $Operation,
        [Parameter(Mandatory)] $Arguments
    )

    if (-not $Provider.ContainsKey($Operation) -or $Provider[$Operation] -isnot [scriptblock]) {
        throw "RUST_V2_PHYSICAL_GUI_PROVIDER_MISSING operation=$Operation"
    }
    & $Provider[$Operation] $Arguments
}

function Test-RustV2PhysicalGuiRootSet {
    <# 校验固定四媒体根，拒绝 I:\Tool、媒体根子目录和任何未设计的真实写入目标。 #>
    param(
        [Parameter(Mandatory)] [string[]] $LocalMediaRoots,
        [Parameter(Mandatory)] [string[]] $RemoteMediaRoots
    )

    $expectedLocal = @('H:\pik\00000000000', 'I:\tmp')
    $expectedRemote = @('D:\tmp', 'F:\tmp\10-31')
    $actualLocal = @($LocalMediaRoots | ForEach-Object { $_.TrimEnd('\') } | Sort-Object)
    $actualRemote = @($RemoteMediaRoots | ForEach-Object { $_.TrimEnd('\') } | Sort-Object)
    if ($actualLocal.Count -ne 2 -or $actualRemote.Count -ne 2 -or
        (@(Compare-Object $expectedLocal $actualLocal -CaseSensitive).Count -ne 0) -or
        (@(Compare-Object $expectedRemote $actualRemote -CaseSensitive).Count -ne 0)) {
        throw 'RUST_V2_PHYSICAL_GUI_PATH_UNSAFE reason=media_roots'
    }
}

function Assert-RustV2PhysicalGuiDiskMappings {
    <# 确保每根媒体目录绑定指定物理盘和介质类型，防止把单盘伪装成双盘验收。 #>
    param(
        [Parameter(Mandatory)] [object[]] $Mappings,
        [Parameter(Mandatory)] [hashtable] $Expected
    )

    if ($Mappings.Count -ne $Expected.Count) {
        throw 'RUST_V2_PHYSICAL_GUI_DISK_MAPPING_INVALID reason=count'
    }
    foreach ($mapping in $Mappings) {
        $root = ([string]$mapping.Root).TrimEnd('\')
        if (-not $Expected.ContainsKey($root) -or [int]$mapping.DiskId -ne [int]$Expected[$root].DiskId -or
            -not ([string]$mapping.MediaType).Equals([string]$Expected[$root].MediaType, [StringComparison]::OrdinalIgnoreCase)) {
            throw "RUST_V2_PHYSICAL_GUI_DISK_MAPPING_INVALID root=$root"
        }
    }
}

function Assert-RustV2PhysicalGuiObserverTasks {
    <# 只接受每台一个双根 completed 任务，避免 GUI 通过按盘拆分任务绕开验收条件。 #>
    param(
        [Parameter(Mandatory)] $Observer,
        [Parameter(Mandatory)] [string] $LocalMachineId,
        [Parameter(Mandatory)] [string] $RemoteMachineId,
        [Parameter(Mandatory)] [string[]] $LocalRoots,
        [Parameter(Mandatory)] [string[]] $RemoteRoots
    )

    $nodes = @($Observer.Nodes)
    if ($nodes.Count -ne 2) { throw 'RUST_V2_PHYSICAL_GUI_TASK_INVALID reason=node_count' }
    foreach ($expectation in @(
            [pscustomobject]@{ MachineId = $LocalMachineId; Roots = $LocalRoots },
            [pscustomobject]@{ MachineId = $RemoteMachineId; Roots = $RemoteRoots }
        )) {
        $node = @($nodes | Where-Object { $_.MachineId -ceq $expectation.MachineId })
        if ($node.Count -ne 1 -or [int]$node[0].TaskCount -ne 1 -or [string]$node[0].TaskStatus -cne 'completed' -or
            (@(Compare-Object @($expectation.Roots | Sort-Object) @($node[0].Roots | Sort-Object) -CaseSensitive).Count -ne 0)) {
            throw "RUST_V2_PHYSICAL_GUI_TASK_INVALID machine=$($expectation.MachineId)"
        }
    }
}

function Invoke-RustV2PhysicalTwoHostGuiAcceptance {
    <# 执行单轮双机验收；外部操作必须显式注入，防止普通测试误连真实环境。 #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)] [string] $CandidateZip,
        [Parameter(Mandatory)] [string] $ObserverPath,
        [Parameter(Mandatory)] [string] $LocalSshAlias,
        [Parameter(Mandatory)] [string] $RemoteSshAlias,
        [Parameter(Mandatory)] [string[]] $LocalMediaRoots,
        [Parameter(Mandatory)] [string[]] $RemoteMediaRoots,
        [Parameter(Mandatory)] [string] $LocalEndpoint,
        [Parameter(Mandatory)] [string] $RemoteEndpoint,
        [Parameter(Mandatory)] [string] $CentralAddress,
        [Parameter(Mandatory)] [string] $EvidenceRoot,
        [ValidateRange(1, 7200)] [int] $MaxTaskSeconds = 7200,
        [Parameter(Mandatory)] [hashtable] $Provider
    )

    # 所有本轮对象以随机 run id 精确命名，清理逻辑只接收这些身份。
    Test-RustV2PhysicalGuiRootSet -LocalMediaRoots $LocalMediaRoots -RemoteMediaRoots $RemoteMediaRoots
    $runId = 'rust-v2-physical-two-host-' + [Guid]::NewGuid().ToString('N')
    $plan = [pscustomobject]@{
        RunId = $runId
        CandidateZip = $CandidateZip
        ObserverPath = $ObserverPath
        Local = [pscustomobject]@{ Host = 'local'; SshAlias = $LocalSshAlias; Roots = $LocalMediaRoots; Endpoint = $LocalEndpoint; RunRoot = "C:\tmp\$runId" }
        Remote = [pscustomobject]@{ Host = 'remote'; SshAlias = $RemoteSshAlias; Roots = $RemoteMediaRoots; Endpoint = $RemoteEndpoint; RunRoot = "D:\tmp\$runId" }
        CentralAddress = $CentralAddress
        EvidenceRoot = $EvidenceRoot
        ContainerName = "$runId-postgres"
        VolumeName = "$runId-postgres-data"
        FirewallRuleName = "$runId-firewall"
        MaxTaskSeconds = $MaxTaskSeconds
    }

    # 安全预检全部完成后才允许任何 Provider 写入外部状态。
    $existingObjects = @(Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation GetExistingObjects -Arguments $plan)
    if ($existingObjects.Count -gt 0) { throw "RUST_V2_PHYSICAL_GUI_OBJECT_CONFLICT objects=$($existingObjects -join ',')" }
    $package = Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation GetPackageInfo -Arguments $plan
    $localBefore = @(Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation GetMediaManifest -Arguments ([pscustomobject]@{ Host = 'local'; Roots = $LocalMediaRoots; Phase = 'before' }))
    $remoteBefore = @(Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation GetMediaManifest -Arguments ([pscustomobject]@{ Host = 'remote'; Roots = $RemoteMediaRoots; Phase = 'before' }))
    $localMappings = @(Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation GetDiskMapping -Arguments $plan.Local)
    $remoteMappings = @(Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation GetDiskMapping -Arguments $plan.Remote)
    Assert-RustV2PhysicalGuiDiskMappings -Mappings $localMappings -Expected @{ 'H:\pik\00000000000' = @{ DiskId = 1; MediaType = 'SSD' }; 'I:\tmp' = @{ DiskId = 2; MediaType = 'SSD' } }
    Assert-RustV2PhysicalGuiDiskMappings -Mappings $remoteMappings -Expected @{ 'D:\tmp' = @{ DiskId = 0; MediaType = 'HDD' }; 'F:\tmp\10-31' = @{ DiskId = 1; MediaType = 'HDD' } }

    $started = [Collections.Generic.List[object]]::new()
    try {
        foreach ($nodeHost in @($plan.Local, $plan.Remote)) {
            Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation PrepareRunRoot -Arguments $nodeHost | Out-Null
            $copySha = [string](Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation CopyCandidate -Arguments ([pscustomobject]@{ Host = $nodeHost; CandidateZip = $CandidateZip }))
            if ($copySha -cne [string]$package.Sha256) { throw "RUST_V2_PHYSICAL_GUI_PACKAGE_SHA_MISMATCH host=$($nodeHost.Host)" }
            Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation WriteConfiguration -Arguments ([pscustomobject]@{ Host = $nodeHost; Plan = $plan; Package = $package }) | Out-Null
            $started.Add((Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation StartSystemSampler -Arguments $nodeHost))
        }
        Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation AddFirewallRule -Arguments $plan | Out-Null
        Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation StartCenterContainer -Arguments $plan | Out-Null
        foreach ($nodeHost in @($plan.Local, $plan.Remote)) {
            $started.Add((Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation StartNode -Arguments $nodeHost))
            Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation WaitEndpoint -Arguments $nodeHost | Out-Null
        }
        $localStatus = Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation GetNodeStatus -Arguments $plan.Local
        $remoteStatus = Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation GetNodeStatus -Arguments $plan.Remote
        if ([string]::IsNullOrWhiteSpace([string]$localStatus.MachineId) -or $localStatus.MachineId -ceq $remoteStatus.MachineId) {
            throw 'RUST_V2_PHYSICAL_GUI_MACHINE_ID_DUPLICATE'
        }

        $desktop = Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation StartDesktop -Arguments $plan
        $started.Add($desktop)
        Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation ShowGuiChecklist -Arguments $plan | Out-Null
        $guiEvidence = Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation WaitDesktopExit -Arguments $desktop
        if (-not [bool]$guiEvidence.NormalExit) { throw 'RUST_V2_PHYSICAL_GUI_DESKTOP_EXIT_ABNORMAL' }
        $observer = Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation RunObserver -Arguments ([pscustomobject]@{ Plan = $plan; ObserverPath = $ObserverPath })
        Assert-RustV2PhysicalGuiObserverTasks -Observer $observer -LocalMachineId $localStatus.MachineId -RemoteMachineId $remoteStatus.MachineId -LocalRoots $LocalMediaRoots -RemoteRoots $RemoteMediaRoots
        $postgres = Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation ExportPostgresSummary -Arguments $plan
        $localAfter = @(Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation GetMediaManifest -Arguments ([pscustomobject]@{ Host = 'local'; Roots = $LocalMediaRoots; Phase = 'after' }))
        $remoteAfter = @(Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation GetMediaManifest -Arguments ([pscustomobject]@{ Host = 'remote'; Roots = $RemoteMediaRoots; Phase = 'after' }))
        [pscustomobject]@{
            Plan = $plan; MaxTaskSeconds = $MaxTaskSeconds; Package = $package; LocalPackageSha256 = $package.Sha256; RemotePackageSha256 = $package.Sha256
            LocalMachineId = $localStatus.MachineId; RemoteMachineId = $remoteStatus.MachineId; DiskMappings = @($localMappings + $remoteMappings)
            GuiEvidence = $guiEvidence; Observer = $observer; Postgres = $postgres
            MediaManifestUnchanged = ((@($localBefore + $remoteBefore) | ConvertTo-Json -Compress) -ceq (@($localAfter + $remoteAfter) | ConvertTo-Json -Compress))
        }
    }
    finally {
        # 只停本轮记录的精确身份；卷、运行根、截图和其他 Docker/规则永不删除。
        foreach ($item in @($started)) { if ($null -ne $item) { Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation StopRunProcess -Arguments ([pscustomobject]@{ Plan = $plan; Process = $item }) | Out-Null } }
        Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation StopCenterContainer -Arguments $plan | Out-Null
        Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation RemoveFirewallRule -Arguments $plan | Out-Null
    }
}
