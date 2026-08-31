<#
.SYNOPSIS
验证双实体机 GUI 验收编排器的隔离边界与裁决行为。
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

# 仓库脚本路径只用于加载待测编排器；所有外部边界由下方替身记录。
$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$invokeScript = Join-Path $PSScriptRoot 'Invoke-RustV2PhysicalTwoHostGuiAcceptance.ps1'
$reportScript = Join-Path $PSScriptRoot 'New-RustV2PhysicalTwoHostGuiReport.ps1'
$fixtureRoot = Join-Path ([IO.Path]::GetTempPath()) ("rust-v2-physical-two-host-gui-" + [Guid]::NewGuid().ToString('N'))

function New-FakeProvider {
    <# 创建记录实际操作名与参数的外部替身；不连接 Docker、SSH、进程或性能计数器。 #>
    param(
        [string] $RemoteRunRoot = 'D:\tmp\rust-v2-physical-two-host-gui-test',
        [string] $LocalMachineId = 'machine-local',
        [string] $RemoteMachineId = 'machine-remote',
        [string[]] $ExistingObjects = @()
    )

    $calls = [Collections.Generic.List[object]]::new()
    $record = {
        param([string] $Name, $Value)
        [void]$calls.Add([pscustomobject]@{ Name = $Name; Value = $Value })
    }.GetNewClosure()
    $provider = @{
        Calls = $calls
        GetExistingObjects = { param($value) & $record 'GetExistingObjects' $value; $ExistingObjects }
        GetPackageInfo = { param($value) & $record 'GetPackageInfo' $value; [pscustomobject]@{ Sha256 = 'zip-sha'; ManifestSha256 = 'manifest-sha'; Executables = @{ 'desktop.exe' = 'desktop-sha'; 'node.exe' = 'node-sha'; 'worker.exe' = 'worker-sha' } } }
        GetMediaManifest = { param($value) & $record 'GetMediaManifest' $value; @([pscustomobject]@{ Path = 'sample.mp4'; Length = 1024; LastWriteTimeUtc = '2026-08-31T00:00:00.0000000Z' }) }
        GetDiskMapping = { param($value) & $record 'GetDiskMapping' $value; if ($value.Host -eq 'local') { @([pscustomobject]@{ Root = 'H:\pik\00000000000'; DiskId = 1; MediaType = 'SSD' }, [pscustomobject]@{ Root = 'I:\tmp'; DiskId = 2; MediaType = 'SSD' }) } else { @([pscustomobject]@{ Root = 'D:\tmp'; DiskId = 0; MediaType = 'HDD' }, [pscustomobject]@{ Root = 'F:\tmp\10-31'; DiskId = 1; MediaType = 'HDD' }) } }
        PrepareRunRoot = { param($value) & $record 'PrepareRunRoot' $value }
        CopyCandidate = { param($value) & $record 'CopyCandidate' $value; 'zip-sha' }
        WriteConfiguration = { param($value) & $record 'WriteConfiguration' $value; [pscustomobject]@{ Sha256 = 'config-sha' } }
        StartSystemSampler = { param($value) & $record 'StartSystemSampler' $value; [pscustomobject]@{ Pid = 100 + $calls.Count; StartedUtc = '2026-08-31T00:00:00.0000000Z' } }
        AddFirewallRule = { param($value) & $record 'AddFirewallRule' $value }
        StartCenterContainer = { param($value) & $record 'StartCenterContainer' $value }
        StartNode = { param($value) & $record 'StartNode' $value; [pscustomobject]@{ Pid = 200 + $calls.Count; StartedUtc = '2026-08-31T00:00:00.0000000Z' } }
        WaitEndpoint = { param($value) & $record 'WaitEndpoint' $value }
        GetNodeStatus = { param($value) & $record 'GetNodeStatus' $value; if ($value.Host -eq 'local') { [pscustomobject]@{ MachineId = $LocalMachineId } } else { [pscustomobject]@{ MachineId = $RemoteMachineId } } }
        StartDesktop = { param($value) & $record 'StartDesktop' $value; [pscustomobject]@{ Pid = 300; StartedUtc = '2026-08-31T00:00:00.0000000Z' } }
        ShowGuiChecklist = { param($value) & $record 'ShowGuiChecklist' $value }
        WaitDesktopExit = { param($value) & $record 'WaitDesktopExit' $value; [pscustomobject]@{ NormalExit = $true; Screenshots = 8; Interactions = 10; UniqueManager = 'desktop.exe' } }
        RunObserver = { param($value) & $record 'RunObserver' $value; [pscustomobject]@{ Nodes = @([pscustomobject]@{ MachineId = $LocalMachineId; Roots = @('H:\pik\00000000000', 'I:\tmp'); TaskStatus = 'completed'; TaskCount = 1; OutboxHighwater = 10 }, [pscustomobject]@{ MachineId = $RemoteMachineId; Roots = @('D:\tmp', 'F:\tmp\10-31'); TaskStatus = 'completed'; TaskCount = 1; OutboxHighwater = 11 }); DiskSchedule = @{ Local = '6:6'; Remote = '1:1'; GrantsConserved = $true } } }
        ExportPostgresSummary = { param($value) & $record 'ExportPostgresSummary' $value; [pscustomobject]@{ SchemaValid = $true; CursorsCaughtUp = $true; CrossAnalysis = 'completed'; HasIncomplete = $false } }
        StopRunProcess = { param($value) & $record 'StopRunProcess' $value }
        StopCenterContainer = { param($value) & $record 'StopCenterContainer' $value }
        RemoveFirewallRule = { param($value) & $record 'RemoveFirewallRule' $value }
    }
    # 让替身在被编排器跨作用域调用时仍保留本次测试的调用记录。
    foreach ($key in @($provider.Keys)) {
        if ($provider[$key] -is [scriptblock]) {
            $provider[$key] = $provider[$key].GetNewClosure()
        }
    }
    $provider
}

