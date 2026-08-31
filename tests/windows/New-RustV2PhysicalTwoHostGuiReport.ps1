<#
.SYNOPSIS
依据已收集的双实体机证据输出七个独立门禁与总裁决。
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

function Get-RustV2PhysicalGuiGate {
    <# 依据已知事实计算单个门禁；缺失值始终返回 INCONCLUSIVE 而非猜测 PASS。 #>
    param([bool] $Passed, [bool] $Failed, [bool] $Complete)
    if ($Failed) { return 'FAIL' }
    if ($Complete -and $Passed) { return 'PASS' }
    'INCONCLUSIVE'
}

function Get-RustV2PhysicalGuiEvidenceValue {
    <# 安全读取可选证据字段，兼容旧观察器未暴露的指标而不把缺失当成零值。 #>
    param([AllowNull()] $Value, [Parameter(Mandatory)] [string] $Name)
    if ($Value -is [Collections.IDictionary] -and $Value.Contains($Name)) { return $Value[$Name] }
    if ($null -ne $Value -and $null -ne $Value.PSObject.Properties[$Name]) { return $Value.$Name }
    $null
}

function New-RustV2PhysicalTwoHostGuiReport {
    <# 生成可序列化裁决对象；调用方可选择将它写入脱敏报告文件。 #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)] $Result,
        [string] $ReportPath = ''
    )

    $observer = Get-RustV2PhysicalGuiEvidenceValue -Value $Result -Name 'Observer'
    $postgres = Get-RustV2PhysicalGuiEvidenceValue -Value $Result -Name 'Postgres'
    $gui = Get-RustV2PhysicalGuiEvidenceValue -Value $Result -Name 'GuiEvidence'
    $localMachineId = Get-RustV2PhysicalGuiEvidenceValue -Value $Result -Name 'LocalMachineId'
    $remoteMachineId = Get-RustV2PhysicalGuiEvidenceValue -Value $Result -Name 'RemoteMachineId'
    $schemaValid = Get-RustV2PhysicalGuiEvidenceValue -Value $postgres -Name 'SchemaValid'
    $cursorsCaughtUp = Get-RustV2PhysicalGuiEvidenceValue -Value $postgres -Name 'CursorsCaughtUp'
    $crossAnalysis = Get-RustV2PhysicalGuiEvidenceValue -Value $postgres -Name 'CrossAnalysis'
    $hasIncomplete = Get-RustV2PhysicalGuiEvidenceValue -Value $postgres -Name 'HasIncomplete'
    $diskSchedule = Get-RustV2PhysicalGuiEvidenceValue -Value $observer -Name 'DiskSchedule'
    $localSchedule = Get-RustV2PhysicalGuiEvidenceValue -Value $diskSchedule -Name 'Local'
    $remoteSchedule = Get-RustV2PhysicalGuiEvidenceValue -Value $diskSchedule -Name 'Remote'
    $grantsConserved = Get-RustV2PhysicalGuiEvidenceValue -Value $diskSchedule -Name 'GrantsConserved'
    $mediaManifestUnchanged = Get-RustV2PhysicalGuiEvidenceValue -Value $Result -Name 'MediaManifestUnchanged'
    $guiScreenshots = Get-RustV2PhysicalGuiEvidenceValue -Value $gui -Name 'Screenshots'
    $guiInteractions = Get-RustV2PhysicalGuiEvidenceValue -Value $gui -Name 'Interactions'
    $guiManager = Get-RustV2PhysicalGuiEvidenceValue -Value $gui -Name 'UniqueManager'
    $guiNormalExit = Get-RustV2PhysicalGuiEvidenceValue -Value $gui -Name 'NormalExit'
    $nodeCount = if ($null -eq $observer) { 0 } else { @($observer.Nodes).Count }
    $tasksComplete = $nodeCount -eq 2 -and @($observer.Nodes | Where-Object { $_.TaskStatus -cne 'completed' -or $_.TaskCount -ne 1 }).Count -eq 0
    $infra = Get-RustV2PhysicalGuiGate -Passed ($localMachineId -cne $remoteMachineId -and [bool]$schemaValid) -Failed ($null -ne $localMachineId -and $localMachineId -ceq $remoteMachineId) -Complete ($null -ne $postgres -and $nodeCount -eq 2)
    $runtime = Get-RustV2PhysicalGuiGate -Passed $tasksComplete -Failed ($nodeCount -gt 0 -and -not $tasksComplete) -Complete ($nodeCount -eq 2)
    $disk = Get-RustV2PhysicalGuiGate -Passed ($localSchedule -ceq '6:6' -and $remoteSchedule -ceq '1:1' -and [bool]$grantsConserved) -Failed ($null -ne $diskSchedule -and -not [bool]$grantsConserved) -Complete ($null -ne $diskSchedule)
    $sync = Get-RustV2PhysicalGuiGate -Passed ([bool]$cursorsCaughtUp) -Failed ($null -ne $postgres -and $cursorsCaughtUp -eq $false) -Complete ($null -ne $postgres)
    $cross = Get-RustV2PhysicalGuiGate -Passed ($crossAnalysis -ceq 'completed' -and -not [bool]$hasIncomplete) -Failed ($null -ne $postgres -and ($crossAnalysis -ceq 'failed' -or [bool]$hasIncomplete)) -Complete ($null -ne $postgres -and -not [string]::IsNullOrWhiteSpace([string]$crossAnalysis))
    $guiGate = Get-RustV2PhysicalGuiGate -Passed ($guiScreenshots -ge 8 -and $guiInteractions -ge 10 -and $guiManager -ceq 'desktop.exe') -Failed ($null -ne $gui -and ($guiNormalExit -eq $false -or ($guiScreenshots -gt 0 -and $guiManager -cne 'desktop.exe'))) -Complete ($null -ne $gui -and $guiScreenshots -ge 8 -and $guiInteractions -ge 10)
    $media = Get-RustV2PhysicalGuiGate -Passed ([bool]$mediaManifestUnchanged) -Failed ($mediaManifestUnchanged -eq $false) -Complete ($null -ne $mediaManifestUnchanged)
    $gates = [pscustomobject]@{ Infra = $infra; Runtime = $runtime; DiskSchedule = $disk; Sync = $sync; CrossAnalysis = $cross; GUI = $guiGate; MediaIntegrity = $media }
    $values = @($gates.PSObject.Properties | ForEach-Object Value)
    $total = if ($values -contains 'FAIL') { 'FAIL' } elseif ($values -notcontains 'PASS' -or $values -contains 'INCONCLUSIVE') { 'INCONCLUSIVE' } else { 'PASS' }
    $plan = Get-RustV2PhysicalGuiEvidenceValue -Value $Result -Name 'Plan'
    $runId = Get-RustV2PhysicalGuiEvidenceValue -Value $plan -Name 'RunId'
    $report = [pscustomobject]@{ Gates = $gates; Total = $total; RunId = $runId }
    if (-not [string]::IsNullOrWhiteSpace($ReportPath)) {
        # 报告只序列化裁决，避免把候选配置或内存密码写入 Git/标准输出。
        [IO.File]::WriteAllText($ReportPath, ($report | ConvertTo-Json -Depth 6), [Text.UTF8Encoding]::new($false))
    }
    $report
}
