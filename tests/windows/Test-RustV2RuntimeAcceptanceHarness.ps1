<#
.SYNOPSIS
验证半小时真实媒体 harness 的输入保护、隔离路径和媒体清单行为。
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$measureScript = Join-Path $repositoryRoot 'tests\windows\Measure-RustV2RuntimeAcceptance.ps1'
$fixtureRoot = Join-Path ([IO.Path]::GetTempPath()) ("rust-v2-runtime-harness-" + [Guid]::NewGuid().ToString('N'))

try {
    if (-not (Test-Path -LiteralPath $measureScript -PathType Leaf)) {
        throw "RUST_V2_RUNTIME_ACCEPTANCE_HARNESS_MISSING path=$measureScript"
    }
    . $measureScript -LibraryOnly

    if (-not [bool]$CompleteWhenTaskTerminal) {
        throw 'RUST_V2_RUNTIME_ACCEPTANCE_TERMINAL_COMPLETION_MUST_DEFAULT_ON'
    }

    # 单次双盘验收是唯一真实入口；旧六轮 A/B 编排及其专用聚合链不再可执行。
    $obsoleteAcceptanceScripts = @(
        (Join-Path $repositoryRoot 'tests\windows\Measure-RustV2CpuIoAb.ps1'),
        (Join-Path $repositoryRoot 'tests\windows\New-RustV2CpuIoAbReport.ps1'),
        (Join-Path $repositoryRoot 'tests\windows\Test-RustV2CpuIoAbReport.ps1'),
        (Join-Path $repositoryRoot 'scripts\build-rust-v2-cpu-io-test-package.ps1')
    )
    $obsoletePresent = @($obsoleteAcceptanceScripts | Where-Object { Test-Path -LiteralPath $_ -PathType Leaf })
    if ($obsoletePresent.Count -gt 0) {
        throw "RUST_V2_RUNTIME_ACCEPTANCE_OBSOLETE_AB_ENTRY_PRESENT: $($obsoletePresent -join ', ')"
    }

    $defaultConfig = New-IsolatedNodeConfig -Port 39124
    if ($defaultConfig -notmatch 'worker_count = 20' -or
        $defaultConfig -notmatch 'manual_worker_count = 20' -or
        $defaultConfig -notmatch 'total_threads = 12') {
        throw "RUST_V2_RUNTIME_ACCEPTANCE_DEFAULTS_INVALID: $defaultConfig"
    }

    $wrongApprovedRootCode = ''
    try {
        Assert-RuntimeAcceptanceMediaRoots -MediaRoots @('H:\pik\other', 'I:\tmp')
    }
    catch {
        $wrongApprovedRootCode = $_.Exception.Message
    }
    if ($wrongApprovedRootCode -ne 'RUST_V2_REAL_MEDIA_ROOTS_NOT_APPROVED') {
        throw "媒体根必须精确绑定到 H:\pik\00000000000 与 I:\tmp，实际=$wrongApprovedRootCode"
    }

    $media = Join-Path $fixtureRoot 'media'
    $release = Join-Path $fixtureRoot 'release'
    $tools = Join-Path $fixtureRoot 'tools'
    $evidence = Join-Path $fixtureRoot 'runs\A-1\evidence'
    $report = Join-Path $fixtureRoot 'runs\A-1\evidence\report.md'
    New-Item -ItemType Directory -Path $media, $release, $tools -Force | Out-Null
    foreach ($name in @('desktop.exe', 'node.exe', 'worker.exe', 'Everything.exe')) {
        [IO.File]::WriteAllText((Join-Path $release $name), "fixture $name")
    }
    # 源 release 故意包含测试工具和额外 EXE，验证 formal copy 不是空 fixture 边界。
    [IO.File]::WriteAllText((Join-Path $release 'runtime_acceptance.exe'), 'source test tool')
    [IO.File]::WriteAllText((Join-Path $release 'export_scan_result_summary.exe'), 'source test exporter')
    [IO.File]::WriteAllText((Join-Path $release 'unexpected-test.exe'), 'source extra exe')
    $acceptanceClient = Join-Path $tools 'runtime_acceptance.exe'
    $resultExporter = Join-Path $tools 'export_scan_result_summary.exe'
    [IO.File]::WriteAllText($acceptanceClient, 'fixture acceptance client')
    [IO.File]::WriteAllText($resultExporter, 'fixture result exporter')
    $runtime = Join-Path $release 'runtime\ffmpeg'
    New-Item -ItemType Directory -Path $runtime -Force | Out-Null
    foreach ($name in @('avutil-60.dll', 'swresample-6.dll', 'swscale-9.dll', 'avcodec-62.dll', 'avformat-62.dll')) {
        [IO.File]::WriteAllText((Join-Path $runtime $name), "fixture $name")
    }
    1..3 | ForEach-Object {
        [IO.File]::WriteAllText((Join-Path $media "fixture-$_.bin"), "media $_")
    }

    $missing = Assert-RuntimeAcceptanceInputs `
        -MediaRoot '' -DurationSeconds 1800 -SampleSeconds 2 -ReleaseRoot $release `
        -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
        -EvidenceRoot $evidence -ReportPath $report -Variant A -RunIndex 1 `
        -ThrowOnError:$false
    if ($missing.Valid -or $missing.Code -ne 'RUST_V2_REAL_MEDIA_ROOT_MISSING') {
        throw "缺媒体根必须在启动前拒绝，实际=$($missing | ConvertTo-Json -Compress)"
    }

    Remove-Item -LiteralPath (Join-Path $release 'desktop.exe') -Force
    $missingFormal = Assert-RuntimeAcceptanceInputs `
        -MediaRoot $media -DurationSeconds 1800 -SampleSeconds 2 -ReleaseRoot $release `
        -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
        -EvidenceRoot $evidence -ReportPath $report -Variant A -RunIndex 1 `
        -ThrowOnError:$false
    if ($missingFormal.Valid -or $missingFormal.Code -ne 'RUST_V2_ACCEPTANCE_BINARY_MISSING:desktop.exe') {
        throw "正式 desktop 缺失必须拒绝，实际=$($missingFormal | ConvertTo-Json -Compress)"
    }
    [IO.File]::WriteAllText((Join-Path $release 'desktop.exe'), 'fixture desktop.exe')

    Remove-Item -LiteralPath (Join-Path $release 'runtime\ffmpeg\avformat-62.dll') -Force
    $missingFfmpeg = Assert-RuntimeAcceptanceInputs `
        -MediaRoot $media -DurationSeconds 1800 -SampleSeconds 2 -ReleaseRoot $release `
        -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
        -EvidenceRoot $evidence -ReportPath $report -Variant A -RunIndex 1 `
        -ThrowOnError:$false
    if ($missingFfmpeg.Valid -or $missingFfmpeg.Code -ne 'RUST_V2_ACCEPTANCE_FFMPEG_MISSING:avformat-62.dll') {
        throw "正式 FFmpeg DLL 缺失必须拒绝，实际=$($missingFfmpeg | ConvertTo-Json -Compress)"
    }
    [IO.File]::WriteAllText((Join-Path $release 'runtime\ffmpeg\avformat-62.dll'), 'fixture avformat-62.dll')

    $short = Assert-RuntimeAcceptanceInputs `
        -MediaRoot $media -DurationSeconds 1799 -SampleSeconds 2 -ReleaseRoot $release `
        -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
        -EvidenceRoot $evidence -ReportPath $report -Variant A -RunIndex 1 `
        -ThrowOnError:$false
    if ($short.Valid -or $short.Code -ne 'RUST_V2_ACCEPTANCE_DURATION_INVALID') {
        throw '少于1800秒必须拒绝'
    }

    $wrongTick = Assert-RuntimeAcceptanceInputs `
        -MediaRoot $media -DurationSeconds 1800 -SampleSeconds 3 -ReleaseRoot $release `
        -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
        -EvidenceRoot $evidence -ReportPath $report -Variant A -RunIndex 1 `
        -ThrowOnError:$false
    if ($wrongTick.Valid -or $wrongTick.Code -ne 'RUST_V2_ACCEPTANCE_SAMPLE_INVALID') {
        throw '采样间隔必须固定2秒'
    }

    # RED：真实验收必须显式选择 Everything；非法枚举器不得悄悄回退为 Walker。
    $badEnumerator = Assert-RuntimeAcceptanceInputs `
        -MediaRoot $media -DurationSeconds 1800 -SampleSeconds 2 -ReleaseRoot $release `
        -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
        -EvidenceRoot $evidence -ReportPath $report -Enumerator 'unknown' `
        -ThrowOnError:$false
    if ($badEnumerator.Valid -or $badEnumerator.Code -ne 'RUST_V2_ACCEPTANCE_ENUMERATOR_INVALID') {
        throw "非法枚举器必须在启动前拒绝，实际=$($badEnumerator | ConvertTo-Json -Compress)"
    }
    $mediaSecond = Join-Path $fixtureRoot 'media-second'
    New-Item -ItemType Directory -Path $mediaSecond -Force | Out-Null
    $terminalValidation = Assert-RuntimeAcceptanceInputs `
        -MediaRoot @($media, $mediaSecond) -DurationSeconds 1800 -SampleSeconds 2 `
        -ReleaseRoot $release -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
        -EvidenceRoot (Join-Path $fixtureRoot 'runs\terminal-A-1\evidence') `
        -ReportPath (Join-Path $fixtureRoot 'runs\terminal-A-1\evidence\report.md') `
        -Enumerator 'everything' -CompleteWhenTaskTerminal -RequireDistinctPhysicalDisks:$false `
        -ThrowOnError:$false
    if (-not $terminalValidation.Valid) {
        throw "Everything + CompleteWhenTaskTerminal + 多媒体根应通过校验，实际=$($terminalValidation | ConvertTo-Json -Compress)"
    }

    $missingClient = Assert-RuntimeAcceptanceInputs `
        -MediaRoot $media -DurationSeconds 1800 -SampleSeconds 2 -ReleaseRoot $release `
        -AcceptanceClientPath (Join-Path $tools 'missing-client.exe') -ResultExporterPath $resultExporter `
        -EvidenceRoot $evidence -ReportPath $report -Variant A -RunIndex 1 `
        -ThrowOnError:$false
    if ($missingClient.Valid -or $missingClient.Code -ne 'RUST_V2_ACCEPTANCE_CLIENT_MISSING') {
        throw "外置 acceptance client 缺失必须拒绝，实际=$($missingClient | ConvertTo-Json -Compress)"
    }

    $missingExporter = Assert-RuntimeAcceptanceInputs `
        -MediaRoot $media -DurationSeconds 1800 -SampleSeconds 2 -ReleaseRoot $release `
        -AcceptanceClientPath $acceptanceClient -ResultExporterPath (Join-Path $tools 'missing-exporter.exe') `
        -EvidenceRoot $evidence -ReportPath $report -Variant A -RunIndex 1 `
        -ThrowOnError:$false
    if ($missingExporter.Valid -or $missingExporter.Code -ne 'RUST_V2_ACCEPTANCE_EXPORTER_MISSING') {
        throw "外置 exporter 缺失必须拒绝，实际=$($missingExporter | ConvertTo-Json -Compress)"
    }

    $relativeTool = Assert-RuntimeAcceptanceInputs `
        -MediaRoot $media -DurationSeconds 1800 -SampleSeconds 2 -ReleaseRoot $release `
        -AcceptanceClientPath 'tools\runtime_acceptance.exe' -ResultExporterPath $resultExporter `
        -EvidenceRoot $evidence -ReportPath $report -Variant A -RunIndex 1 `
        -ThrowOnError:$false
    if ($relativeTool.Valid -or $relativeTool.Code -ne 'RUST_V2_ACCEPTANCE_TOOLS_PATH_INVALID') {
        throw "外置工具必须使用绝对路径，实际=$($relativeTool | ConvertTo-Json -Compress)"
    }

    $toolInsideRelease = Assert-RuntimeAcceptanceInputs `
        -MediaRoot $media -DurationSeconds 1800 -SampleSeconds 2 -ReleaseRoot $release `
        -AcceptanceClientPath (Join-Path $release 'runtime_acceptance.exe') -ResultExporterPath $resultExporter `
        -EvidenceRoot $evidence -ReportPath $report -Variant A -RunIndex 1 `
        -ThrowOnError:$false
    if ($toolInsideRelease.Valid -or $toolInsideRelease.Code -ne 'RUST_V2_ACCEPTANCE_TOOL_INSIDE_RELEASE') {
        throw "正式 release 根内的外置工具必须拒绝，实际=$($toolInsideRelease | ConvertTo-Json -Compress)"
    }

    $badVariant = Assert-RuntimeAcceptanceInputs `
        -MediaRoot $media -DurationSeconds 1800 -SampleSeconds 2 -ReleaseRoot $release `
        -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
        -EvidenceRoot $evidence -ReportPath $report -Variant C -RunIndex 1 `
        -ThrowOnError:$false
    if ($badVariant.Valid -or $badVariant.Code -ne 'RUST_V2_ACCEPTANCE_VARIANT_INVALID') {
        throw "variant 只能为 A/B，实际=$($badVariant | ConvertTo-Json -Compress)"
    }

    $badRun = Assert-RuntimeAcceptanceInputs `
        -MediaRoot $media -DurationSeconds 1800 -SampleSeconds 2 -ReleaseRoot $release `
        -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
        -EvidenceRoot $evidence -ReportPath $report -Variant A -RunIndex 4 `
        -ThrowOnError:$false
    if ($badRun.Valid -or $badRun.Code -ne 'RUST_V2_ACCEPTANCE_RUN_INDEX_INVALID') {
        throw "run index 只能为1..3，实际=$($badRun | ConvertTo-Json -Compress)"
    }

    $existingRunEvidence = Join-Path $fixtureRoot 'runs\B-1\evidence'
    New-Item -ItemType Directory -Path $existingRunEvidence -Force | Out-Null
    [IO.File]::WriteAllText((Join-Path $existingRunEvidence 'harness-result.json'), '{}')
    $reuse = Assert-RuntimeAcceptanceInputs `
        -MediaRoot $media -DurationSeconds 1800 -SampleSeconds 2 -ReleaseRoot $release `
        -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
        -EvidenceRoot $existingRunEvidence -ReportPath (Join-Path $existingRunEvidence 'report.md') `
        -Variant B -RunIndex 1 -ThrowOnError:$false
    if ($reuse.Valid -or $reuse.Code -ne 'RUST_V2_ACCEPTANCE_EVIDENCE_EXISTS') {
        throw "复用既有 run evidence 必须拒绝，实际=$($reuse | ConvertTo-Json -Compress)"
    }

    $emptyRunEvidence = Join-Path $fixtureRoot 'runsC-1evidence'
    New-Item -ItemType Directory -Path $emptyRunEvidence -Force | Out-Null
    $emptyReuse = Assert-RuntimeAcceptanceInputs `
        -MediaRoot $media -DurationSeconds 1800 -SampleSeconds 2 -ReleaseRoot $release `
        -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
        -EvidenceRoot $emptyRunEvidence -ReportPath (Join-Path $emptyRunEvidence 'report.md') `
        -Variant A -RunIndex 1 -ThrowOnError:$false
    if ($emptyReuse.Valid -or $emptyReuse.Code -ne 'RUST_V2_ACCEPTANCE_EVIDENCE_EXISTS') {
        throw "即使为空的既有 run evidence 也必须拒绝复用，实际=$($emptyReuse | ConvertTo-Json -Compress)"
    }

    $copyRoot = Join-Path $fixtureRoot 'copied-release'
    New-Item -ItemType Directory -Path $copyRoot -Force | Out-Null
    Copy-RuntimeAcceptanceRelease -Source $release -Destination $copyRoot
    if ((Test-Path -LiteralPath (Join-Path $copyRoot 'runtime_acceptance.exe') -PathType Leaf) -or
        (Test-Path -LiteralPath (Join-Path $copyRoot 'export_scan_result_summary.exe') -PathType Leaf) -or
        (Test-Path -LiteralPath (Join-Path $copyRoot 'unexpected-test.exe') -PathType Leaf) -or
        -not (Test-Path -LiteralPath (Join-Path $copyRoot 'desktop.exe') -PathType Leaf)) {
        throw '正式 release 复制边界错误：外置工具不能进入 formal root，desktop 必须保留'
    }

    # 自动组装也必须只带正式五个 FFmpeg DLL；源 staging 的额外文件不能泄漏进 formal root。
    $assemblySource = Join-Path $fixtureRoot 'assembly-source'
    $assemblyCargo = Join-Path $assemblySource 'cargo'
    $assemblyRoot = Join-Path $assemblyCargo 'fixture-target\release'
    $assemblyEverything = Join-Path $assemblySource 'third_party\everything'
    $assemblyRuntime = Join-Path $assemblySource 'dist-rust-v2\staging\runtime\ffmpeg'
    New-Item -ItemType Directory -Path $assemblyRoot, $assemblyEverything, $assemblyRuntime -Force | Out-Null
    foreach ($name in @('desktop.exe', 'node.exe', 'worker.exe')) {
        [IO.File]::WriteAllText((Join-Path $assemblyRoot $name), "fixture $name")
    }
    [IO.File]::WriteAllText((Join-Path $assemblyEverything 'Everything.exe'), 'fixture Everything.exe')
    foreach ($name in @('avutil-60.dll', 'swresample-6.dll', 'swscale-9.dll', 'avcodec-62.dll', 'avformat-62.dll', 'unexpected-runtime.dll')) {
        [IO.File]::WriteAllText((Join-Path $assemblyRuntime $name), "fixture $name")
    }
    $savedRepositoryRoot = $script:RepositoryRoot
    $savedTargetTriple = $script:TargetTriple
    try {
        $script:RepositoryRoot = $assemblySource
        $script:TargetTriple = 'fixture-target'
        $assembled = Resolve-ReleaseRoot -CargoTargetDir $assemblyCargo -ReleaseRoot ''
        if (Test-Path -LiteralPath (Join-Path $assembled 'runtime\ffmpeg\unexpected-runtime.dll') -PathType Leaf) {
            throw '自动组装 formal root 不得复制额外 runtime 文件'
        }
        foreach ($name in @('avutil-60.dll', 'swresample-6.dll', 'swscale-9.dll', 'avcodec-62.dll', 'avformat-62.dll')) {
            if (-not (Test-Path -LiteralPath (Join-Path $assembled "runtime\ffmpeg\$name") -PathType Leaf)) {
                throw "自动组装缺少正式 FFmpeg DLL：$name"
            }
        }
    }
    finally {
        $script:RepositoryRoot = $savedRepositoryRoot
        $script:TargetTriple = $savedTargetTriple
    }

    $layout = New-RuntimeAcceptanceLayout -RunId 'fixture-run'
    if (-not $layout.Root.StartsWith('C:\tmp\rust-v2-runtime-acceptance\', [StringComparison]::OrdinalIgnoreCase)) {
        throw "staging必须位于C:\tmp，实际=$($layout.Root)"
    }
    if (-not $layout.Data.StartsWith($layout.Root) -or -not $layout.Evidence.StartsWith($layout.Root)) {
        throw 'data/evidence必须位于同一隔离根'
    }
    foreach ($name in @('Release', 'Data', 'Logs', 'Cache', 'Temp', 'Evidence', 'Tools')) {
        if ([string]::IsNullOrWhiteSpace([string]$layout.$name)) {
            throw "隔离布局缺少字段：$name"
        }
    }

    # RED/GREEN：真实构造 Node 布局与配置，验证缓存安全边界及证据根独立性。
    $releaseRoot = Get-NormalizedAbsolutePath -Path $layout.Release
    if (-not (Test-PathWithin -Candidate $layout.Data -Root $releaseRoot) -or
        -not (Test-PathWithin -Candidate $layout.Logs -Root $releaseRoot) -or
        -not (Test-PathWithin -Candidate $layout.Cache -Root $releaseRoot) -or
        (Get-NormalizedAbsolutePath -Path $layout.Cache).Equals($releaseRoot, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'RUST_V2_ACCEPTANCE_LAYOUT_RELEASE_BOUNDARY: Node Data/Logs/Cache 必须位于 Release 内，且 Cache 不能等于 Release'
    }
    if ((Test-PathWithin -Candidate $layout.Evidence -Root $releaseRoot) -or
        (Test-PathWithin -Candidate $layout.Report -Root $releaseRoot) -or
        -not (Test-PathWithin -Candidate $layout.Report -Root $layout.Evidence)) {
        throw 'RUST_V2_ACCEPTANCE_LAYOUT_EVIDENCE_BOUNDARY: Evidence/Report 必须在 Release 外独立保存，且 Report 位于 Evidence 内'
    }
    $layoutConfig = New-IsolatedNodeConfig -Port 39125 -DataRoot $layout.Data
    foreach ($expectedPath in @($layout.Data, (Join-Path $layout.Data 'config.toml'),
            (Join-Path $layout.Data 'logs'), (Join-Path $layout.Data 'cache'))) {
        $expectedToml = ConvertTo-TomlBasicString -Value (Get-NormalizedAbsolutePath -Path $expectedPath)
        if ($layoutConfig.IndexOf($expectedToml, [StringComparison]::Ordinal) -lt 0) {
            throw "RUST_V2_ACCEPTANCE_LAYOUT_CONFIG_BOUNDARY: Node 配置未引用布局路径：$expectedPath"
        }
    }

    $config = New-IsolatedNodeConfig -Port 39123 -WorkerCount 20 `
        -HddThreadsPerDisk 1 -SsdThreadsPerDisk 16 -UnknownThreadsPerDisk 1 `
        -TotalReadThreads 12 -ReservedCores 1
    if ($config -notmatch 'config_path = "data/node/config.toml"' -or
        $config -notmatch 'data_path = "data/node"' -or
        $config -notmatch 'enumerator = "everything"' -or
        $config -notmatch 'worker_count = 20' -or
        $config -notmatch 'mode = "manual"' -or
        $config -notmatch 'manual_worker_count = 20' -or
        $config -notmatch 'ssd_threads_per_disk = 16' -or
        $config -notmatch 'total_threads = 12') {
        throw "相对路径配置或测试专用Everything错误：$config"
    }

    $walkerConfig = New-IsolatedNodeConfig -Port 39124 -Enumerator windows_walker
    if ($walkerConfig -notmatch 'enumerator = "windows_walker"') {
        throw "显式 Windows Walker 未写入 Node 配置：$walkerConfig"
    }

    # Task 17 GREEN：真实 pwsh sleeper 只验证监督边界，不启动产品、Worker 或媒体。
    function New-RealSupervisorSleeperFixture {
        <# 启动可杀进程树；client 单进程，Node 替身再派生一个长睡眠 child。 #>
        param([Parameter(Mandatory)] [string] $Root, [Parameter(Mandatory)] [string] $Name)

        $fixtureRootPath = Join-Path $Root $Name
        New-Item -ItemType Directory -Path $fixtureRootPath -Force | Out-Null
        $pwshPath = (Join-Path $PSHOME 'pwsh.exe')
        $sleeperScript = "`$ErrorActionPreference = 'SilentlyContinue'; while (`$true) { Start-Sleep -Seconds 60 }"
        $sleeperEncoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($sleeperScript))
        $client = Microsoft.PowerShell.Management\Start-Process -FilePath $pwshPath `
            -ArgumentList @('-NoLogo', '-NoProfile', '-NonInteractive', '-EncodedCommand', $sleeperEncoded) `
            -PassThru -WindowStyle Hidden

        $childPidPath = Join-Path $fixtureRootPath 'node-child.pid'
        $nodeScript = @'
$childInfo = [Diagnostics.ProcessStartInfo]::new()
$childInfo.FileName = (Join-Path $PSHOME 'pwsh.exe')
$childInfo.UseShellExecute = $false
$childInfo.CreateNoWindow = $true
$childInfo.RedirectStandardOutput = $false
$childInfo.RedirectStandardError = $false
$childInfo.ArgumentList.Add('-NoLogo')
$childInfo.ArgumentList.Add('-NoProfile')
$childInfo.ArgumentList.Add('-NonInteractive')
$childInfo.ArgumentList.Add('-EncodedCommand')
$childInfo.ArgumentList.Add('__CHILD_ENCODED__')
$child = [Diagnostics.Process]::new()
$child.StartInfo = $childInfo
if (-not $child.Start()) { exit 7 }
[IO.File]::WriteAllText('__CHILD_PID_PATH__', [string]$child.Id, [Text.UTF8Encoding]::new($false))
while ($true) { Start-Sleep -Seconds 60 }
'@
        $nodeScript = $nodeScript.Replace('__CHILD_ENCODED__', $sleeperEncoded)
        $nodeScript = $nodeScript.Replace('__CHILD_PID_PATH__', $childPidPath.Replace("'", "''"))
        $nodeEncoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($nodeScript))
        $node = Microsoft.PowerShell.Management\Start-Process -FilePath $pwshPath `
            -ArgumentList @('-NoLogo', '-NoProfile', '-NonInteractive', '-EncodedCommand', $nodeEncoded) `
            -PassThru -WindowStyle Hidden

        $childDeadline = [DateTime]::UtcNow.AddSeconds(5)
        while (-not (Test-Path -LiteralPath $childPidPath -PathType Leaf) -and [DateTime]::UtcNow -lt $childDeadline) {
            Start-Sleep -Milliseconds 50
        }
        if (-not (Test-Path -LiteralPath $childPidPath -PathType Leaf)) {
            throw "真实 Node 替身未写 child PID：$childPidPath"
        }
        [pscustomobject]@{
            Root = $fixtureRootPath
            Client = $client
            Node = $node
            ChildPidPath = $childPidPath
            PwshPath = $pwshPath
        }
    }

    function Stop-RealSupervisorFixtureProcess {
        <# 仅清理本 fixture 返回的进程对象；Kill(true) 后有界等待，不使用宽泛 PID 枚举。 #>
        param([object] $Process)
        try {
            if ($null -ne $Process) {
                $Process.Refresh()
                if (-not $Process.HasExited) { $Process.Kill($true) }
                [void]$Process.WaitForExit(5000)
            }
        }
        catch { }
    }

    $realSupervisorFixtureRoot = Join-Path $fixtureRoot 'real-supervisor-fixtures'
    $realSupervisorFixtureProcesses = @()
    $realSupervisorFixtureSupervisors = @()
    $realSupervisorFixtureRoots = @()
    New-Item -ItemType Directory -Path $realSupervisorFixtureRoot -Force | Out-Null
    try {
        # 真实超时：验证原子 stopping/complete 状态、双树 Kill(true) 和客户端/Node/child/supervisor 退出。
        $timeoutFixture = New-RealSupervisorSleeperFixture -Root $realSupervisorFixtureRoot -Name 'timeout'
        $realSupervisorFixtureProcesses += @($timeoutFixture.Client, $timeoutFixture.Node)
        $realSupervisorFixtureRoots += $timeoutFixture.Root
        $timeoutChildPid = [int][IO.File]::ReadAllText($timeoutFixture.ChildPidPath)
        $timeoutChild = Get-Process -Id $timeoutChildPid -ErrorAction Stop
        $realSupervisorFixtureProcesses += $timeoutChild
        $timeoutStatusPath = Join-Path $timeoutFixture.Root 'supervisor-status.json'
        $timeoutSupervisor = Start-RuntimeAcceptanceSupervisor -ClientId $timeoutFixture.Client.Id `
            -ClientPath $timeoutFixture.PwshPath `
            -ClientStartTimeUtc $timeoutFixture.Client.StartTime.ToUniversalTime().ToString('O') `
            -NodeId $timeoutFixture.Node.Id -NodePath $timeoutFixture.PwshPath `
            -NodeStartTimeUtc $timeoutFixture.Node.StartTime.ToUniversalTime().ToString('O') `
            -DeadlineUtc ([DateTime]::UtcNow.AddSeconds(1)) -StatusPath $timeoutStatusPath
        $realSupervisorFixtureSupervisors += $timeoutSupervisor
        $timeoutStatus = $null
        $timeoutWaitDeadline = [DateTime]::UtcNow.AddSeconds(15)
        while ([DateTime]::UtcNow -lt $timeoutWaitDeadline) {
            $timeoutStatus = Get-RuntimeAcceptanceSupervisorStatus -Supervisor $timeoutSupervisor
            if ($null -ne $timeoutStatus -and [string]$timeoutStatus.Phase -eq 'complete') { break }
            Start-Sleep -Milliseconds 100
        }
        $timeoutSupervisorExited = Wait-RuntimeAcceptanceProcessExit -Process $timeoutSupervisor.Process -TimeoutMilliseconds 5000
        $timeoutFixture.Client.Refresh(); $timeoutFixture.Node.Refresh(); $timeoutChild.Refresh()
        $timeoutTmp = @(Get-ChildItem -LiteralPath $timeoutFixture.Root -Recurse -Force -File -ErrorAction SilentlyContinue |
            Where-Object { $_.Name -like '*.tmp' })
        if ($null -eq $timeoutStatus -or -not $timeoutStatus.TimedOut -or
            [string]$timeoutStatus.Phase -cne 'complete' -or
            [string]$timeoutStatus.Diagnostic -cne 'RUST_V2_ACCEPTANCE_SUPERVISOR_TIMEOUT' -or
            -not $timeoutStatus.ExitConfirmed -or -not $timeoutSupervisorExited -or
            -not $timeoutFixture.Client.HasExited -or -not $timeoutFixture.Node.HasExited -or
            -not $timeoutChild.HasExited -or $timeoutTmp.Count -ne 0) {
            throw "真实 supervisor timeout 证据错误：status=$($timeoutStatus | ConvertTo-Json -Compress) supervisor_exit=$timeoutSupervisorExited client=$($timeoutFixture.Client.HasExited) node=$($timeoutFixture.Node.HasExited) child=$($timeoutChild.HasExited) tmp=$($timeoutTmp.Count)"
        }

        # 真实身份失配：错误启动时间只能产生诊断，client/Node/child 均必须保持存活。
        $mismatchFixture = New-RealSupervisorSleeperFixture -Root $realSupervisorFixtureRoot -Name 'identity-mismatch'
        $realSupervisorFixtureProcesses += @($mismatchFixture.Client, $mismatchFixture.Node)
        $realSupervisorFixtureRoots += $mismatchFixture.Root
        $mismatchChildPid = [int][IO.File]::ReadAllText($mismatchFixture.ChildPidPath)
        $mismatchChild = Get-Process -Id $mismatchChildPid -ErrorAction Stop
        $realSupervisorFixtureProcesses += $mismatchChild
        $mismatchStatusPath = Join-Path $mismatchFixture.Root 'supervisor-status.json'
        $mismatchSupervisor = Start-RuntimeAcceptanceSupervisor -ClientId $mismatchFixture.Client.Id `
            -ClientPath $mismatchFixture.PwshPath `
            -ClientStartTimeUtc $mismatchFixture.Client.StartTime.ToUniversalTime().AddSeconds(-10).ToString('O') `
            -NodeId $mismatchFixture.Node.Id -NodePath $mismatchFixture.PwshPath `
            -NodeStartTimeUtc $mismatchFixture.Node.StartTime.ToUniversalTime().ToString('O') `
            -DeadlineUtc ([DateTime]::UtcNow.AddSeconds(5)) -StatusPath $mismatchStatusPath
        $realSupervisorFixtureSupervisors += $mismatchSupervisor
        $mismatchStatus = $null
        $mismatchWaitDeadline = [DateTime]::UtcNow.AddSeconds(10)
        while ([DateTime]::UtcNow -lt $mismatchWaitDeadline) {
            $mismatchStatus = Get-RuntimeAcceptanceSupervisorStatus -Supervisor $mismatchSupervisor
            if ($null -ne $mismatchStatus) { break }
            Start-Sleep -Milliseconds 100
        }
        $mismatchSupervisorExited = Wait-RuntimeAcceptanceProcessExit -Process $mismatchSupervisor.Process -TimeoutMilliseconds 5000
        $mismatchFixture.Client.Refresh(); $mismatchFixture.Node.Refresh(); $mismatchChild.Refresh()
        $mismatchStop = Stop-RuntimeAcceptanceSupervisor -Supervisor $mismatchSupervisor
        if ($null -eq $mismatchStatus -or $mismatchStatus.TimedOut -or
            [string]$mismatchStatus.Diagnostic -notmatch 'RUST_V2_ACCEPTANCE_SUPERVISOR_CLIENT_PID_REUSED' -or
            -not $mismatchSupervisorExited -or -not $mismatchStop.ExitConfirmed -or
            $mismatchFixture.Client.HasExited -or $mismatchFixture.Node.HasExited -or $mismatchChild.HasExited) {
            throw "真实 supervisor identity mismatch 错误：status=$($mismatchStatus | ConvertTo-Json -Compress) supervisor_exit=$mismatchSupervisorExited client_alive=$(-not $mismatchFixture.Client.HasExited) node_alive=$(-not $mismatchFixture.Node.HasExited) child_alive=$(-not $mismatchChild.HasExited)"
        }

        # 真实 wait seam：独立 helper 延迟原子覆写 stopping 状态，Wait 不能提前取消 helper。
        $waitRoot = Join-Path $realSupervisorFixtureRoot 'wait-final-status'
        New-Item -ItemType Directory -Path $waitRoot -Force | Out-Null
        $waitStatusPath = Join-Path $waitRoot 'supervisor-status.json'
        $waitTempPath = Join-Path $waitRoot 'supervisor-status.helper.tmp'
        $stoppingJson = [ordered]@{
            TimedOut = $true; StopAttempted = $true; ExitConfirmed = $false
            Diagnostic = 'RUST_V2_ACCEPTANCE_SUPERVISOR_TIMEOUT'; Phase = 'stopping'
        } | ConvertTo-Json -Compress
        $completeJson = [ordered]@{
            TimedOut = $true; StopAttempted = $true; ExitConfirmed = $true
            Diagnostic = 'RUST_V2_ACCEPTANCE_SUPERVISOR_TIMEOUT'; Phase = 'complete'
        } | ConvertTo-Json -Compress
        [IO.File]::WriteAllText($waitStatusPath, $stoppingJson, [Text.UTF8Encoding]::new($false))
        $waitHelperScript = @'
Start-Sleep -Milliseconds 200
[IO.File]::WriteAllText('__WAIT_TEMP__', '__WAIT_FINAL__', [Text.UTF8Encoding]::new($false))
[IO.File]::Move('__WAIT_TEMP__', '__WAIT_STATUS__', $true)
'@
        $waitHelperScript = $waitHelperScript.Replace('__WAIT_TEMP__', $waitTempPath.Replace("'", "''"))
        $waitHelperScript = $waitHelperScript.Replace('__WAIT_STATUS__', $waitStatusPath.Replace("'", "''"))
        $waitHelperScript = $waitHelperScript.Replace('__WAIT_FINAL__', $completeJson.Replace("'", "''"))
        $waitHelperEncoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($waitHelperScript))
        $waitHelper = Microsoft.PowerShell.Management\Start-Process -FilePath (Join-Path $PSHOME 'pwsh.exe') `
            -ArgumentList @('-NoLogo', '-NoProfile', '-NonInteractive', '-EncodedCommand', $waitHelperEncoded) `
            -PassThru -WindowStyle Hidden
        $realSupervisorFixtureProcesses += $waitHelper
        $waitSupervisor = [pscustomobject]@{ Process = $waitHelper; StatusPath = $waitStatusPath }
        $waitResult = Wait-RuntimeAcceptanceSupervisorFinalStatus -Supervisor $waitSupervisor -TimeoutMilliseconds 5000
        $waitHelperExited = Wait-RuntimeAcceptanceProcessExit -Process $waitHelper -TimeoutMilliseconds 5000
        $waitTmp = @(Get-ChildItem -LiteralPath $waitRoot -Recurse -Force -File -ErrorAction SilentlyContinue |
            Where-Object { $_.Name -like '*.tmp' })
        if ($null -eq $waitResult -or [string]$waitResult.Phase -cne 'complete' -or
            [string]$waitResult.Diagnostic -cne 'RUST_V2_ACCEPTANCE_SUPERVISOR_TIMEOUT' -or
            -not $waitResult.ExitConfirmed -or -not $waitHelperExited -or $waitTmp.Count -ne 0) {
            throw "Wait supervisor final status 错误：result=$($waitResult | ConvertTo-Json -Compress) helper_exit=$waitHelperExited tmp=$($waitTmp.Count)"
        }
        $script:realSupervisorFixtureRegistered = $true
    }
    finally {
        foreach ($supervisor in @($realSupervisorFixtureSupervisors)) {
            try { Stop-RuntimeAcceptanceSupervisor -Supervisor $supervisor | Out-Null } catch { }
        }
        foreach ($process in @($realSupervisorFixtureProcesses)) {
            Stop-RealSupervisorFixtureProcess -Process $process
        }
        foreach ($rootPath in @($realSupervisorFixtureRoots)) {
            if (Test-Path -LiteralPath $rootPath) {
                Remove-Item -LiteralPath $rootPath -Recurse -Force -ErrorAction SilentlyContinue
            }
        }
    }

    # Task 17 RED：客户端永不退出时，外层监督必须在最大窗口加收尾宽限内返回，并完成受控清理。
    $watchdogDeadline = $null
    try {
        $watchdogDeadline = Get-RuntimeAcceptanceSupervisorDeadlineSeconds -DurationSeconds 1800
    }
    catch {
        $watchdogDeadline = $null
    }
    if ($watchdogDeadline -ne 1920) {
        throw "RED: 缺少外层监督截止时间契约，期望1920秒，实际=$watchdogDeadline"
    }
    if ($null -eq (Get-Command Start-RuntimeAcceptanceSupervisor -CommandType Function -ErrorAction SilentlyContinue) -or
        $null -eq (Get-Command Wait-RuntimeAcceptanceProcessExit -CommandType Function -ErrorAction SilentlyContinue)) {
        throw 'RED: 外层监督必须有独立 supervisor 和 Stop 后退出确认 seam'
    }
    $watchdogFunctionNames = @(
        'Resolve-ReleaseRoot', 'Assert-RuntimeAcceptanceInputs', 'Test-IsAdministrator',
        'Get-FreeTcpPort', 'Wait-TcpEndpoint', 'Start-Process', 'Start-Sleep',
        'Stop-Process', 'Write-SystemSample', 'Get-RuntimeAcceptanceElapsedSeconds',
        'Start-RuntimeAcceptanceSupervisor', 'Get-RuntimeAcceptanceSupervisorStatus',
        'Stop-RuntimeAcceptanceSupervisor', 'Wait-RuntimeAcceptanceProcessExit',
        'Get-LastRuntimeResult', 'Request-IsolatedNodeExit')
    $savedWatchdogFunctions = @{}
    foreach ($functionName in $watchdogFunctionNames) {
        $functionCommand = Get-Command $functionName -CommandType Function -ErrorAction SilentlyContinue
        $savedWatchdogFunctions[$functionName] = if ($functionCommand) { $functionCommand.ScriptBlock } else { $null }
    }
    $script:watchdogClientStarts = 0
    $script:watchdogStopCalls = 0
    $script:watchdogNodeStopped = $false
    $script:watchdogClockTick = 0
    $script:watchdogSleepMilliseconds = @()
    $script:watchdogWaitCalls = 0
    $script:watchdogExitConfirmed = $true
    $script:watchdogSampleFailure = $false
    $script:watchdogSupervisorStarts = 0
    $script:watchdogSupervisorStops = 0
    $script:watchdogSupervisorDiagnostic = ''
    $script:watchdogReleaseRoot = $release
    try {
        function Resolve-ReleaseRoot {
            param([string] $CargoTargetDir, [string] $ReleaseRoot)
            $script:watchdogReleaseRoot
        }
        function Assert-RuntimeAcceptanceInputs {
            param(
                [string] $MediaRoot, [string[]] $MediaRoots = @(), [int] $DurationSeconds, [int] $SampleSeconds,
                [string] $ReleaseRoot, [string] $AcceptanceClientPath, [string] $ResultExporterPath,
                [string] $EvidenceRoot, [string] $ReportPath, [string] $Variant = 'A',
                [int] $RunIndex = 1, [int] $WorkerCount = 20, [int] $HddThreadsPerDisk = 1,
                [int] $SsdThreadsPerDisk = 16, [int] $UnknownThreadsPerDisk = 1,
                [int] $TotalReadThreads = 12, [int] $ReservedCores = 1, [string] $Enumerator = 'everything',
                [switch] $SingleRun, [switch] $CompleteWhenTaskTerminal,
                [switch] $RequireDistinctPhysicalDisks, [switch] $ThrowOnError = $true)
            [pscustomobject]@{ Valid = $true; Code = '' }
        }
        function Test-IsAdministrator { $true }
        function Get-FreeTcpPort { 39129 }
        function Wait-TcpEndpoint {
            param([int] $Port, $Process, [int] $TimeoutSeconds = 60)
        }
        function Start-Process {
            param(
                [string] $FilePath, [string] $WorkingDirectory, [switch] $PassThru,
                [string] $WindowStyle, [string] $RedirectStandardOutput, [string] $RedirectStandardError)
            $name = [IO.Path]::GetFileName($FilePath)
            if ($name -ieq 'node.exe') {
                $script:watchdogNode = [pscustomobject]@{
                    HasExited = $false; Id = 6121; ExitCode = 0
                    Path = (Join-Path $script:watchdogReleaseRoot 'node.exe')
                    StartTime = [DateTime]::UtcNow
                }
                return $script:watchdogNode
            }
            if ($name -ieq 'runtime_acceptance.exe') {
                $script:watchdogClientStarts++
                if ($RedirectStandardOutput) { [IO.File]::WriteAllText($RedirectStandardOutput, '') }
                if ($RedirectStandardError) { [IO.File]::WriteAllText($RedirectStandardError, '') }
                $client = [pscustomobject]@{
                    HasExited = $false; Id = 6122; ExitCode = 0
                    Path = $FilePath; StartTime = [DateTime]::UtcNow
                }
                $client | Add-Member -MemberType ScriptMethod -Name Refresh -Value { }
                return $client
            }
            throw "fixture unexpected process=$FilePath"
        }
        function Start-Sleep {
            param([int] $Seconds, [int] $Milliseconds)
            if ($PSBoundParameters.ContainsKey('Milliseconds')) {
                $script:watchdogSleepMilliseconds += $Milliseconds
            }
            else {
                $script:watchdogSleepMilliseconds += $Seconds * 1000
            }
        }
        function Stop-Process {
            [CmdletBinding()]
            param([int] $Id, [switch] $Force)
            if ($Id -eq 6122) { $script:watchdogStopCalls++ }
        }
        function Write-SystemSample {
            param(
                [string] $Path, [string] $Root, [double] $ElapsedMilliseconds,
                [double] $PreviousSampleElapsedMilliseconds, [hashtable] $PreviousCpu,
                [hashtable] $PreviousIo)
            if ($script:watchdogSampleFailure) {
                throw 'fixture sample failure'
            }
            if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
                [IO.File]::WriteAllText($Path, '{"record_type":"system_sample"}' + [char]10)
            }
        }
        function Get-RuntimeAcceptanceElapsedSeconds {
            param([Parameter(Mandatory)] $Stopwatch)
            $script:watchdogClockTick++
            if ($script:watchdogClockTick -eq 1) { return 0.0 }
            if ($script:watchdogClockTick -eq 2) { return 1919.25 }
            1920.0
        }
        function Start-RuntimeAcceptanceSupervisor {
            param(
                [int] $ClientId, [string] $ClientPath, [string] $ClientStartTimeUtc,
                [int] $NodeId, [string] $NodePath, [string] $NodeStartTimeUtc,
                [DateTime] $DeadlineUtc, [string] $StatusPath)
            if ($ClientId -ne 6122 -or $NodeId -ne 6121 -or
                [string]::IsNullOrWhiteSpace($ClientPath) -or
                [string]::IsNullOrWhiteSpace($ClientStartTimeUtc) -or
                [string]::IsNullOrWhiteSpace($NodePath) -or
                [string]::IsNullOrWhiteSpace($NodeStartTimeUtc) -or
                [string]::IsNullOrWhiteSpace($StatusPath)) {
                throw 'fixture supervisor identity binding missing'
            }
            $script:watchdogSupervisorStarts++
            [pscustomobject]@{ Id = 6123 }
        }
        function Get-RuntimeAcceptanceSupervisorStatus {
            param([Parameter(Mandatory)] $Supervisor)
            [pscustomobject]@{ TimedOut = $false; Diagnostic = $script:watchdogSupervisorDiagnostic }
        }
        function Stop-RuntimeAcceptanceSupervisor {
            param([Parameter(Mandatory)] $Supervisor)
            $script:watchdogSupervisorStops++
            [pscustomobject]@{ ExitConfirmed = $true; Diagnostic = '' }
        }
        function Wait-RuntimeAcceptanceProcessExit {
            param([Parameter(Mandatory)] $Process, [int] $TimeoutMilliseconds = 5000)
            $script:watchdogWaitCalls++
            if ($script:watchdogExitConfirmed) {
                $Process.HasExited = $true
                return $true
            }
            $false
        }
        function Get-LastRuntimeResult {
            param([Parameter(Mandatory)] [string] $Path)
            throw 'RUST_V2_ACCEPTANCE_RUNTIME_NDJSON_MISSING'
        }
        function Request-IsolatedNodeExit {
            param([Parameter(Mandatory)] $Node, [Parameter(Mandatory)] [string] $Root, [int] $TimeoutSeconds = 20)
            $Node.HasExited = $true
            $script:watchdogNodeStopped = $true
            ''
        }
        $watchdogEvidence = Join-Path $fixtureRoot 'runs\watchdog-A-1\evidence'
        $watchdogReport = Join-Path $watchdogEvidence 'report.md'
        $watchdogResult = @(Invoke-RustV2RuntimeAcceptance `
            -MediaRoot $media -DurationSeconds 1800 -SampleSeconds 2 `
            -CargoTargetDir $fixtureRoot -ReleaseRoot $release `
            -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
            -EvidenceRoot $watchdogEvidence -ReportPath $watchdogReport `
            -Variant A -RunIndex 1 -SourceRevision 'fixture' -SourceTreeSha256 ('d' * 64) `
            -PackagePath (Join-Path $fixtureRoot 'fixture.zip') -PackageSha256 ('e' * 64) `
            -WorkerCount 20 -HddThreadsPerDisk 1 -SsdThreadsPerDisk 16 `
            -UnknownThreadsPerDisk 1 -TotalReadThreads 12 -ReservedCores 1 -SingleRun)
        $watchdogHarness = [IO.File]::ReadAllText((Join-Path $watchdogEvidence 'harness-result.json')) | ConvertFrom-Json
        if ($script:watchdogClientStarts -ne 1 -or $script:watchdogStopCalls -ne 1 -or
            $script:watchdogWaitCalls -ne 1 -or -not $script:watchdogExitConfirmed -or
            $script:watchdogSupervisorStarts -ne 1 -or $script:watchdogSupervisorStops -ne 1 -or
            -not $script:watchdogNodeStopped -or $script:watchdogClockTick -lt 3) {
            throw "RED: 客户端监督超时必须有界停止、等待确认并清理：starts=$script:watchdogClientStarts stop=$script:watchdogStopCalls wait=$script:watchdogWaitCalls confirmed=$script:watchdogExitConfirmed supervisor=$script:watchdogSupervisorStarts/$script:watchdogSupervisorStops node=$script:watchdogNodeStopped ticks=$script:watchdogClockTick"
        }
        if (@($script:watchdogSleepMilliseconds | Where-Object { [int]$_ -gt 750 }).Count -gt 0) {
            throw "RED: 临界点剩余时间小于采样周期时不得固定睡眠2秒：$($script:watchdogSleepMilliseconds -join ',')"
        }
        if ([string]$watchdogHarness.run_status -cne 'INCONCLUSIVE' -or
            [string]$watchdogHarness.run_diagnostic -cne 'RUST_V2_ACCEPTANCE_SUPERVISOR_TIMEOUT' -or
            -not ($watchdogResult -contains 'RUST_V2_RUNTIME_ACCEPTANCE_MEASURE_INCONCLUSIVE')) {
            throw "RED: 监督超时必须留 INCONCLUSIVE 稳定诊断：$($watchdogHarness | ConvertTo-Json -Compress) result=$($watchdogResult -join '|')"
        }
        if (Test-Path -LiteralPath (Join-Path $watchdogEvidence 'result-summary.tsv') -PathType Leaf) {
            throw 'RED: 监督超时且没有完成任务时不得错误调用 exporter'
        }

        # Stop 后仍存活必须被报告为稳定 unconfirmed，而不是只记录 Stop-Process 调用。
        $script:watchdogClockTick = 0
        $script:watchdogSleepMilliseconds = @()
        $script:watchdogWaitCalls = 0
        $script:watchdogExitConfirmed = $false
        $script:watchdogSampleFailure = $true
        $unconfirmedEvidence = Join-Path $fixtureRoot 'runs\watchdog-unconfirmed-A-1\evidence'
        $unconfirmedReport = Join-Path $unconfirmedEvidence 'report.md'
        $unconfirmedResult = @(Invoke-RustV2RuntimeAcceptance `
            -MediaRoot $media -DurationSeconds 1800 -SampleSeconds 2 `
            -CargoTargetDir $fixtureRoot -ReleaseRoot $release `
            -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
            -EvidenceRoot $unconfirmedEvidence -ReportPath $unconfirmedReport `
            -Variant A -RunIndex 1 -SourceRevision 'fixture' -SourceTreeSha256 ('f' * 64) `
            -PackagePath (Join-Path $fixtureRoot 'fixture-unconfirmed.zip') -PackageSha256 ('a' * 64) `
            -WorkerCount 20 -HddThreadsPerDisk 1 -SsdThreadsPerDisk 16 `
            -UnknownThreadsPerDisk 1 -TotalReadThreads 12 -ReservedCores 1 -SingleRun)
        $unconfirmedHarness = [IO.File]::ReadAllText((Join-Path $unconfirmedEvidence 'harness-result.json')) | ConvertFrom-Json
        if ($script:watchdogWaitCalls -ne 1 -or
            [string]$unconfirmedHarness.run_status -cne 'INCONCLUSIVE' -or
            [string]$unconfirmedHarness.run_diagnostic -notmatch 'RUST_V2_ACCEPTANCE_CLIENT_EXIT_UNCONFIRMED' -or
            -not ($unconfirmedResult -contains 'RUST_V2_RUNTIME_ACCEPTANCE_MEASURE_INCONCLUSIVE')) {
            throw "RED: Stop 后未确认退出必须稳定留 INCONCLUSIVE：wait=$script:watchdogWaitCalls harness=$($unconfirmedHarness | ConvertTo-Json -Compress) result=$($unconfirmedResult -join '|')"
        }

        # 身份不匹配必须 fail-closed，不能让监督器继续盲杀复用后的 PID，也不能进入 exporter。
        $script:watchdogClockTick = 0
        $script:watchdogSleepMilliseconds = @()
        $script:watchdogWaitCalls = 0
        $script:watchdogExitConfirmed = $true
        $script:watchdogSampleFailure = $false
        $script:watchdogSupervisorDiagnostic = 'RUST_V2_ACCEPTANCE_SUPERVISOR_CLIENT_PID_REUSED'
        $identityEvidence = Join-Path $fixtureRoot 'runs\watchdog-identity-mismatch-A-1\evidence'
        $identityReport = Join-Path $identityEvidence 'report.md'
        $identityResult = @(Invoke-RustV2RuntimeAcceptance `
            -MediaRoot $media -DurationSeconds 1800 -SampleSeconds 2 `
            -CargoTargetDir $fixtureRoot -ReleaseRoot $release `
            -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
            -EvidenceRoot $identityEvidence -ReportPath $identityReport `
            -Variant A -RunIndex 1 -SourceRevision 'fixture' -SourceTreeSha256 ('1' * 64) `
            -PackagePath (Join-Path $fixtureRoot 'fixture-identity.zip') -PackageSha256 ('2' * 64) `
            -WorkerCount 20 -HddThreadsPerDisk 1 -SsdThreadsPerDisk 16 `
            -UnknownThreadsPerDisk 1 -TotalReadThreads 12 -ReservedCores 1 -SingleRun)
        $identityHarness = [IO.File]::ReadAllText((Join-Path $identityEvidence 'harness-result.json')) | ConvertFrom-Json
        if ([string]$identityHarness.run_status -cne 'INCONCLUSIVE' -or
            [string]$identityHarness.run_diagnostic -notmatch 'RUST_V2_ACCEPTANCE_SUPERVISOR_CLIENT_PID_REUSED' -or
            -not ($identityResult -contains 'RUST_V2_RUNTIME_ACCEPTANCE_MEASURE_INCONCLUSIVE') -or
            (Test-Path -LiteralPath (Join-Path $identityEvidence 'result-summary.tsv') -PathType Leaf)) {
            throw "RED: 监督身份不匹配必须 fail-closed：harness=$($identityHarness | ConvertTo-Json -Compress) result=$($identityResult -join '|')"
        }
        $script:watchdogSupervisorDiagnostic = ''
    }
    finally {
        foreach ($entry in $savedWatchdogFunctions.GetEnumerator()) {
            $functionPath = "Function:\$($entry.Key)"
            if ($null -eq $entry.Value) {
                Remove-Item -LiteralPath $functionPath -ErrorAction SilentlyContinue
            }
            else {
                Set-Item -LiteralPath $functionPath -Value $entry.Value
            }
        }
        Remove-Variable -Name watchdogClientStarts, watchdogStopCalls, watchdogNodeStopped, watchdogClockTick, watchdogReleaseRoot, watchdogNode, watchdogSleepMilliseconds, watchdogWaitCalls, watchdogExitConfirmed, watchdogSampleFailure, watchdogSupervisorStarts, watchdogSupervisorStops, watchdogSupervisorDiagnostic -Scope Script -ErrorAction SilentlyContinue
    }

    $specialDataRoot = 'C:\tmp\fixture data\quote"name'
    $specialConfig = New-IsolatedNodeConfig -Port 39124 -DataRoot $specialDataRoot
    $expectedEscapedData = 'data_path = "C:\\tmp\\fixture data\\quote\"name"'
    if ($specialConfig.IndexOf($expectedEscapedData, [StringComparison]::Ordinal) -lt 0 -or
        $specialConfig.IndexOf('config_path = "C:\\tmp\\fixture data\\quote\"name\\config.toml"', [StringComparison]::Ordinal) -lt 0) {
        throw "绝对 Windows 路径必须按 TOML basic string 转义反斜杠/引号：$specialConfig"
    }

    $invalidWorkers = Assert-RuntimeAcceptanceInputs `
        -MediaRoot $media -DurationSeconds 1800 -SampleSeconds 2 -ReleaseRoot $release `
        -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
        -EvidenceRoot $evidence -ReportPath $report -Variant A -RunIndex 1 `
        -WorkerCount 0 -HddThreadsPerDisk 1 -SsdThreadsPerDisk 16 `
        -UnknownThreadsPerDisk 1 -TotalReadThreads 12 -ReservedCores 1 `
        -ThrowOnError:$false
    if ($invalidWorkers.Valid -or $invalidWorkers.Code -ne 'RUST_V2_ACCEPTANCE_WORKER_COUNT_INVALID') {
        throw 'Worker数量必须在启动前验证'
    }

    # RED：数据库快照必须复制主库及已有 WAL/SHM，并为每轮证据生成只读、不可覆盖的旁证。
    $snapshotSourceRoot = Join-Path $fixtureRoot 'snapshot-source'
    $snapshotEvidenceRoot = Join-Path $fixtureRoot 'snapshot-evidence'
    New-Item -ItemType Directory -Path $snapshotSourceRoot, $snapshotEvidenceRoot -Force | Out-Null
    $snapshotDatabasePath = Join-Path $snapshotSourceRoot 'node.db'
    [IO.File]::WriteAllText($snapshotDatabasePath, 'fixture database')
    [IO.File]::WriteAllText("$snapshotDatabasePath-wal", 'fixture wal')
    [IO.File]::WriteAllText("$snapshotDatabasePath-shm", 'fixture shm')
    $snapshot = New-ReadOnlyDatabaseSnapshot -DatabasePath $snapshotDatabasePath -EvidenceRoot $snapshotEvidenceRoot
    if (-not (Test-Path -LiteralPath $snapshot.DatabasePath -PathType Leaf) -or
        -not (Test-Path -LiteralPath $snapshot.MetadataPath -PathType Leaf) -or
        -not (Test-Path -LiteralPath $snapshot.SnapshotRoot -PathType Container)) {
        throw 'RED: 数据库快照必须写入唯一目录、主库副本和机器可读 metadata'
    }
    $snapshotMetadata = [IO.File]::ReadAllText($snapshot.MetadataPath) | ConvertFrom-Json
    if ($snapshotMetadata.status -cne 'PASS' -or
        $snapshotMetadata.source_stability_verified -ne $true -or
        @($snapshotMetadata.files).Count -ne 3) {
        throw "RED: 快照 metadata 必须记录稳定性和三件套：$($snapshotMetadata | ConvertTo-Json -Compress)"
    }
    foreach ($snapshotFile in @($snapshotMetadata.files)) {
        $snapshotPath = [string]$snapshotFile.snapshot_path
        $sourcePath = [string]$snapshotFile.source_path
        if ((Get-FileSha256OrNull -Path $sourcePath) -cne [string]$snapshotFile.snapshot_sha256 -or
            (Get-FileSha256OrNull -Path $snapshotPath) -cne [string]$snapshotFile.source_sha256_after -or
            -not (([IO.File]::GetAttributes($snapshotPath) -band [IO.FileAttributes]::ReadOnly) -ne 0)) {
            throw "RED: 快照副本必须与稳定源一致并标记 ReadOnly：$($snapshotFile | ConvertTo-Json -Compress)"
        }
    }
    $snapshotAgain = New-ReadOnlyDatabaseSnapshot -DatabasePath $snapshotDatabasePath -EvidenceRoot $snapshotEvidenceRoot
    if ($snapshotAgain.SnapshotRoot -ceq $snapshot.SnapshotRoot -or
        -not (Test-Path -LiteralPath $snapshot.MetadataPath -PathType Leaf)) {
        throw 'RED: 重复快照不得覆盖既有证据目录'
    }

    # RED：持久 WAL 模式必须要求主库、WAL、SHM 三件套完整；缺失时保留失败快照目录且不得继续导出。
    $missingWalSnapshotCount = @(Get-ChildItem -LiteralPath $snapshotEvidenceRoot -Directory -Filter 'database-snapshot-*').Count
    Remove-Item -LiteralPath "$snapshotDatabasePath-wal" -Force
    $missingWalCode = ''
    try { New-ReadOnlyDatabaseSnapshot -DatabasePath $snapshotDatabasePath -EvidenceRoot $snapshotEvidenceRoot | Out-Null }
    catch { $missingWalCode = $_.Exception.Message }
    $missingWalSnapshotCountAfter = @(Get-ChildItem -LiteralPath $snapshotEvidenceRoot -Directory -Filter 'database-snapshot-*').Count
    if ($missingWalCode -notlike '*RUST_V2_ACCEPTANCE_DATABASE_SNAPSHOT_SIDECAR_MISSING:node.db-wal*' -or
        $missingWalSnapshotCountAfter -le $missingWalSnapshotCount) {
        throw "RED: 缺 WAL 必须稳定拒绝并保留失败快照目录：code=$missingWalCode before=$missingWalSnapshotCount after=$missingWalSnapshotCountAfter"
    }
    [IO.File]::WriteAllText("$snapshotDatabasePath-wal", 'fixture wal')
    $missingShmSnapshotCount = @(Get-ChildItem -LiteralPath $snapshotEvidenceRoot -Directory -Filter 'database-snapshot-*').Count
    Remove-Item -LiteralPath "$snapshotDatabasePath-shm" -Force
    $missingShmCode = ''
    try { New-ReadOnlyDatabaseSnapshot -DatabasePath $snapshotDatabasePath -EvidenceRoot $snapshotEvidenceRoot | Out-Null }
    catch { $missingShmCode = $_.Exception.Message }
    $missingShmSnapshotCountAfter = @(Get-ChildItem -LiteralPath $snapshotEvidenceRoot -Directory -Filter 'database-snapshot-*').Count
    if ($missingShmCode -notlike '*RUST_V2_ACCEPTANCE_DATABASE_SNAPSHOT_SIDECAR_MISSING:node.db-shm*' -or
        $missingShmSnapshotCountAfter -le $missingShmSnapshotCount) {
        throw "RED: 缺 SHM 必须稳定拒绝并保留失败快照目录：code=$missingShmCode before=$missingShmSnapshotCount after=$missingShmSnapshotCountAfter"
    }
    [IO.File]::WriteAllText("$snapshotDatabasePath-shm", 'fixture shm')

    # RED：metadata 写入必须使用 CreateNew，已有文件不得被替换。
    $metadataCollisionPath = Join-Path $snapshot.SnapshotRoot 'metadata-create-new-test.json'
    [IO.File]::WriteAllText($metadataCollisionPath, 'original metadata')
    $metadataCollisionCode = ''
    try {
        Write-DatabaseSnapshotMetadata -Path $metadataCollisionPath `
            -Metadata ([ordered]@{ status = 'replacement' })
    }
    catch { $metadataCollisionCode = $_.Exception.Message }
    if ($metadataCollisionCode -notlike '*RUST_V2_ACCEPTANCE_DATABASE_SNAPSHOT_METADATA_EXISTS*' -or
        [IO.File]::ReadAllText($metadataCollisionPath) -cne 'original metadata') {
        throw "RED: metadata CreateNew 必须拒绝覆盖既有文件：code=$metadataCollisionCode content=$([IO.File]::ReadAllText($metadataCollisionPath))"
    }

    # RED：摘要有效时，MISSING/INCONCLUSIVE 是业务 FAIL；缺终态、导出失败或绑定缺失仍为基础设施 INCONCLUSIVE。
    $missingSummaryStatus = Get-ResultSummaryRunStatus `
        -CompletedTaskIdPresent:$true -ExporterSucceeded:$true -SummaryBindingValid:$true `
        -SummaryStatus 'MISSING'
    $inconclusiveSummaryStatus = Get-ResultSummaryRunStatus `
        -CompletedTaskIdPresent:$true -ExporterSucceeded:$true -SummaryBindingValid:$true `
        -SummaryStatus 'INCONCLUSIVE'
    $missingTaskStatus = Get-ResultSummaryRunStatus -SummaryStatus 'PASS'
    $failedExporterStatus = Get-ResultSummaryRunStatus `
        -CompletedTaskIdPresent:$true -ExporterSucceeded:$false -SummaryBindingValid:$false `
        -SummaryStatus 'PASS'
    $invalidBindingStatus = Get-ResultSummaryRunStatus `
        -CompletedTaskIdPresent:$true -ExporterSucceeded:$true -SummaryBindingValid:$false `
        -SummaryStatus 'PASS'
    $passSummaryStatus = Get-ResultSummaryRunStatus `
        -CompletedTaskIdPresent:$true -ExporterSucceeded:$true -SummaryBindingValid:$true `
        -SummaryStatus 'PASS'
    if ($missingSummaryStatus -cne 'FAIL' -or $inconclusiveSummaryStatus -cne 'FAIL' -or
        $missingTaskStatus -cne 'INCONCLUSIVE' -or $failedExporterStatus -cne 'INCONCLUSIVE' -or
        $invalidBindingStatus -cne 'INCONCLUSIVE' -or $passSummaryStatus -cne 'PASS') {
        throw "RED: 摘要业务/基础设施分类错误：missing=$missingSummaryStatus inconclusive=$inconclusiveSummaryStatus missingTask=$missingTaskStatus exporter=$failedExporterStatus binding=$invalidBindingStatus pass=$passSummaryStatus"
    }

    # RED：新 exporter 必须使用重复 --media-root，并且只写固定 result-summary.tsv；不得下发 task-id。
    $argumentRoot = Join-Path $fixtureRoot 'export args with space'
    New-Item -ItemType Directory -Path $argumentRoot -Force | Out-Null
    $capturePath = Join-Path $argumentRoot 'captured args.txt'
    $fakeExporter = Join-Path $argumentRoot 'exporter with space.cmd'
    [IO.File]::WriteAllText($fakeExporter, "@echo off`r`necho %*> `"$capturePath`"`r`nexit /b 0`r`n")
    $argumentResult = Invoke-ResultExporter -ExporterPath $fakeExporter `
        -DatabasePath (Join-Path $argumentRoot 'node database.sqlite') `
        -CacheRoot (Join-Path $argumentRoot 'cache root') `
        -MediaRoots @((Join-Path $argumentRoot 'H root'), (Join-Path $argumentRoot 'I root')) `
        -OutputPath (Join-Path $argumentRoot 'result-summary.tsv') `
        -EvidenceRoot $argumentRoot -TimeoutSeconds 5
    $capturedArgs = if (Test-Path -LiteralPath $capturePath -PathType Leaf) { Get-Content -Raw -LiteralPath $capturePath } else { '' }
    $expectedDatabase = Join-Path $argumentRoot 'node database.sqlite'
    $expectedCache = Join-Path $argumentRoot 'cache root'
    $expectedHRoot = Join-Path $argumentRoot 'H root'
    $expectedIRoot = Join-Path $argumentRoot 'I root'
    $expectedOutput = Join-Path $argumentRoot 'result-summary.tsv'
    if ($argumentResult.ExitCode -ne 0 -or
        -not $capturedArgs.Contains('--database') -or -not $capturedArgs.Contains('--cache-root') -or
        ([regex]::Matches($capturedArgs, '--media-root')).Count -ne 2 -or
        $capturedArgs.Contains('--task-id') -or -not $capturedArgs.Contains($expectedDatabase) -or
        -not $capturedArgs.Contains($expectedCache) -or -not $capturedArgs.Contains($expectedHRoot) -or
        -not $capturedArgs.Contains($expectedIRoot) -or -not $capturedArgs.Contains($expectedOutput)) {
        throw "exporter 参数必须重复传递多媒体根且保留带空格路径：$capturedArgs"
    }

    $hangingExporter = Join-Path $argumentRoot 'hanging exporter.cmd'
    # 用 ping 生成无 stdin 依赖的稳定阻塞，避免 timeout 在重定向环境中立即返回。
    [IO.File]::WriteAllText($hangingExporter, "@echo off`r`nping.exe -n 6 127.0.0.1 >nul`r`nexit /b 0`r`n")
    $timeoutResult = Invoke-ResultExporter -ExporterPath $hangingExporter `
        -DatabasePath (Join-Path $argumentRoot 'node database.sqlite') -CacheRoot (Join-Path $argumentRoot 'cache root') `
        -MediaRoots @((Join-Path $argumentRoot 'H root')) -OutputPath (Join-Path $argumentRoot 'result-summary.tsv') `
        -EvidenceRoot $argumentRoot -TimeoutSeconds 1
    if (-not $timeoutResult.TimedOut -or $timeoutResult.Diagnostic -ne 'RUST_V2_ACCEPTANCE_EXPORTER_TIMEOUT') {
        throw "挂起 exporter 必须超时、杀进程并返回稳定诊断：$($timeoutResult | ConvertTo-Json -Compress)"
    }
    $killFailureResult = Invoke-ResultExporter -ExporterPath $hangingExporter `
        -DatabasePath (Join-Path $argumentRoot 'node database.sqlite') -CacheRoot (Join-Path $argumentRoot 'cache root') `
        -MediaRoots @((Join-Path $argumentRoot 'H root')) -OutputPath (Join-Path $argumentRoot 'result-summary.tsv') `
        -EvidenceRoot $argumentRoot -TimeoutSeconds 1 `
        -ProcessKiller { param($process) $process.Kill($true); throw 'fixture kill diagnostic failure' }
    if (-not $killFailureResult.TimedOut -or $killFailureResult.Diagnostic -ne 'RUST_V2_ACCEPTANCE_EXPORTER_KILL_FAILED') {
        throw "exporter kill 失败必须返回稳定诊断且不等待无限 drain：$($killFailureResult | ConvertTo-Json -Compress)"
    }

    $badRuntimePath = Join-Path $fixtureRoot 'bad-runtime.ndjson'
    [IO.File]::WriteAllText($badRuntimePath, "{`"record_type`":`"runtime_result`"}`r`n{not-json`r`n")
    $badRuntimeRejected = $false
    try { Get-LastRuntimeResult -Path $badRuntimePath | Out-Null }
    catch { $badRuntimeRejected = $_.Exception.Message -like '*RUST_V2_ACCEPTANCE_RUNTIME_NDJSON_INVALID*' }
    if (-not $badRuntimeRejected) {
        throw 'runtime NDJSON 坏行必须产生稳定 INCONCLUSIVE 诊断'
    }

    $enumRejected = $false
    try { Get-IsolatedProcesses -Root $release -ProcessEnumerator { throw 'fixture enumeration failure' } | Out-Null }
    catch { $enumRejected = $_.Exception.Message -like '*RUST_V2_ACCEPTANCE_PROCESS_ENUMERATION_FAILED*' }
    if (-not $enumRejected) { throw '进程枚举失败必须显式返回稳定诊断' }
    $stopRejected = $false
    try {
        Stop-IsolatedProcesses -Root $release -ProcessEnumerator { @([pscustomobject]@{ ProcessId = 123 }) } `
            -ProcessTerminator { param($ProcessId) throw "fixture stop failure $ProcessId" }
    }
    catch { $stopRejected = $_.Exception.Message -like '*RUST_V2_ACCEPTANCE_PROCESS_STOP_FAILED*' }
    if (-not $stopRejected) { throw '进程停止失败必须显式返回稳定诊断' }

    $runningClient = [pscustomobject]@{ HasExited = $false }
    $runningClientExitCode = Get-CompletedProcessExitCode -Process $runningClient
    if ($null -ne $runningClientExitCode) {
        throw '仍运行的 client 不得读取 ExitCode 或升级为异常'
    }

    # 固定两次采样的累计计数，验证逐 PID CPU 与进程 I/O 必须按真实采样间隔计算增量。
    $previousCpu = @{}
    $previousIo = @{}
    $firstProcess = [pscustomobject]@{
        Id = 1001
        ProcessName = 'worker'
        StartTime = [DateTime]'2026-08-24T00:00:00Z'
        TotalProcessorTime = [TimeSpan]::FromMilliseconds(1000)
        WorkingSet64 = 268435456
        PrivateMemorySize64 = 234881024
    }
    $firstRow = [pscustomobject]@{
        ProcessId = 1001
        ReadOperationCount = 10
        WriteOperationCount = 4
        ReadTransferCount = 4096
        WriteTransferCount = 1024
        OtherTransferCount = 128
    }
    $firstProcessSample = New-IsolatedProcessSample -Row $firstRow -Process $firstProcess `
        -PreviousCpu $previousCpu -PreviousIo $previousIo -SampleIntervalMs 2000
    if ($firstProcessSample.CpuDeltaMs -ne 0 -or
        $firstProcessSample.ReadTransferDeltaBytes -ne 0 -or
        $firstProcessSample.CpuPercentOfOneCore -ne 0) {
        throw '首见 PID 必须建立累计计数基线，不能把进程启动以来的消耗算入当前 tick'
    }

    $secondProcess = [pscustomobject]@{
        Id = 1001
        ProcessName = 'worker'
        StartTime = [DateTime]'2026-08-24T00:00:00Z'
        TotalProcessorTime = [TimeSpan]::FromMilliseconds(2500)
        WorkingSet64 = 276824064
        PrivateMemorySize64 = 243269632
    }
    $secondRow = [pscustomobject]@{
        ProcessId = 1001
        ReadOperationCount = 13
        WriteOperationCount = 6
        ReadTransferCount = 12288
        WriteTransferCount = 3072
        OtherTransferCount = 640
    }
    $secondProcessSample = New-IsolatedProcessSample -Row $secondRow -Process $secondProcess `
        -PreviousCpu $previousCpu -PreviousIo $previousIo -SampleIntervalMs 2000
    if ($secondProcessSample.ProcessId -ne 1001 -or
        $secondProcessSample.CpuDeltaMs -ne 1500 -or
        $secondProcessSample.CpuPercentOfOneCore -ne 75 -or
        $secondProcessSample.ReadOperationDelta -ne 3 -or
        $secondProcessSample.WriteOperationDelta -ne 2 -or
        $secondProcessSample.ReadTransferDeltaBytes -ne 8192 -or
        $secondProcessSample.WriteTransferDeltaBytes -ne 2048 -or
        $secondProcessSample.OtherTransferDeltaBytes -ne 512) {
        throw "逐 PID 资源增量计算错误：$($secondProcessSample | ConvertTo-Json -Compress)"
    }

    # Windows 可以复用已经退出的 PID；新世代即使累计值更大，也必须重新建立零增量基线。
    $replacementProcess = [pscustomobject]@{
        Id = 1001
        ProcessName = 'worker'
        StartTime = [DateTime]'2026-08-24T00:01:00Z'
        TotalProcessorTime = [TimeSpan]::FromMilliseconds(5000)
        WorkingSet64 = 281018368
        PrivateMemorySize64 = 247463936
    }
    $replacementRow = [pscustomobject]@{
        ProcessId = 1001
        ReadOperationCount = 100
        WriteOperationCount = 60
        ReadTransferCount = 104857600
        WriteTransferCount = 10485760
        OtherTransferCount = 4096
    }
    $replacementSample = New-IsolatedProcessSample -Row $replacementRow -Process $replacementProcess `
        -PreviousCpu $previousCpu -PreviousIo $previousIo -SampleIntervalMs 2000
    if ($replacementSample.ProcessStartTimeUtc -ne '2026-08-24T00:01:00.0000000Z' -or
        $replacementSample.CpuDeltaMs -ne 0 -or
        $replacementSample.ReadOperationDelta -ne 0 -or
        $replacementSample.ReadTransferDeltaBytes -ne 0) {
        throw "复用 PID 的新进程世代必须建立独立基线：$($replacementSample | ConvertTo-Json -Compress)"
    }

    # Windows 进程累计传输计数可在单 tick 超过 Int32，必须按 64 位计数做差。
    $largeIoPreviousCpu = @{ '1001|2026-08-24T00:00:00.0000000Z' = 2500.0 }
    $largeIoPrevious = @{
        '1001|2026-08-24T00:00:00.0000000Z' = [pscustomobject]@{
            ReadOperationCount = 13.0
            WriteOperationCount = 6.0
            ReadTransferCount = 12288.0
            WriteTransferCount = 3072.0
            OtherTransferCount = 640.0
        }
    }
    $largeIoRow = [pscustomobject]@{
        ProcessId = 1001
        ReadOperationCount = 14
        WriteOperationCount = 6
        ReadTransferCount = 3221237760
        WriteTransferCount = 3072
        OtherTransferCount = 640
    }
    $largeIoSample = New-IsolatedProcessSample -Row $largeIoRow -Process $secondProcess `
        -PreviousCpu $largeIoPreviousCpu -PreviousIo $largeIoPrevious -SampleIntervalMs 2000
    if ($largeIoSample.ReadTransferDeltaBytes -ne 3221225472) {
        throw "大于 Int32 的进程 I/O 增量必须保留为 64 位，实际=$($largeIoSample.ReadTransferDeltaBytes)"
    }

    # 固定性能计数器样本，验证逻辑核热点与物理盘读写拆分不会在序列化前丢失。
    $processorSample = New-LogicalProcessorSample -Row ([pscustomobject]@{
        Name = '3'
        PercentProcessorTime = 91
        PercentUserTime = 72
        PercentPrivilegedTime = 19
        PercentInterruptTime = 2
        PercentDPCTime = 3
    })
    if ($processorSample.Name -ne '3' -or
        $processorSample.PercentProcessorTime -ne 91 -or
        $processorSample.PercentUserTime -ne 72 -or
        $processorSample.PercentPrivilegedTime -ne 19 -or
        $processorSample.PercentInterruptTime -ne 2 -or
        $processorSample.PercentDPCTime -ne 3) {
        throw "逻辑核采样字段错误：$($processorSample | ConvertTo-Json -Compress)"
    }

    $diskSample = New-PhysicalDiskSample -Row ([pscustomobject]@{
        Name = '2 I:'
        DiskReadBytesPersec = 104857600
        DiskWriteBytesPersec = 2097152
        DiskReadsPersec = 120
        DiskWritesPersec = 8
        AvgDiskQueueLength = 3.5
        AvgDiskReadQueueLength = 3.25
        AvgDiskWriteQueueLength = 0.25
        CurrentDiskQueueLength = 4
        PercentDiskTime = 98
        AvgDisksecPerRead = 0.012
        AvgDisksecPerWrite = 0.004
        SplitIOPerSec = 1
    })
    if ($diskSample.Name -ne '2 I:' -or
        $diskSample.DiskNumber -ne 2 -or
        $diskSample.DiskReadBytesPerSec -ne 104857600 -or
        $diskSample.DiskWriteBytesPerSec -ne 2097152 -or
        $diskSample.DiskReadsPerSec -ne 120 -or
        $diskSample.DiskWritesPerSec -ne 8 -or
        $diskSample.AvgDiskReadQueueLength -ne 3.25 -or
        $diskSample.AvgDiskWriteQueueLength -ne 0.25 -or
        $diskSample.CurrentDiskQueueLength -ne 4 -or
        $diskSample.PercentDiskTime -ne 98 -or
        -not $diskSample.ReadLatencyAvailable -or
        -not $diskSample.WriteLatencyAvailable -or
        $diskSample.AvgDiskSecPerRead -ne 0.012 -or
        $diskSample.AvgDiskSecPerWrite -ne 0.004 -or
        $diskSample.SplitIoPerSec -ne 1) {
        throw "物理盘读写拆分采样字段错误：$($diskSample | ConvertTo-Json -Compress)"
    }

    # 格式化 CIM 在本机可能把亚秒延迟截断为 0；必须显式记为不可用，不能伪装成零延迟。
    $unavailableLatencyDisk = New-PhysicalDiskSample -Row ([pscustomobject]@{
        Name = '2 I:'
        DiskReadBytesPersec = 104857600
        DiskWriteBytesPersec = 2097152
        DiskReadsPersec = 120
        DiskWritesPersec = 8
        AvgDiskQueueLength = 3.5
        AvgDiskReadQueueLength = 3.25
        AvgDiskWriteQueueLength = 0.25
        CurrentDiskQueueLength = 4
        PercentDiskTime = 98
        AvgDisksecPerRead = 0
        AvgDisksecPerWrite = 0
        SplitIOPerSec = 1
    })
    if ($unavailableLatencyDisk.ReadLatencyAvailable -or
        $unavailableLatencyDisk.WriteLatencyAvailable -or
        $null -ne $unavailableLatencyDisk.AvgDiskSecPerRead -or
        $null -ne $unavailableLatencyDisk.AvgDiskSecPerWrite) {
        throw "被 CIM 截断的磁盘延迟必须显式标记不可用：$($unavailableLatencyDisk | ConvertTo-Json -Compress)"
    }

    # 采样器实际 tick 会包含上一轮采集耗时，记录必须使用单调时钟差值而非配置的固定 2 秒。
    $systemRecord = New-SystemSampleRecord `
        -Utc ([DateTime]'2026-08-24T00:00:04Z') `
        -ElapsedMilliseconds 4017 `
        -PreviousSampleElapsedMilliseconds 2001 `
        -CollectionDurationMs 187 `
        -LogicalProcessorCount 24 `
        -Processes @($secondProcessSample) `
        -ProcessSampleSkips @([pscustomobject]@{
                process_id = 1002; reason = 'PROCESS_EXITED_DURING_SAMPLE'
            }) `
        -CpuCores @($processorSample) `
        -Disks @($diskSample)
    if ($systemRecord.elapsed_seconds -ne 4 -or
        $systemRecord.sample_interval_ms -ne 2016 -or
        $systemRecord.collection_duration_ms -ne 187 -or
        $systemRecord.logical_processor_count -ne 24 -or
        @($systemRecord.processes).Count -ne 1 -or
        @($systemRecord.process_sample_skips).Count -ne 1 -or
        [int]$systemRecord.process_sample_skips[0].process_id -ne 1002 -or
        [string]$systemRecord.process_sample_skips[0].reason -ne 'PROCESS_EXITED_DURING_SAMPLE' -or
        @($systemRecord.cpu_cores).Count -ne 1 -or
        @($systemRecord.disks).Count -ne 1) {
        throw "系统采样实际间隔记录错误：$($systemRecord | ConvertTo-Json -Depth 5 -Compress)"
    }

    # 用受控的完整性能计数器行运行真实写入边界，防止采集循环忘记接入新增字段。
    $samplePath = Join-Path $fixtureRoot 'system-sample.ndjson'
    $writePreviousCpu = @{
        '1001|2026-08-24T00:00:00.0000000Z' = 1000.0
        '999|2026-08-23T23:59:00.0000000Z' = 9000.0
    }
    $writePreviousIo = @{
        '1001|2026-08-24T00:00:00.0000000Z' = [pscustomobject]@{
            ReadOperationCount = 10.0
            WriteOperationCount = 4.0
            ReadTransferCount = 4096.0
            WriteTransferCount = 1024.0
            OtherTransferCount = 128.0
        }
        '999|2026-08-23T23:59:00.0000000Z' = [pscustomobject]@{
            ReadOperationCount = 90.0
            WriteOperationCount = 60.0
            ReadTransferCount = 94371840.0
            WriteTransferCount = 62914560.0
            OtherTransferCount = 4096.0
        }
    }
    Write-SystemSample -Path $samplePath -Root $fixtureRoot `
        -ElapsedMilliseconds 4017 -PreviousSampleElapsedMilliseconds 2001 `
        -PreviousCpu $writePreviousCpu -PreviousIo $writePreviousIo `
        -IsolatedProcessRows @($secondRow) `
        -ProcessResolver { param($processId) if ($processId -eq 1001) { $secondProcess } } `
        -ProcessorRows @([pscustomobject]@{
            Name = '3'; PercentProcessorTime = 91; PercentUserTime = 72
            PercentPrivilegedTime = 19; PercentInterruptTime = 2; PercentDPCTime = 3
        }) `
        -DiskRows @([pscustomobject]@{
            Name = '2 I:'; DiskReadBytesPersec = 104857600; DiskWriteBytesPersec = 2097152
            DiskReadsPersec = 120; DiskWritesPersec = 8; AvgDiskQueueLength = 3.5
            AvgDiskReadQueueLength = 3.25; AvgDiskWriteQueueLength = 0.25
            CurrentDiskQueueLength = 4; PercentDiskTime = 98
            AvgDisksecPerRead = 0.012; AvgDisksecPerWrite = 0.004; SplitIOPerSec = 1
        })
    $writtenSample = Get-Content -LiteralPath $samplePath -Raw | ConvertFrom-Json
    if ($writtenSample.sample_interval_ms -ne 2016 -or
        @($writtenSample.processes).Count -ne 1 -or
        $writtenSample.processes[0].CpuDeltaMs -ne 1500 -or
        @($writtenSample.cpu_cores).Count -ne 1 -or
        @($writtenSample.disks).Count -ne 1 -or
        $writtenSample.disks[0].DiskWriteBytesPerSec -ne 2097152 -or
        $writePreviousCpu.Count -ne 1 -or
        $writePreviousIo.Count -ne 1) {
        throw "系统采样写入边界未包含诊断字段：$($writtenSample | ConvertTo-Json -Depth 5 -Compress)"
    }

    function Assert-ExitedProcessSampling {
        <# 使用受控进程对象验证单个 Worker 退出不会中止整轮系统采样。 #>
        param(
            [Parameter(Mandatory)] [string] $Name,
            [Parameter(Mandatory)] $DepartedProcess,
            [object] $DepartedRow = $null
        )

        # 为退出 PID 预置多个世代，断言失败采样会清理该 PID 的全部 CPU/I/O 基线。
        $effectiveDepartedRow = if ($null -ne $DepartedRow) {
            $DepartedRow
        }
        else {
            [pscustomobject]@{
                ProcessId = 1002
                ReadOperationCount = 20
                WriteOperationCount = 8
                ReadTransferCount = 8192
                WriteTransferCount = 2048
                OtherTransferCount = 256
            }
        }
        $departedProcessId = [int]$effectiveDepartedRow.ProcessId
        $healthyRow = [pscustomobject]@{
            ProcessId = 1001
            ReadOperationCount = 16
            WriteOperationCount = 7
            ReadTransferCount = 16384
            WriteTransferCount = 4096
            OtherTransferCount = 768
        }
        $samplePath = Join-Path $fixtureRoot ("system-sample-exited-" + $Name + '.ndjson')
        $previousCpu = @{
            '1001|2026-08-24T00:00:00.0000000Z' = 1000.0
            "$departedProcessId|2026-08-25T23:59:00.0000000Z" = 9000.0
            "$departedProcessId|2026-08-26T00:00:00.0000000Z" = 10000.0
        }
        $previousIo = @{
            '1001|2026-08-24T00:00:00.0000000Z' = [pscustomobject]@{
                ReadOperationCount = 10.0; WriteOperationCount = 4.0
                ReadTransferCount = 4096.0; WriteTransferCount = 1024.0; OtherTransferCount = 128.0
            }
            "$departedProcessId|2026-08-25T23:59:00.0000000Z" = [pscustomobject]@{
                ReadOperationCount = 90.0; WriteOperationCount = 60.0
                ReadTransferCount = 94371840.0; WriteTransferCount = 62914560.0; OtherTransferCount = 4096.0
            }
            "$departedProcessId|2026-08-26T00:00:00.0000000Z" = [pscustomobject]@{
                ReadOperationCount = 100.0; WriteOperationCount = 70.0
                ReadTransferCount = 104857600.0; WriteTransferCount = 73400320.0; OtherTransferCount = 8192.0
            }
        }
        Write-SystemSample -Path $samplePath -Root $fixtureRoot `
            -ElapsedMilliseconds 6033 -PreviousSampleElapsedMilliseconds 4017 `
            -PreviousCpu $previousCpu -PreviousIo $previousIo `
            -IsolatedProcessRows @($healthyRow, $effectiveDepartedRow) `
            -ProcessResolver {
                param($processId)
                if ($processId -eq 1001) { return $secondProcess }
                if ($processId -eq $departedProcessId) { return $DepartedProcess }
            } `
            -ProcessorRows @([pscustomobject]@{
                Name = '3'; PercentProcessorTime = 91; PercentUserTime = 72
                PercentPrivilegedTime = 19; PercentInterruptTime = 2; PercentDPCTime = 3
            }) `
            -DiskRows @([pscustomobject]@{
                Name = '2 I:'; DiskReadBytesPersec = 104857600; DiskWriteBytesPersec = 2097152
                DiskReadsPersec = 120; DiskWritesPersec = 8; AvgDiskQueueLength = 3.5
                AvgDiskReadQueueLength = 3.25; AvgDiskWriteQueueLength = 0.25
                CurrentDiskQueueLength = 4; PercentDiskTime = 98
                AvgDisksecPerRead = 0.012; AvgDisksecPerWrite = 0.004; SplitIOPerSec = 1
            })
        $samples = @(Get-Content -LiteralPath $samplePath | ForEach-Object { $_ | ConvertFrom-Json })
        if (@($samples | Where-Object record_type -eq 'system_sample').Count -ne 1) {
            throw "退出 Worker 后必须仍写入一条 system_sample：$Name"
        }
        $sample = $samples[0]
        if (@($sample.processes).Count -ne 1 -or [int]$sample.processes[0].ProcessId -ne 1001 -or
            @($sample.processes | Where-Object {
                    [int]$_.ProcessId -eq 0 -or [string]::IsNullOrWhiteSpace([string]$_.Name)
                }).Count -ne 0 -or
            @($sample.process_sample_skips).Count -ne 1 -or
            [int]$sample.process_sample_skips[0].process_id -ne $departedProcessId -or
            [string]$sample.process_sample_skips[0].reason -ne 'PROCESS_EXITED_DURING_SAMPLE' -or
            @($sample.cpu_cores).Count -ne 1 -or @($sample.disks).Count -ne 1 -or
            @($previousCpu.Keys | Where-Object { ([string]$_).StartsWith("$departedProcessId|") }).Count -ne 0 -or
            @($previousIo.Keys | Where-Object { ([string]$_).StartsWith("$departedProcessId|") }).Count -ne 0) {
            throw "退出 Worker 采样必须跳过该 PID、保留健康指标并清理基线：$($sample | ConvertTo-Json -Depth 6 -Compress)"
        }
    }

    # null getter 模拟进程在 Get-Process 成功后、CPU 计数读取前退出。
    $departedWithNullGetter = [pscustomobject]@{
        Id = 1002
        ProcessName = 'worker'
        StartTime = [DateTime]'2026-08-26T00:00:00Z'
        TotalProcessorTime = $null
        WorkingSet64 = $null
        PrivateMemorySize64 = $null
    }
    Assert-ExitedProcessSampling -Name 'null-getter' -DepartedProcess $departedWithNullGetter

    # ScriptProperty 抛错模拟 Process API 属性访问本身因退出失败，不能用时间竞态夹具替代。
    $departedWithThrowingGetter = [pscustomobject]@{
        Id = 1002
        ProcessName = 'worker'
        StartTime = [DateTime]'2026-08-26T00:00:00Z'
        WorkingSet64 = 0
        PrivateMemorySize64 = 0
    }
    Add-Member -InputObject $departedWithThrowingGetter -MemberType ScriptProperty `
        -Name TotalProcessorTime -Value { throw 'fixture TotalProcessorTime getter failure' }
    Assert-ExitedProcessSampling -Name 'throwing-getter' -DepartedProcess $departedWithThrowingGetter

    # StartTime 访问失败且 CIM 行没有有效 CreationDate 时，不能退化为 unknown 世代继续采样。
    $departedWithThrowingStartTime = [pscustomobject]@{
        Id = 1002
        ProcessName = 'worker'
        TotalProcessorTime = [TimeSpan]::FromMilliseconds(3000)
        WorkingSet64 = 1048576
        PrivateMemorySize64 = 524288
    }
    Add-Member -InputObject $departedWithThrowingStartTime -MemberType ScriptProperty `
        -Name StartTime -Value { throw 'fixture StartTime getter failure' }
    Assert-ExitedProcessSampling -Name 'throwing-start-time' -DepartedProcess $departedWithThrowingStartTime

    # CPU 正常但内存快照缺失同样表示进程已不可完整采样，不能把 null 误投影为零。
    $departedWithNullWorkingSet = [pscustomobject]@{
        Id = 1002
        ProcessName = 'worker'
        StartTime = [DateTime]'2026-08-26T00:00:00Z'
        TotalProcessorTime = [TimeSpan]::FromMilliseconds(3000)
        WorkingSet64 = $null
        PrivateMemorySize64 = 524288
    }
    Assert-ExitedProcessSampling -Name 'null-working-set' -DepartedProcess $departedWithNullWorkingSet

    $departedWithNullPrivateMemory = [pscustomobject]@{
        Id = 1002
        ProcessName = 'worker'
        StartTime = [DateTime]'2026-08-26T00:00:00Z'
        TotalProcessorTime = [TimeSpan]::FromMilliseconds(3000)
        WorkingSet64 = 1048576
        PrivateMemorySize64 = $null
    }
    Assert-ExitedProcessSampling -Name 'null-private-memory' -DepartedProcess $departedWithNullPrivateMemory

    # 必要的进程 ID 为空时不能转换成伪 PID 0，必须按 Row.ProcessId 记录退出 skip。
    $departedWithNullId = [pscustomobject]@{
        Id = $null
        ProcessName = 'worker'
        StartTime = [DateTime]'2026-08-26T00:00:00Z'
        TotalProcessorTime = [TimeSpan]::FromMilliseconds(3000)
        WorkingSet64 = 1048576
        PrivateMemorySize64 = 524288
    }
    Assert-ExitedProcessSampling -Name 'null-id' -DepartedProcess $departedWithNullId

    # 必要的进程名为空或空白时不能产生无归属样本。
    $departedWithNullName = [pscustomobject]@{
        Id = 1002
        ProcessName = $null
        StartTime = [DateTime]'2026-08-26T00:00:00Z'
        TotalProcessorTime = [TimeSpan]::FromMilliseconds(3000)
        WorkingSet64 = 1048576
        PrivateMemorySize64 = 524288
    }
    Assert-ExitedProcessSampling -Name 'null-name' -DepartedProcess $departedWithNullName

    $departedWithBlankName = [pscustomobject]@{
        Id = 1002
        ProcessName = '   '
        StartTime = [DateTime]'2026-08-26T00:00:00Z'
        TotalProcessorTime = [TimeSpan]::FromMilliseconds(3000)
        WorkingSet64 = 1048576
        PrivateMemorySize64 = 524288
    }
    Assert-ExitedProcessSampling -Name 'blank-name' -DepartedProcess $departedWithBlankName

    # 任一必需 CIM I/O 累计字段为空都不能按 0 继续计算差分。
    $departedWithNullIo = [pscustomobject]@{
        Id = 1002
        ProcessName = 'worker'
        StartTime = [DateTime]'2026-08-26T00:00:00Z'
        TotalProcessorTime = [TimeSpan]::FromMilliseconds(3000)
        WorkingSet64 = 1048576
        PrivateMemorySize64 = 524288
    }
    $nullIoRow = [pscustomobject]@{
        ProcessId = 1002
        ReadOperationCount = $null
        WriteOperationCount = 8
        ReadTransferCount = 8192
        WriteTransferCount = 2048
        OtherTransferCount = 256
    }
    Assert-ExitedProcessSampling -Name 'null-io' -DepartedProcess $departedWithNullIo -DepartedRow $nullIoRow

    $before = Get-RuntimeMediaManifest -MediaRoot $media
    $same = Get-RuntimeMediaManifest -MediaRoot $media
    Assert-RuntimeMediaUnchanged -Before $before -After $same

    [IO.File]::WriteAllText((Join-Path $media 'added.bin'), 'new')
    $changed = Get-RuntimeMediaManifest -MediaRoot $media
    $detected = $false
    try {
        Assert-RuntimeMediaUnchanged -Before $before -After $changed
    }
    catch {
        $detected = $_.Exception.Message -match 'RUST_V2_REAL_MEDIA_CHANGED'
    }
    if (-not $detected) {
        throw '新增媒体文件必须被清单比较检测'
    }

    $serialized = $before | ConvertTo-Json -Depth 8 -Compress
    if ($serialized -match '(?i)password|postgresql://') {
        throw '媒体清单不得泄露PostgreSQL密码'
    }

    # Task 16 RED：多根输入必须真实解析、保持顺序，并生成组合 v2 清单；旧实现没有多根接口。
    $mediaRootA = Join-Path $fixtureRoot 'media-root-a'
    $mediaRootB = Join-Path $fixtureRoot 'media-root-b'
    New-Item -ItemType Directory -Path $mediaRootA, $mediaRootB -Force | Out-Null
    [IO.File]::WriteAllText((Join-Path $mediaRootA 'a.mp4'), 'root a')
    [IO.File]::WriteAllText((Join-Path $mediaRootB 'b.mp4'), 'root b')
    $mediaRoots = @($mediaRootA, $mediaRootB)

    # Task 16 review RED：只有盘符加反斜杠才是 fully-qualified；C:relative 和 \relative 都必须拒绝。
    $driveRelativeValidation = Assert-RuntimeAcceptanceInputs `
        -MediaRoot '' -MediaRoots @('C:relative', $mediaRootB) -DurationSeconds 1800 -SampleSeconds 2 -ReleaseRoot $release `
        -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
        -EvidenceRoot (Join-Path $fixtureRoot 'runs\drive-relative-A-1\evidence') `
        -ReportPath (Join-Path $fixtureRoot 'runs\drive-relative-A-1\evidence\report.md') `
        -Variant A -RunIndex 1 -ThrowOnError:$false
    if ($driveRelativeValidation.Valid -or $driveRelativeValidation.Code -ne 'RUST_V2_REAL_MEDIA_ROOT_NOT_ABSOLUTE') {
        throw "C:relative 必须被 fully-qualified 校验拒绝：$($driveRelativeValidation | ConvertTo-Json -Compress)"
    }
    $rootRelativePath = ([string][char]92) + 'relative'
    $rootRelativeValidation = Assert-RuntimeAcceptanceInputs `
        -MediaRoot '' -MediaRoots @($rootRelativePath, $mediaRootB) -DurationSeconds 1800 -SampleSeconds 2 -ReleaseRoot $release `
        -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
        -EvidenceRoot (Join-Path $fixtureRoot 'runs\root-relative-A-1\evidence') `
        -ReportPath (Join-Path $fixtureRoot 'runs\root-relative-A-1\evidence\report.md') `
        -Variant A -RunIndex 1 -ThrowOnError:$false
    if ($rootRelativeValidation.Valid -or $rootRelativeValidation.Code -ne 'RUST_V2_REAL_MEDIA_ROOT_NOT_ABSOLUTE') {
        throw "\relative 必须被 fully-qualified 校验拒绝：$($rootRelativeValidation | ConvertTo-Json -Compress)"
    }

    $multiValidation = Assert-RuntimeAcceptanceInputs `
        -MediaRoot '' -MediaRoots $mediaRoots -DurationSeconds 1800 -SampleSeconds 2 -ReleaseRoot $release `
        -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
        -EvidenceRoot (Join-Path $fixtureRoot 'runs\multi-A-1\evidence') `
        -ReportPath (Join-Path $fixtureRoot 'runs\multi-A-1\evidence\report.md') `
        -Variant A -RunIndex 1 -SingleRun -ThrowOnError:$false
    if (-not $multiValidation.Valid) {
        throw "多根普通目录必须通过输入校验：$($multiValidation | ConvertTo-Json -Compress)"
    }
    $multiManifest = Get-RuntimeMediaManifest -MediaRoots $mediaRoots
    if ($multiManifest.Schema -cne 'rust-v2-media-manifest/v2' -or
        @($multiManifest.Roots).Count -ne 2 -or
        [string]$multiManifest.Roots[0] -cne (Get-Item -LiteralPath $mediaRootA).FullName -or
        [string]$multiManifest.Roots[1] -cne (Get-Item -LiteralPath $mediaRootB).FullName -or
        [int]$multiManifest.FileCount -ne 2 -or
        @($multiManifest.Files).Count -ne 2 -or
        [int]$multiManifest.Files[0].RootIndex -ne 1 -or
        [int]$multiManifest.Files[1].RootIndex -ne 2) {
        throw "多根组合清单必须是有序 canonical v2：$($multiManifest | ConvertTo-Json -Depth 10 -Compress)"
    }
    $legacyManifest = Get-RuntimeMediaManifest -MediaRoot $media
    if ($legacyManifest.PSObject.Properties['Schema'] -or
        $legacyManifest.PSObject.Properties['Roots']) {
        throw '旧单根 media manifest 不得漂移到 v2 schema'
    }
    $multiManifestCopy = $multiManifest | ConvertTo-Json -Depth 10 | ConvertFrom-Json
    Assert-RuntimeMediaUnchanged -Before $multiManifest -After $multiManifestCopy

    # 多根输入的稳定拒绝路径：重复、互相包含和根自身 ReparsePoint 都不能启动 Node。
    $duplicateValidation = Assert-RuntimeAcceptanceInputs `
        -MediaRoot '' -MediaRoots @($mediaRootA, $mediaRootA) -DurationSeconds 1800 -SampleSeconds 2 `
        -ReleaseRoot $release -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
        -EvidenceRoot (Join-Path $fixtureRoot 'runs\duplicate-A-1\evidence') `
        -ReportPath (Join-Path $fixtureRoot 'runs\duplicate-A-1\evidence\report.md') `
        -Variant A -RunIndex 1 -ThrowOnError:$false
    if ($duplicateValidation.Valid -or $duplicateValidation.Code -ne 'RUST_V2_REAL_MEDIA_ROOTS_DUPLICATE') {
        throw "重复媒体根必须稳定拒绝：$($duplicateValidation | ConvertTo-Json -Compress)"
    }
    $nestedRoot = Join-Path $mediaRootA 'nested'
    New-Item -ItemType Directory -Path $nestedRoot -Force | Out-Null
    $nestedValidation = Assert-RuntimeAcceptanceInputs `
        -MediaRoot '' -MediaRoots @($mediaRootA, $nestedRoot) -DurationSeconds 1800 -SampleSeconds 2 `
        -ReleaseRoot $release -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
        -EvidenceRoot (Join-Path $fixtureRoot 'runs\nested-A-1\evidence') `
        -ReportPath (Join-Path $fixtureRoot 'runs\nested-A-1\evidence\report.md') `
        -Variant A -RunIndex 1 -ThrowOnError:$false
    if ($nestedValidation.Valid -or $nestedValidation.Code -ne 'RUST_V2_REAL_MEDIA_ROOTS_NESTED') {
        throw "互相包含的媒体根必须稳定拒绝：$($nestedValidation | ConvertTo-Json -Compress)"
    }
    $reparseRoot = Join-Path $fixtureRoot 'media-reparse-root'
    $reparseTarget = Join-Path $fixtureRoot 'media-reparse-target'
    New-Item -ItemType Directory -Path $reparseTarget -Force | Out-Null
    # 创建仅位于临时 fixture 的 junction，真实触发 Windows ReparsePoint 属性，不触碰媒体盘。
    New-Item -ItemType Junction -Path $reparseRoot -Target $reparseTarget -ErrorAction Stop | Out-Null
    $reparseValidation = Assert-RuntimeAcceptanceInputs `
        -MediaRoot '' -MediaRoots @($reparseRoot, $mediaRootB) -DurationSeconds 1800 -SampleSeconds 2 `
        -ReleaseRoot $release -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
        -EvidenceRoot (Join-Path $fixtureRoot 'runs\reparse-A-1\evidence') `
        -ReportPath (Join-Path $fixtureRoot 'runs\reparse-A-1\evidence\report.md') `
        -Variant A -RunIndex 1 -ThrowOnError:$false
    if ($reparseValidation.Valid -or $reparseValidation.Code -ne 'RUST_V2_REAL_MEDIA_ROOT_REPARSE_POINT') {
        throw "ReparsePoint 媒体根必须稳定拒绝：$($reparseValidation | ConvertTo-Json -Compress)"
    }

    # Windows Storage API seam：只替换 Get-Partition/Get-Disk 边界，验证不同/相同 DiskNumber 判定。
    $partitionRows = @{
        H = [pscustomobject]@{ PartitionNumber = 4; DiskNumber = 10 }
        I = [pscustomobject]@{ PartitionNumber = 8; DiskNumber = 11 }
    }
    $diskRows = @{
        10 = [pscustomobject]@{ Number = 10; FriendlyName = 'Fixture HDD'; BusType = 'SATA' }
        11 = [pscustomobject]@{ Number = 11; FriendlyName = 'Fixture SSD'; BusType = 'NVMe' }
    }
    $distinctDiskMap = Get-RuntimePhysicalDiskMap -MediaRoots @('H:\pik\00000000000', 'I:\tmp') `
        -RequireDistinctPhysicalDisks `
        -PartitionResolver { param($DriveLetter) $partitionRows[$DriveLetter] } `
        -DiskResolver { param($DiskNumber) $diskRows[[int]$DiskNumber] }
    if (@($distinctDiskMap.entries).Count -ne 2 -or
        @($distinctDiskMap.distinct_disk_numbers).Count -ne 2 -or
        [int]$distinctDiskMap.entries[0].disk_number -ne 10 -or
        [int]$distinctDiskMap.entries[1].disk_number -ne 11) {
        throw "不同物理盘映射必须保留完整绑定：$($distinctDiskMap | ConvertTo-Json -Depth 10 -Compress)"
    }
    $sameDiskCode = ''
    $sameDiskPartition = [pscustomobject]@{ PartitionNumber = 9; DiskNumber = 10 }
    try {
        Get-RuntimePhysicalDiskMap -MediaRoots @('H:\pik\00000000000', 'I:\tmp') `
            -RequireDistinctPhysicalDisks `
            -PartitionResolver { param($DriveLetter) $sameDiskPartition } `
            -DiskResolver { param($DiskNumber) $diskRows[[int]$DiskNumber] } | Out-Null
    }
    catch {
        $sameDiskCode = $_.Exception.Message
    }
    if ($sameDiskCode -notlike '*RUST_V2_ACCEPTANCE_PHYSICAL_DISK_NOT_DISTINCT*') {
        throw "相同 DiskNumber 必须在 Node 启动前拒绝：$sameDiskCode"
    }

    # harness-result 的新增绑定字段必须真实输出，避免双盘证据与媒体清单脱节。
    $fixtureSummary = [pscustomobject]@{
        Path = Join-Path $fixtureRoot 'result-summary.tsv'; Sha256 = ('1' * 64); Status = 'PASS'; TaskId = $null
    }
    $fixtureHarnessArguments = @{
        Variant = 'A'; RunIndex = [int]1; SourceRevision = 'fixture';
        SourceTreeSha256 = ('2' * 64); PackagePath = (Join-Path $fixtureRoot 'fixture.zip');
        PackageSha256 = ('3' * 64); ReleaseRoot = $release; DatabaseSnapshotRoot = '';
        DatabaseSnapshotPath = ''; DatabaseSnapshotMetadataPath = ''; ConfigSha256 = ('4' * 64);
        PackageManifestSha256 = ('5' * 64); MediaBeforeSha256 = ('6' * 64); MediaAfterSha256 = ('6' * 64);
        ResultSummary = $fixtureSummary; ResultSummaryStatus = 'PASS'; ResultSummaryPath = $fixtureSummary.Path;
        ResultSummarySha256 = $fixtureSummary.Sha256; ResultSummaryTaskId = $null;
        MediaRoots = [string[]]$mediaRoots; SingleRun = $true;
        PhysicalDiskMapPath = (Join-Path $fixtureRoot 'physical-disk-map.json'); PhysicalDiskMapSha256 = ('7' * 64);
        MediaBeforeRootPaths = [string[]]@('before-01.json', 'before-02.json');
        MediaAfterRootPaths = [string[]]@('after-01.json', 'after-02.json');
        MediaBeforeRootSha256 = [string[]]@([string]::new('8', 64), [string]::new('9', 64)); MediaAfterRootSha256 = [string[]]@([string]::new('a', 64), [string]::new('b', 64))
    }
    $fixtureHarness = New-HarnessResult @fixtureHarnessArguments
    if (@($fixtureHarness.media_roots).Count -ne 2 -or $fixtureHarness.single_run -ne $true -or
        $null -ne $fixtureHarness.result_summary_task_id -or
        [string]::IsNullOrWhiteSpace([string]$fixtureHarness.physical_disk_map_path) -or
        @($fixtureHarness.media_before_root_paths).Count -ne 2 -or
        @($fixtureHarness.media_after_root_paths).Count -ne 2) {
        throw "harness-result 必须绑定多根、单轮、物理盘和分根清单：$($fixtureHarness | ConvertTo-Json -Depth 10 -Compress)"
    }

    # 受控 runner 真正穿过 Invoke-RustV2RuntimeAcceptance 的 finalization 参数绑定；
    # runner 进程、Node、exporter 和摘要依赖均为 fixture，避免启动真实服务但保留真实调用边界。
    $finalizationFunctionNames = @(
        'Resolve-ReleaseRoot', 'Assert-RuntimeAcceptanceInputs', 'Test-IsAdministrator',
        'Get-FreeTcpPort', 'Wait-TcpEndpoint', 'Start-Process',
        'Get-CompletedProcessExitCode', 'Request-IsolatedNodeExit', 'Get-LastRuntimeResult',
        'Invoke-ResultExporter', 'Parse-ResultSummaryOutput', 'Get-ResultSummaryArtifacts',
        'Get-RuntimeMediaManifest', 'Assert-RuntimeMediaUnchanged', 'New-HarnessResult',
        'Start-RuntimeAcceptanceSupervisor', 'Get-RuntimeAcceptanceSupervisorStatus',
        'Stop-RuntimeAcceptanceSupervisor')
    $savedFinalizationFunctions = @{}
    foreach ($functionName in $finalizationFunctionNames) {
        $functionCommand = Get-Command $functionName -CommandType Function -ErrorAction SilentlyContinue
        $savedFinalizationFunctions[$functionName] = if ($functionCommand) { $functionCommand.ScriptBlock } else { $null }
    }
        $script:finalizationDeadlineTaskId = $null
        $script:finalizationEvidence = $null
        $script:finalizationFailedAfterCancel = $false
        $script:finalizationObservedRootsJson = $null
        $script:finalizationObservedSingleRun = $null
        $originalNewHarnessResult = $savedFinalizationFunctions['New-HarnessResult']
    try {
        function Resolve-ReleaseRoot {
            param([string] $CargoTargetDir, [string] $ReleaseRoot)
            $script:finalizationReleaseRoot
        }
        function Assert-RuntimeAcceptanceInputs {
            param(
                [string] $MediaRoot, [string[]] $MediaRoots = @(), [int] $DurationSeconds, [int] $SampleSeconds,
                [string] $ReleaseRoot, [string] $AcceptanceClientPath, [string] $ResultExporterPath,
                [string] $EvidenceRoot, [string] $ReportPath, [string] $Variant = 'A',
                [int] $RunIndex = 1, [int] $WorkerCount = 20, [int] $HddThreadsPerDisk = 1,
                [int] $SsdThreadsPerDisk = 16, [int] $UnknownThreadsPerDisk = 1,
                [int] $TotalReadThreads = 12, [int] $ReservedCores = 1, [string] $Enumerator = 'everything',
                [switch] $SingleRun, [switch] $CompleteWhenTaskTerminal,
                [switch] $RequireDistinctPhysicalDisks, [switch] $ThrowOnError = $true)
            [pscustomobject]@{ Valid = $true; Code = '' }
        }
        function Test-IsAdministrator { $true }
        function Get-FreeTcpPort { 39125 }
        function Wait-TcpEndpoint {
            param([int] $Port, $Process, [int] $TimeoutSeconds = 60)
        }
        function Start-Process {
            param(
                [string] $FilePath, [string] $WorkingDirectory, [switch] $PassThru,
                [string] $WindowStyle, [string] $RedirectStandardOutput, [string] $RedirectStandardError)
            if ([IO.Path]::GetFileName($FilePath) -ieq 'runtime_acceptance.exe') {
                # 真实观察客户端启动边界，确认多根顺序和终态结束标记来自当前调用而非宿主残留。
                $script:finalizationObservedRootsJson = [Environment]::GetEnvironmentVariable(
                    'RUST_V2_REAL_MEDIA_ROOTS_JSON', 'Process')
                $script:finalizationObservedSingleRun = [Environment]::GetEnvironmentVariable(
                    'RUST_V2_ACCEPTANCE_SINGLE_RUN', 'Process')
            }
            if ($script:finalizationEvidence) {
                [IO.File]::WriteAllText((Join-Path $script:finalizationEvidence 'runtime.ndjson'), "{}`n")
                [IO.File]::WriteAllText((Join-Path $script:finalizationEvidence 'system.ndjson'), "{}`n")
            }
            $fixtureDatabase = Join-Path $WorkingDirectory 'data\node\node.db'
            New-Item -ItemType Directory -Path (Split-Path -Parent $fixtureDatabase) -Force | Out-Null
            if (-not (Test-Path -LiteralPath $fixtureDatabase -PathType Leaf)) {
                [IO.File]::WriteAllText($fixtureDatabase, 'fixture database')
            }
            [IO.File]::WriteAllText("$fixtureDatabase-wal", 'fixture wal')
            [IO.File]::WriteAllText("$fixtureDatabase-shm", 'fixture shm')
            [pscustomobject]@{
                HasExited = $true; Id = 4242; ExitCode = 0
                Path = $FilePath; StartTime = [DateTime]::UtcNow
            }
        }
        function Start-RuntimeAcceptanceSupervisor {
            param(
                [int] $ClientId, [string] $ClientPath, [string] $ClientStartTimeUtc,
                [int] $NodeId, [string] $NodePath, [string] $NodeStartTimeUtc,
                [DateTime] $DeadlineUtc, [string] $StatusPath)
            [pscustomobject]@{ Id = 4243; Process = $null; StatusPath = $StatusPath }
        }
        function Get-RuntimeAcceptanceSupervisorStatus {
            param([Parameter(Mandatory)] $Supervisor)
            [pscustomobject]@{ TimedOut = $false; Diagnostic = '' }
        }
        function Stop-RuntimeAcceptanceSupervisor {
            param([Parameter(Mandatory)] $Supervisor)
            [pscustomobject]@{ ExitConfirmed = $true; Diagnostic = '' }
        }
        function Get-CompletedProcessExitCode {
            param([Parameter(Mandatory)] $Process)
            0
        }
        function Request-IsolatedNodeExit {
            param([Parameter(Mandatory)] $Node, [Parameter(Mandatory)] [string] $Root, [int] $TimeoutSeconds = 20)
            ''
        }
        function Get-LastRuntimeResult {
            param([Parameter(Mandatory)] [string] $Path)
            # 失败终态故意复用 deadline ID，验证失败永远不能享受取消豁免。
            if ($script:finalizationFailedAfterCancel) {
                return [pscustomobject]@{
                    latest_completed_persistent_task_id = 'task-completed'
                    deadline_cancelled_persistent_task_id = 'task-deadline'
                    scan_tasks = @([pscustomobject]@{
                            persistent_task_id = 'task-deadline'
                            terminal_state = 'failed'
                        })
                    failed_scans = 0
                }
            }
            [pscustomobject]@{
                latest_completed_persistent_task_id = 'task-completed'
                deadline_cancelled_persistent_task_id = 'task-deadline'
                scan_tasks = @([pscustomobject]@{
                        persistent_task_id = 'task-completed'
                        terminal_state = 'completed'
                    })
                failed_scans = 0
            }
        }
        function Invoke-ResultExporter {
            param(
                [Parameter(Mandatory)] [string] $ExporterPath,
                [Parameter(Mandatory)] [string] $DatabasePath,
                [Parameter(Mandatory)] [string] $CacheRoot,
                [string[]] $MediaRoots = @(),
                [Parameter(Mandatory)] [string] $OutputPath,
                [Parameter(Mandatory)] [string] $EvidenceRoot,
                [int] $TimeoutSeconds = 120, [scriptblock] $ProcessKiller, [scriptblock] $ProcessWaiter)
            [pscustomobject]@{ ExitCode = 0; Diagnostic = ''; Stdout = 'fixture exporter' }
        }
        function Parse-ResultSummaryOutput {
            param(
                [Parameter(Mandatory)] [string] $Text,
                [Parameter(Mandatory)] [string] $ExpectedPath,
                [string] $ExpectedTaskId = '')
            [pscustomobject]@{
                Status = 'PASS'; Path = $ExpectedPath; Sha256 = ('b' * 64)
                RowCount = 1; MissingCount = 0; InconclusiveCount = 0; TaskId = $ExpectedTaskId
            }
        }
        function Get-ResultSummaryArtifacts {
            param(
                [Parameter(Mandatory)] [string] $SummaryPath,
                [string] $ExpectedTaskId = '', [string] $ExpectedStatus = '',
                [string] $ExpectedSha256 = '', [long] $ExpectedRowCount = -1)
            [pscustomobject]@{ BindingValid = $true; Diagnostic = '' }
        }
        function Get-RuntimeMediaManifest {
            param([string] $MediaRoot = '', [string[]] $MediaRoots = @())
            $roots = if (@($MediaRoots).Count -gt 0) {
                @($MediaRoots | ForEach-Object { (Get-Item -LiteralPath $_).FullName })
            }
            else {
                @((Get-Item -LiteralPath $MediaRoot).FullName)
            }
            if (@($roots).Count -gt 1) {
                $files = @(
                    for ($rootIndex = 0; $rootIndex -lt @($roots).Count; $rootIndex++) {
                        [pscustomobject]@{
                            RootIndex = $rootIndex + 1
                            Root = [string]$roots[$rootIndex]
                            Path = 'fixture.bin'
                            Length = 1
                            LastWriteTimeUtc = '2026-08-24T00:00:00.0000000Z'
                        }
                    }
                )
                return [pscustomobject]@{
                    Schema = 'rust-v2-media-manifest/v2'
                    Roots = [string[]]$roots
                    FileCount = @($files).Count
                    TotalBytes = @($files | Measure-Object -Property Length -Sum).Sum
                    Files = $files
                }
            }
            [pscustomobject]@{
                Root = [string]$roots[0]
                FileCount = 1
                TotalBytes = 1
                Files = @([pscustomobject]@{ Path = 'fixture.bin'; Length = 1; LastWriteTimeUtc = '2026-08-24T00:00:00.0000000Z' })
            }
        }
        function Assert-RuntimeMediaUnchanged {
            param([Parameter(Mandatory)] $Before, [Parameter(Mandatory)] $After)
        }
        function New-HarnessResult {
            param(
                [string] $Variant, [int] $RunIndex, [string] $SourceRevision,
                [string] $SourceTreeSha256, [string] $PackagePath, [string] $PackageSha256,
                [string] $ReleaseRoot, [string] $DatabaseSnapshotRoot, [string] $DatabaseSnapshotPath,
                [string] $DatabaseSnapshotMetadataPath, [string] $ConfigSha256, [string] $PackageManifestSha256,
                [string] $MediaBeforeSha256, [string] $MediaAfterSha256,
                [string[]] $MediaRoots = @(), [switch] $SingleRun,
                [string] $PhysicalDiskMapPath = '', [string] $PhysicalDiskMapSha256 = '',
                [string[]] $MediaBeforeRootPaths = @(), [string[]] $MediaAfterRootPaths = @(),
                [string[]] $MediaBeforeRootSha256 = @(), [string[]] $MediaAfterRootSha256 = @(),
                [Parameter(Mandatory)] $ResultSummary, [string] $ResultSummaryStatus,
                [string] $ResultSummaryPath, [string] $ResultSummarySha256,
                [string] $ResultSummaryTaskId, [long] $ResultSummaryMissingCount = 0,
                [long] $ResultSummaryInconclusiveCount = 0, [long] $ResultSummaryRowCount = 0,
                [string] $RunStatus = 'INCONCLUSIVE', [string] $RunDiagnostic = '', [bool] $MediaUnchanged = $false,
                [bool] $NodeUnexpectedExit = $false, [int] $ExporterExitCode = -1,
                [string] $DeadlineCancelledPersistentTaskId = '', [int] $EffectiveWorkerCount = 0,
                [int] $HddThreadsPerDisk = 0, [int] $SsdThreadsPerDisk = 0,
                [int] $UnknownThreadsPerDisk = 0, [int] $ReadTotalThreads = 0,
                [int] $ReservedCores = 0, [int] $ContactSheetReuseCount = 0,
                [int] $DiskFullCleanupCount = 0)
            $script:finalizationDeadlineTaskId = $DeadlineCancelledPersistentTaskId
            & $script:originalNewHarnessResult @PSBoundParameters
        }
        $script:finalizationReleaseRoot = $release
        $finalizationEvidence = Join-Path $fixtureRoot 'runs\finalization-A-1\evidence'
        $finalizationReport = Join-Path $finalizationEvidence 'report.md'
        $script:finalizationEvidence = $finalizationEvidence
        $originalMediaRootsEnvironment = [Environment]::GetEnvironmentVariable('RUST_V2_REAL_MEDIA_ROOTS_JSON', 'Process')
        $originalSingleRunEnvironment = [Environment]::GetEnvironmentVariable('RUST_V2_ACCEPTANCE_SINGLE_RUN', 'Process')
        $hostMediaRootsEnvironment = 'host-roots-sentinel'
        $hostSingleRunEnvironment = 'host-single-run-sentinel'
        [Environment]::SetEnvironmentVariable('RUST_V2_REAL_MEDIA_ROOTS_JSON', $hostMediaRootsEnvironment, 'Process')
        [Environment]::SetEnvironmentVariable('RUST_V2_ACCEPTANCE_SINGLE_RUN', $hostSingleRunEnvironment, 'Process')
        $expectedMediaRootsJson = ConvertTo-Json -InputObject ([string[]]@(
                (Get-Item -LiteralPath $mediaRootA).FullName,
                (Get-Item -LiteralPath $mediaRootB).FullName)) -Compress
        $finalizationResult = @(Invoke-RustV2RuntimeAcceptance `
            -MediaRoot '' -MediaRoots @($mediaRootA, $mediaRootB) -DurationSeconds 1800 -SampleSeconds 2 `
            -CargoTargetDir $fixtureRoot -ReleaseRoot $release `
            -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
            -EvidenceRoot $finalizationEvidence -ReportPath $finalizationReport `
            -Variant A -RunIndex 1 -SourceRevision 'fixture' -SourceTreeSha256 ('a' * 64) `
            -PackagePath (Join-Path $fixtureRoot 'fixture.zip') -PackageSha256 ('b' * 64) `
            -WorkerCount 20 -HddThreadsPerDisk 1 -SsdThreadsPerDisk 16 `
            -UnknownThreadsPerDisk 1 -TotalReadThreads 12 -ReservedCores 1 -CompleteWhenTaskTerminal)
        if ($script:finalizationObservedRootsJson -cne $expectedMediaRootsJson -or
            $script:finalizationObservedSingleRun -cne '1') {
            throw "受控客户端必须观察到压缩有序多根和 CompleteWhenTaskTerminal=1：roots=$script:finalizationObservedRootsJson single=$script:finalizationObservedSingleRun expected=$expectedMediaRootsJson"
        }
        if ([Environment]::GetEnvironmentVariable('RUST_V2_REAL_MEDIA_ROOTS_JSON', 'Process') -cne $hostMediaRootsEnvironment -or
            [Environment]::GetEnvironmentVariable('RUST_V2_ACCEPTANCE_SINGLE_RUN', 'Process') -cne $hostSingleRunEnvironment) {
            throw 'Invoke-RustV2RuntimeAcceptance 返回后必须恢复宿主环境变量'
        }
        if ($script:finalizationDeadlineTaskId -cne 'task-deadline') {
            throw "finalization 必须复用 deadline task ID，实际=$script:finalizationDeadlineTaskId"
        }
        if (-not ($finalizationResult -contains 'RUST_V2_RUNTIME_ACCEPTANCE_MEASURE_PASS')) {
            throw "受控 finalization runner 未成功完成：$($finalizationResult -join '|')"
        }
        $finalizationHarnessPath = Join-Path $finalizationEvidence 'harness-result.json'
        $finalizationHarness = [IO.File]::ReadAllText($finalizationHarnessPath) | ConvertFrom-Json
        $beforeRootPaths = @($finalizationHarness.media_before_root_paths)
        $beforeRootShas = @($finalizationHarness.media_before_root_sha256)
        $afterRootPaths = @($finalizationHarness.media_after_root_paths)
        $afterRootShas = @($finalizationHarness.media_after_root_sha256)
        if ($beforeRootPaths.Count -ne 2 -or $beforeRootShas.Count -ne 2 -or
            $afterRootPaths.Count -ne 2 -or $afterRootShas.Count -ne 2) {
            throw "分根 evidence 数组必须完整贯通：beforePaths=$($beforeRootPaths.Count) beforeSha=$($beforeRootShas.Count) afterPaths=$($afterRootPaths.Count) afterSha=$($afterRootShas.Count)"
        }
        for ($rootIndex = 0; $rootIndex -lt 2; $rootIndex++) {
            $expectedBeforeSha = (Get-FileHash -LiteralPath $beforeRootPaths[$rootIndex] -Algorithm SHA256).Hash.ToLowerInvariant()
            $expectedAfterSha = (Get-FileHash -LiteralPath $afterRootPaths[$rootIndex] -Algorithm SHA256).Hash.ToLowerInvariant()
            if ([string]$beforeRootShas[$rootIndex] -cne $expectedBeforeSha -or
                [string]$afterRootShas[$rootIndex] -cne $expectedAfterSha) {
                throw "RED: harness 分根 SHA 必须绑定 JSON 文件字节：root=$($rootIndex + 1) before=$($beforeRootShas[$rootIndex])/$expectedBeforeSha after=$($afterRootShas[$rootIndex])/$expectedAfterSha"
            }
        }

        # RED：同一 deadline ID 的 failed 终态也必须被单轮 Measure 判为 FAIL。
        $script:finalizationFailedAfterCancel = $true
        $failedFinalizationEvidence = Join-Path $fixtureRoot 'runs\finalization-failed-A-1\evidence'
        $failedFinalizationReport = Join-Path $failedFinalizationEvidence 'report.md'
        $script:finalizationEvidence = $failedFinalizationEvidence
        $failedFinalizationResult = @(Invoke-RustV2RuntimeAcceptance `
            -MediaRoot $media -DurationSeconds 1800 -SampleSeconds 2 `
            -CargoTargetDir $fixtureRoot -ReleaseRoot $release `
            -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
            -EvidenceRoot $failedFinalizationEvidence -ReportPath $failedFinalizationReport `
            -Variant A -RunIndex 1 -SourceRevision 'fixture' -SourceTreeSha256 ('a' * 64) `
            -PackagePath (Join-Path $fixtureRoot 'fixture.zip') -PackageSha256 ('b' * 64) `
            -WorkerCount 20 -HddThreadsPerDisk 1 -SsdThreadsPerDisk 16 `
            -UnknownThreadsPerDisk 1 -TotalReadThreads 12 -ReservedCores 1)
        $failedFinalizationHarness = [IO.File]::ReadAllText((Join-Path $failedFinalizationEvidence 'harness-result.json')) | ConvertFrom-Json
        if ([string]$failedFinalizationHarness.run_status -cne 'FAIL' -or
            -not ($failedFinalizationResult -contains 'RUST_V2_RUNTIME_ACCEPTANCE_MEASURE_FAIL')) {
            throw "RED: deadline ID 相同的 failed 终态必须保留 FAIL，实际=$($failedFinalizationHarness | ConvertTo-Json -Compress) 输出=$($failedFinalizationResult -join '|')"
        }
        $script:finalizationFailedAfterCancel = $false
    }
    finally {
        if ($null -ne (Get-Variable -Name originalMediaRootsEnvironment -ErrorAction SilentlyContinue)) {
            [Environment]::SetEnvironmentVariable(
                'RUST_V2_REAL_MEDIA_ROOTS_JSON', $originalMediaRootsEnvironment, 'Process')
            [Environment]::SetEnvironmentVariable(
                'RUST_V2_ACCEPTANCE_SINGLE_RUN', $originalSingleRunEnvironment, 'Process')
        }
        foreach ($entry in $savedFinalizationFunctions.GetEnumerator()) {
            $functionPath = "Function:\$($entry.Key)"
            if ($null -eq $entry.Value) {
                Remove-Item -LiteralPath $functionPath -ErrorAction SilentlyContinue
            }
            else {
                Set-Item -LiteralPath $functionPath -Value $entry.Value
            }
        }
        Remove-Variable -Name finalizationReleaseRoot, originalNewHarnessResult, finalizationDeadlineTaskId, finalizationEvidence, finalizationFailedAfterCancel, originalMediaRootsEnvironment, originalSingleRunEnvironment, hostMediaRootsEnvironment, hostSingleRunEnvironment, expectedMediaRootsJson -Scope Script -ErrorAction SilentlyContinue
    }

    function Assert-NonFullyQualifiedInputRejected {
        <# 验证工具、证据和报告路径在归一化前拒绝 C:relative 与根相对路径。 #>
        param(
            [Parameter(Mandatory)] [string] $Name,
            [Parameter(Mandatory)] [string] $ParameterName,
            [Parameter(Mandatory)] [string] $Candidate,
            [Parameter(Mandatory)] [string] $ExpectedCode
        )
        $arguments = @{
            MediaRoot = $media
            DurationSeconds = 1800
            SampleSeconds = 2
            ReleaseRoot = $release
            AcceptanceClientPath = $acceptanceClient
            ResultExporterPath = $resultExporter
            EvidenceRoot = $evidence
            ReportPath = $report
            Variant = 'A'
            RunIndex = 1
            ThrowOnError = $false
        }
        $arguments[$ParameterName] = $Candidate
        $validation = Assert-RuntimeAcceptanceInputs @arguments
        if ($validation.Valid -or $validation.Code -ne $ExpectedCode) {
            throw "$Name 必须在归一化前拒绝：$($validation | ConvertTo-Json -Compress)"
        }
    }
    $rootRelativeInput = ([string][char]92) + 'relative'
    foreach ($invalidRootInput in @('C:relative', $rootRelativeInput)) {
        Assert-NonFullyQualifiedInputRejected -Name "AcceptanceClientPath=$invalidRootInput" `
            -ParameterName 'AcceptanceClientPath' -Candidate $invalidRootInput `
            -ExpectedCode 'RUST_V2_ACCEPTANCE_TOOLS_PATH_INVALID'
        Assert-NonFullyQualifiedInputRejected -Name "ResultExporterPath=$invalidRootInput" `
            -ParameterName 'ResultExporterPath' -Candidate $invalidRootInput `
            -ExpectedCode 'RUST_V2_ACCEPTANCE_TOOLS_PATH_INVALID'
        Assert-NonFullyQualifiedInputRejected -Name "EvidenceRoot=$invalidRootInput" `
            -ParameterName 'EvidenceRoot' -Candidate $invalidRootInput `
            -ExpectedCode 'RUST_V2_ACCEPTANCE_EVIDENCE_PATH_INVALID'
        Assert-NonFullyQualifiedInputRejected -Name "ReportPath=$invalidRootInput" `
            -ParameterName 'ReportPath' -Candidate $invalidRootInput `
            -ExpectedCode 'RUST_V2_ACCEPTANCE_EVIDENCE_PATH_INVALID'
    }

    # 受控 Node 在端点等待阶段立即退出，真实覆盖 collector 的启动、finalization 和结果落盘边界。
    $startupFailureFunctionNames = @(
        'Resolve-ReleaseRoot', 'Assert-RuntimeAcceptanceInputs', 'Test-IsAdministrator',
        'Get-FreeTcpPort', 'Wait-TcpEndpoint', 'Start-Process',
        'Get-CompletedProcessExitCode', 'Request-IsolatedNodeExit', 'Get-LastRuntimeResult',
        'Get-RuntimeMediaManifest', 'Assert-RuntimeMediaUnchanged')
    $savedStartupFailureFunctions = @{}
    foreach ($functionName in $startupFailureFunctionNames) {
        $functionCommand = Get-Command $functionName -CommandType Function -ErrorAction SilentlyContinue
        $savedStartupFailureFunctions[$functionName] = if ($functionCommand) { $functionCommand.ScriptBlock } else { $null }
    }
    $script:startupFailureReleaseRoot = $null
    $script:startupFailureNodeStdout = ''
    $script:startupFailureNodeStderr = ''
    try {
        function Resolve-ReleaseRoot {
            param([string] $CargoTargetDir, [string] $ReleaseRoot)
            $script:startupFailureReleaseRoot
        }
        function Assert-RuntimeAcceptanceInputs {
            param(
                [string] $MediaRoot, [string[]] $MediaRoots = @(), [int] $DurationSeconds, [int] $SampleSeconds,
                [string] $ReleaseRoot, [string] $AcceptanceClientPath, [string] $ResultExporterPath,
                [string] $EvidenceRoot, [string] $ReportPath, [string] $Variant = 'A',
                [int] $RunIndex = 1, [int] $WorkerCount = 20, [int] $HddThreadsPerDisk = 1,
                [int] $SsdThreadsPerDisk = 16, [int] $UnknownThreadsPerDisk = 1,
                [int] $TotalReadThreads = 12, [int] $ReservedCores = 1, [string] $Enumerator = 'everything',
                [switch] $SingleRun, [switch] $CompleteWhenTaskTerminal,
                [switch] $RequireDistinctPhysicalDisks, [switch] $ThrowOnError = $true)
            [pscustomobject]@{ Valid = $true; Code = '' }
        }
        function Test-IsAdministrator { $true }
        function Get-FreeTcpPort { 39126 }
        function Start-Process {
            param(
                [string] $FilePath, [string] $WorkingDirectory, [switch] $PassThru,
                [string] $WindowStyle, [string] $RedirectStandardOutput, [string] $RedirectStandardError)
            if ([IO.Path]::GetFileName($FilePath) -ine 'node.exe') {
                throw "fixture unexpected process=$FilePath"
            }
            $script:startupFailureNodeStdout = $RedirectStandardOutput
            $script:startupFailureNodeStderr = $RedirectStandardError
            if ($RedirectStandardOutput) {
                [IO.File]::WriteAllText($RedirectStandardOutput, 'fixture node stdout')
            }
            if ($RedirectStandardError) {
                [IO.File]::WriteAllText($RedirectStandardError, 'fixture node stderr')
            }
            [pscustomobject]@{
                HasExited = $true; Id = 4343; ExitCode = 17
                Path = $FilePath; StartTime = [DateTime]::UtcNow
            }
        }
        function Wait-TcpEndpoint {
            param([int] $Port, $Process, [int] $TimeoutSeconds = 60)
            if ($Process.HasExited) {
                throw "RUST_V2_ACCEPTANCE_NODE_EXITED code=$($Process.ExitCode)"
            }
            throw 'fixture endpoint wait must not continue'
        }
        function Get-CompletedProcessExitCode {
            param([Parameter(Mandatory)] $Process)
            $null
        }
        function Request-IsolatedNodeExit {
            param([Parameter(Mandatory)] $Node, [Parameter(Mandatory)] [string] $Root, [int] $TimeoutSeconds = 20)
            ''
        }
        function Get-LastRuntimeResult {
            param([Parameter(Mandatory)] [string] $Path)
            throw 'RUST_V2_ACCEPTANCE_RUNTIME_NDJSON_MISSING'
        }
        function Get-RuntimeMediaManifest {
            param([Parameter(Mandatory)] [string] $MediaRoot)
            [pscustomobject]@{
                Root = (Get-Item -LiteralPath $MediaRoot).FullName
                FileCount = 1
                TotalBytes = 1
                Files = @([pscustomobject]@{ Path = 'fixture.bin'; Length = 1; LastWriteTimeUtc = '2026-08-24T00:00:00.0000000Z' })
            }
        }
        function Assert-RuntimeMediaUnchanged {
            param([Parameter(Mandatory)] $Before, [Parameter(Mandatory)] $After)
        }
        $script:startupFailureReleaseRoot = $release
        $startupFailureEvidence = Join-Path $fixtureRoot 'runs\startup-failure-A-1\evidence'
        $startupFailureReport = Join-Path $startupFailureEvidence 'report.md'
        $startupFailureResult = @(Invoke-RustV2RuntimeAcceptance `
            -MediaRoot $media -DurationSeconds 1800 -SampleSeconds 2 `
            -CargoTargetDir $fixtureRoot -ReleaseRoot $release `
            -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
            -EvidenceRoot $startupFailureEvidence -ReportPath $startupFailureReport `
            -Variant A -RunIndex 1 -SourceRevision 'fixture' -SourceTreeSha256 ('c' * 64) `
            -PackagePath (Join-Path $fixtureRoot 'fixture.zip') -PackageSha256 ('d' * 64) `
            -WorkerCount 20 -HddThreadsPerDisk 1 -SsdThreadsPerDisk 16 `
            -UnknownThreadsPerDisk 1 -TotalReadThreads 12 -ReservedCores 1)
        $startupFailureHarnessPath = Join-Path $startupFailureEvidence 'harness-result.json'
        if (-not (Test-Path -LiteralPath $startupFailureHarnessPath -PathType Leaf)) {
            throw 'startup failure fixture must write harness-result.json'
        }
        $startupFailureHarness = [IO.File]::ReadAllText($startupFailureHarnessPath) | ConvertFrom-Json
        $archiveRoot = $env:RUST_V2_TASK13_FIX_EVIDENCE_ROOT
        if ([string]::IsNullOrWhiteSpace($archiveRoot)) {
            $archiveRoot = Join-Path ([IO.Path]::GetTempPath()) 'rust-v2-task13-startup-failure-observation'
        }
        New-Item -ItemType Directory -Path $archiveRoot -Force | Out-Null
        $observation = [ordered]@{
            node_stdout_path = $script:startupFailureNodeStdout
            node_stderr_path = $script:startupFailureNodeStderr
            node_stdout_exists = if ($script:startupFailureNodeStdout) { Test-Path -LiteralPath $script:startupFailureNodeStdout -PathType Leaf } else { $false }
            node_stderr_exists = if ($script:startupFailureNodeStderr) { Test-Path -LiteralPath $script:startupFailureNodeStderr -PathType Leaf } else { $false }
            run_result = @($startupFailureResult)
            harness_result = $startupFailureHarness
        }
        [IO.File]::WriteAllText((Join-Path $archiveRoot 'startup-failure-observation.json'),
            ($observation | ConvertTo-Json -Depth 12), [Text.UTF8Encoding]::new($false))
        if ([string]::IsNullOrWhiteSpace($script:startupFailureNodeStdout) -or
            [string]::IsNullOrWhiteSpace($script:startupFailureNodeStderr)) {
            throw 'RED: Node stdout/stderr must be redirected into evidence before startup'
        }
        if (-not (Test-Path -LiteralPath $script:startupFailureNodeStdout -PathType Leaf) -or
            -not (Test-Path -LiteralPath $script:startupFailureNodeStderr -PathType Leaf)) {
            throw 'RED: redirected Node stdout/stderr files must exist'
        }
        if (-not $startupFailureHarness.PSObject.Properties['run_diagnostic']) {
            throw 'RED: harness-result must persist run_diagnostic'
        }
        if ([string]$startupFailureHarness.run_status -cne 'INCONCLUSIVE') {
            throw "RED: startup exit must be INCONCLUSIVE, actual=$($startupFailureHarness.run_status)"
        }
        if (-not ($startupFailureResult -contains 'RUST_V2_RUNTIME_ACCEPTANCE_MEASURE_INCONCLUSIVE')) {
            throw "RED: startup exit must emit INCONCLUSIVE marker, actual=$($startupFailureResult -join '|')"
        }
        $startupFailureRunRoot = Split-Path -Parent $startupFailureEvidence
        $bootstrapConfigLine = @(Get-Content -LiteralPath (Join-Path $startupFailureRunRoot 'release\bootstrap.toml') |
            Where-Object { $_ -match '^config_path\s*=' })
        $configConfigLine = @(Get-Content -LiteralPath (Join-Path $startupFailureRunRoot 'release\data\node\config.toml') |
            Where-Object { $_ -match '^config_path\s*=' })
        if ($bootstrapConfigLine.Count -ne 1 -or $configConfigLine.Count -ne 1 -or
            [string]$bootstrapConfigLine[0] -cne [string]$configConfigLine[0]) {
            throw "bootstrap 与 node config 必须保留相同 TOML config_path：bootstrap=$bootstrapConfigLine config=$configConfigLine"
        }
    }
    finally {
        foreach ($entry in $savedStartupFailureFunctions.GetEnumerator()) {
            $functionPath = "Function:\$($entry.Key)"
            if ($null -eq $entry.Value) {
                Remove-Item -LiteralPath $functionPath -ErrorAction SilentlyContinue
            }
            else {
                Set-Item -LiteralPath $functionPath -Value $entry.Value
            }
        }
        Remove-Variable -Name startupFailureReleaseRoot, startupFailureNodeStdout, startupFailureNodeStderr -Scope Script -ErrorAction SilentlyContinue
    }

    # RED/GREEN：client 正常结束后，即使 Node 优雅关闭超时，只要受控终止已确认，仍应在数据库关闭后运行 exporter。
    $shutdownFinalizationFunctionNames = @(
        'Resolve-ReleaseRoot', 'Assert-RuntimeAcceptanceInputs', 'Test-IsAdministrator',
        'Get-FreeTcpPort', 'Wait-TcpEndpoint', 'Start-Process',
        'Get-CompletedProcessExitCode', 'Request-IsolatedNodeExit', 'Get-IsolatedProcesses',
        'Stop-IsolatedProcesses',
        'Get-LastRuntimeResult', 'Invoke-ResultExporter', 'Parse-ResultSummaryOutput',
        'Get-ResultSummaryArtifacts', 'Get-RuntimeMediaManifest', 'Assert-RuntimeMediaUnchanged',
        'Start-RuntimeAcceptanceSupervisor', 'Get-RuntimeAcceptanceSupervisorStatus',
        'Stop-RuntimeAcceptanceSupervisor')
    $savedShutdownFinalizationFunctions = @{}
    foreach ($functionName in $shutdownFinalizationFunctionNames) {
        $functionCommand = Get-Command $functionName -CommandType Function -ErrorAction SilentlyContinue
        $savedShutdownFinalizationFunctions[$functionName] = if ($functionCommand) { $functionCommand.ScriptBlock } else { $null }
    }
    $script:shutdownFixtureReleaseRoot = $null
    $script:shutdownFixtureEvidence = $null
    $script:shutdownFixtureExporterCalls = 0
    $script:shutdownFixtureExporterDatabasePath = ''
    $script:shutdownFixtureShutdownCalls = 0
    $script:shutdownFixtureProcessesStopped = $false
    $script:shutdownFixtureObservedZero = $false
    $script:shutdownFixtureNode = $null
    try {
        function Resolve-ReleaseRoot {
            param([string] $CargoTargetDir, [string] $ReleaseRoot)
            $script:shutdownFixtureReleaseRoot
        }
        function Assert-RuntimeAcceptanceInputs {
            param(
                [string] $MediaRoot, [string[]] $MediaRoots = @(), [int] $DurationSeconds, [int] $SampleSeconds,
                [string] $ReleaseRoot, [string] $AcceptanceClientPath, [string] $ResultExporterPath,
                [string] $EvidenceRoot, [string] $ReportPath, [string] $Variant = 'A',
                [int] $RunIndex = 1, [int] $WorkerCount = 20, [int] $HddThreadsPerDisk = 1,
                [int] $SsdThreadsPerDisk = 16, [int] $UnknownThreadsPerDisk = 1,
                [int] $TotalReadThreads = 12, [int] $ReservedCores = 1, [switch] $SingleRun,
                [switch] $RequireDistinctPhysicalDisks, [switch] $ThrowOnError = $true)
            [pscustomobject]@{ Valid = $true; Code = '' }
        }
        function Test-IsAdministrator { $true }
        function Get-FreeTcpPort { 39127 }
        function Wait-TcpEndpoint {
            param([int] $Port, $Process, [int] $TimeoutSeconds = 60)
        }
        function Start-Process {
            param(
                [string] $FilePath, [string] $WorkingDirectory, [switch] $PassThru,
                [string] $WindowStyle, [string] $RedirectStandardOutput, [string] $RedirectStandardError)
            $name = [IO.Path]::GetFileName($FilePath)
            if ($name -ieq 'node.exe') {
                $fixtureDatabase = Join-Path $WorkingDirectory 'data\node\node.db'
                New-Item -ItemType Directory -Path (Split-Path -Parent $fixtureDatabase) -Force | Out-Null
                [IO.File]::WriteAllText($fixtureDatabase, 'fixture database')
                [IO.File]::WriteAllText("$fixtureDatabase-wal", 'fixture wal')
                [IO.File]::WriteAllText("$fixtureDatabase-shm", 'fixture shm')
                $node = [pscustomobject]@{
                    HasExited = $false; Id = 5151; ExitCode = 0
                    Path = $FilePath; StartTime = [DateTime]::UtcNow
                }
                # 真实 round7 分支：CloseMainWindow 成功请求，但优雅等待超时，随后必须受控清理。
                $node | Add-Member -MemberType ScriptMethod -Name CloseMainWindow -Value { $true }
                $node | Add-Member -MemberType ScriptMethod -Name WaitForExit -Value { param([int] $Milliseconds) $false }
                $script:shutdownFixtureNode = $node
                return $node
            }
            if ($name -ieq 'runtime_acceptance.exe') {
                [IO.File]::WriteAllText((Join-Path $script:shutdownFixtureEvidence 'runtime.ndjson'), "{}`n")
                [IO.File]::WriteAllText((Join-Path $script:shutdownFixtureEvidence 'system.ndjson'), "{}`n")
                return [pscustomobject]@{
                    HasExited = $true; Id = 5152; ExitCode = 0
                    Path = $FilePath; StartTime = [DateTime]::UtcNow
                }
            }
            throw "fixture unexpected process=$FilePath"
        }
        function Start-RuntimeAcceptanceSupervisor {
            param(
                [int] $ClientId, [string] $ClientPath, [string] $ClientStartTimeUtc,
                [int] $NodeId, [string] $NodePath, [string] $NodeStartTimeUtc,
                [DateTime] $DeadlineUtc, [string] $StatusPath)
            [pscustomobject]@{ Id = 5154; Process = $null; StatusPath = $StatusPath }
        }
        function Get-RuntimeAcceptanceSupervisorStatus {
            param([Parameter(Mandatory)] $Supervisor)
            [pscustomobject]@{ TimedOut = $false; Diagnostic = '' }
        }
        function Stop-RuntimeAcceptanceSupervisor {
            param([Parameter(Mandatory)] $Supervisor)
            [pscustomobject]@{ ExitConfirmed = $true; Diagnostic = '' }
        }
        function Get-CompletedProcessExitCode {
            param([Parameter(Mandatory)] $Process)
            0
        }
        function Get-IsolatedProcesses {
            param([Parameter(Mandatory)] [string] $Root, [scriptblock] $ProcessEnumerator)
            if ($script:shutdownFixtureProcessesStopped) {
                # 受控 stop 成功后的最终枚举必须确认隔离 Node/Worker 已全部归零。
                $script:shutdownFixtureObservedZero = $true
                return @()
            }
            @(
                [pscustomobject]@{ ProcessId = 5151 }
                [pscustomobject]@{ ProcessId = 5153 }
            )
        }
        function Stop-IsolatedProcesses {
            param(
                [Parameter(Mandatory)] [string] $Root,
                [scriptblock] $ProcessEnumerator,
                [scriptblock] $ProcessTerminator)
            $script:shutdownFixtureShutdownCalls++
            $script:shutdownFixtureProcessesStopped = $true
            if ($script:shutdownFixtureNode) {
                $script:shutdownFixtureNode.HasExited = $true
            }
        }
        function Get-LastRuntimeResult {
            param([Parameter(Mandatory)] [string] $Path)
            [pscustomobject]@{
                latest_completed_persistent_task_id = 'task-completed'
                deadline_cancelled_persistent_task_id = ''
                scan_tasks = @([pscustomobject]@{
                        persistent_task_id = 'task-completed'
                        terminal_state = 'completed'
                    })
                failed_scans = 0
            }
        }
        function Invoke-ResultExporter {
            param(
                [Parameter(Mandatory)] [string] $ExporterPath,
                [Parameter(Mandatory)] [string] $DatabasePath,
                [Parameter(Mandatory)] [string] $CacheRoot,
                [string[]] $MediaRoots = @(),
                [Parameter(Mandatory)] [string] $OutputPath,
                [Parameter(Mandatory)] [string] $EvidenceRoot,
                [int] $TimeoutSeconds = 120, [scriptblock] $ProcessKiller, [scriptblock] $ProcessWaiter)
            if (-not $script:shutdownFixtureProcessesStopped) {
                throw 'RUST_V2_ACCEPTANCE_EXPORTER_BEFORE_NODE_EXIT'
            }
            $script:shutdownFixtureExporterCalls++
            $script:shutdownFixtureExporterDatabasePath = $DatabasePath
            [pscustomobject]@{ ExitCode = 0; Diagnostic = ''; Stdout = 'fixture exporter' }
        }
        function Parse-ResultSummaryOutput {
            param(
                [Parameter(Mandatory)] [string] $Text,
                [Parameter(Mandatory)] [string] $ExpectedPath,
                [string] $ExpectedTaskId = '')
            [pscustomobject]@{
                Status = 'PASS'; Path = $ExpectedPath; Sha256 = ('e' * 64)
                RowCount = 1; MissingCount = 0; InconclusiveCount = 0; TaskId = $ExpectedTaskId
            }
        }
        function Get-ResultSummaryArtifacts {
            param(
                [Parameter(Mandatory)] [string] $SummaryPath,
                [string] $ExpectedTaskId = '', [string] $ExpectedStatus = '',
                [string] $ExpectedSha256 = '', [long] $ExpectedRowCount = -1)
            [pscustomobject]@{ BindingValid = $true; Diagnostic = '' }
        }
        function Get-RuntimeMediaManifest {
            param([Parameter(Mandatory)] [string] $MediaRoot)
            [pscustomobject]@{
                Root = (Get-Item -LiteralPath $MediaRoot).FullName
                FileCount = 1
                TotalBytes = 1
                Files = @([pscustomobject]@{ Path = 'fixture.bin'; Length = 1; LastWriteTimeUtc = '2026-08-24T00:00:00.0000000Z' })
            }
        }
        function Assert-RuntimeMediaUnchanged {
            param([Parameter(Mandatory)] $Before, [Parameter(Mandatory)] $After)
        }
        $script:shutdownFixtureReleaseRoot = $release
        $shutdownFinalizationEvidence = Join-Path $fixtureRoot 'runs\shutdown-timeout-A-1\evidence'
        $shutdownFinalizationReport = Join-Path $shutdownFinalizationEvidence 'report.md'
        $script:shutdownFixtureEvidence = $shutdownFinalizationEvidence
        $shutdownFinalizationResult = @(Invoke-RustV2RuntimeAcceptance `
            -MediaRoot $media -DurationSeconds 1800 -SampleSeconds 2 `
            -CargoTargetDir $fixtureRoot -ReleaseRoot $release `
            -AcceptanceClientPath $acceptanceClient -ResultExporterPath $resultExporter `
            -EvidenceRoot $shutdownFinalizationEvidence -ReportPath $shutdownFinalizationReport `
            -Variant A -RunIndex 1 -SourceRevision 'fixture' -SourceTreeSha256 ('e' * 64) `
            -PackagePath (Join-Path $fixtureRoot 'fixture.zip') -PackageSha256 ('f' * 64) `
            -WorkerCount 20 -HddThreadsPerDisk 1 -SsdThreadsPerDisk 16 `
            -UnknownThreadsPerDisk 1 -TotalReadThreads 12 -ReservedCores 1)
        $shutdownFinalizationHarness = [IO.File]::ReadAllText((Join-Path $shutdownFinalizationEvidence 'harness-result.json')) | ConvertFrom-Json
        if ($script:shutdownFixtureExporterCalls -ne 1) {
            throw "RED: CloseMainWindow=true/WaitForExit=false 且受控 stop 成功后必须运行 exporter，实际调用=$script:shutdownFixtureExporterCalls shutdownCalls=$script:shutdownFixtureShutdownCalls stopped=$script:shutdownFixtureProcessesStopped harness=$($shutdownFinalizationHarness | ConvertTo-Json -Compress)"
        }
        if (-not $script:shutdownFixtureProcessesStopped -or $script:shutdownFixtureShutdownCalls -lt 1) {
            throw 'RED: exporter 前必须先完成受控 Node/Worker 清理确认'
        }
        if (-not $script:shutdownFixtureObservedZero) {
            throw 'RED: 受控 stop 成功后必须通过最终枚举确认 Node/Worker 归零'
        }
        $snapshotRoot = Split-Path -Parent $script:shutdownFixtureExporterDatabasePath
        if ([IO.Path]::GetFileName($snapshotRoot) -notmatch '^database-snapshot-[0-9a-f]{32}$' -or
            -not (Test-Path -LiteralPath (Join-Path $snapshotRoot 'snapshot-metadata.json') -PathType Leaf) -or
            -not (([IO.File]::GetAttributes($script:shutdownFixtureExporterDatabasePath) -band [IO.FileAttributes]::ReadOnly) -ne 0)) {
            throw "RED: exporter 必须读取 evidence 下唯一只读数据库快照：$script:shutdownFixtureExporterDatabasePath"
        }
        if (-not [string]::IsNullOrWhiteSpace([string]$shutdownFinalizationHarness.run_diagnostic) -or
            [string]$shutdownFinalizationHarness.run_status -cne 'PASS' -or
            [int]$shutdownFinalizationHarness.exporter_exit_code -ne 0 -or
            -not ($shutdownFinalizationResult -contains 'RUST_V2_RUNTIME_ACCEPTANCE_MEASURE_PASS')) {
            throw "RED: 已确认受控 stop 后应清空诊断并完成 PASS，实际=$($shutdownFinalizationHarness | ConvertTo-Json -Compress)"
        }
    }
    finally {
        foreach ($entry in $savedShutdownFinalizationFunctions.GetEnumerator()) {
            $functionPath = "Function:\$($entry.Key)"
            if ($null -eq $entry.Value) {
                Remove-Item -LiteralPath $functionPath -ErrorAction SilentlyContinue
            }
            else {
                Set-Item -LiteralPath $functionPath -Value $entry.Value
            }
        }
        Remove-Variable -Name shutdownFixtureReleaseRoot, shutdownFixtureEvidence, shutdownFixtureExporterCalls, shutdownFixtureExporterDatabasePath, shutdownFixtureShutdownCalls, shutdownFixtureProcessesStopped, shutdownFixtureObservedZero, shutdownFixtureNode -Scope Script -ErrorAction SilentlyContinue
    }

    Write-Output 'RUST_V2_RUNTIME_ACCEPTANCE_HARNESS_PASS'
}
finally {
    if (Test-Path -LiteralPath $fixtureRoot) {
        Remove-Item -LiteralPath $fixtureRoot -Recurse -Force
    }
}
