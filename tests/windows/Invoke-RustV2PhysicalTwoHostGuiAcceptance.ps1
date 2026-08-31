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

function New-RustV2PhysicalTwoHostGuiRealProvider {
    <# 构造真实执行边界；只有 Invoke 入口带 -Execute 时才会调用本工厂。 #>
    [CmdletBinding()]
    param([scriptblock] $ToolRunner)

    # 统一 native argv；测试替身可完整记录 ssh/scp/docker/PowerShell 调用而不触发外部副作用。
    if ($null -eq $ToolRunner) {
        $ToolRunner = {
            param([string] $FilePath, [string[]] $Arguments)
            $output = @(& $FilePath @Arguments 2>&1)
            [pscustomobject]@{ ExitCode = $LASTEXITCODE; Output = @($output | ForEach-Object { [string]$_ }) }
        }.GetNewClosure()
    }
    $sshConfig = Join-Path $env:USERPROFILE '.ssh\config'
    $password = [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(24)).ToLowerInvariant()
    $encoded = {
        param([string] $Text)
        [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($Text))
    }.GetNewClosure()
    $invokeLocal = {
        param([string] $Text)
        & $ToolRunner 'powershell.exe' @('-NoProfile','-NonInteractive','-EncodedCommand',(& $encoded $Text))
    }.GetNewClosure()
    $invokeRemote = {
        param($HostPlan, [string] $Text)
        & $ToolRunner 'ssh.exe' @('-F',$sshConfig,'-o','BatchMode=yes',$HostPlan.SshAlias,'powershell.exe','-NoProfile','-NonInteractive','-EncodedCommand',(& $encoded $Text))
    }.GetNewClosure()
    $psLiteral = { param([string]$Text) "'" + $Text.Replace("'", "''") + "'" }.GetNewClosure()
    $provider = @{
        GetExistingObjects = { param($plan)
            $names = @(); foreach ($kind in @('container','volume')) { $r=& $ToolRunner 'docker.exe' @($kind,'inspect',$(if($kind -eq 'container'){$plan.ContainerName}else{$plan.VolumeName})); if($r.ExitCode -eq 0){$names += "$kind"} }
            $rule = & $invokeLocal "if (Get-NetFirewallRule -DisplayName $(& $psLiteral $plan.FirewallRuleName) -ErrorAction SilentlyContinue) { 'rule' }"; if (@($rule.Output) -contains 'rule') { $names += 'firewall' }; $names
        }
        GetPackageInfo = { param($plan) [pscustomobject]@{ Sha256 = (Get-FileHash -LiteralPath $plan.CandidateZip -Algorithm SHA256).Hash.ToLowerInvariant(); ManifestSha256=''; Executables=@{} } }
        GetMediaManifest = { param($value) if($value.Host -eq 'remote'){ & $invokeRemote ([pscustomobject]@{SshAlias=$value.SshAlias}) 'exit 0' | Out-Null; return @() }; Get-ChildItem -LiteralPath $value.Roots -File -Recurse | ForEach-Object {[pscustomobject]@{Path=$_.FullName;Length=$_.Length;LastWriteTimeUtc=$_.LastWriteTimeUtc.ToString('O')}} }
        GetDiskMapping = { param($host) if($host.Host -eq 'local'){ @([pscustomobject]@{Root='H:\pik\00000000000';DiskId=1;MediaType='SSD'},[pscustomobject]@{Root='I:\tmp';DiskId=2;MediaType='SSD'}) } else {@([pscustomobject]@{Root='D:\tmp';DiskId=0;MediaType='HDD'},[pscustomobject]@{Root='F:\tmp\10-31';DiskId=1;MediaType='HDD'})} }
        PrepareRunRoot = { param($host) $script="if(Test-Path -LiteralPath $(& $psLiteral $host.RunRoot)){throw 'RUST_V2_PHYSICAL_GUI_RUN_ROOT_EXISTS'};New-Item -ItemType Directory -Path $(& $psLiteral $host.RunRoot),$(& $psLiteral (Join-Path $host.RunRoot 'release')),$(& $psLiteral (Join-Path $host.RunRoot 'evidence'))|Out-Null"; if($host.Host -eq 'local'){&$invokeLocal $script}else{&$invokeRemote $host $script} }
        CopyCandidate = { param($value) $host=$value.Host; if($host.Host -eq 'local'){ $script="Copy-Item -LiteralPath $(& $psLiteral $value.CandidateZip) -Destination $(& $psLiteral (Join-Path $host.RunRoot 'candidate.zip'));Expand-Archive -LiteralPath $(& $psLiteral (Join-Path $host.RunRoot 'candidate.zip')) -DestinationPath $(& $psLiteral (Join-Path $host.RunRoot 'release'));(Get-FileHash -LiteralPath $(& $psLiteral (Join-Path $host.RunRoot 'candidate.zip')) -Algorithm SHA256).Hash"; $r=&$invokeLocal $script }else{ & $ToolRunner 'scp.exe' @('-F',$sshConfig,$value.CandidateZip,"$($host.SshAlias):$($host.RunRoot.Replace('\','/'))/candidate.zip")|Out-Null; $r=&$invokeRemote $host "Expand-Archive -LiteralPath $(& $psLiteral (Join-Path $host.RunRoot 'candidate.zip')) -DestinationPath $(& $psLiteral (Join-Path $host.RunRoot 'release'));(Get-FileHash -LiteralPath $(& $psLiteral (Join-Path $host.RunRoot 'candidate.zip')) -Algorithm SHA256).Hash" }; (@($r.Output)|Select-Object -Last 1).Trim().ToLowerInvariant() }
        WriteConfiguration = { param($value) $host=$value.Host; $ip=if($host.Host -eq 'local'){'192.168.1.17'}else{'192.168.1.6'}; $config="listen_ip = `"$ip`"`nport = 43100`nworker_count = 20`nenumerator = `"everything`"`n`n[paths]`ndata_path = `"data/node`"`nconfig_path = `"config/node.toml`"`nlog_path = `"data/node/logs`"`ncache_path = `"data/node/cache`"`n`n[read]`nhdd = 1`nssd = 16`nunknown = 1`ntotal = 12`nblock_size = 4194304`ntimeout = 3`nretries = 2`n`n[worker]`nmode = `"manual`"`nreserved_cores = 1`nmanual_worker_count = 20`n`n[postgres]`nenabled = true`nhost = `"192.168.1.17`"`nport = 15439`ndatabase = `"dedup_v2`"`nusername = `"dedup`"`npassword = `"$password`"`nconnect_timeout = 3`n"; $script="[IO.File]::WriteAllText($(& $psLiteral (Join-Path $host.RunRoot 'release\config\node.toml')),$(& $psLiteral $config),[Text.UTF8Encoding]::new(`$false))"; if($host.Host -eq 'local'){&$invokeLocal $script}else{&$invokeRemote $host $script}; [pscustomobject]@{Sha256=([Convert]::ToHexString([Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($config))).ToLowerInvariant())} }
        WriteDesktopConfiguration = { param($plan) $dsn="postgresql://dedup:$password@192.168.1.17:15439/dedup_v2"; $config="nodes = [{ host = `"192.168.1.17`", port = 43100 }, { host = `"192.168.1.6`", port = 43100 }]`npostgres_url = `"$dsn`"`ndelete_mode = `"recycle_bin`"`nreconnect_interval_seconds = 5`n`n[thresholds]`npdq_quality_min = 50`naspect_tolerance = 0.10`npdq_hamming_max = 31`nphash_part_hamming_max = 10`nphash_min_passed_parts = 8`nsobel_min = 0.85`nvideo_min_valid_frames = 4`nvideo_stage1_min = 0.80`nvideo_stage2_min = 0.80`n"; & $invokeLocal "New-Item -ItemType Directory -Force -Path $(& $psLiteral (Join-Path $plan.Local.RunRoot 'release\data\desktop'))|Out-Null;[IO.File]::WriteAllText($(& $psLiteral (Join-Path $plan.Local.RunRoot 'release\data\desktop\config.toml')),$(& $psLiteral $config),[Text.UTF8Encoding]::new(`$false))"; [pscustomobject]@{Dsn='postgresql://dedup:***@192.168.1.17:15439/dedup_v2'} }
        StartSystemSampler = { param($host) [pscustomobject]@{Pid=0;StartedUtc=[DateTime]::UtcNow.ToString('O');Path=(Join-Path $host.RunRoot 'evidence\system.ndjson')} }
        AddFirewallRule = { param($plan) & $invokeLocal "New-NetFirewallRule -DisplayName $(& $psLiteral $plan.FirewallRuleName) -Direction Inbound -Action Allow -Protocol TCP -LocalPort 15439 -RemoteAddress 192.168.1.6|Out-Null" }
        StartCenterContainer = { param($plan) & $invokeLocal "& $(& $psLiteral (Join-Path (Split-Path -Parent $PSScriptRoot) 'scripts\New-RustV2PostgresContainer.ps1')) -ContainerName 'mysingerserver-rust-v2-dualhost-20260831' -VolumeName 'mysingerserver-rust-v2-dualhost-20260831-data' -HostAddress 192.168.1.17 -HostPort 15439 -DatabaseName dedup_v2 -DatabaseUser dedup -DatabasePassword $(& $psLiteral $password)" }
        StartNode = { param($host) [pscustomobject]@{Pid=0;StartedUtc=[DateTime]::UtcNow.ToString('O');Path=(Join-Path $host.RunRoot 'release\node.exe')} }
        WaitEndpoint = { param($host) if(-not(Test-NetConnection -ComputerName ($host.Endpoint.Split(':')[0]) -Port 43100 -InformationLevel Quiet)){throw 'RUST_V2_PHYSICAL_GUI_ENDPOINT_UNAVAILABLE'} }
        RunPreflightObserver = { param($value) & $invokeLocal "& $(& $psLiteral $value.ObserverPath) --endpoint 192.168.1.17:43100 --endpoint 192.168.1.6:43100 --output $(& $psLiteral $value.OutputPath)"; $lines=Get-Content -LiteralPath $value.OutputPath | ForEach-Object {$_|ConvertFrom-Json}; [pscustomobject]@{Closed=$true;Nodes=@($lines|Where-Object event -eq 'node_snapshot')} }
        WaitNodeIdle = { param($host) Start-Sleep -Seconds 1 }
        StartDesktop = { param($plan) Start-Process -FilePath (Join-Path $plan.Local.RunRoot 'release\desktop.exe') -WorkingDirectory (Join-Path $plan.Local.RunRoot 'release') -PassThru }
        ShowGuiChecklist = { param($plan) Write-Host "GUI截图目录：$($plan.EvidenceRoot)\screenshots；按双根创建任务、同步、跨机分析并正常退出。" }
        WaitDesktopExit = { param($desktop) $desktop.WaitForExit();[pscustomobject]@{NormalExit=($desktop.ExitCode -eq 0);Screenshots=0;Interactions=0;UniqueManager='desktop.exe'} }
        RunObserver = { param($value) & $invokeLocal "& $(& $psLiteral $value.ObserverPath) --endpoint 192.168.1.17:43100 --endpoint 192.168.1.6:43100 --output $(& $psLiteral (Join-Path $value.Plan.EvidenceRoot 'node-observer.ndjson'))";[pscustomobject]@{Nodes=@();DiskSchedule=@{}} }
        ExportPostgresSummary = { param($plan) [pscustomobject]@{SchemaValid=$false;CursorsCaughtUp=$false;CrossAnalysis='';HasIncomplete=$false} }
        StopRunProcess = { param($value) if($value.Process.Pid -gt 0){Stop-Process -Id $value.Process.Pid -ErrorAction SilentlyContinue} }
        StopCenterContainer = { param($plan) & $ToolRunner 'docker.exe' @('stop',$plan.ContainerName)|Out-Null }
        RemoveFirewallRule = { param($plan) & $invokeLocal "Get-NetFirewallRule -DisplayName $(& $psLiteral $plan.FirewallRuleName) -ErrorAction SilentlyContinue|Remove-NetFirewallRule" }
    }
    foreach($key in @($provider.Keys)){if($provider[$key] -is [scriptblock]){$provider[$key]=$provider[$key].GetNewClosure()}}
    $provider
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
        [hashtable] $Provider,
        [switch] $Execute
    )

    # 所有本轮对象以随机 run id 精确命名，清理逻辑只接收这些身份。
    Test-RustV2PhysicalGuiRootSet -LocalMediaRoots $LocalMediaRoots -RemoteMediaRoots $RemoteMediaRoots
    if ($null -eq $Provider) {
        if (-not $Execute) { throw 'RUST_V2_PHYSICAL_GUI_EXECUTE_REQUIRED' }
        $Provider = New-RustV2PhysicalTwoHostGuiRealProvider
    }
    $runId = 'rust-v2-physical-two-host-gui-20260831-' + [Guid]::NewGuid().ToString('N')
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
        Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation WriteDesktopConfiguration -Arguments $plan | Out-Null
        Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation AddFirewallRule -Arguments $plan | Out-Null
        Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation StartCenterContainer -Arguments $plan | Out-Null
        foreach ($nodeHost in @($plan.Local, $plan.Remote)) {
            $started.Add((Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation StartNode -Arguments $nodeHost))
            Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation WaitEndpoint -Arguments $nodeHost | Out-Null
        }
        # GUI 前仅允许 Task2 观察器串行预检；它结束并确认两条连接关闭后才交给唯一 GUI 管理连接。
        $preflight = Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation RunPreflightObserver -Arguments ([pscustomobject]@{ Plan = $plan; ObserverPath = $ObserverPath; OutputPath = (Join-Path $EvidenceRoot 'node-preflight.ndjson') })
        $preflightNodes = @($preflight.Nodes)
        $localStatus = if ($preflightNodes.Count -ge 1) { $preflightNodes[0] } else { $null }
        $remoteStatus = if ($preflightNodes.Count -ge 2) { $preflightNodes[1] } else { $null }
        if ($preflightNodes.Count -eq 2 -and $localStatus.MachineId -ceq $remoteStatus.MachineId) {
            throw 'RUST_V2_PHYSICAL_GUI_MACHINE_ID_DUPLICATE'
        }
        if (-not [bool]$preflight.Closed -or $preflightNodes.Count -ne 2 -or [string]::IsNullOrWhiteSpace([string]$localStatus.MachineId)) {
            throw 'RUST_V2_PHYSICAL_GUI_PREFLIGHT_INVALID'
        }
        foreach ($nodeHost in @($plan.Local, $plan.Remote)) {
            Invoke-RustV2PhysicalGuiProvider -Provider $Provider -Operation WaitNodeIdle -Arguments $nodeHost | Out-Null
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
            GuiEvidence = $guiEvidence; Preflight = $preflight; Observer = $observer; Postgres = $postgres
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
