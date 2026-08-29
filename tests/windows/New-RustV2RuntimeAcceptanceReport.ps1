<#{
.SYNOPSIS
将一次真实媒体运行的 NDJSON、双盘清单和固定 TSV 汇总为中文验收报告。

.DESCRIPTION
本报告只处理一次运行。任务在明确进入 completed/failed/cancelled 终态后即可结束，
持续时间只是客户端的最大上限。结果摘要使用 exporter 生成的 result-summary.tsv；
不依赖任务历史、分页游标或额外结果旁车文件。
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string] $EvidenceRoot,
    [string] $OutputPath = ''
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

if ([string]::IsNullOrWhiteSpace($EvidenceRoot)) {
    throw 'RUST_V2_RUNTIME_EVIDENCE_ROOT_INVALID'
}
$evidenceAbsolute = [IO.Path]::GetFullPath($EvidenceRoot).TrimEnd('\')
if ([string]::IsNullOrWhiteSpace($OutputPath)) {
    $OutputPath = Join-Path $EvidenceRoot 'report.md'
}
$outputAbsolute = [IO.Path]::GetFullPath($OutputPath).TrimEnd('\')
if ($outputAbsolute.Equals($evidenceAbsolute, [StringComparison]::OrdinalIgnoreCase) -or
    -not $outputAbsolute.StartsWith(($evidenceAbsolute + '\'), [StringComparison]::OrdinalIgnoreCase)) {
    throw 'RUST_V2_RUNTIME_REPORT_OUTSIDE_EVIDENCE'
}

function Get-OptionalProperty {
    <# 读取可选 JSON 属性；缺失时返回 null，不用默认值伪造实测。 #>
    param($Object, [string] $Name)

    if ($null -eq $Object) { return $null }
    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property) { return $null }
    $property.Value
}

function Get-PropertyArray {
    <# 将 JSON 数组或单值统一为数组，避免空数组在 PowerShell 中丢失。 #>
    param($Object, [string] $Name)

    $value = Get-OptionalProperty -Object $Object -Name $Name
    if ($null -eq $value) { return @() }
    @($value)
}

function Get-NumberOrNull {
    <# 将有限数值转换为 double；空值或非法值保持 null。 #>
    param($Value)

    if ($null -eq $Value) { return $null }
    try {
        $number = [double]$Value
        if ([double]::IsNaN($number) -or [double]::IsInfinity($number)) { return $null }
        $number
    }
    catch {
        $null
    }
}

function Get-IntegerOrNull {
    <# 将非负整数转换为 long，供计数和 footer 校验使用。 #>
    param($Value)

    if ($null -eq $Value) { return $null }
    try {
        $number = [int64]$Value
        if ($number -lt 0) { return $null }
        $number
    }
    catch {
        $null
    }
}

function Get-SignedIntegerOrNull {
    <# 将可为负的整数转换为 long，供进程退出码等状态值使用。 #>
    param($Value)

    if ($null -eq $Value) { return $null }
    try { [int64]$Value }
    catch { $null }
}

function Get-Sha256Bytes {
    <# 计算字节数组 SHA-256，固定输出小写十六进制。 #>
    param([byte[]] $Bytes)

    $sha = [Security.Cryptography.SHA256]::Create()
    try {
        ([BitConverter]::ToString($sha.ComputeHash($Bytes)) -replace '-', '').ToLowerInvariant()
    }
    finally {
        $sha.Dispose()
    }
}

function Get-HashOfText {
    <# 使用 UTF-8 无 BOM 计算 TSV 数据区 SHA。 #>
    param([string] $Text)

    Get-Sha256Bytes -Bytes ([Text.UTF8Encoding]::new($false).GetBytes($Text))
}

function Read-JsonEvidence {
    <# 读取单个 JSON 证据；读取失败只标记 INCONCLUSIVE，不中断报告生成。 #>
    param([Parameter(Mandatory)] [string] $Path)

    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        return [pscustomobject]@{ Valid = $false; Value = $null; Errors = @("文件缺失：$Path") }
    }
    try {
        [pscustomobject]@{
            Valid = $true
            Value = [IO.File]::ReadAllText($Path) | ConvertFrom-Json
            Errors = @()
        }
    }
    catch {
        [pscustomobject]@{ Valid = $false; Value = $null; Errors = @("JSON 解析失败：$Path：$($_.Exception.Message)") }
    }
}

function Read-NdjsonEvidence {
    <# 逐行读取 NDJSON；坏行不会被忽略，防止用部分时间轴冒充完整运行。 #>
    param([Parameter(Mandatory)] [string] $Path)

    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        return [pscustomobject]@{ Valid = $false; Records = @(); Errors = @("文件缺失：$Path") }
    }
    $records = [Collections.Generic.List[object]]::new()
    $errors = [Collections.Generic.List[string]]::new()
    $lineNumber = 0
    try {
        foreach ($line in [IO.File]::ReadAllLines($Path)) {
            $lineNumber++
            if ([string]::IsNullOrWhiteSpace($line)) { continue }
            try {
                [void]$records.Add(($line | ConvertFrom-Json))
            }
            catch {
                [void]$errors.Add("NDJSON 第 $lineNumber 行解析失败：$($_.Exception.Message)")
            }
        }
    }
    catch {
        [void]$errors.Add("NDJSON 读取失败：$Path：$($_.Exception.Message)")
    }
    [pscustomobject]@{ Valid = $errors.Count -eq 0; Records = @($records); Errors = @($errors) }
}

function Get-NormalizedWindowsPath {
    <# 规范化 Windows 路径用于大小写不敏感的根边界比较。 #>
    param([string] $Path)

    if ([string]::IsNullOrWhiteSpace($Path)) { return '' }
    try { [IO.Path]::GetFullPath($Path).TrimEnd('\') }
    catch { '' }
}

function Test-WindowsPathEqual {
    <# 比较两个 Windows 路径，不把 H:\A 当作 H:\AB 的前缀。 #>
    param([string] $Left, [string] $Right)

    $leftPath = Get-NormalizedWindowsPath -Path $Left
    $rightPath = Get-NormalizedWindowsPath -Path $Right
    -not [string]::IsNullOrWhiteSpace($leftPath) -and
        $leftPath.Equals($rightPath, [StringComparison]::OrdinalIgnoreCase)
}

function Test-ApprovedMediaRoots {
    <# 严格验证报告中的双盘媒体根，不能只看到 H:/I: 盘符就认为证据匹配。 #>
    param([object[]] $Roots)

    $expected = @('H:\pik\00000000000', 'I:\tmp')
    if (@($Roots).Count -ne $expected.Count) { return $false }
    for ($index = 0; $index -lt $expected.Count; $index++) {
        if (-not (Test-WindowsPathEqual -Left ([string]$Roots[$index]) -Right $expected[$index])) {
            return $false
        }
    }
    $true
}

function Test-WindowsPathWithin {
    <# 判断候选路径是否属于媒体根的组件边界。 #>
    param([string] $Candidate, [string] $Root)

    $candidatePath = Get-NormalizedWindowsPath -Path $Candidate
    $rootPath = Get-NormalizedWindowsPath -Path $Root
    if ([string]::IsNullOrWhiteSpace($candidatePath) -or [string]::IsNullOrWhiteSpace($rootPath)) {
        return $false
    }
    $candidatePath.Equals($rootPath, [StringComparison]::OrdinalIgnoreCase) -or
        $candidatePath.StartsWith(($rootPath + '\'), [StringComparison]::OrdinalIgnoreCase)
}

function Test-JsonEquivalent {
    <# 比较已解析 JSON 的语义结构，不受属性空白影响。 #>
    param($Left, $Right)

    if ($null -eq $Left -or $null -eq $Right) { return $false }
    (($Left | ConvertTo-Json -Depth 40 -Compress) -ceq ($Right | ConvertTo-Json -Depth 40 -Compress))
}

function Format-Optional {
    <# 报告中统一将协议未提供的值显示为破折号。 #>
    param($Value, [string] $Suffix = '')

    if ($null -eq $Value) { return '—' }
    "$Value$Suffix"
}

function Format-Bytes {
    <# 将字节数转换为简洁可读单位。 #>
    param([double] $Value)

    if ($Value -ge 1GB) { return ('{0:N2} GiB' -f ($Value / 1GB)) }
    if ($Value -ge 1MB) { return ('{0:N2} MiB' -f ($Value / 1MB)) }
    if ($Value -ge 1KB) { return ('{0:N2} KiB' -f ($Value / 1KB)) }
    '{0:N0} B' -f $Value
}

function Get-SampleTimestampMs {
    <# 只读取真实 UTC 毫秒；不以 elapsed_seconds 代替时间戳。 #>
    param($Sample)

    $value = Get-NumberOrNull (Get-OptionalProperty -Object $Sample -Name 'utc_unix_ms')
    if ($null -eq $value -or $value -lt 0) { return $null }
    $value
}

function Get-TimeCoverage {
    <# 校验任务 1 秒快照和系统 2 秒样本的真实时间轴及声明间隔。 #>
    param(
        [Parameter(Mandatory)] [object[]] $Samples,
        [Parameter(Mandatory)] [double] $TargetMs,
        [Parameter(Mandatory)] [double] $MaxGapMs,
        [Parameter(Mandatory)] [double] $AllowedDriftMs,
        [Parameter(Mandatory)] [string] $Label
    )

    $errors = [Collections.Generic.List[string]]::new()
    $ordered = @($Samples | Sort-Object @{ Expression = { Get-SampleTimestampMs -Sample $_ } })
    $times = @($ordered | ForEach-Object { Get-SampleTimestampMs -Sample $_ })
    if ($ordered.Count -lt 2) {
        [void]$errors.Add("$Label 样本少于2条，无法证明时间覆盖")
    }
    if (@($times | Where-Object { $null -eq $_ }).Count -gt 0) {
        [void]$errors.Add("$Label 缺少 utc_unix_ms")
    }
    $gaps = [Collections.Generic.List[double]]::new()
    for ($index = 1; $index -lt $ordered.Count; $index++) {
        $previous = Get-SampleTimestampMs -Sample $ordered[$index - 1]
        $current = Get-SampleTimestampMs -Sample $ordered[$index]
        if ($null -eq $previous -or $null -eq $current) { continue }
        $actualGap = [double]$current - [double]$previous
        [void]$gaps.Add($actualGap)
        $declared = Get-NumberOrNull (Get-OptionalProperty -Object $ordered[$index] -Name 'sample_interval_ms')
        if ($actualGap -le 0 -or $actualGap -gt $MaxGapMs) {
            [void]$errors.Add("$Label 相邻时间间隔无效或超过上限：$([Math]::Round($actualGap, 3)) ms")
        }
        if ($null -eq $declared -or $declared -le 0 -or
            [Math]::Abs($actualGap - $declared) -gt $AllowedDriftMs) {
            [void]$errors.Add("$Label sample_interval_ms 与 UTC 间隔不一致")
        }
    }
    if ($gaps.Count -eq 0) {
        [void]$errors.Add("$Label 没有可验证的正时间间隔")
    }
    $maxGap = if ($gaps.Count -eq 0) { $null } else { ($gaps | Measure-Object -Maximum).Maximum }
    $averageGap = if ($gaps.Count -eq 0) { $null } else { ($gaps | Measure-Object -Average).Average }
    [pscustomobject]@{
        Valid = $errors.Count -eq 0
        Errors = @($errors)
        MaxGapMs = $maxGap
        AverageGapMs = $averageGap
        TargetMs = $TargetMs
        Samples = $ordered.Count
    }
}

function Get-ResultSummaryTsv {
    <# 校验固定 29 列 TSV、R 行、F footer、数据区 SHA 和完整文件 SHA。 #>
    param(
        [Parameter(Mandatory)] [string] $EvidenceRoot,
        [Parameter(Mandatory)] $Harness
    )

    $columns = @(
        'record_type', 'status', 'machine_id', 'normalized_path', 'display_path', 'file_size', 'md5',
        'media_type', 'base_complete', 'feature_payload_sha256', 'image_stage1_sha256', 'image_stage2_sha256',
        'video_metadata_sha256', 'video_frame_stage1_0_sha256', 'video_frame_stage1_1_sha256',
        'video_frame_stage1_2_sha256', 'video_frame_stage1_3_sha256', 'video_frame_stage1_4_sha256',
        'video_frame_stage1_5_sha256', 'video_frame_stage2_0_sha256', 'video_frame_stage2_1_sha256',
        'video_frame_stage2_2_sha256', 'video_frame_stage2_3_sha256', 'video_frame_stage2_4_sha256',
        'video_frame_stage2_5_sha256', 'thumbnail_sha256', 'thumbnail_state', 'contact_sheet_sha256',
        'status_reason'
    )
    $expectedHeader = $columns -join "`t"
    $defaultPath = [IO.Path]::GetFullPath((Join-Path $EvidenceRoot 'result-summary.tsv'))
    $pathProperty = $Harness.PSObject.Properties['result_summary_path']
    $declaredPath = if ($null -eq $pathProperty) { '' } else { [string]$pathProperty.Value }
    $path = $defaultPath
    $errors = [Collections.Generic.List[string]]::new()
    if ([string]::IsNullOrWhiteSpace($declaredPath)) {
        [void]$errors.Add('harness 缺少 result_summary_path')
    }
    else {
        try {
            $path = [IO.Path]::GetFullPath($declaredPath)
            if (-not $path.Equals($defaultPath, [StringComparison]::OrdinalIgnoreCase)) {
                [void]$errors.Add('result_summary_path 未绑定本次 evidence 的 TSV')
            }
        }
        catch {
            [void]$errors.Add('result_summary_path 无效')
        }
    }
    $rows = [Collections.Generic.List[object]]::new()
    $derivedStatus = 'MISSING'
    $rowCount = 0L
    $missingCount = 0L
    $inconclusiveCount = 0L
    $fullSha = $null
    $dataSha = $null
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        [void]$errors.Add('result-summary.tsv 文件缺失')
    }
    else {
        try {
            $bytes = [IO.File]::ReadAllBytes($path)
            if ($bytes.Length -eq 0 -or $bytes[$bytes.Length - 1] -ne [byte]10) {
                [void]$errors.Add('TSV 必须以 LF 结尾')
            }
            if ([Array]::IndexOf($bytes, [byte]13) -ge 0) {
                [void]$errors.Add('TSV 不允许 CRLF')
            }
            if ($bytes.Length -ge 3 -and $bytes[0] -eq [byte]239 -and
                $bytes[1] -eq [byte]187 -and $bytes[2] -eq [byte]191) {
                [void]$errors.Add('TSV 不允许 UTF-8 BOM')
            }
            $text = [Text.UTF8Encoding]::new($false, $true).GetString($bytes)
            $lines = $text.Split([char]10, [StringSplitOptions]::None)
            if ($lines.Count -lt 3) {
                [void]$errors.Add('TSV 缺少数据行或 footer')
            }
            else {
                if ($lines[0] -cne $expectedHeader) {
                    [void]$errors.Add('TSV 固定表头不匹配')
                }
                $footerIndex = $lines.Count - 2
                $footer = @($lines[$footerIndex].Split([char]9))
                if ($footer.Count -ne 3 -or $footer[0] -cne 'F' -or
                    $footer[1] -notmatch '^[0-9]+$' -or $footer[2] -notmatch '^[0-9a-f]{64}$') {
                    [void]$errors.Add('TSV footer 格式无效')
                }
                else {
                    $rowCount = [int64]$footer[1]
                    $dataText = $lines[0] + "`n"
                    $actualRows = 0L
                    for ($lineIndex = 1; $lineIndex -lt $footerIndex; $lineIndex++) {
                        $line = $lines[$lineIndex]
                        $fields = @($line.Split([char]9))
                        if ($fields.Count -ne 29 -or $fields[0] -cne 'R' -or
                            [string]::IsNullOrWhiteSpace($fields[1])) {
                            [void]$errors.Add("TSV R 行无效：第 $($lineIndex + 1) 行")
                            continue
                        }
                        if ($fields[1] -ceq 'MISSING') { $missingCount++ }
                        if ($fields[1] -ceq 'INCONCLUSIVE') { $inconclusiveCount++ }
                        [void]$rows.Add([pscustomobject]@{
                                Status = [string]$fields[1]
                                NormalizedPath = [string]$fields[3]
                                MediaType = [string]$fields[7]
                                Md5 = [string]$fields[6]
                                BaseComplete = [string]$fields[8]
                            })
                        $dataText += $line + "`n"
                        $actualRows++
                    }
                    if ($actualRows -ne $rowCount) {
                        [void]$errors.Add("TSV footer 行数不匹配：footer=$rowCount actual=$actualRows")
                    }
                    $dataSha = Get-HashOfText -Text $dataText
                    if ($dataSha -cne [string]$footer[2]) {
                        [void]$errors.Add('TSV footer 数据区 SHA 校验失败')
                    }
                }
            }
            $fullSha = Get-Sha256Bytes -Bytes $bytes
        }
        catch {
            [void]$errors.Add("TSV 读取或 UTF-8 校验失败：$($_.Exception.Message)")
        }
    }
    if (-not (Test-Path -LiteralPath $path -PathType Leaf) -or $inconclusiveCount -gt 0) {
        # exporter 未产出 TSV 时，报告状态是证据不确定，而不是把缺文件当作普通缺失项。
        $derivedStatus = 'INCONCLUSIVE'
    }
    elseif ($missingCount -gt 0 -or $rowCount -eq 0) {
        $derivedStatus = 'MISSING'
    }
    else {
        $derivedStatus = 'PASS'
    }
    $expectedStatus = [string](Get-OptionalProperty -Object $Harness -Name 'result_summary_status')
    $expectedShaProperty = $Harness.PSObject.Properties['result_summary_sha256']
    $expectedSha = if ($null -ne $expectedShaProperty) { [string]$expectedShaProperty.Value } else { '' }
    if ([string]::IsNullOrWhiteSpace($expectedSha)) {
        if ($null -eq $expectedShaProperty -or $null -ne $fullSha) {
            [void]$errors.Add('harness 缺少 result_summary_sha256')
        }
    }
    elseif ($null -ne $fullSha -and $expectedSha -notmatch '^[0-9a-fA-F]{64}$') {
        [void]$errors.Add('harness result_summary_sha256 格式无效')
    }
    elseif ($null -ne $fullSha -and $fullSha -cne $expectedSha.ToLowerInvariant()) {
        [void]$errors.Add('TSV 完整文件 SHA 与 harness 不一致')
    }
    if (-not [string]::IsNullOrWhiteSpace($expectedStatus) -and
        $expectedStatus.ToUpperInvariant() -cne $derivedStatus) {
        [void]$errors.Add('TSV 状态与 harness 不一致')
    }
    foreach ($binding in @(
            [pscustomobject]@{ Name = 'result_summary_row_count'; Actual = $rowCount }
            [pscustomobject]@{ Name = 'result_summary_missing_count'; Actual = $missingCount }
            [pscustomobject]@{ Name = 'result_summary_inconclusive_count'; Actual = $inconclusiveCount }
        )) {
        $expectedCount = Get-IntegerOrNull (Get-OptionalProperty -Object $Harness -Name $binding.Name)
        if ($null -ne $expectedCount -and $expectedCount -ne $binding.Actual) {
            [void]$errors.Add("$($binding.Name) 与 TSV 不一致")
        }
    }
    foreach ($row in @($rows)) {
        if ($row.Status -notin @('PASS', 'MISSING', 'INCONCLUSIVE')) {
            [void]$errors.Add("TSV 行状态无效：$($row.Status)")
        }
    }
    [pscustomobject]@{
        Valid = $errors.Count -eq 0
        Errors = @($errors)
        Path = $path
        FullSha256 = $fullSha
        DataSha256 = $dataSha
        Status = $derivedStatus
        RowCount = $rowCount
        MissingCount = $missingCount
        InconclusiveCount = $inconclusiveCount
        Rows = @($rows)
    }
}

function Get-RuntimeTerminalEvidence {
    <# 校验一次运行、一次扫描和任务终态；不要求持久任务历史。 #>
    param(
        [Parameter(Mandatory)] [object[]] $RuntimeRecords,
        [Parameter(Mandatory)] [object[]] $RuntimeSamples
    )

    $errors = [Collections.Generic.List[string]]::new()
    $failures = [Collections.Generic.List[string]]::new()
    $results = @($RuntimeRecords | Where-Object {
            [string](Get-OptionalProperty -Object $_ -Name 'record_type') -eq 'runtime_result'
        })
    if ($results.Count -ne 1) {
        [void]$errors.Add("runtime_result 数量应为1，实际=$($results.Count)")
    }
    $result = if ($results.Count -gt 0) { $results[-1] } else { $null }
    $scanTasks = Get-PropertyArray -Object $result -Name 'scan_tasks'
    if ($scanTasks.Count -ne 1) {
        [void]$errors.Add("单次运行必须包含1个扫描终态记录，实际=$($scanTasks.Count)")
    }
    $scansStarted = Get-IntegerOrNull (Get-OptionalProperty -Object $result -Name 'scans_started')
    if ($null -eq $scansStarted) {
        [void]$errors.Add('runtime_result 缺少 scans_started')
    }
    elseif ($scansStarted -ne 1) {
        [void]$failures.Add("本次运行启动了 $scansStarted 个扫描，要求为1")
    }
    $singleRun = Get-OptionalProperty -Object $result -Name 'single_run'
    if ($null -eq $singleRun) {
        [void]$errors.Add('runtime_result 缺少 single_run')
    }
    elseif ([bool]$singleRun -ne $true) {
        [void]$failures.Add('runtime_result 未声明单次终态模式')
    }
    $terminalState = if ($scanTasks.Count -eq 1) {
        [string](Get-OptionalProperty -Object $scanTasks[0] -Name 'terminal_state')
    }
    else { '' }
    if ($terminalState -notin @('completed', 'failed', 'cancelled')) {
        [void]$errors.Add("任务终态缺失或无效：$terminalState")
    }
    elseif ($terminalState -ne 'completed') {
        [void]$failures.Add("任务终态为 $terminalState，不是 completed")
    }
    $terminalSamples = @($RuntimeSamples | Where-Object {
            [string](Get-OptionalProperty -Object $_ -Name 'state') -in @('completed', 'failed', 'cancelled')
        })
    if ($terminalSamples.Count -eq 0) {
        [void]$errors.Add('终态样本缺失：runtime_sample 没有任务终态样本；资源汇总使用最后一条运行样本，不进行终态归零判定')
    }
    elseif ($terminalState -and [string](Get-OptionalProperty -Object $terminalSamples[-1] -Name 'state') -ne $terminalState) {
        [void]$errors.Add('runtime_sample 与 scan_tasks 终态不一致')
    }
    $failedScans = Get-IntegerOrNull (Get-OptionalProperty -Object $result -Name 'failed_scans')
    if ($null -eq $failedScans) {
        [void]$errors.Add('runtime_result 缺少 failed_scans')
    }
    elseif ($failedScans -gt 0) {
        [void]$failures.Add("runtime_result failed_scans=$failedScans")
    }
    $correctness = [string](Get-OptionalProperty -Object $result -Name 'correctness')
    if ($correctness -eq 'FAIL') {
        [void]$failures.Add('runtime_result correctness=FAIL')
    }
    elseif ($correctness -in @('MISSING', 'INCONCLUSIVE', '')) {
        [void]$errors.Add("runtime_result correctness=$correctness")
    }
    elseif ($correctness -ne 'PASS') {
        [void]$errors.Add("runtime_result correctness 无效：$correctness")
    }
    [pscustomobject]@{
        Result = $result
        ScanTasks = @($scanTasks)
        TerminalSamples = @($terminalSamples)
        TerminalSample = if ($terminalSamples.Count -gt 0) { $terminalSamples[-1] } else { $null }
        ResourceSample = if ($terminalSamples.Count -gt 0) { $terminalSamples[-1] } elseif ($RuntimeSamples.Count -gt 0) { $RuntimeSamples[-1] } else { $null }
        TerminalSampleMissing = $terminalSamples.Count -eq 0
        TerminalState = $terminalState
        FailedScans = $failedScans
        Correctness = $correctness
        SingleRun = [bool]$singleRun
        Errors = @($errors)
        Failures = @($failures)
    }
}

function Get-MediaAndDiskMapEvidence {
    <# 校验 H/I 媒体根前后快照、物理盘编号和 Worker 根覆盖。 #>
    param(
        [Parameter(Mandatory)] $Before,
        [Parameter(Mandatory)] $After,
        [Parameter(Mandatory)] $Harness,
        [Parameter(Mandatory)] $RuntimeResult,
        [Parameter(Mandatory)] [object[]] $RuntimeSamples,
        [Parameter(Mandatory)] [string] $EvidenceRoot
    )

    $errors = [Collections.Generic.List[string]]::new()
    $failures = [Collections.Generic.List[string]]::new()
    $beforeRoots = Get-PropertyArray -Object $Before -Name 'Roots'
    $afterRoots = Get-PropertyArray -Object $After -Name 'Roots'
    $harnessRoots = Get-PropertyArray -Object $Harness -Name 'media_roots'
    $runtimeRoots = Get-PropertyArray -Object $RuntimeResult -Name 'media_roots'
    if ($beforeRoots.Count -ne 2 -or $afterRoots.Count -ne 2 -or $harnessRoots.Count -ne 2 -or $runtimeRoots.Count -ne 2) {
        [void]$errors.Add('必须覆盖两个媒体根（H:\pik\00000000000 与 I:\tmp）')
    }
    foreach ($source in @(
            [pscustomobject]@{ Name = 'media-before'; Roots = $beforeRoots },
            [pscustomobject]@{ Name = 'media-after'; Roots = $afterRoots },
            [pscustomobject]@{ Name = 'harness'; Roots = $harnessRoots },
            [pscustomobject]@{ Name = 'runtime-result'; Roots = $runtimeRoots })) {
        if (-not (Test-ApprovedMediaRoots -Roots $source.Roots)) {
            [void]$errors.Add("$($source.Name) 媒体根必须精确绑定为 H:\pik\00000000000 与 I:\tmp")
        }
    }
    for ($index = 0; $index -lt [Math]::Min(2, $beforeRoots.Count); $index++) {
        if ($afterRoots.Count -le $index -or $harnessRoots.Count -le $index -or $runtimeRoots.Count -le $index -or
            -not (Test-WindowsPathEqual -Left ([string]$beforeRoots[$index]) -Right ([string]$afterRoots[$index])) -or
            -not (Test-WindowsPathEqual -Left ([string]$beforeRoots[$index]) -Right ([string]$harnessRoots[$index])) -or
            -not (Test-WindowsPathEqual -Left ([string]$beforeRoots[$index]) -Right ([string]$runtimeRoots[$index]))) {
            [void]$errors.Add("媒体根绑定不一致：第 $($index + 1) 根")
        }
    }
    $mediaUnchanged = [bool](Get-OptionalProperty -Object $Harness -Name 'media_unchanged')
    if (-not (Test-JsonEquivalent -Left $Before -Right $After) -or -not $mediaUnchanged) {
        [void]$failures.Add('真实媒体清单发生变化')
    }
    $mapPathValue = [string](Get-OptionalProperty -Object $Harness -Name 'physical_disk_map_path')
    $map = $null
    $mapPath = ''
    $mapShaMatches = $false
    $entries = @()
    if ([string]::IsNullOrWhiteSpace($mapPathValue)) {
        [void]$errors.Add('缺少 physical_disk_map_path')
    }
    else {
        try { $mapPath = [IO.Path]::GetFullPath($mapPathValue) } catch { $mapPath = '' }
        if ([string]::IsNullOrWhiteSpace($mapPath) -or
            -not $mapPath.StartsWith(($evidenceAbsolute + '\'), [StringComparison]::OrdinalIgnoreCase)) {
            [void]$errors.Add('物理盘映射路径必须位于 evidence 内')
        }
        elseif (-not (Test-Path -LiteralPath $mapPath -PathType Leaf)) {
            [void]$errors.Add('物理盘映射文件缺失')
        }
        else {
            try { $map = [IO.File]::ReadAllText($mapPath) | ConvertFrom-Json } catch { [void]$errors.Add('物理盘映射 JSON 无效') }
            $entries = Get-PropertyArray -Object $map -Name 'entries'
            if ([string](Get-OptionalProperty -Object $map -Name 'schema') -ne 'rust-v2-physical-disk-map/v1' -or $entries.Count -ne 2) {
                [void]$errors.Add('物理盘映射 schema 或根数量无效')
            }
            $diskNumbers = @()
            foreach ($entry in @($entries)) {
                $root = [string](Get-OptionalProperty -Object $entry -Name 'root')
                $diskNumber = Get-IntegerOrNull (Get-OptionalProperty -Object $entry -Name 'disk_number')
                $drive = [string](Get-OptionalProperty -Object $entry -Name 'drive_letter')
                if ([string]::IsNullOrWhiteSpace($root) -or $null -eq $diskNumber -or [string]::IsNullOrWhiteSpace($drive)) {
                    [void]$errors.Add('物理盘映射条目字段缺失')
                }
                if (@($beforeRoots | Where-Object { Test-WindowsPathEqual -Left ([string]$_) -Right $root }).Count -eq 0) {
                    [void]$errors.Add("物理盘映射根未绑定：$root")
                }
                $diskNumbers += [string]$diskNumber
            }
            if (@($diskNumbers | Sort-Object -Unique).Count -ne 2) {
                [void]$errors.Add('两个媒体根没有映射到两个不同物理盘')
            }
            $declaredDistinct = @((Get-PropertyArray -Object $map -Name 'distinct_disk_numbers') | ForEach-Object { [string]$_ } | Sort-Object)
            if (($declaredDistinct -join ',') -cne (($diskNumbers | Sort-Object) -join ',')) {
                [void]$errors.Add('distinct_disk_numbers 与条目不一致')
            }
            $expectedMapSha = [string](Get-OptionalProperty -Object $Harness -Name 'physical_disk_map_sha256')
            if ($expectedMapSha -match '^[0-9a-fA-F]{64}$') {
                $mapShaMatches = ((Get-Sha256Bytes -Bytes ([IO.File]::ReadAllBytes($mapPath))) -ceq $expectedMapSha.ToLowerInvariant())
                if (-not $mapShaMatches) { [void]$errors.Add('物理盘映射 SHA 不一致') }
            }
            else { [void]$errors.Add('物理盘映射 SHA 缺失或无效') }
        }
    }
    $observedRoots = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    foreach ($sample in @($RuntimeSamples)) {
        foreach ($worker in @(Get-PropertyArray -Object $sample -Name 'workers')) {
            $phase = [string](Get-OptionalProperty -Object $worker -Name 'phase')
            if ($phase -eq 'idle') { continue }
            $path = [string](Get-OptionalProperty -Object $worker -Name 'display_path')
            for ($index = 0; $index -lt $beforeRoots.Count; $index++) {
                if (Test-WindowsPathWithin -Candidate $path -Root ([string]$beforeRoots[$index])) {
                    [void]$observedRoots.Add([string]$beforeRoots[$index])
                }
            }
        }
    }
    foreach ($root in @($beforeRoots)) {
        if (-not $observedRoots.Contains([string]$root)) {
            [void]$errors.Add("Worker 未观察到媒体根：$root")
        }
    }
    [pscustomobject]@{
        Errors = @($errors)
        Failures = @($failures)
        BeforeRoots = @($beforeRoots)
        AfterRoots = @($afterRoots)
        Entries = @($entries)
        MapPath = $mapPath
        MapShaMatches = $mapShaMatches
        ObservedRoots = @($observedRoots | Sort-Object)
        MediaUnchanged = $mediaUnchanged
    }
}

function Get-ConfigurationEvidence {
    <# 校验 Everything、Worker20/read12 和 Node 实际执行配置指纹。 #>
    param(
        [Parameter(Mandatory)] $Harness,
        [Parameter(Mandatory)] [object[]] $RuntimeSamples
    )

    $errors = [Collections.Generic.List[string]]::new()
    $failures = [Collections.Generic.List[string]]::new()
    $enumerator = [string](Get-OptionalProperty -Object $Harness -Name 'enumerator')
    if ([string]::IsNullOrWhiteSpace($enumerator)) {
        [void]$errors.Add('缺少显式枚举器配置')
    }
    elseif ($enumerator.ToLowerInvariant() -ne 'everything') {
        [void]$failures.Add("枚举器不是 Everything：$enumerator")
    }
    $completion = Get-OptionalProperty -Object $Harness -Name 'complete_when_task_terminal'
    if ($null -eq $completion) {
        [void]$errors.Add('缺少任务终态完成模式')
    }
    elseif (-not [bool]$completion) {
        [void]$failures.Add('未启用任务终态即完成模式')
    }
    $expectedWorkers = Get-IntegerOrNull (Get-OptionalProperty -Object $Harness -Name 'effective_worker_count')
    $expectedRead = Get-IntegerOrNull (Get-OptionalProperty -Object $Harness -Name 'read_total_threads')
    $expectedHdd = Get-IntegerOrNull (Get-OptionalProperty -Object $Harness -Name 'hdd_threads_per_disk')
    $expectedSsd = Get-IntegerOrNull (Get-OptionalProperty -Object $Harness -Name 'ssd_threads_per_disk')
    $expectedUnknown = Get-IntegerOrNull (Get-OptionalProperty -Object $Harness -Name 'unknown_threads_per_disk')
    if ($null -eq $expectedWorkers -or $null -eq $expectedRead -or $null -eq $expectedHdd -or
        $null -eq $expectedSsd -or $null -eq $expectedUnknown) {
        [void]$errors.Add('Worker/读取线程/磁盘分类配置缺失')
    }
    else {
        if ($expectedWorkers -ne 20) { [void]$failures.Add("Worker 配置应为20，实际=$expectedWorkers") }
        if ($expectedRead -ne 12) { [void]$failures.Add("读取线程配置应为12，实际=$expectedRead") }
    }
    $configs = @($RuntimeSamples | ForEach-Object { Get-OptionalProperty -Object $_ -Name 'execution_config' } |
        Where-Object { $null -ne $_ })
    if ($configs.Count -eq 0) {
        [void]$errors.Add('缺少 Node 实际 execution_config')
    }
    $actual = if ($configs.Count -gt 0) { $configs[-1] } else { $null }
    $actualWorkers = Get-IntegerOrNull (Get-OptionalProperty -Object $actual -Name 'worker_slots')
    $actualRead = Get-IntegerOrNull (Get-OptionalProperty -Object $actual -Name 'global_disk_permits')
    $actualHdd = Get-IntegerOrNull (Get-OptionalProperty -Object $actual -Name 'hdd_per_disk_permits')
    $actualSsd = Get-IntegerOrNull (Get-OptionalProperty -Object $actual -Name 'ssd_per_disk_permits')
    $actualUnknown = Get-IntegerOrNull (Get-OptionalProperty -Object $actual -Name 'unknown_per_disk_permits')
    if ($configs.Count -gt 0) {
        if ($null -eq $actualWorkers -or $null -eq $actualRead -or $null -eq $actualHdd -or
            $null -eq $actualSsd -or $null -eq $actualUnknown) {
            [void]$errors.Add('execution_config 缺少 Worker/读取线程实际字段')
        }
        elseif ($actualWorkers -ne $expectedWorkers -or $actualRead -ne $expectedRead -or
            $actualHdd -ne $expectedHdd -or $actualSsd -ne $expectedSsd -or $actualUnknown -ne $expectedUnknown) {
            [void]$failures.Add("Node 实际配置不匹配：Worker=$actualWorkers/read=$actualRead/HDD=$actualHdd/SSD=$actualSsd/unknown=$actualUnknown")
        }
    }
    [pscustomobject]@{
        Errors = @($errors)
        Failures = @($failures)
        Enumerator = $enumerator
        CompleteWhenTaskTerminal = [bool]$completion
        ExpectedWorkers = $expectedWorkers
        ExpectedRead = $expectedRead
        ExpectedHdd = $expectedHdd
        ExpectedSsd = $expectedSsd
        ExpectedUnknown = $expectedUnknown
        Actual = $actual
    }
}

function Get-PipelineEvidence {
    <# 汇总队列、资源和 ownership，并检查容量、守恒与终态归零。 #>
    param(
        [Parameter(Mandatory)] [object[]] $RuntimeSamples,
        [Parameter(Mandatory)] [AllowNull()] $TerminalSample,
        [bool] $TerminalSampleIsActual = $true
    )

    $errors = [Collections.Generic.List[string]]::new()
    $failures = [Collections.Generic.List[string]]::new()
    $pipelines = @($RuntimeSamples | ForEach-Object { Get-OptionalProperty -Object $_ -Name 'pipeline_metrics' } |
        Where-Object { $null -ne $_ })
    $definitions = @(
        [pscustomobject]@{ Name = 'Hash队列'; Field = 'hash_queue'; Kind = '队列' }
        [pscustomobject]@{ Name = '路径缓存队列'; Field = 'path_cache_queue'; Kind = '队列' }
        [pscustomobject]@{ Name = '内容缓存队列'; Field = 'content_cache_queue'; Kind = '队列' }
        [pscustomobject]@{ Name = '待解码队列'; Field = 'decode_queue'; Kind = '队列' }
        [pscustomobject]@{ Name = '持久化队列'; Field = 'persist_queue'; Kind = '队列' }
        [pscustomobject]@{ Name = 'Hash磁盘许可'; Field = 'hash_io'; Kind = '资源' }
        [pscustomobject]@{ Name = '媒体磁盘许可'; Field = 'media_io'; Kind = '资源' }
        [pscustomobject]@{ Name = 'CPU权重'; Field = 'cpu_weight'; Kind = '资源' }
        [pscustomobject]@{ Name = 'Worker槽'; Field = 'worker_slots'; Kind = '资源' }
    )
    $metricLines = [Collections.Generic.List[string]]::new()
    $capacityViolations = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    foreach ($definition in $definitions) {
        $rows = @($pipelines | ForEach-Object { Get-OptionalProperty -Object $_ -Name $definition.Field } |
            Where-Object { $null -ne $_ })
        if ($rows.Count -eq 0) {
            [void]$errors.Add("缺少流水线字段：$($definition.Field)")
            [void]$metricLines.Add("| $($definition.Name) | $($definition.Kind) | — | — | — | — | — |")
            continue
        }
        $currents = @($rows | ForEach-Object { Get-NumberOrNull (Get-OptionalProperty -Object $_ -Name 'current') } | Where-Object { $null -ne $_ })
        $peaks = @($rows | ForEach-Object { Get-NumberOrNull (Get-OptionalProperty -Object $_ -Name 'peak') } | Where-Object { $null -ne $_ })
        $capacities = @($rows | ForEach-Object { Get-NumberOrNull (Get-OptionalProperty -Object $_ -Name 'capacity') } | Where-Object { $null -ne $_ })
        if ($currents.Count -eq 0 -or $peaks.Count -eq 0 -or $capacities.Count -eq 0) {
            [void]$errors.Add("流水线字段数值缺失：$($definition.Field)")
        }
        $currentMax = if ($currents.Count -gt 0) { ($currents | Measure-Object -Maximum).Maximum } else { $null }
        $peakMax = if ($peaks.Count -gt 0) { ($peaks | Measure-Object -Maximum).Maximum } else { $null }
        $capacity = if ($capacities.Count -gt 0) { ($capacities | Measure-Object -Maximum).Maximum } else { $null }
        foreach ($row in @($rows)) {
            $current = Get-NumberOrNull (Get-OptionalProperty -Object $row -Name 'current')
            $peak = Get-NumberOrNull (Get-OptionalProperty -Object $row -Name 'peak')
            $rowCapacity = Get-NumberOrNull (Get-OptionalProperty -Object $row -Name 'capacity')
            if ($null -ne $rowCapacity -and (($null -ne $current -and $current -gt $rowCapacity) -or
                    ($null -ne $peak -and $peak -gt $rowCapacity))) {
                [void]$capacityViolations.Add($definition.Name)
            }
        }
        $waitP95 = @($rows | ForEach-Object { Get-OptionalProperty -Object (Get-OptionalProperty -Object $_ -Name 'wait_latency') -Name 'p95_ms' } |
            ForEach-Object { Get-NumberOrNull $_ } | Where-Object { $null -ne $_ })
        $serviceP95 = @($rows | ForEach-Object { Get-OptionalProperty -Object (Get-OptionalProperty -Object $_ -Name 'service_latency') -Name 'p95_ms' } |
            ForEach-Object { Get-NumberOrNull $_ } | Where-Object { $null -ne $_ })
        $wait = if ($waitP95.Count -gt 0) { ($waitP95 | Measure-Object -Maximum).Maximum } else { $null }
        $service = if ($serviceP95.Count -gt 0) { ($serviceP95 | Measure-Object -Maximum).Maximum } else { $null }
        [void]$metricLines.Add("| $($definition.Name) | $($definition.Kind) | $(Format-Optional $currentMax) | $(Format-Optional $peakMax) | $(Format-Optional $capacity) | $(Format-Optional $wait ' ms') | $(Format-Optional $service ' ms') |")
    }
    if ($pipelines.Count -eq 0) { [void]$errors.Add('缺少 pipeline_metrics') }

    $ownershipFields = @(
        'hash_waiting_permit', 'hash_reading', 'hash_completed_unjoined', 'media_permit_waiting',
        'media_acquire_ready', 'media_permit_ready', 'worker_dispatching', 'worker_start_pending',
        'worker_decode', 'worker_feature', 'worker_result_wait', 'worker_phase_unknown',
        'content_output_credit_owned', 'hash_refill_token_available', 'decode_credit_owned'
    )
    $ownershipDisplays = @{
        hash_waiting_permit = 'Hash等待许可'; hash_reading = 'Hash读取中'; hash_completed_unjoined = 'Hash待归并'
        media_permit_waiting = '媒体许可等待'; media_acquire_ready = '媒体获取就绪'; media_permit_ready = '媒体许可就绪'
        worker_dispatching = 'Worker派发中'; worker_start_pending = 'Worker待Started'; worker_decode = 'Worker解码'
        worker_feature = 'Worker特征'; worker_result_wait = 'Worker结果等待'; worker_phase_unknown = 'Worker未知阶段'
        content_output_credit_owned = 'Content输出credit'; hash_refill_token_available = 'Hash refill token'
        decode_credit_owned = 'Decode credit'
    }
    $ownershipMissing = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    $ownershipCapacityViolations = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    $ownershipCurrentMax = @{}
    $ownershipPeakMax = @{}
    foreach ($field in $ownershipFields) {
        $rows = @($pipelines | ForEach-Object { Get-OptionalProperty -Object $_ -Name $field } |
            Where-Object { $null -ne $_ })
        if ($rows.Count -eq 0) {
            [void]$ownershipMissing.Add($field)
            continue
        }
        $currentValues = @()
        $peakValues = @()
        foreach ($row in @($rows)) {
            $current = Get-NumberOrNull (Get-OptionalProperty -Object $row -Name 'current')
            $peak = Get-NumberOrNull (Get-OptionalProperty -Object $row -Name 'peak')
            $capacity = Get-NumberOrNull (Get-OptionalProperty -Object $row -Name 'capacity')
            if ($null -eq $current -or $null -eq $peak -or $null -eq $capacity) {
                [void]$ownershipMissing.Add($field)
            }
            else {
                $currentValues += $current
                $peakValues += $peak
                if ($current -gt $capacity -or $peak -gt $capacity) { [void]$ownershipCapacityViolations.Add($field) }
            }
        }
        $ownershipCurrentMax[$field] = if ($currentValues.Count -gt 0) { ($currentValues | Measure-Object -Maximum).Maximum } else { $null }
        $ownershipPeakMax[$field] = if ($peakValues.Count -gt 0) { ($peakValues | Measure-Object -Maximum).Maximum } else { $null }
    }
    $latencyRows = @($pipelines | ForEach-Object { Get-OptionalProperty -Object $_ -Name 'item_completion_latency' } |
        Where-Object { $null -ne $_ })
    if ($latencyRows.Count -eq 0) { [void]$ownershipMissing.Add('item_completion_latency') }

    $hashBalanceBad = $false
    $mediaSubsetBad = $false
    $activeBalanceBad = $false
    foreach ($sample in @($RuntimeSamples)) {
        $pipeline = Get-OptionalProperty -Object $sample -Name 'pipeline_metrics'
        if ($null -eq $pipeline) { continue }
        $hashQueue = Get-NumberOrNull (Get-OptionalProperty -Object (Get-OptionalProperty -Object $pipeline -Name 'hash_queue') -Name 'current')
        $hashSum = 0.0
        $hashComplete = $true
        foreach ($field in @('hash_waiting_permit', 'hash_reading', 'hash_completed_unjoined')) {
            $value = Get-NumberOrNull (Get-OptionalProperty -Object (Get-OptionalProperty -Object $pipeline -Name $field) -Name 'current')
            if ($null -eq $value) { $hashComplete = $false } else { $hashSum += $value }
        }
        if ($hashComplete -and $null -ne $hashQueue -and $hashSum -ne $hashQueue) { $hashBalanceBad = $true }
        $mediaReady = Get-NumberOrNull (Get-OptionalProperty -Object (Get-OptionalProperty -Object $pipeline -Name 'media_acquire_ready') -Name 'current')
        $mediaPermitReady = Get-NumberOrNull (Get-OptionalProperty -Object (Get-OptionalProperty -Object $pipeline -Name 'media_permit_ready') -Name 'current')
        if ($null -ne $mediaReady -and $null -ne $mediaPermitReady -and $mediaPermitReady -gt $mediaReady) { $mediaSubsetBad = $true }
        $activeSum = 0.0
        $activeComplete = $true
        foreach ($field in @('worker_start_pending', 'worker_decode', 'worker_feature', 'worker_result_wait', 'worker_phase_unknown')) {
            $value = Get-NumberOrNull (Get-OptionalProperty -Object (Get-OptionalProperty -Object $pipeline -Name $field) -Name 'current')
            if ($null -eq $value) { $activeComplete = $false } else { $activeSum += $value }
        }
        if ($activeComplete -and $null -ne $sample.PSObject.Properties['workers']) {
            $activeWorkers = @(Get-PropertyArray -Object $sample -Name 'workers' | Where-Object {
                    [string](Get-OptionalProperty -Object $_ -Name 'phase') -ne 'idle'
                }).Count
            if ($activeSum -ne $activeWorkers) { $activeBalanceBad = $true }
        }
    }
    if ($hashBalanceBad) { [void]$failures.Add('Hash ownership 与 Hash 队列不守恒') }
    if ($mediaSubsetBad) { [void]$failures.Add('media_permit_ready 超过 media_acquire_ready') }
    if ($activeBalanceBad) { [void]$failures.Add('Worker 阶段 ownership 与活动 Worker 数不守恒') }
    foreach ($name in @($ownershipCapacityViolations)) { [void]$failures.Add("ownership 容量越界：$name") }
    $terminalNonZero = [Collections.Generic.List[string]]::new()
    $terminalToken = $null
    if ($TerminalSampleIsActual -and $null -ne $TerminalSample) {
        $terminalPipeline = Get-OptionalProperty -Object $TerminalSample -Name 'pipeline_metrics'
        foreach ($field in @($ownershipFields | Where-Object { $_ -ne 'hash_refill_token_available' })) {
            $value = Get-NumberOrNull (Get-OptionalProperty -Object (Get-OptionalProperty -Object $terminalPipeline -Name $field) -Name 'current')
            if ($null -ne $value -and $value -ne 0) { [void]$terminalNonZero.Add($field) }
        }
        $terminalToken = Get-NumberOrNull (Get-OptionalProperty -Object (Get-OptionalProperty -Object $terminalPipeline -Name 'hash_refill_token_available') -Name 'current')
        if ($terminalNonZero.Count -gt 0) { [void]$failures.Add("终态 ownership 未归零：$($terminalNonZero -join '、')") }
        if ($null -ne $terminalToken -and $terminalToken -ne 0) { [void]$failures.Add('终态 Hash refill token 未归零') }
    }
    [pscustomobject]@{
        Errors = @($errors)
        Failures = @($failures)
        Pipelines = @($pipelines)
        MetricLines = @($metricLines)
        CapacityViolations = @($capacityViolations)
        OwnershipMissing = @($ownershipMissing | Sort-Object)
        OwnershipCapacityViolations = @($ownershipCapacityViolations | Sort-Object)
        OwnershipCurrentMax = $ownershipCurrentMax
        OwnershipPeakMax = $ownershipPeakMax
        OwnershipFields = $ownershipFields
        OwnershipDisplays = $ownershipDisplays
        TerminalNonZero = @($terminalNonZero)
        TerminalToken = $terminalToken
        TerminalSampleIsActual = $TerminalSampleIsActual
        LatencyPresent = $latencyRows.Count -gt 0
    }
}

function Get-DiskReadEvidence {
    <# 按物理盘检查读取许可容量、累计 grant/release、终态归零和双盘重叠。 #>
    param(
        [Parameter(Mandatory)] [object[]] $RuntimeSamples,
        [Parameter(Mandatory)] [object[]] $Entries
    )

    $errors = [Collections.Generic.List[string]]::new()
    $failures = [Collections.Generic.List[string]]::new()
    $disks = [Collections.Generic.List[object]]::new()
    foreach ($entry in @($Entries)) {
        $number = Get-IntegerOrNull (Get-OptionalProperty -Object $entry -Name 'disk_number')
        if ($null -eq $number) { continue }
        [void]$disks.Add([pscustomobject]@{
                Id = "PhysicalDisk$number"; Capacity = $null; WaitingPeak = 0.0; ActivePeak = 0.0
                Granted = 0.0; Released = 0.0; Observed = $false; ActiveObserved = $false
                TerminalObserved = $false; Previous = @()
            })
    }
    $overlapObserved = $false
    foreach ($sample in @($RuntimeSamples)) {
        $pipeline = Get-OptionalProperty -Object $sample -Name 'pipeline_metrics'
        $rows = Get-PropertyArray -Object $pipeline -Name 'disk_reads'
        $byId = @{}
        foreach ($row in @($rows)) {
            $id = [string](Get-OptionalProperty -Object $row -Name 'physical_disk_id')
            if (-not [string]::IsNullOrWhiteSpace($id)) { $byId[$id] = $row }
        }
        $activeCount = 0
        foreach ($disk in @($disks)) {
            if (-not $byId.ContainsKey($disk.Id)) {
                $state = [string](Get-OptionalProperty -Object $sample -Name 'state')
                if ($state -in @('completed', 'failed', 'cancelled')) {
                    [void]$errors.Add("终态缺少物理盘读取行：$($disk.Id)")
                }
                continue
            }
            $row = $byId[$disk.Id]
            $required = @('capacity', 'hash_waiting', 'media_waiting', 'hash_active', 'media_active',
                'hash_granted_total', 'media_granted_total', 'hash_released_total', 'media_released_total')
            $values = @{}
            $valid = $true
            foreach ($field in $required) {
                $values[$field] = Get-NumberOrNull (Get-OptionalProperty -Object $row -Name $field)
                if ($null -eq $values[$field]) { $valid = $false }
            }
            if (-not $valid) { [void]$errors.Add("物理盘读取字段缺失：$($disk.Id)"); continue }
            $disk.Observed = $true
            $waiting = [double]$values.hash_waiting + [double]$values.media_waiting
            $active = [double]$values.hash_active + [double]$values.media_active
            $granted = [double]$values.hash_granted_total + [double]$values.media_granted_total
            $released = [double]$values.hash_released_total + [double]$values.media_released_total
            $disk.Capacity = if ($null -eq $disk.Capacity) { $values.capacity } else { [Math]::Max($disk.Capacity, $values.capacity) }
            $disk.WaitingPeak = [Math]::Max($disk.WaitingPeak, $waiting)
            $disk.ActivePeak = [Math]::Max($disk.ActivePeak, $active)
            $disk.Granted = [Math]::Max($disk.Granted, $granted)
            $disk.Released = [Math]::Max($disk.Released, $released)
            if ($active -gt 0) { $disk.ActiveObserved = $true; $activeCount++ }
            if ($waiting -gt $values.capacity -or $active -gt $values.capacity) { [void]$failures.Add("$($disk.Id) 读取许可超过容量") }
            if ($released -gt $granted) { [void]$failures.Add("$($disk.Id) release 超过 grant") }
            $totals = @($values.hash_granted_total, $values.media_granted_total, $values.hash_released_total, $values.media_released_total)
            if ($disk.Previous.Count -gt 0) {
                for ($totalIndex = 0; $totalIndex -lt $totals.Count; $totalIndex++) {
                    if ($totals[$totalIndex] -lt $disk.Previous[$totalIndex]) { [void]$failures.Add("$($disk.Id) 累计值回退") }
                }
            }
            $disk.Previous = $totals
            if ([string](Get-OptionalProperty -Object $sample -Name 'state') -in @('completed', 'failed', 'cancelled')) {
                $disk.TerminalObserved = $true
                if ($waiting -ne 0 -or $active -ne 0 -or $released -ne $granted) {
                    [void]$failures.Add("$($disk.Id) 终态读取 ownership 未归零")
                }
            }
        }
        if ($activeCount -ge 2) { $overlapObserved = $true }
    }
    foreach ($disk in @($disks)) {
        if (-not $disk.Observed) { [void]$errors.Add("未观察到 $($disk.Id) 读取行") }
        if (-not $disk.ActiveObserved) { [void]$errors.Add("未观察到 $($disk.Id) 活动读取") }
        if (-not $disk.TerminalObserved) { [void]$errors.Add("未观察到 $($disk.Id) 终态读取行") }
    }
    if ($disks.Count -lt 2) { [void]$errors.Add('物理盘读取证据少于两个盘') }
    elseif (-not $overlapObserved) {
        if (@($disks | Where-Object { -not $_.ActiveObserved }).Count -gt 0) {
            [void]$errors.Add('至少一个物理盘没有活动读取，无法判断双盘重叠')
        }
        else { [void]$failures.Add('两个物理盘均有工作但没有同一采样点重叠读取') }
    }
    $tableLines = @($disks | Sort-Object Id | ForEach-Object {
            "| $($_.Id) | $(Format-Optional $_.Capacity) | $($_.WaitingPeak) | $($_.ActivePeak) | $($_.Granted) | $($_.Released) |"
        })
    [pscustomobject]@{ Errors = @($errors); Failures = @($failures); Disks = @($disks); OverlapObserved = $overlapObserved; TableLines = $tableLines }
}

function Get-WorkerAndSystemSummary {
    <# 汇总 Worker 阶段、CPU/内存、磁盘吞吐和文件级失败，不推断协议未提供的计数。 #>
    param(
        [Parameter(Mandatory)] [object[]] $RuntimeSamples,
        [Parameter(Mandatory)] [object[]] $SystemSamples
    )

    $workerRows = @($RuntimeSamples | ForEach-Object { Get-PropertyArray -Object $_ -Name 'workers' })
    $activeWorkers = @($workerRows | Where-Object { [string](Get-OptionalProperty -Object $_ -Name 'phase') -ne 'idle' })
    $workerCounts = @($RuntimeSamples | ForEach-Object {
            @(Get-PropertyArray -Object $_ -Name 'workers' | Where-Object { [string](Get-OptionalProperty -Object $_ -Name 'phase') -ne 'idle' }).Count
        })
    $peakWorkers = if ($workerCounts.Count -gt 0) { [int](($workerCounts | Measure-Object -Maximum).Maximum) } else { 0 }
    $averageWorkers = if ($workerCounts.Count -gt 0) { [double](($workerCounts | Measure-Object -Average).Average) } else { 0.0 }
    $workerDisks = @($activeWorkers | ForEach-Object { [string](Get-OptionalProperty -Object $_ -Name 'physical_disk_id') } |
        Where-Object { -not [string]::IsNullOrWhiteSpace($_) } | Sort-Object -Unique)
    $phaseLines = @($activeWorkers | Group-Object { [string](Get-OptionalProperty -Object $_ -Name 'phase') } | Sort-Object Name |
        ForEach-Object {
            $cpu = @($_.Group | ForEach-Object { Get-NumberOrNull (Get-OptionalProperty -Object $_ -Name 'cpu_weight') } | Where-Object { $null -ne $_ })
            $decoder = @($_.Group | ForEach-Object { Get-NumberOrNull (Get-OptionalProperty -Object $_ -Name 'decoder_threads') } | Where-Object { $null -ne $_ })
            $cpuMax = if ($cpu.Count -gt 0) { ($cpu | Measure-Object -Maximum).Maximum } else { $null }
            $decoderMax = if ($decoder.Count -gt 0) { ($decoder | Measure-Object -Maximum).Maximum } else { $null }
            "| $($_.Name) | $($_.Count) | $(Format-Optional $cpuMax) | $(Format-Optional $decoderMax) |"
        })
    if ($phaseLines.Count -eq 0) { $phaseLines = @('| — | 0 | — | — |') }
    $processRows = @($SystemSamples | ForEach-Object { Get-PropertyArray -Object $_ -Name 'processes' })
    $processLines = @($processRows | Group-Object { [string](Get-OptionalProperty -Object $_ -Name 'Name') } | Sort-Object Name |
        ForEach-Object {
            $cpu = @($_.Group | ForEach-Object { Get-NumberOrNull (Get-OptionalProperty -Object $_ -Name 'CpuDeltaMs') } | Where-Object { $null -ne $_ })
            $working = @($_.Group | ForEach-Object { Get-NumberOrNull (Get-OptionalProperty -Object $_ -Name 'WorkingSetBytes') } | Where-Object { $null -ne $_ })
            $private = @($_.Group | ForEach-Object { Get-NumberOrNull (Get-OptionalProperty -Object $_ -Name 'PrivateMemoryBytes') } | Where-Object { $null -ne $_ })
            $averageCpu = if ($cpu.Count -gt 0) { ($cpu | Measure-Object -Average).Average } else { 0 }
            $workingPeak = if ($working.Count -gt 0) { ($working | Measure-Object -Maximum).Maximum } else { 0 }
            $privatePeak = if ($private.Count -gt 0) { ($private | Measure-Object -Maximum).Maximum } else { 0 }
            "| $($_.Name) | $('{0:N2}' -f $averageCpu) | $(Format-Bytes $workingPeak) | $(Format-Bytes $privatePeak) |"
        })
    if ($processLines.Count -eq 0) { $processLines = @('| — | 0 | — | — |') }
    $diskRows = @($SystemSamples | ForEach-Object { Get-PropertyArray -Object $_ -Name 'disks' })
    $diskLines = @($diskRows | Group-Object { [string](Get-OptionalProperty -Object $_ -Name 'Name') } | Sort-Object Name |
        ForEach-Object {
            $read = @($_.Group | ForEach-Object { Get-NumberOrNull (Get-OptionalProperty -Object $_ -Name 'DiskReadBytesPerSec') } | Where-Object { $null -ne $_ })
            $queue = @($_.Group | ForEach-Object { Get-NumberOrNull (Get-OptionalProperty -Object $_ -Name 'AvgDiskQueueLength') } | Where-Object { $null -ne $_ })
            $avgRead = if ($read.Count -gt 0) { ($read | Measure-Object -Average).Average } else { 0 }
            $maxRead = if ($read.Count -gt 0) { ($read | Measure-Object -Maximum).Maximum } else { 0 }
            $maxQueue = if ($queue.Count -gt 0) { ($queue | Measure-Object -Maximum).Maximum } else { 0 }
            "| $($_.Name) | $(Format-Bytes $avgRead)/s | $(Format-Bytes $maxRead)/s | $('{0:N2}' -f $maxQueue) |"
        })
    if ($diskLines.Count -eq 0) { $diskLines = @('| — | — | — | — |') }
    $failures = @($RuntimeSamples | ForEach-Object { Get-PropertyArray -Object $_ -Name 'failures' } |
        Where-Object { $null -ne $_ -and $null -ne $_.PSObject.Properties['message'] })
    # Runtime 快照会重复携带最近失败；按阶段、完整路径和消息组成的身份去重，全部列出而不截断。
    $failureGroups = @($failures | Group-Object {
            "$(Get-OptionalProperty -Object $_ -Name 'stage_id') / $(Get-OptionalProperty -Object $_ -Name 'display_path') / $(Get-OptionalProperty -Object $_ -Name 'message')"
        } | Sort-Object Name)
    $failureLines = @($failureGroups | ForEach-Object { "- $($_.Name)（在 $($_.Count) 条快照中出现）" })
    if ($failureLines.Count -eq 0) { $failureLines = @('- 本次未记录文件级失败。') }
    $physicalFaults = @($failureGroups | Where-Object {
            [string](Get-OptionalProperty -Object $_.Group[0] -Name 'message') -match '物理|timeout|超时|读取'
        }).Count
    $workerCrashes = @($failureGroups | Where-Object {
            [string](Get-OptionalProperty -Object $_.Group[0] -Name 'message') -match 'Worker|崩溃|exit'
        }).Count
    $skipCount = @($SystemSamples | ForEach-Object { Get-PropertyArray -Object $_ -Name 'process_sample_skips' }).Count
    [pscustomobject]@{
        ActiveWorkers = @($activeWorkers); PeakWorkers = $peakWorkers; AverageWorkers = $averageWorkers
        WorkerDisks = $workerDisks; PhaseLines = $phaseLines; ProcessLines = $processLines; DiskLines = $diskLines
        FailureLines = $failureLines; PhysicalFaults = $physicalFaults; WorkerCrashes = $workerCrashes; ProcessSampleSkips = $skipCount
    }
}

function Join-ReasonLines {
    <# 将原因集合渲染为稳定的 Markdown 列表。 #>
    param([object[]] $Reasons, [string] $EmptyText = '- 无。')

    if (@($Reasons).Count -eq 0) { return $EmptyText }
    @($Reasons | Sort-Object -Unique | ForEach-Object { "- $_" }) -join "`n"
}

$runtimeRead = Read-NdjsonEvidence -Path (Join-Path $EvidenceRoot 'runtime.ndjson')
$systemRead = Read-NdjsonEvidence -Path (Join-Path $EvidenceRoot 'system.ndjson')
$beforeRead = Read-JsonEvidence -Path (Join-Path $EvidenceRoot 'media-before.json')
$afterRead = Read-JsonEvidence -Path (Join-Path $EvidenceRoot 'media-after.json')
$harnessRead = Read-JsonEvidence -Path (Join-Path $EvidenceRoot 'harness-result.json')
$runtimeRecords = @($runtimeRead.Records)
$systemSamples = @($systemRead.Records | Where-Object { [string](Get-OptionalProperty -Object $_ -Name 'record_type') -eq 'system_sample' })
$runtimeSamples = @($runtimeRecords | Where-Object { [string](Get-OptionalProperty -Object $_ -Name 'record_type') -eq 'runtime_sample' })
$runtimeResultRecord = @($runtimeRecords | Where-Object { [string](Get-OptionalProperty -Object $_ -Name 'record_type') -eq 'runtime_result' }) | Select-Object -Last 1
$failReasons = [Collections.Generic.List[string]]::new()
$inconclusiveReasons = [Collections.Generic.List[string]]::new()
$allReadErrors = @($runtimeRead.Errors + $systemRead.Errors + $beforeRead.Errors + $afterRead.Errors + $harnessRead.Errors)
$allReadErrors | ForEach-Object { [void]$inconclusiveReasons.Add([string]$_) }
$summary = $null
$terminal = $null
$media = $null
$config = $null
$pipeline = $null
$diskRead = $null
$system = $null
$runtimeCoverage = $null
$systemCoverage = $null
$harness = $harnessRead.Value
$before = $beforeRead.Value
$after = $afterRead.Value
$runtimeResult = $runtimeResultRecord

try {
    if (-not $runtimeRead.Valid -or -not $systemRead.Valid -or -not $harnessRead.Valid -or
        -not $beforeRead.Valid -or -not $afterRead.Valid) {
        # 具体缺失原因已经在 allReadErrors 中列出；此处不再生成假数据。
    }
    else {
        $terminal = Get-RuntimeTerminalEvidence -RuntimeRecords $runtimeRecords -RuntimeSamples $runtimeSamples
        $terminal.Errors | ForEach-Object { [void]$inconclusiveReasons.Add([string]$_) }
        $terminal.Failures | ForEach-Object { [void]$failReasons.Add([string]$_) }
        $harnessRunStatus = [string](Get-OptionalProperty -Object $harness -Name 'run_status')
        if ([string]::IsNullOrWhiteSpace($harnessRunStatus)) {
            [void]$inconclusiveReasons.Add('harness 缺少 run_status')
        }
        elseif ($harnessRunStatus -eq 'FAIL') {
            [void]$failReasons.Add('harness run_status=FAIL')
        }
        elseif ($harnessRunStatus -eq 'INCONCLUSIVE') {
            [void]$inconclusiveReasons.Add('harness run_status=INCONCLUSIVE')
        }
        elseif ($harnessRunStatus -ne 'PASS') {
            [void]$inconclusiveReasons.Add("harness run_status 无效：$harnessRunStatus")
        }
        $nodeUnexpectedExit = Get-OptionalProperty -Object $harness -Name 'node_unexpected_exit'
        if ($null -eq $nodeUnexpectedExit) {
            [void]$inconclusiveReasons.Add('harness 缺少 node_unexpected_exit')
        }
        elseif ([bool]$nodeUnexpectedExit) {
            [void]$failReasons.Add('Node 或 Worker 发生非预期退出')
        }
        $exporterExitCode = Get-SignedIntegerOrNull (Get-OptionalProperty -Object $harness -Name 'exporter_exit_code')
        if ($null -eq $exporterExitCode) {
            [void]$inconclusiveReasons.Add('harness 缺少 exporter_exit_code')
        }
        elseif ($exporterExitCode -eq -1 -and $terminal.TerminalState -ne 'completed') {
            # -1 是 Measure 在任务未完成、因此未启动 exporter 时写入的哨兵值，不是子进程退出码。
            [void]$inconclusiveReasons.Add("结果 exporter 未执行：任务未完成（终态=$($terminal.TerminalState)）")
        }
        elseif ($exporterExitCode -ne 0) {
            [void]$inconclusiveReasons.Add("结果 exporter 未正常退出：$exporterExitCode")
        }
        if ($runtimeSamples.Count -gt 0) {
            $runtimeCoverage = Get-TimeCoverage -Samples $runtimeSamples -TargetMs 1000 -MaxGapMs 2500 -AllowedDriftMs 500 -Label '任务快照'
            $runtimeCoverage.Errors | ForEach-Object { [void]$inconclusiveReasons.Add([string]$_) }
        }
        else { [void]$inconclusiveReasons.Add('runtime_sample 缺失') }
        if ($systemSamples.Count -gt 0) {
            $systemCoverage = Get-TimeCoverage -Samples $systemSamples -TargetMs 2000 -MaxGapMs 6000 -AllowedDriftMs 1000 -Label '系统采样'
            $systemCoverage.Errors | ForEach-Object { [void]$inconclusiveReasons.Add([string]$_) }
        }
        else { [void]$inconclusiveReasons.Add('system_sample 缺失') }
        $summary = Get-ResultSummaryTsv -EvidenceRoot $EvidenceRoot -Harness $harness
        $summary.Errors | ForEach-Object { [void]$inconclusiveReasons.Add([string]$_) }
        if ($summary.Status -in @('MISSING', 'INCONCLUSIVE')) { [void]$inconclusiveReasons.Add("结果 TSV 状态为 $($summary.Status)") }
        if ($null -ne $terminal.Result -and $null -ne $before -and $null -ne $after) {
            $media = Get-MediaAndDiskMapEvidence -Before $before -After $after -Harness $harness `
                -RuntimeResult $terminal.Result -RuntimeSamples $runtimeSamples -EvidenceRoot $EvidenceRoot
            $media.Errors | ForEach-Object { [void]$inconclusiveReasons.Add([string]$_) }
            $media.Failures | ForEach-Object { [void]$failReasons.Add([string]$_) }
        }
        else { [void]$inconclusiveReasons.Add('媒体根或 runtime_result 缺失，无法闭合双盘证据') }
        $config = Get-ConfigurationEvidence -Harness $harness -RuntimeSamples $runtimeSamples
        $config.Errors | ForEach-Object { [void]$inconclusiveReasons.Add([string]$_) }
        $config.Failures | ForEach-Object { [void]$failReasons.Add([string]$_) }
        $pipeline = Get-PipelineEvidence -RuntimeSamples $runtimeSamples -TerminalSample $terminal.ResourceSample `
            -TerminalSampleIsActual:(-not $terminal.TerminalSampleMissing)
        $pipeline.Errors | ForEach-Object { [void]$inconclusiveReasons.Add([string]$_) }
        $pipeline.Failures | ForEach-Object { [void]$failReasons.Add([string]$_) }
        $pipeline.CapacityViolations | ForEach-Object { [void]$failReasons.Add("队列/资源容量越界：$($_)") }
        $pipeline.OwnershipMissing | ForEach-Object { [void]$inconclusiveReasons.Add("缺少 ownership 字段：$($_)") }
        if ($null -ne $media -and $media.Entries.Count -gt 0) {
            $diskRead = Get-DiskReadEvidence -RuntimeSamples $runtimeSamples -Entries $media.Entries
            $diskRead.Errors | ForEach-Object { [void]$inconclusiveReasons.Add([string]$_) }
            $diskRead.Failures | ForEach-Object { [void]$failReasons.Add([string]$_) }
        }
        else { [void]$inconclusiveReasons.Add('没有可用的双物理盘映射条目') }
        $system = Get-WorkerAndSystemSummary -RuntimeSamples $runtimeSamples -SystemSamples $systemSamples
    }
}
catch {
    [void]$inconclusiveReasons.Add("报告输入处理异常：$($_.Exception.Message)")
}

$lastRuntime = if ($runtimeSamples.Count -gt 0) { $runtimeSamples[-1] } else { $null }
$lastElapsed = Get-NumberOrNull (Get-OptionalProperty -Object $lastRuntime -Name 'elapsed_seconds')
$duration = Get-NumberOrNull (Get-OptionalProperty -Object $runtimeResult -Name 'duration_seconds')
if ($null -eq $duration) { $duration = $lastElapsed }
$machineIds = @($runtimeSamples | ForEach-Object { [string](Get-OptionalProperty -Object $_ -Name 'machine_id') } |
    Where-Object { -not [string]::IsNullOrWhiteSpace($_) } | Sort-Object -Unique)
$runtimeTaskIds = @($runtimeSamples | ForEach-Object { [string](Get-OptionalProperty -Object $_ -Name 'runtime_task_id') } |
    Where-Object { -not [string]::IsNullOrWhiteSpace($_) } | Sort-Object -Unique)
$execution = if ($null -ne $config) { $config.Actual } else { $null }
$workerSlots = Get-OptionalProperty -Object $execution -Name 'worker_slots'
$cpuBudget = Get-OptionalProperty -Object $execution -Name 'cpu_budget'
$hashTasks = Get-OptionalProperty -Object $execution -Name 'hash_tasks'
$globalPermits = Get-OptionalProperty -Object $execution -Name 'global_disk_permits'
$hddPermits = Get-OptionalProperty -Object $execution -Name 'hdd_per_disk_permits'
$ssdPermits = Get-OptionalProperty -Object $execution -Name 'ssd_per_disk_permits'
$unknownPermits = Get-OptionalProperty -Object $execution -Name 'unknown_per_disk_permits'
$completedMaximum = if ($runtimeSamples.Count -gt 0) {
    Get-NumberOrNull (($runtimeSamples | ForEach-Object { Get-OptionalProperty -Object $_ -Name 'overall_completed' } |
            ForEach-Object { Get-NumberOrNull $_ } | Where-Object { $null -ne $_ } | Measure-Object -Maximum).Maximum)
} else { $null }
$overallTotal = Get-OptionalProperty -Object $lastRuntime -Name 'overall_total'
$overallFailed = Get-OptionalProperty -Object $lastRuntime -Name 'overall_failed'
$overallSkipped = Get-OptionalProperty -Object $lastRuntime -Name 'overall_skipped'
$beforeCount = Get-OptionalProperty -Object $before -Name 'FileCount'
$afterCount = Get-OptionalProperty -Object $after -Name 'FileCount'
$beforeBytes = Get-NumberOrNull (Get-OptionalProperty -Object $before -Name 'TotalBytes')
$afterBytes = Get-NumberOrNull (Get-OptionalProperty -Object $after -Name 'TotalBytes')
$systemCount = $systemSamples.Count
$runtimeCount = $runtimeSamples.Count
$peakWorkers = if ($null -ne $system) { $system.PeakWorkers } else { 0 }
$averageWorkers = if ($null -ne $system) { $system.AverageWorkers } else { 0.0 }
$workerDisksText = if ($null -ne $system -and $system.WorkerDisks.Count -gt 0) { $system.WorkerDisks -join '、' } else { '—' }
$resultStatusText = if ($null -ne $summary) { $summary.Status } else { '—' }
$resultRowsText = if ($null -ne $summary) { $summary.RowCount } else { '—' }
$resultMissingText = if ($null -ne $summary) { $summary.MissingCount } else { '—' }
$resultInconclusiveText = if ($null -ne $summary) { $summary.InconclusiveCount } else { '—' }
$terminalStateText = if ($null -ne $terminal) { $terminal.TerminalState } else { '—' }
$terminalSampleText = if ($null -eq $terminal) {
    '—'
}
elseif ($terminal.TerminalSampleMissing) {
    '缺失（资源汇总使用最后一条运行样本，未执行终态归零判定）'
}
else {
    '已记录'
}
$mediaRootsText = if ($null -ne $media -and $media.BeforeRoots.Count -gt 0) { ($media.BeforeRoots | ForEach-Object { "- 媒体根：$_" }) -join "`n" } else { '- 媒体根证据不可用。' }
$mapText = if ($null -ne $media -and $media.Entries.Count -gt 0) {
    @($media.Entries | ForEach-Object {
            "- $($_.root) → PhysicalDisk$($_.disk_number)；盘符=$($_.drive_letter)；设备=$($_.friendly_name)；总线=$($_.bus_type)"
        }) -join "`n"
} else { '- 物理盘映射不可用。' }
$runtimeGapText = if ($null -ne $runtimeCoverage -and $null -ne $runtimeCoverage.MaxGapMs) { '{0:N3} 秒' -f ($runtimeCoverage.MaxGapMs / 1000) } else { '—' }
$systemGapText = if ($null -ne $systemCoverage -and $null -ne $systemCoverage.MaxGapMs) { '{0:N3} 秒' -f ($systemCoverage.MaxGapMs / 1000) } else { '—' }
$runtimeTargetText = if ($null -ne $runtimeCoverage) { "目标 1 秒，平均实际间隔=$('{0:N1}' -f $runtimeCoverage.AverageGapMs) ms" } else { '—' }
$systemTargetText = if ($null -ne $systemCoverage) { "目标 2 秒，平均实际间隔=$('{0:N1}' -f $systemCoverage.AverageGapMs) ms" } else { '—' }
$expectedWorkersValue = if ($null -ne $config) { $config.ExpectedWorkers } else { $null }
$expectedReadValue = if ($null -ne $config) { $config.ExpectedRead } else { $null }
$expectedHddValue = if ($null -ne $config) { $config.ExpectedHdd } else { $null }
$expectedSsdValue = if ($null -ne $config) { $config.ExpectedSsd } else { $null }
$expectedUnknownValue = if ($null -ne $config) { $config.ExpectedUnknown } else { $null }
$expectedWorkersText = Format-Optional $expectedWorkersValue
$expectedReadText = Format-Optional $expectedReadValue
$expectedHddText = Format-Optional $expectedHddValue
$expectedSsdText = Format-Optional $expectedSsdValue
$expectedUnknownText = Format-Optional $expectedUnknownValue
$verdict = if ($failReasons.Count -gt 0) { 'FAIL' } elseif ($inconclusiveReasons.Count -gt 0) { 'INCONCLUSIVE' } else { 'PASS' }
$metricLinesText = if ($null -ne $pipeline) { $pipeline.MetricLines -join "`n" } else { '| — | — | — | — | — | — |' }
$diskTableText = if ($null -ne $diskRead -and $diskRead.TableLines.Count -gt 0) { $diskRead.TableLines -join "`n" } else { '| — | — | — | — | — | — |' }
$phaseText = if ($null -ne $system) { $system.PhaseLines -join "`n" } else { '| — | 0 | — | — |' }
$processText = if ($null -ne $system) { $system.ProcessLines -join "`n" } else { '| — | 0 | — | — |' }
$systemDiskText = if ($null -ne $system) { $system.DiskLines -join "`n" } else { '| — | — | — | — |' }
$failureText = if ($null -ne $system) { $system.FailureLines -join "`n" } else { '- 无可用文件级失败记录。' }
$ownershipMissingText = if ($null -ne $pipeline -and $pipeline.OwnershipMissing.Count -gt 0) { $pipeline.OwnershipMissing -join '、' } else { '无' }
$ownershipCurrentSum = if ($null -ne $pipeline) {
    @($pipeline.OwnershipFields | Where-Object { $_ -ne 'hash_refill_token_available' } | ForEach-Object {
            if ($null -ne $pipeline.OwnershipCurrentMax[$_]) { [double]$pipeline.OwnershipCurrentMax[$_] }
        } | Measure-Object -Sum).Sum
} else { 0 }
$ownershipPeakSum = if ($null -ne $pipeline) {
    @($pipeline.OwnershipFields | Where-Object { $_ -ne 'hash_refill_token_available' } | ForEach-Object {
            if ($null -ne $pipeline.OwnershipPeakMax[$_]) { [double]$pipeline.OwnershipPeakMax[$_] }
        } | Measure-Object -Sum).Sum
} else { 0 }
$terminalOwnershipPass = $false
$terminalOwnershipText = 'FAIL/—'
if ($null -ne $pipeline) {
    if (-not $pipeline.TerminalSampleIsActual) {
        $terminalOwnershipText = 'INCONCLUSIVE（终态样本缺失）'
    }
    else {
        $terminalOwnershipPass = ($pipeline.TerminalNonZero.Count -eq 0 -and $null -ne $pipeline.TerminalToken -and $pipeline.TerminalToken -eq 0)
        $terminalOwnershipText = if ($terminalOwnershipPass) { 'PASS' } else { 'FAIL' }
    }
}
$report = @"
# Rust V2 单次真实媒体运行验收

结论：$verdict

## 自动化门禁

- 运行模式：任务进入终态后结束；最大持续时间只作为上限，不要求等待满上限。
- 任务终态：$terminalStateText
- 终态样本：$terminalSampleText
- 实际最后任务快照 elapsed：$(Format-Optional $lastElapsed ' 秒')；runtime_result duration：$(Format-Optional $duration ' 秒')
- 任务快照：$runtimeCount 条；$runtimeTargetText；最大相邻间隔：$runtimeGapText
- 系统采样：$systemCount 条；$systemTargetText；最大相邻间隔：$systemGapText
- 机器 ID：$(if ($machineIds.Count -gt 0) { $machineIds -join '、' } else { '—' })
- Runtime ID：$(if ($runtimeTaskIds.Count -gt 0) { $runtimeTaskIds -join '、' } else { '—' })

### 已证实硬失败
$(Join-ReasonLines -Reasons $failReasons)

### 证据缺失或不确定
$(Join-ReasonLines -Reasons $inconclusiveReasons)

## 本次运行配置

- 枚举器：$(if ($null -ne $config -and $config.Enumerator -and $config.Enumerator.ToLowerInvariant() -eq 'everything') { 'Everything' } else { '—' })
- 完成规则：任务进入终态即完成：$(if ($null -ne $config) { $config.CompleteWhenTaskTerminal } else { '—' })
- Worker 配置：$expectedWorkersText
- 读取线程配置：$expectedReadText
- HDD/盘：$expectedHddText；SSD/盘：$expectedSsdText；未知盘/盘：$expectedUnknownText
- 任务文件与协议未暴露的 P/C/F 计数：—（不参与本报告裁决）

## Node 实际执行配置

- Worker 槽：$(Format-Optional $workerSlots)；CPU 权重预算：$(Format-Optional $cpuBudget)；Hash 并发：$(Format-Optional $hashTasks)
- 全局磁盘许可：$(Format-Optional $globalPermits)；HDD/盘：$(Format-Optional $hddPermits)；SSD/盘：$(Format-Optional $ssdPermits)；未知盘/盘：$(Format-Optional $unknownPermits)
- 以上值直接来自 runtime execution_config；缺字段显示 `—`，不从进程数估算。

## 任务结果摘要

- 任务终态：$terminalStateText；完成=$(Format-Optional $completedMaximum) / 总数=$(Format-Optional $overallTotal)；失败=$(Format-Optional $overallFailed)；跳过=$(Format-Optional $overallSkipped)
- result-summary.tsv：$(if ($null -ne $summary) { $summary.Path } else { Join-Path $EvidenceRoot 'result-summary.tsv' })
- TSV 完整 SHA-256：$(if ($null -ne $summary) { Format-Optional $summary.FullSha256 } else { '—' })
- TSV 状态：$resultStatusText；R 行=$resultRowsText；MISSING=$resultMissingText；INCONCLUSIVE=$resultInconclusiveText
- TSV footer 与数据区 SHA：$(if ($null -ne $summary -and $summary.DataSha256) { '已校验' } else { '—' })

## 媒体根与物理盘

$mediaRootsText
$mapText
- 媒体清单前后逐项一致：$(if ($null -ne $media) { $media.MediaUnchanged } else { '—' })
- 物理盘映射 SHA：$(if ($null -ne $media) { $media.MapShaMatches } else { '—' })
- Worker 根覆盖：$(if ($null -ne $media) { $media.ObservedRoots -join '、' } else { '—' })

## Worker 并行

- 非空闲 Worker 峰值：$peakWorkers；平均：$(' {0:N2}' -f $averageWorkers)
- 观察到的物理盘：$workerDisksText
- system process_sample_skips：$(if ($null -ne $system) { $system.ProcessSampleSkips } else { '—' })

## 阶段与资源

### Worker 阶段

| 阶段 | 采样行数 | 最大 CPU 权重 | 最大解码线程 |
| --- | ---: | ---: | ---: |
$phaseText

### 流水线队列与资源

| 项目 | 类型 | 当前峰值 | 历史峰值 | 容量 | 等待 P95 | 服务/持有 P95 |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
$metricLinesText

- 队列/资源容量守恒：$(if ($null -ne $pipeline -and $pipeline.CapacityViolations.Count -eq 0) { 'PASS' } else { 'FAIL/—' })
- 运行阶段已完成文件最大值：$(Format-Optional $completedMaximum)

### Ownership 守恒

- 非 control ownership 当前峰值和：$(' {0:N0}' -f $ownershipCurrentSum)；历史峰值和：$(' {0:N0}' -f $ownershipPeakSum)
- 缺失字段：$ownershipMissingText
- 终态 ownership 归零：$terminalOwnershipText
- 协议未暴露的 P/C/F 与缓存命中明细：—（不反推）

## 逐物理盘读取许可

| 物理盘 | 容量 | waiting 峰值 | active 峰值 | grant 总数 | release 总数 |
| --- | ---: | ---: | ---: | ---: | ---: |
$diskTableText

- 双盘同采样点重叠读取：$(if ($null -ne $diskRead) { $diskRead.OverlapObserved } else { '—' })

## CPU、内存与磁盘 I/O

### 进程

| 进程 | 平均每 tick CPU 毫秒 | Working Set 峰值 | Private 峰值 |
| --- | ---: | ---: | ---: |
$processText

### 物理磁盘系统采样

| 物理盘实例 | 平均读吞吐 | 峰值读吞吐 | 队列峰值 |
| --- | ---: | ---: | ---: |
$systemDiskText

## 文件级失败观察

$failureText
- 疑似物理读取故障独立项：$(if ($null -ne $system) { $system.PhysicalFaults } else { '—' })
- Worker 崩溃独立项：$(if ($null -ne $system) { $system.WorkerCrashes } else { '—' })

## 媒体未修改证明

- 验收前：$(Format-Optional $beforeCount ' 个文件')，$(if ($null -ne $beforeBytes) { Format-Bytes $beforeBytes } else { '—' })
- 验收后：$(Format-Optional $afterCount ' 个文件')，$(if ($null -ne $afterBytes) { Format-Bytes $afterBytes } else { '—' })
- 路径、长度、LastWriteTimeUtc 逐项一致：$(if ($null -ne $media) { $media.MediaUnchanged } else { '—' })

## 解释边界

- 本报告只汇总本次 evidence；不生成多轮聚合，不使用历史任务列表。
- CPU、内存和系统磁盘 I/O 是本机采样观察值；未发生的故障不会被写成已通过。
- 任务文件内未由当前协议暴露的字段保持 `—`，不把整体进度倒推成任务文件统计。
- 原始证据目录：$EvidenceRoot
"@

$outputParent = Split-Path -Parent $OutputPath
if ($outputParent) { New-Item -ItemType Directory -Path $outputParent -Force | Out-Null }
[IO.File]::WriteAllText($OutputPath, $report, [Text.UTF8Encoding]::new($true))
Write-Output "RUST_V2_RUNTIME_ACCEPTANCE_REPORT_$verdict"
Write-Output "REPORT_PATH=$OutputPath"