function Assert-True {
    <# 统一抛出中文断言，避免测试失败只显示空布尔值。 #>
    param([Parameter(Mandatory)] [bool] $Condition, [Parameter(Mandatory)] [string] $Message)
    if (-not $Condition) { throw $Message }
}

try {
    Assert-True -Condition (Test-Path -LiteralPath $invokeScript -PathType Leaf) -Message "编排脚本缺失：$invokeScript"
    Assert-True -Condition (Test-Path -LiteralPath $reportScript -PathType Leaf) -Message "报告脚本缺失：$reportScript"
    . $invokeScript
    . $reportScript

    $provider = New-FakeProvider
    $result = Invoke-RustV2PhysicalTwoHostGuiAcceptance -CandidateZip 'C:\tmp\candidate.zip' -ObserverPath 'C:\tmp\physical_two_host_observer.exe' `
        -LocalSshAlias 'local-host' -RemoteSshAlias 'remote-host' -LocalMediaRoots @('H:\pik\00000000000', 'I:\tmp') `
        -RemoteMediaRoots @('D:\tmp', 'F:\tmp\10-31') -LocalEndpoint '192.168.1.17:43100' -RemoteEndpoint '192.168.1.6:43100' `
        -CentralAddress '192.168.1.17:15439' -EvidenceRoot (Join-Path $fixtureRoot 'evidence') -Provider $provider

    Assert-True -Condition ($result.MaxTaskSeconds -eq 7200) -Message '默认单机任务上限必须是7200秒'
    Assert-True -Condition ($result.LocalPackageSha256 -ceq 'zip-sha' -and $result.RemotePackageSha256 -ceq 'zip-sha') -Message '同一候选ZIP必须在两端复验同一SHA-256'
    Assert-True -Condition ($result.LocalMachineId -cne $result.RemoteMachineId) -Message '两端MachineId必须不同'
    $names = @($provider.Calls | ForEach-Object Name)
    Assert-True -Condition (($names -join ',') -match 'StartDesktop,ShowGuiChecklist,WaitDesktopExit,RunObserver') -Message '观察器只能在真实GUI退出后运行'
    Assert-True -Condition (@($names | Where-Object { $_ -eq 'StartDesktop' }).Count -eq 1) -Message 'desktop.exe必须仅启动一次并作为唯一管理连接'
    Assert-True -Condition (@($names | Where-Object { $_ -eq 'StartNode' }).Count -eq 2) -Message '两台Node必须各启动一次'
    Assert-True -Condition ($result.MediaManifestUnchanged) -Message '媒体前后清单必须保持一致'
    Assert-True -Condition (@($result.DiskMappings).Count -eq 4 -and $result.DiskMappings[0].DiskId -eq 1 -and $result.DiskMappings[3].DiskId -eq 1) -Message '四个媒体根必须映射各自主机的预期物理盘'

    $report = New-RustV2PhysicalTwoHostGuiReport -Result $result
    Assert-True -Condition ($report.Total -ceq 'PASS') -Message '完整证据必须使七个门禁和总裁决通过'
    Assert-True -Condition (@($report.Gates.PSObject.Properties).Count -eq 7) -Message '报告必须独立输出七个门禁'
    $incomplete = $result.PSObject.Copy()
    $incomplete.GuiEvidence = [pscustomobject]@{ Screenshots = 0; Interactions = 0; UniqueManager = '' }
    $incompleteReport = New-RustV2PhysicalTwoHostGuiReport -Result $incomplete
    Assert-True -Condition ($incompleteReport.Gates.GUI -ceq 'INCONCLUSIVE' -and $incompleteReport.Total -ceq 'INCONCLUSIVE') -Message 'GUI证据缺失不得提升为PASS'

    foreach ($unsafeRoot in @('I:\Tool', 'H:\pik\00000000000\runtime')) {
        $unsafeProvider = New-FakeProvider
        $rejected = $false
        try {
            Invoke-RustV2PhysicalTwoHostGuiAcceptance -CandidateZip 'C:\tmp\candidate.zip' -ObserverPath 'C:\tmp\observer.exe' `
                -LocalSshAlias 'local-host' -RemoteSshAlias 'remote-host' -LocalMediaRoots @($unsafeRoot, 'I:\tmp') `
                -RemoteMediaRoots @('D:\tmp', 'F:\tmp\10-31') -LocalEndpoint '192.168.1.17:43100' -RemoteEndpoint '192.168.1.6:43100' `
                -CentralAddress '192.168.1.17:15439' -EvidenceRoot (Join-Path $fixtureRoot 'unsafe') -Provider $unsafeProvider | Out-Null
        }
        catch { $rejected = $_.Exception.Message -match '^RUST_V2_PHYSICAL_GUI_PATH_UNSAFE' }
        Assert-True -Condition $rejected -Message "危险路径必须拒绝：$unsafeRoot"
        Assert-True -Condition ($unsafeProvider.Calls.Count -eq 0) -Message '危险路径必须在首个外部调用前拒绝'
    }

    $conflictProvider = New-FakeProvider -ExistingObjects @('unrelated-container')
    $conflictRejected = $false
    try {
        Invoke-RustV2PhysicalTwoHostGuiAcceptance -CandidateZip 'C:\tmp\candidate.zip' -ObserverPath 'C:\tmp\observer.exe' `
            -LocalSshAlias 'local-host' -RemoteSshAlias 'remote-host' -LocalMediaRoots @('H:\pik\00000000000', 'I:\tmp') `
            -RemoteMediaRoots @('D:\tmp', 'F:\tmp\10-31') -LocalEndpoint '192.168.1.17:43100' -RemoteEndpoint '192.168.1.6:43100' `
            -CentralAddress '192.168.1.17:15439' -EvidenceRoot (Join-Path $fixtureRoot 'conflict') -Provider $conflictProvider | Out-Null
    }
    catch { $conflictRejected = $_.Exception.Message -match '^RUST_V2_PHYSICAL_GUI_OBJECT_CONFLICT' }
    Assert-True -Condition $conflictRejected -Message '非本轮Docker或防火墙对象必须拒绝'
    Assert-True -Condition (@($conflictProvider.Calls | Where-Object Name -notin @('GetExistingObjects')).Count -eq 0) -Message '对象冲突后不得产生外部写入'

    $sameMachineProvider = New-FakeProvider -RemoteMachineId 'machine-local'
    $sameMachineRejected = $false
    try {
        Invoke-RustV2PhysicalTwoHostGuiAcceptance -CandidateZip 'C:\tmp\candidate.zip' -ObserverPath 'C:\tmp\observer.exe' `
            -LocalSshAlias 'local-host' -RemoteSshAlias 'remote-host' -LocalMediaRoots @('H:\pik\00000000000', 'I:\tmp') `
            -RemoteMediaRoots @('D:\tmp', 'F:\tmp\10-31') -LocalEndpoint '192.168.1.17:43100' -RemoteEndpoint '192.168.1.6:43100' `
            -CentralAddress '192.168.1.17:15439' -EvidenceRoot (Join-Path $fixtureRoot 'same-machine') -Provider $sameMachineProvider | Out-Null
    }
    catch { $sameMachineRejected = $_.Exception.Message -match '^RUST_V2_PHYSICAL_GUI_MACHINE_ID_DUPLICATE' }
    Assert-True -Condition $sameMachineRejected -Message '相同MachineId必须拒绝'
    Assert-True -Condition (-not (@($sameMachineProvider.Calls | ForEach-Object Name) -contains 'StartDesktop')) -Message '相同MachineId后不得启动GUI'

    Write-Output 'RUST_V2_PHYSICAL_TWO_HOST_GUI_ACCEPTANCE_TEST_PASS'
}
finally {
    if (Test-Path -LiteralPath $fixtureRoot) {
        Remove-Item -LiteralPath $fixtureRoot -Recurse -Force
    }
}
